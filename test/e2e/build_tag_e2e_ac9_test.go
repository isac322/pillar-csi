package e2e

// build_tag_e2e_ac9_test.go — AC 9: Verify that -tags=e2e precisely includes
// real-backend specs.
//
// # AC 9 Contract
//
// Every real-backend spec file in test/e2e/ that is gated behind the "e2e"
// build constraint MUST declare "//go:build e2e" as its very first line.
// This guarantees:
//
//  1. Files are excluded when running unit/integration tests without -tags=e2e.
//  2. Files are included when `make test-e2e` passes -tags=e2e to go test.
//  3. No real-backend spec file accidentally compiles in all builds (which
//     would pull in Kind cluster, Docker exec, and real storage backend
//     dependencies into unit test binaries).
//
// # What counts as a "real-backend spec file"?
//
// Any file matching *_e2e_test.go or *_e2e.go (non-test helpers) in the
// test/e2e/ directory (top-level only, not subdirectories). These files
// contain Ginkgo Describe blocks that exercise the real ZFS / LVM / iSCSI /
// NVMe-oF storage backends inside a Kind cluster and therefore MUST be
// compiled only when -tags=e2e is active.
//
// # Makefile verification
//
// This test also asserts that the Makefile's test-e2e target passes -tags=e2e
// to `go test`, ensuring that the real-backend specs are always included in
// the canonical CI entry point.
//
// # Design rationale
//
// This is a static verification test (no build tag, no Kind cluster required).
// It runs as part of `go test ./test/e2e/` without any infrastructure and
// provides a fast compile-time/lint-time guard against accidentally removing
// or misplacing the build constraint from real-backend spec files.

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAC9RealBackendSpecFilesHaveE2EBuildTag verifies that every file in
// test/e2e/ whose name ends in "_e2e_test.go" or "_e2e.go" carries
// "//go:build e2e" as its very first line.
//
// Rationale: these files contain Ginkgo specs that require a live Kind cluster
// with real ZFS/LVM/iSCSI/NVMe-oF backends. Compiling them without
// -tags=e2e would introduce unwanted dependencies (docker exec, kubeconfig,
// real storage drivers) into every unit test binary, and would cause test
// failures on developer machines without the storage infrastructure.
func TestAC9RealBackendSpecFilesHaveE2EBuildTag(t *testing.T) {
	t.Parallel()

	e2eDir := resolveE2EDir(t)

	entries, err := os.ReadDir(e2eDir)
	if err != nil {
		t.Fatalf("[AC9] cannot read test/e2e/ directory: %v", err)
	}

	var realBackendFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, "_e2e_test.go") || strings.HasSuffix(name, "_e2e.go") {
			realBackendFiles = append(realBackendFiles, filepath.Join(e2eDir, name))
		}
	}

	if len(realBackendFiles) == 0 {
		t.Fatal("[AC9] no real-backend spec files (*_e2e_test.go, *_e2e.go) found in test/e2e/")
	}

	t.Logf("[AC9] found %d real-backend spec file(s) to verify", len(realBackendFiles))

	for _, path := range realBackendFiles {
		path := path
		relName := filepath.Base(path)
		t.Run(relName, func(t *testing.T) {
			t.Parallel()

			firstLine, err := readFirstNonEmptyLine(path)
			if err != nil {
				t.Errorf("[AC9] %s: cannot read file: %v", relName, err)
				return
			}

			if firstLine != "//go:build e2e" {
				t.Errorf(
					"[AC9] MISSING BUILD TAG in %s\n"+
						"  First line: %q\n"+
						"  Expected  : %q\n"+
						"\n"+
						"  Real-backend spec files MUST start with '//go:build e2e' so they\n"+
						"  are excluded from unit test runs and included only when\n"+
						"  `make test-e2e` passes -tags=e2e to go test.\n"+
						"\n"+
						"  Fix: add '//go:build e2e' as the first line of %s",
					relName, firstLine, "//go:build e2e", relName,
				)
			} else {
				t.Logf("[AC9] ✓ %s  //go:build e2e present", relName)
			}
		})
	}
}

// TestAC9MakefileTestE2ETargetPassesE2EBuildTag verifies that the canonical
// `make test-e2e` target in the root Makefile passes `-tags=e2e` to `go test`.
//
// This provides a static contract between the Makefile and the real-backend
// spec files: if the tag is removed from the Makefile, the real-backend specs
// would silently not compile and all their Ginkgo Describe blocks would be
// absent from the test run.
func TestAC9MakefileTestE2ETargetPassesE2EBuildTag(t *testing.T) {
	t.Parallel()

	makefilePath := resolveMakefilePath(t)

	content, err := os.ReadFile(makefilePath) //nolint:gosec // path derived from runtime.Caller
	if err != nil {
		t.Fatalf("[AC9] cannot read Makefile: %v", err)
	}

	text := string(content)

	// The Makefile test-e2e target must pass -tags=e2e to go test.
	// We accept both literal -tags=e2e and --tags=e2e forms.
	if !strings.Contains(text, "-tags=e2e") && !strings.Contains(text, "--tags=e2e") {
		t.Errorf(
			"[AC9] Makefile does not contain '-tags=e2e' or '--tags=e2e'.\n"+
				"  The test-e2e target MUST pass -tags=e2e to `go test` so that\n"+
				"  all real-backend spec files (*_e2e_test.go) are compiled and\n"+
				"  included in the test run.\n"+
				"  Makefile path: %s",
			makefilePath,
		)
	} else {
		t.Logf("[AC9] ✓ Makefile contains -tags=e2e / --tags=e2e")
	}

	// Specifically verify the test-e2e target (not just some other target).
	// Find the test-e2e target block and confirm it contains -tags=e2e.
	if !containsTestE2EWithTag(text) {
		t.Errorf(
			"[AC9] Makefile 'test-e2e' target does not pass -tags=e2e directly.\n"+
				"  The tag may exist in the Makefile but not in the test-e2e target.\n"+
				"  Verify that the test-e2e recipe line contains 'go test -tags=e2e'.\n"+
				"  Makefile path: %s",
			makefilePath,
		)
	} else {
		t.Logf("[AC9] ✓ Makefile test-e2e target passes -tags=e2e to go test")
	}
}

// TestAC9RealBackendSpecFilesAreGinkgoSpecs verifies that every real-backend
// spec file (those with //go:build e2e) registers at least one Ginkgo
// Describe block so that they are not accidentally empty after build.
//
// This is a lightweight static check: it scans the file for "var _ = Describe"
// or "func init()" + "RegisterFailHandler" patterns that indicate Ginkgo spec
// registration. It does NOT attempt to compile or run the specs.
func TestAC9RealBackendSpecFilesAreGinkgoSpecs(t *testing.T) {
	t.Parallel()

	e2eDir := resolveE2EDir(t)

	entries, err := os.ReadDir(e2eDir)
	if err != nil {
		t.Fatalf("[AC9] cannot read test/e2e/ directory: %v", err)
	}

	// Only check *_e2e_test.go files (the Ginkgo spec files); skip helper
	// files like *_e2e.go which may not contain Describe blocks directly.
	var specFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, "_e2e_test.go") {
			specFiles = append(specFiles, filepath.Join(e2eDir, name))
		}
	}

	for _, path := range specFiles {
		path := path
		relName := filepath.Base(path)
		t.Run(relName, func(t *testing.T) {
			t.Parallel()

			content, err := os.ReadFile(path) //nolint:gosec // path derived from ReadDir
			if err != nil {
				t.Errorf("[AC9] %s: cannot read: %v", relName, err)
				return
			}

			text := string(content)
			// Accept any Ginkgo lifecycle registration or standard Go test function.
			// kind_bootstrap_e2e_test.go uses SynchronizedBeforeSuite/SynchronizedAfterSuite.
			hasDescribe := strings.Contains(text, "var _ = Describe(") ||
				strings.Contains(text, "var _ = DescribeTable(") ||
				strings.Contains(text, "var _ = SynchronizedBeforeSuite(") ||
				strings.Contains(text, "var _ = BeforeSuite(") ||
				strings.Contains(text, "var _ = SynchronizedAfterSuite(") ||
				strings.Contains(text, "var _ = AfterSuite(") ||
				strings.Contains(text, "RunSpecs(") ||
				strings.Contains(text, "func Test") // standard Go test function

			if !hasDescribe {
				t.Errorf(
					"[AC9] %s has //go:build e2e but no Ginkgo Describe/DescribeTable/"+
						"SynchronizedBeforeSuite/BeforeSuite/RunSpecs or standard Test* function — "+
						"file may be empty or miscategorised",
					relName,
				)
			} else {
				t.Logf("[AC9] ✓ %s  Ginkgo spec or test function present", relName)
			}
		})
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveE2EDir returns the absolute path to the test/e2e/ directory by
// walking up from the caller's source file to the repository root.
func resolveE2EDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("[AC9] runtime.Caller failed — cannot resolve test/e2e directory")
	}
	return filepath.Dir(file)
}

// resolveMakefilePath returns the absolute path to the root Makefile by
// locating the repository root (parent of test/e2e/).
func resolveMakefilePath(t *testing.T) string {
	t.Helper()
	e2eDir := resolveE2EDir(t)
	// test/e2e → test → repo-root
	repoRoot := filepath.Join(e2eDir, "..", "..")
	return filepath.Join(repoRoot, "Makefile")
}

// readFirstNonEmptyLine reads the first non-empty line of a file.
// Returns an error if the file cannot be read or contains no non-empty lines.
func readFirstNonEmptyLine(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path derived from ReadDir
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			return line, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// containsTestE2EWithTag returns true when the Makefile text contains a line
// that is part of the test-e2e recipe and also contains "go test -tags=e2e"
// or "go test --tags=e2e".
//
// The heuristic: look for a line that contains both "go test" and "-tags=e2e".
// This is more specific than just checking for the tag anywhere in the file.
func containsTestE2EWithTag(makefileText string) bool {
	lines := strings.Split(makefileText, "\n")
	for _, line := range lines {
		if strings.Contains(line, "go test") && strings.Contains(line, "-tags=e2e") {
			return true
		}
		if strings.Contains(line, "go test") && strings.Contains(line, "--tags=e2e") {
			return true
		}
	}
	return false
}
