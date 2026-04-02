package runtimepaths

import (
	"path/filepath"
	"testing"
)

func TestSuiteWorkspaceDirRejectsRelativePaths(t *testing.T) {
	t.Setenv(SuiteWorkspaceEnvVar, "relative/workspace")

	if got := SuiteWorkspaceDir(); got != "" {
		t.Fatalf("SuiteWorkspaceDir() = %q, want empty for relative workspace", got)
	}
}

func TestRuntimePathResolversUseSuiteWorkspace(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv(SuiteWorkspaceEnvVar, workspace)

	if got, want := ResolveControllerCSIEndpoint("unix:///var/lib/kubelet/plugins/pillar-csi.bhyoo.com/csi.sock"),
		"unix://"+filepath.Join(workspace, "controller", "csi.sock"); got != want {
		t.Fatalf("ResolveControllerCSIEndpoint() = %q, want %q", got, want)
	}
	if got, want := ResolveNodeCSISocketPath("/var/lib/kubelet/plugins/pillar-csi.bhyoo.com/csi.sock"),
		filepath.Join(workspace, "node", "csi.sock"); got != want {
		t.Fatalf("ResolveNodeCSISocketPath() = %q, want %q", got, want)
	}
	if got, want := ResolveNodeStateDir("/var/lib/pillar-csi/node"),
		filepath.Join(workspace, "node", "state"); got != want {
		t.Fatalf("ResolveNodeStateDir() = %q, want %q", got, want)
	}
	if got, want := ResolveAgentConfigfsRoot("/sys/kernel/config"),
		filepath.Join(workspace, "agent", "configfs"); got != want {
		t.Fatalf("ResolveAgentConfigfsRoot() = %q, want %q", got, want)
	}
	if got, want := ResolveCommandWorkDir("/repo"), workspace; got != want {
		t.Fatalf("ResolveCommandWorkDir() = %q, want %q", got, want)
	}
	if got, want := ResolveCommandTempDir(), filepath.Join(workspace, "tmp"); got != want {
		t.Fatalf("ResolveCommandTempDir() = %q, want %q", got, want)
	}
}

func TestResolveCommandTempDirFallsBackToTmp(t *testing.T) {
	t.Setenv(SuiteWorkspaceEnvVar, "")

	if got, want := ResolveCommandTempDir(), filepath.Join(tempRoot, "pillar-csi-utils"); got != want {
		t.Fatalf("ResolveCommandTempDir() = %q, want %q", got, want)
	}
}
