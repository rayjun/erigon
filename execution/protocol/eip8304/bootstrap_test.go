package eip8304

import (
	"testing"
	"time"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/execution/types/accounts"
	"github.com/stretchr/testify/require"
)

// TestRebuildWindowIsTheDueTablesLeaves expands the due tables to level 0
// through MergeGroup and compares that independent expansion with the window
// the bootstrap derives from the covered ranges.
func TestRebuildWindowIsTheDueTablesLeaves(t *testing.T) {
	blocks := []uint64{0, 1, 3, 63, 64, 79, 255, 256, 319, 320, 383, 575, 831, 1087, 4095, 100000}
	for _, block := range blocks {
		want := map[uint64]struct{}{}
		var walk func(ref TableRef)
		walk = func(ref TableRef) {
			if ref.TableSize == 1 {
				if ref.FirstBlock != block {
					want[ref.FirstBlock] = struct{}{}
				}
				return
			}
			for _, child := range MergeGroup(ref.FirstBlock, ref.TableSize) {
				walk(child)
			}
		}
		for _, ref := range RebuildTables(block) {
			walk(ref)
		}

		got := RebuildWindow(block)
		expected := make([]uint64, 0, len(want))
		for b := range want {
			expected = append(expected, b)
		}
		slicesSort(expected)
		require.Equal(t, expected, got, "block %d", block)

		floor := uint64(0)
		if block > MaxTableLookback {
			floor = block - MaxTableLookback
		}
		for _, b := range got {
			require.Less(t, b, block)
			require.GreaterOrEqual(t, b, floor, "block %d leaf %d", block, b)
		}
		require.LessOrEqual(t, len(got), 256, "block %d", block)
	}
}

func slicesSort(v []uint64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// TestRebuildTablesSkipsTheProcessedBlocksOwnLevelZero: the in-flight block's
// level-0 table always comes from its receipts, so the bootstrap must never
// claim it from the store.
func TestRebuildTablesSkipsTheProcessedBlocksOwnLevelZero(t *testing.T) {
	for _, block := range []uint64{0, 4, 19, 79, 319, 575} {
		for _, ref := range RebuildTables(block) {
			require.NotEqual(t, block, ref.FirstBlock, "block %d", block)
			require.NotEqual(t, uint64(1), ref.TableSize, "block %d", block)
		}
	}
	require.Empty(t, RebuildTables(0))
	// Block 319 is where the (0,256) table is due: exactly one higher level.
	require.Equal(t, []TableRef{{FirstBlock: 0, TableSize: 256}}, RebuildTables(319))
}

// TestPruneFloorKeepsTheWholeRebuildWindow: pruning at the floor must leave
// every level-0 record the window needs, and pruning one block higher must drop
// the oldest one.
func TestPruneFloorKeepsTheWholeRebuildWindow(t *testing.T) {
	for _, block := range []uint64{0, 1, 319, 320, 575, 1087, 100000} {
		floor := PruneFloor(block)
		for _, b := range RebuildWindow(block) {
			require.GreaterOrEqual(t, b, floor, "block %d window leaf %d", block, b)
		}
	}
	require.Equal(t, uint64(0), PruneFloor(319))
	require.Equal(t, uint64(1), PruneFloor(320))
	require.Equal(t, uint64(256), PruneFloor(575))
	require.Equal(t, uint64(0), PruneFloor(0))
}

// TestPruneKeepsTheRecordsAFutureMergeNeeds ties the prune watermark to the
// rebuild window: pruning at the floor derived from the block about to be
// processed leaves every level-0 record its due table merges, so the table is
// still served from the store with the canonical inputs gone; pruning one
// block higher drops the oldest covered record, and the same merge must then
// fail closed instead of publishing a short window.
func TestPruneKeepsTheRecordsAFutureMergeNeeds(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319 // the (0,256) table is due; its window is 0..255, floor 0
	top := TableRef{FirstBlock: 0, TableSize: 256}

	seed := func() (*HotTableStore, *chainStore, kv.RwTx) {
		chain := newTestChain(t, tip+1)
		db := newHotStoreDB(t)
		tx := beginHotStoreTx(t, db)
		store := newTestHotStore(t, tx, chain, tip)
		if _, err := Bootstrap(tip, activeAlways, NewResolver(store, limit), true); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		return store, chain, tx
	}

	// At the floor the whole window survives, so the due table resolves from
	// stored records alone.
	store, chain, tx := seed()
	defer tx.Rollback()
	require.Equal(t, uint64(0), PruneFloor(tip))
	pruned, err := store.Prune(PruneFloor(tip))
	require.NoError(t, err)
	require.Zero(t, pruned)
	for _, b := range RebuildWindow(tip) {
		_, ok, err := store.GetTable(L0(b))
		require.NoError(t, err)
		require.True(t, ok, "block %d must survive the floor prune", b)
	}
	want := oracleRoot(t, chain, top, limit)
	chain.blocks = map[uint64]Entries{}
	served := NewResolver(store, limit)
	table, err := served.Table(top)
	require.NoError(t, err)
	require.Equal(t, want, table.Root)
	require.GreaterOrEqual(t, served.Stats().Hits, 256, "the window is served from the store")

	// One block above the floor drops the oldest covered record.
	above, aboveChain, aboveTx := seed()
	defer aboveTx.Rollback()
	pruned, err = above.Prune(PruneFloor(tip) + 1)
	require.NoError(t, err)
	require.Equal(t, 1, pruned, "only the oldest covered record is below the raised watermark")
	if _, ok, err := above.GetTable(L0(0)); err != nil || ok {
		t.Fatalf("block 0 must be gone, ok=%v err=%v", ok, err)
	}
	aboveChain.blocks = map[uint64]Entries{}
	_, err = NewResolver(above, limit).Table(top)
	require.ErrorIs(t, err, ErrMissingBlockData)
}

// TestBootstrapRebuildsAFullWindowFromReceipts is the 319-block path end to
// end: with an empty store the bootstrap rebuilds the 256 level-0 records of
// the window, records them, and produces the (0,256) root that an independent
// from-scratch merge produces.
func TestBootstrapRebuildsAFullWindowFromReceipts(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319 // the (0,256) table is due here
	chain := newTestChain(t, tip+1)
	resolver := NewResolver(chain, limit)

	result, err := Bootstrap(tip, activeAlways, resolver, true)
	require.NoError(t, err)
	require.Equal(t, 256, result.LevelZero)
	require.Equal(t, 256, result.Recorded)
	require.Len(t, result.Tables, 1)
	require.Equal(t, TableRef{FirstBlock: 0, TableSize: 256}, result.Tables[0].Ref)
	require.Equal(t, oracleRoot(t, chain, TableRef{FirstBlock: 0, TableSize: 256}, limit), result.Tables[0].Root)
	require.Equal(t, len(result.Tables[0].Entries), result.Entries)
	require.GreaterOrEqual(t, result.Stats.Rebuilds, 256)
	require.Len(t, chain.puts, 256, "every level-0 record of the window is written")
	require.NotZero(t, result.RecordedBytes, "the pass reports the disk it wrote")
}

// TestBootstrapIsNotIncremental: running the pass twice for the same block
// rebuilds the same records and overwrites them; it neither appends a second
// copy nor changes the roots. A caller runs it once per block.
func TestBootstrapIsNotIncremental(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319
	chain := newTestChain(t, tip+1)

	first, err := Bootstrap(tip, activeAlways, NewResolver(chain, limit), true)
	require.NoError(t, err)
	written := len(chain.puts)
	require.NotZero(t, written)

	second, err := Bootstrap(tip, activeAlways, NewResolver(chain, limit), true)
	require.NoError(t, err)
	require.Equal(t, first.Tables[0].Root, second.Tables[0].Root)
	require.Equal(t, first.Recorded, second.Recorded)
	require.Len(t, chain.puts, 2*written, "the second pass writes every record again")
	require.Len(t, chain.cached, written, "and overwrites instead of appending")
}

// TestBootstrapParityWithRecordedLevelZero covers the configuration parity the
// rebuild path must have: once the window is recorded, a node whose receipts
// are unavailable produces the same roots from the store, and a node without
// either input fails closed instead of diverging.
func TestBootstrapParityWithRecordedLevelZero(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319
	chain := newTestChain(t, tip+1)

	filled, err := Bootstrap(tip, activeAlways, NewResolver(chain, limit), true)
	require.NoError(t, err)

	// Receipts are gone; the stored level-0 records are all that is left.
	chain.blocks = map[uint64]Entries{}
	reused, err := Bootstrap(tip, activeAlways, NewResolver(chain, limit), false)
	require.NoError(t, err)
	require.Equal(t, filled.Tables[0].Root, reused.Tables[0].Root)
	require.Zero(t, reused.Recorded)
	require.GreaterOrEqual(t, reused.Stats.Hits, 256, "each level-0 record must be served from the store")

	// Both inputs gone: fail closed, and never write a partial table.
	empty := newTestChain(t, tip+1)
	empty.blocks = map[uint64]Entries{}
	_, err = Bootstrap(tip, activeAlways, NewResolver(empty, limit), false)
	require.ErrorIs(t, err, ErrMissingBlockData)
	require.Empty(t, empty.puts)
}

// TestBootstrapFailsClosedWhenOnlyPartOfTheWindowIsAvailable: a node that
// retains some of the covered blocks but not all must not publish a root built
// from a short window.
func TestBootstrapFailsClosedWhenOnlyPartOfTheWindowIsAvailable(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319
	chain := newTestChain(t, tip+1)
	for block := range chain.blocks {
		if block < 100 {
			delete(chain.blocks, block)
		}
	}

	_, err := Bootstrap(tip, activeAlways, NewResolver(chain, limit), true)
	require.ErrorIs(t, err, ErrMissingBlockData)
	require.Empty(t, chain.puts, "nothing is recorded before the missing input is reached")
}

// TestBootstrapIsGatedByActivation: tables the fork was not active for are
// skipped, and their level-0 blocks are not recorded either.
func TestBootstrapIsGatedByActivation(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319
	chain := newTestChain(t, tip+1)
	chain.blocks = map[uint64]Entries{} // prove the gate is checked before any input

	result, err := Bootstrap(tip, func(uint64) bool { return false }, NewResolver(chain, limit), true)
	require.NoError(t, err)
	require.Empty(t, result.Tables)
	require.Zero(t, result.LevelZero)
	require.Zero(t, result.Recorded)
	require.Empty(t, chain.puts)
}

// TestBootstrapRejectsNilInputs mirrors ApplyDueTables: the activation
// predicate and resolver are required, never defaulted.
func TestBootstrapRejectsNilInputs(t *testing.T) {
	chain := newTestChain(t, 8)
	resolver := NewResolver(chain, 1<<12)
	_, err := Bootstrap(7, nil, resolver, false)
	require.ErrorIs(t, err, ErrNilActivationPredicate)
	_, err = Bootstrap(7, activeAlways, nil, false)
	require.ErrorIs(t, err, ErrNilTableResolver)
}

// TestBootstrapCostOnAFullWindow records the cost of the 256-block rebuild the
// first post-sync 256-level table needs. The numbers are logged rather than
// asserted tightly: the point is a reproducible measurement, not a perf gate.
func TestBootstrapCostOnAFullWindow(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319
	chain := newTestChain(t, tip+1)

	start := time.Now()
	result, err := Bootstrap(tip, activeAlways, NewResolver(chain, limit), true)
	require.NoError(t, err)
	t.Logf("bootstrap window=%d recorded=%d bytes=%d tables=%d entries=%d resolver_rebuilds=%d elapsed=%s",
		result.LevelZero, result.Recorded, result.RecordedBytes, len(result.Tables), result.Entries, result.Stats.Rebuilds, time.Since(start))
}

// TestBootstrapMakesTheFirstPostSyncBlockDeterministic is the Week 16 exit
// criterion: a node that lands on a tip and then processes the next block must
// not depend on inputs it may have pruned. After the window is recorded, block
// 320 resolves its due (316,4) table from the stored level-0 records, and only
// the in-flight block's own receipts are read. Without the window the same call
// fails closed.
func TestBootstrapMakesTheFirstPostSyncBlockDeterministic(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319
	const next = tip + 1
	indexAddr := accounts.InternAddress(common.HexToAddress("0x8304"))
	syscall := func(addr accounts.Address, data []byte) ([]byte, error) { return nil, nil }

	chain := newTestChain(t, next+1)
	db := newHotStoreDB(t)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, next)

	due := TableRef{FirstBlock: 316, TableSize: 4}
	want := oracleRoot(t, chain, due, limit)

	// Land on the tip: rebuild and record the window the next block needs.
	// The bootstrap is for the block about to be processed, so the window is
	// the due table's covered range, not the tip's.
	if _, err := Bootstrap(next, activeAlways, NewResolver(store, limit), true); err != nil {
		t.Fatal(err)
	}
	// The window is now in the store: every level-0 record the next block
	// needs is served back without touching receipts.
	for _, b := range RebuildWindow(next) {
		_, recorded, err := store.GetTable(L0(b))
		require.NoError(t, err)
		require.True(t, recorded, "the bootstrap must persist level-0 record %d", b)
	}

	// Receipts for every block before the tip are gone; only the stored
	// level-0 records of the window are left.
	chain.blocks = map[uint64]Entries{}

	block := uint64(next)
	applied, err := ApplyDueTables(block, testHash(block), chain.parentHash(block), chain.receipts[block], indexAddr, activeAlways, NewResolver(store, limit), syscall)
	require.NoError(t, err)
	require.Contains(t, applied, due)

	record, ok, err := store.GetTable(due)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, record.Root)

	// Control: without the recorded window, the same block fails closed.
	empty := newTestChain(t, next+1)
	empty.blocks = map[uint64]Entries{}
	_, err = ApplyDueTables(block, testHash(block), empty.parentHash(block), empty.receipts[block], indexAddr, activeAlways, NewResolver(empty, limit), syscall)
	require.ErrorIs(t, err, ErrMissingBlockData)
}
