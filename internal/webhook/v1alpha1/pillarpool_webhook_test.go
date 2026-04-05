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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// E21.3: PillarPool webhook — immutable field update rejection tests.
//
// These tests validate that PillarPoolCustomValidator.ValidateUpdate() correctly
// rejects mutations to spec.targetRef and spec.backend.type, which are immutable
// because changing them would invalidate all volumes provisioned from the pool.
//
// All tests call the validator directly — no envtest API server is required for
// compilation or execution of the validator logic; the integration build tag is kept
// for consistency with the suite setup file.

var _ = Describe("PillarPool Webhook", func() {
	var (
		obj       *pillarcsiv1alpha1.PillarPool
		oldObj    *pillarcsiv1alpha1.PillarPool
		validator PillarPoolCustomValidator
	)

	BeforeEach(func() {
		obj = &pillarcsiv1alpha1.PillarPool{}
		oldObj = &pillarcsiv1alpha1.PillarPool{}
		validator = PillarPoolCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
		// no teardown required for direct validator tests
	})

	Context("When creating or updating PillarPool under Validating Webhook", func() {

		// ── E21.3 — ID 158 ──────────────────────────────────────────────────────
		// TestPillarPoolWebhook_Update_TargetRefImmutable
		It("[TC-E21.158] 158 TestPillarPoolWebhook_Update_TargetRefImmutable: Should deny update when spec.targetRef is changed", func() {
			By("setting oldObj.spec.targetRef to target-a and newObj.spec.targetRef to target-b")
			oldObj.Spec.TargetRef = "target-a"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
			}
			obj.Spec.TargetRef = "target-b"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when spec.targetRef is changed")
			Expect(err.Error()).To(ContainSubstring("target-a"),
				"Error should mention old targetRef value")
			Expect(err.Error()).To(ContainSubstring("target-b"),
				"Error should mention new targetRef value")
		})

		// ── E21.3 — ID 159 ──────────────────────────────────────────────────────
		// TestPillarPoolWebhook_Update_BackendTypeImmutable
		It("[TC-E21.159] 159 TestPillarPoolWebhook_Update_BackendTypeImmutable: Should deny update when spec.backend.type is changed", func() {
			By("changing backend.type from zfs-zvol to lvm-lv")
			oldObj.Spec.TargetRef = "t1"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
			}
			obj.Spec.TargetRef = "t1"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeLVMLV,
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when spec.backend.type is changed")
			Expect(err.Error()).To(ContainSubstring("zfs-zvol"),
				"Error should mention old backend.type value")
			Expect(err.Error()).To(ContainSubstring("lvm-lv"),
				"Error should mention new backend.type value")
		})

		// ── E21.3 — ID 160 ──────────────────────────────────────────────────────
		// TestPillarPoolWebhook_Update_ZFSPoolChange_OK
		It("[TC-E21.160] 160 TestPillarPoolWebhook_Update_ZFSPoolChange_OK: Should allow update when only the ZFS pool name changes (backend.type unchanged)", func() {
			By("keeping backend.type as zfs-zvol but changing zfs.pool from tank to new-tank")
			oldObj.Spec.TargetRef = "t1"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
			}
			obj.Spec.TargetRef = "t1"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "new-tank"},
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"Changing only the ZFS pool name should be allowed")
		})

		// ── E21.3 — ID 161 ──────────────────────────────────────────────────────
		// TestPillarPoolWebhook_Update_BothFieldsChanged_MultipleErrors
		It("[TC-E21.161] 161 TestPillarPoolWebhook_Update_BothFieldsChanged_MultipleErrors: Should return errors for both spec.targetRef and spec.backend.type when both change", func() {
			By("changing both targetRef and backend.type simultaneously")
			oldObj.Spec.TargetRef = "t1"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
			}
			obj.Spec.TargetRef = "t2"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeLVMLV,
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when both immutable fields are changed")
			Expect(err.Error()).To(ContainSubstring("spec.targetRef"),
				"Error should mention spec.targetRef field")
			Expect(err.Error()).To(ContainSubstring("spec.backend.type"),
				"Error should mention spec.backend.type field")
		})

		// ── E21.3 — ID 162 ──────────────────────────────────────────────────────
		// TestPillarPoolWebhook_Create_Valid
		It("[TC-E21.162] 162 TestPillarPoolWebhook_Create_Valid: Should allow valid PillarPool creation (current ValidateCreate is no-op scaffolding)", func() {
			By("creating a PillarPool with valid spec.targetRef and spec.backend")
			obj.Spec.TargetRef = "target-1"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(),
				"Valid PillarPool creation should be allowed")
		})

		// ── E32.1 TC-280 ─────────────────────────────────────────────────────
		// TestPillarPool_LVM_MissingLVMConfig_Rejected
		// When backend.type == "lvm-lv" but backend.lvm is nil the cross-field
		// constraint validated by validatePillarPoolSpec must return an error.
		It("TC-280: TestPillarPool_LVM_MissingLVMConfig_Rejected — lvm-lv without backend.lvm is rejected by ValidateCreate", func() {
			By("calling ValidateCreate with a PillarPool that has type=lvm-lv and no backend.lvm")
			obj.Spec.TargetRef = "storage-1"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeLVMLV,
				// LVM field intentionally omitted — must be rejected.
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred(),
				"ValidateCreate should reject a PillarPool with type=lvm-lv and no backend.lvm section")
			Expect(err.Error()).To(ContainSubstring("lvm"),
				"error message should mention the missing lvm section")
		})

		// ── E20.1.1 ──────────────────────────────────────────────────────────
		// TestPillarPoolWebhook_ValidCreate_ZFSZvol
		// Create PillarPool with ZFS zvol backend → accepted.
		It("[TC-E20.1.1] E20.1.1 TestPillarPoolWebhook_ValidCreate_ZFSZvol: should accept valid PillarPool with ZFS zvol backend", func() {
			By("creating a PillarPool with backend.type=zfs-zvol and a valid zfs.pool name")
			obj.Spec.TargetRef = "storage-target-1"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(),
				"ValidateCreate should accept a PillarPool with a valid ZFS zvol backend")
		})

		// ── E20.1.2 ──────────────────────────────────────────────────────────
		// TestPillarPoolWebhook_ValidCreate_Dir
		It("Should allow valid PillarPool creation with dir backend type (no ZFS config needed)", func() {
			By("creating a PillarPool with backend.type=dir and no ZFS configuration")
			obj.Spec.TargetRef = "target-a"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeDir,
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(),
				"dir backend PillarPool creation should be allowed; no ZFS config is required")
		})

		// ── E20.3.1 ──────────────────────────────────────────────────────────
		// TestPillarPoolWebhook_ImmutableUpdate_TargetRefChange
		// Changing spec.targetRef is rejected.
		It("[TC-E20.3.1] E20.3.1 TestPillarPoolWebhook_ImmutableUpdate_TargetRefChange: changing spec.targetRef is rejected", func() {
			By("setting oldObj.spec.targetRef='old-target' and newObj.spec.targetRef='new-target'")
			oldObj.Spec.TargetRef = "old-target"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol}
			obj.Spec.TargetRef = "new-target"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"Changing spec.targetRef should be rejected as it is an immutable field")
			Expect(err.Error()).To(ContainSubstring("spec.targetRef"))
		})

		// ── E20.3.2 ──────────────────────────────────────────────────────────
		// TestPillarPoolWebhook_ImmutableUpdate_BackendTypeChange
		// Changing spec.backendType is rejected.
		It("[TC-E20.3.2] E20.3.2 TestPillarPoolWebhook_ImmutableUpdate_BackendTypeChange: changing spec.backendType is rejected", func() {
			By("changing backend.type from zfs-zvol to dir")
			oldObj.Spec.TargetRef = "same-target"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol}
			obj.Spec.TargetRef = "same-target"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeDir}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"Changing spec.backend.type should be rejected as it is an immutable field")
			Expect(err.Error()).To(ContainSubstring("spec.backend.type"))
		})

		// ── E20.3.3 ──────────────────────────────────────────────────────────
		// TestPillarPoolWebhook_ImmutableUpdate_BothFieldsChange
		// Changing both spec.targetRef and spec.backendType is rejected.
		It("[TC-E20.3.3] E20.3.3 TestPillarPoolWebhook_ImmutableUpdate_BothFieldsChange: changing both targetRef and backendType is rejected", func() {
			By("changing both spec.targetRef and spec.backend.type simultaneously")
			oldObj.Spec.TargetRef = "target-a"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeZFSZvol}
			obj.Spec.TargetRef = "target-b"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeDir}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"Changing both immutable fields should produce errors for each")
			Expect(err.Error()).To(ContainSubstring("spec.targetRef"),
				"error should mention spec.targetRef")
			Expect(err.Error()).To(ContainSubstring("spec.backend.type"),
				"error should mention spec.backend.type")
		})

		// ── E20.3.4 ──────────────────────────────────────────────────────────
		// TestPillarPoolWebhook_MutableUpdate_ZFSPropertiesChange
		It("Should allow update when only spec.backend.zfs.properties change (immutable fields unchanged)", func() {
			By("keeping targetRef and backend.type identical; changing only zfs.properties")
			oldObj.Spec.TargetRef = "t1"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
					Pool:       "hot-data",
					Properties: map[string]string{"compression": "off"},
				},
			}
			obj.Spec.TargetRef = "t1"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
					Pool:       "hot-data",
					Properties: map[string]string{"compression": "lz4"},
				},
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"Changing only zfs.properties should be allowed; it is not an immutable field")
		})
	})
})
