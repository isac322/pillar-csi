package e2e

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	independenceShuffleSeed = int64(0x34ac)
	independenceHoldTime    = 20 * time.Millisecond
)

type independenceCase struct {
	TCID string
}

type caseOutcome struct {
	TCID             string
	Passed           bool
	Failure          string
	ResourceClaims   int
	TrackedResources int
	SyntheticPort    bool
}

type scheduleOutcome struct {
	Name           string
	Order          []string
	Results        map[string]caseOutcome
	WorkerCount    int
	MaxActiveCases int
}

type activeCaseRegistry struct {
	mu             sync.Mutex
	owners         map[string]string
	activeCases    int
	maxActiveCases int
}

func TestAC34ExecutionIndependenceAcrossSchedules(t *testing.T) {
	t.Parallel()
	cases := independenceCaseMatrix()
	canonicalOrder := caseOrder(cases)
	parallelWorkers := defaultParallelWorkerCount()

	var alone scheduleOutcome
	t.Run("alone", func(t *testing.T) {
		alone = scheduleOutcome{
			Name:        "alone",
			Order:       canonicalOrder,
			Results:     make(map[string]caseOutcome, len(cases)),
			WorkerCount: 1,
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.TCID, func(t *testing.T) {
				registry := newActiveCaseRegistry()
				outcome := executeIndependenceCase(tc, registry, 0)
				alone.Results[tc.TCID] = outcome
				assertCasePassed(t, outcome)
				if got := registry.MaxActiveCases(); got != 1 {
					t.Fatalf("%s alone max active cases = %d, want 1", tc.TCID, got)
				}
			})
		}
	})

	var shuffled scheduleOutcome
	t.Run("shuffled", func(t *testing.T) {
		order := shuffledCaseMatrix(cases, independenceShuffleSeed)
		shuffled = runSerialSchedule("shuffled", order, independenceHoldTime)

		if slices.Equal(canonicalOrder, shuffled.Order) {
			t.Fatalf("shuffled order unexpectedly matched canonical order: %v", shuffled.Order)
		}
		assertAllCasesPassed(t, shuffled)
		if shuffled.MaxActiveCases != 1 {
			t.Fatalf("shuffled max active cases = %d, want 1", shuffled.MaxActiveCases)
		}
	})

	var parallel scheduleOutcome
	t.Run("parallel", func(t *testing.T) {
		parallel = runParallelSchedule(cases, parallelWorkers, independenceHoldTime)

		assertAllCasesPassed(t, parallel)
		if parallel.WorkerCount != parallelWorkers {
			t.Fatalf("parallel worker count = %d, want %d", parallel.WorkerCount, parallelWorkers)
		}
		if parallelWorkers > 1 && len(cases) > 1 && parallel.MaxActiveCases < 2 {
			t.Fatalf("parallel max active cases = %d, want at least 2", parallel.MaxActiveCases)
		}
	})

	t.Run("compare", func(t *testing.T) {
		assertEquivalentSchedules(t, alone, shuffled)
		assertEquivalentSchedules(t, alone, parallel)
	})
}

func independenceCaseMatrix() []independenceCase {
	return []independenceCase{
		{TCID: "E3.1"},
		{TCID: "E9.1"},
		{TCID: "E19.1"},
		{TCID: "E25.1"},
		{TCID: "E33.1"},
		{TCID: "E34.1"},
		{TCID: "E35.1"},
		{TCID: "F27.1"},
	}
}

func runSerialSchedule(name string, cases []independenceCase, hold time.Duration) scheduleOutcome {
	registry := newActiveCaseRegistry()
	results := make(map[string]caseOutcome, len(cases))
	order := make([]string, 0, len(cases))
	for _, tc := range cases {
		outcome := executeIndependenceCase(tc, registry, hold)
		results[tc.TCID] = outcome
		order = append(order, tc.TCID)
	}

	return scheduleOutcome{
		Name:           name,
		Order:          order,
		Results:        results,
		WorkerCount:    1,
		MaxActiveCases: registry.MaxActiveCases(),
	}
}

func runParallelSchedule(cases []independenceCase, workers int, hold time.Duration) scheduleOutcome {
	if workers < 1 {
		workers = 1
	}

	registry := newActiveCaseRegistry()
	jobs := make(chan independenceCase)
	results := make(chan caseOutcome, len(cases))

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tc := range jobs {
				results <- executeIndependenceCase(tc, registry, hold)
			}
		}()
	}

	go func() {
		for _, tc := range cases {
			jobs <- tc
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	outcome := scheduleOutcome{
		Name:        "parallel",
		Order:       caseOrder(cases),
		Results:     make(map[string]caseOutcome, len(cases)),
		WorkerCount: workers,
	}
	for result := range results {
		outcome.Results[result.TCID] = result
	}
	outcome.MaxActiveCases = registry.MaxActiveCases()
	return outcome
}

func executeIndependenceCase(tc independenceCase, registry *activeCaseRegistry, hold time.Duration) caseOutcome {
	result := caseOutcome{
		TCID:             tc.TCID,
		ResourceClaims:   7,
		TrackedResources: 4,
	}

	ctx, err := StartTestCase(tc.TCID, func(scope *TestCaseScope) (*TestCaseBaseline, error) {
		return BuildTestCaseBaseline(scope, independenceBaselinePlan(tc))
	})
	if err != nil {
		return failedCaseOutcome(tc.TCID, err, result)
	}

	rootDir := ctx.Scope.RootDir
	defer func() {
		if rootDir != "" {
			_ = os.RemoveAll(rootDir)
		}
	}()

	if err := verifyIndependenceBaseline(tc, ctx.Baseline); err != nil {
		_ = ctx.Close()
		return failedCaseOutcome(tc.TCID, err, result)
	}

	if err := prepareTrackedFixtures(ctx); err != nil {
		_ = ctx.Close()
		return failedCaseOutcome(tc.TCID, err, result)
	}

	port := ctx.Baseline.Port("agent")
	if port == nil {
		_ = ctx.Close()
		return failedCaseOutcome(tc.TCID, fmt.Errorf("%s: missing loopback port lease", tc.TCID), result)
	}
	result.SyntheticPort = port.Synthetic

	release, err := registry.Claim(tc.TCID, []string{
		"root:" + ctx.Scope.RootDir,
		"namespace:" + ctx.Scope.Namespace("controller"),
		"kubeconfig:" + ctx.Baseline.Kubeconfig("kind"),
		"backend-name:" + ctx.Baseline.BackendObject("zfs", "pool").Name,
		"backend-root:" + ctx.Baseline.BackendObject("zfs", "pool").RootDir,
		"port:" + port.Addr,
		"volume-name:" + ctx.Scope.Name("volume", "shared"),
	})
	if err != nil {
		_ = ctx.Close()
		return failedCaseOutcome(tc.TCID, err, result)
	}

	time.Sleep(hold)
	release()

	if err := ctx.Close(); err != nil {
		return failedCaseOutcome(tc.TCID, err, result)
	}
	rootDir = ""

	if _, err := os.Stat(ctx.Scope.RootDir); !os.IsNotExist(err) {
		if err == nil {
			err = fmt.Errorf("scope root still exists after close")
		}
		return failedCaseOutcome(tc.TCID, err, result)
	}

	result.Passed = true
	return result
}

func independenceBaselinePlan(tc independenceCase) TestCaseBaselinePlan {
	return TestCaseBaselinePlan{
		TempDirs:    []string{"workspace"},
		Kubeconfigs: []string{"kind"},
		BackendObjects: []BackendFixturePlan{
			{Kind: "zfs", Label: "pool"},
		},
		LoopbackPorts: []string{"agent"},
		Seed: func(baseline *TestCaseBaseline) error {
			workspace := baseline.TempDir("workspace")
			if err := os.WriteFile(
				filepath.Join(workspace, "tc-id.txt"),
				[]byte(tc.TCID),
				0o600,
			); err != nil {
				return err
			}
			if err := os.WriteFile(
				filepath.Join(workspace, "fixture.txt"),
				[]byte("workspace:"+tc.TCID),
				0o600,
			); err != nil {
				return err
			}
			if err := os.WriteFile(
				baseline.Kubeconfig("kind"),
				[]byte("apiVersion: v1\nkind: Config\ncurrent-context: "+tc.TCID+"\n"),
				0o600,
			); err != nil {
				return err
			}
			return os.WriteFile(
				baseline.BackendObject("zfs", "pool").Path("status"),
				[]byte("backend:"+tc.TCID),
				0o600,
			)
		},
	}
}

func verifyIndependenceBaseline(tc independenceCase, baseline *TestCaseBaseline) error {
	if baseline == nil {
		return fmt.Errorf("%s: baseline is nil", tc.TCID)
	}

	workspace := baseline.TempDir("workspace")
	if !strings.HasPrefix(workspace, baseline.Scope.RootDir) {
		return fmt.Errorf("%s: workspace %q escaped scope root %q", tc.TCID, workspace, baseline.Scope.RootDir)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "tc-id.txt"))
	if err != nil {
		return fmt.Errorf("%s: read workspace marker: %w", tc.TCID, err)
	}
	if got := strings.TrimSpace(string(content)); got != tc.TCID {
		return fmt.Errorf("%s: workspace marker = %q, want %q", tc.TCID, got, tc.TCID)
	}

	content, err = os.ReadFile(baseline.BackendObject("zfs", "pool").Path("status"))
	if err != nil {
		return fmt.Errorf("%s: read backend marker: %w", tc.TCID, err)
	}
	if got := strings.TrimSpace(string(content)); got != "backend:"+tc.TCID {
		return fmt.Errorf("%s: backend marker = %q, want %q", tc.TCID, got, "backend:"+tc.TCID)
	}

	content, err = os.ReadFile(baseline.Kubeconfig("kind"))
	if err != nil {
		return fmt.Errorf("%s: read kubeconfig: %w", tc.TCID, err)
	}
	if !strings.Contains(string(content), "current-context: "+tc.TCID) {
		return fmt.Errorf("%s: kubeconfig missing TC-specific context", tc.TCID)
	}

	return nil
}

func prepareTrackedFixtures(ctx *TestCaseContext) error {
	scope := ctx.Scope
	mountPath := scope.Path("mounts", "published")
	volumePath := scope.Path("volumes", "shared")
	snapshotPath := scope.Path("snapshots", "shared")
	recordPath := scope.Path("backend-records", "binding.json")

	for _, dir := range []string{
		filepath.Dir(mountPath),
		filepath.Dir(volumePath),
		snapshotPath,
		filepath.Dir(recordPath),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if err := os.WriteFile(mountPath, []byte("mount"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(volumePath, []byte("volume"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(snapshotPath, "meta.json"), []byte("{}"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(recordPath, []byte("{}"), 0o600); err != nil {
		return err
	}

	if err := scope.TrackMount("publish-target", MountResourceSpec{
		TargetPath: mountPath,
		Cleanup:    defaultPathCleanup(mountPath),
		IsPresent:  defaultPathPresenceProbe(mountPath),
	}); err != nil {
		return err
	}
	if err := scope.TrackVolume("volume-shared", PathResourceSpec{Path: volumePath}); err != nil {
		return err
	}
	if err := scope.TrackSnapshot("snapshot-shared", PathResourceSpec{Path: snapshotPath}); err != nil {
		return err
	}
	return scope.TrackBackendRecord("binding-record", PathResourceSpec{Path: recordPath})
}

func shuffledCaseMatrix(cases []independenceCase, seed int64) []independenceCase {
	shuffled := append([]independenceCase(nil), cases...)
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	if slices.Equal(caseOrder(cases), caseOrder(shuffled)) && len(shuffled) > 1 {
		shuffled = append(shuffled[1:], shuffled[0])
	}
	return shuffled
}

func caseOrder(cases []independenceCase) []string {
	order := make([]string, 0, len(cases))
	for _, tc := range cases {
		order = append(order, tc.TCID)
	}
	return order
}

func assertAllCasesPassed(t *testing.T, outcome scheduleOutcome) {
	t.Helper()

	ids := make([]string, 0, len(outcome.Results))
	for tcID := range outcome.Results {
		ids = append(ids, tcID)
	}
	slices.Sort(ids)

	for _, tcID := range ids {
		assertCasePassed(t, outcome.Results[tcID])
	}
}

func assertCasePassed(t *testing.T, outcome caseOutcome) {
	t.Helper()
	if !outcome.Passed {
		t.Fatalf("%s failed: %s", outcome.TCID, outcome.Failure)
	}
}

func assertEquivalentSchedules(t *testing.T, want, got scheduleOutcome) {
	t.Helper()

	if len(want.Results) != len(got.Results) {
		t.Fatalf("%s case count = %d, want %d from %s", got.Name, len(got.Results), len(want.Results), want.Name)
	}

	ids := make([]string, 0, len(want.Results))
	for tcID := range want.Results {
		ids = append(ids, tcID)
	}
	slices.Sort(ids)

	for _, tcID := range ids {
		left := want.Results[tcID]
		right, ok := got.Results[tcID]
		if !ok {
			t.Fatalf("%s missing case %s", got.Name, tcID)
		}

		if left.Passed != right.Passed {
			t.Fatalf("%s pass/fail for %s = %t, want %t from %s", got.Name, tcID, right.Passed, left.Passed, want.Name)
		}
		if left.ResourceClaims != right.ResourceClaims {
			t.Fatalf("%s resource claim count for %s = %d, want %d from %s", got.Name, tcID, right.ResourceClaims, left.ResourceClaims, want.Name)
		}
		if left.TrackedResources != right.TrackedResources {
			t.Fatalf("%s tracked resource count for %s = %d, want %d from %s", got.Name, tcID, right.TrackedResources, left.TrackedResources, want.Name)
		}
		if left.SyntheticPort != right.SyntheticPort {
			t.Fatalf("%s synthetic-port flag for %s = %t, want %t from %s", got.Name, tcID, right.SyntheticPort, left.SyntheticPort, want.Name)
		}
	}
}

func failedCaseOutcome(tcID string, err error, result caseOutcome) caseOutcome {
	result.Passed = false
	if err != nil {
		result.Failure = err.Error()
	}
	return result
}

func newActiveCaseRegistry() *activeCaseRegistry {
	return &activeCaseRegistry{
		owners: make(map[string]string),
	}
}

func (r *activeCaseRegistry) Claim(tcID string, claims []string) (func(), error) {
	if r == nil {
		return func() {}, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, claim := range claims {
		if owner, exists := r.owners[claim]; exists {
			return nil, fmt.Errorf("%s attempted to reuse %s already claimed by %s", tcID, claim, owner)
		}
	}

	for _, claim := range claims {
		r.owners[claim] = tcID
	}
	r.activeCases++
	if r.activeCases > r.maxActiveCases {
		r.maxActiveCases = r.activeCases
	}

	released := false
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if released {
			return
		}
		released = true
		for _, claim := range claims {
			delete(r.owners, claim)
		}
		r.activeCases--
	}, nil
}

func (r *activeCaseRegistry) MaxActiveCases() int {
	if r == nil {
		return 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxActiveCases
}

func defaultParallelWorkerCount() int {
	testParallel := flag.Lookup("test.parallel")
	if testParallel != nil {
		if value, err := strconv.Atoi(testParallel.Value.String()); err == nil && value > 0 {
			return value
		}
	}

	if value := runtime.GOMAXPROCS(0); value > 0 {
		return value
	}
	return 1
}
