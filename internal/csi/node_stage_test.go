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
	"os"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock Connector
// ─────────────────────────────────────────────────────────────────────────────.

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
// ─────────────────────────────────────────────────────────────────────────────.

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
// ─────────────────────────────────────────────────────────────────────────────.

// testStorageAddr is the storage-target IP address used in tests that need a
// concrete address but do not care about the exact value.
const testStorageAddr = "10.0.0.1"

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
// Port is always "4420" (the NVMe-oF/iSCSI port used in all block-protocol tests).
func mountVolumeContext(nqn, addr string) map[string]string {
	return map[string]string{
		VolumeContextKeyTargetID: nqn,
		VolumeContextKeyAddress:  addr,
		VolumeContextKeyPort:     "4420",
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
// ─────────────────────────────────────────────────────────────────────────────.

// TestNodeStageVolume_MountAccess exercises the MOUNT access type: after
// NodeStageVolume the staging path should be mounted and a state file written.
func TestNodeStageVolume_MountAccess(t *testing.T) { //nolint:gocyclo // multiple assertions on stage state
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
		VolumeContext:     mountVolumeContext(nqn, addr),
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
	mounted, _ := env.mounter.IsMounted(stagingPath) //nolint:errcheck // mock never returns an error
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
	if state.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("state.ProtocolType = %q, want %q", state.ProtocolType, "nvmeof-tcp")
	}
	if state.NVMeoF == nil {
		t.Fatal("state.NVMeoF is nil after NodeStageVolume")
	}
	if state.NVMeoF.SubsysNQN != nqn {
		t.Errorf("state.NVMeoF.SubsysNQN = %q, want %q", state.NVMeoF.SubsysNQN, nqn)
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
		VolumeContext:     mountVolumeContext("nqn.test:vol", testStorageAddr),
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
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
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
		VolumeContext:     mountVolumeContext("nqn.test:idem", testStorageAddr),
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
		VolumeContext:     mountVolumeContext("nqn.test:remount", "10.0.0.2"),
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
	mounted, _ := env.mounter.IsMounted(stagingPath) //nolint:errcheck // mock never returns an error
	if !mounted {
		t.Error("staging path not mounted after re-stage")
	}
	if len(env.mounter.formatAndMountCalls) != 2 {
		t.Errorf("FormatAndMount call count = %d, want 2 (initial + re-stage)", len(env.mounter.formatAndMountCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_* – validation / error tests
// ─────────────────────────────────────────────────────────────────────────────.

func TestNodeStageVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		StagingTargetPath: "/mnt/stage",
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:x", testStorageAddr),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_MissingStagingPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:         "tank/pvc",
		VolumeCapability: mountCap("ext4"),
		VolumeContext:    mountVolumeContext("nqn.test:x", testStorageAddr),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeStageVolume_MissingCapability(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc",
		StagingTargetPath: "/mnt/stage",
		VolumeContext:     mountVolumeContext("nqn.test:x", testStorageAddr),
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
		VolumeContext:     map[string]string{VolumeContextKeyAddress: testStorageAddr, VolumeContextKeyPort: "4420"},
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
		VolumeContext:     map[string]string{VolumeContextKeyTargetID: "nqn.test:x", VolumeContextKeyPort: "4420"},
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
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: "nqn.test:x",
			VolumeContextKeyAddress:  testStorageAddr,
		},
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
		VolumeContext: mountVolumeContext("nqn.test:x", testStorageAddr),
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
		VolumeContext:     mountVolumeContext("nqn.test:err", testStorageAddr),
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
		VolumeContext:     mountVolumeContext("nqn.test:deverr", testStorageAddr),
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
		VolumeContext:     mountVolumeContext("nqn.test:nodv", testStorageAddr),
	})
	// Expect DeadlineExceeded because the polling loop times out.
	if err == nil {
		t.Fatal("expected error when device never appears, got nil")
	}
	// Accept either DeadlineExceeded (our poll timeout) or Internal
	// (context canceled), since the test uses a tight deadline.
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
		VolumeContext:     mountVolumeContext("nqn.test:fmerr", testStorageAddr),
	})
	requireGRPCCode(t, err, codes.Internal)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_* – happy-path and validation tests
// ─────────────────────────────────────────────────────────────────────────────.

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
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
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
	mounted, _ := env.mounter.IsMounted(stagingPath) //nolint:errcheck // mock never returns an error
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
		VolumeContext:     mountVolumeContext("nqn.test:double", testStorageAddr),
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
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
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
// ─────────────────────────────────────────────────────────────────────────────.

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
		VolumeContext:     mountVolumeContext("nqn.test:umerr", testStorageAddr),
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
		VolumeContext:     mountVolumeContext("nqn.test:diserr", testStorageAddr),
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
// ─────────────────────────────────────────────────────────────────────────────.

// TestStageState_WriteReadDelete verifies the state file roundtrip.
func TestStageState_WriteReadDelete(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	srv := NewNodeServerWithStateDir("n", nil, nil, stateDir)

	const volumeID = "pool/vol-1"
	want := &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF:       &NVMeoFStageState{SubsysNQN: "nqn.test:pool.vol-1"},
	}

	if err := srv.writeStageState(volumeID, want); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}

	got, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if got == nil || got.NVMeoF == nil || got.NVMeoF.SubsysNQN != want.NVMeoF.SubsysNQN {
		t.Errorf("readStageState = %+v, want %+v", got, want)
	}

	err = srv.deleteStageState(volumeID)
	if err != nil {
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
		st := &nodeStageState{
			ProtocolType: ProtocolNVMeoFTCP,
			NVMeoF:       &NVMeoFStageState{SubsysNQN: "nqn.test:" + id},
		}
		if err := srv.writeStageState(id, st); err != nil {
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

// ─────────────────────────────────────────────────────────────────────────────
// Discriminated union serialization tests (AC 5)
// ─────────────────────────────────────────────────────────────────────────────

// TestStageState_DiscriminatedUnion_NVMeoF verifies that a full NVMeoF state
// survives a write→read roundtrip with all fields intact.
func TestStageState_DiscriminatedUnion_NVMeoF(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("n", nil, nil, t.TempDir())
	const volumeID = "pool/nvme-vol"

	want := &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: "nqn.2024-01.com.example:vol1",
			Address:   "192.168.1.10",
			Port:      "4420",
		},
	}

	if err := srv.writeStageState(volumeID, want); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}
	got, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if got == nil {
		t.Fatal("readStageState returned nil")
	}
	if got.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q, want %q", got.ProtocolType, "nvmeof-tcp")
	}
	if got.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil")
	}
	if got.NVMeoF.SubsysNQN != want.NVMeoF.SubsysNQN {
		t.Errorf("SubsysNQN = %q, want %q", got.NVMeoF.SubsysNQN, want.NVMeoF.SubsysNQN)
	}
	if got.NVMeoF.Address != want.NVMeoF.Address {
		t.Errorf("Address = %q, want %q", got.NVMeoF.Address, want.NVMeoF.Address)
	}
	if got.NVMeoF.Port != want.NVMeoF.Port {
		t.Errorf("Port = %q, want %q", got.NVMeoF.Port, want.NVMeoF.Port)
	}
	// Other protocol sub-structs must remain nil.
	if got.ISCSI != nil {
		t.Errorf("ISCSI = %+v, want nil", got.ISCSI)
	}
	if got.NFS != nil {
		t.Errorf("NFS = %+v, want nil", got.NFS)
	}
	if got.SMB != nil {
		t.Errorf("SMB = %+v, want nil", got.SMB)
	}
}

// TestStageState_DiscriminatedUnion_NFS verifies that an NFS state file
// roundtrips correctly and only the NFS sub-struct is populated.
func TestStageState_DiscriminatedUnion_NFS(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("n", nil, nil, t.TempDir())
	const volumeID = "pool/nfs-vol"

	want := &nodeStageState{
		ProtocolType: "nfs",
		NFS: &NFSStageState{
			Server:     "192.168.1.20",
			ExportPath: "/mnt/tank/pvc-abc123",
		},
	}

	if err := srv.writeStageState(volumeID, want); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}
	got, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if got == nil {
		t.Fatal("readStageState returned nil")
	}
	if got.ProtocolType != "nfs" {
		t.Errorf("ProtocolType = %q, want %q", got.ProtocolType, "nfs")
	}
	if got.NFS == nil {
		t.Fatal("NFS sub-struct is nil")
	}
	if got.NFS.Server != want.NFS.Server {
		t.Errorf("Server = %q, want %q", got.NFS.Server, want.NFS.Server)
	}
	if got.NFS.ExportPath != want.NFS.ExportPath {
		t.Errorf("ExportPath = %q, want %q", got.NFS.ExportPath, want.NFS.ExportPath)
	}
	if got.NVMeoF != nil {
		t.Errorf("NVMeoF = %+v, want nil", got.NVMeoF)
	}
}

// TestStageState_DiscriminatedUnion_ISCSI verifies iSCSI state roundtrip.
func TestStageState_DiscriminatedUnion_ISCSI(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("n", nil, nil, t.TempDir())
	const volumeID = "pool/iscsi-vol"

	want := &nodeStageState{
		ProtocolType: "iscsi",
		ISCSI: &ISCSIStageState{
			TargetIQN: "iqn.2024-01.com.example:vol1",
			Portal:    "192.168.1.30:3260",
			LUN:       0,
		},
	}

	if err := srv.writeStageState(volumeID, want); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}
	got, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if got == nil || got.ISCSI == nil {
		t.Fatal("iSCSI state not restored after roundtrip")
	}
	if got.ProtocolType != "iscsi" {
		t.Errorf("ProtocolType = %q, want %q", got.ProtocolType, "iscsi")
	}
	if got.ISCSI.TargetIQN != want.ISCSI.TargetIQN {
		t.Errorf("TargetIQN = %q, want %q", got.ISCSI.TargetIQN, want.ISCSI.TargetIQN)
	}
	if got.ISCSI.Portal != want.ISCSI.Portal {
		t.Errorf("Portal = %q, want %q", got.ISCSI.Portal, want.ISCSI.Portal)
	}
}

// TestStageState_DiscriminatedUnion_SMB verifies SMB state roundtrip.
func TestStageState_DiscriminatedUnion_SMB(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("n", nil, nil, t.TempDir())
	const volumeID = "pool/smb-vol"

	want := &nodeStageState{
		ProtocolType: "smb",
		SMB: &SMBStageState{
			Server: "192.168.1.40",
			Share:  "pvc-smb1",
		},
	}

	if err := srv.writeStageState(volumeID, want); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}
	got, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if got == nil || got.SMB == nil {
		t.Fatal("SMB state not restored after roundtrip")
	}
	if got.SMB.Server != want.SMB.Server {
		t.Errorf("Server = %q, want %q", got.SMB.Server, want.SMB.Server)
	}
	if got.SMB.Share != want.SMB.Share {
		t.Errorf("Share = %q, want %q", got.SMB.Share, want.SMB.Share)
	}
}

// TestStageState_LegacyMigration verifies that a pre-Phase2 state file
// (containing only {"subsys_nqn": "…"}) is detected and migrated to the
// discriminated union format on first read.
func TestStageState_LegacyMigration(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	srv := NewNodeServerWithStateDir("n", nil, nil, stateDir)
	const (
		volumeID = "pool/legacy-vol"
		nqn      = "nqn.2024-01.com.example:legacy-vol"
	)

	// Write a Phase 1 (legacy) state file directly.
	legacyJSON := `{"subsys_nqn":"` + nqn + `"}`
	stateFile := srv.stateFilePath(volumeID)
	if err := os.WriteFile(stateFile, []byte(legacyJSON), 0o600); err != nil {
		t.Fatalf("write legacy state file: %v", err)
	}

	// readStageState must migrate the file and return the correct state.
	got, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState (legacy): %v", err)
	}
	if got == nil {
		t.Fatal("readStageState returned nil for legacy file")
	}
	if got.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("migrated ProtocolType = %q, want %q", got.ProtocolType, "nvmeof-tcp")
	}
	if got.NVMeoF == nil {
		t.Fatal("migrated NVMeoF sub-struct is nil")
	}
	if got.NVMeoF.SubsysNQN != nqn {
		t.Errorf("migrated SubsysNQN = %q, want %q", got.NVMeoF.SubsysNQN, nqn)
	}

	// The file should have been rewritten in the new format (in-place migration).
	// A second read must return the same data from the migrated file.
	got2, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState (after migration): %v", err)
	}
	if got2 == nil || got2.NVMeoF == nil || got2.NVMeoF.SubsysNQN != nqn {
		t.Errorf("second readStageState = %+v, want NQN %q", got2, nqn)
	}
}

// TestStageState_ToProtocolState_NVMeoF verifies that ToProtocolState produces
// a *NVMeoFProtocolState with the correct fields for an NVMe-oF stage state.
func TestStageState_ToProtocolState_NVMeoF(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: "nqn.test:vol1",
			Address:   testStorageAddr,
			Port:      "4420",
		},
	}

	ps, err := s.ToProtocolState()
	if err != nil {
		t.Fatalf("ToProtocolState error: %v", err)
	}
	nvme, ok := ps.(*NVMeoFProtocolState)
	if !ok {
		t.Fatalf("ToProtocolState returned %T, want *NVMeoFProtocolState", ps)
	}
	if nvme.SubsysNQN != "nqn.test:vol1" {
		t.Errorf("SubsysNQN = %q, want %q", nvme.SubsysNQN, "nqn.test:vol1")
	}
	if nvme.Address != testStorageAddr {
		t.Errorf("Address = %q, want %q", nvme.Address, testStorageAddr)
	}
	if nvme.Port != "4420" {
		t.Errorf("Port = %q, want %q", nvme.Port, "4420")
	}
}

// TestStageState_ToProtocolState_NilAndUnknown verifies that ToProtocolState
// returns nil for nil receivers and unrecognized protocol types.
func TestStageState_ToProtocolState_NilAndUnknown(t *testing.T) {
	t.Parallel()

	// nil receiver
	var nilState *nodeStageState
	if _, err := nilState.ToProtocolState(); err == nil {
		t.Error("nil.ToProtocolState() should return error")
	}

	// Unknown protocol type
	unknown := &nodeStageState{ProtocolType: "unknown-proto"}
	if _, err := unknown.ToProtocolState(); err == nil {
		t.Error("unknown.ToProtocolState() should return error")
	}

	// nvmeof-tcp with nil NVMeoF sub-struct
	noSub := &nodeStageState{ProtocolType: ProtocolNVMeoFTCP, NVMeoF: nil}
	if _, err := noSub.ToProtocolState(); err == nil {
		t.Error("nvmeof-tcp+nil.ToProtocolState() should return error")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Protocol dispatch tests (AC 7)
// ─────────────────────────────────────────────────────────────────────────────

// TestResolveProtocolType_FromVolumeContext verifies that the explicit
// VolumeContext["pillar-csi.bhyoo.com/protocol-type"] key is preferred.
func TestResolveProtocolType_FromVolumeContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		volumeID     string
		volCtx       map[string]string
		wantProtocol string
	}{
		{
			name:         "nvmeof-tcp from VolumeContext",
			volumeID:     "storage-node/nvmeof-tcp/zfs-zvol/tank/pvc-abc",
			volCtx:       map[string]string{VolumeContextKeyProtocolType: "nvmeof-tcp"},
			wantProtocol: "nvmeof-tcp",
		},
		{
			name:         "iscsi from VolumeContext overrides volumeID",
			volumeID:     "storage-node/nvmeof-tcp/zfs-zvol/tank/pvc-abc",
			volCtx:       map[string]string{VolumeContextKeyProtocolType: "iscsi"},
			wantProtocol: "iscsi",
		},
		{
			name:         "nfs from VolumeContext",
			volumeID:     "storage-node/nfs/zfs-dataset/tank/pvc-abc",
			volCtx:       map[string]string{VolumeContextKeyProtocolType: "nfs"},
			wantProtocol: "nfs",
		},
		{
			name:         "smb from VolumeContext",
			volumeID:     "storage-node/smb/zfs-dataset/tank/pvc-abc",
			volCtx:       map[string]string{VolumeContextKeyProtocolType: "smb"},
			wantProtocol: "smb",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveProtocolType(tt.volumeID, tt.volCtx)
			if got != tt.wantProtocol {
				t.Errorf("resolveProtocolType = %q, want %q", got, tt.wantProtocol)
			}
		})
	}
}

// TestResolveProtocolType_FromVolumeID verifies fallback to volumeID parsing
// when VolumeContext does not carry a protocol-type key.
func TestResolveProtocolType_FromVolumeID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		volumeID     string
		wantProtocol string
	}{
		{
			name:         "nvmeof-tcp from volumeID",
			volumeID:     "storage-node/nvmeof-tcp/zfs-zvol/tank/pvc-abc",
			wantProtocol: "nvmeof-tcp",
		},
		{
			name:         "iscsi from volumeID",
			volumeID:     "storage-node/iscsi/zfs-zvol/tank/pvc-abc",
			wantProtocol: "iscsi",
		},
		{
			name:         "nfs from volumeID",
			volumeID:     "storage-node/nfs/zfs-dataset/tank/pvc-abc",
			wantProtocol: "nfs",
		},
		{
			name:         "smb from volumeID",
			volumeID:     "storage-node/smb/samba-share/tank/pvc-abc",
			wantProtocol: "smb",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveProtocolType(tt.volumeID, nil)
			if got != tt.wantProtocol {
				t.Errorf("resolveProtocolType = %q, want %q", got, tt.wantProtocol)
			}
		})
	}
}

// TestResolveProtocolType_DefaultFallback verifies that resolveProtocolType
// returns "nvmeof-tcp" when neither VolumeContext nor volumeID provides a
// recognized protocol type (backward compatibility for Phase 1 volumes).
func TestResolveProtocolType_DefaultFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		volumeID string
		volCtx   map[string]string
	}{
		{
			name:     "simple two-part volumeID (old format)",
			volumeID: "tank/pvc-test",
		},
		{
			name:     "empty VolumeContext",
			volumeID: "tank/pvc-test",
			volCtx:   map[string]string{},
		},
		{
			name:     "unknown protocol in volumeID",
			volumeID: "storage-node/fibrechannel/lun/0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveProtocolType(tt.volumeID, tt.volCtx)
			if got != ProtocolNVMeoFTCP {
				t.Errorf("resolveProtocolType = %q, want %q (default)", got, ProtocolNVMeoFTCP)
			}
		})
	}
}

// TestNodeStageVolume_ProtocolDispatch_NVMeoF verifies that NodeStageVolume
// dispatches to the registered "nvmeof-tcp" handler when the VolumeContext
// carries VolumeContextKeyProtocolType = "nvmeof-tcp".
func TestNodeStageVolume_ProtocolDispatch_NVMeoF(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()

	const (
		volumeID = "storage-node/nvmeof-tcp/zfs-zvol/tank/pvc-dispatch"
		nqn      = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-dispatch"
		addr     = "192.0.2.100"
		port     = "4420"
	)

	// Include the explicit protocol-type key alongside the NVMe-oF params.
	volCtx := map[string]string{
		VolumeContextKeyTargetID:     nqn,
		VolumeContextKeyAddress:      addr,
		VolumeContextKeyPort:         port,
		VolumeContextKeyProtocolType: "nvmeof-tcp",
	}

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     volCtx,
	})
	if err != nil {
		t.Fatalf("NodeStageVolume with explicit nvmeof-tcp protocol-type: %v", err)
	}

	// The connector mock should have received the Connect call.
	if len(env.connector.connectCalls) != 1 {
		t.Fatalf("Connect called %d times, want 1", len(env.connector.connectCalls))
	}
	if env.connector.connectCalls[0].subsysNQN != nqn {
		t.Errorf("Connect NQN = %q, want %q", env.connector.connectCalls[0].subsysNQN, nqn)
	}

	// State file should record nvmeof-tcp as the protocol type.
	state, stateErr := env.srv.readStageState(volumeID)
	if stateErr != nil {
		t.Fatalf("readStageState: %v", stateErr)
	}
	if state == nil {
		t.Fatal("state is nil after NodeStageVolume")
	}
	if state.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("state.ProtocolType = %q, want %q", state.ProtocolType, ProtocolNVMeoFTCP)
	}
}

// TestNodeStageVolume_UnknownProtocolNoHandler verifies that NodeStageVolume
// returns FailedPrecondition when the protocol type resolves to a value for
// which no handler is registered.
func TestNodeStageVolume_UnknownProtocolNoHandler(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()

	// Use an unknown protocol type in VolumeContext.
	volCtx := map[string]string{
		VolumeContextKeyTargetID:     "target:unknown",
		VolumeContextKeyAddress:      "192.0.2.1",
		VolumeContextKeyPort:         "3260",
		VolumeContextKeyProtocolType: "fibrechannel", // not registered
	}

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "storage-node/fibrechannel/lun/0",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     volCtx,
	})
	// Expect FailedPrecondition because no handler is registered for "fibrechannel".
	requireGRPCCode(t, err, codes.FailedPrecondition)
}

// TestNodeStageVolume_ProtocolTypeFromVolumeID verifies that the protocol type
// is correctly resolved from the volumeID path component when VolumeContext
// does not carry VolumeContextKeyProtocolType.
func TestNodeStageVolume_ProtocolTypeFromVolumeID(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()

	// VolumeContext without protocol-type but volumeID encodes "nvmeof-tcp".
	const nqn = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-viaid"

	volCtx := map[string]string{
		VolumeContextKeyTargetID: nqn,
		VolumeContextKeyAddress:  "192.0.2.1",
		VolumeContextKeyPort:     "4420",
		// No VolumeContextKeyProtocolType — should be resolved from volumeID.
	}

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "storage-node/nvmeof-tcp/zfs-zvol/tank/pvc-viaid",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     volCtx,
	})
	if err != nil {
		t.Fatalf("NodeStageVolume with protocol from volumeID: %v", err)
	}

	// Connector should have been invoked (dispatch succeeded).
	if len(env.connector.connectCalls) != 1 {
		t.Fatalf("Connect called %d times, want 1", len(env.connector.connectCalls))
	}
}

// TestStageStateFromAttachResult_NVMeoF verifies that stageStateFromAttachResult
// builds a correct NVMeoF-typed nodeStageState from an NVMeoFProtocolState.
func TestStageStateFromAttachResult_NVMeoF(t *testing.T) {
	t.Parallel()

	result := &AttachResult{
		DevicePath: "/dev/nvme0n1",
		State: &NVMeoFProtocolState{
			SubsysNQN: "nqn.test:vol",
			Address:   testStorageAddr,
			Port:      "4420",
		},
	}

	s := stageStateFromAttachResult(ProtocolNVMeoFTCP, "nqn.test:vol", testStorageAddr, "4420", result)
	if s.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q, want %q", s.ProtocolType, ProtocolNVMeoFTCP)
	}
	if s.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil")
	}
	if s.NVMeoF.SubsysNQN != "nqn.test:vol" {
		t.Errorf("SubsysNQN = %q, want %q", s.NVMeoF.SubsysNQN, "nqn.test:vol")
	}
	if s.NVMeoF.Address != testStorageAddr {
		t.Errorf("Address = %q, want %q", s.NVMeoF.Address, testStorageAddr)
	}
	if s.NVMeoF.Port != "4420" {
		t.Errorf("Port = %q, want %q", s.NVMeoF.Port, "4420")
	}
}

// TestStageStateFromAttachResult_UnknownProtocol verifies that
// stageStateFromAttachResult returns a state with ProtocolType set but no
// typed sub-struct for unknown protocol types.
func TestStageStateFromAttachResult_UnknownProtocol(t *testing.T) {
	t.Parallel()

	s := stageStateFromAttachResult("fibrechannel", "target:fc1", "", "", nil)
	if s.ProtocolType != "fibrechannel" {
		t.Errorf("ProtocolType = %q, want %q", s.ProtocolType, "fibrechannel")
	}
	if s.NVMeoF != nil {
		t.Error("NVMeoF should be nil for unknown protocol")
	}
}
