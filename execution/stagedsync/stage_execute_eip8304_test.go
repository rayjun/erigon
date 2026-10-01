package stagedsync

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/temporal/temporaltest"
	"github.com/erigontech/erigon/execution/chain"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
	"github.com/erigontech/erigon/execution/protocol/eip8304"
)

// TestInvalidateEip8304TablesIsGatedByForkConfig pins the two properties the
// unwind hook must have: a network that does not schedule EIP-8304 never
// touches the table bucket, and a network that does schedule it invalidates
// the tables the unwind removed.
func TestInvalidateEip8304TablesIsGatedByForkConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	logger := log.New()
	db := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	tx, err := db.BeginTemporalRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	recovery, err := eip8304.NewTableRecovery(tx)
	require.NoError(t, err)

	// Every shipped network leaves Eip8304Time nil, so no unwind may write to
	// the bucket — not even the generation key.
	require.NoError(t, invalidateEip8304Tables(ExecuteBlockCfg{chainConfig: chainspec.Mainnet.Config}, tx, 5, logger))
	generation, err := recovery.Generation()
	require.NoError(t, err)
	require.Zero(t, generation, "a network without the fork must leave the store untouched")

	// A config that does schedule the fork invalidates from the unwind point.
	zero := uint64(0)
	forked := &chain.Config{Eip8304Time: &zero}
	require.NoError(t, invalidateEip8304Tables(ExecuteBlockCfg{chainConfig: forked}, tx, 5, logger))
	generation, err = recovery.Generation()
	require.NoError(t, err)
	require.Equal(t, uint64(1), generation)

	// A node without a chain config is as free as one without the fork.
	require.NoError(t, invalidateEip8304Tables(ExecuteBlockCfg{}, tx, 5, logger))
	generation, err = recovery.Generation()
	require.NoError(t, err)
	require.Equal(t, uint64(1), generation)
}

// TestInvalidateEip8304TablesDropsExactlyTheUnwoundRange pins the translation
// from an unwind point to the first invalid block: unwinding to block 3 removes
// block 4 onward, so a table covering only 0..3 stays and a table covering
// block 4 goes. Both off-by-ones (from = unwindPoint, from = unwindPoint+2)
// fail here.
func TestInvalidateEip8304TablesDropsExactlyTheUnwoundRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	logger := log.New()
	db := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	tx, err := db.BeginTemporalRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	zero := uint64(0)
	cfg := ExecuteBlockCfg{chainConfig: &chain.Config{Eip8304Time: &zero}}

	keep := seedStoredTable(t, tx, 0, 4) // covers 0..3, all surviving
	drop := seedStoredTable(t, tx, 4, 1) // covers block 4, removed

	require.NoError(t, invalidateEip8304Tables(cfg, tx, 3, logger))

	require.True(t, storedTableExists(t, tx, keep), "a table covering only surviving blocks must stay")
	require.False(t, storedTableExists(t, tx, drop), "a table covering the unwind point must go")

	recovery, err := eip8304.NewTableRecovery(tx)
	require.NoError(t, err)
	generation, err := recovery.Generation()
	require.NoError(t, err)
	require.Equal(t, uint64(1), generation, "a real unwind must bump the generation")
}

// seedStoredTable writes the key of a stored table at (first, size). The
// recovery path deletes by covered range and never decodes a value, so a test
// that only cares about the range criterion can seed the key alone.
func seedStoredTable(t *testing.T, tx kv.RwTx, first, size uint64) []byte {
	t.Helper()
	key := make([]byte, 17)
	key[0] = 1 // record version
	binary.BigEndian.PutUint64(key[1:9], first)
	binary.BigEndian.PutUint64(key[9:17], size)
	require.NoError(t, tx.Put(kv.Eip8304Tables, key, []byte("record")))
	return key
}

func storedTableExists(t *testing.T, tx kv.RwTx, key []byte) bool {
	t.Helper()
	value, err := tx.GetOne(kv.Eip8304Tables, key)
	require.NoError(t, err)
	return value != nil
}
