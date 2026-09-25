# pillar-csi

pillar-csi is a Go-based Kubernetes CSI driver that exports local ZFS zvols and LVM logical volumes from a storage node to the rest of the cluster over NVMe-oF/TCP. It runs as three workloads — a controller, a per-worker node plugin, and a per-storage-node agent that writes directly to the kernel via configfs — all configured declaratively through cluster-scoped CRDs. It is not a distributed filesystem. It exports the storage you already have.

[![CI](https://github.com/isac322/pillar-csi/actions/workflows/ci.yml/badge.svg)](https://github.com/isac322/pillar-csi/actions/workflows/ci.yml) [![Go Report Card](https://goreportcard.com/badge/github.com/isac322/pillar-csi)](https://goreportcard.com/report/github.com/isac322/pillar-csi) [![Go](https://img.shields.io/github/go-mod/go-version/isac322/pillar-csi?color=00ADD8)](go.mod) [![Kubernetes ≥ 1.24](https://img.shields.io/badge/kubernetes-%E2%89%A5%201.24-blue?logo=kubernetes)](https://kubernetes.io) [![License](https://img.shields.io/github/license/isac322/pillar-csi?color=green)](LICENSE) [![Release](https://img.shields.io/github/v/release/isac322/pillar-csi?include_prereleases&color=orange)](https://github.com/isac322/pillar-csi/releases)

- **Is**: A CSI driver for self-hosted bare-metal Kubernetes clusters that takes local ZFS zvols or LVM logical volumes on a dedicated storage node and exports them over NVMe-oF/TCP using kernel-native configfs writes — no SSH, no Python daemons, no external target CLI.
- **Is not**: A distributed filesystem; pillar-csi does not replicate, stripe, or pool storage across nodes. It exports the storage you already have, as-is.

[Install](#install) · [Quickstart](#quickstart) · [Architecture](#architecture) · [Docs](docs/PRD.md) · [Roadmap](#roadmap)

## Why pillar-csi

| Concern | democratic-csi | pillar-csi |
|---|---|---|
| Language / footprint | Node.js | Go — single static binary |
| Deployment model | One Helm release per backend (controller + node DaemonSet duplicated) | Single cluster deployment, declarative `Pillar*` CRDs |
| Multi-pool | Extra Helm release per pool (SSH config, RBAC, sidecars duplicated) | Add one `PillarStore` CR |
| Storage-node IPC | SSH (parses shell output, key management, injection risk) | gRPC agent (typed, auto-reconnect, mTLS-capable) |
| Target configuration | `targetcli` / `nvmetcli` CLI (Python dependency) | Direct configfs writes with read-back verification |
| Node prerequisites | open-iscsi / nvme-cli pre-installed on every worker | Bundled in node image + init-container `modprobe` |
| Parameter overrides | StorageClass parameters + PVC annotation | 4-layer hierarchy: Pool → Protocol → Binding → PVC annotation |
| Backend / protocol extension | Driver-type hard-coded (`zfs-generic-iscsi`, …) | `Backend` and `Protocol` plugin interfaces |

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     storage node                         │
│                                                         │
│   ZFS pool (zvol)     LVM VG (LV)     /data (dir)…       │
│         │                  │                │            │
│         └──────────────────┴────────────────┘            │
│                            │                            │
│                      pillar-agent                       │
│              gRPC server + direct configfs              │
│                            │                            │
│              ┌───────────┼───────────┐              │
│           NVMe-oF/TCP   iSCSI*         NFS*              │
└─────────────┼───────────┼───────────┼──────────────────┘
              │             │             │
              ▼             ▼             ▼
         /dev/nvmeXnY    /dev/sdX     mount point
                         worker node (Pod)
```

The control plane (`pillar-controller`) runs as a `Deployment` and reconciles the `Pillar*` CRDs. `pillar-agent` runs as a `DaemonSet` on storage nodes only (auto-labelled when a `PillarAgent` CR is created) and owns all configfs writes on the host; `pillar-node` runs on every worker and handles the CSI Node service — initiator connect, mkfs, bind-mount. Both DaemonSets use `hostNetwork: true` so the NVMe-oF/TCP data plane can bind to the host network namespace.

| CRD | Purpose |
|---|---|
| `PillarAgent` | Locates a storage agent (in-cluster `nodeRef` or external address) |
| `PillarStore` | A storage pool on a target — ZFS pool name, LVM VG, and backend config |
| `PillarProtocol` | Network protocol configuration (NVMe-oF/TCP, iSCSI, NFS, SMB) |
| `PillarStorageClass` | Pool × Protocol → auto-generated `StorageClass` |

`PillarVolumeState` is an internal durable-state CRD used to recover from partial provisioning failures and to record which nodes a volume is published to; users do not author it. The controller enforces CSI access-mode exclusivity from that record: a `SINGLE_NODE_*` (RWO/RWOP) volume published to one node is rejected on any other node with `FAILED_PRECONDITION` until it is unpublished, and a published volume cannot be deleted.

### Stale-operation fencing

Every agent call that changes a volume's resources carries a fencing token: backend create, expand, and delete; export and unexport; initiator grant and revoke; and state resync. The token has two parts. The first is the UID of the volume's `PillarVolumeState`, which identifies one lifecycle of the volume name; deleting and re-creating a volume with the same name produces a new UID. The second is `status.publicationGeneration`, which the controller bumps with a compare-and-swap for each operation. Every compare-and-swap is pinned to the UID the operation started with.

The agent keeps a durable mark per volume: the owning lifecycle, the highest generation applied, whether that lifecycle has ended, and the lifecycles that came before it. It checks each request against the mark in the same critical section that performs the change. It rejects with `FAILED_PRECONDITION` a request that is older than the mark, that comes from a lifecycle that ended or was replaced, or that carries no token at all. The effect: an RPC still in flight from a former controller leader (a paused process, or a request delayed on the network) cannot grant, revoke, export, create, expand, or destroy anything a newer operation or a newer lifecycle owns. `DeleteVolume` first sets `status.deleting`, which only succeeds while the volume is not published. After that, publishing, creating, or exporting the volume fails. A backend delete ends the lifecycle only once it has succeeded. An unpublish marks its records `revoking` in the same write that allocates its token, so a state resync at that generation does not re-grant the node being revoked. A publish of that node fails with `ABORTED` until the unpublish finishes.

`CreateVolume` creates the `PillarVolumeState` before any agent call, so a volume without one owns nothing on any agent, and deleting it needs no agent call.

The agent stores these marks on the storage node's local disk under `/var/lib/pillar-csi/agent/generations/`. The chart mounts that path into the agent DaemonSet as a `hostPath`, never a PVC, so the storage node does not depend on its own volumes, and the marks survive agent restarts and node reboots. Marks are never removed. If the state directory itself is lost (for example the host path is wiped), the fencing history is lost with it.

Out of scope: `SendVolume` and `ReceiveVolume` are out-of-band data streams the controller does not call, and they carry no token.

**Upgrade (clean cutover):** detach every volume (no `VolumeAttachment` for this driver) before upgrading to this version. Earlier versions recorded no publications, lifecycles, or generations, so the new controller cannot revoke access granted by the old one. No migration shim is provided.

### Node stage state

`NodeStageVolume` records each staged volume in `/var/lib/pillar-csi/node/` on the worker: its access type and the transport session to disconnect. `NodeUnstageVolume` reads that record back, because the CO sends neither a volume capability nor a volume context on unstage. The chart mounts the directory into the node DaemonSet as a `hostPath`, so the records survive node plugin restarts and rollouts. Each record is replaced atomically — the JSON is written to a same-directory temporary file, synced, renamed over the previous record, and the directory chain is synced — and success is acknowledged only after the file and directory syncs complete. A failed replacement before the rename preserves the previous record. A record that is nonetheless corrupt (for example, a 0-byte file left by an older version) remains an error: `NodeStageVolume` and `NodeUnstageVolume` fail rather than guess transport state.

If a record is missing while the staging path (Filesystem) or its `device` bind target (Block) is still mounted, `NodeUnstageVolume` fails instead of reporting the volume unstaged. Without the record the plugin cannot tell which session to detach, so it leaves the mount and the session alone. To recover, stage the volume again (for example, by starting a pod that uses it on the same node), which rewrites the record. The next unstage then completes normally. A missing record with nothing mounted counts as already unstaged.

## Supported matrix

| Backend | NVMe-oF/TCP | iSCSI | NFS |
|---|:---:|:---:|:---:|
| ZFS zvol | ✅ | 🚧 | — |
| LVM LV | ✅ | 🚧 | — |
| ZFS dataset | — | — | 🚧 |

✅ Shipped · 🚧 Designed, not yet shipped · — Not applicable

CSI operations: `CreateVolume`, `DeleteVolume`, `ControllerPublish/Unpublish`, `ControllerExpandVolume`, `NodeStage/Unstage`, `NodePublish/Unpublish`, `NodeExpandVolume`, `NodeGetVolumeStats`, `ValidateVolumeCapabilities`, `GetCapacity`.

Access modes: `ReadWriteOnce`, `ReadWriteOncePod`, `ReadOnlyMany`. Volume modes: `Filesystem` (ext4/xfs) and `Block`.

## Install

```sh
helm install pillar-csi charts/pillar-csi \
  --namespace pillar-csi --create-namespace
```

mTLS between the controller and agent is opt-in; the default is plaintext gRPC. Choose one mode:

```sh
# cert-manager mode: certificates auto-issued
helm install pillar-csi charts/pillar-csi \
  --namespace pillar-csi --create-namespace \
  --set mtls.enabled=true \
  --set mtls.certManager.enabled=true

# Secret mode: supply your own certificates
helm install pillar-csi charts/pillar-csi \
  --namespace pillar-csi --create-namespace \
  --set mtls.enabled=true \
  --set mtls.secretRefs.controller.secretName=ctl-mtls \
  --set mtls.secretRefs.agent.secretName=agt-mtls
```

**Kubernetes ≥ 1.24** is required (native `grpc:` liveness/readiness probes; GA in 1.27).

**Kernel modules:** storage nodes need `nvmet` and `nvmet_tcp`; worker nodes need `nvme_tcp` and `nvme_fabrics`. The agent and node init-containers run `modprobe` on startup — the host kernel must include these modules (vanilla Linux ≥ 5.0 is sufficient for NVMe-oF/TCP).

## Quickstart

Apply target, pool, protocol, binding once per cluster, then provision PVCs against the generated `StorageClass`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
metadata:
  name: rock5bp
spec:
  nodeRef:
    name: rock5bp
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: rock5bp-hot
spec:
  agentRef: rock5bp
  backend:
    type: zfs-zvol
    zfs:
      pool: tank
      parentDataset: k8s
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nvmeof-default
spec:
  type: nvmeof-tcp
  nvmeofTcp:
    port: 4420
    acl: true
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: hot
spec:
  storeRef: rock5bp-hot
  protocolRef: nvmeof-default
  storageClass:
    name: pillar-hot
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: pillar-hot
  resources:
    requests:
      storage: 10Gi
```

## Troubleshooting

Everything is reflected in standard Kubernetes resources — no extra CLI needed:

```sh
kubectl describe pillaragent rock5bp        # AgentConnected / Ready conditions
kubectl describe pillarstore   rock5bp-hot    # PoolDiscovered / BackendSupported
kubectl describe pillarstorageclass hot           # PoolReady / ProtocolValid / Compatible / StorageClassCreated
kubectl describe pillarvolumestate <name>         # internal volume state after provisioning
kubectl describe pvc data                    # provisioner events
kubectl get events --field-selector reason=ProvisioningFailed
```

Controller and node logs:

```sh
kubectl logs -n pillar-csi deploy/pillar-csi-controller -c manager
kubectl logs -n pillar-csi ds/pillar-csi-node          -c node
kubectl logs -n pillar-csi ds/pillar-csi-agent         -c agent
```

### Legacy volumes stuck at `ExportSpecMissing` (issue #83)

The `ExportReconciled` condition on every `PillarVolumeState` reports whether the storage node's kernel export still matches durable desired state. `False` with reason `ExportSpecMissing` means the volume was provisioned before `status.exportSpec` was recorded, so after the storage node lost its ephemeral target state (agent restart, node reboot, nvmet reload) the resync controller had no durable spec to rebuild from and failed closed. Volumes created after the change record `exportSpec` at `CreateVolume` and recover automatically; legacy volumes need a one-time manual repair, after which they self-heal like new ones.

There is no automatic inference of the missing spec, and this procedure deliberately keeps it that way. `status.exportInfo` is a runtime observation (`targetID`, `address`, `port`, `volumeRef`) — it does not record `aclEnabled`, and its `address`/`port` describe the last live endpoint, which may not equal the original provisioning inputs (renumbered storage network, replaced PillarProtocol, port moved since). Do not copy from `exportInfo` or from the *current* PillarProtocol/PillarStorageClass/PillarAgent CRs. Guessing the security fields is dangerous in both directions: enabling ACL on a volume that ran open breaks every consumer; disabling it on a volume that required ACL enforcement silently exposes the LUN to the whole network. Use the original provisioning inputs — or make an explicit, recorded operator decision — as described in step 3.

**Scope and prerequisites**
- Applies only when `status.exportSpec` is absent *and* the condition reports `reason: ExportSpecMissing`. Other reasons (`AgentUnavailable`, `StaleGeneration`, `ReconcileFailed`) mean a spec exists or another fault must be fixed first — do not patch. Protocols whose export carries no bind address/port never get an `exportSpec` by design, so this procedure currently applies to `nvmeof-tcp` volumes (and `iscsi` when it ships).
- You need `patch` on `pillarvolumestates/status` (cluster-scoped; e.g. cluster-admin).
- The volume's `PillarAgent` must be reachable (`AgentConnected` condition `True` on `kubectl describe pillaragent <spec.agentRef>`).
- Treat every volume independently; repair one at a time and verify before the next.
- If at any step a check fails, the object changed under you, or you cannot establish a value with confidence: **stop**. Leave the volume in `ExportSpecMissing`; an offline volume is recoverable, a wrongly exported one may not be.

1. **Identify affected volumes and pin the context.** Pick the cluster context once and pin every command below to it — a context switch mid-procedure would redirect your reads and writes:

   ```sh
   kubectl config get-contexts          # choose deliberately
   CTX=<cluster-context>
   ```

   Both `status.exportSpec == null` and the `ExportSpecMissing` reason must hold:

   ```sh
   kubectl --context "$CTX" get pvst -o json | jq -r '
     .items[]
     | select(.status.exportSpec == null)
     | select([.status.conditions[]? | select(.type=="ExportReconciled" and .reason=="ExportSpecMissing")] | length > 0)
     | .metadata.name'
   ```

   Take **one** snapshot of the chosen `PillarVolumeState` and derive every identity from it — this single read is what step 2 verifies and what step 4's patch is pinned to. Never re-resolve identity from a second read:

   ```sh
   PVST=<pvst-name>
   PVS_JSON=$(kubectl --context "$CTX" get pvst "$PVST" -o json)
   PVS_UID=$(jq -r '.metadata.uid' <<<"$PVS_JSON")             # lifecycle identity
   PVS_RV=$(jq -r '.metadata.resourceVersion' <<<"$PVS_JSON") # snapshot for the CAS guard
   VID=$(jq -r '.spec.volumeID' <<<"$PVS_JSON")               # CSI volume ID == PV volumeHandle
   ```

   Resolve the PV and its claim from one PV read. Exactly one PV must match the volume handle — zero or many abort inside jq; stop there:

   ```sh
   PV_JSON=$(kubectl --context "$CTX" get pv -o json)
   PV=$(jq -r --arg vid "$VID" '[.items[]
        | select(.spec.csi.driver=="pillar-csi.bhyoo.com" and .spec.csi.volumeHandle==$vid)]
        | if length == 1 then .[0].metadata.name
          else error("PV identity ambiguous: " + (length|tostring) + " matches") end' <<<"$PV_JSON")
   read -r CLAIM_NS CLAIM_NAME CLAIM_UID < <(jq -r --arg pv "$PV" '
        .items[] | select(.metadata.name==$pv)
        | "\(.spec.claimRef.namespace) \(.spec.claimRef.name) \(.spec.claimRef.uid)"' <<<"$PV_JSON")
   PVC_JSON=$(kubectl --context "$CTX" get pvc "$CLAIM_NAME" -n "$CLAIM_NS" -o json)
   ```

2. **Back up the captured snapshots and verify the full identity chain** before touching anything:

   ```sh
   mkdir -p pvs-recovery-"$PVST" && cd pvs-recovery-"$PVST"
   jq . <<<"$PVS_JSON" > pvst-backup.json
   jq --arg pv "$PV" '.items[] | select(.metadata.name==$pv)' <<<"$PV_JSON" > pv-backup.json
   jq . <<<"$PVC_JSON" > pvc-backup.json
   ```

   Verify every link binds in both directions — all assertions run against the snapshots just saved:

   ```sh
   # PVS: same lifecycle UID and volumeID, still spec-less, not being deleted
   jq -e --arg uid "$PVS_UID" --arg vid "$VID" 'select(
       .metadata.uid == $uid and .spec.volumeID == $vid
       and .status.exportSpec == null
       and .metadata.deletionTimestamp == null and .status.deleting != true)' pvst-backup.json
   # PV: same name, this driver, this volumeHandle, claimRef UID is the captured claim
   jq -e --arg pv "$PV" --arg vid "$VID" --arg claim "$CLAIM_UID" 'select(
       .metadata.name == $pv and .spec.csi.driver=="pillar-csi.bhyoo.com"
       and .spec.csi.volumeHandle == $vid and .spec.claimRef.uid == $claim)' pv-backup.json
   # PVC: the claimed UID is a live PVC Bound to this PV
   jq -e --arg pv "$PV" --arg uid "$CLAIM_UID" 'select(
       .metadata.uid == $uid and .status.phase == "Bound" and .spec.volumeName == $pv)' pvc-backup.json
   # Backend volume exists on the storage node (resync never re-creates it):
   #   spec.agentVolumeID is "<pool>/<vol>" or "<vg>/<vol>" —
   #   zfs list <agentVolumeID>  (zfs-zvol)   |   lvs <pool>/<vol>  (lvm-lv)
   jq -r '.spec.agentVolumeID, .spec.agentRef' pvst-backup.json
   ```

   Non-empty `status.publishedNodes` is expected when consumers were still attached at the loss — those records are the ACL source the resync will re-apply; do not edit them.

3. **Decide the export intent explicitly — there are no defaults.** Three fields, all required by the schema (`bindAddress` non-empty, `port` 0–65535, `aclEnabled` boolean). You must supply explicit, validated values in the block below; step 4 refuses to run with them unset. Decide each from *provision-time* evidence — never from `status.exportInfo` alone and never from the current values of `PillarProtocol`/`PillarStorageClass`/`PillarAgent`, which may have changed since:

   | Field | Authoritative inputs | Evidence, not truth |
   |---|---|---|
   | `bindAddress` | **Explicit operator decision.** No durable record of the *requested* bind exists for legacy volumes — `pv.spec.csi.volumeAttributes["address"]` and `status.exportInfo.address` record only the endpoint the agent actually exported at provision time, so they corroborate but do not authorize. Normally choose that same address. ⚠️ The PV endpoint is *not* updated by this patch — consumers keep dialing `volumeAttributes.address`/`port`, so a changed bind can converge to `ExportReconciled=True` while consumers still connect to the old endpoint; a changed endpoint needs a separate consumer-connectivity plan, not this procedure | `exportInfo.address` on its own |
   | `port` | Explicit value; `volumeAttributes["port"]` and `exportInfo.port` record what the export used, and a flat PVC annotation `pillar-csi.bhyoo.com/param.nvmeof-port` (or `param.iscsi-port`) could have overridden the class default (`4420` / `3260`) — cross-check before reusing any of them | current StorageClass/`PillarProtocol.spec.nvmeofTcp.port` — may be regenerated |
   | `aclEnabled` | **No durable provision-time record exists** — this is why the controller refuses to guess. Reconstruct it from the flat PVC annotation `pillar-csi.bhyoo.com/param.acl-enabled` (the only per-volume override of the class value), the provision-time `acl-enabled` StorageClass parameter (generated from `PillarProtocol.spec.nvmeofTcp.acl`; usable only if the protocol/binding CRs provably have not changed — GitOps history, snapshot, audit), or an explicit recorded operator decision. **If evidence cannot justify `true` or `false`, stop — do not patch.** `true` admits only `publishedNodes` initiators (`revoking` excluded; empty set = nobody, and the resync can still report `Reconciled` while unrecorded consumers stay locked out). `false` writes `attr_allow_any_host=1`, exposing the volume network-wide. Deliberately changing the historical intent is allowed only after recording the choice and its connectivity/security consequences | `exportInfo` has no ACL field; today's `PillarProtocol` value proves nothing about the original |

   Inspect the evidence:

   ```sh
   kubectl --context "$CTX" get pv "$PV" -o jsonpath='{.spec.csi.volumeAttributes}'
   kubectl --context "$CTX" get pvc "$CLAIM_NAME" -n "$CLAIM_NS" -o jsonpath='{.metadata.annotations}'
   ```

   Then set explicit values — unset or malformed values abort the procedure:

   ```sh
   BIND_ADDRESS=   PORT=   ACL_ENABLED=   # REQUIRED — explicit decision, no defaults
   : "${BIND_ADDRESS:?set from step 3 evidence}" \
     "${PORT:?set from step 3 evidence}" \
     "${ACL_ENABLED:?set to true or false explicitly}"
   case "$ACL_ENABLED" in true|false) ;; *) echo "ACL_ENABLED must be 'true' or 'false'"; exit 1;; esac
   case "$PORT" in ''|*[!0-9]*) echo "PORT must be a non-negative integer"; exit 1;; esac
   ```

4. **Patch `/status` with a JSON patch pinned to the verified snapshot.** The `test` ops compare-and-swap against the UID and resourceVersion of the exact snapshot verified in step 2 — the write can never land on a different lifecycle or a changed object, and can never be a blind overwrite:

   ```sh
   kubectl --context "$CTX" patch pvst "$PVST" --subresource=status --type=json -p "$(jq -cn \
     --arg uid "$PVS_UID" --arg rv "$PVS_RV" \
     --arg bind "$BIND_ADDRESS" --argjson port "$PORT" --argjson acl "$ACL_ENABLED" '[
       {op:"test", path:"/metadata/uid",            value:$uid},
       {op:"test", path:"/metadata/resourceVersion", value:$rv},
       {op:"add",  path:"/status/exportSpec",
        value:{bindAddress:$bind, port:$port, aclEnabled:$acl}}
     ]')"
   ```

   If the patch fails (a failed `test` returns `the server rejected our request`, or a conflict error), the object changed between your verified snapshot and the write — **stop, re-read, and restart from step 2. Never refresh the resourceVersion merely to make a retry succeed**: a changed object invalidates the identity and intent you verified.

   The patch touches only `/status/exportSpec`. Do not edit `status.publishedNodes`, `status.publicationGeneration`, `status.deleting`, conditions, or `spec.*`; do not touch the agent's fencing marks under `/var/lib/pillar-csi/agent/generations/` on the storage host; and do not touch the backend volume — the resync only re-creates the kernel export and ACL via the agent (fenced by PVS UID + `publicationGeneration`); it never creates, formats, or deletes backend storage.

5. **Wait for convergence, then verify data through the consumer that already exists.** The PillarVolumeState watch fires on the status update (a 30 s periodic resync is the fallback), so no controller restart is needed:

   ```sh
   kubectl --context "$CTX" wait pvst/"$PVST" \
     --for=jsonpath='{.status.conditions[?(@.type=="ExportReconciled")].status}'=True --timeout=60s
   kubectl --context "$CTX" get pvst "$PVST" \
     -o jsonpath='{.status.conditions[?(@.type=="ExportReconciled")].reason}'
   # must print: Reconciled
   ```

   A still-`False` condition carries the next blocker in its `reason`/`message` (`AgentUnavailable`, `StaleGeneration`, `ReconcileFailed`) — fix that cause; the durable `exportSpec` makes the controller retry on its own, so do not re-patch.

   `ExportReconciled=True` proves the kernel export and ACL exist again — it does not prove bytes are reachable or intact. Check the data through the **existing** consumer first; if the node kept its session and mount, this can already pass and nothing else is needed:

   ```sh
   kubectl --context "$CTX" -n "$CLAIM_NS" exec <consumer-pod> -- sha256sum <known-file-or-device-offset>
   ```

   Only if connectivity really is lost, restart the consumer **stop-before-start**, using whatever operator-approved action fits that workload: fully stop the old consumer, wait until the pod is terminated and the volume is released (pod gone, `VolumeAttachment` removed), and only then start the replacement. Never let two consumers run at once — `ReadWriteOnce` is not a single-pod guarantee (same-node mounts can coexist; a second node cannot publish while the volume is already published elsewhere), so rolling restarts and unpinned pod deletes are unsafe here. A pod restart only re-stages/reconnects when the stage record or session is actually gone; a surviving staged mount is reused by the replacement pod. Re-run the hash check on the replacement before declaring the volume recovered.

**Caveat — one repair, validated once.** The four QA volumes of issue #83 that were repaired with this procedure are historical evidence for those volumes only — not proof that any future legacy volume's recorded intent is correct. Each repaired volume becomes durable (its `exportSpec` persists in etcd, so later target-state losses self-heal), but each remaining `ExportSpecMissing` volume needs this same explicit procedure with its own verified inputs. No automatic guessing is added to the controller: that fail-closed behavior is intentional.

## Documentation

- [`docs/PRD.md`](docs/PRD.md) — product requirements: architecture, CRDs, lifecycle
- [`docs/PRD-iscsi.md`](docs/PRD-iscsi.md) — iSCSI design (Phase 2)
- [`docs/RFC-multi-protocol-driver-foundation.md`](docs/RFC-multi-protocol-driver-foundation.md) — multi-protocol driver foundation RFC
- [`docs/prd-audit-phase1-2026-06.md`](docs/prd-audit-phase1-2026-06.md) — Phase 1 readiness audit (June 2026)
- [`docs/decisions/`](docs/decisions/) — architecture decision records

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1 | ZFS zvol + NVMe-oF/TCP, full CSI block lifecycle, Helm chart, mTLS opt-in, graceful drain; LVM LV shipped early and remains tracked for Phase 5 hardening | **Shipped** |
| 2 | iSCSI protocol (LIO configfs) | In progress |
| 3 | ZFS dataset backend + NFS protocol + `RWX` | Planned |
| 4 | CSI snapshots / clones (ZFS native) | Planned |
| 5 | Standalone LVM backend hardening | Planned |
| 6 | SMB protocol | Planned |
| 7 | External (non-K8s) agent nodes | Planned |
| 8 | Additional backends: raw block, directory, Btrfs subvolume | Planned |

## FAQ

**Is pillar-csi a distributed filesystem like Ceph or Longhorn?**
No. pillar-csi does not replicate or pool storage across nodes; it exports a ZFS pool or LVM VG that already exists on one machine to the rest of the cluster over NVMe-oF/TCP.

**Is pillar-csi an alternative to democratic-csi for homelab Kubernetes storage?**
democratic-csi uses SSH and Python CLI tools (`targetcli`, `nvmetcli`) to drive storage nodes; pillar-csi uses a stateless gRPC agent that writes directly to configfs. One pillar-csi deployment handles any number of pools and protocols via CRDs, whereas democratic-csi requires a separate Helm release per backend type.

**Do I need NVMe-oF hardware (RDMA NIC)?**
No. NVMe-oF/TCP runs over standard ethernet using regular TCP/IP. RDMA (RoCE/InfiniBand) is a separate transport that pillar-csi does not implement.

**Can I run pillar-csi on a Raspberry Pi / single-node homelab?**
Yes, if the node’s kernel ships `nvmet` and `nvmet_tcp`. A Pi 5 on a recent mainline kernel works; older Pi models with vendor kernels may need a custom build or DKMS package for NVMe-oF/TCP target support.

**When will iSCSI ship?**
Phase 2 is currently in progress; follow the [milestone tracker](https://github.com/isac322/pillar-csi/milestones) for an updated estimate.

## Contributing

Project conventions are in [`CLAUDE.md`](CLAUDE.md):

- Use kubebuilder CLI for all new CRDs, controllers, and webhooks — never hand-edit `zz_generated.deepcopy.go` or files under `config/crd/`.
- No silent failures: every `configfs` / `sysfs` write must have read-back verification or an explicit `log.Error`.
- `make lint` must pass with **0 issues** before every commit.
- Commit messages: explain why, not what.

`make help` lists every Make target.

## License

Apache-2.0. See [LICENSE](LICENSE).
