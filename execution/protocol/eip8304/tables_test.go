package eip8304

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

func activeAlways(uint64) bool { return true }

// testHash is a deterministic, non-zero block hash for a height.
func testHash(n uint64) common.Hash {
	var hash common.Hash
	binary.BigEndian.PutUint64(hash[hashLength-8:], n+1)
	return hash
}

// decodeIndexCall reads the (first_block, table_size, table_root) triple back
// out of index-contract calldata, so tests assert on what the contract actually
// received rather than on what the caller intended to send.
func decodeIndexCall(t *testing.T, data []byte) (TableRef, common.Hash) {
	t.Helper()
	require.Len(t, data, 96)
	return TableRef{
		FirstBlock: binary.BigEndian.Uint64(data[24:32]),
		TableSize:  binary.BigEndian.Uint64(data[56:64]),
	}, common.BytesToHash(data[64:96])
}

// testReceipts returns two single-log receipts with deterministic hashes for a
// block. They drive both the store's level-0 data and the block passed to
// ApplyDueTables, so the cached and in-flight paths must agree.
func testReceipts(block uint64) types.Receipts {
	return types.Receipts{
		{
			TxHash: testHash(block*1_000 + 1),
			Logs: types.Logs{{
				Address: common.BytesToAddress([]byte{byte(block), 1}),
				Topics:  []common.Hash{testHash(block*10 + 1)},
			}},
		},
		{
			TxHash: testHash(block*1_000 + 2),
			Logs: types.Logs{{
				Address: common.BytesToAddress([]byte{byte(block), 2}),
				Topics:  []common.Hash{testHash(block*10 + 2)},
			}},
		},
	}
}

// chainStore is an in-memory TableStore. It holds a canonical chain of blocks
// (hash plus level-0 entries), the receipts used to build those entries, and the
// precomputed table cache under test.
type chainStore struct {
	hashes   map[uint64]common.Hash
	receipts map[uint64]types.Receipts
	blocks   map[uint64]Entries
	cached   map[TableRef]TableResult
	puts     []TableRef
}

func (s *chainStore) CanonicalHash(block uint64) (common.Hash, error) {
	hash, ok := s.hashes[block]
	if !ok {
		return common.Hash{}, fmt.Errorf("%w: hash for block %d", ErrMissingBlockData, block)
	}
	return hash, nil
}

func (s *chainStore) BlockEntries(block uint64) (Entries, error) {
	entries, ok := s.blocks[block]
	if !ok {
		return nil, fmt.Errorf("%w: entries for block %d", ErrMissingBlockData, block)
	}
	return append(Entries(nil), entries...), nil
}

func (s *chainStore) GetTable(ref TableRef) (TableResult, bool) {
	result, ok := s.cached[ref]
	return result, ok
}

func (s *chainStore) PutTable(result TableResult) {
	s.cached[result.Ref] = result
	s.puts = append(s.puts, result.Ref)
}

func (s *chainStore) parentHash(block uint64) common.Hash {
	if block == 0 {
		return common.Hash{}
	}
	return s.hashes[block-1]
}

func newTestChain(t *testing.T, count int) *chainStore {
	t.Helper()
	store := &chainStore{
		hashes:   make(map[uint64]common.Hash, count),
		receipts: make(map[uint64]types.Receipts, count),
		blocks:   make(map[uint64]Entries, count),
		cached:   make(map[TableRef]TableResult),
	}
	for block := uint64(0); block < uint64(count); block++ {
		store.hashes[block] = testHash(block)
	}
	for block := uint64(0); block < uint64(count); block++ {
		receipts := testReceipts(block)
		store.receipts[block] = receipts
		entries, err := BuildBlockEntriesFromReceipts(block, store.parentHash(block), receipts)
		require.NoError(t, err)
		entries.Sort()
		store.blocks[block] = entries
	}
	return store
}

// oracleEntries rebuilds a table by concatenating the raw level-0 entries of
// every covered block and sorting once. It deliberately avoids the resolver's
// recursive four-way merge, so the resolver is checked against an independent
// construction rather than against itself.
func oracleEntries(t *testing.T, store *chainStore, ref TableRef) Entries {
	t.Helper()
	var all Entries
	for block := ref.FirstBlock; block < ref.FirstBlock+ref.TableSize; block++ {
		entries, err := store.BlockEntries(block)
		require.NoError(t, err)
		all = append(all, entries...)
	}
	all.Sort()
	return all
}

func oracleRoot(t *testing.T, store *chainStore, ref TableRef, limit uint64) common.Hash {
	t.Helper()
	root, err := RootOfEntries(oracleEntries(t, store, ref), limit)
	require.NoError(t, err)
	return root
}

// TestResolverRebuildsOnCacheMiss covers the plain fallback: with nothing
// precomputed the table is rebuilt from canonical block data, the rebuild is
// counted, and resolving alone never marks the table canonical.
func TestResolverRebuildsOnCacheMiss(t *testing.T) {
	const limit = uint64(1 << 12)
	store := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	resolver := NewResolver(store, limit)

	got, err := resolver.Table(ref)
	require.NoError(t, err)
	require.Equal(t, oracleRoot(t, store, ref, limit), got.Root)
	require.Equal(t, uint64(len(got.Entries)), got.EntryCount)
	require.Equal(t, []common.Hash{testHash(0), testHash(1), testHash(2), testHash(3)}, got.BlockHashes)
	require.True(t, isSorted(got.Entries))

	// One rebuild for the requested table, one for each level-0 child.
	stats := resolver.Stats()
	require.Equal(t, 0, stats.Hits)
	require.Equal(t, 5, stats.Rebuilds)

	_, recorded := store.GetTable(ref)
	require.False(t, recorded, "resolving a table must not persist it")
	require.Empty(t, store.puts)
}

// TestResolverUsesVerifiedPrecomputedResult covers the cache hit: a result that
// verifies against the canonical chain is used as-is, without any rebuild.
func TestResolverUsesVerifiedPrecomputedResult(t *testing.T) {
	const limit = uint64(1 << 12)
	store := newTestChain(t, 16)
	ref := TableRef{FirstBlock: 0, TableSize: 4}

	precomputed, err := NewResolver(store, limit).Table(ref)
	require.NoError(t, err)
	store.PutTable(precomputed)

	resolver := NewResolver(store, limit)
	got, err := resolver.Table(ref)
	require.NoError(t, err)
	require.Equal(t, precomputed.Root, got.Root)
	require.Equal(t, oracleRoot(t, store, ref, limit), got.Root)

	stats := resolver.Stats()
	require.Equal(t, 1, stats.Hits)
	require.Equal(t, 0, stats.Rebuilds)
	require.Equal(t, 0, stats.Rejected)
}

// TestResolverReusesVerifiedChildrenDuringNestedRebuild covers a nested cache:
// the four level-1 children of a level-2 table are already verified, so
// resolving the level-2 table rebuilds only the top level. When one child is
// unusable, only that child's branch is rebuilt, down to level 0.
func TestResolverReusesVerifiedChildrenDuringNestedRebuild(t *testing.T) {
	const limit = uint64(1 << 12)
	ref := TableRef{FirstBlock: 0, TableSize: 16}

	seed := func(t *testing.T, store *chainStore) []TableRef {
		t.Helper()
		children := MergeGroup(ref.FirstBlock, ref.TableSize)
		resolver := NewResolver(store, limit)
		for _, child := range children {
			childResult, err := resolver.Table(child)
			require.NoError(t, err)
			store.PutTable(childResult)
		}
		return children
	}

	t.Run("all children verified", func(t *testing.T) {
		store := newTestChain(t, 32)
		seed(t, store)

		resolver := NewResolver(store, limit)
		got, err := resolver.Table(ref)
		require.NoError(t, err)
		require.Equal(t, oracleRoot(t, store, ref, limit), got.Root)

		stats := resolver.Stats()
		require.Equal(t, 4, stats.Hits, "each verified child must be reused")
		require.Equal(t, 1, stats.Rebuilds, "only the requested table is rebuilt")
		require.Equal(t, 0, stats.Rejected)
	})

	t.Run("one corrupted branch", func(t *testing.T) {
		store := newTestChain(t, 32)
		children := seed(t, store)

		bad := store.cached[children[1]]
		bad.Root = common.HexToHash("0xbad")
		store.cached[children[1]] = bad

		resolver := NewResolver(store, limit)
		got, err := resolver.Table(ref)
		require.NoError(t, err)
		require.Equal(t, oracleRoot(t, store, ref, limit), got.Root)

		stats := resolver.Stats()
		require.Equal(t, 3, stats.Hits, "the other three children must still be reused")
		require.Equal(t, 1, stats.Rejected)
		require.ErrorIs(t, stats.LastReject, ErrTableRootMismatch)
		// The top level, the rejected child, and that child's four level-0 tables.
		require.Equal(t, 6, stats.Rebuilds)
	})
}

// TestResolverRebuildsRejectedResults covers every way a precomputed result can
// be unusable — corruption, truncation, a stale old-chain binding — and checks
// that each one is rejected with a typed reason and replaced by the canonical
// root rather than used or skipped.
func TestResolverRebuildsRejectedResults(t *testing.T) {
	const limit = uint64(1 << 12)
	ref := TableRef{FirstBlock: 0, TableSize: 4}

	cases := []struct {
		name    string
		corrupt func(TableResult) TableResult
		wantErr error
	}{
		{"root does not match entries", func(r TableResult) TableResult {
			r.Root = common.HexToHash("0xdead")
			return r
		}, ErrTableRootMismatch},
		{"entries out of order", func(r TableResult) TableResult {
			slices.Reverse(r.Entries)
			return r
		}, ErrTableUnsortedEntries},
		{"entry count mismatch", func(r TableResult) TableResult {
			r.EntryCount++
			return r
		}, ErrTableEntryCount},
		{"bound to an old chain", func(r TableResult) TableResult {
			r.BlockHashes[0] = common.HexToHash("0xfeed")
			return r
		}, ErrTableNotCanonical},
		{"coverage truncated", func(r TableResult) TableResult {
			r.BlockHashes = r.BlockHashes[:1]
			return r
		}, ErrTableCoverageMismatch},
		{"different table", func(r TableResult) TableResult {
			r.Ref = TableRef{FirstBlock: 4, TableSize: 4}
			return r
		}, ErrTableRefMismatch},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := newTestChain(t, 16)
			stored, err := NewResolver(store, limit).Table(ref)
			require.NoError(t, err)
			store.cached[ref] = testCase.corrupt(stored)

			resolver := NewResolver(store, limit)
			got, err := resolver.Table(ref)
			require.NoError(t, err)
			require.Equal(t, oracleRoot(t, store, ref, limit), got.Root)

			stats := resolver.Stats()
			require.Equal(t, 1, stats.Rejected)
			require.GreaterOrEqual(t, stats.Rebuilds, 1)
			require.ErrorIs(t, stats.LastReject, testCase.wantErr)
		})
	}
}

// TestResolverFailsClosedOnMissingBlockData covers the fail-closed rule: a
// rebuild that cannot obtain a covered block's data returns an error instead of
// producing a partial root or silently skipping the table.
func TestResolverFailsClosedOnMissingBlockData(t *testing.T) {
	store := newTestChain(t, 8)
	delete(store.blocks, 2)

	_, err := NewResolver(store, 1<<8).Table(TableRef{FirstBlock: 0, TableSize: 4})
	require.ErrorIs(t, err, ErrMissingBlockData)
	require.ErrorContains(t, err, "block 2")
}

// TestApplyDueTablesWritesDelayedRoots runs a chain end to end and checks the
// full write sequence against independently computed expectations: L0 at every
// block, each higher level exactly at its delayed write block, and every root
// equal to a from-scratch rebuild of the covered range.
func TestApplyDueTablesWritesDelayedRoots(t *testing.T) {
	const (
		// 322 blocks reach the first level-4 (256-block) write at block 319, the
		// EIP's worst-case 319-block rebuild window.
		blocks = 322
		limit  = uint64(1 << 12)
	)
	store := newTestChain(t, blocks)
	resolver := NewResolver(store, limit)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x0000000000000000000000000000000000000001"))

	var got []TableRef
	var roots []common.Hash
	syscall := func(addr accounts.Address, data []byte) ([]byte, error) {
		require.Equal(t, indexAddr, addr)
		ref, root := decodeIndexCall(t, data)
		got = append(got, ref)
		roots = append(roots, root)
		return nil, nil
	}

	for block := uint64(0); block < blocks; block++ {
		applied, err := ApplyDueTables(block, testHash(block), store.parentHash(block), store.receipts[block], indexAddr, activeAlways, resolver, syscall)
		require.NoError(t, err)
		require.NotEmpty(t, applied)
	}

	// Independent schedule: a table (first_block, table_size) is written at
	// first_block + table_size - 1 + table_size/4.
	var want []TableRef
	for block := uint64(0); block < blocks; block++ {
		want = append(want, L0(block))
		if block >= 4 && block%4 == 0 {
			want = append(want, TableRef{FirstBlock: block - 4, TableSize: 4})
		}
		if block >= 19 && (block-19)%16 == 0 {
			want = append(want, TableRef{FirstBlock: block - 19, TableSize: 16})
		}
		if block >= 79 && (block-79)%64 == 0 {
			want = append(want, TableRef{FirstBlock: block - 79, TableSize: 64})
		}
		if block >= 319 && (block-319)%256 == 0 {
			want = append(want, TableRef{FirstBlock: block - 319, TableSize: 256})
		}
	}
	require.Equal(t, want, got)
	require.Equal(t, want, store.puts)

	for i, ref := range got {
		require.Equal(t, oracleRoot(t, store, ref, limit), roots[i], "root for %+v", ref)
	}

	// The (0,4) table is delayed: it is written at block 4, immediately after
	// that block's own level-0 table, never at blocks 1..3.
	require.Equal(t, L0(4), got[4])
	require.Equal(t, TableRef{FirstBlock: 0, TableSize: 4}, got[5])

	// All five levels fire: the highest-level (0,256) table is only due at
	// block 319, and it reuses the four verified (x,64) tables written earlier.
	require.Contains(t, got, TableRef{FirstBlock: 0, TableSize: 256})
	require.Greater(t, resolver.Stats().Hits, 0)
}

// TestApplyDueTablesSkipsTablesInactiveAtFirstBlock covers the EIP rule that a
// table is only generated if the fork was already active at its first block.
func TestApplyDueTablesSkipsTablesInactiveAtFirstBlock(t *testing.T) {
	const (
		blocks     = 40
		activeFrom = uint64(10)
		limit      = uint64(1 << 12)
	)
	store := newTestChain(t, blocks)
	resolver := NewResolver(store, limit)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x0000000000000000000000000000000000000002"))
	activeAt := func(firstBlock uint64) bool { return firstBlock >= activeFrom }

	var got []TableRef
	syscall := func(addr accounts.Address, data []byte) ([]byte, error) {
		ref, _ := decodeIndexCall(t, data)
		got = append(got, ref)
		return nil, nil
	}

	for block := uint64(0); block < blocks; block++ {
		applied, err := ApplyDueTables(block, testHash(block), store.parentHash(block), store.receipts[block], indexAddr, activeAt, resolver, syscall)
		require.NoError(t, err)
		if activeAt(block) {
			require.NotEmpty(t, applied)
		} else {
			require.Empty(t, applied)
		}
	}

	for _, ref := range got {
		require.GreaterOrEqual(t, ref.FirstBlock, activeFrom, "table %+v generated before activation", ref)
	}
	require.Contains(t, got, L0(10))
	require.NotContains(t, got, L0(9))
	// A (8,4) table is due at block 12 but its first block predates activation.
	require.NotContains(t, got, TableRef{FirstBlock: 8, TableSize: 4})
	require.Contains(t, got, TableRef{FirstBlock: 12, TableSize: 4})
	require.Contains(t, got, TableRef{FirstBlock: 16, TableSize: 16})
}

// TestApplyDueTablesDoesNotRecordFailedTable covers the write-ordering rule: a
// higher-level table is recorded as canonical only after its system call
// succeeds, so a reverted call leaves it absent from the store.
func TestApplyDueTablesDoesNotRecordFailedTable(t *testing.T) {
	const limit = uint64(1 << 12)
	store := newTestChain(t, 8)
	resolver := NewResolver(store, limit)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x0000000000000000000000000000000000000003"))
	boom := &testSyscallError{}

	syscall := func(addr accounts.Address, data []byte) ([]byte, error) {
		if ref, _ := decodeIndexCall(t, data); ref.TableSize > 1 {
			return nil, boom
		}
		return nil, nil
	}

	for block := uint64(0); block < 4; block++ {
		applied, err := ApplyDueTables(block, testHash(block), store.parentHash(block), store.receipts[block], indexAddr, activeAlways, resolver, syscall)
		require.NoError(t, err)
		require.Equal(t, []TableRef{L0(block)}, applied)
	}

	applied, err := ApplyDueTables(4, testHash(4), store.parentHash(4), store.receipts[4], indexAddr, activeAlways, resolver, syscall)
	require.ErrorIs(t, err, boom)
	require.Equal(t, []TableRef{L0(4)}, applied, "only the L0 write succeeded before the failure")

	_, recorded := store.GetTable(TableRef{FirstBlock: 0, TableSize: 4})
	require.False(t, recorded, "a table whose system call failed must not be marked canonical")
	require.Equal(t, []TableRef{L0(0), L0(1), L0(2), L0(3), L0(4)}, store.puts)
}

// TestApplyDueTablesListLimitRejected covers the abort path: an L0 table that
// exceeds the SSZ limit fails before any system call or store write.
func TestApplyDueTablesListLimitRejected(t *testing.T) {
	store := newTestChain(t, 4)
	resolver := NewResolver(store, 1)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x0000000000000000000000000000000000000004"))

	called := false
	syscall := func(addr accounts.Address, data []byte) ([]byte, error) {
		called = true
		return nil, nil
	}

	applied, err := ApplyDueTables(3, testHash(3), store.parentHash(3), store.receipts[3], indexAddr, activeAlways, resolver, syscall)
	require.ErrorIs(t, err, ErrExceedsListLimit)
	require.Empty(t, applied)
	require.False(t, called)
	require.Empty(t, store.puts)
}

// TestApplyDueTablesRejectsNilInputs pins the explicit-input contract: there is
// no silent default activation or resolver.
func TestApplyDueTablesRejectsNilInputs(t *testing.T) {
	store := newTestChain(t, 2)
	resolver := NewResolver(store, 1<<8)
	syscall := func(addr accounts.Address, data []byte) ([]byte, error) { return nil, nil }

	_, err := ApplyDueTables(0, testHash(0), common.Hash{}, nil, accounts.Address{}, nil, resolver, syscall)
	require.ErrorIs(t, err, ErrNilActivationPredicate)

	_, err = ApplyDueTables(0, testHash(0), common.Hash{}, nil, accounts.Address{}, activeAlways, nil, syscall)
	require.ErrorIs(t, err, ErrNilTableResolver)

	_, err = ApplyDueTables(0, testHash(0), common.Hash{}, nil, accounts.Address{}, activeAlways, NewResolver(nil, 1), syscall)
	require.ErrorIs(t, err, ErrNilTableStore)
}

// TestVerifyTableRejectsStoredEntriesThatDisagreeWithTheirRoot is the direct
// unit check behind the cache-hit path: a stored root that its own entries do
// not produce is never accepted.
func TestVerifyTableRejectsStoredEntriesThatDisagreeWithTheirRoot(t *testing.T) {
	const limit = uint64(1 << 12)
	store := newTestChain(t, 8)
	ref := TableRef{FirstBlock: 0, TableSize: 4}

	result, err := NewResolver(store, limit).Table(ref)
	require.NoError(t, err)
	require.NoError(t, VerifyTable(result, ref, store, limit))

	tampered := result
	tampered.Root = common.HexToHash("0xbeef")
	require.ErrorIs(t, VerifyTable(tampered, ref, store, limit), ErrTableRootMismatch)
}
