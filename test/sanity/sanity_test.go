//go:build csi_sanity
// +build csi_sanity

package sanity

// sanity_test.go — runs the upstream kubernetes-csi/csi-test sanity suite
// against an in-process pillar-csi driver wired to a fake AgentService.
//
// The test brings up two gRPC endpoints:
//
//	* an in-process bufconn server hosting the fake AgentService that the
//	  ControllerServer dials to satisfy CreateVolume/Export/Allow/etc.
//	* a Unix-socket gRPC server hosting Identity + Controller + Node services
//	  that csi-sanity drives over its CSI client.
//
// Shared in-memory state — pillarcsi VolumeStateMachine, fake K8s client
// containing the PillarAgent — is plumbed so the state transitions between
// ControllerPublishVolume and NodeStageVolume succeed.

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"

	pillarv1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

const (
	driverName    = "pillar-csi.bhyoo.com"
	driverVersion = "0.0.0-sanity"
	bufconnSize   = 1 << 20
	targetName    = "sanity-target"
)

// storageClassParams mirrors the StorageClass.parameters block that the
// in-cluster external-provisioner forwards to CreateVolume.  The controller
// requires every key.
var storageClassParams = map[string]string{
	"pillar-csi.bhyoo.com/agent":         targetName,
	"pillar-csi.bhyoo.com/store":         "tank",
	"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
	"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
}

// TestCSISanity exercises the entire csi-sanity battery against the in-process
// driver.  A passing run proves the driver's gRPC surface matches the CSI
// specification for every advertised capability.
func TestCSISanity(t *testing.T) {
	workDir := t.TempDir()
	csiSock := filepath.Join(workDir, "csi.sock")

	agentSrv, agentClient, agentLis := startFakeAgentBufconn(t)
	t.Cleanup(func() {
		agentSrv.Stop()
		_ = agentLis.Close()
	})

	identity, controller, node := buildDriver(t, agentClient)
	stopCSI := startCSIEndpoint(t, csiSock, identity, controller, node)
	t.Cleanup(stopCSI)

	cfg := sanity.NewTestConfig()
	cfg.Address = "unix://" + csiSock
	cfg.TargetPath = filepath.Join(workDir, "target")
	cfg.StagingPath = filepath.Join(workDir, "staging")
	cfg.TestVolumeSize = 1 << 30 // 1 GiB
	cfg.TestVolumeParameters = storageClassParams
	cfg.IdempotentCount = 2

	// csi-sanity defaults to os.Mkdir / os.Remove, which fail when the
	// directories already exist or contain entries created by previous
	// specs.  Override with idempotent MkdirAll / RemoveAll so the suite
	// can run all 92 specs in one go.
	cfg.CreateTargetDir = func(path string) (string, error) {
		return path, os.MkdirAll(path, 0o755)
	}
	cfg.CreateStagingDir = func(path string) (string, error) {
		return path, os.MkdirAll(path, 0o755)
	}
	cfg.RemoveTargetPath = func(path string) error {
		return os.RemoveAll(path)
	}
	cfg.RemoveStagingPath = func(path string) error {
		return os.RemoveAll(path)
	}

	sanity.Test(t, cfg)
}

// startFakeAgentBufconn brings up the fake AgentService on an in-memory
// bufconn listener and returns a connected client.
func startFakeAgentBufconn(t *testing.T) (*grpc.Server, agentv1.AgentServiceClient, *bufconn.Listener) {
	t.Helper()

	lis := bufconn.Listen(bufconnSize)
	srv := grpc.NewServer()
	registerFakeAgent(srv)

	go func() {
		_ = srv.Serve(lis)
	}()

	dialOpt := grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return lis.Dial()
	})
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		dialOpt,
	)
	if err != nil {
		srv.Stop()
		_ = lis.Close()
		t.Fatalf("dial fake agent bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return srv, agentv1.NewAgentServiceClient(conn), lis
}

// buildDriver constructs Identity, Controller and Node servers that share an
// in-memory PillarAgent and VolumeStateMachine.
func buildDriver(
	t *testing.T,
	agentClient agentv1.AgentServiceClient,
) (*csidrv.IdentityServer, *csidrv.ControllerServer, *csidrv.NodeServer) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := pillarv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register pillarv1 scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register corev1 scheme: %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register storagev1 scheme: %v", err)
	}

	target := &pillarv1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: targetName},
		Status: pillarv1.PillarAgentStatus{
			ResolvedAddress: "passthrough:///bufnet",
		},
	}
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sanity-node",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn":     "nqn.2026-01.com.bhyoo.pillar-csi:host.sanity",
				"pillar-csi.bhyoo.com/iscsi-initiator-iqn": "iqn.2026-01.com.bhyoo.pillar-csi:host.sanity",
			},
		},
		Spec: storagev1.CSINodeSpec{
			Drivers: []storagev1.CSINodeDriver{{
				Name:   driverName,
				NodeID: "sanity-node",
			}},
		},
	}
	k8sClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&pillarv1.PillarAgent{}, &pillarv1.PillarVolumeState{}).
		WithObjects(target, csiNode).
		Build()

	identity := csidrv.NewIdentityServer(driverName, driverVersion)
	controller := csidrv.NewControllerServerWithDialer(
		k8sClient,
		driverName,
		func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
			return agentClient, io.NopCloser(strings.NewReader("")), nil
		},
	)

	connector := &fakeConnector{devicePath: filepath.Join(t.TempDir(), "fake-block")}
	mounter := newFakeMounter()
	node := csidrv.NewNodeServerWithStateMachine(
		"sanity-node",
		connector,
		mounter,
		t.TempDir(),
		controller.GetStateMachine(),
	).WithResizer(fakeResizer{})

	return identity, controller, node
}

// startCSIEndpoint listens on the given Unix socket and serves Identity,
// Controller and Node on a single gRPC server, mirroring how production
// pillar-csi sidecars dial the driver.
func startCSIEndpoint(
	t *testing.T,
	socketPath string,
	identity *csidrv.IdentityServer,
	controller *csidrv.ControllerServer,
	node *csidrv.NodeServer,
) func() {
	t.Helper()

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix %q: %v", socketPath, err)
	}

	srv := grpc.NewServer()
	csispec.RegisterIdentityServer(srv, identity)
	csispec.RegisterControllerServer(srv, controller)
	csispec.RegisterNodeServer(srv, node)

	go func() {
		_ = srv.Serve(lis)
	}()

	return func() {
		srv.Stop()
		_ = lis.Close()
	}
}
