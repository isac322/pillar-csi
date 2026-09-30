---
title: Prerequisites
description: "Host and cluster requirements for pillar-csi: NVMe-oF and iSCSI kernel modules per node role, Kubernetes and Helm versions, filesystems, kubelet paths, and ports."
sidebar:
  order: 1
---

pillar-csi runs its data path in the Linux kernel: the storage node exports volumes with the kernel NVMe-oF target or the kernel LIO iSCSI target, and the worker connects with the kernel NVMe-oF/TCP or iSCSI initiator. The container images carry the userspace tools the driver runs. The host provides the kernel modules and the storage pool, because a container cannot supply either.

## Node roles

| Role | Runs | Scheduled on |
| --- | --- | --- |
| Storage node | `pillar-agent` DaemonSet | Nodes labelled `pillar-csi.bhyoo.com/agent-node=true`. The controller sets the label for each `PillarAgent` with a `nodeRef`. |
| Worker node | `pillar-node` DaemonSet | Every schedulable node, unless you set `node.nodeSelector` |
| Any node | `pillar-controller` Deployment | Wherever the scheduler places it |

A storage node that also runs Pods using pillar-csi volumes needs the worker requirements too.

## Kernel modules

| Module | Needed on | Kernel option | Purpose |
| --- | --- | --- | --- |
| `nvmet` | Storage nodes | `CONFIG_NVME_TARGET` | NVMe-oF target core and its configfs tree at `/sys/kernel/config/nvmet` |
| `nvmet_tcp` | Storage nodes | `CONFIG_NVME_TARGET_TCP` | TCP transport for the target |
| `zfs` | ZFS storage nodes | OpenZFS, built out of tree | zvols and `/dev/zfs` |
| `dm_thin_pool` | LVM storage nodes with a thin pool | `CONFIG_DM_THIN_PROVISIONING` | Thin logical volumes |
| `dm_mod` | Storage nodes, for [local attach](/docs/how-to/local-attach/) | `CONFIG_BLK_DEV_DM` | Device-mapper target that holds the backend device while a pod on the storage node uses it directly |
| `nvme_fabrics` | Worker nodes | `CONFIG_NVME_FABRICS` | Fabrics layer and `/dev/nvme-fabrics` |
| `nvme_tcp` | Worker nodes | `CONFIG_NVME_TCP` | NVMe-oF/TCP initiator |
| `target_core_mod` | Storage nodes, for iSCSI | `CONFIG_TARGET_CORE` | LIO target core and its configfs tree at `/sys/kernel/config/target` |
| `target_core_iblock` | Storage nodes, for iSCSI | `CONFIG_TCM_IBLOCK` | LIO backstore for block devices such as zvols and logical volumes |
| `iscsi_target_mod` | Storage nodes, for iSCSI | `CONFIG_ISCSI_TARGET` | LIO iSCSI target, at `/sys/kernel/config/target/iscsi` |
| `iscsi_tcp` | Worker nodes, for iSCSI | `CONFIG_ISCSI_TCP` | iSCSI/TCP initiator transport. It pulls in `libiscsi`, `libiscsi_tcp` and `scsi_transport_iscsi` |

You need the NVMe-oF modules only for NVMe-oF volumes and the iSCSI modules only for iSCSI volumes.

The agent and node Pods each start with an init container that runs `modprobe` against the host's `/lib/modules`. By default it loads `nvmet`, `nvmet_tcp`, `target_core_mod`, `target_core_iblock` and `iscsi_target_mod` on storage nodes, and `nvme_fabrics`, `nvme_tcp`, `dm_mod` and `iscsi_tcp` on every node the node plugin runs on. The init container ignores `modprobe` failures, so the Pod starts even when a module is missing. Load the modules on the host and list them in `/etc/modules-load.d/` so they return after a reboot. The [ZFS](/docs/how-to/prepare-zfs-node/) and [LVM](/docs/how-to/prepare-lvm-node/) node guides show how. To change the lists, set `agent.initModprobe.modules` and `node.initModprobe.modules`.

pillar-node checks for `iscsi_tcp` once, when it starts. If the module is not loaded then, pillar-node disables iSCSI until it restarts: it logs `iSCSI initiator disabled: kernel module iscsi_tcp is not loaded`, it does not publish an initiator IQN, and iSCSI volumes fail to stage on that node. Load the module, then restart the pillar-node Pod.

Check a storage node:

```sh
sudo modprobe -a nvmet nvmet_tcp && ls /sys/kernel/config/nvmet
```

Check a worker node:

```sh
sudo modprobe -a nvme_fabrics nvme_tcp && ls -l /dev/nvme-fabrics
```

For iSCSI, check a storage node:

```sh
sudo modprobe -a target_core_mod target_core_iblock iscsi_target_mod && ls /sys/kernel/config/target
```

And a worker node:

```sh
sudo modprobe iscsi_tcp && ls /sys/class/iscsi_transport/tcp
```

On Ubuntu, `nvmet` and `nvmet_tcp` ship in the `linux-modules-extra-$(uname -r)` package.

### Vendor kernels

Many vendor kernels, including some built for single-board computers, leave out NVMe-oF target support. Run the storage node check above before you choose hardware for a storage node. If it fails, you need a kernel built with `CONFIG_NVME_TARGET` and `CONFIG_NVME_TARGET_TCP`. For iSCSI, the kernel needs `CONFIG_TARGET_CORE`, `CONFIG_TCM_IBLOCK` and `CONFIG_ISCSI_TARGET` on the storage node and `CONFIG_ISCSI_TCP` on the workers.

## What the images carry and what the host provides

| Need | Carried in the image | Provided by the host |
| --- | --- | --- |
| ZFS volume management | `zfs` and `zpool` from OpenZFS 2.4, in the agent image | The `zfs` kernel module, `/dev/zfs`, and an existing pool |
| LVM volume management | `lvm2` tools, in the agent image | An existing volume group, the `dm_thin_pool` module and a thin pool if you use thin volumes |
| NVMe-oF target setup | The agent writes `/sys/kernel/config/nvmet` itself, with no `nvmetcli` or `targetcli` | The `nvmet` and `nvmet_tcp` modules |
| NVMe-oF connect | The node plugin writes `/dev/nvme-fabrics` itself, with no `nvme-cli` | The `nvme_fabrics` and `nvme_tcp` modules |
| iSCSI target setup | The agent writes `/sys/kernel/config/target` itself, with no `targetcli` | The `target_core_mod`, `target_core_iblock` and `iscsi_target_mod` modules |
| iSCSI login | The node plugin logs in with its own initiator and hands the connection to the kernel, with no `iscsiadm`, `iscsid` or `open-iscsi` | The `iscsi_tcp` module |
| Local attach on the storage node | `dmsetup`, in the node image | The `dm_mod` module |
| Formatting, mounting, resizing | `util-linux`, `e2fsprogs`, and `xfsprogs`, in the node image | Nothing |
| Controller to agent traffic | gRPC from the controller to each agent, with no SSH | Nothing |

On the host you install no pillar-csi packages. You need the OpenZFS or LVM tools only to create the pool or volume group in the first place.

The node plugin reads the NVMe host NQN from `/etc/nvme/hostnqn` and the host ID from `/etc/nvme/hostid`. It generates and writes either file if it is missing or empty.

The node plugin reads the iSCSI initiator IQN from the `InitiatorName=` line of `/etc/iscsi/initiatorname.iscsi`, which it mounts from the host with a `DirectoryOrCreate` hostPath. If the file is missing, it generates an IQN of the form `iqn.2026-01.com.bhyoo.pillar-csi:node.<32 hex digits>` and writes it there. Uninstalling pillar-csi leaves the file in place. If the host also runs `open-iscsi` and `iscsid`, the node plugin uses the same IQN and manages only the sessions to pillar-csi targets.

The agent runs privileged by default (`agent.privileged: true`) because it opens host device nodes such as `/dev/zfs`, `/dev/mapper/control`, and the physical volumes.

## Kubernetes and Helm

| Component | Requirement |
| --- | --- |
| Kubernetes | 1.24 or later (chart `kubeVersion: >=1.24.0-0`) |
| Helm | 3.8 or later, for OCI chart support |
| kubelet root directory | `/var/lib/kubelet`. The node DaemonSet mounts `/var/lib/kubelet/pods` and `/var/lib/kubelet/plugins/kubernetes.io/csi` at fixed paths. |
| Node DaemonSets | `hostNetwork: true` for both agent and node Pods, so the kernel target and initiator use the host network namespace. iSCSI also needs it on the node Pod, because the kernel's `NETLINK_ISCSI` socket exists only in the host's initial network namespace. |

On Kind and other clusters whose nodes are containers, the node container's network namespace is not the host's initial one. Set `node.iscsi.netlinkNetnsPath` to a host init network namespace file visible in the node, for example `/host/proc/1/ns/net` when the host's `/proc` is mounted at `/host/proc`. Production nodes with `hostNetwork: true` leave it empty.

The end-to-end test scripts in the repository run on Kind with Kubernetes 1.37.

## Filesystems and volume modes

| Setting | Values |
| --- | --- |
| `fsType` | `ext4` (default) or `xfs` |
| Volume modes | `Filesystem` and `Block` |
| Oldest node kernel for `Filesystem` volumes | Linux 5.15 |

Set `fsType`, `mkfsOptions`, and `mountOptions` under `spec.filesystem` of a `PillarStorageClass`, or per volume with the `pillar-csi.bhyoo.com/filesystem` PVC annotation. [Support matrix](/docs/reference/support-matrix/) lists access modes and CSI features.

The node formats a new volume with only the on-disk features that Linux 5.15 can mount, because the volume can later be mounted by any worker. A node with an older kernel may be unable to mount it. [Filesystem compatibility](/docs/reference/support-matrix/#filesystem-compatibility) lists the features and how to opt in to newer ones.

## Network ports

| Port | Protocol | Listener | Clients | Configured by |
| --- | --- | --- | --- | --- |
| 9500 | TCP (gRPC) | `pillar-agent` on each storage node, bound on the host network | `pillar-controller` Pods | `agent.grpcPort`, `agent.hostPort`; per agent with `PillarAgent.spec.nodeRef.port` |
| 4420 | TCP (NVMe-oF) | Kernel target on each storage node | Worker nodes | `PillarProtocol.spec.protocol.nvmeofTcp.port` |
| 3260 | TCP (iSCSI) | Kernel LIO target on each storage node | Worker nodes | `PillarProtocol.spec.protocol.iscsi.port` |
| 9808 | TCP (HTTP) | Node plugin liveness probe, bound on each node's host network | kubelet | `node.livenessPort` |
| 9443 | TCP (HTTPS) | Admission webhook in the controller Pod, behind a Service on port 443 | Kubernetes API server | `webhook.port` |
| 8080 | TCP (HTTP) | Controller metrics, Pod network | Prometheus | `controller.metricsPort` |
| 8081 | TCP (HTTP) | Controller health and readiness, Pod network | kubelet | `controller.healthProbePort` |
| 9809 | TCP (HTTP) | Controller CSI liveness probe, Pod network | kubelet | `controller.livenessPort` |

The controller reaches each agent at the node's `InternalIP` by default; `PillarAgent.spec.nodeRef.addressType` selects `ExternalIP` instead. Firewalls between nodes must allow 9500 from the controller's Pods to storage nodes, and the NVMe-oF or iSCSI port from worker nodes to storage nodes.

Port 9808 is a common default for CSI liveness probes. If another CSI driver's node plugin already uses host port 9808, set `node.livenessPort` to a free port.

Controller-to-agent gRPC is plaintext unless you enable mTLS. See [Configure mTLS](/docs/how-to/configure-mtls/).
