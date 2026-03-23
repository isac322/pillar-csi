# PRD Review: Inconsistencies and Missing Parts

Cross-reference against:
- CSI spec v1.12.0 (latest, released 2025-10-17)
- NVM Express (NVMe) specification for NQN format
- iSCSI RFC 3720 / iscsiadm documentation
- Kubernetes CRD best practices
- Popular CSI drivers (aws-ebs-csi-driver, democratic-csi)
- kubebuilder v4.13.0

---

## CRITICAL ISSUES (contradictions / spec violations)

### C1. Agent gRPC address resolution — contradictory text

**Location:** Section 2.1 vs Section 2.5

**Contradiction:**
- Section 2.1 gRPC address resolution logic: resolves from `K8s Node.status.addresses`
  → `resolvedAddress: 192.168.219.6` (a Node IP)
- Section 2.5 Agent Discovery text: "K8s Node IP → **agent pod IP를 K8s API로 조회**"
  → implies using Pod IP

These are different things. If the agent DaemonSet runs without `hostNetwork: true`, the gRPC
server listens on the **pod IP**, not the node IP. Connecting to the node IP would fail.

**Resolution:** The PRD intends to use node IP, which requires either:
- `hostPort: 9500` on the agent container (preferred — no hostNetwork needed), OR
- `hostNetwork: true` on the agent DaemonSet

Section 2.5 text is wrong. The `resolvedAddress` (node IP) approach is correct **only if**
the agent uses `hostPort`. Fix section 2.5 to say "K8s Node.status.addresses에서 Node IP 조회"
and explicitly document that agent DaemonSet uses `hostPort: 9500`.

---

### C2. iSCSI `nodeSessionTimeout` — non-standard parameter

**Location:** Section 2.1 PillarProtocol iSCSI example (line ~229)

**Issue:** `nodeSessionTimeout: 120` is not a standard iscsiadm/LIO parameter name.

Standard iscsiadm timeout parameters are:
- `node.conn[0].timeo.login_timeout` → PRD's `loginTimeout` ✓
- `node.session.timeo.replacement_timeout` → PRD's `replacementTimeout` ✓
- `nodeSessionTimeout` → **does not map to any standard parameter**

**Fix:** Remove `nodeSessionTimeout` or replace with a correct parameter name
(e.g., `noopOutTimeout` → `node.conn[0].timeo.noop_out_timeout`).

---

### C3. Missing `NodeExpandVolume` RPC for Filesystem expansion

**Location:** Section 6 Phase 1 roadmap, Section 3 ExpandVolume

**Issue:** Phase 1 includes `volumeMode: Filesystem` and `ExpandVolume`. For block-backed
filesystem volumes (NVMe-oF/iSCSI + ext4/xfs), volume expansion requires TWO RPCs:
1. `ControllerExpandVolume` — resize the ZFS zvol on the storage node ✓ (mentioned)
2. `NodeExpandVolume` — resize the filesystem on the worker node ✗ (not mentioned)

`NodeExpandVolume` is **required** for online filesystem expansion per the CSI spec
(Example 2 in the spec). Omitting it means `allowVolumeExpansion: true` will only
resize the block device, leaving the filesystem at the old size.

**Fix:** Add `NodeExpandVolume` to the Phase 1 scope and the CSI Node RPC list.

---

## INCONSISTENCIES (spec version mismatches, format errors)

### I1. CSI spec version outdated

**Location:** Section 9 Tech Stack

**Issue:** `CSI spec | v1.11.0` — Latest release is **v1.12.0** (2025-10-17).

v1.12.0 additions relevant to pillar-csi:
- `GetSnapshot` RPC (Alpha) — relevant for Phase 4 snapshot feature
- `ControllerModifyVolume` moved to GA

**Fix:** Update to `v1.12.0`.

---

### I2. Go version outdated

**Location:** Section 9 Tech Stack

**Issue:** `Go 1.23+` — Latest stable Go is **go1.26.1** (as of March 2026).

**Fix:** Update to `Go 1.26+`.

---

### I3. Storage capacity units inconsistent with Kubernetes conventions

**Location:** Section 2.1 PillarTarget status, PillarPool status

**Issue:** Status fields use SI decimal units (`712G`, `412G`, `32.7T`, `300G`) which are
inconsistent with Kubernetes resource.Quantity conventions. Kubernetes uses binary SI
suffixes (`Gi`, `Ti`) for storage. Key problem: `32.7T` uses a decimal (non-integer)
value which is not representable as a Kubernetes `resource.Quantity`.

| PRD Example | K8s Idiomatic | Notes |
|---|---|---|
| `total: 712G` | `total: 682Gi` | 712GB ≠ 712GiB |
| `available: 32.7T` | not valid | Decimals not supported in resource.Quantity |

**Fix:** Use `resource.Quantity` type (integer + binary suffix) for capacity fields, e.g.,
`712Gi` and `35936Gi`. Or store as int64 bytes in the Go struct and format for display.
ZFS quota/reservation fields (`500G`, `100G`) are ZFS-native values (not K8s quantities)
and can stay as strings.

---

### I4. Volume ID format — `pool` segment ambiguity

**Location:** Section 5, Volume ID 형식

**Issue:** Volume ID is `<target>/<pool>/<volume-name>` where `pool` is described as
"ZFS pool 이름" (e.g., `hot-data`), NOT the PillarPool CR name (`rock5bp-hot-data`).

If two PillarPools reference the **same ZFS pool** on the same PillarTarget with
different `parentDataset` values, both produce Volume IDs with the same prefix
(`rock5bp/hot-data/...`), making routing from Volume ID alone ambiguous.

Example collision:
```
PillarPool A: targetRef=rock5bp, zfs.pool=hot-data, parentDataset=k8s
PillarPool B: targetRef=rock5bp, zfs.pool=hot-data, parentDataset=k8s-staging
→ Both produce: rock5bp/hot-data/<volume-name>
```

**Fix:** Either use PillarPool CR name in Volume ID (e.g., `rock5bp/rock5bp-hot-data/pvc-abc123`),
or explicitly document that multiple PillarPools must not share the same ZFS pool on the
same PillarTarget.

---

### I5. NVMe subsystem NQN vs host NQN distinction missing

**Location:** Section 5, Volume ID 형식; Section 5.2 ControllerPublishVolume

**Issue:** The PRD shows a single NQN format example
(`nqn.2024-01.com.bhyoo.pillar-csi:rock5bp:pvc-abc123`) without distinguishing between:
- **Subsystem NQN** (target NQN): identifies the NVMe-oF export — generated by pillar-csi ✓
- **Host NQN** (initiator NQN): identifies the worker node — read from worker node by pillar-node

Section 5.2 says "대상 노드의 initiator ID 조회 (NodeGetInfo에서 등록된 NQN/IQN)" but
CSI's `NodeGetInfo` RPC only returns `node_id` (an opaque string), not the NQN directly.

The NQN/IQN initiator ID must be:
1. Read by pillar-node from the host (`/etc/nvme/hostnqn` or `nvme gen-hostnqn`)
2. Stored in a K8s Node annotation (e.g., `pillar-csi.bhyoo.com/nvme-hostnqn`)
3. Retrieved by pillar-controller via K8s API during ControllerPublishVolume

**Fix:** Add section describing how initiator IDs (NQN/IQN) are collected and stored,
and clarify that `NodeGetInfo.node_id` is not the NQN.

---

## MISSING PARTS (omissions / incomplete specification)

### M1. Required Identity and capabilities RPCs not listed

**Location:** Section 6 Phase 1 scope, Section 2.4 Components

**Issue:** Phase 1 scope lists specific CSI RPCs but omits implicitly required ones.
Per the CSI spec, the following are REQUIRED for any CSI plugin:

| Service | RPC | Status in PRD |
|---|---|---|
| Identity | `GetPluginInfo` | Not mentioned |
| Identity | `GetPluginCapabilities` | Not mentioned |
| Identity | `Probe` | Not mentioned |
| Controller | `ValidateVolumeCapabilities` | Not mentioned |
| Controller | `ControllerGetCapabilities` | Not mentioned |
| Node | `NodeGetCapabilities` | Not mentioned |
| Node | `NodeGetInfo` | Mentioned only in passing |
| Node | `NodeExpandVolume` | See C3 above |

**Fix:** Add these to the Phase 1 scope or explicitly acknowledge them as "assumed/implicit".

---

### M2. `liveness-probe` sidecar not listed

**Location:** Section 2.4 Components (pillar-controller)

**Issue:** CSI sidecar containers listed: `provisioner, attacher, resizer, snapshotter`.
Missing: **`livenessprobe`** — the standard Kubernetes CSI liveness probe sidecar
(kubernetes-csi/livenessprobe v2.18.0). This is standard in all production CSI drivers
and required for proper health checking.

Also missing: **`node-driver-registrar`** is mentioned for pillar-node ✓ but not for
the controller's sidecar list (it's a node-only sidecar, which is correct).

**Fix:** Add `livenessprobe` sidecar to both pillar-controller and pillar-node sidecar lists.

---

### M3. `observedGeneration` missing from CRD status

**Location:** Section 7.3 CRD Status Conditions, all CRD examples

**Issue:** K8s API conventions require `observedGeneration` in status conditions
(per `metav1.Condition` standard type). The field indicates which `.metadata.generation`
the status reflects, enabling clients to detect stale status.

Standard `metav1.Condition` fields include:
- `observedGeneration int64` — which `.metadata.generation` this condition is for

The PRD's condition examples omit `observedGeneration`.

**Fix:** Add `observedGeneration` to all condition examples and mention it in Section 7.3.

---

### M4. Finalizer names not specified

**Location:** Section 7.2 의존성 삭제 보호

**Issue:** The PRD describes finalizer-based deletion protection but doesn't specify
the finalizer name strings. Kubernetes finalizer names should follow the convention
`{group}/{name}`, e.g.:
- `pillar-csi.bhyoo.com/target-protection`
- `pillar-csi.bhyoo.com/pool-protection`
- `pillar-csi.bhyoo.com/binding-protection`

**Fix:** Add finalizer name constants to the PRD.

---

### M5. `hostPort` for agent DaemonSet not specified

**Location:** Section 2.4 Components (pillar-agent), Section 2.5 Agent Discovery

**Issue:** The PRD says agent is accessible at node IP:port but doesn't mention how
this is achieved without `hostNetwork`. The agent DaemonSet must declare
`containerPort.hostPort: 9500` for the controller to reach it at the node IP.

**Fix:** Explicitly document `hostPort: 9500` in the agent DaemonSet spec.

---

### M6. `inCapsuleDataSize` is an initiator-side parameter, not target-side

**Location:** Section 2.1 PillarProtocol NVMe-oF TCP spec

**Issue:** `inCapsuleDataSize: 16384` is listed as an NVMe-oF target parameter.
In NVMe-oF over TCP, in-capsule data size is negotiated during connection and
is an initiator parameter (specified via `nvme connect --icdoff`), not something
the target configures in configfs.

The nvmet configfs does not have a per-subsystem in-capsule data size setting.

**Fix:** Move `inCapsuleDataSize` to the initiator parameters section or remove it.
If kept, clarify it's passed to `nvme connect` on the worker node, not to configfs.

---

### M7. PillarPool `discoveredPools` vs `parentDataset` — ZFS dataset hierarchy unclear

**Location:** Section 2.1 PillarTarget status `discoveredPools`

**Issue:** `discoveredPools` in PillarTarget status lists pools with `type: zfs` and
a pool name. This seems to list ZFS pools on the agent. However, `PillarPool.spec.backend.zfs`
also has `parentDataset` for organizing volumes within a pool.

The relationship between `discoveredPools` (agent-reported pools) and `PillarPool.spec.backend.zfs.pool`
(user-configured pool name) is not explicitly validated. If a user creates a PillarPool
referencing `pool: non-existent-pool`, the `PoolDiscovered` condition would be False —
this validation flow should be documented.

**Fix:** Clarify that `PillarPool.spec.backend.zfs.pool` must match a name in
`PillarTarget.status.discoveredPools` for `PoolDiscovered` condition to be True.

---

### M8. `volumeMode: Block` omitted from Phase 1 without protocol justification

**Location:** Section 6 Phase 1 "미포함"

**Issue:** Phase 1 excludes `volumeMode: Block`. However, NVMe-oF and iSCSI are
block protocols and `volumeMode: Block` is a natural fit. The PRD doesn't explain
why this simpler mode is deferred when `volumeMode: Filesystem` (which requires
mkfs + mount on top of block) is included.

`volumeMode: Block` is simpler to implement (no mkfs/mount, just present the device).
Including it in Phase 1 would make the driver more complete.

**Fix:** Add rationale for excluding `volumeMode: Block` from Phase 1, or include it.

---

### M9. NFS export path not specified in PillarProtocol

**Location:** Section 2.1 PillarProtocol NFS example

**Issue:** The NFS PillarProtocol spec only shows `version: "4.2"`. Missing:
- How the export path is determined (ZFS dataset mountpoint?)
- What NFS export options are configurable (`rw`, `no_root_squash`, `sync`, etc.)
- Whether NFSv3 is supported (has different behavior for access control)

**Fix:** Add NFS export options to the PillarProtocol NFS spec (Phase 3).

---

## MINOR ISSUES / SUGGESTIONS

### S1. API group name uses hyphen — unusual but valid

**Location:** Section 2.1, throughout

`pillar-csi.bhyoo.com` uses a hyphen in the subdomain part. This is valid DNS and valid
as a Kubernetes API group, but unusual. Common convention is to avoid hyphens.
Alternative: `storage.bhyoo.com` or `csi.bhyoo.com`.
Decision: keep as-is (core product concept), just noting it's atypical.

---

### S2. `zfs create -V 50G` in example uses decimal G, not binary Gi

**Location:** Section 5.1 CreateVolume step 4a

`zfs create -V 50G hot-data/k8s/pvc-xxx` — ZFS uses SI (decimal) G (10^9 bytes).
Kubernetes PVCs use binary Gi (2^30 bytes). When K8s requests `50Gi` (53,687,091,200 bytes),
passing `50G` to ZFS will create a 50,000,000,000 byte volume (5% smaller).

**Fix:** Controller should pass the exact byte count from CSI request:
`zfs create -V 53687091200 pool/dataset/name` (ZFS accepts exact byte sizes).

---

### S3. PillarProtocol `fsType` field placement

**Location:** Section 2.1 PillarProtocol NVMe-oF spec (line ~209)

`fsType: ext4` is defined at both `PillarProtocol.spec` and `PillarBinding.spec.overrides`.
The override hierarchy makes sense, but for file system protocols (NFS), `fsType` is
irrelevant. This should be documented as applying only to block protocols.

The PRD mentions this in section 2.3: "블록 프로토콜 + volumeMode: Filesystem일 때만" ✓.
No change needed but worth keeping consistent in schema definitions.

---

### S4. `StorageClass.reclaimPolicy` ownership

**Location:** Section 2.1 PillarBinding spec

`reclaimPolicy: Delete` in PillarBinding creates a StorageClass with that policy.
If a user changes `reclaimPolicy` in PillarBinding, existing PVs retain their original
reclaim policy (PV.spec.persistentVolumeReclaimPolicy is copied at PV creation time).
This behavior should be documented.

---

## Summary Table

| ID | Severity | Category | Short Description |
|---|---|---|---|
| C1 | Critical | Contradiction | Agent address: node IP vs pod IP text conflict |
| C2 | Critical | Spec Violation | `nodeSessionTimeout` is not a real iSCSI parameter |
| C3 | Critical | Missing RPC | `NodeExpandVolume` required for Filesystem expansion |
| I1 | Medium | Outdated | CSI spec v1.11.0 → update to v1.12.0 |
| I2 | Medium | Outdated | Go 1.23+ → update to Go 1.26+ |
| I3 | Medium | Format Error | Storage units: `32.7T` not a valid K8s resource.Quantity |
| I4 | Medium | Ambiguity | Volume ID `pool` segment ambiguous if shared ZFS pool |
| I5 | Medium | Missing Detail | Host NQN vs subsystem NQN distinction; NodeGetInfo doesn't return NQN |
| M1 | Medium | Missing | Required Identity/capabilities RPCs not in Phase 1 scope |
| M2 | Low | Missing | `livenessprobe` sidecar not listed |
| M3 | Low | Missing | `observedGeneration` missing from condition examples |
| M4 | Low | Missing | Finalizer name strings not specified |
| M5 | Medium | Missing | `hostPort: 9500` not documented for agent DaemonSet |
| M6 | Medium | Wrong Layer | `inCapsuleDataSize` is initiator-side, not target-side |
| M7 | Low | Clarification | `discoveredPools` validation flow for PillarPool not documented |
| M8 | Low | Scope | No rationale given for excluding `volumeMode: Block` from Phase 1 |
| M9 | Low | Missing | NFS export options not specified in PillarProtocol |
| S1 | Trivial | Style | API group uses hyphen (valid but atypical) |
| S2 | Low | Bug | `zfs create -V 50G` vs K8s `50Gi` (5% size mismatch) |
| S3 | Trivial | Clarity | `fsType` applicability scope in PillarProtocol |
| S4 | Trivial | Behavior | StorageClass reclaimPolicy change doesn't affect existing PVs |
