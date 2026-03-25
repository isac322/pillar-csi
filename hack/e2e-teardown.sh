#!/usr/bin/env bash
# hack/e2e-teardown.sh
#
# Tears down the pillar-csi e2e test environment.  Idempotent: safe to run even
# when the cluster or containers do not exist.
#
# What it cleans up:
#   1. External-agent Docker container (pillar-csi-external-agent), if running
#   2. Kind cluster (pillar-csi-e2e by default)
#   3. Temporary host directories created by hack/e2e-setup.sh
#
# Optional:
#   --images     Also remove the three e2e Docker images from the local daemon
#   --all        Equivalent to --images (removes everything)
#
# Usage:
#   hack/e2e-teardown.sh
#   hack/e2e-teardown.sh --images
#   KIND_CLUSTER=my-cluster hack/e2e-teardown.sh
#
# Environment variables (all optional, defaults shown):
#   KIND_CLUSTER          Kind cluster name              (pillar-csi-e2e)
#   EXTERNAL_AGENT_NAME   Docker container name to stop  (pillar-csi-external-agent)
#   E2E_TMPDIR            Host tmp dir to remove         (/tmp/pillar-csi-e2e)
#   CONTROLLER_IMAGE      Controller image tag           (pillar-csi-controller:e2e)
#   AGENT_IMAGE           Agent image tag                (pillar-csi-agent:e2e)
#   NODE_IMAGE            Node image tag                 (pillar-csi-node:e2e)

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
if [ -t 1 ] && command -v tput >/dev/null 2>&1 && tput colors >/dev/null 2>&1; then
  GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3); RED=$(tput setaf 1); BOLD=$(tput bold); RESET=$(tput sgr0)
else
  GREEN=""; YELLOW=""; RED=""; BOLD=""; RESET=""
fi

info()    { echo "${GREEN}[e2e-teardown]${RESET} $*"; }
warning() { echo "${YELLOW}[e2e-teardown] WARN:${RESET} $*"; }
step()    { echo "${BOLD}[e2e-teardown] ──${RESET} $*"; }

# ── Defaults ──────────────────────────────────────────────────────────────────
KIND_CLUSTER="${KIND_CLUSTER:-pillar-csi-e2e}"
EXTERNAL_AGENT_NAME="${EXTERNAL_AGENT_NAME:-pillar-csi-external-agent}"
E2E_TMPDIR="${E2E_TMPDIR:-/tmp/pillar-csi-e2e}"

CONTROLLER_IMAGE="${CONTROLLER_IMAGE:-pillar-csi-controller:e2e}"
AGENT_IMAGE="${AGENT_IMAGE:-pillar-csi-agent:e2e}"
NODE_IMAGE="${NODE_IMAGE:-pillar-csi-node:e2e}"

REMOVE_IMAGES=false

# ── Argument parsing ──────────────────────────────────────────────────────────
for arg in "$@"; do
  case "$arg" in
    --images|--all) REMOVE_IMAGES=true ;;
    --help|-h)
      sed -n '/^# hack/,/^[^#]/{ /^[^#]/d; s/^# \{0,1\}//p }' "$0"
      exit 0
      ;;
    *)
      echo "${RED}[e2e-teardown] Unknown argument: $arg${RESET}" >&2
      echo "Usage: $0 [--images|--all]" >&2
      exit 1
      ;;
  esac
done

# ── Prerequisite checks ───────────────────────────────────────────────────────
if ! command -v kind >/dev/null 2>&1; then
  warning "kind not found in PATH — skipping cluster deletion."
  KIND_AVAILABLE=false
else
  KIND_AVAILABLE=true
fi

if ! command -v docker >/dev/null 2>&1; then
  warning "docker not found in PATH — skipping container / image cleanup."
  DOCKER_AVAILABLE=false
else
  DOCKER_AVAILABLE=true
fi

# ── Step 1: stop external-agent container ────────────────────────────────────
step "Stopping external-agent container '${EXTERNAL_AGENT_NAME}' (if any)..."

if [ "$DOCKER_AVAILABLE" = true ]; then
  if docker inspect --type container "${EXTERNAL_AGENT_NAME}" >/dev/null 2>&1; then
    info "Stopping container ${EXTERNAL_AGENT_NAME}..."
    docker stop "${EXTERNAL_AGENT_NAME}" >/dev/null
    docker rm   "${EXTERNAL_AGENT_NAME}" >/dev/null
    info "Container removed."
  else
    info "Container '${EXTERNAL_AGENT_NAME}' not found — nothing to do."
  fi
else
  info "Skipping (docker unavailable)."
fi

# ── Step 2: delete Kind cluster ───────────────────────────────────────────────
step "Deleting Kind cluster '${KIND_CLUSTER}' (if it exists)..."

if [ "$KIND_AVAILABLE" = true ]; then
  if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
    info "Deleting cluster ${KIND_CLUSTER}..."
    kind delete cluster --name "${KIND_CLUSTER}"
    info "Cluster deleted."
  else
    info "Cluster '${KIND_CLUSTER}' not found — nothing to do."
  fi
else
  info "Skipping (kind unavailable)."
fi

# ── Step 3: remove temporary host directories ─────────────────────────────────
step "Removing temporary host directory '${E2E_TMPDIR}' (if it exists)..."

if [ -e "${E2E_TMPDIR}" ]; then
  # Guard: only remove paths that look like our temp dir to avoid accidents
  case "${E2E_TMPDIR}" in
    /tmp/*)
      rm -rf "${E2E_TMPDIR}"
      info "Removed ${E2E_TMPDIR}."
      ;;
    *)
      warning "E2E_TMPDIR='${E2E_TMPDIR}' is not under /tmp — refusing to remove it."
      warning "Delete it manually if needed."
      ;;
  esac
else
  info "Directory '${E2E_TMPDIR}' not found — nothing to do."
fi

# ── Step 4 (optional): remove e2e Docker images ───────────────────────────────
if [ "$REMOVE_IMAGES" = true ]; then
  step "Removing e2e Docker images..."

  if [ "$DOCKER_AVAILABLE" = true ]; then
    for img in "${CONTROLLER_IMAGE}" "${AGENT_IMAGE}" "${NODE_IMAGE}"; do
      if docker image inspect "${img}" >/dev/null 2>&1; then
        info "Removing image ${img}..."
        docker rmi "${img}" >/dev/null
      else
        info "Image '${img}' not found — skipping."
      fi
    done
  else
    info "Skipping (docker unavailable)."
  fi
else
  info "Skipping image removal (pass --images to also remove e2e Docker images)."
fi

# ── Done ──────────────────────────────────────────────────────────────────────
echo ""
info "${GREEN}${BOLD}e2e teardown complete.${RESET}"
