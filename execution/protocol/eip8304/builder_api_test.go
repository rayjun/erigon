package eip8304_test

import (
	"encoding/binary"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/types"
)

func TestBuildBlockEntriesPublicContract(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
		types.NewTransaction(1, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := types.Receipts{
		{Logs: types.Logs{{Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}},
		{},
	}

	entries, err := eip8304.BuildBlockEntries(42, common.HexToHash("0x01"), transactions, receipts)
	require.NoError(t, err)
	require.Len(t, entries, 4)
	require.Equal(t, eip8304.EntryBlock, entries[0].Type)
	require.Equal(t, eip8304.EntryTransaction, entries[1].Type)
	require.Equal(t, eip8304.EntryLogAddress, entries[2].Type)
	require.Equal(t, eip8304.EntryTransaction, entries[3].Type)

	addressEntry := entries[2].Encode()
	require.Equal(t, uint32(0), binary.BigEndian.Uint32(addressEntry[len(addressEntry)-4:]))
	secondTransactionEntry := entries[3].Encode()
	require.Equal(t, uint32(1), binary.BigEndian.Uint32(secondTransactionEntry[len(secondTransactionEntry)-4:]))
}
