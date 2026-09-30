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

package csi

// Atomic backend-volume reservations (issue #146, review): two PVCs that
// import the same zvol can both pass the List-before-create ownership scan —
// the checks are not atomic, and controller replicas do not share a cache.
// Before its PillarVolumeState exists, a CreateVolume for an import creates a
// PillarVolumeReservation whose name deterministically encodes
// (agent, backend type, agent volume ID).  The API server's single write wins:
// the loser gets AlreadyExists and is refused with FailedPrecondition naming
// the recorded owner, so no second lifecycle ever reaches the agent for the
// same backend volume.
//
// The reservation is held for the whole lifecycle — refused or failed import
// retries keep it, because the claim still intends to import — and is
// released only when the owning PillarVolumeState is removed (see
// finishDelete).  A reservation whose owner was never written and whose
// recorded claim no longer exists is orphaned (the controller crashed between
// the two creates) and may be claimed by a new contender.
//
// Reservations are read and compared through the uncached apiReader like the
// PillarVolumeState scans: a stale informer copy could hide a reservation
// committed moments ago on another replica.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// reservationNamePrefix keeps reservation objects visually distinct from the
// "pvc-" PillarVolumeState names in listings and logs.
const reservationNamePrefix = "rsv-"

// reservationName returns the deterministic name of the reservation binding
// the backend volume (agent, backendType, agentVolID): the API server makes
// the create atomic, so every contender for the same backend volume races on
// the same object name.
func reservationName(agentName, backendType, agentVolID string) string {
	sum := sha256.Sum256([]byte(agentName + "\x00" + backendType + "\x00" + agentVolID))
	return reservationNamePrefix + hex.EncodeToString(sum[:16])
}

// reserveBackendVolume takes the reservation for the backend volume of the
// import lifecycle pvName, creating it when it does not exist and claiming an
// orphaned one when its owner record was never written.  A reservation held
// by a different lifecycle is a FailedPrecondition naming the owner; the
// claim retries until that lifecycle ends or is reaped.  The claimRef
// argument records the owning claim so an orphan can be recognized (nil when
// the provisioner did not report one).
func (s *ControllerServer) reserveBackendVolume(
	ctx context.Context,
	pvName, agentName, backendType, agentVolID, dataset string,
	claimRef *v1alpha1.VolumeClaimRef,
) error {
	name := reservationName(agentName, backendType, agentVolID)
	res := &v1alpha1.PillarVolumeReservation{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Name: name}, res)
	switch {
	case err == nil:
		return s.checkReservationOwner(ctx, res, pvName, dataset)
	case !k8serrors.IsNotFound(err):
		return status.Errorf(codes.Internal,
			"get PillarVolumeReservation %q: %v", name, err)
	}
	res = &v1alpha1.PillarVolumeReservation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PillarVolumeReservationSpec{
			AgentRef:      agentName,
			BackendType:   backendType,
			AgentVolumeID: agentVolID,
			OwnerVolume:   pvName,
			ClaimRef:      claimRef,
		},
	}
	err = s.k8sClient.Create(ctx, res)
	switch {
	case err == nil:
		return nil
	case k8serrors.IsAlreadyExists(err):
		// Lost the create race: re-read the winner's record uncached and
		// refuse (or claim it) on what it actually says.
		held := &v1alpha1.PillarVolumeReservation{}
		getErr := s.uncachedReader().Get(ctx, types.NamespacedName{Name: name}, held)
		if getErr != nil {
			return status.Errorf(codes.Internal,
				"get PillarVolumeReservation %q: %v", name, getErr)
		}
		return s.checkReservationOwner(ctx, held, pvName, dataset)
	default:
		return status.Errorf(codes.Internal,
			"create PillarVolumeReservation %q: %v", name, err)
	}
}

// checkReservationOwner accepts the reservation when this lifecycle owns it
// or when it is orphaned; otherwise it refuses the import with
// FailedPrecondition naming the recorded owner.
func (s *ControllerServer) checkReservationOwner(
	ctx context.Context,
	res *v1alpha1.PillarVolumeReservation,
	pvName, dataset string,
) error {
	owner := res.Spec.OwnerVolume
	if owner == pvName {
		return nil
	}
	orphaned, err := s.reservationOrphaned(ctx, res)
	if err != nil {
		return err
	}
	if !orphaned {
		return status.Errorf(codes.FailedPrecondition,
			"%s: zvol %q is reserved by volume %q (PillarVolumeReservation %q); "+
				"delete that volume first",
			v1alpha1.AnnotationImportZvol, dataset, owner, res.Name)
	}
	// The reservation's owner never completed: delete the stale record so the
	// next attempt re-creates it.  Delete is preconditions-free on purpose —
	// any contender deletes the same orphan and only one re-create wins.
	err = s.k8sClient.Delete(ctx, res)
	if err != nil && !k8serrors.IsNotFound(err) {
		return status.Errorf(codes.Internal,
			"delete orphaned PillarVolumeReservation %q: %v", res.Name, err)
	}
	return status.Errorf(codes.Unavailable,
		"%s: zvol %q reservation of abandoned volume %q released; retry the import",
		v1alpha1.AnnotationImportZvol, dataset, owner)
}

// reservationOrphaned reports whether the reservation's owning lifecycle is
// gone for good: the owner PillarVolumeState does not exist — either it was
// never created (the reservation is created first) or it was already
// retired — and the recorded claim, when any, no longer exists.
func (s *ControllerServer) reservationOrphaned(
	ctx context.Context,
	res *v1alpha1.PillarVolumeReservation,
) (bool, error) {
	_, exists, err := s.readVolumeState(ctx, res.Spec.OwnerVolume)
	if err != nil {
		return false, status.Errorf(codes.Internal, "%v", err)
	}
	if exists {
		return false, nil
	}
	ref := res.Spec.ClaimRef
	if ref == nil || ref.Name == "" || ref.Namespace == "" {
		// No recorded claim: the owner record is the only trace, and it is
		// gone.  A claim-less reservation outlives nothing else.
		return true, nil
	}
	claim := &corev1.PersistentVolumeClaim{}
	getErr := s.uncachedReader().Get(ctx,
		types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, claim)
	switch {
	case k8serrors.IsNotFound(getErr):
		return true, nil
	case getErr != nil:
		return false, status.Errorf(codes.Internal,
			"get PersistentVolumeClaim %s/%s: %v", ref.Namespace, ref.Name, getErr)
	}
	// A claim re-created under the same name is a different claim; when no
	// UID was recorded the named claim's existence still holds the
	// reservation.
	if ref.UID != "" && claim.UID != types.UID(ref.UID) {
		return true, nil
	}
	return false, nil
}

// releaseBackendVolume drops the reservation of the backend volume encoded
// in volumeID — <agent>/<protocol>/<backend>/<agent-vol-id> — but only when
// it is still held by the ending lifecycle pvName: a reservation recorded
// for a different owner belongs to a later lifecycle and is left alone.  A
// missing reservation is success (non-import volumes reserve nothing).
func (s *ControllerServer) releaseBackendVolume(
	ctx context.Context,
	pvName, volumeID string,
) error {
	fields := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(fields) != volumeIDParts {
		return nil
	}
	name := reservationName(fields[0], fields[2], fields[3])
	res := &v1alpha1.PillarVolumeReservation{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Name: name}, res)
	switch {
	case k8serrors.IsNotFound(err):
		return nil
	case err != nil:
		return status.Errorf(codes.Internal,
			"get PillarVolumeReservation %q: %v", name, err)
	}
	if res.Spec.OwnerVolume != pvName {
		return nil // a later lifecycle already holds the reservation
	}
	err = s.k8sClient.Delete(ctx, res)
	if err != nil && !k8serrors.IsNotFound(err) {
		return status.Errorf(codes.Internal,
			"delete PillarVolumeReservation %q: %v", name, err)
	}
	return nil
}
