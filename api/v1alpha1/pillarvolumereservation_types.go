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

// PillarVolumeReservationSpec identifies one backend volume reservation and
// the volume lifecycle that holds it.
type PillarVolumeReservationSpec struct {
	// agentRef is the PillarAgent whose storage node holds the reserved
	// backend volume.
	// +required
	// +kubebuilder:validation:MinLength=1
	AgentRef string `json:"agentRef"`

	// backendType is the storage backend routing token of the reserved
	// backend volume (e.g. "zfs-zvol").
	// +required
	// +kubebuilder:validation:MinLength=1
	BackendType string `json:"backendType"`

	// agentVolumeID is the reserved backend volume identifier
	// ("<pool>/<volume-name>"), the same value the agent RPCs carry.
	// +required
	// +kubebuilder:validation:MinLength=1
	AgentVolumeID string `json:"agentVolumeID"`

	// filesystemResourceID is the canonical native backing-resource key for
	// filesystem adoption, independent of logical pool and source path.
	// Filesystem aliases reserve this key rather than agentVolumeID; the
	// latter remains the routing identity. Omitted for legacy block volumes.
	// +optional
	FilesystemResourceID string `json:"filesystemResourceID,omitempty"`

	// ownerVolume is the name of the PillarVolumeState lifecycle that holds
	// the reservation.  A reservation survives the whole lifecycle, including
	// retries of refused backend calls, and is released only when the owning
	// lifecycle is retired.
	// +required
	// +kubebuilder:validation:MinLength=1
	OwnerVolume string `json:"ownerVolume"`

	// claimRef identifies the PersistentVolumeClaim the owning lifecycle
	// provisions for.  A CreateVolume for any other claim (or claim UID) is
	// refused while the reservation exists; the refusal names this claim so
	// an operator can verify it is gone before deleting the reservation.
	// +optional
	ClaimRef *VolumeClaimRef `json:"claimRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=pvrs
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.ownerVolume`
// +kubebuilder:printcolumn:name="BackendVolume",type=string,JSONPath=`.spec.agentVolumeID`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.agentRef`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarVolumeReservation atomically binds a backend volume
// (agent + backend + agentVolumeID, or the native filesystem resource key)
// to one volume lifecycle. The CSI
// controller creates it before the owning PillarVolumeState with a
// deterministic name, so two concurrent CreateVolume claims on the same
// backend volume — for example two PVCs importing the same zvol — cannot both
// start a lifecycle: the loser sees AlreadyExists and is refused.  The
// reservation is deleted when the owning PillarVolumeState is removed.  It is
// never reclaimed automatically: one whose owner never wrote its record (the
// controller crashed in between) stays until an operator who verified the
// owning claim is gone deletes it with kubectl.
type PillarVolumeReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PillarVolumeReservationSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// PillarVolumeReservationList contains a list of PillarVolumeReservation.
type PillarVolumeReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PillarVolumeReservation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarVolumeReservation{}, &PillarVolumeReservationList{})
}
