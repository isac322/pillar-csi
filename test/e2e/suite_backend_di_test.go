package e2e

// suite_backend_di_test.go — Sub-AC 3 (of AC 4): BackendProvisioner dependency
// injection into bootstrapSuiteBackends.
//
// Acceptance criteria verified here:
//
//  1. bootstrapSuiteBackends accepts custom BackendProvisioner implementations
//     via its variadic provisioners parameter without any framework code changes.
//  2. When no provisioners are passed, the function falls back to the default
//     ZFS + LVM pipeline (backward-compatible zero-argument call).
//  3. Custom provisioners are invoked by the pipeline; their resources are
//     mapped to suiteBackendState for typed ZFS/LVM access.
//  4. A provisioner that returns (nil, nil) (soft skip) does not produce an
//     error and leaves the corresponding suiteBackendState field nil.
//  5. A provisioner that returns a hard error causes bootstrapSuiteBackends to
//     return a wrapped error with the [AC5] tag and all previously provisioned
//     resources are cleaned up.
//  6. Multiple custom provisioners can be injected simultaneously; results are
//     mapped to the correct suiteBackendState fields by BackendType.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/provisioner"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/registry"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// stubKindBootstrapState returns a minimal *kindBootstrapState with the given
// cluster name, suitable for passing to bootstrapSuiteBackends in unit tests.
// It uses a tempdir for SuiteRootDir to satisfy validate().
func stubKindBootstrapState(t *testing.T, clusterName string) *kindBootstrapState {
	t.Helper()
	root := t.TempDir()
	return &kindBootstrapState{
		ClusterName:    clusterName,
		SuiteRootDir:   root,
		WorkspaceDir:   root + "/workspace",
		LogsDir:        root + "/logs",
		GeneratedDir:   root + "/generated",
		KindBinary:     "kind",
		KubectlBinary:  "kubectl",
		CreateTimeout:  2 * time.Minute,
		DeleteTimeout:  2 * time.Minute,
		KubeconfigPath: root + "/generated/kubeconfig",
	}
}

// diZFSProvisioner is a test-double ZFSProvisioner that avoids real docker exec.
// It returns a pre-created *zfs.Pool, enabling tests to verify that the
// returned resource is correctly wired into suiteBackendState.ZFSPool.
type diZFSProvisioner struct {
	pool       *zfs.Pool
	provideErr error
}

func (d *diZFSProvisioner) BackendType() string { return "zfs" }

func (d *diZFSProvisioner) Provision(_ context.Context) (registry.Resource, error) {
	if d.provideErr != nil {
		return nil, d.provideErr
	}
	if d.pool == nil {
		return nil, nil //nolint:nilnil // soft skip: BackendProvisioner contract (nil,nil) = absent resource
	}
	return d.pool, nil
}

// compile-time interface check.
var _ provisioner.BackendProvisioner = (*diZFSProvisioner)(nil)

// diLVMProvisioner is a test-double LVMProvisioner analogous to diZFSProvisioner.
type diLVMProvisioner struct {
	vg         *lvm.VG
	provideErr error
}

func (d *diLVMProvisioner) BackendType() string { return "lvm" }

func (d *diLVMProvisioner) Provision(_ context.Context) (registry.Resource, error) {
	if d.provideErr != nil {
		return nil, d.provideErr
	}
	if d.vg == nil {
		return nil, nil //nolint:nilnil // soft skip: BackendProvisioner contract (nil,nil) = absent resource
	}
	return d.vg, nil
}

// compile-time interface check.
var _ provisioner.BackendProvisioner = (*diLVMProvisioner)(nil)

// diCustomProvisioner simulates a hypothetical new backend type (e.g. iSCSI,
// NVMe-oF) that can be injected without any changes to the framework.
type diCustomProvisioner struct {
	backendType string
	resource    registry.Resource
	provideErr  error
}

func (d *diCustomProvisioner) BackendType() string { return d.backendType }

func (d *diCustomProvisioner) Provision(_ context.Context) (registry.Resource, error) {
	return d.resource, d.provideErr
}

var _ provisioner.BackendProvisioner = (*diCustomProvisioner)(nil)

// destroyCountResource is a registry.Resource that records how many times
// Destroy was called, used to verify resource cleanup on hard errors.
type destroyCountResource struct {
	description  string
	destroyCalls int
}

func (r *destroyCountResource) Destroy(_ context.Context) error {
	r.destroyCalls++
	return nil
}

func (r *destroyCountResource) Description() string { return r.description }

var _ registry.Resource = (*destroyCountResource)(nil)

// ── 1. Custom provisioner DI: no framework changes ────────────────────────────

// TestDICustomProvisionerRequiresNoFrameworkChanges verifies AC 9's core claim
// applied to bootstrapSuiteBackends: a new backend type can be injected via the
// provisioners variadic parameter without modifying any framework code.
func TestDICustomProvisionerRequiresNoFrameworkChanges(t *testing.T) {
	t.Parallel()

	const clusterName = "pillar-csi-e2e-p00001-abcd1234"
	clusterState := stubKindBootstrapState(t, clusterName)

	customRes := &destroyCountResource{description: "custom-iscsi"}
	custom := &diCustomProvisioner{
		backendType: "iscsi",
		resource:    customRes,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Inject a custom provisioner — no changes to framework code required.
	state, err := bootstrapSuiteBackends(ctx, clusterState, nil, custom)
	if err != nil {
		t.Fatalf("DI: bootstrapSuiteBackends with custom provisioner: %v", err)
	}
	if state == nil {
		t.Fatal("DI: state is nil, want non-nil")
	}
	// NodeContainer must still be set from the cluster name.
	if state.NodeContainer == "" {
		t.Error("DI: NodeContainer is empty — must be derived from cluster name")
	}
	// Standard ZFS/LVM fields are nil since we injected a non-zfs/lvm backend.
	if state.ZFSPool != nil {
		t.Error("DI: ZFSPool should be nil when no zfs provisioner was injected")
	}
	if state.LVMVG != nil {
		t.Error("DI: LVMVG should be nil when no lvm provisioner was injected")
	}

	t.Logf("DI: custom backend provisioner injected; state.NodeContainer=%q", state.NodeContainer)
}

// ── 2. Backward-compatible zero-arg call (compile-time) ──────────────────────

// TestDIZeroArgCallCompilesWithNewSignature is a compile-time verification that
// the 3-argument call pattern (ctx, clusterState, output) still compiles after
// the signature was extended with variadic provisioners. This preserves full
// backward compatibility with all existing callers such as runPrimary in
// main_test.go.
//
// The test does not execute bootstrapSuiteBackends at runtime because the
// default provisioners attempt docker exec against the Kind control-plane
// container, which requires a live Kind cluster. Runtime execution of the
// default provisioners is tested in the e2e suite (build tag: e2e).
func TestDIZeroArgCallCompilesWithNewSignature(t *testing.T) {
	t.Parallel()

	// Compile-time check: the assignment below must compile with the exact
	// 3-argument signature.  It proves the variadic extension did not break
	// the 3-argument call site used by runPrimary in main_test.go.
	callWithZeroProviders := func(
		ctx context.Context,
		clusterState *kindBootstrapState,
		output io.Writer,
	) (*suiteBackendState, error) {
		// This is the exact call pattern used by runPrimary in main_test.go.
		// No provisioners passed → default ZFS + LVM pipeline is used.
		return bootstrapSuiteBackends(ctx, clusterState, output)
	}

	// Ensure the variable is used to suppress any unused-variable error.
	_ = callWithZeroProviders

	t.Logf("DI: 3-arg call signature verified at compile time — backward compatibility preserved")
}

// ── 3. Injected ZFS provisioner populates ZFSPool field ──────────────────────

// TestDIInjectedZFSProvisionerPopulatesZFSPoolField verifies that when a
// BackendProvisioner with BackendType()=="zfs" returns a *zfs.Pool,
// bootstrapSuiteBackends correctly assigns it to suiteBackendState.ZFSPool.
func TestDIInjectedZFSProvisionerPopulatesZFSPoolField(t *testing.T) {
	t.Parallel()

	const (
		clusterName = "pillar-csi-e2e-p00002-abcd1234"
		container   = "pillar-csi-e2e-p00002-abcd1234-control-plane"
		poolName    = "e2ep-abcd1234"
	)

	pool := &zfs.Pool{
		NodeContainer: container,
		PoolName:      poolName,
	}

	clusterState := stubKindBootstrapState(t, clusterName)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state, err := bootstrapSuiteBackends(ctx, clusterState, nil,
		&diZFSProvisioner{pool: pool},
	)
	if err != nil {
		t.Fatalf("DI: bootstrapSuiteBackends with ZFS DI provisioner: %v", err)
	}
	if state == nil {
		t.Fatal("DI: state is nil")
	}
	if state.ZFSPool == nil {
		t.Fatal("DI: ZFSPool is nil — injected provisioner result not mapped")
	}
	if state.ZFSPool.PoolName != poolName {
		t.Errorf("DI: ZFSPool.PoolName = %q, want %q", state.ZFSPool.PoolName, poolName)
	}
	if state.LVMVG != nil {
		t.Error("DI: LVMVG should be nil — no LVM provisioner was injected")
	}

	t.Logf("DI: ZFS DI provisioner correctly mapped to ZFSPool field: %q", state.ZFSPool.PoolName)
}

// ── 4. Injected LVM provisioner populates LVMVG field ────────────────────────

// TestDIInjectedLVMProvisionerPopulatesLVMVGField verifies that when a
// BackendProvisioner with BackendType()=="lvm" returns a *lvm.VG,
// bootstrapSuiteBackends correctly assigns it to suiteBackendState.LVMVG.
func TestDIInjectedLVMProvisionerPopulatesLVMVGField(t *testing.T) {
	t.Parallel()

	const (
		clusterName = "pillar-csi-e2e-p00003-abcd1234"
		container   = "pillar-csi-e2e-p00003-abcd1234-control-plane"
		vgName      = "e2evg-abcd1234"
	)

	vg := &lvm.VG{
		NodeContainer: container,
		VGName:        vgName,
	}

	clusterState := stubKindBootstrapState(t, clusterName)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state, err := bootstrapSuiteBackends(ctx, clusterState, nil,
		&diLVMProvisioner{vg: vg},
	)
	if err != nil {
		t.Fatalf("DI: bootstrapSuiteBackends with LVM DI provisioner: %v", err)
	}
	if state == nil {
		t.Fatal("DI: state is nil")
	}
	if state.LVMVG == nil {
		t.Fatal("DI: LVMVG is nil — injected provisioner result not mapped")
	}
	if state.LVMVG.VGName != vgName {
		t.Errorf("DI: LVMVG.VGName = %q, want %q", state.LVMVG.VGName, vgName)
	}
	if state.ZFSPool != nil {
		t.Error("DI: ZFSPool should be nil — no ZFS provisioner was injected")
	}

	t.Logf("DI: LVM DI provisioner correctly mapped to LVMVG field: %q", state.LVMVG.VGName)
}

// ── 5. Soft-skip from injected provisioner leaves field nil ──────────────────

// TestDISoftSkipFromInjectedProvisionerLeavesFieldNil verifies that when an
// injected provisioner returns (nil, nil) (soft skip), bootstrapSuiteBackends
// returns no error and the corresponding state field remains nil.
func TestDISoftSkipFromInjectedProvisionerLeavesFieldNil(t *testing.T) {
	t.Parallel()

	clusterState := stubKindBootstrapState(t, "pillar-csi-e2e-p00004-abcd1234")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Both provisioners return soft skip.
	state, err := bootstrapSuiteBackends(ctx, clusterState, nil,
		&diZFSProvisioner{pool: nil}, // soft skip
		&diLVMProvisioner{vg: nil},   // soft skip
	)
	if err != nil {
		t.Fatalf("DI: soft skip provisioners returned unexpected error: %v", err)
	}
	if state == nil {
		t.Fatal("DI: state is nil — must be non-nil even when all backends skip")
	}
	if state.ZFSPool != nil {
		t.Errorf("DI: ZFSPool = %v, want nil (soft skip)", state.ZFSPool)
	}
	if state.LVMVG != nil {
		t.Errorf("DI: LVMVG = %v, want nil (soft skip)", state.LVMVG)
	}

	t.Logf("DI: soft-skip from injected provisioners leaves state fields nil — correct")
}

// ── 6. Hard error from provisioner returns AC5-tagged error and cleans up ──

// TestDIHardErrorFromProvisionerReturnsAC52TaggedError verifies that when an
// injected provisioner returns a hard error, bootstrapSuiteBackends:
//
//	(a) returns a non-nil error containing the "[AC5]" tag,
//	(b) cleans up (calls Destroy on) any resources provisioned before the failure.
func TestDIHardErrorFromProvisionerReturnsAC52TaggedError(t *testing.T) {
	t.Parallel()

	clusterState := stubKindBootstrapState(t, "pillar-csi-e2e-p00005-abcd1234")

	// First provisioner: succeeds; we track its Destroy calls.
	firstRes := &destroyCountResource{description: "first-backend"}
	first := &diCustomProvisioner{
		backendType: "custom-first",
		resource:    firstRes,
	}

	// Second provisioner: returns a hard error.
	secondErr := errors.New("deliberate-hard-error")
	second := &diCustomProvisioner{
		backendType: "custom-second",
		provideErr:  secondErr,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := bootstrapSuiteBackends(ctx, clusterState, nil, first, second)
	if err == nil {
		t.Fatal("DI: expected non-nil error from hard-error provisioner, got nil")
	}
	if !strings.Contains(err.Error(), "AC5") {
		t.Errorf("DI: error %q does not contain [AC5] tag", err.Error())
	}
	if !strings.Contains(err.Error(), "deliberate-hard-error") {
		t.Errorf("DI: error %q does not mention the original error", err.Error())
	}

	// The first provisioner's resource must have been destroyed (cleanup on error).
	if firstRes.destroyCalls == 0 {
		t.Error("DI: firstRes.Destroy was not called — resource leaked after hard error")
	}

	t.Logf("DI: hard-error propagation: err=%v, firstRes.destroyCalls=%d",
		err, firstRes.destroyCalls)
}

// ── 7. Multiple custom backends can be injected simultaneously ───────────────

// TestDIMultipleCustomBackendsInjectedSimultaneously verifies that several
// custom BackendProvisioner implementations can be injected at once, and that
// results for "zfs" and "lvm" back-ends are correctly mapped to the typed
// fields while unknown backend types are carried in the pipeline but do not
// produce errors.
func TestDIMultipleCustomBackendsInjectedSimultaneously(t *testing.T) {
	t.Parallel()

	const (
		clusterName = "pillar-csi-e2e-p00006-abcd1234"
		container   = "pillar-csi-e2e-p00006-abcd1234-control-plane"
		poolName    = "e2ep-abcd1234"
		vgName      = "e2evg-abcd1234"
	)

	pool := &zfs.Pool{NodeContainer: container, PoolName: poolName}
	vg := &lvm.VG{NodeContainer: container, VGName: vgName}
	unknownRes := &destroyCountResource{description: "unknown-iscsi"}

	clusterState := stubKindBootstrapState(t, clusterName)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state, err := bootstrapSuiteBackends(ctx, clusterState, nil,
		&diZFSProvisioner{pool: pool},
		&diLVMProvisioner{vg: vg},
		&diCustomProvisioner{backendType: "iscsi", resource: unknownRes},
	)
	if err != nil {
		t.Fatalf("DI: multi-backend injection: %v", err)
	}
	if state == nil {
		t.Fatal("DI: state is nil")
	}

	// ZFS and LVM fields must be set from the injected provisioners.
	if state.ZFSPool == nil || state.ZFSPool.PoolName != poolName {
		t.Errorf("DI: ZFSPool = %v, want PoolName=%q", state.ZFSPool, poolName)
	}
	if state.LVMVG == nil || state.LVMVG.VGName != vgName {
		t.Errorf("DI: LVMVG = %v, want VGName=%q", state.LVMVG, vgName)
	}

	t.Logf("DI: multi-backend injection OK: ZFS=%q LVM=%q iscsi-resource=%q",
		state.ZFSPool.PoolName, state.LVMVG.VGName, unknownRes.description)
}

// ── 8. Nil clusterState still returns AC5 error ────────────────────────────

// TestDINilClusterStateWithProvisionersReturnsError verifies that passing nil
// clusterState returns the [AC5]-tagged error regardless of which provisioners
// are injected. This preserves the existing nil-guard contract.
func TestDINilClusterStateWithProvisionersReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := bootstrapSuiteBackends(ctx, nil, nil,
		&diCustomProvisioner{backendType: "custom", resource: &destroyCountResource{}},
	)
	if err == nil {
		t.Fatal("DI: expected error for nil clusterState, got nil")
	}
	if !strings.Contains(err.Error(), "AC5") {
		t.Errorf("DI: error %q does not contain [AC5] tag", err.Error())
	}

	t.Logf("DI: nil clusterState correctly rejected with provisioners injected: %v", err)
}

// ── 9. Interface satisfaction compile-time checks ────────────────────────────

// Compile-time: verify all test-double provisioners satisfy BackendProvisioner.
var (
	_ provisioner.BackendProvisioner = (*diZFSProvisioner)(nil)
	_ provisioner.BackendProvisioner = (*diLVMProvisioner)(nil)
	_ provisioner.BackendProvisioner = (*diCustomProvisioner)(nil)
)

// ── 10. Injected provisioner context propagation ─────────────────────────────

// TestDIContextIsPropagatedToProvisionerProvision verifies that the context
// passed to bootstrapSuiteBackends is forwarded to each provisioner's Provision
// call. A provisioner that checks context cancellation confirms the propagation.
func TestDIContextIsPropagatedToProvisionerProvision(t *testing.T) {
	t.Parallel()

	// Use a context with a known deadline so we can verify the exact context
	// was forwarded to the provisioner's Provision method.
	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// verifyFn captures the context received by Provision and returns a
	// resource so that bootstrapSuiteBackends treats it as a success.
	var receivedCtx context.Context
	verifyFn := func(ctx context.Context) (registry.Resource, error) {
		receivedCtx = ctx
		return &destroyCountResource{description: "ctx-captured"}, nil
	}

	fnProv := &funcProvisioner{
		backendType: "fn-ctx",
		fn:          verifyFn,
	}

	clusterState := stubKindBootstrapState(t, "pillar-csi-e2e-p00007-abcd1234")

	_, err := bootstrapSuiteBackends(ctx, clusterState, nil, fnProv)
	if err != nil {
		t.Fatalf("DI: context propagation test: %v", err)
	}
	if receivedCtx == nil {
		t.Fatal("DI: provisioner did not receive context")
	}
	if d, ok := receivedCtx.Deadline(); !ok || !d.Equal(deadline) {
		t.Errorf("DI: received context deadline = %v ok=%v, want %v", d, ok, deadline)
	}

	t.Logf("DI: context with deadline %v correctly propagated to provisioner", deadline)
}

// funcProvisioner is a BackendProvisioner that delegates Provision to a function.
// Used in tests to create ad-hoc provisioners without defining new named types.
type funcProvisioner struct {
	backendType string
	fn          func(ctx context.Context) (registry.Resource, error)
}

func (f *funcProvisioner) BackendType() string { return f.backendType }

func (f *funcProvisioner) Provision(ctx context.Context) (registry.Resource, error) {
	if f.fn != nil {
		return f.fn(ctx)
	}
	return nil, nil //nolint:nilnil // soft skip: BackendProvisioner contract (nil,nil) = absent resource
}

var _ provisioner.BackendProvisioner = (*funcProvisioner)(nil)

// ── 11. BackendProvisioner interface is the only extension point ──────────────

// TestDIOnlyExtensionPointIsBackendProvisionerInterface verifies that the DI
// contract is fully captured by the BackendProvisioner interface: callers that
// implement the two-method interface (BackendType, Provision) can participate
// in provisioning with no additional framework hooks required.
func TestDIOnlyExtensionPointIsBackendProvisionerInterface(t *testing.T) {
	t.Parallel()

	// This test verifies the negative: a provisioner that does NOT call any
	// framework internals (no zfs.CreatePool, no lvm.CreateVG, no shared state)
	// still integrates correctly via bootstrapSuiteBackends.

	uniqueDesc := fmt.Sprintf("standalone-backend-%d", time.Now().UnixNano())
	standaloneRes := &destroyCountResource{description: uniqueDesc}

	standalone := &funcProvisioner{
		backendType: "standalone",
		fn: func(_ context.Context) (registry.Resource, error) {
			return standaloneRes, nil
		},
	}

	clusterState := stubKindBootstrapState(t, "pillar-csi-e2e-p00008-abcd1234")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state, err := bootstrapSuiteBackends(ctx, clusterState, nil, standalone)
	if err != nil {
		t.Fatalf("DI: standalone provisioner: %v", err)
	}
	if state == nil {
		t.Fatal("DI: state is nil")
	}

	// The standalone backend's resource is not a *zfs.Pool or *lvm.VG so the
	// typed fields remain nil, but the call must succeed.
	if state.ZFSPool != nil || state.LVMVG != nil {
		t.Error("DI: ZFSPool or LVMVG unexpectedly set for a standalone backend")
	}

	t.Logf("DI: standalone backend (%q) integrated via BackendProvisioner interface only", uniqueDesc)
}
