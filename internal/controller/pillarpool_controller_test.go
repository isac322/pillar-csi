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
	"k8s.io/apimachinery/pkg/api/resource"
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
		bctx               context.Context
		reconciler         *PillarPoolReconciler
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

	Context("PoolDiscovered condition — target ready, no discovered pools yet", func() {
		BeforeEach(func() {
			createPool()
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveFinalizer()
			deletePool()
			deleteTarget()
		})

		It("should set PoolDiscovered=Unknown when target is Ready but has no discoveredPools", func() {
			readyStatus := metav1.ConditionTrue
			createTarget(&readyStatus, "all checks pass")
			// Target has no DiscoveredPools in status (agent gRPC not connected).

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "PoolDiscovered")
			Expect(cond).NotTo(BeNil(), "PoolDiscovered condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("WaitingForAgentData"))
			Expect(cond.Message).To(ContainSubstring(targetName))
		})

		It("should set BackendSupported=Unknown when target is Ready but has no capabilities", func() {
			readyStatus := metav1.ConditionTrue
			createTarget(&readyStatus, "all checks pass")
			// Target has no Capabilities in status (agent gRPC not connected).

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "BackendSupported")
			Expect(cond).NotTo(BeNil(), "BackendSupported condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("WaitingForAgentData"))
			Expect(cond.Message).To(ContainSubstring(targetName))
		})

		It("should set Ready=False when PoolDiscovered and BackendSupported are Unknown", func() {
			readyStatus := metav1.ConditionTrue
			createTarget(&readyStatus, "all checks pass")

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "Ready")
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Context("PoolDiscovered condition — target ready with discovered pools", func() {
		const zfsPoolName = "hot-data"

		// Helper: create pool with ZFS backend referencing a named ZFS pool.
		createZFSPool := func() {
			resource := &pillarcsiv1alpha1.PillarPool{}
			err := k8sClient.Get(bctx, poolNamespacedName, resource)
			if err != nil {
				pool := &pillarcsiv1alpha1.PillarPool{
					ObjectMeta: metav1.ObjectMeta{
						Name: poolName,
					},
					Spec: pillarcsiv1alpha1.PillarPoolSpec{
						TargetRef: targetName,
						Backend: pillarcsiv1alpha1.BackendSpec{
							Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
							ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
								Pool: zfsPoolName,
							},
						},
					},
				}
				Expect(k8sClient.Create(bctx, pool)).To(Succeed())
			}
		}

		// Helper: set target status to Ready with given discovered pools and capabilities.
		setTargetReadyWithData := func(pools []pillarcsiv1alpha1.DiscoveredPool, backends []string) {
			target := &pillarcsiv1alpha1.PillarTarget{}
			err := k8sClient.Get(bctx, types.NamespacedName{Name: targetName}, target)
			if err != nil {
				resource := &pillarcsiv1alpha1.PillarTarget{
					ObjectMeta: metav1.ObjectMeta{Name: targetName},
					Spec: pillarcsiv1alpha1.PillarTargetSpec{
						External: &pillarcsiv1alpha1.ExternalSpec{
							Address: "192.0.2.10",
							Port:    9500,
						},
					},
				}
				Expect(k8sClient.Create(bctx, resource)).To(Succeed())
				Expect(k8sClient.Get(bctx, types.NamespacedName{Name: targetName}, target)).To(Succeed())
			}
			var caps *pillarcsiv1alpha1.AgentCapabilities
			if backends != nil {
				caps = &pillarcsiv1alpha1.AgentCapabilities{Backends: backends}
			}
			target.Status.ResolvedAddress = "192.0.2.10:9500"
			target.Status.DiscoveredPools = pools
			target.Status.Capabilities = caps
			target.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "AgentConnected",
					Message:            "agent connected",
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, target)).To(Succeed())
		}

		BeforeEach(func() {
			createZFSPool()
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveFinalizer()
			deletePool()
			deleteTarget()
		})

		It("should set PoolDiscovered=True when pool name is in target's discoveredPools", func() {
			discoveredPools := []pillarcsiv1alpha1.DiscoveredPool{
				{Name: zfsPoolName, Type: "zfs"},
				{Name: "other-pool", Type: "zfs"},
			}
			setTargetReadyWithData(discoveredPools, []string{"zfs-zvol", "zfs-dataset"})

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "PoolDiscovered")
			Expect(cond).NotTo(BeNil(), "PoolDiscovered condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("PoolDiscovered"))
			Expect(cond.Message).To(ContainSubstring(zfsPoolName))
		})

		It("should set PoolDiscovered=False when pool name is NOT in target's discoveredPools", func() {
			discoveredPools := []pillarcsiv1alpha1.DiscoveredPool{
				{Name: "other-pool", Type: "zfs"},
				{Name: "another-pool", Type: "zfs"},
			}
			setTargetReadyWithData(discoveredPools, []string{"zfs-zvol"})

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "PoolDiscovered")
			Expect(cond).NotTo(BeNil(), "PoolDiscovered condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotFound"))
			Expect(cond.Message).To(ContainSubstring(zfsPoolName))
		})

		It("should set BackendSupported=True when backend type is in target capabilities", func() {
			discoveredPools := []pillarcsiv1alpha1.DiscoveredPool{
				{Name: zfsPoolName, Type: "zfs"},
			}
			setTargetReadyWithData(discoveredPools, []string{"zfs-zvol", "zfs-dataset", "lvm-lv"})

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "BackendSupported")
			Expect(cond).NotTo(BeNil(), "BackendSupported condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("BackendSupported"))
			Expect(cond.Message).To(ContainSubstring("zfs-zvol"))
		})

		It("should set BackendSupported=False when backend type is NOT in target capabilities", func() {
			discoveredPools := []pillarcsiv1alpha1.DiscoveredPool{
				{Name: zfsPoolName, Type: "zfs"},
			}
			// Target only supports lvm-lv but the pool uses zfs-zvol.
			setTargetReadyWithData(discoveredPools, []string{"lvm-lv", "dir"})

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			cond := findCondition(fetched, "BackendSupported")
			Expect(cond).NotTo(BeNil(), "BackendSupported condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("BackendNotSupported"))
			Expect(cond.Message).To(ContainSubstring("zfs-zvol"))
		})

		It("should set Ready=True when TargetReady, PoolDiscovered, and BackendSupported are all True", func() {
			discoveredPools := []pillarcsiv1alpha1.DiscoveredPool{
				{Name: zfsPoolName, Type: "zfs"},
			}
			setTargetReadyWithData(discoveredPools, []string{"zfs-zvol", "zfs-dataset"})

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()

			Expect(findCondition(fetched, "TargetReady").Status).To(Equal(metav1.ConditionTrue))
			Expect(findCondition(fetched, "PoolDiscovered").Status).To(Equal(metav1.ConditionTrue))
			Expect(findCondition(fetched, "BackendSupported").Status).To(Equal(metav1.ConditionTrue))

			readyCond := findCondition(fetched, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal("AllConditionsMet"))
		})

		It("should set Ready=False when pool is not discovered even if backend is supported", func() {
			// Pool not found in discovered pools.
			discoveredPools := []pillarcsiv1alpha1.DiscoveredPool{
				{Name: "other-pool", Type: "zfs"},
			}
			setTargetReadyWithData(discoveredPools, []string{"zfs-zvol"})

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()
			readyCond := findCondition(fetched, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("ConditionsNotMet"))
		})
	})

	Context("PoolDiscovered condition — target not ready sets Unknown", func() {
		BeforeEach(func() {
			createPool()
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveFinalizer()
			deletePool()
			deleteTarget()
		})

		It("should set PoolDiscovered=Unknown and BackendSupported=Unknown when target exists but is not Ready", func() {
			notReadyStatus := metav1.ConditionFalse
			createTarget(&notReadyStatus, "agent disconnected")

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchPool()

			pdCond := findCondition(fetched, "PoolDiscovered")
			Expect(pdCond).NotTo(BeNil())
			Expect(pdCond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(pdCond.Reason).To(Equal("TargetNotReady"))

			bsCond := findCondition(fetched, "BackendSupported")
			Expect(bsCond).NotTo(BeNil())
			Expect(bsCond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(bsCond.Reason).To(Equal("TargetNotReady"))
		})
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Capacity sync tests
	// ──────────────────────────────────────────────────────────────────────────

	Context("Capacity sync from DiscoveredPools", func() {
		const (
			capPoolName = "cap-pool"
			capTarget   = "cap-pool-target"
			capZFSPool  = "tank"
		)

		var capPoolNN types.NamespacedName

		// quantityPtr returns a pointer to a parsed resource.Quantity.
		quantityPtr := func(s string) *resource.Quantity {
			q := resource.MustParse(s)
			return &q
		}

		// createCapPool creates a PillarPool with a ZFS backend pointing at capZFSPool.
		createCapPool := func() {
			nn := types.NamespacedName{Name: capPoolName}
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, nn, p); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
					ObjectMeta: metav1.ObjectMeta{Name: capPoolName},
					Spec: pillarcsiv1alpha1.PillarPoolSpec{
						TargetRef: capTarget,
						Backend: pillarcsiv1alpha1.BackendSpec{
							Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
							ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: capZFSPool},
						},
					},
				})).To(Succeed())
			}
		}

		// setCapTarget creates (or updates) the PillarTarget with Ready=True and
		// the supplied discovered pools / backends in status.
		setCapTarget := func(pools []pillarcsiv1alpha1.DiscoveredPool, backends []string) {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: capTarget}, t); err != nil {
				Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarTarget{
					ObjectMeta: metav1.ObjectMeta{Name: capTarget},
					Spec: pillarcsiv1alpha1.PillarTargetSpec{
						External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.20", Port: 9500},
					},
				})).To(Succeed())
				Expect(k8sClient.Get(bctx, types.NamespacedName{Name: capTarget}, t)).To(Succeed())
			}
			var caps *pillarcsiv1alpha1.AgentCapabilities
			if backends != nil {
				caps = &pillarcsiv1alpha1.AgentCapabilities{Backends: backends}
			}
			t.Status.ResolvedAddress = "192.0.2.20:9500"
			t.Status.DiscoveredPools = pools
			t.Status.Capabilities = caps
			t.Status.Conditions = []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "AgentConnected",
				Message:            "agent connected",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(bctx, t)).To(Succeed())
		}

		doCapReconcile := func() (reconcile.Result, error) {
			r := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			return r.Reconcile(bctx, reconcile.Request{NamespacedName: capPoolNN})
		}

		fetchCapPool := func() *pillarcsiv1alpha1.PillarPool {
			p := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, capPoolNN, p)).To(Succeed())
			return p
		}

		BeforeEach(func() {
			capPoolNN = types.NamespacedName{Name: capPoolName}
			createCapPool()
			// First reconcile adds the finalizer.
			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			// Force-remove finalizer so object can be GC'd.
			p := &pillarcsiv1alpha1.PillarPool{}
			if err := k8sClient.Get(bctx, capPoolNN, p); err == nil {
				controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
				_ = k8sClient.Update(bctx, p)
				_ = k8sClient.Delete(bctx, p)
			}
			// Remove target.
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: capTarget}, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				_ = k8sClient.Update(bctx, t)
				_ = k8sClient.Delete(bctx, t)
			}
		})

		It("should populate Total, Available, and computed Used when DiscoveredPool has capacity", func() {
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{
					Name:      capZFSPool,
					Type:      "zfs",
					Total:     quantityPtr("100Gi"),
					Available: quantityPtr("75Gi"),
				},
			}, []string{"zfs-zvol", "zfs-dataset"})

			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchCapPool()
			Expect(fetched.Status.Capacity).NotTo(BeNil(), "Capacity should be populated")
			Expect(fetched.Status.Capacity.Total).NotTo(BeNil(), "Total should be set")
			Expect(fetched.Status.Capacity.Available).NotTo(BeNil(), "Available should be set")
			Expect(fetched.Status.Capacity.Used).NotTo(BeNil(), "Used should be computed")

			// Verify values: 100Gi total, 75Gi available, 25Gi used.
			expectedTotal := resource.MustParse("100Gi")
			expectedAvail := resource.MustParse("75Gi")
			expectedUsed := resource.MustParse("25Gi")
			Expect(fetched.Status.Capacity.Total.Cmp(expectedTotal)).To(Equal(0),
				"Total should equal 100Gi")
			Expect(fetched.Status.Capacity.Available.Cmp(expectedAvail)).To(Equal(0),
				"Available should equal 75Gi")
			Expect(fetched.Status.Capacity.Used.Cmp(expectedUsed)).To(Equal(0),
				"Used should equal Total - Available = 25Gi")
		})

		It("should set Used=0 when Available exceeds Total (corrupted agent data)", func() {
			// Available > Total — guard against negative Used.
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{
					Name:      capZFSPool,
					Type:      "zfs",
					Total:     quantityPtr("10Gi"),
					Available: quantityPtr("20Gi"), // exceeds total
				},
			}, []string{"zfs-zvol"})

			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchCapPool()
			Expect(fetched.Status.Capacity).NotTo(BeNil())
			Expect(fetched.Status.Capacity.Used).NotTo(BeNil())
			zero := resource.MustParse("0")
			Expect(fetched.Status.Capacity.Used.Cmp(zero)).To(Equal(0),
				"Used should be clamped to 0 when Available > Total")
		})

		It("should leave capacity nil when DiscoveredPool has no Total or Available", func() {
			// Pool exists in DiscoveredPools but carries no capacity data.
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{Name: capZFSPool, Type: "zfs"}, // no Total/Available
			}, []string{"zfs-zvol"})

			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchCapPool()
			Expect(fetched.Status.Capacity).To(BeNil(),
				"Capacity should remain nil when the discovered pool carries no capacity data")
		})

		It("should clear capacity when pool is no longer discovered", func() {
			// First reconcile — pool discovered with capacity.
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{
					Name:      capZFSPool,
					Type:      "zfs",
					Total:     quantityPtr("50Gi"),
					Available: quantityPtr("40Gi"),
				},
			}, []string{"zfs-zvol"})
			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(fetchCapPool().Status.Capacity).NotTo(BeNil(), "Capacity should be set after first sync")

			// Second reconcile — pool name no longer in discovered pools.
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{Name: "other-pool", Type: "zfs", Total: quantityPtr("10Gi"), Available: quantityPtr("10Gi")},
			}, []string{"zfs-zvol"})
			_, err = doCapReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchCapPool()
			Expect(fetched.Status.Capacity).To(BeNil(),
				"Stale capacity should be cleared when pool is not discovered")
		})

		It("should clear capacity when pool transitions from discovered to not-discovered", func() {
			// Start with capacity synced.
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{Name: capZFSPool, Type: "zfs", Total: quantityPtr("200Gi"), Available: quantityPtr("150Gi")},
			}, []string{"zfs-zvol"})
			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(fetchCapPool().Status.Capacity).NotTo(BeNil())

			// Target loses the pool (agent reported no pools).
			t := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: capTarget}, t)).To(Succeed())
			t.Status.DiscoveredPools = []pillarcsiv1alpha1.DiscoveredPool{}
			Expect(k8sClient.Status().Update(bctx, t)).To(Succeed())

			_, err = doCapReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchCapPool()
			Expect(fetched.Status.Capacity).To(BeNil(),
				"Capacity should be cleared when DiscoveredPools becomes empty")
		})

		It("should set Total without Used when only Total is reported", func() {
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{
					Name:  capZFSPool,
					Type:  "zfs",
					Total: quantityPtr("500Gi"),
					// Available intentionally absent.
				},
			}, []string{"zfs-zvol"})

			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchCapPool()
			Expect(fetched.Status.Capacity).NotTo(BeNil())
			Expect(fetched.Status.Capacity.Total).NotTo(BeNil())
			Expect(fetched.Status.Capacity.Available).To(BeNil(),
				"Available should be nil when not reported by agent")
			Expect(fetched.Status.Capacity.Used).To(BeNil(),
				"Used should not be computed when Available is missing")
		})

		It("should sync capacity for non-ZFS backends using the first DiscoveredPool entry", func() {
			// Create a dir-backend pool (no named pool).
			dirPoolName := "dir-cap-pool"
			dirNN := types.NamespacedName{Name: dirPoolName}
			Expect(k8sClient.Create(bctx, &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: dirPoolName},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: capTarget,
					Backend:   pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeDir},
				},
			})).To(Succeed())
			defer func() {
				p := &pillarcsiv1alpha1.PillarPool{}
				if err := k8sClient.Get(bctx, dirNN, p); err == nil {
					controllerutil.RemoveFinalizer(p, pillarPoolFinalizer)
					_ = k8sClient.Update(bctx, p)
					_ = k8sClient.Delete(bctx, p)
				}
			}()

			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{
					Name:      "host-dir",
					Type:      "dir",
					Total:     quantityPtr("1Ti"),
					Available: quantityPtr("800Gi"),
				},
			}, []string{"dir"})

			dirReconciler := &PillarPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile — adds finalizer.
			_, err := dirReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: dirNN})
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile — normal path.
			_, err = dirReconciler.Reconcile(bctx, reconcile.Request{NamespacedName: dirNN})
			Expect(err).NotTo(HaveOccurred())

			dirPool := &pillarcsiv1alpha1.PillarPool{}
			Expect(k8sClient.Get(bctx, dirNN, dirPool)).To(Succeed())
			Expect(dirPool.Status.Capacity).NotTo(BeNil(),
				"dir-backend pool should pick up capacity from the first DiscoveredPool entry")
			expectedTotal := resource.MustParse("1Ti")
			Expect(dirPool.Status.Capacity.Total.Cmp(expectedTotal)).To(Equal(0))
		})

		It("should set Ready=True with synced capacity when all conditions pass", func() {
			setCapTarget([]pillarcsiv1alpha1.DiscoveredPool{
				{Name: capZFSPool, Type: "zfs", Total: quantityPtr("100Gi"), Available: quantityPtr("60Gi")},
			}, []string{"zfs-zvol"})

			_, err := doCapReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchCapPool()
			readyCond := meta.FindStatusCondition(fetched.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue),
				"Ready should be True when all sub-conditions pass and capacity is synced")
			Expect(fetched.Status.Capacity).NotTo(BeNil())
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
