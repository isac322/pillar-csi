package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	suiteWorkspaceEnvVar = "PILLAR_CSI_E2E_SUITE_WORKSPACE"
	suiteLogsEnvVar      = "PILLAR_CSI_E2E_SUITE_LOGS"
	suiteGeneratedEnvVar = "PILLAR_CSI_E2E_SUITE_GENERATED"
)

// suiteTempPaths centralizes every suite-owned filesystem location so the
// entire invocation stays contained under a single /tmp root.
type suiteTempPaths struct {
	RootDir      string `json:"rootDir"`
	WorkspaceDir string `json:"workspaceDir"`
	LogsDir      string `json:"logsDir"`
	GeneratedDir string `json:"generatedDir"`
}

func newSuiteTempPaths() (*suiteTempPaths, error) {
	rootDir, err := os.MkdirTemp(tcTempRoot, "pillar-csi-e2e-suite-")
	if err != nil {
		return nil, fmt.Errorf("create e2e suite root: %w", err)
	}

	paths := &suiteTempPaths{
		RootDir:      rootDir,
		WorkspaceDir: filepath.Join(rootDir, "workspace"),
		LogsDir:      filepath.Join(rootDir, "logs"),
		GeneratedDir: filepath.Join(rootDir, "generated"),
	}
	if err := paths.ensure(); err != nil {
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	return paths, nil
}

func (p *suiteTempPaths) ensure() error {
	if err := p.validate(); err != nil {
		return err
	}

	for label, dir := range map[string]string{
		"workspace": p.WorkspaceDir,
		"logs":      p.LogsDir,
		"generated": p.GeneratedDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create suite %s dir %q: %w", label, dir, err)
		}
	}
	return nil
}

func (p *suiteTempPaths) validate() error {
	if p == nil {
		return errors.New("suite temp paths are nil")
	}
	if strings.TrimSpace(p.RootDir) == "" {
		return errors.New("suite root is required")
	}
	if filepath.Clean(p.RootDir) == filepath.Clean(tcTempRoot) {
		return fmt.Errorf("suite root %q must be a dedicated subdirectory under %s", p.RootDir, tcTempRoot)
	}
	if !pathWithinRoot(tcTempRoot, p.RootDir) {
		return fmt.Errorf("suite root %q must stay under %s", p.RootDir, tcTempRoot)
	}
	if err := validateSuiteSubdir("workspace", p.RootDir, p.WorkspaceDir); err != nil {
		return err
	}
	if err := validateSuiteSubdir("logs", p.RootDir, p.LogsDir); err != nil {
		return err
	}
	if err := validateSuiteSubdir("generated", p.RootDir, p.GeneratedDir); err != nil {
		return err
	}
	return nil
}

func (p *suiteTempPaths) KubeconfigPath() string {
	if p == nil {
		return ""
	}
	return filepath.Join(p.GeneratedDir, "kubeconfig")
}

func validateSuiteSubdir(label, root, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("suite %s dir is required", label)
	}
	if filepath.Clean(root) == filepath.Clean(dir) {
		return fmt.Errorf("suite %s dir %q must be a subdirectory of %s", label, dir, root)
	}
	if !pathWithinRoot(root, dir) {
		return fmt.Errorf("suite %s dir %q must stay under %s", label, dir, root)
	}
	return nil
}

func pathWithinRoot(root, target string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	target = filepath.Clean(strings.TrimSpace(target))
	if root == "" || target == "" {
		return false
	}
	if root == target {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
