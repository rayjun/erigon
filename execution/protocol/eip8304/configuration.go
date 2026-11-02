package eip8304

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
)

// ErrTableConfigurationMismatch reports a datadir opened with a different
// EIP-8304 parameter pair than the one used to create its table records.
var ErrTableConfigurationMismatch = errors.New("eip8304: table configuration differs from the datadir")

var tableConfigurationKey = []byte("eip8304.table-configuration.v1")

// EnsureConfiguration pins the index contract and SSZ list bound on first use
// and refuses to replace them later. Both values determine published table
// roots, so changing either one invalidates slots already written to the index
// contract and requires a fresh datadir.
func EnsureConfiguration(tx kv.GetPut, indexContract common.Address, listLimit uint64) error {
	if listLimit == 0 {
		return errors.New("eip8304: table list limit must be non-zero")
	}

	want := encodeTableConfiguration(indexContract, listLimit)
	raw, err := tx.GetOne(kv.DatabaseInfo, tableConfigurationKey)
	if err != nil {
		return fmt.Errorf("read EIP-8304 table configuration: %w", err)
	}
	if raw == nil {
		if err := tx.Put(kv.DatabaseInfo, tableConfigurationKey, want); err != nil {
			return fmt.Errorf("pin EIP-8304 table configuration: %w", err)
		}
		return nil
	}
	if !bytes.Equal(raw, want) {
		return fmt.Errorf("%w: requested contract %s and list limit %d; changing either value requires a fresh datadir",
			ErrTableConfigurationMismatch, indexContract, listLimit)
	}
	return nil
}

func encodeTableConfiguration(indexContract common.Address, listLimit uint64) []byte {
	raw := make([]byte, 1+len(indexContract)+8)
	raw[0] = 1
	copy(raw[1:1+len(indexContract)], indexContract[:])
	binary.BigEndian.PutUint64(raw[1+len(indexContract):], listLimit)
	return raw
}
