package eip8304

import (
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/misc"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

// sortedRoot computes the L0 list root the way FinalizeL0 does, from its
// public inputs. The composition test uses it only to fingerprint the
// expected output; the sorted-vs-unsorted assertion below is what makes the
// sort step observable.
func sortedRoot(t *testing.T, block uint64, parent common.Hash, receipts types.Receipts, limit uint64) common.Hash {
	t.Helper()
	entries, err := BuildBlockEntriesFromReceipts(block, parent, receipts)
	require.NoError(t, err)
	entries.Sort()
	leaves := make([]common.Hash, len(entries))
	for i, e := range entries {
		leaves[i] = e.Encode().LeafHash()
	}
	root, err := ListHashRoot(leaves, limit)
	require.NoError(t, err)
	return root
}

// TestFinalizeL0Composition locks the receipts → sorted L0 → SSZ root →
// index-contract calldata chain. The syscall receives the parameterized index
// address and a calldata whose table_root word is exactly the sorted list
// root.
func TestFinalizeL0Composition(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
		types.NewTransaction(1, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := types.Receipts{
		{TxHash: transactions[0].Hash(), Logs: types.Logs{
			{Address: common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Topics: []common.Hash{{1}, {2}}},
		}},
		{TxHash: transactions[1].Hash(), Logs: types.Logs{}},
	}

	parent := common.HexToHash("0x42f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b65")
	indexAddr := accounts.InternAddress(common.HexToAddress("0x0000000000000000000000000000000000000001"))
	limit := uint64(1 << 8)

	var gotAddr accounts.Address
	var gotData []byte
	var syscall rules.SystemCall = func(addr accounts.Address, data []byte) ([]byte, error) {
		gotAddr = addr
		gotData = append([]byte(nil), data...)
		return nil, nil
	}

	err := FinalizeL0(42, parent, receipts, indexAddr, limit, syscall)
	require.NoError(t, err)

	// The callee must be exactly the parameterized index address.
	require.Equal(t, indexAddr, gotAddr)

	// Sorting must be observable: the unsorted (chronological) root differs
	// from the sorted root for this receipt set (tx1 moves ahead of tx0's
	// logs), and only the sorted root may reach the contract.
	unsortedEntries, err := BuildBlockEntriesFromReceipts(42, parent, receipts)
	require.NoError(t, err)
	unsortedLeaves := make([]common.Hash, len(unsortedEntries))
	for i, e := range unsortedEntries {
		unsortedLeaves[i] = e.Encode().LeafHash()
	}
	unsortedRoot, err := ListHashRoot(unsortedLeaves, limit)
	require.NoError(t, err)
	require.NotEqual(t, unsortedRoot, sortedRoot(t, 42, parent, receipts, limit))

	require.Len(t, gotData, 96)
	wantSorted := sortedRoot(t, 42, parent, receipts, limit)
	require.Equal(t, wantSorted[:], gotData[64:96], "table_root word must be the sorted list root")
	require.Equal(t, misc.IndexCalldata(42, 1, wantSorted), gotData, "calldata must be (first_block=42, table_size=1, sorted root)")
}

// TestFinalizeL0Genesis locks the genesis boundary: the applied table is
// (firstBlock=0, tableSize=1), contains exactly the single transaction entry
// (no parent leaf), and the system call still runs. The expected root is
// computed independently from the entry constructor, not from the builder, so
// a regression that adds a parent entry to genesis would fail this test.
func TestFinalizeL0Genesis(t *testing.T) {
	transaction := types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil)
	receipts := types.Receipts{{TxHash: transaction.Hash(), Logs: types.Logs{}}}

	var gotData []byte
	calls := 0
	var syscall rules.SystemCall = func(addr accounts.Address, data []byte) ([]byte, error) {
		calls++
		gotData = append([]byte(nil), data...)
		return nil, nil
	}

	err := FinalizeL0(0, common.Hash{}, receipts, accounts.Address{}, 1<<6, syscall)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "L0 must still be applied for genesis")

	txEntry := NewTransactionEntry(0, 0, 0, transaction.Hash())
	wantRoot, err := ListHashRoot([]common.Hash{txEntry.Encode().LeafHash()}, 1<<6)
	require.NoError(t, err)
	require.Equal(t, misc.IndexCalldata(0, 1, wantRoot), gotData, "calldata must be (first_block=0, table_size=1, single-tx root)")
}

// TestFinalizeL0EmptyNonGenesis locks the empty-block boundary: a non-genesis
// block with no transactions still applies its L0 table, whose single leaf is
// the parent-block entry. The expected root is computed independently from
// NewBlockEntry, and the whole calldata is compared, so an off-by-one in the
// parent block number or a high-byte leak in the words cannot slip through.
func TestFinalizeL0EmptyNonGenesis(t *testing.T) {
	parent := common.HexToHash("0x11")
	var gotData []byte
	var syscall rules.SystemCall = func(addr accounts.Address, data []byte) ([]byte, error) {
		gotData = append([]byte(nil), data...)
		return nil, nil
	}

	err := FinalizeL0(5, parent, nil, accounts.Address{}, 1<<2, syscall)
	require.NoError(t, err)

	parentEntry := NewBlockEntry(4, parent)
	wantRoot, err := ListHashRoot([]common.Hash{parentEntry.Encode().LeafHash()}, 1<<2)
	require.NoError(t, err)
	require.Equal(t, misc.IndexCalldata(5, 1, wantRoot), gotData, "calldata must be (first_block=5, table_size=1, parent-entry root)")
}

// TestFinalizeL0ListLimitRejected covers the limit boundary: more leaves than
// the SSZ limit rejects before any system call. Block 42 with two no-log
// receipts yields three leaves (parent + two tx entries), exceeding limit 2.
func TestFinalizeL0ListLimitRejected(t *testing.T) {
	transactions := types.Transactions{
		types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
		types.NewTransaction(1, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil),
	}
	receipts := receiptsWithTxHashes(transactions)

	called := false
	var syscall rules.SystemCall = func(addr accounts.Address, data []byte) ([]byte, error) {
		called = true
		return nil, nil
	}

	err := FinalizeL0(42, common.Hash{}, receipts, accounts.Address{}, 2, syscall)
	require.ErrorIs(t, err, ErrExceedsListLimit)
	require.False(t, called)
}

// TestFinalizeL0FailClosed covers propagation: an invalid receipt input or a
// reverted system call must surface, not be swallowed.
func TestFinalizeL0FailClosed(t *testing.T) {
	// Missing TxHash fails before any syscall.
	called := false
	err := FinalizeL0(42, common.Hash{}, types.Receipts{{}}, accounts.Address{}, 8,
		func(addr accounts.Address, data []byte) ([]byte, error) {
			called = true
			return nil, nil
		})
	require.ErrorIs(t, err, ErrMissingTxHash)
	require.False(t, called)

	// A real system-call failure propagates to the caller of FinalizeL0.
	transaction := types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil)
	receipts := types.Receipts{{TxHash: transaction.Hash(), Logs: types.Logs{}}}
	boom := &testSyscallError{}
	err = FinalizeL0(42, common.Hash{}, receipts, accounts.Address{}, 8,
		func(addr accounts.Address, data []byte) ([]byte, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
}

type testSyscallError struct{}

func (*testSyscallError) Error() string { return "revert" }
