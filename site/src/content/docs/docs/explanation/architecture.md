---
title: Architecture
description: "How pillar-csi works. A controller, a storage-node agent and a node plugin export ZFS zvols and LVM volumes to pods over the kernel NVMe-oF/TCP target."
sidebar:
  order: 1
---

pillar-csi turns ZFS zvols and LVM logical volumes on a storage node into Kubernetes volumes that pods on other nodes can use. It runs as three workloads from one Helm release. The Linux kernel carries the data: the storage node exports each volume with the in-kernel NVMe-oF target (`nvmet`), and the worker connects to it with the in-kernel NVMe/TCP initiator. pillar-csi configures both ends and then stays out of the I/O path.

pillar-csi is not a distributed filesystem. It does not replicate, stripe or pool data across nodes. Each volume lives on one storage node, in a ZFS pool or LVM volume group you already run there.

## Components

```text
Kubernetes API
CRDs, PVCs, PillarVolumeState
        |
        v
pillar-controller (Deployment)
  CRD reconcilers
  CSI Controller service
        |
        | control path:
        | gRPC :9500, mTLS opt-in
        v
storage node
  pillar-agent (DaemonSet)
    -> zfs / lvm commands
    -> nvmet configfs
  zvol or LV
    -> kernel nvmet subsystem
    -> TCP listener :4420
        |
        | data path:
        | NVMe/TCP, kernel only
        v
worker node
  pillar-node (DaemonSet)
    connect, mkfs, mount
  pod mounts /dev/nvmeXnY
```

The diagram shows only shipped parts. The one protocol is NVMe-oF over TCP, and the backends are ZFS zvols and LVM logical volumes.

### pillar-controller

A Deployment that runs the CRD reconcilers and the CSI Controller service, with the standard `csi-provisioner`, `csi-attacher`, `csi-resizer` and `livenessprobe` sidecars. It never touches a disk. For every volume operation it calls the agent on the storage node that owns the pool and records the result in the Kubernetes API. `replicaCount` defaults to 1; more replicas are safe because the manager and the sidecars use leader election.

### pillar-agent

A DaemonSet that runs only on storage nodes. The controller labels a node `pillar-csi.bhyoo.com/agent-node=true` when a `PillarAgent` resource points at it, and the agent DaemonSet's `nodeSelector` matches that label. The agent is a gRPC server on port 9500 with no Kubernetes API client. It creates and deletes zvols and logical volumes in the pools listed in its `--config` file (the chart renders `agent.backends` into it), and it writes the NVMe-oF target directly into `/sys/kernel/config/nvmet`. It reads back what it wrote, so a configfs write that did not take effect becomes an error. The agent runs with `hostNetwork: true` because the kernel binds the `nvmet_tcp` listener in the network namespace of the process that enables the port.

### pillar-node

A DaemonSet on every worker that implements the CSI Node service. When kubelet stages a volume, pillar-node connects to the storage node by writing the connect string to the kernel's `/dev/nvme-fabrics` device (it does not need `nvme-cli`), waits for the `/dev/nvmeXnY` device, formats it with ext4 or xfs if it has no filesystem yet, and mounts it. For `volumeMode: Block` it bind-mounts the raw device instead. It also uses `hostNetwork: true`, so the initiator's TCP connection starts from the host network where the target listens.

Both DaemonSets have an init container that runs `modprobe` against the host's `/lib/modules`: `nvme_fabrics` and `nvme_tcp` on workers, `nvmet` and `nvmet_tcp` on storage nodes. The init container loads modules the host already has; it cannot install missing ones, and it exits successfully even when a module fails to load. See [prerequisites](/docs/reference/prerequisites/).

### What the images contain

Each image carries the tools its component runs, so hosts do not need them.

| Image | Base | Tools inside |
| --- | --- | --- |
| `ghcr.io/isac322/pillar-csi/controller` | distroless static | the controller binary only |
| `ghcr.io/isac322/pillar-csi/agent` | Alpine 3.24 | OpenZFS 2.4 userland (`zfs`, `zpool`) and `lvm2`, with LVM set to work without a host udev daemon |
| `ghcr.io/isac322/pillar-csi/node` | Alpine 3.24 | `util-linux` (mount), `e2fsprogs` (mkfs.ext4, resize2fs), `xfsprogs` (mkfs.xfs, xfs_growfs) |

The agent and node images have no shell and no package manager. Neither image uses `nvme-cli`, `nvmetcli`, `targetcli` or SSH: the agent writes the target through configfs and the node plugin connects through `/dev/nvme-fabrics`. The modprobe init containers use `busybox`.

What the host must still provide is kernel-side: the NVMe-oF modules listed above, and on the storage node the pool itself. That means the ZFS kernel module with the pool imported, or device-mapper with the LVM volume group present (plus `dm_thin_pool` for thin pools).

## Control path and data path

The control path runs over gRPC. When a PVC is created, the controller resolves the StorageClass to a `PillarStore` and `PillarProtocol`, and asks the agent to create the backend volume and then to export it. When a pod is scheduled, the controller asks the agent to allow the worker's NVMe host NQN on that export (if the protocol enables ACLs), and kubelet asks pillar-node to connect and mount. Deletion runs the same steps in reverse. The controller-to-agent channel is plaintext by default; mTLS is opt-in (see [configure mTLS](/docs/how-to/configure-mtls/)).

The data path contains only kernel code. Once a volume is connected, reads and writes go from the pod's filesystem to the worker's NVMe/TCP initiator, over TCP to the storage node's `nvmet` target, and into the zvol or logical volume. pillar-agent, pillar-node and the controller can restart without interrupting I/O on volumes that are already connected.

## Custom resources

All pillar-csi resources are cluster-scoped and live in the `pillar-csi.bhyoo.com/v1alpha1` API group.

| Resource | What it describes | References |
| --- | --- | --- |
| `PillarAgent` | One storage node and how to reach its agent: a `nodeRef` to the Kubernetes node, whose address the controller resolves | a Kubernetes `Node` |
| `PillarStore` | One pool on that node: exactly one `zfs` or `lvm` backend member | `agentRef` to a `PillarAgent` |
| `PillarProtocol` | Network export settings: exactly one `nvmeofTcp` member (port, ACL on or off, queue size, in-capsule data size, reconnect timeouts) | none |
| `PillarStorageClass` | A store and a protocol combined into a generated Kubernetes `StorageClass`, with filesystem settings and per-class overrides | `storeRef`, `protocolRef` |
| `PillarVolumeState` | Internal. One per provisioned volume; the controller creates it and users never write it | the `PillarAgent` that hosts the volume |

A validating webhook rejects backend and protocol combinations that cannot work together. Each resource has a finalizer that blocks deletion while something still depends on it: a `PillarAgent` while stores reference it, a `PillarStore` while storage classes or volumes use it, a `PillarProtocol` while storage classes reference it, and a `PillarStorageClass` while PVCs use its generated `StorageClass`. The field-by-field reference is in [CRD reference](/docs/reference/crd/).

## One driver for every backend and protocol

pillar-csi is one CSI driver with one set of workloads: a controller, an agent DaemonSet and a node DaemonSet. The same set serves every pool and every backend and protocol the driver implements. To add a pool, you add it to `agent.backends` and create a `PillarStore`; the driver and the Helm release stay the same.

Configuration is split into three axes: the backend (`zfs` or `lvm`), the protocol (`nvmeofTcp`) and the filesystem (`fsType`, `mkfsOptions`, `mountOptions`). Each axis has one YAML shape, and the same shape appears at every layer that can set it. A later layer overrides an earlier one:

1. `PillarStore.spec.backend` and `PillarProtocol.spec.protocol` hold the defaults for the pool and the transport.
2. `PillarStorageClass.spec.overrides` and `spec.filesystem` override them for one storage class.
3. The PVC annotations `pillar-csi.bhyoo.com/backend`, `pillar-csi.bhyoo.com/protocol` and `pillar-csi.bhyoo.com/filesystem` override them for one volume.

For example, `zfs: {properties: {compression: zstd}}` means the same thing in a store, in a class override and in a PVC annotation. Overrides accept only tuning fields. Placement and security fields such as `pool`, `volumeGroup`, `port` and `acl` are rejected with their path. See [volume overrides](/docs/how-to/volume-overrides/) for the full rules.

Each axis is a union with one member per implementation, and the controller, node plugin and agent dispatch on that member. iSCSI, NFS and SMB are planned as new members of the same driver and the same configuration model. They are not implemented, and the CRDs accept only the `nvmeofTcp` protocol and the `zfs` and `lvm` backends today.

## Durable state

The controller keeps each volume's state in its `PillarVolumeState` so it can recover after a crash, a leader change or a storage-node reboot. The object records:

- the volume's identity and routing: the agent, the backend and protocol type, the capacity and the PVC it was provisioned for;
- the effective configuration resolved at the first `CreateVolume` attempt, so later edits to a store, protocol or class do not change existing volumes;
- the lifecycle phase, including partial-failure phases that record whether the backend volume and the export were created;
- the export spec (bind address, port, ACL flag), which is the desired state the controller uses to rebuild the export after the storage node loses its target configuration;
- the list of nodes the volume is published to, which enforces access-mode exclusivity;
- the `publicationGeneration` counter and the `deleting` flag, which fence stale operations.

The agent keeps two small records per volume in `/var/lib/pillar-csi/agent` on the storage node's own disk: a fencing mark and, for older exports, a pinned NVMe namespace identity. pillar-node records each staged volume in `/var/lib/pillar-csi/node/` on the worker so it can disconnect the right session at unstage. All three paths are `hostPath` mounts, never PVCs, so the storage stack does not depend on its own volumes. [Fencing and consistency](/docs/explanation/fencing-and-consistency/) explains how these records keep a stale operation from undoing a newer one.

## After a storage-node reboot

A reboot empties `nvmet` configfs, so every export on that node disappears. Connected workers keep retrying their connections while the node is down. When the agent starts, it refuses to create any export until the controller sends the complete export state for that node in one request. The agent then prepares every export before it enables any listener, so a reconnecting worker finds either no listener or its fully configured subsystem. Each namespace comes back with the same UUID, NGUID and serial it had before, which the worker's kernel requires in order to keep the device.

Workers reconnect only within the kernel's `ctrl_loss_tmo` window: 600 seconds by default, or the `ctrlLossTmo` set on the `PillarProtocol`. A storage node that stays down longer than that loses its connections for good, and the filesystems on those volumes see I/O errors. While a storage node is down, its volumes are unavailable; no other node holds a copy.

## Further reading

The design documents in the repository go deeper. Some are in Korean.

- [`docs/PRD.md`](https://github.com/isac322/pillar-csi/blob/master/docs/PRD.md): product requirements, CRD design, volume lifecycle
- [`docs/decisions/001-reconciler-design.md`](https://github.com/isac322/pillar-csi/blob/master/docs/decisions/001-reconciler-design.md): why the reconcilers use finalizers, conditions and node labels
- [`docs/RFC-multi-protocol-driver-foundation.md`](https://github.com/isac322/pillar-csi/blob/master/docs/RFC-multi-protocol-driver-foundation.md): the protocol-dispatch structure inside the controller, node and agent; only NVMe-oF/TCP is implemented
