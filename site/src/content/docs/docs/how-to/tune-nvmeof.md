---
title: Tune NVMe-oF/TCP settings
description: Set the NVMe-oF/TCP port, host ACL, queue size, in-capsule data size, ctrlLossTmo and reconnectDelay per PillarProtocol, binding or volume in pillar-csi.
sidebar:
  order: 5
---

A `PillarProtocol` holds the NVMe-oF/TCP settings under `spec.protocol.nvmeofTcp`. pillar-csi does not ship its own performance defaults: every tuning field you leave unset keeps the Linux kernel default. For iSCSI settings under `spec.protocol.iscsi`, see [Configure iSCSI](/docs/how-to/configure-iscsi/).

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
| `ctrlLossTmo` | initiator | unset: kernel default 600 seconds | 0 or more | yes |
| `reconnectDelay` | initiator | unset: kernel default 10 seconds | 0 or more | yes |

`port` is the TCP port the `nvmet` target listens on. The agent binds it on the storage node address that the controller resolves from the `PillarAgent`; `PillarProtocol` has no address field.

`acl: true` makes the export admit only the host NQNs of nodes the volume is published to. The controller grants a node's NQN on `ControllerPublishVolume` and revokes it on unpublish. `acl: false` sets `attr_allow_any_host`, so any initiator that can reach the port can connect. Turn on `acl` on any network you do not fully trust.

`maxQueueSize` is the `queue_size` option the worker passes to the kernel when it connects. It sets the depth of every I/O queue on that connection.

`inCapsuleDataSize` is the target port's `param_inline_data_size`. The kernel keeps one value per listening port, so every volume exported on the same storage node address and port shares it. If a volume asks for a size different from the one the port already has, the agent refuses the export with `FailedPrecondition` and names `param_inline_data_size`. Leave the field unset unless every volume on that port uses the same value.

`ctrlLossTmo` is how many seconds a worker keeps trying to reconnect after it loses the target before the kernel removes the NVMe controller. `reconnectDelay` is the wait in seconds between attempts. See [Maintain storage and worker nodes](/docs/how-to/node-maintenance/) for how these two values decide whether a storage node reboot is survivable.

## When a change takes effect

The controller resolves the effective settings once, in `CreateVolume`, and records them in the volume's `PillarVolumeState` under `spec.resolved`. It also writes the initiator values into the PersistentVolume's volume attributes, which the node reads when it connects. A change to a `PillarProtocol` therefore applies to volumes provisioned after the change. Existing volumes keep the values they were created with.

To see what a volume actually uses (the `PillarVolumeState` has the same name as the PV):

```sh
kubectl get pvst <pv-name> -o jsonpath='{.spec.resolved.protocol.nvmeofTcp}'
kubectl get pv <pv-name> -o jsonpath='{.spec.csi.volumeAttributes}'
```

The initiator settings appear in the volume attributes as `pillar-csi.bhyoo.com/nvmeof-max-queue-size`, `pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo` and `pillar-csi.bhyoo.com/nvmeof-reconnect-delay`, and only when set.

## Override per binding or per volume

The four tunable fields can be overridden with the same nested shape at two more layers. On a `PillarStorageClass`:

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
