package e2e

// tc_failure_output.go — AC 8: Structured per-TC failure lines.
//
// Every spec that ends in a failed or panicked state emits a single log line
// of the form:
//
//	[TC-E1.2] [category:in-process] FAIL :: <assertion message>
//
// This guarantees that "grep TC-E1.2" on raw test output always locates the
// failure context, regardless of the Ginkgo verbosity level or reporter chosen
// by the caller.  The handler is registered as a ReportAfterEach hook so it
// fires even when Ginkgo runs in parallel (--procs), because ReportAfterEach
// executes in the worker process immediately after each spec completes.
//
// TC ID extraction order:
//  1. The "tc_id" ReportEntry added by default_profile_test.go's It body.
//  2. The LeafNodeText, which encodes the DocID inside the spec text produced
//     by catalog.go's specText() helper.
//  3. The ContainerHierarchyTexts, in case a spec nests the TC tag in a parent
//     container.
//
// Category extraction order:
//  1. The "tc_category" ReportEntry.
//  2. Any spec label that is a known category token.

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

// knownCategories is the exhaustive list of category tokens used by the
// default profile.  They are also the Ginkgo label values attached to each
// spec via Label("default-profile", tc.Category, tc.GroupKey).
var knownCategories = map[string]struct{}{
	"in-process": {},
	"envtest":    {},
	"cluster":    {},
	"full-lvm":   {},
}

// specTextTCIDPatterns lists, in priority order, the regular expressions used
// to extract a TC ID string from a Ginkgo spec node text.  Multiple patterns
// are supported so that the extractor stays correct regardless of whether the
// node name uses the legacy "TC[nnn/437] E1.2" format, the newer
// "[TC-E1.2]" format required by AC 7, or the purely numeric row-ID format
// used by some table sections (e.g. "[TC-100] TC[133/437] 100 ::").
var specTextTCIDPatterns = []*regexp.Regexp{
	// New format (AC 7), named IDs:  "[TC-E1.2]" or "[TC-F27.3]"
	regexp.MustCompile(`\[TC-([EF]\d+(?:\.\d+)?)\]`),
	// New format (AC 7), numeric IDs: "[TC-100]" or "[TC-437]"
	regexp.MustCompile(`\[TC-(\d+)\]`),
	// Legacy format, named IDs:  "TC[001/437] E1.2 ::" or "TC[001/437] E1.2"
	regexp.MustCompile(`TC\[\d+/\d+\]\s+([EF]\d+(?:\.\d+)?)`),
	// Legacy format, numeric IDs: "TC[133/437] 100 ::"
	regexp.MustCompile(`TC\[\d+/\d+\]\s+(\d+)\b`),
	// Bare named ID anywhere in the text, last resort: " E1.2 " or "E1.2::"
	regexp.MustCompile(`\b([EF]\d+\.\d+)\b`),
}

// extractTCIDFromText attempts to parse a TC ID from an arbitrary Ginkgo node
// text string. Returns "" when no ID can be identified.
func extractTCIDFromText(text string) string {
	for _, re := range specTextTCIDPatterns {
		if m := re.FindStringSubmatch(text); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// extractTCIDFromReport returns the best available TC ID for a spec report:
//
//  1. "tc_id" report entry (most authoritative – set before any assertion).
//  2. LeafNodeText (the It/Entry text produced by specText()).
//  3. ContainerHierarchyTexts (parent Describe/Context nodes).
func extractTCIDFromReport(report types.SpecReport) string {
	if id, ok := reportEntryValue(report.ReportEntries, "tc_id"); ok {
		return id
	}
	if id := extractTCIDFromText(report.LeafNodeText); id != "" {
		return id
	}
	for _, text := range report.ContainerHierarchyTexts {
		if id := extractTCIDFromText(text); id != "" {
			return id
		}
	}
	return ""
}

// extractCategoryFromReport returns the spec category for a report:
//
//  1. "tc_category" report entry.
//  2. First known-category label present in the spec's merged label set.
func extractCategoryFromReport(report types.SpecReport) string {
	if cat, ok := reportEntryValue(report.ReportEntries, "tc_category"); ok {
		return cat
	}
	for _, label := range report.Labels() {
		if _, known := knownCategories[label]; known {
			return label
		}
	}
	return ""
}

// formatFailurePrefix builds the grep-target prefix for a failure line:
//
//	[TC-E1.2] [category:in-process]
//
// When tcID is empty the prefix is omitted entirely (the hook is a no-op for
// non-TC framework specs such as BeforeSuite / AfterSuite).
func formatFailurePrefix(tcID, category string) string {
	if tcID == "" {
		return ""
	}
	if category == "" {
		return fmt.Sprintf("[TC-%s]", tcID)
	}
	return fmt.Sprintf("[TC-%s] [category:%s]", tcID, category)
}

// extractFailureMessage returns the primary human-readable failure text from a
// Ginkgo spec report, preferring the assertion message over forwarded panics.
func extractFailureMessage(report types.SpecReport) string {
	msg := strings.TrimSpace(report.Failure.Message)
	if msg != "" {
		// Collapse embedded newlines so that the entire context fits on one line
		// and "grep TC-<id>" returns a single, self-contained result.
		msg = strings.ReplaceAll(msg, "\n", " | ")
		return msg
	}
	if report.Failure.ForwardedPanic != "" {
		return "panic: " + strings.ReplaceAll(report.Failure.ForwardedPanic, "\n", " | ")
	}
	return report.State.String()
}

// failureLocationTag returns a compact "file:line" tag for the failure
// location, suitable for inclusion in the single-line output.
func failureLocationTag(report types.SpecReport) string {
	loc := report.Failure.Location
	if loc.FileName == "" {
		return ""
	}
	// Only include the base filename to keep the line short.
	parts := strings.Split(loc.FileName, "/")
	base := parts[len(parts)-1]
	if loc.LineNumber > 0 {
		return fmt.Sprintf("%s:%d", base, loc.LineNumber)
	}
	return base
}

// tcFailureOutputHook is the ReportAfterEach handler that emits structured
// failure lines.  It is registered as a package-level var so that the Ginkgo
// DSL picks it up when the test binary is loaded.
var _ = ReportAfterEach(func(report types.SpecReport) {
	// Only emit for terminal failure states.
	if report.State != types.SpecStateFailed &&
		report.State != types.SpecStatePanicked &&
		report.State != types.SpecStateTimedout {
		return
	}

	tcID := extractTCIDFromReport(report)
	category := extractCategoryFromReport(report)
	prefix := formatFailurePrefix(tcID, category)

	// Non-TC framework specs (e.g. BeforeSuite) have no TC ID. Do not emit a
	// misleading line for those; Ginkgo's own output is sufficient.
	if prefix == "" {
		return
	}

	failMsg := extractFailureMessage(report)
	locTag := failureLocationTag(report)

	var line string
	if locTag != "" {
		line = fmt.Sprintf("%s FAIL :: %s [at %s]\n", prefix, failMsg, locTag)
	} else {
		line = fmt.Sprintf("%s FAIL :: %s\n", prefix, failMsg)
	}

	// Write to stderr so the output is visible even when stdout is captured by
	// the Go test runner with -v off.  The line also appears in the test binary's
	// combined output, making "grep TC-<id>" effective on both streams.
	_, _ = fmt.Fprint(os.Stderr, line)
})
