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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// LVM adoption (issue #163).
//
// The agent pins an adopted LV's identity and preservation policy for the
// volume ID.  The tests drive the public RPCs against a stateful LVM backend
// fake and prove the pin by what later RPCs, also after an agent restart on
// the same state dir, may or may not do to the LV.  Fixtures seed the durable
// mark as an earlier agent run would have written it.

const (
	testLVName       = "pvc-abc"
	testLVDevicePath = "/dev/" + testPool + "/" + testLVName
	testVGUUID       = "VGuuid-0000-1111-2222-3333-4444-555555"
	testLVUUID       = "LVuuid-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	otherUUID        = "Otheru-9999-8888-7777-6666-5555-444444"
)

// mockLVBackend is an LVM backend that adopts existing LVs: it implements
// backend.LVImporter and backend.LVVerifier but not backend.VolumeImporter.
// The lvs map holds the LV currently behind each volume ID; ImportLV and
// VerifyLV refuse ("missing" or ImportRefusedReasonIdentity) unless want
// names exactly that LV.  The embedded mockBackend keeps the LV's presence
// and records every Create/Delete/Expand, so a test can prove the LV was
// never touched.
type mockLVBackend struct {
	*mockBackend

	mu          sync.Mutex
	lvs         map[string]backend.LVMIdentity
	importErr   error // a refusal unrelated to identity, e.g. "in use"
	importCalls int

	// InspectLV observation of the device: claim ("" means free),
	// consumers (mounts/holders), configured exports by target ID, an
	// inconclusive filesystem probe (fsProbeErr non-empty reports
	// FSProbeUnknown instead of ext4) and an injected probe failure.
	claim        string
	consumers    []backend.DeviceConsumer
	exports      []backend.DeviceConsumer
	fsProbeErr   string
	inspectErr   error
	inspectCalls int
}

func newMockLVBackend() *mockLVBackend {
	return &mockLVBackend{
		mockBackend: &mockBackend{
			backendType:            agentv1.BackendType_BACKEND_TYPE_LVM,
			backingResourcePresent: true,
			devicePathResult:       testLVDevicePath,
			expandAllocated:        2 << 30,
		},
		lvs: map[string]backend.LVMIdentity{testVolumeID: lvIdentity(testLVSource())},
	}
}

func (m *mockLVBackend) checkLV(volumeID string, want backend.LVMIdentity) error {
	lv, ok := m.lvs[volumeID]
	if !ok {
		return &backend.ImportRefusedError{VolumeID: volumeID, Reason: "missing", Detail: "lvs found no LV"}
	}
	if lv != want {
		return &backend.ImportRefusedError{
			VolumeID: volumeID,
			Reason:   backend.ImportRefusedReasonIdentity,
			Detail:   "the LV's UUIDs differ from the expected identity",
		}
	}
	return nil
}

func (m *mockLVBackend) ImportLV(
	_ context.Context,
	volumeID string,
	_ int64,
	want backend.LVMIdentity,
) (devicePath string, sizeBytes int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.importCalls++
	if m.importErr != nil {
		return "", 0, m.importErr
	}
	if err := m.checkLV(volumeID, want); err != nil {
		return "", 0, err
	}
	return m.devicePathResult, 1 << 30, nil
}

func (m *mockLVBackend) VerifyLV(_ context.Context, volumeID string, want backend.LVMIdentity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkLV(volumeID, want)
}

// replaceLV simulates the LV behind testVolumeID being recreated: same
// name, new LV UUID.
func (m *mockLVBackend) replaceLV() {
	m.mu.Lock()
	defer m.mu.Unlock()
	lv := m.lvs[testVolumeID]
	lv.LogicalVolumeUUID = otherUUID
	m.lvs[testVolumeID] = lv
}

func (m *mockLVBackend) importCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.importCalls
}

func (m *mockLVBackend) InspectLV(_ context.Context, volumeID string) (backend.LVObservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inspectCalls++
	if m.inspectErr != nil {
		return backend.LVObservation{}, m.inspectErr
	}
	lv, ok := m.lvs[volumeID]
	if !ok {
		return backend.LVObservation{}, &backend.ImportRefusedError{
			VolumeID: volumeID, Reason: "missing", Detail: "lvs found no LV",
		}
	}
	claim := m.claim
	if claim == "" {
		claim = backend.ExclusiveClaimFree
	}
	obs := backend.LVObservation{
		Identity: lv, Attr: "-wi-a-----", Segtype: "linear",
		DevicePath: m.devicePathResult, DevMajorMinor: "253:7",
		FSType: "ext4", FSUUID: "fs-uuid-1", FSProbe: backend.FSProbeDetected,
		SizeBytes: 1 << 30, Active: true,
		ExclusiveClaim: claim,
		Consumers:      slices.Clone(m.consumers),
		Exports:        slices.Clone(m.exports),
	}
	if m.fsProbeErr != "" {
		obs.FSType, obs.FSUUID = "", ""
		obs.FSProbe, obs.FSProbeError = backend.FSProbeUnknown, m.fsProbeErr
	}
	return obs, nil
}

var (
	_ backend.LVImporter  = (*mockLVBackend)(nil)
	_ backend.LVVerifier  = (*mockLVBackend)(nil)
	_ backend.LVInspector = (*mockLVBackend)(nil)
)

// newLVTestServer serves b as the backend of testPool with a temp configfs
// root and a temp agent state dir, both returned.
func newLVTestServer(t *testing.T, b backend.VolumeBackend) (srv *agent.Server, stateDir, cfgRoot string) {
	t.Helper()
	stateDir, cfgRoot = t.TempDir(), t.TempDir()
	return restartLVServer(t, b, stateDir, cfgRoot), stateDir, cfgRoot
}

// restartLVServer starts a new agent process on an existing state dir and
// configfs root, as after an agent restart.
func restartLVServer(t *testing.T, b backend.VolumeBackend, stateDir, cfgRoot string) *agent.Server {
	t.Helper()
	srv := agent.NewServer(map[string]backend.VolumeBackend{testPool: b}, cfgRoot, agent.WithDrainStateDir(stateDir))
	agent.SetDeviceChecker(t, srv, nvmeof.AlwaysPresentChecker)
	return srv
}

// testLVSource is the identity of the pre-existing LV behind testVolumeID.
func testLVSource() *agentv1.LvmSourceIdentity {
	return &agentv1.LvmSourceIdentity{
		VolumeGroup:       testPool,
		LogicalVolume:     testLVName,
		VolumeGroupUuid:   testVGUUID,
		LogicalVolumeUuid: testLVUUID,
	}
}

// lvSourceWith returns testLVSource modified by edit.
func lvSourceWith(edit func(*agentv1.LvmSourceIdentity)) *agentv1.LvmSourceIdentity {
	src := testLVSource()
	edit(src)
	return src
}

func otherLVUUID(s *agentv1.LvmSourceIdentity) { s.LogicalVolumeUuid = otherUUID }
func otherVGUUID(s *agentv1.LvmSourceIdentity) { s.VolumeGroupUuid = otherUUID }

func lvIdentity(src *agentv1.LvmSourceIdentity) backend.LVMIdentity {
	return backend.LVMIdentity{
		VolumeGroup:       src.GetVolumeGroup(),
		LogicalVolume:     src.GetLogicalVolume(),
		VolumeGroupUUID:   src.GetVolumeGroupUuid(),
		LogicalVolumeUUID: src.GetLogicalVolumeUuid(),
	}
}

func token2(uid string, gen uint64) *agentv1.FencingToken {
	return &agentv1.FencingToken{VolumeUid: uid, Generation: gen}
}

func lvImportRequest(
	fence *agentv1.FencingToken,
	src *agentv1.LvmSourceIdentity,
	preserve bool,
) *agentv1.ImportVolumeRequest {
	return &agentv1.ImportVolumeRequest{
		VolumeId:          testVolumeID,
		BackendType:       agentv1.BackendType_BACKEND_TYPE_LVM,
		CapacityBytes:     1 << 30,
		Fence:             fence,
		ExpectedLvmSource: src,
		PreserveOriginal:  &preserve,
	}
}

// markFilePath is the on-disk location of testVolumeID's fencing mark: the
// "generations" directory of the agent state dir, every byte outside
// [a-zA-Z0-9.-] escaped as "_xx".
func markFilePath(stateDir string) string {
	const hexDigits = "0123456789abcdef"
	var b strings.Builder
	for i := range len(testVolumeID) {
		c := testVolumeID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return filepath.Join(stateDir, "generations", b.String()+".mark")
}

// seedMark writes mark as testVolumeID's durable fencing mark, as an earlier
// agent run would have left it, and returns the written bytes.
func seedMark(t *testing.T, stateDir string, mark map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(mark)
	if err != nil {
		t.Fatal(err)
	}
	path := markFilePath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// pinnedMark is the mark of lifecycle uid at generation gen that adopted src
// with the given policy.
func pinnedMark(uid string, gen uint64, src *agentv1.LvmSourceIdentity, preserve bool) map[string]any {
	return map[string]any{
		"volumeUID":  uid,
		"generation": gen,
		"lvmSource": map[string]any{
			"volumeGroup":       src.GetVolumeGroup(),
			"logicalVolume":     src.GetLogicalVolume(),
			"volumeGroupUUID":   src.GetVolumeGroupUuid(),
			"logicalVolumeUUID": src.GetLogicalVolumeUuid(),
		},
		"preserveOriginal": preserve,
	}
}

// endedPinnedMark is pinnedMark after lifecycle-a's lifecycle was released.
func endedPinnedMark(gen uint64, src *agentv1.LvmSourceIdentity, preserve bool) map[string]any {
	const uid = "lifecycle-a"
	mark := pinnedMark(uid, gen, src, preserve)
	mark["ended"] = true
	mark["endedUIDs"] = []string{uid}
	return mark
}

func requireNoMarks(t *testing.T, stateDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateDir, "generations", "*.mark"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("fencing marks %v written by a refused request", matches)
	}
}

// markBytes reads testVolumeID's durable fencing mark.
func markBytes(t *testing.T, stateDir string) []byte {
	t.Helper()
	data, err := os.ReadFile(markFilePath(stateDir))
	if err != nil {
		t.Fatalf("read fencing mark: %v", err)
	}
	return data
}

// requireMarkUnchanged asserts a refused request left testVolumeID's durable
// mark byte-identical.
func requireMarkUnchanged(t *testing.T, stateDir string, want []byte, what string) {
	t.Helper()
	if got := markBytes(t, stateDir); !bytes.Equal(got, want) {
		t.Fatalf("%s rewrote the mark:\n got %s\nwant %s", what, got, want)
	}
}

func requireCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("%s: code %v (err=%v), want %v", what, got, err, want)
	}
}

// requireRetargetRefused proves the volume ID stays pinned to testLVSource:
// importing any other LV under fence is refused without inspecting it.
func requireRetargetRefused(t *testing.T, srv *agent.Server, b *mockLVBackend, fence *agentv1.FencingToken) {
	t.Helper()
	calls := b.importCount()
	for _, edit := range []func(*agentv1.LvmSourceIdentity){otherLVUUID, otherVGUUID} {
		_, err := srv.ImportVolume(context.Background(), lvImportRequest(fence, lvSourceWith(edit), true))
		requireCode(t, "import of another LV", err, codes.FailedPrecondition)
	}
	if b.importCount() != calls {
		t.Fatal("a pinned-source refusal inspected the other LV")
	}
}

// requirePreservedPin proves the volume ID is still pinned PreserveOriginal
// for owner: the owner can neither delete nor resize the LV, the LV stays
// untouched, and the volume ID cannot be retargeted.
func requirePreservedPin(t *testing.T, srv *agent.Server, b *mockLVBackend, owner *agentv1.FencingToken) {
	t.Helper()
	ctx := context.Background()
	_, err := srv.DeleteVolume(ctx, lvDeleteRequest(owner))
	requireCode(t, "DeleteVolume of a preserved LV", err, codes.FailedPrecondition)
	_, err = srv.ExpandVolume(ctx, lvExpandRequest(owner))
	requireCode(t, "ExpandVolume of a preserved LV", err, codes.FailedPrecondition)
	requireLVUntouched(t, b.mockBackend, "a preserved LV")
	requireRetargetRefused(t, srv, b, owner)
}

// An LVM import must name the LV it expects: without the identity the agent
// refuses before inspecting anything and records nothing.
func TestImportVolume_LVMRequiresExpectedLVMSource(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	srv, stateDir, _ := newLVTestServer(t, b)

	_, err := srv.ImportVolume(context.Background(), lvImportRequest(testFence(t), nil, true))
	requireCode(t, "LVM import without expected_lvm_source", err, codes.InvalidArgument)
	if n := b.importCount(); n != 0 {
		t.Errorf("ImportLV ran %d times for an import without an expected source", n)
	}
	requireNoMarks(t, stateDir)
}

// An LVM identity on a ZFS import is a malformed request, not an adoption.
func TestImportVolume_ZFSRejectsExpectedLVMSource(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{mockBackend: &mockBackend{}, importDevicePath: testDevicePath, importSize: 1 << 30}
	srv, stateDir := newImportTestServer(t, mb)

	req := importRequest(t)
	req.ExpectedLvmSource = testLVSource()
	_, err := srv.ImportVolume(context.Background(), req)
	requireCode(t, "ZFS import with expected_lvm_source", err, codes.InvalidArgument)
	if len(mb.importCalledWith) != 0 {
		t.Errorf("zvol Import ran for a request carrying an LVM identity: %v", mb.importCalledWith)
	}
	requireNoMarks(t, stateDir)
}

// An LVM backend that cannot verify LV identities never adopts an LV through
// the name-only VolumeImporter path.
func TestImportVolume_LVMBackendWithoutLVImporter(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{backendType: agentv1.BackendType_BACKEND_TYPE_LVM},
		importDevicePath: testLVDevicePath,
		importSize:       1 << 30,
	}
	srv, stateDir := newImportTestServer(t, mb)

	_, err := srv.ImportVolume(context.Background(), lvImportRequest(testFence(t), testLVSource(), true))
	requireCode(t, "LVM import on a backend without LVImporter", err, codes.Unimplemented)
	if len(mb.importCalledWith) != 0 {
		t.Errorf("name-only Import adopted an LV: %v", mb.importCalledWith)
	}
	requireNoMarks(t, stateDir)
}

// A successful LV import pins the identity and the policy durably: after an
// agent restart the volume ID can neither be downgraded to Managed nor
// retargeted to another LV, by its own lifecycle or another one; each
// refusal leaves the mark byte-identical; the owner's same-source retry
// still succeeds; and the preserved LV can be neither deleted nor resized.
func TestImportVolume_LVMSourceAndPolicyPinned(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	srv, stateDir, cfgRoot := newLVTestServer(t, b)
	ctx := context.Background()

	resp, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-a", 1), testLVSource(), true))
	if err != nil {
		t.Fatalf("ImportVolume: %v", err)
	}
	if resp.GetDevicePath() != testLVDevicePath || resp.GetCapacityBytes() != 1<<30 {
		t.Errorf("response = %v, want the LV's device path and size", resp)
	}
	pinned := markBytes(t, stateDir)

	srv = restartLVServer(t, b, stateDir, cfgRoot)
	calls := b.importCount()
	refused := []struct {
		name     string
		fence    *agentv1.FencingToken
		src      *agentv1.LvmSourceIdentity
		preserve bool
	}{
		{"same lifecycle downgrades to Managed", token2("lifecycle-a", 2), testLVSource(), false},
		{"same lifecycle, other lv_uuid", token2("lifecycle-a", 2), lvSourceWith(otherLVUUID), true},
		{"same lifecycle, other vg_uuid", token2("lifecycle-a", 2), lvSourceWith(otherVGUUID), true},
		{"same lifecycle, other LV name", token2("lifecycle-a", 2),
			lvSourceWith(func(s *agentv1.LvmSourceIdentity) { s.LogicalVolume = "pvc-other" }), true},
		{"foreign lifecycle while owned", token2("lifecycle-b", 9), testLVSource(), true},
	}
	for _, tc := range refused {
		_, err := srv.ImportVolume(ctx, lvImportRequest(tc.fence, tc.src, tc.preserve))
		requireCode(t, tc.name, err, codes.FailedPrecondition)
		if b.importCount() != calls {
			t.Fatalf("%s: a pinned-source refusal inspected the LV", tc.name)
		}
		requireMarkUnchanged(t, stateDir, pinned, tc.name)
	}

	if _, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-a", 2), testLVSource(), true)); err != nil {
		t.Fatalf("owner's retry with the pinned source: %v", err)
	}
	requirePreservedPin(t, restartLVServer(t, b, stateDir, cfgRoot), b, token2("lifecycle-a", 3))
}

// The pin outlives the lifecycle: after the adopting lifecycle was released,
// a new claim's lifecycle may re-adopt only the same LV (never downgrading a
// PreserveOriginal pin), the released lifecycle stays retired, and the new
// lifecycle inherits the pin.
func TestImportVolume_LVMPinSurvivesReleaseAndNewLifecycle(t *testing.T) {
	t.Parallel()
	for _, preserve := range []bool{true, false} {
		t.Run(map[bool]string{true: "PreserveOriginal", false: "Managed"}[preserve], func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			srv, stateDir, cfgRoot := newLVTestServer(t, b)
			ctx := context.Background()
			seeded := seedMark(t, stateDir, endedPinnedMark(3, testLVSource(), preserve))

			refused := []struct {
				name     string
				fence    *agentv1.FencingToken
				src      *agentv1.LvmSourceIdentity
				preserve bool
			}{
				{"new lifecycle, other lv_uuid", token2("lifecycle-b", 1), lvSourceWith(otherLVUUID), preserve},
				{"new lifecycle, other vg_uuid", token2("lifecycle-b", 1), lvSourceWith(otherVGUUID), preserve},
				{"released lifecycle re-imports", token2("lifecycle-a", 4), testLVSource(), preserve},
			}
			if preserve {
				refused = append(refused, struct {
					name     string
					fence    *agentv1.FencingToken
					src      *agentv1.LvmSourceIdentity
					preserve bool
				}{"new lifecycle downgrades to Managed", token2("lifecycle-b", 1), testLVSource(), false})
			}
			for _, tc := range refused {
				_, err := srv.ImportVolume(ctx, lvImportRequest(tc.fence, tc.src, tc.preserve))
				requireCode(t, tc.name, err, codes.FailedPrecondition)
				if n := b.importCount(); n != 0 {
					t.Fatalf("%s: a pinned-source refusal inspected the LV", tc.name)
				}
				requireMarkUnchanged(t, stateDir, seeded, tc.name)
			}

			if _, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-b", 1), testLVSource(), preserve)); err != nil {
				t.Fatalf("new lifecycle re-adopting the pinned LV: %v", err)
			}

			srv = restartLVServer(t, b, stateDir, cfgRoot)
			_, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-a", 5), testLVSource(), preserve))
			requireCode(t, "released lifecycle after the new adoption", err, codes.FailedPrecondition)
			if preserve {
				requirePreservedPin(t, srv, b, token2("lifecycle-b", 2))
				return
			}
			requireRetargetRefused(t, srv, b, token2("lifecycle-b", 2))
			b.replaceLV()
			_, err = srv.ExpandVolume(ctx, lvExpandRequest(token2("lifecycle-b", 2)))
			requireCode(t, "ExpandVolume of a replaced LV", err, codes.FailedPrecondition)
			requireLVUntouched(t, b.mockBackend, "ExpandVolume of a replaced LV")
		})
	}
}

// A refused LV import binds nothing: whether the LV no longer matches the
// expected identity or is in use, the volume ID stays unclaimed, and an
// import after the cause is gone adopts and pins it.
func TestImportVolume_LVMRefusedImportWritesNoMark(t *testing.T) {
	t.Parallel()
	causes := map[string]struct{ set, clear func(*mockLVBackend) }{
		"identity": {
			set:   func(b *mockLVBackend) { b.replaceLV() },
			clear: func(b *mockLVBackend) { b.lvs[testVolumeID] = lvIdentity(testLVSource()) },
		},
		"in use": {
			set: func(b *mockLVBackend) {
				b.importErr = &backend.ImportRefusedError{VolumeID: testVolumeID, Reason: "in use", Detail: "mounted at /data"}
			},
			clear: func(b *mockLVBackend) { b.importErr = nil },
		},
	}
	for name, cause := range causes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			cause.set(b)
			srv, stateDir, cfgRoot := newLVTestServer(t, b)

			_, err := srv.ImportVolume(context.Background(), lvImportRequest(testFence(t), testLVSource(), true))
			requireCode(t, "refused LV import", err, codes.FailedPrecondition)
			requireNoMarks(t, stateDir)

			cause.clear(b)
			if _, err := srv.ImportVolume(context.Background(),
				lvImportRequest(testFence(t), testLVSource(), true)); err != nil {
				t.Fatalf("import after the refusal cause cleared: %v", err)
			}
			requirePreservedPin(t, restartLVServer(t, b, stateDir, cfgRoot), b, token2(t.Name(), 2))
		})
	}
}

// The pin survives every fence transition a preserved LV can go through
// while adopted: export (grant), unexport (revoke), a same-generation
// unexport retry and a foreign lifecycle's release, each followed by an
// agent restart.
func TestPinnedSource_StickyAcrossExportAndForeignRelease(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	srv, stateDir, cfgRoot := newLVTestServer(t, b)
	ctx := context.Background()
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), true))

	steps := []struct {
		name  string
		run   func() error
		owner *agentv1.FencingToken
	}{
		{"ExportVolume(A,6)", func() error {
			_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
				VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testLVDevicePath,
				Fence: token2("lifecycle-a", 6),
			})
			return err
		}, token2("lifecycle-a", 6)},
		{"UnexportVolume(A,7)", func() error {
			_, err := srv.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
				VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				Fence: token2("lifecycle-a", 7),
			})
			return err
		}, token2("lifecycle-a", 7)},
		{"UnexportVolume(A,7) retry at the same generation", func() error {
			_, err := srv.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
				VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				Fence: token2("lifecycle-a", 7),
			})
			return err
		}, token2("lifecycle-a", 7)},
		{"ReleaseVolume(C,1) by a foreign lifecycle", func() error {
			_, err := srv.ReleaseVolume(ctx, releaseRequest(token2("lifecycle-c", 1)))
			return err
		}, token2("lifecycle-a", 7)},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		srv = restartLVServer(t, b, stateDir, cfgRoot)
		requirePreservedPin(t, srv, b, step.owner)
	}
	_, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-c", 2), testLVSource(), true))
	requireCode(t, "released foreign lifecycle imports", err, codes.FailedPrecondition)
}

// A preserving release that finds the LV no longer is the pinned one refuses
// and keeps the lifecycle owning the volume ID: a new claim cannot take it,
// and the owner can still act on it once the LV is back.
func TestReleaseVolume_PreservingIdentityMismatchKeepsOwner(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	b.replaceLV()
	srv, stateDir, _ := newLVTestServer(t, b)
	ctx := context.Background()
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 3, testLVSource(), true))

	_, err := srv.ReleaseVolume(ctx, releaseRequest(token2("lifecycle-a", 4)))
	requireCode(t, "preserving release of a replaced LV", err, codes.FailedPrecondition)
	requireLVUntouched(t, b.mockBackend, "refused release")

	b.lvs[testVolumeID] = lvIdentity(testLVSource())
	_, err = srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-b", 1), testLVSource(), true))
	requireCode(t, "new lifecycle while the refused release's owner holds the LV", err, codes.FailedPrecondition)
	if _, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-a", 5), testLVSource(), true)); err != nil {
		t.Fatalf("owner lost the volume ID to a refused release: %v", err)
	}
}

// decodedMark is the on-disk mark as the agent persisted it.
type decodedMark struct {
	VolumeUID        string         `json:"volumeUID"`
	Generation       uint64         `json:"generation"`
	Ended            bool           `json:"ended"`
	EndedUIDs        []string       `json:"endedUIDs"`
	LVMSource        map[string]any `json:"lvmSource"`
	PreserveOriginal bool           `json:"preserveOriginal"`
}

func readMark(t *testing.T, stateDir string) decodedMark {
	t.Helper()
	var m decodedMark
	if err := json.Unmarshal(markBytes(t, stateDir), &m); err != nil {
		t.Fatalf("decode mark: %v", err)
	}
	return m
}

// requireOwnedPin asserts the mark still pins testLVSource PreserveOriginal
// and uid still owns the volume ID (not ended, not retired).
func requireOwnedPin(t *testing.T, stateDir, uid, what string) {
	t.Helper()
	m := readMark(t, stateDir)
	if m.VolumeUID != uid || m.Ended || slices.Contains(m.EndedUIDs, uid) {
		t.Fatalf("%s: mark %+v, want %s still owning the volume ID", what, m, uid)
	}
	if !m.PreserveOriginal || m.LVMSource["logicalVolumeUUID"] != testLVUUID {
		t.Fatalf("%s: mark %+v lost the PreserveOriginal pin", what, m)
	}
}

// A preserving release removes the lifecycle's export first, then retires
// the lifecycle only when the agent observes the LV idle.  Any observed
// consumer, a busy or unknown exclusive claim, a leftover configured export
// or a failed probe refuses: the export stays removed, the lifecycle keeps
// owning the volume ID with its pin, and the LV is never touched.  The
// filesystem signature is not consumer evidence: an inconclusive signature
// probe (a raw or partitioned block LV) neither blocks an otherwise proven
// idle release nor stands in for the consumer proof.
func TestReleaseVolume_Preserving_RequiresNoConsumers(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		set    func(b *mockLVBackend)
		retire bool
	}{
		"clean":                  {set: func(*mockLVBackend) {}, retire: true},
		"exclusive open busy":    {set: func(b *mockLVBackend) { b.claim = backend.ExclusiveClaimBusy }},
		"exclusive open unknown": {set: func(b *mockLVBackend) { b.claim = backend.ExclusiveClaimUnknown }},
		"mounted": {set: func(b *mockLVBackend) {
			b.consumers = []backend.DeviceConsumer{{Kind: "mount", Detail: "/var/lib/old-workload"}}
		}},
		"dm holder (local attach)": {set: func(b *mockLVBackend) {
			b.consumers = []backend.DeviceConsumer{{Kind: "holder", Detail: "dm-9"}}
		}},
		"foreign export": {set: func(b *mockLVBackend) {
			b.exports = []backend.DeviceConsumer{{Kind: "export", Detail: "iqn.2003-01.org.other:legacy"}}
		}},
		"probe error": {set: func(b *mockLVBackend) { b.inspectErr = errors.New("read /sys/dev/block: EIO") }},
		"raw block, filesystem probe unknown": {set: func(b *mockLVBackend) {
			b.fsProbeErr = "blkid -p: exit status 2 with no output"
		}, retire: true},
		"filesystem probe unknown, exclusive open busy": {set: func(b *mockLVBackend) {
			b.fsProbeErr = "blkid -p: exit status 2 with no output"
			b.claim = backend.ExclusiveClaimBusy
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			srv, stateDir, cfgRoot := newLVTestServer(t, b)
			ctx := context.Background()
			seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), true))
			_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
				VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testLVDevicePath,
				Fence: token2("lifecycle-a", 6),
			})
			if err != nil {
				t.Fatalf("ExportVolume: %v", err)
			}
			tc.set(b)

			_, err = srv.ReleaseVolume(ctx, releaseRequest(token2("lifecycle-a", 6)))
			if n := nvmetSubsystems(t, cfgRoot); n != 0 {
				t.Errorf("NVMe-oF subsystems after release = %d, want the export removed", n)
			}
			requireLVUntouched(t, b.mockBackend, "preserving release")
			if !tc.retire {
				requireRefusedPreservingRelease(t, srv, stateDir, name, err)
				return
			}
			if err != nil {
				t.Fatalf("ReleaseVolume of an idle preserved LV: %v", err)
			}
			requireRetiredPreservingRelease(t, b, stateDir, cfgRoot)
		})
	}
}

// requireRefusedPreservingRelease asserts a refused preserving release left
// lifecycle-a owning the volume ID with its pin, so a new claim cannot take
// it.
func requireRefusedPreservingRelease(t *testing.T, srv *agent.Server, stateDir, name string, err error) {
	t.Helper()
	requireCode(t, "preserving release ("+name+")", err, codes.FailedPrecondition)
	requireOwnedPin(t, stateDir, "lifecycle-a", name)
	// The owner keeps acting on the volume ID; a new claim cannot take it.
	_, err = srv.ImportVolume(context.Background(), lvImportRequest(token2("lifecycle-b", 1), testLVSource(), true))
	requireCode(t, "new lifecycle after a refused release", err, codes.FailedPrecondition)
}

// requireRetiredPreservingRelease asserts a successful preserving release
// retired lifecycle-a with the pin kept, that a delayed retry after an agent
// restart is a no-op, and that a new claim re-adopts the same LV with the
// inherited pin.
func requireRetiredPreservingRelease(t *testing.T, b *mockLVBackend, stateDir, cfgRoot string) {
	t.Helper()
	ctx := context.Background()
	m := readMark(t, stateDir)
	if !m.Ended || !slices.Contains(m.EndedUIDs, "lifecycle-a") || !m.PreserveOriginal ||
		m.LVMSource["logicalVolumeUUID"] != testLVUUID {
		t.Fatalf("mark after release %+v, want lifecycle-a retired with the pin kept", m)
	}
	// A delayed release retry of the retired lifecycle, after an agent
	// restart, is a no-op that keeps the pin and the retirement.
	ended := markBytes(t, stateDir)
	srv := restartLVServer(t, b, stateDir, cfgRoot)
	if _, err := srv.ReleaseVolume(ctx, releaseRequest(token2("lifecycle-a", 6))); err != nil {
		t.Fatalf("ReleaseVolume retry of the retired lifecycle: %v", err)
	}
	requireMarkUnchanged(t, stateDir, ended, "release retry of the retired lifecycle")
	// A new claim re-adopts the same LV and inherits the pin.
	if _, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-b", 1), testLVSource(), true)); err != nil {
		t.Fatalf("new lifecycle re-adopting the released LV: %v", err)
	}
	requirePreservedPin(t, srv, b, token2("lifecycle-b", 2))
}

// A preserving release on a backend that cannot observe consumers fails
// closed: identity verification alone never proves the LV idle.
func TestReleaseVolume_Preserving_BackendWithoutInspectorRefuses(t *testing.T) {
	t.Parallel()
	b := newVerifyOnlyLVBackend()
	srv, stateDir, _ := newLVTestServer(t, b)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), true))

	_, err := srv.ReleaseVolume(context.Background(), releaseRequest(token2("lifecycle-a", 6)))
	requireCode(t, "preserving release without LVInspector", err, codes.FailedPrecondition)
	requireOwnedPin(t, stateDir, "lifecycle-a", "release without LVInspector")
	requireLVUntouched(t, b.mockBackend, "release without LVInspector")
}

// verifyOnlyLVBackend imports and verifies LVs but cannot inspect them.
type verifyOnlyLVBackend struct {
	*mockBackend
	lv *mockLVBackend
}

func newVerifyOnlyLVBackend() *verifyOnlyLVBackend {
	lv := newMockLVBackend()
	return &verifyOnlyLVBackend{mockBackend: lv.mockBackend, lv: lv}
}

func (v *verifyOnlyLVBackend) ImportLV(
	ctx context.Context, volumeID string, capacityBytes int64, want backend.LVMIdentity,
) (devicePath string, sizeBytes int64, err error) {
	return v.lv.ImportLV(ctx, volumeID, capacityBytes, want)
}

func (v *verifyOnlyLVBackend) VerifyLV(ctx context.Context, volumeID string, want backend.LVMIdentity) error {
	return v.lv.VerifyLV(ctx, volumeID, want)
}

// Control: a Managed adopted LV's release only verifies the identity; no
// consumer observation is required because a later DeleteVolume, not the
// release, decides the LV's fate.
func TestReleaseVolume_ManagedPinnedVerifiesIdentityOnly(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	b.claim = backend.ExclusiveClaimBusy
	srv, stateDir, _ := newLVTestServer(t, b)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), false))

	if _, err := srv.ReleaseVolume(context.Background(), releaseRequest(token2("lifecycle-a", 6))); err != nil {
		t.Fatalf("ReleaseVolume of a verified Managed LV: %v", err)
	}
	if m := readMark(t, stateDir); !m.Ended || m.LVMSource["logicalVolumeUUID"] != testLVUUID {
		t.Fatalf("mark %+v, want retired with the pin kept", m)
	}
}

// An LVM import that omits preserve_original adopts the LV PreserveOriginal:
// after it (and an agent restart) the LV can be neither deleted nor resized
// and a later explicit Managed import cannot downgrade it.  Only an explicit
// false opts in to Managed, which a verified DeleteVolume may then remove.
func TestImportVolume_LVMOmittedPolicyDefaultsToPreserve(t *testing.T) {
	t.Parallel()
	t.Run("omitted", func(t *testing.T) {
		t.Parallel()
		b := newMockLVBackend()
		srv, stateDir, cfgRoot := newLVTestServer(t, b)
		req := lvImportRequest(token2("lifecycle-a", 1), testLVSource(), false)
		req.PreserveOriginal = nil
		if _, err := srv.ImportVolume(context.Background(), req); err != nil {
			t.Fatalf("ImportVolume without preserve_original: %v", err)
		}
		srv = restartLVServer(t, b, stateDir, cfgRoot)
		requirePreservedPin(t, srv, b, token2("lifecycle-a", 2))
		_, err := srv.ImportVolume(context.Background(),
			lvImportRequest(token2("lifecycle-a", 3), testLVSource(), false))
		requireCode(t, "explicit Managed after a defaulted PreserveOriginal", err, codes.FailedPrecondition)
		requireLVUntouched(t, b.mockBackend, "defaulted PreserveOriginal")
	})
	t.Run("explicit Managed", func(t *testing.T) {
		t.Parallel()
		b := newMockLVBackend()
		srv, _, _ := newLVTestServer(t, b)
		ctx := context.Background()
		if _, err := srv.ImportVolume(ctx, lvImportRequest(token2("lifecycle-a", 1), testLVSource(), false)); err != nil {
			t.Fatalf("ImportVolume Managed: %v", err)
		}
		if _, err := srv.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 2))); err != nil {
			t.Fatalf("DeleteVolume of an explicitly Managed LV: %v", err)
		}
		if len(b.deleteCalledWith) != 1 {
			t.Fatalf("backend delete=%v, want one", b.deleteCalledWith)
		}
	})
}
