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
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types/accounts"
)

// TestIndexCalldataEncoding checks the full 32-byte word of each calldata
// segment, including the high bytes, so a value written to the word start
// would not slip through.
func TestIndexCalldataEncoding(t *testing.T) {
	root := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	word := func(v uint64) []byte {
		b := make([]byte, 32)
		binary.BigEndian.PutUint64(b[24:], v)
		return b
	}

	for _, tc := range []struct{ firstBlock, tableSize uint64 }{
		{0, 0},
		{42, 16},
		{math.MaxUint64, math.MaxUint64},
	} {
		calldata := IndexCalldata(tc.firstBlock, tc.tableSize, root)
		require.Len(t, calldata, 96)
		require.Equal(t, word(tc.firstBlock), calldata[0:32], "first_block word")
		require.Equal(t, word(tc.tableSize), calldata[32:64], "table_size word")
		require.Equal(t, root[:], calldata[64:96], "table_root word")
	}
}

func TestApplyIndexEip8304PropagatesFailure(t *testing.T) {
	indexAddr := accounts.Address{} // parameterized; actual address is TBD
	root := common.HexToHash("0x11")

	err := ApplyIndexEip8304(indexAddr, 0, 1, root, func(addr accounts.Address, data []byte) ([]byte, error) {
		require.Equal(t, indexAddr, addr)
		require.Equal(t, IndexCalldata(0, 1, root), data)
		return nil, nil
	})
	require.NoError(t, err)

	boom := errors.New("revert")
	err = ApplyIndexEip8304(indexAddr, 0, 1, root, func(addr accounts.Address, data []byte) ([]byte, error) {
		return nil, boom
	})
	require.ErrorIs(t, err, boom)
}
