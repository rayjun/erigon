// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package stages

import (
	"context"
	"errors"
	"testing"

	"github.com/erigontech/erigon/cl/cltypes"
	network2 "github.com/erigontech/erigon/cl/phase1/network"
	"github.com/stretchr/testify/require"
)

// currentSlot can overshoot the captured chainTipSlot; slotsRemaining must clamp
// to 0 rather than underflow into a ~2^64 slot count.
func TestForwardSyncProgress_CurrentSlotPastChainTip(t *testing.T) {
	slotsRemaining, ratePerSec := forwardSyncProgress(1_000_000, 1_000_050, 999_900, 30)
	if slotsRemaining != 0 {
		t.Fatalf("slotsRemaining = %d, want 0 when current slot is past the tip", slotsRemaining)
	}
	if ratePerSec < 0 {
		t.Fatalf("ratePerSec must not be negative, got %g", ratePerSec)
	}
}

// A reorg can drop currentSlot below prevProgress; the rate must clamp to 0
// rather than underflow the slots-processed denominator.
func TestForwardSyncProgress_ReorgBelowPrevProgress(t *testing.T) {
	slotsRemaining, ratePerSec := forwardSyncProgress(1_000_000, 900_000, 950_000, 30)
	if slotsRemaining != 100_000 {
		t.Fatalf("slotsRemaining = %d, want 100000", slotsRemaining)
	}
	if ratePerSec != 0 {
		t.Fatalf("ratePerSec = %g, want 0 when current slot is below prev progress", ratePerSec)
	}
}

// Normal case: slotsRemaining is the tip gap, rate is slots processed per second.
func TestForwardSyncProgress_Normal(t *testing.T) {
	slotsRemaining, ratePerSec := forwardSyncProgress(1_000_000, 900_000, 899_700, 30)
	if slotsRemaining != 100_000 {
		t.Fatalf("slotsRemaining = %d, want 100000", slotsRemaining)
	}
	if ratePerSec != 10 { // (900000-899700)/30
		t.Fatalf("ratePerSec = %g, want 10", ratePerSec)
	}
}

type recordingBlockCollector struct {
	flushes int
	err     error
}

func (c *recordingBlockCollector) AddBlock(*cltypes.BeaconBlock) error { return nil }
func (c *recordingBlockCollector) AddGloasBlock(*cltypes.BeaconBlock, *cltypes.SignedExecutionPayloadEnvelope) error {
	return nil
}
func (c *recordingBlockCollector) HasBlock(uint64) bool { return false }
func (c *recordingBlockCollector) Flush(context.Context) error {
	c.flushes++
	return c.err
}

func TestFlushForwardSyncBlocksCommitsAtBatchBoundary(t *testing.T) {
	collector := &recordingBlockCollector{}
	cfg := &Cfg{blockCollector: collector}

	require.NoError(t, flushForwardSyncBlocks(context.Background(), cfg))
	require.Equal(t, 1, collector.flushes)
}

func TestFlushForwardSyncBlocksPropagatesFailure(t *testing.T) {
	flushErr := errors.New("execution insert failed")
	collector := &recordingBlockCollector{err: flushErr}
	cfg := &Cfg{blockCollector: collector}

	err := flushForwardSyncBlocks(context.Background(), cfg)
	require.ErrorIs(t, err, flushErr)
	require.Equal(t, 1, collector.flushes)
}

func TestProcessForwardSyncBatchFlushesBeforeAdvancingProgress(t *testing.T) {
	collector := &recordingBlockCollector{}
	cfg := &Cfg{blockCollector: collector}

	got, err := processForwardSyncBatch(context.Background(), cfg, true, 3, func() (uint64, error) {
		return 9, nil
	})

	require.NoError(t, err)
	require.Equal(t, uint64(9), got)
	require.Equal(t, 1, collector.flushes)
}

func TestProcessForwardSyncBatchSkipsFlushWithoutInsertion(t *testing.T) {
	collector := &recordingBlockCollector{}
	cfg := &Cfg{blockCollector: collector}

	got, err := processForwardSyncBatch(context.Background(), cfg, false, 3, func() (uint64, error) {
		return 9, nil
	})

	require.NoError(t, err)
	require.Equal(t, uint64(9), got)
	require.Zero(t, collector.flushes)
}

func TestProcessForwardSyncBatchReturnsInitialProgressOnFlushFailure(t *testing.T) {
	flushErr := errors.New("execution insert failed")
	collector := &recordingBlockCollector{err: flushErr}
	cfg := &Cfg{blockCollector: collector}

	got, err := processForwardSyncBatch(context.Background(), cfg, true, 3, func() (uint64, error) {
		return 9, nil
	})

	require.ErrorIs(t, err, flushErr)
	require.ErrorIs(t, err, network2.ErrDoNotBanPeer)
	require.Equal(t, uint64(3), got)
	require.Equal(t, 1, collector.flushes)
}

func TestProcessForwardSyncBatchPreservesProcessFailure(t *testing.T) {
	processErr := errors.New("invalid block")
	collector := &recordingBlockCollector{}
	cfg := &Cfg{blockCollector: collector}

	got, err := processForwardSyncBatch(context.Background(), cfg, true, 3, func() (uint64, error) {
		return 9, processErr
	})

	require.ErrorIs(t, err, processErr)
	require.Equal(t, uint64(3), got)
	require.Equal(t, 1, collector.flushes)
}
