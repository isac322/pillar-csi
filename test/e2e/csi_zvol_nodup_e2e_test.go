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

// Package e2e — zvol no-duplication E2E tests (Sub-AC 4c).
//
// This file verifies that when CreateVolume partially fails (agent.CreateVolume
// succeeds but agent.ExportVolume fails), a retry of CreateVolume:
//
//  1. Does NOT create a second zvol on the agent — the controller's
//     skipBackend optimisation detects the CreatePartial state (persisted in
//     the PillarVolume CRD) and skips agent.CreateVolume entirely on retry.
//
//  2. Advances the PillarVolume CRD to Phase=Ready with ExportInfo populated,
//     i.e. the volume is in a "healthy" state after the retry.
//
// # Test Design
//
// The zvol state is tracked by a statefulZvolAgentServer that wraps the
// mockAgentServer used by the other CSI tests.  The wrapper maintains an
// explicit "zvol registry" (a map from agentVolumeID → struct{}) that mirrors
// what a real ZFS agent would hold.  The registry is updated on every
// CreateVolume (idempotent add) and DeleteVolume (remove) call.
//
//   - CreateVolumeCalls count reveals how many times the controller issued
//     agent.CreateVolume; exactly one call means no duplicate zvol-creation
//     requests were sent.
//   - zvolRegistry length reveals how many distinct zvols currently exist;
//     len == 1 means exactly one zvol exists without duplication.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIZvolNoDup
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	csisrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// statefulZvolAgentServer
// ─────────────────────────────────────────────────────────────────────────────.

// statefulZvolAgentServer wraps mockAgentServer and adds an explicit zvol
// registry that mirrors what a real ZFS agent would maintain.
//
// The registry is keyed by the agentVolumeID (e.g. "tank/pvc-abc123").
// CreateVolume adds to the registry (idempotent – inserting the same key
// twice leaves len == 1).  DeleteVolume removes from it.
//
// This lets tests verify "exactly one zvol exists" without relying solely on
// RPC call counts.
type statefulZvolAgentServer struct {
	// Embedded by pointer to avoid copying the mutex inside mockAgentServer.
	*mockAgentServer

	zvolMu       sync.Mutex
	zvolRegistry map[string]struct{} // agentVolumeID → exists
}

// Compile-time interface check.
var _ agentv1.AgentServiceServer = (*statefulZvolAgentServer)(nil)

// newStatefulZvolAgentServer returns a *statefulZvolAgentServer with the same
// sensible defaults as newMockAgentServer.
func newStatefulZvolAgentServer() *statefulZvolAgentServer {
	return &statefulZvolAgentServer{
		mockAgentServer: newMockAgentServer(),
		zvolRegistry:    make(map[string]struct{}),
	}
}

// ZvolCount returns the number of zvols currently tracked as existing.
// Thread-safe.
func (m *statefulZvolAgentServer) ZvolCount() int {
	m.zvolMu.Lock()
	defer m.zvolMu.Unlock()
	return len(m.zvolRegistry)
}

// ZvolExists returns true if agentVolumeID is present in the zvol registry.
// Thread-safe.
func (m *statefulZvolAgentServer) ZvolExists(agentVolumeID string) bool {
	m.zvolMu.Lock()
	defer m.zvolMu.Unlock()
	_, ok := m.zvolRegistry[agentVolumeID]
	return ok
}

// CreateVolume overrides mockAgentServer.CreateVolume to track zvol existence.
// On a successful CreateVolume the agentVolumeID is added to the registry
// (idempotent: inserting the same key twice leaves len == 1).
func (m *statefulZvolAgentServer) CreateVolume(
	ctx context.Context,
	req *agentv1.CreateVolumeRequest,
) (*agentv1.CreateVolumeResponse, error) {
	resp, err := m.mockAgentServer.CreateVolume(ctx, req)
	if err != nil {
		return nil, err
	}
	// Track the zvol only after a successful agent-level create.
	m.zvolMu.Lock()
	m.zvolRegistry[req.GetVolumeId()] = struct{}{}
	m.zvolMu.Unlock()
	return resp, nil
}

// DeleteVolume overrides mockAgentServer.DeleteVolume to track zvol removal.
func (m *statefulZvolAgentServer) DeleteVolume(
	ctx context.Context,
	req *agentv1.DeleteVolumeRequest,
) (*agentv1.DeleteVolumeResponse, error) {
	resp, err := m.mockAgentServer.DeleteVolume(ctx, req)
	if err != nil {
		return nil, err
	}
	m.zvolMu.Lock()
	delete(m.zvolRegistry, req.GetVolumeId())
	m.zvolMu.Unlock()
	return resp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// zvolTestEnv
// ─────────────────────────────────────────────────────────────────────────────.

// zvolTestEnv is the complete test scaffold for zvol no-duplication tests.
// It mirrors csiControllerE2EEnv but uses a statefulZvolAgentServer instead
// of a plain mockAgentServer so that zvol lifecycle can be asserted explicitly.
type zvolTestEnv struct {
	// Controller is the CSI ControllerServer under test.
	Controller *csisrv.ControllerServer

	// AgentMock is the stateful mock that tracks zvol existence.
	AgentMock *statefulZvolAgentServer

	// TargetName is the Kubernetes PillarTarget name.
	TargetName string

	// K8sClient is the fake Kubernetes client.  Tests use it to read and
	// verify PillarVolume CRD objects.
	K8sClient client.Client
}

// newZvolTestEnv creates a zvolTestEnv for the duration of a single test.
// Cleanup (gRPC server stop) is registered via t.Cleanup.
func newZvolTestEnv(t *testing.T, targetName string) *zvolTestEnv {
	t.Helper()

	// ── Stateful agent gRPC server ────────────────────────────────────────────
	agentMock := newStatefulZvolAgentServer()
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, agentMock)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("newZvolTestEnv: net.Listen: %v", err)
	}
	agentAddr := lis.Addr().String()

	go func() { _ = grpcSrv.Serve(lis) }() //nolint:errcheck // server errors are non-actionable in test setup
	t.Cleanup(func() { grpcSrv.GracefulStop() })

	// ── Fake Kubernetes client with PillarTarget ──────────────────────────────
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("newZvolTestEnv: AddToScheme: %v", err)
	}

	pillarTarget := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: targetName},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{
				Address: "127.0.0.1",
				Port:    4500,
			},
		},
		Status: v1alpha1.PillarTargetStatus{
			ResolvedAddress: agentAddr,
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pillarTarget).
		WithStatusSubresource(&v1alpha1.PillarVolume{}).
		Build()

	// ── AgentDialer (insecure for tests) ─────────────────────────────────────
	dialer := func(_ context.Context, addr string) (agentv1.AgentServiceClient, io.Closer, error) {
		conn, dialErr := grpc.NewClient(
			addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if dialErr != nil {
			return nil, nil, dialErr
		}
		return agentv1.NewAgentServiceClient(conn), conn, nil
	}

	// ── CSI ControllerServer ──────────────────────────────────────────────────
	controller := csisrv.NewControllerServerWithDialer(k8sClient, "pillar-csi.bhyoo.com", dialer)

	return &zvolTestEnv{
		Controller: controller,
		AgentMock:  agentMock,
		TargetName: targetName,
		K8sClient:  k8sClient,
	}
}

// defaultParams returns StorageClass-style parameters for the test environment.
func (e *zvolTestEnv) defaultParams() map[string]string {
	return map[string]string{
		"pillar-csi.bhyoo.com/target":        e.TargetName,
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
		"pillar-csi.bhyoo.com/pool":          "tank",
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIZvolNoDup_ExactlyOneZvolAfterExportFailureRetry
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIZvolNoDup_ExactlyOneZvolAfterExportFailureRetry injects a mock
// ExportVolume failure after CreateVolume (agent-level) succeeds, then retries
// CreateVolume and asserts:
//
//  1. Exactly one zvol exists in the agent's zvol registry — the controller's
//     skipBackend optimisation detects the persisted CreatePartial state and
//     does NOT issue a second agent.CreateVolume call, so no duplicate zvol
//     is ever created.
//
//  2. The volume reaches a healthy state: PillarVolume CRD has Phase=Ready,
//     PartialFailure is nil, and ExportInfo is populated.
//
// This test validates the constraint from the acceptance criteria:
//
//	"calling CreateVolume twice with same name returns same volume" (idempotency)
//
// and more specifically:
//
//	"retries CreateVolume and asserts exactly one zvol exists and the volume
//	reaches a healthy state without duplication" (Sub-AC 4c)
func TestCSIZvolNoDup_ExactlyOneZvolAfterExportFailureRetry(t *testing.T) { //nolint:gocyclo // retry test
	t.Parallel()
	ctx := context.Background()
	env := newZvolTestEnv(t, "storage-1")

	const (
		volName  = "pvc-nodup-export-fail-001"
		capBytes = 1 << 30 // 1 GiB
	)

	req := &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: capBytes},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultParams(),
	}

	// ── Step 1: ExportVolume fails; agent.CreateVolume succeeds ───────────────
	//
	// The mock allows CreateVolume (backend creation) to succeed — the zvol is
	// created on the agent — but rejects ExportVolume (transient network error).
	env.AgentMock.ExportVolumeErr = errors.New("export failed: NVMe-oF target busy (transient)")

	_, step1Err := env.Controller.CreateVolume(ctx, req)
	if step1Err == nil {
		t.Fatal("Step 1: expected CreateVolume to return an error when ExportVolume fails, got nil")
	}
	t.Logf("Step 1: got expected error: %v", step1Err)

	// ── Step 1 assertions ────────────────────────────────────────────────────

	// agent.CreateVolume was called exactly once — one zvol creation request.
	if got := len(env.AgentMock.CreateVolumeCalls); got != 1 {
		t.Fatalf("Step 1: agent.CreateVolume call count: got %d, want 1", got)
	}
	// agent.ExportVolume was attempted once (and failed).
	if got := len(env.AgentMock.ExportVolumeCalls); got != 1 {
		t.Fatalf("Step 1: agent.ExportVolume call count: got %d, want 1", got)
	}
	// Zvol registry must show exactly 1 zvol.
	if got := env.AgentMock.ZvolCount(); got != 1 {
		t.Fatalf("Step 1: zvol registry count: got %d, want 1 "+
			"(CreateVolume created the zvol, ExportVolume failed)", got)
	}

	// PillarVolume CRD must exist with Phase=CreatePartial.
	pvStep1 := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(ctx, types.NamespacedName{Name: volName}, pvStep1); getErr != nil {
		t.Fatalf("Step 1: PillarVolume CRD not found: %v", getErr)
	}
	if pvStep1.Status.Phase != v1alpha1.PillarVolumePhaseCreatePartial {
		t.Errorf("Step 1: PillarVolume phase: got %q, want CreatePartial", pvStep1.Status.Phase)
	}
	// BackendDevicePath must be persisted so the retry can skip agent.CreateVolume.
	if pvStep1.Status.BackendDevicePath == "" {
		t.Error("Step 1: PillarVolume.Status.BackendDevicePath is empty; " +
			"the retry would incorrectly call agent.CreateVolume again (duplication risk)")
	}

	// ── Step 2: Retry — ExportVolume now succeeds ────────────────────────────
	env.AgentMock.ExportVolumeErr = nil // clear the injected error

	createResp, step2Err := env.Controller.CreateVolume(ctx, req)
	if step2Err != nil {
		t.Fatalf("Step 2: unexpected error on retry: %v", step2Err)
	}
	vol := createResp.GetVolume()
	if vol == nil {
		t.Fatal("Step 2: CreateVolumeResponse.Volume is nil")
	}
	if vol.GetVolumeId() == "" {
		t.Error("Step 2: VolumeId must not be empty")
	}

	// ── Step 2 assertions: no duplicate zvol ─────────────────────────────────
	//
	// The controller detects Phase=CreatePartial in the PillarVolume CRD and
	// sets skipBackend=true.  agent.CreateVolume is NOT called again — the
	// zvol count remains at 1, unchanged from Step 1.
	if got := len(env.AgentMock.CreateVolumeCalls); got != 1 {
		t.Errorf("Step 2: agent.CreateVolume call count after retry: got %d, want 1 "+
			"(skipBackend optimisation must prevent duplicate zvol creation)", got)
	}
	// agent.ExportVolume must have been called a second time (the retry).
	if got := len(env.AgentMock.ExportVolumeCalls); got != 2 {
		t.Errorf("Step 2: agent.ExportVolume call count: got %d, want 2 "+
			"(one failed attempt + one successful retry)", got)
	}
	// Zvol registry: still exactly 1 zvol — no duplication.
	if got := env.AgentMock.ZvolCount(); got != 1 {
		t.Errorf("Step 2: zvol registry count: got %d, want 1 (exactly one zvol must exist)", got)
	}

	// ── Step 2 assertions: healthy state ─────────────────────────────────────
	pvStep2 := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(ctx, types.NamespacedName{Name: volName}, pvStep2); getErr != nil {
		t.Fatalf("Step 2: PillarVolume CRD not found after retry: %v", getErr)
	}
	// Phase must advance from CreatePartial → Ready.
	if pvStep2.Status.Phase != v1alpha1.PillarVolumePhaseReady {
		t.Errorf("Step 2: PillarVolume phase: got %q, want Ready", pvStep2.Status.Phase)
	}
	// PartialFailure must be cleared once recovery succeeds.
	if pvStep2.Status.PartialFailure != nil {
		t.Errorf("Step 2: PartialFailure must be nil after successful retry, got %+v",
			pvStep2.Status.PartialFailure)
	}
	// ExportInfo must be populated — volume is accessible over the network.
	if pvStep2.Status.ExportInfo == nil {
		t.Fatal("Step 2: ExportInfo must be populated after successful retry")
	}
	if pvStep2.Status.ExportInfo.TargetID == "" {
		t.Error("Step 2: ExportInfo.TargetID must not be empty")
	}
	if pvStep2.Status.ExportInfo.Address == "" {
		t.Error("Step 2: ExportInfo.Address must not be empty")
	}
	// BackendDevicePath must be cleared in the Ready phase (no longer needed).
	if pvStep2.Status.BackendDevicePath != "" {
		t.Errorf("Step 2: BackendDevicePath should be cleared in Ready phase, got %q",
			pvStep2.Status.BackendDevicePath)
	}

	t.Logf("volume %q reached healthy state; zvol registry count=%d, "+
		"agent.CreateVolume calls=%d, agent.ExportVolume calls=%d",
		vol.GetVolumeId(),
		env.AgentMock.ZvolCount(),
		len(env.AgentMock.CreateVolumeCalls),
		len(env.AgentMock.ExportVolumeCalls))
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIZvolNoDup_ZvolRegistryReflectsDeleteAfterPartialCreate
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIZvolNoDup_ZvolRegistryReflectsDeleteAfterPartialCreate verifies that
// DeleteVolume on a volume in the CreatePartial state correctly removes the
// zvol from the registry (registry goes 1 → 0) and deletes the PillarVolume
// CRD.
func TestCSIZvolNoDup_ZvolRegistryReflectsDeleteAfterPartialCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newZvolTestEnv(t, "storage-1")

	const (
		volName  = "pvc-nodup-delete-partial-001"
		capBytes = 1 << 30
	)

	req := &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: capBytes},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultParams(),
	}

	// ── Cause a partial failure (backend created, export never succeeded) ─────
	env.AgentMock.ExportVolumeErr = errors.New("export failed: target unavailable")
	_, _ = env.Controller.CreateVolume(ctx, req) //nolint:errcheck // intentional failure to test partial state

	// Verify the zvol was created.
	if got := env.AgentMock.ZvolCount(); got != 1 {
		t.Fatalf("after partial failure: zvol count: got %d, want 1", got)
	}

	// Retrieve the VolumeID from the persisted PillarVolume CRD.
	pv := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(ctx, types.NamespacedName{Name: volName}, pv); getErr != nil {
		t.Fatalf("PillarVolume CRD not found: %v", getErr)
	}
	if pv.Status.Phase != v1alpha1.PillarVolumePhaseCreatePartial {
		t.Errorf("pre-delete phase: got %q, want CreatePartial", pv.Status.Phase)
	}
	volumeID := pv.Spec.VolumeID
	if volumeID == "" {
		t.Fatal("PillarVolume.Spec.VolumeID is empty — cannot delete")
	}

	// ── Delete the partially-created volume ───────────────────────────────────
	_, deleteErr := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if deleteErr != nil {
		t.Fatalf("DeleteVolume on partial volume: unexpected error: %v", deleteErr)
	}

	// ── Assert: zvol registry is now empty ────────────────────────────────────
	//
	// The registry must transition 1 → 0: the backend resource has been
	// reclaimed.
	if got := env.AgentMock.ZvolCount(); got != 0 {
		t.Errorf("after DeleteVolume: zvol count: got %d, want 0 (zvol must be deleted)", got)
	}

	// ── Assert: PillarVolume CRD is removed ──────────────────────────────────
	pvAfter := &v1alpha1.PillarVolume{}
	getErr := env.K8sClient.Get(ctx, types.NamespacedName{Name: volName}, pvAfter)
	if getErr == nil {
		t.Fatal("PillarVolume CRD still exists after DeleteVolume on partial volume")
	}
	if !k8serrors.IsNotFound(getErr) {
		t.Errorf("expected NotFound after DeleteVolume, got: %v", getErr)
	}

	t.Logf("DeleteVolume on partial volume succeeded; zvol registry count=%d",
		env.AgentMock.ZvolCount())
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIZvolNoDup_MultipleRetriesNeverDuplicate
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIZvolNoDup_MultipleRetriesNeverDuplicate verifies that even if
// ExportVolume fails multiple times in a row, the zvol count never exceeds 1.
// The controller's skipBackend logic prevents every retry from issuing a new
// agent.CreateVolume call.
func TestCSIZvolNoDup_MultipleRetriesNeverDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newZvolTestEnv(t, "storage-1")

	const (
		volName    = "pvc-nodup-multiretry-001"
		capBytes   = 1 << 30
		retryFails = 3 // number of consecutive export failures before success
	)

	req := &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: capBytes},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultParams(),
	}

	// ── Fail ExportVolume retryFails times ────────────────────────────────────
	for i := range retryFails {
		env.AgentMock.ExportVolumeErr = fmt.Errorf("export failed: transient (attempt %d)", i+1)
		_, err := env.Controller.CreateVolume(ctx, req)
		if err == nil {
			t.Fatalf("attempt %d: expected error, got nil", i+1)
		}

		// After every failed attempt: still exactly one zvol in the registry.
		if got := env.AgentMock.ZvolCount(); got != 1 {
			t.Errorf("after failed attempt %d: zvol count: got %d, want 1 "+
				"(no duplication on retry)", i+1, got)
		}
		// agent.CreateVolume call count must remain at 1 — only the first call
		// creates the backend; all subsequent retries use skipBackend.
		if got := len(env.AgentMock.CreateVolumeCalls); got != 1 {
			t.Errorf("after failed attempt %d: agent.CreateVolume calls: got %d, want 1",
				i+1, got)
		}
		// agent.ExportVolume call count increases with each attempt.
		wantExport := i + 1
		if got := len(env.AgentMock.ExportVolumeCalls); got != wantExport {
			t.Errorf("after failed attempt %d: agent.ExportVolume calls: got %d, want %d",
				i+1, got, wantExport)
		}
	}

	// ── Final retry: ExportVolume succeeds ────────────────────────────────────
	env.AgentMock.ExportVolumeErr = nil
	createResp, finalErr := env.Controller.CreateVolume(ctx, req)
	if finalErr != nil {
		t.Fatalf("final retry: unexpected error: %v", finalErr)
	}
	if createResp.GetVolume() == nil {
		t.Fatal("final retry: expected volume in response, got nil")
	}

	// ── Final assertions ──────────────────────────────────────────────────────

	// agent.CreateVolume was called exactly once across all attempts.
	if got := len(env.AgentMock.CreateVolumeCalls); got != 1 {
		t.Errorf("final: agent.CreateVolume calls: got %d, want 1 "+
			"(skipBackend prevents duplicate zvol creation on all retries)", got)
	}
	// agent.ExportVolume was called retryFails+1 times total.
	wantExportCalls := retryFails + 1
	if got := len(env.AgentMock.ExportVolumeCalls); got != wantExportCalls {
		t.Errorf("final: agent.ExportVolume calls: got %d, want %d", got, wantExportCalls)
	}
	// Zvol registry: still exactly 1 — no duplication across any attempt.
	if got := env.AgentMock.ZvolCount(); got != 1 {
		t.Errorf("final: zvol count: got %d, want 1 "+
			"(exactly one zvol must exist after %d retries)", got, retryFails)
	}

	// Volume must be in healthy state.
	pvFinal := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(ctx, types.NamespacedName{Name: volName}, pvFinal); getErr != nil {
		t.Fatalf("PillarVolume CRD not found after final retry: %v", getErr)
	}
	if pvFinal.Status.Phase != v1alpha1.PillarVolumePhaseReady {
		t.Errorf("final: PillarVolume phase: got %q, want Ready", pvFinal.Status.Phase)
	}
	if pvFinal.Status.PartialFailure != nil {
		t.Errorf("final: PartialFailure must be nil after successful retry, got %+v",
			pvFinal.Status.PartialFailure)
	}
	if pvFinal.Status.ExportInfo == nil {
		t.Fatal("final: ExportInfo must be populated after successful retry")
	}

	t.Logf("after %d export failures + 1 success: zvol count=%d, "+
		"agent.CreateVolume calls=%d, agent.ExportVolume calls=%d",
		retryFails,
		env.AgentMock.ZvolCount(),
		len(env.AgentMock.CreateVolumeCalls),
		len(env.AgentMock.ExportVolumeCalls))
}
