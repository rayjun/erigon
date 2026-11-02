package eip8304_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/jinzhu/copier"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/dbg"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/execmodule/execmoduletester"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/protocol/params"
	"github.com/erigontech/erigon/execution/stagedsync"
	"github.com/erigontech/erigon/execution/tests/blockgen"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
	"github.com/erigontech/erigon/execution/vm"
	"github.com/erigontech/erigon/p2p/protocols/eth"
)

// TestParallelExecutionParityForFinalizedTables executes the same
// log-producing chain through Erigon's serial and parallel executors. Both the
// receipts each path stored and the finalizer's system-call effects are checked
// for parity: the final mock-index storage and the sealed state root must match.
func TestParallelExecutionParityForFinalizedTables(t *testing.T) {
	key, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(key.PublicKey)
	contract := common.HexToAddress("0x0000000000000000000000000000000000008305")
	// emit LOG0 with no topics or data, then stop.
	logCode := []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.LOG0), byte(vm.STOP)}
	var chainConfig chain.Config
	require.NoError(t, copier.CopyWithOption(&chainConfig, chain.TestChainBerlinConfig, copier.Option{DeepCopy: true}))
	gspec := &types.Genesis{
		Config: &chainConfig,
		Alloc: types.GenesisAlloc{
			from:     {Balance: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)},
			contract: {Balance: new(big.Int), Code: logCode},
		},
	}
	require.NoError(t, eip8304.EnableDevnet(gspec, eip8304.DevnetIndexContractAddress))
	const listLimit = uint64(1 << 12)

	// Generate the chain with the same finalizer effects. Both executors then
	// import the same blocks, so a difference can only come from execution, not
	// from two independently generated chains.
	builder := execmoduletester.New(
		t,
		execmoduletester.WithGenesisSpec(gspec),
		execmoduletester.WithKey(key),
		execmoduletester.WithEip8304Finalize(eip8304.DevnetIndexContractAddress.Value(), listLimit),
	)
	signer := types.LatestSignerForChainID(builder.ChainConfig.ChainID)
	generationStore := &parityTableStore{
		tables: make(map[eip8304.TableRef]eip8304.TableResult),
		hashes: make(map[uint64]common.Hash),
	}
	generationStore.seedGenesis(builder.Genesis.Hash(), listLimit)
	generationFinalizer := newGenerationFinalizer(generationStore, listLimit)
	chainPack, err := blockgen.GenerateChain(builder.ChainConfig, builder.Genesis, builder.Engine, builder.DB, 5, func(_ int, b *blockgen.BlockGen) {
		b.SetCoinbase(common.Address{1})
		// Leave room for the contract's LOG0; TxGas alone would run out first.
		tx, err := types.SignTx(types.NewTransaction(b.TxNonce(from), contract, uint256.NewInt(0), params.TxGas+100_000, uint256.NewInt(1), nil), *signer, key)
		require.NoError(t, err)
		b.AddTx(tx)
	}, generationFinalizer)
	require.NoError(t, err)

	runChain := func(t *testing.T, parallel bool) *execmoduletester.ExecModuleTester {
		t.Helper()
		prev := dbg.Exec3Parallel
		dbg.Exec3Parallel = parallel
		t.Cleanup(func() { dbg.Exec3Parallel = prev })

		opts := []execmoduletester.Option{
			execmoduletester.WithGenesisSpec(gspec),
			execmoduletester.WithKey(key),
			execmoduletester.WithEip8304Finalize(eip8304.DevnetIndexContractAddress.Value(), listLimit),
		}
		if !parallel {
			opts = append(opts, execmoduletester.WithoutExperimentalBAL())
		}
		// The counter is process-wide, so this test must stay sequential.
		parallelBefore, serialBefore := stagedsync.Exec3ExecutorRunCounts()
		emt := execmoduletester.New(t, opts...)
		require.NoError(t, emt.InsertChain(chainPack))
		parallelAfter, serialAfter := stagedsync.Exec3ExecutorRunCounts()
		if parallel {
			require.Greater(t, parallelAfter, parallelBefore, "the parallel executor must run")
			require.Equal(t, serialBefore, serialAfter, "the parallel run must not select the serial executor")
		} else {
			require.Greater(t, serialAfter, serialBefore, "the serial executor must run")
			require.Equal(t, parallelBefore, parallelAfter, "the serial run must not select the parallel executor")
		}
		return emt
	}

	serial := runChain(t, false)
	parallel := runChain(t, true)

	type blockResult struct {
		entries []byte
		root    common.Hash
	}
	collect := func(t *testing.T, emt *execmoduletester.ExecModuleTester) []blockResult {
		t.Helper()
		ctx := context.Background()
		tx, err := emt.DB.BeginTemporalRo(ctx)
		require.NoError(t, err)
		defer tx.Rollback()

		out := make([]blockResult, 0, 5)
		for number := uint64(1); number <= 5; number++ {
			block, err := emt.BlockReader.BlockByNumber(ctx, tx, number)
			require.NoError(t, err)
			receipts, err := emt.ReceiptsReader.GetReceipts(ctx, emt.ChainConfig, tx, block, eth.ReceiptsOpts{})
			require.NoError(t, err)
			require.Len(t, receipts, 1, "block %d must store its transaction receipt", number)
			require.Len(t, receipts[0].Logs, 1, "block %d must store the contract log", number)
			require.Equal(t, contract, receipts[0].Logs[0].Address, "block %d log address", number)
			entries, err := eip8304.BuildBlockEntriesFromReceipts(number, block.ParentHash(), receipts)
			require.NoError(t, err)
			entries.Sort()
			root, err := eip8304.RootOfEntries(entries, 1<<12)
			require.NoError(t, err)
			var encoded []byte
			for _, entry := range entries {
				encoded = append(encoded, entry.Encode()...)
			}
			out = append(out, blockResult{entries: encoded, root: root})
		}
		return out
	}

	serialBlocks := collect(t, serial)
	parallelBlocks := collect(t, parallel)
	require.Len(t, parallelBlocks, len(serialBlocks))
	for i := range serialBlocks {
		require.True(t, bytes.Equal(serialBlocks[i].entries, parallelBlocks[i].entries), "block %d entries differ", i+1)
		require.Equal(t, serialBlocks[i].root, parallelBlocks[i].root, "block %d table root differs", i+1)
	}

	type finalizedState struct {
		calls     uint64
		first     uint64
		size      uint64
		root      common.Hash
		stateRoot common.Hash
	}
	readFinalizedState := func(t *testing.T, emt *execmoduletester.ExecModuleTester) finalizedState {
		t.Helper()
		tx, err := emt.DB.BeginTemporalRo(context.Background())
		require.NoError(t, err)
		defer tx.Rollback()

		reader := emt.NewStateReader(tx)
		slot := func(n uint64) (uint256.Int, bool) {
			var key common.Hash
			binary.BigEndian.PutUint64(key[24:], n)
			value, ok, err := reader.ReadAccountStorage(eip8304.DevnetIndexContractAddress, accounts.InternKey(key))
			require.NoError(t, err)
			return value, ok
		}
		calls, ok := slot(3)
		require.True(t, ok, "the mock index contract must have received finalizer calls")
		first, ok := slot(0)
		require.True(t, ok)
		size, ok := slot(1)
		require.True(t, ok)
		root, ok := slot(2)
		require.True(t, ok)
		header, err := emt.BlockReader.HeaderByNumber(context.Background(), tx, 5)
		require.NoError(t, err)
		require.NotNil(t, header)
		return finalizedState{
			calls:     calls.Uint64(),
			first:     first.Uint64(),
			size:      size.Uint64(),
			root:      common.Hash(root.Bytes32()),
			stateRoot: header.Root,
		}
	}
	readHotTables := func(t *testing.T, emt *execmoduletester.ExecModuleTester) map[string][]byte {
		t.Helper()
		tx, err := emt.DB.BeginTemporalRo(context.Background())
		require.NoError(t, err)
		defer tx.Rollback()

		cursor, err := tx.Cursor(kv.Eip8304Tables)
		require.NoError(t, err)
		defer cursor.Close()
		tables := make(map[string][]byte)
		for k, v, err := cursor.First(); k != nil; k, v, err = cursor.Next() {
			require.NoError(t, err)
			tables[string(append([]byte(nil), k...))] = append([]byte(nil), v...)
		}
		return tables
	}
	serialState := readFinalizedState(t, serial)
	parallelState := readFinalizedState(t, parallel)
	require.Equal(t, uint64(6), serialState.calls, "five L0 tables plus the delayed (0,4) table")
	require.Equal(t, serialState, parallelState, "serial and parallel finalizers must publish the same tables and state root")
	serialTables := readHotTables(t, serial)
	parallelTables := readHotTables(t, parallel)
	require.Len(t, serialTables, 6, "five L0 records plus the delayed (0,4) record")
	require.Equal(t, serialTables, parallelTables, "both executors must persist byte-identical table records")
}

// parityTableStore is just large enough for the L0 tables and the delayed
// (0,4) merge in this test. The genesis table is seeded explicitly because the
// execution hook deliberately does not run at block 0.
type parityTableStore struct {
	tables map[eip8304.TableRef]eip8304.TableResult
	hashes map[uint64]common.Hash
}

func (s *parityTableStore) seedGenesis(hash common.Hash, limit uint64) {
	root, err := eip8304.RootOfEntries(nil, limit)
	if err != nil {
		panic(err)
	}
	s.tables[eip8304.L0(0)] = eip8304.TableResult{
		Ref:         eip8304.L0(0),
		Root:        root,
		BlockHashes: []common.Hash{hash},
	}
	s.hashes[0] = hash
}

func (s *parityTableStore) CanonicalHash(block uint64) (common.Hash, error) {
	hash, ok := s.hashes[block]
	if !ok {
		return common.Hash{}, errors.New("canonical hash is not available in the generation store")
	}
	return hash, nil
}

func (s *parityTableStore) BlockEntries(uint64) (eip8304.Entries, error) {
	return nil, errors.New("canonical entries are not needed for L0 generation")
}

func (s *parityTableStore) GetTable(ref eip8304.TableRef) (eip8304.TableResult, bool, error) {
	result, ok := s.tables[ref]
	return result, ok, nil
}

func (s *parityTableStore) PutTable(result eip8304.TableResult) error {
	s.tables[result.Ref] = result
	if result.Ref.TableSize == 1 && len(result.BlockHashes) == 1 {
		s.hashes[result.Ref.FirstBlock] = result.BlockHashes[0]
	}
	return nil
}
