package eip8304_test

import (
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/protocol/misc"
	"github.com/erigontech/erigon/execution/state"
	"github.com/erigontech/erigon/execution/tests/testutil"
	"github.com/erigontech/erigon/execution/tracing"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
	"github.com/erigontech/erigon/execution/vm/runtime"
)

// This is an external test package because the EVM harness below pulls in
// execution/tests/testutil, which reaches cmd/utils and therefore the package
// under test.

func TestEnableDevnetActivatesForkAndPredeploysMockIndexContract(t *testing.T) {
	t.Parallel()

	genesis := chainspec.DeveloperGenesisBlock()
	require.False(t, genesis.Config.IsEip8304(0), "the development genesis must not activate the fork on its own")

	// An unrelated allocation must survive activation.
	other := accounts.InternAddress(common.HexToAddress("0xdeadbeef"))
	genesis.Alloc[other.Value()] = types.GenesisAccount{Balance: big.NewInt(1)}

	require.NoError(t, eip8304.EnableDevnet(genesis, eip8304.DevnetIndexContractAddress))
	require.True(t, genesis.Config.IsEip8304(0), "EnableDevnet must activate the fork from genesis")

	account, ok := genesis.Alloc[eip8304.DevnetIndexContractAddress.Value()]
	require.True(t, ok, "the mock index contract must be allocated")
	require.Equal(t, eip8304.MockIndexContractCode(), account.Code)
	require.NotNil(t, account.Balance)
	require.Equal(t, big.NewInt(1), genesis.Alloc[other.Value()].Balance)
}

func TestEnableDevnetRejectsNilInputs(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, eip8304.EnableDevnet(nil, eip8304.DevnetIndexContractAddress), eip8304.ErrNilDevnetGenesis)
	require.ErrorIs(t, eip8304.EnableDevnet(&types.Genesis{}, eip8304.DevnetIndexContractAddress), eip8304.ErrNilDevnetConfig)
}

// TestMockIndexContractRecordsIndexUpdates runs the mock contract's bytecode in
// the EVM and checks the storage it produces, so the devnet's on-chain evidence
// is validated independently of the devnet itself.
func TestMockIndexContractRecordsIndexUpdates(t *testing.T) {
	t.Parallel()

	db := testutil.TemporalDB(t)
	tx, domains := testutil.TemporalTxSD(t, db)
	statedb := state.New(state.NewReaderV3(domains.AsGetter(tx)))
	defer statedb.Release(false)

	addr := eip8304.DevnetIndexContractAddress
	statedb.CreateAccount(addr, true)
	statedb.SetCode(addr, eip8304.MockIndexContractCode(), tracing.CodeChangeUnspecified)

	cfg := &runtime.Config{State: statedb, Origin: accounts.InternAddress(common.HexToAddress("0x1234"))}

	_, _, err := runtime.Call(addr, misc.IndexCalldata(42, 4, devnetTestHash(7)), cfg)
	require.NoError(t, err)
	requireStorageWord(t, statedb, addr, 0, uint256.NewInt(42))
	requireStorageWord(t, statedb, addr, 1, uint256.NewInt(4))
	requireStorageWord(t, statedb, addr, 2, hashWord(devnetTestHash(7)))
	requireStorageWord(t, statedb, addr, 3, uint256.NewInt(1))

	// A second update overwrites the triple and bumps the call counter.
	_, _, err = runtime.Call(addr, misc.IndexCalldata(43, 16, devnetTestHash(9)), cfg)
	require.NoError(t, err)
	requireStorageWord(t, statedb, addr, 0, uint256.NewInt(43))
	requireStorageWord(t, statedb, addr, 1, uint256.NewInt(16))
	requireStorageWord(t, statedb, addr, 2, hashWord(devnetTestHash(9)))
	requireStorageWord(t, statedb, addr, 3, uint256.NewInt(2))
}

func requireStorageWord(t *testing.T, statedb *state.IntraBlockState, addr accounts.Address, slot uint64, want *uint256.Int) {
	t.Helper()

	var key common.Hash
	binary.BigEndian.PutUint64(key[24:], slot)
	got, err := statedb.GetState(addr, accounts.InternKey(key))
	require.NoError(t, err)
	require.Truef(t, got.Eq(want), "slot %d: have %s, want %s", slot, got.Hex(), want.Hex())
}

func hashWord(h common.Hash) *uint256.Int {
	return new(uint256.Int).SetBytes(h[:])
}

func devnetTestHash(n uint64) common.Hash {
	var hash common.Hash
	binary.BigEndian.PutUint64(hash[len(hash)-8:], n+1)
	return hash
}
