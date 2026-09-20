package eip8304

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/memdb"
	"github.com/erigontech/erigon/execution/types/accounts"
)

const hotStoreTestLimit = uint64(1 << 12)

func hotStoreTestProgress(height uint64) ExecutionProgress {
	return ProgressFunc(func() (uint64, error) { return height, nil })
}

func newHotStoreDB(t *testing.T) kv.RwDB {
	t.Helper()
	db := memdb.NewChainDB(t, t.TempDir())
	t.Cleanup(db.Close)
	return db
}

func beginHotStoreTx(t *testing.T, db kv.RwDB) kv.RwTx {
	t.Helper()
	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	return tx
}

func newTestHotStore(t *testing.T, tx kv.RwTx, chain CanonicalData, progress uint64) *HotTableStore {
	t.Helper()
	return newTestHotStoreWithProgress(t, tx, chain, hotStoreTestProgress(progress).ExecutedHeight)
}

func newTestHotStoreWithProgress(t *testing.T, tx kv.RwTx, chain CanonicalData, progress func() (uint64, error)) *HotTableStore {
	t.Helper()
	store, err := NewHotTableStore(tx, chain, ProgressFunc(progress), hotStoreTestLimit)
	require.NoError(t, err)
	return store
}

// hotStoreSeed builds a verified result for ref through the resolver, which is
// how the finalize path obtains one.
func hotStoreSeed(t *testing.T, chain *chainStore, ref TableRef) TableResult {
	t.Helper()
	result, err := NewResolver(chain, hotStoreTestLimit).Table(ref)
	require.NoError(t, err)
	return result
}

func TestHotTableStoreRoundTripsThroughCommit(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	result := hotStoreSeed(t, chain, ref)

	tx := beginHotStoreTx(t, db)
	store := newTestHotStore(t, tx, chain, 15)
	require.NoError(t, store.PutTable(result))

	got, ok, err := store.GetTable(ref)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, result, got, "a record must round-trip entry for entry")
	require.NoError(t, tx.Commit())

	tx2 := beginHotStoreTx(t, db)
	defer tx2.Rollback()
	got, ok, err = newTestHotStore(t, tx2, chain, 15).GetTable(ref)
	require.NoError(t, err)
	require.True(t, ok, "a committed record must survive a new transaction")
	require.Equal(t, result, got)
}

func TestHotTableStoreMissIsNotAnError(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()

	_, ok, err := newTestHotStore(t, tx, chain, 15).GetTable(TableRef{FirstBlock: 0, TableSize: 4})
	require.NoError(t, err, "an absent record is a miss, not a failure")
	require.False(t, ok)
}

// TestHotTableStoreRollsBackWithItsTransaction covers the atomicity half of the
// store's contract: because records are written through the caller's
// transaction, an aborted block leaves no table behind.
func TestHotTableStoreRollsBackWithItsTransaction(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}

	tx := beginHotStoreTx(t, db)
	store := newTestHotStore(t, tx, chain, 15)
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ref)))
	tx.Rollback()

	tx2 := beginHotStoreTx(t, db)
	defer tx2.Rollback()
	_, ok, err := newTestHotStore(t, tx2, chain, 15).GetTable(ref)
	require.NoError(t, err)
	require.False(t, ok, "a record from a rolled back transaction must not be visible")
}

// TestHotTableStoreRejectsDamagedRecords covers the partial-write half: every
// way a record can be damaged is reported, so the resolver rebuilds instead of
// trusting a record that only looks like a table.
func TestHotTableStoreRejectsDamagedRecords(t *testing.T) {
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	damages := []struct {
		name   string
		damage func(t *testing.T, raw []byte) []byte
		want   error
	}{
		{"truncated value", func(_ *testing.T, raw []byte) []byte { return raw[:len(raw)-1] }, ErrTableRecordCorrupt},
		{"bad magic", func(_ *testing.T, raw []byte) []byte { raw[0] ^= 0xff; return raw }, ErrTableRecordCorrupt},
		{"unknown record version", func(_ *testing.T, raw []byte) []byte { raw[hotStoreVersionOffset] = 99; return raw }, ErrTableRecordVersion},
		{"incomplete write", func(_ *testing.T, raw []byte) []byte { raw[hotStoreStateOffset] = 0; return raw }, ErrTableRecordPartial},
		{"flipped payload byte", func(_ *testing.T, raw []byte) []byte {
			raw[len(raw)-hotStoreTrailer-1] ^= 0x01
			return raw
		}, ErrTableRecordCorrupt},
		{"all zeroes", func(_ *testing.T, raw []byte) []byte { return make([]byte, len(raw)) }, ErrTableRecordCorrupt},
	}

	for _, tc := range damages {
		t.Run(tc.name, func(t *testing.T) {
			db := newHotStoreDB(t)
			tx := beginHotStoreTx(t, db)
			defer tx.Rollback()
			store := newTestHotStore(t, tx, chain, 15)
			require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ref)))

			raw, err := tx.GetOne(kv.Eip8304Tables, hotStoreKey(ref))
			require.NoError(t, err)
			require.NoError(t, tx.Put(kv.Eip8304Tables, hotStoreKey(ref), tc.damage(t, append([]byte(nil), raw...))))

			_, ok, err := store.GetTable(ref)
			require.ErrorIs(t, err, tc.want)
			require.False(t, ok)
		})
	}
}

func TestHotTableStoreRejectsRecordsAboveExecutedHeight(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()

	result := hotStoreSeed(t, chain, ref)
	require.NoError(t, newTestHotStore(t, tx, chain, 3).PutTable(result))

	_, ok, err := newTestHotStore(t, tx, chain, 2).GetTable(ref)
	require.ErrorIs(t, err, ErrTableRecordProgress, "a record covering blocks above the executed height cannot be canonical")
	require.False(t, ok)
}

func TestHotTableStoreValidatesRecordsBeforeWriting(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	result := hotStoreSeed(t, chain, ref)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 15)

	countMismatch := result
	countMismatch.EntryCount = uint64(len(result.Entries)) + 1
	require.ErrorIs(t, store.PutTable(countMismatch), ErrTableEntryCount)

	coverageMismatch := result
	coverageMismatch.BlockHashes = result.BlockHashes[:1]
	require.ErrorIs(t, store.PutTable(coverageMismatch), ErrTableCoverageMismatch)

	badRef := result
	badRef.Ref = TableRef{FirstBlock: 1, TableSize: 4}
	require.ErrorIs(t, store.PutTable(badRef), ErrInvalidTableRef)

	tiny, err := NewHotTableStore(tx, chain, hotStoreTestProgress(15), 1)
	require.NoError(t, err)
	require.ErrorIs(t, tiny.PutTable(result), ErrTableRecordTooLarge)

	_, _, err = store.GetTable(TableRef{FirstBlock: 1, TableSize: 4})
	require.ErrorIs(t, err, ErrInvalidTableRef)
}

func TestNewHotTableStoreRejectsNilInputs(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 4)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()

	_, err := NewHotTableStore(nil, chain, hotStoreTestProgress(3), hotStoreTestLimit)
	require.ErrorIs(t, err, ErrNilTableStoreTx)
	_, err = NewHotTableStore(tx, nil, hotStoreTestProgress(3), hotStoreTestLimit)
	require.ErrorIs(t, err, ErrNilCanonicalData)
	_, err = NewHotTableStore(tx, chain, nil, hotStoreTestLimit)
	require.ErrorIs(t, err, ErrNilProgressSource)
}

func TestHotTableStorePrunesByCoveredRange(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 8)
	refs := []TableRef{L0(0), L0(1), L0(2), L0(3), {FirstBlock: 0, TableSize: 4}}
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 7)
	for _, ref := range refs {
		require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ref)))
	}

	pruned, err := store.Prune(2)
	require.NoError(t, err)
	require.Equal(t, 2, pruned, "the two level-0 tables ending below block 2 are dropped")

	for _, ref := range refs {
		_, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		wantKept := ref.FirstBlock+ref.TableSize-1 >= 2
		require.Equal(t, wantKept, ok, "retention follows the covered range, not arrival order (%+v)", ref)
	}
}

// TestResolverRebuildsAroundADamagedHotStoreRecord is the end-to-end contract:
// a damaged record must never leak into a published root, and must never abort
// the block either — it is rejected and rebuilt from canonical data.
func TestResolverRebuildsAroundADamagedHotStoreRecord(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 15)

	// A stored, verified record is used without a rebuild.
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ref)))
	hitResolver := NewResolver(store, hotStoreTestLimit)
	_, err := hitResolver.Table(ref)
	require.NoError(t, err)
	require.Equal(t, 1, hitResolver.Stats().Hits)

	// Damage it: the resolver rejects the record and rebuilds the same root.
	raw, err := tx.GetOne(kv.Eip8304Tables, hotStoreKey(ref))
	require.NoError(t, err)
	raw[len(raw)-hotStoreTrailer-1] ^= 0x01
	require.NoError(t, tx.Put(kv.Eip8304Tables, hotStoreKey(ref), raw))

	rebuildResolver := NewResolver(store, hotStoreTestLimit)
	got, err := rebuildResolver.Table(ref)
	require.NoError(t, err)
	require.Equal(t, oracleRoot(t, chain, ref, hotStoreTestLimit), got.Root)
	require.ErrorIs(t, rebuildResolver.Stats().LastReject, ErrTableRecordCorrupt)
	require.Equal(t, 1, rebuildResolver.Stats().Rejected, "only the damaged top-level record is rejected")
	require.Equal(t, 5, rebuildResolver.Stats().Rebuilds, "the damaged table and its four children are rebuilt")
}

// TestResolverRejectsHotStoreRecordFromAnOldChain covers the reorg half: the
// record is well-formed but bound to block hashes that are no longer canonical,
// so it is rejected and rebuilt for the current chain.
func TestResolverRejectsHotStoreRecordFromAnOldChain(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 15)
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ref)))

	// A reorg replaces one block in the covered range.
	stale := testHash(1)
	chain.hashes[1] = testHash(101)

	resolver := NewResolver(store, hotStoreTestLimit)
	got, err := resolver.Table(ref)
	require.NoError(t, err)
	require.Equal(t, oracleRoot(t, chain, ref, hotStoreTestLimit), got.Root)
	require.NotEqual(t, stale, got.BlockHashes[1])
	require.ErrorIs(t, resolver.Stats().LastReject, ErrTableNotCanonical)
	require.Equal(t, 1, resolver.Stats().Rejected)
}

// TestHotTableStoreOverwritesARejectedRecordAfterRebuild checks the write path
// that follows a rejection: the rebuild's result replaces the stale record.
func TestHotTableStoreOverwritesARejectedRecordAfterRebuild(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 15)
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ref)))

	chain.hashes[2] = testHash(102)
	rebuilt, err := NewResolver(store, hotStoreTestLimit).Table(ref)
	require.NoError(t, err)
	require.NoError(t, store.PutTable(rebuilt))

	got, ok, err := store.GetTable(ref)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, rebuilt.BlockHashes, got.BlockHashes)
	var want []common.Hash
	for block := uint64(0); block < 4; block++ {
		hash, err := chain.CanonicalHash(block)
		require.NoError(t, err)
		want = append(want, hash)
	}
	require.Equal(t, want, got.BlockHashes, "the record must now be bound to the current canonical hashes")
}

// TestHotTableStoreInvalidateDropsTouchedTables covers unwind: a table is only
// valid while its whole covered range survives, so unwinding to a block drops
// every record whose range reaches it.
func TestHotTableStoreInvalidateDropsTouchedTables(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 8)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 7)

	refs := []TableRef{L0(0), L0(1), L0(2), L0(3), L0(4), L0(5), {FirstBlock: 0, TableSize: 4}}
	for _, ref := range refs {
		require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ref)))
	}

	dropped, err := store.Invalidate(4)
	require.NoError(t, err)
	require.Equal(t, 2, dropped, "the level-0 tables of blocks 4 and 5 are the only records whose range reaches block 4")

	for _, ref := range refs {
		_, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		wantKept := ref.FirstBlock+ref.TableSize-1 < 4
		require.Equal(t, wantKept, ok, "unwinding to block 4 must invalidate %+v", ref)
	}

	generation, err := store.Generation()
	require.NoError(t, err)
	require.Equal(t, uint64(1), generation, "invalidation bumps the generation")
}

// TestHotTableStoreReconcileDropsUnusableRecords covers restart: records are
// checked against the executed height and the canonical chain, and anything
// that does not check out is dropped so the next read rebuilds it.
func TestHotTableStoreReconcileDropsUnusableRecords(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 8)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 7)

	// Valid: covered by the executed height and canonical.
	valid := TableRef{FirstBlock: 0, TableSize: 4}
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, valid)))
	// Ahead of execution: covers block 6 while only 5 blocks were executed.
	ahead := L0(6)
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, ahead)))
	// Stale: a reorg replaced block 5 after the record was written.
	stale := L0(5)
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, stale)))
	chain.hashes[5] = testHash(105)
	// Damaged: a flipped byte in the payload.
	damaged := L0(1)
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, damaged)))
	raw, err := tx.GetOne(kv.Eip8304Tables, hotStoreKey(damaged))
	require.NoError(t, err)
	raw[len(raw)-hotStoreTrailer-1] ^= 0x01
	require.NoError(t, tx.Put(kv.Eip8304Tables, hotStoreKey(damaged), raw))

	report, err := store.Reconcile(5)
	require.NoError(t, err)
	require.Equal(t, 4, report.Checked)
	require.Equal(t, 1, report.Kept)
	require.Equal(t, 1, report.DroppedAhead)
	require.Equal(t, 1, report.DroppedStale)
	require.Equal(t, 1, report.DroppedDamaged)
	require.Equal(t, 3, report.Dropped())
	require.True(t, report.HasRecords)
	require.Equal(t, uint64(3), report.HighestCovered, "only the surviving (0,4) table is counted")

	_, ok, err := store.GetTable(valid)
	require.NoError(t, err)
	require.True(t, ok, "a record that still checks out must survive a restart")
	for _, ref := range []TableRef{ahead, stale, damaged} {
		_, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		require.False(t, ok, "%+v must be dropped so it is rebuilt", ref)
	}

	// The surviving record is still usable, and the dropped ones are rebuilt
	// from canonical data.
	resolver := NewResolver(store, hotStoreTestLimit)
	result, err := resolver.Table(valid)
	require.NoError(t, err)
	require.Equal(t, oracleRoot(t, chain, valid, hotStoreTestLimit), result.Root)
	require.Equal(t, 1, resolver.Stats().Hits, "the reconciled record is served from the store")

	rebuilt, err := resolver.Table(stale)
	require.NoError(t, err)
	require.Equal(t, oracleRoot(t, chain, stale, hotStoreTestLimit), rebuilt.Root)
	require.Equal(t, testHash(105), rebuilt.BlockHashes[0], "the rebuilt table is bound to the reorged chain")
}

// TestHotTableStoreReconcileRejectsRecordsThatDoNotMatchTheirMeaning covers the
// check a checksum cannot make: a record can be intact on disk (valid CRC, valid
// magic, decodable entries) and still be unusable because its root is not the
// root its own entries produce, or because its entries are not in canonical
// order. A restart must count such a record as damaged and drop it, so the next
// read rebuilds instead of the report calling a broken table healthy.
func TestHotTableStoreReconcileRejectsRecordsThatDoNotMatchTheirMeaning(t *testing.T) {
	chain := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}

	cases := []struct {
		name   string
		damage func(t *testing.T, result TableResult) TableResult
	}{
		{
			"root does not match the entries",
			func(_ *testing.T, result TableResult) TableResult {
				// Rewrite an entry in place but keep the root the record
				// claims, then re-encode: the CRC is recomputed and valid.
				result.Entries[len(result.Entries)-1].Value = testHash(9999)
				return result
			},
		},
		{
			"entries are not in canonical order",
			func(t *testing.T, result TableResult) TableResult {
				require.Greater(t, len(result.Entries), 1)
				entries := make(Entries, len(result.Entries))
				copy(entries, result.Entries)
				first, last := entries[0], entries[len(entries)-1]
				require.NotEqual(t, first.Encode(), last.Encode(), "the swapped entries must differ")
				entries[0], entries[len(entries)-1] = last, first
				result.Entries = entries
				// Keep the record self-consistent so only the order check can
				// reject it.
				root, err := RootOfEntries(entries, hotStoreTestLimit)
				require.NoError(t, err)
				result.Root = root
				return result
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newHotStoreDB(t)
			tx := beginHotStoreTx(t, db)
			defer tx.Rollback()
			store := newTestHotStore(t, tx, chain, 15)

			// The tampered record is written directly, bypassing PutTable, so
			// it lands in the store exactly as a broken writer would leave it.
			require.NoError(t, tx.Put(kv.Eip8304Tables, hotStoreKey(ref), encodeTableRecord(tc.damage(t, hotStoreSeed(t, chain, ref)))))

			report, err := store.Reconcile(15)
			require.NoError(t, err)
			require.Equal(t, 1, report.Checked)
			require.Equal(t, 0, report.Kept)
			require.Equal(t, 1, report.DroppedDamaged, "an unusable record must be reported, not kept")
			require.False(t, report.HasRecords)

			_, ok, err := store.GetTable(ref)
			require.NoError(t, err)
			require.False(t, ok, "the dropped record must not be served after the restart")

			rebuilt, err := NewResolver(store, hotStoreTestLimit).Table(ref)
			require.NoError(t, err)
			require.Equal(t, oracleRoot(t, chain, ref, hotStoreTestLimit), rebuilt.Root, "the dropped record is rebuilt from canonical data")
		})
	}
}

// TestHotTableStoreJobGuardCoversStaleAndInvalidatedJobs covers delayed or
// background merges: a result may only be written back while the chain view it
// was computed against still holds.
func TestHotTableStoreJobGuardCoversStaleAndInvalidatedJobs(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 8)
	ref := TableRef{FirstBlock: 0, TableSize: 4}

	t.Run("valid job commits", func(t *testing.T) {
		tx := beginHotStoreTx(t, db)
		defer tx.Rollback()
		store := newTestHotStore(t, tx, chain, 7)
		job, err := store.BeginJob(ref)
		require.NoError(t, err)
		require.Equal(t, ref, job.Ref())
		require.NoError(t, store.CommitJob(job, hotStoreSeed(t, chain, ref)))

		_, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("invalidated job is rejected", func(t *testing.T) {
		tx := beginHotStoreTx(t, db)
		defer tx.Rollback()
		store := newTestHotStore(t, tx, chain, 7)
		job, err := store.BeginJob(ref)
		require.NoError(t, err)

		_, err = store.Invalidate(2)
		require.NoError(t, err)

		err = store.CommitJob(job, hotStoreSeed(t, chain, ref))
		require.ErrorIs(t, err, ErrStaleTableJob)
		_, ok, getErr := store.GetTable(ref)
		require.NoError(t, getErr)
		require.False(t, ok, "a rejected job must not resurrect a record")
	})

	t.Run("reorged job is rejected", func(t *testing.T) {
		tx := beginHotStoreTx(t, db)
		defer tx.Rollback()
		store := newTestHotStore(t, tx, chain, 7)
		job, err := store.BeginJob(ref)
		require.NoError(t, err)

		chain.hashes[2] = testHash(102)
		err = store.CommitJob(job, hotStoreSeed(t, chain, ref))
		require.ErrorIs(t, err, ErrStaleTableJob)
	})

	t.Run("mismatched result is rejected", func(t *testing.T) {
		tx := beginHotStoreTx(t, db)
		defer tx.Rollback()
		store := newTestHotStore(t, tx, chain, 7)
		job, err := store.BeginJob(ref)
		require.NoError(t, err)

		other := hotStoreSeed(t, chain, TableRef{FirstBlock: 4, TableSize: 4})
		require.ErrorIs(t, store.CommitJob(job, other), ErrTableRefMismatch)
	})
}

// TestHotTableStoreSurvivesRestartWithItsGeneration checks the store's own
// metadata across a restart: the generation counter is persisted, so a job from
// before the restart is still rejected afterwards.
func TestHotTableStoreSurvivesRestartWithItsGeneration(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 8)
	ref := TableRef{FirstBlock: 0, TableSize: 4}

	tx := beginHotStoreTx(t, db)
	store := newTestHotStore(t, tx, chain, 7)
	job, err := store.BeginJob(ref)
	require.NoError(t, err)
	_, err = store.Invalidate(1)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	tx2 := beginHotStoreTx(t, db)
	defer tx2.Rollback()
	restarted := newTestHotStore(t, tx2, chain, 7)
	generation, err := restarted.Generation()
	require.NoError(t, err)
	require.Equal(t, uint64(1), generation, "the generation survives a restart")

	require.ErrorIs(t, restarted.CommitJob(job, hotStoreSeed(t, chain, ref)), ErrStaleTableJob)
	report, err := restarted.Reconcile(7)
	require.NoError(t, err)
	require.Equal(t, 0, report.Checked, "invalidation left no records to reconcile")
}

// TestHotTableStoreSurvivesBoundedMultiblockReorg covers a reorg wider than one
// block: the tables covering the replaced blocks are invalidated, the ones
// before it survive, and the rebuild still reuses what survived.
func TestHotTableStoreSurvivesBoundedMultiblockReorg(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 32)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 31)

	target := TableRef{FirstBlock: 0, TableSize: 16}
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, target)))
	survivor := TableRef{FirstBlock: 0, TableSize: 4}
	require.NoError(t, store.PutTable(hotStoreSeed(t, chain, survivor)))
	for block := uint64(0); block < 8; block++ {
		require.NoError(t, store.PutTable(hotStoreSeed(t, chain, L0(block))))
	}

	// Blocks 5, 6 and 7 are replaced by a bounded reorg.
	for _, block := range []uint64{5, 6, 7} {
		chain.hashes[block] = testHash(1000 + block)
	}

	dropped, err := store.Invalidate(5)
	require.NoError(t, err)
	require.Equal(t, 4, dropped, "the (0,16) table and the level-0 tables of blocks 5..7 reach block 5")

	report, err := store.Reconcile(31)
	require.NoError(t, err)
	require.Equal(t, 0, report.DroppedStale, "invalidation already removed the stale records")

	resolver := NewResolver(store, hotStoreTestLimit)
	rebuilt, err := resolver.Table(target)
	require.NoError(t, err)
	require.Equal(t, oracleRoot(t, chain, target, hotStoreTestLimit), rebuilt.Root, "the rebuilt table follows the new chain")
	require.Equal(t, 2, resolver.Stats().Hits, "the surviving (0,4) table and the level-0 table of block 4 are reused")

	got, ok, err := store.GetTable(survivor)
	require.NoError(t, err)
	require.True(t, ok, "a table whose range ends before the reorg survives")
	require.Equal(t, testHash(0), got.BlockHashes[0])
}

// TestApplyDueTablesFillsAndReusesTheHotStore runs the finalize path against the
// persistent store: tables written while producing blocks are reused, across a
// commit, when the same tables come due again.
func TestApplyDueTablesFillsAndReusesTheHotStore(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 5)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x8304"))
	executed := uint64(0)

	syscall := func(addr accounts.Address, data []byte) ([]byte, error) {
		return nil, nil
	}

	// First pass: produce blocks 0..4, recording every applied table root.
	firstPass := make(map[TableRef]common.Hash)
	tx := beginHotStoreTx(t, db)
	store := newTestHotStoreWithProgress(t, tx, chain, func() (uint64, error) { return executed, nil })
	resolver := NewResolver(store, hotStoreTestLimit)
	for block := uint64(0); block <= 4; block++ {
		chain.available = block
		executed = block
		applied, err := ApplyDueTables(block, testHash(block), chain.parentHash(block), chain.receipts[block], indexAddr, activeAlways, resolver, syscall)
		require.NoError(t, err)
		for _, ref := range applied {
			record, ok, err := store.GetTable(ref)
			require.NoError(t, err)
			require.True(t, ok, "%+v must be recorded once its system call succeeded", ref)
			firstPass[ref] = record.Root
		}
	}
	require.Contains(t, firstPass, L0(4))
	require.Contains(t, firstPass, TableRef{FirstBlock: 0, TableSize: 4}, "the delayed (0,4) table is due at block 4")
	require.NoError(t, tx.Commit())

	// Second pass after a "restart": the same tables come from the store.
	tx2 := beginHotStoreTx(t, db)
	defer tx2.Rollback()
	restarted := newTestHotStoreWithProgress(t, tx2, chain, func() (uint64, error) { return executed, nil })
	reuseResolver := NewResolver(restarted, hotStoreTestLimit)
	for block := uint64(0); block <= 4; block++ {
		chain.available = block
		executed = block
		_, err := ApplyDueTables(block, testHash(block), chain.parentHash(block), chain.receipts[block], indexAddr, activeAlways, reuseResolver, syscall)
		require.NoError(t, err)
	}

	stats := reuseResolver.Stats()
	require.Zero(t, stats.Rejected, "nothing in the committed store should be rejected after a restart")
	require.Zero(t, stats.Rebuilds, "the higher-level table recorded in the first pass must not be recomputed")
	require.Equal(t, 1, stats.Hits, "the delayed (0,4) table is served from the store; level-0 tables always come from the block's receipts")

	for ref, root := range firstPass {
		record, ok, err := restarted.GetTable(ref)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, root, record.Root, "%+v must keep the root it was written with", ref)
	}
}
