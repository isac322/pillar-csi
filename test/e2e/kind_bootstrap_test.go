package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCommandRunner struct {
	t       testing.TB
	outputs map[string]fakeCommandResult
	calls   []commandSpec
}

type fakeCommandResult struct {
	stdout string
	err    error
}

func (f *fakeCommandRunner) Run(_ context.Context, cmd commandSpec) (string, error) {
	f.calls = append(f.calls, cmd)

	result, ok := f.outputs[cmd.String()]
	if !ok {
		f.t.Fatalf("unexpected command: %s", cmd.String())
	}
	return result.stdout, result.err
}

func newTestSuiteTempPaths(t testing.TB) *suiteTempPaths {
	t.Helper()

	paths, err := newSuiteTempPaths()
	if err != nil {
		t.Fatalf("newSuiteTempPaths: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(paths.RootDir)
	})
	return paths
}

func TestNewKindBootstrapStateCreatesUniqueTmpScopedArtifacts(t *testing.T) {
	t.Parallel()

	left, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("newKindBootstrapState left: %v", err)
	}
	right, err := newKindBootstrapState()
	if err != nil {
		t.Fatalf("newKindBootstrapState right: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(left.SuiteRootDir)
		_ = os.RemoveAll(right.SuiteRootDir)
	})

	if left.SuiteRootDir == right.SuiteRootDir {
		t.Fatal("expected unique suite root directories")
	}
	if left.ClusterName == right.ClusterName {
		t.Fatal("expected unique kind cluster names")
	}
	if got := left.WorkspaceDir; got != filepath.Join(left.SuiteRootDir, "workspace") {
		t.Fatalf("left workspace dir = %q, want %q", got, filepath.Join(left.SuiteRootDir, "workspace"))
	}
	if got := left.LogsDir; got != filepath.Join(left.SuiteRootDir, "logs") {
		t.Fatalf("left logs dir = %q, want %q", got, filepath.Join(left.SuiteRootDir, "logs"))
	}
	if got := left.GeneratedDir; got != filepath.Join(left.SuiteRootDir, "generated") {
		t.Fatalf("left generated dir = %q, want %q", got, filepath.Join(left.SuiteRootDir, "generated"))
	}
	if got := filepath.Dir(left.KubeconfigPath); got != left.GeneratedDir {
		t.Fatalf("left kubeconfig dir = %q, want %q", got, left.GeneratedDir)
	}
	if got := filepath.Dir(right.KubeconfigPath); got != right.GeneratedDir {
		t.Fatalf("right kubeconfig dir = %q, want %q", got, right.GeneratedDir)
	}
	if filepath.Base(left.KubeconfigPath) != "kubeconfig" {
		t.Fatalf("left kubeconfig base = %q, want kubeconfig", filepath.Base(left.KubeconfigPath))
	}
	if filepath.Base(right.KubeconfigPath) != "kubeconfig" {
		t.Fatalf("right kubeconfig base = %q, want kubeconfig", filepath.Base(right.KubeconfigPath))
	}
}

func TestKindBootstrapNFSPreparationFailureCleansOwnedCluster(t *testing.T) {
	t.Setenv("E2E_NFS_E2E", " TrUe ")
	cause := errors.New("sysfs unavailable")
	for _, tc := range []struct {
		name        string
		nodeSuffix  string
		operation   string
		result      fakeCommandResult
		deleteFails bool
	}{
		{name: "first worker remount", nodeSuffix: "-worker", operation: "mount -o remount,rw /sys", result: fakeCommandResult{err: cause}},
		{name: "second worker remount", nodeSuffix: "-worker2", operation: "mount -o remount,rw /sys", result: fakeCommandResult{err: cause}},
		{name: "readback failure", nodeSuffix: "-worker2", operation: "findmnt -no OPTIONS /sys", result: fakeCommandResult{err: cause}},
		{name: "still read-only", nodeSuffix: "-worker2", operation: "findmnt -no OPTIONS /sys", result: fakeCommandResult{stdout: "ro,nosuid,nodev,noexec\n"}},
		{name: "rw substring is not writable", nodeSuffix: "-worker2", operation: "findmnt -no OPTIONS /sys", result: fakeCommandResult{stdout: "ro,rwfoo\n"}},
		{name: "empty readback", nodeSuffix: "-worker2", operation: "findmnt -no OPTIONS /sys", result: fakeCommandResult{}},
		{name: "failed deletion retains ownership", nodeSuffix: "-worker2", operation: "mount -o remount,rw /sys", result: fakeCommandResult{err: cause}, deleteFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newValidKindBootstrapState(t)
			state.clusterCreated = false
			state.KubeContext = ""
			outputs := map[string]fakeCommandResult{
				"kind create cluster --name " + state.ClusterName + " --kubeconfig " + state.KubeconfigPath + " --wait " + state.CreateTimeout.String() + " --config " + filepath.Join(state.GeneratedDir, "nfs-kind.yaml"): {},
				"kind delete cluster --name " + state.ClusterName: {},
			}
			for _, suffix := range []string{"-worker", "-worker2"} {
				node := state.ClusterName + suffix
				outputs["docker exec "+node+" mount -o remount,rw /sys"] = fakeCommandResult{}
				outputs["docker exec "+node+" findmnt -no OPTIONS /sys"] = fakeCommandResult{stdout: "rw,nosuid,nodev,noexec,relatime\n"}
			}
			node := state.ClusterName + tc.nodeSuffix
			outputs["docker exec "+node+" "+tc.operation] = tc.result
			if tc.deleteFails {
				outputs["kind delete cluster --name "+state.ClusterName] = fakeCommandResult{err: errors.New("delete failed")}
			}
			runner := &fakeCommandRunner{t: t, outputs: outputs}

			err := state.createCluster(context.Background(), runner)
			if err == nil {
				t.Fatal("createCluster accepted failed NFS sysfs preparation")
			}
			if tc.result.err != nil && !errors.Is(err, tc.result.err) {
				t.Fatalf("createCluster error = %v, want cause %v", err, tc.result.err)
			}
			if !strings.Contains(err.Error(), node) || !strings.Contains(err.Error(), "/sys") {
				t.Fatalf("createCluster error does not identify node and sysfs target: %v", err)
			}
			if state.clusterCreated != tc.deleteFails {
				t.Fatalf("clusterCreated = %v after cleanup; deletion failed = %v", state.clusterCreated, tc.deleteFails)
			}
			if state.KubeContext != "" {
				t.Fatalf("bootstrap published context despite sysfs preparation failure: %q", state.KubeContext)
			}
			if _, err := os.Stat(state.SuiteRootDir); !os.IsNotExist(err) {
				t.Fatalf("suite root still exists or returned unexpected error: %v", err)
			}
		})
	}
}

func TestKindBootstrapExportEnvironmentPublishesClusterContext(t *testing.T) {
	suitePaths := newTestSuiteTempPaths(t)
	state := &kindBootstrapState{
		SuiteRootDir:   suitePaths.RootDir,
		WorkspaceDir:   suitePaths.WorkspaceDir,
		LogsDir:        suitePaths.LogsDir,
		GeneratedDir:   suitePaths.GeneratedDir,
		ClusterName:    "pillar-csi-e2e-p1234-abcd1234",
		KubeconfigPath: suitePaths.KubeconfigPath(),
		KubeContext:    "kind-pillar-csi-e2e-p1234-abcd1234",
		KindBinary:     "kind",
		KubectlBinary:  "kubectl",
		CreateTimeout:  time.Second,
		DeleteTimeout:  time.Second,
	}

	for _, key := range []string{
		"KUBECONFIG",
		"KIND_CLUSTER",
		suiteRootEnvVar,
		suiteWorkspaceEnvVar,
		suiteLogsEnvVar,
		suiteGeneratedEnvVar,
		suiteContextEnvVar,
	} {
		t.Setenv(key, "")
	}

	if err := state.exportEnvironment(); err != nil {
		t.Fatalf("exportEnvironment: %v", err)
	}

	if got := os.Getenv("KUBECONFIG"); got != state.KubeconfigPath {
		t.Fatalf("KUBECONFIG = %q, want %q", got, state.KubeconfigPath)
	}
	if got := os.Getenv("KIND_CLUSTER"); got != state.ClusterName {
		t.Fatalf("KIND_CLUSTER = %q, want %q", got, state.ClusterName)
	}
	if got := os.Getenv(suiteRootEnvVar); got != state.SuiteRootDir {
		t.Fatalf("%s = %q, want %q", suiteRootEnvVar, got, state.SuiteRootDir)
	}
	if got := os.Getenv(suiteWorkspaceEnvVar); got != state.WorkspaceDir {
		t.Fatalf("%s = %q, want %q", suiteWorkspaceEnvVar, got, state.WorkspaceDir)
	}
	if got := os.Getenv(suiteLogsEnvVar); got != state.LogsDir {
		t.Fatalf("%s = %q, want %q", suiteLogsEnvVar, got, state.LogsDir)
	}
	if got := os.Getenv(suiteGeneratedEnvVar); got != state.GeneratedDir {
		t.Fatalf("%s = %q, want %q", suiteGeneratedEnvVar, got, state.GeneratedDir)
	}
	if got := os.Getenv(suiteContextEnvVar); got != state.KubeContext {
		t.Fatalf("%s = %q, want %q", suiteContextEnvVar, got, state.KubeContext)
	}
}
