---
title: Annotations, labels and parameters
description: Every pillar-csi.bhyoo.com annotation, label, StorageClass parameter, finalizer and volume attribute that pillar-csi reads or writes, and what each one means.
sidebar:
  order: 5
---

You set pillar-csi configuration with three keys: `pillar-csi.bhyoo.com/backend`, `pillar-csi.bhyoo.com/protocol` and `pillar-csi.bhyoo.com/filesystem`. Each value is a YAML document with the same shape as the matching subtree of `PillarStore.spec.backend`, `PillarProtocol.spec.protocol` or `PillarStorageClass.spec.filesystem`. The same three keys and shapes work as PVC annotations and as parameters of a hand-written StorageClass, so a setting reads the same wherever you write it. iSCSI, NFS and SMB are planned and designed to use the same `protocol` document; in v0.3.0 it accepts only `nvmeofTcp`.

All pillar-csi keys live under the `pillar-csi.bhyoo.com/` prefix. The CSI driver name and StorageClass provisioner is `pillar-csi.bhyoo.com`.

## Keys you set

### PVC annotations

Each value is limited to the tunable fields of its subtree. See [Override settings per binding or per volume](/docs/how-to/volume-overrides/).

| Key | Document shape | Tunable fields |
|---|---|---|
| `pillar-csi.bhyoo.com/backend` | `zfs: {...}` or `lvm: {...}` | `zfs.properties`, `lvm.provisioningMode` |
| `pillar-csi.bhyoo.com/protocol` | `nvmeofTcp: {...}` | `maxQueueSize`, `inCapsuleDataSize`, `ctrlLossTmo`, `reconnectDelay` |
| `pillar-csi.bhyoo.com/filesystem` | `{fsType, mkfsOptions, mountOptions}` | all three |

Rules:

- The controller reads these once, in `CreateVolume`. Later edits do not change an existing volume.
- Any other key under `pillar-csi.bhyoo.com/` on a PVC fails provisioning with `unsupported PVC annotation "<key>"`. The 0.2 keys `backend-override`, `protocol-override`, `fs-override` and `param.*` fall under this rule.
- Structural fields fail with `<key>: <path> is structural and cannot be set per volume`. Unknown fields fail with `unknown field`.
- `pillar-csi.bhyoo.com/filesystem` is rejected on a PVC with `volumeMode: Block`.

### StorageClass parameters

A StorageClass generated from a `PillarStorageClass` carries one pillar-csi parameter:

| Key | Value |
|---|---|
| `pillar-csi.bhyoo.com/storage-class` | name of the `PillarStorageClass` |

Any other `pillar-csi.bhyoo.com/` parameter on a generated class is rejected.

A StorageClass you write yourself (`provisioner: pillar-csi.bhyoo.com`) accepts:

| Key | Required | Value |
|---|---|---|
| `pillar-csi.bhyoo.com/store-ref` | yes | name of a `PillarStore` |
| `pillar-csi.bhyoo.com/protocol-ref` | yes | name of a `PillarProtocol` |
| `pillar-csi.bhyoo.com/backend` | no | backend document, as on a PVC |
| `pillar-csi.bhyoo.com/protocol` | no | protocol document, as on a PVC |
| `pillar-csi.bhyoo.com/filesystem` | no | filesystem document, as on a PVC |
| `csi.storage.k8s.io/fstype` | no | `ext4` or `xfs`; must equal the filesystem document's `fsType` if both are set |

Any other `pillar-csi.bhyoo.com/` parameter, including the 0.2 flat keys such as `zfs-prop.*`, `lvm-*`, `nvmeof-*`, `acl-enabled` and `backend-type`, is rejected.

## Keys pillar-csi writes

You do not set these. They are listed so you can recognize them and leave them alone.

### Labels

| Key | Object | Set by | Meaning |
|---|---|---|---|
| `pillar-csi.bhyoo.com/agent-node=true` | Node | controller | The node is referenced by a `PillarAgent` `nodeRef`. The agent DaemonSet's default `nodeSelector` matches it. |

### Annotations

| Key | Object | Set by | Meaning |
|---|---|---|---|
| `pillar-csi.bhyoo.com/nvmeof-host-nqn` | CSINode | node plugin at startup | The node's NVMe host NQN, read from `/etc/nvme/hostnqn` on the host. If `/etc/nvme/hostnqn` or `/etc/nvme/hostid` is missing, the node plugin writes a generated ID into it. These are ID files; no packages are installed. The controller uses the NQN to grant and revoke ACL access. |
| `pillar-csi.bhyoo.com/storage-class-carry-over` | PillarStorageClass | controller | Temporary record of the generated StorageClass's labels, annotations and `allowedTopologies` while the controller deletes and recreates the class. Removed once the new class exists. |
| `pillar-csi.bhyoo.com/success-recorded` | PillarVolumeState | controller | Marks a volume lifecycle created under the rule that `Ready` is recorded before `CreateVolume` reports success. Used when cleaning up abandoned provisioning attempts. |

### Finalizers

Each finalizer blocks deletion while something still depends on the object.

| Finalizer | Object | Blocks deletion while |
|---|---|---|
| `pillar-csi.bhyoo.com/pillar-agent-protection` | PillarAgent | a `PillarStore` references it, or a volume (PV or `PillarVolumeState`) on it remains |
| `pillar-csi.bhyoo.com/store-protection` | PillarStore | a `PillarStorageClass` references it, or a volume provisioned from it remains |
| `pillar-csi.bhyoo.com/protocol-protection` | PillarProtocol | a `PillarStorageClass` references it |
| `pillar-csi.bhyoo.com/storage-class-protection` | PillarStorageClass | a PVC uses the generated StorageClass, or a volume provisioned through it remains |

While deletion is blocked, the object's `Ready` condition is `False` with reason `DeletionBlocked`, and the message starts with `Deletion blocked:` and lists what still depends on it.

### Volume attributes

The controller writes these into `PersistentVolume.spec.csi.volumeAttributes` at `CreateVolume`. The node plugin reads them when it connects, formats and mounts. Keys that were not set anywhere are omitted. The first three keys have no prefix.

| Key | Value |
|---|---|
| `target_id` | target identifier; for NVMe-oF/TCP, the subsystem NQN |
| `address` | storage node address the export listens on, recorded at provisioning |
| `port` | TCP port the export listens on, recorded at provisioning |
| `pillar-csi.bhyoo.com/protocol-type` | `nvmeof-tcp` |
| `pillar-csi.bhyoo.com/volume-ref` | protocol-level reference of the volume (the NVMe subsystem name) |
| `pillar-csi.bhyoo.com/nvmeof-max-queue-size` | resolved `maxQueueSize` |
| `pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo` | resolved `ctrlLossTmo`, seconds |
| `pillar-csi.bhyoo.com/nvmeof-reconnect-delay` | resolved `reconnectDelay`, seconds |
| `pillar-csi.bhyoo.com/fs-type` | resolved `fsType` |
| `pillar-csi.bhyoo.com/mkfs-options` | resolved `mkfsOptions`, as a JSON string array |
| `pillar-csi.bhyoo.com/mount-options` | resolved `mountOptions`, as a JSON string array |

### Topology

The node plugin reports the topology segment `pillar-csi.bhyoo.com/nvmeof: "true"` on nodes where the `nvme_tcp` kernel module is loaded (`/sys/module/nvme_tcp` exists). Nodes without the module omit the key. You can use it in a StorageClass `allowedTopologies`.
