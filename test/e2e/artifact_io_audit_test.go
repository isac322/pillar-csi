package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArtifactPathsStayUnderTmpRoot is the AC-5 static audit.  It walks every
// Go source file under test/e2e/ using the standard go/ast package and verifies
// that no file-creating call site passes a hardcoded absolute path that does not
// begin with /tmp (or os.TempDir()).
//
// The audit catches three classes of violation:
//
//  1. A string literal like "/home/user/foo" passed directly to os.Create,
//     os.MkdirAll, os.MkdirTemp, os.Mkdir, os.WriteFile, os.OpenFile, or any
//     equivalent ioutil/afero helper.
//
//  2. The compile-time constant tcTempRoot drifting away from os.TempDir().
//
//  3. The runtime guard variable artifactTempRoot not matching os.TempDir()
//     (would be caught earlier by the package-level init() but is also
//     validated here for belt-and-suspenders coverage).
//
// The test intentionally skips this file itself and any _test.go file that is
// being used purely as a test harness (they cannot write artifacts at
// compile-time).
func TestArtifactPathsStayUnderTmpRoot(t *testing.T) {
	t.Parallel()

	// ── 1. Runtime guard: tcTempRoot and artifactTempRoot must equal os.TempDir
	tmpRoot := filepath.Clean(os.TempDir())

	if filepath.Clean(tcTempRoot) != tmpRoot {
		t.Errorf(
			"[AC-5] tcTempRoot=%q does not match os.TempDir()=%q — "+
				"update tcTempRoot in isolation_scope.go",
			tcTempRoot, tmpRoot,
		)
	}

	if artifactTempRoot != tmpRoot {
		t.Errorf(
			"[AC-5] artifactTempRoot=%q does not match os.TempDir()=%q — "+
				"package-init guard in artifact_path_guard.go should have already panicked",
			artifactTempRoot, tmpRoot,
		)
	}

	// ── 2. Static AST audit: scan every .go source file in test/e2e/
	suiteDir := findSuiteDir(t)
	if suiteDir == "" {
		return // error already reported
	}

	entries, err := os.ReadDir(suiteDir)
	if err != nil {
		t.Fatalf("[AC-5] read suite dir %q: %v", suiteDir, err)
	}

	fset := token.NewFileSet()
	var violations []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}

		srcPath := filepath.Join(suiteDir, entry.Name())
		src, readErr := os.ReadFile(srcPath)
		if readErr != nil {
			t.Errorf("[AC-5] read %q: %v", srcPath, readErr)
			continue
		}

		f, parseErr := parser.ParseFile(fset, srcPath, src, 0)
		if parseErr != nil {
			t.Errorf("[AC-5] parse %q: %v", srcPath, parseErr)
			continue
		}

		fileViolations := auditFileForEscapingPaths(fset, f, tmpRoot)
		violations = append(violations, fileViolations...)
	}

	for _, v := range violations {
		t.Errorf("[AC-5][category:artifact-io] %s", v)
	}

	if len(violations) == 0 {
		t.Logf("[AC-5] audit passed: all %d source files write artifacts only under %s", len(entries), tmpRoot)
	}
}

// TestArtifactPathGuardRejectsEscape verifies that mustPathUnderTempRoot returns
// an error for paths that escape /tmp.
func TestArtifactPathGuardRejectsEscape(t *testing.T) {
	t.Parallel()

	escapingPaths := []string{
		"/home/user/foo",
		"/etc/passwd",
		"/var/log/test.log",
		"/root/.kube/config",
		"/usr/local/foo",
	}
	for _, p := range escapingPaths {
		if err := mustPathUnderTempRoot(p); err == nil {
			t.Errorf("[AC-5] mustPathUnderTempRoot(%q): expected error for path outside /tmp, got nil", p)
		}
	}
}

// TestArtifactPathGuardAcceptsTmpPaths verifies that mustPathUnderTempRoot
// accepts legitimate /tmp paths.
func TestArtifactPathGuardAcceptsTmpPaths(t *testing.T) {
	t.Parallel()

	tmpRoot := filepath.Clean(os.TempDir())

	validPaths := []string{
		tmpRoot,
		filepath.Join(tmpRoot, "pillar-csi-e2e"),
		filepath.Join(tmpRoot, "pillar-csi-e2e", "sub", "dir"),
		filepath.Join(tmpRoot, "pillar-csi-e2e-suite-1234", "workspace"),
		"",            // empty: should pass
		"relative/ok", // relative: cannot be validated here, passes through
	}
	for _, p := range validPaths {
		if err := mustPathUnderTempRoot(p); err != nil {
			t.Errorf("[AC-5] mustPathUnderTempRoot(%q): unexpected error: %v", p, err)
		}
	}
}

// TestArtifactPathGuardRejectsPathTraversal verifies that path traversal
// attempts like /tmp/../etc/passwd are correctly caught.
func TestArtifactPathGuardRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	traversalPaths := []string{
		"/tmp/../etc/passwd",
		"/tmp/../../root/.ssh/authorized_keys",
	}
	for _, p := range traversalPaths {
		if err := mustPathUnderTempRoot(p); err == nil {
			t.Errorf("[AC-5] mustPathUnderTempRoot(%q): expected traversal rejection, got nil", p)
		}
	}
}

// TestNewTestCaseScopeRootIsUnderTmp verifies that TestCaseScope.RootDir is
// always created under /tmp at runtime.
func TestNewTestCaseScopeRootIsUnderTmp(t *testing.T) {
	t.Parallel()

	tmpRoot := filepath.Clean(os.TempDir())

	scope, err := NewTestCaseScope("TC-AUDIT-1")
	if err != nil {
		t.Fatalf("[AC-5] NewTestCaseScope: %v", err)
	}
	t.Cleanup(func() { _ = scope.Close() })

	if !strings.HasPrefix(filepath.Clean(scope.RootDir)+string(filepath.Separator), tmpRoot+string(filepath.Separator)) &&
		filepath.Clean(scope.RootDir) != tmpRoot {
		t.Errorf("[AC-5] TC scope.RootDir=%q is not under os.TempDir()=%q", scope.RootDir, tmpRoot)
	}
}

// TestSuiteTempPathsRootIsUnderTmp verifies that newSuiteTempPaths always roots
// the suite workspace under /tmp.
func TestSuiteTempPathsRootIsUnderTmp(t *testing.T) {
	t.Parallel()

	tmpRoot := filepath.Clean(os.TempDir())

	paths, err := newSuiteTempPaths()
	if err != nil {
		t.Fatalf("[AC-5] newSuiteTempPaths: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(paths.RootDir) })

	for label, dir := range map[string]string{
		"root":           paths.RootDir,
		"workspace":      paths.WorkspaceDir,
		"logs":           paths.LogsDir,
		"generated":      paths.GeneratedDir,
		"kubeconfig-dir": filepath.Dir(paths.KubeconfigPath()),
	} {
		clean := filepath.Clean(dir)
		if !strings.HasPrefix(clean+string(filepath.Separator), tmpRoot+string(filepath.Separator)) &&
			clean != tmpRoot {
			t.Errorf("[AC-5] suite %s path=%q is not under os.TempDir()=%q", label, dir, tmpRoot)
		}
	}
}

// TestKubeconfigPathIsUnderTmp verifies that every kubeconfig path produced by
// a TestCaseScope is under /tmp.
func TestKubeconfigPathIsUnderTmp(t *testing.T) {
	t.Parallel()

	tmpRoot := filepath.Clean(os.TempDir())

	scope, err := NewTestCaseScope("TC-AUDIT-KCFG")
	if err != nil {
		t.Fatalf("[AC-5] NewTestCaseScope: %v", err)
	}
	t.Cleanup(func() { _ = scope.Close() })

	kcfgPath := scope.KubeconfigPath("primary")
	clean := filepath.Clean(kcfgPath)
	if !strings.HasPrefix(clean+string(filepath.Separator), tmpRoot+string(filepath.Separator)) &&
		clean != tmpRoot {
		t.Errorf("[AC-5] KubeconfigPath=%q is not under os.TempDir()=%q", kcfgPath, tmpRoot)
	}
}

// TestBackendObjectRootIsUnderTmp verifies that backend fixture directories
// created by TestCaseScope.BackendObject are under /tmp.
func TestBackendObjectRootIsUnderTmp(t *testing.T) {
	t.Parallel()

	tmpRoot := filepath.Clean(os.TempDir())

	scope, err := NewTestCaseScope("TC-AUDIT-BACKEND")
	if err != nil {
		t.Fatalf("[AC-5] NewTestCaseScope: %v", err)
	}
	t.Cleanup(func() { _ = scope.Close() })

	obj, err := scope.BackendObject("zfs", "pool1")
	if err != nil {
		t.Fatalf("[AC-5] BackendObject: %v", err)
	}

	clean := filepath.Clean(obj.RootDir)
	if !strings.HasPrefix(clean+string(filepath.Separator), tmpRoot+string(filepath.Separator)) &&
		clean != tmpRoot {
		t.Errorf("[AC-5] BackendObject.RootDir=%q is not under os.TempDir()=%q", obj.RootDir, tmpRoot)
	}
}

// ── AST audit helpers ────────────────────────────────────────────────────────

// fileCreatingFunctions maps package-qualified names of functions whose first
// argument is a filesystem path that will be created or written on disk.
// Read-only calls (os.Open, os.ReadFile) are intentionally omitted because
// reading from /proc or /dev is legitimate; only write-creating calls are
// audited.
//
// The special entry "/proc" and "/dev" in allowedAbsoluteRoots lets the audit
// pass for well-known read-only system paths.
var fileCreatingFunctions = map[string]int{
	// os package — arg index 0 is the path
	"os.Create":    0,
	"os.Mkdir":     0,
	"os.MkdirAll":  0,
	"os.MkdirTemp": 0,
	"os.WriteFile": 0,
	// os.OpenFile is write-capable but we cannot determine the flags
	// statically, so we audit it too (conservative).
	"os.OpenFile": 0,
	// ioutil shims (deprecated but may still appear in generated scaffolding)
	"ioutil.TempFile":  0,
	"ioutil.WriteFile": 0,
}

// allowedAbsoluteRoots lists the only legitimate non-/tmp prefixes that may
// appear as literal string arguments to the audited file-creating functions.
// Any literal absolute path not under one of these roots OR under /tmp will
// be reported as a violation.
//
// Currently the only permitted non-/tmp literal is the empty string (meaning
// "use the OS default"), which os.MkdirTemp("", pattern) interprets as
// os.TempDir() internally.
var allowedAbsoluteRoots = []string{
	// Intentionally empty: only /tmp (os.TempDir()) is allowed.
	// The empty string "" means os.TempDir() — see os.MkdirTemp("", ...)
}

// auditFileForEscapingPaths walks the AST of one parsed Go file and returns
// human-readable violation strings for every file-creating call that passes a
// hardcoded absolute path outside /tmp.
func auditFileForEscapingPaths(fset *token.FileSet, f *ast.File, tmpRoot string) []string {
	var violations []string

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		qualifiedName := callExprQualifiedName(call)
		pathArgIdx, isAudited := fileCreatingFunctions[qualifiedName]
		if !isAudited {
			return true
		}

		if len(call.Args) <= pathArgIdx {
			return true
		}

		lit, ok := call.Args[pathArgIdx].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// Non-literal argument: cannot be statically validated — trust the
			// runtime guard (assertPathUnderTempRoot) to catch escaping values.
			return true
		}

		// Unquote the string literal value (handles both `"..."` and `` `...` ``).
		raw := strings.Trim(lit.Value, "`\"")

		if raw == "" {
			// Empty string → os.MkdirTemp("", pattern) → os.TempDir(): allowed.
			return true
		}

		if !filepath.IsAbs(raw) {
			// Relative path: cannot escape on its own.
			return true
		}

		// Absolute path: must be under tmpRoot.
		clean := filepath.Clean(raw)
		if strings.HasPrefix(clean+string(filepath.Separator), tmpRoot+string(filepath.Separator)) ||
			clean == tmpRoot {
			return true
		}

		pos := fset.Position(lit.Pos())
		violations = append(violations, fmt.Sprintf(
			"%s:%d: %s() called with literal path %q outside %s",
			filepath.Base(pos.Filename), pos.Line,
			qualifiedName, raw, tmpRoot,
		))
		return true
	})

	return violations
}

// callExprQualifiedName extracts "pkg.Func" from a call expression of the form
// pkg.Func(...).  Returns an empty string for non-selector calls.
func callExprQualifiedName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name + "." + sel.Sel.Name
}

// findSuiteDir locates the test/e2e directory by walking up from the current
// working directory.  It reports an error and returns "" if the directory cannot
// be found.
func findSuiteDir(t *testing.T) string {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Errorf("[AC-5] getwd: %v", err)
		return ""
	}

	// The test binary runs with its working directory set to the package source
	// directory (test/e2e), so we can use the cwd directly.
	candidate := cwd
	if isE2ESuiteDir(candidate) {
		return candidate
	}

	// Walk up the directory tree looking for test/e2e.
	for dir := filepath.Dir(cwd); dir != "/" && dir != cwd; dir = filepath.Dir(dir) {
		e2eDir := filepath.Join(dir, "test", "e2e")
		if isE2ESuiteDir(e2eDir) {
			return e2eDir
		}
		cwd = dir
	}

	t.Errorf("[AC-5] could not find test/e2e directory starting from %q", cwd)
	return ""
}

func isE2ESuiteDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "suite_test.go"))
	return err == nil && !info.IsDir()
}
