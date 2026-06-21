#!/usr/bin/env bash
#
# Drive the SIG-Storage External Storage e2e suite against a running pillar-csi
# install.  Prerequisites:
#
#   * KUBECONFIG points at a cluster where pillar-csi is already deployed.
#   * A PillarTarget named in storage-class.yaml exists and its node is healthy.
#   * curl, tar and kubectl are on PATH.
#
# Environment overrides:
#
#   K8S_VERSION       — kubernetes test bundle version (default: stable).
#   GINKGO_FOCUS      — Ginkgo focus regex (default: 'External.Storage').
#   GINKGO_SKIP       — Ginkgo skip regex (default empty).
#   E2E_TEST_BIN      — path to a pre-extracted e2e.test binary.  When set, the
#                       script skips the download/extract steps entirely.
#   CACHE_DIR         — where to cache the downloaded bundle
#                       (default: $HOME/.cache/pillar-csi/external-e2e).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRIVER_YAML="${SCRIPT_DIR}/external-driver.yaml"
SC_YAML="${SCRIPT_DIR}/storage-class.yaml"

K8S_VERSION="${K8S_VERSION:-}"
CACHE_DIR="${CACHE_DIR:-${HOME}/.cache/pillar-csi/external-e2e}"
GINKGO_FOCUS="${GINKGO_FOCUS:-External.Storage}"

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

exec "${E2E_TEST_BIN}" \
  -kubeconfig="${KUBECONFIG}" \
  -storage.testdriver="${DRIVER_YAML}" \
  -ginkgo.focus="${GINKGO_FOCUS}" \
  -ginkgo.v \
  "${EXTRA_ARGS[@]}"
