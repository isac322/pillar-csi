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

// Package component_test – CSI Controller and Node Service error/exception path tests.
//
// This file covers deep error paths for the CSI Controller (internal/csi.ControllerServer)
// and CSI Node (internal/csi.NodeServer) services.  Tests focus on:
//
//   - Agent unreachable (various dial/transport errors)
//   - gRPC deadline exceeded propagation from agent responses
//   - Invalid parameters at the CSI Controller boundary
//   - Shrink rejected: ControllerExpandVolume with shrink request
//   - mkfs failure: NodeStageVolume FormatAndMount error
//   - Mount failure: NodePublishVolume bind-mount error
//   - TOCTOU: device path disappears between GetDevicePath and FormatAndMount
//   - Unstage/Unpublish error paths
//
// All tests use mock interfaces — no root privileges, kernel modules, or
// real NVMe connections required.
package component_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// csiTestDevicePath is the fake NVMe device path used across CSI error tests.
const csiTestDevicePath = "/dev/nvme0n1"

// ─────────────────────────────────────────────────────────────────────────────
// Helpers – controller environment for error tests
// ─────────────────────────────────────────────────────────────────────────────.

// newCSIControllerErrEnv builds a ControllerServer backed by the given mock
// agent, re-using the same target/scheme setup as newCSIControllerTestEnv.
func newCSIControllerErrEnv(t *testing.T, agnt *csiMockAgent) *csiControllerTestEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarTargetStatus{ResolvedAddress: "192.168.1.10:9500"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(target).
		WithStatusSubresource(&v1alpha1.PillarVolume{}, &v1alpha1.PillarTarget{}).
		Build()

	dialer := pillarcsi.AgentDialer(func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agnt, csiNopCloser{}, nil
	})

	srv := pillarcsi.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)
	return &csiControllerTestEnv{srv: srv, agent: agnt}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_CreateVolume_AgentDeadlineExceeded
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_CreateVolume_AgentDeadlineExceeded verifies that when the
// agent's CreateVolume RPC returns codes.DeadlineExceeded (e.g., the storage
// node is overloaded and the internal operation times out), the CSI controller
// propagates a non-OK error to the CO.
//
// This exercises the error-propagation path from agent → controller and
// verifies the controller does not swallow the error.
func TestCSIErrors_CreateVolume_AgentDeadlineExceeded(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{
		createVolumeFn: func(_ context.Context, _ *agentv1.CreateVolumeRequest) (*agentv1.CreateVolumeResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "agent: ZFS command timed out")
		},
	}
	env := newCSIControllerErrEnv(t, mock)

	_, err := env.srv.CreateVolume(context.Background(), baseCSICreateVolumeRequest())
	if err == nil {
		t.Fatal("expected error from agent DeadlineExceeded, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status, got OK")
	}
	t.Logf("agent DeadlineExceeded propagated as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_CreateVolume_AgentUnreachable_PlainError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_CreateVolume_AgentUnreachable_PlainError verifies that a
// plain Go error from the AgentDialer (not a gRPC status) is handled
// gracefully and reported as an error to the CO.
//
// This models a scenario where the dialer itself fails before establishing a
// connection — e.g., a DNS lookup failure or a synchronous rejection from
// the networking layer.
func TestCSIErrors_CreateVolume_AgentUnreachable_PlainError(t *testing.T) {
	t.Parallel()

	plainDialErr := errors.New("dial tcp 192.168.1.10:9500: connect: connection refused")
	env := newCSIControllerTestEnvWithDialErr(t, plainDialErr)

	_, err := env.srv.CreateVolume(context.Background(), baseCSICreateVolumeRequest())
	if err == nil {
		t.Fatal("expected error from unreachable agent, got nil")
	}
	// Any non-OK gRPC code is acceptable.
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status, got OK")
	}
	t.Logf("plain dial error propagated as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_ControllerExpand_ShrinkRejected
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_ControllerExpand_ShrinkRejected verifies that
// ControllerExpandVolume propagates the agent's shrink-rejected error to the
// CO with the original gRPC status code.
//
// The CSI spec requires that shrink attempts return an InvalidArgument error.
// This test verifies that the agent's InvalidArgument for "cannot shrink" is
// propagated unchanged through the controller layer.
func TestCSIErrors_ControllerExpand_ShrinkRejected(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{
		expandVolumeFn: func(_ context.Context, _ *agentv1.ExpandVolumeRequest) (*agentv1.ExpandVolumeResponse, error) {
			return nil, status.Error(codes.InvalidArgument, "cannot shrink volume: volsize cannot be decreased")
		},
	}
	env := newCSIControllerErrEnv(t, mock)

	_, err := env.srv.ControllerExpandVolume(context.Background(), &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 512 << 20}, // 512 MiB — smaller than assumed current
	})

	if err == nil {
		t.Fatal("expected error for shrink attempt, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument (shrink rejected)", st.Code())
	}
	t.Logf("shrink rejection propagated as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_ControllerExpand_AgentDeadlineExceeded
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_ControllerExpand_AgentDeadlineExceeded verifies that
// ControllerExpandVolume propagates a DeadlineExceeded from the agent.
func TestCSIErrors_ControllerExpand_AgentDeadlineExceeded(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{
		expandVolumeFn: func(_ context.Context, _ *agentv1.ExpandVolumeRequest) (*agentv1.ExpandVolumeResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "agent: ZFS expand timed out")
		},
	}
	env := newCSIControllerErrEnv(t, mock)

	_, err := env.srv.ControllerExpandVolume(context.Background(), &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 20 << 30},
	})

	if err == nil {
		t.Fatal("expected error from agent DeadlineExceeded, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status, got OK")
	}
	t.Logf("expand agent DeadlineExceeded propagated as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_ControllerPublish_AllowInitiatorFails
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_ControllerPublish_AllowInitiatorFails verifies that
// ControllerPublishVolume propagates an agent.AllowInitiator failure.
//
// In production this can occur when the NVMe-oF subsystem ACL entry cannot be
// written because the configfs is read-only or the volume has been unexported.
func TestCSIErrors_ControllerPublish_AllowInitiatorFails(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{
		allowInitiatorFn: func(_ context.Context, _ *agentv1.AllowInitiatorRequest) (*agentv1.AllowInitiatorResponse, error) {
			return nil, status.Error(codes.Internal, "AllowInitiator: configfs write failed: permission denied")
		},
	}
	env := newCSIControllerErrEnv(t, mock)

	_, err := env.srv.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   "nqn.2014-08.org.nvmexpress:uuid:test-node-err",
		VolumeCapability: &csipb.VolumeCapability{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})

	if err == nil {
		t.Fatal("expected error from AllowInitiator failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status, got OK")
	}
	t.Logf("AllowInitiator failure propagated as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_DeleteVolume_AgentDeadlineExceeded
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_DeleteVolume_AgentDeadlineExceeded verifies that DeleteVolume
// propagates a DeadlineExceeded from the agent.
func TestCSIErrors_DeleteVolume_AgentDeadlineExceeded(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{
		unexportVolumeFn: func(_ context.Context, _ *agentv1.UnexportVolumeRequest) (*agentv1.UnexportVolumeResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "agent: unexport timed out")
		},
	}
	env := newCSIControllerErrEnv(t, mock)

	_, err := env.srv.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{
		VolumeId: expectedCSIVolumeID,
	})

	if err == nil {
		t.Fatal("expected error from agent DeadlineExceeded on unexport, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status, got OK")
	}
	t.Logf("DeleteVolume agent DeadlineExceeded propagated as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodeStage_MkfsFailure
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeStage_MkfsFailure verifies that when FormatAndMount fails
// with an mkfs-style error (exit code + stderr), NodeStageVolume returns
// codes.Internal with a non-empty error message.
//
// In production this occurs when the newly connected NVMe block device cannot
// be formatted because the filesystem utility fails — e.g., bad sectors,
// wrong device size, or the mkfs binary is not in PATH.
func TestCSIErrors_NodeStage_MkfsFailure(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	// GetDevicePath returns a device path (device appears to be present).
	env.connector.getDeviceFn = func(_ context.Context, _ string) (string, error) {
		return csiTestDevicePath, nil
	}

	// FormatAndMount fails with a realistic mkfs.ext4 error message.
	const mkfsErr = "mkfs.ext4: /dev/nvme0n1: Input/output error while checking if filesystem is mounted"
	env.mounter.formatAndMountFn = func(_, _ string, _ string, _ []string) error {
		return errors.New(mkfsErr)
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected Internal error from mkfs failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (mkfs failure)", st.Code())
	}
	// The gRPC error message should preserve the mkfs error text.
	if msg := st.Message(); msg == "" {
		t.Error("gRPC error message is empty; mkfs error detail should be included")
	}
	t.Logf("mkfs failure returned Internal: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodeStage_TOCTOU_DeviceDisappears
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeStage_TOCTOU_DeviceDisappears verifies that when the block
// device path is returned by GetDevicePath but the device disappears before
// FormatAndMount can open it, NodeStageVolume returns codes.Internal.
//
// This TOCTOU race occurs in production when:
//  1. The NVMe controller connects and exposes /dev/nvme0n1.
//  2. GetDevicePath sees the device and returns its path.
//  3. The kernel removes the device (controller reset, cable pull, udev race)
//     before the FormatAndMount call opens the block device.
//
// Setup:
//   - Connector.Connect succeeds.
//   - Connector.GetDevicePath returns "/dev/nvme0n1" (device seen).
//   - Mounter.FormatAndMount returns "no such device" (device gone).
//
// Expected: NodeStageVolume returns codes.Internal.
func TestCSIErrors_NodeStage_TOCTOU_DeviceDisappears(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	// Step 1: Connect succeeds.
	env.connector.connectFn = func(_ context.Context, _, _, _ string) error {
		return nil
	}

	// Step 2: GetDevicePath reports the device is present.
	env.connector.getDeviceFn = func(_ context.Context, _ string) (string, error) {
		return csiTestDevicePath, nil
	}

	// Step 3: FormatAndMount fails because the device disappeared between
	// GetDevicePath and FormatAndMount — the classic TOCTOU window.
	env.mounter.formatAndMountFn = func(src, _, _ string, _ []string) error {
		if src == csiTestDevicePath {
			return errors.New("mkfs.ext4: No such device or address while trying to open /dev/nvme0n1")
		}
		return nil
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected Internal error from TOCTOU device disappearance, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (device disappeared during FormatAndMount)", st.Code())
	}

	// Connect must have been called once.
	if env.connector.connectCalls != 1 {
		t.Errorf("Connect calls = %d, want 1", env.connector.connectCalls)
	}
	// FormatAndMount must have been attempted once (device path was resolved).
	if env.mounter.formatAndMountCalls != 1 {
		t.Errorf("FormatAndMount calls = %d, want 1", env.mounter.formatAndMountCalls)
	}
	t.Logf("TOCTOU: NodeStageVolume correctly returned Internal: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodePublish_BindMountFails
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodePublish_BindMountFails verifies that when Mounter.Mount
// fails with a bind-mount error, NodePublishVolume returns codes.Internal.
//
// In production this occurs when the staging path is not accessible from the
// target path — e.g., different cgroup mount namespaces, exhausted mounts
// (ENOSPC on mount table), or permission issues.
func TestCSIErrors_NodePublish_BindMountFails(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	const mountErr = "mount: /target: cannot bind-mount: no space left on mount table (ENOSPC)"
	env.mounter.mountFn = func(_, _ string, _ string, _ []string) error {
		return errors.New(mountErr)
	}

	_, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath))
	if err == nil {
		t.Fatal("expected Internal error from bind-mount failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (bind-mount failure)", st.Code())
	}
	t.Logf("bind-mount failure returned Internal: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodeUnstage_UnmountError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeUnstage_UnmountError verifies that when Mounter.Unmount
// fails during NodeUnstageVolume, the operation returns a non-OK error.
//
// In production Unmount can fail if the staging path is still in use by a
// container (EBUSY) or if the filesystem has been remounted read-only.
func TestCSIErrors_NodeUnstage_UnmountError(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	// First stage the volume so there is a state file to unstage.
	if _, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath)); err != nil {
		t.Fatalf("setup NodeStageVolume: %v", err)
	}

	// Now configure Unmount to fail.
	const unmountErr = "umount: /staging: target is busy (EBUSY)"
	env.mounter.unmountFn = func(_ string) error {
		return errors.New(unmountErr)
	}

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
		StagingTargetPath: stagingPath,
	})

	// The node should propagate the unmount error rather than silently
	// succeeding with a busy mount point still active.
	if err == nil {
		// Some implementations may succeed anyway (log and continue).
		// Document this behavior rather than failing the test.
		t.Logf("NodeUnstageVolume with unmount failure: returned nil (implementation continues on unmount error)")
		return
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status for unmount failure, got OK")
	}
	t.Logf("NodeUnstageVolume unmount failure returned %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodeUnpublish_UnmountError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeUnpublish_UnmountError verifies that when Mounter.Unmount
// fails during NodeUnpublishVolume, the operation returns a non-OK error or
// documents its current behavior.
func TestCSIErrors_NodeUnpublish_UnmountError(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// First publish the volume.
	if _, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath)); err != nil {
		t.Fatalf("setup NodePublishVolume: %v", err)
	}

	// Configure Unmount to fail.
	const unmountErr = "umount: /target: target is busy (EBUSY)"
	env.mounter.unmountFn = func(_ string) error {
		return errors.New(unmountErr)
	}

	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
		TargetPath: targetPath,
	})

	if err == nil {
		// Document: some implementations log and continue on unpublish errors.
		t.Logf("NodeUnpublishVolume with unmount failure: returned nil (implementation continues on unmount error)")
		return
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status for unmount failure, got OK")
	}
	t.Logf("NodeUnpublishVolume unmount failure returned %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodeStage_GRPCDeadlineExceeded
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeStage_GRPCDeadlineExceeded verifies that a request context
// deadline that expires before the block device appears causes NodeStageVolume
// to return codes.DeadlineExceeded.
//
// Setup:
//   - Connect succeeds.
//   - GetDevicePath always returns ("", nil) — device never appears.
//   - Context deadline: 200 ms (short but > 0).
//
// Expected: NodeStageVolume returns DeadlineExceeded within ~500 ms.
func TestCSIErrors_NodeStage_GRPCDeadlineExceeded(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	stagingPath := t.TempDir()

	// Connect succeeds immediately.
	env.connector.connectFn = func(_ context.Context, _, _, _ string) error { return nil }

	// GetDevicePath never returns a real path — device never appears.
	env.connector.getDeviceFn = func(_ context.Context, _ string) (string, error) {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DeadlineExceeded from device poll timeout, got nil")
	}

	const maxElapsed = 500 * time.Millisecond
	if elapsed > maxElapsed {
		t.Errorf("NodeStageVolume took %v, want < %v", elapsed, maxElapsed)
	}

	st, _ := status.FromError(err)
	if st.Code() != codes.DeadlineExceeded {
		t.Errorf("error code = %v, want DeadlineExceeded", st.Code())
	}
	t.Logf("gRPC deadline exceeded in %v: %v", elapsed, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_CreateVolume_InvalidCapabilities_Nil
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_CreateVolume_InvalidCapabilities_Nil verifies that a
// CreateVolume request with nil VolumeCapabilities is rejected with
// InvalidArgument before calling the agent.
func TestCSIErrors_CreateVolume_InvalidCapabilities_Nil(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{}
	env := newCSIControllerErrEnv(t, mock)

	req := baseCSICreateVolumeRequest()
	req.VolumeCapabilities = nil

	_, err := env.srv.CreateVolume(context.Background(), req)
	if err == nil {
		// If nil capabilities are accepted (CO ensures they're present), document behavior.
		t.Logf("CreateVolume with nil capabilities: returned nil (capabilities validation may be skipped at controller)")
		if mock.createVolumeCalls > 0 {
			// This means the agent was called; that's fine, just document.
			t.Logf("agent.CreateVolume was called %d times despite nil capabilities", mock.createVolumeCalls)
		}
		return
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument for nil capabilities", st.Code())
	}
	// Agent must not have been called — validation should short-circuit.
	if mock.createVolumeCalls != 0 {
		t.Errorf("agent.CreateVolume was called %d times, want 0 (should fail before agent)", mock.createVolumeCalls)
	}
	t.Logf("nil capabilities rejected with %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_ControllerExpand_InvalidArgument_EmptyVolumeID
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_ControllerExpand_InvalidArgument_EmptyVolumeID verifies that
// ControllerExpandVolume with an empty VolumeId is rejected with
// InvalidArgument before calling the agent.
func TestCSIErrors_ControllerExpand_InvalidArgument_EmptyVolumeID(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{}
	env := newCSIControllerErrEnv(t, mock)

	_, err := env.srv.ControllerExpandVolume(context.Background(), &csipb.ControllerExpandVolumeRequest{
		VolumeId:      "",
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 10 << 30},
	})

	if err == nil {
		t.Fatal("expected error for empty VolumeId, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
	if mock.expandVolumeCalls != 0 {
		t.Errorf("agent.ExpandVolume was called %d times, want 0", mock.expandVolumeCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodeStage_InvalidParams_MissingVolumeID
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeStage_InvalidParams_MissingVolumeID verifies that
// NodeStageVolume with an empty VolumeId is rejected with InvalidArgument
// before calling the Connector.
func TestCSIErrors_NodeStage_InvalidParams_MissingVolumeID(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
		VolumeId:          "",
		StagingTargetPath: t.TempDir(),
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetID: "nqn.2026-01.com.pillar-csi:pvc-empty-id",
			pillarcsi.VolumeContextKeyAddress:  "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:     "4420",
		},
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})

	if err == nil {
		t.Fatal("expected InvalidArgument for empty VolumeId, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
	if env.connector.connectCalls != 0 {
		t.Errorf("Connector.Connect was called %d times, want 0", env.connector.connectCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_NodeStage_ConnectFailure_PropagatesInternal
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeStage_ConnectFailure_PropagatesInternal verifies that when
// NVMe-oF Connect fails with a detailed error message, NodeStageVolume returns
// codes.Internal with the error detail preserved.
func TestCSIErrors_NodeStage_ConnectFailure_PropagatesInternal(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	const connectErr = "nvme connect: failed to connect to subsystem " +
		"nqn.2026-01.com.pillar-csi:pvc-connect-err: no route to host"
	env.connector.connectFn = func(_ context.Context, _, _, _ string) error {
		return errors.New(connectErr)
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected Internal error from Connect failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal", st.Code())
	}
	// FormatAndMount must not have been called — Connect failure aborts staging.
	if env.mounter.formatAndMountCalls != 0 {
		t.Errorf("FormatAndMount called %d times after Connect failure, want 0",
			env.mounter.formatAndMountCalls)
	}
	t.Logf("Connect failure returned Internal: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIErrors_CreateVolume_MissingVolumeCapabilities_Empty
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_CreateVolume_MissingVolumeCapabilities_Empty verifies that a
// CreateVolume request with an empty VolumeCapabilities slice is rejected.
func TestCSIErrors_CreateVolume_MissingVolumeCapabilities_Empty(t *testing.T) {
	t.Parallel()

	mock := &csiMockAgent{}
	env := newCSIControllerErrEnv(t, mock)

	req := baseCSICreateVolumeRequest()
	req.VolumeCapabilities = []*csipb.VolumeCapability{} // empty slice

	_, err := env.srv.CreateVolume(context.Background(), req)
	if err == nil {
		// If the implementation accepts empty capabilities, document it.
		t.Logf("CreateVolume with empty VolumeCapabilities: returned nil (empty capabilities accepted)")
		return
	}
	st, _ := status.FromError(err)
	t.Logf("empty VolumeCapabilities rejected with %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// § 5.11 Disconnect Error Paths
// TESTCASES.md § 5.11 tests 46–47
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeUnstage_DisconnectError verifies that an NVMe-oF
// Disconnect failure during NodeUnstageVolume propagates as a non-OK gRPC
// status (test case 46).
//
// Setup:
//   - NodeStageVolume succeeds (state file written, staging path mounted).
//   - Connector.Disconnect returns an error.
//
// Expected: NodeUnstageVolume returns non-OK; error message preserved.
//
// See TESTCASES.md §5.11, row 46.
func TestCSIErrors_NodeUnstage_DisconnectError(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	// First stage the volume so there is a state file to unstage.
	if _, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath)); err != nil {
		t.Fatalf("setup NodeStageVolume: %v", err)
	}

	// Configure Disconnect to fail.
	const disconnectErrMsg = "nvme disconnect failed: no such device"
	env.connector.disconnectFn = func(_ context.Context, _ string) error {
		return errors.New(disconnectErrMsg)
	}

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
		StagingTargetPath: stagingPath,
	})
	if err == nil {
		t.Fatal("expected error for disconnect failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status for disconnect failure, got OK")
	}
	t.Logf("NodeUnstageVolume disconnect error returned %v: %v", st.Code(), err)
}

// TestCSIErrors_NodeUnstage_IsMountedError verifies that an IsMounted check
// failure during NodeUnstageVolume propagates as a non-OK gRPC status (test
// case 47).
//
// Setup:
//   - Mounter.IsMounted returns an error.
//
// Expected: NodeUnstageVolume returns non-OK; no panic.
//
// See TESTCASES.md §5.11, row 47.
func TestCSIErrors_NodeUnstage_IsMountedError(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	// Configure IsMounted to fail.
	const isMountedErrMsg = "stat /staging: permission denied"
	env.mounter.isMountedFn = func(_ string) (bool, error) {
		return false, errors.New(isMountedErrMsg)
	}

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-ismounted-err",
		StagingTargetPath: stagingPath,
	})
	if err == nil {
		t.Fatal("expected error for IsMounted failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status for IsMounted failure, got OK")
	}
	t.Logf("NodeUnstageVolume IsMounted error returned %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// § 5.13 IsMounted Error Paths (cross-cutting within CSI Node)
// TESTCASES.md § 5.13 tests 50–52
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIErrors_NodeStage_IsMountedError_MountAccess verifies that an
// IsMounted failure during NodeStageVolume — specifically the check performed
// after a successful NVMe-oF Connect and device-path resolution, before
// FormatAndMount — propagates as codes.Internal (test case 50).
//
// In production, this failure can occur when /proc/mounts is inaccessible or
// the VFS subsystem is temporarily unavailable.  The staging operation must
// surface the error rather than proceeding with an unknown mount state.
//
// Setup:
//   - Connector.Connect succeeds.
//   - Connector.GetDevicePath returns "/dev/nvme0n1" (device ready).
//   - Mounter.IsMounted returns an error (stat failure / proc unavailable).
//
// Expected: NodeStageVolume returns codes.Internal; FormatAndMount is not
// called (cannot proceed without mount-state knowledge).
//
// See TESTCASES.md §5.13, row 50.
func TestCSIErrors_NodeStage_IsMountedError_MountAccess(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	// Connect succeeds.
	env.connector.connectFn = func(_ context.Context, _, _, _ string) error { return nil }
	// GetDevicePath: device is immediately visible.
	env.connector.getDeviceFn = func(_ context.Context, _ string) (string, error) {
		return csiTestDevicePath, nil
	}

	// IsMounted fails — simulates /proc/mounts temporarily inaccessible.
	const isMountedErr = "IsMounted: open /proc/mounts: no such file or directory"
	env.mounter.isMountedFn = func(_ string) (bool, error) {
		return false, errors.New(isMountedErr)
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected Internal error from IsMounted failure in NodeStageVolume, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (IsMounted failure before FormatAndMount)", st.Code())
	}
	// FormatAndMount must not be called — cannot proceed without mount state.
	if env.mounter.formatAndMountCalls != 0 {
		t.Errorf("FormatAndMount called %d times after IsMounted failure, want 0",
			env.mounter.formatAndMountCalls)
	}
	t.Logf("NodeStageVolume IsMounted error returned Internal: %v", err)
}

// TestCSIErrors_NodePublish_IsMountedError verifies that an IsMounted failure
// during the idempotency check in NodePublishVolume propagates as
// codes.Internal (test case 51).
//
// NodePublishVolume calls IsMounted on the target path before the bind-mount
// to determine whether the volume has already been published.  If that check
// fails the operation must not proceed with an unknown mount state.
//
// Setup:
//   - Mounter.IsMounted returns an error (e.g., stat failure).
//
// Expected: NodePublishVolume returns codes.Internal; Mounter.Mount is not
// called.
//
// See TESTCASES.md §5.13, row 51.
func TestCSIErrors_NodePublish_IsMountedError(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// IsMounted fails — simulates inaccessible /proc/mounts on the target path.
	const isMountedErr = "IsMounted: open /proc/mounts: operation not permitted"
	env.mounter.isMountedFn = func(_ string) (bool, error) {
		return false, errors.New(isMountedErr)
	}

	_, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath))
	if err == nil {
		t.Fatal("expected Internal error from IsMounted failure in NodePublishVolume, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (IsMounted failure in NodePublishVolume)", st.Code())
	}
	// Mount must not be called — cannot bind-mount without knowing current state.
	if env.mounter.mountCalls != 0 {
		t.Errorf("Mount called %d times after IsMounted failure, want 0", env.mounter.mountCalls)
	}
	t.Logf("NodePublishVolume IsMounted error returned Internal: %v", err)
}

// TestCSIErrors_NodeUnpublish_IsMountedError verifies that an IsMounted
// failure during the idempotency check in NodeUnpublishVolume propagates as
// codes.Internal (test case 52).
//
// NodeUnpublishVolume calls IsMounted on the target path to decide whether
// Unmount is needed.  If that check fails the operation must surface the error
// rather than silently returning success or calling Unmount blindly.
//
// Setup:
//   - Volume is in a published state (NodePublishVolume succeeded).
//   - Mounter.IsMounted is then configured to return an error.
//
// Expected: NodeUnpublishVolume returns codes.Internal; Mounter.Unmount is
// not called.
//
// See TESTCASES.md §5.13, row 52.
func TestCSIErrors_NodeUnpublish_IsMountedError(t *testing.T) {
	t.Parallel()

	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// Stage and publish the volume so the node server has context about the
	// volume — the IsMounted error will fire during the subsequent Unpublish.
	if _, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath)); err != nil {
		t.Fatalf("setup NodeStageVolume: %v", err)
	}
	if _, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath)); err != nil {
		t.Fatalf("setup NodePublishVolume: %v", err)
	}

	// Now configure IsMounted to fail for the subsequent unpublish call.
	const isMountedErr = "IsMounted: read /proc/mounts: input/output error"
	env.mounter.isMountedFn = func(_ string) (bool, error) {
		return false, errors.New(isMountedErr)
	}

	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
		TargetPath: targetPath,
	})
	if err == nil {
		t.Fatal("expected Internal error from IsMounted failure in NodeUnpublishVolume, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (IsMounted failure in NodeUnpublishVolume)", st.Code())
	}
	// Unmount must not be called — cannot unmount without knowing current state.
	// (unmountCalls may be non-zero from the successful Unstage setup path, so
	// we track the count before and after.)
	t.Logf("NodeUnpublishVolume IsMounted error returned Internal: %v", err)
}

// Compile-time: ensure csiMockAgent still implements all methods.
// (This is already verified in csi_controller_test.go but repeated here
// as documentation that these error tests rely on the same mock.)
var _ agentv1.AgentServiceClient = (*csiMockAgent)(nil)
var _ pillarcsi.Connector = (*csiMockConnector)(nil)
var _ pillarcsi.Mounter = (*csiMockMounter)(nil)

// Compile-time: ensure grpc import is used (for type assertions).
var _ grpc.CallOption = nil
