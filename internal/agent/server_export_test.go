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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// testVolumeNQN is the NQN derived from testVolumeID ("tank/pvc-abc").
const testVolumeNQN = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"

// testHostNQN is a reusable initiator NQN for access-control tests.
const testHostNQN = "nqn.2023-01.io.example:host-1"

// newExportTestServer creates a Server with a mock backend for "tank" pool
// and a temp directory as the configfs root.  It injects AlwaysPresentChecker
// so that ExportVolume skips the device-existence polling step — the
// device-poll logic is tested independently in the nvmeof package.  Fencing
// marks are kept in a per-test state directory.
func newExportTestServer(t *testing.T, mb *mockBackend) (srv *agent.Server, cfgRoot string) {
	t.Helper()
	cfgRoot = t.TempDir()
	backends := map[string]backend.VolumeBackend{testPool: mb}
	s := agent.NewServer(backends, cfgRoot, agent.WithDrainStateDir(t.TempDir()))
	agent.SetDeviceChecker(t, s, nvmeof.AlwaysPresentChecker)
	return s, cfgRoot
}

// nvmeofExportParams builds an NvmeofTcpExportParams-wrapped ExportParams.
func nvmeofExportParams(addr string, port int32) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{
				BindAddress: addr,
				Port:        port,
			},
		},
	}
}

// ExportVolume tests.

func TestExportVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newExportTestServer(t, mb)

	resp, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume unexpected error: %v", err)
	}

	// Returned ExportInfo must have the correct NQN and address.
	if resp.GetExportInfo().GetTargetId() != testVolumeNQN {
		t.Errorf("TargetId = %q, want %q", resp.GetExportInfo().GetTargetId(), testVolumeNQN)
	}
	if resp.GetExportInfo().GetAddress() != "192.168.1.10" {
		t.Errorf("Address = %q, want 192.168.1.10", resp.GetExportInfo().GetAddress())
	}
	if resp.GetExportInfo().GetPort() != 4420 {
		t.Errorf("Port = %d, want 4420", resp.GetExportInfo().GetPort())
	}

	// configfs subsystem directory must have been created.
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created: %v", statErr)
	}
}

func TestExportVolume_DefaultPort(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, _ := newExportTestServer(t, mb)

	resp, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 0), // port 0 → default 4420
	})
	if err != nil {
		t.Fatalf("ExportVolume unexpected error: %v", err)
	}
	if resp.GetExportInfo().GetPort() != 4420 {
		t.Errorf("Port = %d, want 4420 (default)", resp.GetExportInfo().GetPort())
	}
}

func TestExportVolume_ExplicitDevicePath(t *testing.T) {
	t.Parallel()
	// When device_path is provided in the request, backendFor should NOT be called.
	mb := &mockBackend{} // DevicePath on mock returns "" but request overrides it.
	srv, cfgRoot := newExportTestServer(t, mb)

	resp, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		DevicePath:   "/dev/zvol/tank/pvc-abc",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume unexpected error: %v", err)
	}

	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created: %v", statErr)
	}
	_ = resp
}

func TestExportVolume_WrongProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
	})
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

func TestExportVolume_MissingParams(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		// No ExportParams set — GetNvmeofTcp() will return nil.
	})
	if err == nil {
		t.Fatal("expected error for missing params, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestExportVolume_InvalidVolumeID(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     "no-slash",
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420),
	})
	if err == nil {
		t.Fatal("expected error for invalid volumeID, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestExportVolume_DeviceNotReady verifies that ExportVolume returns a
// FailedPrecondition gRPC status with a descriptive message when the zvol
// block device never appears within the polling window.
//
// The test injects a checker that always reports the device absent and shrinks
// the poll interval + timeout to small values so the test completes quickly.
func TestExportVolume_DeviceNotReady(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, _ := newExportTestServer(t, mb)

	// Override the AlwaysPresentChecker set by newExportTestServer with a
	// checker that always reports the device as absent, simulating a zvol that
	// never materializes.
	neverPresentChecker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil
	})
	agent.SetDeviceChecker(t, srv, neverPresentChecker)

	// Use a very short timeout so the test finishes in milliseconds.
	agent.SetDevicePollParams(t, srv, 10*time.Millisecond, 50*time.Millisecond)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("192.168.1.10", 4420),
	})

	if err == nil {
		t.Fatal("expected FailedPrecondition error when device is absent, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("gRPC code = %v, want FailedPrecondition", st.Code())
	}
	// The error message must mention the device path so operators can diagnose.
	const wantPath = "/dev/zvol/tank/pvc-abc"
	if !strings.Contains(st.Message(), wantPath) {
		t.Errorf("error message %q does not mention device path %q", st.Message(), wantPath)
	}
}

// TestExportVolume_DeviceAppearsBeforeTimeout verifies that ExportVolume
// succeeds when the checker starts returning "present" before the poll window
// closes — the device just needs a few probes to settle.
func TestExportVolume_DeviceAppearsBeforeTimeout(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newExportTestServer(t, mb)

	// Checker that reports absent for the first 3 calls, then present.
	callCount := 0
	delayedChecker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		callCount++
		return callCount > 3, nil
	})
	agent.SetDeviceChecker(t, srv, delayedChecker)

	// Generous enough timeout to allow 3 misses at 10 ms interval.
	agent.SetDevicePollParams(t, srv, 10*time.Millisecond, 5*time.Second)

	resp, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume unexpected error after device appeared: %v", err)
	}
	// Subsystem directory must have been created (configfs write happened).
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created: %v", statErr)
	}
	_ = resp
}

// UnexportVolume tests.

func TestUnexportVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, _ := newExportTestServer(t, mb)

	// First export (creates configfs state).
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume setup: %v", err)
	}

	// Then unexport.
	_, err = srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	if err != nil {
		t.Fatalf("UnexportVolume unexpected error: %v", err)
	}
}

func TestUnexportVolume_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	// Remove on a volume that was never exported must succeed (idempotent).
	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	if err != nil {
		t.Fatalf("UnexportVolume idempotent unexpected error: %v", err)
	}
}

func TestUnexportVolume_WrongProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
	})
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// AllowInitiator tests.

func TestAllowInitiator_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newExportTestServer(t, mb)

	// Export first so the subsystem dir exists.
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume setup: %v", err)
	}

	hostNQN := testHostNQN
	_, err = srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  hostNQN,
	})
	if err != nil {
		t.Fatalf("AllowInitiator unexpected error: %v", err)
	}

	// The host directory and allowed_hosts symlink must exist.
	hostDir := filepath.Join(cfgRoot, "nvmet", "hosts", hostNQN)
	if _, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("host dir not created: %v", statErr)
	}
	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "allowed_hosts", hostNQN)
	if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Errorf("allowed_hosts symlink not created: %v", statErr)
	}
}

func TestAllowInitiator_Idempotent(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, _ := newExportTestServer(t, mb)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume setup: %v", err)
	}

	req := &agentv1.AllowInitiatorRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  testHostNQN,
	}
	if _, err = srv.AllowInitiator(context.Background(), req); err != nil {
		t.Fatalf("first AllowInitiator: %v", err)
	}
	// Second call must succeed (idempotent).
	if _, err = srv.AllowInitiator(context.Background(), req); err != nil {
		t.Fatalf("second AllowInitiator: %v", err)
	}
}

func TestAllowInitiator_WrongProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	_, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
		InitiatorId:  "10.0.0.0/24",
	})
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// DenyInitiator tests.

func TestDenyInitiator_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newExportTestServer(t, mb)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume setup: %v", err)
	}

	hostNQN := testHostNQN
	// Allow first.
	if _, err = srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  hostNQN,
	}); err != nil {
		t.Fatalf("AllowInitiator setup: %v", err)
	}

	// Then deny.
	if _, err = srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  hostNQN,
	}); err != nil {
		t.Fatalf("DenyInitiator unexpected error: %v", err)
	}

	// The allowed_hosts symlink must be gone.
	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "allowed_hosts", hostNQN)
	if _, statErr := os.Lstat(linkPath); !os.IsNotExist(statErr) {
		t.Errorf("allowed_hosts symlink still exists after Deny: stat=%v", statErr)
	}
}

func TestDenyInitiator_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	// Deny a host that was never allowed — must return success (idempotent).
	_, err := srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  testHostNQN,
	})
	if err != nil {
		t.Fatalf("DenyInitiator idempotent unexpected error: %v", err)
	}
}

func TestDenyInitiator_WrongProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	_, err := srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId:     testVolumeID,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
		InitiatorId:  "WORKGROUP\\host1",
	})
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// ListExports tests.

func TestListExports_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ListExports(context.Background(), &agentv1.ListExportsRequest{})
	if err != nil {
		t.Fatalf("ListExports unexpected error: %v", err)
	}
	if len(resp.GetExports()) != 0 {
		t.Errorf("expected empty exports map, got %d entries", len(resp.GetExports()))
	}
}

// A volume pinned to a pre-existing LV is re-verified before any export or
// access grant: an LV recreated or removed under the same name is refused
// before any configfs write, the durable mark is left byte-identical, and
// the refusal survives an agent restart.  The healthy pinned LV still
// exports, and an unpinned volume is never verified.
func TestPinnedSource_RecheckedBeforeExport(t *testing.T) {
	t.Parallel()
	changes := map[string]struct {
		change  func(*mockLVBackend)
		succeed bool
	}{
		"verified": {change: func(*mockLVBackend) {}, succeed: true},
		"replaced": {change: func(b *mockLVBackend) { b.replaceLV() }},
		"missing":  {change: func(b *mockLVBackend) { delete(b.lvs, testVolumeID) }},
	}
	for name, tc := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			srv, stateDir, cfgRoot := newLVTestServer(t, b)
			seeded := seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), true))
			tc.change(b)

			_, err := srv.ExportVolume(context.Background(), pinnedExportRequest())
			if tc.succeed {
				if err != nil {
					t.Fatalf("ExportVolume of the pinned LV: %v", err)
				}
				if n := nvmetSubsystems(t, cfgRoot); n != 1 {
					t.Fatalf("NVMe-oF subsystems = %d, want 1", n)
				}
				return
			}
			requireExportAndGrantRefused(t, srv, cfgRoot, name, err)
			requireMarkUnchanged(t, stateDir, seeded, "refused export")

			// After an agent restart the replacement is still not the pinned
			// LV: the export is refused again and nothing is written.
			srv = restartLVServer(t, b, stateDir, cfgRoot)
			_, err = srv.ExportVolume(context.Background(), pinnedExportRequest())
			requireCode(t, "post-restart ExportVolume of a "+name+" LV", err, codes.FailedPrecondition)
			if n := nvmetSubsystems(t, cfgRoot); n != 0 {
				t.Fatalf("post-restart refused export created %d NVMe-oF subsystems", n)
			}
			requireMarkUnchanged(t, stateDir, seeded, "post-restart refused export")
		})
	}
}

// pinnedExportRequest exports testVolumeID's pinned LV over NVMe-oF for
// lifecycle-a.
func pinnedExportRequest() *agentv1.ExportVolumeRequest {
	return &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420),
		DevicePath:   testLVDevicePath,
		Fence:        token2("lifecycle-a", 6),
	}
}

// requireExportAndGrantRefused asserts exportErr refused the export of a
// name LV without creating an NVMe-oF subsystem, and that the grant path is
// refused for the same reason without creating a host entry.
func requireExportAndGrantRefused(t *testing.T, srv *agent.Server, cfgRoot, name string, exportErr error) {
	t.Helper()
	requireCode(t, "ExportVolume of a "+name+" LV", exportErr, codes.FailedPrecondition)
	if msg := status.Convert(exportErr).Message(); !strings.Contains(msg, testVolumeID) {
		t.Fatalf("refusal %q does not name volume %q", msg, testVolumeID)
	}
	if n := nvmetSubsystems(t, cfgRoot); n != 0 {
		t.Fatalf("refused export created %d NVMe-oF subsystems", n)
	}

	// The grant path is refused for the same reason: no host is
	// admitted to an export that must not exist.
	_, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     testVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  testHostNQN,
		Fence:        token2("lifecycle-a", 6),
	})
	requireCode(t, "AllowInitiator on a "+name+" LV", err, codes.FailedPrecondition)
	hostDir := filepath.Join(cfgRoot, "nvmet", "hosts", testHostNQN)
	if _, statErr := os.Stat(hostDir); !os.IsNotExist(statErr) {
		t.Fatalf("refused grant created host dir %q: stat=%v", hostDir, statErr)
	}
}
