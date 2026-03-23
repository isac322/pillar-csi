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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("PillarBinding Webhook", func() {
	var (
		obj       *pillarcsiv1alpha1.PillarBinding
		oldObj    *pillarcsiv1alpha1.PillarBinding
		validator PillarBindingCustomValidator
		defaulter PillarBindingCustomDefaulter
	)

	BeforeEach(func() {
		obj = &pillarcsiv1alpha1.PillarBinding{}
		oldObj = &pillarcsiv1alpha1.PillarBinding{}
		validator = PillarBindingCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		defaulter = PillarBindingCustomDefaulter{Client: k8sClient}
		Expect(defaulter).NotTo(BeNil(), "Expected defaulter to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
		// TODO (user): Add any teardown logic common to all tests
	})

	Context("When creating PillarBinding under Defaulting Webhook", func() {
		It("Should set allowVolumeExpansion=true when pool backend is zfs-zvol", func() {
			By("creating a PillarPool with zfs-zvol backend")
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pool-zvol"},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "test-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
						ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
							Pool: "tank",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })

			By("creating a PillarBinding referencing the pool, without allowVolumeExpansion set")
			obj.Name = "test-binding-zvol"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "test-pool-zvol",
				ProtocolRef: "test-protocol",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			By("verifying allowVolumeExpansion is set to true")
			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeTrue())
		})

		It("Should set allowVolumeExpansion=true when pool backend is lvm-lv", func() {
			By("creating a PillarPool with lvm-lv backend")
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pool-lvm"},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "test-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeLVMLV,
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })

			By("calling Default on a binding without allowVolumeExpansion")
			obj.Name = "test-binding-lvm"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "test-pool-lvm",
				ProtocolRef: "test-protocol",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			By("verifying allowVolumeExpansion is set to true")
			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeTrue())
		})

		It("Should set allowVolumeExpansion=false when pool backend is zfs-dataset", func() {
			By("creating a PillarPool with zfs-dataset backend")
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pool-zfs-ds"},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "test-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSDataset,
						ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
							Pool: "tank",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })

			By("calling Default on a binding without allowVolumeExpansion")
			obj.Name = "test-binding-zfs-ds"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "test-pool-zfs-ds",
				ProtocolRef: "test-protocol",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			By("verifying allowVolumeExpansion is set to false")
			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeFalse())
		})

		It("Should set allowVolumeExpansion=false when pool backend is dir", func() {
			By("creating a PillarPool with dir backend")
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pool-dir"},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "test-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeDir,
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })

			By("calling Default on a binding without allowVolumeExpansion")
			obj.Name = "test-binding-dir"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "test-pool-dir",
				ProtocolRef: "test-protocol",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			By("verifying allowVolumeExpansion is set to false")
			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeFalse())
		})

		It("Should not override allowVolumeExpansion when already explicitly set", func() {
			By("creating a PillarPool with zfs-zvol backend (which would default to true)")
			pool := &pillarcsiv1alpha1.PillarPool{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pool-override"},
				Spec: pillarcsiv1alpha1.PillarPoolSpec{
					TargetRef: "test-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
						ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
							Pool: "tank",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })

			By("setting allowVolumeExpansion to false explicitly (opposite of backend default)")
			falseVal := false
			obj.Name = "test-binding-override"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "test-pool-override",
				ProtocolRef: "test-protocol",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{
					AllowVolumeExpansion: &falseVal,
				},
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			By("verifying the explicit value is preserved")
			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeFalse())
		})

		It("Should leave allowVolumeExpansion unset when pool does not exist", func() {
			By("calling Default with a poolRef that does not exist in the cluster")
			obj.Name = "test-binding-nopool"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "nonexistent-pool",
				ProtocolRef: "test-protocol",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			By("verifying allowVolumeExpansion is still nil (not set)")
			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).To(BeNil())
		})
	})

	Context("When creating or updating PillarBinding under Validating Webhook", func() {
		It("Should admit creation with all required fields present", func() {
			By("simulating a valid creation")
			obj.Name = "test-binding-valid"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "some-pool",
				ProtocolRef: "some-protocol",
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny update when poolRef is changed", func() {
			By("simulating an update that changes poolRef")
			oldObj.Name = "test-binding-immutable"
			oldObj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "pool-a",
				ProtocolRef: "proto-a",
			}
			obj.Name = "test-binding-immutable"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "pool-b", // changed
				ProtocolRef: "proto-a",
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("poolRef"))
		})

		It("Should deny update when protocolRef is changed", func() {
			By("simulating an update that changes protocolRef")
			oldObj.Name = "test-binding-immutable2"
			oldObj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "pool-a",
				ProtocolRef: "proto-a",
			}
			obj.Name = "test-binding-immutable2"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "pool-a",
				ProtocolRef: "proto-b", // changed
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("protocolRef"))
		})

		It("Should admit update when only non-immutable fields are changed", func() {
			By("simulating a valid update changing only storageClass settings")
			oldObj.Name = "test-binding-mutable"
			oldObj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "pool-a",
				ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{
					ReclaimPolicy: pillarcsiv1alpha1.ReclaimPolicyDelete,
				},
			}
			obj.Name = "test-binding-mutable"
			obj.Spec = pillarcsiv1alpha1.PillarBindingSpec{
				PoolRef:     "pool-a",
				ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{
					ReclaimPolicy: pillarcsiv1alpha1.ReclaimPolicyRetain, // allowed to change
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
