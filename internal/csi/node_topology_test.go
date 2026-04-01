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
//  2. buildTopologySegments produces the right key-value pairs for every
//     combination of protocol availability.
//  3. Absent protocols are omitted rather than set to "false", so
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
		{TopologyKeyISCSI, "pillar-csi.bhyoo.com/iscsi"},
		{TopologyKeyNFS, "pillar-csi.bhyoo.com/nfs"},
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

// TestBuildTopologySegments_ISCSIOnly verifies that when only iSCSI is
// available exactly one key is present and it carries value "true".
func TestBuildTopologySegments_ISCSIOnly(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{iscsi: true})
	if len(segs) != 1 {
		t.Errorf("len(segs) = %d, want 1; segs = %v", len(segs), segs)
	}
	if segs[TopologyKeyISCSI] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyISCSI, segs[TopologyKeyISCSI], topologyValueTrue)
	}
}

// TestBuildTopologySegments_NFSOnly verifies that when only NFS is available
// exactly one key is present and it carries value "true".
func TestBuildTopologySegments_NFSOnly(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{nfs: true})
	if len(segs) != 1 {
		t.Errorf("len(segs) = %d, want 1; segs = %v", len(segs), segs)
	}
	if segs[TopologyKeyNFS] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyNFS, segs[TopologyKeyNFS], topologyValueTrue)
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
// buildTopologySegments — all protocols
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildTopologySegments_AllProtocols verifies that when all three
// protocols are available all three keys are present.
func TestBuildTopologySegments_AllProtocols(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{nvmeof: true, iscsi: true, nfs: true})
	if len(segs) != 3 {
		t.Errorf("len(segs) = %d, want 3; segs = %v", len(segs), segs)
	}
	for _, key := range []string{TopologyKeyNVMeoF, TopologyKeyISCSI, TopologyKeyNFS} {
		if segs[key] != topologyValueTrue {
			t.Errorf("segs[%q] = %q, want %q", key, segs[key], topologyValueTrue)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildTopologySegments — partial combinations
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildTopologySegments_NVMeoFAndISCSI verifies the NVMe-oF + iSCSI
// combination produces exactly two keys.
func TestBuildTopologySegments_NVMeoFAndISCSI(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{nvmeof: true, iscsi: true})
	if len(segs) != 2 {
		t.Errorf("len(segs) = %d, want 2; segs = %v", len(segs), segs)
	}
	if segs[TopologyKeyNVMeoF] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyNVMeoF, segs[TopologyKeyNVMeoF], topologyValueTrue)
	}
	if segs[TopologyKeyISCSI] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyISCSI, segs[TopologyKeyISCSI], topologyValueTrue)
	}
	if _, ok := segs[TopologyKeyNFS]; ok {
		t.Errorf("unexpected key %q present when NFS is not available", TopologyKeyNFS)
	}
}

// TestBuildTopologySegments_NVMeoFAndNFS verifies the NVMe-oF + NFS
// combination produces exactly two keys.
func TestBuildTopologySegments_NVMeoFAndNFS(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{nvmeof: true, nfs: true})
	if len(segs) != 2 {
		t.Errorf("len(segs) = %d, want 2; segs = %v", len(segs), segs)
	}
	if segs[TopologyKeyNVMeoF] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyNVMeoF, segs[TopologyKeyNVMeoF], topologyValueTrue)
	}
	if segs[TopologyKeyNFS] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyNFS, segs[TopologyKeyNFS], topologyValueTrue)
	}
	if _, ok := segs[TopologyKeyISCSI]; ok {
		t.Errorf("unexpected key %q present when iSCSI is not available", TopologyKeyISCSI)
	}
}

// TestBuildTopologySegments_ISCSIAndNFS verifies the iSCSI + NFS
// combination produces exactly two keys.
func TestBuildTopologySegments_ISCSIAndNFS(t *testing.T) {
	t.Parallel()

	segs := buildTopologySegments(&stubProber{iscsi: true, nfs: true})
	if len(segs) != 2 {
		t.Errorf("len(segs) = %d, want 2; segs = %v", len(segs), segs)
	}
	if segs[TopologyKeyISCSI] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyISCSI, segs[TopologyKeyISCSI], topologyValueTrue)
	}
	if segs[TopologyKeyNFS] != topologyValueTrue {
		t.Errorf("segs[%q] = %q, want %q", TopologyKeyNFS, segs[TopologyKeyNFS], topologyValueTrue)
	}
	if _, ok := segs[TopologyKeyNVMeoF]; ok {
		t.Errorf("unexpected key %q present when NVMe-oF is not available", TopologyKeyNVMeoF)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildTopologySegments — absent keys are omitted, not "false"
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildTopologySegments_AbsentKeysOmitted verifies that protocols that are
// not available are absent from the map entirely, rather than being set to the
// string "false".  StorageClass allowedTopologies with In/NotIn operators
// interpret key absence differently from key-with-false-value, so omission is
// the correct semantics (RFC §5.8).
func TestBuildTopologySegments_AbsentKeysOmitted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		prober      ProtocolProber
		presentKeys []string
		absentKeys  []string
	}{
		{
			name:        "nvmeof only — iscsi and nfs absent",
			prober:      &stubProber{nvmeof: true},
			presentKeys: []string{TopologyKeyNVMeoF},
			absentKeys:  []string{TopologyKeyISCSI, TopologyKeyNFS},
		},
		{
			name:        "iscsi only — nvmeof and nfs absent",
			prober:      &stubProber{iscsi: true},
			presentKeys: []string{TopologyKeyISCSI},
			absentKeys:  []string{TopologyKeyNVMeoF, TopologyKeyNFS},
		},
		{
			name:        "nfs only — nvmeof and iscsi absent",
			prober:      &stubProber{nfs: true},
			presentKeys: []string{TopologyKeyNFS},
			absentKeys:  []string{TopologyKeyNVMeoF, TopologyKeyISCSI},
		},
		{
			name:        "no protocols — all keys absent",
			prober:      &stubProber{},
			presentKeys: nil,
			absentKeys:  []string{TopologyKeyNVMeoF, TopologyKeyISCSI, TopologyKeyNFS},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			segs := buildTopologySegments(tc.prober)
			for _, key := range tc.presentKeys {
				if segs[key] != topologyValueTrue {
					t.Errorf("present key %q = %q, want %q", key, segs[key], topologyValueTrue)
				}
			}
			for _, key := range tc.absentKeys {
				if val, ok := segs[key]; ok {
					t.Errorf("absent key %q is present with value %q (should be omitted)", key, val)
				}
			}
		})
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
