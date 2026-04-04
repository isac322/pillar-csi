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

// webhook_chain_test.go — I-NEW-15 webhook validation chain integration tests.
//
// These tests verify that the four webhook validators (Target, Pool, Protocol,
// Binding) operate correctly in sequence, modelling the real-world creation
// order that a user follows when setting up pillar-csi:
//
//   PillarTarget → PillarPool → PillarProtocol → PillarBinding
//
// Two scenarios are covered:
//
//   I-NEW-15-1: Happy path — all validators pass when resources are valid and
//     the backend-protocol combination is compatible (zfs-zvol + nvmeof-tcp).
//
//   I-NEW-15-2: Failure path — PillarBindingCustomValidator.ValidateCreate
//     rejects a binding whose pool (lvm-lv) and protocol (nfs) are incompatible,
//     even though the individual Target / Pool / Protocol validators each pass.
//
// Tests call the validators directly (no HTTP webhook server needed) and rely
// on k8sClient — provided by the suite — to pre-create PillarPool and
// PillarProtocol objects so that the Binding validator can look them up.
package v1alpha1

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// chainPoolFinalizer and chainProtocolFinalizer are copied from the production
// controller constants because the webhook tests cannot import the controller
// package (circular import).  They are used only for cleanup in AfterAll.
const (
	chainPoolFinalizer     = "pillar-csi.bhyoo.com/pool-protection"
	chainProtocolFinalizer = "pillar-csi.bhyoo.com/protocol-protection"
)

var _ = Describe("I-NEW-15: Webhook Validation Chain", Ordered, func() {
	var wctx context.Context

	BeforeAll(func() {
		wctx = context.Background()
	})

	// ─── I-NEW-15-1: Happy-path full stack ───────────────────────────────────

	Context("I-NEW-15-1: Full stack creation chain — all validators pass", Ordered, func() {
		const (
			chainPoolName     = "chain-pool-zvol"
			chainProtocolName = "chain-proto-nvmeof"
		)

		BeforeAll(func() {
			// Pre-create PillarPool (zfs-zvol) in envtest so the Binding validator
			// can look it up during ValidateCreate.
			Expect(k8sClient.Create(wctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: chainPoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "chain-target",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())

			// Pre-create PillarProtocol (nvmeof-tcp) in envtest.
			Expect(k8sClient.Create(wctx, &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: chainProtocolName},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
			})).To(Succeed())
		})

		AfterAll(func() {
			pool := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(wctx, types.NamespacedName{Name: chainPoolName}, pool); err == nil {
				controllerutil.RemoveFinalizer(pool, chainPoolFinalizer)
				_ = k8sClient.Update(wctx, pool)
				_ = k8sClient.Delete(wctx, pool)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(wctx, types.NamespacedName{Name: chainProtocolName}, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, chainProtocolFinalizer)
				_ = k8sClient.Update(wctx, proto)
				_ = k8sClient.Delete(wctx, proto)
			}
		})

		It("I-NEW-15-1 TestWebhookChain_FullStack_CreateAndValidate: each layer's ValidateCreate passes for a valid full-stack config", func() {
			// ── Step 1: PillarTarget webhook ─────────────────────────────────
			targetValidator := PillarTargetCustomValidator{}
			target := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "chain-target"},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "10.0.0.50", Port: 9500},
				},
			}
			warnings, err := targetValidator.ValidateCreate(context.Background(), target)
			Expect(err).NotTo(HaveOccurred(),
				"[I-NEW-15-1] Step 1 — PillarTarget ValidateCreate must pass for a valid external spec")
			Expect(warnings).To(BeNil(),
				"[I-NEW-15-1] Step 1 — no warnings expected for a valid PillarTarget")

			// ── Step 2: PillarPool webhook ───────────────────────────────────
			poolValidator := PillarPoolCustomValidator{}
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: chainPoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "chain-target",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			}
			warnings, err = poolValidator.ValidateCreate(context.Background(), pool)
			Expect(err).NotTo(HaveOccurred(),
				"[I-NEW-15-1] Step 2 — PillarPool ValidateCreate must pass for a valid pool spec")
			Expect(warnings).To(BeNil(),
				"[I-NEW-15-1] Step 2 — no warnings expected for a valid PillarPool")

			// ── Step 3: PillarBinding webhook ────────────────────────────────
			// The binding validator looks up the pool and protocol from k8sClient.
			// Both were pre-created in BeforeAll, so compatibility can be checked.
			bindingValidator := PillarBindingCustomValidator{Client: k8sClient}
			binding := &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "chain-binding"},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef: chainPoolName, ProtocolRef: chainProtocolName,
				},
			}
			warnings, err = bindingValidator.ValidateCreate(wctx, binding)
			Expect(err).NotTo(HaveOccurred(),
				"[I-NEW-15-1] Step 3 — PillarBinding ValidateCreate must pass for zfs-zvol+nvmeof-tcp (compatible)")
			Expect(warnings).To(BeNil(),
				"[I-NEW-15-1] Step 3 — no warnings expected for a compatible binding")
		})
	})

	// ─── I-NEW-15-2: Failure path — incompatible binding rejected ────────────

	Context("I-NEW-15-2: Binding webhook rejects incompatible pool+protocol combo", Ordered, func() {
		const (
			incompatPoolName  = "chain-pool-lvm"
			incompatProtoName = "chain-proto-nfs"
		)

		BeforeAll(func() {
			// Pre-create PillarPool (lvm-lv) in envtest.
			Expect(k8sClient.Create(wctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: incompatPoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "incompat-target",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeLVMLV},
				},
			})).To(Succeed())

			// Pre-create PillarProtocol (nfs) in envtest.
			Expect(k8sClient.Create(wctx, &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: incompatProtoName},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNFS},
			})).To(Succeed())
		})

		AfterAll(func() {
			pool := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(wctx, types.NamespacedName{Name: incompatPoolName}, pool); err == nil {
				controllerutil.RemoveFinalizer(pool, chainPoolFinalizer)
				_ = k8sClient.Update(wctx, pool)
				_ = k8sClient.Delete(wctx, pool)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(wctx, types.NamespacedName{Name: incompatProtoName}, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, chainProtocolFinalizer)
				_ = k8sClient.Update(wctx, proto)
				_ = k8sClient.Delete(wctx, proto)
			}
		})

		It("I-NEW-15-2 TestWebhookChain_BindingIncompatible_WebhookRejects: ValidateCreate returns error for lvm-lv+nfs binding", func() {
			validator := PillarBindingCustomValidator{Client: k8sClient}
			binding := &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "chain-binding-incompat"},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef: incompatPoolName, ProtocolRef: incompatProtoName,
				},
			}

			_, err := validator.ValidateCreate(wctx, binding)
			Expect(err).To(HaveOccurred(),
				"[I-NEW-15-2] PillarBinding ValidateCreate must reject lvm-lv+nfs (incompatible combination)")
			Expect(err.Error()).To(ContainSubstring("incompatible"),
				"[I-NEW-15-2] error message must indicate incompatibility")
		})
	})
})
