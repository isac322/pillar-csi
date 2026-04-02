package e2e

import (
	"fmt"
	"os"
)

var suiteOwnedClusterEnvVars = []string{
	"KUBECONFIG",
	"KIND_CLUSTER",
	suiteRootEnvVar,
	suiteWorkspaceEnvVar,
	suiteLogsEnvVar,
	suiteGeneratedEnvVar,
	suiteContextEnvVar,
}

func resetSuiteInvocationEnvironment() error {
	// Reset cluster env vars (set by bootstrapSuiteCluster).
	for _, key := range suiteOwnedClusterEnvVars {
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("unset %s: %w", key, err)
		}
	}
	// Reset backend env vars (set by bootstrapSuiteBackends — AC5.2).
	// These are unset at the start of each primary process run to ensure
	// stale values from a previous invocation are never inherited.
	for _, key := range suiteOwnedBackendEnvVars {
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("unset %s: %w", key, err)
		}
	}
	return nil
}
