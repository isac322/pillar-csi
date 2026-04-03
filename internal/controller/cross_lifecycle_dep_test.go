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

// cross_lifecycle_dep_test.go — E26.1 and E26.2 cross-CRD lifecycle tests.
//
// E26.1: Dependency Ordering — when an upstream CRD is missing or Not-Ready,
// the downstream CRD's reconciler sets the dependent condition to False.
//
//   - E26.1.1: PillarPool with a missing PillarTarget → TargetReady=False, Reason=TargetNotFound.
//   - E26.1.2: PillarPool with a Not-Ready PillarTarget → TargetReady=False, Reason=TargetNotReady.
//   - E26.1.3: PillarPool with a Ready PillarTarget → TargetReady=True.
//   - E26.1.4: PillarBinding with a missing PillarPool → PoolReady=False, Reason=PoolNotFound.
//   - E26.1.5: PillarBinding with a Not-Ready PillarPool → PoolReady=False, Reason=PoolNotReady.
//   - E26.1.6: PillarBinding with a missing PillarProtocol → ProtocolValid=False, Reason=ProtocolNotFound.
//   - E26.1.7: PillarBinding with both Pool and Protocol missing → PoolReady=False + Ready=False.
//   - E26.1.8: PillarBinding with both Pool Ready and Protocol Ready → Ready=True + StorageClass created.
//
// E26.2: Cascading Status Updates — when an upstream CRD's status changes,
// re-reconciling the downstream CRD propagates the change to its conditions.
//
//   - E26.2.1: PillarTarget Ready→False → Pool reconcile sets TargetReady=False.
//   - E26.2.2: PillarTarget Ready→True (recovery) → Pool reconcile restores TargetReady=True.
//   - E26.2.3: PillarPool Ready→False → Binding reconcile sets PoolReady=False.
//   - E26.2.4: PillarProtocol Ready→False → Binding reconcile sets ProtocolValid=False.
//   - E26.2.5: Full chain recovery: Target→True cascades through Pool→Binding.
//   - E26.2.6: Pool becomes Ready → Binding reconcile creates StorageClass.
//   - E26.2.7: Binding references Protocol → Protocol reconcile increments BindingCount.
//
// All tests use the shared envtest environment (k8sClient) set up in suite_test.go.
// Reconcilers are instantiated directly without a live controller manager so that
// the tests are fully deterministic (no background goroutines).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1 "k8s.io/api/storage/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// E26.1 — Dependency Ordering
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E26.1: Cross-CRD Dependency Ordering", func() {
	var bctx context.Context

	BeforeEach(func() {
		bctx = context.Background()
	})

	// ── helper utilities ────────────────────────────────────────────────────

	// setTargetReady sets or clears a Ready condition on a PillarTarget by name.
	setTargetReady := func(name string, ready metav1.ConditionStatus, reason, msg string) {
		tgt := &pillarcsiv1alpha1.PillarTarget{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: name}, tgt)).To(Succeed())
		tgt.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             ready,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(bctx, tgt)).To(Succeed())
	}

	// setPoolReady sets a Ready condition on a PillarPool by name.
	setPoolReady := func(name string, ready metav1.ConditionStatus, reason, msg string) {
		pool := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: name}, pool)).To(Succeed())
		pool.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             ready,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(bctx, pool)).To(Succeed())
	}

	// setProtocolReady sets a Ready condition on a PillarProtocol by name.
	setProtocolReady := func(name string, ready metav1.ConditionStatus, reason, msg string) {
		proto := &pillarcsiv1alpha1.PillarProtocol{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: name}, proto)).To(Succeed())
		proto.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             ready,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(bctx, proto)).To(Succeed())
	}

	// cleanupTarget removes a PillarTarget by name (strips finalizer first).
	cleanupTarget := func(name string) {
		tgt := &pillarcsiv1alpha1.PillarTarget{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, tgt); err == nil {
			controllerutil.RemoveFinalizer(tgt, pillarTargetFinalizer)
			_ = k8sClient.Update(bctx, tgt)
			_ = k8sClient.Delete(bctx, tgt)
		}
	}

	// cleanupPool removes a PillarPool by name (strips finalizer first).
	cleanupPool := func(name string) {
		pool := &pillarcsiv1alpha1.PillarPool{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, pool); err == nil {
			controllerutil.RemoveFinalizer(pool, pillarPoolFinalizer)
			_ = k8sClient.Update(bctx, pool)
			_ = k8sClient.Delete(bctx, pool)
		}
	}

	// cleanupProtocol removes a PillarProtocol by name (strips finalizer first).
	cleanupProtocol := func(name string) {
		proto := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, proto); err == nil {
			controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
			_ = k8sClient.Update(bctx, proto)
			_ = k8sClient.Delete(bctx, proto)
		}
	}

	// cleanupBinding removes a PillarBinding by name (strips finalizer first).
	cleanupBinding := func(name string) {
		b := &pillarcsiv1alpha1.PillarBinding{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, b); err == nil {
			controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
			_ = k8sClient.Update(bctx, b)
			_ = k8sClient.Delete(bctx, b)
		}
	}

	// cleanupStorageClass removes a StorageClass by name.
	cleanupSC := func(name string) {
		sc := &storagev1.StorageClass{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, sc); err == nil {
			_ = k8sClient.Delete(bctx, sc)
		}
	}

	// reconcilePool runs the PillarPoolReconciler for the named pool.
	reconcilePool := func(poolName string) {
		r := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName}})
		Expect(err).NotTo(HaveOccurred())
	}

	// reconcileBinding runs the PillarBindingReconciler for the named binding.
	reconcileBinding := func(bindingName string) {
		r := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: bindingName}})
		Expect(err).NotTo(HaveOccurred())
	}

	// ── E26.1.1 ─────────────────────────────────────────────────────────────
	// PillarPool references a PillarTarget that does not exist in the cluster.
	// After reconcile, TargetReady=False with reason TargetNotFound.
	It("E26.1.1: TestCrossLifecycle_Pool_TargetMissing_TargetReadyFalse — pool with missing target gets TargetReady=False", func() {
		const (
			poolName   = "e261-pool-target-missing"
			targetName = "e261-nonexistent-target"
		)

		By("creating a PillarPool referencing a non-existent PillarTarget")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: targetName,
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPool(poolName) })

		By("running the reconciler once to add finalizer, then again for normal reconcile")
		reconcilePool(poolName)
		reconcilePool(poolName)

		By("verifying TargetReady=False with reason TargetNotFound")
		fetched := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())

		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "TargetReady")
		Expect(cond).NotTo(BeNil(), "TargetReady condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("TargetNotFound"))
		Expect(cond.Message).To(ContainSubstring(targetName))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
		Expect(readyCond).NotTo(BeNil(), "Ready condition must be set")
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	})

	// ── E26.1.2 ─────────────────────────────────────────────────────────────
	// PillarPool references a PillarTarget that exists but has Ready=False.
	// After reconcile, TargetReady=False with reason TargetNotReady.
	It("E26.1.2: TestCrossLifecycle_Pool_TargetNotReady_TargetReadyFalse — pool with not-ready target gets TargetReady=False", func() {
		const (
			poolName   = "e261-pool-target-notready"
			targetName = "e261-target-notready"
		)

		By("creating a PillarTarget with Ready=False")
		tgt := &pillarcsiv1alpha1.PillarTarget{
			ObjectMeta: metav1.ObjectMeta{Name: targetName},
			Spec: pillarcsiv1alpha1.PillarTargetSpec{
				External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.100", Port: 9500},
			},
		}
		Expect(k8sClient.Create(bctx, tgt)).To(Succeed())
		DeferCleanup(func() { cleanupTarget(targetName) })

		By("setting Ready=False on the PillarTarget")
		setTargetReady(targetName, metav1.ConditionFalse, "AgentUnhealthy", "agent health check failed")

		By("creating a PillarPool referencing this not-ready target")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: targetName,
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPool(poolName) })

		By("running the reconciler (finalizer pass + normal pass)")
		reconcilePool(poolName)
		reconcilePool(poolName)

		By("verifying TargetReady=False with reason TargetNotReady")
		fetched := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())

		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "TargetReady")
		Expect(cond).NotTo(BeNil(), "TargetReady condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("TargetNotReady"))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	})

	// ── E26.1.3 ─────────────────────────────────────────────────────────────
	// PillarPool references a PillarTarget that exists and has Ready=True.
	// After reconcile, TargetReady=True.
	It("E26.1.3: TestCrossLifecycle_Pool_TargetReady_TargetReadyTrue — pool with ready target gets TargetReady=True", func() {
		const (
			poolName   = "e261-pool-target-ready"
			targetName = "e261-target-ready"
		)

		By("creating a PillarTarget and marking it Ready=True")
		tgt := &pillarcsiv1alpha1.PillarTarget{
			ObjectMeta: metav1.ObjectMeta{Name: targetName},
			Spec: pillarcsiv1alpha1.PillarTargetSpec{
				External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.101", Port: 9500},
			},
		}
		Expect(k8sClient.Create(bctx, tgt)).To(Succeed())
		DeferCleanup(func() { cleanupTarget(targetName) })

		setTargetReady(targetName, metav1.ConditionTrue, "Authenticated", "agent is healthy and mTLS authenticated")

		By("creating a PillarPool referencing this ready target")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: targetName,
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPool(poolName) })

		By("running the reconciler")
		reconcilePool(poolName)
		reconcilePool(poolName)

		By("verifying TargetReady=True")
		fetched := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())

		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "TargetReady")
		Expect(cond).NotTo(BeNil(), "TargetReady condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	// ── E26.1.4 ─────────────────────────────────────────────────────────────
	// PillarBinding references a PillarPool that does not exist.
	// After reconcile, PoolReady=False with reason PoolNotFound.
	It("E26.1.4: TestCrossLifecycle_Binding_PoolMissing_PoolReadyFalse — binding with missing pool gets PoolReady=False", func() {
		const (
			bindingName  = "e261-binding-pool-missing"
			poolName     = "e261-nonexistent-pool"
			protocolName = "e261-proto-for-pm"
		)

		By("creating a PillarBinding with a missing poolRef")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() { cleanupBinding(bindingName) })

		By("running the binding reconciler (finalizer pass + normal pass)")
		reconcileBinding(bindingName)
		reconcileBinding(bindingName)

		By("verifying PoolReady=False with reason PoolNotFound")
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())

		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(cond).NotTo(BeNil(), "PoolReady condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("PoolNotFound"))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))

		By("verifying no StorageClass was created")
		sc := &storagev1.StorageClass{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)
		Expect(err).To(HaveOccurred(), "StorageClass should not exist when pool is missing")
	})

	// ── E26.1.5 ─────────────────────────────────────────────────────────────
	// PillarBinding references a PillarPool that exists but has Ready=False.
	// After reconcile, PoolReady=False with reason PoolNotReady.
	It("E26.1.5: TestCrossLifecycle_Binding_PoolNotReady_PoolReadyFalse — binding with not-ready pool gets PoolReady=False", func() {
		const (
			bindingName  = "e261-binding-pool-notready"
			poolName     = "e261-pool-notready"
			protocolName = "e261-proto-for-pnr"
		)

		By("creating a PillarPool with Ready=False")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPool(poolName) })
		setPoolReady(poolName, metav1.ConditionFalse, "TargetNotFound", "target not found")

		By("creating a PillarBinding referencing the not-ready pool")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() { cleanupBinding(bindingName) })

		By("running the binding reconciler")
		reconcileBinding(bindingName)
		reconcileBinding(bindingName)

		By("verifying PoolReady=False with reason PoolNotReady")
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())

		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(cond).NotTo(BeNil(), "PoolReady condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("PoolNotReady"))
		Expect(cond.Message).To(ContainSubstring(poolName))
	})

	// ── E26.1.6 ─────────────────────────────────────────────────────────────
	// PillarBinding references a valid PillarPool but a missing PillarProtocol.
	// After reconcile, ProtocolValid=False with reason ProtocolNotFound.
	It("E26.1.6: TestCrossLifecycle_Binding_ProtocolMissing_ProtocolValidFalse — binding with missing protocol gets ProtocolValid=False", func() {
		const (
			bindingName  = "e261-binding-proto-missing"
			poolName     = "e261-pool-for-pm"
			protocolName = "e261-nonexistent-protocol"
		)

		By("creating a Ready PillarPool")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPool(poolName) })
		setPoolReady(poolName, metav1.ConditionTrue, "PoolReady", "pool ready")

		By("creating a PillarBinding referencing the ready pool but a non-existent protocol")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() { cleanupBinding(bindingName) })

		By("running the binding reconciler")
		reconcileBinding(bindingName)
		reconcileBinding(bindingName)

		By("verifying ProtocolValid=False with reason ProtocolNotFound")
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())

		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionProtocolValid)
		Expect(cond).NotTo(BeNil(), "ProtocolValid condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("ProtocolNotFound"))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	})

	// ── E26.1.7 ─────────────────────────────────────────────────────────────
	// PillarBinding references both a missing PillarPool and a missing PillarProtocol.
	// After reconcile, PoolReady=False and Ready=False (pool is checked first).
	It("E26.1.7: TestCrossLifecycle_Binding_BothMissing_BothConditionsFalse — binding with both missing refs gets PoolReady=False+Ready=False", func() {
		const (
			bindingName  = "e261-binding-both-missing"
			poolName     = "e261-nonexistent-pool-bm"
			protocolName = "e261-nonexistent-proto-bm"
		)

		By("creating a PillarBinding referencing two non-existent resources")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() { cleanupBinding(bindingName) })

		By("running the binding reconciler")
		reconcileBinding(bindingName)
		reconcileBinding(bindingName)

		By("verifying PoolReady=False and Ready=False")
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())

		poolCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(poolCond).NotTo(BeNil(), "PoolReady condition must be set")
		Expect(poolCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(poolCond.Reason).To(Equal("PoolNotFound"))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))

		By("verifying no StorageClass was created")
		sc := &storagev1.StorageClass{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)
		Expect(err).To(HaveOccurred(), "StorageClass should not exist when both refs are missing")
	})

	// ── E26.1.8 ─────────────────────────────────────────────────────────────
	// PillarBinding references a Ready PillarPool (zfs-zvol) and a Ready PillarProtocol (nvmeof-tcp).
	// After reconcile, Binding is Ready=True and a StorageClass is created.
	It("E26.1.8: TestCrossLifecycle_Binding_PoolReadyProtocolReady_BecomeReady — both ready → binding Ready=True + StorageClass created", func() {
		const (
			bindingName  = "e261-binding-both-ready"
			poolName     = "e261-pool-both-ready"
			protocolName = "e261-proto-both-ready"
		)

		By("creating a Ready PillarPool with zfs-zvol backend")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
					ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
				},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPool(poolName) })
		setPoolReady(poolName, metav1.ConditionTrue, "PoolReady", "all conditions satisfied")

		By("creating a Ready PillarProtocol with nvmeof-tcp type")
		proto := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
		}
		Expect(k8sClient.Create(bctx, proto)).To(Succeed())
		DeferCleanup(func() { cleanupProtocol(protocolName) })
		setProtocolReady(protocolName, metav1.ConditionTrue, "ProtocolReady", "protocol ready")

		By("creating the PillarBinding")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() {
			cleanupSC(bindingName)
			cleanupBinding(bindingName)
		})

		By("running the binding reconciler (finalizer + normal)")
		reconcileBinding(bindingName)
		reconcileBinding(bindingName)

		By("verifying PoolReady=True, ProtocolValid=True, Compatible=True, Ready=True")
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())

		poolCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(poolCond).NotTo(BeNil())
		Expect(poolCond.Status).To(Equal(metav1.ConditionTrue))

		protoCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionProtocolValid)
		Expect(protoCond).NotTo(BeNil())
		Expect(protoCond.Status).To(Equal(metav1.ConditionTrue))

		compatCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionCompatible)
		Expect(compatCond).NotTo(BeNil())
		Expect(compatCond.Status).To(Equal(metav1.ConditionTrue))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		By("verifying StorageClass was created")
		sc := &storagev1.StorageClass{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed(),
			"StorageClass %q should be created when binding is Ready", bindingName)
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// E26.2 — Cascading Status Updates
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E26.2: Cross-CRD Cascading Status Updates", func() {
	var bctx context.Context

	BeforeEach(func() {
		bctx = context.Background()
	})

	// ── shared helpers ───────────────────────────────────────────────────────

	setTargetReadyE262 := func(name string, ready metav1.ConditionStatus, reason, msg string) {
		tgt := &pillarcsiv1alpha1.PillarTarget{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: name}, tgt)).To(Succeed())
		tgt.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             ready,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(bctx, tgt)).To(Succeed())
	}

	setPoolReadyE262 := func(name string, ready metav1.ConditionStatus, reason, msg string) {
		pool := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: name}, pool)).To(Succeed())
		pool.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             ready,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(bctx, pool)).To(Succeed())
	}

	setProtocolReadyE262 := func(name string, ready metav1.ConditionStatus, reason, msg string) {
		proto := &pillarcsiv1alpha1.PillarProtocol{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: name}, proto)).To(Succeed())
		proto.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             ready,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(bctx, proto)).To(Succeed())
	}

	cleanupTargetE262 := func(name string) {
		tgt := &pillarcsiv1alpha1.PillarTarget{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, tgt); err == nil {
			controllerutil.RemoveFinalizer(tgt, pillarTargetFinalizer)
			_ = k8sClient.Update(bctx, tgt)
			_ = k8sClient.Delete(bctx, tgt)
		}
	}

	cleanupPoolE262 := func(name string) {
		pool := &pillarcsiv1alpha1.PillarPool{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, pool); err == nil {
			controllerutil.RemoveFinalizer(pool, pillarPoolFinalizer)
			_ = k8sClient.Update(bctx, pool)
			_ = k8sClient.Delete(bctx, pool)
		}
	}

	cleanupProtocolE262 := func(name string) {
		proto := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, proto); err == nil {
			controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
			_ = k8sClient.Update(bctx, proto)
			_ = k8sClient.Delete(bctx, proto)
		}
	}

	cleanupBindingE262 := func(name string) {
		b := &pillarcsiv1alpha1.PillarBinding{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, b); err == nil {
			controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
			_ = k8sClient.Update(bctx, b)
			_ = k8sClient.Delete(bctx, b)
		}
	}

	cleanupSCE262 := func(name string) {
		sc := &storagev1.StorageClass{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, sc); err == nil {
			_ = k8sClient.Delete(bctx, sc)
		}
	}

	reconcilePoolE262 := func(poolName string) {
		r := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName}})
		Expect(err).NotTo(HaveOccurred())
	}

	reconcileBindingE262 := func(bindingName string) {
		r := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: bindingName}})
		Expect(err).NotTo(HaveOccurred())
	}

	reconcileProtocolE262 := func(protoName string) {
		r := &PillarProtocolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: protoName}})
		Expect(err).NotTo(HaveOccurred())
	}

	// ── E26.2.1 ─────────────────────────────────────────────────────────────
	// PillarTarget transitions from Ready=True to Ready=False.
	// Re-reconciling the dependent PillarPool propagates TargetReady=False.
	It("E26.2.1: TestCrossLifecycle_Cascade_TargetLosesReady_PoolConditionUpdates — target→False → pool TargetReady=False", func() {
		const (
			targetName = "e262-target-loses-ready"
			poolName   = "e262-pool-target-cascade"
		)

		By("creating a Ready PillarTarget")
		tgt := &pillarcsiv1alpha1.PillarTarget{
			ObjectMeta: metav1.ObjectMeta{Name: targetName},
			Spec: pillarcsiv1alpha1.PillarTargetSpec{
				External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.110", Port: 9500},
			},
		}
		Expect(k8sClient.Create(bctx, tgt)).To(Succeed())
		DeferCleanup(func() { cleanupTargetE262(targetName) })
		setTargetReadyE262(targetName, metav1.ConditionTrue, "Authenticated", "healthy")

		By("creating a PillarPool that depends on this target")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: targetName,
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPoolE262(poolName) })

		By("running the pool reconciler so it observes the ready target")
		reconcilePoolE262(poolName)
		reconcilePoolE262(poolName)

		By("verifying TargetReady=True initially")
		fetched := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())
		initialCond := apimeta.FindStatusCondition(fetched.Status.Conditions, "TargetReady")
		Expect(initialCond).NotTo(BeNil())
		Expect(initialCond.Status).To(Equal(metav1.ConditionTrue))

		By("transitioning the PillarTarget to Ready=False")
		setTargetReadyE262(targetName, metav1.ConditionFalse, "AgentUnhealthy", "gRPC health check failed")

		By("re-reconciling the pool")
		reconcilePoolE262(poolName)

		By("verifying TargetReady=False propagated to PillarPool")
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())
		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "TargetReady")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	})

	// ── E26.2.2 ─────────────────────────────────────────────────────────────
	// PillarTarget recovers from Ready=False to Ready=True.
	// Re-reconciling the dependent PillarPool restores TargetReady=True.
	It("E26.2.2: TestCrossLifecycle_Cascade_TargetRecovery_PoolConditionRestores — target recovery → pool TargetReady=True", func() {
		const (
			targetName = "e262-target-recovers"
			poolName   = "e262-pool-recovery"
		)

		By("creating a not-ready PillarTarget")
		tgt := &pillarcsiv1alpha1.PillarTarget{
			ObjectMeta: metav1.ObjectMeta{Name: targetName},
			Spec: pillarcsiv1alpha1.PillarTargetSpec{
				External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.111", Port: 9500},
			},
		}
		Expect(k8sClient.Create(bctx, tgt)).To(Succeed())
		DeferCleanup(func() { cleanupTargetE262(targetName) })
		setTargetReadyE262(targetName, metav1.ConditionFalse, "AgentUnhealthy", "unhealthy")

		By("creating a PillarPool referencing this not-ready target")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: targetName,
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPoolE262(poolName) })

		By("reconciling to establish TargetReady=False baseline")
		reconcilePoolE262(poolName)
		reconcilePoolE262(poolName)

		fetched := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())
		baselineCond := apimeta.FindStatusCondition(fetched.Status.Conditions, "TargetReady")
		Expect(baselineCond).NotTo(BeNil())
		Expect(baselineCond.Status).To(Equal(metav1.ConditionFalse), "baseline should be TargetReady=False")

		By("recovering the PillarTarget to Ready=True")
		setTargetReadyE262(targetName, metav1.ConditionTrue, "Authenticated", "healthy again")

		By("re-reconciling the pool after target recovery")
		reconcilePoolE262(poolName)

		By("verifying TargetReady=True restored on PillarPool")
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())
		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "TargetReady")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue), "TargetReady should recover to True")
	})

	// ── E26.2.3 ─────────────────────────────────────────────────────────────
	// PillarPool transitions from Ready=True to Ready=False.
	// Re-reconciling the dependent PillarBinding propagates PoolReady=False.
	It("E26.2.3: TestCrossLifecycle_Cascade_PoolLosesReady_BindingConditionUpdates — pool→False → binding PoolReady=False", func() {
		const (
			poolName     = "e262-pool-loses-ready"
			protocolName = "e262-proto-for-cascade"
			bindingName  = "e262-binding-pool-cascade"
		)

		By("creating a Ready PillarPool")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPoolE262(poolName) })
		setPoolReadyE262(poolName, metav1.ConditionTrue, "PoolReady", "all ready")

		By("creating a Ready PillarProtocol")
		proto := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
		}
		Expect(k8sClient.Create(bctx, proto)).To(Succeed())
		DeferCleanup(func() { cleanupProtocolE262(protocolName) })
		setProtocolReadyE262(protocolName, metav1.ConditionTrue, "ProtocolReady", "ready")

		By("creating a PillarBinding referencing both")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() {
			cleanupSCE262(bindingName)
			cleanupBindingE262(bindingName)
		})

		By("reconciling binding to establish PoolReady=True baseline")
		reconcileBindingE262(bindingName)
		reconcileBindingE262(bindingName)

		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())
		poolCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(poolCond).NotTo(BeNil())
		Expect(poolCond.Status).To(Equal(metav1.ConditionTrue), "baseline should be PoolReady=True")

		By("transitioning the PillarPool to Ready=False")
		setPoolReadyE262(poolName, metav1.ConditionFalse, "TargetNotFound", "target gone")

		By("re-reconciling the binding")
		reconcileBindingE262(bindingName)

		By("verifying PoolReady=False propagated to PillarBinding")
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())
		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	})

	// ── E26.2.4 ─────────────────────────────────────────────────────────────
	// PillarProtocol transitions from Ready=True to Ready=False.
	// Re-reconciling the dependent PillarBinding propagates ProtocolValid=False.
	It("E26.2.4: TestCrossLifecycle_Cascade_ProtocolBecomesInvalid_BindingNotReady — protocol→False → binding ProtocolValid=False", func() {
		const (
			poolName     = "e262-pool-for-proto-cascade"
			protocolName = "e262-proto-becomes-invalid"
			bindingName  = "e262-binding-proto-cascade"
		)

		By("creating a Ready PillarPool")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPoolE262(poolName) })
		setPoolReadyE262(poolName, metav1.ConditionTrue, "PoolReady", "ready")

		By("creating a Ready PillarProtocol")
		proto := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
		}
		Expect(k8sClient.Create(bctx, proto)).To(Succeed())
		DeferCleanup(func() { cleanupProtocolE262(protocolName) })
		setProtocolReadyE262(protocolName, metav1.ConditionTrue, "ProtocolReady", "ready")

		By("creating a PillarBinding")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() {
			cleanupSCE262(bindingName)
			cleanupBindingE262(bindingName)
		})

		By("reconciling binding to establish ProtocolValid=True baseline")
		reconcileBindingE262(bindingName)
		reconcileBindingE262(bindingName)

		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())
		protoCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionProtocolValid)
		Expect(protoCond).NotTo(BeNil())
		Expect(protoCond.Status).To(Equal(metav1.ConditionTrue), "baseline should be ProtocolValid=True")

		By("transitioning the PillarProtocol to Ready=False")
		setProtocolReadyE262(protocolName, metav1.ConditionFalse, "ProtocolInvalid", "protocol degraded")

		By("re-reconciling the binding")
		reconcileBindingE262(bindingName)

		By("verifying ProtocolValid=False propagated to PillarBinding")
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())
		cond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionProtocolValid)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	})

	// ── E26.2.5 ─────────────────────────────────────────────────────────────
	// Full chain recovery: PillarTarget recovers → Pool reconcile restores
	// TargetReady=True → Pool becomes Ready → Binding reconcile restores PoolReady=True.
	It("E26.2.5: TestCrossLifecycle_Cascade_FullChainRecovery — target recovery propagates through Pool→Binding", func() {
		const (
			targetName   = "e262-target-chain-recovery"
			poolName     = "e262-pool-chain-recovery"
			protocolName = "e262-proto-chain-recovery"
			bindingName  = "e262-binding-chain-recovery"
		)

		By("creating a not-ready PillarTarget")
		tgt := &pillarcsiv1alpha1.PillarTarget{
			ObjectMeta: metav1.ObjectMeta{Name: targetName},
			Spec: pillarcsiv1alpha1.PillarTargetSpec{
				External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.112", Port: 9500},
			},
		}
		Expect(k8sClient.Create(bctx, tgt)).To(Succeed())
		DeferCleanup(func() { cleanupTargetE262(targetName) })
		setTargetReadyE262(targetName, metav1.ConditionFalse, "AgentUnhealthy", "unhealthy")

		By("creating a PillarPool with not-ready status (no Ready condition initially)")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: targetName,
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPoolE262(poolName) })
		// Set pool Ready=False to simulate degraded state
		setPoolReadyE262(poolName, metav1.ConditionFalse, "TargetNotReady", "target not ready")

		By("creating a Ready PillarProtocol")
		proto := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
		}
		Expect(k8sClient.Create(bctx, proto)).To(Succeed())
		DeferCleanup(func() { cleanupProtocolE262(protocolName) })
		setProtocolReadyE262(protocolName, metav1.ConditionTrue, "ProtocolReady", "ready")

		By("creating a PillarBinding")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() {
			cleanupSCE262(bindingName)
			cleanupBindingE262(bindingName)
		})

		By("reconciling binding to establish degraded baseline (PoolReady=False)")
		reconcileBindingE262(bindingName)
		reconcileBindingE262(bindingName)

		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())
		poolCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(poolCond).NotTo(BeNil())
		Expect(poolCond.Status).To(Equal(metav1.ConditionFalse), "baseline PoolReady should be False")

		By("recovering the PillarTarget to Ready=True")
		setTargetReadyE262(targetName, metav1.ConditionTrue, "Authenticated", "healthy again")

		By("reconciling pool — it should now see target as Ready and become Ready itself")
		reconcilePoolE262(poolName)

		poolFetched := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, poolFetched)).To(Succeed())
		targetReadyCond := apimeta.FindStatusCondition(poolFetched.Status.Conditions, "TargetReady")
		Expect(targetReadyCond).NotTo(BeNil())
		Expect(targetReadyCond.Status).To(Equal(metav1.ConditionTrue), "pool TargetReady should propagate from target recovery")

		By("simulating pool becoming Ready after target recovery")
		setPoolReadyE262(poolName, metav1.ConditionTrue, "PoolReady", "target recovered")

		By("reconciling binding — it should now see pool as Ready")
		reconcileBindingE262(bindingName)

		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())
		recoveredPoolCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionPoolReady)
		Expect(recoveredPoolCond).NotTo(BeNil())
		Expect(recoveredPoolCond.Status).To(Equal(metav1.ConditionTrue), "PoolReady should be True after full chain recovery")
	})

	// ── E26.2.6 ─────────────────────────────────────────────────────────────
	// PillarPool transitions from Ready=False to Ready=True.
	// Re-reconciling the dependent PillarBinding creates a StorageClass.
	It("E26.2.6: TestCrossLifecycle_Cascade_BindingBecomesReady_StorageClassCreated — pool→Ready → binding creates StorageClass", func() {
		const (
			poolName     = "e262-pool-becomes-ready"
			protocolName = "e262-proto-for-sc-test"
			bindingName  = "e262-binding-sc-created"
		)

		By("creating a not-ready PillarPool")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPoolE262(poolName) })
		setPoolReadyE262(poolName, metav1.ConditionFalse, "TargetNotFound", "target not found")

		By("creating a Ready PillarProtocol")
		proto := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
		}
		Expect(k8sClient.Create(bctx, proto)).To(Succeed())
		DeferCleanup(func() { cleanupProtocolE262(protocolName) })
		setProtocolReadyE262(protocolName, metav1.ConditionTrue, "ProtocolReady", "ready")

		By("creating a PillarBinding")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() {
			cleanupSCE262(bindingName)
			cleanupBindingE262(bindingName)
		})

		By("reconciling binding with not-ready pool — no StorageClass should be created")
		reconcileBindingE262(bindingName)
		reconcileBindingE262(bindingName)

		sc := &storagev1.StorageClass{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)
		Expect(err).To(HaveOccurred(), "StorageClass should not exist when pool is not ready")

		By("transitioning the PillarPool to Ready=True")
		setPoolReadyE262(poolName, metav1.ConditionTrue, "PoolReady", "pool now ready")

		By("re-reconciling the binding")
		reconcileBindingE262(bindingName)

		By("verifying StorageClass is now created")
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed(),
			"StorageClass %q should be created after pool becomes Ready", bindingName)

		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, fetched)).To(Succeed())
		scCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionStorageClassCreated)
		Expect(scCond).NotTo(BeNil())
		Expect(scCond.Status).To(Equal(metav1.ConditionTrue))

		readyCond := apimeta.FindStatusCondition(fetched.Status.Conditions, conditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
	})

	// ── E26.2.7 ─────────────────────────────────────────────────────────────
	// A PillarBinding references a PillarProtocol.
	// After reconciling the PillarProtocol, status.bindingCount reflects the binding.
	It("E26.2.7: TestCrossLifecycle_Cascade_ProtocolBindingCount_IncrementOnCreate — binding reference increments protocol BindingCount", func() {
		const (
			poolName     = "e262-pool-for-bindcount"
			protocolName = "e262-proto-for-bindcount"
			bindingName  = "e262-binding-for-bindcount"
		)

		By("creating a PillarProtocol")
		proto := &pillarcsiv1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: protocolName},
			Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
		}
		Expect(k8sClient.Create(bctx, proto)).To(Succeed())
		DeferCleanup(func() { cleanupProtocolE262(protocolName) })

		By("reconciling the protocol with no bindings — BindingCount should be 0")
		reconcileProtocolE262(protocolName)
		reconcileProtocolE262(protocolName)

		fetchedProto := &pillarcsiv1alpha1.PillarProtocol{}
		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, fetchedProto)).To(Succeed())
		Expect(fetchedProto.Status.BindingCount).To(Equal(int32(0)),
			"BindingCount should be 0 before any bindings are created")

		By("creating a PillarPool (needed as binding's poolRef)")
		pool := &pillarcsiv1alpha1.PillarPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "some-target",
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
			},
		}
		Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		DeferCleanup(func() { cleanupPoolE262(poolName) })

		By("creating a PillarBinding that references the protocol")
		binding := &pillarcsiv1alpha1.PillarBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     poolName,
				ProtocolRef: protocolName,
			},
		}
		Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		DeferCleanup(func() {
			cleanupSCE262(bindingName)
			cleanupBindingE262(bindingName)
		})

		By("re-reconciling the protocol — it should count the new binding")
		reconcileProtocolE262(protocolName)

		Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, fetchedProto)).To(Succeed())
		Expect(fetchedProto.Status.BindingCount).To(Equal(int32(1)),
			"BindingCount should be 1 after a PillarBinding references this protocol")
	})
})
