package eip8304

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
)

// Recovery, unwind and job protection for the hot table store.
//
// Three situations can leave a stored table unusable, and each has an explicit
// answer rather than an implicit one: a reorg (Invalidate), a restart
// (Reconcile) and a table computed away from the block it belongs to (TableJob).
// All three drop the record and let the next read rebuild it; none repairs a
// record in place.
//
// The generation counter is the cheap gate for the third case: Invalidate bumps
// it, so any job started earlier fails immediately. It is a gate, not the
// semantics: the job also has to verify against the canonical chain, which is
// what actually decides whether a result is still valid. Details:
// docs/eip8304/hot-table-store.md.

// ErrStaleTableJob reports a job whose chain view changed before it committed.
var ErrStaleTableJob = errors.New("eip8304: table job is stale")

// hotStoreMetaKey is the reserved 1-byte key under which the store keeps its
// generation counter. It is not a table key — those are 17 bytes and start with
// the record version — so isHotStoreMetaKey is what Prune, Invalidate and
// Reconcile use to leave it alone.
var hotStoreMetaKey = []byte{0x00}

func isHotStoreMetaKey(key []byte) bool {
	return len(key) == 1 && key[0] == 0
}

// Generation returns the store's current invalidation generation. It starts at
// 0 and is bumped by every Invalidate.
func (s *HotTableStore) Generation() (uint64, error) {
	return s.readGeneration()
}

func (s *HotTableStore) readGeneration() (uint64, error) {
	raw, err := s.tx.GetOne(kv.Eip8304Tables, hotStoreMetaKey)
	if err != nil {
		return 0, fmt.Errorf("read table store generation: %w", err)
	}
	if raw == nil {
		return 0, nil
	}
	if len(raw) != 8 {
		return 0, fmt.Errorf("%w: generation is %d bytes", ErrTableRecordCorrupt, len(raw))
	}
	return binary.BigEndian.Uint64(raw), nil
}

func (s *HotTableStore) writeGeneration(generation uint64) error {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], generation)
	if err := s.tx.Put(kv.Eip8304Tables, hotStoreMetaKey, raw[:]); err != nil {
		return fmt.Errorf("write table store generation: %w", err)
	}
	return nil
}

// Invalidate drops every record whose covered range reaches `from`, i.e. every
// table an unwind to that height could have invalidated, and bumps the
// generation so in-flight jobs cannot write results back afterwards. Records
// covering only blocks before `from` are untouched.
//
// `from` is the first block that no longer exists after the unwind: a caller
// unwinding to height H (H itself surviving) passes H+1. A table is invalid as
// soon as its range contains a removed block, which is why the test is on the
// range's end (end >= from) and not on its start.
func (s *HotTableStore) Invalidate(from uint64) (int, error) {
	generation, err := s.readGeneration()
	if err != nil {
		return 0, err
	}
	// Bump before deleting: if the delete below fails halfway, stale jobs must
	// already be rejected, which is the safe direction to fail in.
	if err := s.writeGeneration(generation + 1); err != nil {
		return 0, err
	}

	cursor, err := s.tx.RwCursor(kv.Eip8304Tables)
	if err != nil {
		return 0, fmt.Errorf("invalidate table store: %w", err)
	}
	defer cursor.Close()

	dropped := 0
	for k, _, err := cursor.First(); k != nil; k, _, err = cursor.Next() {
		if err != nil {
			return dropped, fmt.Errorf("invalidate table store: %w", err)
		}
		if isHotStoreMetaKey(k) {
			continue
		}
		ref, err := decodeHotStoreKey(k)
		if err != nil {
			// An unreadable key cannot be a usable table; drop it.
			if err := cursor.DeleteCurrent(); err != nil {
				return dropped, fmt.Errorf("invalidate damaged record: %w", err)
			}
			dropped++
			continue
		}
		if ref.FirstBlock+ref.TableSize-1 >= from {
			if err := cursor.DeleteCurrent(); err != nil {
				return dropped, fmt.Errorf("invalidate %+v: %w", ref, err)
			}
			dropped++
		}
	}
	return dropped, nil
}

// ReconcileReport summarises a restart check: how many records were inspected,
// how many survived, and why the rest were dropped.
type ReconcileReport struct {
	Checked int

	Kept int

	DroppedDamaged int // unreadable or not a valid level of a table
	DroppedAhead   int // covers blocks above the executed height
	DroppedStale   int // no longer bound to the canonical chain

	// HighestCovered is the highest block covered by a surviving record, and
	// HasRecords reports whether any record survived.
	HighestCovered uint64
	HasRecords     bool
}

// Dropped returns the number of records Reconcile removed.
func (r ReconcileReport) Dropped() int {
	return r.DroppedDamaged + r.DroppedAhead + r.DroppedStale
}

// Reconcile checks every stored record against the executed height and the
// canonical chain, dropping the ones that cannot be used. It never repairs a
// record in place: a dropped table is rebuilt from canonical data by the next
// read, which keeps the store's contents derivable rather than patched.
//
// `executed` is the highest fully executed block; passing a height below a
// record's covered range is how a restart detects that execution and the table
// store disagree.
func (s *HotTableStore) Reconcile(executed uint64) (ReconcileReport, error) {
	var report ReconcileReport

	cursor, err := s.tx.RwCursor(kv.Eip8304Tables)
	if err != nil {
		return report, fmt.Errorf("reconcile table store: %w", err)
	}
	defer cursor.Close()

	for k, v, err := cursor.First(); k != nil; k, v, err = cursor.Next() {
		if err != nil {
			return report, fmt.Errorf("reconcile table store: %w", err)
		}
		if isHotStoreMetaKey(k) {
			continue
		}
		report.Checked++

		drop := func() error {
			if err := cursor.DeleteCurrent(); err != nil {
				return fmt.Errorf("reconcile: %w", err)
			}
			return nil
		}

		ref, err := decodeHotStoreKey(k)
		if err != nil {
			report.DroppedDamaged++
			if err := drop(); err != nil {
				return report, err
			}
			continue
		}
		result, err := decodeTableRecord(ref, v, s.listLimit)
		if err != nil {
			report.DroppedDamaged++
			if err := drop(); err != nil {
				return report, err
			}
			continue
		}
		if end := ref.FirstBlock + ref.TableSize - 1; end > executed {
			report.DroppedAhead++
			if err := drop(); err != nil {
				return report, err
			}
			continue
		}
		canonical, err := s.canonicalHashes(ref)
		if err != nil {
			// Without the canonical hashes the record cannot be bound, so it
			// cannot be trusted. The read path would rebuild it anyway.
			report.DroppedStale++
			if err := drop(); err != nil {
				return report, err
			}
			continue
		}
		if !equalHashes(result.BlockHashes, canonical) {
			report.DroppedStale++
			if err := drop(); err != nil {
				return report, err
			}
			continue
		}
		// Self-consistency: a record whose root does not match its own entries
		// (which a checksum alone cannot catch, because the checksum covers the
		// written bytes and not their meaning) is unusable. The read path would
		// reject it too, but a restart report should not call it healthy.
		if result.EntryCount != uint64(len(result.Entries)) || !entriesSorted(result.Entries) {
			report.DroppedDamaged++
			if err := drop(); err != nil {
				return report, err
			}
			continue
		}
		root, err := RootOfEntries(result.Entries, s.listLimit)
		if err != nil || root != result.Root {
			report.DroppedDamaged++
			if err := drop(); err != nil {
				return report, err
			}
			continue
		}

		report.Kept++
		if end := ref.FirstBlock + ref.TableSize - 1; !report.HasRecords || end > report.HighestCovered {
			report.HighestCovered = end
			report.HasRecords = true
		}
	}
	return report, nil
}

// TableJob is a table computed away from the block it belongs to. It captures
// the store generation and the canonical block hashes of the covered range, so
// CommitJob can refuse a result that a reorg or unwind invalidated while the
// job was running.
type TableJob struct {
	ref        TableRef
	generation uint64
	hashes     []common.Hash
}

// Ref returns the table a job computes.
func (j TableJob) Ref() TableRef { return j.ref }

// BeginJob captures the chain view a delayed or background merge must compute
// against. Taking a job fails if the covered range's canonical hashes are not
// available, so a job never starts on data the node cannot serve.
func (s *HotTableStore) BeginJob(ref TableRef) (TableJob, error) {
	if err := validateTableRef(ref); err != nil {
		return TableJob{}, err
	}
	generation, err := s.readGeneration()
	if err != nil {
		return TableJob{}, err
	}
	hashes, err := s.canonicalHashes(ref)
	if err != nil {
		return TableJob{}, fmt.Errorf("begin job %+v: %w", ref, err)
	}
	return TableJob{ref: ref, generation: generation, hashes: hashes}, nil
}

// CommitJob writes a job's result if it is still valid: the store must not have
// been invalidated since the job started, and the result must still verify
// against the canonical chain. A rejected result is not written and is not an
// error the caller should retry blindly — it means the chain moved on.
func (s *HotTableStore) CommitJob(job TableJob, result TableResult) error {
	if result.Ref != job.ref {
		return fmt.Errorf("%w: job %+v, result %+v", ErrTableRefMismatch, job.ref, result.Ref)
	}
	generation, err := s.readGeneration()
	if err != nil {
		return err
	}
	if generation != job.generation {
		return fmt.Errorf("%w: job %+v started at generation %d, store is at %d", ErrStaleTableJob, job.ref, job.generation, generation)
	}
	// The job was computed against the hashes captured at BeginJob; if the chain
	// replaced any of them, the result describes a table that no longer exists,
	// even when it happens to verify.
	current, err := s.canonicalHashes(job.ref)
	if err != nil {
		return fmt.Errorf("%w: job %+v: %w", ErrStaleTableJob, job.ref, err)
	}
	if !equalHashes(job.hashes, current) {
		return fmt.Errorf("%w: job %+v was computed against different canonical hashes", ErrStaleTableJob, job.ref)
	}
	if err := VerifyTable(result, job.ref, canonicalOnly{s.canonical}, s.listLimit); err != nil {
		return fmt.Errorf("%w: job %+v: %w", ErrStaleTableJob, job.ref, err)
	}
	return s.PutTable(result)
}

// canonicalOnly narrows a CanonicalData source to the hash lookups VerifyTable
// needs, so a job result is checked against the chain and not against the
// store's own cache.
type canonicalOnly struct{ data CanonicalData }

func (c canonicalOnly) CanonicalHash(block uint64) (common.Hash, error) {
	return c.data.CanonicalHash(block)
}

func (s *HotTableStore) canonicalHashes(ref TableRef) ([]common.Hash, error) {
	hashes := make([]common.Hash, ref.TableSize)
	for i := range hashes {
		hash, err := s.canonical.CanonicalHash(ref.FirstBlock + uint64(i))
		if err != nil {
			return nil, fmt.Errorf("canonical hash for block %d: %w", ref.FirstBlock+uint64(i), err)
		}
		hashes[i] = hash
	}
	return hashes, nil
}

func equalHashes(a, b []common.Hash) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
