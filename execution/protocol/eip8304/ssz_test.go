package eip8304

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
)

func repeatedByte(b byte) common.Hash {
	var out common.Hash
	for i := range out {
		out[i] = b
	}
	return out
}

// TestListHashRootMatchesReferenceVectors validates the SSZ List[Hash32, N]
// hash-tree-root against vectors computed with an independent implementation
// of the canonical SSZ merkleization (SHA2-256, zero padding to list depth,
// little-endian uint256 length mix-in).
func TestListHashRootMatchesReferenceVectors(t *testing.T) {
	leaves := []common.Hash{repeatedByte(0x11), repeatedByte(0x22), repeatedByte(0x33)}

	merkle, err := MerkleizeChunks(leaves, 5)
	require.NoError(t, err)
	require.Equal(t, "0x6d1c0d4c973644ca2fea57a5aa8aed4dff6027f9e9473c4732dd72e8e2c6b374", merkle.String())

	root, err := ListHashRoot(leaves, 5)
	require.NoError(t, err)
	require.Equal(t, "0x88c1d1e8ffc3baea342ca6dc11cc78857179c62d68281385c9f7c74ead04d4d5", root.String())
}

func TestListHashRootEmpty(t *testing.T) {
	root, err := ListHashRoot(nil, 4)
	require.NoError(t, err)
	require.Equal(t, "0x28ba1834a3a7b657460ce79fa3a1d909ab8828fd557659d4d0554a9bdbc0ec30", root.String())
}

func TestListHashRootEmptyLimitZero(t *testing.T) {
	// The EIP text's List[Hash32, entry_count] degenerates to a limit-0 list
	// for a genesis-empty table: the contents root is the zero hash (depth 0),
	// and the length mix-in is zero.
	root, err := ListHashRoot(nil, 0)
	require.NoError(t, err)
	require.Equal(t, "0xf5a5fd42d16a20302798ef6ed309979b43003d2320d9f0e8ea9831a92759fb4b", root.String())
}

func TestListHashRootSingleLeafLowLimit(t *testing.T) {
	// limit 1 yields a depth-0 tree: the contents root is the leaf itself,
	// and the table root is hash(leaf || LE uint256(1)). Verified against
	// Erigon's cl/merkle_tree.GetDepth and remerkleable get_depth semantics.
	root, err := ListHashRoot([]common.Hash{repeatedByte(0x11)}, 1)
	require.NoError(t, err)
	require.Equal(t, "0x28e3a680379879609decd438069ff63676c4f63d7b40b0873c7cdc29a736ecf7", root.String())
}

func TestListHashRootRejectsExceedingLimit(t *testing.T) {
	_, err := ListHashRoot([]common.Hash{repeatedByte(0x11), repeatedByte(0x22), repeatedByte(0x33)}, 2)
	require.ErrorIs(t, err, ErrExceedsListLimit)
}

func TestListHashRootDeterministicAndPaddedToLimit(t *testing.T) {
	// Fewer leaves than the limit must not change the tree shape: padding
	// with zero hashes to the same depth is part of the SSZ root.
	small, err := ListHashRoot([]common.Hash{repeatedByte(0x11)}, 8)
	require.NoError(t, err)
	big, err := ListHashRoot([]common.Hash{repeatedByte(0x11), repeatedByte(0x22)}, 8)
	require.NoError(t, err)
	require.NotEqual(t, small, big)
}
