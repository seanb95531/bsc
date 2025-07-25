// Copyright 2019 The go-ethereum Authors
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

package core

import (
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

const prefetchThread = 3
const checkInterval = 10

// statePrefetcher is a basic Prefetcher, which blindly executes a block on top
// of an arbitrary state with the goal of prefetching potentially useful state
// data from disk before the main block processor start executing.
type statePrefetcher struct {
	config *params.ChainConfig // Chain configuration options
	chain  *HeaderChain        // Canonical block chain
}

// NewStatePrefetcher initialises a new statePrefetcher.
func NewStatePrefetcher(config *params.ChainConfig, chain *HeaderChain) *statePrefetcher {
	return &statePrefetcher{
		config: config,
		chain:  chain,
	}
}

// Prefetch processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb, but any changes are discarded. The
// only goal is to warm the state caches.
func (p *statePrefetcher) Prefetch(transactions types.Transactions, header *types.Header, gasLimit uint64, statedb *state.StateDB, cfg *vm.Config, interruptCh <-chan struct{}) {
	var (
		signer = types.MakeSigner(p.config, header.Number, header.Time)
	)
	txChan := make(chan int, prefetchThread)

	for i := 0; i < prefetchThread; i++ {
		go func() {
			newStatedb := statedb.CopyDoPrefetch()
			gaspool := new(GasPool).AddGas(gasLimit)
			evm := vm.NewEVM(NewEVMBlockContext(header, p.chain, nil), newStatedb, p.config, *cfg)
			// Iterate over and process the individual transactions
			for {
				select {
				case txIndex := <-txChan:
					tx := transactions[txIndex]
					// Convert the transaction into an executable message and pre-cache its sender
					msg, err := TransactionToMessage(tx, signer, header.BaseFee)
					if err != nil {
						return // Also invalid block, bail out
					}
					// Disable the nonce check
					msg.SkipNonceChecks = true

					newStatedb.SetTxContext(tx.Hash(), txIndex)
					// We attempt to apply a transaction. The goal is not to execute
					// the transaction successfully, rather to warm up touched data slots.
					ApplyMessage(evm, msg, gaspool)

				case <-interruptCh:
					// If block precaching was interrupted, abort
					return
				}
			}
		}()
	}

	// it should be in a separate goroutine, to avoid blocking the critical path.
	for i := 0; i < len(transactions); i++ {
		select {
		case txChan <- i:
		case <-interruptCh:
			return
		}
	}
}

// PrefetchMining processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb, but any changes are discarded. The
// only goal is to warm the state caches. Only used for mining stage.
func (p *statePrefetcher) PrefetchMining(txs TransactionsByPriceAndNonce, header *types.Header, gasLimit uint64, statedb *state.StateDB, cfg vm.Config, interruptCh <-chan struct{}, txCurr **types.Transaction) {
	var signer = types.MakeSigner(p.config, header.Number, header.Time)

	txCh := make(chan *types.Transaction, 2*prefetchThread)
	for i := 0; i < prefetchThread; i++ {
		go func(startCh <-chan *types.Transaction, stopCh <-chan struct{}) {
			newStatedb := statedb.CopyDoPrefetch()
			evm := vm.NewEVM(NewEVMBlockContext(header, p.chain, nil), newStatedb, p.config, cfg)
			idx := 0
			// Iterate over and process the individual transactions
			for {
				select {
				case tx := <-startCh:
					// Convert the transaction into an executable message and pre-cache its sender
					msg, err := TransactionToMessage(tx, signer, header.BaseFee)
					if err != nil {
						return // Also invalid block, bail out
					}
					// Disable the nonce check
					msg.SkipNonceChecks = true

					idx++
					newStatedb.SetTxContext(tx.Hash(), idx)
					ApplyMessage(evm, msg, new(GasPool).AddGas(gasLimit))

				case <-stopCh:
					return
				}
			}
		}(txCh, interruptCh)
	}
	go func(txset TransactionsByPriceAndNonce) {
		count := 0
		for {
			select {
			case <-interruptCh:
				return
			default:
				if count++; count%checkInterval == 0 {
					txset.Forward(*txCurr)
				}
				tx := txset.PeekWithUnwrap()
				if tx == nil {
					return
				}

				select {
				case <-interruptCh:
					return
				case txCh <- tx:
				}

				txset.Shift()
			}
		}
	}(txs)
}
