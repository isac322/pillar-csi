package e2e

// suite_backend_ac53_test.go — Sub-AC 5.3: per-test backend isolation.
//
// Acceptance criteria verified here:
//
//  1. pillarStoreName derives a unique PillarStore name from a derived namespace:
//     - starts with "pp-"
//     - is a valid DNS label (≤ 63 chars, matches [a-z0-9]([-a-z0-9]*[a-z0-9])?)
//     - is globally unique across all TC IDs in the test suite
//  2. perTestLVMVGName derives a unique LVM VG name from a derived namespace:
//     - starts with "vg-"
//     - is a valid LVM VG name (≤ 127 chars, alphanumeric + hyphens)
//     - is globally unique across all TC IDs in the test suite
//  3. No two TCs running concurrently share the same PillarStore name or VG name
//     (derived from distinct DerivedNamespaces → distinct derived names).
//  4. ProvisionPerTestPillarStore creates a PillarStore CR with the correct spec:
//     - name matches pillarStoreName(scope.DerivedNamespace)
//     - spec.agentRef matches the given agentRef
//     - spec.backend.type == "lvm-lv"
//     - spec.backend.lvm.volumeGroup matches perTestLVMVGName(scope.DerivedNamespace)
//     - spec.backend.lvm.provisioningMode == "linear"
//  5. ProvisionPerTestPillarStore registers teardown: after scope.Close() the
//     PillarStore CR is deleted and verified absent via the fake K8s client.
//  6. ProvisionPerTestPillarStore rejects nil scope, nil client, empty agentRef.
//  7. namespaceSuffix returns at most 12 characters and never starts with a hyphen.
//  8. PillarStore names and VG names never collide across all 437+ TC IDs registered
//     in the tc_manifest.csv catalog (uniqueness under load).

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	pillarv1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/names"
)

// newAC53FakeClient builds a fake k8s client with the PillarStore scheme
// registered and a PillarAgent stub that satisfies FK constraints.
func newAC53FakeClient(targetName string) client.Client {
	scheme := runtime.NewScheme()
	if err := pillarv1.AddToScheme(scheme); err != nil {
		panic(fmt.Sprintf("AC53: register pillar scheme: %v", err))
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		panic(fmt.Sprintf("AC53: register corev1 scheme: %v", err))
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		panic(fmt.Sprintf("AC53: register storagev1 scheme: %v", err))
	}

	target := &pillarv1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: targetName},
	}

	return clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(target).
		Build()
}

// ── 1. pillarStoreName format and DNS safety ──────────────────────────────────

// TestAC53PillarStoreNameStartsWithPpPrefix verifies that pillarStoreName always
// produces a name that starts with "pp-", identifying it as a PillarStore resource.
func TestAC53PillarStoreNameStartsWithPpPrefix(t *testing.T) {
	t.Parallel()

	namespaces := []string{
		"e2e-tc-e3-1-ab12cd34",
		"e2e-tc-lvm-test-ff00ee11",
		"e2e-tc-e437-1-12345678",
		"e2e-tc-short-0a0a0a0a",
		"e2e-tc-x-00000001",
	}

	for _, ns := range namespaces {
		got := pillarStoreName(ns)
		if !strings.HasPrefix(got, "pp-") {
			t.Errorf("AC53: pillarStoreName(%q) = %q, want prefix 'pp-'", ns, got)
		}
	}
	t.Logf("AC53: all pillarStoreName results have 'pp-' prefix")
}

// TestAC53PillarStoreNameIsDNSSafe verifies that pillarStoreName produces
// valid Kubernetes resource names: DNS labels ≤ 63 characters matching
// [a-z0-9]([-a-z0-9]*[a-z0-9])?.
func TestAC53PillarStoreNameIsDNSSafe(t *testing.T) {
	t.Parallel()

	dnsLabelRE := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	namespaces := []string{
		"e2e-tc-e3-1-ab12cd34",
		"e2e-tc-e30-1-ff00ee11",
		"e2e-tc-a-12345678",
		names.Namespace("E3.1"),
		names.Namespace("E30.1"),
		names.Namespace("E437.1"),
	}

	for _, ns := range namespaces {
		got := pillarStoreName(ns)

		if len(got) > 63 {
			t.Errorf("AC53: pillarStoreName(%q) = %q has %d chars, exceeds DNS label limit of 63",
				ns, got, len(got))
		}
		if !dnsLabelRE.MatchString(got) {
			t.Errorf("AC53: pillarStoreName(%q) = %q is not a valid DNS label (pattern: %s)",
				ns, got, dnsLabelRE.String())
		}
	}
	t.Logf("AC53: all pillarStoreName results are DNS-safe")
}

// ── 2. perTestLVMVGName format and safety ────────────────────────────────────

// TestAC53LVMVGNameStartsWithVgPrefix verifies that perTestLVMVGName always
// produces a name that starts with "vg-", distinguishing per-test VGs from
// the suite-shared VG ("pillar-e2e-lvm-*").
func TestAC53LVMVGNameStartsWithVgPrefix(t *testing.T) {
	t.Parallel()

	namespaces := []string{
		"e2e-tc-e3-1-ab12cd34",
		"e2e-tc-lvm-test-ff00ee11",
		names.Namespace("E3.1"),
		names.Namespace("E437.1"),
	}

	for _, ns := range namespaces {
		got := perTestLVMVGName(ns)
		if !strings.HasPrefix(got, "vg-") {
			t.Errorf("AC53: perTestLVMVGName(%q) = %q, want prefix 'vg-'", ns, got)
		}
	}
	t.Logf("AC53: all perTestLVMVGName results have 'vg-' prefix")
}

// TestAC53LVMVGNameFitsWithinOSLimit verifies that derived LVM VG names are
// within LVM's 127-character Volume Group name limit.
func TestAC53LVMVGNameFitsWithinOSLimit(t *testing.T) {
	t.Parallel()

	const lvmVGNameLimit = 127

	ns := names.Namespace("E-very-long-test-case-id-that-produces-a-long-namespace-slug")
	got := perTestLVMVGName(ns)
	if len(got) > lvmVGNameLimit {
		t.Errorf("AC53: perTestLVMVGName(%q) = %q has %d chars, exceeds LVM limit of %d",
			ns, got, len(got), lvmVGNameLimit)
	}
	t.Logf("AC53: perTestLVMVGName(%q) = %q (%d chars, within limit)", ns, got, len(got))
}

// ── 3. Uniqueness across parallel TCs ────────────────────────────────────────

// TestAC53NamingIsUniqueAcrossParallelTCs verifies that concurrent test cases
// with distinct TC IDs always receive distinct PillarStore names and LVM VG names,
// which is the core AC 5.3 isolation invariant.
func TestAC53NamingIsUniqueAcrossParallelTCs(t *testing.T) {
	t.Parallel()

	// Use a representative sample of TC IDs (E1.1–E30.1 + some variants).
	tcIDs := make([]string, 0, 60)
	for i := 1; i <= 30; i++ {
		tcIDs = append(tcIDs, fmt.Sprintf("E%d.1", i))
		tcIDs = append(tcIDs, fmt.Sprintf("E%d.2", i))
	}

	type result struct {
		tcID     string
		ns       string
		poolName string
		vgName   string
	}

	results := make([]result, len(tcIDs))
	var wg sync.WaitGroup

	for i, id := range tcIDs {
		wg.Add(1)
		go func(idx int, tcID string) {
			defer wg.Done()
			ns := names.Namespace(tcID)
			results[idx] = result{
				tcID:     tcID,
				ns:       ns,
				poolName: pillarStoreName(ns),
				vgName:   perTestLVMVGName(ns),
			}
		}(i, id)
	}
	wg.Wait()

	// Verify PillarStore name uniqueness.
	poolsSeen := make(map[string]string, len(results)) // poolName → tcID
	for _, r := range results {
		if prev, dup := poolsSeen[r.poolName]; dup {
			t.Errorf("AC53: PillarStore name %q duplicated for TC %q and TC %q — isolation violated",
				r.poolName, prev, r.tcID)
		}
		poolsSeen[r.poolName] = r.tcID
	}

	// Verify LVM VG name uniqueness.
	vgsSeen := make(map[string]string, len(results)) // vgName → tcID
	for _, r := range results {
		if prev, dup := vgsSeen[r.vgName]; dup {
			t.Errorf("AC53: LVM VG name %q duplicated for TC %q and TC %q — isolation violated",
				r.vgName, prev, r.tcID)
		}
		vgsSeen[r.vgName] = r.tcID
	}

	if !t.Failed() {
		t.Logf("AC53: %d TCs each received unique PillarStore names and LVM VG names", len(results))
	}
}

// TestAC53NamingIsUniqueAcrossTestCaseScopes verifies that NewTestCaseScope
// produces distinct DerivedNamespaces for distinct TC IDs, and that the
// derived PillarStore / VG names are therefore distinct.
func TestAC53NamingIsUniqueAcrossTestCaseScopes(t *testing.T) {
	t.Parallel()

	const numScopes = 8

	type names struct {
		poolName string
		vgName   string
	}

	results := make([]names, numScopes)
	var wg sync.WaitGroup

	for i := range numScopes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			scope, err := NewTestCaseScope(fmt.Sprintf("ac53-isolation-tc-%d", idx))
			if err != nil {
				t.Errorf("AC53: goroutine %d: NewTestCaseScope: %v", idx, err)
				return
			}
			defer func() { _ = scope.Close() }()

			results[idx] = names{
				poolName: pillarStoreName(scope.DerivedNamespace),
				vgName:   perTestLVMVGName(scope.DerivedNamespace),
			}
		}(i)
	}
	wg.Wait()

	// Verify all PillarStore names are distinct.
	poolsSeen := make(map[string]int)
	for i, n := range results {
		if prev, dup := poolsSeen[n.poolName]; dup {
			t.Errorf("AC53: PillarStore name %q duplicated at indices %d and %d — parallel isolation violated",
				n.poolName, prev, i)
		}
		poolsSeen[n.poolName] = i
	}

	// Verify all LVM VG names are distinct.
	vgsSeen := make(map[string]int)
	for i, n := range results {
		if prev, dup := vgsSeen[n.vgName]; dup {
			t.Errorf("AC53: LVM VG name %q duplicated at indices %d and %d — parallel isolation violated",
				n.vgName, prev, i)
		}
		vgsSeen[n.vgName] = i
	}

	if !t.Failed() {
		t.Logf("AC53: %d concurrent TestCaseScopes each produced distinct PillarStore and VG names",
			numScopes)
	}
}

// ── 4. ProvisionPerTestPillarStore creates a correct PillarStore CR ─────────────

// TestAC53ProvisionPerTestPillarStoreCreatesCorrectSpec verifies that
// ProvisionPerTestPillarStore creates a PillarStore CR with:
//   - Name derived from scope.DerivedNamespace via pillarStoreName
//   - spec.agentRef matching the given agentRef
//   - spec.backend.type == "lvm-lv"
//   - spec.backend.lvm.volumeGroup derived from scope.DerivedNamespace via perTestLVMVGName
//   - spec.backend.lvm.provisioningMode == "linear"
func TestAC53ProvisionPerTestPillarStoreCreatesCorrectSpec(t *testing.T) {
	t.Parallel()

	const agentRef = "storage-node-1"

	scope, err := NewTestCaseScope("ac53-create-spec-tc")
	if err != nil {
		t.Fatalf("AC53: NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	k8sClient := newAC53FakeClient(agentRef)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle, err := ProvisionPerTestPillarStore(ctx, scope, k8sClient, agentRef)
	if err != nil {
		t.Fatalf("AC53: ProvisionPerTestPillarStore: %v", err)
	}

	// Verify returned handle fields.
	wantPoolName := pillarStoreName(scope.DerivedNamespace)
	wantVGName := perTestLVMVGName(scope.DerivedNamespace)

	if handle.PoolName != wantPoolName {
		t.Errorf("AC53: handle.PoolName = %q, want %q", handle.PoolName, wantPoolName)
	}
	if handle.VGName != wantVGName {
		t.Errorf("AC53: handle.VGName = %q, want %q", handle.VGName, wantVGName)
	}
	if handle.AgentRef != agentRef {
		t.Errorf("AC53: handle.AgentRef = %q, want %q", handle.AgentRef, agentRef)
	}

	// Verify the PillarStore CR was created with the correct spec.
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fetchCancel()

	pool := &pillarv1.PillarStore{}
	if err := k8sClient.Get(fetchCtx, client.ObjectKey{Name: wantPoolName}, pool); err != nil {
		t.Fatalf("AC53: get created PillarStore %q: %v", wantPoolName, err)
	}

	if pool.Spec.AgentRef != agentRef {
		t.Errorf("AC53: pool.Spec.AgentRef = %q, want %q", pool.Spec.AgentRef, agentRef)
	}
	if pool.Spec.Backend.Type != pillarv1.BackendTypeLVMLV {
		t.Errorf("AC53: pool.Spec.Backend.Type = %q, want %q",
			pool.Spec.Backend.Type, pillarv1.BackendTypeLVMLV)
	}
	if pool.Spec.Backend.LVM == nil {
		t.Fatal("AC53: pool.Spec.Backend.LVM is nil — LVM config not set")
	}
	if pool.Spec.Backend.LVM.VolumeGroup != wantVGName {
		t.Errorf("AC53: pool.Spec.Backend.LVM.VolumeGroup = %q, want %q",
			pool.Spec.Backend.LVM.VolumeGroup, wantVGName)
	}
	if pool.Spec.Backend.LVM.ProvisioningMode != pillarv1.LVMProvisioningModeLinear {
		t.Errorf("AC53: pool.Spec.Backend.LVM.ProvisioningMode = %q, want %q",
			pool.Spec.Backend.LVM.ProvisioningMode, pillarv1.LVMProvisioningModeLinear)
	}

	t.Logf("AC53: PillarStore %q created with correct spec (target=%s, vg=%s, type=%s, mode=%s)",
		wantPoolName, agentRef, wantVGName,
		pool.Spec.Backend.Type, pool.Spec.Backend.LVM.ProvisioningMode)
}

// ── 5. Teardown deletes the PillarStore CR ────────────────────────────────────

// TestAC53TeardownDeletesPillarStoreCR verifies that after scope.Close(), the
// PillarStore CR created by ProvisionPerTestPillarStore no longer exists in the
// fake K8s client.
func TestAC53TeardownDeletesPillarStoreCR(t *testing.T) {
	t.Parallel()

	const agentRef = "storage-node-teardown"

	scope, err := NewTestCaseScope("ac53-teardown-tc")
	if err != nil {
		t.Fatalf("AC53: NewTestCaseScope: %v", err)
	}

	k8sClient := newAC53FakeClient(agentRef)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle, err := ProvisionPerTestPillarStore(ctx, scope, k8sClient, agentRef)
	if err != nil {
		_ = scope.Close()
		t.Fatalf("AC53: ProvisionPerTestPillarStore: %v", err)
	}

	// Verify the PillarStore exists before teardown.
	pool := &pillarv1.PillarStore{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: handle.PoolName}, pool); err != nil {
		_ = scope.Close()
		t.Fatalf("AC53: PillarStore %q should exist before teardown: %v", handle.PoolName, err)
	}

	// Run teardown.
	if closeErr := scope.Close(); closeErr != nil {
		t.Errorf("AC53: scope.Close() after ProvisionPerTestPillarStore: %v", closeErr)
	}

	// Verify the PillarStore no longer exists.
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer checkCancel()

	deleted := &pillarv1.PillarStore{}
	err = k8sClient.Get(checkCtx, client.ObjectKey{Name: handle.PoolName}, deleted)
	if err == nil {
		t.Errorf("AC53: PillarStore %q still exists after scope.Close() — teardown did not delete it",
			handle.PoolName)
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("AC53: checking PillarStore %q after teardown: unexpected error: %v",
			handle.PoolName, err)
	} else {
		t.Logf("AC53: PillarStore %q correctly absent after teardown", handle.PoolName)
	}
}

// ── 6. Input validation ───────────────────────────────────────────────────────

// TestAC53ProvisionPerTestPillarStoreRejectsNilScope verifies that
// ProvisionPerTestPillarStore returns a descriptive error when scope is nil.
func TestAC53ProvisionPerTestPillarStoreRejectsNilScope(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	k8sClient := newAC53FakeClient("any-target")
	_, err := ProvisionPerTestPillarStore(ctx, nil, k8sClient, "any-target")
	if err == nil {
		t.Fatal("AC53: ProvisionPerTestPillarStore(nil scope) returned nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "AC5.3") {
		t.Errorf("AC53: error %q does not contain [AC5.3] tag for traceability", err.Error())
	}
	t.Logf("AC53: nil scope correctly rejected: %v", err)
}

// TestAC53ProvisionPerTestPillarStoreRejectsNilClient verifies that
// ProvisionPerTestPillarStore returns a descriptive error when k8sClient is nil.
func TestAC53ProvisionPerTestPillarStoreRejectsNilClient(t *testing.T) {
	t.Parallel()

	scope, err := NewTestCaseScope("ac53-nil-client-tc")
	if err != nil {
		t.Fatalf("AC53: NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = ProvisionPerTestPillarStore(ctx, scope, nil, "any-target")
	if err == nil {
		t.Fatal("AC53: ProvisionPerTestPillarStore(nil client) returned nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "AC5.3") {
		t.Errorf("AC53: error %q does not contain [AC5.3] tag for traceability", err.Error())
	}
	t.Logf("AC53: nil k8sClient correctly rejected: %v", err)
}

// TestAC53ProvisionPerTestPillarStoreRejectsEmptyAgentRef verifies that
// ProvisionPerTestPillarStore returns a descriptive error when agentRef is empty.
func TestAC53ProvisionPerTestPillarStoreRejectsEmptyAgentRef(t *testing.T) {
	t.Parallel()

	scope, err := NewTestCaseScope("ac53-empty-target-tc")
	if err != nil {
		t.Fatalf("AC53: NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	k8sClient := newAC53FakeClient("placeholder")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = ProvisionPerTestPillarStore(ctx, scope, k8sClient, "")
	if err == nil {
		t.Fatal("AC53: ProvisionPerTestPillarStore(empty agentRef) returned nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "AC5.3") {
		t.Errorf("AC53: error %q does not contain [AC5.3] tag for traceability", err.Error())
	}
	t.Logf("AC53: empty agentRef correctly rejected: %v", err)
}

// ── 7. namespaceSuffix properties ────────────────────────────────────────────

// TestAC53NamespaceSuffixLengthBound verifies that namespaceSuffix(ns, 12)
// returns at most 12 characters for any input.
func TestAC53NamespaceSuffixLengthBound(t *testing.T) {
	t.Parallel()

	testCases := []string{
		"e2e-tc-e3-1-ab12cd34",
		"e2e-tc-a-short",
		"short",
		"x",
		"e2e-tc-very-long-test-case-id-with-many-words-0a0b0c0d",
		names.Namespace("E437.1"),
	}

	for _, ns := range testCases {
		got := namespaceSuffix(ns, 12)
		if len(got) > 12 {
			t.Errorf("AC53: namespaceSuffix(%q, 12) = %q has %d chars, want ≤ 12",
				ns, got, len(got))
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("AC53: namespaceSuffix(%q, 12) = %q has leading/trailing hyphen",
				ns, got)
		}
	}
	t.Logf("AC53: namespaceSuffix(12) respects length bound for %d inputs", len(testCases))
}

// TestAC53NamespaceSuffixNeverEmpty verifies that namespaceSuffix always
// returns a non-empty string, even for degenerate inputs.
func TestAC53NamespaceSuffixNeverEmpty(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"",
		"-",
		"--",
		"a",
		names.Namespace("E1.1"),
	}

	for _, ns := range inputs {
		got := namespaceSuffix(ns, 12)
		if got == "" {
			t.Errorf("AC53: namespaceSuffix(%q, 12) = empty string, want non-empty", ns)
		}
	}
	t.Logf("AC53: namespaceSuffix always returns a non-empty string")
}

// ── 8. Uniqueness across all catalog TC IDs ───────────────────────────────────

// TestAC53PillarStoreNamesAreUniqueAcrossAllCatalogTCIDs verifies that
// PillarStore names and LVM VG names are globally unique across a set of
// TC IDs representative of the full test suite catalog.
//
// This catches any hash collisions in the naming scheme before they could
// cause test flakes from resource name conflicts.
func TestAC53PillarStoreNamesAreUniqueAcrossAllCatalogTCIDs(t *testing.T) {
	t.Parallel()

	// Build a representative set of TC IDs matching the catalog's pattern.
	// Real catalog has ~437 TCs in the E1–E437 range plus variants.
	tcIDs := make([]string, 0, 450)
	for i := 1; i <= 437; i++ {
		tcIDs = append(tcIDs, fmt.Sprintf("E%d.1", i))
	}
	// Add a few variant IDs to exercise the hash path for collisions.
	tcIDs = append(tcIDs, []string{
		"E1.2", "E2.2", "E3.2", "E10.2", "E100.2",
		"cluster-verify", "kind-lifecycle", "backend-smoke",
	}...)

	poolNamesSeen := make(map[string]string, len(tcIDs)) // poolName → tcID
	vgNamesSeen := make(map[string]string, len(tcIDs))   // vgName → tcID
	collisions := 0

	for _, tcID := range tcIDs {
		ns := names.Namespace(tcID)
		poolName := pillarStoreName(ns)
		vgName := perTestLVMVGName(ns)

		if prev, dup := poolNamesSeen[poolName]; dup {
			t.Errorf("AC53: PillarStore name collision: %q shared by TC %q and TC %q",
				poolName, prev, tcID)
			collisions++
		}
		poolNamesSeen[poolName] = tcID

		if prev, dup := vgNamesSeen[vgName]; dup {
			t.Errorf("AC53: LVM VG name collision: %q shared by TC %q and TC %q",
				vgName, prev, tcID)
			collisions++
		}
		vgNamesSeen[vgName] = tcID
	}

	if collisions == 0 {
		t.Logf("AC53: %d TC IDs produced %d unique PillarStore names and %d unique VG names — no collisions",
			len(tcIDs), len(poolNamesSeen), len(vgNamesSeen))
	} else {
		t.Errorf("AC53: %d name collision(s) detected across %d TC IDs", collisions, len(tcIDs))
	}
}

// ── 9. ProvisionPerTestPillarStore is idempotent-protected by TrackBackendRecord ──

// TestAC53DoubleProvisionForSameScopeReturnsError verifies that calling
// ProvisionPerTestPillarStore twice on the same scope returns an error for
// the second call, because TrackBackendRecord prevents duplicate registration
// of the same resource key.
//
// This test ensures the scope's resource registry acts as the guard against
// accidental double-provisioning.
func TestAC53DoubleProvisionForSameScopeReturnsError(t *testing.T) {
	t.Parallel()

	const agentRef = "storage-node-double"

	scope, err := NewTestCaseScope("ac53-double-provision-tc")
	if err != nil {
		t.Fatalf("AC53: NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	k8sClient := newAC53FakeClient(agentRef)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First call must succeed.
	_, err = ProvisionPerTestPillarStore(ctx, scope, k8sClient, agentRef)
	if err != nil {
		t.Fatalf("AC53: first ProvisionPerTestPillarStore: %v", err)
	}

	// Second call with the same scope must return an error because
	// TrackBackendRecord rejects duplicate keys. The fake client will also
	// return AlreadyExists for the Create, but the guard fires first.
	_, err = ProvisionPerTestPillarStore(ctx, scope, k8sClient, agentRef)
	if err == nil {
		t.Error("AC53: second ProvisionPerTestPillarStore on same scope returned nil error — double-provision not prevented")
	} else {
		t.Logf("AC53: second ProvisionPerTestPillarStore correctly rejected: %v", err)
	}
}

// ── 10. Pool and VG names are traceable to their TC namespace ─────────────────

// TestAC53PoolAndVGNamesEmbedNamespaceHash verifies that derived PillarStore
// and VG names embed the hash suffix from the derived namespace, so that a
// pool/VG name can be correlated back to its owning TC ID in logs.
func TestAC53PoolAndVGNamesEmbedNamespaceHash(t *testing.T) {
	t.Parallel()

	tcID := "E42.1"
	ns := names.Namespace(tcID) // "e2e-tc-e42-1-<hash8>"

	poolName := pillarStoreName(ns)
	vgName := perTestLVMVGName(ns)

	// The derived namespace ends in an 8-char hex hash.  Verify that either the
	// pool name or the VG name contains at least 4 of those last 8 hex characters,
	// demonstrating hash propagation.  (The suffix is 12 chars, so the last 8 of
	// the namespace are fully included when the namespace is ≥ 12 chars.)
	hashSuffix := ns[len(ns)-8:] // last 8 chars of the namespace

	if !strings.Contains(poolName, hashSuffix[4:]) {
		t.Errorf("AC53: PillarStore name %q does not contain namespace hash fragment %q from ns %q",
			poolName, hashSuffix[4:], ns)
	}
	if !strings.Contains(vgName, hashSuffix[4:]) {
		t.Errorf("AC53: LVM VG name %q does not contain namespace hash fragment %q from ns %q",
			vgName, hashSuffix[4:], ns)
	}

	t.Logf("AC53: TC %q → ns=%q → pool=%q, vg=%q (hash fragment %q preserved)",
		tcID, ns, poolName, vgName, hashSuffix[4:])
}

// ── helper: newAC53FakeClient is defined in this file ────────────────────────

// TestAC53FakeClientHasPillarStoreScheme verifies that newAC53FakeClient
// returns a client that can Create, Get, and Delete PillarStore resources.
// This ensures the test helper is correctly configured for all AC53 tests.
func TestAC53FakeClientHasPillarStoreScheme(t *testing.T) {
	t.Parallel()

	k8sClient := newAC53FakeClient("storage-1")

	pool := &pillarv1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "pp-scheme-check"},
		Spec: pillarv1.PillarStoreSpec{
			AgentRef: "storage-1",
			Backend: pillarv1.BackendSpec{
				Type: pillarv1.BackendTypeLVMLV,
				LVM:  &pillarv1.LVMBackendConfig{VolumeGroup: "vg-test"},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("AC53: fake client Create PillarStore: %v", err)
	}

	got := &pillarv1.PillarStore{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "pp-scheme-check"}, got); err != nil {
		t.Fatalf("AC53: fake client Get PillarStore: %v", err)
	}

	if err := k8sClient.Delete(ctx, got); err != nil {
		t.Fatalf("AC53: fake client Delete PillarStore: %v", err)
	}

	missing := &pillarv1.PillarStore{}
	err := k8sClient.Get(ctx, client.ObjectKey{Name: "pp-scheme-check"}, missing)
	if !apierrors.IsNotFound(err) {
		t.Errorf("AC53: PillarStore should be NotFound after delete, got: %v", err)
	}

	t.Logf("AC53: fake client correctly handles PillarStore CRUD operations")
}
