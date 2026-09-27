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

// Tests for the filesystem settings path (issue #115): mkfs options from the
// PillarProtocol / PillarStorageClass / PVC fs-override layers and the PVC
// fsType override travel through the CreateVolume VolumeContext to
// NodeStageVolume, which formats a new volume with them; settings that
// cannot take effect are rejected instead of dropped.

import (
	"context"
	"maps"
	"slices"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mountCreateVolumeRequest is baseCreateVolumeRequest with a Filesystem
// (mount) capability of the given fsType instead of raw block.
func mountCreateVolumeRequest(req *csi.CreateVolumeRequest, fsType string) *csi.CreateVolumeRequest {
	req.VolumeCapabilities = []*csi.VolumeCapability{mountCap(fsType)}
	return req
}

// TestCreateVolume_FilesystemSettingsReachVolumeContext verifies that the
// class-level mkfs options reach the node through the VolumeContext, that a
// PVC fs-override wins over them and adds its fsType, and that an idempotent
// retry returns the same keys.
func TestCreateVolume_FilesystemSettingsReachVolumeContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("class mkfsOptions only", func(t *testing.T) {
		t.Parallel()
		env := newControllerTestEnv(t)
		req := mountCreateVolumeRequest(baseCreateVolumeRequest(), "ext4")
		req.Parameters[paramMkfsOptions] = `["-E","lazy_itable_init=0"]`

		for attempt := 1; attempt <= 2; attempt++ {
			resp, err := env.srv.CreateVolume(ctx, req)
			if err != nil {
				t.Fatalf("attempt %d: CreateVolume: %v", attempt, err)
			}
			vc := resp.GetVolume().GetVolumeContext()
			if got := vc[paramMkfsOptions]; got != `["-E","lazy_itable_init=0"]` {
				t.Errorf("attempt %d: VolumeContext mkfs-options = %q, want class value", attempt, got)
			}
			if _, ok := vc[paramFSType]; ok {
				t.Errorf("attempt %d: VolumeContext must not carry %q without a PVC override", attempt, paramFSType)
			}
		}
	})

	t.Run("PVC fs-override wins", func(t *testing.T) {
		t.Parallel()
		env, req := newControllerTestEnvWithPVC(t, "tenant-a", "pvc-xfs", map[string]string{
			AnnotationFSOverride: "fsType: xfs\nmkfsOptions: [\"-m\", \"reflink=1\"]\n",
		})
		req = mountCreateVolumeRequest(req, "ext4")
		req.Parameters[paramMkfsOptions] = `["-E","lazy_itable_init=0"]`

		resp, err := env.srv.CreateVolume(ctx, req)
		if err != nil {
			t.Fatalf("CreateVolume: %v", err)
		}
		vc := resp.GetVolume().GetVolumeContext()
		if got := vc[paramFSType]; got != xfsFsType {
			t.Errorf("VolumeContext fs-type = %q, want PVC override %q", got, xfsFsType)
		}
		if got := vc[paramMkfsOptions]; got != `["-m","reflink=1"]` {
			t.Errorf("VolumeContext mkfs-options = %q, want PVC override", got)
		}
	})
}

// TestCreateVolume_RejectsInapplicableFilesystemSettings verifies that a
// filesystem setting that is malformed, unsafe, or cannot apply to the
// requested volume fails CreateVolume with InvalidArgument before any agent
// call, instead of provisioning a volume that silently ignores it.
func TestCreateVolume_RejectsInapplicableFilesystemSettings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		fsType string // mount capability fsType; "block" selects raw block
		params map[string]string
		annot  string // PVC fs-override annotation
	}{
		{
			name:   "legacy space-joined mkfs-options",
			fsType: "ext4",
			params: map[string]string{paramMkfsOptions: "-E lazy_itable_init=0"},
		},
		{
			name:   "xfs external log device",
			fsType: "xfs",
			annot:  "mkfsOptions: [\"-l\", \"logdev=/dev/sda\"]\n",
		},
		{
			name:   "ext4 external journal by label",
			fsType: "ext4",
			annot:  "mkfsOptions: [\"-J\", \"device=LABEL=other-journal\"]\n",
		},
		{
			name:   "PVC fsType selects the allowlist",
			fsType: "ext4",
			annot:  "fsType: xfs\nmkfsOptions: [\"-E\", \"lazy_itable_init=0\"]\n",
		},
		{
			name:   "unsupported fs-type via flat param override",
			fsType: "ext4",
			params: map[string]string{paramFSType: "btrfs"},
		},
		{
			name:   "PVC fsType on raw block volume",
			fsType: "block",
			annot:  "fsType: xfs\n",
		},
		{
			name:   "PVC mkfsOptions on raw block volume",
			fsType: "block",
			annot:  "mkfsOptions: [\"-K\"]\n",
		},
		{
			name:   "filesystem settings on NFS",
			fsType: "ext4",
			params: map[string]string{paramProtocolType: "nfs", paramFSType: "xfs"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			annotations := map[string]string{}
			if tc.annot != "" {
				annotations[AnnotationFSOverride] = tc.annot
			}
			env, req := newControllerTestEnvWithPVC(t, "tenant-a", "pvc-fs", annotations)
			if tc.fsType != "block" {
				req = mountCreateVolumeRequest(req, tc.fsType)
			}
			maps.Copy(req.Parameters, tc.params)

			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("CreateVolume code = %v (err %v), want InvalidArgument", status.Code(err), err)
			}
			if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
				t.Fatalf("agent must not be called: create=%d export=%d",
					env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
			}
		})
	}
}

// TestCreateVolume_ClassMkfsOptionsIgnoredForBlockVolume verifies that the
// class-level mkfs options, which apply to the class's filesystem volumes,
// do not block provisioning a raw block volume from the same class.
func TestCreateVolume_ClassMkfsOptionsIgnoredForBlockVolume(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := baseCreateVolumeRequest() // raw block
	req.Parameters[paramMkfsOptions] = `["-E","lazy_itable_init=0"]`

	_, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
}

// TestNodeStageVolume_AppliesFilesystemSettings verifies that NodeStageVolume
// formats with the VolumeContext mkfs options, that the PVC fsType override
// wins over the capability fsType, that the formatted type is recorded in the
// stage state, and that NodeExpandVolume — which receives no VolumeContext —
// resizes with that recorded type rather than the capability's.
func TestNodeStageVolume_AppliesFilesystemSettings(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	resizer := &mockResizer{}
	env.srv.WithResizer(resizer)
	stagingPath := t.TempDir()
	const volumeID = "tank/pvc-xfs"

	volCtx := mountVolumeContext("nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-xfs", testStorageAddr)
	volCtx[paramFSType] = xfsFsType
	volCtx[paramMkfsOptions] = `["-m","reflink=1","-L","data vol"]`

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"), // PV csi.fsType from the StorageClass
		VolumeContext:     volCtx,
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if len(env.mounter.formatAndMountCalls) != 1 {
		t.Fatalf("FormatAndMount called %d times, want 1", len(env.mounter.formatAndMountCalls))
	}
	fm := env.mounter.formatAndMountCalls[0]
	if fm.fsType != xfsFsType {
		t.Errorf("FormatAndMount fsType = %q, want PVC override %q", fm.fsType, xfsFsType)
	}
	wantOpts := []string{"-m", "reflink=1", "-L", "data vol"}
	if !slices.Equal(fm.formatOptions, wantOpts) {
		t.Errorf("FormatAndMount formatOptions = %q, want %q", fm.formatOptions, wantOpts)
	}

	state, err := env.srv.readStageState(volumeID)
	if err != nil || state == nil {
		t.Fatalf("readStageState = (%v, %v), want a state", state, err)
	}
	if state.FsType != xfsFsType {
		t.Errorf("stage state FsType = %q, want %q", state.FsType, xfsFsType)
	}

	_, err = env.srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:         volumeID,
		VolumePath:       t.TempDir(),
		VolumeCapability: mountCap("ext4"),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if resizer.capturedFsType != xfsFsType {
		t.Errorf("ResizeFS fsType = %q, want staged type %q", resizer.capturedFsType, xfsFsType)
	}
}

// TestNodeStageVolume_ClassFsTypeWithoutOverride verifies that without a PVC
// override the capability fsType is used and no mkfs options are passed.
func TestNodeStageVolume_ClassFsTypeWithoutOverride(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-plain",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-plain", testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	fm := env.mounter.formatAndMountCalls[0]
	if fm.fsType != xfsFsType || len(fm.formatOptions) != 0 {
		t.Errorf("FormatAndMount (fsType, formatOptions) = (%q, %q), want (%q, none)",
			fm.fsType, fm.formatOptions, xfsFsType)
	}
}

// TestNodeStageVolume_InvalidFilesystemSettings_NoAttach verifies that a
// malformed or unsafe filesystem setting in the VolumeContext fails staging
// with InvalidArgument before any connect or mount.
func TestNodeStageVolume_InvalidFilesystemSettings_NoAttach(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string]map[string]string{
		"unsupported fs-type": {paramFSType: "btrfs"},
		"non-JSON mkfs":       {paramMkfsOptions: "-E lazy_itable_init=0"},
		"external journal":    {paramMkfsOptions: `["-J","device=LABEL=journal"]`},
		"empty mkfs value":    {paramMkfsOptions: `["-L",""]`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newNodeTestEnv(t)
			volCtx := mountVolumeContext("nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-bad", testStorageAddr)
			maps.Copy(volCtx, extra)
			_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          "tank/pvc-bad",
				StagingTargetPath: t.TempDir(),
				VolumeCapability:  mountCap("ext4"),
				VolumeContext:     volCtx,
			})
			requireGRPCCode(t, err, codes.InvalidArgument)
			if len(env.connector.connectCalls) != 0 || len(env.mounter.formatAndMountCalls) != 0 {
				t.Fatalf("connect=%d formatAndMount=%d, want no side effects",
					len(env.connector.connectCalls), len(env.mounter.formatAndMountCalls))
			}
		})
	}
}

// TestValidateMkfsOptions pins the mkfs allowlist boundary: filesystem
// tuning is accepted, while every option that makes mkfs touch another file
// or device, or not leave the expected filesystem on the volume, is rejected.
func TestValidateMkfsOptions(t *testing.T) {
	t.Parallel()
	allowed := map[string][][]string{
		defaultFsType: {
			{"-E", "lazy_itable_init=0,lazy_journal_init=0"},
			{"-Elazy_itable_init=0"},
			{"-m", "1", "-L", "data vol", "-O", "^has_journal", "-J", "size=64"},
			{"-b", "4096", "-i", "16384", "-I", "256", "-T", "largefile"},
		},
		xfsFsType: {
			{"-m", "reflink=1,crc=1", "-i", "size=512", "-K"},
			{"-d", "su=64k,sw=4", "-l", "size=64m", "-n", "ftype=1", "-f"},
		},
	}
	rejected := map[string][][]string{
		defaultFsType: {
			{"-J", "device=/dev/sdb"},              // external journal by path
			{"-J", "device=LABEL=journal"},         // … by label
			{"-J", "device=UUID=0f0e"},             // … by UUID
			{"-d", "etc"},                          // copy a directory in
			{"-l", "badblocks"},                    // read a bad-block list
			{"-z", "undo"},                         // write an undo file
			{"-E", "offset=4096"},                  // filesystem not at offset 0
			{"-O", "journal_dev"},                  // journal device, not a filesystem
			{"-n"}, {"-S"}, {"-V"}, {"-t", "ext3"}, // no or another filesystem
			{"/dev/sdb"}, {"sdb"}, {"--help"}, // positional / long options
			{"-Fq"},    // clustered flags
			{"-L"},     // missing value
			{"-L", ""}, // empty value
			{"-K"},     // an xfs flag
		},
		xfsFsType: {
			{"-l", "logdev=/dev/sdb"},
			{"-r", "rtdev=/dev/sdb"},
			{"-d", "name=/dev/sdb"},
			{"-d", "file=1,size=1g"},
			{"-p", "proto"},
			{"-c", "options=conf"},
			{"-N"},
			{"-E", "lazy_itable_init=0"}, // an ext4 flag
		},
		"btrfs": {{"-L", "x"}},
	}
	for fsType, cases := range allowed {
		for _, opts := range cases {
			if err := validateMkfsOptions(fsType, opts); err != nil {
				t.Errorf("%s %q: unexpected error %v", fsType, opts, err)
			}
		}
	}
	for fsType, cases := range rejected {
		for _, opts := range cases {
			if err := validateMkfsOptions(fsType, opts); err == nil {
				t.Errorf("%s %q: accepted, want rejection", fsType, opts)
			}
		}
	}
}
