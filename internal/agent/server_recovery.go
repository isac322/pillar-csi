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

package agent

import (
	"context"
	"errors"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/recoveryauth"
)

// Volume-ownership recovery.
//
// A recovery moves a volume ID from its retired lifecycle (the fencing
// mark's recorded owner) to a new lifecycle WITHOUT the old lifecycle's
// fencing token: the old PillarVolumeState is gone, so nothing can present
// its token.  Two signatures replace it:
//
//   - RecoverySnapshot, signed by this agent's own server TLS key while
//     healthy, attesting what the node observed: the mark's UID and exact
//     generation, the pinned LV identity, the preserve policy and the
//     consumer/export evidence (issued by InspectVolume).
//   - RecoveryAuthorization, signed by an operator key the agent is
//     configured to trust (--recovery-trust-anchor), naming that snapshot
//     by digest and declaring the destination lifecycle.
//
// TransferVolumeOwnership verifies both, re-checks every claim against the
// durable mark and a live observation under the per-volume fencing lock,
// then commits one atomic mark write.  Nothing else — no caller identity,
// no flag, no request field — can move the mark between lifecycles, and no
// recovery support exists for a volume ID the agent never adopted.

// recoveryConfigured reports whether this agent may take part in recovery:
// it needs its TLS signer and leaf certificate (to issue and verify
// snapshots) and at least one operator trust anchor (to verify an
// authorization).  Anything missing fails closed.
func (s *Server) recoveryConfigured() bool {
	return s.recoverySigner != nil && s.recoveryCert != nil && len(s.recoveryAnchors) > 0
}

// recoveryIdentity is this agent's attested identity, derived from the
// configured serving certificate.  "" means the certificate cannot attest
// an identity; signing is skipped and transfers are refused.
func (s *Server) recoveryIdentity() string {
	return RecoveryAgentIdentity(s.recoveryCert)
}

// verifiedRecoveryPeer reports whether ctx's RPC peer authenticated over
// mTLS with a verified client certificate chain.  Recovery never accepts an
// arbitrary caller assertion: a plaintext connection or an unverified
// client is refused, independent of signatures.
func verifiedRecoveryPeer(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return false
	}
	return len(tlsInfo.State.VerifiedChains) > 0
}

// recoveryStatus maps a recoveryauth verification failure to the transfer's
// gRPC status: forged or unsupported signatures are Unauthenticated, a
// malformed or self-inconsistent request payload is InvalidArgument, and a
// well-formed authorization whose time bounds no longer hold is
// FailedPrecondition.  Unknown failures fail closed as FailedPrecondition.
func recoveryStatus(rpc string, err error) error {
	switch {
	case errors.Is(err, recoveryauth.ErrInvalidSignature),
		errors.Is(err, recoveryauth.ErrUnsupportedKeyType),
		errors.Is(err, recoveryauth.ErrIdentityMismatch),
		errors.Is(err, recoveryauth.ErrKeyMismatch):
		return status.Errorf(codes.Unauthenticated, "%s: %v", rpc, err)
	case errors.Is(err, recoveryauth.ErrMalformed),
		errors.Is(err, recoveryauth.ErrMissingField),
		errors.Is(err, recoveryauth.ErrDigestMismatch):
		return status.Errorf(codes.InvalidArgument, "%s: %v", rpc, err)
	default:
		return status.Errorf(codes.FailedPrecondition, "%s: %v", rpc, err)
	}
}

// TransferVolumeOwnership moves a volume ID's fencing mark to the
// lifecycle the operator authorized, retiring the recorded owner.
//
// Transport authentication is mandatory: a plaintext or unverified client
// is Unauthenticated before anything is inspected.  The request is then
// authorized only by the two signed payloads — never by the mTLS peer's
// identity — and refused when this agent has no recovery authority.
//
// Under the volume's fencing lock the stored mark must still be the exact
// lifecycle and pinned LV the snapshot and authorization name (exact old
// generation, exact backend identity, matching preserve policy), the
// destination lifecycle must be fresh, and a live observation must prove
// the LV idle: the pinned LV is active, its transient O_EXCL open is free,
// no mount, holder or foreign export resolves to it, and no configured own
// export still admits an initiator (the supported stopped proofs are a
// removed export, a disabled namespace/TPG, or an enforced ACL admitting
// no host).  Any missing, corrupt, mismatched or inconclusive evidence is
// a refusal that writes nothing.
//
// One atomic mark write then retires the old UID, records the new
// UID/generation and preserve policy, and stores the transfer record with
// the authorization digest.  A retry of the exact committed transfer is
// answered from that durable record alone — even after the authorization's
// expiry — while the same destination under a different digest, or a
// different destination, is refused.  When the rename committed but its
// durability is uncertain the outcome is UNKNOWN and nothing is rolled
// back.
func (s *Server) TransferVolumeOwnership(
	ctx context.Context,
	req *agentv1.TransferVolumeOwnershipRequest,
) (*agentv1.TransferVolumeOwnershipResponse, error) {
	const rpc = "TransferVolumeOwnership"
	err := s.checkTransferRequest(ctx, rpc, req)
	if err != nil {
		return nil, err
	}
	snap, auth := req.GetSnapshot(), req.GetAuthorization()
	volumeID := snap.GetVolumeId()
	s.setVolumeSpanAttributes(ctx, volumeID)

	// The authorization digest commits to every payload field, so it
	// alone identifies the exact committed transfer: a retry carrying it
	// is answered from the durable record without re-verifying signatures
	// that may have expired since the commit.
	authDigest, err := recoveryauth.AuthorizationDigest(auth)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: digest authorization: %v", rpc, err)
	}
	authDigestHex := recoveryauth.DigestHex(authDigest)

	unlock := s.lockFencing(volumeID)
	defer unlock()

	stored, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, status.Errorf(codes.FailedPrecondition,
			"%s %q: no fencing mark records a lifecycle to transfer; absent or corrupt history refuses", rpc, volumeID)
	}

	// Idempotent retry: the mark already records a committed transfer to
	// the destination this authorization names.  The durable record is
	// authoritative — re-verifying the (possibly expired) signatures must
	// not reject a retry of a committed transfer.  Re-sync the visible mark
	// before acknowledging ALREADY_COMMITTED: a previous response may have
	// observed the rename but not durable directory fsync.
	if stored.VolumeUID == auth.GetNewVolumeUid() {
		err = checkCommittedTransferRetry(rpc, volumeID, stored.Transfer, auth, authDigestHex)
		if err != nil {
			return nil, err
		}
		_, syncErr := s.writeFencingMarkOutcome(volumeID, stored)
		if syncErr != nil {
			return &agentv1.TransferVolumeOwnershipResponse{
				Outcome:             agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN,
				AuthorizationDigest: authDigest[:],
			}, nil
		}
		return &agentv1.TransferVolumeOwnershipResponse{
			Outcome:             agentv1.TransferOutcome_TRANSFER_OUTCOME_ALREADY_COMMITTED,
			AuthorizationDigest: authDigest[:],
		}, nil
	}

	err = s.verifyTransferSignatures(rpc, snap, auth, time.Now())
	if err != nil {
		return nil, err
	}

	newMark, err := s.transferAdmittedMark(ctx, volumeID, stored, snap, auth)
	if err != nil {
		return nil, err
	}
	newMark.Transfer = &markTransfer{
		AuthorizationDigest: authDigestHex,
		FromUID:             stored.VolumeUID,
		FromGeneration:      stored.Generation,
		ToUID:               auth.GetNewVolumeUid(),
		ToGeneration:        auth.GetNewGeneration(),
	}

	committed, writeErr := s.writeFencingMarkOutcome(volumeID, newMark)
	switch {
	case writeErr == nil:
		return &agentv1.TransferVolumeOwnershipResponse{
			Outcome:             agentv1.TransferOutcome_TRANSFER_OUTCOME_COMMITTED,
			AuthorizationDigest: authDigest[:],
		}, nil
	case committed:
		// The rename applied but its fsync result is unknown: the new mark
		// may or may not be durable.  Rolling back could drop a committed
		// transfer; refusing would misreport a possibly-durable state.
		return &agentv1.TransferVolumeOwnershipResponse{
			Outcome:             agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN,
			AuthorizationDigest: authDigest[:],
		}, nil
	default:
		// The mark was never replaced; the old lifecycle still owns the
		// volume ID and nothing changed.
		return nil, writeErr
	}
}

// checkTransferRequest applies the cheap, lock-free admission checks of
// TransferVolumeOwnership in order: a verified mTLS peer, a configured
// recovery authority, and a snapshot and authorization naming one volume.
func (s *Server) checkTransferRequest(
	ctx context.Context,
	rpc string,
	req *agentv1.TransferVolumeOwnershipRequest,
) error {
	if !verifiedRecoveryPeer(ctx) {
		return status.Errorf(codes.Unauthenticated,
			"%s requires a verified mTLS client; plaintext and unverified clients are refused", rpc)
	}
	if !s.recoveryConfigured() || s.recoveryIdentity() == "" {
		return status.Errorf(codes.Unavailable,
			"%s is unavailable: this agent has no recovery authority "+
				"(--recovery-trust-anchor and TLS identity not configured)", rpc)
	}
	snap, auth := req.GetSnapshot(), req.GetAuthorization()
	if snap == nil || auth == nil || snap.GetVolumeId() == "" || snap.GetVolumeId() != auth.GetVolumeId() {
		return status.Errorf(codes.InvalidArgument,
			"%s requires a snapshot and an authorization naming the same volume_id", rpc)
	}
	return nil
}

// checkCommittedTransferRetry decides whether a request naming the
// lifecycle that already owns the mark is a retry of the exact committed
// transfer tr.  A nil error means the retry is answered as already
// committed; any other ownership history refuses.
func checkCommittedTransferRetry(
	rpc, volumeID string,
	tr *markTransfer,
	auth *agentv1.RecoveryAuthorization,
	authDigestHex string,
) error {
	switch {
	case tr == nil:
		return status.Errorf(codes.FailedPrecondition,
			"%s %q: lifecycle %q already owns the volume and did not gain it by a transfer",
			rpc, volumeID, auth.GetNewVolumeUid())
	case tr.AuthorizationDigest != authDigestHex:
		return status.Errorf(codes.FailedPrecondition,
			"%s %q: a different authorization already committed the transfer to lifecycle %q",
			rpc, volumeID, auth.GetNewVolumeUid())
	case tr.ToUID != auth.GetNewVolumeUid() || tr.ToGeneration != auth.GetNewGeneration() ||
		tr.FromUID != auth.GetOldVolumeUid() || tr.FromGeneration != auth.GetOldGeneration():
		return status.Errorf(codes.FailedPrecondition,
			"%s %q: the recorded transfer to lifecycle %q names different endpoints", rpc, volumeID, auth.GetNewVolumeUid())
	}
	return nil
}

// verifyTransferSignatures verifies, in order, the snapshot against this
// agent's own certificate, the authorization against the trust anchors,
// and that the authorization binds exactly that snapshot.
func (s *Server) verifyTransferSignatures(
	rpc string,
	snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization,
	now time.Time,
) error {
	err := recoveryauth.VerifySnapshotWithCertificate(snap, s.recoveryCert, now)
	if err != nil {
		return recoveryStatus(rpc, err)
	}
	err = recoveryauth.VerifyAuthorization(auth, s.recoveryAnchors, now)
	if err != nil {
		return recoveryStatus(rpc, err)
	}
	err = recoveryauth.CheckAuthorizationMatchesSnapshot(auth, snap)
	if err != nil {
		return recoveryStatus(rpc, err)
	}
	return nil
}

// transferAdmittedMark decides whether the stored mark may move to the
// authorized lifecycle and returns the mark to commit.  The caller holds
// volumeID's fencing lock.  Every refusal writes nothing and keeps the old
// lifecycle as the owner: absent/corrupt/mismatched history, an ended or
// retired lifecycle, an unpinned or mismatched LV identity, a preserve
// downgrade, an already-retired destination, or evidence that the old
// initiator is not proven stopped all refuse.
func (s *Server) transferAdmittedMark(
	ctx context.Context,
	volumeID string,
	stored fencingMark,
	snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization,
) (fencingMark, error) {
	refuse := func(format string, args ...any) (fencingMark, error) {
		return fencingMark{}, status.Errorf(codes.FailedPrecondition,
			"TransferVolumeOwnership %q: "+format, append([]any{volumeID}, args...)...)
	}
	switch {
	case stored.Ended:
		return refuse("lifecycle %q already ended; a transfer cannot move an ended mark", stored.VolumeUID)
	case stored.VolumeUID != auth.GetOldVolumeUid():
		return refuse("the mark is owned by lifecycle %q, not the authorized old lifecycle %q",
			stored.VolumeUID, auth.GetOldVolumeUid())
	case stored.Generation != auth.GetOldGeneration():
		return refuse("the mark records generation %d of lifecycle %q, not the authorized %d",
			stored.Generation, stored.VolumeUID, auth.GetOldGeneration())
	case slices.Contains(stored.EndedUIDs, auth.GetNewVolumeUid()):
		return refuse("destination lifecycle %q was already retired", auth.GetNewVolumeUid())
	case stored.LVMSource == nil:
		return refuse("the volume ID pins no adopted LV; recovery requires an adopted source")
	case !equalLVMSource(stored.LVMSource, markLVMSourceFromProto(auth.GetLvmSource())):
		return refuse("the authorized source %+v differs from the pinned LV %s/%s",
			auth.GetLvmSource(), stored.LVMSource.VolumeGroup, stored.LVMSource.LogicalVolume)
	case snap.GetOldVolumeUid() != stored.VolumeUID || snap.GetOldGeneration() != stored.Generation:
		return refuse("the snapshot's claimed lifecycle %q generation %d is not the mark's %q generation %d",
			snap.GetOldVolumeUid(), snap.GetOldGeneration(), stored.VolumeUID, stored.Generation)
	case stored.PreserveOriginal && !auth.GetPreserveOriginal():
		return refuse("the pinned PreserveOriginal policy cannot be downgraded by a transfer")
	case snap.GetPreserveOriginal() != stored.PreserveOriginal:
		return refuse("the snapshot's observed preserve policy differs from the mark's")
	}
	err := s.verifyTransferEvidence(ctx, volumeID, stored)
	if err != nil {
		return fencingMark{}, err
	}
	retired := slices.Clone(stored.EndedUIDs)
	if !slices.Contains(retired, stored.VolumeUID) {
		retired = append(retired, stored.VolumeUID)
	}
	return fencingMark{
		VolumeUID:        auth.GetNewVolumeUid(),
		Generation:       auth.GetNewGeneration(),
		EndedUIDs:        retired,
		LVMSource:        stored.LVMSource,
		PreserveOriginal: stored.PreserveOriginal || auth.GetPreserveOriginal(),
	}, nil
}

// verifyTransferEvidence re-observes the pinned LV under the fencing lock
// and fails closed unless it proves the old initiator stopped: the observed
// identity equals the pin, the LV is active, the transient O_EXCL open
// succeeded, no mount, holder or configured export resolves to the device,
// and no configured own export still admits an initiator.  An inspection
// error, an unknown claim or a backend without LVInspector is a refusal,
// never idle evidence.
func (s *Server) verifyTransferEvidence(ctx context.Context, volumeID string, stored fencingMark) error {
	pin := stored.LVMSource
	refuse := func(format string, args ...any) error {
		return status.Errorf(codes.FailedPrecondition,
			"TransferVolumeOwnership %q: the old lifecycle is not proven stopped: "+format,
			append([]any{volumeID}, args...)...)
	}
	b, err := s.backendForType(volumeID, agentv1.BackendType_BACKEND_TYPE_LVM)
	if err != nil {
		return refuse("no LVM backend serves the volume: %v", err)
	}
	inspector, ok := b.(backend.LVInspector)
	if !ok {
		return refuse("backend %s cannot observe LV consumers", b.Type())
	}
	obs, err := inspector.InspectLV(ctx, volumeID)
	if err != nil {
		if refused, isRefused := errors.AsType[*backend.ImportRefusedError](err); isRefused {
			return refuse("%v", refused)
		}
		return refuse("consumer probe failed: %v", err)
	}
	ownIDs := ownTargetIDs(volumeID)
	for _, export := range obs.Exports {
		if !slices.Contains(ownIDs, export.Detail) {
			return refuse("foreign configured export %q", export.Detail)
		}
	}
	switch {
	case obs.Identity != pin.identity():
		return refuse("observed identity %+v differs from the pinned one", obs.Identity)
	case !obs.Active:
		return refuse("the LV is inactive, so its consumers cannot be observed")
	case obs.ExclusiveClaim != backend.ExclusiveClaimFree:
		return refuse("exclusive claim is %q", obs.ExclusiveClaim)
	case len(obs.Consumers) > 0:
		return refuse("local consumers %+v", obs.Consumers)
	}
	exports, err := s.inspectOwnExports(volumeID)
	if err != nil {
		return refuse("cannot observe configured exports: %v", err)
	}
	for _, export := range exports {
		if exportAdmitsInitiators(export) {
			return refuse("export %q still admits initiators", export.GetTargetId())
		}
	}
	return nil
}

// exportAdmitsInitiators reports whether a configured export can still
// serve an initiator: a namespace or TPG that is enabled while either ACL
// enforcement is off or at least one host is admitted.  A disabled
// namespace/TPG, or an enforced ACL admitting no host, is the agent's
// proof that initiators were excluded.
func exportAdmitsInitiators(export *agentv1.ExportObservation) bool {
	return export.GetNamespaceEnabled() && (!export.GetAclEnabled() || len(export.GetAllowedHosts()) > 0)
}

// recoverySnapshot builds this agent's signed observation for
// InspectVolume.  It reports whether a snapshot may be issued at
// all: the agent must be configured for recovery with a verifiable mTLS
// peer, the mark must record a live lifecycle pinned to the LV the
// observation just resolved, and the identity must match — a snapshot
// that misstates what it attests is never signed.
func (s *Server) recoverySnapshot(
	ctx context.Context,
	volumeID string,
	stored fencingMark,
	exists bool,
	obs backend.LVObservation,
	consumers []*agentv1.DeviceConsumer,
	exports []*agentv1.ExportObservation,
) (*agentv1.RecoverySnapshot, bool) {
	if !s.recoveryConfigured() || !verifiedRecoveryPeer(ctx) || !exists || stored.Ended || stored.LVMSource == nil {
		return nil, false
	}
	identity := s.recoveryIdentity()
	if identity == "" || obs.Identity != stored.LVMSource.identity() {
		return nil, false
	}
	snap := &agentv1.RecoverySnapshot{
		VolumeId:         volumeID,
		BackendType:      agentv1.BackendType_BACKEND_TYPE_LVM,
		LvmSource:        stored.LVMSource.proto(),
		OldVolumeUid:     stored.VolumeUID,
		OldGeneration:    stored.Generation,
		PreserveOriginal: stored.PreserveOriginal,
		ExclusiveClaim:   obs.ExclusiveClaim,
		Consumers:        consumers,
		Exports:          exports,
		Fence:            fenceObservation(stored, exists),
		AgentIdentity:    identity,
		IssuedAt:         timestamppb.Now(),
	}
	err := recoveryauth.SignSnapshot(snap, s.recoverySigner)
	if err != nil {
		return nil, false
	}
	return snap, true
}
