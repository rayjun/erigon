package stagedsync

import (
	"context"
	"errors"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/temporal/temporaltest"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/types"
)

func TestNewEip8304FinalizerFailsClosedOnMissingConfiguration(t *testing.T) {
	zero := uint64(0)
	scheduled := &chain.Config{Eip8304Time: &zero}

	_, err := (ExecuteBlockCfg{chainConfig: scheduled}).NewEip8304Finalizer(context.Background(), nil, nil, true)
	require.ErrorIs(t, err, ErrEip8304MissingConfig)

	_, err = (ExecuteBlockCfg{
		chainConfig:     scheduled,
		eip8304Finalize: &Eip8304FinalizeConfig{IndexContract: common.Address{1}},
	}).NewEip8304Finalizer(context.Background(), nil, nil, true)
	require.ErrorIs(t, err, ErrEip8304InvalidConfig)

	_, err = (ExecuteBlockCfg{
		chainConfig:     &chain.Config{},
		eip8304Finalize: &Eip8304FinalizeConfig{IndexContract: common.Address{1}, ListLimit: 1},
	}).NewEip8304Finalizer(context.Background(), nil, nil, true)
	require.ErrorIs(t, err, ErrEip8304NotScheduled)

	db := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	roTx, err := db.BeginTemporalRo(context.Background())
	require.NoError(t, err)
	defer roTx.Rollback()
	_, err = (ExecuteBlockCfg{
		chainConfig:     scheduled,
		eip8304Finalize: &Eip8304FinalizeConfig{IndexContract: common.Address{1}, ListLimit: 1},
	}).NewEip8304Finalizer(context.Background(), nil, roTx, true)
	require.ErrorIs(t, err, ErrEip8304RequiresRwTx)

	capturing, err := (ExecuteBlockCfg{
		chainConfig:     scheduled,
		eip8304Finalize: &Eip8304FinalizeConfig{IndexContract: common.Address{1}, ListLimit: 1},
	}).NewEip8304CapturingFinalizer(context.Background(), nil, roTx, nil)
	require.NoError(t, err)
	require.NotNil(t, capturing)
}

func TestEip8304ActivationUsesCurrentBlockTimeWithoutDatabaseLookup(t *testing.T) {
	activationTime := uint64(100)
	activation, err := eip8304Activation(&chain.Config{Eip8304Time: &activationTime}, context.Background(), nil, nil)
	require.NoError(t, err)

	active, err := activation(7, 7, activationTime-1)
	require.NoError(t, err)
	require.False(t, active)
	active, err = activation(7, 7, activationTime)
	require.NoError(t, err)
	require.True(t, active, "a building block's own timestamp gates activation")
}

func TestEip8304ActivationReadsHistoricalHeaderForHigherTables(t *testing.T) {
	activationTime := uint64(100)
	reader := &eip8304ActivationHeaderReaderFunc{fn: func(block uint64) (*types.Header, error) {
		switch block {
		case 4:
			return &types.Header{Number: *uint256.NewInt(4), Time: activationTime - 1}, nil
		case 5:
			return &types.Header{Number: *uint256.NewInt(5), Time: activationTime}, nil
		default:
			return nil, nil
		}
	}}
	activation, err := eip8304Activation(&chain.Config{Eip8304Time: &activationTime}, context.Background(), nil, reader)
	require.NoError(t, err)

	active, err := activation(4, 10, 0)
	require.NoError(t, err)
	require.False(t, active)
	active, err = activation(5, 10, 0)
	require.NoError(t, err)
	require.True(t, active)

	_, err = activation(6, 10, 0)
	require.ErrorIs(t, err, eip8304.ErrMissingBlockData)

	want := errors.New("header read failed")
	activation, err = eip8304Activation(&chain.Config{Eip8304Time: &activationTime}, context.Background(), nil, &eip8304ActivationHeaderReaderFunc{err: want})
	require.NoError(t, err)
	_, err = activation(4, 10, 0)
	require.ErrorIs(t, err, want)

	activation, err = eip8304Activation(&chain.Config{Eip8304Time: &activationTime}, context.Background(), nil, nil)
	require.NoError(t, err)
	_, err = activation(4, 10, 0)
	require.ErrorIs(t, err, eip8304.ErrMissingBlockData)
}

type eip8304ActivationHeaderReaderFunc struct {
	fn  func(block uint64) (*types.Header, error)
	err error
}

func (r *eip8304ActivationHeaderReaderFunc) HeaderByNumber(_ context.Context, _ kv.Getter, block uint64) (*types.Header, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.fn(block)
}

func TestValidateCachedReceiptsChecksCountAndRoot(t *testing.T) {
	tx := types.NewTransaction(0, common.Address{}, uint256.NewInt(0), 21_000, uint256.NewInt(1), nil)
	receipts := types.Receipts{{Type: tx.Type(), Status: types.ReceiptStatusSuccessful, CumulativeGasUsed: 21_000}}
	header := &types.Header{Number: *uint256.NewInt(1), ReceiptHash: types.DeriveSha(receipts)}
	block := types.NewBlock(header, []types.Transaction{tx}, nil, receipts, nil)

	require.NoError(t, validateCachedReceipts(block, receipts))
	require.ErrorContains(t, validateCachedReceipts(block, nil), "0/1 receipts")

	wrong := types.Receipts{{Type: tx.Type(), Status: types.ReceiptStatusFailed, CumulativeGasUsed: 21_000}}
	require.ErrorContains(t, validateCachedReceipts(block, wrong), "receipt cache root")
}
