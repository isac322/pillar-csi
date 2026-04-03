# E2E Acceptance Criteria — Unified 10-Item System

This document defines the **10 canonical Acceptance Criteria** for the
pillar-csi E2E suite.  Both the seed (task specification) and the evaluator
use these exact 10 items with these exact numbers.

| AC  | Title | Requirement |
|-----|-------|-------------|
| AC 1 | All TCs pass, no skips | `make test-e2e` runs with skip=0 and fail=0. Every TC must either pass or hard-fail — `Skip()` is forbidden in TC implementation code. |
| AC 2 | 2-minute suite budget | `make test-e2e` (Ginkgo spec execution phase) completes within 120 seconds. `suiteLevelTimeout = 2 * time.Minute` enforces this; parallel workers are capped at 8 to avoid Kind API-server saturation. |
| AC 3 | 7 E33 standalone specs in default-profile | All seven E33.4 standalone specs (TC-E33.311 through TC-E33.317) carry the `default-profile` Ginkgo label and bind to static `[TC-E33.xxx]` literals in `lvm_backend_standalone_e2e_test.go`. |
| AC 4 | No `defaultLocalVerifierRegistry` singleton | The old package-level `defaultLocalVerifierRegistry` global is replaced by `suiteLocalVerifierRegistry`, initialised per Ginkgo worker process in `warmUpLocalBackend()`. No shared mutable state crosses worker boundaries. |
| AC 5 | No conditional `Skip()` in TC code | TC implementation files (`tc_*.go`, `category_*.go`) contain zero `Skip()` / `GinkgoSkip()` calls. All gate conditions use hard `Fail()` / `Expect()`. |
| AC 6 | `go build -tags=e2e ./... && go vet -tags=e2e ./...` pass | The entire codebase compiles and passes vet under the `e2e` build tag. No build errors, no vet warnings. |
| AC 7 | No artifacts written outside `/tmp` | All temporary files, kubeconfig, build artifacts created during the test run reside under `tcTempRoot` (a `/tmp`-backed per-invocation directory). `os.MkdirTemp("", …)` with an empty base dir is forbidden. |
| AC 8 | All `os.MkdirTemp` calls use `tcTempRoot` | Every `os.MkdirTemp` call in the `test/e2e` package tree passes `tcTempRoot` as the first argument; zero bare empty-string calls exist. Go build-cache `/tmp` redirect is also forbidden. |
| AC 9 | `-tags=e2e` includes real-backend specs | `make test-e2e` passes `-tags=e2e` so that all `*_e2e_test.go` files (E33, E34, E35, F27–F31, Kind bootstrap) are compiled and included in the default-profile run. |
| AC 10 | Provisioner dead code removed + TC count matches | `framework/provisioner/provisioner.go` contains no soft-skip dead code. `docs/E2E-TESTCASES.md` declares `총 테스트 케이스: 404`, matching the 404 TCs that actually run under the default-profile label filter. |

## Mapping from Generation 1 internal Sub-AC hierarchy to flat AC numbers

| Old internal reference | Canonical flat AC |
|------------------------|-------------------|
| Sub-AC 1 (pass/fail profile) | AC 1 |
| Sub-AC 2, 2a, 2b, 2.1, 2.2, 2.3 | AC 2 |
| Sub-AC 3, 3.3 (E33 standalone) | AC 3 |
| Sub-AC 4 (defaultLocalVerifierRegistry) | AC 4 |
| Sub-AC 5 (Skip() removal) | AC 5 |
| Sub-AC 6, AC6 (build/vet) | AC 6 |
| Sub-AC 7, [AC5], [AC5.2], [AC5.3] (/tmp artifacts) | AC 7 |
| Sub-AC 8, AC8 (os.MkdirTemp) | AC 8 |
| Sub-AC 9c, AC9c (real-backend specs) | AC 9 |
| AC 10 (dead code + TC count) | AC 10 |

## Implementation notes

- **AC 2** (2-minute budget): `suiteLevelTimeout` in `main_test.go` enforces the
  Ginkgo-level wall-clock deadline.  `stageBudgetSeconds` in `stage_timer.go`
  tracks per-stage budgets.  The `make test-e2e-bench` Makefile target CI-gates
  the in-process sequential run against a 120-second budget.

- **AC 5** (no `Skip()`): `prereq_ac10_test.go` contains a static source-code scan
  that asserts zero `Skip()` / `GinkgoSkip()` / `t.Skip()` calls exist in
  non-framework production code.

- **AC 7** (`/tmp` boundary): `artifact_path_guard_post_suite.go` runs a post-suite
  filesystem scan; `artifact_io_audit_test.go` performs a static AST audit at
  package-test time.  Both fail hard if any file is detected outside `tcTempRoot`.

- **AC 8** (`os.MkdirTemp` with `tcTempRoot`): `artifact_io_audit_test.go`
  (`TestArtifactPathsStayUnderTmpRoot`) uses Go AST analysis to assert that every
  `os.MkdirTemp` call passes a non-empty `tcTempRoot`-derived first argument.

- **AC 10** (TC count = 404):
  - 239 in-process TCs (E1–E8, E9, E15–E17, E28–E32)
  - 117 envtest TCs (E10–E14, E18–E26, E27 Helm)
  - 48 cluster TCs: E10 (3) + E27 (29) + E33.4 standalone (7) + teardown-guarantee (4) + backend-teardown-absence (5)
  - Non-default-profile (not counted): E33 mount (12), E33 expansion (5), E34 (13), E35 (13), F27–F31 (19)
    Note: E33 mount and expansion have `e2e_helm` build tag (require Helm-deployed agent)
