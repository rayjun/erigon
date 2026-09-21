package eip8304

import (
	"errors"
	"fmt"

	"github.com/erigontech/erigon/common"
)

// A precomputed table result is used only while it verifies. Any of these
// failures sends the resolver down the deterministic synchronous rebuild path;
// none of them lets the caller skip the table or publish a temporary root.
var (
	ErrInvalidTableRef       = errors.New("eip8304: invalid table reference")
	ErrTableRefMismatch      = errors.New("eip8304: table result is for a different table")
	ErrTableEntryCount       = errors.New("eip8304: table result entry count does not match its entries")
	ErrTableCoverageMismatch = errors.New("eip8304: table result does not cover the table's block range")
	ErrTableNotCanonical     = errors.New("eip8304: table result is not bound to the canonical chain")
	ErrTableUnsortedEntries  = errors.New("eip8304: table result entries are not in canonical order")
	ErrTableRootMismatch     = errors.New("eip8304: table result root does not match its entries")
)

// ErrMissingBlockData reports that a rebuild could not obtain the canonical
// block data it needs. TableStore implementations must wrap it (via %w) from
// BlockEntries and CanonicalHash so callers can tell "data unavailable" apart
// from a programming error: a rebuild never substitutes an empty table, it
// fails closed.
var ErrMissingBlockData = errors.New("eip8304: canonical block data unavailable")

// Programming errors: inputs the finalize path must pass explicitly, so none of
// them can fall back to a silent default.
var (
	ErrNilTableStore          = errors.New("eip8304: nil table store")
	ErrNilTableResolver       = errors.New("eip8304: nil table resolver")
	ErrNilActivationPredicate = errors.New("eip8304: nil activation predicate")
)

// TableResult is one computed index table: its identity, its canonical ordered
// entries, the SSZ root of those entries, and the canonical block hashes it is
// bound to.
//
// BlockHashes covers the table's block range [FirstBlock, FirstBlock+TableSize-1].
// Any reorg that could make the table stale changes a hash in this range: the
// entries commit to block FirstBlock-1's hash, and FirstBlock commits to that
// same parent. A level-0 table therefore carries exactly its own block hash.
type TableResult struct {
	Ref         TableRef
	EntryCount  uint64
	Entries     Entries
	Root        common.Hash
	BlockHashes []common.Hash
}

// CanonicalChain supplies the canonical block hash at a height.
type CanonicalChain interface {
	CanonicalHash(block uint64) (common.Hash, error)
}

// CanonicalData is the canonical chain data a deterministic rebuild needs.
//
// The contract is deliberately strict, because a rebuild must never depend on
// data that is only good enough to produce "some" root:
//   - BlockEntries returns the canonical, lexicographically sorted level-0
//     entries for a block, or an error wrapping ErrMissingBlockData when that
//     block's data is unavailable. Missing data must be reported, never
//     replaced by an empty table. CanonicalChain follows the same rule for a
//     height it cannot resolve.
type CanonicalData interface {
	CanonicalChain
	BlockEntries(block uint64) (Entries, error)
}

// TableStore is the cache a resolver reads precomputed tables from, plus the
// canonical inputs a rebuild needs.
//
// Records are keyed by the table reference and bound to the canonical chain:
//   - GetTable reports ok=false only for a genuine cache miss. A row that
//     exists but cannot be trusted — undecodable, partially written, written by
//     an unknown record version, or covering blocks this node has not executed
//     — returns an error so the resolver rejects and rebuilds it instead of
//     using it. Rejection never aborts the block.
//   - PutTable records a table as canonical, atomically or not at all. The
//     finalize path calls it only after the table's system call succeeded,
//     never before, and treats an error as aborting the block. It rejects a
//     result whose entries do not belong to the table's block range, so a
//     writer bug fails the block instead of leaving a record the read path
//     would serve as this table's root.
type TableStore interface {
	CanonicalData
	GetTable(ref TableRef) (TableResult, bool, error)
	PutTable(result TableResult) error
}

// RootOfEntries computes the SSZ root of the table formed by `entries`, which
// must already be in canonical order. `limit` is the SSZ List[Hash32, N] bound;
// it stays an explicit parameter while the EIP bound is unresolved.
func RootOfEntries(entries Entries, limit uint64) (common.Hash, error) {
	leaves := make([]common.Hash, len(entries))
	for i, entry := range entries {
		leaves[i] = entry.Encode().LeafHash()
	}
	return ListHashRoot(leaves, limit)
}

// VerifyTable reports whether a precomputed result may be used for ref on the
// current canonical chain. It checks identity and the canonical block-hash
// binding of the covered range, then that the result is consistent with itself
// (see checkResultShape).
//
// It deliberately does not recompute the canonical entries: doing so is the
// rebuild the cache exists to avoid. What the checks establish is that the
// record describes this table's range on this chain, that its entries belong to
// that range, and that its root is the root of those entries. A record that
// satisfies all of that but holds different (self-consistent) entry content was
// not written by this node's own computation and is trusted to the record
// checksum, not to verification.
func VerifyTable(result TableResult, ref TableRef, chain CanonicalChain, limit uint64) error {
	if result.Ref != ref {
		return fmt.Errorf("%w: have %+v, want %+v", ErrTableRefMismatch, result.Ref, ref)
	}
	if uint64(len(result.BlockHashes)) != ref.TableSize {
		return fmt.Errorf("%w: %d hashes for table_size %d", ErrTableCoverageMismatch, len(result.BlockHashes), ref.TableSize)
	}
	for i, hash := range result.BlockHashes {
		block := ref.FirstBlock + uint64(i)
		canonical, err := chain.CanonicalHash(block)
		if err != nil {
			return fmt.Errorf("canonical hash for block %d: %w", block, err)
		}
		if canonical != hash {
			return fmt.Errorf("%w: block %d", ErrTableNotCanonical, block)
		}
	}
	return checkResultShape(result, ref, limit)
}

// checkResultShape reports whether a result agrees with itself and with the
// table it claims to be, without consulting the canonical chain: entry count,
// canonical entry order, entries inside the covered range's block window, and
// the root derived from those entries. Both the read path (VerifyTable) and the
// restart check (Reconcile) use it, so a record cannot be acceptable to one and
// not the other.
func checkResultShape(result TableResult, ref TableRef, limit uint64) error {
	if result.EntryCount != uint64(len(result.Entries)) {
		return fmt.Errorf("%w: entry_count %d, %d entries", ErrTableEntryCount, result.EntryCount, len(result.Entries))
	}
	if !entriesSorted(result.Entries) {
		return ErrTableUnsortedEntries
	}
	if err := checkEntryBlocks(result.Entries, ref); err != nil {
		return err
	}
	root, err := RootOfEntries(result.Entries, limit)
	if err != nil {
		return err
	}
	if root != result.Root {
		return fmt.Errorf("%w: have %x, want %x", ErrTableRootMismatch, result.Root, root)
	}
	return nil
}

// checkEntryBlocks reports whether every entry can belong to a table covering
// ref. Entries carry the block they were built for, and a table that does not
// start at genesis also carries one parent-hash entry for block FirstBlock-1
// (see buildBlockEntriesFromHashes), so the window is
// [FirstBlock-1, FirstBlock+TableSize-1] (clamped at block 0). A record whose
// entries come from another block range fails here even when its root matches
// those entries, which is what stops the resolver from serving a table built
// for different blocks.
func checkEntryBlocks(entries Entries, ref TableRef) error {
	low := ref.FirstBlock
	if low > 0 {
		low--
	}
	high := ref.FirstBlock + ref.TableSize - 1
	for _, entry := range entries {
		if entry.block < low || entry.block > high {
			return fmt.Errorf("%w: entry for block %d outside %+v", ErrTableCoverageMismatch, entry.block, ref)
		}
	}
	return nil
}

func entriesSorted(entries Entries) bool {
	return checkedSortedKeys(entries) == nil
}

func validateTableRef(ref TableRef) error {
	if !validTableSize(ref.TableSize) {
		return fmt.Errorf("%w: table_size %d", ErrInvalidTableRef, ref.TableSize)
	}
	if ref.TableSize > 1 && ref.FirstBlock%ref.TableSize != 0 {
		return fmt.Errorf("%w: first_block %d is not a multiple of table_size %d", ErrInvalidTableRef, ref.FirstBlock, ref.TableSize)
	}
	return nil
}

// ResolverStats reports how a resolver satisfied its table requests. It exists
// so tests and metrics can prove that a verified precomputed result was really
// used, and that a rejected result was really rebuilt.
type ResolverStats struct {
	Hits       int
	Rebuilds   int
	Rejected   int
	LastReject error
}

// Resolver resolves due tables from verified precomputed results, falling back
// to a deterministic synchronous rebuild whenever a result is missing, corrupt
// or no longer bound to the canonical chain. It never returns a stale root —
// a precomputed result is used only while it verifies against the canonical
// chain and the range it claims (see VerifyTable) — and it never persists a
// result: the caller marks a table canonical only after the table's system call
// succeeded.
//
// A Resolver is not safe for concurrent use; the finalize path is synchronous.
type Resolver struct {
	store     TableStore
	listLimit uint64

	hits       int
	rebuilds   int
	rejected   int
	lastReject error
}

// NewResolver returns a resolver reading precomputed tables from store and
// computing roots under the given SSZ list bound.
func NewResolver(store TableStore, listLimit uint64) *Resolver {
	return &Resolver{store: store, listLimit: listLimit}
}

// Stats returns a snapshot of the resolver's cache statistics.
func (r *Resolver) Stats() ResolverStats {
	return ResolverStats{
		Hits:       r.hits,
		Rebuilds:   r.rebuilds,
		Rejected:   r.rejected,
		LastReject: r.lastReject,
	}
}

// Table returns a verified table for ref, rebuilding it synchronously when no
// usable precomputed result exists. A precomputed result is used only if it
// passes VerifyTable; otherwise it is discarded and rebuilt from canonical
// block data, recursively at every level.
func (r *Resolver) Table(ref TableRef) (TableResult, error) {
	if r == nil {
		return TableResult{}, ErrNilTableResolver
	}
	if r.store == nil {
		return TableResult{}, ErrNilTableStore
	}
	if err := validateTableRef(ref); err != nil {
		return TableResult{}, err
	}

	stored, ok, err := r.store.GetTable(ref)
	switch {
	case err != nil:
		// A row that cannot be read back is a rejected result, not a failure of
		// the block: the rebuild below produces the canonical answer instead.
		r.rejected++
		r.lastReject = err
	case ok:
		if err := VerifyTable(stored, ref, r.store, r.listLimit); err == nil {
			r.hits++
			return stored, nil
		} else {
			r.rejected++
			r.lastReject = err
		}
	}

	rebuilt, err := r.rebuild(ref)
	if err != nil {
		return TableResult{}, err
	}
	r.rebuilds++
	return rebuilt, nil
}

// rebuild deterministically recomputes ref from canonical data: a level-0 table
// from its block's entries, a higher-level table by merging its four children
// (each itself resolved through Table, so a verified precomputed child is still
// reused). A missing input is an error, never an empty table.
func (r *Resolver) rebuild(ref TableRef) (TableResult, error) {
	var entries Entries
	if ref.TableSize == 1 {
		blockEntries, err := r.store.BlockEntries(ref.FirstBlock)
		if err != nil {
			return TableResult{}, fmt.Errorf("level-0 entries for block %d: %w", ref.FirstBlock, err)
		}
		entries = append(Entries(nil), blockEntries...)
		// Level-0 entries are built in execution order; canonicalize here so a
		// rebuild never depends on the order a store happens to return.
		entries.Sort()
	} else {
		group := MergeGroup(ref.FirstBlock, ref.TableSize)
		streams := make([]Entries, 0, len(group))
		for _, child := range group {
			result, err := r.Table(child)
			if err != nil {
				return TableResult{}, fmt.Errorf("rebuild %+v from %+v: %w", ref, child, err)
			}
			streams = append(streams, result.Entries)
		}
		merged, err := MergeSorted(streams...)
		if err != nil {
			return TableResult{}, fmt.Errorf("rebuild %+v: %w", ref, err)
		}
		entries = merged
	}

	root, err := RootOfEntries(entries, r.listLimit)
	if err != nil {
		return TableResult{}, fmt.Errorf("rebuild %+v: %w", ref, err)
	}

	hashes := make([]common.Hash, ref.TableSize)
	for i := range hashes {
		block := ref.FirstBlock + uint64(i)
		hash, err := r.store.CanonicalHash(block)
		if err != nil {
			return TableResult{}, fmt.Errorf("rebuild %+v: canonical hash for block %d: %w", ref, block, err)
		}
		hashes[i] = hash
	}

	return TableResult{
		Ref:         ref,
		EntryCount:  uint64(len(entries)),
		Entries:     entries,
		Root:        root,
		BlockHashes: hashes,
	}, nil
}
