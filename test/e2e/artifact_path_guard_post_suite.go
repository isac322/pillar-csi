package e2e

// artifact_path_guard_post_suite.go — Sub-AC 7c: post-test /tmp boundary scan.
//
// After all specs complete, the ReportAfterSuite hook registered here walks
// the repository root (the working directory when `go test ./test/e2e/...`
// runs) and fails the suite if any regular files were created or modified
// during the test run outside the /tmp boundary.
//
// The hook complements the pre-write assertPathUnderTempRoot guard defined in
// artifact_path_guard.go: the pre-write guard panics the moment a bad path is
// constructed; this post-test scan catches violations that slip through via
// code paths that do not call the guard (e.g. direct os.Create calls or
// third-party helpers that bypass the guard).
//
// Scanning strategy
//
//	Start time: artifactGuardBaseline is set at package-init time — before
//	TestMain, before BeforeSuite, before any spec — so any file written to
//	the repository tree during the run has ModTime strictly after this value.
//
//	Root: the current working directory captured at init time (normally the
//	repository root when `go test ./test/e2e/...` is invoked).
//
//	Skipped: only the .git/ subtree is excluded (git itself updates index
//	files during normal operation). All other top-level directories are
//	scanned so that accidental writes to docs/, hack/, api/, etc. are caught.
//
//	False-positive avoidance: pre-existing files have a ModTime before the
//	baseline because the binary is compiled before package init runs. Build
//	outputs that land in CWD would also appear here and are intentional
//	violations — all build artefacts must be produced before TestMain.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

// artifactGuardBaseline is captured at package-init time — before TestMain,
// before any BeforeSuite node, before any spec — so that any file written to
// the repository tree during the test run has a ModTime strictly after this
// value.
var artifactGuardBaseline = time.Now()

// artifactGuardCWD is the current working directory captured at init time.
// When `go test ./test/e2e/...` runs, the working directory is the repository
// root. Any test artifact found under this tree (outside /tmp) is a violation.
var artifactGuardCWD = func() string {
	cwd, _ := os.Getwd()
	return cwd
}()

// artifactPathGuardPostSuiteHook is the ReportAfterSuite hook that scans the
// repository working directory for files created or modified during the test
// run. It fails the suite with a descriptive message when violations are found.
//
// This runs on the primary Ginkgo process after all parallel workers have
// finished and the consolidated report is available, so it reflects the
// outcome of every spec regardless of how many parallel workers were used.
var _ = ReportAfterSuite("artifact path guard: /tmp boundary scan", func(_ types.Report) {
	violations := cwdArtifactsCreatedDuringRun(artifactGuardCWD, artifactGuardBaseline)
	if len(violations) == 0 {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb,
		"artifact path guard [Sub-AC 7c]: %d file(s) written outside %s during the test run:\n",
		len(violations), artifactTempRoot)
	for _, v := range violations {
		fmt.Fprintf(&sb, "  %s\n", v)
	}
	fmt.Fprintf(&sb,
		"\nAll e2e artifacts must be written under %s (tcTempRoot).\n", artifactTempRoot)
	fmt.Fprintf(&sb,
		"Use scope.Path(...) or scope.TempDir(...) instead of bare os.Create / os.MkdirTemp(\"\", ...).")
	Fail(sb.String())
})

// cwdArtifactsCreatedDuringRun walks root and returns the absolute paths of
// regular files whose modification time is strictly after baseline.
//
// Only the .git/ subtree is excluded because git itself updates internal
// bookkeeping files (index locks, COMMIT_EDITMSG, etc.) during normal
// operations that may overlap with a test run. All other paths are scanned.
//
// On any filesystem error (permission denied, dangling symlink, etc.) the
// affected path is silently skipped so that a single unreadable entry cannot
// suppress violations elsewhere in the tree.
func cwdArtifactsCreatedDuringRun(root string, baseline time.Time) []string {
	if root == "" {
		return nil
	}

	var violations []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip unreadable paths rather than aborting the entire scan.
			return nil //nolint:nilerr
		}

		// Skip the root directory entry itself.
		if path == root {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil //nolint:nilerr
		}

		// Determine the top-level component so we can skip the .git subtree.
		topDir := rel
		if idx := strings.IndexByte(rel, filepath.Separator); idx != -1 {
			topDir = rel[:idx]
		}

		if info.IsDir() {
			if topDir == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		// Flag regular files created or modified after the test-run baseline.
		if info.ModTime().After(baseline) {
			violations = append(violations, path)
		}
		return nil
	})
	return violations
}
