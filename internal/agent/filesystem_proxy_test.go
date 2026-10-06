package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

type proxyTestMounter struct {
	mounted    map[string]bool
	mounts     []string
	unmounts   []string
	mountErr   error
	unmountErr error
}

func (m *proxyTestMounter) Mount(_, target string) error {
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mounted[target] = true
	m.mounts = append(m.mounts, target)
	return nil
}
func (m *proxyTestMounter) Unmount(target string) error {
	m.unmounts = append(m.unmounts, target)
	if m.unmountErr != nil {
		return m.unmountErr
	}
	m.mounted[target] = false
	return nil
}
func (m *proxyTestMounter) Mounted(target string) (bool, error) { return m.mounted[target], nil }

type proxyTestBackend struct {
	backendType agentv1.BackendType
	layout      backend.Layout
	adoption    *agentv1.FilesystemAdoption
	capacity    int64
	source      string
	present     bool
	verifyErr   error
}

func (b *proxyTestBackend) Create(
	context.Context,
	string,
	int64,
	*agentv1.BackendParams,
) (source string, capacity int64, err error) {
	return b.source, b.capacity, errors.New("filesystem test backend must not create")
}
func (b *proxyTestBackend) Delete(context.Context, string) error { b.present = false; return nil }
func (b *proxyTestBackend) Expand(context.Context, string, int64) (int64, error) {
	return b.capacity, nil
}
func (b *proxyTestBackend) Capacity(context.Context) (used, capacity int64, err error) {
	return b.capacity, b.capacity, nil
}
func (*proxyTestBackend) ListVolumes(context.Context) ([]*agentv1.VolumeInfo, error) {
	return nil, nil
}
func (b *proxyTestBackend) DevicePath(string) string  { return b.source }
func (b *proxyTestBackend) Type() agentv1.BackendType { return b.backendType }
func (b *proxyTestBackend) Layout() backend.Layout    { return b.layout }
func (b *proxyTestBackend) InspectImport(
	_ context.Context,
	_ string,
	required int64,
	_ *agentv1.FilesystemAdoption,
	layout backend.Layout,
) (*backend.ImportInspection, error) {
	if required != b.capacity || layout != b.layout || !b.present {
		return nil, &backend.ImportRefusedError{Reason: "identity or quota drift"}
	}
	return &backend.ImportInspection{Filesystem: b.adoption, CapacityBytes: b.capacity}, nil
}
func (b *proxyTestBackend) ImportFilesystem(
	_ context.Context,
	_ string,
	required int64,
	adoption *agentv1.FilesystemAdoption,
	layout backend.Layout,
) (backend.PinnedFilesystem, error) {
	if required != b.capacity ||
		layout != b.layout ||
		!b.present ||
		adoption.GetResourceId() != b.adoption.GetResourceId() {
		return nil, &backend.ImportRefusedError{Reason: "identity or quota drift"}
	}
	return &proxyTestPin{backend: b}, nil
}

type proxyTestPin struct{ backend *proxyTestBackend }

func (p *proxyTestPin) Adoption() *agentv1.FilesystemAdoption     { return p.backend.adoption }
func (p *proxyTestPin) CapacityBytes() int64                      { return p.backend.capacity }
func (p *proxyTestPin) MountSource() string                       { return p.backend.source }
func (p *proxyTestPin) VerifyMount(context.Context, string) error { return p.backend.verifyErr }
func (*proxyTestPin) Close() error                                { return nil }

var _ backend.VolumeBackend = (*proxyTestBackend)(nil)
var _ backend.VolumeInspector = (*proxyTestBackend)(nil)
var _ backend.FilesystemImporter = (*proxyTestBackend)(nil)

type proxyLifecycleFixture struct {
	server   *Server
	backend  *proxyTestBackend
	mounter  *proxyTestMounter
	adoption *agentv1.FilesystemAdoption
	params   *agentv1.BackendParams
	source   string
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temporary directory: %v", err)
	}
	return root
}

func newProxyLifecycleFixture(t *testing.T) *proxyLifecycleFixture {
	t.Helper()
	source := filepath.Join(canonicalTempDir(t), "source")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(canonicalTempDir(t), "proxies")
	adoption := &agentv1.FilesystemAdoption{
		Kind:            "directory",
		CanonicalSource: source,
		ResourceId:      "uuid:proxy-test",
		FilesystemType:  "ext4",
		FilesystemId:    "uuid",
		Inode:           42,
		ProjectId:       7,
	}
	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Directory{
			Directory: &agentv1.DirectoryVolumeParams{
				LogicalPool: "pool",
				HostRoot:    "/trusted",
			},
		},
	}
	b := &proxyTestBackend{
		backendType: agentv1.BackendType_BACKEND_TYPE_DIRECTORY,
		layout:      backend.Layout{HostRoot: "/trusted"},
		adoption:    adoption,
		capacity:    1 << 20,
		source:      source,
		present:     true,
	}
	mounter := &proxyTestMounter{mounted: map[string]bool{}}
	s := NewServer(
		map[string]backend.VolumeBackend{"pool": b},
		"",
		WithDrainStateDir(canonicalTempDir(t)),
		WithFilesystemProxy(root),
	)
	s.filesystemMounter = mounter
	return &proxyLifecycleFixture{
		server:   s,
		backend:  b,
		mounter:  mounter,
		adoption: adoption,
		params:   params,
		source:   source,
	}
}

func proxyImportRequest(
	adoption *agentv1.FilesystemAdoption,
	params *agentv1.BackendParams,
	uid string,
) *agentv1.ImportVolumeRequest {
	return &agentv1.ImportVolumeRequest{
		VolumeId:           "pool/volume",
		BackendType:        agentv1.BackendType_BACKEND_TYPE_DIRECTORY,
		CapacityBytes:      1 << 20,
		FilesystemAdoption: adoption,
		BackendParams:      params,
		Fence: &agentv1.FencingToken{
			VolumeUid:  uid,
			Generation: 1,
		},
	}
}

func proxyLocalRequest(
	adoption *agentv1.FilesystemAdoption,
	params *agentv1.BackendParams,
	uid string,
	generation uint64,
	local bool,
) *agentv1.SetLocalAttachRequest {
	return &agentv1.SetLocalAttachRequest{
		VolumeId:           "pool/volume",
		Local:              local,
		FilesystemAdoption: adoption,
		BackendParams:      params,
		CapacityBytes:      1 << 20,
		Fence: &agentv1.FencingToken{
			VolumeUid:  uid,
			Generation: generation,
		},
	}
}

func proxyReleaseRequest(
	adoption *agentv1.FilesystemAdoption,
	uid string,
	generation uint64,
) *agentv1.ReleaseVolumeRequest {
	return &agentv1.ReleaseVolumeRequest{
		VolumeId:           "pool/volume",
		FilesystemAdoption: adoption,
		Fence: &agentv1.FencingToken{
			VolumeUid:  uid,
			Generation: generation,
		},
	}
}

func TestFilesystemProxyLocalLifecycleRetainsSourceAndRetiresFence(t *testing.T) {
	fixture := newProxyLifecycleFixture(t)
	s := fixture.server
	b := fixture.backend
	mounter := fixture.mounter
	adoption := fixture.adoption
	params := fixture.params
	source := fixture.source
	ctx := context.Background()
	if _, importErr := s.ImportVolume(ctx, proxyImportRequest(adoption, params, "owner-a")); importErr != nil {
		t.Fatal(importErr)
	}
	local, attachErr := s.SetLocalAttach(ctx, proxyLocalRequest(adoption, params, "owner-a", 2, true))
	if attachErr != nil {
		t.Fatal(attachErr)
	}
	target := local.GetDevicePath()
	if target == "" || !mounter.mounted[target] {
		t.Fatalf("local attach did not publish mounted proxy: path=%q mounted=%v", target, mounter.mounted[target])
	}
	deleteReq := &agentv1.DeleteVolumeRequest{
		VolumeId:           "pool/volume",
		BackendType:        agentv1.BackendType_BACKEND_TYPE_DIRECTORY,
		FilesystemAdoption: adoption,
		Fence: &agentv1.FencingToken{
			VolumeUid:  "owner-a",
			Generation: 3,
		},
	}
	if _, deleteErr := s.DeleteVolume(ctx, deleteReq); status.Code(deleteErr) != codes.FailedPrecondition {
		t.Fatalf("DeleteVolume while locally attached = %v, want FailedPrecondition", deleteErr)
	}
	releaseReq := proxyReleaseRequest(adoption, "owner-a", 3)
	if _, releaseErr := s.ReleaseVolume(ctx, releaseReq); status.Code(releaseErr) != codes.FailedPrecondition {
		t.Fatalf("ReleaseVolume while locally attached = %v, want FailedPrecondition", releaseErr)
	}
	if _, detachErr := s.SetLocalAttach(ctx, proxyLocalRequest(adoption, params, "owner-a", 4, false)); detachErr != nil {
		t.Fatal(detachErr)
	}
	if mounter.mounted[target] {
		t.Fatalf("local=false left proxy mounted at %q", target)
	}
	if _, releaseErr := s.ReleaseVolume(ctx, proxyReleaseRequest(adoption, "owner-a", 5)); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if !b.present {
		t.Fatal("release destroyed the adopted source")
	}
	if _, importErr := s.ImportVolume(ctx, proxyImportRequest(adoption, params, "owner-b")); importErr != nil {
		t.Fatalf("new lifecycle could not re-import after release: %v", importErr)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("adopted source was removed: %v", statErr)
	}
}

func TestFilesystemProxyRejectsMismatchedExistingMount(t *testing.T) {
	fixture := newProxyLifecycleFixture(t)
	s := fixture.server
	b := fixture.backend
	mounter := fixture.mounter
	adoption := fixture.adoption
	params := fixture.params
	ctx := context.Background()
	if _, err := s.ImportVolume(ctx, proxyImportRequest(adoption, params, "owner")); err != nil {
		t.Fatal(err)
	}
	mark, exists, markErr := s.readFencingMark(backend.FilesystemFenceID(adoption))
	if markErr != nil || !exists {
		t.Fatalf("read durable import mark: exists=%v err=%v", exists, markErr)
	}
	pin := &proxyTestPin{backend: b}
	target, targetErr := s.filesystemProxyPath(backend.FilesystemFenceID(adoption))
	if targetErr != nil {
		t.Fatal(targetErr)
	}
	mkdirParentErr := ensureDirectoryPath(filepath.Dir(target))
	if mkdirParentErr != nil {
		t.Fatal(mkdirParentErr)
	}
	mkdirTargetErr := os.Mkdir(target, 0o750)
	if mkdirTargetErr != nil {
		t.Fatal(mkdirTargetErr)
	}
	mounter.mounted[target] = true
	_, ensureErr := s.ensureFilesystemProxy(ctx, "pool/alias", pin, &mark)
	if ensureErr == nil {
		t.Fatal("same-native foreign mount was accepted")
	}
	if !mounter.mounted[target] {
		t.Fatal("same-native foreign mount was detached")
	}
}

func TestFilesystemProxyRollbackReportsUnmountFailureAndRemoveRetainsBusyMount(t *testing.T) {
	fixture := newProxyLifecycleFixture(t)
	s := fixture.server
	b := fixture.backend
	mounter := fixture.mounter
	adoption := fixture.adoption
	params := fixture.params
	ctx := context.Background()
	if _, err := s.ImportVolume(ctx, proxyImportRequest(adoption, params, "owner")); err != nil {
		t.Fatal(err)
	}
	mark, exists, markErr := s.readFencingMark(backend.FilesystemFenceID(adoption))
	if markErr != nil || !exists {
		t.Fatalf("read durable import mark: exists=%v err=%v", exists, markErr)
	}
	b.verifyErr = errors.New("native identity mismatch")
	mounter.unmountErr = errors.New("device busy")
	pin := &proxyTestPin{backend: b}
	_, ensureErr := s.ensureFilesystemProxy(ctx, "pool/volume", pin, &mark)
	if ensureErr == nil || !strings.Contains(ensureErr.Error(), "device busy") {
		t.Fatalf("rollback error = %v, want unmount failure", ensureErr)
	}
	target, pathErr := s.filesystemProxyPath(backend.FilesystemFenceID(adoption))
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if !mounter.mounted[target] {
		t.Fatal("failed rollback detached a busy mount")
	}
	removeErr := s.removeFilesystemProxy(ctx, "pool/volume", adoption, &mark)
	if removeErr == nil || !strings.Contains(removeErr.Error(), "device busy") {
		t.Fatalf("remove error = %v, want busy failure", removeErr)
	}
	if !mounter.mounted[target] {
		t.Fatal("remove detached a busy mount")
	}
}

func TestFilesystemProxyRejectsSourceTargetAndPreservesEmptyDirectory(t *testing.T) {
	fixture := newProxyLifecycleFixture(t)
	s := fixture.server
	b := fixture.backend
	adoption := fixture.adoption
	params := fixture.params
	ctx := context.Background()
	if _, err := s.ImportVolume(ctx, proxyImportRequest(adoption, params, "owner")); err != nil {
		t.Fatal(err)
	}
	mark, exists, markErr := s.readFencingMark(backend.FilesystemFenceID(adoption))
	if markErr != nil || !exists {
		t.Fatalf("read durable import mark: exists=%v err=%v", exists, markErr)
	}
	target, targetErr := s.filesystemProxyPath(backend.FilesystemFenceID(adoption))
	if targetErr != nil {
		t.Fatal(targetErr)
	}
	mkdirParentErr := ensureDirectoryPath(filepath.Dir(target))
	if mkdirParentErr != nil {
		t.Fatal(mkdirParentErr)
	}
	mkdirTargetErr := os.Mkdir(target, 0o750)
	if mkdirTargetErr != nil {
		t.Fatal(mkdirTargetErr)
	}
	adoption.CanonicalSource = target
	b.source = target
	_, ensureErr := s.ensureFilesystemProxy(
		ctx,
		"pool/volume",
		&proxyTestPin{backend: b},
		&mark,
	)
	if ensureErr == nil {
		t.Fatal("source equal to proxy target was accepted")
	}
	removeErr := s.removeFilesystemProxy(ctx, "pool/volume", adoption, &mark)
	if removeErr == nil {
		t.Fatal("cleanup accepted source equal to proxy target")
	}
	entries, readDirErr := os.ReadDir(target)
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	if len(entries) != 0 {
		t.Fatalf("source-target directory was changed: %d entries", len(entries))
	}
}

// TestEnsureProxyTargetAccessBoundaryUnderRestrictiveUmask proves that the
// access boundary of a newly created proxy path does not depend on the agent's
// umask: driver-created parents stay traversable by other users (NFS
// root-squashed clients), the unmounted target denies other users, and a
// pre-existing private ancestor is not relaxed. The umask is set only inside a
// re-executed helper process so parallel tests never observe it.
func TestEnsureProxyTargetAccessBoundaryUnderRestrictiveUmask(t *testing.T) {
	t.Parallel()
	base := canonicalTempDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], //nolint:gosec // G204: re-executes own test binary as a helper
		"-test.run=^TestEnsureProxyTargetRestrictiveUmaskHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		"PILLAR_TEST_PROXY_UMASK_HELPER=1",
		"PILLAR_TEST_PROXY_UMASK_BASE="+base,
	)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("restrictive-umask helper failed: %v\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestEnsureProxyTargetRestrictiveUmaskHelper") {
		t.Fatalf("restrictive-umask helper did not run:\n%s", out)
	}
}

// TestEnsureProxyTargetRestrictiveUmaskHelper runs only in the subprocess
// started above; without the marker environment variable it is a no-op.
func TestEnsureProxyTargetRestrictiveUmaskHelper(t *testing.T) {
	base := os.Getenv("PILLAR_TEST_PROXY_UMASK_BASE")
	if os.Getenv("PILLAR_TEST_PROXY_UMASK_HELPER") == "" || base == "" {
		return
	}
	syscall.Umask(0o077)
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(existing, "proxies")
	target := filepath.Join(root, "filesystem", "native")
	created, err := ensureProxyTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("new filesystem proxy target was not created")
	}
	assertProxyAccessBoundary(t, existing, []string{root, filepath.Dir(target)}, target)
	again, againErr := ensureProxyTarget(target)
	if againErr != nil || again {
		t.Fatalf("existing proxy target re-ensure = (%v, %v), want (false, nil)", again, againErr)
	}
}

// assertProxyAccessBoundary checks that other users can traverse the
// driver-created parents, cannot access the unmounted target, and that the
// pre-existing private ancestor kept its 0700 mode.
func assertProxyAccessBoundary(t *testing.T, existing string, createdParents []string, target string) {
	t.Helper()
	const otherTraverse, otherAny, groupWrite = 0o001, 0o007, 0o020
	for _, path := range createdParents {
		if perm := lstatPerm(t, path); perm&otherTraverse == 0 {
			t.Fatalf("driver-created parent %q mode %04o denies traversal to other users", path, perm)
		}
	}
	if perm := lstatPerm(t, target); perm&otherAny != 0 || perm&groupWrite != 0 {
		t.Fatalf("unmounted proxy target mode %04o grants access beyond owner and group read", perm)
	}
	if perm := lstatPerm(t, existing); perm != 0o700 {
		t.Fatalf("pre-existing ancestor mode changed to %04o", perm)
	}
}

func lstatPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
