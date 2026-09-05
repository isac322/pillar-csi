#!/usr/bin/env bash
set -Eeuo pipefail

readonly repo_root=/workspace
readonly image_tag=docker-e2e
readonly controller_image="pillar-csi/controller:${image_tag}"
readonly agent_image="pillar-csi/agent:${image_tag}"
readonly node_image="pillar-csi/node:${image_tag}"
readonly external_agent_image="pillar-csi/external-agent:${image_tag}"
readonly workload_image="busybox:1.38.0"
readonly kind_node_image="kindest/node:v1.36.2"
readonly vg_name="pillar-e2e-vg"
readonly helm_namespace="pillar-csi-system"
readonly helm_release="pillar-csi"

active_cluster=""
active_external_agent=""

log() {
  printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2
}

cleanup_topology() {
  set +e
  if [[ -n "${active_external_agent}" ]]; then
    docker rm -f "${active_external_agent}" >/dev/null 2>&1
    active_external_agent=""
  fi
  if [[ -n "${active_cluster}" ]]; then
    kind delete cluster --name "${active_cluster}" >/dev/null 2>&1
    active_cluster=""
  fi
  set -e
}

collect_diagnostics() {
  set +e
  if [[ -n "${active_cluster}" ]]; then
    log "Diagnostics for ${active_cluster}"
    kubectl get nodes -o wide
    kubectl get pillaragents,pillarstores,pillarprotocols,pillarstorageclasses,pillarvolumestates -A
    kubectl get pods -A -o wide
    kubectl get events -A --sort-by=.lastTimestamp | tail -100
    kubectl -n "${helm_namespace}" logs deployment/pillar-csi-controller --all-containers --tail=300
    kubectl -n "${helm_namespace}" logs daemonset/pillar-csi-node --all-containers --tail=150
    kubectl -n "${helm_namespace}" logs daemonset/pillar-csi-agent --all-containers --tail=150
  fi
  if [[ -n "${active_external_agent}" ]]; then
    docker logs "${active_external_agent}" 2>&1 | tail -300
  fi
  set -e
}

on_exit() {
  rc=$?
  if [[ ${rc} -ne 0 ]]; then
    collect_diagnostics
  fi
  cleanup_topology
  exit "${rc}"
}
trap on_exit EXIT INT TERM

require_linux_storage_stack() {
  log "Loading shared Linux NVMe target and initiator modules"
  missing=()
  for module in nvme_fabrics nvme_tcp nvmet nvmet_tcp; do
    if ! modprobe "${module}" >/dev/null 2>&1; then
      missing+=("${module}")
    fi
  done
  if (( ${#missing[@]} > 0 )); then
    printf 'required kernel modules are unavailable: %s\n' "${missing[*]}" >&2
    printf '%s\n' \
      'Run this harness on a Linux host or Linux VM whose kernel includes CONFIG_NVME_TARGET and CONFIG_NVME_TARGET_TCP.' \
      'Ubuntu/Debian commonly requires: sudo apt-get install linux-modules-extra-$(uname -r)' \
      'Then load: sudo modprobe nvmet nvmet-tcp nvme-fabrics nvme-tcp' >&2
    exit 1
  fi
  if ! mountpoint -q /sys/kernel/config; then
    mount -t configfs none /sys/kernel/config
  fi
  test -c /dev/nvme-fabrics
  test -d /sys/kernel/config/nvmet
}

build_images() {
  log "Building pillar-csi images"
  REGISTRY=pillar-csi TAG="${image_tag}" docker buildx bake --load
  docker build \
    -f "${repo_root}/test/docker-e2e/external-agent.Dockerfile" \
    --build-arg "AGENT_IMAGE=${agent_image}" \
    -t "${external_agent_image}" \
    "${repo_root}"
  docker pull "${workload_image}"
  docker pull "${kind_node_image}"
}

write_kind_config() {
  topology=$1
  config_path=$2
  if [[ "${topology}" == internal ]]; then
    cat >"${config_path}" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    extraMounts:
      - hostPath: /sys/kernel/config
        containerPath: /sys/kernel/config
        propagation: Bidirectional
  - role: worker
  - role: worker
EOF
  else
    cat >"${config_path}" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF
  fi
}

label_nodes() {
  topology=$1
  cluster=$2
  if [[ "${topology}" == internal ]]; then
    storage_node="${cluster}-worker"
    client_a="${cluster}-worker2"
    client_b="${cluster}-worker3"
    kubectl label node "${storage_node}" pillar-csi.bhyoo.com/e2e-role=storage --overwrite >/dev/null
  else
    storage_node=""
    client_a="${cluster}-worker"
    client_b="${cluster}-worker2"
  fi
  kubectl label node "${client_a}" "${client_b}" pillar-csi.bhyoo.com/e2e-role=client --overwrite >/dev/null
  printf '%s\n%s\n%s\n' "${storage_node}" "${client_a}" "${client_b}"
}

setup_internal_backend() {
  storage_node=$1
  log "Creating LVM backend inside ${storage_node}"
  docker exec "${storage_node}" bash -ceu '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends lvm2
    for setting in udev_sync udev_rules obtain_device_list_from_udev; do
      sed -i "s/${setting} = 1/${setting} = 0/" /etc/lvm/lvm.conf
    done
    truncate -s 2G /var/lib/pillar-e2e-lvm.img
    loop_device=$(losetup --find --show /var/lib/pillar-e2e-lvm.img)
    pvcreate --force --yes "$loop_device"
    vgcreate pillar-e2e-vg "$loop_device"
  '
}

start_external_agent() {
  container_name=$1
  log "Starting external storage server ${container_name}"
  docker run -d \
    --name "${container_name}" \
    --hostname "${container_name}" \
    --network kind \
    --privileged \
    -v /dev:/dev \
    -v /lib/modules:/lib/modules:ro \
    -v /sys/kernel/config:/sys/kernel/config \
    -e "PILLAR_E2E_LVM_VG=${vg_name}" \
    "${external_agent_image}" >/dev/null
  ready=false
  for _ in $(seq 1 90); do
    if docker logs "${container_name}" 2>&1 | grep -q 'pillar-agent listening'; then
      ready=true
      break
    fi
    if ! docker inspect -f '{{.State.Running}}' "${container_name}" 2>/dev/null | grep -q true; then
      docker logs "${container_name}" >&2
      return 1
    fi
    sleep 1
  done
  if [[ "${ready}" != true ]]; then
    printf 'external agent did not become ready within 90 seconds\n' >&2
    docker logs "${container_name}" >&2
    return 1
  fi
  docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${container_name}"
}

install_driver() {
  topology=$1
  cluster=$2
  log "Loading images into ${cluster}"
  kind load docker-image --name "${cluster}" "${controller_image}" "${agent_image}" "${node_image}" "${workload_image}"

  helm_args=(
    upgrade --install "${helm_release}" "${repo_root}/charts/pillar-csi"
    --namespace "${helm_namespace}"
    --create-namespace
    --wait
    --timeout 5m
    --set "controller.image.repository=pillar-csi/controller"
    --set "controller.image.tag=${image_tag}"
    --set "controller.image.pullPolicy=Never"
    --set "agent.image.repository=pillar-csi/agent"
    --set "agent.image.tag=${image_tag}"
    --set "agent.image.pullPolicy=Never"
    --set "agent.privileged=true"
    --set "node.image.repository=pillar-csi/node"
    --set "node.image.tag=${image_tag}"
    --set "node.image.pullPolicy=Never"
  )
  if [[ "${topology}" == internal ]]; then
    helm_args+=(--set "agent.backends[0].type=lvm-lv" --set "agent.backends[0].vg=${vg_name}")
  fi
  helm "${helm_args[@]}"
}

apply_storage_resources() {
  topology=$1
  storage_node=$2
  target_address=$3
  if [[ "${topology}" == internal ]]; then
    agent_spec=$(cat <<EOF
  nodeRef:
    name: ${storage_node}
    addressType: InternalIP
    port: 9500
EOF
)
  else
    agent_spec=$(cat <<EOF
  external:
    address: ${target_address}
    port: 9500
EOF
)
  fi

  cat <<EOF | kubectl apply -f -
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
metadata:
  name: pillar-e2e-agent
spec:
${agent_spec}
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: pillar-e2e-store
spec:
  agentRef: pillar-e2e-agent
  backend:
    type: lvm-lv
    lvm:
      volumeGroup: ${vg_name}
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: pillar-e2e-nvme
spec:
  type: nvmeof-tcp
  nvmeofTcp:
    port: 4420
    acl: true
  fsType: ext4
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: pillar-e2e
spec:
  storeRef: pillar-e2e-store
  protocolRef: pillar-e2e-nvme
  storageClass:
    name: pillar-e2e
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
    allowVolumeExpansion: true
EOF

  kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="Ready")].status}'=True \
    pillaragent/pillar-e2e-agent --timeout=3m
  kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="Ready")].status}'=True \
    pillarstorageclass/pillar-e2e --timeout=3m
}

run_tests() {
  topology=$1
  client_a=$2
  client_b=$3
  target_address=$4
  log "Running CSI lifecycle tests for ${topology} topology"
  PILLAR_E2E_TOPOLOGY="${topology}" \
  PILLAR_E2E_STORAGE_CLASS=pillar-e2e \
  PILLAR_E2E_CLIENT_NODE_A="${client_a}" \
  PILLAR_E2E_CLIENT_NODE_B="${client_b}" \
  PILLAR_E2E_TARGET_ADDRESS="${target_address}" \
    go test -tags=docker_e2e -count=1 -timeout=30m -v ./test/docker-e2e
}

run_topology() {
  topology=$1
  cluster="pillar-${topology}"
  config_path="/tmp/${cluster}.yaml"
  cleanup_topology
  active_cluster="${cluster}"

  write_kind_config "${topology}" "${config_path}"
  log "Creating ${topology} Kind cluster ${cluster}"
  kind create cluster --name "${cluster}" --config "${config_path}" --image "${kind_node_image}" --wait 5m

  mapfile -t nodes < <(label_nodes "${topology}" "${cluster}")
  storage_node=${nodes[0]}
  client_a=${nodes[1]}
  client_b=${nodes[2]}

  if [[ "${topology}" == internal ]]; then
    setup_internal_backend "${storage_node}"
    target_address=$(kubectl get node "${storage_node}" -o 'jsonpath={.status.addresses[?(@.type=="InternalIP")].address}')
  else
    active_external_agent="pillar-external-storage"
    target_address=$(start_external_agent "${active_external_agent}")
  fi

  install_driver "${topology}" "${cluster}"
  apply_storage_resources "${topology}" "${storage_node}" "${target_address}"
  run_tests "${topology}" "${client_a}" "${client_b}" "${target_address}"

  log "${topology} topology passed"
  cleanup_topology
}

cd "${repo_root}"
require_linux_storage_stack
build_images
run_topology internal
run_topology external
log "All Docker multi-node E2E scenarios passed"
