---
title: Support matrix
description: "What pillar-csi v0.3.3 supports: CSI capabilities, access and volume modes, filesystems, ZFS and LVM backends, NVMe-oF/TCP, and features not supported yet."
sidebar:
  order: 2
---

This page describes pillar-csi v0.3.3. The CSI driver name is `pillar-csi.bhyoo.com`.

## Backends and protocols

One driver serves every backend and protocol, and each combination uses the same resources: a `PillarStore` and a `PillarProtocol` joined by a `PillarStorageClass`. v0.3.3 ships ZFS and LVM over NVMe-oF/TCP. Cells marked Planned are not in this release.

| Backend | Volume type | NVMe-oF/TCP | iSCSI | NFS | SMB |
| --- | --- | --- | --- | --- | --- |
| ZFS | zvol (block) | Shipped | Planned | Not applicable | Not applicable |
| ZFS | Dataset (file) | Not applicable | Not applicable | Planned | Planned |
| LVM | Linear logical volume (block) | Shipped | Planned | Not applicable | Not applicable |
| LVM | Thin logical volume (block) | Shipped | Planned | Not applicable | Not applicable |

A protocol exports either block devices (NVMe-oF, and iSCSI once added) or file systems (NFS and SMB once added). A block volume therefore pairs only with a block protocol, and the planned ZFS dataset only with a file protocol.

In v0.3.3 the API accepts only `zfs` and `lvm` in `PillarStore.spec.backend` and only `nvmeofTcp` in `PillarProtocol.spec.protocol`. For ZFS, `volumeType` accepts only `zvol`.

pillar-csi does not replicate data. Each volume lives on one storage node, and it is unavailable while that node is down.

## Access modes

| PVC access mode | CSI access mode | Supported |
| --- | --- | --- |
| `ReadWriteOnce` | `SINGLE_NODE_WRITER` or `SINGLE_NODE_MULTI_WRITER` | Yes |
| `ReadWriteOncePod` | `SINGLE_NODE_SINGLE_WRITER` | Yes |
| `ReadOnlyMany` | `MULTI_NODE_READER_ONLY` | Yes |
| `ReadWriteMany` | `MULTI_NODE_MULTI_WRITER` | No, rejected |

The controller records which nodes a volume is published to. It refuses to publish a single-node volume to a second node until the first node unpublishes it.

## Volume modes and filesystems

| Volume mode | Supported | Notes |
| --- | --- | --- |
| `Filesystem` | Yes | `ext4` (default) or `xfs`. The node formats a volume only when it carries no filesystem. |
| `Block` | Yes | The raw NVMe namespace is bound into the Pod. |

Filesystem settings (`fsType`, `mkfsOptions`, `mountOptions`) come from `PillarStorageClass.spec.filesystem` or the `pillar-csi.bhyoo.com/filesystem` PVC annotation.

### Filesystem compatibility

The oldest node kernel pillar-csi supports is Linux 5.15. A volume can move to another node, so the node formats it with only the on-disk features that every supported kernel can mount, whatever the `mkfs` version in the node image:

| `fsType` | Features on a new volume |
| --- | --- |
| `xfs` | The xfsprogs Linux 5.15 LTS profile (`/usr/share/xfsprogs/mkfs/lts_5.15.conf` in the node image): V5 with CRC, reflink, bigtime, inode btree counters, and sparse inodes. Newer `mkfs.xfs` defaults stay off: reverse mapping (`rmapbt`), large extent counters (`nrext64`, needs Linux 5.19), exchange-range (`exchange`, needs Linux 6.10), and parent pointers (`parent`, needs Linux 6.12). |
| `ext4` | The `mke2fs` defaults of the node image. They include `orphan_file`, which needs Linux 5.15. |

The node image build fails if its `mkfs` would create a filesystem that Linux 5.15 cannot mount. To use a newer feature when every node that can mount the volume runs a kernel that supports it, opt in with `mkfsOptions`, for example `["-i", "exchange=1", "-n", "parent=1"]` for XFS. A value you set replaces the profile value for the same option. See [mkfs options](/docs/how-to/volume-overrides/#mkfs-options).

## CSI capabilities

| Service | Capability | Supported |
| --- | --- | --- |
| Identity | `CONTROLLER_SERVICE` | Yes |
| Identity | `VolumeExpansion`: `ONLINE` | Yes |
| Controller | `CREATE_DELETE_VOLUME` | Yes |
| Controller | `PUBLISH_UNPUBLISH_VOLUME` | Yes |
| Controller | `EXPAND_VOLUME` | Yes |
| Controller | `SINGLE_NODE_MULTI_WRITER` | Yes |
| Controller | `GET_CAPACITY` | Yes. The chart does not turn on capacity tracking in `csi-provisioner`. |
| Controller | `CREATE_DELETE_SNAPSHOT`, `LIST_SNAPSHOTS` | Not supported yet |
| Controller | `CLONE_VOLUME` | Not supported yet |
| Node | `STAGE_UNSTAGE_VOLUME` | Yes |
| Node | `EXPAND_VOLUME` | Yes |
| Node | `GET_VOLUME_STATS` | Yes |

Volume expansion runs online: the agent grows the zvol or logical volume, and the node grows the filesystem with `resize2fs` or `xfs_growfs`. A generated StorageClass allows expansion unless `spec.storageClass.allowVolumeExpansion` is set to `false`.

## Kubernetes features

| Feature | Supported |
| --- | --- |
| Dynamic provisioning | Yes |
| Persistent volumes | Yes |
| CSI ephemeral inline volumes | No. The CSIDriver lists only the `Persistent` lifecycle mode. |
| Volume snapshots | Not supported yet |
| Volume cloning | Not supported yet |
| `fsGroup` ownership changes | Yes. The CSIDriver sets `fsGroupPolicy: File`. |
| Attach before mount | Yes. The CSIDriver sets `attachRequired: true`. |
| Reclaim policies | `Delete` (default) and `Retain` |
| Binding modes | `Immediate` (default) and `WaitForFirstConsumer` |

## Security

| Feature | Supported | Default |
| --- | --- | --- |
| mTLS between controller and agent | Yes, with cert-manager or your own Secrets | Off |
| NVMe-oF host access control by host NQN | Yes, `PillarProtocol.spec.protocol.nvmeofTcp.acl` | Off (`acl: false` allows any host) |
| NVMe-oF in-band authentication (DH-HMAC-CHAP) | Not supported yet | Not applicable |
| NVMe-oF/TCP transport encryption (TLS) | Not supported yet | Not applicable |

See [Configure mTLS](/docs/how-to/configure-mtls/) and [Prerequisites](/docs/reference/prerequisites/).
