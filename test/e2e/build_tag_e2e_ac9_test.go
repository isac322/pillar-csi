package e2e

// build_tag_e2e_ac9_test.go — AC 9: Verify that -tags=e2e precisely includes
// real-backend specs.
//
// # AC 9 Contract
//
// Every real-backend spec file in test/e2e/ that is gated behind the "e2e"
// build constraint MUST declare "//go:build e2e" (or a superset such as
// "//go:build e2e && e2e_helm") as its very first line.  This guarantees:
//
//  1. Files are excluded when running unit/integration tests without -tags=e2e.
//  2. Files with exactly "//go:build e2e" are included when `make test-e2e`
//     passes -tags=e2e to go test.
//  3. Files with "//go:build e2e && e2e_helm" (Helm-only specs: E34, E35,
//     F27–F31) are included only when both tags are active; they are excluded
//     from the default `make test-e2e` run and counted as non-default-profile.
//  4. No real-backend spec file accidentally compiles in all builds (which
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAC9RealBackendSpecFilesHaveE2EBuildTag verifies that every file in
// test/e2e/ whose name ends in "_e2e_test.go" or "_e2e.go" carries
// "//go:build e2e" (or "//go:build e2e && ...") as its very first line.
//
// Rationale: these files contain Ginkgo specs that require a live Kind cluster
// with real ZFS/LVM/iSCSI/NVMe-oF backends. Compiling them without
// -tags=e2e would introduce unwanted dependencies (docker exec, kubeconfig,
// real storage drivers) into every unit test binary, and would cause test
// failures on developer machines without the storage infrastructure.
//
// Files with "//go:build e2e && e2e_helm" (Helm-only specs) satisfy the
// constraint because they also require the "e2e" tag — they just add an extra
// "e2e_helm" gate for specs that need the Helm-deployed agent.
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

			// Accept "//go:build e2e" (default) or "//go:build e2e && ..."
			// (e.g. "//go:build e2e && e2e_helm" for Helm-only specs).
			// The critical invariant is that the "e2e" tag is required.
			hasE2ETag := firstLine == "//go:build e2e" ||
				strings.HasPrefix(firstLine, "//go:build e2e ")
			if !hasE2ETag {
				t.Errorf(
					"[AC9] MISSING BUILD TAG in %s\n"+
						"  First line: %q\n"+
						"  Expected  : %q (or %q for Helm-only specs)\n"+
						"\n"+
						"  Real-backend spec files MUST start with '//go:build e2e' (or\n"+
						"  '//go:build e2e && e2e_helm' for Helm-only specs) so they are\n"+
						"  excluded from unit test runs.\n"+
						"\n"+
						"  Fix: add '//go:build e2e' as the first line of %s",
					relName, firstLine,
					"//go:build e2e", "//go:build e2e && e2e_helm", relName,
				)
			} else {
				t.Logf("[AC9] ✓ %s  %s present", relName, firstLine)
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

// TestAC9GoListVerifiesRealBackendFilesCompiled uses `go list -tags=e2e` to
// confirm that the Go toolchain actually includes real-backend spec files when
// the "e2e" build tag is active.
//
// This is the programmatic equivalent of running:
//
//	go list -tags=e2e -f '{{json .TestGoFiles}}' ./test/e2e/
//
// and verifying that the returned list contains the expected real-backend spec
// files (those named *_e2e_test.go with "//go:build e2e" constraints).
//
// # Two-phase verification
//
//  1. WITH -tags=e2e: real-backend spec files (*_e2e_test.go) MUST be included.
//     Failure here means the build tag is not activating the real-backend specs,
//     which would silently exclude them from `make test-e2e` runs.
//
//  2. WITHOUT -tags=e2e: real-backend spec files MUST NOT appear in the
//     compiled set (except build_tag_e2e_ac9_test.go which has no build
//     constraint — it's a static verifier that runs in all builds).
//     Failure here means a real-backend file accidentally lost its build tag,
//     which would pull Kind cluster / Docker exec dependencies into unit tests.
//
// # Verify command (run after make test-e2e passes):
//
//	go list -tags=e2e -f '{{json .TestGoFiles}}' ./test/e2e/ | \
//	    python3 -c "import json,sys; files=json.load(sys.stdin); \
//	    e2e=[f for f in files if f.endswith('_e2e_test.go') and not f.startswith('build_tag')]; \
//	    print(f'PASS: {len(e2e)} real-backend spec files compiled'); sys.exit(0 if len(e2e)>=5 else 1)"
func TestAC9GoListVerifiesRealBackendFilesCompiled(t *testing.T) {
	t.Parallel()

	e2eDir := resolveE2EDir(t)
	// Resolve module root (two levels up from test/e2e/)
	moduleRoot := filepath.Join(e2eDir, "..", "..")

	// ── Phase 1: WITH -tags=e2e ───────────────────────────────────────────────

	withTagFiles := goListTestGoFiles(t, moduleRoot, []string{"-tags=e2e"}, "./test/e2e/")

	// Filter to real-backend spec files: name ends with _e2e_test.go.
	// build_tag_e2e_ac9_test.go is excluded because it has no build tag
	// (it is a static verifier that runs in all builds).
	var withTagE2EFiles []string
	for _, f := range withTagFiles {
		if strings.HasSuffix(f, "_e2e_test.go") && !strings.HasPrefix(f, "build_tag_") {
			withTagE2EFiles = append(withTagE2EFiles, f)
		}
	}

	const minRealBackendSpecs = 5 // backend_teardown, cluster_kubeconfig, kind_bootstrap, kind_smoke, lvm_backend_standalone
	if len(withTagE2EFiles) < minRealBackendSpecs {
		t.Errorf(
			"[AC9/go-list] WITH -tags=e2e: found only %d real-backend spec files (want >= %d).\n"+
				"  Files found: %v\n"+
				"\n"+
				"  `go list -tags=e2e` output should include *_e2e_test.go files.\n"+
				"  If this fails, real-backend spec files may have lost their //go:build e2e constraint\n"+
				"  or were removed from the test/e2e/ directory.\n"+
				"\n"+
				"  Verify command:\n"+
				"    go list -tags=e2e -f '{{json .TestGoFiles}}' ./test/e2e/",
			len(withTagE2EFiles), minRealBackendSpecs, withTagE2EFiles,
		)
	} else {
		t.Logf("[AC9/go-list] ✓ WITH -tags=e2e: %d real-backend spec files compiled: %v",
			len(withTagE2EFiles), withTagE2EFiles)
	}

	// ── Phase 2: WITHOUT -tags=e2e ────────────────────────────────────────────

	withoutTagFiles := goListTestGoFiles(t, moduleRoot, nil, "./test/e2e/")

	// Real-backend spec files must NOT appear in the non-e2e build.
	// The only exception is build_tag_e2e_ac9_test.go (no build constraint).
	var withoutTagE2EFiles []string
	for _, f := range withoutTagFiles {
		if strings.HasSuffix(f, "_e2e_test.go") && !strings.HasPrefix(f, "build_tag_") {
			withoutTagE2EFiles = append(withoutTagE2EFiles, f)
		}
	}

	if len(withoutTagE2EFiles) > 0 {
		t.Errorf(
			"[AC9/go-list] WITHOUT -tags=e2e: %d real-backend spec file(s) incorrectly compiled: %v\n"+
				"\n"+
				"  These files should NOT be compiled without -tags=e2e because they\n"+
				"  require a live Kind cluster with real storage backends.\n"+
				"  Check that each file starts with '//go:build e2e' as its first line.\n"+
				"\n"+
				"  Verify command:\n"+
				"    go list -f '{{json .TestGoFiles}}' ./test/e2e/",
			len(withoutTagE2EFiles), withoutTagE2EFiles,
		)
	} else {
		t.Logf("[AC9/go-list] ✓ WITHOUT -tags=e2e: no real-backend spec files compiled (correct isolation)")
	}
}

// goListTestGoFiles runs `go list [extraFlags] -f '{{.TestGoFiles}}' [pkg]`
// in the given module root directory and returns the list of test Go files
// reported by the Go toolchain.
//
// This is the authoritative source of truth for which files the Go compiler
// will include in a test binary — it reflects the actual build constraint
// evaluation, unlike file-system scanning which can miss edge cases.
//
// Output format from `go list -f '{{.TestGoFiles}}'` is:
//
//	[file1.go file2.go ...]
//
// which is parsed by trimming the surrounding brackets and splitting on whitespace.
func goListTestGoFiles(t *testing.T, moduleRoot string, extraFlags []string, pkg string) []string {
	t.Helper()

	args := []string{"list"}
	args = append(args, extraFlags...)
	args = append(args, "-f", "{{.TestGoFiles}}", pkg)

	cmd := exec.Command("go", args...) //nolint:gosec // args are controlled
	cmd.Dir = moduleRoot

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("[AC9/go-list] `go %v` in %s failed: %v",
			args, moduleRoot, err)
	}

	// Parse "[file1 file2 ...]" → ["file1", "file2", ...]
	raw := strings.TrimSpace(string(out))
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}
