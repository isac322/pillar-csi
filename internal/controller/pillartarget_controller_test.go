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

//go:build integration

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

	Context("AgentConnected with injected mock Dialer — agent reports unhealthy", func() {
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

		It("should set AgentConnected=False with reason AgentUnhealthy when agent reports unhealthy", func() {
			_, err := unhealthyReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: unhealthyNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, unhealthyNN, fetched)).To(Succeed())

			cond := apimeta.FindStatusCondition(fetched.Status.Conditions, "AgentConnected")
			Expect(cond).NotTo(BeNil(), "AgentConnected condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"AgentConnected should be False when agent reports unhealthy status")
			Expect(cond.Reason).To(Equal("AgentUnhealthy"))
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

	Context("GetCapabilities — status not populated when agent is unhealthy", func() {
		const noCapTargetName = "test-target-no-capabilities"
		noCapNN := types.NamespacedName{Name: noCapTargetName}

		var noCapReconciler *PillarTargetReconciler

		BeforeEach(func() {
			noCapReconciler = &PillarTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				// unhealthy agent: HealthCheck fails, so GetCapabilities is never called
				Dialer: &mockDialer{
					healthy: false,
					capabilitiesResp: &agentv1.GetCapabilitiesResponse{
						AgentVersion: "should-not-appear",
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

		It("should NOT populate agentVersion when agent reports unhealthy", func() {
			_, err := noCapReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: noCapNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, noCapNN, fetched)).To(Succeed())
			Expect(fetched.Status.AgentVersion).To(BeEmpty(),
				"agentVersion should not be set when the agent is unhealthy (HealthCheck returns false)")
		})
	})
})
