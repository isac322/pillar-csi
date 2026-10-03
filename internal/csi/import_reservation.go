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
// (agent, backend type, agent volume ID), or (agent, canonical native resource
// key) for adopted filesystems regardless of logical pool aliases. The API
// server's single write wins: the loser gets AlreadyExists and is refused
// with FailedPrecondition naming the recorded owner.
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
// the backend volume. A supplied native resource key replaces backend type
// and routing ID, so aliases in different logical pools race on one object.
// Without it the legacy zvol reservation name remains unchanged.
func reservationName(agentName, backendType, agentVolID string, resourceID ...string) string {
	if len(resourceID) > 0 {
		backendType, agentVolID = "filesystem", resourceID[0]
	}
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
	resourceID ...string,
) error {
	if len(resourceID) > 1 || (len(resourceID) == 1 && resourceID[0] == "") {
		return importInvalid("filesystem reservation requires one non-empty canonical resource ID")
	}
	name := reservationName(agentName, backendType, agentVolID, resourceID...)
	res := &v1alpha1.PillarVolumeReservation{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Name: name}, res)
	switch {
	case err == nil:
		if len(resourceID) == 1 && res.Spec.FilesystemResourceID != resourceID[0] {
			return status.Errorf(
				codes.FailedPrecondition, "PillarVolumeReservation %q has a different native filesystem resource", name,
			)
		}
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
	if len(resourceID) == 1 {
		res.Spec.FilesystemResourceID = resourceID[0]
	}
	err = s.k8sClient.Create(ctx, res)
	switch {
	case err == nil:
		return nil
	case k8serrors.IsAlreadyExists(err):
		// Lost the create race: refuse on what the winner's record says.
		return s.verifyReservation(ctx, pvName, agentName, backendType, agentVolID, dataset, claimRef, resourceID...)
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
	resourceID ...string,
) error {
	if len(resourceID) > 1 || (len(resourceID) == 1 && resourceID[0] == "") {
		return importInvalid("filesystem reservation requires one non-empty canonical resource ID")
	}
	name := reservationName(agentName, backendType, agentVolID, resourceID...)
	res := &v1alpha1.PillarVolumeReservation{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Name: name}, res)
	switch {
	case k8serrors.IsNotFound(err):
		if len(resourceID) == 1 {
			return status.Errorf(codes.Aborted,
				"filesystem source %q: PillarVolumeReservation %q of volume %q disappeared; retry the import",
				dataset, name, pvName)
		}
		return status.Errorf(codes.Aborted,
			"%s: zvol %q: PillarVolumeReservation %q of volume %q disappeared; retry the import",
			v1alpha1.AnnotationImportZvol, dataset, name, pvName)
	case err != nil:
		return status.Errorf(codes.Internal,
			"get PillarVolumeReservation %q: %v", name, err)
	}
	if len(resourceID) == 1 && res.Spec.FilesystemResourceID != resourceID[0] {
		return status.Errorf(
			codes.FailedPrecondition, "PillarVolumeReservation %q has a different native filesystem resource", name,
		)
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
	if res.Spec.FilesystemResourceID != "" {
		return status.Errorf(codes.FailedPrecondition,
			"filesystem source %q is reserved by %s (PillarVolumeReservation %q); delete that claim first, "+
				"or, after verifying that claim and its PillarVolumeState no longer exist, "+
				"release the reservation with `kubectl delete pillarvolumereservation %s`",
			dataset, owner, res.Name, res.Name)
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
	expectedUID types.UID,
) error {
	fields := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(fields) != volumeIDParts {
		return s.releaseInvalidBackendVolumeID(pvName)
	}
	name := reservationName(fields[0], fields[2], fields[3])
	return s.releaseBackendVolumeReservation(ctx, pvName, volumeID, expectedUID, name)
}

func (s *ControllerServer) releaseInvalidBackendVolumeID(pvName string) error {
	if s.effectiveDriverName() == v1alpha1.FileCSIDriver {
		return status.Errorf(
			codes.Aborted,
			"file lifecycle %q has invalid routing ID before reservation cleanup", pvName)
	}
	return nil
}

func (s *ControllerServer) releaseBackendVolumeReservation(
	ctx context.Context,
	pvName, volumeID string,
	expectedUID types.UID,
	name string,
) error {
	// The native key is not encoded in the routing ID; read the lifecycle
	// before deletion so alias pools release the same atomic reservation.
	pvs := &v1alpha1.PillarVolumeState{}
	readErr := s.uncachedReader().Get(ctx, types.NamespacedName{Name: pvName}, pvs)
	if readErr != nil && !k8serrors.IsNotFound(readErr) {
		return status.Errorf(
			codes.Internal, "get PillarVolumeState %q for reservation release: %v", pvName, readErr)
	}
	if readErr != nil && s.effectiveDriverName() == v1alpha1.FileCSIDriver {
		return status.Errorf(
			codes.Aborted, "file lifecycle %q disappeared before reservation cleanup", pvName)
	}
	if readErr == nil && s.effectiveDriverName() == v1alpha1.FileCSIDriver &&
		pvs.Spec.FilesystemAdoption == nil {
		return status.Errorf(
			codes.Aborted,
			"file lifecycle %q was replaced by a non-file lifecycle before reservation cleanup",
			pvName)
	}
	if readErr == nil && pvs.Spec.FilesystemAdoption != nil {
		err := validateFilesystemAdoption(pvs.Spec.FilesystemAdoption, pvs.Spec.Resolved)
		if err != nil {
			return status.Errorf(
				codes.FailedPrecondition,
				"volume %q has invalid recorded filesystem identity: %v", pvName, err)
		}
		return s.releaseFilesystemReservations(ctx, pvs, volumeID, expectedUID)
	}
	return s.releaseLegacyReservation(ctx, pvName, name)
}

func (s *ControllerServer) releaseLegacyReservation(
	ctx context.Context, pvName, name string,
) error {
	res := &v1alpha1.PillarVolumeReservation{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Name: name}, res)
	switch {
	case k8serrors.IsNotFound(err):
		return nil
	case err != nil:
		return status.Errorf(
			codes.Internal, "get PillarVolumeReservation %q: %v", name, err)
	}
	if res.Spec.OwnerVolume != pvName {
		return nil
	}
	uid, rv := res.UID, res.ResourceVersion
	err = s.k8sClient.Delete(
		ctx, res, ctrlclient.Preconditions{UID: &uid, ResourceVersion: &rv})
	switch {
	case err == nil, k8serrors.IsNotFound(err):
		return nil
	case k8serrors.IsConflict(err):
		return status.Errorf(
			codes.Aborted,
			"PillarVolumeReservation %q changed while volume %q released it; retry",
			name, pvName)
	default:
		return status.Errorf(
			codes.Internal, "delete PillarVolumeReservation %q: %v", name, err)
	}
}

// First-attempt inspection can leave more than one native reservation for the
// same claim when concurrent creates race to persist different descriptors.
// Only retirement of their exact deleting lifecycle may collect those losing
// candidates. Active owners and unknown claim UIDs require operator handling.
func (s *ControllerServer) releaseFilesystemReservations(
	ctx context.Context, owner *v1alpha1.PillarVolumeState, volumeID string, expectedUID types.UID,
) error {
	err := validateFilesystemReservationCleanup(owner, volumeID, expectedUID)
	if err != nil {
		return err
	}
	err = s.validateVolumeDriver(owner)
	if err != nil {
		return err
	}
	var list v1alpha1.PillarVolumeReservationList
	err = s.uncachedReader().List(ctx, &list)
	if err != nil {
		return status.Errorf(
			codes.Internal,
			"list native reservations for file lifecycle %q: %v", owner.Name, err)
	}
	for i := range list.Items {
		err = s.cleanupFilesystemReservation(ctx, owner, &list.Items[i])
		if err != nil {
			return err
		}
	}
	return s.verifyFilesystemCleanupOwner(ctx, owner)
}

func validateFilesystemReservationCleanup(
	owner *v1alpha1.PillarVolumeState,
	volumeID string,
	expectedUID types.UID,
) error {
	if expectedUID == "" || owner.UID != expectedUID ||
		!owner.Status.Deleting || owner.Spec.VolumeID != volumeID ||
		len(owner.Status.PublishedNodes) != 0 {
		return status.Errorf(
			codes.Aborted,
			"file lifecycle %q changed or is not deleting; reservation cleanup refused",
			owner.Name)
	}
	if owner.Spec.ClaimRef == nil || owner.Spec.ClaimRef.UID == "" {
		return status.Errorf(
			codes.FailedPrecondition,
			"file lifecycle %q has no recorded claim UID; reservation cleanup requires operator verification",
			owner.Name)
	}
	if owner.Spec.AgentRef == "" || owner.Spec.BackendType == "" ||
		owner.Spec.Resolved == nil ||
		owner.Spec.BackendType != string(owner.Spec.Resolved.Backend.Kind()) {
		return status.Errorf(
			codes.FailedPrecondition,
			"file lifecycle %q has incomplete reservation routing scope", owner.Name)
	}
	return nil
}

func (s *ControllerServer) cleanupFilesystemReservation(
	ctx context.Context,
	owner *v1alpha1.PillarVolumeState,
	res *v1alpha1.PillarVolumeReservation,
) error {
	if res.Spec.FilesystemResourceID == "" || res.Spec.OwnerVolume != owner.Name ||
		res.Spec.AgentRef != owner.Spec.AgentRef ||
		res.Spec.BackendType != owner.Spec.BackendType ||
		res.Spec.ClaimRef == nil || res.Spec.ClaimRef.UID != owner.Spec.ClaimRef.UID {
		return nil
	}
	if !canonicalFilesystemReservationKey(res.Spec.FilesystemResourceID) ||
		res.Name != reservationName(
			res.Spec.AgentRef, res.Spec.BackendType, res.Spec.AgentVolumeID,
			res.Spec.FilesystemResourceID) {
		return status.Errorf(
			codes.FailedPrecondition,
			"native reservation %q has invalid canonical identity; cleanup requires operator verification",
			res.Name)
	}
	err := s.verifyFilesystemCleanupOwner(ctx, owner)
	if err != nil {
		return err
	}
	uid, rv := res.UID, res.ResourceVersion
	if uid == "" || rv == "" {
		return status.Errorf(
			codes.FailedPrecondition,
			"native reservation %q lacks deletion preconditions", res.Name)
	}
	err = s.k8sClient.Delete(
		ctx, res, ctrlclient.Preconditions{UID: &uid, ResourceVersion: &rv})
	switch {
	case err == nil, k8serrors.IsNotFound(err):
		return nil
	case k8serrors.IsConflict(err):
		return status.Errorf(
			codes.Aborted, "native reservation %q changed during cleanup; retry", res.Name)
	default:
		return status.Errorf(
			codes.Internal, "delete native reservation %q: %v", res.Name, err)
	}
}

func (s *ControllerServer) verifyFilesystemCleanupOwner(ctx context.Context, owner *v1alpha1.PillarVolumeState) error {
	current := &v1alpha1.PillarVolumeState{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Name: owner.Name}, current)
	if k8serrors.IsNotFound(err) {
		return status.Errorf(codes.Aborted, "file lifecycle %q disappeared during reservation cleanup", owner.Name)
	}
	if err != nil {
		return status.Errorf(codes.Internal, "read file lifecycle %q during reservation cleanup: %v", owner.Name, err)
	}
	if current.UID != owner.UID || !current.Status.Deleting || current.Spec.ClaimRef == nil ||
		current.Spec.ClaimRef.UID != owner.Spec.ClaimRef.UID || current.Spec.AgentRef != owner.Spec.AgentRef ||
		current.Spec.BackendType != owner.Spec.BackendType || current.Spec.VolumeID != owner.Spec.VolumeID ||
		current.Spec.FilesystemAdoption == nil || *current.Spec.FilesystemAdoption != *owner.Spec.FilesystemAdoption ||
		len(current.Status.PublishedNodes) != 0 {
		return status.Errorf(codes.Aborted, "file lifecycle %q changed or is live during reservation cleanup", owner.Name)
	}
	return nil
}

func canonicalFilesystemReservationKey(key string) bool {
	const prefix = "filesystem/"
	if len(key) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(key, prefix) {
		return false
	}
	for i := len(prefix); i < len(key); i++ {
		c := key[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
