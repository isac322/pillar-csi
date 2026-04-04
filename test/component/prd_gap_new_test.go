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

// Package component_test — PRD gap new test cases.
//
// This file implements the C-NEW-* test cases that cover PRD gaps identified
// after the initial component test suite was written.
package component_test

import (
	"context"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-1: acl=false → ControllerPublish/Unpublish no-op
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_ControllerPublishVolume_ACLDisabled_NoOp verifies that
// when the VolumeContext carries acl-enabled=false, ControllerPublishVolume
// returns success without calling agent.AllowInitiator.
//
// When ACL is disabled (PillarProtocol.spec.nvmeofTcp.acl=false), the agent
// configured attr_allow_any_host=1 during ExportVolume.  There is no need to
// grant per-initiator access — any connected host is permitted automatically.
func TestCSIController_ControllerPublishVolume_ACLDisabled_NoOp(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	// Seed a CSINode annotation so resolveInitiatorID succeeds (required before
	// the ACL check).  The annotation contains the host NQN for nvmeof-tcp.
	const (
		nodeID  = "worker-node-1"
		hostNQN = "nqn.2014-08.org.nvmexpress:uuid:acl-test-node"
	)
	seedCSINodeForNVMeOF(ctx, t, env.k8sClient, nodeID, hostNQN)

	// Ensure AllowInitiator is NOT called by failing the test if it is.
	env.agent.allowInitiatorFn = func(
		_ context.Context, _ *agentv1.AllowInitiatorRequest,
	) (*agentv1.AllowInitiatorResponse, error) {
		t.Error("AllowInitiator must not be called when ACL is disabled")
		return &agentv1.AllowInitiatorResponse{}, nil
	}

	_, err := env.srv.ControllerPublishVolume(ctx, &csipb.ControllerPublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   nodeID,
		VolumeCapability: &csipb.VolumeCapability{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		// VolumeContext carries the ACL flag set by CreateVolume.
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetID: "nqn.2026-01.com.pillar-csi:test",
			pillarcsi.VolumeContextKeyAddress:  "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:     "4420",
			"pillar-csi.bhyoo.com/acl-enabled": "false",
		},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume: unexpected error: %v", err)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("agent.AllowInitiator calls = %d, want 0 (ACL disabled)", env.agent.allowInitiatorCalls)
	}
}

// TestCSIController_ControllerUnpublishVolume_ACLDisabled_NoOp verifies that
// when ACL is disabled, ControllerUnpublishVolume returns success without
// calling agent.DenyInitiator.
//
// With ACL disabled (allow_any_host mode), no per-initiator host entries were
// created during ControllerPublishVolume, so there is nothing to revoke.
// The absence of a CSINode annotation reflects this state: the host NQN was
// never registered because ACL was not enforced.
func TestCSIController_ControllerUnpublishVolume_ACLDisabled_NoOp(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	// Do NOT seed a CSINode annotation — with ACL disabled, the node plugin
	// does not register a host NQN, so no annotation exists.  The controller
	// treats a missing annotation as "already revoked" and returns success
	// without calling DenyInitiator.
	env.agent.denyInitiatorFn = func(
		_ context.Context, _ *agentv1.DenyInitiatorRequest,
	) (*agentv1.DenyInitiatorResponse, error) {
		t.Error("DenyInitiator must not be called when no ACL entry exists")
		return &agentv1.DenyInitiatorResponse{}, nil
	}

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   "worker-node-acl-off",
	})
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume: unexpected error: %v", err)
	}
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("agent.DenyInitiator calls = %d, want 0 (no ACL entry to revoke)", env.agent.denyInitiatorCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-2: acl=false → ExportVolume allow_any_host=1
// ─────────────────────────────────────────────────────────────────────────────

// TestAgent_ExportVolume_ACLDisabled_AllowAnyHost verifies that when CreateVolume
// is called with the acl-enabled=false parameter, the agent receives an
// ExportVolumeRequest with AclEnabled=false.
//
// This test exercises the controller layer: it verifies that the
// pillar-csi.bhyoo.com/acl-enabled=false StorageClass parameter is correctly
// translated to AclEnabled=false in the ExportVolumeRequest sent to the agent.
// The actual configfs attr_allow_any_host write is verified in integration tests.
func TestAgent_ExportVolume_ACLDisabled_AllowAnyHost(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	// Capture the ExportVolumeRequest sent to the agent.
	var capturedACLEnabled *bool
	env.agent.exportVolumeFn = func(
		_ context.Context, req *agentv1.ExportVolumeRequest,
	) (*agentv1.ExportVolumeResponse, error) {
		v := req.GetAclEnabled()
		capturedACLEnabled = &v
		return &agentv1.ExportVolumeResponse{
			ExportInfo: &agentv1.ExportInfo{
				TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:storage-node-1.tank.pvc-acl-test",
				Address:   "192.168.1.10",
				Port:      4420,
				VolumeRef: "tank/pvc-acl-test",
			},
		}, nil
	}

	req := baseCSICreateVolumeRequest()
	req.Name = "pvc-acl-test"
	// Set acl-enabled=false to disable per-initiator ACL enforcement.
	req.Parameters["pillar-csi.bhyoo.com/acl-enabled"] = "false"

	_, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume: unexpected error: %v", err)
	}

	if capturedACLEnabled == nil {
		t.Fatal("ExportVolume was not called")
	}
	if *capturedACLEnabled {
		t.Errorf("ExportVolumeRequest.AclEnabled = true, want false when acl-enabled=false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-9: NodeGetVolumeStats mock 기반
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeGetVolumeStats_Filesystem verifies that NodeGetVolumeStats
// returns both BYTES and INODES usage entries for a real filesystem path.
// Uses t.TempDir() so no root privileges are required.
func TestCSINode_NodeGetVolumeStats_Filesystem(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	// Use a real temp directory on an actual filesystem.
	mountDir := t.TempDir()

	resp, err := env.node.NodeGetVolumeStats(ctx, &csipb.NodeGetVolumeStatsRequest{
		VolumeId:   "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-stats-test",
		VolumePath: mountDir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: unexpected error: %v", err)
	}
	if len(resp.Usage) != 2 {
		t.Fatalf("len(usage) = %d, want 2 (BYTES and INODES)", len(resp.Usage))
	}

	var bytesEntry, inodesEntry *csipb.VolumeUsage
	for _, u := range resp.Usage {
		switch u.Unit {
		case csipb.VolumeUsage_BYTES:
			bytesEntry = u
		case csipb.VolumeUsage_INODES:
			inodesEntry = u
		}
	}
	if bytesEntry == nil {
		t.Error("missing BYTES usage entry")
	}
	if inodesEntry == nil {
		t.Error("missing INODES usage entry")
	}
	if bytesEntry != nil && bytesEntry.Total <= 0 {
		t.Errorf("BYTES total = %d, want > 0", bytesEntry.Total)
	}
	if inodesEntry != nil && inodesEntry.Total <= 0 {
		t.Errorf("INODES total = %d, want > 0", inodesEntry.Total)
	}
}

// TestCSINode_NodeGetVolumeStats_VolumeNotFound verifies that NodeGetVolumeStats
// returns NotFound when the volume_path does not exist.
func TestCSINode_NodeGetVolumeStats_VolumeNotFound(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeGetVolumeStats(ctx, &csipb.NodeGetVolumeStatsRequest{
		VolumeId:   "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-missing",
		VolumePath: "/this/path/does/not/exist/at/all",
	})
	if err == nil {
		t.Fatal("NodeGetVolumeStats: expected NotFound error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-10: Agent Snapshot RPC 에러
// ─────────────────────────────────────────────────────────────────────────────

// TestAgent_CreateSnapshot_Unimplemented verifies that the CSI controller
// returns Unimplemented for CreateSnapshot because pillar-csi does not
// implement snapshot functionality.
func TestAgent_CreateSnapshot_Unimplemented(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{
		SourceVolumeId: expectedCSIVolumeID,
		Name:           "snap-test",
	})
	if err == nil {
		t.Fatal("CreateSnapshot: expected Unimplemented error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// TestAgent_DeleteSnapshot_Unimplemented verifies that the CSI controller
// returns Unimplemented for DeleteSnapshot because pillar-csi does not
// implement snapshot functionality.
func TestAgent_DeleteSnapshot_Unimplemented(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{
		SnapshotId: "snap-not-implemented",
	})
	if err == nil {
		t.Fatal("DeleteSnapshot: expected Unimplemented error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-11: NodeGetInfo GetInitiatorID 형식 검증
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeGetInfo_NQNFormat verifies that when a NodeServer is created
// with an NQN-format node ID, NodeGetInfo returns a NodeId that starts with
// the "nqn." prefix.
//
// This test validates that the CSI node plugin correctly propagates the
// node identity (which may be the host NQN derived from /etc/nvme/hostnqn)
// when the Kubernetes node name is not used as the node ID.
func TestCSINode_NodeGetInfo_NQNFormat(t *testing.T) {
	t.Parallel()

	// Create a NodeServer whose nodeID is an NQN-format string, simulating
	// the case where the node is identified by its NVMe host NQN.
	const hostNQN = "nqn.2014-08.org.nvmexpress:uuid:test-1234"
	stateDir := t.TempDir()
	node := pillarcsi.NewNodeServerWithStateDir(hostNQN, &csiMockConnector{}, newCsiMockMounter(), stateDir)

	resp, err := node.NodeGetInfo(context.Background(), &csipb.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: unexpected error: %v", err)
	}

	nodeID := resp.GetNodeId()
	if !strings.HasPrefix(nodeID, "nqn.") {
		t.Errorf("NodeId = %q, want string starting with \"nqn.\"", nodeID)
	}
	if nodeID != hostNQN {
		t.Errorf("NodeId = %q, want %q", nodeID, hostNQN)
	}
}
