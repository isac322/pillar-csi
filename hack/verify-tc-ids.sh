#!/usr/bin/env bash
# verify-tc-ids.sh — CI verification script for TC-ID 1-to-1 coverage.
#
# Re-parses docs/testing/*.md (6 spec files), extracts all documented TC IDs,
# and asserts a 1-to-1 match with the Ginkgo node names in test/e2e/.
#
# Exit codes:
#   0  — all TC IDs matched: no missing, no extra, no duplicates
#   1  — mismatch detected (see stderr for MISSING / EXTRA / DUPLICATE report)
#   2  — usage error or environment problem
#
# Flags (forwarded verbatim to catalogcheck):
#   --json              emit JSON-structured report to stdout
#   --spec-file <path>  use pre-enumerated spec names from <path> instead of
#                       running ginkgo --dry-run (one spec name per line)
#
# Environment variables:
#   GINKGO_BIN   override the path to the ginkgo CLI binary
#
# Modes of operation
# ──────────────────
# Runtime mode (preferred, ginkgo CLI required):
#   Runs `ginkgo --dry-run ./test/e2e/` to enumerate every Ginkgo spec name,
#   then matches them against the catalogue.  This is the only mode that
#   correctly handles dynamically-generated node names (tc.tcNodeName()).
#
#   Prerequisites: make ginkgo (installs bin/ginkgo from go.mod)
#
# Fallback — static mode (no ginkgo required):
#   When ginkgo is absent the catalogcheck tool falls back to a regex scan of
#   Go string literals.  Because pillar-csi builds node names via tcNodeName()
#   at runtime, the static scanner will report every catalogue case as MISSING.
#   This is expected behaviour — it is NOT a bug in the test suite.
#   Use runtime mode for authoritative CI gate enforcement.
#
# Examples:
#   ./hack/verify-tc-ids.sh                      # auto-detect mode
#   ./hack/verify-tc-ids.sh --json               # JSON report
#   GINKGO_BIN=/usr/local/bin/ginkgo ./hack/verify-tc-ids.sh
#   ./hack/verify-tc-ids.sh --spec-file /tmp/specs.txt

set -euo pipefail

# ── Resolve repository root ───────────────────────────────────────────────────
# Support being invoked from any working directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Sanity-check: the spec directory must exist.
SPEC_DIR="${REPO_ROOT}/docs/testing"
if [[ ! -d "${SPEC_DIR}" ]]; then
  echo "verify-tc-ids: spec directory not found: ${SPEC_DIR}" >&2
  exit 2
fi

# The catalogcheck package path (relative to REPO_ROOT).
CATALOGCHECK_PKG="./test/e2e/docspec/cmd/catalogcheck"

# ── Resolve ginkgo binary ─────────────────────────────────────────────────────
# Honour GINKGO_BIN env, then bin/ginkgo (local make install), then PATH.
if [[ -z "${GINKGO_BIN:-}" ]]; then
  LOCAL_GINKGO="${REPO_ROOT}/bin/ginkgo"
  if [[ -x "${LOCAL_GINKGO}" ]]; then
    export GINKGO_BIN="${LOCAL_GINKGO}"
  fi
fi

# ── Run the catalogcheck tool ─────────────────────────────────────────────────
# Pass --ginkgo to enable Ginkgo-node matching mode and --strict to enforce a
# non-zero exit code on any mismatch.  Append caller-provided flags (e.g.
# --json, --spec-file) after the fixed flags.
#
# --runtime is added automatically when GINKGO_BIN is set or ginkgo is on PATH,
# so that the tool enumerates actual spec names rather than relying on the
# static source scan.

EXTRA_FLAGS=("$@")

# Decide whether to request runtime enumeration.
USE_RUNTIME=false
if [[ -n "${GINKGO_BIN:-}" ]] || command -v ginkgo &>/dev/null; then
  USE_RUNTIME=true
fi

# Check if --spec-file was passed by the caller (mutually exclusive with
# --runtime in the catalogcheck tool).
for flag in "${EXTRA_FLAGS[@]:-}"; do
  if [[ "${flag}" == "--spec-file" || "${flag}" == --spec-file=* ]]; then
    USE_RUNTIME=false
    break
  fi
done

CATALOGCHECK_FLAGS=(--ginkgo --strict)
if [[ "${USE_RUNTIME}" == "true" ]]; then
  CATALOGCHECK_FLAGS+=(--runtime)
  echo "verify-tc-ids: runtime mode (ginkgo --dry-run)" >&2
else
  echo "verify-tc-ids: static mode (ginkgo not found — install via 'make ginkgo' for runtime mode)" >&2
fi

cd "${REPO_ROOT}"
exec go run "${CATALOGCHECK_PKG}" "${CATALOGCHECK_FLAGS[@]}" "${EXTRA_FLAGS[@]+"${EXTRA_FLAGS[@]}"}"
