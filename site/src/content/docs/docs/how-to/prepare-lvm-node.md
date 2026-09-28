---
title: Prepare an LVM storage node
description: "Set up a Linux host as a pillar-csi LVM storage node: load the NVMe-oF target modules at boot, create a volume group and thin pool, and register the node."
sidebar:
  order: 3
---

An LVM storage node is a Kubernetes node with a volume group that pillar-csi carves into logical volumes and exports over NVMe-oF/TCP. This guide covers the one-time operating system setup such a node needs, then registers it with the controller. Run the host commands as root on the storage node.

pillar-csi installs nothing on the host and does not create volume groups or thin pools. Every step before [Configure the agent](#configure-the-agent) is ordinary LVM and kernel setup. If the node already has a volume group and loads the `nvmet` and `nvmet_tcp` modules at boot, skip to that section.

## Install LVM

On Debian or Ubuntu:

```sh
sudo apt update
sudo apt install -y lvm2
```

The agent container brings its own LVM tools. You need `lvm2` on the host to create the volume group.

## Load the kernel modules

Load the NVMe-oF target modules, and `dm_thin_pool` if you plan to use thin provisioning:

```sh
sudo modprobe -a nvmet nvmet_tcp dm_thin_pool
ls /sys/kernel/config/nvmet
```

The last command lists `hosts`, `ports`, and `subsystems`. If `modprobe` cannot find `nvmet` or `nvmet_tcp` on Ubuntu, install `linux-modules-extra-$(uname -r)` and try again. If they are still missing, the kernel lacks NVMe-oF target support; see [Prerequisites](/docs/reference/prerequisites/).

Load them at every boot:

```sh
printf 'nvmet\nnvmet_tcp\ndm_thin_pool\n' | sudo tee /etc/modules-load.d/pillar-csi-target.conf
```

The agent Pod also runs `modprobe nvmet` and `modprobe nvmet_tcp` when it starts, but that only works when the host kernel ships them.

If Pods that use pillar-csi volumes may also run on this node, load the initiator modules too:

```sh
sudo modprobe -a nvme_fabrics nvme_tcp
printf 'nvme_fabrics\nnvme_tcp\n' | sudo tee /etc/modules-load.d/pillar-csi-initiator.conf
```

## Create the volume group

Use stable device paths from `/dev/disk/by-id/`:

```sh
sudo pvcreate /dev/disk/by-id/<disk-id>
sudo vgcreate data-vg /dev/disk/by-id/<disk-id>
sudo vgs data-vg
```

You can also use a volume group you already have.

## Create a thin pool (optional)

By default pillar-csi creates linear logical volumes, which allocate their full size at creation. Thin volumes allocate space as data is written and live inside a thin pool. To use them, create the pool:

```sh
sudo lvcreate --type thin-pool --size 100G --name thin0 data-vg
```

Thin volumes can promise more space than the pool holds. Watch the `Data%` and `Meta%` columns of `sudo lvs data-vg`, because writes fail once the pool fills.

## Configure the agent

Add the volume group to `agent.backends` in your Helm values and install or upgrade the chart as described in [Install with Helm](/docs/how-to/install-helm/).

Linear volumes only:

```yaml
agent:
  backends:
    - lvm:
        volumeGroup: data-vg
```

With the thin pool:

```yaml
agent:
  backends:
    - lvm:
        volumeGroup: data-vg
        thinPool: thin0
```

## Register the node

Create a `PillarAgent` for the node and a `PillarStore` for the volume group. Replace `storage-1` with the node's name from `kubectl get nodes`.

For linear volumes:

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
  name: storage-1-data
spec:
  agentRef: storage-1
  backend:
    lvm:
      volumeGroup: data-vg
```

If `agent.backends` names a thin pool for the volume group, the store must name the same `thinPool`, or it will not become ready. Select thin provisioning for new volumes with `provisioningMode`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: storage-1-thin
spec:
  agentRef: storage-1
  backend:
    lvm:
      volumeGroup: data-vg
      thinPool: thin0
      provisioningMode: thin
```

`provisioningMode` defaults to `linear`. A `PillarStorageClass` or a PVC annotation can override it per binding or per volume, so one thin-pool store can still create linear volumes.

When the `PillarAgent` exists, the controller labels the node with `pillar-csi.bhyoo.com/agent-node=true`, and the agent DaemonSet starts a Pod there. You do not need to add the label yourself. The controller removes it when you delete the `PillarAgent`.

## Verify

```sh
kubectl get node storage-1 -L pillar-csi.bhyoo.com/agent-node
kubectl wait --for=condition=Ready pillaragent/storage-1 --timeout=3m
kubectl get pillaragent storage-1 -o jsonpath='{.status.discoveredPools}'
kubectl get pillarstore
```

The agent lists `data-vg` among its discovered pools, with `thinPool` set if you configured one, and the store shows `True` under `READY`. If the store stays not ready, run `kubectl describe pillarstore <name>`. A `PoolDiscovered` condition with reason `BackendLayoutMismatch` means `volumeGroup` or `thinPool` differs between the store and `agent.backends`.

Next, bind the store to a protocol with a `PillarStorageClass`. The [first PVC tutorial](/docs/tutorials/first-pvc/) shows a complete example.
