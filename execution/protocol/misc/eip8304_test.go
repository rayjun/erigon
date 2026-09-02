package misc

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types/accounts"
)

func TestIndexCalldataEncoding(t *testing.T) {
	tableRoot := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	calldata := IndexCalldata(42, 16, tableRoot)

	require.Len(t, calldata, 96)

	// first_block word: big-endian 42 in the low 8 bytes.
	require.Equal(t, uint64(0), binaryBigEndianUint64(calldata[:24]))
	require.Equal(t, uint64(42), binaryBigEndianUint64(calldata[24:32]))
	// table_size word.
	require.Equal(t, uint64(0), binaryBigEndianUint64(calldata[32:56]))
	require.Equal(t, uint64(16), binaryBigEndianUint64(calldata[56:64]))
	// table_root word.
	require.Equal(t, tableRoot[:], calldata[64:96])
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

func binaryBigEndianUint64(b []byte) uint64 {
	_ = b[7] // ensure len >= 8
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}
