# pillar-csi

A Kubernetes CSI driver for self-hosted clusters that exposes local storage on
storage nodes (ZFS zvol, LVM logical volume, …) to the rest of the cluster over
modern network protocols (NVMe-oF/TCP, iSCSI*, NFS*).

> *iSCSI and NFS are designed but not yet shipped — see [Roadmap](#roadmap).
> Phase 1 ships **ZFS zvol + NVMe-oF/TCP**.

pillar-csi is **not** a distributed filesystem. It does not pool, stripe or
replicate storage across nodes. It takes whatever the operator has already
configured on a storage node (a ZFS pool, a thin LVM VG, …) and exports it
as-is over the wire.

## Why pillar-csi

| Concern | democratic-csi | pillar-csi |
|---|---|---|
| Language / footprint | Node.js | Go — single static binary |
| Deployment model | One Helm release per backend (controller + node DaemonSet duplicated) | Single cluster deployment, declarative `Pillar*` CRDs |
| Multi-pool | Extra Helm release per pool (SSH config, RBAC, sidecars duplicated) | Add one `PillarPool` CR |
| Storage-node IPC | SSH (parses shell output, key management, injection risk) | gRPC `pillar-agent` (typed, auto-reconnect, mTLS-capable) |
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
│              ┌─────────────┼─────────────┐              │
│           NVMe-oF/TCP   iSCSI*         NFS*              │
└─────────────┼─────────────┼─────────────┼────────────────┘
              │             │             │
              ▼             ▼             ▼
         /dev/nvmeXnY    /dev/sdX     mount point
                         worker node (Pod)
```

The control plane (`pillar-controller`) runs as a `Deployment` in the cluster
and reconciles four cluster-scoped CRDs:

| CRD | Purpose |
|---|---|
| `PillarTarget` | Where the agent lives (in-cluster `nodeRef` or external `address`) |
| `PillarPool` | A storage pool on a target (e.g. ZFS pool `tank`, LVM VG `data`) |
| `PillarProtocol` | A network protocol with its tunables (NVMe-oF/TCP, iSCSI, NFS, SMB) |
| `PillarBinding` | Pool × Protocol → auto-generated `StorageClass` |

A fifth CRD, `PillarVolume`, carries durable per-volume state used to recover
from partial provisioning failures.

`pillar-node` runs as a `DaemonSet` on every worker and implements the CSI
Node service: `nvme connect` (today) / `iscsiadm login` (planned) + mkfs +
mount.

The full design lives in [`docs/PRD.md`](docs/PRD.md).

## Supported matrix

| Backend | NVMe-oF/TCP | iSCSI* | NFS* |
|---|:---:|:---:|:---:|
| ZFS zvol | ✅ | 🚧 | — |
| LVM LV  | ✅ | 🚧 | — |
| ZFS dataset | — | — | 🚧 |

✅ Implemented & tested · 🚧 Designed, not yet shipped · — Not applicable

CSI features (Phase 1):
`CreateVolume`, `DeleteVolume`, `ControllerPublish/Unpublish`,
`ControllerExpandVolume`, `NodeStage/Unstage`, `NodePublish/Unpublish`,
`NodeExpandVolume`, `NodeGetVolumeStats`, `ValidateVolumeCapabilities`,
`GetCapacity`. Access modes: `ReadWriteOnce`, `ReadWriteOncePod`,
`ReadOnlyMany`. Volume modes: `Filesystem` (ext4/xfs) and `Block`.

## Install

Pre-built Helm chart lives under [`charts/pillar-csi/`](charts/pillar-csi/).

```sh
helm install pillar-csi charts/pillar-csi \
  --namespace pillar-csi --create-namespace
```

Optional flags:

```sh
# Enable mTLS between controller and agent.
helm install pillar-csi charts/pillar-csi \
  --set mtls.enabled=true \
  --set mtls.certManager.enabled=true        # auto-issue with cert-manager
# or
  --set mtls.secretRefs.controller.secretName=ctl-mtls \
  --set mtls.secretRefs.agent.secretName=agt-mtls   # user-provided Secret
```

**Cluster requirement:** Kubernetes ≥ 1.24 (Helm chart uses native `grpc:`
liveness/readiness probes which were promoted to beta in 1.24 and GA in 1.27).

**Storage node requirement:** kernel modules `nvmet`, `nvmet_tcp` (and ZFS or
LVM userspace tooling for the chosen backend) are loaded by the agent's
init-container, but the host kernel must support them — vanilla Linux ≥ 5.0
is sufficient.

## Quickstart

Create a target + pool + protocol + binding:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarTarget
metadata:
  name: rock5bp
spec:
  nodeRef:
    name: rock5bp     # K8s node hosting the agent DaemonSet pod
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarPool
metadata:
  name: rock5bp-hot
spec:
  targetRef: { name: rock5bp }
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
kind: PillarBinding
metadata:
  name: hot
spec:
  poolRef:     { name: rock5bp-hot }
  protocolRef: { name: nvmeof-default }
  storageClass:
    name: pillar-hot
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
```

Then a normal PVC against the generated `pillar-hot` `StorageClass`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: data, namespace: default }
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: pillar-hot
  resources: { requests: { storage: 10Gi } }
```

## Troubleshooting

Everything is reflected in standard Kubernetes resources — no extra CLI:

```sh
kubectl describe pillartarget rock5bp        # AgentConnected / Ready conditions
kubectl describe pillarpool   rock5bp-hot    # PoolDiscovered / BackendSupported
kubectl describe pillarbinding hot           # PoolReady / ProtocolValid / Compatible / StorageClassCreated
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

- [`docs/PRD.md`](docs/PRD.md) — product requirements (architecture, CRDs, lifecycle)
- [`docs/PRD-iscsi.md`](docs/PRD-iscsi.md) — iSCSI design (Phase 2)
- [`docs/RFC-multi-protocol-driver-foundation.md`](docs/RFC-multi-protocol-driver-foundation.md) — multi-protocol driver foundation
- [`docs/prd-audit-phase1-2026-06.md`](docs/prd-audit-phase1-2026-06.md) — current Phase 1 readiness audit
- [`docs/decisions/`](docs/decisions/) — architecture decision records

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1 | ZFS zvol + NVMe-oF/TCP, full CSI block lifecycle, Helm chart | **Shipped** |
| 2 | iSCSI protocol (LIO configfs) + ZFS-zvol/LVM combinations | In progress |
| 3 | ZFS dataset backend + NFS protocol + `RWX` | Planned |
| 4 | CSI snapshots / clones (ZFS native) | Planned |
| 5 | Standalone LVM backend hardening | Planned |
| 6 | SMB protocol | Planned |
| 7 | External (non-K8s) agent nodes | Planned |
| 8 | Additional backends: raw block, directory, Btrfs subvolume | Planned |

## Contributing

Project conventions live in [`CLAUDE.md`](CLAUDE.md):

- kubebuilder CLI for new CRDs, controllers, webhooks (never hand-edit
  `zz_generated.deepcopy.go` or `config/crd/`)
- "No Silent Failures": every `configfs` / `sysfs` write needs read-back
  verification or explicit `log.Error`
- `make lint` must pass with **0 issues** before every commit
- Commit messages: "why not what"

`make help` lists every Make target.

## License

Apache License 2.0.

```
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```
