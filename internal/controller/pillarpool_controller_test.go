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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("PillarPool Controller", func() {
	const (
		poolName   = "test-pool"
		targetName = "test-pool-target"
	)

	var (
		bctx             context.Context
		reconciler       *PillarPoolReconciler
		poolNamespacedName types.NamespacedName
	)

	BeforeEach(func() {
		bctx = context.Background()
		reconciler = &PillarPoolReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		poolNamespacedName = types.NamespacedName{Name: poolName}
	})

	// Helper: reconcile once and return result + error.
	doReconcile := func() (reconcile.Result, error) {
		return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: poolNamespacedName})
	}

	// Helper: create a minimal PillarPool referencing targetName.
	createPool := func() {
		pool := &pillarcsiv1alpha1.PillarPool{}
		err := k8sClient.Get(bctx, poolNamespacedName, pool)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{
					Name: poolName,
				},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: targetName,
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
					},
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
	}

	// Helper: delete the PillarPool (ignoring not-found).
	deletePool := func() {
		resource := &pillarcsiv1alpha1.PillarPool{}
		if err := k8sClient.Get(bctx, poolNamespacedName, resource); err == nil {
			_ = k8sClient.Delete(bctx, resource)
		}
	}

	// Helper: force-remove finalizer so the object can be GC'd after a test.
	forceRemoveFinalizer := func() {
		resource := &pillarcsiv1alpha1.PillarPool{}
		if err := k8sClient.Get(bctx, poolNamespacedName, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarPoolFinalizer)
			_ = k8sClient.Update(bctx, resource)
		}
	}

	// Helper: create a PillarTarget with name targetName and the given Ready condition.
	createTarget := func(readyStatus *metav1.ConditionStatus, readyMsg string) {
		target := &pillarcsiv1alpha1.PillarTarget{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: targetName}, target)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name: targetName,
				},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "192.0.2.10",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
		if readyStatus != nil {
			// Patch status with the desired Ready condition.
			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: targetName}, fetched)).To(Succeed())
			fetched.Status.ResolvedAddress = "192.0.2.10:9500"
			fetched.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             *readyStatus,
					Reason:             "TestReason",
					Message:            readyMsg,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())
		}
	}

	// Helper: delete the PillarTarget (ignoring not-found).
	deleteTarget := func() {
		resource := &pillarcsiv1alpha1.PillarTarget{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: targetName}, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarTargetFinalizer)
			_ = k8sClient.Update(bctx, resource)
			_ = k8sClient.Delete(bctx, resource)
		}
	}

	// fetchPool fetches the current PillarPool from the API server.
	fetchPool := func() *pillarcsiv1alpha1.PillarPool {
		fetched := &pillarcsiv1alpha1.PillarPool{}
		Expect(k8sClient.Get(bctx, poolNamespacedName, fetched)).To(Succeed())
		return fetched
	}

	// findCondition returns the named condition from a PillarPool, or nil.
	findCondition := func(pool *pillarcsiv1alpha1.PillarPool, condType string) *metav1.Condition {
		return meta.FindStatusCondition(pool.Status.Conditions, condType)
	}

	Context("Finalizer management", func() {
		AfterEach(func() {
			forceRemoveFinalizer()
			deletePool()
		})

		It("should add the pool-protection finalizer on first reconcile", func() {
			createPool()

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// After adding finalizer the reconciler returns immediately without requeue.
			Expect(result.RequeueAfter).To(BeZero())

			fetched := fetchPool()
			Expect(controllerutil.ContainsFinalizer(fetched, pillarPoolFinalizer)).To(BeTrue(),
				"finalizer %q should be present after first reconcile", pillarPoolFinalizer)
		})

		It("should not duplicate the finalizer on subsequent reconciles", func() {
			createPool()

			// First reconcile adds the finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile (normal path) should not duplicate.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			count := 0
			for _, f := range fetched.Finalizers {
				if f == pillarPoolFinalizer {
					count++
				}
			}
			Expect(count).To(Equal(1), "finalizer should appear exactly once")
		})
	})

	Context("TargetReady condition — target does not exist", func() {
		BeforeEach(func() {
			createPool()
			// Run first reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveFinalizer()
			deletePool()
			deleteTarget()
		})

		It("should set TargetReady=False with reason TargetNotFound when PillarTarget is absent", func() {
			// No target created — pool references a non-existent target.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "TargetReady")
			Expect(cond).NotTo(BeNil(), "TargetReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TargetNotFound"))
			Expect(cond.Message).To(ContainSubstring(targetName))
		})

		It("should also set PoolDiscovered=False with reason TargetNotFound when target is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "PoolDiscovered")
			Expect(cond).NotTo(BeNil(), "PoolDiscovered condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TargetNotFound"))
		})

		It("should also set BackendSupported=False with reason TargetNotFound when target is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "BackendSupported")
			Expect(cond).NotTo(BeNil(), "BackendSupported condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TargetNotFound"))
		})

		It("should set Ready=False when target is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Context("TargetReady condition — target exists but not Ready", func() {
		BeforeEach(func() {
			createPool()
			// Run first reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveFinalizer()
			deletePool()
			deleteTarget()
		})

		It("should set TargetReady=False with reason TargetNotReady when target has no Ready condition", func() {
			// Create target without setting any status condition.
			createTarget(nil, "")

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "TargetReady")
			Expect(cond).NotTo(BeNil(), "TargetReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TargetNotReady"))
			Expect(cond.Message).To(ContainSubstring(targetName))
		})

		It("should set TargetReady=False with reason TargetNotReady when target has Ready=False", func() {
			notReadyStatus := metav1.ConditionFalse
			createTarget(&notReadyStatus, "agent is not connected")

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "TargetReady")
			Expect(cond).NotTo(BeNil(), "TargetReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TargetNotReady"))
			// Message should incorporate the target's own Ready condition message.
			Expect(cond.Message).To(ContainSubstring("agent is not connected"))
		})

		It("should set TargetReady=False with reason TargetNotReady when target has Ready=Unknown", func() {
			unknownStatus := metav1.ConditionUnknown
			createTarget(&unknownStatus, "status unknown")

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "TargetReady")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TargetNotReady"))
		})
	})

	Context("TargetReady condition — target exists and is Ready", func() {
		BeforeEach(func() {
			createPool()
			// Run first reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveFinalizer()
			deletePool()
			deleteTarget()
		})

		It("should set TargetReady=True when the referenced PillarTarget has Ready=True", func() {
			readyStatus := metav1.ConditionTrue
			createTarget(&readyStatus, "all checks pass")

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "TargetReady")
			Expect(cond).NotTo(BeNil(), "TargetReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("TargetReady"))
			// Message should include the target name and its resolved address.
			Expect(cond.Message).To(ContainSubstring(targetName))
		})

		It("should reflect transition from not-ready to ready as target status changes", func() {
			// First: target not ready.
			notReadyStatus := metav1.ConditionFalse
			createTarget(&notReadyStatus, "starting up")

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "TargetReady")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))

			// Update target to be Ready=True.
			target := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: targetName}, target)).To(Succeed())
			target.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "AgentConnected",
					Message:            "agent connected successfully",
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, target)).To(Succeed())

			// Reconcile again — TargetReady should flip to True.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched = fetchPool()
			cond = findCondition(fetched, "TargetReady")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"TargetReady should be True after the target becomes Ready")
		})
	})

	Context("Deletion blocking", func() {
		const bindingName = "test-binding-for-pool"

		// Helper: create a PillarBinding referencing this pool.
		createReferencingBinding := func() {
			binding := &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: bindingName,
				},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef:     poolName,
					ProtocolRef: "test-protocol",
				},
			}
			Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		}

		// Helper: delete the referencing binding.
		deleteReferencingBinding := func() {
			binding := &pillarcsiv1alpha1.PillarBinding{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, binding); err == nil {
				Expect(k8sClient.Delete(bctx, binding)).To(Succeed())
			}
		}

		BeforeEach(func() {
			createPool()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile for normal path (status update).
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			deleteReferencingBinding()
			forceRemoveFinalizer()
			deletePool()
		})

		It("should block deletion and retain the finalizer while a PillarBinding references the pool", func() {
			createReferencingBinding()

			// Trigger deletion of the PillarPool.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
			})).To(Succeed())

			// Reconcile — deletion is blocked by the referencing binding.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterPoolDeletionBlock),
				"reconciler should requeue with the deletion-block interval")

			// Finalizer must still be present (object is not gone).
			fetched := fetchPool()
			Expect(controllerutil.ContainsFinalizer(fetched, pillarPoolFinalizer)).To(BeTrue(),
				"finalizer should be retained while the referencing PillarBinding exists")
		})

		It("should remove the finalizer and allow deletion once all referencing PillarBindings are removed", func() {
			createReferencingBinding()

			// Trigger deletion.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
			})).To(Succeed())

			// First reconcile — blocked.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterPoolDeletionBlock))

			// Remove the blocking binding.
			deleteReferencingBinding()

			// Second reconcile — deletion should proceed and finalizer should be removed.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// The object should now be gone (finalizer removed → API server deletes it).
			fetched := &pillarcsiv1alpha1.PillarPool{}
			err = k8sClient.Get(bctx, poolNamespacedName, fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"PillarPool should be deleted once all referencing bindings are removed")
		})

		It("should remove the finalizer immediately when there are no referencing PillarBindings", func() {
			// No binding created — deletion should be unblocked immediately.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
			})).To(Succeed())

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"should not requeue when no bindings are referencing the pool")

			// Object should be gone.
			fetched := &pillarcsiv1alpha1.PillarPool{}
			err = k8sClient.Get(bctx, poolNamespacedName, fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"PillarPool should be deleted when no bindings reference it")
		})

		It("should set a DeletionBlocked status condition while deletion is blocked", func() {
			createReferencingBinding()

			// Trigger deletion.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
			})).To(Succeed())

			// Reconcile — deletion is blocked.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set during deletion block")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("DeletionBlocked"))
			Expect(cond.Message).To(ContainSubstring(bindingName))
		})
	})
})
