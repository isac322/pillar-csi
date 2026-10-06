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
	"strings"

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

	// ── Issue #163 — spec.lvmSource is pinned at creation ──────────────────────
	// The apiserver itself (CEL rules generated from the kubebuilder markers)
	// must refuse every attempt to retarget, downgrade, add or remove the
	// adopted LV source; the controller is never the only guard.
	Describe("spec.lvmSource (import-lv)", func() {
		const (
			vgUUID    = "Ab12Cd-Ef34-Gh56-Ij78-Kl90-Mn12-Op34Qr"
			lvUUID    = "Zy98Xw-Vu76-Ts54-Rq32-Po10-Nm98-Lk76Ji"
			otherUUID = "Qq11Ww-Ee22-Rr33-Tt44-Yy55-Uu66-Ii77Oo"
		)
		newSource := func() *pillarcsiv1alpha1.LVMSourceRef {
			return &pillarcsiv1alpha1.LVMSourceRef{
				VolumeGroup:       "data-vg",
				LogicalVolume:     "legacy",
				VolumeGroupUUID:   vgUUID,
				LogicalVolumeUUID: lvUUID,
				PreserveOriginal:  true,
			}
		}
		newLVMVolume := func(name string, src *pillarcsiv1alpha1.LVMSourceRef) *pillarcsiv1alpha1.PillarVolumeState {
			return &pillarcsiv1alpha1.PillarVolumeState{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
					VolumeID:      "storage-1/nvmeof-tcp/lvm-lv/data-vg/legacy",
					AgentVolumeID: "data-vg/legacy",
					AgentRef:      "storage-1",
					BackendType:   "lvm-lv",
					ProtocolType:  "nvmeof-tcp",
					CapacityBytes: 1 << 30,
					LVMSource:     src,
				},
			}
		}
		createLVMVolume := func(name string, src *pillarcsiv1alpha1.LVMSourceRef) {
			Expect(k8sClient.Create(crdCtx, newLVMVolume(name, src))).To(Succeed())
			DeferCleanup(func() { deleteVolumeIfExists(name) })
		}
		getVolume := func(name string) *pillarcsiv1alpha1.PillarVolumeState {
			v := &pillarcsiv1alpha1.PillarVolumeState{}
			Expect(k8sClient.Get(crdCtx, types.NamespacedName{Name: name}, v)).To(Succeed())
			return v
		}
		expectRejected := func(err error, message string) {
			Expect(err).To(HaveOccurred())
			Expect(errors.IsInvalid(err)).To(BeTrue(), "want HTTP 422, got %v", err)
			Expect(err.Error()).To(ContainSubstring(message))
		}

		It("accepts a complete source and persists it", func() {
			const objName = "i163-lvmsource-create"
			createLVMVolume(objName, newSource())
			Expect(getVolume(objName).Spec.LVMSource).To(Equal(newSource()))
		})

		It("rejects changing any lvmSource field and keeps the stored source", func() {
			for name, mutate := range map[string]func(*pillarcsiv1alpha1.LVMSourceRef){
				"volumeGroup":       func(s *pillarcsiv1alpha1.LVMSourceRef) { s.VolumeGroup = "other-vg" },
				"logicalVolume":     func(s *pillarcsiv1alpha1.LVMSourceRef) { s.LogicalVolume = "renamed" },
				"volumeGroupUUID":   func(s *pillarcsiv1alpha1.LVMSourceRef) { s.VolumeGroupUUID = otherUUID },
				"logicalVolumeUUID": func(s *pillarcsiv1alpha1.LVMSourceRef) { s.LogicalVolumeUUID = otherUUID },
				"preserveOriginal":  func(s *pillarcsiv1alpha1.LVMSourceRef) { s.PreserveOriginal = false },
			} {
				By("changing " + name)
				objName := "i163-lvmsource-mut-" + strings.ToLower(name)
				createLVMVolume(objName, newSource())
				stored := getVolume(objName)
				mutate(stored.Spec.LVMSource)
				expectRejected(k8sClient.Update(crdCtx, stored), "lvmSource is immutable")
				Expect(getVolume(objName).Spec.LVMSource).To(Equal(newSource()))
			}
		})

		It("rejects removing lvmSource after creation", func() {
			const objName = "i163-lvmsource-remove"
			createLVMVolume(objName, newSource())
			stored := getVolume(objName)
			stored.Spec.LVMSource = nil
			expectRejected(k8sClient.Update(crdCtx, stored), "lvmSource cannot be added or removed after creation")
			Expect(getVolume(objName).Spec.LVMSource).To(Equal(newSource()))
		})

		It("rejects adding lvmSource to a volume created without it", func() {
			const objName = "i163-lvmsource-add"
			createLVMVolume(objName, nil)
			stored := getVolume(objName)
			stored.Spec.LVMSource = newSource()
			expectRejected(k8sClient.Update(crdCtx, stored), "lvmSource cannot be added or removed after creation")
			Expect(getVolume(objName).Spec.LVMSource).To(BeNil())
		})

		It("rejects creating with both importedFrom and lvmSource", func() {
			const objName = "i163-lvmsource-with-zvol"
			vol := newLVMVolume(objName, newSource())
			vol.Spec.ImportedFrom = "hot-data/k8s/legacy"
			DeferCleanup(func() { deleteVolumeIfExists(objName) })
			expectRejected(k8sClient.Create(crdCtx, vol), "importedFrom and lvmSource are mutually exclusive")
		})

		It("rejects UUIDs outside the LVM 6-4-4-4-4-4-6 format", func() {
			for name, uuid := range map[string]string{
				"short":         "Ab12Cd-Ef34",
				"bad separator": "Ab12Cd_Ef34-Gh56-Ij78-Kl90-Mn12-Op34Qr",
				"symbol":        "Ab12C!-Ef34-Gh56-Ij78-Kl90-Mn12-Op34Qr",
				"empty":         "",
			} {
				By("vg uuid " + name)
				vgObj := "i163-lvmsource-vguuid-" + strings.ReplaceAll(name, " ", "-")
				src := newSource()
				src.VolumeGroupUUID = uuid
				DeferCleanup(func() { deleteVolumeIfExists(vgObj) })
				err := k8sClient.Create(crdCtx, newLVMVolume(vgObj, src))
				Expect(errors.IsInvalid(err)).To(BeTrue(), "vg uuid %q: want 422, got %v", uuid, err)

				By("lv uuid " + name)
				lvObj := "i163-lvmsource-lvuuid-" + strings.ReplaceAll(name, " ", "-")
				src = newSource()
				src.LogicalVolumeUUID = uuid
				DeferCleanup(func() { deleteVolumeIfExists(lvObj) })
				err = k8sClient.Create(crdCtx, newLVMVolume(lvObj, src))
				Expect(errors.IsInvalid(err)).To(BeTrue(), "lv uuid %q: want 422, got %v", uuid, err)
			}
		})

		It("keeps zvol-import and managed volumes without lvmSource creatable and updatable", func() {
			const objName = "i163-lvmsource-zvol-unchanged"
			vol := &pillarcsiv1alpha1.PillarVolumeState{
				ObjectMeta: metav1.ObjectMeta{Name: objName},
				Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
					VolumeID:      "storage-1/nvmeof-tcp/zfs-zvol/tank/legacy",
					AgentVolumeID: "tank/legacy",
					AgentRef:      "storage-1",
					BackendType:   "zfs-zvol",
					ProtocolType:  "nvmeof-tcp",
					CapacityBytes: 1 << 30,
					ImportedFrom:  "tank/k8s/legacy",
				},
			}
			Expect(k8sClient.Create(crdCtx, vol)).To(Succeed())
			DeferCleanup(func() { deleteVolumeIfExists(objName) })
			stored := getVolume(objName)
			stored.Spec.CapacityBytes = 2 << 30
			Expect(k8sClient.Update(crdCtx, stored)).To(Succeed())
			updated := getVolume(objName)
			Expect(updated.Spec.CapacityBytes).To(Equal(int64(2 << 30)))
			Expect(updated.Spec.ImportedFrom).To(Equal("tank/k8s/legacy"))
			Expect(updated.Spec.LVMSource).To(BeNil())
		})
	})
})
