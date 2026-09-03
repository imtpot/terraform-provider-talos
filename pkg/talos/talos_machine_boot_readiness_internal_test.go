// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos_test

import (
	"context"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
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

	"github.com/siderolabs/terraform-provider-talos/pkg/talos"
)

type bootVersionClient struct {
	machineapi.MachineServiceClient
	calls *int
	err   error
}

func (c bootVersionClient) Version(context.Context, *emptypb.Empty, ...grpc.CallOption) (*machineapi.VersionResponse, error) {
	if c.calls != nil {
		*c.calls++
	}

	if c.err != nil {
		return nil, c.err
	}

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
		{"boot API responds before CRI starts", runtimeres.MachineStageBooting, false, false, false},
		{"boot waits for etcd but CRI is ready", runtimeres.MachineStageBooting, true, true, true},
		{"CRI not started", runtimeres.MachineStageRunning, false, false, false},
		{"CRI not healthy", runtimeres.MachineStageRunning, true, false, false},
		{"ready to install before etcd bootstrap", runtimeres.MachineStageRunning, true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := bootTestClient(t, tt.stage, tt.running, tt.healthy)

			err := talos.TalosMachineCheckBootReady(t.Context(), c)
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

	require.NoError(t, talos.TalosMachineWaitForBoot(ctx, "endpoint", "node", nil, op))
	require.Equal(t, 3, calls, "must wait past the first API response after reboot")
}

func TestTalosMachineBootReadinessWaitsForUnattendedInstall(t *testing.T) {
	t.Parallel()

	for _, phase := range []runtimeres.UnattendedInstallPhase{
		runtimeres.UnattendedInstallPhasePending,
		runtimeres.UnattendedInstallPhaseInstalling,
		runtimeres.UnattendedInstallPhaseWaitingForReboot,
		runtimeres.UnattendedInstallPhaseInstalled,
	} {
		t.Run(phase.String(), func(t *testing.T) {
			t.Parallel()

			c := bootTestClient(t, runtimeres.MachineStageBooting, true, true)
			install := runtimeres.NewUnattendedInstallStatus()
			install.TypedSpec().Phase = phase
			require.NoError(t, c.COSI.Create(t.Context(), install))

			err := talos.TalosMachineCheckBootReady(t.Context(), c)
			if phase == runtimeres.UnattendedInstallPhaseInstalled {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "must not race an unfinished first install or its scheduled reboot")
			}
		})
	}
}

type bootTestUnsupportedState struct {
	state.State
	err error
}

func (s bootTestUnsupportedState) Get(ctx context.Context, ptr resource.Pointer, opts ...state.GetOption) (resource.Resource, error) {
	if ptr.Type() == runtimeres.UnattendedInstallStatusType {
		return nil, s.err
	}

	return s.State.Get(ctx, ptr, opts...)
}

type bootTestDeniedState struct {
	state.State
	err error
}

func (s bootTestDeniedState) Get(context.Context, resource.Pointer, ...state.GetOption) (resource.Resource, error) {
	return nil, s.err
}

func TestTalosMachineBootReadinessOlderTalos(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		err   error
		name  string
		ready bool
	}{
		{
			name:  "old server has no unattended install resource type",
			err:   status.Errorf(codes.PermissionDenied, "resource type %q is not supported", runtimeres.UnattendedInstallStatusType),
			ready: true,
		},
		{
			name: "actual permission denial must not be ignored",
			err:  status.Error(codes.PermissionDenied, "not authorized to read this resource"),
		},
		{
			name: "other server errors must not be ignored",
			err:  status.Errorf(codes.Unknown, "resource type %q is not supported", runtimeres.UnattendedInstallStatusType),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := bootTestClient(t, runtimeres.MachineStageBooting, true, true)
			c.COSI = bootTestUnsupportedState{State: c.COSI, err: tt.err}

			err := talos.TalosMachineCheckBootReady(t.Context(), c)
			if tt.ready {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.err)
			}
		})
	}
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

	require.ErrorIs(t, talos.TalosMachineWaitForBoot(ctx, "endpoint", "node", nil, op), want)
	require.Equal(t, 1, calls)
}

func TestTalosMachineBootWaitFallsBackForRestrictedRole(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	c := bootTestClient(t, runtimeres.MachineStageBooting, true, true)
	c.COSI = bootTestDeniedState{
		State: c.COSI,
		err:   status.Error(codes.PermissionDenied, "role cannot read runtime resources"),
	}
	calls := 0
	op := func(ctx context.Context, _ string, _ string, _ *clientconfig.Config, fn func(context.Context, *client.Client) error) error {
		calls++

		return fn(ctx, c)
	}

	require.NoError(t, talos.TalosMachineWaitForBoot(ctx, "endpoint", "node", nil, op))
	require.Equal(t, 1, calls, "should succeed without retrying")
}

func TestTalosMachineBootWaitDoesNotHideInvalidCredentials(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	versionCalls := 0
	versionErr := status.Error(codes.Unauthenticated, "client credentials are invalid")
	c := bootTestClient(t, runtimeres.MachineStageBooting, true, true)
	c.COSI = bootTestDeniedState{
		State: c.COSI,
		err:   status.Error(codes.PermissionDenied, "role cannot read runtime resources"),
	}
	c.MachineClient = bootVersionClient{calls: &versionCalls, err: versionErr}
	op := func(ctx context.Context, _ string, _ string, _ *clientconfig.Config, fn func(context.Context, *client.Client) error) error {
		return fn(ctx, c)
	}

	require.ErrorIs(t, talos.TalosMachineWaitForBoot(ctx, "endpoint", "node", nil, op), versionErr)
	require.Equal(t, 1, versionCalls, "fallback must verify API access")
}

func TestTalosMachineBootWaitHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	op := func(ctx context.Context, _, _ string, _ *clientconfig.Config, _ func(context.Context, *client.Client) error) error {
		cancel()

		return ctx.Err()
	}

	require.Error(t, talos.TalosMachineWaitForBoot(ctx, "endpoint", "node", nil, op))
}
