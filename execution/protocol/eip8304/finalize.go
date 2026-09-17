package eip8304

import (
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/misc"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

// FinalizeL0 builds the sorted L0 entries for a block from its receipts,
// computes the SSZ list root under the given limit, and applies the index
// contract update for the block's level-0 table (firstBlock = block,
// tableSize = 1). The index address and the SSZ list limit are explicit
// arguments.
func FinalizeL0(
	block uint64,
	parentBlockHash common.Hash,
	receipts types.Receipts,
	indexAddr accounts.Address,
	listLimit uint64,
	syscall rules.SystemCall,
) error {
	entries, err := BuildBlockEntriesFromReceipts(block, parentBlockHash, receipts)
	if err != nil {
		return err
	}
	entries.Sort()

	root, err := RootOfEntries(entries, listLimit)
	if err != nil {
		return err
	}

	table := L0(block)
	return misc.ApplyIndexEip8304(indexAddr, table.FirstBlock, table.TableSize, root, syscall)
}

// ApplyDueTables applies every protocol table whose write moment is `block`, in
// the order DueTables returns, i.e. ascending table size, so the level-0 table
// of the processed block is written first. The level-0 table is built from the
// block's receipts; higher-level tables come from the resolver.
//
// `activeAt` reports whether the EIP was already active at a table's first
// block and is required; so is the resolver. `activeAt` receives a block
// number: when activation is configured as a timestamp
// (chain.Config.Eip8304Time), the caller owns the block-to-time mapping and
// applies the chain's own activation rule here.
//
// A table is recorded as canonical only after its system call succeeded. On
// failure ApplyDueTables stops: the returned refs are the tables applied so
// far, and neither the failed table nor any later one is written or recorded.
func ApplyDueTables(
	block uint64,
	blockHash common.Hash,
	parentBlockHash common.Hash,
	receipts types.Receipts,
	indexAddr accounts.Address,
	activeAt func(firstBlock uint64) bool,
	resolver *Resolver,
	syscall rules.SystemCall,
) ([]TableRef, error) {
	if activeAt == nil {
		return nil, ErrNilActivationPredicate
	}
	if resolver == nil {
		return nil, ErrNilTableResolver
	}
	if resolver.store == nil {
		return nil, ErrNilTableStore
	}

	applied := make([]TableRef, 0, 2)
	for _, ref := range DueTables(block) {
		if !activeAt(ref.FirstBlock) {
			continue
		}
		result, err := dueTableResult(ref, block, blockHash, parentBlockHash, receipts, resolver)
		if err != nil {
			return applied, err
		}
		if err := misc.ApplyIndexEip8304(indexAddr, ref.FirstBlock, ref.TableSize, result.Root, syscall); err != nil {
			return applied, err
		}
		resolver.store.PutTable(result)
		applied = append(applied, ref)
	}
	return applied, nil
}

// dueTableResult produces the root to write for one due table. The level-0 table
// of the block being processed is built from the receipts passed to
// ApplyDueTables; every other table comes from the resolver. A higher-level
// table due at `block` never covers `block` itself (its range ends at least
// table_size/4 blocks earlier), so the resolver never needs the in-flight
// block's entries.
func dueTableResult(
	ref TableRef,
	block uint64,
	blockHash common.Hash,
	parentBlockHash common.Hash,
	receipts types.Receipts,
	resolver *Resolver,
) (TableResult, error) {
	if ref.TableSize != 1 {
		return resolver.Table(ref)
	}

	entries, err := BuildBlockEntriesFromReceipts(block, parentBlockHash, receipts)
	if err != nil {
		return TableResult{}, err
	}
	entries.Sort()
	root, err := RootOfEntries(entries, resolver.listLimit)
	if err != nil {
		return TableResult{}, err
	}
	return TableResult{
		Ref:         ref,
		EntryCount:  uint64(len(entries)),
		Entries:     entries,
		Root:        root,
		BlockHashes: []common.Hash{blockHash},
	}, nil
}
