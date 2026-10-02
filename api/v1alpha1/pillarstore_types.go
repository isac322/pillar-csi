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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackendID identifies a storage backend kind.  It is the union member name
// used everywhere a backend can be selected — PillarStore.spec.backend,
// override documents, PVC annotations, agent config — and also the token
// embedded in CSI volume IDs and agent RPCs.
type BackendID string

// Supported BackendID values.
const (
	// BackendIDZFSZvol provisions ZFS zvols (block volumes).
	BackendIDZFSZvol BackendID = "zfs-zvol"

	// BackendIDZFSDataset provisions ZFS datasets (filesystem volumes).
	BackendIDZFSDataset BackendID = "zfs-dataset"

	// BackendIDLVMLV provisions LVM logical volumes (block volumes).
	BackendIDLVMLV BackendID = "lvm-lv"
)

// ZFSVolumeType enumerates the ZFS volume kinds the driver can create.
// +kubebuilder:validation:Enum=zvol;dataset
type ZFSVolumeType string

// Supported ZFSVolumeType values.
const (
	// ZFSVolumeTypeZvol creates block-device zvols.
	ZFSVolumeTypeZvol ZFSVolumeType = "zvol"

	// ZFSVolumeTypeDataset creates filesystem ZFS datasets.
	ZFSVolumeTypeDataset ZFSVolumeType = "dataset"
)

// ZFSBackendConfig holds ZFS-specific pool and dataset settings.
type ZFSBackendConfig struct {
	// volumeType selects the ZFS volume kind.
	// +optional
	// +kubebuilder:default=zvol
	VolumeType ZFSVolumeType `json:"volumeType,omitempty"`

	// pool is the ZFS pool name (e.g. "hot-data").
	// +required
	// +kubebuilder:validation:MinLength=1
	Pool string `json:"pool"`

	// parentDataset is the ZFS dataset path under which pillar-csi will
	// create per-volume zvols or datasets (e.g. "k8s").  It must equal the
	// parentDataset of the agent's backend configuration for this pool.
	// +optional
	ParentDataset string `json:"parentDataset,omitempty"`

	// properties are arbitrary ZFS properties applied to every volume created
	// in this pool (e.g. compression, volblocksize).
	// +optional
	Properties map[string]string `json:"properties,omitempty"`
}

// LVMProvisioningMode selects between linear and thin LV provisioning.
// +kubebuilder:validation:Enum=linear;thin
type LVMProvisioningMode string

// Supported LVMProvisioningMode values.
const (
	// LVMProvisioningModeLinear creates fully-allocated linear logical volumes
	// directly in the volume group (lvcreate -L <size>b).
	LVMProvisioningModeLinear LVMProvisioningMode = "linear"

	// LVMProvisioningModeThin creates thin-provisioned logical volumes inside a
	// pre-existing thin pool LV (lvcreate -V <size>b --thinpool <pool>).
	LVMProvisioningModeThin LVMProvisioningMode = "thin"
)

// LVMBackendConfig holds LVM-specific volume group and thin pool settings.
type LVMBackendConfig struct {
	// volumeGroup is the LVM Volume Group name that pillar-csi will create
	// logical volumes in (e.g. "data-vg").  The VG must be pre-created on
	// the node before the agent starts; the driver does not manage VG lifecycle.
	// +required
	// +kubebuilder:validation:MinLength=1
	VolumeGroup string `json:"volumeGroup"`

	// thinPool is the name of the LVM thin pool LV within the volume group
	// (e.g. "thin-pool-0").  When non-empty the backend operates in thin-
	// provisioned mode; when empty it creates fully-allocated linear LVs.
	// The thin pool must be pre-created before the agent starts, and must
	// equal the thinPool of the agent's backend configuration for this VG
	// (chart agent.backends[].lvm.thinPool; empty = none): on a mismatch the
	// store is not Ready (PoolDiscovered=False, BackendLayoutMismatch) and
	// CreateVolume fails instead of using another thin pool.
	// +optional
	ThinPool string `json:"thinPool,omitempty"`

	// provisioningMode controls whether new volumes are created as fully-
	// allocated linear LVs or as thin-provisioned LVs inside the thinPool.
	// Defaults to "linear"; set to "thin" together with a non-empty thinPool
	// to enable thin provisioning.
	// +optional
	// +kubebuilder:default=linear
	ProvisioningMode LVMProvisioningMode `json:"provisioningMode,omitempty"`
}

// BackendSpec describes the storage backend of a PillarStore.  Exactly one
// member must be set: the member name selects the backend (zfs or lvm) and
// its value carries that backend's configuration.
//
// The same union shape is reused for the agent's backend placement config
// file; per-volume and per-binding override documents use BackendOverrides,
// which keeps only the tunable subset.
//
// +kubebuilder:validation:XValidation:rule="(has(self.zfs) ? 1 : 0) + (has(self.lvm) ? 1 : 0) == 1",message="exactly one of zfs or lvm must be set"
type BackendSpec struct {
	// zfs holds ZFS-specific configuration.
	// +optional
	ZFS *ZFSBackendConfig `json:"zfs,omitempty"`

	// lvm holds LVM-specific configuration.
	// +optional
	LVM *LVMBackendConfig `json:"lvm,omitempty"`
}

// Kind returns the selected backend member as a BackendID, or "" when the
// union is empty.
func (b BackendSpec) Kind() BackendID {
	switch {
	case b.ZFS != nil && b.ZFS.VolumeType == ZFSVolumeTypeDataset:
		return BackendIDZFSDataset
	case b.ZFS != nil:
		return BackendIDZFSZvol
	case b.LVM != nil:
		return BackendIDLVMLV
	default:
		return ""
	}
}

// PoolName returns the physical pool identifier used in agent volume IDs:
// the ZFS pool name or the LVM volume group name.
func (b BackendSpec) PoolName() string {
	switch {
	case b.ZFS != nil:
		return b.ZFS.Pool
	case b.LVM != nil:
		return b.LVM.VolumeGroup
	default:
		return ""
	}
}

// StoreCapacity reports the measured capacity of the pool.
type StoreCapacity struct {
	// total is the gross capacity of the pool.
	// +optional
	Total *resource.Quantity `json:"total,omitempty"`

	// available is the free capacity available for new volumes.
	// +optional
	Available *resource.Quantity `json:"available,omitempty"`

	// used is the amount of capacity already consumed.
	// +optional
	Used *resource.Quantity `json:"used,omitempty"`
}

// PillarStoreSpec defines the desired state of PillarStore.
type PillarStoreSpec struct {
	// agentRef is the name of the PillarAgent this pool lives on.
	// +required
	// +kubebuilder:validation:MinLength=1
	AgentRef string `json:"agentRef"`

	// backend describes the storage backend and pool to use.  Exactly one
	// member (zfs or lvm) must be set.
	// +required
	Backend BackendSpec `json:"backend"`
}

// PillarStoreStatus defines the observed state of PillarStore.
type PillarStoreStatus struct {
	// capacity reflects the latest capacity reading for this pool.
	// +optional
	Capacity *StoreCapacity `json:"capacity,omitempty"`

	// conditions represent the current state of the PillarStore resource.
	//
	// Known condition types:
	// - "AgentReady"       – the referenced PillarAgent is in Ready state.
	// - "PoolDiscovered"    – the pool named in spec.backend has been found on the agent
	//                         and the agent creates volumes in it where the store declares
	//                         (zfs.parentDataset, lvm.thinPool); reason BackendLayoutMismatch
	//                         otherwise.
	// - "BackendSupported"  – the backend is listed in the agent's capabilities.
	// - "Ready"             – all checks pass; the pool can provision volumes.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pst
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.agentRef`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.capacity.available`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarStore represents a specific storage pool on a PillarAgent.  Users
// create PillarStore resources to declare that a pool is available for CSI
// volume provisioning; the controller validates availability and updates status.
type PillarStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PillarStoreSpec   `json:"spec,omitempty"`
	Status PillarStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PillarStoreList contains a list of PillarStore.
type PillarStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PillarStore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarStore{}, &PillarStoreList{})
}
