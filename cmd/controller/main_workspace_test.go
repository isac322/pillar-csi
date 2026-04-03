package main

import (
	"path/filepath"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/runtimepaths"
)

func TestResolvedDefaultCSIEndpointUsesSuiteWorkspace(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv(runtimepaths.SuiteWorkspaceEnvVar, workspace)

	if got, want := resolvedDefaultCSIEndpoint(),
		"unix://"+filepath.Join(workspace, "controller", "csi.sock"); got != want {
		t.Fatalf("resolvedDefaultCSIEndpoint() = %q, want %q", got, want)
	}
}
