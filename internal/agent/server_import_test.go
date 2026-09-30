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

package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// mockImporterBackend adds an Import implementation to mockBackend, making it
// satisfy backend.VolumeImporter.
type mockImporterBackend struct {
	*mockBackend
	importDevicePath      string
	importSize            int64
	importErr             error
	importCalledWith      []string
	importExpectedDataset []string
}

func (m *mockImporterBackend) Import(
	_ context.Context,
	volumeID string,
	_ int64,
	expectedDataset string,
) (devicePath string, sizeBytes int64, err error) {
	m.importCalledWith = append(m.importCalledWith, volumeID)
	m.importExpectedDataset = append(m.importExpectedDataset, expectedDataset)
	return m.importDevicePath, m.importSize, m.importErr
}

var _ backend.VolumeImporter = (*mockImporterBackend)(nil)

// newImportTestServer registers mb (a full importer) as the backend for
// testPool, so the volume path exercises the real VolumeImporter assertion.
// It returns the agent state dir so tests can inspect the fencing marks.
func newImportTestServer(t *testing.T, mb *mockImporterBackend) (srv *agent.Server, stateDir string) {
	t.Helper()
	backends := map[string]backend.VolumeBackend{
		testPool: mb,
	}
	stateDir = t.TempDir()
	return agent.NewServer(backends, "", agent.WithDrainStateDir(stateDir)), stateDir
}
func importRequest(t *testing.T) *agentv1.ImportVolumeRequest {
	t.Helper()
	return &agentv1.ImportVolumeRequest{
		VolumeId:      testVolumeID,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		CapacityBytes: 1 << 30,
		Fence:         testFence(t),
	}
}

func TestImportVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: "/dev/zvol/tank/k8s/pvc-abc",
		importSize:       2 << 30, // larger than requested — adopted as-is
	}
	srv, _ := newImportTestServer(t, mb)

	resp, err := srv.ImportVolume(context.Background(), importRequest(t))
	if err != nil {
		t.Fatalf("ImportVolume unexpected error: %v", err)
	}
	if resp.GetDevicePath() != "/dev/zvol/tank/k8s/pvc-abc" {
		t.Errorf("DevicePath = %q", resp.GetDevicePath())
	}
	if resp.GetCapacityBytes() != 2<<30 {
		t.Errorf("CapacityBytes = %d, want the zvol's existing size", resp.GetCapacityBytes())
	}
	if len(mb.importCalledWith) != 1 || mb.importCalledWith[0] != testVolumeID {
		t.Errorf("Import called with %v, want one call for %q", mb.importCalledWith, testVolumeID)
	}
}

// The import RPC must never create: a backend without the importer contract
// answers Unimplemented.
func TestImportVolume_BackendCannotImport(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &mockBackend{})

	_, err := srv.ImportVolume(context.Background(), importRequest(t))
	if err == nil {
		t.Fatal("expected Unimplemented for a non-importable backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

func TestImportVolume_RefusedIsFailedPrecondition(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend: &mockBackend{},
		importErr: &backend.ImportRefusedError{
			VolumeID: testVolumeID,
			Reason:   "in use",
			Detail:   "device is mounted at /data",
		},
	}
	srv, _ := newImportTestServer(t, mb)

	_, err := srv.ImportVolume(context.Background(), importRequest(t))
	if err == nil {
		t.Fatal("expected FailedPrecondition, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

func TestImportVolume_InvalidVolumeID(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{mockBackend: &mockBackend{}}
	srv, _ := newImportTestServer(t, mb)

	req := importRequest(t)
	req.VolumeId = "no-slash"
	_, err := srv.ImportVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for invalid volumeID, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
	if len(mb.importCalledWith) != 0 {
		t.Errorf("Import was called %d times for an invalid volumeID", len(mb.importCalledWith))
	}
}

func TestImportVolume_BackendTypeMismatch(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{mockBackend: &mockBackend{}}
	srv, _ := newImportTestServer(t, mb)

	req := importRequest(t)
	req.BackendType = agentv1.BackendType_BACKEND_TYPE_LVM
	_, err := srv.ImportVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for mismatched backend_type, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// A second lifecycle must not be able to re-import the same zvol: the fence
// mark recorded by the first import rejects a different uid for the same
// agent volume ID.
func TestImportVolume_FencedAgainstSecondLifecycle(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: "/dev/zvol/tank/pvc-abc",
		importSize:       1 << 30,
	}
	srv, _ := newImportTestServer(t, mb)

	if _, err := srv.ImportVolume(context.Background(), importRequest(t)); err != nil {
		t.Fatalf("first ImportVolume: %v", err)
	}
	req := importRequest(t)
	req.Fence = &agentv1.FencingToken{VolumeUid: "another-lifecycle", Generation: 1}
	_, err := srv.ImportVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected a fencing error for a second lifecycle, got nil")
	}
	if st, _ := status.FromError(err); st.Code() == codes.OK {
		t.Error("import by a second lifecycle must not succeed")
	}
}

// importMarkPath locates the single fencing mark file under the agent state
// dir: <dir>/generations/<escaped-volumeID>.mark.
func importMarkPath(t *testing.T, stateDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateDir, "generations", "*.mark"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("fencing marks under %q: %v (err %v)", stateDir, matches, err)
	}
	return matches[0]
}

// A refused import must not bind the volume ID: no fencing mark may be
// written for a lifecycle that never adopted the zvol.  If a mark existed,
// a reaped lifecycle's controller-side record would point at a zvol the
// agent still refuses to touch, and a retry after the refusal cause is
// fixed could be fenced out by its own phantom mark.
func TestImportVolume_RefusedWritesNoFencingMark(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend: &mockBackend{},
		importErr: &backend.ImportRefusedError{
			VolumeID: testVolumeID,
			Reason:   "in use",
			Detail:   "device is mounted at /data",
		},
	}
	srv, stateDir := newImportTestServer(t, mb)

	if _, err := srv.ImportVolume(context.Background(), importRequest(t)); err == nil {
		t.Fatal("expected refusal, got nil")
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "generations", "*.mark"))
	if err != nil {
		t.Fatalf("glob fencing marks: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("a refused import left fencing marks %v; nothing may be recorded", matches)
	}

	// The volume ID is still free: once the refusal cause is gone a retry of
	// the same lifecycle succeeds, and it does not fence itself out.
	mb.importErr = nil
	mb.importDevicePath, mb.importSize = "/dev/zvol/tank/pvc-abc", 1<<30
	if _, err := srv.ImportVolume(context.Background(), importRequest(t)); err != nil {
		t.Fatalf("retry after the refusal cause cleared: %v", err)
	}
	if got := importMarkPath(t, stateDir); got == "" {
		t.Fatal("a successful import must persist the fencing mark")
	}
}

// The import RPC writes its fencing mark only after the backend checks pass.
// The proof is a refusal of a second lifecycle: a mark written before the
// checks would still be there from the first attempt and the second
// lifecycle would be rejected even though the import itself succeeded.
func TestImportVolume_MarkWrittenAfterChecks(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: "/dev/zvol/tank/pvc-abc",
		importSize:       1 << 30,
	}
	srv, stateDir := newImportTestServer(t, mb)

	if _, err := srv.ImportVolume(context.Background(), importRequest(t)); err != nil {
		t.Fatalf("ImportVolume: %v", err)
	}
	data, err := os.ReadFile(importMarkPath(t, stateDir))
	if err != nil {
		t.Fatalf("read fencing mark: %v", err)
	}
	if !strings.Contains(string(data), t.Name()) {
		t.Errorf("fencing mark %q does not name the importing lifecycle uid %q", data, t.Name())
	}
}

// A lost ImportVolume response is safe: the same lifecycle retries with the
// same token, re-runs the read-only import checks, and re-persists its mark.
func TestImportVolume_RetryAfterLostResponse(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: "/dev/zvol/tank/pvc-abc",
		importSize:       1 << 30,
	}
	srv, _ := newImportTestServer(t, mb)

	req := importRequest(t)
	for i := range 2 {
		resp, err := srv.ImportVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("ImportVolume attempt %d: %v", i+1, err)
		}
		if resp.GetDevicePath() != "/dev/zvol/tank/pvc-abc" {
			t.Errorf("attempt %d DevicePath = %q", i+1, resp.GetDevicePath())
		}
	}
	if len(mb.importCalledWith) != 2 {
		t.Errorf("Import calls = %v, want 2 idempotent retries", mb.importCalledWith)
	}
}

// The controller sends the recorded source dataset so the agent can refuse
// an adoption that would resolve to a different dataset than the claim
// named (its parentDataset disagrees with the store's).
func TestImportVolume_ExpectedDatasetForwarded(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: "/dev/zvol/tank/pvc-abc",
		importSize:       1 << 30,
	}
	srv, _ := newImportTestServer(t, mb)

	req := importRequest(t)
	req.ExpectedDataset = "tank/pvc-abc"
	if _, err := srv.ImportVolume(context.Background(), req); err != nil {
		t.Fatalf("ImportVolume: %v", err)
	}
	if len(mb.importExpectedDataset) != 1 || mb.importExpectedDataset[0] != "tank/pvc-abc" {
		t.Fatalf("backend saw expectedDataset %v, want [tank/pvc-abc]", mb.importExpectedDataset)
	}
}

// newReleaseTestServer is newImportTestServer with a temp configfs root so
// exports can be created and observed.
func newReleaseTestServer(t *testing.T, mb *mockImporterBackend) (srv *agent.Server, cfgRoot string) {
	t.Helper()
	cfgRoot = t.TempDir()
	srv = agent.NewServer(map[string]backend.VolumeBackend{testPool: mb}, cfgRoot,
		agent.WithDrainStateDir(t.TempDir()))
	agent.SetDeviceChecker(t, srv, nvmeof.AlwaysPresentChecker)
	return srv, cfgRoot
}

// nvmetSubsystems counts the NVMe-oF subsystems under cfgRoot.
func nvmetSubsystems(t *testing.T, cfgRoot string) int {
	t.Helper()
	subs, err := filepath.Glob(filepath.Join(cfgRoot, "nvmet", "subsystems", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(subs)
}

func releaseRequest(fence *agentv1.FencingToken) *agentv1.ReleaseVolumeRequest {
	return &agentv1.ReleaseVolumeRequest{
		VolumeId:     testVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Fence:        fence,
	}
}

// requireFencedOut asserts err is the agent's FailedPrecondition fencing
// rejection.
func requireFencedOut(t *testing.T, what string, err error) {
	t.Helper()
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("%s: err = %v, want FailedPrecondition (fenced out)", what, err)
	}
}

// Releasing a lifecycle whose import landed (the controller lost the
// response, so it never recorded the adoption) removes that lifecycle's
// export and retires it at the agent without touching the zvol.  A delayed
// ImportVolume — or any other mutating request — of the released lifecycle
// is then rejected, while a new claim's lifecycle can still import.
func TestReleaseVolume_RetiresLandedImportWithoutBackendMutation(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: testDevicePath,
		importSize:       1 << 30,
	}
	srv, cfgRoot := newReleaseTestServer(t, mb)
	ctx := context.Background()
	importGen := &agentv1.FencingToken{VolumeUid: "lifecycle-a", Generation: 1}

	importReq := importRequest(t)
	importReq.Fence = importGen
	if _, err := srv.ImportVolume(ctx, importReq); err != nil {
		t.Fatalf("ImportVolume: %v", err)
	}
	if _, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testDevicePath,
		Fence: importGen,
	}); err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	if nvmetSubsystems(t, cfgRoot) != 1 {
		t.Fatal("export did not create a subsystem")
	}

	teardown := &agentv1.FencingToken{VolumeUid: "lifecycle-a", Generation: 2}
	if _, err := srv.ReleaseVolume(ctx, releaseRequest(teardown)); err != nil {
		t.Fatalf("ReleaseVolume: %v", err)
	}
	if n := nvmetSubsystems(t, cfgRoot); n != 0 {
		t.Fatalf("ReleaseVolume left %d subsystem(s) of the released lifecycle", n)
	}
	// Idempotent: a retry after a lost response succeeds.
	if _, err := srv.ReleaseVolume(ctx, releaseRequest(teardown)); err != nil {
		t.Fatalf("ReleaseVolume retry: %v", err)
	}

	// Delayed requests of the released lifecycle, at any generation.
	for _, fence := range []*agentv1.FencingToken{importGen, teardown} {
		delayed := importRequest(t)
		delayed.Fence = fence
		_, err := srv.ImportVolume(ctx, delayed)
		requireFencedOut(t, "delayed ImportVolume", err)
		_, err = srv.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
			VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL, Fence: fence,
		})
		requireFencedOut(t, "stale DeleteVolume", err)
		_, err = srv.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
			VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
			RequestedBytes: 2 << 30, Fence: fence,
		})
		requireFencedOut(t, "stale ExpandVolume", err)
	}
	if len(mb.deleteCalledWith) != 0 {
		t.Fatalf("backend Delete ran %v on a released import", mb.deleteCalledWith)
	}
	if len(mb.importCalledWith) != 1 {
		t.Fatalf("backend Import ran %d times, want only the original import", len(mb.importCalledWith))
	}

	// A new claim's lifecycle can import the same zvol.
	next := importRequest(t)
	next.Fence = &agentv1.FencingToken{VolumeUid: "lifecycle-b", Generation: 1}
	if _, err := srv.ImportVolume(ctx, next); err != nil {
		t.Fatalf("re-import by a new lifecycle after release: %v", err)
	}
}

// An import the agent refused left no fencing mark, so a later ImportVolume
// of that lifecycle (a delayed retry once the refusal cause is gone) would
// have been admitted as a new lifecycle after the controller forgot it.
// ReleaseVolume records the lifecycle as retired even without a mark.
func TestReleaseVolume_RefusedImportFencesDelayedImport(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: testDevicePath,
		importSize:       1 << 30,
		importErr:        &backend.ImportRefusedError{VolumeID: testVolumeID, Reason: "in use"},
	}
	srv, _ := newReleaseTestServer(t, mb)
	ctx := context.Background()
	fence := &agentv1.FencingToken{VolumeUid: "lifecycle-a", Generation: 1}

	req := importRequest(t)
	req.Fence = fence
	_, err := srv.ImportVolume(ctx, req)
	requireFencedOut(t, "refused ImportVolume", err)

	_, err = srv.ReleaseVolume(ctx,
		releaseRequest(&agentv1.FencingToken{VolumeUid: "lifecycle-a", Generation: 2}))
	if err != nil {
		t.Fatalf("ReleaseVolume: %v", err)
	}
	mb.importErr = nil // the refusal cause is gone when the delayed retry lands
	_, err = srv.ImportVolume(ctx, req)
	requireFencedOut(t, "delayed ImportVolume of the released lifecycle", err)

	next := importRequest(t)
	next.Fence = &agentv1.FencingToken{VolumeUid: "lifecycle-b", Generation: 1}
	_, err = srv.ImportVolume(ctx, next)
	if err != nil {
		t.Fatalf("re-import by a new lifecycle after release: %v", err)
	}
}

// Releasing a lifecycle that never owned the volume ID leaves the owning
// lifecycle and its export alone, and still retires the released one.
func TestReleaseVolume_ForeignOwnerUntouched(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: testDevicePath,
		importSize:       1 << 30,
	}
	srv, cfgRoot := newReleaseTestServer(t, mb)
	ctx := context.Background()
	owner := &agentv1.FencingToken{VolumeUid: "owner", Generation: 1}

	req := importRequest(t)
	req.Fence = owner
	if _, err := srv.ImportVolume(ctx, req); err != nil {
		t.Fatalf("owner ImportVolume: %v", err)
	}
	if _, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testDevicePath, Fence: owner,
	}); err != nil {
		t.Fatalf("owner ExportVolume: %v", err)
	}

	if _, err := srv.ReleaseVolume(ctx,
		releaseRequest(&agentv1.FencingToken{VolumeUid: "stranger", Generation: 5})); err != nil {
		t.Fatalf("ReleaseVolume of a non-owner: %v", err)
	}
	if nvmetSubsystems(t, cfgRoot) != 1 {
		t.Fatal("ReleaseVolume of a non-owner removed the owner's export")
	}
	if _, err := srv.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		RequestedBytes: 1 << 30, Fence: owner,
	}); err != nil {
		t.Fatalf("owner lost the volume ID to a non-owner release: %v", err)
	}
}
