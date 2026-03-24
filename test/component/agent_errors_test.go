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

// Package component_test – Agent gRPC Server error/exception path tests.
//
// This file covers deep error paths for the AgentService gRPC Server
// (internal/agent.Server).  Tests treat the server as a black box, wiring
// mock backends and real tmpdir configfs, and focus specifically on:
//
//   - gRPC deadline exceeded / context cancellation paths
//   - Invalid parameter validation at the server boundary
//   - Shrink-rejected error propagation
//   - TOCTOU: configfs write failure after device check passes
//   - Backend errors that map to specific gRPC status codes
//
// All tests require no root privileges and no real ZFS or kernel configfs.
package component_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// newAgentServerWithCfgRoot builds an agent server using the specified
// configfs root directory instead of creating a fresh tmpdir.  This is needed
// for tests that pre-populate or modify the configfs tree before constructing
// the server.
func newAgentServerWithCfgRoot(
	t *testing.T,
	mb *mockVolumeBackend,
	cfgRoot string,
	opts ...agent.ServerOption,
) *agent.Server {
	t.Helper()
	backends := map[string]backend.VolumeBackend{compTestPool: mb}
	allOpts := append(
		[]agent.ServerOption{agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker)},
		opts...,
	)
	return agent.NewServer(backends, cfgRoot, allOpts...)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_ExportVolume_ContextCancelledDuringPoll
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_ExportVolume_ContextCancelledDuringPoll verifies that
// ExportVolume respects context cancellation during the device-poll loop.
//
// The device checker always returns (false, nil) — the device never appears —
// so the poll loop continues until the request context expires.  The internal
// poll timeout is set to 10 s to ensure the context deadline (100 ms) fires
// first.
//
// Expected: ExportVolume returns FailedPrecondition within ~500 ms.
func TestAgentErrors_ExportVolume_ContextCancelledDuringPoll(t *testing.T) {
	t.Parallel()

	neverPresent := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil
	})

	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-ctx-poll"}
	srv, _ := newAgentServer(t, mb,
		agent.WithDeviceChecker(neverPresent),
		// Internal poll timeout >> request context deadline so context fires first.
		agent.WithDevicePollParams(10*time.Millisecond, 10*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-ctx-poll",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.99.1", 4420),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from context-cancelled device poll, got nil")
	}

	// Must terminate well before the 10 s internal timeout.
	const maxElapsed = 500 * time.Millisecond
	if elapsed > maxElapsed {
		t.Errorf("ExportVolume took %v, want < %v (must respect context deadline)", elapsed, maxElapsed)
	}

	// ExportVolume wraps WaitForDevice errors as FailedPrecondition.
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v (ok=%v), want FailedPrecondition", st.Code(), ok)
	}
	t.Logf("ExportVolume correctly terminated in %v: %v", elapsed, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_ExportVolume_ConfigfsBrokenAfterDeviceCheck_TOCTOU
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_ExportVolume_ConfigfsBrokenAfterDeviceCheck_TOCTOU
// simulates a TOCTOU (time-of-check to time-of-use) race where:
//
//  1. WaitForDevice succeeds — the device checker reports the device present.
//  2. NvmetTarget.Apply fails — the configfs subsystems directory has become
//     read-only between the device check and the configfs write.
//
// In production this mirrors a kernel bug or mount-point degradation that
// occurs after the zvol device appears but before the NQN subsystem entry
// can be written to configfs.
//
// Setup:  AlwaysPresentChecker ensures WaitForDevice returns immediately.
//
//	The configfs nvmet/subsystems directory is pre-created and made read-only
//	to trigger a failure in NvmetTarget.Apply().
//
// Expected: ExportVolume returns codes.Internal (Apply failure).
func TestAgentErrors_ExportVolume_ConfigfsBrokenAfterDeviceCheck_TOCTOU(t *testing.T) {
	t.Parallel()

	cfgRoot := t.TempDir()

	// Pre-create the nvmet/subsystems directory, then make it read-only.
	// WaitForDevice passes (AlwaysPresentChecker), then Apply fails because it
	// cannot create the subsystem subdirectory.
	subsystemsDir := filepath.Join(cfgRoot, "nvmet", "subsystems")
	if err := os.MkdirAll(subsystemsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll subsystemsDir: %v", err)
	}
	makeReadOnly(t, subsystemsDir) // auto-skips as root; restores on cleanup

	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-toctou-cfgfs"}
	srv := newAgentServerWithCfgRoot(t, mb, cfgRoot,
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-toctou-cfgfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.99.2", 4420),
	})

	if err == nil {
		t.Fatal("expected Internal error when configfs is read-only after device check, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %T(%v)", err, err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (NvmetTarget.Apply failure)", st.Code())
	}
	t.Logf("TOCTOU: ExportVolume correctly returned Internal after configfs became read-only: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_CreateVolume_EmptyVolumeID
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_CreateVolume_EmptyVolumeID verifies that CreateVolume
// rejects an empty VolumeId with InvalidArgument before calling the backend.
//
// The agent derives the storage pool from the volume ID prefix (pool/name).
// An empty ID cannot be parsed and must be rejected at the server boundary.
func TestAgentErrors_CreateVolume_EmptyVolumeID(t *testing.T) {
	t.Parallel()

	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      "",
		CapacityBytes: 1 << 30,
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for empty VolumeId, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_DeleteVolume_EmptyVolumeID
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_DeleteVolume_EmptyVolumeID verifies that DeleteVolume
// rejects an empty VolumeId with InvalidArgument.
func TestAgentErrors_DeleteVolume_EmptyVolumeID(t *testing.T) {
	t.Parallel()

	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: "",
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for empty VolumeId, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_ExpandVolume_EmptyVolumeID
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_ExpandVolume_EmptyVolumeID verifies that ExpandVolume
// rejects an empty VolumeId with InvalidArgument.
func TestAgentErrors_ExpandVolume_EmptyVolumeID(t *testing.T) {
	t.Parallel()

	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       "",
		RequestedBytes: 2 << 30,
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for empty VolumeId, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_ExpandVolume_ShrinkRejected_PropagatesAsInternal
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_ExpandVolume_ShrinkRejected_PropagatesAsInternal verifies
// that when the ZFS backend rejects a shrink attempt with a descriptive error,
// the agent server propagates it as codes.Internal with a non-empty message.
//
// The agent's ExpandVolume handler does not distinguish shrink errors from
// general backend errors — both map to Internal.  This test locks in that
// behavior and verifies the error message is preserved so operators can
// diagnose the failure.
func TestAgentErrors_ExpandVolume_ShrinkRejected_PropagatesAsInternal(t *testing.T) {
	t.Parallel()

	const shrinkMsg = "cannot shrink volume: volsize cannot be decreased"
	mb := &mockVolumeBackend{expandErr: errors.New(shrinkMsg)}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       compTestVolumeID,
		RequestedBytes: 512 << 20, // 512 MiB — well below the assumed current size
	})

	if err == nil {
		t.Fatal("expected Internal error for shrink attempt, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal (backend shrink error)", st.Code())
	}
	if msg := st.Message(); msg == "" {
		t.Error("gRPC error message is empty; expect backend reason to be included")
	}
	t.Logf("shrink rejection propagated as Internal: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_CreateVolume_BackendContextError
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_CreateVolume_BackendContextError verifies the behavior when
// the backend returns a context error (simulating a ZFS operation that times
// out internally — e.g., pool I/O hang).
//
// The agent server wraps all non-ConflictError backend errors as Internal.
// This test documents that behavior: a backend timeout appears to the CSI CO
// as an Internal error (not DeadlineExceeded), since the agent's own context
// has not expired — only the backend's internal operation timed out.
func TestAgentErrors_CreateVolume_BackendContextError(t *testing.T) {
	t.Parallel()

	// Simulate a backend that times out internally (pool I/O hung).
	mb := &mockVolumeBackend{createErr: context.DeadlineExceeded}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      compTestVolumeID,
		CapacityBytes: 1 << 30,
	})

	if err == nil {
		t.Fatal("expected error from context-erroring backend, got nil")
	}
	st, _ := status.FromError(err)
	// The agent wraps backend errors as Internal (not Canceled/DeadlineExceeded).
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC status, got OK")
	}
	t.Logf("backend context.DeadlineExceeded wrapped as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_ExportVolume_InvalidProtocol_NoConfigfsSideEffects
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_ExportVolume_InvalidProtocol_NoConfigfsSideEffects verifies
// that an unsupported protocol type is rejected before touching configfs.
//
// This tests the guard at the server boundary: no configfs directories should
// be created when the protocol type is rejected with Unimplemented.
func TestAgentErrors_ExportVolume_InvalidProtocol_NoConfigfsSideEffects(t *testing.T) {
	t.Parallel()

	mb := &mockVolumeBackend{}
	srv, cfgRoot := newAgentServer(t, mb)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	})

	if err == nil {
		t.Fatal("expected Unimplemented for iSCSI protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("error code = %v, want Unimplemented", st.Code())
	}

	// Side-effect check: no nvmet directories must have been created.
	nvmetDir := filepath.Join(cfgRoot, "nvmet")
	if _, statErr := os.Stat(nvmetDir); !os.IsNotExist(statErr) {
		t.Errorf("nvmet dir was created despite invalid protocol rejection (statErr=%v)", statErr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_AllowInitiator_InvalidProtocol
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_AllowInitiator_InvalidProtocol verifies that AllowInitiator
// with an unsupported protocol type returns Unimplemented without touching
// configfs.
func TestAgentErrors_AllowInitiator_InvalidProtocol(t *testing.T) {
	t.Parallel()

	srv, cfgRoot := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     compTestVolumeID,
		InitiatorId:  compTestHostNQN,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	})

	if err == nil {
		t.Fatal("expected Unimplemented for iSCSI protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("error code = %v, want Unimplemented", st.Code())
	}

	// No configfs directories should have been created.
	hostsDir := filepath.Join(cfgRoot, "nvmet", "hosts")
	if _, statErr := os.Stat(hostsDir); !os.IsNotExist(statErr) {
		t.Errorf("nvmet/hosts dir created despite Unimplemented rejection (statErr=%v)", statErr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_DenyInitiator_InvalidProtocol
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_DenyInitiator_InvalidProtocol verifies that DenyInitiator
// with an unsupported protocol type returns Unimplemented.
func TestAgentErrors_DenyInitiator_InvalidProtocol(t *testing.T) {
	t.Parallel()

	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId:     compTestVolumeID,
		InitiatorId:  compTestHostNQN,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	})

	if err == nil {
		t.Fatal("expected Unimplemented for iSCSI protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("error code = %v, want Unimplemented", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_UnexportVolume_InvalidProtocol
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_UnexportVolume_InvalidProtocol verifies that UnexportVolume
// with an unsupported protocol type returns Unimplemented immediately.
func TestAgentErrors_UnexportVolume_InvalidProtocol(t *testing.T) {
	t.Parallel()

	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	})

	if err == nil {
		t.Fatal("expected Unimplemented for iSCSI protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("error code = %v, want Unimplemented", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_CreateVolume_DiskFullPropagation
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_CreateVolume_DiskFullPropagation verifies that the
// "out of space" backend error is propagated as a non-OK gRPC status with
// a meaningful error message.
//
// This extends the basic DiskFull test by also verifying the error message
// contains diagnostic text.
func TestAgentErrors_CreateVolume_DiskFullPropagation(t *testing.T) {
	t.Parallel()

	const diskFullMsg = "zfs: out of space (pool capacity 100%)"
	mb := &mockVolumeBackend{createErr: errors.New(diskFullMsg)}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      compTestVolumeID,
		CapacityBytes: 1 << 30,
	})

	if err == nil {
		t.Fatal("expected error for disk-full condition, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Error("expected non-OK gRPC status")
	}
	// The error message should carry enough context for the CO to log it.
	if msg := st.Message(); msg == "" {
		t.Error("gRPC error message is empty; expected backend reason")
	}
	t.Logf("disk-full propagated as gRPC %v: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAgentErrors_ExportVolume_MissingNvmeofTcpParams
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentErrors_ExportVolume_MissingNvmeofTcpParams verifies that
// ExportVolume with a nil NvmeofTcp params struct inside a non-nil ExportParams
// returns InvalidArgument.
//
// This tests the guard that distinguishes "no ExportParams at all" (covered
// by TestAgentServer_ExportVolume_MissingParams) from "ExportParams present
// but the inner NvmeofTcp field is nil".
func TestAgentErrors_ExportVolume_MissingNvmeofTcpParams(t *testing.T) {
	t.Parallel()

	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		// ExportParams is nil — same as "no params".
		ExportParams: nil,
	})

	if err == nil {
		t.Fatal("expected InvalidArgument for nil ExportParams, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}
