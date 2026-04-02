package e2e

// tc_namespace_lifecycle.go provides explicit Ginkgo v2 BeforeEach and
// DeferCleanup (AfterEach-equivalent) hooks that integrate the
// framework/names name-generation utility into the per-TC test lifecycle.
//
// # Why this exists
//
// The framework/names package (names.Namespace, names.ObjectPrefix,
// names.ResourceName) generates deterministic, DNS-label-safe Kubernetes
// resource names from TC IDs. For these names to be useful they must be
// bound to the spec body in a way that:
//
//  1. Creates the namespace isolation domain *before* the It() body runs
//     (BeforeEach phase).
//  2. Tears it down *after* the It() body finishes — regardless of whether
//     the spec passes or fails (DeferCleanup / AfterEach phase).
//
// NewTestCaseScope already integrates names.Namespace() into the BeforeEach
// phase when a full scope is needed (UsePerTestCaseSetup). UseNamespaceLifecycle
// provides the same guarantee for specs that only need the derived namespace
// name and its filesystem isolation directory, without requiring a full
// TestCaseBaseline setup.
//
// # Ginkgo tree positioning
//
// Call UseNamespaceLifecycle at the same Ginkgo tree level as the It() nodes
// that need it. Each call registers one BeforeEach + one DeferCleanup that
// are scoped to the enclosing Describe/Context block, exactly like
// UsePerTestCaseSetup.
//
// Example:
//
//	Describe("E3 — CreateVolume", Label("default-profile"), func() {
//	    Context("E3.1 — basic create", func() {
//	        ns := UseNamespaceLifecycle("E3.1")
//
//	        It("[TC-E3.1] ...", func() {
//	            // ns.Name() == "e2e-tc-e3-1-<hash8>"  (stable across runs)
//	            // ns.Dir()  == /tmp/pillar-csi-<…>/namespaces/e2e-tc-e3-1-<hash8>
//	            Expect(ns.Name()).To(HavePrefix("e2e-tc-"))
//	        })
//	    })
//	})

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/names"
)

// NamespaceLifecycleBinding holds the per-It derived namespace state that is
// populated by UseNamespaceLifecycle before the spec body runs and cleared
// after it finishes.
type NamespaceLifecycleBinding struct {
	tcID    string
	current *derivedNamespaceState
}

// derivedNamespaceState holds the namespace name and filesystem root created
// for a single It() invocation.
type derivedNamespaceState struct {
	// Name is the deterministic Kubernetes-safe namespace name returned by
	// names.Namespace(tcID). It is stable across test runs for the same TC ID.
	Name string

	// Dir is the filesystem isolation directory created under /tmp before the
	// spec body runs. All TC-scoped in-process artifacts should be written here.
	// It is removed (along with all owned objects) by DeferCleanup.
	Dir string
}

// Name returns the deterministic Kubernetes-safe namespace name for the
// running It() body. Empty string is returned outside a spec body.
func (b *NamespaceLifecycleBinding) Name() string {
	if b == nil || b.current == nil {
		return ""
	}
	return b.current.Name
}

// Dir returns the filesystem isolation directory for the derived namespace in
// the running It() body. Empty string is returned outside a spec body.
//
// The directory is guaranteed to exist when the It() body starts and to be
// deleted (with all owned objects) after the body finishes.
func (b *NamespaceLifecycleBinding) Dir() string {
	if b == nil || b.current == nil {
		return ""
	}
	return b.current.Dir
}

// UseNamespaceLifecycle registers Ginkgo v2 BeforeEach and DeferCleanup hooks
// that integrate the names.Namespace name-generation utility into the per-TC
// test lifecycle.
//
// BeforeEach phase (runs before each It() body in the enclosing block):
//
//  1. Calls names.Namespace(tcID) to obtain the deterministic namespace name.
//  2. Creates a private filesystem isolation directory rooted under /tmp
//     at a path that embeds both the TC slug and the derived namespace name:
//     /tmp/pillar-csi-ns-<tc-slug>-<pid>-<random>/<derived-namespace>/
//  3. Populates the binding so Name() and Dir() are available to the spec body.
//
// DeferCleanup phase (runs after each It() body, regardless of pass or fail):
//
//  1. Removes the entire namespace isolation directory and all objects under it
//     (equivalent to "kubectl delete namespace <ns> --all").
//  2. Resets the binding so Name() and Dir() return "" for the next spec.
//
// The binding is safe to capture outside the It() body (e.g. in the enclosing
// Context or Describe) and read from within It(). Writes to Name() / Dir()
// outside the spec body will observe an empty string.
func UseNamespaceLifecycle(tcID string) *NamespaceLifecycleBinding {
	binding := &NamespaceLifecycleBinding{tcID: tcID}

	BeforeEach(func() {
		GinkgoHelper()

		nsName := names.Namespace(tcID)

		// Create a TC-private parent directory so that concurrent runs of the
		// same TC ID (e.g. parallel Ginkgo workers, -count=2) never share the
		// same namespace directory.  The parent is named after the TC slug and
		// the current process PID; os.MkdirTemp appends a random suffix to
		// ensure uniqueness even within the same process.
		parentPattern := fmt.Sprintf("pillar-csi-ns-%s-", dnsLabelToken(tcID))
		parent, err := os.MkdirTemp(os.TempDir(), parentPattern)
		Expect(err).NotTo(HaveOccurred(),
			"[%s] UseNamespaceLifecycle: create namespace parent dir", tcID)

		// Verify that the parent dir is under /tmp (os.TempDir) so that the
		// no-files-outside-tmp constraint is satisfied.
		if !isUnderTempDir(parent) {
			// Clean up and fail immediately; os.RemoveAll on a fresh empty dir
			// is safe regardless of Remove errors.
			_ = os.RemoveAll(parent)
			Fail(fmt.Sprintf(
				"[%s] UseNamespaceLifecycle: parent dir %q is outside os.TempDir() %q",
				tcID, parent, os.TempDir()))
		}

		nsDir := filepath.Join(parent, nsName)
		Expect(os.MkdirAll(nsDir, 0o755)).To(Succeed(),
			"[%s] UseNamespaceLifecycle: create namespace dir %s", tcID, nsDir)

		state := &derivedNamespaceState{
			Name: nsName,
			Dir:  nsDir,
		}
		binding.current = state

		// DeferCleanup runs after the It() body regardless of pass or fail,
		// mirroring Kubernetes namespace deletion semantics: the namespace and
		// all objects it contains are removed atomically.
		DeferCleanup(func() {
			GinkgoHelper()

			binding.current = nil

			// Remove namespace dir and all owned objects.  Using the parent
			// here (not nsDir) so that even artifacts accidentally written
			// adjacent to nsDir inside parent are swept up.
			if removeErr := os.RemoveAll(parent); removeErr != nil {
				// Log but do not fail the spec — teardown errors are reported
				// separately so the spec failure message stays focused on the
				// assertion that actually failed.
				_, _ = fmt.Fprintf(
					GinkgoWriter,
					"[%s] UseNamespaceLifecycle: teardown error: %v\n",
					tcID, removeErr,
				)
			}
		})
	})

	return binding
}

// isUnderTempDir returns true when path is a child of os.TempDir().
func isUnderTempDir(path string) bool {
	tmpDir := filepath.Clean(os.TempDir())
	cleanPath := filepath.Clean(path)
	return strings.HasPrefix(cleanPath, tmpDir+string(filepath.Separator)) ||
		cleanPath == tmpDir
}
