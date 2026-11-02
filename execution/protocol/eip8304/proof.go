package eip8304

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/erigontech/erigon/common"
)

// Local proof PoC: a versioned proof for one condition over one table, plus a
// verifier that needs nothing but the proof and the SSZ list bound. Format,
// threat model and evidence: docs/eip8304/proof-poc.md.

// ProofFormatVersion is the version of TableProof's encoding. The format is
// local on purpose: the pinned spec leaves proof serialization open, so this
// fixes a shape without claiming to be the cross-client one.
const ProofFormatVersion uint8 = 1

// Proof errors. Each names one way a proof can fail, so a caller can tell a
// malformed artifact from a proof that does not match the table.
var (
	ErrProofVersion       = errors.New("eip8304: unsupported proof format version")
	ErrProofCorrupt       = errors.New("eip8304: corrupt proof encoding")
	ErrProofQuery         = errors.New("eip8304: invalid proof query")
	ErrProofQueryMismatch = errors.New("eip8304: proof answers a different query")
	ErrQueryIncomplete    = errors.New("eip8304: proof does not completely answer the query")
	ErrProofEntry         = errors.New("eip8304: proof leaf is not a canonical entry")
	ErrProofCoverage      = errors.New("eip8304: proof does not cover the list or the queried range")
	ErrProofBoundary      = errors.New("eip8304: proof boundaries do not bound the queried key")
	ErrProofRootMismatch  = errors.New("eip8304: proof does not recompute the table root")
	ErrProofRefMismatch   = errors.New("eip8304: proof is for a different table")
)

// ProofQuery is one condition over one bounded block range: entries of type
// Type whose value equals Value, for blocks in [FirstBlock, LastBlock].
//
// Block and transaction entries carry no searchable condition of their own, so
// only the log conditions are accepted, and a query never widens the value.
// Because the block field follows the value in an entry's encoding, the entries
// a query matches are one contiguous run of the leaf list, which is what lets a
// proof about them stay short.
type ProofQuery struct {
	FirstBlock uint64
	LastBlock  uint64
	Type       EntryType
	Value      common.Hash
}

func (q ProofQuery) singleCondition() bool {
	switch q.Type {
	case EntryLogAddress, EntryLogTopic0, EntryLogTopic1, EntryLogTopic2, EntryLogTopic3:
		return true
	default:
		return false
	}
}

// validate checks the condition and the block range, then that the range is
// fully covered by ref. A query that reaches outside the table cannot be
// answered by it, so it is rejected instead of being truncated to the overlap.
func (q ProofQuery) validate(ref TableRef) error {
	if !q.singleCondition() {
		return fmt.Errorf("%w: entry type %d is not a single-condition query", ErrProofQuery, q.Type)
	}
	if q.FirstBlock > q.LastBlock {
		return fmt.Errorf("%w: block range [%d, %d]", ErrProofQuery, q.FirstBlock, q.LastBlock)
	}
	if q.Type == EntryLogAddress && !bytes.Equal(q.Value[:hashLength-addressLength], make([]byte, hashLength-addressLength)) {
		return fmt.Errorf("%w: address condition is not right aligned", ErrProofQuery)
	}
	if q.FirstBlock < ref.FirstBlock || q.LastBlock > ref.FirstBlock+ref.TableSize-1 {
		return fmt.Errorf("%w: block range [%d, %d] outside %+v", ErrProofQuery, q.FirstBlock, q.LastBlock, ref)
	}
	return nil
}

// bounds returns the smallest and the largest canonical encoding an entry
// matching q can have. An encoding is
// type || value || block || transaction || position, so the type and the value
// are its leading bytes, the type fixes its length, and the block follows the
// value: every encoding inside the interval belongs to a matching entry and
// every encoding outside it does not.
func (q ProofQuery) bounds() (lower, upper []byte) {
	// validate rejected every type that is not a log condition, and the only
	// widths a log entry has are the address and the hash, so the width is
	// fixed here. A log entry always carries its transaction and position
	// fields, so the encoding length is fixed too (see Entry.Encode).
	valueSize := hashLength
	if q.Type == EntryLogAddress {
		valueSize = addressLength
	}
	size := 2 + valueSize + 8 + 8
	lower = make([]byte, size)
	upper = make([]byte, size)
	binary.BigEndian.PutUint16(lower, uint16(q.Type))
	binary.BigEndian.PutUint16(upper, uint16(q.Type))
	copy(lower[2:], q.Value[hashLength-valueSize:])
	copy(upper[2:], q.Value[hashLength-valueSize:])
	binary.BigEndian.PutUint64(lower[2+valueSize:], q.FirstBlock)
	binary.BigEndian.PutUint64(upper[2+valueSize:], q.LastBlock)
	for i := 2 + valueSize + 8; i < size; i++ {
		upper[i] = 0xff
	}
	return lower, upper
}

// TableProof proves that a table's leaves at [Start, Start+len(Leaves)) are the
// entries it claims, and that the entries matching Query are exactly the
// MatchCount leaves starting at MatchStart. MatchStart is also the insertion
// point when MatchCount is zero, which is the empty-result case.
type TableProof struct {
	Version    uint8
	Ref        TableRef
	EntryCount uint64
	Root       common.Hash
	Query      ProofQuery
	// Start is the index of Leaves[0] in the table's leaf list.
	Start uint64
	// Leaves are the revealed entries in index order.
	Leaves []EncodedEntry
	// MatchStart and MatchCount locate the matching run inside the list.
	MatchStart uint64
	MatchCount uint64
	// Siblings are the sibling hashes of the revealed range, bottom-up and
	// ascending within each level, exactly as the verifier consumes them.
	Siblings []common.Hash
}

// ProveTable builds the proof for query over table. It returns an error when
// the query is not answerable by this table, and it verifies the proof it just
// built before returning it, so the generator cannot emit an artifact its own
// verifier rejects.
func ProveTable(table TableResult, query ProofQuery, limit uint64) (TableProof, error) {
	if err := validateTableRef(table.Ref); err != nil {
		return TableProof{}, err
	}
	if err := query.validate(table.Ref); err != nil {
		return TableProof{}, err
	}
	// A proof is a claim about a well-formed table, so the generator applies the
	// same shape check the store's read path and its restart check apply: entry
	// count, canonical entry order, entries inside the covered window, and the
	// root those entries hash to. A table that fails it cannot be proven.
	if err := checkResultShape(table, table.Ref, limit); err != nil {
		return TableProof{}, err
	}
	if len(table.Entries) == 0 {
		proof := TableProof{
			Version:    ProofFormatVersion,
			Ref:        table.Ref,
			EntryCount: 0,
			Root:       table.Root,
			Query:      query,
		}
		if err := VerifyTableProof(proof, limit); err != nil {
			return TableProof{}, fmt.Errorf("generated empty proof does not verify: %w", err)
		}
		return proof, nil
	}
	keys := make([]EncodedEntry, len(table.Entries))
	hashes := make([]common.Hash, len(table.Entries))
	for i, entry := range table.Entries {
		keys[i] = entry.Encode()
		hashes[i] = keys[i].LeafHash()
	}
	lower, upper := query.bounds()
	count := uint64(len(keys))
	first := uint64(searchKeys(keys, lower, false))
	after := uint64(searchKeys(keys, upper, true))

	start := first
	if start > 0 {
		start--
	}
	end := after
	if end < count {
		end++
	}
	leaves := make([]EncodedEntry, 0, end-start)
	for i := start; i < end; i++ {
		leaves = append(leaves, keys[i])
	}

	proof := TableProof{
		Version:    ProofFormatVersion,
		Ref:        table.Ref,
		EntryCount: count,
		Root:       table.Root,
		Query:      query,
		Start:      start,
		Leaves:     leaves,
		MatchStart: first,
		MatchCount: after - first,
		Siblings:   buildProofSiblings(hashes, start, end-1, listDepth(limit)),
	}
	if err := VerifyTableProof(proof, limit); err != nil {
		return TableProof{}, fmt.Errorf("generated proof does not verify: %w", err)
	}
	return proof, nil
}

// searchKeys returns the first index whose key is at or after `bound`, or the
// first index strictly after it when strict is set. The caller has already put
// the keys in canonical order (ProveTable runs checkResultShape first), so the
// predicate is monotone and the range of matching entries is the interval
// between the two searches.
func searchKeys(keys []EncodedEntry, bound []byte, strict bool) int {
	return sort.Search(len(keys), func(i int) bool {
		cmp := bytes.Compare(keys[i], bound)
		if strict {
			return cmp > 0
		}
		return cmp >= 0
	})
}

// VerifyTableProof verifies a proof against the root and reference it carries.
// It is the generator's self-check; callers verifying an untrusted proof should
// use VerifyQueryProof, which takes the expected table and storage binding as
// explicit inputs.
func VerifyTableProof(proof TableProof, limit uint64) error {
	_, err := verifyTableProofAgainst(proof, proof.Ref, proof.Root, proof.Query, limit)
	return err
}

// verifyTableProofAgainst recomputes the table root from the revealed leaves
// and siblings and then classifies them against the query's key interval, so
// the matching run is exactly the leaves inside it. ProofValid covers the
// version, expected table, entry encodings, Merkle fold and expected root;
// QueryComplete additionally covers the expected query, the run that answers
// it, and its existing neighbours. A proof can therefore be valid while it
// answers a different or incomplete query.
func verifyTableProofAgainst(
	proof TableProof,
	expectedRef TableRef,
	expectedRoot common.Hash,
	expectedQuery ProofQuery,
	limit uint64,
) (QueryVerdict, error) {
	var verdict QueryVerdict
	if proof.Version != ProofFormatVersion {
		return verdict, fmt.Errorf("%w: %d", ErrProofVersion, proof.Version)
	}
	if err := validateTableRef(expectedRef); err != nil {
		return verdict, err
	}
	if proof.Ref != expectedRef {
		return verdict, fmt.Errorf("%w: have %+v, want %+v", ErrProofRefMismatch, proof.Ref, expectedRef)
	}
	if err := expectedQuery.validate(expectedRef); err != nil {
		return verdict, err
	}
	if proof.Root != expectedRoot {
		return verdict, fmt.Errorf("%w: have %x, want %x", ErrProofRootMismatch, proof.Root, expectedRoot)
	}
	if err := proof.Query.validate(expectedRef); err != nil {
		return verdict, err
	}
	if proof.EntryCount > limit {
		return verdict, fmt.Errorf("%w: entry_count %d, limit %d", ErrProofCoverage, proof.EntryCount, limit)
	}
	if proof.EntryCount == 0 {
		// A genesis table with no transactions is a legal empty SSZ list. Its
		// proof reveals nothing, has no siblings, and is complete for every
		// query that is valid for the table's block range.
		if proof.Start != 0 || len(proof.Leaves) != 0 || proof.MatchStart != 0 || proof.MatchCount != 0 || len(proof.Siblings) != 0 {
			return verdict, fmt.Errorf("%w: empty table proof has range or siblings", ErrProofCoverage)
		}
		root, err := ListHashRoot(nil, limit)
		if err != nil {
			return verdict, err
		}
		if root != expectedRoot {
			return verdict, fmt.Errorf("%w: have %x, want empty root %x", ErrProofRootMismatch, expectedRoot, root)
		}
		verdict.ProofValid = true
		if proof.Query != expectedQuery {
			return verdict, fmt.Errorf("%w: have %+v, want %+v", ErrProofQueryMismatch, proof.Query, expectedQuery)
		}
		verdict.QueryComplete = true
		return verdict, nil
	}
	leaves := uint64(len(proof.Leaves))
	if leaves == 0 || proof.Start >= proof.EntryCount || leaves > proof.EntryCount-proof.Start {
		return verdict, fmt.Errorf("%w: %d leaves at start %d of %d", ErrProofCoverage, leaves, proof.Start, proof.EntryCount)
	}
	if proof.MatchStart > proof.EntryCount || proof.MatchCount > proof.EntryCount-proof.MatchStart {
		return verdict, fmt.Errorf("%w: match [%d, %d) of %d", ErrProofCoverage, proof.MatchStart, proof.MatchStart+proof.MatchCount, proof.EntryCount)
	}

	low, high := coveredBlockWindow(expectedRef)
	keys := make([][]byte, leaves)
	hashes := make([]common.Hash, leaves)
	for i, raw := range proof.Leaves {
		entry, size, err := DecodeEntry(raw)
		if err != nil {
			return verdict, fmt.Errorf("%w: leaf %d: %w", ErrProofEntry, i, err)
		}
		if size != len(raw) {
			return verdict, fmt.Errorf("%w: leaf %d has %d trailing bytes", ErrProofEntry, i, len(raw)-size)
		}
		if entry.block < low || entry.block > high {
			return verdict, fmt.Errorf("%w: leaf %d is for block %d, outside [%d, %d] of %+v", ErrProofCoverage, i, entry.block, low, high, expectedRef)
		}
		keys[i] = raw
		hashes[i] = raw.LeafHash()
	}

	contents, err := reduceProof(hashes, proof.Start, proof.Siblings, listDepth(limit))
	if err != nil {
		return verdict, err
	}
	root := listRootFromContents(contents, proof.EntryCount)
	if root != expectedRoot {
		return verdict, fmt.Errorf("%w: have %x, want %x", ErrProofRootMismatch, expectedRoot, root)
	}
	verdict.ProofValid = true

	if proof.Query != expectedQuery {
		return verdict, fmt.Errorf("%w: have %+v, want %+v", ErrProofQueryMismatch, proof.Query, expectedQuery)
	}

	if err := checkProofQuery(proof, keys); err != nil {
		return verdict, fmt.Errorf("%w: %w", ErrQueryIncomplete, err)
	}
	verdict.QueryComplete = true
	return verdict, nil
}

// checkProofQuery classifies every revealed leaf against the query's key
// interval. The run the proof declares must be inside the interval, its
// neighbours outside it, and the revealed range must contain both the run and
// the neighbours, so the run is bounded on each side that exists.
func checkProofQuery(proof TableProof, keys [][]byte) error {
	lower, upper := proof.Query.bounds()
	matchEnd := proof.MatchStart + proof.MatchCount

	needLow, needHigh := uint64(0), proof.EntryCount-1
	if proof.MatchStart > 0 {
		needLow = proof.MatchStart - 1
	}
	if matchEnd < proof.EntryCount {
		needHigh = matchEnd
	}
	if proof.Start > needLow || proof.Start+uint64(len(keys))-1 < needHigh {
		return fmt.Errorf("%w: revealed [%d, %d) does not contain [%d, %d]", ErrProofCoverage, proof.Start, proof.Start+uint64(len(keys)), needLow, needHigh+1)
	}

	for i, key := range keys {
		index := proof.Start + uint64(i)
		lowCmp := bytes.Compare(key, lower)
		highCmp := bytes.Compare(key, upper)
		switch {
		case index >= proof.MatchStart && index < matchEnd:
			if lowCmp < 0 || highCmp > 0 {
				return fmt.Errorf("%w: leaf %d is not inside the queried keys", ErrProofBoundary, index)
			}
		case index < proof.MatchStart:
			if lowCmp >= 0 {
				return fmt.Errorf("%w: leaf %d is not below the queried keys", ErrProofBoundary, index)
			}
		default:
			if highCmp <= 0 {
				return fmt.Errorf("%w: leaf %d is not above the queried keys", ErrProofBoundary, index)
			}
		}
	}
	return nil
}

// listRootFromContents mixes the leaf count into the contents root, the same
// way ListHashRoot does, without re-hashing the leaves.
func listRootFromContents(contents common.Hash, count uint64) common.Hash {
	var buf [64]byte
	copy(buf[:32], contents[:])
	binary.LittleEndian.PutUint64(buf[32:], count)
	return sha256.Sum256(buf[:])
}

// buildProofSiblings walks the range [start, end] up the tree and records the
// siblings the verifier will ask for, bottom-up and ascending within a level.
// Nodes beyond the real leaves are the precomputed zero roots, the same virtual
// padding MerkleizeChunks uses.
func buildProofSiblings(leafHashes []common.Hash, start, end uint64, depth int) []common.Hash {
	levels := make([][]common.Hash, depth+1)
	levels[0] = leafHashes
	for level := 0; level < depth; level++ {
		cur := levels[level]
		next := make([]common.Hash, 0, (len(cur)+1)/2)
		i := 0
		for ; i+1 < len(cur); i += 2 {
			next = append(next, hashPair(cur[i], cur[i+1]))
		}
		if i < len(cur) {
			next = append(next, hashPair(cur[i], zeroHashes[level]))
		}
		levels[level+1] = next
	}

	var siblings []common.Hash
	for level := 0; level < depth; level++ {
		if start%2 == 1 {
			siblings = append(siblings, proofNode(levels, level, start-1))
			start--
		}
		if end%2 == 0 {
			siblings = append(siblings, proofNode(levels, level, end+1))
			end++
		}
		start, end = start/2, end/2
	}
	return siblings
}

func proofNode(levels [][]common.Hash, level int, index uint64) common.Hash {
	if index < uint64(len(levels[level])) {
		return levels[level][index]
	}
	return zeroHashes[level]
}

// reduceProof is the inverse of buildProofSiblings: it folds the revealed
// leaves with the siblings up to the contents root. It consumes every sibling
// and ends on a single node, so a proof with a missing or an extra sibling is
// rejected instead of being padded away.
func reduceProof(hashes []common.Hash, start uint64, siblings []common.Hash, depth int) (common.Hash, error) {
	cur := make([]common.Hash, len(hashes))
	copy(cur, hashes)
	first, last := start, start+uint64(len(cur))-1
	used := 0
	pull := func() (common.Hash, error) {
		if used >= len(siblings) {
			return common.Hash{}, fmt.Errorf("%w: missing sibling", ErrProofCoverage)
		}
		sibling := siblings[used]
		used++
		return sibling, nil
	}

	for level := 0; level < depth; level++ {
		if first%2 == 1 {
			sibling, err := pull()
			if err != nil {
				return common.Hash{}, err
			}
			cur = append([]common.Hash{sibling}, cur...)
			first--
		}
		if last%2 == 0 {
			sibling, err := pull()
			if err != nil {
				return common.Hash{}, err
			}
			cur = append(cur, sibling)
			last++
		}
		next := make([]common.Hash, 0, len(cur)/2)
		for i := 0; i+1 < len(cur); i += 2 {
			next = append(next, hashPair(cur[i], cur[i+1]))
		}
		cur, first, last = next, first/2, last/2
	}
	if used != len(siblings) {
		return common.Hash{}, fmt.Errorf("%w: %d siblings left over", ErrProofCoverage, len(siblings)-used)
	}
	if len(cur) != 1 || first != 0 || last != 0 {
		return common.Hash{}, fmt.Errorf("%w: proof ends on %d nodes at [%d, %d]", ErrProofCoverage, len(cur), first, last)
	}
	return cur[0], nil
}

// EncodeTableProof serializes a proof as version, table reference, list
// metadata, query, revealed leaves, siblings and a CRC32 trailer. The encoding
// is canonical: DecodeTableProof accepts exactly what this writes, so a
// truncated or extended artifact cannot be read back as a valid proof.
func EncodeTableProof(proof TableProof) []byte {
	raw := make([]byte, 0, 256)
	raw = append(raw, proof.Version)
	raw = appendUint64(raw, proof.Ref.FirstBlock)
	raw = appendUint64(raw, proof.Ref.TableSize)
	raw = appendUint64(raw, proof.EntryCount)
	raw = append(raw, proof.Root[:]...)
	raw = appendUint64(raw, proof.Query.FirstBlock)
	raw = appendUint64(raw, proof.Query.LastBlock)
	raw = appendUint16(raw, uint16(proof.Query.Type))
	raw = append(raw, proof.Query.Value[:]...)
	raw = appendUint64(raw, proof.Start)
	raw = appendUint64(raw, proof.MatchStart)
	raw = appendUint64(raw, proof.MatchCount)
	raw = appendUint32(raw, uint32(len(proof.Leaves)))
	for _, leaf := range proof.Leaves {
		raw = appendUint32(raw, uint32(len(leaf)))
		raw = append(raw, leaf...)
	}
	raw = appendUint32(raw, uint32(len(proof.Siblings)))
	for _, sibling := range proof.Siblings {
		raw = append(raw, sibling[:]...)
	}
	return appendUint32(raw, crc32.ChecksumIEEE(raw))
}

// DecodeTableProof parses what EncodeTableProof wrote. It checks the version
// and the CRC and rejects trailing bytes, but it does not verify the proof
// against a table: that is VerifyTableProof's job.
func DecodeTableProof(raw []byte) (TableProof, error) {
	if len(raw) < 4 {
		return TableProof{}, fmt.Errorf("%w: %d bytes", ErrProofCorrupt, len(raw))
	}
	body, checksum := raw[:len(raw)-4], binary.BigEndian.Uint32(raw[len(raw)-4:])
	if crc32.ChecksumIEEE(body) != checksum {
		return TableProof{}, fmt.Errorf("%w: checksum mismatch", ErrProofCorrupt)
	}
	reader := &proofReader{raw: body}
	version, err := reader.u8()
	if err != nil {
		return TableProof{}, err
	}
	if version != ProofFormatVersion {
		return TableProof{}, fmt.Errorf("%w: %d", ErrProofVersion, version)
	}

	var proof TableProof
	proof.Version = version
	if proof.Ref.FirstBlock, err = reader.u64(); err != nil {
		return TableProof{}, err
	}
	if proof.Ref.TableSize, err = reader.u64(); err != nil {
		return TableProof{}, err
	}
	if proof.EntryCount, err = reader.u64(); err != nil {
		return TableProof{}, err
	}
	if proof.Root, err = reader.hash(); err != nil {
		return TableProof{}, err
	}
	if proof.Query.FirstBlock, err = reader.u64(); err != nil {
		return TableProof{}, err
	}
	if proof.Query.LastBlock, err = reader.u64(); err != nil {
		return TableProof{}, err
	}
	entryType, err := reader.u16()
	if err != nil {
		return TableProof{}, err
	}
	proof.Query.Type = EntryType(entryType)
	if proof.Query.Value, err = reader.hash(); err != nil {
		return TableProof{}, err
	}
	if proof.Start, err = reader.u64(); err != nil {
		return TableProof{}, err
	}
	if proof.MatchStart, err = reader.u64(); err != nil {
		return TableProof{}, err
	}
	if proof.MatchCount, err = reader.u64(); err != nil {
		return TableProof{}, err
	}

	leafCount, err := reader.u32()
	if err != nil {
		return TableProof{}, err
	}
	// A leaf is at least minEncodedEntrySize bytes, so a count larger than the
	// remaining input can hold is rejected before anything is allocated.
	if uint64(leafCount) > uint64(reader.remaining())/minEncodedEntrySize {
		return TableProof{}, fmt.Errorf("%w: %d leaves do not fit", ErrProofCorrupt, leafCount)
	}
	proof.Leaves = make([]EncodedEntry, 0, leafCount)
	for i := uint32(0); i < leafCount; i++ {
		size, err := reader.u32()
		if err != nil {
			return TableProof{}, err
		}
		leaf, err := reader.bytes(int(size))
		if err != nil {
			return TableProof{}, err
		}
		proof.Leaves = append(proof.Leaves, EncodedEntry(leaf))
	}

	siblingCount, err := reader.u32()
	if err != nil {
		return TableProof{}, err
	}
	if uint64(siblingCount) > uint64(reader.remaining())/hashLength {
		return TableProof{}, fmt.Errorf("%w: %d siblings do not fit", ErrProofCorrupt, siblingCount)
	}
	proof.Siblings = make([]common.Hash, 0, siblingCount)
	for i := uint32(0); i < siblingCount; i++ {
		sibling, err := reader.hash()
		if err != nil {
			return TableProof{}, err
		}
		proof.Siblings = append(proof.Siblings, sibling)
	}
	if reader.remaining() != 0 {
		return TableProof{}, fmt.Errorf("%w: %d trailing bytes", ErrProofCorrupt, reader.remaining())
	}
	return proof, nil
}

type proofReader struct {
	raw []byte
	off int
}

func (r *proofReader) remaining() int { return len(r.raw) - r.off }

func (r *proofReader) bytes(n int) ([]byte, error) {
	if n < 0 || r.remaining() < n {
		return nil, fmt.Errorf("%w: need %d bytes, %d left", ErrProofCorrupt, n, r.remaining())
	}
	out := r.raw[r.off : r.off+n]
	r.off += n
	return out, nil
}

func (r *proofReader) u8() (uint8, error) {
	buf, err := r.bytes(1)
	if err != nil {
		return 0, err
	}
	return buf[0], nil
}

func (r *proofReader) u16() (uint16, error) {
	buf, err := r.bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(buf), nil
}

func (r *proofReader) u32() (uint32, error) {
	buf, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf), nil
}

func (r *proofReader) u64() (uint64, error) {
	buf, err := r.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(buf), nil
}

func (r *proofReader) hash() (common.Hash, error) {
	buf, err := r.bytes(hashLength)
	if err != nil {
		return common.Hash{}, err
	}
	return common.BytesToHash(buf), nil
}

func appendUint16(raw []byte, value uint16) []byte {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], value)
	return append(raw, buf[:]...)
}

func appendUint32(raw []byte, value uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], value)
	return append(raw, buf[:]...)
}

func appendUint64(raw []byte, value uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], value)
	return append(raw, buf[:]...)
}
