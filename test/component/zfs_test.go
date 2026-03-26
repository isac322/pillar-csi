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

// Package component_test contains component-level tests for pillar-csi.
//
// Component tests treat each major component as a black box, wiring mock
// dependencies and exercising feature-level behavior including all exception
// paths.  No real ZFS processes, no real kernel configfs, no root privileges
// are required.
package component_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
)

// ---------------------------------------------------------------------------
// seqExec: sequential mock executor for ZFS backend tests.
// ---------------------------------------------------------------------------.

// execResponse captures one preset output from the mock ZFS executor.
type execResponse struct {
	out []byte
	err error
}

// ok returns a successful execResponse with the given output.
func ok(output string) execResponse {
	return execResponse{out: []byte(output)}
}

// fail returns an execResponse that simulates a failed ZFS command.
func fail(output string) execResponse {
	return execResponse{out: []byte(output), err: errors.New("exit status 1")}
}

// seqExec is a test double for the ZFS backend's internal executor interface.
//
// # Mock fidelity
//
// Approximates: the production osExecutor, which runs actual zfs(8) and
// zpool(8) child processes via os/exec.CommandContext and returns their
// combined stdout+stderr output together with the exit status.
//
// Omits / simplifies:
//   - No real child process is created; no kernel or ZFS module interaction
//     occurs.  Commands are serviced entirely in-process.
//   - Exit-code granularity: the real osExecutor returns the raw *exec.ExitError
//     whose ExitCode() reflects the exact ZFS exit status (e.g. 1 for a
//     general error, 2 for an invalid argument).  seqExec always fabricates
//     errors.New("exit status 1"), losing that distinction.
//   - Ordering contract: the real executor can service concurrent calls;
//     seqExec serializes every call with a mutex and replays responses strictly
//     in insertion order.  Tests that rely on this ordering are brittle if
//     production code ever parallelises ZFS calls.
//   - Context cancellation: the real osExecutor propagates context cancellation
//     as SIGKILL to the child process via CommandContext.  The mock forwards
//     the context to the injected closure, which must simulate cancellation
//     itself (see TestZFSBackend_ContextCancelled).
//   - Platform-specific output: real zfs(8) output varies across OpenZFS
//     versions and platforms (FreeBSD vs Linux).  The mock returns fixed strings
//     chosen to match the parsing logic in the production code.
//   - No filesystem side effects: the real executor creates / destroys block
//     device nodes, updates kernel ZFS state, and modifies pool metadata.  The
//     mock leaves all disk and kernel state entirely unchanged.
//
// seqExec replays preset responses in order for each executor call.
type seqExec struct {
	t         *testing.T
	mu        sync.Mutex
	responses []execResponse
	pos       int
}

func newSeqExec(t *testing.T, responses ...execResponse) *seqExec {
	t.Helper()
	return &seqExec{t: t, responses: responses}
}

// do returns the executor function suitable for zfs.NewWithExecFn.
func (e *seqExec) do() func(_ context.Context, name string, args ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.pos >= len(e.responses) {
			e.t.Fatalf("seqExec: unexpected call #%d: %s %v", e.pos+1, name, args)
			return nil, nil
		}
		r := e.responses[e.pos]
		e.pos++
		return r.out, r.err
	}
}

// ---------------------------------------------------------------------------
// Helper: newZFSBackend creates a zfs.Backend with a sequential mock executor.
// ---------------------------------------------------------------------------.

// zfsBackendWith creates a ZFS backend wired to the given sequential executor.
// By default it uses pool="tank" with no parentDataset.
func zfsBackendWith(t *testing.T, responses ...execResponse) *zfs.Backend {
	t.Helper()
	exec := newSeqExec(t, responses...)
	return zfs.NewWithExecFn("tank", "", exec.do())
}

// zfsBackendWithParent creates a ZFS backend with pool="tank" and
// parentDataset="k8s".
func zfsBackendWithParent(t *testing.T, responses ...execResponse) *zfs.Backend {
	t.Helper()
	exec := newSeqExec(t, responses...)
	return zfs.NewWithExecFn("tank", "k8s", exec.do())
}

// ---------------------------------------------------------------------------
// Component 2.1 — Create
// ---------------------------------------------------------------------------.

// TestZFSBackend_Create_Success validates that creating a new zvol issues the
// correct zfs commands and returns the device path and allocated size.
func TestZFSBackend_Create_Success(t *testing.T) {
	t.Parallel()
	const capacityBytes = int64(10 * 1024 * 1024 * 1024) // 10 GiB

	b := zfsBackendWith(t,
		// 1. Existence check — dataset does not exist yet.
		fail("dataset does not exist"),
		// 2. Create command succeeds.
		ok(""),
		// 3. Readback volsize.
		ok("10737418240\n"),
	)

	devicePath, allocated, err := b.Create(context.Background(), "tank/pvc-abc", capacityBytes, nil)
	if err != nil {
		t.Fatalf("Create unexpected error: %v", err)
	}
	if devicePath == "" {
		t.Error("devicePath is empty")
	}
	if !strings.HasSuffix(devicePath, "pvc-abc") {
		t.Errorf("devicePath %q does not contain volume name", devicePath)
	}
	if allocated != capacityBytes {
		t.Errorf("allocated = %d, want %d", allocated, capacityBytes)
	}
}

// TestZFSBackend_Create_Idempotent verifies that calling Create when the zvol
// already exists with the same capacity returns the existing device path
// without issuing a create command.
func TestZFSBackend_Create_Idempotent(t *testing.T) {
	t.Parallel()
	const capacityBytes = int64(10 * 1024 * 1024 * 1024) // 10 GiB

	b := zfsBackendWith(t,
		// Existence check returns the current volsize — same as requested.
		ok("10737418240\n"),
		// No create or readback calls expected.
	)

	devicePath, allocated, err := b.Create(context.Background(), "tank/pvc-abc", capacityBytes, nil)
	if err != nil {
		t.Fatalf("Create idempotent unexpected error: %v", err)
	}
	if devicePath == "" {
		t.Error("devicePath is empty on idempotent create")
	}
	if allocated != capacityBytes {
		t.Errorf("allocated = %d, want %d", allocated, capacityBytes)
	}
}

// TestZFSBackend_Create_ConflictDifferentSize verifies that when the zvol
// already exists with a different capacity, a ConflictError is returned.
func TestZFSBackend_Create_ConflictDifferentSize(t *testing.T) {
	t.Parallel()
	const existing = int64(10 * 1024 * 1024 * 1024)  // 10 GiB
	const requested = int64(20 * 1024 * 1024 * 1024) // 20 GiB

	b := zfsBackendWith(t,
		// Existence check returns a DIFFERENT size.
		ok("10737418240\n"),
	)

	_, _, err := b.Create(context.Background(), "tank/pvc-abc", requested, nil)
	if err == nil {
		t.Fatal("expected ConflictError, got nil")
	}
	var conflictErr *backend.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Errorf("error type = %T, want *backend.ConflictError", err)
	}
	if conflictErr.ExistingBytes != existing {
		t.Errorf("ExistingBytes = %d, want %d", conflictErr.ExistingBytes, existing)
	}
	if conflictErr.RequestedBytes != requested {
		t.Errorf("RequestedBytes = %d, want %d", conflictErr.RequestedBytes, requested)
	}
}

// TestZFSBackend_Create_DiskFull validates that a disk-full error from
// the create command propagates as a non-nil error.
func TestZFSBackend_Create_DiskFull(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Existence check — not found.
		fail("dataset does not exist"),
		// Create fails with ENOSPC.
		fail("cannot create 'tank/pvc-abc': out of space"),
	)

	_, _, err := b.Create(context.Background(), "tank/pvc-abc", 10*1024*1024*1024, nil)
	if err == nil {
		t.Fatal("expected disk-full error, got nil")
	}
}

// TestZFSBackend_Create_PoolOffline validates that a pool-unavailable error
// from the create command propagates correctly.
func TestZFSBackend_Create_PoolOffline(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Existence check returns pool-not-available.
		fail("pool is not available"),
	)

	_, _, err := b.Create(context.Background(), "tank/pvc-abc", 10*1024*1024*1024, nil)
	if err == nil {
		t.Fatal("expected pool-offline error, got nil")
	}
}

// TestZFSBackend_Create_WithParentDataset verifies that when a parentDataset
// is configured, the dataset path includes it.
func TestZFSBackend_Create_WithParentDataset(t *testing.T) {
	t.Parallel()
	const capacityBytes = int64(10 * 1024 * 1024 * 1024) // 10 GiB

	var capturedArgs []string
	exec := newSeqExec(t,
		// Existence check — not found.
		fail("dataset does not exist"),
		// Create succeeds.
		ok(""),
		// Readback.
		ok("10737418240\n"),
	)
	captureAndForward := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		capturedArgs = append(capturedArgs, append([]string{name}, args...)...)
		return exec.do()(ctx, name, args...)
	}
	b := zfs.NewWithExecFn("tank", "k8s", captureAndForward)

	_, _, err := b.Create(context.Background(), "tank/pvc-abc", capacityBytes, nil)
	if err != nil {
		t.Fatalf("Create unexpected error: %v", err)
	}

	// Verify the dataset path contains "k8s" in the captured commands.
	found := false
	for _, arg := range capturedArgs {
		if strings.Contains(arg, "k8s") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected dataset path to contain 'k8s', captured args: %v", capturedArgs)
	}
}

// TestZFSBackend_Create_WithProperties verifies that ZFS properties from
// params are forwarded to the create command.
func TestZFSBackend_Create_WithProperties(t *testing.T) {
	t.Parallel()

	var createArgs []string
	callCount := 0
	exec := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		callCount++
		switch callCount {
		case 1:
			// Existence check.
			return []byte("dataset does not exist"), errors.New("exit status 1")
		case 2:
			// Create command — capture args.
			createArgs = args
			return []byte(""), nil
		case 3:
			// Readback.
			return []byte("10737418240\n"), nil
		default:
			t.Fatalf("unexpected call %d", callCount)
			return nil, nil
		}
	}

	b := zfs.NewWithExecFn("tank", "", exec)
	params := &agentv1.ZfsVolumeParams{
		Properties: map[string]string{
			"compression": "lz4",
		},
	}
	_, _, err := b.Create(context.Background(), "tank/pvc-abc", 10*1024*1024*1024, params)
	if err != nil {
		t.Fatalf("Create unexpected error: %v", err)
	}

	// The create args should contain "-o compression=lz4".
	argsStr := strings.Join(createArgs, " ")
	if !strings.Contains(argsStr, "compression=lz4") {
		t.Errorf("create args %v do not include compression property", createArgs)
	}
}

// ---------------------------------------------------------------------------
// Component 2.2 — Delete
// ---------------------------------------------------------------------------.

// TestZFSBackend_Delete_Success validates normal deletion.
func TestZFSBackend_Delete_Success(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Destroy succeeds.
		ok(""),
	)

	if err := b.Delete(context.Background(), "tank/pvc-abc"); err != nil {
		t.Fatalf("Delete unexpected error: %v", err)
	}
}

// TestZFSBackend_Delete_Idempotent verifies that deleting a non-existent
// dataset is treated as success (idempotent).
func TestZFSBackend_Delete_Idempotent(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Destroy returns "dataset does not exist" — idempotent.
		fail("dataset does not exist"),
	)

	if err := b.Delete(context.Background(), "tank/pvc-abc"); err != nil {
		t.Fatalf("Delete idempotent unexpected error: %v", err)
	}
}

// TestZFSBackend_Delete_DatasetBusy validates that a busy-device error
// (e.g. zvol still exported) propagates as a non-nil error.
func TestZFSBackend_Delete_DatasetBusy(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Destroy fails: device busy.
		fail("dataset is busy"),
	)

	err := b.Delete(context.Background(), "tank/pvc-abc")
	if err == nil {
		t.Fatal("expected busy error, got nil")
	}
	if !strings.Contains(err.Error(), "dataset is busy") {
		t.Errorf("error %q does not mention 'dataset is busy'", err.Error())
	}
}

// TestZFSBackend_Delete_ZFSError validates that an unexpected ZFS error
// propagates as a non-nil error.
func TestZFSBackend_Delete_ZFSError(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Destroy fails with generic error.
		fail("internal error"),
	)

	if err := b.Delete(context.Background(), "tank/pvc-abc"); err == nil {
		t.Fatal("expected error from failed destroy, got nil")
	}
}

// ---------------------------------------------------------------------------
// Component 2.3 — Expand
// ---------------------------------------------------------------------------.

// TestZFSBackend_Expand_Success validates normal expansion.
func TestZFSBackend_Expand_Success(t *testing.T) {
	t.Parallel()
	const newSize = int64(20 * 1024 * 1024 * 1024) // 20 GiB

	b := zfsBackendWith(t,
		// Set volsize succeeds.
		ok(""),
		// Readback returns new size.
		ok("21474836480\n"),
	)

	allocated, err := b.Expand(context.Background(), "tank/pvc-abc", newSize)
	if err != nil {
		t.Fatalf("Expand unexpected error: %v", err)
	}
	if allocated != newSize {
		t.Errorf("allocated = %d, want %d", allocated, newSize)
	}
}

// TestZFSBackend_Expand_ShrinkAttempt validates that a ZFS-level rejection
// of a shrink attempt propagates as a non-nil error.
func TestZFSBackend_Expand_ShrinkAttempt(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// ZFS rejects shrink.
		fail("volsize cannot be decreased"),
	)

	_, err := b.Expand(context.Background(), "tank/pvc-abc", 5*1024*1024*1024)
	if err == nil {
		t.Fatal("expected shrink-rejected error, got nil")
	}
}

// TestZFSBackend_Expand_NotFound validates that expanding a non-existent zvol
// returns a non-nil error.
func TestZFSBackend_Expand_NotFound(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Set volsize fails: dataset not found.
		fail("dataset does not exist"),
	)

	_, err := b.Expand(context.Background(), "tank/pvc-abc", 20*1024*1024*1024)
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
}

// TestZFSBackend_Expand_ZFSError validates that a generic ZFS error on
// set volsize propagates as a non-nil error.
func TestZFSBackend_Expand_ZFSError(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		fail("unexpected internal error"),
	)

	_, err := b.Expand(context.Background(), "tank/pvc-abc", 20*1024*1024*1024)
	if err == nil {
		t.Fatal("expected error from failed expand, got nil")
	}
}

// ---------------------------------------------------------------------------
// Component 2.4 — Capacity
// ---------------------------------------------------------------------------.

// TestZFSBackend_Capacity_Success validates normal capacity parsing.
func TestZFSBackend_Capacity_Success(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// zpool list returns "totalBytes\tfreeBytes".
		ok("107374182400\t64424509440\n"),
	)

	total, avail, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity unexpected error: %v", err)
	}
	if total != 107374182400 {
		t.Errorf("total = %d, want 107374182400", total)
	}
	if avail != 64424509440 {
		t.Errorf("avail = %d, want 64424509440", avail)
	}
}

// TestZFSBackend_Capacity_PoolOffline validates that a pool-unavailable error
// propagates as a non-nil error.
func TestZFSBackend_Capacity_PoolOffline(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		fail("pool unavailable"),
	)

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("expected pool-offline error, got nil")
	}
}

// TestZFSBackend_Capacity_ParseError validates that malformed zpool output
// returns an error without panicking.
func TestZFSBackend_Capacity_ParseError(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Malformed output — only one field, not two.
		ok("not-a-number"),
	)

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestZFSBackend_Capacity_ParseErrorNumeric validates that non-numeric values
// in the size field return an error.
func TestZFSBackend_Capacity_ParseErrorNumeric(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Two fields but the first is non-numeric.
		ok("DEGRADED\t64424509440"),
	)

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("expected parse error for non-numeric size, got nil")
	}
}

// ---------------------------------------------------------------------------
// Component 2.5 — ListVolumes
// ---------------------------------------------------------------------------.

// TestZFSBackend_ListVolumes_Success validates that three volumes are parsed
// correctly from the zfs list output.
func TestZFSBackend_ListVolumes_Success(t *testing.T) {
	t.Parallel()

	output := "tank/pvc-abc\t10737418240\n" +
		"tank/pvc-def\t21474836480\n" +
		"tank/pvc-ghi\t5368709120\n"

	b := zfsBackendWith(t, ok(output))

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes unexpected error: %v", err)
	}
	if len(vols) != 3 {
		t.Fatalf("len(vols) = %d, want 3", len(vols))
	}

	// Verify each volume has the correct ID and size.
	expected := []struct {
		id    string
		bytes int64
	}{
		{"tank/pvc-abc", 10737418240},
		{"tank/pvc-def", 21474836480},
		{"tank/pvc-ghi", 5368709120},
	}
	for i, e := range expected {
		if vols[i].GetVolumeId() != e.id {
			t.Errorf("vols[%d].VolumeId = %q, want %q", i, vols[i].GetVolumeId(), e.id)
		}
		if vols[i].GetCapacityBytes() != e.bytes {
			t.Errorf("vols[%d].CapacityBytes = %d, want %d", i, vols[i].GetCapacityBytes(), e.bytes)
		}
		if vols[i].GetDevicePath() == "" {
			t.Errorf("vols[%d].DevicePath is empty", i)
		}
	}
}

// TestZFSBackend_ListVolumes_Empty validates that an empty pool returns an
// empty (non-nil) slice without error.
func TestZFSBackend_ListVolumes_Empty(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Empty output (no volumes).
		ok(""),
	)

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes unexpected error: %v", err)
	}
	if vols == nil {
		t.Error("expected non-nil empty slice, got nil")
	}
	if len(vols) != 0 {
		t.Errorf("len(vols) = %d, want 0", len(vols))
	}
}

// TestZFSBackend_ListVolumes_ManyVolumes validates that a large number of
// volumes are parsed correctly.
func TestZFSBackend_ListVolumes_ManyVolumes(t *testing.T) {
	t.Parallel()
	const count = 100

	var sb strings.Builder
	for i := range count {
		fmt.Fprintf(&sb, "tank/pvc-%03d\t%d\n", i, int64(i+1)*1024*1024*1024)
	}

	b := zfsBackendWith(t, ok(sb.String()))

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes unexpected error: %v", err)
	}
	if len(vols) != count {
		t.Errorf("len(vols) = %d, want %d", len(vols), count)
	}
}

// TestZFSBackend_ListVolumes_ParentDatasetMissing validates that a
// "dataset does not exist" error for the parent dataset is treated as an empty
// list (not an error), matching the production idempotent behavior.
func TestZFSBackend_ListVolumes_ParentDatasetMissing(t *testing.T) {
	t.Parallel()

	b := zfsBackendWithParent(t,
		// zfs list returns "dataset does not exist" — parent not yet created.
		fail("dataset does not exist"),
	)

	vols, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes expected empty list, got error: %v", err)
	}
	if len(vols) != 0 {
		t.Errorf("len(vols) = %d, want 0 for missing parent dataset", len(vols))
	}
}

// TestZFSBackend_ListVolumes_ParseError validates that a malformed zfs list
// output line returns an error without panicking.
func TestZFSBackend_ListVolumes_ParseError(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// Garbled output — missing tab separator.
		ok("tank/pvc-abc NOT-VALID-NO-TAB\n"),
	)

	_, err := b.ListVolumes(context.Background())
	if err == nil {
		t.Fatal("expected parse error for malformed output, got nil")
	}
}

// ---------------------------------------------------------------------------
// Component 2.6 — DevicePath (no executor needed)
// ---------------------------------------------------------------------------.

// TestZFSBackend_DevicePath_Simple validates device path computation for a
// backend without a parent dataset.
func TestZFSBackend_DevicePath_Simple(t *testing.T) {
	t.Parallel()

	b := zfs.NewWithExecFn("tank", "", func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		t.Fatal("DevicePath should not invoke the executor")
		return nil, nil
	})

	got := b.DevicePath("tank/pvc-abc")
	if !strings.Contains(got, "tank") {
		t.Errorf("DevicePath %q does not contain pool name", got)
	}
	if !strings.Contains(got, "pvc-abc") {
		t.Errorf("DevicePath %q does not contain volume name", got)
	}
}

// TestZFSBackend_DevicePath_WithParentDataset validates device path computation
// for a backend with a parent dataset.
func TestZFSBackend_DevicePath_WithParentDataset(t *testing.T) {
	t.Parallel()

	b := zfs.NewWithExecFn("tank", "k8s", func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		t.Fatal("DevicePath should not invoke the executor")
		return nil, nil
	})

	got := b.DevicePath("tank/pvc-abc")
	if !strings.Contains(got, "k8s") {
		t.Errorf("DevicePath %q does not contain parent dataset", got)
	}
	if !strings.Contains(got, "pvc-abc") {
		t.Errorf("DevicePath %q does not contain volume name", got)
	}
}

// ---------------------------------------------------------------------------
// Component 2.7 — Context cancellation
// ---------------------------------------------------------------------------.

// TestZFSBackend_ContextCancelled verifies that context cancellation is
// propagated to the executor and the operation returns an appropriate error.
func TestZFSBackend_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	blocked := make(chan struct{})
	b := zfs.NewWithExecFn("tank", "", func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		close(blocked)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	// The executor will see a canceled context and return ctx.Err().
	_, _, err := b.Create(ctx, "tank/pvc-abc", 10*1024*1024*1024, nil)
	<-blocked // ensure the goroutine started
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
}
