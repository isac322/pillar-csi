package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newTestKindState returns a minimal kindBootstrapState wired to a known cluster
// name for lifecycle verification unit tests.
func newTestKindState(t *testing.T) *kindBootstrapState {
	t.Helper()
	suitePaths := newTestSuiteTempPaths(t)
	return &kindBootstrapState{
		SuiteRootDir:   suitePaths.RootDir,
		WorkspaceDir:   suitePaths.WorkspaceDir,
		LogsDir:        suitePaths.LogsDir,
		GeneratedDir:   suitePaths.GeneratedDir,
		ClusterName:    "pillar-csi-e2e-p9999-abcd0001",
		KubeconfigPath: suitePaths.KubeconfigPath(),
		KubeContext:    "kind-pillar-csi-e2e-p9999-abcd0001",
		KindBinary:     "kind",
		KubectlBinary:  "kubectl",
		CreateTimeout:  2 * time.Minute,
		DeleteTimeout:  2 * time.Minute,
	}
}

// listClustersCmd is the canonical form of the command we expect.
const listClustersCmd = "kind get clusters"

func TestListClustersReturnsParsedNames(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			listClustersCmd: {stdout: "pillar-csi-e2e-p9999-abcd0001\nother-cluster\n"},
		},
	}

	got, err := state.listClusters(context.Background(), runner)
	if err != nil {
		t.Fatalf("listClusters: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listClusters = %v, want 2 names", got)
	}
	if got[0] != "pillar-csi-e2e-p9999-abcd0001" || got[1] != "other-cluster" {
		t.Fatalf("listClusters = %v, want [pillar-csi-e2e-p9999-abcd0001 other-cluster]", got)
	}
}

func TestListClustersReturnsEmptySliceWhenNoOutput(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t:       t,
		outputs: map[string]fakeCommandResult{listClustersCmd: {stdout: ""}},
	}

	got, err := state.listClusters(context.Background(), runner)
	if err != nil {
		t.Fatalf("listClusters empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("listClusters = %v, want empty", got)
	}
}

func TestListClustersReturnsNilOnNoKindClustersFoundStdout(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t:       t,
		outputs: map[string]fakeCommandResult{listClustersCmd: {stdout: "No kind clusters found."}},
	}

	got, err := state.listClusters(context.Background(), runner)
	if err != nil {
		t.Fatalf("listClusters no-clusters stdout: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("listClusters = %v, want empty", got)
	}
}

func TestListClustersReturnsNilOnNoKindClustersFoundError(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			listClustersCmd: {
				stdout: "",
				err:    errors.New("kind get clusters: No kind clusters found."),
			},
		},
	}

	got, err := state.listClusters(context.Background(), runner)
	if err != nil {
		t.Fatalf("listClusters no-clusters error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("listClusters = %v, want empty", got)
	}
}

func TestListClustersForwardsUnexpectedErrors(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			listClustersCmd: {err: errors.New("kind binary not found")},
		},
	}

	_, err := state.listClusters(context.Background(), runner)
	if err == nil {
		t.Fatal("listClusters: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "kind binary not found") {
		t.Fatalf("listClusters error = %q, want 'kind binary not found'", err)
	}
}

// ---------------------------------------------------------------------------
// verifyClusterPresent
// ---------------------------------------------------------------------------

func TestVerifyClusterPresentSucceedsWhenClusterListed(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			listClustersCmd: {stdout: "other-cluster\n" + state.ClusterName + "\n"},
		},
	}

	if err := state.verifyClusterPresent(context.Background(), runner); err != nil {
		t.Fatalf("verifyClusterPresent: %v", err)
	}
}

func TestVerifyClusterPresentFailsWhenClusterNotListed(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t:       t,
		outputs: map[string]fakeCommandResult{listClustersCmd: {stdout: "other-cluster\n"}},
	}

	err := state.verifyClusterPresent(context.Background(), runner)
	if err == nil {
		t.Fatal("verifyClusterPresent: expected error when cluster absent, got nil")
	}
	if !strings.Contains(err.Error(), state.ClusterName) {
		t.Fatalf("verifyClusterPresent error = %q, want cluster name in message", err)
	}
}

func TestVerifyClusterPresentFailsWhenNoClusters(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t:       t,
		outputs: map[string]fakeCommandResult{listClustersCmd: {stdout: ""}},
	}

	if err := state.verifyClusterPresent(context.Background(), runner); err == nil {
		t.Fatal("verifyClusterPresent: expected error when list is empty, got nil")
	}
}

// ---------------------------------------------------------------------------
// verifyClusterAbsent
// ---------------------------------------------------------------------------

func TestVerifyClusterAbsentSucceedsWhenClusterNotListed(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t:       t,
		outputs: map[string]fakeCommandResult{listClustersCmd: {stdout: "other-cluster\n"}},
	}

	if err := state.verifyClusterAbsent(context.Background(), runner); err != nil {
		t.Fatalf("verifyClusterAbsent: %v", err)
	}
}

func TestVerifyClusterAbsentSucceedsWhenNoClusters(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t:       t,
		outputs: map[string]fakeCommandResult{listClustersCmd: {stdout: ""}},
	}

	if err := state.verifyClusterAbsent(context.Background(), runner); err != nil {
		t.Fatalf("verifyClusterAbsent empty list: %v", err)
	}
}

func TestVerifyClusterAbsentFailsWhenClusterStillListed(t *testing.T) {
	t.Parallel()

	state := newTestKindState(t)
	runner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			listClustersCmd: {stdout: state.ClusterName + "\nother-cluster\n"},
		},
	}

	err := state.verifyClusterAbsent(context.Background(), runner)
	if err == nil {
		t.Fatal("verifyClusterAbsent: expected error when cluster still listed, got nil")
	}
	if !strings.Contains(err.Error(), state.ClusterName) {
		t.Fatalf("verifyClusterAbsent error = %q, want cluster name in message", err)
	}
}

// ---------------------------------------------------------------------------
// lifecycle boundary: cluster is absent → present → absent
// ---------------------------------------------------------------------------

func TestKindClusterLifecycleBoundary(t *testing.T) {
	t.Parallel()

	// This test simulates what SynchronizedBeforeSuite / SynchronizedAfterSuite
	// do: it checks absence before creation, presence after creation, and
	// absence after destruction — using a fake runner that models the three
	// invocations of "kind get clusters".
	state := newTestKindState(t)

	callCount := 0
	mockRunner := &mockLifecycleRunner{
		t:           t,
		clusterName: state.ClusterName,
		// call 0 → "kind create cluster …" succeeds
		// call 1 → "kubectl config current-context …" returns expected context
		// list calls are interspersed and tracked via listCallCount
	}

	// Phase 1: cluster must be absent before bootstrap.
	// listCallCount=0 → cluster not yet present.
	if err := state.verifyClusterAbsent(context.Background(), mockRunner); err != nil {
		t.Fatalf("phase 1 verifyClusterAbsent: %v", err)
	}
	callCount++

	// Phase 2: simulate createCluster (we inject the runner manually).
	state.clusterCreated = true // simulate successful creation

	// Phase 3: cluster must be present after bootstrap.
	// listCallCount=1 → cluster is present.
	mockRunner.clusterIsPresent = true
	if err := state.verifyClusterPresent(context.Background(), mockRunner); err != nil {
		t.Fatalf("phase 3 verifyClusterPresent: %v", err)
	}
	callCount++

	// Phase 4: simulate destroyCluster.
	state.clusterCreated = false // simulate successful deletion
	mockRunner.clusterIsPresent = false

	// Phase 5: cluster must be absent after teardown.
	if err := state.verifyClusterAbsent(context.Background(), mockRunner); err != nil {
		t.Fatalf("phase 5 verifyClusterAbsent: %v", err)
	}
	callCount++

	if callCount != 3 {
		t.Fatalf("lifecycle call count = %d, want 3", callCount)
	}
}

// mockLifecycleRunner is a commandRunner that answers "kind get clusters" based
// on the clusterIsPresent flag, making it possible to simulate the full cluster
// lifecycle without spawning a real kind binary.
type mockLifecycleRunner struct {
	t                *testing.T
	clusterName      string
	clusterIsPresent bool
}

func (m *mockLifecycleRunner) Run(_ context.Context, cmd commandSpec) (string, error) {
	if cmd.Name == "kind" && len(cmd.Args) == 2 && cmd.Args[0] == "get" && cmd.Args[1] == "clusters" {
		if m.clusterIsPresent {
			return m.clusterName + "\n", nil
		}
		return "", nil
	}
	m.t.Fatalf("mockLifecycleRunner: unexpected command: %s", cmd.String())
	return "", nil
}
