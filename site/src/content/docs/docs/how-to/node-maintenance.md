---
title: Maintain storage and worker nodes
description: Reboot a pillar-csi storage node or drain a worker without losing NVMe-oF/TCP, iSCSI or NFS volumes, using protocol recovery, export restore status and a safe drain order.
sidebar:
  order: 9
---

pillar-csi does not replicate data. While a storage node is down, every volume it exports is unavailable. This page covers how to take a storage node or a worker node down and bring it back so that block and NFS workloads resume when their protocol recovery limits allow.

## How workers ride out a storage node outage

When a worker loses its NVMe-oF/TCP connection, the Linux kernel keeps the NVMe controller and retries the connection every `reconnectDelay` seconds. If the target comes back before `ctrlLossTmo` seconds pass, the kernel reconnects and I/O continues. Applications stall for the length of the outage.

If `ctrlLossTmo` runs out first, the kernel removes the controller and its block device. The filesystem on it shuts down and applications get I/O errors. Recovering from that takes a restart of the workload (see [After a timeout](#after-a-timeout)).

`ctrlLossTmo` and `reconnectDelay` come from the volume's `PillarProtocol` and any overrides. When nothing sets them, the kernel defaults apply: 600 seconds and 10 seconds. The chart sets no default of its own. The values are fixed when the volume is created, so check what each volume actually uses:

```sh
kubectl get pv <pv-name> -o jsonpath='{.spec.csi.volumeAttributes}'
```

Look for `pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo` and `pillar-csi.bhyoo.com/nvmeof-reconnect-delay`. If a key is absent, the kernel default applies. A new value on the `PillarProtocol` affects only volumes created afterwards; see [Tune NVMe-oF/TCP settings](/docs/how-to/tune-nvmeof/).

### iSCSI volumes

When a worker loses an iSCSI connection, pillar-node logs in to the target again every few seconds and keeps trying for as long as the volume is staged. The kernel holds the volume's I/O for `replacementTimeout` seconds, 120 by default. If the target comes back within that time, I/O continues. If not, the kernel fails the queued I/O and the filesystem sees errors, even though pillar-node reconnects the session later. The filesystem may shut down or turn read-only, and the workload then needs a restart (see [After a timeout](#after-a-timeout)).

Check a volume's value in the same volume attributes: `pillar-csi.bhyoo.com/iscsi-replacement-timeout`. If the key is absent, 120 seconds applies. See [Configure iSCSI](/docs/how-to/configure-iscsi/).

pillar-node does the iSCSI reconnecting itself, so it must be running for a session to recover. If pillar-node restarts, it adopts the pillar-csi sessions already on the node and continues.

## What the storage node does when it comes back

The agent keeps the state it needs on the storage node's own disk, in hostPath directories under `/var/lib/pillar-csi/agent/`, so it survives reboots:

- `generations/` holds the fencing marks that reject stale requests.
- `nvmet-identity/` holds namespace identities recorded for older exports.

After a reboot the kernel block targets and NFS export state are empty. The agent starts with exports gated and reports it through the `ExportsReady` condition on its `PillarAgent`:

| `ExportsReady` | Reason | Meaning |
|---|---|---|
| `False` | `ExportRestorePending` | The agent restarted and waits for the controller to send its full owned export list. |
| `False` | `ExportRestoreFailed` | The last restore attempt failed; the controller retries. |
| `True` | `ExportsServing` | Every owned block and NFS export is back and the agent serves them. |

The controller sends every export of that storage node in one request. The agent restores block identities and NFS root/child dataset exports, including ACL membership, readonly, squash and version, before reporting readiness. A reconnecting worker therefore meets either a refused connection or its complete export. Foreign NFS exports are never stopped or reconfigured.

## Reboot a storage node

1. Estimate the outage: boot time plus the time for the agent pod to start. Compare it with the smallest `ctrlLossTmo` among the NVMe-oF volumes and the smallest `replacementTimeout` among the iSCSI volumes on this node.

2. If the outage may exceed that value, stop the workloads that use those volumes first, for example by scaling them to zero. Wait until their `VolumeAttachment` objects are gone:

   ```sh
   kubectl get volumeattachment
   ```

3. If other pods run on the storage node, drain it. The agent is a DaemonSet pod and stays until the node shuts down:

   ```sh
   kubectl drain <storage-node> --ignore-daemonsets --delete-emptydir-data
   ```

4. Reboot the node. Keep its IP address: each PersistentVolume records the target address and port it was provisioned with, and workers reconnect only to that address.

5. After boot, the agent's init container loads `nvmet`, `nvmet_tcp`, `target_core_mod`, `target_core_iblock` and `iscsi_target_mod` from the host's `/lib/modules`. Wait for the restore:

   ```sh
   kubectl get pillaragent <name> \
     -o jsonpath='{.status.conditions[?(@.type=="ExportsReady")].reason}'
   # ExportsServing
   kubectl get pillaragent <name>
   ```

6. Check that every volume's export matches its record:

   ```sh
   kubectl get pvst -o custom-columns='NAME:.metadata.name,AGENT:.spec.agentRef,EXPORT:.status.conditions[?(@.type=="ExportReconciled")].reason'
   ```

   Every volume on this agent should show `Reconciled`. For `ExportSpecMissing`, follow [Recover a volume stuck at ExportSpecMissing](/docs/how-to/recover-export-spec/).

7. Uncordon the node and scale stopped workloads back up:

   ```sh
   kubectl uncordon <storage-node>
   ```

Restarting only the agent pod, for example during a Helm upgrade, does not remove the kernel exports. Connected workers keep their sessions, and only new exports wait until the controller has re-sent the export list.

### After a timeout

If a worker already gave up (its kernel log shows the controller being removed, and the filesystem shut down), restart the workload so the volume is staged again on a new connection. Stop every pod that uses the volume, wait until the `VolumeAttachment` is gone, then start the workload again. On the worker, look for the kernel messages:

```sh
dmesg -T | grep -i nvme
```

For an iSCSI volume, look for the session and the SCSI disk errors instead, and read the session state:

```sh
dmesg -T | grep -iE 'iscsi|connection|I/O error'
grep . /sys/class/iscsi_session/session*/state
```

## Drain a worker node

A normal drain is safe:

```sh
kubectl drain <worker> --ignore-daemonsets --delete-emptydir-data
```

For each evicted pod, kubelet unmounts the volume. The node plugin then disconnects the NVMe controller, logs out of iSCSI, or unmounts the exact NFS staging path from its durable stage record before it reports the volume unstaged. Finally the controller unpublishes the volume from the node: it removes the block initiator or NFS node IP from the export ACL when ACLs are enabled and updates the publication record.

`ReadWriteOnce` and `ReadWriteOncePod` volumes can be published to one node at a time. NFS `ReadWriteMany` publications may remain on other nodes while one worker drains; only the draining node's NFS client mount and ACL membership are removed. Block replacement pods wait for unpublish with `FailedPrecondition`.

The node plugin keeps a record of each staged volume in `/var/lib/pillar-csi/node/` on the worker. The record survives node plugin restarts and reboots, and unstaging needs it. Do not delete that directory on a worker that still has volumes staged.

### A worker that went down without a drain

If a worker loses power or crashes, its volumes stay published to it, and the controller refuses to publish them to another node. First make sure the node is really off. Then mark it out of service so Kubernetes detaches its volumes (the Kubernetes non-graceful node shutdown feature):

```sh
kubectl taint nodes <worker> \
  node.kubernetes.io/out-of-service=nodeshutdown:NoExecute
```

After the node is back and healthy, remove the taint:

```sh
kubectl taint nodes <worker> node.kubernetes.io/out-of-service-
```
