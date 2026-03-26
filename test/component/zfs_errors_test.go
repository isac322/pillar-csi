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

// Component error/exception path tests for the ZFS Backend (internal/agent/backend/zfs/).
//
// This file is the dedicated home for ZFS error-path coverage.  Each test
// targets a specific failure scenario that the ZFS backend must handle
// gracefully:
//
//   - Disk-full errors on creation and expansion.
//   - Device-busy errors preventing deletion.
//   - Invalid ZFS property names rejected by zfs(8).
//   - Pool-offline / pool-unavailable errors.
//   - Post-operation readback failures (create/expand succeed, but the
//     subsequent volsize query fails).
//   - Context cancellation / deadline propagation.
//
// Black-box setup: each test wires a zfs.Backend to a seqExec sequential mock
// executor.  No real ZFS processes or root privileges are required.
package component_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
)

// ---------------------------------------------------------------------------
// Disk-full error paths
// ---------------------------------------------------------------------------.

// TestZFSBackend_Error_DiskFull_Expand validates that an ENOSPC error returned
// by 'zfs set volsize' during zvol expansion is propagated as a non-nil error.
//
// This is distinct from the create-time disk-full test: the pool ran out of
// space after the initial allocation and the resize request cannot be fulfilled.
//
//	Setup:   seqExec: "zfs set volsize" returns "out of space" error
//	Expect:  Expand returns non-nil error containing the "out of space" message
func TestZFSBackend_Error_DiskFull_Expand(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// 'zfs set volsize=...' fails with ENOSPC message from zfs(8).
		fail("cannot set property for 'tank/pvc-expand-full': out of space"),
	)

	_, err := b.Expand(context.Background(), "tank/pvc-expand-full", 20*1024*1024*1024)
	if err == nil {
		t.Fatal("expected disk-full error on expand, got nil")
	}
	if !strings.Contains(err.Error(), "out of space") {
		t.Errorf("error %q does not mention 'out of space'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Device-busy error path
// ---------------------------------------------------------------------------.

// TestZFSBackend_Error_DeviceBusy_ExpandFails validates that a "device busy"
// error when expanding a zvol that is still open by a kernel consumer (e.g.
// a connected NVMe-oF initiator) propagates correctly.
//
// 'zfs set volsize' on a busy zvol may emit "cannot resize: device busy".
//
//	Setup:   seqExec: "zfs set volsize" returns "device busy" error
//	Expect:  Expand returns non-nil error; error message includes "device busy"
func TestZFSBackend_Error_DeviceBusy_ExpandFails(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		fail("cannot set property for 'tank/pvc-busy': device busy"),
	)

	_, err := b.Expand(context.Background(), "tank/pvc-busy", 20*1024*1024*1024)
	if err == nil {
		t.Fatal("expected device-busy error on expand, got nil")
	}
	if !strings.Contains(err.Error(), "device busy") {
		t.Errorf("error %q does not mention 'device busy'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Invalid ZFS properties
// ---------------------------------------------------------------------------.

// TestZFSBackend_Error_InvalidProperties validates that a creation request
// with an unrecognized ZFS property name propagates the rejection from zfs(8).
//
// The backend forwards properties verbatim to 'zfs create -o <key>=<value>'.
// When zfs(8) rejects the property, the error must reach the caller so that
// the agent can surface an InvalidArgument or Internal gRPC status.
//
//	Setup:   seqExec: existence check returns "not found"; create returns
//	         "bad property" error from zfs(8)
//	Expect:  Create returns non-nil error; error message includes "bad property"
func TestZFSBackend_Error_InvalidProperties(t *testing.T) {
	t.Parallel()

	callCount := 0
	exec := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		callCount++
		switch callCount {
		case 1:
			// Existence check: dataset not found yet.
			return []byte("dataset does not exist"), errors.New("exit status 1")
		case 2:
			// 'zfs create -V ... -o bad-property=value tank/pvc-bad-prop' fails.
			return []byte("bad property 'bad-property': invalid property"), errors.New("exit status 1")
		default:
			t.Fatalf("unexpected executor call #%d with args %v", callCount, args)
			return nil, nil
		}
	}

	b := zfs.NewWithExecFn("tank", "", exec)
	params := &agentv1.ZfsVolumeParams{
		Properties: map[string]string{
			"bad-property": "invalid-value",
		},
	}

	_, _, err := b.Create(context.Background(), "tank/pvc-bad-prop", 10*1024*1024*1024, params)
	if err == nil {
		t.Fatal("expected error for invalid ZFS property, got nil")
	}
	if !strings.Contains(err.Error(), "bad property") {
		t.Errorf("error %q does not mention 'bad property'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Pool-offline error paths
// ---------------------------------------------------------------------------.

// TestZFSBackend_Error_PoolOffline_ListVolumes validates that a pool-unavailable
// error from 'zfs list' is returned as an error (not silently treated as an empty list).
//
// This is distinct from "dataset does not exist": a missing dataset is an
// empty list, but a pool-offline error must propagate so that the caller can
// distinguish "no volumes provisioned" from "the pool is down".
//
//	Setup:   seqExec: "zfs list" returns "pool is not available" error
//	Expect:  ListVolumes returns non-nil error
func TestZFSBackend_Error_PoolOffline_ListVolumes(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// zfs list returns a pool-unavailable error, not a missing-dataset error.
		fail("cannot open 'tank': pool is not available"),
	)

	_, err := b.ListVolumes(context.Background())
	if err == nil {
		t.Fatal("expected pool-offline error from ListVolumes, got nil")
	}
}

// TestZFSBackend_Error_PoolFaulted_Capacity validates that an I/O error on
// a faulted pool (e.g. a failed drive causing the pool to enter UNAVAIL state)
// propagates from Capacity as a non-nil error.
//
//	Setup:   seqExec: "zpool list" returns an I/O error
//	Expect:  Capacity returns non-nil error
func TestZFSBackend_Error_PoolFaulted_Capacity(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		fail("cannot open 'tank': I/O error"),
	)

	_, _, err := b.Capacity(context.Background())
	if err == nil {
		t.Fatal("expected I/O error from Capacity on faulted pool, got nil")
	}
}

// ---------------------------------------------------------------------------
// Post-operation readback failures
// ---------------------------------------------------------------------------.

// TestZFSBackend_Error_CreateReadback_DatasetGone validates that a failure of
// the post-create volsize readback propagates as an error.
//
// Scenario: 'zfs create -V' succeeds, but the subsequent 'zfs get volsize'
// fails — e.g. because another process destroyed the dataset between creation
// and the readback call.  The caller needs the actual allocated bytes, so this
// is a hard error.
//
//	Setup:   seqExec: existence check → not found; create → success; readback → fail
//	Expect:  Create returns non-nil error
func TestZFSBackend_Error_CreateReadback_DatasetGone(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// 1. Existence check: dataset not found.
		fail("dataset does not exist"),
		// 2. Create: succeeds.
		ok(""),
		// 3. Post-create readback: dataset was destroyed between create and readback.
		fail("dataset does not exist"),
	)

	_, _, err := b.Create(context.Background(), "tank/pvc-readback-fail", 10*1024*1024*1024, nil)
	if err == nil {
		t.Fatal("expected error when volsize readback fails after create, got nil")
	}
}

// TestZFSBackend_Error_ExpandReadback_PoolFailed validates that a failure of
// the post-expand volsize readback propagates as an error.
//
// Scenario: 'zfs set volsize=...' succeeds, but the pool fails before the
// subsequent 'zfs get volsize' call can complete.
//
//	Setup:   seqExec: set volsize → success; readback → pool-failure error
//	Expect:  Expand returns non-nil error
func TestZFSBackend_Error_ExpandReadback_PoolFailed(t *testing.T) {
	t.Parallel()

	b := zfsBackendWith(t,
		// 1. Set volsize: succeeds.
		ok(""),
		// 2. Post-expand readback: pool entered unavailable state.
		fail("cannot open 'tank/pvc-expand-rb-fail': pool is not available"),
	)

	_, err := b.Expand(context.Background(), "tank/pvc-expand-rb-fail", 20*1024*1024*1024)
	if err == nil {
		t.Fatal("expected error when volsize readback fails after expand, got nil")
	}
}

// ---------------------------------------------------------------------------
// Context cancellation / deadline propagation
// ---------------------------------------------------------------------------.

// TestZFSBackend_Error_ContextCancelled_Delete validates that Delete returns
// promptly when the caller's context is canceled before the ZFS command completes.
//
// A blocked 'zfs destroy' (e.g. due to a hung pool I/O path) must not cause
// the agent to hang indefinitely — it must honor context cancellation.
//
//	Setup:   executor blocks on ctx.Done(); context pre-canceled
//	Expect:  Delete returns non-nil error wrapping context.Canceled
func TestZFSBackend_Error_ContextCancelled_Delete(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately, before the command starts

	b := zfs.NewWithExecFn("tank", "", func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	err := b.Delete(ctx, "tank/pvc-del-ctx")
	if err == nil {
		t.Fatal("expected context error from blocked delete, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
}

// TestZFSBackend_Error_ContextCancelled_Expand validates that Expand returns
// promptly when the caller's context is canceled before 'zfs set volsize' completes.
//
//	Setup:   executor blocks on ctx.Done(); context pre-canceled
//	Expect:  Expand returns non-nil error wrapping context.Canceled
func TestZFSBackend_Error_ContextCancelled_Expand(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	b := zfs.NewWithExecFn("tank", "", func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	_, err := b.Expand(ctx, "tank/pvc-exp-ctx", 20*1024*1024*1024)
	if err == nil {
		t.Fatal("expected context error from blocked expand, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
}

// TestZFSBackend_Error_ContextCancelled_ListVolumes validates that ListVolumes
// returns promptly when the caller's context is canceled before
// 'zfs list' completes.
//
// A hung pool I/O path can cause 'zfs list' to block indefinitely; the
// backend must honor context cancellation so that the agent is not stuck.
//
//	Setup:   executor blocks on ctx.Done(); context pre-canceled
//	Expect:  ListVolumes returns non-nil error wrapping context.Canceled
//
// See TESTCASES.md §2.7, row 38.
func TestZFSBackend_Error_ContextCancelled_ListVolumes(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before the command starts

	b := zfs.NewWithExecFn("tank", "", func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	_, err := b.ListVolumes(ctx)
	if err == nil {
		t.Fatal("expected context error from blocked ListVolumes, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
}

// TestZFSBackend_Error_ContextCancelled_Capacity validates that Capacity
// returns promptly when the caller's context is canceled before
// 'zpool list' completes.
//
// A pool I/O stall can cause 'zpool list' to block indefinitely; the
// backend must honor context cancellation to avoid agent hangs.
//
//	Setup:   executor blocks on ctx.Done(); context pre-canceled
//	Expect:  Capacity returns non-nil error wrapping context.Canceled
//
// See TESTCASES.md §2.7, row 39.
func TestZFSBackend_Error_ContextCancelled_Capacity(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before the command starts

	b := zfs.NewWithExecFn("tank", "", func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	_, _, err := b.Capacity(ctx)
	if err == nil {
		t.Fatal("expected context error from blocked Capacity, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
}
