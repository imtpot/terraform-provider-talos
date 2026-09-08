// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos

import "testing"

// TestStripImageTag is the red-phase test for the digest-truncation defect
// stripImageTag shared with replaceImageTag (issue #393): for a digest-pinned
// reference, stripping everything after the last ':' also eats into the
// "sha256:<hex>" digest, collapsing every digest down to the same
// "repo@sha256" string. With ignore_kubernetes_upgrade_drift = true, that made
// K8sManagedConfigHash blind to real changes to a pinned Kubernetes component
// image.
func TestStripImageTag(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		image string
		want  string
	}{
		"tag reference": {
			image: "ghcr.io/siderolabs/kubelet:v1.35.4",
			want:  "ghcr.io/siderolabs/kubelet",
		},
		"no tag": {
			image: "ghcr.io/siderolabs/kubelet",
			want:  "ghcr.io/siderolabs/kubelet",
		},
		"registry host:port with a tag": {
			image: "my-reg.example.com:5000/kubelet:v1.35.4",
			want:  "my-reg.example.com:5000/kubelet",
		},
		"registry host:port with no tag": {
			image: "my-reg.example.com:5000/kubelet",
			want:  "my-reg.example.com:5000/kubelet",
		},
		"digest reference is left untouched": {
			image: "ghcr.io/siderolabs/kubelet@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want:  "ghcr.io/siderolabs/kubelet@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := stripImageTag(test.image); got != test.want {
				t.Fatalf("stripImageTag(%q) = %q, want %q", test.image, got, test.want)
			}
		})
	}
}

// TestStripImageTagDistinguishesDigests proves two different digest-pinned
// references stay distinguishable after stripping, which is what
// K8sManagedConfigHash relies on to detect a real change to a pinned image.
// Before the fix, both collapsed to the identical "repo@sha256" string.
func TestStripImageTagDistinguishesDigests(t *testing.T) {
	t.Parallel()

	const repo = "ghcr.io/siderolabs/kubelet@sha256:"

	a := stripImageTag(repo + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	b := stripImageTag(repo + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	if a == b {
		t.Fatalf("stripImageTag collapsed two different digests to the same value: %q", a)
	}
}
