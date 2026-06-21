#!/usr/bin/env bash
# Helm chart render regression tests for pillar-csi.
#
# Verifies invariants across multiple value combinations:
#
#   default render:
#     - hostNetwork: true on the node DaemonSet
#       (required for nvme-fabrics initiator + nvmet_tcp listener netns reach)
#     - dnsPolicy: ClusterFirstWithHostNet alongside hostNetwork: true
#       (otherwise cluster DNS breaks from host netns)
#     - kubelet-csi-dir hostPath mount at /var/lib/kubelet/plugins/kubernetes.io/csi
#       with mountPropagation: Bidirectional
#       (required for Block-mode publish bind to reach the kubelet on host)
#     - terminationGracePeriodSeconds + preStop hook + matching probes on
#       both the agent and node pods so the SIGTERM-driven Drain contract
#       has a chance to complete
#     - no mTLS Secret references anywhere in the rendered output
#       (mtls.enabled=false is the documented default)
#
#   mtls.enabled=true (secret mode):
#     - controller-deployment and agent-daemonset mount the operator-supplied
#       Secrets pillar-controller-mtls and pillar-agent-mtls
#     - both pods receive the matching --*-tls-* CLI flags
#     - no cert-manager resources are rendered
#
#   mtls.certManager.enabled=true:
#     - chart renders cert-manager Issuer (self-signed by default) plus
#       two Certificate resources whose secretNames match the deployment
#       Secret mounts (so the auto-issued chain reaches the pods)
#
# Run with:   bash charts/pillar-csi/test_render.sh
# Override:   HELM=/tmp/linux-arm64/helm bash charts/pillar-csi/test_render.sh
# CI invokes: make test-chart

set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")" && pwd)"
HELM="${HELM:-helm}"
RELEASE="pillar-csi-test"

render() {
  "${HELM}" template "${RELEASE}" "${CHART_DIR}" "$@"
}

fail=0
mark_fail() {
  fail=1
  echo "FAIL: $*"
}

assert_contains() {
  local body="$1" needle="$2" description="$3"
  if ! grep -qF -- "${needle}" <<< "${body}"; then
    mark_fail "${description}"
    echo "      expected rendered output to contain: ${needle}"
  fi
}

assert_not_contains() {
  local body="$1" needle="$2" description="$3"
  if grep -qF -- "${needle}" <<< "${body}"; then
    mark_fail "${description}"
    echo "      rendered output unexpectedly contained: ${needle}"
  fi
}

assert_min_count() {
  local body="$1" pattern="$2" min="$3" description="$4"
  local got
  got="$(grep -c -- "${pattern}" <<< "${body}" || true)"
  if (( got < min )); then
    mark_fail "${description}"
    echo "      expected at least ${min} occurrences of pattern: ${pattern}"
    echo "      got: ${got}"
  fi
}

# Restrict assertions to a specific template by extracting just that document.
extract_doc() {
  local body="$1" template="$2"
  awk -v t="# Source: pillar-csi/templates/${template}" '
    $0 == t { in_doc = 1 }
    in_doc { print }
    in_doc && /^---$/ && NR > 1 { in_doc = 0 }
  ' <<< "${body}"
}

# ──────────────────────────────────────────────────────────────────────────
# Mode 1: default render
# ──────────────────────────────────────────────────────────────────────────
DEFAULT_OUT="$(render)"
NODE_DS="$(extract_doc "${DEFAULT_OUT}" "node-daemonset.yaml")"
if [[ -z "${NODE_DS}" ]]; then
  mark_fail "default render: node-daemonset.yaml not found in helm template output"
  exit 1
fi

# Existing kernel-data-plane invariants on node DaemonSet.
assert_contains "${NODE_DS}" "hostNetwork: true" \
  "default node DaemonSet must default hostNetwork: true (PRD §2.4)"
assert_contains "${NODE_DS}" "dnsPolicy: ClusterFirstWithHostNet" \
  "default node DaemonSet must emit dnsPolicy: ClusterFirstWithHostNet"
assert_contains "${NODE_DS}" "mountPath: /var/lib/kubelet/plugins/kubernetes.io/csi" \
  "default node container must mount kubelet's CSI state tree"
assert_contains "${NODE_DS}" "name: kubelet-csi-dir" \
  "default node container must reference the kubelet-csi-dir hostPath volume"
assert_min_count "${NODE_DS}" "mountPropagation: Bidirectional" 2 \
  "default node DaemonSet must have ≥2 Bidirectional propagation mounts"

# Cooperative shutdown contract — both the node and agent pods need preStop +
# termGrace so the SIGTERM handler has the budget to Drain + GracefulStop.
AGENT_DS_DEFAULT="$(extract_doc "${DEFAULT_OUT}" "agent-daemonset.yaml")"
assert_contains "${AGENT_DS_DEFAULT}" "terminationGracePeriodSeconds: 60" \
  "default agent DaemonSet must set terminationGracePeriodSeconds=60"
assert_contains "${AGENT_DS_DEFAULT}" "command: [\"/bin/busybox\", \"sleep\", \"5\"]" \
  "default agent DaemonSet must emit preStop busybox sleep 5 (runtime image has no /bin/sh)"
assert_min_count "${AGENT_DS_DEFAULT}" "grpc:" 2 \
  "default agent DaemonSet must expose grpc: liveness AND readiness probes (kubelet >=1.24)"

assert_contains "${NODE_DS}" "terminationGracePeriodSeconds: 60" \
  "default node DaemonSet must set terminationGracePeriodSeconds=60"
# The node DaemonSet also carries the existing node-driver-registrar preStop
# (rm -rf the registration socket), so we expect the literal busybox sleep 5
# entry at least once for the node container itself.
assert_contains "${NODE_DS}" "command: [\"/bin/busybox\", \"sleep\", \"5\"]" \
  "default node DaemonSet must emit preStop busybox sleep 5 on the node container"

# No mTLS plumbing in the default render.
assert_not_contains "${DEFAULT_OUT}" "name: mtls-certs" \
  "default render must NOT include mtls-certs volume (mtls.enabled=false)"
assert_not_contains "${DEFAULT_OUT}" "--agent-tls-cert" \
  "default render must NOT pass --agent-tls-cert to the controller"
assert_not_contains "${DEFAULT_OUT}" "--tls-cert=" \
  "default render must NOT pass --tls-cert to the agent"
assert_not_contains "${DEFAULT_OUT}" "kind: Issuer" \
  "default render must NOT include cert-manager Issuer (certManager.enabled=false)"
assert_not_contains "${DEFAULT_OUT}" "kind: Certificate" \
  "default render must NOT include cert-manager Certificate"

# ──────────────────────────────────────────────────────────────────────────
# Mode 2: mtls.enabled (secret mode, operator-managed Secrets)
# ──────────────────────────────────────────────────────────────────────────
MTLS_OUT="$(render --set mtls.enabled=true)"

# Controller deployment surface.
CTL_DEP="$(extract_doc "${MTLS_OUT}" "controller-deployment.yaml")"
assert_contains "${CTL_DEP}" "secretName: pillar-controller-mtls" \
  "mtls=on: controller deployment must mount pillar-controller-mtls Secret"
assert_contains "${CTL_DEP}" "--agent-tls-cert=/etc/pillar-csi/mtls/tls.crt" \
  "mtls=on: controller must receive --agent-tls-cert flag"
assert_contains "${CTL_DEP}" "--agent-tls-key=/etc/pillar-csi/mtls/tls.key" \
  "mtls=on: controller must receive --agent-tls-key flag"
assert_contains "${CTL_DEP}" "--agent-tls-ca=/etc/pillar-csi/mtls/ca.crt" \
  "mtls=on: controller must receive --agent-tls-ca flag"

# Agent daemonset surface.
AGT_DS="$(extract_doc "${MTLS_OUT}" "agent-daemonset.yaml")"
assert_contains "${AGT_DS}" "secretName: pillar-agent-mtls" \
  "mtls=on: agent DaemonSet must mount pillar-agent-mtls Secret"
assert_contains "${AGT_DS}" "--tls-cert=/etc/pillar-csi/mtls/tls.crt" \
  "mtls=on: agent must receive --tls-cert flag"
assert_contains "${AGT_DS}" "--tls-key=/etc/pillar-csi/mtls/tls.key" \
  "mtls=on: agent must receive --tls-key flag"
assert_contains "${AGT_DS}" "--tls-ca=/etc/pillar-csi/mtls/ca.crt" \
  "mtls=on: agent must receive --tls-ca flag"

# Secret-mode does not auto-render cert-manager resources.
assert_not_contains "${MTLS_OUT}" "kind: Issuer" \
  "mtls=on (secret mode): must NOT auto-render cert-manager Issuer"
assert_not_contains "${MTLS_OUT}" "kind: Certificate" \
  "mtls=on (secret mode): must NOT auto-render cert-manager Certificate"

# ──────────────────────────────────────────────────────────────────────────
# Mode 3: cert-manager mode
# ──────────────────────────────────────────────────────────────────────────
CM_OUT="$(render --set mtls.enabled=true --set mtls.certManager.enabled=true)"

assert_contains "${CM_OUT}" "kind: Issuer" \
  "certManager=on: chart must render cert-manager Issuer"
assert_min_count "${CM_OUT}" "^kind: Certificate" 2 \
  "certManager=on: chart must render exactly 2 Certificates (controller + agent)"

# Auto-generated Secret names propagate to the pod mounts.
CM_CTL_DEP="$(extract_doc "${CM_OUT}" "controller-deployment.yaml")"
assert_contains "${CM_CTL_DEP}" "secretName: ${RELEASE}-controller-mtls" \
  "certManager=on: controller deployment must mount auto-issued controller Secret"

CM_AGT_DS="$(extract_doc "${CM_OUT}" "agent-daemonset.yaml")"
assert_contains "${CM_AGT_DS}" "secretName: ${RELEASE}-agent-mtls" \
  "certManager=on: agent DaemonSet must mount auto-issued agent Secret"

# ──────────────────────────────────────────────────────────────────────────
# Final verdict
# ──────────────────────────────────────────────────────────────────────────
if (( fail != 0 )); then
  echo
  echo "Chart render regression test FAILED."
  exit 1
fi

echo "Chart render regression test passed."
