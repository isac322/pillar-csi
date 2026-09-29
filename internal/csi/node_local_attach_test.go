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

// Tests for the local attach path of NodeStageVolume, NodeUnstageVolume and
// NodeExpandVolume, and for the dmsetup-backed DeviceMapper.
//
// Run with:
//
//	go test ./internal/csi/ -v -run 'Local|DeviceMapper'

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fake DeviceMapper
// ─────────────────────────────────────────────────────────────────────────────

type dmCall struct {
	name, backing string
}

// fakeDeviceMapper records DeviceMapper calls and returns programmed errors.
type fakeDeviceMapper struct {
	ensureErr error
	reloadErr error
	removeErr error

	ensureCalls []dmCall
	reloadCalls []dmCall
	removeCalls []string
}

func (f *fakeDeviceMapper) EnsureLinear(_ context.Context, name, backing string) (string, error) {
	f.ensureCalls = append(f.ensureCalls, dmCall{name, backing})
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return "/dev/mapper/" + name, nil
}

func (f *fakeDeviceMapper) ReloadLinear(_ context.Context, name, backing string) error {
	f.reloadCalls = append(f.reloadCalls, dmCall{name, backing})
	return f.reloadErr
}

func (f *fakeDeviceMapper) Remove(_ context.Context, name string) error {
	f.removeCalls = append(f.removeCalls, name)
	return f.removeErr
}

var _ DeviceMapper = (*fakeDeviceMapper)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

const (
	localTestVolumeID = "pool/nvmeof-tcp/zfs-zvol/pvc-local"
	localTestBacking  = "/dev/zvol/pool/pvc-local"
	localTestNQN      = "nqn.2026-01.com.bhyoo.pillar-csi:pool.pvc-local"
)

type localTestEnv struct {
	*nodeTestEnv
	dm *fakeDeviceMapper
	// nvmetRoot is the fake nvmet configfs root; it exists and holds no
	// subsystem, so the export of the test volume reads as fenced.
	nvmetRoot string
}

func newLocalTestEnv(t *testing.T) *localTestEnv {
	t.Helper()
	env := newNodeTestEnv(t)
	dm := &fakeDeviceMapper{}
	nvmetRoot := filepath.Join(t.TempDir(), "nvmet")
	if err := os.MkdirAll(filepath.Join(nvmetRoot, "subsystems"), 0o750); err != nil {
		t.Fatalf("create fake nvmet root: %v", err)
	}
	env.srv.WithDeviceMapper(dm).WithNvmetConfigfsRoot(nvmetRoot)
	return &localTestEnv{nodeTestEnv: env, dm: dm, nvmetRoot: nvmetRoot}
}

// setNamespaceEnable creates namespace nsid of the test volume's subsystem
// in the fake nvmet root with the given enable value.
func (e *localTestEnv) setNamespaceEnable(t *testing.T, nsid, enable string) {
	t.Helper()
	nsDir := filepath.Join(e.nvmetRoot, "subsystems", localTestNQN, "namespaces", nsid)
	if err := os.MkdirAll(nsDir, 0o750); err != nil {
		t.Fatalf("create namespace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nsDir, "enable"), []byte(enable+"\n"), 0o600); err != nil {
		t.Fatalf("write namespace enable: %v", err)
	}
}

func localPublishContext(node, device string) map[string]string {
	return map[string]string{
		PublishContextKeyAttachMode:      AttachModeLocal,
		PublishContextKeyLocalNode:       node,
		PublishContextKeyLocalDevicePath: device,
	}
}

func localStageRequest(stagingPath string, volCap *csi.VolumeCapability) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId:          localTestVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  volCap,
		// Only the subsystem NQN (for the export fence check): a local
		// attach must not require the NVMe-oF address or port.
		VolumeContext:  map[string]string{VolumeContextKeyTargetID: localTestNQN},
		PublishContext: localPublishContext("test-node", localTestBacking),
	}
}

func requireNoProtocolCalls(t *testing.T, conn *mockConnector) {
	t.Helper()
	if len(conn.connectCalls) != 0 || len(conn.getDeviceCalls) != 0 || len(conn.disconnectCalls) != 0 {
		t.Errorf("protocol handler used by a local attach: connect=%v getDevice=%v disconnect=%v",
			conn.connectCalls, conn.getDeviceCalls, conn.disconnectCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeStageVolume
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeStageVolume_Local_Mount(t *testing.T) {
	env := newLocalTestEnv(t)
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, mountCap("xfs")))
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	wantName := LocalDMName(localTestVolumeID)
	if len(env.dm.ensureCalls) != 1 || env.dm.ensureCalls[0] != (dmCall{wantName, localTestBacking}) {
		t.Fatalf("EnsureLinear calls = %v, want [{%s %s}]", env.dm.ensureCalls, wantName, localTestBacking)
	}
	requireFormatAndMount(t, env.mounter, formatAndMountCall{
		source: "/dev/mapper/" + wantName, target: stagingPath, fsType: "xfs",
	})
	requireNoProtocolCalls(t, env.connector)
	requireLocalStageState(t, env.srv, localTestVolumeID, wantName)
}

// requireFormatAndMount fails t unless the mounter ran exactly want.
func requireFormatAndMount(t *testing.T, m *mockMounter, want formatAndMountCall) {
	t.Helper()
	if len(m.formatAndMountCalls) != 1 {
		t.Fatalf("FormatAndMount calls = %d, want 1", len(m.formatAndMountCalls))
	}
	if call := m.formatAndMountCalls[0]; call.source != want.source ||
		call.target != want.target || call.fsType != want.fsType {
		t.Errorf("FormatAndMount(%q, %q, %q), want the dm device onto the staging path as %s",
			call.source, call.target, call.fsType, want.fsType)
	}
}

// requireLocalStageState fails t unless volumeID staged as a local attach of
// the dm device dmName over the test backend, mounted as xfs.
func requireLocalStageState(t *testing.T, srv *NodeServer, volumeID, dmName string) {
	t.Helper()
	state, err := srv.readStageState(volumeID)
	if err != nil || state == nil {
		t.Fatalf("readStageState = %v, %v", state, err)
	}
	if !state.isLocalAttach() || state.Local == nil ||
		state.Local.DMName != dmName || state.Local.BackingDevice != localTestBacking {
		t.Errorf("stage state = %+v (local %+v), want local attach of %s over %s",
			state, state.Local, dmName, localTestBacking)
	}
	if state.AccessType != AccessTypeFilesystem || state.FsType != "xfs" {
		t.Errorf("stage state access/fs = %q/%q, want filesystem/xfs", state.AccessType, state.FsType)
	}
}

func TestNodeStageVolume_Local_Block(t *testing.T) {
	env := newLocalTestEnv(t)
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, blockCap()))
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	if len(env.mounter.formatAndMountCalls) != 0 {
		t.Errorf("block local attach must not format: %v", env.mounter.formatAndMountCalls)
	}
	if len(env.mounter.mountCalls) != 1 {
		t.Fatalf("Mount calls = %d, want 1", len(env.mounter.mountCalls))
	}
	call := env.mounter.mountCalls[0]
	wantSource := "/dev/mapper/" + LocalDMName(localTestVolumeID)
	if call.source != wantSource || call.target != blockStagingDevicePath(stagingPath) {
		t.Errorf("Mount(%q, %q), want bind of %q onto %q",
			call.source, call.target, wantSource, blockStagingDevicePath(stagingPath))
	}
	requireNoProtocolCalls(t, env.connector)

	state, err := env.srv.readStageState(localTestVolumeID)
	if err != nil || state == nil || !state.isLocalAttach() || state.AccessType != AccessTypeBlock {
		t.Fatalf("stage state = %+v, %v; want local block attach", state, err)
	}
}

func TestNodeStageVolume_Local_WrongNodeRefused(t *testing.T) {
	env := newLocalTestEnv(t)
	req := localStageRequest(t.TempDir(), mountCap("ext4"))
	req.PublishContext = localPublishContext("storage-node", localTestBacking)

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.FailedPrecondition)

	if len(env.dm.ensureCalls) != 0 || len(env.mounter.formatAndMountCalls) != 0 {
		t.Errorf("refused local attach touched the device: ensure=%v format=%v",
			env.dm.ensureCalls, env.mounter.formatAndMountCalls)
	}
	requireNoProtocolCalls(t, env.connector)
}

func TestNodeStageVolume_Local_MissingDevicePath(t *testing.T) {
	env := newLocalTestEnv(t)
	req := localStageRequest(t.TempDir(), mountCap("ext4"))
	req.PublishContext = localPublishContext("test-node", "")

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.InvalidArgument)
	if len(env.dm.ensureCalls) != 0 {
		t.Errorf("EnsureLinear called without a device path: %v", env.dm.ensureCalls)
	}
}

func TestNodeStageVolume_Local_DeviceUnavailable(t *testing.T) {
	env := newLocalTestEnv(t)
	env.dm.ensureErr = fmt.Errorf("stat %q: %w", localTestBacking, ErrLocalDeviceUnavailable)

	_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(t.TempDir(), mountCap("ext4")))
	requireGRPCCode(t, err, codes.FailedPrecondition)

	if len(env.mounter.formatAndMountCalls) != 0 {
		t.Errorf("FormatAndMount called for an unavailable device: %v", env.mounter.formatAndMountCalls)
	}
	state, readErr := env.srv.readStageState(localTestVolumeID)
	if readErr != nil || state != nil {
		t.Errorf("stage state after failed attach = %+v, %v; want none", state, readErr)
	}
}

func TestNodeStageVolume_Local_DeviceMapperFailureIsInternal(t *testing.T) {
	env := newLocalTestEnv(t)
	env.dm.ensureErr = errors.New("dmsetup create: Device or resource busy")

	_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(t.TempDir(), mountCap("ext4")))
	requireGRPCCode(t, err, codes.Internal)
}

// requireLocalClaimReleased fails t unless the failed stage removed the
// device-mapper claim of the test volume and left no stage state behind.
func requireLocalClaimReleased(t *testing.T, env *localTestEnv) {
	t.Helper()
	want := LocalDMName(localTestVolumeID)
	if len(env.dm.ensureCalls) != 1 {
		t.Errorf("EnsureLinear calls = %v, want one claim", env.dm.ensureCalls)
	}
	if len(env.dm.removeCalls) != 1 || env.dm.removeCalls[0] != want {
		t.Errorf("Remove calls = %v, want [%s]: the claim must not outlive a failed stage", env.dm.removeCalls, want)
	}
	state, readErr := env.srv.readStageState(localTestVolumeID)
	if readErr != nil || state != nil {
		t.Errorf("stage state after failed stage = %+v, %v; want none", state, readErr)
	}
}

// TestNodeStageVolume_Local_ExportStillEnabledRefused verifies the node half
// of the export fence: when a namespace of the volume's subsystem is still
// enabled after the claim was created, the stage is refused with
// FailedPrecondition, the claim is removed and nothing is mounted.
func TestNodeStageVolume_Local_ExportStillEnabledRefused(t *testing.T) {
	env := newLocalTestEnv(t)
	env.setNamespaceEnable(t, "1", "0")
	env.setNamespaceEnable(t, "2", "1")

	_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(t.TempDir(), mountCap("ext4")))
	requireGRPCCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "still serving remote initiators") {
		t.Errorf("error = %v, want it to name the serving export", err)
	}

	requireLocalClaimReleased(t, env)
	if len(env.mounter.formatAndMountCalls) != 0 || len(env.mounter.mountCalls) != 0 {
		t.Errorf("mounted while the export is enabled: formatAndMount=%v mount=%v",
			env.mounter.formatAndMountCalls, env.mounter.mountCalls)
	}
}

// TestNodeStageVolume_Local_ExportFencedProceeds verifies that the stage
// proceeds when the subsystem is absent (no export can serve I/O) or all of
// its namespaces are disabled.
func TestNodeStageVolume_Local_ExportFencedProceeds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, env *localTestEnv)
	}{
		{name: "subsystem absent", setup: func(*testing.T, *localTestEnv) {}},
		{name: "namespaces disabled", setup: func(t *testing.T, env *localTestEnv) {
			env.setNamespaceEnable(t, "1", "0")
			env.setNamespaceEnable(t, "2", "0")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newLocalTestEnv(t)
			tc.setup(t, env)
			stagingPath := t.TempDir()

			_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, mountCap("xfs")))
			if err != nil {
				t.Fatalf("NodeStageVolume: %v", err)
			}
			if len(env.dm.removeCalls) != 0 {
				t.Errorf("Remove calls = %v, want none", env.dm.removeCalls)
			}
			requireLocalStageState(t, env.srv, localTestVolumeID, LocalDMName(localTestVolumeID))
		})
	}
}

// TestNodeStageVolume_Local_NvmetRootAbsentFails verifies that a missing
// nvmet configfs root — the export state cannot be verified — fails the
// stage and removes the claim.
func TestNodeStageVolume_Local_NvmetRootAbsentFails(t *testing.T) {
	env := newLocalTestEnv(t)
	env.srv.WithNvmetConfigfsRoot(filepath.Join(t.TempDir(), "no-nvmet"))

	_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(t.TempDir(), mountCap("ext4")))
	requireGRPCCode(t, err, codes.Internal)
	requireLocalClaimReleased(t, env)
	if len(env.mounter.formatAndMountCalls) != 0 {
		t.Errorf("FormatAndMount called without a verified export state: %v", env.mounter.formatAndMountCalls)
	}
}

// TestNodeStageVolume_Local_MissingTargetIDRejected verifies that a local
// stage without the subsystem NQN is rejected before any claim is made.
func TestNodeStageVolume_Local_MissingTargetIDRejected(t *testing.T) {
	env := newLocalTestEnv(t)
	req := localStageRequest(t.TempDir(), mountCap("ext4"))
	req.VolumeContext = map[string]string{}

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.InvalidArgument)
	if len(env.dm.ensureCalls) != 0 {
		t.Errorf("EnsureLinear calls = %v, want none", env.dm.ensureCalls)
	}
}

// rollbackMounter wraps the mock mounter for local-stage rollback tests: it
// runs afterMount once a FormatAndMount or bind Mount succeeded and logs each
// Unmount into events, which rollbackDeviceMapper shares, so the order of
// unmount and claim removal is observable.
type rollbackMounter struct {
	*mockMounter
	afterMount func()
	events     *[]string
}

func (m *rollbackMounter) FormatAndMount(
	ctx context.Context, source, target, fsType string, options, mkfsOptions []string,
) error {
	err := m.mockMounter.FormatAndMount(ctx, source, target, fsType, options, mkfsOptions)
	if err == nil {
		m.afterMount()
	}
	return err
}

func (m *rollbackMounter) Mount(source, target, fsType string, options []string) error {
	err := m.mockMounter.Mount(source, target, fsType, options)
	if err == nil {
		m.afterMount()
	}
	return err
}

func (m *rollbackMounter) Unmount(target string) error {
	*m.events = append(*m.events, "unmount "+target)
	return m.mockMounter.Unmount(target)
}

// rollbackDeviceMapper logs each claim removal into the events it shares
// with rollbackMounter.
type rollbackDeviceMapper struct {
	*fakeDeviceMapper
	events *[]string
}

func (d *rollbackDeviceMapper) Remove(ctx context.Context, name string) error {
	*d.events = append(*d.events, "remove "+name)
	return d.fakeDeviceMapper.Remove(ctx, name)
}

// failStateWriteAfterMount makes the stage state write of env fail once the
// staged surface is mounted: an empty directory at the state file path makes
// the final rename fail, while a rollback can still delete it.  It returns
// the shared log of unmounts and claim removals.
func failStateWriteAfterMount(t *testing.T, env *localTestEnv) *[]string {
	t.Helper()
	events := &[]string{}
	stateFile := env.srv.stateFilePath(localTestVolumeID)
	env.srv.mounter = &rollbackMounter{mockMounter: env.mounter, events: events, afterMount: func() {
		if err := os.Mkdir(stateFile, 0o700); err != nil {
			t.Errorf("block the state file path: %v", err)
		}
	}}
	env.srv.WithDeviceMapper(&rollbackDeviceMapper{fakeDeviceMapper: env.dm, events: events})
	return events
}

// TestNodeStageVolume_Local_MountFailureReleasesClaim verifies that a
// failed format-and-mount or bind mount after the claim was created removes
// the claim so none exists without a stage state file.
func TestNodeStageVolume_Local_MountFailureReleasesClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		volCap *csi.VolumeCapability
		inject func(env *localTestEnv)
	}{
		{name: "format-and-mount", volCap: mountCap("ext4"), inject: func(env *localTestEnv) {
			env.mounter.formatAndMountErr = errors.New("mkfs.ext4: exit status 1")
		}},
		{name: "bind mount", volCap: blockCap(), inject: func(env *localTestEnv) {
			env.mounter.mountErr = errors.New("mount: exit status 32")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newLocalTestEnv(t)
			tc.inject(env)

			_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(t.TempDir(), tc.volCap))
			requireGRPCCode(t, err, codes.Internal)
			requireLocalClaimReleased(t, env)
		})
	}
}

// TestNodeStageVolume_Local_StateWriteFailureRollsBackMount verifies that a
// local stage whose mount or bind succeeded but whose state write failed
// unmounts the staged surface before it removes the claim — removing the
// claim under a live bind would leave the bind on a dead dm device — and
// leaves neither the sentinel nor a stage state entry behind.
func TestNodeStageVolume_Local_StateWriteFailureRollsBackMount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		volCap *csi.VolumeCapability
	}{
		{name: "filesystem", volCap: mountCap("ext4")},
		{name: "block", volCap: blockCap()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newLocalTestEnv(t)
			events := failStateWriteAfterMount(t, env)
			stagingPath := t.TempDir()
			surface := stageBindTarget(stagingPath, tc.volCap)
			isBlock := tc.volCap.GetBlock() != nil
			if isBlock {
				if err := os.WriteFile(surface, nil, 0o600); err != nil {
					t.Fatalf("create block sentinel: %v", err)
				}
			}

			_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, tc.volCap))
			requireGRPCCode(t, err, codes.Internal)
			if !strings.Contains(err.Error(), "persist stage state") {
				t.Errorf("error = %v, want the state write failure", err)
			}

			requireSurfaceRolledBack(t, env, *events, surface, isBlock)
		})
	}
}

// requireSurfaceRolledBack fails t unless the failed local stage unmounted
// surface before it removed the claim, released the claim, left no stage
// state and, for block, removed the sentinel.
func requireSurfaceRolledBack(t *testing.T, env *localTestEnv, events []string, surface string, isBlock bool) {
	t.Helper()
	want := []string{"unmount " + surface, "remove " + LocalDMName(localTestVolumeID)}
	if !slices.Equal(events, want) {
		t.Errorf("rollback = %v, want %v: the surface must be unmounted before the claim is removed", events, want)
	}
	if env.mounter.mountedPaths[surface] {
		t.Errorf("%q still mounted after the failed stage", surface)
	}
	requireLocalClaimReleased(t, env)
	if isBlock {
		if _, statErr := os.Stat(surface); !os.IsNotExist(statErr) {
			t.Errorf("block sentinel %q after rollback: stat err = %v, want not-exist", surface, statErr)
		}
	}
}

// TestNodeStageVolume_Local_RollbackUnmountFailureKeepsClaim verifies that a
// rollback whose unmount fails keeps the claim — it is safe under the live
// mount, while removing it would strand the mount on a dead device — and
// reports the unmount failure joined to the stage failure.
func TestNodeStageVolume_Local_RollbackUnmountFailureKeepsClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		volCap *csi.VolumeCapability
	}{
		{name: "filesystem", volCap: mountCap("ext4")},
		{name: "block", volCap: blockCap()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newLocalTestEnv(t)
			failStateWriteAfterMount(t, env)
			env.mounter.unmountErr = errors.New("umount: target is busy")
			stagingPath := t.TempDir()
			surface := stageBindTarget(stagingPath, tc.volCap)

			_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, tc.volCap))
			requireGRPCCode(t, err, codes.Internal)
			if !strings.Contains(err.Error(), "persist stage state") || !errors.Is(err, env.mounter.unmountErr) {
				t.Errorf("error = %v, want the state write failure joined with the unmount failure", err)
			}
			if len(env.dm.removeCalls) != 0 {
				t.Errorf("Remove calls = %v, want none while %q is still mounted", env.dm.removeCalls, surface)
			}
			if !env.mounter.mountedPaths[surface] {
				t.Errorf("%q not mounted; the failed unmount must leave it in place", surface)
			}
		})
	}
}

// TestNodeStageVolume_Local_ReleaseFailureJoined verifies that a failed
// claim removal after a refused stage is reported together with the
// refusal, keeping the refusal's gRPC code.
func TestNodeStageVolume_Local_ReleaseFailureJoined(t *testing.T) {
	env := newLocalTestEnv(t)
	env.setNamespaceEnable(t, "1", "1")
	env.dm.removeErr = errors.New("dmsetup remove: Device or resource busy")

	_, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(t.TempDir(), mountCap("ext4")))
	requireGRPCCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "still serving remote initiators") ||
		!strings.Contains(err.Error(), "Device or resource busy") {
		t.Errorf("error = %v, want both the refusal and the removal failure", err)
	}
	if !errors.Is(err, env.dm.removeErr) {
		t.Errorf("error %v does not wrap the removal failure", err)
	}
}

func TestNodeStageVolume_Local_UnknownAttachModeRejected(t *testing.T) {
	env := newLocalTestEnv(t)
	req := localStageRequest(t.TempDir(), mountCap("ext4"))
	req.PublishContext[PublishContextKeyAttachMode] = "loopback"

	_, err := env.srv.NodeStageVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.InvalidArgument)
	if len(env.dm.ensureCalls) != 0 {
		t.Errorf("EnsureLinear called for an unknown attach mode: %v", env.dm.ensureCalls)
	}
	requireNoProtocolCalls(t, env.connector)
}

// TestNodeStageVolume_Local_RestageAfterUnmount verifies that re-staging a
// local volume whose mount disappeared (state file kept) re-verifies the
// same device-mapper target over the same backend and mounts it again.
func TestNodeStageVolume_Local_RestageAfterUnmount(t *testing.T) {
	env := newLocalTestEnv(t)
	stagingPath := t.TempDir()
	req := localStageRequest(stagingPath, mountCap("ext4"))

	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodeStageVolume: %v", err)
	}
	// Idempotent while still mounted: nothing is touched again.
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("idempotent NodeStageVolume: %v", err)
	}
	if len(env.dm.ensureCalls) != 1 {
		t.Fatalf("EnsureLinear calls while staged = %d, want 1", len(env.dm.ensureCalls))
	}

	delete(env.mounter.mountedPaths, stagingPath) // e.g. node reboot
	if _, err := env.srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("re-stage NodeStageVolume: %v", err)
	}
	if len(env.dm.ensureCalls) != 2 || env.dm.ensureCalls[1] != env.dm.ensureCalls[0] {
		t.Errorf("EnsureLinear calls = %v, want the same target twice", env.dm.ensureCalls)
	}
	if len(env.mounter.formatAndMountCalls) != 2 {
		t.Errorf("FormatAndMount calls = %d, want 2", len(env.mounter.formatAndMountCalls))
	}
}

// TestNodeStageVolume_ProtocolPathIgnoresDeviceMapper verifies that a publish
// without attach-mode stages through the protocol handler exactly as before.
func TestNodeStageVolume_ProtocolPathIgnoresDeviceMapper(t *testing.T) {
	env := newLocalTestEnv(t)
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-protocol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.2026-01.io.example:vol", testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if len(env.dm.ensureCalls) != 0 {
		t.Errorf("protocol stage used the device mapper: %v", env.dm.ensureCalls)
	}
	if len(env.connector.connectCalls) != 1 {
		t.Errorf("Connect calls = %d, want 1", len(env.connector.connectCalls))
	}
	state, stateErr := env.srv.readStageState("vol-protocol")
	if stateErr != nil {
		t.Fatalf("readStageState: %v", stateErr)
	}
	if state == nil || state.isLocalAttach() || state.Local != nil || state.NVMeoF == nil {
		t.Errorf("protocol stage state = %+v, want an NVMe-oF state without local fields", state)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeUnstageVolume
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeUnstageVolume_Local_RemovesDeviceMapper(t *testing.T) {
	env := newLocalTestEnv(t)
	stagingPath := t.TempDir()
	_, stageErr := env.srv.NodeStageVolume(context.Background(),
		localStageRequest(stagingPath, mountCap("ext4")))
	if stageErr != nil {
		t.Fatalf("NodeStageVolume: %v", stageErr)
	}

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: localTestVolumeID, StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	if len(env.mounter.unmountCalls) != 1 || env.mounter.unmountCalls[0] != stagingPath {
		t.Errorf("Unmount calls = %v, want [%s]", env.mounter.unmountCalls, stagingPath)
	}
	if len(env.dm.removeCalls) != 1 || env.dm.removeCalls[0] != LocalDMName(localTestVolumeID) {
		t.Errorf("Remove calls = %v, want [%s]", env.dm.removeCalls, LocalDMName(localTestVolumeID))
	}
	requireNoProtocolCalls(t, env.connector)
	state, stateErr := env.srv.readStageState(localTestVolumeID)
	if stateErr != nil {
		t.Fatalf("readStageState: %v", stateErr)
	}
	if state != nil {
		t.Errorf("stage state still present after unstage: %+v", state)
	}
}

func TestNodeUnstageVolume_Local_RemoveFailureKeepsState(t *testing.T) {
	env := newLocalTestEnv(t)
	stagingPath := t.TempDir()
	if _, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, blockCap())); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	env.dm.removeErr = errors.New("dmsetup remove: Device or resource busy")

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: localTestVolumeID, StagingTargetPath: stagingPath,
	})
	requireGRPCCode(t, err, codes.Internal)

	state, readErr := env.srv.readStageState(localTestVolumeID)
	if readErr != nil || state == nil || !state.isLocalAttach() {
		t.Errorf("stage state after failed remove = %+v, %v; want the local state kept for the retry", state, readErr)
	}
	requireNoProtocolCalls(t, env.connector)
}

// TestNodeUnstageVolume_NoState_RemovesOrphanLocalClaim verifies that an
// unstage without stage state removes a device-mapper target a failed local
// stage left behind under the volume's deterministic name.
func TestNodeUnstageVolume_NoState_RemovesOrphanLocalClaim(t *testing.T) {
	env := newLocalTestEnv(t)
	want := LocalDMName(localTestVolumeID)
	var probed []string
	env.srv.dmTargetPresentFn = func(name string) (bool, error) {
		probed = append(probed, name)
		return true, nil
	}

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: localTestVolumeID, StagingTargetPath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	if len(probed) != 1 || probed[0] != want {
		t.Errorf("probed targets = %v, want [%s]", probed, want)
	}
	if len(env.dm.removeCalls) != 1 || env.dm.removeCalls[0] != want {
		t.Errorf("Remove calls = %v, want [%s]", env.dm.removeCalls, want)
	}
	requireNoProtocolCalls(t, env.connector)
}

// TestNodeUnstageVolume_NoState_OrphanRemoveFailureIsInternal verifies that
// a failed orphan removal is reported instead of a false success.
func TestNodeUnstageVolume_NoState_OrphanRemoveFailureIsInternal(t *testing.T) {
	env := newLocalTestEnv(t)
	env.srv.dmTargetPresentFn = func(string) (bool, error) { return true, nil }
	env.dm.removeErr = errors.New("dmsetup remove: Device or resource busy")

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: localTestVolumeID, StagingTargetPath: t.TempDir(),
	})
	requireGRPCCode(t, err, codes.Internal)
}

// TestNodeUnstageVolume_NoState_NoOrphanSucceeds verifies that an unstage
// without stage state and without an orphan target succeeds without
// removing anything.
func TestNodeUnstageVolume_NoState_NoOrphanSucceeds(t *testing.T) {
	env := newLocalTestEnv(t)
	env.srv.dmTargetPresentFn = func(string) (bool, error) { return false, nil }

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: localTestVolumeID, StagingTargetPath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	if len(env.dm.removeCalls) != 0 {
		t.Errorf("Remove calls = %v, want none", env.dm.removeCalls)
	}
}

// TestNodeUnstageVolume_NoState_MountedKeepsOrphan verifies that the orphan
// cleanup runs only after the mount-surface guard: a still-mounted staging
// path fails the call and leaves the device-mapper target alone.
func TestNodeUnstageVolume_NoState_MountedKeepsOrphan(t *testing.T) {
	env := newLocalTestEnv(t)
	env.srv.dmTargetPresentFn = func(string) (bool, error) { return true, nil }
	stagingPath := t.TempDir()
	env.mounter.mountedPaths[stagingPath] = true

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: localTestVolumeID, StagingTargetPath: stagingPath,
	})
	requireGRPCCode(t, err, codes.Internal)
	if len(env.dm.removeCalls) != 0 {
		t.Errorf("Remove calls = %v, want none while mounted", env.dm.removeCalls)
	}
}

// TestNodeUnstageVolume_Local_AfterNodeRestart verifies that a restarted node
// plugin (fresh NodeServer, same state directory and host mount table) tears
// down a local attach from the persisted state alone.
func TestNodeUnstageVolume_Local_AfterNodeRestart(t *testing.T) {
	for _, mode := range restartTestModes() {
		t.Run(mode.name, func(t *testing.T) {
			env := newLocalTestEnv(t)
			stagingPath := t.TempDir()
			if _, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, mode.volCap)); err != nil {
				t.Fatalf("NodeStageVolume: %v", err)
			}

			conn := &mockConnector{devicePath: "/dev/nvme0n1"}
			dm := &fakeDeviceMapper{}
			env.mounter.unmountCalls = nil
			restarted := NewNodeServerWithStateDir("test-node", conn, env.mounter, env.stateDir).WithDeviceMapper(dm)

			_, err := restarted.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
				VolumeId: localTestVolumeID, StagingTargetPath: stagingPath,
			})
			if err != nil {
				t.Fatalf("NodeUnstageVolume after restart: %v", err)
			}
			if len(env.mounter.unmountCalls) != 1 || env.mounter.unmountCalls[0] != mode.target(stagingPath) {
				t.Errorf("Unmount calls = %v, want [%s]", env.mounter.unmountCalls, mode.target(stagingPath))
			}
			if len(dm.removeCalls) != 1 || dm.removeCalls[0] != LocalDMName(localTestVolumeID) {
				t.Errorf("Remove calls = %v, want [%s]", dm.removeCalls, LocalDMName(localTestVolumeID))
			}
			requireNoProtocolCalls(t, conn)
		})
	}
}

// TestNodeUnstageVolume_StateWithoutAttachModeIsProtocol verifies that a
// stage state file written before local attach existed decodes as a
// protocol attach: unstage detaches through the handler and never touches
// the device mapper.
func TestNodeUnstageVolume_StateWithoutAttachModeIsProtocol(t *testing.T) {
	env := newLocalTestEnv(t)
	stagingPath := t.TempDir()
	const nqn = "nqn.2026-01.io.example:legacy"
	legacy := `{"protocol_type":"nvmeof-tcp","access_type":"filesystem","fs_type":"ext4",` +
		`"nvmeof":{"subsys_nqn":"` + nqn + `","address":"10.0.0.1","port":"4420"}}`
	if err := os.WriteFile(env.srv.stateFilePath("vol-legacy"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}

	state, err := env.srv.readStageState("vol-legacy")
	if err != nil || state == nil {
		t.Fatalf("readStageState = %+v, %v", state, err)
	}
	if state.isLocalAttach() || state.Local != nil {
		t.Errorf("legacy state decoded as local attach: %+v", state)
	}

	env.mounter.mountedPaths[stagingPath] = true
	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: "vol-legacy", StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	if len(env.connector.disconnectCalls) != 1 || env.connector.disconnectCalls[0] != nqn {
		t.Errorf("Disconnect calls = %v, want [%s]", env.connector.disconnectCalls, nqn)
	}
	if len(env.dm.removeCalls) != 0 {
		t.Errorf("protocol unstage used the device mapper: %v", env.dm.removeCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeExpandVolume
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeExpandVolume_Local_ReloadsThenResizes(t *testing.T) {
	env := newLocalTestEnv(t)
	resizer := &mockResizer{}
	env.srv.WithResizer(resizer)
	stagingPath := t.TempDir()
	_, stageErr := env.srv.NodeStageVolume(context.Background(),
		localStageRequest(stagingPath, mountCap("xfs")))
	if stageErr != nil {
		t.Fatalf("NodeStageVolume: %v", stageErr)
	}

	resp, err := env.srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:         localTestVolumeID,
		VolumePath:       stagingPath,
		VolumeCapability: mountCap("xfs"),
		CapacityRange:    &csi.CapacityRange{RequiredBytes: 2 << 30},
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}

	want := dmCall{LocalDMName(localTestVolumeID), localTestBacking}
	if len(env.dm.reloadCalls) != 1 || env.dm.reloadCalls[0] != want {
		t.Errorf("ReloadLinear calls = %v, want [%v]", env.dm.reloadCalls, want)
	}
	if resizer.called != 1 || resizer.capturedMount != stagingPath || resizer.capturedFsType != "xfs" {
		t.Errorf("ResizeFS called %d times with (%q, %q), want once with (%q, xfs)",
			resizer.called, resizer.capturedMount, resizer.capturedFsType, stagingPath)
	}
	if resp.GetCapacityBytes() != 2<<30 {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), int64(2<<30))
	}
	requireNoProtocolCalls(t, env.connector)
}

func TestNodeExpandVolume_Local_BlockReloadsOnly(t *testing.T) {
	env := newLocalTestEnv(t)
	resizer := &mockResizer{}
	env.srv.WithResizer(resizer)
	stagingPath := t.TempDir()
	if _, err := env.srv.NodeStageVolume(context.Background(), localStageRequest(stagingPath, blockCap())); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	devicePath := blockStagingDevicePath(stagingPath)
	if err := writeEmptyFile(t, devicePath); err != nil {
		t.Fatalf("create block publish sentinel: %v", err)
	}

	// The CO may omit the capability; the regular-file path identifies Block.
	_, err := env.srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   localTestVolumeID,
		VolumePath: devicePath,
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if len(env.dm.reloadCalls) != 1 {
		t.Errorf("ReloadLinear calls = %d, want 1", len(env.dm.reloadCalls))
	}
	if resizer.called != 0 {
		t.Errorf("ResizeFS called %d times for a block volume", resizer.called)
	}
}

func TestNodeExpandVolume_Local_ReloadFailureSkipsResize(t *testing.T) {
	env := newLocalTestEnv(t)
	resizer := &mockResizer{}
	env.srv.WithResizer(resizer)
	stagingPath := t.TempDir()
	_, stageErr := env.srv.NodeStageVolume(context.Background(),
		localStageRequest(stagingPath, mountCap("ext4")))
	if stageErr != nil {
		t.Fatalf("NodeStageVolume: %v", stageErr)
	}
	env.dm.reloadErr = errors.New("dmsetup reload: Invalid argument")

	_, err := env.srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: localTestVolumeID, VolumePath: stagingPath, VolumeCapability: mountCap("ext4"),
	})
	requireGRPCCode(t, err, codes.Internal)
	if resizer.called != 0 {
		t.Errorf("ResizeFS ran although the device-mapper reload failed")
	}
}

func TestNodeExpandVolume_ProtocolDoesNotReload(t *testing.T) {
	env := newLocalTestEnv(t)
	resizer := &mockResizer{}
	env.srv.WithResizer(resizer)
	stagingPath := t.TempDir()
	if _, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-protocol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.2026-01.io.example:vol", testStorageAddr),
	}); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	_, err := env.srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: "vol-protocol", VolumePath: stagingPath, VolumeCapability: mountCap("ext4"),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if len(env.dm.reloadCalls) != 0 {
		t.Errorf("protocol expand reloaded a device-mapper target: %v", env.dm.reloadCalls)
	}
	if resizer.called != 1 {
		t.Errorf("ResizeFS calls = %d, want 1", resizer.called)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// LocalDMName and dev_t helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestLocalDMName(t *testing.T) {
	name := LocalDMName(localTestVolumeID)
	if !regexp.MustCompile(`^pillar-local-[0-9a-f]{16}$`).MatchString(name) {
		t.Errorf("LocalDMName = %q, want pillar-local-<16 hex>", name)
	}
	if LocalDMName(localTestVolumeID) != name {
		t.Error("LocalDMName is not deterministic")
	}
	if LocalDMName(localTestVolumeID+"x") == name {
		t.Error("different volume IDs map to the same device-mapper name")
	}
}

func TestLinuxDevMajorMinorRoundTrip(t *testing.T) {
	for _, tc := range []struct{ major, minor uint32 }{
		{8, 16}, {253, 0}, {230, 1024}, {259, 1 << 19}, {4095, 255}, {1 << 20, 3},
	} {
		major, minor := linuxDevMajorMinor(linuxMkdev(tc.major, tc.minor))
		if major != tc.major || minor != tc.minor {
			t.Errorf("round trip %d:%d = %d:%d", tc.major, tc.minor, major, minor)
		}
	}
	// A well-known encoding: 8:16 (sdb) is 0x810.
	if got := linuxMkdev(8, 16); got != 0x810 {
		t.Errorf("linuxMkdev(8, 16) = %#x, want 0x810", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// execDeviceMapper (scripted dmsetup)
// ─────────────────────────────────────────────────────────────────────────────

type dmsetupReply struct {
	out string
	err error
}

// scriptedDmsetup replays programmed replies per dmsetup subcommand and
// records every invocation.
type scriptedDmsetup struct {
	replies map[string][]dmsetupReply
	calls   [][]string
}

func (s *scriptedDmsetup) run(_ context.Context, args ...string) ([]byte, error) {
	s.calls = append(s.calls, args)
	queue := s.replies[args[0]]
	if len(queue) == 0 {
		return nil, fmt.Errorf("unexpected dmsetup %v", args)
	}
	s.replies[args[0]] = queue[1:]
	return []byte(queue[0].out), queue[0].err
}

func (s *scriptedDmsetup) subcommands() []string {
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, c[0])
	}
	return out
}

var errExit1 = errors.New("exit status 1")

var dmAbsent = dmsetupReply{out: "Device does not exist.\nCommand failed.\n", err: errExit1}

func newScriptedDM(s *scriptedDmsetup, info backingDeviceInfo) (dm *execDeviceMapper, nodes *[]string) {
	var ensured []string
	dm = &execDeviceMapper{
		run:   s.run,
		probe: func(string) (backingDeviceInfo, error) { return info, nil },
		ensureNode: func(name string) (string, error) {
			ensured = append(ensured, name)
			return "/dev/mapper/" + name, nil
		},
	}
	return dm, &ensured
}

var testBackingInfo = backingDeviceInfo{major: 230, minor: 16, sectors: 2048}

func TestExecDeviceMapper_EnsureCreatesWhenAbsent(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{
		"table":  {dmAbsent, {out: "0 2048 linear 230:16 0\n"}},
		"create": {{}},
	}}
	dm, nodes := newScriptedDM(s, testBackingInfo)

	path, err := dm.EnsureLinear(context.Background(), "pillar-local-x", localTestBacking)
	if err != nil {
		t.Fatalf("EnsureLinear: %v", err)
	}
	if path != "/dev/mapper/pillar-local-x" || len(*nodes) != 1 {
		t.Errorf("EnsureLinear = %q (nodes %v), want the ensured /dev/mapper node", path, *nodes)
	}
	want := []string{"create", "pillar-local-x", "--table", "0 2048 linear 230:16 0"}
	if strings.Join(s.calls[1], " ") != strings.Join(want, " ") {
		t.Errorf("create args = %v, want %v", s.calls[1], want)
	}
}

func TestExecDeviceMapper_EnsureReusesMatchingTarget(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{
		"table": {{out: "0 2048 linear 230:16 0\n"}},
	}}
	dm, nodes := newScriptedDM(s, testBackingInfo)

	if _, err := dm.EnsureLinear(context.Background(), "pillar-local-x", localTestBacking); err != nil {
		t.Fatalf("EnsureLinear: %v", err)
	}
	if got := s.subcommands(); len(got) != 1 || got[0] != "table" {
		t.Errorf("dmsetup calls = %v, want only the table lookup", got)
	}
	if len(*nodes) != 1 {
		t.Errorf("device node not ensured for a reused target")
	}
}

func TestExecDeviceMapper_EnsureRejectsForeignTarget(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{
		"table": {{out: "0 2048 linear 8:32 0\n"}},
	}}
	dm, nodes := newScriptedDM(s, testBackingInfo)

	_, err := dm.EnsureLinear(context.Background(), "pillar-local-x", localTestBacking)
	if err == nil || !strings.Contains(err.Error(), "230:16") {
		t.Fatalf("EnsureLinear over a foreign target = %v, want a mismatch error", err)
	}
	if len(s.calls) != 1 || len(*nodes) != 0 {
		t.Errorf("mismatching target was modified: calls %v nodes %v", s.calls, *nodes)
	}
}

func TestExecDeviceMapper_EnsureCreateFailureReturned(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{
		"table":  {dmAbsent},
		"create": {{out: "device-mapper: create ioctl failed: Device or resource busy\n", err: errExit1}},
	}}
	dm, nodes := newScriptedDM(s, testBackingInfo)

	_, err := dm.EnsureLinear(context.Background(), "pillar-local-x", localTestBacking)
	if err == nil || !strings.Contains(err.Error(), "resource busy") {
		t.Fatalf("EnsureLinear = %v, want the dmsetup create failure", err)
	}
	if len(*nodes) != 0 {
		t.Errorf("device node ensured after a failed create")
	}
}

func TestExecDeviceMapper_RemoveAbsentIsSuccess(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{"table": {dmAbsent}}}
	dm, _ := newScriptedDM(s, testBackingInfo)

	if err := dm.Remove(context.Background(), "pillar-local-x"); err != nil {
		t.Fatalf("Remove of an absent target: %v", err)
	}
	if got := s.subcommands(); len(got) != 1 {
		t.Errorf("dmsetup calls = %v, want only the table lookup", got)
	}
}

func TestExecDeviceMapper_RemoveFailureReturned(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{
		"table":  {{out: "0 2048 linear 230:16 0\n"}},
		"remove": {{out: "device-mapper: remove ioctl failed: Device or resource busy\n", err: errExit1}},
	}}
	dm, _ := newScriptedDM(s, testBackingInfo)

	err := dm.Remove(context.Background(), "pillar-local-x")
	if err == nil || !strings.Contains(err.Error(), "resource busy") {
		t.Fatalf("Remove = %v, want the dmsetup remove failure", err)
	}
}

func TestExecDeviceMapper_RemoveVerifiesGone(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{
		"table":  {{out: "0 2048 linear 230:16 0\n"}, {out: "0 2048 linear 230:16 0\n"}},
		"remove": {{}},
	}}
	dm, _ := newScriptedDM(s, testBackingInfo)

	if err := dm.Remove(context.Background(), "pillar-local-x"); err == nil {
		t.Fatal("Remove reported success while the target still exists")
	}
}

func TestExecDeviceMapper_ReloadGrowsToBackingSize(t *testing.T) {
	grown := backingDeviceInfo{major: 230, minor: 16, sectors: 4096}
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{
		"table":  {{out: "0 2048 linear 230:16 0\n"}, {out: "0 4096 linear 230:16 0\n"}},
		"reload": {{}},
		"resume": {{}},
	}}
	dm, _ := newScriptedDM(s, grown)

	if err := dm.ReloadLinear(context.Background(), "pillar-local-x", localTestBacking); err != nil {
		t.Fatalf("ReloadLinear: %v", err)
	}
	want := "table reload resume table"
	if got := strings.Join(s.subcommands(), " "); got != want {
		t.Errorf("dmsetup calls = %q, want %q", got, want)
	}
	if got := strings.Join(s.calls[1], " "); got != "reload pillar-local-x --table 0 4096 linear 230:16 0" {
		t.Errorf("reload args = %q", got)
	}
}

func TestExecDeviceMapper_ReloadMissingTargetFails(t *testing.T) {
	s := &scriptedDmsetup{replies: map[string][]dmsetupReply{"table": {dmAbsent}}}
	dm, _ := newScriptedDM(s, testBackingInfo)

	if err := dm.ReloadLinear(context.Background(), "pillar-local-x", localTestBacking); err == nil {
		t.Fatal("ReloadLinear of an absent target succeeded")
	}
}

// TestDMTargetInSysfs verifies the orphan probe: a target is present only
// when some dm-N/dm/name matches, and a missing block root (no sysfs) means
// no target can exist.
func TestDMTargetInSysfs(t *testing.T) {
	root := t.TempDir()
	for dev, name := range map[string]string{"dm-0": "other", "dm-3": "pillar-local-abc"} {
		dir := filepath.Join(root, dev, "dm")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "name"), []byte(name+"\n"), 0o600); err != nil {
			t.Fatalf("write name: %v", err)
		}
	}

	for _, tc := range []struct {
		root, name string
		want       bool
	}{
		{root, "pillar-local-abc", true},
		{root, "pillar-local-def", false},
		{filepath.Join(root, "absent"), "pillar-local-abc", false},
	} {
		got, err := dmTargetInSysfs(tc.root, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("dmTargetInSysfs(%s, %s) = %v, %v; want %v", tc.root, tc.name, got, err, tc.want)
		}
	}
}
