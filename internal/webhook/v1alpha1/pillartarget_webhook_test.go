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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

		// ── E19.1.1 ───────────────────────────────────────────────────────────
		// TestPillarTargetWebhook_ValidCreate_External
		// ValidateCreate should pass without error for a well-formed external spec.
		It("E19.1.1 TestPillarTargetWebhook_ValidCreate_External: should accept external spec with address+port", func() {
			By("setting spec.external.address='10.0.0.1' and spec.external.port=9500")
			obj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "10.0.0.1", Port: 9500}

			warnings, err := validator.ValidateCreate(ctx, obj)

			Expect(err).NotTo(HaveOccurred(),
				"ValidateCreate should succeed for a valid external spec")
			Expect(warnings).To(BeEmpty())
		})

		// ── E19.1.2 ───────────────────────────────────────────────────────────
		// TestPillarTargetWebhook_ValidCreate_NodeRef
		// ValidateCreate should pass without error for a well-formed nodeRef spec.
		// (This overlaps with ID 157 above; kept explicitly for E19 traceability.)
		It("E19.1.2 TestPillarTargetWebhook_ValidCreate_NodeRef: should accept nodeRef spec with node name set", func() {
			By("setting spec.nodeRef.name='worker-1'")
			obj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "worker-1"}

			warnings, err := validator.ValidateCreate(ctx, obj)

			Expect(err).NotTo(HaveOccurred(),
				"ValidateCreate should succeed for a valid nodeRef spec")
			Expect(warnings).To(BeEmpty())
		})

		// ── E19.3.2 ───────────────────────────────────────────────────────────
		// TestPillarTargetWebhook_ImmutableUpdate_ExternalToNodeRef
		// Changing spec from external to nodeRef should be rejected (spec type change is immutable).
		It("E19.3.2 TestPillarTargetWebhook_ImmutableUpdate_ExternalToNodeRef: changing spec from external to nodeRef should be rejected", func() {
			By("setting oldObj with spec.external and newObj with spec.nodeRef")
			oldObj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "10.0.0.1", Port: 9500}
			oldObj.Spec.NodeRef = nil
			obj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "node-1"}
			obj.Spec.External = nil

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"Switching from external to nodeRef spec type should be rejected as immutable")
			Expect(err.Error()).To(ContainSubstring("cannot switch between nodeRef and external"))
		})

		// ── E19.3.3 ───────────────────────────────────────────────────────────
		// TestPillarTargetWebhook_ImmutableUpdate_NodeRefNameChange
		// Changing spec.nodeRef.name should be rejected.
		It("E19.3.3 TestPillarTargetWebhook_ImmutableUpdate_NodeRefNameChange: changing spec.nodeRef.name should be rejected", func() {
			By("setting oldObj.spec.nodeRef.name='node-x' and newObj.spec.nodeRef.name='node-y'")
			oldObj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "node-x"}
			obj.Spec.NodeRef = &pillarcsiv1alpha1.NodeRefSpec{Name: "node-y"}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"Changing spec.nodeRef.name should be rejected as it is an immutable identity field")
			Expect(err.Error()).To(ContainSubstring("node-x"))
			Expect(err.Error()).To(ContainSubstring("node-y"))
		})

		// ── E19.3.4 ───────────────────────────────────────────────────────────
		// TestPillarTargetWebhook_ImmutableUpdate_ExternalAddressChange
		// Changing spec.external.address should be rejected.
		It("E19.3.4 TestPillarTargetWebhook_ImmutableUpdate_ExternalAddressChange: changing spec.external.address should be rejected", func() {
			By("setting oldObj.spec.external.address='10.1.1.1' and newObj to '10.2.2.2'")
			oldObj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "10.1.1.1", Port: 9500}
			obj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "10.2.2.2", Port: 9500}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"Changing spec.external.address should be rejected as it is an immutable identity field")
			Expect(err.Error()).To(ContainSubstring("10.1.1.1"))
			Expect(err.Error()).To(ContainSubstring("10.2.2.2"))
		})

		// ── E19.3.5 ───────────────────────────────────────────────────────────
		// TestPillarTargetWebhook_ImmutableUpdate_ExternalPortChange
		// Changing spec.external.port should be rejected.
		It("E19.3.5 TestPillarTargetWebhook_ImmutableUpdate_ExternalPortChange: changing spec.external.port should be rejected", func() {
			By("setting oldObj.spec.external.port=9501 and newObj.spec.external.port=9502")
			oldObj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "10.0.0.5", Port: 9501}
			obj.Spec.External = &pillarcsiv1alpha1.ExternalSpec{Address: "10.0.0.5", Port: 9502}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(),
				"Changing spec.external.port should be rejected as it is an immutable identity field")
			Expect(err.Error()).To(ContainSubstring("9501"))
			Expect(err.Error()).To(ContainSubstring("9502"))
		})
	})
})

// =============================================================================
// E19.2: CRD OpenAPI schema validation — API server rejects invalid objects
//
// E19.2.1 TestPillarTargetCRD_InvalidCreate_EmptyNodeRefName
// E19.2.2 TestPillarTargetCRD_InvalidCreate_ExternalPortTooLow
// E19.2.3 TestPillarTargetCRD_InvalidCreate_ExternalPortTooHigh
// E19.2.4 TestPillarTargetCRD_InvalidCreate_EmptyExternalAddress
//
// These tests create objects via the real Kubernetes API server provided by
// envtest.  The CRD OpenAPI schema (generated from kubebuilder markers) must
// reject invalid field values with HTTP 422 UnprocessableEntity.
// =============================================================================
var _ = Describe("PillarTarget CRD Schema Validation", func() {
	Context("E19.2 — k8sClient.Create should be rejected for invalid specs", func() {
		It("E19.2.1 TestPillarTargetCRD_InvalidCreate_EmptyNodeRefName: should reject spec.nodeRef.name=''", func() {
			By("creating a PillarTarget with spec.nodeRef.name='' (violates MinLength=1)")
			target := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "e19-invalid-noderef-name"},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name:        "", // violates +kubebuilder:validation:MinLength=1
						AddressType: "InternalIP",
					},
				},
			}

			err := k8sClient.Create(ctx, target)
			Expect(err).To(HaveOccurred(),
				"API server should reject PillarTarget with empty spec.nodeRef.name")
		})

		It("E19.2.2 TestPillarTargetCRD_InvalidCreate_ExternalPortTooLow: should reject spec.external.port=0", func() {
			By("creating a PillarTarget with spec.external.port=0 (violates Minimum=1)")
			target := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "e19-invalid-port-low"},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.0.1",
						Port:    0, // violates +kubebuilder:validation:Minimum=1
					},
				},
			}

			err := k8sClient.Create(ctx, target)
			Expect(err).To(HaveOccurred(),
				"API server should reject PillarTarget with spec.external.port=0 (below minimum 1)")
		})

		It("E19.2.3 TestPillarTargetCRD_InvalidCreate_ExternalPortTooHigh: should reject spec.external.port=65536", func() {
			By("creating a PillarTarget with spec.external.port=65536 (violates Maximum=65535)")
			target := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "e19-invalid-port-high"},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "10.0.0.1",
						Port:    65536, // violates +kubebuilder:validation:Maximum=65535
					},
				},
			}

			err := k8sClient.Create(ctx, target)
			Expect(err).To(HaveOccurred(),
				"API server should reject PillarTarget with spec.external.port=65536 (above maximum 65535)")
		})

		It("E19.2.4 TestPillarTargetCRD_InvalidCreate_EmptyExternalAddress: should reject spec.external.address=''", func() {
			By("creating a PillarTarget with spec.external.address='' (violates MinLength=1)")
			target := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "e19-invalid-empty-address"},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{
						Address: "", // violates +kubebuilder:validation:MinLength=1
						Port:    9500,
					},
				},
			}

			err := k8sClient.Create(ctx, target)
			Expect(err).To(HaveOccurred(),
				"API server should reject PillarTarget with empty spec.external.address")
		})
	})
})
