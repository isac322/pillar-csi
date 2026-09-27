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

// A volume belongs to a PillarStore only if it lives on the store's agent, in
// its backend type, and in its pool: the pool segment is what the StorageClass
// store parameter put into the agent volume ID.
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
