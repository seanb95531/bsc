// Copyright 2020 The go-ethereum Authors
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

package eth

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/bsc"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

var (
	// errPeerSetClosed is returned if a peer is attempted to be added or removed
	// from the peer set after it has been terminated.
	errPeerSetClosed = errors.New("peerset closed")

	// errPeerAlreadyRegistered is returned if a peer is attempted to be added
	// to the peer set, but one with the same id already exists.
	errPeerAlreadyRegistered = errors.New("peer already registered")

	// errPeerWaitTimeout is returned if a peer waits extension for too long
	errPeerWaitTimeout = errors.New("peer wait timeout")

	// errPeerNotRegistered is returned if a peer is attempted to be removed from
	// a peer set, but no peer with the given id exists.
	errPeerNotRegistered = errors.New("peer not registered")

	// errSnapWithoutEth is returned if a peer attempts to connect only on the
	// snap protocol without advertising the eth main protocol.
	errSnapWithoutEth = errors.New("peer connected on snap without compatible eth support")

	// errBscWithoutEth is returned if a peer attempts to connect only on the
	// bsc protocol without advertising the eth main protocol.
	errBscWithoutEth = errors.New("peer connected on bsc without compatible eth support")
)

const (
	// extensionWaitTimeout is the maximum allowed time for the extension wait to
	// complete before dropping the connection as malicious.
	extensionWaitTimeout = 10 * time.Second
	tryWaitTimeout       = 100 * time.Millisecond
)

var (
	evnWhiteListPeerGuage        = metrics.NewRegisteredGauge("evn/peer/whiteList", nil)
	evnOnchainValidatorPeerGuage = metrics.NewRegisteredGauge("evn/peer/onchainValidator", nil)
)

// peerSet represents the collection of active peers currently participating in
// the `eth` protocol, with or without the `snap` extension.
type peerSet struct {
	peers     map[string]*ethPeer // Peers connected on the `eth` protocol
	snapPeers int                 // Number of `snap` compatible peers for connection prioritization

	validatorNodeIDsMap map[common.Address][]enode.ID

	snapWait map[string]chan *snap.Peer // Peers connected on `eth` waiting for their snap extension
	snapPend map[string]*snap.Peer      // Peers connected on the `snap` protocol, but not yet on `eth`

	bscWait map[string]chan *bsc.Peer // Peers connected on `eth` waiting for their bsc extension
	bscPend map[string]*bsc.Peer      // Peers connected on the `bsc` protocol, but not yet on `eth`

	lock   sync.RWMutex
	closed bool
	quitCh chan struct{} // Quit channel to signal termination
}

// newPeerSet creates a new peer set to track the active participants.
func newPeerSet() *peerSet {
	return &peerSet{
		peers:    make(map[string]*ethPeer),
		snapWait: make(map[string]chan *snap.Peer),
		snapPend: make(map[string]*snap.Peer),
		bscWait:  make(map[string]chan *bsc.Peer),
		bscPend:  make(map[string]*bsc.Peer),
		quitCh:   make(chan struct{}),
	}
}

// registerSnapExtension unblocks an already connected `eth` peer waiting for its
// `snap` extension, or if no such peer exists, tracks the extension for the time
// being until the `eth` main protocol starts looking for it.
func (ps *peerSet) registerSnapExtension(peer *snap.Peer) error {
	// Reject the peer if it advertises `snap` without `eth` as `snap` is only a
	// satellite protocol meaningful with the chain selection of `eth`
	if !peer.RunningCap(eth.ProtocolName, eth.ProtocolVersions) {
		return fmt.Errorf("%w: have %v", errSnapWithoutEth, peer.Caps())
	}
	// Ensure nobody can double connect
	ps.lock.Lock()
	defer ps.lock.Unlock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}
	if _, ok := ps.snapPend[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// Inject the peer into an `eth` counterpart is available, otherwise save for later
	if wait, ok := ps.snapWait[id]; ok {
		delete(ps.snapWait, id)
		wait <- peer
		return nil
	}
	ps.snapPend[id] = peer
	return nil
}

// registerBscExtension unblocks an already connected `eth` peer waiting for its
// `bsc` extension, or if no such peer exists, tracks the extension for the time
// being until the `eth` main protocol starts looking for it.
func (ps *peerSet) registerBscExtension(peer *bsc.Peer) error {
	// Reject the peer if it advertises `bsc` without `eth` as `bsc` is only a
	// satellite protocol meaningful with the chain selection of `eth`
	if !peer.RunningCap(eth.ProtocolName, eth.ProtocolVersions) {
		return errBscWithoutEth
	}
	// Ensure nobody can double connect
	ps.lock.Lock()
	defer ps.lock.Unlock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}
	if _, ok := ps.bscPend[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// Inject the peer into an `eth` counterpart is available, otherwise save for later
	if wait, ok := ps.bscWait[id]; ok {
		delete(ps.bscWait, id)
		wait <- peer
		return nil
	}
	ps.bscPend[id] = peer
	return nil
}

// waitSnapExtension blocks until all satellite protocols are connected and tracked
// by the peerset.
func (ps *peerSet) waitSnapExtension(peer *eth.Peer) (*snap.Peer, error) {
	// If the peer does not support a compatible `snap`, don't wait
	if !peer.RunningCap(snap.ProtocolName, snap.ProtocolVersions) {
		return nil, nil
	}
	// Ensure nobody can double connect
	ps.lock.Lock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}
	if _, ok := ps.snapWait[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// If `snap` already connected, retrieve the peer from the pending set
	if snap, ok := ps.snapPend[id]; ok {
		delete(ps.snapPend, id)

		ps.lock.Unlock()
		return snap, nil
	}
	// Otherwise wait for `snap` to connect concurrently
	wait := make(chan *snap.Peer)
	ps.snapWait[id] = wait
	ps.lock.Unlock()

	select {
	case peer := <-wait:
		return peer, nil

	case <-time.After(extensionWaitTimeout):
		ps.lock.Lock()
		delete(ps.snapWait, id)
		ps.lock.Unlock()
		return nil, errPeerWaitTimeout

	case <-ps.quitCh:
		ps.lock.Lock()
		delete(ps.snapWait, id)
		ps.lock.Unlock()
		return nil, errPeerSetClosed
	}
}

// waitBscExtension blocks until all satellite protocols are connected and tracked
// by the peerset.
func (ps *peerSet) waitBscExtension(peer *eth.Peer) (*bsc.Peer, error) {
	// If the peer does not support a compatible `bsc`, don't wait
	if !peer.RunningCap(bsc.ProtocolName, bsc.ProtocolVersions) {
		return nil, nil
	}
	// Ensure nobody can double connect
	ps.lock.Lock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}
	if _, ok := ps.bscWait[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// If `bsc` already connected, retrieve the peer from the pending set
	if bsc, ok := ps.bscPend[id]; ok {
		delete(ps.bscPend, id)

		ps.lock.Unlock()
		return bsc, nil
	}
	// Otherwise wait for `bsc` to connect concurrently
	wait := make(chan *bsc.Peer)
	ps.bscWait[id] = wait
	ps.lock.Unlock()

	select {
	case peer := <-wait:
		return peer, nil

	case <-time.After(extensionWaitTimeout):
		// could be deadlock, so we use TryLock to avoid it.
		if ps.lock.TryLock() {
			delete(ps.bscWait, id)
			ps.lock.Unlock()
			return nil, errPeerWaitTimeout
		}
		// if TryLock failed, we wait for a while and try again.
		for {
			select {
			case <-wait:
				// discard the peer, even though the peer arrived.
				return nil, errPeerWaitTimeout
			case <-time.After(tryWaitTimeout):
				if ps.lock.TryLock() {
					delete(ps.bscWait, id)
					ps.lock.Unlock()
					return nil, errPeerWaitTimeout
				}
			}
		}

	case <-ps.quitCh:
		ps.lock.Lock()
		delete(ps.bscWait, id)
		ps.lock.Unlock()
		return nil, errPeerSetClosed
	}
}

// registerPeer injects a new `eth` peer into the working set, or returns an error
// if the peer is already known.
func (ps *peerSet) registerPeer(peer *eth.Peer, ext *snap.Peer, bscExt *bsc.Peer) error {
	// Start tracking the new peer
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if ps.closed {
		return errPeerSetClosed
	}
	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered
	}
	eth := &ethPeer{
		Peer: peer,
	}
	if ext != nil {
		eth.snapExt = &snapPeer{ext}
		ps.snapPeers++
	}
	if bscExt != nil {
		eth.bscExt = &bscPeer{bscExt}
	}
	ps.peers[id] = eth
	return nil
}

// unregisterPeer removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *peerSet) unregisterPeer(id string) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	peer, ok := ps.peers[id]
	if !ok {
		return errPeerNotRegistered
	}
	delete(ps.peers, id)
	if peer.snapExt != nil {
		ps.snapPeers--
	}
	return nil
}

// peer retrieves the registered peer with the given id.
func (ps *peerSet) peer(id string) *ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

// enableEVNFeatures enables the given features for the given peers.
func (ps *peerSet) enableEVNFeatures(validatorNodeIDsMap map[common.Address][]enode.ID, evnWhitelistMap map[enode.ID]struct{}) {
	// clone current all peers, and update the validatorNodeIDsMap
	ps.lock.Lock()
	peers := make([]*ethPeer, 0, len(ps.peers))
	for _, peer := range ps.peers {
		peers = append(peers, peer)
	}
	ps.validatorNodeIDsMap = validatorNodeIDsMap
	ps.lock.Unlock()

	// convert to nodeID filter map, avoid too slow operation for slices.Contains
	valNodeIDMap := make(map[enode.ID]struct{})
	for _, nodeIDs := range validatorNodeIDsMap {
		for _, nodeID := range nodeIDs {
			valNodeIDMap[nodeID] = struct{}{}
		}
	}

	var (
		whiteListPeerCnt        int64 = 0
		onchainValidatorPeerCnt int64 = 0
	)
	for _, peer := range peers {
		nodeID := peer.NodeID()
		_, isValidatorPeer := valNodeIDMap[nodeID]
		_, isWhitelistPeer := evnWhitelistMap[nodeID]

		if isValidatorPeer || isWhitelistPeer {
			log.Debug("enable EVNPeerFlag & NoTxBroadcastFlag for", "peer", nodeID)
			peer.EVNPeerFlag.Store(true)
		} else {
			peer.EVNPeerFlag.Store(false)
		}

		if isValidatorPeer {
			onchainValidatorPeerCnt++
		}
		if isWhitelistPeer {
			whiteListPeerCnt++
		}
	}
	evnWhiteListPeerGuage.Update(whiteListPeerCnt)
	evnOnchainValidatorPeerGuage.Update(onchainValidatorPeerCnt)
	log.Info("enable EVN features", "total", len(peers), "whiteListPeerCnt", whiteListPeerCnt, "onchainValidatorPeerCnt", onchainValidatorPeerCnt)
}

// isProxyedValidator checks if the received block from the proxyed validator.
func (ps *peerSet) isProxyedValidator(validator common.Address, proxyedAddressMap map[common.Address]struct{}) bool {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	if len(proxyedAddressMap) == 0 {
		return false
	}
	log.Debug("check whether received block from proxyed peer", "validator", validator, "proxyedAddressMap", proxyedAddressMap)

	// check whether the validator is proxyed validator
	if _, ok := proxyedAddressMap[validator]; !ok {
		return false
	}
	return true
}

// headPeers retrieves a specified number list of peers.
func (ps *peerSet) headPeers(num uint) []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	if num > uint(len(ps.peers)) {
		num = uint(len(ps.peers))
	}

	list := make([]*ethPeer, 0, num)
	for _, p := range ps.peers {
		if len(list) > int(num) {
			break
		}
		list = append(list, p)
	}
	return list
}

// peersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes, so it might be propagated to them.
func (ps *peerSet) peersWithoutBlock(hash common.Hash) []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*ethPeer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.KnownBlock(hash) {
			list = append(list, p)
		}
	}
	log.Debug("get peers without block", "hash", hash, "total", len(ps.peers), "unknown", len(list))
	return list
}

// peersWithoutTransaction retrieves a list of peers that do not have a given
// transaction in their set of known hashes.
func (ps *peerSet) peersWithoutTransaction(hash common.Hash) []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*ethPeer, 0, len(ps.peers))
	for _, p := range ps.peers {
		// it can be optimized in the future, to make it more clear that only when both peers of a connection are EVN nodes, will enable no tx broadcast.
		if p.EVNPeerFlag.Load() {
			log.Debug("skip EVN peer with no tx forwarding feature", "peer", p.ID())
			continue
		}
		if !p.KnownTransaction(hash) {
			list = append(list, p)
		}
	}
	return list
}

// peersWithoutVote retrieves a list of peers that do not have a given
// vote in their set of known hashes.
func (ps *peerSet) peersWithoutVote(hash common.Hash) []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*ethPeer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if p.bscExt != nil && !p.bscExt.KnownVote(hash) {
			list = append(list, p)
		}
	}
	return list
}

// len returns if the current number of `eth` peers in the set. Since the `snap`
// peers are tied to the existence of an `eth` connection, that will always be a
// subset of `eth`.
func (ps *peerSet) len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// snapLen returns if the current number of `snap` peers in the set.
func (ps *peerSet) snapLen() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.snapPeers
}

// peerWithHighestTD retrieves the known peer with the currently highest total
// difficulty, but below the given PoS switchover threshold.
func (ps *peerSet) peerWithHighestTD() *eth.Peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var (
		bestPeer *eth.Peer
		bestTd   *big.Int
	)
	for _, p := range ps.peers {
		if p.Lagging() {
			continue
		}
		if _, td := p.Head(); bestPeer == nil || td.Cmp(bestTd) > 0 {
			bestPeer, bestTd = p.Peer, td
		}
	}
	return bestPeer
}

// close disconnects all peers.
func (ps *peerSet) close() {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	for _, p := range ps.peers {
		p.Disconnect(p2p.DiscQuitting)
	}
	if !ps.closed {
		close(ps.quitCh)
	}
	ps.closed = true
}
