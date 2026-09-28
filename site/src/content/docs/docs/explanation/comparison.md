---
title: Comparison with other drivers
description: "pillar-csi compared with democratic-csi, Longhorn, OpenEBS LocalPV ZFS and LVM, and TopoLVM, including the cases where one of them is the better choice for you."
sidebar:
  order: 3
---

Several open-source drivers put ZFS or LVM storage behind Kubernetes volumes. They answer different questions. Local drivers keep each volume on the node that holds the disk. Longhorn replicates volumes across nodes. democratic-csi and pillar-csi export volumes from a storage host to other nodes over the network.

Checked on 2026-09-28 against each project's own documentation, linked below. Other projects change; check their current docs before you decide.

## At a glance

### Features

| Driver | Serves pods on other nodes | Replicas | Snapshots | RWX |
| --- | --- | --- | --- | --- |
| pillar-csi | Yes, NVMe-oF/TCP | No | Not yet | Not yet |
| democratic-csi | Yes, NFS, iSCSI, SMB or NVMe-oF | No | Yes, plus clones | Yes, NFS and SMB drivers |
| Longhorn | Yes | Yes, synchronous | Yes, plus backups to NFS or S3 | Yes, NFSv4 share-manager pod |
| OpenEBS LocalPV | No, node-bound | No | ZFS: yes, plus clones. LVM: yes | No |
| TopoLVM | No, node-local | No | Thin volumes, same-node restore | Not in scope |

### Storage and management

| Driver | Storage it uses | How it manages the storage |
| --- | --- | --- |
| pillar-csi | An existing ZFS pool or LVM volume group on a storage node | A gRPC agent that writes the kernel NVMe-oF target through configfs |
| democratic-csi | TrueNAS, any ZFS-on-Linux host, Synology and others | SSH commands, or the TrueNAS API (experimental) |
| Longhorn | Disks on cluster nodes, managed by Longhorn | Longhorn manager on each node |
| OpenEBS LocalPV | A ZFS pool or LVM volume group on each node | Node plugin on the same node |
| TopoLVM | An LVM volume group on each node | Node plugin on the same node |

### What you install

| Driver | Drivers for several pools and protocols | Host packages |
| --- | --- | --- |
| pillar-csi | One driver, one Helm release. iSCSI, NFS and SMB are planned in the same driver | None. Hosts provide the kernel modules and the pool; the images carry `zfs`, `lvm2` and the mkfs tools |
| democratic-csi | One Helm release per driver, for example one for NVMe-oF and one for NFS | Storage host: SSH, `zfs`, and `targetcli` or `nvmetcli`. Nodes: `nfs-common`, `cifs-utils` or `open-iscsi` for those protocols |
| Longhorn | One installation | Every node: `open-iscsi` with `iscsid`, an NFSv4 client for RWX, and tools such as `findmnt`, `blkid` and `lsblk` |
| OpenEBS LocalPV | Separate drivers for ZFS and LVM | ZFS: `zfsutils-linux` on every node. LVM: `lvm2` and the `dm-snapshot` module on every node |
| TopoLVM | One driver, LVM only | A volume group on each node; the getting-started guide lists nothing else |

Sources: [democratic-csi README](https://github.com/democratic-csi/democratic-csi), [What is Longhorn](https://longhorn.io/docs/1.12.1/what-is-longhorn/), [Longhorn RWX volumes](https://longhorn.io/docs/1.12.1/nodes-and-volumes/volumes/rwx-volumes/), [Longhorn installation requirements](https://longhorn.io/docs/1.12.1/deploy/install/), [OpenEBS LocalPV ZFS](https://github.com/openebs/zfs-localpv) and its [quickstart](https://github.com/openebs/zfs-localpv/blob/develop/docs/quickstart.md), [OpenEBS LocalPV LVM](https://github.com/openebs/lvm-localpv) and its [quickstart](https://github.com/openebs/lvm-localpv/blob/develop/docs/quickstart.md), [TopoLVM README](https://github.com/topolvm/topolvm), [TopoLVM getting started](https://github.com/topolvm/topolvm/blob/main/docs/getting-started.md), [TopoLVM limitations](https://github.com/topolvm/topolvm/blob/main/docs/limitations.md). pillar-csi facts come from the pillar-csi repository at v0.3.1.

## democratic-csi

democratic-csi is the closest match. Its `zfs-generic-nvmeof` driver also exports ZFS zvols over NVMe-oF from an ordinary Linux host, and it has far more drivers: TrueNAS, Synology, NFS, iSCSI, SMB, Lustre and node-local ZFS among them. It supports resizing, snapshots and clones.

The two differ in how they manage the storage host. For ZFS-on-Linux hosts, democratic-csi "executes many commands over an ssh connection", and its NVMe-oF setup asks you to install `nvmetcli` and a systemd unit and create the target ports by hand. pillar-csi runs its own agent on the storage node. The agent carries the `zfs` and `lvm2` tools in its image, creates the volumes, writes the kernel target configuration directly and reads it back, so the storage host needs no SSH access and no target CLI. The agent also fences stale requests and keeps namespace identity stable across reboots (see [fencing and consistency](/docs/explanation/fencing-and-consistency/)).

They also differ in how many drivers you run. Each democratic-csi deployment runs one driver type, and its README asks for a new Helm release and a unique driver name for each additional deployment. pillar-csi is one driver: you add a pool by listing it in `agent.backends` and creating a `PillarStore`, and every backend and protocol uses the same configuration shape. That model covers only ZFS, LVM and NVMe-oF/TCP today; iSCSI, NFS and SMB are planned for the same driver.

Choose democratic-csi if your storage is a TrueNAS or Synology appliance, if you need NFS, SMB or iSCSI, or if you need snapshots or RWX today.

## Longhorn

Longhorn is distributed block storage. It keeps synchronous replicas of each volume on several nodes, so a volume survives the loss of one node, and it adds scheduled snapshots, backups to NFS or S3, and RWX through an NFSv4 share-manager pod.

pillar-csi has no replication, snapshots, backups or RWX. It exports one copy of each volume from one storage node, and the volume is unavailable while that node is down. It also adds no storage engine of its own: the data sits in your existing ZFS pool or LVM volume group, and the I/O path runs through kernel code only.

Choose Longhorn if you need a volume to survive a node failure, or if you have no dedicated storage node and want to pool disks across the cluster.

## OpenEBS LocalPV ZFS and LVM

The OpenEBS LocalPV engines provision ZFS or LVM volumes on the node where the disks are, and the pod must run on that node. The README states it plainly: "The volume is tied to the node where the disk is physically located" and "No replication". The ZFS engine supports snapshots, restore and clone; the LVM engine supports snapshots and restore.

pillar-csi uses the same kind of storage but serves it over the network, so a pod on any worker can use a volume from the storage node. That matters when your disks sit in one box and your workloads run elsewhere.

Choose LocalPV if your pods can run on the node that holds the disks. A local volume has no network hop and does not need NVMe-oF kernel modules.

## TopoLVM

TopoLVM is a node-local LVM driver with capacity-aware scheduling: it steers pods to nodes that have enough free space in their volume group. Snapshots work for thin volumes and restore on the same node.

Choose TopoLVM if every node has its own disks and you want Kubernetes to place pods by free capacity. Choose pillar-csi if the capacity lives on one storage node and the pods run on other nodes.

## When not to choose pillar-csi

pillar-csi fits a cluster where one or a few machines hold the disks in ZFS or LVM and other nodes run the workloads. It is the wrong choice if you need any of the following today:

- Replication or high availability. Each volume lives on one storage node, and it is unavailable while that node is down.
- ReadWriteMany volumes. pillar-csi supports RWO, RWOP and ROX, and rejects RWX.
- Snapshots or clones.
- NFS, iSCSI or SMB. The only protocol is NVMe-oF over TCP, and it needs the `nvmet` and `nvmet_tcp` modules on the storage node and `nvme_tcp` and `nvme_fabrics` on the workers.
- A storage host outside the cluster. The chart runs the agent as a DaemonSet, so the storage node must be a Kubernetes node.
- Backends other than ZFS zvols and LVM logical volumes.

The [support matrix](/docs/reference/support-matrix/) lists what v0.3.1 supports.
