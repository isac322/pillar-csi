#!/usr/bin/env bash
#
# Drive the SIG-Storage External Storage e2e suite against a running pillar-csi
# install.  Prerequisites:
#
#   * KUBECONFIG points at a cluster where pillar-csi is already deployed.
#   * The PillarStore / PillarProtocol named in storage-class.yaml exist and
#     the store's PillarAgent node is healthy.
#   * curl, tar and kubectl are on PATH.
#
# Environment overrides:
#
#   K8S_VERSION       — kubernetes test bundle version (default: stable).
#   GINKGO_FOCUS      — Ginkgo focus regex (default: 'External.Storage').
#   GINKGO_SKIP       — Ginkgo skip regex (default empty).
#   E2E_TEST_BIN      — path to a pre-extracted e2e.test binary.  When set, the
#                       script skips the download/extract steps entirely.
#   GINKGO_PROCS      — positive worker count (default: 1).  Values greater than
#                       1 require the matching bundled ginkgo executable beside
#                       E2E_TEST_BIN.
#   CACHE_DIR         — where to cache the downloaded bundle
#                       (default: $HOME/.cache/pillar-csi/external-e2e).
#   EXTERNAL_E2E_REPORT_DIR
#                     — optional directory for a merged Ginkgo JSON report
#                       (external-e2e.json) plus a status file
#                       (external-e2e-report-status.txt).  Only honoured when
#                       GINKGO_PROCS > 1; the bundled Ginkgo CLI must advertise
#                       --output-dir and --json-report or the run fails closed.
#                       The GINKGO_PROCS=1 direct e2e.test path ignores it.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRIVER_YAML="${SCRIPT_DIR}/external-driver.yaml"
SC_YAML="${SCRIPT_DIR}/storage-class.yaml"

K8S_VERSION="${K8S_VERSION:-}"
CACHE_DIR="${CACHE_DIR:-${HOME}/.cache/pillar-csi/external-e2e}"
GINKGO_FOCUS="${GINKGO_FOCUS:-External.Storage}"
GINKGO_PROCS="${GINKGO_PROCS-1}"
if [[ ! "${GINKGO_PROCS}" =~ ^[1-9][0-9]*$ ]]; then
  printf 'ERROR: GINKGO_PROCS must be a positive integer (got %q).\n' \
    "${GINKGO_PROCS}" >&2
  exit 1
fi

# Default skip set drops only categories that are intentionally out of scope
# for the PR-gating job:
#
#   * [Slow] / [Serial] / [Disruptive] — standard upstream categories that
#     blow the per-job runtime budget; covered by the scheduled bare-metal
#     run, not by every PR.
#   * Generic Ephemeral-volume — not declared as a supported capability in
#     external-driver.yaml.
#
# Data-plane workload specs (provisioning + actual pod mount + store data +
# exec + topology + volume-expand) are now in scope: with agent + node
# DaemonSets running hostNetwork: true the kernel nvmet listener and the
# `nvme connect` initiator share the host netns and the connect SYN reaches
# the listener (see PRD §2.4 for the kernel netns rationale).
#
# Override with GINKGO_SKIP='' to opt into the otherwise-excluded categories
# locally.
GINKGO_SKIP="${GINKGO_SKIP:-\\[Slow\\]|\\[Serial\\]|\\[Disruptive\\]|Generic Ephemeral-volume}"

if [[ -z "${KUBECONFIG:-}" ]]; then
  echo "ERROR: KUBECONFIG is not set." >&2
  echo "Point it at a cluster where pillar-csi is already deployed." >&2
  exit 1
fi

if ! kubectl cluster-info >/dev/null 2>&1; then
  echo "ERROR: kubectl cannot reach the cluster at ${KUBECONFIG}." >&2
  exit 1
fi

if [[ -z "${E2E_TEST_BIN:-}" ]]; then
  if [[ -z "${K8S_VERSION}" ]]; then
    K8S_VERSION="$(curl -sSL https://dl.k8s.io/release/stable.txt)"
  fi
  ARCH="$(uname -m)"
  case "${ARCH}" in
    x86_64)  GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
    *)       GOARCH="${ARCH}" ;;
  esac
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"

  mkdir -p "${CACHE_DIR}"
  EXTRACT_DIR="${CACHE_DIR}/${K8S_VERSION}-${OS}-${GOARCH}"
  E2E_TEST_BIN="${EXTRACT_DIR}/kubernetes/test/bin/e2e.test"

  if [[ ! -x "${E2E_TEST_BIN}" ]]; then
    TARBALL="${CACHE_DIR}/kubernetes-test-${K8S_VERSION}-${OS}-${GOARCH}.tar.gz"
    URL="https://dl.k8s.io/${K8S_VERSION}/kubernetes-test-${OS}-${GOARCH}.tar.gz"

    echo "==> Downloading e2e.test bundle ${K8S_VERSION} (${OS}/${GOARCH})"
    curl -fSL --retry 3 -o "${TARBALL}" "${URL}"

    echo "==> Extracting to ${EXTRACT_DIR}"
    mkdir -p "${EXTRACT_DIR}"
    # Extract everything: e2e.test relies on testing-manifests being present
    # at "../../testing-manifests" relative to its binary location.
    tar -xzf "${TARBALL}" -C "${EXTRACT_DIR}"
  fi
fi

if [[ ! -x "${E2E_TEST_BIN}" ]]; then
  echo "ERROR: e2e.test binary not found at ${E2E_TEST_BIN}" >&2
  exit 1
fi

echo "==> Applying StorageClass from ${SC_YAML}"
kubectl apply -f "${SC_YAML}"

echo "==> Running External Storage e2e against pillar-csi"
echo "    driver manifest : ${DRIVER_YAML}"
echo "    e2e.test binary : ${E2E_TEST_BIN}"
echo "    focus           : ${GINKGO_FOCUS}"
[[ -n "${GINKGO_SKIP}" ]] && echo "    skip            : ${GINKGO_SKIP}"

EXTRA_ARGS=()
if [[ -n "${GINKGO_SKIP}" ]]; then
  EXTRA_ARGS+=("-ginkgo.skip=${GINKGO_SKIP}")
fi

# e2e.test resolves testing-manifests via paths relative to its cwd
# (it expects test/conformance/testdata/... and test/e2e/testing-manifests/...
# to be reachable as "../../test/..." from where it was invoked).  Cd into
# kubernetes/test/bin so that "../.." lands on the extracted source root.
KUBE_TEST_BIN_DIR="$(dirname "${E2E_TEST_BIN}")"

# external-driver.yaml's StorageClass.FromFile is resolved by e2e.test through
# a RootFileSource rooted at "<cwd>/../..", i.e. the extracted kubernetes/
# directory.  Stage storage-class.yaml there so the lookup succeeds.
KUBE_ROOT_DIR="$(realpath "${KUBE_TEST_BIN_DIR}/../..")"
cp "${SC_YAML}" "${KUBE_ROOT_DIR}/storage-class.yaml"

cd "${KUBE_TEST_BIN_DIR}"
if [[ -n "${E2E_FAIL_FAST:-}" ]]; then
  EXTRA_ARGS+=("-ginkgo.fail-fast")
  echo "    fail-fast       : enabled"
fi

E2E_ARGS=(
  -kubeconfig="${KUBECONFIG}"
  -storage.testdriver="${DRIVER_YAML}"
  -ginkgo.focus="${GINKGO_FOCUS}"
  -ginkgo.v
  "${EXTRA_ARGS[@]}"
)

if [[ "${GINKGO_PROCS}" == 1 ]]; then
  exec "${E2E_TEST_BIN}" "${E2E_ARGS[@]}"
fi

GINKGO_CLI="${KUBE_TEST_BIN_DIR}/ginkgo"
if [[ ! -x "${GINKGO_CLI}" ]]; then
  printf 'ERROR: GINKGO_PROCS=%s requires the matching bundled Ginkgo CLI at %s.\n' \
    "${GINKGO_PROCS}" "${GINKGO_CLI}" >&2
  exit 1
fi

if [[ -z "${EXTERNAL_E2E_REPORT_DIR:-}" ]]; then
  exec "${GINKGO_CLI}" run "--procs=${GINKGO_PROCS}" -v "${E2E_TEST_BIN}" -- \
    "${E2E_ARGS[@]}"
fi

# Fail closed: only pass report flags the bundled CLI advertises.  `ginkgo help
# run` prints the run usage (including every flag) to stdout and exits 0
# without compiling or running anything.
GINKGO_RUN_HELP="$("${GINKGO_CLI}" help run 2>&1 || true)"
for flag in --output-dir --json-report; do
  if [[ "${GINKGO_RUN_HELP}" != *"${flag}"* ]]; then
    printf 'ERROR: EXTERNAL_E2E_REPORT_DIR is set but %s does not advertise %s.\n' \
      "${GINKGO_CLI}" "${flag}" >&2
    exit 1
  fi
done

REPORT_NAME="external-e2e.json"
mkdir -p "${EXTERNAL_E2E_REPORT_DIR}"
REPORT_DIR="$(realpath "${EXTERNAL_E2E_REPORT_DIR}")"
REPORT_PATH="${REPORT_DIR}/${REPORT_NAME}"
STATUS_PATH="${REPORT_DIR}/external-e2e-report-status.txt"
rm -f "${REPORT_PATH}"

# The status file starts as "unmeasured" and is rewritten only after Ginkgo
# exits.  A hard kill (runner timeout SIGKILL) leaves it unmeasured; any
# non-zero exit (fail-fast abort, spec failure, interrupt) marks the report
# partial even when Ginkgo managed to write it.
write_report_status() {
  local status="$1" exit_code="$2" present=false
  [[ -s "${REPORT_PATH}" ]] && present=true
  printf 'status=%s\nexit_code=%s\nprocs=%s\nreport=%s\nreport_present=%s\n' \
    "${status}" "${exit_code}" "${GINKGO_PROCS}" "${REPORT_NAME}" "${present}" \
    >"${STATUS_PATH}"
}
write_report_status unmeasured none
echo "    json report     : ${REPORT_PATH}"

# Run Ginkgo as a child (not exec) so its exit status can be recorded.  Forward
# INT/TERM as TERM — Ginkgo treats both as an interrupt, and background jobs in
# a non-interactive shell start with SIGINT ignored.
ginkgo_pid=""
forward_interrupt() {
  [[ -n "${ginkgo_pid}" ]] && kill -TERM "${ginkgo_pid}" 2>/dev/null || true
}
trap forward_interrupt INT TERM

"${GINKGO_CLI}" run "--procs=${GINKGO_PROCS}" -v \
  "--output-dir=${REPORT_DIR}" "--json-report=${REPORT_NAME}" \
  "${E2E_TEST_BIN}" -- "${E2E_ARGS[@]}" &
ginkgo_pid=$!

ginkgo_rc=0
while :; do
  ginkgo_rc=0
  wait "${ginkgo_pid}" || ginkgo_rc=$?
  # A trapped signal interrupts `wait` while Ginkgo is still draining.
  kill -0 "${ginkgo_pid}" 2>/dev/null || break
done
trap - INT TERM

if [[ "${ginkgo_rc}" -eq 0 && -s "${REPORT_PATH}" ]]; then
  write_report_status complete "${ginkgo_rc}"
elif [[ -s "${REPORT_PATH}" ]]; then
  write_report_status partial "${ginkgo_rc}"
else
  write_report_status unmeasured "${ginkgo_rc}"
fi
echo "==> External e2e report status ($(tr '\n' ' ' <"${STATUS_PATH}"))"
exit "${ginkgo_rc}"
