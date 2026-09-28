---
title: Expand a volume
description: Grow a pillar-csi PVC backed by a ZFS zvol or an LVM logical volume while it stays mounted, and resize its ext4 or xfs filesystem on the Kubernetes worker.
sidebar:
  order: 7
---

pillar-csi supports online expansion. You raise the PVC's storage request, the agent grows the zvol or logical volume on the storage node, and the node plugin grows the filesystem while the pod keeps running. Shrinking is not supported.

## Before you start

The StorageClass must allow expansion. A StorageClass generated from a `PillarStorageClass` has `allowVolumeExpansion: true` unless you set `spec.storageClass.allowVolumeExpansion: false`. Check it:

```sh
kubectl get storageclass <name> -o jsonpath='{.allowVolumeExpansion}'
```

`allowVolumeExpansion` is the one StorageClass field Kubernetes lets you change in place, so switching it on a `PillarStorageClass` updates the generated class without recreating it.

The pool needs free space for the new size. For ZFS, the limit is the space available to the store's parent dataset. For LVM thin volumes, `lvextend` grows the virtual size of the thin volume inside the thin pool.

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
2. The controller asks the agent to grow the backend volume: `zfs set volsize=<bytes>` for ZFS, `lvextend -L <bytes>b` for LVM linear and thin volumes.
3. If the volume is exported, the agent writes `1` to the namespace's `revalidate_size` in nvmet configfs. The namespace stays enabled, so connected workers keep their session.
4. The controller always reports that node expansion is required. On the worker, `NodeExpandVolume` rescans the NVMe controller so the kernel sees the new size, then grows the filesystem: `resize2fs <device>` for ext4, `xfs_growfs <mount point>` for xfs.

For a raw block volume (`volumeMode: Block`) there is no filesystem, so step 4 only reports the new size. The application sees the larger device.

If no pod uses the PVC during expansion, Kubernetes finishes the filesystem step the next time a pod mounts it.

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
