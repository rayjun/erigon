package eip8304

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteBlock(t *testing.T) {
	cases := []struct {
		first, size, want uint64
	}{
		{0, 1, 0},     // L0 written immediately
		{5, 1, 5},     // L0 written immediately
		{0, 4, 4},     // blocks 0..3 written after block 4
		{4, 4, 8},     // blocks 4..7 written after block 8
		{0, 16, 19},   // blocks 0..15 written after block 19
		{0, 64, 79},   // blocks 0..63 written after block 79
		{0, 256, 319}, // blocks 0..255 written after block 319
	}
	for _, c := range cases {
		require.Equal(t, c.want, WriteBlock(c.first, c.size), "first=%d size=%d", c.first, c.size)
	}
}

func TestDueTables(t *testing.T) {
	// block 4: L0 (fb=4) and the first L1 table (fb=0, blocks 0..3).
	require.Equal(t, []TableRef{{4, 1}, {0, 4}}, DueTables(4))

	// block 8: L0 (fb=8) and L1 (fb=4).
	require.Equal(t, []TableRef{{8, 1}, {4, 4}}, DueTables(8))

	// block 19: L0 (fb=19) and the first L2 table (fb=0, blocks 0..15).
	require.Equal(t, []TableRef{{19, 1}, {0, 16}}, DueTables(19))

	// block 319: L0 (fb=319) and the first L4 table (fb=0, blocks 0..255).
	require.Equal(t, []TableRef{{319, 1}, {0, 256}}, DueTables(319))
}

func TestDueTablesEmptyBeforeCoverage(t *testing.T) {
	// Before tables wrap around, no spurious high-level tables are due.
	require.Equal(t, []TableRef{{0, 1}}, DueTables(0))
	require.Equal(t, []TableRef{{1, 1}}, DueTables(1))
	require.Equal(t, []TableRef{{2, 1}}, DueTables(2))
	require.Equal(t, []TableRef{{3, 1}}, DueTables(3))
}

func TestL0(t *testing.T) {
	require.Equal(t, TableRef{FirstBlock: 42, TableSize: 1}, L0(42))
}

func TestMergeGroup(t *testing.T) {
	require.Equal(t, []TableRef{
		{0, 1}, {1, 1}, {2, 1}, {3, 1},
	}, MergeGroup(0, 4))

	require.Equal(t, []TableRef{
		{0, 64}, {64, 64}, {128, 64}, {192, 64},
	}, MergeGroup(0, 256))
}

func TestTableSizesAreGeometricWithRatioFour(t *testing.T) {
	require.Equal(t, [5]uint64{1, 4, 16, 64, 256}, TableSizes)
	for i := 1; i < len(TableSizes); i++ {
		require.Equal(t, TableSizes[i-1]*4, TableSizes[i])
	}
}

// TestDueTablesRoundTrip checks the schedule invariant: the table written at
// WriteBlock(fb, size) is exactly the one DueTables reports at that block.
func TestDueTablesRoundTrip(t *testing.T) {
	for _, size := range TableSizes {
		for fb := uint64(0); fb < 4096; fb += size {
			writeAt := WriteBlock(fb, size)
			require.Contains(t, DueTables(writeAt), TableRef{fb, size}, "fb=%d size=%d", fb, size)
		}
	}
}

func TestWriteBlockRejectsInvalidSize(t *testing.T) {
	require.Panics(t, func() { WriteBlock(0, 2) })
	require.Panics(t, func() { WriteBlock(0, 0) })
}

func TestMergeGroupRejectsNonMergeableSize(t *testing.T) {
	require.Panics(t, func() { MergeGroup(0, 1) })
	require.Panics(t, func() { MergeGroup(0, 2) })
}
