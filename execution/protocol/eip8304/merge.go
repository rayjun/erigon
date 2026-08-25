package eip8304

import (
	"bytes"
	"container/heap"
)

// MergeSorted merges an arbitrary number of already-lexicographically-sorted
// entry streams into a single lexicographically-sorted result. The EIP's
// higher-level tables are produced by merging exactly four adjacent ordered
// lower-level tables (Mark ratio 4), but the merge itself is implemented
// generically so the same primitive can serve tests and any future 1:1
// rebuild comparison.
//
// Ordering key is the canonical encoded representation, identical to
// (Entries).Sort. Duplicate entries are preserved: an index table is an
// ordered list, not a set, so identical encodings must not be collapsed.
//
// Each input is encoded exactly once (per entry) and the merge then walks the
// streams with a min-heap, so the result is produced in O(N·log k) time and
// O(N) memory without re-encoding entries on every comparison.
func MergeSorted(streams ...Entries) Entries {
	streams = nonEmpty(streams)
	if len(streams) == 0 {
		return nil
	}
	if len(streams) == 1 {
		return append(Entries(nil), streams[0]...)
	}

	total := 0
	for _, s := range streams {
		total += len(s)
	}

	h := make(entryHeap, 0, len(streams))
	for _, s := range streams {
		st := &entryStream{entries: s}
		st.keys = make([][]byte, len(s))
		for i, e := range s {
			st.keys[i] = e.Encode()
		}
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
	return out
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

// entryStream is one sorted input to MergeSorted. keys[pos] is the encoded
// form of entries[pos], computed once up front.
type entryStream struct {
	entries Entries
	keys    [][]byte
	pos     int
}

// entryHeap is a min-heap over entry streams, ordered by the encoded form of
// each stream's current head.
type entryHeap []*entryStream

func (h entryHeap) Len() int { return len(h) }

func (h entryHeap) Less(i, j int) bool {
	return bytes.Compare(h[i].keys[h[i].pos], h[j].keys[h[j].pos]) < 0
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
