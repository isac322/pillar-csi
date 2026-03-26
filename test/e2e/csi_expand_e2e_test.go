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

// Package e2e — E11: Volume Expansion integration end-to-end tests.
//
// TestCSIExpand_* exercises the full volume expansion path:
//
//	ControllerExpandVolume → agent.ExpandVolume (storage backend)
//	NodeExpandVolume       → Resizer.ResizeFS   (filesystem resize)
//
// All tests use the in-process fake infrastructure (mockAgentServer,
// mockCSIConnector, mockCSIMounter, mockExpandResizer) — no running
// Kubernetes cluster or storage agent is required.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIExpand
package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csisrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// mockExpandResizer — test double for csisrv.Resizer
// ─────────────────────────────────────────────────────────────────────────────

// resizeFSCall records the arguments of a single ResizeFS invocation.
type resizeFSCall struct {
	MountPath string
	FsType    string
}

// mockExpandResizer is a programmable test double for the csisrv.Resizer
// interface.  It records every ResizeFS invocation and returns the configured
// error (nil by default).
type mockExpandResizer struct {
	// ResizeFSErr is returned by ResizeFS, or nil for success.
	ResizeFSErr error

	// Recorded calls.
	ResizeFSCalls []resizeFSCall
}

// Compile-time interface check.
var _ csisrv.Resizer = (*mockExpandResizer)(nil)

// ResizeFS implements csisrv.Resizer.
func (m *mockExpandResizer) ResizeFS(mountPath, fsType string) error {
	m.ResizeFSCalls = append(m.ResizeFSCalls, resizeFSCall{
		MountPath: mountPath,
		FsType:    fsType,
	})
	return m.ResizeFSErr
}

// ─────────────────────────────────────────────────────────────────────────────
// csiExpandE2EEnv — combined controller + node environment for expansion tests
// ─────────────────────────────────────────────────────────────────────────────

// csiExpandE2EEnv wires up both the CSI ControllerServer and NodeServer
// together with a shared mockAgentServer so volume expansion tests can exercise
// the full ControllerExpandVolume → NodeExpandVolume path.
type csiExpandE2EEnv struct {
	// Controller is the CSI ControllerServer under test.
	Controller *csisrv.ControllerServer

	// Node is the CSI NodeServer under test (with injected mock Resizer).
	Node *csisrv.NodeServer

	// AgentMock is the programmable gRPC server double.
	AgentMock *mockAgentServer

	// Resizer is the mock Resizer injected into the NodeServer.
	Resizer *mockExpandResizer

	// TargetName is the PillarTarget name used in CSI volume IDs.
	TargetName string
}

// newCSIExpandE2EEnv creates a csiExpandE2EEnv for the duration of a single
// test.  Cleanup is registered via t.Cleanup.
func newCSIExpandE2EEnv(t *testing.T, targetName string) *csiExpandE2EEnv {
	t.Helper()

	// Build the controller environment (real gRPC listener + fake k8s client).
	ctrl := newCSIControllerE2EEnv(t, targetName)

	// Build a NodeServer with a mock Resizer (no real resize tools required).
	resizer := &mockExpandResizer{}
	node := csisrv.NewNodeServerWithStateDir(
		"worker-expand",
		&mockCSIConnector{DevicePath: "/dev/nvme0n1"},
		newMockCSIMounter(),
		t.TempDir(),
	).WithResizer(resizer)

	return &csiExpandE2EEnv{
		Controller: ctrl.Controller,
		Node:       node,
		AgentMock:  ctrl.AgentMock,
		Resizer:    resizer,
		TargetName: targetName,
	}
}

// expandVolumeID constructs a CSI volume ID in the standard
// "<target>/<protocol>/<backend>/<agentVolID>" format used by the controller.
func expandVolumeID(targetName, agentVolID string) string {
	return targetName + "/nvmeof-tcp/zfs-zvol/" + agentVolID
}

// ─────────────────────────────────────────────────────────────────────────────
// E11.1 — ControllerExpandVolume: agent delegation and node_expansion_required
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIExpand_ControllerExpandVolume_ForwardsToAgent verifies that
// ControllerExpandVolume delegates to agent.ExpandVolume with the correct
// VolumeId and RequestedBytes, and returns NodeExpansionRequired=true.
//
// Test case 88 from E11.1.
func TestCSIExpand_ControllerExpandVolume_ForwardsToAgent(t *testing.T) {
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-1")
	ctx := context.Background()

	const newSize int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

	// Set agent response capacity to match the requested size.
	env.AgentMock.ExpandVolumeCapacityBytes = newSize

	agentVolID := "tank/pvc-expand-88"
	volID := expandVolumeID(env.TargetName, agentVolID)

	resp, err := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      volID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: newSize},
	})
	assertNoError(t, err, "ControllerExpandVolume")

	// Verify agent.ExpandVolume was called exactly once.
	env.AgentMock.mu.Lock()
	expandCalls := env.AgentMock.ExpandVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(expandCalls) != 1 {
		t.Fatalf("expected 1 agent.ExpandVolume call, got %d", len(expandCalls))
	}
	if expandCalls[0].VolumeID != agentVolID {
		t.Errorf("agent.ExpandVolume VolumeID = %q, want %q",
			expandCalls[0].VolumeID, agentVolID)
	}
	if expandCalls[0].RequestedBytes != newSize {
		t.Errorf("agent.ExpandVolume RequestedBytes = %d, want %d",
			expandCalls[0].RequestedBytes, newSize)
	}

	// Verify response: CapacityBytes=2GiB, NodeExpansionRequired=true.
	if resp.GetCapacityBytes() != newSize {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), newSize)
	}
	if !resp.GetNodeExpansionRequired() {
		t.Error("NodeExpansionRequired should be true")
	}
}

// TestCSIExpand_ControllerExpandVolume_AgentReturnsZeroCapacity verifies that
// when agent.ExpandVolume returns CapacityBytes=0, ControllerExpandVolume falls
// back to the RequiredBytes from the request.
//
// Test case 89 from E11.1.
func TestCSIExpand_ControllerExpandVolume_AgentReturnsZeroCapacity(t *testing.T) {
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-2")
	ctx := context.Background()

	const reqSize int64 = 3 * 1024 * 1024 * 1024 // 3 GiB

	// Agent returns CapacityBytes=0 → should fall back to reqSize.
	env.AgentMock.ExpandVolumeCapacityBytes = 0

	volID := expandVolumeID(env.TargetName, "tank/pvc-expand-89")

	resp, err := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      volID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: reqSize},
	})
	assertNoError(t, err, "ControllerExpandVolume (agent returns 0)")

	// When agent returns 0, controller should fall back to RequiredBytes.
	if resp.GetCapacityBytes() != reqSize {
		t.Errorf("CapacityBytes = %d, want %d (RequiredBytes fallback)",
			resp.GetCapacityBytes(), reqSize)
	}
	if !resp.GetNodeExpansionRequired() {
		t.Error("NodeExpansionRequired should be true even when agent returns 0")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E11.2 — NodeExpandVolume: filesystem-type-specific resize
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIExpand_NodeExpandVolume_Ext4 verifies that NodeExpandVolume calls
// ResizeFS with fsType="ext4" when the VolumeCapability specifies ext4.
//
// Test case 90 from E11.2.
func TestCSIExpand_NodeExpandVolume_Ext4(t *testing.T) {
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-3")
	ctx := context.Background()

	const (
		volumePath = "/mnt/staging/pvc-expand-90"
		reqSize    = int64(2 * 1024 * 1024 * 1024) // 2 GiB
	)

	resp, err := env.Node.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{
		VolumeId:   "pvc-expand-90",
		VolumePath: volumePath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		CapacityRange: &csi.CapacityRange{RequiredBytes: reqSize},
	})
	assertNoError(t, err, "NodeExpandVolume ext4")

	// Verify ResizeFS was called exactly once with the correct arguments.
	if len(env.Resizer.ResizeFSCalls) != 1 {
		t.Fatalf("expected 1 ResizeFS call, got %d", len(env.Resizer.ResizeFSCalls))
	}
	call := env.Resizer.ResizeFSCalls[0]
	if call.FsType != "ext4" {
		t.Errorf("ResizeFS FsType = %q, want %q", call.FsType, "ext4")
	}
	if call.MountPath != volumePath {
		t.Errorf("ResizeFS MountPath = %q, want %q", call.MountPath, volumePath)
	}

	// CapacityBytes should echo RequiredBytes.
	if resp.GetCapacityBytes() != reqSize {
		t.Errorf("NodeExpandVolume CapacityBytes = %d, want %d", resp.GetCapacityBytes(), reqSize)
	}
}

// TestCSIExpand_NodeExpandVolume_XFS verifies that NodeExpandVolume calls
// ResizeFS with fsType="xfs" when the VolumeCapability specifies xfs.
//
// Test case 91 from E11.2.
func TestCSIExpand_NodeExpandVolume_XFS(t *testing.T) {
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-4")
	ctx := context.Background()

	const (
		volumePath = "/mnt/staging/pvc-expand-91"
		reqSize    = int64(4 * 1024 * 1024 * 1024) // 4 GiB
	)

	resp, err := env.Node.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{
		VolumeId:   "pvc-expand-91",
		VolumePath: volumePath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		CapacityRange: &csi.CapacityRange{RequiredBytes: reqSize},
	})
	assertNoError(t, err, "NodeExpandVolume xfs")

	// Verify ResizeFS was called exactly once with fsType="xfs".
	if len(env.Resizer.ResizeFSCalls) != 1 {
		t.Fatalf("expected 1 ResizeFS call, got %d", len(env.Resizer.ResizeFSCalls))
	}
	call := env.Resizer.ResizeFSCalls[0]
	if call.FsType != "xfs" {
		t.Errorf("ResizeFS FsType = %q, want %q", call.FsType, "xfs")
	}
	if call.MountPath != volumePath {
		t.Errorf("ResizeFS MountPath = %q, want %q", call.MountPath, volumePath)
	}

	// CapacityBytes should echo RequiredBytes.
	if resp.GetCapacityBytes() != reqSize {
		t.Errorf("NodeExpandVolume CapacityBytes = %d, want %d", resp.GetCapacityBytes(), reqSize)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E11.3 — Full expansion round trip
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIExpand_FullExpandRoundTrip verifies the complete expansion flow:
// CreateVolume → ControllerExpandVolume → NodeExpandVolume.
//
// Test case 92 from E11.3.
func TestCSIExpand_FullExpandRoundTrip(t *testing.T) { //nolint:gocyclo // full round-trip test
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-5")
	ctx := context.Background()

	const (
		volName     = "pvc-expand-92"
		initialSize = int64(1 * 1024 * 1024 * 1024) // 1 GiB
		newSize     = int64(2 * 1024 * 1024 * 1024) // 2 GiB
		volumePath  = "/mnt/staging/pvc-expand-92"
	)

	// ── Step 1: CreateVolume ───────────────────────────────────────────────────
	createResp, createErr := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: initialSize},
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":        env.TargetName,
			"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
			"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
			"pillar-csi.bhyoo.com/zfs-pool":      "tank",
		},
	})
	assertNoError(t, createErr, "CreateVolume")

	volumeID := createResp.GetVolume().GetVolumeId()
	if volumeID == "" {
		t.Fatal("CreateVolume: returned empty VolumeId")
	}

	// ── Step 2: ControllerExpandVolume ────────────────────────────────────────
	env.AgentMock.ExpandVolumeCapacityBytes = newSize

	ctrlExpandResp, ctrlExpandErr := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: newSize},
	})
	assertNoError(t, ctrlExpandErr, "ControllerExpandVolume")

	if ctrlExpandResp.GetCapacityBytes() != newSize {
		t.Errorf("ControllerExpandVolume CapacityBytes = %d, want %d",
			ctrlExpandResp.GetCapacityBytes(), newSize)
	}
	if !ctrlExpandResp.GetNodeExpansionRequired() {
		t.Error("ControllerExpandVolume: NodeExpansionRequired should be true")
	}

	// Verify agent.ExpandVolume was called.
	env.AgentMock.mu.Lock()
	expandCallCount := len(env.AgentMock.ExpandVolumeCalls)
	env.AgentMock.mu.Unlock()

	if expandCallCount != 1 {
		t.Errorf("expected 1 agent.ExpandVolume call, got %d", expandCallCount)
	}

	// ── Step 3: NodeExpandVolume ──────────────────────────────────────────────
	nodeExpandResp, nodeExpandErr := env.Node.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{
		VolumeId:   volumeID,
		VolumePath: volumePath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		CapacityRange: &csi.CapacityRange{RequiredBytes: newSize},
	})
	assertNoError(t, nodeExpandErr, "NodeExpandVolume")

	// NodeExpandVolume must have called ResizeFS exactly once.
	if len(env.Resizer.ResizeFSCalls) != 1 {
		t.Fatalf("expected 1 ResizeFS call, got %d", len(env.Resizer.ResizeFSCalls))
	}
	if nodeExpandResp.GetCapacityBytes() != newSize {
		t.Errorf("NodeExpandVolume CapacityBytes = %d, want %d",
			nodeExpandResp.GetCapacityBytes(), newSize)
	}
}

// TestCSIExpand_ControllerExpandVolume_Idempotent verifies that calling
// ControllerExpandVolume twice with the same size succeeds both times
// (idempotency).
//
// Test case 93 from E11.3.
func TestCSIExpand_ControllerExpandVolume_Idempotent(t *testing.T) {
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-6")
	ctx := context.Background()

	const reqSize int64 = 5 * 1024 * 1024 * 1024 // 5 GiB

	// Agent always echoes back the requested size (idempotent expand).
	env.AgentMock.ExpandVolumeCapacityBytes = reqSize

	volID := expandVolumeID(env.TargetName, "tank/pvc-expand-93")
	req := &csi.ControllerExpandVolumeRequest{
		VolumeId:      volID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: reqSize},
	}

	// First call.
	resp1, err1 := env.Controller.ControllerExpandVolume(ctx, req)
	assertNoError(t, err1, "ControllerExpandVolume (first call)")

	// Second call with the same parameters.
	resp2, err2 := env.Controller.ControllerExpandVolume(ctx, req)
	assertNoError(t, err2, "ControllerExpandVolume (second call, idempotent)")

	// Both calls must succeed and return the same capacity.
	if resp1.GetCapacityBytes() != reqSize {
		t.Errorf("first call CapacityBytes = %d, want %d", resp1.GetCapacityBytes(), reqSize)
	}
	if resp2.GetCapacityBytes() != reqSize {
		t.Errorf("second call CapacityBytes = %d, want %d", resp2.GetCapacityBytes(), reqSize)
	}

	// Both calls must have reached the agent.
	env.AgentMock.mu.Lock()
	expandCallCount := len(env.AgentMock.ExpandVolumeCalls)
	env.AgentMock.mu.Unlock()

	if expandCallCount != 2 {
		t.Errorf("expected 2 agent.ExpandVolume calls (idempotent), got %d", expandCallCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E11.4 — Error paths
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIExpand_ControllerExpandVolume_AgentFails verifies that when
// agent.ExpandVolume returns a gRPC error, ControllerExpandVolume propagates
// the error code to the caller.
//
// Test case 94 from E11.4.
func TestCSIExpand_ControllerExpandVolume_AgentFails(t *testing.T) {
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-7")
	ctx := context.Background()

	// Configure agent to return ResourceExhausted (pool capacity exceeded).
	env.AgentMock.ExpandVolumeErr = status.Error(codes.ResourceExhausted,
		"agent: ZFS pool capacity exceeded")

	volID := expandVolumeID(env.TargetName, "tank/pvc-expand-94")

	_, err := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      volID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 10 * 1024 * 1024 * 1024},
	})

	if err == nil {
		t.Fatal("expected error when agent.ExpandVolume fails, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted, got %s: %s", st.Code(), st.Message())
	}
}

// TestCSIExpand_NodeExpandVolume_ResizerFails verifies that when the Resizer
// returns an error, NodeExpandVolume returns a gRPC Internal error containing
// "resize" in the message.
//
// Test case 95 from E11.4.
func TestCSIExpand_NodeExpandVolume_ResizerFails(t *testing.T) {
	t.Parallel()
	env := newCSIExpandE2EEnv(t, "storage-expand-8")
	ctx := context.Background()

	// Configure the resizer to fail.
	env.Resizer.ResizeFSErr = errors.New("resize2fs: /dev/nvme0n1: no such file or directory")

	_, err := env.Node.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{
		VolumeId:   "pvc-expand-95",
		VolumePath: "/mnt/staging/pvc-expand-95",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 2 * 1024 * 1024 * 1024,
		},
	})

	if err == nil {
		t.Fatal("expected error when Resizer fails, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("expected Internal, got %s: %s", st.Code(), st.Message())
	}
	if !strings.Contains(strings.ToLower(st.Message()), "resize") {
		t.Errorf("expected 'resize' in error message, got %q", st.Message())
	}
}
