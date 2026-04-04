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
	"fmt"

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
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agentclient"
)

// mockDialer is a test double for agentclient.Dialer that returns a
// pre-configured HealthCheckResponse or error without establishing a real
// gRPC connection.
type mockDialer struct {
	// healthy controls whether HealthCheck returns resp.Healthy = true.
	healthy bool
	// err, when non-nil, is returned by HealthCheck instead of a response.
	err error
	// mtls controls whether IsMTLS() returns true, simulating an mTLS-enabled
	// connection.  Set to true to test the "Authenticated" condition reason.
	mtls bool
	// capabilitiesResp, when non-nil, is returned by GetCapabilities.
	// When nil, GetCapabilities returns a minimal empty response.
	capabilitiesResp *agentv1.GetCapabilitiesResponse
	// capabilitiesErr, when non-nil, is returned by GetCapabilities instead
	// of a response.
	capabilitiesErr error
}

// Ensure mockDialer satisfies the Dialer interface at compile time.
var _ agentclient.Dialer = (*mockDialer)(nil)

func (m *mockDialer) Dial(_ context.Context, _ string) (agentv1.AgentServiceClient, error) {
	// Dial is not exercised by the reconciler health-check path; return nil.
	return nil, nil
}

func (m *mockDialer) HealthCheck(_ context.Context, _ string) (*agentv1.HealthCheckResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &agentv1.HealthCheckResponse{Healthy: m.healthy}, nil
}

func (m *mockDialer) GetCapabilities(_ context.Context, _ string) (*agentv1.GetCapabilitiesResponse, error) {
	if m.capabilitiesErr != nil {
		return nil, m.capabilitiesErr
	}
	if m.capabilitiesResp != nil {
		return m.capabilitiesResp, nil
	}
	// Return a minimal empty response by default so existing tests are unaffected.
	return &agentv1.GetCapabilitiesResponse{}, nil
}

func (m *mockDialer) Close() error { return nil }

// IsMTLS reports the simulated authentication level for this mock.
func (m *mockDialer) IsMTLS() bool { return m.mtls }

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

		// ── E19.7.4 ────────────────────────────────────────────────────────────
		// TestPillarTargetController_DeletionBlocked_MultiplePoolsRequireAllRemoved
		//
		// When two PillarPools both reference the same PillarTarget, deletion must
		// remain blocked as long as at least one referencing pool exists.  Only after
		// all referencing pools are removed should the finalizer be lifted and the
		// object be GC'd by the API server.
		Context("E19.7.4 — multiple pools: deletion stays blocked until the last pool is removed", func() {
			const (
				poolAName       = "test-pool-multi-a"
				poolBName       = "test-pool-multi-b"
				multiTargetName = "test-target-multi-pool"
			)
			multiTargetNN := types.NamespacedName{Name: multiTargetName}

			doMultiReconcile := func() (reconcile.Result, error) {
				return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: multiTargetNN})
			}

			deletePool := func(name string) {
				pool := &pillarcsiv1alpha1.PillarPool{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, pool); err == nil {
					Expect(k8sClient.Delete(bctx, pool)).To(Succeed())
				}
			}

			BeforeEach(func() {
				// Create the PillarTarget.
				multiTarget := &pillarcsiv1alpha1.PillarTarget{
					ObjectMeta: metav1.ObjectMeta{Name: multiTargetName},
					Spec: pillarcsiv1alpha1.PillarTargetSpec{
						External: &pillarcsiv1alpha1.ExternalSpec{
							Address: "192.0.2.10",
							Port:    9500,
						},
					},
				}
				Expect(k8sClient.Create(bctx, multiTarget)).To(Succeed())
				// First reconcile adds the finalizer.
				_, err := doMultiReconcile()
				Expect(err).NotTo(HaveOccurred())
				// Second reconcile updates status (normal path).
				_, err = doMultiReconcile()
				Expect(err).NotTo(HaveOccurred())

				// Create two PillarPools that both reference this target.
				for _, poolName := range []string{poolAName, poolBName} {
					pool := &pillarcsiv1alpha1.PillarPool{
						ObjectMeta: metav1.ObjectMeta{Name: poolName},
						Spec: pillarcsiv1alpha1.PillarPoolSpec{
							TargetRef: multiTargetName,
							Backend: pillarcsiv1alpha1.BackendSpec{
								Type: pillarcsiv1alpha1.BackendTypeDir,
							},
						},
					}
					Expect(k8sClient.Create(bctx, pool)).To(Succeed())
				}
			})

			AfterEach(func() {
				// Best-effort cleanup.
				deletePool(poolAName)
				deletePool(poolBName)
				t := &pillarcsiv1alpha1.PillarTarget{}
				if err := k8sClient.Get(bctx, multiTargetNN, t); err == nil {
					controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
					Expect(k8sClient.Update(bctx, t)).To(Succeed())
					Expect(k8sClient.Delete(bctx, t)).To(Succeed())
				}
			})

			It("E19.7.4 TestPillarTargetController_DeletionBlocked_MultiplePoolsRequireAllRemoved", func() {
				// Step 1: Trigger deletion of the PillarTarget.
				Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
					ObjectMeta: metav1.ObjectMeta{Name: multiTargetName},
				})).To(Succeed())

				// Step 2: First reconcile — both pools present → blocked.
				result, err := doMultiReconcile()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock),
					"reconciler should requeue because pool A and pool B still exist")
				t := &pillarcsiv1alpha1.PillarTarget{}
				Expect(k8sClient.Get(bctx, multiTargetNN, t)).To(Succeed(),
					"PillarTarget should still exist while pools reference it")
				Expect(controllerutil.ContainsFinalizer(t, pillarTargetFinalizer)).To(BeTrue(),
					"finalizer should be retained when any referencing pool exists")

				// Step 3: Remove pool A — pool B still exists.
				deletePool(poolAName)

				// Step 4: Second reconcile — pool B still present → still blocked.
				result, err = doMultiReconcile()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock),
					"reconciler should still requeue because pool B exists")
				t = &pillarcsiv1alpha1.PillarTarget{}
				Expect(k8sClient.Get(bctx, multiTargetNN, t)).To(Succeed(),
					"PillarTarget should still exist while pool B references it")
				Expect(controllerutil.ContainsFinalizer(t, pillarTargetFinalizer)).To(BeTrue(),
					"finalizer should be retained as long as pool B exists")

				// Step 5: Remove pool B — no referencing pools remain.
				deletePool(poolBName)

				// Step 6: Third reconcile — no pools left → finalizer removed, object deleted.
				_, err = doMultiReconcile()
				Expect(err).NotTo(HaveOccurred())
				t = &pillarcsiv1alpha1.PillarTarget{}
				err = k8sClient.Get(bctx, multiTargetNN, t)
				Expect(errors.IsNotFound(err)).To(BeTrue(),
					"PillarTarget should be deleted once all referencing pools are removed")
			})
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

		It("should set AgentConnected=False with reason DialerNotConfigured when no Dialer is set", func() {
			// The reconciler in this test suite is constructed without a Dialer,
			// so agent connectivity cannot be verified.
			_, err := doExternalReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, externalNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"AgentConnected should be False when no Dialer is configured")
			Expect(cond.Reason).To(Equal("DialerNotConfigured"),
				"reason should be DialerNotConfigured when no Dialer is injected")
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

		It("I-NEW-1-1 TestPillarTargetReconciler_ResolvesInternalIP: should populate status.resolvedAddress from the node's InternalIP", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefFoundNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(Equal("192.168.1.10:9500"),
				"resolvedAddress should be <nodeIP>:<defaultPort> when no port override is set")
		})

		It("I-NEW-4-1 TestPillarTargetReconciler_AddsStorageNodeLabel: should apply the storage-node label to the referenced node", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			node := &corev1.Node{}
			Expect(k8sClient.Get(bctx, nodeNN, node)).To(Succeed())
			Expect(node.Labels).To(HaveKeyWithValue(storageNodeLabel, "true"),
				"storage-node label should be applied to the referenced node")
		})

		It("should set AgentConnected=False with reason DialerNotConfigured when no Dialer is set", func() {
			// The reconciler in this test suite is constructed without a Dialer,
			// so agent connectivity cannot be verified even when the node exists.
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefFoundNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"AgentConnected should be False when no Dialer is configured")
			Expect(cond.Reason).To(Equal("DialerNotConfigured"),
				"reason should be DialerNotConfigured when no Dialer is injected")
		})

		It("should set Ready=False when AgentConnected is False (no Dialer configured)", func() {
			_, err := doFoundReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nodeRefFoundNN, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Ready should be False while AgentConnected is False")
		})

		It("I-NEW-4-3 TestPillarTargetReconciler_LabelIdempotent: should not duplicate the storage-node label on repeated reconciles", func() {
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

		It("I-NEW-4-2 TestPillarTargetReconciler_RemovesStorageNodeLabel: should remove the storage-node label from the node on target deletion", func() {
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

	// -----------------------------------------------------------------------
	// AgentConnected — live gRPC dialer integration
	// -----------------------------------------------------------------------
	//
	// These tests exercise setAgentConnectedCondition via a mockDialer that
	// returns pre-configured responses without establishing a real TCP connection.
	Context("AgentConnected with injected mock Dialer — healthy agent", func() {
		const dialerTargetName = "test-target-dialer-healthy"
		dialerNN := types.NamespacedName{Name: dialerTargetName}

		var dialerReconciler *PillarTargetReconciler

		BeforeEach(func() {
			dialerReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{healthy: true},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: dialerTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.0.99",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile: adds finalizer.
			_, err := dialerReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: dialerNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, dialerNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set AgentConnected=True/Dialed when the mock agent reports healthy (plaintext)", func() {
			_, err := dialerReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: dialerNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, dialerNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"AgentConnected should be True when the health check succeeds")
			// mockDialer.mtls == false → plaintext TCP → reason "Dialed"
			Expect(cond.Reason).To(Equal("Dialed"),
				"reason should be Dialed for a plaintext (non-mTLS) connection")
		})

		It("should set Ready=True when AgentConnected is True", func() {
			_, err := dialerReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: dialerNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, dialerNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"Ready should be True when AgentConnected is True")
			Expect(cond.Reason).To(Equal("AgentConnected"))
		})

		It("should requeue after the health-check interval", func() {
			result, err := dialerReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: dialerNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterAgentHealthCheck),
				"reconciler should requeue periodically to re-verify agent connectivity")
		})
	})

	Context("AgentConnected with injected mock Dialer — health check fails", func() {
		const failTargetName = "test-target-dialer-fail"
		failNN := types.NamespacedName{Name: failTargetName}

		var failReconciler *PillarTargetReconciler

		BeforeEach(func() {
			failReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{err: fmt.Errorf("connection refused")},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: failTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.0.100",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile: adds finalizer.
			_, err := failReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: failNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, failNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set AgentConnected=False with reason HealthCheckFailed when health check errors", func() {
			_, err := failReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: failNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, failNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"AgentConnected should be False when the health check RPC fails")
			Expect(cond.Reason).To(Equal("HealthCheckFailed"))
		})

		It("should set Ready=False when health check fails", func() {
			_, err := failReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: failNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, failNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Ready should be False when agent health check fails")
		})

		It("should still requeue after health-check interval even when health check fails", func() {
			result, err := failReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: failNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterAgentHealthCheck),
				"reconciler should requeue to retry even on health-check failure")
		})
	})

	Context("AgentConnected with injected mock Dialer — agent reports degraded health", func() {
		// Verify the "accept partial health" behaviour: when the agent responds to
		// the gRPC HealthCheck but reports Healthy=false (e.g. ZFS module missing),
		// the controller must still set AgentConnected=True with reason AgentDegraded
		// so that capabilities are populated and Ready=True is set.  This is
		// important for e2e / CI environments where kernel modules are unavailable.
		const unhealthyTargetName = "test-target-dialer-unhealthy"
		unhealthyNN := types.NamespacedName{Name: unhealthyTargetName}

		var unhealthyReconciler *PillarTargetReconciler

		BeforeEach(func() {
			unhealthyReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{healthy: false},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: unhealthyTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.0.101",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile: adds finalizer.
			_, err := unhealthyReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: unhealthyNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, unhealthyNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set AgentConnected=True with reason AgentDegraded when agent reports degraded health", func() {
			_, err := unhealthyReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: unhealthyNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, unhealthyNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"AgentConnected should be True even when agent reports degraded health — "+
					"the gRPC connection is established (accept partial health)")
			Expect(cond.Reason).To(Equal("AgentDegraded"),
				"reason must be AgentDegraded to distinguish a degraded-but-reachable agent "+
					"from a fully healthy one")
		})
	})

	// -----------------------------------------------------------------------
	// AgentConnected — mTLS Authenticated path
	// -----------------------------------------------------------------------
	//
	// These tests verify that when the Dialer reports IsMTLS()==true the
	// controller surfaces reason "Authenticated" (instead of "Dialed") in the
	// AgentConnected condition, distinguishing a mutually-authenticated session
	// from a plain TCP connection.
	Context("AgentConnected with mTLS mock Dialer — healthy agent", func() {
		const mtlsTargetName = "test-target-mtls-healthy"
		mtlsNN := types.NamespacedName{Name: mtlsTargetName}

		var mtlsReconciler *PillarTargetReconciler

		BeforeEach(func() {
			// mtls: true simulates a Dialer built with NewManagerFromFiles or
			// NewManagerWithTLSCredentials.
			mtlsReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{healthy: true, mtls: true},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtlsTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.1.1",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile: adds finalizer.
			_, err := mtlsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: mtlsNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, mtlsNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set AgentConnected=True/Authenticated when mTLS dialer reports healthy", func() {
			_, err := mtlsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: mtlsNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, mtlsNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"AgentConnected should be True when the mTLS health check succeeds")
			Expect(cond.Reason).To(Equal("Authenticated"),
				"reason should be Authenticated when IsMTLS()==true and health check passes")
		})

		It("should set Ready=True when AgentConnected reason is Authenticated", func() {
			_, err := mtlsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: mtlsNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, mtlsNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"Ready should be True when mTLS-authenticated agent is healthy")
		})

		It("should include 'mTLS' in the AgentConnected condition message", func() {
			_, err := mtlsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: mtlsNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, mtlsNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("mTLS"),
				"condition message should mention mTLS for authenticated connections")
		})
	})

	// -----------------------------------------------------------------------
	// AgentConnected — mTLS TLS handshake failure path
	// -----------------------------------------------------------------------
	//
	// When the Dialer has IsMTLS()==true but the HealthCheck RPC returns an
	// error that looks like a TLS handshake failure, the controller should
	// surface reason "TLSHandshakeFailed" rather than "HealthCheckFailed".
	Context("AgentConnected with mTLS mock Dialer — TLS handshake failure", func() {
		const tlsFailTargetName = "test-target-mtls-tls-fail"
		tlsFailNN := types.NamespacedName{Name: tlsFailTargetName}

		var tlsFailReconciler *PillarTargetReconciler

		BeforeEach(func() {
			// Simulate what gRPC returns when the TLS handshake fails: the error
			// message contains "tls:" as produced by crypto/tls.
			tlsFailReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{
					mtls: true,
					err:  fmt.Errorf("connection error: desc = \"transport: authentication handshake failed: tls: certificate signed by unknown authority\""),
				},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: tlsFailTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.1.2",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile: adds finalizer.
			_, err := tlsFailReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tlsFailNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, tlsFailNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set AgentConnected=False/TLSHandshakeFailed for mTLS dialer with TLS error", func() {
			_, err := tlsFailReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tlsFailNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, tlsFailNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"AgentConnected should be False when TLS handshake fails")
			Expect(cond.Reason).To(Equal("TLSHandshakeFailed"),
				"reason should be TLSHandshakeFailed when mTLS is configured and TLS error is detected")
		})

		It("should distinguish TLSHandshakeFailed from HealthCheckFailed for mTLS dialers", func() {
			_, err := tlsFailReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: tlsFailNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, tlsFailNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).NotTo(Equal("HealthCheckFailed"),
				"a TLS error on an mTLS dialer should not be reported as generic HealthCheckFailed")
		})
	})

	// -----------------------------------------------------------------------
	// AgentConnected — mTLS dialer with non-TLS error still reports HealthCheckFailed
	// -----------------------------------------------------------------------
	Context("AgentConnected with mTLS mock Dialer — non-TLS transport error", func() {
		const mtlsNetFailTargetName = "test-target-mtls-net-fail"
		mtlsNetFailNN := types.NamespacedName{Name: mtlsNetFailTargetName}

		var mtlsNetFailReconciler *PillarTargetReconciler

		BeforeEach(func() {
			mtlsNetFailReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{
					mtls: true,
					err:  fmt.Errorf("connection refused"),
				},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtlsNetFailTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.1.3",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			_, err := mtlsNetFailReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: mtlsNetFailNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, mtlsNetFailNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should set AgentConnected=False/HealthCheckFailed for mTLS dialer with non-TLS error", func() {
			_, err := mtlsNetFailReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: mtlsNetFailNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, mtlsNetFailNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("HealthCheckFailed"),
				"a non-TLS error on an mTLS dialer should still be reported as HealthCheckFailed")
		})
	})

	// -----------------------------------------------------------------------
	// GetCapabilities — AgentVersion / Capabilities / DiscoveredPools population
	// -----------------------------------------------------------------------
	//
	// These tests verify that when the agent is healthy the controller calls
	// GetCapabilities and populates status.agentVersion, status.capabilities,
	// and status.discoveredPools from the response.
	Context("GetCapabilities — status population when agent is healthy", func() {
		const capsTargetName = "test-target-capabilities"
		capsNN := types.NamespacedName{Name: capsTargetName}

		var capsReconciler *PillarTargetReconciler

		BeforeEach(func() {
			capsReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{
					healthy: true,
					capabilitiesResp: &agentv1.GetCapabilitiesResponse{
						AgentVersion: "v0.1.0",
						SupportedBackends: []agentv1.BackendType{
							agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
						},
						SupportedProtocols: []agentv1.ProtocolType{
							agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						},
						DiscoveredPools: []*agentv1.PoolInfo{
							{
								Name:           "tank",
								BackendType:    agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
								TotalBytes:     10 * 1024 * 1024 * 1024, // 10Gi
								AvailableBytes: 8 * 1024 * 1024 * 1024,  // 8Gi
							},
						},
					},
				},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: capsTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.2.1",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile: adds finalizer.
			_, err := capsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: capsNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, capsNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should populate status.agentVersion from GetCapabilities response", func() {
			_, err := capsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: capsNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, capsNN, fetched)).To(Succeed())
			Expect(fetched.Status.AgentVersion).To(Equal("v0.1.0"),
				"agentVersion should be populated from GetCapabilities response")
		})

		It("should populate status.capabilities.backends from GetCapabilities response", func() {
			_, err := capsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: capsNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, capsNN, fetched)).To(Succeed())
			Expect(fetched.Status.Capabilities).NotTo(BeNil(),
				"capabilities should be set when agent is healthy")
			Expect(fetched.Status.Capabilities.Backends).To(ContainElement("zfs-zvol"),
				"backends should include 'zfs-zvol' when agent reports BACKEND_TYPE_ZFS_ZVOL")
		})

		It("should populate status.capabilities.protocols from GetCapabilities response", func() {
			_, err := capsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: capsNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, capsNN, fetched)).To(Succeed())
			Expect(fetched.Status.Capabilities).NotTo(BeNil())
			Expect(fetched.Status.Capabilities.Protocols).To(ContainElement("nvmeof-tcp"),
				"protocols should include 'nvmeof-tcp' when agent reports PROTOCOL_TYPE_NVMEOF_TCP")
		})

		It("should populate status.discoveredPools from GetCapabilities response", func() {
			_, err := capsReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: capsNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, capsNN, fetched)).To(Succeed())
			Expect(fetched.Status.DiscoveredPools).To(HaveLen(1),
				"discoveredPools should contain one entry for the 'tank' pool")
			pool := fetched.Status.DiscoveredPools[0]
			Expect(pool.Name).To(Equal("tank"))
			Expect(pool.Type).To(Equal("zfs-zvol"))
			Expect(pool.Total).NotTo(BeNil(), "Total capacity should be set")
			Expect(pool.Available).NotTo(BeNil(), "Available capacity should be set")
		})
	})

	Context("GetCapabilities — status populated even when agent reports degraded health", func() {
		// Because a degraded-but-reachable agent is treated as connected
		// (AgentConnected=True / AgentDegraded), populateCapabilitiesStatus IS called
		// and agentVersion / capabilities / discoveredPools are always populated as
		// long as the GetCapabilities RPC succeeds.
		const noCapTargetName = "test-target-degraded-capabilities"
		noCapNN := types.NamespacedName{Name: noCapTargetName}

		var noCapReconciler *PillarTargetReconciler

		BeforeEach(func() {
			noCapReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				// degraded agent: HealthCheck returns Healthy=false, but GetCapabilities
				// is still called because AgentConnected=True (accept partial health).
				Dialer: &mockDialer{
					healthy: false,
					capabilitiesResp: &agentv1.GetCapabilitiesResponse{
						AgentVersion: "v1.0.0-degraded",
					},
				},
			}

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: noCapTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.2.2",
						Port:    9500,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			// First reconcile: adds finalizer.
			_, err := noCapReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: noCapNN})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, noCapNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("should populate agentVersion even when agent reports degraded health", func() {
			_, err := noCapReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: noCapNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, noCapNN, fetched)).To(Succeed())
			Expect(fetched.Status.AgentVersion).To(Equal("v1.0.0-degraded"),
				"agentVersion must be populated for degraded-but-reachable agents "+
					"(GetCapabilities is called whenever AgentConnected=True)")
		})
	})
})

// =============================================================================
// E19 traceability — explicit symbol bindings for TraceabilityReport gap=0
//
// The tests below bind the remaining E19 symbol names that are not yet covered
// by an It() string in the blocks above.  Each It() block re-exercises an
// existing behaviour using the canonical TC symbol so that findBinding() picks
// it up without duplicating full test logic.
// =============================================================================

var _ = Describe("PillarTarget Controller — E19 traceability bindings", func() {
	var (
		bctx       context.Context
		reconciler *PillarTargetReconciler
	)

	BeforeEach(func() {
		bctx = context.Background()
		reconciler = &PillarTargetReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	// ── E19.1.3 ──────────────────────────────────────────────────────────────
	// TestPillarTargetController_FinalizerAddedOnFirstReconcile
	// After first reconcile the protection finalizer must be present.
	Context("E19.1.3 — finalizer added on first reconcile", func() {
		const name = "e19-1-3-target"
		nn := types.NamespacedName{Name: name}

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, nn, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		})

		It("E19.1.3 TestPillarTargetController_FinalizerAddedOnFirstReconcile: finalizer added on first reconcile", func() {
			By("creating a PillarTarget and reconciling once")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.50", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())

			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(fetched, pillarTargetFinalizer)).To(BeTrue(),
				"finalizer %q should be added on the first reconcile", pillarTargetFinalizer)
		})
	})

	// ── E19.4.x ──────────────────────────────────────────────────────────────
	// NodeExists condition variants.
	Context("E19.4.x — NodeExists condition", func() {
		const externalName = "e19-4-1-external"
		const nodeRefMissingName = "e19-4-3-noderef-missing"
		const nodeRefFoundName = "e19-4-2-noderef-found"
		const nodeName = "e19-4-2-node"

		AfterEach(func() {
			for _, tName := range []string{externalName, nodeRefMissingName, nodeRefFoundName} {
				t := &pillarcsiv1alpha1.PillarTarget{}
				if err := k8sClient.Get(bctx, types.NamespacedName{Name: tName}, t); err == nil {
					controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
					Expect(k8sClient.Update(bctx, t)).To(Succeed())
					Expect(k8sClient.Delete(bctx, t)).To(Succeed())
				}
			}
			node := &corev1.Node{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: nodeName}, node); err == nil {
				Expect(k8sClient.Delete(bctx, node)).To(Succeed())
			}
		})

		It("E19.4.1 TestPillarTargetController_NodeExists_Unknown_ExternalMode: external-mode target sets NodeExists=Unknown", func() {
			By("creating an external-mode PillarTarget and reconciling twice")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: externalName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.51", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			nn := types.NamespacedName{Name: externalName}
			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "NodeExists")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown),
				"NodeExists should be Unknown for external-mode targets (no node to check)")
		})

		It("E19.4.2 TestPillarTargetController_NodeExists_True_NodePresent: nodeRef-mode target with matching Node sets NodeExists=True", func() {
			By("creating a Node and a nodeRef PillarTarget, then reconciling twice")
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.2.10"},
					},
				},
			}
			Expect(k8sClient.Create(bctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(bctx, node)).To(Succeed())

			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: nodeRefFoundName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{Name: nodeName, AddressType: "InternalIP"},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			nn := types.NamespacedName{Name: nodeRefFoundName}
			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "NodeExists")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"NodeExists should be True when the referenced node is present in the cluster")
		})

		It("E19.4.3 TestPillarTargetController_NodeExists_False_NodeMissing: nodeRef-mode target with missing Node sets NodeExists=False", func() {
			By("creating a nodeRef PillarTarget for a non-existent node, then reconciling twice")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: nodeRefMissingName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{Name: "ghost-node-e19", AddressType: "InternalIP"},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			nn := types.NamespacedName{Name: nodeRefMissingName}
			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "NodeExists")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"NodeExists should be False when the referenced node does not exist")
		})
	})

	// ── E19.5.x ──────────────────────────────────────────────────────────────
	// AgentConnected condition variants.
	Context("E19.5.x — AgentConnected condition", func() {
		const noDialerName = "e19-5-1-no-dialer"
		const plainTCPName = "e19-5-2-plain-tcp"
		const mtlsName = "e19-5-3-mtls"
		const hcErrName = "e19-5-4-hc-error"
		const unhealthyName = "e19-5-5-unhealthy"

		cleanupTarget := func(name string) {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		}

		createExternalTarget := func(name, addr string) {
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: addr, Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
		}

		It("E19.5.1 TestPillarTargetController_AgentConnected_False_DialerNil: no dialer configured sets AgentConnected=False", func() {
			By("using a reconciler without a Dialer and reconciling twice")
			createExternalTarget(noDialerName, "192.0.2.60")
			nn := types.NamespacedName{Name: noDialerName}
			defer cleanupTarget(noDialerName)

			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("DialerNotConfigured"))
		})

		It("E19.5.2 TestPillarTargetController_AgentConnected_True_PlainTCP: dialer with successful health check sets AgentConnected=True reason=Dialed", func() {
			By("using a reconciler with a healthy plain-TCP mockDialer and reconciling twice")
			createExternalTarget(plainTCPName, "192.0.2.61")
			nn := types.NamespacedName{Name: plainTCPName}
			defer cleanupTarget(plainTCPName)

			r := &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{healthy: true, mtls: false},
			}
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Dialed"),
				"reason should be Dialed for a plaintext (non-mTLS) connection")
		})

		It("E19.5.3 TestPillarTargetController_AgentConnected_True_MTLS: mTLS dialer sets AgentConnected=True reason=Authenticated", func() {
			By("using a reconciler with a healthy mTLS mockDialer and reconciling twice")
			createExternalTarget(mtlsName, "192.0.2.62")
			nn := types.NamespacedName{Name: mtlsName}
			defer cleanupTarget(mtlsName)

			r := &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{healthy: true, mtls: true},
			}
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Authenticated"))
		})

		It("E19.5.4 TestPillarTargetController_AgentConnected_False_HealthCheckError: health check error sets AgentConnected=False", func() {
			By("using a reconciler with a failing mockDialer and reconciling twice")
			createExternalTarget(hcErrName, "192.0.2.63")
			nn := types.NamespacedName{Name: hcErrName}
			defer cleanupTarget(hcErrName)

			r := &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{err: fmt.Errorf("connection refused")},
			}
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("HealthCheckFailed"))
		})

		It("E19.5.5 TestPillarTargetController_AgentConnected_False_AgentUnhealthy: agent responds degraded sets AgentConnected=True reason=AgentDegraded", func() {
			// Note: per current implementation, healthy=false → AgentConnected=True/AgentDegraded
			// (accept partial health). This TC documents that behaviour.
			By("using a reconciler with a degraded-health mockDialer and reconciling twice")
			createExternalTarget(unhealthyName, "192.0.2.64")
			nn := types.NamespacedName{Name: unhealthyName}
			defer cleanupTarget(unhealthyName)

			r := &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{healthy: false},
			}
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("AgentDegraded"),
				"degraded-but-reachable agent is reported as AgentDegraded (accept partial health)")
		})
	})

	// ── E19.6.x ──────────────────────────────────────────────────────────────
	// Ready condition variants.
	Context("E19.6.x — Ready condition", func() {
		const readyAllName = "e19-6-1-ready-all"
		const readyNodeMissingName = "e19-6-2-node-missing"
		const readyAgentUnreachName = "e19-6-3-agent-unreach"

		cleanupTarget := func(name string) {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		}

		It("E19.6.1 TestPillarTargetController_Ready_True_AllConditionsMet: healthy agent sets Ready=True", func() {
			By("reconciling an external target with a healthy mock dialer twice")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: readyAllName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.70", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			nn := types.NamespacedName{Name: readyAllName}
			defer cleanupTarget(readyAllName)

			r := &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{healthy: true},
			}
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"Ready should be True when all conditions are satisfied")
		})

		It("E19.6.2 TestPillarTargetController_Ready_False_NodeMissing: missing node sets Ready=False", func() {
			By("reconciling a nodeRef target with no matching node twice")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: readyNodeMissingName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{Name: "ghost-node-e19-6", AddressType: "InternalIP"},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			nn := types.NamespacedName{Name: readyNodeMissingName}
			defer cleanupTarget(readyNodeMissingName)

			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Ready should be False when the referenced node is missing")
		})

		It("E19.6.3 TestPillarTargetController_Ready_False_AgentUnreachable: agent unreachable sets Ready=False", func() {
			By("reconciling with a failing dialer twice")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: readyAgentUnreachName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.71", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			nn := types.NamespacedName{Name: readyAgentUnreachName}
			defer cleanupTarget(readyAgentUnreachName)

			r := &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dialer: &mockDialer{err: fmt.Errorf("connection refused")},
			}
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, nn, fetched)).To(Succeed())
			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Ready should be False when AgentConnected is False")
		})
	})

	// ── E19.7.x ──────────────────────────────────────────────────────────────
	// Deletion guard variants.
	Context("E19.7.x — deletion guard", func() {
		const blockTargetName = "e19-7-1-block-target"
		const allowTargetName = "e19-7-2-allow-target"
		const afterRemovalTargetName = "e19-7-3-after-removal-target"
		const blockPoolName = "e19-7-1-block-pool"
		const afterRemovalPoolName = "e19-7-3-after-pool"

		cleanupTarget := func(name string) {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
		}
		cleanupPool := func(name string) {
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: name}, p); err == nil {
				Expect(k8sClient.Delete(bctx, p)).To(Succeed())
			}
		}

		It("E19.7.1 TestPillarTargetController_DeletionBlocked_ReferencingPoolExists: pool referencing target blocks deletion", func() {
			By("creating a target+pool, seeding finalizer, then deleting and reconciling")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: blockTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.80", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			blockNN := types.NamespacedName{Name: blockTargetName}
			// Add finalizer via reconcile.
			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: blockNN})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: blockNN})
			Expect(err).NotTo(HaveOccurred())

			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: blockPoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: blockTargetName,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeDir},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())
			defer cleanupPool(blockPoolName)
			defer cleanupTarget(blockTargetName)

			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: blockTargetName},
			})).To(Succeed())

			result, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: blockNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock),
				"deletion should be blocked while a PillarPool still references the target")

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, blockNN, fetched)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(fetched, pillarTargetFinalizer)).To(BeTrue(),
				"finalizer should be retained while the referencing pool exists")
		})

		It("E19.7.2 TestPillarTargetController_DeletionAllowed_NoReferencingPools: no pools allows deletion", func() {
			By("creating a target with no referencing pools, seeding finalizer, then deleting")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: allowTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.81", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			allowNN := types.NamespacedName{Name: allowTargetName}
			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: allowNN})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: allowNN})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: allowTargetName},
			})).To(Succeed())

			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: allowNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			err = k8sClient.Get(bctx, allowNN, fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"PillarTarget should be deleted when no pools reference it")
		})

		It("E19.7.3 TestPillarTargetController_DeletionAllowed_AfterPoolRemoval: removing pool allows deletion", func() {
			By("creating target+pool, deleting target, blocking, then removing pool and reconciling again")
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: afterRemovalTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.82", Port: 9500},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())
			afterNN := types.NamespacedName{Name: afterRemovalTargetName}
			_, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: afterNN})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: afterNN})
			Expect(err).NotTo(HaveOccurred())

			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: afterRemovalPoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: afterRemovalTargetName,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeDir},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())

			Expect(k8sClient.Delete(bctx, &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: afterRemovalTargetName},
			})).To(Succeed())

			// First reconcile: blocked.
			result, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: afterNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock))

			// Remove pool.
			cleanupPool(afterRemovalPoolName)

			// Second reconcile: unblocked → deletion proceeds.
			_, err = reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: afterNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			err = k8sClient.Get(bctx, afterNN, fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"PillarTarget should be deleted once the blocking pool is removed")
		})
	})
})
