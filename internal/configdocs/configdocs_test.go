package configdocs

import (
	"strings"
	"testing"

	pillarv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// TestDecodeOverrides_Rejections pins the strict-decoding contract shared by
// PVC annotations and StorageClass parameter documents: every rejection
// names the source and the full path of the offending field.
func TestDecodeOverrides_Rejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		decode  func(raw string) error
		raw     string
		wantErr string
	}{
		{
			name:    "protocol structural acl",
			decode:  protocolOverride,
			raw:     "nvmeofTcp: {acl: true}",
			wantErr: "pillar-csi.bhyoo.com/protocol: nvmeofTcp.acl is structural and cannot be set per volume",
		},
		{
			name:    "protocol structural port",
			decode:  protocolOverride,
			raw:     "nvmeofTcp: {port: 4421}",
			wantErr: "pillar-csi.bhyoo.com/protocol: nvmeofTcp.port is structural and cannot be set per volume",
		},
		{
			name:    "iscsi structural acl",
			decode:  protocolOverride,
			raw:     "iscsi: {acl: true}",
			wantErr: "pillar-csi.bhyoo.com/protocol: iscsi.acl is structural and cannot be set per volume",
		},
		{
			name:    "iscsi structural port",
			decode:  protocolOverride,
			raw:     "iscsi: {port: 3261}",
			wantErr: "pillar-csi.bhyoo.com/protocol: iscsi.port is structural and cannot be set per volume",
		},
		{
			name:    "iscsi login timeout must be positive",
			decode:  protocolOverride,
			raw:     "iscsi: {loginTimeout: 0}",
			wantErr: "iscsi.loginTimeout: 0 is out of range [1, 2147483647]",
		},
		{
			name:    "protocol union with two members",
			decode:  protocolOverride,
			raw:     "nvmeofTcp: {maxQueueSize: 64}\niscsi: {loginTimeout: 30}",
			wantErr: "pillar-csi.bhyoo.com/protocol: exactly one of iscsi or nvmeofTcp must be set (got iscsi, nvmeofTcp)",
		},
		{
			name:    "backend structural zfs.pool",
			decode:  backendOverride,
			raw:     "zfs: {pool: other}",
			wantErr: "pillar-csi.bhyoo.com/backend: zfs.pool is structural and cannot be set per volume",
		},
		{
			name:    "backend structural lvm.thinPool",
			decode:  backendOverride,
			raw:     "lvm: {thinPool: t}",
			wantErr: "pillar-csi.bhyoo.com/backend: lvm.thinPool is structural and cannot be set per volume",
		},
		{
			name:    "unknown nested field carries its path",
			decode:  protocolOverride,
			raw:     "nvmeofTcp: {queueDepth: 64}",
			wantErr: "pillar-csi.bhyoo.com/protocol: unknown field nvmeofTcp.queueDepth",
		},
		{
			name:    "unsupported protocol variant",
			decode:  protocolOverride,
			raw:     "nfs: {version: 4}",
			wantErr: `pillar-csi.bhyoo.com/protocol: unknown field "nfs" (supported: iscsi or nvmeofTcp)`,
		},
		{
			name:    "removed discriminator field",
			decode:  backendOverride,
			raw:     "type: zfs-zvol\nzfs: {properties: {a: b}}",
			wantErr: `pillar-csi.bhyoo.com/backend: unknown field "type" (supported: lvm or zfs)`,
		},
		{
			name:    "union with two members",
			decode:  backendOverride,
			raw:     "zfs: {properties: {a: b}}\nlvm: {provisioningMode: thin}",
			wantErr: "pillar-csi.bhyoo.com/backend: exactly one of lvm or zfs must be set (got lvm, zfs)",
		},
		{
			name:    "member value must be a mapping",
			decode:  backendOverride,
			raw:     "zfs: null",
			wantErr: "pillar-csi.bhyoo.com/backend: zfs must be a mapping of its fields",
		},
		{
			name:    "range below kernel minimum",
			decode:  protocolOverride,
			raw:     "nvmeofTcp: {maxQueueSize: 8}",
			wantErr: "nvmeofTcp.maxQueueSize: 8 is out of range [16, 1024]",
		},
		{
			name:    "negative timeout rejected at every layer",
			decode:  protocolOverride,
			raw:     "nvmeofTcp: {ctrlLossTmo: -1}",
			wantErr: "nvmeofTcp.ctrlLossTmo: -1 is out of range [0, 2147483647]",
		},
		{
			name:    "in-capsule size below connect data",
			decode:  protocolOverride,
			raw:     "nvmeofTcp: {inCapsuleDataSize: 512}",
			wantErr: "nvmeofTcp.inCapsuleDataSize: 512 is out of range [1024, 2147483647]",
		},
		{
			name:    "type mismatch",
			decode:  protocolOverride,
			raw:     `nvmeofTcp: {maxQueueSize: "64"}`,
			wantErr: `nvmeofTcp.maxQueueSize: expected an integer, got string "64"`,
		},
		{
			name:    "enum provisioningMode",
			decode:  backendOverride,
			raw:     "lvm: {provisioningMode: striped}",
			wantErr: `lvm.provisioningMode: unsupported value "striped" (supported: linear, thin)`,
		},
		{
			name:    "zfs property values are strings",
			decode:  backendOverride,
			raw:     "zfs: {properties: {copies: 2}}",
			wantErr: "zfs.properties.copies: expected a string value, got number 2",
		},
		{
			name:    "filesystem unknown field",
			decode:  filesystemDoc,
			raw:     "fsType: xfs\nmkfsOpts: [-K]",
			wantErr: "pillar-csi.bhyoo.com/filesystem: unknown field mkfsOpts",
		},
		{
			name:    "filesystem fsType enum",
			decode:  filesystemDoc,
			raw:     "fsType: btrfs",
			wantErr: `fsType: unsupported value "btrfs" (supported: ext4, xfs)`,
		},
		{
			name:    "filesystem list items are strings",
			decode:  filesystemDoc,
			raw:     "mountOptions: [noatime, 5]",
			wantErr: "mountOptions[1]: expected a string, got number 5",
		},
		{
			name:    "filesystem periodicTrim is a boolean",
			decode:  filesystemDoc,
			raw:     `periodicTrim: "false"`,
			wantErr: "periodicTrim: expected a boolean, got string",
		},
		{
			name:    "not a mapping",
			decode:  filesystemDoc,
			raw:     "- xfs",
			wantErr: "pillar-csi.bhyoo.com/filesystem: expected a YAML mapping, got a list",
		},
		{
			name:    "duplicate member key is not silently merged",
			decode:  backendOverride,
			raw:     "zfs: {properties: {compression: lz4}}\nzfs: {properties: {copies: \"2\"}}",
			wantErr: `line 2: mapping key "zfs" already defined at line 1`,
		},
		{
			name:    "duplicate nested key keeps neither value",
			decode:  protocolOverride,
			raw:     "nvmeofTcp:\n  maxQueueSize: 64\n  maxQueueSize: 128",
			wantErr: `mapping key "maxQueueSize" already defined`,
		},
		{
			name:    "duplicate filesystem key",
			decode:  filesystemDoc,
			raw:     "fsType: xfs\nfsType: ext4",
			wantErr: `mapping key "fsType" already defined`,
		},
		{
			name:    "date-shaped zfs property stays a string",
			decode:  backendOverride,
			raw:     "zfs: {properties: {\"org.example:date\": 2026-01-01, copies: 2}}",
			wantErr: "zfs.properties.copies: expected a string value, got number 2",
		},
		{
			name:    "placement string fields are typed",
			decode:  backendSpec,
			raw:     "zfs: {pool: tank, parentDataset: [k8s]}",
			wantErr: "zfs.parentDataset: expected a string, got a list",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.decode(tt.raw)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestDecodeOverrides_AcceptsTunables checks that the tunable subset decodes
// into the CRD override types unchanged, i.e. the per-volume document has
// the same shape as PillarStorageClass.spec.overrides.
func TestDecodeOverrides_AcceptsTunables(t *testing.T) {
	t.Parallel()

	b, err := DecodeBackendOverride(BackendDocKey, "zfs:\n  properties:\n    volblocksize: 128K\n")
	if err != nil {
		t.Fatalf("DecodeBackendOverride: %v", err)
	}
	if b.ZFS == nil || b.ZFS.Properties["volblocksize"] != "128K" || b.LVM != nil {
		t.Fatalf("backend = %+v, want zfs.properties.volblocksize=128K", b)
	}

	p, err := DecodeProtocolOverride(ProtocolDocKey,
		"nvmeofTcp: {maxQueueSize: 64, inCapsuleDataSize: 8192, ctrlLossTmo: 0, reconnectDelay: 5}")
	if err != nil {
		t.Fatalf("DecodeProtocolOverride: %v", err)
	}
	n := p.NVMeOFTCP
	if n == nil || *n.MaxQueueSize != 64 || *n.InCapsuleDataSize != 8192 || *n.CtrlLossTmo != 0 || *n.ReconnectDelay != 5 {
		t.Fatalf("protocol = %+v, want all four tunables set", n)
	}
}

// TestDecodeOverrides_AcceptsISCSITunables checks that the iSCSI tunables
// decode into the CRD override type unchanged.
func TestDecodeOverrides_AcceptsISCSITunables(t *testing.T) {
	t.Parallel()

	p, err := DecodeProtocolOverride(ProtocolDocKey,
		"iscsi: {loginTimeout: 30, replacementTimeout: 0, noopOutInterval: 10, noopOutTimeout: 7}")
	if err != nil {
		t.Fatalf("DecodeProtocolOverride(iscsi): %v", err)
	}
	i := p.ISCSI
	if p.NVMeOFTCP != nil || i == nil || *i.LoginTimeout != 30 || *i.ReplacementTimeout != 0 ||
		*i.NoopOutInterval != 10 || *i.NoopOutTimeout != 7 {
		t.Fatalf("protocol = %+v, want iscsi with all four tunables set", p)
	}
}

// TestDecodeOverrides_AbsentDocument checks that an empty, blank or null
// document decodes to no override.
func TestDecodeOverrides_AbsentDocument(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "   \n", "null"} {
		b, err := DecodeBackendOverride(BackendDocKey, raw)
		if err != nil || b != nil {
			t.Fatalf("DecodeBackendOverride(%q) = %v, %v; want nil, nil (absent document)", raw, b, err)
		}
	}
}

// TestDecodeFilesystemDoc_ListSemantics pins the list rule shared by every
// layer: an omitted list inherits (nil), an explicit [] clears (non-nil,
// empty).
func TestDecodeFilesystemDoc_ListSemantics(t *testing.T) {
	t.Parallel()

	fs, err := DecodeFilesystemDoc(FilesystemDocKey, "fsType: xfs\nmkfsOptions: []\n")
	if err != nil {
		t.Fatalf("DecodeFilesystemDoc: %v", err)
	}
	if fs.FSType != "xfs" {
		t.Errorf("fsType = %q, want xfs", fs.FSType)
	}
	if fs.MkfsOptions == nil || len(*fs.MkfsOptions) != 0 {
		t.Errorf("mkfsOptions = %v, want explicit empty list (clear)", fs.MkfsOptions)
	}
	if fs.MountOptions != nil {
		t.Errorf("mountOptions = %v, want nil (inherit)", *fs.MountOptions)
	}

	fs, err = DecodeFilesystemDoc(FilesystemDocKey, "mountOptions: null\nmkfsOptions: [-L, data]\n")
	if err != nil {
		t.Fatalf("DecodeFilesystemDoc: %v", err)
	}
	if fs.MountOptions != nil {
		t.Errorf("mountOptions: null = %v, want nil (null inherits like an omitted field)", *fs.MountOptions)
	}
	if fs.MkfsOptions == nil || strings.Join(*fs.MkfsOptions, " ") != "-L data" {
		t.Errorf("mkfsOptions = %v, want [-L data]", fs.MkfsOptions)
	}
}

// TestDecodeFilesystemDoc_PeriodicTrim verifies periodicTrim decodes as a
// tri-state: omitted or null inherits (nil), false opts out explicitly.
func TestDecodeFilesystemDoc_PeriodicTrim(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]*bool{
		"periodicTrim: false\n": new(false),
		"periodicTrim: true\n":  new(true),
		"periodicTrim: null\n":  nil,
		"fsType: xfs\n":         nil,
	} {
		fs, err := DecodeFilesystemDoc(FilesystemDocKey, raw)
		if err != nil {
			t.Fatalf("DecodeFilesystemDoc(%q): %v", raw, err)
		}
		if (fs.PeriodicTrim == nil) != (want == nil) || (want != nil && *fs.PeriodicTrim != *want) {
			t.Errorf("DecodeFilesystemDoc(%q).PeriodicTrim = %v, want %v", raw, fs.PeriodicTrim, want)
		}
	}
}

// TestDecodeBackendSpec covers the full backend document used by the agent
// config file: structural fields are accepted, required fields enforced and
// the CRD defaults applied.
func TestDecodeBackendSpec(t *testing.T) {
	t.Parallel()

	zfs, err := DecodeBackendSpec("cfg: backends[0]", "zfs: {pool: hot-data, parentDataset: k8s}")
	if err != nil {
		t.Fatalf("zfs: %v", err)
	}
	if zfs.ZFS.Pool != "hot-data" || zfs.ZFS.ParentDataset != "k8s" ||
		zfs.ZFS.VolumeType != pillarv1alpha1.ZFSVolumeTypeZvol {
		t.Fatalf("zfs = %+v, want pool/parentDataset set and volumeType defaulted to zvol", zfs.ZFS)
	}

	lvm, err := DecodeBackendSpec("cfg: backends[1]", "lvm: {volumeGroup: data-vg, thinPool: thin0}")
	if err != nil {
		t.Fatalf("lvm: %v", err)
	}
	if lvm.LVM.ProvisioningMode != pillarv1alpha1.LVMProvisioningModeLinear {
		t.Fatalf("provisioningMode = %q, want the single default linear", lvm.LVM.ProvisioningMode)
	}

	for raw, want := range map[string]string{
		"zfs: {parentDataset: k8s}":        "cfg: backends[2]: zfs.pool is required",
		"lvm: {thinPool: thin0}":           "cfg: backends[2]: lvm.volumeGroup is required",
		"zfs: {pool: p, volumeType: file}": `zfs.volumeType: unsupported value "file" (supported: zvol)`,
		"dir: {path: /srv}":                `cfg: backends[2]: unknown field "dir" (supported: lvm or zfs)`,
		"{}":                               "cfg: backends[2]: exactly one of lvm or zfs must be set (got none)",
	} {
		_, err := DecodeBackendSpec("cfg: backends[2]", raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeBackendSpec(%q) error = %v, want %q", raw, err, want)
		}
	}
}

func backendOverride(raw string) error {
	_, err := DecodeBackendOverride(BackendDocKey, raw)
	return err
}

func protocolOverride(raw string) error {
	_, err := DecodeProtocolOverride(ProtocolDocKey, raw)
	return err
}

func backendSpec(raw string) error {
	_, err := DecodeBackendSpec("agent config", raw)
	return err
}

func filesystemDoc(raw string) error {
	_, err := DecodeFilesystemDoc(FilesystemDocKey, raw)
	return err
}
