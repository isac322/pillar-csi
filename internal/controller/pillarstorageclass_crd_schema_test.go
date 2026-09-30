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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// E25.2 — PillarStorageClass CRD schema validation tests.
//
// These tests verify that the Kubernetes API server (running under envtest)
// enforces the OpenAPI v3 schema constraints embedded in the PillarStorageClass CRD:
//   - spec.storeRef must be non-empty (MinLength=1)
//   - spec.protocolRef must be non-empty (MinLength=1)
//   - spec.storageClass.reclaimPolicy must be one of the allowed enum values (Delete, Retain)
//   - spec.filesystem.fsType must be one of the allowed enum values (ext4, xfs)
//   - spec.overrides.backend and spec.overrides.protocol are exactly-one unions
//   - the removed spec.overrides.fsType/mkfsOptions fields are rejected
//
// All tests exercise the real CRD validation path by sending invalid objects and
// expecting a 422 UnprocessableEntity response with a descriptive validation error.

var _ = Describe("PillarStorageClass CRD Schema Validation", func() {
	var crdCtx context.Context

	BeforeEach(func() {
		crdCtx = context.Background()
	})

	// cleanup helper — silently ignores NotFound so AfterEach is idempotent.
	deleteBindingIfExists := func(name string) {
		b := &pillarcsiv1alpha1.PillarStorageClass{}
		if err := k8sClient.Get(crdCtx, types.NamespacedName{Name: name}, b); err == nil {
			// Strip any finalizers so the object can be garbage-collected.
			b.Finalizers = nil
			_ = k8sClient.Update(crdCtx, b)
			_ = k8sClient.Delete(crdCtx, b)
		}
	}

	// ── E25.2.1 — TestPillarStorageClassCRD_InvalidCreate_EmptyStoreRef ────────────
	It("Should reject creation when spec.storeRef is an empty string", func() {
		By("attempting to create a PillarStorageClass with spec.storeRef=\"\"")
		// We use a Server-Side Apply patch with an explicit empty storeRef so the
		// CRD schema validator sees a MinLength=1 violation rather than a missing
		// field. The Go struct serialises "" without omitempty, so the field is
		// present in the JSON and the minimum-length check fires.
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarStorageClass",
			"metadata": {"name": "crd-test-empty-pool-ref"},
			"spec": {
				"storeRef": "",
				"protocolRef": "some-protocol"
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-empty-pool-ref"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarStorageClass with empty spec.storeRef")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for MinLength violation")

		DeferCleanup(func() { deleteBindingIfExists("crd-test-empty-pool-ref") })
	})

	// ── E25.2.2 — TestPillarStorageClassCRD_InvalidCreate_EmptyProtocolRef ────────
	It("Should reject creation when spec.protocolRef is an empty string", func() {
		By("attempting to create a PillarStorageClass with spec.protocolRef=\"\"")
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarStorageClass",
			"metadata": {"name": "crd-test-empty-protocol-ref"},
			"spec": {
				"storeRef": "some-pool",
				"protocolRef": ""
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-empty-protocol-ref"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarStorageClass with empty spec.protocolRef")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for MinLength violation")

		DeferCleanup(func() { deleteBindingIfExists("crd-test-empty-protocol-ref") })
	})

	// ── E25.2.3 — TestPillarStorageClassCRD_InvalidCreate_InvalidReclaimPolicy ────
	It("Should reject creation when spec.storageClass.reclaimPolicy is not an allowed enum value", func() {
		By("attempting to create a PillarStorageClass with spec.storageClass.reclaimPolicy=\"Archive\"")
		// "Archive" is not in the enum [Delete, Retain] so the CRD schema should
		// reject it with a 422 response.
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarStorageClass",
			"metadata": {"name": "crd-test-invalid-reclaim"},
			"spec": {
				"storeRef": "some-pool",
				"protocolRef": "some-protocol",
				"storageClass": {
					"reclaimPolicy": "Archive"
				}
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-invalid-reclaim"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarStorageClass with reclaimPolicy=Archive (not in enum)")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for enum violation")

		DeferCleanup(func() { deleteBindingIfExists("crd-test-invalid-reclaim") })
	})

	// applyBinding server-side applies a PillarStorageClass named name with the
	// given raw spec JSON and returns the API server's answer.
	applyBinding := func(name, spec string) error {
		DeferCleanup(func() { deleteBindingIfExists(name) })
		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarStorageClass",
			"metadata": {"name": %q},
			"spec": %s
		}`, name, spec))
		return k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarStorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)
	}

	expectUnprocessable := func(err error, contains string) {
		Expect(err).To(HaveOccurred())
		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError: %v", err)
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity: %v", err)
		Expect(err.Error()).To(ContainSubstring(contains))
	}

	// ── E25.2.4 — TestPillarStorageClassCRD_InvalidCreate_InvalidFSType ──────
	It("Should reject spec.filesystem.fsType outside the enum (btrfs)", func() {
		err := applyBinding("crd-test-fstype",
			`{"storeRef": "p", "protocolRef": "q", "filesystem": {"fsType": "btrfs"}}`)
		expectUnprocessable(err, "spec.filesystem.fsType")
	})

	// ── E25.2.5 — TestPillarStorageClassCRD_InvalidCreate_TwoBackendOverrides ─
	It("Should reject spec.overrides.backend with both zfs and lvm", func() {
		err := applyBinding("crd-test-two-backend-overrides", `{"storeRef": "p", "protocolRef": "q",
			"overrides": {"backend": {"zfs": {"properties": {"compression": "zstd"}}, "lvm": {"provisioningMode": "thin"}}}}`)
		expectUnprocessable(err, "exactly one of zfs or lvm must be set")
	})

	// ── E25.2.6 — TestPillarStorageClassCRD_InvalidCreate_EmptyBackendOverrides
	It("Should reject spec.overrides.backend without a member", func() {
		err := applyBinding("crd-test-empty-backend-overrides",
			`{"storeRef": "p", "protocolRef": "q", "overrides": {"backend": {}}}`)
		expectUnprocessable(err, "exactly one of zfs or lvm must be set")
	})

	// ── E25.2.7 — TestPillarStorageClassCRD_InvalidCreate_EmptyProtocolOverrides
	It("Should reject spec.overrides.protocol without a member", func() {
		err := applyBinding("crd-test-empty-protocol-overrides",
			`{"storeRef": "p", "protocolRef": "q", "overrides": {"protocol": {}}}`)
		expectUnprocessable(err, "exactly one protocol member must be set")
	})

	// ── E25.2.8 — TestPillarStorageClassCRD_InvalidCreate_RemovedOverrideFields
	It("Should reject the removed spec.overrides.fsType field", func() {
		err := applyBinding("crd-test-removed-override-fstype",
			`{"storeRef": "p", "protocolRef": "q", "overrides": {"fsType": "xfs"}}`)
		Expect(err).To(HaveOccurred(), "removed fields must not be accepted")
		Expect(err.Error()).To(ContainSubstring(".spec.overrides.fsType: field not declared in schema"))
	})

	// ── E25.2.9 — TestPillarStorageClassCRD_InvalidCreate_StructuralOverride ──
	It("Should reject a structural field in a backend override (zfs.pool)", func() {
		err := applyBinding("crd-test-structural-override",
			`{"storeRef": "p", "protocolRef": "q", "overrides": {"backend": {"zfs": {"pool": "other"}}}}`)
		Expect(err).To(HaveOccurred(), "structural fields are not part of the override schema")
		Expect(err.Error()).To(ContainSubstring(".spec.overrides.backend.zfs.pool: field not declared in schema"))
	})

	// ── E25.2.10 — TestPillarStorageClassCRD_InvalidCreate_TwoProtocolOverrides
	It("Should reject spec.overrides.protocol with both nvmeofTcp and iscsi", func() {
		err := applyBinding("crd-test-two-protocol-overrides", `{"storeRef": "p", "protocolRef": "q",
			"overrides": {"protocol": {"nvmeofTcp": {"ctrlLossTmo": 30}, "iscsi": {"loginTimeout": 30}}}}`)
		expectUnprocessable(err, "exactly one protocol member must be set (supported: nvmeofTcp, iscsi)")
	})

	// ── E25.2.11 — TestPillarStorageClassCRD_InvalidCreate_ISCSIStructuralOverride
	It("Should reject a structural field in an iscsi protocol override (iscsi.acl)", func() {
		err := applyBinding("crd-test-iscsi-structural-override",
			`{"storeRef": "p", "protocolRef": "q", "overrides": {"protocol": {"iscsi": {"acl": true}}}}`)
		Expect(err).To(HaveOccurred(), "structural fields are not part of the override schema")
		Expect(err.Error()).To(ContainSubstring(".spec.overrides.protocol.iscsi.acl: field not declared in schema"))
	})

	// ── E25.2.12 — TestPillarStorageClassCRD_ValidCreate_ISCSIOverride ─────────
	It("Should accept an iscsi protocol override", func() {
		Expect(applyBinding("crd-test-iscsi-override",
			`{"storeRef": "p", "protocolRef": "q", "overrides": {"protocol": {"iscsi": {"loginTimeout": 30}}}}`)).
			To(Succeed())
	})
})
