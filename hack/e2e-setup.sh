#!/usr/bin/env bash
# hack/e2e-setup.sh
#
# Bootstrap the pillar-csi e2e test environment.
#
# What this script does (in order):
#   1. Validate prerequisites (kind, kubectl, docker/podman, helm)
#   2. Create host directories required by the Kind config (configfs simulation)
#   3. Create the Kind cluster — idempotent: skips creation if already running
#   4. Export / merge the kubeconfig so kubectl points at the e2e cluster
#   5. Build container images (controller, agent, node) and load into Kind
#   6. Install or upgrade the Helm chart with e2e-specific values
#
# Usage:
#   hack/e2e-setup.sh [--cluster-name NAME] [--skip-prereq-check] [--skip-image-build] \
#                     [--skip-helm-deploy] [--helm-release NAME] [--helm-namespace NS]
#
# Environment variables (all optional):
#   KIND_CLUSTER        Cluster name override  (default: pillar-csi-e2e)
#   KIND_CONFIG         Path to kind config    (default: hack/kind-config.yaml)
#   KIND_NODE_IMAGE     Kind node image to pin Kubernetes version
#                       (default: "" — uses Kind's built-in default for the
#                        installed Kind version, e.g. kindest/node:v1.31.0)
#   KUBECONFIG          Written / merged here  (default: ${HOME}/.kube/config)
#   CONTAINER_TOOL      docker or podman        (default: docker)
#   SKIP_IMAGE_BUILD    Set to "true" to skip image build step  (default: false)
#   IMAGE_TAG           Tag applied to all e2e images           (default: e2e)
#   SKIP_HELM_DEPLOY    Set to "true" to skip Helm chart deploy (default: false)
#   HELM_RELEASE        Helm release name                       (default: pillar-csi)
#   HELM_NAMESPACE      Kubernetes namespace for the release    (default: pillar-csi-system)
#   HELM_VALUES         Path to Helm values override file       (default: hack/e2e-values.yaml)
#   HELM_EXTRA_ARGS     Extra arguments appended to helm upgrade (default: "")
#
# Image names built and loaded into Kind:
#   pillar-csi-controller:${IMAGE_TAG}   (built from Dockerfile)
#   pillar-csi-agent:${IMAGE_TAG}        (built from Dockerfile.agent)
#   pillar-csi-node:${IMAGE_TAG}         (built from Dockerfile.node)
#
# Exit codes:
#   0  success
#   1  prerequisite missing / unrecoverable error
#
set -euo pipefail

# ── Constants & defaults ──────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KIND_CLUSTER="${KIND_CLUSTER:-pillar-csi-e2e}"
KIND_CONFIG="${KIND_CONFIG:-${REPO_ROOT}/hack/kind-config.yaml}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"
IMAGE_TAG="${IMAGE_TAG:-e2e}"
SKIP_IMAGE_BUILD="${SKIP_IMAGE_BUILD:-false}"
SKIP_HELM_DEPLOY="${SKIP_HELM_DEPLOY:-false}"
HELM_RELEASE="${HELM_RELEASE:-pillar-csi}"
HELM_NAMESPACE="${HELM_NAMESPACE:-pillar-csi-system}"
HELM_VALUES="${HELM_VALUES:-${REPO_ROOT}/hack/e2e-values.yaml}"
HELM_EXTRA_ARGS="${HELM_EXTRA_ARGS:-}"

# ── Image names (constraint: these are the canonical e2e image names) ─────────
IMG_CONTROLLER="pillar-csi-controller:${IMAGE_TAG}"
IMG_AGENT="pillar-csi-agent:${IMAGE_TAG}"
IMG_NODE="pillar-csi-node:${IMAGE_TAG}"

# Host directory mounted into the storage-worker node as a configfs simulation.
# See hack/kind-config.yaml for the corresponding extraMounts entry.
CONFIGFS_HOST_DIR="/tmp/${KIND_CLUSTER}/configfs"

# ── Colour helpers ────────────────────────────────────────────────────────────

# Detect whether stdout supports colours (disable in CI if TERM is unset)
if [ -t 1 ] && [ "${NO_COLOR:-}" = "" ]; then
  _CLR_RESET='\033[0m'
  _CLR_BOLD='\033[1m'
  _CLR_GREEN='\033[0;32m'
  _CLR_YELLOW='\033[0;33m'
  _CLR_CYAN='\033[0;36m'
  _CLR_RED='\033[0;31m'
else
  _CLR_RESET=''
  _CLR_BOLD=''
  _CLR_GREEN=''
  _CLR_YELLOW=''
  _CLR_CYAN=''
  _CLR_RED=''
fi

log_info()    { printf "${_CLR_CYAN}[INFO]${_CLR_RESET}  %s\n"    "$*"; }
log_ok()      { printf "${_CLR_GREEN}[OK]${_CLR_RESET}    %s\n"   "$*"; }
log_warn()    { printf "${_CLR_YELLOW}[WARN]${_CLR_RESET}  %s\n"  "$*"; }
log_error()   { printf "${_CLR_RED}[ERROR]${_CLR_RESET} %s\n"     "$*" >&2; }
log_section() { printf "\n${_CLR_BOLD}==> %s${_CLR_RESET}\n"     "$*"; }

# ── Argument parsing ──────────────────────────────────────────────────────────

SKIP_PREREQ_CHECK=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster-name)
      KIND_CLUSTER="$2"; shift 2 ;;
    --cluster-name=*)
      KIND_CLUSTER="${1#*=}"; shift ;;
    --skip-prereq-check)
      SKIP_PREREQ_CHECK=true; shift ;;
    --skip-image-build)
      SKIP_IMAGE_BUILD=true; shift ;;
    --skip-helm-deploy)
      SKIP_HELM_DEPLOY=true; shift ;;
    --helm-release)
      HELM_RELEASE="$2"; shift 2 ;;
    --helm-release=*)
      HELM_RELEASE="${1#*=}"; shift ;;
    --helm-namespace)
      HELM_NAMESPACE="$2"; shift 2 ;;
    --helm-namespace=*)
      HELM_NAMESPACE="${1#*=}"; shift ;;
    --image-tag)
      IMAGE_TAG="$2"
      IMG_CONTROLLER="pillar-csi-controller:${IMAGE_TAG}"
      IMG_AGENT="pillar-csi-agent:${IMAGE_TAG}"
      IMG_NODE="pillar-csi-node:${IMAGE_TAG}"
      shift 2 ;;
    --image-tag=*)
      IMAGE_TAG="${1#*=}"
      IMG_CONTROLLER="pillar-csi-controller:${IMAGE_TAG}"
      IMG_AGENT="pillar-csi-agent:${IMAGE_TAG}"
      IMG_NODE="pillar-csi-node:${IMAGE_TAG}"
      shift ;;
    -h|--help)
      sed -n '/^# hack\/e2e-setup.sh/,/^set -/{ /^set -/d; s/^# \{0,1\}//; p }' "$0"
      exit 0 ;;
    *)
      log_error "Unknown argument: $1"
      exit 1 ;;
  esac
done

# ── Section 1: Prerequisite check ────────────────────────────────────────────

log_section "Checking prerequisites"

check_tool() {
  local tool="$1"
  local hint="${2:-}"
  if command -v "${tool}" >/dev/null 2>&1; then
    log_ok "${tool} found: $(command -v "${tool}")"
  else
    log_error "${tool} is not installed or not on PATH."
    [ -n "${hint}" ] && log_error "  Hint: ${hint}"
    return 1
  fi
}

if [ "${SKIP_PREREQ_CHECK}" = "false" ]; then
  _prereq_ok=true

  check_tool kind       "https://kind.sigs.k8s.io/docs/user/quick-start/#installation"  || _prereq_ok=false
  check_tool kubectl    "https://kubernetes.io/docs/tasks/tools/"                        || _prereq_ok=false
  check_tool helm       "https://helm.sh/docs/intro/install/"                            || _prereq_ok=false
  check_tool "${CONTAINER_TOOL}" \
    "https://docs.docker.com/engine/install/ (or set CONTAINER_TOOL=podman)" || _prereq_ok=false

  if [ "${_prereq_ok}" = "false" ]; then
    log_error "One or more prerequisites are missing. Aborting."
    exit 1
  fi
  log_ok "All prerequisites satisfied."
else
  log_warn "Prerequisite check skipped (--skip-prereq-check)."
fi

# ── Section 2: Host directory setup ──────────────────────────────────────────

log_section "Preparing host directories"

# The storage-worker Kind node mounts CONFIGFS_HOST_DIR as a configfs
# simulation (/sys/kernel/config inside the node container).  The directory
# must exist on the host before `kind create cluster` is called, otherwise
# Kind will fail when it tries to bind-mount a non-existent path.
if [ ! -d "${CONFIGFS_HOST_DIR}" ]; then
  log_info "Creating configfs host directory: ${CONFIGFS_HOST_DIR}"
  mkdir -p "${CONFIGFS_HOST_DIR}"
  log_ok "Created ${CONFIGFS_HOST_DIR}"
else
  log_ok "configfs host directory already exists: ${CONFIGFS_HOST_DIR}"
fi

# ── Section 3: Kind cluster bootstrap (idempotent) ───────────────────────────

log_section "Kind cluster bootstrap"
log_info "Cluster name : ${KIND_CLUSTER}"
log_info "Kind config  : ${KIND_CONFIG}"
if [ -n "${KIND_NODE_IMAGE}" ]; then
  log_info "Node image   : ${KIND_NODE_IMAGE}"
fi

# ── Idempotency check ─────────────────────────────────────────────────────────
# `kind get clusters` prints one cluster name per line.  We grep for an exact
# match (word-boundary anchors) to avoid false positives when one cluster name
# is a prefix of another (e.g. "pillar-csi-e2e" vs "pillar-csi-e2e-old").
if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
  log_warn "Kind cluster '${KIND_CLUSTER}' already exists — skipping creation."
  log_warn "To recreate it, run:  kind delete cluster --name ${KIND_CLUSTER}"
else
  log_info "Kind cluster '${KIND_CLUSTER}' not found — creating..."

  if [ ! -f "${KIND_CONFIG}" ]; then
    log_error "Kind config file not found: ${KIND_CONFIG}"
    log_error "Run this script from the repository root or set KIND_CONFIG."
    exit 1
  fi

  # Build the kind create command.  --image is optional: when KIND_NODE_IMAGE is
  # set it pins the Kubernetes version used in every cluster node; when unset
  # Kind's built-in default for the installed Kind release is used.
  KIND_CREATE_ARGS=(
    "--name"   "${KIND_CLUSTER}"
    "--config" "${KIND_CONFIG}"
    "--wait"   "120s"
  )
  if [ -n "${KIND_NODE_IMAGE}" ]; then
    KIND_CREATE_ARGS+=( "--image" "${KIND_NODE_IMAGE}" )
  fi

  kind create cluster "${KIND_CREATE_ARGS[@]}"

  log_ok "Kind cluster '${KIND_CLUSTER}' created successfully."
fi

# ── Section 4: Kubeconfig ─────────────────────────────────────────────────────

log_section "Configuring kubeconfig"

# Ensure the kubeconfig directory exists (handles the case where ~/.kube doesn't
# exist yet, e.g. a fresh CI runner).
KUBECONFIG_DIR="$(dirname "${KUBECONFIG}")"
if [ ! -d "${KUBECONFIG_DIR}" ]; then
  log_info "Creating kubeconfig directory: ${KUBECONFIG_DIR}"
  mkdir -p "${KUBECONFIG_DIR}"
fi

# Export the cluster's kubeconfig and merge it into the target KUBECONFIG file.
# `kind export kubeconfig` does the merge automatically when KUBECONFIG is set.
log_info "Merging kubeconfig for '${KIND_CLUSTER}' into ${KUBECONFIG}"
kind export kubeconfig \
  --name    "${KIND_CLUSTER}" \
  --kubeconfig "${KUBECONFIG}"

log_ok "kubeconfig updated."

# Switch current context to the e2e cluster.
KUBE_CONTEXT="kind-${KIND_CLUSTER}"
if kubectl config use-context "${KUBE_CONTEXT}" --kubeconfig "${KUBECONFIG}" >/dev/null 2>&1; then
  log_ok "kubectl context set to '${KUBE_CONTEXT}'."
else
  log_warn "Could not switch kubectl context to '${KUBE_CONTEXT}'; it may already be current."
fi

# ── Section 5: Container image build & Kind load ──────────────────────────────
#
# Build the three pillar-csi container images and load them into every node of
# the Kind cluster.  Loading (instead of pushing to a registry) avoids the need
# for a local registry sidecar and works identically on Linux and macOS.
#
# Build targets:
#   Dockerfile        → pillar-csi-controller:${IMAGE_TAG}
#   Dockerfile.agent  → pillar-csi-agent:${IMAGE_TAG}
#   Dockerfile.node   → pillar-csi-node:${IMAGE_TAG}

log_section "Container image build"

if [ "${SKIP_IMAGE_BUILD}" = "true" ]; then
  log_warn "Image build skipped (--skip-image-build / SKIP_IMAGE_BUILD=true)."
  log_warn "Assuming images already exist in the local daemon:"
  log_warn "  ${IMG_CONTROLLER}"
  log_warn "  ${IMG_AGENT}"
  log_warn "  ${IMG_NODE}"
else
  log_info "Image tag    : ${IMAGE_TAG}"
  log_info "Build tool   : ${CONTAINER_TOOL}"
  log_info "Repo root    : ${REPO_ROOT}"
  log_info ""

  # ── Helper: build one image ────────────────────────────────────────────────
  # Usage: build_image <image-ref> <dockerfile>
  build_image() {
    local image_ref="$1"
    local dockerfile="$2"

    if [ ! -f "${REPO_ROOT}/${dockerfile}" ]; then
      log_error "Dockerfile not found: ${REPO_ROOT}/${dockerfile}"
      return 1
    fi

    log_info "Building ${image_ref} from ${dockerfile} ..."
    "${CONTAINER_TOOL}" build \
      --file   "${REPO_ROOT}/${dockerfile}" \
      --tag    "${image_ref}" \
      "${REPO_ROOT}"
    log_ok "Built ${image_ref}"
  }

  # ── Build all three images ─────────────────────────────────────────────────
  build_image "${IMG_CONTROLLER}" "Dockerfile"
  build_image "${IMG_AGENT}"      "Dockerfile.agent"
  build_image "${IMG_NODE}"       "Dockerfile.node"

  log_ok "All images built successfully."
fi

# ── Section 5b: Load images into Kind ─────────────────────────────────────────
#
# `kind load docker-image` copies an image from the local Docker / podman daemon
# into every node of the named Kind cluster.  This makes the images available
# without requiring a registry or `imagePullPolicy: Never` workaround.
#
# The load step is intentionally separate from the build step so that it always
# runs — even when --skip-image-build is passed — allowing pre-built images to
# be (re-)loaded after a cluster recreate.

log_section "Loading images into Kind cluster '${KIND_CLUSTER}'"

# ── Helper: load one image ─────────────────────────────────────────────────
# Usage: load_image <image-ref>
load_image() {
  local image_ref="$1"

  # Verify the image actually exists in the local daemon before attempting to
  # load it, so we get a clear error message rather than a cryptic kind failure.
  if ! "${CONTAINER_TOOL}" image inspect "${image_ref}" >/dev/null 2>&1; then
    log_error "Image not found in local daemon: ${image_ref}"
    log_error "Build it first (remove --skip-image-build) or pull/tag it manually."
    return 1
  fi

  log_info "Loading ${image_ref} → kind/${KIND_CLUSTER} ..."
  kind load docker-image \
    --name "${KIND_CLUSTER}" \
    "${image_ref}"
  log_ok "Loaded ${image_ref}"
}

load_image "${IMG_CONTROLLER}"
load_image "${IMG_AGENT}"
load_image "${IMG_NODE}"

log_ok "All images loaded into Kind cluster '${KIND_CLUSTER}'."

# ── Section 6: Helm chart deploy ──────────────────────────────────────────────
#
# Install or upgrade the pillar-csi Helm chart into the Kind cluster.
# We use `helm upgrade --install` for idempotency: the command creates the
# release on first run and upgrades it on subsequent runs.
#
# Values layering (last wins):
#   1. charts/pillar-csi/values.yaml       — chart defaults
#   2. hack/e2e-values.yaml                — e2e overrides (imagePullPolicy: Never,
#                                            local image repos, reduced resources)
#   3. --set overrides below               — dynamic values (image tag)

log_section "Helm chart deployment"

if [ "${SKIP_HELM_DEPLOY}" = "true" ]; then
  log_warn "Helm deploy skipped (--skip-helm-deploy / SKIP_HELM_DEPLOY=true)."
  log_warn "Release '${HELM_RELEASE}' in namespace '${HELM_NAMESPACE}' may not exist."
else
  log_info "Helm release  : ${HELM_RELEASE}"
  log_info "Namespace     : ${HELM_NAMESPACE}"
  log_info "Chart         : ${REPO_ROOT}/charts/pillar-csi"
  log_info "Values file   : ${HELM_VALUES}"
  log_info "Image tag     : ${IMAGE_TAG}"
  log_info ""

  # Verify the values override file exists.
  if [ ! -f "${HELM_VALUES}" ]; then
    log_error "Helm values override file not found: ${HELM_VALUES}"
    log_error "Expected: hack/e2e-values.yaml — run this script from the repo root."
    exit 1
  fi

  # Verify the chart directory exists.
  HELM_CHART_DIR="${REPO_ROOT}/charts/pillar-csi"
  if [ ! -d "${HELM_CHART_DIR}" ]; then
    log_error "Helm chart directory not found: ${HELM_CHART_DIR}"
    exit 1
  fi

  # Build the helm upgrade command.
  # --set overrides for the three locally-built images (dynamic IMAGE_TAG).
  # These are applied AFTER the static values file so they always win.
  HELM_SET_ARGS=(
    "--set" "controller.image.repository=pillar-csi-controller"
    "--set" "controller.image.tag=${IMAGE_TAG}"
    "--set" "controller.image.pullPolicy=Never"
    "--set" "agent.image.repository=pillar-csi-agent"
    "--set" "agent.image.tag=${IMAGE_TAG}"
    "--set" "agent.image.pullPolicy=Never"
    "--set" "node.image.repository=pillar-csi-node"
    "--set" "node.image.tag=${IMAGE_TAG}"
    "--set" "node.image.pullPolicy=Never"
  )

  # Construct optional extra-args array (may be empty).
  HELM_EXTRA_ARGS_ARRAY=()
  if [ -n "${HELM_EXTRA_ARGS}" ]; then
    # Word-split intentional; caller is responsible for quoting.
    # shellcheck disable=SC2206
    HELM_EXTRA_ARGS_ARRAY=( ${HELM_EXTRA_ARGS} )
  fi

  log_info "Running: helm upgrade --install ${HELM_RELEASE} ..."
  helm upgrade --install \
    "${HELM_RELEASE}" \
    "${HELM_CHART_DIR}" \
    --namespace        "${HELM_NAMESPACE}" \
    --create-namespace \
    --kubeconfig       "${KUBECONFIG}" \
    --kube-context     "kind-${KIND_CLUSTER}" \
    --values           "${HELM_VALUES}" \
    "${HELM_SET_ARGS[@]}" \
    "${HELM_EXTRA_ARGS_ARRAY[@]+"${HELM_EXTRA_ARGS_ARRAY[@]}"}" \
    --wait \
    --timeout 5m0s

  log_ok "Helm release '${HELM_RELEASE}' deployed to namespace '${HELM_NAMESPACE}'."
fi

# ── Done ──────────────────────────────────────────────────────────────────────

log_section "e2e environment ready"
log_info "Cluster   : ${KIND_CLUSTER}"
log_info "Context   : kind-${KIND_CLUSTER}"
log_info "Config    : ${KIND_CONFIG}"
log_info "Images    :"
log_info "  ${IMG_CONTROLLER}"
log_info "  ${IMG_AGENT}"
log_info "  ${IMG_NODE}"
if [ "${SKIP_HELM_DEPLOY}" != "true" ]; then
  log_info "Helm      :"
  log_info "  release   : ${HELM_RELEASE}"
  log_info "  namespace : ${HELM_NAMESPACE}"
fi
log_info ""
log_info "Next steps:"
log_info "  # Run e2e tests:"
log_info "  make test-e2e KIND_CLUSTER=${KIND_CLUSTER}"
log_info ""
log_info "  # Tear down when done:"
log_info "  hack/e2e-teardown.sh --cluster-name ${KIND_CLUSTER}"
log_info "  # or: kind delete cluster --name ${KIND_CLUSTER}"
