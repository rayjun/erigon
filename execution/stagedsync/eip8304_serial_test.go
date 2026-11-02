package stagedsync

import (
	"context"
	"testing"

	"github.com/holiman/uint256"
	"github.com/jinzhu/copier"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/snapshotsync/freezeblocks"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/exec"
	"github.com/erigontech/erigon/execution/protocol/params"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/protocol/rules/ethash"
	"github.com/erigontech/erigon/execution/state"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/vm"
)

// TestSerialPartialBlockFailsClosedWhenEip8304IsActive drives the serial
// executor's real block-end path with a resumed block whose prefix receipts
// cannot be rebuilt. The EIP-8304 gate must reject the block before
// engine.Finalize or the finalizer can publish a table from truncated receipts.
func TestSerialPartialBlockFailsClosedWhenEip8304IsActive(t *testing.T) {
	db := newResumeTestDB(t)
	logger := log.New()
	dirs := datadir.New(t.TempDir())

	zero := uint64(0)
	var config chain.Config
	require.NoError(t, copier.CopyWithOption(&config, chain.TestChainBerlinConfig, copier.Option{DeepCopy: true}))
	config.Eip8304Time = &zero
	tx0 := signSelfSendTx(t, 0, 0, 1, params.TxGas, &config, 0)
	tx1 := signSelfSendTx(t, 1, 0, 1, params.TxGas, &config, 0)

	roTx, err := db.BeginTemporalRo(context.Background())
	require.NoError(t, err)
	t.Cleanup(roTx.Rollback)
	doms, err := execctx.NewSharedDomains(context.Background(), roTx, logger)
	require.NoError(t, err)
	t.Cleanup(doms.Close)

	blockReader := freezeblocks.NewBlockReader(db.(freezeblocks.HasBlockFiles).DebugBlockFiles(), nil)
	rs := state.NewStateV3Buffered(state.NewStateV3(doms, false, logger))
	engine := ethash.NewFaker()
	worker := exec.NewWorker(context.Background(), false, exec.NewWorkerMetrics(), db, nil, blockReader, &config, nil, nil, engine, dirs, logger)
	require.NoError(t, worker.ResetState(rs, roTx, nil, nil, nil))

	se := &serialExecutor{
		txExecutor: txExecutor{
			cfg: ExecuteBlockCfg{
				chainConfig: &config,
				db:          db,
				blockReader: blockReader,
				engine:      engine,
				dirs:        dirs,
				vmConfig:    &vm.Config{},
			},
			doms:             doms,
			rs:               rs,
			applyTx:          roTx,
			logger:           logger,
			eip8304Finalizer: &serialFailClosedFinalizer{t: t},
		},
		worker: worker,
	}

	header := &types.Header{
		Number:   *uint256.NewInt(1),
		GasLimit: 10_000_000,
	}
	// Only the block-end marker is supplied. That is enough to reach the
	// finalize gate while startTxIndex=2 makes the block explicitly partial.
	task := &exec.TxTask{
		Header:  header,
		TxNum:   2,
		TxIndex: 2,
		Config:  &config,
		Txs:     types.Transactions{tx0, tx1},
	}
	_, err = se.executeBlock(context.Background(), []exec.Task{task}, false, false)
	require.ErrorIs(t, err, rules.ErrInvalidBlock)
	require.ErrorContains(t, err, "EIP-8304 needs complete receipts for partial block 1")
}

type serialFailClosedFinalizer struct {
	t *testing.T
}

func (f *serialFailClosedFinalizer) Finalize(uint64, uint64, common.Hash, common.Hash, types.Receipts, rules.SystemCall) error {
	f.t.Fatal("EIP-8304 finalizer ran despite incomplete prior receipts")
	return nil
}
