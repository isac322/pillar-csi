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

// Ownership recovery (spec.recovery, phase RecoveryPending).
//
// A volume record created by the operator with spec.recovery set exists only
// to receive an operator-authorized ownership transfer: the retired
// lifecycle's adopted LVM logical volume is transferred to this record's
// lifecycle by agent.TransferVolumeOwnership.  The record is non-serving —
// publish, expand and delete refuse it — and non-reapable until the agent
// commits the transfer, the controller exports the volume and persists
// phase Ready.  The retained-PV rebind runbook is a different flow: it keeps
// the same PillarVolumeState and never touches spec.recovery.
//
// The destination lifecycle is this record's metadata.uid; the operator
// cannot know it at create time, so newVolumeUID, authorization and
// authorizationDigest are write-once fields populated after the record's UID
// is read and the grant signed.  Every CreateVolume attempt for the record's
// claim drives the same idempotent steps:
//
//  1. adopt: verify the intent is consistent, fill newVolumeUID/spec.resolved
//     when still empty, and commit phase RecoveryPending plus the declared
//     fencing generation — the record's first status write.
//  2. inspect: agent.InspectVolume returns the live mark.  A mark that
//     already records this lifecycle (uid + exact generation + source +
//     retired old uid) means the transfer committed earlier; no
//     authorization is needed to continue.
//  3. authorize: the serialized RecoveryAuthorization must digest to
//     spec.authorizationDigest and pin this lifecycle (uid, generation),
//     the retired lifecycle (uid, generation) and the source exactly.
//  4. snapshot: the operator supplies the agent-signed RecoverySnapshot it
//     took from InspectVolume (annotationRecoverySnapshot, standard base64)
//     together with the grant signed over its digest; the controller never
//     substitutes its own observation, which a grant signed earlier cannot name.
//  5. transfer: agent.TransferVolumeOwnership; UNKNOWN is retried once with
//     the identical request, then reported Unavailable for the next
//     CreateVolume retry — never rolled back, never re-signed.
//  6. persist CreatePartial (device path + export spec) so a retry only
//     re-exports, then the shared export step and persistVolumeReady run as
//     in every CreateVolume.  Ready is recorded only after all of this —
//     the phase never leaves RecoveryPending before the transfer committed.

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	"github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/recoveryauth"
)

// annotationRecoverySnapshot carries the agent-signed RecoverySnapshot the
// operator's authorization names: the standard base64 encoding of its
// deterministic protobuf bytes (annotations are UTF-8 strings, so raw proto
// bytes would be corrupted in transit).  The operator sets it in the same
// patch as spec.recovery.authorization/authorizationDigest; every attempt,
// including retries after an outcome-unknown transfer, presents exactly
// these bytes.  A tampered value no longer matches the grant's
// snapshot_digest and is refused.
const annotationRecoverySnapshot = "pillar-csi.bhyoo.com/recovery-snapshot"

// recoveryPending reports whether pvs is a recovery record whose transfer
// has not completed: spec.recovery is set and phase Ready has not been
// persisted yet.  Every status before Ready — none, RecoveryPending,
// CreatePartial — counts as pending.
func recoveryPending(pvs *v1alpha1.PillarVolumeState) bool {
	return pvs.Spec.Recovery != nil && pvs.Status.ExportInfo == nil
}

// refuseRecoveryPending rejects serving operations on a recovery record
// whose transfer has not completed.  The record is intentionally
// non-serving: it owns nothing until TransferVolumeOwnership commits and
// persistVolumeReady records phase Ready.
func refuseRecoveryPending(pvs *v1alpha1.PillarVolumeState, volumeID string) error {
	if !recoveryPending(pvs) {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition,
		"volume %q is a recovery record (phase %q) awaiting an authorized ownership "+
			"transfer; it cannot be published, expanded or deleted until the transfer "+
			"commits and the volume is Ready", volumeID, pvs.Status.Phase)
}

// recoveryPreservesOriginal reports whether pvs is a recovery record whose
// declared policy preserves the original LV.
func recoveryPreservesOriginal(pvs *v1alpha1.PillarVolumeState) bool {
	r := pvs.Spec.Recovery
	return r != nil && r.Source != nil && r.Source.PreserveOriginal
}

// volumePreservesOriginal reports whether the lifecycle's LV data is
// promised untouched: an import-lv adoption under PreserveOriginal or a
// recovery record preserving the original.
func volumePreservesOriginal(pvs *v1alpha1.PillarVolumeState) bool {
	return preservesOriginal(pvs) || recoveryPreservesOriginal(pvs)
}

// refuseRecoveryExpand refuses expansion of a recovery record.  While the
// transfer is pending every serving operation is refused; once the record
// is Ready the preserve policy of the recovered LV decides, exactly like an
// import-lv adoption.
func refuseRecoveryExpand(pvs *v1alpha1.PillarVolumeState, volumeID string) error {
	if pvs.Spec.Recovery == nil {
		return nil
	}
	err := refuseRecoveryPending(pvs, volumeID)
	if err != nil {
		return err
	}
	if recoveryPreservesOriginal(pvs) {
		return status.Errorf(codes.FailedPrecondition,
			"cannot expand volume %q: its recovery adopted LV %q under policy %s, "+
				"which never resizes the original",
			volumeID, lvmSourceLocator(pvs.Spec.Recovery.Source), v1alpha1.ImportLVPolicyPreserveOriginal)
	}
	return nil
}

// addVolumePreserveContext marks the PV volumeContext when the lifecycle
// preserves the original LV (import-lv adoption or recovery), so the node
// mounts the existing filesystem without formatting.
func addVolumePreserveContext(pvs *v1alpha1.PillarVolumeState, volCtx map[string]string) {
	addPreserveVolumeContext(pvs, volCtx)
	if recoveryPreservesOriginal(pvs) {
		volCtx[VolumeContextKeyPreserveOriginal] = annotationValueTrue
	}
}

// lvmSourceMatches reports whether the agent-side LVM identity ident pins
// the same logical volume as the record's source: same names and UUIDs.
func lvmSourceMatches(ident *agentv1.LvmSourceIdentity, src *v1alpha1.LVMSourceRef) bool {
	if ident == nil || src == nil {
		return ident == nil && src == nil
	}
	return ident.GetVolumeGroup() == src.VolumeGroup &&
		ident.GetLogicalVolume() == src.LogicalVolume &&
		ident.GetVolumeGroupUuid() == src.VolumeGroupUUID &&
		ident.GetLogicalVolumeUuid() == src.LogicalVolumeUUID
}

// recoverySpecDrift refuses the attempt when the claim's resolved routing
// disagrees with the operator-authored record: the record's immutable spec
// is authoritative, so a claim that would route elsewhere is rejected
// rather than served.
func recoverySpecDrift(
	pvs *v1alpha1.PillarVolumeState,
	volumeID, agentVolID, targetName, protocolID, backendID string,
	backendType agentv1.BackendType,
) error {
	if pvs.Spec.VolumeID != volumeID || pvs.Spec.AgentVolumeID != agentVolID {
		return status.Errorf(codes.FailedPrecondition,
			"PillarVolumeState %q declares volume %q (agent volume %q) but the claim "+
				"resolves to volume %q (agent volume %q): the record is authoritative; "+
				"fix the claim's configuration to match the record",
			pvs.Name, pvs.Spec.VolumeID, pvs.Spec.AgentVolumeID, volumeID, agentVolID)
	}
	if pvs.Spec.AgentRef != targetName ||
		pvs.Spec.ProtocolType != protocolID ||
		pvs.Spec.BackendType != backendID {
		return status.Errorf(codes.FailedPrecondition,
			"PillarVolumeState %q declares agent %q, protocol %q, backend %q but the "+
				"claim resolves to agent %q, protocol %q, backend %q: the record is "+
				"authoritative; fix the claim's configuration to match the record",
			pvs.Name, pvs.Spec.AgentRef, pvs.Spec.ProtocolType, pvs.Spec.BackendType,
			targetName, protocolID, backendID)
	}
	if backendType != agentv1.BackendType_BACKEND_TYPE_LVM {
		return status.Errorf(codes.FailedPrecondition,
			"cannot recover volume %q: ownership transfer is supported only for LVM "+
				"volumes, record declares backend %q", volumeID, backendID)
	}
	return nil
}

// adoptRecoveryIntent commits the first status write of a recovery record:
// phase RecoveryPending and the declared fencing generation.  In the same
// step it fills the write-once spec fields the controller can supply —
// newVolumeUID (the record's own metadata.uid) and spec.resolved — so the
// operator only ever patches the authorization it signs.  An intent that
// names another lifecycle, or a record whose fencing generation already
// advanced past the declared one, is refused fail-closed.
func (s *ControllerServer) adoptRecoveryIntent(
	ctx context.Context,
	pvName string,
	uid types.UID,
	resolved *v1alpha1.ResolvedVolumeConfig,
) error {
	// Fill the write-once spec fields first.  The update is pinned to the
	// lifecycle UID: a record re-created under the same name is refused by
	// the conflict retry's re-read.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvs, exists, getErr := s.readVolumeState(ctx, pvName)
		if getErr != nil {
			return status.Errorf(codes.Internal, "%v", getErr)
		}
		if !exists || pvs.UID != uid {
			return status.Errorf(codes.Aborted,
				"PillarVolumeState %q (uid %s) no longer exists; the volume was deleted or re-created",
				pvName, uid)
		}
		changed, fillErr := fillRecoveryWriteOnce(pvName, pvs, uid, resolved)
		if fillErr != nil || !changed {
			return fillErr
		}
		return s.k8sClient.Update(ctx, pvs)
	})
	if err != nil {
		return publicationRecordError("adopt recovery intent", pvName, "", err)
	}

	_, err = s.updateVolumeState(ctx, pvName, uid, false, func(pvs *v1alpha1.PillarVolumeState) error {
		return adoptRecoveryPhase(pvName, pvs)
	})
	return err
}

// fillRecoveryWriteOnce fills the write-once spec fields the controller can
// supply — newVolumeUID and spec.resolved — and reports whether pvs changed.
// An intent that names another destination lifecycle is refused.
func fillRecoveryWriteOnce(
	pvName string,
	pvs *v1alpha1.PillarVolumeState,
	uid types.UID,
	resolved *v1alpha1.ResolvedVolumeConfig,
) (bool, error) {
	intent := pvs.Spec.Recovery
	if intent == nil {
		return false, status.Errorf(codes.FailedPrecondition,
			"PillarVolumeState %q has no recovery intent", pvName)
	}
	if intent.NewVolumeUID != "" && intent.NewVolumeUID != string(uid) {
		return false, status.Errorf(codes.FailedPrecondition,
			"recovery intent of PillarVolumeState %q declares destination uid %q, "+
				"but this record's uid is %s: the intent belongs to a different lifecycle",
			pvName, intent.NewVolumeUID, uid)
	}
	changed := false
	if intent.NewVolumeUID == "" {
		intent.NewVolumeUID = string(uid)
		changed = true
	}
	if pvs.Spec.Resolved == nil && resolved != nil {
		pvs.Spec.Resolved = resolved
		changed = true
	}
	return changed, nil
}

// adoptRecoveryPhase is the status mutation of adoptRecoveryIntent: it
// commits phase RecoveryPending and the declared fencing generation, or
// reports errNoStatusChange when an earlier attempt already did.
func adoptRecoveryPhase(pvName string, pvs *v1alpha1.PillarVolumeState) error {
	intent := pvs.Spec.Recovery
	if intent == nil {
		return status.Errorf(codes.FailedPrecondition,
			"PillarVolumeState %q has no recovery intent", pvName)
	}
	if pvs.Status.Deleting {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q is under deletion", pvName)
	}
	switch pvs.Status.Phase {
	case "":
		// First controller status write of this record.
	case v1alpha1.PillarVolumeStatePhaseRecoveryPending:
		// Adoption committed on an earlier attempt.
	case v1alpha1.PillarVolumeStatePhaseCreatePartial:
		// The transfer committed on an earlier attempt and its partial
		// state is durable; the export step runs without adopting again.
		return errNoStatusChange
	default:
		return status.Errorf(codes.FailedPrecondition,
			"PillarVolumeState %q is a recovery record in unexpected phase %q",
			pvName, pvs.Status.Phase)
	}
	if pvs.Status.PublicationGeneration > intent.NewGeneration {
		// A generation newer than the one the grant declares means the
		// record already took fencing operations this transfer cannot
		// precede; the declared generation can never be committed again.
		return status.Errorf(codes.FailedPrecondition,
			"PillarVolumeState %q records fencing generation %d newer than the "+
				"recovery intent's newGeneration %d; the declared transfer can "+
				"never be replayed — re-issue the recovery record",
			pvName, pvs.Status.PublicationGeneration, intent.NewGeneration)
	}
	if pvs.Status.Phase == v1alpha1.PillarVolumeStatePhaseRecoveryPending &&
		pvs.Status.PublicationGeneration == intent.NewGeneration {
		return errNoStatusChange
	}
	pvs.Status.Phase = v1alpha1.PillarVolumeStatePhaseRecoveryPending
	// The grant names the generation the transferred mark records;
	// fencing operations of this lifecycle (the export's generation
	// bump) must issue strictly newer generations.
	pvs.Status.PublicationGeneration = intent.NewGeneration
	return nil
}

// recoveryAuthorization decodes and validates the operator-signed grant
// declared by spec.recovery: the serialized authorization must exist,
// digest to spec.authorizationDigest, and pin exactly this lifecycle
// (new uid = metadata.uid, new generation), the retired lifecycle
// (old uid + exact generation) and the declared source.  The signature
// itself is verified by the agent under its configured trust anchor; the
// controller's job is to never issue a transfer the record does not pin.
// A grant not yet populated is Unavailable — the provisioner retries — a
// populated grant that disagrees with the record is FailedPrecondition.
func recoveryAuthorization(
	pvName string,
	pvs *v1alpha1.PillarVolumeState,
) (*agentv1.RecoveryAuthorization, error) {
	intent := pvs.Spec.Recovery
	if len(intent.Authorization) == 0 {
		if intent.AuthorizationDigest == "" {
			return nil, status.Errorf(codes.Unavailable,
				"recovery record %q awaits the operator's authorization: "+
					"spec.recovery.authorization and authorizationDigest are not populated yet",
				pvName)
		}
		return nil, status.Errorf(codes.Unavailable,
			"recovery record %q pins authorization digest %s but carries no "+
				"spec.recovery.authorization bytes; populate the authorization the "+
				"digest names", pvName, intent.AuthorizationDigest)
	}
	if intent.AuthorizationDigest == "" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"recovery record %q carries an authorization but no authorizationDigest "+
				"to pin it", pvName)
	}
	var auth agentv1.RecoveryAuthorization
	err := proto.Unmarshal(intent.Authorization, &auth)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"recovery record %q: spec.recovery.authorization is not a serialized "+
				"RecoveryAuthorization: %v", pvName, err)
	}
	digest, err := recoveryauth.AuthorizationDigest(&auth)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"recovery record %q: cannot digest the declared authorization: %v", pvName, err)
	}
	pinned, err := recoveryauth.ParseDigestHex(intent.AuthorizationDigest)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"recovery record %q: spec.recovery.authorizationDigest %q is malformed: %v",
			pvName, intent.AuthorizationDigest, err)
	}
	if subtle.ConstantTimeCompare(digest[:], pinned[:]) != 1 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the declared authorization digests to %s but "+
				"spec.recovery.authorizationDigest pins %s; the record can only "+
				"consume the grant it pins",
			pvName, recoveryauth.DigestHex(digest), intent.AuthorizationDigest)
	}
	err = recoveryMatchesIntent(&auth, intent, string(pvs.UID), pvs.Spec.AgentVolumeID, pvName)
	if err != nil {
		return nil, err
	}
	return &auth, nil
}

// recoveryMatchesIntent requires every identity field of the authorization
// to equal the record's declared intent — wrong source, lifecycle or
// generation is a permanent refusal, never a retry.
func recoveryMatchesIntent(
	auth *agentv1.RecoveryAuthorization,
	intent *v1alpha1.VolumeRecoveryIntent,
	uid, agentVolID, pvName string,
) error {
	mismatch := func(field, got, want string) error {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization %s is %s but the intent declares %s",
			pvName, field, got, want)
	}
	switch {
	case auth.GetOldVolumeUid() != intent.OldVolumeUID:
		return mismatch("old_volume_uid", fmt.Sprintf("%q", auth.GetOldVolumeUid()),
			fmt.Sprintf("%q", intent.OldVolumeUID))
	case !generationIs(auth.GetOldGeneration(), intent.OldGeneration):
		return mismatch("old_generation", fmt.Sprintf("%d", auth.GetOldGeneration()),
			fmt.Sprintf("%d", intent.OldGeneration))
	case auth.GetNewVolumeUid() != uid:
		return mismatch("new_volume_uid", fmt.Sprintf("%q", auth.GetNewVolumeUid()),
			fmt.Sprintf("this record's uid %q", uid))
	case !generationIs(auth.GetNewGeneration(), intent.NewGeneration):
		return mismatch("new_generation", fmt.Sprintf("%d", auth.GetNewGeneration()),
			fmt.Sprintf("%d", intent.NewGeneration))
	case auth.GetVolumeId() != agentVolID:
		return mismatch("volume_id", fmt.Sprintf("%q", auth.GetVolumeId()),
			fmt.Sprintf("%q", agentVolID))
	case auth.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM:
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization backend_type is %v; only LVM "+
				"volumes can be recovered", pvName, auth.GetBackendType())
	case !lvmSourceMatches(auth.GetLvmSource(), intent.Source):
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization lvm_source %v does not equal the "+
				"declared source %q (vg_uuid %s, lv_uuid %s)",
			pvName, auth.GetLvmSource(), lvmSourceLocator(intent.Source),
			intent.Source.VolumeGroupUUID, intent.Source.LogicalVolumeUUID)
	case auth.PreserveOriginal == nil:
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization does not set preserve_original; "+
				"an explicit policy is required", pvName)
	case auth.GetPreserveOriginal() != intent.Source.PreserveOriginal:
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization preserve_original is %t but the "+
				"declared source policy is %s",
			pvName, auth.GetPreserveOriginal(), lvmSourcePolicy(intent.Source))
	}
	return recoveryValidityWindow(auth, pvName)
}

// generationIs reports whether an agent-side unsigned generation equals the
// record's signed CRD generation.  A negative declared generation can never
// name a real fencing generation, so it matches nothing.
func generationIs(observed uint64, declared int64) bool {
	if declared < 0 {
		return false
	}
	return observed == uint64(declared)
}

// recoveryValidityWindow requires the authorization to carry a well-formed
// validity window no longer than the maximum lifetime that covers now,
// allowing the shared clock skew.  A grant not valid yet is Unavailable; an
// expired or malformed one is a permanent refusal.
func recoveryValidityWindow(auth *agentv1.RecoveryAuthorization, pvName string) error {
	if auth.GetIssuedAt() == nil || auth.GetExpiresAt() == nil {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization lacks issued_at/expires_at", pvName)
	}
	err := auth.GetIssuedAt().CheckValid()
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization issued_at is invalid: %v", pvName, err)
	}
	err = auth.GetExpiresAt().CheckValid()
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization expires_at is invalid: %v", pvName, err)
	}
	issued, expires := auth.GetIssuedAt().AsTime(), auth.GetExpiresAt().AsTime()
	if !expires.After(issued) || expires.Sub(issued) > recoveryauth.MaxAuthorizationLifetime+recoveryauth.ClockSkew {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization validity window %s is malformed or "+
				"exceeds the maximum %s", pvName, expires.Sub(issued),
			recoveryauth.MaxAuthorizationLifetime)
	}
	now := time.Now()
	if issued.After(now.Add(recoveryauth.ClockSkew)) {
		return status.Errorf(codes.Unavailable,
			"recovery record %q: authorization is not valid until %s (clock skew %s)",
			pvName, issued.UTC(), recoveryauth.ClockSkew)
	}
	if !expires.After(now.Add(-recoveryauth.ClockSkew)) {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization expired at %s; re-issue the "+
				"recovery record with a fresh grant", pvName, expires.UTC())
	}
	return nil
}

// markTransferredTo reports whether the observed fence mark already records
// this lifecycle: the current generation is at or beyond the declared
// transfer generation, the durable transfer identity matches the exact
// authorization and lifecycle endpoints, the pinned source/policy match, and
// the retired lifecycle is in ended_uids.  Later generations are normal:
// claimOperation advances the fence before each export attempt.
func markTransferredTo(fence *agentv1.FenceObservation, intent *v1alpha1.VolumeRecoveryIntent, uid string) bool {
	if fence == nil || !fence.GetExists() || intent.NewGeneration < 0 {
		return false
	}
	return fence.GetVolumeUid() == uid &&
		fence.GetGeneration() >= uint64(intent.NewGeneration) &&
		!fence.GetEnded() &&
		slices.Contains(fence.GetEndedUids(), intent.OldVolumeUID) &&
		lvmSourceMatches(fence.GetLvmSource(), intent.Source) &&
		fence.GetPreserveOriginal() == intent.Source.PreserveOriginal &&
		fence.GetTransferAuthorizationDigest() == intent.AuthorizationDigest &&
		fence.GetTransferFromUid() == intent.OldVolumeUID &&
		generationIs(fence.GetTransferFromGeneration(), intent.OldGeneration) &&
		fence.GetTransferToUid() == uid &&
		generationIs(fence.GetTransferToGeneration(), intent.NewGeneration)
}

// recoverySnapshot returns the agent-signed snapshot the operator's grant
// names, decoded from the record's annotationRecoverySnapshot.  The operator
// obtains the snapshot from the agent's InspectVolume, signs the
// authorization over its digest and supplies both in the same patch; the
// controller never substitutes its own observation, because a fresh
// snapshot carries a new issued_at and can never match a grant signed
// earlier.  A missing snapshot is Unavailable (the operator has not
// supplied it yet); one that does not decode is a permanent refusal.
func recoverySnapshot(pvName string, pvs *v1alpha1.PillarVolumeState) (*agentv1.RecoverySnapshot, error) {
	encoded := pvs.Annotations[annotationRecoverySnapshot]
	if encoded == "" {
		return nil, status.Errorf(codes.Unavailable,
			"recovery record %q awaits the agent-signed snapshot the authorization "+
				"names: set annotation %s to the standard base64 of the "+
				"deterministic protobuf RecoverySnapshot from InspectVolume",
			pvName, annotationRecoverySnapshot)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"recovery record %q: annotation %s is not standard base64: %v",
			pvName, annotationRecoverySnapshot, err)
	}
	snap := &agentv1.RecoverySnapshot{}
	err = proto.Unmarshal(raw, snap)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"recovery record %q: annotation %s does not decode as a RecoverySnapshot: %v",
			pvName, annotationRecoverySnapshot, err)
	}
	return snap, nil
}

// verifyRecoveryObservation refuses the transfer early when the agent's
// live report cannot possibly satisfy the grant: the observed mark must
// still belong to the retired lifecycle at the exact declared generation,
// the observed LV identity must equal the declared source, no consumer or
// export may be reported, and the snapshot must echo the same old identity
// and be the very snapshot the grant names.
func verifyRecoveryObservation(
	pvName, agentVolID string,
	intent *v1alpha1.VolumeRecoveryIntent,
	inspectResp *agentv1.InspectVolumeResponse,
	snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization,
) error {
	err := verifyLiveRecoveryMark(pvName, agentVolID, intent, inspectResp)
	if err != nil {
		return err
	}
	return verifyRecoverySnapshot(pvName, agentVolID, intent, snap, auth)
}

// recoveryAdmittingExportCount counts configured own exports that still admit
// a remote initiator. Disabled namespaces and enforced empty ACLs are stopped
// evidence, not live consumers.
func recoveryAdmittingExportCount(exports []*agentv1.ExportObservation) int {
	count := 0
	for _, export := range exports {
		if export.GetNamespaceEnabled() &&
			(!export.GetAclEnabled() || len(export.GetAllowedHosts()) > 0) {
			count++
		}
	}
	return count
}

// verifyLiveRecoveryMark requires the agent's live report to show the
// retired lifecycle's mark at the exact declared generation over the
// declared source, with no consumer or admitting export.
func verifyLiveRecoveryMark(
	pvName, agentVolID string,
	intent *v1alpha1.VolumeRecoveryIntent,
	inspectResp *agentv1.InspectVolumeResponse,
) error {
	fence := inspectResp.GetFence()
	if !fence.GetExists() {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: agent reports no fence mark on %q; the retired "+
				"lifecycle %q cannot be proven — refusing to recover an unmanaged LV",
			pvName, agentVolID, intent.OldVolumeUID)
	}
	if fence.GetVolumeUid() != intent.OldVolumeUID {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the live fence mark on %q belongs to lifecycle %q, "+
				"not the retired lifecycle %q the intent declares",
			pvName, agentVolID, fence.GetVolumeUid(), intent.OldVolumeUID)
	}
	if !generationIs(fence.GetGeneration(), intent.OldGeneration) {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the live fence mark on %q records generation %d, "+
				"not the exact generation %d the intent declares; the old lifecycle "+
				"changed since the intent was written — re-issue the recovery record",
			pvName, agentVolID, fence.GetGeneration(), intent.OldGeneration)
	}
	if !lvmSourceMatches(fence.GetLvmSource(), intent.Source) {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the fence mark on %q pins source %v, not the "+
				"declared source %q; the intent names a different LV",
			pvName, agentVolID, fence.GetLvmSource(), lvmSourceLocator(intent.Source))
	}
	admittingExports := recoveryAdmittingExportCount(inspectResp.GetExports())
	if len(inspectResp.GetConsumers()) > 0 || admittingExports > 0 {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the live report lists %d consumer(s) and %d "+
				"admitting export(s) on %q; a volume in use cannot be recovered",
			pvName, len(inspectResp.GetConsumers()), admittingExports, agentVolID)
	}
	return nil
}

// verifyRecoverySnapshot requires the agent-signed snapshot to echo the
// declared old identity and an unused, free LV under the declared policy,
// and to be the very snapshot the grant names.
func verifyRecoverySnapshot(
	pvName, agentVolID string,
	intent *v1alpha1.VolumeRecoveryIntent,
	snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization,
) error {
	if snap.GetVolumeId() != agentVolID ||
		snap.GetOldVolumeUid() != intent.OldVolumeUID ||
		!generationIs(snap.GetOldGeneration(), intent.OldGeneration) ||
		snap.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM ||
		!lvmSourceMatches(snap.GetLvmSource(), intent.Source) {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the agent-signed snapshot does not describe the "+
				"declared recovery (volume %q, retired lifecycle %q generation %d, "+
				"source %q)", pvName, agentVolID, intent.OldVolumeUID,
			intent.OldGeneration, lvmSourceLocator(intent.Source))
	}
	admittingExports := recoveryAdmittingExportCount(snap.GetExports())
	if len(snap.GetConsumers()) > 0 || admittingExports > 0 {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the signed snapshot reports %d consumer(s) and %d "+
				"admitting export(s) on %q; the grant cannot transfer a volume in use",
			pvName, len(snap.GetConsumers()), admittingExports, agentVolID)
	}
	if snap.GetExclusiveClaim() != "free" {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the signed snapshot reports exclusive claim %q on %q, "+
				"not \"free\"; the device may be in use",
			pvName, snap.GetExclusiveClaim(), agentVolID)
	}
	if snap.GetPreserveOriginal() != intent.Source.PreserveOriginal {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the signed snapshot reports preserve policy %t but "+
				"the intent declares %s", pvName, snap.GetPreserveOriginal(),
			lvmSourcePolicy(intent.Source))
	}
	err := recoveryauth.CheckAuthorizationMatchesSnapshot(auth, snap)
	if err != nil {
		if errors.Is(err, recoveryauth.ErrDigestMismatch) {
			return status.Errorf(codes.FailedPrecondition,
				"recovery record %q: the authorization was signed for a different "+
					"snapshot (digest mismatch); the snapshot this record persisted is "+
					"not the one the grant names — re-issue the recovery record", pvName)
		}
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: authorization does not match the snapshot: %v", pvName, err)
	}
	return nil
}

// recoverVolume drives the recovery record's transfer pipeline and returns
// the device path and capacity the shared export step continues with.  A
// live mark that already records this lifecycle means a previous attempt
// committed: the step is skipped without needing the authorization again.
// Otherwise the declared authorization and the persisted snapshot are
// verified against the live observation and TransferVolumeOwnership runs.
// TRANSFER_OUTCOME_UNKNOWN is retried once with the identical request;
// anything still unknown is Unavailable, never rolled back.
func (s *ControllerServer) recoverVolume(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	pvName string,
	pvs *v1alpha1.PillarVolumeState,
	exportSpec *v1alpha1.VolumeExportSpec,
	requiredBytes int64,
) (devicePath string, capacity int64, err error) {
	intent := pvs.Spec.Recovery
	agentVolID := pvs.Spec.AgentVolumeID
	uid := string(pvs.UID)

	inspectResp, err := agentClient.InspectVolume(ctx, &agentv1.InspectVolumeRequest{
		VolumeId:    agentVolID,
		BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
	})
	if err != nil {
		grpcSt, _ := status.FromError(err)
		return "", 0, status.Errorf(grpcSt.Code(),
			"agent InspectVolume(%q) for recovery %q failed: %v", agentVolID, pvName, err)
	}

	capacity = pvs.Spec.CapacityBytes
	if lvm := inspectResp.GetLvm(); lvm != nil {
		devicePath = lvm.GetDevicePath()
		if lvm.GetSizeBytes() > 0 {
			capacity = lvm.GetSizeBytes()
		}
	}
	if requiredBytes > capacity {
		return "", 0, status.Errorf(codes.FailedPrecondition,
			"cannot recover volume %q: the claim requests %d bytes but the LV "+
				"provides %d bytes; a recovered LV cannot grow", pvName,
			requiredBytes, capacity)
	}

	if !markTransferredTo(inspectResp.GetFence(), intent, uid) {
		err = s.transferRecoveredVolume(ctx, agentClient, pvName, pvs, inspectResp)
		if err != nil {
			return "", 0, err
		}
	}
	// The inspection is the authoritative capacity observation for both a
	// fresh transfer and a retry that observes a transfer committed after an
	// outcome-unknown response. Persist it before CreatePartial so every
	// subsequent response uses the actual LV size.
	err = s.recordAllocatedCapacity(ctx, pvName, pvs.UID, capacity)
	if err != nil {
		return "", 0, err
	}
	// The transfer is durable: record the partial state so a retry only
	// re-exports, exactly like an adopted backend whose export is pending.
	err = s.persistCreatePartial(ctx, pvName, pvs.UID, devicePath, exportSpec)
	if err != nil {
		return "", 0, err
	}
	return devicePath, capacity, nil
}

// transferRecoveredVolume runs the authorized transfer for a recovery record
// whose live mark does not record this lifecycle yet: the declared grant and
// the persisted snapshot are verified against the live observation, and
// TransferVolumeOwnership runs with the committed grant pinned.
func (*ControllerServer) transferRecoveredVolume(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	pvName string,
	pvs *v1alpha1.PillarVolumeState,
	inspectResp *agentv1.InspectVolumeResponse,
) error {
	intent := pvs.Spec.Recovery
	auth, err := recoveryAuthorization(pvName, pvs)
	if err != nil {
		return err
	}
	snap, err := recoverySnapshot(pvName, pvs)
	if err != nil {
		return err
	}
	err = verifyRecoveryObservation(pvName, pvs.Spec.AgentVolumeID, intent, inspectResp, snap, auth)
	if err != nil {
		return err
	}
	transfer, err := recoveryTransfer(ctx, agentClient, pvName, snap, auth)
	if err != nil {
		return err
	}
	pinned, err := recoveryauth.ParseDigestHex(intent.AuthorizationDigest)
	if err != nil || subtle.ConstantTimeCompare(pinned[:], transfer.GetAuthorizationDigest()) != 1 {
		return status.Errorf(codes.FailedPrecondition,
			"recovery record %q: the committed transfer recorded authorization "+
				"digest %x but the record pins %s; the committed grant is not the "+
				"declared one", pvName, transfer.GetAuthorizationDigest(),
			intent.AuthorizationDigest)
	}
	return nil
}

// recoveryTransfer issues TransferVolumeOwnership, retrying an
// outcome-unknown call exactly once with the identical request — the grant
// only ever authorizes the snapshot it names, so no other request may be
// tried, and the agent resolves an uncertain mark through its durable
// transfer record (COMMITTED / ALREADY_COMMITTED on the same request).
func recoveryTransfer(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	pvName string,
	snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization,
) (*agentv1.TransferVolumeOwnershipResponse, error) {
	req := &agentv1.TransferVolumeOwnershipRequest{Snapshot: snap, Authorization: auth}
	var last *agentv1.TransferVolumeOwnershipResponse
	for range 2 {
		resp, err := agentClient.TransferVolumeOwnership(ctx, req)
		if err != nil {
			grpcSt, _ := status.FromError(err)
			return nil, status.Errorf(grpcSt.Code(),
				"agent TransferVolumeOwnership for recovery %q failed: %v", pvName, err)
		}
		switch resp.GetOutcome() {
		case agentv1.TransferOutcome_TRANSFER_OUTCOME_COMMITTED,
			agentv1.TransferOutcome_TRANSFER_OUTCOME_ALREADY_COMMITTED:
			return resp, nil
		case agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN:
			last = resp
			continue
		default:
			return nil, status.Errorf(codes.Unavailable,
				"agent TransferVolumeOwnership for recovery %q returned outcome %v; "+
					"retrying", pvName, resp.GetOutcome())
		}
	}
	return nil, status.Errorf(codes.Unavailable,
		"agent TransferVolumeOwnership for recovery %q outcome is still unknown "+
			"after replaying the identical request; the agent keeps the mark state "+
			"without rollback and the next CreateVolume attempt retries "+
			"(last reported outcome %v)", pvName, last.GetOutcome())
}
