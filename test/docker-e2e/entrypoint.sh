#!/usr/bin/env bash
set -Eeuo pipefail

log_file=/tmp/dockerd.log

dockerd \
  --host=unix:///var/run/docker.sock \
  --storage-driver=overlay2 \
  >"${log_file}" 2>&1 &
dockerd_pid=$!

cleanup() {
  kill "${dockerd_pid}" 2>/dev/null || true
  wait "${dockerd_pid}" 2>/dev/null || true
}
trap cleanup EXIT

for _ in $(seq 1 120); do
  if docker info >/dev/null 2>&1; then
    exec /usr/local/bin/pillar-csi-docker-e2e-run
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
