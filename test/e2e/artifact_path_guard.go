package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// artifactTempRoot is the canonical filesystem root under which every artifact
// produced by the e2e suite must reside.  It is derived at package-init time
// from os.TempDir() so that it adapts to the host OS, but remains a fixed
// constant ("/tmp") on Linux where the suite is designed to run.
//
// The value is intentionally package-private.  Nothing outside the e2e package
// should need to bypass the /tmp constraint.
var artifactTempRoot = filepath.Clean(os.TempDir())

// assertPathUnderTempRoot panics when path is an absolute filesystem path
// that does not reside under os.TempDir().  Relative paths and empty strings
// are silently accepted because they cannot directly escape the /tmp root
// (they must be joined to a base that is itself already validated).
//
// Call this function from every site that produces a file or directory path
// that will be passed to os.Create / os.MkdirAll / os.MkdirTemp / os.WriteFile
// or any other file-creating syscall wrapper.
func assertPathUnderTempRoot(path string) {
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) {
		// Relative paths cannot be individually validated here; callers are
		// expected to join them to a scope.RootDir that has already been
		// validated.
		return
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean+string(filepath.Separator), artifactTempRoot+string(filepath.Separator)) &&
		clean != artifactTempRoot {
		panic(fmt.Sprintf(
			"e2e artifact path guard: path %q escapes /tmp boundary (root=%s); "+
				"all e2e file I/O must stay under os.TempDir()",
			path, artifactTempRoot,
		))
	}
}

// mustPathUnderTempRoot is the non-panicking variant of assertPathUnderTempRoot.
// It returns an error instead of panicking, making it safe to use in setup
// helpers that already propagate errors.
func mustPathUnderTempRoot(path string) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return nil
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean+string(filepath.Separator), artifactTempRoot+string(filepath.Separator)) &&
		clean != artifactTempRoot {
		return fmt.Errorf(
			"artifact path %q escapes /tmp boundary (root=%s): "+
				"all e2e file I/O must stay under os.TempDir()",
			path, artifactTempRoot,
		)
	}
	return nil
}

// tcTempRootMatchesOSTempDir is a package-init guard that fails hard if
// tcTempRoot (the compile-time constant) diverges from os.TempDir() at
// runtime.  On every supported Linux CI host /tmp == os.TempDir(); if that
// invariant ever breaks the suite would silently write artifacts outside /tmp.
func init() {
	if filepath.Clean(tcTempRoot) != artifactTempRoot {
		panic(fmt.Sprintf(
			"e2e setup: tcTempRoot=%q does not match os.TempDir()=%q; "+
				"update tcTempRoot in isolation_scope.go to match the host temp root",
			tcTempRoot, artifactTempRoot,
		))
	}
}
