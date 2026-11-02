package blockgen

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/types"
)

type plainFinalizer struct{}

func (*plainFinalizer) Finalize(uint64, uint64, common.Hash, common.Hash, types.Receipts, rules.SystemCall) error {
	return nil
}

type preSealFinalizer struct{}

func (*preSealFinalizer) Finalize(uint64, uint64, common.Hash, common.Hash, types.Receipts, rules.SystemCall) error {
	return nil
}

func (*preSealFinalizer) FinalizePreSeal(uint64, uint64, common.Hash, types.Receipts, rules.SystemCall) error {
	return nil
}

func TestValidatePreSealFinalizers(t *testing.T) {
	_, err := validatePreSealFinalizers([]protocol.BlockFinalizer{&plainFinalizer{}})
	require.ErrorContains(t, err, "must implement PreSealFinalizer")

	_, err = validatePreSealFinalizers([]protocol.BlockFinalizer{&preSealFinalizer{}, &preSealFinalizer{}})
	require.ErrorContains(t, err, "at most one finalizer")

	got, err := validatePreSealFinalizers([]protocol.BlockFinalizer{&preSealFinalizer{}})
	require.NoError(t, err)
	require.NotNil(t, got)
}
