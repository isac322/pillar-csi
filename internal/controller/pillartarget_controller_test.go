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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("PillarTarget Controller", func() {
	const targetName = "test-target"

	var (
		bctx                 context.Context
		reconciler           *PillarTargetReconciler
		targetNamespacedName types.NamespacedName
	)

	BeforeEach(func() {
		bctx = context.Background()
		reconciler = &PillarTargetReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		targetNamespacedName = types.NamespacedName{Name: targetName}
	})

	// Helper: reconcile once and return error.
	doReconcile := func() (reconcile.Result, error) {
		return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: targetNamespacedName})
	}

	// Helper: create a minimal external PillarTarget (no nodeRef so no Node lookup required).
	createTarget := func() {
		target := &pillarcsiv1alpha1.PillarTarget{}
		err := k8sClient.Get(bctx, targetNamespacedName, target)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name: targetName,
				},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "192.0.2.1",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
	}

	// Helper: delete the PillarTarget (ignoring not-found).
	deleteTarget := func() {
		resource := &pillarcsiv1alpha1.PillarTarget{}
		if err := k8sClient.Get(bctx, targetNamespacedName, resource); err == nil {
			_ = k8sClient.Delete(bctx, resource)
		}
	}

	// Helper: force-remove finalizer so the object can be GC'd after a test.
	forceRemoveFinalizer := func() {
		resource := &pillarcsiv1alpha1.PillarTarget{}
		if err := k8sClient.Get(bctx, targetNamespacedName, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarTargetFinalizer)
			_ = k8sClient.Update(bctx, resource)
		}
	}

	Context("Finalizer management", func() {
		AfterEach(func() {
			forceRemoveFinalizer()
			deleteTarget()
		})

		It("should add the pillar-target-protection finalizer on first reconcile", func() {
			createTarget()

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// After adding the finalizer the reconciler returns immediately (no requeue requested).
			Expect(result.RequeueAfter).To(BeZero())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, targetNamespacedName, fetched)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(fetched, pillarTargetFinalizer)).To(BeTrue(),
				"finalizer %q should be present after first reconcile", pillarTargetFinalizer)
		})
	})

	Context("Deletion blocking", func() {
		const poolName = "test-pool-for-target"

		// Helper: create a PillarPool that references this target.
		createReferencingPool := func() {
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{
					Name: poolName,
				},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: targetName,
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeDir,
					},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())
		}

		// Helper: delete the referencing pool.
		deleteReferencingPool := func() {
			pool := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, pool); err == nil {
				Expect(k8sClient.Delete(bctx, pool)).To(Succeed())
			}
		}

		BeforeEach(func() {
			createTarget()
			// Seed the finalizer so deletion reconcile is reached.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile for normal path (status update).
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			deleteReferencingPool()
			forceRemoveFinalizer()
			deleteTarget()
		})

		It("should block deletion and retain the finalizer while a PillarPool references the target", func() {
			createReferencingPool()

			// Trigger deletion of the PillarTarget.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: targetName},
			})).To(Succeed())

			// Reconcile — deletion is blocked by the referencing pool.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock),
				"reconciler should requeue with the deletion-block interval")

			// Finalizer must still be present (object is not gone).
			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, targetNamespacedName, fetched)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(fetched, pillarTargetFinalizer)).To(BeTrue(),
				"finalizer should be retained while the referencing PillarPool exists")
		})

		It("should remove the finalizer and allow deletion once all referencing PillarPools are removed", func() {
			createReferencingPool()

			// Trigger deletion.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: targetName},
			})).To(Succeed())

			// First reconcile — blocked.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock))

			// Remove the blocking pool.
			deleteReferencingPool()

			// Second reconcile — deletion should proceed and finalizer should be removed.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// The object should now be gone (finalizer removed → API server deletes it).
			fetched := &pillarcsiv1alpha1.PillarTarget{}
			err = k8sClient.Get(bctx, targetNamespacedName, fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"PillarTarget should be deleted once all referencing pools are removed")
		})

		It("should remove the finalizer immediately when there are no referencing PillarPools", func() {
			// No pool created — deletion should be unblocked immediately.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: targetName},
			})).To(Succeed())

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(),
				"should not requeue when no pools are referencing the target")

			// Object should be gone.
			fetched := &pillarcsiv1alpha1.PillarTarget{}
			err = k8sClient.Get(bctx, targetNamespacedName, fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"PillarTarget should be deleted when no pools reference it")
		})
	})
})
