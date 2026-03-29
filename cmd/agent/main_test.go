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
//   - backendFlag parses type=zfs-zvol,pool=<name>[,parent=<p>] values.
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

// Backend registry tests.

// buildBackends creates the pool→backend registry for testing, mirroring the
// backend-registration loop from main().  Keeping the logic here lets the test
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

// BackendFlag tests — validate the pluggable --backend flag parsing.

// TestBackendFlag_Set_BasicZfsZvol verifies that a well-formed
// "type=zfs-zvol,pool=<name>" value is parsed correctly.
func TestBackendFlag_Set_BasicZfsZvol(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=zfs-zvol,pool=tank"); err != nil {
		t.Fatalf("backendFlag.Set: unexpected error: %v", err)
	}
	if got, want := len(bf), 1; got != want {
		t.Fatalf("len(backendFlag) = %d; want %d", got, want)
	}
	if got, want := bf[0].typ, "zfs-zvol"; got != want {
		t.Errorf("spec.typ = %q; want %q", got, want)
	}
	if got, want := bf[0].pool, "tank"; got != want {
		t.Errorf("spec.pool = %q; want %q", got, want)
	}
	if got, want := bf[0].parent, ""; got != want {
		t.Errorf("spec.parent = %q; want %q", got, want)
	}
}

// TestBackendFlag_Set_WithParent verifies that the optional parent= key is
// parsed and stored correctly.
func TestBackendFlag_Set_WithParent(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=zfs-zvol,pool=hot-data,parent=k8s"); err != nil {
		t.Fatalf("backendFlag.Set: unexpected error: %v", err)
	}
	if got, want := bf[0].parent, "k8s"; got != want {
		t.Errorf("spec.parent = %q; want %q", got, want)
	}
}

// TestBackendFlag_Set_Repeated verifies that multiple Set calls accumulate
// distinct backendSpec entries in order.
func TestBackendFlag_Set_Repeated(t *testing.T) {
	t.Parallel()

	values := []string{
		"type=zfs-zvol,pool=alpha",
		"type=zfs-zvol,pool=beta,parent=ds",
		"type=zfs-zvol,pool=gamma",
	}

	var bf backendFlag
	for _, v := range values {
		if err := bf.Set(v); err != nil {
			t.Fatalf("backendFlag.Set(%q): %v", v, err)
		}
	}

	if got, want := len(bf), len(values); got != want {
		t.Fatalf("len(backendFlag) = %d; want %d", got, want)
	}

	pools := []string{"alpha", "beta", "gamma"}
	for i, want := range pools {
		if got := bf[i].pool; got != want {
			t.Errorf("bf[%d].pool = %q; want %q", i, got, want)
		}
	}
}

// TestBackendFlag_Set_RejectsEmpty verifies that an empty value returns an
// error.
func TestBackendFlag_Set_RejectsEmpty(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set(""); err == nil {
		t.Error("backendFlag.Set(\"\") expected error, got nil")
	}
}

// TestBackendFlag_Set_RejectsMissingType verifies that omitting the type=
// key returns an error.
func TestBackendFlag_Set_RejectsMissingType(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("pool=tank"); err == nil {
		t.Error("backendFlag.Set without type= expected error, got nil")
	}
}

// TestBackendFlag_Set_RejectsMissingPool verifies that omitting the pool=
// key returns an error.
func TestBackendFlag_Set_RejectsMissingPool(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=zfs-zvol"); err == nil {
		t.Error("backendFlag.Set without pool= expected error, got nil")
	}
}

// TestBackendFlag_Set_RejectsUnknownType verifies that an unsupported type
// value returns an error.
func TestBackendFlag_Set_RejectsUnknownType(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm,pool=vg0"); err == nil {
		t.Error("backendFlag.Set with unsupported type expected error, got nil")
	}
}

// TestBackendFlag_Set_RejectsUnknownKey verifies that an unrecognized key
// causes an error so that typos are detected early.
func TestBackendFlag_Set_RejectsUnknownKey(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=zfs-zvol,pool=tank,bogus=val"); err == nil {
		t.Error("backendFlag.Set with unknown key expected error, got nil")
	}
}

// TestBackendFlag_Set_RejectsNonKeyValue verifies that a token without '='
// returns an error.
func TestBackendFlag_Set_RejectsNonKeyValue(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=zfs-zvol,notakeyvalue,pool=tank"); err == nil {
		t.Error("backendFlag.Set with bare token expected error, got nil")
	}
}

// TestBackendFlag_String_Empty verifies that String() returns "" on a
// zero-value flag.
func TestBackendFlag_String_Empty(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if got := bf.String(); got != "" {
		t.Errorf("empty backendFlag.String() = %q; want %q", got, "")
	}
}

// TestBackendFlag_String_MultipleSpecs verifies that String() produces a
// space-separated summary for multiple registered specs.
func TestBackendFlag_String_MultipleSpecs(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=zfs-zvol,pool=alpha"); err != nil {
		t.Fatalf("bf.Set(alpha): %v", err)
	}
	if err := bf.Set("type=zfs-zvol,pool=beta,parent=k8s"); err != nil {
		t.Fatalf("bf.Set(beta): %v", err)
	}

	s := bf.String()
	if !strings.Contains(s, "pool=alpha") {
		t.Errorf("backendFlag.String() = %q; missing pool=alpha", s)
	}
	if !strings.Contains(s, "pool=beta") {
		t.Errorf("backendFlag.String() = %q; missing pool=beta", s)
	}
	if !strings.Contains(s, "parent=k8s") {
		t.Errorf("backendFlag.String() = %q; missing parent=k8s", s)
	}
}

// TestBackendFlag_BuildsCorrectBackend verifies that a backendFlag parsed
// from a --backend value produces a backend whose DevicePath includes the
// pool name — i.e. the spec is wired correctly to zfs.New.
func TestBackendFlag_BuildsCorrectBackend(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=zfs-zvol,pool=mypool,parent=ds"); err != nil {
		t.Fatalf("backendFlag.Set: %v", err)
	}

	// Replicate the --backend build loop from main().
	bs := make(map[string]backend.VolumeBackend, len(bf))
	for _, spec := range bf {
		bs[spec.pool] = zfs.New(spec.pool, spec.parent)
	}

	b, ok := bs["mypool"]
	if !ok {
		t.Fatal("backend for pool 'mypool' not found in registry")
	}

	devPath := b.DevicePath("mypool/pvc-test")
	wantPrefix := "/dev/zvol/mypool/"
	if !strings.HasPrefix(devPath, wantPrefix) {
		t.Errorf("DevicePath(%q) = %q; want prefix %q", "mypool/pvc-test", devPath, wantPrefix)
	}
}
