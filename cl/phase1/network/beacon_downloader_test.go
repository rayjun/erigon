package network

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erigontech/erigon/cl/cltypes"
	"github.com/stretchr/testify/require"
)

func TestForwardBlockRequestsStayBelowPeerRefillRate(t *testing.T) {
	// A batch requests one lookahead block in addition to the configured count.
	// Lighthouse-compatible peers refill blocks-by-range at 12.8 tokens/second.
	const remoteRefillPerSecond = 12.8

	tokensPerSecond := float64(forwardBlockRequestCount+1) / forwardBlockRequestInterval.Seconds()
	require.Less(t, tokensPerSecond, remoteRefillPerSecond)
	require.Less(t, forwardBlockRequestInterval, forwardBlockRequestTimeout)
	require.Positive(t, time.Duration(forwardBlockRequestInterval))
}

func TestForwardBlockRequestBackoff(t *testing.T) {
	require.Equal(t, time.Duration(0), forwardBlockRequestBackoff(0))
	require.Equal(t, time.Second, forwardBlockRequestBackoff(1))
	require.Equal(t, 2*time.Second, forwardBlockRequestBackoff(2))
	require.Equal(t, 4*time.Second, forwardBlockRequestBackoff(3))
	require.Equal(t, 5*time.Second, forwardBlockRequestBackoff(4))
	require.Equal(t, 5*time.Second, forwardBlockRequestBackoff(20))
}

func TestForwardBlockRequestInitialDelayAfterHTTPFallback(t *testing.T) {
	require.Equal(t, forwardBlockRequestInterval, forwardBlockRequestInitialDelay(false, false, forwardBlockRequestInterval))
	require.Equal(t, time.Duration(0), forwardBlockRequestInitialDelay(true, false, forwardBlockRequestInterval))
}

type blockingForwardBlocksRPC struct {
	active    atomic.Int32
	maxActive atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	callCount atomic.Int32
}

func (f *blockingForwardBlocksRPC) SendBeaconBlocksByRangeReq(ctx context.Context, _, _ uint64) ([]*cltypes.SignedBeaconBlock, string, error) {
	f.callCount.Add(1)
	active := f.active.Add(1)
	for {
		maxActive := f.maxActive.Load()
		if active <= maxActive || f.maxActive.CompareAndSwap(maxActive, active) {
			break
		}
	}
	f.entered <- struct{}{}
	select {
	case <-f.release:
	case <-ctx.Done():
	}
	f.active.Add(-1)
	return nil, "", nil
}

func TestForwardBlockRequestsNeverOverlapWithConcurrentCallers(t *testing.T) {
	fakeRPC := &blockingForwardBlocksRPC{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}
	downloader := &ForwardBeaconDownloader{
		sendBlocksByRange: fakeRPC.SendBeaconBlocksByRangeReq,
		requestInterval:   10 * time.Millisecond,
		requestTimeout:    time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		downloader.RequestMore(ctx)
	}()
	go func() {
		defer wg.Done()
		downloader.RequestMore(ctx)
	}()

	require.Eventually(t, func() bool { return fakeRPC.callCount.Load() == 1 }, time.Second, time.Millisecond)
	select {
	case <-fakeRPC.entered:
	case <-ctx.Done():
		t.Fatal("first blocks-by-range request did not start")
	}

	select {
	case <-fakeRPC.entered:
		t.Fatal("second blocks-by-range request overlapped the first")
	case <-time.After(50 * time.Millisecond):
	}

	fakeRPC.release <- struct{}{}
	select {
	case <-fakeRPC.entered:
	case <-ctx.Done():
		t.Fatal("second blocks-by-range request did not start after the first completed")
	}
	fakeRPC.release <- struct{}{}
	wg.Wait()

	require.Equal(t, int32(2), fakeRPC.callCount.Load())
	require.Equal(t, int32(1), fakeRPC.maxActive.Load())
}

func TestForwardBlockRequestWaiterHonorsContextCancellation(t *testing.T) {
	fakeRPC := &blockingForwardBlocksRPC{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}
	downloader := &ForwardBeaconDownloader{
		sendBlocksByRange: fakeRPC.SendBeaconBlocksByRangeReq,
		requestInterval:   10 * time.Millisecond,
		requestTimeout:    time.Second,
	}
	holderCtx, holderCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer holderCancel()
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		downloader.RequestMore(holderCtx)
	}()

	select {
	case <-fakeRPC.entered:
	case <-holderCtx.Done():
		t.Fatal("holder request did not start")
	}

	waiterCtx, waiterCancel := context.WithCancel(context.Background())
	waiterCancel()
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		downloader.RequestMore(waiterCtx)
	}()

	select {
	case <-waiterDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cancelled waiter blocked on the request gate")
	}

	fakeRPC.release <- struct{}{}
	select {
	case <-holderDone:
	case <-holderCtx.Done():
		t.Fatal("holder request did not finish")
	}
	require.Equal(t, int32(1), fakeRPC.callCount.Load())
}
