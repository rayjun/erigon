package eip8304_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/execmodule/execmoduletester"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/p2p/protocols/eth"
)

// independentRootFromImportedChain reimplements the entry encoding and SSZ list
// root from the EIP text. It deliberately imports no eip8304 calculation helper,
// so a shared encoding or Merkleization regression cannot make the two sides
// agree by construction.
func independentRootFromImportedChain(t *testing.T, ctx context.Context, emt *execmoduletester.ExecModuleTester, limit uint64, ref eip8304.TableRef) common.Hash {
	t.Helper()
	tx, err := emt.DB.BeginTemporalRo(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	var encoded [][]byte
	for number := ref.FirstBlock; number < ref.FirstBlock+ref.TableSize; number++ {
		block, err := emt.BlockReader.BlockByNumber(ctx, tx, number)
		require.NoError(t, err)
		require.NotNil(t, block)
		var receipts types.Receipts
		if number > 0 {
			receipts, err = emt.ReceiptsReader.GetReceipts(ctx, emt.ChainConfig, tx, block, eth.ReceiptsOpts{})
			require.NoError(t, err)
		}
		require.Len(t, receipts, len(block.Transactions()))

		if number > 0 {
			encoded = append(encoded, independentBlockEntry(number-1, block.ParentHash()))
		}
		var cumulativeLogs uint32
		for i, transaction := range block.Transactions() {
			encoded = append(encoded, independentTransactionEntry(number, uint32(i), cumulativeLogs, transaction.Hash()))
			for j, log := range receipts[i].Logs {
				encoded = append(encoded, independentAddressEntry(number, uint32(i), uint32(j), log.Address))
				for topicPosition, topic := range log.Topics {
					encoded = append(encoded, independentTopicEntry(number, uint32(i), uint32(j), uint8(topicPosition), topic))
				}
			}
			cumulativeLogs += uint32(len(receipts[i].Logs))
		}
	}
	slices.SortFunc(encoded, bytes.Compare)

	leaves := make([]common.Hash, len(encoded))
	for i := range encoded {
		leaves[i] = sha256.Sum256(encoded[i])
	}
	return independentListRoot(leaves, limit)
}

func independentBlockEntry(block uint64, hash common.Hash) []byte {
	out := make([]byte, 2+32+8)
	binary.BigEndian.PutUint16(out, 0)
	copy(out[2:34], hash[:])
	binary.BigEndian.PutUint64(out[34:], block)
	return out
}

func independentTransactionEntry(block uint64, transaction, cumulativeLogs uint32, hash common.Hash) []byte {
	out := make([]byte, 2+32+8+4+4)
	binary.BigEndian.PutUint16(out, 1)
	copy(out[2:34], hash[:])
	binary.BigEndian.PutUint64(out[34:], block)
	binary.BigEndian.PutUint32(out[42:], transaction)
	binary.BigEndian.PutUint32(out[46:], cumulativeLogs)
	return out
}

func independentAddressEntry(block uint64, transaction, logIndex uint32, address common.Address) []byte {
	out := make([]byte, 2+20+8+4+4)
	binary.BigEndian.PutUint16(out, 2)
	copy(out[2:22], address[:])
	binary.BigEndian.PutUint64(out[22:], block)
	binary.BigEndian.PutUint32(out[30:], transaction)
	binary.BigEndian.PutUint32(out[34:], logIndex)
	return out
}

func independentTopicEntry(block uint64, transaction, logIndex uint32, topicPosition uint8, topic common.Hash) []byte {
	out := make([]byte, 2+32+8+4+4)
	binary.BigEndian.PutUint16(out, 3+uint16(topicPosition))
	copy(out[2:34], topic[:])
	binary.BigEndian.PutUint64(out[34:], block)
	binary.BigEndian.PutUint32(out[42:], transaction)
	binary.BigEndian.PutUint32(out[46:], logIndex)
	return out
}

func independentListRoot(leaves []common.Hash, limit uint64) common.Hash {
	depth := 0
	if limit > 1 {
		depth = bits.Len64(limit - 1)
	}
	zero := common.Hash{}
	zeros := make([]common.Hash, depth+1)
	zeros[0] = zero
	for i := 1; i <= depth; i++ {
		zeros[i] = independentPair(zeros[i-1], zeros[i-1])
	}

	contents := zero
	switch {
	case len(leaves) == 0:
		contents = zeros[depth]
	case depth == 0:
		contents = leaves[0]
	default:
		cur := append([]common.Hash(nil), leaves...)
		for level := 0; level < depth; level++ {
			next := make([]common.Hash, 0, (len(cur)+1)/2)
			for i := 0; i < len(cur); i += 2 {
				right := zeros[level]
				if i+1 < len(cur) {
					right = cur[i+1]
				}
				next = append(next, independentPair(cur[i], right))
			}
			cur = next
		}
		contents = cur[0]
	}

	var length [32]byte
	binary.LittleEndian.PutUint64(length[:8], uint64(len(leaves)))
	return sha256.Sum256(append(contents[:], length[:]...))
}

func independentPair(left, right common.Hash) common.Hash {
	var input [64]byte
	copy(input[:32], left[:])
	copy(input[32:], right[:])
	return sha256.Sum256(input[:])
}
