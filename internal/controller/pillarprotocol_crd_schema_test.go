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

// E23.2 — PillarProtocol CRD schema validation tests.
//
// These tests verify that the Kubernetes API server (running under envtest)
// enforces the OpenAPI v3 schema constraints embedded in the PillarProtocol CRD:
//   - spec.type must be one of the allowed enum values
//   - spec.nvmeofTcp.port must be in the range [1, 65535]
//   - spec.fsType must be one of the allowed enum values
//
// All tests exercise the real CRD validation path by calling k8sClient.Create and
// expecting a 422 UnprocessableEntity response with a descriptive validation error.

var _ = Describe("PillarProtocol CRD Schema Validation", func() {
	var crdCtx context.Context

	BeforeEach(func() {
		crdCtx = context.Background()
	})

	// cleanup helper — silently ignores NotFound so AfterEach is idempotent.
	deleteProtocolIfExists := func(name string) {
		p := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(crdCtx, types.NamespacedName{Name: name}, p); err == nil {
			_ = k8sClient.Delete(crdCtx, p)
		}
	}

	// ── E23.2.1 — TestPillarProtocolCRD_InvalidCreate_UnknownType ────────────
	It("Should reject creation when spec.type is not an allowed enum value", func() {
		By("attempting to create a PillarProtocol with spec.type=unknown-protocol")
		// We submit raw JSON so we can bypass Go type safety and reach the CRD
		// schema validator with an invalid enum value.
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarProtocol",
			"metadata": {"name": "crd-test-unknown-type"},
			"spec": {"type": "unknown-protocol"}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-unknown-type"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarProtocol with an unknown spec.type")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for enum violation")

		DeferCleanup(func() { deleteProtocolIfExists("crd-test-unknown-type") })
	})

	// ── E23.2.2 — TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooLow ────
	It("Should reject creation when spec.nvmeofTcp.port is below the minimum (0 < minimum=1)", func() {
		By("attempting to create a PillarProtocol with spec.nvmeofTcp.port=0")
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarProtocol",
			"metadata": {"name": "crd-test-port-low"},
			"spec": {
				"type": "nvmeof-tcp",
				"nvmeofTcp": {"port": 0}
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-port-low"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarProtocol with port=0 (below minimum=1)")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for minimum violation")

		DeferCleanup(func() { deleteProtocolIfExists("crd-test-port-low") })
	})

	// ── E23.2.3 — TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooHigh ───
	It("Should reject creation when spec.nvmeofTcp.port exceeds the maximum (65536 > maximum=65535)", func() {
		By("attempting to create a PillarProtocol with spec.nvmeofTcp.port=65536")
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarProtocol",
			"metadata": {"name": "crd-test-port-high"},
			"spec": {
				"type": "nvmeof-tcp",
				"nvmeofTcp": {"port": 65536}
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-port-high"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarProtocol with port=65536 (above maximum=65535)")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for maximum violation")

		DeferCleanup(func() { deleteProtocolIfExists("crd-test-port-high") })
	})

	// ── E23.2.4 — TestPillarProtocolCRD_InvalidCreate_InvalidFSType ──────────
	It("Should reject creation when spec.fsType is not an allowed enum value (btrfs is not in enum)", func() {
		By("attempting to create a PillarProtocol with spec.fsType=btrfs")
		rawJSON := []byte(`{
			"apiVersion": "pillar-csi.pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarProtocol",
			"metadata": {"name": "crd-test-fstype"},
			"spec": {
				"type": "nvmeof-tcp",
				"fsType": "btrfs"
			}
		}`)

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-test-fstype"},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarProtocol with fsType=btrfs (not in enum [ext4, xfs])")

		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue(), "error should be a *errors.StatusError")
		Expect(statusErr.ErrStatus.Code).To(Equal(int32(422)),
			"HTTP status code should be 422 UnprocessableEntity for fsType enum violation")

		DeferCleanup(func() { deleteProtocolIfExists("crd-test-fstype") })
	})
})
