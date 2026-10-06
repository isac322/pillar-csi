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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/zfs"
	"github.com/isac322/pillar-csi/internal/agent/lio"
	"github.com/isac322/pillar-csi/internal/agent/lio/liotest"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// testDevicePath is the block-device path used in ReconcileState tests.
const testDevicePath = "/dev/zvol/tank/pvc-abc"

// nvmeofExportState builds an ExportDesiredState for NVMe-oF TCP on port 4420.
func nvmeofExportState(addr string, hosts ...string) *agentv1.ExportDesiredState {
	return &agentv1.ExportDesiredState{
		ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams:      nvmeofExportParams(addr, 4420),
		AllowedInitiators: hosts,
	}
}

// nvmeofACLExportState builds an ACL-enforced ExportDesiredState on
// 10.0.0.1:4420 whose exact initiator set is hosts.
func nvmeofACLExportState(hosts ...string) *agentv1.ExportDesiredState {
	state := nvmeofExportState("10.0.0.1", hosts...)
	state.AclEnabled = true
	return state
}

// reconcileOne reconciles a single NVMe-oF volume and fails the test unless
// the agent reports success.
func reconcileOne(t *testing.T, srv *agent.Server, devicePath string, export *agentv1.ExportDesiredState) {
	t.Helper()
	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId:    testVolumeID,
			BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
			DevicePath:  devicePath,
			Exports:     []*agentv1.ExportDesiredState{export},
			Fence:       testFence(t),
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 || !resp.GetResults()[0].GetSuccess() {
		t.Fatalf("reconcile failed: %v", resp.GetResults())
	}
}

func readAllowAnyHost(t *testing.T, cfgRoot, nqn string) string {
	t.Helper()
	//nolint:gosec // G304: test reads a file under t.TempDir().
	raw, err := os.ReadFile(filepath.Join(cfgRoot, "nvmet", "subsystems", nqn, "attr_allow_any_host"))
	if err != nil {
		t.Fatalf("read attr_allow_any_host of %s: %v", nqn, err)
	}
	return strings.TrimSpace(string(raw))
}

func allowedHostLinked(cfgRoot, nqn, host string) bool {
	_, err := os.Lstat(filepath.Join(cfgRoot, "nvmet", "subsystems", nqn, "allowed_hosts", host))
	return err == nil
}

// ReconcileState tests.

func TestReconcileState_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 0 {
		t.Errorf("Results len = %d, want 0 for empty volume list", len(resp.GetResults()))
	}
	if resp.GetReconciledAt() == nil {
		t.Error("ReconciledAt timestamp is nil")
	}
}

func TestReconcileState_NvmeofExportCreatesConfigfs(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:    testVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Fence:       testFence(t),
				DevicePath:  testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("192.168.1.10"),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 {
		t.Fatalf("Results len = %d, want 1", len(resp.GetResults()))
	}
	result := resp.GetResults()[0]
	if !result.GetSuccess() {
		t.Errorf("result.Success=false, ErrorMessage=%q", result.GetErrorMessage())
	}
	if result.GetVolumeId() != testVolumeID {
		t.Errorf("result.VolumeId = %q, want %q", result.GetVolumeId(), testVolumeID)
	}

	// The configfs subsystem directory must have been created.
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created by ReconcileState: %v", statErr)
	}
}

func TestReconcileState_UnsupportedProtocolReported(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:    testVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Fence:       testFence(t),
				DevicePath:  testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
						ExportParams: &agentv1.ExportParams{},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 {
		t.Fatalf("Results len = %d, want 1", len(resp.GetResults()))
	}
	if resp.GetResults()[0].GetSuccess() {
		t.Fatal("expected unsupported protocol to be reported as a reconcile failure")
	}
	if !strings.Contains(
		resp.GetResults()[0].GetErrorMessage(),
		"protocol PROTOCOL_TYPE_SMB is not supported by this agent",
	) {
		t.Errorf("ErrorMessage = %q, want unsupported protocol detail", resp.GetResults()[0].GetErrorMessage())
	}
}

func TestReconcileState_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	req := &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:    testVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Fence:       testFence(t),
				DevicePath:  testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("10.0.0.1"),
				},
			},
		},
	}
	// First reconcile.
	resp1, err := srv.ReconcileState(context.Background(), req)
	if err != nil {
		t.Fatalf("first ReconcileState unexpected error: %v", err)
	}
	if !resp1.GetResults()[0].GetSuccess() {
		t.Fatalf("first reconcile failed: %q", resp1.GetResults()[0].GetErrorMessage())
	}

	// Second reconcile on the same state must also succeed (idempotent).
	resp2, err := srv.ReconcileState(context.Background(), req)
	if err != nil {
		t.Fatalf("second ReconcileState unexpected error: %v", err)
	}
	if !resp2.GetResults()[0].GetSuccess() {
		t.Fatalf("second reconcile failed (not idempotent): %q",
			resp2.GetResults()[0].GetErrorMessage())
	}
}

func TestReconcileState_WithAllowedInitiators(t *testing.T) {
	t.Parallel()
	hostNQN := testHostNQN
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:    testVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Fence:       testFence(t),
				DevicePath:  testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofACLExportState(hostNQN),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if !resp.GetResults()[0].GetSuccess() {
		t.Fatalf("reconcile failed: %q", resp.GetResults()[0].GetErrorMessage())
	}

	// The host directory must have been created by Apply().
	hostDir := filepath.Join(cfgRoot, "nvmet", "hosts", hostNQN)
	if _, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("host dir not created: %v", statErr)
	}
	// The allowed_hosts symlink must exist.
	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "allowed_hosts", hostNQN)
	if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Errorf("allowed_hosts symlink not created: %v", statErr)
	}
}

func TestReconcileState_MultipleVolumes(t *testing.T) {
	t.Parallel()
	const secondVolumeID = "tank/pvc-def"
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:    testVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Fence:       testFence(t),
				DevicePath:  testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("10.0.0.1"),
				},
			},
			{
				VolumeId:    secondVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Fence:       testFence(t),
				DevicePath:  "/dev/zvol/tank/pvc-def",
				Exports:     []*agentv1.ExportDesiredState{},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 2 {
		t.Fatalf("Results len = %d, want 2", len(resp.GetResults()))
	}

	// Both volumes must succeed.
	for _, result := range resp.GetResults() {
		if !result.GetSuccess() {
			t.Errorf("volume %q reconcile failed: %q",
				result.GetVolumeId(), result.GetErrorMessage())
		}
	}
	// First result must be for the first volume.
	if resp.GetResults()[0].GetVolumeId() != testVolumeID {
		t.Errorf("Results[0].VolumeId = %q, want %q",
			resp.GetResults()[0].GetVolumeId(), testVolumeID)
	}
	if resp.GetResults()[1].GetVolumeId() != secondVolumeID {
		t.Errorf("Results[1].VolumeId = %q, want %q",
			resp.GetResults()[1].GetVolumeId(), secondVolumeID)
	}
}

// TestReconcileState_RestoresLostTargetState: after the target loses its
// configfs state (reboot), reconcile re-creates the export with the ACL
// closed to everyone except the published initiator.
func TestReconcileState_RestoresLostTargetState(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testDevicePath, AclEnabled: true,
		Fence: testFence(t),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(cfgRoot, "nvmet")); err != nil {
		t.Fatalf("simulate target state loss: %v", err)
	}

	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState(testHostNQN))

	if got := readAllowAnyHost(t, cfgRoot, testVolumeNQN); got != "0" {
		t.Errorf("attr_allow_any_host = %q, want 0", got)
	}
	if !allowedHostLinked(cfgRoot, testVolumeNQN, testHostNQN) {
		t.Errorf("allowed_hosts entry for %s not restored", testHostNQN)
	}
	ns := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "namespaces", "1", "device_path")
	//nolint:gosec // G304: test reads a file under t.TempDir().
	if raw, readErr := os.ReadFile(ns); readErr != nil || strings.TrimSpace(string(raw)) != testDevicePath {
		t.Errorf("namespace device_path = %q (err %v), want %q", raw, readErr, testDevicePath)
	}
}

// TestReconcileState_EmptyACLStaysClosed: an ACL-enforced volume without any
// published node must admit nobody after reconcile (fail-closed).
func TestReconcileState_EmptyACLStaysClosed(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState())

	if got := readAllowAnyHost(t, cfgRoot, testVolumeNQN); got != "0" {
		t.Errorf("attr_allow_any_host = %q, want 0 (empty ACL must not fail open)", got)
	}
}

// TestReconcileState_ACLDisabledStaysOpen: without ACL enforcement the target
// admits any initiator even when initiators are listed.
func TestReconcileState_ACLDisabledStaysOpen(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1", testHostNQN))

	if got := readAllowAnyHost(t, cfgRoot, testVolumeNQN); got != "1" {
		t.Errorf("attr_allow_any_host = %q, want 1", got)
	}
}

// TestReconcileState_RevokesHostsOutsideDesiredSet: the ACL converges to the
// exact desired set, while the shared host object and other subsystems'
// grants of the revoked host are untouched.
func TestReconcileState_RevokesHostsOutsideDesiredSet(t *testing.T) {
	t.Parallel()
	const otherHost = "nqn.2023-01.io.example:host-2"
	const foreignNQN = "nqn.2014-08.org.example:foreign"
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})
	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState(testHostNQN, otherHost))

	foreignLink := filepath.Join(cfgRoot, "nvmet", "subsystems", foreignNQN, "allowed_hosts", testHostNQN)
	if err := os.MkdirAll(filepath.Dir(foreignLink), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cfgRoot, "nvmet", "hosts", testHostNQN), foreignLink); err != nil {
		t.Fatal(err)
	}

	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState(otherHost))

	if allowedHostLinked(cfgRoot, testVolumeNQN, testHostNQN) {
		t.Errorf("host %s outside the desired set is still allowed", testHostNQN)
	}
	if !allowedHostLinked(cfgRoot, testVolumeNQN, otherHost) {
		t.Errorf("desired host %s lost its grant", otherHost)
	}
	if _, err := os.Lstat(foreignLink); err != nil {
		t.Errorf("foreign subsystem grant was modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfgRoot, "nvmet", "hosts", testHostNQN)); err != nil {
		t.Errorf("shared host object removed: %v", err)
	}
}

// TestReconcileState_DerivesDevicePathFromBackend: an empty device_path is
// resolved through the backend that owns the volume.
func TestReconcileState_DerivesDevicePathFromBackend(t *testing.T) {
	t.Parallel()
	const backendPath = "/dev/zvol/tank/pvc-abc-from-backend"
	srv, cfgRoot := newExportTestServer(t, &mockBackend{devicePathResult: backendPath})

	reconcileOne(t, srv, "", nvmeofACLExportState())

	ns := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "namespaces", "1", "device_path")
	raw, err := os.ReadFile(ns) //nolint:gosec // G304: test reads a file under t.TempDir().
	if err != nil || strings.TrimSpace(string(raw)) != backendPath {
		t.Errorf("namespace device_path = %q (err %v), want %q", raw, err, backendPath)
	}
}

type mixedPoolReconcileEnv struct {
	srv     *agent.Server
	kernel  *liotest.Kernel
	root    string
	manager *nfs.Manager
}

func newMixedPoolReconcileEnv(t *testing.T, stateDir, datasetRoot string, datasetDefault bool) mixedPoolReconcileEnv {
	t.Helper()
	root := t.TempDir()
	kernel, err := liotest.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if probeErr := os.MkdirAll(filepath.Join(datasetRoot, "tank", "files"), 0o700); probeErr != nil {
		t.Fatal(probeErr)
	}
	manager, err := nfs.NewManager(nfs.Config{
		StateDir: filepath.Join(stateDir, "nfs"), ExportRoot: datasetRoot, BindAddress: "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := manager.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	zvol := zfs.New("tank", "blocks")
	dataset := zfs.NewDataset("tank", "files", datasetRoot)
	var defaultBackend backend.VolumeBackend = zvol
	if datasetDefault {
		defaultBackend = dataset
	}
	srv := agent.NewServer(map[string]backend.VolumeBackend{"tank": defaultBackend}, root,
		agent.WithBackendVariants(map[string]map[agentv1.BackendType]backend.VolumeBackend{
			"tank": {
				agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL:    zvol,
				agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET: dataset,
			},
		}),
		agent.WithNFSManager(manager),
		agent.WithLIOFS(kernel),
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
		agent.WithDeviceClaimer(kernel.Claimer()),
		agent.WithDrainStateDir(stateDir),
		agent.WithExportRestoreGate(),
	)
	return mixedPoolReconcileEnv{srv: srv, kernel: kernel, root: root, manager: manager}
}

func mixedPoolDesired(generation uint64, nvmeHost, iscsiHost string) []*agentv1.VolumeDesiredState {
	nvme := nvmeofACLExportState(nvmeHost)
	iscsi := &agentv1.ExportDesiredState{
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		ExportParams: iscsiExportParams("192.0.2.10", 3260),
		AclEnabled:   true, AllowedInitiators: []string{iscsiHost},
	}
	file := &agentv1.ExportDesiredState{
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
		ExportParams: &agentv1.ExportParams{Params: &agentv1.ExportParams_Nfs{Nfs: &agentv1.NfsExportParams{
			Version: "4.2", BindAddress: "192.0.2.10", Squash: "root",
		}}},
		AclEnabled: true, AllowedInitiators: []string{"192.0.2.20"},
	}
	return []*agentv1.VolumeDesiredState{
		{
			VolumeId: "tank/pvc-nvme", BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
			Fence:   &agentv1.FencingToken{VolumeUid: "nvme-owner", Generation: generation},
			Exports: []*agentv1.ExportDesiredState{nvme},
		},
		{
			VolumeId: "tank/pvc-iscsi", BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
			Fence:   &agentv1.FencingToken{VolumeUid: "iscsi-owner", Generation: generation},
			Exports: []*agentv1.ExportDesiredState{iscsi},
		},
		{
			VolumeId: "tank/pvc-nfs", BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
			Fence:   &agentv1.FencingToken{VolumeUid: "nfs-owner", Generation: generation},
			Exports: []*agentv1.ExportDesiredState{file},
		},
	}
}

func assertMixedPoolBlockExports(t *testing.T, env mixedPoolReconcileEnv, nvmeHost, iscsiHost string) {
	t.Helper()
	nvmeExports, err := nvmeof.ListExports(env.root)
	if err != nil || len(nvmeExports) != 1 {
		t.Fatalf("NVMe exports = %v, %v; want one restored subsystem", nvmeExports, err)
	}
	if got := nvmeExports[0].NamespaceDevicePaths[1]; got != "/dev/zvol/tank/blocks/pvc-nvme" {
		t.Fatalf("NVMe namespace path = %q; want the zvol in the block layout", got)
	}
	if !slices.Equal(nvmeExports[0].AllowedHosts, []string{nvmeHost}) {
		t.Fatalf("NVMe admission = %v; want [%s]", nvmeExports[0].AllowedHosts, nvmeHost)
	}
	if got := readAllowAnyHost(t, env.root, nvmeExports[0].NQN); got != "0" {
		t.Fatalf("NVMe allow_any_host = %q; want closed ACL", got)
	}
	iscsiExports, err := lio.ListTargets(env.kernel, env.root)
	if err != nil || len(iscsiExports) != 1 {
		t.Fatalf("iSCSI exports = %v, %v; want one restored target", iscsiExports, err)
	}
	if got := iscsiExports[0].DevicePath; got != "/dev/zvol/tank/blocks/pvc-iscsi" {
		t.Fatalf("iSCSI LUN path = %q; want the zvol in the block layout", got)
	}
	target := &lio.Target{ConfigfsRoot: env.root, FS: env.kernel, IQN: iscsiExports[0].IQN}
	initiators, err := target.Initiators()
	if err != nil || !slices.Equal(initiators, []string{iscsiHost}) {
		t.Fatalf("iSCSI admission = %v, %v; want [%s]", initiators, err, iscsiHost)
	}
}

// The real backend path resolvers and built-in protocol handlers must work
// regardless of which same-pool backend is registered as the default. The NFS
// directory is deliberately absent: that export must fail path validation, not
// ambiguous backend lookup, and must not suppress either block export.
func reconcileMixedPool(
	t *testing.T,
	env mixedPoolReconcileEnv,
	datasetRoot string,
	complete bool,
	vols []*agentv1.VolumeDesiredState,
) {
	t.Helper()
	resp, err := env.srv.ReconcileState(t.Context(), &agentv1.ReconcileStateRequest{Complete: complete, Volumes: vols})
	if err != nil || len(resp.GetResults()) != 3 {
		t.Fatalf("mixed-pool reconcile = %v, %v; want three item results", resp, err)
	}
	for i, result := range resp.GetResults() {
		if result.GetVolumeId() != vols[i].GetVolumeId() {
			t.Fatalf("result %d volume = %q; want %q", i, result.GetVolumeId(), vols[i].GetVolumeId())
		}
		if i < 2 && !result.GetSuccess() {
			t.Fatalf("block export %s failed: %s", result.GetVolumeId(), result.GetErrorMessage())
		}
		if vols[i].GetDevicePath() != "" {
			t.Fatalf("reconcile mutated caller device path for %s", vols[i].GetVolumeId())
		}
	}
	fileResult := resp.GetResults()[2]
	wantPath := filepath.Join(datasetRoot, "tank", "files", "pvc-nfs")
	if fileResult.GetSuccess() || !strings.Contains(fileResult.GetErrorMessage(), "resolve NFS export path") ||
		!strings.Contains(fileResult.GetErrorMessage(), wantPath) {
		t.Fatalf("NFS result = %v; want missing dataset path %q rejection", fileResult, wantPath)
	}
	healthErr := env.manager.Health()
	if healthErr == nil {
		t.Fatal("missing dataset export was reported healthy")
	}
	clientPath, err := env.manager.ExportPath(wantPath)
	if err != nil || clientPath != "/tank/files/pvc-nfs" {
		t.Fatalf("NFS client path = %q, %v; want dataset child path", clientPath, err)
	}
}

func TestReconcileState_MixedPoolTypedPathsRestore(t *testing.T) {
	t.Parallel()
	for _, datasetDefault := range []bool{false, true} {
		name := "zvol-default"
		if datasetDefault {
			name = "dataset-default"
		}
		t.Run(name, func(t *testing.T) {
			stateDir, datasetRoot := t.TempDir(), filepath.Join(t.TempDir(), "datasets")
			env := newMixedPoolReconcileEnv(t, stateDir, datasetRoot, datasetDefault)
			vols := mixedPoolDesired(2, testHostNQN, testInitiatorIQN)
			_, err := env.srv.ReconcileState(
				t.Context(),
				&agentv1.ReconcileStateRequest{Volumes: vols},
			)
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("partial restore before complete = %v; want Unavailable", err)
			}
			reconcileMixedPool(t, env, datasetRoot, true, vols)
			assertMixedPoolBlockExports(t, env, testHostNQN, testInitiatorIQN)

			const nextHost = "nqn.2026-01.com.bhyoo.pillar-csi:node.next"
			const nextIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.next"
			newer := mixedPoolDesired(3, nextHost, nextIQN)
			reconcileMixedPool(t, env, datasetRoot, false, newer)
			assertMixedPoolBlockExports(t, env, nextHost, nextIQN)

			// A reboot loses configfs but keeps the durable fencing directory.
			restarted := newMixedPoolReconcileEnv(t, stateDir, datasetRoot, datasetDefault)
			reconcileMixedPool(t, restarted, datasetRoot, true, newer)
			assertMixedPoolBlockExports(t, restarted, nextHost, nextIQN)
			stale, err := restarted.srv.ReconcileState(t.Context(), &agentv1.ReconcileStateRequest{Volumes: vols[:2]})
			if err != nil || len(stale.GetResults()) != 2 {
				t.Fatalf("stale reconcile = %v, %v", stale, err)
			}
			for _, result := range stale.GetResults() {
				if result.GetSuccess() || !strings.Contains(result.GetErrorMessage(), "stale fencing token") {
					t.Fatalf("stale restore was not fenced: %v", result)
				}
			}
			assertMixedPoolBlockExports(t, restarted, nextHost, nextIQN)
		})
	}
}

func TestReconcileState_MixedPoolPreservesExplicitNFSPathAndTypeGuard(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	env := newMixedPoolReconcileEnv(t, stateDir, filepath.Join(t.TempDir(), "datasets"), false)
	for _, complete := range []bool{true, false} {
		vol := mixedPoolDesired(1, testHostNQN, testInitiatorIQN)[2]
		vol.DevicePath = "/"
		resp, err := env.srv.ReconcileState(t.Context(), &agentv1.ReconcileStateRequest{
			Complete: complete, Volumes: []*agentv1.VolumeDesiredState{vol},
		})
		if err != nil || len(resp.GetResults()) != 1 {
			t.Fatalf("explicit NFS path reconcile = %v, %v", resp, err)
		}
		if result := resp.GetResults()[0]; result.GetSuccess() ||
			!strings.Contains(result.GetErrorMessage(), `unsafe NFS export path "/"`) {
			t.Fatalf("explicit unsafe path was replaced or accepted: %v", result)
		}
		if vol.GetDevicePath() != "/" {
			t.Fatal("reconcile mutated caller's explicit path")
		}
		vol.BackendType = agentv1.BackendType_BACKEND_TYPE_LVM
		vol.Fence.Generation = 2
		resp, err = env.srv.ReconcileState(t.Context(), &agentv1.ReconcileStateRequest{
			Complete: complete, Volumes: []*agentv1.VolumeDesiredState{vol},
		})
		if err != nil || len(resp.GetResults()) != 1 || resp.GetResults()[0].GetSuccess() ||
			!strings.Contains(resp.GetResults()[0].GetErrorMessage(), "backend_type") {
			t.Fatalf("explicit path bypassed backend-type guard: %v, %v", resp, err)
		}
	}
	if err := env.manager.Health(); err == nil {
		t.Fatal("refused NFS export was reported healthy")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "nfs")); !os.IsNotExist(err) {
		t.Fatalf("refused NFS export created runtime ownership/admission state: %v", err)
	}
}

// ReconcileState re-verifies a pinned LV before re-exporting it: a volume
// whose LV was replaced fails on its own and gets no target, while the other
// listed volumes are still reconciled.  The second volume is an unpinned
// managed LV with no adopted identity behind it, so it is reconciled only if
// unpinned volumes are not verified.
func TestPinnedSource_RecheckedBeforeReconcile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		replaced   bool
		wantFirst  bool
		wantTarget int
	}{
		{"verified", false, true, 2},
		{"replaced LV", true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			if tc.replaced {
				b.replaceLV()
			}
			srv, stateDir, cfgRoot := newLVTestServer(t, b)
			seeded := seedMark(t, stateDir, pinnedMark("lifecycle-a", 1, testLVSource(), true))

			resp, err := srv.ReconcileState(context.Background(), pinnedAndUnpinnedReconcileRequest(t))
			if err != nil {
				t.Fatalf("ReconcileState: %v", err)
			}
			requirePinnedReconcileResults(t, resp, tc.wantFirst)
			if n := nvmetSubsystems(t, cfgRoot); n != tc.wantTarget {
				t.Errorf("NVMe-oF subsystems = %d, want %d", n, tc.wantTarget)
			}
			if !tc.wantFirst {
				requireMarkUnchanged(t, stateDir, seeded, "refused reconcile")
			}
		})
	}
}

// requirePinnedReconcileResults asserts ReconcileState reported one result
// per volume: the pinned volume succeeded exactly when wantFirst, and the
// unpinned volume always succeeded.
func requirePinnedReconcileResults(t *testing.T, resp *agentv1.ReconcileStateResponse, wantFirst bool) {
	t.Helper()
	if len(resp.GetResults()) != 2 {
		t.Fatalf("Results = %v, want 2", resp.GetResults())
	}
	if got := resp.GetResults()[0].GetSuccess(); got != wantFirst {
		t.Errorf("pinned volume success = %t, want %t (%q)",
			got, wantFirst, resp.GetResults()[0].GetErrorMessage())
	}
	if !resp.GetResults()[1].GetSuccess() {
		t.Errorf("unpinned volume failed: %q", resp.GetResults()[1].GetErrorMessage())
	}
}

// pinnedAndUnpinnedReconcileRequest asks ReconcileState to export
// testVolumeID's pinned LV under lifecycle-a and the unpinned managed LV
// tank/pvc-def over NVMe-oF.
func pinnedAndUnpinnedReconcileRequest(t *testing.T) *agentv1.ReconcileStateRequest {
	t.Helper()
	const secondVolumeID = "tank/pvc-def"
	return &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:    testVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				Fence:       token2("lifecycle-a", 1),
				DevicePath:  testLVDevicePath,
				Exports:     []*agentv1.ExportDesiredState{nvmeofExportState("10.0.0.1")},
			},
			{
				VolumeId:    secondVolumeID,
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				Fence:       testFence(t),
				DevicePath:  "/dev/tank/pvc-def",
				Exports:     []*agentv1.ExportDesiredState{nvmeofExportState("10.0.0.1")},
			},
		},
	}
}
