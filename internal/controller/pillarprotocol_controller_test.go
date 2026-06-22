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

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("PillarProtocol Controller", func() {
	const (
		// Use distinctive names to avoid collision with other test suites.
		pprProtocolName = "ppr-main"
		pprPoolName     = "ppr-pool"
		pprTargetName   = "ppr-target"
		pprBindingName  = "ppr-binding"
	)

	var (
		pctx                   context.Context
		reconciler             *PillarProtocolReconciler
		protocolNamespacedName types.NamespacedName
	)

	BeforeEach(func() {
		pctx = context.Background()
		reconciler = &PillarProtocolReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		protocolNamespacedName = types.NamespacedName{Name: pprProtocolName}
	})

	// doReconcile triggers a single reconcile pass and returns result + error.
	doReconcile := func() (reconcile.Result, error) {
		return reconciler.Reconcile(pctx, reconcile.Request{NamespacedName: protocolNamespacedName})
	}

	// createProtocol creates a minimal PillarProtocol with NVMeOF-TCP type.
	createProtocol := func() {
		resource := &pillarcsiv1alpha1.PillarProtocol{}
		err := k8sClient.Get(pctx, protocolNamespacedName, resource)
		if err != nil && errors.IsNotFound(err) {
			obj := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{
					Name: pprProtocolName,
				},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{
					Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
				},
			}
			Expect(k8sClient.Create(pctx, obj)).To(Succeed())
		}
	}

	// deleteProtocol marks the PillarProtocol for deletion (sets DeletionTimestamp).
	// If the object has a finalizer it will remain until the finalizer is removed.
	deleteProtocol := func() {
		resource := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(pctx, protocolNamespacedName, resource); err == nil {
			Expect(k8sClient.Delete(pctx, resource)).To(Succeed())
		}
	}

	// forceRemoveProtocolFinalizer strips the finalizer so the object can be GC'd.
	forceRemoveProtocolFinalizer := func() {
		resource := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(pctx, protocolNamespacedName, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarProtocolFinalizer)
			Expect(k8sClient.Update(pctx, resource)).To(Succeed())
		}
	}

	// fetchProtocol fetches the current PillarProtocol from the API server.
	fetchProtocol := func() *pillarcsiv1alpha1.PillarProtocol {
		fetched := &pillarcsiv1alpha1.PillarProtocol{}
		Expect(k8sClient.Get(pctx, protocolNamespacedName, fetched)).To(Succeed())
		return fetched
	}

	// findProtocolCondition returns the named condition from a PillarProtocol, or nil.
	findProtocolCondition := func(protocol *pillarcsiv1alpha1.PillarProtocol, condType string) *metav1.Condition {
		return apimeta.FindStatusCondition(protocol.Status.Conditions, condType)
	}

	// createPool creates a PillarStore that references pprTargetName.
	createPool := func() {
		pool := &pillarcsiv1alpha1.PillarStore{}
		err := k8sClient.Get(pctx, types.NamespacedName{Name: pprPoolName}, pool)
		if err != nil && errors.IsNotFound(err) {
			obj := &pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{
					Name: pprPoolName,
				},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: pprTargetName,
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
					},
				},
			}
			Expect(k8sClient.Create(pctx, obj)).To(Succeed())
		}
	}

	// deletePool removes any finalizer and deletes the PillarStore.
	deletePool := func() {
		resource := &pillarcsiv1alpha1.PillarStore{}
		if err := k8sClient.Get(pctx, types.NamespacedName{Name: pprPoolName}, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarStoreFinalizer)
			Expect(k8sClient.Update(pctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(pctx, resource)).To(Succeed())
		}
	}

	// createBinding creates a PillarStorageClass with StoreRef=pprPoolName and ProtocolRef=pprProtocolName.
	createBinding := func() {
		binding := &pillarcsiv1alpha1.PillarStorageClass{}
		err := k8sClient.Get(pctx, types.NamespacedName{Name: pprBindingName}, binding)
		if err != nil && errors.IsNotFound(err) {
			obj := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: pprBindingName,
				},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    pprPoolName,
					ProtocolRef: pprProtocolName,
				},
			}
			Expect(k8sClient.Create(pctx, obj)).To(Succeed())
		}
	}

	// deleteBinding removes any finalizer and deletes the PillarStorageClass.
	deleteBinding := func() {
		resource := &pillarcsiv1alpha1.PillarStorageClass{}
		if err := k8sClient.Get(pctx, types.NamespacedName{Name: pprBindingName}, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarStorageClassFinalizer)
			Expect(k8sClient.Update(pctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(pctx, resource)).To(Succeed())
		}
	}

	// =========================================================================
	Context("Finalizer management", func() {
		AfterEach(func() {
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should add the protocol-protection finalizer on first reconcile", func() {
			createProtocol()

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// After adding finalizer the reconciler returns immediately without requeue.
			Expect(result.RequeueAfter).To(BeZero())

			fetched := fetchProtocol()
			Expect(controllerutil.ContainsFinalizer(fetched, pillarProtocolFinalizer)).To(BeTrue(),
				"finalizer %q should be present after first reconcile", pillarProtocolFinalizer)
		})

		It("should not duplicate the finalizer on subsequent reconciles", func() {
			createProtocol()

			// First reconcile adds the finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile (normal path) should not duplicate.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			count := 0
			for _, f := range fetched.Finalizers {
				if f == pillarProtocolFinalizer {
					count++
				}
			}
			Expect(count).To(Equal(1), "finalizer should appear exactly once")
		})
	})

	// =========================================================================
	Context("Normal reconciliation — no bindings referencing this protocol", func() {
		BeforeEach(func() {
			createProtocol()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should set Ready=True on the normal reconcile path", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should set Ready reason to ProtocolConfigured", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("ProtocolConfigured"))
		})

		It("should set StorageClassCount=0 when no bindings reference this protocol", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.StorageClassCount).To(Equal(int32(0)))
		})

		It("should set ActiveAgents to empty when no bindings reference this protocol", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.ActiveAgents).To(BeEmpty())
		})

		It("should not requeue when the protocol is correctly configured", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})

		It("should mention the protocol type in the Ready condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring(string(pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP)))
		})
	})

	// =========================================================================
	Context("Normal reconciliation — with a referencing PillarStorageClass and PillarStore", func() {
		BeforeEach(func() {
			createProtocol()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool()
			createBinding()
		})

		AfterEach(func() {
			deleteBinding()
			deletePool()
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should set StorageClassCount=1 when one binding references this protocol", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.StorageClassCount).To(Equal(int32(1)))
		})

		It("should populate ActiveAgents from the pool's agentRef", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.ActiveAgents).To(ContainElement(pprTargetName))
		})

		It("should set Ready=True even when bindings exist", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should mention binding count in the Ready condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil())
			// Message should include "1 binding(s)".
			Expect(cond.Message).To(ContainSubstring("1 binding"))
		})

		It("should mention active target count in the Ready condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil())
			// Message should include "1 active target(s)".
			Expect(cond.Message).To(ContainSubstring("1 active target"))
		})

		It("should not requeue when protocol is configured and bindings exist", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	// =========================================================================
	Context("ActiveAgents — pool referenced by binding does not exist (graceful skip)", func() {
		BeforeEach(func() {
			createProtocol()
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create binding referencing pprPoolName, but do NOT create the pool.
			createBinding()
		})

		AfterEach(func() {
			deleteBinding()
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should still count the binding even if the referenced pool is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.StorageClassCount).To(Equal(int32(1)))
		})

		It("should leave ActiveAgents empty when the referenced pool does not exist", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.ActiveAgents).To(BeEmpty())
		})

		It("should still set Ready=True when pool is not found (graceful degradation)", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// =========================================================================
	Context("StorageClassCount decrements when a binding is removed", func() {
		BeforeEach(func() {
			createProtocol()
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool()
			createBinding()
			// Verify count is 1 initially.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			deleteBinding()
			deletePool()
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should decrement StorageClassCount to 0 after the binding is deleted", func() {
			deleteBinding()

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.StorageClassCount).To(Equal(int32(0)))
		})

		It("should clear ActiveAgents after the binding is deleted", func() {
			deleteBinding()

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.ActiveAgents).To(BeEmpty())
		})
	})

	// =========================================================================
	Context("Deletion — no blocking PillarStorageClasss", func() {
		BeforeEach(func() {
			createProtocol()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Mark for deletion — no bindings reference this protocol.
			deleteProtocol()
		})

		AfterEach(func() {
			// Guard: remove finalizer in case the reconcile did not run.
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should remove the finalizer and allow GC when no bindings exist", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should not requeue after clean deletion")

			// After reconcile the finalizer should be gone.
			// The object may already be GC'd or may still exist without the finalizer.
			fetched := &pillarcsiv1alpha1.PillarProtocol{}
			err = k8sClient.Get(pctx, protocolNamespacedName, fetched)
			if err == nil {
				Expect(controllerutil.ContainsFinalizer(fetched, pillarProtocolFinalizer)).To(BeFalse(),
					"finalizer should be removed after clean deletion")
			}
			// If err != nil it means the object was already GC'd, which is also correct.
		})
	})

	// =========================================================================
	Context("Deletion — blocked by a referencing PillarStorageClass", func() {
		BeforeEach(func() {
			createProtocol()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Normal reconcile.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create a binding BEFORE marking for deletion.
			createBinding()
			// Mark protocol for deletion.
			deleteProtocol()
		})

		AfterEach(func() {
			deleteBinding()
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should block deletion and requeue while bindings still reference the protocol", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterProtocolDeletionBlock))
		})

		It("should set Ready=False with reason DeletionBlocked", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set during deletion block")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("DeletionBlocked"))
		})

		It("should mention the blocking binding name in the DeletionBlocked message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring(pprBindingName))
		})

		It("should not remove the finalizer while bindings are blocking", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(controllerutil.ContainsFinalizer(fetched, pillarProtocolFinalizer)).To(BeTrue(),
				"finalizer should remain while bindings are blocking")
		})

		It("should update StorageClassCount to reflect the number of blocking bindings", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.StorageClassCount).To(Equal(int32(1)),
				"StorageClassCount should equal the number of blocking bindings")
		})

		It("should remove the finalizer once the blocking binding is deleted", func() {
			// First reconcile with the binding present — deletion is blocked.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterProtocolDeletionBlock))

			// Now remove the blocking binding.
			deleteBinding()

			// Reconcile again — no bindings, finalizer should be removed.
			result, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should not requeue after unblocked deletion")

			fetched := &pillarcsiv1alpha1.PillarProtocol{}
			err = k8sClient.Get(pctx, protocolNamespacedName, fetched)
			if err == nil {
				Expect(controllerutil.ContainsFinalizer(fetched, pillarProtocolFinalizer)).To(BeFalse(),
					"finalizer should be removed once blocking binding is gone")
			}
		})
	})

	// =========================================================================
	// E23.6.6 — TestPillarProtocolController_DeletionBlocked_MultipleBindingsAllNamed
	Context("Deletion — blocked by multiple referencing PillarStorageClasss", func() {
		const (
			pprBindingNameA = "ppr-binding-a"
			pprBindingNameB = "ppr-binding-b"
		)

		// createBindingNamed creates a PillarStorageClass with an explicit name.
		createBindingNamed := func(name string) {
			binding := &pillarcsiv1alpha1.PillarStorageClass{}
			err := k8sClient.Get(pctx, types.NamespacedName{Name: name}, binding)
			if err != nil && errors.IsNotFound(err) {
				obj := &pillarcsiv1alpha1.PillarStorageClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
						StoreRef:    pprPoolName,
						ProtocolRef: pprProtocolName,
					},
				}
				Expect(k8sClient.Create(pctx, obj)).To(Succeed())
			}
		}

		// deleteBindingNamed removes any finalizer and deletes a named PillarStorageClass.
		deleteBindingNamed := func(name string) {
			resource := &pillarcsiv1alpha1.PillarStorageClass{}
			if err := k8sClient.Get(pctx, types.NamespacedName{Name: name}, resource); err == nil {
				controllerutil.RemoveFinalizer(resource, pillarStorageClassFinalizer)
				Expect(k8sClient.Update(pctx, resource)).To(Succeed())
				Expect(k8sClient.Delete(pctx, resource)).To(Succeed())
			}
		}

		BeforeEach(func() {
			createProtocol()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Normal reconcile.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create two bindings BEFORE marking for deletion.
			createBindingNamed(pprBindingNameA)
			createBindingNamed(pprBindingNameB)
			// Mark protocol for deletion.
			deleteProtocol()
		})

		AfterEach(func() {
			deleteBindingNamed(pprBindingNameA)
			deleteBindingNamed(pprBindingNameB)
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should mention all blocking binding names in Ready.Message when multiple bindings reference the protocol", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterProtocolDeletionBlock),
				"should requeue after 10s when deletion is blocked")

			fetched := fetchProtocol()
			cond := findProtocolCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set during deletion block")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("DeletionBlocked"))
			Expect(cond.Message).To(ContainSubstring(pprBindingNameA),
				"DeletionBlocked message should name binding-a")
			Expect(cond.Message).To(ContainSubstring(pprBindingNameB),
				"DeletionBlocked message should name binding-b")
		})

		It("should set StorageClassCount to the total number of blocking bindings", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.StorageClassCount).To(Equal(int32(2)),
				"StorageClassCount should reflect all blocking bindings")
		})
	})

	// =========================================================================
	// E23.5.4 — TestPillarProtocolController_ActiveAgents_DeduplicatedSorted
	Context("ActiveAgents — deduplicated when multiple bindings share the same pool", func() {
		const (
			pprBindingNameC = "ppr-binding-c"
			pprBindingNameD = "ppr-binding-d"
		)

		// createBindingWithPool creates a PillarStorageClass referencing pprPoolName and pprProtocolName.
		createBindingWithPool := func(name string) {
			binding := &pillarcsiv1alpha1.PillarStorageClass{}
			err := k8sClient.Get(pctx, types.NamespacedName{Name: name}, binding)
			if err != nil && errors.IsNotFound(err) {
				obj := &pillarcsiv1alpha1.PillarStorageClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
						StoreRef:    pprPoolName,
						ProtocolRef: pprProtocolName,
					},
				}
				Expect(k8sClient.Create(pctx, obj)).To(Succeed())
			}
		}

		deleteBindingByName := func(name string) {
			resource := &pillarcsiv1alpha1.PillarStorageClass{}
			if err := k8sClient.Get(pctx, types.NamespacedName{Name: name}, resource); err == nil {
				controllerutil.RemoveFinalizer(resource, pillarStorageClassFinalizer)
				Expect(k8sClient.Update(pctx, resource)).To(Succeed())
				Expect(k8sClient.Delete(pctx, resource)).To(Succeed())
			}
		}

		BeforeEach(func() {
			createProtocol()
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create pool and two bindings both referencing the same pool.
			createPool()
			createBindingWithPool(pprBindingNameC)
			createBindingWithPool(pprBindingNameD)
		})

		AfterEach(func() {
			deleteBindingByName(pprBindingNameC)
			deleteBindingByName(pprBindingNameD)
			deletePool()
			forceRemoveProtocolFinalizer()
			deleteProtocol()
		})

		It("should deduplicate ActiveAgents when two bindings point to the same pool", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchProtocol()
			Expect(fetched.Status.StorageClassCount).To(Equal(int32(2)),
				"StorageClassCount should count both bindings")
			// Even though two bindings reference the same pool (same agentRef="ppr-target"),
			// ActiveAgents should contain only one entry (deduplicated).
			Expect(fetched.Status.ActiveAgents).To(HaveLen(1),
				"ActiveAgents should be deduplicated — one target even with two bindings")
			Expect(fetched.Status.ActiveAgents[0]).To(Equal(pprTargetName),
				"ActiveAgents should contain the pool's agentRef value")
		})
	})
})
