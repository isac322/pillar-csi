package e2e

// tc_e29_inprocess_test.go — Per-TC assertions for E29: CSI Controller LVM parameter propagation.
//
// E29 covers the CSI controller's LVM parameter propagation path: how an lvm
// PillarStore (volumeGroup, thinPool, provisioningMode), the binding-level
// overrides.backend.lvm document, a hand-written StorageClass backend document
// and the pillar-csi.bhyoo.com/backend PVC annotation resolve into the
// LvmVolumeParams carried by the agent CreateVolume RPC.
//
// All functions use the controllerTestEnv (fakeAgentServer via bufconn).

import (
	"fmt"
	"strings"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const (
	e29StoreName = "lvm-store"
	e29VG        = "data-vg"
	e29ThinPool  = "thin0"
)

// lvmControllerParams seeds the LVM PillarStore used by the E29 TCs (volume
// group data-vg with thin pool thin0, linear by default) and returns the
// hand-written StorageClass parameters referencing it.
func lvmControllerParams(env *controllerTestEnv) map[string]string {
	store := e2eLVMStore(e29StoreName, env.target.Name, e29VG, e29ThinPool, pillarv1.LVMProvisioningModeLinear)
	if err := env.k8sClient.Create(env.ctx, store); err != nil {
		Expect(err.Error()).To(ContainSubstring("already exists"), "create LVM store %s", e29StoreName)
	}
	return e2eHandWrittenParams(e29StoreName, e2eDefaultProtocolName)
}

// lvmACLControllerParams is lvmControllerParams over the ACL-enabled protocol
// "nvmeof-acl", for TCs that exercise the initiator grant/revoke path.
func lvmACLControllerParams(env *controllerTestEnv) map[string]string {
	params := lvmControllerParams(env)
	params[e2eParamProtocolRef] = e2eDefaultACLProtocolName
	return params
}

// agentCreateReqs safely copies the fakeAgentServer's captured CreateVolume requests.
func agentCreateReqs(env *controllerTestEnv) []*agentv1.CreateVolumeRequest {
	env.agentSrv.mu.Lock()
	defer env.agentSrv.mu.Unlock()
	out := make([]*agentv1.CreateVolumeRequest, len(env.agentSrv.createVolumeReqs))
	copy(out, env.agentSrv.createVolumeReqs)
	return out
}

// makeLVMBinding creates an lvm PillarStore and a PillarStorageClass binding it
// to the default NVMe-oF protocol, and returns the binding name for the
// generated-StorageClass parameter.
// storeMode may be empty (store leaves provisioningMode unset).
// bindingOverrideMode may be empty (no binding-level override).
func makeLVMBinding(
	env *controllerTestEnv,
	suffix string,
	storeMode pillarv1.LVMProvisioningMode,
	bindingOverrideMode pillarv1.LVMProvisioningMode,
) string {
	storeName := fmt.Sprintf("store-lvm-%s", suffix)
	bindingName := fmt.Sprintf("binding-lvm-%s", suffix)

	store := e2eLVMStore(storeName, env.target.Name, e29VG, e29ThinPool, "")
	store.Spec.Backend.LVM.ProvisioningMode = storeMode
	Expect(env.k8sClient.Create(env.ctx, store)).To(Succeed(),
		"create LVM store %s for test", storeName)

	binding := e2eBinding(bindingName, storeName, e2eDefaultProtocolName)
	if bindingOverrideMode != "" {
		binding.Spec.Overrides = &pillarv1.StorageClassOverrides{
			Backend: &pillarv1.BackendOverrides{
				LVM: &pillarv1.LVMBackendOverrides{
					ProvisioningMode: bindingOverrideMode,
				},
			},
		}
	}
	Expect(env.k8sClient.Create(env.ctx, binding)).To(Succeed(),
		"create LVM binding %s for test", bindingName)

	return bindingName
}

// makePVCWithBackendAnnotation creates a PVC whose pillar-csi.bhyoo.com/backend
// document sets lvm.provisioningMode to mode (an empty mode writes an empty
// document).  Returns the PVC.
func makePVCWithBackendAnnotation(env *controllerTestEnv, name, mode string) *corev1.PersistentVolumeClaim {
	var annot string
	if mode != "" {
		annot = fmt.Sprintf("lvm:\n  provisioningMode: %s\n", mode)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				e2eDocBackend: annot,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{},
	}
	Expect(env.k8sClient.Create(env.ctx, pvc)).To(Succeed(),
		"create PVC %s for test", name)
	return pvc
}

// e29BindingParams returns generated-StorageClass parameters for binding plus
// the external-provisioner PVC metadata when pvc is non-nil.
func e29BindingParams(binding string, pvc *corev1.PersistentVolumeClaim) map[string]string {
	params := e2eBindingParams(binding)
	if pvc != nil {
		params[e2eParamPVCName] = pvc.Name
		params[e2eParamPVCNamespace] = pvc.Namespace
	}
	return params
}

// e29CreateAndCaptureMode issues CreateVolume and returns the provisioning mode
// the agent received.
func e29CreateAndCaptureMode(tc documentedCase, env *controllerTestEnv, name string, params map[string]string) string {
	GinkgoHelper()
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               name,
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume %s", tc.tcNodeLabel(), name)
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: VolumeId", tc.tcNodeLabel())
	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: exactly one agent CreateVolume call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendType()).To(Equal(agentv1.BackendType_BACKEND_TYPE_LVM),
		"%s: BackendType must be BACKEND_TYPE_LVM", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetVolumeGroup()).To(Equal(e29VG),
		"%s: the store's lvm.volumeGroup must reach the agent", tc.tcNodeLabel())
	return reqs[0].GetBackendParams().GetLvm().GetProvisionMode()
}

// ─────────────────────────────────────────────────────────────────────────────
// E29.1: LVM CreateVolume normal path (linear, thin, VolumeId format)
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_CreateVolume_LVM_Linear — a linear lvm store provisions a
// linear LV.
func assertE29_CreateVolume_LVM_Linear(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	mode := e29CreateAndCaptureMode(tc, env, "pvc-e29-linear", lvmControllerParams(env))
	Expect(mode).To(Equal("linear"), "%s: ProvisionMode must be linear", tc.tcNodeLabel())
}

// TestCSIController_CreateVolume_LVM_Thin — a hand-written StorageClass backend
// document selects thin provisioning on a store that declares a thin pool.
func assertE29_CreateVolume_LVM_Thin(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env)
	params[e2eDocBackend] = "lvm:\n  provisioningMode: thin\n"
	mode := e29CreateAndCaptureMode(tc, env, "pvc-e29-thin", params)
	Expect(mode).To(Equal("thin"), "%s: ProvisionMode must be thin", tc.tcNodeLabel())
	Expect(agentCreateReqs(env)[0].GetBackendParams().GetLvm().GetThinPool()).To(Equal(e29ThinPool),
		"%s: the store's lvm.thinPool must reach the agent", tc.tcNodeLabel())
}

// TestCSIController_CreateVolume_LVM_VolumeIdFormat
func assertE29_CreateVolume_LVM_VolumeIdFormat(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-volid-fmt",
		Parameters:         lvmControllerParams(env),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume for VolumeId format test", tc.tcNodeLabel())

	vid := resp.GetVolume().GetVolumeId()
	// Format: <agent>/<protocol>/<backend>/<volumeGroup>/<volume-name>
	parts := strings.Split(vid, "/")
	Expect(parts).To(HaveLen(5), "%s: VolumeId should have 5 slash-delimited segments, got %q", tc.tcNodeLabel(), vid)
	Expect(parts[2]).To(Equal("lvm-lv"),
		"%s: VolumeId segment[2] must be 'lvm-lv' (backend), got %q", tc.tcNodeLabel(), vid)
	Expect(parts[3]).To(Equal(e29VG),
		"%s: VolumeId segment[3] must be the volume group, got %q", tc.tcNodeLabel(), vid)
}

// ─────────────────────────────────────────────────────────────────────────────
// E29.2: Provisioning mode override 3-tier + PVC annotation edge cases
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_LVM_ModeOverride_PoolDefault — store lvm.provisioningMode=thin
// with no binding or PVC override: agent receives ProvisionMode="thin".
func assertE29_LVM_ModeOverride_PoolDefault(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	binding := makeLVMBinding(env, "pool-default", pillarv1.LVMProvisioningModeThin, "")
	mode := e29CreateAndCaptureMode(tc, env, "pvc-e29-mode-pool", e29BindingParams(binding, nil))
	Expect(mode).To(Equal("thin"), "%s: store-level mode 'thin' must reach agent", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_StorageClassOverridesPool — the binding
// overrides the store mode. Store=thin, binding override=linear → "linear".
func assertE29_LVM_ModeOverride_StorageClassOverridesPool(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	binding := makeLVMBinding(env, "bind-override",
		pillarv1.LVMProvisioningModeThin, pillarv1.LVMProvisioningModeLinear)
	mode := e29CreateAndCaptureMode(tc, env, "pvc-e29-mode-bind", e29BindingParams(binding, nil))
	Expect(mode).To(Equal("linear"), "%s: binding override 'linear' must win over store 'thin'", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_PVCAnnotationOverridesBinding — the PVC
// backend document overrides the binding. Binding=linear, PVC=thin → "thin".
func assertE29_LVM_ModeOverride_PVCAnnotationOverridesBinding(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	binding := makeLVMBinding(env, "pvc-override",
		pillarv1.LVMProvisioningModeThin, pillarv1.LVMProvisioningModeLinear)
	pvc := makePVCWithBackendAnnotation(env, "pvc-e29-mode-annot", "thin")
	mode := e29CreateAndCaptureMode(tc, env, "pvc-e29-mode-annot", e29BindingParams(binding, pvc))
	Expect(mode).To(Equal("thin"), "%s: PVC document 'thin' must win over binding 'linear'", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_AbsentUsesBackendDefault — when no layer
// sets a mode the single documented default (linear) applies.  The fake client
// does not run CRD defaulting, so the store's mode is genuinely unset here;
// the agent treats an empty mode as linear as well.
func assertE29_LVM_ModeOverride_AbsentUsesBackendDefault(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	binding := makeLVMBinding(env, "absent-mode", "", "")
	mode := e29CreateAndCaptureMode(tc, env, "pvc-e29-mode-absent", e29BindingParams(binding, nil))
	Expect(mode).To(BeElementOf("", "linear"),
		"%s: an unset mode must resolve to the linear default, got %q", tc.tcNodeLabel(), mode)
}

// TestCSIController_LVM_ModeOverride_InvalidPVCAnnotation — a PVC backend
// document with provisioningMode="striped" is outside the lvm enum and is
// rejected by the shared decoder before the agent is called.
func assertE29_LVM_ModeOverride_InvalidPVCAnnotation(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	binding := makeLVMBinding(env, "invalid-mode", "", pillarv1.LVMProvisioningModeLinear)
	pvc := makePVCWithBackendAnnotation(env, "pvc-e29-invalid-mode", "striped")

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-invalid-mode",
		Parameters:         e29BindingParams(binding, pvc),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: invalid provisioning mode must be rejected, got %v", tc.tcNodeLabel(), err)
	Expect(err.Error()).To(ContainSubstring("lvm.provisioningMode"),
		"%s: the rejection must name the field path", tc.tcNodeLabel())
	Expect(agentCreateReqs(env)).To(BeEmpty(),
		"%s: an invalid document must not reach the agent", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_EmptyPVCAnnotation_FallsThrough — an empty
// pillar-csi.bhyoo.com/backend document is not an override, so the binding's
// value "thin" is preserved.
func assertE29_LVM_ModeOverride_EmptyPVCAnnotation_FallsThrough(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	binding := makeLVMBinding(env, "fallthrough", "", pillarv1.LVMProvisioningModeThin)
	pvc := makePVCWithBackendAnnotation(env, "pvc-e29-empty-annot", "")
	mode := e29CreateAndCaptureMode(tc, env, "pvc-e29-empty-annot", e29BindingParams(binding, pvc))
	Expect(mode).To(Equal("thin"),
		"%s: binding 'thin' preserved because an empty document is not an override", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E29.3: LVM DeleteVolume and ControllerExpandVolume
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_DeleteVolume_LVM — DeleteVolume calls UnexportVolume → DeleteVolume on agent.
func assertE29_DeleteVolume_LVM(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-del-lvm",
		Parameters:         lvmControllerParams(env),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume", tc.tcNodeLabel())
	vid := resp.GetVolume().GetVolumeId()

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: vid,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM DeleteVolume", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.UnexportVolume).To(Equal(1),
		"%s: UnexportVolume must be called exactly once during DeleteVolume", tc.tcNodeLabel())
	Expect(c.DeleteVolume).To(Equal(1),
		"%s: DeleteVolume must be called exactly once on agent", tc.tcNodeLabel())
}

// TestCSIController_ControllerExpandVolume_LVM — ControllerExpandVolume calls agent.ExpandVolume.
func assertE29_ControllerExpandVolume_LVM(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-expand-lvm",
		Parameters:         lvmControllerParams(env),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume", tc.tcNodeLabel())
	vid := resp.GetVolume().GetVolumeId()

	expandResp, err := env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      vid,
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 20 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM ControllerExpandVolume", tc.tcNodeLabel())
	Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", 20<<20),
		"%s: expanded capacity must be >= 2 GiB", tc.tcNodeLabel())
	Expect(expandResp.GetNodeExpansionRequired()).To(BeTrue(),
		"%s: NodeExpansionRequired must be true for LVM block device", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.ExpandVolume).To(Equal(1),
		"%s: agent.ExpandVolume must be called exactly once", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E29.4: LVM full round-trip (4-stage Controller chain)
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_LVM_FullRoundTrip — CreateVolume → ControllerPublishVolume →
// ControllerUnpublishVolume → DeleteVolume; verifies agent call sequence.
func assertE29_LVM_FullRoundTrip(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Stage 1: CreateVolume
	createResp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-fullroundtrip",
		Parameters:         lvmACLControllerParams(env),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: stage 1 CreateVolume", tc.tcNodeLabel())
	vid := createResp.GetVolume().GetVolumeId()
	Expect(vid).NotTo(BeEmpty(), "%s: VolumeId", tc.tcNodeLabel())

	// Stage 2: ControllerPublishVolume
	makeCSINodeWithNQN(env, "worker-lvm", "nqn.2026-01.com.bhyoo.pillar-csi:worker-lvm")
	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         vid,
		NodeId:           "worker-lvm",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: stage 2 ControllerPublishVolume", tc.tcNodeLabel())

	// Stage 3: ControllerUnpublishVolume
	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: vid,
		NodeId:   "worker-lvm",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: stage 3 ControllerUnpublishVolume", tc.tcNodeLabel())

	// Stage 4: DeleteVolume
	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: vid,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: stage 4 DeleteVolume", tc.tcNodeLabel())

	// Verify agent call sequence and BackendType
	c := env.agentSrv.counts()
	Expect(c.CreateVolume).To(BeNumerically(">=", 1),
		"%s: agent.CreateVolume must have been called", tc.tcNodeLabel())
	Expect(c.AllowInitiator).To(BeNumerically(">=", 1),
		"%s: agent.AllowInitiator (ControllerPublish) must have been called", tc.tcNodeLabel())
	Expect(c.DenyInitiator).To(BeNumerically(">=", 1),
		"%s: agent.DenyInitiator (ControllerUnpublish) must have been called", tc.tcNodeLabel())
	Expect(c.DeleteVolume).To(BeNumerically(">=", 1),
		"%s: agent.DeleteVolume must have been called", tc.tcNodeLabel())

	// Verify BackendType=LVM in the CreateVolume request
	reqs := agentCreateReqs(env)
	Expect(reqs).NotTo(BeEmpty(), "%s: CreateVolume request was captured", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendType()).To(Equal(agentv1.BackendType_BACKEND_TYPE_LVM),
		"%s: BackendType must be BACKEND_TYPE_LVM", tc.tcNodeLabel())
}
