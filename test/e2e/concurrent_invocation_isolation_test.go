package e2e

// concurrent_invocation_isolation_test.go — Sub-AC 2: tests that prove
// concurrent go test invocations each get their own Kind cluster name and
// kubeconfig path with zero possibility of collision.
//
// Verified invariants:
//
//  1. Cluster names embed the current process PID (format p{pid}).
//  2. Two calls to newKindBootstrapState within the same process produce
//     different cluster names and suite root directories.
//  3. Cluster names are valid DNS labels (≤63 chars, [a-z0-9-] only).
//  4. Kubeconfig paths are distinct for each newKindBootstrapState call and
//     live under /tmp.
//  5. pidFromClusterName correctly extracts the PID embedded in the name.
//  6. pidFromClusterName returns 0 for names that don't match the pattern.
//  7. isOrphanedCluster returns false for clusters with an alive PID.
//  8. isOrphanedCluster returns true for clusters with a dead PID.
//  9. isOrphanedCluster returns false for names without an embedded PID.
// 10. The reaper (reapOrphanClustersWithRunner) skips clusters whose
//     owning process is still alive — i.e., concurrent sibling invocations.
// 11. The reaper deletes clusters whose owning process is dead.
// 12. isAliveProcess returns true for the current process (os.Getpid).
// 13. isAliveProcess returns false for a PID that does not exist.
// 14. Cluster name uniqueness holds across a large number of synthetic calls
//     (entropy is sufficient to prevent intra-process collisions).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// clusterNameDNSLabelRegex is the Kubernetes DNS label constraint: lowercase
// alphanumeric and hyphens, first and last characters must be alphanumeric,
// max 63 chars.
var clusterNameDNSLabelRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

func isValidClusterDNSLabel(s string) bool {
	return len(s) <= 63 && clusterNameDNSLabelRegex.MatchString(s)
}

// ── 1. Cluster names embed the current process PID ────────────────────────────

func TestClusterNameContainsCurrentPID(t *testing.T) {
	t.Parallel()

	state, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("newKindBootstrapState: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state.SuiteRootDir) })

	pid := os.Getpid()
	pidToken := fmt.Sprintf("p%d", pid)
	if !strings.Contains(state.ClusterName, pidToken) {
		t.Errorf("cluster name %q does not contain PID token %q — "+
			"concurrent isolation requires PID in cluster name",
			state.ClusterName, pidToken)
	}
}

// ── 2. Two newKindBootstrapState calls produce different names & roots ─────────

func TestTwoBootstrapStatesAreDistinct(t *testing.T) {
	t.Parallel()

	left, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("left newKindBootstrapState: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(left.SuiteRootDir) })

	right, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("right newKindBootstrapState: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(right.SuiteRootDir) })

	if left.ClusterName == right.ClusterName {
		t.Errorf("cluster names are identical %q — "+
			"concurrent invocations would collide on the same Kind cluster",
			left.ClusterName)
	}
	if left.SuiteRootDir == right.SuiteRootDir {
		t.Errorf("suite root dirs are identical %q — "+
			"concurrent invocations would collide on the same kubeconfig directory",
			left.SuiteRootDir)
	}
	if left.KubeconfigPath == right.KubeconfigPath {
		t.Errorf("kubeconfig paths are identical %q — "+
			"concurrent invocations would clobber each other's kubeconfig files",
			left.KubeconfigPath)
	}
}

// ── 3. Cluster names are valid DNS labels ─────────────────────────────────────

func TestClusterNameIsDNSLabel(t *testing.T) {
	t.Parallel()

	state, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("newKindBootstrapState: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state.SuiteRootDir) })

	if !isValidClusterDNSLabel(state.ClusterName) {
		t.Errorf("cluster name %q is not a valid DNS label (must match %s and be ≤63 chars)",
			state.ClusterName, clusterNameDNSLabelRegex)
	}
}

// ── 4. Kubeconfig paths are distinct and under /tmp ───────────────────────────

func TestKubeconfigPathsAreDistinctAndUnderTmp(t *testing.T) {
	t.Parallel()

	a, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("a newKindBootstrapState: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(a.SuiteRootDir) })

	b, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("b newKindBootstrapState: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(b.SuiteRootDir) })

	tmp := os.TempDir()
	for _, path := range []string{a.KubeconfigPath, b.KubeconfigPath} {
		if !strings.HasPrefix(path, tmp) {
			t.Errorf("kubeconfig path %q is not under %s — environment-hygiene violation", path, tmp)
		}
		if !strings.Contains(path, "pillar-csi") {
			t.Errorf("kubeconfig path %q does not contain pillar-csi — "+
				"unexpected directory hierarchy", path)
		}
	}
	if a.KubeconfigPath == b.KubeconfigPath {
		t.Errorf("kubeconfig paths are identical %q — "+
			"concurrent invocations would overwrite each other's kubeconfig", a.KubeconfigPath)
	}
}

// ── 5. pidFromClusterName extracts the embedded PID ───────────────────────────

func TestPIDFromClusterNameParsesKnownFormat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		wantPID int
	}{
		{"pillar-csi-e2e-p12345-abcd1234", 12345},
		{"pillar-csi-e2e-p1-ffffffff", 1},
		{"pillar-csi-e2e-p99999-00000000", 99999},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pidFromClusterName(tc.name)
			if got != tc.wantPID {
				t.Errorf("pidFromClusterName(%q) = %d, want %d", tc.name, got, tc.wantPID)
			}
		})
	}
}

// ── 6. pidFromClusterName returns 0 for unrecognised names ────────────────────

func TestPIDFromClusterNameReturnsZeroForUnrecognisedNames(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"other-cluster",
		"pillar-csi-e2e-",          // no PID token
		"pillar-csi-e2e-notpid",    // no 'p' prefix
		"pillar-csi-e2e-p",         // 'p' with no digits
		"pillar-csi-e2e-p-entropy", // 'p' immediately followed by '-'
		"pillar-csi-e2e-p0-abcd",   // PID 0 is invalid
		"pillar-csi-e2e-p-1-abcd",  // negative PID (after dnsLabelToken normalisation, '-1' → '1' but structure differs)
	}

	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := pidFromClusterName(name)
			if got != 0 {
				t.Errorf("pidFromClusterName(%q) = %d, want 0 (unrecognised format)", name, got)
			}
		})
	}
}

// ── 7. isOrphanedCluster returns false for alive PIDs ────────────────────────

func TestIsOrphanedClusterReturnsFalseForAlivePID(t *testing.T) {
	t.Parallel()

	// Use the current process PID — it is definitely alive.
	currentPID := os.Getpid()
	clusterName := fmt.Sprintf("pillar-csi-e2e-p%d-abcd1234", currentPID)

	// Ensure the default processAliveChecker is used (no override needed — the
	// current process is alive and the default checker will confirm that).
	if isOrphanedCluster(clusterName) {
		t.Errorf("isOrphanedCluster(%q) returned true for live PID %d — "+
			"reaper would incorrectly delete a concurrent invocation's cluster",
			clusterName, currentPID)
	}
}

// ── 8. isOrphanedCluster returns true for dead PIDs ──────────────────────────

func TestIsOrphanedClusterReturnsTrueForDeadPID(t *testing.T) {
	t.Parallel()

	// Use impossiblePID1 (> pid_max) so defaultIsAliveProcess returns false
	// without any global state mutation — safe for parallel execution.
	clusterName := fmt.Sprintf("pillar-csi-e2e-p%d-abcd1234", impossiblePID1)
	if !isOrphanedCluster(clusterName) {
		t.Errorf("isOrphanedCluster(%q) returned false for impossible PID %d — "+
			"reaper would incorrectly skip a stale cluster from a dead invocation",
			clusterName, impossiblePID1)
	}
}

// ── 9. isOrphanedCluster returns false for names without PID ─────────────────

func TestIsOrphanedClusterReturnsFalseForNoPID(t *testing.T) {
	t.Parallel()

	cases := []string{
		"pillar-csi-e2e-",
		"pillar-csi-e2e-nopid",
		"other-cluster",
		"",
	}

	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if isOrphanedCluster(name) {
				t.Errorf("isOrphanedCluster(%q) returned true for name without PID — "+
					"conservative default should be false", name)
			}
		})
	}
}

// ── 10. Reaper skips clusters from alive processes (concurrent sibling) ───────

func TestReaperSkipsLiveProcessClusters(t *testing.T) {
	t.Parallel()

	// Use the current process PID as the "live" process (definitely alive) and
	// impossiblePID2 as the "dead" process (> pid_max, always dead).
	// No global state mutation needed — safe for parallel tests.
	livePID := os.Getpid()
	liveCluster := fmt.Sprintf("pillar-csi-e2e-p%d-aabbccdd", livePID)
	deadCluster := fmt.Sprintf("pillar-csi-e2e-p%d-11223344", impossiblePID2)

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters": {
				stdout: liveCluster + "\n" + deadCluster + "\n",
			},
			"kind delete cluster --name " + deadCluster: {},
			// liveCluster must NOT be called — fakeCommandRunner would fatal if it is.
		},
	}

	if err := reapOrphanClustersWithRunner(context.Background(), runner, &buf); err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: %v", err)
	}

	// 1 list + 1 delete (only deadCluster).
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %d, want 2 (1 list + 1 delete for dead cluster only); "+
			"calls: %v", len(runner.calls), runner.calls)
	}

	output := buf.String()

	// Dead cluster must be mentioned as deleted.
	if !strings.Contains(output, deadCluster) {
		t.Errorf("output does not mention deleted dead cluster %q:\n%s", deadCluster, output)
	}

	// Live cluster must be mentioned as skipped (not deleted).
	if !strings.Contains(output, liveCluster) {
		t.Errorf("output does not mention skipped live cluster %q:\n%s", liveCluster, output)
	}
	if !strings.Contains(output, "skipping") {
		t.Errorf("output does not contain 'skipping' for live cluster:\n%s", output)
	}
}

// ── 11. Reaper deletes clusters from dead processes ──────────────────────────

func TestReaperDeletesDeadProcessClusters(t *testing.T) {
	t.Parallel()

	// Use impossiblePID1 and impossiblePID2 — no global state mutation needed.
	dead1 := fmt.Sprintf("pillar-csi-e2e-p%d-aabb1111", impossiblePID1)
	dead2 := fmt.Sprintf("pillar-csi-e2e-p%d-ccdd2222", impossiblePID2)

	var buf bytes.Buffer
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind get clusters":                   {stdout: dead1 + "\n" + dead2 + "\nother-cluster\n"},
			"kind delete cluster --name " + dead1: {},
			"kind delete cluster --name " + dead2: {},
		},
	}

	if err := reapOrphanClustersWithRunner(context.Background(), runner, &buf); err != nil {
		t.Fatalf("reapOrphanClustersWithRunner: %v", err)
	}

	// 1 list + 2 deletes.
	if len(runner.calls) != 3 {
		t.Fatalf("runner calls = %d, want 3 (1 list + 2 deletes); calls: %v",
			len(runner.calls), runner.calls)
	}
}

// ── 12. isAliveProcess returns true for the current process ──────────────────

func TestIsAliveProcessReturnsTrueForCurrentProcess(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()
	if !defaultIsAliveProcess(pid) {
		t.Errorf("defaultIsAliveProcess(%d) = false — current process should be alive", pid)
	}
}

// ── 13. isAliveProcess returns false for a non-existent PID ──────────────────

func TestIsAliveProcessReturnsFalseForNonExistentPID(t *testing.T) {
	t.Parallel()

	// PID 9_999_999 is far above the Linux max PID (typically 4_194_304 on modern
	// kernels). kill(pid, 0) for such a PID always returns ESRCH.
	const impossiblePID = 9_999_999
	if defaultIsAliveProcess(impossiblePID) {
		t.Errorf("defaultIsAliveProcess(%d) = true — PID above OS max should be dead/absent", impossiblePID)
	}
}

// ── 14. Cluster name uniqueness across many synthetic calls ──────────────────

func TestClusterNameUniquenessAcrossManyStates(t *testing.T) {
	t.Parallel()

	const count = 20
	names := make(map[string]int, count)
	roots := make(map[string]int, count)

	for i := 0; i < count; i++ {
		state, err := newKindBootstrapState()
		if err != nil {
			t.Fatalf("newKindBootstrapState iteration %d: %v", i, err)
		}
		t.Cleanup(func(dir string) func() {
			return func() { _ = os.RemoveAll(dir) }
		}(state.SuiteRootDir))

		if prev, exists := names[state.ClusterName]; exists {
			t.Errorf("duplicate cluster name %q at iteration %d (first seen at %d) — "+
				"entropy is insufficient for intra-process uniqueness",
				state.ClusterName, i, prev)
		}
		names[state.ClusterName] = i

		if prev, exists := roots[state.SuiteRootDir]; exists {
			t.Errorf("duplicate suite root %q at iteration %d (first seen at %d)",
				state.SuiteRootDir, i, prev)
		}
		roots[state.SuiteRootDir] = i
	}
}

// ── 15. KUBECONFIG env var reset prevents stale inheritance ──────────────────

// TestResetSuiteEnvironmentPreventsKubeconfigInheritance verifies that
// resetSuiteInvocationEnvironment clears KUBECONFIG before a new invocation
// can call newKindBootstrapState, preventing stale cluster references from
// leaking across concurrent or sequential go test runs.
func TestResetSuiteEnvironmentPreventsKubeconfigInheritance(t *testing.T) {
	// Not parallel: manipulates process-wide env vars.

	// Simulate a stale kubeconfig left by a previous invocation.
	t.Setenv("KUBECONFIG", "/tmp/pillar-csi-stale-kubeconfig")
	t.Setenv("KIND_CLUSTER", "pillar-csi-e2e-stale-cluster")
	t.Setenv(suiteRootEnvVar, "/tmp/pillar-csi-stale-suite-root")
	t.Setenv(suiteWorkspaceEnvVar, "/tmp/stale-workspace")
	t.Setenv(suiteLogsEnvVar, "/tmp/stale-logs")
	t.Setenv(suiteGeneratedEnvVar, "/tmp/stale-generated")
	t.Setenv(suiteContextEnvVar, "kind-stale-cluster")

	if err := resetSuiteInvocationEnvironment(); err != nil {
		t.Fatalf("resetSuiteInvocationEnvironment: %v", err)
	}

	// All suite-owned env vars must be cleared.
	for _, key := range suiteOwnedClusterEnvVars {
		if val, ok := os.LookupEnv(key); ok && val != "" {
			t.Errorf("env var %s = %q after reset — stale value not cleared", key, val)
		}
	}
}

// ── 16. Two simulated processes produce non-colliding cluster names ───────────

// TestSimulatedConcurrentProcessesHaveDistinctClusters simulates two processes
// with different PIDs building their cluster names and verifies that the names
// cannot collide even with the same random entropy (they encode different PIDs).
func TestSimulatedConcurrentProcessesHaveDistinctClusters(t *testing.T) {
	t.Parallel()

	// Two simulated invocations with different PIDs but the same entropy string.
	// In practice entropy is random, but this exercises the PID-uniqueness guarantee.
	const (
		pid1    = 11111
		pid2    = 22222
		entropy = "abcd1234"
	)

	name1 := dnsLabel("pillar", "csi", "e2e", fmt.Sprintf("p%d", pid1), entropy)
	name2 := dnsLabel("pillar", "csi", "e2e", fmt.Sprintf("p%d", pid2), entropy)

	if name1 == name2 {
		t.Errorf("cluster names are equal for different PIDs %d and %d: %q — "+
			"PID embedding does not provide uniqueness",
			pid1, pid2, name1)
	}

	// Both names must contain the respective PID token.
	if !strings.Contains(name1, fmt.Sprintf("p%d", pid1)) {
		t.Errorf("name1 %q does not contain PID token p%d", name1, pid1)
	}
	if !strings.Contains(name2, fmt.Sprintf("p%d", pid2)) {
		t.Errorf("name2 %q does not contain PID token p%d", name2, pid2)
	}
}
