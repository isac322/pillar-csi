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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
			Expect(k8sClient.Delete(bctx, resource)).To(Succeed())
		}
	}

	// Helper: force-remove finalizer so the object can be GC'd after a test.
	forceRemoveFinalizer := func() {
		resource := &pillarcsiv1alpha1.PillarTarget{}
		if err := k8sClient.Get(bctx, targetNamespacedName, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarTargetFinalizer)
			Expect(k8sClient.Update(bctx, resource)).To(Succeed())
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

	// -----------------------------------------------------------------------
	// External mode status conditions
	// -----------------------------------------------------------------------
	Context("External mode — status condition updates", func() {
		const externalTargetName = "test-target-external"
		externalNN := types.NamespacedName{Name: externalTargetName}

		doExternalReconcile := func() (reconcile.Result, error) {
			return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: externalNN})
		}

		BeforeEach(func() {
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: externalTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.0.1",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile adds finalizer.
			_, err := doExternalReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, externalNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set NodeExists=Unknown for an external-mode target", func() {
			_, err := doExternalReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, externalNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "NodeExists")
			Expect(cond).NotTo(BeNil(), "NodeExists condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown),
				"NodeExists should be Unknown for external-mode targets")
			Expect(cond.Reason).To(Equal("ExternalMode"))
		})

		It("should set AgentConnected=False for an external-mode target (stubbed)", func() {
			_, err := doExternalReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, externalNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"AgentConnected should be False until gRPC connection is implemented")
		})

		It("should set Ready=False for an external-mode target while AgentConnected is False", func() {
			_, err := doExternalReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, externalNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Ready should be False while AgentConnected is False")
		})

		It("should set status.resolvedAddress from spec.external address:port", func() {
			_, err := doExternalReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, externalNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(Equal("10.0.0.1:9500"),
				"resolvedAddress should be address:port from spec.external")
		})
	})

	// -----------------------------------------------------------------------
	// NodeRef mode — status condition updates and node auto-labeling
	// -----------------------------------------------------------------------
	Context("NodeRef mode — node not found", func() {
		const nodeRefTargetName = "test-target-noderef-missing"
		nodeRefNN := types.NamespacedName{Name: nodeRefTargetName}

		doNodeRefReconcile := func() (reconcile.Result, error) {
			return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nodeRefNN})
		}

		BeforeEach(func() {
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: nodeRefTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name:        "nonexistent-node",
						AddressType: "InternalIP",
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile adds finalizer.
			_, err := doNodeRefReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, nodeRefNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set NodeExists=False when the referenced node does not exist", func() {
			_, err := doNodeRefReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "NodeExists")
			Expect(cond).NotTo(BeNil(), "NodeExists condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"NodeExists should be False when the referenced node does not exist")
			Expect(cond.Reason).To(Equal("NodeNotFound"))
		})

		It("should set AgentConnected=False when the referenced node does not exist", func() {
			_, err := doNodeRefReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NodeNotFound"))
		})

		It("should set Ready=False when the referenced node does not exist", func() {
			_, err := doNodeRefReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NodeNotFound"))
		})

		It("should clear status.resolvedAddress when the node does not exist", func() {
			_, err := doNodeRefReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(BeEmpty(),
				"resolvedAddress should be empty when the node is missing")
		})
	})

	Context("NodeRef mode — node found and auto-labeling", func() {
		const (
			nodeRefFoundTargetName = "test-target-noderef-found"
			nodeRefFoundNodeName   = "test-storage-node"
		)
		nodeRefFoundNN := types.NamespacedName{Name: nodeRefFoundTargetName}
		nodeNN := types.NamespacedName{Name: nodeRefFoundNodeName}

		doFoundReconcile := func() (reconcile.Result, error) {
			return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nodeRefFoundNN})
		}

		BeforeEach(func() {
			// Create the node that the PillarTarget will reference.
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: nodeRefFoundNodeName},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					},
				},
			}
			Expect(k8sClient.Create(bctx, node)).To(Succeed())
			// envtest doesn't run NodeStatus sub-resource automatically — patch status directly.
			Expect(k8sClient.Status().Update(bctx, node)).To(Succeed())

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: nodeRefFoundTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name:        nodeRefFoundNodeName,
						AddressType: "InternalIP",
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile adds finalizer.
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			// Remove label from node before deletion to ensure clean state.
			node := &corev1.Node{}
			if err := k8sClient.Get(bctx, nodeNN, node); err == nil {
				if node.Labels != nil {
					delete(node.Labels, storageNodeLabel)
					Expect(k8sClient.Update(bctx, node)).To(Succeed())
				}
				Expect(k8sClient.Delete(bctx, node)).To(Succeed())
			}
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, nodeRefFoundNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set NodeExists=True when the referenced node exists", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefFoundNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "NodeExists")
			Expect(cond).NotTo(BeNil(), "NodeExists condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"NodeExists should be True when the referenced node exists in the cluster")
			Expect(cond.Reason).To(Equal("NodeFound"))
		})

		It("should populate status.resolvedAddress from the node's InternalIP", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefFoundNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(Equal("192.168.1.10:9500"),
				"resolvedAddress should be <nodeIP>:<defaultPort> when no port override is set")
		})

		It("should apply the storage-node label to the referenced node", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			node := &corev1.Node{}
			Expect(k8sClient.Get(bctx, nodeNN, node)).To(Succeed())
			Expect(node.Labels).To(HaveKeyWithValue(storageNodeLabel, "true"),
				"storage-node label should be applied to the referenced node")
		})

		It("should set AgentConnected=False (stubbed) even when node is found", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefFoundNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"AgentConnected should remain False until gRPC connection is implemented")
		})

		It("should set Ready=False (stubbed) even when node is found", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefFoundNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Ready should be False while AgentConnected is False (stub phase)")
		})

		It("should not duplicate the storage-node label on repeated reconciles", func() {
			// Reconcile twice — label should still be present exactly once.
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())
			_, err = doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			node := &corev1.Node{}
			Expect(k8sClient.Get(bctx, nodeNN, node)).To(Succeed())
			Expect(node.Labels[storageNodeLabel]).To(Equal("true"),
				"label value should remain 'true' after multiple reconciles")
		})

		It("should remove the storage-node label from the node on target deletion", func() {
			// First ensure label is applied.
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			node := &corev1.Node{}
			Expect(k8sClient.Get(bctx, nodeNN, node)).To(Succeed())
			Expect(node.Labels).To(HaveKey(storageNodeLabel), "label should be present before deletion")

			// Trigger deletion.
			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: nodeRefFoundTargetName},
			})).To(Succeed())

			// Reconcile — no blocking pools, finalizer removed.
			_, err = doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Node label should be removed.
			node = &corev1.Node{}
			Expect(k8sClient.Get(bctx, nodeNN, node)).To(Succeed())
			Expect(node.Labels).NotTo(HaveKey(storageNodeLabel),
				"storage-node label should be removed when PillarTarget is deleted")
		})
	})
})
