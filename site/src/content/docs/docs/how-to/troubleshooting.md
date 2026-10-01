---
title: Troubleshooting
description: Diagnose pillar-csi problems in the controller, the storage node agent and the worker node plugin, with kubectl commands to confirm each symptom and the fix.
sidebar:
  order: 12
---

pillar-csi reports its state through Kubernetes objects: conditions on the `Pillar*` resources, events on PVCs, and container logs. The examples assume the release name and namespace `pillar-csi`.

## Where to look first

```sh
kubectl get pillaragent,pillarstore,pillarprotocol,pillarstorageclass
kubectl describe pillaragent <name>          # NodeExists, AgentConnected, ExportsReady, Ready
kubectl describe pillarstore <name>          # AgentReady, PoolDiscovered, BackendSupported, Ready
kubectl describe pillarstorageclass <name>   # StoreReady, ProtocolValid, Compatible, StorageClassCreated, Ready
kubectl describe pvc <name>                  # provisioning, attach and resize events
kubectl get pvst <pv-name> -o yaml           # per-volume state; same name as the PV
```

Logs of the three components:

```sh
kubectl -n pillar-csi logs deploy/pillar-csi-controller -c controller
kubectl -n pillar-csi logs ds/pillar-csi-agent -c agent
kubectl -n pillar-csi logs ds/pillar-csi-node -c node
```

`logs ds/...` reads one pod of the DaemonSet. To read the pod on a given node, list the pods with `kubectl -n pillar-csi get pods -o wide` and pass the pod name.

## Controller

### PVC stays Pending with ProvisioningFailed

The provisioner retries, and each failure adds an event with the controller's message:

```sh
kubectl describe pvc <name>
kubectl get events --field-selector reason=ProvisioningFailed
```

Match the message:

| Message contains | Cause | Fix |
|---|---|---|
| `unsupported PVC annotation` | The PVC has a `pillar-csi.bhyoo.com/` annotation other than `backend`, `protocol`, `filesystem` or `import-zvol`, such as a 0.2 key. | Remove it. Use the YAML documents described in [Override settings](/docs/how-to/volume-overrides/). |
| `pillar-csi.bhyoo.com/import-zvol` or `import of volume ... refused` | A zvol import was refused. | See the refusals in [Import a zvol from another CSI driver](/docs/how-to/import-zvol/#refusals). |
| `is structural and cannot be set per volume` | The annotation sets `pool`, `parentDataset`, `volumeGroup`, `thinPool`, `port` or `acl`. | Remove the field. Create a separate `PillarStore` or `PillarProtocol` instead. |
| `unknown field` | A typo or a field that does not exist at that path. | Fix the key; the message lists the supported ones. |
| `mkfs option ... is not allowed` | An `mkfsOptions` entry is not on the allowlist for that filesystem type. | Remove it; the message lists the allowed flags. |
| `(named via ...) not found` | The `PillarStore`, `PillarProtocol` or `PillarStorageClass` named by the StorageClass does not exist. | Create it, or fix the name. |
| `lvm.provisioningMode thin requires PillarStore ... to set lvm.thinPool` | Thin provisioning without a thin pool. | Set `lvm.thinPool` on the store and in `agent.backends`, or use `linear`. |
| `ResourceExhausted` | The pool or volume group has too little free space. For ZFS the limit is the space available under the store's parent dataset. | Free space, add disks, or request less. |
| `param_inline_data_size is ... but the export requires` | The volume's `inCapsuleDataSize` differs from the value already set on that storage node port. | Use one `inCapsuleDataSize` for every volume on the port, or leave it unset. See [Tune NVMe-oF/TCP](/docs/how-to/tune-nvmeof/). |
| `volume_content_source is not supported` | The PVC has `dataSource` or `dataSourceRef`. pillar-csi does not support snapshots or clones. | Remove the data source. |

A PVC that stays `Pending` without these events usually points at a `PillarStorageClass` or `PillarStore` that is not Ready. Check those next.

### PillarStorageClass is not Ready

```sh
kubectl describe pillarstorageclass <name>
```

`StoreReady=False` with `PoolNotFound` or `PoolNotReady` means the referenced `PillarStore` is missing or not Ready; fix the store first. `ProtocolValid=False` means the same for the `PillarProtocol`. `StorageClassCreated=False` with `StorageClassError` means the controller could not create or update the generated StorageClass, and the message carries the API error.

### Pod stuck in ContainerCreating: volume published to another node

The pod's events show an attach error with `FailedPrecondition` and `is published to another node`. A `ReadWriteOnce` or `ReadWriteOncePod` volume is published to one node at a time.

```sh
kubectl get volumeattachment
kubectl get pvst <pv-name> -o jsonpath='{.status.publishedNodes}'
```

Stop the pod on the old node and wait until its `VolumeAttachment` is gone. If the old node is down and will not come back soon, see [a worker that went down without a drain](/docs/how-to/node-maintenance/#a-worker-that-went-down-without-a-drain).

### Pod stuck in ContainerCreating: backend device still held on the storage node

The attach error contains `FailedPrecondition` and `backend device ... is still held on the storage node (local attach in use)`. The volume was last attached [locally on the storage node](/docs/how-to/local-attach/), and the storage node still holds its `pillar-local-*` device-mapper target, so the export stays disabled for other nodes.

```sh
kubectl describe volumeattachment <name>
kubectl get pvst <pv-name> -o jsonpath='{.status.localAttachNode}'
```

The publish goes through once the storage node unstages the volume. If the old pod on the storage node is still terminating, wait for it. If the storage node's kubelet is down, the old container may still be writing; bring the kubelet back or reboot the storage node. Do not remove the device-mapper target by hand.

### Deleting a Pillar resource hangs

The object stays in `Terminating` with a `pillar-csi.bhyoo.com/*-protection` finalizer. Its `Ready` condition is `False` with reason `DeletionBlocked`, and the message lists what still depends on it.

```sh
kubectl get pillarstore <name> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

Delete the dependents first, in this order: PVCs, then `PillarStorageClass`, then `PillarStore` and `PillarProtocol`, then `PillarAgent`. A volume blocks deletion until its PV and `PillarVolumeState` are both gone. Do not remove the finalizer by hand: it keeps a store or agent from disappearing while volumes still live on it.

### A volume's export is not reconciled

Each `PillarVolumeState` has an `ExportReconciled` condition:

```sh
kubectl get pvst -o custom-columns='NAME:.metadata.name,AGENT:.spec.agentRef,EXPORT:.status.conditions[?(@.type=="ExportReconciled")].reason'
```

| Reason | Meaning | Fix |
|---|---|---|
| `Reconciled` | The export and ACL on the storage node match the record. | Nothing. |
| `AgentUnavailable` | The controller cannot reach the agent. | See the agent section below. |
| `ReconcileFailed` | An agent call failed. | Read the condition message and the agent log. |
| `StaleGeneration` | The attempt used an outdated fencing generation. | The controller retries with the current one. |
| `ExportSpecMissing` | A legacy volume without `status.exportSpec`. | Follow [Recover a volume stuck at ExportSpecMissing](/docs/how-to/recover-export-spec/). |

## Agent

### No agent pod on the storage node

The agent DaemonSet runs only on nodes labelled `pillar-csi.bhyoo.com/agent-node=true`. The controller adds that label when a `PillarAgent` names the node in `spec.nodeRef`.

```sh
kubectl get nodes -l pillar-csi.bhyoo.com/agent-node=true
kubectl describe pillaragent <name>
```

`NodeExists=False` with `NodeNotFound` means `spec.nodeRef.name` does not match a Node name.

### Agent pod in CrashLoopBackOff

```sh
kubectl -n pillar-csi logs <agent-pod> -c agent --previous
```

| Log message contains | Fix |
|---|---|
| `backends: at least one backend is required` | `agent.backends` is empty, which is the chart default. Add a `zfs` or `lvm` entry that matches your `PillarStore`. |
| `duplicate pool/VG` | Two `agent.backends` entries name the same pool or volume group. Keep one. |
| `unknown field` | An `agent.backends` entry has a key the agent does not accept, such as a 0.2 key (`type`, `vg`, `parent`). Use the 0.3 shape. |

### PillarAgent: AgentConnected is False or degraded

```sh
kubectl get pillaragent <name> \
  -o jsonpath='{.status.conditions[?(@.type=="AgentConnected")]}'
```

With reason `HealthCheckFailed`, the controller could not complete a gRPC health check on the agent's port (`9500` by default). The message carries the network error, for example `connection refused`. Check that the agent pod is running on that node, that the node's firewall admits TCP 9500 from the controller's node, and that `agent.hostNetwork` is `true`.

With reason `TLSHandshakeFailed`, mTLS is on and the certificates do not match. See [Configure mTLS](/docs/how-to/configure-mtls/).

With `AddressNotResolved`, the node has no address of the type in `spec.nodeRef.addressType`, or none inside `spec.nodeRef.addressSelector`.

`AgentConnected` can also be `True` with reason `AgentDegraded`. The agent answers but reports a degraded subsystem, for example a missing `nvmet` module or an unmounted configfs. The agent's init container runs `modprobe` for `nvmet`, `nvmet_tcp`, `target_core_mod`, `target_core_iblock` and `iscsi_target_mod` against the host's `/lib/modules`, so the modules must exist in the host kernel.

If iSCSI volumes do not provision on a storage node, check that the agent lists `iscsi` among its protocols. It does so only when the LIO iSCSI target works on the node, which needs `target_core_mod`, `target_core_iblock` and `iscsi_target_mod`:

```sh
kubectl get pillaragent <name> -o jsonpath='{.status.capabilities.protocols}'
```

### PillarStore is not Ready: pool not discovered

```sh
kubectl describe pillarstore <name>
kubectl get pillaragent <agent> -o jsonpath='{.status.discoveredPools}'
```

| `PoolDiscovered` reason | Cause | Fix |
|---|---|---|
| `WaitingForAgentData` | The agent has not reported pools yet, often because it is not connected. | Fix `AgentConnected` first. |
| `PoolNotFound` | The pool or volume group is not in the agent's `discoveredPools`. | Add it to `agent.backends`. If the list is empty, check that `agent.privileged` is `true` (the default); without it the container cannot open `/dev/zfs` or `/dev/mapper/control`. Check that the pool is imported or the VG is active on the host. |
| `BackendLayoutMismatch` | The store's `zfs.parentDataset` or `lvm.thinPool` differs from the agent's entry. The message names both values. | Make the `PillarStore` and the `agent.backends` entry agree. `CreateVolume` is refused until they do. |

### PillarAgent stays not Ready after a restart

```sh
kubectl get pillaragent <name> \
  -o jsonpath='{.status.conditions[?(@.type=="ExportsReady")]}'
```

`ExportRestorePending` is normal for a short time after the agent starts: the agent waits for the controller to send its full export list. `ExportRestoreFailed` means the last attempt failed. The controller retries, and its log shows the error. See [Maintain storage and worker nodes](/docs/how-to/node-maintenance/).

## Node

### Pod cannot mount: ControllerPublish or NodeStage fails

Read the pod events first, then the node plugin log on that worker:

```sh
kubectl describe pod <pod>
kubectl -n pillar-csi logs <node-pod> -c node
```

| Message contains | Cause | Fix |
|---|---|---|
| `CSINode "<node>" is missing annotation "pillar-csi.bhyoo.com/nvmeof-host-nqn"` | The node plugin has not published the worker's host NQN yet. | Check that the node plugin pod runs on that worker. |
| `CSINode "<node>" is missing annotation "pillar-csi.bhyoo.com/iscsi-initiator-iqn"` | The node plugin has not published the worker's iSCSI initiator IQN. It skips the IQN when `iscsi_tcp` was not loaded at its start. | Check that the node plugin pod runs on that worker and that its log has no `iSCSI initiator disabled` line. If it has, load `iscsi_tcp` on the host and restart the pod. |
| `CSINode "<node>" not found` | The node plugin never registered on that worker. | Check the `node-driver-registrar` container of the node plugin pod. |
| an NVMe connect error, or a timeout waiting for the device | The worker cannot reach the storage node's NVMe/TCP port (`4420` by default), or `nvme_tcp` is not loaded. | Open the port on the storage node's firewall. On the worker, check that `/sys/module/nvme_tcp` exists; the init container only runs `modprobe` against the host's `/lib/modules`. |
| `the iSCSI initiator is disabled on this node because kernel module iscsi_tcp was not loaded when pillar-node started` | `iscsi_tcp` was missing when the node plugin started. | Load `iscsi_tcp` on the host, list it in `/etc/modules-load.d/`, and restart the node plugin pod. |
| `iscsi Attach: login to <target> at <address>:<port>` followed by a connection or timeout error | The worker cannot reach the storage node's iSCSI port (`3260` by default). | Open the port on the storage node's firewall. With `acl: true`, check that the worker's `pillar-csi.bhyoo.com/iscsi-initiator-iqn` annotation is set. |
| `create iSCSI initiator (netlink netns ...)` or `start iSCSI initiator` in the node plugin log, and the node plugin pod restarts | The node plugin cannot open the kernel's `NETLINK_ISCSI` socket, which exists only in the host's initial network namespace. | Keep `hostNetwork: true` on the node plugin. On Kind or other nodes that run in containers, set `node.iscsi.netlinkNetnsPath`; see [Prerequisites](/docs/reference/prerequisites/#kubernetes-and-helm). |
| `read iSCSI initiator IQN` in the node plugin log, and the node plugin pod restarts | `/etc/iscsi/initiatorname.iscsi` on the host holds an `InitiatorName=` that is not a valid iSCSI name, or the file cannot be read or written. The node plugin never overwrites an existing name. | Fix the `InitiatorName=` line, or remove the file so the node plugin generates a new IQN. |
| `resize2fs` or `xfs_growfs` | Growing the filesystem after an expansion failed. | See [Expand a volume](/docs/how-to/expand-volume/#failures). |

A worker without `nvme_tcp` loaded also lacks the topology key `pillar-csi.bhyoo.com/nvmeof` on its CSINode:

```sh
kubectl get csinode <node> -o yaml
```

### Pod teardown hangs: stage record missing

The node plugin log shows `stage state for "<volume>" is missing but "<path>" is still mounted; refusing to report the volume unstaged`. The plugin keeps a record of each staged volume in `/var/lib/pillar-csi/node/` on the worker and needs it to know which session to disconnect. Without it, it leaves the mount and session alone.

Stage the volume again on the same node, for example by starting a pod that uses it there. That rewrites the record, and the next unstage completes. Check that the node plugin's hostPath for `/var/lib/pillar-csi/node` was not removed or replaced.

### I/O errors after a storage node reboot

The application gets `Input/output error`, and on the worker:

```sh
dmesg -T | grep -i nvme
```

If the log shows reconnect attempts followed by the controller being removed, the outage lasted longer than the volume's `ctrlLossTmo` (600 seconds when unset). The kernel removed the device and the filesystem shut down. Stop every pod that uses the volume, wait until its `VolumeAttachment` is gone, and start the workload again. To avoid it next time, see [Maintain storage and worker nodes](/docs/how-to/node-maintenance/).

For an iSCSI volume, the outage lasted longer than its `replacementTimeout` (120 seconds when unset). The kernel failed the queued I/O. pillar-node logs in again once the target is back, but a filesystem that saw the errors may have shut down or turned read-only. Restart the workload the same way. Check the session state with `grep . /sys/class/iscsi_session/session*/state`.

If the log shows `identifiers changed for nsid`, the namespace came back with a different identity. Volumes exported by 0.3.0 keep a fixed identity across reboots. An export created by 0.2.0 or earlier that was lost before an upgraded agent recorded its identity cannot keep it. Restart the workload as above.

## Reporting a bug

If none of this matches, open an issue at [github.com/isac322/pillar-csi/issues](https://github.com/isac322/pillar-csi/issues) with the pillar-csi version, the conditions of the affected `Pillar*` objects, the related PVC events, and the controller, agent and node logs.
