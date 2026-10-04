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
// The remaining E33 cases are implemented as real-backend Ginkgo specs
// in dedicated *_e2e_test.go files.  Of those, only 7 carry both the
// "default-profile" label AND the plain "e2e" build tag (no "e2e_helm"):
//
//	lvm_backend_standalone_e2e_test.go  → 7 (TC-E33.311–317, build: e2e)
//
// Note: lvm_pvc_pod_mount_e2e_test.go (12 specs) and
// lvm_volume_expansion_e2e_test.go (5 specs) have build tag "e2e && e2e_helm"
// and do NOT carry Label("default-profile") — they require a Helm-deployed
// pillar-csi agent and are excluded from the standard "make test-e2e" run.
//
// Additionally, the following default-profile specs from dedicated test files
// are included in the canonical total but are NOT managed by buildDefaultProfile:
//
//	teardown_panic_guarantee_test.go    → 4 (ac:3 teardown-guarantee)
//	backend_teardown_ac43_e2e_test.go   → 5 (ac:4.3 backend teardown absence)
//	tc_e34_local_attach_inprocess_test.go → 6 (E34 local attach, in-process)
//	tc_e35_iscsi_inprocess_test.go        → 12 (E35 iSCSI, in-process)
//
// Total default-profile spec count: 388+7+4+5+6+12 = 422.
// This 422 total is the canonical "실제 실행되는 테스트 케이스" count declared
// in docs/E2E-TESTCASES.md (257 in-process + 117 envtest + 48 cluster).
//
// Note: lvm_backend_core_rpcs_e2e_test.go (9 E33.1 specs) intentionally omits
// "default-profile" because those specs require a Helm-deployed agent pod that
// exceeds the 2-minute suiteLevelTimeout.
//
// Note: F27 (9), F28 (2), F29 (3), F30 (3), F31 (2) = 19 specs are NOT in
// the default-profile because they require host-level NVMe-oF initiator
// tooling (nvme_tcp module / nvme CLI) that is not available in the standard CI host environment.
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
	// Combined with 7 E33 standalone default-profile specs, 4 teardown-guarantee
	// specs, and 5 backend teardown-absence specs (in *_e2e_test.go files), the
	// total default-profile running TC count is 404 as declared in
	// docs/E2E-TESTCASES.md (239 in-process + 117 envtest + 48 cluster).
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
		// E33 and F27–F31 are NOT in the catalog-driven profile.
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
		// Rows in cluster / full-E2E sections use section-local numeric IDs,
		// optionally with a lowercase letter suffix (e.g. 285, 217a, 263k).
		// Prefix them with the group key so the TC node name starts with
		// [TC-E33.285] or [TC-E27.217a] — required by strict TC coverage.
		docID := rawDocID
		if isSectionLocalID(rawDocID) && groupKey != "" {
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

// TCExecutionProfile is the source-of-truth inventory used by strict TC
// coverage checks.  The default profile is generated from buildDefaultProfile;
// dedicated backend and NFS lanes append their explicitly registered IDs.
type TCExecutionProfile struct {
	Name string
	IDs  []string
}

var dedicatedDefaultProfileTCIDs = []string{
	"E33.311", "E33.312", "E33.313", "E33.314", "E33.315", "E33.316", "E33.317",
	"E34.1", "E34.2", "E34.3", "E34.4", "E34.5", "E34.6",
	"E35.1", "E35.2", "E35.3", "E35.4", "E35.5", "E35.6",
	"E35.7", "E35.8", "E35.9", "E35.10", "E35.11", "E35.12",
}

var nfsTCIDs = []string{
	"E37.1", "E37.2", "E37.3", "E37.4", "E37.5", "E37.6", "E37.7",
	"E37.8", "E37.9", "E37.10", "E37.11", "E37.12", "E37.13",
}

// CurrentTCExecutionProfiles returns the registered TC-ID profiles without
// executing any Ginkgo node or fixture.  Catalog-driven IDs come directly
// from buildDefaultProfile, so strict coverage and registration share the
// same selector rather than duplicating row quotas.
func CurrentTCExecutionProfiles() ([]TCExecutionProfile, error) {
	cases, err := buildDefaultProfile()
	if err != nil {
		return nil, err
	}
	defaultIDs := make([]string, 0, len(cases)+len(dedicatedDefaultProfileTCIDs))
	for _, tc := range cases {
		defaultIDs = append(defaultIDs, tc.DocID)
	}
	defaultIDs = append(defaultIDs, dedicatedDefaultProfileTCIDs...)
	return []TCExecutionProfile{
		{Name: "default-profile", IDs: defaultIDs},
		{Name: "nfs", IDs: append([]string(nil), nfsTCIDs...)},
	}, nil
}

// isSectionLocalID returns true when s is a non-empty numeric row ID, with an
// optional lowercase alphabetic suffix used by grouped catalog tables.
func isSectionLocalID(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	for i < len(s) {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
		i++
	}
	return true
}
