package csi

import (
	"path/filepath"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/runtimepaths"
)

func TestNewNodeServerUsesSuiteWorkspaceStateDir(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv(runtimepaths.SuiteWorkspaceEnvVar, workspace)

	server := NewNodeServer("node-a", nil, nil)

	if got, want := server.stateDir, filepath.Join(workspace, "node", "state"); got != want {
		t.Fatalf("stateDir = %q, want %q", got, want)
	}
}

func TestNewNodeServerFallsBackToProductionStateDir(t *testing.T) {
	t.Setenv(runtimepaths.SuiteWorkspaceEnvVar, "")

	server := NewNodeServer("node-a", nil, nil)

	if got, want := server.stateDir, defaultStateDir; got != want {
		t.Fatalf("stateDir = %q, want %q", got, want)
	}
}
