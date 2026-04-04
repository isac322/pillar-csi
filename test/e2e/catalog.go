package e2e

import (
	"fmt"
	goruntime "runtime"
	"strings"

	"github.com/bhyoo/pillar-csi/test/e2e/docspec"
)

// defaultProfileCaseCount is the number of TC entries assembled by
// buildDefaultProfile() — the catalog-driven portion of the default profile.
//
// The default-profile cases break down (new 6-file catalog structure):
//
//	219 in-process  (COMPONENT-TESTS.md E1–E30 + INTEGRATION-TESTS.md E28)
//	166 envtest     (INTEGRATION-TESTS.md E19, E20, E21, E23, E25, E26, E32)
//	 40 cluster     (INTEGRATION-TESTS.md E27 + E2E-TESTS.md E10)
//	──────────────
//	425 catalog total, excluding standalone E33/E34/E35, F27-F31, and PRD-gap
//	    cases without execution plans (C-NEW, E-NEW, E-FAULT, NEW-U).
//
// Additionally, the following default-profile specs from dedicated test files
// are included in the canonical total but are NOT managed by buildDefaultProfile:
//
//	backend_teardown_ac43_e2e_test.go   → 5 (ac:4.3 backend teardown absence)
//	teardown_panic_guarantee_test.go    → 4 (ac:3 teardown-guarantee)
//	lvm_backend_standalone_e2e_test.go  → 7 (TC-E33.311–317, build: e2e)
//
// Total default-profile spec count: 425 + 7 + 4 + 5 = 441.
//
// Note: E33, E34, E35, F27–F31 standalone cluster specs are NOT in the
// catalog-driven profile. They live directly in *_e2e_test.go files with
// Label("default-profile") and run under the default label filter without
// going through the catalog.
const defaultProfileCaseCount = 425

// componentSectionKeys is the set of E-group section keys from
// COMPONENT-TESTS.md that are included in the in-process default profile.
// These keys have known execution plans in resolveLocalExecutionPlan().
var componentSectionKeys = map[string]bool{
	"E1": true, "E2": true, "E3": true, "E7": true, "E8": true, "E9": true,
	"E11": true, "E15": true, "E16": true, "E17": true, "E18": true,
	"E21": true, "E24": true, "E29": true, "E30": true,
}

// envtestSectionKeys is the set of E-group section keys from
// INTEGRATION-TESTS.md that are included in the envtest default profile.
var envtestSectionKeys = map[string]bool{
	"E19": true, "E20": true, "E21": true,
	"E23": true, "E25": true, "E26": true, "E32": true,
}

type documentedCase struct {
	Ordinal         int
	Category        string
	GroupKey        string
	DocID           string
	TestName        string
	SectionTitle    string
	SubsectionTitle string
	DocLine         int
}

func (tc documentedCase) specText() string {
	return fmt.Sprintf("TC[%03d/%03d] %s :: %s", tc.Ordinal, defaultProfileCaseCount, tc.DocID, tc.TestName)
}

// tcNodeLabel returns the deterministic Ginkgo node label for this TC.
// The label embeds the TC ID in [TC-<ID>] format so that individual specs
// can be addressed via --ginkgo.focus="TC-E1\.1" or go test with the
// pattern matching against the subtest path element.
//
// Example: TC "E1.1" → "[TC-E1.1]"
func (tc documentedCase) tcNodeLabel() string {
	return fmt.Sprintf("[TC-%s]", tc.DocID)
}

// tcNodeName returns the full deterministic Ginkgo It-node name for this TC.
// The name contains both the [TC-<ID>] label (for focus filtering) and the
// legacy specText() (for ordinal, group, and human-readable test function
// name). The format is:
//
//	[TC-E1.1] TC[001/425] E1.1 :: TestCSIController_CreateVolume
//
// Note: inferTimingIdentity in timing_capture.go relies on the "::" separator
// and the LAST "]" appearing before the DocID token. The [TC-<ID>] prefix is
// placed before TC[ordinal/total] so that LastIndex("]") still finds the
// bracket in TC[ordinal/total] and correctly extracts the DocID suffix.
func (tc documentedCase) tcNodeName() string {
	return fmt.Sprintf("%s %s", tc.tcNodeLabel(), tc.specText())
}

// repoRootFromCaller returns the repository root directory by walking up from
// this source file's location (catalog.go in test/e2e/).
func repoRootFromCaller() string {
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		panic("unable to resolve caller path for e2e catalog")
	}
	// catalog.go is at <repo>/test/e2e/catalog.go
	// go up two directories: test/e2e -> test -> repo root
	return resolveRepoRoot(file)
}

func resolveRepoRoot(catalogFile string) string {
	// Walk up from test/e2e/catalog.go to repo root.
	// The file path is: .../test/e2e/catalog.go
	// We need: .../
	parts := strings.Split(catalogFile, "/")
	// Remove "catalog.go", "e2e", "test" from the end
	if len(parts) >= 3 {
		parts = parts[:len(parts)-3]
	}
	return strings.Join(parts, "/")
}

// buildDefaultProfile loads the 6-file catalog via docspec.LoadCatalog() and
// selects the cases that belong to the catalog-driven default profile.
//
// Cases are included based on their source file and section key:
//
//	COMPONENT-TESTS.md + componentSectionKeys  → "in-process"
//	INTEGRATION-TESTS.md + E28                 → "in-process" (agent gRPC)
//	INTEGRATION-TESTS.md + envtestSectionKeys  → "envtest"
//	INTEGRATION-TESTS.md + E27                 → "cluster" (Helm chart)
//	E2E-TESTS.md + E10                         → "cluster" (Kind bootstrap)
//
// Cases that do not match any of the above rules are excluded from the
// catalog-driven profile (they are handled by standalone *_e2e_test.go files
// or belong to unit/performance test layers).
func buildDefaultProfile() ([]documentedCase, error) {
	repoRoot := repoRootFromCaller()

	cat, err := docspec.LoadCatalog(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("load test catalog: %w", err)
	}

	selected := make([]documentedCase, 0, defaultProfileCaseCount)

	for _, c := range cat.CanonicalCases {
		dc, include := specCaseToDocumentedCase(c)
		if !include {
			continue
		}
		selected = append(selected, dc)
	}

	if len(selected) != defaultProfileCaseCount {
		return nil, fmt.Errorf(
			"default profile expected %d cases, found %d — "+
				"update defaultProfileCaseCount or adjust catalog filters",
			defaultProfileCaseCount, len(selected),
		)
	}

	// Assign ordinals after selection so the ordinal reflects the ordered
	// position in the profile (matches TC[001/425] display in spec names).
	seen := make(map[string]struct{}, len(selected))
	for i := range selected {
		selected[i].Ordinal = i + 1
		traceKey := fmt.Sprintf("%s|%s|%s",
			selected[i].GroupKey, selected[i].DocID, selected[i].TestName)
		if _, exists := seen[traceKey]; exists {
			return nil, fmt.Errorf("duplicate trace key in default profile: %s", traceKey)
		}
		seen[traceKey] = struct{}{}
	}

	// Validate that all TC node labels ([TC-<DocID>]) are distinct.
	if err := validateTCNodeLabelUniqueness(selected); err != nil {
		return nil, err
	}

	return selected, nil
}

// specCaseToDocumentedCase converts a docspec.Case into a documentedCase and
// reports whether the case should be included in the default profile.
//
// Category assignment follows the source-file and section-key rules described
// in buildDefaultProfile().
func specCaseToDocumentedCase(c docspec.Case) (documentedCase, bool) {
	var category string

	switch {
	// In-process: COMPONENT-TESTS.md with known section keys
	case strings.HasSuffix(c.SourceFile, "COMPONENT-TESTS.md") &&
		componentSectionKeys[c.SectionKey]:
		category = "in-process"

	// In-process: INTEGRATION-TESTS.md E28 (agent gRPC + LVM backend)
	case strings.HasSuffix(c.SourceFile, "INTEGRATION-TESTS.md") &&
		c.SectionKey == "E28":
		category = "in-process"

	// Envtest: INTEGRATION-TESTS.md with envtest section keys
	case strings.HasSuffix(c.SourceFile, "INTEGRATION-TESTS.md") &&
		envtestSectionKeys[c.SectionKey]:
		category = "envtest"

	// Cluster: INTEGRATION-TESTS.md E27 (Helm chart deployment tests)
	case strings.HasSuffix(c.SourceFile, "INTEGRATION-TESTS.md") &&
		c.SectionKey == "E27":
		category = "cluster"

	// Cluster: E2E-TESTS.md E10 (Kind bootstrap lifecycle)
	case strings.HasSuffix(c.SourceFile, "E2E-TESTS.md") &&
		c.SectionKey == "E10":
		category = "cluster"

	default:
		// Exclude: unit tests, performance tests, CSI sanity tests,
		// E33/E34/E35 (standalone files), F27–F31 (standalone files),
		// E-FAULT/E-NEW (standalone files), C-NEW/I-NEW/NEW-U (PRD gap).
		return documentedCase{}, false
	}

	// Compute DocID: numeric ordinals get prefixed with the section key.
	docID := c.ID
	if isAllDigits(c.ID) && c.SectionKey != "" {
		docID = c.SectionKey + "." + c.ID
	}

	return documentedCase{
		Category:        category,
		GroupKey:        c.SectionKey,
		DocID:           docID,
		TestName:        c.Symbol,
		SectionTitle:    c.Section,
		SubsectionTitle: c.Subsection,
		DocLine:         c.Line,
	}, true
}

func mustBuildDefaultProfile() []documentedCase {
	cases, err := buildDefaultProfile()
	if err != nil {
		panic(err)
	}

	return cases
}

// isAllDigits returns true when s is a non-empty string composed entirely of
// ASCII decimal digits.  Used to detect purely numeric row IDs in cluster /
// full-E2E spec table rows so they can be prefixed with the section group key.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
