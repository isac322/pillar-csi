package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// withDeadPIDChecker overrides processAliveChecker for the duration of the
// test so that all PID checks report the given pids as dead (not running).
// All other PIDs are treated as alive. The original checker is restored via
// t.Cleanup.
//
// This helper mutates a package-level variable; callers MUST NOT call
// t.Parallel() when using it (or races will corrupt the shared state).
func withDeadPIDChecker(t *testing.T, deadPIDs ...int) {
	t.Helper()
	original := processAliveChecker
	t.Cleanup(func() { processAliveChecker = original })

	dead := make(map[int]bool, len(deadPIDs))
	for _, pid := range deadPIDs {
		dead[pid] = true
	}
	processAliveChecker = func(pid int) bool {
		return !dead[pid]
	}
}

// impossiblePID is a PID value guaranteed to be above the Linux kernel's
// maximum PID (typically 4,194,304 = 2^22). Cluster names embedding this PID
// will always be treated as orphaned by defaultIsAliveProcess without any
// mocking, making them suitable for parallel tests that must not modify the
// package-level processAliveChecker.
//
// Note: any value > 4,194,304 works; we use 9,900,001–9,900,009 as a stable
// pool of test-reserved fake PIDs.
const (
	impossiblePID1 = 9_900_001
	impossiblePID2 = 9_900_002
	impossiblePID3 = 9_900_003
)

// TestReapOrphanedClustersSkipsNonMatchingClusters verifies that clusters
// whose names do not start with the orphanClusterPrefix are not deleted.
func TestReapOrphanedClustersSkipsNonMatchingClusters(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters": {stdout: "other-cluster\nanother-cluster\n"},
		},
	}

	// Temporarily override the runner used by reapOrphanedClusters would use —
	// since reapOrphanedClusters calls execCommandRunner directly, we test the
	// underlying parse logic via reapOrphanClustersWithRunner.
	err := reapOrphanClustersWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: unexpected error: %v", err)
	}

	// Only the "kind get clusters" call should have happened; no delete calls.
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %v; want exactly 1 (kind get clusters)", runner.calls)
	}
}

// TestReapOrphanedClustersDeletesMatchingClusters verifies that clusters
// whose names start with orphanClusterPrefix are deleted when their owning
// process is no longer running.
//
// We use impossiblePID1 and impossiblePID2 (> kernel pid_max = 4,194,304)
// so defaultIsAliveProcess returns false without any mocking, enabling this
// test to run in parallel safely.
func TestReapOrphanedClustersDeletesMatchingClusters(t *testing.T) {
	t.Parallel()

	orphan1 := fmt.Sprintf("pillar-csi-e2e-p%d-aabbccdd", impossiblePID1)
	orphan2 := fmt.Sprintf("pillar-csi-e2e-p%d-11223344", impossiblePID2)

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters":                     {stdout: orphan1 + "\nother-cluster\n" + orphan2 + "\n"},
			"kind delete cluster --name " + orphan1: {},
			"kind delete cluster --name " + orphan2: {},
		},
	}

	err := reapOrphanClustersWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: unexpected error: %v", err)
	}

	// Expect: 1 list call + 2 delete calls.
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %v; want 3 (1 list + 2 deletes)", runner.calls)
	}

	output := buf.String()
	if !strings.Contains(output, orphan1) {
		t.Errorf("output does not mention orphan %q:\n%s", orphan1, output)
	}
	if !strings.Contains(output, orphan2) {
		t.Errorf("output does not mention orphan %q:\n%s", orphan2, output)
	}
}

// TestReapOrphanedClustersHandlesEmptyList verifies that an empty cluster list
// produces no delete calls and no errors.
func TestReapOrphanedClustersHandlesEmptyList(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters": {stdout: ""},
		},
	}

	err := reapOrphanClustersWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: unexpected error: %v", err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("calls = %v; want exactly 1 (kind get clusters)", runner.calls)
	}
}

// TestReapOrphanedClustersHandlesNoKindClustersFoundStdout verifies the
// "No kind clusters found." stdout sentinel is handled gracefully.
func TestReapOrphanedClustersHandlesNoKindClustersFoundStdout(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters": {stdout: "No kind clusters found."},
		},
	}

	err := reapOrphanClustersWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: unexpected error: %v", err)
	}

	// No delete calls should have been made.
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %v; want exactly 1 (kind get clusters)", runner.calls)
	}
}

// TestReapOrphanedClustersHandlesNoKindClustersFoundError verifies that
// "no kind clusters found" in the error text is treated as "nothing to reap".
func TestReapOrphanedClustersHandlesNoKindClustersFoundError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters": {
				stdout: "",
				err:    errors.New("kind get clusters: No kind clusters found."),
			},
		},
	}

	err := reapOrphanClustersWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: unexpected error: %v", err)
	}
}

// TestReapOrphanedClustersDeleteFailureIsLogged verifies that a delete failure
// is logged but does not abort the reaper (best-effort semantics).
func TestReapOrphanedClustersDeleteFailureIsLogged(t *testing.T) {
	t.Parallel()

	// Use impossiblePID3 (> pid_max) so no mocking is needed.
	orphan := fmt.Sprintf("pillar-csi-e2e-p%d-deadbeef", impossiblePID3)

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters": {stdout: orphan + "\n"},
			"kind delete cluster --name " + orphan: {
				err: errors.New("docker: connection refused"),
			},
		},
	}

	err := reapOrphanClustersWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "WARNING") {
		t.Errorf("output does not contain WARNING for failed delete:\n%s", output)
	}
	if !strings.Contains(output, orphan) {
		t.Errorf("output does not mention the orphan cluster name:\n%s", output)
	}
}

// ── reapOrphanKernelResourcesWithRunner tests ─────────────────────────────────

// TestReapKernelResourcesNoOrphans verifies that when no pillar-e2e resources
// are present, no destroy/remove calls are made.
func TestReapKernelResourcesNoOrphans(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"zpool list -H -o name":          {stdout: "rpool\nother-pool\n"},
			"losetup -a":                     {stdout: "/dev/loop0: []: (/var/lib/docker/overlay.img)\n"},
			"vgs --noheadings -o vg_name":    {stdout: "  ubuntu-vg\n"},
		},
	}

	err := reapOrphanKernelResourcesWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanKernelResourcesWithRunner: unexpected error: %v", err)
	}

	// Only 3 list calls: zpool list, losetup -a, vgs.
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %v; want exactly 3 (zpool list, losetup -a, vgs)", runner.calls)
	}
}

// TestReapKernelResourcesOrphanedZFSPool verifies that orphaned ZFS pools
// matching orphanKernelResourcePrefix are destroyed.
func TestReapKernelResourcesOrphanedZFSPool(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"zpool list -H -o name":                          {stdout: "rpool\npillar-e2e-zfs-abcd1234\n"},
			"zpool destroy -f pillar-e2e-zfs-abcd1234":      {},
			"losetup -a":                                     {stdout: ""},
			"vgs --noheadings -o vg_name":                   {stdout: ""},
		},
	}

	err := reapOrphanKernelResourcesWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanKernelResourcesWithRunner: unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "pillar-e2e-zfs-abcd1234") {
		t.Errorf("output does not mention destroyed pool:\n%s", output)
	}
	if strings.Contains(output, "WARNING") {
		t.Errorf("unexpected WARNING in output:\n%s", output)
	}
}

// TestReapKernelResourcesOrphanedLoopDevice verifies that orphaned loop
// devices backing pillar-e2e images are detached.
func TestReapKernelResourcesOrphanedLoopDevice(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"zpool list -H -o name":                                                {stdout: ""},
			"losetup -a":                                                           {stdout: "/dev/loop7: []: (/tmp/pillar-e2e-lvm-abcd1234.img)\n/dev/loop0: []: (/other.img)\n"},
			"losetup -d /dev/loop7":                                                {},
			"vgs --noheadings -o vg_name":                                         {stdout: ""},
		},
	}

	err := reapOrphanKernelResourcesWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanKernelResourcesWithRunner: unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "/dev/loop7") {
		t.Errorf("output does not mention detached loop device:\n%s", output)
	}
	if strings.Contains(output, "WARNING") {
		t.Errorf("unexpected WARNING in output:\n%s", output)
	}
}

// TestReapKernelResourcesOrphanedLVMVG verifies that orphaned LVM VGs
// matching orphanKernelResourcePrefix are removed.
func TestReapKernelResourcesOrphanedLVMVG(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"zpool list -H -o name":                   {stdout: ""},
			"losetup -a":                              {stdout: ""},
			"vgs --noheadings -o vg_name":             {stdout: "  ubuntu-vg\n  pillar-e2e-lvm-abcd1234\n"},
			"vgremove -f pillar-e2e-lvm-abcd1234":    {},
		},
	}

	err := reapOrphanKernelResourcesWithRunner(context.Background(), runner, &buf)
	if err != nil {
		t.Fatalf("reapOrphanKernelResourcesWithRunner: unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "pillar-e2e-lvm-abcd1234") {
		t.Errorf("output does not mention removed VG:\n%s", output)
	}
	if strings.Contains(output, "WARNING") {
		t.Errorf("unexpected WARNING in output:\n%s", output)
	}
}

// TestReapKernelResourcesToolNotInstalled verifies that when list commands fail
// (e.g. tool not installed), the reaper silently ignores the error and
// continues with the next resource type.
func TestReapKernelResourcesToolNotInstalled(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"zpool list -H -o name":        {err: errors.New("zpool: command not found")},
			"losetup -a":                   {err: errors.New("losetup: command not found")},
			"vgs --noheadings -o vg_name": {err: errors.New("vgs: command not found")},
		},
	}

	err := reapOrphanKernelResourcesWithRunner(context.Background(), runner, &buf)
	if err != nil {
		// Errors from list commands are silently ignored (tool not installed).
		t.Fatalf("reapOrphanKernelResourcesWithRunner: unexpected error: %v", err)
	}
}

// TestReapKernelResourcesDestroyFailureIsLogged verifies that a failure to
// destroy/remove an orphaned resource is logged with WARNING but does not
// abort cleanup of remaining resources.
func TestReapKernelResourcesDestroyFailureIsLogged(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"zpool list -H -o name":                     {stdout: "pillar-e2e-zfs-deadbeef\n"},
			"zpool destroy -f pillar-e2e-zfs-deadbeef": {err: errors.New("pool is busy")},
			"losetup -a":                                {stdout: ""},
			"vgs --noheadings -o vg_name":              {stdout: ""},
		},
	}

	err := reapOrphanKernelResourcesWithRunner(context.Background(), runner, &buf)
	// The destroy failure should be returned.
	if err == nil {
		t.Fatal("expected non-nil error for destroy failure, got nil")
	}

	output := buf.String()
	if !strings.Contains(output, "WARNING") {
		t.Errorf("output does not contain WARNING for failed destroy:\n%s", output)
	}
}
