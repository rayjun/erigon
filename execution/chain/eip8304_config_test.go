package chain_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/execution/chain"
)

// TestConfigEip8304ForkActivation covers the experimental fork scheduling:
// nil means not scheduled, 0 means active from genesis, and a concrete time
// activates the fork from that timestamp on. The fork is experimental, so no
// shipped network configuration may schedule it.
func TestConfigEip8304ForkActivation(t *testing.T) {
	cases := []struct {
		name string
		time *uint64
		head uint64
		want bool
	}{
		{"nil never activates", nil, 1 << 62, false},
		{"zero activates from genesis", ptr64(0), 0, true},
		{"before activation time", ptr64(1000), 999, false},
		{"at activation time", ptr64(1000), 1000, true},
		{"after activation time", ptr64(1000), 1001, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &chain.Config{Eip8304Time: c.time}
			assert.Equal(t, c.want, cfg.IsEip8304(c.head))
		})
	}
}

// TestConfigEip8304DefaultNetworksNotAffected locks the Milestone 2 invariant:
// default chainspecs never enable the experimental fork, so IsEip8304 is
// always false for their configurations.
func TestConfigEip8304DefaultNetworksNotAffected(t *testing.T) {
	for _, name := range []string{"mainnet", "sepolia", "hoodi"} {
		t.Run(name, func(t *testing.T) {
			cfg := readChainspecConfig(t, name)
			assert.Nil(t, cfg.Eip8304Time, "%s must not schedule the experimental fork", name)
			assert.False(t, cfg.IsEip8304(uint64(1<<62)), "%s must not be EIP-8304 active", name)
		})
	}
}

func readChainspecConfig(t *testing.T, network string) *chain.Config {
	t.Helper()
	raw, err := os.ReadFile("spec/chainspecs/" + network + ".json")
	require.NoError(t, err, "cannot read chainspec for %s", network)
	var cfg chain.Config
	require.NoError(t, json.Unmarshal(raw, &cfg), "cannot parse chainspec for %s", network)
	return &cfg
}

func ptr64(v uint64) *uint64 { return &v }
