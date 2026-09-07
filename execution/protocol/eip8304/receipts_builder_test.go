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
