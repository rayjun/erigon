package network

import (
	"testing"
	"time"

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
	require.Equal(t, forwardBlockRequestInterval, forwardBlockRequestInitialDelay(false, false))
	require.Equal(t, time.Duration(0), forwardBlockRequestInitialDelay(true, false))
}
