package eip8304

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashesOnlyRejectsEntryReads(t *testing.T) {
	_, err := (hashesOnly{}).BlockEntries(1)
	require.ErrorIs(t, err, ErrMissingBlockData)
}
