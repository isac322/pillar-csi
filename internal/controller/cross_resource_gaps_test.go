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

// cross_resource_gaps_test.go — I-NEW-13, I-NEW-14, and I-NEW-16 integration tests.
//
// This file covers PRD-gap tests identified in the cross-resource and integration
// gap analysis:
//
//   - I-NEW-13: Multi-pool failover — cross-binding pool isolation
//     Verifies that independent PillarBindings referencing different PillarPools
//     have fully isolated lifecycle states: one pool going not-ready does not
//     degrade bindings that reference a separate pool.
//
//   - I-NEW-14: SSOT enforcement — controller-level spec constraint verification
//     Verifies that conditions for pools sharing the same target remain independent,
//     that reconciler enforces backend-protocol compatibility even when the webhook
//     is bypassed, and that conditions contain no duplicates after repeated reconciles.
//
//   - I-NEW-16: Reconciler idempotency — repeated-reconcile stability
//     Verifies that calling Reconcile() N times on an unchanged resource yields
//     the same conditions and exactly one derived resource (StorageClass) with
//     no duplicates or unbounded increments.
//
// All tests use the shared envtest environment (k8sClient) set up in suite_test.go.
// Reconcilers are instantiated directly — no live controller-manager — so tests
// are deterministic and do not race with background goroutines.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// =============================================================================
// I-NEW-13: Multi-Pool Failover — Cross-Binding Pool Isolation
// =============================================================================

var _ = Describe("I-NEW-13: Multi-Pool Failover", Ordered, func() {
	var bctx context.Context

	BeforeAll(func() {
		bctx = context.Background()
	})

	// ─── shared protocol used by I-NEW-13-1 and I-NEW-13-3 ──────────────────
	const protocolMPName = "proto-mp"

	BeforeAll(func() {
		proto := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolMPName},
			Spec: pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
			},
		}
		Expect(k8sClient.Create(bctx, proto)).To(Succeed())
		// Set protocol Ready so bindings can reach Ready state.
		fetched := &pillarcsiv1alpha1.PillarProtocol{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolMPName}, fetched)).To(Succeed())
		fetched.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue,
			Reason: "TestReady", Message: "protocol ready", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())
	})

	AfterAll(func() {
		proto := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: protocolMPName}, proto); err == nil {
			controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
			_ = k8sClient.Update(bctx, proto)
			_ = k8sClient.Delete(bctx, proto)
		}
	})

	// ─── I-NEW-13-1 ─────────────────────────────────────────────────────────

	Context("I-NEW-13-1: Two bindings with different pools; one pool fails", Ordered, func() {
		const (
			poolMPAName    = "pool-mp-a"
			poolMPBName    = "pool-mp-b"
			bindingMPAName = "binding-mp-a"
			bindingMPBName = "binding-mp-b"
		)
		bindingMPANN := types.NamespacedName{Name: bindingMPAName}
		bindingMPBNN := types.NamespacedName{Name: bindingMPBName}

		// setPoolReady is a helper that sets or clears the Ready condition on a pool.
		setPoolReady := func(name string, ready metav1.ConditionStatus) {
			p := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: name}, p)).To(Succeed())
			apimeta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type: "Ready", Status: ready,
				Reason: "TestSet", Message: "set by test", LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(bctx, p)).To(Succeed())
		}

		BeforeAll(func() {
			// Create pool-mp-a (zfs-zvol).
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolMPAName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "target-mp1",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
			// Create pool-mp-b (zfs-zvol).
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolMPBName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "target-mp1",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())

			// Mark both pools Ready.
			setPoolReady(poolMPAName, metav1.ConditionTrue)
			setPoolReady(poolMPBName, metav1.ConditionTrue)

			// Create bindings.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bindingMPAName},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef: poolMPAName, ProtocolRef: protocolMPName,
				},
			})).To(Succeed())
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bindingMPBName},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef: poolMPBName, ProtocolRef: protocolMPName,
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			for _, name := range []string{bindingMPAName, bindingMPBName} {
				b := &pillarcsiv1alpha1.PillarBinding{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, b); err == nil {
					controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
					_ = k8sClient.Update(bctx, b)
					_ = k8sClient.Delete(bctx, b)
				}
				sc := &storagev1.StorageClass{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, sc); err == nil {
					_ = k8sClient.Delete(bctx, sc)
				}
			}
			for _, name := range []string{poolMPAName, poolMPBName} {
				p := &pillarcsiv1alpha1.PillarPool{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, p); err == nil {
					controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
					_ = k8sClient.Update(bctx, p)
					_ = k8sClient.Delete(bctx, p)
				}
			}
		})

		It("I-NEW-13-1 TestMultiPool_TwoBindings_OnePoolFails_OtherStaysReady: binding-mp-b remains Ready when pool-mp-a fails", func() {
			rA := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			rB := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile: add finalizers to both bindings.
			_, err := rA.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPANN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-1] binding-mp-a first reconcile")
			_, err = rB.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPBNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-1] binding-mp-b first reconcile")

			// Second reconcile: both bindings should become Ready.
			_, err = rA.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPANN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-1] binding-mp-a second reconcile")
			_, err = rB.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPBNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-1] binding-mp-b second reconcile")

			// Verify binding-mp-b is Ready.
			bmpb := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, bindingMPBNN, bmpb)).To(Succeed())
			readyB := apimeta.FindStatusCondition(bmpb.Status.Conditions, "Ready")
			Expect(readyB).NotTo(BeNil(), "[I-NEW-13-1] binding-mp-b must have Ready condition")
			Expect(readyB.Status).To(Equal(metav1.ConditionTrue),
				"[I-NEW-13-1] binding-mp-b must be Ready before failure injection")

			// Now degrade pool-mp-a.
			setPoolReady(poolMPAName, metav1.ConditionFalse)

			// Reconcile binding-mp-a: should pick up pool-mp-a's degraded state.
			_, err = rA.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPANN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-1] binding-mp-a post-failure reconcile")

			// Reconcile binding-mp-b: should be unaffected.
			_, err = rB.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPBNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-1] binding-mp-b post-failure reconcile")

			// binding-mp-a: PoolReady=False.
			bmpa := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, bindingMPANN, bmpa)).To(Succeed())
			poolReadyA := apimeta.FindStatusCondition(bmpa.Status.Conditions, "PoolReady")
			Expect(poolReadyA).NotTo(BeNil(), "[I-NEW-13-1] binding-mp-a must have PoolReady condition")
			Expect(poolReadyA.Status).To(Equal(metav1.ConditionFalse),
				"[I-NEW-13-1] binding-mp-a PoolReady must be False after pool-mp-a fails")

			// binding-mp-b: Ready=True (pool-mp-b still Ready).
			bmpbAfter := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, bindingMPBNN, bmpbAfter)).To(Succeed())
			readyBAfter := apimeta.FindStatusCondition(bmpbAfter.Status.Conditions, "Ready")
			Expect(readyBAfter).NotTo(BeNil(), "[I-NEW-13-1] binding-mp-b must have Ready condition after test")
			Expect(readyBAfter.Status).To(Equal(metav1.ConditionTrue),
				"[I-NEW-13-1] binding-mp-b must remain Ready — isolated from pool-mp-a failure")
		})
	})

	// ─── I-NEW-13-2 ─────────────────────────────────────────────────────────

	Context("I-NEW-13-2: Two pools share a target; target fails → both pools lose TargetReady", Ordered, func() {
		const (
			targetMP2Name = "target-mp2"
			poolMPCName   = "pool-mp-c"
			poolMPDName   = "pool-mp-d"
		)
		poolMPCNN := types.NamespacedName{Name: poolMPCName}
		poolMPDNN := types.NamespacedName{Name: poolMPDName}

		BeforeAll(func() {
			// Create target-mp2 as Ready.
			tgt := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: targetMP2Name},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.20", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, tgt)).To(Succeed())
			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: targetMP2Name}, fetched)).To(Succeed())
			fetched.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "target ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())

			// Create pool-mp-c and pool-mp-d both referencing target-mp2.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolMPCName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: targetMP2Name,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolMPDName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: targetMP2Name,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			for _, name := range []string{poolMPCName, poolMPDName} {
				p := &pillarcsiv1alpha1.PillarPool{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, p); err == nil {
					controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
					_ = k8sClient.Update(bctx, p)
					_ = k8sClient.Delete(bctx, p)
				}
			}
			tgt := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: targetMP2Name}, tgt); err == nil {
				controllerutil.RemoveFinalizer(tgt, pillarTargetFinalizer)
				_ = k8sClient.Update(bctx, tgt)
				_ = k8sClient.Delete(bctx, tgt)
			}
		})

		It("I-NEW-13-2 TestMultiPool_SharedTarget_TargetFails_BothPoolsLoseTargetReady: both pools TargetReady=False when shared target degrades", func() {
			rC := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			rD := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// Finalizer reconcile for each pool.
			_, err := rC.Reconcile(bctx, reconcile.Request{NamespacedName: poolMPCNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-2] pool-mp-c finalizer reconcile")
			_, err = rD.Reconcile(bctx, reconcile.Request{NamespacedName: poolMPDNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-2] pool-mp-d finalizer reconcile")

			// Normal reconcile — target is Ready, both pools should get TargetReady=True.
			_, err = rC.Reconcile(bctx, reconcile.Request{NamespacedName: poolMPCNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-2] pool-mp-c normal reconcile (target Ready)")
			_, err = rD.Reconcile(bctx, reconcile.Request{NamespacedName: poolMPDNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-2] pool-mp-d normal reconcile (target Ready)")

			// Degrade the shared target.
			tgt := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: targetMP2Name}, tgt)).To(Succeed())
			apimeta.SetStatusCondition(&tgt.Status.Conditions, metav1.Condition{
				Type: "Ready", Status: metav1.ConditionFalse,
				Reason: "TestDegraded", Message: "target degraded by test", LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(bctx, tgt)).To(Succeed())

			// Re-reconcile both pools.
			_, err = rC.Reconcile(bctx, reconcile.Request{NamespacedName: poolMPCNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-2] pool-mp-c reconcile after target degraded")
			_, err = rD.Reconcile(bctx, reconcile.Request{NamespacedName: poolMPDNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-2] pool-mp-d reconcile after target degraded")

			// Both pools must now show TargetReady=False.
			poolC := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolMPCNN, poolC)).To(Succeed())
			condC := apimeta.FindStatusCondition(poolC.Status.Conditions, "TargetReady")
			Expect(condC).NotTo(BeNil(), "[I-NEW-13-2] pool-mp-c must have TargetReady condition")
			Expect(condC.Status).To(Equal(metav1.ConditionFalse),
				"[I-NEW-13-2] pool-mp-c TargetReady must be False after shared target degraded")

			poolD := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolMPDNN, poolD)).To(Succeed())
			condD := apimeta.FindStatusCondition(poolD.Status.Conditions, "TargetReady")
			Expect(condD).NotTo(BeNil(), "[I-NEW-13-2] pool-mp-d must have TargetReady condition")
			Expect(condD.Status).To(Equal(metav1.ConditionFalse),
				"[I-NEW-13-2] pool-mp-d TargetReady must be False after shared target degraded")
		})
	})

	// ─── I-NEW-13-3 ─────────────────────────────────────────────────────────

	Context("I-NEW-13-3: Pool recovery restores binding Ready", Ordered, func() {
		const (
			poolMPEName    = "pool-mp-e"
			protocolMP2    = "proto-mp2"
			bindingMPEName = "binding-mp-e"
		)
		poolMPENN    := types.NamespacedName{Name: poolMPEName}
		bindingMPENN := types.NamespacedName{Name: bindingMPEName}

		BeforeAll(func() {
			// Create a second protocol for isolation.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: protocolMP2},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
			})).To(Succeed())
			fetchedProto := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolMP2}, fetchedProto)).To(Succeed())
			fetchedProto.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "proto2 ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetchedProto)).To(Succeed())

			// Create pool-mp-e with not-ready status.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolMPEName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "target-mpe",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
			// Explicitly mark pool as not-ready.
			pmpeFetched := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolMPENN, pmpeFetched)).To(Succeed())
			pmpeFetched.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse,
				Reason: "TestNotReady", Message: "pool initially not ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, pmpeFetched)).To(Succeed())

			// Create the binding.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bindingMPEName},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef: poolMPEName, ProtocolRef: protocolMP2,
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			b := &pillarcsiv1alpha1.PillarBinding{}
			if err := k8sClient.Get(bctx, bindingMPENN, b); err == nil {
				controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
				_ = k8sClient.Update(bctx, b)
				_ = k8sClient.Delete(bctx, b)
			}
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingMPEName}, sc); err == nil {
				_ = k8sClient.Delete(bctx, sc)
			}
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, poolMPENN, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: protocolMP2}, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
				_ = k8sClient.Update(bctx, proto)
				_ = k8sClient.Delete(bctx, proto)
			}
		})

		It("I-NEW-13-3 TestMultiPool_PoolRecovery_BindingRegainsReady: binding re-gains Ready after pool recovers", func() {
			r := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile: add finalizer.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPENN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-3] finalizer reconcile")

			// Second reconcile: pool is not-ready → PoolReady=False.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPENN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-3] reconcile with not-ready pool")

			bmpeBefore := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, bindingMPENN, bmpeBefore)).To(Succeed())
			poolReadyBefore := apimeta.FindStatusCondition(bmpeBefore.Status.Conditions, "PoolReady")
			Expect(poolReadyBefore).NotTo(BeNil(), "[I-NEW-13-3] binding must have PoolReady condition")
			Expect(poolReadyBefore.Status).To(Equal(metav1.ConditionFalse),
				"[I-NEW-13-3] PoolReady must be False while pool is not-ready")

			// Recover pool-mp-e: set Ready=True.
			pmpe := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolMPENN, pmpe)).To(Succeed())
			apimeta.SetStatusCondition(&pmpe.Status.Conditions, metav1.Condition{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "Recovered", Message: "pool recovered", LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(bctx, pmpe)).To(Succeed())

			// Reconcile binding again: should pick up pool recovery.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: bindingMPENN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-13-3] reconcile after pool recovery")

			bmpeAfter := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, bindingMPENN, bmpeAfter)).To(Succeed())
			poolReadyAfter := apimeta.FindStatusCondition(bmpeAfter.Status.Conditions, "PoolReady")
			Expect(poolReadyAfter).NotTo(BeNil(), "[I-NEW-13-3] binding must have PoolReady condition after recovery")
			Expect(poolReadyAfter.Status).To(Equal(metav1.ConditionTrue),
				"[I-NEW-13-3] PoolReady must be True after pool-mp-e recovers")
		})
	})
})

// =============================================================================
// I-NEW-14: SSOT Enforcement — Controller-Level Spec Constraint Verification
// =============================================================================

var _ = Describe("I-NEW-14: SSOT Enforcement", Ordered, func() {
	var bctx context.Context

	BeforeAll(func() {
		bctx = context.Background()
	})

	// ─── I-NEW-14-1 ─────────────────────────────────────────────────────────

	Context("I-NEW-14-1: Two pools same target — independent conditions", Ordered, func() {
		const (
			targetSSOT1Name = "target-ssot1"
			poolSSOTAName   = "pool-ssot-a"
			poolSSOTBName   = "pool-ssot-b"
		)
		poolSSOTANN := types.NamespacedName{Name: poolSSOTAName}
		poolSSOTBNN := types.NamespacedName{Name: poolSSOTBName}

		BeforeAll(func() {
			// Create target-ssot1 as Ready.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: targetSSOT1Name},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.30", Port: 9500},
				},
			})).To(Succeed())
			tgt := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: targetSSOT1Name}, tgt)).To(Succeed())
			tgt.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "ssot1 target ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, tgt)).To(Succeed())

			// Create both pools referencing target-ssot1.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolSSOTAName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: targetSSOT1Name,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolSSOTBName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: targetSSOT1Name,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			for _, name := range []string{poolSSOTAName, poolSSOTBName} {
				p := &pillarcsiv1alpha1.PillarPool{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, p); err == nil {
					controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
					_ = k8sClient.Update(bctx, p)
					_ = k8sClient.Delete(bctx, p)
				}
			}
			tgt := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: targetSSOT1Name}, tgt); err == nil {
				controllerutil.RemoveFinalizer(tgt, pillarTargetFinalizer)
				_ = k8sClient.Update(bctx, tgt)
				_ = k8sClient.Delete(bctx, tgt)
			}
		})

		It("I-NEW-14-1 TestMultiPool_TwoPoolsSameTarget_IndependentConditions: each pool has own condition set with no cross-contamination", func() {
			rA := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			rB := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// Two reconciles each: finalizer + normal.
			for i := 0; i < 2; i++ {
				_, err := rA.Reconcile(bctx, reconcile.Request{NamespacedName: poolSSOTANN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-14-1] pool-ssot-a reconcile %d", i+1)
				_, err = rB.Reconcile(bctx, reconcile.Request{NamespacedName: poolSSOTBNN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-14-1] pool-ssot-b reconcile %d", i+1)
			}

			poolA := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolSSOTANN, poolA)).To(Succeed())
			poolB := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolSSOTBNN, poolB)).To(Succeed())

			// Each pool should have TargetReady=True.
			condA := apimeta.FindStatusCondition(poolA.Status.Conditions, "TargetReady")
			Expect(condA).NotTo(BeNil(), "[I-NEW-14-1] pool-ssot-a must have TargetReady condition")
			Expect(condA.Status).To(Equal(metav1.ConditionTrue),
				"[I-NEW-14-1] pool-ssot-a TargetReady must be True (target is Ready)")
			condB := apimeta.FindStatusCondition(poolB.Status.Conditions, "TargetReady")
			Expect(condB).NotTo(BeNil(), "[I-NEW-14-1] pool-ssot-b must have TargetReady condition")
			Expect(condB.Status).To(Equal(metav1.ConditionTrue),
				"[I-NEW-14-1] pool-ssot-b TargetReady must be True (same target, same readiness)")

			// Conditions are independent: no type appears more than once in either pool.
			checkNoDuplicateConditions(poolA.Status.Conditions, "pool-ssot-a", "[I-NEW-14-1]")
			checkNoDuplicateConditions(poolB.Status.Conditions, "pool-ssot-b", "[I-NEW-14-1]")
		})
	})

	// ─── I-NEW-14-2 ─────────────────────────────────────────────────────────

	Context("I-NEW-14-2: Incompatible pool+protocol → Compatible=False at reconcile level", Ordered, func() {
		const (
			poolSSOT2Name     = "pool-ssot2"
			protocolSSOT2Name = "protocol-ssot2"
			bindingSSOT2Name  = "binding-ssot2"
		)
		bindingSSOT2NN := types.NamespacedName{Name: bindingSSOT2Name}

		BeforeAll(func() {
			// Create pool with lvm-lv backend, mark Ready.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolSSOT2Name},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "ssot2-target",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeLVMLV},
				},
			})).To(Succeed())
			pssot2 := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolSSOT2Name}, pssot2)).To(Succeed())
			pssot2.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "ssot2 pool ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, pssot2)).To(Succeed())

			// Create protocol with nfs type (incompatible with lvm-lv), mark Ready.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: protocolSSOT2Name},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNFS},
			})).To(Succeed())
			proSSOT2 := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolSSOT2Name}, proSSOT2)).To(Succeed())
			proSSOT2.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "ssot2 proto ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, proSSOT2)).To(Succeed())

			// Create binding (lvm-lv + nfs = incompatible, but webhook is not active in this suite).
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bindingSSOT2Name},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef: poolSSOT2Name, ProtocolRef: protocolSSOT2Name,
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			b := &pillarcsiv1alpha1.PillarBinding{}
			if err := k8sClient.Get(bctx, bindingSSOT2NN, b); err == nil {
				controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
				_ = k8sClient.Update(bctx, b)
				_ = k8sClient.Delete(bctx, b)
			}
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingSSOT2Name}, sc); err == nil {
				_ = k8sClient.Delete(bctx, sc)
			}
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: poolSSOT2Name}, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: protocolSSOT2Name}, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
				_ = k8sClient.Update(bctx, proto)
				_ = k8sClient.Delete(bctx, proto)
			}
		})

		It("I-NEW-14-2 TestPillarBindingReconciler_IncompatiblePair_CompatibleFalse: reconciler sets Compatible=False for lvm-lv+nfs binding", func() {
			r := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// Reconcile 1: add finalizer.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: bindingSSOT2NN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-14-2] first reconcile (finalizer)")

			// Reconcile 2: main logic — lvm-lv + nfs is incompatible.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: bindingSSOT2NN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-14-2] second reconcile (compatibility check)")

			b := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, bindingSSOT2NN, b)).To(Succeed())

			compatCond := apimeta.FindStatusCondition(b.Status.Conditions, "Compatible")
			Expect(compatCond).NotTo(BeNil(), "[I-NEW-14-2] Compatible condition must be set")
			Expect(compatCond.Status).To(Equal(metav1.ConditionFalse),
				"[I-NEW-14-2] Compatible must be False: lvm-lv backend is incompatible with nfs protocol")
			Expect(compatCond.Message).To(ContainSubstring("lvm-lv"),
				"[I-NEW-14-2] Compatible message must include backend type")
			Expect(compatCond.Message).To(ContainSubstring("nfs"),
				"[I-NEW-14-2] Compatible message must include protocol type")

			// Ready must also be False.
			readyCond := apimeta.FindStatusCondition(b.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil(), "[I-NEW-14-2] Ready condition must be set")
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse),
				"[I-NEW-14-2] Ready must be False when Compatible=False")
		})
	})

	// ─── I-NEW-14-3 ─────────────────────────────────────────────────────────

	Context("I-NEW-14-3: Pool condition stability — no duplicates on repeated reconcile", Ordered, func() {
		const poolSSOT3Name = "pool-ssot3"
		poolSSOT3NN := types.NamespacedName{Name: poolSSOT3Name}

		BeforeAll(func() {
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolSSOT3Name},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "missing-target-ssot3",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, poolSSOT3NN, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
		})

		It("I-NEW-14-3 TestPillarPoolReconciler_ConditionStability_NoDuplicateOnSameStatus: 3 reconciles produce no duplicate conditions", func() {
			r := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			for i := 0; i < 3; i++ {
				_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: poolSSOT3NN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-14-3] reconcile %d", i+1)
			}

			pool := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolSSOT3NN, pool)).To(Succeed())

			checkNoDuplicateConditions(pool.Status.Conditions, poolSSOT3Name, "[I-NEW-14-3]")
		})
	})
})

// =============================================================================
// I-NEW-16: Reconciler Idempotency — Repeated-Reconcile Stability
// =============================================================================

var _ = Describe("I-NEW-16: Reconciler Idempotency", Ordered, func() {
	var bctx context.Context

	BeforeAll(func() {
		bctx = context.Background()
	})

	// ─── I-NEW-16-1 ─────────────────────────────────────────────────────────

	Context("I-NEW-16-1: PillarTarget — 5 reconciles, stable conditions", Ordered, func() {
		const targetIdem1Name = "target-idem1"
		targetIdem1NN := types.NamespacedName{Name: targetIdem1Name}

		BeforeAll(func() {
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: targetIdem1Name},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.40", Port: 9500},
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			tgt := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, targetIdem1NN, tgt); err == nil {
				controllerutil.RemoveFinalizer(tgt, pillarTargetFinalizer)
				_ = k8sClient.Update(bctx, tgt)
				_ = k8sClient.Delete(bctx, tgt)
			}
		})

		It("I-NEW-16-1 TestIdempotency_PillarTarget_MultipleReconciles_StableConditions: 5 reconciles produce no duplicate conditions", func() {
			r := &PillarTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			for i := 0; i < 5; i++ {
				_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: targetIdem1NN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-16-1] reconcile %d", i+1)
			}

			tgt := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, targetIdem1NN, tgt)).To(Succeed())

			checkNoDuplicateConditions(tgt.Status.Conditions, targetIdem1Name, "[I-NEW-16-1]")

			// Condition count must be stable (capture after reconcile 3 and compare to 5).
			countAfterThird := len(tgt.Status.Conditions)

			// Two more reconciles should not change the count.
			for i := 5; i < 7; i++ {
				_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: targetIdem1NN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-16-1] reconcile %d (stability check)", i+1)
			}
			tgtAfter := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, targetIdem1NN, tgtAfter)).To(Succeed())
			Expect(len(tgtAfter.Status.Conditions)).To(Equal(countAfterThird),
				"[I-NEW-16-1] condition count must be stable across repeated reconciles")
		})
	})

	// ─── I-NEW-16-2 ─────────────────────────────────────────────────────────

	Context("I-NEW-16-2: PillarPool — 5 reconciles, no duplicate conditions", Ordered, func() {
		const (
			poolIdem2Name   = "pool-idem2"
			targetIdem2Name = "target-idem2"
		)
		poolIdem2NN := types.NamespacedName{Name: poolIdem2Name}

		BeforeAll(func() {
			// Create a target that is not-ready (to keep conditions deterministic).
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: targetIdem2Name},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.41", Port: 9500},
				},
			})).To(Succeed())
			tgt := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: targetIdem2Name}, tgt)).To(Succeed())
			tgt.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse,
				Reason: "TestNotReady", Message: "target not ready for idempotency test", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, tgt)).To(Succeed())

			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolIdem2Name},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: targetIdem2Name,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, poolIdem2NN, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
			tgt := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: targetIdem2Name}, tgt); err == nil {
				controllerutil.RemoveFinalizer(tgt, pillarTargetFinalizer)
				_ = k8sClient.Update(bctx, tgt)
				_ = k8sClient.Delete(bctx, tgt)
			}
		})

		It("I-NEW-16-2 TestIdempotency_PillarPool_MultipleReconciles_NoDuplicateConditions: 5 reconciles never duplicate condition types", func() {
			r := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			for i := 0; i < 5; i++ {
				_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: poolIdem2NN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-16-2] reconcile %d", i+1)
			}

			pool := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, poolIdem2NN, pool)).To(Succeed())

			checkNoDuplicateConditions(pool.Status.Conditions, poolIdem2Name, "[I-NEW-16-2]")

			// The total number of conditions must not exceed the known set
			// (TargetReady, PoolDiscovered, BackendSupported, Ready = 4 at most).
			Expect(len(pool.Status.Conditions)).To(BeNumerically("<=", 4),
				"[I-NEW-16-2] pool conditions must not exceed 4 (one per known type)")
		})
	})

	// ─── I-NEW-16-3 ─────────────────────────────────────────────────────────

	Context("I-NEW-16-3: PillarBinding — 5 reconciles, exactly one StorageClass", Ordered, func() {
		const (
			poolIdem3Name     = "pool-idem3"
			protocolIdem3Name = "protocol-idem3"
			bindingIdem3Name  = "binding-idem3"
		)
		bindingIdem3NN := types.NamespacedName{Name: bindingIdem3Name}

		BeforeAll(func() {
			// Create pool, mark Ready.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolIdem3Name},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "target-idem3",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
			pidem3 := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolIdem3Name}, pidem3)).To(Succeed())
			pidem3.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "idem3 pool ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, pidem3)).To(Succeed())

			// Create protocol, mark Ready.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: protocolIdem3Name},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
			})).To(Succeed())
			prIdem3 := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolIdem3Name}, prIdem3)).To(Succeed())
			prIdem3.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "idem3 proto ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, prIdem3)).To(Succeed())

			// Create binding.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bindingIdem3Name},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef: poolIdem3Name, ProtocolRef: protocolIdem3Name,
				},
			})).To(Succeed())
		})

		AfterAll(func() {
			b := &pillarcsiv1alpha1.PillarBinding{}
			if err := k8sClient.Get(bctx, bindingIdem3NN, b); err == nil {
				controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
				_ = k8sClient.Update(bctx, b)
				_ = k8sClient.Delete(bctx, b)
			}
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingIdem3Name}, sc); err == nil {
				_ = k8sClient.Delete(bctx, sc)
			}
			for _, name := range []string{poolIdem3Name} {
				p := &pillarcsiv1alpha1.PillarPool{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, p); err == nil {
					controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
					_ = k8sClient.Update(bctx, p)
					_ = k8sClient.Delete(bctx, p)
				}
			}
			for _, name := range []string{protocolIdem3Name} {
				proto := &pillarcsiv1alpha1.PillarProtocol{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, proto); err == nil {
					controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
					_ = k8sClient.Update(bctx, proto)
					_ = k8sClient.Delete(bctx, proto)
				}
			}
		})

		It("I-NEW-16-3 TestIdempotency_PillarBinding_MultipleReconciles_SingleStorageClass: 5 reconciles yield exactly 1 StorageClass", func() {
			r := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			for i := 0; i < 5; i++ {
				_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: bindingIdem3NN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-16-3] reconcile %d", i+1)
			}

			// Exactly one StorageClass named "binding-idem3" must exist.
			sc := &storagev1.StorageClass{}
			err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingIdem3Name}, sc)
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-16-3] StorageClass must exist")
			Expect(sc.Provisioner).To(Equal(pillarCSIProvisioner),
				"[I-NEW-16-3] StorageClass provisioner must match pillar-csi")

			// The binding's conditions must have no duplicates.
			b := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, bindingIdem3NN, b)).To(Succeed())
			checkNoDuplicateConditions(b.Status.Conditions, bindingIdem3Name, "[I-NEW-16-3]")
		})
	})

	// ─── I-NEW-16-4 ─────────────────────────────────────────────────────────

	Context("I-NEW-16-4: PillarProtocol — 5 reconciles, stable bindingCount", Ordered, func() {
		const (
			protoIdem4Name    = "proto-idem4"
			bindingIdem4AName = "binding-idem4a"
			bindingIdem4BName = "binding-idem4b"
			// poolIdem4 is also needed for the bindings to be valid.
			poolIdem4Name = "pool-idem4"
		)
		protoIdem4NN := types.NamespacedName{Name: protoIdem4Name}

		BeforeAll(func() {
			// Create pool, mark Ready.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolIdem4Name},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "target-idem4",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			})).To(Succeed())
			pidem4 := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolIdem4Name}, pidem4)).To(Succeed())
			pidem4.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestReady", Message: "idem4 pool ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, pidem4)).To(Succeed())

			// Create PillarProtocol.
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: protoIdem4Name},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
			})).To(Succeed())

			// Create two bindings referencing proto-idem4.
			for _, name := range []string{bindingIdem4AName, bindingIdem4BName} {
				Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarBinding{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Spec: pillarcsiv1alpha1.PillarBindingSpec{
						PoolRef: poolIdem4Name, ProtocolRef: protoIdem4Name,
					},
				})).To(Succeed())
			}
		})

		AfterAll(func() {
			for _, name := range []string{bindingIdem4AName, bindingIdem4BName} {
				b := &pillarcsiv1alpha1.PillarBinding{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, b); err == nil {
					controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
					_ = k8sClient.Update(bctx, b)
					_ = k8sClient.Delete(bctx, b)
				}
				sc := &storagev1.StorageClass{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, sc); err == nil {
					_ = k8sClient.Delete(bctx, sc)
				}
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(bctx, protoIdem4NN, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
				_ = k8sClient.Update(bctx, proto)
				_ = k8sClient.Delete(bctx, proto)
			}
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: poolIdem4Name}, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
		})

		It("I-NEW-16-4 TestIdempotency_PillarProtocol_MultipleReconciles_StableBindingCount: bindingCount=2 after 5 reconciles with no accumulation", func() {
			rProto := &PillarProtocolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// Reconcile the protocol 5 times.
			for i := 0; i < 5; i++ {
				_, err := rProto.Reconcile(bctx, reconcile.Request{NamespacedName: protoIdem4NN})
				Expect(err).NotTo(HaveOccurred(), "[I-NEW-16-4] proto reconcile %d", i+1)
			}

			proto := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, protoIdem4NN, proto)).To(Succeed())

			// bindingCount must equal 2 (the two bindings created in BeforeAll).
			Expect(proto.Status.BindingCount).To(Equal(int32(2)),
				"[I-NEW-16-4] bindingCount must be exactly 2 — must not accumulate on repeated reconciles")
		})
	})
})

// =============================================================================
// helpers shared by this file
// =============================================================================

// checkNoDuplicateConditions asserts that no condition Type appears more than
// once in the given slice. It emits clear failure messages that include the
// resource name and a caller-supplied label prefix for easy diagnosis.
func checkNoDuplicateConditions(conditions []metav1.Condition, resourceName, label string) {
	seen := make(map[string]int, len(conditions))
	for _, c := range conditions {
		seen[c.Type]++
	}
	for condType, count := range seen {
		Expect(count).To(Equal(1),
			"%s resource %q: condition type %q appears %d times (must be exactly 1)",
			label, resourceName, condType, count)
	}
}

// Ensure errors package is used even if a specific path doesn't call it
// directly, preventing unused-import compilation errors.
var _ = errors.IsNotFound
