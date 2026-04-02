package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bhyoo/pillar-csi/test/e2e/docspec"
)

func main() {
	var (
		showMissing    bool
		ginkgoMode     bool
		ginkgoRuntime  bool
		ginkgoSpecFile string
		strict         bool
		jsonOutput     bool
	)
	flag.BoolVar(&showMissing, "missing", false,
		"print every missing canonical case (symbol mode)")
	flag.BoolVar(&ginkgoMode, "ginkgo", false,
		"check 1-to-1 match between spec TC IDs and Ginkgo node labels [TC-<ID>]\n"+
			"\tStatic mode: scans It/Describe string literals in test/e2e/*.go\n"+
			"\tCombine with --runtime for dynamic check via ginkgo --dry-run")
	flag.BoolVar(&ginkgoRuntime, "runtime", false,
		"(with --ginkgo) run 'ginkgo --dry-run ./test/e2e/' to enumerate actual\n"+
			"\tGinkgo spec names; requires ginkgo CLI on PATH or GINKGO_BIN env var")
	flag.StringVar(&ginkgoSpecFile, "spec-file", "",
		"(with --ginkgo) path to a file containing one Ginkgo spec name per line\n"+
			"\t(e.g. produced by: ginkgo --dry-run ./test/e2e/ 2>&1 | grep '\\[TC-')")
	flag.BoolVar(&strict, "strict", false,
		"exit 1 when there are MISSING, EXTRA, or DUPLICATE entries (ginkgo mode)")
	flag.BoolVar(&jsonOutput, "json", false,
		"emit JSON-structured report to stdout")
	flag.Parse()

	repoRoot, err := docspec.FindRepoRoot(".")
	if err != nil {
		exitf("find repo root: %v", err)
	}

	catalog, err := docspec.LoadCatalog(repoRoot)
	if err != nil {
		exitf("load catalog: %v", err)
	}

	switch {
	case ginkgoMode && ginkgoRuntime:
		runGinkgoRuntimeCheck(repoRoot, catalog, strict, jsonOutput)
	case ginkgoMode && ginkgoSpecFile != "":
		runGinkgoSpecFileCheck(catalog, ginkgoSpecFile, strict, jsonOutput)
	case ginkgoMode:
		runGinkgoStaticCheck(repoRoot, catalog, strict, jsonOutput)
	default:
		runSymbolCheck(repoRoot, catalog, showMissing, jsonOutput)
	}
}

// ── Ginkgo runtime mode ───────────────────────────────────────────────────────

// runGinkgoRuntimeCheck runs `ginkgo --dry-run ./test/e2e/`, collects the
// emitted spec names, and asserts a 1-to-1 match against the spec catalogue.
//
// The ginkgo binary is resolved in this order:
//  1. $GINKGO_BIN environment variable
//  2. $PATH lookup for "ginkgo"
//  3. `go run github.com/onsi/ginkgo/v2/ginkgo` (fallback)
func runGinkgoRuntimeCheck(repoRoot string, catalog docspec.Catalog, strict, jsonOutput bool) {
	specNames, err := enumerateGinkgoSpecs(repoRoot)
	if err != nil {
		exitf("enumerate ginkgo specs: %v", err)
	}

	report := docspec.FindGinkgoNodeBindingsFromSpecNames(catalog, specNames)
	printGinkgoReport(report, strict, jsonOutput)
}

// runGinkgoSpecFileCheck reads pre-enumerated spec names from a file and
// asserts a 1-to-1 match against the catalogue.  Each non-empty line in the
// file is treated as one Ginkgo spec name.
func runGinkgoSpecFileCheck(catalog docspec.Catalog, specFile string, strict, jsonOutput bool) {
	f, err := os.Open(specFile) //nolint:gosec // path supplied by operator
	if err != nil {
		exitf("open spec file %s: %v", specFile, err)
	}
	defer f.Close()

	var specNames []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			specNames = append(specNames, line)
		}
	}
	if err := sc.Err(); err != nil {
		exitf("read spec file: %v", err)
	}

	report := docspec.FindGinkgoNodeBindingsFromSpecNames(catalog, specNames)
	printGinkgoReport(report, strict, jsonOutput)
}

// runGinkgoStaticCheck performs a static scan of test/e2e/*.go files looking
// for "[TC-<ID>]" tokens inside Ginkgo It/Describe string literals.
func runGinkgoStaticCheck(repoRoot string, catalog docspec.Catalog, strict, jsonOutput bool) {
	report, err := docspec.FindGinkgoNodeBindings(repoRoot, catalog)
	if err != nil {
		exitf("build ginkgo traceability report: %v", err)
	}
	printGinkgoReport(report, strict, jsonOutput)
}

// enumerateGinkgoSpecs runs `ginkgo --dry-run` in the test/e2e directory and
// returns the list of spec names printed to stdout/stderr.
func enumerateGinkgoSpecs(repoRoot string) ([]string, error) {
	ginkgoBin := resolveGinkgoBin()
	e2eDir := filepath.Join(repoRoot, "test", "e2e")

	var cmdArgs []string
	var cmdName string

	if ginkgoBin != "" {
		cmdName = ginkgoBin
		cmdArgs = []string{"--dry-run", "-v", e2eDir}
	} else {
		// Fallback: go run github.com/onsi/ginkgo/v2/ginkgo
		cmdName = "go"
		cmdArgs = []string{"run", "github.com/onsi/ginkgo/v2/ginkgo",
			"--dry-run", "-v", e2eDir}
	}

	cmd := exec.Command(cmdName, cmdArgs...) //nolint:gosec
	cmd.Dir = repoRoot

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// ginkgo --dry-run exits 0 even when no specs match, so we ignore the
	// exit code but still report exec failures.
	if err := cmd.Run(); err != nil {
		// Non-zero exit is acceptable for --dry-run (spec failures are reported
		// inline), but a hard exec error (binary not found, etc.) is fatal.
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, fmt.Errorf("exec %s: %w", cmdName, err)
		}
	}

	// Collect all non-empty lines from both stdout and stderr.
	combined := append(stdout.Bytes(), stderr.Bytes()...)
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(combined))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// resolveGinkgoBin returns the path to the ginkgo binary, or "" if not found.
func resolveGinkgoBin() string {
	if v := os.Getenv("GINKGO_BIN"); v != "" {
		return v
	}
	if path, err := exec.LookPath("ginkgo"); err == nil {
		return path
	}
	return ""
}

// ── shared Ginkgo report output ───────────────────────────────────────────────

// ginkgoReportJSON is the JSON-serialisable summary emitted with --json.
type ginkgoReportJSON struct {
	DeclaredTotal  int                                    `json:"declared_total"`
	CanonicalCases int                                    `json:"canonical_cases"`
	Bound          int                                    `json:"bound"`
	Missing        int                                    `json:"missing"`
	Extra          int                                    `json:"extra"`
	Duplicates     int                                    `json:"duplicates"`
	MissingCases   []ginkgoMissingEntry                   `json:"missing_cases,omitempty"`
	ExtraCases     []docspec.GinkgoNodeBinding            `json:"extra_cases,omitempty"`
	DuplicateCases map[string][]docspec.GinkgoNodeBinding `json:"duplicate_cases,omitempty"`
}

type ginkgoMissingEntry struct {
	GinkgoNodeID string `json:"ginkgo_node_id"`
	RawID        string `json:"raw_id"`
	Symbol       string `json:"symbol"`
	Section      string `json:"section"`
	Subsection   string `json:"subsection"`
}

func printGinkgoReport(report docspec.GinkgoTraceabilityReport, strict, jsonOutput bool) {
	if jsonOutput {
		emitGinkgoJSON(report)
	} else {
		emitGinkgoText(report)
	}

	if strict && (report.MissingCount() > 0 || report.ExtraCount() > 0 || report.DuplicateCount() > 0) {
		os.Exit(1)
	}
}

func emitGinkgoText(report docspec.GinkgoTraceabilityReport) {
	fmt.Printf(
		"declared_total=%d canonical_cases=%d bound=%d missing=%d extra=%d duplicates=%d\n",
		report.Catalog.DeclaredTotal,
		len(report.Catalog.CanonicalCases),
		report.BoundCount(),
		report.MissingCount(),
		report.ExtraCount(),
		report.DuplicateCount(),
	)

	if report.ExtraCount() > 0 {
		fmt.Fprintf(os.Stderr, "\nEXTRA Ginkgo node labels (not in spec document):\n")
		for _, b := range report.Extra {
			fmt.Fprintf(os.Stderr, "  EXTRA  [TC-%s]  %s:%d\n\t%s\n",
				b.CaseID, b.Path, b.Line, b.Evidence)
		}
	}

	if report.DuplicateCount() > 0 {
		fmt.Fprintf(os.Stderr, "\nDUPLICATE Ginkgo node labels (same TC ID in >1 node):\n")
		nodeIDs := make([]string, 0, len(report.Duplicates))
		for id := range report.Duplicates {
			nodeIDs = append(nodeIDs, id)
		}
		sort.Strings(nodeIDs)
		for _, id := range nodeIDs {
			fmt.Fprintf(os.Stderr, "  DUPLICATE [TC-%s] appears in:\n", id)
			for _, b := range report.Duplicates[id] {
				fmt.Fprintf(os.Stderr, "    %s:%d\n\t%s\n", b.Path, b.Line, b.Evidence)
			}
		}
	}

	if report.MissingCount() > 0 {
		fmt.Fprintf(os.Stderr, "\nMISSING Ginkgo node labels (%d of %d — in spec but no [TC-<ID>] node found):\n",
			report.MissingCount(), len(report.Catalog.CanonicalCases))
		for _, tc := range report.Missing {
			fmt.Fprintf(os.Stderr, "  MISSING [TC-%s]  %s  %s\n",
				tc.GinkgoNodeID(), tc.Symbol, tc.Section)
		}
	}
}

func emitGinkgoJSON(report docspec.GinkgoTraceabilityReport) {
	missing := make([]ginkgoMissingEntry, 0, len(report.Missing))
	for _, tc := range report.Missing {
		missing = append(missing, ginkgoMissingEntry{
			GinkgoNodeID: tc.GinkgoNodeID(),
			RawID:        tc.ID,
			Symbol:       tc.Symbol,
			Section:      tc.Section,
			Subsection:   tc.Subsection,
		})
	}

	r := ginkgoReportJSON{
		DeclaredTotal:  report.Catalog.DeclaredTotal,
		CanonicalCases: len(report.Catalog.CanonicalCases),
		Bound:          report.BoundCount(),
		Missing:        report.MissingCount(),
		Extra:          report.ExtraCount(),
		Duplicates:     report.DuplicateCount(),
		MissingCases:   missing,
		ExtraCases:     report.Extra,
		DuplicateCases: report.Duplicates,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		exitf("encode JSON: %v", err)
	}
}

// ── Symbol mode (original behaviour) ─────────────────────────────────────────

type symbolReportJSON struct {
	DeclaredTotal    int `json:"declared_total"`
	ConcreteRows     int `json:"concrete_rows"`
	CanonicalCases   int `json:"canonical_cases"`
	DuplicateSymbols int `json:"duplicate_symbols"`
	Bound            int `json:"bound"`
	Missing          int `json:"missing"`
}

func runSymbolCheck(repoRoot string, catalog docspec.Catalog, showMissing, jsonOutput bool) {
	report, err := docspec.BuildTraceabilityReport(repoRoot, catalog)
	if err != nil {
		exitf("build traceability report: %v", err)
	}

	if jsonOutput {
		r := symbolReportJSON{
			DeclaredTotal:    catalog.DeclaredTotal,
			ConcreteRows:     len(catalog.Cases),
			CanonicalCases:   len(catalog.CanonicalCases),
			DuplicateSymbols: len(catalog.DuplicateSymbols),
			Bound:            report.BoundCount(),
			Missing:          report.MissingCount(),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			exitf("encode JSON: %v", err)
		}
		return
	}

	fmt.Printf(
		"declared_total=%d concrete_rows=%d canonical_cases=%d duplicate_symbols=%d bound=%d missing=%d\n",
		catalog.DeclaredTotal,
		len(catalog.Cases),
		len(catalog.CanonicalCases),
		len(catalog.DuplicateSymbols),
		report.BoundCount(),
		report.MissingCount(),
	)

	if !showMissing {
		return
	}

	missing := append([]docspec.Case(nil), report.Missing...)
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Section != missing[j].Section {
			return missing[i].Section < missing[j].Section
		}
		if missing[i].Subsection != missing[j].Subsection {
			return missing[i].Subsection < missing[j].Subsection
		}
		return missing[i].ID < missing[j].ID
	})

	for _, tc := range missing {
		fmt.Printf(
			"%s\t%s\t%s\t%s\n",
			tc.ID,
			tc.Symbol,
			tc.Section,
			tc.Subsection,
		)
	}
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
