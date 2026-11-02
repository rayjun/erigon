package eth

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/types"
)

func TestEip8304StartupConfigPrefersStoredGenesis(t *testing.T) {
	zero := uint64(0)
	stored := &types.Genesis{Config: &chain.Config{Eip8304Time: &zero}}
	cli := &types.Genesis{Config: &chain.Config{}}
	addr := common.HexToAddress("0x0000000000000000000000000000000000008304")

	enabled, err := eip8304StartupEnabled(eip8304StartupChainConfig(stored, cli), &addr, 4096)
	require.NoError(t, err)
	require.True(t, enabled, "a restart must use the fork config stored in the datadir")
}

func TestEip8304StartupEnabledRejectsOneSidedConfiguration(t *testing.T) {
	zero := uint64(0)
	fork := &chain.Config{Eip8304Time: &zero}
	noFork := &chain.Config{}
	addr := common.HexToAddress("0x0000000000000000000000000000000000008304")

	tests := []struct {
		name        string
		config      *chain.Config
		addr        *common.Address
		limit       uint64
		wantEnabled bool
		wantErr     string
	}{
		{name: "disabled", config: noFork},
		{name: "fork without parameters", config: fork, wantErr: "fork is scheduled"},
		{name: "parameters without fork", config: noFork, addr: &addr, limit: 4096, wantErr: "fork is not scheduled"},
		{name: "address only", config: fork, addr: &addr, wantErr: "must be configured together"},
		{name: "limit only", config: fork, limit: 4096, wantErr: "must be configured together"},
		{name: "fork with pair", config: fork, addr: &addr, limit: 4096, wantEnabled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eip8304StartupEnabled(tc.config, tc.addr, tc.limit)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantEnabled, got)
		})
	}
}
