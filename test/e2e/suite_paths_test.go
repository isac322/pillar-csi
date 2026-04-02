package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewSuiteTempPathsCreatesTmpScopedLayout(t *testing.T) {
	t.Parallel()

	paths, err := newSuiteTempPaths()
	if err != nil {
		t.Fatalf("newSuiteTempPaths: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(paths.RootDir)
	})

	if !pathWithinRoot(tcTempRoot, paths.RootDir) {
		t.Fatalf("suite root = %q, want under %s", paths.RootDir, tcTempRoot)
	}

	checkDir := func(label, dir string) {
		t.Helper()

		if !pathWithinRoot(paths.RootDir, dir) {
			t.Fatalf("%s dir = %q, want under %s", label, dir, paths.RootDir)
		}
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatalf("stat %s dir %q: %v", label, dir, statErr)
		}
		if !info.IsDir() {
			t.Fatalf("%s path = %q, want directory", label, dir)
		}
	}

	checkDir("workspace", paths.WorkspaceDir)
	checkDir("logs", paths.LogsDir)
	checkDir("generated", paths.GeneratedDir)

	if got := paths.KubeconfigPath(); filepath.Dir(got) != paths.GeneratedDir {
		t.Fatalf("kubeconfig dir = %q, want %q", filepath.Dir(got), paths.GeneratedDir)
	}
}

func TestSuiteTempPathsValidateRejectsEscapingSubdirs(t *testing.T) {
	t.Parallel()

	paths := &suiteTempPaths{
		RootDir:      filepath.Join(tcTempRoot, "pillar-csi-e2e-suite-test"),
		WorkspaceDir: filepath.Join(tcTempRoot, "pillar-csi-e2e-suite-test", "workspace"),
		LogsDir:      filepath.Join(tcTempRoot, "pillar-csi-e2e-suite-test", "logs"),
		GeneratedDir: filepath.Join(tcTempRoot, "escape"),
	}

	if err := paths.validate(); err == nil {
		t.Fatal("validate: expected escape rejection, got nil")
	}
}
