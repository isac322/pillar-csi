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
		// The child shell derives $PWD from getcwd(), which returns the
		// canonical physical path: on macOS /tmp resolves to /private/tmp
		// (and /var to /private/var), so the spelling can differ from the
		// workspace path even though it is the same directory. Fall back to
		// a directory-identity check before declaring a mismatch.
		gotInfo, err := os.Stat(got)
		if err != nil {
			t.Fatalf("stat pwd %q: %v", got, err)
		}
		wantInfo, err := os.Stat(want)
		if err != nil {
			t.Fatalf("stat workspace %q: %v", want, err)
		}
		if !os.SameFile(gotInfo, wantInfo) {
			t.Fatalf("pwd = %q, want %q (different directories)", got, want)
		}
	}
	if got, want := lines[1], filepath.Join(workspace, "tmp"); got != want {
		t.Fatalf("TMPDIR = %q, want %q", got, want)
	}
}
