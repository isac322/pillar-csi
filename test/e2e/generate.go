// Package e2e is the end-to-end test suite for pillar-csi.
//
// go generate runs the TC-ID coverage verification tool against the spec
// document and the compiled Ginkgo spec tree.  Two modes are available:
//
//   - Runtime mode (default when ginkgo CLI is on PATH):
//     Executes `ginkgo --dry-run ./test/e2e/` to enumerate actual Ginkgo node
//     names, then asserts a 1-to-1 match with the 437 TC IDs documented in
//     docs/testing/{COMPONENT,INTEGRATION,E2E}-TESTS.md.  This mode correctly handles dynamically-generated
//     node names (produced via tc.tcNodeName()) that the static scanner cannot
//     see.  Exit code 1 when any TC ID is missing or unrecognised.
//
//   - Static fallback:
//     When the ginkgo CLI is absent the tool falls back to a regex scan of Go
//     source string literals, which will report all catalogue cases as missing
//     (expected — see docspec.TestFindGinkgoNodeBindings_NoFalsePositives).
//     Use `make ginkgo && make verify-tc-ids` for the authoritative runtime check.
//
// Usage:
//
//	go generate ./test/e2e/...                     # runtime verification
//	make verify-tc-ids                              # alias, explicit ginkgo dep
//	make verify-tc-coverage-strict                 # static-only strict check
//	make verify-tc-coverage-runtime                # runtime check (make ginkgo first)
//
//go:generate go run ./docspec/cmd/catalogcheck --ginkgo --runtime --strict
package e2e
