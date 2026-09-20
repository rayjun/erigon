package eip8304

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/memdb"
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
	store, err := NewHotTableStore(tx, chain, hotStoreTestProgress(progress), hotStoreTestLimit)
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
