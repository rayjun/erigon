package eip8304

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
)

func TestEnsureConfigurationPinsTableParameters(t *testing.T) {
	const limit = uint64(1 << 12)
	addr := common.HexToAddress("0x0000000000000000000000000000000000008304")
	db := newHotStoreDB(t)
	ctx := context.Background()

	require.NoError(t, db.Update(ctx, func(tx kv.RwTx) error {
		return EnsureConfiguration(tx, addr, limit)
	}))
	require.NoError(t, db.Update(ctx, func(tx kv.RwTx) error {
		return EnsureConfiguration(tx, addr, limit)
	}), "the same pair must remain valid after a restart")

	require.ErrorIs(t, db.Update(ctx, func(tx kv.RwTx) error {
		return EnsureConfiguration(tx, addr, limit+1)
	}), ErrTableConfigurationMismatch)
	require.ErrorIs(t, db.Update(ctx, func(tx kv.RwTx) error {
		return EnsureConfiguration(tx, common.Address{1}, limit)
	}), ErrTableConfigurationMismatch)
	require.Error(t, EnsureConfiguration(nil, addr, 0))
}
