package e2e

// tc_e35_iscsi_inprocess_test.go — E35: iSCSI protocol support.
//
// An iscsi PillarProtocol exports volumes as LIO iSCSI targets and the node
// plugin logs in with its in-process initiator.  These specs pin the
// component contracts in-process: the controller's CreateVolume / publish
// path for an iscsi protocol (export RPC shape, VolumeContext keys, initiator
// IQN resolution from the CSINode annotation) and the admission rules of the
// protocol union.  The kernel data path (LIO target, iscsi_tcp session,
// /dev/sd* device, ACL rejection, online resize) is exercised by
// test/docker-e2e/iscsi_e2e_test.go.
//
// The specs are dedicated Ginkgo nodes (not catalog-driven) carrying the
// "default-profile" label, like the E34 standalone specs.

import (
	"context"
	"strings"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	csidrv "github.com/isac322/pillar-csi/internal/csi"
	webhookv1alpha1 "github.com/isac322/pillar-csi/internal/webhook/v1alpha1"
)

const (
	// e35ProtocolName is an iscsi PillarProtocol without ACL.
	e35ProtocolName = "iscsi"
	// e35ACLProtocolName is an iscsi PillarProtocol with acl: true.
	e35ACLProtocolName = "iscsi-acl"
	// e35ClientNode is the node the volume is published to.
	e35ClientNode = "worker-1"
	// e35ClientIQN is the initiator IQN pillar-node publishes for e35ClientNode.
	e35ClientIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
	// e35LoginTimeout is the loginTimeout of e35ProtocolName, in seconds.
	e35LoginTimeout = int32(30)
)

// e35ISCSIProtocol returns an iscsi PillarProtocol on the default port.
func e35ISCSIProtocol(name string, acl bool) *pillarv1.PillarProtocol {
	loginTimeout := e35LoginTimeout
	return &pillarv1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarv1.PillarProtocolSpec{
			Protocol: pillarv1.ProtocolSpec{
				ISCSI: &pillarv1.ISCSIConfig{Port: 3260, ACL: acl, LoginTimeout: &loginTimeout},
			},
		},
	}
}

// newE35ControllerEnv returns a controller environment that additionally
// holds the iscsi protocols.
func newE35ControllerEnv() *controllerTestEnv {
	env := newControllerTestEnv()
	Expect(env.k8sClient.Create(env.ctx, e35ISCSIProtocol(e35ProtocolName, false))).To(Succeed())
	Expect(env.k8sClient.Create(env.ctx, e35ISCSIProtocol(e35ACLProtocolName, true))).To(Succeed())
	return env
}

// e35CreateVolume provisions name through store and protocol.
func e35CreateVolume(env *controllerTestEnv, name, store, protocol string) *csiapi.Volume {
	GinkgoHelper()
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               name,
		Parameters:         e2eHandWrittenParams(store, protocol),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "CreateVolume %s over %s", name, protocol)
	return resp.GetVolume()
}

// e35ExportReqs returns the recorded ExportVolume requests.
func e35ExportReqs(env *controllerTestEnv) []*agentv1.ExportVolumeRequest {
	env.agentSrv.mu.Lock()
	defer env.agentSrv.mu.Unlock()
	return append([]*agentv1.ExportVolumeRequest(nil), env.agentSrv.exportVolumeReqs...)
}

// e35MakeCSINodeWithIQN registers the CSINode annotation pillar-node publishes
// with its iSCSI initiator IQN.
func e35MakeCSINodeWithIQN(env *controllerTestEnv, nodeName, iqn string) {
	GinkgoHelper()
	Expect(env.k8sClient.Create(env.ctx, &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{csidrv.AnnotationISCSIInitiatorIQN: iqn},
		},
	})).To(Succeed())
}

var _ = Describe("E35: iSCSI 프로토콜 — LIO 타깃 export와 인프로세스 initiator 계약", Label("default-profile", "E35", "iscsi"), func() {

	It("[TC-E35.1] iscsi 프로토콜 CreateVolume은 iSCSI export와 iSCSI VolumeContext를 만든다", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()

		vol := e35CreateVolume(env, "pvc-e35-zfs", e2eDefaultStoreName, e35ProtocolName)
		Expect(vol.GetVolumeId()).To(Equal("storage-1/iscsi/zfs-zvol/tank/pvc-e35-zfs"),
			"[TC-E35.1] volume ID carries the iscsi protocol token")

		reqs := e35ExportReqs(env)
		Expect(reqs).To(HaveLen(1), "[TC-E35.1] one ExportVolume")
		Expect(reqs[0].GetProtocolType()).To(Equal(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI),
			"[TC-E35.1] export protocol")
		Expect(reqs[0].GetExportParams().GetNvmeofTcp()).To(BeNil(), "[TC-E35.1] no NVMe-oF params")
		Expect(reqs[0].GetExportParams().GetIscsi()).NotTo(BeNil(), "[TC-E35.1] iSCSI export params")
		Expect(reqs[0].GetExportParams().GetIscsi().GetPort()).To(Equal(int32(3260)), "[TC-E35.1] portal port")

		vc := vol.GetVolumeContext()
		Expect(vc).To(HaveKeyWithValue(csidrv.VolumeContextKeyTargetID,
			"iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-e35-zfs"), "[TC-E35.1] target IQN from the agent")
		Expect(vc).To(HaveKeyWithValue(csidrv.VolumeContextKeyPort, "3260"), "[TC-E35.1] portal port")
		Expect(vc).To(HaveKeyWithValue(csidrv.VolumeContextKeyProtocolType, "iscsi"), "[TC-E35.1] protocol type")
		Expect(vc).To(HaveKeyWithValue(csidrv.VolumeContextKeyISCSILoginTimeout, "30"),
			"[TC-E35.1] resolved loginTimeout forwarded to the node")
		for _, unset := range []string{
			csidrv.VolumeContextKeyISCSIReplacementTimeout,
			csidrv.VolumeContextKeyISCSINoopOutInterval,
			csidrv.VolumeContextKeyISCSINoopOutTimeout,
		} {
			Expect(vc).NotTo(HaveKey(unset), "[TC-E35.1] unset timeout %s keeps the node default", unset)
		}
		for key := range vc {
			Expect(strings.Contains(strings.ToLower(key), "nvme")).To(BeFalse(),
				"[TC-E35.1] iscsi VolumeContext carries NVMe-oF key %s", key)
		}
	})

	It("[TC-E35.2] LVM 백엔드도 iscsi 프로토콜로 export", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		Expect(env.k8sClient.Create(env.ctx, e2eLVMStore("lvm-store", env.target.Name, "data-vg", "", ""))).To(Succeed())

		vol := e35CreateVolume(env, "pvc-e35-lvm", "lvm-store", e35ProtocolName)
		Expect(vol.GetVolumeId()).To(Equal("storage-1/iscsi/lvm-lv/data-vg/pvc-e35-lvm"),
			"[TC-E35.2] volume ID names iscsi, the lvm-lv backend and the volume group")
		reqs := e35ExportReqs(env)
		Expect(reqs).To(HaveLen(1), "[TC-E35.2] one ExportVolume")
		Expect(reqs[0].GetVolumeId()).To(Equal("data-vg/pvc-e35-lvm"), "[TC-E35.2] agent volume ID")
		Expect(reqs[0].GetProtocolType()).To(Equal(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI), "[TC-E35.2] export protocol")
		Expect(vol.GetVolumeContext()).To(HaveKeyWithValue(csidrv.VolumeContextKeyTargetID,
			"iqn.2026-01.com.bhyoo.pillar-csi:data-vg.pvc-e35-lvm"), "[TC-E35.2] target IQN")
	})

	It("[TC-E35.3] acl=true publish는 CSINode의 initiator IQN을 허용하고 unpublish는 회수", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		e35MakeCSINodeWithIQN(env, e35ClientNode, e35ClientIQN)
		vol := e35CreateVolume(env, "pvc-e35-acl", e2eDefaultStoreName, e35ACLProtocolName)

		_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
			VolumeId:         vol.GetVolumeId(),
			NodeId:           e35ClientNode,
			VolumeCapability: mountCapability("ext4"),
		})
		Expect(err).NotTo(HaveOccurred(), "[TC-E35.3] publish")
		env.agentSrv.mu.Lock()
		allows := append([]*agentv1.AllowInitiatorRequest(nil), env.agentSrv.allowInitiatorReqs...)
		env.agentSrv.mu.Unlock()
		Expect(allows).To(HaveLen(1), "[TC-E35.3] one AllowInitiator")
		Expect(allows[0].GetInitiatorId()).To(Equal(e35ClientIQN), "[TC-E35.3] initiator IQN from the CSINode annotation")
		Expect(allows[0].GetProtocolType()).To(Equal(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI), "[TC-E35.3] grant protocol")

		_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
			VolumeId: vol.GetVolumeId(),
			NodeId:   e35ClientNode,
		})
		Expect(err).NotTo(HaveOccurred(), "[TC-E35.3] unpublish")
		env.agentSrv.mu.Lock()
		denies := append([]*agentv1.DenyInitiatorRequest(nil), env.agentSrv.denyInitiatorReqs...)
		env.agentSrv.mu.Unlock()
		Expect(denies).To(HaveLen(1), "[TC-E35.3] one DenyInitiator")
		Expect(denies[0].GetInitiatorId()).To(Equal(e35ClientIQN), "[TC-E35.3] revoked initiator IQN")
		Expect(denies[0].GetProtocolType()).To(Equal(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI), "[TC-E35.3] revoke protocol")
	})

	It("[TC-E35.4] CSINode에 iSCSI IQN 주석이 없으면 acl publish는 FailedPrecondition", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		makeCSINodeWithNQN(env, e35ClientNode, "nqn.2026-01.io.example:worker-1")
		vol := e35CreateVolume(env, "pvc-e35-no-iqn", e2eDefaultStoreName, e35ACLProtocolName)

		_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
			VolumeId:         vol.GetVolumeId(),
			NodeId:           e35ClientNode,
			VolumeCapability: mountCapability("ext4"),
		})
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
			"[TC-E35.4] an NVMe host NQN is not an iSCSI initiator identity: %v", err)
		Expect(err.Error()).To(ContainSubstring(csidrv.AnnotationISCSIInitiatorIQN), "[TC-E35.4] names the missing annotation")
		Expect(env.agentSrv.counts().AllowInitiator).To(BeZero(), "[TC-E35.4] no grant without an initiator IQN")
	})

	It("[TC-E35.5] PillarProtocol 웹훅은 nvmeofTcp와 iscsi를 동시에 지정하면 거부하고 iscsi 단독은 허용", Label("default-profile", "E35"), func() {
		validator := &webhookv1alpha1.PillarProtocolCustomValidator{}
		both := e35ISCSIProtocol("both-members", false)
		both.Spec.Protocol.NVMeOFTCP = &pillarv1.NVMeOFTCPConfig{Port: 4420}
		_, err := validator.ValidateCreate(context.Background(), both)
		Expect(err).To(HaveOccurred(), "[TC-E35.5] two protocol members admitted")
		Expect(err.Error()).To(ContainSubstring("exactly one protocol member"), "[TC-E35.5] union rule named")

		_, err = validator.ValidateCreate(context.Background(), e35ISCSIProtocol("iscsi-only", true))
		Expect(err).NotTo(HaveOccurred(), "[TC-E35.5] a lone iscsi member is valid")
	})

	It("[TC-E35.6] PillarStorageClass 웹훅은 nvmeofTcp 프로토콜에 대한 iscsi 오버라이드를 거부", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		validator := &webhookv1alpha1.PillarStorageClassCustomValidator{Client: env.k8sClient}
		loginTimeout := int32(20)

		mismatch := e2eBinding("e35-mismatch", e2eDefaultStoreName, e2eDefaultProtocolName)
		mismatch.Spec.Overrides = &pillarv1.StorageClassOverrides{
			Protocol: &pillarv1.ProtocolOverrides{ISCSI: &pillarv1.ISCSIOverrides{LoginTimeout: &loginTimeout}},
		}
		_, err := validator.ValidateCreate(env.ctx, mismatch)
		Expect(err).To(HaveOccurred(), "[TC-E35.6] iscsi override on an nvmeofTcp protocol admitted")
		Expect(err.Error()).To(ContainSubstring(`protocol override member "iscsi" does not match the "nvmeofTcp" protocol`),
			"[TC-E35.6] rejection names both members")

		matching := e2eBinding("e35-matching", e2eDefaultStoreName, e35ProtocolName)
		matching.Spec.Overrides = &pillarv1.StorageClassOverrides{
			Protocol: &pillarv1.ProtocolOverrides{ISCSI: &pillarv1.ISCSIOverrides{LoginTimeout: &loginTimeout}},
		}
		_, err = validator.ValidateCreate(env.ctx, matching)
		Expect(err).NotTo(HaveOccurred(), "[TC-E35.6] iscsi override on an iscsi protocol is valid")
	})
})
