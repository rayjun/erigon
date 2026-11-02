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
	"github.com/erigontech/erigon/execution/builder"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/execmodule/execmoduletester"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/protocol/params"
	"github.com/erigontech/erigon/execution/tests/blockgen"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/vm"
	"github.com/erigontech/erigon/txnprovider"
)

// TestBuildingExecutionParityForFinalizedTables imports blocks 1..3, then
// builds block 4 through the production payload builder with the same
// transaction and attributes as the sealed block generated for the import.
// Building runs the due L0(3) table and the delayed (0,4) merge, so comparing
// the resulting header root covers FinalizeBlockExecution(isMining=true),
// finalizer system calls, and the builder's commitment calculation end to end.
func TestBuildingExecutionParityForFinalizedTables(t *testing.T) {
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

	builderNode := execmoduletester.New(
		t,
		execmoduletester.WithGenesisSpec(gspec),
		execmoduletester.WithKey(key),
		execmoduletester.WithEip8304Finalize(eip8304.DevnetIndexContractAddress.Value(), listLimit),
	)
	signer := types.LatestSignerForChainID(builderNode.ChainConfig.ChainID)

	generationStore := &parityTableStore{
		tables: make(map[eip8304.TableRef]eip8304.TableResult),
		hashes: make(map[uint64]common.Hash),
	}
	generationStore.seedGenesis(builderNode.Genesis.Hash(), listLimit)
	generationFinalizer := newGenerationFinalizer(generationStore, listLimit)

	chainPack, err := blockgen.GenerateChain(builderNode.ChainConfig, builderNode.Genesis, builderNode.Engine, builderNode.DB, 4, func(_ int, b *blockgen.BlockGen) {
		b.SetCoinbase(common.Address{1})
		tx, txErr := types.SignTx(types.NewTransaction(b.TxNonce(from), contract, uint256.NewInt(0), params.TxGas+100_000, uint256.NewInt(1), nil), *signer, key)
		require.NoError(t, txErr)
		b.AddTx(tx)
	}, generationFinalizer)
	require.NoError(t, err)

	// Import only the prefix. Block 4 is then produced by the builder from the
	// same parent state the generator used for its fourth block.
	prefix := &blockgen.ChainPack{
		Headers:          chainPack.Headers[:3],
		Blocks:           chainPack.Blocks[:3],
		Receipts:         chainPack.Receipts[:3],
		BlockAccessLists: chainPack.BlockAccessLists[:3],
		TopBlock:         chainPack.Blocks[2],
	}
	require.NoError(t, builderNode.InsertChain(prefix))
	tablesBefore := readEip8304Tables(t, builderNode)

	sealed := chainPack.Blocks[3]
	targetGasLimit := sealed.GasLimit()
	buildParams := &builder.Parameters{
		ParentHash:            sealed.ParentHash(),
		Timestamp:             sealed.Time(),
		PrevRandao:            sealed.MixDigest(),
		SuggestedFeeRecipient: sealed.Coinbase(),
		ParentBeaconBlockRoot: sealed.Header().ParentBeaconBlockRoot,
		TargetGasLimit:        &targetGasLimit,
		CustomTxnProvider:     &singleTxnProvider{txns: sealed.Transactions()},
		ExtraData:             sealed.Extra(),
	}

	ctx := context.Background()
	started, err := builderNode.ExecModule.AssembleBlock(ctx, buildParams)
	require.NoError(t, err)
	require.False(t, started.Busy)
	require.NotZero(t, started.PayloadID)

	assembled, err := builderNode.ExecModule.GetAssembledBlock(ctx, started.PayloadID)
	require.NoError(t, err)
	require.False(t, assembled.Busy)
	require.NotNil(t, assembled.Block)
	require.NotNil(t, assembled.Block.Block)
	require.Equal(t, len(sealed.Transactions()), len(assembled.Block.Block.Transactions()))
	require.Equal(t, sealed.Root(), assembled.Block.Block.Root(), "builder and sealed import must compute the same post-finalizer state root")
	require.Equal(t, tablesBefore, readEip8304Tables(t, builderNode), "block building must publish roots without persisting table records")

	reimportNode := execmoduletester.New(
		t,
		execmoduletester.WithGenesisSpec(gspec),
		execmoduletester.WithKey(key),
		execmoduletester.WithEip8304Finalize(eip8304.DevnetIndexContractAddress.Value(), listLimit),
	)
	require.NoError(t, reimportNode.InsertChain(prefix))
	require.Len(t, readEip8304Tables(t, reimportNode), 3, "the imported prefix has one L0 record per block")
	assembledPack := &blockgen.ChainPack{
		Headers:  []*types.Header{assembled.Block.Block.HeaderNoCopy()},
		Blocks:   []*types.Block{assembled.Block.Block},
		Receipts: []types.Receipts{assembled.Block.Receipts},
		TopBlock: assembled.Block.Block,
	}
	require.NoError(t, reimportNode.InsertChain(assembledPack), "a block built without recording must be importable")
	require.Len(t, readEip8304Tables(t, reimportNode), 5, "import must record L0(4) and the delayed (0,4) table")
}

func readEip8304Tables(t *testing.T, emt *execmoduletester.ExecModuleTester) map[string][]byte {
	t.Helper()
	tx, err := emt.DB.BeginTemporalRo(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()

	cursor, err := tx.Cursor(kv.Eip8304Tables)
	require.NoError(t, err)
	defer cursor.Close()
	tables := make(map[string][]byte)
	for key, value, err := cursor.First(); key != nil; key, value, err = cursor.Next() {
		require.NoError(t, err)
		tables[string(append([]byte(nil), key...))] = append([]byte(nil), value...)
	}
	return tables
}

type singleTxnProvider struct {
	txns types.Transactions
	done bool
}

var _ txnprovider.TxnProvider = (*singleTxnProvider)(nil)

func (p *singleTxnProvider) ProvideTxns(_ context.Context, opts ...txnprovider.ProvideOption) ([]types.Transaction, error) {
	if p.done {
		return nil, nil
	}
	options := txnprovider.ApplyProvideOptions(opts...)
	txns := p.txns
	if options.Amount < len(txns) {
		txns = txns[:options.Amount]
	}
	p.done = true
	return txns, nil
}
