package stagedsync_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/rawdb"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/db/state/statecfg"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/execmodule/execmoduletester"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/protocol/params"
	"github.com/erigontech/erigon/execution/stagedsync"
	"github.com/erigontech/erigon/execution/tests/blockgen"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/p2p/protocols/eth"
)

func TestEip8304ReceiptSourceReplaysWhenCacheDisabled(t *testing.T) {
	key, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &types.Genesis{
		Config: chain.TestChainBerlinConfig,
		Alloc:  types.GenesisAlloc{from: {Balance: new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)}},
	}
	emt := execmoduletester.New(
		t,
		execmoduletester.WithGenesisSpec(gspec),
		execmoduletester.WithKey(key),
		execmoduletester.WithReceiptsCache(false),
	)
	signer := types.LatestSignerForChainID(emt.ChainConfig.ChainID)
	chainPack, err := blockgen.GenerateChain(emt.ChainConfig, emt.Genesis, emt.Engine, emt.DB, 1, func(_ int, b *blockgen.BlockGen) {
		tx, txErr := types.SignTx(types.NewTransaction(b.TxNonce(from), common.Address{0x42}, uint256.NewInt(1), params.TxGas, uint256.NewInt(1), nil), *signer, key)
		require.NoError(t, txErr)
		b.AddTx(tx)
	})
	require.NoError(t, err)
	require.NoError(t, emt.InsertChain(chainPack))

	ctx := context.Background()
	roTx, err := emt.DB.BeginTemporalRo(ctx)
	require.NoError(t, err)
	defer roTx.Rollback()

	block, err := emt.BlockReader.BlockByNumber(ctx, roTx, 1)
	require.NoError(t, err)
	require.NotNil(t, block)
	cached, err := rawdb.ReadReceiptsCacheV2(roTx, block, emt.BlockReader.TxnumReader())
	require.NoError(t, err)
	require.Empty(t, cached, "the test must exercise the no-cache path")

	want, err := emt.ReceiptsReader.GetReceipts(ctx, emt.ChainConfig, roTx, block, eth.ReceiptsOpts{})
	require.NoError(t, err)
	require.Len(t, want, 1)

	source := stagedsync.NewEip8304ReceiptSourceForTest(ctx, roTx, emt.BlockReader, emt.ChainConfig, emt.Engine, nil)
	got, err := source.Receipts(1)
	require.NoError(t, err)
	require.Equal(t, want, got, "the replay fallback must reproduce the canonical receipts")
	require.Equal(t, block.ReceiptHash(), types.DeriveSha(got), "replayed receipts must authenticate the canonical header")

	prunedTx := historyStartOverrideTx{TemporalTx: roTx, start: ^uint64(0)}
	pruned := stagedsync.NewEip8304ReceiptSourceForTest(ctx, prunedTx, emt.BlockReader, emt.ChainConfig, emt.Engine, nil)
	_, err = pruned.Receipts(1)
	require.ErrorIs(t, err, eip8304.ErrMissingBlockData)
	require.ErrorContains(t, err, "state history starts at tx")

	noReader := stagedsync.NewEip8304ReceiptSourceForTest(ctx, roTx, nil, nil, nil, nil)
	_, err = noReader.Receipts(1)
	require.ErrorIs(t, err, eip8304.ErrMissingBlockData)
	require.ErrorContains(t, err, "no block reader")
}

func TestEip8304ReceiptSourceReplaysAPoisonedCache(t *testing.T) {
	key, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &types.Genesis{
		Config: chain.TestChainBerlinConfig,
		Alloc:  types.GenesisAlloc{from: {Balance: new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)}},
	}
	statecfg.EnableHistoricalRCache()
	emt := execmoduletester.New(
		t,
		execmoduletester.WithGenesisSpec(gspec),
		execmoduletester.WithKey(key),
		execmoduletester.WithReceiptsCache(false),
	)
	signer := types.LatestSignerForChainID(emt.ChainConfig.ChainID)
	chainPack, err := blockgen.GenerateChain(emt.ChainConfig, emt.Genesis, emt.Engine, emt.DB, 1, func(_ int, b *blockgen.BlockGen) {
		tx, txErr := types.SignTx(types.NewTransaction(b.TxNonce(from), common.Address{0x42}, uint256.NewInt(1), params.TxGas, uint256.NewInt(1), nil), *signer, key)
		require.NoError(t, txErr)
		b.AddTx(tx)
	})
	require.NoError(t, err)
	require.NoError(t, emt.InsertChain(chainPack))

	ctx := context.Background()
	roTx, err := emt.DB.BeginTemporalRo(ctx)
	require.NoError(t, err)
	block, err := emt.BlockReader.BlockByNumber(ctx, roTx, 1)
	require.NoError(t, err)
	require.NotNil(t, block)
	want, err := emt.ReceiptsReader.GetReceipts(ctx, emt.ChainConfig, roTx, block, eth.ReceiptsOpts{})
	require.NoError(t, err)
	require.Len(t, want, 1)
	roTx.Rollback()

	rwTx, err := emt.DB.BeginTemporalRw(ctx)
	require.NoError(t, err)
	defer rwTx.Rollback()
	minTxNum, err := emt.BlockReader.TxnumReader().Min(ctx, rwTx, 1)
	require.NoError(t, err)
	maxTxNum, err := emt.BlockReader.TxnumReader().Max(ctx, rwTx, 1)
	require.NoError(t, err)
	poisoned := *want[0]
	poisoned.Status = types.ReceiptStatusFailed
	poisoned.CumulativeGasUsed += 1
	poisoned.GasUsed += 1

	sd, err := execctx.NewSharedDomains(ctx, rwTx, log.New())
	require.NoError(t, err)
	defer sd.Close()
	require.NoError(t, rawdb.WriteReceiptCacheV2(sd.AsPutDel(rwTx), nil, minTxNum))
	require.NoError(t, rawdb.WriteReceiptCacheV2(sd.AsPutDel(rwTx), &poisoned, minTxNum+1))
	require.NoError(t, rawdb.WriteReceiptCacheV2(sd.AsPutDel(rwTx), nil, maxTxNum))
	_, err = sd.ComputeCommitment(ctx, rwTx, true, 1, maxTxNum, "test-flush-poisoned-cache", nil)
	require.NoError(t, err)
	require.NoError(t, sd.Flush(ctx, rwTx))
	require.NoError(t, rwTx.Commit())

	roTx, err = emt.DB.BeginTemporalRo(ctx)
	require.NoError(t, err)
	defer roTx.Rollback()
	cached, err := rawdb.ReadReceiptsCacheV2(roTx, block, emt.BlockReader.TxnumReader())
	require.NoError(t, err)
	require.Len(t, cached, 1)
	require.NotEqual(t, block.ReceiptHash(), types.DeriveSha(cached))
	require.NotEqual(t, want[0].CumulativeGasUsed, cached[0].CumulativeGasUsed, "the test must really poison RCacheV2")

	source := stagedsync.NewEip8304ReceiptSourceForTest(ctx, roTx, emt.BlockReader, emt.ChainConfig, emt.Engine, nil)
	got, err := source.Receipts(1)
	require.NoError(t, err)
	require.Equal(t, want, got, "a cache row with the right count but the wrong root must be replayed")
	require.Equal(t, block.ReceiptHash(), types.DeriveSha(got))
}

type historyStartOverrideTx struct {
	kv.TemporalTx
	start uint64
}

func (tx historyStartOverrideTx) Debug() kv.TemporalDebugTx {
	return historyStartOverrideDebug{TemporalDebugTx: tx.TemporalTx.Debug(), start: tx.start}
}

type historyStartOverrideDebug struct {
	kv.TemporalDebugTx
	start uint64
}

func (d historyStartOverrideDebug) HistoryStartFrom(kv.Domain) uint64 { return d.start }
