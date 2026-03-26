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

// Component 6: Cross-cutting network/filesystem exception path tests.
//
// These tests verify that all major components handle exceptional OS-level
// conditions gracefully: permission denied errors, concurrent filesystem
// modifications, context cancellation, and timing races.  Each test is a
// black-box scenario that crosses package boundaries.
//
// Tests here correspond to TESTCASES.md Component 6 (XC1–XC8).
package component_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ---------------------------------------------------------------------------
// XC1: configfs write permission denied
// ---------------------------------------------------------------------------.

// TestException_ConfigfsWritePermissionDenied verifies that NvmetTarget.Apply
// returns a descriptive error (rather than panicking) when the configfs
// subsystems directory is read-only and a new subdirectory cannot be created.
//
// This mirrors a real-world scenario where the nvmet configfs mount is
// unexpectedly read-only or the process has insufficient privileges.
//
// Setup:  pre-create <tmpdir>/nvmet/subsystems/ with mode 0555.
// Expected outcome: Apply returns a non-nil error; no panic.
func TestException_ConfigfsWritePermissionDenied(t *testing.T) {
	t.Parallel()

	tmpdir := t.TempDir()

	// Pre-create the nvmet/subsystems directory so that NvmetTarget.Apply
	// attempts to create a subdirectory inside it, then make it read-only.
	subsystemsDir := filepath.Join(tmpdir, "nvmet", "subsystems")
	if err := os.MkdirAll(subsystemsDir, 0o750); err != nil {
		t.Fatalf("MkdirAll %q: %v", subsystemsDir, err)
	}
	makeReadOnly(t, subsystemsDir) // auto-skips as root; restores 0755 on cleanup

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: "nqn.2026-01.com.bhyoo:pvc-xc1-permdeny",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-xc1",
		BindAddress:  "192.168.1.1",
		Port:         4420,
	}

	err := tgt.Apply()
	if err == nil {
		t.Fatal("expected error when configfs subsystems dir is read-only, got nil")
	}
	t.Logf("Apply correctly returned error on permission-denied configfs: %v", err)
}

// ---------------------------------------------------------------------------
// XC2: concurrent Apply — directory creation idempotency under race
// ---------------------------------------------------------------------------.

// TestException_ConfigfsDirCreateRace verifies that multiple goroutines can
// call NvmetTarget.Apply simultaneously on different volumes that share the
// same underlying port directory, without corrupting the configfs state.
//
// In production this scenario arises when several PVCs are provisioned
// concurrently on the same storage node (all using the same bind address and
// TCP port).
//
// Setup:  N goroutines each have a unique NQN but share the same
//
//	BindAddress+Port (same port directory).
//
// Expected outcome: all goroutines return nil; all subsystem directories exist.
func TestException_ConfigfsDirCreateRace(t *testing.T) {
	t.Parallel()

	tmpdir := t.TempDir()

	const goroutines = 5

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	start := make(chan struct{}) // closed to release all goroutines simultaneously

	for i := range goroutines {
		wg.Add(1)

		go func() { //nolint:modernize // start channel needed for synchronized concurrent launch
			defer wg.Done()
			<-start // wait until all goroutines are ready, then run concurrently
			tgt := &nvmeof.NvmetTarget{
				ConfigfsRoot: tmpdir,
				// Unique NQN per goroutine: no symlink path collision.
				SubsystemNQN: fmt.Sprintf("nqn.2026-01.com.bhyoo:pvc-race-%d", i),
				NamespaceID:  1,
				DevicePath:   fmt.Sprintf("/dev/zvol/tank/pvc-race-%d", i),
				// Same bind address + port: all goroutines share one port directory.
				// os.MkdirAll handles the concurrent EEXIST race internally.
				BindAddress: "192.168.100.1",
				Port:        4421,
			}
			errs[i] = tgt.Apply()
		}()
	}
	close(start) // unleash all goroutines at once
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Apply error: %v", i, err)
		}
	}

	// Every subsystem directory must have been created successfully.
	for i := range goroutines {
		nqn := fmt.Sprintf("nqn.2026-01.com.bhyoo:pvc-race-%d", i)
		dir := filepath.Join(tmpdir, "nvmet", "subsystems", nqn)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("goroutine %d: subsystem dir missing after concurrent Apply: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// XC3: symlink already exists with wrong destination
// ---------------------------------------------------------------------------.

// TestException_SymlinkWrongDestination verifies that AllowInitiator returns
// a gRPC Internal error when the configfs allowed_hosts symlink already exists
// but points to the wrong target.
//
// This can arise when a previous partial ACL operation left stale state in
// configfs, or when two agents raced on the same subsystem.
//
// Setup:  export a volume, then manually pre-create a symlink at
//
//	allowed_hosts/<hostNQN> pointing to an incorrect target.
//
// Expected outcome: AllowInitiator returns codes.Internal.
func TestException_SymlinkWrongDestination(t *testing.T) {
	t.Parallel()

	const (
		volumeID = "tank/pvc-symlink-wrong"
		// nqn is derived from volumeNQN("tank/pvc-symlink-wrong")
		nqn     = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-symlink-wrong"
		hostNQN = "nqn.2023-01.io.example:host-wrong-sym"
	)

	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-symlink-wrong"}
	srv, cfgRoot := newAgentServer(t, mb)

	// Export the volume so the subsystem directory is created in configfs.
	exportVolume(t, srv, volumeID, "192.168.1.50", 4420)

	// Manually create the allowed_hosts directory and a symlink that points to
	// the WRONG target — simulating stale state from a previous partial operation.
	ahDir := filepath.Join(cfgRoot, "nvmet", "subsystems", nqn, "allowed_hosts")
	if err := os.MkdirAll(ahDir, 0o750); err != nil {
		t.Fatalf("MkdirAll allowed_hosts: %v", err)
	}
	// The wrong target — different from what AllowHost would compute.
	wrongTarget := filepath.Join(cfgRoot, "nvmet", "hosts", "nqn.some-other-unrelated-host")
	linkPath := filepath.Join(ahDir, hostNQN)
	if err := os.Symlink(wrongTarget, linkPath); err != nil {
		t.Fatalf("create wrong symlink: %v", err)
	}

	// AllowInitiator must detect the stale symlink and return an error instead
	// of silently leaving the configfs in an inconsistent state.
	_, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     volumeID,
		InitiatorId:  hostNQN,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	if err == nil {
		t.Fatal("expected error when allowed_hosts symlink points to wrong target, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
	t.Logf("AllowInitiator correctly rejected wrong symlink: %v", err)
}

// ---------------------------------------------------------------------------
// XC4: ZFS command timeout via context cancellation
// ---------------------------------------------------------------------------.

// TestException_ZFSCommandTimeout verifies that the ZFS backend respects
// context cancellation.  When a ZFS command blocks (e.g., a hung pool) and
// the caller's context deadline expires, the backend returns promptly.
//
// Setup:  ZFS backend with an executor that blocks until its context is done.
//
// Expected outcome: Backend.Create returns a non-nil error within 500 ms of
// the 50 ms deadline expiring; no goroutine leak.
func TestException_ZFSCommandTimeout(t *testing.T) {
	t.Parallel()

	// Executor blocks indefinitely until the context is canceled/expired.
	b := zfs.NewWithExecFn("tank", "", func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := b.Create(ctx, "tank/pvc-timeout-xc4", 10*1024*1024*1024, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected context error from blocking executor, got nil")
	}

	// The operation should complete promptly after context expiry, not linger.
	const maxElapsed = 500 * time.Millisecond
	if elapsed > maxElapsed {
		t.Errorf("operation took %v, want < %v (should respect context deadline)", elapsed, maxElapsed)
	}
	t.Logf("ZFS command timeout returned in %v with error: %v", elapsed, err)
}

// ---------------------------------------------------------------------------
// XC5: gRPC request deadline propagated through device polling
// ---------------------------------------------------------------------------.

// TestException_GRPCDeadlineExceeded verifies that when a request context's
// deadline expires during ExportVolume's zvol-ready polling loop, the
// operation terminates promptly — well before the internal poll timeout fires.
//
// Setup:  device never appears (checker always returns false); internal poll
//
//	timeout = 10 s; request context deadline = 80 ms.
//
// Expected outcome: ExportVolume returns codes.FailedPrecondition within
// ~500 ms (not 10 s).
func TestException_GRPCDeadlineExceeded(t *testing.T) {
	t.Parallel()

	neverPresent := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil
	})

	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-deadline-xc5"}
	srv, _ := newAgentServer(t, mb,
		agent.WithDeviceChecker(neverPresent),
		// Internal poll timeout is 10 s; the request context (80 ms) must expire first.
		agent.WithDevicePollParams(5*time.Millisecond, 10*time.Second),
	)

	// Request context with a short deadline — simulates a gRPC client timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-deadline-xc5",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.1.1", 4420),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from deadline exceeded, got nil")
	}

	// Must terminate promptly when context expires, NOT after the 10 s internal timeout.
	const maxElapsed = 500 * time.Millisecond
	if elapsed > maxElapsed {
		t.Errorf("operation took %v, want < %v (must respect request context deadline)", elapsed, maxElapsed)
	}

	// ExportVolume wraps WaitForDevice errors as FailedPrecondition.
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
	t.Logf("gRPC deadline exceeded in %v: %v", elapsed, err)
}

// ---------------------------------------------------------------------------
// XC6: configfs pseudo-file write fails mid-Apply
// ---------------------------------------------------------------------------.

// TestException_PartialConfigfsWrite verifies that NvmetTarget.Apply returns
// a descriptive error when a write to a configfs pseudo-file fails after the
// parent directory has already been created.  This simulates the kernel
// rejecting a write (e.g., invalid device path, or a permission issue on a
// specific pseudo-file).
//
// Setup:  pre-create the namespace directory; make the device_path pseudo-file
//
//	read-only (0444) so that os.WriteFile cannot overwrite it.
//
// Expected outcome: Apply returns a non-nil error; no panic.
func TestException_PartialConfigfsWrite(t *testing.T) {
	t.Parallel()

	const nqn = "nqn.2026-01.com.bhyoo:pvc-xc6-partial-write"
	tmpdir := t.TempDir()

	// Pre-create the namespace directory so that mkdirAll in createNamespace
	// succeeds (it would normally create this directory).
	nsDir := filepath.Join(tmpdir, "nvmet", "subsystems", nqn, "namespaces", "1")
	if err := os.MkdirAll(nsDir, 0o750); err != nil {
		t.Fatalf("MkdirAll namespace dir: %v", err)
	}

	// Pre-create the device_path pseudo-file and make it unwritable.
	// NvmetTarget.Apply (createNamespace) will try to overwrite it and fail.
	devicePathFile := filepath.Join(nsDir, "device_path")
	if err := os.WriteFile(devicePathFile, []byte("old-path"), 0o600); err != nil {
		t.Fatalf("WriteFile device_path: %v", err)
	}
	makeFileReadOnly(t, devicePathFile) // auto-skips as root; restores 0644 on cleanup

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-xc6",
		BindAddress:  "192.168.1.1",
		Port:         4420,
	}

	err := tgt.Apply()
	if err == nil {
		t.Fatal("expected error when device_path pseudo-file is read-only, got nil")
	}
	t.Logf("Apply correctly returned error on read-only device_path: %v", err)
}

// ---------------------------------------------------------------------------
// XC7: device TOCTOU — WaitForDevice exits on first success, no re-check
// ---------------------------------------------------------------------------.

// TestException_DeviceTOCTOU verifies that WaitForDevice exits immediately
// after the device is first detected as present, without performing a
// subsequent re-check.
//
// This is the anti-TOCTOU property: once the device appears, the polling loop
// stops — it does not re-verify the device's presence on the next tick.  A
// re-check would create a TOCTOU window where a transiently-absent device
// (e.g., a zvol being re-created) would cause a spurious "device not ready"
// error.
//
// Setup:  DeviceChecker returns (false, nil) for calls 1–2, then (true, nil)
//
//	on call 3.  After WaitForDevice returns, the checker would return
//	(false, nil) for call 4+.
//
// Expected outcome: WaitForDevice returns nil; checker is called exactly 3 times.
func TestException_DeviceTOCTOU(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32

	checker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		n := callCount.Add(1) // atomically increment and capture new value
		// Device is absent on calls 1-2, present on call 3.
		// If WaitForDevice re-checks, call 4 would return false — a TOCTOU bug.
		return n == 3, nil
	})

	err := nvmeof.WaitForDevice(
		context.Background(),
		"/dev/zvol/tank/pvc-toctou-xc7",
		5*time.Millisecond, // poll interval
		2*time.Second,      // internal timeout (much larger than needed)
		checker,
	)
	if err != nil {
		t.Fatalf("WaitForDevice unexpected error: %v", err)
	}

	// WaitForDevice must have returned immediately after call 3 returned true.
	// It must NOT call the checker a 4th time to "confirm" the device is still present.
	finalCalls := callCount.Load()
	if finalCalls != 3 {
		t.Errorf("checker called %d times, want exactly 3 (no re-check after first success)", finalCalls)
	}
}

// ---------------------------------------------------------------------------
// XC8: concurrent export + unexport of the same volume
// ---------------------------------------------------------------------------.

// TestException_ConcurrentExportUnexport verifies that simultaneously calling
// ExportVolume and UnexportVolume on the same volume does not cause a deadlock,
// panic, or corrupted configfs state.
//
// In production this can occur when a reconciliation loop re-exports a volume
// at the same time as a volume deletion is in flight.
//
// Setup:  export a volume to establish initial state; then run ExportVolume
//
//	(re-export, idempotent) and UnexportVolume concurrently.
//
// Expected outcome: both goroutines complete within 5 s (no deadlock); the
// final configfs state is internally consistent.
func TestException_ConcurrentExportUnexport(t *testing.T) {
	t.Parallel()

	const (
		volumeID = "tank/pvc-concurrent-xc8"
		// nqn is derived from volumeNQN("tank/pvc-concurrent-xc8")
		nqn = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-concurrent-xc8"
	)

	mb := &mockVolumeBackend{
		devicePathResult: "/dev/zvol/tank/pvc-concurrent-xc8",
	}
	srv, cfgRoot := newAgentServer(t, mb)

	// Establish an initial export so both concurrent operations have real configfs state.
	exportVolume(t, srv, volumeID, "192.168.1.1", 4420)

	var (
		wg          sync.WaitGroup
		exportErr   error
		unexportErr error
	)

	ready := make(chan struct{}) // closed to release both goroutines simultaneously

	// Goroutine A: re-export (idempotent).
	wg.Go(func() {
		<-ready
		_, exportErr = srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
			VolumeId:     volumeID,
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
			ExportParams: nvmeofParams("192.168.1.1", 4420),
		})
	})

	// Goroutine B: unexport.
	wg.Go(func() {
		<-ready
		_, unexportErr = srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
			VolumeId:     volumeID,
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		})
	})

	close(ready) // trigger both goroutines simultaneously

	// Both goroutines must complete within the deadline — deadlock detection.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Both operations completed without deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent export+unexport deadlocked after 5 s")
	}

	// Log outcomes for diagnostic purposes — errors are acceptable because the
	// concurrent operations race on the same configfs files.
	t.Logf("concurrent export result:   %v", exportErr)
	t.Logf("concurrent unexport result: %v", unexportErr)

	// Consistency check: if the subsystem directory exists, it must contain
	// the attr_allow_any_host attribute file (i.e., not be partially constructed).
	subsysDir := filepath.Join(cfgRoot, "nvmet", "subsystems", nqn)
	if _, statErr := os.Stat(subsysDir); statErr == nil {
		attrPath := filepath.Join(subsysDir, "attr_allow_any_host")
		if _, attrErr := os.Stat(attrPath); attrErr != nil {
			t.Errorf("subsystem dir exists but attr_allow_any_host missing: %v", attrErr)
		}
	}
	// If the subsystem dir does not exist, the unexport goroutine won; the
	// final state is fully cleaned up — also a valid outcome.
}
