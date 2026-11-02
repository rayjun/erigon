package eip8304

import (
	"encoding/binary"
	"math"
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/hexutil"
	"github.com/erigontech/erigon/execution/commitment/trie"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

var verifierTestContract = common.HexToAddress("0x0000000000000000000000000000000000008304")

func TestQueryVerifierAcceptsAnAuthenticatedProof(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	anchor := authenticatedStorageAnchor(t, ref, table.Root, WriteBlock(ref.FirstBlock, ref.TableSize))

	verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
	require.NoError(t, err)
	require.True(t, verdict.ProofValid)
	require.True(t, verdict.QueryComplete)
	require.NoError(t, verdict.QueryIssue)
}

func TestQueryVerifierSupportsAnEmptyGenesisTable(t *testing.T) {
	chain := newTestChainWith(t, 1, func(uint64) types.Receipts { return nil })
	ref := L0(0)
	table := proofTable(t, chain, ref)
	require.Zero(t, table.EntryCount)
	query := ProofQuery{FirstBlock: 0, LastBlock: 0, Type: EntryLogAddress, Value: addressValue(proofAddress)}

	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	require.Zero(t, proof.EntryCount)
	require.Empty(t, proof.Leaves)
	require.Empty(t, proof.Siblings)
	require.NoError(t, VerifyTableProof(proof, proofLimit))

	anchor := authenticatedStorageAnchor(t, ref, table.Root, WriteBlock(0, 1))
	verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
	require.NoError(t, err)
	require.True(t, verdict.ProofValid)
	require.True(t, verdict.QueryComplete)

	t.Run("query mismatch", func(t *testing.T) {
		expectedQuery := query
		expectedQuery.Value = addressValue(common.BytesToAddress([]byte{0x99}))
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, expectedQuery, anchor, verifierTestContract)
		require.NoError(t, err, "a valid empty-table proof for another query is not an authentication failure")
		require.True(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
		require.ErrorIs(t, verdict.QueryIssue, ErrProofQueryMismatch)
	})

	// The SSZ bound stays explicit: with limit 0 the empty list root and proof
	// are still well defined.
	zeroTable, err := NewResolver(chain, 0).Table(ref)
	require.NoError(t, err)
	zeroProof, err := ProveTable(zeroTable, query, 0)
	require.NoError(t, err)
	zeroAnchor := authenticatedStorageAnchor(t, ref, zeroTable.Root, WriteBlock(0, 1))
	zeroVerdict, err := VerifyQueryProof(zeroProof, ref, zeroTable.Root, query, zeroAnchor.Header.Hash(), verifierTestContract, zeroAnchor, 0)
	require.NoError(t, err)
	require.True(t, zeroVerdict.ProofValid)
	require.True(t, zeroVerdict.QueryComplete)

	for name, mutate := range map[string]func(*TableProof){
		"leaf":        func(p *TableProof) { p.Leaves = []EncodedEntry{{0x00}} },
		"start":       func(p *TableProof) { p.Start = 1 },
		"match start": func(p *TableProof) { p.MatchStart = 1 },
		"match count": func(p *TableProof) { p.MatchCount = 1 },
		"sibling":     func(p *TableProof) { p.Siblings = []common.Hash{{0x01}} },
	} {
		t.Run("empty proof with "+name, func(t *testing.T) {
			bad := proof
			mutate(&bad)
			verdict, err := verifyTableProofAgainst(bad, ref, table.Root, query, proofLimit)
			require.ErrorIs(t, err, ErrProofCoverage)
			require.False(t, verdict.ProofValid)
		})
	}
}

func TestQueryVerifierRejectsStorageMutations(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	writeBlock := WriteBlock(ref.FirstBlock, ref.TableSize)

	t.Run("forged table root", func(t *testing.T) {
		tampered := proof
		tampered.Root[0] ^= 0xff
		anchor := authenticatedStorageAnchor(t, ref, tampered.Root, writeBlock)
		verdict, err := verifyQueryProof(t, tampered, ref, tampered.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofRootMismatch)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("forged storage value", func(t *testing.T) {
		forged := table.Root
		forged[0] ^= 0xff
		anchor := authenticatedStorageAnchor(t, ref, forged, writeBlock)
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStorage)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("wrong slot", func(t *testing.T) {
		wrongSlot, err := TableStorageSlot(ref)
		require.NoError(t, err)
		wrongSlot[hashLength-1] ^= 0xff
		anchor := authenticatedStorageAnchorAtSlot(t, wrongSlot, table.Root, writeBlock)
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStorage)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("stale ring-buffer value", func(t *testing.T) {
		staleAt := writeBlock + TablesPerLevel*ref.TableSize
		anchor := authenticatedStorageAnchor(t, ref, table.Root, staleAt)
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStale)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("header before the table write", func(t *testing.T) {
		anchor := authenticatedStorageAnchor(t, ref, table.Root, writeBlock-1)
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofAnchor)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("relabelled header", func(t *testing.T) {
		// Pin the genuine header first, then relabel it as a recent block. The
		// account witness is still valid for the old state root; only the
		// trusted header hash can tell the two headers apart.
		anchor := authenticatedStorageAnchor(t, ref, table.Root, writeBlock)
		trustedHash := anchor.Header.Hash()
		anchor.Header = &types.Header{Number: *uint256.NewInt(writeBlock + 1), Root: anchor.Header.Root}
		verdict, err := VerifyQueryProof(proof, ref, table.Root, query, trustedHash, verifierTestContract, anchor, proofLimit)
		require.ErrorIs(t, err, ErrProofAnchor)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("mutated header after a cached hash", func(t *testing.T) {
		// Header.Hash caches on first use. Prime the cache, then mutate the
		// caller-supplied header; the verifier must recompute from the current
		// fields instead of accepting the stale cached hash.
		anchor := authenticatedStorageAnchor(t, ref, table.Root, writeBlock)
		trustedHash := anchor.Header.Hash()
		anchor.Header.Number = *uint256.NewInt(writeBlock + 1)
		verdict, err := VerifyQueryProof(proof, ref, table.Root, query, trustedHash, verifierTestContract, anchor, proofLimit)
		require.ErrorIs(t, err, ErrProofAnchor)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("self-consistent header with the wrong state", func(t *testing.T) {
		// The caller can pin a self-consistent but non-canonical header. The
		// account proof must then fail against that header's state root.
		anchor := authenticatedStorageAnchor(t, ref, table.Root, writeBlock)
		anchor.Header.Root[0] ^= 0xff
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStorage)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("wrong index contract", func(t *testing.T) {
		otherContract := common.HexToAddress("0x0000000000000000000000000000000000008305")
		anchor := authenticatedStorageAnchorFor(t, otherContract, ref, table.Root, writeBlock)
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStorage)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("more than one storage proof", func(t *testing.T) {
		anchor := authenticatedStorageAnchor(t, ref, table.Root, writeBlock)
		anchor.Account.StorageProof = append(anchor.Account.StorageProof, anchor.Account.StorageProof[0])
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStorage)
		require.False(t, verdict.ProofValid)
	})

	t.Run("missing storage value does not panic", func(t *testing.T) {
		anchor := authenticatedStorageAnchor(t, ref, table.Root, writeBlock)
		anchor.Account.StorageProof[0].Value = nil
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStorage)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})

	t.Run("missing account balance does not panic", func(t *testing.T) {
		anchor := authenticatedStorageAnchor(t, ref, table.Root, writeBlock)
		anchor.Account.Balance = nil
		verdict, err := verifyQueryProof(t, proof, ref, table.Root, query, anchor, verifierTestContract)
		require.ErrorIs(t, err, ErrProofStorage)
		require.False(t, verdict.ProofValid)
		require.False(t, verdict.QueryComplete)
	})
}

func TestQueryVerifierRejectsADifferentExpectedTable(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	anchor := authenticatedStorageAnchor(t, ref, table.Root, WriteBlock(ref.FirstBlock, ref.TableSize))

	expected := TableRef{FirstBlock: 16, TableSize: 16}
	verdict, err := verifyQueryProof(t, proof, expected, table.Root, query, anchor, verifierTestContract)
	require.ErrorIs(t, err, ErrProofRefMismatch)
	require.False(t, verdict.ProofValid)
	require.False(t, verdict.QueryComplete)
}

func TestQueryVerifierRejectsAProofForAnotherQuery(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)
	proofQuery := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	proof, err := ProveTable(table, proofQuery, proofLimit)
	require.NoError(t, err)
	anchor := authenticatedStorageAnchor(t, ref, table.Root, WriteBlock(ref.FirstBlock, ref.TableSize))

	// The narrower query is answerable by the same table. The proof itself is
	// valid, but it does not answer the caller's question.
	expectedQuery := ProofQuery{FirstBlock: 4, LastBlock: 8, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	verdict, err := verifyQueryProof(t, proof, ref, table.Root, expectedQuery, anchor, verifierTestContract)
	require.NoError(t, err, "a valid proof with another query is not an authentication failure")
	require.True(t, verdict.ProofValid, "the table proof and storage binding are still valid")
	require.False(t, verdict.QueryComplete, "the proof answers another query")
	require.ErrorIs(t, verdict.QueryIssue, ErrProofQueryMismatch)
}

func TestQueryVerdictSeparatesValidityFromCompleteness(t *testing.T) {
	chain := newTestChainWith(t, 16, fixedLogReceipts(4, 9, proofAddress, testHash(900)))
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	table := proofTable(t, chain, ref)
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	proof, err := ProveTable(table, query, proofLimit)
	require.NoError(t, err)
	anchor := authenticatedStorageAnchor(t, ref, table.Root, WriteBlock(ref.FirstBlock, ref.TableSize))

	incomplete := proof
	incomplete.Query.LastBlock = 8
	verdict, err := verifyQueryProof(t, incomplete, ref, table.Root, incomplete.Query, anchor, verifierTestContract)
	require.NoError(t, err, "an incomplete query is not an authentication failure")
	require.True(t, verdict.ProofValid, "the tree proof and storage binding are still valid")
	require.False(t, verdict.QueryComplete, "the narrowed range omits a matching leaf")
	require.ErrorIs(t, verdict.QueryIssue, ErrQueryIncomplete)
	require.ErrorIs(t, verdict.QueryIssue, ErrProofBoundary)
}

func TestAnchorWindowCoversEveryTableSize(t *testing.T) {
	// The write blocks are literals so this test does not derive its expected
	// boundary from the production WriteBlock helper.
	for _, tc := range []struct {
		ref        TableRef
		writeBlock uint64
	}{
		{TableRef{FirstBlock: 4, TableSize: 1}, 4},
		{TableRef{FirstBlock: 16, TableSize: 4}, 20},
		{TableRef{FirstBlock: 64, TableSize: 16}, 83},
		{TableRef{FirstBlock: 256, TableSize: 64}, 335},
		{TableRef{FirstBlock: 1024, TableSize: 256}, 1343},
	} {
		require.Equal(t, tc.writeBlock, WriteBlock(tc.ref.FirstBlock, tc.ref.TableSize), "ref %+v", tc.ref)
		require.NoError(t, checkAnchorWindow(tc.ref, tc.writeBlock), "ref %+v", tc.ref)
		require.NoError(t, checkAnchorWindow(tc.ref, tc.writeBlock+TablesPerLevel*tc.ref.TableSize-1), "ref %+v oldest retained slot", tc.ref)
		require.ErrorIs(t, checkAnchorWindow(tc.ref, tc.writeBlock+TablesPerLevel*tc.ref.TableSize), ErrProofStale, "ref %+v", tc.ref)
		require.ErrorIs(t, checkAnchorWindow(tc.ref, tc.writeBlock-1), ErrProofAnchor, "ref %+v", tc.ref)
	}
}

func TestTableStorageSlotLiteralVectors(t *testing.T) {
	for _, tc := range []struct {
		ref  TableRef
		slot uint64
	}{
		{TableRef{FirstBlock: 1024, TableSize: 1}, 1024},
		{TableRef{FirstBlock: 4096, TableSize: 4}, 4096},
		{TableRef{FirstBlock: 65600, TableSize: 64}, 65537},
	} {
		slot, err := TableStorageSlot(tc.ref)
		require.NoError(t, err)
		require.Equal(t, slotOf(tc.slot), slot, "ref %+v", tc.ref)
	}

	_, err := TableStorageSlot(TableRef{FirstBlock: 3, TableSize: 4})
	require.ErrorIs(t, err, ErrInvalidTableRef)
}

func TestVerifierRejectsOverflowingProofRanges(t *testing.T) {
	ref := TableRef{FirstBlock: 0, TableSize: 16}
	query := ProofQuery{FirstBlock: 4, LastBlock: 9, Type: EntryLogAddress, Value: addressValue(proofAddress)}
	base := TableProof{Version: ProofFormatVersion, Ref: ref, EntryCount: 10, Root: common.Hash{1}, Query: query}

	t.Run("table reference", func(t *testing.T) {
		overflowing := TableRef{FirstBlock: math.MaxUint64 - 3, TableSize: 4}
		require.ErrorIs(t, validateTableRef(overflowing), ErrInvalidTableRef)
		require.ErrorIs(t, checkAnchorWindow(overflowing, 0), ErrInvalidTableRef)
		require.Panics(t, func() { WriteBlock(overflowing.FirstBlock, overflowing.TableSize) })
	})

	t.Run("revealed start", func(t *testing.T) {
		proof := base
		proof.Start = math.MaxUint64
		proof.Leaves = []EncodedEntry{{0x00}}
		verdict, err := verifyTableProofAgainst(proof, ref, proof.Root, query, proofLimit)
		require.ErrorIs(t, err, ErrProofCoverage)
		require.False(t, verdict.ProofValid)
	})

	t.Run("match start", func(t *testing.T) {
		proof := base
		proof.Start = 0
		proof.Leaves = []EncodedEntry{{0x00}}
		proof.MatchStart = math.MaxUint64
		proof.MatchCount = 2
		verdict, err := verifyTableProofAgainst(proof, ref, proof.Root, query, proofLimit)
		require.ErrorIs(t, err, ErrProofCoverage)
		require.False(t, verdict.ProofValid)
	})
}

func verifyQueryProof(t *testing.T, proof TableProof, ref TableRef, root common.Hash, query ProofQuery, anchor StorageAnchor, contract common.Address) (QueryVerdict, error) {
	t.Helper()
	require.NotNil(t, anchor.Header)
	return VerifyQueryProof(proof, ref, root, query, anchor.Header.Hash(), contract, anchor, proofLimit)
}

func authenticatedStorageAnchor(t *testing.T, ref TableRef, root common.Hash, headerBlock uint64) StorageAnchor {
	t.Helper()
	return authenticatedStorageAnchorFor(t, verifierTestContract, ref, root, headerBlock)
}

func authenticatedStorageAnchorAtSlot(t *testing.T, slot, value common.Hash, headerBlock uint64) StorageAnchor {
	t.Helper()
	return authenticatedStorageAnchorForSlot(t, verifierTestContract, slot, value, headerBlock)
}

func authenticatedStorageAnchorFor(t *testing.T, addr common.Address, ref TableRef, value common.Hash, headerBlock uint64) StorageAnchor {
	t.Helper()
	slot, err := TableStorageSlot(ref)
	require.NoError(t, err)
	return authenticatedStorageAnchorForSlot(t, addr, slot, value, headerBlock)
}

func authenticatedStorageAnchorForSlot(t *testing.T, addr common.Address, slot, value common.Hash, headerBlock uint64) StorageAnchor {
	t.Helper()
	addrHash := crypto.Keccak256Hash(addr[:])
	slotHash := crypto.Keccak256Hash(slot[:])
	encodedValue, err := rlp.EncodeToBytes(new(big.Int).SetBytes(value[:]).Bytes())
	require.NoError(t, err)

	state := trie.NewTestRLPTrie(common.Hash{})
	account := accounts.NewAccount()
	account.Nonce = 1
	account.Root = trie.EmptyRoot
	account.CodeHash = accounts.InternCodeHash(crypto.Keccak256Hash([]byte{0x60, 0x00}))
	state.UpdateAccount(addrHash[:], &account)
	fullKey := append(append([]byte{}, addrHash[:]...), slotHash[:]...)
	state.Update(fullKey, encodedValue)
	stateRoot := state.Hash()

	stored, ok := state.GetAccount(addrHash[:])
	require.True(t, ok)
	accountProof, err := state.Prove(addrHash[:], 0, false)
	require.NoError(t, err)
	storageProof, err := state.Prove(fullKey, len(accountProof), true)
	require.NoError(t, err)

	header := &types.Header{Number: *uint256.NewInt(headerBlock), Root: stateRoot}
	return StorageAnchor{
		Header: header,
		Account: accounts.AccProofResult{
			Address:      addr,
			AccountProof: toHexBytes(accountProof),
			Balance:      (*hexutil.Big)(new(big.Int)),
			CodeHash:     stored.CodeHash.Value(),
			Nonce:        hexutil.Uint64(stored.Nonce),
			StorageHash:  stored.Root,
			StorageProof: []accounts.StorProofResult{{
				Key:   slot.Hex(),
				Value: (*hexutil.Big)(new(big.Int).SetBytes(value[:])),
				Proof: toHexBytes(storageProof),
			}},
		},
	}
}

func slotOf(value uint64) common.Hash {
	var slot common.Hash
	binary.BigEndian.PutUint64(slot[hashLength-8:], value)
	return slot
}

func toHexBytes(in [][]byte) []hexutil.Bytes {
	out := make([]hexutil.Bytes, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}
