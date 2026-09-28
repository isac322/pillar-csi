---
title: Override settings per binding or per volume
description: Configure pillar-csi with one YAML shape at every layer, and override ZFS, LVM, NVMe-oF/TCP and filesystem settings per PillarStorageClass or per PVC annotation.
sidebar:
  order: 6
---

Every pillar-csi setting has one name and one nested YAML shape, and that shape is the same at every layer where you can set it. The subtree you write on a `PillarProtocol` is the subtree you write in a `PillarStorageClass` override and in a PVC annotation:

```yaml
# PillarProtocol: the default for every volume that uses this protocol
spec:
  protocol:
    nvmeofTcp: {maxQueueSize: 64}
---
# PillarStorageClass: the default for every volume of this binding
spec:
  overrides:
    protocol:
      nvmeofTcp: {maxQueueSize: 64}
---
# PVC: this volume only
metadata:
  annotations:
    pillar-csi.bhyoo.com/protocol: |
      nvmeofTcp: {maxQueueSize: 64}
```

Configuration has three axes: storage, protocol and filesystem. Each axis has a base, and you can override its tunable fields for one binding (`PillarStorageClass`) or for one volume (a PVC annotation, or a parameter on a StorageClass you write by hand).

| Axis | Base | Per binding | Per volume |
|---|---|---|---|
| Storage | `PillarStore.spec.backend.{zfs,lvm}` | `PillarStorageClass.spec.overrides.backend` | `pillar-csi.bhyoo.com/backend` |
| Protocol | `PillarProtocol.spec.protocol.nvmeofTcp` | `PillarStorageClass.spec.overrides.protocol` | `pillar-csi.bhyoo.com/protocol` |
| Filesystem | `fsType: ext4` | `PillarStorageClass.spec.filesystem` | `pillar-csi.bhyoo.com/filesystem` |

`nvmeofTcp` is the only protocol in v0.3.0, and `zfs` and `lvm` are the only backends. iSCSI, NFS and SMB are planned. Each is designed to arrive as another member of the same `protocol` document, next to `nvmeofTcp`, and to follow the same three layers.

## What you can override

Only tunable fields are accepted above the base:

| Document | Tunable fields |
|---|---|
| `backend` with `zfs` | `properties` |
| `backend` with `lvm` | `provisioningMode` (`linear` or `thin`) |
| `protocol` with `nvmeofTcp` | `maxQueueSize`, `inCapsuleDataSize`, `ctrlLossTmo`, `reconnectDelay` |
| `filesystem` | `fsType` (`ext4` or `xfs`), `mkfsOptions`, `mountOptions` |

The structural fields `zfs.pool`, `zfs.parentDataset`, `zfs.volumeType`, `lvm.volumeGroup`, `lvm.thinPool`, `nvmeofTcp.port` and `nvmeofTcp.acl` decide where a volume lives and who can reach it. They are rejected with their path, for example:

```text
pillar-csi.bhyoo.com/protocol: nvmeofTcp.acl is structural and cannot be set per volume
```

The document member must match the base. A `zfs` document on a PVC whose store is LVM fails with `zfs overrides do not apply to a lvm-lv backend`. `provisioningMode: thin` fails unless the `PillarStore` sets `lvm.thinPool`.

## Per binding

Put the overrides on the `PillarStorageClass`. Every PVC that uses the generated StorageClass inherits them.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: db
spec:
  storeRef: storage-1-hot
  protocolRef: nvmeof-default
  filesystem:
    fsType: xfs
    mountOptions: [noatime]
  overrides:
    backend:
      zfs:
        properties: {volblocksize: 16K}
    protocol:
      nvmeofTcp: {maxQueueSize: 64}
```

The generated StorageClass carries only `pillar-csi.bhyoo.com/storage-class` (the binding name), `csi.storage.k8s.io/fstype` and `mountOptions`. The controller reads the rest from the live `PillarStorageClass` at `CreateVolume`, so editing an override does not recreate the StorageClass.

## Per volume

Add up to three annotations to the PVC. Each value is a YAML document:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
  annotations:
    pillar-csi.bhyoo.com/backend: |
      zfs:
        properties: {volblocksize: 16K, compression: zstd}
    pillar-csi.bhyoo.com/protocol: |
      nvmeofTcp: {maxQueueSize: 64}
    pillar-csi.bhyoo.com/filesystem: |
      fsType: xfs
      mkfsOptions: ["-K"]
spec:
  storageClassName: pillar-hot
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 50Gi
```

The annotations must be on the PVC when it is provisioned. The controller reads them once in `CreateVolume`; adding or editing them later changes nothing for that volume.

Any other annotation key under `pillar-csi.bhyoo.com/` on the PVC is rejected, so a typo or a key from 0.2 fails provisioning instead of being ignored. An unknown field or a value outside its range is rejected the same way. The PVC stays `Pending`, and its events carry the error. Read them with:

```sh
kubectl describe pvc postgres-data
```

A `filesystem` annotation on a PVC with `volumeMode: Block` is rejected. A filesystem document inherited from the binding is ignored for block volumes.

## Hand-written StorageClass

A StorageClass you write yourself names the CRs directly and may carry the same three documents as parameters:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: manual-hot
provisioner: pillar-csi.bhyoo.com
parameters:
  pillar-csi.bhyoo.com/store-ref: storage-1-hot
  pillar-csi.bhyoo.com/protocol-ref: nvmeof-default
  pillar-csi.bhyoo.com/backend: |
    zfs: {properties: {compression: lz4}}
  csi.storage.k8s.io/fstype: ext4
```

If it sets both `csi.storage.k8s.io/fstype` and a filesystem document with `fsType`, the two must agree. StorageClass parameters are immutable in Kubernetes, so to change one, delete and recreate the class.

## Precedence

From lowest to highest:

1. `PillarStore` and `PillarProtocol`, and the `ext4` filesystem default
2. `PillarStorageClass.spec.overrides` and `spec.filesystem`, or the documents on a hand-written StorageClass
3. PVC annotations

Scalar fields replace the value below them. ZFS `properties` merge per key. For `mkfsOptions` and `mountOptions`, an omitted list inherits the layer below, and an explicit `[]` clears it.

The controller records the result in the volume's `PillarVolumeState` under `spec.resolved`. The `PillarVolumeState` has the same name as the PV:

```sh
kubectl get pvst <pv-name> -o yaml
```

## mkfs options

`mkfsOptions` runs as root on the worker, and anyone who can create a PVC can set it. pillar-csi accepts only flags that tune the filesystem being created, from a fixed list per filesystem type. Options that make mkfs read or write another file or device are rejected, and so are positional arguments. Write each flag as its own list element (`["-L", "data"]` or `["-Ldata"]`); clustered flags such as `-Fq` are rejected. The options apply only when the node formats a blank device. A volume that already has a filesystem is never reformatted.

See the [annotations reference](/docs/reference/annotations/) for every key pillar-csi reads or writes.
