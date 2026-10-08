package eip8304

import (
	"fmt"
	"slices"
	"time"
)

// Bootstrap for a node that reaches a tip without having merged the window the
// next block's delayed tables need.
//
// A table due at `block` merges the level-0 records of every block it covers,
// and the largest table reaches back 319 blocks. A node that executed that
// window already has the records in its store; a node that synced past it must
// rebuild them from canonical data before the first post-sync block runs, or
// ApplyDueTables stops on the missing input.
//
// RebuildWindow names the window, Bootstrap rebuilds it through the resolver
// and (optionally) records its level-0 results in the caller's transaction, so
// the delayed tables resolve from the store rather than from receipts. A
// missing input fails the pass closed. Details: docs/eip8304/sync-rebuild.md.

// MaxTableLookback is how far back a table due at `block` can reach. The
// 256-block level is written at first_block+319 (see WriteBlock), so a due
// table never covers a block older than block-MaxTableLookback, and never needs
// that block's level-0 record.
const MaxTableLookback = 319

// RebuildTables returns the tables due at `block` that a node rebuilds from
// stored history, in ascending table size like DueTables. The block's own
// level-0 table is excluded; it comes from the receipts passed to
// ApplyDueTables, never from the store.
func RebuildTables(block uint64) []TableRef {
	due := DueTables(block)
	tables := make([]TableRef, 0, len(due))
	for _, ref := range due {
		if ref.TableSize == 1 {
			continue
		}
		tables = append(tables, ref)
	}
	return tables
}

// RebuildWindow returns, in ascending order, the level-0 blocks whose entries
// are needed to rebuild every table due at `block`.
//
// A table's entries are the recursive merge of the level-0 entries of every
// block it covers, so its leaves are exactly its covered range. The window is
// the union of those ranges over the due tables and therefore lies inside
// [block-MaxTableLookback, block-1]; it never holds more blocks than the
// largest table covers (256). The block's own level-0 table is not included:
// ApplyDueTables builds it from the receipts of the block being processed.
func RebuildWindow(block uint64) []uint64 {
	return coveredBlocks(RebuildTables(block), block)
}

// coveredBlocks is the union of the tables' covered ranges, ascending. Only
// size>1 tables reach it, and their ranges end at least size/4 blocks before
// `block`, so `block` itself is never covered; it is filtered out defensively.
func coveredBlocks(tables []TableRef, block uint64) []uint64 {
	seen := make(map[uint64]struct{})
	for _, ref := range tables {
		for b := ref.FirstBlock; b < ref.FirstBlock+ref.TableSize; b++ {
			if b != block {
				seen[b] = struct{}{}
			}
		}
	}
	window := make([]uint64, 0, len(seen))
	for b := range seen {
		window = append(window, b)
	}
	slices.Sort(window)
	return window
}

// activeRebuildTables keeps the due tables the fork was already active for.
func activeRebuildTables(block uint64, activeAt func(uint64) bool) []TableRef {
	tables := RebuildTables(block)
	active := make([]TableRef, 0, len(tables))
	for _, ref := range tables {
		if activeAt(ref.FirstBlock) {
			active = append(active, ref)
		}
	}
	return active
}

// PruneFloor is the oldest block a table store must keep to still rebuild the
// tables due at `block`. It is the retention watermark a caller derives for
// HotTableStore.Prune: pruning at or above it drops a level-0 record a future
// due table can merge.
func PruneFloor(block uint64) uint64 {
	if block <= MaxTableLookback {
		return 0
	}
	return block - MaxTableLookback
}

// BootstrapResult reports what one bootstrap pass resolved.
type BootstrapResult struct {
	// Tables are the resolved due tables, in resolution order.
	Tables []TableResult
	// LevelZero is how many level-0 records the window needs.
	LevelZero int
	// Recorded is how many level-0 records were written to the store.
	Recorded int
	// Entries is the total entry count over the resolved tables.
	Entries int
	// Stats is the resolver delta over the whole pass, including the level-0
	// records it rebuilt to fill the window.
	Stats ResolverStats
	// Elapsed is the wall time the pass took.
	Elapsed time.Duration
}

// Bootstrap makes the tables due at `block` ready before the block is
// processed, so the first block after a sync resolves them without depending on
// which inputs this node happens to retain.
//
// With record true it first resolves every level-0 table in the window and
// writes it through the store, then resolves the due tables: each higher level
// rebuilds from the recorded level-0 records, so the receipt source is read
// once and never again for this window. With record false it resolves the due
// tables directly and leaves persistence to the caller. Either way a missing
// input returns an error wrapping ErrMissingBlockData and the pass stops: it
// never skips a table or publishes a temporary root.
//
// Tables the fork was not active for are skipped, and their level-0 blocks are
// not recorded either. Like ApplyDueTables, the caller must run this inside the
// transaction it will roll back if the block fails, so a partially recorded
// window never becomes visible.
func Bootstrap(block uint64, activeAt func(uint64) bool, resolver *Resolver, record bool) (BootstrapResult, error) {
	if activeAt == nil {
		return BootstrapResult{}, ErrNilActivationPredicate
	}
	if resolver == nil {
		return BootstrapResult{}, ErrNilTableResolver
	}
	if resolver.store == nil {
		return BootstrapResult{}, ErrNilTableStore
	}

	start := time.Now()
	before := resolver.Stats()
	var result BootstrapResult

	tables := activeRebuildTables(block, activeAt)
	window := coveredBlocks(tables, block)
	result.LevelZero = len(window)
	if record {
		for _, b := range window {
			table, err := resolver.Table(L0(b))
			if err != nil {
				return BootstrapResult{}, fmt.Errorf("bootstrap level-0 table %d: %w", b, err)
			}
			if err := resolver.store.PutTable(table); err != nil {
				return BootstrapResult{}, fmt.Errorf("record level-0 table %d: %w", b, err)
			}
			result.Recorded++
		}
	}

	for _, ref := range tables {
		table, err := resolver.Table(ref)
		if err != nil {
			return BootstrapResult{}, fmt.Errorf("bootstrap %+v: %w", ref, err)
		}
		result.Tables = append(result.Tables, table)
		result.Entries += len(table.Entries)
	}

	after := resolver.Stats()
	result.Stats = ResolverStats{
		Hits:     after.Hits - before.Hits,
		Rebuilds: after.Rebuilds - before.Rebuilds,
		Rejected: after.Rejected - before.Rejected,
	}
	if result.Stats.Rejected > 0 {
		result.Stats.LastReject = after.LastReject
	}
	result.Elapsed = time.Since(start)
	return result, nil
}
