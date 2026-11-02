package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
)

func TestParseEip8304Config(t *testing.T) {
	const raw = "0x0000000000000000000000000000000000008304"
	addr, limit, err := parseEip8304Config(raw, 4096)
	require.NoError(t, err)
	require.Equal(t, common.HexToAddress(raw), *addr)
	require.Equal(t, uint64(4096), limit)

	addr, limit, err = parseEip8304Config("", 0)
	require.NoError(t, err)
	require.Nil(t, addr)
	require.Zero(t, limit)

	_, _, err = parseEip8304Config(raw, 0)
	require.ErrorContains(t, err, "must be set together")
	_, _, err = parseEip8304Config("", 4096)
	require.ErrorContains(t, err, "must be set together")
	_, _, err = parseEip8304Config("not-an-address", 4096)
	require.ErrorContains(t, err, "invalid --eip8304.index-contract")
}
