package agent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/zfs"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
)

type filesystemTestBackend struct {
	*mockBackend
	adoption   *agentv1.FilesystemAdoption
	capacity   int64
	inspectErr error
	pinErr     error
	closeErr   error
	closed     bool
}

func (b *filesystemTestBackend) InspectImport(
	_ context.Context, _ string, required int64, layout backend.Layout,
) (*backend.ImportInspection, error) {
	if b.inspectErr != nil {
		return nil, b.inspectErr
	}
	if layout.HostRoot != b.layout.HostRoot || required != b.capacity {
		return nil, &backend.ImportRefusedError{Reason: "quota or layout drift"}
	}
	return &backend.ImportInspection{Filesystem: b.adoption, CapacityBytes: b.capacity}, nil
}
func (b *filesystemTestBackend) ImportFilesystem(
	_ context.Context, _ string, required int64, a *agentv1.FilesystemAdoption, layout backend.Layout,
) (backend.PinnedFilesystem, error) {
	if b.pinErr != nil {
		return nil, b.pinErr
	}
	if layout.HostRoot != b.layout.HostRoot || required != b.capacity || !proto.Equal(a, b.adoption) {
		return nil, &backend.ImportRefusedError{Reason: "source identity or exact quota changed"}
	}
	b.closed = false
	return &filesystemTestPin{b: b}, nil
}

type filesystemTestPin struct{ b *filesystemTestBackend }

func (p *filesystemTestPin) Adoption() *agentv1.FilesystemAdoption   { return p.b.adoption }
func (p *filesystemTestPin) CapacityBytes() int64                    { return p.b.capacity }
func (p *filesystemTestPin) MountSource() string                     { return p.b.adoption.GetCanonicalSource() }
func (*filesystemTestPin) VerifyMount(context.Context, string) error { return nil }
func (p *filesystemTestPin) Close() error                            { p.b.closed = true; return p.b.closeErr }

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temporary directory: %v", err)
	}
	return root
}

type filesystemTestFixture struct {
	server  *agent.Server
	backend *filesystemTestBackend
	state   string
	request *agentv1.ImportVolumeRequest
}

func filesystemFixture(t *testing.T) *filesystemTestFixture {
	t.Helper()
	a := &agentv1.FilesystemAdoption{
		Kind: "directory", CanonicalSource: "/existing/data", ResourceId: "uuid:42",
		FilesystemType: "ext4", FilesystemId: "uuid", Inode: 42, ProjectId: 7,
	}
	b := &filesystemTestBackend{
		mockBackend: &mockBackend{
			backendType: agentv1.BackendType_BACKEND_TYPE_DIRECTORY,
			layout:      backend.Layout{HostRoot: "/existing"}, backingResourcePresent: true,
		},
		adoption: a, capacity: 1 << 20,
	}
	state := canonicalTempDir(t)
	srv := agent.NewServer(
		map[string]backend.VolumeBackend{"one": b, "two": b}, "", agent.WithDrainStateDir(state),
		agent.WithFilesystemProxy(filepath.Join(state, "proxies")),
	)
	req := &agentv1.ImportVolumeRequest{
		VolumeId: "one/native", BackendType: agentv1.BackendType_BACKEND_TYPE_DIRECTORY,
		CapacityBytes: b.capacity, FilesystemAdoption: a,
		Fence: &agentv1.FencingToken{VolumeUid: "owner", Generation: 1},
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Directory{
				Directory: &agentv1.DirectoryVolumeParams{LogicalPool: "one", HostRoot: "/existing"},
			},
		},
	}
	return &filesystemTestFixture{server: srv, backend: b, state: state, request: req}
}

func TestFilesystemInspectDoesNotClaimResource(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, state := fixture.server, fixture.backend, fixture.state
	got, err := srv.InspectImport(context.Background(), &agentv1.InspectImportRequest{
		PoolName: "one", Source: "/existing/data", BackendType: agentv1.BackendType_BACKEND_TYPE_DIRECTORY,
		RequiredBytes: b.capacity, ExpectedHostRoot: "/existing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got.GetFilesystemAdoption(), b.adoption) || got.GetCapacityBytes() != b.capacity {
		t.Fatalf("inspection lost native identity or exact capacity: %v", got)
	}
	if _, err = os.Stat(filepath.Join(state, "generations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only inspection created ownership: %v", err)
	}
}

func TestFilesystemImportAliasesShareNativeFenceAcrossRestart(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, state, req := fixture.server, fixture.backend, fixture.state, fixture.request
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !b.closed {
		t.Fatal("import response escaped with native pin open")
	}
	alias, ok := proto.Clone(req).(*agentv1.ImportVolumeRequest)
	if !ok {
		t.Fatal("cloned import request has an unexpected type")
	}
	alias.VolumeId = "two/other-routing-name"
	alias.BackendParams.GetDirectory().LogicalPool = "two"
	alias.Fence.VolumeUid = "different-owner"
	restarted := agent.NewServer(
		map[string]backend.VolumeBackend{"two": b}, "", agent.WithDrainStateDir(state),
		agent.WithFilesystemProxy(filepath.Join(state, "proxies")),
	)
	if _, err := restarted.ImportVolume(context.Background(), alias); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("pool alias seized same filesystem: %v", err)
	}
}

func TestFilesystemRefusedImportLeavesSourceAvailable(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, state, req := fixture.server, fixture.backend, fixture.state, fixture.request
	b.pinErr = &backend.ImportRefusedError{Reason: "native project quota changed"}
	if _, err := srv.ImportVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("quota drift admitted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "generations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed inspection persisted ownership: %v", err)
	}
	b.pinErr = nil
	req.Fence.VolumeUid = "another-owner"
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatalf("refused import left phantom owner: %v", err)
	}
	if !b.backingResourcePresent {
		t.Fatal("read-only import changed original source")
	}
}

func TestFilesystemTeardownRejectsChangedDescriptor(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, req := fixture.server, fixture.request
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	replacement, ok := proto.Clone(req.GetFilesystemAdoption()).(*agentv1.FilesystemAdoption)
	if !ok {
		t.Fatal("cloned filesystem adoption has an unexpected type")
	}
	replacement.CanonicalSource = "/existing/replacement"
	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId: req.VolumeId, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
		FilesystemAdoption: replacement, Fence: req.Fence,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("teardown accepted substituted descriptor: %v", err)
	}
}

func TestFilesystemReconcileQuotaDriftDoesNotCreateSource(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, req := fixture.server, fixture.backend, fixture.request
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b.capacity *= 2
	result, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Complete: true,
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId: req.VolumeId, BackendType: req.BackendType, BackendParams: req.BackendParams,
			FilesystemAdoption: req.FilesystemAdoption, CapacityBytes: req.CapacityBytes, Fence: req.Fence,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Success {
		t.Fatalf("quota drift recovered as success: %v", result)
	}
	if len(b.createCalledWith) != 0 || !b.backingResourcePresent {
		t.Fatal("adoption recovery recreated or destroyed original source")
	}
}

func TestFilesystemPinCloseFailureReachesCaller(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, req := fixture.server, fixture.backend, fixture.request
	b.closeErr = errors.New("close native resource")
	if _, err := srv.ImportVolume(context.Background(), req); status.Code(err) != codes.Internal {
		t.Fatalf("pin close failure silently ignored: %v", err)
	}
}

func TestFilesystemInvalidDescriptorDoesNotUseLegacyFence(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, state, req := fixture.server, fixture.backend, fixture.state, fixture.request
	adoption, ok := proto.Clone(req.FilesystemAdoption).(*agentv1.FilesystemAdoption)
	if !ok {
		t.Fatal("cloned filesystem adoption has an unexpected type")
	}
	req.FilesystemAdoption = adoption
	req.FilesystemAdoption.ResourceId = ""
	if _, err := srv.ImportVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid native identity admitted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "generations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid identity fell back to routing fence: %v", err)
	}
	if !b.backingResourcePresent {
		t.Fatal("invalid descriptor changed source")
	}
}

func TestFilesystemMissingSourceRecoveryNeverRecreates(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, req := fixture.server, fixture.backend, fixture.request
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b.pinErr = &backend.ImportRefusedError{Reason: "source missing"}
	b.backingResourcePresent = false
	result, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Complete: true,
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId: req.VolumeId, BackendType: req.BackendType, BackendParams: req.BackendParams,
			FilesystemAdoption: req.FilesystemAdoption, CapacityBytes: req.CapacityBytes, Fence: req.Fence,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Success {
		t.Fatalf("missing original source recovered as success: %v", result)
	}
	if len(b.createCalledWith) != 0 || b.backingResourcePresent {
		t.Fatal("missing adopted filesystem was recreated")
	}
}

func TestFilesystemImportCannotChangeRecordedQuotaContract(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, req := fixture.server, fixture.backend, fixture.request
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b.capacity *= 2
	req.CapacityBytes = b.capacity
	req.Fence.Generation++
	if _, err := srv.ImportVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("same lifecycle silently accepted a changed quota contract: %v", err)
	}
}

func (b *filesystemTestBackend) ExistingFilesystemIdentity(
	_ context.Context, _ string,
) (*agentv1.FilesystemAdoption, error) {
	if !b.backingResourcePresent {
		return nil, nil //nolint:nilnil // A missing source has no native filesystem identity.
	}
	return b.adoption, nil
}

func zfsFilesystemFixture(t *testing.T) (*agent.Server, *filesystemTestBackend, *agentv1.ImportVolumeRequest) {
	t.Helper()
	fixture := filesystemFixture(t)
	b, state, req := fixture.backend, fixture.state, fixture.request
	b.backendType = agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
	b.layout = backend.Layout{ParentDataset: "k8s"}
	b.adoption = &agentv1.FilesystemAdoption{
		Kind: "zfs-dataset", CanonicalSource: "one/k8s/existing", ResourceId: "42", FilesystemType: "zfs",
	}
	b.createDevicePath = "/var/lib/pillar-csi/agent/datasets/one/k8s/existing"
	b.createAllocated = b.capacity
	req.BackendType = b.backendType
	req.FilesystemAdoption = b.adoption
	req.BackendParams = &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Zfs{Zfs: &agentv1.ZfsVolumeParams{ParentDataset: "k8s"}},
	}
	srv := agent.NewServer(
		map[string]backend.VolumeBackend{"one": b}, "", agent.WithDrainStateDir(state),
		agent.WithFilesystemProxy(filepath.Join(state, "proxies")),
	)
	return srv, b, req
}

func TestFilesystemAdoptionRejectsLiveManagedDatasetOwner(t *testing.T) {
	t.Parallel()
	srv, b, req := zfsFilesystemFixture(t)
	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId: "one/existing", BackendType: b.backendType, CapacityBytes: b.capacity,
		BackendParams: req.BackendParams, Fence: &agentv1.FencingToken{VolumeUid: "managed", Generation: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = srv.InspectImport(context.Background(), &agentv1.InspectImportRequest{
		PoolName: "one", Source: b.adoption.CanonicalSource, BackendType: b.backendType,
		RequiredBytes: b.capacity, ExpectedParentDataset: "k8s",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("inspection accepted managed source: %v", err)
	}
	if _, err = srv.ImportVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("file lifecycle seized managed native dataset: %v", err)
	}
	if !b.backingResourcePresent {
		t.Fatal("refused adoption destroyed managed source")
	}
}

func TestManagedDatasetCreateCannotClaimLiveFileNativeOwner(t *testing.T) {
	t.Parallel()
	srv, b, req := zfsFilesystemFixture(t)
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId: "one/existing", BackendType: b.backendType, CapacityBytes: b.capacity,
		BackendParams: req.BackendParams, Fence: &agentv1.FencingToken{VolumeUid: "managed", Generation: 1},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("managed Create seized adopted native source: %v", err)
	}
	if len(b.createCalledWith) != 0 || !b.backingResourcePresent {
		t.Fatal("managed Create touched adopted source")
	}
	if _, err = srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatalf("refused managed operation left a phantom old-name owner: %v", err)
	}
}

func TestManagedDatasetExpandCannotMutateLiveFileNativeOwner(t *testing.T) {
	t.Parallel()
	srv, b, req := zfsFilesystemFixture(t)
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId: "one/existing", BackendType: b.backendType, RequestedBytes: b.capacity * 2,
		Fence: &agentv1.FencingToken{VolumeUid: "managed", Generation: 1},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("managed expansion admitted adopted source: %v", err)
	}
	if _, err = srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatalf("refused expansion left a phantom old-name owner: %v", err)
	}
}

func TestManagedDatasetDeleteCannotDestroyLiveFileNativeOwner(t *testing.T) {
	t.Parallel()
	_, b, req := zfsFilesystemFixture(t)
	manager, err := nfs.NewManager(nfs.Config{
		StateDir: canonicalTempDir(t), BindAddress: "192.0.2.10", ExportRoot: canonicalTempDir(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	state := canonicalTempDir(t)
	srv := agent.NewServer(
		map[string]backend.VolumeBackend{"one": b}, "", agent.WithDrainStateDir(state),
		agent.WithNFSManager(manager), agent.WithFilesystemProxy(filepath.Join(state, "proxies")),
	)
	agent.SetNFSUnexportTestHandler(t, srv)
	if _, err = srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_, err = srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: "one/existing", BackendType: b.backendType,
		Fence: &agentv1.FencingToken{VolumeUid: "managed", Generation: 1},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("managed Delete admitted adopted source: %v", err)
	}
	if len(b.deleteCalledWith) != 0 || !b.backingResourcePresent {
		t.Fatal("managed Delete destroyed adopted source")
	}
	if _, err = srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatalf("refused Delete left a phantom old-name owner: %v", err)
	}
}

type physicalFilesystemZvolBackend struct{ *filesystemTestBackend }

func (*physicalFilesystemZvolBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

func TestOldZvolDeleteCannotDestroyAdoptedPhysicalFilesystem(t *testing.T) {
	t.Parallel()
	_, b, req := zfsFilesystemFixture(t)
	old := &physicalFilesystemZvolBackend{filesystemTestBackend: b}
	variants := map[string]map[agentv1.BackendType]backend.VolumeBackend{
		"one": {
			agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET: b,
			agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL:    old,
		},
	}
	state := canonicalTempDir(t)
	srv := agent.NewServer(
		map[string]backend.VolumeBackend{"one": old}, "", agent.WithDrainStateDir(state),
		agent.WithBackendVariants(variants), agent.WithFilesystemProxy(filepath.Join(state, "proxies")),
	)
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: "one/existing", BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence: &agentv1.FencingToken{VolumeUid: "old-block", Generation: 1},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("typed zvol Delete destroyed a filesystem-kind native source: %v", err)
	}
	if !b.backingResourcePresent {
		t.Fatal("adopted original source destroyed through zvol backend")
	}
	if _, err = srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatalf("refused zvol Delete left a phantom name owner: %v", err)
	}
}

type realZvolTestBackend struct {
	*mockBackend
	native *zfs.Backend
}

func (b *realZvolTestBackend) ExistingFilesystemIdentity(
	ctx context.Context, volumeID string,
) (*agentv1.FilesystemAdoption, error) {
	return b.native.ExistingFilesystemIdentity(ctx, volumeID)
}

func TestRealOldZvolDeleteStillWorksWithAnActiveFileOwner(t *testing.T) {
	t.Parallel()
	_, b, req := zfsFilesystemFixture(t)
	old := &realZvolTestBackend{
		mockBackend: &mockBackend{backendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL, backingResourcePresent: true},
		native: zfs.NewWithExecFn("one", "k8s", func(
			_ context.Context, _ string, args ...string,
		) ([]byte, error) {
			if len(args) != 6 || args[0] != "get" || args[4] != "type,guid" {
				return nil, errors.New("unexpected native identity query")
			}
			return []byte("type\tvolume\nguid\t77\n"), nil
		}),
	}
	variants := map[string]map[agentv1.BackendType]backend.VolumeBackend{
		"one": {
			agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET: b,
			agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL:    old,
		},
	}
	state := canonicalTempDir(t)
	srv := agent.NewServer(
		map[string]backend.VolumeBackend{"one": old}, "", agent.WithDrainStateDir(state),
		agent.WithBackendVariants(variants), agent.WithFilesystemProxy(filepath.Join(state, "proxies")),
	)
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: "one/real-zvol", BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence: &agentv1.FencingToken{VolumeUid: "old-block", Generation: 1},
	})
	if err != nil {
		t.Fatalf("legitimate old zvol deletion changed: %v", err)
	}
	if old.backingResourcePresent || !b.backingResourcePresent {
		t.Fatal("old zvol deletion failed or changed the unrelated file source")
	}
}

func TestFilesystemFirstClaimRepeatsDeepQuotaProofBeforeOwnership(t *testing.T) {
	t.Parallel()
	fixture := filesystemFixture(t)
	srv, b, state, req := fixture.server, fixture.backend, fixture.state, fixture.request
	_, err := srv.InspectImport(context.Background(), &agentv1.InspectImportRequest{
		PoolName: "one", Source: b.adoption.CanonicalSource, BackendType: b.backendType,
		RequiredBytes: b.capacity, ExpectedHostRoot: b.layout.HostRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The shallow pin still matches, but the native deep project-scope proof
	// changes between pre-reservation inspection and the first ownership claim.
	b.inspectErr = &backend.ImportRefusedError{Reason: "project quota scope reused outside source"}
	if _, err = srv.ImportVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("first claim trusted stale deep inspection: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(state, "generations")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed first-claim proof persisted ownership: %v", statErr)
	}
	if !b.backingResourcePresent {
		t.Fatal("first-claim refusal changed original source")
	}
}
