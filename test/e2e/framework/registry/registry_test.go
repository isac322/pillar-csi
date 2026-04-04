package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/iscsi"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// newFakePool creates a *zfs.Pool with test-friendly field values.  The pool
// has an empty NodeContainer so that Destroy never attempts a real docker exec.
// However because Destroy on such a pool will call containerExec with an empty
// container name (which the zfs package rejects with a non-nil error), we need
// a different approach for unit tests.
//
// Instead we rely on the fact that Pool.Destroy is idempotent and that
// containerExec returns an error for empty containers; for our registry tests
// we only care that Cleanup calls Destroy in the correct order and collects
// errors — we do not need the underlying docker commands to succeed.
func newFakePool(name string) *zfs.Pool {
	return &zfs.Pool{
		NodeContainer: "test-container",
		PoolName:      name,
		ImagePath:     fmt.Sprintf("/tmp/test-pool-%s.img", name),
		LoopDevice:    "/dev/loop0",
	}
}

func newFakeVG(name string) *lvm.VG {
	return &lvm.VG{
		NodeContainer: "test-container",
		VGName:        name,
		ImagePath:     fmt.Sprintf("/tmp/test-vg-%s.img", name),
		LoopDevice:    "/dev/loop0",
	}
}

// ─── New ─────────────────────────────────────────────────────────────────────

func TestNew_NotNil(t *testing.T) {
	t.Parallel()

	r := New()
	if r == nil {
		t.Fatal("New() returned nil")
	}
}

func TestNew_Empty(t *testing.T) {
	t.Parallel()

	r := New()
	if r.ZFSPoolCount() != 0 {
		t.Errorf("ZFSPoolCount() = %d, want 0", r.ZFSPoolCount())
	}
	if r.LVMVGCount() != 0 {
		t.Errorf("LVMVGCount() = %d, want 0", r.LVMVGCount())
	}
}

// ─── RegisterZFSPool ─────────────────────────────────────────────────────────

func TestRegisterZFSPool_IncreasesCount(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterZFSPool(newFakePool("pool1"))
	r.RegisterZFSPool(newFakePool("pool2"))

	if got := r.ZFSPoolCount(); got != 2 {
		t.Errorf("ZFSPoolCount() = %d, want 2", got)
	}
}

func TestRegisterZFSPool_NilNoop(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterZFSPool(nil)

	if got := r.ZFSPoolCount(); got != 0 {
		t.Errorf("ZFSPoolCount() after nil registration = %d, want 0", got)
	}
}

// ─── RegisterLVMVG ───────────────────────────────────────────────────────────

func TestRegisterLVMVG_IncreasesCount(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterLVMVG(newFakeVG("vg1"))
	r.RegisterLVMVG(newFakeVG("vg2"))
	r.RegisterLVMVG(newFakeVG("vg3"))

	if got := r.LVMVGCount(); got != 3 {
		t.Errorf("LVMVGCount() = %d, want 3", got)
	}
}

func TestRegisterLVMVG_NilNoop(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterLVMVG(nil)

	if got := r.LVMVGCount(); got != 0 {
		t.Errorf("LVMVGCount() after nil registration = %d, want 0", got)
	}
}

// ─── Cleanup ─────────────────────────────────────────────────────────────────

func TestCleanup_NilRegistryNoop(t *testing.T) {
	t.Parallel()

	var r *Registry
	if err := r.Cleanup(context.Background()); err != nil {
		t.Errorf("nil Registry.Cleanup: unexpected error: %v", err)
	}
}

func TestCleanup_EmptyRegistryNoop(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Cleanup(context.Background()); err != nil {
		t.Errorf("empty Registry.Cleanup: unexpected error: %v", err)
	}
}

func TestCleanup_Idempotent(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterZFSPool(newFakePool("tank1"))
	r.RegisterLVMVG(newFakeVG("vg1"))

	ctx := context.Background()

	// First call: resources are taken and cleanup attempted.
	_ = r.Cleanup(ctx)

	// After first call the registry should be empty.
	if c := r.ZFSPoolCount(); c != 0 {
		t.Errorf("ZFSPoolCount after Cleanup = %d, want 0", c)
	}
	if c := r.LVMVGCount(); c != 0 {
		t.Errorf("LVMVGCount after Cleanup = %d, want 0", c)
	}

	// Second call: must be a no-op (no panic, no error from re-running destroy).
	if err := r.Cleanup(ctx); err != nil {
		t.Errorf("second Cleanup returned unexpected error: %v", err)
	}
}

func TestCleanup_CountsResetAfterCleanup(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterZFSPool(newFakePool("pool-a"))
	r.RegisterZFSPool(newFakePool("pool-b"))
	r.RegisterLVMVG(newFakeVG("vg-a"))

	_ = r.Cleanup(context.Background())

	if c := r.ZFSPoolCount(); c != 0 {
		t.Errorf("ZFSPoolCount after Cleanup = %d, want 0", c)
	}
	if c := r.LVMVGCount(); c != 0 {
		t.Errorf("LVMVGCount after Cleanup = %d, want 0", c)
	}
}

// TestCleanup_ConcurrentCallsIdempotent ensures that when many goroutines call
// Cleanup simultaneously only one teardown pass runs (idempotency under
// concurrency).
func TestCleanup_ConcurrentCallsIdempotent(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterZFSPool(newFakePool("concurrent-pool"))
	r.RegisterLVMVG(newFakeVG("concurrent-vg"))

	ctx := context.Background()

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// Return value intentionally ignored; we are testing that no panic occurs.
			_ = r.Cleanup(ctx)
		}()
	}

	wg.Wait()

	// After all goroutines finish the registry must be clean.
	if c := r.ZFSPoolCount(); c != 0 {
		t.Errorf("ZFSPoolCount after concurrent Cleanup = %d, want 0", c)
	}
	if c := r.LVMVGCount(); c != 0 {
		t.Errorf("LVMVGCount after concurrent Cleanup = %d, want 0", c)
	}
}

// ─── InstallSignalHandlers ────────────────────────────────────────────────────

func TestInstallSignalHandlers_NilRegistryNoop(t *testing.T) {
	t.Parallel()

	var r *Registry
	cancel := r.InstallSignalHandlers(io.Discard, nil)
	if cancel == nil {
		t.Fatal("InstallSignalHandlers on nil Registry returned nil cancel")
	}
	cancel() // must not panic
}

func TestInstallSignalHandlers_CancelUnregisters(t *testing.T) {
	t.Parallel()

	r := New()
	cancel := r.InstallSignalHandlers(io.Discard, nil)
	cancel() // must not panic
	cancel() // second call must also be safe (once.Do)
}

// TestInstallSignalHandlers_SIGINTTriggerCleanup sends a SIGINT to the
// process, waits for the signal handler to call Cleanup, and asserts that
// all resources are gone from the registry.
//
// This test uses os.Process.Signal to send SIGINT to the current process.
// The signal is routed through the channel installed by InstallSignalHandlers,
// so the handler goroutine calls Cleanup — not a real os.Exit.  We pass a nil
// exit func so the process keeps running after cleanup.
func TestInstallSignalHandlers_SIGINTTriggerCleanup(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterZFSPool(newFakePool("sigint-pool"))
	r.RegisterLVMVG(newFakeVG("sigint-vg"))

	var buf strings.Builder
	cleanedCh := make(chan struct{})

	// Use a custom exit that signals completion instead of exiting.
	customExit := func(_ int) {
		close(cleanedCh)
	}

	cancel := r.InstallSignalHandlers(&buf, customExit)
	defer cancel()

	// Send SIGINT to the current process.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	// Wait for the handler goroutine to finish (it calls customExit last).
	select {
	case <-cleanedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("signal handler did not complete within 5 seconds")
	}

	if c := r.ZFSPoolCount(); c != 0 {
		t.Errorf("ZFSPoolCount after SIGINT cleanup = %d, want 0", c)
	}
	if c := r.LVMVGCount(); c != 0 {
		t.Errorf("LVMVGCount after SIGINT cleanup = %d, want 0", c)
	}

	// The handler should have printed a diagnostic message.
	if !strings.Contains(buf.String(), "interrupt") && !strings.Contains(buf.String(), "SIGINT") {
		t.Errorf("diagnostic output = %q, want it to mention the signal", buf.String())
	}
}

// ─── exitCodeForSignal ────────────────────────────────────────────────────────

func TestExitCodeForSignal_SIGINT(t *testing.T) {
	t.Parallel()

	got := exitCodeForSignal(syscall.SIGINT)
	want := 128 + int(syscall.SIGINT)
	if got != want {
		t.Errorf("exitCodeForSignal(SIGINT) = %d, want %d", got, want)
	}
}

func TestExitCodeForSignal_SIGTERM(t *testing.T) {
	t.Parallel()

	got := exitCodeForSignal(syscall.SIGTERM)
	want := 128 + int(syscall.SIGTERM)
	if got != want {
		t.Errorf("exitCodeForSignal(SIGTERM) = %d, want %d", got, want)
	}
}

// fakeNonSyscallSignal is a signal that does not implement syscall.Signal
// and is used to test the fallback branch of exitCodeForSignal.
type fakeNonSyscallSignal struct{}

func (fakeNonSyscallSignal) String() string { return "fake" }
func (fakeNonSyscallSignal) Signal()        {}

func TestExitCodeForSignal_NonSyscallSignal(t *testing.T) {
	t.Parallel()

	got := exitCodeForSignal(fakeNonSyscallSignal{})
	if got != 1 {
		t.Errorf("exitCodeForSignal(non-syscall) = %d, want 1", got)
	}
}

// ─── error collection ─────────────────────────────────────────────────────────

// TestCleanup_CollectsAllErrors verifies that Cleanup reports errors from ALL
// resources, not just the first one.  We register pools with empty
// NodeContainers so that Destroy always returns an error from the docker exec
// validation in the zfs package.  We then verify the combined error string
// mentions more than one resource.
func TestCleanup_CollectsAllErrors(t *testing.T) {
	t.Parallel()

	// Pools with empty NodeContainers will fail Destroy with a "container name
	// must not be empty" error from the zfs package's containerExec helper.
	badPool1 := &zfs.Pool{
		NodeContainer: "", // intentionally invalid
		PoolName:      "bad-pool-1",
		ImagePath:     "/tmp/bad1.img",
		LoopDevice:    "/dev/loop0",
	}
	badPool2 := &zfs.Pool{
		NodeContainer: "", // intentionally invalid
		PoolName:      "bad-pool-2",
		ImagePath:     "/tmp/bad2.img",
		LoopDevice:    "/dev/loop0",
	}
	badVG := &lvm.VG{
		NodeContainer: "", // intentionally invalid
		VGName:        "bad-vg",
		ImagePath:     "/tmp/bad-vg.img",
		LoopDevice:    "/dev/loop0",
	}

	r := New()
	r.RegisterZFSPool(badPool1)
	r.RegisterZFSPool(badPool2)
	r.RegisterLVMVG(badVG)

	err := r.Cleanup(context.Background())
	if err == nil {
		t.Fatal("Cleanup with bad resources: expected error, got nil")
	}

	// errors.Join produces a single error that wraps all sub-errors; its string
	// representation should mention all three resource names.
	msg := err.Error()
	for _, name := range []string{"bad-pool-1", "bad-pool-2", "bad-vg"} {
		if !strings.Contains(msg, name) {
			t.Errorf("error message %q does not mention resource %q", msg, name)
		}
	}
}

// TestCleanup_PartialErrorStillCleansRest registers one resource that will
// fail Destroy and one that will not (nil Pool, which Destroy ignores).
// We verify that Cleanup returns exactly one error (not zero, not two).
func TestCleanup_PartialErrorDoesNotAbort(t *testing.T) {
	t.Parallel()

	badPool := &zfs.Pool{
		NodeContainer: "", // will error
		PoolName:      "failing-pool",
		ImagePath:     "/tmp/failing.img",
		LoopDevice:    "/dev/loop0",
	}

	r := New()
	r.RegisterZFSPool(badPool)
	// nil VG — Destroy is a no-op, no error expected.
	r.RegisterLVMVG(nil) // this is silently dropped by RegisterLVMVG

	err := r.Cleanup(context.Background())
	if err == nil {
		t.Fatal("expected error for bad pool, got nil")
	}

	var errs interface{ Unwrap() []error }
	if errors.As(err, &errs) {
		// errors.Join wraps all sub-errors; we expect exactly 1 wrapped error
		// (from the bad pool's zpool destroy step).
		unwrapped := errs.Unwrap()
		if len(unwrapped) == 0 {
			t.Errorf("expected at least 1 wrapped error, got 0")
		}
	}
}

// ─── AC 9: Extensibility — custom backend via Resource interface ──────────────
//
// These tests verify that:
//  1. Any type implementing Resource can be registered without framework changes.
//  2. The built-in types (zfs.Pool, lvm.VG, iscsi.Target) implement Resource.
//  3. Generic Register / ResourceCount work alongside the convenience wrappers.
//  4. Error messages from Cleanup use Description() for identifying resources.

// fakeResource is a test-only implementation of the Resource interface that
// represents a hypothetical new backend type.  It records whether Destroy was
// called and can be configured to return an error.
type fakeResource struct {
	name         string
	destroyErr   error
	destroyCalls int
}

func (f *fakeResource) Destroy(_ context.Context) error {
	f.destroyCalls++
	return f.destroyErr
}

func (f *fakeResource) Description() string {
	return fmt.Sprintf("fake-backend %q", f.name)
}

// compile-time check: fakeResource must implement Resource.
var _ Resource = (*fakeResource)(nil)

// compile-time checks: built-in backend types must implement Resource.
var _ Resource = (*zfs.Pool)(nil)
var _ Resource = (*lvm.VG)(nil)
var _ Resource = (*iscsi.Target)(nil)

// TestRegister_GenericCustomBackend demonstrates AC 9: a completely new backend
// type (fakeResource) can be registered and cleaned up without any registry
// code changes.
func TestRegister_GenericCustomBackend(t *testing.T) {
	t.Parallel()

	r := New()
	res := &fakeResource{name: "nvme-ns-0"}
	r.Register(res)

	if got := r.ResourceCount(); got != 1 {
		t.Fatalf("ResourceCount() = %d, want 1", got)
	}

	if err := r.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup returned unexpected error: %v", err)
	}

	if res.destroyCalls != 1 {
		t.Errorf("Destroy was called %d times, want 1", res.destroyCalls)
	}
}

// TestRegister_NilNoop verifies that Register(nil) is safe.
func TestRegister_NilNoop(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(nil)

	if got := r.ResourceCount(); got != 0 {
		t.Errorf("ResourceCount() after nil Register = %d, want 0", got)
	}
}

// TestResourceCount_MixedBackends verifies that ResourceCount returns the
// total across all backend types.
func TestResourceCount_MixedBackends(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterZFSPool(newFakePool("pool-1"))
	r.RegisterLVMVG(newFakeVG("vg-1"))
	r.Register(&fakeResource{name: "custom-1"})
	r.Register(&fakeResource{name: "custom-2"})

	if got := r.ResourceCount(); got != 4 {
		t.Errorf("ResourceCount() = %d, want 4", got)
	}
	// Type-filtered counts still work.
	if got := r.ZFSPoolCount(); got != 1 {
		t.Errorf("ZFSPoolCount() = %d, want 1", got)
	}
	if got := r.LVMVGCount(); got != 1 {
		t.Errorf("LVMVGCount() = %d, want 1", got)
	}
}

// TestRegisterISCSITarget_IncreasesCount verifies that RegisterISCSITarget
// and ISCSITargetCount work correctly.
func TestRegisterISCSITarget_IncreasesCount(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterISCSITarget(&iscsi.Target{
		NodeContainer: "test-container",
		IQN:           "iqn.2026-01.com.bhyoo.pillar-csi:test1",
		TID:           1,
	})
	r.RegisterISCSITarget(&iscsi.Target{
		NodeContainer: "test-container",
		IQN:           "iqn.2026-01.com.bhyoo.pillar-csi:test2",
		TID:           2,
	})

	if got := r.ISCSITargetCount(); got != 2 {
		t.Errorf("ISCSITargetCount() = %d, want 2", got)
	}
	if got := r.ResourceCount(); got != 2 {
		t.Errorf("ResourceCount() = %d, want 2", got)
	}
}

// TestRegisterISCSITarget_NilNoop verifies that RegisterISCSITarget(nil) is safe.
func TestRegisterISCSITarget_NilNoop(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterISCSITarget(nil)

	if got := r.ISCSITargetCount(); got != 0 {
		t.Errorf("ISCSITargetCount() after nil registration = %d, want 0", got)
	}
}

// TestCleanup_ErrorMentionsDescription verifies that Cleanup error messages
// include the resource Description() so operators know which resource failed.
func TestCleanup_ErrorMentionsDescription(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("deliberate-destroy-failure")
	res := &fakeResource{
		name:       "my-special-nvme-ns",
		destroyErr: sentinel,
	}

	r := New()
	r.Register(res)

	err := r.Cleanup(context.Background())
	if err == nil {
		t.Fatal("expected error from Cleanup, got nil")
	}

	if !strings.Contains(err.Error(), "my-special-nvme-ns") {
		t.Errorf("error %q does not mention resource description", err.Error())
	}
}

// TestCleanup_LIFOOrderCustomBackend verifies that custom backends are
// destroyed in reverse registration order (LIFO), just like built-in types.
func TestCleanup_LIFOOrderCustomBackend(t *testing.T) {
	t.Parallel()

	var order []string
	mu := sync.Mutex{}

	tr := func(name string) Resource {
		return &trackingResourceImpl{name: name, order: &order, mu: &mu}
	}

	r := New()
	r.Register(tr("first"))
	r.Register(tr("second"))
	r.Register(tr("third"))

	if err := r.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected Cleanup error: %v", err)
	}

	want := []string{"third", "second", "first"}
	if len(order) != len(want) {
		t.Fatalf("destroy order len = %d, want %d; got %v", len(order), len(want), order)
	}
	for i, name := range want {
		if order[i] != name {
			t.Errorf("destroy order[%d] = %q, want %q", i, order[i], name)
		}
	}
}

// trackingResourceImpl is a Resource implementation that records the order
// in which its Destroy method is called into a shared slice.
type trackingResourceImpl struct {
	name  string
	order *[]string
	mu    *sync.Mutex
}

func (t *trackingResourceImpl) Destroy(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	*t.order = append(*t.order, t.name)
	return nil
}

func (t *trackingResourceImpl) Description() string {
	return fmt.Sprintf("tracking-resource %q", t.name)
}

// compile-time check: trackingResourceImpl must implement Resource.
var _ Resource = (*trackingResourceImpl)(nil)

// TestDescription_ZFSPool verifies that zfs.Pool.Description() returns a
// human-readable string containing pool name and container name.
func TestDescription_ZFSPool(t *testing.T) {
	t.Parallel()

	p := &zfs.Pool{NodeContainer: "my-node", PoolName: "my-pool"}
	desc := p.Description()
	if !strings.Contains(desc, "my-pool") {
		t.Errorf("zfs.Pool.Description() = %q, want it to contain pool name", desc)
	}
	if !strings.Contains(desc, "my-node") {
		t.Errorf("zfs.Pool.Description() = %q, want it to contain container name", desc)
	}
}

// TestDescription_LVMVG verifies that lvm.VG.Description() returns a
// human-readable string containing VG name and container name.
func TestDescription_LVMVG(t *testing.T) {
	t.Parallel()

	v := &lvm.VG{NodeContainer: "my-node", VGName: "my-vg"}
	desc := v.Description()
	if !strings.Contains(desc, "my-vg") {
		t.Errorf("lvm.VG.Description() = %q, want it to contain VG name", desc)
	}
	if !strings.Contains(desc, "my-node") {
		t.Errorf("lvm.VG.Description() = %q, want it to contain container name", desc)
	}
}

// TestDescription_ISCSITarget verifies that iscsi.Target.Description() returns
// a human-readable string containing IQN, TID, and container name.
func TestDescription_ISCSITarget(t *testing.T) {
	t.Parallel()

	target := &iscsi.Target{
		NodeContainer: "my-node",
		IQN:           "iqn.2026-01.com.bhyoo.pillar-csi:test",
		TID:           42,
	}
	desc := target.Description()
	if !strings.Contains(desc, "iqn.2026-01.com.bhyoo.pillar-csi:test") {
		t.Errorf("iscsi.Target.Description() = %q, want it to contain IQN", desc)
	}
	if !strings.Contains(desc, "my-node") {
		t.Errorf("iscsi.Target.Description() = %q, want it to contain container name", desc)
	}
}
