---
title: Annotations, labels and parameters
description: Every pillar-csi.bhyoo.com annotation, label, StorageClass parameter, finalizer and volume attribute that pillar-csi reads or writes, and what each one means.
sidebar:
  order: 5
---

You set pillar-csi configuration with three keys: `pillar-csi.bhyoo.com/backend`, `pillar-csi.bhyoo.com/protocol` and `pillar-csi.bhyoo.com/filesystem`. Each value is a YAML document with the same shape as the matching subtree of `PillarStore.spec.backend`, `PillarProtocol.spec.protocol` or `PillarStorageClass.spec.filesystem`. The same three keys and shapes work as PVC annotations and as parameters of a hand-written StorageClass. The `protocol` document accepts `nvmeofTcp`, `iscsi`, or `nfs`, and its member must match the `PillarProtocol`. On the default CSI identity, NFS requires a ZFS dataset backend; the opt-in file CSI identity also supports NFS for adopted directories. NFS version, port, ACL and squash are structural and not per-volume tunables. SMB is unavailable. The directory backend is available only for opt-in existing-filesystem adoption through the separate `files.pillar-csi.bhyoo.com` CSI identity.

All pillar-csi keys live under the `pillar-csi.bhyoo.com/` prefix. The CSI driver name and StorageClass provisioner is `pillar-csi.bhyoo.com`.

## Keys you set

### PVC annotations

Each value is limited to the tunable fields of its subtree. See [Override settings per binding or per volume](/docs/how-to/volume-overrides/).

| Key | Document shape | Tunable fields |
|---|---|---|
| `pillar-csi.bhyoo.com/backend` | `zfs: {...}` or `lvm: {...}` | `zfs.properties`, `lvm.provisioningMode` |
| `pillar-csi.bhyoo.com/protocol` | `nvmeofTcp: {...}`, `iscsi: {...}`, or `nfs: {}` | block protocol tuning fields; NFS has no per-volume protocol tunables |
| `pillar-csi.bhyoo.com/filesystem` | `{fsType, mkfsOptions, mountOptions, periodicTrim}` | block: all four; NFS: `fsType` omitted or `nfs`, and `mountOptions` only |

Rules:

- The controller reads these once, in `CreateVolume`. Later edits do not change an existing volume.
- Any other key under `pillar-csi.bhyoo.com/` on a PVC fails provisioning with `unsupported PVC annotation "<key>"`. The 0.2 keys `backend-override`, `protocol-override`, `fs-override` and `param.*` fall under this rule.
- Structural fields fail with `<key>: <path> is structural and cannot be set per volume`. Unknown fields fail with `unknown field`.
`pillar-csi.bhyoo.com/filesystem` is rejected on a PVC with `volumeMode: Block`. For NFS, `fsType` may be omitted or `nfs`; nonempty `mkfsOptions`, enabled periodic trim, and contradictory mount flags are rejected.

Three more PVC annotations are not configuration documents. They adopt an existing volume instead of creating one, and they are valid only on a PVC, not as StorageClass parameters. A claim may carry at most one adoption selector: `import-zvol`, `import-lv`, or one of the filesystem selectors described below.

| Key | Value |
|---|---|
| `pillar-csi.bhyoo.com/import-zvol` | full name of an existing ZFS zvol, for example `hot-data/k8s/pvc-0d52...`. `CreateVolume` adopts that zvol instead of creating a volume. The zvol must sit directly under the store's `pool` and `parentDataset` (exactly `<pool>/<parentDataset>/<name>`, or `<pool>/<name>` without a `parentDataset`), be unused on the storage node and be at least the requested size. See [Import a zvol from another CSI driver](/docs/how-to/import-zvol/). |
| `pillar-csi.bhyoo.com/import-lv` | existing LVM logical volume as `<vg>/<lv>:<vg_uuid>:<lv_uuid>`, for example `data-vg/legacy:<vg_uuid>:<lv_uuid>`. All four parts are required; read the UUIDs with `lvs -o vg_uuid,lv_uuid`. The store must use the `lvm` backend with the same volume group. The LV must be active, at least the requested size, unused on the storage node, linear when the store has no thin pool, and a thin LV of the store's configured thin pool when it has one. Snapshots, thin snapshot origins, pools, mirrors, RAID and other LV types are refused. Not in a release yet. See [Adopt an existing LVM logical volume](/docs/how-to/import-lv/). |
| `pillar-csi.bhyoo.com/import-lv-policy` | `PreserveOriginal` (the default when the key is absent) or `Managed`; the value is case-sensitive and anything else is refused. `PreserveOriginal` keeps the LV and its data: `DeleteVolume` only releases it, expansion is refused, and the node mounts the existing filesystem without `mkfs`, `fsck` or resize. `Managed` turns the LV into an ordinary volume that a `Delete` reclaim policy destroys and expansion may grow; you get it only by writing `Managed`. Valid only together with `import-lv`. |

The first `CreateVolume` pins the adopted LV's names, UUIDs and policy in the `PillarVolumeState` and in the agent's persistent state under `/var/lib/pillar-csi/agent`. Retries replay that record: an annotation that later names another LV, other UUIDs or another policy fails with `InvalidArgument`, and the agent refuses to retarget the volume or downgrade `PreserveOriginal` to `Managed`. Import never activates, renames, resizes or otherwise changes the volume group or the LV. `import-zvol` behavior is unchanged.

### Existing filesystem adoption annotations

The file CSI identity is branch-only and unreleased. Set `fileDriver.enabled: true` in the chart and set `PillarStorageClass.spec.csiDriver: files.pillar-csi.bhyoo.com`; the default `pillar-csi.bhyoo.com` identity does not route filesystem-adoption claims. The file driver is disabled by default and does not dynamically create a directory or ZFS filesystem.

| Key | Value | Requirements |
|---|---|---|
| `pillar-csi.bhyoo.com/import-directory` | Canonical absolute directory path, such as `/srv/pillar/app-data` | PVC annotation only. The path must be strictly below the configured directory backend `hostRoot`, use ext4 or XFS, and already have a nonzero project ID with a finite enforceable project quota exactly equal to the PVC request. |
| `pillar-csi.bhyoo.com/import-zfs-dataset` | Full existing ZFS filesystem dataset name, such as `tank/k8s/existing-dataset` | PVC annotation only. The dataset must be directly below the store's configured `pool` and `parentDataset`, with a finite effective `refquota` or `quota` exactly equal to the PVC request. |

The two filesystem selectors are mutually exclusive with each other, with `pillar-csi.bhyoo.com/import-zvol` and with `pillar-csi.bhyoo.com/import-lv`. Adoption reads and records the native filesystem UUID, root inode and project identity for directories, or the native dataset GUID for ZFS. It revalidates that identity, the exact quota and the configured backend layout on later operations. A changed source, quota, project ID, dataset GUID or layout is refused. Filesystem expansion is refused.

Filesystem adoption preserves the source's data, properties, ownership, ACLs and mount state. The file CSI identity uses `fsGroupPolicy: None`, so kubelet does not recursively change source ownership. With `localAttach: true`, a single-node claim uses a direct mount on the agent's Kubernetes node and receives that node as its topology. With `localAttach: false`, `ReadWriteMany` requires `fileDriver.nfs.enabled: true`; the owned-host NFS server and file node use an actual NFSv4.2 mount. The topology constraint is not a substitute for the remote NFS data path. The file node does not stage volumes: its `NodeGetCapabilities` advertises only `GET_VOLUME_STATS`, and `NodePublishVolume` mounts each pod target directly, binding the owned proxy of the source for a local volume or mounting the owned NFS export for a multi-node one. `NodeStageVolume`, `NodeUnstageVolume` and `NodeExpandVolume` return `Unimplemented`. `NodeUnpublishVolume` removes only the target it is asked for; the source, the owned proxy and the NFS export stay in place.

Use `reclaimPolicy: Retain` when you need to rebind a retained PV manually. After the old PVC is gone, remove the PV's stale `spec.claimRef`, then create a new PVC with `spec.volumeName` set to that PV. Keep the file CSI driver and existing volume handle. Deleting a filesystem-adoption volume retires CSI ownership and exports but preserves the original directory or dataset. This deletion rule does not change the separate zvol-import behavior: an imported zvol with `reclaimPolicy: Delete` is still destroyed.

### StorageClass parameters

A StorageClass generated from a `PillarStorageClass` carries one pillar-csi parameter:

| Key | Value |
|---|---|
| `pillar-csi.bhyoo.com/storage-class` | name of the `PillarStorageClass` |


A StorageClass you write yourself (`provisioner: pillar-csi.bhyoo.com`) accepts:

| Key | Required | Value |
|---|---|---|
| `pillar-csi.bhyoo.com/store-ref` | yes | name of a `PillarStore` |
| `pillar-csi.bhyoo.com/protocol-ref` | yes | name of a `PillarProtocol` |
| `pillar-csi.bhyoo.com/backend` | no | backend document, as on a PVC |
| `pillar-csi.bhyoo.com/protocol` | no | protocol document, as on a PVC |
| `pillar-csi.bhyoo.com/filesystem` | no | filesystem document, as on a PVC |
| `pillar-csi.bhyoo.com/local-attach` | no | `"true"` or `"false"`, default `false`; rejected for legacy NFS volumes on the default CSI identity because those volumes always use their network mount. For adopted files, `localAttach: true` is the direct single-node mode. For block volumes, see [Attach volumes locally on the storage node](/docs/how-to/local-attach/) |
| `csi.storage.k8s.io/fstype` | no | `ext4`, `xfs`, or `nfs`; must equal the filesystem document's `fsType` if both are set |

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
| `pillar-csi.bhyoo.com/iscsi-initiator-iqn` | CSINode | node plugin at startup | The node's iSCSI initiator IQN, read from `InitiatorName=` in `/etc/iscsi/initiatorname.iscsi` on the host. If the file is missing, the node plugin generates `iqn.2026-01.com.bhyoo.pillar-csi:node.<32 hex digits>` and writes it there. No packages are installed. The controller uses the IQN to grant and revoke ACL access. The node plugin omits the key when the `iscsi_tcp` kernel module is not loaded. |
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
| `target_id` | target identifier; for NVMe-oF/TCP, the subsystem NQN; for iSCSI, the target IQN; for NFS, the server export path |
| `address` | storage node address the export listens on, recorded at provisioning |
| `port` | TCP port the export listens on, recorded at provisioning; NFS is fixed at `2049` |
| `pillar-csi.bhyoo.com/protocol-type` | `nvmeof-tcp`, `iscsi` or `nfs` |
| `pillar-csi.bhyoo.com/nfs-version` | resolved NFS version, currently `4.2` |
| `pillar-csi.bhyoo.com/volume-ref` | protocol-level reference of the volume (the NVMe subsystem name, iSCSI LUN number `0`, or the NFS export path) |
| `pillar-csi.bhyoo.com/nvmeof-max-queue-size` | resolved `maxQueueSize` |
| `pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo` | resolved `ctrlLossTmo`, seconds |
| `pillar-csi.bhyoo.com/nvmeof-reconnect-delay` | resolved `reconnectDelay`, seconds |
| `pillar-csi.bhyoo.com/iscsi-login-timeout` | resolved `loginTimeout`, seconds |
| `pillar-csi.bhyoo.com/iscsi-replacement-timeout` | resolved `replacementTimeout`, seconds |
| `pillar-csi.bhyoo.com/iscsi-noop-out-interval` | resolved `noopOutInterval`, seconds |
| `pillar-csi.bhyoo.com/iscsi-noop-out-timeout` | resolved `noopOutTimeout`, seconds |
| `pillar-csi.bhyoo.com/fs-type` | resolved `fsType` |
| `pillar-csi.bhyoo.com/mkfs-options` | resolved `mkfsOptions`, as a JSON string array |
| `pillar-csi.bhyoo.com/mount-options` | resolved `mountOptions`, as a JSON string array |
| `pillar-csi.bhyoo.com/periodic-trim` | resolved `periodicTrim`, `"true"` or `"false"`; omitted when no layer sets it, and the node then trims the volume. See [Reclaiming freed space](/docs/how-to/volume-overrides/#reclaiming-freed-space) |

### Publish context

`ControllerPublishVolume` returns these keys for a [local attach](/docs/how-to/local-attach/), and the NVMe/TCP digest keys for an NVMe/TCP publish over the network whose `PillarProtocol` enables them. Otherwise a publish over the network protocol returns an empty publish context. Kubernetes stores the map on the `VolumeAttachment` and passes it to the node plugin.

| Key | Value |
|---|---|
| `pillar-csi.bhyoo.com/attach-mode` | `local` |
| `pillar-csi.bhyoo.com/local-node` | name of the storage node the publish is for; `NodeStageVolume` on any other node fails with `FAILED_PRECONDITION` |
| `pillar-csi.bhyoo.com/local-device-path` | path of the backend zvol or logical volume on that node, as reported by the agent |
| `pillar-csi.bhyoo.com/nvmeof-hdr-digest` | `"true"` when the volume's current `PillarProtocol` sets `nvmeofTcp.hdrDigest`; the node connects with `hdr_digest`. See [Header and data digests](/docs/how-to/tune-nvmeof/#header-and-data-digests) |
| `pillar-csi.bhyoo.com/nvmeof-data-digest` | `"true"` when the volume's current `PillarProtocol` sets `nvmeofTcp.dataDigest`; the node connects with `data_digest` |

### Topology

The node plugin reports the topology segment `pillar-csi.bhyoo.com/nvmeof: "true"` on nodes where the `nvme_tcp` kernel module is loaded (`/sys/module/nvme_tcp` exists). Nodes without the module omit the key. You can use it in a StorageClass `allowedTopologies`. There is no topology key for iSCSI.
