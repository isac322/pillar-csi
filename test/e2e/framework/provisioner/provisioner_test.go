// Package provisioner_test — AC 9: BackendProvisioner extensibility.
//
// These tests verify that:
//  1. A new backend type can implement BackendProvisioner and be registered with
//     a Pipeline without any framework code changes.
//  2. The built-in provisioners (ZFSProvisioner, LVMProvisioner) satisfy the
//     BackendProvisioner interface.
//  3. Pipeline.RunAll collects results correctly across success, skip, and error.
//  4. RegisterResources registers only non-nil, non-skipped, non-errored resources.
//  5. Soft-skip semantics (nil, nil from Provision) are correctly propagated.
//  6. Hard errors are collected without aborting remaining backends.
//  7. Pipeline ordering is preserved (FIFO registration order).
//  8. Nil provisioner registration is a safe no-op.
//  9. Nil Pipeline operations are safe no-ops.
//
// 10. Pipeline.AddBackend/BackendCount work correctly.
package provisioner_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/provisioner"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/registry"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// ─── AC 9: compile-time interface satisfaction ────────────────────────────────

// Verify that built-in provisioner types satisfy BackendProvisioner.
var _ provisioner.BackendProvisioner = (*provisioner.ZFSProvisioner)(nil)
var _ provisioner.BackendProvisioner = (*provisioner.LVMProvisioner)(nil)

// ─── fakeBackend — custom backend without framework changes ──────────────────

// fakeBackend demonstrates AC 9: a completely new backend type can implement
// BackendProvisioner and participate in the pipeline without any framework
// code changes. This type represents a hypothetical NVMe-oF namespace backend.
type fakeBackend struct {
	name        string
	provisionFn func(ctx context.Context) (registry.Resource, error)
}

func (f *fakeBackend) BackendType() string { return f.name }

func (f *fakeBackend) Provision(ctx context.Context) (registry.Resource, error) {
	if f.provisionFn != nil {
		return f.provisionFn(ctx)
	}
	// Default: provision succeeds and returns a fake resource.
	return &fakeResource{description: "fake-" + f.name}, nil
}

// compile-time check: fakeBackend must implement BackendProvisioner.
var _ provisioner.BackendProvisioner = (*fakeBackend)(nil)

// fakeResource is a registry.Resource implementation for testing.
type fakeResource struct {
	description  string
	destroyErr   error
	destroyCalls int
	mu           sync.Mutex
}

func (r *fakeResource) Destroy(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.destroyCalls++
	return r.destroyErr
}

func (r *fakeResource) Description() string {
	return r.description
}

// compile-time check: fakeResource must implement registry.Resource.
var _ registry.Resource = (*fakeResource)(nil)

// ─── 1. Custom backend registers and provisions without framework changes ─────

// TestNewBackendTypeRequiresNoFrameworkChanges verifies AC 9's core claim:
// a completely new backend type (fakeBackend, representing NVMe-oF or any
// other future backend) can be registered with a Pipeline and have its
// Provision method called without any changes to provisioner package code.
func TestNewBackendTypeRequiresNoFrameworkChanges(t *testing.T) {
	t.Parallel()

	p := provisioner.NewPipeline()
	p.AddBackend(&fakeBackend{name: "nvmeof"})
	p.AddBackend(&fakeBackend{name: "ceph-rbd"}) // second hypothetical backend

	if got := p.BackendCount(); got != 2 {
		t.Fatalf("BackendCount() = %d, want 2", got)
	}

	results, err := p.RunAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunAll returned unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("RunAll returned %d results, want 2", len(results))
	}

	for _, r := range results {
		if r.Err != nil {
			t.Errorf("backend %q: unexpected error: %v", r.BackendType, r.Err)
		}
		if r.Resource == nil {
			t.Errorf("backend %q: Resource is nil, want non-nil", r.BackendType)
		}
	}

	t.Logf("AC9: new backend types registered and provisioned without framework changes")
}

// ─── 2. Built-in types implement the interface ────────────────────────────────

// TestZFSProvisionerImplementsBackendProvisioner verifies that ZFSProvisioner
// satisfies BackendProvisioner at both compile-time and runtime.
//
// Soft-skip is DISABLED: ZFSProvisioner.Provision always hard-fails rather
// than returning (nil, nil). The compile-time check above already guarantees
// interface satisfaction; this test verifies BackendType() at runtime.
func TestZFSProvisionerImplementsBackendProvisioner(t *testing.T) {
	t.Parallel()

	var prov provisioner.BackendProvisioner = &provisioner.ZFSProvisioner{
		NodeContainer: "test-container",
		PoolName:      "test-pool",
	}

	if got := prov.BackendType(); got != "zfs" {
		t.Errorf("ZFSProvisioner.BackendType() = %q, want %q", got, "zfs")
	}
}

// TestLVMProvisionerImplementsBackendProvisioner verifies that LVMProvisioner
// satisfies BackendProvisioner at both compile-time and runtime.
//
// Soft-skip is DISABLED: LVMProvisioner.Provision always hard-fails rather
// than returning (nil, nil). The compile-time check above already guarantees
// interface satisfaction; this test verifies BackendType() at runtime.
func TestLVMProvisionerImplementsBackendProvisioner(t *testing.T) {
	t.Parallel()

	var prov provisioner.BackendProvisioner = &provisioner.LVMProvisioner{
		NodeContainer: "test-container",
		VGName:        "test-vg",
	}

	if got := prov.BackendType(); got != "lvm" {
		t.Errorf("LVMProvisioner.BackendType() = %q, want %q", got, "lvm")
	}
}

// ─── 3. Pipeline.RunAll result collection ─────────────────────────────────────

// TestPipelineRunAllCollectsAllResults verifies that RunAll returns one
// ProvisionResult per registered backend regardless of the outcome (success,
// skip, or error).
func TestPipelineRunAllCollectsAllResults(t *testing.T) {
	t.Parallel()

	sentinelErr := errors.New("deliberate-provision-error")

	p := provisioner.NewPipeline()
	// Backend 1: success
	p.AddBackend(&fakeBackend{name: "success-backend"})
	// Backend 2: protocol violation — returns (nil, nil) which is now a hard error
	p.AddBackend(&fakeBackend{
		name: "violation-backend",
		provisionFn: func(_ context.Context) (registry.Resource, error) {
			return nil, nil //nolint:nilnil // intentional protocol violation for test
		},
	})
	// Backend 3: hard error
	p.AddBackend(&fakeBackend{
		name: "error-backend",
		provisionFn: func(_ context.Context) (registry.Resource, error) {
			return nil, sentinelErr
		},
	})

	results, err := p.RunAll(context.Background(), nil)

	// Exactly 3 results.
	if len(results) != 3 {
		t.Fatalf("RunAll: got %d results, want 3", len(results))
	}

	// Backend 1: success.
	if results[0].Err != nil || results[0].Resource == nil {
		t.Errorf("backend 1 (success): got err=%v resource=%v",
			results[0].Err, results[0].Resource)
	}

	// Backend 2: protocol violation — (nil, nil) must produce a hard error.
	if results[1].Err == nil {
		t.Errorf("backend 2 (violation): expected hard error for (nil,nil) return, got nil error")
	}
	if results[1].Resource != nil {
		t.Errorf("backend 2 (violation): Resource must be nil, got %v", results[1].Resource)
	}
	if !strings.Contains(results[1].Err.Error(), "protocol violation") {
		t.Errorf("backend 2 (violation): error %q must mention 'protocol violation'", results[1].Err.Error())
	}

	// Backend 3: error.
	if results[2].Err == nil || results[2].Resource != nil {
		t.Errorf("backend 3 (error): got err=%v resource=%v",
			results[2].Err, results[2].Resource)
	}

	// RunAll must return a non-nil error (from backends 2 and 3).
	if err == nil {
		t.Error("RunAll: expected non-nil error for violation/error backends, got nil")
	}
	if !strings.Contains(err.Error(), "error-backend") {
		t.Errorf("RunAll: error %q does not mention failed backend name", err.Error())
	}
}

// ─── 4. RegisterResources ─────────────────────────────────────────────────────

// TestRegisterResourcesOnlyRegistersSuccessful verifies that RegisterResources
// registers only resources from successful (non-nil Resource, nil Err)
// ProvisionResults.
func TestRegisterResourcesOnlyRegistersSuccessful(t *testing.T) {
	t.Parallel()

	res1 := &fakeResource{description: "resource-1"}
	res2 := &fakeResource{description: "resource-2"}

	results := []provisioner.ProvisionResult{
		{BackendType: "success-1", Resource: res1},           // should be registered
		{BackendType: "nil-resource", Resource: nil},         // should NOT be registered (nil Resource)
		{BackendType: "error-backend", Err: errors.New("x")}, // should NOT be registered
		{BackendType: "success-2", Resource: res2},           // should be registered
	}

	reg := registry.New()
	provisioner.RegisterResources(reg, results)

	if got := reg.ResourceCount(); got != 2 {
		t.Errorf("ResourceCount after RegisterResources = %d, want 2", got)
	}
}

// TestRegisterResourcesNilRegistryNoop verifies that passing a nil registry is
// a safe no-op.
func TestRegisterResourcesNilRegistryNoop(t *testing.T) {
	t.Parallel()

	results := []provisioner.ProvisionResult{
		{BackendType: "backend", Resource: &fakeResource{description: "r"}},
	}

	// Must not panic.
	provisioner.RegisterResources(nil, results)
}

// ─── 5. Protocol violation: (nil, nil) is a hard error ───────────────────────

// TestProtocolViolationNilNilResultsInError verifies that a backend returning
// (nil, nil) from Provision produces a ProvisionResult with Resource==nil and
// a non-nil Err mentioning "protocol violation".
//
// Production provisioners must return (resource, nil) or (nil, err); returning
// (nil, nil) is a hard protocol-violation error.
func TestProtocolViolationNilNilResultsInError(t *testing.T) {
	t.Parallel()

	p := provisioner.NewPipeline()
	p.AddBackend(&fakeBackend{
		name: "violation-backend",
		provisionFn: func(_ context.Context) (registry.Resource, error) {
			return nil, nil //nolint:nilnil // intentional protocol violation for test
		},
	})

	results, err := p.RunAll(context.Background(), nil)
	// RunAll must return a non-nil error for the protocol violation.
	if err == nil {
		t.Fatal("RunAll: expected non-nil error for protocol violation, got nil")
	}
	if len(results) != 1 {
		t.Fatalf("RunAll: got %d results, want 1", len(results))
	}

	r := results[0]
	if r.Resource != nil {
		t.Errorf("violation: Resource = %v, want nil", r.Resource)
	}
	if r.Err == nil {
		t.Errorf("violation: Err is nil, want non-nil error")
	}
	if r.Err != nil && !strings.Contains(r.Err.Error(), "protocol violation") {
		t.Errorf("violation: Err %q must contain 'protocol violation'", r.Err.Error())
	}
}

// ─── 6. Hard errors collected without aborting ────────────────────────────────

// TestHardErrorDoesNotAbortRemainingBackends verifies that when a backend
// returns a hard error, RunAll continues provisioning the remaining backends
// and collects all errors.
func TestHardErrorDoesNotAbortRemainingBackends(t *testing.T) {
	t.Parallel()

	var provisionedNames []string
	var mu sync.Mutex

	trackProvision := func(name string) provisioner.BackendProvisioner {
		return &fakeBackend{
			name: name,
			provisionFn: func(_ context.Context) (registry.Resource, error) {
				mu.Lock()
				provisionedNames = append(provisionedNames, name)
				mu.Unlock()
				return &fakeResource{description: name}, nil
			},
		}
	}

	errBackend := &fakeBackend{
		name: "middle-error",
		provisionFn: func(_ context.Context) (registry.Resource, error) {
			mu.Lock()
			provisionedNames = append(provisionedNames, "middle-error")
			mu.Unlock()
			return nil, errors.New("middle-backend-failed")
		},
	}

	p := provisioner.NewPipeline()
	p.AddBackend(trackProvision("first"))
	p.AddBackend(errBackend)
	p.AddBackend(trackProvision("last"))

	results, err := p.RunAll(context.Background(), nil)
	if err == nil {
		t.Fatal("RunAll: expected non-nil error for middle backend, got nil")
	}

	// All 3 backends must have been attempted.
	if len(results) != 3 {
		t.Fatalf("RunAll: got %d results, want 3", len(results))
	}

	// Verify all 3 were provisioned (in order).
	mu.Lock()
	defer mu.Unlock()

	if len(provisionedNames) != 3 {
		t.Errorf("only %d backends were attempted, want 3: %v", len(provisionedNames), provisionedNames)
	}
}

// ─── 7. Ordering preserved ────────────────────────────────────────────────────

// TestPipelinePreservesRegistrationOrder verifies that RunAll provisions
// backends in the order they were registered (FIFO).
func TestPipelinePreservesRegistrationOrder(t *testing.T) {
	t.Parallel()

	var order []string
	var mu sync.Mutex

	makeOrdered := func(name string) provisioner.BackendProvisioner {
		return &fakeBackend{
			name: name,
			provisionFn: func(_ context.Context) (registry.Resource, error) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return &fakeResource{description: name}, nil
			},
		}
	}

	p := provisioner.NewPipeline()
	p.AddBackend(makeOrdered("alpha"))
	p.AddBackend(makeOrdered("beta"))
	p.AddBackend(makeOrdered("gamma"))

	if _, err := p.RunAll(context.Background(), nil); err != nil {
		t.Fatalf("RunAll: unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	want := []string{"alpha", "beta", "gamma"}
	if len(order) != len(want) {
		t.Fatalf("provisioning order len = %d, want %d; got %v", len(order), len(want), order)
	}
	for i, name := range want {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q", i, order[i], name)
		}
	}
}

// ─── 8. Nil provisioner is safe ───────────────────────────────────────────────

// TestAddBackendNilIsNoop verifies that passing nil to AddBackend does not
// increment the backend count and does not cause RunAll to panic.
func TestAddBackendNilIsNoop(t *testing.T) {
	t.Parallel()

	p := provisioner.NewPipeline()
	p.AddBackend(nil)
	p.AddBackend(nil)

	if got := p.BackendCount(); got != 0 {
		t.Errorf("BackendCount after nil adds = %d, want 0", got)
	}

	results, err := p.RunAll(context.Background(), nil)
	if err != nil {
		t.Errorf("RunAll with no backends: unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("RunAll with no backends: got %d results, want 0", len(results))
	}
}

// ─── 9. Nil Pipeline ─────────────────────────────────────────────────────────

// TestNilPipelineRunAllIsNoop verifies that calling RunAll on a nil Pipeline
// is a safe no-op returning (nil, nil).
func TestNilPipelineRunAllIsNoop(t *testing.T) {
	t.Parallel()

	var p *provisioner.Pipeline
	results, err := p.RunAll(context.Background(), nil)
	if err != nil {
		t.Errorf("nil Pipeline.RunAll: unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("nil Pipeline.RunAll: results = %v, want nil", results)
	}
}

// TestNilPipelineBackendCountIsZero verifies that BackendCount on a nil
// Pipeline returns 0 without panicking.
func TestNilPipelineBackendCountIsZero(t *testing.T) {
	t.Parallel()

	var p *provisioner.Pipeline
	if got := p.BackendCount(); got != 0 {
		t.Errorf("nil Pipeline.BackendCount() = %d, want 0", got)
	}
}

// ─── 10. BackendCount ────────────────────────────────────────────────────────

// TestBackendCountReflectsRegistrations verifies that BackendCount increases
// with each non-nil AddBackend call.
func TestBackendCountReflectsRegistrations(t *testing.T) {
	t.Parallel()

	p := provisioner.NewPipeline()
	if got := p.BackendCount(); got != 0 {
		t.Errorf("BackendCount() initial = %d, want 0", got)
	}

	p.AddBackend(&fakeBackend{name: "b1"})
	if got := p.BackendCount(); got != 1 {
		t.Errorf("BackendCount() after 1 add = %d, want 1", got)
	}

	p.AddBackend(&fakeBackend{name: "b2"})
	p.AddBackend(&fakeBackend{name: "b3"})
	if got := p.BackendCount(); got != 3 {
		t.Errorf("BackendCount() after 3 adds = %d, want 3", got)
	}
}

// ─── 11. ProvisionResult.Duration is recorded ─────────────────────────────────

// TestProvisionResultDurationIsRecorded verifies that RunAll records a
// non-negative Duration for each ProvisionResult.
func TestProvisionResultDurationIsRecorded(t *testing.T) {
	t.Parallel()

	p := provisioner.NewPipeline()
	p.AddBackend(&fakeBackend{name: "timed-backend"})

	results, err := p.RunAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunAll: unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("RunAll: got %d results, want 1", len(results))
	}

	if results[0].Duration < 0 {
		t.Errorf("Duration = %v, want ≥ 0", results[0].Duration)
	}
}

// ─── 12. BackendType in results matches provisioner ──────────────────────────

// TestProvisionResultBackendTypeMatchesProvisioner verifies that
// ProvisionResult.BackendType equals what BackendType() returns for each
// registered provisioner.
func TestProvisionResultBackendTypeMatchesProvisioner(t *testing.T) {
	t.Parallel()

	p := provisioner.NewPipeline()
	p.AddBackend(&fakeBackend{name: "custom-type-1"})
	p.AddBackend(&fakeBackend{name: "custom-type-2"})

	results, _ := p.RunAll(context.Background(), nil)
	if len(results) != 2 {
		t.Fatalf("RunAll: got %d results, want 2", len(results))
	}

	if results[0].BackendType != "custom-type-1" {
		t.Errorf("results[0].BackendType = %q, want %q", results[0].BackendType, "custom-type-1")
	}
	if results[1].BackendType != "custom-type-2" {
		t.Errorf("results[1].BackendType = %q, want %q", results[1].BackendType, "custom-type-2")
	}
}

// ─── 13. ZFSProvisioner/LVMProvisioner validation errors ─────────────────────
//
// NOTE: Tests 13 and 14 ("soft-skip when module absent") have been removed
// because soft-skip is DISABLED. ZFSProvisioner and LVMProvisioner no longer
// return (nil, nil) — all failures are hard errors. kernel-module checks are
// enforced upfront by [kind.CheckBackendKernelModules] before provisioning runs.

// TestZFSProvisionerRejectsEmptyNodeContainer verifies that ZFSProvisioner
// returns a hard error when NodeContainer is empty. The validation check
// runs before any docker exec, so no real cluster is required.
func TestZFSProvisionerRejectsEmptyNodeContainer(t *testing.T) {
	t.Parallel()

	prov := &provisioner.ZFSProvisioner{
		NodeContainer: "", // invalid
		PoolName:      "pool",
	}

	_, err := prov.Provision(context.Background())
	if err == nil {
		t.Error("ZFSProvisioner.Provision with empty NodeContainer: expected error, got nil")
	}
}

// TestLVMProvisionerRejectsEmptyVGName verifies that LVMProvisioner returns a
// hard error when VGName is empty. The validation check runs before any docker
// exec, so no real cluster is required.
func TestLVMProvisionerRejectsEmptyVGName(t *testing.T) {
	t.Parallel()

	prov := &provisioner.LVMProvisioner{
		NodeContainer: "container",
		VGName:        "", // invalid
	}

	_, err := prov.Provision(context.Background())
	if err == nil {
		t.Error("LVMProvisioner.Provision with empty VGName: expected error, got nil")
	}
}

// ─── 16. BackendType strings ────────────────────────────────────────────────

// TestBuiltInProvisionerBackendTypeStrings verifies the canonical BackendType
// strings for the built-in provisioners.
func TestBuiltInProvisionerBackendTypeStrings(t *testing.T) {
	t.Parallel()

	zfsProv := &provisioner.ZFSProvisioner{}
	if got := zfsProv.BackendType(); got != "zfs" {
		t.Errorf("ZFSProvisioner.BackendType() = %q, want %q", got, "zfs")
	}

	lvmProv := &provisioner.LVMProvisioner{}
	if got := lvmProv.BackendType(); got != "lvm" {
		t.Errorf("LVMProvisioner.BackendType() = %q, want %q", got, "lvm")
	}
}

// ─── 17. Multiple errors in RunAll are joined ────────────────────────────────

// TestRunAllJoinsMultipleErrors verifies that when multiple backends fail,
// RunAll returns an error that mentions all failing backend names.
func TestRunAllJoinsMultipleErrors(t *testing.T) {
	t.Parallel()

	makeErrBackend := func(name string) provisioner.BackendProvisioner {
		return &fakeBackend{
			name: name,
			provisionFn: func(_ context.Context) (registry.Resource, error) {
				return nil, fmt.Errorf("deliberate failure from %s", name)
			},
		}
	}

	p := provisioner.NewPipeline()
	p.AddBackend(makeErrBackend("backend-A"))
	p.AddBackend(makeErrBackend("backend-B"))

	_, err := p.RunAll(context.Background(), nil)
	if err == nil {
		t.Fatal("RunAll: expected non-nil error, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "backend-A") {
		t.Errorf("error %q does not mention backend-A", msg)
	}
	if !strings.Contains(msg, "backend-B") {
		t.Errorf("error %q does not mention backend-B", msg)
	}
}

// ─── 18. registry.Resource compile-time checks ───────────────────────────────

// Verify that built-in backend resource types implement registry.Resource.
// These compile-time checks guarantee that the existing zfs.Pool and lvm.VG
// types (which BackendProvisioner.Provision returns) satisfy registry.Resource
// and can thus be registered for cleanup.
var _ registry.Resource = (*zfs.Pool)(nil)
var _ registry.Resource = (*lvm.VG)(nil)

// ─── 19–22. RunAllConcurrent (Sub-AC 2.1 parallel provisioning) ──────────────

// TestRunAllConcurrentReturnsResultsInRegistrationOrder verifies that
// RunAllConcurrent preserves registration order in the returned slice even
// though provisioners execute concurrently.
//
// Sub-AC 2.1: results[i] must correspond to the i-th registered provisioner
// regardless of which goroutine finishes first.
func TestRunAllConcurrentReturnsResultsInRegistrationOrder(t *testing.T) {
	t.Parallel()

	// Use sleep to ensure goroutines finish in reverse registration order,
	// proving that the results slice is ordered by registration, not completion.
	p := provisioner.NewPipeline()
	p.AddBackend(&fakeBackend{
		name: "slow",
		provisionFn: func(ctx context.Context) (registry.Resource, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-func() chan struct{} {
				ch := make(chan struct{})
				go func() {
					// Briefly yield to allow "fast" to complete first.
					for i := 0; i < 1000; i++ {
						// busy spin — no time.Sleep to avoid external dependencies
					}
					close(ch)
				}()
				return ch
			}():
			}
			return &fakeResource{description: "slow-resource"}, nil
		},
	})
	p.AddBackend(&fakeBackend{
		name: "fast",
		provisionFn: func(_ context.Context) (registry.Resource, error) {
			return &fakeResource{description: "fast-resource"}, nil
		},
	})

	results, err := p.RunAllConcurrent(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunAllConcurrent: unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("RunAllConcurrent: got %d results, want 2", len(results))
	}

	// Index 0 must be "slow" (first registered), index 1 must be "fast".
	if results[0].BackendType != "slow" {
		t.Errorf("results[0].BackendType = %q, want %q", results[0].BackendType, "slow")
	}
	if results[1].BackendType != "fast" {
		t.Errorf("results[1].BackendType = %q, want %q", results[1].BackendType, "fast")
	}
	if results[0].Resource == nil {
		t.Error("results[0].Resource is nil, want non-nil (slow backend)")
	}
	if results[1].Resource == nil {
		t.Error("results[1].Resource is nil, want non-nil (fast backend)")
	}
}

// TestRunAllConcurrentCollectsAllResults verifies that RunAllConcurrent handles
// success, protocol-violation, and hard-error outcomes correctly — same
// contract as RunAll. (nil, nil) is a protocol violation, not a soft-skip.
func TestRunAllConcurrentCollectsAllResults(t *testing.T) {
	t.Parallel()

	sentinelErr := errors.New("concurrent-deliberate-error")

	p := provisioner.NewPipeline()
	p.AddBackend(&fakeBackend{name: "ok"})
	p.AddBackend(&fakeBackend{
		name: "violation",
		provisionFn: func(_ context.Context) (registry.Resource, error) {
			return nil, nil //nolint:nilnil // intentional protocol violation for test
		},
	})
	p.AddBackend(&fakeBackend{
		name: "fail",
		provisionFn: func(_ context.Context) (registry.Resource, error) {
			return nil, sentinelErr
		},
	})

	results, err := p.RunAllConcurrent(context.Background(), nil)

	if len(results) != 3 {
		t.Fatalf("RunAllConcurrent: got %d results, want 3", len(results))
	}
	if results[0].Err != nil || results[0].Resource == nil {
		t.Errorf("results[0] (ok): err=%v resource=%v",
			results[0].Err, results[0].Resource)
	}
	// results[1] (violation): (nil, nil) must produce a hard error.
	if results[1].Err == nil {
		t.Errorf("results[1] (violation): expected hard error for (nil,nil) return, got nil")
	}
	if results[1].Resource != nil {
		t.Errorf("results[1] (violation): Resource must be nil, got %v", results[1].Resource)
	}
	if results[2].Err == nil || results[2].Resource != nil {
		t.Errorf("results[2] (fail): err=%v resource=%v", results[2].Err, results[2].Resource)
	}
	if err == nil {
		t.Error("RunAllConcurrent: expected non-nil error for violation/failing backends")
	}
}

// TestRunAllConcurrentNilPipelineIsNoop verifies that calling RunAllConcurrent
// on a nil Pipeline is a safe no-op (returns nil, nil).
func TestRunAllConcurrentNilPipelineIsNoop(t *testing.T) {
	t.Parallel()

	var p *provisioner.Pipeline
	results, err := p.RunAllConcurrent(context.Background(), nil)
	if err != nil {
		t.Errorf("nil Pipeline.RunAllConcurrent: unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("nil Pipeline.RunAllConcurrent: results = %v, want nil", results)
	}
}

// TestRunAllConcurrentIsFasterThanSerial verifies that RunAllConcurrent
// completes N independent backends in roughly 1/N the time of RunAll.
// This tests the Sub-AC 2.1 performance guarantee.
func TestRunAllConcurrentIsFasterThanSerial(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping performance test in short mode")
	}

	const holdTime = 10 * time.Millisecond // simulates provisioning work
	const numBackends = 4

	slowBackend := func(name string) provisioner.BackendProvisioner {
		return &fakeBackend{
			name: name,
			provisionFn: func(ctx context.Context) (registry.Resource, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(holdTime):
				}
				return &fakeResource{description: name}, nil
			},
		}
	}

	buildPipeline := func() *provisioner.Pipeline {
		p := provisioner.NewPipeline()
		for i := range numBackends {
			p.AddBackend(slowBackend(fmt.Sprintf("backend-%d", i)))
		}
		return p
	}

	// Sequential: sum of all durations.
	start := time.Now()
	if _, err := buildPipeline().RunAll(context.Background(), nil); err != nil {
		t.Fatalf("RunAll: unexpected error: %v", err)
	}
	seqDur := time.Since(start)

	// Concurrent: max of all durations.
	start = time.Now()
	if _, err := buildPipeline().RunAllConcurrent(context.Background(), nil); err != nil {
		t.Fatalf("RunAllConcurrent: unexpected error: %v", err)
	}
	concDur := time.Since(start)

	// Concurrent must be significantly faster: at least 2× speedup.
	speedup := float64(seqDur) / float64(concDur)
	if speedup < 2.0 {
		t.Errorf("Sub-AC 2.1: RunAllConcurrent speedup = %.2fx (seq=%v conc=%v), want ≥ 2×",
			speedup, seqDur, concDur)
	} else {
		t.Logf("Sub-AC 2.1: RunAllConcurrent speedup = %.2fx (seq=%v conc=%v)",
			speedup, seqDur, concDur)
	}
}
