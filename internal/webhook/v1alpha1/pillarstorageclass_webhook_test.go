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
	"context"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

func newZFSStore(name string) *pillarcsiv1alpha1.PillarStore {
	return &pillarcsiv1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "test-target",
			Backend:  pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"}},
		},
	}
}

func newLVMStore(name string) *pillarcsiv1alpha1.PillarStore {
	return &pillarcsiv1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "test-target",
			Backend:  pillarcsiv1alpha1.BackendSpec{LVM: &pillarcsiv1alpha1.LVMBackendConfig{VolumeGroup: "data-vg"}},
		},
	}
}

func newNVMeProtocol(name string) *pillarcsiv1alpha1.PillarProtocol {
	return &pillarcsiv1alpha1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarcsiv1alpha1.PillarProtocolSpec{
			Protocol: pillarcsiv1alpha1.ProtocolSpec{NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420}},
		},
	}
}

var _ = Describe("PillarStorageClass Webhook", func() {
	var (
		obj       *pillarcsiv1alpha1.PillarStorageClass
		oldObj    *pillarcsiv1alpha1.PillarStorageClass
		validator PillarStorageClassCustomValidator
		defaulter PillarStorageClassCustomDefaulter
	)

	createStore := func(store *pillarcsiv1alpha1.PillarStore) {
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, store))).To(Succeed())
		})
	}
	createProtocol := func(proto *pillarcsiv1alpha1.PillarProtocol) {
		Expect(k8sClient.Create(ctx, proto)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, proto))).To(Succeed())
		})
	}

	BeforeEach(func() {
		obj = &pillarcsiv1alpha1.PillarStorageClass{}
		oldObj = &pillarcsiv1alpha1.PillarStorageClass{}
		validator = PillarStorageClassCustomValidator{Client: k8sClient}
		defaulter = PillarStorageClassCustomDefaulter{Client: k8sClient}
	})

	Context("When creating PillarStorageClass under Defaulting Webhook", func() {
		It("Should set allowVolumeExpansion=true when the store backend is zfs", func() {
			createStore(newZFSStore("test-pool-zvol"))

			obj.Name = "test-binding-zvol"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "test-pool-zvol", ProtocolRef: "test-protocol"}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeTrue())
		})

		It("Should set allowVolumeExpansion=true when the store backend is lvm", func() {
			createStore(newLVMStore("test-pool-lvm"))

			obj.Name = "test-binding-lvm"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "test-pool-lvm", ProtocolRef: "test-protocol"}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeTrue())
		})

		It("Should not override allowVolumeExpansion when already explicitly set", func() {
			createStore(newZFSStore("test-pool-override"))

			falseVal := false
			obj.Name = "test-binding-override"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef:     "test-pool-override",
				ProtocolRef:  "test-protocol",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{AllowVolumeExpansion: &falseVal},
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*obj.Spec.StorageClass.AllowVolumeExpansion).To(BeFalse())
		})

		It("Should leave allowVolumeExpansion unset when the store does not exist", func() {
			obj.Name = "test-binding-nopool"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "nonexistent-pool", ProtocolRef: "test-protocol"}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())

			Expect(obj.Spec.StorageClass.AllowVolumeExpansion).To(BeNil())
		})
	})

	Context("When creating or updating PillarStorageClass under Validating Webhook", func() {
		It("Should admit creation with all required fields present", func() {
			obj.Name = "test-binding-valid"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "some-pool", ProtocolRef: "some-protocol"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny update when storeRef is changed", func() {
			oldObj.Name = "test-binding-immutable"
			oldObj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "pool-a", ProtocolRef: "proto-a"}
			obj.Name = "test-binding-immutable"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "pool-b", ProtocolRef: "proto-a"}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("storeRef"))
		})

		It("Should deny update when protocolRef is changed", func() {
			oldObj.Name = "test-binding-immutable2"
			oldObj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "pool-a", ProtocolRef: "proto-a"}
			obj.Name = "test-binding-immutable2"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "pool-a", ProtocolRef: "proto-b"}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("protocolRef"))
		})

		It("Should deny update when the generated StorageClass name changes", func() {
			oldObj.Name = "test-binding-scname"
			oldObj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "pool-a", ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{Name: "fast"},
			}
			obj.Name = "test-binding-scname"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "pool-a", ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{Name: "faster"},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.storageClass.name"))
		})

		It("Should deny setting storageClass.name when it renames the defaulted StorageClass", func() {
			oldObj.Name = "test-binding-scname-default"
			oldObj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "pool-a", ProtocolRef: "proto-a"}
			obj.Name = "test-binding-scname-default"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "pool-a", ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{Name: "renamed"},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.storageClass.name"))
		})

		It("Should admit spelling out the defaulted StorageClass name", func() {
			oldObj.Name = "test-binding-scname-explicit"
			oldObj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "pool-a", ProtocolRef: "proto-a"}
			obj.Name = "test-binding-scname-explicit"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "pool-a", ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{Name: "test-binding-scname-explicit"},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit update when only non-immutable fields are changed", func() {
			oldObj.Name = "test-binding-mutable"
			oldObj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "pool-a", ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{ReclaimPolicy: pillarcsiv1alpha1.ReclaimPolicyDelete},
			}
			obj.Name = "test-binding-mutable"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "pool-a", ProtocolRef: "proto-a",
				StorageClass: pillarcsiv1alpha1.StorageClassTemplate{ReclaimPolicy: pillarcsiv1alpha1.ReclaimPolicyRetain},
				Filesystem:   &pillarcsiv1alpha1.FilesystemConfig{FSType: "xfs"},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When validating Backend-Protocol compatibility and override members", func() {
		It("Should admit zfs backend with nvmeofTcp protocol", func() {
			createStore(newZFSStore("compat-pool-zvol-nvme"))
			createProtocol(newNVMeProtocol("compat-proto-nvme"))

			obj.Name = "compat-binding-zvol-nvme"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "compat-pool-zvol-nvme", ProtocolRef: "compat-proto-nvme",
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit lvm backend with nvmeofTcp protocol and matching overrides", func() {
			createStore(newLVMStore("compat-pool-lvm-nvme"))
			createProtocol(newNVMeProtocol("compat-proto-nvme-lvm"))

			obj.Name = "compat-binding-lvm-nvme"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "compat-pool-lvm-nvme", ProtocolRef: "compat-proto-nvme-lvm",
				Overrides: &pillarcsiv1alpha1.StorageClassOverrides{
					Backend: &pillarcsiv1alpha1.BackendOverrides{LVM: &pillarcsiv1alpha1.LVMBackendOverrides{
						ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeThin,
					}},
					Protocol: &pillarcsiv1alpha1.ProtocolOverrides{NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPOverrides{}},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny a zfs backend override on an lvm store", func() {
			createStore(newLVMStore("mismatch-pool-lvm"))
			createProtocol(newNVMeProtocol("mismatch-proto-nvme"))

			obj.Name = "mismatch-binding"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: "mismatch-pool-lvm", ProtocolRef: "mismatch-proto-nvme",
				Overrides: &pillarcsiv1alpha1.StorageClassOverrides{
					Backend: &pillarcsiv1alpha1.BackendOverrides{ZFS: &pillarcsiv1alpha1.ZFSBackendOverrides{
						Properties: map[string]string{"compression": "lz4"},
					}},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.overrides.backend"))

			By("rejecting the same mismatch on update")
			_, err = validator.ValidateUpdate(ctx, obj.DeepCopy(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.overrides.backend"))
		})

		It("Should admit creation when the store does not exist (defer to controller)", func() {
			createProtocol(newNVMeProtocol("compat-proto-nopool"))

			obj.Name = "compat-binding-nopool"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "nonexistent-pool", ProtocolRef: "compat-proto-nopool"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit creation when the protocol does not exist (defer to controller)", func() {
			createStore(newZFSStore("compat-pool-noproto"))

			obj.Name = "compat-binding-noproto"
			obj.Spec = pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "compat-pool-noproto", ProtocolRef: "nonexistent-protocol"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// TestPillarStorageClass_OverrideMemberMatch exercises the override member
// checks against a fake client (no envtest needed).
func TestPillarStorageClass_OverrideMemberMatch(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := pillarcsiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		newZFSStore("zfs-store"), newLVMStore("lvm-store"), newNVMeProtocol("nvme"),
	).Build()
	validator := &PillarStorageClassCustomValidator{Client: c}

	zfsOverride := &pillarcsiv1alpha1.BackendOverrides{ZFS: &pillarcsiv1alpha1.ZFSBackendOverrides{
		Properties: map[string]string{"volblocksize": "16K"},
	}}
	lvmOverride := &pillarcsiv1alpha1.BackendOverrides{LVM: &pillarcsiv1alpha1.LVMBackendOverrides{
		ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeLinear,
	}}
	nvmeOverride := &pillarcsiv1alpha1.ProtocolOverrides{NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPOverrides{}}

	tests := []struct {
		name      string
		store     string
		overrides *pillarcsiv1alpha1.StorageClassOverrides
		wantPaths []string // empty = admitted
	}{
		{name: "no overrides", store: "zfs-store"},
		{name: "zfs override on zfs store", store: "zfs-store",
			overrides: &pillarcsiv1alpha1.StorageClassOverrides{Backend: zfsOverride, Protocol: nvmeOverride}},
		{name: "lvm override on lvm store", store: "lvm-store",
			overrides: &pillarcsiv1alpha1.StorageClassOverrides{Backend: lvmOverride}},
		{name: "lvm override on zfs store", store: "zfs-store",
			overrides: &pillarcsiv1alpha1.StorageClassOverrides{Backend: lvmOverride},
			wantPaths: []string{"spec.overrides.backend"}},
		{name: "zfs override on lvm store", store: "lvm-store",
			overrides: &pillarcsiv1alpha1.StorageClassOverrides{Backend: zfsOverride},
			wantPaths: []string{"spec.overrides.backend"}},
		{name: "empty backend and protocol override unions", store: "zfs-store",
			overrides: &pillarcsiv1alpha1.StorageClassOverrides{
				Backend: &pillarcsiv1alpha1.BackendOverrides{}, Protocol: &pillarcsiv1alpha1.ProtocolOverrides{},
			},
			wantPaths: []string{"spec.overrides.backend", "spec.overrides.protocol"}},
		{name: "missing store skips backend check", store: "missing",
			overrides: &pillarcsiv1alpha1.StorageClassOverrides{Backend: lvmOverride}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			psc := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "psc"},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef: tt.store, ProtocolRef: "nvme", Overrides: tt.overrides,
				},
			}
			_, err := validator.ValidateCreate(context.Background(), psc)
			if len(tt.wantPaths) == 0 {
				if err != nil {
					t.Fatalf("ValidateCreate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCreate() error = nil, want errors on %v", tt.wantPaths)
			}
			for _, p := range tt.wantPaths {
				if !strings.Contains(err.Error(), p+":") {
					t.Fatalf("ValidateCreate() error = %v, want path %s", err, p)
				}
			}
		})
	}
}
func TestPillarStorageClass_DriverAndDirectoryValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := pillarcsiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	store := &pillarcsiv1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "directory-store"},
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "agent",
			Backend: pillarcsiv1alpha1.BackendSpec{Directory: &pillarcsiv1alpha1.DirectoryBackendConfig{
				LogicalPool: "files", HostRoot: "/srv/files",
			}},
		},
	}
	protocol := &pillarcsiv1alpha1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-protocol"},
		Spec: pillarcsiv1alpha1.PillarProtocolSpec{
			Protocol: pillarcsiv1alpha1.ProtocolSpec{NFS: &pillarcsiv1alpha1.NFSConfig{}},
		},
	}
	validator := &PillarStorageClassCustomValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(store, protocol, newNVMeProtocol("nvme")).Build(),
	}

	newBinding := func(driver string) *pillarcsiv1alpha1.PillarStorageClass {
		return &pillarcsiv1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "directory-binding"},
			Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
				CSIDriver: driver, StoreRef: store.Name, ProtocolRef: protocol.Name,
			},
		}
	}

	if _, err := validator.ValidateCreate(context.Background(), newBinding("")); err == nil {
		t.Fatal("legacy driver directory binding was admitted, want explicit file driver error")
	}
	if _, err := validator.ValidateCreate(context.Background(), newBinding(pillarcsiv1alpha1.FileCSIDriver)); err != nil {
		t.Fatalf("file driver directory binding rejected: %v", err)
	}
	local := newBinding(pillarcsiv1alpha1.FileCSIDriver)
	local.Spec.LocalAttach = true
	if _, err := validator.ValidateCreate(context.Background(), local); err != nil {
		t.Fatalf("native local-only files class rejected: %v", err)
	}
	blockProtocol := newBinding(pillarcsiv1alpha1.FileCSIDriver)
	blockProtocol.Spec.ProtocolRef = "nvme"
	if _, err := validator.ValidateCreate(context.Background(), blockProtocol); err == nil {
		t.Fatal("directory class with block protocol was admitted")
	}

	// Omitted and explicit legacy driver spellings are the same immutable
	// selection; the webhook must not reject normal defaulting on an update.
	legacy := newBinding("")
	explicitLegacy := legacy.DeepCopy()
	explicitLegacy.Spec.CSIDriver = pillarcsiv1alpha1.DefaultCSIDriver
	noClientValidator := &PillarStorageClassCustomValidator{}
	if _, err := noClientValidator.ValidateUpdate(context.Background(), legacy, explicitLegacy); err != nil {
		t.Fatalf("explicit unchanged legacy driver rejected: %v", err)
	}

	expansion := true
	fileWithExpansion := newBinding(pillarcsiv1alpha1.FileCSIDriver)
	fileWithExpansion.Spec.StorageClass.AllowVolumeExpansion = &expansion
	if _, err := validator.ValidateCreate(context.Background(), fileWithExpansion); err == nil {
		t.Fatal("file driver allowVolumeExpansion=true was admitted")
	}

	oldBinding := newBinding("")
	newBindingWithFile := newBinding(pillarcsiv1alpha1.FileCSIDriver)
	if _, err := validator.ValidateUpdate(context.Background(), oldBinding, newBindingWithFile); err == nil {
		t.Fatal("CSI driver update was admitted, want immutable field error")
	}
}

// TestPillarStorageClass_DefaultAllowVolumeExpansion checks that every served
// backend member defaults allowVolumeExpansion to true.
func TestPillarStorageClass_DefaultAllowVolumeExpansion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := pillarcsiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		newZFSStore("zfs-store"), newLVMStore("lvm-store"),
	).Build()
	defaulter := &PillarStorageClassCustomDefaulter{Client: c}

	for _, store := range []string{"zfs-store", "lvm-store"} {
		t.Run(store, func(t *testing.T) {
			psc := &pillarcsiv1alpha1.PillarStorageClass{Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: store, ProtocolRef: "nvme",
			}}
			if err := defaulter.Default(context.Background(), psc); err != nil {
				t.Fatalf("Default() error = %v", err)
			}
			got := psc.Spec.StorageClass.AllowVolumeExpansion
			if got == nil || !*got {
				t.Fatalf("allowVolumeExpansion = %v, want true", got)
			}
		})
	}
}
