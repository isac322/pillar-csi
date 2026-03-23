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

// BackendType enumerates supported storage backend drivers.
// +kubebuilder:validation:Enum=zfs-zvol;zfs-dataset;lvm-lv;dir
type BackendType string

const (
	BackendTypeZFSZvol    BackendType = "zfs-zvol"
	BackendTypeZFSDataset BackendType = "zfs-dataset"
	BackendTypeLVMLV      BackendType = "lvm-lv"
	BackendTypeDir        BackendType = "dir"
)

// ZFSBackendConfig holds ZFS-specific pool and dataset settings.
type ZFSBackendConfig struct {
	// pool is the ZFS pool name (e.g. "hot-data").
	// +required
	// +kubebuilder:validation:MinLength=1
	Pool string `json:"pool"`

	// parentDataset is the ZFS dataset path under which pillar-csi will
	// create per-volume datasets or zvols (e.g. "k8s").
	// +optional
	ParentDataset string `json:"parentDataset,omitempty"`

	// properties are arbitrary ZFS properties applied to every volume created
	// in this pool (e.g. compression, volblocksize).
	// +optional
	Properties map[string]string `json:"properties,omitempty"`
}

// BackendSpec describes the storage technology and its configuration.
// Exactly one backend config field must be set to match the chosen type.
type BackendSpec struct {
	// type identifies the backend driver.
	// +required
	Type BackendType `json:"type"`

	// zfs holds ZFS-specific configuration; required when type is zfs-zvol or zfs-dataset.
	// +optional
	ZFS *ZFSBackendConfig `json:"zfs,omitempty"`
}

// PoolCapacity reports the measured capacity of the pool.
type PoolCapacity struct {
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

// PillarPoolSpec defines the desired state of PillarPool.
type PillarPoolSpec struct {
	// targetRef is the name of the PillarTarget this pool lives on.
	// +required
	// +kubebuilder:validation:MinLength=1
	TargetRef string `json:"targetRef"`

	// backend describes the storage technology and pool to use.
	// +required
	Backend BackendSpec `json:"backend"`
}

// PillarPoolStatus defines the observed state of PillarPool.
type PillarPoolStatus struct {
	// capacity reflects the latest capacity reading for this pool.
	// +optional
	Capacity *PoolCapacity `json:"capacity,omitempty"`

	// conditions represent the current state of the PillarPool resource.
	//
	// Known condition types:
	// - "TargetReady"       – the referenced PillarTarget is in Ready state.
	// - "PoolDiscovered"    – the pool named in spec.backend has been found on the agent.
	// - "BackendSupported"  – the backend type is listed in the agent's capabilities.
	// - "Ready"             – all checks pass; the pool can provision volumes.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pp
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend.type`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.capacity.available`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarPool represents a specific storage pool on a PillarTarget.  Users
// create PillarPool resources to declare that a pool is available for CSI
// volume provisioning; the controller validates availability and updates status.
type PillarPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PillarPool.
	// +required
	Spec PillarPoolSpec `json:"spec"`

	// status defines the observed state of PillarPool.
	// +optional
	Status PillarPoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PillarPoolList contains a list of PillarPool.
type PillarPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PillarPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarPool{}, &PillarPoolList{})
}
