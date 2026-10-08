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

// ProtocolID identifies a network storage protocol.  It is the union member
// name used everywhere a protocol can be selected — PillarProtocol.spec.protocol,
// override documents, PVC annotations — and also the token embedded in CSI
// volume IDs and agent RPCs.
type ProtocolID string

// Supported ProtocolID values.
const (
	// ProtocolIDNVMeOFTCP exports volumes over NVMe-oF/TCP.
	ProtocolIDNVMeOFTCP ProtocolID = "nvmeof-tcp"

	// ProtocolIDISCSI exports volumes over iSCSI (LIO target, in-process
	// initiator on the node).
	ProtocolIDISCSI ProtocolID = "iscsi"

	// ProtocolIDNFS exports filesystem volumes over NFS.
	ProtocolIDNFS ProtocolID = "nfs"
)

// NFSSquash selects the NFS root squashing policy.
// +kubebuilder:validation:Enum=root;none;all
type NFSSquash string

const (
	// NFSSquashRoot applies root squashing (the safe default).
	NFSSquashRoot NFSSquash = "root"
	// NFSSquashNone disables root squashing.
	NFSSquashNone NFSSquash = "none"
	// NFSSquashAll squashes all users.
	NFSSquashAll NFSSquash = "all"
)

// NVMeOFTCPConfig holds NVMe-oF/TCP-specific protocol parameters.
// Target bind IP is not included here — the controller resolves it
// at runtime from the referenced PillarAgent.
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
	// Defaults to false (allow_any_host) so that e2e tests and simple
	// deployments work without registering initiator NQNs.
	// +optional
	// +kubebuilder:default=false
	ACL bool `json:"acl"`

	// maxQueueSize is the I/O queue depth the initiator requests for each
	// queue of the connection (the fabrics queue_size connect option).
	// Unset keeps the kernel default (128).  The kernel accepts 16-1024.
	// Applies to connections made after the change; a staged volume keeps
	// the value from its CreateVolume.
	// +optional
	// +kubebuilder:validation:Minimum=16
	// +kubebuilder:validation:Maximum=1024
	MaxQueueSize *int32 `json:"maxQueueSize,omitempty"`

	// inCapsuleDataSize is the maximum in-capsule data size in bytes the
	// target advertises (the nvmet port's param_inline_data_size).  It is a
	// property of the listening port, shared by every volume exported on the
	// same storage node address and port: exporting a volume that requests a
	// value different from the port's current value fails instead of
	// silently using the port's value.  Unset keeps the transport default
	// (16384 for TCP on 4 KiB pages) and accepts whatever value the port has.
	// The minimum is 1024: NVMe/TCP hosts send the 1024-byte fabrics Connect
	// data in-capsule, so a smaller value makes every connect fail.
	// +optional
	// +kubebuilder:validation:Minimum=1024
	InCapsuleDataSize *int32 `json:"inCapsuleDataSize,omitempty"`

	// maxDataTransferSize is the largest data transfer, in bytes, of one
	// NVMe I/O command (MDTS).  Unset resolves to 4194304 (4 MiB); 0 means
	// no limit.  Any other value is a power of two from 8192 to 1073741824.
	//
	// The target advertises it through the nvmet port's param_mdts on
	// Linux 7.1 and later.  Like inCapsuleDataSize it is a property of the
	// listening port, shared by every volume exported on the same storage
	// node address and port, but a volume's value is an upper bound: a port
	// advertising a smaller limit only makes commands smaller.  Exporting a
	// volume therefore fails only when the port already advertises a larger
	// limit than the volume's (non-zero) value.  A port that advertises no
	// limit accepts any value, and when the agent restores the exports of a
	// port, for example after the storage node rebooted, it sets the port to
	// the smallest value among them before exporting any.  An older target
	// kernel cannot advertise a limit (MDTS 0); the node then caps the
	// volume's queue/max_sectors_kb to this size itself, which keeps large
	// writes from failing on a storage node whose memory is too fragmented
	// to allocate a whole command's scatterlist.  A node whose target
	// advertises any limit leaves the device unchanged.  The node cannot set
	// queue/max_sectors_kb below one memory page, so on a worker with
	// 16 KiB or 64 KiB pages a non-zero value smaller than the page size
	// fails NodeStageVolume with InvalidArgument instead of being rounded
	// up.
	// +optional
	// +kubebuilder:validation:Enum=0;8192;16384;32768;65536;131072;262144;524288;1048576;2097152;4194304;8388608;16777216;33554432;67108864;134217728;268435456;536870912;1073741824
	MaxDataTransferSize *int32 `json:"maxDataTransferSize,omitempty"`

	// ctrlLossTmo is the maximum seconds to wait before declaring a target
	// permanently lost after connectivity failure.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CtrlLossTmo *int32 `json:"ctrlLossTmo,omitempty"`

	// reconnectDelay is the interval in seconds between reconnect attempts.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReconnectDelay *int32 `json:"reconnectDelay,omitempty"`

	// hdrDigest makes the initiator request the NVMe/TCP header digest: a
	// CRC32C over every PDU header, so a header corrupted in flight fails
	// with a digest error instead of being acted on.  The target needs no
	// configuration; it enables the digest the initiator requests.
	//
	// Unlike the tunables above it is not recorded per volume: the
	// controller reads it from this PillarProtocol whenever it publishes a
	// volume to a node, so a change applies to every volume of the
	// protocol, including volumes provisioned before the change, from
	// their next attach.  A volume already connected on a node keeps the
	// digest settings of its existing connection until it is unstaged.
	// +optional
	HdrDigest bool `json:"hdrDigest,omitempty"`

	// dataDigest makes the initiator request the NVMe/TCP data digest: a
	// CRC32C over every PDU data payload, so data corrupted in flight
	// fails with a digest error instead of reaching the disk or the
	// application.  It applies like hdrDigest.
	// +optional
	DataDigest bool `json:"dataDigest,omitempty"`
}

// NFSConfig holds NFS-specific protocol parameters.
// The server always uses NFSv4.2 over TCP on port 2049.
type NFSConfig struct {
	// version is the NFS protocol version.
	// +optional
	// +kubebuilder:default="4.2"
	// +kubebuilder:validation:Enum="4.2"
	Version string `json:"version,omitempty"`

	// port is the fixed NFS listener port.
	// +optional
	// +kubebuilder:default=2049
	// +kubebuilder:validation:Minimum=2049
	// +kubebuilder:validation:Maximum=2049
	Port int32 `json:"port,omitempty"`

	// acl enables client-IP access control.
	// Defaults to false.
	// +optional
	// +kubebuilder:default=false
	ACL bool `json:"acl"`

	// squash selects the NFS identity squashing policy.
	// Defaults to root.
	// +optional
	// +kubebuilder:default=root
	Squash NFSSquash `json:"squash,omitempty"`
}

// EffectiveVersion returns the configured NFS version, defaulting to 4.2.
func (c *NFSConfig) EffectiveVersion() string {
	if c == nil || c.Version == "" {
		return "4.2"
	}
	return c.Version
}

// EffectivePort returns the configured NFS port, defaulting to 2049.
func (c *NFSConfig) EffectivePort() int32 {
	if c == nil || c.Port == 0 {
		return 2049
	}
	return c.Port
}

// EffectiveSquash returns the configured NFS squash mode, defaulting to root.
func (c *NFSConfig) EffectiveSquash() NFSSquash {
	if c == nil || c.Squash == "" {
		return NFSSquashRoot
	}
	return c.Squash
}

// ISCSIConfig holds iSCSI-specific protocol parameters.
// Target bind IP is not included here — the controller resolves it
// at runtime from the referenced PillarAgent.  The target IQN is derived by
// the agent from the volume ID, and the node's initiator IQN is published on
// its CSINode object, so neither is configured here.
//
// All timeouts are in seconds.  An unset timeout keeps the node default
// (loginTimeout 15, replacementTimeout 120, noopOutInterval 5,
// noopOutTimeout 5).  Timeouts apply to sessions logged in after the change;
// a staged volume keeps the values from its CreateVolume.
//
// +kubebuilder:validation:XValidation:rule="!has(self.auth) || self.auth.method == 'None' || has(self.auth.secretRef)",message="auth.secretRef is required when auth.method is CHAP or MutualCHAP"
// +kubebuilder:validation:XValidation:rule="!has(self.auth) || self.auth.method == 'None' || self.acl",message="auth.method CHAP and MutualCHAP require acl: true"
type ISCSIConfig struct {
	// port is the TCP port on which the iSCSI target portal listens.
	// Defaults to 3260.
	// +optional
	// +kubebuilder:default=3260
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// acl enables initiator IQN-based access control when true.
	// When false the target portal group runs in demo mode
	// (generate_node_acls) and accepts any initiator.
	// Defaults to false so that e2e tests and simple deployments work
	// without registering initiator IQNs.
	// +optional
	// +kubebuilder:default=false
	ACL bool `json:"acl"`

	// loginTimeout is the maximum seconds the initiator waits for a login
	// (TCP connect plus the login PDU exchange) to complete.
	// +optional
	// +kubebuilder:validation:Minimum=1
	LoginTimeout *int32 `json:"loginTimeout,omitempty"`

	// replacementTimeout is the maximum seconds the initiator keeps I/O
	// queued while re-establishing a failed session before failing it back
	// to the block layer.  0 fails I/O immediately on connection loss.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReplacementTimeout *int32 `json:"replacementTimeout,omitempty"`

	// noopOutInterval is the interval in seconds between NOP-Out pings the
	// initiator sends to detect a dead connection.  0 disables the pings.
	// +optional
	// +kubebuilder:validation:Minimum=0
	NoopOutInterval *int32 `json:"noopOutInterval,omitempty"`

	// noopOutTimeout is the maximum seconds the initiator waits for a
	// NOP-In reply before declaring the connection failed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	NoopOutTimeout *int32 `json:"noopOutTimeout,omitempty"`

	// auth configures iSCSI CHAP authentication.  Unset is method None
	// (initiator IQN ACLs only).  Auth is fixed per volume at CreateVolume:
	// a later change applies only to volumes provisioned afterwards, and it
	// cannot be overridden per binding or per volume.  CHAP credentials are
	// set on the per-initiator ACLs, so CHAP and MutualCHAP require acl: true.
	// +optional
	Auth *ISCSIAuth `json:"auth,omitempty"`
}

// ISCSIAuthMethod selects the iSCSI authentication method.
// +kubebuilder:validation:Enum=None;CHAP;MutualCHAP
type ISCSIAuthMethod string

// Supported ISCSIAuthMethod values.
const (
	// ISCSIAuthMethodNone disables authentication: initiator IQN ACLs only.
	ISCSIAuthMethodNone ISCSIAuthMethod = "None"

	// ISCSIAuthMethodCHAP is one-way CHAP: the target authenticates the
	// initiator.
	ISCSIAuthMethodCHAP ISCSIAuthMethod = "CHAP"

	// ISCSIAuthMethodMutualCHAP is CHAP in both directions: the initiator
	// also authenticates the target.
	ISCSIAuthMethodMutualCHAP ISCSIAuthMethod = "MutualCHAP"
)

// ISCSIAuth configures iSCSI CHAP authentication.
//
// The credentials live in a Secret in the pillar-csi installation namespace
// (the controller's namespace) with the keys username and password (CHAP and
// MutualCHAP) plus mutualUsername and mutualPassword (MutualCHAP only).
// Passwords must be 12 to 255 bytes, and mutualPassword must differ from
// password (RFC 7143 §12.1.3).  The controller reads the Secret each time it
// configures a target: new contents apply to the target at the next
// ControllerPublishVolume or export restore, and to the node at the next
// NodeStageVolume.
type ISCSIAuth struct {
	// method selects the authentication method.
	// +optional
	// +kubebuilder:default=None
	Method ISCSIAuthMethod `json:"method,omitempty"`

	// secretRef names the Secret holding the CHAP credentials in the
	// pillar-csi installation namespace.  Required unless method is None.
	// +optional
	SecretRef *ISCSIAuthSecretReference `json:"secretRef,omitempty"`
}

// ISCSIAuthSecretReference names a Secret in the pillar-csi installation
// namespace.
type ISCSIAuthSecretReference struct {
	// name is the name of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// EffectiveMethod returns the configured method, None when auth is unset
// or its method is empty.
func (a *ISCSIAuth) EffectiveMethod() ISCSIAuthMethod {
	if a == nil || a.Method == "" {
		return ISCSIAuthMethodNone
	}
	return a.Method
}

// ProtocolSpec describes the transport protocol of a PillarProtocol.
// Exactly one member must be set: the member name selects the protocol
// (nvmeofTcp, iscsi, or nfs) and its value carries that protocol's configuration.
//
// Per-binding and per-volume override documents use ProtocolOverrides,
// which keeps only the tunable subset.
//
// +kubebuilder:validation:XValidation:rule="(has(self.nvmeofTcp) ? 1 : 0) + (has(self.iscsi) ? 1 : 0) + (has(self.nfs) ? 1 : 0) == 1",message="exactly one protocol member must be set (supported: nvmeofTcp, iscsi, nfs)"
type ProtocolSpec struct {
	// nvmeofTcp holds NVMe-oF/TCP configuration.
	// +optional
	NVMeOFTCP *NVMeOFTCPConfig `json:"nvmeofTcp,omitempty"`

	// iscsi holds iSCSI configuration.
	// +optional
	ISCSI *ISCSIConfig `json:"iscsi,omitempty"`

	// nfs holds NFS configuration.
	// +optional
	NFS *NFSConfig `json:"nfs,omitempty"`
}

// Kind returns the selected protocol member as a ProtocolID, or "" when the
// union is empty.
func (p ProtocolSpec) Kind() ProtocolID {
	switch {
	case p.NVMeOFTCP != nil:
		return ProtocolIDNVMeOFTCP
	case p.ISCSI != nil:
		return ProtocolIDISCSI
	case p.NFS != nil:
		return ProtocolIDNFS
	default:
		return ""
	}
}

// PillarProtocolSpec defines the desired state of PillarProtocol.
type PillarProtocolSpec struct {
	// protocol selects the network storage protocol and its configuration.
	// Exactly one member must be set.
	// +required
	Protocol ProtocolSpec `json:"protocol"`
}

// PillarProtocolStatus defines the observed state of PillarProtocol.
type PillarProtocolStatus struct {
	// storageClassCount is the number of PillarStorageClass resources that reference
	// this protocol.  Maintained automatically by the reconciler.
	// +optional
	StorageClassCount int32 `json:"storageClassCount,omitempty"`

	// activeAgents lists the names of PillarAgents currently serving
	// volumes via this protocol.  Maintained automatically by the reconciler.
	// +optional
	ActiveAgents []string `json:"activeAgents,omitempty"`

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
// +kubebuilder:resource:scope=Cluster,shortName=pstr
// +kubebuilder:printcolumn:name="Bindings",type=integer,JSONPath=`.status.storageClassCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarProtocol describes a reusable network-storage protocol configuration.
// It is node-independent: the same PillarProtocol can be referenced by multiple
// PillarStorageClass resources across different pools and targets.  The controller
// resolves the target bind address at runtime from the relevant PillarAgent.
type PillarProtocol struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PillarProtocolSpec   `json:"spec,omitempty"`
	Status PillarProtocolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PillarProtocolList contains a list of PillarProtocol.
type PillarProtocolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PillarProtocol `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarProtocol{}, &PillarProtocolList{})
}
