//go:build linux

package nfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// memoryRuntime models admission state, not command text or call counts.
// Faults represent a kernel/etab mutation whose acknowledgement was lost.
type memoryRuntime struct {
	rows         []entry
	alive        bool
	failGrant    bool
	loseReadOnly bool
	failure      func(error)
	cancelGrant  context.CancelFunc
	invalid      map[string]error
	rejectWrites bool
	grantError   error
}

func (r *memoryRuntime) validateExport(e Export) error { return r.invalid[e.VolumeID] }

func (*memoryRuntime) identity() (string, error) { return "boot/netns", nil }
func (r *memoryRuntime) start(_ context.Context, s *diskState, save func() error, failed func(error)) error {
	r.alive = true
	r.failure = failed
	s.Identity = "boot/netns"
	return save()
}
func (r *memoryRuntime) list(ctx context.Context) ([]entry, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}
	return slices.Clone(r.rows), nil
}
func (r *memoryRuntime) grant(ctx context.Context, e Export, client string) error {
	if r.rejectWrites {
		return errors.New("kernel export table is not writable")
	}
	if r.cancelGrant != nil {
		r.cancelGrant()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.grantError != nil {
		return r.grantError
	}
	r.rows = slices.DeleteFunc(r.rows, func(row entry) bool { return row.Path == e.Path && row.Client == client })
	options := e.options()
	if r.loseReadOnly {
		options = strings.Replace(options, "ro,", "rw,", 1)
	}
	r.rows = append(r.rows, entry{e.Path, client, options})
	if r.failGrant {
		return errors.New("lost grant acknowledgement")
	}
	return nil
}
func (r *memoryRuntime) revoke(ctx context.Context, path, client string) error {
	if r.rejectWrites {
		return errors.New("kernel export table is not writable")
	}
	err := ctx.Err()
	if err != nil {
		return err
	}
	r.rows = slices.DeleteFunc(r.rows, func(row entry) bool { return row.Path == path && row.Client == client })
	return nil
}
func (r *memoryRuntime) health() error {
	if !r.alive {
		return errors.New("daemon unavailable")
	}
	return nil
}
func (r *memoryRuntime) close() error { r.alive = false; return nil }

func testManager(t *testing.T, r *memoryRuntime, stateDir string) *Manager {
	t.Helper()
	m, err := NewManager(Config{StateDir: stateDir, BindAddress: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(m.config.ExportRoot, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	m.runtime = r
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func testExport(t *testing.T, m *Manager) Export {
	t.Helper()
	path := t.TempDir()
	if m != nil {
		var err error
		path, err = os.MkdirTemp(m.config.ExportRoot, "volume-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(path); err != nil {
				t.Error(err)
			}
		})
	}
	return Export{
		VolumeID: "pool/volume", Path: path, BindAddress: "192.0.2.10",
		Version: nfsVersion, Squash: squashRoot, ACLEnabled: true,
	}
}

func admitted(r *memoryRuntime) []string {
	clients := make([]string, 0, len(r.rows))
	for _, row := range r.rows {
		if optionValue(row.Options, "fsid") != "0" {
			clients = append(clients, row.Client)
		}
	}
	slices.Sort(clients)
	return clients
}

func assertOnlyGoodDataset(t *testing.T, r *memoryRuntime, good, bad Export) {
	t.Helper()
	if !reflect.DeepEqual(admitted(r), good.Clients) || !r.alive {
		t.Fatalf("bad dataset disrupted healthy writer: %#v", r.rows)
	}
	for _, row := range r.rows {
		if row.Client == bad.Clients[0] {
			t.Fatal("unavailable dataset still admitted through child or pseudoroot")
		}
	}
}

func TestExactAdmissionAndSurvivingWriter(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	if err := m.Put(t.Context(), e, false, true); err != nil {
		t.Fatal(err)
	}
	if len(r.rows) != 0 {
		t.Fatal("empty ACL admitted a client")
	}
	for _, client := range []string{"192.0.2.20", "192.0.2.21"} {
		if err := m.ChangeClient(t.Context(), e.VolumeID, client, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Put(t.Context(), e, false, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(admitted(r), []string{"192.0.2.20", "192.0.2.21"}) {
		t.Fatalf("export retry lost writers: %v", admitted(r))
	}
	if err := m.ChangeClient(t.Context(), e.VolumeID, "192.0.2.20", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(admitted(r), []string{"192.0.2.21"}) {
		t.Fatalf("peer revoke lost surviving writer: %v", admitted(r))
	}
	e.Clients = []string{"192.0.2.22", "192.0.2.22"}
	if err := m.Put(t.Context(), e, true, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(admitted(r), []string{"192.0.2.21"}) {
		t.Fatal("prepare withdrew the surviving writer")
	}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(admitted(r), []string{"192.0.2.22"}) {
		t.Fatalf("reconcile was not exact: %v", admitted(r))
	}
	e.Clients = nil
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	if len(r.rows) != 0 {
		t.Fatal("empty reconcile ACL admitted a client")
	}
}

func TestDurableRecoveryAfterUnacknowledgedGrant(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	r := &memoryRuntime{failGrant: true}
	m := testManager(t, r, stateDir)
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	if err := m.Put(t.Context(), e, true, true); err == nil {
		t.Fatal("lost acknowledgement was hidden")
	}
	// A crash can occur before fail-closed cleanup runs; retain the already
	// admitted row to exercise the write-ahead ownership proof on restart.
	normalized, err := e.normalized()
	if err != nil {
		t.Fatal(err)
	}
	r.rows = []entry{{e.Path, "192.0.2.20", normalized.options()}}
	// Simulate abrupt process death: release flock without graceful unexport.
	if err := releaseLock(m.lock); err != nil {
		t.Fatal(err)
	}
	m.lock = nil
	m.closed = true
	r.failGrant = false
	restarted := testManager(t, r, stateDir)
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(admitted(r), []string{"192.0.2.20"}) {
		t.Fatalf("recovery broadened/lost ACL: %v", admitted(r))
	}
	for _, row := range r.rows {
		if row.Path == e.Path && optionValue(row.Options, "fsid") != FSID(e.VolumeID) {
			t.Fatal("filehandle identity changed on recovery")
		}
	}
	if err := restarted.Remove(t.Context(), e.VolumeID); err != nil {
		t.Fatal(err)
	}
	if len(r.rows) != 0 {
		t.Fatal("cleanup left an export")
	}
}

func TestCanceledExportFailureStillRevokesOwnedAdmissions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &memoryRuntime{failGrant: true, cancelGrant: cancel}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	foreign := entry{Path: "/foreign", Client: "*", Options: "rw,fsid=777"}
	r.rows = append(r.rows, foreign)
	err := m.Put(ctx, e, true, true)
	if err == nil {
		t.Fatal("lost grant acknowledgement was hidden")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("request cancellation was not exercised")
	}
	if !reflect.DeepEqual(r.rows, []entry{foreign}) {
		t.Fatalf("canceled failure left owned admissions or changed foreign exports: %#v", r.rows)
	}
	if r.alive {
		t.Fatal("canceled failure left the owned helper running")
	}
}

func TestDaemonFailureRevokesOwnedNotForeign(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	foreign := entry{Path: "/foreign", Client: "*", Options: "rw,fsid=777"}
	r.rows = append(r.rows, foreign)
	r.alive = false
	r.failure(errors.New("mountd died"))
	if err := m.Health(); err == nil {
		t.Fatal("failed daemon reported healthy")
	}
	if !reflect.DeepEqual(r.rows, []entry{foreign}) {
		t.Fatalf("failure changed foreign export or left owned export: %#v", r.rows)
	}
	if err := m.ChangeClient(t.Context(), e.VolumeID, "192.0.2.21", true); err == nil {
		t.Fatal("failed daemon admitted another client")
	}
}

func TestPolicyReadbackAndForeignConflictFail(t *testing.T) {
	t.Parallel()
	t.Run("readonly lost", func(t *testing.T) {
		r := &memoryRuntime{loseReadOnly: true}
		m := testManager(t, r, t.TempDir())
		e := testExport(t, m)
		e.ReadOnly = true
		e.Clients = []string{"192.0.2.20"}
		if err := m.Put(t.Context(), e, true, true); err == nil {
			t.Fatal("readonly readback mismatch hidden")
		}
		if err := m.Health(); err == nil {
			t.Fatal("readonly violation left runtime advertised healthy")
		}
		if len(r.rows) != 0 {
			t.Fatal("readonly violation left an owned writer reachable")
		}
	})
	t.Run("foreign admission", func(t *testing.T) {
		r := &memoryRuntime{}
		m := testManager(t, r, t.TempDir())
		e := testExport(t, m)
		foreign := entry{Path: e.Path, Client: "*", Options: "rw,fsid=foreign"}
		r.rows = []entry{foreign}
		e.Clients = []string{"192.0.2.20"}
		if err := m.Put(t.Context(), e, true, true); err == nil {
			t.Fatal("foreign admission overwritten")
		}
		if !reflect.DeepEqual(r.rows, []entry{foreign}) {
			t.Fatal("foreign export mutated")
		}
	})
}

func TestCorruptRecoveryFailsClosed(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "owner.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &memoryRuntime{}
	m := testManager(t, r, stateDir)
	if err := m.Start(t.Context()); err == nil {
		t.Fatal("corrupt ownership accepted")
	}
	if r.alive {
		t.Fatal("daemon started before validating ownership")
	}
}

func TestUnsafePathsAndClientsRejected(t *testing.T) {
	t.Parallel()
	base := testExport(t, nil)
	for _, path := range []string{"relative", "/", "/a/../b", "/a b", "/a\n", "/a:options", "/a\\b"} {
		e := base
		e.Path = path
		if _, err := e.normalized(); err == nil {
			t.Fatalf("unsafe path %q accepted", path)
		}
	}
	for _, client := range []string{"*", "192.0.2.0/24", "host.test", "2001:db8::1", "192.0.2.1\n", "-o"} {
		e := base
		e.Clients = []string{client}
		if _, err := e.normalized(); err == nil {
			t.Fatalf("unsafe/mixed-family client %q accepted", client)
		}
	}
	e := base
	e.BindAddress = "2001:db8::10"
	e.Clients = []string{"2001:db8::20", "2001:0db8::20"}
	validated, err := e.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(validated.Clients, []string{"2001:db8::20"}) {
		t.Fatal("equivalent IPv6 addresses were not deduplicated")
	}
}

func TestRevocationAfterDaemonFailureIsDurable(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	r := &memoryRuntime{}
	m := testManager(t, r, stateDir)
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	r.failure(errors.New("mountd stopped"))
	if err := m.ChangeClient(t.Context(), e.VolumeID, "192.0.2.20", false); err != nil {
		t.Fatalf("deny after failure: %v", err)
	}
	if err := m.Health(); err == nil {
		t.Fatal("revoke falsely healed the daemon")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := testManager(t, r, stateDir)
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.rows) != 0 {
		t.Fatal("restart re-admitted a client revoked while daemon was down")
	}
	if err := restarted.Remove(t.Context(), e.VolumeID); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareAndNoopPreserveLiveAdmissions(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	if err := m.Put(t.Context(), e, true, false); err != nil {
		t.Fatal(err)
	}
	if m.started || len(m.state.Desired) != 0 {
		t.Fatal("preparation created runtime or durable desired state")
	}
	if _, err := os.Stat(filepath.Join(m.config.StateDir, "owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation wrote ownership state: %v", err)
	}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	before := slices.Clone(r.rows)
	data, err := os.ReadFile(filepath.Join(m.config.StateDir, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.rejectWrites = true
	t.Cleanup(func() { r.rejectWrites = false })
	changed := e
	changed.Clients = []string{"192.0.2.21"}
	putErr := m.Put(t.Context(), changed, true, false)
	if putErr != nil {
		t.Fatal(putErr)
	}
	after, err := os.ReadFile(filepath.Join(m.config.StateDir, "owner.json"))
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("preparation changed durable admissions: %v", err)
	}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatalf("no-op required kernel mutation: %v", err)
	}
	if !reflect.DeepEqual(r.rows, before) || m.Health() != nil {
		t.Fatal("prepare/no-op disrupted live admissions")
	}
}

func TestUnavailableDatasetIsolationRecoveryAndRemoval(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	r := &memoryRuntime{invalid: map[string]error{}}
	m := testManager(t, r, stateDir)
	good := testExport(t, m)
	good.Clients = []string{"192.0.2.20"}
	bad := testExport(t, m)
	bad.VolumeID = "pool/unmounted"
	bad.Clients = []string{"192.0.2.21"}
	for _, e := range []Export{good, bad} {
		if err := m.Put(t.Context(), e, true, true); err != nil {
			t.Fatal(err)
		}
	}
	// Restart with one dataset unavailable, keeping the previous export table.
	if err := releaseLock(m.lock); err != nil {
		t.Fatal(err)
	}
	m.lock = nil
	m.closed = true
	r.invalid[bad.VolumeID] = errors.New("dataset is not mounted")
	restarted := testManager(t, r, stateDir)
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatalf("bad dataset disabled shared runtime startup: %v", err)
	}
	assertOnlyGoodDataset(t, r, good, bad)
	if err := restarted.Health(); err == nil || !strings.Contains(err.Error(), bad.VolumeID) {
		t.Fatalf("unavailable dataset not exposed by health: %v", err)
	}
	if err := restarted.Put(t.Context(), bad, true, true); err == nil {
		t.Fatal("unmounted dataset reconcile succeeded")
	}
	if err := restarted.Put(t.Context(), good, true, true); err != nil {
		t.Fatalf("healthy dataset reconcile failed: %v", err)
	}
	assertOnlyGoodDataset(t, r, good, bad)
	delete(r.invalid, bad.VolumeID)
	if err := restarted.Put(t.Context(), bad, true, true); err != nil {
		t.Fatalf("remounted dataset could not recover: %v", err)
	}
	if restarted.Health() != nil || !reflect.DeepEqual(admitted(r), []string{"192.0.2.20", "192.0.2.21"}) {
		t.Fatal("recovery did not restore exact admissions and health")
	}
	exerciseUnavailableDatasetRemoval(t, restarted, r, good, bad)
}

func exerciseUnavailableDatasetRemoval(t *testing.T, restarted *Manager, r *memoryRuntime, good, bad Export) {
	t.Helper()
	r.invalid[bad.VolumeID] = errors.New("dataset disappeared")
	if err := restarted.Put(t.Context(), bad, true, true); err == nil {
		t.Fatal("disappeared dataset was reported successfully reconciled")
	}
	assertOnlyGoodDataset(t, r, good, bad)
	if err := restarted.ChangeClient(t.Context(), bad.VolumeID, bad.Clients[0], false); err != nil {
		t.Fatalf("unavailable dataset denial failed: %v", err)
	}
	assertOnlyGoodDataset(t, r, good, bad)
	if err := restarted.Remove(t.Context(), bad.VolumeID); err != nil {
		t.Fatalf("unavailable dataset removal failed: %v", err)
	}
	if restarted.Health() != nil {
		t.Fatal("removal retained unavailable dataset health error")
	}
	assertOnlyGoodDataset(t, r, good, bad)
}

func TestCallerCancellationDoesNotInterruptCommittedAdmissions(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	foreign := entry{Path: "/foreign", Client: "*", Options: "rw,fsid=777"}
	r.rows = append(r.rows, foreign)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.Clients = []string{"192.0.2.21"}
	if err := m.Put(ctx, e, true, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-commit cancellation accepted: %v", err)
	}
	if !reflect.DeepEqual(admitted(r), []string{"*", "192.0.2.20"}) || m.Health() != nil {
		t.Fatal("pre-commit cancellation altered admissions or health")
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	r.cancelGrant = cancel
	if err := m.ChangeClient(ctx, e.VolumeID, "192.0.2.21", true); err != nil {
		t.Fatalf("committed grant inherited caller cancellation: %v", err)
	}
	if ctx.Err() == nil || m.Health() != nil {
		t.Fatal("caller cancellation was not exercised or disabled the runtime")
	}
	if !reflect.DeepEqual(admitted(r), []string{"*", "192.0.2.20", "192.0.2.21"}) || !slices.Contains(r.rows, foreign) {
		t.Fatalf("cancellation lost peers or foreign export: %#v", r.rows)
	}
}

func TestRuntimeValidationFailureRemainsFailClosed(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{invalid: map[string]error{}}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	r.invalid[e.VolumeID] = &runtimeValidationError{errors.New("mount table unavailable")}
	if err := m.ChangeClient(t.Context(), e.VolumeID, "192.0.2.21", true); err == nil {
		t.Fatal("shared validation failure was hidden")
	}
	if r.alive || len(r.rows) != 0 || m.Health() == nil {
		t.Fatal("shared runtime failure was treated as an isolated dataset")
	}
}

func TestConvergenceTimeoutFailurePreservesForeignExports(t *testing.T) {
	t.Parallel()
	r := &memoryRuntime{}
	m := testManager(t, r, t.TempDir())
	e := testExport(t, m)
	e.Clients = []string{"192.0.2.20"}
	if err := m.Put(t.Context(), e, true, true); err != nil {
		t.Fatal(err)
	}
	foreign := entry{Path: "/foreign", Client: "*", Options: "rw,fsid=777"}
	r.rows = append(r.rows, foreign)
	r.grantError = context.DeadlineExceeded
	err := m.ChangeClient(t.Context(), e.VolumeID, "192.0.2.21", true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("finite operation timeout was hidden: %v", err)
	}
	if r.alive || !reflect.DeepEqual(r.rows, []entry{foreign}) || m.Health() == nil {
		t.Fatalf("runtime timeout left partial owned state or changed foreign admissions: %#v", r.rows)
	}
}
