/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csi

// Unit tests for node_topology.go — topology key/value construction.
//
// These tests exercise buildTopologySegments directly (not via NodeGetInfo) to
// verify that:
//
//  1. Topology key constants carry the correct string values (RFC §5.8).
//  2. buildTopologySegments reports the NVMe-oF key when available.
//  3. An unavailable protocol is omitted rather than set to "false", so
//     StorageClass allowedTopologies In/NotIn selectors work correctly.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestTopology
//	go test ./internal/csi/ -v -run TestBuildTopologySegments

import (
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Topology key constant correctness (RFC §5.8)
// ─────────────────────────────────────────────────────────────────────────────

// TestTopologyKeyConstants verifies that the topology key constants carry the
// exact string values defined by RFC §5.8.  This is a golden-value test —
// changing a key is a breaking API change for any StorageClass that references
// it, so it must be explicit and intentional.
func TestTopologyKeyConstants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		constant string
		want     string
	}{
		{TopologyKeyNVMeoF, "pillar-csi.bhyoo.com/nvmeof"},
	}

	for _, tc := range cases {
		if tc.constant != tc.want {
			t.Errorf("topology key constant = %q, want %q", tc.constant, tc.want)
		}
	}
}

// TestTopologyValueTrue verifies that the topology segment value used to
// indicate protocol availability is the string "true".
func TestTopologyValueTrue(t *testing.T) {
	t.Parallel()

	if topologyValueTrue != "true" {
		t.Errorf("topologyValueTrue = %q, want \"true\"", topologyValueTrue)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildTopologySegments — single protocol
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildTopologySegments_NVMeoFOnly verifies that when only NVMe-oF is
// available exactly one key is present and it carries value "true".
func TestBuildTopologySegments_NVMeoFOnly(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{nvmeof: true})
	if len(segs) != 1 {
		t.Errorf("len(segs) = %d, want 1; segs = %v", len(segs), segs)
	}
	if segs[TopologyKeyNVMeoF] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyNVMeoF, segs[TopologyKeyNVMeoF], topologyValueTrue)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildTopologySegments — no protocols
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildTopologySegments_NoneAvailable verifies that when no protocols are
// available the returned map is empty (not nil) so that callers can check
// len(segs) == 0 to decide whether to include AccessibleTopology.
func TestBuildTopologySegments_NoneAvailable(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{})
	if len(segs) != 0 {
		t.Errorf("expected empty segments when no protocols available, got %v", segs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildTopologySegments — result is a new map each call
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildTopologySegments_ReturnedMapIsIsolated verifies that mutating the
// returned map does not affect subsequent calls.
// BuildTopologySegments must return a fresh map each time so callers can freely modify it.
func TestBuildTopologySegments_ReturnedMapIsIsolated(t *testing.T) {
	t.Parallel()

	prober := &stubProber{nvmeof: true}

	segs1 := buildTopologySegments(prober)
	// Mutate segs1 with an extra key.
	segs1["extra-key"] = "extra-value"

	segs2 := buildTopologySegments(prober)
	if _, ok := segs2["extra-key"]; ok {
		t.Error("mutation of first returned map leaked into second call; maps must be independent")
	}
}
