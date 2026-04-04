package e2e

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func TestKindBootstrapCreateClusterUsesKindKubeconfigAndContext(t *testing.T) {
	t.Parallel()

	suitePaths := newTestSuiteTempPaths(t)
	state := &kindBootstrapState{
		SuiteRootDir:   suitePaths.RootDir,
		WorkspaceDir:   suitePaths.WorkspaceDir,
		LogsDir:        suitePaths.LogsDir,
		GeneratedDir:   suitePaths.GeneratedDir,
		ClusterName:    "pillar-csi-e2e-p1234-abcd1234",
		KubeconfigPath: suitePaths.KubeconfigPath(),
		KindBinary:     "kind",
		KubectlBinary:  "kubectl",
		CreateTimeout:  2 * time.Minute,
		DeleteTimeout:  2 * time.Minute,
	}

	// SSOT compliance: createCluster now writes a kind-config.yaml to GeneratedDir
	// and passes --config <path> to kind create cluster.  The config path is
	// deterministic: GeneratedDir/kind-config.yaml.
	configPath := filepath.Join(suitePaths.GeneratedDir, "kind-config.yaml")

	fakeRunner := &fakeCommandRunner{
		t: t,
		outputs: map[string]fakeCommandResult{
			"kind create cluster --name pillar-csi-e2e-p1234-abcd1234 --kubeconfig " + state.KubeconfigPath + " --wait 2m0s --config " + configPath: {},
			"kubectl config current-context --kubeconfig " + state.KubeconfigPath: {
				stdout: "kind-pillar-csi-e2e-p1234-abcd1234\n",
			},
		},
	}

	if err := state.createCluster(context.Background(), fakeRunner); err != nil {
		t.Fatalf("createCluster: %v", err)
	}

	if state.KubeContext != "kind-"+state.ClusterName {
		t.Fatalf("KubeContext = %q, want %q", state.KubeContext, "kind-"+state.ClusterName)
	}

	wantCalls := []commandSpec{
		{
			Name: "kind",
			Args: []string{
				"create", "cluster",
				"--name", state.ClusterName,
				"--kubeconfig", state.KubeconfigPath,
				"--wait", "2m0s",
				"--config", configPath,
			},
		},
		{
			Name: "kubectl",
			Args: []string{
				"config", "current-context",
				"--kubeconfig", state.KubeconfigPath,
			},
		},
	}
	if !reflect.DeepEqual(fakeRunner.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", fakeRunner.calls, wantCalls)
	}
}

// TestKindClusterConfigYAMLWritesFile verifies that kindClusterConfigYAML
// writes a valid YAML file under the given generatedDir.
func TestKindClusterConfigYAMLWritesFile(t *testing.T) {
	t.Parallel()

	suitePaths := newTestSuiteTempPaths(t)

	configPath, err := kindClusterConfigYAML(suitePaths.GeneratedDir)
	if err != nil {
		t.Fatalf("kindClusterConfigYAML: %v", err)
	}

	// Config file must exist in GeneratedDir.
	wantPath := filepath.Join(suitePaths.GeneratedDir, "kind-config.yaml")
	if configPath != wantPath {
		t.Errorf("configPath = %q, want %q", configPath, wantPath)
	}

	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("read kind-config.yaml: %v", readErr)
	}
	content := string(data)

	// Must start with the Kind cluster header.
	if !strings.Contains(content, "kind: Cluster") {
		t.Errorf("kind-config.yaml missing 'kind: Cluster':\n%s", content)
	}
	if !strings.Contains(content, "apiVersion: kind.x-k8s.io/v1alpha4") {
		t.Errorf("kind-config.yaml missing apiVersion:\n%s", content)
	}
}

// TestKindClusterConfigYAMLExtraMountsForExistingPaths verifies that
// extraMounts entries are generated only for host paths that exist.
func TestKindClusterConfigYAMLExtraMountsForExistingPaths(t *testing.T) {
	t.Parallel()

	// /tmp always exists; use it as a stand-in for a "device that exists".
	suitePaths := newTestSuiteTempPaths(t)

	configPath, err := kindClusterConfigYAML(suitePaths.GeneratedDir)
	if err != nil {
		t.Fatalf("kindClusterConfigYAML: %v", err)
	}
	data, _ := os.ReadFile(configPath)
	content := string(data)

	// If /dev/mapper exists on this machine, the config must include it.
	if _, statErr := os.Stat("/dev/mapper"); statErr == nil {
		if !strings.Contains(content, "/dev/mapper") {
			t.Errorf("kind-config.yaml must include /dev/mapper extraMount (path exists):\n%s", content)
		}
		if !strings.Contains(content, "Bidirectional") {
			t.Errorf("kind-config.yaml /dev/mapper mount must use Bidirectional propagation:\n%s", content)
		}
	}

	// If /dev/nvme-fabrics exists, it must be present with HostToContainer.
	if _, statErr := os.Stat("/dev/nvme-fabrics"); statErr == nil {
		if !strings.Contains(content, "/dev/nvme-fabrics") {
			t.Errorf("kind-config.yaml must include /dev/nvme-fabrics extraMount (path exists):\n%s", content)
		}
		if !strings.Contains(content, "HostToContainer") {
			t.Errorf("kind-config.yaml /dev/nvme-fabrics mount must use HostToContainer propagation:\n%s", content)
		}
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
