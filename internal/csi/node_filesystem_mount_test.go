//go:build linux

package csi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/isac322/pillar-csi/api/v1alpha1"
)

// fileZFSDatasetAdoption is the immutable adoption descriptor JSON of a ZFS
// dataset adoption used by the native local-publish fixtures.
func fileZFSDatasetAdoption() string {
	return `{"kind":"zfs-dataset","canonicalSource":"tank/existing",` +
		`"resourceId":"12345","filesystemType":"zfs"}`
}

// fileLocalPublishContext returns the PublishContext a controller produces for
// a same-node (local) consumer: immutable adoption identity plus the
// controller-owned proxy mount path and its node binding.
func fileLocalPublishContext(proxy, node, adoption, layout string) map[string]string {
	return map[string]string{
		PublishContextKeyFilesystemAdoption:  adoption,
		PublishContextKeyFilesystemCapacity:  "1048576",
		PublishContextKeyFilesystemLayout:    layout,
		PublishContextKeyFilesystemLocalNode: node,
		PublishContextKeyFilesystemProxyPath: proxy,
	}
}

// A durable agent proxy directory can survive the mount it used to contain.
// Exercise the public publish RPC with real paths and the production mounter:
// that directory must never be published to a pod as an empty replacement
// filesystem, and no staging path is involved at any point.
func TestNodePublishFilesystemRejectsMissingOwnedProxyMount(t *testing.T) {
	for _, kind := range []string{"missing", "empty-directory", "foreign-directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, proxy, target := rejectedFilesystemProxyPaths(t, kind)
			registerFilesystemPublishCleanup(t, root)
			node := NewNodeServer("storage-node", nil, NewKubeMounter()).
				WithDriverName(v1alpha1.FileCSIDriver).WithStateDir(filepath.Join(root, "state"))
			req := localFilesystemPublishRequest(proxy, target, "", nil, fileZFSDatasetAdoption(),
				`{"zfs":{"pool":"tank","parentDataset":""}}`)
			_, err := node.NodePublishVolume(context.Background(), req)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("publish without an owned proxy mount = %v, want FailedPrecondition for kubelet retry", err)
			}
			assertFilesystemPublishRejected(t, node, req)
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
		if linkErr := os.Symlink(root, proxy); linkErr != nil {
			t.Fatal(linkErr)
		}
	}
	target = filepath.Join(root, "pod-target")
	if mkdirErr := os.Mkdir(target, 0o750); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	return root, proxy, target
}

// registerFilesystemPublishCleanup unmounts every mount the test left under
// root, innermost first, so a failing assertion never leaks host mounts.
func registerFilesystemPublishCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		for {
			mounts, err := readMountInfoFile(procMountInfoPath)
			if err != nil {
				t.Errorf("read test cleanup mount table: %v", err)
				return
			}
			var under []string
			for _, m := range mounts {
				if m.MountPoint == root || strings.HasPrefix(m.MountPoint, root+"/") {
					under = append(under, m.MountPoint)
				}
			}
			if len(under) == 0 {
				return
			}
			slices.SortFunc(under, func(a, b string) int { return len(b) - len(a) })
			if unmountErr := unix.Unmount(under[0], unix.MNT_DETACH); unmountErr != nil {
				t.Errorf("cleanup test mount %q: %v", under[0], unmountErr)
				return
			}
		}
	})
}

func assertFilesystemPublishRejected(t *testing.T, node *NodeServer, req *csipb.NodePublishVolumeRequest) {
	t.Helper()
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if mounted, ok := findMount(mounts, req.TargetPath); ok {
		t.Fatalf("rejected publish exposed a mount: %+v", mounted)
	}
	if _, statErr := os.Stat(node.stateFilePath(req.VolumeId)); !os.IsNotExist(statErr) {
		t.Fatalf("rejected publish persisted a success record: %v", statErr)
	}
	entries, err := os.ReadDir(req.TargetPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected publish exposed host directory contents: entries=%v, err=%v", entries, err)
	}
}

// localFilesystemPublishRequest is the kubelet NodePublish of a NoStage
// driver for a same-node consumer: StagingTargetPath is empty.
func localFilesystemPublishRequest(
	proxy, target, fsType string, volCtx map[string]string, adoption, layout string,
) *csipb.NodePublishVolumeRequest {
	return &csipb.NodePublishVolumeRequest{
		VolumeId: "agent/nfs/zfs-dataset/tank/native/lifecycle", TargetPath: target,
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{FsType: fsType}},
			AccessMode: &csipb.VolumeCapability_AccessMode{Mode: csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		VolumeContext:  volCtx,
		PublishContext: fileLocalPublishContext(proxy, "storage-node", adoption, layout),
	}
}

// The storage node publishes an adopted RWX PV through a local bind while
// remote consumers use NFS, so the PV's csi.fsType "nfs" is a transport hint.
// It must reach native source verification (FailedPrecondition for an
// unmounted proxy) instead of failing as a type mismatch, while a wrong native
// type or an "nfs" hint outside the NFS contract fails as InvalidArgument.
func TestNodePublishFilesystemNFSTransportHintKeepsNativeChecks(t *testing.T) {
	cases := []struct {
		name   string
		fsType string
		volCtx map[string]string
		want   codes.Code
	}{
		{"nfs transport hint", ProtocolNFS, nfsTransportRoute(), codes.FailedPrecondition},
		{"empty fs-type", "", nfsTransportRoute(), codes.FailedPrecondition},
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
			registerFilesystemPublishCleanup(t, root)
			node := NewNodeServer("storage-node", nil, NewKubeMounter()).
				WithDriverName(v1alpha1.FileCSIDriver).WithStateDir(filepath.Join(root, "state"))
			req := localFilesystemPublishRequest(proxy, target, tc.fsType, tc.volCtx,
				fileZFSDatasetAdoption(), `{"zfs":{"pool":"tank","parentDataset":""}}`)
			_, err := node.NodePublishVolume(context.Background(), req)
			if status.Code(err) != tc.want {
				t.Fatalf("NodePublishVolume(fsType %q) = %v, want %s", tc.fsType, err, tc.want)
			}
			assertFilesystemPublishRejected(t, node, req)
		})
	}
}

// With mount privileges, the local publish under the "nfs" transport hint
// binds the native source directly at the pod target: the target keeps the
// native type and data, the record names the controller-owned proxy and the
// native identity, and no staging/global mount exists.
func TestNodePublishFilesystemBindsNativeSourceUnderNFSTransportHint(t *testing.T) {
	fx := mountNativeProxyFixture(t)
	node := fx.node(t)
	req := fx.publishRequest(fx.target, ProtocolNFS)

	_, err := node.NodePublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("local publish under the NFS transport hint: %v", err)
	}
	assertNativeBindPublished(t, fx, fx.target)
	assertNativeFilePublishRecord(t, node, req.VolumeId, fx)
}

// Direct host I/O through the published bind reaches the adopted source and
// the proxy stays owned by the agent/controller lifecycle after teardown.
func TestNodePublishFilesystemDirectHostIOAndSourceRetained(t *testing.T) {
	fx := mountNativeProxyFixture(t)
	node := fx.node(t)
	req := fx.publishRequest(fx.target, "")
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.target, "pod-write"), []byte("from pod"), 0o600); err != nil {
		t.Fatalf("write through published target: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(fx.source, "pod-write"))
	if err != nil || string(data) != "from pod" {
		t.Fatalf("pod write not visible in adopted source: %q, %v", data, err)
	}

	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: req.VolumeId, TargetPath: fx.target,
	}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	assertNotMounted(t, fx.target)
	assertMounted(t, fx.proxy)
	if _, err := os.Stat(filepath.Join(fx.source, "pod-write")); err != nil {
		t.Fatalf("unpublish touched the adopted source: %v", err)
	}
	if _, err := os.Stat(node.stateFilePath(req.VolumeId)); !os.IsNotExist(err) {
		t.Fatalf("last-target unpublish left the record: %v", err)
	}
}

// Unpublish never tears down a mount the volume record does not own: naming
// the controller-owned proxy (with or without a record for the handle) is
// refused, and the proxy, the published pod bind and the record all survive
// in the real mount table.
func TestNodePublishFilesystemUnpublishRefusesOwnedProxy(t *testing.T) {
	fx := mountNativeProxyFixture(t)
	node := fx.node(t)
	req := fx.publishRequest(fx.target, "")
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}
	recordBefore, err := os.ReadFile(node.stateFilePath(req.VolumeId))
	if err != nil {
		t.Fatalf("read durable record: %v", err)
	}

	for _, volumeID := range []string{req.VolumeId, "agent/nfs/directory/pool/native/never-published"} {
		_, unpublishErr := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
			VolumeId: volumeID, TargetPath: fx.proxy,
		})
		if status.Code(unpublishErr) != unpublishWrongTargetCode {
			t.Fatalf("unpublish of owned proxy under %q = %v, want %s", volumeID, unpublishErr, unpublishWrongTargetCode)
		}
		assertMounted(t, fx.proxy)
		assertNativeBindPublished(t, fx, fx.target)
	}
	recordAfter, err := os.ReadFile(node.stateFilePath(req.VolumeId))
	if err != nil || !bytes.Equal(recordAfter, recordBefore) {
		t.Fatalf("refused unpublish changed the durable record: %v\nbefore=%s\nafter=%s", err, recordBefore, recordAfter)
	}
}

// The kernel-visible state of a published target must still match the durable
// record on every retry: a read-only remount of a read-write publish, or a
// foreign mount shadowing the target, fails republish and unpublish alike
// with nothing torn down.
func TestNodePublishFilesystemKernelMismatchRefused(t *testing.T) {
	fx := mountNativeProxyFixture(t)
	node := fx.node(t)
	req := fx.publishRequest(fx.target, "")
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Read-only drift: the record says read-write; a remount flipped the flag.
	if err := unix.Mount("", fx.target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("read-only bind remount not permitted: %v", err)
		}
		t.Fatalf("ro remount: %v", err)
	}
	if _, err := node.NodePublishVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("republish over ro-drifted mount = %v, want FailedPrecondition", err)
	}
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: req.VolumeId, TargetPath: fx.target,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unpublish of ro-drifted mount = %v, want FailedPrecondition", err)
	}
	assertNativeBindPublished(t, fx, fx.target)
	assertNativeFilePublishRecord(t, node, req.VolumeId, fx)

	// Convergence after the kernel state is repaired.
	if err := unix.Mount("", fx.target, "", unix.MS_REMOUNT|unix.MS_BIND, ""); err != nil {
		t.Fatalf("rw remount: %v", err)
	}
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("republish after rw remount: %v", err)
	}
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: req.VolumeId, TargetPath: fx.target,
	}); err != nil {
		t.Fatalf("unpublish repaired mount: %v", err)
	}

	// Wrong filesystem at the target: a foreign mount is never adopted or
	// torn down, whether or not a record exists for the volume.
	if err := unix.Mount("none", fx.target, "tmpfs", 0, ""); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("tmpfs mount not permitted: %v", err)
		}
		t.Fatalf("foreign mount: %v", err)
	}
	defer func() {
		if err := unix.Unmount(fx.target, 0); err != nil {
			t.Errorf("cleanup foreign mount: %v", err)
		}
	}()
	if _, err := node.NodePublishVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("republish over foreign mount = %v, want FailedPrecondition", err)
	}
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: req.VolumeId, TargetPath: fx.target,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unpublish over foreign mount = %v, want FailedPrecondition", err)
	}
	assertMounted(t, fx.target)
}

// Multiple pods on one node: each target is an independent direct bind with
// its own readonly flag; unpublishing one keeps the peer, the proxy and the
// record, and the last unpublish removes only the record.
func TestNodePublishFilesystemMultiTargetReadonlyPeerRetained(t *testing.T) {
	fx := mountNativeProxyFixture(t)
	node := fx.node(t)
	rwTarget := fx.target
	roTarget := filepath.Join(fx.root, "pod-ro")
	if err := os.Mkdir(roTarget, 0o750); err != nil {
		t.Fatal(err)
	}

	if _, err := node.NodePublishVolume(context.Background(), fx.publishRequest(rwTarget, "")); err != nil {
		t.Fatalf("rw publish: %v", err)
	}
	ro := fx.publishRequest(roTarget, "")
	ro.Readonly = true
	if _, err := node.NodePublishVolume(context.Background(), ro); err != nil {
		t.Fatalf("ro publish: %v", err)
	}
	assertNativeBindPublished(t, fx, rwTarget)
	assertNativeBindPublished(t, fx, roTarget)
	assertReadOnly(t, roTarget, true)
	assertReadOnly(t, rwTarget, false)

	// The recorded readonly flag is authoritative per target.
	ro.Readonly = false
	if _, err := node.NodePublishVolume(context.Background(), ro); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ro target republished rw = %v, want FailedPrecondition", err)
	}
	assertReadOnly(t, roTarget, true)

	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: ro.VolumeId, TargetPath: roTarget,
	}); err != nil {
		t.Fatalf("unpublish ro peer: %v", err)
	}
	assertNotMounted(t, roTarget)
	assertNativeBindPublished(t, fx, rwTarget)
	assertMounted(t, fx.proxy)
	assertNativeFilePublishRecord(t, node, ro.VolumeId, fx)

	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: ro.VolumeId, TargetPath: rwTarget,
	}); err != nil {
		t.Fatalf("unpublish last target: %v", err)
	}
	assertNotMounted(t, rwTarget)
	assertMounted(t, fx.proxy)
	if _, err := os.Stat(node.stateFilePath(ro.VolumeId)); !os.IsNotExist(err) {
		t.Fatalf("last-target unpublish left the record: %v", err)
	}
}

// A restarted node recovers from the actual mount table and the durable
// record: republishing a still-mounted target is idempotent (one mount entry,
// no blind remount), a drifted descriptor is refused, and unpublish works.
func TestNodePublishFilesystemRestartIdempotentAndImmutable(t *testing.T) {
	fx := mountNativeProxyFixture(t)
	node := fx.node(t)
	req := fx.publishRequest(fx.target, ProtocolNFS)
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}

	restarted := fx.node(t)
	if _, err := restarted.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("republish after restart: %v", err)
	}
	if n := mountEntries(t, fx.target); n != 1 {
		t.Fatalf("republish stacked %d mounts on the target, want 1", n)
	}

	drifted := fx.publishRequest(fx.target, ProtocolNFS)
	drifted.PublishContext[PublishContextKeyFilesystemCapacity] = "2097152"
	if _, err := restarted.NodePublishVolume(context.Background(), drifted); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("drifted republish = %v, want FailedPrecondition", err)
	}
	assertNativeBindPublished(t, fx, fx.target)
	assertNativeFilePublishRecord(t, restarted, req.VolumeId, fx)

	if _, err := restarted.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: req.VolumeId, TargetPath: fx.target,
	}); err != nil {
		t.Fatalf("unpublish after restart: %v", err)
	}
	assertNotMounted(t, fx.target)
}

// nativeProxyFixture is a native source directory bind-mounted onto the
// controller-owned proxy, plus an empty pod target.
type nativeProxyFixture struct {
	root, source, proxy, target, nativeType string
	inode                                   uint64
}

func (fx nativeProxyFixture) node(t *testing.T) *NodeServer {
	t.Helper()
	return NewNodeServer("storage-node", nil, NewKubeMounter()).
		WithDriverName(v1alpha1.FileCSIDriver).WithStateDir(filepath.Join(fx.root, "state"))
}

func (fx nativeProxyFixture) publishRequest(target, fsType string) *csipb.NodePublishVolumeRequest {
	adoption := fmt.Sprintf(`{"kind":"directory","canonicalSource":%q,"resourceId":"uuid:42",`+
		`"filesystemType":%q,"inode":%d}`, fx.source, fx.nativeType, fx.inode)
	return localFilesystemPublishRequest(fx.proxy, target, fsType, nfsTransportRoute(), adoption,
		`{"directory":{"logicalPool":"pool","hostRoot":"/existing"}}`)
}

func mountNativeProxyFixture(t *testing.T) nativeProxyFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := nativeProxyFixture{
		root: root, source: filepath.Join(root, "source"),
		proxy: filepath.Join(root, "proxy"), target: filepath.Join(root, "pod-target"),
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
	registerFilesystemPublishCleanup(t, root)
	err = unix.Mount(fx.source, fx.proxy, "", unix.MS_BIND, "")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("bind-mounting the owned proxy requires mount privileges: %v", err)
	}
	if err != nil {
		t.Fatalf("bind-mount owned proxy: %v", err)
	}
	return fx
}

// assertNativeBindPublished checks the pod target is a native-type mount
// that exposes the adopted source's data.
func assertNativeBindPublished(t *testing.T, fx nativeProxyFixture, target string) {
	t.Helper()
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	published, ok := findMount(mounts, target)
	if !ok || published.FsType != fx.nativeType {
		t.Fatalf("published mount = %+v (mounted %v), want a native %s bind", published, ok, fx.nativeType)
	}
	view, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = view.Close() }() //nolint:errcheck // read-only handle
	data, err := view.ReadFile("marker")
	if err != nil || string(data) != "native data" {
		t.Fatalf("published view of the native source = %q, %v", data, err)
	}
}

func assertNativeFilePublishRecord(t *testing.T, node *NodeServer, volumeID string, fx nativeProxyFixture) {
	t.Helper()
	state, err := node.readStageState(volumeID)
	if err != nil || state == nil || state.File == nil {
		t.Fatalf("publish record = %+v, %v; want a file record", state, err)
	}
	file := state.File
	if !file.Local || file.ProxyPath != fx.proxy ||
		file.FilesystemType != fx.nativeType || file.Inode != fx.inode || file.CanonicalSource != fx.source {
		t.Fatalf("publish record = %+v, want local %s bind of %q through %q", file, fx.nativeType, fx.source, fx.proxy)
	}
}

func mountEntries(t *testing.T, path string) int {
	t.Helper()
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range mounts {
		if m.MountPoint == path {
			n++
		}
	}
	return n
}

func assertMounted(t *testing.T, path string) {
	t.Helper()
	if mountEntries(t, path) == 0 {
		t.Fatalf("%q is no longer mounted", path)
	}
}

func assertNotMounted(t *testing.T, path string) {
	t.Helper()
	if n := mountEntries(t, path); n != 0 {
		t.Fatalf("%q still has %d mount entries", path, n)
	}
}

func assertReadOnly(t *testing.T, path string, want bool) {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		t.Fatal(err)
	}
	if got := fs.Flags&unix.ST_RDONLY != 0; got != want {
		t.Fatalf("%q read-only = %v, want %v", path, got, want)
	}
	writeErr := os.WriteFile(filepath.Join(path, "ro-probe"), []byte("x"), 0o600)
	if want && !errors.Is(writeErr, unix.EROFS) {
		t.Fatalf("write to read-only target %q = %v, want EROFS", path, writeErr)
	}
	if !want {
		if writeErr != nil {
			t.Fatalf("write to rw target %q: %v", path, writeErr)
		}
		_ = os.Remove(filepath.Join(path, "ro-probe")) //nolint:errcheck // probe cleanup
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
