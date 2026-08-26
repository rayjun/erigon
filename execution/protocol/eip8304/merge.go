package eip8304

import (
	"bytes"
	"container/heap"
	"errors"
)

// ErrUnsortedMergeInput reports a merge input whose entries are not in
// lexicographic order. A higher-level table must be produced only from sorted
// lower-level tables; silently merging an unsorted stream would create a
// non-canonical table.
var ErrUnsortedMergeInput = errors.New("eip8304: merge input stream is not sorted")

// MergeSorted merges already-lexicographically-sorted entry streams into a
// single lexicographically-sorted result. The EIP's higher-level tables are
// produced by merging exactly four adjacent ordered lower-level tables, but
// the merge itself is generic so tests and any future rebuild path can reuse
// it.
//
// The result is the ordered union of the inputs: duplicates are preserved
// (an index table is an ordered list, not a set). Ordering uses the canonical
// encoded representation, identical to (Entries).Sort.
//
// Each input is encoded exactly once (per entry) and the streams are walked
// with a min-heap, so merging takes O(N·log k) time and O(N) memory without
// re-encoding entries on every comparison. Ties between equal keys from
// different streams are broken by stream index, making the output
// deterministic. Any unsorted input is rejected before any output is built.
func MergeSorted(streams ...Entries) (Entries, error) {
	streams = nonEmpty(streams)
	if len(streams) == 0 {
		return nil, nil
	}
	if len(streams) == 1 {
		out := append(Entries(nil), streams[0]...)
		if err := checkedSortedKeys(streams[0]); err != nil {
			return nil, err
		}
		return out, nil
	}

	total := 0
	h := make(entryHeap, 0, len(streams))
	for idx, s := range streams {
		st := &entryStream{entries: s, idx: idx}
		st.keys = make([][]byte, len(s))
		for i, e := range s {
			st.keys[i] = e.Encode()
			if i > 0 && bytes.Compare(st.keys[i-1], st.keys[i]) > 0 {
				return nil, ErrUnsortedMergeInput
			}
		}
		total += len(s)
		h = append(h, st)
	}
	heap.Init(&h)

	out := make(Entries, 0, total)
	for h.Len() > 0 {
		top := h[0]
		out = append(out, top.entries[top.pos])
		top.pos++
		if top.pos >= len(top.entries) {
			heap.Pop(&h)
		} else {
			heap.Fix(&h, 0)
		}
	}
	return out, nil
}

func nonEmpty(streams []Entries) []Entries {
	out := make([]Entries, 0, len(streams))
	for _, s := range streams {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func checkedSortedKeys(entries Entries) error {
	if len(entries) < 2 {
		return nil
	}
	prev := entries[0].Encode()
	for i := 1; i < len(entries); i++ {
		next := entries[i].Encode()
		if bytes.Compare(prev, next) > 0 {
			return ErrUnsortedMergeInput
		}
		prev = next
	}
	return nil
}

// entryStream is one sorted input to MergeSorted. keys[pos] is the encoded
// form of entries[pos], computed once up front. idx ties equal keys across
// streams so the merge is deterministic.
type entryStream struct {
	entries Entries
	keys    [][]byte
	idx     int
	pos     int
}

// entryHeap is a min-heap over entry streams, ordered by the encoded form of
// each stream's current head, then by stream index for determinism.
type entryHeap []*entryStream

func (h entryHeap) Len() int { return len(h) }

func (h entryHeap) Less(i, j int) bool {
	cmp := bytes.Compare(h[i].keys[h[i].pos], h[j].keys[h[j].pos])
	if cmp != 0 {
		return cmp < 0
	}
	return h[i].idx < h[j].idx
}

func (h entryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *entryHeap) Push(x any) { *h = append(*h, x.(*entryStream)) }

func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
