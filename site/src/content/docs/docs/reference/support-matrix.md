---
title: Support matrix
description: "What pillar-csi supports: CSI capabilities, access and volume modes, ZFS and LVM backends, NVMe-oF/TCP, iSCSI and NFSv4.2, and features not supported yet."
sidebar:
  order: 2
---

This page describes the current pillar-csi release. Rows marked unreleased describe work that no release contains yet. The CSI driver name is `pillar-csi.bhyoo.com`.

## Backends and protocols

One driver serves every backend and protocol, and each combination uses the same resources: a `PillarStore` and a `PillarProtocol` joined by a `PillarStorageClass`. pillar-csi ships ZFS and LVM block volumes over NVMe-oF/TCP and iSCSI, and ZFS dataset filesystems over NFSv4.2. SMB is planned and unavailable.

| Backend | Volume type | NVMe-oF/TCP | iSCSI | NFS | SMB |
| --- | --- | --- | --- | --- | --- |
| ZFS | zvol (block) | Shipped | Shipped | Not applicable | Not applicable |
| ZFS | Dataset (file) | Not applicable | Not applicable | Shipped | Planned |
| LVM | Linear logical volume (block) | Shipped | Shipped | Not applicable | Not applicable |
| LVM | Thin logical volume (block) | Shipped | Shipped | Not applicable | Not applicable |

A protocol exports either block devices (NVMe-oF and iSCSI) or file systems (NFS and SMB). A block volume therefore pairs only with a block protocol, and a dataset only with a file protocol.

The API accepts `zfs` and `lvm` in `PillarStore.spec.backend`, and `nvmeofTcp`, `iscsi`, or `nfs` in `PillarProtocol.spec.protocol`. For ZFS, `volumeType` is `zvol` for block protocols and `dataset` for NFS. The `dir` backend and SMB protocol are not offered.

### Protocol features

| Feature | NVMe-oF/TCP | iSCSI |
| --- | --- | --- |
| Storage-node target | Kernel `nvmet`, written through configfs | Kernel LIO target, written through configfs |
| Node initiator | Kernel `nvme_tcp`, connected through `/dev/nvme-fabrics` | In-process initiator in pillar-node that hands the session to kernel `iscsi_tcp` |
| Storage-node kernel modules | `nvmet`, `nvmet_tcp` | `target_core_mod`, `target_core_iblock`, `iscsi_target_mod` |
| Worker kernel modules | `nvme_fabrics`, `nvme_tcp` | `iscsi_tcp` |
| Default port | 4420 | 3260 |
| Initiator identity | Host NQN, on CSINode annotation `pillar-csi.bhyoo.com/nvmeof-host-nqn` | Initiator IQN, on CSINode annotation `pillar-csi.bhyoo.com/iscsi-initiator-iqn` |
| Access control (`acl: true`) | Per host NQN | Per initiator IQN |
| Online expansion | Yes | Yes, the node rescans the SCSI device |
| Local attach | Yes | Yes |
| In-band authentication | Not supported yet (DH-HMAC-CHAP) | CHAP and mutual CHAP |
| Addresses per export | One | One portal per target. Multipath and multi-portal are not supported. |

### NFSv4.2 features

| Feature | NFS |
| --- | --- |
| Storage-node target | Kernel NFS server with supervised `rpc.mountd`/`exportfs`; only pillar-csi-owned exports are reconciled |
| Node initiator | Bundled NFS mount helper; no host `nfs-common` installation |
| Storage-node kernel support | NFS server (`nfsd`) support |
| Storage-node filesystem backing | Persistent `agent-state` hostPath at `/var/lib/pillar-csi/agent`; dedicated NFS-exportable backing for its `datasets` pseudoroot |
| Worker kernel support | NFS client support |
| Port and version | Fixed TCP port 2049, NFSv4.2 only |
| Access control (`acl: true`) | Published node `InternalIP` values; an empty ACL denies volume data (an unauthorized mount may expose only an empty backing stub) |
| Squash | `root` by default; `none` or `all` may be selected explicitly |
| Read-only policy | `ReadOnlyMany` exports are volume-wide readonly; RWO/RWX exports are writable |
| Online expansion | Yes, server-side dataset quota; `NodeExpansionRequired` is false |
| Local attach | No; NFS always uses the network mount |
| Formatting | No; `mkfsOptions`, explicit ext4/xfs and periodic trim are rejected |
| Mount flags | `filesystem.mountOptions` only; support defaults cannot be overridden |
| RPC TLS | Not offered |

Neither block protocol nor NFS requires a package on the host. The images carry the user-space helpers; hosts provide the required kernel support and storage, including the NFS pseudoroot backing.


pillar-csi does not replicate data. Each volume lives on one storage node, and it is unavailable while that node is down.

### NFS deployment support

| Storage-node layout | NFS support |
| --- | --- |
| Persistent agent state and a dedicated NFS-exportable filesystem at `/var/lib/pillar-csi/agent/datasets` | Required production layout; backing must support export filehandles and persist across restarts |
| Container overlay/rootfs as the pseudoroot backing | Unsupported; NFS server startup rejects backing that cannot encode export filehandles |
| Dedicated Kind test fixture with a temporary `tmpfs` pseudoroot | QA only; not persistent production backing or an automatic fallback |

The pseudoroot contains dataset mountpoints, not private export/recovery state. That state lives under `/var/lib/pillar-csi/agent/nfs`. Use canonical paths without symlinks. Missing exportable backing makes the configured NFS deployment unavailable; NFS is not an optional replacement for a configured dataset backend. See [NFS persistent backing](/docs/reference/prerequisites/#nfs-persistent-backing).


## Access modes

| PVC access mode | CSI access mode | Supported |
| --- | --- | --- |
| `ReadWriteOnce` | `SINGLE_NODE_WRITER` or `SINGLE_NODE_MULTI_WRITER` | Yes |
| `ReadWriteOncePod` | `SINGLE_NODE_SINGLE_WRITER` | Yes |
| `ReadOnlyMany` | `MULTI_NODE_READER_ONLY` | Yes |
| `ReadWriteMany` | `MULTI_NODE_MULTI_WRITER` | Yes, NFS only |

Block protocols reject RWX because ext4/xfs block filesystems are single-node filesystems. NFS publishes the same dataset to multiple node IPs; `acl: true` preserves the published-node set, and an empty ACL denies volume data even if an unauthorized mount request reaches the server.

The controller records which nodes a volume is published to. It refuses to publish a single-node block volume to a second node until the first node unpublishes it; NFS can retain concurrent RWX publications.

## Volume modes and filesystems

| Volume mode | Supported | Notes |
| --- | --- | --- |
| `Filesystem` | Yes | Block protocols use `ext4` (default) or `xfs`; NFS uses the mounted ZFS dataset and never formats it. |
| `Block` | Yes, block protocols only | The raw NVMe namespace or SCSI disk is bound into the Pod. NFS rejects Block. |

Filesystem settings (`fsType`, `mkfsOptions`, `mountOptions`) come from `PillarStorageClass.spec.filesystem` or the `pillar-csi.bhyoo.com/filesystem` PVC annotation. For NFS, `fsType` may be omitted or set to `nfs`; `mkfsOptions` and enabled periodic trim are rejected, and `mountOptions` is the only tunable filesystem setting.

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
| Controller | `EXPAND_VOLUME` | Yes, `pillar-csi.bhyoo.com` only |
| Controller | `SINGLE_NODE_MULTI_WRITER` | Yes |
| Controller | `MULTI_NODE_MULTI_WRITER` | Yes, NFS only |
| Controller | `GET_CAPACITY` | Yes. The chart does not turn on capacity tracking in `csi-provisioner`. |
| Controller | `CREATE_DELETE_SNAPSHOT`, `LIST_SNAPSHOTS` | Not supported yet |
| Controller | `CLONE_VOLUME` | Not supported yet |
| Node | `STAGE_UNSTAGE_VOLUME` | Yes, `pillar-csi.bhyoo.com` only |
| Node | `EXPAND_VOLUME` | Yes, `pillar-csi.bhyoo.com` only |
| Node | `GET_VOLUME_STATS` | Yes |

Volume expansion runs online for `pillar-csi.bhyoo.com` volumes. For block protocols, the agent grows the zvol or logical volume and the node grows the filesystem with `resize2fs` or `xfs_growfs`. For NFS, the agent grows the server-side dataset quota; the mounted client filesystem sees the new capacity without a node-side resize and `NodeExpansionRequired` is false. A generated StorageClass allows expansion unless `spec.storageClass.allowVolumeExpansion` is set to `false`.

The branch-only file identity `files.pillar-csi.bhyoo.com` advertises `GET_VOLUME_STATS` as its only node capability. Its node never stages a volume: the kubelet calls `NodePublishVolume` once per pod target and the node mounts that target directly, binding the owned proxy of the source for a local volume or mounting the owned-host NFS export for a multi-node volume. `NodeStageVolume`, `NodeUnstageVolume` and `NodeExpandVolume` return `Unimplemented`, and volume expansion is refused.

## Kubernetes features

| Feature | Supported |
| --- | --- |
| Dynamic provisioning | Yes |
| Persistent volumes | Yes |
| CSI ephemeral inline volumes | No. The CSIDriver lists only the `Persistent` lifecycle mode. |
| Volume snapshots | Not supported yet |
| Volume cloning | Not supported yet |
| `fsGroup` ownership changes | Yes, with protocol limits. The `pillar-csi.bhyoo.com` CSIDriver sets `fsGroupPolicy: File`; NFS uses the export's squash policy, so use `squash: none` explicitly when root/fsGroup initialization must reach the dataset. The `files.pillar-csi.bhyoo.com` CSIDriver sets `fsGroupPolicy: None`, so kubelet never rewrites ownership of an adopted source. |
| Attach before mount | Yes. Both CSIDrivers set `attachRequired: true`. |
| Reclaim policies | `Delete` (default) and `Retain` |
| Binding modes | `Immediate` (default) and `WaitForFirstConsumer` |

## Adopting existing volumes

A PVC annotation can adopt a volume that already exists on a storage node instead of creating one. See [Annotations](/docs/reference/annotations/) for the keys. LVM adoption and metadata-loss recovery are implemented on the current source revision but are not in a release yet: 0.5.3 and earlier do not contain them, and they need controller, agent and node images built from a source tree that includes them. "Partial" means the listed path is implemented with the limits shown; it does not mean every LVM layout or an automatic takeover is supported.

| Capability | Support | Limits |
| --- | --- | --- |
| Import a ZFS zvol (`import-zvol`) | Full | The zvol becomes an ordinary pillar-csi volume. With `reclaimPolicy: Delete`, deleting the PVC destroys it. Handing a zvol back intact is not supported. See [Import a zvol](/docs/how-to/import-zvol/). |
| Adopt an LVM LV, `PreserveOriginal` (default) | Partial, unreleased | Active linear LVs, or thin LVs of the store's configured thin pool. `DeleteVolume` releases the LV and keeps it and its pool; expansion is refused; the node mounts the existing filesystem without `mkfs`, `fsck`, automatic repair or resize. Read-write mounts still allow journal replay and workload writes, so the bytes do not stay identical. |
| Adopt an LVM LV, `Managed` (explicit) | Partial, unreleased | Same source checks. The LV then behaves like a volume pillar-csi created: `Delete` reclaim destroys it and expansion may grow it. |
| Inspect an LV through the agent `InspectVolume` RPC | Partial, unreleased | Read-only report of identity, layout, filesystem signature, exclusive-open state, consumers and exports. An unknown filesystem probe is reported as unknown, not blank. An empty consumer list does not rule out a mount the agent cannot see, which is why the exclusive-open check is reported separately. The RPC is not a separate permission: a client certificate the agent trusts can call every agent RPC. |
| Rebind a retained adopted volume to a new PVC | Partial, manual | The same PV, `volumeHandle` and `PillarVolumeState` are bound to a replacement claim; no new `CreateVolume` runs. The operator steps are in [Rebind a retained volume](/docs/how-to/import-lv/#rebind-a-retained-volume). |
| Recover an adopted LV after its PV or `PillarVolumeState` is lost | Partial, unreleased | Requires an agent-signed observation, an operator-signed authorization, persistent agent state, exact old/new lifecycle and LV identity, and verified owner-stop evidence. Recovery records are non-serving and non-reapable until the transfer commits; missing or uncertain evidence refuses the transfer. See [Recover after metadata loss](/docs/how-to/import-lv/#recover-after-metadata-loss). |
| Adopt an existing directory or ZFS filesystem (`import-directory`, `import-zfs-dataset`) | Partial, branch-only, unreleased | Opt-in `files.pillar-csi.bhyoo.com` identity only. The source must already carry an exact finite quota equal to the request; deleting the volume keeps the original directory or dataset, and expansion is refused. See [Existing filesystem adoption annotations](/docs/reference/annotations/#existing-filesystem-adoption-annotations). |
| Adopt snapshots, thin snapshot origins, thin pools, mirrors, RAID or inactive LVs | Unsupported | Import never activates or converts an LV. |

The pinned LV identity lives in the agent's persistent state under `/var/lib/pillar-csi/agent`, so LV adoption needs that hostPath to survive agent and node restarts. Mounting an adopted filesystem without formatting is implemented only for Linux nodes; on other platforms the stage fails with an error and never falls back to formatting. See [Adopt an existing LVM logical volume](/docs/how-to/import-lv/) for the workflow and [What is not supported](/docs/how-to/import-lv/#what-is-not-supported) for the remaining limits.

## Security

| Feature | Supported | Default |
| --- | --- | --- |
| mTLS between controller and agent | Yes, with cert-manager or your own Secrets | Off |
| NVMe-oF host access control by host NQN | Yes, `PillarProtocol.spec.protocol.nvmeofTcp.acl` | Off (`acl: false` allows any host) |
| iSCSI initiator access control by initiator IQN | Yes, `PillarProtocol.spec.protocol.iscsi.acl` | Off (`acl: false` allows any initiator) |
| NFS client access control by node IP | Yes, `PillarProtocol.spec.protocol.nfs.acl` | Off (`acl: false` allows any reachable client) |
| NFS root squash | Yes, `squash: root`, `none`, or `all` may be selected | `root` |
| NVMe-oF in-band authentication (DH-HMAC-CHAP) | Not supported yet | Not applicable |
| iSCSI transport encryption | Not supported. iSCSI data is not encrypted. | Not applicable |
| NFS RPC TLS | Not offered. Do not infer encryption from ACLs or mTLS. | Not applicable |

Protocol ACLs control who may connect; they do not encrypt volume traffic. See [Configure mTLS](/docs/how-to/configure-mtls/) and [Prerequisites](/docs/reference/prerequisites/).
