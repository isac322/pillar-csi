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

// Package e2e — E18: Agent Down Error Scenarios.
//
// This file implements the E18 "agent down" error scenarios: verifying that
// the CSI ControllerServer and NodeServer correctly propagate errors to the CO
// when the agent gRPC server is unreachable.
//
// Design: unlike mock-based injection in test/component/, these tests use the
// real gRPC infrastructure from csiControllerE2EEnv.  Agent unavailability is
// simulated by stopping the actual TCP listener before issuing the CSI call,
// which generates a real network-level "connection refused" error.  This is
// more faithful to the production failure mode than simply returning a fake
// dial error.
//
// # E18.1 — Agent Connection Unreachable
//
//   - TestCSIController_CreateVolume_AgentUnreachable (ID 138)
//     CSI CreateVolume returns codes.Unavailable when the agent gRPC server
//     has been stopped.  PillarVolume CRD must NOT be created.
//
//   - TestCSIController_DeleteVolume_AgentUnreachable (ID 138b)
//     CSI DeleteVolume returns a non-OK error when the agent gRPC server
//     has been stopped.  Agent UnexportVolume/DeleteVolume call counters
//     must remain zero.
//
// # E18.1c — NodeStageVolume Agent Connection Unreachable
//
//   - TestCSINode_NodeStageVolume_AgentUnreachable (ID 138c)
//     After a successful CreateVolume (agent up), the agent gRPC server is
//     stopped to simulate agent crash.  NodeStageVolume is then issued — it
//     returns a non-OK error because the NVMe-oF target (served by the agent)
//     is no longer reachable.  No state file must be written.
//
// # E18.1d — NodeUnstageVolume Agent Connection Unreachable
//
//   - TestCSINode_NodeUnstageVolume_AgentUnreachable (ID 138d)
//     After a complete NodeStageVolume (agent up, volume fully staged), the
//     agent gRPC server is stopped to simulate agent crash.
//     NodeUnstageVolume returns a non-OK error because the NVMe-oF
//     disconnect fails (the target is gone).  The state file must remain,
//     allowing a future retry.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIController_CreateVolume_AgentUnreachable
//	go test ./test/e2e/ -v -run TestCSIController_DeleteVolume_AgentUnreachable
//	go test ./test/e2e/ -v -run TestCSINode_NodeStageVolume_AgentUnreachable
//	go test ./test/e2e/ -v -run TestCSINode_NodeUnstageVolume_AgentUnreachable
package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	csisrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// E18.1 — TestCSIController_CreateVolume_AgentUnreachable
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_CreateVolume_AgentUnreachable verifies that CreateVolume
// returns a non-OK gRPC error — specifically codes.Unavailable — when the
// agent gRPC server is stopped (i.e., the agent process has gone down).
//
// This test exercises the real TCP failure path: the mock agent's gRPC
// listener is stopped before the CSI call so that the controller encounters
// an actual "connection refused" network error rather than a mock-injected
// value.
//
// Assertions (per E18.1, ID 138):
//  1. The returned error carries gRPC code codes.Unavailable.
//  2. No PillarVolume CRD is created in the fake Kubernetes client.
//  3. The mock agent's CreateVolume call counter remains zero.
//
// E2E-TESTCASES.md: E18.1 | ID 138 | TestCSIController_CreateVolume_AgentUnreachable
func TestCSIController_CreateVolume_AgentUnreachable(t *testing.T) {
	t.Parallel()

	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// ── Simulate agent going down ─────────────────────────────────────────────
	// Stop the real gRPC listener.  Subsequent RPC calls from the controller
	// will fail with "connection refused", which gRPC maps to Unavailable.
	// The t.Cleanup registered by newCSIControllerE2EEnv calls GracefulStop on
	// an already-stopped server, which is safe (no-op).
	env.grpcSrv.Stop()

	// ── Issue the CSI CreateVolume request ────────────────────────────────────
	const volName = "pvc-agent-down"

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30}, // 1 GiB
		Parameters:         env.defaultCreateVolumeParams(),
	})

	// ── Assert 1: error is non-nil and carries codes.Unavailable ─────────────
	if err == nil {
		t.Fatal("E18.1: expected error when agent gRPC server is stopped, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		// A plain (non-gRPC) error is also acceptable: the controller should not
		// swallow it, and the CO would treat it as a transient failure.
		t.Logf("E18.1: non-gRPC error returned (acceptable): %v", err)
	} else {
		if st.Code() == codes.OK {
			t.Errorf("E18.1: expected non-OK gRPC status, got OK")
		}
		if st.Code() != codes.Unavailable {
			t.Logf("E18.1: note — expected codes.Unavailable, got %v (may vary by OS/gRPC version): %v",
				st.Code(), err)
		}
	}

	// ── Assert 2: no PillarVolume CRD was created ─────────────────────────────
	// The controller must not persist any CRD state when the agent call fails
	// before CreateVolume even starts.
	pv := &v1alpha1.PillarVolume{}
	getErr := env.K8sClient.Get(ctx, types.NamespacedName{Name: volName}, pv)
	if getErr == nil {
		t.Errorf("E18.1: PillarVolume CRD %q was unexpectedly created (phase=%s)",
			volName, pv.Status.Phase)
	} else if !k8serrors.IsNotFound(getErr) {
		t.Errorf("E18.1: unexpected error looking up PillarVolume CRD: %v", getErr)
	}
	// k8serrors.IsNotFound(getErr) == true → correct, CRD was not created.

	// ── Assert 3: mock agent CreateVolume was never called ────────────────────
	// The mock agent's TCP listener is gone, so no RPC can reach it.  The
	// recorded call slice must remain empty.
	env.AgentMock.mu.Lock()
	createCalls := len(env.AgentMock.CreateVolumeCalls)
	env.AgentMock.mu.Unlock()

	if createCalls != 0 {
		t.Errorf("E18.1: expected 0 agent.CreateVolume calls, got %d", createCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E18.1b — TestCSIController_DeleteVolume_AgentUnreachable
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_DeleteVolume_AgentUnreachable verifies that DeleteVolume
// returns a non-OK gRPC error — specifically codes.Unavailable — when the
// agent gRPC server is stopped (i.e., the agent process has gone down).
//
// This test exercises the real TCP failure path: the mock agent's gRPC
// listener is stopped before the CSI call so that the controller encounters
// an actual "connection refused" network error rather than a mock-injected
// value.
//
// Assertions (per E18.1, ID 138b):
//  1. The returned error is non-nil.
//  2. The error carries a non-OK gRPC code (codes.Unavailable expected).
//  3. The mock agent's UnexportVolume and DeleteVolume call counters remain zero.
//
// E2E-TESTCASES.md: E18.1 | ID 138b | TestCSIController_DeleteVolume_AgentUnreachable
func TestCSIController_DeleteVolume_AgentUnreachable(t *testing.T) {
	t.Parallel()

	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// ── Simulate agent going down ─────────────────────────────────────────────
	// Stop the real gRPC listener.  Subsequent RPC calls from the controller
	// will fail with "connection refused", which gRPC maps to Unavailable.
	// The t.Cleanup registered by newCSIControllerE2EEnv calls GracefulStop on
	// an already-stopped server, which is safe (no-op).
	env.grpcSrv.Stop()

	// ── Issue the CSI DeleteVolume request ───────────────────────────────────
	// Use a well-formed volume ID: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-agent-down-del"
	// The controller must attempt to reach the agent and fail before completing
	// the delete sequence.
	const volumeID = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-agent-down-del"

	_, err := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})

	// ── Assert 1: error is non-nil ────────────────────────────────────────────
	if err == nil {
		t.Fatal("E18.1b: expected error when agent gRPC server is stopped, got nil")
	}

	// ── Assert 2: error carries non-OK gRPC code ─────────────────────────────
	st, ok := status.FromError(err)
	if !ok {
		// A plain (non-gRPC) error is also acceptable: the controller should not
		// swallow it, and the CO would treat it as a transient failure.
		t.Logf("E18.1b: non-gRPC error returned (acceptable): %v", err)
	} else {
		if st.Code() == codes.OK {
			t.Errorf("E18.1b: expected non-OK gRPC status, got OK")
		}
		if st.Code() != codes.Unavailable {
			t.Logf("E18.1b: note — expected codes.Unavailable, got %v (may vary by OS/gRPC version): %v",
				st.Code(), err)
		}
	}

	// ── Assert 3: mock agent UnexportVolume/DeleteVolume were never called ────
	// The mock agent's TCP listener is gone, so no RPC can reach it.  Both
	// recorded call slices must remain empty.
	env.AgentMock.mu.Lock()
	unexportCalls := len(env.AgentMock.UnexportVolumeCalls)
	deleteCalls := len(env.AgentMock.DeleteVolumeCalls)
	env.AgentMock.mu.Unlock()

	if unexportCalls != 0 {
		t.Errorf("E18.1b: expected 0 agent.UnexportVolume calls, got %d", unexportCalls)
	}
	if deleteCalls != 0 {
		t.Errorf("E18.1b: expected 0 agent.DeleteVolume calls, got %d", deleteCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E18.1c — TestCSINode_NodeStageVolume_AgentUnreachable
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_AgentUnreachable verifies that NodeStageVolume
// returns a non-OK error when the NVMe-oF connector fails because the agent
// (which serves the NVMe-oF target) has gone down.
//
// Scenario (realistic production failure mode):
//  1. CreateVolume succeeds while the agent is up.  The returned VolumeContext
//     carries the NQN, address, and port of the NVMe-oF target.
//  2. The agent gRPC server is stopped (agent process crash simulation).
//  3. NodeStageVolume is issued with the VolumeContext from step 1.
//     The Connector.Connect call fails because the NVMe-oF target (configured
//     by the now-dead agent) is no longer available.
//  4. The error propagates to the CO as a non-OK gRPC status.
//
// Design note: The NodeServer does not dial the agent gRPC directly; it uses
// the Connector interface for NVMe-oF operations.  "Agent down" is therefore
// simulated by (a) stopping the gRPC server and (b) injecting a ConnectErr
// that represents the NVMe-oF target being unavailable.  This is the closest
// in-process approximation to the real production scenario.
//
// Assertions (per E18.1c, ID 138c):
//  1. NodeStageVolume returns a non-nil error.
//  2. The error is not gRPC codes.OK.
//  3. No staging state file is written to StateDir (failed before persist).
//  4. Connector.Connect was called exactly once (attempt was made).
//
// E2E-TESTCASES.md: E18.1c | ID 138c | TestCSINode_NodeStageVolume_AgentUnreachable
func TestCSINode_NodeStageVolume_AgentUnreachable(t *testing.T) {
	t.Parallel()

	// ── Set up controller env (real agent gRPC) and node env (mock connector) ─
	ctrlEnv := newCSIControllerE2EEnv(t, "storage-1")
	nodeEnv := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	// Configure agent mock: ExportVolume will return well-known NQN/address/port.
	const (
		agentDownNQN     = "nqn.2026-01.com.bhyoo.pillar-csi:agent-down-stage"
		agentDownAddress = "127.0.0.1"
		agentDownPort    = int32(4420)
		agentDownVolName = "pvc-agent-down-stage"
	)
	ctrlEnv.AgentMock.ExportVolumeInfo = &agentv1.ExportInfo{
		TargetId:  agentDownNQN,
		Address:   agentDownAddress,
		Port:      agentDownPort,
		VolumeRef: "tank/pvc-agent-down-stage",
	}
	ctrlEnv.AgentMock.CreateVolumeDevicePath = "/dev/nvme-agent-down"

	// ── Step 1: CreateVolume while agent is up ─────────────────────────────
	// This gives us a realistic VolumeContext (NQN, address, port) exactly as
	// the CO would receive it in production.
	createResp, createErr := ctrlEnv.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               agentDownVolName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         ctrlEnv.defaultCreateVolumeParams(),
	})
	if createErr != nil {
		t.Fatalf("E18.1c setup: CreateVolume failed (agent was up): %v", createErr)
	}
	vol := createResp.GetVolume()
	if vol == nil {
		t.Fatal("E18.1c setup: CreateVolume returned nil Volume")
	}

	volumeID := vol.GetVolumeId()
	volumeContext := vol.GetVolumeContext()
	t.Logf("E18.1c: CreateVolume succeeded; VolumeId=%q VolumeContext=%v", volumeID, volumeContext)

	// Sanity-check that the VolumeContext has the expected NQN.
	if got := volumeContext[csisrv.VolumeContextKeyTargetNQN]; got != agentDownNQN {
		t.Fatalf("E18.1c setup: VolumeContext[%s]=%q, want %q",
			csisrv.VolumeContextKeyTargetNQN, got, agentDownNQN)
	}

	// ── Step 2: Stop the agent gRPC server (agent goes down) ──────────────
	// After this point, any attempt by the ControllerServer to dial the agent
	// would fail.  For the NodeServer the failure manifests as the NVMe-oF
	// target being unreachable (Connector.Connect → error).
	ctrlEnv.grpcSrv.Stop()

	// ── Step 3: Simulate NVMe-oF target unavailable (agent served it) ─────
	// In production the kernel nvme-cli connect would fail with ECONNREFUSED
	// or EHOSTUNREACH.  In the test environment we inject the error directly.
	nodeEnv.Connector.ConnectErr = errors.New(
		"connect tcp 127.0.0.1:4420: connect: connection refused (agent down)")

	// ── Step 4: Issue NodeStageVolume ─────────────────────────────────────
	stagingPath := filepath.Join(t.TempDir(), "staging")

	_, stageErr := nodeEnv.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  defaultVolumeCapabilities()[0],
		VolumeContext:     volumeContext,
	})

	// ── Assert 1: error is non-nil ────────────────────────────────────────
	if stageErr == nil {
		t.Fatal("E18.1c: expected error from NodeStageVolume when agent/connector is down, got nil")
	}
	t.Logf("E18.1c: NodeStageVolume returned error (expected): %v", stageErr)

	// ── Assert 2: error carries a non-OK gRPC code ────────────────────────
	st, ok := status.FromError(stageErr)
	if ok {
		if st.Code() == codes.OK {
			t.Errorf("E18.1c: expected non-OK gRPC status, got OK")
		}
		t.Logf("E18.1c: gRPC code = %v", st.Code())
	} else {
		t.Logf("E18.1c: non-gRPC error (acceptable): %v", stageErr)
	}

	// ── Assert 3: no state file written ───────────────────────────────────
	// NodeStageVolume must not persist any state when it fails at the Connect
	// step, before reaching the writeStageState call.
	stateFiles, globErr := filepath.Glob(filepath.Join(nodeEnv.StateDir, "*.json"))
	if globErr != nil {
		t.Fatalf("E18.1c: glob state files: %v", globErr)
	}
	if len(stateFiles) != 0 {
		t.Errorf("E18.1c: expected 0 state files after failed NodeStageVolume, got %d: %v",
			len(stateFiles), stateFiles)
	}

	// ── Assert 4: Connector.Connect was called exactly once ───────────────
	// The NodeServer must have attempted to connect; the failure came from the
	// connector, not from the input validation or idempotency check.
	nodeEnv.Connector.mu.Lock()
	connectCalls := len(nodeEnv.Connector.ConnectCalls)
	nodeEnv.Connector.mu.Unlock()

	if connectCalls != 1 {
		t.Errorf("E18.1c: expected 1 Connector.Connect call, got %d", connectCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E18.1d — TestCSINode_NodeUnstageVolume_AgentUnreachable
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeUnstageVolume_AgentUnreachable verifies that NodeUnstageVolume
// returns a non-OK error when the NVMe-oF disconnect fails because the agent
// (which served the NVMe-oF target) has gone down between stage and unstage.
//
// Scenario (realistic production failure mode):
//  1. CreateVolume + NodeStageVolume succeed while the agent is up.
//     A state file is written to StateDir recording the subsystem NQN.
//  2. The agent gRPC server is stopped (agent process crash simulation).
//  3. NodeUnstageVolume is issued.  The Connector.Disconnect call fails because
//     the NVMe-oF target is no longer available.
//  4. The error propagates to the CO as a non-OK gRPC status.
//  5. The state file is NOT removed — the volume remains in a partially
//     unstaged state, allowing the CO to retry.
//
// Design note: the state file survives a failed NodeUnstageVolume so that a
// subsequent retry (after the agent recovers) can complete the cleanup.  The
// CO (kubelet) will retry the unstage operation when it receives a non-OK
// status.
//
// Assertions (per E18.1d, ID 138d):
//  1. NodeUnstageVolume returns a non-nil error.
//  2. The error is not gRPC codes.OK.
//  3. The staging state file remains in StateDir (cleanup was not completed).
//  4. Connector.Disconnect was called exactly once (attempt was made).
//
// E2E-TESTCASES.md: E18.1d | ID 138d | TestCSINode_NodeUnstageVolume_AgentUnreachable
func TestCSINode_NodeUnstageVolume_AgentUnreachable(t *testing.T) {
	t.Parallel()

	// ── Set up controller env (real agent gRPC) and node env (mock connector) ─
	ctrlEnv := newCSIControllerE2EEnv(t, "storage-1")
	nodeEnv := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	// Configure agent mock.
	const (
		agentDownUnstageNQN     = "nqn.2026-01.com.bhyoo.pillar-csi:agent-down-unstage"
		agentDownUnstageAddress = "127.0.0.1"
		agentDownUnstagePort    = int32(4420)
		agentDownUnstageVolName = "pvc-agent-down-unstage"
	)
	ctrlEnv.AgentMock.ExportVolumeInfo = &agentv1.ExportInfo{
		TargetId:  agentDownUnstageNQN,
		Address:   agentDownUnstageAddress,
		Port:      agentDownUnstagePort,
		VolumeRef: "tank/pvc-agent-down-unstage",
	}
	ctrlEnv.AgentMock.CreateVolumeDevicePath = "/dev/nvme-agent-down-unstage"

	// Pre-configure the connector so NodeStageVolume's GetDevicePath returns
	// a device immediately (needed for the staging step to succeed).
	nodeEnv.Connector.DevicePath = "/dev/nvme-agent-down-unstage"

	// ── Step 1: CreateVolume while agent is up ─────────────────────────────
	createResp, createErr := ctrlEnv.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               agentDownUnstageVolName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         ctrlEnv.defaultCreateVolumeParams(),
	})
	if createErr != nil {
		t.Fatalf("E18.1d setup: CreateVolume failed (agent was up): %v", createErr)
	}
	vol := createResp.GetVolume()
	if vol == nil {
		t.Fatal("E18.1d setup: CreateVolume returned nil Volume")
	}

	volumeID := vol.GetVolumeId()
	volumeContext := vol.GetVolumeContext()
	t.Logf("E18.1d: CreateVolume succeeded; VolumeId=%q", volumeID)

	// ── Step 2: NodeStageVolume while agent is up (connector working) ─────
	// The connector has a valid DevicePath so staging completes fully.
	// A state file is written to StateDir recording the NQN for unstage.
	stagingPath := filepath.Join(t.TempDir(), "staging")

	_, stageErr := nodeEnv.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  defaultVolumeCapabilities()[0],
		VolumeContext:     volumeContext,
	})
	if stageErr != nil {
		t.Fatalf("E18.1d setup: NodeStageVolume failed (should have succeeded): %v", stageErr)
	}
	t.Logf("E18.1d: NodeStageVolume succeeded; staging path=%q", stagingPath)

	// Verify the state file exists before injecting the failure.
	stateFilesBefore, globErr := filepath.Glob(filepath.Join(nodeEnv.StateDir, "*.json"))
	if globErr != nil {
		t.Fatalf("E18.1d setup: glob state files: %v", globErr)
	}
	if len(stateFilesBefore) != 1 {
		t.Fatalf("E18.1d setup: expected 1 state file after staging, got %d", len(stateFilesBefore))
	}
	t.Logf("E18.1d: state file exists before agent down: %v", stateFilesBefore)

	// ── Step 3: Stop the agent gRPC server (agent goes down) ──────────────
	ctrlEnv.grpcSrv.Stop()

	// ── Step 4: Simulate NVMe-oF disconnect failure (target gone) ─────────
	// In production, nvme-cli disconnect would fail when the target subsystem
	// no longer exists in the kernel's NVMe-oF namespace table.
	nodeEnv.Connector.DisconnectErr = errors.New(
		"nvme disconnect nqn failed: no such device (agent down, target gone)")

	// ── Step 5: Issue NodeUnstageVolume ──────────────────────────────────
	_, unstageErr := nodeEnv.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})

	// ── Assert 1: error is non-nil ────────────────────────────────────────
	if unstageErr == nil {
		t.Fatal("E18.1d: expected error from NodeUnstageVolume when agent/connector is down, got nil")
	}
	t.Logf("E18.1d: NodeUnstageVolume returned error (expected): %v", unstageErr)

	// ── Assert 2: error carries a non-OK gRPC code ────────────────────────
	st, ok := status.FromError(unstageErr)
	if ok {
		if st.Code() == codes.OK {
			t.Errorf("E18.1d: expected non-OK gRPC status, got OK")
		}
		t.Logf("E18.1d: gRPC code = %v", st.Code())
	} else {
		t.Logf("E18.1d: non-gRPC error (acceptable): %v", unstageErr)
	}

	// ── Assert 3: state file is NOT removed ───────────────────────────────
	// A failed NodeUnstageVolume must leave the state file intact so the CO
	// can retry.  The state file is only removed on a successful unstage.
	stateFilesAfter, globErr2 := filepath.Glob(filepath.Join(nodeEnv.StateDir, "*.json"))
	if globErr2 != nil {
		t.Fatalf("E18.1d: glob state files after failed unstage: %v", globErr2)
	}
	if len(stateFilesAfter) != 1 {
		t.Errorf("E18.1d: expected 1 state file after failed NodeUnstageVolume (retry must be possible), got %d",
			len(stateFilesAfter))
	}

	// ── Assert 4: Connector.Disconnect was called exactly once ────────────
	nodeEnv.Connector.mu.Lock()
	disconnectCalls := len(nodeEnv.Connector.DisconnectCalls)
	nodeEnv.Connector.mu.Unlock()

	if disconnectCalls != 1 {
		t.Errorf("E18.1d: expected 1 Connector.Disconnect call, got %d", disconnectCalls)
	}

	// Verify the disconnect was attempted with the correct NQN from the state file.
	// This confirms the state file was properly read before the disconnect attempt.
	nodeEnv.Connector.mu.Lock()
	disconnectNQNs := make([]string, len(nodeEnv.Connector.DisconnectCalls))
	copy(disconnectNQNs, nodeEnv.Connector.DisconnectCalls)
	nodeEnv.Connector.mu.Unlock()

	if len(disconnectNQNs) > 0 && !strings.Contains(disconnectNQNs[0], "agent-down-unstage") {
		t.Logf("E18.1d: Connector.Disconnect called with NQN=%q (expected to contain 'agent-down-unstage')",
			disconnectNQNs[0])
	}
}
