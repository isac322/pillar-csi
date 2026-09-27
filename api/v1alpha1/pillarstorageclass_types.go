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
//
// A StorageClass is immutable apart from allowVolumeExpansion, so a change to
// reclaimPolicy or volumeBindingMode makes the controller delete and re-create
// the StorageClass.  Tunable overrides do not feed the StorageClass parameters
// — they are resolved from live CRs at CreateVolume — so editing them never
// forces a StorageClass recreation.
type StorageClassTemplate struct {
	// name is the name of the generated StorageClass.
	// Defaults to the PillarStorageClass's own name when omitted.
	// Immutable: PVCs reference their StorageClass by name.
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

// FilesystemConfig describes the filesystem axis: which filesystem the CSI
// node creates on a new block volume and how it mounts it.  The same shape
// is used at every layer that may set it — PillarStorageClass.spec.filesystem,
// a hand-written StorageClass's pillar-csi.bhyoo.com/filesystem parameter and
// the pillar-csi.bhyoo.com/filesystem PVC annotation.
//
// List fields use list semantics on every layer: omitted (absent or null)
// inherits the value of the layer below, an explicit empty list [] clears it.
type FilesystemConfig struct {
	// fsType is the filesystem the node formats a new volume with when
	// volumeMode is Filesystem.
	// +optional
	// +kubebuilder:validation:Enum=ext4;xfs
	// +kubebuilder:default=ext4
	FSType string `json:"fsType,omitempty"`

	// mkfsOptions are additional mkfs arguments used when the node formats a
	// new volume; a volume that already carries a filesystem is never
	// reformatted.  Each element is one argv element (no shell); only
	// filesystem tuning flags of the formatted type are accepted.
	// A null/omitted value inherits the options of the layer below; an
	// explicit empty list [] clears them.
	// +optional
	MkfsOptions *[]string `json:"mkfsOptions,omitempty"`

	// mountOptions are the mount options the node applies when mounting a
	// Filesystem-mode volume.  On a PillarStorageClass they are written to
	// the generated Kubernetes StorageClass's mountOptions; a PVC annotation
	// value overrides them for that volume.
	// A null/omitted value inherits the options of the layer below; an
	// explicit empty list [] clears them.
	// +optional
	MountOptions *[]string `json:"mountOptions,omitempty"`
}

// ZFSBackendOverrides holds the per-volume-tunable subset of
// ZFSBackendConfig.  Structural placement fields (pool, parentDataset,
// volumeType) are not part of this type and are rejected with their path by
// the shared document decoder.
type ZFSBackendOverrides struct {
	// properties are arbitrary ZFS properties that override pool defaults
	// (e.g. volblocksize, compression).  Entries merge key-wise onto the
	// PillarStore's zfs.properties.
	// +optional
	Properties map[string]string `json:"properties,omitempty"`
}

// LVMBackendOverrides holds the per-volume-tunable subset of
// LVMBackendConfig.  Structural placement fields (volumeGroup, thinPool)
// are not part of this type and are rejected with their path by the shared
// document decoder.
type LVMBackendOverrides struct {
	// provisioningMode overrides the LVM provisioning mode for this volume or
	// binding: "linear" (fully-allocated LV) or "thin" (thin-provisioned LV
	// inside the backend's thin pool).  When omitted, the PillarStore-level
	// value is used.
	// +optional
	// +kubebuilder:validation:Enum=linear;thin
	ProvisioningMode LVMProvisioningMode `json:"provisioningMode,omitempty"`
}

// BackendOverrides is the per-binding or per-volume override document for the
// storage backend.  Exactly one member must be set, and it must match the
// backend member configured on the referenced PillarStore.
//
// +kubebuilder:validation:XValidation:rule="(has(self.zfs) ? 1 : 0) + (has(self.lvm) ? 1 : 0) == 1",message="exactly one of zfs or lvm must be set"
type BackendOverrides struct {
	// zfs overrides ZFS-specific tunables; valid only when the store's
	// backend is zfs.
	// +optional
	ZFS *ZFSBackendOverrides `json:"zfs,omitempty"`

	// lvm overrides LVM-specific tunables; valid only when the store's
	// backend is lvm.
	// +optional
	LVM *LVMBackendOverrides `json:"lvm,omitempty"`
}

// Kind returns the selected override member name ("zfs" or "lvm"), or "" when
// the union is empty.
func (b BackendOverrides) Kind() string {
	switch {
	case b.ZFS != nil:
		return "zfs"
	case b.LVM != nil:
		return "lvm"
	default:
		return ""
	}
}

// NVMeOFTCPOverrides holds the per-volume-tunable subset of NVMeOFTCPConfig.
// Structural fields (port — which listener the export lives on — and acl —
// the security policy anchor) are not part of this type and are rejected with
// their path by the shared document decoder.
type NVMeOFTCPOverrides struct {
	// maxQueueSize overrides the protocol-level maxQueueSize (the initiator's
	// fabrics queue_size; the kernel accepts 16-1024).
	// +optional
	// +kubebuilder:validation:Minimum=16
	// +kubebuilder:validation:Maximum=1024
	MaxQueueSize *int32 `json:"maxQueueSize,omitempty"`

	// inCapsuleDataSize overrides the protocol-level inCapsuleDataSize (the
	// target port's param_inline_data_size, shared by every volume exported
	// on the same storage node address and port; at least 1024).
	// +optional
	// +kubebuilder:validation:Minimum=1024
	InCapsuleDataSize *int32 `json:"inCapsuleDataSize,omitempty"`

	// ctrlLossTmo overrides the protocol-level ctrlLossTmo (seconds before
	// declaring a target permanently lost).
	// +optional
	// +kubebuilder:validation:Minimum=0
	CtrlLossTmo *int32 `json:"ctrlLossTmo,omitempty"`

	// reconnectDelay overrides the protocol-level reconnectDelay (seconds
	// between reconnect attempts).
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReconnectDelay *int32 `json:"reconnectDelay,omitempty"`
}

// ProtocolOverrides is the per-binding or per-volume override document for
// the transport protocol.  Exactly one member must be set, and it must match
// the protocol member configured on the referenced PillarProtocol.
//
// +kubebuilder:validation:XValidation:rule="has(self.nvmeofTcp)",message="exactly one protocol member must be set (supported: nvmeofTcp)"
type ProtocolOverrides struct {
	// nvmeofTcp overrides NVMe-oF/TCP tunables; valid only when the
	// protocol's member is nvmeofTcp.
	// +optional
	NVMeOFTCP *NVMeOFTCPOverrides `json:"nvmeofTcp,omitempty"`
}

// Kind returns the selected override member name ("nvmeofTcp"), or "" when
// the union is empty.
func (p ProtocolOverrides) Kind() string {
	if p.NVMeOFTCP != nil {
		return "nvmeofTcp"
	}
	return ""
}

// StorageClassOverrides is the optional layer of per-binding parameter
// overrides applied on top of the store and protocol defaults.
type StorageClassOverrides struct {
	// backend contains backend tunable overrides (same shape as the
	// pillar-csi.bhyoo.com/backend PVC annotation document).
	// +optional
	Backend *BackendOverrides `json:"backend,omitempty"`

	// protocol contains protocol tunable overrides (same shape as the
	// pillar-csi.bhyoo.com/protocol PVC annotation document).
	// +optional
	Protocol *ProtocolOverrides `json:"protocol,omitempty"`
}

// PillarStorageClassSpec defines the desired state of PillarStorageClass.
type PillarStorageClassSpec struct {
	// storeRef is the name of the PillarStore to use for provisioning.
	// +required
	// +kubebuilder:validation:MinLength=1
	StoreRef string `json:"storeRef"`

	// protocolRef is the name of the PillarProtocol used to expose volumes.
	// +required
	// +kubebuilder:validation:MinLength=1
	ProtocolRef string `json:"protocolRef"`

	// storageClass configures the Kubernetes StorageClass that this binding
	// generates.  The controller creates and owns the StorageClass; deleting
	// the PillarStorageClass also deletes the StorageClass.
	// +optional
	StorageClass StorageClassTemplate `json:"storageClass,omitempty"`

	// filesystem configures the filesystem axis for volumes of this binding:
	// which filesystem the node formats and which mount options it applies.
	// +optional
	Filesystem *FilesystemConfig `json:"filesystem,omitempty"`

	// overrides provides a fine-grained parameter layer on top of the
	// referenced store and protocol defaults.
	// +optional
	Overrides *StorageClassOverrides `json:"overrides,omitempty"`
}

// PillarStorageClassStatus defines the observed state of PillarStorageClass.
type PillarStorageClassStatus struct {
	// storageClassName is the name of the generated StorageClass.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// conditions represent the current state of the PillarStorageClass resource.
	//
	// Known condition types:
	// - "StoreReady"           – the referenced PillarStore is in Ready state.
	// - "ProtocolValid"       – the referenced PillarProtocol exists and is valid.
	// - "Compatible"          – the pool backend and protocol are compatible
	//                           (e.g. block backend cannot be combined with a file protocol).
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
// +kubebuilder:resource:scope=Cluster,shortName=psc
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.storeRef`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocolRef`
// +kubebuilder:printcolumn:name="StorageClass",type=string,JSONPath=`.status.storageClassName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarStorageClass combines a PillarStore and a PillarProtocol to create a
// Kubernetes StorageClass.  A validation webhook rejects incompatible
// backend/protocol combinations (e.g. a block backend with a file protocol).
// Parameter overrides allow fine-tuning per binding without changing the
// shared store or protocol resources.
type PillarStorageClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PillarStorageClassSpec   `json:"spec,omitempty"`
	Status PillarStorageClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PillarStorageClassList contains a list of PillarStorageClass.
type PillarStorageClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PillarStorageClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarStorageClass{}, &PillarStorageClassList{})
}
