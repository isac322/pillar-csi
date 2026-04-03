package e2e

// tc_e29_inprocess_test.go — Per-TC assertions for E29: CSI Controller LVM parameter propagation.
//
// E29 covers the CSI controller's LVM parameter propagation path: how the
// BackendType=LVM flag, LvmVolumeParams (VolumeGroup, ProvisionMode), and the
// 3-tier mode override hierarchy (Pool → Binding → PVC annotation) are carried
// through to the agent RPC requests.
//
// All functions use the controllerTestEnv (fakeAgentServer via bufconn).

import (
	"fmt"
	"strings"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarv1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// lvmControllerParams returns default StorageClass params for LVM volumes.
// Uses pool "data-vg" (matching the paramPool field) and sets lvm-vg for
// VolumeGroup propagation to the agent.
func lvmControllerParams(target string) map[string]string {
	return map[string]string{
		"pillar-csi.bhyoo.com/target":        target,
		"pillar-csi.bhyoo.com/pool":          "data-vg",
		"pillar-csi.bhyoo.com/backend-type":  "lvm-lv",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}
}

// agentCreateReqs safely copies the fakeAgentServer's captured CreateVolume requests.
func agentCreateReqs(env *controllerTestEnv) []*agentv1.CreateVolumeRequest {
	env.agentSrv.mu.Lock()
	defer env.agentSrv.mu.Unlock()
	out := make([]*agentv1.CreateVolumeRequest, len(env.agentSrv.createVolumeReqs))
	copy(out, env.agentSrv.createVolumeReqs)
	return out
}

// makeLVMBinding creates a PillarPool (lvm-lv type) and PillarBinding in the
// fake K8s client and returns the binding name for use as paramBinding.
// poolMode may be empty (pool has no LVM mode preference).
// bindingOverrideMode may be empty (no binding-level override).
func makeLVMBinding(
	env *controllerTestEnv,
	suffix string,
	poolMode pillarv1.LVMProvisioningMode,
	bindingOverrideMode pillarv1.LVMProvisioningMode,
) string {
	poolName := fmt.Sprintf("pool-lvm-%s", suffix)
	bindingName := fmt.Sprintf("binding-lvm-%s", suffix)

	pool := &pillarv1.PillarPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName},
		Spec: pillarv1.PillarPoolSpec{
			TargetRef: env.target.Name,
			Backend: pillarv1.BackendSpec{
				Type: pillarv1.BackendTypeLVMLV,
				LVM: &pillarv1.LVMBackendConfig{
					VolumeGroup:      "data-vg",
					ProvisioningMode: poolMode,
				},
			},
		},
	}
	Expect(env.k8sClient.Create(env.ctx, pool)).To(Succeed(),
		"create LVM pool %s for test", poolName)

	binding := &pillarv1.PillarBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName},
		Spec: pillarv1.PillarBindingSpec{
			PoolRef:     poolName,
			ProtocolRef: "proto-nvmeof",
		},
	}
	if bindingOverrideMode != "" {
		binding.Spec.Overrides = &pillarv1.BindingOverrides{
			Backend: &pillarv1.BackendOverrides{
				LVM: &pillarv1.LVMOverrides{
					ProvisioningMode: bindingOverrideMode,
				},
			},
		}
	}
	Expect(env.k8sClient.Create(env.ctx, binding)).To(Succeed(),
		"create LVM binding %s for test", bindingName)

	return bindingName
}

// makePVCWithBackendAnnotation creates a PVC with the given LVM provisioningMode
// in the structured backend-override annotation. Returns the PVC.
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
				"pillar-csi.bhyoo.com/backend-override": annot,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{},
	}
	Expect(env.k8sClient.Create(env.ctx, pvc)).To(Succeed(),
		"create PVC %s for test", name)
	return pvc
}

// ─────────────────────────────────────────────────────────────────────────────
// E29.1: LVM CreateVolume normal path (linear, thin, VolumeId format)
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_CreateVolume_LVM_Linear
func assertE29_CreateVolume_LVM_Linear(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/lvm-mode"] = "linear"

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-linear",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM linear CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: VolumeId", tc.tcNodeLabel())

	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: exactly one agent CreateVolume call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendType()).To(Equal(agentv1.BackendType_BACKEND_TYPE_LVM),
		"%s: BackendType must be BACKEND_TYPE_LVM", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(Equal("linear"),
		"%s: ProvisionMode must be linear", tc.tcNodeLabel())
}

// TestCSIController_CreateVolume_LVM_Thin
func assertE29_CreateVolume_LVM_Thin(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/lvm-mode"] = "thin"

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-thin",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM thin CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: VolumeId", tc.tcNodeLabel())

	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: exactly one agent CreateVolume call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(Equal("thin"),
		"%s: ProvisionMode must be thin", tc.tcNodeLabel())
}

// TestCSIController_CreateVolume_LVM_VolumeIdFormat
func assertE29_CreateVolume_LVM_VolumeIdFormat(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-volid-fmt",
		Parameters:         lvmControllerParams(env.target.Name),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume for VolumeId format test", tc.tcNodeLabel())

	vid := resp.GetVolume().GetVolumeId()
	// Format: <target>/<protocol>/<backend>/<pool>/<volume-name>
	// e.g. "storage-1/nvmeof-tcp/lvm-lv/data-vg/pvc-e29-volid-fmt"
	parts := strings.Split(vid, "/")
	Expect(parts).To(HaveLen(5),
		"%s: VolumeId should have 5 slash-delimited segments, got %q", tc.tcNodeLabel(), vid)
	Expect(parts[2]).To(Equal("lvm-lv"),
		"%s: VolumeId segment[2] must be 'lvm-lv' (backend-type), got %q", tc.tcNodeLabel(), vid)
	Expect(parts[3]).To(Equal("data-vg"),
		"%s: VolumeId segment[3] must be 'data-vg' (pool), got %q", tc.tcNodeLabel(), vid)
}

// ─────────────────────────────────────────────────────────────────────────────
// E29.2: Provisioning mode override 3-tier + PVC annotation edge cases
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_LVM_ModeOverride_PoolDefault — Pool.lvm.provisioningMode="thin"
// with no Binding or PVC override: agent receives ProvisionMode="thin".
func assertE29_LVM_ModeOverride_PoolDefault(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Pool: thin. No binding override, no PVC annotation.
	bindingName := makeLVMBinding(env, "pool-default", pillarv1.LVMProvisioningModeThin, "")

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/binding"] = bindingName

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-mode-pool",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume with Pool default mode", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: VolumeId", tc.tcNodeLabel())

	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: one agent call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(Equal("thin"),
		"%s: Pool-level mode 'thin' must reach agent", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_BindingOverridesPool — Binding overrides Pool mode.
// Pool="thin", Binding override="linear" → agent receives "linear".
func assertE29_LVM_ModeOverride_BindingOverridesPool(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Pool: thin. Binding override: linear.
	bindingName := makeLVMBinding(env, "bind-override",
		pillarv1.LVMProvisioningModeThin, pillarv1.LVMProvisioningModeLinear)

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/binding"] = bindingName

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-mode-bind",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume with Binding override mode", tc.tcNodeLabel())

	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: one agent call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(Equal("linear"),
		"%s: Binding override 'linear' must win over Pool's 'thin'", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_PVCAnnotationOverridesBinding — PVC annotation
// overrides Binding-level mode. Binding="linear", PVC annotation="thin" → agent gets "thin".
func assertE29_LVM_ModeOverride_PVCAnnotationOverridesBinding(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Pool: thin. Binding override: linear.
	bindingName := makeLVMBinding(env, "pvc-override",
		pillarv1.LVMProvisioningModeThin, pillarv1.LVMProvisioningModeLinear)

	// PVC with backend-override annotation setting lvm.provisioningMode=thin
	pvc := makePVCWithBackendAnnotation(env, "pvc-e29-mode-annot", "thin")

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/binding"] = bindingName
	params["csi.storage.k8s.io/pvc-name"] = pvc.Name
	params["csi.storage.k8s.io/pvc-namespace"] = pvc.Namespace

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-mode-annot",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume with PVC annotation override", tc.tcNodeLabel())

	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: one agent call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(Equal("thin"),
		"%s: PVC annotation 'thin' must win over Binding's 'linear'", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_AbsentUsesBackendDefault — when all layers are absent,
// ProvisionMode="" is passed to the agent (agent applies its compiled-in default).
func assertE29_LVM_ModeOverride_AbsentUsesBackendDefault(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Pool: no mode set. No binding override. No PVC annotation.
	bindingName := makeLVMBinding(env, "absent-mode", "", "")

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/binding"] = bindingName

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-mode-absent",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume with absent mode", tc.tcNodeLabel())

	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: one agent call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(BeEmpty(),
		"%s: absent mode must result in empty ProvisionMode (agent uses backend default)", tc.tcNodeLabel())
	_ = resp
}

// TestCSIController_LVM_ModeOverride_InvalidPVCAnnotation — PVC annotation sets
// provisioningMode="striped" which is invalid; fakeAgent is configured to reject it.
func assertE29_LVM_ModeOverride_InvalidPVCAnnotation(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Simulate agent rejecting an unknown provisioning mode.
	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Errorf(codes.InvalidArgument,
		"unknown provisioning mode: striped")
	env.agentSrv.mu.Unlock()

	// PVC with backend-override annotation: lvm.provisioningMode=striped
	pvc := makePVCWithBackendAnnotation(env, "pvc-e29-invalid-mode", "striped")

	params := lvmControllerParams(env.target.Name)
	params["csi.storage.k8s.io/pvc-name"] = pvc.Name
	params["csi.storage.k8s.io/pvc-namespace"] = pvc.Namespace

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-invalid-mode",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(),
		"%s: CreateVolume with invalid provisioning mode should fail", tc.tcNodeLabel())

	// Verify the mode made it to the agent (fakeAgent recorded the request before rejecting).
	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: agent was called once", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(Equal("striped"),
		"%s: invalid mode 'striped' was propagated to agent before rejection", tc.tcNodeLabel())
}

// TestCSIController_LVM_ModeOverride_EmptyPVCAnnotation_FallsThrough — when the PVC
// annotation uses the flat-key style with an empty value for lvm-mode, the Binding-level
// value "thin" is preserved (flat-key annotation uses "lvm-mode" not the full paramLVMMode key).
func assertE29_LVM_ModeOverride_EmptyPVCAnnotation_FallsThrough(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Binding override: thin (this should be the effective mode).
	bindingName := makeLVMBinding(env, "fallthrough", "", pillarv1.LVMProvisioningModeThin)

	// PVC with flat-key annotation lvm-mode="" — this does NOT override paramLVMMode
	// because the flat key strips the "pillar-csi.bhyoo.com/param." prefix, yielding
	// "lvm-mode" which is a different key from paramLVMMode="pillar-csi.bhyoo.com/lvm-mode".
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pvc-e29-empty-annot",
			Namespace: "default",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/param.lvm-mode": "", // flat key, empty value → falls through
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{},
	}
	Expect(env.k8sClient.Create(env.ctx, pvc)).To(Succeed())

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/binding"] = bindingName
	params["csi.storage.k8s.io/pvc-name"] = pvc.Name
	params["csi.storage.k8s.io/pvc-namespace"] = pvc.Namespace

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-empty-annot",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume with empty flat-key annotation", tc.tcNodeLabel())

	reqs := agentCreateReqs(env)
	Expect(reqs).To(HaveLen(1), "%s: one agent call", tc.tcNodeLabel())
	Expect(reqs[0].GetBackendParams().GetLvm().GetProvisionMode()).To(Equal("thin"),
		"%s: Binding 'thin' preserved because empty flat-key annotation does not override paramLVMMode",
		tc.tcNodeLabel())
	_ = resp
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
		Parameters:         lvmControllerParams(env.target.Name),
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
		Parameters:         lvmControllerParams(env.target.Name),
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
		Parameters:         lvmControllerParams(env.target.Name),
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
