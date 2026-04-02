package namespace

// ginkgo.go provides a Ginkgo v2 integration layer for the namespace fixture
// factory.  It mirrors the UseNamespaceLifecycle pattern already established
// in tc_namespace_lifecycle.go but operates against a real Kubernetes cluster
// instead of the local filesystem.
//
// Usage:
//
//	var _ = Describe("E30 — full provisioning", func() {
//	    var suiteClient kubernetes.Interface
//
//	    BeforeSuite(func() {
//	        suiteClient = kubernetes.NewForConfigOrDie(SuiteKubeRestConfig())
//	    })
//
//	    ns := namespace.UseKubeNamespaceLifecycle(func() kubernetes.Interface {
//	        return suiteClient
//	    })
//
//	    It("[TC-E30.1] creates PVC inside namespace", func() {
//	        Expect(ns.Name()).To(HavePrefix("e2e-test-"))
//	        // create PVC in ns.Name() ...
//	    })
//	})

import (
	. "github.com/onsi/ginkgo/v2"
	"k8s.io/client-go/kubernetes"
)

// GinkgoBinding holds the per-It namespace fixture populated by
// UseKubeNamespaceLifecycle's BeforeEach hook.
type GinkgoBinding struct {
	clientFn func() kubernetes.Interface
	prefix   string
	current  *Fixture
}

// Name returns the unique Kubernetes namespace name for the running It() body.
// Returns an empty string outside a spec body (when the BeforeEach has not yet
// run or after DeferCleanup has fired).
func (b *GinkgoBinding) Name() string {
	if b == nil || b.current == nil {
		return ""
	}
	return b.current.Name()
}

// UseKubeNamespaceLifecycle registers Ginkgo v2 BeforeEach and DeferCleanup
// hooks that create a unique Kubernetes namespace before each It() body and
// delete it afterwards, regardless of pass or fail.
//
// clientFn is called during BeforeEach to obtain the Kubernetes client.  It
// is a function (not a value) so callers can initialise the client in a
// BeforeSuite or outer BeforeEach without worrying about initialization order.
//
// The returned *GinkgoBinding can be captured outside the It() body and its
// Name() method read from within the spec body.
//
// Example:
//
//	ns := namespace.UseKubeNamespaceLifecycle(func() kubernetes.Interface {
//	    return suiteK8sClient
//	})
//
//	It("[TC-E30.1] ...", func() {
//	    Expect(ns.Name()).To(HavePrefix("e2e-test-"))
//	})
func UseKubeNamespaceLifecycle(clientFn func() kubernetes.Interface) *GinkgoBinding {
	return UseKubeNamespaceLifecycleWithPrefix(clientFn, DefaultPrefix)
}

// UseKubeNamespaceLifecycleWithPrefix is like UseKubeNamespaceLifecycle but
// lets callers choose a custom namespace name prefix (e.g. "e2e-pvc").
func UseKubeNamespaceLifecycleWithPrefix(clientFn func() kubernetes.Interface, prefix string) *GinkgoBinding {
	binding := &GinkgoBinding{
		clientFn: clientFn,
		prefix:   prefix,
	}

	BeforeEach(func() {
		GinkgoHelper()

		client := clientFn()
		if client == nil {
			Fail("[namespace.UseKubeNamespaceLifecycle] clientFn returned nil kubernetes.Interface — " +
				"ensure the client is initialised in BeforeSuite or an outer BeforeEach")
		}

		// Create the namespace using the standard testing.TB adapter so
		// the same NewWithClient logic applies.
		f := NewWithClient(GinkgoT(), client, binding.prefix)
		binding.current = f

		DeferCleanup(func() {
			// The namespace was already registered for deletion via
			// GinkgoT().Cleanup inside NewWithClient.  We nil out the binding
			// here so Name() returns "" for the next spec (if any).
			binding.current = nil
		})
	})

	return binding
}
