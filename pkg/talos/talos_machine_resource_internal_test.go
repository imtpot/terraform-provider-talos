// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos //nolint:testpackage // needs access to unexported talosMachineUpgradeLegacy and clientOpFunc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// TestLegacyUpgrade_ImmediateRPCError_AbortsPollEarly calls the real
// talosMachineUpgradeLegacy with a mock op that returns an error immediately.
// It verifies the function returns well within the 5-second context deadline
// with "upgrade RPC failed:" in the message, proving the goroutine error is
// propagated through rpcErrCh to abort the poll rather than being discarded.
func TestLegacyUpgrade_ImmediateRPCError_AbortsPollEarly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rpcErr := errors.New("authentication failed")

	// Mock that always fails immediately — covers both the goroutine RPC call
	// and the poll loop's version check.
	failImmediately := clientOpFunc(func(_ context.Context, _, _ string, _ *clientconfig.Config, _ func(context.Context, *client.Client) error) error {
		return rpcErr
	})

	state := &talosMachineResourceModel{
		Image:      types.StringValue("ghcr.io/siderolabs/installer:v1.13.0"),
		RebootMode: types.StringValue("DEFAULT"),
	}

	err := talosMachineUpgradeLegacy(ctx, "10.0.0.1", "10.0.0.1", nil, state, "DEFAULT", failImmediately)
	if err == nil {
		t.Fatal("expected error from immediate RPC failure, got nil")
	}

	// The "upgrade RPC failed:" prefix is injected by the channel-based early-abort
	// path. Its presence proves the goroutine error reached the poll loop.
	if !strings.Contains(err.Error(), "upgrade RPC failed:") {
		t.Fatalf("expected 'upgrade RPC failed:' in error message, got: %v", err)
	}
}

func TestTalosMachineImageFactorySchematic(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		imageRef  string
		schematic string
		ok        bool
	}{
		"bare installer": {
			imageRef:  "factory.talos.dev/installer/abc123:v1.13.0",
			schematic: "abc123",
			ok:        true,
		},
		"bare secure boot installer": {
			imageRef:  "factory.talos.dev/installer-secureboot/abc123:v1.13.0",
			schematic: "abc123",
			ok:        true,
		},
		"default factory": {
			imageRef:  "factory.talos.dev/metal-installer/abc123:v1.13.0",
			schematic: "abc123",
			ok:        true,
		},
		"custom registry": {
			imageRef:  "images.example.com/talos/openstack-installer/def456:v1.13.0",
			schematic: "def456",
			ok:        true,
		},
		"registry with port and secure boot": {
			imageRef:  "images.example.com:5000/metal-installer-secureboot/789abc:v1.13.0",
			schematic: "789abc",
			ok:        true,
		},
		"standard installer": {
			imageRef: "ghcr.io/siderolabs/installer:v1.13.0",
		},
		"factory artifact": {
			imageRef: "factory.talos.dev/image/abc123/v1.13.0/metal-amd64.raw.xz",
		},
		"digest": {
			imageRef: "factory.talos.dev/metal-installer/abc123@sha256:1234",
		},
		"empty schematic": {
			imageRef: "factory.talos.dev/metal-installer/:v1.13.0",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			schematic, ok := talosMachineImageFactorySchematic(test.imageRef)
			if schematic != test.schematic || ok != test.ok {
				t.Fatalf("talosMachineImageFactorySchematic(%q) = (%q, %t), want (%q, %t)",
					test.imageRef, schematic, ok, test.schematic, test.ok)
			}
		})
	}
}
