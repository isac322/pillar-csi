package e2e

// tc_e35_iscsi_inprocess_test.go — E35: iSCSI protocol support.
//
// An iscsi PillarProtocol exports volumes as LIO iSCSI targets and the node
// plugin logs in with its in-process initiator.  These specs pin the
// component contracts in-process: the controller's CreateVolume / publish
// path for an iscsi protocol (export RPC shape, VolumeContext keys, initiator
// IQN resolution from the CSINode annotation, CHAP credentials read from the
// installation-namespace Secret), the generated StorageClass's node-stage
// Secret and the admission rules of the protocol union and of auth.  The
// kernel data path (LIO target, iscsi_tcp session, /dev/sd* device, ACL and
// CHAP rejection, online resize) is exercised by
// test/docker-e2e/iscsi_e2e_test.go and iscsi_chap_e2e_test.go.
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
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	pillarcontroller "github.com/isac322/pillar-csi/internal/controller"
	csidrv "github.com/isac322/pillar-csi/internal/csi"
	"github.com/isac322/pillar-csi/internal/testutil/fakeuid"
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

	// e35CHAPProtocolName is an iscsi PillarProtocol with one-way CHAP.
	e35CHAPProtocolName = "iscsi-chap"
	// e35MutualCHAPProtocolName is an iscsi PillarProtocol with MutualCHAP.
	e35MutualCHAPProtocolName = "iscsi-mutual-chap"
	// e35InstallNamespace is the pillar-csi installation namespace holding
	// the CHAP Secret.
	e35InstallNamespace = "pillar-csi-system"
	// e35CHAPSecretName is the Secret both CHAP protocols reference.
	e35CHAPSecretName = "iscsi-chap-credentials"

	e35CHAPUsername    = "pillar-initiator"
	e35CHAPPassword    = "initiator-secret-0001"
	e35MutualUsername  = "pillar-target"
	e35MutualPassword  = "target-secret-00002"
	e35RotatedPassword = "initiator-secret-0002"

	// StorageClass parameters through which kubelet hands the node-stage
	// Secret to NodeStageVolume.
	e35NodeStageSecretNameParam      = "csi.storage.k8s.io/node-stage-secret-name"
	e35NodeStageSecretNamespaceParam = "csi.storage.k8s.io/node-stage-secret-namespace"
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

// e35CHAPProtocol returns an acl: true iscsi PillarProtocol authenticating
// with method through e35CHAPSecretName.
func e35CHAPProtocol(name string, method pillarv1.ISCSIAuthMethod) *pillarv1.PillarProtocol {
	p := e35ISCSIProtocol(name, true)
	p.Spec.Protocol.ISCSI.Auth = &pillarv1.ISCSIAuth{
		Method:    method,
		SecretRef: &pillarv1.ISCSIAuthSecretReference{Name: e35CHAPSecretName},
	}
	return p
}

// e35CHAPSecretData returns valid CHAP Secret data; mutual adds the target
// credentials MutualCHAP needs.
func e35CHAPSecretData(mutual bool) map[string]string {
	data := map[string]string{
		csidrv.ISCSIChapSecretKeyUsername: e35CHAPUsername,
		csidrv.ISCSIChapSecretKeyPassword: e35CHAPPassword,
	}
	if mutual {
		data[csidrv.ISCSIChapSecretKeyMutualUsername] = e35MutualUsername
		data[csidrv.ISCSIChapSecretKeyMutualPassword] = e35MutualPassword
	}
	return data
}

// e35SecretBytes converts string Secret data to Secret.Data.
func e35SecretBytes(data map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(data))
	for k, v := range data {
		out[k] = []byte(v)
	}
	return out
}

// e35CreateCHAPSecret stores a CHAP Secret in the installation namespace.
func e35CreateCHAPSecret(env *controllerTestEnv, name string, data map[string]string) {
	GinkgoHelper()
	Expect(env.k8sClient.Create(env.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: e35InstallNamespace, Name: name},
		Data:       e35SecretBytes(data),
	})).To(Succeed())
}

// newE35ControllerEnv returns a controller environment that additionally
// holds the iscsi protocols and knows the installation namespace.
func newE35ControllerEnv() *controllerTestEnv {
	env := newControllerTestEnv()
	env.controller.SetInstallNamespace(e35InstallNamespace)
	Expect(env.k8sClient.Create(env.ctx, e35ISCSIProtocol(e35ProtocolName, false))).To(Succeed())
	Expect(env.k8sClient.Create(env.ctx, e35ISCSIProtocol(e35ACLProtocolName, true))).To(Succeed())
	Expect(env.k8sClient.Create(env.ctx, e35CHAPProtocol(e35CHAPProtocolName, pillarv1.ISCSIAuthMethodCHAP))).
		To(Succeed())
	Expect(env.k8sClient.Create(env.ctx, e35CHAPProtocol(e35MutualCHAPProtocolName, pillarv1.ISCSIAuthMethodMutualCHAP))).
		To(Succeed())
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

// e35AllowReqs returns the recorded AllowInitiator requests.
func e35AllowReqs(env *controllerTestEnv) []*agentv1.AllowInitiatorRequest {
	env.agentSrv.mu.Lock()
	defer env.agentSrv.mu.Unlock()
	return append([]*agentv1.AllowInitiatorRequest(nil), env.agentSrv.allowInitiatorReqs...)
}

// e35Publish publishes volumeID to e35ClientNode.
func e35Publish(env *controllerTestEnv, volumeID string) {
	GinkgoHelper()
	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           e35ClientNode,
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "publish %s to %s", volumeID, e35ClientNode)
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

	It("[TC-E35.7] CHAP 프로토콜 CreateVolume은 Secret 자격 증명을 export에 싣고 VolumeContext에 auth method를 기록", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		e35CreateCHAPSecret(env, e35CHAPSecretName, e35CHAPSecretData(false))

		vol := e35CreateVolume(env, "pvc-e35-chap", e2eDefaultStoreName, e35CHAPProtocolName)
		Expect(vol.GetVolumeContext()).To(HaveKeyWithValue(csidrv.VolumeContextKeyISCSIAuthMethod, "CHAP"),
			"[TC-E35.7] auth method is fixed into the VolumeContext at CreateVolume")
		reqs := e35ExportReqs(env)
		Expect(reqs).To(HaveLen(1), "[TC-E35.7] one ExportVolume")
		chap := reqs[0].GetExportParams().GetIscsi().GetChap()
		Expect(chap).NotTo(BeNil(), "[TC-E35.7] CHAP export carries credentials")
		Expect(chap.GetUsername()).To(Equal(e35CHAPUsername), "[TC-E35.7] username from the Secret")
		Expect(chap.GetPassword()).To(Equal(e35CHAPPassword), "[TC-E35.7] password from the Secret")
		Expect(chap.GetMutualUsername()).To(BeEmpty(), "[TC-E35.7] one-way CHAP has no mutual username")
		Expect(chap.GetMutualPassword()).To(BeEmpty(), "[TC-E35.7] one-way CHAP has no mutual password")

		plain := e35CreateVolume(env, "pvc-e35-none", e2eDefaultStoreName, e35ACLProtocolName)
		Expect(plain.GetVolumeContext()).NotTo(HaveKey(csidrv.VolumeContextKeyISCSIAuthMethod),
			"[TC-E35.7] method None omits the auth key")
		reqs = e35ExportReqs(env)
		Expect(reqs).To(HaveLen(2), "[TC-E35.7] second ExportVolume")
		Expect(reqs[1].GetExportParams().GetIscsi().GetChap()).To(BeNil(), "[TC-E35.7] method None exports without CHAP")
	})

	It("[TC-E35.8] MutualCHAP publish는 AllowInitiator에 양방향 자격 증명을 싣고 Secret 교체는 다음 publish에 반영", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		e35CreateCHAPSecret(env, e35CHAPSecretName, e35CHAPSecretData(true))
		e35MakeCSINodeWithIQN(env, e35ClientNode, e35ClientIQN)

		vol := e35CreateVolume(env, "pvc-e35-mutual", e2eDefaultStoreName, e35MutualCHAPProtocolName)
		Expect(vol.GetVolumeContext()).To(HaveKeyWithValue(csidrv.VolumeContextKeyISCSIAuthMethod, "MutualCHAP"),
			"[TC-E35.8] mutual method recorded in the VolumeContext")
		e35Publish(env, vol.GetVolumeId())
		allows := e35AllowReqs(env)
		Expect(allows).To(HaveLen(1), "[TC-E35.8] one AllowInitiator")
		Expect(allows[0].GetInitiatorId()).To(Equal(e35ClientIQN), "[TC-E35.8] grant for the CSINode IQN")
		chap := allows[0].GetExportParams().GetIscsi().GetChap()
		Expect(chap.GetUsername()).To(Equal(e35CHAPUsername), "[TC-E35.8] initiator username on the ACL")
		Expect(chap.GetPassword()).To(Equal(e35CHAPPassword), "[TC-E35.8] initiator password on the ACL")
		Expect(chap.GetMutualUsername()).To(Equal(e35MutualUsername), "[TC-E35.8] target username on the ACL")
		Expect(chap.GetMutualPassword()).To(Equal(e35MutualPassword), "[TC-E35.8] target password on the ACL")

		_, err := env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
			VolumeId: vol.GetVolumeId(),
			NodeId:   e35ClientNode,
		})
		Expect(err).NotTo(HaveOccurred(), "[TC-E35.8] unpublish")
		secret := &corev1.Secret{}
		Expect(env.k8sClient.Get(env.ctx, types.NamespacedName{Namespace: e35InstallNamespace, Name: e35CHAPSecretName}, secret)).
			To(Succeed())
		secret.Data[csidrv.ISCSIChapSecretKeyPassword] = []byte(e35RotatedPassword)
		Expect(env.k8sClient.Update(env.ctx, secret)).To(Succeed(), "[TC-E35.8] rotate the initiator password")

		e35Publish(env, vol.GetVolumeId())
		allows = e35AllowReqs(env)
		Expect(allows).To(HaveLen(2), "[TC-E35.8] republish grants again")
		Expect(allows[1].GetExportParams().GetIscsi().GetChap().GetPassword()).To(Equal(e35RotatedPassword),
			"[TC-E35.8] the next publish reads the rotated Secret")
	})

	It("[TC-E35.9] CHAP Secret이 없거나 유효하지 않으면 FailedPrecondition이고 인증 없는 export·ACL은 만들지 않는다", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		e35MakeCSINodeWithIQN(env, e35ClientNode, e35ClientIQN)

		_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
			Name:               "pvc-e35-no-secret",
			Parameters:         e2eHandWrittenParams(e2eDefaultStoreName, e35CHAPProtocolName),
			VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
			CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
		})
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E35.9] missing Secret: %v", err)
		Expect(err.Error()).To(ContainSubstring(e35CHAPSecretName), "[TC-E35.9] names the missing Secret")
		Expect(e35ExportReqs(env)).To(BeEmpty(), "[TC-E35.9] no export without credentials")

		const shortPassword = "too-short"
		invalid := e35CHAPSecretData(false)
		invalid[csidrv.ISCSIChapSecretKeyPassword] = shortPassword
		e35CreateCHAPSecret(env, e35CHAPSecretName, invalid)
		_, err = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
			Name:               "pvc-e35-short-secret",
			Parameters:         e2eHandWrittenParams(e2eDefaultStoreName, e35CHAPProtocolName),
			VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
			CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
		})
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E35.9] short password: %v", err)
		Expect(err.Error()).To(ContainSubstring(csidrv.ISCSIChapSecretKeyPassword), "[TC-E35.9] names the invalid key")
		Expect(err.Error()).NotTo(ContainSubstring(shortPassword), "[TC-E35.9] never echoes the secret value")
		Expect(e35ExportReqs(env)).To(BeEmpty(), "[TC-E35.9] no export with invalid credentials")

		secret := &corev1.Secret{}
		Expect(env.k8sClient.Get(env.ctx, types.NamespacedName{Namespace: e35InstallNamespace, Name: e35CHAPSecretName}, secret)).
			To(Succeed())
		secret.Data = e35SecretBytes(e35CHAPSecretData(false))
		Expect(env.k8sClient.Update(env.ctx, secret)).To(Succeed())
		vol := e35CreateVolume(env, "pvc-e35-later-missing", e2eDefaultStoreName, e35CHAPProtocolName)
		Expect(env.k8sClient.Delete(env.ctx, secret)).To(Succeed(), "[TC-E35.9] delete the Secret after provisioning")
		_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
			VolumeId:         vol.GetVolumeId(),
			NodeId:           e35ClientNode,
			VolumeCapability: mountCapability("ext4"),
		})
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E35.9] publish without the Secret: %v", err)
		Expect(err.Error()).To(ContainSubstring(e35CHAPSecretName), "[TC-E35.9] publish error names the Secret")
		Expect(env.agentSrv.counts().AllowInitiator).To(BeZero(), "[TC-E35.9] no ACL without credentials")
	})

	It("[TC-E35.10] CHAP 프로토콜의 생성 StorageClass는 node-stage Secret 파라미터를 가진다", Label("default-profile", "E35"), func() {
		ctx := context.Background()
		scheme := runtime.NewScheme()
		Expect(pillarv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1.AddToScheme(scheme)).To(Succeed())
		ready := []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now(),
		}}
		store := e2eZFSStore(e2eDefaultStoreName, "storage-1", e2eDefaultZFSPool)
		store.Status.Conditions = ready
		chapProtocol := e35CHAPProtocol(e35CHAPProtocolName, pillarv1.ISCSIAuthMethodCHAP)
		chapProtocol.Status.Conditions = ready
		plainProtocol := e35ISCSIProtocol(e35ACLProtocolName, true)
		plainProtocol.Status.Conditions = ready
		k8sClient := clientfake.NewClientBuilder().
			WithInterceptorFuncs(fakeuid.Interceptor()).
			WithScheme(scheme).
			WithStatusSubresource(&pillarv1.PillarStorageClass{}, &pillarv1.PillarStore{}, &pillarv1.PillarProtocol{}).
			WithObjects(store, chapProtocol, plainProtocol,
				e2eBinding("e35-chap-class", e2eDefaultStoreName, e35CHAPProtocolName),
				e2eBinding("e35-plain-class", e2eDefaultStoreName, e35ACLProtocolName)).
			Build()
		reconciler := &pillarcontroller.PillarStorageClassReconciler{
			Client:    k8sClient,
			Scheme:    scheme,
			Namespace: e35InstallNamespace,
		}
		params := func(binding string) map[string]string {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: binding}}
			// The first pass adds the deletion-protection finalizer; the second
			// creates the StorageClass.
			for range 2 {
				_, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.10] reconcile %s", binding)
			}
			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: binding}, sc)).To(Succeed(),
				"[TC-E35.10] generated StorageClass %s", binding)
			return sc.Parameters
		}

		chapParams := params("e35-chap-class")
		Expect(chapParams).To(HaveKeyWithValue(e35NodeStageSecretNameParam, e35CHAPSecretName),
			"[TC-E35.10] kubelet passes the protocol's Secret to NodeStage")
		Expect(chapParams).To(HaveKeyWithValue(e35NodeStageSecretNamespaceParam, e35InstallNamespace),
			"[TC-E35.10] the Secret lives in the installation namespace")

		plainParams := params("e35-plain-class")
		Expect(plainParams).NotTo(HaveKey(e35NodeStageSecretNameParam), "[TC-E35.10] method None needs no node-stage Secret")
		Expect(plainParams).NotTo(HaveKey(e35NodeStageSecretNamespaceParam), "[TC-E35.10] method None needs no Secret namespace")
	})

	It("[TC-E35.11] PillarProtocol 웹훅은 secretRef 없는 CHAP과 acl false인 CHAP을 거부", Label("default-profile", "E35"), func() {
		validator := &webhookv1alpha1.PillarProtocolCustomValidator{}

		noSecret := e35CHAPProtocol("chap-no-secret", pillarv1.ISCSIAuthMethodCHAP)
		noSecret.Spec.Protocol.ISCSI.Auth.SecretRef = nil
		_, err := validator.ValidateCreate(context.Background(), noSecret)
		Expect(err).To(HaveOccurred(), "[TC-E35.11] CHAP without secretRef admitted")
		Expect(err.Error()).To(ContainSubstring("auth.secretRef is required"), "[TC-E35.11] secretRef rule named")

		noACL := e35CHAPProtocol("mutual-no-acl", pillarv1.ISCSIAuthMethodMutualCHAP)
		noACL.Spec.Protocol.ISCSI.ACL = false
		_, err = validator.ValidateCreate(context.Background(), noACL)
		Expect(err).To(HaveOccurred(), "[TC-E35.11] MutualCHAP without acl admitted")
		Expect(err.Error()).To(ContainSubstring("require acl: true"), "[TC-E35.11] acl rule named")

		_, err = validator.ValidateCreate(context.Background(), e35CHAPProtocol("chap-valid", pillarv1.ISCSIAuthMethodCHAP))
		Expect(err).NotTo(HaveOccurred(), "[TC-E35.11] CHAP with secretRef and acl is valid")
	})

	It("[TC-E35.12] PVC 프로토콜 오버라이드 문서의 iscsi.auth는 구조 필드로 거부", Label("default-profile", "E35"), func() {
		env := newE35ControllerEnv()
		defer env.close()
		e35CreateCHAPSecret(env, e35CHAPSecretName, e35CHAPSecretData(false))
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "pvc-e35-auth-override",
				Namespace:   "default",
				Annotations: map[string]string{e2eDocProtocol: "iscsi:\n  auth:\n    method: None\n"},
			},
		}
		Expect(env.k8sClient.Create(env.ctx, pvc)).To(Succeed())

		params := e2eHandWrittenParams(e2eDefaultStoreName, e35CHAPProtocolName)
		params[e2eParamPVCName] = pvc.Name
		params[e2eParamPVCNamespace] = pvc.Namespace
		_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
			Name:               pvc.Name,
			Parameters:         params,
			VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
			CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
		})
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument), "[TC-E35.12] auth downgrade per volume: %v", err)
		Expect(err.Error()).To(ContainSubstring("iscsi.auth is structural and cannot be set per volume"),
			"[TC-E35.12] rejection names the structural path")
		env.agentSrv.mu.Lock()
		created := len(env.agentSrv.createVolumeReqs)
		env.agentSrv.mu.Unlock()
		Expect(created).To(BeZero(), "[TC-E35.12] no agent call after a rejected document")
	})
})
