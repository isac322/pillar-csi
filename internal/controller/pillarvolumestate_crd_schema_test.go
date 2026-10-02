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

// E21.4 — PillarVolumeState CRD OpenAPI schema validation tests.
//
// These tests verify that the envtest API server enforces the OpenAPI v3
// schema constraints generated from the kubebuilder markers in
// api/v1alpha1/pillarvolumestate_types.go:
//
//   - spec.capacityBytes:  Minimum=0
//   - status.phase:        Enum=Provisioning;CreatePartial;Ready;...
//
// TC IDs: 169–170  (E21.4 series)

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

var _ = Describe("PillarVolumeState CRD Schema Validation", func() {
	var crdCtx context.Context

	BeforeEach(func() {
		crdCtx = context.Background()
	})

	// cleanup helper — silently ignores NotFound so cleanup is idempotent.
	deleteVolumeIfExists := func(name string) {
		v := &pillarcsiv1alpha1.PillarVolumeState{}
		if err := k8sClient.Get(crdCtx, types.NamespacedName{Name: name}, v); err == nil {
			v.Finalizers = nil
			_ = k8sClient.Update(crdCtx, v)
			_ = k8sClient.Delete(crdCtx, v)
		}
	}

	// ── E21.4 TC-169 — TestCRDSchema_PillarVolumeState_Phase_Invalid ─────────────
	// status.phase is annotated +kubebuilder:validation:Enum=Provisioning;...
	// Setting phase to an unknown value via the status subresource should be
	// rejected with HTTP 422.
	It("TC-169: Should reject PillarVolumeState status update with invalid phase (Enum violation)", func() {
		const objName = "e214-volume-invalid-phase"
		By("creating a valid PillarVolumeState first")
		vol := &pillarcsiv1alpha1.PillarVolumeState{
			ObjectMeta: metav1.ObjectMeta{Name: objName},
			Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
				VolumeID:      "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-schema-test",
				AgentVolumeID: "tank/pvc-schema-test",
				AgentRef:      "storage-1",
				BackendType:   "zfs-zvol",
				ProtocolType:  "nvmeof-tcp",
				CapacityBytes: 1 << 30, // 1 GiB
			},
		}
		Expect(k8sClient.Create(crdCtx, vol)).To(Succeed())
		DeferCleanup(func() { deleteVolumeIfExists(objName) })

		By("attempting to set status.phase to an invalid enum value via status subresource")
		rawPatch := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarVolumeState",
			"metadata": {"name": %q},
			"status": {"phase": "InvalidPhase"}
		}`, objName))

		err := k8sClient.Status().Patch(
			crdCtx,
			vol,
			client.RawPatch(types.MergePatchType, rawPatch),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarVolumeState status update with phase=InvalidPhase")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for enum violation in status.phase")
	})

	It("NFS export state version accepts 4.2 and rejects unsupported versions without persistence", func() {
		const objName = "e214-volume-nfs-export-version"
		vol := &pillarcsiv1alpha1.PillarVolumeState{
			ObjectMeta: metav1.ObjectMeta{Name: objName},
			Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
				VolumeID:      "storage-1/nfs/zfs-dataset/tank/pvc-schema-nfs",
				AgentVolumeID: "tank/pvc-schema-nfs",
				AgentRef:      "storage-1",
				BackendType:   "zfs-dataset",
				ProtocolType:  "nfs",
				CapacityBytes: 1 << 30,
			},
		}
		Expect(k8sClient.Create(crdCtx, vol)).To(Succeed())
		DeferCleanup(func() { deleteVolumeIfExists(objName) })

		By("persisting the supported NFS version with its export configuration")
		exportSpec := &pillarcsiv1alpha1.VolumeExportSpec{
			BindAddress: "10.0.0.10",
			Port:        2049,
			ACLEnabled:  true,
			NFS: &pillarcsiv1alpha1.NFSExportSpec{
				Version:  "4.2",
				Squash:   pillarcsiv1alpha1.NFSSquashRoot,
				Readonly: true,
			},
		}
		vol.Status.ExportSpec = exportSpec
		Expect(k8sClient.Status().Update(crdCtx, vol)).To(Succeed())

		key := types.NamespacedName{Name: objName}
		stored := &pillarcsiv1alpha1.PillarVolumeState{}
		Expect(k8sClient.Get(crdCtx, key, stored)).To(Succeed())
		Expect(stored.Status.ExportSpec).To(Equal(exportSpec))

		By("rejecting an unsupported NFS version at the status field")
		stored.Status.ExportSpec.NFS.Version = "4.1"
		err := k8sClient.Status().Update(crdCtx, stored)
		Expect(errors.IsInvalid(err)).To(BeTrue())
		statusErr, ok := err.(*errors.StatusError)
		Expect(ok).To(BeTrue())
		Expect(statusErr.ErrStatus.Details).NotTo(BeNil())
		Expect(statusErr.ErrStatus.Details.Causes).To(ContainElement(
			HaveField("Field", "status.exportSpec.nfs.version"),
		))

		By("retaining the supported version after the rejected update")
		retained := &pillarcsiv1alpha1.PillarVolumeState{}
		Expect(k8sClient.Get(crdCtx, key, retained)).To(Succeed())
		Expect(retained.Status.ExportSpec).To(Equal(exportSpec))
	})

	// ── E21.4 TC-170 — TestCRDSchema_PillarVolumeState_CapacityBytes_Negative ────
	// spec.capacityBytes is annotated +kubebuilder:validation:Minimum=0.
	// A negative value should be rejected at create time.
	It("TC-170: Should reject PillarVolumeState with negative spec.capacityBytes (Minimum=0 violation)", func() {
		const objName = "e214-volume-neg-capacity"
		By("submitting a PillarVolumeState with spec.capacityBytes=-1 via Server-Side Apply")

		rawJSON := []byte(fmt.Sprintf(`{
			"apiVersion": "pillar-csi.bhyoo.com/v1alpha1",
			"kind": "PillarVolumeState",
			"metadata": {"name": %q},
			"spec": {
				"volumeID":      "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-neg",
				"agentVolumeID": "tank/pvc-neg",
				"agentRef":     "storage-1",
				"backendType":   "zfs-zvol",
				"protocolType":  "nvmeof-tcp",
				"capacityBytes": -1
			}
		}`, objName))

		err := k8sClient.Patch(
			crdCtx,
			&pillarcsiv1alpha1.PillarVolumeState{
				ObjectMeta: metav1.ObjectMeta{Name: objName},
			},
			client.RawPatch(types.ApplyPatchType, rawJSON),
			client.ForceOwnership,
			client.FieldOwner("e2e-test"),
		)

		Expect(err).To(HaveOccurred(),
			"API server should reject PillarVolumeState with spec.capacityBytes=-1")
		Expect(errors.IsInvalid(err)).To(BeTrue(),
			"error should be HTTP 422 for Minimum=0 violation")

		DeferCleanup(func() { deleteVolumeIfExists(objName) })
	})
})
