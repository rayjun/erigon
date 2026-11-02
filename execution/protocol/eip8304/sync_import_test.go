package eip8304_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/jinzhu/copier"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/execmodule/execmoduletester"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/protocol/params"
	"github.com/erigontech/erigon/execution/tests/blockgen"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/p2p/protocols/eth"
)

// TestBootstrapFromImportedChain drives the sync path through blocks Erigon
// actually imported: InsertChain persists the block, receipts and execution
// progress, DBData reads canonical hashes and receipts back from that database,
// and Bootstrap rebuilds the windows due at the imported tip and at the first
// post-sync block. This is the import-driven counterpart to the in-memory
// bootstrap harness.
func TestBootstrapFromImportedChain(t *testing.T) {
	const limit = uint64(1 << 12)
	const tip = 319 // importing through 319 makes block 320 the first post-sync block
	const importedRoot = "0x2cfbd0b53605c60f097045dd80ad375d8fe6a900b70981810dd9f5c2ff252a75"
	const postSyncRoot = "0x1a9917dffec01d622ca2ffb90b8fabc9f29c3d4825660a5bde3c0efa255f536b"

	key, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(key.PublicKey)
	var chainConfig chain.Config
	require.NoError(t, copier.CopyWithOption(&chainConfig, chain.TestChainBerlinConfig, copier.Option{DeepCopy: true}))
	gspec := &types.Genesis{
		Config: &chainConfig,
		Alloc:  types.GenesisAlloc{from: {Balance: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)}},
	}
	require.NoError(t, eip8304.EnableDevnet(gspec, eip8304.DevnetIndexContractAddress))
	emt := execmoduletester.New(
		t,
		execmoduletester.WithGenesisSpec(gspec),
		execmoduletester.WithKey(key),
		execmoduletester.WithEip8304Finalize(eip8304.DevnetIndexContractAddress.Value(), limit),
	)
	signer := types.LatestSignerForChainID(emt.ChainConfig.ChainID)
	recipient := common.Address{0x42}
	generationStore := &parityTableStore{
		tables: make(map[eip8304.TableRef]eip8304.TableResult),
		hashes: make(map[uint64]common.Hash),
	}
	generationStore.seedGenesis(emt.Genesis.Hash(), limit)
	generationFinalizer := newGenerationFinalizer(generationStore, limit)
	chainPack, err := blockgen.GenerateChain(emt.ChainConfig, emt.Genesis, emt.Engine, emt.DB, tip, func(_ int, b *blockgen.BlockGen) {
		b.SetCoinbase(common.Address{1})
		tx, txErr := types.SignTx(types.NewTransaction(b.TxNonce(from), recipient, uint256.NewInt(1), params.TxGas, uint256.NewInt(1), nil), *signer, key)
		require.NoError(t, txErr)
		b.AddTx(tx)
	}, generationFinalizer)
	require.NoError(t, err)
	require.NoError(t, emt.InsertChain(chainPack))

	ctx := context.Background()
	var imported, postSync eip8304.BootstrapResult
	require.NoError(t, emt.DB.UpdateTemporal(ctx, func(tx kv.TemporalRwTx) error {
		data, err := eip8304.NewDBData(tx, importReceipts{ctx: ctx, emt: emt, tx: tx})
		if err != nil {
			return err
		}
		progress, err := eip8304.NewStageProgress(tx)
		if err != nil {
			return err
		}
		store, err := eip8304.NewHotTableStore(tx, data, progress, limit)
		if err != nil {
			return err
		}
		activeAlways := func(uint64) bool { return true }
		imported, err = eip8304.Bootstrap(tip, activeAlways, eip8304.NewResolver(store, limit), true)
		if err != nil {
			return err
		}
		postSync, err = eip8304.Bootstrap(tip+1, activeAlways, eip8304.NewResolver(store, limit), true)
		return err
	}))

	importedRef := eip8304.TableRef{FirstBlock: 0, TableSize: 256}
	require.Equal(t, 256, imported.LevelZero)
	require.Equal(t, 256, imported.Recorded)
	require.Len(t, imported.Tables, 1)
	require.Equal(t, importedRef, imported.Tables[0].Ref)
	require.Equal(t, common.HexToHash(importedRoot), imported.Tables[0].Root, "independently recorded import root")
	require.Equal(t, imported.Tables[0].Root, independentRootFromImportedChain(t, ctx, emt, limit, importedRef))

	postSyncRef := eip8304.TableRef{FirstBlock: 316, TableSize: 4}
	require.Equal(t, 4, postSync.LevelZero)
	require.Equal(t, 4, postSync.Recorded)
	require.Len(t, postSync.Tables, 1)
	require.Equal(t, postSyncRef, postSync.Tables[0].Ref)
	require.Equal(t, common.HexToHash(postSyncRoot), postSync.Tables[0].Root, "independently recorded first post-sync root")
	require.Equal(t, postSync.Tables[0].Root, independentRootFromImportedChain(t, ctx, emt, limit, postSyncRef))
}

type importReceipts struct {
	ctx context.Context
	emt *execmoduletester.ExecModuleTester
	tx  kv.TemporalTx
}

func (r importReceipts) Receipts(block uint64) (types.Receipts, error) {
	if block == 0 {
		return nil, nil
	}
	b, err := r.emt.BlockReader.BlockByNumber(r.ctx, r.tx, block)
	if err != nil {
		return nil, err
	}
	return r.emt.ReceiptsReader.GetReceipts(r.ctx, r.emt.ChainConfig, r.tx, b, eth.ReceiptsOpts{})
}
