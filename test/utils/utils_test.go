package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/runtimepaths"
)

func TestGetProjectDirResolvesRepositoryRoot(t *testing.T) {
	projectDir, err := GetProjectDir()
	if err != nil {
		t.Fatalf("GetProjectDir() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(projectDir, "go.mod")); err != nil {
		t.Fatalf("project root %q missing go.mod: %v", projectDir, err)
	}
}

func TestRunUsesSuiteWorkspaceAndTmpDir(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv(runtimepaths.SuiteWorkspaceEnvVar, workspace)

	cmd := exec.Command("sh", "-c", "printf '%s\\n%s' \"$PWD\" \"$TMPDIR\"")
	output, err := Run(cmd)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 {
		t.Fatalf("Run() output lines = %d, want 2 (%q)", len(lines), output)
	}
	if got, want := lines[0], workspace; got != want {
		t.Fatalf("pwd = %q, want %q", got, want)
	}
	if got, want := lines[1], filepath.Join(workspace, "tmp"); got != want {
		t.Fatalf("TMPDIR = %q, want %q", got, want)
	}
}
