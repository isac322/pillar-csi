package e2e

// AC 3: No TC can affect another TC outcome — each TC uses a unique namespace
// or unique object name set derived from its TC ID.
//
// This file provides the comprehensive uniqueness verification that covers all
// 437 documented test case IDs simultaneously.  The independence matrix in
// tc_independence_test.go covers behavioural independence (same outcome in any
// schedule); this file covers structural uniqueness (no two TC scopes share an
// identifier even when created in a tight concurrent burst).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// allCasesUniquenessResult holds the identifiers generated for a single TC
// scope so the uniqueness sweeps can compare across all 437 cases.
type allCasesUniquenessResult struct {
	TCID        string
	Namespace   string
	VolumeName  string
	BackendName string
	RootDir     string
	ScopeTag    string
	Err         error
}

// TestAC3AllDocumentedTCIDsProduceUniqueScopes creates one TestCaseScope for
// every documented TC in the default 437-case profile and asserts that no two
// scopes share a namespace, volume name, backend object name, root directory,
// or scope tag.
//
// Concurrency: all 437 scopes are created in parallel (bounded by
// GOMAXPROCS*4) so the test also validates that the atomic sequence counter
// provides uniqueness under real concurrent load.
func TestAC3AllDocumentedTCIDsProduceUniqueScopes(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC3 [catalog]: build default profile: %v", err)
	}
	if len(profile) == 0 {
		t.Fatal("AC3 [catalog]: no documented cases found — check docs/E2E-TESTCASES.md")
	}
	if len(profile) != defaultProfileCaseCount {
		t.Fatalf("AC3 [catalog]: expected %d cases, got %d", defaultProfileCaseCount, len(profile))
	}
	t.Logf("AC3 [catalog]: verifying uniqueness across %d documented TC IDs", len(profile))

	// Use a semaphore to bound peak goroutine count while still exercising
	// real parallel scope creation.
	const maxConcurrent = 64
	sem := make(chan struct{}, maxConcurrent)

	results := make([]allCasesUniquenessResult, len(profile))
	scopes := make([]*TestCaseScope, len(profile))

	var wg sync.WaitGroup
	for i, tc := range profile {
		i, tc := i, tc
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			scope, err := NewTestCaseScope(tc.DocID)
			if err != nil {
				results[i] = allCasesUniquenessResult{TCID: tc.DocID, Err: err}
				return
			}
			scopes[i] = scope

			backendObj, err := scope.BackendObject("nvme", "vol")
			if err != nil {
				results[i] = allCasesUniquenessResult{TCID: tc.DocID, Err: fmt.Errorf("BackendObject: %w", err)}
				return
			}

			results[i] = allCasesUniquenessResult{
				TCID:        tc.DocID,
				Namespace:   scope.Namespace("controller"),
				VolumeName:  scope.Name("volume", "shared"),
				BackendName: backendObj.Name,
				RootDir:     scope.RootDir,
				ScopeTag:    scope.ScopeTag,
			}
		}()
	}
	wg.Wait()

	// Cleanup all scopes regardless of test outcome.
	t.Cleanup(func() {
		for _, scope := range scopes {
			if scope != nil {
				_ = scope.Close()
			}
		}
	})

	// ── Phase 1: scope creation errors ───────────────────────────────────────
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("AC3 [%s]: create scope: %v", r.TCID, r.Err)
		}
	}
	if t.Failed() {
		return
	}

	// ── Phase 2: all paths must be under /tmp ────────────────────────────────
	tmpRoot := os.TempDir()
	for _, r := range results {
		if !filepath.IsAbs(r.RootDir) {
			t.Errorf("AC3 [%s]: root dir %q is not absolute", r.TCID, r.RootDir)
			continue
		}
		clean := filepath.Clean(r.RootDir)
		if !strings.HasPrefix(clean, filepath.Clean(tmpRoot)+string(filepath.Separator)) &&
			clean != filepath.Clean(tmpRoot) {
			t.Errorf("AC3 [%s]: root dir %q escapes %s", r.TCID, r.RootDir, tmpRoot)
		}
	}

	// ── Phase 3: global uniqueness sweeps ────────────────────────────────────
	assertAllUnique(t, results, "namespace", func(r allCasesUniquenessResult) string { return r.Namespace })
	assertAllUnique(t, results, "volume-name", func(r allCasesUniquenessResult) string { return r.VolumeName })
	assertAllUnique(t, results, "backend-name", func(r allCasesUniquenessResult) string { return r.BackendName })
	assertAllUnique(t, results, "root-dir", func(r allCasesUniquenessResult) string { return r.RootDir })
	assertAllUnique(t, results, "scope-tag", func(r allCasesUniquenessResult) string { return r.ScopeTag })

	if !t.Failed() {
		t.Logf("AC3 [catalog]: all %d documented TC IDs verified — no identifier collisions", len(results))
	}
}

// TestAC3CrossCategoryIDsDifferentNamespaces verifies that TC IDs from
// different categories (in-process, envtest, cluster, full-lvm) always produce
// different namespaces even when their numeric suffix matches.
func TestAC3CrossCategoryIDsDifferentNamespaces(t *testing.T) {
	t.Parallel()

	// Pairs of TC IDs that have the same numeric suffix to confirm the
	// category prefix (E vs F, different group numbers) is reflected.
	pairs := []struct{ a, b string }{
		{"E1.1", "E2.1"},
		{"E9.1", "E19.1"},
		{"E19.1", "E20.1"},
		{"E25.1", "E26.1"},
		{"E33.1", "E34.1"},
		{"E33.1", "E35.1"},
		{"F27.1", "F28.1"},
		{"F27.1", "E27.1"},
	}

	for _, pair := range pairs {
		pair := pair
		t.Run(pair.a+"_vs_"+pair.b, func(t *testing.T) {
			t.Parallel()

			scopeA, err := NewTestCaseScope(pair.a)
			if err != nil {
				t.Fatalf("AC3 [%s]: create scope: %v", pair.a, err)
			}
			defer func() { _ = scopeA.Close() }()

			scopeB, err := NewTestCaseScope(pair.b)
			if err != nil {
				t.Fatalf("AC3 [%s]: create scope: %v", pair.b, err)
			}
			defer func() { _ = scopeB.Close() }()

			nsA := scopeA.Namespace("controller")
			nsB := scopeB.Namespace("controller")
			if nsA == nsB {
				t.Errorf("AC3: TC %s and TC %s share namespace %q", pair.a, pair.b, nsA)
			}

			volA := scopeA.Name("volume", "shared")
			volB := scopeB.Name("volume", "shared")
			if volA == volB {
				t.Errorf("AC3: TC %s and TC %s share volume name %q", pair.a, pair.b, volA)
			}

			if scopeA.RootDir == scopeB.RootDir {
				t.Errorf("AC3: TC %s and TC %s share root dir %q", pair.a, pair.b, scopeA.RootDir)
			}
		})
	}
}

// TestAC3SameTCIDConcurrentRunsProduceDistinctScopes verifies that running the
// same TC ID concurrently — as happens when a flaky test retries — never
// produces colliding identifiers.
func TestAC3SameTCIDConcurrentRunsProduceDistinctScopes(t *testing.T) {
	t.Parallel()

	// Simulate 16 concurrent invocations of the same TC (a realistic worst
	// case for parallel test shards running the same spec).
	const concurrency = 16
	const tcID = "E1.1"

	results := make([]allCasesUniquenessResult, concurrency)
	scopes := make([]*TestCaseScope, concurrency)

	var wg sync.WaitGroup
	for i := range concurrency {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			scope, err := NewTestCaseScope(tcID)
			if err != nil {
				results[i] = allCasesUniquenessResult{TCID: tcID, Err: err}
				return
			}
			scopes[i] = scope
			results[i] = allCasesUniquenessResult{
				TCID:       tcID,
				Namespace:  scope.Namespace("controller"),
				VolumeName: scope.Name("volume", "shared"),
				RootDir:    scope.RootDir,
				ScopeTag:   scope.ScopeTag,
			}
		}()
	}
	wg.Wait()

	t.Cleanup(func() {
		for _, scope := range scopes {
			if scope != nil {
				_ = scope.Close()
			}
		}
	})

	for _, r := range results {
		if r.Err != nil {
			t.Errorf("AC3 [%s]: concurrent scope create: %v", r.TCID, r.Err)
		}
	}
	if t.Failed() {
		return
	}

	assertAllUnique(t, results, "namespace", func(r allCasesUniquenessResult) string { return r.Namespace })
	assertAllUnique(t, results, "root-dir", func(r allCasesUniquenessResult) string { return r.RootDir })
	assertAllUnique(t, results, "scope-tag", func(r allCasesUniquenessResult) string { return r.ScopeTag })

	if !t.Failed() {
		t.Logf("AC3 [%s]: %d concurrent runs all produced distinct scopes", tcID, concurrency)
	}
}

// TestAC3ScopeNamesAreDNSSafe verifies that all scope-derived identifiers
// conform to Kubernetes DNS label syntax so they are safe for use as namespace
// names, CRD object names, and PVC names.
func TestAC3ScopeNamesAreDNSSafe(t *testing.T) {
	t.Parallel()

	// Sample from each documented category group.
	sampleIDs := []string{
		"E1.1", "E2.1", "E3.1", "E4.1", "E5.1",
		"E6.1", "E7.1", "E8.1", "E9.1", "E10.1",
		"E11.1", "E12.1", "E13.1", "E14.1", "E15.1",
		"E16.1", "E17.1", "E18.1", "E19.1", "E20.1",
		"E21.1", "E22.1", "E23.1", "E24.1", "E25.1",
		"E26.1", "E28.1", "E29.1", "E30.1", "E32.1",
		"E33.1", "E34.1", "E35.1",
		"F27.1", "F28.1", "F29.1", "F30.1", "F31.1",
	}

	for _, tcID := range sampleIDs {
		tcID := tcID
		t.Run(tcID, func(t *testing.T) {
			t.Parallel()

			scope, err := NewTestCaseScope(tcID)
			if err != nil {
				t.Fatalf("AC3 [%s]: create scope: %v", tcID, err)
			}
			defer func() { _ = scope.Close() }()

			names := map[string]string{
				"scope-tag":         scope.ScopeTag,
				"namespace":         scope.Namespace("controller"),
				"volume-name":       scope.Name("volume", "shared"),
				"backend-name-zfs":  scope.Name("backend", "zfs", "pool"),
				"backend-name-nvme": scope.Name("backend", "nvme", "target"),
			}

			for field, name := range names {
				if !isDNSLabel(name) {
					t.Errorf("AC3 [%s]: %s %q is not a valid DNS label", tcID, field, name)
				}
				if len(name) > 63 {
					t.Errorf("AC3 [%s]: %s %q exceeds 63-character DNS label limit (len=%d)", tcID, field, name, len(name))
				}
			}
		})
	}
}

// TestAC3BackendObjectNamesAreUniqueCrossTCAndLabel verifies that backend
// object names for different (TC ID, kind, label) combinations are all
// globally unique.
func TestAC3BackendObjectNamesAreUniqueCrossTCAndLabel(t *testing.T) {
	t.Parallel()

	type backendFixtureKey struct {
		tcID  string
		kind  string
		label string
	}
	fixtures := []backendFixtureKey{
		{"E9.1", "zfs", "pool"},
		{"E9.1", "lvm", "pool"},
		{"E9.2", "zfs", "pool"},
		{"E28.1", "lvm", "pool"},
		{"E33.1", "nvme", "target"},
		{"E34.1", "iscsi", "target"},
		{"E35.1", "zfs", "pool"},
		{"F27.1", "lvm", "vg"},
	}

	type namedBackend struct {
		key  backendFixtureKey
		name string
		root string
	}

	scopes := make(map[string]*TestCaseScope)
	t.Cleanup(func() {
		for _, scope := range scopes {
			_ = scope.Close()
		}
	})

	named := make([]namedBackend, 0, len(fixtures))
	for _, fix := range fixtures {
		scope, ok := scopes[fix.tcID]
		if !ok {
			var err error
			scope, err = NewTestCaseScope(fix.tcID)
			if err != nil {
				t.Fatalf("AC3 [%s]: create scope: %v", fix.tcID, err)
			}
			scopes[fix.tcID] = scope
		}

		obj, err := scope.BackendObject(fix.kind, fix.label)
		if err != nil {
			t.Fatalf("AC3 [%s/%s/%s]: BackendObject: %v", fix.tcID, fix.kind, fix.label, err)
		}
		named = append(named, namedBackend{key: fix, name: obj.Name, root: obj.RootDir})
	}

	seenNames := make(map[string]backendFixtureKey)
	seenRoots := make(map[string]backendFixtureKey)
	for _, b := range named {
		if prev, exists := seenNames[b.name]; exists {
			t.Errorf("AC3: backend name %q shared by (%s/%s/%s) and (%s/%s/%s)",
				b.name,
				b.key.tcID, b.key.kind, b.key.label,
				prev.tcID, prev.kind, prev.label,
			)
		} else {
			seenNames[b.name] = b.key
		}

		if prev, exists := seenRoots[b.root]; exists {
			t.Errorf("AC3: backend root %q shared by (%s/%s/%s) and (%s/%s/%s)",
				b.root,
				b.key.tcID, b.key.kind, b.key.label,
				prev.tcID, prev.kind, prev.label,
			)
		} else {
			seenRoots[b.root] = b.key
		}
	}

	if !t.Failed() {
		t.Logf("AC3 [backend-objects]: all %d (TC, kind, label) combinations produce distinct names and roots", len(named))
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// assertAllUnique checks that the extractor function returns distinct values
// for every result.  On collision it reports the two TC IDs involved so the
// failure can be traced to the spec document.
func assertAllUnique(
	t *testing.T,
	results []allCasesUniquenessResult,
	field string,
	extractor func(allCasesUniquenessResult) string,
) {
	t.Helper()
	seen := make(map[string]string, len(results))
	for _, r := range results {
		if r.Err != nil || r.TCID == "" {
			continue
		}
		value := extractor(r)
		if value == "" {
			t.Errorf("AC3 [%s]: empty %s — scope identifier generation failed", r.TCID, field)
			continue
		}
		if prev, exists := seen[value]; exists {
			t.Errorf("AC3 [%s]: %s %q collides with TC %s", r.TCID, field, value, prev)
		} else {
			seen[value] = r.TCID
		}
	}
}

// isDNSLabel returns true when name conforms to Kubernetes DNS label rules:
// lower-case alphanumeric characters or '-', must start and end with
// alphanumeric, max 63 characters.
func isDNSLabel(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(name)-1:
		default:
			return false
		}
	}
	return true
}
