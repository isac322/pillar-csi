#!/usr/bin/env bash
set -Eeuo pipefail

log_file=/tmp/dockerd.log
dns_server=${PILLAR_E2E_DNS:-1.1.1.1}

if ! findmnt -no OPTIONS /sys | tr ',' '\n' | grep -qx rw; then
  if ! mount -o remount,rw /sys; then
    printf 'failed to remount /sys read-write before sharing the NVMe fabrics and iSCSI host sysfs subtrees\n' >&2
    exit 1
  fi
fi

# Kind bind-mounts these host sysfs subtrees into its node containers, whose
# own /sys is a read-only sysfs instance.  Give each subtree its own shared
# mount so Kind can propagate it without making all of /sys shared between
# the Docker-in-Docker container and its children.
#   - nvme-fabrics: pillar-node writes /dev/nvme-fabrics controllers there.
#   - platform: software iSCSI SCSI hosts (/sys/devices/platform/host<N>);
#     pillar-node writes scsi_host/host<N>/scan to discover a session's LUN
#     and <H:C:T:L>/rescan after an online resize.
if ! modprobe nvme_fabrics >/dev/null 2>&1; then
  printf '%s\n' \
    'failed to load nvme_fabrics before starting Docker' \
    'This harness requires a Linux kernel with CONFIG_NVME_TARGET and CONFIG_NVME_TARGET_TCP.' \
    "Ubuntu/Debian commonly requires: sudo apt-get install linux-modules-extra-$(uname -r)" \
    'Then load: sudo modprobe nvmet nvmet-tcp nvme-fabrics nvme-tcp' >&2
  exit 1
fi
for shared_sysfs in /sys/devices/virtual/nvme-fabrics /sys/devices/platform; do
  if ! mount --bind "${shared_sysfs}" "${shared_sysfs}"; then
    printf 'failed to create bind mount for %s\n' "${shared_sysfs}" >&2
    exit 1
  fi
  if ! mount --make-rshared "${shared_sysfs}"; then
    printf 'failed to make %s a shared mount\n' "${shared_sysfs}" >&2
    exit 1
  fi
done

# /var/lib/docker is a cache volume that outlives this container (see
# compose.yaml): pulled images and the BuildKit cache, including the Go build
# cache mount, are reused by the next run.  Containers, networks and volumes
# are run state, not cache.  An interrupted run can leave Kind nodes or the
# external agent behind with a restart policy, and dockerd would restart them
# on startup against the host storage stack.  Clear those restart policies
# before dockerd reads them; purge_docker_state then removes the containers
# through the daemon.
for hostconfig in /var/lib/docker/containers/*/hostconfig.json; do
  [[ -e "${hostconfig}" ]] || continue
  if ! jq '.RestartPolicy = {"Name": "no", "MaximumRetryCount": 0}' "${hostconfig}" >"${hostconfig}.tmp" \
    || ! mv "${hostconfig}.tmp" "${hostconfig}"; then
    printf 'failed to disable the restart policy in stale %s\n' "${hostconfig}" >&2
    exit 1
  fi
done

setsid dockerd \
  --host=unix:///var/run/docker.sock \
  --dns="${dns_server}" \
  --storage-driver=overlay2 \
  >"${log_file}" 2>&1 &
dockerd_pid=$!

run_pid=""
requested_exit_code=""

forward_signal() {
  signal=$1
  exit_code=$2
  requested_exit_code=${exit_code}
  if [[ -n "${run_pid}" ]] && kill -0 "${run_pid}" 2>/dev/null; then
    # run.sh owns a separate process group so its shell trap and all active
    # children receive the stop signal before Compose reaches its kill timeout.
    kill -s "${signal}" -- "-${run_pid}" 2>/dev/null || true
    return
  fi
  exit "${exit_code}"
}

# purge_docker_state returns the nested daemon to zero run state while keeping
# tagged images and the BuildKit cache: it removes every container with its
# anonymous volumes, every unused network and volume, and dangling images left
# behind when a rebuild moved an image tag.
purge_docker_state() {
  local containers
  containers=$(docker ps -aq) || return 1
  if [[ -n "${containers}" ]]; then
    printf 'removing leftover nested Docker containers: %s\n' "${containers//$'\n'/ }" >&2
    # shellcheck disable=SC2086 # container IDs are whitespace-separated words.
    docker rm -f -v ${containers} >/dev/null || return 1
  fi
  docker network prune -f >/dev/null || return 1
  docker volume prune --all -f >/dev/null || return 1
  docker image prune -f >/dev/null || return 1
}

cleanup_docker_mounts() {
  local cleanup_rc=0
  local target
  local -a targets=()
  mapfile -t targets < <(findmnt -Rrno TARGET /var/lib/docker 2>/dev/null | sort -r)
  for target in "${targets[@]}"; do
    if [[ "${target}" == /var/lib/docker ]]; then
      continue
    fi
    if ! umount -l "${target}"; then
      printf 'failed to unmount nested Docker path %s\n' "${target}" >&2
      cleanup_rc=1
    fi
  done
  return "${cleanup_rc}"
}

cleanup() {
  local rc=$?
  local cleanup_rc=0
  trap - EXIT
  if [[ -n "${run_pid}" ]] && kill -0 "${run_pid}" 2>/dev/null; then
    kill -s TERM -- "-${run_pid}" 2>/dev/null || cleanup_rc=1
    wait "${run_pid}" 2>/dev/null || true
  fi
  if kill -0 "${dockerd_pid}" 2>/dev/null; then
    if docker info >/dev/null 2>&1; then
      purge_docker_state || cleanup_rc=1
    fi
    kill "${dockerd_pid}" 2>/dev/null || cleanup_rc=1
    wait "${dockerd_pid}" 2>/dev/null || true
  fi
  cleanup_docker_mounts || cleanup_rc=1
  restore_inotify_limits || cleanup_rc=1
  if [[ ${rc} -eq 0 && ${cleanup_rc} -ne 0 ]]; then
    rc=${cleanup_rc}
  fi
  exit "${rc}"
}
trap cleanup EXIT
trap 'forward_signal TERM 130' INT
trap 'forward_signal TERM 143' TERM

# Every Kind node runs systemd, containerd and kubelet, whose inotify instances
# count against root in the host kernel.  A four-node cluster nearly exhausts
# the common default of 128 instances, and a restarted kubelet then fails to
# start cAdvisor ("inotify_init: too many open files").  Raise the limits to
# the values Kind recommends for the duration of the run; cleanup restores
# the host's values.
declare -A inotify_originals=()
raise_inotify_limit() {
  local name=$1
  local want=$2
  local path=/proc/sys/fs/inotify/${name}
  local current
  current=$(<"${path}")
  if (( current >= want )); then
    return
  fi
  if ! printf '%s\n' "${want}" >"${path}"; then
    printf 'failed to raise %s from %s to %s\n' "${path}" "${current}" "${want}" >&2
    exit 1
  fi
  inotify_originals[${name}]=${current}
}
restore_inotify_limits() {
  local rc=0
  local name
  for name in "${!inotify_originals[@]}"; do
    if ! printf '%s\n' "${inotify_originals[${name}]}" >"/proc/sys/fs/inotify/${name}"; then
      printf 'failed to restore /proc/sys/fs/inotify/%s to %s\n' "${name}" "${inotify_originals[${name}]}" >&2
      rc=1
    fi
  done
  return "${rc}"
}
raise_inotify_limit max_user_instances 512
raise_inotify_limit max_user_watches 524288

for _ in $(seq 1 120); do
  if docker info >/dev/null 2>&1; then
    if ! purge_docker_state; then
      printf 'failed to remove leftover nested Docker state from a previous run\n' >&2
      exit 1
    fi
    set +e
    setsid /usr/local/bin/pillar-csi-docker-e2e-run &
    run_pid=$!
    wait "${run_pid}"
    rc=$?
    if kill -0 "${run_pid}" 2>/dev/null; then
      wait "${run_pid}"
      rc=$?
    fi
    if [[ -n "${requested_exit_code}" ]]; then
      rc=${requested_exit_code}
    fi
    run_pid=""
    set -e
    exit "${rc}"
  fi
  if ! kill -0 "${dockerd_pid}" 2>/dev/null; then
    cat "${log_file}" >&2
    exit 1
  fi
  sleep 1
done

cat "${log_file}" >&2
printf 'dockerd did not become ready within 120 seconds\n' >&2
exit 1
