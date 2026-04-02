// Package namespace provides a test fixture factory for per-test-case
// Kubernetes namespace lifecycle management.
//
// # Design
//
// Each call to New (or NewWithClient) creates a uniquely-named Kubernetes
// namespace and registers a t.Cleanup hook that deletes the namespace —
// along with all objects inside it — after the test completes, regardless
// of whether the test passed or failed.
//
// Namespace names have the format:
//
//	e2e-test-<uuid>   (e.g. "e2e-test-550e8400-e29b-41d4-a716-446655440000")
//
// The UUID suffix guarantees uniqueness across concurrent test runs without
// requiring coordination between goroutines or processes.  Unlike the
// deterministic names produced by framework/names, these names change on
// every invocation, which is intentional: the goal here is isolation, not
// repeatability.
//
// # Integration with testing.T
//
// The fixture is wired into the standard testing.TB lifecycle via t.Cleanup:
//
//	func TestMyFeature(t *testing.T) {
//	    cfg := SuiteKubeRestConfig()                         // from e2e suite
//	    ns := namespace.New(t, cfg)                          // creates + registers cleanup
//	    // ... create PVCs, Pods, CRs inside ns.Name() ...
//	    // t.Cleanup fires: namespace is deleted with all contents
//	}
//
// # Integration with Ginkgo
//
// For Ginkgo specs, use UseKubeNamespaceLifecycle which registers the same
// lifecycle via Ginkgo's BeforeEach / DeferCleanup hooks:
//
//	var _ = Describe("E30 — full provisioning", func() {
//	    ns := namespace.UseKubeNamespaceLifecycle(func() kubernetes.Interface {
//	        return suiteClient
//	    })
//	    It("[TC-E30.1] ...", func() {
//	        Expect(ns.Name()).To(HavePrefix("e2e-test-"))
//	    })
//	})
//
// # Constraints
//
//   - No files outside /tmp: the factory creates no on-disk state; all
//     ephemeral artifacts remain in-cluster.
//   - No root/sudo on host: namespace creation runs via the API server
//     inside the Kind container.
//   - Cleanup is registered synchronously so that test failures cannot
//     cause namespace leaks even when the cleanup goroutine is the last to run.
package namespace

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// THelper is a minimal subset of testing.TB that the namespace fixture needs.
// It is satisfied by *testing.T, *testing.B, and Ginkgo's GinkgoT().
//
// We use this narrower interface instead of testing.TB so the fixture compiles
// regardless of which additional methods a future Go version adds to testing.TB
// that Ginkgo may not immediately implement.
type THelper interface {
	Helper()
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
	Cleanup(f func())
}

const (
	// DefaultPrefix is the namespace name prefix used when no custom prefix is
	// specified.  The full name is: DefaultPrefix + "-" + uuid.
	DefaultPrefix = "e2e-test"

	// maxNameLen is the Kubernetes DNS-label length limit.
	maxNameLen = 63

	// createTimeout is the maximum wait for the Namespace to be created.
	createTimeout = 30 * time.Second

	// deleteTimeout is the maximum wait for the Namespace deletion call.
	deleteTimeout = 30 * time.Second
)

// Fixture manages the lifecycle of a unique Kubernetes namespace for a single
// test case.  It is created and deleted exclusively via the Kubernetes API;
// no on-disk state is produced.
type Fixture struct {
	// name is the immutable namespace name assigned at construction time.
	name string

	// clientset is retained so that Ginkgo lifecycle helpers can defer cleanup
	// without capturing a closure over an external variable.
	clientset kubernetes.Interface
}

// Name returns the unique Kubernetes namespace name assigned to this fixture.
// Safe to call from any goroutine after New / NewWithClient returns.
// Returns an empty string if the receiver is nil.
func (f *Fixture) Name() string {
	if f == nil {
		return ""
	}
	return f.name
}

// New creates a NamespaceFixture backed by the cluster reachable via cfg.
// It builds a kubernetes.Clientset from cfg, then delegates to NewWithClient.
//
// The namespace name is: DefaultPrefix + "-" + uuid (e.g. "e2e-test-<uuid>").
// Cleanup is registered via t.Cleanup — the namespace is deleted after the
// test body returns regardless of pass or fail.
func New(t *testing.T, cfg *rest.Config) *Fixture {
	t.Helper()
	return NewWithPrefix(t, cfg, DefaultPrefix)
}

// NewWithPrefix creates a NamespaceFixture with a custom name prefix.
// The resulting namespace name is: <prefix>-<uuid>, truncated to 63 characters.
//
// Use this when the default "e2e-test" prefix is not descriptive enough for
// the test (e.g. "e2e-pvc", "e2e-zfs-provision").
func NewWithPrefix(t *testing.T, cfg *rest.Config, prefix string) *Fixture {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("[namespace.NewWithPrefix] create kubernetes clientset: %v", err)
	}
	return NewWithClient(t, clientset, prefix)
}

// NewWithClient creates a NamespaceFixture using an already-constructed
// kubernetes.Interface.  This is the preferred entry point for tests that
// already hold a clientset (e.g. from SuiteKubeRestConfig) and for unit tests
// that inject a fake client.
//
// Lifecycle:
//  1. Generates a unique namespace name: <prefix>-<uuid>.
//  2. Creates the Kubernetes Namespace via the API (immediate, synchronous).
//  3. Registers t.Cleanup to delete the namespace after the test returns.
//
// The cleanup deletes the namespace with propagation policy Background so that
// Kubernetes garbage-collects all owned objects asynchronously.  If the
// namespace is already gone when cleanup runs (e.g. because the test deleted it
// explicitly), the 404 is silently ignored.
func NewWithClient(t THelper, clientset kubernetes.Interface, prefix string) *Fixture {
	t.Helper()

	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultPrefix
	}

	// Generate a UUID-based namespace name.  UUIDs contain only lowercase
	// hex digits and hyphens, which are valid in Kubernetes namespace names.
	id := uuid.New().String()
	name := buildNamespaceName(prefix, id)

	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				// These labels make it easy to identify and bulk-delete
				// stale namespaces left over from interrupted test runs.
				"pillar-csi.bhyoo.com/managed-by":   "e2e-fixture",
				"pillar-csi.bhyoo.com/fixture-type": "namespace",
			},
		},
	}

	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("[namespace fixture] create namespace %q: %v", name, err)
	}

	fixture := &Fixture{
		name:      name,
		clientset: clientset,
	}

	// Register cleanup: delete the namespace (and all its contents) after the
	// test returns.  t.Cleanup runs in LIFO order, so if the test created other
	// resources with their own t.Cleanup hooks, those hooks run first.
	t.Cleanup(func() {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer deleteCancel()

		// Background propagation: Kubernetes will asynchronously delete all
		// owned objects (Pods, PVCs, Secrets …) after the namespace is gone.
		propagation := metav1.DeletePropagationBackground
		delErr := clientset.CoreV1().Namespaces().Delete(
			deleteCtx,
			name,
			metav1.DeleteOptions{PropagationPolicy: &propagation},
		)
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			// Log but do not call t.Fatal/t.Error — cleanup failures must not
			// mask the actual test result reported by the test body.
			t.Logf("[namespace fixture] cleanup: delete namespace %q: %v", name, delErr)
		}
	})

	return fixture
}

// buildNamespaceName produces a valid Kubernetes namespace name from a prefix
// and a UUID string.  The result is: <prefix>-<id>, truncated to maxNameLen (63)
// characters.  If truncation is needed, the UUID suffix is always preserved in
// full and the prefix is shortened.
func buildNamespaceName(prefix, id string) string {
	// Happy path: the concatenated name fits within the 63-char limit.
	// "e2e-test-" (9 chars) + UUID (36 chars) = 45 chars — always fits.
	full := prefix + "-" + id
	if len(full) <= maxNameLen {
		return full
	}

	// Truncate the prefix, keeping "-<id>" intact.
	// available = 63 - 1 ("-") - len(id)
	available := maxNameLen - 1 - len(id)
	if available <= 0 {
		// Extremely unlikely: id alone fills the budget.  Return truncated id.
		return id[:maxNameLen]
	}
	truncatedPrefix := strings.TrimRight(prefix[:available], "-")
	if truncatedPrefix == "" {
		return fmt.Sprintf("x-%s", id)[:maxNameLen]
	}
	return truncatedPrefix + "-" + id
}
