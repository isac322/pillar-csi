package e2e

// profile_ac63_test.go — Sub-AC 6.3: configurable N for bottleneck identification.
//
// Acceptance criteria verified here:
//
//  1. -e2e.profile.top-n flag controls the number of slowest TCs in
//     ProfileReport.Bottlenecks (JSON) and the text summary.
//  2. E2E_PROFILE_TOP_N env var is used as a fallback when the flag is not set.
//  3. -e2e.profile flag path is used when set.
//  4. E2E_PROFILE env var is used as a fallback for the profile path.
//  5. SlowSetupPhases in ProfileReport includes BeforeSuite / AfterSuite phases.
//  6. SlowSetupPhases is capped at the configured N.
//  7. SlowSetupPhases entries are ordered by TotalNanos descending.
//  8. SlowSetupPhases PctOfSuiteRuntime is in (0,100] when suite runtime > 0.
//  9. Per-TC setup phases (TCSetupNanos > 0) appear in SlowSetupPhases.
// 10. The text timing report includes a "slowest setup phases:" section when
//     setup phase data is available.
// 11. configureSuiteExecution wires SlowSetupPhaseLimit from the resolved N.
// 12. ProfileCollector.Flush passes SlowSetupPhaseLimit to buildProfileReport.

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

// ── 1. -e2e.profile.top-n flag controls bottleneck count ─────────────────────

func TestAC63TopNFlagControlsBottleneckCount(t *testing.T) {
	savedFlag := *e2eProfileTopNFlag
	savedReport := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eProfileTopNFlag = savedFlag
		*e2eTimingReportFlag = savedReport
		configureSuiteExecution(nil)
	})

	dir := t.TempDir()
	profilePath := filepath.Join(dir, "top3.json")

	*e2eProfileTopNFlag = 3
	*e2eTimingReportFlag = profilePath

	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.BottleneckLimit != 3 {
		t.Fatalf("BottleneckLimit = %d, want 3 (from -e2e.profile.top-n=3)", cfg.TimingReport.BottleneckLimit)
	}
	if cfg.TimingReport.SlowSpecLimit != 3 {
		t.Fatalf("SlowSpecLimit = %d, want 3", cfg.TimingReport.SlowSpecLimit)
	}
	if cfg.TimingReport.SlowSetupPhaseLimit != 3 {
		t.Fatalf("SlowSetupPhaseLimit = %d, want 3", cfg.TimingReport.SlowSetupPhaseLimit)
	}

	// Build a 10-TC report and verify JSON bottlenecks = 3.
	var specs types.SpecReports
	for i := 0; i < 10; i++ {
		specs = append(specs, sampleTimedSpecReport(
			fmt.Sprintf("E%d.1", i+1), "Test", time.Duration(i+1)*time.Second,
		))
	}
	report := types.Report{RunTime: 55 * time.Second, SpecReports: specs}

	collector := newProfileCollector(profilePath, cfg.TimingReport.BottleneckLimit)
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
	if len(pr.Bottlenecks) != 3 {
		t.Errorf("len(Bottlenecks) = %d, want 3 (top-n=3)", len(pr.Bottlenecks))
	}
}

// ── 2. E2E_PROFILE_TOP_N env var fallback ────────────────────────────────────

func TestAC63EnvProfileTopNFallback(t *testing.T) {
	savedFlag := *e2eProfileTopNFlag
	savedProfileFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eProfileTopNFlag = savedFlag
		*e2eTimingReportFlag = savedProfileFlag
		_ = os.Unsetenv(envProfileTopN)
		configureSuiteExecution(nil)
	})

	// Flag is 0 (unset); env var provides N=7.
	*e2eProfileTopNFlag = 0
	*e2eTimingReportFlag = filepath.Join(t.TempDir(), "p.json")
	if err := os.Setenv(envProfileTopN, "7"); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.BottleneckLimit != 7 {
		t.Fatalf("BottleneckLimit = %d, want 7 (from E2E_PROFILE_TOP_N=7)", cfg.TimingReport.BottleneckLimit)
	}
	if cfg.TimingReport.SlowSetupPhaseLimit != 7 {
		t.Fatalf("SlowSetupPhaseLimit = %d, want 7", cfg.TimingReport.SlowSetupPhaseLimit)
	}
}

func TestAC63FlagTakesPrecedenceOverEnvProfileTopN(t *testing.T) {
	savedFlag := *e2eProfileTopNFlag
	savedProfileFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eProfileTopNFlag = savedFlag
		*e2eTimingReportFlag = savedProfileFlag
		_ = os.Unsetenv(envProfileTopN)
		configureSuiteExecution(nil)
	})

	// Flag = 2; env var = 99; flag must win.
	*e2eProfileTopNFlag = 2
	*e2eTimingReportFlag = filepath.Join(t.TempDir(), "p.json")
	if err := os.Setenv(envProfileTopN, "99"); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.BottleneckLimit != 2 {
		t.Fatalf("BottleneckLimit = %d, want 2 (flag beats env var)", cfg.TimingReport.BottleneckLimit)
	}
}

// ── 3. E2E_PROFILE env var as profile path fallback ──────────────────────────

func TestAC63EnvProfilePathFallback(t *testing.T) {
	savedFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = savedFlag
		_ = os.Unsetenv(envProfilePath)
		configureSuiteExecution(nil)
	})

	dir := t.TempDir()
	wantPath := filepath.Join(dir, "env-profile.json")

	// Flag is empty; env var provides the path.
	*e2eTimingReportFlag = ""
	if err := os.Setenv(envProfilePath, wantPath); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.Enabled {
		t.Fatal("expected profiling to be enabled via E2E_PROFILE env var")
	}
	if cfg.TimingReport.ProfilePath != wantPath {
		t.Fatalf("ProfilePath = %q, want %q (from E2E_PROFILE)", cfg.TimingReport.ProfilePath, wantPath)
	}
}

func TestAC63ProfileFlagTakesPrecedenceOverEnvProfilePath(t *testing.T) {
	savedFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = savedFlag
		_ = os.Unsetenv(envProfilePath)
		configureSuiteExecution(nil)
	})

	dir := t.TempDir()
	flagPath := filepath.Join(dir, "flag-profile.json")
	envPath := filepath.Join(dir, "env-profile.json")

	*e2eTimingReportFlag = flagPath
	if err := os.Setenv(envProfilePath, envPath); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.ProfilePath != flagPath {
		t.Fatalf("ProfilePath = %q, want %q (flag beats env var)", cfg.TimingReport.ProfilePath, flagPath)
	}
}

// ── 4. Default N when neither flag nor env var is set ────────────────────────

func TestAC63DefaultNWhenNeitherFlagNorEnvSet(t *testing.T) {
	savedFlag := *e2eProfileTopNFlag
	savedProfileFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eProfileTopNFlag = savedFlag
		*e2eTimingReportFlag = savedProfileFlag
		_ = os.Unsetenv(envProfileTopN)
		configureSuiteExecution(nil)
	})

	*e2eProfileTopNFlag = 0
	*e2eTimingReportFlag = filepath.Join(t.TempDir(), "p.json")
	_ = os.Unsetenv(envProfileTopN)

	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.BottleneckLimit != profileReportBottleneckLimit {
		t.Fatalf("BottleneckLimit = %d, want %d (default)", cfg.TimingReport.BottleneckLimit, profileReportBottleneckLimit)
	}
	if cfg.TimingReport.SlowSpecLimit != defaultTimingReportSlowSpecLimit {
		t.Fatalf("SlowSpecLimit = %d, want %d (default)", cfg.TimingReport.SlowSpecLimit, defaultTimingReportSlowSpecLimit)
	}
	if cfg.TimingReport.SlowSetupPhaseLimit != defaultSlowSetupPhaseLimit {
		t.Fatalf("SlowSetupPhaseLimit = %d, want %d (default)", cfg.TimingReport.SlowSetupPhaseLimit, defaultSlowSetupPhaseLimit)
	}
}

// ── 5. SlowSetupPhases includes BeforeSuite and AfterSuite phases ─────────────

func TestAC63SlowSetupPhasesIncludesBeforeAndAfterSuite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "setup-phases.json")

	report := types.Report{
		SuiteDescription: "Phase Coverage Test",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 2,
		},
		RunTime: 20 * time.Second,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 5 * time.Second},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 2 * time.Second},
			sampleTimedSpecReport("E1.1", "TestA", 1*time.Second),
			sampleTimedSpecReport("E2.1", "TestB", 800*time.Millisecond),
		},
	}

	collector := newProfileCollector(profilePath, profileReportBottleneckLimit, defaultSlowSetupPhaseLimit)
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

	if len(pr.SlowSetupPhases) == 0 {
		t.Fatal("SlowSetupPhases is empty; expected at least before_suite and after_suite entries")
	}

	var foundBefore, foundAfter bool
	for _, p := range pr.SlowSetupPhases {
		if p.Phase == setupPhaseBeforeSuite {
			foundBefore = true
		}
		if p.Phase == "after_suite" {
			foundAfter = true
		}
	}
	if !foundBefore {
		t.Error("SlowSetupPhases missing before_suite entry")
	}
	if !foundAfter {
		t.Error("SlowSetupPhases missing after_suite entry")
	}
}

// ── 6. SlowSetupPhases is capped at the configured N ─────────────────────────

func TestAC63SlowSetupPhasesCappedAtN(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "capped.json")

	// Report with BeforeSuite + AfterSuite + 10 TC phases with TCSetupNanos > 0.
	// Limit set to 3.
	var specs types.SpecReports
	specs = append(specs,
		types.SpecReport{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 5 * time.Second},
		types.SpecReport{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 3 * time.Second},
	)

	// Create TC specs; we don't inject TCSetupNanos via sampleTimedSpecReport
	// (which doesn't add tc_timing entries), so SlowSetupPhases will only
	// contain the BeforeSuite and AfterSuite here. That's fine for capping.
	for i := 0; i < 5; i++ {
		specs = append(specs, sampleTimedSpecReport(fmt.Sprintf("E%d.1", i+1), "Test", time.Duration(i+1)*time.Second))
	}

	report := types.Report{
		RunTime:     30 * time.Second,
		SpecReports: specs,
	}

	const limit = 1
	collector := newProfileCollector(profilePath, profileReportBottleneckLimit, limit)
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

	if len(pr.SlowSetupPhases) != limit {
		t.Fatalf("len(SlowSetupPhases) = %d, want %d (capped)", len(pr.SlowSetupPhases), limit)
	}
}

// ── 7. SlowSetupPhases is ordered by TotalNanos descending ───────────────────

func TestAC63SlowSetupPhasesOrderedByDurationDescending(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "ordered.json")

	report := types.Report{
		RunTime: 30 * time.Second,
		SpecReports: types.SpecReports{
			// before_suite is slower than after_suite — must appear first.
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 8 * time.Second},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 3 * time.Second},
			sampleTimedSpecReport("E1.1", "Test", 1*time.Second),
		},
	}

	collector := newProfileCollector(profilePath, profileReportBottleneckLimit, 5)
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

	if len(pr.SlowSetupPhases) < 2 {
		t.Fatalf("expected at least 2 setup phases, got %d", len(pr.SlowSetupPhases))
	}
	for i := 1; i < len(pr.SlowSetupPhases); i++ {
		if pr.SlowSetupPhases[i].TotalNanos > pr.SlowSetupPhases[i-1].TotalNanos {
			t.Errorf("SlowSetupPhases not sorted descending: [%d].TotalNanos=%d > [%d].TotalNanos=%d",
				i, pr.SlowSetupPhases[i].TotalNanos, i-1, pr.SlowSetupPhases[i-1].TotalNanos)
		}
	}
	// The slowest phase must be before_suite (8s).
	if pr.SlowSetupPhases[0].Phase != setupPhaseBeforeSuite {
		t.Errorf("SlowSetupPhases[0].Phase = %q, want %q (slowest)",
			pr.SlowSetupPhases[0].Phase, setupPhaseBeforeSuite)
	}
}

// ── 8. SlowSetupPhases PctOfSuiteRuntime is in (0, 100] ─────────────────────

func TestAC63SlowSetupPhasesPctOfSuiteRuntimeIsValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "pct.json")

	report := types.Report{
		RunTime: 20 * time.Second,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 5 * time.Second},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 2 * time.Second},
			sampleTimedSpecReport("E1.1", "Test", 1*time.Second),
		},
	}

	collector := newProfileCollector(profilePath, profileReportBottleneckLimit, 5)
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

	for _, p := range pr.SlowSetupPhases {
		if p.PctOfSuiteRuntime <= 0 {
			t.Errorf("SlowSetupPhases[rank=%d].PctOfSuiteRuntime = %.2f, want > 0",
				p.Rank, p.PctOfSuiteRuntime)
		}
		if p.PctOfSuiteRuntime > 100 {
			t.Errorf("SlowSetupPhases[rank=%d].PctOfSuiteRuntime = %.2f, want <= 100",
				p.Rank, p.PctOfSuiteRuntime)
		}
	}

	// before_suite: 5s / 20s = 25%.
	got := pr.SlowSetupPhases[0].PctOfSuiteRuntime
	want := 25.0
	if abs(got-want) > 0.01 {
		t.Errorf("SlowSetupPhases[0].PctOfSuiteRuntime = %.4f, want %.4f", got, want)
	}
}

// ── 9. Per-TC setup phases appear in SlowSetupPhases ─────────────────────────

func TestAC63TCSetupPhasesIncludedInSlowSetupPhases(t *testing.T) {
	t.Parallel()
	// Build TCProfile entries with TCSetupNanos populated.
	tcs := []TCProfile{
		{TCID: "E1.1", Phases: PhaseTimings{TCSetupNanos: (100 * time.Millisecond).Nanoseconds()}},
		{TCID: "E1.2", Phases: PhaseTimings{TCSetupNanos: (50 * time.Millisecond).Nanoseconds()}},
		{TCID: "E1.3", Phases: PhaseTimings{TCSetupNanos: 0}}, // excluded
	}

	report := types.Report{RunTime: 10 * time.Second}
	phases := buildSlowSetupPhases(report, tcs, 5)

	// Should have 2 entries: E1.1 and E1.2 (E1.3 has zero setup time).
	if len(phases) != 2 {
		t.Fatalf("len(SlowSetupPhases) = %d, want 2 (only TCs with TCSetupNanos > 0)", len(phases))
	}
	// E1.1 is slower (100ms) → rank 1.
	if phases[0].TCID != "E1.1" {
		t.Errorf("phases[0].TCID = %q, want E1.1 (slowest setup)", phases[0].TCID)
	}
	if phases[0].Phase != "tc_setup" {
		t.Errorf("phases[0].Phase = %q, want tc_setup", phases[0].Phase)
	}
	if phases[1].TCID != "E1.2" {
		t.Errorf("phases[1].TCID = %q, want E1.2", phases[1].TCID)
	}
}

// ── 10. Text report includes "slowest setup phases:" section ─────────────────

func TestAC63TextReportIncludesSetupPhaseSection(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:             true,
			Output:              &sink,
			SlowSpecLimit:       3,
			SlowSetupPhaseLimit: 3,
		},
	}

	report := types.Report{
		SuiteDescription: "Setup Phase Text Test",
		PreRunStats: types.PreRunStats{
			TotalSpecs:       437,
			SpecsThatWillRun: 2,
		},
		RunTime: 20 * time.Second,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 6 * time.Second},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 2 * time.Second},
			sampleTimedSpecReport("E1.1", "TestFoo", 3*time.Second),
			sampleTimedSpecReport("E2.1", "TestBar", 1*time.Second),
		},
	}

	if err := emitSuiteTimingReport(cfg, report); err != nil {
		t.Fatalf("emitSuiteTimingReport: %v", err)
	}

	output := sink.String()
	if !strings.Contains(output, "slowest setup phases:") {
		t.Errorf("text report missing 'slowest setup phases:' section; got:\n%s", output)
	}
	if !strings.Contains(output, setupPhaseBeforeSuite) {
		t.Errorf("text report missing before_suite phase; got:\n%s", output)
	}
}

func TestAC63TextReportOmitsSetupPhaseSectionWhenNoData(t *testing.T) {
	var sink bytes.Buffer
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:             true,
			Output:              &sink,
			SlowSpecLimit:       3,
			SlowSetupPhaseLimit: 3,
		},
	}

	// Report with no BeforeSuite / AfterSuite nodes.
	report := types.Report{
		SuiteDescription: "No Setup Phases",
		PreRunStats:      types.PreRunStats{TotalSpecs: 10, SpecsThatWillRun: 2},
		RunTime:          5 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E1.1", "TestFoo", 2*time.Second),
			sampleTimedSpecReport("E2.1", "TestBar", 1*time.Second),
		},
	}

	if err := emitSuiteTimingReport(cfg, report); err != nil {
		t.Fatalf("emitSuiteTimingReport: %v", err)
	}

	output := sink.String()
	if strings.Contains(output, "slowest setup phases:") {
		t.Errorf("text report should omit setup phases section when no data; got:\n%s", output)
	}
}

// ── 11. configureSuiteExecution wires SlowSetupPhaseLimit from resolved N ────

func TestAC63ConfigureSuiteWiresSlowSetupPhaseLimitFromN(t *testing.T) {
	savedFlag := *e2eProfileTopNFlag
	savedProfileFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eProfileTopNFlag = savedFlag
		*e2eTimingReportFlag = savedProfileFlag
		configureSuiteExecution(nil)
	})

	*e2eProfileTopNFlag = 8
	*e2eTimingReportFlag = filepath.Join(t.TempDir(), "p.json")

	configureSuiteExecution(nil)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.SlowSetupPhaseLimit != 8 {
		t.Fatalf("SlowSetupPhaseLimit = %d, want 8 (from top-n=8)", cfg.TimingReport.SlowSetupPhaseLimit)
	}
	if cfg.TimingReport.BottleneckLimit != 8 {
		t.Fatalf("BottleneckLimit = %d, want 8", cfg.TimingReport.BottleneckLimit)
	}
	if cfg.TimingReport.SlowSpecLimit != 8 {
		t.Fatalf("SlowSpecLimit = %d, want 8", cfg.TimingReport.SlowSpecLimit)
	}
}

// ── 12. ProfileCollector.Flush passes SlowSetupPhaseLimit to buildProfileReport

func TestAC63ProfileCollectorFlushPassesSetupPhaseLimitToReport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "setup-limit.json")

	const setupPhaseLimit = 1

	report := types.Report{
		RunTime: 30 * time.Second,
		SpecReports: types.SpecReports{
			// Two BeforeSuite nodes (e.g. from parallel workers) → would produce 2 entries.
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 10 * time.Second},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 5 * time.Second},
			sampleTimedSpecReport("E1.1", "Test", 1*time.Second),
		},
	}

	// Passing setupPhaseLimit=1 should cap SlowSetupPhases at 1.
	collector := newProfileCollector(profilePath, profileReportBottleneckLimit, setupPhaseLimit)
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
	if len(pr.SlowSetupPhases) != setupPhaseLimit {
		t.Errorf("len(SlowSetupPhases) = %d, want %d (capped by collector limit)",
			len(pr.SlowSetupPhases), setupPhaseLimit)
	}
}

// ── 13. Rank ordering in SlowSetupPhases ────────────────────────────────────

func TestAC63SlowSetupPhasesRankIsSequential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "ranks.json")

	report := types.Report{
		RunTime: 20 * time.Second,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 8 * time.Second},
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 3 * time.Second},
			sampleTimedSpecReport("E1.1", "Test", 1*time.Second),
		},
	}

	collector := newProfileCollector(profilePath, profileReportBottleneckLimit, 5)
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

	for i, p := range pr.SlowSetupPhases {
		if p.Rank != i+1 {
			t.Errorf("SlowSetupPhases[%d].Rank = %d, want %d", i, p.Rank, i+1)
		}
	}
}

// ── 14. resolveProfileTopN direct unit tests ─────────────────────────────────

func TestAC63ResolveProfileTopNFromFlag(t *testing.T) {
	saved := *e2eProfileTopNFlag
	t.Cleanup(func() { *e2eProfileTopNFlag = saved })

	*e2eProfileTopNFlag = 12
	got := resolveProfileTopN()
	if got != 12 {
		t.Fatalf("resolveProfileTopN() = %d, want 12 (from flag)", got)
	}
}

func TestAC63ResolveProfileTopNFromEnv(t *testing.T) {
	saved := *e2eProfileTopNFlag
	t.Cleanup(func() {
		*e2eProfileTopNFlag = saved
		_ = os.Unsetenv(envProfileTopN)
	})

	*e2eProfileTopNFlag = 0
	if err := os.Setenv(envProfileTopN, "15"); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	got := resolveProfileTopN()
	if got != 15 {
		t.Fatalf("resolveProfileTopN() = %d, want 15 (from env)", got)
	}
}

func TestAC63ResolveProfileTopNDefaultsToZero(t *testing.T) {
	saved := *e2eProfileTopNFlag
	t.Cleanup(func() {
		*e2eProfileTopNFlag = saved
		_ = os.Unsetenv(envProfileTopN)
	})

	*e2eProfileTopNFlag = 0
	_ = os.Unsetenv(envProfileTopN)

	got := resolveProfileTopN()
	if got != 0 {
		t.Fatalf("resolveProfileTopN() = %d, want 0 (both unset)", got)
	}
}

func TestAC63ResolveProfileTopNIgnoresNegativeFlag(t *testing.T) {
	saved := *e2eProfileTopNFlag
	t.Cleanup(func() { *e2eProfileTopNFlag = saved })

	*e2eProfileTopNFlag = -3
	got := resolveProfileTopN()
	if got != 0 {
		t.Fatalf("resolveProfileTopN() = %d, want 0 (negative flag treated as unset)", got)
	}
}

// ── 15. resolveProfilePath direct unit tests ──────────────────────────────────

func TestAC63ResolveProfilePathFromFlag(t *testing.T) {
	saved := *e2eTimingReportFlag
	t.Cleanup(func() { *e2eTimingReportFlag = saved })

	*e2eTimingReportFlag = "/tmp/flag-path.json"
	got := resolveProfilePath()
	if got != "/tmp/flag-path.json" {
		t.Fatalf("resolveProfilePath() = %q, want /tmp/flag-path.json", got)
	}
}

func TestAC63ResolveProfilePathFromEnv(t *testing.T) {
	saved := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = saved
		_ = os.Unsetenv(envProfilePath)
	})

	*e2eTimingReportFlag = ""
	if err := os.Setenv(envProfilePath, "/tmp/env-path.json"); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	got := resolveProfilePath()
	if got != "/tmp/env-path.json" {
		t.Fatalf("resolveProfilePath() = %q, want /tmp/env-path.json", got)
	}
}

func TestAC63ResolveProfilePathEmptyWhenBothUnset(t *testing.T) {
	saved := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = saved
		_ = os.Unsetenv(envProfilePath)
	})

	*e2eTimingReportFlag = ""
	_ = os.Unsetenv(envProfilePath)

	got := resolveProfilePath()
	if got != "" {
		t.Fatalf("resolveProfilePath() = %q, want empty when both unset", got)
	}
}

// ── 16. SetupPhaseBottleneck type accessors ───────────────────────────────────

func TestAC63SetupPhaseBottleneckTotalDurationAccessor(t *testing.T) {
	t.Parallel()
	b := SetupPhaseBottleneck{TotalNanos: (250 * time.Millisecond).Nanoseconds()}
	if b.TotalDuration() != 250*time.Millisecond {
		t.Errorf("TotalDuration() = %s, want 250ms", b.TotalDuration())
	}
}

// ── 17. ProfileReport SlowSetupPhases round-trips through JSON ────────────────

func TestAC63ProfileReportSlowSetupPhasesRoundTripJSON(t *testing.T) {
	t.Parallel()
	pr := ProfileReport{
		SuiteName: "Round-trip Test",
		SlowSetupPhases: []SetupPhaseBottleneck{
			{
				Rank:              1,
				Phase:             setupPhaseBeforeSuite,
				TotalNanos:        (5 * time.Second).Nanoseconds(),
				PctOfSuiteRuntime: 25.0,
			},
			{
				Rank:              2,
				Phase:             "tc_setup",
				TCID:              "E1.1",
				TotalNanos:        (100 * time.Millisecond).Nanoseconds(),
				PctOfSuiteRuntime: 0.5,
			},
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

	if len(decoded.SlowSetupPhases) != 2 {
		t.Fatalf("len(SlowSetupPhases) = %d, want 2", len(decoded.SlowSetupPhases))
	}
	if decoded.SlowSetupPhases[0].Phase != setupPhaseBeforeSuite {
		t.Errorf("SlowSetupPhases[0].Phase = %q, want %q", decoded.SlowSetupPhases[0].Phase, setupPhaseBeforeSuite)
	}
	if decoded.SlowSetupPhases[1].TCID != "E1.1" {
		t.Errorf("SlowSetupPhases[1].TCID = %q, want E1.1", decoded.SlowSetupPhases[1].TCID)
	}
	if decoded.SlowSetupPhases[1].Phase != "tc_setup" {
		t.Errorf("SlowSetupPhases[1].Phase = %q, want tc_setup", decoded.SlowSetupPhases[1].Phase)
	}
}

// ── 18. buildSlowSetupPhases unit tests ──────────────────────────────────────

func TestAC63BuildSlowSetupPhasesEmptyWhenNoData(t *testing.T) {
	t.Parallel()
	report := types.Report{RunTime: 5 * time.Second}
	phases := buildSlowSetupPhases(report, nil, 5)
	if phases != nil {
		t.Fatalf("expected nil SlowSetupPhases when no data, got %v", phases)
	}
}

func TestAC63BuildSlowSetupPhasesIgnoresZeroRuntimePhases(t *testing.T) {
	t.Parallel()
	report := types.Report{
		RunTime: 10 * time.Second,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, RunTime: 0}, // zero — excluded
			{LeafNodeType: types.NodeTypeAfterSuite, RunTime: 3 * time.Second},
		},
	}

	phases := buildSlowSetupPhases(report, nil, 5)
	if len(phases) != 1 {
		t.Fatalf("expected 1 phase (AfterSuite; BeforeSuite with 0 runtime excluded), got %d", len(phases))
	}
	if phases[0].Phase != "after_suite" {
		t.Errorf("phases[0].Phase = %q, want after_suite", phases[0].Phase)
	}
}

func TestAC63BuildSlowSetupPhasesSynchronizedBeforeSuiteIsIncluded(t *testing.T) {
	t.Parallel()
	report := types.Report{
		RunTime: 10 * time.Second,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeSynchronizedBeforeSuite, RunTime: 4 * time.Second},
			{LeafNodeType: types.NodeTypeSynchronizedAfterSuite, RunTime: 2 * time.Second},
		},
	}

	phases := buildSlowSetupPhases(report, nil, 5)
	if len(phases) != 2 {
		t.Fatalf("expected 2 phases (Synchronized* variants), got %d", len(phases))
	}
	// SynchronizedBeforeSuite → setupPhaseBeforeSuite.
	if phases[0].Phase != setupPhaseBeforeSuite {
		t.Errorf("phases[0].Phase = %q, want %q", phases[0].Phase, setupPhaseBeforeSuite)
	}
}
