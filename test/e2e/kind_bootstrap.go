package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultKindBinary    = "kind"
	defaultKubectlBinary = "kubectl"

	suiteRootEnvVar    = "PILLAR_CSI_E2E_SUITE_ROOT"
	suiteContextEnvVar = "PILLAR_CSI_E2E_KUBE_CONTEXT"
)

var (
	e2eKindBinaryFlag = flag.String(
		"e2e.kind-binary",
		envOrDefault("KIND", defaultKindBinary),
		"path to the kind binary for //go:build e2e cluster bootstrap",
	)
	e2eKubectlBinaryFlag = flag.String(
		"e2e.kubectl-binary",
		envOrDefault("KUBECTL", defaultKubectlBinary),
		"path to the kubectl binary for //go:build e2e cluster bootstrap",
	)
	e2eKindWaitFlag = flag.Duration(
		"e2e.kind-wait",
		2*time.Minute,
		"maximum wait for kind create cluster during //go:build e2e bootstrap",
	)
	e2eKindDeleteTimeoutFlag = flag.Duration(
		"e2e.kind-delete-timeout",
		2*time.Minute,
		"maximum wait for kind delete cluster during //go:build e2e teardown",
	)
)

type commandSpec struct {
	Name string
	Args []string
}

func (c commandSpec) String() string {
	if len(c.Args) == 0 {
		return c.Name
	}
	return c.Name + " " + strings.Join(c.Args, " ")
}

type commandRunner interface {
	Run(ctx context.Context, cmd commandSpec) (string, error)
}

type execCommandRunner struct {
	Output io.Writer
}

func (r execCommandRunner) Run(ctx context.Context, spec commandSpec) (string, error) {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...) //nolint:gosec

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	output := r.Output
	if output == nil {
		output = io.Discard
	}

	cmd.Stdout = io.MultiWriter(output, &stdout)
	cmd.Stderr = io.MultiWriter(output, &stderr)
	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(stderr.String())
		if errText == "" {
			errText = strings.TrimSpace(stdout.String())
		}
		if errText == "" {
			errText = err.Error()
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s: %s", spec.String(), errText)
	}

	return strings.TrimSpace(stdout.String()), nil
}

type kindBootstrapState struct {
	SuiteRootDir   string        `json:"suiteRootDir"`
	WorkspaceDir   string        `json:"workspaceDir"`
	LogsDir        string        `json:"logsDir"`
	GeneratedDir   string        `json:"generatedDir"`
	ClusterName    string        `json:"clusterName"`
	KubeconfigPath string        `json:"kubeconfigPath"`
	KubeContext    string        `json:"kubeContext"`
	KindBinary     string        `json:"kindBinary"`
	KubectlBinary  string        `json:"kubectlBinary"`
	CreateTimeout  time.Duration `json:"createTimeout"`
	DeleteTimeout  time.Duration `json:"deleteTimeout"`

	clusterCreated bool
}

func newKindBootstrapState() (*kindBootstrapState, error) {
	suitePaths, err := newSuiteTempPaths()
	if err != nil {
		return nil, err
	}

	clusterEntropy, err := randomHex(4)
	if err != nil {
		_ = os.RemoveAll(suitePaths.RootDir)
		return nil, fmt.Errorf("generate kind cluster suffix: %w", err)
	}

	clusterName := dnsLabel("pillar", "csi", "e2e", fmt.Sprintf("p%d", os.Getpid()), clusterEntropy)
	return &kindBootstrapState{
		SuiteRootDir:   suitePaths.RootDir,
		WorkspaceDir:   suitePaths.WorkspaceDir,
		LogsDir:        suitePaths.LogsDir,
		GeneratedDir:   suitePaths.GeneratedDir,
		ClusterName:    clusterName,
		KubeconfigPath: suitePaths.KubeconfigPath(),
		KindBinary:     strings.TrimSpace(*e2eKindBinaryFlag),
		KubectlBinary:  strings.TrimSpace(*e2eKubectlBinaryFlag),
		CreateTimeout:  *e2eKindWaitFlag,
		DeleteTimeout:  *e2eKindDeleteTimeoutFlag,
	}, nil
}

func decodeKindBootstrapState(data []byte) (*kindBootstrapState, error) {
	if len(data) == 0 {
		return nil, errors.New("kind bootstrap payload is empty")
	}

	var state kindBootstrapState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode kind bootstrap payload: %w", err)
	}
	return &state, nil
}

func (s *kindBootstrapState) encode() ([]byte, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode kind bootstrap state: %w", err)
	}
	return payload, nil
}

// kindClusterConfigYAML generates a Kind cluster configuration YAML that
// satisfies the SSOT requirements in docs/testing/infra/KIND.md §2 (리소스 생성).
//
// # ExtraMount strategy
//
// The SSOT mandates bind-mounting three host paths into every Kind node
// container so that storage subsystems work inside the container:
//
//	/dev/mapper          → Bidirectional    (LVM device-mapper)
//	/dev/nvme-fabrics    → HostToContainer  (NVMe-oF fabric driver)
//	/sys/kernel/config   → Bidirectional    (configfs: NVMe-oF + iSCSI)
//
// Each mount is included ONLY when its host path actually exists, so that
// cluster creation succeeds on development machines that do not have every
// storage subsystem configured. CI always has all three paths because
// CI-HOST-SETUP.md loads the required kernel modules in a dedicated step
// before `make test-e2e`.
//
// When none of the paths exist (e.g., in CI-less unit-test environments),
// the function returns a minimal single-node config without extraMounts.
//
// # Node topology
//
// The SSOT specifies 1 control-plane node for the pillar-csi E2E suite.
// Worker nodes are not provisioned here because backend resources (ZFS pools,
// LVM VGs) are only set up on the control-plane container, and the CSI
// DaemonSet relies on those resources being present on the node it runs on.
// A multi-node topology upgrade is tracked separately.
func kindClusterConfigYAML(generatedDir string) (configPath string, err error) {
	type mountSpec struct {
		hostPath      string
		containerPath string
		propagation   string
	}

	// SSOT-mandated extraMounts (docs/testing/infra/KIND.md §2).
	candidates := []mountSpec{
		{"/dev/mapper", "/dev/mapper", "Bidirectional"},
		{"/dev/nvme-fabrics", "/dev/nvme-fabrics", "HostToContainer"},
		{"/sys/kernel/config", "/sys/kernel/config", "Bidirectional"},
	}

	var mountsYAML strings.Builder
	for _, m := range candidates {
		if _, statErr := os.Stat(m.hostPath); statErr != nil {
			// Host path absent — skip this mount to avoid cluster-creation failure.
			continue
		}
		fmt.Fprintf(&mountsYAML,
			"  - hostPath: %s\n    containerPath: %s\n    propagation: %s\n",
			m.hostPath, m.containerPath, m.propagation,
		)
	}

	var cfg strings.Builder
	cfg.WriteString("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\n")
	if mountsYAML.Len() > 0 {
		cfg.WriteString("nodes:\n- role: control-plane\n  extraMounts:\n")
		cfg.WriteString(mountsYAML.String())
	}

	configPath = filepath.Join(generatedDir, "kind-config.yaml")
	if writeErr := os.WriteFile(configPath, []byte(cfg.String()), 0o644); writeErr != nil {
		return "", fmt.Errorf("write kind cluster config %q: %w", configPath, writeErr)
	}
	return configPath, nil
}

func (s *kindBootstrapState) createCluster(ctx context.Context, runner commandRunner) (err error) {
	if s == nil {
		return errors.New("kind bootstrap state is nil")
	}
	if err := s.validate(); err != nil {
		return err
	}
	if runner == nil {
		return errors.New("kind bootstrap runner is nil")
	}

	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), s.DeleteTimeout)
		defer cancel()
		_ = s.destroyCluster(cleanupCtx, runner)
	}()

	if err := os.MkdirAll(filepath.Dir(s.KubeconfigPath), 0o755); err != nil {
		return fmt.Errorf("create kubeconfig directory: %w", err)
	}

	// SSOT compliance: docs/testing/infra/KIND.md §2 mandates kind-config.yaml
	// with extraMounts for /dev/mapper, /dev/nvme-fabrics, /sys/kernel/config.
	// kindClusterConfigYAML writes the config file and returns its path.
	configPath, cfgErr := kindClusterConfigYAML(filepath.Dir(s.KubeconfigPath))
	if cfgErr != nil {
		return fmt.Errorf("generate kind cluster config: %w", cfgErr)
	}

	_, err = runner.Run(ctx, commandSpec{
		Name: s.KindBinary,
		Args: []string{
			"create", "cluster",
			"--name", s.ClusterName,
			"--kubeconfig", s.KubeconfigPath,
			"--wait", s.CreateTimeout.String(),
			"--config", configPath,
		},
	})
	if err != nil {
		return err
	}
	s.clusterCreated = true

	contextName, err := runner.Run(ctx, commandSpec{
		Name: s.KubectlBinary,
		Args: []string{
			"config", "current-context",
			"--kubeconfig", s.KubeconfigPath,
		},
	})
	if err != nil {
		return err
	}

	s.KubeContext = strings.TrimSpace(contextName)
	expectedContext := "kind-" + s.ClusterName
	if s.KubeContext != expectedContext {
		return fmt.Errorf("kubectl current-context=%q, want %q", s.KubeContext, expectedContext)
	}

	return nil
}

func (s *kindBootstrapState) destroyCluster(ctx context.Context, runner commandRunner) error {
	if s == nil {
		return nil
	}
	if runner == nil {
		return errors.New("kind bootstrap runner is nil")
	}

	var errs []error
	if s.clusterCreated && strings.TrimSpace(s.ClusterName) != "" && strings.TrimSpace(s.KindBinary) != "" {
		if _, err := runner.Run(ctx, commandSpec{
			Name: s.KindBinary,
			Args: []string{"delete", "cluster", "--name", s.ClusterName},
		}); err != nil {
			errs = append(errs, err)
		} else {
			s.clusterCreated = false
		}
	}
	if strings.TrimSpace(s.SuiteRootDir) != "" {
		if err := os.RemoveAll(s.SuiteRootDir); err != nil {
			errs = append(errs, fmt.Errorf("remove suite root %q: %w", s.SuiteRootDir, err))
		}
	}
	return errors.Join(errs...)
}

func (s *kindBootstrapState) exportEnvironment() error {
	if s == nil {
		return errors.New("kind bootstrap state is nil")
	}
	if err := s.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.KubeContext) == "" {
		return errors.New("kind kube context is required")
	}

	exports := map[string]string{
		"KUBECONFIG":         s.KubeconfigPath,
		"KIND_CLUSTER":       s.ClusterName,
		suiteRootEnvVar:      s.SuiteRootDir,
		suiteWorkspaceEnvVar: s.WorkspaceDir,
		suiteLogsEnvVar:      s.LogsDir,
		suiteGeneratedEnvVar: s.GeneratedDir,
		suiteContextEnvVar:   s.KubeContext,
	}
	for key, value := range exports {
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("export %s: %w", key, err)
		}
	}
	return nil
}

func (s *kindBootstrapState) validate() error {
	if s == nil {
		return errors.New("kind bootstrap state is nil")
	}
	if err := s.suiteTempPaths().validate(); err != nil {
		return fmt.Errorf("kind suite paths: %w", err)
	}
	if strings.TrimSpace(s.ClusterName) == "" {
		return errors.New("kind cluster name is required")
	}
	if strings.TrimSpace(s.KubeconfigPath) == "" {
		return errors.New("kind kubeconfig path is required")
	}
	if filepath.Clean(s.GeneratedDir) == filepath.Clean(s.KubeconfigPath) {
		return fmt.Errorf("kind kubeconfig %q must be a file under %s", s.KubeconfigPath, s.GeneratedDir)
	}
	if !pathWithinRoot(s.GeneratedDir, s.KubeconfigPath) {
		return fmt.Errorf("kind kubeconfig %q must stay under %s", s.KubeconfigPath, s.GeneratedDir)
	}
	if strings.TrimSpace(s.KindBinary) == "" {
		return errors.New("kind binary is required")
	}
	if strings.TrimSpace(s.KubectlBinary) == "" {
		return errors.New("kubectl binary is required")
	}
	if s.CreateTimeout <= 0 {
		return errors.New("kind create timeout must be positive")
	}
	if s.DeleteTimeout <= 0 {
		return errors.New("kind delete timeout must be positive")
	}
	return nil
}

func (s *kindBootstrapState) suiteTempPaths() *suiteTempPaths {
	if s == nil {
		return nil
	}
	return &suiteTempPaths{
		RootDir:      s.SuiteRootDir,
		WorkspaceDir: s.WorkspaceDir,
		LogsDir:      s.LogsDir,
		GeneratedDir: s.GeneratedDir,
	}
}

// kindBootstrapStateFromEnv reconstructs a kindBootstrapState from the
// environment variables exported by bootstrapSuiteCluster. It is called by
// SynchronizedBeforeSuite in ginkgo parallel workers (and in the sequential
// path) to obtain the cluster reference without creating a new cluster.
//
// Returns an error if any required env var is missing or the reconstructed
// state fails validation.
func kindBootstrapStateFromEnv() (*kindBootstrapState, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if strings.TrimSpace(kubeconfigPath) == "" {
		return nil, errors.New("KUBECONFIG env var not set — cluster not bootstrapped by TestMain")
	}
	clusterName := os.Getenv("KIND_CLUSTER")
	if strings.TrimSpace(clusterName) == "" {
		return nil, errors.New("KIND_CLUSTER env var not set — cluster not bootstrapped by TestMain")
	}
	kubeContext := os.Getenv(suiteContextEnvVar)
	if strings.TrimSpace(kubeContext) == "" {
		return nil, fmt.Errorf("%s env var not set — cluster not bootstrapped by TestMain", suiteContextEnvVar)
	}
	suiteRootDir := os.Getenv(suiteRootEnvVar)
	if strings.TrimSpace(suiteRootDir) == "" {
		return nil, fmt.Errorf("%s env var not set — cluster not bootstrapped by TestMain", suiteRootEnvVar)
	}
	workspaceDir := os.Getenv(suiteWorkspaceEnvVar)
	logsDir := os.Getenv(suiteLogsEnvVar)
	generatedDir := os.Getenv(suiteGeneratedEnvVar)

	state := &kindBootstrapState{
		SuiteRootDir:   suiteRootDir,
		WorkspaceDir:   workspaceDir,
		LogsDir:        logsDir,
		GeneratedDir:   generatedDir,
		ClusterName:    clusterName,
		KubeconfigPath: kubeconfigPath,
		KubeContext:    kubeContext,
		KindBinary:     envOrDefault("KIND", defaultKindBinary),
		KubectlBinary:  envOrDefault("KUBECTL", defaultKubectlBinary),
		CreateTimeout:  *e2eKindWaitFlag,
		DeleteTimeout:  *e2eKindDeleteTimeoutFlag,
		clusterCreated: true, // cluster exists; TestMain created it
	}
	if err := state.validate(); err != nil {
		return nil, fmt.Errorf("validate cluster state from env: %w", err)
	}
	return state, nil
}

func envOrDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func randomHex(byteCount int) (string, error) {
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Sub-AC 5.4: USE_EXISTING_CLUSTER support
// ─────────────────────────────────────────────────────────────────────────────

const (
	// useExistingClusterEnvVar skips Kind cluster creation and reuses an
	// existing cluster when set to "true" or "1".  The caller must also set:
	//   KUBECONFIG=<path>           — kubeconfig file for the existing cluster
	//   KIND_CLUSTER=<name>         — cluster name (used for image loading)
	//   PILLAR_CSI_E2E_KUBE_CONTEXT — kubectl context name (kind-<name>)
	//
	// Combine with E2E_SKIP_IMAGE_BUILD=true to reuse images from a previous run
	// and keep make test-e2e total wall-clock time well under 30 seconds when
	// the cluster and images are already live.
	//
	// Example workflow:
	//   # First run (creates cluster and images, takes ~90s):
	//   make test-e2e
	//
	//   # Subsequent runs reusing the cluster (takes ~15s):
	//   make test-e2e E2E_USE_EXISTING_CLUSTER=true E2E_SKIP_IMAGE_BUILD=true
	useExistingClusterEnvVar = "E2E_USE_EXISTING_CLUSTER"
)

// resolveUseExistingCluster returns true when E2E_USE_EXISTING_CLUSTER is set
// to "true" or "1", instructing bootstrapSuiteCluster to skip Kind cluster
// creation and read the cluster reference from the environment instead.
func resolveUseExistingCluster() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(useExistingClusterEnvVar)))
	return v == "true" || v == "1"
}

// existingClusterState reconstructs a kindBootstrapState for a cluster that
// was created in a previous test run (or by a separate `kind create cluster`
// invocation).  It reads the cluster connection details from the same
// environment variables that bootstrapSuiteCluster exports:
//
//	KUBECONFIG              — path to the kubeconfig file (required)
//	KIND_CLUSTER            — cluster name (required)
//	PILLAR_CSI_E2E_KUBE_CONTEXT — kubectl context name (required)
//
// The returned state has clusterCreated=false so that CleanupWithRunner does
// not attempt to delete a cluster that this test invocation does not own.
//
// Suite temp directories (workspace, logs, generated) are created fresh for
// each invocation so that per-run artifacts do not collide across runs.
func existingClusterState() (*kindBootstrapState, error) {
	kubeconfigPath := strings.TrimSpace(os.Getenv("KUBECONFIG"))
	if kubeconfigPath == "" {
		return nil, errors.New(
			"E2E_USE_EXISTING_CLUSTER=true requires KUBECONFIG to be set")
	}
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return nil, fmt.Errorf(
			"E2E_USE_EXISTING_CLUSTER: KUBECONFIG=%s not accessible: %w",
			kubeconfigPath, err)
	}

	clusterName := strings.TrimSpace(os.Getenv("KIND_CLUSTER"))
	if clusterName == "" {
		return nil, errors.New(
			"E2E_USE_EXISTING_CLUSTER=true requires KIND_CLUSTER to be set")
	}

	kubeContext := strings.TrimSpace(os.Getenv(suiteContextEnvVar))
	if kubeContext == "" {
		// Default to the kind-<name> convention if not overridden.
		kubeContext = "kind-" + clusterName
	}

	// Create fresh suite temp directories so per-run artifacts don't collide.
	suitePaths, err := newSuiteTempPaths()
	if err != nil {
		return nil, fmt.Errorf("E2E_USE_EXISTING_CLUSTER: create suite temp paths: %w", err)
	}

	state := &kindBootstrapState{
		SuiteRootDir:   suitePaths.RootDir,
		WorkspaceDir:   suitePaths.WorkspaceDir,
		LogsDir:        suitePaths.LogsDir,
		GeneratedDir:   suitePaths.GeneratedDir,
		ClusterName:    clusterName,
		KubeconfigPath: suitePaths.KubeconfigPath(),
		KubeContext:    kubeContext,
		KindBinary:     envOrDefault("KIND", defaultKindBinary),
		KubectlBinary:  envOrDefault("KUBECTL", defaultKubectlBinary),
		CreateTimeout:  *e2eKindWaitFlag,
		DeleteTimeout:  *e2eKindDeleteTimeoutFlag,
		// clusterCreated=false: this invocation does not own the cluster
		// and must not attempt to delete it on teardown.
		clusterCreated: false,
	}

	// Validate the reconstructed state (cluster name, binary paths, etc.).
	if err := state.validate(); err != nil {
		_ = os.RemoveAll(suitePaths.RootDir)
		return nil, fmt.Errorf("E2E_USE_EXISTING_CLUSTER: validate state: %w", err)
	}

	// Copy the user-supplied kubeconfig into the suite-owned temp directory so
	// that the path exported via exportEnvironment stays within the suite root.
	if err := copyFile(kubeconfigPath, state.KubeconfigPath); err != nil {
		_ = os.RemoveAll(suitePaths.RootDir)
		return nil, fmt.Errorf("E2E_USE_EXISTING_CLUSTER: copy kubeconfig: %w", err)
	}

	return state, nil
}

// copyFile copies the file at src to dst, creating dst if it does not exist.
// dst's parent directory must already exist.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create parent dir of %s: %w", dst, err)
	}
	srcData, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.WriteFile(dst, srcData, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}
