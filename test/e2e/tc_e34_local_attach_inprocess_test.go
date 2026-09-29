package e2e

// tc_e34_local_attach_inprocess_test.go — E34: local attach on the storage
// node.
//
// A localAttach volume published to the node named by its PillarAgent's
// spec.nodeRef is attached directly (device-mapper linear target over the
// backend device) instead of through NVMe-oF/TCP, and the agent fences the
// network export while the storage node uses it.  These specs pin the
// component contracts in-process; the kernel-level behaviour (dm exclusive
// claim, force-detach fencing, data survival across moves) is exercised by
// test/docker-e2e/storage_e2e_test.go.
//
// The specs are dedicated Ginkgo nodes (not catalog-driven) carrying the
// "default-profile" label, like the E33 standalone specs.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	agentsvc "github.com/isac322/pillar-csi/internal/agent"
	agentbackend "github.com/isac322/pillar-csi/internal/agent/backend"
	nvmeof "github.com/isac322/pillar-csi/internal/agent/nvmeof"
	csidrv "github.com/isac322/pillar-csi/internal/csi"
)

const (
	// e34StorageNode is the node the controller-env PillarAgent is bound to.
	e34StorageNode = "storage-node"
	// e34ClientNode is a node without a PillarAgent.
	e34ClientNode    = "worker-1"
	e34ClientHostNQN = "nqn.2026-01.io.example:worker-1"
)

// newE34ControllerEnv returns a controller environment whose PillarAgent is
// bound to e34StorageNode through spec.nodeRef, so a local attach can apply.
func newE34ControllerEnv() *controllerTestEnv {
	env := newControllerTestEnv()
	agent := &pillarv1.PillarAgent{}
	Expect(env.k8sClient.Get(env.ctx, types.NamespacedName{Name: env.target.Name}, agent)).To(Succeed())
	agent.Spec.NodeRef = &pillarv1.NodeRefSpec{Name: e34StorageNode}
	Expect(env.k8sClient.Update(env.ctx, agent)).To(Succeed())
	return env
}

// e34LocalAttachParams returns ACL-enabled hand-written StorageClass
// parameters with local attach turned on.
func e34LocalAttachParams(env *controllerTestEnv) map[string]string {
	params := env.aclParams()
	params[e2eParamLocalAttach] = "true"
	return params
}

// e34CreateVolume creates a localAttach volume named name and returns its ID.
func e34CreateVolume(env *controllerTestEnv, name string) string {
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               name,
		Parameters:         e34LocalAttachParams(env),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "CreateVolume %s", name)
	return resp.GetVolume().GetVolumeId()
}

func e34Publish(env *controllerTestEnv, volumeID, node string) (*csiapi.ControllerPublishVolumeResponse, error) {
	return env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           node,
		VolumeCapability: mountCapability("ext4"),
	})
}

func e34VolumeState(env *controllerTestEnv, name string) *pillarv1.PillarVolumeState {
	pvs := &pillarv1.PillarVolumeState{}
	Expect(env.k8sClient.Get(env.ctx, types.NamespacedName{Name: name}, pvs)).To(Succeed())
	return pvs
}

func e34SetLocalAttachReqs(env *controllerTestEnv) []*agentv1.SetLocalAttachRequest {
	env.agentSrv.mu.Lock()
	defer env.agentSrv.mu.Unlock()
	return append([]*agentv1.SetLocalAttachRequest(nil), env.agentSrv.setLocalAttachReqs...)
}

// ─── Node: fake device mapper ────────────────────────────────────────────────

type e34DMCall struct {
	op, name, backing string
}

// e34FakeDeviceMapper records device-mapper operations and reports
// /dev/mapper/<name> as the target device.
type e34FakeDeviceMapper struct {
	mu    sync.Mutex
	calls []e34DMCall
}

var _ csidrv.DeviceMapper = (*e34FakeDeviceMapper)(nil)

func (d *e34FakeDeviceMapper) EnsureLinear(_ context.Context, name, backingDevice string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, e34DMCall{op: "ensure", name: name, backing: backingDevice})
	return "/dev/mapper/" + name, nil
}

func (d *e34FakeDeviceMapper) ReloadLinear(_ context.Context, name, backingDevice string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, e34DMCall{op: "reload", name: name, backing: backingDevice})
	return nil
}

func (d *e34FakeDeviceMapper) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, e34DMCall{op: "remove", name: name})
	return nil
}

func (d *e34FakeDeviceMapper) recorded() []e34DMCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]e34DMCall(nil), d.calls...)
}

func e34LocalPublishContext(localNode, devicePath string) map[string]string {
	return map[string]string{
		csidrv.PublishContextKeyAttachMode:      csidrv.AttachModeLocal,
		csidrv.PublishContextKeyLocalNode:       localNode,
		csidrv.PublishContextKeyLocalDevicePath: devicePath,
	}
}

// e34NvmetRoot creates a fake nvmet configfs root whose subsystems directory
// exists but holds no subsystem, so the export of any volume reads as
// fenced.
func e34NvmetRoot() string {
	root, err := os.MkdirTemp(tcTempRoot, "pillar-csi-e34-nvmet-*")
	Expect(err).NotTo(HaveOccurred())
	Expect(os.MkdirAll(filepath.Join(root, "subsystems"), 0o750)).To(Succeed())
	return root
}

// e34WriteNamespaceEnable creates namespace 1 of subsystem nqn in the fake
// nvmet root with the given enable value.
func e34WriteNamespaceEnable(root, nqn, enable string) {
	nsDir := filepath.Join(root, "subsystems", nqn, "namespaces", "1")
	Expect(os.MkdirAll(nsDir, 0o750)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(nsDir, "enable"), []byte(enable+"\n"), 0o600)).To(Succeed())
}

// ─── Agent: namespace enable state ───────────────────────────────────────────

const (
	e34AgentVolumeID   = "tank/pvc-e34-local"
	e34AgentDevicePath = "/dev/zvol/tank/pvc-e34-local"
)

func e34NamespaceEnable(configfsRoot string) string {
	nqn := "nqn.2026-01.com.bhyoo.pillar-csi:" + strings.ReplaceAll(e34AgentVolumeID, "/", ".")
	raw, err := os.ReadFile(filepath.Join(configfsRoot, "nvmet", "subsystems", nqn, "namespaces", "1", "enable"))
	Expect(err).NotTo(HaveOccurred(), "read namespace enable of %s", nqn)
	return strings.TrimSpace(string(raw))
}

func e34Reconcile(srv *agentsvc.Server, fence *agentv1.FencingToken, local bool) *agentv1.ReconcileItemResult {
	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId:   e34AgentVolumeID,
			DevicePath: e34AgentDevicePath,
			Fence:      fence,
			Exports: []*agentv1.ExportDesiredState{{
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
				LocalAttach:  local,
			}},
		}},
	})
	Expect(err).NotTo(HaveOccurred(), "ReconcileState(local_attach=%t)", local)
	Expect(resp.GetResults()).To(HaveLen(1))
	return resp.GetResults()[0]
}

func e34SetLocalAttach(srv *agentsvc.Server, fence *agentv1.FencingToken, local bool) (*agentv1.SetLocalAttachResponse, error) {
	return srv.SetLocalAttach(context.Background(), &agentv1.SetLocalAttachRequest{
		VolumeId:     e34AgentVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Local:        local,
		Fence:        fence,
	})
}

var _ = Describe("E34: 로컬 attach — 스토리지 노드 직접 attach와 export 펜싱", Label("default-profile", "E34", "local-attach"), func() {

	It("[TC-E34.1] 스토리지 노드 publish는 로컬 PublishContext를 반환하고 export를 펜싱", Label("default-profile", "E34"), func() {
		env := newE34ControllerEnv()
		defer env.close()
		volumeID := e34CreateVolume(env, "pvc-e34-local")

		resp, err := e34Publish(env, volumeID, e34StorageNode)
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.1] publish to the storage node")

		reqs := e34SetLocalAttachReqs(env)
		Expect(reqs).To(HaveLen(1), "[TC-E34.1] SetLocalAttach called once")
		Expect(reqs[0].GetLocal()).To(BeTrue(), "[TC-E34.1] export fenced (local=true)")
		Expect(reqs[0].GetFence()).NotTo(BeNil(), "[TC-E34.1] SetLocalAttach carries a fencing token")
		Expect(resp.GetPublishContext()).To(Equal(map[string]string{
			csidrv.PublishContextKeyAttachMode:      csidrv.AttachModeLocal,
			csidrv.PublishContextKeyLocalNode:       e34StorageNode,
			csidrv.PublishContextKeyLocalDevicePath: fakeLocalDevicePath(reqs[0].GetVolumeId()),
		}), "[TC-E34.1] local PublishContext")
		Expect(env.agentSrv.counts().AllowInitiator).To(BeZero(), "[TC-E34.1] no initiator granted for a local attach")

		pvs := e34VolumeState(env, "pvc-e34-local")
		Expect(pvs.Status.LocalAttachNode).To(Equal(e34StorageNode), "[TC-E34.1] status.localAttachNode recorded")
		Expect(pvs.Status.PublishedNodes).To(ContainElement(And(
			HaveField("NodeID", e34StorageNode),
			HaveField("Local", true),
		)), "[TC-E34.1] local publication recorded")

		again, err := e34Publish(env, volumeID, e34StorageNode)
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.1] idempotent retry")
		Expect(again.GetPublishContext()).To(Equal(resp.GetPublishContext()),
			"[TC-E34.1] retry returns the same PublishContext")
	})

	It("[TC-E34.2] 다른 노드 publish는 프로토콜 경로를 사용", Label("default-profile", "E34"), func() {
		env := newE34ControllerEnv()
		defer env.close()
		makeCSINodeWithNQN(env, e34ClientNode, e34ClientHostNQN)
		volumeID := e34CreateVolume(env, "pvc-e34-remote")

		resp, err := e34Publish(env, volumeID, e34ClientNode)
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.2] publish to a client node")

		Expect(resp.GetPublishContext()).NotTo(HaveKey(csidrv.PublishContextKeyAttachMode),
			"[TC-E34.2] protocol publish carries no attach-mode")
		c := env.agentSrv.counts()
		Expect(c.AllowInitiator).To(Equal(1), "[TC-E34.2] initiator granted")
		Expect(c.SetLocalAttach).To(Equal(1), "[TC-E34.2] localAttach export re-enabled once")
		reqs := e34SetLocalAttachReqs(env)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].GetLocal()).To(BeFalse(), "[TC-E34.2] export re-enabled, not fenced")
		Expect(e34VolumeState(env, "pvc-e34-remote").Status.LocalAttachNode).To(BeEmpty(),
			"[TC-E34.2] no localAttachNode")
	})

	It("[TC-E34.3] 스토리지 노드가 디바이스를 잡고 있으면 원격 publish는 FailedPrecondition, 해제 후 성공", Label("default-profile", "E34"), func() {
		env := newE34ControllerEnv()
		defer env.close()
		makeCSINodeWithNQN(env, e34ClientNode, e34ClientHostNQN)
		volumeID := e34CreateVolume(env, "pvc-e34-handoff")

		_, err := e34Publish(env, volumeID, e34StorageNode)
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.3] local publish")
		_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
			VolumeId: volumeID,
			NodeId:   e34StorageNode,
		})
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.3] unpublish from the storage node")
		Expect(env.agentSrv.counts().DenyInitiator).To(BeZero(), "[TC-E34.3] nothing to revoke for a local publication")
		Expect(e34VolumeState(env, "pvc-e34-handoff").Status.LocalAttachNode).To(Equal(e34StorageNode),
			"[TC-E34.3] unpublish keeps localAttachNode")

		env.agentSrv.mu.Lock()
		env.agentSrv.setLocalAttachErr = status.Error(codes.FailedPrecondition,
			"backend device /dev/zvol/tank/pvc-e34-handoff is still held on the storage node (local attach in use)")
		env.agentSrv.mu.Unlock()

		_, err = e34Publish(env, volumeID, e34ClientNode)
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
			"[TC-E34.3] remote publish refused while the device is held: %v", err)
		Expect(env.agentSrv.counts().AllowInitiator).To(BeZero(), "[TC-E34.3] no grant while held")
		Expect(e34VolumeState(env, "pvc-e34-handoff").Status.LocalAttachNode).To(Equal(e34StorageNode),
			"[TC-E34.3] localAttachNode kept while held")

		env.agentSrv.mu.Lock()
		env.agentSrv.setLocalAttachErr = nil
		env.agentSrv.mu.Unlock()

		resp, err := e34Publish(env, volumeID, e34ClientNode)
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.3] remote publish after release")
		Expect(resp.GetPublishContext()).NotTo(HaveKey(csidrv.PublishContextKeyAttachMode))
		reqs := e34SetLocalAttachReqs(env)
		Expect(reqs).NotTo(BeEmpty())
		Expect(reqs[len(reqs)-1].GetLocal()).To(BeFalse(), "[TC-E34.3] export returned to remote service")
		Expect(env.agentSrv.counts().AllowInitiator).To(Equal(1), "[TC-E34.3] client initiator granted")
		Expect(e34VolumeState(env, "pvc-e34-handoff").Status.LocalAttachNode).To(BeEmpty(),
			"[TC-E34.3] localAttachNode cleared after release")
	})

	It("[TC-E34.4] agent는 local_attach 동안 namespace를 끄고 디바이스가 잡혀 있으면 다시 켜지 않음", Label("default-profile", "E34"), func() {
		configfsRoot, err := os.MkdirTemp(tcTempRoot, "pillar-csi-e34-configfs-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(configfsRoot) }()
		Expect(os.MkdirAll(filepath.Join(configfsRoot, "nvmet"), 0o755)).To(Succeed())

		var held atomic.Bool
		var probed atomic.Value
		srv := agentsvc.NewServer(
			map[string]agentbackend.VolumeBackend{},
			configfsRoot,
			agentsvc.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
			agentsvc.WithDrainStateDir(filepath.Join(configfsRoot, ".agent-state")),
			agentsvc.WithDeviceClaimer(func(path string) (func() error, error) {
				probed.Store(path)
				if held.Load() {
					return nil, nvmeof.ErrDeviceHeld
				}
				return func() error { return nil }, nil
			}),
		)
		fence := agentLifecycleFence(e34AgentVolumeID)

		result := e34Reconcile(srv, fence, true)
		Expect(result.GetSuccess()).To(BeTrue(), "[TC-E34.4] reconcile local_attach=true: %s", result.GetErrorMessage())
		Expect(e34NamespaceEnable(configfsRoot)).To(Equal("0"), "[TC-E34.4] namespace disabled for local attach")

		held.Store(true)
		result = e34Reconcile(srv, fence, false)
		Expect(result.GetSuccess()).To(BeFalse(), "[TC-E34.4] reconcile must not enable a held device")
		Expect(e34NamespaceEnable(configfsRoot)).To(Equal("0"), "[TC-E34.4] namespace stays disabled")
		Expect(probed.Load()).To(Equal(e34AgentDevicePath), "[TC-E34.4] holder probed on the backend device")

		_, err = e34SetLocalAttach(srv, fence, false)
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
			"[TC-E34.4] SetLocalAttach(false) refused while held: %v", err)
		Expect(e34NamespaceEnable(configfsRoot)).To(Equal("0"))

		held.Store(false)
		_, err = e34SetLocalAttach(srv, fence, false)
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.4] SetLocalAttach(false) after release")
		Expect(e34NamespaceEnable(configfsRoot)).To(Equal("1"), "[TC-E34.4] namespace enabled again")

		resp, err := e34SetLocalAttach(srv, fence, true)
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.4] SetLocalAttach(true)")
		Expect(resp.GetDevicePath()).To(Equal(e34AgentDevicePath), "[TC-E34.4] backend device path returned")
		Expect(e34NamespaceEnable(configfsRoot)).To(Equal("0"), "[TC-E34.4] namespace fenced")
	})

	It("[TC-E34.5] 로컬 stage/unstage는 프로토콜 connector 없이 dm 디바이스를 사용", Label("default-profile", "E34"), func() {
		env := newNodeTestEnv()
		defer env.close()
		dm := &e34FakeDeviceMapper{}
		env.node.WithDeviceMapper(dm).WithNvmetConfigfsRoot(e34NvmetRoot())

		const backing = "/dev/zvol/tank/pvc-node-test"
		volumeID := testVolumeID
		stagePath := filepath.Join(env.stateDir, "stage")
		dmName := csidrv.LocalDMName(volumeID)
		env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

		_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagePath,
			VolumeCapability:  mountCapability("ext4"),
			VolumeContext:     map[string]string{csidrv.VolumeContextKeyTargetID: testTargetNQN},
			PublishContext:    e34LocalPublishContext("node-local", backing),
		})
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.5] local NodeStageVolume")
		Expect(env.connector.connectCalls).To(BeEmpty(), "[TC-E34.5] protocol connector not used on stage")
		Expect(dm.recorded()).To(Equal([]e34DMCall{{op: "ensure", name: dmName, backing: backing}}),
			"[TC-E34.5] dm linear target created over the backend device")
		Expect(env.mounter.formatAndMountCalls).To(HaveLen(1))
		Expect(env.mounter.formatAndMountCalls[0].source).To(Equal("/dev/mapper/"+dmName),
			"[TC-E34.5] filesystem mounted on the dm device")
		Expect(env.mounter.formatAndMountCalls[0].target).To(Equal(stagePath))

		_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagePath,
		})
		Expect(err).NotTo(HaveOccurred(), "[TC-E34.5] local NodeUnstageVolume")
		Expect(env.connector.disconnectCalls).To(BeEmpty(), "[TC-E34.5] protocol connector not used on unstage")
		Expect(env.mounter.unmountCalls).To(ContainElement(stagePath), "[TC-E34.5] staging path unmounted")
		calls := dm.recorded()
		Expect(calls).To(HaveLen(2))
		Expect(calls[1]).To(Equal(e34DMCall{op: "remove", name: dmName}), "[TC-E34.5] dm target removed")

		// A namespace still enabled (export not fenced) must fail the stage
		// and release the claim again.
		nvmetRoot := e34NvmetRoot()
		env.node.WithNvmetConfigfsRoot(nvmetRoot)
		e34WriteNamespaceEnable(nvmetRoot, testTargetNQN, "1")
		env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

		_, err = env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagePath,
			VolumeCapability:  mountCapability("ext4"),
			VolumeContext:     map[string]string{csidrv.VolumeContextKeyTargetID: testTargetNQN},
			PublishContext:    e34LocalPublishContext("node-local", backing),
		})
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
			"[TC-E34.5] stage refused while the export still serves remote initiators: %v", err)
		calls = dm.recorded()
		Expect(calls[len(calls)-2:]).To(Equal([]e34DMCall{
			{op: "ensure", name: dmName, backing: backing},
			{op: "remove", name: dmName},
		}), "[TC-E34.5] dm claim released on a refused stage")
		Expect(env.connector.connectCalls).To(BeEmpty())
		Expect(env.mounter.formatAndMountCalls).To(HaveLen(1), "[TC-E34.5] refused stage mounted nothing")
	})

	It("[TC-E34.6] 다른 노드용 로컬 PublishContext는 FailedPrecondition", Label("default-profile", "E34"), func() {
		env := newNodeTestEnv()
		defer env.close()
		dm := &e34FakeDeviceMapper{}
		env.node.WithDeviceMapper(dm).WithNvmetConfigfsRoot(e34NvmetRoot())

		volumeID := testVolumeID
		env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

		_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: filepath.Join(env.stateDir, "stage"),
			VolumeCapability:  mountCapability("ext4"),
			VolumeContext:     map[string]string{csidrv.VolumeContextKeyTargetID: testTargetNQN},
			PublishContext:    e34LocalPublishContext(e34StorageNode, "/dev/zvol/tank/pvc-node-test"),
		})
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
			"[TC-E34.6] local attach refused on a node other than local-node: %v", err)
		Expect(dm.recorded()).To(BeEmpty(), "[TC-E34.6] no dm target")
		Expect(env.connector.connectCalls).To(BeEmpty(), "[TC-E34.6] no protocol connect")
		Expect(env.mounter.formatAndMountCalls).To(BeEmpty(), "[TC-E34.6] nothing mounted")
	})
})
