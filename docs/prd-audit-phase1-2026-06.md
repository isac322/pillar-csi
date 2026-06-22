# Phase 1 PRD Audit — 2026-06

**Date:** 2026-06-21
**Supersedes:** [docs/prd-audit-phase1.md](./prd-audit-phase1.md) (2026-03-24)
**Source of truth:** `docs/PRD.md` §6 Phase 1 + `docs/PRD-iscsi.md` MVP
**Build:** `go build ./...` ✅ (clean)
**Tests:** `go test ./... -count=1` → 13 unit/component packages PASS · 446 cases pass · 36 e2e cases require a Kind cluster with real ZFS/LVM kernel modules (`PILLAR_E2E_BACKEND_CONTAINER`) and are skipped from `go test ./...` by design — they pass under `make test-e2e`.

---

## TL;DR

Phase 1 (ZFS zvol + NVMe-oF/TCP MVP) is **deployable**.

All blocking gaps from the 2026-03 audit are **resolved**. Two hardening
batches have landed since: (a) full CSI service wiring + lifecycle features
landed between March and June (see git log), (b) deployment-readiness
improvements landed in the 2026-06-21 batch (this audit).

The Phase 2 (iSCSI) and Phase 3+ (NFS / Snapshot / external nodes / extra
backends) features remain out of scope for current deployments.

---

## 2026-03 gaps — all CLOSED

| 2026-03 gap | Status now | Evidence |
|---|---|---|
| CSI gRPC server not wired into binaries | ✅ Resolved | [`cmd/controller/main.go`](../cmd/controller/main.go) registers Identity + Controller; [`cmd/node/main.go`](../cmd/node/main.go) registers Identity + Node |
| `NodeExpandVolume` not implemented | ✅ Resolved | [`internal/csi/node_expand.go`](../internal/csi/node_expand.go) — NVMe controller rescan + `resize2fs` / `xfs_growfs` |
| `NodeGetVolumeStats` not implemented | ✅ Resolved | [`internal/csi/node_stats.go`](../internal/csi/node_stats.go) — `BLKGETSIZE64` for block, `statfs` for filesystem |
| CSI `GetCapacity` not implemented | ✅ Resolved | [`internal/csi/controller.go`](../internal/csi/controller.go) `GetCapacity` calls agent `GetCapacity` |
| `AclEnabled: true` hardcoded | ✅ Resolved | `parseACLEnabled(params[paramACLEnabled])` reads `PillarProtocol.spec.nvmeofTcp.acl` |
| `AgentVersion` / `Capabilities` / `DiscoveredPools` not populated | ✅ Resolved | [`internal/controller/pillaragent_controller.go`](../internal/controller/pillaragent_controller.go) calls `GetCapabilities` RPC and writes status fields |
| PVC annotation overrides not implemented | ✅ Resolved | [`internal/csi/pvc_annotations.go`](../internal/csi/pvc_annotations.go) + 4-layer merge in `controller.go` |
| Helm chart missing | ✅ Resolved | [`charts/pillar-csi/`](../charts/pillar-csi/) with all templates + `values.yaml` + `test_render.sh` |

---

## Phase 1 PRD requirement matrix (current)

| PRD §6 requirement | Status | Key file |
|---|---|---|
| 4 CRDs cluster-scoped + `PillarVolumeState` durable state | ✅ | `api/v1alpha1/` |
| 4 reconcilers + finalizer-based deletion protection | ✅ | `internal/controller/` |
| Validation webhooks (immutability) | ✅ | `internal/webhook/v1alpha1/` (incl. `backend.zfs.pool` immutability landed 2026-06) |
| `PillarStorageClass` defaulter for `allowVolumeExpansion` | ✅ | `pillarstorageclass_webhook.go` |
| Agent gRPC: all Phase 1 RPCs (`GetCapabilities`, `GetCapacity`, `ListVolumes`, `ListExports`, `HealthCheck`, `CreateVolume`, `DeleteVolume`, `ExpandVolume`, `ExportVolume`, `UnexportVolume`, `AllowInitiator`, `DenyInitiator`, `ReconcileState`, `Drain`) | ✅ | `internal/agent/server_*.go` (`Drain` landed 2026-06) |
| ZFS zvol backend | ✅ | `internal/agent/backend/zfs/zfs.go` |
| NVMe-oF/TCP via direct configfs (no `nvmetcli`) + read-back verification | ✅ | `internal/agent/nvmeof/configfs.go` |
| IPv4 + IPv6 BindAddress | ✅ | `createPort` (IPv6 landed 2026-06) |
| ACL on/off respecting `PillarProtocol.spec.nvmeofTcp.acl` | ✅ | `internal/csi/controller.go` |
| `ReconcileState` agent restart recovery | ✅ | `internal/agent/server_reconcile.go` |
| CSI Controller (`CreateVolume` / `DeleteVolume` / `Expand` / `Pub` / `Unpub` / `Validate` / `GetCapacity`) | ✅ | `internal/csi/controller.go` |
| CSI Node (`Stage` / `Unstage` / `Pub` / `Unpub` / `Stats` / `Expand` / `GetInfo`) | ✅ | `internal/csi/node.go` etc. |
| CSI Identity (`GetPluginInfo` / `GetPluginCapabilities` / `Probe`) with real readiness | ✅ | `internal/csi/identity.go` (real `readyFn` landed 2026-06) |
| Volume mode: Filesystem | ✅ | `internal/csi/node.go` |
| Volume mode: Block | ✅ | `internal/csi/node.go` (bind-mounts raw device) |
| AccessMode `RWO` / `RWOP` / `ROX` | ✅ | `controller.go` validation |
| 4-layer override (Pool → Protocol → Binding → PVC annotation) | ✅ | `mergeParamsFromCRDs` + `applyPVCAnnotationOverrides` |
| `StorageClass` auto-create + ownerReference + manual-edit revert + drift Event | ✅ | `pillarstorageclass_controller.go` (drift Event landed 2026-06) |
| `pillaragent` storage-node label auto-management | ✅ | `pillaragent_controller.go` |
| Helm chart + CSI sidecars (`provisioner` / `attacher` / `resizer` / `livenessprobe` / `node-driver-registrar`) | ✅ | `charts/pillar-csi/templates/` |
| Native gRPC liveness/readiness probes (k8s ≥ 1.24) | ✅ | `agent-daemonset.yaml` + `node-daemonset.yaml` (landed 2026-06) |
| Graceful shutdown — `preStop` + `Drain` RPC + state-flush marker | ✅ | `cmd/agent/main.go` SIGTERM handler + Helm `preStop` (landed 2026-06) |
| mTLS opt-in (Secret reference + cert-manager mode) | ✅ | `internal/tlscreds/`, `internal/agentclient/`, Helm `mtls.*` values (landed 2026-06) |
| Helm `NOTES.txt` install verification | ✅ | `charts/pillar-csi/templates/NOTES.txt` (landed 2026-06) |
| `ListExports` returns real configfs scan | ✅ | `server_discovery.go` calls `nvmeof.ListExports` (landed 2026-06) |
| `kubebuilder` boilerplate `TODO(user)` cleanup | ✅ | `internal/webhook/v1alpha1/*` + 2 controller files (landed 2026-06) |
| README authored | ✅ | `README.md` (landed 2026-06) |

---

## 2026-06-21 hardening batch

Landed in this batch (one commit per item):

1. **`chore(webhook)`**: `PillarStore.spec.backend.zfs.pool` immutability — closes silent volume-dangle risk on pool rename.
2. **`feat(agent)`**: `ListExports` wired to real configfs scan — drift detection no longer silent no-op.
3. **`feat(nvmeof)`**: IPv6 `BindAddress` detection in `createPort` — `addr_adrfam` and listen wildcard now derived from parsed IP family.
4. **`chore`**: stale `TODO(user)` scaffolding comments removed across 6 files.
5. **`docs`**: README and this audit document.
6. **`feat(csi)`**: Identity Probe gated on real readiness — controller waits for `mgr.Started`; node checks `/dev/nvme-fabrics` and state-dir writability. `/dev/nvme-fabrics` literal extracted to `internal/csi/constants.go`.
7. **`feat(controller)`**: `StorageClass` drift emits Normal `StorageClassReverted` Event — operators see manual edits being reverted.
8. **`feat(helm)`**: Helm `NOTES.txt` printing post-install verification commands.
9. **`feat(proto)` + `feat(agent)`**: `Drain` RPC + handler + `DrainGuardInterceptor` — rejects new RPCs after drain with `codes.Unavailable`, waits for in-flight per-target locks, writes state-flush marker.
10. **`feat(cmd)`**: SIGTERM-driven sequence on agent + node — flip gRPC standard health to `NOT_SERVING` → call `Drain` (agent) → grace → `GracefulStop`.
11. **`feat(helm)`**: gRPC `livenessProbe` / `readinessProbe`, `preStop sleep 5`, `terminationGracePeriodSeconds: 60` on agent + node DaemonSets.
12. **`chore(helm)`**: `kubeVersion: ">=1.24.0-0"` on Chart.yaml so older clusters fail fast.
13. **`feat(helm)`**: top-level `mtls` values section + helper templates + Secret-mount wiring on controller and agent + cert-manager mode (Issuer + 2 Certificates rendered when `mtls.certManager.enabled`).
14. **`test(helm)`**: `test_render.sh` extended with mTLS-on / mTLS-off / cert-manager-on assertions and probe-rendering assertions.

---

## Out-of-scope items (per PRD)

These are explicitly deferred:

- **iSCSI** (Phase 2 — see [`docs/PRD-iscsi.md`](./PRD-iscsi.md)): scaffolding
  exists across `internal/agent`, `internal/csi`, CRD types and the Helm chart,
  but the agent LIO configfs writer, the node IQN reader, the CSINode IQN
  publisher and the node-image `open-iscsi` bundle are not yet implemented.
- **NFS / SMB** (Phase 3 / 6): CRD scaffolding only.
- **Snapshot / Clone** (Phase 4): not started.
- **External (non-K8s) agent nodes** (Phase 7): `PillarAgent.spec.external`
  type exists, controller `external` branch + agent packaging are not yet
  implemented.
- **`volumeMode: Block` for snapshot/clone**: pending Phase 4.
- **`RWX`**: requires NFS (Phase 3).

---

## Per-area readiness verdict

| Area | Verdict | Note |
|---|---|---|
| CRD types (`api/v1alpha1/`) | READY | All 5 cluster-scoped, conditions standard `metav1.Condition` |
| Validation webhooks | READY | Immutability complete incl. `backend.zfs.pool` |
| Reconcilers | READY | All conditions, watches, finalizers wired |
| Agent server + ZFS backend | READY | Read-back verification on every configfs write |
| LVM backend | READY (early) | Phase 5 per PRD but feature-complete |
| NVMe-oF/TCP protocol | READY | IPv4 + IPv6; `ListExports` real scan |
| CSI Controller / Node / Identity | READY | All Phase 1 RPCs incl. NodeExpand/Stats; Probe real readiness |
| Helm chart | READY | All sidecars, hostNetwork, grpc probes, preStop, mTLS, NOTES |
| Binaries (`cmd/agent`, `cmd/controller`, `cmd/node`) | READY | Drain + SIGTERM handler + gRPC health on agent and node |
| Tests | READY | 13 packages green; e2e runs under `make test-e2e` with Kind |
| Documentation | READY | README + PRD + this audit |
