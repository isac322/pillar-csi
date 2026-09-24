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

// Tests for NodeUnstageVolume after the node plugin restarts between
// NodeStageVolume and NodeUnstageVolume.
//
// A restart is modeled as a fresh NodeServer that shares the host mount
// table (the mockMounter) with the server that staged the volume.  The stage
// state directory is either the same one (host-persisted) or a new empty one
// (state lost with the old pod's container filesystem).
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodeUnstageVolume_AfterNodeRestart

import (
	"context"
	"errors"
	"slices"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// restartTestMode describes one CSI access type for the restart tests.
type restartTestMode struct {
	name    string
	volCap  *csi.VolumeCapability
	target  func(stagingPath string) string
	probing func(stagingPath string) []string // IsMounted targets expected when state is lost
}

func restartTestModes() []restartTestMode {
	return []restartTestMode{
		{
			name:   "Filesystem",
			volCap: mountCap("ext4"),
			target: func(p string) string { return p },
			// The Block child sits inside the mounted filesystem; probing it
			// can return EIO, so a mounted root must stop the probe.
			probing: func(p string) []string { return []string{p} },
		},
		{
			name:    "Block",
			volCap:  blockCap(),
			target:  blockStagingDevicePath,
			probing: func(p string) []string { return []string{p, blockStagingDevicePath(p)} },
		},
	}
}

// restartFixture is a NodeServer freshly constructed after another server
// staged a volume, together with the mocks it observes through. The mounter
// is shared with the staging server (the host mount table survives restarts).
type restartFixture struct {
	srv         *NodeServer
	connector   *mockConnector
	mounter     *mockMounter
	stagingPath string
}

// stageThenRestart stages a volume on one NodeServer and returns a fixture
// around a second, freshly constructed NodeServer that shares the host mount
// table.  When keepState is true the new server uses the same state directory.
func stageThenRestart(
	t *testing.T, volumeID, nqn string, volCap *csi.VolumeCapability, keepState bool,
) *restartFixture {
	t.Helper()
	mnt := newMockMounter()
	stateDir := t.TempDir()
	stagingPath := t.TempDir()

	before := NewNodeServerWithStateDir("test-node", &mockConnector{devicePath: "/dev/nvme0n1"}, mnt, stateDir)
	if _, err := before.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  volCap,
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	}); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	if !keepState {
		stateDir = t.TempDir()
	}
	conn := &mockConnector{devicePath: "/dev/nvme0n1"}
	mnt.isMountedCalls = nil
	mnt.unmountCalls = nil
	return &restartFixture{
		srv:         NewNodeServerWithStateDir("test-node", conn, mnt, stateDir),
		connector:   conn,
		mounter:     mnt,
		stagingPath: stagingPath,
	}
}

// TestNodeUnstageVolume_AfterNodeRestart_StatePersisted verifies that a node
// plugin restarted with the same state directory tears down a volume staged
// by its predecessor: the staged target is unmounted, the transport session
// is detached with the persisted NQN, and the state file is removed.
func TestNodeUnstageVolume_AfterNodeRestart_StatePersisted(t *testing.T) {
	t.Parallel()
	for _, mode := range restartTestModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			const (
				volumeID = "tank/pvc-restart-persisted"
				nqn      = "nqn.test:restart-persisted"
			)
			fx := stageThenRestart(t, volumeID, nqn, mode.volCap, true)
			target := mode.target(fx.stagingPath)

			if _, err := fx.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
				VolumeId:          volumeID,
				StagingTargetPath: fx.stagingPath,
			}); err != nil {
				t.Fatalf("NodeUnstageVolume after restart: %v", err)
			}

			if fx.mounter.mountedPaths[target] {
				t.Errorf("%q still mounted after NodeUnstageVolume", target)
			}
			if !slices.Equal(fx.connector.disconnectCalls, []string{nqn}) {
				t.Errorf("Disconnect calls = %v, want [%s]", fx.connector.disconnectCalls, nqn)
			}
			if remaining, err := fx.srv.readStageState(volumeID); err != nil || remaining != nil {
				t.Errorf("stage state after unstage = %+v (err %v), want none", remaining, err)
			}
		})
	}
}

// TestNodeUnstageVolume_AfterNodeRestart_StateLost verifies that a node
// plugin restarted without the stage state file refuses to report a still-
// mounted volume as unstaged.  Reporting success would let the CO consider
// the volume detached while the mount and transport session leak.  Nothing
// may be unmounted or detached, and the Filesystem root must be probed before
// (and instead of) the Block child inside it.
func TestNodeUnstageVolume_AfterNodeRestart_StateLost(t *testing.T) {
	t.Parallel()
	for _, mode := range restartTestModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			const (
				volumeID = "tank/pvc-restart-lost"
				nqn      = "nqn.test:restart-lost"
			)
			fx := stageThenRestart(t, volumeID, nqn, mode.volCap, false)
			target := mode.target(fx.stagingPath)

			_, err := fx.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
				VolumeId:          volumeID,
				StagingTargetPath: fx.stagingPath,
			})
			requireGRPCCode(t, err, codes.Internal)

			if !fx.mounter.mountedPaths[target] {
				t.Errorf("%q was unmounted without stage state", target)
			}
			if len(fx.mounter.unmountCalls) != 0 {
				t.Errorf("Unmount calls = %v, want none", fx.mounter.unmountCalls)
			}
			if len(fx.connector.disconnectCalls) != 0 {
				t.Errorf("Disconnect calls = %v, want none", fx.connector.disconnectCalls)
			}
			if want := mode.probing(fx.stagingPath); !slices.Equal(fx.mounter.isMountedCalls, want) {
				t.Errorf("IsMounted calls = %v, want %v", fx.mounter.isMountedCalls, want)
			}
		})
	}
}

// TestNodeUnstageVolume_NoState_MountProbeErrorFails verifies that when the
// stage state file is missing, a failure to determine whether the staging
// path is mounted is reported rather than treated as "not staged".
func TestNodeUnstageVolume_NoState_MountProbeErrorFails(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.mounter.isMountedErr = errors.New("mountinfo unreadable")

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "tank/pvc-no-state-probe-err",
		StagingTargetPath: t.TempDir(),
	})
	requireGRPCCode(t, err, codes.Internal)
}
