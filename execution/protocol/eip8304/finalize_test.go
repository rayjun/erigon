package eip8304

import (
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/misc"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

// TestFinalizeL0Composition locks the receipts → sorted L0 → SSZ root →
// index-contract calldata chain: for a fixed block the syscall receives the
// parameterized index address and a calldata whose table_root word is exactly
// the computed list root. The whole pipeline is deterministic and fail-closed.
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
	syscall := func(addr accounts.Address, data []byte) ([]byte, error) {
		gotAddr = addr
		gotData = append([]byte(nil), data...)
		return nil, nil
	}

	err := FinalizeL0(42, parent, receipts, indexAddr, limit, syscall)
	require.NoError(t, err)

	// The callee must be exactly the parameterized index address.
	require.Equal(t, indexAddr, gotAddr)

	// Recompute the expected root from the same pipeline.
	entries, err := BuildBlockEntriesFromReceipts(42, parent, receipts)
	require.NoError(t, err)
	entries.Sort()
	leaves := make([]common.Hash, len(entries))
	for i, e := range entries {
		leaves[i] = e.Encode().LeafHash()
	}
	wantRoot, err := ListHashRoot(leaves, limit)
	require.NoError(t, err)

	require.Len(t, gotData, 96)
	require.Equal(t, wantRoot[:], gotData[64:96], "table_root word must be the computed list root")
	require.Equal(t, misc.IndexCalldata(42, 1, wantRoot), gotData, "calldata must be (first_block=42, table_size=1, root)")
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
