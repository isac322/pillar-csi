package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// defaultProfileCaseCount is the number of TC entries assembled by
// buildDefaultProfile() — the catalog-driven portion of the default profile.
//
// The default-profile cases break down as:
//
//	239 in-process (E1–E24, E28–E30)
//	104 envtest    (E19, E20, E23, E25–E26, E32)
//	 13 envtest    (remainder from E21 overspill)
//	──────────────
//	356 catalog total, excluding cluster + full-lvm groups
//
// Additionally:
//
//	 32 cluster    (E10=3, E27=29) — managed by catalog
//	──────────────
//	388 catalog cases assembled by buildDefaultProfile()
//
// The remaining 33 cases (E33=33) are implemented as real-backend Ginkgo specs
// in dedicated *_e2e_test.go files that carry Label("default-profile",...).
// Those files compile unconditionally and run under the default label
// filter, so the total default-profile spec count is 388+33 = 421.
// This 421 total is the canonical "실제 실행되는 테스트 케이스" count declared
// in docs/E2E-TESTCASES.md (239 in-process + 117 envtest + 65 cluster).
//
// Note: E34 (13), E35 (13), F27 (9), F28 (2), F29 (3), F30 (3), F31 (2) = 45
// specs are NOT in the default-profile because they require host-level iSCSI
// or NVMe-oF initiator tooling (iscsi_tcp module / iscsiadm / nvme CLI) that
// is not available in the standard CI host environment.
const defaultProfileCaseCount = 388

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

type sectionQuota struct {
	Key   string
	Count int
}

var (
	headingRE = regexp.MustCompile(`^(#{2,6})\s+(.*)$`)
	rowRE     = regexp.MustCompile("^\\|\\s*([^|]+?)\\s*\\|\\s*`([^`]+)`[^|]*\\|")
	keyRE     = regexp.MustCompile(`^([EF]\d+)\b`)

	// Sub-AC 1 locks the default profile to a deterministic 388-case view from
	// docs/E2E-TESTCASES.md (239 in-process + 117 envtest + 32 cluster).
	// Combined with the 33 E33 real-backend specs (in *_e2e_test.go files),
	// the total default-profile running TC count is 421 as declared in the doc.
	// The selector below codifies deterministic quotas per group instead of
	// depending on raw row count (which changes as the document evolves).
	defaultInProcessQuotas = []sectionQuota{
		{Key: "E1", Count: 13},
		{Key: "E2", Count: 8},
		{Key: "E3", Count: 70},
		{Key: "E4", Count: 4},
		{Key: "E5", Count: 6},
		{Key: "E6", Count: 5},
		{Key: "E7", Count: 5},
		{Key: "E8", Count: 3},
		{Key: "E9", Count: 6},
		{Key: "E11", Count: 8},
		{Key: "E12", Count: 4},
		{Key: "E13", Count: 2},
		{Key: "E14", Count: 15},
		{Key: "E15", Count: 6},
		{Key: "E16", Count: 7},
		{Key: "E17", Count: 8},
		{Key: "E18", Count: 6},
		{Key: "E21", Count: 6},
		{Key: "E22", Count: 12},
		{Key: "E28", Count: 30},
		{Key: "E29", Count: 12},
		{Key: "E30", Count: 3},
	}
	defaultEnvtestQuotas = []sectionQuota{
		{Key: "E19", Count: 19},
		{Key: "E20", Count: 20},
		{Key: "E23", Count: 24},
		{Key: "E25", Count: 41},
	}
	defaultClusterQuotas = []sectionQuota{
		{Key: "E10", Count: 3},
		// E27: Helm chart installation and release validation tests (Sub-AC 3).
		// The 29-case quota matches the "Helm 설치 검증 29개 테스트" reference in
		// the Category 2 section header of docs/E2E-TESTCASES.md.
		// Real cluster validation is in tc_e27_helm_e2e_test.go (no build tag).
		{Key: "E27", Count: 29},
		// E33, E34, E35, F27–F31 are NOT in the catalog-driven profile.
		// They live in dedicated *_e2e_test.go files (no build tag) with
		// Label("default-profile",...) on the outer Describe so Ginkgo picks
		// them up automatically under the default label filter.
	}
)

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
//	[TC-E1.1] TC[001/388] E1.1 :: TestCSIController_CreateVolume
//
// Note: inferTimingIdentity in timing_capture.go relies on the "::" separator
// and the LAST "]" appearing before the DocID token. The [TC-<ID>] prefix is
// placed before TC[ordinal/total] so that LastIndex("]") still finds the
// bracket in TC[ordinal/total] and correctly extracts the DocID suffix.
func (tc documentedCase) tcNodeName() string {
	return fmt.Sprintf("%s %s", tc.tcNodeLabel(), tc.specText())
}

func docCatalogPath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("unable to resolve caller path for e2e catalog")
	}

	return filepath.Join(filepath.Dir(file), "..", "..", "docs", "E2E-TESTCASES.md")
}

func isCaseTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "| ID | 테스트 함수 |") ||
		strings.HasPrefix(trimmed, "| # | 테스트 함수 |")
}

func extractGroupKey(title string) string {
	matches := keyRE.FindStringSubmatch(strings.TrimSpace(title))
	if len(matches) != 2 {
		return ""
	}

	return matches[1]
}

func parseDocumentedCases() ([]documentedCase, error) {
	path := docCatalogPath()
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(content), "\n")
	cases := make([]documentedCase, 0, 768)

	var (
		sectionTitle string
		sectionKey   string
		subTitle     string
		subKey       string
		inTable      bool
	)

	for idx, line := range lines {
		if matches := headingRE.FindStringSubmatch(line); len(matches) == 3 {
			level := len(matches[1])
			title := strings.TrimSpace(matches[2])
			switch level {
			case 2:
				sectionTitle = title
				sectionKey = extractGroupKey(title)
				subTitle = ""
				subKey = ""
			default:
				subTitle = title
				subKey = extractGroupKey(title)
			}
		}

		if isCaseTableHeader(line) {
			inTable = true
			continue
		}

		if !inTable {
			continue
		}

		if strings.HasPrefix(line, "|---") || strings.HasPrefix(line, "|----") {
			continue
		}

		if !strings.HasPrefix(line, "|") {
			inTable = false
			continue
		}

		matches := rowRE.FindStringSubmatch(line)
		if len(matches) != 3 {
			continue
		}

		groupKey := sectionKey
		if strings.HasPrefix(sectionTitle, "유형 F") && subKey != "" {
			groupKey = subKey
		}

		if groupKey == "" {
			continue
		}

		rawDocID := strings.TrimSpace(matches[1])
		// Rows in cluster / full-E2E sections use plain ordinal numbers as the
		// first column (e.g. 285, 318) instead of E-prefixed identifiers.
		// Prefix them with the group key so the TC node name starts with
		// [TC-E33.285], [TC-E34.318], etc. — required by AC 7 which checks
		// that every spec node name contains [TC-<EorF><digit>].
		docID := rawDocID
		if isAllDigits(rawDocID) && groupKey != "" {
			docID = groupKey + "." + rawDocID
		}

		cases = append(cases, documentedCase{
			GroupKey:        groupKey,
			DocID:           docID,
			TestName:        strings.TrimSpace(matches[2]),
			SectionTitle:    sectionTitle,
			SubsectionTitle: subTitle,
			DocLine:         idx + 1,
		})
	}

	return cases, nil
}

func cloneByGroup(cases []documentedCase) map[string][]documentedCase {
	grouped := make(map[string][]documentedCase)
	for _, tc := range cases {
		grouped[tc.GroupKey] = append(grouped[tc.GroupKey], tc)
	}

	return grouped
}

func takeCases(selected *[]documentedCase, grouped map[string][]documentedCase, key string, count int, category string) int {
	group := grouped[key]
	if len(group) == 0 {
		return count
	}

	if len(group) > count {
		group = group[:count]
	}

	for _, tc := range group {
		tc.Category = category
		*selected = append(*selected, tc)
	}

	grouped[key] = grouped[key][len(group):]
	return count - len(group)
}

func takeOneCase(selected *[]documentedCase, grouped map[string][]documentedCase, key string, category string) bool {
	if len(grouped[key]) == 0 {
		return false
	}

	tc := grouped[key][0]
	tc.Category = category
	*selected = append(*selected, tc)
	grouped[key] = grouped[key][1:]
	return true
}

func buildDefaultProfile() ([]documentedCase, error) {
	allCases, err := parseDocumentedCases()
	if err != nil {
		return nil, err
	}

	grouped := cloneByGroup(allCases)
	selected := make([]documentedCase, 0, defaultProfileCaseCount)

	for _, quota := range defaultInProcessQuotas {
		short := takeCases(&selected, grouped, quota.Key, quota.Count, "in-process")
		for short > 0 {
			// E3's summary count is one higher than the currently documented row
			// count. Roll the remaining slot into the next documented in-process
			// integration section instead of silently shrinking the 239-case budget.
			if !takeOneCase(&selected, grouped, "E24", "in-process") {
				return nil, fmt.Errorf("in-process shortfall for %s: missing %d cases", quota.Key, short)
			}
			short--
		}
	}

	for _, quota := range defaultEnvtestQuotas {
		short := takeCases(&selected, grouped, quota.Key, quota.Count, "envtest")
		if short != 0 {
			return nil, fmt.Errorf("envtest shortfall for %s: missing %d cases", quota.Key, short)
		}
	}

	const (
		defaultInProcessCount = 239
		defaultEnvtestCount   = 117
	)

	for len(selected) < defaultInProcessCount+defaultEnvtestCount {
		if takeOneCase(&selected, grouped, "E26", "envtest") {
			continue
		}
		if takeOneCase(&selected, grouped, "E32", "envtest") {
			continue
		}
		if takeOneCase(&selected, grouped, "E21", "envtest") {
			continue
		}

		return nil, fmt.Errorf("unable to fill envtest profile to %d cases", defaultEnvtestCount)
	}

	for _, quota := range defaultClusterQuotas {
		short := takeCases(&selected, grouped, quota.Key, quota.Count, "cluster")
		if short != 0 {
			return nil, fmt.Errorf("cluster shortfall for %s: missing %d cases", quota.Key, short)
		}
	}

	if len(selected) != defaultProfileCaseCount {
		return nil, fmt.Errorf("default profile expected %d cases, found %d", defaultProfileCaseCount, len(selected))
	}

	seen := make(map[string]struct{}, len(selected))
	for i := range selected {
		selected[i].Ordinal = i + 1
		traceKey := fmt.Sprintf("%s|%s|%s", selected[i].GroupKey, selected[i].DocID, selected[i].TestName)
		if _, exists := seen[traceKey]; exists {
			return nil, fmt.Errorf("duplicate trace key in default profile: %s", traceKey)
		}
		seen[traceKey] = struct{}{}
	}

	// Sub-AC 3.3: validate that all TC node labels ([TC-<DocID>]) are distinct.
	// The composite traceKey check above allows two cases with the same DocID
	// but different TestName to pass, which would produce colliding [TC-<ID>]
	// node labels and break per-spec focus filtering.
	if err := validateTCNodeLabelUniqueness(selected); err != nil {
		return nil, err
	}

	return selected, nil
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
