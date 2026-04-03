// Package framework — isolation_check.go
//
// IsolationChecker enforces TC-level resource isolation by maintaining a
// process-scoped registry of active TC scope root directories and scanning
// /tmp for orphaned directories after each spec's cleanup phase.
//
// # Design
//
// Every TC scope creates a unique directory under /tmp with a name of the form:
//
//	pillar-csi-<tcSlug>-p<pid>-s<seq>-<rand>
//
// The PID component allows the checker to restrict its scan to directories
// created by the current test process, avoiding interference from other
// concurrent test runs.
//
// The registry tracks which of those directories are "active" (the scope has
// been created and not yet closed).  After a TC's Close() method is called,
// its root directory is deregistered.  Any directory that remains on disk after
// deregistration is an orphan — evidence that Close() was not called or that
// it failed to remove the directory.
//
// # Integration
//
//   - Call [RegisterActiveScope] from NewTestCaseScope immediately after the
//     /tmp root directory is created.
//   - Call [DeregisterActiveScope] at the very start of TestCaseScope.Close()
//     (before removing the directory) so that the scope is immediately removed
//     from the active set.  If Close() subsequently fails to remove the
//     directory, the next scan will detect it as an orphan.
//   - Call [InstallAfterEachIsolationCheck] from within the outermost Describe
//     container (or a BeforeSuite) to register the per-spec AfterEach hook.
//
// # Background cleanup
//
// CloseBackground() fires Close() in a goroutine.  For the duration of the
// background cleanup, the scope's directory is still being removed, so it
// should remain in the active registry until Close() begins executing.
// Because [DeregisterActiveScope] is called at the start of Close() (inside
// the goroutine), the directory stays registered while the goroutine is
// queued but not yet running.  This prevents spurious orphan alerts during the
// brief window between goroutine launch and goroutine execution.
//
// # Ginkgo hook timing
//
// [InstallAfterEachIsolationCheck] registers an AfterEach node.  In Ginkgo v2,
// AfterEach runs BEFORE DeferCleanup from BeforeEach.  This means:
//
//   - The CURRENT TC's scope is still registered when AfterEach runs
//     (DeferCleanup from BeforeEach, which calls Close/CloseBackground, has
//     not yet fired).
//   - Previous TCs whose background cleanup is still in progress keep their
//     scope registered while the goroutine is live.
//   - Any dir that is not in the registry IS orphaned — it came from a scope
//     whose Close() was called (deregistered) but whose root dir was not
//     removed, or a scope that was never closed.
package framework

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ─── IsolationViolationErrorKind ───────────────────────────────────────────────────

// IsolationViolationErrorKind classifies an isolation boundary breach detected by
// the post-cleanup scanner.
type IsolationViolationErrorKind string

const (
	// ViolationOrphanedTempDir is set when a /tmp directory that was created by
	// this process is found on disk but is NOT owned by any currently-active TC
	// scope.  This indicates that a previous TC's Close() was never called, or
	// that Close() failed to remove the root directory.
	ViolationOrphanedTempDir IsolationViolationErrorKind = "orphaned_temp_dir"
)

// ─── IsolationViolationError ───────────────────────────────────────────────────────

// IsolationViolationError describes a single detected isolation breach.
type IsolationViolationError struct {
	// Kind classifies the type of isolation breach.
	Kind IsolationViolationErrorKind

	// RootDir is the /tmp directory involved in the violation.
	RootDir string

	// Detail is a human-readable explanation suitable for test failure output.
	Detail string
}

// Error implements the error interface so violations can be surfaced as errors.
func (v *IsolationViolationError) Error() string {
	return fmt.Sprintf("[isolation:%s] %s — %s", v.Kind, v.RootDir, v.Detail)
}

// String returns the same representation as Error.
func (v *IsolationViolationError) String() string {
	return v.Error()
}

// ─── scopeEntry ───────────────────────────────────────────────────────────────

// scopeEntry records the minimal metadata for one registered TC scope.
type scopeEntry struct {
	TCID       string
	ScopeTag   string
	RegisterAt time.Time
}

// ─── scopeRegistry ────────────────────────────────────────────────────────────

// scopeRegistry is a process-scoped ledger that tracks which TC scope root
// directories are currently active.  It is the single source of truth used
// by the isolation checker to distinguish live scope directories from orphaned
// remnants left by incomplete or failed cleanup.
//
// The zero value is not usable; always construct via [newScopeRegistry].
type scopeRegistry struct {
	mu     sync.RWMutex
	active map[string]*scopeEntry // key: absolute rootDir path
	pid    int                    // current process PID
}

// newScopeRegistry creates a fresh registry bound to the current process PID.
func newScopeRegistry() *scopeRegistry {
	return &scopeRegistry{
		active: make(map[string]*scopeEntry),
		pid:    os.Getpid(),
	}
}

// register records rootDir as an active scope.  Calling register with an empty
// path is a safe no-op.  Repeated registration of the same path overwrites the
// previous entry (idempotent).
func (r *scopeRegistry) register(rootDir, tcID, scopeTag string) {
	if strings.TrimSpace(rootDir) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active[rootDir] = &scopeEntry{
		TCID:       tcID,
		ScopeTag:   scopeTag,
		RegisterAt: time.Now(),
	}
}

// deregister removes rootDir from the active set.  Calling deregister with an
// empty path or a path that was never registered is a safe no-op.
func (r *scopeRegistry) deregister(rootDir string) {
	if strings.TrimSpace(rootDir) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, rootDir)
}

// isActive returns true if rootDir is currently registered as an active scope.
func (r *scopeRegistry) isActive(rootDir string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.active[rootDir]
	return ok
}

// activeCount returns the number of currently-registered active scopes.
// Intended for diagnostics.
func (r *scopeRegistry) activeCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.active)
}

// orphanedDirs scans /tmp for directories matching the pillar-csi temp dir
// naming convention for this process, then returns a violation for every
// directory that exists on disk but is NOT in the active-scope registry.
//
// The naming convention is: pillar-csi-*-p<pid>-*
// (see isolation_scope.go: os.MkdirTemp("/tmp", "pillar-csi-<slug>-p<pid>-s<seq>-"))
//
// Directories that belong to active scopes are silently skipped.
func (r *scopeRegistry) orphanedDirs() ([]*IsolationViolationError, error) {
	pattern := filepath.Join("/tmp", fmt.Sprintf("pillar-csi-*-p%d-*", r.pid))

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("isolation-check: glob %q: %w", pattern, err)
	}

	if len(matches) == 0 {
		return nil, nil
	}

	// Take a snapshot of active dirs while holding the read lock so that the
	// active-state check and the disk-existence check are consistent.
	r.mu.RLock()
	activeCopy := make(map[string]bool, len(r.active))
	for k := range r.active {
		activeCopy[k] = true
	}
	r.mu.RUnlock()

	var violations []*IsolationViolationError
	for _, dir := range matches {
		if activeCopy[dir] {
			// Directory belongs to a currently-active scope — legitimate.
			continue
		}

		// Directory is on disk but not in the active registry.  Verify it
		// really still exists (racing concurrent cleanup could remove it
		// between the glob and the check).
		if _, statErr := os.Lstat(dir); os.IsNotExist(statErr) {
			// Cleanup raced us — the dir is now gone; no violation.
			continue
		}

		violations = append(violations, &IsolationViolationError{
			Kind:    ViolationOrphanedTempDir,
			RootDir: dir,
			Detail: fmt.Sprintf(
				"directory %q exists on disk but is not owned by any active TC scope "+
					"(Close() was not called, or was called but failed to remove the directory)",
				dir,
			),
		})
	}

	return violations, nil
}

// ─── Global registry ─────────────────────────────────────────────────────────

// globalScopeRegistry is the process-scoped isolation boundary ledger used by
// all exported functions in this file.
//
// It is initialised once at package load time and is safe for concurrent use
// from multiple goroutines (Ginkgo parallel workers run in separate processes,
// so no cross-process synchronisation is required).
var globalScopeRegistry = newScopeRegistry()

// ─── Public API ───────────────────────────────────────────────────────────────

// RegisterActiveScope records rootDir as an active TC scope directory.
//
// This function MUST be called by NewTestCaseScope immediately after the /tmp
// root directory is created and before the scope is returned to the caller.
// Failure to call it means the directory will be treated as an orphan the next
// time [ScanOrphanedTempDirs] runs.
//
// The tcID and scopeTag parameters are stored for diagnostic output only; they
// do not affect orphan detection.
//
// Calling RegisterActiveScope with an empty rootDir is a safe no-op.
func RegisterActiveScope(rootDir, tcID, scopeTag string) {
	globalScopeRegistry.register(rootDir, tcID, scopeTag)
}

// DeregisterActiveScope removes rootDir from the active-scope registry.
//
// This function MUST be called by TestCaseScope.Close() at the very start of
// the cleanup phase, before os.RemoveAll is called on the root directory.
// Once deregistered, any subsequent discovery of rootDir on disk is reported
// as an isolation violation by [ScanOrphanedTempDirs].
//
// Calling DeregisterActiveScope with an empty rootDir or a path that was never
// registered is a safe no-op.
func DeregisterActiveScope(rootDir string) {
	globalScopeRegistry.deregister(rootDir)
}

// IsScopeActive returns true if rootDir is currently registered as an active
// TC scope.  Intended for diagnostics and testing.
func IsScopeActive(rootDir string) bool {
	return globalScopeRegistry.isActive(rootDir)
}

// ActiveScopeCount returns the number of currently-registered active TC scopes.
// Useful for asserting no scope leaks in suite-level tests.
func ActiveScopeCount() int {
	return globalScopeRegistry.activeCount()
}

// ScanOrphanedTempDirs returns isolation violations for any /tmp directories
// from this process that exist on disk but are NOT owned by an active TC scope.
//
// A violation indicates that a previous TC's cleanup failed: either Close() was
// never called, or Close() returned an error without removing the root dir.
// Any such directory could contaminate a concurrently-running TC that happens
// to scan the filesystem.
//
// Returns (nil, nil) when no orphans are found.
func ScanOrphanedTempDirs() ([]*IsolationViolationError, error) {
	return globalScopeRegistry.orphanedDirs()
}

// ─── Ginkgo AfterEach integration ────────────────────────────────────────────

// InstallAfterEachIsolationCheck registers an AfterEach node that scans for
// orphaned TC /tmp directories after each spec's body finishes.
//
// In Ginkgo v2, AfterEach runs before DeferCleanup from BeforeEach.  This
// ordering means:
//
//   - The CURRENT TC's scope is still registered when the check runs, so it
//     is NOT treated as an orphan.
//   - Previous TCs whose background cleanup goroutine has started (and whose
//     Close() call deregistered the scope) will be caught if their root dir
//     was not removed.
//   - Background-closing scopes that have not yet called Close() are still
//     registered and thus excluded from the scan.
//
// The hook fails the current spec via Gomega's Expect if orphaned directories
// are found.  Because each spec gets its own AfterEach run, isolation failures
// are reported immediately after the offending TC completes.
//
// Usage — call this from within the outermost container or a per-file Describe:
//
//	var _ = Describe("pillar-csi E2E suite", func() {
//	    framework.InstallAfterEachIsolationCheck()
//	    // ... test cases ...
//	})
//
// Or during suite setup, if the suite has a single top-level Describe:
//
//	var _ = BeforeSuite(func() {
//	    // note: AfterEach cannot be registered from BeforeSuite; use a
//	    // top-level Describe wrapper instead.
//	})
func InstallAfterEachIsolationCheck() {
	AfterEach(func() {
		GinkgoHelper()

		violations, err := ScanOrphanedTempDirs()
		Expect(err).NotTo(HaveOccurred(),
			"[isolation-check] scanning /tmp for orphaned TC directories failed — "+
				"check that the test binary has read access to /tmp",
		)

		if len(violations) == 0 {
			return
		}

		// Build a descriptive failure message listing every orphaned directory.
		Expect(violations).To(BeEmpty(),
			"[isolation-check] %d orphaned TC temp dir(s) detected after cleanup.\n"+
				"Each entry is a /tmp directory from a previous TC whose Close() was\n"+
				"not called or failed to remove the directory — potential cross-TC\n"+
				"state contamination.\n\n%s",
			len(violations),
			formatViolations(violations),
		)
	})
}

// ─── Formatting helpers ───────────────────────────────────────────────────────

// formatViolations returns a multi-line human-readable description of a slice
// of isolation violations, suitable for embedding in a test failure message.
func formatViolations(violations []*IsolationViolationError) string {
	if len(violations) == 0 {
		return "  (none)"
	}
	var sb strings.Builder
	for i, v := range violations {
		if v == nil {
			continue
		}
		fmt.Fprintf(&sb, "  [%d] %s\n", i+1, v.Error())
	}
	return sb.String()
}
