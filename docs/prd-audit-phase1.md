# Phase 1 PRD Audit — Implemented vs Missing

**Date:** 2026-03-24
**Source:** `docs/PRD.md` §6 (Phase 1 scope) cross-referenced against the codebase.

---

## Summary

| Area | Status |
|------|--------|
| CRDs (4×) | ✅ All defined |
| Controller reconcilers (4×) | ✅ All implemented |
| Validation + defaulting webhooks | ⚠️ Partial (validation only; no defaulting webhooks) |
| Agent gRPC server — all Phase 1 RPCs | ✅ Implemented |
| Backend: ZFS zvol | ✅ Implemented |
| Protocol: NVMe-oF TCP via configfs | ✅ Implemented |
| ACL: on/off (allow_any_host toggle) | ✅ Implemented |
| Volume expansion (ExpandVolume) | ✅ Implemented |
| Access modes: RWO, RWOP, ROX | ⚠️ Defined in proto/CRD, not yet enforced in CSI layer |
| CSI Controller service | ❌ Not yet implemented |
| CSI Node service | ❌ Not yet implemented |

---

## Implemented Features

### CRDs (`api/v1alpha1/`)

All four cluster-scoped CRDs are defined:

| CRD | File | Notes |
|-----|------|-------|
| `PillarTarget` | `api/v1alpha1/pillartarget_types.go` | `nodeRef` + `external` discriminated union, status conditions, resolvedAddress |
| `PillarPool` | `api/v1alpha1/pillarpool_types.go` | `targetRef`, `backend.zfs`, capacity status, conditions |
| `PillarProtocol` | `api/v1alpha1/pillarprotocol_types.go` | `nvmeofTcp` config, ACL field, `bindingCount`/`activeTargets` status |
| `PillarBinding` | `api/v1alpha1/pillarbinding_types.go` | `poolRef`/`protocolRef`, StorageClass spec, overrides, conditions |

### Controller Reconcilers (`internal/controller/`)

All four reconcilers are implemented:

| Reconciler | File | Key capabilities |
|-----------|------|-----------------|
| `PillarTargetReconciler` | `pillartarget_controller.go` | Node IP resolution, `NodeExists`/`AgentConnected`/`Ready` conditions, storage-node label management, finalizer + deletion blocking on PillarPools, Node + PillarPool watches |
| `PillarPoolReconciler` | `pillarpool_controller.go` | `TargetReady`/`PoolDiscovered`/`BackendSupported`/`Ready` conditions, capacity status, finalizer + deletion blocking on PillarBindings |
| `PillarProtocolReconciler` | `pillarprotocol_controller.go` | `bindingCount`/`activeTargets` status, finalizer, PillarBinding watch |
| `PillarBindingReconciler` | `pillarbinding_controller.go` | StorageClass auto-creation + ownerReference, `PoolReady`/`ProtocolValid`/`Compatible`/`StorageClassCreated`/`Ready` conditions, PVC-blocking deletion, PillarPool + PillarProtocol watches |

Additional features in controllers:
- Finalizer-based dependency protection on all CRDs
- Storage-node label auto-applied to K8s Node on PillarTarget creation (`pillar-csi.bhyoo.com/storage-node=true`)
- Storage-node label removed on PillarTarget deletion

### Validation Webhooks (`internal/webhook/v1alpha1/`)

All four CRD validation webhooks are registered:

| Webhook | File | Validates |
|---------|------|----------|
| `PillarTargetCustomValidator` | `pillartarget_webhook.go` | Immutability of `spec.nodeRef.name`, `spec.external.address/port`; discriminant switch forbidden |
| `PillarPoolCustomValidator` | `pillarpool_webhook.go` | Immutability of `spec.targetRef`, `spec.backend.type`, `spec.backend.zfs.pool` |
| `PillarProtocolCustomValidator` | `pillarprotocol_webhook.go` | Immutability of `spec.type` |
| `PillarBindingCustomValidator` | `pillarbinding_webhook.go` | Immutability of `spec.poolRef`, `spec.protocolRef`; backend↔protocol compatibility check |

### Agent gRPC Server (`internal/agent/`, `proto/pillar_csi/agent/v1/agent.proto`)

The gRPC server is fully implemented for all Phase 1 RPCs:

| RPC | Handler file | Status |
|-----|-------------|--------|
| `GetCapabilities` | `server_discovery.go` | ✅ Returns `BACKEND_TYPE_ZFS_ZVOL` + `PROTOCOL_TYPE_NVMEOF_TCP`, pool list |
| `GetCapacity` | `server_discovery.go` | ✅ Per-pool total/available/used bytes |
| `ListVolumes` | `server_discovery.go` | ✅ Per-pool volume listing |
| `ListExports` | `server_discovery.go` | ✅ Returns empty map (Phase 1; full configfs scan deferred) |
| `HealthCheck` | `server_discovery.go` | ✅ Structured per-subsystem health: ZFS module, nvmet configfs, per-pool |
| `CreateVolume` | `server_volume.go` | ✅ ZFS zvol creation via `zfs create -V`, idempotent |
| `DeleteVolume` | `server_volume.go` | ✅ ZFS zvol destruction, idempotent |
| `ExpandVolume` | `server_volume.go` | ✅ ZFS zvol expansion |
| `ExportVolume` | `server_export.go` | ✅ nvmet configfs Apply(), waits for zvol device node |
| `UnexportVolume` | `server_export.go` | ✅ nvmet configfs Remove(), idempotent |
| `AllowInitiator` | `server_export.go` | ✅ `allowed_hosts` symlink in configfs |
| `DenyInitiator` | `server_export.go` | ✅ Removes `allowed_hosts` symlink |
| `ReconcileState` | `server_reconcile.go` | ✅ Re-applies full desired state after restart/reboot |

### Backend: ZFS zvol (`internal/agent/backend/zfs/`)

- `zfs.go`: `ZfsBackend` implements `backend.VolumeBackend`
- Operations: `Create`, `Delete`, `Expand`, `Capacity`, `ListVolumes`, `DevicePath`
- Uses pluggable `executor` interface (real: `os/exec`; tests: mock)
- ZFS dataset naming: `<pool>/<parentDataset>/<name>` or `<pool>/<name>`
- Block device path: `/dev/zvol/<pool>/...`
- Supports ZFS properties: `compression`, `volblocksize`, `quota`, `reservation`, etc.

### Protocol: NVMe-oF TCP via configfs (`internal/agent/nvmeof/`)

- `configfs.go` + `exports.go`: `NvmetTarget` manages nvmet configfs entries
- Operations: `Apply` (create subsystem/namespace/port/allowed_hosts), `Remove`, `AllowHost`, `DenyHost`
- Uses `t.TempDir()`-compatible configfs root (no real kernel configfs required for tests)
- `device_poll.go`: `WaitForDevice` polls for zvol block device before configfs writes

### Agent Binary (`cmd/agent/main.go`)

- Parses CLI flags: `--pools` (name:type:parentDataset format), `--configfs-root`, `--addr`
- Constructs `ZfsBackend` per pool
- Starts `grpc.Server` with `AgentService` registered
- Graceful shutdown on SIGTERM/SIGINT

---

## Missing / Partially Implemented Features

### 1. CSI Controller Service ❌

**PRD scope (Phase 1 §6):**
> pillar-controller: CSI Controller (CreateVolume, DeleteVolume, ExpandVolume,
> ControllerPublishVolume/UnpublishVolume, ValidateVolumeCapabilities, GetCapacity)

**Status:** No CSI Controller service implementation exists anywhere in the codebase.

- No `internal/csi/` or `internal/controller/csi*.go` files
- `cmd/main.go` does not register any CSI gRPC server
- The controller binary (`cmd/main.go`) currently only runs the Kubernetes reconcilers

**Missing files (to be created in a later task):**
- `internal/csi/controller.go` — `csi.ControllerServer` implementation
- `cmd/main.go` needs CSI socket + gRPC server setup

**Affected RPCs:**
- `CreateVolume` (calls agent `CreateVolume` + `ExportVolume`)
- `DeleteVolume` (calls agent `UnexportVolume` + `DeleteVolume`)
- `ExpandVolume` (calls agent `ExpandVolume`)
- `ControllerPublishVolume` (calls agent `AllowInitiator` when ACL=true)
- `ControllerUnpublishVolume` (calls agent `DenyInitiator` when ACL=true)
- `ValidateVolumeCapabilities`
- `GetCapacity`

### 2. CSI Node Service ❌

**PRD scope (Phase 1 §6):**
> CSI Node (Stage/Unstage/Publish/Unpublish, NodeGetVolumeStats, NodeExpandVolume)

**Status:** No CSI Node service implementation exists.

- No `internal/csi/node.go` or similar
- `cmd/` contains only `cmd/main.go` (controller) and `cmd/agent/main.go` (agent); no `cmd/node/`

**Missing files (to be created in a later task):**
- `internal/csi/node.go` — `csi.NodeServer` implementation
- `cmd/node/main.go` — pillar-node binary entry point

**Affected operations (NVMe-oF TCP + volumeMode=Filesystem):**
- `NodeStageVolume`: `nvme connect -t tcp …`, optional mkfs + mount
- `NodeUnstageVolume`: umount + `nvme disconnect`
- `NodePublishVolume`: bind mount staging → pod target path
- `NodeUnpublishVolume`: umount pod target path
- `NodeGetVolumeStats`
- `NodeExpandVolume`

### 3. Defaulting Webhooks ⚠️

**PRD scope (Phase 1 §6):**
> CRD controller + validation webhook (immutable 필드 검증)

**Status:** Validation webhooks exist for all 4 CRDs. Defaulting (mutating) webhooks are **not yet registered**.

The PRD mentions `+kubebuilder:default` markers on CRD fields (e.g., `addressType` defaults to `InternalIP`, `fsType` defaults to `ext4`) and the webhook scaffold in kubebuilder supports `WithDefaulter`. However, no `CustomDefaulter` is implemented.

**Impact:** Low for Phase 1 — kubebuilder `+kubebuilder:default` CEL markers provide field-level defaults without a webhook. The main missing defaulting would be cross-field business logic.

**Files:** `internal/webhook/v1alpha1/` — no `*_defaulter.go` files present.

### 4. AgentConnected Condition — Stubbed ⚠️

**PRD scope:**
> PillarTarget `AgentConnected` condition: reflects real gRPC connection state

**Status:** In `pillartarget_controller.go`, the `AgentConnected` condition is **stubbed** to always `False` with reason `AgentConnectionNotImplemented`:

```go
// AgentConnected: stubbed False until Task 3 implements actual gRPC dial.
meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
    Type:   "AgentConnected",
    Status: metav1.ConditionFalse,
    Reason: "AgentConnectionNotImplemented",
    ...
})
```

The controller currently resolves the agent IP/port but does not attempt an actual gRPC connection. As a result, `Ready` is always `False` even when the agent is running.

**File:** `internal/controller/pillartarget_controller.go` lines 141-148, 354-361.

### 5. PillarTarget `agentVersion` and `capabilities` in Status ⚠️

**PRD scope:**
> `status.agentVersion`, `status.capabilities` (backends/protocols), `status.discoveredPools`

**Status:** These fields are defined in `pillartarget_types.go` but are never populated in the controller because the actual gRPC dial (`GetCapabilities`) has not been implemented yet (blocked by item 4 above).

**File:** `internal/controller/pillartarget_controller.go` — no `GetCapabilities` call.

### 6. PillarPool `PoolDiscovered` and `BackendSupported` Conditions ⚠️

**PRD scope:**
> `PoolDiscovered`: pool found on agent; `BackendSupported`: backend type in agent capabilities

**Status:** These conditions appear to be set by the reconciler but rely on agent `GetCapabilities` / `ListVolumes` gRPC calls. Until `AgentConnected` is wired up (item 4), these conditions cannot be verified against the real agent.

**File:** `internal/controller/pillarpool_controller.go`

### 7. CSI Topology ❌ (explicitly excluded from Phase 1)

The PRD explicitly excludes `CSI Topology` from Phase 1. No implementation is required.

### 8. Parameter Override Hierarchy via PVC Annotations ❌ (CSI-layer feature)

**PRD scope:**
> 파라미터 오버라이드 계층 (Pool → Protocol → Binding → PVC annotation)

**Status:** The CRD spec types for overrides are defined (`PillarBinding.spec.overrides`). The merge logic that reads PVC annotations at `CreateVolume` time is part of the CSI Controller service (item 1 above) and does not yet exist.

### 9. `volumeMode: Block` ❌ (explicitly excluded from Phase 1)

The PRD explicitly excludes `volumeMode: Block` from Phase 1 scope.

### 10. Helm Chart ❌ (not yet present)

**PRD scope (Phase 1 §6):**
> Helm chart

**Status:** No `chart/` or `helm/` directory found. Helm packaging is a deployment artifact; it does not affect functionality.

---

## File Index

| Feature | Key Files |
|---------|-----------|
| CRD types | `api/v1alpha1/pillartarget_types.go`, `pillarpool_types.go`, `pillarprotocol_types.go`, `pillarbinding_types.go` |
| Controller: PillarTarget | `internal/controller/pillartarget_controller.go` |
| Controller: PillarPool | `internal/controller/pillarpool_controller.go` |
| Controller: PillarProtocol | `internal/controller/pillarprotocol_controller.go` |
| Controller: PillarBinding | `internal/controller/pillarbinding_controller.go` |
| Webhook: validation | `internal/webhook/v1alpha1/pillartarget_webhook.go`, `pillarpool_webhook.go`, `pillarprotocol_webhook.go`, `pillarbinding_webhook.go` |
| Agent server | `internal/agent/server.go`, `server_volume.go`, `server_export.go`, `server_discovery.go`, `server_reconcile.go` |
| ZFS backend | `internal/agent/backend/zfs/zfs.go` |
| NVMe-oF configfs | `internal/agent/nvmeof/configfs.go`, `exports.go`, `device_poll.go` |
| Health check | `internal/agent/health/health.go` |
| Agent binary | `cmd/agent/main.go` |
| Controller binary | `cmd/main.go` |
| Proto definition | `proto/pillar_csi/agent/v1/agent.proto` |
| Generated gRPC stubs | `gen/go/pillar_csi/agent/v1/` |
| **CSI Controller** | ❌ **not yet created** |
| **CSI Node** | ❌ **not yet created** |
