package e2e

// isolation_check_integration_test.go — Ginkgo v2 integration tests for the
// framework isolation checker.
//
// These specs validate:
//  1. NewTestCaseScope registers the scope with the isolation checker.
//  2. scope.Close() deregisters the scope.
//  3. After Close(), the root dir is gone from the registry AND from disk.
//  4. ScanOrphanedTempDirs does not flag live scopes as orphaned.
//  5. InstallAfterEachIsolationCheck (the AfterEach hook) passes when all
//     scopes are properly cleaned up.
//
// Note: these are unit/integration tests for the framework itself, not for
// any specific TC behaviour.  They run in the test binary without a Kind
// cluster and are labelled "ac:3.4" and "framework" for filtering.

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

var _ = Describe("Isolation check framework", Label("ac:3.4", "framework"), func() {

	// The outer Describe installs the hook once; all child specs run under it.
	// This validates that the hook does NOT incorrectly fail specs that clean
	// up properly.
	framework.InstallAfterEachIsolationCheck()

	Describe("scope registry lifecycle", func() {

		It("3.4.1 NewTestCaseScope registers the scope with the isolation checker", func() {
			scope, err := NewTestCaseScope("iso-check-3.4.1")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = scope.Close()
			})

			Expect(framework.IsScopeActive(scope.RootDir)).To(BeTrue(),
				"scope.RootDir %q should be registered as active immediately after NewTestCaseScope",
				scope.RootDir)

			_, statErr := os.Stat(scope.RootDir)
			Expect(statErr).NotTo(HaveOccurred(),
				"scope.RootDir %q should exist on disk", scope.RootDir)
		})

		It("3.4.2 scope.Close() deregisters the scope and removes the root dir", func() {
			scope, err := NewTestCaseScope("iso-check-3.4.2")
			Expect(err).NotTo(HaveOccurred())
			rootDir := scope.RootDir

			// Verify it's active before Close.
			Expect(framework.IsScopeActive(rootDir)).To(BeTrue())

			// Close synchronously.
			Expect(scope.Close()).To(Succeed())

			// After Close: deregistered AND dir gone.
			Expect(framework.IsScopeActive(rootDir)).To(BeFalse(),
				"scope.RootDir %q should be deregistered after Close()", rootDir)

			_, statErr := os.Stat(rootDir)
			Expect(os.IsNotExist(statErr)).To(BeTrue(),
				"scope.RootDir %q should be removed from disk after Close()", rootDir)
		})

		It("3.4.3 ScanOrphanedTempDirs does not flag an active scope's dir", func() {
			scope, err := NewTestCaseScope("iso-check-3.4.3")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = scope.Close()
			})

			violations, scanErr := framework.ScanOrphanedTempDirs()
			Expect(scanErr).NotTo(HaveOccurred())

			for _, v := range violations {
				Expect(v.RootDir).NotTo(Equal(scope.RootDir),
					"active scope dir %q must NOT appear in orphan scan results", scope.RootDir)
			}
		})

		It("3.4.4 two concurrent scopes both registered; neither flagged as orphan", func() {
			left, errL := NewTestCaseScope("iso-check-3.4.4-left")
			Expect(errL).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = left.Close() })

			right, errR := NewTestCaseScope("iso-check-3.4.4-right")
			Expect(errR).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = right.Close() })

			violations, scanErr := framework.ScanOrphanedTempDirs()
			Expect(scanErr).NotTo(HaveOccurred())

			for _, v := range violations {
				Expect(v.RootDir).NotTo(Equal(left.RootDir),
					"left scope dir should not be orphaned")
				Expect(v.RootDir).NotTo(Equal(right.RootDir),
					"right scope dir should not be orphaned")
			}
		})

		It("3.4.5 a deregistered dir that still exists IS detected as orphan", func() {
			scope, err := NewTestCaseScope("iso-check-3.4.5")
			Expect(err).NotTo(HaveOccurred())
			rootDir := scope.RootDir

			// Deregister WITHOUT removing the dir (simulating failed cleanup).
			framework.DeregisterActiveScope(rootDir)
			DeferCleanup(func() {
				// Ensure cleanup on test exit even if dir was not removed.
				_ = os.RemoveAll(rootDir)
			})

			// Dir still exists but is no longer registered → orphan.
			violations, scanErr := framework.ScanOrphanedTempDirs()
			Expect(scanErr).NotTo(HaveOccurred())

			var found bool
			for _, v := range violations {
				if v.RootDir == rootDir {
					found = true
					Expect(v.Kind).To(Equal(framework.ViolationOrphanedTempDir),
						"violation kind should be ViolationOrphanedTempDir")
					break
				}
			}
			Expect(found).To(BeTrue(),
				"deregistered dir %q should be detected as orphaned; violations=%v",
				rootDir, violations)

			// Re-register so that the outer AfterEach doesn't also flag it.
			framework.RegisterActiveScope(rootDir, scope.TCID, scope.ScopeTag)
			// Close will deregister and remove the dir cleanly.
			Expect(scope.Close()).To(Succeed())
		})
	})
})
