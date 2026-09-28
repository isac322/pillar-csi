---
title: Prepare a ZFS storage node
description: Set up a Linux host as a pillar-csi ZFS storage node. Load the NVMe-oF target modules at boot, create a zpool and parent dataset, and register the node.
sidebar:
  order: 2
---

A ZFS storage node is a Kubernetes node with a ZFS pool that pillar-csi carves into zvols and exports over NVMe-oF/TCP. This guide prepares the host, creates the pool, and registers the node with the controller. Run the host commands as root on the storage node.

pillar-csi does not create or import pools. The pool must exist, and the `zfs` kernel module must be loaded, before the agent starts.

## Install ZFS

On Ubuntu:

```sh
sudo apt update
sudo apt install -y zfsutils-linux
```

For other distributions, follow the [OpenZFS getting started guide](https://openzfs.github.io/openzfs-docs/Getting%20Started/index.html).

The agent container brings its own copy of the ZFS command-line tools. The host provides the `zfs` kernel module, the `/dev/zfs` device, and the `/dev/zvol/` links for new zvols. Check that the module is loaded:

```sh
lsmod | grep -w zfs
```

## Load the NVMe-oF target modules

Load the modules now:

```sh
sudo modprobe -a nvmet nvmet_tcp
ls /sys/kernel/config/nvmet
```

The last command lists `hosts`, `ports`, and `subsystems`. If `modprobe` cannot find the modules on Ubuntu, install `linux-modules-extra-$(uname -r)` and try again. If they are still missing, the kernel lacks NVMe-oF target support; see [Prerequisites](/docs/reference/prerequisites/).

Load them at every boot:

```sh
printf 'nvmet\nnvmet_tcp\n' | sudo tee /etc/modules-load.d/pillar-csi-target.conf
```

The agent Pod also runs `modprobe` for these modules when it starts, but that only works when the host kernel ships them.

If Pods that use pillar-csi volumes may also run on this node, load the initiator modules too:

```sh
sudo modprobe -a nvme_fabrics nvme_tcp
printf 'nvme_fabrics\nnvme_tcp\n' | sudo tee /etc/modules-load.d/pillar-csi-initiator.conf
```

## Create the pool

Use stable device paths from `/dev/disk/by-id/` so the pool survives device renumbering. A single disk:

```sh
sudo zpool create -o ashift=12 tank /dev/disk/by-id/<disk-id>
```

A two-disk mirror:

```sh
sudo zpool create -o ashift=12 tank mirror \
  /dev/disk/by-id/<disk-a-id> /dev/disk/by-id/<disk-b-id>
```

You can also use a pool you already have.

## Create a parent dataset

Keep Kubernetes volumes under their own dataset so they stay apart from the rest of the pool. The agent does not create this dataset:

```sh
sudo zfs create tank/k8s
```

pillar-csi then creates each volume as a zvol named `tank/k8s/<volume>`. To place volumes at the pool root, skip this step and leave `parentDataset` out of both configurations below.

## Configure the agent

Add the pool to `agent.backends` in your Helm values and install or upgrade the chart as described in [Install with Helm](/docs/how-to/install-helm/):

```yaml
agent:
  backends:
    - zfs:
        pool: tank
        parentDataset: k8s
```

## Register the node

Create a `PillarAgent` for the node and a `PillarStore` for the pool. Replace `storage-1` with the node's name from `kubectl get nodes`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
metadata:
  name: storage-1
spec:
  nodeRef:
    name: storage-1
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: storage-1-tank
spec:
  agentRef: storage-1
  backend:
    zfs:
      pool: tank
      parentDataset: k8s
      properties:
        compression: lz4
```

`parentDataset` must equal the value in `agent.backends`. `properties` is optional; the agent passes each entry to `zfs create` for every new volume in this store.

When the `PillarAgent` exists, the controller labels the node with `pillar-csi.bhyoo.com/agent-node=true`, and the agent DaemonSet starts a Pod there. You do not need to add the label yourself. The controller removes it when you delete the `PillarAgent`.

## Verify

```sh
kubectl get node storage-1 -L pillar-csi.bhyoo.com/agent-node
kubectl wait --for=condition=Ready pillaragent/storage-1 --timeout=3m
kubectl get pillaragent storage-1 -o jsonpath='{.status.discoveredPools}'
kubectl get pillarstore storage-1-tank
```

The agent lists `tank` among its discovered pools with `parentDataset` set to `k8s`, and the store shows `True` under `READY`. If the store stays not ready, run `kubectl describe pillarstore storage-1-tank`. A `PoolDiscovered` condition with reason `BackendLayoutMismatch` means the pool or `parentDataset` differs between the store and `agent.backends`.

Next, bind the store to a protocol with a `PillarStorageClass`. The [first PVC tutorial](/docs/tutorials/first-pvc/) shows a complete example.
