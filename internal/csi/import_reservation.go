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
// finishDelete).  It is never reclaimed automatically: a contender cannot
// tell a crashed creator from one that is merely paused (a GC pause, a
// partitioned replica) and could still reach the agent, so a reservation
// whose owner never wrote its record stays until an operator who verified
// the owning claim is gone deletes it (the refusal names the exact command).
// Every delete carries UID and resourceVersion preconditions, so a stale
// read never removes a replacement reservation.
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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

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
// import lifecycle pvName, creating it when it does not exist.  A
// reservation held by any other lifecycle or claim is a FailedPrecondition
// naming the owner (see reservationOwnerCheck).  The claimRef argument
// records the owning claim (nil when the provisioner did not report one).
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
		return reservationOwnerCheck(res, pvName, dataset, claimRef)
	case !k8serrors.IsNotFound(err):
		return status.Errorf(codes.Internal,
			"get PillarVolumeReservation %q: %v", name, err)
	}
	res = &v1alpha1.PillarVolumeReservation{
		Name: name,
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
		// Lost the create race: refuse on what the winner's record says.
		return s.verifyReservation(ctx, pvName, agentName, backendType, agentVolID, dataset, claimRef)
	default:
		return status.Errorf(codes.Internal,
			"create PillarVolumeReservation %q: %v", name, err)
	}
}

// verifyReservation re-reads the reservation of the backend volume uncached
// and refuses unless the lifecycle pvName of claimRef holds it.  CreateVolume
// calls it immediately before ImportVolume, the first agent call of an
// import, so a reservation deleted by an operator and re-taken by another
// claim since this attempt reserved it stops the attempt before it can bind
// the zvol at the agent.
func (s *ControllerServer) verifyReservation(
	ctx context.Context,
	pvName, agentName, backendType, agentVolID, dataset string,
	claimRef *v1alpha1.VolumeClaimRef,
) error {
	name := reservationName(agentName, backendType, agentVolID)
	res := &v1alpha1.PillarVolumeReservation{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Name: name}, res)
	switch {
	case k8serrors.IsNotFound(err):
		return status.Errorf(codes.Aborted,
			"%s: zvol %q: PillarVolumeReservation %q of volume %q disappeared; retry the import",
			v1alpha1.AnnotationImportZvol, dataset, name, pvName)
	case err != nil:
		return status.Errorf(codes.Internal,
			"get PillarVolumeReservation %q: %v", name, err)
	}
	return reservationOwnerCheck(res, pvName, dataset, claimRef)
}

// reservationOwnerCheck accepts the reservation only when the lifecycle
// pvName holds it for the same claim: the recorded owner volume must match
// and, when both sides know the claim UID, so must the UID.  Anything else is
// refused with FailedPrecondition naming the owner and the command that
// releases the reservation once an operator verified its claim is gone.
func reservationOwnerCheck(
	res *v1alpha1.PillarVolumeReservation,
	pvName, dataset string,
	claimRef *v1alpha1.VolumeClaimRef,
) error {
	held := res.Spec.ClaimRef
	sameClaim := held == nil || claimRef == nil || held.UID == "" || claimRef.UID == "" ||
		held.UID == claimRef.UID
	if res.Spec.OwnerVolume == pvName && sameClaim {
		return nil
	}
	owner := "volume " + res.Spec.OwnerVolume
	if held != nil && held.Name != "" {
		owner += " of claim " + held.Namespace + "/" + held.Name
		if held.UID != "" {
			owner += " (uid " + held.UID + ")"
		}
	}
	return status.Errorf(codes.FailedPrecondition,
		"%s: zvol %q is reserved by %s (PillarVolumeReservation %q); delete that claim first, "+
			"or, after verifying that claim and its PillarVolumeState no longer exist, "+
			"release the reservation with `kubectl delete pillarvolumereservation %s`",
		v1alpha1.AnnotationImportZvol, dataset, owner, res.Name, res.Name)
}

// releaseBackendVolume drops the reservation of the backend volume encoded
// in volumeID — <agent>/<protocol>/<backend>/<agent-vol-id> — but only when
// it is still held by the ending lifecycle pvName: a reservation recorded
// for a different owner belongs to a later lifecycle and is left alone.  The
// delete is preconditioned on the UID and resourceVersion that were read, so
// a reservation replaced in between is never removed; a conflict is re-read
// and decided again on the next attempt.  A missing reservation is success
// (non-import volumes reserve nothing).
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
	uid, rv := res.UID, res.ResourceVersion
	err = s.k8sClient.Delete(ctx, res,
		ctrlclient.Preconditions{UID: &uid, ResourceVersion: &rv})
	switch {
	case err == nil, k8serrors.IsNotFound(err):
		return nil
	case k8serrors.IsConflict(err):
		return status.Errorf(codes.Aborted,
			"PillarVolumeReservation %q changed while volume %q released it; retry", name, pvName)
	default:
		return status.Errorf(codes.Internal,
			"delete PillarVolumeReservation %q: %v", name, err)
	}
}
