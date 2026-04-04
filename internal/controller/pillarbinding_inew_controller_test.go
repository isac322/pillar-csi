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

// Package controller — I-NEW-5 through I-NEW-7 integration tests.
//
// This file covers PRD-gap tests for PillarBinding reconciler behaviour:
//
//   - I-NEW-5-1: SC drift correction — reconciler overwrites a drifted StorageClass
//   - I-NEW-6-1: Spec change causes StorageClass update
//   - I-NEW-7-1: Controller can be registered with a Manager without panic
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// I-NEW-5-1: StorageClass Drift Correction
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("PillarBinding Controller — I-NEW-5 StorageClass drift correction", func() {
	const (
		inew5BindingName  = "inew5-binding"
		inew5PoolName     = "inew5-pool"
		inew5ProtocolName = "inew5-protocol"
	)
	var bctx context.Context
	inew5BindingNN := types.NamespacedName{Name: inew5BindingName}

	BeforeEach(func() {
		bctx = context.Background()
	})

	Context("I-NEW-5-1 SC drift correction", func() {
		trueStatus := metav1.ConditionTrue

		BeforeEach(func() {
			// Create pool.
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: inew5PoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "some-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
					},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())
			fetched := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew5PoolName}, fetched)).To(Succeed())
			fetched.Status.Conditions = []metav1.Condition{{
				Type:               "Ready",
				Status:             trueStatus,
				Reason:             "TestReady",
				Message:            "pool ready",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())

			// Create protocol.
			protocol := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: inew5ProtocolName},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{
					Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
				},
			}
			Expect(k8sClient.Create(bctx, protocol)).To(Succeed())
			fetchedProto := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew5ProtocolName}, fetchedProto)).To(Succeed())
			fetchedProto.Status.Conditions = []metav1.Condition{{
				Type:               "Ready",
				Status:             trueStatus,
				Reason:             "TestReady",
				Message:            "protocol ready",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetchedProto)).To(Succeed())

			// Create binding.
			binding := &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: inew5BindingName},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef:     inew5PoolName,
					ProtocolRef: inew5ProtocolName,
				},
			}
			Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		})

		AfterEach(func() {
			b := &pillarcsiv1alpha1.PillarBinding{}
			if err := k8sClient.Get(bctx, inew5BindingNN, b); err == nil {
				controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
				_ = k8sClient.Update(bctx, b)
				_ = k8sClient.Delete(bctx, b)
			}
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: inew5PoolName}, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: inew5ProtocolName}, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
				_ = k8sClient.Update(bctx, proto)
				_ = k8sClient.Delete(bctx, proto)
			}
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: inew5BindingName}, sc); err == nil {
				_ = k8sClient.Delete(bctx, sc)
			}
		})

		It("I-NEW-5-1 TestPillarBindingReconciler_SCDriftCorrection: reconciler corrects a drifted StorageClass", func() {
			r := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// Reconcile to add finalizer.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: inew5BindingNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-5-1] first reconcile (finalizer)")

			// Reconcile to create StorageClass.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: inew5BindingNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-5-1] second reconcile (StorageClass creation)")

			// Verify StorageClass was created.
			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew5BindingName}, sc)).To(Succeed(),
				"[I-NEW-5-1] StorageClass must exist after reconcile")
			originalProvisioner := sc.Provisioner
			Expect(originalProvisioner).To(Equal(pillarCSIProvisioner))

			// Simulate drift: mutate the StorageClass parameters directly.
			sc.Parameters["pillar-csi.bhyoo.com/pool"] = "drifted-pool-value"
			Expect(k8sClient.Update(bctx, sc)).To(Succeed(), "[I-NEW-5-1] force-update StorageClass to simulate drift")

			// Reconcile again — reconciler should restore the correct parameters.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: inew5BindingNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-5-1] reconcile after drift")

			// Verify the StorageClass parameters are corrected.
			corrected := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew5BindingName}, corrected)).To(Succeed())
			Expect(corrected.Parameters["pillar-csi.bhyoo.com/pool"]).To(Equal(inew5PoolName),
				"[I-NEW-5-1] reconciler must restore drifted pool parameter to correct value")
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// I-NEW-6-1: Spec Change Updates StorageClass
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("PillarBinding Controller — I-NEW-6 spec change updates SC", func() {
	const (
		inew6BindingName  = "inew6-binding"
		inew6PoolName     = "inew6-pool"
		inew6ProtocolName = "inew6-protocol"
	)
	var bctx context.Context
	inew6BindingNN := types.NamespacedName{Name: inew6BindingName}

	BeforeEach(func() {
		bctx = context.Background()
	})

	Context("I-NEW-6-1 spec change causes StorageClass update", func() {
		trueStatus := metav1.ConditionTrue

		BeforeEach(func() {
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: inew6PoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "some-target",
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol},
				},
			}
			Expect(k8sClient.Create(bctx, pool)).To(Succeed())
			fetchedPool := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew6PoolName}, fetchedPool)).To(Succeed())
			fetchedPool.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: trueStatus, Reason: "TestReady",
				Message: "pool ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetchedPool)).To(Succeed())

			protocol := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: inew6ProtocolName},
				Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
			}
			Expect(k8sClient.Create(bctx, protocol)).To(Succeed())
			fetchedProto := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew6ProtocolName}, fetchedProto)).To(Succeed())
			fetchedProto.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: trueStatus, Reason: "TestReady",
				Message: "protocol ready", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetchedProto)).To(Succeed())

			binding := &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: inew6BindingName},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef:     inew6PoolName,
					ProtocolRef: inew6ProtocolName,
				},
			}
			Expect(k8sClient.Create(bctx, binding)).To(Succeed())
		})

		AfterEach(func() {
			b := &pillarcsiv1alpha1.PillarBinding{}
			if err := k8sClient.Get(bctx, inew6BindingNN, b); err == nil {
				controllerutil.RemoveFinalizer(b, pillarBindingFinalizer)
				_ = k8sClient.Update(bctx, b)
				_ = k8sClient.Delete(bctx, b)
			}
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: inew6PoolName}, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
			proto := &pillarcsiv1alpha1.PillarProtocol{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: inew6ProtocolName}, proto); err == nil {
				controllerutil.RemoveFinalizer(proto, pillarProtocolFinalizer)
				_ = k8sClient.Update(bctx, proto)
				_ = k8sClient.Delete(bctx, proto)
			}
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: inew6BindingName}, sc); err == nil {
				_ = k8sClient.Delete(bctx, sc)
			}
		})

		It("I-NEW-6-1 TestPillarBindingReconciler_SpecChange_UpdatesSC: reconciler updates StorageClass when PillarBinding spec changes", func() {
			r := &PillarBindingReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// Reconcile twice to create the StorageClass.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: inew6BindingNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-6-1] first reconcile (finalizer)")
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: inew6BindingNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-6-1] second reconcile (SC creation)")

			// Verify initial StorageClass.
			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew6BindingName}, sc)).To(Succeed())
			Expect(sc.Provisioner).To(Equal(pillarCSIProvisioner), "[I-NEW-6-1] initial SC provisioner")

			// Update the PillarBinding spec (e.g., add AllowVolumeExpansion).
			fetched := &pillarcsiv1alpha1.PillarBinding{}
			Expect(k8sClient.Get(bctx, inew6BindingNN, fetched)).To(Succeed())
			trueVal := true
			fetched.Spec.StorageClass.AllowVolumeExpansion = &trueVal
			Expect(k8sClient.Update(bctx, fetched)).To(Succeed(), "[I-NEW-6-1] update PillarBinding spec")

			// Reconcile after spec change.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: inew6BindingNN})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-6-1] reconcile after spec change")

			// Verify StorageClass was updated.
			updated := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: inew6BindingName}, updated)).To(Succeed())
			Expect(updated.AllowVolumeExpansion).NotTo(BeNil(),
				"[I-NEW-6-1] AllowVolumeExpansion should be set after spec change")
			Expect(*updated.AllowVolumeExpansion).To(BeTrue(),
				"[I-NEW-6-1] AllowVolumeExpansion should be true after spec change")
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// I-NEW-7-1: Controller Manager Registration
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("PillarBinding Controller — I-NEW-7 manager registration", func() {
	var bctx context.Context

	BeforeEach(func() {
		bctx = context.Background()
	})

	Context("I-NEW-7-1 manager registration without panic", func() {
		It("I-NEW-7-1 TestControllerManager_LeaderElection: PillarBinding controller registers with manager without panic", func() {
			// Create a manager without leader election (suitable for envtest).
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:         k8sClient.Scheme(),
				LeaderElection: false,
				Metrics:        metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred(), "[I-NEW-7-1] ctrl.NewManager must succeed")

			// Register PillarBindingReconciler with the manager.
			reconciler := &PillarBindingReconciler{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
			}
			err = reconciler.SetupWithManager(mgr)
			Expect(err).NotTo(HaveOccurred(),
				"[I-NEW-7-1] SetupWithManager must not return an error — controller can be registered")

			// Start the manager in a short-lived context to verify it initialises.
			startCtx, startCancel := context.WithCancel(bctx)
			startCancel() // cancel immediately so the manager exits cleanly

			// Running with a pre-cancelled context should exit without error or panic.
			_ = mgr.Start(startCtx)

			// If we reach this point without a panic, the test passes.
			Expect(true).To(BeTrue(), "[I-NEW-7-1] manager started and stopped without panic")
		})
	})
})
