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

// PillarVolumeStatePhase describes the lifecycle phase of a CSI volume as tracked
// by the pillar-csi controller.
//
// The phases map to VolumeState constants in the internal/csi package:
//
//	PillarVolumeStatePhaseProvisioning     → in-progress CreateVolume (transient)
//	PillarVolumeStatePhaseCreatePartial    → StateCreatePartial (backend created, export failed)
//	PillarVolumeStatePhaseReady            → StateCreated (fully provisioned)
//	PillarVolumeStatePhaseControllerPublished → StateControllerPublished
//	PillarVolumeStatePhaseNodeStagePartial → StateNodeStagePartial
//	PillarVolumeStatePhaseNodeStaged       → StateNodeStaged
//	PillarVolumeStatePhaseNodePublished    → StateNodePublished
//
// +kubebuilder:validation:Enum=Provisioning;CreatePartial;Ready;ControllerPublished;NodeStagePartial;NodeStaged;NodePublished
type PillarVolumeStatePhase string

const (
	// PillarVolumeStatePhaseProvisioning means CreateVolume has been started but
	// has not yet completed.  This is a transient state; the controller should
	// advance it to CreatePartial or Ready before returning to the caller.
	PillarVolumeStatePhaseProvisioning PillarVolumeStatePhase = "Provisioning"

	// PillarVolumeStatePhaseCreatePartial means the backend storage resource
	// (zvol, LVM LV, etc.) was created successfully, but the ExportVolume
	// step failed.  The volume exists on the storage node but is not yet
	// accessible over the network.
	//
	// Recovery options:
	//   - Retry CreateVolume: the controller re-attempts ExportVolume.
	//   - Call DeleteVolume: the controller calls UnexportVolume (noop) and
	//     then DeleteVolume on the agent to clean up the backend resource.
	PillarVolumeStatePhaseCreatePartial PillarVolumeStatePhase = "CreatePartial"

	// PillarVolumeStatePhaseReady means CreateVolume completed fully: both the
	// backend resource and its network export (NVMe-oF target, iSCSI target,
	// NFS share) exist.  Corresponds to StateCreated in VolumeStateMachine.
	PillarVolumeStatePhaseReady PillarVolumeStatePhase = "Ready"

	// PillarVolumeStatePhaseControllerPublished means ControllerPublishVolume has
	// succeeded.  The initiator NQN has been granted access to the NVMe-oF
	// subsystem.
	PillarVolumeStatePhaseControllerPublished PillarVolumeStatePhase = "ControllerPublished"

	// PillarVolumeStatePhaseNodeStagePartial means NodeStageVolume partially
	// succeeded: the NVMe-oF connect step completed but the mount step
	// failed.  Corresponds to StateNodeStagePartial in VolumeStateMachine.
	PillarVolumeStatePhaseNodeStagePartial PillarVolumeStatePhase = "NodeStagePartial"

	// PillarVolumeStatePhaseNodeStaged means NodeStageVolume has succeeded.
	// The volume is formatted and mounted at the CSI staging target path.
	PillarVolumeStatePhaseNodeStaged PillarVolumeStatePhase = "NodeStaged"

	// PillarVolumeStatePhaseNodePublished means NodePublishVolume has succeeded.
	// The staging path has been bind-mounted into a pod's target path.
	PillarVolumeStatePhaseNodePublished PillarVolumeStatePhase = "NodePublished"
)

// PartialFailureInfo records what happened when a CSI operation partially
// succeeded, leaving the volume in an inconsistent state that requires
// explicit recovery or cleanup.
type PartialFailureInfo struct {
	// failedOperation is the name of the CSI or agent-level operation that
	// failed (e.g., "ExportVolume", "NodeStageMount").
	// +required
	FailedOperation string `json:"failedOperation"`

	// failedAt is the time when the partial failure was recorded.
	// +required
	FailedAt metav1.Time `json:"failedAt"`

	// reason is a brief, machine-readable CamelCase word that describes the
	// category of failure (e.g., "AgentRPCFailed", "MountFailed").
	// +optional
	Reason string `json:"reason,omitempty"`

	// message is a human-readable sentence describing what failed and how to
	// recover.
	// +optional
	Message string `json:"message,omitempty"`

	// backendCreated is true when the backend storage resource (zvol, LVM LV,
	// etc.) was successfully created before the failure occurred.  When false,
	// DeleteVolume only needs to call UnexportVolume (idempotent no-op); when
	// true, it must also call DeleteVolume on the agent to reclaim the storage.
	// +optional
	BackendCreated bool `json:"backendCreated,omitempty"`

	// exportCreated is true when the network export (NVMe-oF subsystem) was
	// successfully created before the failure.  When
	// true, cleanup must call UnexportVolume before DeleteVolume.
	// +optional
	ExportCreated bool `json:"exportCreated,omitempty"`
}

// VolumeExportInfo holds the network export information returned by the
// agent's ExportVolume RPC.  These values are stored durably so that
// DeleteVolume can tear down the export after a controller restart.
type VolumeExportInfo struct {
	// targetID is the NVMe Qualified Name (NQN) of the NVMe-oF subsystem.
	// +optional
	TargetID string `json:"targetID,omitempty"`

	// address is the IP address of the storage node (same as
	// PillarAgent.Status.ResolvedAddress with the port stripped).
	// +optional
	Address string `json:"address,omitempty"`

	// port is the TCP port on which the NVMe-oF target listens.
	// +optional
	Port int32 `json:"port,omitempty"`

	// volumeRef is the protocol-level reference for this volume (the NVMe-oF
	// subsystem name).
	// +optional
	VolumeRef string `json:"volumeRef,omitempty"`
}

// VolumeExportSpec is the export configuration requested from the agent at
// CreateVolume time.  It is the durable desired state from which the
// controller re-creates the export after the storage node loses its target
// state (agent restart, node reboot), independent of later StorageClass or
// PillarProtocol changes.
type VolumeExportSpec struct {
	// bindAddress is the storage node address the target listens on.
	// +required
	// +kubebuilder:validation:MinLength=1
	BindAddress string `json:"bindAddress"`

	// port is the TCP port the target listens on; 0 selects the protocol's
	// default port, exactly as in the original export request.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// aclEnabled is true when the target admits only the initiators of
	// published nodes; false admits any initiator.
	// +required
	ACLEnabled bool `json:"aclEnabled"`

	// inCapsuleDataSize is the NVMe-oF/TCP in-capsule data size in bytes the
	// export requires on its port; absent when the export accepts the port's
	// value.  NVMe-oF/TCP only.
	// +optional
	// +kubebuilder:validation:Minimum=1024
	InCapsuleDataSize *int32 `json:"inCapsuleDataSize,omitempty"`
}

// VolumePublication records one node to which ControllerPublishVolume granted
// access to this volume.  The list of publications is the durable source of
// truth for CSI publish exclusivity (a SINGLE_NODE_* volume may be published
// to at most one node) and for the initiator ACL set the storage target must
// hold.  An entry is written before the agent grants access and removed only
// after the agent revoked it, so a crash between the two steps leaves the
// record fail-closed.
type VolumePublication struct {
	// nodeID is the CSI node_id (Kubernetes node name) the volume is
	// published to.
	// +required
	// +kubebuilder:validation:MinLength=1
	NodeID string `json:"nodeID"`

	// initiatorID is the protocol-specific initiator identity granted access
	// (NVMe-oF host NQN, iSCSI IQN, or the node ID for file protocols).  It is
	// recorded so the grant can be revoked even after the node's CSINode
	// object is gone.
	// +required
	// +kubebuilder:validation:MinLength=1
	InitiatorID string `json:"initiatorID"`

	// accessMode is the CSI VolumeCapability access mode name requested by
	// the publish (e.g. "SINGLE_NODE_WRITER", "MULTI_NODE_READER_ONLY").
	// +required
	// +kubebuilder:validation:MinLength=1
	AccessMode string `json:"accessMode"`

	// readonly mirrors ControllerPublishVolumeRequest.readonly.
	// +optional
	Readonly bool `json:"readonly,omitempty"`

	// revoking is set by ControllerUnpublishVolume in the same update that
	// allocates the fencing generation for the revoke, and the record is
	// removed once the agent revoked the initiator.  A revoking record still
	// occupies the volume for exclusivity (fail-closed) but is not part of the
	// initiator set the target should grant: state recovery must exclude it,
	// and a publish to the same node is rejected until the unpublish finishes.
	// +optional
	Revoking bool `json:"revoking,omitempty"`

	// local is true when the publication attaches the backend device
	// directly on the storage node instead of through the network export
	// (localAttach enabled and nodeID is the node of the volume's
	// PillarAgent).  A local publication grants no initiator: it is excluded
	// from the export's ACL set and its unpublish revokes nothing on the
	// target.
	// +optional
	Local bool `json:"local,omitempty"`
}

// VolumeClaimRef identifies the PersistentVolumeClaim a volume was
// provisioned for.  The UID pins one claim: a claim deleted and re-created
// under the same name is a different claim.
type VolumeClaimRef struct {
	// uid is the UID of the PersistentVolumeClaim.
	// +required
	// +kubebuilder:validation:MinLength=1
	UID string `json:"uid"`

	// namespace is the namespace of the PersistentVolumeClaim.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// name is the name of the PersistentVolumeClaim.
	// +optional
	Name string `json:"name,omitempty"`
}

// PillarVolumeStateSpec defines the immutable identity and routing information for
// a CSI volume.  Fields are populated by the controller at CreateVolume time
// and never changed thereafter.
type PillarVolumeStateSpec struct {
	// volumeID is the CSI volume ID assigned by the controller.
	// Format: <target-name>/<protocol-type>/<backend-type>/<agent-vol-id>
	// +required
	// +kubebuilder:validation:MinLength=1
	VolumeID string `json:"volumeID"`

	// agentVolumeID is the volume identifier used in agent RPCs.  The format
	// is "<pool>/<volume-name>" where pool is the storage pool name (e.g. ZFS
	// pool name), or just "<volume-name>" for backends with no pool prefix.
	// +required
	// +kubebuilder:validation:MinLength=1
	AgentVolumeID string `json:"agentVolumeID"`

	// agentRef is the name of the PillarAgent that hosts this volume.
	// +required
	// +kubebuilder:validation:MinLength=1
	AgentRef string `json:"agentRef"`

	// backendType is the storage backend routing token (e.g. "zfs-zvol",
	// "lvm-lv").  It is the backend member selected by the store's
	// spec.backend union at CreateVolume time.
	// +required
	BackendType string `json:"backendType"`

	// protocolType is the network storage protocol routing token
	// (e.g. "nvmeof-tcp").  It is the protocol member selected by the
	// protocol's spec.protocol union at CreateVolume time.
	// +required
	ProtocolType string `json:"protocolType"`

	// capacityBytes is the requested volume size in bytes.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CapacityBytes int64 `json:"capacityBytes,omitempty"`

	// claimRef identifies the PersistentVolumeClaim this volume was
	// provisioned for, when the provisioner named it (external-provisioner
	// --extra-create-metadata); its UID is read from the claim at the first
	// CreateVolume.  The controller uses it to recognize a provisioning
	// attempt that was abandoned because its claim was removed before any
	// PersistentVolume was created.  Without it the claim UID is derived from
	// the default "pvc-<claim UID>" volume name.
	// +optional
	ClaimRef *VolumeClaimRef `json:"claimRef,omitempty"`

	// importedFrom records the full source dataset name when the volume was
	// adopted from an existing ZFS zvol via the
	// "pillar-csi.bhyoo.com/import-zvol" PVC annotation (e.g.
	// "hot-data/k8s/pvc-abc123"), instead of being created empty.  The field
	// is informational — the imported zvol keeps its data but becomes a
	// normal volume: DeleteVolume destroys it, and the annotation has no
	// effect once the volume exists.
	// +optional
	ImportedFrom string `json:"importedFrom,omitempty"`

	// resolved is the effective per-volume configuration resolved at the
	// first CreateVolume attempt from the PillarStore, PillarProtocol,
	// PillarStorageClass overrides, StorageClass parameter documents and PVC
	// annotations (in that precedence order).  It is replayed on every retry
	// so the volume keeps the settings it was provisioned with even when the
	// claim or the CRDs behind the overrides no longer exist.
	// +optional
	Resolved *ResolvedVolumeConfig `json:"resolved,omitempty"`
}

// ResolvedVolumeConfig is the durable record of the effective configuration
// a volume was provisioned with.  The shapes reuse the CRD union documents
// so the stored object reads exactly like the layers it was resolved from.
type ResolvedVolumeConfig struct {
	// backend is the effective storage backend configuration: the store's
	// spec.backend with binding, StorageClass-document and PVC tunable
	// overrides applied.
	// +required
	Backend BackendSpec `json:"backend"`

	// protocol is the effective transport configuration: the protocol's
	// spec.protocol with binding, StorageClass-document and PVC tunable
	// overrides applied.
	// +required
	Protocol ProtocolSpec `json:"protocol"`

	// filesystem is the effective filesystem configuration the node applies
	// when the volume is a Filesystem-mode mount.
	// +optional
	Filesystem *FilesystemConfig `json:"filesystem,omitempty"`

	// localAttach is true when the volume may be attached directly on the
	// storage node, bypassing the network protocol, whenever it is published
	// to the node that hosts its PillarAgent.  Publishes to any other node
	// always use the protocol.
	// +optional
	LocalAttach bool `json:"localAttach,omitempty"`
}

// PillarVolumeStateStatus reflects the controller-observed state of a PillarVolumeState.
type PillarVolumeStateStatus struct {
	// phase is the current lifecycle phase of the volume.
	// See PillarVolumeStatePhase for the full state diagram.
	// +optional
	Phase PillarVolumeStatePhase `json:"phase,omitempty"`

	// partialFailure is populated whenever the volume is in a partial-failure
	// phase (CreatePartial, NodeStagePartial).  It records what succeeded and
	// what failed so that the recovery controller can take the minimum
	// necessary corrective action.  Cleared when the partial failure is
	// resolved.
	// +optional
	PartialFailure *PartialFailureInfo `json:"partialFailure,omitempty"`

	// backendDevicePath is the device path returned by agent.CreateVolume
	// (e.g. "/dev/zvol/pool/pvc-abc123").  Persisted when the volume enters
	// the CreatePartial phase so that a retry of CreateVolume can skip the
	// backend-creation step and call agent.ExportVolume directly, using this
	// stored path rather than re-querying the agent.
	// Cleared when the volume reaches the Ready phase.
	// +optional
	BackendDevicePath string `json:"backendDevicePath,omitempty"`

	// importAcquired records that agent.ImportVolume succeeded for a
	// spec.importedFrom volume: the agent durably adopted the pre-existing
	// zvol into this lifecycle.  While it is unset the lifecycle cannot prove
	// the agent ever took ownership, so ReapAbandonedVolume and DeleteVolume
	// end the lifecycle with agent.ReleaseVolume — which retires it at the
	// agent without touching the zvol — instead of UnexportVolume and
	// DeleteVolume, and ControllerExpandVolume and ControllerPublishVolume
	// refuse it: a refused or lost-response import that was torn down,
	// resized or exposed anyway would harm a zvol this driver never owned.
	// +optional
	ImportAcquired bool `json:"importAcquired,omitempty"`

	// exportInfo holds the network export parameters returned by ExportVolume.
	// Populated when phase is Ready or later.  Used by DeleteVolume to
	// unmount the export after a controller restart without re-querying the
	// agent.
	// +optional
	ExportInfo *VolumeExportInfo `json:"exportInfo,omitempty"`

	// publishedNodes lists every node the volume is currently published to
	// by ControllerPublishVolume.  ControllerPublishVolume rejects a publish
	// that is incompatible with an existing entry (for example a second node
	// for a SINGLE_NODE_* access mode); ControllerUnpublishVolume removes the
	// entry after the agent revoked the node's access; DeleteVolume refuses
	// to delete a volume while this list is non-empty.  Entries are also the
	// exact initiator set the storage target's ACL must contain.
	// +listType=map
	// +listMapKey=nodeID
	// +optional
	PublishedNodes []VolumePublication `json:"publishedNodes,omitempty"`

	// exportSpec is the export configuration the controller requested at
	// CreateVolume time.  It is the durable desired state the resync
	// controller uses to re-create the export after the storage node loses
	// its target state.  Volumes created before this field existed have no
	// exportSpec and are reported via the ExportReconciled condition instead
	// of being recovered.
	// +optional
	ExportSpec *VolumeExportSpec `json:"exportSpec,omitempty"`

	// localAttachNode is the storage node on which the volume was last
	// attached directly.  It is set in the same update that reserves a local
	// publication and cleared only after the agent confirmed the backend
	// device is no longer held on that node and returned the export to
	// serving remote initiators.  While it is set the export serves no I/O
	// to remote initiators, including after an agent restart.
	// +optional
	LocalAttachNode string `json:"localAttachNode,omitempty"`

	// publicationGeneration is a monotonically increasing counter bumped by
	// exactly 1 on every status update that changes publishedNodes or sets
	// deleting.  The value committed by that update is sent to the agent as
	// the fencing generation on AllowInitiator, DenyInitiator, UnexportVolume
	// and ReconcileState; the agent rejects any request carrying a lower
	// generation than the one it last applied.  This closes the window where
	// a stale (former leader) controller's in-flight RPC lands after a new
	// leader already changed the publication set.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PublicationGeneration int64 `json:"publicationGeneration,omitempty"`

	// deleting is set by DeleteVolume before it removes the export and the
	// backend resource.  It is written by a resourceVersion compare-and-swap
	// that only succeeds while publishedNodes is empty, so a volume can never
	// be concurrently published and deleted.  ControllerPublishVolume rejects
	// reservations while deleting is true, and state resync skips the volume.
	// +optional
	Deleting bool `json:"deleting,omitempty"`

	// conditions represent the current observed state of the PillarVolumeState.
	//
	// Known condition types:
	// - "BackendCreated"  – the backend storage resource exists on the agent.
	// - "ExportCreated"   – the network export exists on the agent.
	// - "Ready"           – both backend and export exist; volume is usable.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pvst
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.agentRef`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backendType`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocolType`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarVolumeState tracks the lifecycle state of a single CSI volume provisioned
// by pillar-csi.  The controller creates a PillarVolumeState during CreateVolume,
// updates it at each lifecycle stage, and deletes it during DeleteVolume.
//
// The primary purpose of PillarVolumeState is to provide durable partial-failure
// state tracking: if CreateVolume creates the backend zvol but then crashes
// before ExportVolume returns, the PillarVolumeStatePhaseCreatePartial phase is
// already written to etcd, allowing the next CreateVolume call (or an
// automated recovery controller) to skip the backend-creation step and retry
// only the export.
type PillarVolumeState struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// The name is the CSI volume name (the PVC UID-based name assigned by the
	// CO, e.g. "pvc-abc123").
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec holds the immutable volume identity and routing parameters.
	// +required
	Spec PillarVolumeStateSpec `json:"spec"`

	// status reflects the mutable lifecycle state of the volume.
	// +optional
	Status PillarVolumeStateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PillarVolumeStateList contains a list of PillarVolumeState.
type PillarVolumeStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PillarVolumeState `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarVolumeState{}, &PillarVolumeStateList{})
}
