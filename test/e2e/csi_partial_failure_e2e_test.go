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

// Package e2e — CSI Controller partial-failure persistence tests.
//
// TestCSIController_PartialFailure_* exercises the durable partial-failure
// state tracking implemented via PillarVolume CRDs:
//
//   - A successful agent.CreateVolume but failed agent.ExportVolume must
//     result in a PillarVolume CRD with Phase=CreatePartial and
//     PartialFailure.BackendCreated=true.
//
//   - Retrying CreateVolume after the above partial failure (ExportVolume
//     now succeeds) must advance the CRD to Phase=Ready with ExportInfo
//     populated and PartialFailure cleared.
//
//   - A successful DeleteVolume must delete the PillarVolume CRD.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIController_PartialFailure
//	go test ./test/e2e/ -v -run TestCSIController_DeleteVolume_CleansUpCRD
package e2e

import (
	"context"
	"errors"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_PartialFailure_CRDCreatedOnExportFailure
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_PartialFailure_CRDCreatedOnExportFailure verifies that
// when agent.CreateVolume succeeds but agent.ExportVolume fails, the controller:
//
//  1. Returns a non-nil error to the CO (so the CO will retry).
//  2. Creates a PillarVolume CRD with Phase=CreatePartial.
//  3. Populates PartialFailure with BackendCreated=true and
//     FailedOperation="ExportVolume".
func TestCSIController_PartialFailure_CRDCreatedOnExportFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newCSIControllerE2EEnv(t, "storage-1")

	const volName = "pvc-partial-export-001"

	// ── Arrange: ExportVolume will fail ──────────────────────────────────────
	env.AgentMock.ExportVolumeErr = errors.New("export failed: NVMe-oF target busy")

	// ── Act: CreateVolume ─────────────────────────────────────────────────────
	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultCreateVolumeParams(),
	})

	// ── Assert 1: the call returns an error ───────────────────────────────────
	if err == nil {
		t.Fatal("CreateVolume: expected error when ExportVolume fails, got nil")
	}

	// ── Assert 2: PillarVolume CRD exists with CreatePartial phase ────────────
	pv := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(
		ctx,
		types.NamespacedName{Name: volName},
		pv,
	); getErr != nil {
		t.Fatalf("PillarVolume CRD not found after partial failure: %v", getErr)
	}

	if pv.Status.Phase != v1alpha1.PillarVolumePhaseCreatePartial {
		t.Errorf("PillarVolume phase: got %q, want %q",
			pv.Status.Phase, v1alpha1.PillarVolumePhaseCreatePartial)
	}

	// ── Assert 3: PartialFailure records BackendCreated=true ──────────────────
	if pv.Status.PartialFailure == nil {
		t.Fatal("PillarVolume.Status.PartialFailure is nil; expected partial-failure info")
	}
	if !pv.Status.PartialFailure.BackendCreated {
		t.Error("PillarVolume.Status.PartialFailure.BackendCreated: got false, want true")
	}
	if pv.Status.PartialFailure.FailedOperation != "ExportVolume" {
		t.Errorf("PillarVolume.Status.PartialFailure.FailedOperation: got %q, want %q",
			pv.Status.PartialFailure.FailedOperation, "ExportVolume")
	}

	// ExportInfo must be nil while export has not succeeded.
	if pv.Status.ExportInfo != nil {
		t.Errorf("PillarVolume.Status.ExportInfo: expected nil while in CreatePartial, got %+v",
			pv.Status.ExportInfo)
	}

	// The Spec must record the immutable volume identity.
	if pv.Spec.VolumeID == "" {
		t.Error("PillarVolume.Spec.VolumeID: must not be empty after CreatePartial")
	}
	if pv.Spec.AgentVolumeID == "" {
		t.Error("PillarVolume.Spec.AgentVolumeID: must not be empty after CreatePartial")
	}
	if pv.Spec.TargetRef != "storage-1" {
		t.Errorf("PillarVolume.Spec.TargetRef: got %q, want %q", pv.Spec.TargetRef, "storage-1")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_PartialFailure_RetryAdvancesToReady
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_PartialFailure_RetryAdvancesToReady verifies that after a
// partial failure (Phase=CreatePartial), retrying CreateVolume with a working
// ExportVolume:
//
//  1. Returns a successful CreateVolumeResponse.
//  2. Advances the PillarVolume CRD to Phase=Ready.
//  3. Populates ExportInfo in the CRD.
//  4. Clears PartialFailure.
func TestCSIController_PartialFailure_RetryAdvancesToReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newCSIControllerE2EEnv(t, "storage-1")

	const volName = "pvc-partial-retry-001"
	req := &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 2 << 30},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultCreateVolumeParams(),
	}

	// ── Step 1: cause a partial failure ──────────────────────────────────────
	env.AgentMock.ExportVolumeErr = errors.New("export failed: transient network error")
	_, step1Err := env.Controller.CreateVolume(ctx, req)
	if step1Err == nil {
		t.Fatal("Step 1: expected error when ExportVolume fails, got nil")
	}

	// Confirm CreatePartial phase is persisted.
	pvStep1 := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(
		ctx,
		types.NamespacedName{Name: volName},
		pvStep1,
	); getErr != nil {
		t.Fatalf("Step 1: PillarVolume CRD not found: %v", getErr)
	}
	if pvStep1.Status.Phase != v1alpha1.PillarVolumePhaseCreatePartial {
		t.Errorf("Step 1: phase: got %q, want CreatePartial", pvStep1.Status.Phase)
	}

	// ── Step 2: retry — ExportVolume now succeeds ─────────────────────────────
	env.AgentMock.ExportVolumeErr = nil // clear the error
	createResp, step2Err := env.Controller.CreateVolume(ctx, req)
	if step2Err != nil {
		t.Fatalf("Step 2: unexpected error on retry: %v", step2Err)
	}
	if createResp.GetVolume() == nil {
		t.Fatal("Step 2: expected a volume in the response, got nil")
	}
	if createResp.GetVolume().GetVolumeId() == "" {
		t.Error("Step 2: VolumeId in response must not be empty")
	}

	// ── Assert: PillarVolume CRD is now Ready with ExportInfo populated ───────
	pvStep2 := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(
		ctx,
		types.NamespacedName{Name: volName},
		pvStep2,
	); getErr != nil {
		t.Fatalf("Step 2: PillarVolume CRD not found: %v", getErr)
	}

	if pvStep2.Status.Phase != v1alpha1.PillarVolumePhaseReady {
		t.Errorf("Step 2: phase: got %q, want Ready", pvStep2.Status.Phase)
	}
	if pvStep2.Status.ExportInfo == nil {
		t.Fatal("Step 2: PillarVolume.Status.ExportInfo is nil; expected export info")
	}
	if pvStep2.Status.ExportInfo.TargetID == "" {
		t.Error("Step 2: ExportInfo.TargetID must not be empty")
	}
	if pvStep2.Status.ExportInfo.Address == "" {
		t.Error("Step 2: ExportInfo.Address must not be empty")
	}
	if pvStep2.Status.PartialFailure != nil {
		t.Errorf("Step 2: PartialFailure should be nil after successful retry, got %+v",
			pvStep2.Status.PartialFailure)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry verifies
// the skipBackend optimisation: when retrying a CreateVolume that previously
// succeeded at the backend step (agent.CreateVolume) but failed at the export
// step, the controller must NOT call agent.CreateVolume a second time
// (the BackendDevicePath is already persisted in the CRD, so re-creating the
// zvol would corrupt data).  ExportVolume is called again on each retry.
func TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newCSIControllerE2EEnv(t, "storage-1")

	const volName = "pvc-partial-agent-call-count"
	req := &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultCreateVolumeParams(),
	}

	// ── First call: ExportVolume fails ────────────────────────────────────────
	env.AgentMock.ExportVolumeErr = errors.New("export fail")
	_, _ = env.Controller.CreateVolume(ctx, req)

	callsAfterFirst := len(env.AgentMock.CreateVolumeCalls)
	exportCallsAfterFirst := len(env.AgentMock.ExportVolumeCalls)
	if callsAfterFirst != 1 {
		t.Errorf("after first (failed) CreateVolume: agent.CreateVolume called %d times, want 1",
			callsAfterFirst)
	}
	if exportCallsAfterFirst != 1 {
		t.Errorf("after first (failed) CreateVolume: agent.ExportVolume called %d times, want 1",
			exportCallsAfterFirst)
	}

	// ── Second call: ExportVolume succeeds ────────────────────────────────────
	env.AgentMock.ExportVolumeErr = nil
	_, retryErr := env.Controller.CreateVolume(ctx, req)
	if retryErr != nil {
		t.Fatalf("retry CreateVolume: unexpected error: %v", retryErr)
	}

	callsAfterRetry := len(env.AgentMock.CreateVolumeCalls)
	exportCallsAfterRetry := len(env.AgentMock.ExportVolumeCalls)

	// agent.CreateVolume must NOT be called again: the controller's skipBackend
	// optimisation skips backend creation when BackendDevicePath is already
	// persisted in the CRD, preventing accidental zvol re-creation.
	if callsAfterRetry != 1 {
		t.Errorf("after retry: agent.CreateVolume called %d times, want 1 (skipBackend must fire)", callsAfterRetry)
	}
	// agent.ExportVolume must be called again because the prior export failed.
	if exportCallsAfterRetry != 2 {
		t.Errorf("after retry: agent.ExportVolume called %d times, want 2", exportCallsAfterRetry)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_DeleteVolume_CleansUpCRD
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_DeleteVolume_CleansUpCRD verifies that a successful
// DeleteVolume removes the PillarVolume CRD from the Kubernetes API.
func TestCSIController_DeleteVolume_CleansUpCRD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newCSIControllerE2EEnv(t, "storage-1")

	const volName = "pvc-delete-crd-001"
	req := &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultCreateVolumeParams(),
	}

	// ── Create the volume ─────────────────────────────────────────────────────
	createResp, err := env.Controller.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume: unexpected error: %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()

	// Verify PillarVolume CRD was created and is in Ready phase.
	pvBefore := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(
		ctx,
		types.NamespacedName{Name: volName},
		pvBefore,
	); getErr != nil {
		t.Fatalf("PillarVolume CRD not found after CreateVolume: %v", getErr)
	}
	if pvBefore.Status.Phase != v1alpha1.PillarVolumePhaseReady {
		t.Errorf("PillarVolume phase before delete: got %q, want Ready", pvBefore.Status.Phase)
	}

	// ── Delete the volume ─────────────────────────────────────────────────────
	_, deleteErr := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if deleteErr != nil {
		t.Fatalf("DeleteVolume: unexpected error: %v", deleteErr)
	}

	// ── Assert: PillarVolume CRD is gone ─────────────────────────────────────
	pvAfter := &v1alpha1.PillarVolume{}
	getErr := env.K8sClient.Get(
		ctx,
		types.NamespacedName{Name: volName},
		pvAfter,
	)
	if getErr == nil {
		t.Fatal("PillarVolume CRD still exists after DeleteVolume; expected it to be deleted")
	}
	if !k8serrors.IsNotFound(getErr) {
		t.Errorf("expected NotFound error after DeleteVolume, got: %v", getErr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates verifies that
// DeleteVolume can clean up a volume that is in the CreatePartial state
// (backend created, export never completed).  The CRD must be removed.
func TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newCSIControllerE2EEnv(t, "storage-1")

	const volName = "pvc-delete-partial-001"
	params := env.defaultCreateVolumeParams()
	createReq := &csi.CreateVolumeRequest{
		Name:               volName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         params,
	}

	// ── Cause a partial failure ───────────────────────────────────────────────
	env.AgentMock.ExportVolumeErr = errors.New("export failed")
	_, _ = env.Controller.CreateVolume(ctx, createReq)

	// Confirm CreatePartial persisted.
	pv := &v1alpha1.PillarVolume{}
	if getErr := env.K8sClient.Get(
		ctx,
		types.NamespacedName{Name: volName},
		pv,
	); getErr != nil {
		t.Fatalf("PillarVolume CRD not found after partial failure: %v", getErr)
	}
	if pv.Status.Phase != v1alpha1.PillarVolumePhaseCreatePartial {
		t.Errorf("phase: got %q, want CreatePartial", pv.Status.Phase)
	}

	// Build the volumeID from the Spec (what would have been in the response if
	// CreateVolume had succeeded).
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

	// ── Assert: PillarVolume CRD is gone ─────────────────────────────────────
	pvAfter := &v1alpha1.PillarVolume{}
	getErr := env.K8sClient.Get(
		ctx,
		types.NamespacedName{Name: volName},
		pvAfter,
	)
	if getErr == nil {
		t.Fatal("PillarVolume CRD still exists after DeleteVolume on partial volume")
	}
	if !k8serrors.IsNotFound(getErr) {
		t.Errorf("expected NotFound error, got: %v", getErr)
	}
}
