package eip8304

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/bits"

	"github.com/erigontech/erigon/common"
)

// ErrExceedsListLimit reports more leaves than the SSZ list limit allows.
var ErrExceedsListLimit = errors.New("ssz list: leaf count exceeds limit")

// zeroHash is the SSZ padding leaf: 32 zero bytes.
var zeroHash common.Hash

// listDepth returns the SSZ tree depth for a list with the given limit: the
// smallest d (>= 1) such that 2^d >= limit. SSZ defines this as
// max(1, (limit-1).bit_length()).
func listDepth(limit uint64) int {
	if limit <= 1 {
		return 1
	}
	return bits.Len64(limit - 1)
}

func hashPair(a, b common.Hash) common.Hash {
	sum := sha256.Sum256(append(append([]byte{}, a[:]...), b[:]...))
	return common.BytesToHash(sum[:])
}

// MerkleizeChunks builds the SHA2-256 Merkle root of `leaves`, padded with
// zero hashes to the SSZ list depth implied by `limit`. This is the
// "contents root" of an SSZ List whose elements are exactly one 32-byte chunk
// each, which is the case for List[Hash32, N] (the leaves of an EIP-8304
// table are the SHA2-256 hashes of its encoded entries).
//
// This mirrors the remerkleable implementation used to generate consensus SSZ
// test vectors: depth = listDepth(limit), leaves padded with zero hashes to
// 2^depth, then pairwise SHA2-256.
func MerkleizeChunks(leaves []common.Hash, limit uint64) (common.Hash, error) {
	if uint64(len(leaves)) > limit {
		return common.Hash{}, ErrExceedsListLimit
	}
	depth := listDepth(limit)
	total := 1 << depth

	nodes := make([]common.Hash, 0, total)
	nodes = append(nodes, leaves...)
	for len(nodes) < total {
		nodes = append(nodes, zeroHash)
	}
	for len(nodes) > 1 {
		next := make([]common.Hash, 0, len(nodes)/2)
		for i := 0; i < len(nodes); i += 2 {
			next = append(next, hashPair(nodes[i], nodes[i+1]))
		}
		nodes = next
	}
	return nodes[0], nil
}

// ListHashRoot computes the SSZ hash tree root of a `List[Hash32, limit]`.
// The element count is mixed in as a little-endian uint256 chunk appended to
// the contents root, per the canonical SSZ definition.
//
// `limit` is the SSZ list bound. The EIP-8304 specification currently keeps
// the exact bound (and whether it equals the table's entry count) as `<TBD>`.
// This implementation is parameterized on `limit` so that no hidden default
// enters consensus code before the bound is confirmed; callers must pass the
// confirmed bound, and no consensus fixture publishes a root until then.
func ListHashRoot(leaves []common.Hash, limit uint64) (common.Hash, error) {
	contents, err := MerkleizeChunks(leaves, limit)
	if err != nil {
		return common.Hash{}, err
	}
	var lengthBuf [32]byte
	binary.LittleEndian.PutUint64(lengthBuf[:8], uint64(len(leaves)))

	h := sha256.New()
	h.Write(contents[:])
	h.Write(lengthBuf[:])
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return common.BytesToHash(sum[:]), nil
}
