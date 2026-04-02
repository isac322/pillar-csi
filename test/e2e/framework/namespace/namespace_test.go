package namespace_test

// namespace_test.go verifies Sub-AC 5.1:
//
//   Implement a test fixture factory that generates a unique namespace name
//   per test case (e.g. e2e-test-<uuid>) and creates/deletes it via the
//   Kubernetes client in TestMain or t.Cleanup.
//
// Tests in this file run against a fake kubernetes client so they execute
// without a live Kind cluster and without any build tags.  The fake client
// from k8s.io/client-go/kubernetes/fake records all API calls in memory and
// lets the tests assert that namespaces were created and cleaned up correctly.
//
// # Coverage
//
//   5.1.1  New / NewWithClient produce a name with the e2e-test-<uuid> format.
//   5.1.2  Name() returns the same value consistently.
//   5.1.3  Name() returns "" for a nil receiver.
//   5.1.4  Namespace is created in the cluster (fake client records the call).
//   5.1.5  Namespace is deleted when t.Cleanup runs (fake client confirms absence).
//   5.1.6  Custom prefix is respected by NewWithPrefix.
//   5.1.7  Empty prefix falls back to DefaultPrefix.
//   5.1.8  Each fixture gets a distinct name even when created in the same test.
//   5.1.9  Cleanup silently ignores 404 (namespace already deleted by the test).
//   5.1.10 buildNamespaceName truncates a long prefix while preserving the UUID.
//   5.1.11 Namespace names are valid Kubernetes DNS labels (≤63 chars, chars ok).
//   5.1.12 Cleanup labels mark the namespace as managed-by=e2e-fixture.

import (
	"context"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/namespace"
)

// uuidPattern matches a standard lowercase UUID v4 string.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// dnsLabelPattern matches valid Kubernetes namespace names.
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]*[a-z0-9]$`)

// ─── 5.1.1 ─── name format ───────────────────────────────────────────────────

// TestNameFormat verifies that NewWithClient produces a namespace name with the
// documented format: e2e-test-<uuid>.
func TestNameFormat(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	f := namespace.NewWithClient(t, client, namespace.DefaultPrefix)

	name := f.Name()

	if !strings.HasPrefix(name, namespace.DefaultPrefix+"-") {
		t.Errorf("5.1.1: Name() = %q, want prefix %q-", name, namespace.DefaultPrefix)
	}

	// Extract the UUID part (everything after the prefix and dash).
	suffix := strings.TrimPrefix(name, namespace.DefaultPrefix+"-")
	if !uuidPattern.MatchString(suffix) {
		t.Errorf("5.1.1: Name() suffix %q is not a valid UUID (pattern %s)", suffix, uuidPattern)
	}
}

// ─── 5.1.2 ─── Name() is stable ──────────────────────────────────────────────

// TestNameIsStable verifies that Name() returns the same value on every call.
func TestNameIsStable(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	f := namespace.NewWithClient(t, client, "")

	first := f.Name()
	if first == "" {
		t.Fatal("5.1.2: Name() returned empty string after construction")
	}
	for i := 0; i < 5; i++ {
		if got := f.Name(); got != first {
			t.Errorf("5.1.2: Name() call %d = %q, want stable %q", i, got, first)
		}
	}
}

// ─── 5.1.3 ─── nil receiver ───────────────────────────────────────────────────

// TestNilReceiverNameReturnsEmpty verifies the nil guard on Name().
func TestNilReceiverNameReturnsEmpty(t *testing.T) {
	t.Parallel()

	var f *namespace.Fixture
	if got := f.Name(); got != "" {
		t.Errorf("5.1.3: nil.Name() = %q, want empty string", got)
	}
}

// ─── 5.1.4 ─── namespace is created in the cluster ───────────────────────────

// TestNamespaceCreatedInCluster verifies that NewWithClient creates the namespace
// in the Kubernetes API server (fake client).
func TestNamespaceCreatedInCluster(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	f := namespace.NewWithClient(t, client, namespace.DefaultPrefix)

	ctx := context.Background()
	ns, err := client.CoreV1().Namespaces().Get(ctx, f.Name(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("5.1.4: namespace %q not found in cluster: %v", f.Name(), err)
	}
	if ns.Name != f.Name() {
		t.Errorf("5.1.4: cluster namespace name = %q, want %q", ns.Name, f.Name())
	}
}

// ─── 5.1.5 ─── namespace is deleted on t.Cleanup ─────────────────────────────

// TestNamespaceDeletedOnCleanup verifies that t.Cleanup (simulated via
// a sub-test) deletes the namespace from the cluster.
func TestNamespaceDeletedOnCleanup(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	var capturedName string

	// Run the fixture in a sub-test so its t.Cleanup fires before this
	// test's assertions execute (sub-test Cleanup runs when the sub-test ends).
	t.Run("fixture-scope", func(inner *testing.T) {
		f := namespace.NewWithClient(inner, client, namespace.DefaultPrefix)
		capturedName = f.Name()

		// Verify the namespace exists inside the sub-test.
		ctx := context.Background()
		if _, err := client.CoreV1().Namespaces().Get(ctx, capturedName, metav1.GetOptions{}); err != nil {
			inner.Fatalf("5.1.5: namespace %q not created: %v", capturedName, err)
		}
		// inner.Cleanup runs when inner test returns, deleting the namespace.
	})

	// After the sub-test ends its Cleanup has fired; the namespace must be gone.
	ctx := context.Background()
	_, err := client.CoreV1().Namespaces().Get(ctx, capturedName, metav1.GetOptions{})
	if err == nil {
		t.Errorf("5.1.5: namespace %q still exists after t.Cleanup", capturedName)
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("5.1.5: unexpected error checking deleted namespace %q: %v", capturedName, err)
	}
}

// ─── 5.1.6 ─── custom prefix ──────────────────────────────────────────────────

// TestCustomPrefix verifies that NewWithPrefix honours the caller-supplied prefix.
func TestCustomPrefix(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	const myPrefix = "e2e-pvc"
	f := namespace.NewWithClient(t, client, myPrefix)

	if !strings.HasPrefix(f.Name(), myPrefix+"-") {
		t.Errorf("5.1.6: Name() = %q, want prefix %q-", f.Name(), myPrefix)
	}
}

// ─── 5.1.7 ─── empty prefix falls back to DefaultPrefix ──────────────────────

// TestEmptyPrefixFallback verifies that an empty prefix is replaced by DefaultPrefix.
func TestEmptyPrefixFallback(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	f := namespace.NewWithClient(t, client, "")

	if !strings.HasPrefix(f.Name(), namespace.DefaultPrefix+"-") {
		t.Errorf("5.1.7: Name() = %q, want prefix %q- (fallback to DefaultPrefix)", f.Name(), namespace.DefaultPrefix)
	}
}

// ─── 5.1.8 ─── each fixture gets a distinct name ────────────────────────────

// TestEachFixtureHasDistinctName verifies that concurrent fixtures within the
// same test always receive different namespace names.
func TestEachFixtureHasDistinctName(t *testing.T) {
	t.Parallel()

	const count = 10
	client := fake.NewSimpleClientset()
	seen := make(map[string]int)

	for i := 0; i < count; i++ {
		f := namespace.NewWithClient(t, client, namespace.DefaultPrefix)
		name := f.Name()
		if prev, exists := seen[name]; exists {
			t.Errorf("5.1.8: name collision: %q was already used at iteration %d (current=%d)", name, prev, i)
		}
		seen[name] = i
	}
}

// ─── 5.1.9 ─── cleanup ignores 404 ──────────────────────────────────────────

// TestCleanupIgnoresNotFound verifies that t.Cleanup does not fail the test
// when the namespace has already been deleted by the test body itself.
func TestCleanupIgnoresNotFound(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()

	t.Run("fixture-scope", func(inner *testing.T) {
		f := namespace.NewWithClient(inner, client, namespace.DefaultPrefix)

		// Delete the namespace explicitly inside the test body.
		ctx := context.Background()
		if err := client.CoreV1().Namespaces().Delete(ctx, f.Name(), metav1.DeleteOptions{}); err != nil {
			inner.Fatalf("5.1.9: explicit delete of %q failed: %v", f.Name(), err)
		}
		// inner.Cleanup will fire next; it must not call inner.Fatal/Error
		// because the namespace is already gone (404).  If it did, the sub-test
		// would fail here, which would propagate as a failure to the outer test.
	})
	// If we reach here, Cleanup silently ignored the 404 — test passes.
}

// ─── 5.1.10 ─── long prefix truncation ───────────────────────────────────────

// TestBuildNamespaceName_LongPrefix verifies that buildNamespaceName (via
// NewWithClient) truncates a very long prefix while keeping the UUID intact.
func TestBuildNamespaceName_LongPrefix(t *testing.T) {
	t.Parallel()

	// A prefix longer than 63 chars to force truncation.
	longPrefix := strings.Repeat("e2e-fixture-", 10) // 120 chars

	client := fake.NewSimpleClientset()
	f := namespace.NewWithClient(t, client, longPrefix)

	name := f.Name()
	if len(name) > 63 {
		t.Errorf("5.1.10: Name() length = %d, want ≤63: %q", len(name), name)
	}
	// The UUID portion (36 chars) must appear at the end of the name.
	// Split on the last '-' before the UUID section.
	if len(name) < 36 {
		t.Fatalf("5.1.10: Name() too short to contain UUID: %q", name)
	}
	uuidSuffix := name[len(name)-36:]
	if !uuidPattern.MatchString(uuidSuffix) {
		t.Errorf("5.1.10: Name() UUID suffix %q is not a valid UUID", uuidSuffix)
	}
}

// ─── 5.1.11 ─── DNS-label validity ───────────────────────────────────────────

// TestNameIsDNSLabel verifies that all generated names are valid Kubernetes
// DNS-label names: ≤63 chars, lowercase alphanumeric and hyphens only,
// starting and ending with alphanumeric.
func TestNameIsDNSLabel(t *testing.T) {
	t.Parallel()

	prefixes := []string{
		namespace.DefaultPrefix,
		"e2e-pvc",
		"e2e-zfs",
		"e2e-lvm",
		"x",
	}

	client := fake.NewSimpleClientset()
	for _, prefix := range prefixes {
		f := namespace.NewWithClient(t, client, prefix)
		name := f.Name()

		if len(name) > 63 {
			t.Errorf("5.1.11: prefix=%q Name() length = %d > 63: %q", prefix, len(name), name)
		}
		// Must contain only lowercase alphanumeric and hyphens.
		if matched := regexp.MustCompile(`^[a-z0-9][a-z0-9\-]*[a-z0-9]$`).MatchString(name); !matched {
			t.Errorf("5.1.11: prefix=%q Name() = %q is not a valid DNS label", prefix, name)
		}
	}
}

// ─── 5.1.12 ─── labels on the namespace ──────────────────────────────────────

// TestNamespaceHasManagedByLabel verifies that the created namespace carries
// the expected management labels so stale namespaces can be identified and
// cleaned up easily.
func TestNamespaceHasManagedByLabel(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	f := namespace.NewWithClient(t, client, namespace.DefaultPrefix)

	ctx := context.Background()
	ns, err := client.CoreV1().Namespaces().Get(ctx, f.Name(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("5.1.12: namespace %q not found: %v", f.Name(), err)
	}

	labels := ns.Labels
	if labels == nil {
		t.Fatal("5.1.12: namespace has no labels")
	}

	wantLabels := map[string]string{
		"pillar-csi.bhyoo.com/managed-by":   "e2e-fixture",
		"pillar-csi.bhyoo.com/fixture-type": "namespace",
	}
	for k, want := range wantLabels {
		got, ok := labels[k]
		if !ok {
			t.Errorf("5.1.12: namespace label %q missing", k)
			continue
		}
		if got != want {
			t.Errorf("5.1.12: namespace label %q = %q, want %q", k, got, want)
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// namespaceExistsInFakeClient checks whether a namespace with the given name
// is present in the fake kubernetes client.
func namespaceExistsInFakeClient(t *testing.T, client *fake.Clientset, nsName string) bool {
	t.Helper()
	_, err := client.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false
		}
		t.Fatalf("unexpected error checking namespace %q: %v", nsName, err)
	}
	return true
}

// verifyNamespaceHasLabel is a test helper that retrieves a namespace and
// asserts that it carries the given label key/value pair.
func verifyNamespaceHasLabel(t *testing.T, client *fake.Clientset, nsName, key, value string) {
	t.Helper()
	ns, err := client.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace %q for label check: %v", nsName, err)
	}
	if ns.Labels[key] != value {
		t.Errorf("namespace %q label %q = %q, want %q", nsName, key, ns.Labels[key], value)
	}
}

// ensure unused imports are used — compile guard.
var _ corev1.Namespace
