---
title: Configure NFS volumes
description: Serve shared ZFS datasets over NFSv4.2 without installing NFS userspace packages on Kubernetes nodes.
sidebar:
  order: 3
---

NFS volumes use a ZFS dataset as the backend and NFSv4.2 as the network protocol. The storage node serves the dataset on TCP port `2049`; the node image carries the NFS mount helper. Kubernetes nodes need kernel NFS support, but they do not need `nfs-common`, `nfs-utils`, `mount.nfs`, or another host-side NFS package.

NFS is a single-storage-node export. `ReadWriteMany` permits concurrent mounts, but it does not replicate the dataset or make it available when the storage node is down.

## Before you start

Prepare all of the following:

- A ZFS pool and a persistent, NFS-exportable backing filesystem for the agent's dataset pseudoroot.
- NFS server support (`nfsd`) on the storage node.
- NFS client support (`nfs` and `nfsv4`, or built-in kernel support) on every worker that can mount the volume.
- The `agent.backends` entry for the ZFS pool in Helm values.
- A `PillarAgent` that points at the storage node.

The production pseudoroot is `/var/lib/pillar-csi/agent/datasets`. It must persist across agent and node restarts and must support stable NFS export filehandles. Container overlay/rootfs is not supported for this path. See [Prerequisites](/docs/reference/prerequisites/#nfs-persistent-backing).

If the kernel modules are loaded after `pillar-node` starts, restart the `pillar-node` Pod. The startup check is intentional: a node that cannot mount NFS does not advertise NFS client capability.

## Define the dataset store

Set the ZFS volume type to `dataset`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: storage-1-datasets
spec:
  agentRef: storage-1
  backend:
    zfs:
      pool: tank
      parentDataset: k8s
      volumeType: dataset
```

The `parentDataset` must match the agent's `agent.backends[].zfs.parentDataset`. A `zfs-zvol` store cannot be paired with NFS.

## Define the NFS protocol

NFS settings belong to the `PillarProtocol`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nfs-default
spec:
  protocol:
    nfs:
      version: "4.2"
      port: 2049
      acl: true
      squash: root
```

The version and port are fixed by the driver. `acl: true` limits access to the InternalIP addresses of nodes published to the volume. `squash: root` is the default and prevents remote root from becoming root on the dataset.

## Create a storage class binding

Join the dataset store and NFS protocol with a `PillarStorageClass`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: shared-datasets
spec:
  storeRef: storage-1-datasets
  protocolRef: nfs-default
  filesystem:
    mountOptions:
      - noatime
```

`ReadWriteMany` is supported for NFS. `ReadOnlyMany` creates a volume-wide read-only export. A volume cannot mix writable and read-only-many capabilities.

## Per-volume settings

NFS has no per-volume protocol tuning. This document is accepted for the common override shape but does not change NFS behavior:

```yaml
pillar-csi.bhyoo.com/protocol: |
  nfs: {}
```

Do not put `version`, `port`, `acl`, or `squash` in that document. Those fields are structural and are rejected. Set them on the referenced `PillarProtocol` instead.

For one volume, use the filesystem annotation only for mount options:

```yaml
metadata:
  annotations:
    pillar-csi.bhyoo.com/filesystem: |
      mountOptions:
        - noatime
```

`fsType` may be omitted or set to `nfs`. `mkfsOptions`, enabled `periodicTrim`, block volume mode, `localAttach`, and contradictory NFS mount flags are rejected. The controller adds the required NFSv4.2, TCP, hard-mount and port settings.

## Access and filesystem ownership

NFS publishes access for each node through `ControllerPublishVolume`. With `acl: true`, revoked or unpublished node IPs cannot access the dataset. The export still comes from the one storage node that owns the ZFS pool.

When a workload needs root or `fsGroup` initialization to write the dataset, choose `squash: none` explicitly. This changes authorization; it does not encrypt NFS traffic. NFS RPC TLS is not offered.

## Expand and recover

Edit the PVC's requested capacity as usual. The agent increases the server-side ZFS dataset quota; the mounted client sees the new capacity without a node-side filesystem resize.

After an agent restart or storage-node reboot, the controller replays the durable export state. The agent restores the managed root and child dataset exports, ACL membership, read-only state, squash policy and NFS version. Foreign NFS exports are not stopped or reconfigured.

If the volume does not mount, check:

```sh
kubectl describe pvc <claim>
kubectl describe pillaragent <agent>
kubectl describe pillarstore <store>
```

Then verify the worker's NFS client support and the storage node's persistent pseudoroot. See [Troubleshooting](/docs/how-to/troubleshooting/) for the common NFS errors.
