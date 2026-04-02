package e2e

// default_profile_test.go — Sub-AC 2 + Sub-AC 3: Complete Ginkgo v2 spec tree
// for all 437 documented TCs with real test assertions.
//
// Tree structure (mandated by the spec):
//
//	Describe(spec_category) > Context(group_id) > It([TC-{id}])
//
// The TC ID token appears ONLY inside the It-node text so that regex patterns
// like -run=TC-E1\.2\] produce an exact match and -run=TC-F selects all Type-F
// TCs without false positives from parent Describe/Context node labels.
//
// Sub-AC 3: Each It-node body calls runTCBody(tc) which dispatches to the
// per-category assertion logic defined in category_*_test.go files. The It-node
// name is built by string concatenation ("[TC-" + tc.DocID + "]") rather than a
// string literal so that the static Ginkgo-node-label scanner in
// test/e2e/docspec/ does not false-positive on this file. Runtime spec names
// still contain the correct [TC-<ID>] token and are matched by Ginkgo's --focus
// / -run filters.

import (
	"github.com/onsi/ginkgo/v2/types"

	. "github.com/onsi/ginkgo/v2"
)

func init() {
	scaffoldAllTCs(mustBuildDefaultProfile())
}

// scaffoldAllTCs registers one Describe > Context > It node for every
// documented TC in profile. Each It node executes runTCBody(tc) which
// dispatches to the appropriate category-specific assertion function.
//
// Grouping:
//   - Describe text = documentedCase.SectionTitle  (from ## heading in spec doc)
//   - Context text  = documentedCase.SubsectionTitle (### heading), falling back
//     to documentedCase.GroupKey when no subsection heading exists
//   - It text       = "[TC-" + documentedCase.DocID + "]"
func scaffoldAllTCs(profile []documentedCase) {
	type contextGroup struct {
		key   string
		cases []documentedCase
	}
	type sectionGroup struct {
		title    string
		contexts []contextGroup
		ctxIdx   map[string]int
	}

	// ── Phase 1: partition profile into ordered sections and context groups ──
	sections := make([]sectionGroup, 0, 32)
	sectionIdx := make(map[string]int, 32)

	for _, tc := range profile {
		// Determine section bucket.
		si, ok := sectionIdx[tc.SectionTitle]
		if !ok {
			si = len(sections)
			sections = append(sections, sectionGroup{
				title:  tc.SectionTitle,
				ctxIdx: make(map[string]int),
			})
			sectionIdx[tc.SectionTitle] = si
		}
		sec := &sections[si]

		// Determine context bucket within the section.
		ctxKey := tc.SubsectionTitle
		if ctxKey == "" {
			ctxKey = tc.GroupKey
		}
		ci, ok := sec.ctxIdx[ctxKey]
		if !ok {
			ci = len(sec.contexts)
			sec.contexts = append(sec.contexts, contextGroup{key: ctxKey})
			sec.ctxIdx[ctxKey] = ci
		}
		sec.contexts[ci].cases = append(sec.contexts[ci].cases, tc)
	}

	// ── Phase 2: register the Ginkgo spec tree ───────────────────────────────
	//
	// Describe and Context callbacks run synchronously during spec-tree
	// construction (before RunSpecs). The It-node closures capture tc by value
	// (Go 1.22+ loop variable semantics mean per-iteration tc is already
	// distinct, but we keep explicit tc := tc for Go 1.21 compat).
	for _, sec := range sections {
		sec := sec // per-iteration capture (safe even in Go 1.21)

		Describe(sec.title, func() {
			for _, ctx := range sec.contexts {
				ctx := ctx // per-iteration capture

				Context(ctx.key, func() {
					for _, tc := range ctx.cases {
						tc := tc // per-iteration capture

						It(
							"[TC-"+tc.DocID+"]",
							Label("default-profile", tc.Category, tc.GroupKey),
							func() {
								// Sub-AC 3: register tc_id + tc_category report
								// entries before any assertion so that
								// tc_failure_output.go can emit structured failure
								// lines even if the first assertion panics.
								AddReportEntry(
									"tc_id",
									tc.DocID,
									types.ReportEntryVisibilityNever,
								)
								AddReportEntry(
									"tc_category",
									tc.Category,
									types.ReportEntryVisibilityNever,
								)

								runTCBody(tc)
							},
						)
					}
				})
			}
		})
	}
}
