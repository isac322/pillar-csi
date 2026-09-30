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
func (s *Server) fenced(
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
func (s *Server) fencedImport(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
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
	if mutate != nil {
		// The import checks run between admission and persistence: a refusal
		// leaves no durable trace of this lifecycle on the volume ID.
		err = mutate()
		if err != nil {
			recordFenceDecision(ctx, fenceGrant, adm.decision)
			return err
		}
	}
	err = s.persistFencingMark(volumeID, adm.next, adm.changed)
	if err != nil {
		recordFenceDecision(ctx, fenceGrant, telemetry.FenceMarkIOError)
		return err
	}
	recordFenceDecision(ctx, fenceGrant, adm.decision)
	return nil
}

// retireFence durably retires token's lifecycle from volumeID without any
// backend mutation, for ReleaseVolume.  Afterwards the lifecycle's UID is in
// EndedUIDs, so every later request carrying it — a delayed ImportVolume, a
// stale DeleteVolume — is rejected as retired, while a new lifecycle may
// still claim the volume ID.
//
// When token's lifecycle currently owns the mark it may hold an export, so
// the mark is retired only once the caller removed it: with unexported false
// retireFence then writes nothing and returns owned=true.  A mark owned by a
// different lifecycle is left owned by it (only token's UID is added to the
// retired set); without any mark the lifecycle is recorded as the ended,
// retired owner.  Releasing an already retired lifecycle is a no-op.
func (s *Server) retireFence(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
	unexported bool,
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
// between consecutive mutations.
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
			next:     fencingMark{VolumeUID: uid, Generation: gen, EndedUIDs: retired},
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
	data, err := json.Marshal(mark)
	if err != nil {
		return status.Errorf(codes.Internal, "encode fencing mark for %q: %v", volumeID, err)
	}
	root, err := s.openFencingRoot()
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // sync errors are returned below

	name := path.Join(fencingDirName, fencingFilename(volumeID))
	tmp := name + ".tmp"
	err = writeFileSynced(root, tmp, data)
	if err != nil {
		return status.Errorf(codes.Internal, "write fencing mark %q: %v", tmp, err)
	}
	err = root.Rename(tmp, name)
	if err != nil {
		return status.Errorf(codes.Internal, "rename fencing mark %q: %v", name, err)
	}
	return syncFencingDirs(root)
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
