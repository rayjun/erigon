package eip8304

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/mdbx"
	"github.com/erigontech/erigon/db/rawdb"
	"github.com/erigontech/erigon/execution/stagedsync/stages"
	"github.com/erigontech/erigon/execution/types"
)

// newFileDatadir opens a real, file-backed chaindata database. The recovery
// paths are the ones a restart actually exercises, so they are tested on the
// same MDBX a node opens rather than on an in-memory double.
func newFileDatadir(t *testing.T) kv.RwDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chaindata")
	db, err := mdbx.New(dbcfg.ChainDB, log.New()).Path(path).Open(context.Background())
	require.NoError(t, err)
	t.Cleanup(db.Close)
	return db
}

// chainReceipts adapts the in-memory test chain to the ReceiptSource the node
// adapter takes. Which sync input can serve receipts without
// --prune.include-receipts is still open (T3.3), so it stays injectable.
type chainReceipts struct{ chain *chainStore }

func (c chainReceipts) Receipts(block uint64) (types.Receipts, error) {
	receipts, ok := c.chain.receipts[block]
	if !ok {
		return nil, fmt.Errorf("%w: receipts for block %d", ErrMissingBlockData, block)
	}
	return receipts, nil
}

// seedCanonicalChain writes the canonical hashes (and execution progress) the
// node adapter reads, using the same hashes the in-memory chain holds.
func seedCanonicalChain(t *testing.T, db kv.RwDB, chain *chainStore, executed uint64) {
	t.Helper()
	require.NoError(t, db.Update(context.Background(), func(tx kv.RwTx) error {
		for block := range chain.hashes {
			if err := rawdb.WriteCanonicalHash(tx, chain.hashes[block], block); err != nil {
				return err
			}
		}
		return stages.SaveStageProgress(tx, stages.Execution, executed)
	}))
}

func TestDBDataReadsHashesAndProgressFromTheDatabase(t *testing.T) {
	require := require.New(t)
	db := newFileDatadir(t)
	chain := newTestChain(t, 8)
	seedCanonicalChain(t, db, chain, 7)

	require.NoError(db.View(context.Background(), func(tx kv.Tx) error {
		hashes, err := NewDBHashes(tx)
		require.NoError(err)

		got, err := hashes.CanonicalHash(5)
		require.NoError(err)
		require.Equal(chain.hashes[5], got)

		_, err = hashes.CanonicalHash(8)
		require.ErrorIs(err, ErrMissingBlockData, "a height without a canonical hash must fail closed")

		progress, err := NewStageProgress(tx)
		require.NoError(err)
		executed, err := progress.ExecutedHeight()
		require.NoError(err)
		require.Equal(uint64(7), executed)

		data, err := NewDBData(tx, chainReceipts{chain})
		require.NoError(err)
		for block, want := range chain.blocks {
			entries, err := data.BlockEntries(block)
			require.NoError(err)
			require.Equal(Entries(want), entries, "rebuild of block %d must match the canonical entries", block)
		}
		return nil
	}))
}

func TestDBDataFailsClosedWithoutReceipts(t *testing.T) {
	require := require.New(t)
	db := newFileDatadir(t)
	chain := newTestChain(t, 4)
	seedCanonicalChain(t, db, chain, 3)

	require.NoError(db.View(context.Background(), func(tx kv.Tx) error {
		data, err := NewDBData(tx, chainReceipts{chain: newTestChain(t, 4)})
		require.NoError(err)

		// Block 40 has no receipts in the source, though its hash would be
		// unknown too; block 3 is canonical but its receipts are swapped out.
		empty, err := NewDBData(tx, chainReceipts{chain: &chainStore{receipts: map[uint64]types.Receipts{}}})
		require.NoError(err)
		_, err = empty.BlockEntries(3)
		require.ErrorIs(err, ErrMissingBlockData)

		_, err = data.BlockEntries(40)
		require.ErrorIs(err, ErrMissingBlockData)
		return nil
	}))
}

func TestNewDBDataRejectsMissingInputs(t *testing.T) {
	require := require.New(t)
	db := newFileDatadir(t)

	require.NoError(db.View(context.Background(), func(tx kv.Tx) error {
		_, err := NewDBHashes(nil)
		require.ErrorIs(err, ErrNilCanonicalDB)
		_, err = NewStageProgress(nil)
		require.ErrorIs(err, ErrNilCanonicalDB)
		_, err = NewDBData(tx, nil)
		require.ErrorIs(err, ErrNilReceiptSource)
		_, err = NewDBData(nil, chainReceipts{chain: newTestChain(t, 1)})
		require.ErrorIs(err, ErrNilCanonicalDB)
		return nil
	}))
}

// TestHotTableStoreRecoversOnARealDatadirRestart walks T3.2's acceptance case
// on a file-backed datadir: a table is written, the database is closed and
// reopened (a process restart), the record survives the restart check, a
// bounded multiblock reorg makes it stale, and an unwind to a height inside its
// range drops it — with the generation staying bumped across the next restart.
func TestHotTableStoreRecoversOnARealDatadirRestart(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "chaindata")
	open := func() kv.RwDB {
		db, err := mdbx.New(dbcfg.ChainDB, log.New()).Path(path).Open(context.Background())
		require.NoError(err)
		return db
	}

	db := open()
	chain := newTestChain(t, 16)
	seedCanonicalChain(t, db, chain, 15)

	ref := TableRef{FirstBlock: 0, TableSize: 4}
	result := hotStoreSeed(t, chain, ref)

	require.NoError(db.Update(context.Background(), func(tx kv.RwTx) error {
		data, err := NewDBData(tx, chainReceipts{chain})
		if err != nil {
			return err
		}
		progress, err := NewStageProgress(tx)
		if err != nil {
			return err
		}
		store, err := NewHotTableStore(tx, data, progress, hotStoreTestLimit)
		if err != nil {
			return err
		}
		return store.PutTable(result)
	}))
	db.Close()

	// 1. Restart: a new store over a reopened database must still serve the
	// record, and the restart check must keep it.
	db = open()
	require.NoError(db.Update(context.Background(), func(tx kv.RwTx) error {
		store := reopenedTestStore(t, tx, chain)
		got, ok, err := store.GetTable(ref)
		require.NoError(err)
		require.True(ok, "a committed record must survive the restart")
		require.Equal(result, got)

		report, err := store.Reconcile(15)
		require.NoError(err)
		require.Equal(1, report.Kept)
		require.Equal(0, report.Dropped())
		require.Equal(uint64(3), report.HighestCovered)
		return nil
	}))

	// 2. A bounded multiblock reorg replaces blocks 1..3; the record covers
	// them, so the restart check must report it stale and drop it.
	require.NoError(db.Update(context.Background(), func(tx kv.RwTx) error {
		for block := uint64(1); block <= 3; block++ {
			if err := rawdb.WriteCanonicalHash(tx, testHash(block+100), block); err != nil {
				return err
			}
		}
		report, err := reopenedTestStore(t, tx, chain).Reconcile(15)
		require.NoError(err)
		require.Equal(0, report.Kept)
		require.Equal(1, report.DroppedStale)
		return nil
	}))

	// Re-seed the chain, write the record again, and unwind to block 1: the
	// table's range reaches block 3, so it is invalid and must go.
	require.NoError(db.Update(context.Background(), func(tx kv.RwTx) error {
		for block := uint64(0); block < 16; block++ {
			if err := rawdb.WriteCanonicalHash(tx, chain.hashes[block], block); err != nil {
				return err
			}
		}
		store := reopenedTestStore(t, tx, chain)
		if err := store.PutTable(result); err != nil {
			return err
		}
		recovery, err := NewTableRecovery(tx)
		if err != nil {
			return err
		}
		generation, err := recovery.Generation()
		require.NoError(err)
		require.Equal(uint64(0), generation)

		dropped, err := recovery.Invalidate(2)
		require.NoError(err)
		require.Equal(1, dropped, "a table whose range reaches the unwind point must be dropped")

		_, ok, err := store.GetTable(ref)
		require.NoError(err)
		require.False(ok, "the unwound table must be gone")

		generation, err = recovery.Generation()
		require.NoError(err)
		require.Equal(uint64(1), generation, "invalidation bumps the generation in the same transaction")
		return nil
	}))
	db.Close()

	// 3. The store's state is the database's state: a second restart sees the
	// dropped record and the bumped generation, so pre-unwind jobs stay rejected.
	db = open()
	defer db.Close()
	require.NoError(db.Update(context.Background(), func(tx kv.RwTx) error {
		recovery, err := NewTableRecovery(tx)
		require.NoError(err)
		generation, err := recovery.Generation()
		require.NoError(err)
		require.Equal(uint64(1), generation, "the generation must survive a restart")

		store := reopenedTestStore(t, tx, chain)
		_, ok, err := store.GetTable(ref)
		require.NoError(err)
		require.False(ok)
		return nil
	}))
}

// reopenedTestStore builds a store the way the node would after a restart: the
// canonical hashes and execution progress come from the database.
func reopenedTestStore(t *testing.T, tx kv.RwTx, chain *chainStore) *HotTableStore {
	t.Helper()
	data, err := NewDBData(tx, chainReceipts{chain})
	require.NoError(t, err)
	progress, err := NewStageProgress(tx)
	require.NoError(t, err)
	store, err := NewHotTableStore(tx, data, progress, hotStoreTestLimit)
	require.NoError(t, err)
	return store
}
