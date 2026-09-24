package csi

// Cross-controller fencing regression tests (isac322/pillar-csi#56).
//
// Two ControllerServer instances (a current and a stale leader) share one fake
// Kubernetes API server and one REAL agent.Server: a fake volume backend, a
// temp configfs tree, and a temp durable state dir, so the agent's actual
// fencing store and configfs ACL writes are exercised.  Each stale RPC is held
// at the agent boundary by a channel gate, so every interleaving is
// deterministic.  Every scenario asserts that the stale RPC is rejected by the
// agent and that the state the newer operation produced is preserved.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ── Real agent with a fake backend ──────────────────────────────────────────.

// fenceTestBackend is an in-memory backend.VolumeBackend.
type fenceTestBackend struct {
	mu      sync.Mutex
	volumes map[string]bool
	// roundUp is added to every allocation, like a zvol rounded to its block size.
	roundUp int64
}

func (b *fenceTestBackend) Create(
	_ context.Context, volumeID string, capacityBytes int64, _ *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.volumes[volumeID] = true
	return "/dev/fake/" + volumeID, capacityBytes + b.roundUp, nil
}

func (b *fenceTestBackend) Delete(_ context.Context, volumeID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.volumes, volumeID)
	return nil
}

func (*fenceTestBackend) Expand(_ context.Context, _ string, requested int64) (int64, error) {
	return requested, nil
}

func (*fenceTestBackend) Capacity(context.Context) (totalBytes, availableBytes int64, err error) {
	return 1 << 40, 1 << 40, nil
}

func (*fenceTestBackend) ListVolumes(context.Context) ([]*agentv1.VolumeInfo, error) { return nil, nil }

func (*fenceTestBackend) DevicePath(volumeID string) string { return "/dev/fake/" + volumeID }

func (*fenceTestBackend) Type() agentv1.BackendType { return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL }

// hasTestVolume reports whether the test volume's backend resource exists.
func (b *fenceTestBackend) hasTestVolume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.volumes[fenceAgentVolumeID]
}

var _ backend.VolumeBackend = (*fenceTestBackend)(nil)

// fenceAgent is one real agent process shared by the controllers of a test.
type fenceAgent struct {
	srv      *agent.Server
	cfgRoot  string
	stateDir string
	backend  *fenceTestBackend
}

func newFenceAgent(t *testing.T) *fenceAgent {
	t.Helper()
	a := &fenceAgent{
		cfgRoot:  t.TempDir(),
		stateDir: t.TempDir(),
		backend:  &fenceTestBackend{volumes: map[string]bool{}},
	}
	a.restart()
	return a
}

// restart replaces the agent process with a new one on the same state dir
// and configfs root, like an agent restart or node reboot.
func (a *fenceAgent) restart() {
	a.srv = agent.NewServer(map[string]backend.VolumeBackend{"tank": a.backend}, a.cfgRoot,
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
		agent.WithDrainStateDir(a.stateDir))
}

// allowedHosts lists every initiator granted on any target.
func (a *fenceAgent) allowedHosts(t *testing.T) []string {
	t.Helper()
	links, err := filepath.Glob(filepath.Join(a.cfgRoot, "nvmet", "subsystems", "*", "allowed_hosts", "*"))
	if err != nil {
		t.Fatal(err)
	}
	hosts := make([]string, 0, len(links))
	for _, link := range links {
		hosts = append(hosts, filepath.Base(link))
	}
	slices.Sort(hosts)
	return hosts
}

// exported reports whether any target subsystem exists.
func (a *fenceAgent) exported(t *testing.T) bool {
	t.Helper()
	subs, err := filepath.Glob(filepath.Join(a.cfgRoot, "nvmet", "subsystems", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(subs) > 0
}

// fenceGates holds a controller's in-flight RPCs at the agent boundary.  A
// non-nil gate makes the call signal entered and wait BEFORE reaching the
// agent; afterDeny waits AFTER a successful DenyInitiator.
type fenceGates struct {
	allow, export, deleteVol, deny, afterDeny chan struct{}
	entered                                   chan struct{}
}

func (g *fenceGates) hold(gate chan struct{}) {
	if gate != nil {
		g.entered <- struct{}{}
		<-gate
	}
}

// fenceClient adapts the real agent to agentv1.AgentServiceClient for one
// controller.  Requests reach the production agent.Server unchanged.
type fenceClient struct {
	mockAgentClient
	agent *fenceAgent
	gates fenceGates
	// exportFailures makes the next ExportVolume calls fail with Unavailable.
	exportFailures int
}

func (c *fenceClient) CreateVolume(
	ctx context.Context, req *agentv1.CreateVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.CreateVolumeResponse, error) {
	return c.agent.srv.CreateVolume(ctx, req)
}

func (c *fenceClient) DeleteVolume(
	ctx context.Context, req *agentv1.DeleteVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.DeleteVolumeResponse, error) {
	c.gates.hold(c.gates.deleteVol)
	return c.agent.srv.DeleteVolume(ctx, req)
}

func (c *fenceClient) ExportVolume(
	ctx context.Context, req *agentv1.ExportVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.ExportVolumeResponse, error) {
	c.gates.hold(c.gates.export)
	if c.exportFailures > 0 {
		c.exportFailures--
		return nil, status.Error(codes.Unavailable, "injected export failure")
	}
	return c.agent.srv.ExportVolume(ctx, req)
}

func (c *fenceClient) UnexportVolume(
	ctx context.Context, req *agentv1.UnexportVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.UnexportVolumeResponse, error) {
	return c.agent.srv.UnexportVolume(ctx, req)
}

func (c *fenceClient) AllowInitiator(
	ctx context.Context, req *agentv1.AllowInitiatorRequest, _ ...grpc.CallOption,
) (*agentv1.AllowInitiatorResponse, error) {
	c.gates.hold(c.gates.allow)
	return c.agent.srv.AllowInitiator(ctx, req)
}

func (c *fenceClient) DenyInitiator(
	ctx context.Context, req *agentv1.DenyInitiatorRequest, _ ...grpc.CallOption,
) (*agentv1.DenyInitiatorResponse, error) {
	c.gates.hold(c.gates.deny)
	resp, err := c.agent.srv.DenyInitiator(ctx, req)
	if err == nil {
		c.gates.hold(c.gates.afterDeny)
	}
	return resp, err
}

func (c *fenceClient) ExpandVolume(
	ctx context.Context, req *agentv1.ExpandVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.ExpandVolumeResponse, error) {
	return c.agent.srv.ExpandVolume(ctx, req)
}

// fenceServer builds a controller that talks to the agent through client c.
func fenceServer(env *controllerTestEnv, c *fenceClient) *ControllerServer {
	return NewControllerServerWithDialer(env.srv.k8sClient, "pillar-csi.bhyoo.com",
		func(context.Context, string) (agentv1.AgentServiceClient, io.Closer, error) {
			return c, nopCloser{}, nil
		})
}

// newFencePair returns the clients of a stale leader (whose gates the caller
// sets) and a current leader, both talking to agent a.
func newFencePair(a *fenceAgent) (stale, current *fenceClient) {
	stale = &fenceClient{agent: a, gates: fenceGates{entered: make(chan struct{}, 1)}}
	current = &fenceClient{agent: a}
	return stale, current
}

func waitHeld(t *testing.T, g *fenceGates) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("gated RPC was never reached")
	}
}

// runStale starts op in the background and returns a wait function.
func runStale(op func() error) func() error {
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err = op()
	}()
	return func() error {
		<-done
		return err
	}
}

func fenceVolumeID() string { return basePublishRequest().GetVolumeId() }

const fenceAgentVolumeID = "tank/pvc-abc123"

func createVolume(t *testing.T, srv *ControllerServer) {
	t.Helper()
	if _, err := srv.CreateVolume(context.Background(), baseCreateVolumeRequest()); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
}

func deleteVolume(t *testing.T, srv *ControllerServer) {
	t.Helper()
	if _, err := srv.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: fenceVolumeID()}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
}

func publish(t *testing.T, srv *ControllerServer, node string) {
	t.Helper()
	if _, err := srv.ControllerPublishVolume(context.Background(),
		exclPublishReq(fenceVolumeID(), node, snw, false)); err != nil {
		t.Fatalf("publish %s: %v", node, err)
	}
}

func unpublish(t *testing.T, srv *ControllerServer, node string) {
	t.Helper()
	if _, err := srv.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: fenceVolumeID(), NodeId: node,
	}); err != nil {
		t.Fatalf("unpublish %s: %v", node, err)
	}
}

func requireStaleRejected(t *testing.T, err error) {
	t.Helper()
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale operation: %v, want FailedPrecondition from the agent", err)
	}
}

const snw = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER

// ── Stale agent RPCs ────────────────────────────────────────────────────────.

// A stale leader's AllowInitiator that lands after a new leader revoked the
// node and granted another must not produce two writers.
func TestFencing_StaleGrantAfterRevoke(t *testing.T) {
	t.Parallel()
	env := newPublishTestEnv(t, exclCSINodes()...)
	ag := newFenceAgent(t)
	staleC, curC := newFencePair(ag)
	staleC.gates.allow = make(chan struct{})
	stale, cur := fenceServer(env, staleC), fenceServer(env, curC)
	createVolume(t, cur)

	wait := runStale(func() error {
		_, err := stale.ControllerPublishVolume(context.Background(),
			exclPublishReq(fenceVolumeID(), exclNode1, snw, false))
		return err
	})
	waitHeld(t, &staleC.gates) // X recorded, AllowInitiator(X) in flight
	unpublish(t, cur, exclNode1)
	publish(t, cur, exclNode2)
	close(staleC.gates.allow)

	requireStaleRejected(t, wait())
	if hosts := ag.allowedHosts(t); !slices.Equal(hosts, []string{exclNQN(exclNode2)}) {
		t.Errorf("hosts %v, want only %s", hosts, exclNQN(exclNode2))
	}
}

// A stale CreateVolume export must not re-create the target of a volume a
// newer controller already deleted.
func TestFencing_StaleExportAfterDelete(t *testing.T) {
	t.Parallel()
	env := newPublishTestEnv(t)
	ag := newFenceAgent(t)
	staleC, curC := newFencePair(ag)
	staleC.gates.export = make(chan struct{})
	stale, cur := fenceServer(env, staleC), fenceServer(env, curC)

	wait := runStale(func() error {
		_, err := stale.CreateVolume(context.Background(), baseCreateVolumeRequest())
		return err
	})
	waitHeld(t, &staleC.gates) // backend created, ExportVolume in flight
	deleteVolume(t, cur)
	close(staleC.gates.export)

	requireStaleRejected(t, wait())
	if ag.exported(t) {
		t.Error("stale export re-created the target of a deleted volume")
	}
}

// A stale delete that already passed UnexportVolume must not destroy the
// backend of a volume that was deleted and re-created under the same name
// before its backend DeleteVolume landed.
func TestFencing_StaleBackendDeleteAfterRecreate(t *testing.T) {
	t.Parallel()
	env := newPublishTestEnv(t)
	ag := newFenceAgent(t)
	staleC, curC := newFencePair(ag)
	staleC.gates.deleteVol = make(chan struct{})
	stale, cur := fenceServer(env, staleC), fenceServer(env, curC)
	createVolume(t, cur)

	wait := runStale(func() error {
		_, err := stale.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: fenceVolumeID()})
		return err
	})
	waitHeld(t, &staleC.gates) // deleting committed, unexported, backend delete in flight
	deleteVolume(t, cur)       // the new leader finishes the delete
	createVolume(t, cur)       // and the name is re-created: a new lifecycle
	close(staleC.gates.deleteVol)

	requireStaleRejected(t, wait())
	if !ag.backend.hasTestVolume() {
		t.Error("stale delete destroyed the re-created volume's backend")
	}
	if !ag.exported(t) {
		t.Error("re-created volume lost its export")
	}
	if _, exists, err := env.srv.readVolumeState(context.Background(), "pvc-abc123"); err != nil || !exists {
		t.Errorf("re-created PillarVolumeState: exists=%t err=%v, want kept", exists, err)
	}
}

// A stale grant from a previous lifecycle of the volume name must not grant
// access on the re-created volume.
func TestFencing_StaleGrantAcrossRecreate(t *testing.T) {
	t.Parallel()
	env := newPublishTestEnv(t, exclCSINodes()...)
	ag := newFenceAgent(t)
	staleC, curC := newFencePair(ag)
	staleC.gates.allow = make(chan struct{})
	stale, cur := fenceServer(env, staleC), fenceServer(env, curC)
	createVolume(t, cur)

	wait := runStale(func() error {
		_, err := stale.ControllerPublishVolume(context.Background(),
			exclPublishReq(fenceVolumeID(), exclNode1, snw, false))
		return err
	})
	waitHeld(t, &staleC.gates) // lifecycle 1: X recorded, AllowInitiator in flight
	unpublish(t, cur, exclNode1)
	deleteVolume(t, cur)
	createVolume(t, cur) // lifecycle 2 of the same name
	close(staleC.gates.allow)

	requireStaleRejected(t, wait())
	if hosts := ag.allowedHosts(t); len(hosts) != 0 {
		t.Errorf("re-created volume has grants %v, want none", hosts)
	}
	if !ag.backend.hasTestVolume() {
		t.Error("re-created volume's backend is missing")
	}
}

// The fencing state is durable: an agent restarted on the same state dir
// (node reboot, configfs gone) still rejects the stale grant.
func TestFencing_StaleGrantRejectedAfterAgentRestart(t *testing.T) {
	t.Parallel()
	env := newPublishTestEnv(t, exclCSINodes()...)
	ag := newFenceAgent(t)
	staleC, curC := newFencePair(ag)
	staleC.gates.allow = make(chan struct{})
	stale, cur := fenceServer(env, staleC), fenceServer(env, curC)
	createVolume(t, cur)

	wait := runStale(func() error {
		_, err := stale.ControllerPublishVolume(context.Background(),
			exclPublishReq(fenceVolumeID(), exclNode1, snw, false))
		return err
	})
	waitHeld(t, &staleC.gates)
	unpublish(t, cur, exclNode1)

	if err := os.RemoveAll(filepath.Join(ag.cfgRoot, "nvmet")); err != nil {
		t.Fatal(err)
	}
	ag.restart()
	close(staleC.gates.allow)

	requireStaleRejected(t, wait())
	if hosts := ag.allowedHosts(t); len(hosts) != 0 {
		t.Errorf("hosts %v after restart, want none", hosts)
	}
}

// ── Controller-record scenarios ─────────────────────────────────────────────.

// While an unpublish is between its fenced revoke and dropping the record, a
// publish of the same node is refused, on both sides of the DenyInitiator.
func TestFencing_RepublishDuringRevokeIsRejected(t *testing.T) {
	t.Parallel()
	t.Run("held before DenyInitiator", func(t *testing.T) {
		t.Parallel()
		checkRepublishDuringRevoke(t, false)
	})
	t.Run("held after DenyInitiator", func(t *testing.T) {
		t.Parallel()
		checkRepublishDuringRevoke(t, true)
	})
}

func checkRepublishDuringRevoke(t *testing.T, afterDeny bool) {
	t.Helper()
	env := newPublishTestEnv(t, exclCSINodes()...)
	ag := newFenceAgent(t)
	staleC, curC := newFencePair(ag)
	stale, cur := fenceServer(env, staleC), fenceServer(env, curC)
	ctx := context.Background()
	createVolume(t, cur)
	publish(t, cur, exclNode1)

	gate := make(chan struct{})
	if afterDeny {
		staleC.gates.afterDeny = gate
	} else {
		staleC.gates.deny = gate
	}
	wait := runStale(func() error {
		_, err := stale.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
			VolumeId: fenceVolumeID(), NodeId: exclNode1,
		})
		return err
	})
	waitHeld(t, &staleC.gates) // record marked revoking, fence committed

	_, err := cur.ControllerPublishVolume(ctx, exclPublishReq(fenceVolumeID(), exclNode1, snw, false))
	if status.Code(err) != codes.Aborted {
		t.Errorf("re-publish during revoke: %v, want Aborted", err)
	}
	close(gate)
	if err := wait(); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if hosts := ag.allowedHosts(t); len(hosts) != 0 {
		t.Errorf("hosts %v after unpublish, want none", hosts)
	}
	if pubs := exclPublishedNodes(t, env.srv.k8sClient, fenceVolumeID()); len(pubs) != 0 {
		t.Errorf("publishedNodes %+v after unpublish, want none", pubs)
	}
	publish(t, cur, exclNode1)
}

// A delete that committed the deleting flag makes a concurrent publish fail
// instead of racing it, and the delete completes.
func TestFencing_PublishDuringDeleteIsRejected(t *testing.T) {
	t.Parallel()
	env := newPublishTestEnv(t, exclCSINodes()...)
	ag := newFenceAgent(t)
	staleC, curC := newFencePair(ag)
	staleC.gates.deleteVol = make(chan struct{})
	stale, cur := fenceServer(env, staleC), fenceServer(env, curC)
	createVolume(t, cur)

	wait := runStale(func() error {
		_, err := stale.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: fenceVolumeID()})
		return err
	})
	waitHeld(t, &staleC.gates)
	_, err := cur.ControllerPublishVolume(context.Background(), exclPublishReq(fenceVolumeID(), exclNode1, snw, false))
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("publish during delete: %v, want FailedPrecondition", err)
	}
	close(staleC.gates.deleteVol)
	if err := wait(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ag.backend.hasTestVolume() || len(ag.allowedHosts(t)) != 0 {
		t.Errorf("after delete: backend=%t hosts=%v, want neither", ag.backend.hasTestVolume(), ag.allowedHosts(t))
	}
}

// The capacity the backend actually allocated is durable: the first response,
// a cached retry, and a retry after a failed export all report it.
func TestCreateVolume_ReportsAllocatedCapacity(t *testing.T) {
	t.Parallel()
	t.Run("cached retry", func(t *testing.T) {
		t.Parallel()
		checkAllocatedCapacity(t, false)
	})
	t.Run("retry after failed export", func(t *testing.T) {
		t.Parallel()
		checkAllocatedCapacity(t, true)
	})
}

func checkAllocatedCapacity(t *testing.T, failFirstExport bool) {
	t.Helper()
	const roundUp = 4096
	want := baseCreateVolumeRequest().GetCapacityRange().GetRequiredBytes() + roundUp
	env := newPublishTestEnv(t)
	ag := newFenceAgent(t)
	ag.backend.roundUp = roundUp
	_, c := newFencePair(ag)
	srv := fenceServer(env, c)
	ctx := context.Background()
	if failFirstExport {
		c.exportFailures = 1
		if _, err := srv.CreateVolume(ctx, baseCreateVolumeRequest()); status.Code(err) != codes.Unavailable {
			t.Fatalf("first CreateVolume: %v, want the injected Unavailable", err)
		}
	}
	for attempt := range 2 {
		resp, err := srv.CreateVolume(ctx, baseCreateVolumeRequest())
		if err != nil {
			t.Fatalf("CreateVolume attempt %d: %v", attempt, err)
		}
		if got := resp.GetVolume().GetCapacityBytes(); got != want {
			t.Errorf("attempt %d capacity %d, want allocated %d", attempt, got, want)
		}
	}
}
