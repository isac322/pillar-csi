package e2e

// setup_phase_log.go — Sub-AC 6.2: structured append-only timing log for
// BeforeSuite, BeforeEach, and JustBeforeEach setup phases.
//
// The setup-phase log is a JSON-Lines file that accumulates one record per
// setup-phase event during the suite run:
//
//	{"phase":"before_suite","startedAt":"...","finishedAt":"...","durationNanos":12345}
//	{"phase":"before_each","tcID":"E1.2","parallelProcess":2,"startedAt":"...","durationNanos":12345}
//	{"phase":"just_before_each","tcID":"E1.2","parallelProcess":2,"startedAt":"...","durationNanos":1234}
//
// The log is written incrementally: BeforeEach and JustBeforeEach entries are
// appended as each spec's setup completes (via the DeferCleanup registered in
// timing_capture.go's BeforeEach hook); the BeforeSuite entry is appended via
// the ReportAfterSuite hook in profile_report.go when the consolidated Ginkgo
// report becomes available.
//
// File location: configured via the -e2e.setup-timing-log flag (default: "").
// When the path is empty, all Append calls are silent no-ops.
//
// Thread safety: Append is safe to call concurrently from multiple Ginkgo
// parallel worker goroutines.

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	// setupPhaseBeforeSuite is the phase name used for suite-level BeforeSuite
	// timing entries in the setup phase log.
	setupPhaseBeforeSuite = "before_suite"

	// setupPhaseBeforeEach is the phase name for per-TC BeforeEach timing entries.
	setupPhaseBeforeEach = "before_each"

	// setupPhaseJustBeforeEach is the phase name for per-TC JustBeforeEach
	// timing entries in the setup phase log.
	setupPhaseJustBeforeEach = "just_before_each"

	// setupPhaseAfterEach is the phase name for per-TC AfterEach timing entries.
	// It spans from when the spec body (phaseSpecBody) ends (in JustAfterEach)
	// to when the DeferCleanup installed by timing_capture.go's BeforeEach fires.
	// This captures all AfterEach work that executes after the spec body, including
	// per-TC teardown registered via DeferCleanup in nested BeforeEach blocks.
	setupPhaseAfterEach = "after_each"

	// setupPhaseAfterSuite is the phase name used for suite-level AfterSuite
	// timing entries in the setup phase log.
	setupPhaseAfterSuite = "after_suite"
)

// setupPhaseLogEntry is one record in the setup-phase structured timing log.
// It captures the wall-clock start and finish timestamps plus the computed
// duration for a single Ginkgo setup hook invocation.
type setupPhaseLogEntry struct {
	// Phase identifies the Ginkgo hook that produced this entry.
	// One of: "before_suite", "before_each", "just_before_each".
	Phase string `json:"phase"`

	// TCID is the test-case identifier (e.g. "E1.2") for per-TC phases.
	// Empty for suite-level phases (BeforeSuite).
	TCID string `json:"tcID,omitempty"`

	// ParallelProcess is the Ginkgo worker process number (1-based).
	// 0 means the value was not recorded (e.g. suite-level BeforeSuite entry).
	ParallelProcess int `json:"parallelProcess,omitempty"`

	// StartedAt is the UTC wall-clock time when the setup phase began.
	StartedAt time.Time `json:"startedAt"`

	// FinishedAt is the UTC wall-clock time when the setup phase ended.
	FinishedAt time.Time `json:"finishedAt"`

	// DurationNanos is the wall-clock duration of the phase in nanoseconds.
	// Equals FinishedAt.Sub(StartedAt).Nanoseconds().
	DurationNanos int64 `json:"durationNanos"`
}

// Duration returns the setup phase duration as a time.Duration.
func (e setupPhaseLogEntry) Duration() time.Duration {
	return time.Duration(e.DurationNanos)
}

// setupPhaseLogWriter is the minimal interface required by the timing
// integration points. Both the file-based logger and the in-memory test
// double implement it.
type setupPhaseLogWriter interface {
	Append(entry setupPhaseLogEntry) error
}

// fileSetupPhaseLogger appends setupPhaseLogEntry records as JSON-Lines to a
// file. The file is created (or opened for appending) on the first Append
// call so that no empty files are created when the flag is set but no phases
// complete (e.g. very early suite failure).
type fileSetupPhaseLogger struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// newFileSetupPhaseLogger creates a logger that writes to path.
// When path is empty, Append is a silent no-op.
func newFileSetupPhaseLogger(path string) *fileSetupPhaseLogger {
	return &fileSetupPhaseLogger{path: path}
}

// Append serialises entry as a JSON-Lines record and appends it to the log
// file. The first call opens (or creates) the file. Safe for concurrent use.
func (l *fileSetupPhaseLogger) Append(entry setupPhaseLogEntry) error {
	if l == nil || l.path == "" {
		return nil
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("setup phase log: encode entry: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		f, openErr := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if openErr != nil {
			return fmt.Errorf("setup phase log: open %s: %w", l.path, openErr)
		}
		l.f = f
	}

	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("setup phase log: write: %w", err)
	}
	return nil
}

// Close flushes and closes the underlying file if it was opened.
// Safe to call on a nil or no-path logger; idempotent after the first call.
func (l *fileSetupPhaseLogger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		err := l.f.Close()
		l.f = nil
		return err
	}
	return nil
}

// inMemorySetupPhaseLogger accumulates entries in a slice and is used by unit
// tests to verify that the correct entries are appended without touching the
// file system.
type inMemorySetupPhaseLogger struct {
	mu      sync.Mutex
	entries []setupPhaseLogEntry
}

// newInMemorySetupPhaseLogger creates a fresh in-memory setup phase log accumulator.
func newInMemorySetupPhaseLogger() *inMemorySetupPhaseLogger {
	return &inMemorySetupPhaseLogger{}
}

// Append records the entry in memory. Always succeeds.
func (m *inMemorySetupPhaseLogger) Append(entry setupPhaseLogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entry)
	return nil
}

// Snapshot returns a copy of all accumulated entries in insertion order.
// Returns nil when the log is empty. The caller may sort or filter the
// returned slice without affecting the underlying log.
func (m *inMemorySetupPhaseLogger) Snapshot() []setupPhaseLogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) == 0 {
		return nil
	}
	out := make([]setupPhaseLogEntry, len(m.entries))
	copy(out, m.entries)
	return out
}

// Len returns the number of entries accumulated so far.
func (m *inMemorySetupPhaseLogger) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// ─────────────────────────────────────────────────────────────────────────────
// Package-level logger (replaced by configureSuiteExecution when the flag is set)
// ─────────────────────────────────────────────────────────────────────────────

// suiteSetupPhaseLog is the package-level setup-phase log writer used by all
// timing integration hooks. It is initialised as a silent no-op logger and
// replaced by configureSuiteExecution when -e2e.setup-timing-log is set.
//
// Tests may swap this variable for an inMemorySetupPhaseLogger using
// installTestSetupPhaseLog.
var suiteSetupPhaseLog setupPhaseLogWriter = newFileSetupPhaseLogger("")

// appendSetupPhaseEntry appends entry to suiteSetupPhaseLog. Errors are
// discarded silently — timing log failures must not abort TC execution.
func appendSetupPhaseEntry(entry setupPhaseLogEntry) {
	if suiteSetupPhaseLog == nil {
		return
	}
	_ = suiteSetupPhaseLog.Append(entry)
}

// installTestSetupPhaseLog replaces suiteSetupPhaseLog for the duration of
// the test t. The original value is restored via t.Cleanup. Returns the
// installed inMemorySetupPhaseLogger so the caller can inspect it.
func installTestSetupPhaseLog(t interface {
	Helper()
	Cleanup(func())
}) *inMemorySetupPhaseLogger {
	t.Helper()
	logger := newInMemorySetupPhaseLogger()
	original := suiteSetupPhaseLog
	suiteSetupPhaseLog = logger
	t.Cleanup(func() {
		suiteSetupPhaseLog = original
	})
	return logger
}
