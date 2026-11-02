package eip8304

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types/accounts"
)

func alwaysActive(_, _, _ uint64) (bool, error) { return true, nil }

func TestFinalizerAppliesAndRecordsDueTables(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 5)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	executed := uint64(0)
	store := newTestHotStoreWithProgress(t, tx, chain, func() (uint64, error) { return executed, nil })
	indexAddr := accounts.InternAddress(common.HexToAddress("0x8304"))

	finalizer, err := NewFinalizer(indexAddr, hotStoreTestLimit, alwaysActive, store, true)
	require.NoError(t, err)

	calls := 0
	syscall := func(addr accounts.Address, data []byte) ([]byte, error) {
		require.Equal(t, indexAddr, addr)
		calls++
		return nil, nil
	}
	for block := uint64(0); block <= 4; block++ {
		chain.available = block
		executed = block
		require.NoError(t, finalizer.Finalize(block, block, testHash(block), chain.parentHash(block), chain.receipts[block], syscall))
	}

	require.Equal(t, 6, calls, "five L0 tables plus the delayed (0,4) table")
	chain.available = 5
	for _, ref := range []TableRef{L0(4), {FirstBlock: 0, TableSize: 4}} {
		record, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		require.True(t, ok, "%+v must be recorded", ref)
		require.Equal(t, oracleRoot(t, chain, ref, hotStoreTestLimit), record.Root)
	}
}

func TestFinalizerWithoutRecordingKeepsTheStoreUntouched(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 5)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStoreWithProgress(t, tx, chain, func() (uint64, error) { return 4, nil })
	indexAddr := accounts.InternAddress(common.HexToAddress("0x8304"))

	finalizer, err := NewFinalizer(indexAddr, hotStoreTestLimit, alwaysActive, store, false)
	require.NoError(t, err)
	calls := 0
	syscall := func(accounts.Address, []byte) ([]byte, error) {
		calls++
		return nil, nil
	}
	require.NoError(t, finalizer.Finalize(4, 4, common.Hash{}, chain.parentHash(4), chain.receipts[4], syscall))
	require.Equal(t, 2, calls)

	for _, ref := range []TableRef{L0(4), {FirstBlock: 0, TableSize: 4}} {
		_, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		require.False(t, ok, "%+v must not be persisted by a building-path finalizer", ref)
	}
}

func TestCapturingFinalizerDefersRecordsToTheTransactionOwner(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 5)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 4)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x8304"))

	finalizer, err := NewCapturingFinalizer(indexAddr, hotStoreTestLimit, alwaysActive, store, nil)
	require.NoError(t, err)
	calls := 0
	require.NoError(t, finalizer.Finalize(4, 4, testHash(4), chain.parentHash(4), chain.receipts[4], func(accounts.Address, []byte) ([]byte, error) {
		calls++
		return nil, nil
	}))
	require.Equal(t, 2, calls)

	for _, ref := range []TableRef{L0(4), {FirstBlock: 0, TableSize: 4}} {
		_, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		require.False(t, ok, "%+v must remain buffered, not written by the exec-loop goroutine", ref)
	}

	tables := finalizer.TakeTableResults()
	require.Len(t, tables, 2)
	require.Empty(t, finalizer.TakeTableResults(), "taking the buffer clears it")
	for _, table := range tables {
		require.NoError(t, store.PutTable(table))
	}
	for _, ref := range []TableRef{L0(4), {FirstBlock: 0, TableSize: 4}} {
		record, ok, err := store.GetTable(ref)
		require.NoError(t, err)
		require.True(t, ok, "%+v must be recorded by the transaction owner", ref)
		require.Equal(t, oracleRoot(t, chain, ref, hotStoreTestLimit), record.Root)
	}
}

func TestTableOverlayDropsRecordsOutsideTheLookbackWindow(t *testing.T) {
	overlay := NewTableOverlay()
	old := TableResult{
		Ref:         L0(0),
		BlockHashes: []common.Hash{testHash(0)},
	}
	newest := TableResult{
		Ref:         L0(MaxTableLookback + 1),
		BlockHashes: []common.Hash{testHash(MaxTableLookback + 1)},
	}
	overlay.Remember(old)
	overlay.Remember(newest)

	_, ok := overlay.table(old.Ref)
	require.False(t, ok, "an old L0 record is no longer reachable from a future due table")
	_, ok = overlay.canonicalHash(old.Ref.FirstBlock)
	require.False(t, ok)
	_, ok = overlay.table(newest.Ref)
	require.True(t, ok)
}

func TestFinalizerRejectsMissingHashAndActivationFailure(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 1)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 0)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x8304"))
	syscall := func(accounts.Address, []byte) ([]byte, error) { return nil, nil }

	recording, err := NewFinalizer(indexAddr, hotStoreTestLimit, alwaysActive, store, true)
	require.NoError(t, err)
	err = recording.Finalize(1, 1, common.Hash{}, chain.parentHash(1), chain.receipts[1], syscall)
	require.ErrorIs(t, err, ErrMissingBlockHash)

	activationErr := errors.New("activation unavailable")
	activation := func(uint64, uint64, uint64) (bool, error) { return false, activationErr }
	notRecording, err := NewFinalizer(indexAddr, hotStoreTestLimit, activation, store, false)
	require.NoError(t, err)
	err = notRecording.Finalize(1, 1, common.Hash{}, chain.parentHash(1), chain.receipts[1], syscall)
	require.ErrorIs(t, err, activationErr)

	_, err = NewFinalizer(indexAddr, hotStoreTestLimit, nil, store, true)
	require.ErrorIs(t, err, ErrNilFinalizerActivation)
	_, err = NewFinalizer(indexAddr, hotStoreTestLimit, alwaysActive, nil, true)
	require.ErrorIs(t, err, ErrNilTableStore)
}

func TestFinalizerUsesTheInFlightBlockTimeForActivation(t *testing.T) {
	db := newHotStoreDB(t)
	chain := newTestChain(t, 2)
	tx := beginHotStoreTx(t, db)
	defer tx.Rollback()
	store := newTestHotStore(t, tx, chain, 1)
	indexAddr := accounts.InternAddress(common.HexToAddress("0x8304"))

	var gotFirst, gotCurrent, gotTime uint64
	activation := func(firstBlock, currentBlock, currentTime uint64) (bool, error) {
		gotFirst, gotCurrent, gotTime = firstBlock, currentBlock, currentTime
		return true, nil
	}
	finalizer, err := NewFinalizer(indexAddr, hotStoreTestLimit, activation, store, false)
	require.NoError(t, err)
	require.NoError(t, finalizer.Finalize(1, 1_234, common.Hash{}, chain.parentHash(1), chain.receipts[1], func(accounts.Address, []byte) ([]byte, error) { return nil, nil }))
	require.Equal(t, uint64(1), gotFirst)
	require.Equal(t, uint64(1), gotCurrent)
	require.Equal(t, uint64(1_234), gotTime, "the header need not be persisted before its timestamp gates activation")
}
