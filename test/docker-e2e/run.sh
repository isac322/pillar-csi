#!/usr/bin/env bash
set -Eeuo pipefail

readonly repo_root=/workspace
readonly image_tag=docker-e2e
readonly controller_image="pillar-csi/controller:${image_tag}"
readonly agent_image="pillar-csi/agent:${image_tag}"
readonly node_image="pillar-csi/node:${image_tag}"
readonly external_agent_image="pillar-csi/external-agent:${image_tag}"
readonly workload_base_image="busybox:1.38.0"
readonly kind_node_image="kindest/node:v1.37.0"
readonly -a sidecar_images=(
  "registry.k8s.io/sig-storage/csi-provisioner:v6.3.0"
  "registry.k8s.io/sig-storage/csi-attacher:v4.12.0"
  "registry.k8s.io/sig-storage/csi-resizer:v2.2.1"
  "registry.k8s.io/sig-storage/livenessprobe:v2.19.0"
  "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.17.0"
)
readonly -a build_base_images=(
  "golang:1.27-alpine3.24"
  "alpine:3.24"
  "gcr.io/distroless/static:nonroot"
)
readonly vg_name="pillar-e2e-vg"
readonly internal_backing_file="/var/lib/pillar-e2e-lvm.img"
readonly external_backing_file="/var/lib/pillar-csi/${vg_name}.img"
readonly nvmeof_port=4420
readonly iscsi_port=3260
readonly iscsi_protocol="pillar-e2e-iscsi"
readonly iscsi_storage_class="pillar-e2e-iscsi"
readonly iscsi_xfs_storage_class="pillar-e2e-iscsi-xfs"
# pillar-csi names every LIO target iqn.2026-01.com.bhyoo.pillar-csi:<volume ID
# with "/" replaced by ".">; the E2E volumes live in ${vg_name}.
readonly iscsi_owned_iqn_prefix="iqn.2026-01.com.bhyoo.pillar-csi:${vg_name}."
# NETLINK_ISCSI exists only in the host init network namespace.  Compose
# bind-mounts the host /proc at /host/proc; the Kind worker nodes receive the
# same mount so pillar-node can open the netlink socket there.
readonly host_proc=/host/proc
readonly host_init_netns="${host_proc}/1/ns/net"
readonly iscsi_cleanup_bin=/tmp/pillar-e2e-iscsi-session-cleanup
readonly storage_class="pillar-e2e"
readonly local_storage_class="pillar-e2e-local"
readonly helm_namespace="pillar-csi-system"
readonly helm_release="pillar-csi"
readonly requested_topologies="${PILLAR_E2E_TOPOLOGIES:-internal external}"

active_cluster=""
active_external_agent=""
active_storage_node=""
active_target_address=""
active_host_nqns=()
# Whether the LIO iSCSI fabric directory and pillar-csi's iblock HBA existed
# before this harness ran; cleanup removes only what the run created.
lio_iscsi_fabric_preexisting=false
lio_iblock_hba_preexisting=false
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
stable_port_id() {
  local address=$1
  local port=$2
  local hash=2166136261
  local index
  local byte
  for ((index = 0; index < ${#address}; index++)); do
    printf -v byte '%d' "'${address:index:1}"
    hash=$(( ((hash ^ byte) * 16777619) & 0xffffffff ))
  done
  hash=$(( ((hash ^ port) * 16777619) & 0xffffffff ))
  printf '%d\n' "$((hash % 65535 + 1))"
}
capture_active_nvme_host_nqns() {
  active_host_nqns=()
  local node
  local nqn
  local existing
  for node in "$@"; do
    if ! nqn=$(docker exec "${node}" cat /etc/nvme/hostnqn 2>&1); then
      printf 'failed to read NVMe host NQN from Kind node %s: %s\n' "${node}" "${nqn}" >&2
      return 1
    fi
    if [[ "${nqn}" != nqn.2014-08.org.nvmexpress:uuid:?* ]]; then
      printf 'invalid NVMe host NQN from Kind node %s: %q\n' "${node}" "${nqn}" >&2
      return 1
    fi
    for existing in "${active_host_nqns[@]}"; do
      if [[ "${existing}" == "${nqn}" ]]; then
        printf 'Kind nodes reuse NVMe host NQN %s; initiator isolation is invalid\n' "${nqn}" >&2
        return 1
      fi
    done
    active_host_nqns+=("${nqn}")
  done
}



# cleanup_iscsi_sessions tears down every host iSCSI initiator session to an
# E2E-owned target.  Kind nodes share the host kernel, so sessions opened by
# pillar-node outlive a deleted cluster.
cleanup_iscsi_sessions() {
  if [[ ! -x "${iscsi_cleanup_bin}" ]]; then
    printf 'iSCSI session cleanup helper %s is missing\n' "${iscsi_cleanup_bin}" >&2
    return 1
  fi
  if ! timeout --kill-after=2s 60s "${iscsi_cleanup_bin}" \
    --netns "${host_init_netns}" --target-prefix "${iscsi_owned_iqn_prefix}"; then
    printf 'failed to tear down host iSCSI sessions to targets %s*\n' "${iscsi_owned_iqn_prefix}" >&2
    return 1
  fi
}

cleanup_host_storage_state() {
  log "Removing host-global storage state owned by Docker E2E"
  local cleanup_rc=0
  local iscsi_rc=0
  local nvme_port_id=""
  if [[ -n "${active_target_address}" ]]; then
    nvme_port_id=$(stable_port_id "${active_target_address}" "${nvmeof_port}")
  fi
  # Initiator sessions first: a live session keeps the LIO target's TPG busy.
  cleanup_iscsi_sessions || iscsi_rc=1
  timeout --kill-after=2s 30s bash -u -o pipefail -c '
    vg=$1
    internal_backing_file=$2
    external_backing_file=$3
    nvme_port_id=$4
    lio_iscsi_fabric_preexisting=$5
    lio_iblock_hba_preexisting=$6
    nvmet_root=/sys/kernel/config/nvmet
    subsystems_root=${nvmet_root}/subsystems
    ports_root=${nvmet_root}/ports
    logical_nqn_prefix="nqn.2026-01.com.bhyoo.pillar-csi:${vg}/"
    # volumeTargetID canonicalizes the volume ID slash to a dot in configfs.
    configfs_nqn_prefix=${logical_nqn_prefix%/}.
    lvm_config="devices/obtain_device_list_from_udev=0 activation/udev_sync=0 activation/udev_rules=0"
    rc=0
    affected_ports=()
    loop_devices=()
    host_dirs=()
    shift 6
    requested_host_nqns=("$@")
    shopt -s nullglob

    report_failure() {
      local operation=$1
      local target=$2
      local reason=$3
      if [[ -z "${reason}" ]]; then
        reason="command exited nonzero"
      fi
      printf "failed to %s %s: %s\n" "${operation}" "${target}" "${reason}" >&2
      rc=1
    }

    remember_loop_device() {
      local candidate=$1
      local existing
      [[ "${candidate}" == /dev/loop* ]] || return
      for existing in "${loop_devices[@]}"; do
        [[ "${existing}" == "${candidate}" ]] && return
      done
      loop_devices+=("${candidate}")
    }
    remember_host_dir() {
      local candidate=$1
      local existing
      [[ "${candidate}" == "${nvmet_root}/hosts/"?* ]] || {
        report_failure "track NVMe host directory outside configfs host root" "${candidate}" "${nvmet_root}/hosts"
        return
      }
      for existing in "${host_dirs[@]}"; do
        [[ "${existing}" == "${candidate}" ]] && return
      done
      host_dirs+=("${candidate}")
    }
    for host_nqn in "${requested_host_nqns[@]}"; do
      remember_host_dir "${nvmet_root}/hosts/${host_nqn}"
    done


    for subsystem_dir in "${subsystems_root}/${configfs_nqn_prefix}"*; do
      [[ -d "${subsystem_dir}" ]] || continue
      nqn=${subsystem_dir##*/}
      [[ "${nqn}" == "${configfs_nqn_prefix}"?* ]] || continue

      for port_dir in "${ports_root}"/*; do
        [[ -d "${port_dir}" ]] || continue
        link_path="${port_dir}/subsystems/${nqn}"
        if [[ -L "${link_path}" ]]; then
          if error=$(rm -- "${link_path}" 2>&1); then
            affected_ports+=("${port_dir}")
          else
            report_failure "remove NVMe port symlink" "${link_path}" "${error}"
          fi
        elif [[ -e "${link_path}" ]]; then
          report_failure "remove NVMe port symlink" "${link_path}" "path exists but is not a symbolic link"
        fi
      done

      for namespace_dir in "${subsystem_dir}/namespaces/"*; do
        [[ -d "${namespace_dir}" ]] || continue
        enable_path="${namespace_dir}/enable"
        if [[ -e "${enable_path}" ]]; then
          if ! error=$(sh -c "printf 0 >\"\$1\"" pillar-e2e-disable "${enable_path}" 2>&1); then
            report_failure "disable NVMe namespace through" "${enable_path}" "${error}"
          fi
        fi
        if ! error=$(rmdir -- "${namespace_dir}" 2>&1); then
          report_failure "remove NVMe namespace directory" "${namespace_dir}" "${error}"
        fi
      done

      for acl_link in "${subsystem_dir}/allowed_hosts/"*; do
        if [[ -L "${acl_link}" ]]; then
          if host_dir=$(readlink -f -- "${acl_link}" 2>&1); then
            remember_host_dir "${host_dir}"
          else
            report_failure "resolve NVMe ACL host target" "${acl_link}" "${host_dir}"
          fi
          if ! error=$(rm -- "${acl_link}" 2>&1); then
            report_failure "remove NVMe ACL symlink" "${acl_link}" "${error}"
          fi
        elif [[ -e "${acl_link}" ]]; then
          report_failure "remove NVMe ACL symlink" "${acl_link}" "path exists but is not a symbolic link"
        fi
      done

      if ! error=$(rmdir -- "${subsystem_dir}" 2>&1); then
        report_failure "remove NVMe subsystem directory" "${subsystem_dir}" "${error}"
      fi
    done
    for host_dir in "${host_dirs[@]}"; do
      still_referenced=false
      for remaining_acl in "${subsystems_root}"/*/allowed_hosts/*; do
        [[ -L "${remaining_acl}" ]] || continue
        if remaining_host=$(readlink -f -- "${remaining_acl}" 2>&1); then
          if [[ "${remaining_host}" == "${host_dir}" ]]; then
            still_referenced=true
            break
          fi
        else
          report_failure "resolve remaining NVMe ACL host target" "${remaining_acl}" "${remaining_host}"
        fi
      done
      [[ "${still_referenced}" == true || ! -d "${host_dir}" ]] && continue
      if ! error=$(rmdir -- "${host_dir}" 2>&1); then
        report_failure "remove unreferenced NVMe host directory" "${host_dir}" "${error}"
      fi
    done

    # Include the current topology listener even when CSI teardown removed the
    # last E2E subsystem before this cleanup observed its symlink.
    if [[ -n "${nvme_port_id}" ]]; then
      affected_ports+=("${ports_root}/${nvme_port_id}")
    fi

    checked_ports=()
    for port_dir in "${affected_ports[@]}"; do
      already_checked=false
      for checked_port in "${checked_ports[@]}"; do
        if [[ "${checked_port}" == "${port_dir}" ]]; then
          already_checked=true
          break
        fi
      done
      [[ "${already_checked}" == true ]] && continue
      checked_ports+=("${port_dir}")
      [[ -d "${port_dir}" ]] || continue
      remaining_links=("${port_dir}/subsystems/"*)
      if (( ${#remaining_links[@]} == 0 )); then
        if ! error=$(rmdir -- "${port_dir}" 2>&1); then
          report_failure "remove empty NVMe listener port" "${port_dir}" "${error}"
        fi
      fi
    done

    # LIO iSCSI targets of E2E volumes (lio.Target.Remove order): disable the
    # TPG, drop node ACLs with their mapped LUNs, network portals and LUNs,
    # then the TPG, the target and its iblock backstore.  A backstore holds
    # the LV open, so this must precede vgremove.
    lio_root=/sys/kernel/config/target
    iscsi_root=${lio_root}/iscsi
    iblock_hba=${lio_root}/core/iblock_3260
    owned_iqn_prefix="iqn.2026-01.com.bhyoo.pillar-csi:${vg}."

    remove_symlinks_in() {
      local dir=$1
      local entry
      for entry in "${dir}"/*; do
        [[ -L "${entry}" ]] || continue
        if ! error=$(rm -- "${entry}" 2>&1); then
          report_failure "remove LIO symlink" "${entry}" "${error}"
        fi
      done
    }
    remove_lio_dir() {
      local dir=$1
      local operation=$2
      [[ -d "${dir}" ]] || return 0
      if ! error=$(rmdir -- "${dir}" 2>&1); then
        report_failure "${operation}" "${dir}" "${error}"
      fi
    }

    for target_dir in "${iscsi_root}/${owned_iqn_prefix}"*; do
      [[ -d "${target_dir}" ]] || continue
      for tpg_dir in "${target_dir}"/tpgt_*; do
        [[ -d "${tpg_dir}" ]] || continue
        if [[ -e "${tpg_dir}/enable" ]]; then
          if ! error=$(sh -c "printf 0 > \"\$1\"" pillar-e2e-disable "${tpg_dir}/enable" 2>&1); then
            report_failure "disable LIO TPG through" "${tpg_dir}/enable" "${error}"
          fi
        fi
        for acl_dir in "${tpg_dir}"/acls/*; do
          [[ -d "${acl_dir}" ]] || continue
          for mapped_dir in "${acl_dir}"/lun_*; do
            [[ -d "${mapped_dir}" ]] || continue
            remove_symlinks_in "${mapped_dir}"
            remove_lio_dir "${mapped_dir}" "remove LIO mapped LUN"
          done
          remove_lio_dir "${acl_dir}" "remove LIO node ACL"
        done
        for portal_dir in "${tpg_dir}"/np/*; do
          remove_lio_dir "${portal_dir}" "remove LIO network portal"
        done
        for lun_dir in "${tpg_dir}"/lun/lun_*; do
          [[ -d "${lun_dir}" ]] || continue
          remove_symlinks_in "${lun_dir}"
          remove_lio_dir "${lun_dir}" "remove LIO LUN"
        done
        remove_lio_dir "${tpg_dir}" "remove LIO TPG"
      done
      remove_lio_dir "${target_dir}" "remove LIO iSCSI target"
    done
    # Backstores are named after the target IQN without the owned prefix,
    # i.e. "<vg>.<volume>"; this also catches a backstore whose target a
    # previous interrupted run already removed.
    for backstore_dir in "${iblock_hba}/${vg}."*; do
      remove_lio_dir "${backstore_dir}" "remove LIO iblock backstore"
    done

    remaining_targets=("${iscsi_root}/${owned_iqn_prefix}"*)
    if (( ${#remaining_targets[@]} > 0 )); then
      report_failure "verify LIO iSCSI targets removed" "${iscsi_root}" "remaining: ${remaining_targets[*]}"
    fi
    if [[ "${lio_iblock_hba_preexisting}" != true && -d "${iblock_hba}" ]]; then
      remaining_backstores=()
      for entry in "${iblock_hba}"/*; do
        [[ -d "${entry}" ]] && remaining_backstores+=("${entry}")
      done
      if (( ${#remaining_backstores[@]} == 0 )); then
        remove_lio_dir "${iblock_hba}" "remove pillar-csi LIO iblock HBA"
      fi
    fi
    if [[ "${lio_iscsi_fabric_preexisting}" != true && -d "${iscsi_root}" ]]; then
      remaining_iscsi_targets=()
      for entry in "${iscsi_root}"/iqn.*; do
        [[ -d "${entry}" ]] && remaining_iscsi_targets+=("${entry}")
      done
      if (( ${#remaining_iscsi_targets[@]} == 0 )); then
        remove_lio_dir "${iscsi_root}" "remove LIO iSCSI fabric"
      fi
    fi

    # A local attach leaves a device-mapper linear target pillar-local-<hash>
    # over an E2E logical volume while the volume is staged on the storage
    # node.  Remove any left behind by an interrupted run; it would otherwise
    # hold the LV open and make vgremove fail.
    lv_dm_prefix="${vg//-/--}-"
    for dm_name_file in /sys/class/block/dm-*/dm/name; do
      [[ -r "${dm_name_file}" ]] || continue
      dm_name=$(<"${dm_name_file}")
      [[ "${dm_name}" == pillar-local-* ]] || continue
      dm_dir=${dm_name_file%/dm/name}
      owned=false
      for slave_name_file in "${dm_dir}"/slaves/*/dm/name; do
        [[ -r "${slave_name_file}" ]] || continue
        [[ "$(<"${slave_name_file}")" == "${lv_dm_prefix}"* ]] && owned=true
      done
      [[ "${owned}" == true ]] || continue
      if ! command -v dmsetup >/dev/null 2>&1; then
        report_failure "remove local attach device-mapper target" "${dm_name}" "dmsetup is not installed"
        continue
      fi
      if ! error=$(dmsetup remove --noudevsync "${dm_name}" 2>&1); then
        report_failure "remove local attach device-mapper target" "${dm_name}" "${error}"
      fi
    done

    vg_output=
    if ! vg_output=$(vgs --config "${lvm_config}" --noheadings -o vg_name 2>&1); then
      report_failure "list volume groups while locating" "${vg}" "${vg_output}"
    else
      vg_present=false
      while read -r candidate_vg; do
        [[ "${candidate_vg}" == "${vg}" ]] && vg_present=true
      done <<<"${vg_output}"
      if [[ "${vg_present}" == true ]]; then
        pv_output=
        if ! pv_output=$(pvs --config "${lvm_config}" --noheadings -o pv_name --select "vg_name=${vg}" 2>&1); then
          report_failure "list physical volumes for volume group" "${vg}" "${pv_output}"
        else
          while read -r device; do
            remember_loop_device "${device}"
          done <<<"${pv_output}"
        fi
        if ! error=$(vgremove --config "${lvm_config}" -ff -y "${vg}" 2>&1); then
          report_failure "remove volume group" "${vg}" "${error}"
        fi
      fi
    fi

    loop_output=
    if ! loop_output=$(losetup --list --noheadings --raw --output NAME,BACK-FILE 2>&1); then
      report_failure "list loop devices for E2E backing files" "${internal_backing_file},${external_backing_file}" "${loop_output}"
    else
      while read -r device backing_file; do
        backing_file=${backing_file% (deleted)}
        case "${backing_file}" in
          "${internal_backing_file}" | *"${internal_backing_file}" | \
            "${external_backing_file}" | *"${external_backing_file}")
            remember_loop_device "${device}"
            ;;
        esac
      done <<<"${loop_output}"
    fi

    for device in "${loop_devices[@]}"; do
      if ! error=$(losetup -d "${device}" 2>&1); then
        report_failure "detach E2E loop device" "${device}" "${error}"
      fi
    done
    exit "${rc}"
  ' pillar-e2e-cleanup "${vg_name}" "${internal_backing_file}" "${external_backing_file}" "${nvme_port_id}" \
    "${lio_iscsi_fabric_preexisting}" "${lio_iblock_hba_preexisting}" \
    "${active_host_nqns[@]}" || cleanup_rc=$?
  if [[ ${cleanup_rc} -eq 124 || ${cleanup_rc} -eq 137 ]]; then
    printf 'host storage cleanup for volume group %s exceeded its 32-second termination budget\n' "${vg_name}" >&2
  elif [[ ${cleanup_rc} -ne 0 && ${cleanup_rc} -ne 1 ]]; then
    printf 'host storage cleanup for volume group %s failed with exit status %d\n' \
      "${vg_name}" "${cleanup_rc}" >&2
  fi
  if [[ ${cleanup_rc} -eq 0 && ${iscsi_rc} -ne 0 ]]; then
    cleanup_rc=1
  fi
  return "${cleanup_rc}"
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
  if pv_output=$(kubectl --request-timeout=5s get pv -o \
    "jsonpath={range .items[?(@.spec.storageClassName==\"${storage_class}\")]}{.metadata.name}{\"\\n\"}{end}" 2>/dev/null); then
    mapfile -t cleanup_pvs <<<"${pv_output}"
  else
    log "Failed to list Docker E2E persistent volumes before namespace cleanup"
    cleanup_rc=1
  fi
  if ! kubectl --request-timeout=5s delete namespace \
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
  if (( ${#pv_resources[@]} > 0 )) && ! kubectl --request-timeout=5s wait \
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
  # Kubernetes cleanup 125s + diagnostics 42s + container/Kind removal 84s +
  # host-global storage cleanup 32s = 283s.
  if ! cleanup_test_namespaces; then
    cleanup_rc=1
    collect_diagnostics
    set +e
  fi
  if [[ -n "${active_external_agent}" ]] &&
    docker inspect "${active_external_agent}" >/dev/null 2>&1; then
    if ! error=$(timeout --kill-after=2s 20s docker rm -f "${active_external_agent}" 2>&1); then
      printf 'failed to remove external agent %s: %s\n' \
        "${active_external_agent}" "${error:-docker rm exited nonzero}" >&2
      cleanup_rc=1
      collect_diagnostics
      set +e
    fi
  fi
  if [[ -n "${active_cluster}" ]]; then
    if ! error=$(timeout --kill-after=2s 60s kind delete cluster --name "${active_cluster}" 2>&1); then
      printf 'failed to delete Kind cluster %s: %s\n' \
        "${active_cluster}" "${error:-kind delete exited nonzero}" >&2
      cleanup_rc=1
      collect_diagnostics
      set +e
    fi
  fi
  if ! cleanup_host_storage_state; then
    cleanup_rc=1
    collect_diagnostics
    set +e
  fi
  active_storage_node=""
  active_external_agent=""
  active_cluster=""
  active_target_address=""
  active_host_nqns=()
  set -e
  return "${cleanup_rc}"
}

collect_diagnostics() {
  if [[ "${diagnostics_collected}" == true ]]; then
    return
  fi
  diagnostics_collected=true
  local diagnostics_rc=0
  timeout --kill-after=2s 40s bash -u -o pipefail -c '
    active_cluster=$1
    helm_namespace=$2
    active_external_agent=$3
    if [[ -n "${active_cluster}" ]]; then
      printf "\n[%s] Diagnostics for %s\n" "$(date -u +%H:%M:%S)" "${active_cluster}" >&2
      kubectl --request-timeout=5s get nodes -o wide
      kubectl --request-timeout=5s get pillaragents,pillarstores,pillarprotocols,pillarstorageclasses,pillarvolumestates -A
      kubectl --request-timeout=5s get pods -A -o wide
      kubectl --request-timeout=5s get events -A --sort-by=.lastTimestamp | tail -100
      kubectl --request-timeout=5s -n "${helm_namespace}" logs deployment/pillar-csi-controller --all-containers --tail=300
      kubectl --request-timeout=5s -n "${helm_namespace}" logs daemonset/pillar-csi-node --all-containers --tail=150
      kubectl --request-timeout=5s -n "${helm_namespace}" logs daemonset/pillar-csi-agent --all-containers --tail=150
    fi
    if [[ -n "${active_external_agent}" ]]; then
      timeout --kill-after=2s 5s docker logs "${active_external_agent}" 2>&1 | tail -300
    fi
    # iSCSI state is host-global: LIO targets in configfs and initiator
    # sessions in sysfs.
    printf "\n[%s] LIO iSCSI targets and host iSCSI sessions\n" "$(date -u +%H:%M:%S)" >&2
    find /sys/kernel/config/target/iscsi -mindepth 1 -maxdepth 5 \( -path "*/tpgt_*/acls/*" -o -path "*/tpgt_*/np/*" -o -path "*/tpgt_*/lun/*" -o -name "tpgt_*" -o -name "iqn.*" \) -print 2>&1 | head -200
    for session in /sys/class/iscsi_session/session*; do
      [[ -d "${session}" ]] || continue
      printf "%s target=%s initiator=%s state=%s\n" "${session##*/}" \
        "$(cat "${session}/targetname" 2>&1)" "$(cat "${session}/initiatorname" 2>&1)" "$(cat "${session}/state" 2>&1)"
    done
  ' pillar-e2e-diagnostics "${active_cluster}" "${helm_namespace}" "${active_external_agent}" ||
    diagnostics_rc=$?
  if [[ ${diagnostics_rc} -eq 124 || ${diagnostics_rc} -eq 137 ]]; then
    printf 'diagnostic collection exceeded its 42-second termination budget\n' >&2
  fi
  return 0
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
  log "Loading shared Linux storage modules"
  missing=()
  if ! findmnt -no OPTIONS /sys | tr ',' '\n' | grep -qx rw; then
    if ! mount -o remount,rw /sys; then
      printf 'failed to remount /sys read-write; storage teardown requires writable sysfs\n' >&2
      exit 1
    fi
  fi
  for module in dm_mod loop nvme_fabrics nvme_tcp nvmet nvmet_tcp iscsi_tcp target_core_mod target_core_iblock iscsi_target_mod; do
    if ! error=$(modprobe "${module}" 2>&1); then
      missing+=("${module}")
      printf 'failed to load required kernel module %s: %s\n' "${module}" "${error:-modprobe exited nonzero}" >&2
    fi
  done
  if (( ${#missing[@]} > 0 )); then
    printf 'required kernel modules are unavailable: %s\n' "${missing[*]}" >&2
    printf '%s\n' \
      'Run this harness on a Linux host or Linux VM whose kernel includes device-mapper, loop, NVMe initiator/target, and iSCSI initiator/LIO target support.' \
      'Ubuntu/Debian commonly requires: sudo apt-get install linux-modules-extra-$(uname -r)' \
      'Then load: sudo modprobe dm_mod loop nvmet nvmet-tcp nvme-fabrics nvme-tcp iscsi_tcp target_core_mod target_core_iblock iscsi_target_mod' >&2
    exit 1
  fi
  if ! mountpoint -q /sys/kernel/config; then
    if ! error=$(mount -t configfs none /sys/kernel/config 2>&1); then
      printf 'failed to mount configfs at /sys/kernel/config: %s\n' "${error:-mount exited nonzero}" >&2
      exit 1
    fi
  fi
  if [[ ! -d /sys/kernel/config/nvmet ]]; then
    printf '%s\n' \
      'NVMe target configfs is unavailable at /sys/kernel/config/nvmet.' \
      'Mount configfs on the Linux Docker host before running this harness.' >&2
    exit 1
  fi
  # target_core_mod registers /sys/kernel/config/target; the iscsi fabric
  # directory below it appears only once something mkdirs it (the agent
  # does), so only the core directory is a prerequisite.
  if [[ ! -d /sys/kernel/config/target ]]; then
    printf '%s\n' \
      'LIO target configfs is unavailable at /sys/kernel/config/target.' \
      'Load target_core_mod on the Linux Docker host before running this harness.' >&2
    exit 1
  fi
  if [[ ! -r /sys/class/iscsi_transport/tcp/handle ]]; then
    printf 'iscsi_tcp loaded but the iSCSI tcp transport is not registered at /sys/class/iscsi_transport/tcp\n' >&2
    exit 1
  fi
  [[ -d /sys/kernel/config/target/iscsi ]] && lio_iscsi_fabric_preexisting=true
  [[ -d /sys/kernel/config/target/core/iblock_3260 ]] && lio_iblock_hba_preexisting=true
  # pillar-node opens NETLINK_ISCSI in the host init network namespace through
  # the host /proc that compose.yaml mounts at ${host_proc}.
  if ! error=$(readlink "${host_init_netns}" 2>&1); then
    printf 'host init network namespace %s is unavailable (compose.yaml must mount the host /proc at %s): %s\n' \
      "${host_init_netns}" "${host_proc}" "${error:-readlink exited nonzero}" >&2
    exit 1
  fi
  if [[ "${error}" == "$(readlink /proc/self/ns/net)" ]]; then
    printf '%s is the harness network namespace, not the host init namespace; mount the host /proc at %s\n' \
      "${host_init_netns}" "${host_proc}" >&2
    exit 1
  fi
  log "Building the iSCSI session cleanup helper"
  if ! error=$(cd "${repo_root}" && go build -buildvcs=false -o "${iscsi_cleanup_bin}" ./test/docker-e2e/cmd/iscsi-session-cleanup 2>&1); then
    printf 'failed to build the iSCSI session cleanup helper: %s\n' "${error}" >&2
    exit 1
  fi
  if [[ ! -c /dev/mapper/control ]]; then
    if ! error=$(mkdir -p /dev/mapper 2>&1); then
      printf 'failed to create device-mapper directory /dev/mapper: %s\n' "${error:-mkdir exited nonzero}" >&2
      exit 1
    fi
    if [[ ! -r /sys/class/misc/device-mapper/dev ]]; then
      printf 'dm_mod loaded but /sys/class/misc/device-mapper/dev is unavailable\n' >&2
      exit 1
    fi
    IFS=: read -r dm_major dm_minor < /sys/class/misc/device-mapper/dev
    if ! error=$(mknod /dev/mapper/control c "${dm_major}" "${dm_minor}" 2>&1); then
      printf 'failed to create /dev/mapper/control from kernel device %s:%s: %s\n' \
        "${dm_major}" "${dm_minor}" "${error:-mknod exited nonzero}" >&2
      exit 1
    fi
  fi
  if [[ ! -c /dev/nvme-fabrics ]]; then
    if [[ ! -r /sys/class/misc/nvme-fabrics/dev ]]; then
      printf 'nvme_fabrics loaded but /sys/class/misc/nvme-fabrics/dev is unavailable\n' >&2
      exit 1
    fi
    IFS=: read -r nvme_major nvme_minor < /sys/class/misc/nvme-fabrics/dev
    if ! error=$(mknod /dev/nvme-fabrics c "${nvme_major}" "${nvme_minor}" 2>&1); then
      printf 'failed to create /dev/nvme-fabrics from kernel device %s:%s: %s\n' \
        "${nvme_major}" "${nvme_minor}" "${error:-mknod exited nonzero}" >&2
      exit 1
    fi
  fi
  if [[ ! -c /dev/mapper/control || ! -c /dev/nvme-fabrics ]]; then
    printf 'required storage control devices are unavailable: /dev/mapper/control and /dev/nvme-fabrics\n' >&2
    exit 1
  fi
  cleanup_host_storage_state
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

# Every worker runs pillar-node, which opens NETLINK_ISCSI in the host init
# network namespace through /host/proc/1/ns/net (helm value
# node.iscsi.netlinkNetnsPath).  /host/proc here is the host /proc that
# compose.yaml mounts into this container.
#
# A Kind node's own /sys is a read-only sysfs instance, so the client workers
# receive the writable sysfs subtrees their initiators write, shared by
# entrypoint.sh: nvme-fabrics for NVMe-oF, and /sys/devices/platform, where
# the software iSCSI SCSI hosts live (pillar-node writes their scan and
# per-device rescan attributes).
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
      - hostPath: /host/proc
        containerPath: /host/proc
        readOnly: true
  - role: worker
    extraMounts:
      - hostPath: /sys/devices/virtual/nvme-fabrics
        containerPath: /sys/devices/virtual/nvme-fabrics
        readOnly: false
        propagation: Bidirectional
      - hostPath: /sys/devices/platform
        containerPath: /sys/devices/platform
        readOnly: false
        propagation: Bidirectional
      - hostPath: /host/proc
        containerPath: /host/proc
        readOnly: true
  - role: worker
    extraMounts:
      - hostPath: /sys/devices/virtual/nvme-fabrics
        containerPath: /sys/devices/virtual/nvme-fabrics
        readOnly: false
        propagation: Bidirectional
      - hostPath: /sys/devices/platform
        containerPath: /sys/devices/platform
        readOnly: false
        propagation: Bidirectional
      - hostPath: /host/proc
        containerPath: /host/proc
        readOnly: true
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
      - hostPath: /sys/devices/platform
        containerPath: /sys/devices/platform
        readOnly: false
        propagation: Bidirectional
      - hostPath: /host/proc
        containerPath: /host/proc
        readOnly: true
  - role: worker
    extraMounts:
      - hostPath: /sys/devices/virtual/nvme-fabrics
        containerPath: /sys/devices/virtual/nvme-fabrics
        readOnly: false
        propagation: Bidirectional
      - hostPath: /sys/devices/platform
        containerPath: /sys/devices/platform
        readOnly: false
        propagation: Bidirectional
      - hostPath: /host/proc
        containerPath: /host/proc
        readOnly: true
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
  docker exec "${storage_node}" bash -ceu '
    backing_file=$1
    vg=$2
    truncate -s 2G "${backing_file}"
    loop_device=$(losetup --find --show "${backing_file}")
    pvcreate --force --yes "${loop_device}"
    vgcreate "${vg}" "${loop_device}"
  ' pillar-e2e-create-backend "${internal_backing_file}" "${vg_name}"
}

start_external_agent() {
  container_name=$1
  log "Starting external storage server ${container_name}"
  if docker inspect "${container_name}" >/dev/null 2>&1; then
    log "Removing stale external storage server ${container_name}"
    if ! error=$(timeout --kill-after=2s 20s docker rm -f "${container_name}" 2>&1); then
      printf 'failed to remove stale external storage server %s: %s\n' \
        "${container_name}" "${error:-docker rm exited nonzero}" >&2
      return 1
    fi
    cleanup_host_storage_state
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

  # kindest/node ships open-iscsi with one baked-in
  # /etc/iscsi/initiatorname.iscsi, so every Kind node starts with the same
  # IQN.  All Kind nodes share this kernel, and pillar-node treats sessions
  # carrying its IQN as its own, so identical IQNs make every node recover
  # and rebind the same kernel connection (a double BIND_CONN that crashes
  # iscsi_tcp).  Remove the baked file so each pillar-node generates a unique
  # IQN, as it would on a real host without open-iscsi.
  for kind_node in $(kind get nodes --name "${cluster}"); do
    docker exec "${kind_node}" rm -f /etc/iscsi/initiatorname.iscsi
  done

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
    --set "node.iscsi.netlinkNetnsPath=${host_init_netns}"
  )
  if [[ "${topology}" == internal ]]; then
    helm_args+=(--set "agent.backends[0].lvm.volumeGroup=${vg_name}")
  fi
  helm "${helm_args[@]}"
  wait_for_webhook_api
}

wait_for_webhook_api() {
  for attempt in $(seq 1 30); do
    if kubectl apply --dry-run=server -f - >/dev/null 2>&1 <<EOF
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: pillar-e2e-webhook-readiness
spec:
  protocol:
    nvmeofTcp:
      port: ${nvmeof_port}
      acl: true
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

wait_for_resource_ready() {
  local resource=$1
  local deadline=$((SECONDS + 180))
  local state=""
  while ((SECONDS < deadline)); do
    if state=$(kubectl --request-timeout=5s get "${resource}" \
      -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>&1) &&
      [[ "${state}" == "True" ]]; then
      return
    fi
    sleep 2
  done
  printf 'timed out waiting for %s Ready condition; last state: %s\n' \
    "${resource}" "${state}" >&2
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
    lvm:
      volumeGroup: ${vg_name}
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: pillar-e2e-nvme
spec:
  protocol:
    nvmeofTcp:
      port: ${nvmeof_port}
      acl: true
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
  filesystem:
    fsType: ext4
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: ${local_storage_class}
spec:
  storeRef: pillar-e2e-store
  protocolRef: pillar-e2e-nvme
  localAttach: true
  storageClass:
    name: ${local_storage_class}
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
    allowVolumeExpansion: true
  filesystem:
    fsType: ext4
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: ${iscsi_protocol}
spec:
  protocol:
    iscsi:
      port: ${iscsi_port}
      acl: true
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: ${iscsi_storage_class}
spec:
  storeRef: pillar-e2e-store
  protocolRef: ${iscsi_protocol}
  storageClass:
    name: ${iscsi_storage_class}
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
    allowVolumeExpansion: true
  filesystem:
    fsType: ext4
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: ${iscsi_xfs_storage_class}
spec:
  storeRef: pillar-e2e-store
  protocolRef: ${iscsi_protocol}
  storageClass:
    name: ${iscsi_xfs_storage_class}
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
    allowVolumeExpansion: true
  filesystem:
    fsType: xfs
EOF

  wait_for_resource_ready pillaragent/pillar-e2e-agent
  wait_for_resource_ready "pillarstorageclass/${storage_class}"
  wait_for_resource_ready "pillarstorageclass/${local_storage_class}"
  wait_for_resource_ready "pillarstorageclass/${iscsi_storage_class}"
  wait_for_resource_ready "pillarstorageclass/${iscsi_xfs_storage_class}"
}

run_tests() {
  topology=$1
  storage_node=$2
  client_a=$3
  client_b=$4
  target_address=$5
  log "Running CSI lifecycle tests for ${topology} topology"
  PILLAR_E2E_TOPOLOGY="${topology}" \
  PILLAR_E2E_STORAGE_CLASS="${storage_class}" \
  PILLAR_E2E_LOCAL_STORAGE_CLASS="${local_storage_class}" \
  PILLAR_E2E_ISCSI_STORAGE_CLASS="${iscsi_storage_class}" \
  PILLAR_E2E_ISCSI_XFS_STORAGE_CLASS="${iscsi_xfs_storage_class}" \
  PILLAR_E2E_ISCSI_PROTOCOL="${iscsi_protocol}" \
  PILLAR_E2E_STORAGE_NODE="${storage_node}" \
  PILLAR_E2E_CLIENT_NODE_A="${client_a}" \
  PILLAR_E2E_CLIENT_NODE_B="${client_b}" \
  PILLAR_E2E_TARGET_ADDRESS="${target_address}" \
    go test -tags=docker_e2e -count=1 -timeout=150m -v ./test/docker-e2e
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
    if kubectl --request-timeout=5s get --raw=/readyz >/dev/null 2>&1; then
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
  active_target_address="${target_address}"

  install_driver "${topology}" "${cluster}"
  capture_active_nvme_host_nqns "${client_a}" "${client_b}"
  apply_storage_resources "${topology}" "${storage_node}" "${target_address}"
  run_tests "${topology}" "${storage_node}" "${client_a}" "${client_b}" "${target_address}"

  log "${topology} topology passed"
  if ! cleanup_topology; then
    topology_cleanup_failed=true
    return 1
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
