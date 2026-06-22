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

// cross_lifecycle_test.go — E26 cross-CRD lifecycle interaction tests.
//
// These tests verify that the pillar-csi finalizer-based deletion protection
// mechanism correctly prevents deletion of a CRD while dependent CRDs still
// reference it:
//
//   - PillarAgent cannot be deleted while a PillarStore references it via
//     spec.agentRef (E26.3.1, E26.3.2).
//   - PillarStore cannot be deleted while a PillarStorageClass references it via
//     spec.storeRef (E26.3.3, E26.3.4).
//   - PillarProtocol cannot be deleted while a PillarStorageClass references it via
//     spec.protocolRef (E26.3.5, E26.3.6).
//   - Full reverse-order deletion (Binding→Pool→Target) completes cleanly
//     (E26.3.7).
//   - A CRD with no dependents is deleted immediately after finalizer removal
//     (E26.3.8).
//
// All tests use the shared envtest environment (k8sClient) set up in suite_test.go.
// Reconcilers are instantiated directly without a live controller manager so that
// the tests are fully deterministic (no background goroutines).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// E26: Cross-CRD Lifecycle — Deletion Protection
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E26: Cross-CRD Deletion Protection", func() {
	var bctx context.Context

	BeforeEach(func() {
		bctx = context.Background()
	})

	// ─────────────────────────────────────────────────────────────────────
	// E26.3.1 / E26.3.2 — PillarAgent blocked/unblocked by PillarStore
	// ─────────────────────────────────────────────────────────────────────
	Describe("E26.3.1–E26.3.2: Target deletion protection — blocked by Pool", Ordered, func() {
		const (
			tgtName  = "e26-target-blocked"
			poolName = "e26-pool-blocking-target"
		)
		tgtNN := types.NamespacedName{Name: tgtName}
		poolNN := types.NamespacedName{Name: poolName}

		var targetReconciler *PillarAgentReconciler
		var poolReconciler *PillarStoreReconciler

		BeforeAll(func() {
			targetReconciler = &PillarAgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			poolReconciler = &PillarStoreReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create PillarAgent.
			target := &pillarcsiv1alpha1.PillarAgent{
				ObjectMeta: metav1.ObjectMeta{Name: tgtName},
				Spec: pillarcsiv1alpha1.PillarAgentSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "192.0.2.10",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, target)).To(Succeed())

			// First reconcile: adds finalizer.
			_, err := targetReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tgtNN})
			Expect(err).NotTo(HaveOccurred())

			// Create a PillarStore that references this target.
			pool := &pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: tgtName,
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeDir,
					},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())

			// Reconcile pool once to add its own finalizer.
			_, err = poolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			// Best-effort cleanup: force-remove finalizers so the objects can be GC'd.
			tgt := &pillarcsiv1alpha1.PillarAgent{}
			if err := k8sClient.Get(bctx, tgtNN, tgt); err == nil {
				controllerutil.RemoveFinalizer(tgt, pillarAgentFinalizer)
				_ = k8sClient.Update(bctx, tgt)
				_ = k8sClient.Delete(bctx, tgt)
			}
			pool := &pillarcsiv1alpha1.PillarStore{}
			if err := k8sClient.Get(bctx, poolNN, pool); err == nil {
				controllerutil.RemoveFinalizer(pool, pillarStoreFinalizer)
				_ = k8sClient.Update(bctx, pool)
				_ = k8sClient.Delete(bctx, pool)
			}
		})

		It("E26.3.1: marks DeletionTimestamp and retains finalizer while pool references target", func() {
			// Issue delete.
			tgt := &pillarcsiv1alpha1.PillarAgent{}
			Expect(k8sClient.Get(bctx, tgtNN, tgt)).To(Succeed())
			Expect(k8sClient.Delete(bctx, tgt)).To(Succeed())

			// Reconcile — should block.
			result, err := targetReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tgtNN})
			Expect(err).NotTo(HaveOccurred())

			// The reconciler must requeue to retry when the pool is removed.
			Expect(result.RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock),
				"reconciler must requeue while pool still references the target")

			// The object must still exist (finalizer blocks GC).
			fetched := &pillarcsiv1alpha1.PillarAgent{}
			Expect(k8sClient.Get(bctx, tgtNN, fetched)).To(Succeed(),
				"PillarAgent must still exist while referenced by PillarStore")

			// Finalizer must still be present.
			Expect(controllerutil.ContainsFinalizer(fetched, pillarAgentFinalizer)).To(BeTrue(),
				"finalizer must not be removed while PillarStore references the target")

			// DeletionTimestamp must be set (API server acknowledged the delete request).
			Expect(fetched.DeletionTimestamp).NotTo(BeNil(),
				"DeletionTimestamp must be set after k8sClient.Delete()")
		})

		It("E26.3.2: finalizer is removed and target is deleted after pool is removed", func() {
			// Remove the blocking PillarStore (force-remove its finalizer first).
			pool := &pillarcsiv1alpha1.PillarStore{}
			Expect(k8sClient.Get(bctx, poolNN, pool)).To(Succeed())
			controllerutil.RemoveFinalizer(pool, pillarStoreFinalizer)
			Expect(k8sClient.Update(bctx, pool)).To(Succeed())
			Expect(k8sClient.Delete(bctx, pool)).To(Succeed())

			// Confirm pool is gone.
			Eventually(func() bool {
				err := k8sClient.Get(bctx, poolNN, &pillarcsiv1alpha1.PillarStore{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarStore must be deleted before proceeding")

			// Reconcile target again — no pools remain, finalizer should be removed.
			result, err := targetReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tgtNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"reconciler must not requeue after finalizer is removed")

			// The target should now be gone (no finalizer → GC completes immediately).
			Eventually(func() bool {
				err := k8sClient.Get(bctx, tgtNN, &pillarcsiv1alpha1.PillarAgent{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarAgent must be deleted once no pools reference it")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// E26.3.3 / E26.3.4 — PillarStore blocked/unblocked by PillarStorageClass
	// ─────────────────────────────────────────────────────────────────────
	Describe("E26.3.3–E26.3.4: Pool deletion protection — blocked by Binding", Ordered, func() {
		const (
			poolName    = "e26-pool-blocked"
			bindingName = "e26-binding-blocking-pool"
			protoName   = "e26-proto-for-pool-test"
		)
		poolNN := types.NamespacedName{Name: poolName}
		bindingNN := types.NamespacedName{Name: bindingName}
		protoNN := types.NamespacedName{Name: protoName}

		var poolReconciler *PillarStoreReconciler
		var bindingReconciler *PillarStorageClassReconciler
		var protocolReconciler *PillarProtocolReconciler

		BeforeAll(func() {
			poolReconciler = &PillarStoreReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			bindingReconciler = &PillarStorageClassReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			protocolReconciler = &PillarProtocolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create PillarStore.
			pool := &pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: "nonexistent-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeDir,
					},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())

			// First reconcile: adds finalizer.
			_, err := poolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNN})
			Expect(err).NotTo(HaveOccurred())

			// Create PillarProtocol (needed for PillarStorageClass's protocolRef).
			proto := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: protoName},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{
					Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
					NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
						Port: 4420,
					},
				},
			}
			Expect(k8sClient.Create(bctx, proto)).To(Succeed())

			// Reconcile protocol once to add its finalizer.
			_, err = protocolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: protoNN})
			Expect(err).NotTo(HaveOccurred())

			// Create PillarStorageClass that references the pool.
			binding := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    poolName,
					ProtocolRef: protoName,
				},
			}
			Expect(k8sClient.Create(bctx, binding)).To(Succeed())

			// Reconcile binding once to add its finalizer.
			_, err = bindingReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			// Best-effort cleanup.
			binding := &pillarcsiv1alpha1.PillarStorageClass{}
			if err := k8sClient.Get(bctx, bindingNN, binding); err == nil {
				controllerutil.RemoveFinalizer(binding, pillarStorageClassFinalizer)
				_ = k8sClient.Update(bctx, binding)
				_ = k8sClient.Delete(bctx, binding)
			}
			pool := &pillarcsiv1alpha1.PillarStore{}
			if err := k8sClient.Get(bctx, poolNN, pool); err == nil {
				controllerutil.RemoveFinalizer(pool, pillarStoreFinalizer)
				_ = k8sClient.Update(bctx, pool)
				_ = k8sClient.Delete(bctx, pool)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(bctx, protoNN, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
				_ = k8sClient.Update(bctx, proto)
				_ = k8sClient.Delete(bctx, proto)
			}
		})

		It("E26.3.3: marks DeletionTimestamp and retains finalizer while binding references pool", func() {
			// Issue delete on the pool.
			pool := &pillarcsiv1alpha1.PillarStore{}
			Expect(k8sClient.Get(bctx, poolNN, pool)).To(Succeed())
			Expect(k8sClient.Delete(bctx, pool)).To(Succeed())

			// Reconcile — should block.
			result, err := poolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNN})
			Expect(err).NotTo(HaveOccurred())

			Expect(result.RequeueAfter).To(Equal(requeueAfterPoolDeletionBlock),
				"reconciler must requeue while binding still references the pool")

			// Pool must still exist.
			fetched := &pillarcsiv1alpha1.PillarStore{}
			Expect(k8sClient.Get(bctx, poolNN, fetched)).To(Succeed(),
				"PillarStore must still exist while referenced by PillarStorageClass")

			// Finalizer must still be present.
			Expect(controllerutil.ContainsFinalizer(fetched, pillarStoreFinalizer)).To(BeTrue(),
				"pool finalizer must not be removed while PillarStorageClass references it")

			// DeletionTimestamp must be set.
			Expect(fetched.DeletionTimestamp).NotTo(BeNil(),
				"DeletionTimestamp must be set after k8sClient.Delete()")
		})

		It("E26.3.4: finalizer is removed and pool is deleted after binding is removed", func() {
			// Remove the blocking binding (force-remove its finalizer first).
			binding := &pillarcsiv1alpha1.PillarStorageClass{}
			Expect(k8sClient.Get(bctx, bindingNN, binding)).To(Succeed())
			controllerutil.RemoveFinalizer(binding, pillarStorageClassFinalizer)
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())
			Expect(k8sClient.Delete(bctx, binding)).To(Succeed())

			// Confirm binding is gone.
			Eventually(func() bool {
				err := k8sClient.Get(bctx, bindingNN, &pillarcsiv1alpha1.PillarStorageClass{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarStorageClass must be deleted before proceeding")

			// Reconcile pool again — no bindings remain, finalizer should be removed.
			result, err := poolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"reconciler must not requeue after finalizer is removed")

			// Pool should now be gone.
			Eventually(func() bool {
				err := k8sClient.Get(bctx, poolNN, &pillarcsiv1alpha1.PillarStore{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarStore must be deleted once no bindings reference it")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// E26.3.5 / E26.3.6 — PillarProtocol blocked/unblocked by PillarStorageClass
	// ─────────────────────────────────────────────────────────────────────
	Describe("E26.3.5–E26.3.6: Protocol deletion protection — blocked by Binding", Ordered, func() {
		const (
			protoName   = "e26-proto-blocked"
			poolName    = "e26-pool-for-proto-test"
			bindingName = "e26-binding-blocking-proto"
		)
		protoNN := types.NamespacedName{Name: protoName}
		poolNN := types.NamespacedName{Name: poolName}
		bindingNN := types.NamespacedName{Name: bindingName}

		var protocolReconciler *PillarProtocolReconciler
		var poolReconciler *PillarStoreReconciler
		var bindingReconciler *PillarStorageClassReconciler

		BeforeAll(func() {
			protocolReconciler = &PillarProtocolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			poolReconciler = &PillarStoreReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			bindingReconciler = &PillarStorageClassReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create PillarProtocol.
			proto := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: protoName},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{
					Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
					NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
						Port: 4420,
					},
				},
			}
			Expect(k8sClient.Create(bctx, proto)).To(Succeed())

			// First reconcile: adds finalizer.
			_, err := protocolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: protoNN})
			Expect(err).NotTo(HaveOccurred())

			// Create PillarStore (needed for PillarStorageClass's storeRef).
			pool := &pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: "nonexistent-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeDir,
					},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())

			// Reconcile pool once to add its finalizer.
			_, err = poolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNN})
			Expect(err).NotTo(HaveOccurred())

			// Create PillarStorageClass that references the protocol.
			binding := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    poolName,
					ProtocolRef: protoName,
				},
			}
			Expect(k8sClient.Create(bctx, binding)).To(Succeed())

			// Reconcile binding once to add its finalizer.
			_, err = bindingReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			// Best-effort cleanup.
			binding := &pillarcsiv1alpha1.PillarStorageClass{}
			if err := k8sClient.Get(bctx, bindingNN, binding); err == nil {
				controllerutil.RemoveFinalizer(binding, pillarStorageClassFinalizer)
				_ = k8sClient.Update(bctx, binding)
				_ = k8sClient.Delete(bctx, binding)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(bctx, protoNN, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
				_ = k8sClient.Update(bctx, proto)
				_ = k8sClient.Delete(bctx, proto)
			}
			pool := &pillarcsiv1alpha1.PillarStore{}
			if err := k8sClient.Get(bctx, poolNN, pool); err == nil {
				controllerutil.RemoveFinalizer(pool, pillarStoreFinalizer)
				_ = k8sClient.Update(bctx, pool)
				_ = k8sClient.Delete(bctx, pool)
			}
		})

		It("E26.3.5: marks DeletionTimestamp and retains finalizer while binding references protocol", func() {
			// Issue delete on the protocol.
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, protoNN, proto)).To(Succeed())
			Expect(k8sClient.Delete(bctx, proto)).To(Succeed())

			// Reconcile — should block.
			result, err := protocolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: protoNN})
			Expect(err).NotTo(HaveOccurred())

			Expect(result.RequeueAfter).To(Equal(requeueAfterProtocolDeletionBlock),
				"reconciler must requeue while binding still references the protocol")

			// Protocol must still exist.
			fetched := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, protoNN, fetched)).To(Succeed(),
				"PillarProtocol must still exist while referenced by PillarStorageClass")

			// Finalizer must still be present.
			Expect(controllerutil.ContainsFinalizer(fetched, pillarProtocolFinalizer)).To(BeTrue(),
				"protocol finalizer must not be removed while PillarStorageClass references it")

			// DeletionTimestamp must be set.
			Expect(fetched.DeletionTimestamp).NotTo(BeNil(),
				"DeletionTimestamp must be set after k8sClient.Delete()")
		})

		It("E26.3.6: finalizer is removed and protocol is deleted after binding is removed", func() {
			// Remove the blocking binding.
			binding := &pillarcsiv1alpha1.PillarStorageClass{}
			Expect(k8sClient.Get(bctx, bindingNN, binding)).To(Succeed())
			controllerutil.RemoveFinalizer(binding, pillarStorageClassFinalizer)
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())
			Expect(k8sClient.Delete(bctx, binding)).To(Succeed())

			// Confirm binding is gone.
			Eventually(func() bool {
				err := k8sClient.Get(bctx, bindingNN, &pillarcsiv1alpha1.PillarStorageClass{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarStorageClass must be deleted before proceeding")

			// Reconcile protocol again — no bindings remain, finalizer should be removed.
			result, err := protocolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: protoNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"reconciler must not requeue after finalizer is removed")

			// Protocol should now be gone.
			Eventually(func() bool {
				err := k8sClient.Get(bctx, protoNN, &pillarcsiv1alpha1.PillarProtocol{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarProtocol must be deleted once no bindings reference it")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// E26.3.7 — Full chain reverse-order deletion
	// ─────────────────────────────────────────────────────────────────────
	Describe("E26.3.7: Full chain reverse-order deletion (Binding→Pool→Target)", Ordered, func() {
		const (
			tgtName     = "e26-chain-target"
			poolName    = "e26-chain-pool"
			protoName   = "e26-chain-proto"
			bindingName = "e26-chain-binding"
		)
		tgtNN := types.NamespacedName{Name: tgtName}
		poolNN := types.NamespacedName{Name: poolName}
		protoNN := types.NamespacedName{Name: protoName}
		bindingNN := types.NamespacedName{Name: bindingName}

		var targetReconciler *PillarAgentReconciler
		var poolReconciler *PillarStoreReconciler
		var protocolReconciler *PillarProtocolReconciler
		var bindingReconciler *PillarStorageClassReconciler

		BeforeAll(func() {
			targetReconciler = &PillarAgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			poolReconciler = &PillarStoreReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			protocolReconciler = &PillarProtocolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			bindingReconciler = &PillarStorageClassReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create all four CRDs in dependency order.
			target := &pillarcsiv1alpha1.PillarAgent{
				ObjectMeta: metav1.ObjectMeta{Name: tgtName},
				Spec: pillarcsiv1alpha1.PillarAgentSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "192.0.2.20",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, target)).To(Succeed())
			_, err := targetReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tgtNN})
			Expect(err).NotTo(HaveOccurred())

			pool := &pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: tgtName,
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeDir,
					},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())
			_, err = poolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNN})
			Expect(err).NotTo(HaveOccurred())

			proto := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: protoName},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{
					Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
					NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
						Port: 4420,
					},
				},
			}
			Expect(k8sClient.Create(bctx, proto)).To(Succeed())
			_, err = protocolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: protoNN})
			Expect(err).NotTo(HaveOccurred())

			binding := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    poolName,
					ProtocolRef: protoName,
				},
			}
			Expect(k8sClient.Create(bctx, binding)).To(Succeed())
			_, err = bindingReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			// Best-effort cleanup in case any step failed.
			for _, nn := range []types.NamespacedName{bindingNN, poolNN, protoNN, tgtNN} {
				switch nn.Name {
				case bindingName:
					obj := &pillarcsiv1alpha1.PillarStorageClass{}
					if err := k8sClient.Get(bctx, nn, obj); err == nil {
						controllerutil.RemoveFinalizer(obj, pillarStorageClassFinalizer)
						_ = k8sClient.Update(bctx, obj)
						_ = k8sClient.Delete(bctx, obj)
					}
				case poolName:
					obj := &pillarcsiv1alpha1.PillarStore{}
					if err := k8sClient.Get(bctx, nn, obj); err == nil {
						controllerutil.RemoveFinalizer(obj, pillarStoreFinalizer)
						_ = k8sClient.Update(bctx, obj)
						_ = k8sClient.Delete(bctx, obj)
					}
				case protoName:
					obj := &pillarcsiv1alpha1.PillarProtocol{}
					if err := k8sClient.Get(bctx, nn, obj); err == nil {
						controllerutil.RemoveFinalizer(obj, pillarProtocolFinalizer)
						_ = k8sClient.Update(bctx, obj)
						_ = k8sClient.Delete(bctx, obj)
					}
				case tgtName:
					obj := &pillarcsiv1alpha1.PillarAgent{}
					if err := k8sClient.Get(bctx, nn, obj); err == nil {
						controllerutil.RemoveFinalizer(obj, pillarAgentFinalizer)
						_ = k8sClient.Update(bctx, obj)
						_ = k8sClient.Delete(bctx, obj)
					}
				}
			}
		})

		It("Step 1: deletes PillarStorageClass successfully (leaf node, no dependents)", func() {
			// Delete binding — it has no dependents so reconcile should remove finalizer.
			binding := &pillarcsiv1alpha1.PillarStorageClass{}
			Expect(k8sClient.Get(bctx, bindingNN, binding)).To(Succeed())
			Expect(k8sClient.Delete(bctx, binding)).To(Succeed())

			result, err := bindingReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			Eventually(func() bool {
				err := k8sClient.Get(bctx, bindingNN, &pillarcsiv1alpha1.PillarStorageClass{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarStorageClass must be deleted (no dependents)")
		})

		It("Step 2: PillarStore reconcile unblocks and removes finalizer after binding is gone", func() {
			// Pool was previously blocked; now binding is gone so it can be unblocked.
			// First, mark pool for deletion.
			pool := &pillarcsiv1alpha1.PillarStore{}
			Expect(k8sClient.Get(bctx, poolNN, pool)).To(Succeed())
			Expect(k8sClient.Delete(bctx, pool)).To(Succeed())

			result, err := poolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"pool reconciler must remove finalizer when no bindings remain")

			Eventually(func() bool {
				err := k8sClient.Get(bctx, poolNN, &pillarcsiv1alpha1.PillarStore{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarStore must be deleted after binding is removed")
		})

		It("Step 3: PillarProtocol reconcile removes finalizer (no bindings)", func() {
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, protoNN, proto)).To(Succeed())
			Expect(k8sClient.Delete(bctx, proto)).To(Succeed())

			result, err := protocolReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: protoNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			Eventually(func() bool {
				err := k8sClient.Get(bctx, protoNN, &pillarcsiv1alpha1.PillarProtocol{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarProtocol must be deleted after all bindings are gone")
		})

		It("Step 4: PillarAgent reconcile removes finalizer (no pools)", func() {
			target := &pillarcsiv1alpha1.PillarAgent{}
			Expect(k8sClient.Get(bctx, tgtNN, target)).To(Succeed())
			Expect(k8sClient.Delete(bctx, target)).To(Succeed())

			result, err := targetReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tgtNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"target reconciler must remove finalizer when no pools remain")

			Eventually(func() bool {
				err := k8sClient.Get(bctx, tgtNN, &pillarcsiv1alpha1.PillarAgent{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarAgent must be deleted after all pools are removed")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// E26.3.8 — No dependents → immediate deletion
	// ─────────────────────────────────────────────────────────────────────
	Describe("E26.3.8: No dependents — finalizer removed immediately", func() {
		const tgtName = "e26-target-no-deps"
		tgtNN := types.NamespacedName{Name: tgtName}

		var targetReconciler *PillarAgentReconciler

		BeforeEach(func() {
			targetReconciler = &PillarAgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create a standalone PillarAgent with no referencing pools.
			target := &pillarcsiv1alpha1.PillarAgent{
				ObjectMeta: metav1.ObjectMeta{Name: tgtName},
				Spec: pillarcsiv1alpha1.PillarAgentSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "192.0.2.30",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, target)).To(Succeed())

			// First reconcile: adds finalizer.
			_, err := targetReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tgtNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			// Best-effort cleanup if the test failed before deletion.
			tgt := &pillarcsiv1alpha1.PillarAgent{}
			if err := k8sClient.Get(bctx, tgtNN, tgt); err == nil {
				controllerutil.RemoveFinalizer(tgt, pillarAgentFinalizer)
				_ = k8sClient.Update(bctx, tgt)
				_ = k8sClient.Delete(bctx, tgt)
			}
		})

		It("should remove the finalizer and allow deletion when no pools reference the target", func() {
			// Delete the target.
			tgt := &pillarcsiv1alpha1.PillarAgent{}
			Expect(k8sClient.Get(bctx, tgtNN, tgt)).To(Succeed())
			Expect(k8sClient.Delete(bctx, tgt)).To(Succeed())

			// Reconcile — no pools reference this target, so finalizer should be removed.
			result, err := targetReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tgtNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"reconciler must not requeue when no pools reference the target")

			// Target should be gone.
			Eventually(func() bool {
				err := k8sClient.Get(bctx, tgtNN, &pillarcsiv1alpha1.PillarAgent{})
				return errors.IsNotFound(err)
			}).Should(BeTrue(), "PillarAgent must be deleted immediately when no pools reference it")
		})
	})
})
