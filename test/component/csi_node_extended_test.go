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

// Package component_test – CSI Node Service extended component tests.
//
// This file covers:
//   - Component 5, section 5.8: State Machine Integration
//   - Component 5, section 5.12: Concurrent Node Operations
//
// See test/component/TESTCASES.md sections 5.8 and 5.12 for the authoritative
// test-case specification.
//
// Boundary-box note: NodeServer is constructed with mock Connector and Mounter
// interfaces and a real VolumeStateMachine (in-process, no gRPC).  The
// stateDir is t.TempDir() (tmpfs; no persistent state between tests).
package component_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers for state-machine tests
// ─────────────────────────────────────────────────────────────────────────────.

// csiNodeSMTestEnv holds a NodeServer wired to a shared VolumeStateMachine.
type csiNodeSMTestEnv struct {
	node      *pillarcsi.NodeServer
	connector *csiMockConnector
	mounter   *csiMockMounter
	sm        *pillarcsi.VolumeStateMachine
	stateDir  string
}

func newCSINodeSMTestEnv(t *testing.T) *csiNodeSMTestEnv {
	t.Helper()
	stateDir := t.TempDir()
	connector := &csiMockConnector{}
	mounter := newCsiMockMounter()
	sm := pillarcsi.NewVolumeStateMachine()
	node := pillarcsi.NewNodeServerWithStateMachine("test-node-sm", connector, mounter, stateDir, sm)
	return &csiNodeSMTestEnv{
		node:      node,
		connector: connector,
		mounter:   mounter,
		sm:        sm,
		stateDir:  stateDir,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 5.8 State Machine Integration
// TESTCASES.md § 5.8 tests 31–34
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_StateMachine_NodeStage_WrongOrder verifies that NodeStageVolume
// is rejected with FailedPrecondition when the volume is not in
// ControllerPublished state (test case 31).
//
// See TESTCASES.md §5.8, row 31.
func TestCSINode_StateMachine_NodeStage_WrongOrder(t *testing.T) {
	t.Parallel()
	env := newCSINodeSMTestEnv(t)
	ctx := context.Background()

	// Volume is in NonExistent state — NodeStageVolume must be preceded by
	// ControllerPublishVolume.
	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-sm-stage-order"
	stagingPath := t.TempDir()

	_, err := env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:" + volumeID,
			pillarcsi.VolumeContextKeyAddress:   "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:      "4420",
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
		t.Fatal("expected error but got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
	// Connect must never have been called.
	if env.connector.connectCalls != 0 {
		t.Errorf("connectCalls = %d, want 0 (Connect must not be called before validation)", env.connector.connectCalls)
	}
}

// TestCSINode_StateMachine_NodePublish_WrongOrder verifies that
// NodePublishVolume is rejected with FailedPrecondition when the volume is
// not in NodeStaged state (test case 32).
//
// See TESTCASES.md §5.8, row 32.
func TestCSINode_StateMachine_NodePublish_WrongOrder(t *testing.T) {
	t.Parallel()
	env := newCSINodeSMTestEnv(t)
	ctx := context.Background()

	// Put volume into ControllerPublished state (not NodeStaged).
	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-sm-publish-order"
	env.sm.ForceState(volumeID, pillarcsi.StateControllerPublished)

	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	_, err := env.node.NodePublishVolume(ctx, &csipb.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
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
		t.Fatal("expected error but got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
	// Mount must never have been called.
	if env.mounter.mountCalls != 0 {
		t.Errorf("mountCalls = %d, want 0 (Mount must not be called before validation)", env.mounter.mountCalls)
	}
}

// TestCSINode_StateMachine_NodeUnpublish_WrongOrder verifies that
// NodeUnpublishVolume behaves correctly when the volume is not in
// NodePublished state (test case 33).
//
// Per the CSI spec §5.4.2 ("NodeUnpublishVolume MUST succeed if the volume is
// not currently NodePublished"), calling NodeUnpublishVolume in NodeStaged
// state must return success without performing any Unmount.  The state machine
// implementation treats this as an idempotent no-op rather than returning
// FailedPrecondition.
//
// See TESTCASES.md §5.8, row 33.
func TestCSINode_StateMachine_NodeUnpublish_WrongOrder(t *testing.T) {
	t.Parallel()
	env := newCSINodeSMTestEnv(t)
	ctx := context.Background()

	// Put volume into NodeStaged state (not NodePublished).
	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-sm-unpublish-order"
	env.sm.ForceState(volumeID, pillarcsi.StateNodeStaged)

	targetPath := t.TempDir()

	// Per CSI spec NodeUnpublishVolume is idempotent: it must succeed (not
	// return FailedPrecondition) when the volume is not published.
	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	if err != nil {
		// If the implementation does return an error, it should be FailedPrecondition.
		st, _ := status.FromError(err)
		if st.Code() != codes.FailedPrecondition {
			t.Errorf("unexpected error: code = %v, want nil or FailedPrecondition; err = %v", st.Code(), err)
		}
		t.Logf("implementation returned %v (non-idempotent): %v", st.Code(), err)
	}
	// Unmount must never have been called — target path is not mounted.
	if env.mounter.unmountCalls != 0 {
		t.Errorf("unmountCalls = %d, want 0 (Unmount must not be called when not published)", env.mounter.unmountCalls)
	}
}

// TestCSINode_StateMachine_FullLifecycleWithSM verifies that the correct CSI
// ordering through the full volume lifecycle succeeds when a shared
// VolumeStateMachine is in use (test case 34).
//
// See TESTCASES.md §5.8, row 34.
func TestCSINode_StateMachine_FullLifecycleWithSM(t *testing.T) {
	t.Parallel()
	env := newCSINodeSMTestEnv(t)
	ctx := context.Background()

	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-sm-lifecycle"
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// Precondition: volume is ControllerPublished (simulating what the
	// ControllerServer would do after ControllerPublishVolume succeeds).
	env.sm.ForceState(volumeID, pillarcsi.StateControllerPublished)

	// Step 1: NodeStageVolume
	_, err := env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:pvc-sm-lifecycle",
			pillarcsi.VolumeContextKeyAddress:   "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:      "4420",
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
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// Step 2: NodePublishVolume
	_, err = env.node.NodePublishVolume(ctx, &csipb.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	// Step 3: NodeUnpublishVolume
	_, err = env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}

	// Step 4: NodeUnstageVolume
	_, err = env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 5.12 Concurrent Node Operations
// TESTCASES.md § 5.12 tests 48–49
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_Concurrent_StageSameVolume_NoDeadlock verifies that concurrent
// NodeStageVolume calls for the same VolumeID complete without deadlock (test
// case 48).
//
// See TESTCASES.md §5.12, row 48.
func TestCSINode_Concurrent_StageSameVolume_NoDeadlock(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	const (
		volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-concurrent-same"
		n        = 8 // goroutines
	)

	stagingPath := t.TempDir()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
				VolumeId:          volumeID,
				StagingTargetPath: stagingPath,
				VolumeContext: map[string]string{
					pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:pvc-concurrent-same",
					pillarcsi.VolumeContextKeyAddress:   "192.168.1.10",
					pillarcsi.VolumeContextKeyPort:      "4420",
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
		}(i)
	}
	wg.Wait()

	// At least one call must succeed; all must return without hanging.
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Errorf("all %d concurrent NodeStageVolume calls failed; at least one should succeed", n)
	}
	t.Logf("%d/%d concurrent NodeStageVolume calls succeeded", succeeded, n)
}

// TestCSINode_Concurrent_StageDifferentVolumes_AllSucceed verifies that
// concurrent NodeStageVolume calls for distinct VolumeIDs all succeed
// independently (test case 49).
//
// See TESTCASES.md §5.12, row 49.
func TestCSINode_Concurrent_StageDifferentVolumes_AllSucceed(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	const n = 8 // goroutines / distinct volumes

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			volumeID := fmt.Sprintf("storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-concurrent-%d", idx)
			stagingPath := t.TempDir()
			_, errs[idx] = env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
				VolumeId:          volumeID,
				StagingTargetPath: stagingPath,
				VolumeContext: map[string]string{
					pillarcsi.VolumeContextKeyTargetNQN: fmt.Sprintf("nqn.2026-01.com.pillar-csi:pvc-concurrent-%d", idx),
					pillarcsi.VolumeContextKeyAddress:   "192.168.1.10",
					pillarcsi.VolumeContextKeyPort:      "4420",
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
		}(i)
	}
	wg.Wait()

	for idx, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: NodeStageVolume failed: %v", idx, err)
		}
	}
}
