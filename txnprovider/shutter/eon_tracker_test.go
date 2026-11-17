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

package shutter_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"golang.org/x/sync/errgroup"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/execution/abi"
	"github.com/erigontech/erigon/execution/abi/bind"
	"github.com/erigontech/erigon/execution/chain/networkname"
	"github.com/erigontech/erigon/execution/vm"
	"github.com/erigontech/erigon/node/gointerfaces/remoteproto"
	"github.com/erigontech/erigon/txnprovider/shutter"
	shuttercontracts "github.com/erigontech/erigon/txnprovider/shutter/internal/contracts"
	"github.com/erigontech/erigon/txnprovider/shutter/internal/testhelpers"
	"github.com/erigontech/erigon/txnprovider/shutter/shuttercfg"
)

func TestEonTrackerRecoversAfterUnwind(t *testing.T) {
	for _, tc := range []struct {
		name             string
		beforeDeployment bool
		noSets           bool
		readErr          error
	}{
		{name: "before deployment", beforeDeployment: true},
		{name: "before activation"},
		{name: "before first set", noSets: true},
		{name: "RPC failure", readErr: errors.New("RPC unavailable")},
		{name: "unrelated revert", readErr: vm.ErrExecutionReverted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				logger := log.New()
				backend := NewMockContractBackend(gomock.NewController(t), logger)
				if !tc.beforeDeployment {
					contractABI, err := abi.JSON(strings.NewReader(shuttercontracts.KeyperSetManagerABI))
					require.NoError(t, err)
					backend.EXPECT().CallContract(gomock.Any(), gomock.Any(), uint256.NewInt(9)).DoAndReturn(
						func(_ context.Context, msg bind.CallMsg, _ *uint256.Int) ([]byte, error) {
							if tc.readErr != nil {
								return nil, tc.readErr
							}
							method, err := contractABI.MethodById(msg.Data)
							if err != nil {
								return nil, err
							}
							switch method.Name {
							case "getNumKeyperSets":
								count := uint64(1)
								if tc.noSets {
									count = 0
								}
								return method.Outputs.PackValues([]any{count})
							case "getKeyperSetActivationBlock":
								return method.Outputs.PackValues([]any{uint64(10)})
							default:
								require.Equal(t, "getKeyperSetIndexByBlock", method.Name)
								return nil, vm.ErrExecutionReverted
							}
						}).AnyTimes()
				}
				config := shuttercfg.ConfigByChainName(networkname.Chiado)
				config.ReorgDepthAwareness = 5
				broadcast := common.HexToAddress(config.KeyBroadcastContractAddress)
				broadcastABI, err := abi.JSON(strings.NewReader(shuttercontracts.KeyBroadcastContractABI))
				require.NoError(t, err)
				callData, err := broadcastABI.Pack("getEonKey", uint64(2))
				require.NoError(t, err)
				emptyKey, err := broadcastABI.Methods["getEonKey"].Outputs.PackValues([]any{[]byte{}})
				require.NoError(t, err)
				backend.EXPECT().CallContract(gomock.Any(), bind.CallMsg{To: &broadcast, Data: callData}, uint256.NewInt(18)).Return(emptyKey, nil).AnyTimes()
				backend.PrepareMocks()
				batches := make(chan *remoteproto.StateChangeBatch)
				listener := shutter.NewBlockListener(logger, testhelpers.NewStateChangesClientMock(ctx, batches))
				tracker := shutter.NewKsmEonTracker(logger, config, listener, backend)
				eg, runCtx := errgroup.WithContext(ctx)
				eg.Go(func() error { return listener.Run(runCtx) })
				eg.Go(func() error { return tracker.Run(runCtx) })
				defer func() {
					cancel()
					wantErr := tc.readErr
					if wantErr == nil {
						wantErr = context.Canceled
					}
					require.ErrorIs(t, eg.Wait(), wantErr)
				}()
				synctest.Wait()

				handle := PoolTestHandle{config: config, contractBackend: backend}
				initial, err := testhelpers.MockEonKeyGeneration(0, 1, 2, 10)
				require.NoError(t, err)
				handle.SimulateInitialEonRead(t, initial)
				batches <- &remoteproto.StateChangeBatch{ChangeBatch: []*remoteproto.StateChange{{BlockHeight: 100}}}
				synctest.Wait()
				eon, ok := tracker.CurrentEon()
				require.True(t, ok)
				require.Equal(t, initial.Eon(), eon)

				manager := common.HexToAddress(config.KeyperSetManagerContractAddress)
				unwindBlock := uint64(9)
				if tc.beforeDeployment {
					unwindBlock = 0
					handle.SimulateCallResult(manager, nil)
					backend.EXPECT().CodeAt(gomock.Any(), manager, uint256.NewInt(0)).Return(nil, nil)
				}
				batches <- &remoteproto.StateChangeBatch{
					StateChangesIncomplete: true,
					ChangeBatch:            []*remoteproto.StateChange{{BlockHeight: unwindBlock, Direction: remoteproto.Direction_UNWIND}},
				}
				synctest.Wait()
				if tc.readErr != nil {
					return
				}
				_, ok = tracker.CurrentEon()
				require.False(t, ok)
				_, ok = tracker.RecentEon(initial.EonIndex)
				require.False(t, ok)

				redeployed, err := testhelpers.MockEonKeyGeneration(0, 1, 2, 10)
				require.NoError(t, err)
				handle.SimulateInitialEonRead(t, redeployed)
				batches <- &remoteproto.StateChangeBatch{ChangeBatch: []*remoteproto.StateChange{{BlockHeight: 10}}}
				synctest.Wait()
				eon, ok = tracker.CurrentEon()
				require.True(t, ok)
				require.Equal(t, redeployed.Eon(), eon)

				next, err := testhelpers.MockEonKeyGeneration(1, 1, 2, 11)
				require.NoError(t, err)
				handle.SimulateNewEonRead(t, next)
				batches <- &remoteproto.StateChangeBatch{ChangeBatch: []*remoteproto.StateChange{{BlockHeight: 11}}}
				synctest.Wait()
				for _, value := range []uint64{2, 10} {
					word := uint256.NewInt(value).Bytes32()
					handle.SimulateCallResult(manager, word[:])
				}
				handle.SimulateCachedEonRead(t, redeployed)
				batches <- &remoteproto.StateChangeBatch{ChangeBatch: []*remoteproto.StateChange{{BlockHeight: 10, Direction: remoteproto.Direction_UNWIND}}}
				synctest.Wait()
				eon, ok = tracker.CurrentEon()
				require.True(t, ok)
				require.Equal(t, redeployed.Eon(), eon)
				handle.SimulateCachedEonRead(t, next)
				batches <- &remoteproto.StateChangeBatch{ChangeBatch: []*remoteproto.StateChange{{BlockHeight: 11}}}
				synctest.Wait()
				handle.SimulateCachedEonRead(t, next)
				batches <- &remoteproto.StateChangeBatch{ChangeBatch: []*remoteproto.StateChange{{BlockHeight: 17}}}
				synctest.Wait()
				_, ok = tracker.RecentEon(redeployed.EonIndex)
				require.False(t, ok, "cleanup must resume without reaching the height of the old deployment")
				_, ok = tracker.RecentEon(next.EonIndex)
				require.True(t, ok)

				unkeyed, err := testhelpers.MockEonKeyGeneration(2, 1, 2, 18)
				require.NoError(t, err)
				handle.SimulateNewEonRead(t, unkeyed)
				batches <- &remoteproto.StateChangeBatch{ChangeBatch: []*remoteproto.StateChange{{BlockHeight: 18}}}
				synctest.Wait()
				eon, ok = tracker.CurrentEon()
				require.True(t, ok)
				require.Equal(t, next.Eon(), eon)
				_, ok = tracker.RecentEon(next.EonIndex)
				require.True(t, ok)
			})
		})
	}
}
