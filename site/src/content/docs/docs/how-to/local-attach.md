---
title: Attach volumes locally on the storage node
description: Let pods on the storage node use a pillar-csi zvol or logical volume directly instead of through an NVMe-oF/TCP loopback, and how the network export is fenced while they do.
sidebar:
  order: 7
---

By default every consumer reaches a volume over NVMe-oF/TCP, including a pod scheduled on the storage node itself: that pod goes through a loopback connection to the kernel target on the same host. With `localAttach` enabled, a pod on the storage node uses the backend zvol or logical volume directly. Pods on every other node still use NVMe-oF/TCP, and a volume can move between the two paths each time it is published.

## When a publish is local

`ControllerPublishVolume` picks the local path only when all of these hold:

- the volume was provisioned with `localAttach` enabled;
- the node being published to is the `spec.nodeRef.name` of the volume's `PillarAgent`. An agent defined with `spec.external` never qualifies, because it runs outside the cluster;
- the access mode is a single-node mode: `ReadWriteOnce`, `ReadWriteOncePod`, or single-node read-only. Multi-node modes such as `ReadOnlyMany` always use the protocol.

Otherwise the volume attaches over NVMe-oF/TCP exactly as it does without the flag. A local publish needs no NVMe host NQN on the `CSINode` and grants no initiator on the target.

## Requirements

- The `dm_mod` kernel module on the storage node. The node plugin claims the backend device with a device-mapper target. The chart's node init container loads `dm_mod` by default (`node.initModprobe.modules`); as with the other modules, load it on the host and list it in `/etc/modules-load.d/` so a failed `modprobe` does not go unnoticed. See [Prerequisites](/docs/reference/prerequisites/#kernel-modules).
- `dmsetup`, which the node image ships.
- The node plugin must run on the storage node and see `/sys/kernel/config/nvmet` there. The chart's node DaemonSet runs on every node unless you restrict `node.nodeSelector`, and its host `/sys` mount exposes the nvmet configfs. If the node plugin cannot read the nvmet configfs, a local stage fails instead of guessing.

## Enable it

On a `PillarStorageClass`, set `spec.localAttach`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: hot-local
spec:
  storeRef: storage-1-hot
  protocolRef: nvmeof-default
  localAttach: true
  storageClass:
    name: pillar-hot-local
```

The generated StorageClass does not carry the flag. The controller reads it from the live `PillarStorageClass` at `CreateVolume`, like the [overrides](/docs/how-to/volume-overrides/).

On a StorageClass you write yourself, set the parameter `pillar-csi.bhyoo.com/local-attach` to `"true"` or `"false"`. Any other value fails provisioning with `InvalidArgument`. Omitting it means `false`.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: manual-hot-local
provisioner: pillar-csi.bhyoo.com
parameters:
  pillar-csi.bhyoo.com/store-ref: storage-1-hot
  pillar-csi.bhyoo.com/protocol-ref: nvmeof-default
  pillar-csi.bhyoo.com/local-attach: "true"
```

There is no PVC annotation for it.

The setting is recorded per volume at provisioning, in `spec.resolved.localAttach` of the volume's `PillarVolumeState`. Changing `spec.localAttach` later affects only volumes provisioned afterwards. Check an existing volume with:

```sh
kubectl get pvst <pv-name> -o jsonpath='{.spec.resolved.localAttach}'
```

## What happens on a local attach

1. The controller records the storage node in the volume's `PillarVolumeState` as `status.localAttachNode`, in the same update that reserves the publication.
2. The controller asks the agent to take the export away from remote initiators. For NVMe-oF/TCP the agent writes `0` to the namespace's `enable` file in nvmet configfs and reads it back. The subsystem and the port stay configured; any remote session that is still connected can no longer do I/O.
3. The agent returns the backend device path, and the controller hands it to the node plugin in the publish context.
4. `NodeStageVolume` on the storage node creates a device-mapper linear target named `pillar-local-<16 hex>` over the whole backend device. The name is `pillar-local-` followed by the first 16 hex characters of the SHA-256 of the volume ID. The target holds the backend device open exclusively.
5. With the claim in place, the node plugin reads the nvmet state of the volume's subsystem. If any namespace is still enabled, it backs off and fails the stage with `FailedPrecondition`.
6. The node plugin formats and mounts the filesystem on `/dev/mapper/pillar-local-<16 hex>`. A `volumeMode: Block` volume is bound from the same device.

If a stage fails at any step after the claim, the node plugin unmounts whatever it staged and then removes the target. If the unmount fails, it keeps the target and reports the unmount failure along with the original error, so no mount is left pointing at a removed device.

`NodeUnstageVolume` unmounts and then removes the device-mapper target. If a stage record is missing, for example after a failed stage, unstage still removes a `pillar-local-*` target left for that volume.

## Moving a pod between the storage node and other nodes

Pods move freely; each publish picks the path again.

**From another node to the storage node.** Kubernetes unpublishes the old node first, which revokes its initiator on the target. The local publish then disables the namespace, so even a remote host that kept its NVMe-oF session, for example after a force-detach while its kubelet was down, cannot write to the volume.

**From the storage node to another node.** Kubernetes unstages the volume on the storage node, which removes the `pillar-local-*` target, and then unpublishes it. Unpublishing a local attach does not re-enable the export and leaves `status.localAttachNode` set. Every publish of the volume over the network re-enables it before granting the new node's initiator:

1. The agent opens the backend device with `O_EXCL`. If something on the storage node still holds it, the agent refuses, the namespace stays disabled, and the publish fails with `FailedPrecondition`.
2. Otherwise the agent keeps the device open while it writes `1` to the namespace's `enable` file and reads it back, then closes it. While the agent holds the device, the node plugin cannot claim it, so the export and a local attach are never active together.
3. If `status.localAttachNode` was set, the controller clears it and asks the agent to re-enable the export once more under the fencing generation that the clear committed. Then it grants the new node's initiator.

The re-enable call is a no-op when the export is already enabled. The controller makes it on every network publish of a `localAttach` volume, including when `status.localAttachNode` is already empty.

The external-attacher retries a failed publish, so the pod on the new node stays in `ContainerCreating` until the storage node really lets go of the device. This is what protects the volume during a force-detach: if the storage node's kubelet is dead but the container still runs and writes through the local mount, the target stays in place and the export stays disabled. The publish proceeds only after the claim is gone, for example after the kubelet comes back and unstages the volume, or after the storage node reboots.

Do not remove a `pillar-local-*` target by hand to unblock a publish. While it exists, something on the storage node may still be writing to the volume.

## Expansion

Expansion works the same way as for a protocol attach; see [Expand a volume](/docs/how-to/expand-volume/). While the namespace is disabled, the agent grows the zvol or logical volume but skips the NVMe `revalidate_size` step, which the kernel rejects on a disabled namespace. On the storage node, `NodeExpandVolume` reloads the device-mapper table to the new size of the backend device and then grows the filesystem. For a block volume it only reloads the table.

## Observe it

Which node holds the volume locally:

```sh
kubectl get pvst <pv-name> -o jsonpath='{.status.localAttachNode}'
kubectl get pvst <pv-name> -o jsonpath='{.status.publishedNodes}'
```

A publication with `local: true` in `status.publishedNodes` is a local attach. While `status.localAttachNode` is set, the export serves no I/O to remote initiators, and export resync keeps the namespace disabled after an agent restart or a storage-node reboot.

The device-mapper target on the storage node:

```sh
sudo dmsetup ls | grep pillar-local-
sudo dmsetup table pillar-local-<16 hex>
```

A publish to another node that is blocked by the local claim shows up on the `VolumeAttachment` and in the pod's events:

```sh
kubectl get volumeattachment
kubectl describe volumeattachment <name>
```

The attach error contains `FailedPrecondition` and `backend device ... is still held on the storage node (local attach in use)`. A local stage refused because the export was still enabled shows `network export of volume ... is still serving remote initiators` in the pod's events and the node plugin log.

See [Fencing and consistency](/docs/explanation/fencing-and-consistency/#local-attach) for why each side checks the other.
