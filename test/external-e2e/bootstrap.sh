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
# LVM (not ZFS) for the in-cluster backend.  ZFS userland in the Kind node
# image must match the kernel module version loaded on the runner; LVM has
# no such coupling and Just Works once dm_mod / dm_thin_pool are loaded.
VG_NAME="${VG_NAME:-pillar-vg}"
VG_SIZE="${VG_SIZE:-5G}"
VG_IMG_PATH="/var/lib/${VG_NAME}.img"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONTROL_PLANE="${CLUSTER_NAME}-control-plane"

log() { printf "[bootstrap] %s\n" "$*" >&2; }

# ── 1. Kind cluster ──────────────────────────────────────────────────────────
log "Ensuring Kind cluster ${CLUSTER_NAME} exists"
if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  # Bind-mount the host's /dev/mapper and /sys/kernel/config so LVM commands
  # and the NVMe-oF / iSCSI configfs target setup inside the container can
  # reach the kernel modules already loaded on the runner.
  KIND_CONFIG="$(mktemp)"
  cat > "${KIND_CONFIG}" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /dev/mapper
    containerPath: /dev/mapper
    propagation: Bidirectional
  - hostPath: /sys/kernel/config
    containerPath: /sys/kernel/config
    propagation: Bidirectional
EOF
  kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 5m
  rm -f "${KIND_CONFIG}"
else
  log "  cluster already present, reusing"
fi
kind export kubeconfig --name "${CLUSTER_NAME}"

# ── 2. LVM volume group inside the control-plane node container ─────────────
log "Installing lvm2 + provisioning VG ${VG_NAME} inside ${CONTROL_PLANE}"
docker exec "${CONTROL_PLANE}" bash -c "
  set -e
  export DEBIAN_FRONTEND=noninteractive
  if ! command -v vgcreate >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y -q --no-install-recommends lvm2 thin-provisioning-tools
    # Disable udev integration in LVM — udev is not running inside the
    # container; without these flips pvcreate / vgcreate either fail or
    # warn loudly on every invocation.
    for setting in udev_sync udev_rules obtain_device_list_from_udev; do
      sed -i \"s/\${setting} = 1/\${setting} = 0/\" /etc/lvm/lvm.conf 2>/dev/null || true
    done
  fi
  if ! vgs --noheadings -o vg_name 2>/dev/null | grep -qw '${VG_NAME}'; then
    truncate -s ${VG_SIZE} ${VG_IMG_PATH}
    LOOP=\$(losetup --find --show ${VG_IMG_PATH})
    pvcreate --yes --force \"\${LOOP}\"
    vgcreate ${VG_NAME} \"\${LOOP}\"
  fi
  vgs ${VG_NAME}
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
# Dump every pod / event in the release namespace on Helm failure so the CI
# log captures the actual reason instead of just "context deadline exceeded".
trap 'rc=$?; if [ $rc -ne 0 ]; then
  log "Helm install failed; dumping namespace ${HELM_NAMESPACE} state";
  kubectl -n "${HELM_NAMESPACE}" get pods,events --sort-by=.metadata.creationTimestamp || true;
  for pod in $(kubectl -n "${HELM_NAMESPACE}" get pods -o name 2>/dev/null); do
    log "---- describe ${pod} ----";
    kubectl -n "${HELM_NAMESPACE}" describe "${pod}" || true;
    log "---- logs ${pod} (all containers, last 80 lines) ----";
    kubectl -n "${HELM_NAMESPACE}" logs "${pod}" --all-containers --tail=80 --prefix=true || true;
  done;
fi; exit $rc' EXIT
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
  --set "agent.backends[0].type=lvm-lv" \
  --set "agent.backends[0].vg=${VG_NAME}" \
  --set "controller.extraEnv[0].name=ENABLE_WEBHOOKS" \
  --set "controller.extraEnv[0].value=false"

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
  name: ${VG_NAME}
spec:
  targetRef: pillar-target-default
  backend:
    type: lvm-lv
    lvm:
      volumeGroup: ${VG_NAME}
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
