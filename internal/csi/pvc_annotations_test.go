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

// Tests for ParsePVCAnnotations and the three internal parsers.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestParsePVCAnnotations

import (
	"encoding/json"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────.

// mustParseAnnotations calls ParsePVCAnnotations and fails the test on error.
func mustParseAnnotations(t *testing.T, ann map[string]string) map[string]string {
	t.Helper()
	result, err := ParsePVCAnnotations(ann)
	if err != nil {
		t.Fatalf("ParsePVCAnnotations: unexpected error: %v", err)
	}
	return result
}

// assertParam asserts that the result map contains key=want.
func assertParam(t *testing.T, result map[string]string, key, want string) {
	t.Helper()
	got, ok := result[key]
	if !ok {
		t.Errorf("key %q: not present in result", key)
		return
	}
	if got != want {
		t.Errorf("key %q: got %q, want %q", key, got, want)
	}
}

// assertNoKey asserts that the result map does NOT contain key.
func assertNoKey(t *testing.T, result map[string]string, key string) {
	t.Helper()
	if _, ok := result[key]; ok {
		t.Errorf("key %q: unexpectedly present in result", key)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ParsePVCAnnotations — nil / empty input
// ─────────────────────────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_Nil(t *testing.T) {
	result, err := ParsePVCAnnotations(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result must never be nil")
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestParsePVCAnnotations_Empty(t *testing.T) {
	result := mustParseAnnotations(t, map[string]string{})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestParsePVCAnnotations_UnrelatedAnnotationsIgnored(t *testing.T) {
	result := mustParseAnnotations(t, map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
		"some.other.annotation/foo":                        "bar",
	})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Flat param.* overrides (legacy path)
// ─────────────────────────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_FlatParamPrefix(t *testing.T) {
	ann := map[string]string{
		// Should be stripped to "zfs-prop.compression"
		"pillar-csi.bhyoo.com/param.zfs-prop.compression": "zstd",
		// Should be stripped to "zfs-prop.volblocksize"
		"pillar-csi.bhyoo.com/param.zfs-prop.volblocksize": "16K",
		// Prefix-only (no suffix) — must be ignored
		"pillar-csi.bhyoo.com/param.": "ignored",
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, "zfs-prop.compression", "zstd")
	assertParam(t, result, "zfs-prop.volblocksize", "16K")
	// Prefix-only key must not appear
	assertNoKey(t, result, "")
}

// ─────────────────────────────────────────────────────────────────────────────
// backend-override annotation
// ─────────────────────────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_BackendOverride_ZFS(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
zfs:
  properties:
    volblocksize: "8K"
    compression: zstd
`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramZFSPropPrefix+"volblocksize", "8K")
	assertParam(t, result, paramZFSPropPrefix+"compression", "zstd")
}

func TestParsePVCAnnotations_BackendOverride_ZFS_Quota(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
zfs:
  properties:
    quota: 500G
    reservation: 100G
`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramZFSPropPrefix+"quota", "500G")
	assertParam(t, result, paramZFSPropPrefix+"reservation", "100G")
}

func TestParsePVCAnnotations_BackendOverride_Empty(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: "{}",
	}
	result := mustParseAnnotations(t, ann)
	if len(result) != 0 {
		t.Errorf("expected empty result for empty backend override, got %v", result)
	}
}

func TestParsePVCAnnotations_BackendOverride_EmptyString(t *testing.T) {
	// Empty annotation value should be treated as absent.
	ann := map[string]string{
		AnnotationBackendOverride: "",
	}
	result := mustParseAnnotations(t, ann)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}
}

// ── Blocked structural fields in backend-override ─────────────────────────.

func TestParsePVCAnnotations_BackendOverride_ZFS_BlockedPool(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
zfs:
  pool: hacked-pool
  properties:
    compression: lz4
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for blocked zfs.pool field")
	}
}

func TestParsePVCAnnotations_BackendOverride_ZFS_BlockedParentDataset(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
zfs:
  parentDataset: k8s/override
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for blocked zfs.parentDataset field")
	}
}

// ── YAML malformed ────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_BackendOverride_MalformedYAML(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: "this: is: invalid: yaml: [",
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestParsePVCAnnotations_BackendOverride_ZFS_NotAMap(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: "zfs: just-a-string",
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when zfs is not a map")
	}
}

func TestParsePVCAnnotations_BackendOverride_ZFS_Properties_NotAMap(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
zfs:
  properties: not-a-map
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when zfs.properties is not a map")
	}
}

// ── LVM backend-override ─────────────────────────────────────────────────.

func TestParsePVCAnnotations_BackendOverride_LVM_ProvisioningMode(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
lvm:
  provisioningMode: linear
`,
	}
	result := mustParseAnnotations(t, ann)
	assertParam(t, result, paramLVMMode, "linear")
}

func TestParsePVCAnnotations_BackendOverride_LVM_BlockedVolumeGroup(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
lvm:
  volumeGroup: hacked-vg
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for blocked lvm.volumeGroup field")
	}
}

func TestParsePVCAnnotations_BackendOverride_LVM_BlockedThinPool(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: `
lvm:
  thinPool: hacked-pool
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for blocked lvm.thinPool field")
	}
}

func TestParsePVCAnnotations_BackendOverride_LVM_NotAMap(t *testing.T) {
	ann := map[string]string{
		AnnotationBackendOverride: "lvm: just-a-string",
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when lvm is not a map")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// protocol-override annotation
// ─────────────────────────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_ProtocolOverride_NVMeOF(t *testing.T) {
	ann := map[string]string{
		AnnotationProtocolOverride: `
nvmeofTcp:
  maxQueueSize: 64
  ctrlLossTmo: 600
  reconnectDelay: 10
`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramNVMeOFMaxQueueSize, "64")
	assertParam(t, result, paramNVMeOFCtrlLossTmo, "600")
	assertParam(t, result, paramNVMeOFReconnectDelay, "10")
}

func TestParsePVCAnnotations_ProtocolOverride_NVMeOF_Partial(t *testing.T) {
	// Only maxQueueSize is overridden — other keys must not appear in output.
	ann := map[string]string{
		AnnotationProtocolOverride: `
nvmeofTcp:
  maxQueueSize: 128
`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramNVMeOFMaxQueueSize, "128")
	assertNoKey(t, result, paramNVMeOFCtrlLossTmo)
	assertNoKey(t, result, paramNVMeOFReconnectDelay)
}

func TestParsePVCAnnotations_ProtocolOverride_ISCSI(t *testing.T) {
	ann := map[string]string{
		AnnotationProtocolOverride: `
iscsi:
  loginTimeout: 30
  replacementTimeout: 240
  nodeSessionTimeout: 180
`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramISCSILoginTimeout, "30")
	assertParam(t, result, paramISCSIReplacementTimeout, "240")
	assertParam(t, result, paramISCSINodeSessionTimeout, "180")
}

// ── Blocked structural fields in protocol-override ───────────────────────.

func TestParsePVCAnnotations_ProtocolOverride_NVMeOF_BlockedPort(t *testing.T) {
	ann := map[string]string{
		AnnotationProtocolOverride: `
nvmeofTcp:
  port: 9999
  maxQueueSize: 64
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for blocked nvmeofTcp.port field")
	}
}

func TestParsePVCAnnotations_ProtocolOverride_ISCSI_BlockedPort(t *testing.T) {
	ann := map[string]string{
		AnnotationProtocolOverride: `
iscsi:
  port: 12345
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for blocked iscsi.port field")
	}
}

func TestParsePVCAnnotations_ProtocolOverride_NVMeOF_NotAMap(t *testing.T) {
	ann := map[string]string{
		AnnotationProtocolOverride: "nvmeofTcp: just-a-string",
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when nvmeofTcp is not a map")
	}
}

func TestParsePVCAnnotations_ProtocolOverride_ISCSI_NotAMap(t *testing.T) {
	ann := map[string]string{
		AnnotationProtocolOverride: "iscsi: 42",
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when iscsi is not a map")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fs-override annotation
// ─────────────────────────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_FSOverride_XFS(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `
fsType: xfs
mkfsOptions: ["-K"]
`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramFSType, "xfs")

	// mkfsOptions should be JSON-encoded
	var opts []string
	if err := json.Unmarshal([]byte(result[paramMkfsOptions]), &opts); err != nil {
		t.Fatalf("mkfsOptions: invalid JSON %q: %v", result[paramMkfsOptions], err)
	}
	if len(opts) != 1 || opts[0] != "-K" {
		t.Errorf("mkfsOptions: got %v, want [\"-K\"]", opts)
	}
}

func TestParsePVCAnnotations_FSOverride_Ext4(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `fsType: ext4`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramFSType, "ext4")
	assertNoKey(t, result, paramMkfsOptions)
}

func TestParsePVCAnnotations_FSOverride_MkfsOptionsMultiple(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `
fsType: ext4
mkfsOptions:
  - "-E"
  - "lazy_itable_init=0"
  - "-b"
  - "4096"
`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramFSType, "ext4")

	var opts []string
	if err := json.Unmarshal([]byte(result[paramMkfsOptions]), &opts); err != nil {
		t.Fatalf("mkfsOptions: invalid JSON: %v", err)
	}
	want := []string{"-E", "lazy_itable_init=0", "-b", "4096"}
	if len(opts) != len(want) {
		t.Fatalf("mkfsOptions: got %v, want %v", opts, want)
	}
	for i, v := range want {
		if opts[i] != v {
			t.Errorf("mkfsOptions[%d]: got %q, want %q", i, opts[i], v)
		}
	}
}

func TestParsePVCAnnotations_FSOverride_FsTypeOnly(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `fsType: xfs`,
	}
	result := mustParseAnnotations(t, ann)

	assertParam(t, result, paramFSType, "xfs")
	assertNoKey(t, result, paramMkfsOptions)
}

// ── Validation errors for fs-override ────────────────────────────────────.

func TestParsePVCAnnotations_FSOverride_InvalidFsType(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `fsType: btrfs`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error for unsupported fsType")
	}
}

func TestParsePVCAnnotations_FSOverride_FsTypeNotString(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `fsType: 42`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when fsType is not a string")
	}
}

func TestParsePVCAnnotations_FSOverride_MkfsOptionsNotAList(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `
fsType: ext4
mkfsOptions: not-a-list
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when mkfsOptions is not a list")
	}
}

func TestParsePVCAnnotations_FSOverride_MkfsOptionsNonStringElement(t *testing.T) {
	ann := map[string]string{
		AnnotationFSOverride: `
fsType: ext4
mkfsOptions: [42, 99]
`,
	}
	_, err := ParsePVCAnnotations(ann)
	if err == nil {
		t.Fatal("expected error when mkfsOptions element is not a string")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Combined annotations — all three + flat prefix
// ─────────────────────────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_AllAnnotations(t *testing.T) {
	ann := map[string]string{
		// Flat prefix (low-level)
		"pillar-csi.bhyoo.com/param.zfs-prop.atime": "off",

		// Structured backend override
		AnnotationBackendOverride: `
zfs:
  properties:
    compression: zstd
    volblocksize: "16K"
`,
		// Structured protocol override
		AnnotationProtocolOverride: `
nvmeofTcp:
  maxQueueSize: 256
  ctrlLossTmo: 300
`,
		// Structured FS override
		AnnotationFSOverride: `
fsType: xfs
mkfsOptions: ["-K"]
`,
	}

	result := mustParseAnnotations(t, ann)

	// Flat prefix
	assertParam(t, result, "zfs-prop.atime", "off")

	// Backend overrides
	assertParam(t, result, paramZFSPropPrefix+"compression", "zstd")
	assertParam(t, result, paramZFSPropPrefix+"volblocksize", "16K")

	// Protocol overrides
	assertParam(t, result, paramNVMeOFMaxQueueSize, "256")
	assertParam(t, result, paramNVMeOFCtrlLossTmo, "300")
	assertNoKey(t, result, paramNVMeOFReconnectDelay)

	// FS overrides
	assertParam(t, result, paramFSType, "xfs")
	var opts []string
	if err := json.Unmarshal([]byte(result[paramMkfsOptions]), &opts); err != nil {
		t.Fatalf("mkfsOptions: invalid JSON: %v", err)
	}
	if len(opts) != 1 || opts[0] != "-K" {
		t.Errorf("mkfsOptions: got %v, want [\"-K\"]", opts)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Higher-priority structured annotation overwrites flat prefix
// ─────────────────────────────────────────────────────────────────────────────.

func TestParsePVCAnnotations_StructuredOverwritesFlat(t *testing.T) {
	// The structured backend-override is processed after the flat param.* loop,
	// so it wins when both set the same key.
	ann := map[string]string{
		// Flat prefix sets compression=lz4
		"pillar-csi.bhyoo.com/param." + paramZFSPropPrefix + "compression": "lz4",
		// Structured sets compression=zstd — should win
		AnnotationBackendOverride: `
zfs:
  properties:
    compression: zstd
`,
	}
	result := mustParseAnnotations(t, ann)
	assertParam(t, result, paramZFSPropPrefix+"compression", "zstd")
}

// ─────────────────────────────────────────────────────────────────────────────
// Annotation constant values (regression: do not rename unexpectedly)
// ─────────────────────────────────────────────────────────────────────────────.

func TestAnnotationConstants(t *testing.T) {
	if AnnotationBackendOverride != "pillar-csi.bhyoo.com/backend-override" {
		t.Errorf("AnnotationBackendOverride = %q", AnnotationBackendOverride)
	}
	if AnnotationProtocolOverride != "pillar-csi.bhyoo.com/protocol-override" {
		t.Errorf("AnnotationProtocolOverride = %q", AnnotationProtocolOverride)
	}
	if AnnotationFSOverride != "pillar-csi.bhyoo.com/fs-override" {
		t.Errorf("AnnotationFSOverride = %q", AnnotationFSOverride)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Param key constant values (regression: do not rename unexpectedly)
// ─────────────────────────────────────────────────────────────────────────────.

func TestParamKeyConstants(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"paramNVMeOFMaxQueueSize", paramNVMeOFMaxQueueSize, "pillar-csi.bhyoo.com/nvmeof-max-queue-size"},
		{"paramNVMeOFCtrlLossTmo", paramNVMeOFCtrlLossTmo, "pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo"},
		{"paramNVMeOFReconnectDelay", paramNVMeOFReconnectDelay, "pillar-csi.bhyoo.com/nvmeof-reconnect-delay"},
		{"paramISCSILoginTimeout", paramISCSILoginTimeout, "pillar-csi.bhyoo.com/iscsi-login-timeout"},
		{"paramISCSIReplacementTimeout", paramISCSIReplacementTimeout, "pillar-csi.bhyoo.com/iscsi-replacement-timeout"},
		{"paramISCSINodeSessionTimeout", paramISCSINodeSessionTimeout, "pillar-csi.bhyoo.com/iscsi-node-session-timeout"},
		{"paramFSType", paramFSType, "pillar-csi.bhyoo.com/fs-type"},
		{"paramMkfsOptions", paramMkfsOptions, "pillar-csi.bhyoo.com/mkfs-options"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
