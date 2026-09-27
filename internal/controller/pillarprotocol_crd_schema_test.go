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

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// E23.2 — PillarProtocol CRD schema validation tests.
//
// These tests verify that the Kubernetes API server (running under envtest)
// enforces the OpenAPI v3 schema constraints embedded in the PillarProtocol CRD:
//   - spec.protocol is an exactly-one union: a union without a member is rejected
//   - members the schema does not declare (removed protocols, the removed
//     spec.type/spec.fsType fields) are rejected, not silently dropped
//   - spec.protocol.nvmeofTcp.port must be in the range [1, 65535]
//
// All tests exercise the real CRD validation path through a Server-Side Apply
// of raw JSON, bypassing Go type safety.

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

	// applyProtocol server-side applies a PillarProtocol named name with the
	// given raw spec JSON and returns the API server's answer.
	applyProtocol := func(name, spec string) error {
		DeferCleanup(func() { deleteProtocolIfExists(name) })
		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarProtocol",
			"metadata": {"name": %q},
			"spec": %s
		}`, name, spec))
		return k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarProtocol{ObjectMeta: metav1.ObjectMeta{Name: name}},
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

	// ── E23.2.1 — TestPillarProtocolCRD_InvalidCreate_NoProtocolMember ────────
	It("Should reject creation when spec.protocol sets no member", func() {
		err := applyProtocol("crd-test-no-member", `{"protocol": {}}`)
		expectUnprocessable(err, "exactly one protocol member must be set")
	})

	// ── E23.2.2 — TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooLow ────
	It("Should reject creation when spec.protocol.nvmeofTcp.port is below the minimum (0 < minimum=1)", func() {
		err := applyProtocol("crd-test-port-low", `{"protocol": {"nvmeofTcp": {"port": 0}}}`)
		expectUnprocessable(err, "spec.protocol.nvmeofTcp.port")
	})

	// ── E23.2.3 — TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooHigh ───
	It("Should reject creation when spec.protocol.nvmeofTcp.port exceeds the maximum (65536 > maximum=65535)", func() {
		err := applyProtocol("crd-test-port-high", `{"protocol": {"nvmeofTcp": {"port": 65536}}}`)
		expectUnprocessable(err, "spec.protocol.nvmeofTcp.port")
	})

	// ── E23.2.4 — TestPillarProtocolCRD_InvalidCreate_RemovedProtocolMember ──
	It("Should reject a protocol member the schema does not serve (iscsi)", func() {
		err := applyProtocol("crd-test-iscsi", `{"protocol": {"iscsi": {"port": 3260}}}`)
		Expect(err).To(HaveOccurred(), "an unserved protocol member must not be accepted")
		Expect(err.Error()).To(ContainSubstring(".spec.protocol.iscsi: field not declared in schema"))
	})

	// ── E23.2.5 — TestPillarProtocolCRD_InvalidCreate_RemovedTopLevelFields ──
	It("Should reject the removed spec.type and spec.fsType fields", func() {
		err := applyProtocol("crd-test-old-fields",
			`{"type": "nvmeof-tcp", "fsType": "ext4", "protocol": {"nvmeofTcp": {}}}`)
		Expect(err).To(HaveOccurred(), "removed fields must not be accepted")
		Expect(err.Error()).To(MatchRegexp(`\.spec\.(type|fsType): field not declared in schema`))
	})

	// ── E23.2.6 — TestPillarProtocolCRD_ValidCreate_Defaults ──────────────────
	It("Should accept nvmeofTcp and default port=4420 and acl=false", func() {
		Expect(applyProtocol("crd-test-defaults", `{"protocol": {"nvmeofTcp": {}}}`)).To(Succeed())
		got := &pillarcsiv1alpha1.PillarProtocol{}
		Expect(k8sClient.Get(crdCtx, types.NamespacedName{Name: "crd-test-defaults"}, got)).To(Succeed())
		Expect(got.Spec.Protocol.NVMeOFTCP).NotTo(BeNil())
		Expect(got.Spec.Protocol.NVMeOFTCP.Port).To(Equal(int32(4420)))
		Expect(got.Spec.Protocol.NVMeOFTCP.ACL).To(BeFalse())
	})
})
