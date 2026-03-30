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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReclaimPolicy mirrors corev1.PersistentVolumeReclaimPolicy for inline use.
// +kubebuilder:validation:Enum=Delete;Retain
type ReclaimPolicy string

// Supported ReclaimPolicy values.
const (
	ReclaimPolicyDelete ReclaimPolicy = "Delete"
	ReclaimPolicyRetain ReclaimPolicy = "Retain"
)

// VolumeBindingMode mirrors storagev1.VolumeBindingMode for inline use.
// +kubebuilder:validation:Enum=Immediate;WaitForFirstConsumer
type VolumeBindingMode string

// Supported VolumeBindingMode values.
const (
	VolumeBindingImmediate            VolumeBindingMode = "Immediate"
	VolumeBindingWaitForFirstConsumer VolumeBindingMode = "WaitForFirstConsumer"
)

// StorageClassTemplate defines the parameters used to generate a Kubernetes
// StorageClass from this binding.
type StorageClassTemplate struct {
	// name is the name of the generated StorageClass.
	// Defaults to the PillarBinding's own name when omitted.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name,omitempty"`

	// reclaimPolicy determines what happens to a PersistentVolume when its
	// PersistentVolumeClaim is deleted.
	// +optional
	// +kubebuilder:default=Delete
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// volumeBindingMode controls when volume binding and dynamic provisioning occur.
	// +optional
	// +kubebuilder:default=Immediate
	VolumeBindingMode VolumeBindingMode `json:"volumeBindingMode,omitempty"`

	// allowVolumeExpansion enables online volume expansion.
	// When unset the controller derives the value from backend capabilities.
	// +optional
	AllowVolumeExpansion *bool `json:"allowVolumeExpansion,omitempty"`
}

// ZFSPropertyOverrides are ZFS dataset/zvol property overrides applied on top
// of the pool-level defaults.
type ZFSPropertyOverrides struct {
	// properties are arbitrary ZFS properties that override pool defaults
	// (e.g. volblocksize, compression).
	// +optional
	Properties map[string]string `json:"properties,omitempty"`
}

// LVMOverrides holds per-binding overrides for LVM backend configuration.
// These settings override the PillarPool-level LVM defaults for volumes
// created through this specific binding.
type LVMOverrides struct {
	// provisioningMode overrides the LVM provisioning mode for this binding.
	// Accepted values: "linear" (fully-allocated LV) or "thin"
	// (thin-provisioned LV inside the backend's thin pool).
	// When omitted, the PillarPool-level default is used.
	// +optional
	// +kubebuilder:validation:Enum=linear;thin
	ProvisioningMode LVMProvisioningMode `json:"provisioningMode,omitempty"`
}

// BackendOverrides holds per-binding overrides for backend configuration.
// Only the field matching the pool's backend type is used.
type BackendOverrides struct {
	// zfs overrides ZFS-specific properties; used when the pool backend is
	// zfs-zvol or zfs-dataset.
	// +optional
	ZFS *ZFSPropertyOverrides `json:"zfs,omitempty"`

	// lvm overrides LVM-specific parameters; used when the pool backend is lvm-lv.
	// +optional
	LVM *LVMOverrides `json:"lvm,omitempty"`
}

// NVMeOFTCPOverrides holds per-binding NVMe-oF/TCP parameter overrides.
type NVMeOFTCPOverrides struct {
	// maxQueueSize overrides the protocol-level maxQueueSize.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxQueueSize *int32 `json:"maxQueueSize,omitempty"`

	// inCapsuleDataSize overrides the protocol-level inCapsuleDataSize.
	// +optional
	// +kubebuilder:validation:Minimum=0
	InCapsuleDataSize *int32 `json:"inCapsuleDataSize,omitempty"`
}

// ISCSIOverrides holds per-binding iSCSI parameter overrides.
type ISCSIOverrides struct {
	// loginTimeout overrides the protocol-level loginTimeout.
	// +optional
	// +kubebuilder:validation:Minimum=0
	LoginTimeout *int32 `json:"loginTimeout,omitempty"`

	// replacementTimeout overrides the protocol-level replacementTimeout.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReplacementTimeout *int32 `json:"replacementTimeout,omitempty"`
}

// ProtocolOverrides holds per-binding overrides for protocol parameters.
// Only the field matching the protocol type is consulted.
type ProtocolOverrides struct {
	// nvmeofTcp overrides NVMe-oF/TCP parameters.
	// +optional
	NVMeOFTCP *NVMeOFTCPOverrides `json:"nvmeofTcp,omitempty"`

	// iscsi overrides iSCSI parameters.
	// +optional
	ISCSI *ISCSIOverrides `json:"iscsi,omitempty"`
}

// BindingOverrides is the optional layer of per-binding parameter overrides
// applied on top of pool and protocol defaults.
type BindingOverrides struct {
	// backend contains backend-specific parameter overrides.
	// +optional
	Backend *BackendOverrides `json:"backend,omitempty"`

	// protocol contains protocol-specific parameter overrides.
	// +optional
	Protocol *ProtocolOverrides `json:"protocol,omitempty"`

	// fsType overrides the protocol-level fsType for this binding.
	// Only relevant for block protocols with volumeMode: Filesystem.
	// +optional
	// +kubebuilder:validation:Enum=ext4;xfs
	FSType string `json:"fsType,omitempty"`

	// mkfsOptions overrides the protocol-level mkfsOptions for this binding.
	// +optional
	MkfsOptions []string `json:"mkfsOptions,omitempty"`
}

// PillarBindingSpec defines the desired state of PillarBinding.
type PillarBindingSpec struct {
	// poolRef is the name of the PillarPool to use for provisioning.
	// +required
	// +kubebuilder:validation:MinLength=1
	PoolRef string `json:"poolRef"`

	// protocolRef is the name of the PillarProtocol used to expose volumes.
	// +required
	// +kubebuilder:validation:MinLength=1
	ProtocolRef string `json:"protocolRef"`

	// storageClass configures the Kubernetes StorageClass that this binding
	// generates.  The controller creates and owns the StorageClass; deleting
	// the PillarBinding also deletes the StorageClass.
	// +optional
	StorageClass StorageClassTemplate `json:"storageClass,omitempty"`

	// overrides provides a fine-grained parameter layer on top of the
	// referenced pool and protocol defaults.
	// +optional
	Overrides *BindingOverrides `json:"overrides,omitempty"`
}

// PillarBindingStatus defines the observed state of PillarBinding.
type PillarBindingStatus struct {
	// storageClassName is the name of the generated StorageClass.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// conditions represent the current state of the PillarBinding resource.
	//
	// Known condition types:
	// - "PoolReady"           – the referenced PillarPool is in Ready state.
	// - "ProtocolValid"       – the referenced PillarProtocol exists and is valid.
	// - "Compatible"          – the pool backend and protocol are compatible
	//                           (e.g. block backend cannot be combined with NFS).
	// - "StorageClassCreated" – the Kubernetes StorageClass has been created.
	// - "Ready"               – all checks pass; the binding is operational.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pb
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocolRef`
// +kubebuilder:printcolumn:name="StorageClass",type=string,JSONPath=`.status.storageClassName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarBinding combines a PillarPool and a PillarProtocol to create a
// Kubernetes StorageClass.  A validation webhook rejects incompatible
// backend/protocol combinations (e.g. block backend with NFS).
// Parameter overrides allow fine-tuning per binding without changing the
// shared pool or protocol resources.
type PillarBinding struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PillarBinding.
	// +required
	Spec PillarBindingSpec `json:"spec"`

	// status reflects the reconciler-observed state of this binding.
	// +optional
	Status PillarBindingStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PillarBindingList contains a list of PillarBinding.
type PillarBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PillarBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarBinding{}, &PillarBindingList{})
}
