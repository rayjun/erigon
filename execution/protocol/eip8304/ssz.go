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

// zeroHashes[i] is the root of a full zero subtree of depth i (2^i zero
// leaves), i.e. hash(zeroHashes[i-1], zeroHashes[i-1]). Virtual zero padding
// uses these precomputed roots instead of materializing the padding leaves of
// a large list.
var zeroHashes = buildZeroHashes(64)

func buildZeroHashes(maxDepth int) []common.Hash {
	z := make([]common.Hash, maxDepth+1)
	z[0] = zeroHash
	for i := 1; i <= maxDepth; i++ {
		z[i] = hashPair(z[i-1], z[i-1])
	}
	return z
}

// listDepth returns the SSZ tree depth for a list with the given limit. A
// single chunk needs no internal levels (depth 0); larger limits use
// ceil(log2(limit)). This matches Erigon's cl/merkle_tree.GetDepth and
// remerkleable's get_depth: 0 for limit <= 1, else (limit-1).bit_length().
func listDepth(limit uint64) int {
	if limit <= 1 {
		return 0
	}
	return bits.Len64(limit - 1)
}

func hashPair(a, b common.Hash) common.Hash {
	var buf [64]byte
	copy(buf[:32], a[:])
	copy(buf[32:], b[:])
	return sha256.Sum256(buf[:])
}

// MerkleizeChunks builds the SHA2-256 Merkle root of `leaves`, padded with
// zero hashes to the SSZ list depth implied by `limit`. This is the
// "contents root" of an SSZ List whose elements are exactly one 32-byte chunk
// each, which is the case for List[Hash32, N] (the leaves of an EIP-8304
// table are the SHA2-256 hashes of its encoded entries).
//
// The zero padding is virtual: only the real leaves produce internal nodes,
// and every odd trailing node pairs with the precomputed root of its zero
// subtree. The result is identical to materializing a full 2^depth tree, but
// uses O(len(leaves)) memory instead of O(2^depth).
func MerkleizeChunks(leaves []common.Hash, limit uint64) (common.Hash, error) {
	if uint64(len(leaves)) > limit {
		return common.Hash{}, ErrExceedsListLimit
	}
	depth := listDepth(limit)
	if len(leaves) == 0 {
		return zeroHashes[depth], nil
	}
	if depth == 0 {
		// Only one leaf slot: the contents root is the leaf itself.
		return leaves[0], nil
	}

	cur := make([]common.Hash, len(leaves))
	copy(cur, leaves)
	for level := 0; level < depth; level++ {
		next := make([]common.Hash, 0, (len(cur)+1)/2)
		i := 0
		for ; i+1 < len(cur); i += 2 {
			next = append(next, hashPair(cur[i], cur[i+1]))
		}
		if i < len(cur) {
			next = append(next, hashPair(cur[i], zeroHashes[level]))
		}
		cur = next
	}
	return cur[0], nil
}

// ListHashRoot computes the SSZ hash tree root of a `List[Hash32, limit]`.
// The element count is mixed in as a little-endian uint256 chunk appended to
// the contents root, per the canonical SSZ definition. `limit` is the SSZ
// list bound supplied by the caller; it stays a parameter while the EIP-8304
// bound is open so that no hidden default enters consensus code.
func ListHashRoot(leaves []common.Hash, limit uint64) (common.Hash, error) {
	contents, err := MerkleizeChunks(leaves, limit)
	if err != nil {
		return common.Hash{}, err
	}
	var lengthBuf [32]byte
	binary.LittleEndian.PutUint64(lengthBuf[:8], uint64(len(leaves)))
	var buf [64]byte
	copy(buf[:32], contents[:])
	copy(buf[32:], lengthBuf[:])
	sum := sha256.Sum256(buf[:])
	return common.BytesToHash(sum[:]), nil
}
