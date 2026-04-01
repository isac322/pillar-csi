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

// E21.4 — PillarTarget CRD OpenAPI schema validation tests.
//
// These tests verify that the envtest API server enforces the OpenAPI v3
// schema constraints generated from the kubebuilder markers in
// api/v1alpha1/pillartarget_types.go:
//
//   - spec.nodeRef.name:        MinLength=1
//   - spec.external.address:    MinLength=1
//   - spec.external.port:       Minimum=1, Maximum=65535
//   - spec.nodeRef.addressType: Enum=InternalIP;ExternalIP
//
// All tests attempt to create a PillarTarget that violates one constraint and
// expect the API server to return HTTP 422 (StatusReasonInvalid).
//
// TC IDs: 163–166  (E21.4 series)

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("PillarTarget CRD Schema Validation", func() {
	var crdCtx context.Context

	BeforeEach(func() {
		crdCtx = context.Background()
	})

	// cleanup helper — silently ignores NotFound so cleanup is idempotent.
	deleteTargetIfExists := func(name string) {
		t := &pillarcsiv1alpha1.PillarTarget{}
		if err := k8sClient.Get(crdCtx, types.NamespacedName{Name: name}, t); err == nil {
			// Strip any finalizers so the object can be garbage-collected.
			t.Finalizers = nil
			_ = k8sClient.Update(crdCtx, t)
			_ = k8sClient.Delete(crdCtx, t)
		}
	}

	// ── E21.4 TC-163 — TestCRDSchema_PillarTarget_NodeRefName_Empty ──────────
	// spec.nodeRef.name is annotated +kubebuilder:validation:MinLength=1.
	// Submitting an empty string should be rejected with HTTP 422.
	It("TC-163: Should reject PillarTarget with empty spec.nodeRef.name (MinLength=1 violation)", func() {
		const objName = "e214-target-empty-noderef-name"
		By("submitting a PillarTarget whose spec.nodeRef.name is an empty string")

		// Use Server-Side Apply with raw JSON so the Go struct zero-values are
		// sent on the wire and the CRD validator sees the MinLength constraint.
		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarTarget",
			"metadata": {"name": %q},
			"spec": {
				"nodeRef": {"name": ""}
			}
		}`, objName))

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: objName},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarTarget with empty spec.nodeRef.name")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for MinLength=1 violation")

		DeferCleanup(func() { deleteTargetIfExists(objName) })
	})

	// ── E21.4 TC-164 — TestCRDSchema_PillarTarget_ExternalPort_Zero ──────────
	// spec.external.port is annotated +kubebuilder:validation:Minimum=1.
	// A port value of 0 should be rejected.
	It("TC-164: Should reject PillarTarget with spec.external.port=0 (Minimum=1 violation)", func() {
		const objName = "e214-target-port-zero"
		By("submitting a PillarTarget whose spec.external.port is 0")

		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarTarget",
			"metadata": {"name": %q},
			"spec": {
				"external": {"address": "10.0.0.1", "port": 0}
			}
		}`, objName))

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: objName},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarTarget with spec.external.port=0")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for Minimum=1 violation")

		DeferCleanup(func() { deleteTargetIfExists(objName) })
	})

	// ── E21.4 TC-165 — TestCRDSchema_PillarTarget_ExternalAddress_Empty ──────
	// spec.external.address is annotated +kubebuilder:validation:MinLength=1.
	// An empty address string should be rejected.
	It("TC-165: Should reject PillarTarget with empty spec.external.address (MinLength=1 violation)", func() {
		const objName = "e214-target-empty-address"
		By("submitting a PillarTarget whose spec.external.address is an empty string")

		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarTarget",
			"metadata": {"name": %q},
			"spec": {
				"external": {"address": "", "port": 9500}
			}
		}`, objName))

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: objName},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarTarget with empty spec.external.address")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for MinLength=1 violation")

		DeferCleanup(func() { deleteTargetIfExists(objName) })
	})

	// ── E21.4 TC-166 — TestCRDSchema_PillarTarget_NodeRefAddressType_Invalid ─
	// spec.nodeRef.addressType is annotated +kubebuilder:validation:Enum=InternalIP;ExternalIP.
	// A value outside the enum should be rejected.
	It("TC-166: Should reject PillarTarget with invalid spec.nodeRef.addressType (Enum violation)", func() {
		const objName = "e214-target-bad-addrtype"
		By("submitting a PillarTarget with spec.nodeRef.addressType=Hostname (not in enum)")

		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarTarget",
			"metadata": {"name": %q},
			"spec": {
				"nodeRef": {"name": "worker-1", "addressType": "Hostname"}
			}
		}`, objName))

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: objName},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarTarget with addressType=Hostname (not in enum)")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for enum violation")

		DeferCleanup(func() { deleteTargetIfExists(objName) })
	})
})
