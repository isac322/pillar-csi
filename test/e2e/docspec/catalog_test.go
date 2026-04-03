package docspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCatalog(t *testing.T) {
	t.Parallel()

	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	if catalog.DeclaredTotal != 404 {
		t.Fatalf("declared total = %d, want 404", catalog.DeclaredTotal)
	}
	if len(catalog.Cases) == 0 {
		t.Fatal("expected at least one concrete case row")
	}
	if len(catalog.CanonicalCases) == 0 {
		t.Fatal("expected at least one canonical case")
	}
	if len(catalog.CanonicalCases) > len(catalog.Cases) {
		t.Fatalf("canonical case count %d exceeds concrete row count %d", len(catalog.CanonicalCases), len(catalog.Cases))
	}
}

func TestBuildTraceabilityReport(t *testing.T) {
	t.Parallel()

	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	report, err := BuildTraceabilityReport(repoRoot, catalog)
	if err != nil {
		t.Fatalf("build traceability report: %v", err)
	}

	if got, want := report.BoundCount()+report.MissingCount(), len(catalog.CanonicalCases); got != want {
		t.Fatalf("bound+missing = %d, want %d", got, want)
	}

	t.Logf(
		"declared_total=%d concrete_rows=%d canonical_cases=%d duplicate_symbols=%d bound=%d missing=%d",
		catalog.DeclaredTotal,
		len(catalog.Cases),
		len(catalog.CanonicalCases),
		len(catalog.DuplicateSymbols),
		report.BoundCount(),
		report.MissingCount(),
	)
}

// ── GinkgoNodeID ──────────────────────────────────────────────────────────────

func TestGinkgoNodeID_NamedID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		id         string
		sectionKey string
		want       string
	}{
		{"E1.1", "E1", "E1.1"},
		{"F27.3", "F27", "F27.3"},
		{"E33.285", "E33", "E33.285"},
		{"E19.3.1", "E19", "E19.3.1"},
		{"E1.10-1", "E1", "E1.10-1"},
	} {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			c := Case{ID: tc.id, SectionKey: tc.sectionKey}
			if got := c.GinkgoNodeID(); got != tc.want {
				t.Errorf("GinkgoNodeID() = %q, want %q", got, tc.want)
			}
			label := c.GinkgoNodeLabel()
			if !strings.HasPrefix(label, "[TC-") || !strings.HasSuffix(label, "]") {
				t.Errorf("GinkgoNodeLabel() = %q does not have [TC-...] form", label)
			}
		})
	}
}

func TestGinkgoNodeID_NumericIDPrefixed(t *testing.T) {
	t.Parallel()

	c := Case{ID: "285", SectionKey: "E33"}
	if got := c.GinkgoNodeID(); got != "E33.285" {
		t.Errorf("GinkgoNodeID() = %q, want E33.285", got)
	}
	if got := c.GinkgoNodeLabel(); got != "[TC-E33.285]" {
		t.Errorf("GinkgoNodeLabel() = %q, want [TC-E33.285]", got)
	}
}

func TestGinkgoNodeID_NumericIDNoSectionKey(t *testing.T) {
	t.Parallel()

	c := Case{ID: "285", SectionKey: ""}
	// Without a section key the raw ID is returned unchanged.
	if got := c.GinkgoNodeID(); got != "285" {
		t.Errorf("GinkgoNodeID() = %q, want 285", got)
	}
}

// ── FindGinkgoNodeBindingsFromSpecNames ───────────────────────────────────────

func TestFindGinkgoNodeBindingsFromSpecNames_ExactMatch(t *testing.T) {
	t.Parallel()

	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	// Build a synthetic set of spec names that covers every canonical case.
	specNames := make([]string, 0, len(catalog.CanonicalCases))
	for _, tc := range catalog.CanonicalCases {
		nodeID := tc.GinkgoNodeID()
		if nodeID == "" {
			continue
		}
		specNames = append(specNames, "[TC-"+nodeID+"] TC[001/388] "+nodeID+" :: "+tc.Symbol)
	}

	report := FindGinkgoNodeBindingsFromSpecNames(catalog, specNames)

	if report.MissingCount() != 0 {
		t.Errorf("expected 0 missing, got %d", report.MissingCount())
	}
	if report.ExtraCount() != 0 {
		t.Errorf("expected 0 extra, got %d", report.ExtraCount())
	}
	if report.DuplicateCount() != 0 {
		t.Errorf("expected 0 duplicates, got %d", report.DuplicateCount())
	}
	if report.BoundCount() == 0 {
		t.Error("expected bound > 0")
	}
	t.Logf("full-coverage synthetic check: bound=%d missing=%d extra=%d duplicates=%d",
		report.BoundCount(), report.MissingCount(), report.ExtraCount(), report.DuplicateCount())
}

func TestFindGinkgoNodeBindingsFromSpecNames_Extra(t *testing.T) {
	t.Parallel()

	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	// Introduce a spec name that references a TC ID not in the catalogue.
	specNames := []string{
		"[TC-E99.999] TC[999/388] E99.999 :: TestNonExistent",
	}

	report := FindGinkgoNodeBindingsFromSpecNames(catalog, specNames)
	if report.ExtraCount() != 1 {
		t.Errorf("expected 1 extra, got %d", report.ExtraCount())
	}
	if report.Extra[0].CaseID != "E99.999" {
		t.Errorf("extra CaseID = %q, want E99.999", report.Extra[0].CaseID)
	}
}

func TestFindGinkgoNodeBindingsFromSpecNames_Duplicate(t *testing.T) {
	t.Parallel()

	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	// Use the first canonical case's ID twice.
	var firstID string
	for _, tc := range catalog.CanonicalCases {
		if id := tc.GinkgoNodeID(); id != "" {
			firstID = id
			break
		}
	}
	if firstID == "" {
		t.Fatalf("no canonical cases with a GinkgoNodeID — catalog must contain at least one case with a Ginkgo node ID")
	}

	specNames := []string{
		"[TC-" + firstID + "] first occurrence",
		"[TC-" + firstID + "] second occurrence",
	}

	report := FindGinkgoNodeBindingsFromSpecNames(catalog, specNames)
	if report.DuplicateCount() != 1 {
		t.Errorf("expected 1 duplicate ID, got %d", report.DuplicateCount())
	}
	if _, ok := report.Duplicates[firstID]; !ok {
		t.Errorf("expected %q in Duplicates map", firstID)
	}
}

// ── FindGinkgoNodeBindings (static) ──────────────────────────────────────────

func TestFindGinkgoNodeBindings_NoFalsePositives(t *testing.T) {
	t.Parallel()

	// The static scanner finds It() calls with [TC-<ID>] string literals in the
	// real-backend spec files (e.g. lvm_backend_standalone_e2e_test.go).
	// Most catalog cases are dynamically generated via tcNodeName() and will be
	// "missing" from the static scan — that is expected.
	// The key invariant is that zero "extra" bindings exist (no false positives:
	// every statically-bound TC ID must be in the catalogue).
	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	report, err := FindGinkgoNodeBindings(repoRoot, catalog)
	if err != nil {
		t.Fatalf("FindGinkgoNodeBindings: %v", err)
	}

	if report.ExtraCount() != 0 {
		t.Errorf("expected 0 extra (no false positives), got %d", report.ExtraCount())
		for _, b := range report.Extra {
			t.Logf("  extra: [TC-%s] at %s:%d — %s", b.CaseID, b.Path, b.Line, b.Evidence)
		}
	}
	if report.DuplicateCount() != 0 {
		t.Errorf("expected 0 duplicates, got %d", report.DuplicateCount())
	}

	t.Logf("static scan: bound=%d missing=%d extra=%d duplicates=%d",
		report.BoundCount(), report.MissingCount(), report.ExtraCount(), report.DuplicateCount())
}

// TestE33StandaloneSpecsInDefaultProfile verifies that all 7 E33.4 standalone
// specs (TC-E33.311 through TC-E33.317) are:
//
//  1. Present in the E2E-TESTCASES.md catalogue with their expected GinkgoNodeIDs.
//  2. Statically bound to Ginkgo It-node string literals in
//     test/e2e/lvm_backend_standalone_e2e_test.go.
//  3. The outer Describe in that file carries the "default-profile" label so
//     Ginkgo includes them under the defaultLabelFilter.
//
// This is the AC 3 invariant guard: failing here means the spec document and
// the implementation have diverged for E33 standalone cases.
func TestE33StandaloneSpecsInDefaultProfile(t *testing.T) {
	t.Parallel()

	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("AC3 [E33-standalone]: find repo root: %v", err)
	}

	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("AC3 [E33-standalone]: load catalog: %v", err)
	}

	// ── 1: catalogue contains E33.311–E33.317 ────────────────────────────────
	// These are the 7 documented cases in the E33.4 subsection.
	wantIDs := []string{
		"E33.311", "E33.312", "E33.313", "E33.314",
		"E33.315", "E33.316", "E33.317",
	}

	catalogIDSet := make(map[string]bool, len(catalog.CanonicalCases))
	for _, c := range catalog.CanonicalCases {
		catalogIDSet[c.GinkgoNodeID()] = true
	}

	for _, id := range wantIDs {
		if !catalogIDSet[id] {
			t.Errorf("AC3 [E33-standalone]: %q not found in catalogue — "+
				"docs/E2E-TESTCASES.md E33.4 section may be out of sync", id)
		}
	}
	if t.Failed() {
		return
	}

	// ── 2: static Ginkgo node binding in lvm_backend_standalone_e2e_test.go ─
	report, err := FindGinkgoNodeBindings(repoRoot, catalog)
	if err != nil {
		t.Fatalf("AC3 [E33-standalone]: FindGinkgoNodeBindings: %v", err)
	}

	// FindGinkgoNodeBindings scans test/e2e/ and returns paths relative to
	// that directory, so the standalone file appears as just its basename.
	const standaloneFile = "test/e2e/lvm_backend_standalone_e2e_test.go"
	const standaloneBasename = "lvm_backend_standalone_e2e_test.go"
	for _, id := range wantIDs {
		bindings, found := report.Matches[id]
		if !found || len(bindings) == 0 {
			t.Errorf("AC3 [E33-standalone]: [TC-%s] has no static Ginkgo node binding — "+
				"add It(\"[TC-%s] ...\") in %s", id, id, standaloneFile)
			continue
		}
		// Verify at least one binding is in the standalone file.
		// The Path field is relative to test/e2e/, so only the basename is used.
		var inFile bool
		for _, b := range bindings {
			normalised := strings.ReplaceAll(b.Path, "\\", "/")
			if normalised == standaloneBasename ||
				strings.HasSuffix(normalised, "/"+standaloneBasename) {
				inFile = true
				break
			}
		}
		if !inFile {
			t.Errorf("AC3 [E33-standalone]: [TC-%s] is bound but not in %s (found in: %s)",
				id, standaloneFile, bindings[0].Path)
		}
	}

	// ── 3: outer Describe carries "default-profile" label ────────────────────
	// Read the source file and check for the label string.
	srcPath := filepath.Join(repoRoot, standaloneFile)
	content, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("AC3 [E33-standalone]: read %s: %v", standaloneFile, err)
	}
	src := string(content)
	if !strings.Contains(src, `Label("default-profile"`) {
		t.Errorf("AC3 [E33-standalone]: %s outer Describe is missing "+
			`Label("default-profile", ...) — specs will not run under the default label filter`,
			standaloneFile)
	}

	if !t.Failed() {
		t.Logf("AC3 [E33-standalone]: all %d E33.4 specs verified in catalogue + "+
			"statically bound in %s with default-profile label", len(wantIDs), standaloneFile)
	}
}

func TestCatalogLoadPreservesSectionKey(t *testing.T) {
	t.Parallel()

	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	catalog, err := LoadCatalog(repoRoot)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	// Verify that cluster-level cases (all-digit IDs) have a non-empty
	// SectionKey so GinkgoNodeID() can produce the normalised E33.285 form.
	var checkedDigit bool
	for _, tc := range catalog.Cases {
		if allDigits(tc.ID) {
			if tc.SectionKey == "" {
				t.Errorf("numeric row ID %q has empty SectionKey (section=%q)", tc.ID, tc.Section)
			}
			checkedDigit = true
			break
		}
	}
	if !checkedDigit {
		t.Fatalf("no all-digit row IDs found in catalog — spec document must contain numeric cluster-level case IDs")
	}
}
