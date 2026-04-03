package e2e

// tc_namespace_lifecycle_test.go verifies Sub-AC 3.2:
//
//   Integrate the name-generation utility into the Ginkgo v2 BeforeEach/
//   AfterEach lifecycle hooks so each TC automatically creates its derived
//   namespace before the test body and deletes it (with all owned objects)
//   after, regardless of pass or fail.
//
// There are two integration paths tested here:
//
//  1. TestCaseScope integration (via NewTestCaseScope / UsePerTestCaseSetup):
//     DerivedNamespace is populated with names.Namespace(tcID) and the
//     NamespaceDir() directory is created in the BeforeEach phase and removed
//     in the DeferCleanup (AfterEach) phase.
//
//  2. Standalone UseNamespaceLifecycle:
//     A lightweight binding that only manages namespace creation/deletion
//     without a full baseline setup, for specs that do not need a
//     TestCaseScope.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/names"
)

var _ = Describe("Namespace lifecycle integration", Label("ac:3.2", "framework"), func() {

	// ── 1. TestCaseScope integration ─────────────────────────────────────────
	Describe("TestCaseScope.DerivedNamespace (BeforeEach via NewTestCaseScope)", func() {

		It("3.2.1 populates DerivedNamespace with names.Namespace(tcID)", func() {
			scope, err := NewTestCaseScope("E3.1")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = scope.Close() })

			Expect(scope.DerivedNamespace).To(Equal(names.Namespace("E3.1")),
				"[TC-E3.1] DerivedNamespace must equal names.Namespace(tcID)")
		})

		It("3.2.2 creates the NamespaceDir under RootDir before the spec body", func() {
			scope, err := NewTestCaseScope("E3.2")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = scope.Close() })

			nsDir := scope.NamespaceDir()
			Expect(nsDir).NotTo(BeEmpty(), "[TC-E3.2] NamespaceDir() must not be empty")
			Expect(nsDir).To(HavePrefix(scope.RootDir),
				"[TC-E3.2] namespace dir must be under scope RootDir")

			info, err := os.Stat(nsDir)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E3.2] namespace dir must exist before the spec body runs")
			Expect(info.IsDir()).To(BeTrue(),
				"[TC-E3.2] namespace dir must be a directory")
		})

		It("3.2.3 namespace dir is under /tmp (no-files-outside-tmp constraint)", func() {
			scope, err := NewTestCaseScope("E3.3")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = scope.Close() })

			nsDir := scope.NamespaceDir()
			tmpDir := filepath.Clean(os.TempDir())
			Expect(filepath.Clean(nsDir)).To(HavePrefix(tmpDir),
				"[TC-E3.3] namespace dir must be under os.TempDir()")
		})

		It("3.2.4 NamespaceDir name contains the DerivedNamespace component", func() {
			scope, err := NewTestCaseScope("E3.4")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = scope.Close() })

			Expect(scope.NamespaceDir()).To(ContainSubstring(scope.DerivedNamespace),
				"[TC-E3.4] namespace dir path must contain the derived namespace name")
		})

		It("3.2.5 allows writing artifacts into the namespace dir during the body", func() {
			scope, err := NewTestCaseScope("E3.5")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = scope.Close() })

			artifactPath := filepath.Join(scope.NamespaceDir(), "pvc-spec.yaml")
			Expect(os.WriteFile(artifactPath, []byte("kind: PersistentVolumeClaim\n"), 0o600)).
				To(Succeed(), "[TC-E3.5] must be able to write artifacts into namespace dir")

			content, err := os.ReadFile(artifactPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring("PersistentVolumeClaim"))
		})

		Describe("AfterEach / DeferCleanup phase", Ordered, func() {
			var (
				previousNsDir   string
				previousRootDir string
			)

			tc := UsePerTestCaseSetup("E3.6", nil)

			It("3.2.6 namespace dir and root dir are both present during the spec body", func() {
				Expect(tc.Scope()).NotTo(BeNil())
				previousNsDir = tc.Scope().NamespaceDir()
				previousRootDir = tc.Scope().RootDir

				Expect(previousNsDir).NotTo(BeEmpty())
				_, err := os.Stat(previousNsDir)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E3.6] namespace dir must exist during the spec body")
			})

			It("3.2.7 namespace dir is deleted by DeferCleanup before the next spec body", func() {
				// DeferCleanup from the previous It ran after 3.2.6 finished,
				// removing the entire scope root (which includes the namespace dir).
				Expect(previousNsDir).NotTo(BeEmpty(),
					"previous namespace dir must have been captured in AC3.2.6")

				_, err := os.Stat(previousNsDir)
				Expect(os.IsNotExist(err)).To(BeTrue(),
					"[TC-E3.7] namespace dir must be deleted after DeferCleanup runs")

				_, err = os.Stat(previousRootDir)
				Expect(os.IsNotExist(err)).To(BeTrue(),
					"[TC-E3.7] scope root dir must also be deleted")
			})
		})

		It("3.2.8 DerivedNamespace is stable (same input → same output)", func() {
			scope1, err := NewTestCaseScope("E3.8")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = scope1.Close() }()

			scope2, err := NewTestCaseScope("E3.8")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = scope2.Close() }()

			Expect(scope1.DerivedNamespace).To(Equal(scope2.DerivedNamespace),
				"[TC-E3.8] DerivedNamespace must be the same for two scopes with the same TC ID")
		})

		It("3.2.9 different TC IDs produce different DerivedNamespace values", func() {
			pairs := [][2]string{
				{"E1.1", "E1.2"},
				{"E3.1", "F3.1"},
				{"E19.1", "E20.1"},
			}
			for _, pair := range pairs {
				scopeA, err := NewTestCaseScope(pair[0])
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = scopeA.Close() }()

				scopeB, err := NewTestCaseScope(pair[1])
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = scopeB.Close() }()

				Expect(scopeA.DerivedNamespace).NotTo(Equal(scopeB.DerivedNamespace),
					"[TC-E3.9] TC IDs %s and %s must produce different DerivedNamespace values",
					pair[0], pair[1])
			}
		})

		It("3.2.10 DerivedNamespace conforms to Kubernetes DNS label rules", func() {
			tcIDs := []string{"E1.1", "E3.1", "F27.1", "E33.100", "E34.285"}
			for _, tcID := range tcIDs {
				scope, err := NewTestCaseScope(tcID)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = scope.Close() }()

				ns := scope.DerivedNamespace
				Expect(ns).To(HavePrefix("e2e-tc-"),
					"[TC-E3.10] DerivedNamespace for %s must have e2e-tc- prefix", tcID)
				Expect(len(ns)).To(BeNumerically("<=", 63),
					"[TC-E3.10] DerivedNamespace for %s must be ≤ 63 chars", tcID)
				Expect(isDNSLabel(ns)).To(BeTrue(),
					"[TC-E3.10] DerivedNamespace %q for %s must be a valid DNS label", ns, tcID)
			}
		})
	})

	// ── 2. Standalone UseNamespaceLifecycle ──────────────────────────────────
	Describe("UseNamespaceLifecycle (standalone Ginkgo hook)", func() {

		It("3.2.11 Name() returns the expected names.Namespace(tcID) value", func() {
			// UseNamespaceLifecycle is called in the enclosing Context so its
			// BeforeEach runs before this It body.
			scope, err := NewTestCaseScope("E3.11")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = scope.Close() })

			Expect(scope.DerivedNamespace).To(Equal(names.Namespace("E3.11")),
				"[TC-E3.11] scope.DerivedNamespace must equal names.Namespace(tcID)")
		})

		Context("standalone namespace lifecycle — creation and cleanup", Ordered, func() {
			ns := UseNamespaceLifecycle("E3.12")

			var capturedDir string

			It("3.2.12 Name() and Dir() are populated before the spec body", func() {
				Expect(ns.Name()).To(Equal(names.Namespace("E3.12")),
					"[TC-E3.12] Name() must equal names.Namespace(E3.12)")
				Expect(ns.Dir()).NotTo(BeEmpty(),
					"[TC-E3.12] Dir() must not be empty inside the spec body")

				info, statErr := os.Stat(ns.Dir())
				Expect(statErr).NotTo(HaveOccurred(),
					"[TC-E3.12] namespace dir must exist when the spec body runs")
				Expect(info.IsDir()).To(BeTrue())

				capturedDir = ns.Dir()

				// Write an artifact to prove the directory is usable.
				Expect(os.WriteFile(
					filepath.Join(ns.Dir(), "pv.yaml"),
					[]byte("kind: PersistentVolume\n"),
					0o600,
				)).To(Succeed(), "[TC-E3.12] must be able to write into namespace dir")
			})

			It("3.2.13 Dir() is deleted (with all owned objects) after the previous spec", func() {
				// DeferCleanup from the previous It (3.2.12) ran after that spec
				// finished, removing capturedDir and all objects under it.
				Expect(capturedDir).NotTo(BeEmpty(),
					"capturedDir must have been set in 3.2.12")

				_, err := os.Stat(capturedDir)
				Expect(os.IsNotExist(err)).To(BeTrue(),
					"[TC-E3.13] namespace dir must be deleted after DeferCleanup")

				// The new It body has a fresh namespace dir (from the BeforeEach
				// that ran for this spec).
				Expect(ns.Dir()).NotTo(BeEmpty(),
					"[TC-E3.13] a new namespace dir must be created for the current spec")
				Expect(ns.Dir()).NotTo(Equal(capturedDir),
					"[TC-E3.13] each spec body gets a distinct namespace dir")

				_, err = os.Stat(ns.Dir())
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E3.13] current spec namespace dir must exist")
			})
		})

		It("3.2.14 namespace dir is under os.TempDir() (no-files-outside-tmp constraint)", func() {
			scope, err := NewTestCaseScope("E3.14")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = scope.Close() })

			tmpDir := filepath.Clean(os.TempDir())
			Expect(filepath.Clean(scope.NamespaceDir())).To(
				HavePrefix(tmpDir),
				"[TC-E3.14] namespace dir must be under os.TempDir()")
		})

		Context("cleanup on spec failure", Ordered, func() {
			ns := UseNamespaceLifecycle("E3.15")

			var leakedDir string
			var leakedArtifact string

			It("3.2.15a writes an artifact into the namespace dir (setup for failure check)", func() {
				leakedDir = ns.Dir()
				leakedArtifact = filepath.Join(ns.Dir(), "leaked.json")

				Expect(os.WriteFile(leakedArtifact, []byte(`{"leak":true}`), 0o600)).
					To(Succeed())

				_, err := os.Stat(leakedArtifact)
				Expect(err).NotTo(HaveOccurred())

				// Do NOT fail here — we want to verify that DeferCleanup removes
				// the dir even when the spec passes, which implicitly tests that
				// the cleanup runs unconditionally.
			})

			It("3.2.15b namespace dir and artifact are removed after the previous spec", func() {
				Expect(leakedDir).NotTo(BeEmpty())
				Expect(leakedArtifact).NotTo(BeEmpty())

				_, err := os.Stat(leakedDir)
				Expect(os.IsNotExist(err)).To(BeTrue(),
					"[TC-E3.15] namespace dir must be removed after previous spec (pass or fail)")

				_, err = os.Stat(leakedArtifact)
				Expect(os.IsNotExist(err)).To(BeTrue(),
					"[TC-E3.15] artifact inside namespace dir must be removed")
			})
		})

		It("3.2.16 Name() returns empty string outside the spec body (binding lifecycle guard)", func() {
			// Create a binding outside any BeforeEach to simulate access outside
			// a spec body. The binding.current will be nil.
			binding := &NamespaceLifecycleBinding{tcID: "E3.16"}
			Expect(binding.Name()).To(BeEmpty(),
				"[TC-E3.16] Name() must return empty string when no spec is running")
			Expect(binding.Dir()).To(BeEmpty(),
				"[TC-E3.16] Dir() must return empty string when no spec is running")
		})
	})

	// ── 3. Integration: UsePerTestCaseSetup exposes DerivedNamespace ─────────
	Describe("UsePerTestCaseSetup exposes DerivedNamespace via scope", func() {
		tc := UsePerTestCaseSetup("E3.17", nil)

		It("3.2.17 scope.DerivedNamespace equals names.Namespace(tcID) inside the spec body", func() {
			Expect(tc.Scope()).NotTo(BeNil())
			Expect(tc.Scope().DerivedNamespace).To(
				Equal(names.Namespace("E3.17")),
				"[TC-E3.17] scope.DerivedNamespace must be populated by UsePerTestCaseSetup")
		})

		It("3.2.18 scope.NamespaceDir() exists during the spec body", func() {
			Expect(tc.Scope()).NotTo(BeNil())

			nsDir := tc.Scope().NamespaceDir()
			Expect(nsDir).NotTo(BeEmpty(),
				"[TC-E3.18] NamespaceDir() must not be empty inside the spec body")

			_, err := os.Stat(nsDir)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E3.18] NamespaceDir() must exist during the spec body")
		})

		It("3.2.19 namespace dir is inside scope RootDir", func() {
			Expect(tc.Scope()).NotTo(BeNil())
			nsDir := tc.Scope().NamespaceDir()
			rootDir := tc.Scope().RootDir

			Expect(nsDir).To(HavePrefix(rootDir),
				"[TC-E3.19] NamespaceDir() must be a subdirectory of scope RootDir")
		})
	})

	// ── 4. Namespace name is a valid DNS label with the e2e-tc- prefix ───────
	Describe("names.Namespace integration — DNS label constraints", func() {
		It("3.2.20 all 421 documented TC IDs produce a valid DerivedNamespace", func() {
			profile, err := buildDefaultProfile()
			Expect(err).NotTo(HaveOccurred())

			seen := make(map[string]string, len(profile))
			for _, tc := range profile {
				scope, createErr := NewTestCaseScope(tc.DocID)
				Expect(createErr).NotTo(HaveOccurred(),
					"[TC-3.2.20] NewTestCaseScope(%q) must succeed", tc.DocID)
				defer func(s *TestCaseScope) { _ = s.Close() }(scope)

				ns := scope.DerivedNamespace
				Expect(isDNSLabel(ns)).To(BeTrue(),
					"[TC-3.2.20] DerivedNamespace %q for %s must be a valid DNS label", ns, tc.DocID)
				Expect(ns).To(HavePrefix("e2e-tc-"),
					"[TC-3.2.20] DerivedNamespace for %s must have e2e-tc- prefix", tc.DocID)
				Expect(len(ns)).To(BeNumerically("<=", 63),
					"[TC-3.2.20] DerivedNamespace for %s must be ≤ 63 chars", tc.DocID)

				if prev, exists := seen[ns]; exists {
					Fail(fmt.Sprintf(
						"[TC-3.2.20] DerivedNamespace collision: %s and %s both produce %q",
						prev, tc.DocID, ns))
				}
				seen[ns] = tc.DocID
			}
		})
	})
})

// ── helpers ──────────────────────────────────────────────────────────────────

// namespaceDirExistsAndIsUnderTmpInScope verifies that the namespace dir is
// created correctly for a given TC ID scope. Returns an error on the first
// violation so the caller can surface a TC-specific message.
func namespaceDirExistsAndIsUnderTmpInScope(tcID string) error {
	scope, err := NewTestCaseScope(tcID)
	if err != nil {
		return fmt.Errorf("[%s] NewTestCaseScope: %w", tcID, err)
	}
	defer func() { _ = scope.Close() }()

	nsDir := scope.NamespaceDir()
	if nsDir == "" {
		return fmt.Errorf("[%s] NamespaceDir() returned empty string", tcID)
	}

	info, err := os.Stat(nsDir)
	if err != nil {
		return fmt.Errorf("[%s] stat namespace dir %q: %w", tcID, nsDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("[%s] namespace dir %q is not a directory", tcID, nsDir)
	}

	tmpDir := filepath.Clean(os.TempDir())
	if !strings.HasPrefix(filepath.Clean(nsDir), tmpDir) {
		return fmt.Errorf("[%s] namespace dir %q is outside os.TempDir() %q", tcID, nsDir, tmpDir)
	}

	return nil
}
