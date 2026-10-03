---
title: Import a zvol from another CSI driver
description: Move a PVC from democratic-csi, openebs zfs-localpv or any other driver that stores it as a ZFS zvol to pillar-csi without copying data, by adopting the existing zvol through a PVC annotation.
sidebar:
  order: 13
---

If a volume already lives as a ZFS zvol on a pillar-csi storage node, pillar-csi can take it over in place instead of copying it into a new volume. You create a PVC with the annotation `pillar-csi.bhyoo.com/import-zvol` set to the zvol's full dataset name. `CreateVolume` then adopts that zvol instead of creating one, and everything else happens as for a new volume: the PV, the `PillarVolumeState`, the NVMe-oF/TCP export and its recorded export spec, and fencing.

This works for zvols created by democratic-csi (`zfs-generic-iscsi`, `zfs-generic-nvmeof`), by openebs zfs-localpv for volumes with an `ext4` or `xfs` `fsType` (those are zvols), or by hand. It does not work for ZFS filesystem datasets.

## Existing filesystem adoption is a separate feature

This guide documents zvol adoption only. Existing directory and ZFS filesystem adoption is a separate, branch-only and unreleased feature that uses the opt-in `files.pillar-csi.bhyoo.com` CSI identity, `pillar-csi.bhyoo.com/import-directory`, or `pillar-csi.bhyoo.com/import-zfs-dataset`. See [Existing filesystem adoption in the project README](https://github.com/isac322/pillar-csi#existing-filesystem-adoption-branch-only-opt-in) for the chart values, `PillarStore` and `PillarStorageClass` examples, exact quota prerequisite, topology, NFS/RWX mode and manual `Retain` rebinding.

Filesystem adoption preserves the original directory or dataset when its CSI lifecycle is deleted. That rule does not apply to the zvol flow on this page: with `reclaimPolicy: Delete`, deleting an imported zvol still destroys it, as described below.

## What import does and does not do

Import never creates, resizes, formats or changes properties of the zvol. ZFS properties from the store, the class or a `pillar-csi.bhyoo.com/backend` annotation are not applied to it. User properties left by the old driver, such as `democratic-csi:*`, stay and are ignored.

The node plugin mounts the filesystem that is already on the zvol. It formats only blank devices, so existing data is never reformatted. The filesystem type the volume resolves to must match what is on the disk, or the mount fails. Set it with the `pillar-csi.bhyoo.com/filesystem` annotation or on the `PillarStorageClass`.

After the import the volume is an ordinary pillar-csi volume. **With `reclaimPolicy: Delete`, deleting the new PVC destroys the imported zvol**, like any pillar-csi volume. Use a StorageClass with `reclaimPolicy: Retain` for imports unless you want that.

## Before you start

- The zvol must be on a pillar-csi storage node, exactly one level below the `pool` and `parentDataset` of a `PillarStore`. A zvol at `hot-data/k8s/pvc-0d52...` needs a store with `pool: hot-data` and `parentDataset: k8s`. A store with no `parentDataset` imports zvols directly under the pool, such as `hot-data/pvc-0d52...`. The agent's backend entry for that pool must use the same `parentDataset`.
- The zvol name, the last component, must use only ASCII letters, digits and `-` `_` `.` `:`.
- Plan a short outage. The workload must stop, and the old driver must release the zvol before pillar-csi can take it.

## Move one volume

The example moves the PVC `data-postgres-0` in namespace `db`, owned by the StatefulSet `postgres`. Its zvol is `hot-data/k8s/pvc-0d5201a5-3c1e-4c55-a2a7-3f7d3c4e9b10`.

### 1. Keep the old PV and save the old claim

Set the old PV to `Retain`, so that deleting the old PVC does not delete the zvol. Save the old PVC, because you copy its labels and annotations in step 5:

```sh
PV=$(kubectl -n db get pvc data-postgres-0 -o jsonpath='{.spec.volumeName}')
kubectl patch pv "$PV" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
kubectl -n db get pvc data-postgres-0 -o yaml > data-postgres-0.old.yaml
```

Note the zvol name and size. For democratic-csi the zvol is `<datasetParentName>/<PV name>`. For openebs zfs-localpv it is `<poolname>/<PV name>`, and `kubectl -n openebs get zfsvolume "$PV" -o yaml` shows it. On the storage node:

```sh
zfs get -Hp -o property,value type,volsize hot-data/k8s/pvc-0d5201a5-3c1e-4c55-a2a7-3f7d3c4e9b10
```

`type` must be `volume`.

### 2. Stop the workload

Scale the workload down so no pod uses the PVC, then wait until the old driver has unmounted and detached it. The `VolumeAttachment` for the old PV disappears when detach finishes:

```sh
kubectl -n db scale statefulset postgres --replicas=0
kubectl get volumeattachment -o custom-columns=NAME:.metadata.name,PV:.spec.source.persistentVolumeName | grep "$PV"
```

Repeat the second command until it prints nothing. openebs zfs-localpv does not attach, so it has no `VolumeAttachment`; wait until the pod is gone.

### 3. Take a snapshot

On the storage node, take a snapshot as a restore point:

```sh
zfs snapshot hot-data/k8s/pvc-0d5201a5-3c1e-4c55-a2a7-3f7d3c4e9b10@pre-pillar
```

### 4. Remove the old driver's hold on the zvol

Delete the old PVC and PV. With `Retain`, the old driver does not delete the zvol:

```sh
kubectl -n db delete pvc data-postgres-0
kubectl delete pv "$PV"
```

Then remove what the old driver still keeps on the storage node:

- **democratic-csi over iSCSI.** Deleting a `Retain` PV leaves the LIO iSCSI target and its block backstore. Find the backstore whose device is the zvol, then delete its iSCSI target and the backstore, and save the configuration so they do not come back at boot:

  ```sh
  targetcli ls /backstores/block
  targetcli /iscsi delete <target IQN of this volume>
  targetcli /backstores/block delete <backstore name>
  targetcli saveconfig
  ```

- **democratic-csi over NVMe-oF.** Remove the nvmet subsystem that exports the zvol in the same way.
- **openebs zfs-localpv.** Leave the `ZFSVolume` resource in the `openebs` namespace for now. Deleting it while it still has the `zfs.openebs.io/finalizer` finalizer makes the openebs node plugin destroy the dataset. The openebs node plugin already unmounted the zvol when the pod stopped, so nothing else needs to be removed. Once the import works, you can remove the leftover resource. First remove the finalizer, then delete it. Without the finalizer the plugin only sees a delete event for an object that is already gone, and it leaves the zvol alone:

  ```sh
  kubectl -n openebs patch zfsvolume "$PV" --type=merge -p '{"metadata":{"finalizers":null}}'
  kubectl -n openebs delete zfsvolume "$PV"
  zfs list hot-data/k8s/pvc-0d5201a5-3c1e-4c55-a2a7-3f7d3c4e9b10   # still there
  ```

You do not need to remove the old driver's ZFS user properties. If you are not sure the zvol is free, the agent checks it in the next step and refuses the claim with the reason.

### 5. Create the PVC with the annotation

Create a PVC with the same name and namespace, so that a StatefulSet, CNPG cluster or other operator that owns the claim finds it. Use a pillar-csi StorageClass whose `PillarStore` has the zvol's `pool` and `parentDataset`. Request at most the zvol's `volsize`. Set the filesystem type to the one on the disk:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-postgres-0
  namespace: db
  labels:
    app: postgres
  annotations:
    pillar-csi.bhyoo.com/import-zvol: hot-data/k8s/pvc-0d5201a5-3c1e-4c55-a2a7-3f7d3c4e9b10
    pillar-csi.bhyoo.com/filesystem: |
      fsType: xfs
spec:
  storageClassName: pillar-hot-data-retain
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 20Gi
```

Copy the labels and annotations from `data-postgres-0.old.yaml` that the owner relies on. A StatefulSet matches its claims by name, but operators often select them by label: a CNPG instance PVC needs its `cnpg.io/*` labels and annotations, or CNPG does not recognise the claim. Do not copy `pv.kubernetes.io/*`, `volume.kubernetes.io/*` or `volume.beta.kubernetes.io/*` annotations, or the old driver's own keys. They belong to the old binding.

Wait for the PVC to become `Bound`, then scale the workload back up:

```sh
kubectl -n db get pvc data-postgres-0 -w
kubectl -n db scale statefulset postgres --replicas=1
```

If the PVC stays `Pending`, `kubectl -n db describe pvc data-postgres-0` shows the refusal. The table in [Refusals](#refusals) lists each one. Fix the cause. The provisioner retries on its own, except for the refusals that say to re-create the PVC.

### 6. Verify and clean up

The `PillarVolumeState` of an imported volume records the source dataset:

```sh
kubectl get pillarvolumestate -o custom-columns=NAME:.metadata.name,IMPORTED:.spec.importedFrom
```

The PV has the size of the zvol, which can be larger than the request. The zvol keeps its original name on the storage node: its agent volume ID is `<pool>/<zvol name>`, not `<pool>/<PV name>`.

When the workload runs correctly, destroy the snapshot:

```sh
zfs destroy hot-data/k8s/pvc-0d5201a5-3c1e-4c55-a2a7-3f7d3c4e9b10@pre-pillar
```

pillar-csi destroys a zvol without `-r`, so while the snapshot exists, `DeleteVolume` for this volume fails.

## Roll back

**Before the PVC binds.** A refused claim holds nothing. Delete the new PVC. When the claim is deleted, pillar-csi tells the agent to forget the claim and removes its records. It never destroys, resizes or reformats a zvol whose import did not complete, and it keeps retrying while the agent is unreachable. The zvol is unchanged, and the snapshot is only needed if you wrote to the zvol by hand.

**After the PVC binds.** To restore the data as it was before the move, stop the workload, wait until its `VolumeAttachment` is gone as in step 2, then roll the zvol back on the storage node and start the workload again:

```sh
kubectl -n db scale statefulset postgres --replicas=0
zfs rollback hot-data/k8s/pvc-0d5201a5-3c1e-4c55-a2a7-3f7d3c4e9b10@pre-pillar
kubectl -n db scale statefulset postgres --replicas=1
```

`zfs rollback` discards everything written after the snapshot. pillar-csi has no way to hand a bound volume back to the old driver without destroying it. Deleting a `Retain` PV leaves the `PillarVolumeState` and the NVMe-oF/TCP export in place, and deleting a `Delete` PV destroys the zvol. To go back to the old driver, copy the data out first, then delete the pillar-csi volume and restore the data into a volume of the old driver.

## Refusals

The PVC stays `Pending`, and its events show one of these messages. `<annotation>` stands for `pillar-csi.bhyoo.com/import-zvol`.

| Message | Cause | Fix |
|---|---|---|
| `unsupported PVC annotation "<annotation>": value must name a ZFS dataset ..., got empty` | The annotation is present with an empty value. | Set the full dataset name or remove the annotation. |
| `<annotation> must name a ZFS dataset as "<pool>[/<parent>]/<name>", got ...` | The value has no `/`, or starts or ends with `/`. | Use the full name, for example `hot-data/k8s/pvc-0d52...`. |
| `<annotation> dataset ...: empty dataset component` | The value contains `//`. | Remove the extra `/`. |
| `<annotation> dataset ...: dataset component ... is not a name` | A component is `.` or `..`. | Use the dataset's real name. |
| `<annotation> dataset ...: dataset component ... contains invalid character ...` | A component contains `@` (a snapshot), `#` (a bookmark), a space or a non-ASCII character. | Name the zvol itself. A snapshot cannot be imported; clone or rename it into a zvol first. |
| `<annotation> requires a zfs.zvol PillarStore backend, got backend ...` | The StorageClass's store is LVM or a ZFS filesystem store. | Use a StorageClass of a ZFS zvol store. |
| `<annotation> dataset ... lives in pool ... but the PillarStore selects pool ...` | The first component is not the store's `pool`. | Use a StorageClass whose store has that pool. |
| `<annotation> dataset ... is not under the PillarStore's parent dataset ...` | The path between pool and zvol name is not exactly the store's `parentDataset`: a sibling such as `hot-data/k8s-other/x`, a deeper zvol such as `hot-data/k8s/a/b`, or a pool-root zvol for a store with a `parentDataset`. | Use a store with that `parentDataset`, or `zfs rename` the zvol under the store's `parentDataset`. |
| `<annotation>: zvol ... is already managed by volume ... (PillarVolumeState ...); delete that volume first` | Another pillar-csi volume already owns the zvol. Either another PVC imported it, or another PVC's import of it is still `Pending`, for example refused because it asked for more than `volsize`. | Import each zvol once. Delete the other PVC if it is a failed attempt. Its cleanup leaves the zvol untouched, and the next retry of this claim proceeds. |
| ``<annotation>: zvol ... is reserved by volume ... of claim ... (PillarVolumeReservation ...); delete that claim first, or, after verifying that claim and its PillarVolumeState no longer exist, release the reservation with `kubectl delete pillarvolumereservation ...` `` | Another claim holds the zvol's reservation. Usually another PVC imports the same zvol. Rarely, the controller stopped after reserving the zvol for an earlier claim and before recording that claim's volume. | Import each zvol once. If the named claim is stale, delete it. If the claim and its volume are gone, release the reservation by hand (see [Release a stale reservation](#release-a-stale-reservation)). |
| `cannot expand volume ...` or `cannot publish volume ...: its import of zvol ... was never adopted` | The volume's import has not completed, so pillar-csi does not manage the zvol yet. | Fix the import refusal first. |
| `<annotation>: volume ... was already provisioned without an import; delete the PersistentVolumeClaim and re-create it to import a zvol` | The annotation was added to a claim whose provisioning had already started. | Delete the PVC and create it again with the annotation. |
| `<annotation>: volume ... already imported ...; the import source cannot be changed` | The annotation changed after the import started. | Restore the first value, or delete the PVC and create it again. |
| `import of volume ... refused: missing: dataset ... does not exist` | No dataset with that name exists on the storage node. | Check the name with `zfs list -t volume`. |
| `import of volume ... refused: missing: block device ... is not present on the storage node` | The zvol exists but its `/dev/zvol` device is not there yet. | Wait for udev, or check `volmode` is not `none`. |
| `import of volume ... refused: wrong type: dataset ... has type ..., only ZFS volumes (zvols) can be imported` | The dataset is a filesystem or a snapshot. | Only zvols can be imported. |
| `import of volume ... refused: too small: dataset ... is ... bytes, requested capacity is ... bytes; import never resizes` | The PVC requests more than the zvol's `volsize`. | Request at most `volsize`, then [expand](/docs/how-to/expand-volume/) after the import. |
| `import of volume ... refused: in use: LIO backstore ... still points at ...; remove the old driver's export (targetcli delete) first` | democratic-csi's iSCSI export still exists. | Delete the target and backstore as in step 4. |
| `import of volume ... refused: in use: nvmet subsystem ... namespace ... still exports ...; remove the export first` | An NVMe-oF export of the zvol still exists. | Remove the old driver's nvmet subsystem. |
| `import of volume ... refused: in use: device ... is mounted at ...; unmount it first` | The zvol or one of its partitions is mounted on the storage node. | Stop the pod that uses it, or unmount it. |
| `import of volume ... refused: in use: device ... has device-mapper holders in sysfs; release the holding device first` | A multipath map, LVM, md or other device-mapper device sits on the zvol. | Remove that device. |
| `import of volume ... refused: in use: device ... is held exclusively on the storage node (mounted, exported, or opened by another consumer)` | Some other process holds the zvol open exclusively. | Find it with `fuser` or `lsof` on the device and stop it. |
| `volume ...: requested ZFS parent dataset ... does not match the agent backend's configured ZFS parent dataset ...` | The `PillarStore`'s `parentDataset` differs from the agent's backend entry for the pool. | Align the store with the agent's `backends` entry. |
| `import of volume ... refused: layout: expected dataset ... does not match the resolved dataset ...` | The store's `pool`/`parentDataset` disagree with the agent's backend layout, so the agent resolved a different dataset than the annotation named. | Align the store with the agent's `backends` entry, then retry. |

Messages that start with `import of volume` and the parent dataset mismatch come from the agent on the storage node. The others come from the controller before it calls the agent.

### Release a stale reservation

pillar-csi reserves a zvol with a `PillarVolumeReservation` before it calls the agent, so that two claims cannot import the same zvol. When the claim is deleted, the reservation is removed with the claim's volume. The controller never removes a reservation on its own: it cannot tell a controller that crashed from one that is only paused and could still reach the agent. If the claim that holds the reservation is gone, release the reservation by hand:

1. Read the owner from the refusal message, or from the reservation:

   ```sh
   kubectl get pillarvolumereservation <name> -o yaml
   ```

2. Check that the owning claim (`spec.claimRef`, same namespace, name and UID) no longer exists, and that no `PillarVolumeState` named `spec.ownerVolume` exists:

   ```sh
   kubectl -n <namespace> get pvc <claim name> -o jsonpath='{.metadata.uid}'
   kubectl get pillarvolumestate <ownerVolume>
   ```

3. Only when both are gone, delete the reservation:

   ```sh
   kubectl delete pillarvolumereservation <name>
   ```

The next retry of the new claim then reserves the zvol again and imports it.
