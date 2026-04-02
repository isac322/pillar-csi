package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
)

// BackendFixturePlan declares one backend fixture that must be reset for a TC.
type BackendFixturePlan struct {
	Kind  string
	Label string
}

// TestCaseBaselinePlan declares the baseline resources that must be recreated
// before a TC executes.
type TestCaseBaselinePlan struct {
	TempDirs       []string
	Kubeconfigs    []string
	BackendObjects []BackendFixturePlan
	LoopbackPorts  []string
	Seed           func(*TestCaseBaseline) error
}

// TestCaseBaseline holds the recreated baseline resources for a single TC.
type TestCaseBaseline struct {
	Scope          *TestCaseScope
	TempDirs       map[string]string
	Kubeconfigs    map[string]string
	BackendObjects map[string]*BackendObject
	PortLeases     map[string]*PortLease
}

// TempDir returns the recreated temp directory for the logical label.
func (b *TestCaseBaseline) TempDir(label string) string {
	if b == nil {
		return ""
	}
	return b.TempDirs[pathToken(label)]
}

// Kubeconfig returns the recreated kubeconfig path for the logical label.
func (b *TestCaseBaseline) Kubeconfig(label string) string {
	if b == nil {
		return ""
	}
	return b.Kubeconfigs[pathToken(label)]
}

// BackendObject returns the recreated backend fixture for the kind/label pair.
func (b *TestCaseBaseline) BackendObject(kind, label string) *BackendObject {
	if b == nil {
		return nil
	}
	return b.BackendObjects[baselineObjectKey(kind, label)]
}

// Port returns the recreated loopback lease for the logical label.
func (b *TestCaseBaseline) Port(label string) *PortLease {
	if b == nil {
		return nil
	}
	return b.PortLeases[pathToken(label)]
}

// BuildTestCaseBaseline recreates the declared baseline state for one TC.
func BuildTestCaseBaseline(scope *TestCaseScope, plan TestCaseBaselinePlan) (*TestCaseBaseline, error) {
	if scope == nil {
		return nil, fmt.Errorf("build baseline: scope is required")
	}

	baseline := newEmptyBaseline(scope)

	if err := measureTimingPhaseErr(phaseSetupBaselineTotal, func() error {
		if len(plan.TempDirs) > 0 {
			if err := measureTimingPhaseErr(phaseSetupTempDirs, func() error {
				for _, label := range uniqueLabels(plan.TempDirs) {
					dir, err := scope.RecreateTempDir(label)
					if err != nil {
						return err
					}
					baseline.TempDirs[pathToken(label)] = dir
				}
				return nil
			}); err != nil {
				return err
			}
		}

		if len(plan.Kubeconfigs) > 0 {
			if err := measureTimingPhaseErr(phaseSetupKubeconfigs, func() error {
				for _, label := range uniqueLabels(plan.Kubeconfigs) {
					path, err := scope.RecreateKubeconfigPath(label)
					if err != nil {
						return err
					}
					baseline.Kubeconfigs[pathToken(label)] = path
				}
				return nil
			}); err != nil {
				return err
			}
		}

		if len(plan.BackendObjects) > 0 {
			if err := measureTimingPhaseErr(phaseSetupBackendObjects, func() error {
				for _, fixture := range uniqueBackendFixtures(plan.BackendObjects) {
					obj, err := scope.RecreateBackendObject(fixture.Kind, fixture.Label)
					if err != nil {
						return err
					}
					baseline.BackendObjects[baselineObjectKey(fixture.Kind, fixture.Label)] = obj
				}
				return nil
			}); err != nil {
				return err
			}
		}

		if len(plan.LoopbackPorts) > 0 {
			if err := measureTimingPhaseErr(phaseSetupLoopbackPorts, func() error {
				for _, label := range uniqueLabels(plan.LoopbackPorts) {
					lease, err := scope.RecreateLoopbackPort(label)
					if err != nil {
						return err
					}
					baseline.PortLeases[pathToken(label)] = lease
				}
				return nil
			}); err != nil {
				return err
			}
		}

		if plan.Seed != nil {
			if err := measureTimingPhaseErr(phaseSetupSeed, func() error {
				if err := plan.Seed(baseline); err != nil {
					return fmt.Errorf("seed baseline for %s: %w", scope.TCID, err)
				}
				return nil
			}); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return baseline, nil
}

// TestCaseSetupFunc prepares the baseline for a single documented TC.
type TestCaseSetupFunc func(*TestCaseScope) (*TestCaseBaseline, error)

// TestCaseContext is the live per-TC execution context exposed to the spec.
type TestCaseContext struct {
	Scope    *TestCaseScope
	Baseline *TestCaseBaseline
}

// Close releases the TC scope and its resources.
func (c *TestCaseContext) Close() error {
	if c == nil || c.Scope == nil {
		return nil
	}
	return c.Scope.Close()
}

// CloseBackground starts TC scope cleanup in a background goroutine, allowing
// the next test case to start before this TC's cleanup finishes. It delegates
// to c.Scope.CloseBackground() which registers the cleanup result with
// suiteAsyncCleanup so that DrainPendingCleanups can collect it at suite
// teardown time (Sub-AC 5.3).
//
// CloseBackground is a no-op when c or c.Scope is nil.
func (c *TestCaseContext) CloseBackground() {
	if c == nil || c.Scope == nil {
		return
	}
	c.Scope.CloseBackground()
}

// StartTestCase creates a fresh TC scope and rebuilds its baseline state.
func StartTestCase(tcID string, setup TestCaseSetupFunc) (*TestCaseContext, error) {
	if setup == nil {
		setup = func(scope *TestCaseScope) (*TestCaseBaseline, error) {
			return newEmptyBaseline(scope), nil
		}
	}

	var (
		scope    *TestCaseScope
		baseline *TestCaseBaseline
	)

	if err := measureTimingPhaseErr(phaseSetupTotal, func() error {
		var err error
		scope, err = measureTimingPhaseValue(phaseSetupScope, func() (*TestCaseScope, error) {
			return NewTestCaseScope(tcID)
		})
		if err != nil {
			return err
		}

		baseline, err = measureTimingPhaseValue(phaseSetupCallback, func() (*TestCaseBaseline, error) {
			return setup(scope)
		})
		if err != nil {
			_ = scope.Close()
			return fmt.Errorf("setup %s: %w", tcID, err)
		}

		return nil
	}); err != nil {
		return nil, err
	}
	if baseline == nil {
		baseline = newEmptyBaseline(scope)
	}
	if baseline.Scope == nil {
		baseline.Scope = scope
	}

	return &TestCaseContext{
		Scope:    scope,
		Baseline: baseline,
	}, nil
}

// BoundTestCase binds a TC-specific BeforeEach hook to the surrounding spec.
type BoundTestCase struct {
	current *TestCaseContext
}

// UsePerTestCaseSetup registers a BeforeEach hook that rebuilds the baseline
// for the current TC before each spec executes.
func UsePerTestCaseSetup(tcID string, setup TestCaseSetupFunc) *BoundTestCase {
	binding := &BoundTestCase{}

	BeforeEach(func() {
		if _, ok := reportEntryValue(CurrentSpecReport().ReportEntries, "tc_id"); !ok {
			AddReportEntry("tc_id", tcID, types.ReportEntryVisibilityNever)
		}

		ctx, err := StartTestCase(tcID, setup)
		Expect(err).NotTo(HaveOccurred())

		binding.current = ctx
		DeferCleanup(func() {
			binding.current = nil
			// Sub-AC 5.3: fire cleanup in a background goroutine so the next
			// TC can start immediately without waiting for this TC's teardown.
			// Cleanup errors are collected by DrainPendingCleanups in AfterSuite.
			// Tests that inspect cleaned-up state in subsequent It blocks within
			// an Ordered container should call DrainPendingCleanups first.
			ctx.CloseBackground()
		})
	})

	return binding
}

// Context returns the current per-TC context for the running spec.
func (b *BoundTestCase) Context() *TestCaseContext {
	if b == nil {
		return nil
	}
	return b.current
}

// Scope returns the current TC scope for the running spec.
func (b *BoundTestCase) Scope() *TestCaseScope {
	if b == nil || b.current == nil {
		return nil
	}
	return b.current.Scope
}

// Baseline returns the current TC baseline for the running spec.
func (b *BoundTestCase) Baseline() *TestCaseBaseline {
	if b == nil || b.current == nil {
		return nil
	}
	return b.current.Baseline
}

func newEmptyBaseline(scope *TestCaseScope) *TestCaseBaseline {
	return &TestCaseBaseline{
		Scope:          scope,
		TempDirs:       make(map[string]string),
		Kubeconfigs:    make(map[string]string),
		BackendObjects: make(map[string]*BackendObject),
		PortLeases:     make(map[string]*PortLease),
	}
}

func baselineObjectKey(kind, label string) string {
	return pathToken(kind) + ":" + pathToken(label)
}

func uniqueLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	unique := make([]string, 0, len(labels))
	for _, label := range labels {
		key := pathToken(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, label)
	}
	return unique
}

func uniqueBackendFixtures(fixtures []BackendFixturePlan) []BackendFixturePlan {
	seen := make(map[string]struct{}, len(fixtures))
	unique := make([]BackendFixturePlan, 0, len(fixtures))
	for _, fixture := range fixtures {
		key := baselineObjectKey(fixture.Kind, fixture.Label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, fixture)
	}
	return unique
}
