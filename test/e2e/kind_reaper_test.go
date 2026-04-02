package e2e

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
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
// whose names start with orphanClusterPrefix are deleted.
func TestReapOrphanedClustersDeletesMatchingClusters(t *testing.T) {
	t.Parallel()

	orphan1 := "pillar-csi-e2e-p0001-aabbccdd"
	orphan2 := "pillar-csi-e2e-p0002-11223344"

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

	orphan := "pillar-csi-e2e-p0001-deadbeef"

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
