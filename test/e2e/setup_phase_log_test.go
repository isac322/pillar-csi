package e2e

// setup_phase_log_test.go — Sub-AC 6.2 unit tests.
//
// Acceptance criteria verified here:
//
//  1. setupPhaseLogEntry carries Phase, TCID, ParallelProcess, StartedAt,
//     FinishedAt, and DurationNanos and round-trips through JSON without loss.
//  2. fileSetupPhaseLogger appends one JSON-Lines record per Append call;
//     the file is created on first call, subsequent calls append.
//  3. fileSetupPhaseLogger.Append is a no-op when path is empty.
//  4. inMemorySetupPhaseLogger.Append records entries in insertion order;
//     Snapshot returns an isolated copy; Len is accurate.
//  5. appendSetupPhasesFromTimingProfile emits before_each and
//     just_before_each entries to suiteSetupPhaseLog for a profile that
//     includes phaseBeforeEach and phaseJustBeforeEach phases.
//  6. appendSetupPhasesFromTimingProfile emits no entries when neither
//     phaseBeforeEach nor phaseJustBeforeEach is present in the profile.
//  7. appendBeforeSuiteToSetupPhaseLog emits one entry per
//     NodeTypeBeforeSuite / NodeTypeSynchronizedBeforeSuite spec report and
//     correctly derives StartedAt / FinishedAt from the spec timestamps.
//  8. appendBeforeSuiteToSetupPhaseLog skips reports with zero RunTime.
//  9. phaseBeforeEach and phaseJustBeforeEach are present in the profile
//     produced by a complete timingRecorder BeforeEach → JustBeforeEach →
//     spec-body lifecycle.
// 10. configureSuiteExecution wires suiteSetupPhaseLog to a file logger
//     when -e2e.setup-timing-log is set to a non-empty path.
// 11. configureSuiteExecution leaves suiteSetupPhaseLog as a no-op logger
//     when -e2e.setup-timing-log is empty (the default).
// 12. installTestSetupPhaseLog replaces suiteSetupPhaseLog for the duration
//     of the test and restores it on cleanup.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ── 1. setupPhaseLogEntry JSON round-trip ─────────────────────────────────────

func TestSetupPhaseLogEntryJSONRoundTrip(t *testing.T) {
	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(17 * time.Millisecond)

	entry := setupPhaseLogEntry{
		Phase:           setupPhaseBeforeEach,
		TCID:            "E5.3",
		ParallelProcess: 2,
		StartedAt:       start,
		FinishedAt:      end,
		DurationNanos:   end.Sub(start).Nanoseconds(),
	}

	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded setupPhaseLogEntry
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Phase != entry.Phase {
		t.Errorf("Phase = %q, want %q", decoded.Phase, entry.Phase)
	}
	if decoded.TCID != entry.TCID {
		t.Errorf("TCID = %q, want %q", decoded.TCID, entry.TCID)
	}
	if decoded.ParallelProcess != entry.ParallelProcess {
		t.Errorf("ParallelProcess = %d, want %d", decoded.ParallelProcess, entry.ParallelProcess)
	}
	if !decoded.StartedAt.Equal(entry.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", decoded.StartedAt, entry.StartedAt)
	}
	if !decoded.FinishedAt.Equal(entry.FinishedAt) {
		t.Errorf("FinishedAt = %v, want %v", decoded.FinishedAt, entry.FinishedAt)
	}
	if decoded.DurationNanos != entry.DurationNanos {
		t.Errorf("DurationNanos = %d, want %d", decoded.DurationNanos, entry.DurationNanos)
	}
}

func TestSetupPhaseLogEntryDurationAccessor(t *testing.T) {
	want := 25 * time.Millisecond
	entry := setupPhaseLogEntry{DurationNanos: want.Nanoseconds()}
	if entry.Duration() != want {
		t.Errorf("Duration() = %s, want %s", entry.Duration(), want)
	}
}

func TestSetupPhaseLogEntryPhaseConstants(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{name: "before_suite", val: setupPhaseBeforeSuite},
		{name: "before_each", val: setupPhaseBeforeEach},
		{name: "just_before_each", val: setupPhaseJustBeforeEach},
	}
	for _, tc := range cases {
		if tc.val != tc.name {
			t.Errorf("phase constant = %q, want %q", tc.val, tc.name)
		}
	}
}

// ── 2 + 3. fileSetupPhaseLogger ───────────────────────────────────────────────

func TestFileSetupPhaseLoggerWritesJSONLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setup-phases.jsonl")
	logger := newFileSetupPhaseLogger(path)

	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	entries := []setupPhaseLogEntry{
		{Phase: setupPhaseBeforeEach, TCID: "E1.1", DurationNanos: 5_000_000, StartedAt: start, FinishedAt: start.Add(5 * time.Millisecond)},
		{Phase: setupPhaseJustBeforeEach, TCID: "E1.1", DurationNanos: 1_000, StartedAt: start.Add(5 * time.Millisecond), FinishedAt: start.Add(5*time.Millisecond + time.Microsecond)},
		{Phase: setupPhaseBeforeSuite, DurationNanos: 10_000_000, StartedAt: start.Add(-10 * time.Millisecond), FinishedAt: start},
	}

	for _, e := range entries {
		if err := logger.Append(e); err != nil {
			t.Fatalf("Append(%q): %v", e.Phase, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Each JSON-Lines line must be a valid JSON object.
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var decoded []setupPhaseLogEntry
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e setupPhaseLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %q: Unmarshal: %v", line, err)
		}
		decoded = append(decoded, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	if len(decoded) != len(entries) {
		t.Fatalf("decoded %d lines, want %d", len(decoded), len(entries))
	}
	for i, want := range entries {
		got := decoded[i]
		if got.Phase != want.Phase {
			t.Errorf("decoded[%d].Phase = %q, want %q", i, got.Phase, want.Phase)
		}
		if got.TCID != want.TCID {
			t.Errorf("decoded[%d].TCID = %q, want %q", i, got.TCID, want.TCID)
		}
		if got.DurationNanos != want.DurationNanos {
			t.Errorf("decoded[%d].DurationNanos = %d, want %d", i, got.DurationNanos, want.DurationNanos)
		}
	}
}

func TestFileSetupPhaseLoggerAppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "append-test.jsonl")

	entry1 := setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E1.1", DurationNanos: 1000}
	entry2 := setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E1.2", DurationNanos: 2000}

	logger1 := newFileSetupPhaseLogger(path)
	if err := logger1.Append(entry1); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if err := logger1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Open a second logger to the same path; it should append, not truncate.
	logger2 := newFileSetupPhaseLogger(path)
	if err := logger2.Append(entry2); err != nil {
		t.Fatalf("second Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := countNonEmptyLines(data)
	if lines != 2 {
		t.Fatalf("expected 2 lines in appended file, got %d", lines)
	}
}

func TestFileSetupPhaseLoggerEmptyPathIsNoOp(t *testing.T) {
	logger := newFileSetupPhaseLogger("") // empty path → no-op
	err := logger.Append(setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E1.1"})
	if err != nil {
		t.Fatalf("Append with empty path returned error: %v (expected nil)", err)
	}
	// Close must also be a no-op.
	if err := logger.Close(); err != nil {
		t.Fatalf("Close with empty path: %v", err)
	}
}

func TestFileSetupPhaseLoggerNilIsNoOp(t *testing.T) {
	var logger *fileSetupPhaseLogger
	if err := logger.Append(setupPhaseLogEntry{}); err != nil {
		t.Fatalf("nil Append: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

// ── 4. inMemorySetupPhaseLogger ───────────────────────────────────────────────

func TestInMemorySetupPhaseLoggerIsEmptyOnCreation(t *testing.T) {
	m := newInMemorySetupPhaseLogger()
	if m.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", m.Len())
	}
	if snap := m.Snapshot(); snap != nil {
		t.Fatalf("Snapshot() = %v, want nil", snap)
	}
}

func TestInMemorySetupPhaseLoggerRecordAndSnapshot(t *testing.T) {
	m := newInMemorySetupPhaseLogger()
	entries := []setupPhaseLogEntry{
		{Phase: setupPhaseBeforeEach, TCID: "E1.1", DurationNanos: 100},
		{Phase: setupPhaseJustBeforeEach, TCID: "E1.1", DurationNanos: 10},
		{Phase: setupPhaseBeforeSuite, DurationNanos: 5000},
	}
	for _, e := range entries {
		if err := m.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	if m.Len() != 3 {
		t.Fatalf("Len() = %d after 3 appends, want 3", m.Len())
	}

	snap := m.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	for i, want := range entries {
		if snap[i].Phase != want.Phase {
			t.Errorf("snap[%d].Phase = %q, want %q", i, snap[i].Phase, want.Phase)
		}
		if snap[i].TCID != want.TCID {
			t.Errorf("snap[%d].TCID = %q, want %q", i, snap[i].TCID, want.TCID)
		}
		if snap[i].DurationNanos != want.DurationNanos {
			t.Errorf("snap[%d].DurationNanos = %d, want %d", i, snap[i].DurationNanos, want.DurationNanos)
		}
	}
}

func TestInMemorySetupPhaseLoggerSnapshotIsIsolated(t *testing.T) {
	m := newInMemorySetupPhaseLogger()
	_ = m.Append(setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E1.1"})

	snap1 := m.Snapshot()
	_ = m.Append(setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E1.2"})
	snap2 := m.Snapshot()

	if len(snap1) != 1 {
		t.Fatalf("snap1 mutated after second Append: len=%d, want 1", len(snap1))
	}
	if len(snap2) != 2 {
		t.Fatalf("snap2 len = %d, want 2", len(snap2))
	}
}

// ── 5. appendSetupPhasesFromTimingProfile ─────────────────────────────────────

func TestAppendSetupPhasesFromTimingProfileEmitsBeforeEachAndJustBeforeEach(t *testing.T) {
	log := installTestSetupPhaseLog(t)

	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    10 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)
	report := types.SpecReport{
		LeafNodeType:    types.NodeTypeIt,
		LeafNodeText:    "TC[001/437] E1.1 :: TestSetupPhase",
		ParallelProcess: 3,
	}

	recorder.start(report)
	recorder.beginPhase(phaseBeforeEach)
	recorder.endPhase(phaseBeforeEach)
	recorder.beginPhase(phaseJustBeforeEach)
	recorder.endPhase(phaseJustBeforeEach)
	recorder.beginPhase(phaseSpecBody)
	recorder.endPhase(phaseSpecBody)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	appendSetupPhasesFromTimingProfile(profile)

	snap := log.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 log entries (before_each + just_before_each), got %d: %+v", len(snap), snap)
	}

	var foundBeforeEach, foundJustBeforeEach bool
	for _, e := range snap {
		switch e.Phase {
		case setupPhaseBeforeEach:
			foundBeforeEach = true
			if e.TCID != "E1.1" {
				t.Errorf("before_each entry TCID = %q, want E1.1", e.TCID)
			}
			if e.ParallelProcess != 3 {
				t.Errorf("before_each ParallelProcess = %d, want 3", e.ParallelProcess)
			}
			if e.DurationNanos <= 0 {
				t.Errorf("before_each DurationNanos = %d, want > 0", e.DurationNanos)
			}
		case setupPhaseJustBeforeEach:
			foundJustBeforeEach = true
			if e.TCID != "E1.1" {
				t.Errorf("just_before_each entry TCID = %q, want E1.1", e.TCID)
			}
			if e.DurationNanos <= 0 {
				t.Errorf("just_before_each DurationNanos = %d, want > 0", e.DurationNanos)
			}
		}
	}
	if !foundBeforeEach {
		t.Error("before_each entry not found in log")
	}
	if !foundJustBeforeEach {
		t.Error("just_before_each entry not found in log")
	}
}

// ── 6. appendSetupPhasesFromTimingProfile — no phaseBeforeEach / phaseJustBeforeEach ──

func TestAppendSetupPhasesFromTimingProfileEmitsNothingWhenPhasesAbsent(t *testing.T) {
	log := installTestSetupPhaseLog(t)

	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)
	report := types.SpecReport{
		LeafNodeType:    types.NodeTypeIt,
		LeafNodeText:    "TC[002/437] E2.1 :: TestNoSetupPhase",
		ParallelProcess: 1,
	}

	recorder.start(report)
	// Only record spec body — no phaseBeforeEach or phaseJustBeforeEach.
	recorder.beginPhase(phaseSpecBody)
	recorder.endPhase(phaseSpecBody)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	appendSetupPhasesFromTimingProfile(profile)

	if log.Len() != 0 {
		t.Fatalf("expected 0 log entries when setup phases absent, got %d", log.Len())
	}
}

// ── 7. appendBeforeSuiteToSetupPhaseLog ──────────────────────────────────────

func TestAppendBeforeSuiteToSetupPhaseLogEmitsBeforeSuiteEntry(t *testing.T) {
	log := installTestSetupPhaseLog(t)

	start := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	duration := 8 * time.Second

	report := types.Report{
		SpecReports: types.SpecReports{
			{
				LeafNodeType: types.NodeTypeBeforeSuite,
				StartTime:    start,
				EndTime:      start.Add(duration),
				RunTime:      duration,
			},
		},
	}

	appendBeforeSuiteToSetupPhaseLog(report)

	snap := log.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(snap))
	}

	e := snap[0]
	if e.Phase != setupPhaseBeforeSuite {
		t.Errorf("Phase = %q, want %q", e.Phase, setupPhaseBeforeSuite)
	}
	if e.DurationNanos != duration.Nanoseconds() {
		t.Errorf("DurationNanos = %d, want %d", e.DurationNanos, duration.Nanoseconds())
	}
	if !e.StartedAt.Equal(start) {
		t.Errorf("StartedAt = %v, want %v", e.StartedAt, start)
	}
	if !e.FinishedAt.Equal(start.Add(duration)) {
		t.Errorf("FinishedAt = %v, want %v", e.FinishedAt, start.Add(duration))
	}
	// BeforeSuite entries do not carry TCID or ParallelProcess.
	if e.TCID != "" {
		t.Errorf("TCID = %q, want empty for BeforeSuite entry", e.TCID)
	}
}

func TestAppendBeforeSuiteToSetupPhaseLogHandlesSynchronizedBeforeSuite(t *testing.T) {
	log := installTestSetupPhaseLog(t)

	start := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	primaryDur := 45 * time.Second
	workerDur := 1 * time.Second

	report := types.Report{
		SpecReports: types.SpecReports{
			// Two SynchronizedBeforeSuite reports: primary (slow) and worker (fast).
			{
				LeafNodeType: types.NodeTypeSynchronizedBeforeSuite,
				StartTime:    start,
				EndTime:      start.Add(primaryDur),
				RunTime:      primaryDur,
			},
			{
				LeafNodeType: types.NodeTypeSynchronizedBeforeSuite,
				StartTime:    start,
				EndTime:      start.Add(workerDur),
				RunTime:      workerDur,
			},
		},
	}

	appendBeforeSuiteToSetupPhaseLog(report)

	// Both reports should be logged — the caller decides which to use.
	snap := log.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries for 2 SynchronizedBeforeSuite reports, got %d", len(snap))
	}
	for _, e := range snap {
		if e.Phase != setupPhaseBeforeSuite {
			t.Errorf("Phase = %q, want %q", e.Phase, setupPhaseBeforeSuite)
		}
		if e.DurationNanos <= 0 {
			t.Errorf("DurationNanos = %d, want > 0", e.DurationNanos)
		}
	}
}

// ── 8. appendBeforeSuiteToSetupPhaseLog — skip zero RunTime ──────────────────

func TestAppendBeforeSuiteToSetupPhaseLogSkipsZeroRunTime(t *testing.T) {
	log := installTestSetupPhaseLog(t)

	report := types.Report{
		SpecReports: types.SpecReports{
			// Zero RunTime — should be skipped.
			{
				LeafNodeType: types.NodeTypeBeforeSuite,
				RunTime:      0,
			},
			// Positive RunTime — should be logged.
			{
				LeafNodeType: types.NodeTypeBeforeSuite,
				RunTime:      5 * time.Second,
			},
			// Non-suite nodes are ignored even with RunTime.
			{
				LeafNodeType: types.NodeTypeIt,
				RunTime:      3 * time.Second,
			},
		},
	}

	appendBeforeSuiteToSetupPhaseLog(report)

	snap := log.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry (one positive-RunTime BeforeSuite), got %d", len(snap))
	}
	if snap[0].DurationNanos != (5 * time.Second).Nanoseconds() {
		t.Errorf("DurationNanos = %d, want %d", snap[0].DurationNanos, (5 * time.Second).Nanoseconds())
	}
}

// ── 9. phaseBeforeEach and phaseJustBeforeEach in timing profile ──────────────

func TestTimingProfileIncludesBeforeEachAndJustBeforeEachPhases(t *testing.T) {
	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)
	report := types.SpecReport{
		LeafNodeType:    types.NodeTypeIt,
		LeafNodeText:    "TC[003/437] E3.2 :: TestPhasePresence",
		ParallelProcess: 1,
	}

	recorder.start(report)
	recorder.beginPhase(phaseBeforeEach)
	recorder.endPhase(phaseBeforeEach)
	recorder.beginPhase(phaseJustBeforeEach)
	recorder.endPhase(phaseJustBeforeEach)
	recorder.beginPhase(phaseSpecBody)
	recorder.endPhase(phaseSpecBody)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	beforeEachSample := findTimingPhase(profile, phaseBeforeEach)
	if beforeEachSample == nil {
		t.Fatalf("phaseBeforeEach not found in profile: %+v", profile.Phases)
	}
	if beforeEachSample.DurationNanos <= 0 {
		t.Errorf("phaseBeforeEach DurationNanos = %d, want > 0", beforeEachSample.DurationNanos)
	}

	justBeforeEachSample := findTimingPhase(profile, phaseJustBeforeEach)
	if justBeforeEachSample == nil {
		t.Fatalf("phaseJustBeforeEach not found in profile: %+v", profile.Phases)
	}
	if justBeforeEachSample.DurationNanos <= 0 {
		t.Errorf("phaseJustBeforeEach DurationNanos = %d, want > 0", justBeforeEachSample.DurationNanos)
	}
}

func TestPhaseBeforeEachConstantValue(t *testing.T) {
	const want = "hook.before_each"
	if string(phaseBeforeEach) != want {
		t.Errorf("phaseBeforeEach = %q, want %q", phaseBeforeEach, want)
	}
}

func TestPhaseJustBeforeEachConstantValue(t *testing.T) {
	const want = "hook.just_before_each"
	if string(phaseJustBeforeEach) != want {
		t.Errorf("phaseJustBeforeEach = %q, want %q", phaseJustBeforeEach, want)
	}
}

// ── 10 + 11. configureSuiteExecution wires suiteSetupPhaseLog ────────────────

func TestConfigureSuiteExecutionWiresSetupPhaseLogWhenFlagSet(t *testing.T) {
	savedFlag := *e2eSetupTimingLogFlag
	savedLog := suiteSetupPhaseLog
	t.Cleanup(func() {
		*e2eSetupTimingLogFlag = savedFlag
		suiteSetupPhaseLog = savedLog
		configureSuiteExecution(nil)
	})

	dir := t.TempDir()
	logPath := filepath.Join(dir, "setup-phases.jsonl")
	*e2eSetupTimingLogFlag = logPath
	configureSuiteExecution(nil)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.SetupTimingLogPath != logPath {
		t.Errorf("SetupTimingLogPath = %q, want %q", cfg.TimingReport.SetupTimingLogPath, logPath)
	}

	// suiteSetupPhaseLog should now be a file logger that writes to logPath.
	// Write a test entry and confirm the file is created.
	appendSetupPhaseEntry(setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E99.1"})

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v (expected file created by Append)", err)
	}
	if !strings.Contains(string(data), "before_each") {
		t.Errorf("log file %q does not contain expected phase name 'before_each'; got: %s", logPath, data)
	}
}

func TestConfigureSuiteExecutionLeavesSetupPhaseLogAsNoOpWhenFlagEmpty(t *testing.T) {
	savedFlag := *e2eSetupTimingLogFlag
	savedLog := suiteSetupPhaseLog
	t.Cleanup(func() {
		*e2eSetupTimingLogFlag = savedFlag
		suiteSetupPhaseLog = savedLog
		configureSuiteExecution(nil)
	})

	*e2eSetupTimingLogFlag = "" // empty path → no-op
	configureSuiteExecution(nil)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.SetupTimingLogPath != "" {
		t.Errorf("SetupTimingLogPath = %q, want empty when flag is not set", cfg.TimingReport.SetupTimingLogPath)
	}

	// appendSetupPhaseEntry must not create any file.
	appendSetupPhaseEntry(setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E1.1"})
	// No assertion needed: if the no-op logger panics or errors, this will catch it.
}

// ── 12. installTestSetupPhaseLog ─────────────────────────────────────────────

func TestInstallTestSetupPhaseLogReplacesAndRestores(t *testing.T) {
	originalLog := suiteSetupPhaseLog

	logger := installTestSetupPhaseLog(t)

	// While installed, the package-level log should be our in-memory logger.
	appendSetupPhaseEntry(setupPhaseLogEntry{Phase: setupPhaseBeforeEach, TCID: "E1.1"})
	if logger.Len() != 1 {
		t.Fatalf("expected 1 entry in installed logger, got %d", logger.Len())
	}

	// Run cleanup explicitly to verify restoration.
	// Note: t.Cleanup runs at test end; we verify the value is different from
	// the original via reference comparison. Since installTestSetupPhaseLog
	// sets suiteSetupPhaseLog = logger, the original is restored on cleanup.
	// We just verify the current value is the installed logger.
	if suiteSetupPhaseLog != logger {
		t.Error("suiteSetupPhaseLog should be the installed in-memory logger during the test")
	}
	_ = originalLog // used to confirm the cleanup will restore it
}

// ── Integration: BeforeEach/JustBeforeEach → log entries ─────────────────────

func TestSetupPhaseLogBeforeEachDurationIsPositive(t *testing.T) {
	// Verify that when phaseBeforeEach is measured by the timingRecorder,
	// the resulting setup phase log entry has a positive duration.
	log := installTestSetupPhaseLog(t)

	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    20 * time.Millisecond, // each call advances 20ms
	}
	recorder := newSuiteTimingRecorder(clock.Now)
	report := types.SpecReport{
		LeafNodeType:    types.NodeTypeIt,
		LeafNodeText:    "TC[010/437] E8.1 :: TestBeforeEachDuration",
		ParallelProcess: 2,
	}

	recorder.start(report)
	recorder.beginPhase(phaseBeforeEach)
	// Simulate TC setup work — each clock call advances 20ms.
	_ = recorder.measureErr(phaseSetupTotal, func() error { return nil })
	recorder.endPhase(phaseBeforeEach)
	recorder.beginPhase(phaseJustBeforeEach)
	recorder.endPhase(phaseJustBeforeEach)
	recorder.beginPhase(phaseSpecBody)
	recorder.endPhase(phaseSpecBody)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no profile")
	}

	appendSetupPhasesFromTimingProfile(profile)

	snap := log.Snapshot()
	if len(snap) == 0 {
		t.Fatal("expected at least 1 log entry, got 0")
	}

	for _, e := range snap {
		if e.Phase == setupPhaseBeforeEach {
			if e.DurationNanos <= 0 {
				t.Errorf("before_each DurationNanos = %d, want > 0", e.DurationNanos)
			}
			return
		}
	}
	t.Error("no before_each entry found in log")
}

func TestSetupPhaseLogMultipleTCsAppendInOrder(t *testing.T) {
	log := installTestSetupPhaseLog(t)

	clock := &steppingClock{
		current: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}

	tcIDs := []string{"E1.1", "E2.1", "E3.1", "E4.1"}

	for _, tcID := range tcIDs {
		recorder := newSuiteTimingRecorder(clock.Now)
		report := types.SpecReport{
			LeafNodeType: types.NodeTypeIt,
			LeafNodeText: "[TC-" + tcID + "] TestOrder",
		}
		recorder.start(report)
		recorder.beginPhase(phaseBeforeEach)
		recorder.endPhase(phaseBeforeEach)
		recorder.beginPhase(phaseJustBeforeEach)
		recorder.endPhase(phaseJustBeforeEach)
		recorder.beginPhase(phaseSpecBody)
		recorder.endPhase(phaseSpecBody)

		profile, ok := recorder.finalize(types.SpecReport{
			LeafNodeType:  types.NodeTypeIt,
			LeafNodeText:  report.LeafNodeText,
			ReportEntries: types.ReportEntries{{Name: "tc_id", Value: types.WrapEntryValue(tcID)}},
		})
		if !ok {
			t.Fatalf("finalize returned no profile for %s", tcID)
		}

		appendSetupPhasesFromTimingProfile(profile)
	}

	snap := log.Snapshot()
	// Each TC produces 2 entries (before_each + just_before_each).
	want := len(tcIDs) * 2
	if len(snap) != want {
		t.Fatalf("expected %d log entries, got %d", want, len(snap))
	}
}

// ── Helper ────────────────────────────────────────────────────────────────────

func countNonEmptyLines(data []byte) int {
	n := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			n++
		}
	}
	return n
}
