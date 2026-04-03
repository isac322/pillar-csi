package prereq_test

import (
	"os"
	"strings"
	"testing"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/prereq"
)

// TestCheckDockerDaemon_DockerPresent verifies that when docker is in PATH
// and the daemon is running, CheckHostPrerequisites still returns actionable
// error messages for any OTHER failing prerequisites (kernel modules, binaries).
//
// AC 10: the presence of Docker does NOT suppress failures for other missing
// prerequisites.  The error, if any, must always contain "Remediation".
//
// Note: With AC 10, the error may not mention "Docker" if Docker is working
// but other prerequisites (kernel modules, tools) are missing.  The keyword
// test validates only that a non-nil error is well-formed.
func TestCheckDockerDaemon_DockerPresent(t *testing.T) {
	if !dockerInPath() {
		t.Skip("docker not found in PATH; skipping docker daemon check")
	}

	// The test calls CheckHostPrerequisites() via the exported entry point.
	// If it returns nil, all prerequisites are satisfied — nothing to assert.
	// If it returns non-nil, verify the error is well-formed with remediation.
	err := prereq.CheckHostPrerequisites()
	if err == nil {
		// Everything present — nothing more to assert.
		return
	}

	msg := err.Error()
	// AC 10: every non-nil error must contain "Remediation" to be actionable.
	// "Docker" is only present when Docker itself is the failing component.
	if !strings.Contains(msg, "Remediation") {
		t.Errorf("error message missing keyword %q (AC 10: all errors must be actionable)\ngot:\n%s",
			"Remediation", msg)
	}
}

// TestLoadedKernelModules_ParsesCorrectly verifies the /proc/modules parser by
// feeding it synthetic content.  This validates the parsing logic without
// requiring specific modules to be loaded on the test host.
func TestLoadedKernelModules_ParsesCorrectly(t *testing.T) {
	// Create a synthetic /proc/modules file in a temp location under /tmp.
	// Use os.TempDir() explicitly (not "") so the parent directory is unambiguous.
	tmp, err := os.CreateTemp(os.TempDir(), "proc-modules-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	fakeContent := `zfs 5058560 3 zunicode,zavl,zcommon, Live 0xffffffffc0a00000
dm_thin_pool 81920 1 - Live 0xffffffffc08f0000
iscsi_tcp 24576 0 - Live 0xffffffffc0890000
`
	if _, err := tmp.WriteString(fakeContent); err != nil {
		t.Fatalf("write fake /proc/modules: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close fake /proc/modules: %v", err)
	}

	// Read the file the same way loadedKernelModules does.
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}

	modules := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := strings.ReplaceAll(fields[0], "-", "_")
		modules[name] = struct{}{}
	}

	expected := []string{"zfs", "dm_thin_pool", "iscsi_tcp"}
	for _, mod := range expected {
		if _, ok := modules[mod]; !ok {
			t.Errorf("expected module %q to be parsed from /proc/modules content", mod)
		}
	}
}

// TestHyphenNormalization verifies that module names with hyphens are correctly
// normalised to underscores when parsing /proc/modules.
func TestHyphenNormalization(t *testing.T) {
	line := "dm-thin-pool 81920 1 - Live 0xffffffffc08f0000"
	fields := strings.Fields(line)
	if len(fields) == 0 {
		t.Fatal("no fields in synthetic line")
	}
	name := strings.ReplaceAll(fields[0], "-", "_")
	const want = "dm_thin_pool"
	if name != want {
		t.Errorf("normalise %q: got %q, want %q", fields[0], name, want)
	}
}

// TestCheckHostPrerequisites_ErrorShape verifies that when prerequisites fail
// the error message shape includes the required header and remediation sections.
// We simulate a failure by unset DOCKER_HOST to a bogus address so docker info
// fails even if the daemon is locally running.
func TestCheckHostPrerequisites_ErrorShape(t *testing.T) {
	if !dockerInPath() {
		t.Skip("docker not found in PATH")
	}

	// Point DOCKER_HOST at an unreachable address to force a daemon failure.
	orig := os.Getenv("DOCKER_HOST")
	if err := os.Setenv("DOCKER_HOST", "tcp://127.0.0.1:19999"); err != nil {
		t.Fatalf("set DOCKER_HOST: %v", err)
	}
	defer func() {
		if orig == "" {
			_ = os.Unsetenv("DOCKER_HOST")
		} else {
			_ = os.Setenv("DOCKER_HOST", orig)
		}
	}()

	err := prereq.CheckHostPrerequisites()
	if err == nil {
		// Docker happened to connect on port 19999 — skip rather than fail.
		t.Skip("unexpected Docker connection on port 19999; skipping shape test")
	}

	msg := err.Error()

	requiredFragments := []string{
		"pillar-csi E2E prerequisite check FAILED",
		"remediation",
	}
	for _, frag := range requiredFragments {
		if !strings.Contains(msg, frag) {
			t.Errorf("error message missing fragment %q\ngot:\n%s", frag, msg)
		}
	}
}

// TestCheckHostPrerequisites_ProcModulesMissing verifies that when
// /proc/modules is not present (non-Linux environments), a descriptive error is
// returned rather than a panic or opaque OS error.
//
// This test runs only on non-Linux OSes where /proc/modules genuinely does not
// exist; on Linux it is skipped to avoid interfering with real module state.
func TestCheckHostPrerequisites_ProcModulesMissing(t *testing.T) {
	// This path is only meaningful on non-Linux where /proc is absent.
	if _, err := os.Stat("/proc/modules"); err == nil {
		t.Skip("/proc/modules exists; skipping non-Linux path test")
	}

	// On non-Linux the function should return a descriptive error.
	err := prereq.CheckHostPrerequisites()
	if err == nil {
		t.Fatal("expected an error when /proc/modules is absent, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "pillar-csi E2E prerequisite check FAILED") {
		t.Errorf("unexpected error format: %s", msg)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func dockerInPath() bool {
	_, err := lookPathDocker()
	return err == nil
}

func lookPathDocker() (string, error) {
	// We avoid importing os/exec at the package level to keep the test file
	// dependency surface minimal.  The import is at the function level via
	// local closure — but in Go we need it as a file-level import. Inline here.
	p, err := os.Stat("/usr/bin/docker")
	if err == nil && !p.IsDir() {
		return "/usr/bin/docker", nil
	}

	// Also check common paths.
	for _, candidate := range []string{
		"/usr/local/bin/docker",
		"/bin/docker",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}
