package misc

import (
	"encoding/binary"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/types/accounts"
)

// EIP-8304 index contract calldata is exactly three 32-byte words:
// first_block, table_size, table_root (all big-endian).
const eip8304CalldataLength = 96

// IndexCalldata encodes the three 32-byte calldata words for the EIP-8304
// index contract set() call. firstBlock and tableSize fit in uint64; the high
// bytes of their words are zero. tableRoot is copied as-is.
func IndexCalldata(firstBlock, tableSize uint64, tableRoot common.Hash) []byte {
	data := make([]byte, eip8304CalldataLength)
	binary.BigEndian.PutUint64(data[24:32], firstBlock)
	binary.BigEndian.PutUint64(data[56:64], tableSize)
	copy(data[64:96], tableRoot[:])
	return data
}

// ApplyIndexEip8304 invokes the protocol-mandated index contract update for a
// due table (firstBlock, tableSize) with the given tableRoot. The caller is
// the SYSTEM_ADDRESS and the callee is the (parameterized) index address.
//
// Failure semantics per spec-lock: a call to an address without code succeeds
// trivially in the EVM and is therefore silently harmless, while any real
// failure (e.g. a revert, which the index contract must not do on a
// well-formed set() call) must fail the block. This helper therefore
// propagates syscall errors instead of swallowing them.
func ApplyIndexEip8304(
	indexAddr accounts.Address,
	firstBlock, tableSize uint64,
	tableRoot common.Hash,
	syscall rules.SystemCall,
) error {
	_, err := syscall(indexAddr, IndexCalldata(firstBlock, tableSize, tableRoot))
	return err
}
