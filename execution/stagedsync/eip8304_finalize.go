package stagedsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/dbservices"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/rawdb"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/protocol"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/protocol/rules"
	sharedreceipts "github.com/erigontech/erigon/execution/receipts"
	"github.com/erigontech/erigon/execution/state"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

var (
	ErrEip8304RequiresRwTx  = errors.New("eip8304: recording finalizer requires a writable transaction")
	ErrEip8304NotScheduled  = errors.New("eip8304: finalizer configured but the fork is not scheduled")
	ErrEip8304MissingConfig = errors.New("eip8304: fork is scheduled but the index contract/list limit are not configured")
	ErrEip8304InvalidConfig = errors.New("eip8304: list limit must be positive")
)

// Eip8304FinalizeConfig carries the two consensus parameters that remain
// external to the EIP: the index contract address and the SSZ list bound. They
// are passed explicitly by the node configuration; the execution stage never
// substitutes a hidden default.
type Eip8304FinalizeConfig struct {
	IndexContract common.Address
	ListLimit     uint64
}

type eip8304FinalizerMode uint8

const (
	eip8304FinalizeReadOnly eip8304FinalizerMode = iota
	eip8304FinalizeRecord
	eip8304FinalizeCapture
)

// NewEip8304Finalizer builds the block finalizer for the execution path. A
// nil config means the experimental fork is not wired, so the caller gets no
// hook and the pre-existing behavior is unchanged.
func (cfg ExecuteBlockCfg) NewEip8304Finalizer(ctx context.Context, doms *execctx.SharedDomains, tx kv.TemporalTx, record bool) (protocol.BlockFinalizer, error) {
	mode := eip8304FinalizeReadOnly
	if record {
		mode = eip8304FinalizeRecord
	}
	return cfg.newEip8304Finalizer(ctx, doms, tx, mode, nil)
}

// NewEip8304CapturingFinalizer computes the due tables on tx's goroutine and
// returns their records without writing them. The caller persists
// TakeTableResults() through the transaction that owns the block state.
func (cfg ExecuteBlockCfg) NewEip8304CapturingFinalizer(ctx context.Context, doms *execctx.SharedDomains, tx kv.TemporalTx, overlay *eip8304.TableOverlay) (*eip8304.Finalizer, error) {
	return cfg.newEip8304Finalizer(ctx, doms, tx, eip8304FinalizeCapture, overlay)
}

func (cfg ExecuteBlockCfg) newEip8304Finalizer(ctx context.Context, doms *execctx.SharedDomains, tx kv.TemporalTx, mode eip8304FinalizerMode, overlay *eip8304.TableOverlay) (*eip8304.Finalizer, error) {
	if err := cfg.validateEip8304FinalizeConfig(); err != nil {
		return nil, err
	}
	if cfg.eip8304Finalize == nil {
		return nil, nil
	}
	if mode == eip8304FinalizeRecord {
		if _, ok := tx.(kv.RwTx); !ok {
			return nil, ErrEip8304RequiresRwTx
		}
	}

	data, err := eip8304.NewDBData(tx, eip8304ReceiptSource{
		ctx:         ctx,
		tx:          tx,
		blockReader: cfg.blockReader,
		chainConfig: cfg.chainConfig,
		engine:      cfg.engine,
		doms:        doms,
	})
	if err != nil {
		return nil, err
	}
	progress, err := eip8304.NewStageProgress(tx)
	if err != nil {
		return nil, err
	}

	var store *eip8304.HotTableStore
	if mode == eip8304FinalizeRecord {
		store, err = eip8304.NewHotTableStore(tx.(kv.RwTx), data, progress, cfg.eip8304Finalize.ListLimit)
	} else {
		store, err = eip8304.NewHotTableReader(tx, data, progress, cfg.eip8304Finalize.ListLimit)
	}
	if err != nil {
		return nil, err
	}

	activeAt, err := eip8304Activation(cfg.chainConfig, ctx, tx, cfg.blockReader)
	if err != nil {
		return nil, err
	}
	indexContract := accounts.InternAddress(cfg.eip8304Finalize.IndexContract)
	switch mode {
	case eip8304FinalizeRecord:
		return eip8304.NewFinalizer(indexContract, cfg.eip8304Finalize.ListLimit, activeAt, store, true)
	case eip8304FinalizeCapture:
		return eip8304.NewCapturingFinalizer(indexContract, cfg.eip8304Finalize.ListLimit, activeAt, store, overlay)
	default:
		return eip8304.NewFinalizer(indexContract, cfg.eip8304Finalize.ListLimit, activeAt, store, false)
	}
}

// ValidateEip8304FinalizeConfig checks the node-level wiring without opening a
// table store. Parallel execution calls it before its goroutines start and
// builds the actual read-side finalizer later on the goroutine that owns its
// read transaction.
func (cfg ExecuteBlockCfg) ValidateEip8304FinalizeConfig() error {
	return cfg.validateEip8304FinalizeConfig()
}

func (cfg ExecuteBlockCfg) validateEip8304FinalizeConfig() error {
	if cfg.eip8304Finalize == nil {
		if cfg.chainConfig != nil && cfg.chainConfig.IsEip8304Scheduled() {
			return ErrEip8304MissingConfig
		}
		return nil
	}
	if cfg.chainConfig == nil || !cfg.chainConfig.IsEip8304Scheduled() {
		return ErrEip8304NotScheduled
	}
	if cfg.eip8304Finalize.ListLimit == 0 {
		return ErrEip8304InvalidConfig
	}
	return nil
}

// NewEip8304TableWriter builds the write-side store on the goroutine that owns
// tx. It writes records only; the canonical read source is the same data the
// finalizer used.
func (cfg ExecuteBlockCfg) NewEip8304TableWriter(ctx context.Context, doms *execctx.SharedDomains, tx kv.TemporalRwTx) (*eip8304.HotTableStore, error) {
	if err := cfg.validateEip8304FinalizeConfig(); err != nil {
		return nil, err
	}
	if cfg.eip8304Finalize == nil {
		return nil, nil
	}
	data, err := eip8304.NewDBData(tx, eip8304ReceiptSource{
		ctx:         ctx,
		tx:          tx,
		blockReader: cfg.blockReader,
		chainConfig: cfg.chainConfig,
		engine:      cfg.engine,
		doms:        doms,
	})
	if err != nil {
		return nil, err
	}
	progress, err := eip8304.NewStageProgress(tx)
	if err != nil {
		return nil, err
	}
	return eip8304.NewHotTableStore(tx, data, progress, cfg.eip8304Finalize.ListLimit)
}

type eip8304ActivationHeaderReader interface {
	HeaderByNumber(ctx context.Context, tx kv.Getter, blockNum uint64) (*types.Header, error)
}

func eip8304Activation(config *chain.Config, ctx context.Context, tx kv.Tx, blockReader eip8304ActivationHeaderReader) (func(firstBlock, currentBlock, currentTime uint64) (bool, error), error) {
	if config == nil || config.Eip8304Time == nil {
		return nil, ErrEip8304NotScheduled
	}
	if *config.Eip8304Time == 0 {
		return func(uint64, uint64, uint64) (bool, error) { return true, nil }, nil
	}
	return func(firstBlock, currentBlock, currentTime uint64) (bool, error) {
		if firstBlock == currentBlock {
			return config.IsEip8304(currentTime), nil
		}
		if blockReader == nil {
			return false, fmt.Errorf("%w: no block reader for activation block %d", eip8304.ErrMissingBlockData, firstBlock)
		}
		header, err := blockReader.HeaderByNumber(ctx, tx, firstBlock)
		if err != nil {
			return false, fmt.Errorf("read activation header %d: %w", firstBlock, err)
		}
		if header == nil {
			return false, fmt.Errorf("%w: no header for activation block %d", eip8304.ErrMissingBlockData, firstBlock)
		}
		return config.IsEip8304(header.Time), nil
	}, nil
}

type eip8304ReceiptSource struct {
	ctx         context.Context
	tx          kv.TemporalTx
	blockReader dbservices.FullBlockReader
	chainConfig *chain.Config
	engine      rules.EngineReader
	doms        *execctx.SharedDomains
}

var _ eip8304.ReceiptSource = eip8304ReceiptSource{}

func (s eip8304ReceiptSource) Receipts(block uint64) (types.Receipts, error) {
	if block == 0 {
		return nil, nil
	}
	if s.blockReader == nil {
		return nil, fmt.Errorf("%w: receipt source has no block reader", eip8304.ErrMissingBlockData)
	}
	b, err := s.blockReader.BlockByNumber(s.ctx, s.tx, block)
	if err != nil {
		return nil, fmt.Errorf("%w: read block %d: %w", eip8304.ErrMissingBlockData, block, err)
	}
	if b == nil {
		return nil, fmt.Errorf("%w: block %d not found", eip8304.ErrMissingBlockData, block)
	}
	if len(b.Transactions()) == 0 {
		return nil, nil
	}
	receipts, cacheErr := rawdb.ReadReceiptsCacheV2(s.tx, b, s.blockReader.TxnumReader())
	if cacheErr == nil {
		if err := validateCachedReceipts(b, receipts); err == nil {
			return receipts, nil
		} else {
			cacheErr = err
		}
	}

	replayed, replayErr := s.replayReceipts(b)
	if replayErr != nil {
		return nil, fmt.Errorf("%w: receipts for block %d: cache: %v; state-history replay: %w", eip8304.ErrMissingBlockData, block, cacheErr, replayErr)
	}
	return replayed, nil
}

func validateCachedReceipts(b *types.Block, receipts types.Receipts) error {
	if len(receipts) != len(b.Transactions()) {
		return fmt.Errorf("block %d has %d/%d receipts in its receipt cache", b.NumberU64(), len(receipts), len(b.Transactions()))
	}
	got, want := types.DeriveSha(receipts), b.ReceiptHash()
	if got != want {
		return fmt.Errorf("receipt cache root %s, header has %s", got, want)
	}
	return nil
}

// replayReceipts is the fail-closed fallback for nodes that do not persist the
// receipt cache. It replays the block against state history and checks the
// derived receipts against the header before returning them. A pruned history
// window or a mismatch is reported as missing data; it is never replaced with
// an empty receipt list.
func (s eip8304ReceiptSource) replayReceipts(b *types.Block) (types.Receipts, error) {
	if s.chainConfig == nil || s.engine == nil || s.blockReader == nil {
		return nil, errors.New("receipt replay dependencies are incomplete")
	}
	txns := b.Transactions()
	if len(txns) == 0 {
		return nil, nil
	}

	minTxNum, err := s.blockReader.TxnumReader().Min(s.ctx, s.tx, b.NumberU64())
	if err != nil {
		return nil, fmt.Errorf("read first transaction number: %w", err)
	}
	if minTxNum == ^uint64(0) {
		return nil, errors.New("first transaction number overflows")
	}
	// Min is the block's block-begin/system txNum. User receipts start at the
	// next txNum, matching the RPC receipt generator's as-of reader. Keep this
	// explicit: a block-begin write must not leak into the replay's first tx.
	readTxNum := minTxNum + 1
	if minHistoryTxNum := state.StateHistoryStartTxNum(s.tx); readTxNum < minHistoryTxNum {
		return nil, fmt.Errorf("state history starts at tx %d, block %d needs %d", minHistoryTxNum, b.NumberU64(), readTxNum)
	}

	var reader *state.HistoryReaderV3
	if s.doms != nil {
		reader = state.NewHistoryReaderV3WithSharedDomains(s.tx, s.doms, readTxNum)
	} else {
		reader = state.NewHistoryReaderV3(s.tx, readTxNum)
	}
	ibs := state.New(reader)
	defer ibs.Release(false)

	gp := new(protocol.GasPool).AddGas(b.GasLimit()).AddBlobGas(s.chainConfig.GetMaxBlobGasPerBlock(b.Time()))
	getHeader := func(hash common.Hash, number uint64) (*types.Header, error) {
		return s.blockReader.Header(s.ctx, s.tx, hash, number)
	}
	derived, err := sharedreceipts.DeriveBlockReceipts(s.ctx, s.chainConfig, s.engine, b.HeaderNoCopy(), txns, ibs, gp, getHeader)
	if err != nil {
		return nil, err
	}
	sharedreceipts.DeriveFields(derived, b.Hash())
	if len(derived) != len(txns) {
		return nil, fmt.Errorf("derived %d/%d receipts", len(derived), len(txns))
	}
	if got, want := types.DeriveSha(derived), b.ReceiptHash(); got != want {
		return nil, fmt.Errorf("derived receipts root %s, header has %s", got, want)
	}
	return derived, nil
}
