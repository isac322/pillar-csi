---
title: Comparison with other drivers
description: "pillar-csi compared with democratic-csi, Longhorn, OpenEBS LocalPV ZFS and LVM, and TopoLVM, including the cases where one of them is the better choice for you."
sidebar:
  order: 3
---

Several open-source drivers put ZFS or LVM storage behind Kubernetes volumes. They answer different questions. Local drivers keep each volume on the node that holds the disk. Longhorn replicates volumes across nodes. democratic-csi and pillar-csi export volumes from a storage host to other nodes over the network.

Checked on 2026-09-28 against each project's own documentation, linked below. Other projects change; check their current docs before you decide.

## At a glance

| | pillar-csi | democratic-csi | Longhorn | OpenEBS LocalPV ZFS / LVM | TopoLVM |
| --- | --- | --- | --- | --- | --- |
| Pods on other nodes can use a volume | Yes, over NVMe/TCP | Yes, over NFS, iSCSI, SMB or NVMe-oF | Yes | No, node-bound | No, node-local |
| Replication across nodes | No | Not provided by the driver | Yes, synchronous replicas | No | No |
| Storage it uses | An existing ZFS pool or LVM volume group on a storage node | ZFS on TrueNAS or any ZFS-on-Linux host, plus Synology and others | Disks on cluster nodes, managed by Longhorn | ZFS pool or LVM volume group on each node | LVM volume group on each node |
| How the driver manages the storage host | gRPC agent that writes kernel `nvmet` configfs directly | SSH commands, or the TrueNAS API (experimental) | Longhorn manager on each node | Node plugin on the same node | Node plugin on the same node |
| Snapshots and clones | Not supported yet | Yes | Yes, plus backups to NFS or S3 | ZFS: snapshot, restore, clone. LVM: snapshot, restore | Snapshots of thin volumes, restored on the same node |
| RWX (ReadWriteMany) | Not supported yet | Yes, with the NFS and SMB drivers | Yes, through an NFSv4 share-manager pod | No | Not listed in scope |
| Drivers to install for several pools and protocols | One driver and one Helm release. iSCSI, NFS and SMB are planned inside the same driver | One Helm release per driver, such as one for `zfs-generic-nvmeof` and another for `zfs-generic-nfs` | One installation | Separate drivers for ZFS and LVM | One driver, LVM only |
| Host packages required | None beyond kernel modules and the pool: the images carry `zfs`, `lvm2` and mkfs tools, and there is no SSH or target CLI | On a ZFS-on-Linux storage host: SSH, `zfs`, and `targetcli` or `nvmetcli` for iSCSI or NVMe-oF. On nodes: `nfs-common`, `cifs-utils` or `open-iscsi` for those protocols | `open-iscsi` with `iscsid` running, an NFSv4 client for RWX, and basic tools such as `findmnt`, `blkid` and `lsblk` on every node | ZFS: `zfsutils-linux` on every node. LVM: `lvm2` and the `dm-snapshot` module on every node | A volume group on each node; getting-started lists nothing else |

Sources: [democratic-csi README](https://github.com/democratic-csi/democratic-csi), [What is Longhorn](https://longhorn.io/docs/1.12.1/what-is-longhorn/), [Longhorn RWX volumes](https://longhorn.io/docs/1.12.1/nodes-and-volumes/volumes/rwx-volumes/), [Longhorn installation requirements](https://longhorn.io/docs/1.12.1/deploy/install/), [OpenEBS LocalPV ZFS](https://github.com/openebs/zfs-localpv) and its [quickstart](https://github.com/openebs/zfs-localpv/blob/develop/docs/quickstart.md), [OpenEBS LocalPV LVM](https://github.com/openebs/lvm-localpv) and its [quickstart](https://github.com/openebs/lvm-localpv/blob/develop/docs/quickstart.md), [TopoLVM README](https://github.com/topolvm/topolvm), [TopoLVM getting started](https://github.com/topolvm/topolvm/blob/main/docs/getting-started.md), [TopoLVM limitations](https://github.com/topolvm/topolvm/blob/main/docs/limitations.md). pillar-csi facts come from this repository at v0.3.0 (`Dockerfile`, `api/v1alpha1/`).

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

The [support matrix](/docs/reference/support-matrix/) lists what v0.3.0 supports.
