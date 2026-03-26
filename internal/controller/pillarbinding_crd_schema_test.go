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

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// E25.2 — PillarBinding CRD schema validation tests.
//
// These tests verify that the Kubernetes API server (running under envtest)
// enforces the OpenAPI v3 schema constraints embedded in the PillarBinding CRD:
//   - spec.poolRef must be non-empty (MinLength=1)
//   - spec.protocolRef must be non-empty (MinLength=1)
//   - spec.storageClass.reclaimPolicy must be one of the allowed enum values (Delete, Retain)
//
// All tests exercise the real CRD validation path by sending invalid objects and
// expecting a 422 UnprocessableEntity response with a descriptive validation error.

var _ = Describe("PillarBinding CRD Schema Validation", func() {
	var crdCtx context.Context

	BeforeEach(func() {
		crdCtx = context.Background()
	})

	// cleanup helper — silently ignores NotFound so AfterEach is idempotent.
	deleteBindingIfExists := func(name string) {
		b := &pillarcsiv1alpha1.PillarBinding{}
		if err := k8sClient.Get(crdCtx, types.NamespacedName{Name: name}, b); err == nil {
			// Strip any finalizers so the object can be garbage-collected.
			b.Finalizers = nil
			_ = k8sClient.Update(crdCtx, b)
			_ = k8sClient.Delete(crdCtx, b)
		}
	}

	// ── E25.2.1 — TestPillarBindingCRD_InvalidCreate_EmptyPoolRef ────────────
	It("Should reject creation when spec.poolRef is an empty string", func() {
		By("attempting to create a PillarBinding with spec.poolRef=\"\"")
		// We use a Server-Side Apply patch with an explicit empty poolRef so the
		// CRD schema validator sees a MinLength=1 violation rather than a missing
		// field. The Go struct serialises "" without omitempty, so the field is
		// present in the JSON and the minimum-length check fires.
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarBinding",
			"metadata": {"name": "crd-test-empty-pool-ref"},
			"spec": {
				"poolRef": "",
				"protocolRef": "some-protocol"
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-empty-pool-ref"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarBinding with empty spec.poolRef")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for MinLength violation")

		DeferCleanup(func() { deleteBindingIfExists("crd-test-empty-pool-ref") })
	})

	// ── E25.2.2 — TestPillarBindingCRD_InvalidCreate_EmptyProtocolRef ────────
	It("Should reject creation when spec.protocolRef is an empty string", func() {
		By("attempting to create a PillarBinding with spec.protocolRef=\"\"")
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarBinding",
			"metadata": {"name": "crd-test-empty-protocol-ref"},
			"spec": {
				"poolRef": "some-pool",
				"protocolRef": ""
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-empty-protocol-ref"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarBinding with empty spec.protocolRef")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for MinLength violation")

		DeferCleanup(func() { deleteBindingIfExists("crd-test-empty-protocol-ref") })
	})

	// ── E25.2.3 — TestPillarBindingCRD_InvalidCreate_InvalidReclaimPolicy ────
	It("Should reject creation when spec.storageClass.reclaimPolicy is not an allowed enum value", func() {
		By("attempting to create a PillarBinding with spec.storageClass.reclaimPolicy=\"Archive\"")
		// "Archive" is not in the enum [Delete, Retain] so the CRD schema should
		// reject it with a 422 response.
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarBinding",
			"metadata": {"name": "crd-test-invalid-reclaim"},
			"spec": {
				"poolRef": "some-pool",
				"protocolRef": "some-protocol",
				"storageClass": {
					"reclaimPolicy": "Archive"
				}
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-invalid-reclaim"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarBinding with reclaimPolicy=Archive (not in enum)")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for enum violation")

		DeferCleanup(func() { deleteBindingIfExists("crd-test-invalid-reclaim") })
	})
})
