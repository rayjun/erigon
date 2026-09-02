// Copyright 2026 The Erigon Authors
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

package chain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/erigontech/erigon/execution/chain"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
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
// every shipped chainspec never enables the experimental fork, so IsEip8304
// is always false for its configuration. Configs come from the in-memory
// registered specs rather than re-reading the JSON files.
func TestConfigEip8304DefaultNetworksNotAffected(t *testing.T) {
	shipped := []*chainspec.Spec{
		&chainspec.Mainnet,
		&chainspec.Sepolia,
		&chainspec.Hoodi,
		&chainspec.Gnosis,
		&chainspec.Chiado,
		&chainspec.Bloatnet,
	}
	for _, s := range shipped {
		t.Run(s.Name, func(t *testing.T) {
			assert.Nil(t, s.Config.Eip8304Time, "%s must not schedule the experimental fork", s.Name)
			assert.False(t, s.Config.IsEip8304(uint64(1<<62)), "%s must not be EIP-8304 active", s.Name)
		})
	}
}

// TestConfigEip8304DevGenesisNotAffected covers the special development
// genesis used by --dev mode.
func TestConfigEip8304DevGenesisNotAffected(t *testing.T) {
	cfg := chainspec.DeveloperGenesisBlock().Config
	if cfg == nil {
		return
	}
	assert.Nil(t, cfg.Eip8304Time)
	assert.False(t, cfg.IsEip8304(uint64(1<<62)))
}

func ptr64(v uint64) *uint64 { return &v }
