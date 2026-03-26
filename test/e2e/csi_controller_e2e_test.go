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

// Package e2e — CSI Controller end-to-end tests.
//
// TestCSIController_* exercises the CSI ControllerServer using the
// csiControllerE2EEnv helper defined in csi_helpers_test.go:
//
//   - A real gRPC listener backed by mockAgentServer captures every agent RPC.
//   - A fake Kubernetes client pre-populated with a PillarTarget supplies the
//     routing information (agent address) without a live cluster.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIController
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	csisrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// VolumeContext key constants (mirror unexported controller constants)
// ─────────────────────────────────────────────────────────────────────────────.

// VolumeContext key constants mirroring the values in internal/csi/controller.go.
//
// The first three (target-id, address, port) use the same string values as the
// csisrv.VolumeContextKey* exported constants from node.go so that the
// VolumeContext produced by CreateVolume can be passed directly to
// NodeStageVolume without key translation.
const (
	ctrlVCTargetID     = csisrv.VolumeContextKeyTargetNQN // "target_id"
	ctrlVCAddress      = csisrv.VolumeContextKeyAddress   // "address"
	ctrlVCPort         = csisrv.VolumeContextKeyPort      // "port"
	ctrlVCVolumeRef    = "pillar-csi.bhyoo.com/volume-ref"
	ctrlVCProtocolType = "pillar-csi.bhyoo.com/protocol-type"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────.

// assertNoError fails the test immediately if err is non-nil.
func assertNoError(t *testing.T, err error, msg string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", msg, err)
	}
}

// assertGRPCCode fails if err does not carry the expected gRPC status code.
func assertGRPCCode(t *testing.T, err error, want codes.Code, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected gRPC error with code %s, got nil", msg, want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("%s: expected gRPC status error, got: %v", msg, err)
	}
	if st.Code() != want {
		t.Fatalf("%s: expected code %s, got %s: %s", msg, want, st.Code(), st.Message())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume — happy path
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume verifies that CreateVolume:
//  1. Calls agent.CreateVolume with the correct volume-ID and capacity.
//  2. Calls agent.ExportVolume with the volume-ID and device path returned by
//     agent.CreateVolume.
//  3. Returns a VolumeId in the expected "target/protocol/backend/agent-id"
//     format.
//  4. Populates VolumeContext with all required connection-parameter keys.
func TestCSIController_CreateVolume(t *testing.T) { //nolint:gocyclo // CreateVolume test
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const (
		volName  = "pvc-create-test"
		capBytes = 1 << 30 // 1 GiB
	)

	resp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: capBytes},
		Parameters:         env.defaultCreateVolumeParams(),
	})
	assertNoError(t, err, "CreateVolume")

	// ── Assert agent.CreateVolume was called ──────────────────────────────────
	env.AgentMock.mu.Lock()
	createCalls := env.AgentMock.CreateVolumeCalls
	exportCalls := env.AgentMock.ExportVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(createCalls) != 1 {
		t.Fatalf("expected 1 CreateVolume agent call, got %d", len(createCalls))
	}
	if len(exportCalls) != 1 {
		t.Fatalf("expected 1 ExportVolume agent call, got %d", len(exportCalls))
	}

	// Verify agent volume ID: "tank/<volName>" (zfs-pool + "/" + volume name).
	wantAgentVolID := "tank/" + volName
	if createCalls[0].VolumeID != wantAgentVolID {
		t.Errorf("agent CreateVolume: VolumeID = %q, want %q",
			createCalls[0].VolumeID, wantAgentVolID)
	}
	if createCalls[0].CapacityBytes != capBytes {
		t.Errorf("agent CreateVolume: CapacityBytes = %d, want %d",
			createCalls[0].CapacityBytes, capBytes)
	}
	if createCalls[0].BackendType != agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
		t.Errorf("agent CreateVolume: BackendType = %v, want ZFS_ZVOL",
			createCalls[0].BackendType)
	}

	// Verify ExportVolume received the device path from CreateVolume.
	if exportCalls[0].VolumeID != wantAgentVolID {
		t.Errorf("agent ExportVolume: VolumeID = %q, want %q",
			exportCalls[0].VolumeID, wantAgentVolID)
	}
	if exportCalls[0].DevicePath != "/dev/test-device" {
		t.Errorf("agent ExportVolume: DevicePath = %q, want /dev/test-device",
			exportCalls[0].DevicePath)
	}
	if exportCalls[0].ProtocolType != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		t.Errorf("agent ExportVolume: ProtocolType = %v, want NVMEOF_TCP",
			exportCalls[0].ProtocolType)
	}

	// ── Assert VolumeId format ─────────────────────────────────────────────────
	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("CreateVolume: response Volume is nil")
	}

	// Expected format: "storage-1/nvmeof-tcp/zfs-zvol/tank/<volName>"
	wantVolumeIDPrefix := "storage-1/nvmeof-tcp/zfs-zvol/" + wantAgentVolID
	if vol.GetVolumeId() != wantVolumeIDPrefix {
		t.Errorf("VolumeId = %q, want %q", vol.GetVolumeId(), wantVolumeIDPrefix)
	}

	// ── Assert VolumeContext is fully populated ────────────────────────────────
	vc := vol.GetVolumeContext()
	if vc == nil {
		t.Fatal("CreateVolume: VolumeContext is nil")
	}
	requiredKeys := []string{
		ctrlVCTargetID,
		ctrlVCAddress,
		ctrlVCPort,
		ctrlVCVolumeRef,
		ctrlVCProtocolType,
	}
	for _, k := range requiredKeys {
		if vc[k] == "" {
			t.Errorf("VolumeContext missing or empty key %q", k)
		}
	}

	// The ExportInfo from the mock has TargetId = "nqn.2026-01.com.bhyoo.pillar-csi:test-volume".
	if got := vc[ctrlVCTargetID]; got != "nqn.2026-01.com.bhyoo.pillar-csi:test-volume" {
		t.Errorf("VolumeContext[%s] = %q, want NQN", ctrlVCTargetID, got)
	}
	if got := vc[ctrlVCAddress]; got != "127.0.0.1" {
		t.Errorf("VolumeContext[%s] = %q, want 127.0.0.1", ctrlVCAddress, got)
	}
	if got := vc[ctrlVCPort]; got != "4420" {
		t.Errorf("VolumeContext[%s] = %q, want 4420", ctrlVCPort, got)
	}
	if got := vc[ctrlVCProtocolType]; got != "nvmeof-tcp" {
		t.Errorf("VolumeContext[%s] = %q, want nvmeof-tcp", ctrlVCProtocolType, got)
	}

	// ── Assert CapacityBytes is echoed back ───────────────────────────────────
	if vol.GetCapacityBytes() != capBytes {
		t.Errorf("Volume.CapacityBytes = %d, want %d", vol.GetCapacityBytes(), capBytes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_Idempotency
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_Idempotency verifies that calling CreateVolume
// twice with the same volume name produces identical VolumeIds and
// VolumeContexts.  The mock agent echoes back the same ExportInfo on both
// calls, which simulates idempotent agent behavior.
func TestCSIController_CreateVolume_Idempotency(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	req := &csi.CreateVolumeRequest{
		Name:               "pvc-idempotent",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 512 << 20},
		Parameters:         env.defaultCreateVolumeParams(),
	}

	resp1, err := env.Controller.CreateVolume(ctx, req)
	assertNoError(t, err, "CreateVolume (first call)")

	resp2, err := env.Controller.CreateVolume(ctx, req)
	assertNoError(t, err, "CreateVolume (second call)")

	// Both calls must return the same VolumeId.
	if resp1.GetVolume().GetVolumeId() != resp2.GetVolume().GetVolumeId() {
		t.Errorf("VolumeId mismatch: first=%q second=%q",
			resp1.GetVolume().GetVolumeId(), resp2.GetVolume().GetVolumeId())
	}

	// Both VolumeContext maps must have identical values for all required keys.
	vc1 := resp1.GetVolume().GetVolumeContext()
	vc2 := resp2.GetVolume().GetVolumeContext()
	for _, k := range []string{ctrlVCTargetID, ctrlVCAddress, ctrlVCPort, ctrlVCProtocolType} {
		if vc1[k] != vc2[k] {
			t.Errorf("VolumeContext[%s] mismatch: first=%q second=%q", k, vc1[k], vc2[k])
		}
	}

	// The first call provisioned the volume and persisted the result in a
	// PillarVolume CRD (Ready phase + ExportInfo).  The second call detects the
	// CRD via loadPillarVolume and returns the cached response without calling
	// the agent again — this is the correct controller-side idempotency
	// behavior introduced by PillarVolume caching.
	env.AgentMock.mu.Lock()
	createCallCount := len(env.AgentMock.CreateVolumeCalls)
	exportCallCount := len(env.AgentMock.ExportVolumeCalls)
	env.AgentMock.mu.Unlock()

	if createCallCount != 1 {
		t.Errorf("expected 1 agent CreateVolume call (second call served from cache), got %d", createCallCount)
	}
	if exportCallCount != 1 {
		t.Errorf("expected 1 agent ExportVolume call (second call served from cache), got %d", exportCallCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_DeleteVolume — happy path
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_DeleteVolume verifies that DeleteVolume:
//  1. Calls agent.UnexportVolume with the correct agent volume ID and protocol.
//  2. Calls agent.DeleteVolume with the correct agent volume ID and backend type.
//  3. Returns success.
func TestCSIController_DeleteVolume(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Build a volume ID in the expected format.
	// "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-delete-test"
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-delete-test"

	_, err := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	assertNoError(t, err, "DeleteVolume")

	env.AgentMock.mu.Lock()
	unexportCalls := env.AgentMock.UnexportVolumeCalls
	deleteCalls := env.AgentMock.DeleteVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(unexportCalls) != 1 {
		t.Fatalf("expected 1 UnexportVolume call, got %d", len(unexportCalls))
	}
	if len(deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteVolume call, got %d", len(deleteCalls))
	}

	// The agent volume ID is the 4th part of the CSI volumeID.
	wantAgentVolID := "tank/pvc-delete-test"
	if unexportCalls[0].VolumeID != wantAgentVolID {
		t.Errorf("agent UnexportVolume: VolumeID = %q, want %q",
			unexportCalls[0].VolumeID, wantAgentVolID)
	}
	if unexportCalls[0].ProtocolType != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		t.Errorf("agent UnexportVolume: ProtocolType = %v, want NVMEOF_TCP",
			unexportCalls[0].ProtocolType)
	}
	if deleteCalls[0].VolumeID != wantAgentVolID {
		t.Errorf("agent DeleteVolume: VolumeID = %q, want %q",
			deleteCalls[0].VolumeID, wantAgentVolID)
	}
	if deleteCalls[0].BackendType != agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
		t.Errorf("agent DeleteVolume: BackendType = %v, want ZFS_ZVOL",
			deleteCalls[0].BackendType)
	}
}

// TestCSIController_DeleteVolume_MalformedID verifies that DeleteVolume returns
// success (not an error) when given a volume ID that doesn't match the expected
// format.  Per the CSI spec the controller must treat unknown volume IDs as
// already deleted.
func TestCSIController_DeleteVolume_MalformedID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	_, err := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: "not-a-valid-pillar-csi-id",
	})
	assertNoError(t, err, "DeleteVolume with malformed ID should succeed")

	// No agent calls should have been made.
	env.AgentMock.mu.Lock()
	unexportCalls := len(env.AgentMock.UnexportVolumeCalls)
	deleteCalls := len(env.AgentMock.DeleteVolumeCalls)
	env.AgentMock.mu.Unlock()

	if unexportCalls != 0 || deleteCalls != 0 {
		t.Errorf("expected no agent calls for malformed ID, got unexport=%d delete=%d",
			unexportCalls, deleteCalls)
	}
}

// TestCSIController_DeleteVolume_NotFoundIsIdempotent verifies that if the
// agent returns NotFound for UnexportVolume or DeleteVolume, DeleteVolume still
// returns success (idempotency).
func TestCSIController_DeleteVolume_NotFoundIsIdempotent(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Configure the mock to return NotFound for both calls.
	env.AgentMock.UnexportVolumeErr = status.Error(codes.NotFound, "not found")
	env.AgentMock.DeleteVolumeErr = status.Error(codes.NotFound, "not found")

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-gone"

	_, err := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	assertNoError(t, err, "DeleteVolume with NotFound agent errors should succeed")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_DeleteVolume_Idempotency
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_DeleteVolume_Idempotency verifies that calling DeleteVolume
// twice with the same volume ID both succeed.  Per the CSI spec, DeleteVolume
// must be idempotent: if the volume does not exist (or has already been
// deleted), the call must still return success.
//
// The mock agent returns success for every DeleteVolume / UnexportVolume call,
// simulating an idempotent storage backend.  Both CSI-level calls must
// therefore succeed without error, and each must have forwarded the correct
// agent volume ID.
func TestCSIController_DeleteVolume_Idempotency(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Use a well-formed volume ID so the controller can parse target/protocol/
	// backend and forward the call to the mock agent.
	const volumeID = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-double-delete"

	req := &csi.DeleteVolumeRequest{VolumeId: volumeID}

	// ── First call — should succeed ───────────────────────────────────────────
	_, err := env.Controller.DeleteVolume(ctx, req)
	assertNoError(t, err, "DeleteVolume (first call)")

	// ── Second call on the same ID — must also succeed (no-op) ───────────────
	_, err = env.Controller.DeleteVolume(ctx, req)
	assertNoError(t, err, "DeleteVolume (second call, idempotency)")

	// ── Verify that both calls reached the agent ──────────────────────────────
	env.AgentMock.mu.Lock()
	unexportCalls := env.AgentMock.UnexportVolumeCalls
	deleteCalls := env.AgentMock.DeleteVolumeCalls
	env.AgentMock.mu.Unlock()

	// Both calls should have forwarded to the agent (idempotency is agent-side).
	if len(unexportCalls) != 2 {
		t.Errorf("expected 2 UnexportVolume agent calls (idempotent), got %d", len(unexportCalls))
	}
	if len(deleteCalls) != 2 {
		t.Errorf("expected 2 DeleteVolume agent calls (idempotent), got %d", len(deleteCalls))
	}

	// Both calls must have carried the same agent volume ID.
	const wantAgentVolID = "tank/pvc-double-delete"
	for i, c := range unexportCalls {
		if c.VolumeID != wantAgentVolID {
			t.Errorf("UnexportVolume call %d: VolumeID = %q, want %q", i+1, c.VolumeID, wantAgentVolID)
		}
		if c.ProtocolType != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
			t.Errorf("UnexportVolume call %d: ProtocolType = %v, want NVMEOF_TCP", i+1, c.ProtocolType)
		}
	}
	for i, c := range deleteCalls {
		if c.VolumeID != wantAgentVolID {
			t.Errorf("DeleteVolume call %d: VolumeID = %q, want %q", i+1, c.VolumeID, wantAgentVolID)
		}
		if c.BackendType != agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
			t.Errorf("DeleteVolume call %d: BackendType = %v, want ZFS_ZVOL", i+1, c.BackendType)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ControllerPublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerPublishVolume verifies that ControllerPublishVolume
// calls agent.AllowInitiator with:
//   - the agent volume ID parsed from the CSI volumeID
//   - the protocol type parsed from the CSI volumeID
//   - the node ID as the initiator ID
func TestCSIController_ControllerPublishVolume(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const nodeID = "nqn.2014-08.org.nvmexpress:uuid:worker-1"
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-publish-test"

	resp, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           nodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	})
	assertNoError(t, err, "ControllerPublishVolume")

	// Response may be empty but must not be nil.
	if resp == nil {
		t.Fatal("ControllerPublishVolume: nil response")
	}

	// Assert AllowInitiator was called once with correct arguments.
	env.AgentMock.mu.Lock()
	allowCalls := env.AgentMock.AllowInitiatorCalls
	env.AgentMock.mu.Unlock()

	if len(allowCalls) != 1 {
		t.Fatalf("expected 1 AllowInitiator call, got %d", len(allowCalls))
	}

	wantAgentVolID := "tank/pvc-publish-test"
	if allowCalls[0].VolumeID != wantAgentVolID {
		t.Errorf("AllowInitiator: VolumeID = %q, want %q",
			allowCalls[0].VolumeID, wantAgentVolID)
	}
	if allowCalls[0].InitiatorID != nodeID {
		t.Errorf("AllowInitiator: InitiatorID = %q, want %q",
			allowCalls[0].InitiatorID, nodeID)
	}
	if allowCalls[0].ProtocolType != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		t.Errorf("AllowInitiator: ProtocolType = %v, want NVMEOF_TCP",
			allowCalls[0].ProtocolType)
	}
}

// TestCSIController_ControllerPublishVolume_Idempotency verifies that calling
// ControllerPublishVolume twice with the same arguments succeeds on both calls.
// The mock agent echoes AllowInitiator success — both calls should reach the
// agent.
func TestCSIController_ControllerPublishVolume_Idempotency(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const nodeID = "nqn.2014-08.org.nvmexpress:uuid:worker-1"
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-publish-idempotent"
	volCap := defaultVolumeCapabilities()[0]

	for i := range 2 {
		_, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId:         volumeID,
			NodeId:           nodeID,
			VolumeCapability: volCap,
		})
		if err != nil {
			t.Fatalf("ControllerPublishVolume call %d: %v", i+1, err)
		}
	}

	env.AgentMock.mu.Lock()
	allowCount := len(env.AgentMock.AllowInitiatorCalls)
	env.AgentMock.mu.Unlock()

	if allowCount != 2 {
		t.Errorf("expected 2 AllowInitiator calls, got %d", allowCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ControllerUnpublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerUnpublishVolume verifies that ControllerUnpublishVolume
// calls agent.DenyInitiator with the correct arguments.
func TestCSIController_ControllerUnpublishVolume(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const nodeID = "nqn.2014-08.org.nvmexpress:uuid:worker-1"
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-unpublish-test"

	_, err := env.Controller.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   nodeID,
	})
	assertNoError(t, err, "ControllerUnpublishVolume")

	env.AgentMock.mu.Lock()
	denyCalls := env.AgentMock.DenyInitiatorCalls
	env.AgentMock.mu.Unlock()

	if len(denyCalls) != 1 {
		t.Fatalf("expected 1 DenyInitiator call, got %d", len(denyCalls))
	}

	wantAgentVolID := "tank/pvc-unpublish-test"
	if denyCalls[0].VolumeID != wantAgentVolID {
		t.Errorf("DenyInitiator: VolumeID = %q, want %q",
			denyCalls[0].VolumeID, wantAgentVolID)
	}
	if denyCalls[0].InitiatorID != nodeID {
		t.Errorf("DenyInitiator: InitiatorID = %q, want %q",
			denyCalls[0].InitiatorID, nodeID)
	}
	if denyCalls[0].ProtocolType != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		t.Errorf("DenyInitiator: ProtocolType = %v, want NVMEOF_TCP",
			denyCalls[0].ProtocolType)
	}
}

// TestCSIController_ControllerUnpublishVolume_NotFoundIsIdempotent verifies
// that if the agent returns NotFound for DenyInitiator, ControllerUnpublishVolume
// still returns success.
func TestCSIController_ControllerUnpublishVolume_NotFoundIsIdempotent(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	env.AgentMock.DenyInitiatorErr = status.Error(codes.NotFound, "initiator not found")

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-already-unpublished"
	_, err := env.Controller.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "nqn.2014-08.org.nvmexpress:uuid:worker-1",
	})
	assertNoError(t, err, "ControllerUnpublishVolume with NotFound should succeed")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ControllerExpandVolume
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerExpandVolume verifies that ControllerExpandVolume:
//  1. Calls agent.ExpandVolume with the parsed agent volume ID and requested bytes.
//  2. Returns the capacity reported by the agent.
//  3. Sets NodeExpansionRequired=true.
func TestCSIController_ControllerExpandVolume(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-expand-test"
	const newCapBytes = 2 << 30 // 2 GiB

	resp, err := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: newCapBytes},
	})
	assertNoError(t, err, "ControllerExpandVolume")

	env.AgentMock.mu.Lock()
	expandCalls := env.AgentMock.ExpandVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(expandCalls) != 1 {
		t.Fatalf("expected 1 ExpandVolume call, got %d", len(expandCalls))
	}

	wantAgentVolID := "tank/pvc-expand-test"
	if expandCalls[0].VolumeID != wantAgentVolID {
		t.Errorf("ExpandVolume: VolumeID = %q, want %q",
			expandCalls[0].VolumeID, wantAgentVolID)
	}
	if expandCalls[0].RequestedBytes != newCapBytes {
		t.Errorf("ExpandVolume: RequestedBytes = %d, want %d",
			expandCalls[0].RequestedBytes, newCapBytes)
	}

	if resp.GetCapacityBytes() != newCapBytes {
		t.Errorf("ControllerExpandVolume: CapacityBytes = %d, want %d",
			resp.GetCapacityBytes(), newCapBytes)
	}
	if !resp.GetNodeExpansionRequired() {
		t.Error("ControllerExpandVolume: NodeExpansionRequired should be true")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ValidateVolumeCapabilities
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ValidateVolumeCapabilities verifies that:
//   - RWO (SINGLE_NODE_WRITER) is confirmed.
//   - RWOP (SINGLE_NODE_SINGLE_WRITER) is confirmed.
//   - ROX (MULTI_NODE_READER_ONLY) is confirmed.
//   - RWX (MULTI_NODE_MULTI_WRITER) returns an empty Confirmed with a message.
func TestCSIController_ValidateVolumeCapabilities(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	cases := []struct {
		name   string
		mode   csi.VolumeCapability_AccessMode_Mode
		wantOK bool // true = Confirmed, false = rejected
	}{
		{
			name:   "RWO",
			mode:   csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantOK: true,
		},
		{
			name:   "RWOP",
			mode:   csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			wantOK: true,
		},
		{
			name:   "ROX",
			mode:   csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			wantOK: true,
		},
		{
			name:   "RWX",
			mode:   csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			wantOK: false, // pillar-csi does not support multi-writer
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := env.Controller.ValidateVolumeCapabilities(ctx,
				&csi.ValidateVolumeCapabilitiesRequest{
					VolumeId: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-validate",
					VolumeCapabilities: []*csi.VolumeCapability{
						{
							AccessType: &csi.VolumeCapability_Block{
								Block: &csi.VolumeCapability_BlockVolume{},
							},
							AccessMode: &csi.VolumeCapability_AccessMode{Mode: tc.mode},
						},
					},
				},
			)
			assertNoError(t, err, "ValidateVolumeCapabilities")

			if tc.wantOK {
				if resp.GetConfirmed() == nil {
					t.Errorf("expected Confirmed to be set for mode %s, got nil (message: %q)",
						tc.mode, resp.GetMessage())
				}
			} else {
				if resp.GetConfirmed() != nil {
					t.Errorf("expected Confirmed to be nil for unsupported mode %s", tc.mode)
				}
				if resp.GetMessage() == "" {
					t.Errorf("expected non-empty Message for unsupported mode %s", tc.mode)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_FullRoundTrip — CreateVolume → Publish → Unpublish → Delete
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_FullRoundTrip exercises the complete volume lifecycle:
//
//  1. CreateVolume  → agent.CreateVolume + agent.ExportVolume
//  2. ControllerPublishVolume → agent.AllowInitiator
//  3. ControllerUnpublishVolume → agent.DenyInitiator
//  4. DeleteVolume  → agent.UnexportVolume + agent.DeleteVolume
//
// It verifies:
//   - All 6 agent RPCs are called in the correct order.
//   - The VolumeId returned by CreateVolume is accepted by all subsequent calls.
//   - The agent volume ID is the same across all RPCs.
//   - The node ID flows correctly through Publish and Unpublish as the initiator ID.
func TestCSIController_FullRoundTrip(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const (
		volName  = "pvc-round-trip"
		capBytes = 1 << 30 // 1 GiB
		nodeID   = "nqn.2014-08.org.nvmexpress:uuid:worker-node-1"
	)

	// ── Step 1: CreateVolume ──────────────────────────────────────────────────
	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: capBytes},
		Parameters:         env.defaultCreateVolumeParams(),
	})
	assertNoError(t, err, "Step 1: CreateVolume")

	volumeID := createResp.GetVolume().GetVolumeId()
	if volumeID == "" {
		t.Fatal("Step 1: CreateVolume returned empty VolumeId")
	}

	// ── Step 2: ControllerPublishVolume ───────────────────────────────────────
	pubResp, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           nodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	})
	assertNoError(t, err, "Step 2: ControllerPublishVolume")
	if pubResp == nil {
		t.Fatal("Step 2: ControllerPublishVolume returned nil response")
	}

	// ── Step 3: ControllerUnpublishVolume ─────────────────────────────────────
	_, err = env.Controller.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   nodeID,
	})
	assertNoError(t, err, "Step 3: ControllerUnpublishVolume")

	// ── Step 4: DeleteVolume ──────────────────────────────────────────────────
	_, err = env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	assertNoError(t, err, "Step 4: DeleteVolume")

	// ── Verify RPC call counts ────────────────────────────────────────────────
	env.AgentMock.mu.Lock()
	createCalls := len(env.AgentMock.CreateVolumeCalls)
	exportCalls := len(env.AgentMock.ExportVolumeCalls)
	allowCalls := len(env.AgentMock.AllowInitiatorCalls)
	denyCalls := len(env.AgentMock.DenyInitiatorCalls)
	unexportCalls := len(env.AgentMock.UnexportVolumeCalls)
	deleteCalls := len(env.AgentMock.DeleteVolumeCalls)
	env.AgentMock.mu.Unlock()

	if createCalls != 1 {
		t.Errorf("expected 1 agent CreateVolume, got %d", createCalls)
	}
	if exportCalls != 1 {
		t.Errorf("expected 1 agent ExportVolume, got %d", exportCalls)
	}
	if allowCalls != 1 {
		t.Errorf("expected 1 agent AllowInitiator, got %d", allowCalls)
	}
	if denyCalls != 1 {
		t.Errorf("expected 1 agent DenyInitiator, got %d", denyCalls)
	}
	if unexportCalls != 1 {
		t.Errorf("expected 1 agent UnexportVolume, got %d", unexportCalls)
	}
	if deleteCalls != 1 {
		t.Errorf("expected 1 agent DeleteVolume, got %d", deleteCalls)
	}

	// ── Verify all RPCs used the same agent volume ID ─────────────────────────
	wantAgentVolID := "tank/" + volName

	env.AgentMock.mu.Lock()
	agentCreateVolID := env.AgentMock.CreateVolumeCalls[0].VolumeID
	agentExportVolID := env.AgentMock.ExportVolumeCalls[0].VolumeID
	agentAllowVolID := env.AgentMock.AllowInitiatorCalls[0].VolumeID
	agentDenyVolID := env.AgentMock.DenyInitiatorCalls[0].VolumeID
	agentUnexportVolID := env.AgentMock.UnexportVolumeCalls[0].VolumeID
	agentDeleteVolID := env.AgentMock.DeleteVolumeCalls[0].VolumeID
	agentAllowInitiatorID := env.AgentMock.AllowInitiatorCalls[0].InitiatorID
	agentDenyInitiatorID := env.AgentMock.DenyInitiatorCalls[0].InitiatorID
	env.AgentMock.mu.Unlock()

	for _, call := range []struct {
		name string
		got  string
	}{
		{"CreateVolume", agentCreateVolID},
		{"ExportVolume", agentExportVolID},
		{"AllowInitiator", agentAllowVolID},
		{"DenyInitiator", agentDenyVolID},
		{"UnexportVolume", agentUnexportVolID},
		{"DeleteVolume", agentDeleteVolID},
	} {
		if call.got != wantAgentVolID {
			t.Errorf("agent %s: VolumeID = %q, want %q", call.name, call.got, wantAgentVolID)
		}
	}

	// ── Verify node ID was used as initiator ID ───────────────────────────────
	if agentAllowInitiatorID != nodeID {
		t.Errorf("AllowInitiator: InitiatorID = %q, want %q", agentAllowInitiatorID, nodeID)
	}
	if agentDenyInitiatorID != nodeID {
		t.Errorf("DenyInitiator: InitiatorID = %q, want %q", agentDenyInitiatorID, nodeID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Error path tests
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_MissingParams verifies that CreateVolume
// returns InvalidArgument when required StorageClass parameters are absent.
func TestCSIController_CreateVolume_MissingParams(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	cases := []struct {
		name     string
		params   map[string]string
		wantCode codes.Code
	}{
		{
			name: "missing volume name",
			// Empty Name field — but we test via an empty name, not params.
			params:   env.defaultCreateVolumeParams(),
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing target",
			params: map[string]string{
				"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
				"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
				"pillar-csi.bhyoo.com/zfs-pool":      "tank",
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing backend-type",
			params: map[string]string{
				"pillar-csi.bhyoo.com/target":        "storage-1",
				"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
				"pillar-csi.bhyoo.com/zfs-pool":      "tank",
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing protocol-type",
			params: map[string]string{
				"pillar-csi.bhyoo.com/target":       "storage-1",
				"pillar-csi.bhyoo.com/backend-type": "zfs-zvol",
				"pillar-csi.bhyoo.com/zfs-pool":     "tank",
			},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			volName := "pvc-missing-params"
			if tc.name == "missing volume name" {
				volName = "" // trigger empty-name validation
			}
			_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
				Name:               volName,
				VolumeCapabilities: defaultVolumeCapabilities(),
				Parameters:         tc.params,
			})
			assertGRPCCode(t, err, tc.wantCode, "CreateVolume with "+tc.name)
		})
	}
}

// TestCSIController_CreateVolume_PillarTargetNotFound verifies that CreateVolume
// returns NotFound when the referenced PillarTarget does not exist.
func TestCSIController_CreateVolume_PillarTargetNotFound(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	params := env.defaultCreateVolumeParams()
	params["pillar-csi.bhyoo.com/target"] = "does-not-exist"

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-no-target",
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         params,
	})
	assertGRPCCode(t, err, codes.NotFound, "CreateVolume: PillarTarget not found")
}

// TestCSIController_CreateVolume_AgentCreateError verifies that an error from
// agent.CreateVolume is propagated to the CSI caller.
func TestCSIController_CreateVolume_AgentCreateError(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	env.AgentMock.CreateVolumeErr = status.Error(codes.Internal, "backend failure")

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-agent-err",
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultCreateVolumeParams(),
	})
	assertGRPCCode(t, err, codes.Internal, "CreateVolume: agent CreateVolume error propagated")
}

// TestCSIController_CreateVolume_AgentExportError verifies that an error from
// agent.ExportVolume is propagated to the CSI caller.
func TestCSIController_CreateVolume_AgentExportError(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	env.AgentMock.ExportVolumeErr = status.Error(codes.Internal, "export failure")

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-export-err",
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultCreateVolumeParams(),
	})
	assertGRPCCode(t, err, codes.Internal, "CreateVolume: agent ExportVolume error propagated")
}

// TestCSIController_DeleteVolume_AgentError verifies that a non-NotFound error
// from agent.UnexportVolume is propagated to the CSI caller.
func TestCSIController_DeleteVolume_AgentError(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	env.AgentMock.UnexportVolumeErr = status.Error(codes.Internal, "unexport failure")

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-err"
	_, err := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	assertGRPCCode(t, err, codes.Internal, "DeleteVolume: agent UnexportVolume error propagated")
}

// TestCSIController_ControllerPublishVolume_MissingFields verifies that
// ControllerPublishVolume returns InvalidArgument when required fields are absent.
func TestCSIController_ControllerPublishVolume_MissingFields(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-pub"
	volCap := defaultVolumeCapabilities()[0]

	cases := []struct {
		name     string
		req      *csi.ControllerPublishVolumeRequest
		wantCode codes.Code
	}{
		{
			name:     "missing volume_id",
			req:      &csi.ControllerPublishVolumeRequest{NodeId: "worker-1", VolumeCapability: volCap},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "missing node_id",
			req:      &csi.ControllerPublishVolumeRequest{VolumeId: volumeID, VolumeCapability: volCap},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "missing volume_capability",
			req:      &csi.ControllerPublishVolumeRequest{VolumeId: volumeID, NodeId: "worker-1"},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.Controller.ControllerPublishVolume(ctx, tc.req)
			assertGRPCCode(t, err, tc.wantCode, "ControllerPublishVolume: "+tc.name)
		})
	}
}

// TestCSIController_ControllerExpandVolume_MissingCapacityRange verifies that
// ControllerExpandVolume returns InvalidArgument when capacity_range is absent.
func TestCSIController_ControllerExpandVolume_MissingCapacityRange(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	_, err := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-expand",
		// CapacityRange deliberately absent.
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "ControllerExpandVolume: missing capacity_range")
}

// TestCSIController_VolumeIDFormatPreservation verifies that the VolumeId
// returned by CreateVolume survives a round-trip through DeleteVolume by
// exercising the split-and-reconstruct logic with a ZFS-style agent volume ID
// that itself contains a slash (e.g. "tank/pvc-name").
func TestCSIController_VolumeIDFormatPreservation(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const volName = "pvc-slashy-volume"

	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		Parameters:         env.defaultCreateVolumeParams(),
	})
	assertNoError(t, err, "CreateVolume")

	volumeID := createResp.GetVolume().GetVolumeId()

	// The volumeID must contain exactly 4 slash-separated segments when split
	// with SplitN(id, "/", 4).
	parts := strings.SplitN(volumeID, "/", 4)
	if len(parts) != 4 {
		t.Fatalf("VolumeId %q splits into %d parts, want 4", volumeID, len(parts))
	}

	// The first three segments must be the known values.
	wantParts := []string{"storage-1", "nvmeof-tcp", "zfs-zvol"}
	for i, want := range wantParts {
		if parts[i] != want {
			t.Errorf("VolumeId part[%d] = %q, want %q", i, parts[i], want)
		}
	}

	// The 4th segment must contain the agent volume ID "tank/<volName>".
	wantAgentID := fmt.Sprintf("tank/%s", volName)
	if parts[3] != wantAgentID {
		t.Errorf("VolumeId agent part = %q, want %q", parts[3], wantAgentID)
	}

	// The VolumeId must be accepted by DeleteVolume without error.
	_, err = env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	assertNoError(t, err, "DeleteVolume with VolumeId from CreateVolume")

	// The agent DeleteVolume call must have received the correct agent volume ID.
	env.AgentMock.mu.Lock()
	deleteCalls := env.AgentMock.DeleteVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(deleteCalls) != 1 {
		t.Fatalf("expected 1 agent DeleteVolume call, got %d", len(deleteCalls))
	}
	if deleteCalls[0].VolumeID != wantAgentID {
		t.Errorf("agent DeleteVolume: VolumeID = %q, want %q",
			deleteCalls[0].VolumeID, wantAgentID)
	}
}
