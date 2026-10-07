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
	"fmt"
	"strconv"

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
//	PillarVolumeStatePhaseRecoveryPending  → no VolumeState mapping (non-serving recovery intent)
//
// +kubebuilder:validation:Enum=Provisioning;CreatePartial;Ready;ControllerPublished;NodeStagePartial;NodeStaged;NodePublished;RecoveryPending
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

	// PillarVolumeStatePhaseRecoveryPending means the volume record exists
	// only to receive an operator-authorized ownership transfer (spec.recovery
	// is set).  From its initial creation the object is non-serving — CSI
	// publish, stage, expand and resync operations refuse it — and non-reapable:
	// the abandoned-volume reaper must not release or delete it.  The phase
	// advances only after the agent's TransferVolumeOwnership commits and the
	// transferred mark's recorded uid/generation and authorization digest agree
	// with spec.recovery.
	PillarVolumeStatePhaseRecoveryPending PillarVolumeStatePhase = "RecoveryPending"
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
	// targetID is the protocol-specific export identifier: NQN for NVMe-oF,
	// IQN for iSCSI, or the server export path for NFS.
	// +optional
	TargetID string `json:"targetID,omitempty"`

	// address is the IP address of the storage node (same as
	// PillarAgent.Status.ResolvedAddress with the port stripped).
	// +optional
	Address string `json:"address,omitempty"`

	// port is the TCP port on which the protocol target listens.
	// NFS uses its fixed port 2049.
	// +optional
	Port int32 `json:"port,omitempty"`

	// volumeRef is the protocol-level reference for this volume.
	// +optional
	VolumeRef string `json:"volumeRef,omitempty"`
}

// NFSExportSpec is the protocol-specific durable desired state for an NFS
// export.  BindAddress and Port remain on VolumeExportSpec because they are
// common routing fields shared by every protocol.
type NFSExportSpec struct {
	// version is the NFS protocol version.
	// +required
	// +kubebuilder:validation:Enum="4.2"
	Version string `json:"version"`

	// squash is the NFS identity squashing policy.
	// +required
	// +kubebuilder:validation:Enum=root;none;all
	Squash NFSSquash `json:"squash"`

	// readonly makes the export read-only for every client.
	// +required
	Readonly bool `json:"readonly"`
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

	// maxDataTransferSize is the NVMe-oF/TCP maximum data transfer size in
	// bytes (0 = no limit) the export allows its port to advertise at most;
	// absent when the export accepts the port's value.  NVMe-oF/TCP only.
	// +optional
	// +kubebuilder:validation:Enum=0;8192;16384;32768;65536;131072;262144;524288;1048576;2097152;4194304;8388608;16777216;33554432;67108864;134217728;268435456;536870912;1073741824
	MaxDataTransferSize *int32 `json:"maxDataTransferSize,omitempty"`
	// nfs contains the durable NFS-specific export configuration.
	// +optional
	NFS *NFSExportSpec `json:"nfs,omitempty"`
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

	Name string `json:"name,omitempty"`
}

// FilesystemAdoptionKind identifies a preserved, existing filesystem source.
// +kubebuilder:validation:Enum=directory;zfs-dataset
type FilesystemAdoptionKind string

const (
	// FilesystemAdoptionKindDirectory identifies an adopted existing directory.
	FilesystemAdoptionKindDirectory FilesystemAdoptionKind = "directory"
	// FilesystemAdoptionKindZFSDataset identifies an adopted existing ZFS dataset.
	FilesystemAdoptionKindZFSDataset FilesystemAdoptionKind = "zfs-dataset"
)

// FilesystemAdoption pins an existing filesystem's canonical source and native
// identity. Its presence selects non-destructive filesystem lifecycle handling;
// it is distinct from importedFrom, whose zvol deletion semantics are unchanged.
// +kubebuilder:validation:XValidation:rule="self.kind != 'directory' || (self.filesystemType in ['ext4', 'xfs'] && has(self.filesystemID) && has(self.inode) && has(self.projectID) && !has(self.hostPath))",message="directory adoption requires ext4 or xfs, a filesystem UUID, inode and project ID, and no hostPath"
// +kubebuilder:validation:XValidation:rule="self.kind != 'zfs-dataset' || (self.filesystemType == 'zfs' && !has(self.filesystemID) && !has(self.inode) && !has(self.projectID))",message="ZFS dataset adoption requires zfs and no directory identity fields"
type FilesystemAdoption struct {
	// kind selects the existing source type.
	// +required
	Kind FilesystemAdoptionKind `json:"kind"`

	// canonicalSource is the resolved host directory or native dataset fullname.
	// +required
	// +kubebuilder:validation:MinLength=1
	CanonicalSource string `json:"canonicalSource"`

	// resourceID is the stable native identity, independent of path and pool
	// alias: filesystem UUID plus root inode, or ZFS dataset GUID.
	// +required
	// +kubebuilder:validation:MinLength=1
	ResourceID string `json:"resourceID"`

	// hostPath is a dataset's existing mounted host path, if mounted. It may
	// be empty for an unmounted legacy dataset. For directories, canonicalSource
	// already is the host path, so this field is omitted.
	// +optional
	HostPath string `json:"hostPath,omitempty"`

	// filesystemType identifies the native quota and identity implementation.
	// +required
	// +kubebuilder:validation:Enum=ext4;xfs;zfs
	FilesystemType string `json:"filesystemType"`

	// filesystemID is the directory's stable native filesystem UUID, never
	// a volatile device number or statfs filesystem ID. Omitted for ZFS.
	// +optional
	// +kubebuilder:validation:MinLength=1
	FilesystemID string `json:"filesystemID,omitempty"`

	// inode is the existing directory root's native inode as a canonical
	// nonzero decimal string. It is omitted for ZFS.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:Pattern="^[1-9][0-9]{0,19}$"
	Inode string `json:"inode,omitempty"`

	// projectID identifies the directory's existing bounded project-quota
	// scope. Adoption verifies it read-only; it never stamps project IDs.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ProjectID uint32 `json:"projectID,omitempty"`
}

// ParseFilesystemAdoptionInode parses the canonical decimal representation used
// by the Kubernetes API into the native uint64 inode value.
func ParseFilesystemAdoptionInode(value string) (uint64, error) {
	if value == "" || len(value) > 20 || (len(value) > 1 && value[0] == '0') {
		return 0, fmt.Errorf("inode must be a nonzero canonical decimal string")
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return 0, fmt.Errorf("inode must be a nonzero canonical decimal string")
		}
	}
	inode, err := strconv.ParseUint(value, 10, 64)
	if err != nil || inode == 0 {
		return 0, fmt.Errorf("inode must be a nonzero uint64 value")
	}
	return inode, nil
}

// FormatFilesystemAdoptionInode returns the canonical decimal representation
// of a native inode. Zero returns the omitted-value representation.
func FormatFilesystemAdoptionInode(value uint64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatUint(value, 10)
}

// PillarVolumeStateSpec defines the immutable identity and routing information for
// a CSI volume.  Fields are populated by the controller at CreateVolume time
// and never changed thereafter.
//
// The spec-level transition rules preserve descriptor presence; a rule on an
// optional descriptor alone cannot prevent adding or removing it.
// +kubebuilder:validation:XValidation:rule="!has(self.filesystemAdoption) || !has(self.importedFrom) || size(self.importedFrom) == 0",message="filesystemAdoption and importedFrom are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!(has(self.filesystemAdoption) && has(self.lvmSource))",message="filesystemAdoption and lvmSource are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.filesystemAdoption) == has(oldSelf.filesystemAdoption)",message="filesystem adoption presence is immutable"
// +kubebuilder:validation:XValidation:rule="!(has(self.importedFrom) && has(self.lvmSource))",message="importedFrom and lvmSource are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.lvmSource) == has(oldSelf.lvmSource)",message="lvmSource cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="!(has(self.recovery) && (has(self.lvmSource) || has(self.importedFrom) || has(self.filesystemAdoption)))",message="recovery is mutually exclusive with lvmSource, importedFrom and filesystemAdoption"
// +kubebuilder:validation:XValidation:rule="has(self.recovery) == has(oldSelf.recovery)",message="recovery cannot be added or removed after creation"
type PillarVolumeStateSpec struct {
	// volumeID is the public CSI handle assigned by the controller.
	// Existing volumes keep the format:
	// <target-name>/<protocol-type>/<backend-type>/<agent-vol-id>.
	// New file volumes append "." and an opaque 32-lowercase-hex lifecycle
	// nonce. Retries and manual Retain rebinding preserve the stored handle;
	// adopting the same source into a later lifecycle produces a new handle.
	// +required
	// +kubebuilder:validation:MinLength=1
	VolumeID string `json:"volumeID"`

	// agentVolumeID is the backing-resource routing identifier used in agent
	// RPCs. The format is "<pool>/<volume-name>" where pool is the storage
	// pool name, or just "<volume-name>" for backends with no pool prefix.
	// Filesystem adoption derives its stable leaf from native resource identity,
	// independent of the public handle's lifecycle nonce. Source re-adoption
	// retains this routing identity but does not reuse the old public handle.
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

	// filesystemAdoption records the immutable identity of an existing
	// directory or ZFS filesystem dataset. Delete retires only owned state
	// and preserves the original source; recovery must never recreate it.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="filesystem adoption identity is immutable"
	FilesystemAdoption *FilesystemAdoption `json:"filesystemAdoption,omitempty"`

	// lvmSource pins the pre-existing LVM logical volume this volume adopted
	// via the "pillar-csi.bhyoo.com/import-lv" PVC annotation, by name and
	// by stable VG/LV UUIDs, together with the adoption policy.  It is set
	// once at the first CreateVolume attempt and is immutable: it can be
	// neither changed nor added or removed after creation.  Absent for
	// volumes created empty and for zvol imports (see importedFrom), which
	// keep their existing semantics.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="lvmSource is immutable"
	LVMSource *LVMSourceRef `json:"lvmSource,omitempty"`

	// resolved is the effective per-volume configuration resolved at the
	// first CreateVolume attempt from the PillarStore, PillarProtocol,
	// PillarStorageClass overrides, StorageClass parameter documents and PVC
	// annotations (in that precedence order).  It is replayed on every retry
	// so the volume keeps the settings it was provisioned with even when the
	// claim or the CRDs behind the overrides no longer exist.
	// +optional
	Resolved *ResolvedVolumeConfig `json:"resolved,omitempty"`

	// recovery declares that this volume record exists solely to receive an
	// operator-authorized ownership transfer of a pre-existing adopted
	// backend resource (see VolumeRecoveryIntent).  It is set at creation
	// for a volume born in phase RecoveryPending and cannot be added or
	// removed after creation.  Mutually exclusive with lvmSource,
	// importedFrom and filesystemAdoption — a recovery volume is not itself
	// an import.
	//
	// The fields the operator cannot know at create time — newVolumeUID
	// (this object's own metadata.uid), authorization and
	// authorizationDigest — are write-once: they may be empty at create and
	// populated exactly once afterwards; every other field is immutable
	// from creation.
	// +optional
	Recovery *VolumeRecoveryIntent `json:"recovery,omitempty"`
}

// VolumeRecoveryIntent pins the operator-declared recovery intent for a
// volume born in phase RecoveryPending: which retired lifecycle's adopted
// backend resource transfers into this record, under which policy, and
// bound to which signed RecoveryAuthorization.
//
// The operator cannot know this record's metadata.uid or produce the
// authorization before the record exists, so intent is declared in two
// steps: oldVolumeUID, oldGeneration, source and newGeneration are set at
// creation and immutable; newVolumeUID (which MUST equal metadata.uid) and
// authorizationDigest may be empty at creation and are write-once
// afterwards — each can be set exactly once and then never changed or
// cleared.
//
// Authorization (the raw serialized grant) is deliberately not
// compared by CEL: Kubernetes CEL cannot reliably compare byte-format
// fields across updates, and the grant's identity is already pinned by the
// write-once authorizationDigest.  The controller requires the digest of
// whatever authorization bytes it uses to equal authorizationDigest, and
// the agent verifies the grant's signature against its trust anchor, so a
// re-supplied or different payload can never authorize anything the
// digest does not name.  The controller performs the transfer only once
// all fields are populated and consistent.
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.newVolumeUID) || (has(self.newVolumeUID) && self.newVolumeUID == oldSelf.newVolumeUID)) && (!has(oldSelf.authorizationDigest) || (has(self.authorizationDigest) && self.authorizationDigest == oldSelf.authorizationDigest))",message="newVolumeUID and authorizationDigest are write-once once populated"
type VolumeRecoveryIntent struct {
	// oldVolumeUID is the UID of the retired PillarVolumeState lifecycle
	// whose backend resource is recovered (the fence mark's current
	// volume_uid).  Immutable from creation.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="oldVolumeUID is immutable"
	OldVolumeUID string `json:"oldVolumeUID"`

	// oldGeneration is the exact publication generation recorded in the
	// old lifecycle's fence mark.  Exact equality required.  Immutable
	// from creation.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="oldGeneration is immutable"
	OldGeneration int64 `json:"oldGeneration"`

	// source pins the adopted LVM logical volume being recovered by name
	// and stable VG/LV UUIDs; its preserveOriginal is the recovery
	// preserve policy.  Immutable from creation.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="source is immutable"
	Source *LVMSourceRef `json:"source"`

	// newVolumeUID is the destination lifecycle UID; when set it MUST
	// equal this PillarVolumeState's metadata.uid.  It is unknowable at
	// create (the API server assigns metadata.uid), so it may be empty at
	// creation and is write-once once populated.  The transfer requires
	// it to be set.
	// +optional
	// +kubebuilder:validation:MinLength=1
	NewVolumeUID string `json:"newVolumeUID,omitempty"`

	// newGeneration is the generation the agent writes into the
	// transferred fence mark for the new lifecycle.  Immutable from
	// creation.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="newGeneration is immutable"
	NewGeneration int64 `json:"newGeneration"`

	// authorization is the serialized RecoveryAuthorization protobuf (the
	// operator-signed grant).  It may be empty at creation — the operator
	// can only sign once metadata.uid exists.  It is not write-once at the
	// schema level (CEL does not compare these raw bytes); it may be
	// re-supplied on retry, but only bytes whose canonical digest equals
	// authorizationDigest are ever used, and the agent verifies the
	// signature.  When absent at transfer time the authorization must be
	// supplied out-of-band, with authorizationDigest pinning its identity.
	// +optional
	Authorization []byte `json:"authorization,omitempty"`

	// authorizationDigest is the lowercase hex SHA-256 of the
	// RecoveryAuthorization's deterministic protobuf encoding with its
	// signature field cleared.  It pins the exact grant this record may
	// consume; the fence mark stores the same digest after transfer.  It
	// may be empty at creation and is write-once once populated.  The
	// transfer requires it to be set.
	// +optional
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	AuthorizationDigest string `json:"authorizationDigest,omitempty"`
}

// LVMSourceRef pins the pre-existing LVM logical volume an import-lv volume
// adopted.  It is set once at the first CreateVolume attempt and is
// immutable.  The UUIDs are the stable identity: a VG or LV renamed or
// recreated under the same name never matches.
type LVMSourceRef struct {
	// volumeGroup is the LVM volume group name of the adopted LV.
	// +required
	// +kubebuilder:validation:MinLength=1
	VolumeGroup string `json:"volumeGroup"`

	// logicalVolume is the LVM logical volume name of the adopted LV.
	// +required
	// +kubebuilder:validation:MinLength=1
	LogicalVolume string `json:"logicalVolume"`

	// volumeGroupUUID is the LVM UUID of the volume group (`vgs -o vg_uuid`).
	// +required
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]{6}(-[A-Za-z0-9]{4}){5}-[A-Za-z0-9]{6}$`
	VolumeGroupUUID string `json:"volumeGroupUUID"`

	// logicalVolumeUUID is the LVM UUID of the logical volume
	// (`lvs -o lv_uuid`).
	// +required
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]{6}(-[A-Za-z0-9]{4}){5}-[A-Za-z0-9]{6}$`
	LogicalVolumeUUID string `json:"logicalVolumeUUID"`

	// preserveOriginal selects the adoption policy.  When true, DeleteVolume
	// only releases the LV (never lvremove), expansion is refused, and the
	// node never formats, fscks or resizes it.  When false (policy Managed)
	// the adopted LV becomes a normal managed volume.
	// +required
	PreserveOriginal bool `json:"preserveOriginal"`
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

	// localAttach records the volume's resolved attachment mode. Default-driver
	// volumes may bypass the protocol on their PillarAgent's node; publishes
	// on other nodes use the protocol. For filesystem adoption, true means
	// local-only attachment with no network export or NFS manager dependency.
	// Single-node file volumes always record true; multi-node file volumes
	// record false and require a working NFS manager.
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

	// importAcquired records that agent.ImportVolume succeeded for an
	// importedFrom, lvmSource or filesystemAdoption volume: the agent durably adopted
	// the existing resource into this lifecycle. While it is unset, the lifecycle
	// cannot prove the agent ever took ownership, so ReapAbandonedVolume and DeleteVolume
	// end the lifecycle with agent.ReleaseVolume — which retires it at the
	// agent without touching the source — instead of UnexportVolume and
	// DeleteVolume, and ControllerExpandVolume and ControllerPublishVolume
	// refuse it: a refused or lost-response import that was torn down,
	// resized or exposed anyway would harm a resource this driver never owned.
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
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.lvmSource.logicalVolumeUUID`
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
