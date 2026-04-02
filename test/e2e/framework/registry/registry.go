// Package registry provides a thread-safe registry for ephemeral E2E test
// resources (ZFS pools, LVM VGs, iSCSI targets, …) created inside Kind
// container nodes during a test run.
//
// # Motivation
//
// Each E2E test that creates an ephemeral storage resource must ensure the
// resource is destroyed after the test completes — even when the process exits
// abnormally (panic, SIGINT, SIGTERM). A plain "defer pool.Destroy(ctx)" is
// insufficient for signal termination because Go's deferred functions do not
// run when the process receives a SIGINT or SIGTERM by default.
//
// The Registry solves this by combining:
//
//  1. A centralized list of all live resources so cleanup iterates exactly once
//     across all registered resources.
//  2. OS signal handlers (SIGINT, SIGTERM) that invoke [Registry.Cleanup] before
//     exiting, installed via [Registry.InstallSignalHandlers].
//  3. Idempotent cleanup: the first call to [Registry.Cleanup] destroys all
//     resources and marks the registry as cleaned; subsequent calls are no-ops.
//
// # Extensibility
//
// The Registry is backend-agnostic.  Any type that implements [Resource]
// (Destroy + Description) can be registered via [Registry.Register] without
// any changes to this package.  Convenience wrappers exist for the built-in
// backends:
//
//   - [Registry.RegisterZFSPool]      — wraps zfs.Pool
//   - [Registry.RegisterLVMVG]        — wraps lvm.VG
//   - [Registry.RegisterISCSITarget]  — wraps iscsi.Target
//
// To add a new backend type:
//
//  1. Implement the [Resource] interface on your type.
//  2. Call reg.Register(yourResource).
//  3. No registry code changes are required.
//
// # Typical usage
//
//	reg := registry.New()
//	cancel := reg.InstallSignalHandlers(os.Stderr, os.Exit)
//	defer cancel()           // remove signal handlers when suite ends normally
//	defer reg.Cleanup(ctx)   // destroy resources on normal / panic exit
//
//	// In each test:
//	pool, err := zfs.CreatePool(ctx, opts)
//	if err != nil { … }
//	reg.Register(pool)   // or reg.RegisterZFSPool(pool) — identical effect
//
// # Thread safety
//
// All methods are safe for concurrent use from multiple goroutines.  The
// registry uses a single mutex to protect its internal state.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/iscsi"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// Registry is a thread-safe store of ephemeral storage resources created
// during a test run.  It guarantees that every registered resource is
// destroyed exactly once by combining defer-based cleanup with OS signal
// handling.
//
// The zero value is NOT usable.  Always create a Registry with [New].
//
// The Registry is backend-agnostic: any type implementing [Resource] can be
// registered via [Register] without modifying this package.
type Registry struct {
	mu sync.Mutex

	// resources holds every registered [Resource] in registration order.
	// All backend types (ZFS, LVM, iSCSI, …) are stored here via the common
	// interface so that new backends require zero framework changes.
	resources []Resource

	// cleaned is set to true on the first call to cleanupLocked so that
	// subsequent calls are no-ops.
	cleaned bool
}

// New allocates and returns a new, empty Registry.
//
// The caller should install cleanup handlers immediately after creation:
//
//	reg := registry.New()
//	cancel := reg.InstallSignalHandlers(os.Stderr, os.Exit)
//	defer cancel()
//	defer reg.Cleanup(context.Background())
func New() *Registry {
	return &Registry{}
}

// ── Generic registration ───────────────────────────────────────────────────

// Register adds r to the registry.  After registration the registry will call
// r.Destroy during [Registry.Cleanup].
//
// Register is the primary extensibility point: any type that implements
// [Resource] can be registered here without any changes to this package.
//
// Passing a nil Resource is a safe no-op — nil resources are silently ignored.
func (r *Registry) Register(res Resource) {
	if res == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources = append(r.resources, res)
}

// ResourceCount returns the number of resources currently registered
// (all backend types combined).  Resources that have been cleaned up after
// [Cleanup] is called are not counted.
func (r *Registry) ResourceCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.resources)
}

// ── Convenience wrappers for built-in backends ─────────────────────────────

// RegisterZFSPool adds pool to the registry.  It is equivalent to calling
// [Register](pool).  After registration the registry will call pool.Destroy
// during [Registry.Cleanup].
//
// Passing a nil pool is a safe no-op — nil pools are silently ignored.
func (r *Registry) RegisterZFSPool(pool *zfs.Pool) {
	if pool == nil {
		return
	}
	r.Register(pool)
}

// RegisterLVMVG adds vg to the registry.  It is equivalent to calling
// [Register](vg).  After registration the registry will call vg.Destroy
// during [Registry.Cleanup].
//
// Passing a nil VG is a safe no-op — nil VGs are silently ignored.
func (r *Registry) RegisterLVMVG(vg *lvm.VG) {
	if vg == nil {
		return
	}
	r.Register(vg)
}

// RegisterISCSITarget adds target to the registry.  It is equivalent to
// calling [Register](target).  After registration the registry will call
// target.Destroy during [Registry.Cleanup].
//
// Passing a nil target is a safe no-op — nil targets are silently ignored.
func (r *Registry) RegisterISCSITarget(target *iscsi.Target) {
	if target == nil {
		return
	}
	r.Register(target)
}

// ── Type-filtered counts (convenience for tests & diagnostics) ─────────────

// ZFSPoolCount returns the number of ZFS pools currently registered.
// Pools that have already been cleaned up (after [Cleanup] is called) are not
// counted.
func (r *Registry) ZFSPoolCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, res := range r.resources {
		if _, ok := res.(*zfs.Pool); ok {
			n++
		}
	}
	return n
}

// LVMVGCount returns the number of LVM VGs currently registered.
// VGs that have already been cleaned up (after [Cleanup] is called) are not
// counted.
func (r *Registry) LVMVGCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, res := range r.resources {
		if _, ok := res.(*lvm.VG); ok {
			n++
		}
	}
	return n
}

// ISCSITargetCount returns the number of iSCSI targets currently registered.
// Targets that have already been cleaned up (after [Cleanup] is called) are
// not counted.
func (r *Registry) ISCSITargetCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, res := range r.resources {
		if _, ok := res.(*iscsi.Target); ok {
			n++
		}
	}
	return n
}

// ── Cleanup ────────────────────────────────────────────────────────────────

// Cleanup destroys all registered resources, collecting every error so that a
// failure in one resource's teardown does not prevent the others from being
// cleaned up.
//
// Resources are destroyed in reverse registration order (LIFO) to mirror the
// typical dependency order: the last resource created is the first destroyed.
//
// Cleanup is idempotent: the first call performs teardown and marks the
// registry as cleaned; every subsequent call is a no-op that returns nil.
//
// Cleanup is safe to call concurrently from multiple goroutines; only one
// goroutine will perform the actual teardown — the others return immediately.
//
// Calling Cleanup on a nil *Registry is a safe no-op.
func (r *Registry) Cleanup(ctx context.Context) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	if r.cleaned {
		r.mu.Unlock()
		return nil
	}
	// Take snapshot and mark as cleaned while holding the lock so that
	// concurrent callers see cleaned==true and return immediately.
	snapshot := r.resources
	r.resources = nil
	r.cleaned = true
	r.mu.Unlock()

	var errs []error

	// Destroy resources in reverse registration order (LIFO).
	for i := len(snapshot) - 1; i >= 0; i-- {
		res := snapshot[i]
		if err := res.Destroy(ctx); err != nil {
			errs = append(errs, fmt.Errorf("resource %s: %w", res.Description(), err))
		}
	}

	return errors.Join(errs...)
}

// ── Signal handling ────────────────────────────────────────────────────────

// InstallSignalHandlers registers OS-level handlers for SIGINT and SIGTERM
// that call [Registry.Cleanup] and then invoke exit with the conventional
// signal exit code (128 + signal number).
//
// output is used to print a diagnostic message when a signal is received.
// Passing nil uses [io.Discard].
//
// exit is called with the exit code after cleanup.  Pass os.Exit in production
// and a custom function in tests.  Passing nil skips the os.Exit call (useful
// in tests that want to observe the cleanup without terminating the process).
//
// The returned cancel function unregisters the signal handlers.  Always call
// it (e.g. via defer) when the test suite ends normally so that the handlers
// do not interfere with the process's default signal behaviour after the suite
// has finished cleaning up on its own.
//
// InstallSignalHandlers is safe to call concurrently, but installing multiple
// handlers for the same Registry is discouraged — each call creates an
// independent goroutine and channel.
func (r *Registry) InstallSignalHandlers(output io.Writer, exit func(int)) func() {
	if r == nil {
		return func() {}
	}
	if output == nil {
		output = io.Discard
	}

	signals := make(chan os.Signal, 1)
	stop := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-signals:
			r.handleSignal(sig, output, exit)
		case <-stop:
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(signals)
			close(stop)
		})
	}
}

// handleSignal is invoked by the goroutine spawned in InstallSignalHandlers
// when a SIGINT or SIGTERM is received.  It prints a diagnostic message,
// runs cleanup, and calls exit.
func (r *Registry) handleSignal(sig os.Signal, output io.Writer, exit func(int)) {
	if sig == nil {
		return
	}

	_, _ = fmt.Fprintf(output,
		"registry: received %s — cleaning up ephemeral storage resources\n", sig)

	ctx, cancel := context.WithTimeout(context.Background(), cleanupSignalTimeout)
	defer cancel()

	if err := r.Cleanup(ctx); err != nil {
		_, _ = fmt.Fprintf(output, "registry: cleanup error: %v\n", err)
	}

	if exit != nil {
		exit(exitCodeForSignal(sig))
	}
}

// cleanupSignalTimeout is the maximum time allowed for cleanup when triggered
// by a signal.  30 seconds is generous enough for zpool destroy + vgremove on
// small (512 MiB) loop devices while still being short enough that the process
// does not hang indefinitely.
const cleanupSignalTimeout = 30 * 1_000_000_000 // 30 s in nanoseconds (time.Duration)

// exitCodeForSignal follows the Unix convention of 128 + signal number for
// processes killed by a signal.
func exitCodeForSignal(sig os.Signal) int {
	sv, ok := sig.(syscall.Signal)
	if !ok {
		return 1
	}
	return 128 + int(sv)
}
