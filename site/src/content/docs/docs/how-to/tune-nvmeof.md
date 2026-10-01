---
title: Tune NVMe-oF/TCP settings
description: Set the NVMe-oF/TCP port, host ACL, queue size, in-capsule data size, maximum data transfer size, ctrlLossTmo and reconnectDelay per PillarProtocol, binding or volume in pillar-csi.
sidebar:
  order: 5
---

A `PillarProtocol` holds the NVMe-oF/TCP settings under `spec.protocol.nvmeofTcp`. pillar-csi ships one default of its own, a 4 MiB `maxDataTransferSize`; every other tuning field you leave unset keeps the Linux kernel default. For iSCSI settings under `spec.protocol.iscsi`, see [Configure iSCSI](/docs/how-to/configure-iscsi/).

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nvmeof-default
spec:
  protocol:
    nvmeofTcp:
      port: 4420
      acl: true
      maxQueueSize: 128
      ctrlLossTmo: 600
      reconnectDelay: 10
```

## Fields

| Field | Side | Default | Allowed | Per binding or per volume |
|---|---|---|---|---|
| `port` | target | `4420` | 1 to 65535 | no (structural) |
| `acl` | target | `false` | `true`, `false` | no (structural) |
| `maxQueueSize` | initiator | unset: kernel default 128 | 16 to 1024 | yes |
| `inCapsuleDataSize` | target | unset: transport default, 16384 for TCP on 4 KiB pages | 1024 or more | yes |
| `maxDataTransferSize` | target, initiator fallback | unset: 4194304 (4 MiB) | `0` (no limit) or a power of two from 8192 to 1073741824 | yes |
| `ctrlLossTmo` | initiator | unset: kernel default 600 seconds | 0 or more | yes |
| `reconnectDelay` | initiator | unset: kernel default 10 seconds | 0 or more | yes |

`port` is the TCP port the `nvmet` target listens on. The agent binds it on the storage node address that the controller resolves from the `PillarAgent`; `PillarProtocol` has no address field.

`acl: true` makes the export admit only the host NQNs of nodes the volume is published to. The controller grants a node's NQN on `ControllerPublishVolume` and revokes it on unpublish. `acl: false` sets `attr_allow_any_host`, so any initiator that can reach the port can connect. Turn on `acl` on any network you do not fully trust.

`maxQueueSize` is the `queue_size` option the worker passes to the kernel when it connects. It sets the depth of every I/O queue on that connection.

`inCapsuleDataSize` is the target port's `param_inline_data_size`. The kernel keeps one value per listening port, so every volume exported on the same storage node address and port shares it. If a volume asks for a size different from the one the port already has, the agent refuses the export with `FailedPrecondition` and names `param_inline_data_size`. Leave the field unset unless every volume on that port uses the same value.

`maxDataTransferSize` is the largest amount of data, in bytes, one NVMe I/O command may carry (MDTS). See [Maximum data transfer size](#maximum-data-transfer-size).

`ctrlLossTmo` is how many seconds a worker keeps trying to reconnect after it loses the target before the kernel removes the NVMe controller. `reconnectDelay` is the wait in seconds between attempts. See [Maintain storage and worker nodes](/docs/how-to/node-maintenance/) for how these two values decide whether a storage node reboot is survivable.

## When a change takes effect

The controller resolves the effective settings once, in `CreateVolume`, and records them in the volume's `PillarVolumeState` under `spec.resolved`. It also writes the initiator values into the PersistentVolume's volume attributes, which the node reads when it connects. A change to a `PillarProtocol` therefore applies to volumes provisioned after the change. Existing volumes keep the values they were created with.

To see what a volume actually uses (the `PillarVolumeState` has the same name as the PV):

```sh
kubectl get pvst <pv-name> -o jsonpath='{.spec.resolved.protocol.nvmeofTcp}'
kubectl get pv <pv-name> -o jsonpath='{.spec.csi.volumeAttributes}'
```

The initiator settings appear in the volume attributes as `pillar-csi.bhyoo.com/nvmeof-max-queue-size`, `pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo` and `pillar-csi.bhyoo.com/nvmeof-reconnect-delay`, and only when set. `pillar-csi.bhyoo.com/nvmeof-max-data-transfer-size` is always present for volumes provisioned by this release or later.

## Maximum data transfer size

A Linux `nvmet` target without a configured limit advertises MDTS 0, meaning no limit. Recent host kernels then send up to 32 MiB in one command. For every command the target allocates the command's scatterlist in one piece: 256 KiB of physically contiguous memory for 32 MiB. On a storage node whose memory is fragmented that allocation fails, the target answers with an internal error, the host retries and then fails the write. XFS only logs `writeback error`, and an application that does not call `fsync` later reads zeros. At 4 MiB a command needs 32 KiB of contiguous memory, which is why pillar-csi limits commands to 4 MiB unless you set the field.

pillar-csi enforces the limit in two places:

- **Target, Linux 7.1 and later.** The agent writes the limit to the port's `param_mdts`, so every host connecting through the port sees it in Identify Controller and splits larger requests. Like `inCapsuleDataSize`, the value belongs to the listening port that every volume on the same storage node address and port shares, and the kernel accepts a change only while no volume is exported on the port. Unlike `inCapsuleDataSize`, a volume's value is an upper bound, because a port advertising a smaller limit only makes commands smaller. A volume is refused with `FailedPrecondition` naming `param_mdts` only when the port already advertises a larger limit than the volume's value; a volume asking for a larger limit, or for `0`, joins a port with a smaller one. A port that advertises no limit, for example one enabled by volumes created before this setting existed, accepts a volume with any limit, because the worker then applies it itself. When the agent restores the exports of a port, for example after the storage node rebooted, it sets the port to the smallest non-zero limit among them before exporting any, so volumes that shared a port with different limits come back whatever their order.
- **Worker fallback.** On every `NodeStageVolume`, pillar-node reads the controller's MDTS with an Identify Controller command. If the target advertises any limit, the node leaves the device alone. If it advertises none (MDTS 0) and `maxDataTransferSize` is not `0`, the node sets `queue/max_sectors_kb` of the namespace block device, including each multipath path device, to `maxDataTransferSize / 1024`, reads it back and fails the stage if the kernel did not take it. This also runs when kubelet repeats `NodeStageVolume` for a volume that is already staged. Volumes provisioned before the setting existed have no `pillar-csi.bhyoo.com/nvmeof-max-data-transfer-size` attribute and get the 4 MiB default.
- **Staged volumes.** A volume that stays staged gets no new `NodeStageVolume`, for example while pillar-node is upgraded or after the kernel reconnects a dropped controller. pillar-node therefore re-checks every NVMe-oF volume it has staged in the background, starting right after it begins serving CSI calls and then every 30 seconds. A device already at or below the limit is left alone without an admin command. Otherwise pillar-node asks the live controllers for their MDTS, with a 5 second timeout per Identify Controller command, and applies the same rule as above. A volume whose controller is connecting or resetting is retried on the next check. A target found to advertise a limit is asked again once a check found its controller disconnected or not live, and at least every 5 minutes, since a reconnected controller keeps its name but the target may have restarted without the limit. It uses the limit recorded when the volume was staged, or the 4 MiB default for volumes staged by an older release, and takes the same volume lock as `NodeStageVolume`; for a volume staged before pillar-node recorded volume IDs it reads the ID from kubelet's staging directory, and skips the volume with a warning if it cannot. pillar-node logs at info level when it limits a device, and logs each failure at error level once per device with its volume ID, device and cause; pillar-node keeps running.

On storage nodes older than Linux 7.1 the kernel has no `param_mdts`. The export still succeeds, the agent logs once that the target cannot advertise a limit, and the worker fallback applies the limit instead. Set the field to `0` only if you want the old behavior of no limit on either side.

The smallest limit is 8192 bytes. `param_mdts` stores the limit as a power of two of 4 KiB pages, and its value 0 already means no limit, so a single 4 KiB page cannot be expressed.

The worker cannot set `queue/max_sectors_kb` below one memory page (`PAGE_SIZE / 1024`). Most workers use 4 KiB pages, but arm64 workers can use 16 KiB or 64 KiB pages. On such a worker a non-zero `maxDataTransferSize` smaller than the page size, for example 8192 on 16 KiB pages, fails `NodeStageVolume` with `InvalidArgument` naming the value and the page size before the node connects. pillar-node does not round the value up. Check the page size with `getconf PAGESIZE` and use at least that value, or `0`.

To check the result on a worker, read the limit of the volume's namespace device:

```sh
cat /sys/block/nvme0n1/queue/max_sectors_kb
```

## Override per binding or per volume

The five tunable fields can be overridden with the same nested shape at two more layers. On a `PillarStorageClass`:

```yaml
spec:
  overrides:
    protocol:
      nvmeofTcp:
        maxQueueSize: 64
```

On a PVC, as a YAML document in an annotation:

```yaml
metadata:
  annotations:
    pillar-csi.bhyoo.com/protocol: |
      nvmeofTcp: {ctrlLossTmo: 1800}
```

`port` and `acl` are rejected in both places. A PVC that sets one fails provisioning with a message such as `pillar-csi.bhyoo.com/protocol: nvmeofTcp.acl is structural and cannot be set per volume`. To use a different port or ACL policy, create a second `PillarProtocol` and a `PillarStorageClass` that references it.

[Per-volume overrides](/docs/how-to/volume-overrides/) covers precedence across all layers.

## Check the result on a worker

pillar-node connects through the kernel's `/dev/nvme-fabrics` interface and does not need `nvme-cli` on the host. To see a connection's settings on a worker, read the controller's sysfs entries. Each `nvmeN` directory is one connection, and its `subsysnqn` file names the volume's subsystem:

```sh
grep . /sys/class/nvme/nvme*/subsysnqn
cat /sys/class/nvme/nvme0/ctrl_loss_tmo
cat /sys/class/nvme/nvme0/reconnect_delay
```

pillar-csi publishes no benchmark results. Measure with your own workload before and after a change.
