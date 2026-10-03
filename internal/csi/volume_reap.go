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

// Abandoned provisioning attempts.
//
// CreateVolume creates the PillarVolumeState before any agent call, so a
// failed attempt leaves one behind.  While the claim exists the provisioner
// retries CreateVolume with the same name and the record makes that retry
// idempotent.  Once the claim is removed before any PersistentVolume was
// created, no PersistentVolume ever calls DeleteVolume: nothing else ends
// that lifecycle.
//
// The lifecycle rules that make ending it safe:
//
//   - CreateVolume reports success only after the record is Ready, so a
//     lifecycle that is not Ready never had a PersistentVolume.  Only those
//     (Provisioning, CreatePartial) are reaped: a Ready volume is owned by
//     its PersistentVolume and reclaim policy, even after the
//     PersistentVolume object is gone (Retain).
//   - A claim that still exists, even terminating, may still be provisioned:
//     external-provisioner keeps retrying it.  Such an attempt is kept.
//   - Once the claim is gone, CreateVolume refuses to start or continue the
//     attempt with a final error (FailedPrecondition), which also ends the
//     provisioner's in-progress retries of the deleted claim.
//   - The phase does not tell whether the storage node holds anything: the
//     agent may have created the backend resource (and even the export) of an
//     attempt whose later CRD write or response was lost.  An abandoned
//     lifecycle is ended by the same fenced teardown as DeleteVolume before
//     its record is removed. Adopted filesystems use UnexportVolume followed
//     by ReleaseVolume, preserving the original source. Legacy adopted
//     zvols and dynamically created volumes retain destructive deletion.
//     Imports without durable adoption (status.importAcquired unset, no
//     backend device path or export info) use ReleaseVolume only; a lost
//     response must never justify destroying pre-existing data.
import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// provisionerVolumePrefix is external-provisioner's default volume name
// prefix; the volume, the PersistentVolume and the PillarVolumeState are named
// "<prefix>-<claim UID>".
const provisionerVolumePrefix = "pvc-"

// reapTeardownTimeout bounds the agent teardown of one abandoned volume.  The
// reaper runs on the PillarVolumeState reconciler's worker and holds the
// volume lock, so an agent that accepts the call but never answers must not
// stall export resync of every other volume.
const reapTeardownTimeout = 60 * time.Second

// claimRefFor returns the identity of the claim a first CreateVolume call for
// volume volumeName provisions for, from the claim name and namespace
// external-provisioner passes with --extra-create-metadata; found is false
// when they are absent.  The provisioner only provisions an existing claim,
// so its current UID is read back uncached.  A named claim that no longer
// exists, or whose UID differs from the one a default "pvc-<claim UID>"
// volume name encodes (the name was reused by a new claim), means the
// provisioning was abandoned: FailedPrecondition is returned.
func (s *ControllerServer) claimRefFor(
	ctx context.Context,
	volumeName string,
	params map[string]string,
) (ref v1alpha1.VolumeClaimRef, found bool, err error) {
	name, namespace := params[paramPVCNameMeta], params[paramPVCNamespaceMeta]
	if name == "" || namespace == "" {
		return ref, false, nil
	}
	claim := &corev1.PersistentVolumeClaim{}
	err = s.apiReader.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, claim)
	switch {
	case k8serrors.IsNotFound(err):
		return ref, false, status.Errorf(codes.FailedPrecondition,
			"PersistentVolumeClaim %s/%s of volume %q no longer exists; provisioning abandoned",
			namespace, name, volumeName)
	case err != nil:
		return ref, false, status.Errorf(codes.Internal, "get PersistentVolumeClaim %s/%s: %v", namespace, name, err)
	}
	nameUID := volumeClaimUID(&v1alpha1.PillarVolumeState{Name: volumeName})
	if nameUID != "" && nameUID != claim.UID {
		return ref, false, status.Errorf(codes.FailedPrecondition,
			"PersistentVolumeClaim %s/%s is now claim %s, not claim %s of volume %q; provisioning abandoned",
			namespace, name, claim.UID, nameUID, volumeName)
	}
	return v1alpha1.VolumeClaimRef{UID: string(claim.UID), Namespace: namespace, Name: name}, true, nil
}

// volumeClaimUID returns the UID of the claim pvs was provisioned for: the
// recorded claimRef, else the UID in a default "pvc-<claim UID>" name.  It
// returns "" when the claim cannot be identified; such a volume is never
// treated as abandoned.
func volumeClaimUID(pvs *v1alpha1.PillarVolumeState) types.UID {
	if ref := pvs.Spec.ClaimRef; ref != nil && ref.UID != "" {
		return types.UID(ref.UID)
	}
	suffix, ok := strings.CutPrefix(pvs.Name, provisionerVolumePrefix)
	if !ok {
		return ""
	}
	_, err := uuid.Parse(suffix)
	if err != nil {
		return ""
	}
	return types.UID(suffix)
}

// claimGone reports whether pvs is attributed to a claim and no claim with
// that UID exists any more (a terminating claim still exists).  It reads
// uncached: a stale informer cache could miss a just-created claim.
func (s *ControllerServer) claimGone(ctx context.Context, pvs *v1alpha1.PillarVolumeState) (bool, error) {
	claimUID := volumeClaimUID(pvs)
	if claimUID == "" {
		return false, nil
	}
	if ref := pvs.Spec.ClaimRef; ref != nil && ref.Name != "" && ref.Namespace != "" {
		claim := &corev1.PersistentVolumeClaim{}
		err := s.apiReader.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, claim)
		switch {
		case k8serrors.IsNotFound(err):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("get PersistentVolumeClaim %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		// A claim re-created under the same name is a different claim.
		return claim.UID != claimUID, nil
	}
	claims := &corev1.PersistentVolumeClaimList{}
	err := s.apiReader.List(ctx, claims)
	if err != nil {
		return false, fmt.Errorf("list PersistentVolumeClaims for volume %q: %w", pvs.Spec.VolumeID, err)
	}
	for i := range claims.Items {
		if claims.Items[i].UID == claimUID {
			return false, nil
		}
	}
	return true, nil
}

// refuseAbandonedClaim rejects a CreateVolume attempt whose claim is gone with
// FailedPrecondition, a final error for external-provisioner: no one can use
// the volume, and ending the provisioner's retries lets ReapAbandonedVolume
// end the lifecycle for good.
func (s *ControllerServer) refuseAbandonedClaim(ctx context.Context, attempt *v1alpha1.PillarVolumeState) error {
	gone, err := s.claimGone(ctx, attempt)
	if err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	if gone {
		return status.Errorf(codes.FailedPrecondition,
			"PersistentVolumeClaim %s of volume %q no longer exists; provisioning abandoned",
			volumeClaimUID(attempt), attempt.Spec.VolumeID)
	}
	return nil
}

// annotationSuccessRecorded marks a lifecycle created by a controller that
// records Ready before CreateVolume reports success.  Earlier controllers
// treated the Ready write as best-effort, so a lifecycle they left in
// CreatePartial may have been reported created and may have a (Retain)
// PersistentVolume history; only Provisioning is unambiguous for those.
const annotationSuccessRecorded = "pillar-csi.bhyoo.com/success-recorded"

// annotationValueTrue is the annotation value recording contract, kept as a
// constant so writers and readers cannot disagree.
const annotationValueTrue = "true"

// reapablePhase reports whether pvs never reported success, i.e. never had a
// PersistentVolume.  Provisioning precedes the backend record under every
// controller version; CreatePartial is conclusive only for lifecycles created
// under the success-recording contract.
func reapablePhase(pvs *v1alpha1.PillarVolumeState) bool {
	switch pvs.Status.Phase {
	case "", v1alpha1.PillarVolumeStatePhaseProvisioning:
		return true
	case v1alpha1.PillarVolumeStatePhaseCreatePartial:
		return pvs.Annotations[annotationSuccessRecorded] == annotationValueTrue
	default:
		return false
	}
}

// ReapAbandonedVolume ends the lifecycle of the named PillarVolumeState when
// its provisioning was abandoned: CreateVolume never reported it created
// (phase Provisioning or CreatePartial), it is attributed to a claim, that
// claim no longer exists (a terminating claim still exists), and no
// PersistentVolume refers to the volume.  It reports whether the lifecycle
// was ended.
//
// Every other volume is left alone and (false, nil) is returned.
//
// The teardown runs under the volume lock and commits status.deleting with a
// fresh fencing generation first, in a compare-and-swap that still requires
// the reapable phase, so an attempt that became Ready in between is kept.  A
// CreateVolume still in flight is then refused (it re-reads deleting, and
// persistVolumeReady refuses a lifecycle under deletion) or rejected by the
// agent as stale.  A failure keeps the record marked deleting and is returned
// so the caller retries; the retry repeats the idempotent steps with the same
// token.  A volume still recorded as published is refused and kept.
func (s *ControllerServer) ReapAbandonedVolume(ctx context.Context, pvsName string) (bool, error) {
	pvs, found, err := s.readVolumeState(ctx, pvsName)
	if err != nil || !found {
		return false, err
	}
	if scopedDriverForVolume(pvs) != s.effectiveDriverName() {
		return false, nil
	}
	volumeID := pvs.Spec.VolumeID
	unlock := s.volumeLocks.lock(volumeID)
	defer unlock()

	// Decide on the current record: another holder of the lock may have
	// deleted or re-created it.
	pvs, found, err = s.readVolumeState(ctx, pvsName)
	if err != nil || !found {
		return false, err
	}
	if scopedDriverForVolume(pvs) != s.effectiveDriverName() {
		return false, nil
	}
	abandoned, err := s.provisioningAbandoned(ctx, pvs)
	if err != nil || !abandoned {
		return false, err
	}

	// SP4 starts only for an abandoned lifecycle, so the periodic reconcile
	// of every other volume creates no span.
	ctx, span := telemetry.Tracer().Start(ctx, telemetry.SpanControllerVolumeReap,
		trace.WithNewRoot(), trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(telemetry.VolumeAttributes(volumeID)...))
	defer span.End()

	result, err := s.reapAbandoned(ctx, pvsName, pvs)
	span.SetAttributes(telemetry.KeyReapResult.String(result))
	volumesReaped.WithLabelValues(pvs.Spec.AgentRef, result).Inc()
	if result == reapResultError {
		telemetry.SetSpanError(span, err, "")
	}
	return result == reapResultReaped, err
}

// reapAbandoned tears down the abandoned lifecycle pvs (see
// ReapAbandonedVolume) and returns the pillar_csi.reap.result outcome. A kept
// outcome may still carry the refusal error for the caller to return.
func (s *ControllerServer) reapAbandoned(
	ctx context.Context,
	pvsName string,
	pvs *v1alpha1.PillarVolumeState,
) (string, error) {
	err := s.validateVolumeDriver(pvs)
	if err != nil {
		return reapResultKept, err
	}
	volumeID := pvs.Spec.VolumeID
	decidedUID := pvs.UID
	marked, fence, err := s.markVolumeDeleting(ctx, pvsName, volumeID, func(cur *v1alpha1.PillarVolumeState) error {
		if cur.UID != decidedUID || !reapablePhase(cur) {
			return status.Errorf(codes.Aborted,
				"volume %q changed to phase %q while being reaped", volumeID, cur.Status.Phase)
		}
		return nil
	})
	if err != nil {
		if status.Code(err) == codes.Aborted {
			return reapResultKept, err
		}
		return reapResultError, err
	}
	if marked == nil {
		return reapResultKept, nil
	}

	teardownCtx, cancel := context.WithTimeout(ctx, reapTeardownTimeout)
	defer cancel()
	var filesystemAdoption *agentv1.FilesystemAdoption
	if marked.Spec.FilesystemAdoption != nil {
		filesystemAdoption, err = filesystemAdoptionProto(marked.Spec.FilesystemAdoption)
		if err != nil {
			return reapResultError, fmt.Errorf(
				"convert abandoned filesystem adoption: %w", err)
		}
	}
	err = s.teardownMarkedVolume(teardownCtx, volumeTeardown{
		volumeID:           volumeID,
		pvName:             pvsName,
		uid:                marked.UID,
		targetName:         marked.Spec.AgentRef,
		protocolType:       mapProtocolType(marked.Spec.ProtocolType),
		backendType:        mapBackendType(marked.Spec.BackendType),
		agentVolID:         marked.Spec.AgentVolumeID,
		fence:              fence,
		releaseOnly:        importNeverAdopted(marked),
		filesystemAdoption: filesystemAdoption,
	})
	if err != nil {
		return reapResultError, fmt.Errorf("tear down abandoned volume %q: %w", volumeID, err)
	}
	return reapResultReaped, nil
}

// provisioningAbandoned reports whether pvs never reported success, its claim
// is gone, and no PersistentVolume refers to it.  The claim is checked first:
// once it is gone CreateVolume refuses the attempt, so no success (and no
// PersistentVolume) can follow the checks.  A lifecycle already marked
// deleting by an earlier reap resumes its teardown.
func (s *ControllerServer) provisioningAbandoned(
	ctx context.Context,
	pvs *v1alpha1.PillarVolumeState,
) (bool, error) {
	if scopedDriverForVolume(pvs) != s.effectiveDriverName() {
		return false, nil
	}
	if !reapablePhase(pvs) {
		return false, nil
	}
	gone, err := s.claimGone(ctx, pvs)
	if err != nil || !gone {
		return false, err
	}
	hasPV, err := s.persistentVolumeExists(ctx, pvs)
	if err != nil || hasPV {
		return false, err
	}
	return true, nil
}

// persistentVolumeExists reports whether a PersistentVolume refers to the
// volume: one named after it (external-provisioner names the
// PersistentVolume after the volume) or any whose CSI volume handle is its
// volume ID (a statically created PersistentVolume).
func (s *ControllerServer) persistentVolumeExists(
	ctx context.Context,
	pvs *v1alpha1.PillarVolumeState,
) (bool, error) {
	if scopedDriverForVolume(pvs) != s.effectiveDriverName() {
		return false, nil
	}
	pv := &corev1.PersistentVolume{}
	err := s.apiReader.Get(ctx, types.NamespacedName{Name: pvs.Name}, pv)
	if err == nil && pv.Spec.CSI != nil &&
		pv.Spec.CSI.Driver == s.effectiveDriverName() &&
		pv.Spec.CSI.VolumeHandle == pvs.Spec.VolumeID {
		return true, nil
	}
	if err != nil && !k8serrors.IsNotFound(err) {
		return false, fmt.Errorf("get PersistentVolume %q: %w", pvs.Name, err)
	}

	pvList := &corev1.PersistentVolumeList{}
	err = s.apiReader.List(ctx, pvList)
	if err != nil {
		return false, fmt.Errorf("list PersistentVolumes for volume %q: %w", pvs.Spec.VolumeID, err)
	}
	for i := range pvList.Items {
		src := pvList.Items[i].Spec.CSI
		if src != nil && src.Driver == s.effectiveDriverName() && src.VolumeHandle == pvs.Spec.VolumeID {
			return true, nil
		}
	}
	return false, nil
}
