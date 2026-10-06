package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// Durable per-volume fencing (see agentv1.FencingToken).
//
// Every RPC that mutates a volume's resources carries a token: the UID of the
// volume's PillarVolumeState (one lifecycle of the volume ID) and the
// publicationGeneration the controller committed for the operation.  The agent
// keeps one mark per volume on the storage node's local disk, under the agent
// state directory (a hostPath, never a PersistentVolume, so the storage node
// never depends on its own volumes).  The check and the mutation run under one
// per-volume lock, so a request that passed the check cannot be overtaken by a
// newer one before it mutates.  The mark survives agent restarts and node
// reboots, which is exactly when a stale in-flight RPC from a former
// controller leader could otherwise land, and it outlives the backend volume
// (ended=true) so a stale request arriving after the delete is still rejected.

const (
	// The generations subdirectory of the agent state dir holds the
	// per-volume marks.
	fencingDirName  = "generations"
	fencingDirPerm  = 0o750
	fencingFilePerm = 0o600
	fencingSuffix   = ".mark"
)

// fencingMark is the durable per-volume fencing state.
type fencingMark struct {
	// VolumeUID identifies the lifecycle that currently owns the volume ID.
	VolumeUID string `json:"volumeUID"`
	// Generation is the highest generation applied for that lifecycle.
	Generation uint64 `json:"generation"`
	// Ended is set once that lifecycle's backend volume was deleted.
	Ended bool `json:"ended"`
	// EndedUIDs lists every earlier lifecycle of the volume ID.  UIDs carry no
	// order, so a delayed request from a retired lifecycle is recognized only
	// by membership here.  It grows only when a volume ID is reused.
	EndedUIDs []string `json:"endedUIDs,omitempty"`
	// LVMSource pins the pre-existing LV the first successful LVM import
	// adopted for the volume ID.  It is sticky: no admission branch,
	// release or later lifecycle clears or retargets it, so the volume ID
	// can only ever resolve to that LV.  Nil for zvol, dataset and managed
	// LV volumes.
	LVMSource *markLVMSource `json:"lvmSource,omitempty"`
	// PreserveOriginal pins the adoption policy of LVMSource.  Once true it
	// is never cleared: DeleteVolume and ExpandVolume are refused and a
	// release retires the lifecycle only after verifying no local consumer.
	PreserveOriginal bool `json:"preserveOriginal,omitempty"`
	// Transfer records the recovery transfer that last changed the owning
	// lifecycle (see server_recovery.go).  It is what makes a repeated
	// TransferVolumeOwnership carrying the same authorization idempotent:
	// the request is answered from this durable record, while any different
	// destination or authorization digest is refused.  Nil for volume IDs
	// whose lifecycle never came from a recovery transfer.
	Transfer *markTransfer `json:"transfer,omitempty"`
}

// markTransfer is the durable record of one committed ownership transfer.
// The controller-side authority is the signed RecoveryAuthorization; the
// mark keeps only what a retry needs to be answered exactly: the source and
// destination lifecycles and the SHA-256 digest (lowercase hex) of the
// authorization payload that committed it.
type markTransfer struct {
	// AuthorizationDigest is recoveryauth.DigestHex of the committed
	// authorization payload (signature excluded).
	AuthorizationDigest string `json:"authorizationDigest"`
	// FromUID/FromGeneration name the retired lifecycle the transfer
	// moved the volume ID away from.
	FromUID        string `json:"fromUID"`
	FromGeneration uint64 `json:"fromGeneration"`
	// ToUID/ToGeneration name the lifecycle the transfer committed; ToUID
	// always equals the mark's VolumeUID.
	ToUID        string `json:"toUID"`
	ToGeneration uint64 `json:"toGeneration"`
}

// markLVMSource is the durable form of an adopted LV's identity.
type markLVMSource struct {
	VolumeGroup       string `json:"volumeGroup"`
	LogicalVolume     string `json:"logicalVolume"`
	VolumeGroupUUID   string `json:"volumeGroupUUID"`
	LogicalVolumeUUID string `json:"logicalVolumeUUID"`
}

// markLVMSourceFromProto converts a request identity; nil stays nil.
func markLVMSourceFromProto(src *agentv1.LvmSourceIdentity) *markLVMSource {
	if src == nil {
		return nil
	}
	return &markLVMSource{
		VolumeGroup:       src.GetVolumeGroup(),
		LogicalVolume:     src.GetLogicalVolume(),
		VolumeGroupUUID:   src.GetVolumeGroupUuid(),
		LogicalVolumeUUID: src.GetLogicalVolumeUuid(),
	}
}

// identity returns the backend form of the pinned source.
func (m *markLVMSource) identity() backend.LVMIdentity {
	return backend.LVMIdentity{
		VolumeGroup:       m.VolumeGroup,
		LogicalVolume:     m.LogicalVolume,
		VolumeGroupUUID:   m.VolumeGroupUUID,
		LogicalVolumeUUID: m.LogicalVolumeUUID,
	}
}

// proto returns the wire form of the pinned source; nil stays nil.
func (m *markLVMSource) proto() *agentv1.LvmSourceIdentity {
	if m == nil {
		return nil
	}
	return &agentv1.LvmSourceIdentity{
		VolumeGroup:       m.VolumeGroup,
		LogicalVolume:     m.LogicalVolume,
		VolumeGroupUuid:   m.VolumeGroupUUID,
		LogicalVolumeUuid: m.LogicalVolumeUUID,
	}
}

// equalLVMSource reports whether a and b pin the same LV (both nil counts).
func equalLVMSource(a, b *markLVMSource) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// equalTransfer reports whether a and b record the same committed transfer
// (both nil counts).
func equalTransfer(a, b *markTransfer) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// equalMarks reports whether two marks serialize to the same state.
func equalMarks(a, b fencingMark) bool {
	return a.VolumeUID == b.VolumeUID && a.Generation == b.Generation && a.Ended == b.Ended &&
		slices.Equal(a.EndedUIDs, b.EndedUIDs) && equalLVMSource(a.LVMSource, b.LVMSource) &&
		a.PreserveOriginal == b.PreserveOriginal && equalTransfer(a.Transfer, b.Transfer)
}

// verifyPinnedSource re-verifies, for a mark that pins an adopted LV, that
// volumeID still resolves to exactly that LV.  The caller MUST hold
// volumeID's fencing lock (lockFencing); the helper never locks, so it runs
// inside fenced, recheckFence and retireFence without nesting.  A mark
// without a pinned source is a no-op.  It fails closed: an identity or
// missing refusal is FailedPrecondition, a backend that cannot verify LVs
// is FailedPrecondition (the pinned lifecycle's verification prerequisite
// cannot be met on this agent), and any other verifier error is Internal.
func (s *Server) verifyPinnedSource(ctx context.Context, volumeID string, stored fencingMark) error {
	if stored.LVMSource == nil {
		return nil
	}
	b, err := s.backendForType(volumeID, agentv1.BackendType_BACKEND_TYPE_LVM)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"volume %q pins LV %s/%s but no LVM backend serves it to verify the source: %v",
			volumeID, stored.LVMSource.VolumeGroup, stored.LVMSource.LogicalVolume, err)
	}
	verifier, ok := b.(backend.LVVerifier)
	if !ok {
		return status.Errorf(codes.FailedPrecondition,
			"volume %q pins LV %s/%s but backend %s cannot verify LV identities; refusing the mutation",
			volumeID, stored.LVMSource.VolumeGroup, stored.LVMSource.LogicalVolume, b.Type())
	}
	err = verifier.VerifyLV(ctx, volumeID, stored.LVMSource.identity())
	if err == nil {
		return nil
	}
	if refused, isRefused := errors.AsType[*backend.ImportRefusedError](err); isRefused {
		return status.Errorf(codes.FailedPrecondition,
			"volume %q no longer resolves to its pinned LV: %v", volumeID, refused)
	}
	return status.Errorf(codes.Internal, "verify pinned LV of volume %q: %v", volumeID, err)
}

// fencingFilename converts a volume ID into a filesystem-safe mark name.
// Every byte outside [a-zA-Z0-9.-] (including '/' and '_') is escaped as
// "_XX" hex so that distinct volume IDs never map to the same file.
func fencingFilename(volumeID string) string {
	const hexDigits = "0123456789abcdef"
	var b strings.Builder
	for i := range len(volumeID) {
		c := volumeID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String() + fencingSuffix
}

// lockFencing acquires the per-volume fencing mutex and returns its unlock.
func (s *Server) lockFencing(volumeID string) func() {
	v, _ := s.fencingMu.LoadOrStore(volumeID, &sync.Mutex{})
	mu, ok := v.(*sync.Mutex)
	if !ok {
		// Should never happen: only *sync.Mutex values are stored.
		mu = &sync.Mutex{}
	}
	mu.Lock()
	return mu.Unlock
}

// fenceOp classifies a fenced mutation.
type fenceOp int

const (
	// Grant-class operations create or extend access or resources:
	// CreateVolume, ExpandVolume, ExportVolume, AllowInitiator, ReconcileState.
	fenceGrant fenceOp = iota
	// Revoke-class operations only remove access: DenyInitiator, UnexportVolume.
	fenceRevoke
	// The destroy operation deletes the backend volume and ends the lifecycle
	// once the deletion succeeded.
	fenceDestroy
)

// label returns the fence_op label value of op.
func (op fenceOp) label() string {
	switch op {
	case fenceGrant:
		return telemetry.FenceOpGrant
	case fenceRevoke:
		return telemetry.FenceOpRevoke
	case fenceDestroy:
		return telemetry.FenceOpDestroy
	default:
		return telemetry.LabelOther
	}
}

// recordFenceDecision counts one admit/recheck outcome (M7) and records it on
// the RPC's span and failure log line.
func recordFenceDecision(ctx context.Context, op fenceOp, decision string) {
	fencingDecisions.WithLabelValues(op.label(), telemetry.FenceDecisionLabel(decision)).Inc()
	telemetry.RecordFenceDecision(ctx, op.label(), decision)
}

// fenced validates token against volumeID's durable mark, persists the
// advanced mark, runs mutate, and for fenceDestroy records the lifecycle as
// ended after mutate succeeded, all while holding the per-volume fencing lock
// so no other request can be admitted in between.  A nil mutate only checks
// and persists.  A rejection returns FailedPrecondition and a persistence
// failure returns Internal; in both cases mutate does not run.  The ended
// state is written only after a successful deletion: if the deletion fails
// the lifecycle stays open and no new lifecycle can claim the volume ID.
//
// For a mark that pins an adopted LV, every non-revoke operation first
// re-verifies the LV (verifyPinnedSource) before anything is persisted, so a
// refusal leaves the mark byte-identical.  Revocations stay exempt: removing
// access to a replaced LV must still be possible.
func (s *Server) fenced(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
	op fenceOp,
	mutate func() error,
) error {
	return s.fencedChecked(ctx, volumeID, token, op, nil, mutate)
}

// fencedChecked is fenced with an extra policy check: after admission and
// before the pinned-source verification, persistence and mutate, check runs
// on the stored mark (zero when absent) under the fencing lock and may
// refuse the operation.  Neither check nor the verification runs for a
// terminal retry of an already ended lifecycle, whose resource is gone.
//
// A terminal destroy retry on a mark that pins an adopted LV never runs
// mutate: the pinned LV was already deleted (or released), and the volume's
// locator may since name a different LV that a delayed retry must not
// destroy.  It re-syncs the unchanged mark and reports idempotent success.
func (s *Server) fencedChecked(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
	op fenceOp,
	check func(stored fencingMark) error,
	mutate func() error,
) error {
	unlock := s.lockFencing(volumeID)
	defer unlock()

	stored, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		recordFenceDecision(ctx, op, telemetry.FenceMarkIOError)
		return err
	}
	adm, err := admitFencingToken(volumeID, token, op, stored, exists)
	if err != nil {
		recordFenceDecision(ctx, op, adm.decision)
		return err
	}
	if adm.decision == telemetry.FenceAdmitTerminalRetry && op == fenceDestroy && stored.LVMSource != nil {
		err = s.persistFencingMark(volumeID, stored, false)
		if err != nil {
			recordFenceDecision(ctx, op, telemetry.FenceMarkIOError)
			return err
		}
		recordFenceDecision(ctx, op, adm.decision)
		return nil
	}
	if adm.decision != telemetry.FenceAdmitTerminalRetry {
		err = s.checkFencePreconditions(ctx, volumeID, op, check, stored)
		if err != nil {
			return err
		}
	}
	next := adm.next
	err = s.persistFencingMark(volumeID, next, adm.changed)
	if err != nil {
		recordFenceDecision(ctx, op, telemetry.FenceMarkIOError)
		return err
	}
	recordFenceDecision(ctx, op, adm.decision)
	if mutate != nil {
		err = mutate()
		if err != nil {
			return err
		}
	}
	if op == fenceDestroy && !next.Ended {
		next.Ended = true
		return s.writeFencingMark(volumeID, next)
	}
	return nil
}

// checkFencePreconditions runs fencedChecked's policy check and then, for
// every operation but a revoke, the pinned-source verification on the
// stored mark.  The caller holds the fencing lock and has not yet persisted
// anything, so a refusal leaves the mark untouched.
func (s *Server) checkFencePreconditions(
	ctx context.Context,
	volumeID string,
	op fenceOp,
	check func(stored fencingMark) error,
	stored fencingMark,
) error {
	if check != nil {
		err := check(stored)
		if err != nil {
			return err
		}
	}
	if op == fenceRevoke {
		return nil
	}
	return s.verifyPinnedSource(ctx, volumeID, stored)
}

// fencedImport is the fenced variant for ImportVolume: it validates the
// token, runs mutate (the backend's read-only import checks), and persists
// the admitted mark only when mutate succeeded — fenced persists the mark
// BEFORE its mutation runs because create/export must own the volume ID
// before they add resources, but an import owns nothing until the resource
// is proven adoptable.  Persisting a refused import would bind a
// pre-existing resource to a lifecycle that never adopted it: a later
// teardown (or a stale second controller) carrying that lifecycle's token
// could then delete a volume this driver never owned, and a retry after the
// refusal cause is fixed could be fenced out by its own phantom mark.
//
// Admission still runs under the per-volume fencing lock and the mark is
// still written before the response, under the same lock: two concurrent
// imports of one volume ID cannot both bind it, and a retry of the same
// lifecycle after a lost response re-admits idempotently (same UID, same or
// higher generation).  A refusal leaves a pre-existing mark untouched, so a
// stale token is still rejected even when the mutation would fail anyway.
//
// The src and preserve arguments are the LVM import's expected source and
// policy (nil and false for ZFS).  A mark that already pins a source admits
// only that exact source and never a downgrade of PreserveOriginal — for the
// owning lifecycle and any later one alike — and refuses before mutate runs.
// On success the mark keeps the stored pin or records src, and
// PreserveOriginal only ever upgrades.
func (s *Server) fencedImport(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
	src *markLVMSource,
	preserve bool,
	mutate func() error,
) error {
	unlock := s.lockFencing(volumeID)
	defer unlock()

	stored, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		recordFenceDecision(ctx, fenceGrant, telemetry.FenceMarkIOError)
		return err
	}
	adm, err := admitFencingToken(volumeID, token, fenceGrant, stored, exists)
	if err != nil {
		recordFenceDecision(ctx, fenceGrant, adm.decision)
		return err
	}
	next, err := pinImportSource(volumeID, adm.next, stored, src, preserve)
	if err != nil {
		return err
	}
	if mutate != nil {
		// The import checks run between admission and persistence: a refusal
		// leaves no durable trace of this lifecycle on the volume ID.
		err = mutate()
		if err != nil {
			recordFenceDecision(ctx, fenceGrant, adm.decision)
			return err
		}
	}
	changed := !exists || !equalMarks(next, stored)
	err = s.persistFencingMark(volumeID, next, changed)
	if err != nil {
		recordFenceDecision(ctx, fenceGrant, telemetry.FenceMarkIOError)
		return err
	}
	recordFenceDecision(ctx, fenceGrant, adm.decision)
	return nil
}

// pinImportSource applies the sticky pin rules of an import to the admitted
// mark next: a stored pin must equal src exactly and a stored
// PreserveOriginal cannot be downgraded; otherwise FailedPrecondition.
func pinImportSource(
	volumeID string,
	next, stored fencingMark,
	src *markLVMSource,
	preserve bool,
) (fencingMark, error) {
	if stored.LVMSource != nil {
		if !equalLVMSource(stored.LVMSource, src) {
			return fencingMark{}, status.Errorf(codes.FailedPrecondition,
				"volume %q is pinned to LV %s/%s (vg_uuid %s, lv_uuid %s); refusing to import another source",
				volumeID, stored.LVMSource.VolumeGroup, stored.LVMSource.LogicalVolume,
				stored.LVMSource.VolumeGroupUUID, stored.LVMSource.LogicalVolumeUUID)
		}
		if stored.PreserveOriginal && !preserve {
			return fencingMark{}, status.Errorf(codes.FailedPrecondition,
				"volume %q is pinned PreserveOriginal; refusing to downgrade the adoption to Managed", volumeID)
		}
	}
	next.LVMSource = stored.LVMSource
	if next.LVMSource == nil && src != nil {
		pinned := *src
		next.LVMSource = &pinned
	}
	next.PreserveOriginal = stored.PreserveOriginal || (src != nil && preserve)
	return next, nil
}

// retireFence durably retires token's lifecycle from volumeID without any
// backend mutation, for ReleaseVolume.  Afterwards the lifecycle's UID is in
// EndedUIDs, so every later request carrying it — a delayed ImportVolume, a
// stale DeleteVolume — is rejected as retired, while a new lifecycle may
// still claim the volume ID.
//
// When token's lifecycle currently owns the mark it may hold an export, so
// the mark is retired only once the caller removed it: with unexported false
// retireFence then writes nothing and returns owned=true.  With unexported
// true, check (when non-nil) runs on the stored mark under the fencing lock
// right before the owner's retirement is written; a check error keeps the
// lifecycle owning the volume ID and writes nothing.  A mark owned by a
// different lifecycle is left owned by it (only token's UID is added to the
// retired set); without any mark the lifecycle is recorded as the ended,
// retired owner.  Releasing an already retired lifecycle is a no-op.  Every
// branch keeps a pinned LVM source and policy.
func (s *Server) retireFence(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
	unexported bool,
	check func(stored fencingMark) error,
) (owned bool, err error) {
	unlock := s.lockFencing(volumeID)
	defer unlock()

	stored, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		recordFenceDecision(ctx, fenceDestroy, telemetry.FenceMarkIOError)
		return false, err
	}
	uid := token.GetVolumeUid()
	var next fencingMark
	switch {
	case uid == "":
		adm, admErr := admitFencingToken(volumeID, token, fenceDestroy, stored, exists)
		recordFenceDecision(ctx, fenceDestroy, adm.decision)
		return false, admErr
	case !exists:
		next = fencingMark{VolumeUID: uid, Generation: token.GetGeneration(), Ended: true, EndedUIDs: []string{uid}}
	case slices.Contains(stored.EndedUIDs, uid):
		return false, nil
	case uid != stored.VolumeUID:
		next = stored
		next.EndedUIDs = append(slices.Clone(stored.EndedUIDs), uid)
	default:
		// The generation rules still apply to the owning lifecycle: a
		// superseded token must not end the lifecycle a newer operation owns.
		adm, admErr := admitFencingToken(volumeID, token, fenceRevoke, stored, exists)
		if admErr != nil {
			recordFenceDecision(ctx, fenceDestroy, adm.decision)
			return false, admErr
		}
		if !unexported {
			return true, nil
		}
		if check != nil {
			err = check(stored)
			if err != nil {
				return true, err
			}
		}
		next = adm.next
		next.Ended = true
		next.EndedUIDs = append(slices.Clone(stored.EndedUIDs), uid)
	}
	err = s.writeFencingMark(volumeID, next)
	if err != nil {
		recordFenceDecision(ctx, fenceDestroy, telemetry.FenceMarkIOError)
		return false, err
	}
	return false, nil
}

// recheckFence runs mutate under volumeID's fencing lock if token is still
// admitted without advancing the durable mark, i.e. token already passed
// fenced for op and no newer operation superseded it since.  It writes
// nothing to disk, so a caller can finish a mutation that fenced started
// (Reconcile links prepared exports this way) without a durable write
// between consecutive mutations.  Like fenced, a non-revoke operation on a
// mark that pins an adopted LV re-verifies the LV before mutate.
func (s *Server) recheckFence(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
	op fenceOp,
	mutate func() error,
) error {
	unlock := s.lockFencing(volumeID)
	defer unlock()

	stored, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		recordFenceDecision(ctx, op, telemetry.FenceMarkIOError)
		return err
	}
	adm, err := admitFencingToken(volumeID, token, op, stored, exists)
	if err != nil {
		recordFenceDecision(ctx, op, adm.decision)
		return err
	}
	if adm.changed {
		recordFenceDecision(ctx, op, telemetry.FenceRejectMarkChanged)
		return status.Errorf(codes.FailedPrecondition,
			"fencing mark of volume %q changed since the operation was admitted", volumeID)
	}
	if op != fenceRevoke && adm.decision != telemetry.FenceAdmitTerminalRetry {
		err = s.verifyPinnedSource(ctx, volumeID, stored)
		if err != nil {
			return err
		}
	}
	recordFenceDecision(ctx, op, adm.decision)
	return mutate()
}

// fenceAdmission is the outcome of admitFencingToken.
type fenceAdmission struct {
	// next is the mark that must be durable before the mutation runs.
	next fencingMark
	// changed reports whether next differs from the stored mark.
	changed bool
	// decision is the telemetry.Fence* admit or reject value, set for both
	// admitted and rejected tokens.
	decision string
}

// admitFencingToken applies the fencing rules (see agentv1.FencingToken).  A
// rejection returns FailedPrecondition together with the reject decision.  A
// request without a token is always rejected: every mutation must belong to
// a lifecycle the controller recorded.
func admitFencingToken(
	volumeID string,
	token *agentv1.FencingToken,
	op fenceOp,
	stored fencingMark,
	exists bool,
) (fenceAdmission, error) {
	uid, gen := token.GetVolumeUid(), token.GetGeneration()
	reject := func(decision, reason string) (fenceAdmission, error) {
		return fenceAdmission{decision: decision}, status.Errorf(codes.FailedPrecondition,
			"stale fencing token (uid %q, generation %d) for volume %q: %s "+
				"(mark uid %q, generation %d, ended %t)",
			uid, gen, volumeID, reason, stored.VolumeUID, stored.Generation, stored.Ended)
	}
	switch {
	case uid == "":
		return fenceAdmission{decision: telemetry.FenceRejectMissingToken}, status.Errorf(codes.FailedPrecondition,
			"fencing token required for volume %q: every mutating request must carry the "+
				"PillarVolumeState UID and generation", volumeID)
	case !exists:
		return fenceAdmission{
			next:     fencingMark{VolumeUID: uid, Generation: gen},
			changed:  true,
			decision: telemetry.FenceAdmitNewLifecycle,
		}, nil
	case slices.Contains(stored.EndedUIDs, uid):
		return reject(telemetry.FenceRejectRetired, "lifecycle was retired")
	case uid != stored.VolumeUID:
		if !stored.Ended {
			return reject(telemetry.FenceRejectOtherOwner, "another lifecycle owns the volume")
		}
		retired := slices.Clone(stored.EndedUIDs)
		if !slices.Contains(retired, stored.VolumeUID) {
			retired = append(retired, stored.VolumeUID)
		}
		return fenceAdmission{
			// A new lifecycle inherits the pinned source and policy: the
			// volume ID stays bound to the adopted LV forever.
			next: fencingMark{
				VolumeUID: uid, Generation: gen, EndedUIDs: retired,
				LVMSource: stored.LVMSource, PreserveOriginal: stored.PreserveOriginal,
			},
			changed:  true,
			decision: telemetry.FenceAdmitNewLifecycle,
		}, nil
	case stored.Ended:
		// Only a terminal-cleanup retry of the operation that ended the
		// lifecycle may pass; nothing may be created or granted again.
		if op == fenceGrant || gen != stored.Generation {
			return reject(telemetry.FenceRejectEnded, "lifecycle already ended")
		}
		return fenceAdmission{next: stored, decision: telemetry.FenceAdmitTerminalRetry}, nil
	case gen < stored.Generation:
		return reject(telemetry.FenceRejectSuperseded, "superseded by a newer operation")
	case gen == stored.Generation:
		return fenceAdmission{next: stored, decision: telemetry.FenceAdmitSameGeneration}, nil
	default:
		advanced := stored
		advanced.Generation = gen
		return fenceAdmission{next: advanced, changed: true, decision: telemetry.FenceAdmitAdvance}, nil
	}
}

// persistFencingMark makes next durable.  An unchanged mark is re-synced
// instead of rewritten, so a mark whose earlier persistence failed after the
// rename is never trusted as durable.
func (s *Server) persistFencingMark(volumeID string, next fencingMark, changed bool) error {
	if changed {
		return s.writeFencingMark(volumeID, next)
	}
	root, err := s.openFencingRoot()
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // read-only handle; sync errors are returned below
	return syncFencingDirs(root)
}

// openFencingRoot opens the agent state directory as an os.Root so every mark
// path is confined to it, creating the directories on first use.
func (s *Server) openFencingRoot() (*os.Root, error) {
	return s.openStateRoot(fencingDirName)
}

// openStateRoot opens the agent state directory as an os.Root and creates its
// subdirectory subdir on first use.
func (s *Server) openStateRoot(subdir string) (*os.Root, error) {
	stateDir := s.resolvedDrainStateDir()
	err := os.MkdirAll(stateDir, fencingDirPerm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create agent state dir %q: %v", stateDir, err)
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open agent state dir %q: %v", stateDir, err)
	}
	err = root.MkdirAll(subdir, fencingDirPerm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create %s dir in %q: %v",
			subdir, stateDir, errors.Join(err, root.Close()))
	}
	return root, nil
}

// readFencingMark returns the stored mark for volumeID.  An unreadable or
// corrupt mark is an error: guessing a value could re-admit a stale operation.
func (s *Server) readFencingMark(volumeID string) (fencingMark, bool, error) {
	stateDir := s.resolvedDrainStateDir()
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No state dir yet: no volume has fencing history.
			return fencingMark{}, false, nil
		}
		return fencingMark{}, false, status.Errorf(codes.Internal, "open agent state dir %q: %v", stateDir, err)
	}
	defer root.Close() //nolint:errcheck // read-only handle

	name := path.Join(fencingDirName, fencingFilename(volumeID))
	data, err := root.ReadFile(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fencingMark{}, false, nil
		}
		return fencingMark{}, false, status.Errorf(codes.Internal, "read fencing mark %q: %v", name, err)
	}
	var mark fencingMark
	err = json.Unmarshal(data, &mark)
	if err != nil || mark.VolumeUID == "" {
		return fencingMark{}, false, status.Errorf(codes.Internal, "corrupt fencing mark %q: %v", name, err)
	}
	return mark, true, nil
}

// writeFencingMark atomically replaces the mark: write a temp file, fsync it,
// rename it over the mark, then fsync the generations directory and the state
// directory containing it.  A reader sees either the old or the new mark.
func (s *Server) writeFencingMark(volumeID string, mark fencingMark) error {
	_, err := s.writeFencingMarkOutcome(volumeID, mark)
	return err
}

// fencingMarkSyncDirs is the post-rename directory sync of
// writeFencingMarkOutcome.  It is a package variable so tests can inject the
// failure that makes a committed rename's durability unknown — a real fsync
// failure cannot be triggered portably.
var fencingMarkSyncDirs = syncFencingDirs

// writeFencingMarkOutcome is writeFencingMark that additionally reports
// whether a failure happened after the rename: renamed is true when the new
// mark reached the mark path, so its durability is unknown (the rename was
// applied but the directory fsync may have been lost).  Callers that commit
// irreversible ownership changes (the recovery transfer) must answer
// outcome-unknown instead of refusing or rolling back in that case; a nil
// error means the new mark is durable, and renamed=false with a non-nil
// error means the old mark (or no mark) is still intact.
func (s *Server) writeFencingMarkOutcome(volumeID string, mark fencingMark) (renamed bool, err error) {
	data, err := json.Marshal(mark)
	if err != nil {
		return false, status.Errorf(codes.Internal, "encode fencing mark for %q: %v", volumeID, err)
	}
	root, err := s.openFencingRoot()
	if err != nil {
		return false, err
	}
	defer root.Close() //nolint:errcheck // sync errors are returned below

	name := path.Join(fencingDirName, fencingFilename(volumeID))
	tmp := name + ".tmp"
	err = writeFileSynced(root, tmp, data)
	if err != nil {
		return false, status.Errorf(codes.Internal, "write fencing mark %q: %v", tmp, err)
	}
	err = root.Rename(tmp, name)
	if err != nil {
		return false, status.Errorf(codes.Internal, "rename fencing mark %q: %v", name, err)
	}
	return true, fencingMarkSyncDirs(root)
}

// writeFileSynced writes data to name inside root and fsyncs it.
func writeFileSynced(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fencingFilePerm)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	joined := errors.Join(writeErr, syncErr, closeErr)
	if joined != nil {
		return fmt.Errorf("write/sync/close: %w", joined)
	}
	return nil
}

// syncFencingDirs fsyncs the generations directory and the state directory,
// making a rename inside the former and the former's own entry durable.
func syncFencingDirs(root *os.Root) error {
	return syncStateDirs(root, fencingDirName)
}

// syncStateDirs fsyncs subdir and the state directory, making a rename or
// removal inside the former and the former's own entry durable.
func syncStateDirs(root *os.Root, subdir string) error {
	for _, dir := range []string{subdir, "."} {
		d, err := root.Open(dir)
		if err != nil {
			return status.Errorf(codes.Internal, "open %q for fsync: %v", dir, err)
		}
		syncErr := d.Sync()
		closeErr := d.Close()
		joined := errors.Join(syncErr, closeErr)
		if joined != nil {
			return status.Errorf(codes.Internal, "fsync %q: %v", dir, joined)
		}
	}
	return nil
}
