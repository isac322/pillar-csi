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

// Package component_test – CSI Node Service component tests.
//
// This file covers the CSI Node Service (internal/csi.NodeServer) as a black
// box.  All privileged operations (NVMe-oF connect/disconnect, filesystem
// format, bind mount) are delegated to injectable mock interfaces so no root
// privileges or kernel modules are needed.
package component_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock Connector (implements pillarcsi.Connector)
// ─────────────────────────────────────────────────────────────────────────────

// csiMockConnector is a test double for the pillarcsi.Connector interface.
//
// # Mock fidelity
//
// Approximates: the real NVMe-oF TCP connector, which uses the nvme-cli
// tool (nvme-connect(1), nvme-disconnect(1)) or equivalent kernel ioctls to
// establish and tear down NVMe-oF TCP connections to a remote storage target.
//
// Omits / simplifies:
//   - No kernel module interaction: the real connector requires the nvme_tcp
//     kernel module to be loaded and a reachable NVMe-oF TCP target.  The
//     mock succeeds or fails instantly without any kernel involvement.
//   - No network connection: the real Connect initiates a TCP handshake and
//     an NVMe-oF discovery/connection sequence.  The mock calls connectFn
//     (or returns nil) synchronously with no network I/O.
//   - Device node discovery: the real GetDevicePath polls sysfs (or runs
//     nvme-list) until a /dev/nvme* block device appears, which may take
//     several seconds for udev to create the node.  The mock returns a fixed
//     path ("/dev/nvme0n1") or the result of getDeviceFn without any polling.
//   - Multipath: the real connector may expose the same namespace through
//     multiple /dev/nvme paths when multiple network interfaces or paths are
//     configured.  The mock always returns a single device path.
//   - Connection teardown: the real Disconnect waits for outstanding I/O to
//     complete before sending the NVMe Disconnect command.  The mock calls
//     disconnectFn or returns nil immediately.
//   - Call counters are not goroutine-safe; tests that invoke the connector
//     from multiple goroutines concurrently must add external synchronisation.
//
// Function fields let each test install per-call behaviour.
type csiMockConnector struct {
	connectFn    func(ctx context.Context, subsysNQN, trAddr, trSvcID string) error
	disconnectFn func(ctx context.Context, subsysNQN string) error
	getDeviceFn  func(ctx context.Context, subsysNQN string) (string, error)

	// call counters
	connectCalls    int
	disconnectCalls int
	getDeviceCalls  int
}

// Verify csiMockConnector implements the full Connector interface.
var _ pillarcsi.Connector = (*csiMockConnector)(nil)

func (m *csiMockConnector) Connect(ctx context.Context, subsysNQN, trAddr, trSvcID string) error {
	m.connectCalls++
	if m.connectFn != nil {
		return m.connectFn(ctx, subsysNQN, trAddr, trSvcID)
	}
	return nil // default: success
}

func (m *csiMockConnector) Disconnect(ctx context.Context, subsysNQN string) error {
	m.disconnectCalls++
	if m.disconnectFn != nil {
		return m.disconnectFn(ctx, subsysNQN)
	}
	return nil // default: success
}

func (m *csiMockConnector) GetDevicePath(ctx context.Context, subsysNQN string) (string, error) {
	m.getDeviceCalls++
	if m.getDeviceFn != nil {
		return m.getDeviceFn(ctx, subsysNQN)
	}
	return "/dev/nvme0n1", nil // default: device ready immediately
}

// ─────────────────────────────────────────────────────────────────────────────
// Mock Mounter (implements pillarcsi.Mounter)
// ─────────────────────────────────────────────────────────────────────────────

// csiMockMounter is a test double for the pillarcsi.Mounter interface.
//
// # Mock fidelity
//
// Approximates: the real mounter — a thin wrapper around the
// k8s.io/utils/mount package or direct exec/syscall paths — that formats
// filesystem devices and performs bind-mounts on the node.
//
// Omits / simplifies:
//   - No filesystem I/O: the real FormatAndMount runs mkfs (e.g. mke2fs for
//     ext4, mkfs.xfs for XFS) which writes a superblock and initialises
//     inodes.  The mock records the call and marks the target path as mounted
//     in an in-memory map.
//   - No kernel mount tables: the real Mount / Unmount updates /proc/mounts
//     and the kernel's VFS mount tree.  The mock only tracks state in the
//     in-memory mounted map.
//   - Mount option validation: the real mounter rejects invalid fsType strings
//     or unsupported mount flags with EINVAL.  The mock accepts any arguments
//     without validation.
//   - Bind-mount propagation: the real NodePublishVolume issues bind mounts
//     with MS_BIND | MS_SHARED propagation flags, and the propagation mode
//     affects VFS visibility across mount namespaces.  The mock treats all
//     Mount calls identically regardless of fsType or options.
//   - Idempotency detection: the real IsMounted reads /proc/mounts or calls
//     the kernel findmnt(8) utility.  The mock consults its in-memory mounted
//     map, which is only updated by FormatAndMount and Mount calls made
//     through the mock itself.
//   - Error recovery: the real mounter may partially write filesystem metadata
//     before failing, leaving a partially-formatted device.  The mock is
//     atomic: a call either fully succeeds or returns the preset error without
//     any intermediate state change.
type csiMockMounter struct {
	formatAndMountFn func(source, target, fsType string, options []string) error
	mountFn          func(source, target, fsType string, options []string) error
	unmountFn        func(target string) error
	isMountedFn      func(target string) (bool, error)

	// call counters
	formatAndMountCalls int
	mountCalls          int
	unmountCalls        int
	isMountedCalls      int

	// mounted tracks which paths are currently "mounted" by default behaviour.
	mounted map[string]bool
}

// Verify csiMockMounter implements the full Mounter interface.
var _ pillarcsi.Mounter = (*csiMockMounter)(nil)

func newCsiMockMounter() *csiMockMounter {
	return &csiMockMounter{mounted: make(map[string]bool)}
}

func (m *csiMockMounter) FormatAndMount(source, target, fsType string, options []string) error {
	m.formatAndMountCalls++
	if m.formatAndMountFn != nil {
		return m.formatAndMountFn(source, target, fsType, options)
	}
	m.mounted[target] = true
	return nil
}

func (m *csiMockMounter) Mount(source, target, fsType string, options []string) error {
	m.mountCalls++
	if m.mountFn != nil {
		return m.mountFn(source, target, fsType, options)
	}
	m.mounted[target] = true
	return nil
}

func (m *csiMockMounter) Unmount(target string) error {
	m.unmountCalls++
	if m.unmountFn != nil {
		return m.unmountFn(target)
	}
	delete(m.mounted, target)
	return nil
}

func (m *csiMockMounter) IsMounted(target string) (bool, error) {
	m.isMountedCalls++
	if m.isMountedFn != nil {
		return m.isMountedFn(target)
	}
	return m.mounted[target], nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Test environment helper
// ─────────────────────────────────────────────────────────────────────────────

// csiNodeTestEnv holds everything for a NodeServer component test.
type csiNodeTestEnv struct {
	node      *pillarcsi.NodeServer
	connector *csiMockConnector
	mounter   *csiMockMounter
	stateDir  string
}

func newCSINodeTestEnv(t *testing.T) *csiNodeTestEnv {
	t.Helper()
	stateDir := t.TempDir()
	connector := &csiMockConnector{}
	mounter := newCsiMockMounter()
	node := pillarcsi.NewNodeServerWithStateDir("test-node", connector, mounter, stateDir)
	return &csiNodeTestEnv{
		node:      node,
		connector: connector,
		mounter:   mounter,
		stateDir:  stateDir,
	}
}

// baseStageRequest returns a valid NodeStageVolumeRequest for a MOUNT access type.
func baseStageRequest(stagingPath string) *csipb.NodeStageVolumeRequest {
	return &csipb.NodeStageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:pvc-node-test",
			pillarcsi.VolumeContextKeyAddress:   "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:      "4420",
		},
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{
					FsType: "ext4",
				},
			},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
}

// basePublishRequest returns a valid NodePublishVolumeRequest for a MOUNT volume.
func basePublishRequest(stagingPath, targetPath string) *csipb.NodePublishVolumeRequest {
	return &csipb.NodePublishVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
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
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_GetCapabilities
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_GetCapabilities verifies that NodeGetCapabilities advertises
// STAGE_UNSTAGE_VOLUME and EXPAND_VOLUME.
func TestCSINode_GetCapabilities(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	resp, err := env.node.NodeGetCapabilities(ctx, &csipb.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}

	wantCaps := map[csipb.NodeServiceCapability_RPC_Type]bool{
		csipb.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME: true,
		csipb.NodeServiceCapability_RPC_EXPAND_VOLUME:        true,
	}

	for _, cap := range resp.GetCapabilities() {
		delete(wantCaps, cap.GetRpc().GetType())
	}
	for missing := range wantCaps {
		t.Errorf("capability %v missing from NodeGetCapabilities response", missing)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_GetInfo
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_GetInfo verifies that NodeGetInfo returns the configured node ID.
func TestCSINode_GetInfo(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	resp, err := env.node.NodeGetInfo(ctx, &csipb.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if got, want := resp.GetNodeId(), "test-node"; got != want {
		t.Errorf("NodeId = %q, want %q", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_MountAccess
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_MountAccess verifies the full happy-path stage
// sequence for a MOUNT access type volume:
//  1. Connector.Connect is called with the correct NQN, address, port.
//  2. Connector.GetDevicePath is polled and returns a device path.
//  3. Mounter.FormatAndMount is called to format and mount the device.
func TestCSINode_NodeStageVolume_MountAccess(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	const (
		wantNQN    = "nqn.2026-01.com.pillar-csi:pvc-node-test"
		wantAddr   = "192.168.1.10"
		wantPort   = "4420"
		wantDevice = "/dev/nvme0n1"
	)

	var capturedNQN, capturedAddr, capturedSvcID string
	env.connector.connectFn = func(_ context.Context, subsysNQN, trAddr, trSvcID string) error {
		capturedNQN, capturedAddr, capturedSvcID = subsysNQN, trAddr, trSvcID
		return nil
	}
	env.connector.getDeviceFn = func(_ context.Context, _ string) (string, error) {
		return wantDevice, nil
	}

	var capturedSource, capturedTarget string
	env.mounter.formatAndMountFn = func(source, target, fsType string, options []string) error {
		capturedSource, capturedTarget = source, target
		return nil
	}

	req := baseStageRequest(stagingPath)

	_, err := env.node.NodeStageVolume(ctx, req)
	if err != nil {
		t.Fatalf("NodeStageVolume: unexpected error: %v", err)
	}

	// Verify Connector was called with correct parameters.
	if capturedNQN != wantNQN {
		t.Errorf("Connect NQN = %q, want %q", capturedNQN, wantNQN)
	}
	if capturedAddr != wantAddr {
		t.Errorf("Connect addr = %q, want %q", capturedAddr, wantAddr)
	}
	if capturedSvcID != wantPort {
		t.Errorf("Connect port = %q, want %q", capturedSvcID, wantPort)
	}
	if env.connector.connectCalls != 1 {
		t.Errorf("Connector.Connect calls = %d, want 1", env.connector.connectCalls)
	}

	// Verify FormatAndMount was called with the device path and staging path.
	if capturedSource != wantDevice {
		t.Errorf("FormatAndMount source = %q, want %q", capturedSource, wantDevice)
	}
	if capturedTarget != stagingPath {
		t.Errorf("FormatAndMount target = %q, want %q", capturedTarget, stagingPath)
	}
	if env.mounter.formatAndMountCalls != 1 {
		t.Errorf("Mounter.FormatAndMount calls = %d, want 1", env.mounter.formatAndMountCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_BlockAccess
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_BlockAccess verifies the stage sequence for
// a BLOCK access type: a bind-mount is used instead of FormatAndMount.
func TestCSINode_NodeStageVolume_BlockAccess(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	req := &csipb.NodeStageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-block-test",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:pvc-block-test",
			pillarcsi.VolumeContextKeyAddress:   "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:      "4420",
		},
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Block{
				Block: &csipb.VolumeCapability_BlockVolume{},
			},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	var mountOptions []string
	env.mounter.mountFn = func(_, _ string, _ string, opts []string) error {
		mountOptions = opts
		return nil
	}

	_, err := env.node.NodeStageVolume(ctx, req)
	if err != nil {
		t.Fatalf("NodeStageVolume (block): unexpected error: %v", err)
	}

	// FormatAndMount must NOT be called for block access.
	if env.mounter.formatAndMountCalls != 0 {
		t.Errorf("FormatAndMount was called %d times for block access, expected 0",
			env.mounter.formatAndMountCalls)
	}
	// Mount (bind) must be called once.
	if env.mounter.mountCalls != 1 {
		t.Errorf("Mounter.Mount calls = %d, want 1", env.mounter.mountCalls)
	}
	// Mount options must include "bind".
	hasBind := false
	for _, o := range mountOptions {
		if o == "bind" {
			hasBind = true
		}
	}
	if !hasBind {
		t.Errorf("mount options %v do not contain 'bind'", mountOptions)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_AlreadyStaged
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_AlreadyStaged verifies idempotency: calling
// NodeStageVolume a second time when the staging path is already mounted
// returns success without repeating Connect or FormatAndMount.
func TestCSINode_NodeStageVolume_AlreadyStaged(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	req := baseStageRequest(stagingPath)

	// First call — stages the volume.
	if _, err := env.node.NodeStageVolume(ctx, req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}
	firstConnects := env.connector.connectCalls
	firstMounts := env.mounter.formatAndMountCalls

	// Second call — must be a no-op.
	if _, err := env.node.NodeStageVolume(ctx, req); err != nil {
		t.Fatalf("second NodeStageVolume (idempotent): %v", err)
	}

	if env.connector.connectCalls != firstConnects {
		t.Errorf("Connector.Connect called again on retry: total=%d, after first=%d",
			env.connector.connectCalls, firstConnects)
	}
	if env.mounter.formatAndMountCalls != firstMounts {
		t.Errorf("Mounter.FormatAndMount called again on retry: total=%d, after first=%d",
			env.mounter.formatAndMountCalls, firstMounts)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_ConnectFails
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_ConnectFails verifies that a Connector.Connect
// failure causes NodeStageVolume to return an Internal error and prevents
// FormatAndMount from being called.
func TestCSINode_NodeStageVolume_ConnectFails(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	env.connector.connectFn = func(_ context.Context, _, _, _ string) error {
		return errors.New("nvme connect: connection refused")
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want %v", st.Code(), codes.Internal)
	}
	if env.mounter.formatAndMountCalls != 0 {
		t.Errorf("FormatAndMount called %d times after Connect failure, want 0",
			env.mounter.formatAndMountCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_DeviceTimeout
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_DeviceTimeout verifies that when the block
// device never appears, NodeStageVolume returns DeadlineExceeded.
//
// The context carries a short deadline (200 ms) which causes the internal
// device-poll loop to time out well before the 30 s device-wait timeout,
// keeping the test fast.
func TestCSINode_NodeStageVolume_DeviceTimeout(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	stagingPath := t.TempDir()

	// GetDevicePath always returns ("", nil) — device never appears.
	env.connector.getDeviceFn = func(_ context.Context, _ string) (string, error) {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.DeadlineExceeded {
		t.Errorf("error code = %v, want %v", st.Code(), codes.DeadlineExceeded)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_GetDevicePathError
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_GetDevicePathError verifies that a
// GetDevicePath error (e.g., permission denied) returns Internal immediately
// without polling further.
func TestCSINode_NodeStageVolume_GetDevicePathError(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	env.connector.getDeviceFn = func(_ context.Context, _ string) (string, error) {
		return "", errors.New("permission denied")
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want %v", st.Code(), codes.Internal)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_MountFails
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_MountFails verifies that a FormatAndMount
// failure causes NodeStageVolume to return an Internal error.
func TestCSINode_NodeStageVolume_MountFails(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	env.mounter.formatAndMountFn = func(_, _ string, _ string, _ []string) error {
		return errors.New("mkfs.ext4: device busy")
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want %v", st.Code(), codes.Internal)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_MissingVolumeContext
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_MissingVolumeContext verifies that missing
// required VolumeContext keys are rejected with InvalidArgument.
func TestCSINode_NodeStageVolume_MissingVolumeContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name    string
		dropKey string
	}{
		{"missing NQN", pillarcsi.VolumeContextKeyTargetNQN},
		{"missing address", pillarcsi.VolumeContextKeyAddress},
		{"missing port", pillarcsi.VolumeContextKeyPort},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCSINodeTestEnv(t)
			stagingPath := t.TempDir()
			req := baseStageRequest(stagingPath)
			delete(req.VolumeContext, tc.dropKey)

			_, err := env.node.NodeStageVolume(ctx, req)
			if err == nil {
				t.Fatalf("expected error for missing %q, got nil", tc.dropKey)
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument {
				t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeUnstageVolume_Success
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeUnstageVolume_Success verifies the full unstage sequence:
//  1. Mounter.Unmount is called on the staging path.
//  2. Connector.Disconnect is called with the correct subsystem NQN.
//  3. The stage state file is removed so subsequent unstage calls return idempotently.
func TestCSINode_NodeUnstageVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	// First stage the volume.
	if _, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath)); err != nil {
		t.Fatalf("setup NodeStageVolume: %v", err)
	}

	var capturedNQN string
	env.connector.disconnectFn = func(_ context.Context, subsysNQN string) error {
		capturedNQN = subsysNQN
		return nil
	}

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: unexpected error: %v", err)
	}

	const wantNQN = "nqn.2026-01.com.pillar-csi:pvc-node-test"
	if capturedNQN != wantNQN {
		t.Errorf("Disconnect NQN = %q, want %q", capturedNQN, wantNQN)
	}
	if env.connector.disconnectCalls != 1 {
		t.Errorf("Connector.Disconnect calls = %d, want 1", env.connector.disconnectCalls)
	}
	if env.mounter.unmountCalls != 1 {
		t.Errorf("Mounter.Unmount calls = %d, want 1", env.mounter.unmountCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeUnstageVolume_AlreadyUnstaged
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeUnstageVolume_AlreadyUnstaged verifies that calling
// NodeUnstageVolume on a volume that was never staged (no state file) returns
// success without calling Disconnect (idempotent per CSI spec §4.7).
func TestCSINode_NodeUnstageVolume_AlreadyUnstaged(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-never-staged",
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume (never staged): unexpected error: %v", err)
	}
	if env.connector.disconnectCalls != 0 {
		t.Errorf("Connector.Disconnect calls = %d, want 0 (no state file)", env.connector.disconnectCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodePublishVolume_Success
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodePublishVolume_Success verifies that NodePublishVolume
// performs a bind-mount from the staging path to the target path.
func TestCSINode_NodePublishVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	var capturedSrc, capturedDst string
	env.mounter.mountFn = func(src, dst, _ string, _ []string) error {
		capturedSrc, capturedDst = src, dst
		return nil
	}

	_, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath))
	if err != nil {
		t.Fatalf("NodePublishVolume: unexpected error: %v", err)
	}
	if capturedSrc != stagingPath {
		t.Errorf("bind-mount source = %q, want %q", capturedSrc, stagingPath)
	}
	if capturedDst != targetPath {
		t.Errorf("bind-mount destination = %q, want %q", capturedDst, targetPath)
	}
	if env.mounter.mountCalls != 1 {
		t.Errorf("Mounter.Mount calls = %d, want 1", env.mounter.mountCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodePublishVolume_ReadOnly
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodePublishVolume_ReadOnly verifies that when Readonly=true
// the bind-mount options include "ro".
func TestCSINode_NodePublishVolume_ReadOnly(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	var capturedOpts []string
	env.mounter.mountFn = func(_, _ string, _ string, opts []string) error {
		capturedOpts = opts
		return nil
	}

	req := basePublishRequest(stagingPath, targetPath)
	req.Readonly = true

	_, err := env.node.NodePublishVolume(ctx, req)
	if err != nil {
		t.Fatalf("NodePublishVolume (readonly): unexpected error: %v", err)
	}

	hasRO := false
	for _, o := range capturedOpts {
		if o == "ro" {
			hasRO = true
		}
	}
	if !hasRO {
		t.Errorf("mount options %v do not contain 'ro' for readonly volume", capturedOpts)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodePublishVolume_Idempotent
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodePublishVolume_Idempotent verifies that a second
// NodePublishVolume call when the target path is already mounted returns
// success without calling Mount again.
func TestCSINode_NodePublishVolume_Idempotent(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// First call — mounts the target.
	if _, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath)); err != nil {
		t.Fatalf("first NodePublishVolume: %v", err)
	}
	firstMounts := env.mounter.mountCalls

	// Second call — must be a no-op.
	if _, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath)); err != nil {
		t.Fatalf("second NodePublishVolume (idempotent): %v", err)
	}
	if env.mounter.mountCalls != firstMounts {
		t.Errorf("Mounter.Mount called again on retry: total=%d, after first=%d",
			env.mounter.mountCalls, firstMounts)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodePublishVolume_MountFails
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodePublishVolume_MountFails verifies that a Mounter.Mount
// failure during publish returns Internal.
func TestCSINode_NodePublishVolume_MountFails(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	env.mounter.mountFn = func(_, _ string, _ string, _ []string) error {
		return errors.New("mount: permission denied")
	}

	_, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want %v", st.Code(), codes.Internal)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeUnpublishVolume_Success
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeUnpublishVolume_Success verifies that NodeUnpublishVolume
// calls Mounter.Unmount on the target path.
func TestCSINode_NodeUnpublishVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// Publish first.
	if _, err := env.node.NodePublishVolume(ctx, basePublishRequest(stagingPath, targetPath)); err != nil {
		t.Fatalf("setup NodePublishVolume: %v", err)
	}

	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test",
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume: unexpected error: %v", err)
	}
	if env.mounter.unmountCalls < 1 {
		t.Errorf("Mounter.Unmount calls = %d, want >= 1", env.mounter.unmountCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeUnpublishVolume_Idempotent
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeUnpublishVolume_Idempotent verifies that unpublishing a
// path that is not currently mounted returns success without error.
func TestCSINode_NodeUnpublishVolume_Idempotent(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	targetPath := t.TempDir()

	// No prior publish — target is not mounted.
	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-never-published",
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume (never published): unexpected error: %v", err)
	}
	if env.mounter.unmountCalls != 0 {
		t.Errorf("Mounter.Unmount calls = %d, want 0 (not mounted)", env.mounter.unmountCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageUnstagePublishUnpublish_FullLifecycle
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageUnstagePublishUnpublish_FullLifecycle exercises the
// complete node-side volume lifecycle:
//
//	Stage → Publish → Unpublish → Unstage
func TestCSINode_NodeStageUnstagePublishUnpublish_FullLifecycle(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-lifecycle-test"
	const nqn = "nqn.2026-01.com.pillar-csi:pvc-lifecycle-test"

	// Override the stage request to use the lifecycle volumeID.
	stageReq := &csipb.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: nqn,
			pillarcsi.VolumeContextKeyAddress:   "10.0.0.1",
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
	}

	// 1. Stage.
	if _, err := env.node.NodeStageVolume(ctx, stageReq); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// 2. Publish.
	pubReq := &csipb.NodePublishVolumeRequest{
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
	}
	if _, err := env.node.NodePublishVolume(ctx, pubReq); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	// 3. Unpublish.
	if _, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}

	// 4. Unstage.
	if _, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	}); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	// After unstage, Connector.Disconnect must have been called with the NQN.
	if env.connector.disconnectCalls != 1 {
		t.Errorf("Connector.Disconnect calls = %d, want 1", env.connector.disconnectCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeStageVolume_MissingVolumeID
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_MissingVolumeID verifies that an empty volume_id
// is rejected with InvalidArgument.
func TestCSINode_NodeStageVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
		VolumeId:          "",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  &csipb.VolumeCapability{},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 5.7 NodeExpandVolume
// TESTCASES.md § 5.7 tests 29–30
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeExpandVolume_UnknownMount verifies that NodeExpandVolume
// returns Internal when the volume_path is not a known mount point (test
// case 29).
//
// NodeExpandVolume is implemented: it calls the filesystem-specific resize
// tool (resize2fs / xfs_growfs).  When the supplied volume_path is not
// present in /proc/mounts the resizer cannot determine the backing block
// device and returns an Internal error rather than panicking or hanging.
//
// See TESTCASES.md §5.7, row 29.
func TestCSINode_NodeExpandVolume_UnknownMount(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeExpandVolume(ctx, &csipb.NodeExpandVolumeRequest{
		VolumeId:      "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-expand-test",
		VolumePath:    t.TempDir(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 10 * 1024 * 1024 * 1024},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want %v", st.Code(), codes.Internal)
	}
}

// TestCSINode_GetCapabilities_AdvertisesExpandVolume verifies that
// NodeGetCapabilities includes both STAGE_UNSTAGE_VOLUME and EXPAND_VOLUME
// RPC types (test case 30).
//
// See TESTCASES.md §5.7, row 30.
func TestCSINode_GetCapabilities_AdvertisesExpandVolume(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	resp, err := env.node.NodeGetCapabilities(ctx, &csipb.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}

	want := map[csipb.NodeServiceCapability_RPC_Type]bool{
		csipb.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME: true,
		csipb.NodeServiceCapability_RPC_EXPAND_VOLUME:        true,
	}
	for _, cap := range resp.GetCapabilities() {
		delete(want, cap.GetRpc().GetType())
	}
	for missing := range want {
		t.Errorf("capability %v missing from NodeGetCapabilities response", missing)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 5.9 State File Edge Cases
// TESTCASES.md § 5.9 tests 35–38
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeUnstage_CorruptStateFile verifies that a corrupt (non-JSON)
// state file during NodeUnstageVolume returns a non-OK status without panic
// (test case 35).
//
// See TESTCASES.md §5.9, row 35.
func TestCSINode_NodeUnstage_CorruptStateFile(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-corrupt-state"
	stagingPath := t.TempDir()

	// Write corrupt bytes directly to the expected state file location.
	// The stateDir is env.stateDir; path computation mirrors stateFilePath.
	safeID := strings.ReplaceAll(volumeID, "/", "_")
	stateFilePath := env.stateDir + "/" + safeID + ".json"

	if err := os.WriteFile(stateFilePath, []byte("not valid json {{{"), 0o600); err != nil {
		t.Fatalf("write corrupt state file: %v", err)
	}

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err == nil {
		t.Fatal("expected error for corrupt state file, got nil")
	}
	// No panic means the test passed the panic check; just log the error.
	t.Logf("got expected error: %v", err)
}

// TestCSINode_NodeStage_StateDirUnwritable verifies that an unwritable stateDir
// causes NodeStageVolume to fail after mount succeeds (test case 36).
//
// The test is skipped when running as root because root can write to 0555 dirs.
//
// See TESTCASES.md §5.9, row 36.
func TestCSINode_NodeStage_StateDirUnwritable(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("running as root: permission check is ineffective")
	}

	stateDir := t.TempDir()
	// Make stateDir read-only so that writing the state file fails.
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatalf("chmod stateDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	connector := &csiMockConnector{}
	mounter := newCsiMockMounter()
	node := pillarcsi.NewNodeServerWithStateDir("test-node", connector, mounter, stateDir)

	ctx := context.Background()
	stagingPath := t.TempDir()

	_, err := node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-unwritable",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:pvc-unwritable",
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
		t.Fatal("expected error for unwritable stateDir, got nil")
	}
	t.Logf("got expected error: %v", err)
}

// TestCSINode_NodeUnstage_StateFileMissingIsOK verifies that a missing state
// file is treated as "not staged" — NodeUnstageVolume becomes a no-op (test
// case 37).
//
// See TESTCASES.md §5.9, row 37.
func TestCSINode_NodeUnstage_StateFileMissingIsOK(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-no-state"
	stagingPath := t.TempDir()

	// Mounter reports the staging path is not mounted.
	env.mounter.isMountedFn = func(target string) (bool, error) {
		return false, nil
	}

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume with missing state file: %v", err)
	}
}

// TestCSINode_NodeStage_Idempotent_StateFileExists verifies that a second
// NodeStageVolume when the state file exists and the path is already mounted
// is a no-op (test case 38).
//
// Connector.Connect must be called at most once across both invocations.
//
// See TESTCASES.md §5.9, row 38.
func TestCSINode_NodeStage_Idempotent_StateFileExists(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	stagingPath := t.TempDir()

	// First call — stages the volume and writes the state file.
	req := baseStageRequest(stagingPath)
	if _, err := env.node.NodeStageVolume(ctx, req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}
	connectAfterFirst := env.connector.connectCalls

	// Second call with the same request — the state file exists and the path is
	// reported as mounted by the in-memory mock mounter.
	if _, err := env.node.NodeStageVolume(ctx, req); err != nil {
		t.Fatalf("second NodeStageVolume: %v", err)
	}

	// Connect should have been called at most once across both invocations.
	if env.connector.connectCalls > connectAfterFirst {
		t.Errorf("Connect called %d additional time(s) on idempotent retry, want 0",
			env.connector.connectCalls-connectAfterFirst)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 5.10 Additional Input Validation
// TESTCASES.md § 5.10 tests 39–45
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeStageVolume_MissingStagingTargetPath verifies that an empty
// staging_target_path is rejected with InvalidArgument (test case 39).
//
// See TESTCASES.md §5.10, row 39.
func TestCSINode_NodeStageVolume_MissingStagingTargetPath(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-missing-staging",
		StagingTargetPath: "", // missing
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:pvc-missing-staging",
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
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestCSINode_NodeStageVolume_NilVolumeCapability verifies that a nil
// volume_capability is rejected with InvalidArgument (test case 40).
//
// See TESTCASES.md §5.10, row 40.
func TestCSINode_NodeStageVolume_NilVolumeCapability(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-nil-cap",
		StagingTargetPath: t.TempDir(),
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetNQN: "nqn.2026-01.com.pillar-csi:pvc-nil-cap",
			pillarcsi.VolumeContextKeyAddress:   "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:      "4420",
		},
		VolumeCapability: nil, // nil capability
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestCSINode_NodePublishVolume_MissingVolumeID verifies that an empty
// VolumeID on NodePublishVolume is rejected with InvalidArgument (test case 41).
//
// See TESTCASES.md §5.10, row 41.
func TestCSINode_NodePublishVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodePublishVolume(ctx, &csipb.NodePublishVolumeRequest{
		VolumeId:          "", // missing
		StagingTargetPath: t.TempDir(),
		TargetPath:        t.TempDir(),
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
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestCSINode_NodePublishVolume_MissingTargetPath verifies that an empty
// target_path on NodePublishVolume is rejected with InvalidArgument (test
// case 42).
//
// See TESTCASES.md §5.10, row 42.
func TestCSINode_NodePublishVolume_MissingTargetPath(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodePublishVolume(ctx, &csipb.NodePublishVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-missing-target",
		StagingTargetPath: t.TempDir(),
		TargetPath:        "", // missing
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
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestCSINode_NodeUnstageVolume_MissingVolumeID verifies that an empty
// VolumeID on NodeUnstageVolume is rejected with InvalidArgument (test
// case 43).
//
// See TESTCASES.md §5.10, row 43.
func TestCSINode_NodeUnstageVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          "", // missing
		StagingTargetPath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestCSINode_NodeUnstageVolume_MissingStagingTargetPath verifies that an
// empty staging_target_path on NodeUnstageVolume is rejected with
// InvalidArgument (test case 44).
//
// See TESTCASES.md §5.10, row 44.
func TestCSINode_NodeUnstageVolume_MissingStagingTargetPath(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-missing-staging-unstage",
		StagingTargetPath: "", // missing
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// TestCSINode_NodeUnpublishVolume_MissingVolumeID verifies that an empty
// VolumeID on NodeUnpublishVolume is rejected with InvalidArgument (test
// case 45).
//
// See TESTCASES.md §5.10, row 45.
func TestCSINode_NodeUnpublishVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   "", // missing
		TargetPath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}
