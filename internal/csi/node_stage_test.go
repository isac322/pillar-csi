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

// Tests for NodeStageVolume and NodeUnstageVolume.
//
// All tests use injectable mock Connector and Mounter implementations so no
// NVMe-oF kernel modules, real block devices, or root privileges are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodeStage

import (
	"context"
	"errors"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock Connector
// ─────────────────────────────────────────────────────────────────────────────

// mockConnector is a test double for the Connector interface.
// It records every call and returns pre-programmed responses.
type mockConnector struct {
	// connectErr is returned by Connect, or nil for success.
	connectErr error
	// disconnectErr is returned by Disconnect, or nil for success.
	disconnectErr error
	// devicePath is returned by GetDevicePath (non-empty means device ready).
	devicePath string
	// devicePathErr is returned by GetDevicePath instead of devicePath.
	devicePathErr error

	// Recorded calls.
	connectCalls    []connectCall
	disconnectCalls []string // NQNs
	getDeviceCalls  []string // NQNs
}

type connectCall struct {
	subsysNQN string
	trAddr    string
	trSvcID   string
}

func (m *mockConnector) Connect(_ context.Context, subsysNQN, trAddr, trSvcID string) error {
	m.connectCalls = append(m.connectCalls, connectCall{subsysNQN, trAddr, trSvcID})
	return m.connectErr
}

func (m *mockConnector) Disconnect(_ context.Context, subsysNQN string) error {
	m.disconnectCalls = append(m.disconnectCalls, subsysNQN)
	return m.disconnectErr
}

func (m *mockConnector) GetDevicePath(_ context.Context, subsysNQN string) (string, error) {
	m.getDeviceCalls = append(m.getDeviceCalls, subsysNQN)
	if m.devicePathErr != nil {
		return "", m.devicePathErr
	}
	return m.devicePath, nil
}

// Compile-time interface check.
var _ Connector = (*mockConnector)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Mock Mounter
// ─────────────────────────────────────────────────────────────────────────────

// mockMounter is a test double for the Mounter interface.
// It records every call and maintains a simple in-memory mount table.
type mockMounter struct {
	// mountedPaths is the set of paths currently "mounted".
	mountedPaths map[string]bool

	// errors to return per method (nil = success).
	formatAndMountErr error
	mountErr          error
	unmountErr        error
	isMountedErr      error

	// Recorded calls.
	formatAndMountCalls []formatAndMountCall
	mountCalls          []mountCall
	unmountCalls        []string
}

type formatAndMountCall struct {
	source, target, fsType string
	options                []string
}

type mountCall struct {
	source, target, fsType string
	options                []string
}

func newMockMounter() *mockMounter {
	return &mockMounter{mountedPaths: make(map[string]bool)}
}

func (m *mockMounter) FormatAndMount(source, target, fsType string, options []string) error {
	m.formatAndMountCalls = append(m.formatAndMountCalls, formatAndMountCall{source, target, fsType, options})
	if m.formatAndMountErr != nil {
		return m.formatAndMountErr
	}
	m.mountedPaths[target] = true
	return nil
}

func (m *mockMounter) Mount(source, target, fsType string, options []string) error {
	m.mountCalls = append(m.mountCalls, mountCall{source, target, fsType, options})
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mountedPaths[target] = true
	return nil
}

func (m *mockMounter) Unmount(target string) error {
	m.unmountCalls = append(m.unmountCalls, target)
	if m.unmountErr != nil {
		return m.unmountErr
	}
	delete(m.mountedPaths, target)
	return nil
}

func (m *mockMounter) IsMounted(target string) (bool, error) {
	if m.isMountedErr != nil {
		return false, m.isMountedErr
	}
	return m.mountedPaths[target], nil
}

// Compile-time interface check.
var _ Mounter = (*mockMounter)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// nodeTestEnv holds the pieces needed for a single node service test.
type nodeTestEnv struct {
	srv       *NodeServer
	connector *mockConnector
	mounter   *mockMounter
	stateDir  string
}

func newNodeTestEnv(t *testing.T) *nodeTestEnv {
	t.Helper()
	conn := &mockConnector{devicePath: "/dev/nvme0n1"}
	mnt := newMockMounter()
	stateDir := t.TempDir()
	srv := NewNodeServerWithStateDir("test-node", conn, mnt, stateDir)
	return &nodeTestEnv{srv: srv, connector: conn, mounter: mnt, stateDir: stateDir}
}

// mountVolumeContext returns a VolumeContext map with the three required keys.
func mountVolumeContext(nqn, addr, port string) map[string]string {
	return map[string]string{
		VolumeContextKeyTargetNQN: nqn,
		VolumeContextKeyAddress:   addr,
		VolumeContextKeyPort:      port,
	}
}

// mountCap returns a VolumeCapability for filesystem mount access.
func mountCap(fsType string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// blockCap returns a VolumeCapability for raw block access.
func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{
			Block: &csi.VolumeCapability_BlockVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// requireCode fatally fails t if err does not carry the expected gRPC code.
func requireGRPCCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != want {
		t.Errorf("gRPC code = %v, want %v (msg: %q)", st.Code(), want, st.Message())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_* – happy-path and validation tests
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_MountAccess exercises the MOUNT access type: after
// NodeStageVolume the staging path should be mounted and a state file written.
func TestNodeStageVolume_MountAccess(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const (
		volumeID = "tank/pvc-test"
		nqn      = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-test"
		addr     = "192.0.2.1"
		port     = "4420"
	)

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(nqn, addr, port),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// Connector must have been called with correct NQN and address.
	if len(env.connector.connectCalls) != 1 {
		t.Fatalf("Connect called %d times, want 1", len(env.connector.connectCalls))
	}
	call := env.connector.connectCalls[0]
	if call.subsysNQN != nqn {
		t.Errorf("Connect subsysNQN = %q, want %q", call.subsysNQN, nqn)
	}
	if call.trAddr != addr {
		t.Errorf("Connect trAddr = %q, want %q", call.trAddr, addr)
	}
	if call.trSvcID != port {
		t.Errorf("Connect trSvcID = %q, want %q", call.trSvcID, port)
	}

	// Staging path must be mounted.
	mounted, _ := env.mounter.IsMounted(stagingPath)
	if !mounted {
		t.Error("staging path not mounted after NodeStageVolume")
	}

	// FormatAndMount must have been called once.
	if len(env.mounter.formatAndMountCalls) != 1 {
		t.Errorf("FormatAndMount called %d times, want 1", len(env.mounter.formatAndMountCalls))
	}
	fm := env.mounter.formatAndMountCalls[0]
	if fm.fsType != "ext4" {
		t.Errorf("FormatAndMount fsType = %q, want %q", fm.fsType, "ext4")
	}
	if fm.source != env.connector.devicePath {
		t.Errorf("FormatAndMount source = %q, want %q", fm.source, env.connector.devicePath)
	}
	if fm.target != stagingPath {
		t.Errorf("FormatAndMount target = %q, want %q", fm.target, stagingPath)
	}

	// State file must exist with correct NQN.
	state, readErr := env.srv.readStageState(volumeID)
	if readErr != nil {
		t.Fatalf("readStageState: %v", readErr)
	}
	if state == nil {
		t.Fatal("stage state is nil after NodeStageVolume")
	}
	if state.SubsysNQN != nqn {
		t.Errorf("state.SubsysNQN = %q, want %q", state.SubsysNQN, nqn)
	}
}

// TestNodeStageVolume_DefaultFsType verifies that an empty fsType in the
// VolumeCapability falls back to the default (ext4).
func TestNodeStageVolume_DefaultFsType(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-fs-default",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap(""), // empty fsType
		VolumeContext:     mountVolumeContext("nqn.test:vol", "10.0.0.1", "4420"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	if len(env.mounter.formatAndMountCalls) == 0 {
		t.Fatal("FormatAndMount not called")
	}
	if got := env.mounter.formatAndMountCalls[0].fsType; got != defaultFsType {
		t.Errorf("fsType = %q, want default %q", got, defaultFsType)
	}
}

// TestNodeStageVolume_BlockAccess exercises the BLOCK access type: the staging
// path should receive a bind mount of the raw device.
func TestNodeStageVolume_BlockAccess(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const nqn = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-block"

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-block",
		StagingTargetPath: stagingPath,
		VolumeCapability:  blockCap(),
		VolumeContext:     mountVolumeContext(nqn, "10.0.0.1", "4420"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume (block): %v", err)
	}

	// Mount (not FormatAndMount) must have been called with "bind" option.
	if len(env.mounter.mountCalls) != 1 {
		t.Fatalf("Mount called %d times, want 1", len(env.mounter.mountCalls))
	}
	mc := env.mounter.mountCalls[0]
	if mc.source != env.connector.devicePath {
		t.Errorf("Mount source = %q, want %q", mc.source, env.connector.devicePath)
	}
	hasBindOpt := false
	for _, o := range mc.options {
		if o == "bind" {
			hasBindOpt = true
		}
	}
	if !hasBindOpt {
		t.Errorf("Mount options %v do not contain 'bind'", mc.options)
	}

	// FormatAndMount must NOT have been called for block access.
	if len(env.mounter.formatAndMountCalls) != 0 {
		t.Errorf("FormatAndMount called %d times for block access, want 0", len(env.mounter.formatAndMountCalls))
	}
}

// TestNodeStageVolume_Idempotent verifies that calling NodeStageVolume twice
// on a fully staged volume returns success without re-connecting or re-mounting.
func TestNodeStageVolume_Idempotent(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-idem",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:idem", "10.0.0.1", "4420"),
	}

	// First call: performs full staging.
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}
	connectCount1 := len(env.connector.connectCalls)
	fmCount1 := len(env.mounter.formatAndMountCalls)

	// Second call: already staged → must return success without extra work.
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("second NodeStageVolume (idempotent): %v", err)
	}
	if got := len(env.connector.connectCalls); got != connectCount1 {
		t.Errorf("Connect called again on idempotent stage: count went %d → %d", connectCount1, got)
	}
	if got := len(env.mounter.formatAndMountCalls); got != fmCount1 {
		t.Errorf("FormatAndMount called again on idempotent stage: count went %d → %d", fmCount1, got)
	}
}

// TestNodeStageVolume_IdempotentAfterUnmount verifies that if the state file
// exists but the staging path is no longer mounted (e.g. after a node reboot),
// NodeStageVolume re-mounts without error.
func TestNodeStageVolume_IdempotentAfterUnmount(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const volumeID = "tank/pvc-remount"
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:remount", "10.0.0.2", "4420"),
	}

	// Initial stage.
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("initial stage: %v", err)
	}

	// Simulate unmount (e.g. node reboot) by directly clearing the mock's
	// mounted map without going through Unmount (which would be called by
	// NodeUnstageVolume in a normal teardown).
	delete(env.mounter.mountedPaths, stagingPath)

	// Re-stage: must succeed and re-mount.
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	mounted, _ := env.mounter.IsMounted(stagingPath)
	if !mounted {
		t.Error("staging path not mounted after re-stage")
	}
	if len(env.mounter.formatAndMountCalls) != 2 {
		t.Errorf("FormatAndMount call count = %d, want 2 (initial + re-stage)", len(env.mounter.formatAndMountCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_* – validation / error tests
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeStageVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		StagingTargetPath: "/mnt/stage",
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:x", "10.0.0.1", "4420"),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_MissingStagingPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:         "tank/pvc",
		VolumeCapability: mountCap("ext4"),
		VolumeContext:    mountVolumeContext("nqn.test:x", "10.0.0.1", "4420"),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_MissingCapability(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc",
		StagingTargetPath: "/mnt/stage",
		VolumeContext:     mountVolumeContext("nqn.test:x", "10.0.0.1", "4420"),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_MissingNQN(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     map[string]string{VolumeContextKeyAddress: "10.0.0.1", VolumeContextKeyPort: "4420"},
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_MissingAddress(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     map[string]string{VolumeContextKeyTargetNQN: "nqn.test:x", VolumeContextKeyPort: "4420"},
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_MissingPort(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     map[string]string{VolumeContextKeyTargetNQN: "nqn.test:x", VolumeContextKeyAddress: "10.0.0.1"},
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_NoAccessType(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc",
		StagingTargetPath: t.TempDir(),
		VolumeCapability: &csi.VolumeCapability{
			// No AccessType set.
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		VolumeContext: mountVolumeContext("nqn.test:x", "10.0.0.1", "4420"),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_ConnectError(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.connector.connectErr = errors.New("network unreachable")

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-err",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:err", "10.0.0.1", "4420"),
	})
	requireGRPCCode(t, err, codes.Internal)
}

func TestNodeStageVolume_DevicePathError(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.connector.devicePathErr = errors.New("sysfs error")

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-deverr",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:deverr", "10.0.0.1", "4420"),
	})
	requireGRPCCode(t, err, codes.Internal)
}

func TestNodeStageVolume_DeviceNeverAppears(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	// GetDevicePath always returns ("", nil) → device never appears.
	env.connector.devicePath = ""

	ctx, cancel := context.WithTimeout(context.Background(), devicePollInterval*3)
	defer cancel()

	_, err := env.srv.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-nodv",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:nodv", "10.0.0.1", "4420"),
	})
	// Expect DeadlineExceeded because the polling loop times out.
	if err == nil {
		t.Fatal("expected error when device never appears, got nil")
	}
	// Accept either DeadlineExceeded (our poll timeout) or Internal
	// (context cancelled), since the test uses a tight deadline.
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.DeadlineExceeded && st.Code() != codes.Internal {
		t.Errorf("expected DeadlineExceeded or Internal, got %v", st.Code())
	}
}

func TestNodeStageVolume_FormatAndMountError(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.mounter.formatAndMountErr = errors.New("mkfs.ext4 failed")

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-fmerr",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:fmerr", "10.0.0.1", "4420"),
	})
	requireGRPCCode(t, err, codes.Internal)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_* – happy-path and validation tests
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeUnstageVolume_RoundTrip exercises the full stage→unstage lifecycle.
func TestNodeUnstageVolume_RoundTrip(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const (
		volumeID = "tank/pvc-roundtrip"
		nqn      = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-roundtrip"
	)

	// Stage.
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(nqn, "10.0.0.1", "4420"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// Unstage.
	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	// Staging path must be unmounted.
	mounted, _ := env.mounter.IsMounted(stagingPath)
	if mounted {
		t.Error("staging path still mounted after NodeUnstageVolume")
	}

	// Connector must have received exactly one Disconnect for the NQN.
	if len(env.connector.disconnectCalls) != 1 {
		t.Fatalf("Disconnect called %d times, want 1", len(env.connector.disconnectCalls))
	}
	if env.connector.disconnectCalls[0] != nqn {
		t.Errorf("Disconnect NQN = %q, want %q", env.connector.disconnectCalls[0], nqn)
	}

	// State file must be removed.
	state, readErr := env.srv.readStageState(volumeID)
	if readErr != nil {
		t.Fatalf("readStageState after unstage: %v", readErr)
	}
	if state != nil {
		t.Error("stage state still present after NodeUnstageVolume")
	}
}

// TestNodeUnstageVolume_Idempotent verifies that calling NodeUnstageVolume on
// a volume that was never staged (or already unstaged) returns success.
func TestNodeUnstageVolume_Idempotent(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()

	// No prior NodeStageVolume — call NodeUnstageVolume directly.
	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "tank/pvc-never-staged",
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume on unstaged volume: %v", err)
	}

	// No disconnect should have been attempted.
	if len(env.connector.disconnectCalls) != 0 {
		t.Errorf("Disconnect called %d times for unstaged volume, want 0", len(env.connector.disconnectCalls))
	}
}

// TestNodeUnstageVolume_IdempotentSecondCall verifies that calling
// NodeUnstageVolume a second time after a clean first unstage succeeds.
func TestNodeUnstageVolume_IdempotentSecondCall(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const volumeID = "tank/pvc-double-unstage"

	// Stage.
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:double", "10.0.0.1", "4420"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// First unstage.
	if _, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagingPath,
	}); err != nil {
		t.Fatalf("first NodeUnstageVolume: %v", err)
	}

	// Second unstage (idempotent).
	if _, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagingPath,
	}); err != nil {
		t.Fatalf("second NodeUnstageVolume (idempotent): %v", err)
	}

	// Disconnect should still have been called only once (for the first unstage).
	if len(env.connector.disconnectCalls) != 1 {
		t.Errorf("Disconnect called %d times, want 1", len(env.connector.disconnectCalls))
	}
}

// TestNodeUnstageVolume_UnmountedPath verifies that NodeUnstageVolume succeeds
// even when the staging path is not currently mounted (e.g., the CO already
// cleaned it up or the node rebooted).
func TestNodeUnstageVolume_UnmountedPath(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const (
		volumeID = "tank/pvc-unmounted"
		nqn      = "nqn.test:unmounted"
	)

	// Stage.
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(nqn, "10.0.0.1", "4420"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// Simulate the staging path being already unmounted (node reboot scenario).
	delete(env.mounter.mountedPaths, stagingPath)

	// Unstage should still succeed.
	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume with unmounted path: %v", err)
	}

	// Disconnect must still have been called.
	if len(env.connector.disconnectCalls) != 1 || env.connector.disconnectCalls[0] != nqn {
		t.Errorf("Disconnect calls = %v, want [%s]", env.connector.disconnectCalls, nqn)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_* – error tests
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeUnstageVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		StagingTargetPath: "/mnt/stage",
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeUnstageVolume_MissingStagingPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: "tank/pvc",
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeUnstageVolume_UnmountError(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.mounter.unmountErr = errors.New("device busy")
	stagingPath := t.TempDir()

	// Stage first so the path ends up in the mounted set.
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-umerr",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:umerr", "10.0.0.1", "4420"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "tank/pvc-umerr",
		StagingTargetPath: stagingPath,
	})
	requireGRPCCode(t, err, codes.Internal)
}

func TestNodeUnstageVolume_DisconnectError(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.connector.disconnectErr = errors.New("NVMe transport error")
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-diserr",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:diserr", "10.0.0.1", "4420"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "tank/pvc-diserr",
		StagingTargetPath: stagingPath,
	})
	requireGRPCCode(t, err, codes.Internal)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestStageState_Helpers – unit tests for state file helpers
// ─────────────────────────────────────────────────────────────────────────────

// TestStageState_WriteReadDelete verifies the state file roundtrip.
func TestStageState_WriteReadDelete(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	srv := NewNodeServerWithStateDir("n", nil, nil, stateDir)

	const volumeID = "pool/vol-1"
	want := &nodeStageState{SubsysNQN: "nqn.test:pool.vol-1"}

	if err := srv.writeStageState(volumeID, want); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}

	got, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if got == nil || got.SubsysNQN != want.SubsysNQN {
		t.Errorf("readStageState = %+v, want %+v", got, want)
	}

	if err := srv.deleteStageState(volumeID); err != nil {
		t.Fatalf("deleteStageState: %v", err)
	}

	// After deletion, readStageState must return nil.
	afterDelete, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState after delete: %v", err)
	}
	if afterDelete != nil {
		t.Errorf("expected nil after deleteStageState, got %+v", afterDelete)
	}
}

// TestStageState_DeleteIdempotent verifies that deleting a non-existent state
// file succeeds silently.
func TestStageState_DeleteIdempotent(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("n", nil, nil, t.TempDir())
	if err := srv.deleteStageState("pool/nonexistent"); err != nil {
		t.Errorf("deleteStageState on missing file: %v", err)
	}
}

// TestStageState_VolumeIDSanitization verifies that volume IDs containing
// slashes produce distinct state file names without path traversal issues.
func TestStageState_VolumeIDSanitization(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	srv := NewNodeServerWithStateDir("n", nil, nil, stateDir)

	ids := []string{"pool/vol-a", "pool/vol-b", "other-pool/vol-c"}
	for _, id := range ids {
		if err := srv.writeStageState(id, &nodeStageState{SubsysNQN: "nqn.test:" + id}); err != nil {
			t.Fatalf("writeStageState(%q): %v", id, err)
		}
	}

	// Each ID must produce an independently readable state.
	for _, id := range ids {
		st, err := srv.readStageState(id)
		if err != nil {
			t.Errorf("readStageState(%q): %v", id, err)
		}
		if st == nil {
			t.Errorf("readStageState(%q) = nil", id)
		}
	}
}
