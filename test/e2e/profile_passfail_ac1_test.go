package e2e

// profile_passfail_ac1_test.go — Sub-AC 1: per-TC pass/fail traceability in the
// JSON timing profile.
//
// Acceptance criteria verified here:
//
//  1. TCProfile.Passed is true when spec.State == SpecStatePassed.
//  2. TCProfile.Passed is false when spec.State == SpecStateFailed.
//  3. TCProfile.Passed is false when spec.State == SpecStatePanicked.
//  4. TCProfile.Passed is false when spec.State == SpecStateTimedout.
//  5. TCProfile.Passed is false when spec.State == SpecStateSkipped.
//  6. TCProfile.Passed is false when spec.State == SpecStatePending.
//  7. specStatePassed correctly classifies all known SpecState values.
//  8. buildProfileReport propagates Passed from spec.State to TCProfile.
//  9. ProfileCollector.Flush writes Passed field to the JSON report file.
// 10. simulateLiveProfileCaptureHook captures Passed from report.State.
// 11. ProfileReport JSON round-trips Passed field without loss.
// 12. Mixed pass/fail suite reports have correct per-TC Passed values.
// 13. Passed field is present in the JSON output even when false.
// 14. Timing data (TotalNanos) is independent of Passed value.
//
// These tests run as plain Go unit tests (no Ginkgo suite) so they execute
// quickly via `go test -run TestAC1PassFail ./test/e2e/`.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// sampleTimedSpecReportWithState creates a synthetic types.SpecReport for use in
// profile tests.  Unlike sampleTimedSpecReport, it accepts an explicit SpecState
// so pass/fail behaviour can be exercised.
func sampleTimedSpecReportWithState(tcID, testName string, duration time.Duration, state types.SpecState) types.SpecReport {
	return types.SpecReport{
		LeafNodeText: testName,
		RunTime:      duration,
		State:        state,
		ReportEntries: types.ReportEntries{
			{
				Name:  "tc_id",
				Value: types.WrapEntryValue(tcID),
			},
			{
				Name:  "tc_test_name",
				Value: types.WrapEntryValue(testName),
			},
		},
	}
}

// ── 1–6. specStatePassed state classification ─────────────────────────────────

// TestAC1PassFailSpecStatePassedMapsToTrue verifies that SpecStatePassed is the
// only state that causes specStatePassed to return true.
func TestAC1PassFailSpecStatePassedMapsToTrue(t *testing.T) {
	t.Parallel()
	if !specStatePassed(types.SpecStatePassed) {
		t.Error("specStatePassed(SpecStatePassed) = false, want true (Sub-AC 1)")
	}
}

// TestAC1PassFailSpecStateFailedMapsToFalse verifies SpecStateFailed → false.
func TestAC1PassFailSpecStateFailedMapsToFalse(t *testing.T) {
	t.Parallel()
	if specStatePassed(types.SpecStateFailed) {
		t.Error("specStatePassed(SpecStateFailed) = true, want false (Sub-AC 1)")
	}
}

// TestAC1PassFailSpecStatePanickedMapsToFalse verifies SpecStatePanicked → false.
func TestAC1PassFailSpecStatePanickedMapsToFalse(t *testing.T) {
	t.Parallel()
	if specStatePassed(types.SpecStatePanicked) {
		t.Error("specStatePassed(SpecStatePanicked) = true, want false (Sub-AC 1)")
	}
}

// TestAC1PassFailSpecStateTimedoutMapsToFalse verifies SpecStateTimedout → false.
func TestAC1PassFailSpecStateTimedoutMapsToFalse(t *testing.T) {
	t.Parallel()
	if specStatePassed(types.SpecStateTimedout) {
		t.Error("specStatePassed(SpecStateTimedout) = true, want false (Sub-AC 1)")
	}
}

// TestAC1PassFailSpecStateSkippedMapsToFalse verifies SpecStateSkipped → false.
func TestAC1PassFailSpecStateSkippedMapsToFalse(t *testing.T) {
	t.Parallel()
	if specStatePassed(types.SpecStateSkipped) {
		t.Error("specStatePassed(SpecStateSkipped) = true, want false (Sub-AC 1)")
	}
}

// TestAC1PassFailSpecStatePendingMapsToFalse verifies SpecStatePending → false.
func TestAC1PassFailSpecStatePendingMapsToFalse(t *testing.T) {
	t.Parallel()
	if specStatePassed(types.SpecStatePending) {
		t.Error("specStatePassed(SpecStatePending) = true, want false (Sub-AC 1)")
	}
}

// ── 7. Exhaustive specStatePassed classification ──────────────────────────────

// TestAC1PassFailSpecStatePassed_OnlyPassedReturnsTrue exhaustively verifies that
// exactly one SpecState — SpecStatePassed — causes specStatePassed to return true,
// and all other known states return false. This guards against future Ginkgo
// additions of new states silently being treated as passing.
func TestAC1PassFailSpecStatePassed_OnlyPassedReturnsTrue(t *testing.T) {
	t.Parallel()

	passingStates := []types.SpecState{
		types.SpecStatePassed,
	}
	nonPassingStates := []types.SpecState{
		types.SpecStateFailed,
		types.SpecStatePanicked,
		types.SpecStateTimedout,
		types.SpecStateSkipped,
		types.SpecStatePending,
		types.SpecStateInterrupted,
		types.SpecStateAborted,
		types.SpecStateInvalid,
	}

	for _, state := range passingStates {
		if !specStatePassed(state) {
			t.Errorf("specStatePassed(%v) = false, want true", state)
		}
	}
	for _, state := range nonPassingStates {
		if specStatePassed(state) {
			t.Errorf("specStatePassed(%v) = true, want false", state)
		}
	}
}

// ── 8. buildProfileReport propagates Passed ───────────────────────────────────

// TestAC1PassFailBuildProfileReportSetsPassedTrue verifies that buildProfileReport
// sets TCProfile.Passed = true when spec.State == SpecStatePassed.
func TestAC1PassFailBuildProfileReportSetsPassedTrue(t *testing.T) {
	t.Parallel()

	report := types.Report{
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReportWithState("E1.1", "TestPass", 100*time.Millisecond, types.SpecStatePassed),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 1 {
		t.Fatalf("len(TCs) = %d, want 1", len(pr.TCs))
	}
	if !pr.TCs[0].Passed {
		t.Errorf("TCProfile.Passed = false for SpecStatePassed; want true (Sub-AC 1)")
	}
}

// TestAC1PassFailBuildProfileReportSetsPassedFalseForFailed verifies that
// buildProfileReport sets TCProfile.Passed = false when spec.State == SpecStateFailed.
func TestAC1PassFailBuildProfileReportSetsPassedFalseForFailed(t *testing.T) {
	t.Parallel()

	report := types.Report{
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReportWithState("E1.2", "TestFail", 200*time.Millisecond, types.SpecStateFailed),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 1 {
		t.Fatalf("len(TCs) = %d, want 1", len(pr.TCs))
	}
	if pr.TCs[0].Passed {
		t.Errorf("TCProfile.Passed = true for SpecStateFailed; want false (Sub-AC 1)")
	}
}

// TestAC1PassFailBuildProfileReportMixedPassFail verifies that buildProfileReport
// correctly assigns Passed/!Passed to each TCProfile when a suite report contains
// a mix of passing and failing specs.
func TestAC1PassFailBuildProfileReportMixedPassFail(t *testing.T) {
	t.Parallel()

	report := types.Report{
		RunTime: 10 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReportWithState("E1.1", "TestPass1", 100*time.Millisecond, types.SpecStatePassed),
			sampleTimedSpecReportWithState("E1.2", "TestFail1", 200*time.Millisecond, types.SpecStateFailed),
			sampleTimedSpecReportWithState("E1.3", "TestPass2", 150*time.Millisecond, types.SpecStatePassed),
			sampleTimedSpecReportWithState("E1.4", "TestPanic", 300*time.Millisecond, types.SpecStatePanicked),
			sampleTimedSpecReportWithState("E1.5", "TestTimeout", 400*time.Millisecond, types.SpecStateTimedout),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 5 {
		t.Fatalf("len(TCs) = %d, want 5", len(pr.TCs))
	}

	// Build a map for convenient lookup.
	byID := make(map[string]TCProfile, len(pr.TCs))
	for _, tc := range pr.TCs {
		byID[tc.TCID] = tc
	}

	// Verify each TC's Passed value.
	cases := []struct {
		id     string
		passed bool
	}{
		{"E1.1", true},
		{"E1.2", false},
		{"E1.3", true},
		{"E1.4", false},
		{"E1.5", false},
	}

	for _, want := range cases {
		got, ok := byID[want.id]
		if !ok {
			t.Errorf("TC %q missing from ProfileReport.TCs", want.id)
			continue
		}
		if got.Passed != want.passed {
			t.Errorf("TC %q: Passed = %v, want %v (Sub-AC 1)", want.id, got.Passed, want.passed)
		}
	}
}

// ── 9. ProfileCollector.Flush writes Passed field to JSON ────────────────────

// TestAC1PassFailFlushWritesPassedFieldToJSON verifies that ProfileCollector.Flush
// produces a JSON file where each TCProfile entry includes the "passed" field and
// that the value matches the spec.State.
func TestAC1PassFailFlushWritesPassedFieldToJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "passfail.json")

	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 3,
		},
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReportWithState("E2.1", "TestPassed", 100*time.Millisecond, types.SpecStatePassed),
			sampleTimedSpecReportWithState("E2.2", "TestFailed", 200*time.Millisecond, types.SpecStateFailed),
			sampleTimedSpecReportWithState("E2.3", "TestSkipped", 50*time.Millisecond, types.SpecStateSkipped),
		},
	}

	collector := newProfileCollector(profilePath, profileReportBottleneckLimit)
	if err := collector.Flush(report); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile file: %v", err)
	}

	var pr ProfileReport
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &pr); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if len(pr.TCs) != 3 {
		t.Fatalf("len(TCs) = %d, want 3", len(pr.TCs))
	}

	byID := make(map[string]TCProfile, len(pr.TCs))
	for _, tc := range pr.TCs {
		byID[tc.TCID] = tc
	}

	want := []struct {
		id     string
		passed bool
	}{
		{"E2.1", true},
		{"E2.2", false},
		{"E2.3", false},
	}
	for _, w := range want {
		got, ok := byID[w.id]
		if !ok {
			t.Errorf("TC %q missing from flushed JSON", w.id)
			continue
		}
		if got.Passed != w.passed {
			t.Errorf("TC %q: Passed = %v, want %v in flushed JSON (Sub-AC 1)", w.id, got.Passed, w.passed)
		}
	}
}

// ── 10. simulateLiveProfileCaptureHook captures Passed ───────────────────────

// TestAC1PassFailLiveProfileCaptureHookCapturesPassed verifies that the
// simulateLiveProfileCaptureHook (and by extension the real profileCollectorReportAfterEach
// hook) captures report.State and converts it to TCProfile.Passed correctly.
func TestAC1PassFailLiveProfileCaptureHookCapturesPassed(t *testing.T) {
	t.Parallel()
	capture := newLiveProfileCapture()

	// Passing spec.
	simulateLiveProfileCaptureHook(capture, types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "[TC-E3.1] TestPass",
		RunTime:      100 * time.Millisecond,
		State:        types.SpecStatePassed,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E3.1")},
		},
	})

	// Failing spec.
	simulateLiveProfileCaptureHook(capture, types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: "[TC-E3.2] TestFail",
		RunTime:      200 * time.Millisecond,
		State:        types.SpecStateFailed,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E3.2")},
		},
	})

	if capture.len() != 2 {
		t.Fatalf("expected 2 entries, got %d", capture.len())
	}

	snap := capture.snapshot()
	if snap[0].TCID != "E3.1" || !snap[0].Passed {
		t.Errorf("snap[0]: TCID=%q Passed=%v, want E3.1/true (Sub-AC 1)", snap[0].TCID, snap[0].Passed)
	}
	if snap[1].TCID != "E3.2" || snap[1].Passed {
		t.Errorf("snap[1]: TCID=%q Passed=%v, want E3.2/false (Sub-AC 1)", snap[1].TCID, snap[1].Passed)
	}
}

// ── 11. ProfileReport JSON round-trips Passed without loss ───────────────────

// TestAC1PassFailProfileReportJSONRoundTripPreservesPassed verifies that
// EncodeProfileReport / json.Unmarshal round-trip the Passed field on TCProfile
// without loss, preserving both true and false values correctly.
func TestAC1PassFailProfileReportJSONRoundTripPreservesPassed(t *testing.T) {
	t.Parallel()

	pr := ProfileReport{
		SuiteName:         "Round-Trip Suite",
		TotalSpecs:        437,
		SelectedSpecs:     2,
		SuiteRuntimeNanos: (10 * time.Second).Nanoseconds(),
		GeneratedAt:       time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
		TCs: []TCProfile{
			{TCID: "E1.1", TestName: "TestPassedTC", Passed: true, TotalNanos: (100 * time.Millisecond).Nanoseconds()},
			{TCID: "E1.2", TestName: "TestFailedTC", Passed: false, TotalNanos: (200 * time.Millisecond).Nanoseconds()},
		},
	}

	var buf bytes.Buffer
	if err := EncodeProfileReport(&buf, pr); err != nil {
		t.Fatalf("EncodeProfileReport: %v", err)
	}

	var decoded ProfileReport
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if len(decoded.TCs) != 2 {
		t.Fatalf("decoded len(TCs) = %d, want 2", len(decoded.TCs))
	}
	if !decoded.TCs[0].Passed {
		t.Errorf("TCs[0].Passed = false after round-trip, want true (Sub-AC 1)")
	}
	if decoded.TCs[1].Passed {
		t.Errorf("TCs[1].Passed = true after round-trip, want false (Sub-AC 1)")
	}
}

// ── 12. Mixed suite — per-TC Passed accuracy ─────────────────────────────────

// TestAC1PassFailMixedSuiteAccuracy is an end-to-end test of the full pipeline
// for a synthetic 437-TC report with a known pass/fail distribution. It verifies
// that the final JSON report accurately reflects each spec's outcome.
func TestAC1PassFailMixedSuiteAccuracy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "mixed-suite.json")

	var specs types.SpecReports
	wantPassed := make(map[string]bool)
	for i := 1; i <= 437; i++ {
		tcID := mkTCID(i)
		// Every third TC "fails" to exercise the false branch.
		passed := i%3 != 0
		state := types.SpecStatePassed
		if !passed {
			state = types.SpecStateFailed
		}
		specs = append(specs, sampleTimedSpecReportWithState(tcID, "Test"+tcID, time.Duration(i)*time.Millisecond, state))
		wantPassed[tcID] = passed
	}

	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 437,
		},
		RunTime:     90 * time.Second,
		SpecReports: specs,
	}

	collector := newProfileCollector(profilePath, profileReportBottleneckLimit)
	if err := collector.Flush(report); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile file: %v", err)
	}
	var pr ProfileReport
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &pr); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if len(pr.TCs) != 437 {
		t.Fatalf("len(TCs) = %d, want 437", len(pr.TCs))
	}

	for _, tc := range pr.TCs {
		wantP, ok := wantPassed[tc.TCID]
		if !ok {
			t.Errorf("unexpected TC %q in ProfileReport", tc.TCID)
			continue
		}
		if tc.Passed != wantP {
			t.Errorf("TC %q: Passed = %v, want %v (Sub-AC 1 mixed suite)", tc.TCID, tc.Passed, wantP)
		}
	}

	t.Logf("Sub-AC 1 mixed suite: %d TCs, pass/fail mapping verified", len(pr.TCs))
}

// mkTCID constructs a synthetic TC ID of the form "E{i}.1" for use in
// mixed-suite tests.
func mkTCID(i int) string {
	return "E" + itoa(i) + ".1"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	result := ""
	for i > 0 {
		result = string(rune('0'+i%10)) + result
		i /= 10
	}
	return result
}

// ── 13. "passed" JSON field is present even when false ───────────────────────

// TestAC1PassFailJSONFieldPresentWhenFalse verifies that the "passed" JSON field
// is serialised in the TCProfile output even when its value is false. This ensures
// consumers that check for field presence can always find the key, regardless of
// the test outcome.
func TestAC1PassFailJSONFieldPresentWhenFalse(t *testing.T) {
	t.Parallel()

	tc := TCProfile{
		TCID:       "E5.1",
		TestName:   "TestAlwaysFails",
		Passed:     false,
		TotalNanos: (300 * time.Millisecond).Nanoseconds(),
	}

	payload, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// The "passed" key must be present even when false.
	if !strings.Contains(string(payload), `"passed"`) {
		t.Errorf("JSON missing 'passed' field when Passed=false: %s (Sub-AC 1)", payload)
	}

	// The value must be false, not omitted.
	if !strings.Contains(string(payload), `"passed":false`) {
		t.Errorf("JSON 'passed' field is not false: %s (Sub-AC 1)", payload)
	}
}

// TestAC1PassFailJSONFieldPresentWhenTrue verifies that the "passed" JSON field
// is correctly serialised as true for passing TCs.
func TestAC1PassFailJSONFieldPresentWhenTrue(t *testing.T) {
	t.Parallel()

	tc := TCProfile{
		TCID:       "E5.2",
		TestName:   "TestAlwaysPasses",
		Passed:     true,
		TotalNanos: (150 * time.Millisecond).Nanoseconds(),
	}

	payload, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	if !strings.Contains(string(payload), `"passed":true`) {
		t.Errorf("JSON 'passed' field is not true: %s (Sub-AC 1)", payload)
	}
}

// ── 14. TotalNanos is independent of Passed value ─────────────────────────────

// TestAC1PassFailTimingIsIndependentOfPassedValue verifies that the TotalNanos
// (duration) in a TCProfile is identical regardless of whether the spec passed
// or failed, so that timing data is never distorted by test outcome.
func TestAC1PassFailTimingIsIndependentOfPassedValue(t *testing.T) {
	t.Parallel()

	const duration = 250 * time.Millisecond
	report := types.Report{
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReportWithState("E6.1", "TestPassDuration", duration, types.SpecStatePassed),
			sampleTimedSpecReportWithState("E6.2", "TestFailDuration", duration, types.SpecStateFailed),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 2 {
		t.Fatalf("len(TCs) = %d, want 2", len(pr.TCs))
	}

	byID := make(map[string]TCProfile, len(pr.TCs))
	for _, tc := range pr.TCs {
		byID[tc.TCID] = tc
	}

	passTC := byID["E6.1"]
	failTC := byID["E6.2"]

	if passTC.TotalNanos != failTC.TotalNanos {
		t.Errorf("TotalNanos: passed=%d != failed=%d; duration must be independent of test outcome (Sub-AC 1)",
			passTC.TotalNanos, failTC.TotalNanos)
	}
	if passTC.TotalNanos != duration.Nanoseconds() {
		t.Errorf("TotalNanos = %d, want %d (from spec RunTime)", passTC.TotalNanos, duration.Nanoseconds())
	}
}

// ── sampleTimedSpecReport compatibility: default state is passed ──────────────

// TestAC1PassFailLegacySampleTimedSpecReportDefaultState verifies that the
// existing sampleTimedSpecReport helper (used by many other tests) produces
// SpecReport entries that buildProfileReport treats as passing (Passed=true) by
// default, since the zero value of types.SpecState is not SpecStatePassed.
//
// Note: This test documents the actual behaviour rather than asserting a specific
// requirement, because the legacy helper does not set a State field. If the zero
// value of SpecState is not SpecStatePassed, Passed will be false for those specs.
// The test captures this edge-case for observability.
func TestAC1PassFailLegacySampleTimedSpecReportDefaultState(t *testing.T) {
	t.Parallel()

	// The existing sampleTimedSpecReport does not set State, so State is the zero
	// value. Check what buildProfileReport assigns to Passed in this case.
	report := types.Report{
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E7.1", "TestLegacy", 100*time.Millisecond),
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.TCs) != 1 {
		t.Fatalf("len(TCs) = %d, want 1", len(pr.TCs))
	}

	// Document the actual value; the test ensures we have observability into
	// the zero-state case. This is expected to be false since SpecStateInvalid
	// (the zero value of SpecState) is not SpecStatePassed.
	tc := pr.TCs[0]
	t.Logf("AC1 legacy sampleTimedSpecReport: TC %q Passed=%v (State zero-value=%v)",
		tc.TCID, tc.Passed, types.SpecStateInvalid)

	// The TCProfile must have a "passed" field (JSON structural requirement).
	payload, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"passed"`) {
		t.Errorf("JSON missing 'passed' field for legacy spec: %s", payload)
	}
}
