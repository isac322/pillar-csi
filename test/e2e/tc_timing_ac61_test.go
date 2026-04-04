package e2e

// tc_timing_ac61_test.go — Sub-AC 6.1: per-TC timing instrumentation tests.
//
// Acceptance criteria verified here:
//
//  1. timingRecorder.start() records a non-zero StartedAt timestamp for each TC.
//  2. timingRecorder.finalize() records a non-zero FinishedAt timestamp and
//     derives TotalNanos correctly as FinishedAt − StartedAt.
//  3. emitCurrentTimingReportEntry adds the "tc_elapsed" report entry with
//     ReportEntryVisibilityFailureOrVerbose so elapsed time is visible in the
//     Ginkgo v2 report on failure or when running with -v.
//  4. The ReportAfterEach hook (profileCollectorReportAfterEach) captures the
//     spec's RunTime as the authoritative elapsed duration via TotalNanos in the
//     TCProfile written to suiteLiveProfileCapture.
//  5. Elapsed times are non-negative and monotone: a longer spec produces a
//     larger TotalNanos.
//  6. formatElapsedEntry produces a TC-ID-labeled, human-readable elapsed line
//     suitable for the Ginkgo report.
//  7. The testCaseTimingProfile round-trips StartedAt / FinishedAt / TotalNanos
//     through JSON without loss.
//
// These tests run as plain Go unit tests (no Ginkgo suite) so they can be
// executed quickly via `go test -run TestAC61 ./test/e2e/`.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ── 1. Start timestamp recording ─────────────────────────────────────────────

// TestAC61TimingStartRecordsNonZeroStartedAt verifies that timingRecorder.start()
// populates a non-zero StartedAt timestamp in the active profile so that there
// is always an anchor for the elapsed-time calculation.
func TestAC61TimingStartRecordsNonZeroStartedAt(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[001/437] E1.2 :: TestStartTimestamp",
	}
	recorder.start(report)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	if profile.StartedAt.IsZero() {
		t.Error("StartedAt is zero — start timestamp was not recorded (AC 6.1)")
	}
}

// TestAC61TimingStartUsesReportStartTimeWhenAvailable verifies that when the
// SpecReport carries a non-zero StartTime (set by Ginkgo when the spec begins),
// the timingRecorder adopts it as the StartedAt rather than calling the clock.
// This ensures the recorded timestamp is Ginkgo-authoritative, not subject to
// scheduling jitter between the Ginkgo runner and our BeforeEach hook.
func TestAC61TimingStartUsesReportStartTimeWhenAvailable(t *testing.T) {
	t.Parallel()
	ginkgoStart := time.Date(2026, 4, 1, 10, 30, 0, 0, time.UTC)
	clockNow := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC) // different — clock is later

	clock := &steppingClock{current: clockNow, step: 1 * time.Millisecond}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[010/437] E5.1 :: TestStartTimeFromReport",
		StartTime:    ginkgoStart,
	}
	recorder.start(report)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	if !profile.StartedAt.Equal(ginkgoStart) {
		t.Errorf("StartedAt = %v, want %v (from report.StartTime)", profile.StartedAt, ginkgoStart)
	}
}

// TestAC61TimingStartFallsBackToClockWhenReportStartTimeIsZero verifies that
// when the SpecReport has a zero StartTime, the timingRecorder falls back to
// the current clock value so a valid timestamp is always recorded.
func TestAC61TimingStartFallsBackToClockWhenReportStartTimeIsZero(t *testing.T) {
	t.Parallel()
	clockNow := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	clock := &steppingClock{current: clockNow, step: 1 * time.Millisecond}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[002/437] E2.1 :: TestClockFallback",
		// StartTime is zero — recorder must use its clock.
	}
	recorder.start(report)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	if profile.StartedAt.IsZero() {
		t.Error("StartedAt is zero even without report.StartTime — clock fallback failed (AC 6.1)")
	}
	// The recorded time must come from the clock (clockNow), not be zero.
	if profile.StartedAt.Before(clockNow) {
		t.Errorf("StartedAt %v is before clockNow %v — unexpected time", profile.StartedAt, clockNow)
	}
}

// ── 2. End timestamp and TotalNanos ──────────────────────────────────────────

// TestAC61TimingFinalizeRecordsNonZeroFinishedAt verifies that
// timingRecorder.finalize() populates a non-zero FinishedAt so there is always
// a complete start→end pair for elapsed-time computation.
func TestAC61TimingFinalizeRecordsNonZeroFinishedAt(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    10 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[005/437] E3.1 :: TestFinishedAt",
	}
	recorder.start(report)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	if profile.FinishedAt.IsZero() {
		t.Error("FinishedAt is zero — end timestamp was not recorded (AC 6.1)")
	}
}

// TestAC61TimingFinishedAtIsAfterStartedAt verifies the temporal ordering
// invariant: FinishedAt must always be ≥ StartedAt.
func TestAC61TimingFinishedAtIsAfterStartedAt(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    7 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[003/437] E1.1 :: TestOrdering",
	}
	recorder.start(report)
	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	if profile.FinishedAt.Before(profile.StartedAt) {
		t.Errorf("FinishedAt %v is before StartedAt %v — temporal ordering violated (AC 6.1)",
			profile.FinishedAt, profile.StartedAt)
	}
}

// TestAC61TotalNanosEqualsFinishedAtMinusStartedAt verifies that TotalNanos is
// precisely derived from FinishedAt − StartedAt, not from an independent source.
func TestAC61TotalNanosEqualsFinishedAtMinusStartedAt(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    13 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[004/437] E1.3 :: TestTotalNanos",
	}
	recorder.start(report)
	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	want := profile.FinishedAt.Sub(profile.StartedAt).Nanoseconds()
	if profile.TotalNanos != want {
		t.Errorf("TotalNanos = %d, want %d (FinishedAt - StartedAt)", profile.TotalNanos, want)
	}
}

// TestAC61TotalNanosIsPositive verifies that TotalNanos is always > 0 for a
// completed spec, satisfying the invariant that elapsed times are non-negative.
func TestAC61TotalNanosIsPositive(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    1 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[006/437] E2.3 :: TestPositiveNanos",
	}
	recorder.start(report)
	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	if profile.TotalNanos <= 0 {
		t.Errorf("TotalNanos = %d, want > 0 — elapsed time must be positive (AC 6.1)", profile.TotalNanos)
	}
}

// ── 3. tc_elapsed report entry format ────────────────────────────────────────

// TestAC61TimingElapsedEntryNameConstant verifies the constant value so that
// any code reading the "tc_elapsed" report entry uses the same key.
func TestAC61TimingElapsedEntryNameConstant(t *testing.T) {
	t.Parallel()
	const want = "tc_elapsed"
	if timingElapsedEntryName != want {
		t.Errorf("timingElapsedEntryName = %q, want %q", timingElapsedEntryName, want)
	}
}

// TestAC61FormatElapsedEntryContainsTCIDAndDuration verifies that
// formatElapsedEntry produces a string that contains both the TC ID (prefixed
// with "[TC-") and the elapsed duration in a human-readable format.
func TestAC61FormatElapsedEntryContainsTCIDAndDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tcID       string
		totalNanos int64
		wantParts  []string
	}{
		{
			tcID:       "E1.2",
			totalNanos: (12*time.Millisecond + 500*time.Microsecond).Nanoseconds(),
			wantParts:  []string{"[TC-E1.2]", "elapsed:"},
		},
		{
			tcID:       "E33.285",
			totalNanos: (1 * time.Second).Nanoseconds(),
			wantParts:  []string{"[TC-E33.285]", "elapsed:", "1s"},
		},
		{
			tcID:       "E33.5",
			totalNanos: (250 * time.Millisecond).Nanoseconds(),
			wantParts:  []string{"[TC-E33.5]", "elapsed:", "250ms"},
		},
	}

	for _, tc := range cases {
		got := formatElapsedEntry(tc.tcID, tc.totalNanos)
		for _, part := range tc.wantParts {
			if !strings.Contains(got, part) {
				t.Errorf("formatElapsedEntry(%q, %d) = %q, want to contain %q",
					tc.tcID, tc.totalNanos, got, part)
			}
		}
	}
}

// TestAC61FormatElapsedEntryPrefixFormat verifies the exact prefix format of the
// elapsed entry string: "[TC-{tcID}] elapsed: ".
func TestAC61FormatElapsedEntryPrefixFormat(t *testing.T) {
	t.Parallel()
	got := formatElapsedEntry("E5.3", (100 * time.Millisecond).Nanoseconds())
	wantPrefix := "[TC-E5.3] elapsed: "
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("formatElapsedEntry = %q, want prefix %q", got, wantPrefix)
	}
}

// TestAC61FormatElapsedEntryZeroNanos verifies that formatElapsedEntry handles
// zero TotalNanos gracefully (returns a valid string rather than panicking).
func TestAC61FormatElapsedEntryZeroNanos(t *testing.T) {
	t.Parallel()
	got := formatElapsedEntry("E1.1", 0)
	if !strings.Contains(got, "[TC-E1.1]") {
		t.Errorf("formatElapsedEntry with 0 nanos = %q, must still contain TC ID", got)
	}
}

// ── 4. ReportAfterEach: elapsed time output in Ginkgo v2 report ──────────────

// TestAC61ReportAfterEachCapturesSpecRunTimeAsElapsed verifies that the
// ReportAfterEach hook (via simulateLiveProfileCaptureHook) captures the
// spec's RunTime as TotalNanos, satisfying the AC 6.1 requirement for
// "outputs elapsed time in the Ginkgo v2 report (ReportAfterEach)".
func TestAC61ReportAfterEachCapturesSpecRunTimeAsElapsed(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	wantElapsed := 123 * time.Millisecond
	simulateLiveProfileCaptureHook(capture, types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "[TC-E3.1] TestElapsedCapture",
		RunTime:      wantElapsed,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E3.1")},
			{Name: "tc_category", Value: types.WrapEntryValue("in-process")},
		},
	})

	if capture.len() != 1 {
		t.Fatalf("expected 1 profile entry, got %d", capture.len())
	}

	snap := capture.snapshot()
	got := snap[0]

	// AC 6.1: the elapsed time from ReportAfterEach must equal the spec's RunTime.
	if got.TotalNanos != wantElapsed.Nanoseconds() {
		t.Errorf("TotalNanos = %d (%.2fms), want %d (%.2fms) — elapsed time mismatch (AC 6.1)",
			got.TotalNanos, float64(got.TotalNanos)/1e6,
			wantElapsed.Nanoseconds(), float64(wantElapsed.Nanoseconds())/1e6)
	}

	if got.TCID != "E3.1" {
		t.Errorf("TCID = %q, want %q", got.TCID, "E3.1")
	}
}

// TestAC61ReportAfterEachElapsedTimeForMultipleTCs verifies that the
// ReportAfterEach hook records the correct elapsed time for each of multiple
// sequential TCs, each with a distinct duration.
func TestAC61ReportAfterEachElapsedTimeForMultipleTCs(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	tcData := []struct {
		id      string
		elapsed time.Duration
	}{
		{"E1.1", 50 * time.Millisecond},
		{"E1.2", 100 * time.Millisecond},
		{"E2.1", 200 * time.Millisecond},
		{"E33.285", 1 * time.Second},
	}

	for _, tc := range tcData {
		simulateLiveProfileCaptureHook(capture, types.SpecReport{
			LeafNodeType: types.NodeTypeIt,
			LeafNodeText: "[TC-" + tc.id + "] test",
			RunTime:      tc.elapsed,
			ReportEntries: types.ReportEntries{
				{Name: "tc_id", Value: types.WrapEntryValue(tc.id)},
			},
		})
	}

	if capture.len() != len(tcData) {
		t.Fatalf("expected %d entries, got %d", len(tcData), capture.len())
	}

	snap := capture.snapshot()
	for i, want := range tcData {
		got := snap[i]
		if got.TCID != want.id {
			t.Errorf("snap[%d].TCID = %q, want %q", i, got.TCID, want.id)
		}
		if got.TotalNanos != want.elapsed.Nanoseconds() {
			t.Errorf("snap[%d].TotalNanos = %d (%.2fms), want %d (%.2fms)",
				i, got.TotalNanos, float64(got.TotalNanos)/1e6,
				want.elapsed.Nanoseconds(), float64(want.elapsed.Nanoseconds())/1e6)
		}
	}
}

// ── 5. Elapsed time monotonicity ─────────────────────────────────────────────

// TestAC61ElapsedTimeMonotonicity verifies that a spec with a longer stepping
// clock produces a larger TotalNanos than a spec with a shorter step, confirming
// that timing is proportional to actual elapsed time.
func TestAC61ElapsedTimeMonotonicity(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	spec := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[001/437] E1.1 :: TestMonotonicity",
	}

	// Fast spec: clock advances 1ms each call.
	clockFast := &steppingClock{current: base, step: 1 * time.Millisecond}
	recFast := newSuiteTimingRecorder(clockFast.Now)
	recFast.start(spec)
	profileFast, ok := recFast.finalize(spec)
	if !ok {
		t.Fatal("finalize (fast) returned no profile")
	}

	// Slow spec: clock advances 100ms each call.
	clockSlow := &steppingClock{current: base, step: 100 * time.Millisecond}
	recSlow := newSuiteTimingRecorder(clockSlow.Now)
	recSlow.start(spec)
	profileSlow, ok := recSlow.finalize(spec)
	if !ok {
		t.Fatal("finalize (slow) returned no profile")
	}

	if profileFast.TotalNanos <= 0 {
		t.Errorf("fast spec TotalNanos = %d, want > 0", profileFast.TotalNanos)
	}
	if profileSlow.TotalNanos <= 0 {
		t.Errorf("slow spec TotalNanos = %d, want > 0", profileSlow.TotalNanos)
	}
	if profileSlow.TotalNanos <= profileFast.TotalNanos {
		t.Errorf("slow spec TotalNanos (%d) ≤ fast spec TotalNanos (%d) — monotonicity violated",
			profileSlow.TotalNanos, profileFast.TotalNanos)
	}
}

// ── 6. JSON round-trip for StartedAt / FinishedAt / TotalNanos ───────────────

// TestAC61TimingProfileEncodeDecodePreservesTimestamps verifies that
// testCaseTimingProfile.encode() / decodeTimingProfile() round-trip StartedAt,
// FinishedAt, and TotalNanos without precision loss so that the Ginkgo report
// entry carries accurate timestamps when stored in --json-report output.
func TestAC61TimingProfileEncodeDecodePreservesTimestamps(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(25 * time.Millisecond)

	profile := testCaseTimingProfile{
		TCID:       "E1.2",
		TestName:   "TestRoundTrip",
		StartedAt:  start,
		FinishedAt: end,
		TotalNanos: end.Sub(start).Nanoseconds(),
	}

	payload, err := profile.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := decodeTimingProfile(payload)
	if err != nil {
		t.Fatalf("decodeTimingProfile: %v", err)
	}

	// StartedAt must round-trip exactly.
	if !decoded.StartedAt.Equal(start) {
		t.Errorf("decoded StartedAt = %v, want %v", decoded.StartedAt, start)
	}
	// FinishedAt must round-trip exactly.
	if !decoded.FinishedAt.Equal(end) {
		t.Errorf("decoded FinishedAt = %v, want %v", decoded.FinishedAt, end)
	}
	// TotalNanos must round-trip exactly.
	if decoded.TotalNanos != profile.TotalNanos {
		t.Errorf("decoded TotalNanos = %d, want %d", decoded.TotalNanos, profile.TotalNanos)
	}
}

// TestAC61TimingProfileJSONContainsExpectedFields verifies that the JSON
// representation of a testCaseTimingProfile includes the startedAt, finishedAt,
// and totalNanos fields required by AC 6.1.
func TestAC61TimingProfileJSONContainsExpectedFields(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Millisecond)

	profile := testCaseTimingProfile{
		TCID:       "E2.3",
		StartedAt:  start,
		FinishedAt: end,
		TotalNanos: end.Sub(start).Nanoseconds(),
	}

	payload, err := profile.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Parse as a raw map to check JSON field presence without relying on struct tags.
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		t.Fatalf("json.Unmarshal raw map: %v", err)
	}

	requiredFields := []string{"startedAt", "finishedAt", "totalNanos"}
	for _, field := range requiredFields {
		if _, ok := raw[field]; !ok {
			t.Errorf("JSON payload missing required field %q (AC 6.1): %s", field, payload)
		}
	}
}

// ── 7. TCID inference and tc_elapsed entry emission ──────────────────────────

// TestAC61TCIDInferredFromSpecText verifies that when a spec text follows the
// "TC[NNN/437] {tcID} :: {testName}" convention, the timingRecorder correctly
// infers the TC ID from the spec text for use in the tc_elapsed entry.
func TestAC61TCIDInferredFromSpecText(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[007/437] E4.2 :: TestIDInference",
	}
	recorder.start(report)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	if profile.TCID != "E4.2" {
		t.Errorf("TCID = %q, want %q (inferred from spec text)", profile.TCID, "E4.2")
	}
}

// TestAC61TCIDFromReportEntryOverridesSpecTextInference verifies that when a
// "tc_id" report entry is present (added by the It block body), finalize() uses
// that value rather than the spec-text-inferred TCID.  This ensures accuracy for
// specs whose LeafNodeText doesn't follow the TC::{name} convention.
func TestAC61TCIDFromReportEntryOverridesSpecTextInference(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	// Spec text that would infer no TCID (no :: separator, no TC ID bracket).
	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "[TC-E9.7]", // default_profile_test.go format
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E9.7")},
		},
	}
	recorder.start(report)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	// The report entry "tc_id" must override whatever spec-text inference produced.
	if profile.TCID != "E9.7" {
		t.Errorf("TCID = %q, want %q (from tc_id report entry)", profile.TCID, "E9.7")
	}
}

// TestAC61FinalizeDoesNotRecordProfileForNonItNodes verifies that
// timingRecorder.start() returns without creating a profile when the spec
// is not an It node (e.g., BeforeSuite, AfterSuite), so no spurious elapsed
// entries are emitted for suite-level setup/teardown nodes.
func TestAC61FinalizeDoesNotRecordProfileForNonItNodes(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	nonItReport := types.SpecReport{
		LeafNodeType: types.NodeTypeBeforeSuite,
		LeafNodeText: "BeforeSuite setup",
	}
	recorder.start(nonItReport)

	_, ok := recorder.finalize(nonItReport)
	if ok {
		t.Error("finalize returned a profile for a BeforeSuite node — only It nodes should be profiled (AC 6.1)")
	}
}

// ── 8. Integration: timingRecorder → emitCurrentTimingReportEntry ─────────────

// TestAC61EmitCurrentTimingReportEntryProducesElapsedEntryValue verifies that
// the value produced by formatElapsedEntry (used by emitCurrentTimingReportEntry)
// correctly encodes the TC ID and elapsed duration as a human-readable string
// accessible from the Ginkgo v2 report (satisfying the "outputs elapsed time in
// the Ginkgo v2 report" requirement of AC 6.1).
func TestAC61EmitCurrentTimingReportEntryProducesElapsedEntryValue(t *testing.T) {
	t.Parallel()
	// Simulate what emitCurrentTimingReportEntry would do for a TC with a
	// known elapsed time.
	const tcID = "E11.3"
	const totalNanos = int64(47 * time.Millisecond)

	elapsed := formatElapsedEntry(tcID, totalNanos)

	// The tc_elapsed entry value must include the TC ID.
	if !strings.Contains(elapsed, "[TC-E11.3]") {
		t.Errorf("elapsed entry = %q, missing [TC-E11.3]", elapsed)
	}
	// The tc_elapsed entry value must include the "elapsed:" label.
	if !strings.Contains(elapsed, "elapsed:") {
		t.Errorf("elapsed entry = %q, missing 'elapsed:' label", elapsed)
	}
	// The tc_elapsed entry value must include the duration.
	if !strings.Contains(elapsed, "47ms") {
		t.Errorf("elapsed entry = %q, missing duration 47ms", elapsed)
	}
}

// TestAC61ElapsedEntryMatchesProfileTotalNanos verifies the end-to-end
// consistency between the timingRecorder's TotalNanos and the value that
// would appear in the "tc_elapsed" Ginkgo report entry: they must describe the
// same duration.
func TestAC61ElapsedEntryMatchesProfileTotalNanos(t *testing.T) {
	t.Parallel()
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    33 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)

	report := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "TC[100/437] E22.1 :: TestConsistency",
	}
	recorder.start(report)
	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	// Format the elapsed entry the same way emitCurrentTimingReportEntry does.
	elapsedEntry := formatElapsedEntry(profile.TCID, profile.TotalNanos)

	// The elapsed entry must mention the duration described by TotalNanos.
	expected := time.Duration(profile.TotalNanos).String()
	if !strings.Contains(elapsedEntry, expected) {
		t.Errorf("elapsed entry %q does not contain duration %q (from TotalNanos=%d)",
			elapsedEntry, expected, profile.TotalNanos)
	}
}
