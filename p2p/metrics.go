// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Contains the meters and timers used by the networking layer.

package p2p

import (
	"errors"
	"net"

	"github.com/ethereum/go-ethereum/metrics"
)

const (
	// HandleHistName is the prefix of the per-packet serving time histograms.
	HandleHistName = "p2p/handle"

	// ingressMeterName is the prefix of the per-packet inbound metrics.
	ingressMeterName = "p2p/ingress"

	// egressMeterName is the prefix of the per-packet outbound metrics.
	egressMeterName = "p2p/egress"
)

var (
	activePeerGauge         = metrics.NewRegisteredGauge("p2p/peers", nil)
	activeInboundPeerGauge  = metrics.NewRegisteredGauge("p2p/peers/inbound", nil)
	activeOutboundPeerGauge = metrics.NewRegisteredGauge("p2p/peers/outbound", nil)

	ingressTrafficMeter = metrics.NewRegisteredMeter("p2p/ingress", nil)
	egressTrafficMeter  = metrics.NewRegisteredMeter("p2p/egress", nil)

	// general ingress/egress connection meters
	serveMeter          = metrics.NewRegisteredMeter("p2p/serves", nil)
	serveSuccessMeter   = metrics.NewRegisteredMeter("p2p/serves/success", nil)
	dialMeter           = metrics.NewRegisteredMeter("p2p/dials", nil)
	dialSuccessMeter    = metrics.NewRegisteredMeter("p2p/dials/success", nil)
	dialConnectionError = metrics.NewRegisteredMeter("p2p/dials/error/connection", nil)

	// dial error meters
	dialTooManyPeers        = metrics.NewRegisteredMeter("p2p/dials/error/saturated", nil)
	dialAlreadyConnected    = metrics.NewRegisteredMeter("p2p/dials/error/known", nil)
	dialSelf                = metrics.NewRegisteredMeter("p2p/dials/error/self", nil)
	dialUselessPeer         = metrics.NewRegisteredMeter("p2p/dials/error/useless", nil)
	dialUnexpectedIdentity  = metrics.NewRegisteredMeter("p2p/dials/error/id/unexpected", nil)
	dialEncHandshakeError   = metrics.NewRegisteredMeter("p2p/dials/error/rlpx/enc", nil)
	dialProtoHandshakeError = metrics.NewRegisteredMeter("p2p/dials/error/rlpx/proto", nil)

	// serve error meters
	serveTooManyPeers        = metrics.NewRegisteredMeter("p2p/serves/error/saturated", nil)
	serveAlreadyConnected    = metrics.NewRegisteredMeter("p2p/serves/error/known", nil)
	serveSelf                = metrics.NewRegisteredMeter("p2p/serves/error/self", nil)
	serveUselessPeer         = metrics.NewRegisteredMeter("p2p/serves/error/useless", nil)
	serveUnexpectedIdentity  = metrics.NewRegisteredMeter("p2p/serves/error/id/unexpected", nil)
	serveEncHandshakeError   = metrics.NewRegisteredMeter("p2p/serves/error/rlpx/enc", nil)
	serveProtoHandshakeError = metrics.NewRegisteredMeter("p2p/serves/error/rlpx/proto", nil)

	normalPeerLatencyStat = metrics.NewRegisteredTimer("p2p/peers/normal/latency", nil)
	evnPeerLatencyStat    = metrics.NewRegisteredTimer("p2p/peers/evn/latency", nil)
)

// markDialError matches errors that occur while setting up a dial connection
// to the corresponding meter.
func markDialError(err error) {
	if !metrics.Enabled() {
		return
	}
	if err2 := errors.Unwrap(err); err2 != nil {
		err = err2
	}
	switch err {
	case DiscTooManyPeers:
		dialTooManyPeers.Mark(1)
	case DiscAlreadyConnected:
		dialAlreadyConnected.Mark(1)
	case DiscSelf:
		dialSelf.Mark(1)
	case DiscUselessPeer:
		dialUselessPeer.Mark(1)
	case DiscUnexpectedIdentity:
		dialUnexpectedIdentity.Mark(1)
	case errEncHandshakeError:
		dialEncHandshakeError.Mark(1)
	case errProtoHandshakeError:
		dialProtoHandshakeError.Mark(1)
	}
}

// markServeError matches errors that occur while setting up a serve connection
// to the corresponding meter.
func markServeError(err error) {
	if !metrics.Enabled() {
		return
	}
	if err2 := errors.Unwrap(err); err2 != nil {
		err = err2
	}
	switch err {
	case DiscTooManyPeers:
		serveTooManyPeers.Mark(1)
	case DiscAlreadyConnected:
		serveAlreadyConnected.Mark(1)
	case DiscSelf:
		serveSelf.Mark(1)
	case DiscUselessPeer:
		serveUselessPeer.Mark(1)
	case DiscUnexpectedIdentity:
		serveUnexpectedIdentity.Mark(1)
	case errEncHandshakeError:
		serveEncHandshakeError.Mark(1)
	case errProtoHandshakeError:
		serveProtoHandshakeError.Mark(1)
	}
}

// meteredConn is a wrapper around a net.Conn that meters both the
// inbound and outbound network traffic.
type meteredConn struct {
	net.Conn
}

// newMeteredConn creates a new metered connection, bumps the ingress or egress
// connection meter and also increases the metered peer count. If the metrics
// system is disabled, function returns the original connection.
func newMeteredConn(conn net.Conn) net.Conn {
	if !metrics.Enabled() {
		return conn
	}
	return &meteredConn{Conn: conn}
}

// Read delegates a network read to the underlying connection, bumping the common
// and the peer ingress traffic meters along the way.
func (c *meteredConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	ingressTrafficMeter.Mark(int64(n))
	return n, err
}

// Write delegates a network write to the underlying connection, bumping the common
// and the peer egress traffic meters along the way.
func (c *meteredConn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	egressTrafficMeter.Mark(int64(n))
	return n, err
}
