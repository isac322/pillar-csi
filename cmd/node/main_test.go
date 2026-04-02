package main

import (
	"path/filepath"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/runtimepaths"
)

func TestResolvedDefaultCSISocketPathUsesSuiteWorkspace(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv(runtimepaths.SuiteWorkspaceEnvVar, workspace)

	if got, want := resolvedDefaultCSISocketPath(),
		filepath.Join(workspace, "node", "csi.sock"); got != want {
		t.Fatalf("resolvedDefaultCSISocketPath() = %q, want %q", got, want)
	}
}
