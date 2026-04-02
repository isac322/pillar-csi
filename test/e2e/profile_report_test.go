package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

func TestConfigureSuiteExecutionDefaultsTimingReportingOff(t *testing.T) {
	original := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = original
		configureSuiteExecution(nil)
	})

	*e2eTimingReportFlag = "" // empty path → disabled
	var sink bytes.Buffer

	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.Enabled {
		t.Fatal("expected timing report to be disabled by default")
	}
	if cfg.TimingReport.ProfilePath != "" {
		t.Fatalf("ProfilePath = %q, want empty when disabled", cfg.TimingReport.ProfilePath)
	}
	if cfg.TimingReport.SlowSpecLimit != defaultTimingReportSlowSpecLimit {
		t.Fatalf("SlowSpecLimit = %d, want %d", cfg.TimingReport.SlowSpecLimit, defaultTimingReportSlowSpecLimit)
	}
	if _, err := cfg.TimingReport.Output.Write([]byte("ignored")); err != nil {
		t.Fatalf("write discard timing output: %v", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("disabled timing report wrote %q", sink.String())
	}
}

func TestConfigureSuiteExecutionEnablesTimingReportingWhenFlagSet(t *testing.T) {
	original := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = original
		configureSuiteExecution(nil)
	})

	wantPath := filepath.Join(t.TempDir(), "profile.json")
	*e2eTimingReportFlag = wantPath
	var sink bytes.Buffer

	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.Enabled {
		t.Fatal("expected timing report to be enabled")
	}
	if cfg.TimingReport.ProfilePath != wantPath {
		t.Fatalf("ProfilePath = %q, want %q", cfg.TimingReport.ProfilePath, wantPath)
	}
	if _, err := cfg.TimingReport.Output.Write([]byte("enabled")); err != nil {
		t.Fatalf("write timing output: %v", err)
	}
	if sink.String() != "enabled" {
		t.Fatalf("timing output = %q, want %q", sink.String(), "enabled")
	}
}

func TestEmitSuiteTimingReportIncludesSlowestTCsAndBottleneck(t *testing.T) {
	var sink bytes.Buffer
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:       true,
			Output:        &sink,
			SlowSpecLimit: 2,
		},
	}

	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 11,
		},
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E2.3", "TestFast", 800*time.Millisecond),
			sampleTimedSpecReport("F27.1", "TestSlow", 3*time.Second),
			sampleTimedSpecReport("E19.4", "TestMid", 1500*time.Millisecond),
			{
				LeafNodeText: "BeforeSuite",
				RunTime:      4 * time.Second,
			},
		},
	}

	if err := emitSuiteTimingReport(cfg, report); err != nil {
		t.Fatalf("emitSuiteTimingReport: %v", err)
	}

	output := sink.String()
	for _, want := range []string{
		"=== E2E Timing Profile ===",
		"suite: Pillar CSI E2E Suite",
		"selected specs: 11/437",
		"1. F27.1 :: TestSlow (3s)",
		"2. E19.4 :: TestMid (1.5s)",
		"bottleneck: slowest TC F27.1 consumed 60.0% of suite runtime",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("timing report missing %q in output:\n%s", want, output)
		}
	}
	if strings.Contains(output, "E2.3 :: TestFast") {
		t.Fatalf("timing report exceeded slow spec limit:\n%s", output)
	}
	if strings.Contains(output, "BeforeSuite") {
		t.Fatalf("timing report should ignore non-TC suite nodes:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// Tests for the new JSON ProfileReport types (TCProfile, PhaseTimings,
// BottleneckEntry, ProfileReport) and buildProfileReport.
// ---------------------------------------------------------------------------

func TestBuildProfileReportContainsAllTCsAndBottlenecks(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 4,
		},
		RunTime: 10 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E2.3", "TestFast", 800*time.Millisecond),
			sampleTimedSpecReport("F27.1", "TestSlow", 3*time.Second),
			sampleTimedSpecReport("E19.4", "TestMid", 1500*time.Millisecond),
			sampleTimedSpecReport("A1.1", "TestAnother", 2*time.Second),
			// Non-TC spec (no tc_id) – must be excluded from TCs.
			{LeafNodeText: "BeforeSuite", RunTime: 500 * time.Millisecond},
		},
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if pr.SuiteName != "Pillar CSI E2E Suite" {
		t.Errorf("SuiteName = %q, want %q", pr.SuiteName, "Pillar CSI E2E Suite")
	}
	if pr.TotalSpecs != 437 {
		t.Errorf("TotalSpecs = %d, want 437", pr.TotalSpecs)
	}
	if pr.SelectedSpecs != 4 {
		t.Errorf("SelectedSpecs = %d, want 4", pr.SelectedSpecs)
	}
	if pr.SuiteRuntimeNanos != (10 * time.Second).Nanoseconds() {
		t.Errorf("SuiteRuntimeNanos = %d", pr.SuiteRuntimeNanos)
	}
	if len(pr.TCs) != 4 {
		t.Errorf("len(TCs) = %d, want 4", len(pr.TCs))
	}
	// TCs should be sorted by duration descending.
	if pr.TCs[0].TCID != "F27.1" {
		t.Errorf("TCs[0].TCID = %q, want F27.1", pr.TCs[0].TCID)
	}
	if pr.TCs[1].TCID != "A1.1" {
		t.Errorf("TCs[1].TCID = %q, want A1.1", pr.TCs[1].TCID)
	}
	// All 4 TCs are slower than the limit of 5, so all 4 appear as bottlenecks.
	if len(pr.Bottlenecks) != 4 {
		t.Errorf("len(Bottlenecks) = %d, want 4", len(pr.Bottlenecks))
	}
	if pr.Bottlenecks[0].Rank != 1 {
		t.Errorf("Bottlenecks[0].Rank = %d, want 1", pr.Bottlenecks[0].Rank)
	}
	if pr.Bottlenecks[0].TCID != "F27.1" {
		t.Errorf("Bottlenecks[0].TCID = %q, want F27.1", pr.Bottlenecks[0].TCID)
	}
	// PctOfSuiteRuntime for F27.1: 3s / 10s = 30 %
	wantPct := 30.0
	if pr.Bottlenecks[0].PctOfSuiteRuntime != wantPct {
		t.Errorf("Bottlenecks[0].PctOfSuiteRuntime = %.1f, want %.1f",
			pr.Bottlenecks[0].PctOfSuiteRuntime, wantPct)
	}
}

func TestBuildProfileReportBottleneckLimitCappedAtFive(t *testing.T) {
	var specs types.SpecReports
	for i := 0; i < 10; i++ {
		tcID := fmt.Sprintf("E%d.1", i+1)
		specs = append(specs, sampleTimedSpecReport(tcID, "Test", time.Duration(i+1)*time.Second))
	}

	report := types.Report{
		RunTime:     55 * time.Second,
		SpecReports: specs,
	}

	pr := buildProfileReport(report, profileReportBottleneckLimit)

	if len(pr.Bottlenecks) != profileReportBottleneckLimit {
		t.Errorf("len(Bottlenecks) = %d, want %d", len(pr.Bottlenecks), profileReportBottleneckLimit)
	}
}

func TestProfileReportRoundTripJSON(t *testing.T) {
	pr := ProfileReport{
		SuiteName:         "Test Suite",
		TotalSpecs:        437,
		SelectedSpecs:     10,
		SuiteRuntimeNanos: (90 * time.Second).Nanoseconds(),
		GeneratedAt:       time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		TCs: []TCProfile{
			{
				TCID:       "E1.2",
				Category:   "Type-E",
				TestName:   "TestSomething",
				TotalNanos: (500 * time.Millisecond).Nanoseconds(),
				Phases: PhaseTimings{
					TCSetupNanos:    (50 * time.Millisecond).Nanoseconds(),
					TCExecuteNanos:  (400 * time.Millisecond).Nanoseconds(),
					TCTeardownNanos: (50 * time.Millisecond).Nanoseconds(),
				},
			},
		},
		Bottlenecks: []BottleneckEntry{
			{Rank: 1, TCID: "E1.2", TotalNanos: (500 * time.Millisecond).Nanoseconds(), PctOfSuiteRuntime: 0.56},
		},
	}

	var buf bytes.Buffer
	if err := EncodeProfileReport(&buf, pr); err != nil {
		t.Fatalf("EncodeProfileReport: %v", err)
	}

	// Output should be a valid single-line JSON terminated by \n.
	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("encoded JSON missing trailing newline")
	}

	var decoded ProfileReport
	if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.SuiteName != pr.SuiteName {
		t.Errorf("SuiteName = %q, want %q", decoded.SuiteName, pr.SuiteName)
	}
	if len(decoded.TCs) != 1 {
		t.Fatalf("len(TCs) = %d, want 1", len(decoded.TCs))
	}
	if decoded.TCs[0].TCID != "E1.2" {
		t.Errorf("TCs[0].TCID = %q, want E1.2", decoded.TCs[0].TCID)
	}
	if decoded.TCs[0].Phases.TCExecute() != 400*time.Millisecond {
		t.Errorf("TCExecute = %s, want 400ms", decoded.TCs[0].Phases.TCExecute())
	}
	if len(decoded.Bottlenecks) != 1 {
		t.Fatalf("len(Bottlenecks) = %d, want 1", len(decoded.Bottlenecks))
	}
	if decoded.Bottlenecks[0].Rank != 1 {
		t.Errorf("Bottlenecks[0].Rank = %d, want 1", decoded.Bottlenecks[0].Rank)
	}
}

func TestPhaseTimingsDurationAccessors(t *testing.T) {
	pt := PhaseTimings{
		GroupSetupNanos:    int64(10 * time.Millisecond),
		TCSetupNanos:       int64(20 * time.Millisecond),
		TCExecuteNanos:     int64(300 * time.Millisecond),
		TCTeardownNanos:    int64(15 * time.Millisecond),
		GroupTeardownNanos: int64(5 * time.Millisecond),
	}

	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"GroupSetup", pt.GroupSetup(), 10 * time.Millisecond},
		{"TCSetup", pt.TCSetup(), 20 * time.Millisecond},
		{"TCExecute", pt.TCExecute(), 300 * time.Millisecond},
		{"TCTeardown", pt.TCTeardown(), 15 * time.Millisecond},
		{"GroupTeardown", pt.GroupTeardown(), 5 * time.Millisecond},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestEmitSuiteTimingReportEmitsTextSummaryOnly(t *testing.T) {
	// emitSuiteTimingReport now emits only the human-readable text summary to
	// cfg.TimingReport.Output. The JSON ProfileReport is written separately by
	// ProfileCollector.Flush via the ReportAfterSuite hook.
	var sink bytes.Buffer
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:         true,
			ProfilePath:     filepath.Join(t.TempDir(), "profile.json"),
			Output:          &sink,
			SlowSpecLimit:   2,
			BottleneckLimit: profileReportBottleneckLimit,
		},
	}

	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 2,
		},
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("F27.1", "TestSlow", 3*time.Second),
			sampleTimedSpecReport("E2.3", "TestFast", 800*time.Millisecond),
		},
	}

	if err := emitSuiteTimingReport(cfg, report); err != nil {
		t.Fatalf("emitSuiteTimingReport: %v", err)
	}

	output := sink.String()

	// Must contain the legacy text summary.
	if !strings.Contains(output, "=== E2E Timing Profile ===") {
		t.Error("missing text summary header")
	}
	if !strings.Contains(output, "F27.1") {
		t.Error("missing slowest TC F27.1 in text summary")
	}

	// Must NOT contain a JSON line – JSON is now written by ProfileCollector.Flush.
	for _, l := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "{") {
			t.Fatalf("emitSuiteTimingReport must not emit JSON to Output; found: %s", l)
		}
	}
}

func TestProfileCollectorFlushWritesJSONToFile(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.json")

	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 2,
		},
		RunTime: 5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("F27.1", "TestSlow", 3*time.Second),
			sampleTimedSpecReport("E2.3", "TestFast", 800*time.Millisecond),
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

	// File must contain a single JSON line terminated by \n.
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("profile file missing trailing newline")
	}

	var pr ProfileReport
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &pr); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if pr.SuiteName != "Pillar CSI E2E Suite" {
		t.Errorf("SuiteName = %q, want %q", pr.SuiteName, "Pillar CSI E2E Suite")
	}
	if len(pr.TCs) != 2 {
		t.Errorf("len(TCs) = %d, want 2", len(pr.TCs))
	}
	if pr.TCs[0].TCID != "F27.1" {
		t.Errorf("TCs[0].TCID = %q, want F27.1 (slowest first)", pr.TCs[0].TCID)
	}
	// Bottlenecks capped at min(5, 2) = 2.
	if len(pr.Bottlenecks) != 2 {
		t.Errorf("len(Bottlenecks) = %d, want 2", len(pr.Bottlenecks))
	}
	if pr.Bottlenecks[0].Rank != 1 {
		t.Errorf("Bottlenecks[0].Rank = %d, want 1", pr.Bottlenecks[0].Rank)
	}
	if pr.Bottlenecks[0].TCID != "F27.1" {
		t.Errorf("Bottlenecks[0].TCID = %q, want F27.1", pr.Bottlenecks[0].TCID)
	}
}

func TestProfileCollectorFlushComputesTop5Bottlenecks(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.json")

	var specs types.SpecReports
	for i := 0; i < 10; i++ {
		tcID := fmt.Sprintf("E%d.1", i+1)
		specs = append(specs, sampleTimedSpecReport(tcID, "Test", time.Duration(i+1)*time.Second))
	}

	report := types.Report{
		RunTime:     55 * time.Second,
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

	// Bottlenecks must be capped at exactly 5.
	if len(pr.Bottlenecks) != profileReportBottleneckLimit {
		t.Errorf("len(Bottlenecks) = %d, want %d", len(pr.Bottlenecks), profileReportBottleneckLimit)
	}

	// Rank 1 must be the slowest TC (E10.1 = 10s).
	if pr.Bottlenecks[0].Rank != 1 {
		t.Errorf("Bottlenecks[0].Rank = %d, want 1", pr.Bottlenecks[0].Rank)
	}
	if pr.Bottlenecks[0].TCID != "E10.1" {
		t.Errorf("Bottlenecks[0].TCID = %q, want E10.1", pr.Bottlenecks[0].TCID)
	}
	// Rank 5 must be E6.1 (6s).
	if pr.Bottlenecks[4].TCID != "E6.1" {
		t.Errorf("Bottlenecks[4].TCID = %q, want E6.1", pr.Bottlenecks[4].TCID)
	}

	// All 10 TCs must appear in pr.TCs.
	if len(pr.TCs) != 10 {
		t.Errorf("len(TCs) = %d, want 10", len(pr.TCs))
	}
}

func TestProfileCollectorFlushNoOpWhenPathEmpty(t *testing.T) {
	collector := newProfileCollector("", profileReportBottleneckLimit)
	if err := collector.Flush(types.Report{}); err != nil {
		t.Fatalf("Flush with empty path should be no-op, got error: %v", err)
	}
}

func TestProfileCollectorFlushNoOpOnNilReceiver(t *testing.T) {
	var collector *ProfileCollector
	if err := collector.Flush(types.Report{}); err != nil {
		t.Fatalf("Flush on nil receiver should be no-op, got error: %v", err)
	}
}

func TestProfileCollectorFlushErrorsWhenDirMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent-subdir", "profile.json")
	collector := newProfileCollector(path, profileReportBottleneckLimit)
	if err := collector.Flush(types.Report{}); err == nil {
		t.Fatal("expected error when parent directory does not exist, got nil")
	}
}

func sampleTimedSpecReport(tcID, testName string, duration time.Duration) types.SpecReport {
	return types.SpecReport{
		LeafNodeText: testName,
		RunTime:      duration,
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
