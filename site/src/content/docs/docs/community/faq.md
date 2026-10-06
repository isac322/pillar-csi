---
title: FAQ
description: "Answers about pillar-csi, the ZFS zvol, ZFS dataset, LVM, NVMe-oF/TCP, iSCSI and NFSv4.2 CSI driver: replication, host and kernel requirements, RWX, snapshots and reboots."
sidebar:
  order: 1
---

## Is pillar-csi a distributed filesystem like Ceph or Longhorn?

No. pillar-csi does not replicate, stripe or pool data across nodes. Each volume is a ZFS zvol or LVM logical volume in a pool on one storage node, and pillar-csi exports it to the node that runs the pod. If you need a volume to survive the loss of a node, use replicated storage such as Longhorn or Rook/Ceph. The [comparison page](/docs/explanation/comparison/) covers the trade-offs.

## Do I need RDMA or special network hardware?

No. pillar-csi uses NVMe-oF over TCP, iSCSI, or NFSv4.2, and they run on ordinary Ethernet and IP. RDMA transports such as RoCE and InfiniBand are not implemented; TCP is the only transport. NVMe-oF listens on port 4420, iSCSI on 3260, and NFS on fixed port 2049.

## What do I need to install on the hosts?

No user-space packages. The images carry the tools pillar-csi runs: the agent image has the OpenZFS userland and `lvm2`, and the node image has block filesystem tools plus the bundled NFS mount helper. The node plugin connects through the kernel's `/dev/nvme-fabrics` device, so workers do not need `nvme-cli`. For iSCSI, the node plugin logs in with its own initiator and hands the connection to the kernel, so workers do not need `open-iscsi`, `iscsiadm` or `iscsid`. For NFS, the node image supplies the mount helper; workers need only NFS client kernel support. The agent writes block targets through configfs and owns its NFS export state, so storage nodes do not need `nvmetcli`, `targetcli`, host `rpc.mountd` configuration, or SSH access.

The hosts must provide what a container cannot: the kernel modules or built-in support for the protocol, and on the storage node the pool itself (a ZFS pool imported by the host's ZFS kernel module, or an LVM volume group). You also list each pool in the chart's `agent.backends` value, which is empty by default. At run time the agent and node plugin keep small state files in hostPath directories under `/var/lib/pillar-csi/`; that is data, not installed software. See [prerequisites](/docs/reference/prerequisites/).

## What are the kernel requirements?

For NVMe-oF/TCP, storage nodes need the `nvmet` and `nvmet_tcp` modules; workers need `nvme_fabrics` and `nvme_tcp`. For iSCSI, storage nodes need `target_core_mod`, `target_core_iblock` and `iscsi_target_mod`; workers need `iscsi_tcp`. The chart's init containers only run `modprobe` against the host's `/lib/modules`, so the modules must already be built for the running kernel. Some kernels leave them out or ship them in a separate package, and then you need a kernel or module package that provides them. Check with `modprobe nvmet_tcp` or `modprobe iscsi_target_mod` on the storage node and `modprobe nvme_tcp` or `modprobe iscsi_tcp` on each worker before you install. The CPU architecture does not matter as long as the kernel has these modules and the images are published for it.

## Does it support ReadWriteMany (RWX)?

Yes, for NFS only. A ZFS dataset exported over NFSv4.2 supports `ReadWriteOnce`, `ReadWriteOncePod`, `ReadWriteMany`, and `ReadOnlyMany`. Block volumes over NVMe-oF/TCP and iSCSI continue to reject RWX because ext4/xfs filesystems cannot be mounted read-write by multiple nodes safely.

## Does it support snapshots or clones?

Not yet. The controller does not implement the CSI snapshot calls. You can still take ZFS or LVM snapshots on the storage node yourself, but Kubernetes `VolumeSnapshot` objects will not work with pillar-csi.

## Can I grow a volume?

Yes. Edit the PVC's `spec.resources.requests.storage`. The controller grows the zvol or logical volume, and the node plugin grows the filesystem. See [expand a volume](/docs/how-to/expand-volume/). An LV adopted with the `PreserveOriginal` policy is the exception: pillar-csi refuses to expand it.

## Can pillar-csi use a volume that already exists?

Yes, with limits. The `pillar-csi.bhyoo.com/import-zvol` PVC annotation adopts a ZFS zvol, which then becomes an ordinary pillar-csi volume; see [import a zvol](/docs/how-to/import-zvol/). The `pillar-csi.bhyoo.com/import-lv` annotation adopts an active linear LV, or a thin LV of the store's thin pool, and keeps its data by default: deleting the PVC releases the LV instead of removing it. LVM adoption and metadata-loss recovery are implemented on the current source revision but are not in a tagged release yet. Recovery requires an agent-signed snapshot, an operator-signed grant, persistent agent state and verified owner-stop evidence; uncertain ownership is refused. See [adopt an existing LVM logical volume](/docs/how-to/import-lv/) and the [support matrix](/docs/reference/support-matrix/#adopting-existing-volumes).

## Can I use more than one storage node or pool?

Yes. Create one `PillarAgent` per storage node and one `PillarStore` per pool, then a `PillarStorageClass` for each store you want to offer. All of them run under the same Helm release. Each pool must also be listed in `agent.backends` in the chart values, which is empty by default.

## Does iSCSI need another driver? Will NFS or SMB?

No. iSCSI and NFS are part of the same driver: create a `PillarProtocol` with the `iscsi` or `nfs` member and bind it to a compatible store with a `PillarStorageClass`. NFS requires a ZFS dataset (`zfs.volumeType: dataset`), uses NFSv4.2 on fixed port 2049, and supports RWX. SMB remains planned and unavailable; the API does not offer its protocol member. See the [support matrix](/docs/reference/support-matrix/).

## How does democratic-csi compare?

Both can export ZFS zvols over NVMe-oF and iSCSI. democratic-csi manages a ZFS-on-Linux host by running commands over SSH and expects you to set up `nvmetcli` and the target ports by hand. pillar-csi runs a gRPC agent on the storage node that writes the kernel target configuration directly and reads it back. It adds LVM, fencing of stale requests and stable namespace identity across reboots, and it serves any number of pools from one Helm release. democratic-csi covers far more: TrueNAS and Synology appliances, NFS and SMB, snapshots and RWX. The [comparison page](/docs/explanation/comparison/#democratic-csi) has details and sources.

## What happens when the storage node reboots?

Its volumes are unavailable until it comes back. Workers keep retrying their connections or NFS mounts in the meantime. The reboot clears kernel block-target state and NFS export state, so when the agent starts, the controller sends the complete list of owned exports for that node and the agent rebuilds them before reporting exports ready. Block namespaces and iSCSI targets return with stable identities; NFS restores the owned root and child dataset exports with their recorded ACL, squash, readonly, and version state. Foreign NFS exports are never stopped or reconfigured.

NVMe-oF workers retry only for the kernel's `ctrl_loss_tmo` period: 600 seconds by default, or the `ctrlLossTmo` value in the `PillarProtocol`. If the node is down longer than that, the workers drop the connection and filesystems see I/O errors. iSCSI workers keep logging in again, but the kernel holds I/O only for `replacementTimeout` seconds, 120 by default, and then fails it. NFS clients retry according to their mount behavior; an outage longer than the workload's tolerance can still surface I/O errors. Plan longer outages with [node maintenance](/docs/how-to/node-maintenance/). 

## Is the traffic encrypted?

The controller-to-agent gRPC channel can use mTLS, but it is off by default; see [configure mTLS](/docs/how-to/configure-mtls/). pillar-csi does not configure TLS for NVMe/TCP, iSCSI, or NFS RPC, so volume data crosses the network unencrypted. ACLs limit which nodes may connect but do not encrypt traffic. iSCSI CHAP is available only when configured on the iSCSI protocol. Keep storage traffic on a network you trust, and set protocol ACLs explicitly when other machines share the network.

## Which filesystems does it support?

Block protocols format new volumes with ext4 (the default) or xfs and mount them. NFS uses an already-mounted ZFS dataset and never formats it. For NFS, `fsType` may be omitted or set to `nfs`; `mountOptions` is the only supported filesystem setting. `mkfsOptions`, enabled periodic trim, and explicit block volume mode are rejected. It never reformats a volume that already has a filesystem. With `volumeMode: Block`, the pod gets the raw NVMe or SCSI device and no filesystem is created.

For NFS, `squash: root` is the conservative default. Select `squash: none` explicitly when a workload needs root or fsGroup initialization to reach the dataset; this is an authorization choice, not transport protection.
