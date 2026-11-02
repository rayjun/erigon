package eip8304

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/hexutil"
	"github.com/erigontech/erigon/execution/commitment/trie"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

var (
	ErrProofAnchor  = errors.New("eip8304: proof is not anchored to the expected header")
	ErrProofStorage = errors.New("eip8304: system-contract storage does not bind the table root")
	ErrProofStale   = errors.New("eip8304: table root has left the ring buffer")
)

// QueryVerdict separates a valid table proof from a complete answer. ProofValid
// is false when the proof does not authenticate the expected table under the
// expected header. QueryComplete is false when the authenticated leaves do not
// cover exactly the queried run and its existing neighbours. QueryIssue then
// names the query-level reason (ErrProofQueryMismatch or ErrQueryIncomplete).
//
// VerifyQueryProof returns a non-nil error only when ProofValid is false. A
// valid proof that answers a different or incomplete query returns a nil error
// with QueryComplete=false and QueryIssue set, so callers can use err != nil as
// the authentication signal without losing the query diagnosis.
type QueryVerdict struct {
	ProofValid    bool
	QueryComplete bool
	QueryIssue    error
}

// StorageAnchor is the execution header and eth_getProof-style witness that
// bind a table root to the system index contract. AccountProof must contain
// exactly one storage proof, for TableStorageSlot(expectedRef).
//
// The anchor deliberately carries no trusted configuration: the caller passes
// the expected header hash and index-contract address separately, so a witness
// cannot authenticate itself by naming its own contract.
type StorageAnchor struct {
	Header  *types.Header
	Account accounts.AccProofResult
}

// VerifyQueryProof verifies a table proof against expectedRef and expectedRoot,
// authenticates expectedRoot through the index contract at anchor.Header, and
// checks that the proof answers expectedQuery completely. It has no database or
// RPC dependency: all state evidence is in anchor.Account, and the caller is
// responsible for establishing that anchor.Header is canonical.
//
// Every trust input is explicit. expectedHeaderHash pins the whole header,
// including its number and post-state root, so a genuine old state witness
// cannot be relabelled with a recent block number. expectedIndexContract pins
// the contract whose storage is read. expectedQuery pins the question the
// proof must answer; a proof for another valid query is still a valid table
// proof, but QueryComplete is false.
func VerifyQueryProof(
	proof TableProof,
	expectedRef TableRef,
	expectedRoot common.Hash,
	expectedQuery ProofQuery,
	expectedHeaderHash common.Hash,
	expectedIndexContract common.Address,
	anchor StorageAnchor,
	limit uint64,
) (QueryVerdict, error) {
	verdict, proofErr := verifyTableProofAgainst(proof, expectedRef, expectedRoot, expectedQuery, limit)
	if !verdict.ProofValid {
		return verdict, proofErr
	}
	if proofErr != nil {
		verdict.QueryIssue = proofErr
	}
	if err := verifyStorageBinding(expectedRef, expectedRoot, expectedHeaderHash, expectedIndexContract, anchor); err != nil {
		verdict.ProofValid = false
		verdict.QueryComplete = false
		verdict.QueryIssue = nil
		return verdict, err
	}
	return verdict, nil
}

// TableStorageSlot returns the index contract slot for ref as specified by
// EIP-8304: table_size * TABLES_PER_LEVEL + (first_block / table_size) % TABLES_PER_LEVEL.
func TableStorageSlot(ref TableRef) (common.Hash, error) {
	if err := validateTableRef(ref); err != nil {
		return common.Hash{}, err
	}
	slot := ref.TableSize*TablesPerLevel + (ref.FirstBlock/ref.TableSize)%TablesPerLevel
	var out common.Hash
	binary.BigEndian.PutUint64(out[hashLength-8:], slot)
	return out, nil
}

func verifyStorageBinding(
	ref TableRef,
	expectedRoot common.Hash,
	expectedHeaderHash common.Hash,
	expectedIndexContract common.Address,
	anchor StorageAnchor,
) error {
	if anchor.Header == nil {
		return fmt.Errorf("%w: missing execution header", ErrProofAnchor)
	}
	// Header.Hash caches its result on first use. The anchor is caller-supplied
	// and mutable, so a cached hash could otherwise survive a later change to
	// Number or Root. Recompute from the current fields every time.
	if got := types.RlpHash(anchor.Header); got != expectedHeaderHash {
		return fmt.Errorf("%w: header hash %x, want %x", ErrProofAnchor, got, expectedHeaderHash)
	}
	if !anchor.Header.Number.IsUint64() {
		return fmt.Errorf("%w: block number %s does not fit uint64", ErrProofAnchor, anchor.Header.Number.String())
	}
	anchorBlock := anchor.Header.Number.Uint64()
	if err := checkAnchorWindow(ref, anchorBlock); err != nil {
		return err
	}
	if anchor.Account.Address != expectedIndexContract {
		return fmt.Errorf("%w: account %s is not the index contract %s", ErrProofStorage, anchor.Account.Address, expectedIndexContract)
	}
	if anchor.Account.Balance == nil {
		return fmt.Errorf("%w: account proof has no balance", ErrProofStorage)
	}
	if len(anchor.Account.StorageProof) != 1 {
		return fmt.Errorf("%w: have %d storage proofs, want 1", ErrProofStorage, len(anchor.Account.StorageProof))
	}
	slot, err := TableStorageSlot(ref)
	if err != nil {
		return err
	}
	storageProof := anchor.Account.StorageProof[0]
	if !storageProofKeyMatchesSlot(storageProof.Key, slot) {
		return fmt.Errorf("%w: storage proof is for key %q, want slot %x", ErrProofStorage, storageProof.Key, slot)
	}
	if err := trie.VerifyAccountProof(anchor.Header.Root, &anchor.Account); err != nil {
		return fmt.Errorf("%w: account proof: %w", ErrProofStorage, err)
	}
	if storageProof.Value == nil {
		return fmt.Errorf("%w: storage proof has no value", ErrProofStorage)
	}
	slotHash := crypto.Keccak256Hash(slot[:])
	if err := trie.VerifyStorageProofByHash(anchor.Account.StorageHash, slotHash, storageProof); err != nil {
		return fmt.Errorf("%w: storage proof: %w", ErrProofStorage, err)
	}
	value := storageProof.Value.ToInt()
	if value.Sign() < 0 || value.BitLen() > hashLength*8 {
		return fmt.Errorf("%w: storage value does not fit a 32-byte root", ErrProofStorage)
	}
	got := common.BytesToHash(value.Bytes())
	if got != expectedRoot {
		return fmt.Errorf("%w: storage has root %x, want %x", ErrProofStorage, got, expectedRoot)
	}
	return nil
}

// checkAnchorWindow rejects a header from before the table was written and a
// table whose ring-buffer slot has since been overwritten at anchorBlock.
func checkAnchorWindow(ref TableRef, anchorBlock uint64) error {
	if err := validateTableRef(ref); err != nil {
		return err
	}
	writeBlock := WriteBlock(ref.FirstBlock, ref.TableSize)
	if anchorBlock < writeBlock {
		return fmt.Errorf("%w: block %d is before write block %d of %+v", ErrProofAnchor, anchorBlock, writeBlock, ref)
	}

	offset := ref.TableSize - 1 + ref.TableSize/4
	newestIndex := (anchorBlock - offset) / ref.TableSize
	index := ref.FirstBlock / ref.TableSize
	if index > newestIndex {
		return fmt.Errorf("%w: block %d is before table index %d of %+v", ErrProofAnchor, anchorBlock, index, ref)
	}
	if newestIndex-index >= TablesPerLevel {
		return fmt.Errorf("%w: table index %d is older than %d at block %d", ErrProofStale, index, newestIndex-TablesPerLevel+1, anchorBlock)
	}
	return nil
}

func storageProofKeyMatchesSlot(key string, slot common.Hash) bool {
	raw, err := hexutil.Decode(key)
	if err != nil || len(raw) > hashLength {
		return false
	}
	var got common.Hash
	copy(got[hashLength-len(raw):], raw)
	return got == slot
}
