package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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
	agentclientpkg "github.com/bhyoo/pillar-csi/internal/agentclient"
	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
	"github.com/bhyoo/pillar-csi/internal/testutil/testcerts"
	"github.com/bhyoo/pillar-csi/internal/tlscreds"
	webhookv1alpha1 "github.com/bhyoo/pillar-csi/internal/webhook/v1alpha1"
)

type localVerifierName string

const (
	localVerifierController localVerifierName = "controller-local"
	localVerifierNode       localVerifierName = "node-local"
	localVerifierAgent      localVerifierName = "agent-local"
	localVerifierMTLS       localVerifierName = "mtls-local"
	localVerifierCRD        localVerifierName = "crd-contracts-local"
	localVerifierKind       localVerifierName = "kind-bootstrap-local"
	localVerifierLVM        localVerifierName = "lvm-local"
	localVerifierZFS        localVerifierName = "zfs-local"
	// localVerifierHelm verifies that the Helm chart tree at charts/pillar-csi/
	// is structurally correct (valid Chart.yaml, values.yaml, required templates).
	// This is a fast in-process check that does not require a running cluster.
	// Real cluster validation lives in tc_e27_helm_e2e_test.go (//go:build e2e).
	localVerifierHelm localVerifierName = "helm-chart-local"
)

type localExecutionPlan struct {
	Summary   string
	Verifiers []localVerifierName
}

type localVerifierResult struct {
	Name     localVerifierName
	Duration time.Duration
	Err      error
}

func (r localVerifierResult) failureMessage() string {
	if r.Err == nil {
		return ""
	}
	return fmt.Sprintf("%s failed after %s: %v", r.Name, r.Duration, r.Err)
}

type localVerifierEntry struct {
	once   sync.Once
	fn     func() error
	result localVerifierResult
}

type localVerifierRegistry struct {
	entries map[localVerifierName]*localVerifierEntry
}

func newLocalVerifierRegistry() *localVerifierRegistry {
	return newLocalVerifierRegistryForProcess(0)
}

// newLocalVerifierRegistryForProcess creates a verifier registry where
// verifiers that allocate shared backend resources use worker-specific names
// derived from processNum to avoid collisions when multiple Ginkgo workers
// run their SynchronizedBeforeSuite all-nodes phase concurrently.
func newLocalVerifierRegistryForProcess(processNum int) *localVerifierRegistry {
	return &localVerifierRegistry{
		entries: map[localVerifierName]*localVerifierEntry{
			localVerifierController: {fn: verifyControllerLocalBackend},
			localVerifierNode:       {fn: verifyNodeLocalBackend},
			// Use process-specific volume name to prevent "dataset already exists"
			// when 4 parallel workers all call verifyAgentLocalBackend at the same time.
			localVerifierAgent: {fn: func() error {
				return verifyAgentLocalBackendForProcess(processNum)
			}},
			localVerifierMTLS: {fn: verifyMTLSLocalBackend},
			localVerifierCRD:  {fn: verifyCRDLocalContracts},
			localVerifierKind: {fn: verifyKindBootstrapLocalContracts},
			localVerifierLVM:  {fn: verifyLVMLocalBackend},
			localVerifierZFS:  {fn: verifyZFSLocalBackend},
			localVerifierHelm: {fn: verifyHelmChartLocalContracts},
		},
	}
}

func (r *localVerifierRegistry) Result(name localVerifierName) localVerifierResult {
	entry, ok := r.entries[name]
	if !ok {
		return localVerifierResult{
			Name: name,
			Err:  fmt.Errorf("unknown local verifier %q", name),
		}
	}

	entry.once.Do(func() {
		started := time.Now()
		entry.result = localVerifierResult{Name: name}
		defer func() {
			if recovered := recover(); recovered != nil {
				entry.result.Err = fmt.Errorf("panic: %v", recovered)
			}
			entry.result.Duration = time.Since(started)
		}()
		entry.result.Err = entry.fn()
	})

	return entry.result
}

func (r *localVerifierRegistry) Has(name localVerifierName) bool {
	_, ok := r.entries[name]
	return ok
}

// suiteLocalVerifierRegistry holds the per-suite in-process verifier registry.
// It is nil until warmUpLocalBackend() is called during SynchronizedBeforeSuite,
// ensuring there is no package-level singleton initialised at import time.
// Each Ginkgo parallel worker (OS process) creates its own registry instance
// via warmUpLocalBackend(), so there is no shared mutable state across workers.
var suiteLocalVerifierRegistry *localVerifierRegistry

// allLocalVerifierNames enumerates every registered in-process verifier in the
// order they should be pre-warmed during SynchronizedBeforeSuite.
//
// The order is chosen so that cheap verifiers (controller, node, CRD) run before
// the heavier ones (agent gRPC server, mTLS, ZFS/LVM) so that any early failure
// is diagnosed quickly without blocking on slower initialisation.
//
// localVerifierHelm is included so that E27 "cluster" specs in the default
// profile (which call suiteLocalVerifierRegistry.Result(localVerifierHelm))
// always find a cached result rather than paying the first-call overhead
// mid-spec.  verifyHelmChartLocalContracts is a pure filesystem check and
// completes in <1 ms, so pre-warming it is free.
var allLocalVerifierNames = []localVerifierName{
	localVerifierController,
	localVerifierNode,
	localVerifierCRD,
	localVerifierKind,
	localVerifierAgent,
	localVerifierMTLS,
	localVerifierLVM,
	localVerifierZFS,
	localVerifierHelm,
}

// warmUpLocalBackend creates the per-suite verifier registry (if not yet
// created) and eagerly initialises every in-process verifier on the current
// Ginkgo node by calling suiteLocalVerifierRegistry.Result for each known
// verifier name.
//
// Each verifier uses sync.Once internally so it runs at most once per OS
// process. Calling warmUpLocalBackend in the SynchronizedBeforeSuite all-nodes
// phase guarantees that all verifier results are ready before any It-node is
// scheduled on this node, providing two benefits:
//
//  1. Verifier failures are diagnosed during suite setup (fast-fail), not
//     mid-run while specs are executing.
//  2. Per-spec execution cost is reduced because no spec ever pays the
//     first-call initialisation overhead of a verifier.
//
// Verifier errors are intentionally NOT surfaced here — they are stored in the
// registry and checked by individual specs via runInProcessTCBody and friends.
// This allows a ZFS or LVM verifier failure to fail only the relevant specs
// rather than aborting the entire suite.
// warmUpLocalBackend creates and pre-warms the verifier registry for the
// given Ginkgo process node number. The processNum is used to assign unique
// resource names per worker, preventing "dataset already exists" collisions
// when multiple parallel workers warm up their verifiers concurrently.
//
// Pass GinkgoParallelProcess() (from Ginkgo test code) as processNum.
func warmUpLocalBackend(processNum int) {
	suiteLocalVerifierRegistry = newLocalVerifierRegistryForProcess(processNum)
	for _, name := range allLocalVerifierNames {
		_ = suiteLocalVerifierRegistry.Result(name)
	}
}

func resolveLocalExecutionPlan(tc documentedCase) (localExecutionPlan, error) {
	switch tc.GroupKey {
	case "E1", "E2", "E4", "E5", "E6", "E7", "E11", "E12", "E13", "E14", "E15", "E16", "E17", "E18", "E21", "E22", "E24", "E29", "E30":
		return localExecutionPlan{
			Summary:   "local CSI controller and volume lifecycle contracts",
			Verifiers: []localVerifierName{localVerifierController},
		}, nil
	case "E3":
		return localExecutionPlan{
			Summary:   "local CSI node staging, publishing, expansion, and cleanup contracts",
			Verifiers: []localVerifierName{localVerifierNode},
		}, nil
	case "E8":
		return localExecutionPlan{
			Summary:   "local mTLS transport contracts",
			Verifiers: []localVerifierName{localVerifierMTLS},
		}, nil
	case "E9", "E28":
		return localExecutionPlan{
			Summary:   "local agent gRPC, fake configfs, and export contracts",
			Verifiers: []localVerifierName{localVerifierAgent},
		}, nil
	case "E19", "E20", "E23", "E25", "E26", "E32":
		return localExecutionPlan{
			Summary:   "local CRD, webhook, compatibility, and manifest contracts",
			Verifiers: []localVerifierName{localVerifierCRD},
		}, nil
	case "E10":
		return localExecutionPlan{
			Summary:   "kind bootstrap and invocation-scoped lifecycle contracts",
			Verifiers: []localVerifierName{localVerifierKind},
		}, nil
	case "E27":
		return localExecutionPlan{
			Summary:   "helm chart structure and template rendering contracts",
			Verifiers: []localVerifierName{localVerifierHelm},
		}, nil
	// E33, E34, E35, F27–F31 are NOT catalog-driven.
	// Their Ginkgo specs live in dedicated *_e2e_test.go files and run under
	// the "default-profile" label filter directly — no dispatch through
	// resolveLocalExecutionPlan / runTCBody needed.
	default:
		return localExecutionPlan{}, fmt.Errorf("no local execution plan for group %q", tc.GroupKey)
	}
}

func joinLocalVerifierNames(names []localVerifierName) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, string(name))
	}
	return strings.Join(parts, ",")
}

func verifyControllerLocalBackend() error {
	ctx := context.Background()

	scheme := runtime.NewScheme()
	if err := pillarv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register pillar scheme: %w", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register corev1 scheme: %w", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register storagev1 scheme: %w", err)
	}

	target := &pillarv1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "target-local"},
		Status: pillarv1.PillarTargetStatus{
			ResolvedAddress: "passthrough:///bufnet",
		},
	}

	// CSINode is needed for ControllerPublishVolume to resolve initiator identity.
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-local",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.2026-01.io.example:node-local",
			},
		},
	}

	k8sClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&pillarv1.PillarTarget{}, &pillarv1.PillarVolume{}).
		WithObjects(target, csiNode).
		Build()

	// Use fakeAgentServer (controllable gRPC server) to test the CSI controller
	// protocol layer without requiring real storage backends.
	agentSrv := newFakeAgentServer()
	agentSrv.createVolumeResp = &agentv1.CreateVolumeResponse{
		DevicePath:    "/dev/test-device",
		CapacityBytes: 10 << 20,
	}
	agentSrv.exportVolumeResp = &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-local",
			Address:   "127.0.0.1",
			Port:      4420,
			VolumeRef: "tank/pvc-local",
		},
	}
	agentSrv.expandVolumeResp = &agentv1.ExpandVolumeResponse{
		CapacityBytes: 20 << 20,
	}
	agentSrv.getCapacityResp = &agentv1.GetCapacityResponse{
		TotalBytes:     100 << 30,
		AvailableBytes: 60 << 30,
		UsedBytes:      40 << 30,
	}

	lis := bufconn.Listen(inprocessBufSize)
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, agentSrv)
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.Stop()
	defer func() { _ = lis.Close() }()

	dialOption := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		dialOption,
	)
	if err != nil {
		return fmt.Errorf("verifyControllerLocalBackend: dial bufconn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	agentClient := agentv1.NewAgentServiceClient(conn)

	controller := csidrv.NewControllerServerWithDialer(
		k8sClient,
		"pillar-csi.bhyoo.com",
		func(context.Context, string) (agentv1.AgentServiceClient, io.Closer, error) {
			return agentClient, io.NopCloser(strings.NewReader("")), nil
		},
	)

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}

	createResp, err := controller.CreateVolume(ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-local",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	if err != nil {
		return fmt.Errorf("controller create volume: %w", err)
	}
	if createResp.GetVolume() == nil || createResp.GetVolume().GetVolumeId() == "" {
		return errors.New("controller create volume returned empty volume metadata")
	}
	if agentSrv.createVolumeCalls != 1 || agentSrv.exportVolumeCalls != 1 {
		return fmt.Errorf("controller create expected 1 create + 1 export agent call, got create=%d export=%d",
			agentSrv.createVolumeCalls, agentSrv.exportVolumeCalls)
	}

	if _, err := controller.ControllerPublishVolume(ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         createResp.GetVolume().GetVolumeId(),
		NodeId:           "node-local",
		VolumeCapability: mountCapability("ext4"),
	}); err != nil {
		return fmt.Errorf("controller publish volume: %w", err)
	}
	if agentSrv.allowInitiatorCalls != 1 {
		return fmt.Errorf("controller publish expected allow initiator call, got %d", agentSrv.allowInitiatorCalls)
	}

	expandResp, err := controller.ControllerExpandVolume(ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      createResp.GetVolume().GetVolumeId(),
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 20 << 20},
	})
	if err != nil {
		return fmt.Errorf("controller expand volume: %w", err)
	}
	if !expandResp.GetNodeExpansionRequired() || expandResp.GetCapacityBytes() != 20<<20 {
		return fmt.Errorf("controller expand returned capacity=%d nodeExpansionRequired=%v",
			expandResp.GetCapacityBytes(), expandResp.GetNodeExpansionRequired())
	}

	capResp, err := controller.GetCapacity(ctx, &csiapi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":       target.Name,
			"pillar-csi.bhyoo.com/pool":         "tank",
			"pillar-csi.bhyoo.com/backend-type": "zfs-zvol",
		},
	})
	if err != nil {
		return fmt.Errorf("controller get capacity: %w", err)
	}
	if capResp.GetAvailableCapacity() != 60<<30 {
		return fmt.Errorf("controller get capacity returned %d, want %d", capResp.GetAvailableCapacity(), 60<<30)
	}

	if _, err := controller.ControllerUnpublishVolume(ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
		NodeId:   "node-local",
	}); err != nil {
		return fmt.Errorf("controller unpublish volume: %w", err)
	}
	if agentSrv.denyInitiatorCalls != 1 {
		return fmt.Errorf("controller unpublish expected deny initiator call, got %d", agentSrv.denyInitiatorCalls)
	}

	if _, err := controller.DeleteVolume(ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	}); err != nil {
		return fmt.Errorf("controller delete volume: %w", err)
	}
	if agentSrv.unexportVolumeCalls != 1 || agentSrv.deleteVolumeCalls != 1 {
		return fmt.Errorf("controller delete expected 1 unexport + 1 delete agent call, got unexport=%d delete=%d",
			agentSrv.unexportVolumeCalls, agentSrv.deleteVolumeCalls)
	}

	_, err = controller.CreateVolume(ctx, &csiapi.CreateVolumeRequest{
		Name:       "pvc-invalid-access",
		Parameters: params,
		VolumeCapabilities: []*csiapi.VolumeCapability{{
			AccessType: &csiapi.VolumeCapability_Mount{
				Mount: &csiapi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csiapi.VolumeCapability_AccessMode{
				Mode: csiapi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		}},
	})
	if status.Code(err) != codes.InvalidArgument {
		return fmt.Errorf("controller invalid access mode error = %v, want InvalidArgument", status.Code(err))
	}

	return nil
}

func verifyNodeLocalBackend() error {
	ctx := context.Background()
	scope, err := NewTestCaseScope("verifier-node-local")
	if err != nil {
		return err
	}
	defer func() { _ = scope.Close() }()

	stateDir, err := scope.TempDir("state")
	if err != nil {
		return err
	}
	stagePath := scope.Path("stage", "volume")
	targetPath := scope.Path("target", "volume")

	connector := &localMockConnector{devicePath: "/dev/nvme0n1"}
	mounter := newLocalMockMounter()
	resizer := &localMockResizer{}
	sm := csidrv.NewVolumeStateMachine()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-local"
	sm.ForceState(volumeID, csidrv.StateControllerPublished)

	node := csidrv.NewNodeServerWithStateMachine("node-local", connector, mounter, stateDir, sm).
		WithResizer(resizer)

	if _, err := node.NodeStageVolume(ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext: map[string]string{
			csidrv.VolumeContextKeyTargetID: "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-local",
			csidrv.VolumeContextKeyAddress:  "127.0.0.1",
			csidrv.VolumeContextKeyPort:     "4420",
		},
	}); err != nil {
		return fmt.Errorf("node stage volume: %w", err)
	}
	if len(connector.connectCalls) != 1 || len(mounter.formatAndMountCalls) != 1 {
		return fmt.Errorf("node stage expected 1 connect + 1 format/mount call, got connect=%d formatAndMount=%d",
			len(connector.connectCalls), len(mounter.formatAndMountCalls))
	}

	if _, err := node.NodePublishVolume(ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	}); err != nil {
		return fmt.Errorf("node publish volume: %w", err)
	}
	if len(mounter.mountCalls) != 1 {
		return fmt.Errorf("node publish expected 1 bind mount call, got %d", len(mounter.mountCalls))
	}

	expandResp, err := node.NodeExpandVolume(ctx, &csiapi.NodeExpandVolumeRequest{
		VolumeId:         volumeID,
		VolumePath:       targetPath,
		VolumeCapability: mountCapability("ext4"),
		CapacityRange:    &csiapi.CapacityRange{RequiredBytes: 20 << 20},
	})
	if err != nil {
		return fmt.Errorf("node expand volume: %w", err)
	}
	if expandResp.GetCapacityBytes() != 20<<20 || len(resizer.calls) != 1 {
		return fmt.Errorf("node expand returned capacity=%d resizeCalls=%d", expandResp.GetCapacityBytes(), len(resizer.calls))
	}

	if _, err := node.NodeUnpublishVolume(ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}); err != nil {
		return fmt.Errorf("node unpublish volume: %w", err)
	}
	if len(mounter.unmountCalls) != 1 {
		return fmt.Errorf("node unpublish expected 1 unmount call, got %d", len(mounter.unmountCalls))
	}

	if _, err := node.NodeUnstageVolume(ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	}); err != nil {
		return fmt.Errorf("node unstage volume: %w", err)
	}
	if len(connector.disconnectCalls) != 1 {
		return fmt.Errorf("node unstage expected 1 disconnect call, got %d", len(connector.disconnectCalls))
	}

	return nil
}

func verifyAgentLocalBackend() error {
	return verifyAgentLocalBackendForProcess(0)
}

// verifyAgentLocalBackendForProcess runs the agent local backend verification
// using a worker-specific volume name (pvc-local-<processNum>) to prevent
// "dataset already exists" collisions when multiple Ginkgo parallel workers
// each call this verifier concurrently in SynchronizedBeforeSuite.
func verifyAgentLocalBackendForProcess(processNum int) error {
	ctx := context.Background()
	scope, err := NewTestCaseScope("verifier-agent-local")
	if err != nil {
		return err
	}
	defer func() { _ = scope.Close() }()

	configfsRoot, err := scope.TempDir("configfs")
	if err != nil {
		return err
	}

	// Unique volume name per worker avoids dataset collision during parallel warmup.
	volName := fmt.Sprintf("pvc-local-%d", processNum)

	container, zfsPool, _ := requireSuiteBackendEnv()
	execFn := realContainerExecFn(container)

	// Ensure the ZFS namespace parent dataset exists inside the container.
	if _, err := execFn(ctx, "zfs", "create", "-p", zfsPool+"/k8s"); err != nil {
		if !strings.Contains(err.Error(), "already exists") &&
			!strings.Contains(err.Error(), "dataset already exists") {
			return fmt.Errorf("verifyAgentLocalBackend: create ZFS parent dataset %s/k8s: %w", zfsPool, err)
		}
	}

	backend := zfsb.NewWithExecFn(zfsPool, "k8s", execFn)

	server := agentsvc.NewServer(
		map[string]agentbackend.VolumeBackend{zfsPool: backend},
		configfsRoot,
		agentsvc.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	volID := zfsPool + "/" + volName

	// Pre-cleanup: idempotently destroy any leftover zvol from a previous run
	// that may have exited before the DeleteVolume step completed.
	_, _ = server.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volID})

	if _, err := server.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      volID,
		CapacityBytes: 10 << 20, // 10 MiB — pool is 128 MiB, keep volumes small
	}); err != nil {
		return fmt.Errorf("agent create volume: %w", err)
	}

	devicePath := backend.DevicePath(volID)

	exportResp, err := server.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     volID,
		DevicePath:   devicePath,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	if err != nil {
		return fmt.Errorf("agent export volume: %w", err)
	}
	wantNQN := "nqn.2026-01.com.bhyoo.pillar-csi:" + zfsPool + "." + volName
	if exportResp.GetExportInfo().GetTargetId() != wantNQN {
		return fmt.Errorf("agent export target id = %q, want %q", exportResp.GetExportInfo().GetTargetId(), wantNQN)
	}

	if _, err := server.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     volID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-local",
	}); err != nil {
		return fmt.Errorf("agent allow initiator: %w", err)
	}

	hostLink := filepath.Join(
		configfsRoot,
		"nvmet",
		"subsystems",
		wantNQN,
		"allowed_hosts",
		"nqn.2026-01.io.example:host-local",
	)
	if _, err := os.Lstat(hostLink); err != nil {
		return fmt.Errorf("agent allow initiator did not create host link: %w", err)
	}

	if _, err := server.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     volID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-local",
	}); err != nil {
		return fmt.Errorf("agent deny initiator: %w", err)
	}
	if _, err := os.Lstat(hostLink); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agent deny initiator expected host link removal, got %v", err)
	}

	if _, err := server.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     volID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	}); err != nil {
		return fmt.Errorf("agent unexport volume: %w", err)
	}

	if _, err := server.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: volID,
	}); err != nil {
		return fmt.Errorf("agent delete volume: %w", err)
	}

	return nil
}

func verifyMTLSLocalBackend() error {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		return fmt.Errorf("generate test certs: %w", err)
	}

	serverCreds, err := tlscreds.NewServerCredentials(bundle.ServerCert, bundle.ServerKey, bundle.CACert)
	if err != nil {
		return fmt.Errorf("create server creds: %w", err)
	}
	clientCreds, err := tlscreds.NewClientCredentials(bundle.ClientCert, bundle.ClientKey, bundle.CACert, "127.0.0.1")
	if err != nil {
		return fmt.Errorf("create client creds: %w", err)
	}

	lis := bufconn.Listen(1 << 20)
	defer func() { _ = lis.Close() }()

	grpcServer := grpc.NewServer(grpc.Creds(serverCreds))
	agentv1.RegisterAgentServiceServer(grpcServer, &localHealthServer{})
	defer grpcServer.GracefulStop()

	go func() { _ = grpcServer.Serve(lis) }()

	dialOption := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})

	manager := agentclientpkg.NewManagerWithOptions(
		grpc.WithTransportCredentials(clientCreds),
		dialOption,
	)
	defer func() { _ = manager.Close() }()
	target := "passthrough:///bufnet"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := manager.HealthCheck(ctx, target)
	if err != nil {
		return fmt.Errorf("mtls healthcheck: %w", err)
	}
	if !resp.GetHealthy() {
		return fmt.Errorf("mtls verification unhealthy=%v", resp.GetHealthy())
	}

	plaintextManager := agentclientpkg.NewManagerWithOptions(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		dialOption,
	)
	defer func() { _ = plaintextManager.Close() }()

	if _, err := plaintextManager.HealthCheck(ctx, target); err == nil {
		return errors.New("plaintext client unexpectedly succeeded against mTLS server")
	}

	wrongBundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		return fmt.Errorf("generate wrong-ca certs: %w", err)
	}
	wrongClientCreds, err := tlscreds.NewClientCredentials(wrongBundle.ClientCert, wrongBundle.ClientKey, wrongBundle.CACert, "127.0.0.1")
	if err != nil {
		return fmt.Errorf("create wrong-ca client creds: %w", err)
	}
	wrongCAManager := agentclientpkg.NewManagerWithOptions(
		grpc.WithTransportCredentials(wrongClientCreds),
		dialOption,
	)
	defer func() { _ = wrongCAManager.Close() }()

	if _, err := wrongCAManager.HealthCheck(ctx, target); err == nil {
		return errors.New("wrong-ca client unexpectedly succeeded against mTLS server")
	}

	return nil
}

func verifyCRDLocalContracts() error {
	repoRoot := repoRootFromCaller()
	requiredCRDs := []string{
		filepath.Join(repoRoot, "config", "crd", "bases", "pillar-csi.pillar-csi.bhyoo.com_pillartargets.yaml"),
		filepath.Join(repoRoot, "config", "crd", "bases", "pillar-csi.pillar-csi.bhyoo.com_pillarpools.yaml"),
		filepath.Join(repoRoot, "config", "crd", "bases", "pillar-csi.pillar-csi.bhyoo.com_pillarprotocols.yaml"),
		filepath.Join(repoRoot, "config", "crd", "bases", "pillar-csi.pillar-csi.bhyoo.com_pillarbindings.yaml"),
	}
	for _, path := range requiredCRDs {
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read CRD %s: %w", path, err)
		}
		text := string(content)
		for _, needle := range []string{"openAPIV3Schema", "shortNames", "pillar-csi.pillar-csi.bhyoo.com"} {
			if !strings.Contains(text, needle) {
				return fmt.Errorf("CRD %s missing %q", path, needle)
			}
		}
	}

	targetValidator := &webhookv1alpha1.PillarTargetCustomValidator{}
	_, err := targetValidator.ValidateUpdate(context.Background(),
		&pillarv1.PillarTarget{
			Spec: pillarv1.PillarTargetSpec{
				NodeRef: &pillarv1.NodeRefSpec{Name: "node-a"},
			},
		},
		&pillarv1.PillarTarget{
			Spec: pillarv1.PillarTargetSpec{
				NodeRef: &pillarv1.NodeRefSpec{Name: "node-b"},
			},
		},
	)
	if err == nil {
		return errors.New("pillar target validator accepted immutable nodeRef change")
	}

	poolValidator := &webhookv1alpha1.PillarPoolCustomValidator{}
	_, err = poolValidator.ValidateUpdate(context.Background(),
		&pillarv1.PillarPool{
			Spec: pillarv1.PillarPoolSpec{
				TargetRef: "target-a",
				Backend:   pillarv1.BackendSpec{Type: pillarv1.BackendTypeZFSZvol},
			},
		},
		&pillarv1.PillarPool{
			Spec: pillarv1.PillarPoolSpec{
				TargetRef: "target-a",
				Backend:   pillarv1.BackendSpec{Type: pillarv1.BackendTypeLVMLV},
			},
		},
	)
	if err == nil {
		return errors.New("pillar pool validator accepted immutable backend type change")
	}

	protocolValidator := &webhookv1alpha1.PillarProtocolCustomValidator{}
	_, err = protocolValidator.ValidateUpdate(context.Background(),
		&pillarv1.PillarProtocol{Spec: pillarv1.PillarProtocolSpec{Type: pillarv1.ProtocolTypeNVMeOFTCP}},
		&pillarv1.PillarProtocol{Spec: pillarv1.PillarProtocolSpec{Type: pillarv1.ProtocolTypeISCSI}},
	)
	if err == nil {
		return errors.New("pillar protocol validator accepted immutable type change")
	}

	scheme := runtime.NewScheme()
	if err := pillarv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register pillar scheme for CRD verifier: %w", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register corev1 scheme for CRD verifier: %w", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register storagev1 scheme for CRD verifier: %w", err)
	}

	lvmPool := &pillarv1.PillarPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-lvm"},
		Spec: pillarv1.PillarPoolSpec{
			TargetRef: "target-a",
			Backend: pillarv1.BackendSpec{
				Type: pillarv1.BackendTypeLVMLV,
				LVM: &pillarv1.LVMBackendConfig{
					VolumeGroup:      "data-vg",
					ProvisioningMode: pillarv1.LVMProvisioningModeThin,
				},
			},
		},
	}
	nfsProtocol := &pillarv1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: "protocol-nfs"},
		Spec: pillarv1.PillarProtocolSpec{
			Type: pillarv1.ProtocolTypeNFS,
			NFS:  &pillarv1.NFSConfig{Version: "4.2"},
		},
	}

	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lvmPool, nfsProtocol).
		Build()

	defaulter := &webhookv1alpha1.PillarBindingCustomDefaulter{Client: fakeClient}
	binding := &pillarv1.PillarBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "binding-local"},
		Spec: pillarv1.PillarBindingSpec{
			PoolRef:     lvmPool.Name,
			ProtocolRef: nfsProtocol.Name,
		},
	}
	if err := defaulter.Default(context.Background(), binding); err != nil {
		return fmt.Errorf("pillar binding defaulter: %w", err)
	}
	if binding.Spec.StorageClass.AllowVolumeExpansion == nil || !*binding.Spec.StorageClass.AllowVolumeExpansion {
		return errors.New("pillar binding defaulter did not derive allowVolumeExpansion=true for lvm pool")
	}

	bindingValidator := &webhookv1alpha1.PillarBindingCustomValidator{Client: fakeClient}
	_, err = bindingValidator.ValidateCreate(context.Background(), binding)
	if err == nil {
		return errors.New("pillar binding validator accepted incompatible lvm+nfs combination")
	}

	return nil
}

func verifyKindBootstrapLocalContracts() error {
	state, err := newKindBootstrapState()
	if err != nil {
		return fmt.Errorf("new kind bootstrap state: %w", err)
	}
	defer func() { _ = os.RemoveAll(state.SuiteRootDir) }()

	if err := state.validate(); err != nil {
		return fmt.Errorf("validate kind bootstrap state: %w", err)
	}

	payload, err := state.encode()
	if err != nil {
		return fmt.Errorf("encode kind bootstrap state: %w", err)
	}
	decoded, err := decodeKindBootstrapState(payload)
	if err != nil {
		return fmt.Errorf("decode kind bootstrap state: %w", err)
	}
	if decoded.ClusterName != state.ClusterName || decoded.KubeconfigPath != state.KubeconfigPath {
		return fmt.Errorf("decoded kind bootstrap state mismatch: %#v vs %#v", decoded, state)
	}

	state.KubeContext = "kind-" + state.ClusterName

	restoreEnv := captureEnv("KUBECONFIG", "KIND_CLUSTER", suiteRootEnvVar, suiteContextEnvVar)
	defer restoreEnv()

	if err := state.exportEnvironment(); err != nil {
		return fmt.Errorf("export kind bootstrap environment: %w", err)
	}

	if got := os.Getenv("KUBECONFIG"); got != state.KubeconfigPath {
		return fmt.Errorf("KUBECONFIG = %q, want %q", got, state.KubeconfigPath)
	}
	if got := os.Getenv(suiteRootEnvVar); got != state.SuiteRootDir {
		return fmt.Errorf("%s = %q, want %q", suiteRootEnvVar, got, state.SuiteRootDir)
	}

	return nil
}

// verifyHelmChartLocalContracts checks that the Helm chart tree at
// charts/pillar-csi/ is structurally sound without running helm or connecting
// to a Kubernetes cluster.  It verifies:
//
//  1. The chart directory exists at the expected path.
//  2. Chart.yaml is present and contains the required 'name' and 'version' fields.
//  3. values.yaml is present and contains key top-level sections.
//  4. The templates/ directory exists.
//  5. Required template files are present (csidriver.yaml, agent-daemonset.yaml,
//     node-daemonset.yaml, controller-deployment.yaml).
//
// This is a fast in-process validation used by the E27 cases in the default
// profile.  Real cluster deployment/validation lives in tc_e27_helm_e2e_test.go.
func verifyHelmChartLocalContracts() error {
	chartPath, err := findHelmChartPath()
	if err != nil {
		return fmt.Errorf("locate helm chart: %w", err)
	}

	// 1. Chart directory must exist.
	if info, err := os.Stat(chartPath); err != nil {
		return fmt.Errorf("helm chart directory %q: %w", chartPath, err)
	} else if !info.IsDir() {
		return fmt.Errorf("helm chart path %q is not a directory", chartPath)
	}

	// 2. Chart.yaml must be present and contain required fields.
	chartYAML := filepath.Join(chartPath, "Chart.yaml")
	chartContent, err := os.ReadFile(chartYAML) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read Chart.yaml: %w", err)
	}
	chartStr := string(chartContent)
	for _, required := range []string{"name:", "version:", "apiVersion:"} {
		if !strings.Contains(chartStr, required) {
			return fmt.Errorf("chart.yaml missing required field %q", required)
		}
	}
	if !strings.Contains(chartStr, "pillar-csi") {
		return fmt.Errorf("chart.yaml does not reference 'pillar-csi'")
	}

	// 3. values.yaml must be present and have key sections.
	valuesYAML := filepath.Join(chartPath, "values.yaml")
	valuesContent, err := os.ReadFile(valuesYAML) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read values.yaml: %w", err)
	}
	valuesStr := string(valuesContent)
	for _, section := range []string{"csiDriver:", "controller:", "node:", "agent:"} {
		if !strings.Contains(valuesStr, section) {
			return fmt.Errorf("values.yaml missing required section %q", section)
		}
	}

	// 4 & 5. templates/ directory and required template files must exist.
	templatesDir := filepath.Join(chartPath, "templates")
	if info, err := os.Stat(templatesDir); err != nil {
		return fmt.Errorf("helm chart templates/ directory %q: %w", templatesDir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("helm chart templates path %q is not a directory", templatesDir)
	}

	requiredTemplates := []string{
		"csidriver.yaml",
		"agent-daemonset.yaml",
		"node-daemonset.yaml",
		"controller-deployment.yaml",
	}
	for _, tmpl := range requiredTemplates {
		tmplPath := filepath.Join(templatesDir, tmpl)
		if _, err := os.Stat(tmplPath); err != nil {
			return fmt.Errorf("required helm template %q not found: %w", tmpl, err)
		}
	}

	// Validate CSIDriver template references the expected driver name.
	csiDriverTmpl := filepath.Join(templatesDir, "csidriver.yaml")
	csiDriverContent, err := os.ReadFile(csiDriverTmpl) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read csidriver.yaml template: %w", err)
	}
	if !strings.Contains(string(csiDriverContent), "pillar-csi.bhyoo.com") {
		return fmt.Errorf("csidriver.yaml template does not reference 'pillar-csi.bhyoo.com'")
	}

	return nil
}

// findHelmChartPath locates the charts/pillar-csi directory relative to the
// test file's location in the module tree.  It walks up from the current
// source file (local_backend.go in test/e2e/) to find the module root.
func findHelmChartPath() (string, error) {
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed: cannot determine test file path")
	}
	// thisFile is …/test/e2e/local_backend.go — walk up two levels to module root.
	moduleRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	chartPath := filepath.Join(moduleRoot, "charts", "pillar-csi")
	abs, err := filepath.Abs(chartPath)
	if err != nil {
		return "", fmt.Errorf("resolve chart path: %w", err)
	}
	return abs, nil
}

func verifyLVMLocalBackend() error {
	ctx := context.Background()

	container, _, lvmVG := requireSuiteBackendEnv()
	execFn := realContainerExecFn(container)
	backend := lvmb.NewWithExecFn(lvmVG, "", execFn)
	if err := backend.Validate(); err != nil {
		return fmt.Errorf("lvm validate: %w", err)
	}
	if backend.Type() != agentv1.BackendType_BACKEND_TYPE_LVM {
		return fmt.Errorf("lvm backend type = %v, want BACKEND_TYPE_LVM", backend.Type())
	}

	volumeID := lvmVG + "/pvc-verifier-local"

	devicePath, allocated, err := backend.Create(ctx, volumeID, 10<<20, nil)
	if err != nil {
		return fmt.Errorf("lvm create: %w", err)
	}
	wantPath := "/dev/" + lvmVG + "/pvc-verifier-local"
	if devicePath != wantPath {
		return fmt.Errorf("lvm create returned path=%q, want %q", devicePath, wantPath)
	}
	if allocated != 10<<20 {
		return fmt.Errorf("lvm create allocated=%d, want %d", allocated, 10<<20)
	}

	allocated, err = backend.Expand(ctx, volumeID, 20<<20)
	if err != nil {
		return fmt.Errorf("lvm expand: %w", err)
	}
	if allocated != 20<<20 {
		return fmt.Errorf("lvm expand allocated=%d, want %d", allocated, 20<<20)
	}

	total, available, err := backend.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("lvm capacity: %w", err)
	}
	if total <= 0 {
		return fmt.Errorf("lvm capacity total=%d, want > 0", total)
	}
	if available < 0 {
		return fmt.Errorf("lvm capacity available=%d, want >= 0", available)
	}

	volumes, err := backend.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("lvm list volumes: %w", err)
	}
	found := false
	for _, v := range volumes {
		if v.GetVolumeId() == volumeID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("lvm list volumes did not include %q: %#v", volumeID, volumes)
	}

	if err := backend.Delete(ctx, volumeID); err != nil {
		return fmt.Errorf("lvm delete: %w", err)
	}

	return nil
}

// verifyZFSLocalBackend exercises the ZFS VolumeBackend against real ZFS inside
// the Kind cluster container. All backend code paths (Create, idempotent Create,
// Expand, Capacity, ListVolumes, Delete, idempotent Delete) are exercised against
// the production implementation in internal/agent/backend/zfs.
//
// Commands execute via "docker exec <container>" using realContainerExecFn.
func verifyZFSLocalBackend() error {
	ctx := context.Background()

	container, zfsPool, _ := requireSuiteBackendEnv()
	execFn := realContainerExecFn(container)

	// Ensure the ZFS namespace parent dataset exists.
	// The ZFS backend (namespace "k8s") stores zvols at pool/k8s/<volname>;
	// the parent filesystem dataset pool/k8s must be created first.
	if _, err := execFn(ctx, "zfs", "create", "-p", zfsPool+"/k8s"); err != nil {
		if !strings.Contains(err.Error(), "already exists") &&
			!strings.Contains(err.Error(), "dataset already exists") {
			return fmt.Errorf("verifyZFSLocalBackend: create parent dataset %s/k8s: %w", zfsPool, err)
		}
	}

	backend := zfsb.NewWithExecFn(zfsPool, "k8s", execFn)
	if backend.Type() != agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
		return fmt.Errorf("zfs backend type = %v, want BACKEND_TYPE_ZFS_ZVOL", backend.Type())
	}

	volumeID := zfsPool + "/pvc-verifier-local"
	wantDevicePath := "/dev/zvol/" + zfsPool + "/k8s/pvc-verifier-local"

	devicePath, allocated, err := backend.Create(ctx, volumeID, 10<<20, nil)
	if err != nil {
		return fmt.Errorf("zfs create: %w", err)
	}
	if devicePath != wantDevicePath {
		return fmt.Errorf("zfs create returned path=%q, want %q", devicePath, wantDevicePath)
	}
	if allocated != 10<<20 {
		return fmt.Errorf("zfs create allocated=%d, want %d", allocated, 10<<20)
	}

	// Idempotent create (same size) must return existing device path and size.
	dp2, alloc2, err := backend.Create(ctx, volumeID, 10<<20, nil)
	if err != nil {
		return fmt.Errorf("zfs create idempotent: %w", err)
	}
	if dp2 != devicePath || alloc2 != allocated {
		return fmt.Errorf("zfs create idempotent: got path=%q alloc=%d, want %q %d",
			dp2, alloc2, devicePath, allocated)
	}

	allocated, err = backend.Expand(ctx, volumeID, 20<<20)
	if err != nil {
		return fmt.Errorf("zfs expand: %w", err)
	}
	if allocated != 20<<20 {
		return fmt.Errorf("zfs expand allocated=%d, want %d", allocated, 20<<20)
	}

	total, available, err := backend.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("zfs capacity: %w", err)
	}
	if total <= 0 {
		return fmt.Errorf("zfs capacity total=%d, want > 0", total)
	}
	if available < 0 {
		return fmt.Errorf("zfs capacity available=%d, want >= 0", available)
	}

	volumes, err := backend.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("zfs list volumes: %w", err)
	}
	found := false
	for _, v := range volumes {
		if v.GetVolumeId() == volumeID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("zfs list volumes did not include %q: %#v", volumeID, volumes)
	}

	if err := backend.Delete(ctx, volumeID); err != nil {
		return fmt.Errorf("zfs delete: %w", err)
	}

	// Idempotent delete must return nil for a non-existent volume.
	if err := backend.Delete(ctx, volumeID); err != nil {
		return fmt.Errorf("zfs delete idempotent: %w", err)
	}

	volumesAfter, err := backend.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("zfs list volumes after delete: %w", err)
	}
	for _, v := range volumesAfter {
		if v.GetVolumeId() == volumeID {
			return fmt.Errorf("zfs list volumes after delete: volume %q still present", volumeID)
		}
	}

	return nil
}

func mountCapability(fsType string) *csiapi.VolumeCapability {
	return &csiapi.VolumeCapability{
		AccessType: &csiapi.VolumeCapability_Mount{
			Mount: &csiapi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csiapi.VolumeCapability_AccessMode{
			Mode: csiapi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func captureEnv(keys ...string) func() {
	original := make(map[string]*string, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			copyValue := value
			original[key] = &copyValue
			continue
		}
		original[key] = nil
	}

	return func() {
		for _, key := range keys {
			if value := original[key]; value != nil {
				_ = os.Setenv(key, *value)
				continue
			}
			_ = os.Unsetenv(key)
		}
	}
}

func nvmeofTCPExportParams(addr string, port int32) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{
				BindAddress: addr,
				Port:        port,
			},
		},
	}
}

type localMockConnector struct {
	connectErr    error
	disconnectErr error
	devicePath    string
	devicePathErr error

	connectCalls    []localConnectCall
	disconnectCalls []string
	getDeviceCalls  []string
}

type localConnectCall struct {
	subsysNQN string
	trAddr    string
	trSvcID   string
}

var _ csidrv.Connector = (*localMockConnector)(nil)

func (m *localMockConnector) Connect(_ context.Context, subsysNQN, trAddr, trSvcID string) error {
	m.connectCalls = append(m.connectCalls, localConnectCall{subsysNQN: subsysNQN, trAddr: trAddr, trSvcID: trSvcID})
	return m.connectErr
}

func (m *localMockConnector) Disconnect(_ context.Context, subsysNQN string) error {
	m.disconnectCalls = append(m.disconnectCalls, subsysNQN)
	return m.disconnectErr
}

func (m *localMockConnector) GetDevicePath(_ context.Context, subsysNQN string) (string, error) {
	m.getDeviceCalls = append(m.getDeviceCalls, subsysNQN)
	if m.devicePathErr != nil {
		return "", m.devicePathErr
	}
	return m.devicePath, nil
}

type localMockMounter struct {
	mounted map[string]bool

	formatAndMountErr error
	mountErr          error
	unmountErr        error
	isMountedErr      error

	formatAndMountCalls []localFormatAndMountCall
	mountCalls          []localMountCall
	unmountCalls        []string
}

type localFormatAndMountCall struct {
	source  string
	target  string
	fsType  string
	options []string
}

type localMountCall struct {
	source  string
	target  string
	fsType  string
	options []string
}

func newLocalMockMounter() *localMockMounter {
	return &localMockMounter{mounted: make(map[string]bool)}
}

var _ csidrv.Mounter = (*localMockMounter)(nil)

func (m *localMockMounter) FormatAndMount(source, target, fsType string, options []string) error {
	m.formatAndMountCalls = append(m.formatAndMountCalls, localFormatAndMountCall{source: source, target: target, fsType: fsType, options: slices.Clone(options)})
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if m.formatAndMountErr != nil {
		return m.formatAndMountErr
	}
	m.mounted[target] = true
	return nil
}

func (m *localMockMounter) Mount(source, target, fsType string, options []string) error {
	m.mountCalls = append(m.mountCalls, localMountCall{source: source, target: target, fsType: fsType, options: slices.Clone(options)})
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mounted[target] = true
	return nil
}

func (m *localMockMounter) Unmount(target string) error {
	m.unmountCalls = append(m.unmountCalls, target)
	if m.unmountErr != nil {
		return m.unmountErr
	}
	delete(m.mounted, target)
	return nil
}

func (m *localMockMounter) IsMounted(target string) (bool, error) {
	if m.isMountedErr != nil {
		return false, m.isMountedErr
	}
	return m.mounted[target], nil
}

type localMockResizer struct {
	calls []localResizeCall
	err   error
}

type localResizeCall struct {
	mountPath string
	fsType    string
}

var _ csidrv.Resizer = (*localMockResizer)(nil)

func (m *localMockResizer) ResizeFS(mountPath, fsType string) error {
	m.calls = append(m.calls, localResizeCall{mountPath: mountPath, fsType: fsType})
	return m.err
}

type localHealthServer struct {
	agentv1.UnimplementedAgentServiceServer
}

func (*localHealthServer) HealthCheck(_ context.Context, _ *agentv1.HealthCheckRequest) (*agentv1.HealthCheckResponse, error) {
	return &agentv1.HealthCheckResponse{
		Healthy:      true,
		AgentVersion: "local-mtls",
	}, nil
}
