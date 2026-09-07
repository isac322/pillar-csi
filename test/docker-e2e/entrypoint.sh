#!/usr/bin/env bash
set -Eeuo pipefail

log_file=/tmp/dockerd.log
dns_server=${PILLAR_E2E_DNS:-1.1.1.1}

if ! findmnt -no OPTIONS /sys | tr ',' '\n' | grep -qx rw; then
  if ! mount -o remount,rw /sys; then
    printf 'failed to remount /sys read-write before sharing NVMe fabrics sysfs\n' >&2
    exit 1
  fi
fi

# Kind bind-mounts this host sysfs subtree into its node containers.  Give that
# subtree its own shared mount so Kind can propagate it without making all of
# /sys shared between the Docker-in-Docker container and its children.
if ! modprobe nvme_fabrics >/dev/null 2>&1; then
  printf '%s\n' \
    'failed to load nvme_fabrics before starting Docker' \
    'This harness requires a Linux kernel with CONFIG_NVME_TARGET and CONFIG_NVME_TARGET_TCP.' \
    "Ubuntu/Debian commonly requires: sudo apt-get install linux-modules-extra-$(uname -r)" \
    'Then load: sudo modprobe nvmet nvmet-tcp nvme-fabrics nvme-tcp' >&2
  exit 1
fi
nvme_fabrics_sysfs=/sys/devices/virtual/nvme-fabrics
if ! mount --bind "${nvme_fabrics_sysfs}" "${nvme_fabrics_sysfs}"; then
  printf 'failed to create bind mount for %s\n' "${nvme_fabrics_sysfs}" >&2
  exit 1
fi
if ! mount --make-rshared "${nvme_fabrics_sysfs}"; then
  printf 'failed to make %s a shared mount\n' "${nvme_fabrics_sysfs}" >&2
  exit 1
fi

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
    kill "${dockerd_pid}" 2>/dev/null || cleanup_rc=1
    wait "${dockerd_pid}" 2>/dev/null || true
  fi
  cleanup_docker_mounts || cleanup_rc=1
  if [[ ${rc} -eq 0 && ${cleanup_rc} -ne 0 ]]; then
    rc=${cleanup_rc}
  fi
  exit "${rc}"
}
trap cleanup EXIT
trap 'forward_signal TERM 130' INT
trap 'forward_signal TERM 143' TERM

for _ in $(seq 1 120); do
  if docker info >/dev/null 2>&1; then
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
