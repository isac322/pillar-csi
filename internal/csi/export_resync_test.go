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

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/testutil/fakeuid"
)

// These tests drive ReconcileVolumeExport against a real agent.Server whose
// configfs root is a temp dir; the only fakes are the Kubernetes API (fake
// client) and the in-memory gRPC transport.

const (
	resyncAgentName  = "storage-1"
	resyncAgentVolID = "tank/pvc-resync"
	resyncVolumeID   = resyncAgentName + "/nvmeof-tcp/zfs-zvol/" + resyncAgentVolID
	resyncPVSName    = "pvc-resync"
	resyncPVSUID     = "11111111-2222-3333-4444-555566667777"
	resyncNQN        = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-resync"
	resyncDevicePath = "/dev/zvol/tank/pvc-resync"
	resyncHostA      = "nqn.2023-01.io.example:host-a"
	resyncHostB      = "nqn.2023-01.io.example:host-b"
)

type resyncBackend struct{}

func (resyncBackend) Create(
	context.Context, string, int64, *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, err error) {
	return resyncDevicePath, 1 << 30, nil
}
func (resyncBackend) Delete(context.Context, string) error                 { return nil }
func (resyncBackend) Expand(context.Context, string, int64) (int64, error) { return 1 << 30, nil }
func (resyncBackend) Capacity(context.Context) (totalBytes, availableBytes int64, err error) {
	return 1 << 40, 1 << 40, nil
}
func (resyncBackend) ListVolumes(context.Context) ([]*agentv1.VolumeInfo, error) {
	return nil, nil
}
func (resyncBackend) DevicePath(string) string  { return resyncDevicePath }
func (resyncBackend) Type() agentv1.BackendType { return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL }

type resyncEnv struct {
	srv            *ControllerServer
	agentClient    agentv1.AgentServiceClient
	cfgRoot        string
	reconcileCalls *atomic.Int32
}

func newResyncEnv(t *testing.T, pvs *v1alpha1.PillarVolumeState) *resyncEnv {
	return newResyncEnvState(t, pvs, t.TempDir())
}

// newResyncEnvState is newResyncEnv with an explicit agent state dir, so a
// test can restart the agent (new Server, same stateDir) and verify that the
// fencing generation mark survives.
func newResyncEnvState(t *testing.T, pvs *v1alpha1.PillarVolumeState, stateDir string) *resyncEnv {
	cfgRoot := t.TempDir()
	calls := &atomic.Int32{}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.UnaryInterceptor(func(
		ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler,
	) (any, error) {
		if info.FullMethod == agentv1.AgentService_ReconcileState_FullMethodName {
			calls.Add(1)
		}
		return h(ctx, req)
	}))
	agentv1.RegisterAgentServiceServer(gs,
		agent.NewServer(map[string]backend.VolumeBackend{"tank": resyncBackend{}}, cfgRoot,
			agent.WithDrainStateDir(stateDir)))
	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()
	t.Cleanup(func() {
		gs.Stop()
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("agent gRPC server: %v", err)
		}
	})

	conn, err := grpc.NewClient("passthrough:///agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("agent connection close: %v", closeErr)
		}
	})
	agentClient := agentv1.NewAgentServiceClient(conn)

	scheme := runtime.NewScheme()
	if err = v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	pa := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: resyncAgentName},
		Status:     v1alpha1.PillarAgentStatus{ResolvedAddress: "127.0.0.1:9500"},
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pa).
		WithStatusSubresource(&v1alpha1.PillarAgent{}, &v1alpha1.PillarVolumeState{}).
		WithInterceptorFuncs(fakeuid.Interceptor())
	if pvs != nil {
		builder = builder.WithObjects(pvs)
	}
	dialer := func(context.Context, string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agentClient, nopCloser{}, nil
	}
	return &resyncEnv{
		srv:            NewControllerServerWithDialer(builder.Build(), "pillar-csi.bhyoo.com", dialer),
		agentClient:    agentClient,
		cfgRoot:        cfgRoot,
		reconcileCalls: calls,
	}
}

// resyncPVS builds the volume's PillarVolumeState at publication fencing
// generation 1.
func resyncPVS(spec *v1alpha1.VolumeExportSpec, initiators ...string) *v1alpha1.PillarVolumeState {
	pvs := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: resyncPVSName, UID: types.UID(resyncPVSUID)},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID: resyncVolumeID, AgentVolumeID: resyncAgentVolID, AgentRef: resyncAgentName,
			BackendType: "zfs-zvol", ProtocolType: "nvmeof-tcp",
		},
		Status: v1alpha1.PillarVolumeStateStatus{
			Phase: v1alpha1.PillarVolumeStatePhaseReady, ExportSpec: spec,
			PublicationGeneration: 1,
		},
	}
	for i, initiator := range initiators {
		pvs.Status.PublishedNodes = append(pvs.Status.PublishedNodes, v1alpha1.VolumePublication{
			NodeID: "node-" + string(rune('a'+i)), InitiatorID: initiator, AccessMode: "SINGLE_NODE_WRITER",
		})
	}
	return pvs
}

var aclSpec = &v1alpha1.VolumeExportSpec{BindAddress: "10.0.0.1", Port: 4420, ACLEnabled: true}

// resyncFence is the fencing token of the test volume's lifecycle at gen.
func resyncFence(gen uint64) *agentv1.FencingToken {
	return &agentv1.FencingToken{VolumeUid: resyncPVSUID, Generation: gen}
}

// exportWithHosts creates the volume's export through the agent RPCs, as
// CreateVolume and ControllerPublishVolume do, at generation 1 of the
// volume's lifecycle.
func (e *resyncEnv) exportWithHosts(t *testing.T, hosts ...string) {
	t.Helper()
	ctx := context.Background()
	_, err := e.agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: exportParamsFor(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, aclSpec),
		DevicePath:   resyncDevicePath, AclEnabled: true,
		Fence: resyncFence(1),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	for _, host := range hosts {
		_, err = e.agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
			VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, InitiatorId: host,
			Fence: resyncFence(1),
		})
		if err != nil {
			t.Fatalf("AllowInitiator(%s): %v", host, err)
		}
	}
}

func (e *resyncEnv) subsystemPath(elem ...string) string {
	return filepath.Join(append([]string{e.cfgRoot, "nvmet", "subsystems", resyncNQN}, elem...)...)
}

func (e *resyncEnv) hostAllowed(host string) bool {
	_, err := os.Lstat(e.subsystemPath("allowed_hosts", host))
	return err == nil
}

func (e *resyncEnv) exportCondition(t *testing.T) *metav1.Condition {
	t.Helper()
	pvs := &v1alpha1.PillarVolumeState{}
	err := e.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: resyncPVSName}, pvs)
	if err != nil {
		t.Fatalf("get PillarVolumeState: %v", err)
	}
	return meta.FindStatusCondition(pvs.Status.Conditions, ConditionExportReconciled)
}

func TestReconcileVolumeExport_RestoresExportAndACLAfterTargetStateLoss(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA))
	env.exportWithHosts(t, resyncHostA)
	if err := os.RemoveAll(filepath.Join(env.cfgRoot, "nvmet")); err != nil {
		t.Fatalf("simulate target state loss: %v", err)
	}

	if err := env.srv.ReconcileVolumeExport(context.Background(), resyncPVSName); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}

	raw, err := os.ReadFile(env.subsystemPath("namespaces", "1", "device_path"))
	if err != nil || strings.TrimSpace(string(raw)) != resyncDevicePath {
		t.Fatalf("namespace not restored with backend device: %q err=%v", raw, err)
	}
	raw, err = os.ReadFile(env.subsystemPath("attr_allow_any_host"))
	if err != nil || strings.TrimSpace(string(raw)) != "0" {
		t.Errorf("attr_allow_any_host = %q err=%v, want 0", raw, err)
	}
	if !env.hostAllowed(resyncHostA) {
		t.Errorf("published initiator %s not re-granted", resyncHostA)
	}
	cond := env.exportCondition(t)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("ExportReconciled condition = %+v, want True", cond)
	}
}

func TestReconcileVolumeExport_ACLIsExactlyPublishedNodes(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostB))
	env.exportWithHosts(t, resyncHostA, resyncHostB)

	if err := env.srv.ReconcileVolumeExport(context.Background(), resyncPVSName); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}

	if env.hostAllowed(resyncHostA) {
		t.Errorf("initiator %s without a publication is still allowed", resyncHostA)
	}
	if !env.hostAllowed(resyncHostB) {
		t.Errorf("published initiator %s lost access", resyncHostB)
	}
}

func TestReconcileVolumeExport_LeavesOtherTargetsUntouched(t *testing.T) {
	t.Parallel()
	const foreignNQN = "nqn.2014-08.org.example:foreign"
	env := newResyncEnv(t, resyncPVS(aclSpec))
	foreign := filepath.Join(env.cfgRoot, "nvmet", "subsystems", foreignNQN)
	if err := os.MkdirAll(foreign, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "attr_allow_any_host"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := env.srv.ReconcileVolumeExport(context.Background(), resyncPVSName); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}

	//nolint:gosec // G304: test reads a file under t.TempDir().
	raw, err := os.ReadFile(filepath.Join(foreign, "attr_allow_any_host"))
	if err != nil || strings.TrimSpace(string(raw)) != "1" {
		t.Errorf("foreign subsystem modified: %q err=%v", raw, err)
	}
	raw, err = os.ReadFile(env.subsystemPath("attr_allow_any_host"))
	if err != nil || strings.TrimSpace(string(raw)) != "0" {
		t.Errorf("own ACL without publications = %q err=%v, want 0 (fail-closed)", raw, err)
	}
}

func TestReconcileVolumeExport_MissingExportSpecIsReportedNotApplied(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(nil, resyncHostA))

	if err := env.srv.ReconcileVolumeExport(context.Background(), resyncPVSName); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}

	if n := env.reconcileCalls.Load(); n != 0 {
		t.Errorf("agent ReconcileState called %d times without exportSpec", n)
	}
	cond := env.exportCondition(t)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonExportSpecMissing {
		t.Errorf("ExportReconciled condition = %+v, want False/%s", cond, reasonExportSpecMissing)
	}
}

func TestReconcileVolumeExport_DeletedVolumeIsNotReExported(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, nil)

	if err := env.srv.ReconcileVolumeExport(context.Background(), resyncPVSName); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}

	if n := env.reconcileCalls.Load(); n != 0 {
		t.Errorf("agent ReconcileState called %d times for a deleted volume", n)
	}
	if _, err := os.Stat(env.subsystemPath()); !os.IsNotExist(err) {
		t.Errorf("export created for a deleted volume (stat err=%v)", err)
	}
}

func TestReconcileVolumeExport_AgentFailureIsReportedAndRetried(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA))
	// Unknown pool: the agent cannot resolve the backend device.
	pvs := &v1alpha1.PillarVolumeState{}
	ctx := context.Background()
	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: resyncPVSName}, pvs); err != nil {
		t.Fatal(err)
	}
	pvs.Spec.AgentVolumeID = "missing-pool/pvc-resync"
	if err := env.srv.k8sClient.Update(ctx, pvs); err != nil {
		t.Fatal(err)
	}

	if err := env.srv.ReconcileVolumeExport(ctx, resyncPVSName); err == nil {
		t.Fatal("ReconcileVolumeExport succeeded although the agent could not reconcile the volume")
	}

	cond := env.exportCondition(t)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonReconcileFailed {
		t.Errorf("ExportReconciled condition = %+v, want False/%s", cond, reasonReconcileFailed)
	}
}

func TestReconcileVolumeExport_SkipsDeletingVolume(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA))
	ctx := context.Background()
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: resyncPVSName}, pvs); err != nil {
		t.Fatal(err)
	}
	pvs.Status.Deleting = true
	if err := env.srv.k8sClient.Status().Update(ctx, pvs); err != nil {
		t.Fatal(err)
	}

	if err := env.srv.ReconcileVolumeExport(ctx, resyncPVSName); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}

	if n := env.reconcileCalls.Load(); n != 0 {
		t.Errorf("agent ReconcileState called %d times for a deleting volume", n)
	}
	if _, err := os.Stat(env.subsystemPath()); !os.IsNotExist(err) {
		t.Errorf("export re-created for a deleting volume (stat err=%v)", err)
	}
}

// A resync carrying a generation older than the agent's high-water mark is a
// stale-leader write: the agent must reject it and the resync must report
// StaleGeneration so the next pass retries with the fresh generation.
func TestReconcileVolumeExport_StaleGenerationIsRejected(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA))
	env.exportWithHosts(t, resyncHostA)
	ctx := context.Background()

	// A newer publication transition (generation 2) reaches the agent first.
	if _, err := env.agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: resyncHostB,
		Fence:       resyncFence(2),
	}); err != nil {
		t.Fatalf("AllowInitiator gen=2: %v", err)
	}

	if err := env.srv.ReconcileVolumeExport(ctx, resyncPVSName); err == nil {
		t.Fatal("stale resync succeeded although the agent holds a newer generation")
	}

	// The stale exact-set write must not have revoked the newer grant.
	if !env.hostAllowed(resyncHostB) {
		t.Errorf("stale resync revoked hostB granted at generation 2")
	}
	cond := env.exportCondition(t)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonStaleGeneration {
		t.Errorf("ExportReconciled condition = %+v, want False/%s", cond, reasonStaleGeneration)
	}
}

// The fencing mark lives on host disk: a restarted agent (new Server, same
// state dir) still rejects a stale generation, and a fresh resync restores
// the lost configfs state.
func TestReconcileVolumeExport_FencingMarkSurvivesAgentRestart(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	env := newResyncEnvState(t, resyncPVS(aclSpec, resyncHostA), stateDir)
	env.exportWithHosts(t, resyncHostA)
	ctx := context.Background()

	if _, err := env.agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: resyncHostB,
		Fence:       resyncFence(2),
	}); err != nil {
		t.Fatalf("AllowInitiator gen=2: %v", err)
	}

	// Agent restart: new Server, same state dir, wiped configfs.
	env2 := newResyncEnvState(t, resyncPVS(aclSpec, resyncHostA), stateDir)

	if err := env2.srv.ReconcileVolumeExport(ctx, resyncPVSName); err == nil {
		t.Fatal("stale resync succeeded against a restarted agent holding generation 2")
	}
	if _, err := os.Stat(env2.subsystemPath()); !os.IsNotExist(err) {
		t.Errorf("stale resync applied configfs state after agent restart (stat err=%v)", err)
	}

	// The next pass carries the fresh generation and restores the export.
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env2.srv.k8sClient.Get(ctx, types.NamespacedName{Name: resyncPVSName}, pvs); err != nil {
		t.Fatal(err)
	}
	pvs.Status.PublicationGeneration = 2
	pvs.Status.PublishedNodes = append(pvs.Status.PublishedNodes, v1alpha1.VolumePublication{
		NodeID: "node-b", InitiatorID: resyncHostB, AccessMode: "SINGLE_NODE_WRITER",
	})
	if err := env2.srv.k8sClient.Status().Update(ctx, pvs); err != nil {
		t.Fatal(err)
	}
	if err := env2.srv.ReconcileVolumeExport(ctx, resyncPVSName); err != nil {
		t.Fatalf("fresh resync after restart: %v", err)
	}
	if !env2.hostAllowed(resyncHostA) || !env2.hostAllowed(resyncHostB) {
		t.Errorf("post-restart resync did not restore the published initiator set")
	}
}
