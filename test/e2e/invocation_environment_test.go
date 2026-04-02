package e2e

import (
	"os"
	"testing"
)

func TestResetSuiteInvocationEnvironmentClearsOwnedClusterEnv(t *testing.T) {
	for _, key := range suiteOwnedClusterEnvVars {
		t.Setenv(key, "stale")
	}

	if err := resetSuiteInvocationEnvironment(); err != nil {
		t.Fatalf("resetSuiteInvocationEnvironment: %v", err)
	}

	for _, key := range suiteOwnedClusterEnvVars {
		if value, ok := os.LookupEnv(key); ok {
			t.Fatalf("%s still set to %q", key, value)
		}
	}
}
