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

package core

import (
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// Validator is an interface which defines the standard for block validation. It
// is only responsible for validating block contents, as the header validation is
// done by the specific consensus engines.
type Validator interface {
	// ValidateBody validates the given block's content.
	ValidateBody(block *types.Block) error

	// ValidateState validates the given statedb and optionally the process result.
	ValidateState(block *types.Block, state *state.StateDB, res *ProcessResult, stateless bool) error
}

type TransactionsByPriceAndNonce interface {
	PeekWithUnwrap() *types.Transaction
	Shift()
	Forward(tx *types.Transaction)
}

// Prefetcher is an interface for pre-caching transaction signatures and state.
type Prefetcher interface {
	// Prefetch processes the state changes according to the Ethereum rules by running
	// the transaction messages using the statedb, but any changes are discarded. The
	// only goal is to warm the state caches.
	Prefetch(transactions types.Transactions, header *types.Header, gasLimit uint64, statedb *state.StateDB, cfg *vm.Config, interruptCh <-chan struct{})
	// PrefetchMining used for pre-caching transaction signatures and state trie nodes. Only used for mining stage.
	PrefetchMining(txs TransactionsByPriceAndNonce, header *types.Header, gasLimit uint64, statedb *state.StateDB, cfg vm.Config, interruptCh <-chan struct{}, txCurr **types.Transaction)
}

// Processor is an interface for processing blocks using a given initial state.
type Processor interface {
	// Process processes the state changes according to the Ethereum rules by running
	// the transaction messages using the statedb and applying any rewards to both
	// the processor (coinbase) and any included uncles.
	Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (*ProcessResult, error)
}

// ProcessResult contains the values computed by Process.
type ProcessResult struct {
	Receipts types.Receipts
	Requests [][]byte
	Logs     []*types.Log
	GasUsed  uint64
}
