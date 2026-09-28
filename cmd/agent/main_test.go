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

// White-box unit tests for the agent's backend config file and registry.
//
// These tests verify:
//   - the --config file (backends: [{zfs: ...} | {lvm: ...}]) is decoded
//     strictly with the shared configdocs decoder, rejecting unknown fields,
//     removed vocabulary and invalid placements with the entry path;
//   - buildVolumeBackends produces exactly one distinct backend per pool/VG,
//     keyed by pool/VG name, bound to its own pool (DevicePath), and refuses
//     duplicate keys.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pillarv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/lvm"
)

const testConfigSource = "/etc/pillar-agent/config.yaml"

// mustParse parses an agent config document, failing the test on error.
func mustParse(t *testing.T, doc string) []pillarv1alpha1.BackendSpec {
	t.Helper()
	specs, err := parseAgentConfig(testConfigSource, []byte(doc))
	if err != nil {
		t.Fatalf("parseAgentConfig: unexpected error: %v\n%s", err, doc)
	}
	return specs
}

// mustBuild parses an agent config document and builds its registry.
func mustBuild(t *testing.T, doc string) map[string]backend.VolumeBackend {
	t.Helper()
	bs, err := buildVolumeBackends(mustParse(t, doc))
	if err != nil {
		t.Fatalf("buildVolumeBackends: unexpected error: %v", err)
	}
	return bs
}

// zfsPoolsDoc renders a config with one zfs entry per pool.
func zfsPoolsDoc(pools []string, parent string) string {
	var b strings.Builder
	b.WriteString("backends:\n")
	for _, p := range pools {
		if parent == "" {
			fmt.Fprintf(&b, "  - zfs: {pool: %s}\n", p)
		} else {
			fmt.Fprintf(&b, "  - zfs: {pool: %s, parentDataset: %s}\n", p, parent)
		}
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// Registry tests
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildVolumeBackends_OneEntryPerPool verifies that the registry contains
// exactly one non-nil entry for each configured pool.
func TestBuildVolumeBackends_OneEntryPerPool(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}
	backends := mustBuild(t, zfsPoolsDoc(pools, "k8s"))

	if got, want := len(backends), len(pools); got != want {
		t.Fatalf("len(backends) = %d; want %d", got, want)
	}
	for _, pool := range pools {
		if b, ok := backends[pool]; !ok || b == nil {
			t.Errorf("pool %q: missing or nil in registry", pool)
		}
	}
}

// TestBuildVolumeBackends_DistinctInstances verifies that each pool maps to a
// distinct backend instance — sharing would let volume operations on one
// pool mutate another's state.
func TestBuildVolumeBackends_DistinctInstances(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}
	backends := mustBuild(t, zfsPoolsDoc(pools, "k8s"))

	for i := range pools {
		for j := i + 1; j < len(pools); j++ {
			if backends[pools[i]] == backends[pools[j]] {
				t.Errorf("pools %q and %q share the same backend instance", pools[i], pools[j])
			}
		}
	}
}

// TestBuildVolumeBackends_PoolBoundCorrectly verifies that each backend is
// bound to its own pool: DevicePath("P/vol") must start with /dev/zvol/P/
// and never with another pool's prefix (catches loop-variable aliasing).
func TestBuildVolumeBackends_PoolBoundCorrectly(t *testing.T) {
	t.Parallel()

	pools := []string{"tank", "hot-data", "ssd-pool"}
	backends := mustBuild(t, zfsPoolsDoc(pools, "k8s"))

	for _, pool := range pools {
		devPath := backends[pool].DevicePath(pool + "/pvc-verify")
		if want := "/dev/zvol/" + pool + "/"; !strings.HasPrefix(devPath, want) {
			t.Errorf("backends[%q].DevicePath = %q; want prefix %q", pool, devPath, want)
		}
		for _, other := range pools {
			if other != pool && strings.HasPrefix(devPath, "/dev/zvol/"+other+"/") {
				t.Errorf("backends[%q].DevicePath = %q: bound to pool %q", pool, devPath, other)
			}
		}
	}
}

// TestBuildVolumeBackends_EmptyParent verifies that omitting parentDataset
// places volumes at the pool root.
func TestBuildVolumeBackends_EmptyParent(t *testing.T) {
	t.Parallel()

	backends := mustBuild(t, zfsPoolsDoc([]string{"fast", "slow"}, ""))
	for _, pool := range []string{"fast", "slow"} {
		if got, want := backends[pool].DevicePath(pool+"/pvc-x"), "/dev/zvol/"+pool+"/pvc-x"; got != want {
			t.Errorf("backends[%q].DevicePath = %q; want %q", pool, got, want)
		}
	}
}

// TestBuildVolumeBackends_Lvm verifies that LVM entries are keyed by VG name
// and produce /dev/<vg>/<lv> device paths, linear and thin alike.
func TestBuildVolumeBackends_Lvm(t *testing.T) {
	t.Parallel()

	backends := mustBuild(t, `
backends:
  - lvm: {volumeGroup: data-vg}
  - lvm: {volumeGroup: ssd-vg, thinPool: fast-pool, provisioningMode: thin}
`)
	for vg, volID := range map[string]string{"data-vg": "data-vg/pvc-l", "ssd-vg": "ssd-vg/pvc-thin"} {
		b, ok := backends[vg]
		if !ok || b == nil {
			t.Fatalf("backend for VG %q missing", vg)
		}
		if got, want := b.DevicePath(volID), "/dev/"+volID; got != want {
			t.Errorf("DevicePath(%q) = %q; want %q", volID, got, want)
		}
	}
}

// TestBuildVolumeBackends_LvmProvisioningMode verifies the config's
// lvm.provisioningMode becomes the backend's mode for requests without a
// per-volume mode: linear by default even when a thin pool is configured.
func TestBuildVolumeBackends_LvmProvisioningMode(t *testing.T) {
	t.Parallel()

	backends := mustBuild(t, `
backends:
  - lvm: {volumeGroup: plain-vg}
  - lvm: {volumeGroup: pool-vg, thinPool: t0}
  - lvm: {volumeGroup: thin-vg, thinPool: t0, provisioningMode: thin}
`)
	for vg, want := range map[string]lvm.ProvisionMode{
		"plain-vg": lvm.ProvisionModeLinear,
		"pool-vg":  lvm.ProvisionModeLinear,
		"thin-vg":  lvm.ProvisionModeThin,
	} {
		b, ok := backends[vg].(*lvm.Backend)
		if !ok {
			t.Fatalf("backend for VG %q is %T, want *lvm.Backend", vg, backends[vg])
		}
		if got := b.Mode(); got != want {
			t.Errorf("VG %q: Mode() = %v, want %v", vg, got, want)
		}
	}
}

// TestParseAgentConfig_DateShapedNamesStayStrings verifies plain scalars
// that YAML would read as timestamps keep their text, as they do in the
// PillarStore the layout is compared with.
func TestParseAgentConfig_DateShapedNamesStayStrings(t *testing.T) {
	t.Parallel()

	specs := mustParse(t, `
backends:
  - zfs: {pool: tank, parentDataset: 2026-01-01}
  - lvm: {volumeGroup: 2026-02-02}
`)
	if got := specs[0].ZFS.ParentDataset; got != "2026-01-01" {
		t.Errorf("parentDataset = %q, want 2026-01-01", got)
	}
	if got := specs[1].LVM.VolumeGroup; got != "2026-02-02" {
		t.Errorf("volumeGroup = %q, want 2026-02-02", got)
	}
}

// TestBuildVolumeBackends_LvmAndZfsMixed verifies a mixed registry keys each
// backend by its own pool/VG with the matching device path layout.
func TestBuildVolumeBackends_LvmAndZfsMixed(t *testing.T) {
	t.Parallel()

	bs := mustBuild(t, `
backends:
  - zfs: {pool: tank}
  - lvm: {volumeGroup: data-vg}
`)
	if len(bs) != 2 {
		t.Fatalf("len(registry) = %d; want 2", len(bs))
	}
	if got := bs["tank"].DevicePath("tank/pvc-z"); !strings.HasPrefix(got, "/dev/zvol/tank/") {
		t.Errorf("ZFS DevicePath = %q; want /dev/zvol/tank/ prefix", got)
	}
	if got := bs["data-vg"].DevicePath("data-vg/pvc-l"); !strings.HasPrefix(got, "/dev/data-vg/") {
		t.Errorf("LVM DevicePath = %q; want /dev/data-vg/ prefix", got)
	}
}

// TestBuildVolumeBackends_DuplicateKeyRejected verifies that two entries
// sharing a pool/VG registry key are rejected instead of silently dropping
// all but the last.  RPCs route by the volume ID's first path component
// alone, so two backends behind one key are indistinguishable (issue #100).
func TestBuildVolumeBackends_DuplicateKeyRejected(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"same zfs pool, different parents": `
backends:
  - zfs: {pool: tank, parentDataset: fast}
  - zfs: {pool: tank, parentDataset: bulk}
`,
		"same zfs pool, identical entries": `
backends:
  - zfs: {pool: tank, parentDataset: k8s}
  - zfs: {pool: tank, parentDataset: k8s}
`,
		"same lvm vg, different thin pools": `
backends:
  - lvm: {volumeGroup: data-vg, thinPool: thin-a}
  - lvm: {volumeGroup: data-vg, thinPool: thin-b}
`,
		"cross-backend collision: zfs pool equals lvm vg": `
backends:
  - zfs: {pool: shared}
  - lvm: {volumeGroup: shared}
`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bs, err := buildVolumeBackends(mustParse(t, doc))
			if err == nil {
				t.Fatal("buildVolumeBackends succeeded; want duplicate-key error")
			}
			if bs != nil {
				t.Error("buildVolumeBackends returned a registry alongside the error")
			}
			if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "backends[1]") {
				t.Errorf("error %q should name the duplicate and the conflicting entry backends[1]", err)
			}
		})
	}
}

// TestBuildVolumeBackends_DistinctKeysAccepted verifies distinct pools and
// VGs build a complete registry.
func TestBuildVolumeBackends_DistinctKeysAccepted(t *testing.T) {
	t.Parallel()

	bs := mustBuild(t, `
backends:
  - zfs: {pool: tank, parentDataset: fast}
  - zfs: {pool: archive, parentDataset: bulk}
  - lvm: {volumeGroup: data-vg}
  - lvm: {volumeGroup: ssd-vg, thinPool: thin-0}
`)
	if len(bs) != 4 {
		t.Fatalf("len(registry) = %d; want 4", len(bs))
	}
	for _, key := range []string{"tank", "archive", "data-vg", "ssd-vg"} {
		if bs[key] == nil {
			t.Errorf("registry missing backend for %q", key)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Config file decoding tests
// ─────────────────────────────────────────────────────────────────────────────

// TestParseAgentConfig_ValidFullSpec verifies the full backend shape —
// structural placement fields included — decodes with CRD defaults applied
// (zfs.volumeType=zvol, lvm.provisioningMode=linear) and entry order kept.
func TestParseAgentConfig_ValidFullSpec(t *testing.T) {
	t.Parallel()

	specs := mustParse(t, `
backends:
  - zfs: {pool: hot-data, parentDataset: k8s}
  - zfs: {volumeType: zvol, pool: tank}
  - lvm: {volumeGroup: data-vg}
  - lvm: {volumeGroup: ssd-vg, thinPool: thin0, provisioningMode: thin}
`)
	want := []pillarv1alpha1.BackendSpec{
		{ZFS: &pillarv1alpha1.ZFSBackendConfig{
			VolumeType: pillarv1alpha1.ZFSVolumeTypeZvol, Pool: "hot-data", ParentDataset: "k8s",
		}},
		{ZFS: &pillarv1alpha1.ZFSBackendConfig{VolumeType: pillarv1alpha1.ZFSVolumeTypeZvol, Pool: "tank"}},
		{LVM: &pillarv1alpha1.LVMBackendConfig{
			VolumeGroup: "data-vg", ProvisioningMode: pillarv1alpha1.LVMProvisioningModeLinear,
		}},
		{LVM: &pillarv1alpha1.LVMBackendConfig{
			VolumeGroup: "ssd-vg", ThinPool: "thin0", ProvisioningMode: pillarv1alpha1.LVMProvisioningModeThin,
		}},
	}
	if !reflect.DeepEqual(specs, want) {
		t.Errorf("specs = %s; want %s (defaults volumeType=zvol, provisioningMode=linear applied)",
			describeSpecs(specs), describeSpecs(want))
	}
}

func describeSpecs(specs []pillarv1alpha1.BackendSpec) string {
	parts := make([]string, 0, len(specs))
	for _, s := range specs {
		switch {
		case s.ZFS != nil:
			parts = append(parts, fmt.Sprintf("zfs%+v", *s.ZFS))
		case s.LVM != nil:
			parts = append(parts, fmt.Sprintf("lvm%+v", *s.LVM))
		default:
			parts = append(parts, "{}")
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestParseAgentConfig_Rejected verifies every invalid document is refused
// with an error naming the config file and the offending path.
func TestParseAgentConfig_Rejected(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		doc     string
		wantErr []string
	}{
		"empty document": {
			doc:     "",
			wantErr: []string{"at least one backend is required"},
		},
		"empty backends list": {
			doc:     "backends: []\n",
			wantErr: []string{"backends: at least one backend is required"},
		},
		"backends not a list": {
			doc:     "backends:\n  zfs: {pool: tank}\n",
			wantErr: []string{"backends: expected a list"},
		},
		"unknown top-level field": {
			doc:     "backends:\n  - zfs: {pool: tank}\nlisten: \":9500\"\n",
			wantErr: []string{`unknown field "listen"`},
		},
		"duplicate key keeps neither value": {
			doc:     "backends:\n  - zfs: {pool: tank, pool: other}\n",
			wantErr: []string{`mapping key "pool" already defined`},
		},
		"duplicate top-level key": {
			doc:     "backends:\n  - zfs: {pool: tank}\nbackends:\n  - lvm: {volumeGroup: vg0}\n",
			wantErr: []string{`mapping key "backends" already defined`},
		},
		"unknown member field": {
			doc:     "backends:\n  - zfs: {pool: tank, bogus: x}\n",
			wantErr: []string{"backends[0]", "unknown field zfs.bogus"},
		},
		"removed flat vocabulary (type/pool)": {
			doc:     "backends:\n  - type: zfs-zvol\n    pool: tank\n",
			wantErr: []string{"backends[0]", `unknown field "pool"`},
		},
		"removed lvm vocabulary (vg/thinpool)": {
			doc:     "backends:\n  - lvm: {vg: data-vg, thinpool: t0}\n",
			wantErr: []string{"backends[0]", "unknown field lvm.thinpool"},
		},
		"unimplemented backend member": {
			doc:     "backends:\n  - zfs: {pool: tank}\n  - dir: {path: /srv}\n",
			wantErr: []string{"backends[1]", `unknown field "dir"`},
		},
		"both members in one entry": {
			doc:     "backends:\n  - zfs: {pool: tank}\n    lvm: {volumeGroup: vg0}\n",
			wantErr: []string{"backends[0]", "exactly one of"},
		},
		"null entry": {
			doc:     "backends:\n  - zfs: {pool: tank}\n  -\n",
			wantErr: []string{"backends[1]", "entry is empty"},
		},
		"unimplemented zfs volumeType": {
			doc:     "backends:\n  - zfs: {volumeType: dataset, pool: tank}\n",
			wantErr: []string{"backends[0]", "zfs.volumeType"},
		},
		"zfs pool missing": {
			doc:     "backends:\n  - zfs: {parentDataset: k8s}\n",
			wantErr: []string{"backends[0]", "zfs.pool is required"},
		},
		"zfs pool empty": {
			doc:     "backends:\n  - zfs: {pool: \"\"}\n",
			wantErr: []string{"backends[0]", "zfs.pool"},
		},
		"zfs properties are per-volume": {
			doc:     "backends:\n  - zfs: {pool: tank, properties: {compression: lz4}}\n",
			wantErr: []string{"backends[0]", "zfs.properties"},
		},
		"lvm volumeGroup missing": {
			doc:     "backends:\n  - lvm: {thinPool: t0}\n",
			wantErr: []string{"backends[0]", "lvm.volumeGroup"},
		},
		"lvm volumeGroup illegal name": {
			doc:     "backends:\n  - lvm: {volumeGroup: -bad}\n",
			wantErr: []string{"backends[0]", "lvm.volumeGroup"},
		},
		"lvm provisioningMode enum": {
			doc:     "backends:\n  - lvm: {volumeGroup: vg0, provisioningMode: striped}\n",
			wantErr: []string{"backends[0]", "lvm.provisioningMode"},
		},
		"lvm thin mode without thinPool": {
			doc:     "backends:\n  - lvm: {volumeGroup: vg0, provisioningMode: thin}\n",
			wantErr: []string{"backends[0]", "requires lvm.thinPool"},
		},
		"invalid YAML": {
			doc:     "backends: [\n",
			wantErr: []string{"invalid YAML"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			specs, err := parseAgentConfig(testConfigSource, []byte(tc.doc))
			if err == nil {
				t.Fatalf("parseAgentConfig succeeded with %+v; want error", specs)
			}
			for _, want := range append([]string{testConfigSource}, tc.wantErr...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestParseAgentConfig_RejectsEscapingParent verifies that a parentDataset
// leaving the pool is refused at startup: "../k8s" would create volumes in
// pool "k8s" while the agent reports a layout inside "tank" (issue #113).
// A nested dataset path stays valid.
func TestParseAgentConfig_RejectsEscapingParent(t *testing.T) {
	t.Parallel()

	for _, parent := range []string{"../k8s", "k8s/../../other", "./k8s", "."} {
		doc := fmt.Sprintf("backends:\n  - zfs: {pool: tank, parentDataset: %q}\n", parent)
		_, err := parseAgentConfig(testConfigSource, []byte(doc))
		if err == nil || !strings.Contains(err.Error(), "zfs.parentDataset") {
			t.Errorf("parentDataset %q: err = %v; want zfs.parentDataset rejection", parent, err)
		}
	}
	mustParse(t, "backends:\n  - zfs: {pool: tank, parentDataset: k8s/sub}\n")
}

// TestLoadAgentConfig_File verifies loading from disk: a valid file decodes
// and a missing file is an error naming the path.
func TestLoadAgentConfig_File(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("backends:\n  - lvm: {volumeGroup: data-vg}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	specs, err := loadAgentConfig(path)
	if err != nil {
		t.Fatalf("loadAgentConfig: %v", err)
	}
	if len(specs) != 1 || specs[0].PoolName() != "data-vg" {
		t.Errorf("specs = %+v; want one lvm backend data-vg", specs)
	}

	missing := filepath.Join(dir, "absent.yaml")
	if _, err := loadAgentConfig(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("loadAgentConfig(missing) err = %v; want error naming %s", err, missing)
	}
}
