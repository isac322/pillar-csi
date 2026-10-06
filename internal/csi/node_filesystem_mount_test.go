//go:build linux

package csi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A durable agent proxy directory can survive the mount it used to contain.
// Exercise the public stage RPC with real paths and the production mounter:
// that directory must never be published as an empty replacement filesystem.
func TestNodeStageFilesystemRejectsMissingOwnedProxyMount(t *testing.T) {
	for _, kind := range []string{"missing", "empty-directory", "foreign-directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, proxy, target := rejectedFilesystemProxyPaths(t, kind)
			mounter := NewKubeMounter()
			registerFilesystemStageCleanup(t, target)
			node := NewNodeServer("storage-node", nil, mounter).
				WithDriverName("files.pillar-csi.bhyoo.com").WithStateDir(filepath.Join(root, "state"))
			const adoption = `{"kind":"zfs-dataset","canonicalSource":"tank/existing",` +
				`"resourceId":"12345","filesystemType":"zfs"}`
			req := &csipb.NodeStageVolumeRequest{
				VolumeId: "agent/nfs/zfs-dataset/tank/native/lifecycle", StagingTargetPath: target,
				VolumeCapability: &csipb.VolumeCapability{
					AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
					AccessMode: &csipb.VolumeCapability_AccessMode{Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
				},
				PublishContext: map[string]string{
					PublishContextKeyFilesystemAdoption:  adoption,
					PublishContextKeyFilesystemCapacity:  "1048576",
					PublishContextKeyFilesystemLayout:    `{"zfs":{"pool":"tank","parentDataset":""}}`,
					PublishContextKeyFilesystemLocalNode: "storage-node",
					PublishContextKeyFilesystemProxyPath: proxy,
				},
			}
			_, stageErr := node.NodeStageVolume(context.Background(), req)
			if status.Code(stageErr) != codes.FailedPrecondition {
				t.Fatalf("stage without an owned proxy mount = %v, want FailedPrecondition for kubelet retry", stageErr)
			}
			assertFilesystemStageRejected(t, node, req)
		})
	}
}

func rejectedFilesystemProxyPaths(t *testing.T, kind string) (root, proxy, target string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy = filepath.Join(root, "proxy")
	switch kind {
	case "empty-directory", "foreign-directory":
		if mkdirErr := os.Mkdir(proxy, 0o750); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if kind == "foreign-directory" {
			path := filepath.Join(proxy, "foreign-data")
			if writeErr := os.WriteFile(path, []byte("must remain private"), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
	case "symlink":
		if symlinkErr := os.Symlink(root, proxy); symlinkErr != nil {
			t.Fatal(symlinkErr)
		}
	}
	target = filepath.Join(root, "stage")
	if mkdirErr := os.Mkdir(target, 0o750); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	return root, proxy, target
}

func registerFilesystemStageCleanup(t *testing.T, target string) {
	t.Helper()
	t.Cleanup(func() {
		mounts, err := readMountInfoFile(procMountInfoPath)
		if err != nil {
			t.Errorf("read test cleanup mount table: %v", err)
			return
		}
		if _, mounted := findMount(mounts, target); mounted {
			if unmountErr := unix.Unmount(target, 0); unmountErr != nil {
				t.Errorf("cleanup test stage: %v", unmountErr)
			}
		}
	})
}

func assertFilesystemStageRejected(t *testing.T, node *NodeServer, req *csipb.NodeStageVolumeRequest) {
	t.Helper()
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if mounted, ok := findMount(mounts, req.StagingTargetPath); ok {
		t.Fatalf("rejected stage exposed a mount: %+v", mounted)
	}
	if _, statErr := os.Stat(node.stateFilePath(req.VolumeId)); !os.IsNotExist(statErr) {
		t.Fatalf("rejected stage persisted a success record: %v", statErr)
	}
	entries, err := os.ReadDir(req.StagingTargetPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected stage exposed host directory contents: entries=%v, err=%v", entries, err)
	}
}

// nfsTransportRoute is the VolumeContext of an adopted PV whose remote
// consumers stage it over NFS; its csi.fsType is the "nfs" transport type.
func nfsTransportRoute() map[string]string {
	return map[string]string{VolumeContextKeyProtocolType: ProtocolNFS, paramFSType: ProtocolNFS}
}

func localFilesystemStageRequest(
	proxy, target, fsType string, volCtx map[string]string, adoption, layout string,
) *csipb.NodeStageVolumeRequest {
	return &csipb.NodeStageVolumeRequest{
		VolumeId: "agent/nfs/zfs-dataset/tank/native/lifecycle", StagingTargetPath: target,
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{FsType: fsType}},
			AccessMode: &csipb.VolumeCapability_AccessMode{Mode: csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		VolumeContext: volCtx,
		PublishContext: map[string]string{
			PublishContextKeyFilesystemAdoption:  adoption,
			PublishContextKeyFilesystemCapacity:  "1048576",
			PublishContextKeyFilesystemLayout:    layout,
			PublishContextKeyFilesystemLocalNode: "storage-node",
			PublishContextKeyFilesystemProxyPath: proxy,
		},
	}
}

// The storage node stages an adopted RWX PV through a local bind while remote
// consumers use NFS, so the PV's csi.fsType "nfs" is a transport hint. It must
// reach native source verification (FailedPrecondition for an unmounted proxy)
// instead of failing as a type mismatch, while a wrong native type or an "nfs"
// hint outside the NFS stage contract fails as InvalidArgument.
func TestNodeStageFilesystemNFSTransportHintKeepsNativeChecks(t *testing.T) {
	cases := []struct {
		name   string
		fsType string
		volCtx map[string]string
		want   codes.Code
	}{
		{"nfs transport hint", ProtocolNFS, nfsTransportRoute(), codes.FailedPrecondition},
		{"wrong native filesystem", "xfs", nfsTransportRoute(), codes.InvalidArgument},
		{"nfs hint off the NFS route", ProtocolNFS,
			map[string]string{VolumeContextKeyProtocolType: ProtocolNVMeoFTCP}, codes.InvalidArgument},
		{"nfs hint with a formatting fs-type", ProtocolNFS,
			map[string]string{VolumeContextKeyProtocolType: ProtocolNFS, paramFSType: "xfs"}, codes.InvalidArgument},
		{"nfs hint with mkfs options", ProtocolNFS,
			map[string]string{VolumeContextKeyProtocolType: ProtocolNFS, paramMkfsOptions: `["-E","nodiscard"]`},
			codes.InvalidArgument},
		{"nfs hint with periodic trim", ProtocolNFS,
			map[string]string{VolumeContextKeyProtocolType: ProtocolNFS, paramPeriodicTrim: "true"},
			codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, proxy, target := rejectedFilesystemProxyPaths(t, "foreign-directory")
			registerFilesystemStageCleanup(t, target)
			node := NewNodeServer("storage-node", nil, NewKubeMounter()).
				WithDriverName("files.pillar-csi.bhyoo.com").WithStateDir(filepath.Join(root, "state"))
			req := localFilesystemStageRequest(proxy, target, tc.fsType, tc.volCtx,
				`{"kind":"zfs-dataset","canonicalSource":"tank/existing","resourceId":"12345","filesystemType":"zfs"}`,
				`{"zfs":{"pool":"tank","parentDataset":""}}`)
			_, err := node.NodeStageVolume(context.Background(), req)
			if status.Code(err) != tc.want {
				t.Fatalf("NodeStageVolume(fsType %q) = %v, want %s", tc.fsType, err, tc.want)
			}
			assertFilesystemStageRejected(t, node, req)
		})
	}
}

// With mount privileges, the local stage under the "nfs" transport hint binds
// the native source itself: the staged mount keeps the native type and inode,
// and the record names the controller-owned proxy and native identity.
func TestNodeStageFilesystemBindsNativeSourceUnderNFSTransportHint(t *testing.T) {
	fx := mountNativeProxyFixture(t)
	adoption := fmt.Sprintf(`{"kind":"directory","canonicalSource":%q,"resourceId":"uuid:42",`+
		`"filesystemType":%q,"inode":%d}`, fx.source, fx.nativeType, fx.inode)
	node := NewNodeServer("storage-node", nil, NewKubeMounter()).
		WithDriverName("files.pillar-csi.bhyoo.com").WithStateDir(filepath.Join(fx.root, "state"))
	req := localFilesystemStageRequest(fx.proxy, fx.target, ProtocolNFS, nfsTransportRoute(), adoption,
		`{"directory":{"logicalPool":"pool","hostRoot":"/existing"}}`)

	_, err := node.NodeStageVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("local stage under the NFS transport hint: %v", err)
	}
	assertNativeBindStaged(t, fx)
	assertNativeFileStageRecord(t, node, req.VolumeId, fx)
}

// nativeProxyFixture is a native source directory bind-mounted onto the
// controller-owned proxy, plus an empty staging target.
type nativeProxyFixture struct {
	root, source, proxy, target, nativeType string
	inode                                   uint64
}

func mountNativeProxyFixture(t *testing.T) nativeProxyFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := nativeProxyFixture{
		root: root, source: filepath.Join(root, "source"),
		proxy: filepath.Join(root, "proxy"), target: filepath.Join(root, "stage"),
	}
	for _, dir := range []string{fx.source, fx.proxy, fx.target} {
		err = os.Mkdir(dir, 0o750)
		if err != nil {
			t.Fatal(err)
		}
	}
	err = os.WriteFile(filepath.Join(fx.source, "marker"), []byte("native data"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fx.nativeType = nativeFilesystemType(t, fx.source)
	var sourceStat unix.Stat_t
	err = unix.Stat(fx.source, &sourceStat)
	if err != nil {
		t.Fatal(err)
	}
	fx.inode = sourceStat.Ino
	err = unix.Mount(fx.source, fx.proxy, "", unix.MS_BIND, "")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("bind-mounting the owned proxy requires mount privileges: %v", err)
	}
	if err != nil {
		t.Fatalf("bind-mount owned proxy: %v", err)
	}
	t.Cleanup(func() {
		unmountErr := unix.Unmount(fx.proxy, 0)
		if unmountErr != nil {
			t.Errorf("cleanup owned proxy: %v", unmountErr)
		}
	})
	registerFilesystemStageCleanup(t, fx.target)
	return fx
}

// assertNativeBindStaged checks the staging target is a native-type mount
// that exposes the adopted source's data.
func assertNativeBindStaged(t *testing.T, fx nativeProxyFixture) {
	t.Helper()
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	staged, ok := findMount(mounts, fx.target)
	if !ok || staged.FsType != fx.nativeType {
		t.Fatalf("staged mount = %+v (mounted %v), want a native %s bind", staged, ok, fx.nativeType)
	}
	stage, err := os.OpenRoot(fx.target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stage.Close() }() //nolint:errcheck // read-only handle
	data, err := stage.ReadFile("marker")
	if err != nil || string(data) != "native data" {
		t.Fatalf("staged view of the native source = %q, %v", data, err)
	}
}

func assertNativeFileStageRecord(t *testing.T, node *NodeServer, volumeID string, fx nativeProxyFixture) {
	t.Helper()
	state, err := node.readStageState(volumeID)
	if err != nil || state == nil || state.File == nil {
		t.Fatalf("stage record = %+v, %v; want a file stage", state, err)
	}
	file := state.File
	if state.ProtocolType != ProtocolNFS || !file.Local || file.ProxyPath != fx.proxy ||
		file.FilesystemType != fx.nativeType || file.Inode != fx.inode || file.CanonicalSource != fx.source {
		t.Fatalf("stage record = %+v, want local %s bind of %q through %q", file, fx.nativeType, fx.source, fx.proxy)
	}
}

func nativeFilesystemType(t *testing.T, path string) string {
	t.Helper()
	var filesystem unix.Statfs_t
	err := unix.Statfs(path, &filesystem)
	if err != nil {
		t.Fatal(err)
	}
	switch filesystem.Type {
	case unix.EXT4_SUPER_MAGIC:
		return "ext4"
	case unix.XFS_SUPER_MAGIC:
		return "xfs"
	case 0x2fc12fc1:
		return "zfs"
	}
	t.Skipf("temporary directory filesystem 0x%x is not an adoptable native type", filesystem.Type)
	return ""
}
