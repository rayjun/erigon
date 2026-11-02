package eip8304_test

import (
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/types"
)

// generationFinalizer applies the same due-table system calls as a real
// finalizer, but it cannot bind the in-flight table to a block hash that does
// not exist yet. Its records are only a generation fixture: import computes
// the canonical records again from the sealed blocks.
type generationFinalizer struct {
	store *parityTableStore
	limit uint64
}

var _ protocol.BlockFinalizer = (*generationFinalizer)(nil)

func newGenerationFinalizer(store *parityTableStore, limit uint64) *generationFinalizer {
	return &generationFinalizer{store: store, limit: limit}
}

func (f *generationFinalizer) FinalizePreSeal(block, _ uint64, parentBlockHash common.Hash, receipts types.Receipts, syscall rules.SystemCall) error {
	return f.apply(block, common.Hash{}, parentBlockHash, receipts, syscall)
}

func (f *generationFinalizer) Finalize(block, _ uint64, blockHash, parentBlockHash common.Hash, receipts types.Receipts, syscall rules.SystemCall) error {
	return f.apply(block, blockHash, parentBlockHash, receipts, syscall)
}

func (f *generationFinalizer) apply(block uint64, blockHash, parentBlockHash common.Hash, receipts types.Receipts, syscall rules.SystemCall) error {
	_, err := eip8304.ApplyDueTables(
		block,
		blockHash,
		parentBlockHash,
		receipts,
		eip8304.DevnetIndexContractAddress,
		func(uint64) bool { return true },
		eip8304.NewResolver(f.store, f.limit),
		syscall,
	)
	return err
}
