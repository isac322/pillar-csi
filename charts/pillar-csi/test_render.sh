#!/usr/bin/env bash
# Helm chart render regression tests for pillar-csi.
#
# Verifies that the node-daemonset.yaml template renders the kernel-data-plane
# invariants the cluster depends on:
#
#   - hostNetwork: true on the node DaemonSet
#     (required for nvme-fabrics initiator + nvmet_tcp listener netns reach)
#   - dnsPolicy: ClusterFirstWithHostNet alongside hostNetwork: true
#     (otherwise cluster DNS breaks from host netns)
#   - kubelet-csi-dir hostPath mount at /var/lib/kubelet/plugins/kubernetes.io/csi
#     with mountPropagation: Bidirectional
#     (required for Block-mode publish bind to reach the kubelet on host)
#
# Run with:   bash charts/pillar-csi/test_render.sh
# CI invokes: make test-chart

set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")" && pwd)"
RENDER_OUT="$(helm template pillar-csi-test "${CHART_DIR}")"

# Restrict assertions to the node DaemonSet manifest by extracting just that
# document.  awk emits the lines between the DaemonSet document header and
# the next '---' separator.
node_ds_yaml() {
  echo "${RENDER_OUT}" | awk '
    /^# Source: pillar-csi\/templates\/node-daemonset\.yaml/ { in_doc = 1 }
    in_doc { print }
    in_doc && /^---$/ && NR > 1 { in_doc = 0 }
  '
}

NODE_DS="$(node_ds_yaml)"
if [[ -z "${NODE_DS}" ]]; then
  echo "FAIL: node-daemonset.yaml not found in helm template output" >&2
  exit 1
fi

fail=0
assert_contains() {
  local needle="$1"
  local description="$2"
  if ! grep -qF -- "${needle}" <<< "${NODE_DS}"; then
    echo "FAIL: ${description}"
    echo "      expected node-daemonset.yaml to contain: ${needle}"
    fail=1
  fi
}

# 1. hostNetwork: true at pod spec level.
assert_contains "hostNetwork: true" \
  "node DaemonSet must default hostNetwork: true (PRD §2.4 + kernel netns rationale)"

# 2. dnsPolicy: ClusterFirstWithHostNet whenever hostNetwork is on.
assert_contains "dnsPolicy: ClusterFirstWithHostNet" \
  "node DaemonSet must emit dnsPolicy: ClusterFirstWithHostNet so cluster DNS keeps working from host netns"

# 3. kubelet-csi-dir volume mount at the expected path with Bidirectional propagation.
assert_contains "mountPath: /var/lib/kubelet/plugins/kubernetes.io/csi" \
  "node container must mount kubelet's CSI state tree for Block-mode publish path visibility"
assert_contains "name: kubelet-csi-dir" \
  "node container must reference the kubelet-csi-dir hostPath volume"

# 4. Bidirectional propagation must accompany the kubelet-csi-dir mount.
#    We assert the literal token appears at least twice in the node DaemonSet:
#    once for pods-mount-dir (existing) and once for kubelet-csi-dir (new).
bidirectional_count="$(grep -c "mountPropagation: Bidirectional" <<< "${NODE_DS}" || true)"
if (( bidirectional_count < 2 )); then
  echo "FAIL: expected at least 2 mountPropagation: Bidirectional entries (pods-mount-dir + kubelet-csi-dir), got ${bidirectional_count}"
  fail=1
fi

if (( fail != 0 )); then
  echo
  echo "Chart render regression test FAILED."
  exit 1
fi

echo "Chart render regression test passed."
