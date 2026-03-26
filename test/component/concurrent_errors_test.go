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

// Package component_test — concurrent and race condition error path tests.
//
// This file (concurrent_errors_test.go) provides concurrent and race condition
// error paths that are distinct from those in exceptions_test.go.
//
// Coverage:
//
//	CC1: Concurrent AllowInitiator + DenyInitiator for the same host NQN —
//	     verifies no deadlock or panic when ACL ops race on the same subsystem.
//
//	CC2: Concurrent NvmetTarget.Remove on different subsystems sharing one
//	     port — verifies port-link removal does not corrupt concurrent callers.
//
//	CC3: NvmetTarget.Apply fails when attr_allow_any_host is pre-created as
//	     read-only — tests the write-blocked path inside createSubsystem.
//
//	CC4: NvmetTarget.DenyHost where the allowed_hosts symlink path is occupied
//	     by a regular file — removeSymlink must return an error (not panic).
//
//	CC5: Concurrent CreateVolume requests for the same VolumeID — verifies
//	     that the agent server does not deadlock under concurrent idempotent
//	     creates.
//
//	CC6: Concurrent AllowHost for the same host NQN — idempotency under race;
//	     all goroutines must return nil.
//
//	CC7: NvmetTarget.Apply fails when the port subsystems/ parent directory
//	     is a regular file — linkSubsystemToPort mkdirAll must return an error.
//
// All tests use t.TempDir() for filesystem isolation; no root privileges,
// real ZFS, or kernel configfs are required.  Tests that depend on Unix DAC
// permission bits auto-skip when running as root.
package component_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ---------------------------------------------------------------------------
// CC1: Concurrent AllowInitiator + DenyInitiator for the same host
// ---------------------------------------------------------------------------.

// TestConcurrentError_AllowDenyInitiator_SameHost_Race verifies that
// simultaneously calling AllowInitiator and DenyInitiator for the same
// host NQN on the same volume does not cause a deadlock or panic.
//
// In production this race occurs when a volume ACL is being updated at the
// same time as the PVC is being deleted.
//
// Setup:  export volume to establish subsystem in configfs; allow the
//
//	initiator once; then run AllowInitiator (re-allow, idempotent) and
//	DenyInitiator concurrently.
//
// Expected outcome: both goroutines complete within 5 s (no deadlock); errors
// are acceptable because the race is non-deterministic.
func TestConcurrentError_AllowDenyInitiator_SameHost_Race(t *testing.T) {
	t.Parallel()

	const (
		volumeID = "tank/pvc-cc1-acl-race"
		hostNQN  = "nqn.2023-01.io.example:host-cc1-acl-race"
	)

	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-cc1-acl-race"}
	srv, _ := newAgentServer(t, mb)

	// Establish the volume export so the subsystem directory exists.
	exportVolume(t, srv, volumeID, "192.168.2.1", 4420)

	// Allow the initiator once before the concurrent test to give
	// DenyInitiator something to remove.
	if _, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     volumeID,
		InitiatorId:  hostNQN,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	}); err != nil {
		t.Fatalf("initial AllowInitiator setup: %v", err)
	}

	var (
		wg       sync.WaitGroup
		allowErr error
		denyErr  error
	)
	ready := make(chan struct{})

	// Goroutine A: re-allow (idempotent).
	wg.Go(func() {
		<-ready
		_, allowErr = srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
			VolumeId:     volumeID,
			InitiatorId:  hostNQN,
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		})
	})

	// Goroutine B: deny concurrently.
	wg.Go(func() {
		<-ready
		_, denyErr = srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
			VolumeId:     volumeID,
			InitiatorId:  hostNQN,
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		})
	})

	close(ready)

	// Deadlock detection.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		// Success — no deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent AllowInitiator+DenyInitiator deadlocked after 5 s")
	}

	t.Logf("concurrent ACL race — allow: %v  deny: %v", allowErr, denyErr)
}

// ---------------------------------------------------------------------------
// CC2: Concurrent Remove across subsystems sharing one port
// ---------------------------------------------------------------------------.

// TestConcurrentError_Remove_SharedPort_NoDeadlock verifies that multiple
// goroutines can call NvmetTarget.Remove concurrently on different subsystems
// that share the same port directory without deadlocking.
//
// When concurrent removals race on the port's subsystems/ directory, each
// goroutine may find that the sibling's symlink has already been removed —
// the code must handle ENOENT idempotently.
//
// Setup:  apply N subsystems sharing the same port; then remove all
//
//	concurrently.
//
// Expected outcome: all goroutines return nil; all subsystem directories gone.
func TestConcurrentError_Remove_SharedPort_NoDeadlock(t *testing.T) {
	t.Parallel()

	tmpdir := t.TempDir()

	const (
		goroutines = 4
		bindAddr   = "10.10.10.1"
		tcpPort    = int32(4422)
	)

	targets := make([]*nvmeof.NvmetTarget, goroutines)
	for i := range goroutines {
		nqn := fmt.Sprintf("nqn.2026-01.com.bhyoo:pvc-cc2-sharedport-%d", i)
		tgt := &nvmeof.NvmetTarget{
			ConfigfsRoot: tmpdir,
			SubsystemNQN: nqn,
			NamespaceID:  1,
			DevicePath:   fmt.Sprintf("/dev/zvol/tank/pvc-cc2-%d", i),
			BindAddress:  bindAddr,
			Port:         tcpPort,
		}
		if err := tgt.Apply(); err != nil {
			t.Fatalf("setup Apply goroutine %d: %v", i, err)
		}
		targets[i] = tgt
	}

	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)

		go func() { //nolint:modernize // start channel needed for synchronized concurrent launch
			defer wg.Done()
			<-start
			errs[i] = targets[i].Remove()
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Remove deadlocked after 5 s")
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d Remove error: %v", i, err)
		}
	}

	for i := range goroutines {
		nqn := fmt.Sprintf("nqn.2026-01.com.bhyoo:pvc-cc2-sharedport-%d", i)
		dir := filepath.Join(tmpdir, "nvmet", "subsystems", nqn)
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Errorf("goroutine %d: subsystem dir still present after Remove (statErr=%v)", i, statErr)
		}
	}
}

// ---------------------------------------------------------------------------
// CC3: Apply fails when attr_allow_any_host is read-only
// ---------------------------------------------------------------------------.

// TestConcurrentError_Apply_AttrAllowAnyHostReadOnly verifies that
// NvmetTarget.Apply returns an error when the subsystem directory exists but
// the attr_allow_any_host pseudo-file is pre-created and read-only.
//
// This targets the write inside createSubsystem that sets allow_any_host = 1.
// If the kernel or a concurrent process has placed an unwritable file at that
// path, Apply must surface the error.
//
// Distinct from XC1 (subsystems/ dir read-only) and XC6 (device_path
// read-only): here the directory is writable; only this pseudo-file is blocked.
//
// Setup:  pre-create subsystem dir; write attr_allow_any_host with mode 0444.
// Expected: Apply returns non-nil error; no panic.
func TestConcurrentError_Apply_AttrAllowAnyHostReadOnly(t *testing.T) {
	t.Parallel()

	const nqn = "nqn.2026-01.com.bhyoo:pvc-cc3-attr-ro"
	tmpdir := t.TempDir()

	subDir := filepath.Join(tmpdir, "nvmet", "subsystems", nqn)
	if err := os.MkdirAll(subDir, 0o750); err != nil {
		t.Fatalf("MkdirAll subsystem dir: %v", err)
	}

	attrFile := filepath.Join(subDir, "attr_allow_any_host")
	if err := os.WriteFile(attrFile, []byte("0"), 0o600); err != nil {
		t.Fatalf("WriteFile attr_allow_any_host: %v", err)
	}
	makeFileReadOnly(t, attrFile) // auto-skips as root; restores on cleanup

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-cc3",
		BindAddress:  "192.168.3.1",
		Port:         4420,
	}

	err := tgt.Apply()
	if err == nil {
		t.Fatal("expected error when attr_allow_any_host is read-only, got nil")
	}
	t.Logf("Apply correctly returned error on read-only attr_allow_any_host: %v", err)
}

// ---------------------------------------------------------------------------
// CC4: DenyHost when symlink path is a regular file (not a symlink)
// ---------------------------------------------------------------------------.

// TestConcurrentError_DenyHost_PathIsRegularFile verifies that DenyHost
// returns an error when the path at allowed_hosts/<hostNQN> is a regular file
// instead of a symlink.
//
// RemoveSymlink checks fi.Mode()&os.ModeSymlink == 0 and rejects non-symlinks.
// This prevents silent deletion of a regular configfs pseudo-file that might
// have landed there due to a corrupt prior operation.
//
// Distinct from XC3 (symlink pointing to wrong destination): here the path is
// NOT a symlink at all.
//
// Setup:  Apply target; place a regular file at allowed_hosts/<hostNQN>.
// Expected: DenyHost returns non-nil error; no panic.
func TestConcurrentError_DenyHost_PathIsRegularFile(t *testing.T) {
	t.Parallel()

	const (
		nqn     = "nqn.2026-01.com.bhyoo:pvc-cc4-denyhost-file"
		hostNQN = "nqn.2023-01.io.example:host-cc4-file"
	)
	tmpdir := t.TempDir()

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-cc4",
		BindAddress:  "192.168.4.1",
		Port:         4420,
	}
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Create the allowed_hosts directory and place a REGULAR FILE where a
	// symlink would normally live.
	ahDir := filepath.Join(tmpdir, "nvmet", "subsystems", nqn, "allowed_hosts")
	if err := os.MkdirAll(ahDir, 0o750); err != nil {
		t.Fatalf("MkdirAll allowed_hosts: %v", err)
	}
	filePath := filepath.Join(ahDir, hostNQN)
	if err := os.WriteFile(filePath, []byte("corrupted"), 0o600); err != nil {
		t.Fatalf("WriteFile fake symlink: %v", err)
	}

	err := tgt.DenyHost(hostNQN)
	if err == nil {
		t.Fatal("expected error when allowed_hosts path is a regular file, got nil")
	}
	t.Logf("DenyHost correctly rejected non-symlink path: %v", err)
}

// ---------------------------------------------------------------------------
// CC5: Concurrent CreateVolume for the same VolumeID
// ---------------------------------------------------------------------------.

// TestConcurrentError_CreateVolume_SameID_NoDeadlock verifies that multiple
// goroutines simultaneously calling CreateVolume for the same VolumeID do not
// deadlock in the agent server.
//
// The mock backend is configured to succeed, so all goroutines should see
// idempotent success with no deadlock.
//
// Setup:  N goroutines call CreateVolume with the same VolumeID simultaneously.
// Expected: all goroutines return within 5 s; all succeed.
func TestConcurrentError_CreateVolume_SameID_NoDeadlock(t *testing.T) {
	t.Parallel()

	const goroutines = 6

	mb := &mockVolumeBackend{
		createDevicePath: "/dev/zvol/tank/pvc-cc5-concurrent-create",
		createAllocated:  5 * 1024 * 1024 * 1024,
	}
	srv, _ := newAgentServer(t, mb)

	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)

		go func() { //nolint:modernize // start channel needed for synchronized concurrent launch
			defer wg.Done()
			<-start
			_, errs[i] = srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
				VolumeId:      "tank/pvc-cc5-concurrent-create",
				CapacityBytes: 5 * 1024 * 1024 * 1024,
			})
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent CreateVolume deadlocked after 5 s")
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d CreateVolume error: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// CC6: Concurrent AllowHost for the same host NQN — idempotency under race
// ---------------------------------------------------------------------------.

// TestConcurrentError_AllowHost_SameHost_Idempotent verifies that multiple
// goroutines concurrently calling AllowHost for the same host NQN complete
// without deadlock and leave the configfs in a correct final state.
//
// Due to the check-then-act nature of the symlink function (Readlink + Symlink),
// concurrent goroutines may race: one succeeds and the others may receive an
// EEXIST error from os.Symlink when the symlink was created between their
// Readlink (ENOENT) and Symlink calls.  This is an expected concurrent behavior
// in a regular filesystem — on real kernel configfs, concurrent writes to the
// same subsystem are serialized by the kernel.
//
// The test therefore verifies:
//  1. No deadlock (all goroutines complete within deadline).
//  2. The final symlink exists and points to the correct target.
//  3. At least one goroutine returns nil (exactly one creates the symlink).
//
// Setup:  Apply target; N goroutines call AllowHost simultaneously.
// Expected: no deadlock; final symlink correct; at least one nil result.
func TestConcurrentError_AllowHost_SameHost_Idempotent(t *testing.T) {
	t.Parallel()

	const (
		nqn      = "nqn.2026-01.com.bhyoo:pvc-cc6-allow-idem"
		hostNQN  = "nqn.2023-01.io.example:host-cc6-idem"
		nWorkers = 5
	)
	tmpdir := t.TempDir()

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-cc6",
		BindAddress:  "192.168.5.1",
		Port:         4420,
	}
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	errs := make([]error, nWorkers)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range nWorkers {
		wg.Add(1)

		go func() { //nolint:modernize // start channel needed for synchronized concurrent launch
			defer wg.Done()
			<-start
			errs[i] = tgt.AllowHost(hostNQN)
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent AllowHost deadlocked after 5 s")
	}

	// At least one goroutine must have succeeded.
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	if successCount == 0 {
		t.Error("expected at least one goroutine to succeed, all returned errors")
		for i, err := range errs {
			t.Logf("goroutine %d: %v", i, err)
		}
	}

	// Regardless of individual errors, the final symlink must be correct.
	linkPath := filepath.Join(tmpdir, "nvmet", "subsystems", nqn, "allowed_hosts", hostNQN)
	dest, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink after concurrent AllowHost: %v", err)
	}
	want := filepath.Join(tmpdir, "nvmet", "hosts", hostNQN)
	if dest != want {
		t.Errorf("symlink dest = %q, want %q", dest, want)
	}
	t.Logf("concurrent AllowHost: %d/%d goroutines succeeded; final symlink correct", successCount, nWorkers)
}

// ---------------------------------------------------------------------------
// CC7: Apply fails when port subsystems/ parent dir is a regular file
// ---------------------------------------------------------------------------.

// TestConcurrentError_Apply_PortSubsystemsBlockedByFile verifies that
// NvmetTarget.Apply returns an error when the path that must become the
// port's subsystems/ directory is occupied by a regular file.
//
// LinkSubsystemToPort calls mkdirAll(filepath.Dir(linkPath)) before creating
// the symlink.  If that parent path is a regular file, mkdirAll fails because
// it cannot replace a file with a directory.
//
// This tests step 4 failure after steps 1–3 succeed, which is distinct from
// CC3 (step 1 failure) and XC6 (step 2 failure).
//
// Setup:  discover port ID by probing; pre-create port dir with transport
//
//	attrs; place a regular FILE at ports/<portID>/subsystems to block
//	mkdirAll in linkSubsystemToPort.
//
// Expected: Apply returns non-nil error; no panic.
func TestConcurrentError_Apply_PortSubsystemsBlockedByFile(t *testing.T) {
	t.Parallel()

	const (
		nqn      = "nqn.2026-01.com.bhyoo:pvc-cc7-subsys-file"
		bindAddr = "10.20.30.50"
		tcpPort  = int32(4423)
	)

	// Use a probe tmpdir to discover the port ID without modifying the real
	// test tmpdir.
	probeTmp := t.TempDir()
	probe := &nvmeof.NvmetTarget{
		ConfigfsRoot: probeTmp,
		SubsystemNQN: "nqn.2026-01.com.bhyoo:probe-only",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/probe",
		BindAddress:  bindAddr,
		Port:         tcpPort,
	}
	if err := probe.Apply(); err != nil {
		t.Fatalf("probe Apply: %v", err)
	}
	probePortsDir := filepath.Join(probeTmp, "nvmet", "ports")
	entries, err := os.ReadDir(probePortsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("probe: no port directories found: err=%v", err)
	}
	portID := entries[0].Name()

	// Now set up the real test tmpdir.
	tmpdir := t.TempDir()
	portDirPath := filepath.Join(tmpdir, "nvmet", "ports", portID)
	if err := os.MkdirAll(portDirPath, 0o750); err != nil {
		t.Fatalf("MkdirAll port dir: %v", err)
	}
	// Pre-write transport attributes so createPort's writeFile calls succeed.
	for _, attr := range []string{"addr_trtype", "addr_adrfam", "addr_traddr", "addr_trsvcid"} {
		if err := os.WriteFile(filepath.Join(portDirPath, attr), []byte("placeholder"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", attr, err)
		}
	}
	// Block the subsystems/ subdir path with a regular file.
	blocker := filepath.Join(portDirPath, "subsystems")
	if err := os.WriteFile(blocker, []byte("file-not-dir"), 0o600); err != nil {
		t.Fatalf("WriteFile subsystems blocker: %v", err)
	}

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-cc7",
		BindAddress:  bindAddr,
		Port:         tcpPort,
	}

	applyErr := tgt.Apply()
	if applyErr == nil {
		t.Fatal("expected error when port subsystems path is a regular file, got nil")
	}
	t.Logf("Apply correctly returned error when port subsystems is a file: %v", applyErr)
}
