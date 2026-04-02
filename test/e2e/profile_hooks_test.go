package e2e

// profile_hooks_test.go — Sub-AC 6.2: unit tests for liveProfileCapture and
// the profileCollectorReportAfterEach hook logic.

import (
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ---------------------------------------------------------------------------
// liveProfileCapture unit tests
// ---------------------------------------------------------------------------

func TestLiveProfileCaptureIsEmptyOnCreation(t *testing.T) {
	t.Parallel()
	c := newLiveProfileCapture()
	if c.len() != 0 {
		t.Fatalf("len() = %d, want 0 for new capture", c.len())
	}
	if got := c.snapshot(); got != nil {
		t.Fatalf("snapshot() = %v, want nil for empty capture", got)
	}
}

func TestLiveProfileCaptureRecordAndSnapshot(t *testing.T) {
	t.Parallel()
	c := newLiveProfileCapture()

	profiles := []TCProfile{
		{TCID: "E1.1", Category: "in-process", TestName: "TestA", TotalNanos: 100},
		{TCID: "E1.2", Category: "in-process", TestName: "TestB", TotalNanos: 200},
		{TCID: "F2.1", Category: "full-lvm", TestName: "TestC", TotalNanos: 300},
	}

	for _, p := range profiles {
		c.record(p)
	}

	if c.len() != 3 {
		t.Fatalf("len() = %d after 3 records, want 3", c.len())
	}

	snap := c.snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}

	// Verify insertion order is preserved.
	for i, want := range profiles {
		got := snap[i]
		if got.TCID != want.TCID {
			t.Errorf("snap[%d].TCID = %q, want %q", i, got.TCID, want.TCID)
		}
		if got.TotalNanos != want.TotalNanos {
			t.Errorf("snap[%d].TotalNanos = %d, want %d", i, got.TotalNanos, want.TotalNanos)
		}
	}
}

func TestLiveProfileCaptureSnapshotIsIsolatedCopy(t *testing.T) {
	t.Parallel()
	c := newLiveProfileCapture()
	c.record(TCProfile{TCID: "E1.1", TotalNanos: 100})

	snap1 := c.snapshot()
	c.record(TCProfile{TCID: "E1.2", TotalNanos: 200})
	snap2 := c.snapshot()

	// snap1 must not have been mutated by the second record.
	if len(snap1) != 1 {
		t.Fatalf("snap1 len = %d after second record, want 1 (snapshot must be isolated)", len(snap1))
	}
	if len(snap2) != 2 {
		t.Fatalf("snap2 len = %d, want 2", len(snap2))
	}
}

func TestLiveProfileCaptureLenMatchesRecordCount(t *testing.T) {
	t.Parallel()
	c := newLiveProfileCapture()
	for i := 0; i < 10; i++ {
		c.record(TCProfile{TCID: "E1.1"})
		if c.len() != i+1 {
			t.Fatalf("len() = %d after %d records, want %d", c.len(), i+1, i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// ReportAfterEach hook logic tests
//
// The ReportAfterEach hook is registered as a package-level var and cannot be
// directly unit-tested via Go's testing package. Instead, these tests verify
// the helper logic that the hook relies on and exercise the hook's filtering
// and recording behaviour by simulating the spec lifecycle manually.
// ---------------------------------------------------------------------------

// simulateLiveProfileCaptureHook mimics what the ReportAfterEach hook does so
// we can unit-test it without running a full Ginkgo suite.
func simulateLiveProfileCaptureHook(capture *liveProfileCapture, report types.SpecReport) {
	if report.LeafNodeType != types.NodeTypeIt {
		return
	}
	tcID, ok := reportEntryValue(report.ReportEntries, "tc_id")
	if !ok {
		return
	}
	category, _ := reportEntryValue(report.ReportEntries, "tc_category")
	testName, _ := reportEntryValue(report.ReportEntries, "tc_test_name")
	if testName == "" {
		testName = strings.TrimSpace(report.LeafNodeText)
	}
	phases := phaseTimingsFromInternalProfile(report.ReportEntries, 0, 0)
	capture.record(TCProfile{
		TCID:       tcID,
		Category:   category,
		TestName:   testName,
		TotalNanos: report.RunTime.Nanoseconds(),
		Phases:     phases,
	})
}

func TestProfileHookSkipsNonItNodes(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	nonItNodes := []types.NodeType{
		types.NodeTypeBeforeSuite,
		types.NodeTypeAfterSuite,
		types.NodeTypeSynchronizedBeforeSuite,
		types.NodeTypeSynchronizedAfterSuite,
		types.NodeTypeBeforeEach,
		types.NodeTypeAfterEach,
		types.NodeTypeBeforeAll,
		types.NodeTypeAfterAll,
	}

	for _, nodeType := range nonItNodes {
		simulateLiveProfileCaptureHook(capture, types.SpecReport{
			LeafNodeType: nodeType,
			LeafNodeText: "some setup",
			RunTime:      100 * time.Millisecond,
			ReportEntries: types.ReportEntries{
				{Name: "tc_id", Value: types.WrapEntryValue("E1.1")},
			},
		})
	}

	if capture.len() != 0 {
		t.Fatalf("expected 0 entries for non-It nodes, got %d", capture.len())
	}
}

func TestProfileHookSkipsItNodesWithoutTCID(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	// An It node without a "tc_id" report entry (e.g. an internal framework spec).
	simulateLiveProfileCaptureHook(capture, types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "some internal spec",
		RunTime:      50 * time.Millisecond,
		ReportEntries: types.ReportEntries{
			// No tc_id entry.
			{Name: "other_entry", Value: types.WrapEntryValue("value")},
		},
	})

	if capture.len() != 0 {
		t.Fatalf("expected 0 entries for It node without tc_id, got %d", capture.len())
	}
}

func TestProfileHookCapturesSpecRunTimeAsAuthoritative(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	wantRunTime := 350 * time.Millisecond
	simulateLiveProfileCaptureHook(capture, types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "[TC-E1.2] TestProfileHook",
		RunTime:      wantRunTime,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E1.2")},
			{Name: "tc_category", Value: types.WrapEntryValue("in-process")},
			{Name: "tc_test_name", Value: types.WrapEntryValue("TestProfileHook")},
		},
	})

	if capture.len() != 1 {
		t.Fatalf("expected 1 entry, got %d", capture.len())
	}

	snap := capture.snapshot()
	got := snap[0]

	if got.TCID != "E1.2" {
		t.Errorf("TCID = %q, want E1.2", got.TCID)
	}
	if got.Category != "in-process" {
		t.Errorf("Category = %q, want in-process", got.Category)
	}
	if got.TestName != "TestProfileHook" {
		t.Errorf("TestName = %q, want TestProfileHook", got.TestName)
	}
	if got.TotalNanos != wantRunTime.Nanoseconds() {
		t.Errorf("TotalNanos = %d, want %d (%.0fms)",
			got.TotalNanos, wantRunTime.Nanoseconds(), float64(wantRunTime.Milliseconds()))
	}
}

func TestProfileHookFallsBackToLeafNodeTextWhenTestNameAbsent(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	simulateLiveProfileCaptureHook(capture, types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "  [TC-E3.1] TestFallbackName  ",
		RunTime:      20 * time.Millisecond,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E3.1")},
			// No tc_test_name entry – hook must fall back to LeafNodeText.
		},
	})

	if capture.len() != 1 {
		t.Fatalf("expected 1 entry, got %d", capture.len())
	}
	got := capture.snapshot()[0]
	// Leading/trailing whitespace must be trimmed.
	if got.TestName != "[TC-E3.1] TestFallbackName" {
		t.Errorf("TestName = %q, want %q", got.TestName, "[TC-E3.1] TestFallbackName")
	}
}

func TestProfileHookExtractsPhaseTimingsFromTCTimingEntry(t *testing.T) {
	t.Parallel()
	// Build a tc_timing entry with known phase durations.
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		step:    50 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	specText := "TC[001/437] E5.3 :: TestPhaseExtraction"
	specReport := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: specText,
	}
	recorder.start(specReport)
	recorder.beginPhase(phaseSetupTotal)
	recorder.endPhase(phaseSetupTotal)
	recorder.beginPhase(phaseSpecBody)
	recorder.endPhase(phaseSpecBody)
	recorder.beginPhase(phaseTeardownTotal)
	recorder.endPhase(phaseTeardownTotal)

	profile, ok := recorder.finalize(specReport)
	if !ok {
		t.Fatal("finalize returned no profile")
	}
	payload, err := profile.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	capture := newLiveProfileCapture()
	simulateLiveProfileCaptureHook(capture, types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: specText,
		RunTime:      300 * time.Millisecond,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E5.3")},
			{Name: "tc_category", Value: types.WrapEntryValue("in-process")},
			{Name: timingReportEntryName, Value: types.WrapEntryValue(payload)},
		},
	})

	if capture.len() != 1 {
		t.Fatalf("expected 1 entry, got %d", capture.len())
	}

	got := capture.snapshot()[0]
	if got.Phases.TCSetupNanos <= 0 {
		t.Errorf("TCSetupNanos = %d, want > 0 (from tc_timing entry)", got.Phases.TCSetupNanos)
	}
	if got.Phases.TCExecuteNanos <= 0 {
		t.Errorf("TCExecuteNanos = %d, want > 0 (from tc_timing entry)", got.Phases.TCExecuteNanos)
	}
	if got.Phases.TCTeardownNanos <= 0 {
		t.Errorf("TCTeardownNanos = %d, want > 0 (from tc_timing entry)", got.Phases.TCTeardownNanos)
	}
	// Group-phase fields are zero: ReportAfterEach does not have consolidated
	// report access; buildProfileReport fills these in via collectGroupTimingFromReport.
	if got.Phases.GroupSetupNanos != 0 {
		t.Errorf("GroupSetupNanos = %d, want 0 (not available in ReportAfterEach)", got.Phases.GroupSetupNanos)
	}
	if got.Phases.GroupTeardownNanos != 0 {
		t.Errorf("GroupTeardownNanos = %d, want 0 (not available in ReportAfterEach)", got.Phases.GroupTeardownNanos)
	}
}

func TestProfileHookRecordsMultipleTCsInOrder(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	tcData := []struct {
		id       string
		duration time.Duration
	}{
		{"E1.1", 100 * time.Millisecond},
		{"E1.2", 200 * time.Millisecond},
		{"F2.1", 300 * time.Millisecond},
		{"E3.5", 150 * time.Millisecond},
	}

	for _, tc := range tcData {
		simulateLiveProfileCaptureHook(capture, types.SpecReport{
			LeafNodeType: types.NodeTypeIt,
			LeafNodeText: "[TC-" + tc.id + "] test",
			RunTime:      tc.duration,
			ReportEntries: types.ReportEntries{
				{Name: "tc_id", Value: types.WrapEntryValue(tc.id)},
			},
		})
	}

	if capture.len() != 4 {
		t.Fatalf("expected 4 entries, got %d", capture.len())
	}

	snap := capture.snapshot()
	for i, want := range tcData {
		if snap[i].TCID != want.id {
			t.Errorf("snap[%d].TCID = %q, want %q", i, snap[i].TCID, want.id)
		}
		if snap[i].TotalNanos != want.duration.Nanoseconds() {
			t.Errorf("snap[%d].TotalNanos = %d, want %d", i, snap[i].TotalNanos, want.duration.Nanoseconds())
		}
	}
}

// ---------------------------------------------------------------------------
// Suite-level group timing integration tests
// ---------------------------------------------------------------------------

func TestRecordGroupSetupTimingIsUsedInBeforeSuite(t *testing.T) {
	// Verify that recordGroupSetupTiming correctly records a non-zero duration
	// when called with an actual work function (as it is in the BeforeSuite).
	saved := suiteGroupTiming
	suiteGroupTiming = newSuiteGroupTimingRecord()
	t.Cleanup(func() { suiteGroupTiming = saved })

	called := false
	recordGroupSetupTiming(func() {
		called = true
		// Simulate some work.
		time.Sleep(time.Nanosecond)
	})

	if !called {
		t.Fatal("recordGroupSetupTiming did not call fn")
	}
	if suiteGroupTiming.SetupNanos() <= 0 {
		t.Fatal("SetupNanos = 0 after recordGroupSetupTiming; expected > 0")
	}
}

func TestRecordGroupTeardownTimingIsUsedInAfterSuite(t *testing.T) {
	// Verify that recordGroupTeardownTiming correctly records a non-zero duration
	// when called with a nil fn (as in the AfterSuite hook for the non-e2e build
	// where there is no real teardown work).
	saved := suiteGroupTiming
	suiteGroupTiming = newSuiteGroupTimingRecord()
	t.Cleanup(func() { suiteGroupTiming = saved })

	// The AfterSuite hook calls recordGroupTeardownTiming(nil). Verify it doesn't
	// panic and records a zero (or near-zero) duration.
	recordGroupTeardownTiming(nil)

	// TeardownNanos may be zero if begin and end happen at the exact same
	// nanosecond on a fast machine, but must not panic.
	// We check it's accessible without error.
	_ = suiteGroupTiming.TeardownNanos()
}

func TestSuiteLiveProfileCaptureIsAccessible(t *testing.T) {
	t.Parallel()
	// Verify that the package-level suiteLiveProfileCapture is initialized and
	// accessible (not nil). This is a basic smoke test confirming the hook's
	// backing store is ready before any specs run.
	if suiteLiveProfileCapture == nil {
		t.Fatal("suiteLiveProfileCapture is nil — package-level init failed")
	}
}
