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

// ProtocolType enumerates supported network storage protocols.
// +kubebuilder:validation:Enum=nvmeof-tcp;iscsi;nfs
type ProtocolType string

// Supported ProtocolType values.
const (
	ProtocolTypeNVMeOFTCP ProtocolType = "nvmeof-tcp"
	ProtocolTypeISCSI     ProtocolType = "iscsi"
	ProtocolTypeNFS       ProtocolType = "nfs"
)

// NVMeOFTCPConfig holds NVMe-oF/TCP-specific protocol parameters.
// Target bind IP is not included here — the controller resolves it
// at runtime from the referenced PillarTarget.
type NVMeOFTCPConfig struct {
	// port is the TCP port on which the NVMe-oF target listens.
	// Defaults to 4420.
	// +optional
	// +kubebuilder:default=4420
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// acl enables host NQN-based access control when true.
	// When false the subsystem uses allow_any_host.
	// +optional
	// +kubebuilder:default=true
	ACL bool `json:"acl,omitempty"`

	// maxQueueSize is the maximum number of I/O queue entries per connection.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxQueueSize *int32 `json:"maxQueueSize,omitempty"`

	// inCapsuleDataSize is the maximum in-capsule data size in bytes.
	// +optional
	// +kubebuilder:validation:Minimum=0
	InCapsuleDataSize *int32 `json:"inCapsuleDataSize,omitempty"`

	// ctrlLossTmo is the maximum seconds to wait before declaring a target
	// permanently lost after connectivity failure.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CtrlLossTmo *int32 `json:"ctrlLossTmo,omitempty"`

	// reconnectDelay is the interval in seconds between reconnect attempts.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReconnectDelay *int32 `json:"reconnectDelay,omitempty"`
}

// ISCSIConfig holds iSCSI-specific protocol parameters.
type ISCSIConfig struct {
	// port is the TCP port on which the iSCSI target listens.
	// Defaults to 3260.
	// +optional
	// +kubebuilder:default=3260
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// acl enables initiator IQN-based access control when true.
	// When false the target allows any initiator.
	// +optional
	// +kubebuilder:default=true
	ACL bool `json:"acl,omitempty"`

	// loginTimeout is the number of seconds to wait for a login response.
	// Defaults to 15.
	// +optional
	// +kubebuilder:validation:Minimum=0
	LoginTimeout *int32 `json:"loginTimeout,omitempty"`

	// replacementTimeout is the number of seconds to wait for a session
	// replacement after a connection failure. Defaults to 120.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReplacementTimeout *int32 `json:"replacementTimeout,omitempty"`

	// nodeSessionTimeout is the number of seconds for the node session
	// retry timeout. Defaults to 120.
	// +optional
	// +kubebuilder:validation:Minimum=0
	NodeSessionTimeout *int32 `json:"nodeSessionTimeout,omitempty"`
}

// NFSConfig holds NFS-specific protocol parameters.
type NFSConfig struct {
	// version is the NFS protocol version to use (e.g. "4.2").
	// +optional
	// +kubebuilder:default="4.2"
	Version string `json:"version,omitempty"`
}

// PillarProtocolSpec defines the desired state of PillarProtocol.
// Exactly one protocol config field must be set, matching the chosen type.
type PillarProtocolSpec struct {
	// type identifies the network storage protocol.
	// +required
	Type ProtocolType `json:"type"`

	// nvmeofTcp holds NVMe-oF/TCP configuration; required when type is nvmeof-tcp.
	// +optional
	NVMeOFTCP *NVMeOFTCPConfig `json:"nvmeofTcp,omitempty"`

	// iscsi holds iSCSI configuration; required when type is iscsi.
	// +optional
	ISCSI *ISCSIConfig `json:"iscsi,omitempty"`

	// nfs holds NFS configuration; required when type is nfs.
	// +optional
	NFS *NFSConfig `json:"nfs,omitempty"`

	// fsType is the default filesystem type for block protocols when
	// volumeMode is Filesystem.  Only relevant for block-based protocols
	// (nvmeof-tcp, iscsi).
	// +optional
	// +kubebuilder:validation:Enum=ext4;xfs
	// +kubebuilder:default=ext4
	FSType string `json:"fsType,omitempty"`

	// mkfsOptions are additional arguments passed to mkfs when formatting a
	// new volume.  Only relevant for block-based protocols.
	// +optional
	MkfsOptions []string `json:"mkfsOptions,omitempty"`
}

// PillarProtocolStatus defines the observed state of PillarProtocol.
type PillarProtocolStatus struct {
	// bindingCount is the number of PillarBinding resources that reference
	// this protocol.  Maintained automatically by the reconciler.
	// +optional
	BindingCount int32 `json:"bindingCount,omitempty"`

	// activeTargets lists the names of PillarTargets currently serving
	// volumes via this protocol.  Maintained automatically by the reconciler.
	// +optional
	ActiveTargets []string `json:"activeTargets,omitempty"`

	// conditions represent the current state of the PillarProtocol resource.
	//
	// Known condition types:
	// - "Ready" – the protocol configuration is valid and ready for use.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ppr
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Bindings",type=integer,JSONPath=`.status.bindingCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarProtocol describes a reusable network-storage protocol configuration.
// It is node-independent: the same PillarProtocol can be referenced by multiple
// PillarBinding resources across different pools and targets.  The controller
// resolves the target bind address at runtime from the relevant PillarTarget.
type PillarProtocol struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired protocol configuration.
	// +required
	Spec PillarProtocolSpec `json:"spec"`

	// status reflects the reconciler-observed state of this protocol.
	// +optional
	Status PillarProtocolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PillarProtocolList contains a list of PillarProtocol.
type PillarProtocolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PillarProtocol `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarProtocol{}, &PillarProtocolList{})
}
