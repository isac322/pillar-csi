package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestE2E is the single Ginkgo entry point registered with the Go testing
// framework. All documented test cases are assembled under this runner.
//
// Under normal operation TestMain has already re-exec'd via the ginkgo CLI,
// and TestE2E runs inside each of the N parallel ginkgo worker processes.
// When running sequentially (PILLAR_E2E_SEQUENTIAL=true or ginkgo not found),
// TestE2E runs in-process with no inter-process coordination.
//
// Individual TC selection (AC 7):
//
//	go test -run=TC-E1.2   ./test/e2e/...   # run only TC E1.2
//	go test -run='TC-E1\.2' ./test/e2e/...  # same with escaped dot
//	go test -run=TC-F       ./test/e2e/...  # run all Type F TCs
//
// TestMain detects the "TC-" prefix in the -run flag, rewrites it to
// "^TestE2E$", and stores it in tcRunFocusOverride. TestE2E applies it below
// as a Ginkgo FocusStrings filter.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	configureSuiteExecution(os.Stderr)

	// Set Gomega default timeouts consistent with the 2-minute suite budget.
	// Individual specs may override these via Eventually(...).WithTimeout(...).
	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(250 * time.Millisecond)
	SetDefaultConsistentlyDuration(5 * time.Second)
	SetDefaultConsistentlyPollingInterval(250 * time.Millisecond)

	suiteConfig, reporterConfig := GinkgoConfiguration()

	// AC 6: apply fail-fast from -e2e.fail-fast flag or E2E_FAIL_FAST env var.
	// Default is false (continue on failure) so the full summary report is always emitted.
	applyFailFast(&suiteConfig)

	if tcRunFocusOverride != "" {
		// A TC-pattern was given on the command line (e.g. "TC-E1.2" or
		// "TC-F"). Apply it as a Ginkgo focus so only matching specs run.
		// Clear the label filter so specs are selected purely by TC ID,
		// not by the default-profile label.
		suiteConfig.FocusStrings = append(suiteConfig.FocusStrings, tcRunFocusOverride)
		suiteConfig.LabelFilter = ""
	} else {
		// Enforce the 2-minute suite-level timeout. If the caller already
		// specified a shorter (non-zero) timeout via --timeout, honor it;
		// otherwise apply suiteLevelTimeout as the hard ceiling so that a
		// full parallel run is guaranteed to terminate within the budget.
		if suiteConfig.Timeout <= 0 {
			suiteConfig.Timeout = suiteLevelTimeout
		}

		// Restrict to the default execution profile unless the caller has
		// already narrowed or broadened the label expression.
		if strings.TrimSpace(suiteConfig.LabelFilter) == "" {
			suiteConfig.LabelFilter = "default-profile"
		}
	}

	RunSpecs(t, "Pillar CSI E2E Suite", suiteConfig, reporterConfig)
}
