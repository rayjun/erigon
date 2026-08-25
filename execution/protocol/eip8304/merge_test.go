package eip8304

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types"
)

// encodedOf returns the canonical encoded hex for a list of entries.
func encodedOf(entries Entries) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Encode().String()
	}
	return out
}

func isSorted(entries Entries) bool {
	for i := 1; i < len(entries); i++ {
		if bytes.Compare(entries[i-1].Encode(), entries[i].Encode()) > 0 {
			return false
		}
	}
	return true
}

// oneTxBlock builds the sorted L0 entries for a single block containing one
// transaction with two no-topic logs.
func oneTxBlock(t *testing.T, block uint64) Entries {
	t.Helper()
	parent := common.HexToHash(fmt.Sprintf("0x%064x", block-1))
	tx := types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil)
	receipts := types.Receipts{{Logs: types.Logs{
		{Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Topics: []common.Hash{{1}}},
		{Address: common.HexToAddress("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Topics: []common.Hash{{2}, {3}}},
	}}}
	entries, err := BuildBlockEntries(block, parent, types.Transactions{tx}, receipts)
	require.NoError(t, err)
	entries.Sort()
	return entries
}

// TestMergeFourL0TablesEqualsFullRebuild verifies that merging four sorted
// single-block tables produces exactly the same entries as re-sorting the
// concatenation of those tables (a four-way merge must equal a full sort).
func TestMergeFourL0TablesEqualsFullRebuild(t *testing.T) {
	streams := make([]Entries, 0, 4)
	var all Entries
	for i := 0; i < 4; i++ {
		entries := oneTxBlock(t, 40+uint64(i))
		streams = append(streams, entries)
		all = append(all, entries...)
	}

	ref := append(Entries(nil), all...)
	ref.Sort()
	merged := MergeSorted(streams...)

	require.Equal(t, len(ref), len(merged))
	require.Equal(t, encodedOf(ref), encodedOf(merged))
	require.True(t, isSorted(merged))
}

// TestMergePreservesBlockEntryOffset checks the EIP's one-block-delay rule
// for merged tables: a table covering blocks [40..43] must contain block
// entries for blocks 39..42 (the parent entry of each indexed block).
func TestMergePreservesBlockEntryOffset(t *testing.T) {
	merged := MergeSorted(
		oneTxBlock(t, 40),
		oneTxBlock(t, 41),
		oneTxBlock(t, 42),
		oneTxBlock(t, 43),
	)

	seen := map[uint64]bool{}
	for _, e := range merged {
		if e.Type == EntryBlock {
			seen[e.block] = true
		}
	}
	require.Equal(t, map[uint64]bool{39: true, 40: true, 41: true, 42: true}, seen)
}

// TestMergePreservesDuplicates checks that identical encodings are not
// collapsed: an index table is an ordered list, not a set.
func TestMergePreservesDuplicates(t *testing.T) {
	a := Entries{NewBlockEntry(39, common.HexToHash("0x11"))}
	b := Entries{NewBlockEntry(39, common.HexToHash("0x11"))}
	merged := MergeSorted(a, b)
	require.Len(t, merged, 2)
	require.Equal(t, merged[0].Encode().String(), merged[1].Encode().String())
}

// TestMergeSortedSkipsEmptyStreams checks that missing sub-tables (for example
// an early block range where some level-0 tables are absent) simply drop out.
func TestMergeSortedSkipsEmptyStreams(t *testing.T) {
	merged := MergeSorted(nil, oneTxBlock(t, 40), nil)
	require.Len(t, merged, len(oneTxBlock(t, 40)))
	require.True(t, isSorted(merged))
}

// TestMergeSortedSingleStreamDoesNotAlias checks the result is a fresh copy.
func TestMergeSortedSingleStreamDoesNotAlias(t *testing.T) {
	src := oneTxBlock(t, 40)
	got := MergeSorted(src)
	require.Equal(t, encodedOf(src), encodedOf(got))
	got[0].block = 999
	require.NotEqual(t, uint64(999), src[0].block)
}
