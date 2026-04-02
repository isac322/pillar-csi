// Package runtimepaths resolves E2E-test runtime paths relative to the
// per-invocation suite workspace exported by the test binary's TestMain.
package runtimepaths

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// SuiteWorkspaceEnvVar coordinates local runtime paths with the E2E suite's
	// /tmp-backed invocation workspace.
	SuiteWorkspaceEnvVar = "PILLAR_CSI_E2E_SUITE_WORKSPACE"

	tempRoot = "/tmp"
)

// SuiteWorkspaceDir returns the cleaned absolute suite workspace path when the
// E2E suite exported one. Relative values are rejected so local helpers do not
// silently fall back to repo-relative artifact paths.
func SuiteWorkspaceDir() string {
	raw, ok := os.LookupEnv(SuiteWorkspaceEnvVar)
	if !ok {
		return ""
	}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !filepath.IsAbs(trimmed) {
		return ""
	}
	return filepath.Clean(trimmed)
}

// ResolveControllerCSIEndpoint returns the controller CSI endpoint URL,
// preferring the suite workspace path over the provided fallback.
func ResolveControllerCSIEndpoint(fallback string) string {
	if workspace := SuiteWorkspaceDir(); workspace != "" {
		return "unix://" + filepath.Join(workspace, "controller", "csi.sock")
	}
	return fallback
}

// ResolveNodeCSISocketPath returns the node CSI socket path,
// preferring the suite workspace path over the provided fallback.
func ResolveNodeCSISocketPath(fallback string) string {
	if workspace := SuiteWorkspaceDir(); workspace != "" {
		return filepath.Join(workspace, "node", "csi.sock")
	}
	return fallback
}

// ResolveNodeStateDir returns the node state directory path,
// preferring the suite workspace path over the provided fallback.
func ResolveNodeStateDir(fallback string) string {
	if workspace := SuiteWorkspaceDir(); workspace != "" {
		return filepath.Join(workspace, "node", "state")
	}
	return fallback
}

// ResolveAgentConfigfsRoot returns the agent configfs root path,
// preferring the suite workspace path over the provided fallback.
func ResolveAgentConfigfsRoot(fallback string) string {
	if workspace := SuiteWorkspaceDir(); workspace != "" {
		return filepath.Join(workspace, "agent", "configfs")
	}
	return fallback
}

// ResolveCommandWorkDir returns the working directory for shell commands,
// using the suite workspace when available and falling back to projectDir.
func ResolveCommandWorkDir(projectDir string) string {
	if workspace := SuiteWorkspaceDir(); workspace != "" {
		return workspace
	}
	return projectDir
}

// ResolveCommandTempDir returns a temporary directory path suitable for
// command scratch space, under the suite workspace when available.
func ResolveCommandTempDir() string {
	if workspace := SuiteWorkspaceDir(); workspace != "" {
		return filepath.Join(workspace, "tmp")
	}
	return filepath.Join(tempRoot, "pillar-csi-utils")
}
