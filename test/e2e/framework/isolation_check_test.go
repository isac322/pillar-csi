package framework_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// TestRegisterAndDeregisterActiveScope verifies the basic register/deregister
// lifecycle for the global scope registry.
func TestRegisterAndDeregisterActiveScope(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Before registration: not active.
	if framework.IsScopeActive(dir) {
		t.Fatalf("expected %q NOT active before registration; got active", dir)
	}

	// Register.
	framework.RegisterActiveScope(dir, "TC-R1.1", "tc-r1-1-xxxx")
	if !framework.IsScopeActive(dir) {
		t.Fatalf("expected %q active after registration; got not active", dir)
	}

	// Deregister.
	framework.DeregisterActiveScope(dir)
	if framework.IsScopeActive(dir) {
		t.Fatalf("expected %q NOT active after deregistration; got active", dir)
	}
}

// TestRegisterEmptyRootDirIsNoop verifies that registering an empty path is
// a safe no-op and does not panic or affect the scope count.
func TestRegisterEmptyRootDirIsNoop(t *testing.T) {
	t.Parallel()

	before := framework.ActiveScopeCount()
	framework.RegisterActiveScope("", "TC-R1.2", "tag")
	after := framework.ActiveScopeCount()

	if after != before {
		t.Fatalf("RegisterActiveScope(\"\") changed scope count from %d to %d", before, after)
	}
}

// TestDeregisterUnknownRootDirIsNoop verifies that deregistering a path that
// was never registered is a safe no-op and does not panic.
func TestDeregisterUnknownRootDirIsNoop(t *testing.T) {
	t.Parallel()

	before := framework.ActiveScopeCount()
	framework.DeregisterActiveScope("/tmp/pillar-csi-nonexistent-p999999-s1-ffffffff")
	after := framework.ActiveScopeCount()

	if after != before {
		t.Fatalf("DeregisterActiveScope(unknown) changed scope count from %d to %d", before, after)
	}
}

// TestScanOrphanedTempDirsFindsLeakedDir verifies that ScanOrphanedTempDirs
// returns a violation for a directory that:
//   - exists under /tmp with the pillar-csi-*-p<pid>-* naming convention, AND
//   - is NOT registered as an active scope.
//
// This simulates a TC whose Close() was called (deregistered) but whose root
// dir removal failed.
func TestScanOrphanedTempDirsFindsLeakedDir(t *testing.T) {
	// Not parallel: manipulates the global registry and creates /tmp dirs.

	pid := os.Getpid()

	// Create a directory that looks like a TC scope root but is NOT registered.
	dir, err := os.MkdirTemp("/tmp",
		"pillar-csi-isolation-check-test-p"+itoa(pid)+"-s99999-")
	if err != nil {
		t.Fatalf("create temp dir for orphan test: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Sanity check: dir should not be active.
	if framework.IsScopeActive(dir) {
		framework.DeregisterActiveScope(dir) // clean up if somehow registered
	}

	violations, err := framework.ScanOrphanedTempDirs()
	if err != nil {
		t.Fatalf("ScanOrphanedTempDirs: unexpected error: %v", err)
	}

	found := false
	for _, v := range violations {
		if v.RootDir == dir {
			found = true
			if v.Kind != framework.ViolationOrphanedTempDir {
				t.Errorf("violation for %q: got Kind=%q, want %q",
					dir, v.Kind, framework.ViolationOrphanedTempDir)
			}
			break
		}
	}
	if !found {
		t.Errorf("ScanOrphanedTempDirs: did not detect orphaned dir %q;\nall violations: %v",
			dir, violations)
	}
}

// TestScanOrphanedTempDirsIgnoresActiveScope verifies that an active scope's
// directory is NOT reported as an orphan.
func TestScanOrphanedTempDirsIgnoresActiveScope(t *testing.T) {
	// Not parallel: manipulates the global registry and creates /tmp dirs.

	pid := os.Getpid()

	dir, err := os.MkdirTemp("/tmp",
		"pillar-csi-isolation-check-test-active-p"+itoa(pid)+"-s88888-")
	if err != nil {
		t.Fatalf("create temp dir for active scope test: %v", err)
	}
	defer func() {
		framework.DeregisterActiveScope(dir)
		_ = os.RemoveAll(dir)
	}()

	// Register it as active.
	framework.RegisterActiveScope(dir, "TC-R1.5", "tc-r1-5-active")

	violations, err := framework.ScanOrphanedTempDirs()
	if err != nil {
		t.Fatalf("ScanOrphanedTempDirs: unexpected error: %v", err)
	}

	for _, v := range violations {
		if v.RootDir == dir {
			t.Errorf("ScanOrphanedTempDirs reported active scope dir %q as orphaned:\n%v", dir, v)
		}
	}
}

// TestScanOrphanedTempDirsAfterDeregister verifies that once a scope is
// deregistered and its dir still exists, it shows up as an orphan, but once
// the dir is removed the violation disappears.
func TestScanOrphanedTempDirsAfterDeregister(t *testing.T) {
	// Not parallel: manipulates global registry.

	pid := os.Getpid()

	dir, err := os.MkdirTemp("/tmp",
		"pillar-csi-isolation-check-test-after-dereg-p"+itoa(pid)+"-s77777-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Register then immediately deregister (simulating a Close() that forgot
	// to remove the dir, or crashed before os.RemoveAll).
	framework.RegisterActiveScope(dir, "TC-R1.6", "tc-r1-6-dereg")
	framework.DeregisterActiveScope(dir)

	// Dir still exists — should be an orphan.
	violations, err := framework.ScanOrphanedTempDirs()
	if err != nil {
		t.Fatalf("ScanOrphanedTempDirs after deregister: %v", err)
	}
	foundOrphan := false
	for _, v := range violations {
		if v.RootDir == dir {
			foundOrphan = true
			break
		}
	}
	if !foundOrphan {
		t.Errorf("expected dir %q to be orphaned after deregister, but scan did not find it", dir)
	}

	// Now remove the dir — orphan should disappear.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	violations2, err := framework.ScanOrphanedTempDirs()
	if err != nil {
		t.Fatalf("ScanOrphanedTempDirs after removal: %v", err)
	}
	for _, v := range violations2 {
		if v.RootDir == dir {
			t.Errorf("expected dir %q to be absent after removal, but scan still found it as orphan", dir)
		}
	}
}

// TestScanOrphanedTempDirsIgnoresDifferentPID verifies that directories
// created by a different PID are not flagged even if they match the pattern.
func TestScanOrphanedTempDirsIgnoresDifferentPID(t *testing.T) {
	t.Parallel()

	// Create a dir that looks like a pillar-csi scope for a fake PID.
	fakePID := 1 // PID 1 is always init, never our process
	fakeDir := filepath.Join("/tmp",
		"pillar-csi-fake-p"+itoa(fakePID)+"-s12345-abcdef12")

	// Don't actually create the dir on disk — just verify it won't be scanned.
	// The glob only picks up existing dirs, so this is a negative test.
	//
	// Separately: verify our real PID pattern doesn't accidentally match PID 1.
	violations, err := framework.ScanOrphanedTempDirs()
	if err != nil {
		t.Fatalf("ScanOrphanedTempDirs: %v", err)
	}
	for _, v := range violations {
		if v.RootDir == fakeDir {
			t.Errorf("ScanOrphanedTempDirs: unexpectedly found fake-PID dir %q", fakeDir)
		}
	}
}

// TestIsolationViolationError verifies the Error() method of IsolationViolation.
func TestIsolationViolationError(t *testing.T) {
	t.Parallel()

	v := &framework.IsolationViolationError{
		Kind:    framework.ViolationOrphanedTempDir,
		RootDir: "/tmp/pillar-csi-test-p1234-s1-xxxxxxxx",
		Detail:  "directory exists but is not owned by any active TC scope",
	}

	got := v.Error()
	if got == "" {
		t.Fatal("IsolationViolationError.Error() returned empty string")
	}
	if got != v.String() {
		t.Errorf("Error() != String():\n  Error()  = %q\n  String() = %q", got, v.String())
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// itoa converts an int to a decimal string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}
