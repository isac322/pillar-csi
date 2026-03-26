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

// Package csi – VolumeStateMachine
//
// This file formalizes the CSI volume lifecycle as an explicit finite state
// machine.  Every CSI controller and node operation must call Transition
// before executing any privileged work.  Out-of-order or otherwise illegal
// operations are rejected with a gRPC FailedPrecondition status so that the
// Container Orchestrator receives a clear, actionable error rather than
// silently corrupt state.
//
// # Volume lifecycle (happy path)
//
//	NonExistent ──CreateVolume──► Created ──ControllerPublish──► ControllerPublished
//	                │                                                      │
//	                ◄────────────DeleteVolume──────────────────────────────┤
//	                                                              NodeStage▼
//	                                                           NodeStaged ──NodePublish──► NodePublished
//	                                                                │                          │
//	                                                                ◄──────NodeUnpublish────────┘
//	                                                      NodeUnstage▼
//	                                              ControllerPublished ──ControllerUnpublish──► Created
//
// # Partial-failure states
//
//	NodeStagePartial: NVMe-oF Connect succeeded but the mount step failed.
//	  - Retry: NodeStageVolume → NodeStaged
//	  - Cleanup: NodeUnstageVolume → ControllerPublished (disconnects the fabric)

import (
	"fmt"
	"maps"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// VolumeState
// ─────────────────────────────────────────────────────────────────────────────.

// VolumeState represents the lifecycle state of a CSI volume.
type VolumeState int

const (
	// StateNonExistent means the volume has not been created yet, or has been
	// successfully deleted.  This is the zero value and the default state for
	// any unknown volume ID.
	StateNonExistent VolumeState = iota

	// StateCreated means CreateVolume has succeeded but ControllerPublishVolume
	// has not yet been called.  The backing storage exists on the agent node
	// but no initiator has been granted access.
	StateCreated

	// StateControllerPublished means ControllerPublishVolume has succeeded.
	// The volume's NVMe-oF target (or equivalent) has been authorized to accept
	// connections from the target node's initiator NQN.
	StateControllerPublished

	// StateNodeStaged means NodeStageVolume has succeeded.  The node is
	// connected to the NVMe-oF target and the volume is formatted (if
	// necessary) and mounted at the staging target path.
	StateNodeStaged

	// StateNodePublished means NodePublishVolume has succeeded.  The staging
	// path has been bind-mounted into a pod's target path, making the volume
	// visible to the workload.
	StateNodePublished

	// StateNodeStagePartial is a partial-failure state that arises when
	// NodeStageVolume completes the NVMe-oF connect step but fails during the
	// mount step.  From this state:
	//   - NodeStageVolume may be retried (transitions to StateNodeStaged).
	//   - NodeUnstageVolume performs cleanup (disconnects NVMe-oF, transitions
	//     to StateControllerPublished).
	StateNodeStagePartial

	// StateCreatePartial is a partial-failure state that arises when
	// CreateVolume creates the backend storage resource (zvol, LVM LV, etc.)
	// successfully but fails during the ExportVolume step.
	//
	// The volume exists on the storage node but is not yet accessible over
	// the network.  From this state:
	//   - CreateVolume may be retried: the controller skips backend creation
	//     (already done) and re-attempts ExportVolume.  On success the state
	//     advances to StateCreated.
	//   - DeleteVolume performs cleanup: the controller calls UnexportVolume
	//     (idempotent, may be a no-op) and then DeleteVolume on the agent to
	//     reclaim the backing storage.  The state returns to StateNonExistent.
	//
	// This state is persisted durably in a PillarVolume CRD so that the
	// controller can recover correctly after a restart.
	StateCreatePartial
)

// String returns a human-readable label for the state (for logging and error
// messages).
func (s VolumeState) String() string {
	switch s {
	case StateNonExistent:
		return "NonExistent"
	case StateCreated:
		return "Created"
	case StateControllerPublished:
		return "ControllerPublished"
	case StateNodeStaged:
		return "NodeStaged"
	case StateNodePublished:
		return "NodePublished"
	case StateNodeStagePartial:
		return "NodeStagePartial"
	case StateCreatePartial:
		return "CreatePartial"
	default:
		return fmt.Sprintf("VolumeState(%d)", int(s))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// VolumeOperation
// ─────────────────────────────────────────────────────────────────────────────.

// VolumeOperation identifies a CSI lifecycle operation that drives a state
// transition.
type VolumeOperation string

const (
	// OpCreateVolume corresponds to the CSI CreateVolume RPC.
	OpCreateVolume VolumeOperation = "CreateVolume"

	// OpDeleteVolume corresponds to the CSI DeleteVolume RPC.
	OpDeleteVolume VolumeOperation = "DeleteVolume"

	// OpControllerPublish corresponds to the CSI ControllerPublishVolume RPC.
	OpControllerPublish VolumeOperation = "ControllerPublishVolume"

	// OpControllerUnpublish corresponds to the CSI ControllerUnpublishVolume RPC.
	OpControllerUnpublish VolumeOperation = "ControllerUnpublishVolume"

	// OpNodeStage corresponds to the CSI NodeStageVolume RPC.
	OpNodeStage VolumeOperation = "NodeStageVolume"

	// OpNodeUnstage corresponds to the CSI NodeUnstageVolume RPC.
	OpNodeUnstage VolumeOperation = "NodeUnstageVolume"

	// OpNodePublish corresponds to the CSI NodePublishVolume RPC.
	OpNodePublish VolumeOperation = "NodePublishVolume"

	// OpNodeUnpublish corresponds to the CSI NodeUnpublishVolume RPC.
	OpNodeUnpublish VolumeOperation = "NodeUnpublishVolume"

	// OpNodeStageConnected is an internal pseudo-operation used by
	// NodeStageVolume to record that the NVMe-oF connect step has succeeded
	// but the mount step has not yet started.  It drives the volume into
	// StateNodeStagePartial so that a subsequent mount failure leaves the
	// state machine in a recoverable partial-failure state rather than the
	// pre-stage ControllerPublished state.
	//
	// Callers outside the csi package should not use this operation directly.
	OpNodeStageConnected VolumeOperation = "NodeStageConnected"

	// OpCreateVolumeBackend is an internal pseudo-operation used by
	// CreateVolume to record that the backend storage resource (zvol, LVM LV,
	// etc.) has been created successfully, but the ExportVolume step has not
	// yet succeeded.  It drives the volume into StateCreatePartial so that a
	// subsequent ExportVolume failure leaves the state machine in a
	// recoverable partial-failure state.
	//
	// The controller also persists this transition durably in a PillarVolume
	// CRD so that a restart does not lose track of the partially-created
	// volume.
	//
	// Callers outside the csi package should not use this operation directly.
	OpCreateVolumeBackend VolumeOperation = "CreateVolumeBackend"
)

// ─────────────────────────────────────────────────────────────────────────────
// Transition table
// ─────────────────────────────────────────────────────────────────────────────.

// transitionKey is the lookup key for the transition table.
type transitionKey struct {
	from VolumeState
	op   VolumeOperation
}

// transitionResult describes the outcome of a legal transition.
type transitionResult struct {
	// to is the state the volume moves to after the operation succeeds.
	to VolumeState

	// isNoop is true when the volume is already in the desired state and the
	// operation is idempotent.  The caller must still succeed (per the CSI
	// spec) but must not repeat any side-effecting work.
	isNoop bool
}

// legalTransitions is the complete transition table for the CSI volume
// lifecycle.  Any (from, op) pair absent from this map is illegal and will
// cause Transition to return a FailedPrecondition error.
//
// Idempotent (noop) transitions are included explicitly so that callers can
// detect the "already done" case and return early without re-executing
// privileged operations.
var legalTransitions = map[transitionKey]transitionResult{
	// ── CreateVolume / DeleteVolume ─────────────────────────────────────────
	// CreateVolume is idempotent when the volume already exists (CSI §5.1.1).
	{StateNonExistent, OpCreateVolume}: {to: StateCreated},
	{StateCreated, OpCreateVolume}:     {to: StateCreated, isNoop: true},

	// CreateVolumeBackend records that the backend storage resource was
	// created but ExportVolume has not yet been attempted.  This drives the
	// volume into the StateCreatePartial partial-failure state.
	{StateNonExistent, OpCreateVolumeBackend}:   {to: StateCreatePartial},
	{StateCreatePartial, OpCreateVolumeBackend}: {to: StateCreatePartial, isNoop: true},

	// From StateCreatePartial the volume can be recovered in two ways:
	//   1. Retry CreateVolume → ExportVolume succeeds → StateCreated.
	//   2. Call DeleteVolume → cleanup → StateNonExistent.
	{StateCreatePartial, OpCreateVolume}: {to: StateCreated},
	{StateCreatePartial, OpDeleteVolume}: {to: StateNonExistent},

	// DeleteVolume is idempotent: deleting a non-existent volume must succeed
	// (CSI §5.1.2).
	{StateCreated, OpDeleteVolume}:     {to: StateNonExistent},
	{StateNonExistent, OpDeleteVolume}: {to: StateNonExistent, isNoop: true},

	// ── ControllerPublish / ControllerUnpublish ─────────────────────────────
	// ControllerPublishVolume is idempotent (CSI §5.2.1).
	{StateCreated, OpControllerPublish}:             {to: StateControllerPublished},
	{StateControllerPublished, OpControllerPublish}: {to: StateControllerPublished, isNoop: true},

	// ControllerUnpublishVolume is idempotent (CSI §5.2.2).
	{StateControllerPublished, OpControllerUnpublish}: {to: StateCreated},
	{StateCreated, OpControllerUnpublish}:             {to: StateCreated, isNoop: true},

	// ── NodeStage / NodeUnstage ─────────────────────────────────────────────
	// NodeStageVolume is idempotent (CSI §5.3.1).
	{StateControllerPublished, OpNodeStage}: {to: StateNodeStaged},
	{StateNodeStaged, OpNodeStage}:          {to: StateNodeStaged, isNoop: true},

	// NodeStageConnected records that the NVMe-oF connect succeeded but mount
	// has not yet been attempted.  This is an internal operation that drives
	// the volume into the NodeStagePartial state.
	{StateControllerPublished, OpNodeStageConnected}: {to: StateNodeStagePartial},

	// Partial-failure recovery: retry mount (→ NodeStaged) or clean up (→ ControllerPublished).
	{StateNodeStagePartial, OpNodeStage}:   {to: StateNodeStaged},
	{StateNodeStagePartial, OpNodeUnstage}: {to: StateControllerPublished},

	// NodeUnstageVolume is idempotent (CSI §5.3.2).
	{StateNodeStaged, OpNodeUnstage}:          {to: StateControllerPublished},
	{StateControllerPublished, OpNodeUnstage}: {to: StateControllerPublished, isNoop: true},

	// ── NodePublish / NodeUnpublish ─────────────────────────────────────────
	// NodePublishVolume is idempotent (CSI §5.4.1).
	{StateNodeStaged, OpNodePublish}:    {to: StateNodePublished},
	{StateNodePublished, OpNodePublish}: {to: StateNodePublished, isNoop: true},

	// NodeUnpublishVolume is idempotent (CSI §5.4.2).
	{StateNodePublished, OpNodeUnpublish}: {to: StateNodeStaged},
	{StateNodeStaged, OpNodeUnpublish}:    {to: StateNodeStaged, isNoop: true},
}

// ─────────────────────────────────────────────────────────────────────────────
// VolumeStateMachine
// ─────────────────────────────────────────────────────────────────────────────.

// VolumeStateMachine tracks the lifecycle state of volumes and validates that
// CSI operations are applied in the correct order.
//
// The machine is safe for concurrent use by multiple goroutines.  Each volume
// ID has an independent state; operations on different volumes do not block
// each other.
//
// Usage pattern (NodeStageVolume with partial-failure support):
//
//	isNoop, err := sm.Transition(volumeID, csiState.OpNodeStage)
//	if err != nil { return nil, err }
//	if isNoop { return &csi.NodeStageVolumeResponse{}, nil }
//
//	// Mark partial: NVMe-oF connect succeeded, mount not yet started.
//	if err := connector.Connect(ctx, nqn, addr, port); err != nil { … }
//	_, _ = sm.Transition(volumeID, csiState.OpNodeStageConnected)
//
//	if err := mounter.FormatAndMount(…); err != nil {
//	    // Leave volume in NodeStagePartial; caller can retry or unstage.
//	    return nil, err
//	}
//	// Mount succeeded — advance to fully staged.
//	sm.ForceState(volumeID, csiState.StateNodeStaged)
type VolumeStateMachine struct {
	mu     sync.RWMutex
	states map[string]VolumeState
}

// NewVolumeStateMachine constructs an empty VolumeStateMachine.
func NewVolumeStateMachine() *VolumeStateMachine {
	return &VolumeStateMachine{
		states: make(map[string]VolumeState),
	}
}

// GetState returns the current state of the given volume.
// Returns StateNonExistent if the volume is not tracked by the machine.
func (m *VolumeStateMachine) GetState(volumeID string) VolumeState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lockedGetState(volumeID)
}

// lockedGetState is the non-locking variant of GetState; callers must hold at
// least a read lock.
func (m *VolumeStateMachine) lockedGetState(volumeID string) VolumeState {
	if s, ok := m.states[volumeID]; ok {
		return s
	}
	return StateNonExistent
}

// Transition validates and applies the given operation for volumeID.
//
// The method is atomic with respect to concurrent Transition calls for the
// same volume: the state is read and updated under a single write lock.
//
// Returns:
//   - isNoop: true when the transition is idempotent and the volume was
//     already in the target state.  The caller must return success immediately
//     without repeating side-effecting work.
//   - err: a gRPC status error with code FailedPrecondition when the
//     (current state, operation) pair is not in the legal transition table.
//     The error message names the volume ID, the operation, and the current
//     state to aid debugging.
func (m *VolumeStateMachine) Transition(volumeID string, op VolumeOperation) (isNoop bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current := m.lockedGetState(volumeID)
	key := transitionKey{from: current, op: op}

	result, legal := legalTransitions[key]
	if !legal {
		return false, status.Errorf(
			codes.FailedPrecondition,
			"volume %q: operation %s is not valid in state %s",
			volumeID, op, current,
		)
	}

	if !result.isNoop {
		m.lockedSetState(volumeID, result.to)
	}

	return result.isNoop, nil
}

// ForceState directly sets the state for volumeID, bypassing transition
// validation.
//
// This method should only be used for two purposes:
//  1. Recovering from a detected partial-failure (e.g., after a successful
//     mount following OpNodeStageConnected).
//  2. Loading persisted state on restart.
//
// Incorrect use of ForceState can corrupt the state machine; prefer Transition
// for normal operation.
func (m *VolumeStateMachine) ForceState(volumeID string, state VolumeState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockedSetState(volumeID, state)
}

// lockedSetState updates (or removes) the state for volumeID; callers must
// hold the write lock.
func (m *VolumeStateMachine) lockedSetState(volumeID string, state VolumeState) {
	if state == StateNonExistent {
		delete(m.states, volumeID)
	} else {
		m.states[volumeID] = state
	}
}

// AllStates returns a snapshot of all tracked volumes and their current states.
// The returned map is a copy; mutations do not affect the state machine.
//
// This method is intended for diagnostics and testing only.
func (m *VolumeStateMachine) AllStates() map[string]VolumeState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshot := make(map[string]VolumeState, len(m.states))
	maps.Copy(snapshot, m.states)
	return snapshot
}
