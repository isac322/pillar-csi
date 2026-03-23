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
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("PillarBinding Controller", func() {
	const (
		bindingName  = "test-binding"
		poolName     = "test-binding-pool"
		protocolName = "test-binding-protocol"
	)

	var (
		bctx               context.Context
		reconciler         *PillarBindingReconciler
		bindingNamespacedName types.NamespacedName
	)

	BeforeEach(func() {
		bctx = context.Background()
		reconciler = &PillarBindingReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		bindingNamespacedName = types.NamespacedName{Name: bindingName}
	})

	// doReconcile triggers a single reconcile pass and returns result + error.
	doReconcile := func() (reconcile.Result, error) {
		return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: bindingNamespacedName})
	}

	// createBinding creates a minimal PillarBinding referencing poolName and protocolName.
	createBinding := func() {
		binding := &pillarcsiv1alpha1.PillarBinding{}
		err := k8sClient.Get(bctx, bindingNamespacedName, binding)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: bindingName,
				},
				Spec: pillarcsiv1alpha1.PillarBindingSpec{
					PoolRef:     poolName,
					ProtocolRef: protocolName,
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
	}

	// deleteBinding deletes the PillarBinding (ignoring not-found).
	deleteBinding := func() {
		resource := &pillarcsiv1alpha1.PillarBinding{}
		if err := k8sClient.Get(bctx, bindingNamespacedName, resource); err == nil {
			_ = k8sClient.Delete(bctx, resource)
		}
	}

	// forceRemoveBindingFinalizer strips the finalizer so the object can be GC'd.
	forceRemoveBindingFinalizer := func() {
		resource := &pillarcsiv1alpha1.PillarBinding{}
		if err := k8sClient.Get(bctx, bindingNamespacedName, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarBindingFinalizer)
			_ = k8sClient.Update(bctx, resource)
		}
	}

	// createPool creates a PillarPool with an optional Ready condition.
	// readyStatus == nil means no condition is set on the pool.
	createPool := func(readyStatus *metav1.ConditionStatus, msg string) {
		pool := &pillarcsiv1alpha1.PillarPool{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, pool)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{
					Name: poolName,
				},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "some-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
					},
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
		if readyStatus != nil {
			fetched := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())
			fetched.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             *readyStatus,
					Reason:             "TestReason",
					Message:            msg,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())
		}
	}

	// deletePool deletes the PillarPool, removing any finalizer first.
	deletePool := func() {
		resource := &pillarcsiv1alpha1.PillarPool{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarPoolFinalizer)
			_ = k8sClient.Update(bctx, resource)
			_ = k8sClient.Delete(bctx, resource)
		}
	}

	// createProtocol creates a PillarProtocol with an optional Ready condition.
	// readyStatus == nil means no condition is set on the protocol.
	createProtocol := func(readyStatus *metav1.ConditionStatus, msg string) {
		protocol := &pillarcsiv1alpha1.PillarProtocol{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, protocol)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{
					Name: protocolName,
				},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{
					Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
		if readyStatus != nil {
			fetched := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, fetched)).To(Succeed())
			fetched.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             *readyStatus,
					Reason:             "TestReason",
					Message:            msg,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())
		}
	}

	// deleteProtocol deletes the PillarProtocol, removing any finalizer first.
	deleteProtocol := func() {
		resource := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarProtocolFinalizer)
			_ = k8sClient.Update(bctx, resource)
			_ = k8sClient.Delete(bctx, resource)
		}
	}

	// fetchBinding fetches the current PillarBinding from the API server.
	fetchBinding := func() *pillarcsiv1alpha1.PillarBinding {
		fetched := &pillarcsiv1alpha1.PillarBinding{}
		Expect(k8sClient.Get(bctx, bindingNamespacedName, fetched)).To(Succeed())
		return fetched
	}

	// findBindingCondition returns the named condition from a binding, or nil.
	findBindingCondition := func(binding *pillarcsiv1alpha1.PillarBinding, condType string) *metav1.Condition {
		return apimeta.FindStatusCondition(binding.Status.Conditions, condType)
	}

	trueStatus := metav1.ConditionTrue
	falseStatus := metav1.ConditionFalse

	// -------------------------------------------------------------------------
	Context("Finalizer management", func() {
		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
		})

		It("should add the binding-protection finalizer on first reconcile", func() {
			createBinding()

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// After adding finalizer the reconciler returns immediately without requeue.
			Expect(result.RequeueAfter).To(BeZero())

			fetched := fetchBinding()
			Expect(controllerutil.ContainsFinalizer(fetched, pillarBindingFinalizer)).To(BeTrue(),
				"finalizer %q should be present after first reconcile", pillarBindingFinalizer)
		})

		It("should not duplicate the finalizer on subsequent reconciles", func() {
			createBinding()

			// First reconcile adds the finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile (normal path) should not duplicate.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			count := 0
			for _, f := range fetched.Finalizers {
				if f == pillarBindingFinalizer {
					count++
				}
			}
			Expect(count).To(Equal(1), "finalizer should appear exactly once")
		})
	})

	// -------------------------------------------------------------------------
	Context("PoolReady condition — pool does not exist", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
		})

		It("should set PoolReady=False with reason PoolNotFound when PillarPool is absent", func() {
			// No pool created — binding references a non-existent pool.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotFound"))
			Expect(cond.Message).To(ContainSubstring(poolName))

			// Requeue after a delay is expected when pool is not found.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when pool is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotFound"))
		})
	})

	// -------------------------------------------------------------------------
	Context("PoolReady condition — pool exists but is not Ready", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create pool with Ready=False.
			createPool(&falseStatus, "target not found")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
		})

		It("should set PoolReady=False with reason PoolNotReady", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotReady"))
			Expect(cond.Message).To(ContainSubstring(poolName))

			// Requeue after a delay is expected when pool is not ready.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when pool is not ready", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotReady"))
		})

		It("should include the pool's failure message in PoolReady condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("target not found"))
		})
	})

	// -------------------------------------------------------------------------
	Context("PoolReady condition — pool exists with no Ready condition yet", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create pool without any condition (nil readyStatus).
			createPool(nil, "")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
		})

		It("should set PoolReady=False when pool has no Ready condition", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotReady"))

			// Requeue expected.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})
	})

	// -------------------------------------------------------------------------
	Context("ProtocolValid condition — protocol does not exist", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create a ready pool so pool validation passes.
			createPool(&trueStatus, "pool is ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set ProtocolValid=False with reason ProtocolNotFound when PillarProtocol is absent", func() {
			// No protocol created — binding references a non-existent protocol.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotFound"))
			Expect(cond.Message).To(ContainSubstring(protocolName))

			// Requeue after a delay is expected when protocol is not found.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when protocol is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotFound"))
		})

		It("should set PoolReady=True even when protocol is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// -------------------------------------------------------------------------
	Context("ProtocolValid condition — protocol exists but is not Ready", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create a ready pool so pool validation passes.
			createPool(&trueStatus, "pool is ready")
			// Create protocol with Ready=False.
			createProtocol(&falseStatus, "protocol initialization failed")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set ProtocolValid=False with reason ProtocolNotReady", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotReady"))
			Expect(cond.Message).To(ContainSubstring(protocolName))

			// Requeue after a delay is expected when protocol is not ready.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when protocol is not ready", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotReady"))
		})

		It("should include the protocol's failure message in ProtocolValid condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("protocol initialization failed"))
		})
	})

	// -------------------------------------------------------------------------
	Context("ProtocolValid condition — protocol exists with no Ready condition yet", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool is ready")
			// Create protocol without any condition.
			createProtocol(nil, "")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set ProtocolValid=False when protocol has no Ready condition", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotReady"))

			// Requeue expected.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})
	})

	// -------------------------------------------------------------------------
	Context("Both pool and protocol are Ready", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create ready pool and ready protocol.
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			// Clean up any StorageClass that may have been created.
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				_ = k8sClient.Delete(bctx, sc)
			}
		})

		It("should set PoolReady=True and ProtocolValid=True", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			poolCond := findBindingCondition(fetched, conditionPoolReady)
			Expect(poolCond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(poolCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(poolCond.Reason).To(Equal("PoolReady"))

			protoCond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(protoCond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(protoCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(protoCond.Reason).To(Equal("ProtocolValid"))
		})

		It("should set Compatible=True for compatible backend/protocol types", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			compatCond := findBindingCondition(fetched, conditionCompatible)
			Expect(compatCond).NotTo(BeNil(), "Compatible condition should be set")
			Expect(compatCond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should set StorageClassCreated=True and create the StorageClass", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			scCond := findBindingCondition(fetched, conditionStorageClassCreated)
			Expect(scCond).NotTo(BeNil(), "StorageClassCreated condition should be set")
			Expect(scCond.Status).To(Equal(metav1.ConditionTrue))

			// Verify the StorageClass was actually created.
			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())
			Expect(sc.Provisioner).To(Equal(pillarCSIProvisioner))
		})

		It("should set Ready=True when all conditions pass", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			readyCond := findBindingCondition(fetched, conditionReady)
			Expect(readyCond).NotTo(BeNil(), "Ready condition should be set")
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal("AllConditionsMet"))
		})

		It("should set status.storageClassName when ready", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			Expect(fetched.Status.StorageClassName).To(Equal(bindingName))
		})

		It("should not requeue when everything is ready", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(result.Requeue).To(BeFalse())
		})
	})

	// -------------------------------------------------------------------------
	Context("Incompatible backend/protocol — NFS with block backend", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Create pool with a block-only backend (zfs-zvol).
			createPool(&trueStatus, "pool ready")

			// Create an NFS protocol (incompatible with zfs-zvol).
			protocol := &pillarcsiv1alpha1.PillarProtocol{}
			err = k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, protocol)
			if err != nil && errors.IsNotFound(err) {
				resource := &pillarcsiv1alpha1.PillarProtocol{
					ObjectMeta: metav1.ObjectMeta{
						Name: protocolName,
					},
					Spec: pillarcsiv1alpha1.PillarProtocolSpec{
						Type: pillarcsiv1alpha1.ProtocolTypeNFS,
					},
				}
				Expect(k8sClient.Create(bctx, resource)).To(Succeed())
			}
			fetched := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, fetched)).To(Succeed())
			fetched.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "TestReason",
					Message:            "protocol ready",
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set Compatible=False with reason Incompatible", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionCompatible)
			Expect(cond).NotTo(BeNil(), "Compatible condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Incompatible"))
		})

		It("should set Ready=False with reason Incompatible", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Incompatible"))
		})

		It("should mention the incompatible backend type in the Compatible condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionCompatible)
			Expect(cond).NotTo(BeNil())
			// Message should name the offending backend.
			Expect(cond.Message).To(ContainSubstring(string(pillarcsiv1alpha1.BackendTypeZFSZvol)))
		})
	})

	// -------------------------------------------------------------------------
	Context("Incompatible backend/protocol — block protocol with dir backend", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Create pool with dir backend (file-system only — cannot serve block devices).
			dirPool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "some-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeDir,
					},
				},
			}
			Expect(k8sClient.Create(bctx, dirPool)).To(Succeed())
			fetchedPool := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetchedPool)).To(Succeed())
			fetchedPool.Status.Conditions = []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "TestReason",
				Message:            "pool ready",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetchedPool)).To(Succeed())

			// Use an NVMeOF-TCP protocol — incompatible with dir backend.
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set Compatible=False with reason Incompatible", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionCompatible)
			Expect(cond).NotTo(BeNil(), "Compatible condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Incompatible"))
			Expect(cond.Message).To(ContainSubstring(string(pillarcsiv1alpha1.BackendTypeDir)))
		})

		It("should set Ready=False with reason Incompatible", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Incompatible"))
		})

		It("should not create a StorageClass when incompatible", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			err = k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "StorageClass should NOT be created when incompatible")
		})
	})

	// -------------------------------------------------------------------------
	Context("Incompatible backend/protocol — block protocol with zfs-dataset backend", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Create pool with zfs-dataset backend (filesystem — cannot serve block devices).
			dsPool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "some-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSDataset,
						ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
							Pool: "data",
						},
					},
				},
			}
			Expect(k8sClient.Create(bctx, dsPool)).To(Succeed())
			fetchedPool := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetchedPool)).To(Succeed())
			fetchedPool.Status.Conditions = []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "TestReason",
				Message:            "pool ready",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, fetchedPool)).To(Succeed())

			// Use an NVMeOF-TCP protocol — incompatible with zfs-dataset backend.
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set Compatible=False with reason Incompatible", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionCompatible)
			Expect(cond).NotTo(BeNil(), "Compatible condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Incompatible"))
			Expect(cond.Message).To(ContainSubstring(string(pillarcsiv1alpha1.BackendTypeZFSDataset)))
		})

		It("should set Ready=False with reason Incompatible", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Incompatible"))
		})
	})
})

// =============================================================================
// Unit tests for evaluateCompatibility (no envtest / API server required)
// =============================================================================

var _ = Describe("evaluateCompatibility", func() {
	// helpers to build minimal objects without persisting to a cluster.
	makePool := func(backendType pillarcsiv1alpha1.BackendType) *pillarcsiv1alpha1.PillarPool {
		return &pillarcsiv1alpha1.PillarPool{
			Spec: pillarcsiv1alpha1.PillarPoolSpec{
				TargetRef: "test-target",
				Backend:   pillarcsiv1alpha1.BackendSpec{Type: backendType},
			},
		}
	}
	makeProtocol := func(protoType pillarcsiv1alpha1.ProtocolType) *pillarcsiv1alpha1.PillarProtocol {
		return &pillarcsiv1alpha1.PillarProtocol{
			Spec: pillarcsiv1alpha1.PillarProtocolSpec{Type: protoType},
		}
	}

	// -------------------------------------------------------------------------
	// Compatible pairs: block backends ↔ block protocols
	DescribeTable("compatible combinations return (empty, true)",
		func(backend pillarcsiv1alpha1.BackendType, proto pillarcsiv1alpha1.ProtocolType) {
			msg, ok := evaluateCompatibility(makePool(backend), makeProtocol(proto))
			Expect(ok).To(BeTrue(), "expected compatible, got incompatible: %s", msg)
			Expect(msg).To(BeEmpty())
		},
		Entry("zfs-zvol + nvmeof-tcp", pillarcsiv1alpha1.BackendTypeZFSZvol, pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP),
		Entry("zfs-zvol + iscsi", pillarcsiv1alpha1.BackendTypeZFSZvol, pillarcsiv1alpha1.ProtocolTypeISCSI),
		Entry("lvm-lv + nvmeof-tcp", pillarcsiv1alpha1.BackendTypeLVMLV, pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP),
		Entry("lvm-lv + iscsi", pillarcsiv1alpha1.BackendTypeLVMLV, pillarcsiv1alpha1.ProtocolTypeISCSI),
		Entry("zfs-dataset + nfs", pillarcsiv1alpha1.BackendTypeZFSDataset, pillarcsiv1alpha1.ProtocolTypeNFS),
		Entry("dir + nfs", pillarcsiv1alpha1.BackendTypeDir, pillarcsiv1alpha1.ProtocolTypeNFS),
	)

	// -------------------------------------------------------------------------
	// Incompatible pairs
	DescribeTable("incompatible combinations return (non-empty, false)",
		func(backend pillarcsiv1alpha1.BackendType, proto pillarcsiv1alpha1.ProtocolType) {
			msg, ok := evaluateCompatibility(makePool(backend), makeProtocol(proto))
			Expect(ok).To(BeFalse(), "expected incompatible, got compatible for %s + %s", backend, proto)
			Expect(msg).NotTo(BeEmpty(), "incompatibility message should not be empty")
			// Message should mention the problematic backend type.
			Expect(msg).To(ContainSubstring(string(backend)))
		},
		// NFS with block-only backends (Rule 1)
		Entry("zfs-zvol + nfs", pillarcsiv1alpha1.BackendTypeZFSZvol, pillarcsiv1alpha1.ProtocolTypeNFS),
		Entry("lvm-lv + nfs", pillarcsiv1alpha1.BackendTypeLVMLV, pillarcsiv1alpha1.ProtocolTypeNFS),
		// Block protocols with file-only backends (Rule 2)
		Entry("dir + nvmeof-tcp", pillarcsiv1alpha1.BackendTypeDir, pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP),
		Entry("dir + iscsi", pillarcsiv1alpha1.BackendTypeDir, pillarcsiv1alpha1.ProtocolTypeISCSI),
		Entry("zfs-dataset + nvmeof-tcp", pillarcsiv1alpha1.BackendTypeZFSDataset, pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP),
		Entry("zfs-dataset + iscsi", pillarcsiv1alpha1.BackendTypeZFSDataset, pillarcsiv1alpha1.ProtocolTypeISCSI),
	)

	// -------------------------------------------------------------------------
	// Verify message content for Rule 1 violation (NFS + block backend)
	It("should mention NFS and suggest block protocols in Rule-1 message", func() {
		msg, ok := evaluateCompatibility(
			makePool(pillarcsiv1alpha1.BackendTypeZFSZvol),
			makeProtocol(pillarcsiv1alpha1.ProtocolTypeNFS),
		)
		Expect(ok).To(BeFalse())
		Expect(msg).To(ContainSubstring("NFS"))
		Expect(msg).To(ContainSubstring("nvmeof-tcp"))
	})

	// Verify message content for Rule 2 violation (block protocol + file-only backend)
	It("should mention block protocol and suggest NFS in Rule-2 message", func() {
		msg, ok := evaluateCompatibility(
			makePool(pillarcsiv1alpha1.BackendTypeDir),
			makeProtocol(pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP),
		)
		Expect(ok).To(BeFalse())
		Expect(msg).To(ContainSubstring("NFS"))
		Expect(msg).To(ContainSubstring(string(pillarcsiv1alpha1.BackendTypeDir)))
	})
})
