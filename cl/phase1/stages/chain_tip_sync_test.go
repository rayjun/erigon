package stages

import (
	"testing"
	"time"

	"github.com/erigontech/erigon/cl/cltypes"
	"github.com/stretchr/testify/require"
)

func TestMissedBlockRequestCountIsRateLimitSafe(t *testing.T) {
	tests := []struct {
		name        string
		currentSlot uint64
		from        uint64
		want        uint64
	}{
		{name: "current slot behind start", currentSlot: 3, from: 10, want: 4},
		{name: "at start", currentSlot: 10, from: 10, want: 4},
		{name: "short gap", currentSlot: 13, from: 10, want: 7},
		{name: "cap reached", currentSlot: 14, from: 10, want: 8},
		{name: "long catch-up", currentSlot: 100, from: 10, want: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, missedBlockRequestCount(tt.currentSlot, tt.from))
			require.LessOrEqual(t, missedBlockRequestCount(tt.currentSlot, tt.from), uint64(maxMissedBlocksPerRequest))
		})
	}
}

func TestChainTipBackfillIsPacedBelowPeerRefillRate(t *testing.T) {
	const remoteRefillPerSecond = 12.8
	tokensPerSecond := float64(maxMissedBlocksPerRequest) / chainTipBlockRequestInterval.Seconds()
	require.Less(t, tokensPerSecond, remoteRefillPerSecond)
	require.Positive(t, time.Duration(chainTipBlockRequestInterval))
}

func TestMissedBlockRangeSlidesPastEmptyWindow(t *testing.T) {
	const (
		highestSeen = uint64(10)
		currentSlot = uint64(30)
	)

	cursor := uint64(0)
	from, count := nextMissedBlockRange(highestSeen, currentSlot, cursor)
	require.Equal(t, uint64(8), from)
	require.Equal(t, uint64(maxMissedBlocksPerRequest), count)
	cursor = advanceMissedBlockRequestCursor(from, count, nil)

	from, count = nextMissedBlockRange(highestSeen, currentSlot, cursor)
	require.Equal(t, uint64(16), from)
	require.Equal(t, uint64(maxMissedBlocksPerRequest), count)
	cursor = advanceMissedBlockRequestCursor(from, count, nil)

	from, _ = nextMissedBlockRange(highestSeen, currentSlot, cursor)
	require.Equal(t, uint64(24), from)
}

func TestMissedBlockRangeResetsToNewHighestSeen(t *testing.T) {
	from, count := nextMissedBlockRange(100, 120, 10)
	require.Equal(t, uint64(98), from)
	require.Equal(t, uint64(8), count)
}

func TestAdvanceMissedBlockRequestCursorAfterBlocks(t *testing.T) {
	blocks := []*cltypes.SignedBeaconBlock{
		{Block: &cltypes.BeaconBlock{Slot: 20}},
		{Block: &cltypes.BeaconBlock{Slot: 24}},
	}
	require.Equal(t, uint64(25), advanceMissedBlockRequestCursor(18, 8, blocks))
}
