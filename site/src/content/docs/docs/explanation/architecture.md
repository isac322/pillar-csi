---
title: Architecture
description: "How pillar-csi works. A controller, a storage-node agent and a node plugin export ZFS zvols and LVM volumes over NVMe-oF/TCP or iSCSI, and ZFS datasets over NFSv4.2."
sidebar:
  order: 1
---

pillar-csi turns ZFS zvols, LVM logical volumes, and ZFS datasets on a storage node into Kubernetes volumes that pods can use. It runs as three workloads from one Helm release. Block data uses the Linux kernel's `nvmet` or LIO target and matching initiator; NFS datasets use the kernel NFS server and a bundled client mount helper. pillar-csi configures the endpoints and stays out of steady-state I/O.

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
    -> nvmet or LIO configfs
    -> owned NFS export state
  zvol, LV, or ZFS dataset
    -> block target or kernel NFS server
    -> TCP listener :4420, :3260, or :2049
        |
        | data path:
        | block protocol or NFSv4.2
        v
worker node
  pillar-node (DaemonSet)
    connect or log in, or mount NFS
  pod gets a block device or filesystem mount
```

The shipped protocols are NVMe-oF/TCP, iSCSI, and NFSv4.2. The shipped backends are ZFS zvols, ZFS datasets, and LVM logical volumes. SMB and directory backends are not offered.

### pillar-controller

A Deployment that runs the CRD reconcilers and the CSI Controller service, with the standard `csi-provisioner`, `csi-attacher`, `csi-resizer` and `livenessprobe` sidecars. It never touches a disk. For every volume operation it calls the agent on the storage node that owns the pool and records the result in the Kubernetes API. `replicaCount` defaults to 1; more replicas are safe because the manager and the sidecars use leader election.

### pillar-agent

A DaemonSet that runs only on storage nodes. The controller labels a node `pillar-csi.bhyoo.com/agent-node=true` when a `PillarAgent` resource points at it, and the agent DaemonSet's `nodeSelector` matches that label. The agent is a gRPC server on port 9500 with no Kubernetes API client. It creates and deletes zvols, datasets, and logical volumes in the pools listed in its `--config` file. It writes block targets through configfs and supervises only the NFS exports it owns; it never stops or reconfigures foreign NFS server state. It reads back what it writes and refuses to report the protocol ready when ownership or recovery cannot be established.

### pillar-node

A DaemonSet on every worker that implements the CSI Node service. For NVMe-oF and iSCSI it connects to the kernel block initiator; for NFS it mounts the server export with the bundled helper. It formats only blank block devices. NFS datasets are already filesystems, so `fsType`, `mkfsOptions`, periodic trim, and block volume mode are invalid for them. NFS defaults to hard, NFSv4.2 and TCP; contradictory mount flags are rejected.

pillar-node publishes each block initiator identity on its CSINode object. NFS ACLs use the node `InternalIP` resolved for each publication. It persists typed stage state so an unstage after plugin restart unmounts the exact NFS staging path; no host package or global mount helper is required.

Both DaemonSets have init containers that load only the kernel modules already present on the host. See [prerequisites](/docs/reference/prerequisites/).

### What the images contain

Each image carries the tools its component runs, so hosts do not need user-space packages.

| Image | Base | Tools inside |
| --- | --- | --- |
| `ghcr.io/isac322/pillar-csi/controller` | distroless static | the controller binary only |
| `ghcr.io/isac322/pillar-csi/agent` | Alpine 3.24 | OpenZFS 2.4 userland (`zfs`, `zpool`), `lvm2`, and supervised NFS export helpers |
| `ghcr.io/isac322/pillar-csi/node` | Alpine 3.24 | `util-linux`, `e2fsprogs`, `xfsprogs`, and bundled NFS mount utilities |

The images have no package manager. They do not use `nvme-cli`, `nvmetcli`, `targetcli`, `iscsiadm`, `iscsid` or SSH. Hosts still provide kernel support and storage; container-bundled utilities do not install host packages.

The control path runs over gRPC. When a PVC is created, the controller resolves the StorageClass to a `PillarStore` and `PillarProtocol`, then asks the agent to create the backend volume and export it. For NFS, `ControllerPublishVolume` uses the node's `InternalIP` for the export ACL when enabled; an empty allowed set denies volume data even if an unauthorized mount reaches an empty backing stub. Kubelet asks pillar-node to connect or mount. Deletion runs the same steps in reverse. The controller-to-agent channel is plaintext by default; mTLS is opt-in.

The data path is kernel-backed. Block I/O runs through the worker initiator and storage target; NFS I/O runs through the worker NFS client and storage-node NFS server. RPC TLS is not offered, and mTLS protects only controller-to-agent control traffic. A protocol ACL is access control, not encryption.

## Custom resources

All pillar-csi resources are cluster-scoped and live in the `pillar-csi.bhyoo.com/v1alpha1` API group.

| Resource | What it describes | References |
| --- | --- | --- |
| `PillarAgent` | One storage node and how to reach its agent: a `nodeRef` to the Kubernetes node, whose address the controller resolves | a Kubernetes `Node` |
| `PillarStore` | One pool and backend: ZFS zvol or dataset, or an LVM volume group | `agentRef` to a `PillarAgent` |
| `PillarProtocol` | Exactly one `nvmeofTcp`, `iscsi`, or `nfs` member; NFS is fixed to version 4.2 and port 2049 | none |
| `PillarStorageClass` | A store and a protocol combined into a generated Kubernetes `StorageClass`, with filesystem settings and per-class overrides | `storeRef`, `protocolRef` |
| `PillarVolumeState` | Internal. One per provisioned volume; the controller creates it and users never write it | the `PillarAgent` that hosts the volume |

A validating webhook rejects backend and protocol combinations that cannot work together, including NFS with zvol/LVM, NFS with Block mode, and localAttach for NFS.

## One driver for every backend and protocol

pillar-csi is one CSI driver with one set of workloads: a controller, an agent DaemonSet and a node DaemonSet. The same set serves every pool and every backend and protocol the driver implements. To add a pool, you add it to `agent.backends` and create a `PillarStore`; the driver and the Helm release stay the same.

Configuration is split into three axes: the backend (`zfs` or `lvm`), the protocol (`nvmeofTcp`, `iscsi`, or `nfs`) and the filesystem (`fsType`, `mkfsOptions`, `mountOptions`). Each axis has one YAML shape, and the same shape appears at every layer that can set it. A later layer overrides an earlier one:

1. `PillarStore.spec.backend` and `PillarProtocol.spec.protocol` hold the defaults for the pool and transport.
2. `PillarStorageClass.spec.overrides` and `spec.filesystem` override them for one storage class.
3. The PVC annotations `pillar-csi.bhyoo.com/backend`, `pillar-csi.bhyoo.com/protocol` and `pillar-csi.bhyoo.com/filesystem` override them for one volume.

For NFS, the ZFS dataset backend and NFS protocol are structural. `version: "4.2"` and `port: 2049` are fixed, `squash` defaults to `root`, and NFS protocol settings are not per-volume overrides. Only `filesystem.mountOptions` supplies mount flags; support defaults such as hard, NFSv4.2 and TCP cannot be contradicted. `fsType`, `mkfsOptions`, periodic trim and `localAttach` are rejected for NFS.

Each axis is a union with one member per implementation, and the controller, node plugin and agent dispatch on that member. The CRDs accept the `nvmeofTcp`, `iscsi`, and `nfs` protocols and the `zfs` and `lvm` backends. SMB and directory backends are not offered.

## Durable state

The controller keeps each volume's state in its `PillarVolumeState` so it can recover after a crash, a leader change or a storage-node reboot. The object records:

- the volume's identity and routing: the agent, backend and protocol type, capacity and PVC;
- the effective configuration resolved at the first `CreateVolume` attempt, so later edits do not change an existing volume;
- lifecycle phases recording whether the backend volume and export were created;
- the export spec, including bind address/port and protocol-specific ACL, squash, readonly and version state;
- the list of nodes the volume is published to, which enforces block access-mode exclusivity and tracks NFS ACL membership;
- the `publicationGeneration` counter and `deleting` flag, which fence stale operations.

The agent persists fencing and recovery records under the existing storage-node hostPath; pillar-node persists typed stage state under its worker hostPath so it can unmount the exact path after restart. These paths are never PVCs, so the storage stack does not depend on its own volumes. [Fencing and consistency](/docs/explanation/fencing-and-consistency/) explains how durable records keep stale operations from undoing newer ones.

## After a storage-node reboot

A reboot clears block target state and NFS export state, so every export on that node is temporarily unavailable. Connected workers keep retrying their connections or NFS mounts while the node is down. When the agent starts, it refuses to report availability until the controller sends the complete owned export state. The agent restores block identities and NFS root/child dataset exports from that state before serving listeners. It restores ACL membership, readonly state, squash, version and stable internal identities, and removes only pillar-csi-owned exports that are no longer desired. Foreign NFS exports are never stopped or reconfigured.

While a storage node is down, its volumes are unavailable; no other node holds a copy. Block protocols retain their existing timeout behavior. NFS clients retry according to their mount behavior, but an outage beyond workload tolerance can still surface I/O errors. A daemon failure cannot silently leave a volume reported healthy.

## Further reading

The design documents in the repository go deeper. Some are in Korean.

- [`docs/PRD.md`](https://github.com/isac322/pillar-csi/blob/master/docs/PRD.md): product requirements, CRD design, volume lifecycle
- [`docs/decisions/001-reconciler-design.md`](https://github.com/isac322/pillar-csi/blob/master/docs/decisions/001-reconciler-design.md): why the reconcilers use finalizers, conditions and node labels
- [`docs/RFC-multi-protocol-driver-foundation.md`](https://github.com/isac322/pillar-csi/blob/master/docs/RFC-multi-protocol-driver-foundation.md): the protocol-dispatch structure inside the controller, node and agent
- [`docs/PRD-iscsi.md`](https://github.com/isac322/pillar-csi/blob/master/docs/PRD-iscsi.md): the design of the iSCSI protocol, including why pillar-node has its own initiator instead of `open-iscsi`
