---
title: Prerequisites
description: "Host and cluster requirements for pillar-csi: kernel support for NVMe-oF, iSCSI or NFS, Kubernetes and Helm versions, file systems, kubelet paths, and ports."
sidebar:
  order: 1
---

pillar-csi runs its data path in the Linux kernel: storage nodes export block volumes with the kernel NVMe-oF or LIO iSCSI target, or serve ZFS datasets with the kernel NFS server; workers connect with the matching initiator or NFS client. Container images carry the user-space tools the driver runs. Hosts provide kernel support and the storage pool, because a container cannot supply either.

## Node roles

| Role | Runs | Scheduled on |
| --- | --- | --- |
| Storage node | `pillar-agent` DaemonSet | Nodes labelled `pillar-csi.bhyoo.com/agent-node=true`. The controller sets the label for each `PillarAgent` with a `nodeRef`. |
| Worker node | `pillar-node` DaemonSet | Every schedulable node, unless you set `node.nodeSelector` |
| Any node | `pillar-controller` Deployment | Wherever the scheduler places it |

A storage node that also runs Pods using pillar-csi volumes needs the worker requirements too.

## Kernel modules

| Module/support | Needed on | Kernel option | Purpose |
| --- | --- | --- | --- |
| `nvmet`, `nvmet_tcp` | Storage nodes using NVMe-oF | `CONFIG_NVME_TARGET`, `CONFIG_NVME_TARGET_TCP` | NVMe-oF target and TCP transport |
| `zfs` | ZFS storage nodes | OpenZFS, built out of tree | zvols, datasets and `/dev/zfs` |
| `nfsd`/NFS server support | Storage nodes using NFS | Kernel NFS server support | NFSv4.2 server for ZFS datasets |
| `dm_thin_pool` | LVM storage nodes with a thin pool | `CONFIG_DM_THIN_PROVISIONING` | Thin logical volumes |
| `dm_mod` | Storage nodes, for block local attach | `CONFIG_BLK_DEV_DM` | Device-mapper claim for a local block volume |
| `nvme_fabrics`, `nvme_tcp` | Worker nodes using NVMe-oF | `CONFIG_NVME_FABRICS`, `CONFIG_NVME_TCP` | NVMe-oF/TCP initiator |
| `target_core_mod`, `target_core_iblock`, `iscsi_target_mod` | Storage nodes using iSCSI | `CONFIG_TARGET_CORE`, `CONFIG_TCM_IBLOCK`, `CONFIG_ISCSI_TARGET` | LIO iSCSI target |
| `iscsi_tcp` | Worker nodes using iSCSI | `CONFIG_ISCSI_TCP` | iSCSI/TCP initiator |
| NFS client support | Worker nodes using NFS | Kernel NFS client support | NFSv4.2 mounts |

You need the NVMe-oF modules only for NVMe-oF volumes, iSCSI modules only for iSCSI volumes, and NFS server/client support only for NFS volumes.

The agent and node Pods each start with an init container that runs `modprobe` against the host's `/lib/modules`. It loads the modules required by the configured protocols when present; it cannot install missing modules, and failures remain visible in protocol capabilities or NodeStage errors. Load modules on the host and list them in `/etc/modules-load.d/` so they return after a reboot. To change the lists, set `agent.initModprobe.modules` and `node.initModprobe.modules`.

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

Many vendor kernels, including some built for single-board computers, leave out NVMe-oF target support or NFS server/client support. Run the storage node checks above before you choose hardware. If NVMe checks fail, use a kernel built with `CONFIG_NVME_TARGET` and `CONFIG_NVME_TARGET_TCP`; for iSCSI use `CONFIG_TARGET_CORE`, `CONFIG_TCM_IBLOCK` and `CONFIG_ISCSI_TARGET` on storage nodes plus `CONFIG_ISCSI_TCP` on workers; for NFS use kernel NFS server support on storage nodes and NFS client support on workers.

## What the images carry and what the host provides

| Need | Carried in the image | Provided by the host |
| --- | --- | --- |
| ZFS volume management | `zfs` and `zpool` from OpenZFS 2.4, in the agent image | The `zfs` kernel module, `/dev/zfs`, and an existing pool |
| LVM volume management | `lvm2` tools, in the agent image | An existing volume group, the `dm_thin_pool` module and a thin pool if you use thin volumes |
| NFS dataset/export management | Bundled export supervision helpers and private state paths | Kernel NFS server support, an existing ZFS dataset parent, and persistent NFS-exportable pseudoroot backing |
| NFS mounting | Bundled NFS client mount helper in the node image | Kernel NFS client support |
| NVMe-oF target/connect | Agent and node write configfs or `/dev/nvme-fabrics` directly; no `nvmetcli`, `targetcli` or `nvme-cli` | NVMe target and initiator modules |
| iSCSI target/login | Agent writes configfs; node uses its own initiator; no `targetcli`, `iscsiadm`, `iscsid` or `open-iscsi` | iSCSI target and initiator modules |
| Block local attach | `dmsetup`, in the node image | The `dm_mod` module |
| Formatting, mounting, resizing | `util-linux`, `e2fsprogs`, `xfsprogs`, and NFS mount utilities in the images | Nothing beyond protocol kernel support |
| Controller to agent traffic | gRPC from the controller to each agent, with no SSH | Nothing |

On the host you install no pillar-csi packages and no host NFS/iSCSI utilities. You need OpenZFS or LVM tools only to create the pool or volume group in the first place.

The node plugin reads the NVMe host NQN from `/etc/nvme/hostnqn` and the host ID from `/etc/nvme/hostid`. It generates and writes either file if it is missing or empty.

The node plugin reads the iSCSI initiator IQN from the `InitiatorName=` line of `/etc/iscsi/initiatorname.iscsi`, which it mounts from the host with a `DirectoryOrCreate` hostPath. If the file is missing, it generates an IQN of the form `iqn.2026-01.com.bhyoo.pillar-csi:node.<32 hex digits>` and writes it there. Uninstalling pillar-csi leaves the file in place. If the host also runs `open-iscsi` and `iscsid`, the node plugin uses the same IQN and manages only the sessions to pillar-csi targets.

The agent runs privileged by default (`agent.privileged: true`) because it opens host device nodes such as `/dev/zfs`, `/dev/mapper/control`, and the physical volumes.

### NFS persistent backing

Before deploying a ZFS dataset backend, provide persistent storage at the chart's fixed `agent-state` hostPath, `/var/lib/pillar-csi/agent`. The dedicated NFSv4 pseudoroot is `/var/lib/pillar-csi/agent/datasets`: it must sit on an NFS-exportable filesystem that can encode export filehandles. Do not leave it on container overlay/rootfs. A ZFS pool for volume datasets does not by itself provide backing for this separate pseudoroot.

Use these canonical paths without symlinks:

| Path | Purpose and requirement |
| --- | --- |
| `/var/lib/pillar-csi/agent` | Persistent host agent state; must survive agent and storage-node restarts |
| `/var/lib/pillar-csi/agent/datasets` | Dedicated NFS-exportable pseudoroot and parent of managed ZFS dataset mountpoints; persistent production backing |
| `/var/lib/pillar-csi/agent/nfs` | Private export/recovery state; `nfs/lib` is also mounted at the agent container's `/var/lib/nfs`, not the host's foreign NFS state |

Prepare the backing on the storage node before the agent starts, and preserve it across reboots. The chart enables bidirectional dataset mount propagation, host PID visibility for foreign `mountd` detection, privileged access, and host networking for dataset backends. These settings do not make an unexportable backing filesystem exportable. Do not use a PVC served by pillar-csi for agent state.

NFS server startup rejects a pseudoroot that cannot encode export filehandles and reports NFS as unavailable. Correct the backing before using the configured dataset/NFS deployment; it is not a supported optional or degraded storage layout. The agent does not create a `tmpfs` fallback.

The dedicated Kind NFS QA fixture mounts a temporary `tmpfs` at the pseudoroot to avoid the node container's overlay backing. That fixture is for ephemeral tests only: it does not meet production persistence requirements. The images still provide all NFS user-space helpers; no host NFS package installation is required.


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
| Block `fsType` | `ext4` (default) or `xfs` |
| NFS `fsType` | Omit; NFS is already a filesystem |
| Volume modes | `Filesystem` for NFS; `Filesystem` and `Block` for block protocols |
| NFS mount flags | `filesystem.mountOptions` only; defaults hard, NFSv4.2 and TCP cannot be contradicted |
| NFS restrictions | No `mkfsOptions`, explicit ext4/xfs, periodic trim, or `localAttach` |
| Oldest node kernel for block `Filesystem` volumes | Linux 5.15 |

Set filesystem options under `spec.filesystem` of a `PillarStorageClass`, or per volume with the `pillar-csi.bhyoo.com/filesystem` PVC annotation. [Support matrix](/docs/reference/support-matrix/) lists access modes and CSI features. NFS mount flags do not install or configure host packages.

The node formats a new block volume with only the on-disk features that Linux 5.15 can mount. NFS datasets are never formatted.

## Network ports

| Port | Protocol | Listener | Clients | Configured by |
| --- | --- | --- | --- | --- |
| 9500 | TCP (gRPC) | `pillar-agent` on each storage node, bound on the host network | `pillar-controller` Pods | `agent.grpcPort`, `agent.hostPort`; per agent with `PillarAgent.spec.nodeRef.port` |
| 4420 | TCP (NVMe-oF) | Kernel target on each storage node | Worker nodes | `PillarProtocol.spec.protocol.nvmeofTcp.port` |
| 2049 | TCP (NFSv4.2) | Kernel NFS server and owned export supervisor on each storage node | Worker nodes | Fixed; `PillarProtocol.spec.protocol.nfs.port` must be 2049 |
| 3260 | TCP (iSCSI) | Kernel LIO target on each storage node | Worker nodes | `PillarProtocol.spec.protocol.iscsi.port` |
| 9808 | TCP (HTTP) | Node plugin liveness probe, bound on each node's host network | kubelet | `node.livenessPort` |
| 9443 | TCP (HTTPS) | Admission webhook in the controller Pod, behind a Service on port 443 | Kubernetes API server | `webhook.port` |
| 8080 | TCP (HTTP) | Controller metrics, Pod network | Prometheus | `controller.metricsPort` |
| 8081 | TCP (HTTP) | Controller health and readiness, Pod network | kubelet | `controller.healthProbePort` |
| 9809 | TCP (HTTP) | Controller CSI liveness probe, Pod network | kubelet | `controller.livenessPort` |

The controller reaches each agent at the node's `InternalIP` by default; `PillarAgent.spec.nodeRef.addressType` selects `ExternalIP` instead. Firewalls between nodes must allow 9500 from the controller's Pods to storage nodes, and the protocol port from worker nodes to storage nodes: 4420 for NVMe-oF, 3260 for iSCSI, or 2049 for NFS. NFS RPC TLS is not offered; protect the network separately.

Port 9808 is a common default for CSI liveness probes. If another CSI driver's node plugin already uses host port 9808, set `node.livenessPort` to a free port.

Controller-to-agent gRPC is plaintext unless you enable mTLS. See [Configure mTLS](/docs/how-to/configure-mtls/).
