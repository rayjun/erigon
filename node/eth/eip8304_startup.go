package eth

import (
	"errors"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/types"
)

func eip8304StartupChainConfig(storedGenesis, cliGenesis *types.Genesis) *chain.Config {
	if storedGenesis != nil {
		return storedGenesis.Config
	}
	if cliGenesis != nil {
		return cliGenesis.Config
	}
	return nil
}

func eip8304StartupEnabled(chainConfig *chain.Config, indexContract *common.Address, listLimit uint64) (bool, error) {
	scheduled := chainConfig != nil && chainConfig.IsEip8304Scheduled()
	configured := indexContract != nil || listLimit != 0
	if scheduled && !configured {
		return false, errors.New("EIP-8304 fork is scheduled but the index contract and list limit are not configured")
	}
	if !configured {
		return false, nil
	}
	if indexContract == nil || listLimit == 0 {
		return false, errors.New("EIP-8304 index contract and list limit must be configured together")
	}
	if !scheduled {
		return false, errors.New("EIP-8304 parameters are set but the fork is not scheduled")
	}
	return true, nil
}
