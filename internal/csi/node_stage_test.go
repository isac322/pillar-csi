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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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

func (m *mockConnector) Connect(
	_ context.Context, subsysNQN, trAddr, trSvcID string, _ NVMeoFConnectOptions,
) error {
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
//
// The mount table tracks the mount source of every mounted path so that
// bind mounts share the filesystem identity of their origin: marking a
// device source unhealthy makes every mount and bind of it report
// ErrMountUnhealthy, mirroring a kernel-shutdown filesystem whose
// superblock outlives any single mount entry.  A device source heals when
// its last mount is unmounted (the next FormatAndMount/Mount of that
// device re-uses the source without the shutdown mark), or when
// FormatAndMount mounts it fresh.
type mockMounter struct {
	// mountedPaths is the set of paths currently "mounted".
	mountedPaths map[string]bool
	// mountSource records the mount source per mounted path: the device
	// for filesystem mounts, the source path for bind mounts.
	mountSource map[string]string
	// unhealthy holds device sources whose filesystem entered kernel
	// shutdown; probes of every mount rooted at one report
	// ErrMountUnhealthy.
	unhealthy map[string]bool
	// mountRO records mounts created read-only ("ro" option or a bind of a
	// read-only mount): their health probe fails with EROFS, the same
	// answer a kernel shutdown produces, which is why read-only mounts are
	// never write-probed in production code.
	mountRO map[string]bool
	// diskFormat models the filesystem signature blkid reports for the
	// devices a test registers ("" = blank).  FormatAndMount of a
	// registered device behaves like KubeMounter (formatIfBlank +
	// SafeFormatAndMount): a blank device is formatted (mkfsDevices), an
	// existing filesystem is fsck-checked before a read-write mount
	// (fsckDevices), and a filesystem of another type fails the mount.
	// Unregistered devices keep the historical record-only behavior.
	diskFormat  map[string]string
	mkfsDevices []string
	fsckDevices []string

	// errors to return per method (nil = success).
	formatAndMountErr   error
	mountErr            error
	mountErrOnce        error
	unmountErr          error
	mountEntryExistsErr error
	checkHealthErr      error
	hasOtherMountsErr   error

	// Recorded calls.
	formatAndMountCalls   []formatAndMountCall
	mountCalls            []mountCall
	unmountCalls          []string
	mountEntryExistsCalls []string
	checkHealthCalls      []string
}

type formatAndMountCall struct {
	source, target, fsType string
	options                []string
	formatOptions          []string
}

type mountCall struct {
	source, target, fsType string
	options                []string
}

func newMockMounter() *mockMounter {
	return &mockMounter{
		mountedPaths: make(map[string]bool),
		mountSource:  make(map[string]string),
		unhealthy:    make(map[string]bool),
		mountRO:      make(map[string]bool),
		diskFormat:   make(map[string]string),
	}
}

// markUnhealthy makes every mount whose filesystem is rooted at source
// (itself a device path) report ErrMountUnhealthy, like an XFS forced
// shutdown: mounts stay in the table but fail the health probe.
func (m *mockMounter) markUnhealthy(source string) {
	m.unhealthy[source] = true
}

// resolveSource walks bind chains to the ultimate device source of the
// filesystem mounted at path, mirroring mountinfo's shared device number.
func (m *mockMounter) resolveSource(path string) string {
	source := m.mountSource[path]
	for m.mountedPaths[source] {
		source = m.mountSource[source]
	}
	return source
}

// deviceMounts counts mounts whose filesystem is rooted at source,
// including bind mounts of any directory of it.
func (m *mockMounter) deviceMounts(source string) int {
	count := 0
	for path := range m.mountedPaths {
		if m.resolveSource(path) == source {
			count++
		}
	}
	return count
}

func (m *mockMounter) FormatAndMount(
	_ context.Context, source, target, fsType string, options, formatOptions []string,
) error {
	m.formatAndMountCalls = append(m.formatAndMountCalls,
		formatAndMountCall{source, target, fsType, options, formatOptions})
	if m.formatAndMountErr != nil {
		return m.formatAndMountErr
	}
	if existing, tracked := m.diskFormat[source]; tracked {
		want := fsType
		if want == "" {
			want = defaultFsType
		}
		readOnly := slices.Contains(options, "ro")
		switch {
		case existing == "" && readOnly:
			return fmt.Errorf("cannot mount unformatted disk %s read-only", source)
		case existing == "":
			m.mkfsDevices = append(m.mkfsDevices, source)
			m.diskFormat[source] = want
		default:
			if !readOnly {
				m.fsckDevices = append(m.fsckDevices, source)
			}
			if existing != want {
				return fmt.Errorf("mount -t %s %s: wrong fs type, bad option, bad superblock (device carries %s)",
					want, source, existing)
			}
		}
	}
	m.mountedPaths[target] = true
	m.mountSource[target] = source
	m.mountRO[target] = slices.Contains(options, "ro")
	// Mounting a device whose dead superblock is still pinned by other
	// mounts re-attaches the dead filesystem, so the unhealthy mark is
	// deliberately kept; Unmount clears it when the last mount goes away.
	return nil
}

// MountExisting mirrors KubeMounter's preserve-original mount path: it
// reads the recorded blkid signature of the device — never echoes the
// request — and mounts the existing filesystem as is, without mkfs, fsck
// or any repair step.  A device with no signature answers ErrNoFilesystem
// and one whose signature differs from the requested type answers
// ErrFilesystemMismatch; nothing is mounted in either case.
func (m *mockMounter) MountExisting(
	_ context.Context, source, target, fsType string, options []string,
) error {
	want := fsType
	if want == "" {
		want = defaultFsType
	}
	existing := m.diskFormat[source]
	switch {
	case existing == "":
		return ErrNoFilesystem
	case existing != want:
		return fmt.Errorf("%w (device carries %s, want %s)", ErrFilesystemMismatch, existing, want)
	}
	m.mountedPaths[target] = true
	m.mountSource[target] = source
	m.mountRO[target] = slices.Contains(options, "ro")
	// As in FormatAndMount, re-mounting while the dead superblock is still
	// pinned re-attaches the dead filesystem; the unhealthy mark is kept.
	return nil
}

func (m *mockMounter) Mount(source, target, fsType string, options []string) error {
	m.mountCalls = append(m.mountCalls, mountCall{source, target, fsType, options})
	if m.mountErr != nil {
		return m.mountErr
	}
	if err := m.mountErrOnce; err != nil {
		m.mountErrOnce = nil
		return err
	}
	m.mountedPaths[target] = true
	m.mountSource[target] = source
	// A bind mount is read-only when asked ("ro") or when its source mount
	// is read-only — mount(2) inherits the read-only superblock flag.
	m.mountRO[target] = slices.Contains(options, "ro") || m.mountRO[source]
	return nil
}

func (m *mockMounter) Unmount(target string) error {
	m.unmountCalls = append(m.unmountCalls, target)
	if m.unmountErr != nil {
		return m.unmountErr
	}
	source := m.resolveSource(target)
	delete(m.mountedPaths, target)
	delete(m.mountSource, target)
	delete(m.mountRO, target)
	// The superblock dies with its last mount: the next mount starts clean.
	if m.deviceMounts(source) == 0 {
		delete(m.unhealthy, source)
	}
	return nil
}

// CheckMountReadable mirrors KubeMounter's stat probe: a mounted filesystem
// in kernel shutdown keeps its mount table entry but answers stat(2) with
// EIO (issue #175); an unmounted directory stats fine.
func (m *mockMounter) CheckMountReadable(target string) error {
	if m.mountedPaths[target] && m.unhealthy[m.resolveSource(target)] {
		return fmt.Errorf("stat %s: %w: %w", target, ErrMountUnhealthy, syscall.EIO)
	}
	return nil
}

// MountEntryExists mirrors mountinfo: a dead mount keeps its entry.
func (m *mockMounter) MountEntryExists(target string) (bool, error) {
	m.mountEntryExistsCalls = append(m.mountEntryExistsCalls, target)
	if m.mountEntryExistsErr != nil {
		return false, m.mountEntryExistsErr
	}
	return m.mountedPaths[target], nil
}

func (m *mockMounter) CheckMountHealth(target string) error {
	m.checkHealthCalls = append(m.checkHealthCalls, target)
	if m.checkHealthErr != nil {
		return m.checkHealthErr
	}
	if !m.mountedPaths[target] {
		return fmt.Errorf("%q is not a mount point", target)
	}
	if m.unhealthy[m.resolveSource(target)] {
		return fmt.Errorf("probe %q: %w: %w", target, ErrMountUnhealthy, syscall.EIO)
	}
	// A write probe on a read-only mount answers EROFS — indistinguishable
	// from an ext4 remount-ro shutdown, which is why production code never
	// probes mounts that are read-only by request.
	if m.mountRO[target] {
		return fmt.Errorf("probe %q: %w: %w", target, ErrMountUnhealthy, syscall.EROFS)
	}
	return nil
}

func (m *mockMounter) HasOtherMounts(target string) (bool, error) {
	if m.hasOtherMountsErr != nil {
		return false, m.hasOtherMountsErr
	}
	if !m.mountedPaths[target] {
		return false, fmt.Errorf("%q is not a mount point", target)
	}
	return m.deviceMounts(m.resolveSource(target)) > 1, nil
}

func (m *mockMounter) MountSource(target string) (string, error) {
	if !m.mountedPaths[target] {
		return "", fmt.Errorf("%q is not a mount point", target)
	}
	return m.mountSource[target], nil
}

// ObserveMount mirrors mountinfo plus statfs: the recorded source, the
// filesystem type of the latest mount at target and its read-only flag.
func (m *mockMounter) ObserveMount(target string) (MountObservation, error) {
	if !m.mountedPaths[target] {
		return MountObservation{}, fmt.Errorf("%q is not a mount point", target)
	}
	fsType := ""
	for _, call := range slices.Backward(m.mountCalls) {
		if call.target == target {
			fsType = call.fsType
			break
		}
	}
	return MountObservation{Source: m.mountSource[target], FsType: fsType, ReadOnly: m.mountRO[target]}, nil
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

// mountCapRO returns a VolumeCapability for filesystem mount access that is
// staged read-only through the "ro" mount flag.
func mountCapRO(fsType string) *csi.VolumeCapability {
	vc := mountCap(fsType)
	vc.GetMount().MountFlags = []string{"ro"}
	return vc
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
	mounted, _ := env.mounter.MountEntryExists(stagingPath) //nolint:errcheck // mock never returns an error
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

// TestNodeStageVolume_MalformedNVMeoFTuning_NoConnect verifies that a
// malformed ctrl-loss-tmo in the VolumeContext fails staging before any
// connect or mount, instead of silently connecting with kernel defaults.
func TestNodeStageVolume_MalformedNVMeoFTuning_NoConnect(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	volCtx := mountVolumeContext("nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-tuned", "192.0.2.1")
	volCtx[paramNVMeOFCtrlLossTmo] = "ten minutes"

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-tuned",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     volCtx,
	})
	if err == nil {
		t.Fatal("expected NodeStageVolume to fail for malformed ctrl-loss-tmo")
	}
	if len(env.connector.connectCalls) != 0 {
		t.Fatalf("Connect must not be called for malformed tuning, got %d calls", len(env.connector.connectCalls))
	}
	if mounted, _ := env.mounter.MountEntryExists(stagingPath); mounted { //nolint:errcheck // mock never errors
		t.Fatal("staging path must not be mounted after rejected NodeStageVolume")
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

// TestNodeStageVolume_BlockAccess exercises the BLOCK access type: the bind
// mount must point /dev/nvmeXnY at the device-sentinel file inside the
// kubelet-created staging directory (blockStagingDevicePath), NOT at the
// staging directory itself.  Kubelet pre-creates stagingTargetPath as a
// directory and the Linux kernel refuses a block-source → directory-target
// bind with EXT_SOURCEMOUNTREJECTED, so binding to stagingTargetPath
// directly would make every Block-mode workload pod fail at NodeStageVolume.
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
	wantTarget := blockStagingDevicePath(stagingPath)
	if mc.target != wantTarget {
		t.Errorf("Mount target = %q, want %q (in-staging device sentinel; "+
			"binding to stagingPath itself would fail kernel "+
			"EXT_SOURCEMOUNTREJECTED check)",
			mc.target, wantTarget)
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

// TestNodeStageVolume_IdempotentHealthyMount verifies the health gate does
// not disturb the common case: a staged volume whose filesystem is alive
// returns idempotent success without re-mounting.
func TestNodeStageVolume_IdempotentHealthyMount(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-healthy",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:healthy", testStorageAddr),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("second NodeStageVolume: %v", err)
	}

	if got := len(env.mounter.formatAndMountCalls); got != 1 {
		t.Errorf("FormatAndMount called %d times, want 1 (no re-mount of a healthy stage)", got)
	}
	if len(env.mounter.unmountCalls) != 0 {
		t.Errorf("Unmount called %d times on a healthy staged mount, want 0", len(env.mounter.unmountCalls))
	}
	if got := len(env.mounter.checkHealthCalls); got < 1 {
		t.Errorf("CheckMountHealth not called on the idempotent path")
	}
}

// TestNodeStageVolume_StagedFilesystemShutdown_ReStages covers issue #168:
// the staged filesystem entered kernel shutdown (XFS forced shutdown,
// ext4 abort) while its device stayed healthy.  With no pod bind mounts
// left, NodeStageVolume must not report success — it must drop the dead
// mount and re-mount so the filesystem recovers (journal replay) without
// an operator forcing NodeUnstageVolume.
func TestNodeStageVolume_StagedFilesystemShutdown_ReStages(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const volumeID = "tank/pvc-shutdown"
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:shutdown", testStorageAddr),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}

	// The filesystem enters kernel shutdown while every mount table entry
	// stays in place — the signature a mount-table check alone cannot see.
	env.mounter.markUnhealthy(env.connector.devicePath)

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume on a dead staged filesystem: %v", err)
	}

	// The dead mount must have been unmounted and re-mounted, not simply
	// reported as still staged.
	if len(env.mounter.unmountCalls) != 1 || env.mounter.unmountCalls[0] != stagingPath {
		t.Fatalf("Unmount calls = %v, want exactly [%s]", env.mounter.unmountCalls, stagingPath)
	}
	if got := len(env.mounter.formatAndMountCalls); got != 2 {
		t.Errorf("FormatAndMount called %d times, want 2 (re-mount after drop)", got)
	}
	// After repair the mock clears the shutdown mark: the staged filesystem
	// must now probe healthy, proving the repair actually happened.
	if err := env.mounter.CheckMountHealth(stagingPath); err != nil {
		t.Errorf("staged filesystem still unhealthy after repair: %v", err)
	}
	state, err := env.srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState after repair: %v", err)
	}
	if state == nil {
		t.Error("stage state missing after repair")
	}
}

// TestNodeStageVolume_StagedFilesystemShutdown_PinnedByBind covers the same
// shutdown while a pod bind mount still references the dead superblock:
// unmount+mount would silently re-attach the same dead filesystem, so the
// RPC must fail instead of reporting success.  The mount stays in place so
// NodeUnstageVolume can still run once teardown removes the bind.
func TestNodeStageVolume_StagedFilesystemShutdown_PinnedByBind(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-pinned"
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:pinned", testStorageAddr),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}
	// A pod bind mount keeps the dead superblock alive.
	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:pinned", testStorageAddr),
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.Internal)

	// The dead staged mount must NOT be dropped: it is the only handle on
	// the shared filesystem whose other mounts survive, and re-mounting
	// would only re-attach the dead superblock.
	for _, target := range env.mounter.unmountCalls {
		if target == stagingPath {
			t.Fatalf("staging path was unmounted although pod binds still pin the filesystem")
		}
	}
	if got := len(env.mounter.formatAndMountCalls); got != 1 {
		t.Errorf("FormatAndMount called %d times, want 1 (no re-mount while pinned)", got)
	}
	mounted, _ := env.mounter.MountEntryExists(stagingPath) //nolint:errcheck // mock never errors here
	if !mounted {
		t.Error("staging mount was removed although the filesystem is still pinned")
	}

	// Once teardown removes the bind mount, the next stage repairs the
	// volume without operator action.
	if _, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume after bind removal: %v", err)
	}
	if err := env.mounter.CheckMountHealth(stagingPath); err != nil {
		t.Errorf("staged filesystem still unhealthy after repair: %v", err)
	}
}

// TestNodeStageVolume_StagedFilesystemShutdown_ProbeError covers a health
// probe that fails inconclusively (not ErrMountUnhealthy): nothing can be
// concluded about the filesystem, so the mount must not be dropped and the
// error must surface to the CO instead of an idempotent success.
func TestNodeStageVolume_StagedFilesystemShutdown_ProbeError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-probe",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:probe", testStorageAddr),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}

	env.mounter.checkHealthErr = errors.New("mountinfo unreadable")

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.Internal)

	if len(env.mounter.unmountCalls) != 0 {
		t.Errorf("Unmount called %d times on an inconclusive probe, want 0", len(env.mounter.unmountCalls))
	}
	mounted, _ := env.mounter.MountEntryExists(stagingPath) //nolint:errcheck // mock never errors
	if !mounted {
		t.Error("staging mount was dropped on an inconclusive probe")
	}
}

// TestNodeStageVolume_StagedFilesystemShutdown_HasOtherMountsError covers a
// dead filesystem whose mount-sharing check itself fails: the mount must
// not be dropped on an inconclusive check.
func TestNodeStageVolume_StagedFilesystemShutdown_HasOtherMountsError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-mounts-err",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:mounts-err", testStorageAddr),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}

	env.mounter.markUnhealthy(env.connector.devicePath)
	env.mounter.hasOtherMountsErr = errors.New("mountinfo parse failed")

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.Internal)

	if len(env.mounter.unmountCalls) != 0 {
		t.Errorf("Unmount called %d times on an inconclusive mount-sharing check, want 0",
			len(env.mounter.unmountCalls))
	}
}

// TestNodeStageVolume_StagedFilesystemShutdown_BlockSkipsProbe verifies the
// shutdown detection only applies to filesystem mounts: a Block-mode stage
// has no staged filesystem to probe and must keep returning idempotent
// success.
func TestNodeStageVolume_StagedFilesystemShutdown_BlockSkipsProbe(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-block-dead",
		StagingTargetPath: stagingPath,
		VolumeCapability:  blockCap(),
		VolumeContext:     mountVolumeContext("nqn.test:block-dead", testStorageAddr),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("second NodeStageVolume (block): %v", err)
	}
	if len(env.mounter.checkHealthCalls) != 0 {
		t.Errorf("CheckMountHealth called %d times for block access, want 0",
			len(env.mounter.checkHealthCalls))
	}
}

// TestNodeStageVolume_ReadonlyMountSkipsProbe pins the read-only contract:
// a filesystem staged with the "ro" mount flag answers the write probe
// with EROFS — the same signature as an ext4 remount-ro shutdown — so
// NodeStageVolume must not run the probe at all, on the first stage or on
// the idempotent retry.
func TestNodeStageVolume_ReadonlyMountSkipsProbe(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-stage-ro",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapRO("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:stage-ro", testStorageAddr),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("read-only NodeStageVolume: %v", err)
	}
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("idempotent read-only NodeStageVolume: %v", err)
	}
	if len(env.mounter.checkHealthCalls) != 0 {
		t.Errorf("CheckMountHealth called %d times on a read-only stage, want 0: "+
			"EROFS is the expected answer, not a dead filesystem", len(env.mounter.checkHealthCalls))
	}
	if got := len(env.mounter.unmountCalls); got != 0 {
		t.Errorf("Unmount called %d times on a read-only stage, want 0", got)
	}
	if got := len(env.mounter.formatAndMountCalls); got != 1 {
		t.Errorf("FormatAndMount called %d times, want 1", got)
	}
}

// TestNodeStageVolume_ReadonlyDeadStageFails covers the read-only side of
// issue #175: a filesystem staged "ro" skips the write probe, but when it
// enters kernel shutdown (mount entry kept, stat EIO) the idempotent retry
// must still fail rather than report the dead mount staged — the
// non-writing stat probe catches it — and nothing may be unmounted.
func TestNodeStageVolume_ReadonlyDeadStageFails(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-stage-ro-dead",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapRO("xfs"),
		VolumeContext:     mountVolumeContext("nqn.test:stage-ro-dead", testStorageAddr),
	}
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("read-only NodeStageVolume: %v", err)
	}
	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.Internal)
	if len(env.mounter.checkHealthCalls) != 0 {
		t.Errorf("write probe ran on a read-only stage: %v", env.mounter.checkHealthCalls)
	}
	if got := len(env.mounter.unmountCalls); got != 0 {
		t.Errorf("Unmount called %d times, want 0", got)
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
	mounted, _ := env.mounter.MountEntryExists(stagingPath) //nolint:errcheck // mock never returns an error
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

// TestNodeStageVolume_MountProbeError pins the strict-probe contract:
// NodeStageVolume must keep failing when the mount-table lookup for the
// staging path errors (e.g. an unreadable mountinfo).  A failed lookup must
// never look like a healthy staged volume (false-healthy) — a dead mount
// still listed in the table is handled by the health probe and repair,
// not by treating the mount as absent.
func TestNodeStageVolume_MountProbeError(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.mounter.mountEntryExistsErr = errors.New("mountinfo unreadable")

	stagingPath := t.TempDir()
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-probe-err",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:probe-err", testStorageAddr),
	})
	requireGRPCCode(t, err, codes.Internal)

	// The failed probe must not be mistaken for "already mounted": nothing
	// may be formatted or mounted on top of an unverifiable target.
	if len(env.mounter.formatAndMountCalls) != 0 || len(env.mounter.mountCalls) != 0 {
		t.Errorf("mount calls after probe failure: formatAndMount=%v mount=%v, want none",
			env.mounter.formatAndMountCalls, env.mounter.mountCalls)
	}
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
	mounted, _ := env.mounter.MountEntryExists(stagingPath) //nolint:errcheck // mock never returns an error
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

// TestNodeUnstageVolume_FilesystemMode_SingleUnmountTarget guards the
// AccessType-driven single-path dispatch: when state.AccessType is
// Filesystem, the unmount must touch ONLY stagingPath and never
// blockStagingDevicePath(stagingPath) (which lives inside the Filesystem
// mount and can return EIO during post-ControllerExpand NVMe namespace
// re-identify).  A regression that re-introduces the dual-path probe or
// unmount would surface that EIO as a gRPC Internal and trap kubelet in
// infinite UnmountDevice retries.
func TestNodeUnstageVolume_FilesystemMode_SingleUnmountTarget(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const (
		volumeID = "tank/pvc-fs-unstage"
		nqn      = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-fs-unstage"
	)

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// Reset the call logs so we only count NodeUnstageVolume's operations.
	env.mounter.mountEntryExistsCalls = nil
	env.mounter.unmountCalls = nil

	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	blockSentinel := blockStagingDevicePath(stagingPath)
	for _, probed := range env.mounter.mountEntryExistsCalls {
		if probed == blockSentinel {
			t.Errorf("NodeUnstageVolume probed block sentinel %q in Filesystem mode; calls=%v",
				blockSentinel, env.mounter.mountEntryExistsCalls)
		}
	}
	// Unmount is invoked unconditionally (the mounter owns idempotency and
	// corrupted-mount handling); the AccessType discriminator must route it
	// at the Filesystem staging root and nothing else.
	if len(env.mounter.unmountCalls) != 1 || env.mounter.unmountCalls[0] != stagingPath {
		t.Errorf("expected exactly one Unmount on %q; calls=%v",
			stagingPath, env.mounter.unmountCalls)
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

// TestNodeUnstageVolume_UnmountError verifies that a failed unmount aborts
// teardown at step 2: the RPC returns Internal, the transport session is
// NOT detached, and the stage state file is retained so the retried
// NodeUnstageVolume still has the parameters needed to finish teardown.
// Detaching first would strand the volume with no recoverable state.
func TestNodeUnstageVolume_UnmountError(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	env.mounter.unmountErr = errors.New("device busy")
	stagingPath := t.TempDir()
	const volumeID = "tank/pvc-umerr"

	// Stage first so the path ends up in the mounted set.
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:umerr", testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	requireGRPCCode(t, err, codes.Internal)

	// The transport must stay connected: no Detach may run before the mount
	// is actually torn down.
	if len(env.connector.disconnectCalls) != 0 {
		t.Errorf("Disconnect called %d times despite unmount failure, want 0",
			len(env.connector.disconnectCalls))
	}
	// The stage state file must survive so a retried NodeUnstageVolume can
	// still resolve the access type and the transport parameters.
	if remaining, err := env.srv.readStageState(volumeID); err != nil || remaining == nil {
		t.Errorf("stage state after failed unstage = %+v (err %v), want retained",
			remaining, err)
	}
	// The mount must still be present — the failed unmount must not be
	// reported as teardown progress.
	if !env.mounter.mountedPaths[stagingPath] {
		t.Error("staging path marked unmounted after failed unmount")
	}
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

// TestStageState_FailedWritePreservesPriorRecord verifies the issue #81
// invariant through the public NodeStageVolume RPC: when the state-file
// replacement fails mid-write, the previously committed record is preserved
// byte-for-byte.  The rewrite is forced to fail deterministically by running
// the re-stage in a subprocess whose RLIMIT_FSIZE is 0, so write(2) returns
// EFBIG — a controlled stand-in for the crash-window interruption that left a
// 0-byte record on the worker.  The limit is confined to the child process so
// the parent test's own file writes are unaffected.
func TestStageState_FailedWritePreservesPriorRecord(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const volumeID = "tank/pvc-failed-write"
	nqn := "nqn.2026-01.com.bhyoo.pillar-csi:" + strings.ReplaceAll(volumeID, "/", ".")
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(nqn, "192.0.2.1"),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("initial NodeStageVolume: %v", err)
	}
	stateFile := env.srv.stateFilePath(volumeID)
	before, err := os.ReadFile(stateFile) //nolint:gosec // G304: test reads state file under t.TempDir()
	if err != nil {
		t.Fatalf("read committed state file: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("state file is empty after successful NodeStageVolume")
	}

	// Simulate a node reboot: the mount table is empty but the committed
	// record survives, so the next NodeStageVolume re-attaches, re-mounts, and
	// rewrites the record.
	env.mounter.mountedPaths = map[string]bool{}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], //nolint:gosec // G204: re-executes own test binary as a helper
		"-test.run=^TestStageStateFailedWriteHelper$")
	cmd.Env = append(os.Environ(),
		"PILLAR_TEST_RLIMIT_FSIZE_HELPER=1",
		"PILLAR_TEST_STATE_DIR="+env.stateDir,
		"PILLAR_TEST_STAGING_PATH="+stagingPath,
		"PILLAR_TEST_VOLUME_ID="+volumeID,
	)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("helper subprocess failed: %v\n%s", runErr, out)
	}

	after, err := os.ReadFile(stateFile) //nolint:gosec // G304: test reads state file under t.TempDir()
	if err != nil {
		t.Fatalf("read state file after failed re-stage: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Errorf("committed record changed by failed write: got %q, want %q", after, before)
	}
}

// TestStageState_FailedRewriteWhenMountedPreservesRecord verifies the retry
// side of the issue #81 ack contract through the public NodeStageVolume RPC:
// when a state file exists and the staging path is still mounted, a failed
// record rewrite surfaces an Internal error instead of acknowledging success
// on an unverified record, the committed record is preserved, and a later
// retry without the fault succeeds.
func TestStageState_FailedRewriteWhenMountedPreservesRecord(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	const volumeID = "tank/pvc-mounted-rewrite"
	nqn := "nqn.2026-01.com.bhyoo.pillar-csi:" + strings.ReplaceAll(volumeID, "/", ".")
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(nqn, "192.0.2.1"),
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("initial NodeStageVolume: %v", err)
	}
	stateFile := env.srv.stateFilePath(volumeID)
	before, err := os.ReadFile(stateFile) //nolint:gosec // G304: test reads state file under t.TempDir()
	if err != nil {
		t.Fatalf("read committed state file: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("state file is empty after successful NodeStageVolume")
	}

	// The mount survived (env.mounter still reports stagingPath mounted), so
	// the helper hits the already-staged fast path, which must re-acknowledge
	// the record's durability.  The helper subprocess fails the rewrite with
	// RLIMIT_FSIZE=0, confined to the child process.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], //nolint:gosec // G204: re-executes own test binary as a helper
		"-test.run=^TestStageStateFailedWriteHelper$")
	cmd.Env = append(os.Environ(),
		"PILLAR_TEST_RLIMIT_FSIZE_HELPER=1",
		"PILLAR_TEST_HELPER_MOUNTED=1",
		"PILLAR_TEST_STATE_DIR="+env.stateDir,
		"PILLAR_TEST_STAGING_PATH="+stagingPath,
		"PILLAR_TEST_VOLUME_ID="+volumeID,
	)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("helper subprocess failed: %v\n%s", runErr, out)
	}

	after, err := os.ReadFile(stateFile) //nolint:gosec // G304: test reads state file under t.TempDir()
	if err != nil {
		t.Fatalf("read state file after failed re-stage: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Errorf("committed record changed by failed write: got %q, want %q", after, before)
	}

	// With the limit confined to the subprocess, the retry in this process
	// succeeds and re-acknowledges the durable record.
	_, retryErr := env.srv.NodeStageVolume(context.Background(), req)
	if retryErr != nil {
		t.Fatalf("NodeStageVolume retry after restored limit: %v", retryErr)
	}
	final, err := os.ReadFile(stateFile) //nolint:gosec // G304: test reads state file under t.TempDir()
	if err != nil {
		t.Fatalf("read state file after retry: %v", err)
	}
	if !bytes.Equal(final, before) {
		t.Errorf("record after retry = %q, want unchanged %q", final, before)
	}
}

// TestStageStateFailedWriteHelper runs in a re-executed subprocess for the
// failed-write regression tests above.  It sets RLIMIT_FSIZE to 0 (with
// SIGXFSZ ignored so write(2) returns EFBIG instead of terminating the
// process) and re-issues NodeStageVolume, which must fail while persisting
// the stage state.  When PILLAR_TEST_HELPER_MOUNTED is set the mock mounter
// reports the staging path still mounted, driving the already-staged fast
// path; otherwise the full re-stage path runs.  Without the marker
// environment variable it returns immediately so a normal test run is a
// no-op.
func TestStageStateFailedWriteHelper(t *testing.T) {
	if os.Getenv("PILLAR_TEST_RLIMIT_FSIZE_HELPER") == "" {
		return
	}

	signal.Ignore(syscall.SIGXFSZ)
	var prev syscall.Rlimit
	getErr := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &prev)
	if getErr != nil {
		t.Fatalf("getrlimit RLIMIT_FSIZE: %v", getErr)
	}
	rlimErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: prev.Max})
	if rlimErr != nil {
		t.Fatalf("setrlimit RLIMIT_FSIZE: %v", rlimErr)
	}
	// The limit must be lifted before this test returns: the test binary
	// writes its coverage profile on exit and any file write still capped at
	// RLIMIT_FSIZE=0 fails with EFBIG.  The deferred restore covers panic and
	// Goexit paths; the explicit restore below runs before assertions.
	defer func() {
		deferredErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &prev)
		if deferredErr != nil {
			t.Errorf("deferred RLIMIT_FSIZE restore failed: %v", deferredErr)
		}
	}()

	mnt := newMockMounter()
	if os.Getenv("PILLAR_TEST_HELPER_MOUNTED") != "" {
		mnt.mountedPaths[os.Getenv("PILLAR_TEST_STAGING_PATH")] = true
	}
	srv := NewNodeServerWithStateDir("test-node",
		&mockConnector{devicePath: "/dev/nvme0n1"},
		mnt,
		os.Getenv("PILLAR_TEST_STATE_DIR"))
	volumeID := os.Getenv("PILLAR_TEST_VOLUME_ID")
	nqn := "nqn.2026-01.com.bhyoo.pillar-csi:" + strings.ReplaceAll(volumeID, "/", ".")
	_, err := srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: os.Getenv("PILLAR_TEST_STAGING_PATH"),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(nqn, "192.0.2.1"),
	})
	// Lift the cap before evaluating the result so assertion output and the
	// coverage profile written at process exit are not capped by the limit.
	restoreErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &prev)
	if restoreErr != nil {
		t.Fatalf("restore RLIMIT_FSIZE: %v", restoreErr)
	}
	if err == nil {
		t.Fatal("NodeStageVolume succeeded despite RLIMIT_FSIZE=0; want persist-stage-state failure")
	}
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "persist stage state") {
		t.Fatalf("NodeStageVolume failed at an unexpected step: %v", err)
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
			name:         "VolumeContext value wins over volumeID",
			volumeID:     "storage-node/nvmeof-tcp/zfs-zvol/tank/pvc-abc",
			volCtx:       map[string]string{VolumeContextKeyProtocolType: "iscsi"},
			wantProtocol: "iscsi",
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

	s := stageStateFromAttachResult(
		ProtocolNVMeoFTCP, AccessTypeFilesystem,
		"nqn.test:vol", testStorageAddr, "4420", result)
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

	s := stageStateFromAttachResult("fibrechannel", AccessTypeFilesystem, "target:fc1", "", "", nil)
	if s.ProtocolType != "fibrechannel" {
		t.Errorf("ProtocolType = %q, want %q", s.ProtocolType, "fibrechannel")
	}
	if s.NVMeoF != nil {
		t.Error("NVMeoF should be nil for unknown protocol")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeStageVolume — preserve-original adoption (issue #163)
// ─────────────────────────────────────────────────────────────────────────────.

// preserveOriginalVolumeContextKey is the wire spelling of the VolumeContext
// key the controller sets on an adopted LV whose data must be preserved; the
// value "true" restricts the node to mounting the filesystem already on the
// device.
const preserveOriginalVolumeContextKey = "pillar-csi.bhyoo.com/preserve-original"

// preserveNQN is the NVMe-oF target of the adopted volume in these tests.
const preserveNQN = "nqn.2026-01.com.bhyoo.pillar-csi:vg0.lv-adopted"

// preserveVolumeID is the CSI volume ID of the adopted LV in these tests.
const preserveVolumeID = "vg0/lv-adopted"

// adoptedPreserveVolumeContext returns the mountVolumeContext of the adopted
// volume (preserveNQN at testStorageAddr) plus the preserve-original key set
// to "true".
func adoptedPreserveVolumeContext() map[string]string {
	volCtx := mountVolumeContext(preserveNQN, testStorageAddr)
	volCtx[preserveOriginalVolumeContextKey] = "true"
	return volCtx
}

// setPersistedStageField seeds one field of the on-disk stage record as JSON,
// the way a record written by an earlier plugin process carries it.  It is
// fixture setup only; tests prove the effect through later RPCs.
func setPersistedStageField(t *testing.T, srv *NodeServer, volumeID, key string, value any) {
	t.Helper()
	path := srv.stateFilePath(volumeID)
	data, err := os.ReadFile(path) //nolint:gosec // G304: test-owned state directory
	if err != nil {
		t.Fatalf("read stage record of %q: %v", volumeID, err)
	}
	record := map[string]any{}
	err = json.Unmarshal(data, &record)
	if err != nil {
		t.Fatalf("decode stage record of %q: %v", volumeID, err)
	}
	record[key] = value
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatalf("encode stage record of %q: %v", volumeID, err)
	}
	err = os.WriteFile(path, data, 0o600)
	if err != nil {
		t.Fatalf("write stage record of %q: %v", volumeID, err)
	}
}

// requirePreservedAcrossRestart proves the preservation is durable node
// state, not request state: a plugin restarted over the same state directory
// receives no VolumeContext on NodeExpandVolume, yet must refuse to resize the
// volume with FailedPrecondition and never run the resize tool.
func requirePreservedAcrossRestart(
	t *testing.T, conn Connector, mnt Mounter, stateDir, volumePath string, volCap *csi.VolumeCapability,
) {
	t.Helper()
	resizer := &mockResizer{}
	restarted := NewNodeServerWithStateDir("test-node", conn, mnt, stateDir).WithResizer(resizer)
	_, err := restarted.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:          preserveVolumeID,
		VolumePath:        volumePath,
		StagingTargetPath: volumePath,
		VolumeCapability:  volCap,
		CapacityRange:     &csi.CapacityRange{RequiredBytes: 2 << 30},
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("NodeExpandVolume after restart: gRPC code = %v, want %v (err: %v); "+
			"the preservation did not survive the restart", got, codes.FailedPrecondition, err)
	}
	if resizer.called != 0 {
		t.Errorf("resize tool ran %d times on a preserved volume after restart, want 0", resizer.called)
	}
}

// resetFormatJournal forgets the recorded format-and-mount, mkfs and fsck
// history so a test asserts only what the next RPC does to the device.
func (m *mockMounter) resetFormatJournal() {
	m.formatAndMountCalls = nil
	m.mkfsDevices = nil
	m.fsckDevices = nil
}

// requireDeviceUntouched asserts that the device still carries wantFormat and
// that nothing formatted, checked, or repaired it: the format-and-mount path
// (mkfs on a blank device, fsck before a read-write mount) never ran.
func requireDeviceUntouched(t *testing.T, m *mockMounter, device, wantFormat string) {
	t.Helper()
	if got := m.diskFormat[device]; got != wantFormat {
		t.Errorf("device %s signature = %q, want %q unchanged", device, got, wantFormat)
	}
	if len(m.mkfsDevices) != 0 {
		t.Errorf("mkfs ran on %v, want no format of a preserved device", m.mkfsDevices)
	}
	if len(m.fsckDevices) != 0 {
		t.Errorf("fsck ran on %v, want no check or repair of a preserved device", m.fsckDevices)
	}
	if len(m.formatAndMountCalls) != 0 {
		t.Errorf("format-and-mount path ran %d times (%+v), want 0 for a preserved device",
			len(m.formatAndMountCalls), m.formatAndMountCalls)
	}
}

// requireFailedPrecondition fails t (without stopping it) unless err carries
// codes.FailedPrecondition.
func requireFailedPrecondition(t *testing.T, err error) {
	t.Helper()
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("gRPC code = %v, want %v (err: %v)", got, codes.FailedPrecondition, err)
	}
}

// TestNodeStageVolume_PreserveOriginal_MountsExisting verifies that a
// preserve-original volume whose device already carries the requested
// filesystem is mounted as is — never formatted, fsck-checked or repaired —
// and that the preservation survives a plugin restart (resize refused).
func TestNodeStageVolume_PreserveOriginal_MountsExisting(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	device := env.connector.devicePath
	env.mounter.diskFormat[device] = "ext4"
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          preserveVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     adoptedPreserveVolumeContext(),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	requireDeviceUntouched(t, env.mounter, device, "ext4")
	if source, srcErr := env.mounter.MountSource(stagingPath); srcErr != nil || source != device {
		t.Errorf("staging mount source = %q (err %v), want %q", source, srcErr, device)
	}
	requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCap("ext4"))
}

// TestNodeStageVolume_PreserveOriginal_BlankOrMismatchRefused verifies that a
// preserve-original volume is refused with FailedPrecondition when its
// device carries no filesystem or one of another type: the device is left
// exactly as it was and nothing is mounted.
func TestNodeStageVolume_PreserveOriginal_BlankOrMismatchRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		onDisk string
		volCap *csi.VolumeCapability
	}{
		{name: "blank device", onDisk: "", volCap: mountCap("ext4")},
		{name: "blank device staged read-only", onDisk: "", volCap: mountCapRO("ext4")},
		{name: "xfs device for an ext4 request", onDisk: "xfs", volCap: mountCap("ext4")},
		{name: "ext4 device for an xfs request", onDisk: "ext4", volCap: mountCap("xfs")},
		{name: "xfs device for the default fs type", onDisk: "xfs", volCap: mountCap("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := newNodeTestEnv(t)
			device := env.connector.devicePath
			env.mounter.diskFormat[device] = tc.onDisk
			stagingPath := t.TempDir()

			_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          preserveVolumeID,
				StagingTargetPath: stagingPath,
				VolumeCapability:  tc.volCap,
				VolumeContext:     adoptedPreserveVolumeContext(),
			})
			requireFailedPrecondition(t, err)
			requireDeviceUntouched(t, env.mounter, device, tc.onDisk)
			if mounted, _ := env.mounter.MountEntryExists(stagingPath); mounted { //nolint:errcheck // mock never errors here
				t.Error("staging path mounted although the stage was refused")
			}
		})
	}
}

// stagePreservedForRestage stages a preserve-original ext4 volume, pins the
// preservation in its stage record (as the stage that adopted it recorded
// it), and clears the format journal so the caller observes only the
// re-stage.
func stagePreservedForRestage(t *testing.T, env *nodeTestEnv, stagingPath string) {
	t.Helper()
	env.mounter.diskFormat[env.connector.devicePath] = "ext4"
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          preserveVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     adoptedPreserveVolumeContext(),
	})
	if err != nil {
		t.Fatalf("initial NodeStageVolume: %v", err)
	}
	setPersistedStageField(t, env.srv, preserveVolumeID, "preserve_original", true)
	env.mounter.resetFormatJournal()
}

// rebootNode drops the staging mount the way a node reboot does and returns
// a fresh NodeServer over the same state directory (a restarted plugin).
func rebootNode(t *testing.T, env *nodeTestEnv, stagingPath string) *NodeServer {
	t.Helper()
	err := env.mounter.Unmount(stagingPath)
	if err != nil {
		t.Fatalf("simulate reboot: %v", err)
	}
	env.mounter.resetFormatJournal()
	return NewNodeServerWithStateDir("test-node", env.connector, env.mounter, env.stateDir)
}

// TestNodeStageVolume_PreservePinSurvivesMissingVC verifies that the
// preservation pinned in the stage record governs every later stage of the
// volume, even one whose VolumeContext lacks the key, and that the key
// upgrades an unpinned record but nothing downgrades a pinned one.
func TestNodeStageVolume_PreservePinSurvivesMissingVC(t *testing.T) {
	t.Parallel()

	t.Run("pinned record remounts the existing filesystem", func(t *testing.T) {
		t.Parallel()
		env := newNodeTestEnv(t)
		stagingPath := t.TempDir()
		stagePreservedForRestage(t, env, stagingPath)
		restarted := rebootNode(t, env, stagingPath)

		_, err := restarted.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          preserveVolumeID,
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountCap("ext4"),
			VolumeContext:     mountVolumeContext(preserveNQN, testStorageAddr),
		})
		if err != nil {
			t.Fatalf("re-stage after reboot: %v", err)
		}
		requireDeviceUntouched(t, env.mounter, env.connector.devicePath, "ext4")
		if source, srcErr := env.mounter.MountSource(stagingPath); srcErr != nil || source != env.connector.devicePath {
			t.Errorf("staging mount source = %q (err %v), want %q", source, srcErr, env.connector.devicePath)
		}
		requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCap("ext4"))
	})

	t.Run("pinned record refuses a blank device", func(t *testing.T) {
		t.Parallel()
		env := newNodeTestEnv(t)
		stagingPath := t.TempDir()
		stagePreservedForRestage(t, env, stagingPath)
		restarted := rebootNode(t, env, stagingPath)
		// The signature is gone (e.g. wiped out of band): the pinned volume
		// must not be re-created empty.
		env.mounter.diskFormat[env.connector.devicePath] = ""

		_, err := restarted.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          preserveVolumeID,
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountCap("ext4"),
			VolumeContext:     mountVolumeContext(preserveNQN, testStorageAddr),
		})
		requireFailedPrecondition(t, err)
		requireDeviceUntouched(t, env.mounter, env.connector.devicePath, "")
		if mounted, _ := env.mounter.MountEntryExists(stagingPath); mounted { //nolint:errcheck // mock never errors here
			t.Error("staging path mounted although the stage was refused")
		}
		requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCap("ext4"))
	})

	t.Run("key upgrades an unpinned record", func(t *testing.T) {
		t.Parallel()
		env := newNodeTestEnv(t)
		stagingPath := t.TempDir()
		env.mounter.diskFormat[env.connector.devicePath] = "ext4"
		_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          preserveVolumeID,
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountCap("ext4"),
			VolumeContext:     mountVolumeContext(preserveNQN, testStorageAddr),
		})
		if err != nil {
			t.Fatalf("initial NodeStageVolume without the key: %v", err)
		}
		restarted := rebootNode(t, env, stagingPath)

		_, err = restarted.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          preserveVolumeID,
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountCap("ext4"),
			VolumeContext:     adoptedPreserveVolumeContext(),
		})
		if err != nil {
			t.Fatalf("re-stage with the key: %v", err)
		}
		requireDeviceUntouched(t, env.mounter, env.connector.devicePath, "ext4")
		requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCap("ext4"))
	})

	t.Run("idempotent restage without the key keeps the pin", func(t *testing.T) {
		t.Parallel()
		env := newNodeTestEnv(t)
		stagingPath := t.TempDir()
		stagePreservedForRestage(t, env, stagingPath)

		// Still mounted: the restage takes the idempotent path and rewrites
		// the record; a request without the key must not clear the pin.
		_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          preserveVolumeID,
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountCap("ext4"),
			VolumeContext:     mountVolumeContext(preserveNQN, testStorageAddr),
		})
		if err != nil {
			t.Fatalf("idempotent re-stage without the key: %v", err)
		}
		requireDeviceUntouched(t, env.mounter, env.connector.devicePath, "ext4")
		requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCap("ext4"))
	})
}

// TestNodeStageVolume_PreserveOriginal_DeadStageRemountsExisting verifies
// that re-staging a pinned volume whose staged filesystem entered kernel
// shutdown drops the dead mount and re-mounts the existing filesystem
// without the format-and-mount path (no fsck or repair step).
func TestNodeStageVolume_PreserveOriginal_DeadStageRemountsExisting(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	stagePreservedForRestage(t, env, stagingPath)
	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          preserveVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(preserveNQN, testStorageAddr),
	})
	if err != nil {
		t.Fatalf("re-stage of a dead preserved filesystem: %v", err)
	}
	requireDeviceUntouched(t, env.mounter, env.connector.devicePath, "ext4")
	if healthErr := env.mounter.CheckMountHealth(stagingPath); healthErr != nil {
		t.Errorf("staged filesystem not re-mounted healthy: %v", healthErr)
	}
	requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCap("ext4"))
}

// TestNodeStageVolume_PreserveKeyRestagesUnpinnedDeadMount verifies that the
// VolumeContext key upgrades the preservation before the dead-mount repair
// of a record staged without the pin: the still-mounted, kernel-shutdown
// staged filesystem is dropped and re-mounted from the unchanged device
// with no mkfs, fsck or format-and-mount, it comes back healthy, and the
// upgraded pin survives a plugin restart (resize refused).
func TestNodeStageVolume_PreserveKeyRestagesUnpinnedDeadMount(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	device := env.connector.devicePath
	env.mounter.diskFormat[device] = "ext4"
	stagingPath := t.TempDir()
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          preserveVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext(preserveNQN, testStorageAddr),
	})
	if err != nil {
		t.Fatalf("unpinned NodeStageVolume: %v", err)
	}
	env.mounter.markUnhealthy(device)
	env.mounter.resetFormatJournal()

	_, err = env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          preserveVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     adoptedPreserveVolumeContext(),
	})
	if err != nil {
		t.Fatalf("re-stage of a dead unpinned filesystem with the key: %v", err)
	}
	requireDeviceUntouched(t, env.mounter, device, "ext4")
	if healthErr := env.mounter.CheckMountHealth(stagingPath); healthErr != nil {
		t.Errorf("staged filesystem not re-mounted healthy: %v", healthErr)
	}
	requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCap("ext4"))
}

// TestNodeStageVolume_PreserveOriginal_BlockMode verifies that a raw block
// preserve-original volume is bound without any filesystem probe or format
// — even on a blank device — and that the preservation survives a plugin
// restart (resize refused).
func TestNodeStageVolume_PreserveOriginal_BlockMode(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	device := env.connector.devicePath
	env.mounter.diskFormat[device] = ""
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          preserveVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  blockCap(),
		VolumeContext:     adoptedPreserveVolumeContext(),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume (block): %v", err)
	}
	requireDeviceUntouched(t, env.mounter, device, "")
	bindTarget := blockStagingDevicePath(stagingPath)
	if source, srcErr := env.mounter.MountSource(bindTarget); srcErr != nil || source != device {
		t.Errorf("block bind source = %q (err %v), want %q", source, srcErr, device)
	}
	requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, blockCap())
}

// TestNodeStageVolume_PreserveOriginal_ReadOnlyStage verifies that a
// preserve-original volume staged read-only is mounted read-only from its
// existing filesystem and is health-checked only with the non-writing probe,
// both when staged and when the stage is repeated.
func TestNodeStageVolume_PreserveOriginal_ReadOnlyStage(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	device := env.connector.devicePath
	env.mounter.diskFormat[device] = "ext4"
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          preserveVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapRO("ext4"),
		VolumeContext:     adoptedPreserveVolumeContext(),
	}

	for attempt := range 2 {
		_, err := env.srv.NodeStageVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("NodeStageVolume attempt %d: %v", attempt+1, err)
		}
	}
	requireDeviceUntouched(t, env.mounter, device, "ext4")
	if !env.mounter.mountRO[stagingPath] {
		t.Error("read-only stage of a preserved volume is not mounted read-only")
	}
	if slices.Contains(env.mounter.checkHealthCalls, stagingPath) {
		t.Errorf("read-only staged mount was write-probed: %v", env.mounter.checkHealthCalls)
	}
	requirePreservedAcrossRestart(t, env.connector, env.mounter, env.stateDir, stagingPath, mountCapRO("ext4"))
}
