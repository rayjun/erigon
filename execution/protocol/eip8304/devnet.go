package eip8304

import (
	"errors"
	"math/big"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
	"github.com/erigontech/erigon/execution/vm"
)

// Local development network support.
//
// EIP-8304 is not scheduled on any shipped network, so the only way to exercise
// it end to end is a local chain that activates the fork from genesis and
// predeploys an index contract. `--chain dev --dev.eip8304` does exactly that:
// it sets Eip8304Time=0 on the development genesis and allocates the mock
// contract below, so the index updates the finalize path issues can be read
// back over RPC (eth_getStorageAt) and compared against an independently
// computed root. None of this is consensus code: shipped networks never set
// Eip8304Time, and the mock stands in for the real contract whose address and
// deployment flow are still <TBD> (see docs/eip8304/open-questions.md).

// DevnetIndexContractAddress is the address the mock index contract is
// predeployed at on the local development network.
var DevnetIndexContractAddress = accounts.InternAddress(common.HexToAddress("0x0000000000000000000000000000000000008304"))

// Errors returned by EnableDevnet for inputs the devnet setup must not default.
var (
	ErrNilDevnetGenesis = errors.New("eip8304: nil genesis for devnet activation")
	ErrNilDevnetConfig  = errors.New("eip8304: nil chain config for devnet activation")
)

// MockIndexContractCode returns the runtime bytecode of the development-network
// mock index contract. Every index update writes the three calldata words into
// storage and bumps a call counter:
//
//	slot 0: first_block
//	slot 1: table_size
//	slot 2: table_root
//	slot 3: number of index updates applied
//
// The contract performs no validation and emits no event; it only makes the
// (first_block, table_size, table_root) sequence a node publishes observable,
// which is what the devnet and the integration tests assert on.
func MockIndexContractCode() []byte {
	return []byte{
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD), byte(vm.PUSH1), 0x00, byte(vm.SSTORE),
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD), byte(vm.PUSH1), 0x01, byte(vm.SSTORE),
		byte(vm.PUSH1), 0x40, byte(vm.CALLDATALOAD), byte(vm.PUSH1), 0x02, byte(vm.SSTORE),
		byte(vm.PUSH1), 0x03, byte(vm.SLOAD), byte(vm.PUSH1), 0x01, byte(vm.ADD), byte(vm.PUSH1), 0x03, byte(vm.SSTORE),
		byte(vm.STOP),
	}
}

// EnableDevnet activates the experimental fork from genesis and predeploys the
// mock index contract at indexAddr. The activation time is 0, i.e. every
// devnet block is EIP-8304-active; callers that need a later activation build
// their own genesis instead.
func EnableDevnet(genesis *types.Genesis, indexAddr accounts.Address) error {
	if genesis == nil {
		return ErrNilDevnetGenesis
	}
	if genesis.Config == nil {
		return ErrNilDevnetConfig
	}

	activation := uint64(0)
	genesis.Config.Eip8304Time = &activation

	if genesis.Alloc == nil {
		genesis.Alloc = types.GenesisAlloc{}
	}
	addr := indexAddr.Value()
	account := genesis.Alloc[addr]
	if account.Balance == nil {
		account.Balance = new(big.Int)
	}
	if account.Nonce == 0 {
		account.Nonce = 1
	}
	account.Code = MockIndexContractCode()
	genesis.Alloc[addr] = account
	return nil
}
