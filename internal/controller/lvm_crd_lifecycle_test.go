//go:build integration

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// E32: PillarPool/PillarBinding LVM CRD 라이프사이클
//
// This file implements envtest integration tests that verify the CRD schema
// validation, webhook validation, and controller reconcile behavior for the
// LVM-specific fields of PillarPool and PillarBinding.
//
// E32.1 — PillarPool LVM configuration validation (TC IDs 276–280)
//
//   - TC-276 TestPillarPool_LVM_ValidLinearConfig:
//     type=lvm-lv + lvm.volumeGroup + lvm.provisioningMode=linear is accepted.
//   - TC-277 TestPillarPool_LVM_ValidThinConfig:
//     type=lvm-lv + lvm.volumeGroup + lvm.thinPool + lvm.provisioningMode=thin is accepted.
//   - TC-278 TestPillarPool_LVM_MissingVolumeGroup_Rejected:
//     type=lvm-lv with lvm.volumeGroup="" → CRD MinLength=1 violation → HTTP 422.
//   - TC-279 TestPillarPool_LVM_InvalidProvisioningMode_Rejected:
//     lvm.provisioningMode="striped" → CRD Enum violation → HTTP 422.
//   - TC-280 TestPillarPool_LVM_MissingLVMConfig_Rejected:
//     type=lvm-lv with backend.lvm=nil → webhook Required field error.
//
// E32.2 — PillarBinding LVM override and compatibility (TC IDs 281–284)
//
//   - TC-281 TestPillarBinding_LVM_ValidOverride:
//     PillarBinding with overrides.backend.lvm.provisioningMode=linear reconciles
//     to Ready=True and creates a StorageClass with lvm-vg parameter.
//   - TC-282 TestPillarBinding_LVM_InvalidOverride_Rejected:
//     overrides.backend.lvm.provisioningMode="raid5" → CRD Enum violation → HTTP 422.
//   - TC-283 TestPillarBinding_LVM_NVMeOFTCP_Compatible:
//     lvm-lv backend + nvmeof-tcp protocol → Compatible=True after reconcile.
//   - TC-284 TestPillarBinding_LVM_NFS_Incompatible:
//     lvm-lv backend + nfs protocol → webhook rejects (incompatible).

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// E32.1 — PillarPool LVM CRD schema and webhook validation
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E32.1: PillarPool LVM Configuration Validation", func() {
	var e32Ctx context.Context

	BeforeEach(func() {
		e32Ctx = context.Background()
	})

	// deletePoolIfExists removes a PillarPool by name, stripping finalizers first.
	deletePoolIfExists := func(name string) {
		p := &pillarcsiv1alpha1.PillarPool{}
		if err := k8sClient.Get(e32Ctx, types.NamespacedName{Name: name}, p); err == nil {
			controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
			_ = k8sClient.Update(e32Ctx, p)
			_ = k8sClient.Delete(e32Ctx, p)
		}
	}

	// ── TC-276 ────────────────────────────────────────────────────────────────
	// A PillarPool with type=lvm-lv, a non-empty volumeGroup, and
	// provisioningMode=linear satisfies both the CRD schema and the webhook
	// cross-field constraint.
	It("TC-276: TestPillarPool_LVM_ValidLinearConfig — lvm-lv with linear config is accepted", func() {
		const poolName = "e32-lvm-valid-linear"
		By("creating a PillarPool with type=lvm-lv, volumeGroup=data-vg, provisioningMode=linear")

		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeLVMLV,
					LVM: &pillarcsiv1alpha1.LVMBackendConfig{
						VolumeGroup:      "data-vg",
						ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeLinear,
					},
				},
			},
		}
		Expect(k8sClient.Create(e32Ctx, pool)).To(Succeed(),
			"PillarPool with lvm-lv + linear config should be accepted by the API server")

		DeferCleanup(func() { deletePoolIfExists(poolName) })
	})

	// ── TC-277 ────────────────────────────────────────────────────────────────
	// A PillarPool with type=lvm-lv, volumeGroup, thinPool, and
	// provisioningMode=thin satisfies both the CRD schema and the webhook.
	It("TC-277: TestPillarPool_LVM_ValidThinConfig — lvm-lv with thin config is accepted", func() {
		const poolName = "e32-lvm-valid-thin"
		By("creating a PillarPool with type=lvm-lv, volumeGroup=data-vg, thinPool=thin-pool-0, provisioningMode=thin")

		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeLVMLV,
					LVM: &pillarcsiv1alpha1.LVMBackendConfig{
						VolumeGroup:      "data-vg",
						ThinPool:         "thin-pool-0",
						ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeThin,
					},
				},
			},
		}
		Expect(k8sClient.Create(e32Ctx, pool)).To(Succeed(),
			"PillarPool with lvm-lv + thin config should be accepted by the API server")

		DeferCleanup(func() { deletePoolIfExists(poolName) })
	})

	// ── TC-278 ────────────────────────────────────────────────────────────────
	// spec.backend.lvm.volumeGroup has +kubebuilder:validation:MinLength=1.
	// Submitting an empty volumeGroup should trigger HTTP 422.
	It("TC-278: TestPillarPool_LVM_MissingVolumeGroup_Rejected — empty lvm.volumeGroup is rejected", func() {
		const poolName = "e32-lvm-empty-vg"
		By("submitting a PillarPool with type=lvm-lv and lvm.volumeGroup=\"\"")

		// Use Server-Side Apply so the empty string is sent on the wire.
		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarPool",
			"metadata": {"name": %q},
			"spec": {
				"targetRef": "some-target",
				"backend": {
					"type": "lvm-lv",
					"lvm": {"volumeGroup": ""}
				}
			}
		}`, poolName))

		err := k8sClient.Patch(
			e32Ctx,
			&pillarcsiv1alpha1.PillarPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarPool with empty lvm.volumeGroup")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for MinLength=1 violation on lvm.volumeGroup")

		DeferCleanup(func() { deletePoolIfExists(poolName) })
	})

	// ── TC-279 ────────────────────────────────────────────────────────────────
	// LVMProvisioningMode has +kubebuilder:validation:Enum=linear;thin.
	// A value outside this enum should be rejected.
	It("TC-279: TestPillarPool_LVM_InvalidProvisioningMode_Rejected — invalid provisioningMode is rejected", func() {
		const poolName = "e32-lvm-bad-mode"
		By("submitting a PillarPool with lvm.provisioningMode=\"striped\" (not in enum)")

		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarPool",
			"metadata": {"name": %q},
			"spec": {
				"targetRef": "some-target",
				"backend": {
					"type": "lvm-lv",
					"lvm": {"volumeGroup": "data-vg", "provisioningMode": "striped"}
				}
			}
		}`, poolName))

		err := k8sClient.Patch(
			e32Ctx,
			&pillarcsiv1alpha1.PillarPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarPool with provisioningMode=striped")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for Enum violation on lvm.provisioningMode")

		DeferCleanup(func() { deletePoolIfExists(poolName) })
	})

	// TC-280 is tested in internal/webhook/v1alpha1/pillarpool_webhook_test.go
	// as a direct validator call because the controller suite does not run
	// admission webhooks.  Testing it there exercises the same real code path
	// (validatePillarPoolSpec) without requiring a live webhook server.
})

// ─────────────────────────────────────────────────────────────────────────────
// E32.2 — PillarBinding LVM override and compatibility
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E32.2: PillarBinding LVM Override and Compatibility", func() {
	var e32bCtx context.Context

	BeforeEach(func() {
		e32bCtx = context.Background()
	})

	// deleteBindingIfExists removes a PillarBinding by name.
	deleteBindingIfExists := func(name string) {
		b := &pillarcsiv1alpha1.PillarBinding{}
		if err := k8sClient.Get(e32bCtx, types.NamespacedName{Name: name}, b); err == nil {
			controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
			_ = k8sClient.Update(e32bCtx, b)
			_ = k8sClient.Delete(e32bCtx, b)
		}
	}

	deletePoolIfExistsE32b := func(name string) {
		p := &pillarcsiv1alpha1.PillarPool{}
		if err := k8sClient.Get(e32bCtx, types.NamespacedName{Name: name}, p); err == nil {
			controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
			_ = k8sClient.Update(e32bCtx, p)
			_ = k8sClient.Delete(e32bCtx, p)
		}
	}

	deleteProtocolIfExistsE32b := func(name string) {
		p := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(e32bCtx, types.NamespacedName{Name: name}, p); err == nil {
			controllerutil.RemoveFinalizer(p, pillarProtocolFinalizer)
			_ = k8sClient.Update(e32bCtx, p)
			_ = k8sClient.Delete(e32bCtx, p)
		}
	}

	deleteSCIfExists := func(name string) {
		sc := &storagev1.StorageClass{}
		if err := k8sClient.Get(e32bCtx, types.NamespacedName{Name: name}, sc); err == nil {
			_ = k8sClient.Delete(e32bCtx, sc)
		}
	}

	// ── TC-281 ────────────────────────────────────────────────────────────────
	// A PillarBinding with overrides.backend.lvm.provisioningMode=linear that
	// references a Ready lvm-lv PillarPool and a Ready nvmeof-tcp PillarProtocol
	// should reconcile to Compatible=True, Ready=True, and produce a StorageClass
	// that includes the lvm-vg parameter.
	It("TC-281: TestPillarBinding_LVM_ValidOverride — LVM override reconciles to Ready with StorageClass", func() {
		const (
			poolName     = "e32-lvm-override-pool"
			protocolName = "e32-lvm-override-proto"
			bindingName  = "e32-lvm-valid-override"
		)

		By("creating a Ready lvm-lv PillarPool")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeLVMLV,
					LVM: &pillarcsiv1alpha1.LVMBackendConfig{
						VolumeGroup:      "data-vg",
						ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeLinear,
					},
				},
			},
		}
		Expect(k8sClient.Create(e32bCtx, pool)).To(Succeed())
		DeferCleanup(func() {
			deleteBindingIfExists(bindingName)
			deleteSCIfExists(bindingName)
			deletePoolIfExistsE32b(poolName)
			deleteProtocolIfExistsE32b(protocolName)
		})

		// Set pool Ready condition via status subresource.
		readyStatus := metav1.ConditionTrue
		pool.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             readyStatus,
				Reason:             "AllConditionsMet",
				Message:            "pool ready",
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(e32bCtx, pool)).To(Succeed())

		By("creating a Ready nvmeof-tcp PillarProtocol")
		protocol := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec: pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
			},
		}
		Expect(k8sClient.Create(e32bCtx, protocol)).To(Succeed())

		protocol.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             readyStatus,
				Reason:             "AllConditionsMet",
				Message:            "protocol ready",
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(e32bCtx, protocol)).To(Succeed())

		By("creating a PillarBinding with LVM provisioningMode override=linear")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
				Overrides: &pillarcsiv1alpha1.BindingOverrides{
					Backend: &pillarcsiv1alpha1.BackendOverrides{
						LVM: &pillarcsiv1alpha1.LVMOverrides{
							ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeLinear,
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(e32bCtx, binding)).To(Succeed())

		By("reconciling the PillarBinding (finalizer pass)")
		reconciler := &PillarBindingReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		bindingNN := types.NamespacedName{Name: bindingName}

		_, err := reconciler.Reconcile(e32bCtx, reconcile.Request{NamespacedName: bindingNN})
		Expect(err).NotTo(HaveOccurred())

		By("reconciling the PillarBinding (main reconcile with pool+protocol ready)")
		_, err = reconciler.Reconcile(e32bCtx, reconcile.Request{NamespacedName: bindingNN})
		Expect(err).NotTo(HaveOccurred())

		By("verifying Compatible=True condition")
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(e32bCtx, bindingNN, fetched)).To(Succeed())

		compatCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionCompatible)
		Expect(compatCond).NotTo(BeNil(), "Compatible condition should be set")
		Expect(compatCond.Status).To(Equal(metav1.ConditionTrue),
			"lvm-lv + nvmeof-tcp should be Compatible=True")

		By("verifying Ready=True condition")
		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionReady)
		Expect(readyCond).NotTo(BeNil(), "Ready condition should be set")
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		By("verifying StorageClass was created with lvm-vg parameter")
		sc := &storagev1.StorageClass{}
		Expect(k8sClient.Get(e32bCtx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())
		Expect(sc.Provisioner).To(Equal(pillarCSIProvisioner))
		Expect(sc.Parameters).To(HaveKey("pillar-csi.bhyoo.com/lvm-vg"),
			"StorageClass should carry lvm-vg parameter derived from pool.spec.backend.lvm.volumeGroup")
		Expect(sc.Parameters["pillar-csi.bhyoo.com/lvm-vg"]).To(Equal("data-vg"))
	})

	// ── TC-282 ────────────────────────────────────────────────────────────────
	// LVMOverrides.ProvisioningMode has +kubebuilder:validation:Enum=linear;thin.
	// An invalid value ("raid5") should be rejected by the API server with HTTP 422.
	It("TC-282: TestPillarBinding_LVM_InvalidOverride_Rejected — invalid LVM provisioningMode override is rejected", func() {
		const bindingName = "e32-lvm-invalid-override"
		By("submitting a PillarBinding with overrides.backend.lvm.provisioningMode=\"raid5\"")

		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarBinding",
			"metadata": {"name": %q},
			"spec": {
				"poolRef": "some-pool",
				"protocolRef": "some-protocol",
				"overrides": {
					"backend": {
						"lvm": {"provisioningMode": "raid5"}
					}
				}
			}
		}`, bindingName))

		err := k8sClient.Patch(
			e32bCtx,
			&pillarcsiv1alpha1.PillarBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName}},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarBinding with overrides.lvm.provisioningMode=raid5")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for Enum violation on overrides.lvm.provisioningMode")

		DeferCleanup(func() { deleteBindingIfExists(bindingName) })
	})

	// ── TC-283 ────────────────────────────────────────────────────────────────
	// lvm-lv (block backend) + nvmeof-tcp (block protocol) → Compatible=True.
	// This test drives the full controller reconcile path with real CRDs.
	It("TC-283: TestPillarBinding_LVM_NVMeOFTCP_Compatible — lvm-lv + nvmeof-tcp reconciles Compatible=True", func() {
		const (
			poolName     = "e32-lvm-nvme-pool"
			protocolName = "e32-lvm-nvme-proto"
			bindingName  = "e32-lvm-nvme-binding"
		)

		By("creating a Ready lvm-lv PillarPool")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeLVMLV,
					LVM: &pillarcsiv1alpha1.LVMBackendConfig{
						VolumeGroup:      "data-vg",
						ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeLinear,
					},
				},
			},
		}
		Expect(k8sClient.Create(e32bCtx, pool)).To(Succeed())
		DeferCleanup(func() {
			deleteBindingIfExists(bindingName)
			deleteSCIfExists(bindingName)
			deletePoolIfExistsE32b(poolName)
			deleteProtocolIfExistsE32b(protocolName)
		})

		readyStatus := metav1.ConditionTrue
		pool.Status.Conditions = []metav1.Condition{
			{Type: "Ready", Status: readyStatus, Reason: "AllConditionsMet",
				Message: "pool ready", LastTransitionTime: metav1.Now()},
		}
		Expect(k8sClient.Status().Update(e32bCtx, pool)).To(Succeed())

		By("creating a Ready nvmeof-tcp PillarProtocol")
		protocol := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec: pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
			},
		}
		Expect(k8sClient.Create(e32bCtx, protocol)).To(Succeed())

		protocol.Status.Conditions = []metav1.Condition{
			{Type: "Ready", Status: readyStatus, Reason: "AllConditionsMet",
				Message: "protocol ready", LastTransitionTime: metav1.Now()},
		}
		Expect(k8sClient.Status().Update(e32bCtx, protocol)).To(Succeed())

		By("creating a PillarBinding for lvm-lv pool + nvmeof-tcp protocol")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(e32bCtx, binding)).To(Succeed())

		reconciler := &PillarBindingReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		bindingNN := types.NamespacedName{Name: bindingName}

		By("reconciling twice: first adds finalizer, second evaluates compatibility")
		for range 2 {
			_, err := reconciler.Reconcile(e32bCtx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())
		}

		By("verifying Compatible=True")
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(e32bCtx, bindingNN, fetched)).To(Succeed())
		compatCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionCompatible)
		Expect(compatCond).NotTo(BeNil())
		Expect(compatCond.Status).To(Equal(metav1.ConditionTrue),
			"lvm-lv + nvmeof-tcp is a valid block+block pairing → Compatible=True")
	})

	// TC-284 is tested in internal/webhook/v1alpha1/pillarbinding_webhook_test.go
	// via the full envtest webhook server.  That suite registers PillarBinding
	// ValidateCreate which fetches the referenced pool and protocol and rejects
	// incompatible pairings.  The controller suite does not run admission webhooks
	// so the rejection cannot be observed here.
})
