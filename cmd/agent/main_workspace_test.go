package main

import (
	"path/filepath"
	"testing"

	"github.com/isac322/pillar-csi/internal/runtimepaths"
)

func TestResolvedDefaultConfigfsRootUsesSuiteWorkspace(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv(runtimepaths.SuiteWorkspaceEnvVar, workspace)

	if got, want := resolvedDefaultConfigfsRoot(),
		filepath.Join(workspace, "agent", "configfs"); got != want {
		t.Fatalf("resolvedDefaultConfigfsRoot() = %q, want %q", got, want)
	}
}
