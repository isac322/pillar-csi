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

package csi

// Tests for VolumeStateMachine.
//
// The test suite covers:
//   - Every legal transition in the happy-path lifecycle.
//   - Idempotent (noop) transitions for all CSI operations that require it.
//   - Partial-failure state transitions (NodeStagePartial → retry / cleanup).
//   - Illegal transitions that must return gRPC FailedPrecondition.
//   - Concurrent safety: multiple goroutines transitioning different volumes
//     simultaneously must not corrupt internal state.
//   - AllStates and ForceState helpers used for diagnostics and recovery.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestVolumeStateMachine

import (
	"fmt"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────.

// mustTransition calls sm.Transition and fails the test on error.
func mustTransition(t *testing.T, sm *VolumeStateMachine, volID string, op VolumeOperation) bool {
	t.Helper()
	noop, err := sm.Transition(volID, op)
	if err != nil {
		t.Fatalf("Transition(%q, %s): unexpected error: %v", volID, op, err)
	}
	return noop
}

// assertState checks that volID is in the expected state.
func assertState(t *testing.T, sm *VolumeStateMachine, volID string, want VolumeState) {
	t.Helper()
	got := sm.GetState(volID)
	if got != want {
		t.Errorf("GetState(%q) = %s; want %s", volID, got, want)
	}
}

// assertFailedPrecondition calls sm.Transition and verifies that the error
// carries gRPC code FailedPrecondition.
func assertFailedPrecondition(t *testing.T, sm *VolumeStateMachine, volID string, op VolumeOperation) {
	t.Helper()
	_, err := sm.Transition(volID, op)
	if err == nil {
		t.Fatalf("Transition(%q, %s): expected FailedPrecondition error, got nil", volID, op)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("Transition(%q, %s): error is not a gRPC status: %v", volID, op, err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("Transition(%q, %s): got gRPC code %s; want FailedPrecondition", volID, op, st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_HappyPath: every legal non-noop transition
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_HappyPath(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-happy-path"

	// ── Initial state ────────────────────────────────────────────────────────
	assertState(t, sm, vol, StateNonExistent)

	// ── CreateVolume ─────────────────────────────────────────────────────────
	noop := mustTransition(t, sm, vol, OpCreateVolume)
	if noop {
		t.Error("CreateVolume from NonExistent: expected non-noop")
	}
	assertState(t, sm, vol, StateCreated)

	// ── ControllerPublishVolume ───────────────────────────────────────────────
	noop = mustTransition(t, sm, vol, OpControllerPublish)
	if noop {
		t.Error("ControllerPublish from Created: expected non-noop")
	}
	assertState(t, sm, vol, StateControllerPublished)

	// ── NodeStageVolume ───────────────────────────────────────────────────────
	noop = mustTransition(t, sm, vol, OpNodeStage)
	if noop {
		t.Error("NodeStage from ControllerPublished: expected non-noop")
	}
	assertState(t, sm, vol, StateNodeStaged)

	// ── NodePublishVolume ─────────────────────────────────────────────────────
	noop = mustTransition(t, sm, vol, OpNodePublish)
	if noop {
		t.Error("NodePublish from NodeStaged: expected non-noop")
	}
	assertState(t, sm, vol, StateNodePublished)

	// ── NodeUnpublishVolume ───────────────────────────────────────────────────
	noop = mustTransition(t, sm, vol, OpNodeUnpublish)
	if noop {
		t.Error("NodeUnpublish from NodePublished: expected non-noop")
	}
	assertState(t, sm, vol, StateNodeStaged)

	// ── NodeUnstageVolume ─────────────────────────────────────────────────────
	noop = mustTransition(t, sm, vol, OpNodeUnstage)
	if noop {
		t.Error("NodeUnstage from NodeStaged: expected non-noop")
	}
	assertState(t, sm, vol, StateControllerPublished)

	// ── ControllerUnpublishVolume ─────────────────────────────────────────────
	noop = mustTransition(t, sm, vol, OpControllerUnpublish)
	if noop {
		t.Error("ControllerUnpublish from ControllerPublished: expected non-noop")
	}
	assertState(t, sm, vol, StateCreated)

	// ── DeleteVolume ─────────────────────────────────────────────────────────
	noop = mustTransition(t, sm, vol, OpDeleteVolume)
	if noop {
		t.Error("DeleteVolume from Created: expected non-noop")
	}
	assertState(t, sm, vol, StateNonExistent)

	// Volume should no longer appear in AllStates.
	all := sm.AllStates()
	if _, exists := all[vol]; exists {
		t.Errorf("AllStates: deleted volume %q still present", vol)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_IdempotentTransitions: noop for already-done states
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_IdempotentTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(sm *VolumeStateMachine, vol string) // bring vol to starting state
		op        VolumeOperation
		wantState VolumeState
	}{
		{
			name: "CreateVolume/already_created",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
			},
			op:        OpCreateVolume,
			wantState: StateCreated,
		},
		{
			name: "DeleteVolume/already_nonexistent",
			setup: func(_ *VolumeStateMachine, _ string) {
				// No setup; volume starts at NonExistent.
			},
			op:        OpDeleteVolume,
			wantState: StateNonExistent,
		},
		{
			name: "ControllerPublish/already_published",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
			},
			op:        OpControllerPublish,
			wantState: StateControllerPublished,
		},
		{
			name: "ControllerUnpublish/already_unpublished",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				// Volume is Created (= ControllerUnpublish is noop per spec).
			},
			op:        OpControllerUnpublish,
			wantState: StateCreated,
		},
		{
			name: "NodeStage/already_staged",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
			},
			op:        OpNodeStage,
			wantState: StateNodeStaged,
		},
		{
			name: "NodeUnstage/already_unstaged",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				// Volume is ControllerPublished; NodeUnstage is noop per spec.
			},
			op:        OpNodeUnstage,
			wantState: StateControllerPublished,
		},
		{
			name: "NodePublish/already_published",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
				mustTransition(t, sm, vol, OpNodePublish)
			},
			op:        OpNodePublish,
			wantState: StateNodePublished,
		},
		{
			name: "NodeUnpublish/already_unpublished",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
				// Volume is NodeStaged; NodeUnpublish is noop per spec.
			},
			op:        OpNodeUnpublish,
			wantState: StateNodeStaged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewVolumeStateMachine()
			vol := "pvc-noop-" + tc.name
			tc.setup(sm, vol)

			noop, err := sm.Transition(vol, tc.op)
			if err != nil {
				t.Fatalf("Transition(%s): unexpected error: %v", tc.op, err)
			}
			if !noop {
				t.Errorf("Transition(%s): expected isNoop=true", tc.op)
			}
			assertState(t, sm, vol, tc.wantState)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_PartialFailure: NodeStagePartial state
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_PartialFailure_RetrySucceeds(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-partial-retry"

	// Bring volume to ControllerPublished.
	mustTransition(t, sm, vol, OpCreateVolume)
	mustTransition(t, sm, vol, OpControllerPublish)
	assertState(t, sm, vol, StateControllerPublished)

	// Protocol attach succeeded; record the partial state.
	noop := mustTransition(t, sm, vol, OpNodeStageConnected)
	if noop {
		t.Error("NodeStageConnected from ControllerPublished: expected non-noop")
	}
	assertState(t, sm, vol, StateNodeStagePartial)

	// Mount step (e.g.) failed — leave volume in partial state.
	// Now retry the full NodeStageVolume: must succeed and advance to NodeStaged.
	noop = mustTransition(t, sm, vol, OpNodeStage)
	if noop {
		t.Error("NodeStage from NodeStagePartial: expected non-noop")
	}
	assertState(t, sm, vol, StateNodeStaged)
}

func TestVolumeStateMachine_PartialFailure_CleanupViaUnstage(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-partial-cleanup"

	mustTransition(t, sm, vol, OpCreateVolume)
	mustTransition(t, sm, vol, OpControllerPublish)

	// Simulate partial stage (connect OK, mount failed).
	mustTransition(t, sm, vol, OpNodeStageConnected)
	assertState(t, sm, vol, StateNodeStagePartial)

	// Unstage cleans up the partial connection.
	noop := mustTransition(t, sm, vol, OpNodeUnstage)
	if noop {
		t.Error("NodeUnstage from NodeStagePartial: expected non-noop")
	}
	assertState(t, sm, vol, StateControllerPublished)
}

func TestVolumeStateMachine_PartialFailure_ForceStateAfterMountSuccess(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-partial-force"

	mustTransition(t, sm, vol, OpCreateVolume)
	mustTransition(t, sm, vol, OpControllerPublish)
	mustTransition(t, sm, vol, OpNodeStageConnected)
	assertState(t, sm, vol, StateNodeStagePartial)

	// Mount succeeded; use ForceState to advance to NodeStaged.
	sm.ForceState(vol, StateNodeStaged)
	assertState(t, sm, vol, StateNodeStaged)

	// Continue lifecycle normally.
	mustTransition(t, sm, vol, OpNodePublish)
	assertState(t, sm, vol, StateNodePublished)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_IllegalTransitions: must return FailedPrecondition
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_IllegalTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(sm *VolumeStateMachine, vol string)
		op    VolumeOperation
	}{
		// ── Operations on NonExistent volume ─────────────────────────────────
		{
			name:  "ControllerPublish/NonExistent",
			setup: func(_ *VolumeStateMachine, _ string) {},
			op:    OpControllerPublish,
		},
		{
			name:  "NodeStage/NonExistent",
			setup: func(_ *VolumeStateMachine, _ string) {},
			op:    OpNodeStage,
		},
		{
			name:  "NodePublish/NonExistent",
			setup: func(_ *VolumeStateMachine, _ string) {},
			op:    OpNodePublish,
		},
		{
			name:  "NodeUnstage/NonExistent",
			setup: func(_ *VolumeStateMachine, _ string) {},
			op:    OpNodeUnstage,
		},
		{
			name:  "NodeUnpublish/NonExistent",
			setup: func(_ *VolumeStateMachine, _ string) {},
			op:    OpNodeUnpublish,
		},

		// ── Skipping ControllerPublish ────────────────────────────────────────
		{
			name: "NodeStage/Created_no_ControllerPublish",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
			},
			op: OpNodeStage,
		},
		{
			name: "NodePublish/Created_no_ControllerPublish",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
			},
			op: OpNodePublish,
		},

		// ── Skipping NodeStage ────────────────────────────────────────────────
		{
			name: "NodePublish/ControllerPublished_no_NodeStage",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
			},
			op: OpNodePublish,
		},

		// ── Out-of-order teardown ─────────────────────────────────────────────
		{
			name: "NodeUnstage/NodePublished_no_NodeUnpublish",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
				mustTransition(t, sm, vol, OpNodePublish)
			},
			op: OpNodeUnstage,
		},
		{
			name: "ControllerUnpublish/NodeStaged_no_NodeUnstage",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
			},
			op: OpControllerUnpublish,
		},
		{
			name: "ControllerUnpublish/NodePublished_no_cleanup",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
				mustTransition(t, sm, vol, OpNodePublish)
			},
			op: OpControllerUnpublish,
		},
		{
			name: "DeleteVolume/NodeStaged_no_cleanup",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
			},
			op: OpDeleteVolume,
		},
		{
			name: "DeleteVolume/NodePublished_no_cleanup",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
				mustTransition(t, sm, vol, OpNodePublish)
			},
			op: OpDeleteVolume,
		},
		{
			name: "DeleteVolume/ControllerPublished_no_unpublish",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
			},
			op: OpDeleteVolume,
		},

		// ── NodeStageConnected only valid from ControllerPublished ────────────
		{
			name: "NodeStageConnected/Created",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
			},
			op: OpNodeStageConnected,
		},
		{
			name: "NodeStageConnected/NodeStaged",
			setup: func(sm *VolumeStateMachine, vol string) {
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
			},
			op: OpNodeStageConnected,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewVolumeStateMachine()
			vol := "pvc-illegal-" + tc.name
			tc.setup(sm, vol)
			assertFailedPrecondition(t, sm, vol, tc.op)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_MultipleVolumes: independent tracking per volume ID
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_MultipleVolumes(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()

	const volA = "pvc-multi-a"
	const volB = "pvc-multi-b"

	// Volume A goes through the full lifecycle.
	mustTransition(t, sm, volA, OpCreateVolume)
	mustTransition(t, sm, volA, OpControllerPublish)

	// Volume B is only created.
	mustTransition(t, sm, volB, OpCreateVolume)

	// Check states are independent.
	assertState(t, sm, volA, StateControllerPublished)
	assertState(t, sm, volB, StateCreated)

	// Illegal for B (not ControllerPublished) but legal for A.
	assertFailedPrecondition(t, sm, volB, OpNodeStage)
	mustTransition(t, sm, volA, OpNodeStage)
	assertState(t, sm, volA, StateNodeStaged)
	assertState(t, sm, volB, StateCreated) // B unchanged
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_AllStates: snapshot reflects current state
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_AllStates(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()

	// AllStates on empty machine.
	all := sm.AllStates()
	if len(all) != 0 {
		t.Errorf("AllStates on empty machine: want 0 entries, got %d", len(all))
	}

	mustTransition(t, sm, "vol-a", OpCreateVolume)
	mustTransition(t, sm, "vol-b", OpCreateVolume)
	mustTransition(t, sm, "vol-b", OpControllerPublish)

	all = sm.AllStates()
	if got, want := all["vol-a"], StateCreated; got != want {
		t.Errorf("AllStates[vol-a] = %s; want %s", got, want)
	}
	if got, want := all["vol-b"], StateControllerPublished; got != want {
		t.Errorf("AllStates[vol-b] = %s; want %s", got, want)
	}

	// AllStates must be a copy; mutations must not affect the machine.
	all["vol-a"] = StateNodePublished
	if sm.GetState("vol-a") != StateCreated {
		t.Error("AllStates copy was not isolated from the state machine")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_ForceState: bypass validation for recovery
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_ForceState(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-force"

	// Force a volume directly to NodeStaged without normal transitions.
	sm.ForceState(vol, StateNodeStaged)
	assertState(t, sm, vol, StateNodeStaged)

	// ForceState to NonExistent must remove the entry.
	sm.ForceState(vol, StateNonExistent)
	assertState(t, sm, vol, StateNonExistent)
	all := sm.AllStates()
	if _, exists := all[vol]; exists {
		t.Errorf("ForceState(NonExistent): volume still present in AllStates")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_ConcurrentSafety: no data races
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping concurrent safety test in short mode")
	}
	sm := NewVolumeStateMachine()

	const goroutines = 20
	const volsPerGoroutine = 5

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			for j := range volsPerGoroutine {
				vol := fmt.Sprintf("pvc-concurrent-%d-%d", id, j)
				mustTransition(t, sm, vol, OpCreateVolume)
				mustTransition(t, sm, vol, OpControllerPublish)
				mustTransition(t, sm, vol, OpNodeStage)
				mustTransition(t, sm, vol, OpNodePublish)
				mustTransition(t, sm, vol, OpNodeUnpublish)
				mustTransition(t, sm, vol, OpNodeUnstage)
				mustTransition(t, sm, vol, OpControllerUnpublish)
				mustTransition(t, sm, vol, OpDeleteVolume)
				assertState(t, sm, vol, StateNonExistent)
			}
		}(i)
	}

	wg.Wait()

	// All volumes should be gone.
	if got := len(sm.AllStates()); got != 0 {
		t.Errorf("after concurrent full lifecycle: AllStates has %d entries; want 0", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeState_String: human-readable labels
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeState_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state VolumeState
		want  string
	}{
		{StateNonExistent, "NonExistent"},
		{StateCreated, "Created"},
		{StateControllerPublished, "ControllerPublished"},
		{StateNodeStaged, "NodeStaged"},
		{StateNodePublished, "NodePublished"},
		{StateNodeStagePartial, "NodeStagePartial"},
		{StateCreatePartial, "CreatePartial"},
		{VolumeState(99), "VolumeState(99)"},
	}
	for _, tc := range tests {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("VolumeState(%d).String() = %q; want %q", int(tc.state), got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_ErrorMessageContent: error messages are informative
// ─────────────────────────────────────────────────────────────────────────────.

func TestVolumeStateMachine_ErrorMessageContent(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-err-msg"

	_, err := sm.Transition(vol, OpControllerPublish)
	if err == nil {
		t.Fatal("expected error for illegal transition")
	}

	msg := err.Error()
	for _, want := range []string{vol, string(OpControllerPublish), "NonExistent"} {
		if !containsSubstring(msg, want) {
			t.Errorf("error message %q missing expected substring %q", msg, want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestVolumeStateMachine_CreatePartial: partial-failure within CreateVolume
// ─────────────────────────────────────────────────────────────────────────────.

// TestVolumeStateMachine_CreatePartial_BackendSucceeds verifies that the
// OpCreateVolumeBackend pseudo-operation drives a new volume into
// StateCreatePartial from StateNonExistent.
func TestVolumeStateMachine_CreatePartial_BackendSucceeds(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-create-partial"

	assertState(t, sm, vol, StateNonExistent)

	// Simulate: backend creation succeeded.
	noop := mustTransition(t, sm, vol, OpCreateVolumeBackend)
	if noop {
		t.Error("CreateVolumeBackend from NonExistent: expected non-noop")
	}
	assertState(t, sm, vol, StateCreatePartial)
}

// TestVolumeStateMachine_CreatePartial_IdempotentBackend verifies that
// calling OpCreateVolumeBackend from StateCreatePartial is a noop (the
// agent's CreateVolume is idempotent on retry).
func TestVolumeStateMachine_CreatePartial_IdempotentBackend(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-create-partial-idem"

	mustTransition(t, sm, vol, OpCreateVolumeBackend)
	assertState(t, sm, vol, StateCreatePartial)

	// Retry backend creation: should be a noop.
	noop := mustTransition(t, sm, vol, OpCreateVolumeBackend)
	if !noop {
		t.Error("CreateVolumeBackend from CreatePartial: expected noop (idempotent)")
	}
	assertState(t, sm, vol, StateCreatePartial)
}

// TestVolumeStateMachine_CreatePartial_ExportRetrySucceeds verifies that
// calling OpCreateVolume from StateCreatePartial (export retry) advances the
// volume to StateCreated.
func TestVolumeStateMachine_CreatePartial_ExportRetrySucceeds(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-create-partial-retry"

	// Backend created, export failed.
	mustTransition(t, sm, vol, OpCreateVolumeBackend)
	assertState(t, sm, vol, StateCreatePartial)

	// Retry: export succeeds → advance to StateCreated.
	noop := mustTransition(t, sm, vol, OpCreateVolume)
	if noop {
		t.Error("CreateVolume from CreatePartial: expected non-noop (export retry)")
	}
	assertState(t, sm, vol, StateCreated)
}

// TestVolumeStateMachine_CreatePartial_DeleteVolumeCleanup verifies that
// DeleteVolume from StateCreatePartial transitions back to StateNonExistent,
// allowing the CO to clean up a partially-created volume.
func TestVolumeStateMachine_CreatePartial_DeleteVolumeCleanup(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-create-partial-delete"

	// Backend created, export failed.
	mustTransition(t, sm, vol, OpCreateVolumeBackend)
	assertState(t, sm, vol, StateCreatePartial)

	// DeleteVolume cleans up the backend.
	noop := mustTransition(t, sm, vol, OpDeleteVolume)
	if noop {
		t.Error("DeleteVolume from CreatePartial: expected non-noop (cleanup)")
	}
	assertState(t, sm, vol, StateNonExistent)

	// Volume should no longer appear in AllStates.
	if _, exists := sm.AllStates()[vol]; exists {
		t.Errorf("AllStates: cleaned-up partial volume %q still present", vol)
	}
}

// TestVolumeStateMachine_CreatePartial_FullLifecycle verifies that a volume
// can proceed through the full lifecycle after recovering from CreatePartial
// (export retry succeeds).
func TestVolumeStateMachine_CreatePartial_FullLifecycle(t *testing.T) {
	t.Parallel()
	sm := NewVolumeStateMachine()
	const vol = "pvc-create-partial-full"

	// Simulate: backend created (export will fail first time).
	mustTransition(t, sm, vol, OpCreateVolumeBackend)
	assertState(t, sm, vol, StateCreatePartial)

	// Retry: export succeeds.
	mustTransition(t, sm, vol, OpCreateVolume)
	assertState(t, sm, vol, StateCreated)

	// Proceed through the full lifecycle.
	mustTransition(t, sm, vol, OpControllerPublish)
	assertState(t, sm, vol, StateControllerPublished)

	mustTransition(t, sm, vol, OpNodeStage)
	assertState(t, sm, vol, StateNodeStaged)

	mustTransition(t, sm, vol, OpNodePublish)
	assertState(t, sm, vol, StateNodePublished)

	mustTransition(t, sm, vol, OpNodeUnpublish)
	mustTransition(t, sm, vol, OpNodeUnstage)
	mustTransition(t, sm, vol, OpControllerUnpublish)
	mustTransition(t, sm, vol, OpDeleteVolume)
	assertState(t, sm, vol, StateNonExistent)
}

// TestVolumeStateMachine_CreatePartial_IllegalTransitions verifies that
// operations which are not valid from StateCreatePartial return
// FailedPrecondition.
func TestVolumeStateMachine_CreatePartial_IllegalTransitions(t *testing.T) {
	t.Parallel()
	illegalOps := []VolumeOperation{
		OpControllerPublish,
		OpControllerUnpublish,
		OpNodeStage,
		OpNodeUnstage,
		OpNodePublish,
		OpNodeUnpublish,
		OpNodeStageConnected,
	}
	for _, op := range illegalOps {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()
			sm := NewVolumeStateMachine()
			vol := "pvc-create-partial-illegal"
			mustTransition(t, sm, vol, OpCreateVolumeBackend)
			assertState(t, sm, vol, StateCreatePartial)
			assertFailedPrecondition(t, sm, vol, op)
		})
	}
}

// TestVolumeState_String_CreatePartial verifies the String() method includes
// the new StateCreatePartial label.
func TestVolumeState_String_CreatePartial(t *testing.T) {
	t.Parallel()
	if got, want := StateCreatePartial.String(), "CreatePartial"; got != want {
		t.Errorf("StateCreatePartial.String() = %q; want %q", got, want)
	}
}

// containsSubstring is a simple string containment helper to avoid importing
// the strings package just for tests.
func containsSubstring(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
