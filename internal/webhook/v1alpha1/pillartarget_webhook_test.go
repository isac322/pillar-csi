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

// E21.2: PillarTarget webhook — immutable field update rejection tests.
//
// These tests validate that PillarTargetCustomValidator.ValidateUpdate() correctly
// rejects mutations to identity-forming fields (spec.nodeRef / spec.external) that
// would silently redirect the target to a different physical storage host.
//
// All tests call the validator directly — no envtest API server is required for
// compilation or execution of the validator logic; the integration build tag is kept
// for consistency with the suite setup file.

var _ = Describe("PillarTarget Webhook", func() {
	var (
		obj       *pillarcsiv1alpha1.PillarTarget
		oldObj    *pillarcsiv1alpha1.PillarTarget
		validator PillarTargetCustomValidator
	)

	BeforeEach(func() {
		obj = &pillarcsiv1alpha1.PillarTarget{}
		oldObj = &pillarcsiv1alpha1.PillarTarget{}
		validator = PillarTargetCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
		// no teardown required for direct validator tests
	})

	Context("When creating or updating PillarTarget under Validating Webhook", func() {

		// ── E21.2 — ID 151 ──────────────────────────────────────────────────────
		// TestPillarTargetWebhook_Update_DiscriminantSwitch_NodeToExternal
		It("Should deny update when switching discriminant from nodeRef to external", func() {
			By("setting up oldObj with spec.nodeRef and newObj with spec.external")
			oldObj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "node1"}
			obj.Spec.NodeRef = nil
			obj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "1.2.3.4", Port: 9500}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when switching from nodeRef to external")
			Expect(err.Error()).To(ContainSubstring("cannot switch between nodeRef and external"),
				"Error message should mention discriminant switch")
		})

		// ── E21.2 — ID 152 ──────────────────────────────────────────────────────
		// TestPillarTargetWebhook_Update_DiscriminantSwitch_ExternalToNode
		It("Should deny update when switching discriminant from external to nodeRef", func() {
			By("setting up oldObj with spec.external and newObj with spec.nodeRef")
			oldObj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "1.2.3.4", Port: 9500}
			oldObj.Spec.NodeRef = nil
			obj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "node1"}
			obj.Spec.External = nil

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when switching from external to nodeRef")
			Expect(err.Error()).To(ContainSubstring("cannot switch between nodeRef and external"),
				"Error message should mention discriminant switch")
		})

		// ── E21.2 — ID 153 ──────────────────────────────────────────────────────
		// TestPillarTargetWebhook_Update_NodeRefNameImmutable
		It("Should deny update when spec.nodeRef.name is changed", func() {
			By("setting oldObj.spec.nodeRef.name to node-a and newObj.spec.nodeRef.name to node-b")
			oldObj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "node-a"}
			obj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "node-b"}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when nodeRef.name is changed")
			Expect(err.Error()).To(ContainSubstring("node-a"),
				"Error should mention old nodeRef.name value")
			Expect(err.Error()).To(ContainSubstring("node-b"),
				"Error should mention new nodeRef.name value")
		})

		// ── E21.2 — ID 154 ──────────────────────────────────────────────────────
		// TestPillarTargetWebhook_Update_ExternalAddressImmutable
		It("Should deny update when spec.external.address is changed", func() {
			By("setting oldObj.spec.external.address to 1.2.3.4 and newObj to 5.6.7.8")
			oldObj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "1.2.3.4", Port: 9500}
			obj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "5.6.7.8", Port: 9500}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when external.address is changed")
			Expect(err.Error()).To(ContainSubstring("1.2.3.4"),
				"Error should mention old external.address value")
			Expect(err.Error()).To(ContainSubstring("5.6.7.8"),
				"Error should mention new external.address value")
		})

		// ── E21.2 — ID 155 ──────────────────────────────────────────────────────
		// TestPillarTargetWebhook_Update_ExternalPortImmutable
		It("Should deny update when spec.external.port is changed", func() {
			By("setting oldObj.spec.external.port to 9500 and newObj to 9600")
			oldObj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "1.2.3.4", Port: 9500}
			obj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "1.2.3.4", Port: 9600}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when external.port is changed")
			Expect(err.Error()).To(ContainSubstring("9500"),
				"Error should mention old external.port value")
			Expect(err.Error()).To(ContainSubstring("9600"),
				"Error should mention new external.port value")
		})

		// ── E21.2 — ID 156 ──────────────────────────────────────────────────────
		// TestPillarTargetWebhook_Update_NodeRefNonIdentityFieldChange_OK
		It("Should allow update when only non-identity nodeRef fields change (addressType)", func() {
			By("changing only addressType while keeping nodeRef.name the same")
			oldObj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{
				Name:        "node-a",
				AddressType: "InternalIP",
			}
			obj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{
				Name:        "node-a",
				AddressType: "ExternalIP",
			}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"Non-identity field changes (addressType) should be allowed")
		})

		// ── E21.2 — ID 157 ──────────────────────────────────────────────────────
		// TestPillarTargetWebhook_Create_Valid
		It("Should allow valid PillarTarget creation (current ValidateCreate is no-op scaffolding)", func() {
			By("creating a PillarTarget with spec.nodeRef set to a valid node name")
			obj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "storage-node-1"}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(),
				"Valid PillarTarget creation should be allowed")
		})
	})
})
