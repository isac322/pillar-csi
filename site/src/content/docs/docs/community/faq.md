---
title: FAQ
description: "Answers about pillar-csi, the ZFS and LVM NVMe-oF CSI driver: replication, RDMA, host and kernel requirements, RWX, snapshots, democratic-csi and reboots."
sidebar:
  order: 1
---

## Is pillar-csi a distributed filesystem like Ceph or Longhorn?

No. pillar-csi does not replicate, stripe or pool data across nodes. Each volume is a ZFS zvol or LVM logical volume in a pool on one storage node, and pillar-csi exports it to the node that runs the pod. If you need a volume to survive the loss of a node, use replicated storage such as Longhorn or Rook/Ceph. The [comparison page](/docs/explanation/comparison/) covers the trade-offs.

## Do I need RDMA or special network hardware?

No. pillar-csi uses NVMe-oF over TCP, which runs on ordinary Ethernet and IP. RDMA transports such as RoCE and InfiniBand are not implemented; TCP is the only transport. The target listens on port 4420 unless the `PillarProtocol` sets another port.

## What do I need to install on the hosts?

No packages. The images carry the tools pillar-csi runs: the agent image has the OpenZFS userland and `lvm2`, and the node image has `util-linux`, `e2fsprogs` and `xfsprogs`. The node plugin connects through the kernel's `/dev/nvme-fabrics` device, so workers do not need `nvme-cli`. The agent writes the target through configfs, so the storage node does not need `nvmetcli` or `targetcli`, and the controller reaches the agent over gRPC, so no host needs SSH access.

The hosts must provide what a container cannot: the NVMe-oF kernel modules, and on the storage node the pool itself (a ZFS pool imported by the host's ZFS kernel module, or an LVM volume group). You also list each pool in the chart's `agent.backends` value, which is empty by default. At run time the agent and node plugin keep small state files in hostPath directories under `/var/lib/pillar-csi/`; that is data, not installed software. See [prerequisites](/docs/reference/prerequisites/).

## What are the kernel requirements?

Storage nodes need the `nvmet` and `nvmet_tcp` modules; workers need `nvme_fabrics` and `nvme_tcp`. The chart's init containers only run `modprobe` against the host's `/lib/modules`, so the modules must already be built for the running kernel. Some kernels leave them out or ship them in a separate package, and then you need a kernel or module package that provides them. Check with `modprobe nvmet_tcp` on the storage node and `modprobe nvme_tcp` on each worker before you install. The CPU architecture does not matter as long as the kernel has these modules and the images are published for it.

## Does it support ReadWriteMany (RWX)?

Not yet. pillar-csi exports block devices, and an ext4 or xfs filesystem on a block device cannot be mounted read-write by two nodes at once. It supports `ReadWriteOnce`, `ReadWriteOncePod` and `ReadOnlyMany`, and it rejects RWX requests. For shared read-write storage, use an NFS-based driver.

## Does it support snapshots or clones?

Not yet. The controller does not implement the CSI snapshot calls. You can still take ZFS or LVM snapshots on the storage node yourself, but Kubernetes `VolumeSnapshot` objects will not work with pillar-csi.

## Can I grow a volume?

Yes. Edit the PVC's `spec.resources.requests.storage`. The controller grows the zvol or logical volume, and the node plugin grows the filesystem. See [expand a volume](/docs/how-to/expand-volume/).

## Can I use more than one storage node or pool?

Yes. Create one `PillarAgent` per storage node and one `PillarStore` per pool, then a `PillarStorageClass` for each store you want to offer. All of them run under the same Helm release. Each pool must also be listed in `agent.backends` in the chart values, which is empty by default.

## Will iSCSI, NFS or SMB need another driver?

No. They are planned as additions to the same driver and the same configuration model: a new member of `PillarProtocol.spec.protocol`, set with the same YAML shape in storage class overrides and PVC annotations. They are not implemented yet, and there are no release dates. Today the CRDs accept only the `nvmeofTcp` protocol. [Architecture](/docs/explanation/architecture/#one-driver-for-every-backend-and-protocol) explains the model.

## How does democratic-csi compare?

Both can export ZFS zvols over NVMe-oF. democratic-csi manages a ZFS-on-Linux host by running commands over SSH and expects you to set up `nvmetcli` and the target ports by hand. pillar-csi runs a gRPC agent on the storage node that writes the kernel target configuration directly and reads it back. It adds LVM, fencing of stale requests and stable namespace identity across reboots, and it serves any number of pools from one Helm release. democratic-csi covers far more: TrueNAS and Synology appliances, NFS, iSCSI and SMB, snapshots and RWX. The [comparison page](/docs/explanation/comparison/#democratic-csi) has details and sources.

## What happens when the storage node reboots?

Its volumes are unavailable until it comes back. Workers keep retrying their connections in the meantime. The reboot clears the kernel's NVMe-oF target configuration, so when the agent starts, the controller sends it the full list of exports for that node and the agent rebuilds all of them before it opens any listener. Each namespace returns with the same identifiers it had before, so the workers' kernels accept it and I/O resumes on the existing mounts.

Workers retry only for the kernel's `ctrl_loss_tmo` period: 600 seconds by default, or the `ctrlLossTmo` value in the `PillarProtocol`. If the node is down longer than that, the workers drop the connection and the filesystems on those volumes see I/O errors. Plan longer outages with [node maintenance](/docs/how-to/node-maintenance/). [Architecture](/docs/explanation/architecture/#after-a-storage-node-reboot) describes the recovery in more detail.

## Is the traffic encrypted?

The controller-to-agent gRPC channel can use mTLS, but it is off by default; see [configure mTLS](/docs/how-to/configure-mtls/). pillar-csi does not configure TLS for NVMe/TCP, so volume data crosses the network unencrypted. Keep storage traffic on a network you trust, and set `acl: true` on the `PillarProtocol` so the target admits only the hosts that volumes are published to.

## Which filesystems does it support?

pillar-csi formats new volumes with ext4 (the default) or xfs and mounts them. It never reformats a volume that already has a filesystem. With `volumeMode: Block`, the pod gets the raw NVMe device and no filesystem is created.
