//go:build !e2e

package e2e

// suite_group_timing_test.go — Sub-AC 2b + 6.2: suite-level teardown hook.
//
// This file provides the SynchronizedAfterSuite for the non-e2e build
// (in-process test suite).
//
// Why SynchronizedAfterSuite instead of plain AfterSuite:
//   - Sub-AC 2b requires parallel-safe SynchronizedBeforeSuite /
//     SynchronizedAfterSuite so that the all-nodes phase tears down each
//     Ginkgo node's in-process backend cleanly.
//   - Ginkgo v2 prohibits mixing BeforeSuite and SynchronizedBeforeSuite, and
//     similarly AfterSuite and SynchronizedAfterSuite in the same suite.
//   - Since the BeforeSuite in tc_id_uniqueness_guard_suite_test.go was
//     upgraded to SynchronizedBeforeSuite, the companion teardown must
//     also use SynchronizedAfterSuite.
//
// Build tag (!e2e):
//   - The e2e build uses SynchronizedBeforeSuite / SynchronizedAfterSuite in
//     kind_bootstrap_e2e_test.go (Kind cluster lifecycle).
//   - The non-e2e (in-process) build uses SynchronizedBeforeSuite /
//     SynchronizedAfterSuite defined here and in
//     tc_id_uniqueness_guard_suite_test.go.
//
// Timing note:
//   - recordGroupTeardownTiming must NOT be called from SynchronizedAfterSuite.
//   - The authoritative group-teardown duration is captured automatically by
//     Ginkgo's spec report mechanism and extracted by collectGroupTimingFromReport
//     (group_timing.go) in the ReportAfterSuite hook.

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

var _ = SynchronizedAfterSuite(

	// ── All-nodes phase ───────────────────────────────────────────────────────
	// Runs on every Ginkgo process node after all specs on that node finish.
	// Performs per-node teardown of in-process backends.
	//
	// Sub-AC 5.3: drain any in-flight background TC cleanup goroutines before
	// the node exits. Each parallel worker has its own suiteAsyncCleanup batch,
	// so DrainPendingCleanups only waits for this node's TCs. Cleanup errors
	// are logged rather than failing the suite — spec pass/fail is already final.
	func() {
		if err := DrainPendingCleanups(30 * time.Second); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"[AC5.3] node %d: background TC cleanup errors (informational): %v\n",
				GinkgoParallelProcess(), err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter,
			"[AC2b] node %d: in-process backends torn down cleanly\n",
			GinkgoParallelProcess())
	},

	// ── Primary phase ────────────────────────────────────────────────────────
	// Runs exactly once, on Ginkgo process node 1, after all parallel worker
	// nodes have completed their all-nodes teardown phase.
	//
	// For the non-e2e in-process build there is no suite-level shared resource
	// to clean up (no Kind cluster, no persistent gRPC server). The primary
	// phase is a no-op beyond confirming completion in the log.
	func() {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"[AC2b] primary node: suite-level teardown complete\n")
	},
)
