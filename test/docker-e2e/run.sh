#!/usr/bin/env bash
set -Eeuo pipefail

readonly repo_root=/workspace
readonly image_tag=docker-e2e
readonly controller_image="pillar-csi/controller:${image_tag}"
readonly agent_image="pillar-csi/agent:${image_tag}"
readonly node_image="pillar-csi/node:${image_tag}"
readonly external_agent_image="pillar-csi/external-agent:${image_tag}"
readonly workload_base_image="busybox:1.38.0"
readonly kind_node_image="kindest/node:v1.36.1"
readonly -a sidecar_images=(
  "registry.k8s.io/sig-storage/csi-provisioner:v6.3.0"
  "registry.k8s.io/sig-storage/csi-attacher:v4.12.0"
  "registry.k8s.io/sig-storage/csi-resizer:v2.2.0"
  "registry.k8s.io/sig-storage/livenessprobe:v2.19.0"
  "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.17.0"
)
readonly -a build_base_images=(
  "golang:1.26-alpine3.24"
  "alpine:3.24"
  "gcr.io/distroless/static:nonroot"
)
readonly vg_name="pillar-e2e-vg"
readonly storage_class="pillar-e2e"
readonly helm_namespace="pillar-csi-system"
readonly helm_release="pillar-csi"
readonly requested_topologies="${PILLAR_E2E_TOPOLOGIES:-internal external}"

active_cluster=""
active_external_agent=""
active_storage_node=""
topology_cleanup_failed=false
diagnostics_collected=false

log() {
  printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2
}

ensure_image() {
  image=$1
  if docker image inspect "${image}" >/dev/null 2>&1; then
    return
  fi
  for attempt in 1 2 3; do
    if docker pull "${image}"; then
      return
    fi
    if [[ "${attempt}" != 3 ]]; then
      log "Pulling ${image} failed; retrying"
      sleep "$((attempt * 5))"
    fi
  done
  return 1
}

run_with_retry() {
  for attempt in 1 2 3; do
    if "$@"; then
      return
    fi
    if [[ "${attempt}" != 3 ]]; then
      log "Command failed; retrying: $*"
      sleep "$((attempt * 5))"
    fi
  done
  return 1
}


cleanup_lvm_backend() {
  container=$1
  if [[ "$(docker inspect -f '{{.State.Running}}' "${container}" 2>/dev/null)" != true ]]; then
    return
  fi
  timeout 20s docker exec "${container}" sh -cu '
    vg=${PILLAR_E2E_LVM_VG:-pillar-e2e-vg}
    backing_files="/var/lib/pillar-e2e-lvm.img /var/lib/pillar-csi/${vg}.img"
    devices=
    rc=0

    if command -v vgs >/dev/null 2>&1 && vgs "${vg}" >/dev/null 2>&1; then
      devices=$(pvs --noheadings -o pv_name --select "vg_name=${vg}" 2>/dev/null)
      if ! vgremove -ff -y "${vg}"; then
        printf "failed to remove volume group %s\n" "${vg}" >&2
        rc=1
      fi
    fi

    for backing_file in ${backing_files}; do
      backing_devices=$(losetup -j "${backing_file}" -O NAME --noheadings 2>/dev/null)
      devices="${devices} ${backing_devices}"
    done
    seen=
    for device in ${devices}; do
      case " ${seen} " in
        *" ${device} "*) continue ;;
      esac
      seen="${seen} ${device}"
      if ! losetup -d "${device}"; then
        printf "failed to detach loop device %s for %s\n" "${device}" "${vg}" >&2
        rc=1
      fi
    done
    exit "${rc}"
  ' >&2
}

cleanup_test_namespaces() {
  local -a cleanup_pvs=()
  local -a pv_resources=()
  local cleanup_rc=0
  local pv
  local pv_output
  if [[ -z "${active_cluster}" ]]; then
    return
  fi
  if pv_output=$(kubectl --request-timeout=10s get pv -o \
    "jsonpath={range .items[?(@.spec.storageClassName==\"${storage_class}\")]}{.metadata.name}{\"\\n\"}{end}" 2>/dev/null); then
    mapfile -t cleanup_pvs <<<"${pv_output}"
  else
    log "Failed to list Docker E2E persistent volumes before namespace cleanup"
    cleanup_rc=1
  fi
  if ! kubectl --request-timeout=10s delete namespace \
    -l pillar-csi.bhyoo.com/docker-e2e=true \
    --wait=true \
    --timeout=60s >/dev/null 2>&1; then
    log "Failed to delete Docker E2E workload namespaces before storage cleanup"
    cleanup_rc=1
  fi
  for pv in "${cleanup_pvs[@]}"; do
    if [[ -n "${pv}" ]]; then
      pv_resources+=("pv/${pv}")
    fi
  done
  if (( ${#pv_resources[@]} > 0 )) && ! kubectl --request-timeout=10s wait \
    --for=delete \
    --timeout=60s \
    "${pv_resources[@]}" >/dev/null 2>&1; then
    log "Persistent volumes were not deleted before storage cleanup: ${cleanup_pvs[*]}"
    cleanup_rc=1
  fi
  return "${cleanup_rc}"
}

cleanup_topology() {
  set +e
  local cleanup_rc=0
  if ! cleanup_test_namespaces; then
    cleanup_rc=1
    collect_diagnostics
    set +e
  fi
  if [[ -n "${active_storage_node}" ]] && ! cleanup_lvm_backend "${active_storage_node}"; then
    log "Failed to remove LVM state from storage node ${active_storage_node}"
    cleanup_rc=1
    collect_diagnostics
    set +e
  fi
  if [[ -n "${active_external_agent}" ]]; then
    if ! cleanup_lvm_backend "${active_external_agent}"; then
      log "Failed to remove LVM state from external agent ${active_external_agent}"
      cleanup_rc=1
      collect_diagnostics
      set +e
    fi
    if docker inspect "${active_external_agent}" >/dev/null 2>&1 &&
      ! timeout 20s docker rm -f "${active_external_agent}" >/dev/null 2>&1; then
      log "Failed to remove external agent ${active_external_agent}"
      cleanup_rc=1
      collect_diagnostics
      set +e
    fi
  fi
  if [[ -n "${active_cluster}" ]] &&
    ! timeout 60s kind delete cluster --name "${active_cluster}" >/dev/null 2>&1; then
    log "Failed to delete Kind cluster ${active_cluster}"
    cleanup_rc=1
    collect_diagnostics
    set +e
  fi
  active_storage_node=""
  active_external_agent=""
  active_cluster=""
  set -e
  return "${cleanup_rc}"
}

collect_diagnostics() {
  if [[ "${diagnostics_collected}" == true ]]; then
    return
  fi
  diagnostics_collected=true
  set +e
  if [[ -n "${active_cluster}" ]]; then
    log "Diagnostics for ${active_cluster}"
    kubectl --request-timeout=3s get nodes -o wide
    kubectl --request-timeout=3s get pillaragents,pillarstores,pillarprotocols,pillarstorageclasses,pillarvolumestates -A
    kubectl --request-timeout=3s get pods -A -o wide
    kubectl --request-timeout=3s get events -A --sort-by=.lastTimestamp | tail -100
    kubectl --request-timeout=3s -n "${helm_namespace}" logs deployment/pillar-csi-controller --all-containers --tail=300
    kubectl --request-timeout=3s -n "${helm_namespace}" logs daemonset/pillar-csi-node --all-containers --tail=150
    kubectl --request-timeout=3s -n "${helm_namespace}" logs daemonset/pillar-csi-agent --all-containers --tail=150
  fi
  if [[ -n "${active_external_agent}" ]]; then
    timeout 5s docker logs "${active_external_agent}" 2>&1 | tail -300
  fi
  set -e
}

on_exit() {
  rc=$?
  if [[ ${rc} -ne 0 ]]; then
    collect_diagnostics
  fi
  local cleanup_rc=0
  cleanup_topology || cleanup_rc=$?
  if [[ ${rc} -eq 0 && "${topology_cleanup_failed}" == true ]]; then
    rc=1
  fi
  if [[ ${rc} -eq 0 && ${cleanup_rc} -ne 0 ]]; then
    rc=${cleanup_rc}
  fi
  exit "${rc}"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

require_linux_storage_stack() {
  log "Loading shared Linux NVMe target and initiator modules"
  missing=()
  if ! findmnt -no OPTIONS /sys | tr ',' '\n' | grep -qx rw; then
    if ! mount -o remount,rw /sys; then
      printf 'failed to remount /sys read-write; NVMe controller teardown requires writable sysfs\n' >&2
      exit 1
    fi
  fi
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
  if [[ ! -d /sys/kernel/config/nvmet ]]; then
    printf '%s\n' \
      'NVMe target configfs is unavailable at /sys/kernel/config/nvmet.' \
      'Mount configfs on the Linux Docker host before running this harness.' >&2
    exit 1
  fi
  if [[ ! -c /dev/nvme-fabrics ]]; then
    if [[ ! -r /sys/class/misc/nvme-fabrics/dev ]]; then
      printf 'nvme_fabrics loaded but /sys/class/misc/nvme-fabrics/dev is unavailable\n' >&2
      exit 1
    fi
    IFS=: read -r nvme_major nvme_minor < /sys/class/misc/nvme-fabrics/dev
    if ! mknod /dev/nvme-fabrics c "${nvme_major}" "${nvme_minor}"; then
      printf 'failed to create /dev/nvme-fabrics from kernel device %s:%s\n' "${nvme_major}" "${nvme_minor}" >&2
      exit 1
    fi
  fi
  test -c /dev/nvme-fabrics
}

build_images() {
  log "Preparing and building pillar-csi images"
  for image in "${build_base_images[@]}"; do
    ensure_image "${image}"
  done
  run_with_retry env \
    BUILDX_NO_DEFAULT_ATTESTATIONS=1 \
    REGISTRY=pillar-csi \
    TAG="${image_tag}" \
    docker buildx bake --load
  run_with_retry docker build \
    --provenance=false \
    -f "${repo_root}/test/docker-e2e/external-agent.Dockerfile" \
    --build-arg "AGENT_IMAGE=${agent_image}" \
    -t "${external_agent_image}" \
    "${repo_root}"
  ensure_image "${workload_base_image}"
  docker build \
    --provenance=false \
    -t "${workload_base_image}" - <<'EOF'
FROM busybox:1.38.0
EOF
  ensure_image "${kind_node_image}"
  for image in "${sidecar_images[@]}"; do
    ensure_image "${image}"
  done
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
    extraMounts:
      - hostPath: /sys/devices/virtual/nvme-fabrics
        containerPath: /sys/devices/virtual/nvme-fabrics
        readOnly: false
        propagation: Bidirectional
  - role: worker
    extraMounts:
      - hostPath: /sys/devices/virtual/nvme-fabrics
        containerPath: /sys/devices/virtual/nvme-fabrics
        readOnly: false
        propagation: Bidirectional
EOF
  else
    cat >"${config_path}" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    extraMounts:
      - hostPath: /sys/devices/virtual/nvme-fabrics
        containerPath: /sys/devices/virtual/nvme-fabrics
        readOnly: false
        propagation: Bidirectional
  - role: worker
    extraMounts:
      - hostPath: /sys/devices/virtual/nvme-fabrics
        containerPath: /sys/devices/virtual/nvme-fabrics
        readOnly: false
        propagation: Bidirectional
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
    for attempt in 1 2 3; do
      if apt-get update -qq && apt-get install -y -qq --no-install-recommends lvm2; then
        break
      fi
      if [[ "${attempt}" == 3 ]]; then
        exit 1
      fi
      sleep "$((attempt * 3))"
    done
    for setting in udev_sync udev_rules obtain_device_list_from_udev; do
      sed -i "s/${setting} = 1/${setting} = 0/" /etc/lvm/lvm.conf
    done
  '
  cleanup_lvm_backend "${storage_node}"
  docker exec "${storage_node}" bash -ceu '
    truncate -s 2G /var/lib/pillar-e2e-lvm.img
    loop_device=$(losetup --find --show /var/lib/pillar-e2e-lvm.img)
    pvcreate --force --yes "$loop_device"
    vgcreate pillar-e2e-vg "$loop_device"
  '
}

start_external_agent() {
  container_name=$1
  log "Starting external storage server ${container_name}"
  if docker inspect "${container_name}" >/dev/null 2>&1; then
    log "Removing stale external storage server ${container_name}"
    if [[ "$(docker inspect -f '{{.State.Running}}' "${container_name}" 2>/dev/null)" == true ]] &&
      ! cleanup_lvm_backend "${container_name}"; then
      log "Failed to clean LVM state from stale external storage server ${container_name}"
      return 1
    fi
    docker rm -f "${container_name}" >/dev/null
  fi
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
  kind load docker-image --name "${cluster}" \
    "${controller_image}" \
    "${agent_image}" \
    "${node_image}" \
    "${workload_base_image}" \
    "${sidecar_images[@]}"

  helm_args=(
    upgrade --install "${helm_release}" "${repo_root}/charts/pillar-csi"
    --namespace "${helm_namespace}"
    --create-namespace
    --wait
    --timeout 15m
    --set "imagePullPolicy=Never"
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
  wait_for_webhook_api
}

wait_for_webhook_api() {
  for attempt in $(seq 1 30); do
    if kubectl apply --dry-run=server -f - >/dev/null 2>&1 <<'EOF'
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: pillar-e2e-webhook-readiness
spec:
  type: nvmeof-tcp
  nvmeofTcp:
    port: 4420
    acl: true
  fsType: ext4
EOF
    then
      return
    fi
    if [[ "${attempt}" != 30 ]]; then
      sleep 2
    fi
  done
  printf 'admission webhook did not become reachable within the readiness window\n' >&2
  return 1
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
  name: ${storage_class}
spec:
  storeRef: pillar-e2e-store
  protocolRef: pillar-e2e-nvme
  storageClass:
    name: ${storage_class}
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
    allowVolumeExpansion: true
EOF

  kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="Ready")].status}'=True \
    pillaragent/pillar-e2e-agent --timeout=3m
  kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="Ready")].status}'=True \
    "pillarstorageclass/${storage_class}" --timeout=3m
}

run_tests() {
  topology=$1
  client_a=$2
  client_b=$3
  target_address=$4
  log "Running CSI lifecycle tests for ${topology} topology"
  PILLAR_E2E_TOPOLOGY="${topology}" \
  PILLAR_E2E_STORAGE_CLASS="${storage_class}" \
  PILLAR_E2E_CLIENT_NODE_A="${client_a}" \
  PILLAR_E2E_CLIENT_NODE_B="${client_b}" \
  PILLAR_E2E_TARGET_ADDRESS="${target_address}" \
    go test -tags=docker_e2e -count=1 -timeout=90m -v ./test/docker-e2e
}

create_kind_cluster() {
  cluster=$1
  config_path=$2

  for attempt in 1 2; do
    if kind create cluster \
      --name "${cluster}" \
      --config "${config_path}" \
      --image "${kind_node_image}" \
      --wait 5m; then
      return
    fi
    if [[ "${attempt}" == 2 ]]; then
      return 1
    fi
    log "Kind cluster creation failed; deleting the partial cluster and retrying"
    kind delete cluster --name "${cluster}" >/dev/null 2>&1
  done
}

wait_for_kubernetes_api() {
  log "Waiting for the ${active_cluster} API server to accept requests"
  for attempt in $(seq 1 30); do
    if kubectl --request-timeout=10s get --raw=/readyz >/dev/null 2>&1; then
      return
    fi
    if [[ "${attempt}" != 30 ]]; then
      sleep 5
    fi
  done
  printf 'Kubernetes API did not become responsive within the readiness window\n' >&2
  return 1
}

run_topology() {
  topology=$1
  cluster="pillar-${topology}"
  config_path="/tmp/${cluster}.yaml"
  cleanup_topology
  diagnostics_collected=false
  active_cluster="${cluster}"

  write_kind_config "${topology}" "${config_path}"
  log "Creating ${topology} Kind cluster ${cluster}"
  create_kind_cluster "${cluster}" "${config_path}"
  wait_for_kubernetes_api

  mapfile -t nodes < <(label_nodes "${topology}" "${cluster}")
  storage_node=${nodes[0]}
  client_a=${nodes[1]}
  client_b=${nodes[2]}

  active_storage_node="${storage_node}"
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
  if ! cleanup_topology; then
    topology_cleanup_failed=true
  fi
}

cd "${repo_root}"
require_linux_storage_stack
build_images
for topology in ${requested_topologies}; do
  case "${topology}" in
    internal | external) run_topology "${topology}" ;;
    *)
      printf 'unsupported topology %q; expected internal and/or external\n' "${topology}" >&2
      exit 2
      ;;
  esac
done
if [[ "${topology_cleanup_failed}" == true ]]; then
  log "Docker E2E scenarios completed, but topology cleanup failed"
  exit 1
fi
log "Requested Docker multi-node E2E scenarios passed: ${requested_topologies}"
