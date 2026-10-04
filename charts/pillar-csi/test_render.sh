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
#   metrics / tracing / PodMonitor (T9):
#     - default: no OTEL_* env, no agent/node metrics port or flag, secure
#       controller metrics with TokenReview/SubjectAccessReview RBAC
#     - metrics.enabled: agent/node/sidecar ports unique and probe-resolvable,
#       agent/node bind [$(HOST_IP)]:<port> under hostNetwork
#     - tracing.enabled: POD_NAMESPACE/POD_NAME/NODE_NAME precede
#       OTEL_RESOURCE_ATTRIBUTES; an empty endpoint fails the render
#     - podMonitor.enabled: https + bearer controller endpoint, metrics-reader
#       token; fails without the PodMonitor API
#
#   API contract (default and installCRDs=false, via hack/chartcontract;
#   requires Go):
#     - rendered CRDs equal config/crd/bases (names, shortNames, schema)
#     - chart RBAC names only resources those CRDs serve and covers
#       config/rbac/role.yaml
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

# Kubelet resolves a probe's named port against the probing container's own
# ports only (pkg/probe ResolveContainerPort), while the API server warns when
# two containers in one Pod declare the same port name. Fail when a Pod spec
# rendered from a workload template declares a port name in more than one
# container, or when a probe names a port its own container does not declare.
assert_pod_ports_unambiguous() {
  local body="$1" description="$2" problems
  problems="$(awk '
    /^      [a-zA-Z]+:/ { in_containers = ($0 ~ /^      containers:/); container = ""; next }
    in_containers && /^        - name: / { container = $3; section = ""; next }
    container != "" && /^          [a-zA-Z]+:/ { section = $1; sub(/:$/, "", section); next }
    container != "" && section == "ports" && /^            - name: / {
      name = $3
      declared[container, name] = 1
      if ((name in owner) && owner[name] != container) {
        printf "port name %s declared by containers %s and %s\n", name, owner[name], container
      }
      owner[name] = container
      next
    }
    container != "" && section ~ /Probe$/ && /^              port: [^0-9]/ {
      refs[++n] = container SUBSEP $2 SUBSEP section
    }
    END {
      for (i = 1; i <= n; i++) {
        split(refs[i], r, SUBSEP)
        if (!((r[1], r[2]) in declared)) {
          printf "container %s %s uses port %s that it does not declare\n", r[1], r[3], r[2]
        }
      }
    }
  ' <<< "${body}")"
  if [[ -n "${problems}" ]]; then
    mark_fail "${description}"
    sed 's/^/      /' <<< "${problems}"
  fi
}

# Resolve the agent's mounts to their backing volumes. Checking unrelated
# occurrences of "hostPath" or "Bidirectional" cannot prove that dataset
# mounts survive a restart or that nfs-utils avoids foreign host exports.
assert_agent_nfs_storage() {
  local body="$1" description="$2" problems
  problems="$(awk '
    /^      containers:/ { section = "containers"; next }
    /^      volumes:/ { section = "volumes"; container = ""; next }
    /^      [a-zA-Z]+:/ { section = ""; container = ""; next }
    section == "containers" && /^        - name: / { container = $3; mounts = 0; env_section = 0; next }
    container == "agent" && /^          [a-zA-Z]+:/ {
      mounts = ($0 ~ /^          volumeMounts:/);
      env_section = ($0 ~ /^          env:/); next
    }
    env_section && /^            - name: / { env_name = $3; next }
    env_section && env_name == "PILLAR_AGENT_BIND_ADDRESS" && /^                  fieldPath:/ {
      bind_source = $2; next
    }
    mounts && /^            - name: / { nmount++; volume[nmount] = $3; next }
    mounts && /^              mountPath:/ { path[nmount] = $2; next }
    mounts && /^              mountPropagation:/ { propagation[nmount] = $2; next }
    mounts && /^              subPath:/ { subpath[nmount] = $2; next }
    mounts && /^              readOnly:/ { readonly[nmount] = $2; next }
    section == "volumes" && /^        - name: / { backing = $3; next }
    section == "volumes" && /^            path:/ { hostpath[backing] = $2; next }
    section == "volumes" && /^            type:/ { type[backing] = $2; next }
    END {
      if (bind_source != "status.hostIP") {
        print "NFS bind address must come from the numeric node host IP independently of telemetry";
      }
      for (i = 1; i <= nmount; i++) {
        if (path[i] == "/var/lib/pillar-csi/agent") root = i;
        if (path[i] == "/var/lib/nfs") recovery = i;
      }
      if (!root || propagation[root] != "Bidirectional" || readonly[root] == "true" ||
          hostpath[volume[root]] != "/var/lib/pillar-csi/agent" ||
          type[volume[root]] !~ /^Directory(OrCreate)?$/) {
        print "dataset mount root must be writable, host-persistent and Bidirectional";
      }
      if (!recovery || volume[recovery] != volume[root] ||
          subpath[recovery] != "nfs/lib" || readonly[recovery] == "true") {
        print "nfs-utils state must use private writable nfs/lib under the persistent agent volume";
      }
      for (v in hostpath) {
        if (hostpath[v] == "/var/lib/nfs") print "foreign host NFS state must not be mounted";
        if (hostpath[v] ~ /^\/(usr\/)?s?bin(\/|$)/) print "NFS helpers must be image-bundled, not host-mounted";
      }
    }
  ' <<< "${body}")"
  if [[ -n "${problems}" ]]; then
    mark_fail "${description}"
    printf '      %s\n' "${problems}"
  fi
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
CTL_DEP_DEFAULT="$(extract_doc "${DEFAULT_OUT}" "controller-deployment.yaml")"
WEBHOOK_DEFAULT="$(extract_doc "${DEFAULT_OUT}" "webhook.yaml")"

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
assert_contains "${NODE_DS}" "mountPropagation: HostToContainer" \
  "default node DaemonSet must propagate host NVMe sysfs submounts into the node container"

# Cooperative shutdown contract — both the node and agent pods need preStop +
# termGrace so the SIGTERM handler has the budget to Drain + GracefulStop.
AGENT_DS_DEFAULT="$(extract_doc "${DEFAULT_OUT}" "agent-daemonset.yaml")"
assert_contains "${AGENT_DS_DEFAULT}" "terminationGracePeriodSeconds: 60" \
  "default agent DaemonSet must set terminationGracePeriodSeconds=60"
assert_contains "${AGENT_DS_DEFAULT}" "command: [\"/bin/busybox\", \"sleep\", \"5\"]" \
  "default agent DaemonSet must emit preStop busybox sleep 5 (runtime image has no /bin/sh)"
assert_min_count "${AGENT_DS_DEFAULT}" "grpc:" 2 \
  "default agent DaemonSet must expose grpc: liveness AND readiness probes (kubelet >=1.24)"
assert_not_contains "${AGENT_DS_DEFAULT}" "hostPID: true" \
  "block-only agent must not share the host PID namespace"
assert_not_contains "${AGENT_DS_DEFAULT}" "mountPath: /var/lib/nfs" \
  "block-only agent must not mount NFS server state"

# Agent device access contract — the agent container must be privileged by
# default: otherwise the runtime's default device cgroup allowlist makes
# open(/dev/mapper/control, PV block devices, /dev/zfs) fail with EPERM and no
# pool is ever discovered. The modprobe init container is always privileged,
# so the only possible "privileged: false" in this document is the agent's.
assert_not_contains "${AGENT_DS_DEFAULT}" "privileged: false" \
  "default agent container must be privileged (host device nodes need device-cgroup access)"
AGENT_UNPRIV_DS="$(extract_doc "$(render --set agent.privileged=false)" "agent-daemonset.yaml")"
assert_contains "${AGENT_UNPRIV_DS}" "privileged: false" \
  "explicit agent.privileged=false must be honoured, not overridden by the default"
if render --set-string agent.privileged=yes >/dev/null 2>&1; then
  mark_fail "non-boolean agent.privileged must fail the render instead of silently dropping privilege"
fi

# Agent config file contract. agent.backends is rendered verbatim (same keys as
# PillarStore.spec.backend) into the agent-config ConfigMap, which the agent
# DaemonSet mounts and passes via --config; no per-backend CLI flag exists.
# Prints the config.yaml payload of the rendered agent-config ConfigMap.
agent_config_yaml() {
  extract_doc "$1" "agent-configmap.yaml" | awk '
    /^  config.yaml: \|$/ { in_cfg = 1; next }
    in_cfg && /^    / { print substr($0, 5); next }
    in_cfg && /^$/ { next }
    in_cfg { exit }
  '
}
assert_agent_config() {
  local body="$1" want="$2" description="$3" got
  got="$(agent_config_yaml "${body}")"
  if [[ "${got}" != "${want}" ]]; then
    mark_fail "${description}"
    echo "      expected agent config.yaml:"
    sed 's/^/        /' <<< "${want}"
    echo "      got:"
    sed 's/^/        /' <<< "${got}"
  fi
}

assert_contains "${AGENT_DS_DEFAULT}" "- --config=/etc/pillar-agent/config.yaml" \
  "default agent DaemonSet must pass --config pointing at the mounted config file"
assert_contains "${AGENT_DS_DEFAULT}" "name: pillar-csi-test-agent-config" \
  "default agent DaemonSet must mount the agent-config ConfigMap"
assert_contains "${AGENT_DS_DEFAULT}" "mountPath: /etc/pillar-agent" \
  "default agent DaemonSet must mount the config directory at /etc/pillar-agent"
assert_not_contains "${AGENT_DS_DEFAULT}" "--backend" \
  "agent DaemonSet must not render the removed --backend flag"
assert_agent_config "${DEFAULT_OUT}" "backends: []" \
  "default agent config must render an empty backends list"

BACKENDS_OK_OUT="$(render \
  --set 'agent.backends[0].zfs.volumeType=zvol' --set 'agent.backends[0].zfs.pool=tank' --set 'agent.backends[0].zfs.parentDataset=k8s' \
  --set 'agent.backends[1].lvm.volumeGroup=data-vg' --set 'agent.backends[1].lvm.thinPool=thin0' --set 'agent.backends[1].lvm.provisioningMode=thin' \
  --set 'agent.backends[2].zfs.pool=hot')"
assert_agent_config "${BACKENDS_OK_OUT}" "backends:
  - zfs:
      parentDataset: k8s
      pool: tank
      volumeType: zvol
  - lvm:
      provisioningMode: thin
      thinPool: thin0
      volumeGroup: data-vg
  - zfs:
      pool: hot" \
  "distinct agent.backends entries must render verbatim, in order, into the agent config file"
BACKENDS_OK_DS="$(extract_doc "${BACKENDS_OK_OUT}" "agent-daemonset.yaml")"
assert_not_contains "${BACKENDS_OK_DS}" "--backend" \
  "agent DaemonSet with backends must not render the removed --backend flag"
assert_not_contains "${BACKENDS_OK_DS}" "hostPID: true" \
  "configured block-only agent must not share host PIDs"
assert_not_contains "${BACKENDS_OK_DS}" "mountPath: /var/lib/nfs" \
  "configured block-only agent must not acquire server NFS state"
# The agent reads its config file only at startup, so the pod template must
# change whenever the rendered config changes.
checksum_of() { grep -o 'checksum/agent-config: [0-9a-f]*' <<< "$1" || true; }
if [[ -z "$(checksum_of "${AGENT_DS_DEFAULT}")" || "$(checksum_of "${AGENT_DS_DEFAULT}")" == "$(checksum_of "${BACKENDS_OK_DS}")" ]]; then
  mark_fail "agent pod template must carry a checksum/agent-config annotation that changes with agent.backends"
fi

# Dataset placement is a supported agent config, not a directory-backend
# substitute. Its server mount and recovery contract also holds with mTLS.
NFS_OUT="$(render \
  --set 'agent.backends[0].zfs.volumeType=dataset' \
  --set 'agent.backends[0].zfs.pool=tank')"
NFS_AGENT="$(extract_doc "${NFS_OUT}" "agent-daemonset.yaml")"
assert_agent_nfs_storage "${NFS_AGENT}" \
  "dataset-configured agent must preserve dataset mounts and private NFS recovery"
assert_contains "${NFS_AGENT}" "hostPID: true" \
  "NFS agent must see foreign listenerless mountd processes to refuse shared kernel upcalls"
NFS_MTLS_OUT="$(render --set mtls.enabled=true \
  --set 'agent.backends[0].zfs.volumeType=dataset' \
  --set 'agent.backends[0].zfs.pool=tank')"
assert_agent_nfs_storage "$(extract_doc "${NFS_MTLS_OUT}" "agent-daemonset.yaml")" \
  "dataset-configured mTLS agent must preserve dataset mounts and private NFS recovery"
for denied in agent.hostNetwork=false agent.privileged=false; do
  if render --set "${denied}" --set 'agent.backends[0].zfs.volumeType=dataset' \
    --set 'agent.backends[0].zfs.pool=tank' >/dev/null 2>&1; then
    mark_fail "dataset/NFS backend must reject ${denied}: host kernel serving and Bidirectional mounts require it"
  fi
done

# Union contract: each entry sets exactly one of zfs or lvm; the old flat
# {type,pool,vg,...} entry shape and unimplemented variants fail the render.
BOTH_ERR="$(render \
  --set 'agent.backends[0].zfs.pool=tank' --set 'agent.backends[0].lvm.volumeGroup=vg0' 2>&1 >/dev/null || true)"
assert_contains "${BOTH_ERR}" 'agent.backends[0]: exactly one of zfs or lvm must be set' \
  "an agent.backends entry setting both zfs and lvm must fail the render"
LEGACY_ERR="$(render \
  --set 'agent.backends[0].type=zfs-zvol' --set 'agent.backends[0].pool=tank' 2>&1 >/dev/null || true)"
assert_contains "${LEGACY_ERR}" 'agent.backends[0]: exactly one of zfs or lvm must be set' \
  "the removed flat agent.backends[].{type,pool} shape must fail the render"
UNSUPPORTED_ERR="$(render --set 'agent.backends[0].dir.path=/srv' 2>&1 >/dev/null || true)"
assert_contains "${UNSUPPORTED_ERR}" 'agent.backends[0].dir is not a supported backend' \
  "an unimplemented backend member must fail the render"
NO_POOL_ERR="$(render --set 'agent.backends[0].zfs.parentDataset=k8s' 2>&1 >/dev/null || true)"
assert_contains "${NO_POOL_ERR}" 'agent.backends[0].zfs.pool is required' \
  "a zfs entry without pool must fail the render"
NO_VG_ERR="$(render --set 'agent.backends[0].lvm.thinPool=thin0' 2>&1 >/dev/null || true)"
assert_contains "${NO_VG_ERR}" 'agent.backends[0].lvm.volumeGroup is required' \
  "an lvm entry without volumeGroup must fail the render"

# Backend registry key contract (issue #100): exact (pool, backend type)
# duplicates remain ambiguous. A zvol and a dataset placement on one ZFS pool
# are distinct, while an LVM VG cannot collide with either ZFS backend type.
if render \
  --set 'agent.backends[0].zfs.pool=tank' --set 'agent.backends[0].zfs.parentDataset=a' \
  --set 'agent.backends[1].zfs.pool=tank' --set 'agent.backends[1].zfs.parentDataset=b' \
  >/dev/null 2>&1; then
  mark_fail "two zvol placements on one ZFS pool must fail the render"
fi
if render \
  --set 'agent.backends[0].lvm.volumeGroup=vg0' \
  --set 'agent.backends[1].lvm.volumeGroup=vg0' --set 'agent.backends[1].lvm.thinPool=thin0' \
  >/dev/null 2>&1; then
  mark_fail "two agent.backends entries on one LVM VG must fail the render"
fi
if render \
  --set 'agent.backends[0].zfs.pool=shared' \
  --set 'agent.backends[1].lvm.volumeGroup=shared' >/dev/null 2>&1; then
  mark_fail "a ZFS pool and an LVM VG sharing one name must fail the render"
fi
# Keys are compared trimmed, so " tank " and "tank" collide at render time.
if render \
  --set 'agent.backends[0].zfs.pool=tank' \
  --set-string 'agent.backends[1].zfs.pool= tank ' \
  >/dev/null 2>&1; then
  mark_fail "agent.backends pool names differing only in whitespace must fail the render"
fi
if render \
  --set 'agent.backends[0].zfs.pool=tank' --set 'agent.backends[0].zfs.volumeType=dataset' \
  --set 'agent.backends[1].zfs.pool=tank' --set 'agent.backends[1].zfs.volumeType=dataset' \
  >/dev/null 2>&1; then
  mark_fail "two dataset placements on one ZFS pool must fail the render"
fi
MIXED_ZFS_OUT="$(render \
  --set 'agent.backends[0].zfs.pool=tank' --set 'agent.backends[0].zfs.volumeType=zvol' \
  --set 'agent.backends[1].zfs.pool=tank' --set 'agent.backends[1].zfs.volumeType=dataset')"
assert_agent_nfs_storage "$(extract_doc "${MIXED_ZFS_OUT}" "agent-daemonset.yaml")" \
  "same-pool zvol and dataset placements must coexist and retain NFS mount/recovery requirements"

assert_contains "${NODE_DS}" "terminationGracePeriodSeconds: 60" \
  "default node DaemonSet must set terminationGracePeriodSeconds=60"
# The node DaemonSet also carries the existing node-driver-registrar preStop
# (rm -rf the registration socket), so we expect the literal busybox sleep 5
# entry at least once for the node container itself.
assert_contains "${NODE_DS}" "command: [\"/bin/busybox\", \"sleep\", \"5\"]" \
  "default node DaemonSet must emit preStop busybox sleep 5 on the node container"


# Admission webhook deployment contract. The controller must receive a serving
# certificate and the API server must have both mutating and validating routes.
assert_contains "${CTL_DEP_DEFAULT}" "--webhook-cert-path=/tmp/k8s-webhook-server/serving-certs" \
  "default controller must use the chart-managed webhook serving certificate"
assert_contains "${CTL_DEP_DEFAULT}" "--webhook-port=9443" \
  "default controller must bind the configured webhook HTTPS port"
assert_contains "${CTL_DEP_DEFAULT}" "secretName: pillar-csi-test-webhook-cert" \
  "default controller must mount the release-scoped webhook certificate Secret"
assert_contains "${CTL_DEP_DEFAULT}" "containerPort: 9443" \
  "default controller must expose the webhook HTTPS port"
assert_contains "${WEBHOOK_DEFAULT}" "kind: Secret" \
  "default render must create the webhook TLS Secret"
assert_contains "${WEBHOOK_DEFAULT}" "kind: Service" \
  "default render must create the webhook Service"
assert_contains "${WEBHOOK_DEFAULT}" "kind: MutatingWebhookConfiguration" \
  "default render must register the PillarStorageClass defaulting webhook"
assert_contains "${WEBHOOK_DEFAULT}" "kind: ValidatingWebhookConfiguration" \
  "default render must register CRD validation webhooks"
assert_contains "${WEBHOOK_DEFAULT}" "caBundle:" \
  "default webhook configurations must trust the generated serving certificate"

WEBHOOK_PORT_OUT="$(render --set webhook.port=10443)"
WEBHOOK_PORT_CTL="$(extract_doc "${WEBHOOK_PORT_OUT}" "controller-deployment.yaml")"
WEBHOOK_PORT_RESOURCES="$(extract_doc "${WEBHOOK_PORT_OUT}" "webhook.yaml")"
assert_contains "${WEBHOOK_PORT_CTL}" "--webhook-port=10443" \
  "webhook.port override must reach the controller manager"
assert_contains "${WEBHOOK_PORT_CTL}" "containerPort: 10443" \
  "webhook.port override must reach the controller container port"
assert_contains "${WEBHOOK_PORT_RESOURCES}" "targetPort: webhook-server" \
  "webhook Service must continue routing through the named controller port"
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

# Probe named ports must resolve in the probing container, and no Pod may
# declare one port name twice (issue #60: duplicate csi-healthz warning).
assert_pod_ports_unambiguous "${CTL_DEP_DEFAULT}" \
  "default controller Pod ports must be unique and probe-resolvable"
assert_pod_ports_unambiguous "${NODE_DS}" \
  "default node Pod ports must be unique and probe-resolvable"
assert_pod_ports_unambiguous "${AGENT_DS_DEFAULT}" \
  "default agent Pod ports must be unique and probe-resolvable"

# CreateVolume reads the PVC override annotations and the claim identity from
# the claim name/namespace csi-provisioner passes only with this flag (#112).
assert_contains "${CTL_DEP_DEFAULT}" "- --extra-create-metadata" \
  "default csi-provisioner must pass --extra-create-metadata"

# ──────────────────────────────────────────────────────────────────────────
# Mode 1b: controller.replicaCount=2 (issue #96 — standby replicas)
# ──────────────────────────────────────────────────────────────────────────
# The standby-safety contract: every replica's pod-local CSI socket serves all
# four CSI sidecars, so scaling out must preserve the socket wiring, the
# sidecars' leader-election flags, and the liveness probe path verbatim.
HA_DEP="$(extract_doc "$(render --set controller.replicaCount=2)" "controller-deployment.yaml")"
assert_contains "${HA_DEP}" "replicas: 2" \
  "replicaCount=2 must reach the controller Deployment spec"
assert_contains "${HA_DEP}" "- --leader-elect" \
  "replicaCount=2: controller must still run with --leader-elect"
assert_min_count "${HA_DEP}" "--csi-address=/csi/csi.sock" 4 \
  "replicaCount=2: all 4 CSI sidecars must dial the pod-local socket"
assert_min_count "${HA_DEP}" "--leader-election$" 3 \
  "replicaCount=2: provisioner, attacher and resizer must keep leader election"
assert_contains "${HA_DEP}" "port: csi-healthz" \
  "replicaCount=2: controller liveness probe must keep targeting csi-healthz"
assert_pod_ports_unambiguous "${HA_DEP}" \
  "replicaCount=2 controller Pod ports must be unique and probe-resolvable"

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
assert_min_count "${AGT_DS}" "tcpSocket:" 2 \
  "mtls=on: agent liveness and readiness probes must use TCP (kubelet gRPC probes cannot do TLS)"
assert_not_contains "${AGT_DS}" "grpc:" \
  "mtls=on: agent probes must not use plaintext native gRPC probes"

# Secret-mode does not auto-render cert-manager resources.
assert_not_contains "${MTLS_OUT}" "kind: Issuer" \
  "mtls=on (secret mode): must NOT auto-render cert-manager Issuer"
assert_not_contains "${MTLS_OUT}" "kind: Certificate" \
  "mtls=on (secret mode): must NOT auto-render cert-manager Certificate"

# Secret-mode without explicit mtls.serverName: controller must NOT
# pass --agent-tls-server-name (operator-managed certs typically embed
# the node IP as SAN so the runtime-resolved server name is correct).
assert_not_contains "${CTL_DEP}" "--agent-tls-server-name" \
  "mtls=on (secret mode, default): must NOT pass --agent-tls-server-name"

# Secret-mode WITH explicit mtls.serverName: controller MUST pass it.
OVERRIDE_OUT="$(render --set mtls.enabled=true --set mtls.serverName=my-agent.example.svc)"
OVERRIDE_CTL="$(extract_doc "${OVERRIDE_OUT}" "controller-deployment.yaml")"
assert_contains "${OVERRIDE_CTL}" "--agent-tls-server-name=my-agent.example.svc" \
  "mtls=on (secret mode, serverName override): controller must pass --agent-tls-server-name=<override>"

# ──────────────────────────────────────────────────────────────────────────
# Mode 3: cert-manager mode
# ──────────────────────────────────────────────────────────────────────────
CM_OUT="$(render --set mtls.enabled=true --set mtls.certManager.enabled=true)"

assert_min_count "${CM_OUT}" "^kind: Issuer" 2 \
  "certManager=on (chart-managed CA): chart must render bootstrap Issuer + CA Issuer (2 total)"
assert_min_count "${CM_OUT}" "^kind: Certificate" 3 \
  "certManager=on (chart-managed CA): chart must render CA Certificate + controller leaf + agent leaf (3 total)"
assert_contains "${CM_OUT}" "isCA: true" \
  "certManager=on: chart must render an isCA Certificate so leaves share a single trust root"
assert_contains "${CM_OUT}" "ca:" \
  "certManager=on: chart must render a kind=ca Issuer that references the bootstrap CA secret"

# Operator-supplied issuerRef path: chart must NOT render the bootstrap
# Issuer or the CA Certificate (the operator owns the CA already), and
# both leaf Certificates must point at the supplied issuer.
OVERRIDE_CM_OUT="$(render --set mtls.enabled=true --set mtls.certManager.enabled=true --set mtls.certManager.issuerRef.name=my-org-ca --set mtls.certManager.issuerRef.kind=ClusterIssuer)"
assert_not_contains "${OVERRIDE_CM_OUT}" "kind: Issuer" \
  "certManager=on (operator issuerRef): chart must NOT render any in-cluster Issuer"
assert_not_contains "${OVERRIDE_CM_OUT}" "isCA: true" \
  "certManager=on (operator issuerRef): chart must NOT render a CA Certificate"
assert_min_count "${OVERRIDE_CM_OUT}" "^kind: Certificate" 2 \
  "certManager=on (operator issuerRef): chart must render the 2 leaf Certificates only"
assert_contains "${OVERRIDE_CM_OUT}" "name: my-org-ca" \
  "certManager=on (operator issuerRef): both leaves must reference the supplied issuer name"
assert_contains "${OVERRIDE_CM_OUT}" "kind: ClusterIssuer" \
  "certManager=on (operator issuerRef): leaves must honour issuerRef.kind override"

# Auto-generated Secret names propagate to the pod mounts.
CM_CTL_DEP="$(extract_doc "${CM_OUT}" "controller-deployment.yaml")"
assert_contains "${CM_CTL_DEP}" "secretName: ${RELEASE}-controller-mtls" \
  "certManager=on: controller deployment must mount auto-issued controller Secret"

CM_AGT_DS="$(extract_doc "${CM_OUT}" "agent-daemonset.yaml")"
assert_contains "${CM_AGT_DS}" "secretName: ${RELEASE}-agent-mtls" \
  "certManager=on: agent DaemonSet must mount auto-issued agent Secret"

# cert-manager mode: controller MUST pass --agent-tls-server-name matching
# the dnsNames the cert-manager Certificate generates, otherwise SAN
# verification fails when the controller dials a node IP.
assert_contains "${CM_CTL_DEP}" "--agent-tls-server-name=${RELEASE}-agent.default.svc" \
  "certManager=on: controller must pass --agent-tls-server-name matching the agent Certificate dnsName"

# ──────────────────────────────────────────────────────────────────────────
# Mode 4 (T9): metrics, tracing and PodMonitor wiring
# ──────────────────────────────────────────────────────────────────────────
# Kubelet expands $(VAR) in an env value only from entries listed earlier in
# the same container, so every variable OTEL_RESOURCE_ATTRIBUTES (or a
# $(HOST_IP) endpoint) references must come first.
assert_env_before() {
  local body="$1" var="$2" anchor="$3" description="$4" var_line anchor_line
  var_line="$(grep -n -m1 -- "- name: ${var}\$" <<< "${body}" | cut -d: -f1 || true)"
  anchor_line="$(grep -n -m1 -- "- name: ${anchor}\$" <<< "${body}" | cut -d: -f1 || true)"
  if [[ -z "${var_line}" || -z "${anchor_line}" ]] || (( var_line >= anchor_line )); then
    mark_fail "${description}"
    echo "      expected env ${var} (line ${var_line:-missing}) before ${anchor} (line ${anchor_line:-missing})"
  fi
}

# Default: tracing and the agent/node/sidecar metrics endpoints are off; the
# controller endpoint stays HTTPS with its authn/authz RBAC.
assert_not_contains "${DEFAULT_OUT}" "OTEL_" \
  "default render must NOT set any OTEL_* variable (tracing.enabled=false)"
for doc in "${AGENT_DS_DEFAULT}" "${NODE_DS}"; do
  assert_not_contains "${doc}" "--metrics-bind-address" \
    "default agent/node must NOT pass --metrics-bind-address (metrics.enabled=false)"
  assert_not_contains "${doc}" "name: metrics" \
    "default agent/node must NOT declare a metrics port (metrics.enabled=false)"
done
assert_not_contains "${CTL_DEP_DEFAULT}" "--http-endpoint" \
  "default CSI sidecars must NOT serve --http-endpoint (metrics.enabled=false)"
assert_contains "${CTL_DEP_DEFAULT}" "- --metrics-secure=true" \
  "default controller must serve metrics over HTTPS with authn/authz"
CR_DEFAULT="$(extract_doc "${DEFAULT_OUT}" "clusterrole.yaml")"
assert_contains "${CR_DEFAULT}" "- tokenreviews" \
  "default controller ClusterRole must allow TokenReview creation for secure metrics"
assert_contains "${CR_DEFAULT}" "- subjectaccessreviews" \
  "default controller ClusterRole must allow SubjectAccessReview creation for secure metrics"
assert_not_contains "${DEFAULT_OUT}" "kind: PodMonitor" \
  "default render must NOT include PodMonitors"
assert_not_contains "${DEFAULT_OUT}" "metrics-reader" \
  "default render must NOT include the metrics-reader identity"

# metrics.enabled: every new port is unique within its Pod, and the plaintext
# agent/node endpoints bind the host IP only.
METRICS_OUT="$(render --set metrics.enabled=true)"
METRICS_CTL="$(extract_doc "${METRICS_OUT}" "controller-deployment.yaml")"
METRICS_AGENT="$(extract_doc "${METRICS_OUT}" "agent-daemonset.yaml")"
METRICS_NODE="$(extract_doc "${METRICS_OUT}" "node-daemonset.yaml")"
assert_contains "${METRICS_AGENT}" '- --metrics-bind-address=[$(HOST_IP)]:9501' \
  "metrics=on: hostNetwork agent must bind metrics to [\$(HOST_IP)]:9501"
assert_contains "${METRICS_NODE}" '- --metrics-bind-address=[$(HOST_IP)]:9502' \
  "metrics=on: hostNetwork node must bind metrics to [\$(HOST_IP)]:9502"
for doc in "${METRICS_AGENT}" "${METRICS_NODE}"; do
  assert_contains "${doc}" "fieldPath: status.hostIP" \
    "metrics=on: agent/node must receive HOST_IP from the downward API"
done
assert_contains "${METRICS_AGENT}" "containerPort: 9501" \
  "metrics=on: agent must declare containerPort 9501"
assert_contains "${METRICS_NODE}" "containerPort: 9502" \
  "metrics=on: node must declare containerPort 9502"
for port in 8090 8091 8092; do
  assert_contains "${METRICS_CTL}" "- --http-endpoint=:${port}" \
    "metrics=on: a CSI sidecar must serve --http-endpoint=:${port}"
  assert_contains "${METRICS_CTL}" "containerPort: ${port}" \
    "metrics=on: a CSI sidecar must declare containerPort ${port}"
done
assert_pod_ports_unambiguous "${METRICS_CTL}" \
  "metrics=on controller Pod ports must be unique and probe-resolvable"
assert_pod_ports_unambiguous "${METRICS_AGENT}" \
  "metrics=on agent Pod ports must be unique and probe-resolvable"
assert_pod_ports_unambiguous "${METRICS_NODE}" \
  "metrics=on node Pod ports must be unique and probe-resolvable"
NOHOSTNET_AGENT="$(extract_doc "$(render --set metrics.enabled=true --set agent.hostNetwork=false)" "agent-daemonset.yaml")"
assert_contains "${NOHOSTNET_AGENT}" "- --metrics-bind-address=:9501" \
  "metrics=on, agent.hostNetwork=false: agent must bind metrics on the pod IP (:9501)"

# node.trim: weekly by default; a custom interval is passed through verbatim,
# and disabling renders --trim-interval=0 (the binary's own default is on).
assert_contains "${NODE_DS}" '- "--trim-interval=168h"' \
  "default node must trim weekly (--trim-interval=168h)"
TRIM_NODE="$(extract_doc "$(render --set node.trim.interval=30s)" "node-daemonset.yaml")"
assert_contains "${TRIM_NODE}" '- "--trim-interval=30s"' \
  "node.trim.interval=30s must render --trim-interval=30s"
NOTRIM_NODE="$(extract_doc "$(render --set node.trim.enabled=false)" "node-daemonset.yaml")"
assert_contains "${NOTRIM_NODE}" "- --trim-interval=0" \
  "node.trim.enabled=false must render --trim-interval=0"
assert_not_contains "${NOTRIM_NODE}" "--trim-interval=168h" \
  "node.trim.enabled=false must not also pass the default interval"

# tracing.enabled: OTEL_* env on all three binaries, resource attributes
# expanded from earlier downward-API entries; extraEnv stays last.
TRACING_OUT="$(render --set tracing.enabled=true --set tracing.endpoint=http://collector:4317 \
  --set 'agent.extraEnv[0].name=OVERRIDE_ME' --set 'agent.extraEnv[0].value=x')"
for tmpl in controller-deployment.yaml agent-daemonset.yaml node-daemonset.yaml; do
  doc="$(extract_doc "${TRACING_OUT}" "${tmpl}")"
  assert_contains "${doc}" 'value: "http://collector:4317"' \
    "tracing=on: ${tmpl} must set OTEL_EXPORTER_OTLP_ENDPOINT from tracing.endpoint"
  assert_contains "${doc}" "- name: OTEL_EXPORTER_OTLP_INSECURE" \
    "tracing=on: ${tmpl} must set OTEL_EXPORTER_OTLP_INSECURE"
  assert_contains "${doc}" "- name: OTEL_TRACES_SAMPLER_ARG" \
    "tracing=on: ${tmpl} must set OTEL_TRACES_SAMPLER_ARG"
  assert_contains "${doc}" 'value: "k8s.namespace.name=$(POD_NAMESPACE),k8s.pod.name=$(POD_NAME),k8s.node.name=$(NODE_NAME)"' \
    "tracing=on: ${tmpl} must set OTEL_RESOURCE_ATTRIBUTES from the downward API"
  for var in POD_NAMESPACE POD_NAME NODE_NAME; do
    assert_env_before "${doc}" "${var}" "OTEL_RESOURCE_ATTRIBUTES" \
      "tracing=on: ${tmpl} must declare ${var} before OTEL_RESOURCE_ATTRIBUTES"
  done
  assert_env_before "${doc}" "HOST_IP" "OTEL_EXPORTER_OTLP_ENDPOINT" \
    "tracing=on: ${tmpl} must declare HOST_IP before OTEL_EXPORTER_OTLP_ENDPOINT (node-local collector endpoints)"
done
assert_env_before "$(extract_doc "${TRACING_OUT}" "agent-daemonset.yaml")" "OTEL_RESOURCE_ATTRIBUTES" "OVERRIDE_ME" \
  "tracing=on: agent.extraEnv must stay after the OTEL_* env so it can override it"
NO_ENDPOINT_ERR="$(render --set tracing.enabled=true 2>&1 >/dev/null || true)"
assert_contains "${NO_ENDPOINT_ERR}" "tracing.endpoint is required when tracing.enabled is true" \
  "tracing.enabled with an empty tracing.endpoint must fail the render with a clear error"

# PodMonitors: the controller endpoint is scraped over HTTPS with the chart's
# metrics-reader token; the render refuses clusters without the PodMonitor API.
PM_ARGS=(--set metrics.enabled=true --set metrics.podMonitor.enabled=true --set metrics.podMonitor.interval=30s)
NO_PM_API_ERR="$(render "${PM_ARGS[@]}" 2>&1 >/dev/null || true)"
assert_contains "${NO_PM_API_ERR}" "requires the prometheus-operator PodMonitor CRD (monitoring.coreos.com/v1/PodMonitor)" \
  "podMonitor.enabled without the monitoring.coreos.com/v1 PodMonitor API must fail the render clearly"
PM_OUT="$(render "${PM_ARGS[@]}" --api-versions monitoring.coreos.com/v1/PodMonitor)"
assert_min_count "${PM_OUT}" "^kind: PodMonitor" 3 \
  "podMonitor=on, metrics=on: controller, agent and node PodMonitors must render"
assert_contains "${PM_OUT}" "scheme: https" \
  "podMonitor=on: the secure controller endpoint must be scraped over https"
assert_contains "${PM_OUT}" "type: Bearer" \
  "podMonitor=on: the secure controller endpoint must send a bearer token"
assert_contains "${PM_OUT}" "name: ${RELEASE}-metrics-reader-token" \
  "podMonitor=on: the controller endpoint must read the metrics-reader token Secret"
assert_contains "${PM_OUT}" "insecureSkipVerify: true" \
  "podMonitor=on: the controller endpoint must accept the self-signed metrics certificate"
for port in prov-metrics attach-metrics resize-metrics; do
  assert_contains "${PM_OUT}" "- port: ${port}" \
    "podMonitor=on, metrics=on: the controller PodMonitor must scrape sidecar port ${port}"
done
assert_contains "${PM_OUT}" 'interval: "30s"' \
  "podMonitor=on: metrics.podMonitor.interval must reach the endpoints"
assert_contains "${PM_OUT}" "type: kubernetes.io/service-account-token" \
  "podMonitor=on: the metrics-reader token Secret must render"
assert_contains "${PM_OUT}" "kubernetes.io/service-account.name: ${RELEASE}-metrics-reader" \
  "podMonitor=on: the token Secret must be bound to the metrics-reader ServiceAccount"
assert_contains "${PM_OUT}" "- /metrics" \
  "podMonitor=on: the metrics-reader ClusterRole must allow get on /metrics"
PM_INSECURE_OUT="$(render --set metrics.podMonitor.enabled=true --set metrics.controller.secure=false \
  --api-versions monitoring.coreos.com/v1/PodMonitor)"
assert_contains "${PM_INSECURE_OUT}" "- --metrics-secure=false" \
  "metrics.controller.secure=false must pass --metrics-secure=false"
assert_not_contains "${PM_INSECURE_OUT}" "tokenreviews" \
  "metrics.controller.secure=false must not grant TokenReview creation"
assert_not_contains "${PM_INSECURE_OUT}" "metrics-reader" \
  "metrics.controller.secure=false must not render the metrics-reader identity"
assert_not_contains "${PM_INSECURE_OUT}" "scheme: https" \
  "metrics.controller.secure=false must scrape the controller over plain http"
if [[ "$(grep -c '^kind: PodMonitor' <<< "${PM_INSECURE_OUT}" || true)" != 1 ]]; then
  mark_fail "podMonitor=on, metrics=off: only the controller PodMonitor must render"
fi
if render --set-string metrics.controller.secure=yes >/dev/null 2>&1; then
  mark_fail "non-boolean metrics.controller.secure must fail the render"
fi

# ──────────────────────────────────────────────────────────────────────────
# API contract: rendered CRDs and RBAC vs controller-gen output
# ──────────────────────────────────────────────────────────────────────────
# Decodes the rendered objects (not text) and compares them with
# config/crd/bases and config/rbac/role.yaml. installCRDs=false must still
# grant RBAC only on resources the separately applied generated CRDs serve.
REPO_ROOT="$(cd "${CHART_DIR}/../.." && pwd)"
check_api_contract() {
  local mode="$1"; shift
  if ! render "$@" | (cd "${REPO_ROOT}" && go run ./hack/chartcontract -controller-role "${RELEASE}" ${mode:+"${mode}"}); then
    mark_fail "chart API contract (${*:-default values}) must match controller-gen CRDs and RBAC"
  fi
}
check_api_contract ""
check_api_contract "-expect-no-crds" --set installCRDs=false

# ──────────────────────────────────────────────────────────────────────────
# Final verdict
# ──────────────────────────────────────────────────────────────────────────
if (( fail != 0 )); then
  echo
  echo "Chart render regression test FAILED."
  exit 1
fi

echo "Chart render regression test passed."
