package eip8304

import (
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types"
)

func TestBuildBlockEntries(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
		types.NewTransaction(1, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := types.Receipts{
		{FirstLogIndexWithinBlock: 50, Logs: types.Logs{
			{Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Topics: []common.Hash{{1}, {2}, {3}, {4}}, Index: 50},
			{Address: common.HexToAddress("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Topics: []common.Hash{{5}, {6}}, Index: 51},
		}},
		{FirstLogIndexWithinBlock: 52, Logs: types.Logs{
			{Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Topics: []common.Hash{{7}}, Index: 52},
		}},
	}

	entries, err := BuildBlockEntries(
		42,
		common.HexToHash("0x42f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b65"),
		transactions,
		receipts,
	)
	require.NoError(t, err)
	require.Len(t, entries, 13)
	require.Equal(t, EntryBlock, entries[0].Type)
	require.Equal(t, uint64(41), entries[0].block)
	require.Equal(t, EntryTransaction, entries[1].Type)
	require.Equal(t, transactions[0].Hash(), entries[1].Value)
	require.Equal(t, uint32(0), entries[1].position)
	require.Equal(t, EntryLogTopic3, entries[6].Type)
	require.Equal(t, EntryTransaction, entries[10].Type)
	require.Equal(t, transactions[1].Hash(), entries[10].Value)
	require.Equal(t, uint32(2), entries[10].position)
	require.Equal(t, EntryLogAddress, entries[11].Type)
	require.Equal(t, uint32(0), entries[11].position)
}

func TestBuildBlockEntriesGenesisHasNoParentEntry(t *testing.T) {
	entries, err := BuildBlockEntries(0, common.Hash{}, nil, nil)

	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestBuildBlockEntriesGenesisIncludesTransactionAndLogs(t *testing.T) {
	transaction := types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil)
	receipts := types.Receipts{{Logs: types.Logs{{
		Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Topics:  []common.Hash{{1}},
	}}}}

	entries, err := BuildBlockEntries(0, common.HexToHash("0x01"), types.Transactions{transaction}, receipts)

	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, EntryTransaction, entries[0].Type)
	require.Equal(t, EntryLogAddress, entries[1].Type)
	require.Equal(t, EntryLogTopic0, entries[2].Type)
	for _, entry := range entries {
		require.Equal(t, uint64(0), entry.block)
	}
}

func TestBuildBlockEntriesMatchesHashBasedFixturePath(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
		types.NewTransaction(1, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := types.Receipts{
		{Logs: types.Logs{{Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}},
		{},
	}
	transactionHashes := []common.Hash{transactions[0].Hash(), transactions[1].Hash()}
	parentBlockHash := common.HexToHash("0x978ce0036b6d1c62d716045505587d15cc85a1def92f9f450937b6467295e517")

	publicEntries, err := BuildBlockEntries(42, parentBlockHash, transactions, receipts)
	require.NoError(t, err)
	fixtureEntries, err := buildBlockEntriesFromHashes(42, parentBlockHash, transactionHashes, receipts)
	require.NoError(t, err)

	require.Equal(t, fixtureEntries, publicEntries)
}

func TestBuildBlockEntriesRejectsInvalidInputs(t *testing.T) {
	transaction := types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil)
	fiveTopics := []common.Hash{{1}, {2}, {3}, {4}, {5}}

	tests := []struct {
		name         string
		transactions types.Transactions
		receipts     types.Receipts
		want         error
	}{
		{
			name:         "receipt count mismatch",
			transactions: types.Transactions{transaction},
			want:         ErrReceiptCountMismatch,
		},
		{
			name:         "nil transaction",
			transactions: types.Transactions{nil},
			receipts:     types.Receipts{{}},
			want:         ErrNilTransaction,
		},
		{
			name:         "nil receipt",
			transactions: types.Transactions{transaction},
			receipts:     types.Receipts{nil},
			want:         ErrNilReceipt,
		},
		{
			name:         "nil log",
			transactions: types.Transactions{transaction},
			receipts:     types.Receipts{{Logs: types.Logs{nil}}},
			want:         ErrNilLog,
		},
		{
			name:         "too many topics",
			transactions: types.Transactions{transaction},
			receipts: types.Receipts{{Logs: types.Logs{
				{Topics: fiveTopics},
			}}},
			want: ErrTooManyLogTopics,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildBlockEntries(42, common.Hash{}, test.transactions, test.receipts)
			require.ErrorIs(t, err, test.want)
		})
	}
}
