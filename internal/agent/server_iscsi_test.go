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

package agent_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/lio"
	"github.com/isac322/pillar-csi/internal/agent/lio/liotest"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

const (
	testVolumeIQN    = "iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"
	testInitiatorIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
)
const (
	testMixedZVOLDevicePath    = "/dev/zvol/tank/pvc-abc"
	testMixedDatasetDevicePath = "/var/lib/pillar-csi/datasets/tank/pvc-abc"
)

type typedMockBackend struct {
	*mockBackend
	backendType agentv1.BackendType
}

func (m *typedMockBackend) Type() agentv1.BackendType {
	return m.backendType
}

func newMixedISCSIServer(t *testing.T) iscsiEnv {
	t.Helper()
	return newMixedISCSIServerWithPaths(t, testMixedZVOLDevicePath, testMixedDatasetDevicePath)
}

func newMixedISCSIServerWithPaths(t *testing.T, zvolPath, datasetPath string) iscsiEnv {
	t.Helper()
	return newMixedISCSIServerWithPathsAndType(
		t,
		agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		zvolPath,
		datasetPath,
	)
}

func newMixedISCSIServerWithPathsAndType(
	t *testing.T,
	zvolType agentv1.BackendType,
	zvolPath, datasetPath string,
) iscsiEnv {
	t.Helper()
	root := t.TempDir()
	k, err := liotest.New(root)
	if err != nil {
		t.Fatalf("liotest.New: %v", err)
	}
	zvol := &typedMockBackend{
		mockBackend: &mockBackend{devicePathResult: zvolPath},
		backendType: zvolType,
	}
	dataset := &typedMockBackend{
		mockBackend: &mockBackend{devicePathResult: datasetPath},
		backendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
	}
	// Make the dataset the pool's default backend deliberately.  iSCSI local
	// attach must still select the zvol variant by type.
	backends := map[string]backend.VolumeBackend{testPool: dataset}
	variants := map[string]map[agentv1.BackendType]backend.VolumeBackend{
		testPool: {
			agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET: dataset,
			agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL:    zvol,
		},
	}
	opts := []agent.ServerOption{
		agent.WithDrainStateDir(t.TempDir()),
		agent.WithLIOFS(k),
		agent.WithDeviceClaimer(k.Claimer()),
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
		agent.WithBackendVariants(variants),
	}
	return iscsiEnv{srv: agent.NewServer(backends, root, opts...), k: k, root: root}
}

type iscsiEnv struct {
	srv  *agent.Server
	k    *liotest.Kernel
	root string
}

func (e iscsiEnv) tpg() string {
	return filepath.Join(e.root, "target", "iscsi", testVolumeIQN, "tpgt_1")
}

func (e iscsiEnv) lun0() string { return filepath.Join(e.tpg(), "lun", "lun_0") }

func newISCSIServer(t *testing.T, opts ...agent.ServerOption) iscsiEnv {
	t.Helper()
	root := t.TempDir()
	k, err := liotest.New(root)
	if err != nil {
		t.Fatalf("liotest.New: %v", err)
	}
	backends := map[string]backend.VolumeBackend{testPool: &mockBackend{devicePathResult: testDevicePath}}
	opts = append([]agent.ServerOption{
		agent.WithDrainStateDir(t.TempDir()),
		agent.WithLIOFS(k),
		agent.WithDeviceClaimer(k.Claimer()),
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	}, opts...)
	return iscsiEnv{srv: agent.NewServer(backends, root, opts...), k: k, root: root}
}

func iscsiExportParams(addr string, port int32) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_Iscsi{Iscsi: &agentv1.IscsiExportParams{BindAddress: addr, Port: port}},
	}
}

func exportISCSI(t *testing.T, srv *agent.Server, acl bool) *agentv1.ExportInfo {
	t.Helper()
	resp, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		DevicePath:   testDevicePath,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		ExportParams: iscsiExportParams("10.0.0.1", 0),
		AclEnabled:   acl,
	})
	if err != nil {
		t.Fatalf("ExportVolume(iscsi): %v", err)
	}
	return resp.GetExportInfo()
}

func iscsiSetLocalAttach(t *testing.T, srv *agent.Server, local bool) (*agentv1.SetLocalAttachResponse, error) {
	t.Helper()
	return srv.SetLocalAttach(context.Background(), &agentv1.SetLocalAttachRequest{
		VolumeId:     testVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		Local:        local,
		Fence:        testFence(t),
	})
}

func TestExportVolume_ISCSI(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)

	info := exportISCSI(t, env.srv, false)
	if info.GetTargetId() != testVolumeIQN || info.GetAddress() != "10.0.0.1" ||
		info.GetPort() != lio.DefaultPort || info.GetVolumeRef() != "0" {
		t.Fatalf("ExportInfo = %v, want %s at 10.0.0.1:3260 LUN 0", info, testVolumeIQN)
	}
	if !env.k.Exists(filepath.Join(env.lun0(), "backstore")) ||
		!env.k.Exists(filepath.Join(env.tpg(), "np", "0.0.0.0:3260")) {
		t.Fatal("export did not create LUN 0 and portal")
	}
	// Re-export is idempotent.
	if again := exportISCSI(t, env.srv, false); again.GetTargetId() != testVolumeIQN {
		t.Fatalf("re-export ExportInfo = %v", again)
	}
}

func TestExportVolume_ISCSIInvalidParams(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	for name, params := range map[string]*agentv1.ExportParams{
		"missing":      nil,
		"bad address":  iscsiExportParams("node-1", 3260),
		"port too big": iscsiExportParams("10.0.0.1", 70000),
	} {
		_, err := env.srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
			VolumeId:     testVolumeID,
			Fence:        testFence(t),
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
			ExportParams: params,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v (%v), want InvalidArgument", name, status.Code(err), err)
		}
	}
}

func TestISCSI_AllowDenyUnexport(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	exportISCSI(t, env.srv, true)
	aclDir := filepath.Join(env.tpg(), "acls", testInitiatorIQN)

	_, err := env.srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId: testVolumeID, Fence: testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, InitiatorId: testInitiatorIQN,
	})
	if err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}
	if !env.k.Exists(filepath.Join(aclDir, "lun_0", "lun")) {
		t.Fatal("AllowInitiator did not map LUN 0")
	}

	_, err = env.srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId: testVolumeID, Fence: testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, InitiatorId: testInitiatorIQN,
	})
	if err != nil {
		t.Fatalf("DenyInitiator: %v", err)
	}
	if env.k.Exists(aclDir) {
		t.Fatal("DenyInitiator left the node ACL")
	}

	for range 2 {
		_, err = env.srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
			VolumeId: testVolumeID, Fence: testFence(t), ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		})
		if err != nil {
			t.Fatalf("UnexportVolume: %v", err)
		}
	}
	if env.k.Exists(filepath.Join(env.root, "target", "iscsi", testVolumeIQN)) {
		t.Fatal("UnexportVolume left the target")
	}
}

func TestISCSI_AllowInitiatorMissingExport(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	_, err := env.srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId: testVolumeID, Fence: testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, InitiatorId: testInitiatorIQN,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("AllowInitiator without export: code = %v (%v), want NotFound", status.Code(err), err)
	}
}

// SetLocalAttach(true) returns the device and leaves no lun_0 (the node's
// local-attach fence); SetLocalAttach(false) is refused while the node holds
// the device and restores LUN 0 once it is released.
func TestISCSI_SetLocalAttachToggle(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	exportISCSI(t, env.srv, false)

	for range 2 {
		resp, err := iscsiSetLocalAttach(t, env.srv, true)
		if err != nil || resp.GetDevicePath() != testDevicePath {
			t.Fatalf("SetLocalAttach(true) = %v, %v; want %s", resp, err, testDevicePath)
		}
		if env.k.Exists(env.lun0()) {
			t.Fatal("lun_0 present after SetLocalAttach(true)")
		}
	}

	env.k.HoldDevice(testDevicePath)
	_, err := iscsiSetLocalAttach(t, env.srv, false)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SetLocalAttach(false) while held: code = %v (%v), want FailedPrecondition", status.Code(err), err)
	}
	if env.k.Exists(env.lun0()) {
		t.Fatal("refused SetLocalAttach(false) created lun_0")
	}

	env.k.ReleaseDevice(testDevicePath)
	for range 2 {
		resp, err := iscsiSetLocalAttach(t, env.srv, false)
		if err != nil || resp.GetDevicePath() != "" {
			t.Fatalf("SetLocalAttach(false) = %v, %v; want empty device path", resp, err)
		}
	}
	if !env.k.Exists(filepath.Join(env.lun0(), "backstore")) {
		t.Fatal("SetLocalAttach(false) did not restore LUN 0")
	}
}

// A mixed ZFS pool may default to its dataset backend, but iSCSI local
// attach always resolves the zvol variant when no backstore records a path.
func TestISCSI_SetLocalAttachMixedPoolSelectsZVOLDevicePath(t *testing.T) {
	t.Parallel()
	env := newMixedISCSIServer(t)
	exportISCSI(t, env.srv, false)

	resp, err := iscsiSetLocalAttach(t, env.srv, true)
	if err != nil || resp.GetDevicePath() != testDevicePath {
		t.Fatalf("initial SetLocalAttach(true) = %v, %v; want %s", resp, err, testDevicePath)
	}

	// The export is already locally attached, so EnterLocalAttach returns no
	// backstore path and the private resolver must select the zvol backend.
	resp, err = iscsiSetLocalAttach(t, env.srv, true)
	if err != nil || resp.GetDevicePath() != testMixedZVOLDevicePath {
		t.Fatalf("retry SetLocalAttach(true) = %v, %v; want %s", resp, err, testMixedZVOLDevicePath)
	}
	if env.k.Exists(env.lun0()) {
		t.Fatal("lun_0 present after repeated SetLocalAttach(true)")
	}

	resp, err = iscsiSetLocalAttach(t, env.srv, false)
	if err != nil || resp.GetDevicePath() != "" {
		t.Fatalf("SetLocalAttach(false) = %v, %v; want empty device path", resp, err)
	}
	resp, err = iscsiSetLocalAttach(t, env.srv, true)
	if err != nil || resp.GetDevicePath() != testMixedZVOLDevicePath {
		t.Fatalf("post-reenable SetLocalAttach(true) = %v, %v; want %s", resp, err, testMixedZVOLDevicePath)
	}
	if resp.GetDevicePath() == testMixedDatasetDevicePath {
		t.Fatal("iSCSI local attach selected the dataset device path")
	}
}

func TestISCSI_SetLocalAttachMixedPoolRejectsWrongTypedZVOL(t *testing.T) {
	t.Parallel()
	env := newMixedISCSIServerWithPathsAndType(
		t,
		agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		testMixedZVOLDevicePath,
		testMixedDatasetDevicePath,
	)
	exportISCSI(t, env.srv, false)

	_, err := iscsiSetLocalAttach(t, env.srv, false)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("SetLocalAttach(false) = %v, want InvalidArgument for wrong typed zvol lookup", err)
	}
	if !env.k.Exists(env.lun0()) {
		t.Fatal("typed backend rejection changed the existing LUN state")
	}
}

func TestISCSI_SetLocalAttachMixedPoolRejectsEmptyZVOLDevicePath(t *testing.T) {
	t.Parallel()
	env := newMixedISCSIServerWithPaths(t, "", testMixedDatasetDevicePath)
	exportISCSI(t, env.srv, false)

	_, err := iscsiSetLocalAttach(t, env.srv, false)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SetLocalAttach(false) = %v, want FailedPrecondition for empty zvol path", err)
	}
	if !env.k.Exists(env.lun0()) {
		t.Fatal("empty zvol path rejection changed the existing LUN state")
	}
}

func TestISCSI_SetLocalAttachMissingExport(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	_, err := iscsiSetLocalAttach(t, env.srv, true)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("SetLocalAttach without export: code = %v (%v), want NotFound", status.Code(err), err)
	}
}

func iscsiDesired(allowed ...string) *agentv1.VolumeDesiredState {
	return &agentv1.VolumeDesiredState{
		VolumeId:    testVolumeID,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		DevicePath:  "/dev/zvol/" + testVolumeID,
		Fence:       &agentv1.FencingToken{VolumeUid: testVolumeID, Generation: 1},
		Exports: []*agentv1.ExportDesiredState{{
			ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
			ExportParams:      iscsiExportParams("10.0.0.1", 3260),
			AclEnabled:        len(allowed) > 0,
			AllowedInitiators: allowed,
		}},
	}
}

func reconcileISCSI(t *testing.T, srv *agent.Server, complete bool, vols ...*agentv1.VolumeDesiredState) {
	t.Helper()
	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: vols, Complete: complete,
	})
	if err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}
	for _, r := range resp.GetResults() {
		if !r.GetSuccess() {
			t.Fatalf("ReconcileState(%s): %s", r.GetVolumeId(), r.GetErrorMessage())
		}
	}
}

// ReconcileState modifies only the targets of the volumes it lists: the
// restoring complete request rebuilds the desired target with its ACL and
// leaves an owned target it does not list, like NVMe-oF leaves unlisted
// subsystems.  A delayed controller snapshot can omit a newly exported
// volume, so absence from the request must never unexport it.
func TestReconcileState_ISCSIKeepsUnlistedTargets(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t, agent.WithExportRestoreGate())
	unlisted := &lio.Target{
		ConfigfsRoot: env.root, FS: env.k, IQN: lio.OwnedIQNPrefix + "tank.pvc-unlisted",
		DevicePath: "/dev/zvol/tank/pvc-unlisted", BindAddress: "10.0.0.1", Port: 3260, DeviceClaimer: env.k.Claimer(),
	}
	if err := unlisted.Apply(); err != nil {
		t.Fatalf("seed unlisted target: %v", err)
	}

	reconcileISCSI(t, env.srv, true, iscsiDesired(testInitiatorIQN))

	if !env.k.Exists(filepath.Join(env.tpg(), "acls", testInitiatorIQN, "lun_0", "lun")) {
		t.Fatal("restore did not build the desired target with its ACL")
	}
	if !env.k.Exists(filepath.Join(env.root, "target", "iscsi", unlisted.IQN, "tpgt_1", "lun", "lun_0")) ||
		!env.k.Exists(unlisted.BackstoreDir()) {
		t.Fatal("complete restore removed an unlisted owned target or its backstore")
	}

	reconcileISCSI(t, env.srv, true, iscsiDesired(testInitiatorIQN))
	if !env.k.Exists(filepath.Join(env.root, "target", "iscsi", unlisted.IQN)) {
		t.Fatal("later complete reconcile removed an unlisted owned target")
	}
}

// Reconcile with ACL makes the node ACLs exactly the allowed initiators and
// keeps a local attach free of LUN 0.
func TestReconcileState_ISCSIACLAndLocalAttach(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	const other = "iqn.2026-01.com.bhyoo.pillar-csi:node.other"
	reconcileISCSI(t, env.srv, false, iscsiDesired(testInitiatorIQN, other))
	reconcileISCSI(t, env.srv, false, iscsiDesired(testInitiatorIQN))
	target := &lio.Target{ConfigfsRoot: env.root, FS: env.k, IQN: testVolumeIQN}
	got, err := target.Initiators()
	if err != nil || !slices.Equal(got, []string{testInitiatorIQN}) {
		t.Fatalf("Initiators = %v, %v; want [%s]", got, err, testInitiatorIQN)
	}

	local := iscsiDesired(testInitiatorIQN)
	local.Exports[0].LocalAttach = true
	reconcileISCSI(t, env.srv, false, local)
	if env.k.Exists(env.lun0()) {
		t.Fatal("local-attach reconcile left lun_0")
	}
}

func TestGetCapabilities_ISCSI(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	resp, err := env.srv.GetCapabilities(context.Background(), &agentv1.GetCapabilitiesRequest{})
	if err != nil || !slices.Contains(resp.GetSupportedProtocols(), agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI) {
		t.Fatalf("GetCapabilities = %v, %v; want iSCSI supported", resp.GetSupportedProtocols(), err)
	}

	unloaded := liotest.NewUnloaded(t.TempDir())
	srv := agent.NewServer(nil, unloaded.Root, agent.WithDrainStateDir(t.TempDir()), agent.WithLIOFS(unloaded))
	resp, err = srv.GetCapabilities(context.Background(), &agentv1.GetCapabilitiesRequest{})
	if err != nil || slices.Contains(resp.GetSupportedProtocols(), agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI) {
		t.Fatalf("GetCapabilities without LIO = %v, %v; want no iSCSI", resp.GetSupportedProtocols(), err)
	}
	health, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	for _, sub := range health.GetSubsystems() {
		if sub.GetName() == "iscsi_target_configfs" && sub.GetHealthy() {
			t.Error("iscsi_target_configfs healthy without LIO")
		}
	}
}

func TestListExports_ISCSI(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	exportISCSI(t, env.srv, false)
	resp, err := env.srv.ListExports(context.Background(), &agentv1.ListExportsRequest{})
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	info := resp.GetExports()[testVolumeID]
	if info.GetTargetId() != testVolumeIQN || info.GetPort() != 3260 || info.GetVolumeRef() != "0" {
		t.Fatalf("ListExports[%s] = %v", testVolumeID, info)
	}
}
