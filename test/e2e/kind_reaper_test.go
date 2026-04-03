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
