package eip8304

import (
	"errors"
	"fmt"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/rawdb"
	"github.com/erigontech/erigon/execution/stagedsync/stages"
	"github.com/erigontech/erigon/execution/types"
)

// Node-side inputs for the EIP-8304 table store.
//
// The recovery paths need two things the protocol package cannot know about:
// which block is canonical at a height, and how far this node has executed.
// Both answers come from the chain database, and both must fail closed: a
// rebuild that cannot prove a block is canonical must report missing data
// rather than substitute an empty table (see CanonicalData).

var (
	ErrNilCanonicalDB   = errors.New("eip8304: nil canonical database")
	ErrNilReceiptSource = errors.New("eip8304: nil receipt source")
)

// DBHashes is the canonical-hash view of the chain database: the only canonical
// input the unwind and restart paths need. Invalidation reads no canonical data
// at all, and the restart check only compares stored hashes against these.
type DBHashes struct {
	db kv.Getter
}

var _ CanonicalChain = (*DBHashes)(nil)

// NewDBHashes returns the canonical-hash view over db.
func NewDBHashes(db kv.Getter) (*DBHashes, error) {
	if db == nil {
		return nil, ErrNilCanonicalDB
	}
	return &DBHashes{db: db}, nil
}

// CanonicalHash returns the canonical hash at a height, or an error wrapping
// ErrMissingBlockData when the database has none. A zero hash is reported as
// missing data: it is a legal-looking value that must not silently become a
// rebuild input.
func (d *DBHashes) CanonicalHash(block uint64) (common.Hash, error) {
	hash, err := rawdb.ReadCanonicalHash(d.db, block)
	if err != nil {
		return common.Hash{}, fmt.Errorf("canonical hash for block %d: %w", block, err)
	}
	if hash == (common.Hash{}) {
		return common.Hash{}, fmt.Errorf("%w: no canonical hash for block %d", ErrMissingBlockData, block)
	}
	return hash, nil
}

// StageProgress reports the highest fully executed block from the staged-sync
// progress table, which is what "executed" means everywhere else in the node.
type StageProgress struct {
	db kv.Getter
}

var _ ExecutionProgress = (*StageProgress)(nil)

// NewStageProgress returns the staged-sync view of execution progress.
func NewStageProgress(db kv.Getter) (*StageProgress, error) {
	if db == nil {
		return nil, ErrNilCanonicalDB
	}
	return &StageProgress{db: db}, nil
}

// ExecutedHeight returns the execution stage's progress.
func (p *StageProgress) ExecutedHeight() (uint64, error) {
	height, err := stages.GetStageProgress(p.db, stages.Execution)
	if err != nil {
		return 0, fmt.Errorf("execution stage progress: %w", err)
	}
	return height, nil
}

// ReceiptSource returns a block's receipts in transaction order, with the
// receipt fields the entry builder needs already derived.
//
// It is an interface on purpose: which sync input can serve receipts without
// --prune.include-receipts is still undecided, so the production source stays a
// dependency of the node-side adapter instead of being hardcoded here.
type ReceiptSource interface {
	Receipts(block uint64) (types.Receipts, error)
}

// DBData is CanonicalData over the chain database: canonical hashes from the
// canonical-hash index, level-0 entries rebuilt from a block's receipts. It is
// what a deterministic rebuild reads when a stored table is missing or rejected.
type DBData struct {
	*DBHashes
	receipts ReceiptSource
}

var _ CanonicalData = (*DBData)(nil)

// NewDBData returns the node's canonical data view. Both inputs are required:
// without a receipt source a rebuild could only fail, and failing later would
// hide the misconfiguration behind ErrMissingBlockData.
func NewDBData(db kv.Getter, receipts ReceiptSource) (*DBData, error) {
	hashes, err := NewDBHashes(db)
	if err != nil {
		return nil, err
	}
	if receipts == nil {
		return nil, ErrNilReceiptSource
	}
	return &DBData{DBHashes: hashes, receipts: receipts}, nil
}

// BlockEntries rebuilds a block's canonical level-0 entries. It fails closed
// when the block is not canonical or its receipts are unavailable, so a rebuild
// never turns missing input into an empty table.
func (d *DBData) BlockEntries(block uint64) (Entries, error) {
	if _, err := d.CanonicalHash(block); err != nil {
		return nil, err
	}
	var parent common.Hash
	if block > 0 {
		hash, err := d.CanonicalHash(block - 1)
		if err != nil {
			return nil, err
		}
		parent = hash
	}
	receipts, err := d.receipts.Receipts(block)
	if err != nil {
		return nil, fmt.Errorf("%w: receipts for block %d: %w", ErrMissingBlockData, block, err)
	}
	entries, err := BuildBlockEntriesFromReceipts(block, parent, receipts)
	if err != nil {
		return nil, err
	}
	entries.Sort()
	return entries, nil
}
