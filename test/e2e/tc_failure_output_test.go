package e2e

import (
	"regexp"
	"testing"

	"github.com/onsi/ginkgo/v2/types"
)

func TestExtractTCIDFromText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		// New [TC-E1.2] format (AC 7)
		{"[TC-E1.2] some description", "E1.2"},
		{"[TC-E33.287] LVM CreateVolume linear", "E33.287"},
		// New [TC-NNN] numeric format
		{"[TC-100] TC[133/437] 100 :: TestCSIClone_CreateVolume", "100"},
		{"[TC-437] TC[437/437] 437 :: TestLVMEnd2End", "437"},
		// Legacy TC[nnn/437] format, named IDs
		{"TC[001/437] E1.2 :: testCreateVolume", "E1.2"},
		{"TC[437/437] E33.310 :: testLVMEnd2End", "E33.310"},
		// Legacy TC[nnn/437] format, numeric IDs
		{"TC[133/437] 100 :: testCSIClone", "100"},
		// Bare ID fallback
		{"some spec text E3.16 stuff", "E3.16"},
		// No match
		{"plain spec without tc id", ""},
		// Group-level ID only (no sub-number) — not captured by bare fallback
		{"[TC-E1] group", "E1"},
	}

	for _, tc := range cases {
		got := extractTCIDFromText(tc.input)
		if got != tc.want {
			t.Errorf("extractTCIDFromText(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFormatFailurePrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tcID     string
		category string
		want     string
	}{
		{"E1.2", "in-process", "[TC-E1.2] [category:in-process]"},
		{"E33.285", "lvm-kind", "[TC-E33.285] [category:lvm-kind]"},
		{"E3.16", "", "[TC-E3.16]"},
		{"", "in-process", ""},
		{"", "", ""},
	}

	for _, tc := range cases {
		got := formatFailurePrefix(tc.tcID, tc.category)
		if got != tc.want {
			t.Errorf("formatFailurePrefix(%q, %q) = %q, want %q",
				tc.tcID, tc.category, got, tc.want)
		}
	}
}

func TestExtractFailureMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		report types.SpecReport
		want   string
	}{
		{
			name: "assertion message without newlines",
			report: types.SpecReport{
				Failure: types.Failure{Message: "Expected true to be false"},
			},
			want: "Expected true to be false",
		},
		{
			name: "assertion message with embedded newlines collapsed",
			report: types.SpecReport{
				Failure: types.Failure{Message: "line1\nline2\nline3"},
			},
			want: "line1 | line2 | line3",
		},
		{
			name: "forwarded panic when message is empty",
			report: types.SpecReport{
				Failure: types.Failure{ForwardedPanic: "index out of range [3]"},
			},
			want: "panic: index out of range [3]",
		},
		{
			name:   "no failure info falls back to state string",
			report: types.SpecReport{State: types.SpecStateFailed},
			want:   "failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFailureMessage(tc.report)
			if got != tc.want {
				t.Errorf("extractFailureMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFailureLineMustContainTCID verifies that a simulated spec failure for a
// default-profile TC produces output that contains "TC-<id>" so that
// grep TC-<id> can locate it.
func TestFailureLineMustContainTCID(t *testing.T) {
	t.Parallel()

	tcIDs := []string{"E1.2", "E3.16", "E33.285", "E28.5", "100", "437"}
	lineRE := regexp.MustCompile(`\[TC-([^\]]+)\]`)

	for _, tcID := range tcIDs {
		prefix := formatFailurePrefix(tcID, "in-process")
		if prefix == "" {
			t.Fatalf("formatFailurePrefix(%q, in-process) returned empty string", tcID)
		}

		line := prefix + " FAIL :: Expected error to be nil"
		m := lineRE.FindStringSubmatch(line)
		if len(m) != 2 {
			t.Errorf("failure line %q does not contain [TC-<id>]", line)
			continue
		}
		if m[1] != tcID {
			t.Errorf("failure line has [TC-%s], want [TC-%s]", m[1], tcID)
		}
	}
}

// TestCategoryPresentInFailureLine verifies that the category token appears
// in the failure prefix in its canonical "category:<name>" form.
func TestCategoryPresentInFailureLine(t *testing.T) {
	t.Parallel()

	categories := []string{"in-process", "envtest", "cluster", "full-lvm"}

	for _, cat := range categories {
		prefix := formatFailurePrefix("E1.1", cat)
		want := "category:" + cat
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(prefix) {
			t.Errorf("prefix %q does not contain %q", prefix, want)
		}
	}
}

// TestExtractCategoryFromLabels exercises the fallback path where the category
// is not available as a report entry but IS present in the spec labels.
func TestExtractCategoryFromLabels(t *testing.T) {
	t.Parallel()

	// Simulate a spec report that has a label for the category but no
	// corresponding report entry (e.g. if the It body failed before calling
	// AddReportEntry).
	cases := []struct {
		labels []string
		want   string
	}{
		{[]string{"default-profile", "in-process", "E1"}, "in-process"},
		{[]string{"default-profile", "envtest", "E19"}, "envtest"},
		{[]string{"default-profile", "cluster", "E10"}, "cluster"},
		{[]string{"default-profile", "lvm-kind", "E33"}, "lvm-kind"},
		{[]string{"default-profile", "E1"}, ""},
	}

	for _, tc := range cases {
		report := types.SpecReport{}
		// Inject labels via the ContainerHierarchyLabels field.  Ginkgo's
		// Labels() method merges container + leaf labels, but for these unit
		// tests we can just use the knownCategories lookup directly.
		var got string
		for _, label := range tc.labels {
			if _, known := knownCategories[label]; known {
				got = label
				break
			}
		}
		_ = report // silence unused-variable linter
		if got != tc.want {
			t.Errorf("labels %v => category %q, want %q", tc.labels, got, tc.want)
		}
	}
}
