package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agentclient"
	csisvc "github.com/isac322/pillar-csi/internal/csi"
)

// TestCSIGRPCServer_DoesNotNeedLeaderElection is the regression guard for
// issue #96: the CSI runnable must stay out of the manager's leader-election
// group or standby replicas never bind the socket and their sidecars
// crash-loop.  On the code that caused the bug this test does not compile,
// because csiGRPCServer had no NeedLeaderElection method.
func TestCSIGRPCServer_DoesNotNeedLeaderElection(t *testing.T) {
	if (&csiGRPCServer{}).NeedLeaderElection() {
		t.Fatal("csiGRPCServer must serve on every replica; " +
			"leader-electing it leaves standby pods without a CSI socket")
	}
}

// TestCSIGRPCServer_ServesProbeOnSocket proves the runnable path end-to-end
// without a manager: Start binds the unix socket, LoadState runs, and a real
// CSI client Probe over the socket reports Ready.  This mirrors what the
// liveness-probe sidecar and ProbeForever do on every replica.
func TestCSIGRPCServer_ServesProbeOnSocket(t *testing.T) {
	for _, selectedDriver := range []string{
		pillarcsiv1alpha1.DefaultCSIDriver,
		pillarcsiv1alpha1.FileCSIDriver,
	} {
		t.Run(selectedDriver, func(t *testing.T) {
			testCSIGRPCServerSocket(t, selectedDriver)
		})
	}
}

type csiGRPCServerTestFixture struct {
	server     *csiGRPCServer
	conn       *grpc.ClientConn
	identity   csispec.IdentityClient
	controller csispec.ControllerClient
	ctx        context.Context
	cancel     context.CancelFunc
	errCh      chan error
	legacyID   string
	filesID    string
}

func newCSIGRPCServerTestFixture(t *testing.T, selectedDriver string) csiGRPCServerTestFixture {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "csi.sock")
	legacyID := "agent/nfs/zfs-dataset/tank/dynamic"
	adoption := &pillarcsiv1alpha1.FilesystemAdoption{
		Kind:            pillarcsiv1alpha1.FilesystemAdoptionKindZFSDataset,
		CanonicalSource: "tank/existing", ResourceID: "42",
		HostPath: "/mnt/tank/existing", FilesystemType: "zfs",
	}
	fenceID := backend.FilesystemFenceID(&agentv1.FilesystemAdoption{
		Kind: string(adoption.Kind), CanonicalSource: adoption.CanonicalSource,
		ResourceId: adoption.ResourceID, HostPath: adoption.HostPath, FilesystemType: adoption.FilesystemType,
	})
	agentVolumeID := "tank/fs-" + strings.TrimPrefix(fenceID, "filesystem/")
	filesID := "agent/nfs/zfs-dataset/" + agentVolumeID + ".aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	legacyMeta := metav1.ObjectMeta{Name: "dynamic", UID: types.UID("legacy-uid")}
	filesMeta := metav1.ObjectMeta{Name: "adopted", UID: types.UID("files-uid")}
	legacy := &pillarcsiv1alpha1.PillarVolumeState{
		ObjectMeta: legacyMeta,
		Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
			VolumeID: legacyID, AgentVolumeID: "tank/dynamic", AgentRef: "agent",
			ProtocolType: "nfs", BackendType: "zfs-dataset", CapacityBytes: 1 << 30,
		},
		Status: pillarcsiv1alpha1.PillarVolumeStateStatus{Phase: pillarcsiv1alpha1.PillarVolumeStatePhaseReady},
	}
	files := &pillarcsiv1alpha1.PillarVolumeState{
		ObjectMeta: filesMeta,
		Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
			VolumeID: filesID, AgentVolumeID: agentVolumeID, AgentRef: "agent",
			ProtocolType: "nfs", BackendType: "zfs-dataset", CapacityBytes: 1 << 30,
			FilesystemAdoption: adoption,
			Resolved: &pillarcsiv1alpha1.ResolvedVolumeConfig{
				Backend: pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
					Pool: "tank", VolumeType: pillarcsiv1alpha1.ZFSVolumeTypeDataset,
				}},
				Protocol: pillarcsiv1alpha1.ProtocolSpec{NFS: &pillarcsiv1alpha1.NFSConfig{}},
			},
		},
		Status: pillarcsiv1alpha1.PillarVolumeStateStatus{
			Phase: pillarcsiv1alpha1.PillarVolumeStatePhaseReady, ImportAcquired: true,
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(legacy, files).
		WithObjects(legacy, files).
		Build()
	agentDialer := agentclient.NewManager()
	t.Cleanup(func() {
		if closeErr := agentDialer.Close(); closeErr != nil {
			t.Errorf("close agent gRPC connection manager: %v", closeErr)
		}
	})
	servers, frontend, err := newScopedControllerServers(
		fakeClient, fakeClient, agentDialer, "pillar-csi", selectedDriver,
	)
	if err != nil {
		t.Fatalf("newScopedControllerServers: %v", err)
	}
	server := &csiGRPCServer{
		endpoint: "unix://" + sockPath, grpcSrv: grpc.NewServer(), ctrlServers: servers,
	}
	identitySrv := csisvc.NewIdentityServerWithReadyFn(selectedDriver, "test", server.probeReady)
	csisvc.RegisterGRPC(server.grpcSrv, identitySrv, frontend)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- server.Start(ctx) }()
	conn, err := grpc.NewClient("unix://"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return csiGRPCServerTestFixture{
		server: server, conn: conn,
		identity:   csispec.NewIdentityClient(conn),
		controller: csispec.NewControllerClient(conn),
		ctx:        ctx, cancel: cancel, errCh: errCh,
		legacyID: legacyID, filesID: filesID,
	}
}

func testCSIGRPCServerSocket(t *testing.T, selectedDriver string) {
	t.Helper()
	fixture := newCSIGRPCServerTestFixture(t, selectedDriver)
	defer fixture.cancel()
	defer func() {
		if closeErr := fixture.conn.Close(); closeErr != nil {
			t.Errorf("close CSI gRPC client connection: %v", closeErr)
		}
	}()
	waitForCSIProbe(t, fixture)
	assertCSIFrontendCapabilities(t, fixture, selectedDriver)
	assertCSIDriverRouting(t, fixture, selectedDriver)
	fixture.cancel()
	if err := <-fixture.errCh; err != nil {
		t.Fatalf("CSI server returned error on shutdown: %v", err)
	}
	ready, readyErr := fixture.server.probeReady(context.Background())
	if readyErr != nil || ready {
		t.Fatalf("probeReady after shutdown = (%v, %v), want (false, nil)", ready, readyErr)
	}
}

func waitForCSIProbe(t *testing.T, fixture csiGRPCServerTestFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, probeErr := fixture.identity.Probe(fixture.ctx, &csispec.ProbeRequest{})
		if probeErr == nil && resp.GetReady().GetValue() {
			return
		}
		select {
		case serveErr := <-fixture.errCh:
			t.Fatalf("CSI server exited before serving: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Probe never reported ready (last err: %v)", probeErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertCSIFrontendCapabilities(t *testing.T, fixture csiGRPCServerTestFixture, selectedDriver string) {
	t.Helper()
	info, err := fixture.identity.GetPluginInfo(fixture.ctx, &csispec.GetPluginInfoRequest{})
	if err != nil || info.GetName() != selectedDriver {
		t.Fatalf("served plugin identity = %v, error=%v, want %q", info, err, selectedDriver)
	}
	capabilities, err := fixture.controller.ControllerGetCapabilities(
		fixture.ctx, &csispec.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("ControllerGetCapabilities: %v", err)
	}
	expansion := false
	for _, capability := range capabilities.GetCapabilities() {
		expansion = expansion || capability.GetRpc().GetType() == csispec.ControllerServiceCapability_RPC_EXPAND_VOLUME
	}
	if expansion != (selectedDriver == pillarcsiv1alpha1.DefaultCSIDriver) {
		t.Fatalf("selected %q frontend expansion=%v", selectedDriver, expansion)
	}
}

func assertCSIDriverRouting(t *testing.T, fixture csiGRPCServerTestFixture, selectedDriver string) {
	t.Helper()
	ownID, otherID := fixture.legacyID, fixture.filesID
	if selectedDriver == pillarcsiv1alpha1.FileCSIDriver {
		ownID, otherID = fixture.filesID, fixture.legacyID
	}
	_, err := fixture.controller.ControllerUnpublishVolume(
		fixture.ctx,
		&csispec.ControllerUnpublishVolumeRequest{VolumeId: ownID, NodeId: "node"},
	)
	if err != nil {
		t.Fatalf("unpublishing owned, already-unpublished volume: %v", err)
	}
	_, err = fixture.controller.ControllerUnpublishVolume(
		fixture.ctx,
		&csispec.ControllerUnpublishVolumeRequest{VolumeId: otherID, NodeId: "node"},
	)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("wrong-driver handle error=%v, want NotFound", err)
	}
	for i, server := range fixture.server.ctrlServers {
		owned, foreign := fixture.legacyID, fixture.filesID
		if i == 1 {
			owned, foreign = fixture.filesID, fixture.legacyID
		}
		if server.GetStateMachine().GetState(owned) != csisvc.StateCreated ||
			server.GetStateMachine().GetState(foreign) != csisvc.StateNonExistent {
			t.Fatalf("internal driver %d restored wrong lifecycle scope: %v", i, server.GetStateMachine().AllStates())
		}
	}
}
