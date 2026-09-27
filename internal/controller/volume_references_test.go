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
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

func pvWithHandle(driver, handle string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: handle},
			},
		},
	}
}

func TestVolumeRefFromPV(t *testing.T) {
	cases := []struct {
		name   string
		pv     *corev1.PersistentVolume
		want   volumeRef
		wantOK bool
	}{
		{
			name:   "pooled pillar-csi volume",
			pv:     pvWithHandle(pillarCSIProvisioner, "node-a/nvmeof-tcp/zfs-zvol/tank/pvc-1"),
			want:   volumeRef{agent: "node-a", protocol: "nvmeof-tcp", backend: "zfs-zvol", pool: "tank"},
			wantOK: true,
		},
		{
			name:   "volume without a pool segment",
			pv:     pvWithHandle(pillarCSIProvisioner, "node-a/nfs/dir/pvc-1"),
			want:   volumeRef{agent: "node-a", protocol: "nfs", backend: "dir"},
			wantOK: true,
		},
		{
			name: "volume of another driver with a look-alike handle",
			pv:   pvWithHandle("other.csi.example.com", "node-a/nvmeof-tcp/zfs-zvol/tank/pvc-1"),
		},
		{
			name: "malformed handle",
			pv:   pvWithHandle(pillarCSIProvisioner, "node-a/nvmeof-tcp"),
		},
		{
			name: "non-CSI volume",
			pv:   &corev1.PersistentVolume{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := volumeRefFromPV(tc.pv)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("volumeRefFromPV() = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// inStore is the location fallback: a volume lies in a PillarStore's pool when
// it is on the store's agent, of its backend type, and in its pool — the pool
// segment being what the StorageClass store parameter put into the agent
// volume ID.  Ownership between sibling stores on one pool is decided by
// volumeBlocksStore, not by inStore.
func TestVolumeRefInStore(t *testing.T) {
	zfsStore := &pillarcsiv1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "zfs-store"},
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "node-a",
			Backend: pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
			},
		},
	}
	lvmStore := &pillarcsiv1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-store"},
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "node-a",
			Backend: pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeLVMLV,
				LVM:  &pillarcsiv1alpha1.LVMBackendConfig{VolumeGroup: "vg0"},
			},
		},
	}
	dirStore := &pillarcsiv1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "dir-store"},
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "node-a",
			Backend:  pillarcsiv1alpha1.BackendSpec{Type: pillarcsiv1alpha1.BackendTypeDir},
		},
	}

	cases := []struct {
		name  string
		ref   volumeRef
		store *pillarcsiv1alpha1.PillarStore
		want  bool
	}{
		{"zfs volume in the pool", volumeRef{agent: "node-a", backend: "zfs-zvol", pool: "tank"}, zfsStore, true},
		{"zfs volume in another pool of the agent", volumeRef{agent: "node-a", backend: "zfs-zvol", pool: "tank2"}, zfsStore, false},
		{"zfs volume on another agent", volumeRef{agent: "node-b", backend: "zfs-zvol", pool: "tank"}, zfsStore, false},
		{"lvm volume in the volume group", volumeRef{agent: "node-a", backend: "lvm-lv", pool: "vg0"}, lvmStore, true},
		{"volume of another backend in a same-named pool", volumeRef{agent: "node-a", backend: "zfs-zvol", pool: "vg0"}, lvmStore, false},
		{"dir volume keyed by the store name", volumeRef{agent: "node-a", backend: "dir", pool: "dir-store"}, dirStore, true},
		{"dir volume of another store", volumeRef{agent: "node-a", backend: "dir", pool: "other-store"}, dirStore, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.inStore(tc.store); got != tc.want {
				t.Errorf("%+v.inStore(%s) = %v, want %v", tc.ref, tc.store.Name, got, tc.want)
			}
		})
	}
}

// storageClassStores maps each generated StorageClass name to the store of its
// PillarStorageClass.  Provenance must be confirmed: a live class needs a
// controller owner reference to a binding, and a binding's
// status.storageClassName attributes only an absent class.  A requested spec
// name without either never maps — a binding can fail before generating its
// class, and the name may belong to a hand-written class on another store.
// Colliding claims for an absent class drop out so volumeBlocksStore fails
// closed on the location match instead of picking one store.
func TestStorageClassStores(t *testing.T) {
	owner := func(name string) *metav1.OwnerReference {
		controller := true
		return &metav1.OwnerReference{
			APIVersion: pillarcsiv1alpha1.GroupVersion.String(),
			Kind:       "PillarStorageClass",
			Name:       name,
			Controller: &controller,
		}
	}
	binding := func(name, scName, storeRef string) pillarcsiv1alpha1.PillarStorageClass {
		b := pillarcsiv1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef:    storeRef,
				ProtocolRef: "proto",
			},
		}
		if scName != "" {
			b.Spec.StorageClass.Name = scName
		}
		return b
	}
	class := func(name string, owner *metav1.OwnerReference) storagev1.StorageClass {
		sc := storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if owner != nil {
			sc.OwnerReferences = []metav1.OwnerReference{*owner}
		}
		return sc
	}

	bindings := &pillarcsiv1alpha1.PillarStorageClassList{Items: []pillarcsiv1alpha1.PillarStorageClass{
		binding("bind-a", "", "store-a"),
		binding("bind-b", "legacy", "store-b"), // requests a hand-written class
		binding("bind-c", "", "store-c"),
		binding("bind-d", "", "store-d"),
		binding("bind-e", "", "store-e"),
		binding("bind-f", "", "store-f"),
	}}
	// bind-c and bind-d recorded the same class name: ambiguous, drop it.
	bindings.Items[2].Status.StorageClassName = "gone-sc"
	bindings.Items[3].Status.StorageClassName = "gone-sc"
	// bind-e recorded a class that is absent: evidence of an earlier generate.
	bindings.Items[4].Status.StorageClassName = "deleted-sc"
	// bind-f records a name that is a live hand-written class; the record is
	// stale and must not attribute "legacy" to store-f.
	bindings.Items[5].Status.StorageClassName = "legacy"

	classes := &storagev1.StorageClassList{Items: []storagev1.StorageClass{
		class("bind-a", owner("bind-a")), // generated, live
		class("legacy", nil),             // hand-written: never attributed
		class("bind-c", owner("bind-c")), // live ownerRef beats stale record
	}}

	got := storageClassStores(bindings, classes)
	want := map[string]string{
		"bind-a":     "store-a",
		"bind-c":     "store-c",
		"deleted-sc": "store-e",
	}
	for name, store := range want {
		if got[name] != store {
			t.Errorf("scStores[%q] = %q, want %q", name, got[name], store)
		}
	}
	for _, name := range []string{"legacy", "gone-sc"} {
		if _, ok := got[name]; ok {
			t.Errorf("scStores[%q] must not exist; the name is unattributable", name)
		}
	}
}

// volumeBlocksStore: an attributed volume blocks only its own store; an
// unattributable one fails closed on inStore.
func TestVolumeBlocksStore(t *testing.T) {
	store := &pillarcsiv1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "store-a"},
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "node-a",
			Backend: pillarcsiv1alpha1.BackendSpec{
				Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
				ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
			},
		},
	}
	ref := volumeRef{agent: "node-a", backend: "zfs-zvol", pool: "tank"}
	scStores := map[string]string{"sc-of-a": "store-a", "sc-of-b": "store-b"}

	cases := []struct {
		name   string
		scName string
		want   bool
	}{
		{"volume of the store's class", "sc-of-a", true},
		{"volume of a sibling store's class on the same pool", "sc-of-b", false},
		{"unattributable class fails closed on the pool", "unknown-class", true},
		{"no class fails closed on the pool", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumeBlocksStore(tc.scName, ref, store, scStores); got != tc.want {
				t.Errorf("volumeBlocksStore(%q, %+v, %s) = %v, want %v", tc.scName, ref, store.Name, got, tc.want)
			}
		})
	}
}
