// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos //nolint:testpackage // exercises the boot wait used after applying machine configuration

import (
	"context"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type bootVersionClient struct {
	machineapi.MachineServiceClient
}

func (bootVersionClient) Version(context.Context, *emptypb.Empty, ...grpc.CallOption) (*machineapi.VersionResponse, error) {
	return &machineapi.VersionResponse{}, nil
}

func bootTestClient(t *testing.T, stage runtimeres.MachineStage, running, healthy bool) *client.Client {
	t.Helper()

	st := state.WrapCore(inmem.NewState(runtimeres.NamespaceName))
	machine := runtimeres.NewMachineStatus()
	machine.TypedSpec().Stage = stage
	// Kubernetes is not bootstrapped yet: overall Ready deliberately remains false.
	require.NoError(t, st.Create(t.Context(), machine))

	cri := v1alpha1.NewService("cri")
	cri.TypedSpec().Running = running
	cri.TypedSpec().Healthy = healthy
	require.NoError(t, st.Create(t.Context(), cri))

	return &client.Client{COSI: st, MachineClient: bootVersionClient{}}
}

func TestTalosMachineBootReadiness(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		stage   runtimeres.MachineStage
		running bool
		healthy bool
		ready   bool
	}{
		{"maintenance API responds before installation", runtimeres.MachineStageMaintenance, false, false, false},
		{"installation in progress", runtimeres.MachineStageInstalling, false, false, false},
		{"reboot pending with services still alive", runtimeres.MachineStageRebooting, true, true, false},
		{"boot API responds before sequence completes", runtimeres.MachineStageBooting, true, true, false},
		{"CRI not started", runtimeres.MachineStageRunning, false, false, false},
		{"CRI not healthy", runtimeres.MachineStageRunning, true, false, false},
		{"ready to install before etcd bootstrap", runtimeres.MachineStageRunning, true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := bootTestClient(t, tt.stage, tt.running, tt.healthy)

			err := talosMachineCheckBootReady(t.Context(), c)
			if tt.ready {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a responding Version API does not mean the node can pull an installer")
			}
		})
	}
}

func TestTalosMachineBootWaitSurvivesReboot(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	booting := bootTestClient(t, runtimeres.MachineStageBooting, false, false)
	ready := bootTestClient(t, runtimeres.MachineStageRunning, true, true)
	calls := 0
	op := func(ctx context.Context, _, _ string, _ *clientconfig.Config, fn func(context.Context, *client.Client) error) error {
		calls++
		switch calls {
		case 1:
			return status.Error(codes.Unavailable, "connection refused during reboot")
		case 2:
			return fn(ctx, booting)
		default:
			return fn(ctx, ready)
		}
	}

	require.NoError(t, talosMachineWaitForBoot(ctx, "endpoint", "node", nil, op))
	require.Equal(t, 3, calls, "must wait past the first API response after reboot")
}

func TestTalosMachineBootWaitRejectsInvalidCredentials(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	want := status.Error(codes.PermissionDenied, "not authorized")
	calls := 0
	op := func(context.Context, string, string, *clientconfig.Config, func(context.Context, *client.Client) error) error {
		calls++

		return want
	}

	require.ErrorIs(t, talosMachineWaitForBoot(ctx, "endpoint", "node", nil, op), want)
	require.Equal(t, 1, calls)
}

func TestTalosMachineBootWaitHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	op := func(ctx context.Context, _, _ string, _ *clientconfig.Config, _ func(context.Context, *client.Client) error) error {
		cancel()

		return ctx.Err()
	}

	require.Error(t, talosMachineWaitForBoot(ctx, "endpoint", "node", nil, op))
}
