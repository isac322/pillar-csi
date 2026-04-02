package e2e

// profile_hooks.go — Sub-AC 6.2: ProfileCollector hooks for automatic per-TC
// timing capture.
//
// This file wires the ProfileCollector into two Ginkgo hook types so that
// phase timings are captured automatically for every TC without any manual
// instrumentation inside TC spec bodies:
//
//  1. ReportAfterEach — fires after each spec completes (including all cleanup),
//     at which point SpecReport.RunTime is Ginkgo's authoritative wall-clock
//     total. The hook reads the finalized SpecReport and appends a TCProfile to
//     suiteLiveProfileCapture.
//
//  2. Suite-level setup/teardown — the BeforeSuite / AfterSuite hooks in
//     suite_group_timing_test.go (non-e2e build) and
//     kind_bootstrap_e2e_test.go (e2e build) use recordGroupSetupTiming /
//     recordGroupTeardownTiming to feed suiteGroupTiming. The group-phase
//     durations for the final ProfileReport are extracted from the consolidated
//     Ginkgo report via collectGroupTimingFromReport in buildProfileReport.
//
// Phase timing integration points (all automatic, no TC body calls needed):
//
//	spec.body       → JustBeforeEach / JustAfterEach in timing_capture.go
//	tc-setup        → measureTimingPhaseErr in tc_setup.go StartTestCase
//	tc-teardown     → measureTimingPhaseErr in TestCaseScope.Close
//	tc-timing entry → emitted by DeferCleanup in timing_capture.go
//	group-setup     → BeforeSuite / SynchronizedBeforeSuite runtime via
//	                  collectGroupTimingFromReport in buildProfileReport
//	group-teardown  → AfterSuite / SynchronizedAfterSuite runtime via
//	                  collectGroupTimingFromReport in buildProfileReport

import (
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

// suiteLiveProfileCapture is the package-level live accumulator populated by
// the profileCollectorReportAfterEach hook. Each Ginkgo worker process has its
// own copy; the ReportAfterSuite hook on the primary process uses the
// consolidated types.Report (which aggregates all workers) for the final
// ProfileReport, so the live capture complements rather than replaces that path.
//
// The primary uses of suiteLiveProfileCapture are:
//   - Incremental progress tracking during a long suite run.
//   - Availability of per-worker TC timing data before ReportAfterSuite fires.
//   - Unit-testable snapshot of what the ReportAfterEach hook recorded.
var suiteLiveProfileCapture = newLiveProfileCapture()

// liveProfileCapture is a thread-safe ordered accumulator of TCProfile entries
// built incrementally by the ReportAfterEach hook as specs complete on the
// current Ginkgo worker process.
//
// Entries are appended in spec-completion order. The slice is not sorted; the
// final ProfileReport sort (by TotalNanos descending) happens in buildProfileReport.
type liveProfileCapture struct {
	mu      sync.Mutex
	entries []TCProfile
}

// newLiveProfileCapture returns an empty, ready-to-use liveProfileCapture.
func newLiveProfileCapture() *liveProfileCapture {
	return &liveProfileCapture{}
}

// record appends p to the capture store. It is safe to call concurrently:
// Ginkgo serialises ReportAfterEach calls within a single worker process.
func (c *liveProfileCapture) record(p TCProfile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, p)
}

// snapshot returns a copy of all recorded TCProfile entries in insertion order.
// The caller may sort or filter the slice without affecting the store.
func (c *liveProfileCapture) snapshot() []TCProfile {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		return nil
	}
	result := make([]TCProfile, len(c.entries))
	copy(result, c.entries)
	return result
}

// len returns the number of recorded TCProfile entries. Safe for concurrent use.
func (c *liveProfileCapture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// profileCollectorReportAfterEach is the ReportAfterEach handler that feeds
// per-TC timing into suiteLiveProfileCapture automatically, without requiring
// manual timing calls inside TC spec bodies.
//
// Execution timing in the Ginkgo spec lifecycle:
//
//	BeforeEach  → timing_capture.go starts suiteTimingRecorder, registers DeferCleanup
//	JustBeforeEach → begins phaseSpecBody
//	It body     → spec executes
//	JustAfterEach → ends phaseSpecBody
//	DeferCleanup (UsePerTestCaseSetup) → TestCaseScope.Close → measures teardown phases
//	DeferCleanup (timing_capture.go) → emitCurrentTimingReportEntry → writes "tc_timing"
//	ReportAfterEach ← THIS HOOK — receives the fully finalised SpecReport:
//	                   • SpecReport.RunTime is Ginkgo's authoritative total duration
//	                   • "tc_timing" entry contains all phase breakdowns
//
// The hook skips:
//   - Non-It nodes (BeforeSuite, AfterSuite, BeforeEach, etc.).
//   - It nodes without a "tc_id" report entry (internal framework specs, helpers).
//
// GroupSetupNanos and GroupTeardownNanos are set to zero here because
// BeforeSuite/AfterSuite runtimes are only available in the consolidated suite
// report delivered to the primary Ginkgo process in ReportAfterSuite;
// buildProfileReport attaches the correct group-phase values via
// collectGroupTimingFromReport when producing the final ProfileReport.
var _ = ReportAfterEach(func(report types.SpecReport) {
	// Skip non-It nodes (BeforeSuite, AfterSuite, BeforeAll, AfterAll, etc.).
	if report.LeafNodeType != types.NodeTypeIt {
		return
	}

	// Only record specs that carry a documented TC ID. Framework-internal specs
	// (uniqueness guard, isolation checks) do not set "tc_id" and are excluded.
	tcID, ok := reportEntryValue(report.ReportEntries, "tc_id")
	if !ok {
		return
	}

	category, _ := reportEntryValue(report.ReportEntries, "tc_category")

	testName, _ := reportEntryValue(report.ReportEntries, "tc_test_name")
	if testName == "" {
		testName = strings.TrimSpace(report.LeafNodeText)
	}

	// Extract per-TC phase breakdowns from the "tc_timing" report entry written
	// by timing_capture.go's DeferCleanup. Group-level phase durations are not
	// available per-worker; they are set to zero and filled in by
	// buildProfileReport via collectGroupTimingFromReport.
	phases := phaseTimingsFromInternalProfile(report.ReportEntries, 0, 0)

	// SpecReport.RunTime is the authoritative Ginkgo-computed total duration
	// for this spec, covering setup + body + teardown (all DeferCleanup nodes).
	suiteLiveProfileCapture.record(TCProfile{
		TCID:       tcID,
		Category:   category,
		TestName:   testName,
		TotalNanos: report.RunTime.Nanoseconds(),
		Phases:     phases,
	})
})
