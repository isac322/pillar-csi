# Phase 1 PRD Audit — Implementation Status

**Date:** 2026-03-24 (final verification)
**Source:** `docs/PRD.md` §6 (Phase 1 scope) cross-referenced against the full codebase.
**Build/test:** `go build ./...` ✅ `go test ./...` ✅ (all 17 test packages pass)

---

## Summary

| Area | Status |
|------|--------|
| CRDs (4×) | ✅ All defined and cluster-scoped |
| Controller reconcilers (4×) | ✅ All implemented |
| Validation webhooks (4×) | ✅ All implemented |
| Defaulting webhook for PillarBinding | ✅ Implemented |
| Agent gRPC server — all Phase 1 RPCs | ✅ Implemented |
| Backend: ZFS zvol | ✅ Implemented |
| Protocol: NVMe-oF TCP via configfs | ✅ Implemented |
| ACL on (AllowInitiator/DenyInitiator) | ✅ Implemented |
| ACL off toggle (from PillarProtocol.acl) | ⚠️ AclEnabled hardcoded `true`; protocol flag not read |
| Volume expansion (ControllerExpandVolume) | ✅ Implemented |
| Access modes: RWO, RWOP, ROX | ✅ Defined and validated in CSI controller |
| Finalizer deletion protection | ✅ All 4 CRDs |
| StorageClass auto-creation from PillarBinding | ✅ Implemented |
| Agent stateless recovery (ReconcileState) | ✅ Implemented |
| AgentConnected live gRPC HealthCheck | ✅ Implemented (was stub in previous audit) |
| CSI Controller service | ✅ Implemented (new since previous audit) |
| CSI Node service | ✅ Implemented (new since previous audit) |
| NodeExpandVolume | ⚠️ Capability advertised; function not implemented |
| NodeGetVolumeStats | ⚠️ Not implemented |
| CSI GetCapacity (controller RPC) | ⚠️ Not implemented (Unimplemented fallthrough) |
| AgentVersion / DiscoveredPools / Capabilities status | ⚠️ Fields defined, never populated |
| PVC annotation parameter overrides | ⚠️ CRD types defined; merge logic not in CSI layer |
| CSI gRPC server wired into binaries | ⚠️ internal/csi exists; not wired in cmd/ |
| Helm chart | ❌ Not yet present |

---

## Implemented Features

### CRDs (`api/v1alpha1/`)

All four cluster-scoped CRDs are defined:

| CRD | File | Notes |
|-----|------|-------|
| `PillarTarget` | `pillartarget_types.go` | `nodeRef` + `external` discriminated union, status conditions, resolvedAddress, AgentVersion/Capabilities/DiscoveredPools fields |
| `PillarPool` | `pillarpool_types.go` | `targetRef`, `backend.zfs`, capacity status, conditions |
| `PillarProtocol` | `pillarprotocol_types.go` | `nvmeofTcp` config with `acl` field, `bindingCount`/`activeTargets` status |
| `PillarBinding` | `pillarbinding_types.go` | `poolRef`/`protocolRef`, StorageClass spec, overrides, conditions |
| `PillarVolume` | `pillarvolume_types.go` | **New** — durable partial-failure tracking for CSI lifecycle |

### Controller Reconcilers (`internal/controller/`)

All four reconcilers are fully implemented:

| Reconciler | File | Key capabilities |
|-----------|------|-----------------|
| `PillarTargetReconciler` | `pillartarget_controller.go` | Node IP resolution, live `HealthCheck` via `agentclient.Dialer`, `NodeExists`/`AgentConnected`/`Ready` conditions, mTLS vs plaintext reason codes, storage-node label management, finalizer + deletion blocking, Node + PillarPool watches, 30s requeue |
| `PillarPoolReconciler` | `pillarpool_controller.go` | `TargetReady`/`PoolDiscovered`/`BackendSupported`/`Ready` conditions evaluated from `target.Status.DiscoveredPools`/`Capabilities`, capacity status, finalizer + deletion blocking on PillarBindings |
| `PillarProtocolReconciler` | `pillarprotocol_controller.go` | `bindingCount`/`activeTargets` status, finalizer, PillarBinding watch |
| `PillarBindingReconciler` | `pillarbinding_controller.go` | StorageClass auto-creation + ownerReference, `PoolReady`/`ProtocolValid`/`Compatible`/`StorageClassCreated`/`Ready` conditions, PVC-blocking deletion, PillarPool + PillarProtocol watches |

### Validation Webhooks (`internal/webhook/v1alpha1/`)

All four CRD validation webhooks:

| Webhook | File | Validates |
|---------|------|----------|
| `PillarTargetCustomValidator` | `pillartarget_webhook.go` | Immutability of `spec.nodeRef.name`, `spec.external.address/port`; discriminant switch forbidden |
| `PillarPoolCustomValidator` | `pillarpool_webhook.go` | Immutability of `spec.targetRef`, `spec.backend.type`, `spec.backend.zfs.pool` |
| `PillarProtocolCustomValidator` | `pillarprotocol_webhook.go` | Immutability of `spec.type` |
| `PillarBindingCustomValidator` | `pillarbinding_webhook.go` | Immutability of `spec.poolRef`, `spec.protocolRef`; backend↔protocol compatibility check |

### Defaulting Webhook (`internal/webhook/v1alpha1/`)

`PillarBindingCustomDefaulter` — registered via `WithDefaulter` in `pillarbinding_webhook.go`:
- `Default()` auto-sets `spec.storageClass.allowVolumeExpansion` based on backend type when not explicitly set by the user.
- File: `pillarbinding_webhook.go` lines 50–118.
- Tests: `pillarbinding_webhook_test.go`.

### Agent gRPC Server (`internal/agent/`)

All Phase 1 RPCs implemented:

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
- Tests: `zfs_test.go`

### Protocol: NVMe-oF TCP via configfs (`internal/agent/nvmeof/`)

- `configfs.go` + `exports.go`: `NvmetTarget` manages nvmet configfs entries
- Operations: `Apply` (create subsystem/namespace/port/allowed_hosts), `Remove`, `AllowHost`, `DenyHost`
- `device_poll.go`: `WaitForDevice` polls for zvol block device before configfs writes
- Tests: `configfs_test.go`, `device_poll_test.go`

### CSI Controller Service (`internal/csi/controller.go`) — **New**

`ControllerServer` implements `csi.ControllerServer`:

| Method | Status | Notes |
|--------|--------|-------|
| `ControllerGetCapabilities` | ✅ | CREATE_DELETE_VOLUME, PUBLISH_UNPUBLISH, EXPAND_VOLUME, SINGLE_NODE_MULTI_WRITER |
| `ValidateVolumeCapabilities` | ✅ | Validates RWO/RWOP/ROX; rejects RWX |
| `CreateVolume` | ✅ | Calls agent CreateVolume + ExportVolume; idempotent; durable state via PillarVolume CRD |
| `DeleteVolume` | ✅ | Calls agent UnexportVolume + DeleteVolume; idempotent |
| `ControllerPublishVolume` | ✅ | Calls agent AllowInitiator; records ACL state in VolumeStateMachine |
| `ControllerUnpublishVolume` | ✅ | Calls agent DenyInitiator; idempotent on NotFound |
| `ControllerExpandVolume` | ✅ | Calls agent ExpandVolume; sets `node_expansion_required=true` |
| `GetCapacity` | ⚠️ | Falls through to `UnimplementedControllerServer` (not implemented) |
| `ListVolumes` | ⚠️ | Falls through to `UnimplementedControllerServer` (not implemented) |

Tests: `controller_test.go` — CreateVolume idempotency, partial retry, validation, agent-unavailable paths.

### CSI Node Service (`internal/csi/node.go`) — **New**

`NodeServer` implements `csi.NodeServer`:

| Method | Status | Notes |
|--------|--------|-------|
| `NodeGetCapabilities` | ✅ | STAGE_UNSTAGE_VOLUME, EXPAND_VOLUME advertised |
| `NodeGetInfo` | ✅ | Returns node ID (NQN) for ACL; topology not used |
| `NodeStageVolume` | ✅ | nvme connect; optional mkfs (ext4/xfs) + mount for Filesystem mode |
| `NodeUnstageVolume` | ✅ | umount + nvme disconnect; persisted state cleanup |
| `NodePublishVolume` | ✅ | Bind mount staging → pod target path; Block mode also handled |
| `NodeUnpublishVolume` | ✅ | Unmount pod target path; idempotent |
| `NodeExpandVolume` | ⚠️ | EXPAND_VOLUME capability advertised but function not implemented (UnimplementedNodeServer fallthrough) |
| `NodeGetVolumeStats` | ⚠️ | Not implemented (UnimplementedNodeServer fallthrough) |

Tests: `node_stage_test.go`, `node_publish_test.go` — full stage/unstage/publish/unpublish lifecycle coverage.

### VolumeStateMachine (`internal/csi/statemachine.go`) — **New**

Tracks in-memory + durable (via PillarVolume CRD) lifecycle state for each volume.
States: `StateCreated` → `StateControllerPublished` → `StateNodeStaged` → `StateNodePublished`.
Handles partial failures and idempotent retries.
Tests: `statemachine_test.go` — 20+ test cases covering happy path, partial failure, illegal transitions, concurrency.

### AgentClient (`internal/agentclient/`)

- `Manager` struct: caches one `*grpc.ClientConn` per resolved address
- Supports plaintext (default, Phase 1) and mTLS transport credentials
- `NewManagerFromFiles(certFile, keyFile, caFile, serverName)` — loads certs from disk
- `HealthCheck(ctx, address)` — used by PillarTargetReconciler
- `Dial(ctx, address)` — used by CSI ControllerServer to reach agent RPCs
- Tests: `dialer_test.go`, `mtls_test.go`, `mtls_files_test.go`

### Agent Binary (`cmd/agent/main.go`)

- Parses CLI flags: `--pools` (name:type:parentDataset format), `--configfs-root`, `--addr`, TLS flags
- Constructs `ZfsBackend` per pool
- Starts `grpc.Server` with `AgentService` registered
- Graceful shutdown on SIGTERM/SIGINT

---

## Gaps / Partially Implemented Features

### 1. CSI gRPC Server Not Wired into Binary Entry Points ⚠️

**PRD scope:** `pillar-controller` binary runs the CSI Controller service on a Unix socket; `pillar-node` binary runs the CSI Node service.

**Status:** `internal/csi/` package is fully implemented and tested, but:
- `cmd/controller/main.go` does NOT import `internal/csi` or start a CSI gRPC socket.
- There is NO `cmd/node/` directory — no pillar-node binary entry point.

**Impact:** The CSI controller and node implementations cannot be deployed or used until wired up.

**Files needed:**
- `cmd/controller/main.go`: import `internal/csi`, start CSI gRPC socket (e.g., `unix:///var/lib/kubelet/plugins/pillar-csi.bhyoo.com/csi.sock`), register `IdentityServer` + `ControllerServer`
- `cmd/node/main.go`: pillar-node entry point registering `IdentityServer` + `NodeServer`

### 2. NodeExpandVolume Not Implemented ⚠️

**PRD scope:** "CSI Node (Stage/Unstage/Publish/Unpublish, **NodeGetVolumeStats, NodeExpandVolume**)"

**Status:** `NodeGetCapabilities` advertises `EXPAND_VOLUME`, but no `NodeExpandVolume` function is defined in `node.go`. Calls fall through to `csi.UnimplementedNodeServer` which returns `codes.Unimplemented`.

**File:** `internal/csi/node.go` — function missing.

### 3. NodeGetVolumeStats Not Implemented ⚠️

**PRD scope:** "CSI Node (Stage/Unstage/Publish/Unpublish, **NodeGetVolumeStats**, NodeExpandVolume)"

**Status:** Not implemented; falls through to `csi.UnimplementedNodeServer`.

### 4. CSI GetCapacity Not Implemented ⚠️

**PRD scope:** "CSI Controller (CreateVolume, DeleteVolume, ExpandVolume, ControllerPublishVolume/UnpublishVolume, ValidateVolumeCapabilities, **GetCapacity**)"

**Status:** `GetCapacity` is NOT in the `ControllerGetCapabilities` advertised list and no implementation exists. Falls through to `UnimplementedControllerServer`.

### 5. ACL Off (acl=false) Not Respected ⚠️

**PRD scope:** "`acl: true` → host NQN ACL; `acl: false` → `allow_any_host=1`". "If `acl=false`, ControllerPublish/Unpublish is no-op."

**Status:** In `CreateVolume` (controller.go line 610), `AclEnabled: true` is hardcoded in the `ExportVolumeRequest`. The `PillarProtocol.spec.nvmeofTcp.acl` flag is not read. All volumes use ACL regardless of protocol configuration.

**File:** `internal/csi/controller.go` line 610.

### 6. AgentVersion / DiscoveredPools / Capabilities Not Populated ⚠️

**PRD scope:** `status.agentVersion`, `status.capabilities`, `status.discoveredPools` in PillarTarget.

**Status:** These fields are defined in `pillartarget_types.go` but the controller never calls `GetCapabilities` RPC to populate them. Only `HealthCheck` is called. As a result, `PillarPool` conditions `PoolDiscovered` and `BackendSupported` will be `Unknown` until an agent responds and those fields are populated.

**File:** `internal/controller/pillartarget_controller.go` — no `GetCapabilities` call.

### 7. PVC Annotation Parameter Overrides Not Implemented ⚠️

**PRD scope:** "파라미터 오버라이드 계층 (Pool → Protocol → Binding → **PVC annotation**)"

**Status:** The CRD spec types for overrides are defined (`PillarBinding.spec.overrides`). The `CreateVolume` implementation reads StorageClass parameters but does NOT parse or merge PVC annotations (`pillar-csi.bhyoo.com/backend-override`, `protocol-override`, `fs-override`). This is a CSI-layer feature dependent on item 1 (wiring) and additional CSI controller logic.

### 8. Helm Chart Missing ❌

**PRD scope:** "Helm chart"

**Status:** No `chart/` or `helm/` directory exists. This is a deployment artifact and does not affect code functionality.

---

## Test Coverage Notes

| Package | Test files | Coverage areas |
|---------|-----------|----------------|
| `internal/agent` | `server_*_test.go` | All agent RPCs including ReconcileState |
| `internal/agent/backend/zfs` | `zfs_test.go` | ZFS zvol CRUD, capacity, device path |
| `internal/agent/nvmeof` | `configfs_test.go`, `device_poll_test.go` | configfs Apply/Remove/ACL, device polling |
| `internal/agent/health` | (health package tests) | Health check subsystem reporting |
| `internal/agentclient` | `dialer_test.go`, `mtls_test.go` | gRPC dial, mTLS credentials, HealthCheck |
| `internal/csi` | `controller_test.go`, `node_stage_test.go`, `node_publish_test.go`, `statemachine_test.go` | Full CSI lifecycle, partial failures, idempotency |
| `test/component` | Component integration tests | Controller + webhook suite |
| `test/e2e` | E2E smoke tests | End-to-end flow |
| `internal/controller` | (no standalone test files) | Controllers tested via `test/component` |
| `internal/webhook/v1alpha1` | (no standalone test files) | Webhooks tested via `test/component` |

---

## File Index

| Feature | Key Files |
|---------|-----------|
| CRD types | `api/v1alpha1/pillartarget_types.go`, `pillarpool_types.go`, `pillarprotocol_types.go`, `pillarbinding_types.go`, `pillarvolume_types.go` |
| Controller: PillarTarget | `internal/controller/pillartarget_controller.go` |
| Controller: PillarPool | `internal/controller/pillarpool_controller.go` |
| Controller: PillarProtocol | `internal/controller/pillarprotocol_controller.go` |
| Controller: PillarBinding | `internal/controller/pillarbinding_controller.go` |
| Webhook: validation (4×) | `internal/webhook/v1alpha1/pillar*_webhook.go` |
| Webhook: defaulting (PillarBinding) | `internal/webhook/v1alpha1/pillarbinding_webhook.go` (`PillarBindingCustomDefaulter`) |
| CSI Controller | `internal/csi/controller.go` |
| CSI Node | `internal/csi/node.go` |
| CSI Identity | `internal/csi/identity.go` |
| VolumeStateMachine | `internal/csi/statemachine.go` |
| Agent server | `internal/agent/server.go`, `server_volume.go`, `server_export.go`, `server_discovery.go`, `server_reconcile.go` |
| ZFS backend | `internal/agent/backend/zfs/zfs.go` |
| NVMe-oF configfs | `internal/agent/nvmeof/configfs.go`, `exports.go`, `device_poll.go` |
| Health check | `internal/agent/health/health.go` |
| AgentClient | `internal/agentclient/dialer.go` |
| Agent binary | `cmd/agent/main.go` |
| Controller binary | `cmd/controller/main.go` (no CSI socket yet) |
| Proto definition | `proto/pillar_csi/agent/v1/agent.proto` |
| Generated gRPC stubs | `gen/go/pillar_csi/agent/v1/` |
| **CSI socket in cmd/controller/main.go** | ⚠️ **not yet wired** |
| **cmd/node/ binary** | ⚠️ **not yet created** |
| **Helm chart** | ❌ **not yet present** |
