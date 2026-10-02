---
title: Expand a volume
description: Grow a pillar-csi PVC backed by a ZFS zvol, ZFS dataset, or LVM logical volume while it stays mounted; block filesystems resize on the worker and NFS datasets grow server-side.
sidebar:
  order: 8
---

pillar-csi supports online expansion. You raise the PVC's storage request, and the agent grows the zvol, ZFS dataset quota, or logical volume while the pod keeps running. Block volumes then grow their ext4 or xfs filesystem on the worker; NFS reports the larger dataset capacity without a node-side filesystem resize. Shrinking is not supported.

## Before you start

The StorageClass must allow expansion. A StorageClass generated from a `PillarStorageClass` has `allowVolumeExpansion: true` unless you set `spec.storageClass.allowVolumeExpansion: false`. Check it:

```sh
kubectl get storageclass <name> -o jsonpath='{.allowVolumeExpansion}'
```

`allowVolumeExpansion` is the one StorageClass field Kubernetes lets you change in place, so switching it on a `PillarStorageClass` updates the generated class without recreating it.

The pool or dataset parent needs free space for the new size. For ZFS block volumes, the limit is the available space for the zvol; for NFS, the limit is the dataset quota and its parent dataset. For LVM thin volumes, `lvextend` grows the virtual size inside the thin pool.

## Expand

Edit the PVC's request:

```sh
kubectl patch pvc postgres-data \
  -p '{"spec":{"resources":{"requests":{"storage":"80Gi"}}}}'
```

Then watch the PVC until `status.capacity` shows the new size:

```sh
kubectl get pvc postgres-data -w
kubectl describe pvc postgres-data
```

## What happens

1. `csi-resizer` in the controller pod calls `ControllerExpandVolume`.
2. The controller asks the agent to grow the backend volume: `zfs set volsize=<bytes>` for a ZFS zvol, `zfs set quota=<bytes>` for a ZFS dataset, or `lvextend -L <bytes>b` for LVM linear and thin volumes.
3. For NVMe-oF/TCP, the agent revalidates the namespace; an iSCSI LIO backstore reads the new block size. NFS needs no client-side device step.
4. The controller reports node expansion required for block volumes. On the worker, `NodeExpandVolume` makes the kernel see the new block size, then grows ext4 with `resize2fs` or xfs with `xfs_growfs`. For NFS, `NodeExpansionRequired` is false and the mounted client sees the updated server-side quota.

For a raw block volume (`volumeMode: Block`) there is no filesystem, so block step 4 only reports the new size. NFS rejects `volumeMode: Block`.

For a volume [attached locally on the storage node](/docs/how-to/local-attach/#expansion), the NVMe namespace is disabled, so the agent skips `revalidate_size`; an iSCSI target has no LUN at that time. `NodeExpandVolume` on the storage node reloads the `pillar-local-*` device-mapper table to the new size and then grows the filesystem on that device.

If no pod uses a block PVC during expansion, Kubernetes finishes the filesystem step the next time a pod mounts it. An NFS dataset does not need a node-side expansion step.

## Failures

| Symptom | Cause | Fix |
|---|---|---|
| PVC event with `ResourceExhausted` | The pool or volume group has too little free space for the new size. | Free space in the pool, or request a smaller size. |
| Error containing `cannot shrink` (LVM) or `cannot be decreased` (ZFS) | The new request is smaller than the current size. | Request a size at least as large as the current one. |
| `resize2fs` or `xfs_growfs` error in the node plugin log | The filesystem resize failed on the worker. | Read the tool output in the log, then fix the filesystem problem it names. |
| Expansion is never attempted | The StorageClass has `allowVolumeExpansion: false`. | Set it to `true` on the `PillarStorageClass`. |

Read the node plugin log on the worker that runs the pod:

```sh
kubectl -n pillar-csi logs ds/pillar-csi-node -c node
```

`kubectl logs ds/...` picks one pod of the DaemonSet. To read the pod on a specific worker, find it first:

```sh
kubectl -n pillar-csi get pods -o wide -l app.kubernetes.io/component=node
```
