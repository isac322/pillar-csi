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
	"context"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// E21.3: PillarStore webhook — union validation and immutable field update rejection.
//
// These tests validate that PillarStoreCustomValidator rejects an empty or
// double-member backend union on create, and on update rejects mutations to
// spec.agentRef, the backend member (zfs ↔ lvm), spec.backend.zfs.pool and
// spec.backend.lvm.volumeGroup, which are immutable because changing them
// would invalidate all volumes provisioned from the store.
//
// All tests call the validator directly — no envtest API server is required for
// compilation or execution of the validator logic.

func zfsBackend(pool string) pillarcsiv1alpha1.BackendSpec {
	return pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{Pool: pool}}
}

func lvmBackend(vg string) pillarcsiv1alpha1.BackendSpec {
	return pillarcsiv1alpha1.BackendSpec{LVM: &pillarcsiv1alpha1.LVMBackendConfig{VolumeGroup: vg}}
}

var _ = Describe("PillarStore Webhook", func() {
	var (
		ctx       context.Context
		obj       *pillarcsiv1alpha1.PillarStore
		oldObj    *pillarcsiv1alpha1.PillarStore
		validator PillarStoreCustomValidator
	)

	BeforeEach(func() {
		ctx = context.Background()
		obj = &pillarcsiv1alpha1.PillarStore{}
		oldObj = &pillarcsiv1alpha1.PillarStore{}
		validator = PillarStoreCustomValidator{}
	})

	Context("When creating or updating PillarStore under Validating Webhook", func() {
		// ── E21.3 — ID 158 ──────────────────────────────────────────────────────
		// TestPillarStoreWebhook_Update_AgentRefImmutable
		It("Should deny update when spec.agentRef is changed", func() {
			oldObj.Spec.AgentRef = "target-a"
			oldObj.Spec.Backend = zfsBackend("tank")
			obj.Spec.AgentRef = "target-b"
			obj.Spec.Backend = zfsBackend("tank")

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when spec.agentRef is changed")
			Expect(err.Error()).To(ContainSubstring("target-a"), "Error should mention old agentRef value")
			Expect(err.Error()).To(ContainSubstring("target-b"), "Error should mention new agentRef value")
		})

		// ── E21.3 — ID 159 ──────────────────────────────────────────────────────
		// TestPillarStoreWebhook_Update_BackendMemberImmutable
		It("Should deny update when the backend member changes from zfs to lvm", func() {
			oldObj.Spec.AgentRef = "t1"
			oldObj.Spec.Backend = zfsBackend("tank")
			obj.Spec.AgentRef = "t1"
			obj.Spec.Backend = lvmBackend("data-vg")

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Expected error when the backend member is changed")
			Expect(err.Error()).To(ContainSubstring("spec.backend"), "Error should name spec.backend")
			Expect(err.Error()).To(ContainSubstring(`"zfs"`), "Error should mention the old member")
			Expect(err.Error()).To(ContainSubstring(`"lvm"`), "Error should mention the new member")
		})

		// ── E21.3 — ID 160 ──────────────────────────────────────────────────────
		// TestPillarStoreWebhook_Update_ZFSPoolImmutable
		It("Should deny update when only the ZFS pool name changes", func() {
			oldObj.Spec.AgentRef = "t1"
			oldObj.Spec.Backend = zfsBackend("tank")
			obj.Spec.AgentRef = "t1"
			obj.Spec.Backend = zfsBackend("new-tank")

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Changing the ZFS pool name should be rejected")
			Expect(err.Error()).To(ContainSubstring("spec.backend.zfs.pool"))
		})

		// TestPillarStoreWebhook_Update_LVMVolumeGroupImmutable
		It("Should deny update when only the LVM volume group changes", func() {
			oldObj.Spec.AgentRef = "t1"
			oldObj.Spec.Backend = lvmBackend("data-vg")
			obj.Spec.AgentRef = "t1"
			obj.Spec.Backend = lvmBackend("other-vg")

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred(), "Changing the LVM volume group should be rejected")
			Expect(err.Error()).To(ContainSubstring("spec.backend.lvm.volumeGroup"))
		})

		// ── E21.3 — ID 161 ──────────────────────────────────────────────────────
		// TestPillarStoreWebhook_Update_BothFieldsChanged_MultipleErrors
		It("Should return errors for both spec.agentRef and spec.backend when both change", func() {
			oldObj.Spec.AgentRef = "t1"
			oldObj.Spec.Backend = zfsBackend("tank")
			obj.Spec.AgentRef = "t2"
			obj.Spec.Backend = lvmBackend("data-vg")

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.agentRef"))
			Expect(err.Error()).To(ContainSubstring("spec.backend"))
		})

		// ── E21.3 — ID 162 ──────────────────────────────────────────────────────
		// TestPillarStoreWebhook_Create_Valid
		It("Should allow valid zfs and lvm PillarStore creation", func() {
			obj.Spec.AgentRef = "target-1"
			obj.Spec.Backend = zfsBackend("tank")
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())

			obj.Spec.Backend = lvmBackend("data-vg")
			_, err = validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		// ── E32.1 TC-280 ───────────────────────────────────────────────────────
		// TestPillarStore_EmptyBackendUnion_Rejected
		It("TC-280: should reject a PillarStore whose backend sets no member", func() {
			obj.Spec.AgentRef = "storage-1"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of zfs or lvm"))
		})

		// ── E20.3.4 ───────────────────────────────────────────────────────────
		// TestPillarStoreWebhook_MutableUpdate_ZFSPropertiesChange
		It("Should allow update when only zfs.properties change", func() {
			oldObj.Spec.AgentRef = "t1"
			oldObj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
				Pool: "hot-data", Properties: map[string]string{"compression": "off"},
			}}
			obj.Spec.AgentRef = "t1"
			obj.Spec.Backend = pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
				Pool: "hot-data", Properties: map[string]string{"compression": "lz4"},
			}}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred(), "zfs.properties is not an immutable field")
		})
	})
})

func TestPillarStore_ValidateCreate_BackendUnion(t *testing.T) {
	tests := []struct {
		name     string
		backend  pillarcsiv1alpha1.BackendSpec
		wantPath string // expected field path; admitted when blank
	}{
		{name: "zfs admitted", backend: zfsBackend("tank")},
		{name: "lvm admitted", backend: lvmBackend("data-vg")},
		{name: "empty union", backend: pillarcsiv1alpha1.BackendSpec{}, wantPath: "spec.backend"},
		{
			name: "both members",
			backend: pillarcsiv1alpha1.BackendSpec{
				ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
				LVM: &pillarcsiv1alpha1.LVMBackendConfig{VolumeGroup: "data-vg"},
			},
			wantPath: "spec.backend",
		},
		{name: "empty zfs pool", backend: zfsBackend(""), wantPath: "spec.backend.zfs.pool"},
		{name: "empty lvm volumeGroup", backend: lvmBackend(""), wantPath: "spec.backend.lvm.volumeGroup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &pillarcsiv1alpha1.PillarStore{Spec: pillarcsiv1alpha1.PillarStoreSpec{
				AgentRef: "target-a", Backend: tt.backend,
			}}
			_, err := (&PillarStoreCustomValidator{}).ValidateCreate(context.Background(), store)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("ValidateCreate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCreate() error = nil, want error on %s", tt.wantPath)
			}
			if !strings.Contains(err.Error(), tt.wantPath) {
				t.Fatalf("ValidateCreate() error = %v, want path %s", err, tt.wantPath)
			}
		})
	}
}

func TestPillarStore_BackendImmutable(t *testing.T) {
	tests := []struct {
		name       string
		oldBackend pillarcsiv1alpha1.BackendSpec
		newBackend pillarcsiv1alpha1.BackendSpec
		wantPath   string // expected field path; admitted when blank
	}{
		{name: "zfs pool rename forbidden", oldBackend: zfsBackend("tank"), newBackend: zfsBackend("tank2"),
			wantPath: "spec.backend.zfs.pool"},
		{name: "lvm volumeGroup rename forbidden", oldBackend: lvmBackend("data-vg"),
			newBackend: lvmBackend("other-vg"), wantPath: "spec.backend.lvm.volumeGroup"},
		{name: "zfs to lvm forbidden", oldBackend: zfsBackend("tank"), newBackend: lvmBackend("tank"),
			wantPath: "spec.backend"},
		{name: "lvm to zfs forbidden", oldBackend: lvmBackend("data-vg"), newBackend: zfsBackend("data-vg"),
			wantPath: "spec.backend"},
		{name: "zfs unchanged", oldBackend: zfsBackend("tank"), newBackend: zfsBackend("tank")},
		{name: "lvm unchanged", oldBackend: lvmBackend("data-vg"), newBackend: lvmBackend("data-vg")},
		{
			name:       "lvm thinPool and provisioningMode mutable",
			oldBackend: lvmBackend("data-vg"),
			newBackend: pillarcsiv1alpha1.BackendSpec{LVM: &pillarcsiv1alpha1.LVMBackendConfig{
				VolumeGroup: "data-vg", ThinPool: "thin0",
				ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeThin,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldStore := &pillarcsiv1alpha1.PillarStore{Spec: pillarcsiv1alpha1.PillarStoreSpec{
				AgentRef: "target-a", Backend: tt.oldBackend,
			}}
			newStore := &pillarcsiv1alpha1.PillarStore{Spec: pillarcsiv1alpha1.PillarStoreSpec{
				AgentRef: "target-a", Backend: tt.newBackend,
			}}
			_, err := (&PillarStoreCustomValidator{}).ValidateUpdate(context.Background(), oldStore, newStore)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("ValidateUpdate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateUpdate() error = nil, want forbidden %s error", tt.wantPath)
			}
			errText := err.Error()
			if !strings.Contains(errText, tt.wantPath+":") {
				t.Fatalf("ValidateUpdate() error = %v, want %s path", err, tt.wantPath)
			}
			if !strings.Contains(errText, "Forbidden") {
				t.Fatalf("ValidateUpdate() error = %v, want field.Forbidden error", err)
			}
		})
	}
}
