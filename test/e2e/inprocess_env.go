package e2e

// inprocess_env.go — Isolated test environment factories for in-process TCs.
//
// controllerTestEnv creates a fresh, isolated ControllerServer backed by a
// fakeAgentServer registered with a real gRPC server (bufconn transport).
//
// nodeTestEnv creates a fresh, isolated NodeServer with fake connector/mounter.
//
// agentTestEnv creates a real agentsvc.Server backed by real ZFS/LVM backends
// executing inside the Kind cluster container via docker exec, exposed via
// bufconn for E9 and E28 TCs.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	pillarv1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	agentsvc "github.com/bhyoo/pillar-csi/internal/agent"
	agentbackend "github.com/bhyoo/pillar-csi/internal/agent/backend"
	lvmb "github.com/bhyoo/pillar-csi/internal/agent/backend/lvm"
	zfsb "github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
	nvmeof "github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	inprocessBufSize = 1 << 20 // 1 MiB
)

// ─────────────────────────────────────────────────────────────────────────────
// controllerTestEnv
// ─────────────────────────────────────────────────────────────────────────────

// controllerTestEnv is an isolated test environment for CSI controller TCs.
// It creates:
//   - A fakeAgentServer registered with a real gRPC server (bufconn transport)
//   - A fake K8s client with pre-registered PillarTarget/PillarVolume/PVC objects
//   - A CSI ControllerServer dialing the bufconn gRPC server
type controllerTestEnv struct {
	ctx        context.Context
	cancel     context.CancelFunc
	controller *csidrv.ControllerServer
	agentSrv   *fakeAgentServer // controllable fake agent
	k8sClient  client.Client
	target     *pillarv1.PillarTarget
	params     map[string]string // default StorageClass params
	lis        *bufconn.Listener
	grpcSrv    *grpc.Server
	agentConn  *grpc.ClientConn
}

func newControllerTestEnv() *controllerTestEnv {
	ctx, cancel := context.WithCancel(context.Background())

	scheme := runtime.NewScheme()
	if err := pillarv1.AddToScheme(scheme); err != nil {
		cancel()
		panic(fmt.Sprintf("register pillar scheme: %v", err))
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		cancel()
		panic(fmt.Sprintf("register corev1 scheme: %v", err))
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		cancel()
		panic(fmt.Sprintf("register storagev1 scheme: %v", err))
	}

	target := &pillarv1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-1"},
		Status: pillarv1.PillarTargetStatus{
			ResolvedAddress: "passthrough:///bufnet",
		},
	}

	k8sClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&pillarv1.PillarTarget{}, &pillarv1.PillarVolume{}).
		WithObjects(target).
		Build()

	agentSrv := newFakeAgentServer()
	lis := bufconn.Listen(inprocessBufSize)
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, agentSrv)
	go func() { _ = grpcSrv.Serve(lis) }()

	dialOption := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		dialOption,
	)
	if err != nil {
		grpcSrv.Stop()
		_ = lis.Close()
		cancel()
		panic(fmt.Sprintf("newControllerTestEnv: dial bufconn: %v", err))
	}

	agentClient := agentv1.NewAgentServiceClient(conn)

	controller := csidrv.NewControllerServerWithDialer(
		k8sClient,
		"pillar-csi.bhyoo.com",
		func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
			return agentClient, io.NopCloser(strings.NewReader("")), nil
		},
	)

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}

	return &controllerTestEnv{
		ctx:        ctx,
		cancel:     cancel,
		controller: controller,
		agentSrv:   agentSrv,
		k8sClient:  k8sClient,
		target:     target,
		params:     params,
		lis:        lis,
		grpcSrv:    grpcSrv,
		agentConn:  conn,
	}
}

func (e *controllerTestEnv) close() {
	e.cancel()
	e.grpcSrv.Stop()
	_ = e.agentConn.Close()
	_ = e.lis.Close()
}

// createVolume is a helper that calls CreateVolume with the environment's
// default params and returns the volume ID or panics the test.
func (e *controllerTestEnv) createVolume(name string, caps []*csiapi.VolumeCapability) (string, error) {
	resp, err := e.controller.CreateVolume(e.ctx, &csiapi.CreateVolumeRequest{
		Name:               name,
		Parameters:         e.params,
		VolumeCapabilities: caps,
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	if err != nil {
		return "", err
	}
	return resp.GetVolume().GetVolumeId(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// nodeTestEnv
// ─────────────────────────────────────────────────────────────────────────────

// nodeTestEnv is an isolated test environment for CSI node TCs.
type nodeTestEnv struct {
	ctx       context.Context
	cancel    context.CancelFunc
	node      *csidrv.NodeServer
	connector *localMockConnector
	mounter   *localMockMounter
	resizer   *localMockResizer
	stateDir  string
	sm        *csidrv.VolumeStateMachine
}

func newNodeTestEnv() *nodeTestEnv {
	ctx, cancel := context.WithCancel(context.Background())

	stateDir, err := os.MkdirTemp(tcTempRoot, "pillar-csi-node-test-*")
	if err != nil {
		cancel()
		panic(fmt.Sprintf("newNodeTestEnv: create temp dir: %v", err))
	}

	connector := &localMockConnector{devicePath: "/dev/nvme0n1"}
	mounter := newLocalMockMounter()
	resizer := &localMockResizer{}
	sm := csidrv.NewVolumeStateMachine()

	node := csidrv.NewNodeServerWithStateMachine("node-local", connector, mounter, stateDir, sm).
		WithResizer(resizer)

	return &nodeTestEnv{
		ctx:       ctx,
		cancel:    cancel,
		node:      node,
		connector: connector,
		mounter:   mounter,
		resizer:   resizer,
		stateDir:  stateDir,
		sm:        sm,
	}
}

func (e *nodeTestEnv) close() {
	e.cancel()
	_ = os.RemoveAll(e.stateDir)
}

// ─────────────────────────────────────────────────────────────────────────────
// agentTestEnv
// ─────────────────────────────────────────────────────────────────────────────

// agentTestEnv is for direct agent gRPC tests (E9, E28).
// Uses the REAL agentsvc.Server with REAL ZFS/LVM backends inside the Kind cluster.
// Commands execute via "docker exec <container>" to reach real ZFS/LVM in the Kind node.
type agentTestEnv struct {
	ctx    context.Context
	cancel context.CancelFunc
	server *agentsvc.Server // real agent server
	client agentv1.AgentServiceClient
	// Real backend info (read from suite env vars).
	zfsPool      string
	lvmVG        string
	container    string
	configfsRoot string
	lis          *bufconn.Listener
	grpcSrv      *grpc.Server
	agentConn    *grpc.ClientConn
}

func newAgentTestEnv() *agentTestEnv {
	container, zfsPool, lvmVG := requireSuiteBackendEnv()
	lvmThinPool := os.Getenv(suiteLVMThinPoolEnvVar)
	return newAgentTestEnvWithBackends(container, zfsPool, lvmVG, lvmThinPool)
}

// newLinearAgentTestEnv creates an agentTestEnv with a LINEAR LVM backend
// (no thin pool configured). Use this for TCs that need to verify failure
// behaviour when thin provisioning is requested without a configured pool
// (TC-E28.259) or when capacity-exhaustion rejection is needed with real
// linear LVM semantics (TC-E28.263i, TC-E28.263j).
func newLinearAgentTestEnv() *agentTestEnv {
	container, zfsPool, lvmVG := requireSuiteBackendEnv()
	return newAgentTestEnvWithBackends(container, zfsPool, lvmVG, "") // "" = no thin pool = linear
}

func newAgentTestEnvWithBackends(container, zfsPool, lvmVG, lvmThinPool string) *agentTestEnv {
	ctx, cancel := context.WithCancel(context.Background())

	configfsRoot, err := os.MkdirTemp(tcTempRoot, "pillar-csi-configfs-*")
	if err != nil {
		cancel()
		panic(fmt.Sprintf("newAgentTestEnv: create configfs dir: %v", err))
	}

	// Pre-create the nvmet directory that health.checkNvmetConfigfs expects.
	// Without this, HealthCheck always returns healthy=false because
	// os.Stat(configfsRoot+"/nvmet") fails.
	if err := os.MkdirAll(filepath.Join(configfsRoot, "nvmet"), 0755); err != nil {
		_ = os.RemoveAll(configfsRoot)
		cancel()
		panic(fmt.Sprintf("newAgentTestEnv: create nvmet dir: %v", err))
	}

	execFn := realContainerExecFn(container)

	// Ensure the ZFS namespace parent dataset exists inside the container.
	// The ZFS backend (namespace "k8s") creates zvols at pool/k8s/<volname>;
	// the parent dataset pool/k8s must exist before any zvol can be created.
	if _, zfsCreateErr := execFn(ctx, "zfs", "create", "-p", zfsPool+"/k8s"); zfsCreateErr != nil {
		// Tolerate "already exists" — parent may have been created by a prior
		// test run or concurrent worker.
		if !strings.Contains(zfsCreateErr.Error(), "already exists") &&
			!strings.Contains(zfsCreateErr.Error(), "dataset already exists") {
			_ = os.RemoveAll(configfsRoot)
			cancel()
			panic(fmt.Sprintf("newAgentTestEnv: create ZFS namespace parent %s/k8s: %v", zfsPool, zfsCreateErr))
		}
	}

	zfsBackend := zfsb.NewWithExecFn(zfsPool, "k8s", execFn)
	// lvmThinPool: "" = linear backend (no over-provisioning), non-empty = thin pool.
	lvmBackend := lvmb.NewWithExecFn(lvmVG, lvmThinPool, execFn)

	backends := map[string]agentbackend.VolumeBackend{
		zfsPool: zfsBackend,
		lvmVG:   lvmBackend,
	}

	server := agentsvc.NewServer(
		backends,
		configfsRoot,
		agentsvc.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	lis := bufconn.Listen(inprocessBufSize)
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, server)
	go func() { _ = grpcSrv.Serve(lis) }()

	dialOption := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		dialOption,
	)
	if err != nil {
		grpcSrv.Stop()
		_ = lis.Close()
		_ = os.RemoveAll(configfsRoot)
		cancel()
		panic(fmt.Sprintf("newAgentTestEnv: dial bufconn: %v", err))
	}

	return &agentTestEnv{
		ctx:          ctx,
		cancel:       cancel,
		server:       server,
		client:       agentv1.NewAgentServiceClient(conn),
		zfsPool:      zfsPool,
		lvmVG:        lvmVG,
		container:    container,
		configfsRoot: configfsRoot,
		lis:          lis,
		grpcSrv:      grpcSrv,
		agentConn:    conn,
	}
}

func (e *agentTestEnv) close() {
	e.cancel()
	e.grpcSrv.Stop()
	_ = e.agentConn.Close()
	_ = e.lis.Close()
	_ = os.RemoveAll(e.configfsRoot)
}
