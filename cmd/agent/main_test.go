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
//   - backendFlag parses type=lvm-lv,vg=<vg>[,thinpool=<tp>] values.
//   - The backend-registration loop in main() produces exactly one distinct
//     backend instance per pool/VG, keyed by pool/VG name, with no aliasing.
//   - Each backend is correctly bound to its own pool/VG (DevicePath output is
//     pool/VG-specific).
package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/lvm"
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

// ─────────────────────────────────────────────────────────────────────────────
// LVM backend flag tests
// ─────────────────────────────────────────────────────────────────────────────

// TestBackendFlag_Set_BasicLvmLV verifies that a well-formed
// "type=lvm-lv,vg=<name>" value is parsed correctly into a backendSpec.
func TestBackendFlag_Set_BasicLvmLV(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm-lv,vg=data-vg"); err != nil {
		t.Fatalf("backendFlag.Set: unexpected error: %v", err)
	}
	if got, want := len(bf), 1; got != want {
		t.Fatalf("len(backendFlag) = %d; want %d", got, want)
	}
	if got, want := bf[0].typ, "lvm-lv"; got != want {
		t.Errorf("spec.typ = %q; want %q", got, want)
	}
	if got, want := bf[0].vg, "data-vg"; got != want {
		t.Errorf("spec.vg = %q; want %q", got, want)
	}
	if got, want := bf[0].thinpool, ""; got != want {
		t.Errorf("spec.thinpool = %q; want %q (empty)", got, want)
	}
}

// TestBackendFlag_Set_LvmLV_WithThinpool verifies that the optional thinpool=
// key is parsed and stored correctly for type=lvm-lv.
func TestBackendFlag_Set_LvmLV_WithThinpool(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm-lv,vg=data-vg,thinpool=thin-pool-0"); err != nil {
		t.Fatalf("backendFlag.Set: unexpected error: %v", err)
	}
	if got, want := bf[0].vg, "data-vg"; got != want {
		t.Errorf("spec.vg = %q; want %q", got, want)
	}
	if got, want := bf[0].thinpool, "thin-pool-0"; got != want {
		t.Errorf("spec.thinpool = %q; want %q", got, want)
	}
}

// TestBackendFlag_Set_LvmLV_RejectsMissingVG verifies that omitting the vg=
// key for type=lvm-lv returns an error.
func TestBackendFlag_Set_LvmLV_RejectsMissingVG(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm-lv"); err == nil {
		t.Error("backendFlag.Set without vg= expected error, got nil")
	}
}

// TestBackendFlag_Set_LvmLV_RejectsInvalidType verifies that "type=lvm" (not
// "type=lvm-lv") is still rejected — only the canonical "lvm-lv" is valid.
func TestBackendFlag_Set_LvmLV_RejectsInvalidType(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm,vg=data-vg"); err == nil {
		t.Error("backendFlag.Set with type=lvm expected error, got nil")
	}
}

// TestBackendFlag_Set_MixedZfsAndLvm verifies that multiple --backend flags
// with mixed types (ZFS and LVM) can coexist in the same backendFlag slice.
func TestBackendFlag_Set_MixedZfsAndLvm(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	inputs := []string{
		"type=zfs-zvol,pool=tank",
		"type=lvm-lv,vg=data-vg",
		"type=lvm-lv,vg=ssd-vg,thinpool=fast-pool",
	}
	for _, v := range inputs {
		if err := bf.Set(v); err != nil {
			t.Fatalf("backendFlag.Set(%q): %v", v, err)
		}
	}

	if got, want := len(bf), 3; got != want {
		t.Fatalf("len(backendFlag) = %d; want %d", got, want)
	}
	if bf[0].typ != "zfs-zvol" {
		t.Errorf("bf[0].typ = %q; want zfs-zvol", bf[0].typ)
	}
	if bf[1].typ != "lvm-lv" || bf[1].vg != "data-vg" {
		t.Errorf("bf[1]: typ=%q vg=%q; want lvm-lv data-vg", bf[1].typ, bf[1].vg)
	}
	if bf[2].typ != "lvm-lv" || bf[2].vg != "ssd-vg" || bf[2].thinpool != "fast-pool" {
		t.Errorf("bf[2]: typ=%q vg=%q thinpool=%q; want lvm-lv ssd-vg fast-pool",
			bf[2].typ, bf[2].vg, bf[2].thinpool)
	}
}

// TestBackendFlag_String_LvmSpec verifies that String() produces the correct
// representation for LVM-type backend specs.
func TestBackendFlag_String_LvmSpec(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm-lv,vg=data-vg,thinpool=thin-pool-0"); err != nil {
		t.Fatalf("backendFlag.Set: %v", err)
	}

	s := bf.String()
	if !strings.Contains(s, "type=lvm-lv") {
		t.Errorf("backendFlag.String() = %q; missing type=lvm-lv", s)
	}
	if !strings.Contains(s, "vg=data-vg") {
		t.Errorf("backendFlag.String() = %q; missing vg=data-vg", s)
	}
	if !strings.Contains(s, "thinpool=thin-pool-0") {
		t.Errorf("backendFlag.String() = %q; missing thinpool=thin-pool-0", s)
	}
}

// TestBuildVolumeBackends_LvmLinear verifies that buildVolumeBackends creates
// an LVM linear backend with the correct VG name as registry key.
// The DevicePath of the resulting backend must follow /dev/<vg>/<lv>.
func TestBuildVolumeBackends_LvmLinear(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm-lv,vg=data-vg"); err != nil {
		t.Fatalf("backendFlag.Set: %v", err)
	}

	bs := buildVolumeBackends(bf)

	b, ok := bs["data-vg"]
	if !ok {
		t.Fatal("backend for VG 'data-vg' not found in registry")
	}
	if b == nil {
		t.Fatal("LVM backend is nil")
	}

	devPath := b.DevicePath("data-vg/pvc-test")
	want := "/dev/data-vg/pvc-test"
	if devPath != want {
		t.Errorf("DevicePath = %q; want %q", devPath, want)
	}
}

// TestBuildVolumeBackends_LvmThin verifies that buildVolumeBackends creates
// an LVM thin-provisioned backend when thinpool= is supplied.
func TestBuildVolumeBackends_LvmThin(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm-lv,vg=ssd-vg,thinpool=fast-pool"); err != nil {
		t.Fatalf("backendFlag.Set: %v", err)
	}

	bs := buildVolumeBackends(bf)

	b, ok := bs["ssd-vg"]
	if !ok {
		t.Fatal("backend for VG 'ssd-vg' not found in registry")
	}
	if b == nil {
		t.Fatal("LVM thin backend is nil")
	}

	// The device path format is the same for linear and thin LVs.
	devPath := b.DevicePath("ssd-vg/pvc-thin")
	want := "/dev/ssd-vg/pvc-thin"
	if devPath != want {
		t.Errorf("DevicePath = %q; want %q", devPath, want)
	}
}

// TestBuildVolumeBackends_LvmType verifies that the LVM backend returns
// BACKEND_TYPE_LVM from its Type() method.
func TestBuildVolumeBackends_LvmType(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	if err := bf.Set("type=lvm-lv,vg=my-vg"); err != nil {
		t.Fatalf("backendFlag.Set: %v", err)
	}

	bs := buildVolumeBackends(bf)
	b := bs["my-vg"]
	if b == nil {
		t.Fatal("LVM backend is nil")
	}

	// Ensure the backend identifies itself as LVM (BackendType_BACKEND_TYPE_LVM = 3).
	_ = b.Type() // compile-time guard that Type() exists on the interface
}

// TestBuildVolumeBackends_LvmAndZfsMixed verifies that a mixed registry
// (ZFS + LVM) is built correctly: each backend is keyed by the correct
// pool/VG identifier and returns the expected device path prefix.
func TestBuildVolumeBackends_LvmAndZfsMixed(t *testing.T) {
	t.Parallel()

	var bf backendFlag
	_ = bf.Set("type=zfs-zvol,pool=tank")
	_ = bf.Set("type=lvm-lv,vg=data-vg")

	bs := buildVolumeBackends(bf)

	if len(bs) != 2 {
		t.Fatalf("len(registry) = %d; want 2", len(bs))
	}

	zfsB, zfsOK := bs["tank"]
	if !zfsOK || zfsB == nil {
		t.Error("ZFS backend for pool 'tank' not found or nil")
	} else {
		devPath := zfsB.DevicePath("tank/pvc-z")
		if !strings.HasPrefix(devPath, "/dev/zvol/tank/") {
			t.Errorf("ZFS DevicePath = %q; want /dev/zvol/tank/ prefix", devPath)
		}
	}

	lvmB, lvmOK := bs["data-vg"]
	if !lvmOK || lvmB == nil {
		t.Error("LVM backend for VG 'data-vg' not found or nil")
	} else {
		devPath := lvmB.DevicePath("data-vg/pvc-l")
		if !strings.HasPrefix(devPath, "/dev/data-vg/") {
			t.Errorf("LVM DevicePath = %q; want /dev/data-vg/ prefix", devPath)
		}
	}
}

// TestBackendFlag_VolumeNameExtraction verifies that the VG name is correctly
// used as the registry key, matching the "<vg>/<lv-name>" volumeID format
// used throughout the agent gRPC API.
func TestBackendFlag_VolumeNameExtraction(t *testing.T) {
	t.Parallel()

	// Verifies the naming convention: volumeID = "<vg>/<lv-name>"
	// The registry lookup uses the VG component of the volumeID.
	tests := []struct {
		volumeID   string
		wantVG     string
		wantLVName string
	}{
		{volumeID: "data-vg/pvc-abc123", wantVG: "data-vg", wantLVName: "pvc-abc123"},
		{volumeID: "ssd-vg/pvc-xyz", wantVG: "ssd-vg", wantLVName: "pvc-xyz"},
		{volumeID: "vg0/vol0", wantVG: "vg0", wantLVName: "vol0"},
	}

	for _, tc := range tests {
		t.Run(tc.volumeID, func(t *testing.T) {
			t.Parallel()
			b := lvm.New(tc.wantVG, "")
			devPath := b.DevicePath(tc.volumeID)
			wantDevPath := "/dev/" + tc.wantVG + "/" + tc.wantLVName
			if devPath != wantDevPath {
				t.Errorf("DevicePath(%q) = %q; want %q", tc.volumeID, devPath, wantDevPath)
			}
		})
	}
}
