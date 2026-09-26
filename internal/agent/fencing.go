package agent

import (
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

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
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

// fenced validates token against volumeID's durable mark, persists the
// advanced mark, runs mutate, and for fenceDestroy records the lifecycle as
// ended after mutate succeeded, all while holding the per-volume fencing lock
// so no other request can be admitted in between.  A nil mutate only checks
// and persists.  A rejection returns FailedPrecondition and a persistence
// failure returns Internal; in both cases mutate does not run.  The ended
// state is written only after a successful deletion: if the deletion fails
// the lifecycle stays open and no new lifecycle can claim the volume ID.
func (s *Server) fenced(
	volumeID string,
	token *agentv1.FencingToken,
	op fenceOp,
	mutate func() error,
) error {
	unlock := s.lockFencing(volumeID)
	defer unlock()

	stored, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		return err
	}
	next, changed, err := admitFencingToken(volumeID, token, op, stored, exists)
	if err != nil {
		return err
	}
	err = s.persistFencingMark(volumeID, next, changed)
	if err != nil {
		return err
	}
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

// admitFencingToken applies the fencing rules (see agentv1.FencingToken).  It
// returns the mark that must be durable before the mutation runs and whether
// that mark differs from the stored one.  A request without a token is always
// rejected: every mutation must belong to a lifecycle the controller recorded.
func admitFencingToken(
	volumeID string,
	token *agentv1.FencingToken,
	op fenceOp,
	stored fencingMark,
	exists bool,
) (next fencingMark, changed bool, err error) {
	uid, gen := token.GetVolumeUid(), token.GetGeneration()
	stale := func(reason string) error {
		return status.Errorf(codes.FailedPrecondition,
			"stale fencing token (uid %q, generation %d) for volume %q: %s "+
				"(mark uid %q, generation %d, ended %t)",
			uid, gen, volumeID, reason, stored.VolumeUID, stored.Generation, stored.Ended)
	}
	switch {
	case uid == "":
		return fencingMark{}, false, status.Errorf(codes.FailedPrecondition,
			"fencing token required for volume %q: every mutating request must carry the "+
				"PillarVolumeState UID and generation", volumeID)
	case !exists:
		return fencingMark{VolumeUID: uid, Generation: gen}, true, nil
	case slices.Contains(stored.EndedUIDs, uid):
		return fencingMark{}, false, stale("lifecycle was retired")
	case uid != stored.VolumeUID:
		if !stored.Ended {
			return fencingMark{}, false, stale("another lifecycle owns the volume")
		}
		retired := append(slices.Clone(stored.EndedUIDs), stored.VolumeUID)
		return fencingMark{VolumeUID: uid, Generation: gen, EndedUIDs: retired}, true, nil
	case stored.Ended:
		// Only a terminal-cleanup retry of the operation that ended the
		// lifecycle may pass; nothing may be created or granted again.
		if op == fenceGrant || gen != stored.Generation {
			return fencingMark{}, false, stale("lifecycle already ended")
		}
		return stored, false, nil
	case gen < stored.Generation:
		return fencingMark{}, false, stale("superseded by a newer operation")
	case gen == stored.Generation:
		return stored, false, nil
	default:
		advanced := stored
		advanced.Generation = gen
		return advanced, true, nil
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
