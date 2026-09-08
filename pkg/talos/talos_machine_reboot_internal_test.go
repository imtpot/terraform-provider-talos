// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos //nolint:testpackage // exercises reboot before Kubernetes bootstrap

import (
	"context"
	"io"
	"testing"
	"time"

	commonapi "github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type rebootTestStream struct {
	grpc.ClientStream
	bootID string
	done   bool
}

func (s *rebootTestStream) Recv() (*commonapi.Data, error) {
	if s.done {
		return nil, io.EOF
	}

	s.done = true

	return &commonapi.Data{Bytes: []byte(s.bootID)}, nil
}

func (*rebootTestStream) CloseSend() error { return nil }

type rebootTestMachine struct {
	machineapi.MachineServiceClient
	read   func() (string, error)
	reboot func(*machineapi.RebootRequest) error
}

func (m rebootTestMachine) Read(_ context.Context, req *machineapi.ReadRequest, _ ...grpc.CallOption) (machineapi.MachineService_ReadClient, error) {
	if req.GetPath() != "/proc/sys/kernel/random/boot_id" {
		return nil, status.Error(codes.InvalidArgument, "unexpected read path")
	}

	id, err := m.read()
	if err != nil {
		return nil, err
	}

	return &rebootTestStream{bootID: id}, nil
}

func (m rebootTestMachine) Reboot(_ context.Context, req *machineapi.RebootRequest, _ ...grpc.CallOption) (*machineapi.RebootResponse, error) {
	return &machineapi.RebootResponse{Messages: []*machineapi.Reboot{{}}}, m.reboot(req)
}

func TestTalosMachineRebootBeforeBootstrap(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	reads, reboots := 0, 0
	machine := rebootTestMachine{
		read: func() (string, error) {
			reads++
			switch reads {
			case 1, 2:
				return "old-boot", nil
			case 3:
				return "", status.Error(codes.Unavailable, "rebooting")
			default:
				return "new-boot", nil
			}
		},
		reboot: func(req *machineapi.RebootRequest) error {
			reboots++

			require.Equal(t, 1, reads, "record boot ID before requesting reboot")
			require.Equal(t, machineapi.RebootRequest_POWERCYCLE, req.GetMode())

			return nil
		},
	}
	booting := bootTestClient(t, runtimeres.MachineStageBooting, false, false)
	ready := bootTestClient(t, runtimeres.MachineStageBooting, true, true)
	booting.MachineClient, ready.MachineClient = machine, machine
	op := func(ctx context.Context, _, _ string, _ *clientconfig.Config, fn func(context.Context, *client.Client) error) error {
		c := booting
		if reads < 3 || reads >= 5 {
			c = ready
		}

		return fn(ctx, c)
	}

	// No Events RPC is implemented: a missed shutdown event must not block
	// completion, and overall machine Ready is false until etcd is bootstrapped.
	require.NoError(t, talosMachineRebootBeforeBootstrap(ctx, "endpoint", "node", nil, machineapi.RebootRequest_POWERCYCLE, op))
	require.Equal(t, 5, reads, "wait for a different boot ID and healthy CRI")
	require.Equal(t, 1, reboots, "never repeat the reboot RPC while polling")
}

func TestTalosMachineRebootBeforeBootstrapRejectsErrors(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		rebootError error
		name        string
		bootID      string
		wantReboots int
	}{
		{name: "missing initial boot ID"},
		{name: "reboot rejected", bootID: "old-boot", rebootError: status.Error(codes.PermissionDenied, "denied"), wantReboots: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reads, reboots := 0, 0
			c := &client.Client{MachineClient: rebootTestMachine{
				read: func() (string, error) {
					reads++

					return tt.bootID, nil
				},
				reboot: func(*machineapi.RebootRequest) error {
					reboots++

					return tt.rebootError
				},
			}}
			op := func(ctx context.Context, _, _ string, _ *clientconfig.Config, fn func(context.Context, *client.Client) error) error {
				return fn(ctx, c)
			}

			require.Error(t, talosMachineRebootBeforeBootstrap(t.Context(), "endpoint", "node", nil, machineapi.RebootRequest_DEFAULT, op))
			require.Equal(t, 1, reads)
			require.Equal(t, tt.wantReboots, reboots)
		})
	}
}
