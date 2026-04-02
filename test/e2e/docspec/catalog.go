package docspec

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	documentRelativePath = "docs/E2E-TESTCASES.md"
	manualSectionHeading = "## 유형 M:"
	goModFileName        = "go.mod"
)

// Case captures one concrete executable case row from docs/E2E-TESTCASES.md.
type Case struct {
	ID         string
	Symbol     string
	Section    string
	Subsection string
	// SectionKey is the E/F-prefixed key extracted from the ## section heading
	// (e.g. "E1", "E33", "F27").  It is used by GinkgoNodeID to produce
	// normalised TC IDs for cluster-level sections whose table rows carry plain
	// ordinal integers instead of E/F-prefixed identifiers.
	SectionKey string
	Line       int
}

// sectionKeyRE matches the leading E/F-group token in a section heading, e.g.
// "E33" in "## E33: Kind+LVM NVMe-oF テスト".
var sectionKeyRE = regexp.MustCompile(`^([EF]\d+)\b`)

// GinkgoNodeID returns the normalised TC ID that must appear inside the Ginkgo
// It-node name as "[TC-<GinkgoNodeID>]".
//
// Rules:
//   - If the row ID already starts with E or F it is used verbatim.
//   - If the row ID is all-numeric (cluster / full-E2E table rows) and a
//     SectionKey is available, the returned ID is "<SectionKey>.<ID>" so that
//     the node name matches the format produced by tcNodeLabel() in the e2e
//     package (e.g. the cluster-test row "285" under section "E33" becomes
//     "E33.285").
//   - Otherwise the raw ID is returned unchanged.
func (c Case) GinkgoNodeID() string {
	if c.ID == "" {
		return ""
	}
	first := c.ID[0]
	if first == 'E' || first == 'F' || first == 'M' {
		return c.ID
	}
	if allDigits(c.ID) && c.SectionKey != "" {
		return c.SectionKey + "." + c.ID
	}
	return c.ID
}

// GinkgoNodeLabel returns the "[TC-<GinkgoNodeID>]" bracket label that every
// matching Ginkgo It-node name must contain.
func (c Case) GinkgoNodeLabel() string {
	id := c.GinkgoNodeID()
	if id == "" {
		return ""
	}
	return "[TC-" + id + "]"
}

// GinkgoNodeBinding records a Ginkgo-node-level reference to a documented case
// (i.e. a string literal in Go source that contains "[TC-<ID>]").
type GinkgoNodeBinding struct {
	CaseID    string // normalised TC ID (GinkgoNodeID value)
	NodeLabel string // "[TC-<CaseID>]"
	Path      string // repo-relative file path
	Line      int    // 1-based line number of the match
	Evidence  string // trimmed source line containing the node label
}

// GinkgoTraceabilityReport summarises Ginkgo-node-name coverage against the
// spec catalogue.  It answers three questions:
//
//  1. Which documented TC IDs have at least one Ginkgo node label? (Matches)
//  2. Which documented TC IDs have no Ginkgo node label?            (Missing)
//  3. Which Ginkgo node labels reference IDs absent from the spec?  (Extra)
//  4. Which documented TC IDs appear in more than one node label?   (Duplicates)
type GinkgoTraceabilityReport struct {
	Catalog    Catalog
	Matches    map[string][]GinkgoNodeBinding // key: normalised TC ID
	Missing    []Case
	Extra      []GinkgoNodeBinding            // bindings whose ID is not in the catalogue
	Duplicates map[string][]GinkgoNodeBinding // IDs found in >1 node
}

// BoundCount returns the number of canonical TC IDs with at least one Ginkgo
// node binding.
func (r GinkgoTraceabilityReport) BoundCount() int { return len(r.Matches) }

// MissingCount returns the number of canonical TC IDs with no Ginkgo node
// binding.
func (r GinkgoTraceabilityReport) MissingCount() int { return len(r.Missing) }

// ExtraCount returns the number of Ginkgo node labels whose TC ID is absent
// from the spec document.
func (r GinkgoTraceabilityReport) ExtraCount() int { return len(r.Extra) }

// DuplicateCount returns the number of TC IDs that appear in more than one
// Ginkgo node label.
func (r GinkgoTraceabilityReport) DuplicateCount() int { return len(r.Duplicates) }

// Catalog is the parsed machine-readable view of the E2E spec document.
type Catalog struct {
	RepoRoot         string
	DocumentPath     string
	DeclaredTotal    int
	Cases            []Case
	CanonicalCases   []Case
	DuplicateSymbols map[string][]Case
}

// Binding records one code-level reference to a documented case.
type Binding struct {
	CaseID   string
	Symbol   string
	Path     string
	Line     int
	Kind     string
	Evidence string
}

// TraceabilityReport summarizes the current document-to-code binding state.
type TraceabilityReport struct {
	Catalog Catalog
	Matches map[string][]Binding
	Missing []Case
}

// BoundCount returns the number of canonical cases with at least one binding.
func (r TraceabilityReport) BoundCount() int {
	return len(r.Matches)
}

// MissingCount returns the number of canonical cases with no binding.
func (r TraceabilityReport) MissingCount() int {
	return len(r.Missing)
}

var (
	errRepoRootNotFound = errors.New("repository root not found")
)

// FindRepoRoot walks upward from start until it finds the repository root.
func FindRepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve start path: %w", err)
	}

	for {
		if fileExists(filepath.Join(dir, goModFileName)) &&
			fileExists(filepath.Join(dir, documentRelativePath)) {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errRepoRootNotFound
		}
		dir = parent
	}
}

// LoadCatalog parses docs/E2E-TESTCASES.md and returns the extracted catalog.
//
// The parser stops at the manual-only Type M section because the declared
// 437-case total explicitly excludes those manual scenarios.
func LoadCatalog(repoRoot string) (Catalog, error) {
	documentPath := filepath.Join(repoRoot, documentRelativePath)
	file, err := os.Open(documentPath)
	if err != nil {
		return Catalog{}, fmt.Errorf("open spec document: %w", err)
	}
	defer file.Close()

	var (
		declaredTotal     int
		currentSection    string
		currentSectionKey string
		currentSubsection string
		cases             []Case
	)

	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		if strings.HasPrefix(line, manualSectionHeading) {
			break
		}

		switch {
		case strings.HasPrefix(line, "## "):
			currentSection = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			currentSubsection = ""
			// Extract the E/F-group key from the section heading, e.g. "E33"
			// from "## E33: Kind+LVM NVMe-oF テスト".
			if m := sectionKeyRE.FindStringSubmatch(currentSection); len(m) == 2 {
				currentSectionKey = m[1]
			} else {
				currentSectionKey = ""
			}
		case strings.HasPrefix(line, "### "):
			currentSubsection = strings.TrimSpace(strings.TrimPrefix(line, "### "))
		}

		if declaredTotal == 0 {
			if total, ok := parseDeclaredTotal(line); ok {
				declaredTotal = total
			}
		}

		tc, ok := parseCaseRow(line, currentSection, currentSectionKey, currentSubsection, lineNo)
		if ok {
			cases = append(cases, tc)
		}
	}
	if err := scanner.Err(); err != nil {
		return Catalog{}, fmt.Errorf("scan spec document: %w", err)
	}
	if declaredTotal == 0 {
		return Catalog{}, fmt.Errorf("declared total not found in %s", documentRelativePath)
	}

	canonicalCases, duplicateSymbols := canonicalizeCases(cases)
	return Catalog{
		RepoRoot:         repoRoot,
		DocumentPath:     documentPath,
		DeclaredTotal:    declaredTotal,
		Cases:            cases,
		CanonicalCases:   canonicalCases,
		DuplicateSymbols: duplicateSymbols,
	}, nil
}

// BuildTraceabilityReport scans the Go tree and reports which documented cases
// are currently referenced by code.
func BuildTraceabilityReport(repoRoot string, catalog Catalog) (TraceabilityReport, error) {
	files, err := loadGoFiles(repoRoot)
	if err != nil {
		return TraceabilityReport{}, err
	}

	matches := make(map[string][]Binding, len(catalog.CanonicalCases))
	var missing []Case

	for _, tc := range catalog.CanonicalCases {
		var caseBindings []Binding
		for _, file := range files {
			binding, ok := findBinding(file, tc)
			if ok {
				caseBindings = append(caseBindings, binding)
			}
		}
		if len(caseBindings) == 0 {
			missing = append(missing, tc)
			continue
		}
		matches[tc.Symbol] = caseBindings
	}

	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Section != missing[j].Section {
			return missing[i].Section < missing[j].Section
		}
		if missing[i].Subsection != missing[j].Subsection {
			return missing[i].Subsection < missing[j].Subsection
		}
		return missing[i].ID < missing[j].ID
	})

	return TraceabilityReport{
		Catalog: catalog,
		Matches: matches,
		Missing: missing,
	}, nil
}

type sourceFile struct {
	RelativePath string
	Lines        []string
}

func parseDeclaredTotal(line string) (int, bool) {
	const marker = "총 테스트 케이스:"
	idx := strings.Index(line, marker)
	if idx < 0 {
		return 0, false
	}

	rest := strings.TrimSpace(line[idx+len(marker):])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}

	value, err := strconv.Atoi(strings.Trim(fields[0], "*"))
	if err != nil {
		return 0, false
	}
	return value, true
}

func parseCaseRow(line, section, sectionKey, subsection string, lineNo int) (Case, bool) {
	if !strings.HasPrefix(line, "|") {
		return Case{}, false
	}

	cells := splitMarkdownRow(line)
	if len(cells) < 2 {
		return Case{}, false
	}

	id := strings.TrimSpace(cells[0])
	symbol := strings.TrimSpace(cells[1])
	if !looksLikeCaseID(id) || !looksLikeBacktickedSymbol(symbol) {
		return Case{}, false
	}

	return Case{
		ID:         id,
		Symbol:     strings.Trim(symbol, "`"),
		Section:    section,
		SectionKey: sectionKey,
		Subsection: subsection,
		Line:       lineNo,
	}, true
}

func splitMarkdownRow(line string) []string {
	trimmed := strings.Trim(line, "|")
	rawCells := strings.Split(trimmed, "|")
	cells := make([]string, 0, len(rawCells))
	for _, cell := range rawCells {
		cells = append(cells, strings.TrimSpace(cell))
	}
	return cells
}

func looksLikeCaseID(value string) bool {
	if value == "" {
		return false
	}
	if allDigits(value) {
		return true
	}
	if strings.HasPrefix(value, "E") || strings.HasPrefix(value, "F") || strings.HasPrefix(value, "M") {
		for _, r := range value[1:] {
			if (r < '0' || r > '9') && r != '.' && r != '-' {
				return false
			}
		}
		return true
	}
	return false
}

func looksLikeBacktickedSymbol(value string) bool {
	return strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`")
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func canonicalizeCases(cases []Case) ([]Case, map[string][]Case) {
	firstBySymbol := make(map[string]Case, len(cases))
	duplicates := make(map[string][]Case)
	order := make([]string, 0, len(cases))

	for _, tc := range cases {
		if _, exists := firstBySymbol[tc.Symbol]; !exists {
			firstBySymbol[tc.Symbol] = tc
			order = append(order, tc.Symbol)
			continue
		}
		duplicates[tc.Symbol] = append(duplicates[tc.Symbol], tc)
	}

	canonical := make([]Case, 0, len(firstBySymbol))
	for _, symbol := range order {
		canonical = append(canonical, firstBySymbol[symbol])
	}
	return canonical, duplicates
}

func loadGoFiles(repoRoot string) ([]sourceFile, error) {
	var files []sourceFile
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		relativePath, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}
		files = append(files, sourceFile{
			RelativePath: filepath.ToSlash(relativePath),
			Lines:        strings.Split(string(content), "\n"),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan go files: %w", err)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].RelativePath < files[j].RelativePath
	})
	return files, nil
}

func findBinding(file sourceFile, tc Case) (Binding, bool) {
	for idx, line := range file.Lines {
		lineNo := idx + 1

		if strings.Contains(line, "func "+tc.Symbol+"(") {
			return Binding{
				CaseID:   tc.ID,
				Symbol:   tc.Symbol,
				Path:     file.RelativePath,
				Line:     lineNo,
				Kind:     "go_test_func",
				Evidence: strings.TrimSpace(line),
			}, true
		}
		if strings.Contains(line, tc.Symbol) {
			return Binding{
				CaseID:   tc.ID,
				Symbol:   tc.Symbol,
				Path:     file.RelativePath,
				Line:     lineNo,
				Kind:     "symbol_reference",
				Evidence: strings.TrimSpace(line),
			}, true
		}
		if tc.ID != "" && strings.Contains(line, tc.ID) {
			return Binding{
				CaseID:   tc.ID,
				Symbol:   tc.Symbol,
				Path:     file.RelativePath,
				Line:     lineNo,
				Kind:     "doc_id_reference",
				Evidence: strings.TrimSpace(line),
			}, true
		}
	}
	return Binding{}, false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ginkgoNodeLabelRE matches "[TC-<ID>]" tokens inside Ginkgo node-name string
// literals. The captured group is the raw normalised ID token.
//
// Pattern design:
//
//	The outer structure `(?:It|Describe|…)\s*\(\s*"` ensures the match is
//	anchored to the first string argument of a well-known Ginkgo function.
//	Requiring (?:\.\d+)+ (one or more ".N" segments) avoids matching
//	bare section-group tokens like "[TC-E1]" that appear in test comments.
//
//	The named-capture group grabs the normalised TC ID (e.g. "E1.2",
//	"F27.3", "E33.285") that the caller uses to look up the spec catalogue.
//
// Note: this regex does NOT match dynamically-constructed node names (e.g.
// `It(tc.tcNodeName(), ...)` where the string value is only known at runtime).
// For those cases use FindGinkgoNodeBindingsFromSpecNames with dry-run output.
// tcIDFragment matches the TC ID token inside a "[TC-<ID>]" bracket.
//
// Grammar:
//
//	[EF]         — prefix letter (E for E-series, F for F-series)
//	\d+          — group number (e.g. 1, 27, 33)
//	(?:\.\d+)+   — one or more ".N" decimal segments (e.g. .1, .10, .3.1)
//	(?:-\d+)?    — optional hyphen-suffix (e.g. -1 in E1.10-1)
const tcIDFragment = `[EF]\d+(?:\.\d+)+(?:-\d+)?`

var ginkgoNodeLabelRE = regexp.MustCompile(
	`(?:It|Describe|Context|When|DescribeTable|Entry)\s*\(\s*"[^"]*\[TC-(` + tcIDFragment + `)\]`,
)

// ginkgoNodeLabelLooseRE is the fallback used when the caller passes
// rawLines=true to findGinkgoBindingsInLines.  It matches any "[TC-E1.2]"
// token in a line, regardless of surrounding context, and is used for
// parsing pre-enumerated spec-name strings (e.g. ginkgo --dry-run output)
// where each line IS a spec name.
var ginkgoNodeLabelLooseRE = regexp.MustCompile(`\[TC-(` + tcIDFragment + `)\]`)

// FindGinkgoNodeBindings scans all Go files under repoRoot/test/e2e/ for
// "[TC-<ID>]" tokens inside Ginkgo It/Describe/Context/When/DescribeTable
// string-literal arguments and returns a GinkgoTraceabilityReport.
//
// The scan uses ginkgoNodeLabelRE which requires the TC label to appear inside
// a Ginkgo function call string, filtering out TC ID mentions in comments,
// test helper data, or plain strings.
//
// For dynamic node names built at runtime (e.g. tc.tcNodeName()), use
// FindGinkgoNodeBindingsFromSpecNames with the output of `ginkgo --dry-run`.
func FindGinkgoNodeBindings(repoRoot string, catalog Catalog) (GinkgoTraceabilityReport, error) {
	e2eDir := filepath.Join(repoRoot, "test", "e2e")
	files, err := loadGoFiles(e2eDir)
	if err != nil {
		return GinkgoTraceabilityReport{}, fmt.Errorf("scan test/e2e Go files: %w", err)
	}

	allBindings := collectGinkgoBindingsFromFiles(files, false)
	return buildGinkgoReport(catalog, allBindings), nil
}

// FindGinkgoNodeBindingsFromSpecNames builds a GinkgoTraceabilityReport from a
// slice of pre-enumerated Ginkgo spec names (e.g. from `ginkgo --dry-run`
// output or a Ginkgo JSON report).  Each entry in specNames is treated as a
// complete spec full name; the function extracts every "[TC-<ID>]" token from
// each name using the loose regex (no Ginkgo-call context required).
//
// specNames format: each element is one full Ginkgo spec name such as
//
//	"[TC-E1.1] TC[001/437] E1.1 :: TestCSIController_CreateVolume"
//
// The source Path and Line in returned GinkgoNodeBindings are set to the
// pseudo-path "spec-list:<index>" so callers can identify the entry.
func FindGinkgoNodeBindingsFromSpecNames(catalog Catalog, specNames []string) GinkgoTraceabilityReport {
	allBindings := make(map[string][]GinkgoNodeBinding)
	for i, name := range specNames {
		pseudoPath := fmt.Sprintf("spec-list:%d", i+1)
		for _, m := range ginkgoNodeLabelLooseRE.FindAllStringSubmatch(name, -1) {
			nodeID := m[1]
			b := GinkgoNodeBinding{
				CaseID:    nodeID,
				NodeLabel: "[TC-" + nodeID + "]",
				Path:      pseudoPath,
				Line:      1,
				Evidence:  strings.TrimSpace(name),
			}
			allBindings[nodeID] = append(allBindings[nodeID], b)
		}
	}
	return buildGinkgoReport(catalog, allBindings)
}

// collectGinkgoBindingsFromFiles scans a set of sourceFiles for "[TC-<ID>]"
// occurrences.  When rawLines is false it uses ginkgoNodeLabelRE (anchored to
// Ginkgo call contexts); when true it uses ginkgoNodeLabelLooseRE.
func collectGinkgoBindingsFromFiles(files []sourceFile, rawLines bool) map[string][]GinkgoNodeBinding {
	re := ginkgoNodeLabelRE
	if rawLines {
		re = ginkgoNodeLabelLooseRE
	}

	allBindings := make(map[string][]GinkgoNodeBinding)
	for _, f := range files {
		for idx, line := range f.Lines {
			// Skip pure comment lines to reduce false-positive matches.
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			lineNo := idx + 1
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				nodeID := m[1]
				b := GinkgoNodeBinding{
					CaseID:    nodeID,
					NodeLabel: "[TC-" + nodeID + "]",
					Path:      f.RelativePath,
					Line:      lineNo,
					Evidence:  strings.TrimSpace(line),
				}
				allBindings[nodeID] = append(allBindings[nodeID], b)
			}
		}
	}
	return allBindings
}

// buildGinkgoReport partitions a raw binding map into Matches / Missing /
// Extra / Duplicates and returns the completed GinkgoTraceabilityReport.
func buildGinkgoReport(catalog Catalog, allBindings map[string][]GinkgoNodeBinding) GinkgoTraceabilityReport {
	// Build a lookup: normalised GinkgoNodeID → Case.
	specByNodeID := make(map[string]Case, len(catalog.CanonicalCases))
	for _, tc := range catalog.CanonicalCases {
		nodeID := tc.GinkgoNodeID()
		if nodeID != "" {
			specByNodeID[nodeID] = tc
		}
	}

	matches := make(map[string][]GinkgoNodeBinding)
	var extra []GinkgoNodeBinding
	duplicates := make(map[string][]GinkgoNodeBinding)

	for nodeID, bindings := range allBindings {
		if _, inSpec := specByNodeID[nodeID]; !inSpec {
			extra = append(extra, bindings...)
			continue
		}
		matches[nodeID] = bindings
		if len(bindings) > 1 {
			duplicates[nodeID] = bindings
		}
	}

	var missing []Case
	for _, tc := range catalog.CanonicalCases {
		nodeID := tc.GinkgoNodeID()
		if nodeID == "" {
			continue
		}
		if _, found := matches[nodeID]; !found {
			missing = append(missing, tc)
		}
	}

	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Section != missing[j].Section {
			return missing[i].Section < missing[j].Section
		}
		return missing[i].GinkgoNodeID() < missing[j].GinkgoNodeID()
	})
	sort.Slice(extra, func(i, j int) bool {
		if extra[i].Path != extra[j].Path {
			return extra[i].Path < extra[j].Path
		}
		return extra[i].Line < extra[j].Line
	})

	return GinkgoTraceabilityReport{
		Catalog:    catalog,
		Matches:    matches,
		Missing:    missing,
		Extra:      extra,
		Duplicates: duplicates,
	}
}
