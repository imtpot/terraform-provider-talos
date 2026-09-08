// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/conditions"
)

// TestReporterCapturesLastAssertionError is the red-phase test for
// https://github.com/siderolabs/terraform-provider-talos/issues/400: when
// talos_cluster's health check times out, the user only ever sees "context
// deadline exceeded" with no indication of which check failed or why (e.g. that
// etcd gained members outside of the declared control_plane_nodes). The
// underlying conditions.PollingCondition discards its last assertion error on
// timeout, so the reporter is the only place that error can be recovered from.
//
// reporter.Update currently records condition.String() (just the check's
// description), not conditions.StatusLine(condition) (description plus last
// error), so the assertion error never reaches reporter.String() either.
func TestReporterCapturesLastAssertionError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New(`etcd member ips ["10.0.0.2"] are not subset of control plane node ips ["10.0.0.1"]`)

	condition := conditions.PollingCondition(
		"etcd members to be control plane nodes",
		func(context.Context) error { return wantErr },
		5*time.Millisecond,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	condition.Wait(ctx) //nolint:errcheck // exercising the timeout path; the error is ctx.Err(), not wantErr

	r := newReporter()
	r.Update(condition)

	if !strings.Contains(r.String(), wantErr.Error()) {
		t.Fatalf("reporter lost the assertion error; want it to contain %q, got:\n%s", wantErr.Error(), r.String())
	}
}

// TestReporterBoundsLineGrowthPerCheck guards against a regression in the fix
// above: recording conditions.StatusLine(condition) instead of a static
// description means the recorded line changes every time a check's last error
// changes, not just when the check itself changes. A health check can poll every
// 100ms for up to 30 minutes, so if reporter.Update naively appended on every
// distinct StatusLine, one flaky check could produce thousands of lines. The
// reporter must keep exactly one line per check, updated in place.
func TestReporterBoundsLineGrowthPerCheck(t *testing.T) {
	t.Parallel()

	var calls int

	condition := conditions.PollingCondition(
		"etcd members to be control plane nodes",
		func(context.Context) error {
			calls++

			return fmt.Errorf("attempt %d: members not consistent", calls)
		},
		time.Millisecond,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	waitDone := make(chan struct{})

	go func() {
		defer close(waitDone)

		condition.Wait(ctx) //nolint:errcheck // exercising the timeout path
	}()

	r := newReporter()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-waitDone:
			break loop
		case <-ticker.C:
			r.Update(condition)
		}
	}

	r.Update(condition)

	if lines := strings.Split(r.String(), "\n"); len(lines) != 1 {
		t.Fatalf("expected exactly one line for a single check polled repeatedly with a changing error, got %d:\n%s",
			len(lines), r.String())
	}
}
