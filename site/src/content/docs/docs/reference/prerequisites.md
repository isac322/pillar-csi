---
title: Prerequisites
description: "Host and cluster requirements for pillar-csi: NVMe-oF kernel modules per node role, Kubernetes and Helm versions, filesystems, kubelet paths, and ports."
sidebar:
  order: 1
---

pillar-csi runs its data path in the Linux kernel: the storage node exports volumes with the kernel NVMe-oF target, and the worker connects with the kernel NVMe-oF/TCP initiator. The container images carry the userspace tools the driver runs. The host provides the kernel modules and the storage pool, because a container cannot supply either.

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
| `nvme_fabrics` | Worker nodes | `CONFIG_NVME_FABRICS` | Fabrics layer and `/dev/nvme-fabrics` |
| `nvme_tcp` | Worker nodes | `CONFIG_NVME_TCP` | NVMe-oF/TCP initiator |

The agent and node Pods each start with an init container that runs `modprobe` against the host's `/lib/modules`. By default it loads `nvmet` and `nvmet_tcp` on storage nodes and `nvme_fabrics` and `nvme_tcp` on worker nodes. The init container ignores `modprobe` failures, so the Pod starts even when a module is missing. Load the modules on the host and list them in `/etc/modules-load.d/` so they return after a reboot. The [ZFS](/docs/how-to/prepare-zfs-node/) and [LVM](/docs/how-to/prepare-lvm-node/) node guides show how. To change the lists, set `agent.initModprobe.modules` and `node.initModprobe.modules`.

Check a storage node:

```sh
sudo modprobe -a nvmet nvmet_tcp && ls /sys/kernel/config/nvmet
```

Check a worker node:

```sh
sudo modprobe -a nvme_fabrics nvme_tcp && ls -l /dev/nvme-fabrics
```

On Ubuntu, `nvmet` and `nvmet_tcp` ship in the `linux-modules-extra-$(uname -r)` package.

### Vendor kernels

Many vendor kernels, including some built for single-board computers, leave out NVMe-oF target support. Run the storage node check above before you choose hardware for a storage node. If it fails, you need a kernel built with `CONFIG_NVME_TARGET` and `CONFIG_NVME_TARGET_TCP`.

## What the images carry and what the host provides

| Need | Carried in the image | Provided by the host |
| --- | --- | --- |
| ZFS volume management | `zfs` and `zpool` from OpenZFS 2.4, in the agent image | The `zfs` kernel module, `/dev/zfs`, and an existing pool |
| LVM volume management | `lvm2` tools, in the agent image | An existing volume group, the `dm_thin_pool` module and a thin pool if you use thin volumes |
| NVMe-oF target setup | The agent writes `/sys/kernel/config/nvmet` itself, with no `nvmetcli` or `targetcli` | The `nvmet` and `nvmet_tcp` modules |
| NVMe-oF connect | The node plugin writes `/dev/nvme-fabrics` itself, with no `nvme-cli` | The `nvme_fabrics` and `nvme_tcp` modules |
| Formatting, mounting, resizing | `util-linux`, `e2fsprogs`, and `xfsprogs`, in the node image | Nothing |
| Controller to agent traffic | gRPC from the controller to each agent, with no SSH | Nothing |

On the host you install no pillar-csi packages. You need the OpenZFS or LVM tools only to create the pool or volume group in the first place.

The node plugin reads the NVMe host NQN from `/etc/nvme/hostnqn` and the host ID from `/etc/nvme/hostid`. It generates and writes either file if it is missing or empty.

The agent runs privileged by default (`agent.privileged: true`) because it opens host device nodes such as `/dev/zfs`, `/dev/mapper/control`, and the physical volumes.

## Kubernetes and Helm

| Component | Requirement |
| --- | --- |
| Kubernetes | 1.24 or later (chart `kubeVersion: >=1.24.0-0`) |
| Helm | 3.8 or later, for OCI chart support |
| kubelet root directory | `/var/lib/kubelet`. The node DaemonSet mounts `/var/lib/kubelet/pods` and `/var/lib/kubelet/plugins/kubernetes.io/csi` at fixed paths. |
| Node DaemonSets | `hostNetwork: true` for both agent and node Pods, so the kernel target and initiator use the host network namespace |

The end-to-end test scripts in the repository run on Kind with Kubernetes 1.37.

## Filesystems and volume modes

| Setting | Values |
| --- | --- |
| `fsType` | `ext4` (default) or `xfs` |
| Volume modes | `Filesystem` and `Block` |

Set `fsType`, `mkfsOptions`, and `mountOptions` under `spec.filesystem` of a `PillarStorageClass`, or per volume with the `pillar-csi.bhyoo.com/filesystem` PVC annotation. [Support matrix](/docs/reference/support-matrix/) lists access modes and CSI features.

## Network ports

| Port | Protocol | Listener | Clients | Configured by |
| --- | --- | --- | --- | --- |
| 9500 | TCP (gRPC) | `pillar-agent` on each storage node, bound on the host network | `pillar-controller` Pods | `agent.grpcPort`, `agent.hostPort`; per agent with `PillarAgent.spec.nodeRef.port` |
| 4420 | TCP (NVMe-oF) | Kernel target on each storage node | Worker nodes | `PillarProtocol.spec.protocol.nvmeofTcp.port` |
| 9808 | TCP (HTTP) | Node plugin liveness probe, bound on each node's host network | kubelet | `node.livenessPort` |
| 9443 | TCP (HTTPS) | Admission webhook in the controller Pod, behind a Service on port 443 | Kubernetes API server | `webhook.port` |
| 8080 | TCP (HTTP) | Controller metrics, Pod network | Prometheus | `controller.metricsPort` |
| 8081 | TCP (HTTP) | Controller health and readiness, Pod network | kubelet | `controller.healthProbePort` |
| 9809 | TCP (HTTP) | Controller CSI liveness probe, Pod network | kubelet | `controller.livenessPort` |

The controller reaches each agent at the node's `InternalIP` by default; `PillarAgent.spec.nodeRef.addressType` selects `ExternalIP` instead. Firewalls between nodes must allow 9500 from the controller's Pods to storage nodes and the NVMe-oF port from worker nodes to storage nodes.

Port 9808 is a common default for CSI liveness probes. If another CSI driver's node plugin already uses host port 9808, set `node.livenessPort` to a free port.

Controller-to-agent gRPC is plaintext unless you enable mTLS. See [Configure mTLS](/docs/how-to/configure-mtls/).
