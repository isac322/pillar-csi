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

// Package e2e — CSI publish idempotency end-to-end tests.
//
// This file contains Sub-AC 7c: dedicated tests for the double-publish
// idempotency contract.  Each test calls a publish RPC twice with identical
// arguments and asserts:
//
//  1. Both calls succeed (no error).
//  2. Both calls return identical responses (no response drift).
//  3. The underlying side effects are not duplicated — the second call is a
//     no-op at the system level (no extra mounts, no double-initiator grants
//     beyond the expected per-call agent forwarding, etc.).
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIPublishIdempotency
package e2e

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs verifies that
// calling ControllerPublishVolume twice with identical arguments is safe:
//
//  1. Both calls return no error.
//  2. Both calls return the same PublishContext (response identity).
//  3. agent.AllowInitiator is called exactly once per CSI call (2 total) —
//     the CSI layer does not fan-out or retry silently.
//  4. No other agent side-effects occur (CreateVolume / ExportVolume are not
//     triggered by Publish).
func TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const (
		nodeID   = "nqn.2014-08.org.nvmexpress:uuid:idempotency-worker"
		volumeID = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-double-publish"
	)

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           nodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	}

	// ── First call ────────────────────────────────────────────────────────────
	resp1, err := env.Controller.ControllerPublishVolume(ctx, req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume (call 1): unexpected error: %v", err)
	}
	if resp1 == nil {
		t.Fatal("ControllerPublishVolume (call 1): got nil response")
	}

	// ── Second call (identical arguments) ────────────────────────────────────
	resp2, err := env.Controller.ControllerPublishVolume(ctx, req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume (call 2): unexpected error: %v", err)
	}
	if resp2 == nil {
		t.Fatal("ControllerPublishVolume (call 2): got nil response")
	}

	// ── Assert: responses are identical (PublishContext must not drift) ───────
	if !reflect.DeepEqual(resp1.GetPublishContext(), resp2.GetPublishContext()) {
		t.Errorf("PublishContext mismatch between call 1 and call 2:\n  call 1: %v\n  call 2: %v",
			resp1.GetPublishContext(), resp2.GetPublishContext())
	}

	// ── Assert: AllowInitiator called exactly 2 times (1 per CSI call) ───────
	// The CSI ControllerPublishVolume implementation forwards each call to the
	// agent; idempotency at the storage level is the agent's responsibility.
	// Exactly 2 AllowInitiator calls (no more, no fewer) is the expected and
	// correct behavior — the CSI layer must not suppress, deduplicate, or
	// silently retry the agent call.
	env.AgentMock.mu.Lock()
	allowCalls := env.AgentMock.AllowInitiatorCalls
	createCalls := env.AgentMock.CreateVolumeCalls
	exportCalls := env.AgentMock.ExportVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(allowCalls) != 2 {
		t.Errorf("AllowInitiator: want exactly 2 calls, got %d", len(allowCalls))
	}

	// ── Assert: both AllowInitiator calls carry the same arguments ────────────
	if len(allowCalls) == 2 {
		c1, c2 := allowCalls[0], allowCalls[1]
		if c1.VolumeID != c2.VolumeID {
			t.Errorf("AllowInitiator VolumeID: call 1 = %q, call 2 = %q", c1.VolumeID, c2.VolumeID)
		}
		if c1.InitiatorID != c2.InitiatorID {
			t.Errorf("AllowInitiator InitiatorID: call 1 = %q, call 2 = %q", c1.InitiatorID, c2.InitiatorID)
		}
		if c1.ProtocolType != c2.ProtocolType {
			t.Errorf("AllowInitiator ProtocolType: call 1 = %v, call 2 = %v", c1.ProtocolType, c2.ProtocolType)
		}
		// The node ID must flow through as the initiator ID.
		if c1.InitiatorID != nodeID {
			t.Errorf("AllowInitiator InitiatorID = %q, want %q", c1.InitiatorID, nodeID)
		}
	}

	// ── Assert: no unexpected CreateVolume or ExportVolume side-effects ───────
	// ControllerPublishVolume must NOT trigger volume creation or export — those
	// belong to CreateVolume.
	if len(createCalls) != 0 {
		t.Errorf("unexpected agent CreateVolume calls during Publish: got %d, want 0", len(createCalls))
	}
	if len(exportCalls) != 0 {
		t.Errorf("unexpected agent ExportVolume calls during Publish: got %d, want 0", len(exportCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes verifies that
// calling ControllerPublishVolume for the same volume but two different nodes
// produces two independent AllowInitiator calls — they must not be collapsed
// into one.
func TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const (
		nodeID1  = "nqn.2014-08.org.nvmexpress:uuid:worker-node-a"
		nodeID2  = "nqn.2014-08.org.nvmexpress:uuid:worker-node-b"
		volumeID = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-multi-node"
	)
	volCap := defaultVolumeCapabilities()[0]

	_, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           nodeID1,
		VolumeCapability: volCap,
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume node 1: %v", err)
	}

	_, err = env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           nodeID2,
		VolumeCapability: volCap,
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume node 2: %v", err)
	}

	env.AgentMock.mu.Lock()
	allowCalls := env.AgentMock.AllowInitiatorCalls
	env.AgentMock.mu.Unlock()

	if len(allowCalls) != 2 {
		t.Fatalf("want 2 AllowInitiator calls (one per node), got %d", len(allowCalls))
	}

	// Each call must carry a distinct initiator ID.
	initiators := map[string]struct{}{
		allowCalls[0].InitiatorID: {},
		allowCalls[1].InitiatorID: {},
	}
	if len(initiators) != 2 {
		t.Errorf("expected 2 distinct initiator IDs, got: %v", initiators)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget verifies the full
// idempotency contract for NodePublishVolume when called twice with identical
// arguments:
//
//  1. Both calls succeed.
//  2. Both calls return non-nil responses.
//  3. Exactly one bind-mount operation is performed (the second call is a
//     no-op — the target path is already mounted).
//  4. The in-memory mount table shows the target path mounted exactly once
//     (no double-mount artifacts).
//  5. The staging path and target path are each mounted at most once.
//  6. No additional Connect calls are issued during NodePublishVolume (Connect
//     belongs to NodeStageVolume and must not be repeated at publish time).
func TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-idempotency")
	ctx := context.Background()

	env.Connector.DevicePath = "/dev/nvme0n1"

	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	const volumeID = "pool/vol-publish-idempotency"

	// ── Prerequisites: stage the volume ──────────────────────────────────────
	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume prerequisite: %v", err)
	}

	// Record connector baseline after staging (Connect should have happened once).
	connectCallsAfterStage := len(env.Connector.ConnectCalls)
	if connectCallsAfterStage != 1 {
		t.Errorf("expected 1 Connect call after NodeStageVolume, got %d", connectCallsAfterStage)
	}

	pubReq := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}

	// ── First NodePublishVolume ───────────────────────────────────────────────
	resp1, err := env.Node.NodePublishVolume(ctx, pubReq)
	if err != nil {
		t.Fatalf("NodePublishVolume (call 1): %v", err)
	}
	if resp1 == nil {
		t.Fatal("NodePublishVolume (call 1): got nil response")
	}

	mountCallsAfterFirst := len(env.Mounter.MountCalls)

	// Verify the target path is mounted after the first call.
	mounted1, err := env.Mounter.IsMounted(targetPath)
	if err != nil {
		t.Fatalf("IsMounted after first publish: %v", err)
	}
	if !mounted1 {
		t.Fatal("target path not mounted after first NodePublishVolume")
	}

	// ── Second NodePublishVolume (same arguments) ─────────────────────────────
	resp2, err := env.Node.NodePublishVolume(ctx, pubReq)
	if err != nil {
		t.Fatalf("NodePublishVolume (call 2): %v", err)
	}
	if resp2 == nil {
		t.Fatal("NodePublishVolume (call 2): got nil response")
	}

	// ── Assert: no duplicate mount side-effect ────────────────────────────────
	// The second NodePublishVolume must not issue another bind-mount because
	// the target path is already mounted.  The mount call count must be
	// unchanged from after the first call.
	if len(env.Mounter.MountCalls) != mountCallsAfterFirst {
		t.Errorf(
			"Mount calls after second NodePublishVolume: want %d (same as after first), got %d — "+
				"second call must not re-mount an already-mounted target path",
			mountCallsAfterFirst, len(env.Mounter.MountCalls),
		)
	}

	// ── Assert: target is still mounted (no accidental unmount) ──────────────
	mounted2, err := env.Mounter.IsMounted(targetPath)
	if err != nil {
		t.Fatalf("IsMounted after second publish: %v", err)
	}
	if !mounted2 {
		t.Fatal("target path not mounted after second NodePublishVolume — idempotent call must not unmount")
	}

	// ── Assert: no extra Connect calls during NodePublishVolume ───────────────
	// Connect is a NodeStageVolume side-effect.  Neither Publish call should
	// trigger it.
	if len(env.Connector.ConnectCalls) != connectCallsAfterStage {
		t.Errorf(
			"Connect calls: want %d (no change during Publish), got %d",
			connectCallsAfterStage, len(env.Connector.ConnectCalls),
		)
	}

	// ── Assert: no FormatAndMount during Publish ──────────────────────────────
	// FormatAndMount is a NodeStageVolume operation.  Neither Publish call
	// should reformat the device.
	formatCallsExpected := 1 // from NodeStageVolume
	if len(env.Mounter.FormatAndMountCalls) != formatCallsExpected {
		t.Errorf(
			"FormatAndMount calls: want %d (only from Stage), got %d",
			formatCallsExpected, len(env.Mounter.FormatAndMountCalls),
		)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIPublishIdempotency_NodePublishVolume_DoubleBlockAccess
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIPublishIdempotency_NodePublishVolume_DoubleBlockAccess verifies that
// the idempotency contract holds for BLOCK-access volumes.  Calling
// NodePublishVolume twice with a raw-block VolumeCapability must not
// issue two bind-mounts.
func TestCSIPublishIdempotency_NodePublishVolume_DoubleBlockAccess(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-block-idempotency")
	ctx := context.Background()

	env.Connector.DevicePath = "/dev/nvme1n1"

	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	const volumeID = "pool/block-vol-idempotency"

	// ── Stage (MOUNT access on staging) ──────────────────────────────────────
	// Note: NodeStageVolume uses MOUNT access type to format the device.
	// NodePublishVolume will use BLOCK access type for the bind-mount.
	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume prerequisite (block test): %v", err)
	}

	pubReq := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  blockVolumeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}

	// ── First publish ─────────────────────────────────────────────────────────
	_, err = env.Node.NodePublishVolume(ctx, pubReq)
	if err != nil {
		t.Fatalf("NodePublishVolume BLOCK (call 1): %v", err)
	}

	mountCallsAfterFirst := len(env.Mounter.MountCalls)

	// ── Second publish ────────────────────────────────────────────────────────
	_, err = env.Node.NodePublishVolume(ctx, pubReq)
	if err != nil {
		t.Fatalf("NodePublishVolume BLOCK (call 2): %v", err)
	}

	// No extra mount on the second call.
	if len(env.Mounter.MountCalls) != mountCallsAfterFirst {
		t.Errorf(
			"BLOCK: Mount calls after second publish: want %d (idempotent), got %d",
			mountCallsAfterFirst, len(env.Mounter.MountCalls),
		)
	}

	// Target still mounted.
	mounted, err := env.Mounter.IsMounted(targetPath)
	if err != nil {
		t.Fatalf("IsMounted after double block publish: %v", err)
	}
	if !mounted {
		t.Fatal("BLOCK: target path not mounted after double NodePublishVolume")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble verifies that
// calling NodePublishVolume(Readonly=true) twice succeeds without re-mounting.
// The "ro" flag must appear in the first (and only) mount call, and the second
// call must be a no-op.
func TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-ro-idempotency")
	ctx := context.Background()

	env.Connector.DevicePath = "/dev/nvme2n1"

	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	const volumeID = "pool/ro-vol-idempotency"

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume prerequisite (readonly test): %v", err)
	}

	pubReq := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY),
		Readonly:          true,
	}

	// ── First call ────────────────────────────────────────────────────────────
	_, err = env.Node.NodePublishVolume(ctx, pubReq)
	if err != nil {
		t.Fatalf("NodePublishVolume readonly (call 1): %v", err)
	}

	// Verify the first mount included "ro".
	if len(env.Mounter.MountCalls) == 0 {
		t.Fatal("expected at least one Mount call after first readonly publish")
	}
	hasRO := slices.Contains(env.Mounter.MountCalls[0].Options, "ro")
	if !hasRO {
		t.Errorf("first readonly publish: mount options %v do not contain 'ro'",
			env.Mounter.MountCalls[0].Options)
	}

	mountCallsAfterFirst := len(env.Mounter.MountCalls)

	// ── Second call (same Readonly=true args) ─────────────────────────────────
	_, err = env.Node.NodePublishVolume(ctx, pubReq)
	if err != nil {
		t.Fatalf("NodePublishVolume readonly (call 2): %v", err)
	}

	// No additional mount.
	if len(env.Mounter.MountCalls) != mountCallsAfterFirst {
		t.Errorf(
			"readonly: Mount calls after second publish: want %d (no-op), got %d",
			mountCallsAfterFirst, len(env.Mounter.MountCalls),
		)
	}

	// Target still mounted.
	mounted, err := env.Mounter.IsMounted(targetPath)
	if err != nil {
		t.Fatalf("IsMounted after double readonly publish: %v", err)
	}
	if !mounted {
		t.Fatal("readonly: target path not mounted after double publish")
	}
}
