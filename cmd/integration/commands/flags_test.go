package commands

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
)

func TestEip8304FinalizeConfig(t *testing.T) {
	oldIndex, oldLimit := eip8304IndexContract, eip8304ListLimit
	t.Cleanup(func() {
		eip8304IndexContract, eip8304ListLimit = oldIndex, oldLimit
	})

	eip8304IndexContract, eip8304ListLimit = "", 0
	got, err := eip8304FinalizeConfig()
	require.NoError(t, err)
	require.Nil(t, got)

	eip8304IndexContract = "0x0000000000000000000000000000000000008304"
	eip8304ListLimit = 4096
	got, err = eip8304FinalizeConfig()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, common.HexToAddress(eip8304IndexContract), got[0].IndexContract)
	require.Equal(t, eip8304ListLimit, got[0].ListLimit)

	eip8304ListLimit = 0
	_, err = eip8304FinalizeConfig()
	require.ErrorContains(t, err, "must be set together")

	eip8304IndexContract, eip8304ListLimit = "not-an-address", 4096
	_, err = eip8304FinalizeConfig()
	require.ErrorContains(t, err, "invalid --eip8304.index-contract")
}
