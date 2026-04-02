package e2e

import (
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ---------------------------------------------------------------------------
// suiteGroupTimingRecord unit tests
// ---------------------------------------------------------------------------

func TestSuiteGroupTimingRecordSetupNanosReturnsZeroWhenNotRecorded(t *testing.T) {
	g := newSuiteGroupTimingRecord()
	if got := g.SetupNanos(); got != 0 {
		t.Fatalf("SetupNanos before recording = %d, want 0", got)
	}
}

func TestSuiteGroupTimingRecordTeardownNanosReturnsZeroWhenNotRecorded(t *testing.T) {
	g := newSuiteGroupTimingRecord()
	if got := g.TeardownNanos(); got != 0 {
		t.Fatalf("TeardownNanos before recording = %d, want 0", got)
	}
}

func TestSuiteGroupTimingRecordSetupNanosReturnsZeroWhenOnlyBeginCalled(t *testing.T) {
	g := newSuiteGroupTimingRecord()
	g.beginSetup(time.Now())
	// endSetup never called – result must still be 0 (incomplete measurement).
	if got := g.SetupNanos(); got != 0 {
		t.Fatalf("SetupNanos with only beginSetup = %d, want 0", got)
	}
}

func TestSuiteGroupTimingRecordSetupNanosReturnsPositiveDuration(t *testing.T) {
	g := newSuiteGroupTimingRecord()
	start := time.Now()
	g.beginSetup(start)
	g.endSetup(start.Add(50 * time.Millisecond))

	got := g.SetupNanos()
	if got <= 0 {
		t.Fatalf("SetupNanos = %d, want > 0", got)
	}
	want := (50 * time.Millisecond).Nanoseconds()
	if got != want {
		t.Fatalf("SetupNanos = %d, want %d", got, want)
	}
}

func TestSuiteGroupTimingRecordTeardownNanosReturnsPositiveDuration(t *testing.T) {
	g := newSuiteGroupTimingRecord()
	start := time.Now()
	g.beginTeardown(start)
	g.endTeardown(start.Add(30 * time.Millisecond))

	got := g.TeardownNanos()
	if got <= 0 {
		t.Fatalf("TeardownNanos = %d, want > 0", got)
	}
	want := (30 * time.Millisecond).Nanoseconds()
	if got != want {
		t.Fatalf("TeardownNanos = %d, want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// collectGroupTimingFromReport tests
// ---------------------------------------------------------------------------

func TestCollectGroupTimingFromReportReturnsZerosForEmptyReport(t *testing.T) {
	setup, teardown := collectGroupTimingFromReport(types.Report{})
	if setup != 0 {
		t.Fatalf("setupNanos from empty report = %d, want 0", setup)
	}
	if teardown != 0 {
		t.Fatalf("teardownNanos from empty report = %d, want 0", teardown)
	}
}

func TestCollectGroupTimingFromReportIgnoresItNodes(t *testing.T) {
	report := types.Report{
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeIt, RunTime: 5 * time.Second},
			{LeafNodeType: types.NodeTypeIt, RunTime: 3 * time.Second},
		},
	}
	setup, teardown := collectGroupTimingFromReport(report)
	if setup != 0 {
		t.Fatalf("setupNanos from It-only report = %d, want 0", setup)
	}
	if teardown != 0 {
		t.Fatalf("teardownNanos from It-only report = %d, want 0", teardown)
	}
}

func TestCollectGroupTimingFromReportExtractsBeforeSuiteRuntime(t *testing.T) {
	report := types.Report{
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 3 * time.Second},
			{LeafNodeType: types.NodeTypeIt, RunTime: 1 * time.Second},
		},
	}
	setup, teardown := collectGroupTimingFromReport(report)
	want := (3 * time.Second).Nanoseconds()
	if setup != want {
		t.Fatalf("setupNanos = %d, want %d", setup, want)
	}
	if teardown != 0 {
		t.Fatalf("teardownNanos = %d, want 0", teardown)
	}
}

func TestCollectGroupTimingFromReportExtractsSynchronizedBeforeSuiteRuntime(t *testing.T) {
	report := types.Report{
		SpecReports: types.SpecReports{
			// Primary process: long cluster creation.
			{LeafNodeType: types.NodeTypeSynchronizedBeforeSuite, RunTime: 45 * time.Second},
			// Worker process: short "receive" phase.
			{LeafNodeType: types.NodeTypeSynchronizedBeforeSuite, RunTime: 1 * time.Second},
		},
	}
	setup, _ := collectGroupTimingFromReport(report)
	// Should take the MAX, not the sum.
	want := (45 * time.Second).Nanoseconds()
	if setup != want {
		t.Fatalf("setupNanos = %d, want %d (max across processes)", setup, want)
	}
}

func TestCollectGroupTimingFromReportExtractsAfterSuiteRuntime(t *testing.T) {
	report := types.Report{
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 2 * time.Second},
		},
	}
	setup, teardown := collectGroupTimingFromReport(report)
	if setup != 0 {
		t.Fatalf("setupNanos = %d, want 0", setup)
	}
	want := (2 * time.Second).Nanoseconds()
	if teardown != want {
		t.Fatalf("teardownNanos = %d, want %d", teardown, want)
	}
}

func TestCollectGroupTimingFromReportExtractsSynchronizedAfterSuiteRuntime(t *testing.T) {
	report := types.Report{
		SpecReports: types.SpecReports{
			// Primary process: cluster deletion (slow).
			{LeafNodeType: types.NodeTypeSynchronizedAfterSuite, RunTime: 30 * time.Second},
			// Worker process: quick local cleanup.
			{LeafNodeType: types.NodeTypeSynchronizedAfterSuite, RunTime: 500 * time.Millisecond},
		},
	}
	_, teardown := collectGroupTimingFromReport(report)
	want := (30 * time.Second).Nanoseconds()
	if teardown != want {
		t.Fatalf("teardownNanos = %d, want %d (max across processes)", teardown, want)
	}
}

func TestCollectGroupTimingFromReportExtractsBothPhases(t *testing.T) {
	report := types.Report{
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 10 * time.Second},
			{LeafNodeType: types.NodeTypeIt, RunTime: 500 * time.Millisecond},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 5 * time.Second},
		},
	}
	setup, teardown := collectGroupTimingFromReport(report)
	if setup != (10 * time.Second).Nanoseconds() {
		t.Fatalf("setupNanos = %d, want %d", setup, (10 * time.Second).Nanoseconds())
	}
	if teardown != (5 * time.Second).Nanoseconds() {
		t.Fatalf("teardownNanos = %d, want %d", teardown, (5 * time.Second).Nanoseconds())
	}
}

// ---------------------------------------------------------------------------
// phaseTimingsFromInternalProfile tests (with group args)
// ---------------------------------------------------------------------------

func TestPhaseTimingsFromInternalProfilePopulatesGroupFields(t *testing.T) {
	const setupNanos = int64(10_000_000)   // 10 ms
	const teardownNanos = int64(5_000_000) // 5 ms

	// Pass empty entries (no tc_timing entry) – only group timing should be set.
	pt := phaseTimingsFromInternalProfile(types.ReportEntries{}, setupNanos, teardownNanos)

	if pt.GroupSetupNanos != setupNanos {
		t.Fatalf("GroupSetupNanos = %d, want %d", pt.GroupSetupNanos, setupNanos)
	}
	if pt.GroupTeardownNanos != teardownNanos {
		t.Fatalf("GroupTeardownNanos = %d, want %d", pt.GroupTeardownNanos, teardownNanos)
	}
	// Without a tc_timing entry, per-TC phases should be zero.
	if pt.TCSetupNanos != 0 {
		t.Fatalf("TCSetupNanos = %d, want 0 (no tc_timing entry)", pt.TCSetupNanos)
	}
	if pt.TCExecuteNanos != 0 {
		t.Fatalf("TCExecuteNanos = %d, want 0 (no tc_timing entry)", pt.TCExecuteNanos)
	}
	if pt.TCTeardownNanos != 0 {
		t.Fatalf("TCTeardownNanos = %d, want 0 (no tc_timing entry)", pt.TCTeardownNanos)
	}
}

func TestPhaseTimingsFromInternalProfileZeroGroupFieldsWhenZeroPassed(t *testing.T) {
	pt := phaseTimingsFromInternalProfile(types.ReportEntries{}, 0, 0)
	if pt.GroupSetupNanos != 0 {
		t.Fatalf("GroupSetupNanos = %d, want 0", pt.GroupSetupNanos)
	}
	if pt.GroupTeardownNanos != 0 {
		t.Fatalf("GroupTeardownNanos = %d, want 0", pt.GroupTeardownNanos)
	}
}

func TestPhaseTimingsFromInternalProfileMergesPerTCAndGroupPhases(t *testing.T) {
	// Build a tc_timing internal profile that has setup, execute, teardown phases.
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		step:    10 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)
	specReport := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[001/437] E1.1 :: TestMerge",
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

	entries := types.ReportEntries{
		{Name: timingReportEntryName, Value: types.WrapEntryValue(payload)},
	}

	const groupSetup = int64(20_000_000)   // 20 ms
	const groupTeardown = int64(8_000_000) // 8 ms

	pt := phaseTimingsFromInternalProfile(entries, groupSetup, groupTeardown)

	if pt.GroupSetupNanos != groupSetup {
		t.Fatalf("GroupSetupNanos = %d, want %d", pt.GroupSetupNanos, groupSetup)
	}
	if pt.GroupTeardownNanos != groupTeardown {
		t.Fatalf("GroupTeardownNanos = %d, want %d", pt.GroupTeardownNanos, groupTeardown)
	}
	if pt.TCSetupNanos <= 0 {
		t.Fatalf("TCSetupNanos = %d, want > 0", pt.TCSetupNanos)
	}
	if pt.TCExecuteNanos <= 0 {
		t.Fatalf("TCExecuteNanos = %d, want > 0", pt.TCExecuteNanos)
	}
	if pt.TCTeardownNanos <= 0 {
		t.Fatalf("TCTeardownNanos = %d, want > 0", pt.TCTeardownNanos)
	}
}

// ---------------------------------------------------------------------------
// buildProfileReport integration tests
// ---------------------------------------------------------------------------

func TestBuildProfileReportPopulatesGroupSetupAndTeardownNanos(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 2,
		},
		RunTime: 30 * time.Second,
		SpecReports: types.SpecReports{
			// Group-level suite phases.
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 8 * time.Second},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 3 * time.Second},
			// TC specs.
			sampleTimedSpecReport("E1.1", "TestOne", 500*time.Millisecond),
			sampleTimedSpecReport("E1.2", "TestTwo", 750*time.Millisecond),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 2 {
		t.Fatalf("len(TCs) = %d, want 2", len(pr.TCs))
	}

	wantSetup := (8 * time.Second).Nanoseconds()
	wantTeardown := (3 * time.Second).Nanoseconds()

	for _, tc := range pr.TCs {
		if tc.Phases.GroupSetupNanos != wantSetup {
			t.Errorf("TC %s GroupSetupNanos = %d, want %d", tc.TCID, tc.Phases.GroupSetupNanos, wantSetup)
		}
		if tc.Phases.GroupTeardownNanos != wantTeardown {
			t.Errorf("TC %s GroupTeardownNanos = %d, want %d", tc.TCID, tc.Phases.GroupTeardownNanos, wantTeardown)
		}
	}
}

func TestBuildProfileReportGroupTimingZeroWhenNoBeforeAfterSuite(t *testing.T) {
	report := types.Report{
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E2.1", "TestNoGroup", 300*time.Millisecond),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 1 {
		t.Fatalf("len(TCs) = %d, want 1", len(pr.TCs))
	}
	if pr.TCs[0].Phases.GroupSetupNanos != 0 {
		t.Fatalf("GroupSetupNanos = %d, want 0 (no BeforeSuite in report)", pr.TCs[0].Phases.GroupSetupNanos)
	}
	if pr.TCs[0].Phases.GroupTeardownNanos != 0 {
		t.Fatalf("GroupTeardownNanos = %d, want 0 (no AfterSuite in report)", pr.TCs[0].Phases.GroupTeardownNanos)
	}
}

func TestBuildProfileReportGroupTimingFromSynchronizedSuite(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 1,
		},
		RunTime: 60 * time.Second,
		SpecReports: types.SpecReports{
			// SynchronizedBeforeSuite: primary is slow (cluster creation),
			// worker is fast (env export). Max should win.
			{LeafNodeType: types.NodeTypeSynchronizedBeforeSuite, RunTime: 45 * time.Second},
			{LeafNodeType: types.NodeTypeSynchronizedBeforeSuite, RunTime: 500 * time.Millisecond},
			// SynchronizedAfterSuite: primary does the cluster deletion.
			{LeafNodeType: types.NodeTypeSynchronizedAfterSuite, RunTime: 20 * time.Second},
			{LeafNodeType: types.NodeTypeSynchronizedAfterSuite, RunTime: 100 * time.Millisecond},
			// A TC spec.
			sampleTimedSpecReport("E33.1", "TestKind", 1*time.Second),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 1 {
		t.Fatalf("len(TCs) = %d, want 1", len(pr.TCs))
	}

	wantSetup := (45 * time.Second).Nanoseconds()
	wantTeardown := (20 * time.Second).Nanoseconds()

	tc := pr.TCs[0]
	if tc.Phases.GroupSetupNanos != wantSetup {
		t.Errorf("GroupSetupNanos = %d, want %d", tc.Phases.GroupSetupNanos, wantSetup)
	}
	if tc.Phases.GroupTeardownNanos != wantTeardown {
		t.Errorf("GroupTeardownNanos = %d, want %d", tc.Phases.GroupTeardownNanos, wantTeardown)
	}
}

// ---------------------------------------------------------------------------
// recordGroupSetupTiming / recordGroupTeardownTiming unit tests
// ---------------------------------------------------------------------------

func TestRecordGroupSetupTimingCallsFn(t *testing.T) {
	called := false
	saved := suiteGroupTiming
	suiteGroupTiming = newSuiteGroupTimingRecord()
	t.Cleanup(func() { suiteGroupTiming = saved })

	recordGroupSetupTiming(func() { called = true })

	if !called {
		t.Fatal("recordGroupSetupTiming did not invoke fn")
	}
	if suiteGroupTiming.SetupNanos() <= 0 {
		t.Fatal("SetupNanos = 0 after recordGroupSetupTiming; expected > 0")
	}
}

func TestRecordGroupSetupTimingAcceptsNilFn(t *testing.T) {
	saved := suiteGroupTiming
	suiteGroupTiming = newSuiteGroupTimingRecord()
	t.Cleanup(func() { suiteGroupTiming = saved })

	// Must not panic when fn is nil.
	recordGroupSetupTiming(nil)
}

func TestRecordGroupTeardownTimingCallsFn(t *testing.T) {
	called := false
	saved := suiteGroupTiming
	suiteGroupTiming = newSuiteGroupTimingRecord()
	t.Cleanup(func() { suiteGroupTiming = saved })

	recordGroupTeardownTiming(func() { called = true })

	if !called {
		t.Fatal("recordGroupTeardownTiming did not invoke fn")
	}
	if suiteGroupTiming.TeardownNanos() <= 0 {
		t.Fatal("TeardownNanos = 0 after recordGroupTeardownTiming; expected > 0")
	}
}

func TestRecordGroupTeardownTimingAcceptsNilFn(t *testing.T) {
	saved := suiteGroupTiming
	suiteGroupTiming = newSuiteGroupTimingRecord()
	t.Cleanup(func() { suiteGroupTiming = saved })

	// Must not panic when fn is nil.
	recordGroupTeardownTiming(nil)
}
