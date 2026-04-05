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

// Package unit_test — E5 Ordering Constraints (VolumeStateMachine)
//
// Unit test 근거: VolumeStateMachine은 현재 상태와 요청된 전이만으로
// 허용/거부를 결정하는 순수 상태 머신이다.
// 외부 I/O 없이 상태 전이 규칙의 정확성을 검증할 수 있다.
//
// Run with:
//
//	go test ./test/unit/ -v -run TestCSIOrdering
package unit_test

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// requireFailedPrecondition verifies that err is a gRPC FailedPrecondition
// error, which the state machine returns for out-of-order transitions.
func requireFailedPrecondition(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected gRPC error, got nil")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("expected codes.FailedPrecondition, got %s: %v", got, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E5.1 역순 호출 거부
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIOrdering_NodePublishAfterUnstage verifies that NodePublish returns
// FailedPrecondition when called after a full lifecycle has completed and the
// volume has been unstaged — leaving it in ControllerPublished state.
//
// Corresponds to E5 ID 46 in docs/testing/UNIT-TESTS.md.
func TestCSIOrdering_NodePublishAfterUnstage(t *testing.T) {
	t.Parallel()

	sm := pillarcsi.NewVolumeStateMachine()
	const vol = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-ordering-46"

	// Advance through a full lifecycle until NodeUnstage.
	advanceOrFail := func(op pillarcsi.VolumeOperation) {
		t.Helper()
		_, err := sm.Transition(vol, op)
		if err != nil {
			t.Fatalf("unexpected SM error during setup step %s: %v", op, err)
		}
	}
	advanceOrFail(pillarcsi.OpCreateVolume)
	advanceOrFail(pillarcsi.OpControllerPublish)
	advanceOrFail(pillarcsi.OpNodeStage)
	advanceOrFail(pillarcsi.OpNodePublish)
	advanceOrFail(pillarcsi.OpNodeUnpublish)
	advanceOrFail(pillarcsi.OpNodeUnstage)

	// Volume is now in ControllerPublished state.
	// NodePublish requires NodeStaged — must return FailedPrecondition.
	_, err := sm.Transition(vol, pillarcsi.OpNodePublish)
	requireFailedPrecondition(t, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// E5.2 정상 순서 통과
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIOrdering_FullLifecycleWithSM verifies that a complete CSI volume
// lifecycle succeeds when operations are applied in the correct order.
// Each state transition is checked for correctness using the VolumeStateMachine
// without any external dependencies (agent, K8s, filesystem).
//
// Corresponds to E5 ID 47 in docs/testing/UNIT-TESTS.md.
func TestCSIOrdering_FullLifecycleWithSM(t *testing.T) {
	t.Parallel()

	sm := pillarcsi.NewVolumeStateMachine()
	const vol = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-ordering-47"

	type step struct {
		op        pillarcsi.VolumeOperation
		wantState pillarcsi.VolumeState
	}

	steps := []step{
		{pillarcsi.OpCreateVolume, pillarcsi.StateCreated},
		{pillarcsi.OpControllerPublish, pillarcsi.StateControllerPublished},
		{pillarcsi.OpNodeStage, pillarcsi.StateNodeStaged},
		{pillarcsi.OpNodePublish, pillarcsi.StateNodePublished},
		{pillarcsi.OpNodeUnpublish, pillarcsi.StateNodeStaged},
		{pillarcsi.OpNodeUnstage, pillarcsi.StateControllerPublished},
		{pillarcsi.OpControllerUnpublish, pillarcsi.StateCreated},
		{pillarcsi.OpDeleteVolume, pillarcsi.StateNonExistent},
	}

	for _, s := range steps {
		isNoop, err := sm.Transition(vol, s.op)
		if err != nil {
			t.Fatalf("SM.Transition(%q, %s): unexpected error: %v", vol, s.op, err)
		}
		if isNoop {
			t.Errorf("SM.Transition(%q, %s): got isNoop=true, want false (fresh transition)", vol, s.op)
		}
		got := sm.GetState(vol)
		if got != s.wantState {
			t.Errorf("after %s: state = %s, want %s", s.op, got, s.wantState)
		}
	}
}

// TestCSIOrdering_IdempotencyWithSM verifies that repeating a CSI operation
// when the volume is already in the target state does not raise a
// FailedPrecondition error; instead, the transition is marked as a noop.
//
// Corresponds to E5 ID 48 in docs/testing/UNIT-TESTS.md.
func TestCSIOrdering_IdempotencyWithSM(t *testing.T) {
	t.Parallel()

	sm := pillarcsi.NewVolumeStateMachine()
	const vol = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-ordering-48"

	type idempotentCase struct {
		state pillarcsi.VolumeState
		op    pillarcsi.VolumeOperation
	}

	cases := []idempotentCase{
		// CreateVolume is idempotent when already Created.
		{pillarcsi.StateCreated, pillarcsi.OpCreateVolume},
		// ControllerPublishVolume is idempotent when already ControllerPublished.
		{pillarcsi.StateControllerPublished, pillarcsi.OpControllerPublish},
		// NodeStageVolume is idempotent when already NodeStaged.
		{pillarcsi.StateNodeStaged, pillarcsi.OpNodeStage},
		// NodePublishVolume is idempotent when already NodePublished.
		{pillarcsi.StateNodePublished, pillarcsi.OpNodePublish},
		// NodeUnpublishVolume is idempotent when already NodeStaged (unpublished).
		{pillarcsi.StateNodeStaged, pillarcsi.OpNodeUnpublish},
		// NodeUnstageVolume is idempotent when already ControllerPublished (unstaged).
		{pillarcsi.StateControllerPublished, pillarcsi.OpNodeUnstage},
		// ControllerUnpublishVolume is idempotent when already Created.
		{pillarcsi.StateCreated, pillarcsi.OpControllerUnpublish},
		// DeleteVolume is idempotent when NonExistent.
		{pillarcsi.StateNonExistent, pillarcsi.OpDeleteVolume},
	}

	for _, c := range cases {
		sm.ForceState(vol, c.state)
		isNoop, err := sm.Transition(vol, c.op)
		if err != nil {
			t.Errorf("SM.Transition(%s → %s): unexpected error: %v", c.state, c.op, err)
		}
		if !isNoop {
			t.Errorf("SM.Transition(%s → %s): got isNoop=false, want true (idempotent case)", c.state, c.op)
		}
	}
}
