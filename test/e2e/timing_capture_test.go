package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

type steppingClock struct {
	current time.Time
	step    time.Duration
}

func (c *steppingClock) Now() time.Time {
	now := c.current
	c.current = c.current.Add(c.step)
	return now
}

func installTestTimingRecorder(t *testing.T, step time.Duration) *timingRecorder {
	t.Helper()

	clock := &steppingClock{
		current: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
		step:    step,
	}

	original := suiteTimingRecorder
	recorder := newSuiteTimingRecorder(clock.Now)
	suiteTimingRecorder = recorder
	t.Cleanup(func() {
		suiteTimingRecorder = original
	})

	return recorder
}

func findTimingPhase(profile testCaseTimingProfile, phase executionPhase) *phaseTimingSample {
	for i := range profile.Phases {
		if profile.Phases[i].Name == string(phase) {
			return &profile.Phases[i]
		}
	}
	return nil
}

func TestTimingProfileRoundTripFromReportEntry(t *testing.T) {
	clock := &steppingClock{
		current: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
		step:    5 * time.Millisecond,
	}
	recorder := newSuiteTimingRecorder(clock.Now)
	report := types.SpecReport{
		LeafNodeType:    types.NodeTypeIt,
		LeafNodeText:    "TC[437/437] F27.1 :: TestSlowPath",
		ParallelProcess: 3,
	}

	recorder.start(report)
	recorder.beginPhase(phaseSpecBody)
	recorder.endPhase(phaseSpecBody)

	profile, ok := recorder.finalize(report)
	if !ok {
		t.Fatal("finalize returned no timing profile")
	}
	if profile.TCID != "F27.1" {
		t.Fatalf("TCID = %q, want %q", profile.TCID, "F27.1")
	}
	if profile.TestName != "TestSlowPath" {
		t.Fatalf("TestName = %q, want %q", profile.TestName, "TestSlowPath")
	}
	if profile.ParallelProcess != 3 {
		t.Fatalf("ParallelProcess = %d, want %d", profile.ParallelProcess, 3)
	}

	body := findTimingPhase(profile, phaseSpecBody)
	if body == nil {
		t.Fatalf("missing %s phase in profile: %#v", phaseSpecBody, profile.Phases)
	}
	if body.Duration() <= 0 {
		t.Fatalf("%s duration = %s, want > 0", phaseSpecBody, body.Duration())
	}

	payload, err := profile.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, found, err := timingProfileFromReportEntries(types.ReportEntries{
		{
			Name:  timingReportEntryName,
			Value: types.WrapEntryValue(payload),
		},
	})
	if err != nil {
		t.Fatalf("timingProfileFromReportEntries: %v", err)
	}
	if !found {
		t.Fatal("timingProfileFromReportEntries did not find timing entry")
	}
	if decoded.TCID != profile.TCID {
		t.Fatalf("decoded TCID = %q, want %q", decoded.TCID, profile.TCID)
	}
	if decoded.TotalDuration() != profile.TotalDuration() {
		t.Fatalf("decoded total duration = %s, want %s", decoded.TotalDuration(), profile.TotalDuration())
	}
}

func TestStartTestCaseAndCloseRecordStructuredSetupAndTeardownPhases(t *testing.T) {
	recorder := installTestTimingRecorder(t, 7*time.Millisecond)
	report := types.SpecReport{
		LeafNodeType:    types.NodeTypeIt,
		LeafNodeText:    "TC[021/437] E17.1 :: TestTimingLifecycle",
		ParallelProcess: 1,
	}
	recorder.start(report)

	ctx, err := StartTestCase("E17.1", func(scope *TestCaseScope) (*TestCaseBaseline, error) {
		baseline, err := BuildTestCaseBaseline(scope, TestCaseBaselinePlan{
			TempDirs:    []string{"workspace"},
			Kubeconfigs: []string{"kind"},
			BackendObjects: []BackendFixturePlan{
				{Kind: "zfs", Label: "pool"},
			},
			LoopbackPorts: []string{"agent"},
			Seed: func(baseline *TestCaseBaseline) error {
				return os.WriteFile(
					filepath.Join(baseline.TempDir("workspace"), "seed.txt"),
					[]byte("ok"),
					0o600,
				)
			},
		})
		if err != nil {
			return nil, err
		}

		volumePath := scope.Path("volumes", "vol-1")
		if err := os.MkdirAll(filepath.Dir(volumePath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(volumePath, []byte("volume"), 0o600); err != nil {
			return nil, err
		}
		if err := scope.TrackVolume("vol-1", PathResourceSpec{Path: volumePath}); err != nil {
			return nil, err
		}

		return baseline, nil
	})
	if err != nil {
		t.Fatalf("StartTestCase: %v", err)
	}

	rootDir := ctx.Scope.RootDir
	if err := ctx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(rootDir); !os.IsNotExist(err) {
		t.Fatalf("root dir still exists or returned unexpected error: %v", err)
	}

	profile, ok := recorder.finalize(types.SpecReport{
		LeafNodeType:    types.NodeTypeIt,
		LeafNodeText:    report.LeafNodeText,
		ParallelProcess: report.ParallelProcess,
		ReportEntries: types.ReportEntries{
			{
				Name:  "tc_id",
				Value: types.WrapEntryValue("E17.1"),
			},
			{
				Name:  "tc_test_name",
				Value: types.WrapEntryValue("TestTimingLifecycle"),
			},
		},
	})
	if !ok {
		t.Fatal("finalize returned no timing profile")
	}

	for _, phase := range []executionPhase{
		phaseSetupTotal,
		phaseSetupScope,
		phaseSetupCallback,
		phaseSetupBaselineTotal,
		phaseSetupTempDirs,
		phaseSetupKubeconfigs,
		phaseSetupBackendObjects,
		phaseSetupLoopbackPorts,
		phaseSetupSeed,
		phaseTeardownTotal,
		phaseTeardownResources,
		phaseTeardownPortLeases,
		phaseTeardownRootDir,
	} {
		sample := findTimingPhase(profile, phase)
		if sample == nil {
			t.Fatalf("missing %s phase in profile: %#v", phase, profile.Phases)
		}
		if sample.Duration() <= 0 {
			t.Fatalf("%s duration = %s, want > 0", phase, sample.Duration())
		}
	}
}
