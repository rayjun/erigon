package eip8304

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types"
)

const proofLimit = uint64(1 << 12)

var proofAddress = common.BytesToAddress([]byte{0x11, 0x11})

// fixedLogReceipts adds one log with a shared address and topic to every block
// in [first, last], so one condition matches a known run of entries. Blocks
// outside the range keep the default receipts, whose addresses are unique per
// block.
func fixedLogReceipts(first, last uint64, address common.Address, topic common.Hash) func(uint64) types.Receipts {
	return func(block uint64) types.Receipts {
		receipts := testReceipts(block)
		if block >= first && block <= last {
			receipts[0].Logs = append(receipts[0].Logs, &types.Log{Address: address, Topics: []common.Hash{topic}})
		}
		return receipts
	}
}

// proofHighBits returns an address-condition value whose leading bytes are set,
// which the right-alignment check must reject.
func proofHighBits() common.Hash {
	var value common.Hash
	binary.BigEndian.PutUint64(value[:8], 1)
	return value
}

func addressValue(address common.Address) common.Hash {
	var value common.Hash
	copy(value[hashLength-addressLength:], address[:])
	return value
}

func proofTable(t *testing.T, chain *chainStore, ref TableRef) TableResult {
	t.Helper()
	table, err := NewResolver(chain, proofLimit).Table(ref)
	require.NoError(t, err)
	require.Equal(t, len(table.Entries), int(table.EntryCount))
	return table
}

func TestProofProvesAnAddressConditionInABlockRange(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}

	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	require.Equal(t, ref, proof.Ref)
	require.Equal(t, table.EntryCount, proof.EntryCount)
	require.Equal(t, table.Root, proof.Root)
	require.Equal(t, uint64(6), proof.MatchCount, "one matching entry per covered block")

	// The revealed run is the matches plus one neighbour on each side, and the
	// match indexes are contiguous.
	require.Equal(t, proof.MatchStart-1, proof.Start)
	require.Equal(t, proof.MatchStart+proof.MatchCount, proof.Start+uint64(len(proof.Leaves))-1)

	for i := proof.MatchStart; i < proof.MatchStart+proof.MatchCount; i++ {
		entry, consumed, err := DecodeEntry(proof.Leaves[i-proof.Start])
		require.NoError(t, err)
		require.Equal(t, len(proof.Leaves[i-proof.Start]), consumed)
		require.Equal(t, EntryLogAddress, entry.Type)
		require.Equal(t, addressValue(proofAddress), entry.Value)
		require.Equal(t, 4+i-proof.MatchStart, entry.block)
	}

	lower, upper := query.bounds()
	left, _, err := DecodeEntry(proof.Leaves[0])
	require.NoError(t, err)
	require.Negative(t, bytes.Compare(left.Encode(), lower), "left neighbour must sort below the queried keys")
	right, _, err := DecodeEntry(proof.Leaves[len(proof.Leaves)-1])
	require.NoError(t, err)
	require.Positive(t, bytes.Compare(right.Encode(), upper), "right neighbour must sort above the queried keys")

	require.NoError(t, VerifyTableProof(proof, proofLimit))
}

func TestProofProvesATopicCondition(t *testing.T) {
	topic := testHash(0x777)
	chain := newTestChainWith(t, 16, fixedLogReceipts(2, 5, proofAddress, topic))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})

	// The bounded range cuts the run: only blocks 3..5 are asked for.
	query := ProofQuery{FirstBlock: 3, LastBlock: 5, Type: EntryLogTopic0, Value: topic}
	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	require.Equal(t, uint64(3), proof.MatchCount)
	require.NoError(t, VerifyTableProof(proof, proofLimit))
}

func TestProofProvesARunThatEndsOnTheLastLeaf(t *testing.T) {
	lastAddress := common.BytesToAddress(bytes.Repeat([]byte{0xff}, addressLength))
	lastTopic := common.HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	chain := newTestChainWith(t, 16, fixedLogReceipts(15, 15, lastAddress, lastTopic))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})

	query := ProofQuery{FirstBlock: 15, LastBlock: 15, Type: EntryLogTopic0, Value: lastTopic}
	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	require.Equal(t, uint64(1), proof.MatchCount)
	require.Equal(t, proof.EntryCount-1, proof.MatchStart, "the maximum key is the last leaf")
	require.Equal(t, proof.Start+uint64(len(proof.Leaves)), proof.EntryCount, "no neighbour above the last leaf")
	require.NoError(t, VerifyTableProof(proof, proofLimit))
}

func TestProofReportsAnEmptyResult(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})

	t.Run("absent condition", func(t *testing.T) {
		query := ProofQuery{FirstBlock: 0, LastBlock: 15, Type: EntryLogAddress, Value: addressValue(common.BytesToAddress([]byte{0x99}))}
		proof, err := ProveTable(table, query, proofLimit)
		require.NoError(t, err)
		require.Zero(t, proof.MatchCount)
		require.LessOrEqual(t, len(proof.Leaves), 2, "an empty result reveals only the neighbours")
		require.NoError(t, VerifyTableProof(proof, proofLimit))
	})

	t.Run("condition outside the block range", func(t *testing.T) {
		for _, blocks := range [][2]uint64{{0, 3}, {10, 15}} {
			query := ProofQuery{FirstBlock: blocks[0], LastBlock: blocks[1], Type: EntryLogAddress, Value: addressValue(proofAddress)}
			proof, err := ProveTable(table, query, proofLimit)
			require.NoError(t, err)
			require.Zero(t, proof.MatchCount, "blocks [%d, %d] hold no match", blocks[0], blocks[1])
			require.NoError(t, VerifyTableProof(proof, proofLimit))
		}
	})

	t.Run("range overlaps the run", func(t *testing.T) {
		query := ProofQuery{FirstBlock: 8, LastBlock: 15, Type: EntryLogAddress, Value: addressValue(proofAddress)}
		proof, err := ProveTable(table, query, proofLimit)
		require.NoError(t, err)
		require.Equal(t, uint64(2), proof.MatchCount)
		require.NoError(t, VerifyTableProof(proof, proofLimit))
	})
}

func TestProofEncodingRoundTripsAndIsVersioned(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})
	proof, err := ProveTable(table, ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}, proofLimit)
	require.NoError(t, err)

	raw := EncodeTableProof(proof)
	require.Equal(t, byte(ProofFormatVersion), raw[0])
	require.Equal(t, raw, EncodeTableProof(proof), "the encoding is canonical")
	decoded, err := DecodeTableProof(raw)
	require.NoError(t, err)
	require.Equal(t, proof, decoded)
	require.NoError(t, VerifyTableProof(decoded, proofLimit))
}

func TestProofRejectsTamperedProofs(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	base, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	require.NoError(t, VerifyTableProof(base, proofLimit))

	emptyQuery := ProofQuery{FirstBlock: 10, LastBlock: 15, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	empty, err := ProveTable(table, emptyQuery, proofLimit)
	require.NoError(t, err)
	require.Zero(t, empty.MatchCount)

	clone := func(p TableProof) TableProof {
		out := p
		out.Leaves = append([]EncodedEntry(nil), p.Leaves...)
		for i := range out.Leaves {
			out.Leaves[i] = append(EncodedEntry(nil), p.Leaves[i]...)
		}
		out.Siblings = append([]common.Hash(nil), p.Siblings...)
		return out
	}

	cases := []struct {
		name   string
		mutate func(*TableProof)
		want   error
	}{
		{"flipped leaf byte", func(p *TableProof) { p.Leaves[1][10] ^= 0xff }, ErrProofRootMismatch},
		{"deleted leaf", func(p *TableProof) { p.Leaves = append(p.Leaves[:2], p.Leaves[3:]...) }, ErrProofCoverage},
		{"gap below the run", func(p *TableProof) { p.Start++ }, ErrProofCoverage},
		{"swapped leaves", func(p *TableProof) { p.Leaves[1], p.Leaves[2] = p.Leaves[2], p.Leaves[1] }, ErrProofRootMismatch},
		{"duplicated leaf", func(p *TableProof) { p.Leaves[2] = p.Leaves[1] }, ErrProofRootMismatch},
		{"wrong root", func(p *TableProof) { p.Root[0] ^= 0xff }, ErrProofRootMismatch},
		{"wrong entry count", func(p *TableProof) { p.EntryCount++ }, ErrProofRootMismatch},
		{"dropped sibling", func(p *TableProof) { p.Siblings = p.Siblings[:len(p.Siblings)-1] }, ErrProofCoverage},
		{"extra sibling", func(p *TableProof) { p.Siblings = append(p.Siblings, common.Hash{}) }, ErrProofCoverage},
		{"padding leaf claimed", func(p *TableProof) { p.EntryCount = p.Start + uint64(len(p.Leaves)) - 1 }, ErrProofCoverage},
		{"match run shifted", func(p *TableProof) { p.MatchStart++ }, ErrProofCoverage},
		{"extra match", func(p *TableProof) { p.MatchCount++ }, ErrProofCoverage},
		{"unsupported version", func(p *TableProof) { p.Version = 2 }, ErrProofVersion},
		{"query outside the table", func(p *TableProof) { p.Query.FirstBlock = 16; p.Query.LastBlock = 20 }, ErrProofQuery},
		{"block range reversed", func(p *TableProof) { p.Query.FirstBlock, p.Query.LastBlock = p.Query.LastBlock, p.Query.FirstBlock }, ErrProofQuery},
		{"condition type not supported", func(p *TableProof) { p.Query.Type = EntryTransaction }, ErrProofQuery},
		{"missing match", func(p *TableProof) { p.MatchCount-- }, ErrProofBoundary},
		{"empty claimed for a match", func(p *TableProof) { p.MatchCount = 0 }, ErrProofBoundary},
		{"range shifted off the run", func(p *TableProof) { p.Query.FirstBlock++; p.Query.LastBlock++ }, ErrProofBoundary},
		{"match claimed at the table start", func(p *TableProof) { p.MatchStart = 0 }, ErrProofCoverage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tampered := clone(base)
			tc.mutate(&tampered)
			require.ErrorIs(t, VerifyTableProof(tampered, proofLimit), tc.want)
		})
	}

	t.Run("empty proof claimed non-empty", func(t *testing.T) {
		tampered := clone(empty)
		tampered.MatchCount = 1
		require.ErrorIs(t, VerifyTableProof(tampered, proofLimit), ErrProofCoverage)
	})
	t.Run("empty proof at a wrong insertion point", func(t *testing.T) {
		tampered := clone(empty)
		tampered.MatchStart++
		require.ErrorIs(t, VerifyTableProof(tampered, proofLimit), ErrProofCoverage)
	})
	t.Run("non-empty proof claimed empty", func(t *testing.T) {
		tampered := clone(base)
		tampered.MatchCount = 0
		require.ErrorIs(t, VerifyTableProof(tampered, proofLimit), ErrProofBoundary)
	})
}

// TestProofOverATableReadBackFromTheHotStore covers the shape the finalize path
// will hand the prover: a table that came back from the versioned store rather
// than straight out of the resolver, and an encoded proof that survives the
// round trip.
func TestProofOverATableReadBackFromTheHotStore(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)

	tx := beginHotStoreTx(t, db)
	require.NoError(t, newTestHotStore(t, tx, chain, 15).PutTable(table))
	require.NoError(t, tx.Commit())

	tx2 := beginHotStoreTx(t, db)
	defer tx2.Rollback()
	stored, ok, err := newTestHotStore(t, tx2, chain, 15).GetTable(ref)
	require.NoError(t, err)
	require.True(t, ok)

	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	proof, err := ProveTable(stored, query, hotStoreTestLimit)
	require.NoError(t, err)
	require.Equal(t, table.Root, proof.Root, "the proof anchors to the stored table's root")
	require.NoError(t, VerifyTableProof(proof, hotStoreTestLimit))

	decoded, err := DecodeTableProof(EncodeTableProof(proof))
	require.NoError(t, err)
	require.Equal(t, proof, decoded)
	require.NoError(t, VerifyTableProof(decoded, hotStoreTestLimit))
}

// TestProofBindsTheRunNotTheQueriedRange pins what the verifier actually
// establishes: the revealed run is the complete set of matching entries inside
// the queried keys, whatever range the generator happened to ask about.
func TestProofBindsTheRunNotTheQueriedRange(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})
	proof, err := ProveTable(table, ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}, proofLimit)
	require.NoError(t, err)
	require.Equal(t, uint64(6), proof.MatchCount)

	// Blocks 3 and 10 hold no entry for this address, so the wider range has
	// the same run and the same answer, and the neighbours already bound it.
	wider := proof
	wider.Query.FirstBlock, wider.Query.LastBlock = 3, 10
	require.NoError(t, VerifyTableProof(wider, proofLimit))

	// A range that pushes a match out of the queried keys is rejected: the
	// claimed run is no longer the entries inside the range.
	narrowed := proof
	narrowed.Query.LastBlock = 8
	require.ErrorIs(t, VerifyTableProof(narrowed, proofLimit), ErrProofBoundary)

	narrowed = proof
	narrowed.Query.FirstBlock = 5
	require.ErrorIs(t, VerifyTableProof(narrowed, proofLimit), ErrProofBoundary)
}

func TestProofRejectsMalformedLeaves(t *testing.T) {
	ref := TableRef{FirstBlock: 0, TableSize: 4}
	query := ProofQuery{FirstBlock: 0, LastBlock: 3, Type: EntryLogAddress, Value: addressValue(proofAddress)}

	t.Run("trailing bytes in a leaf", func(t *testing.T) {
		leaf := NewLogAddressEntry(0, 0, 0, proofAddress).Encode()
		proof := TableProof{Version: ProofFormatVersion, Ref: ref, EntryCount: 1, Query: query, Leaves: []EncodedEntry{append(leaf, 0x00)}, MatchStart: 0, MatchCount: 1}
		require.ErrorIs(t, VerifyTableProof(proof, proofLimit), ErrProofEntry)
	})
	t.Run("unknown entry type", func(t *testing.T) {
		proof := TableProof{Version: ProofFormatVersion, Ref: ref, EntryCount: 1, Query: query, Leaves: []EncodedEntry{{0xff, 0xff}}, MatchStart: 0, MatchCount: 1}
		require.ErrorIs(t, VerifyTableProof(proof, proofLimit), ErrProofEntry)
	})
	t.Run("leaf for a block outside the table", func(t *testing.T) {
		leaf := NewLogAddressEntry(99, 0, 0, proofAddress).Encode()
		proof := TableProof{Version: ProofFormatVersion, Ref: ref, EntryCount: 1, Query: query, Leaves: []EncodedEntry{leaf}, MatchStart: 0, MatchCount: 1}
		require.ErrorIs(t, VerifyTableProof(proof, proofLimit), ErrProofCoverage)
	})
}

func TestProofDecodeRejectsTamperedEncodings(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})
	proof, err := ProveTable(table, ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}, proofLimit)
	require.NoError(t, err)
	raw := EncodeTableProof(proof)

	t.Run("truncated", func(t *testing.T) {
		_, err := DecodeTableProof(raw[:len(raw)-1])
		require.ErrorIs(t, err, ErrProofCorrupt)
	})
	t.Run("extended", func(t *testing.T) {
		_, err := DecodeTableProof(append(append([]byte{}, raw...), 0x00))
		require.ErrorIs(t, err, ErrProofCorrupt)
	})
	t.Run("bad checksum", func(t *testing.T) {
		corrupt := append([]byte{}, raw...)
		corrupt[len(corrupt)-1] ^= 0xff
		_, err := DecodeTableProof(corrupt)
		require.ErrorIs(t, err, ErrProofCorrupt)
	})
	t.Run("unknown version", func(t *testing.T) {
		unknown := append([]byte{}, raw...)
		unknown[0] = ProofFormatVersion + 1
		// Keep the checksum valid so the version check, not the CRC, fires.
		unknown = resealProof(unknown)
		_, err := DecodeTableProof(unknown)
		require.ErrorIs(t, err, ErrProofVersion)
	})
	t.Run("leaf count beyond the input", func(t *testing.T) {
		huge := append([]byte{}, raw...)
		// The leaf count follows version(1)+ref(16)+count(8)+root(32)+query(50)+start(8)+match(16).
		at := 1 + 8 + 8 + 8 + hashLength + 8 + 8 + 2 + hashLength + 8 + 8 + 8
		huge[at], huge[at+1], huge[at+2], huge[at+3] = 0xff, 0xff, 0xff, 0xff
		huge = resealProof(huge)
		_, err := DecodeTableProof(huge)
		require.ErrorIs(t, err, ErrProofCorrupt)
	})
}

// resealProof rewrites the CRC32 trailer after a test mutates the body.
func resealProof(raw []byte) []byte {
	binary.BigEndian.PutUint32(raw[len(raw)-4:], crc32.ChecksumIEEE(raw[:len(raw)-4]))
	return raw
}

func TestProveTableRejectsQueriesTheTableCannotAnswer(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)

	cases := []struct {
		name  string
		query ProofQuery
	}{
		{"range above the table", ProofQuery{FirstBlock: 16, LastBlock: 20, Type: EntryLogAddress, Value: addressValue(proofAddress)}},
		{"range below the table", ProofQuery{FirstBlock: 0, LastBlock: 20, Type: EntryLogAddress, Value: addressValue(proofAddress)}},
		{"reversed range", ProofQuery{FirstBlock: 9, LastBlock: 4, Type: EntryLogAddress, Value: addressValue(proofAddress)}},
		{"block condition", ProofQuery{FirstBlock: 0, LastBlock: 3, Type: EntryBlock, Value: testHash(1)}},
		{"transaction condition", ProofQuery{FirstBlock: 0, LastBlock: 3, Type: EntryTransaction, Value: testHash(1)}},
		{"address not right aligned", ProofQuery{FirstBlock: 0, LastBlock: 3, Type: EntryLogAddress, Value: proofHighBits()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ProveTable(table, tc.query, proofLimit)
			require.ErrorIs(t, err, ErrProofQuery)
			// The verifier rejects the same query when it arrives pre-built.
			proof := TableProof{Version: ProofFormatVersion, Ref: ref, EntryCount: table.EntryCount, Root: table.Root, Query: tc.query, Leaves: []EncodedEntry{NewLogAddressEntry(0, 0, 0, proofAddress).Encode()}, MatchStart: 0, MatchCount: 1}
			require.ErrorIs(t, VerifyTableProof(proof, proofLimit), ErrProofQuery)
		})
	}
}

func TestProveTableRejectsAnInconsistentTable(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}

	t.Run("entry count does not match the entries", func(t *testing.T) {
		broken := table
		broken.EntryCount++
		_, err := ProveTable(broken, query, proofLimit)
		require.ErrorIs(t, err, ErrTableEntryCount)
	})
	t.Run("root does not match the entries", func(t *testing.T) {
		broken := table
		broken.Root[0] ^= 0xff
		_, err := ProveTable(broken, query, proofLimit)
		require.ErrorIs(t, err, ErrTableRootMismatch)
	})
	t.Run("entries are not in canonical order", func(t *testing.T) {
		broken := table
		broken.Entries = append(Entries(nil), table.Entries...)
		broken.Entries[0], broken.Entries[1] = broken.Entries[1], broken.Entries[0]
		_, err := ProveTable(broken, query, proofLimit)
		require.ErrorIs(t, err, ErrTableUnsortedEntries)
	})
}

func TestVerifyRejectsAProofUnderADifferentListBound(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 16})
	proof, err := ProveTable(table, ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}, proofLimit)
	require.NoError(t, err)

	require.NoError(t, VerifyTableProof(proof, proofLimit))
	require.ErrorIs(t, VerifyTableProof(proof, proofLimit*2), ErrProofCoverage, "the list bound changes the tree depth")
}

func TestProofOverAFullSizeTable(t *testing.T) {
	// The largest table level, so the proof runs over a 256-block window.
	chain := newTestChainWith(t, 256, fixedLogReceipts(200, 255, proofAddress, testHash(900)))
	table := proofTable(t, chain, TableRef{FirstBlock: 0, TableSize: 256})

	query := ProofQuery{FirstBlock: 250, LastBlock: 255, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	require.Equal(t, uint64(6), proof.MatchCount)
	require.NoError(t, VerifyTableProof(proof, proofLimit))
	require.Less(t, len(proof.Siblings), 2*listDepth(proofLimit), "the multi-proof is logarithmic in the list, not in the revealed run")
}
