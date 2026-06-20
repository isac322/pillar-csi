#!/usr/bin/env bash
#
# Bootstrap a Kind cluster with pillar-csi deployed and ready for the upstream
# SIG-Storage External Storage e2e suite.
#
# After successful completion:
#   * Kind cluster ${CLUSTER_NAME} (default: pillar-csi-ext-e2e) exists.
#   * KUBECONFIG points at the new cluster's kubeconfig.
#   * pillar-csi is helm-installed in namespace ${HELM_NAMESPACE}.
#   * A PillarTarget + PillarPool + PillarProtocol CR set is applied, with the
#     PillarTarget reporting condition=Ready.
#
# Required tools on PATH: kind, docker, kubectl, helm.
#
# The script is idempotent: it skips cluster creation and Helm install if the
# named resources already exist.  Teardown is handled by the caller (e.g.
# `kind delete cluster --name ${CLUSTER_NAME}`).

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-pillar-csi-ext-e2e}"
HELM_NAMESPACE="${HELM_NAMESPACE:-pillar-csi-system}"
HELM_RELEASE="${HELM_RELEASE:-pillar-csi}"
IMAGE_TAG="${IMAGE_TAG:-ext-e2e}"
ZFS_POOL_NAME="${ZFS_POOL_NAME:-tank}"
ZFS_POOL_SIZE="${ZFS_POOL_SIZE:-5G}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONTROL_PLANE="${CLUSTER_NAME}-control-plane"

log() { printf "[bootstrap] %s\n" "$*" >&2; }

# ── 1. Kind cluster ──────────────────────────────────────────────────────────
log "Ensuring Kind cluster ${CLUSTER_NAME} exists"
if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  kind create cluster --name "${CLUSTER_NAME}" --wait 5m
else
  log "  cluster already present, reusing"
fi
kind export kubeconfig --name "${CLUSTER_NAME}"

# ── 2. ZFS pool inside the control-plane node container ──────────────────────
log "Installing zfsutils-linux + provisioning pool ${ZFS_POOL_NAME} inside ${CONTROL_PLANE}"
docker exec "${CONTROL_PLANE}" bash -c "
  set -e
  export DEBIAN_FRONTEND=noninteractive
  if ! command -v zpool >/dev/null 2>&1; then
    # zfsutils-linux ships in Debian contrib, not main.  Append 'contrib' to
    # whichever sources file the Kind node image uses (modern deb822 or
    # legacy one-line) rather than dropping a new file: the new file would
    # collide with the signed-by setting of the upstream sources and apt
    # refuses to load conflicting Signed-By values.
    if [ -f /etc/apt/sources.list.d/debian.sources ]; then
      sed -i 's/^\(Components:[[:space:]]*main\)\$/\1 contrib/' /etc/apt/sources.list.d/debian.sources
    fi
    if [ -f /etc/apt/sources.list ]; then
      sed -i 's/\\(^deb .*main\\)\$/\\1 contrib/' /etc/apt/sources.list
    fi
    apt-get update -qq
    apt-get install -y -q --no-install-recommends zfsutils-linux
  fi
  if ! zpool list -H -o name 2>/dev/null | grep -qx '${ZFS_POOL_NAME}'; then
    truncate -s ${ZFS_POOL_SIZE} /var/lib/${ZFS_POOL_NAME}.img
    zpool create -f ${ZFS_POOL_NAME} /var/lib/${ZFS_POOL_NAME}.img
  fi
  zpool list ${ZFS_POOL_NAME}
"

# ── 3. Build + load pillar-csi images ────────────────────────────────────────
log "Building controller / agent / node images at tag ${IMAGE_TAG}"
for target in controller agent node; do
  docker build \
    --target="${target}" \
    --tag="pillar-csi/${target}:${IMAGE_TAG}" \
    "${REPO_ROOT}"
  kind load docker-image "pillar-csi/${target}:${IMAGE_TAG}" --name "${CLUSTER_NAME}"
done

# ── 4. Helm install pillar-csi ──────────────────────────────────────────────
log "Helm-installing release ${HELM_RELEASE} into namespace ${HELM_NAMESPACE}"
helm upgrade --install "${HELM_RELEASE}" "${REPO_ROOT}/charts/pillar-csi" \
  --namespace "${HELM_NAMESPACE}" --create-namespace \
  --wait --timeout 5m \
  --set "controller.image.repository=pillar-csi/controller" \
  --set "controller.image.tag=${IMAGE_TAG}" \
  --set "controller.image.pullPolicy=Never" \
  --set "node.image.repository=pillar-csi/node" \
  --set "node.image.tag=${IMAGE_TAG}" \
  --set "node.image.pullPolicy=Never" \
  --set "agent.image.repository=pillar-csi/agent" \
  --set "agent.image.tag=${IMAGE_TAG}" \
  --set "agent.image.pullPolicy=Never" \
  --set "agent.privileged=true" \
  --set "agent.backends[0].type=zfs-zvol" \
  --set "agent.backends[0].pool=${ZFS_POOL_NAME}"

# ── 5. Apply PillarTarget / PillarPool / PillarProtocol ─────────────────────
log "Applying PillarTarget / PillarPool / PillarProtocol"
kubectl apply -f - <<EOF
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarTarget
metadata:
  name: pillar-target-default
spec:
  nodeRef:
    name: ${CONTROL_PLANE}
    addressType: InternalIP
    port: 9500
---
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarPool
metadata:
  name: ${ZFS_POOL_NAME}
spec:
  targetRef: pillar-target-default
  backend:
    type: zfs-zvol
    zfs:
      pool: ${ZFS_POOL_NAME}
---
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nvmeof-tcp
spec:
  type: nvmeof-tcp
  nvmeofTcp:
    port: 4420
  fsType: ext4
EOF

# ── 6. Wait for PillarTarget Ready ──────────────────────────────────────────
log "Waiting for PillarTarget condition=Ready"
kubectl wait --for=condition=Ready --timeout=2m pillartarget/pillar-target-default

log "Bootstrap complete"
log "  KUBECONFIG=$(kind get kubeconfig --name=${CLUSTER_NAME} 2>/dev/null | head -1 || echo "$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')")"
