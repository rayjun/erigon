package eip8304

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
)

// The hot table store persists computed tables in the kv.Eip8304Tables bucket so
// a node can reuse them after a restart without re-deriving them from receipts.
//
// Two properties are not visible in the layout on their own: a record's identity
// is its ref *and* the canonical hashes of its covered range, and only a complete
// record is ever written, so a torn write can only ever read back as damage.
// Record layout and rationale: docs/eip8304/hot-table-store.md.

// Hot store record layout constants.
const (
	hotStoreKeyVersion = 1

	hotStoreMagic0 = 0xE8
	hotStoreMagic1 = 0x30
	hotStoreMagic2 = 0x04
	hotStoreMagic3 = 0x01

	hotStoreKeySize = 1 + 8 + 8 // version, first_block, table_size

	// Value layout: magic, record version, state, reserved, then the fixed
	// width fields, the covered hashes, the entries and the CRC32 trailer.
	hotStoreVersionOffset    = 4
	hotStoreStateOffset      = 5
	hotStoreEntryCountOffset = 8
	hotStoreRootOffset       = 16
	hotStoreHashCountOffset  = 48
	hotStoreHeader           = 56
	hotStoreTrailer          = 4 // crc32
)

// hotStoreStateComplete marks a record whose writer finished encoding it. A
// record with any other state is treated as a partial write.
const hotStoreStateComplete = 1

// Hot store errors. A damaged record is reported, not silently treated as a
// miss: callers decide to rebuild, and the contradiction stays visible in
// metrics and tests.
var (
	ErrTableRecordCorrupt  = errors.New("eip8304: corrupt table store record")
	ErrTableRecordVersion  = errors.New("eip8304: unsupported table store record version")
	ErrTableRecordPartial  = errors.New("eip8304: partial table store record")
	ErrTableRecordTooLarge = errors.New("eip8304: table store record exceeds the SSZ list limit")
	ErrTableRecordProgress = errors.New("eip8304: table store record covers blocks above the executed height")

	ErrNilTableStoreTx    = errors.New("eip8304: nil table store transaction")
	ErrNilCanonicalData   = errors.New("eip8304: nil canonical data source")
	ErrNilProgressSource  = errors.New("eip8304: nil execution progress source")
	ErrReadOnlyTableStore = errors.New("eip8304: read-only table store")
)

// ExecutionProgress reports the highest block this node has fully executed. A
// stored table is only usable when its whole covered range is at or below that
// height; ahead of it, the record cannot be canonical yet.
type ExecutionProgress interface {
	ExecutedHeight() (uint64, error)
}

// ProgressFunc adapts a function to ExecutionProgress.
type ProgressFunc func() (uint64, error)

func (f ProgressFunc) ExecutedHeight() (uint64, error) { return f() }

// HotTableStore is a TableStore backed by a database bucket: canonical data
// (block entries and block hashes) comes from another source, while computed
// tables are read from and written to `tx`.
//
// The store writes into the caller's transaction, so a table becomes canonical
// together with the state change it belongs to: if the block is aborted, the
// record is rolled back with everything else.
type HotTableStore struct {
	reads     kv.Tx
	writes    kv.RwTx
	canonical CanonicalData
	progress  ExecutionProgress
	listLimit uint64
	// pending makes records written in this transaction visible immediately.
	// The staged-sync execution progress advances only at the batch boundary,
	// so a table for a block executed moments ago would otherwise be rejected as
	// "above the executed height" and forced through a rebuild that cannot see
	// the uncommitted canonical hashes yet.
	pending map[TableRef]pendingTable
}

type pendingTable struct {
	raw    []byte
	result TableResult
}

var _ TableStore = (*HotTableStore)(nil)

// NewHotTableStore returns a hot table store writing to tx. `listLimit` is the
// SSZ list bound the tables were computed under and is used to bound a record's
// size; it stays explicit while the EIP's bound is unresolved.
func NewHotTableStore(tx kv.RwTx, canonical CanonicalData, progress ExecutionProgress, listLimit uint64) (*HotTableStore, error) {
	if tx == nil {
		return nil, ErrNilTableStoreTx
	}
	if canonical == nil {
		return nil, ErrNilCanonicalData
	}
	if progress == nil {
		return nil, ErrNilProgressSource
	}
	return &HotTableStore{
		reads:     tx,
		writes:    tx,
		canonical: canonical,
		progress:  progress,
		listLimit: listLimit,
		pending:   make(map[TableRef]pendingTable),
	}, nil
}

// NewHotTableReader returns the read side of a hot table store over tx. It is
// used by block building, where the state transaction is read-only and the
// sealed block hash is not known yet, so table records must not be written.
func NewHotTableReader(tx kv.Tx, canonical CanonicalData, progress ExecutionProgress, listLimit uint64) (*HotTableStore, error) {
	if tx == nil {
		return nil, ErrNilTableStoreTx
	}
	if canonical == nil {
		return nil, ErrNilCanonicalData
	}
	if progress == nil {
		return nil, ErrNilProgressSource
	}
	return &HotTableStore{
		reads:     tx,
		canonical: canonical,
		progress:  progress,
		listLimit: listLimit,
		pending:   make(map[TableRef]pendingTable),
	}, nil
}

func (s *HotTableStore) CanonicalHash(block uint64) (common.Hash, error) {
	return s.canonical.CanonicalHash(block)
}

func (s *HotTableStore) BlockEntries(block uint64) (Entries, error) {
	return s.canonical.BlockEntries(block)
}

// GetTable reads the record for ref, reporting a miss only when no record
// exists. A record that cannot be decoded, was written by another record
// version, is incomplete, covers blocks above the executed height, or fails its
// checksum is an error the resolver answers by rebuilding.
func (s *HotTableStore) GetTable(ref TableRef) (TableResult, bool, error) {
	if err := validateTableRef(ref); err != nil {
		return TableResult{}, false, err
	}
	raw, err := s.reads.GetOne(kv.Eip8304Tables, hotStoreKey(ref))
	if err != nil {
		return TableResult{}, false, fmt.Errorf("read table %+v: %w", ref, err)
	}
	if raw == nil {
		return TableResult{}, false, nil
	}
	result, err := decodeTableRecord(ref, raw, s.listLimit)
	if err != nil {
		return TableResult{}, false, err
	}
	progress, err := s.progress.ExecutedHeight()
	if err != nil {
		return TableResult{}, false, fmt.Errorf("execution progress: %w", err)
	}
	if end := ref.FirstBlock + ref.TableSize - 1; end > progress {
		// The staged-sync progress advances at a batch boundary, after the
		// finalizer has already written this transaction's table. Accept that
		// record only while the bytes still match what this store wrote; a
		// later direct rewrite, damage or delete must remain visible.
		if pending, ok := s.pending[ref]; ok && bytes.Equal(pending.raw, raw) {
			return pending.result, true, nil
		}
		return TableResult{}, false, fmt.Errorf("%w: %+v covers block %d, executed height %d", ErrTableRecordProgress, ref, end, progress)
	}
	return result, true, nil
}

// PutTable encodes result and writes it as one value, so a record is either
// fully visible or absent. The caller must have applied the table's system call
// already; an error means nothing was recorded and the block must be aborted.
//
// A result whose entries do not belong to result.Ref's block range is rejected
// rather than stored: it would be served back as this table's root, and a
// checksum cannot tell it apart from a correct record.
func (s *HotTableStore) PutTable(result TableResult) error {
	if s.writes == nil {
		return ErrReadOnlyTableStore
	}
	if err := validateTableResult(result, s.listLimit); err != nil {
		return err
	}

	raw := encodeTableRecord(result)
	if err := s.writes.Put(kv.Eip8304Tables, hotStoreKey(result.Ref), raw); err != nil {
		return fmt.Errorf("write table %+v: %w", result.Ref, err)
	}
	s.pending[result.Ref] = pendingTable{raw: raw, result: result}
	if newest := result.Ref.FirstBlock + result.Ref.TableSize - 1; newest > MaxTableLookback {
		keepFrom := newest - MaxTableLookback
		for ref := range s.pending {
			if ref.FirstBlock+ref.TableSize-1 < keepFrom {
				delete(s.pending, ref)
			}
		}
	}
	return nil
}

func validateTableResult(result TableResult, listLimit uint64) error {
	if err := validateTableRef(result.Ref); err != nil {
		return err
	}
	if result.EntryCount != uint64(len(result.Entries)) {
		return fmt.Errorf("%w: entry_count %d, %d entries", ErrTableEntryCount, result.EntryCount, len(result.Entries))
	}
	if result.EntryCount > listLimit {
		return fmt.Errorf("%w: %d entries, limit %d", ErrTableRecordTooLarge, result.EntryCount, listLimit)
	}
	if uint64(len(result.BlockHashes)) != result.Ref.TableSize {
		return fmt.Errorf("%w: %d hashes for table_size %d", ErrTableCoverageMismatch, len(result.BlockHashes), result.Ref.TableSize)
	}
	// The entries must belong to the table being written. The root is not
	// recomputed here: the caller has just produced it, and the read path
	// re-derives it before serving the record.
	if err := checkEntryBlocks(result.Entries, result.Ref); err != nil {
		return err
	}
	return nil
}

// Prune drops every record whose covered range ends below keepFrom and reports
// how many were removed. A table can only be needed while a future due table
// can merge it, so callers pass a watermark derived from the retained range.
func (s *HotTableStore) Prune(keepFrom uint64) (int, error) {
	if s.writes == nil {
		return 0, ErrReadOnlyTableStore
	}
	pruned, err := PruneHotTableRecords(s.writes, keepFrom)
	if err != nil {
		return pruned, err
	}
	for ref := range s.pending {
		if ref.FirstBlock+ref.TableSize-1 < keepFrom {
			delete(s.pending, ref)
		}
	}
	return pruned, nil
}

// PruneHotTableRecords drops table records whose covered range ends before
// keepFrom. It is the transaction-only form of HotTableStore.Prune, so staged
// sync can reclaim the bucket without constructing canonical data or a full
// execution store.
func PruneHotTableRecords(tx kv.RwTx, keepFrom uint64) (int, error) {
	if tx == nil {
		return 0, ErrNilTableStoreTx
	}
	cursor, err := tx.RwCursor(kv.Eip8304Tables)
	if err != nil {
		return 0, fmt.Errorf("prune table store: %w", err)
	}
	defer cursor.Close()

	pruned := 0
	for k, _, err := cursor.First(); k != nil; k, _, err = cursor.Next() {
		if err != nil {
			return pruned, fmt.Errorf("prune table store: %w", err)
		}
		if isHotStoreMetaKey(k) {
			continue
		}
		ref, err := decodeHotStoreKey(k)
		if err == nil {
			err = validateTableRef(ref)
		}
		if err != nil {
			// An unreadable key, or one naming a table that cannot exist, is a
			// damaged record: drop it so it cannot be mistaken for a usable
			// table. Its covered range is meaningless, so a key like this would
			// otherwise never be evicted by the watermark below and would sit in
			// the bucket forever.
			if err := cursor.DeleteCurrent(); err != nil {
				return pruned, fmt.Errorf("prune damaged record: %w", err)
			}
			pruned++
			continue
		}
		if ref.FirstBlock+ref.TableSize-1 < keepFrom {
			if err := cursor.DeleteCurrent(); err != nil {
				return pruned, fmt.Errorf("prune %+v: %w", ref, err)
			}
			pruned++
		}
	}
	return pruned, nil
}

func hotStoreKey(ref TableRef) []byte {
	key := make([]byte, hotStoreKeySize)
	key[0] = hotStoreKeyVersion
	binary.BigEndian.PutUint64(key[1:9], ref.FirstBlock)
	binary.BigEndian.PutUint64(key[9:17], ref.TableSize)
	return key
}

func decodeHotStoreKey(key []byte) (TableRef, error) {
	if len(key) != hotStoreKeySize {
		return TableRef{}, fmt.Errorf("%w: key is %d bytes", ErrTableRecordCorrupt, len(key))
	}
	if key[0] != hotStoreKeyVersion {
		return TableRef{}, fmt.Errorf("%w: key version %d", ErrTableRecordVersion, key[0])
	}
	return TableRef{
		FirstBlock: binary.BigEndian.Uint64(key[1:9]),
		TableSize:  binary.BigEndian.Uint64(key[9:17]),
	}, nil
}

// encodeTableRecord encodes a result as magic, record version, completeness
// flag, entry count, root, covered hashes, entries and a CRC32 trailer.
func encodeTableRecord(result TableResult) []byte {
	entriesSize := 0
	for _, entry := range result.Entries {
		entriesSize += len(entry.Encode())
	}
	raw := make([]byte, 0, hotStoreHeader+entriesSize+hotStoreTrailer)

	raw = append(raw, hotStoreMagic0, hotStoreMagic1, hotStoreMagic2, hotStoreMagic3, hotStoreKeyVersion, hotStoreStateComplete, 0, 0)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], result.EntryCount)
	raw = append(raw, buf[:]...)
	raw = append(raw, result.Root[:]...)
	binary.BigEndian.PutUint64(buf[:], uint64(len(result.BlockHashes)))
	raw = append(raw, buf[:]...)
	for _, hash := range result.BlockHashes {
		raw = append(raw, hash[:]...)
	}
	for _, entry := range result.Entries {
		raw = append(raw, entry.Encode()...)
	}
	binary.BigEndian.PutUint32(buf[:4], crc32.ChecksumIEEE(raw))
	return append(raw, buf[:4]...)
}

// decodeTableRecord is the inverse of encodeTableRecord. Every rejection names
// what was wrong with the record so a corrupt store is diagnosable.
//
// Everything the record says about its own size is checked against the limit
// before it is used to allocate: a record is read from a bucket this node wrote,
// but a damaged one must be rejected, not trusted into a huge allocation.
func decodeTableRecord(ref TableRef, raw []byte, limit uint64) (TableResult, error) {
	if err := validateTableRef(ref); err != nil {
		return TableResult{}, err
	}
	if len(raw) < hotStoreHeader+hotStoreTrailer {
		return TableResult{}, fmt.Errorf("%w: record is %d bytes", ErrTableRecordCorrupt, len(raw))
	}
	if raw[0] != hotStoreMagic0 || raw[1] != hotStoreMagic1 || raw[2] != hotStoreMagic2 || raw[3] != hotStoreMagic3 {
		return TableResult{}, fmt.Errorf("%w: bad magic", ErrTableRecordCorrupt)
	}
	if raw[hotStoreVersionOffset] != hotStoreKeyVersion {
		return TableResult{}, fmt.Errorf("%w: record version %d", ErrTableRecordVersion, raw[hotStoreVersionOffset])
	}
	if raw[hotStoreStateOffset] != hotStoreStateComplete {
		return TableResult{}, fmt.Errorf("%w: state %d", ErrTableRecordPartial, raw[hotStoreStateOffset])
	}
	if want, got := crc32.ChecksumIEEE(raw[:len(raw)-hotStoreTrailer]), binary.BigEndian.Uint32(raw[len(raw)-hotStoreTrailer:]); want != got {
		return TableResult{}, fmt.Errorf("%w: checksum %08x, want %08x", ErrTableRecordCorrupt, got, want)
	}

	entryCount := binary.BigEndian.Uint64(raw[hotStoreEntryCountOffset : hotStoreEntryCountOffset+8])
	if entryCount > limit {
		return TableResult{}, fmt.Errorf("%w: entry_count %d, limit %d", ErrTableRecordTooLarge, entryCount, limit)
	}
	root := common.BytesToHash(raw[hotStoreRootOffset : hotStoreRootOffset+hashLength])
	hashCount := binary.BigEndian.Uint64(raw[hotStoreHashCountOffset:hotStoreHeader])
	if hashCount != ref.TableSize {
		return TableResult{}, fmt.Errorf("%w: %d hashes for table_size %d", ErrTableCoverageMismatch, hashCount, ref.TableSize)
	}
	hashesOffset := hotStoreHeader
	entriesOffset := hashesOffset + int(hashCount)*hashLength
	if entriesOffset > len(raw)-hotStoreTrailer {
		return TableResult{}, fmt.Errorf("%w: %d hashes do not fit the record", ErrTableRecordCorrupt, hashCount)
	}
	hashes := make([]common.Hash, hashCount)
	for i := range hashes {
		hashes[i] = common.BytesToHash(raw[hashesOffset+i*hashLength : hashesOffset+(i+1)*hashLength])
	}

	entries, consumed, err := DecodeEntries(raw[entriesOffset:len(raw)-hotStoreTrailer], entryCount)
	if err != nil {
		return TableResult{}, fmt.Errorf("%w: %w", ErrTableRecordCorrupt, err)
	}
	if entriesOffset+consumed != len(raw)-hotStoreTrailer {
		return TableResult{}, fmt.Errorf("%w: %d trailing bytes", ErrTableRecordCorrupt, len(raw)-hotStoreTrailer-entriesOffset-consumed)
	}

	return TableResult{
		Ref:         ref,
		EntryCount:  entryCount,
		Entries:     entries,
		Root:        root,
		BlockHashes: hashes,
	}, nil
}
