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

// E23 — PillarProtocol CRD lifecycle: webhook validation tests.
//
// These tests validate the PillarProtocolCustomValidator webhook logic directly
// (no API server round-trip required for the validator call itself).  The
// integration build tag is kept for consistency with the suite setup file.

var _ = Describe("PillarProtocol Webhook", func() {
	var (
		obj       *pillarcsiv1alpha1.PillarProtocol
		oldObj    *pillarcsiv1alpha1.PillarProtocol
		validator PillarProtocolCustomValidator
	)

	BeforeEach(func() {
		obj = &pillarcsiv1alpha1.PillarProtocol{}
		oldObj = &pillarcsiv1alpha1.PillarProtocol{}
		validator = PillarProtocolCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
		// no teardown required for direct validator tests
	})

	// ── E23.1: Valid spec creation ────────────────────────────────────────────

	Context("When creating PillarProtocol under Validating Webhook", func() {

		// E23.1.1 — TestPillarProtocolWebhook_ValidCreate_NVMeOFTCP
		It("Should allow creation with spec.type=nvmeof-tcp (ValidateCreate is a pass-through)", func() {
			By("setting spec.type to nvmeof-tcp with port 4420")
			obj.Name = "pp-nvmeof-tcp"
			obj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
				NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
					Port: 4420,
				},
			}

			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(),
				"ValidateCreate should return no error for valid nvmeof-tcp spec")
			Expect(warnings).To(BeNil(),
				"ValidateCreate should return no warnings for valid nvmeof-tcp spec")
		})

		// E23.1.2 — TestPillarProtocolWebhook_ValidCreate_ISCSI
		It("Should allow creation with spec.type=iscsi", func() {
			By("setting spec.type to iscsi with port 3260")
			obj.Name = "pp-iscsi"
			obj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeISCSI,
				ISCSI: &pillarcsiv1alpha1.ISCSIConfig{
					Port: 3260,
				},
			}

			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(),
				"ValidateCreate should return no error for valid iscsi spec")
			Expect(warnings).To(BeNil(),
				"ValidateCreate should return no warnings for valid iscsi spec")
		})

		// E23.1.3 — TestPillarProtocolWebhook_ValidCreate_NFS
		It("Should allow creation with spec.type=nfs", func() {
			By("setting spec.type to nfs with version 4.2")
			obj.Name = "pp-nfs"
			obj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNFS,
				NFS: &pillarcsiv1alpha1.NFSConfig{
					Version: "4.2",
				},
			}

			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(),
				"ValidateCreate should return no error for valid nfs spec")
			Expect(warnings).To(BeNil(),
				"ValidateCreate should return no warnings for valid nfs spec")
		})
	})

	// ── E23.3: Immutable field update rejection ───────────────────────────────

	Context("When updating PillarProtocol under Validating Webhook", func() {

		// E23.3.1 — TestPillarProtocolWebhook_ImmutableUpdate_TypeChange_NVMeToISCSI
		It("Should deny update when spec.type changes from nvmeof-tcp to iscsi", func() {
			By("setting oldObj.spec.type=nvmeof-tcp and newObj.spec.type=iscsi")
			oldObj.Name = "pp-immutable-nvme"
			oldObj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
				NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
					Port: 4420,
				},
			}
			obj.Name = "pp-immutable-nvme"
			obj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeISCSI,
				ISCSI: &pillarcsiv1alpha1.ISCSIConfig{
					Port: 3260,
				},
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"ValidateUpdate should reject type change from nvmeof-tcp to iscsi")
			Expect(err.Error()).To(ContainSubstring("immutable"),
				"Error message should mention that the field is immutable")
			Expect(err.Error()).To(ContainSubstring(string(pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP)),
				"Error message should include the old type value")
			Expect(err.Error()).To(ContainSubstring(string(pillarcsiv1alpha1.ProtocolTypeISCSI)),
				"Error message should include the new type value")
		})

		// E23.3.2 — TestPillarProtocolWebhook_ImmutableUpdate_TypeChange_ISCSIToNFS
		It("Should deny update when spec.type changes from iscsi to nfs", func() {
			By("setting oldObj.spec.type=iscsi and newObj.spec.type=nfs")
			oldObj.Name = "pp-immutable-iscsi"
			oldObj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeISCSI,
				ISCSI: &pillarcsiv1alpha1.ISCSIConfig{
					Port: 3260,
				},
			}
			obj.Name = "pp-immutable-iscsi"
			obj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNFS,
				NFS:  &pillarcsiv1alpha1.NFSConfig{Version: "4.2"},
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"ValidateUpdate should reject type change from iscsi to nfs")
			Expect(err.Error()).To(ContainSubstring("immutable"),
				"Error message should mention that the field is immutable")
			Expect(err.Error()).To(ContainSubstring(string(pillarcsiv1alpha1.ProtocolTypeISCSI)),
				"Error message should include the old type value")
			Expect(err.Error()).To(ContainSubstring(string(pillarcsiv1alpha1.ProtocolTypeNFS)),
				"Error message should include the new type value")
		})

		// E23.3.3 — TestPillarProtocolWebhook_MutableUpdate_PortChange
		It("Should allow update when only spec.nvmeofTcp.port changes (type is unchanged)", func() {
			By("keeping spec.type as nvmeof-tcp but changing port from 4420 to 4421")
			oldObj.Name = "pp-mutable-port"
			oldObj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
				NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
					Port: 4420,
				},
			}
			obj.Name = "pp-mutable-port"
			obj.Spec = pillarcsiv1alpha1.PillarProtocolSpec{
				Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
				NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
					Port: 4421,
				},
			}

			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"Changing only spec.nvmeofTcp.port should be allowed (spec.type is unchanged)")
			Expect(warnings).To(BeNil(),
				"No warnings expected for a mutable field update")
		})
	})
})
