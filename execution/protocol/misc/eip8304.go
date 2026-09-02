// Copyright 2026 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

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

// IndexCalldata encodes first_block, table_size, and table_root as three
// big-endian 32-byte words.
func IndexCalldata(firstBlock, tableSize uint64, tableRoot common.Hash) []byte {
	data := make([]byte, eip8304CalldataLength)
	binary.BigEndian.PutUint64(data[24:32], firstBlock)
	binary.BigEndian.PutUint64(data[56:64], tableSize)
	copy(data[64:96], tableRoot[:])
	return data
}

// ApplyIndexEip8304 invokes the index contract update for the due table
// (firstBlock, tableSize) with tableRoot, propagating system call errors.
func ApplyIndexEip8304(
	indexAddr accounts.Address,
	firstBlock, tableSize uint64,
	tableRoot common.Hash,
	syscall rules.SystemCall,
) error {
	_, err := syscall(indexAddr, IndexCalldata(firstBlock, tableSize, tableRoot))
	return err
}
