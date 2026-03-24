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

// White-box unit tests for cmd/agent/main.go.
//
// These tests verify:
//   - poolsFlag accumulates one entry per --zfs-pool invocation.
//   - The backend-registration loop in main() produces exactly one distinct
//     *zfs.Backend instance per pool, keyed by pool name, with no aliasing.
//   - Each backend is correctly bound to its own pool (DevicePath output is
//     pool-specific).
package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
)

// Unit tests for poolsFlag exercise multi-pool flag parsing.

// TestPoolsFlag_Set_SinglePool verifies that a single Set call populates the
// flag with exactly one entry.
func TestPoolsFlag_Set_SinglePool(t *testing.T) {
	t.Parallel()

	var pf poolsFlag
	if err := pf.Set("tank"); err != nil {
		t.Fatalf("poolsFlag.Set(%q): unexpected error: %v", "tank", err)
	}
	if got, want := len(pf), 1; got != want {
		t.Errorf("len(poolsFlag) = %d; want %d", got, want)
	}
	if got, want := pf[0], "tank"; got != want {
		t.Errorf("poolsFlag[0] = %q; want %q", got, want)
	}
}

// TestPoolsFlag_Set_MultiplePools verifies that repeated Set calls accumulate
// all pool names in order — exactly the behavior flag.Parse uses when the
// same flag appears multiple times on the command line.
func TestPoolsFlag_Set_MultiplePools(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}

	var pf poolsFlag
	for _, p := range pools {
		if err := pf.Set(p); err != nil {
			t.Fatalf("poolsFlag.Set(%q): unexpected error: %v", p, err)
		}
	}

	if got, want := len(pf), len(pools); got != want {
		t.Fatalf("len(poolsFlag) = %d; want %d", got, want)
	}
	for i, want := range pools {
		if got := pf[i]; got != want {
			t.Errorf("poolsFlag[%d] = %q; want %q", i, got, want)
		}
	}
}

// TestPoolsFlag_String_Empty verifies that String() returns "" on a
// zero-value (unset) flag, which is required by the flag.Value interface for
// the default-value display.
func TestPoolsFlag_String_Empty(t *testing.T) {
	t.Parallel()

	var pf poolsFlag
	if got := pf.String(); got != "" {
		t.Errorf("empty poolsFlag.String() = %q; want %q", got, "")
	}
}

// TestPoolsFlag_String_MultiplePools verifies that String() joins accumulated
// pool names with commas when multiple pools are registered.
func TestPoolsFlag_String_MultiplePools(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}
	want := strings.Join(pools, ",")

	var pf poolsFlag
	for _, p := range pools {
		if err := pf.Set(p); err != nil {
			t.Fatalf("poolsFlag.Set(%q): unexpected error: %v", p, err)
		}
	}

	if got := pf.String(); got != want {
		t.Errorf("poolsFlag.String() = %q; want %q", got, want)
	}
}

// TestPoolsFlag_Set_RejectsEmpty verifies that Set returns a non-nil error
// when supplied an empty pool name, guarding against misconfigurations.
func TestPoolsFlag_Set_RejectsEmpty(t *testing.T) {
	t.Parallel()

	var pf poolsFlag
	if err := pf.Set(""); err == nil {
		t.Error("poolsFlag.Set(\"\") expected error for empty name, got nil")
	}
}

// TestPoolsFlag_Set_NoDuplicateElimination verifies that poolsFlag is a
// simple accumulator: it does NOT deduplicate pool names.  Deduplication (or
// rejection of duplicates) is a policy concern left to the caller.
func TestPoolsFlag_Set_NoDuplicateElimination(t *testing.T) {
	t.Parallel()

	var pf poolsFlag
	if err := pf.Set("tank"); err != nil {
		t.Fatalf("poolsFlag.Set(%q): %v", "tank", err)
	}
	if err := pf.Set("tank"); err != nil {
		t.Fatalf("poolsFlag.Set(%q) second call: %v", "tank", err)
	}

	if got, want := len(pf), 2; got != want {
		t.Errorf("len(poolsFlag) after two identical Set calls = %d; want %d", got, want)
	}
}

// Backend registry tests.

// buildBackends mirrors the backend-registration loop from main():
//
//	backends := make(map[string]backend.VolumeBackend, len(zfsPools))
//	for _, pool := range zfsPools {
//	    backends[pool] = zfs.New(pool, zfsParent)
//	}
//
// Keeping the logic here (rather than calling main itself) lets the test
// remain a pure unit test with no flag/os interaction.
func buildBackends(pools []string, parent string) map[string]backend.VolumeBackend {
	backends := make(map[string]backend.VolumeBackend, len(pools))
	for _, pool := range pools {
		backends[pool] = zfs.New(pool, parent)
	}
	return backends
}

// TestBuildBackends_OneEntryPerPool verifies that the registry contains
// exactly one entry for each pool name and that all entries are non-nil.
func TestBuildBackends_OneEntryPerPool(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}
	backends := buildBackends(pools, "k8s")

	if got, want := len(backends), len(pools); got != want {
		t.Fatalf("len(backends) = %d; want %d", got, want)
	}

	for _, pool := range pools {
		b, ok := backends[pool]
		if !ok {
			t.Errorf("pool %q: not found in registry", pool)
			continue
		}
		if b == nil {
			t.Errorf("pool %q: backend is nil", pool)
		}
	}
}

// TestBuildBackends_DistinctInstances verifies that each pool maps to a
// distinct backend instance — no two pools share the same pointer.  Sharing
// would allow volume operations on one pool to accidentally mutate another's
// state.
func TestBuildBackends_DistinctInstances(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}
	backends := buildBackends(pools, "k8s")

	for i := range pools {
		for j := i + 1; j < len(pools); j++ {
			p1, p2 := pools[i], pools[j]
			if backends[p1] == backends[p2] {
				t.Errorf("pools %q and %q share the same backend instance (pointer aliasing)", p1, p2)
			}
		}
	}
}

// TestBuildBackends_PoolBoundCorrectly verifies that each backend in the
// registry is bound to the correct pool by inspecting DevicePath output.
//
// For a backend registered under pool P, calling DevicePath("P/vol") must
// return a path whose first component after /dev/zvol/ is P — not any other
// pool name.  This catches transposition bugs where, e.g., the loop variable
// is captured by reference rather than value.
func TestBuildBackends_PoolBoundCorrectly(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}
	const parent = "k8s"
	const volName = "pvc-verify"

	backends := buildBackends(pools, parent)

	for _, pool := range pools {
		t.Run(fmt.Sprintf("pool=%s", pool), func(t *testing.T) {
			t.Parallel()

			b := backends[pool]
			volumeID := pool + "/" + volName

			devPath := b.DevicePath(volumeID)

			// The path must start with /dev/zvol/<pool>/.
			wantPrefix := "/dev/zvol/" + pool + "/"
			if !strings.HasPrefix(devPath, wantPrefix) {
				t.Errorf("backends[%q].DevicePath(%q) = %q; want prefix %q",
					pool, volumeID, devPath, wantPrefix)
			}

			// Extra guard: the path must not start with any OTHER pool's
			// prefix, confirming no backend cross-mapping.
			for _, other := range pools {
				if other == pool {
					continue
				}
				wrongPrefix := "/dev/zvol/" + other + "/"
				if strings.HasPrefix(devPath, wrongPrefix) {
					t.Errorf("backends[%q].DevicePath(%q) = %q: starts with wrong pool prefix %q",
						pool, volumeID, devPath, wrongPrefix)
				}
			}
		})
	}
}

// TestBuildBackends_EmptyParent verifies that the registry is built correctly
// when no parent dataset is specified (parentDataset=""), which is a valid
// production configuration.
func TestBuildBackends_EmptyParent(t *testing.T) {
	t.Parallel()

	pools := []string{"fast", "slow"}
	backends := buildBackends(pools, "" /* no parent dataset */)

	for _, pool := range pools {
		b, ok := backends[pool]
		if !ok {
			t.Errorf("pool %q: not found in registry", pool)
			continue
		}

		volumeID := pool + "/pvc-x"
		devPath := b.DevicePath(volumeID)

		// With no parent dataset: /dev/zvol/<pool>/<volName>.
		want := "/dev/zvol/" + pool + "/pvc-x"
		if devPath != want {
			t.Errorf("backends[%q].DevicePath(%q) = %q; want %q", pool, volumeID, devPath, want)
		}
	}
}

// TestBuildBackends_FromPoolsFlag exercises the end-to-end flag→registry path:
// parse pool names via poolsFlag.Set (as flag.Parse would), then build the
// registry exactly as main() does.
func TestBuildBackends_FromPoolsFlag(t *testing.T) {
	t.Parallel()

	rawPools := []string{"alpha", "beta", "gamma"}

	var pf poolsFlag
	for _, p := range rawPools {
		if err := pf.Set(p); err != nil {
			t.Fatalf("poolsFlag.Set(%q): %v", p, err)
		}
	}

	// Replicate the main() registration loop verbatim.
	const parent = "volumes"
	backends := make(map[string]backend.VolumeBackend, len(pf))
	for _, pool := range pf {
		backends[pool] = zfs.New(pool, parent)
	}

	// One backend per flag occurrence.
	if got, want := len(backends), len(rawPools); got != want {
		t.Fatalf("registry size = %d; want %d", got, want)
	}

	// Every pool flag value must appear as a registry key.
	for _, pool := range pf {
		if _, ok := backends[pool]; !ok {
			t.Errorf("pool %q from poolsFlag missing from registry", pool)
		}
	}

	// All instances must be distinct.
	seen := make(map[backend.VolumeBackend]string, len(backends))
	for pool, b := range backends {
		if prev, dup := seen[b]; dup {
			t.Errorf("pools %q and %q share the same backend instance", prev, pool)
		}
		seen[b] = pool
	}
}
