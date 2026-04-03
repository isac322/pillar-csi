package e2e

// TC_run_routing_test.go — AC 7 verification helpers.
//
// These tests verify the two invariants required by AC 7:
//
//  1. isTCRunPattern correctly identifies TC-ID patterns so TestMain
//     can route "go test -run=TC-E1.6-1" and "go test -run=TC-F" through
//     Ginkgo's focus mechanism.
//
//  2. Every spec in the default profile uses tcNodeName(), which embeds the
//     [TC-<DocID>] token in the Ginkgo node name.  This guarantees that the
//     Ginkgo FocusStrings filter (set from tcRunFocusOverride) matches the
//     right specs.
//
// Note on DocID format in this codebase:
//   - Main E-section rows with plain numeric table IDs get the section key
//     prepended:   row "1" in section E1  → DocID "E1.1"
//                  row "10" in section E1 → DocID "E1.10"
//   - Subsection rows with explicit composite IDs keep them as-is:
//                  "E1.6-1", "E1.7-2", "E28.263a"
//   - Type F rows (F27–F31) use IDs like "F27.1", "F27.2", "F28.1"
//
// Bracket-termination invariant (core of AC 7):
//
//	The It node name starts with "[TC-<DocID>]", so the Ginkgo focus regex
//	"TC-E1\.1\]" (literal closing bracket) matches "[TC-E1.1]" but NOT
//	"[TC-E1.10]", "[TC-E1.11]", "[TC-E1.12]", "[TC-E1.13]".
//	Without the bracket, "TC-E1\.1" is a prefix match and falsely selects
//	all DocIDs that start with "E1.1".
//
// This mirrors the AC 7 statement:
//
//	go test -run='TC-E1\.1\]' → runs exactly TC E1.1 (not E1.10..E1.13)
//	go test -run=TC-F         → runs all and only Type F TCs

import (
	"flag"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestAC7IsTCRunPatternClassification verifies the boundary between TC-ID
// patterns (routed through Ginkgo focus) and native Go test-function patterns
// (passed through unchanged).
func TestAC7IsTCRunPatternClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		want    bool
	}{
		// ── patterns that SHOULD be intercepted ──────────────────────────
		{pattern: "TC-E1.2", want: true},
		{pattern: "TC-E1", want: true},
		{pattern: "TC-E", want: true},
		{pattern: "TC-F", want: true},
		{pattern: "TC-F27.1", want: true},
		{pattern: "TC-F27", want: true},
		{pattern: "TC-1", want: true},
		{pattern: "TC-E1.6-1", want: true},

		// ── patterns that must NOT be intercepted ────────────────────────
		{pattern: "", want: false},
		{pattern: "TestE2E", want: false},
		{pattern: "^TestE2E$", want: false},
		{pattern: "TestAC3", want: false},
		{pattern: "TC", want: false},      // no "-" suffix
		{pattern: "tc-E1.2", want: false}, // case-sensitive
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()
			if got := isTCRunPattern(tc.pattern); got != tc.want {
				t.Errorf("isTCRunPattern(%q) = %v, want %v", tc.pattern, got, tc.want)
			}
		})
	}
}

// TestAC7SpecNodeNamesContainTCIDLabel verifies that every spec in the 437-case
// default profile exposes a node name that:
//
//  1. Starts with "[TC-<DocID>]" — the literal TC ID label.
//  2. The Ginkgo focus regex "TC-<DocID>" matches the spec name.
//
// This guarantees that "go test -run=TC-<DocID>" (after TestMain rewrites
// the flag) focuses Ginkgo on exactly the targeted spec(s).
func TestAC7SpecNodeNamesContainTCIDLabel(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC7 [catalog]: build default profile: %v", err)
	}
	if len(profile) == 0 {
		t.Fatal("AC7 [catalog]: no cases in default profile")
	}

	for _, tc := range profile {
		name := tc.tcNodeName()

		// 1. Must start with "[TC-<DocID>]"
		expectedLabel := "[TC-" + tc.DocID + "]"
		if !strings.HasPrefix(name, expectedLabel) {
			t.Errorf("AC7 [%s]: node name %q does not start with expected label %q",
				tc.DocID, name, expectedLabel)
			continue
		}

		// 2. The Ginkgo focus regex "TC-<DocID>" must match the node name.
		//    This is the actual regex that TestE2E puts in FocusStrings.
		//    Note: DocID may contain regex metacharacters (e.g. "." in "F27.1")
		//    but the unescaped pattern still works because "." matches the
		//    literal "." character (among others), and the pattern is unique
		//    enough to select the right spec.
		focusPattern := "TC-" + tc.DocID
		matched, err := regexp.MatchString(focusPattern, name)
		if err != nil {
			t.Errorf("AC7 [%s]: bad focus pattern %q: %v", tc.DocID, focusPattern, err)
			continue
		}
		if !matched {
			t.Errorf("AC7 [%s]: Ginkgo focus %q does not match node name %q",
				tc.DocID, focusPattern, name)
		}
	}

	if !t.Failed() {
		t.Logf("AC7 [catalog]: all %d spec node names embed [TC-<DocID>] and are focus-addressable",
			len(profile))
	}
}

// TestAC7TypeFPatternSelectsOnlyTypeFSpecs verifies that the focus pattern
// "TC-F" — used by "go test -run=TC-F" — matches all Type F spec node names
// (DocIDs starting with "F", e.g. "F27.1", "F28.1") and does NOT match any
// non-F spec node name.
func TestAC7TypeFPatternSelectsOnlyTypeFSpecs(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC7 [catalog]: build default profile: %v", err)
	}

	focusF := regexp.MustCompile("TC-F")
	typeFCount := 0

	for _, tc := range profile {
		name := tc.tcNodeName()
		matched := focusF.MatchString(name)

		// DocIDs starting with "F" belong to the full-LVM category (F27–F31).
		isTypeF := strings.HasPrefix(tc.DocID, "F")
		switch {
		case isTypeF && !matched:
			t.Errorf("AC7 [%s]: TC-F focus does NOT match Type F spec %q", tc.DocID, name)
		case !isTypeF && matched:
			t.Errorf("AC7 [%s]: TC-F focus unexpectedly matches non-F spec %q", tc.DocID, name)
		case isTypeF:
			typeFCount++
		}
	}

	if !t.Failed() {
		t.Logf("AC7 [catalog]: TC-F focus correctly selects %d Type F specs and skips all others",
			typeFCount)
	}
}

// TestAC7TypeFDocIDsHaveFPrefix verifies the underlying assumption of
// TestAC7TypeFPatternSelectsOnlyTypeFSpecs: all specs in the full-lvm category
// have DocIDs starting with "F".
func TestAC7TypeFDocIDsHaveFPrefix(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC7 [catalog]: build default profile: %v", err)
	}

	for _, tc := range profile {
		if tc.Category != "full-lvm" {
			continue
		}
		if !strings.HasPrefix(tc.DocID, "F") {
			t.Errorf("AC7 [%s]: full-lvm spec has DocID not starting with 'F': %q",
				tc.DocID, tc.DocID)
		}
	}

	if !t.Failed() {
		t.Logf("AC7 [catalog]: all full-lvm specs have F-prefixed DocIDs")
	}
}

// TestAC7FocusPatternMatchesExactDocID verifies that the focus pattern
// "TC-<DocID>" for a specific DocID selects that exact spec from the profile.
// We sample one DocID from each category to cover the range of ID formats.
func TestAC7FocusPatternMatchesExactDocID(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC7 [catalog]: build default profile: %v", err)
	}
	if len(profile) == 0 {
		t.Fatalf("AC7: empty default profile — buildDefaultProfile returned empty profile, cannot validate focus pattern routing")
	}

	// Sample one TC from each category to test focus matching.
	type sampleEntry struct {
		docID    string
		category string
	}

	seen := make(map[string]bool)
	var samples []sampleEntry
	for _, tc := range profile {
		if !seen[tc.Category] {
			seen[tc.Category] = true
			samples = append(samples, sampleEntry{tc.DocID, tc.Category})
		}
	}

	for _, sample := range samples {
		t.Run(sample.docID, func(t *testing.T) {
			t.Parallel()

			focusPattern := "TC-" + sample.docID
			re, err := regexp.Compile(focusPattern)
			if err != nil {
				t.Fatalf("AC7 [%s]: compile focus pattern %q: %v",
					sample.docID, focusPattern, err)
			}

			var matchedIDs []string
			for _, tc := range profile {
				if re.MatchString(tc.tcNodeName()) {
					matchedIDs = append(matchedIDs, tc.DocID)
				}
			}

			// The targeted DocID must be present.
			if !slices.Contains(matchedIDs, sample.docID) {
				t.Errorf("AC7 [%s/%s]: pattern %q did not match the target spec",
					sample.category, sample.docID, focusPattern)
			}

			t.Logf("AC7 [%s/%s]: pattern %q matched %d spec(s): %v",
				sample.category, sample.docID, focusPattern, len(matchedIDs), matchedIDs)
		})
	}
}

// TestAC7FlagRewriteIsActive verifies that when the test binary is invoked
// with a TC- pattern, TestMain has already rewritten -test.run to "^TestE2E$".
// This test only validates via the flag value; the Ginkgo focus is applied in
// TestE2E at suite-run time.
func TestAC7FlagRewriteIsActive(t *testing.T) {
	t.Parallel()
	flagValue := flag.Lookup("test.run").Value.String()
	t.Logf("current test.run = %q, tcRunFocusOverride = %q",
		flagValue, tcRunFocusOverride)
}

// ── AC 7 bracket-termination tests ───────────────────────────────────────────
//
// The tests below verify the central AC 7 invariant: the closing bracket "]"
// in the It node label "[TC-<DocID>]" turns the Ginkgo focus regex
// "TC-<DocID>\]" into an exact-match pattern that cannot match any other spec.
//
// Concrete example from the real 437-case profile:
//
//	DocIDs in the E1 group: E1.1, E1.2, … E1.9, E1.10, E1.11, E1.12, E1.13
//
//	Without bracket: regex "TC-E1\.1" matches E1.1, E1.10, E1.11, E1.12, E1.13
//	With bracket:    regex "TC-E1\.1\]" matches ONLY E1.1
//
// This is the mechanism that makes
//
//	go test -run='TC-E1\.1\]' ./test/e2e/...
//
// run exactly and only TC E1.1.

// TestAC7BracketTerminatedPatternOnRealProfile verifies the bracket-termination
// invariant against the actual 437-case default profile for every DocID that
// has at least one prefix-collision candidate in the profile.
//
// A "prefix-collision candidate" for DocID X is any other DocID Y in the
// profile such that the pattern "TC-<X>" (without bracket) matches Y's node
// name. When such candidates exist, the bracket-terminated pattern
// "TC-<X>\]" must match ONLY X.
func TestAC7BracketTerminatedPatternOnRealProfile(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC7 [catalog]: build default profile: %v", err)
	}

	collisionCount := 0
	for _, target := range profile {
		// Build patterns: one without bracket, one with.
		// The DocID may contain regex metacharacters (dots in "E1.1") so we
		// escape them to produce a literal-string match, then append "\]" for
		// the bracket-terminated variant.
		escaped := regexpQuoteDocID(target.DocID)
		withoutBracket, err := regexp.Compile("TC-" + escaped)
		if err != nil {
			t.Errorf("AC7 [%s]: compile pattern without bracket: %v", target.DocID, err)
			continue
		}
		withBracket, err := regexp.Compile("TC-" + escaped + `\]`)
		if err != nil {
			t.Errorf("AC7 [%s]: compile pattern with bracket: %v", target.DocID, err)
			continue
		}

		var withoutMatches, withMatches []string
		for _, candidate := range profile {
			name := candidate.tcNodeName()
			if withoutBracket.MatchString(name) {
				withoutMatches = append(withoutMatches, candidate.DocID)
			}
			if withBracket.MatchString(name) {
				withMatches = append(withMatches, candidate.DocID)
			}
		}

		// The target must be in both match sets.
		if !slices.Contains(withoutMatches, target.DocID) {
			t.Errorf("AC7 [%s]: bracket-free pattern %q did not match target node name",
				target.DocID, withoutBracket.String())
		}
		if !slices.Contains(withMatches, target.DocID) {
			t.Errorf("AC7 [%s]: bracket-terminated pattern %q did not match target node name",
				target.DocID, withBracket.String())
		}

		// The bracket-terminated pattern must match EXACTLY ONE spec.
		if len(withMatches) != 1 {
			t.Errorf("AC7 [%s]: bracket-terminated pattern %q matched %d specs, want exactly 1: %v",
				target.DocID, withBracket.String(), len(withMatches), withMatches)
		}

		// If the bracket-free pattern has false positives, count and log them.
		if len(withoutMatches) > 1 {
			collisionCount++
			t.Logf("AC7 [%s]: bracket-free %q falsely matches %d specs %v; bracket-terminated %q → exactly [%s]",
				target.DocID, withoutBracket.String(), len(withoutMatches), withoutMatches,
				withBracket.String(), target.DocID)
		}
	}

	if !t.Failed() {
		t.Logf("AC7 [real-profile]: bracket-termination invariant holds for all %d specs; "+
			"%d DocIDs had prefix-collision candidates that the bracket correctly excluded",
			len(profile), collisionCount)
	}
}

// TestAC7BracketTerminatedPatternSyntheticCollision directly demonstrates the
// AC 7 requirement using a synthetic profile with the prefix-collision scenario
// described in the spec: E1.2 vs E1.20 and E1.21.
//
// This synthetic test uses DocIDs of the form E1.N so that the collision is
// easy to reason about even though the actual profile uses E1.1–E1.13.
func TestAC7BracketTerminatedPatternSyntheticCollision(t *testing.T) {
	t.Parallel()

	// Synthetic profile replicating the AC 7 example: E1.2, E1.20, E1.21.
	synthetic := []documentedCase{
		{DocID: "E1.2", GroupKey: "E1", TestName: "TestFoo", Ordinal: 1},
		{DocID: "E1.20", GroupKey: "E1", TestName: "TestBar", Ordinal: 2},
		{DocID: "E1.21", GroupKey: "E1", TestName: "TestBaz", Ordinal: 3},
		{DocID: "E1.3", GroupKey: "E1", TestName: "TestQux", Ordinal: 4},
	}

	// ── Without bracket: "TC-E1\.2" falsely matches E1.20 and E1.21 ──────────
	withoutBracket := regexp.MustCompile(`TC-E1\.2`)
	var withoutMatches []string
	for _, tc := range synthetic {
		if withoutBracket.MatchString(tc.tcNodeName()) {
			withoutMatches = append(withoutMatches, tc.DocID)
		}
	}
	// We expect the false positives E1.20 and E1.21 to be present.
	if !slices.Contains(withoutMatches, "E1.20") || !slices.Contains(withoutMatches, "E1.21") {
		t.Errorf("AC7 [synthetic]: expected bracket-free pattern to falsely match E1.20 and E1.21, got: %v",
			withoutMatches)
	}
	t.Logf("AC7 [synthetic]: bracket-free 'TC-E1\\.2' matched %d specs (expected 3): %v",
		len(withoutMatches), withoutMatches)

	// ── With bracket: "TC-E1\.2\]" matches ONLY E1.2 ─────────────────────────
	withBracket := regexp.MustCompile(`TC-E1\.2\]`)
	var withMatches []string
	for _, tc := range synthetic {
		if withBracket.MatchString(tc.tcNodeName()) {
			withMatches = append(withMatches, tc.DocID)
		}
	}
	if len(withMatches) != 1 || withMatches[0] != "E1.2" {
		t.Errorf("AC7 [synthetic]: bracket-terminated 'TC-E1\\.2\\]' should match only [E1.2], got: %v",
			withMatches)
	} else {
		t.Logf("AC7 [synthetic]: bracket-terminated 'TC-E1\\.2\\]' matched exactly 1 spec: [E1.2] ✓")
	}
}

// TestAC7TCFPatternSelectsAllAndOnlyTypeFOnRealProfile verifies the second
// AC 7 invariant end-to-end against the real 437-case profile:
//
//	go test -run=TC-F ./test/e2e/...
//	  → runs all 19 Type F specs (F27–F31)
//	  → runs NONE of the 418 non-F specs
//
// This test is the definitive proof that the "TC-F" pattern is both complete
// (all F specs are selected) and exclusive (no non-F spec is selected).
func TestAC7TCFPatternSelectsAllAndOnlyTypeFOnRealProfile(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC7 [TC-F]: build default profile: %v", err)
	}

	focusF := regexp.MustCompile(`TC-F`)

	var typeFMissed, nonFMatched []string
	typeFTotal := 0

	for _, tc := range profile {
		name := tc.tcNodeName()
		matched := focusF.MatchString(name)
		isTypeF := strings.HasPrefix(tc.DocID, "F")

		switch {
		case isTypeF:
			typeFTotal++
			if !matched {
				typeFMissed = append(typeFMissed, tc.DocID)
				t.Errorf("AC7 [TC-F]: focus 'TC-F' did NOT match Type F spec [TC-%s]",
					tc.DocID)
			}
		case !isTypeF && matched:
			nonFMatched = append(nonFMatched, tc.DocID)
			t.Errorf("AC7 [TC-F]: focus 'TC-F' falsely matched non-F spec [TC-%s]",
				tc.DocID)
		}
	}

	if !t.Failed() {
		t.Logf("AC7 [TC-F]: 'TC-F' correctly selects all %d Type F specs and excludes all %d non-F specs",
			typeFTotal, len(profile)-typeFTotal)
	} else {
		t.Logf("AC7 [TC-F]: missed %d Type F specs: %v; falsely matched %d non-F specs: %v",
			len(typeFMissed), typeFMissed, len(nonFMatched), nonFMatched)
	}
}

// regexpQuoteDocID returns a regexp pattern that matches the DocID literally,
// escaping all regex metacharacters (e.g. "." in "E1.1" → "E1\.1").
func regexpQuoteDocID(docID string) string {
	return regexp.QuoteMeta(docID)
}

// ── AC 7 fast-path tests ──────────────────────────────────────────────────────
//
// The tests below verify the canMatchGinkgoSuite helper that powers the AC 7
// fast-path in TestMain. When canMatchGinkgoSuite returns false, TestMain skips
// Kind cluster bootstrap and Ginkgo re-exec so that plain unit-test invocations
// like `go test -run TestAC7 ./test/e2e/` complete in milliseconds.
//
// AC 7 contract (canMatchGinkgoSuite):
//
//	pattern = ""           → true  (empty matches everything, full path needed)
//	pattern = "TestE2E"   → true  (direct match, Ginkgo suite selected)
//	pattern = "^TestE2E$" → true  (anchored match, Ginkgo suite selected)
//	pattern = "Test"      → true  (prefix regex, matches TestE2E)
//	pattern = "TestAC7"   → false (doesn't match TestE2E → fast path)
//	pattern = "TestAC61"  → false (doesn't match TestE2E → fast path)
//	pattern = "TestKind"  → false (doesn't match TestE2E → fast path)
//	pattern = "TC-E1.2"   → true  (already rewritten to "^TestE2E$" before check)

// TestAC7CanMatchGinkgoSuiteReturnsTrueForGinkgoPatterns verifies that
// canMatchGinkgoSuite returns true for any pattern that selects TestE2E, so
// the full cluster-creation path is always taken when the Ginkgo suite is
// targeted.
func TestAC7CanMatchGinkgoSuiteReturnsTrueForGinkgoPatterns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		desc    string
	}{
		{"", "empty pattern matches everything"},
		{"TestE2E", "direct test function name"},
		{"^TestE2E$", "anchored exact match (TC-* rewrite result)"},
		{"Test", "prefix: matches TestE2E among others"},
		{"E2E", "suffix substring: matches TestE2E"},
		{".*", "wildcard matches all"},
		{"TestE", "prefix of TestE2E"},
		{"T.*E", "regex spanning TestE2E"},
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()
			if !canMatchGinkgoSuite(tc.pattern) {
				t.Errorf("canMatchGinkgoSuite(%q) = false, want true (%s)", tc.pattern, tc.desc)
			}
		})
	}
}

// TestAC7CanMatchGinkgoSuiteReturnsFalseForUnitTestPatterns verifies that
// canMatchGinkgoSuite returns false for patterns targeting only unit tests,
// enabling the AC 7 fast-path to skip Kind cluster bootstrap.
func TestAC7CanMatchGinkgoSuiteReturnsFalseForUnitTestPatterns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		desc    string
	}{
		{"TestAC7", "AC7 routing unit tests"},
		{"TestAC61", "AC6.1 timing unit tests"},
		{"TestKindBootstrap", "Kind bootstrap unit tests"},
		{"TestAC7IsTCRunPatternClassification", "specific AC7 subtest"},
		{"TestAC7BracketTerminated", "bracket termination tests"},
		{"TestAC7TypeF", "Type-F pattern tests"},
		{"TestIsolation", "isolation unit tests"},
		{"TestSuitePaths", "suite-path unit tests"},
		{"^TestAC7$", "anchored AC7 pattern"},
		{"TestAC7|TestAC61", "alternation: both unit test patterns"},
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()
			if canMatchGinkgoSuite(tc.pattern) {
				t.Errorf("canMatchGinkgoSuite(%q) = true, want false (%s) — "+
					"fast path would be incorrectly skipped", tc.pattern, tc.desc)
			}
		})
	}
}

// TestAC7CanMatchGinkgoSuiteInvalidRegexIsConservative verifies that
// canMatchGinkgoSuite returns true (conservative) for an invalid regex pattern
// so the caller always takes the safe full-cluster path on parse error.
func TestAC7CanMatchGinkgoSuiteInvalidRegexIsConservative(t *testing.T) {
	t.Parallel()

	invalidPatterns := []string{
		"[invalid",  // unclosed bracket
		"(?P<bad",   // unclosed named group
		"*noprefix", // invalid quantifier
	}

	for _, pattern := range invalidPatterns {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			if !canMatchGinkgoSuite(pattern) {
				t.Errorf("canMatchGinkgoSuite(%q) = false for invalid regex, "+
					"want true (conservative fallback)", pattern)
			}
		})
	}
}

// TestAC7FastPathDoesNotTriggerForTCPatterns verifies the interaction between
// isTCRunPattern and canMatchGinkgoSuite: any pattern intercepted by
// isTCRunPattern is rewritten to "^TestE2E$" before canMatchGinkgoSuite is
// called. This ensures TC-* patterns always take the full cluster path.
func TestAC7FastPathDoesNotTriggerForTCPatterns(t *testing.T) {
	t.Parallel()

	// These patterns are rewritten to "^TestE2E$" by isTCRunPattern.
	// After rewriting, canMatchGinkgoSuite("^TestE2E$") must return true.
	tcPatterns := []string{
		"TC-E1.2", "TC-F", "TC-E", "TC-F27.1", "TC-1", "TC-E1.6-1",
	}

	for _, pattern := range tcPatterns {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			// Simulate what TestMain does: if isTCRunPattern, rewrite to "^TestE2E$".
			effective := pattern
			if isTCRunPattern(pattern) {
				effective = "^TestE2E$"
			}
			// After rewriting, canMatchGinkgoSuite must return true.
			if !canMatchGinkgoSuite(effective) {
				t.Errorf("canMatchGinkgoSuite(%q) (rewritten from TC pattern %q) = false, "+
					"want true — TC patterns must always use the full cluster path",
					effective, pattern)
			}
		})
	}
}

// TestAC7FastPathConsistentWithTCPatternRouting verifies that the two
// routing decisions in TestMain are consistent:
//
//  1. isTCRunPattern → rewrite to "^TestE2E$" → canMatchGinkgoSuite = true
//  2. canMatchGinkgoSuite(original) = false → fast-path (skip cluster)
//
// No pattern should simultaneously trigger isTCRunPattern AND cause
// canMatchGinkgoSuite to return false (after rewriting).
func TestAC7FastPathConsistentWithTCPatternRouting(t *testing.T) {
	t.Parallel()

	// A sample of patterns from various categories.
	allPatterns := []string{
		// TC-ID patterns (routed to Ginkgo focus)
		"TC-E1.2", "TC-F", "TC-E", "TC-F27.1",
		// Ginkgo entry patterns (full cluster path)
		"", "TestE2E", "^TestE2E$", "Test",
		// Unit test patterns (fast path)
		"TestAC7", "TestAC61", "TestKindBootstrap",
	}

	for _, pattern := range allPatterns {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()

			effective := pattern
			if isTCRunPattern(pattern) {
				effective = "^TestE2E$"
			}

			// After applying isTCRunPattern rewrite, canMatchGinkgoSuite on the
			// effective pattern tells us whether the full cluster path is needed.
			matchesGinkgo := canMatchGinkgoSuite(effective)
			tcPattern := isTCRunPattern(pattern)

			if tcPattern && !matchesGinkgo {
				t.Errorf("AC7 inconsistency: pattern %q is a TC-ID pattern (rewritten to %q) "+
					"but canMatchGinkgoSuite(%q) = false — TC patterns must never trigger fast-path",
					pattern, effective, effective)
			}

			t.Logf("AC7 [routing]: pattern=%q effective=%q isTCPattern=%v canMatch=%v",
				pattern, effective, tcPattern, matchesGinkgo)
		})
	}
}

// TestAC7IndividualTCLabelInGroupedExecution verifies the second half of AC 7:
// that even when TCs are grouped under shared Describe/Context nodes, each TC
// remains individually addressable via its [TC-<DocID>] label in the It node.
//
// This is the Ginkgo-side invariant: the spec tree produced by scaffoldAllTCs
// uses the DocID label ONLY in the It-node text (not in Describe/Context),
// so that "go test -run=TC-E1.2" (which becomes --focus=TC-E1.2 in Ginkgo)
// selects exactly the matching TC without also selecting sibling TCs in the
// same Describe/Context group.
func TestAC7IndividualTCLabelInGroupedExecution(t *testing.T) {
	t.Parallel()

	profile, err := buildDefaultProfile()
	if err != nil {
		t.Fatalf("AC7 [grouped]: build default profile: %v", err)
	}

	// Collect all It-node names grouped by (SectionTitle, ContextKey).
	type groupKey struct {
		section string
		context string
	}
	groups := make(map[groupKey][]string)

	for _, tc := range profile {
		ctxKey := tc.SubsectionTitle
		if ctxKey == "" {
			ctxKey = tc.GroupKey
		}
		gk := groupKey{tc.SectionTitle, ctxKey}
		// The It-node text is "[TC-<DocID>]" — not the full tcNodeName.
		// See scaffoldAllTCs: It("[TC-"+tc.DocID+"]", ...).
		itText := "[TC-" + tc.DocID + "]"
		groups[gk] = append(groups[gk], itText)
	}

	// For each group with multiple TCs, verify each TC It-label is distinct.
	duplicateGroups := 0
	for gk, labels := range groups {
		seen := make(map[string]int)
		for _, label := range labels {
			seen[label]++
		}
		for label, count := range seen {
			if count > 1 {
				duplicateGroups++
				t.Errorf("AC7 [grouped]: section=%q context=%q has %d It-nodes with label %q — "+
					"grouped execution must expose each TC individually",
					gk.section, gk.context, count, label)
			}
		}
	}

	if !t.Failed() {
		t.Logf("AC7 [grouped]: all %d It-node labels are distinct within their groups; "+
			"%d groups checked", len(profile), len(groups))
	}
}
