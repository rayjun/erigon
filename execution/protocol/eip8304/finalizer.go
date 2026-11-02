package eip8304

import (
	"errors"
	"fmt"
	"sync"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

var (
	ErrNilFinalizerActivation = errors.New("eip8304: nil finalizer activation predicate")
	ErrMissingBlockHash       = errors.New("eip8304: finalizer has no block hash to bind the table to")
)

// Finalizer applies the EIP-8304 index tables due at the end of a block. It is
// an explicit dependency of the execution paths that persist state: callers
// that only replay or trace a block pass nil and get no system calls or store
// writes.
//
// Record is false for block building, where the block hash is not known until
// after the finalizer has run. The same roots are published to the index
// contract, but table records are left for the execution path that validates
// (or later re-executes) the sealed block.
type Finalizer struct {
	IndexContract accounts.Address
	ListLimit     uint64
	// ActiveAt reports whether the fork was active at firstBlock. The current
	// block's header may not be in the database yet (block building), so the
	// finalizer also passes the current block number and timestamp and the
	// caller uses those instead of a database lookup when both refer to the
	// in-flight block.
	ActiveAt func(firstBlock, currentBlock, currentTime uint64) (bool, error)
	resolver *Resolver
	record   bool
	captured *captureTableStore
}

// NewFinalizer builds a finalizer over store. `record` says whether the block
// hash passed to Finalize is final and should be persisted in the hot store.
// For block building it is false; for canonical execution it is true.
func NewFinalizer(
	indexContract accounts.Address,
	listLimit uint64,
	activeAt func(firstBlock, currentBlock, currentTime uint64) (bool, error),
	store TableStore,
	record bool,
) (*Finalizer, error) {
	if activeAt == nil {
		return nil, ErrNilFinalizerActivation
	}
	if store == nil {
		return nil, ErrNilTableStore
	}
	if !record {
		store = noRecordTableStore{TableStore: store}
	}
	return newFinalizer(indexContract, listLimit, activeAt, store, record, nil)
}

// NewCapturingFinalizer builds a finalizer that computes and applies the due
// tables but buffers their records instead of writing them to the store. The
// caller must persist TakeTableResults() in the transaction that owns the
// block's state transition. This is used by parallel execution, where the
// block-finalize goroutine does not own the thread-bound RwTx.
func NewCapturingFinalizer(
	indexContract accounts.Address,
	listLimit uint64,
	activeAt func(firstBlock, currentBlock, currentTime uint64) (bool, error),
	store TableStore,
	overlay *TableOverlay,
) (*Finalizer, error) {
	if store == nil {
		return nil, ErrNilTableStore
	}
	if overlay != nil {
		store = overlayTableStore{TableStore: store, overlay: overlay}
	}
	captured := &captureTableStore{TableStore: store, listLimit: listLimit}
	return newFinalizer(indexContract, listLimit, activeAt, captured, false, captured)
}

func newFinalizer(
	indexContract accounts.Address,
	listLimit uint64,
	activeAt func(firstBlock, currentBlock, currentTime uint64) (bool, error),
	store TableStore,
	record bool,
	captured *captureTableStore,
) (*Finalizer, error) {
	if activeAt == nil {
		return nil, ErrNilFinalizerActivation
	}
	return &Finalizer{
		IndexContract: indexContract,
		ListLimit:     listLimit,
		ActiveAt:      activeAt,
		resolver:      NewResolver(store, listLimit),
		record:        record,
		captured:      captured,
	}, nil
}

// TakeTableResults returns the records buffered since the previous call. It is
// nil for a finalizer that writes through or discards records.
func (f *Finalizer) TakeTableResults() []TableResult {
	if f == nil || f.captured == nil {
		return nil
	}
	return f.captured.take()
}

// Finalize computes and applies every table due at block. The caller must run
// it after the engine's post-transaction finalization and before the block's
// write set is committed, so the index-contract writes belong to the same state
// transition as the transactions.
func (f *Finalizer) Finalize(
	block uint64,
	blockTime uint64,
	blockHash common.Hash,
	parentBlockHash common.Hash,
	receipts types.Receipts,
	syscall rules.SystemCall,
) error {
	if f == nil {
		return nil
	}
	if f.ActiveAt == nil {
		return ErrNilFinalizerActivation
	}
	if f.resolver == nil {
		return ErrNilTableResolver
	}
	if f.record && blockHash == (common.Hash{}) {
		return fmt.Errorf("%w: block %d", ErrMissingBlockHash, block)
	}

	activeByFirstBlock := make(map[uint64]bool)
	for _, ref := range DueTables(block) {
		if _, ok := activeByFirstBlock[ref.FirstBlock]; ok {
			continue
		}
		active, err := f.ActiveAt(ref.FirstBlock, block, blockTime)
		if err != nil {
			return fmt.Errorf("activation at first block %d: %w", ref.FirstBlock, err)
		}
		activeByFirstBlock[ref.FirstBlock] = active
	}

	activeAt := func(firstBlock uint64) bool {
		return activeByFirstBlock[firstBlock]
	}
	if _, err := ApplyDueTables(block, blockHash, parentBlockHash, receipts, f.IndexContract, activeAt, f.resolver, syscall); err != nil {
		return fmt.Errorf("finalize EIP-8304 tables at block %d: %w", block, err)
	}
	return nil
}

// noRecordTableStore keeps the resolver's read path intact while making table
// publication a no-op. It is used while building a block, before the header
// hash exists.
type noRecordTableStore struct {
	TableStore
}

func (s noRecordTableStore) PutTable(TableResult) error { return nil }

var _ TableStore = noRecordTableStore{}

// captureTableStore keeps the read path of the wrapped store but buffers table
// records for the transaction owner to persist later. It applies the same write
// validation as HotTableStore, so a malformed result still fails the block
// before it can be handed to another goroutine.
type captureTableStore struct {
	TableStore
	listLimit uint64
	results   []TableResult
}

func (s *captureTableStore) PutTable(result TableResult) error {
	if err := validateTableResult(result, s.listLimit); err != nil {
		return err
	}
	s.results = append(s.results, result)
	return nil
}

func (s *captureTableStore) take() []TableResult {
	results := s.results
	s.results = nil
	return results
}

var _ TableStore = (*captureTableStore)(nil)

// TableOverlay exposes tables computed earlier in the same parallel batch.
// Parallel execution computes on one read transaction and records on the
// transaction owner's goroutine, so the read transaction cannot see a table
// written even one block earlier in the batch. The overlay closes that gap
// without writing through a thread-bound transaction.
type TableOverlay struct {
	mu     sync.RWMutex
	tables map[TableRef]TableResult
	hashes map[uint64]common.Hash
}

func NewTableOverlay() *TableOverlay {
	return &TableOverlay{
		tables: make(map[TableRef]TableResult),
		hashes: make(map[uint64]common.Hash),
	}
}

func (o *TableOverlay) Remember(result TableResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tables[result.Ref] = result
	for i, hash := range result.BlockHashes {
		o.hashes[result.Ref.FirstBlock+uint64(i)] = hash
	}
	// A due table can only merge inputs from the MaxTableLookback window below
	// the current block. Keep the overlay bounded even in a very long batch.
	newest := result.Ref.FirstBlock + result.Ref.TableSize - 1
	if newest <= MaxTableLookback {
		return
	}
	keepFrom := newest - MaxTableLookback
	for ref := range o.tables {
		if ref.FirstBlock+ref.TableSize-1 < keepFrom {
			delete(o.tables, ref)
		}
	}
	for block := range o.hashes {
		if block < keepFrom {
			delete(o.hashes, block)
		}
	}
}

func (o *TableOverlay) table(ref TableRef) (TableResult, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	result, ok := o.tables[ref]
	return result, ok
}

func (o *TableOverlay) canonicalHash(block uint64) (common.Hash, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	hash, ok := o.hashes[block]
	return hash, ok
}

type overlayTableStore struct {
	TableStore
	overlay *TableOverlay
}

func (s overlayTableStore) GetTable(ref TableRef) (TableResult, bool, error) {
	if result, ok := s.overlay.table(ref); ok {
		return result, true, nil
	}
	return s.TableStore.GetTable(ref)
}

// CanonicalHash serves hashes from tables computed earlier in this batch.
// VerifyTable checks the hashes carried by an overlay record, but the batch's
// canonical hashes are not visible through the exec loop's read transaction
// until the apply loop commits them. Falling through to the underlying store
// here would reject a perfectly valid just-computed table and force a rebuild
// from receipts that are equally unavailable.
func (s overlayTableStore) CanonicalHash(block uint64) (common.Hash, error) {
	if hash, ok := s.overlay.canonicalHash(block); ok {
		return hash, nil
	}
	return s.TableStore.CanonicalHash(block)
}

var _ TableStore = overlayTableStore{}
