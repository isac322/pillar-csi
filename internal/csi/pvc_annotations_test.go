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

// Tests for decodePVCAnnotations: the per-volume YAML documents
// pillar-csi.bhyoo.com/{backend,protocol,filesystem} and the rejection of
// every other pillar-csi.bhyoo.com/ annotation on a claim.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestDecodePVCAnnotations

import (
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// mustDecodeAnnotations calls decodePVCAnnotations and fails on error.
func mustDecodeAnnotations(t *testing.T, ann map[string]string) pvcDocs {
	t.Helper()
	docs, err := decodePVCAnnotations(ann)
	if err != nil {
		t.Fatalf("decodePVCAnnotations: unexpected error: %v", err)
	}
	return docs
}

// requireDecodeError asserts that decoding fails and the error message
// contains every fragment.
func requireDecodeError(t *testing.T, ann map[string]string, fragments ...string) {
	t.Helper()
	_, err := decodePVCAnnotations(ann)
	if err == nil {
		t.Fatalf("decodePVCAnnotations(%v): expected error, got nil", ann)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not contain %q", err.Error(), f)
		}
	}
}

func testInt32(v int32) *int32 { return new(v) }

func TestDecodePVCAnnotations_AbsentDocuments(t *testing.T) {
	t.Parallel()
	for name, ann := range map[string]map[string]string{
		"nil":   nil,
		"empty": {},
		"unrelated": {
			"kubectl.kubernetes.io/last-applied-configuration": "{}",
			"volume.kubernetes.io/storage-provisioner":         "pillar-csi.bhyoo.com",
		},
		"blank documents": {
			v1alpha1.AnnotationBackendDoc:    "",
			v1alpha1.AnnotationProtocolDoc:   "  \n",
			v1alpha1.AnnotationFilesystemDoc: "",
		},
	} {
		docs := mustDecodeAnnotations(t, ann)
		if docs.Backend != nil || docs.Protocol != nil || docs.Filesystem != nil {
			t.Errorf("%s: docs = %+v, want all nil (absent document is not an override)", name, docs)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend document
// ─────────────────────────────────────────────────────────────────────────────.

func TestDecodePVCAnnotations_BackendZFSProperties(t *testing.T) {
	t.Parallel()
	docs := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationBackendDoc: "zfs:\n  properties:\n    compression: zstd\n    volblocksize: 16K\n",
	})
	if docs.Backend == nil || docs.Backend.ZFS == nil || docs.Backend.LVM != nil {
		t.Fatalf("Backend = %+v, want only the zfs member", docs.Backend)
	}
	want := map[string]string{"compression": "zstd", "volblocksize": "16K"}
	if got := docs.Backend.ZFS.Properties; len(got) != len(want) ||
		got["compression"] != want["compression"] || got["volblocksize"] != want["volblocksize"] {
		t.Errorf("zfs.properties = %v, want %v", got, want)
	}
}

func TestDecodePVCAnnotations_BackendLVMProvisioningMode(t *testing.T) {
	t.Parallel()
	docs := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationBackendDoc: "lvm:\n  provisioningMode: thin\n",
	})
	if docs.Backend == nil || docs.Backend.LVM == nil || docs.Backend.ZFS != nil {
		t.Fatalf("Backend = %+v, want only the lvm member", docs.Backend)
	}
	if got := docs.Backend.LVM.ProvisioningMode; got != v1alpha1.LVMProvisioningModeThin {
		t.Errorf("lvm.provisioningMode = %q, want thin", got)
	}
}

// Structural placement fields identify the pool; a claim can never move
// its volume by naming another one.
func TestDecodePVCAnnotations_BackendStructuralFieldsRejected(t *testing.T) {
	t.Parallel()
	for doc, path := range map[string]string{
		"zfs:\n  pool: evil-pool\n":       "zfs.pool",
		"zfs:\n  parentDataset: other\n":  "zfs.parentDataset",
		"zfs:\n  volumeType: zvol\n":      "zfs.volumeType",
		"lvm:\n  volumeGroup: other-vg\n": "lvm.volumeGroup",
		"lvm:\n  thinPool: other-thin\n":  "lvm.thinPool",
	} {
		requireDecodeError(t, map[string]string{v1alpha1.AnnotationBackendDoc: doc},
			v1alpha1.AnnotationBackendDoc+": "+path+" is structural and cannot be set per volume")
	}
}

func TestDecodePVCAnnotations_BackendInvalidDocuments(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc       string
		fragments []string
	}{
		"malformed YAML":       {"this: is: invalid: yaml: [", []string{"invalid YAML"}},
		"not a mapping":        {"- zfs", []string{"expected a YAML mapping"}},
		"unknown member":       {"dir:\n  path: /x\n", []string{`unknown field "dir"`}},
		"zfs-dataset member":   {"zfs-dataset:\n  properties: {}\n", []string{`unknown field "zfs-dataset"`}},
		"two members":          {"zfs:\n  properties: {a: b}\nlvm:\n  provisioningMode: thin\n", []string{"exactly one of"}},
		"zfs member scalar":    {"zfs: just-a-string", []string{"zfs"}},
		"unknown zfs field":    {"zfs:\n  quota: 10G\n", []string{"unknown field zfs.quota"}},
		"properties not a map": {"zfs:\n  properties: not-a-map\n", []string{"zfs.properties"}},
		"invalid lvm mode":     {"lvm:\n  provisioningMode: striped\n", []string{"lvm.provisioningMode"}},
		"legacy type field":    {"type: zfs-zvol\n", []string{`unknown field "type"`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireDecodeError(t, map[string]string{v1alpha1.AnnotationBackendDoc: tc.doc},
				append([]string{v1alpha1.AnnotationBackendDoc}, tc.fragments...)...)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Protocol document
// ─────────────────────────────────────────────────────────────────────────────.

func TestDecodePVCAnnotations_ProtocolNVMeOFTunables(t *testing.T) {
	t.Parallel()
	docs := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  maxQueueSize: 64\n  inCapsuleDataSize: 16384\n" +
			"  ctrlLossTmo: 0\n  reconnectDelay: 5\n",
	})
	if docs.Protocol == nil || docs.Protocol.NVMeOFTCP == nil {
		t.Fatalf("Protocol = %+v, want nvmeofTcp member", docs.Protocol)
	}
	got := docs.Protocol.NVMeOFTCP
	for name, pair := range map[string][2]*int32{
		"maxQueueSize":      {got.MaxQueueSize, testInt32(64)},
		"inCapsuleDataSize": {got.InCapsuleDataSize, testInt32(16384)},
		"ctrlLossTmo":       {got.CtrlLossTmo, testInt32(0)}, // explicit zero preserved
		"reconnectDelay":    {got.ReconnectDelay, testInt32(5)},
	} {
		if pair[0] == nil || *pair[0] != *pair[1] {
			t.Errorf("nvmeofTcp.%s = %v, want %d", name, pair[0], *pair[1])
		}
	}
}

// Fields the document does not mention stay nil so lower layers apply.
func TestDecodePVCAnnotations_ProtocolPartialLeavesOthersUnset(t *testing.T) {
	t.Parallel()
	docs := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  maxQueueSize: 128\n",
	})
	o := docs.Protocol.NVMeOFTCP
	if o.MaxQueueSize == nil || *o.MaxQueueSize != 128 {
		t.Errorf("maxQueueSize = %v, want 128", o.MaxQueueSize)
	}
	if o.InCapsuleDataSize != nil || o.CtrlLossTmo != nil || o.ReconnectDelay != nil {
		t.Errorf("unmentioned fields must stay nil: %+v", o)
	}
}

// An explicit YAML null means "omitted": the field inherits from the layer
// below instead of failing validation or clearing it.
func TestDecodePVCAnnotations_NullMeansOmitted(t *testing.T) {
	t.Parallel()
	docs := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationBackendDoc:    "lvm:\n  provisioningMode: null\n",
		v1alpha1.AnnotationProtocolDoc:   "nvmeofTcp:\n  maxQueueSize: null\n  ctrlLossTmo: 30\n",
		v1alpha1.AnnotationFilesystemDoc: "fsType: null\nmkfsOptions: null\nmountOptions: [noatime]\n",
	})
	if docs.Backend == nil || docs.Backend.LVM == nil || docs.Backend.LVM.ProvisioningMode != "" {
		t.Errorf("Backend = %+v, want lvm member with provisioningMode unset", docs.Backend)
	}
	if p := docs.Protocol.NVMeOFTCP; p.MaxQueueSize != nil || p.CtrlLossTmo == nil || *p.CtrlLossTmo != 30 {
		t.Errorf("nvmeofTcp = %+v, want maxQueueSize nil and ctrlLossTmo 30", p)
	}
	fs := docs.Filesystem
	if fs.FSType != "" || fs.MkfsOptions != nil {
		t.Errorf("filesystem = %+v, want fsType and mkfsOptions inherited (unset)", fs)
	}
	if fs.MountOptions == nil || !slices.Equal(*fs.MountOptions, []string{"noatime"}) {
		t.Errorf("mountOptions = %v, want [noatime]", fs.MountOptions)
	}
}

func TestDecodePVCAnnotations_ProtocolStructuralFieldsRejected(t *testing.T) {
	t.Parallel()
	for doc, path := range map[string]string{
		"nvmeofTcp:\n  acl: false\n": "nvmeofTcp.acl",
		"nvmeofTcp:\n  port: 4421\n": "nvmeofTcp.port",
		"iscsi:\n  acl: true\n":      "iscsi.acl",
		"iscsi:\n  port: 3261\n":     "iscsi.port",
	} {
		requireDecodeError(t, map[string]string{v1alpha1.AnnotationProtocolDoc: doc},
			v1alpha1.AnnotationProtocolDoc+": "+path+" is structural and cannot be set per volume")
	}
}

func TestDecodePVCAnnotations_NFSStructuralFieldsRejected(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		"nfs:\n  version: \"4.2\"\n",
		"nfs:\n  port: 2049\n",
		"nfs:\n  acl: true\n",
		"nfs:\n  squash: root\n",
	} {
		_, err := decodePVCAnnotations(map[string]string{v1alpha1.AnnotationProtocolDoc: doc})
		if err == nil {
			t.Fatalf("decodePVCAnnotations(%q): expected structural-field error", doc)
		}
	}
}

func TestDecodePVCAnnotations_ProtocolInvalidDocuments(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc       string
		fragments []string
	}{
		"smb member":             {"smb:\n  share: data\n", []string{`unknown field "smb"`}},
		"iscsi login timeout 0":  {"iscsi:\n  loginTimeout: 0\n", []string{"iscsi.loginTimeout"}},
		"member scalar":          {"nvmeofTcp: just-a-string", []string{"nvmeofTcp"}},
		"unknown field":          {"nvmeofTcp:\n  keepAliveTmo: 5\n", []string{"unknown field nvmeofTcp.keepAliveTmo"}},
		"queue too small":        {"nvmeofTcp:\n  maxQueueSize: 8\n", []string{"nvmeofTcp.maxQueueSize"}},
		"queue too large":        {"nvmeofTcp:\n  maxQueueSize: 2048\n", []string{"nvmeofTcp.maxQueueSize"}},
		"in-capsule too small":   {"nvmeofTcp:\n  inCapsuleDataSize: 1023\n", []string{"nvmeofTcp.inCapsuleDataSize"}},
		"in-capsule with suffix": {"nvmeofTcp:\n  inCapsuleDataSize: 16K\n", []string{"nvmeofTcp.inCapsuleDataSize"}},
		"negative ctrl loss tmo": {"nvmeofTcp:\n  ctrlLossTmo: -1\n", []string{"nvmeofTcp.ctrlLossTmo"}},
		"duration reconnect":     {"nvmeofTcp:\n  reconnectDelay: 10m\n", []string{"nvmeofTcp.reconnectDelay"}},
		"legacy protocol-type":   {"type: nvmeof-tcp\n", []string{`unknown field "type"`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireDecodeError(t, map[string]string{v1alpha1.AnnotationProtocolDoc: tc.doc},
				append([]string{v1alpha1.AnnotationProtocolDoc}, tc.fragments...)...)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Filesystem document
// ─────────────────────────────────────────────────────────────────────────────.

func TestDecodePVCAnnotations_Filesystem(t *testing.T) {
	t.Parallel()
	docs := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationFilesystemDoc: "fsType: xfs\nmkfsOptions: [\"-K\"]\nmountOptions: [noatime, nodiscard]\n",
	})
	fs := docs.Filesystem
	if fs == nil || fs.FSType != "xfs" {
		t.Fatalf("Filesystem = %+v, want fsType xfs", fs)
	}
	if fs.MkfsOptions == nil || !slices.Equal(*fs.MkfsOptions, []string{"-K"}) {
		t.Errorf("mkfsOptions = %v, want [-K]", fs.MkfsOptions)
	}
	if fs.MountOptions == nil || !slices.Equal(*fs.MountOptions, []string{"noatime", "nodiscard"}) {
		t.Errorf("mountOptions = %v, want [noatime nodiscard]", fs.MountOptions)
	}
}

// List semantics: an omitted list inherits (nil), an explicit [] clears
// (non-nil, empty).
func TestDecodePVCAnnotations_FilesystemListSemantics(t *testing.T) {
	t.Parallel()
	inherit := mustDecodeAnnotations(t, map[string]string{v1alpha1.AnnotationFilesystemDoc: "fsType: ext4\n"})
	if inherit.Filesystem.MkfsOptions != nil || inherit.Filesystem.MountOptions != nil {
		t.Errorf("omitted lists must decode as nil (inherit): %+v", inherit.Filesystem)
	}

	cleared := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationFilesystemDoc: "mkfsOptions: []\nmountOptions: []\n",
	})
	fs := cleared.Filesystem
	if fs.MkfsOptions == nil || len(*fs.MkfsOptions) != 0 {
		t.Errorf("mkfsOptions: [] must decode as an explicit empty list, got %v", fs.MkfsOptions)
	}
	if fs.MountOptions == nil || len(*fs.MountOptions) != 0 {
		t.Errorf("mountOptions: [] must decode as an explicit empty list, got %v", fs.MountOptions)
	}
	if fs.FSType != "" {
		t.Errorf("fsType = %q, want empty (inherit)", fs.FSType)
	}
}

func TestDecodePVCAnnotations_FilesystemInvalidDocuments(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc       string
		fragments []string
	}{
		"unsupported fsType":     {"fsType: btrfs\n", []string{"fsType"}},
		"fsType not a string":    {"fsType: 42\n", []string{"fsType"}},
		"mkfsOptions not a list": {"mkfsOptions: not-a-list\n", []string{"mkfsOptions"}},
		"non-string element":     {"mkfsOptions: [42, 99]\n", []string{"mkfsOptions"}},
		"unknown field":          {"fsTyp: xfs\n", []string{"unknown field fsTyp"}},
		"not a mapping":          {"xfs", []string{"expected a YAML mapping"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireDecodeError(t, map[string]string{v1alpha1.AnnotationFilesystemDoc: tc.doc},
				append([]string{v1alpha1.AnnotationFilesystemDoc}, tc.fragments...)...)
		})
	}
}

func TestDecodePVCAnnotations_AllDocuments(t *testing.T) {
	t.Parallel()
	docs := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationBackendDoc:    "zfs:\n  properties: {atime: \"off\"}\n",
		v1alpha1.AnnotationProtocolDoc:   "nvmeofTcp:\n  ctrlLossTmo: 900\n",
		v1alpha1.AnnotationFilesystemDoc: "fsType: xfs\n",
		"unrelated.example.com/key":      "ignored",
	})
	if docs.Backend == nil || docs.Backend.ZFS.Properties["atime"] != "off" {
		t.Errorf("Backend = %+v", docs.Backend)
	}
	if docs.Protocol == nil || docs.Protocol.NVMeOFTCP.CtrlLossTmo == nil || *docs.Protocol.NVMeOFTCP.CtrlLossTmo != 900 {
		t.Errorf("Protocol = %+v", docs.Protocol)
	}
	if docs.Filesystem == nil || docs.Filesystem.FSType != "xfs" {
		t.Errorf("Filesystem = %+v", docs.Filesystem)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Removed vocabulary and the strict pillar-csi.bhyoo.com/ domain
// ─────────────────────────────────────────────────────────────────────────────.

// The flat param.<name> path is gone; the error points at the documents.
func TestDecodePVCAnnotations_FlatParamRejected(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"pillar-csi.bhyoo.com/param.zfs-prop.compression",
		"pillar-csi.bhyoo.com/param.lvm-mode",
		"pillar-csi.bhyoo.com/param.nvmeof-ctrl-loss-tmo",
		"pillar-csi.bhyoo.com/param.store",
		"pillar-csi.bhyoo.com/param.acl-enabled",
	} {
		requireDecodeError(t, map[string]string{key: "x"}, key, v1alpha1.AnnotationBackendDoc)
	}
}

// The *-override annotations were renamed; they are rejected with the
// replacement key rather than silently ignored.
func TestDecodePVCAnnotations_OldOverrideKeysRejected(t *testing.T) {
	t.Parallel()
	for old, replacement := range map[string]string{
		"pillar-csi.bhyoo.com/backend-override":  v1alpha1.AnnotationBackendDoc,
		"pillar-csi.bhyoo.com/protocol-override": v1alpha1.AnnotationProtocolDoc,
		"pillar-csi.bhyoo.com/fs-override":       v1alpha1.AnnotationFilesystemDoc,
	} {
		requireDecodeError(t, map[string]string{old: "zfs:\n  properties: {a: b}\n"}, old, replacement)
	}
}

// Any other pillar-csi.bhyoo.com/ key on a claim is unknown: a typo must
// not silently provision with defaults.
func TestDecodePVCAnnotations_UnknownPillarKeyRejected(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"pillar-csi.bhyoo.com/filesytem",
		"pillar-csi.bhyoo.com/zfs-prop.compression",
		"pillar-csi.bhyoo.com/store-ref",
		"pillar-csi.bhyoo.com/storage-class",
	} {
		requireDecodeError(t, map[string]string{key: "x"}, key)
	}
}

func TestDecodePVCAnnotations_FilesystemSelectorsRejectEmpty(t *testing.T) {
	t.Parallel()
	for _, key := range []string{v1alpha1.AnnotationImportDirectory, v1alpha1.AnnotationImportZFSDataset,
		v1alpha1.AnnotationImportZvol} {
		t.Run(key, func(t *testing.T) {
			requireDecodeError(t, map[string]string{key: " \n\t "}, key, "empty")
		})
	}
}

func TestDecodePVCAnnotations_FilesystemSelectorValues(t *testing.T) {
	t.Parallel()
	directory := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationImportDirectory: " /srv/imports/app "})
	if directory.ImportDirectory != "/srv/imports/app" || directory.ImportZFSDataset != "" || directory.ImportZvol != "" {
		t.Fatalf("directory intent changed: %+v", directory)
	}
	dataset := mustDecodeAnnotations(t, map[string]string{
		v1alpha1.AnnotationImportZFSDataset: " tank/existing "})
	if dataset.ImportZFSDataset != "tank/existing" || dataset.ImportDirectory != "" || dataset.ImportZvol != "" {
		t.Fatalf("dataset intent changed: %+v", dataset)
	}
}
