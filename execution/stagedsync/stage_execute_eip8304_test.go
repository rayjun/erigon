package stagedsync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
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
