// Package e2e contains the Pillar CSI end-to-end test suite.
package e2e

import (
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// suiteGroupTimingRecord stores the wall-clock start/finish times for the
// suite-level group-setup and group-teardown phases. It is written once per
// phase by the timing helper functions below and read during report assembly
// in buildProfileReport.
//
// In parallel mode, each Ginkgo process runs its own BeforeSuite/AfterSuite.
// The timing recorded here reflects the local process's group-phase cost.
// The authoritative aggregate (used in buildProfileReport) is extracted from
// the consolidated types.Report, which aggregates data from all processes.
type suiteGroupTimingRecord struct {
	mu sync.RWMutex

	setupStartedAt     time.Time
	setupFinishedAt    time.Time
	teardownStartedAt  time.Time
	teardownFinishedAt time.Time
}

var suiteGroupTiming = newSuiteGroupTimingRecord()

func newSuiteGroupTimingRecord() *suiteGroupTimingRecord {
	return &suiteGroupTimingRecord{}
}

func (g *suiteGroupTimingRecord) beginSetup(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setupStartedAt = now
}

func (g *suiteGroupTimingRecord) endSetup(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setupFinishedAt = now
}

func (g *suiteGroupTimingRecord) beginTeardown(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.teardownStartedAt = now
}

func (g *suiteGroupTimingRecord) endTeardown(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.teardownFinishedAt = now
}

// SetupNanos returns the measured group-setup duration in nanoseconds.
// Returns 0 when the setup phase was not recorded on this process.
func (g *suiteGroupTimingRecord) SetupNanos() int64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.setupStartedAt.IsZero() || g.setupFinishedAt.IsZero() {
		return 0
	}
	return g.setupFinishedAt.Sub(g.setupStartedAt).Nanoseconds()
}

// TeardownNanos returns the measured group-teardown duration in nanoseconds.
// Returns 0 when the teardown phase was not recorded on this process.
func (g *suiteGroupTimingRecord) TeardownNanos() int64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.teardownStartedAt.IsZero() || g.teardownFinishedAt.IsZero() {
		return 0
	}
	return g.teardownFinishedAt.Sub(g.teardownStartedAt).Nanoseconds()
}

// collectGroupTimingFromReport scans the consolidated Ginkgo suite report for
// BeforeSuite, SynchronizedBeforeSuite, AfterSuite, and SynchronizedAfterSuite
// spec reports and returns the maximum runtime observed across all Ginkgo
// processes for each phase, in nanoseconds.
//
// Taking the maximum rather than the sum reflects the wall-clock impact of
// the group phases on TC execution: TCs on the slowest process are blocked
// until that process's BeforeSuite completes.
//
// This function is called by buildProfileReport to populate GroupSetupNanos
// and GroupTeardownNanos on every TCProfile in the report.
func collectGroupTimingFromReport(report types.Report) (setupNanos, teardownNanos int64) {
	for i := range report.SpecReports {
		spec := &report.SpecReports[i]
		n := spec.RunTime.Nanoseconds()
		switch spec.LeafNodeType {
		case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite:
			if n > setupNanos {
				setupNanos = n
			}
		case types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite:
			if n > teardownNanos {
				teardownNanos = n
			}
		}
	}
	return setupNanos, teardownNanos
}

// recordGroupSetupTiming is the canonical timing-hook entry point for
// non-SynchronizedBeforeSuite suites (suites without the e2e build tag).
// It wraps a user-supplied setup function with timing instrumentation that
// records to suiteGroupTiming.
//
// Usage inside a BeforeSuite body:
//
//	var _ = BeforeSuite(func() {
//	    recordGroupSetupTiming(func() {
//	        // actual suite-level setup work
//	    })
//	})
//
// NOTE: Do NOT call this from SynchronizedBeforeSuite – it is for plain
// BeforeSuite only. Suites that use SynchronizedBeforeSuite (the e2e cluster
// tests) derive group timing solely from the consolidated Ginkgo report via
// collectGroupTimingFromReport.
func recordGroupSetupTiming(fn func()) {
	suiteGroupTiming.beginSetup(time.Now())
	defer suiteGroupTiming.endSetup(time.Now())
	if fn != nil {
		fn()
	}
}

// recordGroupTeardownTiming is the canonical timing-hook entry point for
// non-SynchronizedAfterSuite suites.
//
// Usage inside an AfterSuite body:
//
//	var _ = AfterSuite(func() {
//	    recordGroupTeardownTiming(func() {
//	        // actual suite-level teardown work
//	    })
//	})
func recordGroupTeardownTiming(fn func()) {
	suiteGroupTiming.beginTeardown(time.Now())
	defer suiteGroupTiming.endTeardown(time.Now())
	if fn != nil {
		fn()
	}
}
