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
| Multi-pool | Extra Helm release per pool (SSH config, RBAC, sidecars duplicated) | Add one `PillarPool` CR |
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

The control plane (`pillar-controller`) runs as a `Deployment` and reconciles the `Pillar*` CRDs. `pillar-agent` runs as a `DaemonSet` on storage nodes only (auto-labelled when a `PillarTarget` CR is created) and owns all configfs writes on the host; `pillar-node` runs on every worker and handles the CSI Node service — initiator connect, mkfs, bind-mount. Both DaemonSets use `hostNetwork: true` so the NVMe-oF/TCP data plane can bind to the host network namespace.

| CRD | Purpose |
|---|---|
| `PillarTarget` | Locates a storage agent (in-cluster `nodeRef` or external address) |
| `PillarPool` | A storage pool on a target — ZFS pool name, LVM VG, and backend config |
| `PillarProtocol` | Network protocol configuration (NVMe-oF/TCP, iSCSI, NFS, SMB) |
| `PillarBinding` | Pool × Protocol → auto-generated `StorageClass` |

`PillarVolume` is an internal durable-state CRD used to recover from partial provisioning failures; users do not author it.

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
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarTarget
metadata:
  name: rock5bp
spec:
  nodeRef:
    name: rock5bp
---
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarPool
metadata:
  name: rock5bp-hot
spec:
  targetRef: rock5bp
  backend:
    type: zfs-zvol
    zfs:
      pool: tank
      parentDataset: k8s
---
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nvmeof-default
spec:
  type: nvmeof-tcp
  nvmeofTcp:
    port: 4420
    acl: true
---
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarBinding
metadata:
  name: hot
spec:
  poolRef: rock5bp-hot
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
kubectl describe pillartarget rock5bp        # AgentConnected / Ready conditions
kubectl describe pillarpool   rock5bp-hot    # PoolDiscovered / BackendSupported
kubectl describe pillarbinding hot           # PoolReady / ProtocolValid / Compatible / StorageClassCreated
kubectl describe pillarvolume <name>         # internal volume state after provisioning
kubectl describe pvc data                    # provisioner events
kubectl get events --field-selector reason=ProvisioningFailed
```

Controller and node logs:

```sh
kubectl logs -n pillar-csi deploy/pillar-csi-controller -c manager
kubectl logs -n pillar-csi ds/pillar-csi-node          -c node
kubectl logs -n pillar-csi ds/pillar-csi-agent         -c agent
```

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
