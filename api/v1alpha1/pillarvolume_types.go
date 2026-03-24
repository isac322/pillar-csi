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

// PillarVolumePhase describes the lifecycle phase of a CSI volume as tracked
// by the pillar-csi controller.
//
// The phases map to VolumeState constants in the internal/csi package:
//
//	PillarVolumePhaseProvisioning     → in-progress CreateVolume (transient)
//	PillarVolumePhaseCreatePartial    → StateCreatePartial (backend created, export failed)
//	PillarVolumePhaseReady            → StateCreated (fully provisioned)
//	PillarVolumePhaseControllerPublished → StateControllerPublished
//	PillarVolumePhaseNodeStagePartial → StateNodeStagePartial
//	PillarVolumePhaseNodeStaged       → StateNodeStaged
//	PillarVolumePhaseNodePublished    → StateNodePublished
//
// +kubebuilder:validation:Enum=Provisioning;CreatePartial;Ready;ControllerPublished;NodeStagePartial;NodeStaged;NodePublished
type PillarVolumePhase string

const (
	// PillarVolumePhaseProvisioning means CreateVolume has been started but
	// has not yet completed.  This is a transient state; the controller should
	// advance it to CreatePartial or Ready before returning to the caller.
	PillarVolumePhaseProvisioning PillarVolumePhase = "Provisioning"

	// PillarVolumePhaseCreatePartial means the backend storage resource
	// (zvol, LVM LV, etc.) was created successfully, but the ExportVolume
	// step failed.  The volume exists on the storage node but is not yet
	// accessible over the network.
	//
	// Recovery options:
	//   - Retry CreateVolume: the controller re-attempts ExportVolume.
	//   - Call DeleteVolume: the controller calls UnexportVolume (noop) and
	//     then DeleteVolume on the agent to clean up the backend resource.
	PillarVolumePhaseCreatePartial PillarVolumePhase = "CreatePartial"

	// PillarVolumePhaseReady means CreateVolume completed fully: both the
	// backend resource and its network export (NVMe-oF target, iSCSI target,
	// NFS share) exist.  Corresponds to StateCreated in VolumeStateMachine.
	PillarVolumePhaseReady PillarVolumePhase = "Ready"

	// PillarVolumePhaseControllerPublished means ControllerPublishVolume has
	// succeeded.  The initiator NQN has been granted access to the NVMe-oF
	// subsystem.
	PillarVolumePhaseControllerPublished PillarVolumePhase = "ControllerPublished"

	// PillarVolumePhaseNodeStagePartial means NodeStageVolume partially
	// succeeded: the NVMe-oF connect step completed but the mount step
	// failed.  Corresponds to StateNodeStagePartial in VolumeStateMachine.
	PillarVolumePhaseNodeStagePartial PillarVolumePhase = "NodeStagePartial"

	// PillarVolumePhaseNodeStaged means NodeStageVolume has succeeded.
	// The volume is formatted and mounted at the CSI staging target path.
	PillarVolumePhaseNodeStaged PillarVolumePhase = "NodeStaged"

	// PillarVolumePhaseNodePublished means NodePublishVolume has succeeded.
	// The staging path has been bind-mounted into a pod's target path.
	PillarVolumePhaseNodePublished PillarVolumePhase = "NodePublished"
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

	// exportCreated is true when the network export (NVMe-oF target, iSCSI
	// target, NFS share) was successfully created before the failure.  When
	// true, cleanup must call UnexportVolume before DeleteVolume.
	// +optional
	ExportCreated bool `json:"exportCreated,omitempty"`
}

// VolumeExportInfo holds the network export information returned by the
// agent's ExportVolume RPC.  These values are stored durably so that
// DeleteVolume can tear down the export after a controller restart.
type VolumeExportInfo struct {
	// targetID is the NVMe Qualified Name (NQN) of the NVMe-oF subsystem, or
	// the iSCSI Qualified Name (IQN) of the iSCSI target.
	// +optional
	TargetID string `json:"targetID,omitempty"`

	// address is the IP address of the storage node (same as
	// PillarTarget.Status.ResolvedAddress with the port stripped).
	// +optional
	Address string `json:"address,omitempty"`

	// port is the TCP port on which the NVMe-oF or iSCSI target listens.
	// +optional
	Port int32 `json:"port,omitempty"`

	// volumeRef is the protocol-level reference for this volume (e.g., the
	// NVMe-oF subsystem name or the iSCSI target LUN identifier).
	// +optional
	VolumeRef string `json:"volumeRef,omitempty"`
}

// PillarVolumeSpec defines the immutable identity and routing information for
// a CSI volume.  Fields are populated by the controller at CreateVolume time
// and never changed thereafter.
type PillarVolumeSpec struct {
	// volumeID is the CSI volume ID assigned by the controller.
	// Format: <target-name>/<protocol-type>/<backend-type>/<agent-vol-id>
	// +required
	// +kubebuilder:validation:MinLength=1
	VolumeID string `json:"volumeID"`

	// agentVolumeID is the volume identifier used in agent RPCs.  For ZFS
	// backends this is "<zfs-pool>/<volume-name>"; for others it is
	// "<pool>/<volume-name>" or just "<volume-name>".
	// +required
	// +kubebuilder:validation:MinLength=1
	AgentVolumeID string `json:"agentVolumeID"`

	// targetRef is the name of the PillarTarget that hosts this volume.
	// +required
	// +kubebuilder:validation:MinLength=1
	TargetRef string `json:"targetRef"`

	// backendType is the storage backend driver string (e.g. "zfs-zvol",
	// "lvm-lv").  Mirrors the pillar-csi.bhyoo.com/backend-type StorageClass
	// parameter.
	// +required
	BackendType string `json:"backendType"`

	// protocolType is the network storage protocol string (e.g. "nvmeof-tcp",
	// "iscsi").  Mirrors the pillar-csi.bhyoo.com/protocol-type StorageClass
	// parameter.
	// +required
	ProtocolType string `json:"protocolType"`

	// capacityBytes is the requested volume size in bytes.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
}

// PillarVolumeStatus reflects the controller-observed state of a PillarVolume.
type PillarVolumeStatus struct {
	// phase is the current lifecycle phase of the volume.
	// See PillarVolumePhase for the full state diagram.
	// +optional
	Phase PillarVolumePhase `json:"phase,omitempty"`

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

	// exportInfo holds the network export parameters returned by ExportVolume.
	// Populated when phase is Ready or later.  Used by DeleteVolume to
	// unmount the export after a controller restart without re-querying the
	// agent.
	// +optional
	ExportInfo *VolumeExportInfo `json:"exportInfo,omitempty"`

	// conditions represent the current observed state of the PillarVolume.
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
// +kubebuilder:resource:scope=Cluster,shortName=pv
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backendType`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocolType`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarVolume tracks the lifecycle state of a single CSI volume provisioned
// by pillar-csi.  The controller creates a PillarVolume during CreateVolume,
// updates it at each lifecycle stage, and deletes it during DeleteVolume.
//
// The primary purpose of PillarVolume is to provide durable partial-failure
// state tracking: if CreateVolume creates the backend zvol but then crashes
// before ExportVolume returns, the PillarVolumePhaseCreatePartial phase is
// already written to etcd, allowing the next CreateVolume call (or an
// automated recovery controller) to skip the backend-creation step and retry
// only the export.
type PillarVolume struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// The name is the CSI volume name (the PVC UID-based name assigned by the
	// CO, e.g. "pvc-abc123").
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec holds the immutable volume identity and routing parameters.
	// +required
	Spec PillarVolumeSpec `json:"spec"`

	// status reflects the mutable lifecycle state of the volume.
	// +optional
	Status PillarVolumeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PillarVolumeList contains a list of PillarVolume.
type PillarVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PillarVolume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarVolume{}, &PillarVolumeList{})
}
