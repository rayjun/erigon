package eip8304

import (
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types"
)

// receiptsWithTxHashes builds receipts whose TxHash matches the given
// transactions, simulating receipts after DeriveFields* on the Finalize path.
func receiptsWithTxHashes(transactions types.Transactions) types.Receipts {
	receipts := make(types.Receipts, len(transactions))
	for i, tx := range transactions {
		receipts[i] = &types.Receipt{TxHash: tx.Hash(), Logs: types.Logs{}}
	}
	return receipts
}

// TestBuildBlockEntriesFromReceiptsEqualsTransactionPath locks the Week 12
// input-source contract: building entries from receipts alone (via TxHash)
// must produce exactly the same entries as building from the transactions
// themselves, so the Finalize path can reconstruct ordered hashes without a
// txs parameter and without database access.
func TestBuildBlockEntriesFromReceiptsEqualsTransactionPath(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
		types.NewTransaction(1, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := receiptsWithTxHashes(transactions)
	// Receipt 0 gets one log with two topics, receipt 1 none.
	receipts[0].Logs = types.Logs{{
		Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Topics:  []common.Hash{{1}, {2}},
	}}

	parent := common.HexToHash("0x42f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b65")
	fromReceipts, err := BuildBlockEntriesFromReceipts(42, parent, receipts)
	require.NoError(t, err)
	fromTransactions, err := BuildBlockEntries(42, parent, transactions, receipts)
	require.NoError(t, err)

	require.Equal(t, len(fromTransactions), len(fromReceipts))
	require.Equal(t, encodedOf(fromTransactions), encodedOf(fromReceipts))
}

// TestBuildBlockEntriesFromReceiptsMissingTxHash covers the fail-closed
// contract: a receipt without a derived TxHash must be rejected rather than
// silently contributing a zero transaction hash.
func TestBuildBlockEntriesFromReceiptsMissingTxHash(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := receiptsWithTxHashes(transactions)
	receipts[0].TxHash = common.Hash{} // simulate not-yet-derived receipt

	_, err := BuildBlockEntriesFromReceipts(42, common.Hash{}, receipts)
	require.ErrorIs(t, err, ErrMissingTxHash)
}

// TestBuildBlockEntriesFromReceiptsNilReceipt and topic/log validation still
// apply on the receipts-only path.
func TestBuildBlockEntriesFromReceiptsNilReceipt(t *testing.T) {
	_, err := BuildBlockEntriesFromReceipts(42, common.Hash{}, types.Receipts{nil})
	require.ErrorIs(t, err, ErrNilReceipt)
}

func TestBuildBlockEntriesFromReceiptsTooManyTopics(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := receiptsWithTxHashes(transactions)
	receipts[0].Logs = types.Logs{{Topics: []common.Hash{{1}, {2}, {3}, {4}, {5}}}}

	_, err := BuildBlockEntriesFromReceipts(42, common.Hash{}, receipts)
	require.ErrorIs(t, err, ErrTooManyLogTopics)
}

// TestBuildBlockEntriesFromReceiptsGenesisWithTransactions locks the genesis
// boundary on the receipts-only path: block 0 emits no parent entry, but its
// transactions and logs are still indexed.
func TestBuildBlockEntriesFromReceiptsGenesisWithTransactions(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := receiptsWithTxHashes(transactions)
	receipts[0].Logs = types.Logs{{Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}

	entries, err := BuildBlockEntriesFromReceipts(0, common.Hash{}, receipts)
	require.NoError(t, err)
	require.Len(t, entries, 2) // transaction entry + log address entry, no parent block entry
	require.Equal(t, EntryTransaction, entries[0].Type)
	require.Equal(t, EntryLogAddress, entries[1].Type)
}

// TestBuildBlockEntriesFromReceiptsEmptyBlock locks the empty non-genesis block
// boundary: exactly one parent-block entry, no transactions.
func TestBuildBlockEntriesFromReceiptsEmptyBlock(t *testing.T) {
	entries, err := BuildBlockEntriesFromReceipts(42, common.HexToHash("0x01"), nil)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, EntryBlock, entries[0].Type)
	require.Equal(t, uint64(41), entries[0].block)
	require.Equal(t, common.HexToHash("0x01"), entries[0].Value)
}
