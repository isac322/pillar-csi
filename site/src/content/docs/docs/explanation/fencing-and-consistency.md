---
title: Fencing and consistency
description: "How pillar-csi fences stale operations with publication generations, keeps RWO volumes on one node, and pins NVMe namespace identity across storage reboots."
sidebar:
  order: 2
---

A CSI driver that exports block devices over the network can corrupt data in ways a local driver cannot. Two nodes can mount the same ext4 filesystem. A delayed request from an old controller can re-grant access that a newer request revoked. A reboot can change a namespace's identity under a connected host. This page describes how pillar-csi prevents each of these, and why it refuses to proceed when it cannot prove an operation is safe.

## One writer per volume

The controller records every node a volume is published to in the `status.publishedNodes` list of the volume's `PillarVolumeState`. It writes that record before it asks the agent to grant the node access, using a compare-and-swap on the object's `resourceVersion`. Because the record lives in the Kubernetes API, it survives controller restarts and leader changes.

`ControllerPublishVolume` checks the new request against every existing record:

| Existing publication | New request | Result |
| --- | --- | --- |
| RWO or RWOP on node A | any mode on node B | `FAILED_PRECONDITION` |
| ROX on node A | ROX on node B | allowed |
| any mode on node A | a different mode on node A | `ALREADY_EXISTS` |
| node A, unpublish still revoking | node A again | `ABORTED`, retry |

Kubernetes retries a failed publish, so a pod scheduled to a second node waits until the first node unpublishes. RWX (`MULTI_NODE_MULTI_WRITER`) is not a supported access mode and is rejected when the PVC is provisioned.

Unpublish removes a record only after the agent confirms the grant is revoked. If the revoke fails, the record stays and the call returns an error; if the `PillarAgent` object is gone, it returns `FAILED_PRECONDITION`. Either way the volume remains unavailable to other single-writer publishers until Kubernetes retries and the revoke succeeds. `DeleteVolume` also refuses to run while any publication is recorded.

The publication record is enforced by the controller. On the wire, the storage node's kernel target enforces it only when the `PillarProtocol` sets `acl: true`: the subsystem then admits only the host NQNs of published nodes. The default is `acl: false`, which sets `allow_any_host` on the subsystem, so any host that can reach the port can connect. Turn ACLs on if other machines share that network.

## Stale-operation fencing

A controller can issue a request and then lose its leader lease, pause, or sit behind a slow network while a new leader handles the same volume. When the old request finally reaches the agent, it must not undo the newer work. pillar-csi solves this with a fencing token on every agent call that changes a volume: backend create, expand and delete; export and unexport; initiator grant and revoke; switching the export in and out of [local attach](#local-attach); and export resync.

The token has two parts:

- The UID of the volume's `PillarVolumeState`. A volume deleted and re-created with the same name gets a new UID, so the UID identifies one lifecycle of that name.
- `status.publicationGeneration`, a counter the controller increments with a compare-and-swap for each operation. Every compare-and-swap is pinned to the UID the operation started with. A controller working on a deleted lifecycle gets `ABORTED` and can never obtain a token for the new one.

The agent keeps one mark per volume: the owning UID, the highest generation it has applied, whether that lifecycle has ended, and the UIDs of earlier lifecycles. It checks each request against the mark and applies the change inside the same per-volume lock, so no newer request can slip in between the check and the change. It rejects with `FAILED_PRECONDITION` any request that:

- carries no token;
- carries a generation lower than the mark;
- comes from a lifecycle that another UID currently owns, or that has already been retired;
- tries to create or grant anything on a lifecycle that has already ended.

The mark is a small JSON file under `/var/lib/pillar-csi/agent/generations/` on the storage node's local disk. The agent writes a temporary file, fsyncs it, renames it over the old mark, and fsyncs the directory before it runs the change, so the mark survives agent restarts and node reboots. An unreadable or corrupt mark is an error; the agent does not guess a value, because a guess could re-admit a stale request. Marks are never removed. If the whole directory is lost, for example because the host path was wiped, the fencing history for that node is lost with it.

The controller creates the `PillarVolumeState` before it creates anything on a storage node. A volume without that object therefore owns nothing on any agent, and deleting it needs no agent call.

## Stable NVMe namespace identity

The Linux NVMe host caches each namespace's identifiers: UUID, NGUID and EUI-64. When it reconnects after the target went away, it compares the new identifiers with the cached ones. If they differ, it logs `identifiers changed for nsid N` and removes the namespace, so I/O fails and the filesystem on it shuts down, even though the reconnect itself succeeded.

The kernel target assigns a random UUID and subsystem serial each time a namespace is created, and a storage-node reboot recreates every namespace. The agent therefore sets the identity explicitly before it enables a namespace and reads it back to confirm. The namespace UUID, the NGUID and the subsystem serial are derived from the subsystem NQN, which is derived from the volume ID. The same volume gets the same identity after an agent restart, after a storage-node reboot, and after the agent's state directory is lost. No CRD or RPC field is involved.

The agent never changes the identity of a namespace that is enabled, because connected hosts have already cached it and `nvmet` cannot change it without disabling the namespace. Exports created by pillar-csi 0.2.0 or earlier carry kernel-random identifiers. When the agent finds such a live export, it keeps the identity and records it under `/var/lib/pillar-csi/agent/nvmet-identity/`. Later re-creations reproduce the recorded identity. The record is deleted when the volume is unexported, after which the derived identity applies.

## Local attach

A volume provisioned with [`localAttach`](/docs/how-to/local-attach/) is used on the storage node through the backend device itself, and on every other node through the network export. Two paths to the same blocks mean two ways to write them, and the publication record alone does not keep them apart. Kubernetes can force-detach a volume from a node whose kubelet is dead while the node's containers keep running and writing. A new publish on another node then succeeds as far as Kubernetes is concerned, and the old writer is still there. pillar-csi fences each path from the other on the storage node itself, where both writers would meet.

**The export is off while the volume may be local.** A local publish first commits `status.localAttachNode` on the `PillarVolumeState`, in the same compare-and-swap that reserves the publication, and then asks the agent to disable the export for remote initiators. For NVMe-oF/TCP the agent writes `0` to the namespace's `enable` file and reads it back. A remote host that kept its session, because it was force-detached and never disconnected, gets I/O errors instead of reaching the disk. Export resync sends the same flag, so the namespace comes back disabled after an agent restart or a storage-node reboot.

**A local stage holds the device.** The node plugin opens the backend device through a device-mapper linear target, `pillar-local-<16 hex>`, and mounts the filesystem on that target. The kernel holds the backend device exclusively for as long as the target exists. The target is not tied to the node plugin or the kubelet process; it disappears only when the volume is really unstaged or the storage node reboots.

**Both sides take the same exclusive claim.** The kernel grants an exclusive claim on a block device to one holder at a time, and both sides use it.

- The node plugin takes the claim first, by creating the device-mapper target, then reads the enable state of every nvmet namespace of the volume's subsystem. If any namespace is enabled, it backs off and fails the stage with `FAILED_PRECONDITION`. A stage that fails for any reason after the claim unmounts what it staged and then removes the target; if the unmount fails, it keeps the target, so no mount is left on a removed device. An unstage that finds no stage record removes a claim left for the volume.
- The agent enables a namespace only while it holds its own claim: it opens the backend device with `O_EXCL`, keeps it open across the write of `1` and the read-back, and closes it afterwards. If the device is already held, for example by the storage node's `pillar-local-*` target, the agent refuses with `FAILED_PRECONDITION` and `backend device ... is still held on the storage node (local attach in use)`, and the namespace stays disabled. The agent enables the namespace the same way when an agent restart or export resync would enable it.

The two claims exclude each other. While the agent is enabling the namespace, the node plugin cannot create its target. Once the agent has released its claim, the namespace already reads enabled, so a node plugin that claims the device afterwards sees that and backs off before it mounts anything. The agent never enables the export while the storage node holds the device, and the node plugin never mounts while the export is enabled.

**Returning to the network is fenced like any other change.** Unpublishing a local attach grants and revokes nothing on the target and leaves `status.localAttachNode` set, so the export stays disabled. Every later publish of the volume over the network asks the agent to re-enable the export under the reservation's fencing generation, before it grants the new node's initiator. The call is a no-op when the export is already enabled, and it is made even when `status.localAttachNode` is empty, because the controller cannot tell a volume that was never local from one whose earlier publish attempt cleared the field and then failed. If the device is still held, the publish fails, and the external-attacher retries it until the storage node lets go. When `status.localAttachNode` was set, the controller then clears it with a compare-and-swap that commits a new generation and asks the agent again at that generation to re-enable the export. The agent's mark then sits above every generation at which the volume was recorded as local, so a resync built from an older snapshot, which would still ask for the export to be disabled, is rejected as stale instead of cutting off the new consumer.

The cost is the usual one of failing closed: a pod that moves off the storage node while the storage node's kubelet is down waits in `ContainerCreating` until that node unstages the volume or reboots.

## Why pillar-csi fails closed

When pillar-csi cannot prove that an operation is safe, it returns an error and keeps its records. A few examples from the code:

- `DeleteVolume` fails with `FAILED_PRECONDITION` when the `PillarAgent` object is missing, because a missing object does not prove the node's resources are gone.
- A failed revoke keeps the publication record, so the volume stays blocked for other single-writer nodes.
- The agent refuses to create any export after a restart until the controller has sent the complete export state for that node, so reconnecting hosts never see a half-configured port.
- A corrupt fencing mark or node stage record is an error; neither the agent nor the node plugin substitutes a default.

The cost is that some failures need an operator. A pod can stay in `ContainerCreating`, or a PVC in `Terminating`, until the storage node is reachable again. A stuck volume can be recovered once the node is back, while two nodes writing to one ext4 filesystem, or a stale request deleting a volume that a new PVC now owns, destroys data.

## Related pages

- [Architecture](/docs/explanation/architecture/)
- [Node maintenance](/docs/how-to/node-maintenance/)
- [Attach volumes locally on the storage node](/docs/how-to/local-attach/)
- [Troubleshooting](/docs/how-to/troubleshooting/)

The code is on GitHub: [controller fencing](https://github.com/isac322/pillar-csi/blob/master/internal/csi/volume_fencing.go), [agent fencing](https://github.com/isac322/pillar-csi/blob/master/internal/agent/fencing.go) and [namespace identity](https://github.com/isac322/pillar-csi/blob/master/internal/agent/nvme_identity.go).
