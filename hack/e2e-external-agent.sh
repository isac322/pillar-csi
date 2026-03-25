#!/usr/bin/env bash
# hack/e2e-external-agent.sh
#
# Starts a pillar-agent Docker container OUTSIDE the Kind cluster, connected
# to the Kind cluster's Docker network so it is reachable from within the
# cluster pods (e.g. the controller and node DaemonSet).
#
# This simulates a bare-metal storage node running the gRPC agent without
# being managed as a Kubernetes DaemonSet.  Tests that exercise the "external
# agent" connectivity path use the IP printed at the end of this script to
# configure PillarTarget CRs.
#
# What this script does:
#   1. Validates prerequisites (docker, kind, grpcurl or nc for readiness)
#   2. Verifies the target Kind cluster exists
#   3. Stops/removes any stale container with the same name (idempotent)
#   4. Creates the external-agent configfs simulation directory on the host
#   5. Detects the Kind Docker network name
#   6. Starts the agent container attached to that network
#   7. Waits up to AGENT_READY_TIMEOUT seconds for the gRPC port to be live
#   8. Prints the container's IP on the Kind network (used by e2e tests)
#
# Environment variables (all optional, defaults shown):
#   KIND_CLUSTER            Kind cluster name                (pillar-csi-e2e)
#   AGENT_IMAGE             Agent Docker image               (pillar-csi-agent:e2e)
#   EXTERNAL_AGENT_NAME     Container name                   (pillar-csi-external-agent)
#   EXTERNAL_AGENT_PORT     gRPC listen port inside container(9500)
#   ZFS_POOL                Mock ZFS pool name passed to agent (e2e-pool)
#   E2E_TMPDIR              Host base tmpdir                 (/tmp/pillar-csi-e2e)
#   AGENT_READY_TIMEOUT     Max seconds to wait for port     (30)
#   EXTRA_AGENT_ARGS        Additional flags passed to agent  ("")
#
# Usage:
#   hack/e2e-external-agent.sh
#   AGENT_IMAGE=pillar-csi-agent:dev hack/e2e-external-agent.sh
#   KIND_CLUSTER=my-cluster hack/e2e-external-agent.sh
#
# Output (last line on stdout, prefixed with "EXTERNAL_AGENT_ADDR="):
#   EXTERNAL_AGENT_ADDR=172.18.0.5:9500
#
# The address can be captured by e2e test drivers:
#   ADDR=$(hack/e2e-external-agent.sh | grep ^EXTERNAL_AGENT_ADDR= | cut -d= -f2)

set -euo pipefail

# ── Colour helpers ─────────────────────────────────────────────────────────────
if [ -t 1 ] && command -v tput >/dev/null 2>&1 && tput colors >/dev/null 2>&1; then
  GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3); RED=$(tput setaf 1)
  BOLD=$(tput bold); RESET=$(tput sgr0)
else
  GREEN=""; YELLOW=""; RED=""; BOLD=""; RESET=""
fi

info()    { echo "${GREEN}[e2e-external-agent]${RESET} $*"; }
warning() { echo "${YELLOW}[e2e-external-agent] WARN:${RESET} $*" >&2; }
error()   { echo "${RED}[e2e-external-agent] ERROR:${RESET} $*" >&2; }
step()    { echo "${BOLD}[e2e-external-agent] ──${RESET} $*"; }
die()     { error "$*"; exit 1; }

# ── Configuration ─────────────────────────────────────────────────────────────
KIND_CLUSTER="${KIND_CLUSTER:-pillar-csi-e2e}"
AGENT_IMAGE="${AGENT_IMAGE:-pillar-csi-agent:e2e}"
EXTERNAL_AGENT_NAME="${EXTERNAL_AGENT_NAME:-pillar-csi-external-agent}"
EXTERNAL_AGENT_PORT="${EXTERNAL_AGENT_PORT:-9500}"
ZFS_POOL="${ZFS_POOL:-e2e-pool}"
E2E_TMPDIR="${E2E_TMPDIR:-/tmp/pillar-csi-e2e}"
AGENT_READY_TIMEOUT="${AGENT_READY_TIMEOUT:-30}"
EXTRA_AGENT_ARGS="${EXTRA_AGENT_ARGS:-}"

# Derived paths
EXTERNAL_AGENT_CONFIGFS="${E2E_TMPDIR}/external-agent/configfs"

# ── Prerequisite checks ────────────────────────────────────────────────────────
step "Checking prerequisites..."

if ! command -v docker >/dev/null 2>&1; then
  die "docker is not installed or not in PATH. Please install Docker."
fi

if ! command -v kind >/dev/null 2>&1; then
  die "kind is not installed or not in PATH. Please install Kind."
fi

# Detect a readiness-check tool (prefer nc/netcat; grpcurl is optional).
READINESS_TOOL=""
if command -v nc >/dev/null 2>&1; then
  READINESS_TOOL="nc"
elif command -v grpcurl >/dev/null 2>&1; then
  READINESS_TOOL="grpcurl"
else
  warning "Neither 'nc' nor 'grpcurl' found — readiness check will be skipped."
fi

info "docker    : $(docker --version 2>&1 | head -1)"
info "kind      : $(kind version 2>&1 | head -1)"

# ── Verify cluster exists ──────────────────────────────────────────────────────
step "Verifying Kind cluster '${KIND_CLUSTER}' exists..."

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
  die "Kind cluster '${KIND_CLUSTER}' does not exist. Run hack/e2e-setup.sh first."
fi
info "Cluster '${KIND_CLUSTER}' found."

# ── Remove stale container (idempotent) ───────────────────────────────────────
step "Cleaning up any existing '${EXTERNAL_AGENT_NAME}' container..."

if docker inspect --type container "${EXTERNAL_AGENT_NAME}" >/dev/null 2>&1; then
  info "Stopping and removing stale container '${EXTERNAL_AGENT_NAME}'..."
  docker stop "${EXTERNAL_AGENT_NAME}" >/dev/null 2>&1 || true
  docker rm   "${EXTERNAL_AGENT_NAME}" >/dev/null 2>&1 || true
  info "Stale container removed."
else
  info "No existing container found — nothing to clean up."
fi

# ── Create configfs simulation directory ──────────────────────────────────────
step "Creating external-agent configfs directory '${EXTERNAL_AGENT_CONFIGFS}'..."

mkdir -p "${EXTERNAL_AGENT_CONFIGFS}"
info "Directory created: ${EXTERNAL_AGENT_CONFIGFS}"

# ── Detect Kind Docker network ────────────────────────────────────────────────
step "Detecting Kind Docker network..."

# Kind creates a Docker network named "kind" by default regardless of the
# cluster name.  Verify it exists before attaching our container to it.
KIND_NETWORK=$(docker network ls --filter "name=^kind$" --format "{{.Name}}" 2>/dev/null | head -1)

if [ -z "${KIND_NETWORK}" ]; then
  # Fallback: try <cluster-name> in case a custom network name is in use.
  KIND_NETWORK=$(docker network ls --filter "name=^${KIND_CLUSTER}$" --format "{{.Name}}" 2>/dev/null | head -1)
fi

if [ -z "${KIND_NETWORK}" ]; then
  die "Could not find a Docker network for Kind cluster '${KIND_CLUSTER}'. " \
      "Expected a network named 'kind' or '${KIND_CLUSTER}'."
fi

info "Using Docker network: ${KIND_NETWORK}"

# ── Verify agent image exists ─────────────────────────────────────────────────
step "Verifying agent image '${AGENT_IMAGE}'..."

if ! docker image inspect "${AGENT_IMAGE}" >/dev/null 2>&1; then
  die "Agent image '${AGENT_IMAGE}' not found in local Docker daemon. " \
      "Run hack/e2e-setup.sh to build and load images."
fi
info "Image '${AGENT_IMAGE}' found."

# ── Start the external agent container ────────────────────────────────────────
step "Starting external agent container '${EXTERNAL_AGENT_NAME}'..."

# Security context notes:
#   --cap-add SYS_ADMIN  — required by ZFS userspace (zpool/zfs commands) and
#                          NVMe-oF configfs writes; mirrors the K8s DaemonSet
#                          manifest's securityContext.capabilities.
#   --security-opt no-new-privileges:true — defence-in-depth (no SUID escalation)
#   --network kind       — connects the container to the same Layer-2 segment as
#                          the Kind node containers so cluster pods can reach it
#                          via the container IP without NAT.
#   --publish            — also expose the gRPC port to the host so local
#                          grpcurl / integration tests can reach the container
#                          without going through the Kind network.
#
# The --configfs-root flag points to our simulated configfs directory rather
# than /sys/kernel/config, which avoids the need for nvmet/nvmet_tcp kernel
# modules on the CI host while still exercising all configfs-path code paths.

# Build agent command-line arguments.
AGENT_CMD_ARGS=(
  "--listen-address=:${EXTERNAL_AGENT_PORT}"
  "--zfs-pool=${ZFS_POOL}"
  "--configfs-root=/sys/kernel/config"
)

# Append any caller-supplied extra flags (may be empty).
if [ -n "${EXTRA_AGENT_ARGS}" ]; then
  # Word-split intentional here; caller is responsible for quoting.
  # shellcheck disable=SC2206
  AGENT_CMD_ARGS+=( ${EXTRA_AGENT_ARGS} )
fi

docker run \
  --detach \
  --name  "${EXTERNAL_AGENT_NAME}" \
  --network "${KIND_NETWORK}" \
  --publish "${EXTERNAL_AGENT_PORT}:${EXTERNAL_AGENT_PORT}" \
  --cap-add SYS_ADMIN \
  --security-opt no-new-privileges:true \
  --volume "${EXTERNAL_AGENT_CONFIGFS}:/sys/kernel/config" \
  --restart no \
  "${AGENT_IMAGE}" \
  "${AGENT_CMD_ARGS[@]}" \
  >/dev/null

info "Container '${EXTERNAL_AGENT_NAME}' started."

# ── Wait for gRPC port to be reachable ────────────────────────────────────────
step "Waiting for gRPC port ${EXTERNAL_AGENT_PORT} to be live (timeout: ${AGENT_READY_TIMEOUT}s)..."

# Obtain the container's IP on the Kind network immediately after start.
# We look up the IP address of our container within the Kind Docker network
# by inspecting the container's network settings for the specific network.
#
# Strategy (in order):
#   1. Use docker network inspect to list container IPs for our network,
#      then filter for our container name/ID.
#   2. Fallback: grab any non-empty IP from the container's network settings.
_get_container_ip() {
  local net="$1" cname="$2"
  # docker network inspect returns a JSON array of containers attached to the
  # network, each with an IPv4Address field.  We filter by container name.
  docker network inspect "${net}" \
    --format '{{range $id, $c := .Containers}}{{$c.Name}} {{$c.IPv4Address}}{{"\n"}}{{end}}' \
    2>/dev/null \
    | grep "^${cname} " \
    | awk '{print $2}' \
    | cut -d/ -f1  # strip CIDR suffix (e.g. 172.18.0.5/16 → 172.18.0.5)
}

CONTAINER_IP=$(_get_container_ip "${KIND_NETWORK}" "${EXTERNAL_AGENT_NAME}")

# Fallback: use the first non-empty IP from the container's own network settings.
if [ -z "${CONTAINER_IP}" ]; then
  CONTAINER_IP=$(docker inspect \
    --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{"\n"}}{{end}}' \
    "${EXTERNAL_AGENT_NAME}" 2>/dev/null \
    | grep -v '^$' | head -1)
fi

if [ -z "${CONTAINER_IP}" ]; then
  die "Could not determine container IP for '${EXTERNAL_AGENT_NAME}'."
fi

info "Container IP on '${KIND_NETWORK}' network: ${CONTAINER_IP}"

# Poll the TCP port for readiness.
READY=false
deadline=$(( $(date +%s) + AGENT_READY_TIMEOUT ))

while [ "$(date +%s)" -lt "${deadline}" ]; do
  # Check whether the container is still running (fast-fail on crash-loop).
  CONTAINER_STATUS=$(docker inspect --format '{{.State.Status}}' "${EXTERNAL_AGENT_NAME}" 2>/dev/null || echo "gone")
  if [ "${CONTAINER_STATUS}" != "running" ]; then
    error "Container '${EXTERNAL_AGENT_NAME}' is not running (status: ${CONTAINER_STATUS})."
    error "Last 20 lines of container log:"
    docker logs --tail 20 "${EXTERNAL_AGENT_NAME}" >&2 2>/dev/null || true
    die "External agent container exited prematurely."
  fi

  # TCP reachability check using the chosen tool.
  case "${READINESS_TOOL}" in
    nc)
      # -z: scan-only (no data), -w1: 1-second timeout.
      # On macOS nc uses -G for connect timeout; -w is supported on both.
      if nc -z -w1 "${CONTAINER_IP}" "${EXTERNAL_AGENT_PORT}" >/dev/null 2>&1; then
        READY=true
        break
      fi
      ;;
    grpcurl)
      # grpcurl list with a very short deadline; ignore TLS errors (plaintext).
      if grpcurl -plaintext -connect-timeout 1 \
          "${CONTAINER_IP}:${EXTERNAL_AGENT_PORT}" list >/dev/null 2>&1; then
        READY=true
        break
      fi
      ;;
    "")
      # No readiness tool — use a simple /dev/tcp fallback (bash built-in).
      if (: </dev/tcp/"${CONTAINER_IP}"/"${EXTERNAL_AGENT_PORT}") 2>/dev/null; then
        READY=true
        break
      fi
      ;;
  esac

  sleep 1
done

if [ "${READY}" = false ]; then
  error "External agent did not become ready within ${AGENT_READY_TIMEOUT} seconds."
  error "Container logs:"
  docker logs --tail 30 "${EXTERNAL_AGENT_NAME}" >&2 2>/dev/null || true
  die "Timed out waiting for agent to accept connections on ${CONTAINER_IP}:${EXTERNAL_AGENT_PORT}"
fi

info "External agent is ready."

# ── Emit summary ──────────────────────────────────────────────────────────────
echo ""
info "${GREEN}${BOLD}External agent started successfully.${RESET}"
info "  Container name : ${EXTERNAL_AGENT_NAME}"
info "  Image          : ${AGENT_IMAGE}"
info "  Kind network   : ${KIND_NETWORK}"
info "  gRPC address   : ${CONTAINER_IP}:${EXTERNAL_AGENT_PORT}"
info "  configfs root  : ${EXTERNAL_AGENT_CONFIGFS} → /sys/kernel/config"
info "  ZFS pool arg   : ${ZFS_POOL}"
echo ""
info "To stop the container:  docker stop ${EXTERNAL_AGENT_NAME} && docker rm ${EXTERNAL_AGENT_NAME}"
info "Or run:                 hack/e2e-teardown.sh"
echo ""

# Machine-readable output on stdout (last line) so scripts can capture it:
#   ADDR=$(hack/e2e-external-agent.sh 2>/dev/null | grep ^EXTERNAL_AGENT_ADDR= | cut -d= -f2)
echo "EXTERNAL_AGENT_ADDR=${CONTAINER_IP}:${EXTERNAL_AGENT_PORT}"
