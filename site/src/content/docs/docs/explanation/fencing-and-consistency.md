---
title: Fencing and consistency
description: "How pillar-csi fences stale operations with publication generations, keeps block RWO volumes on one node, tracks NFS RWX ACL membership, and preserves export state across storage reboots."
sidebar:
  order: 2
---

A CSI driver that exports block devices or NFS filesystems over the network can corrupt data when access is not fenced. Two nodes can mount the same ext4 filesystem, or a stale request can re-grant access after a revoke. NFS intentionally permits concurrent RWX mounts, so its node-IP ACL membership and owned export state must remain exact. This page describes how pillar-csi prevents unsafe block sharing and fails closed when it cannot prove an operation is safe.

## One writer per volume

The controller records every node a volume is published to in the `status.publishedNodes` list of the volume's `PillarVolumeState`. It writes that record before it asks the agent to grant the node access, using a compare-and-swap on the object's `resourceVersion`. Because the record lives in the Kubernetes API, it survives controller restarts and leader changes.

`ControllerPublishVolume` checks the new request against every existing record:

| Existing publication | New request | Result |
| --- | --- | --- |
| RWO or RWOP on node A | any mode on node B | `FAILED_PRECONDITION` |
| ROX on node A | ROX on node B | allowed |
| NFS RWX on node A | NFS RWX on node B | allowed; each node IP is tracked in the export ACL when enabled |
| any mode on node A | a different mode on node A | `ALREADY_EXISTS` |
| node A, unpublish still revoking | node A again | `ABORTED`, retry |

Kubernetes retries a failed block publish, so a pod scheduled to a second node waits until the first node unpublishes. NFS `ReadWriteMany` is the supported exception: concurrent publications are expected, but an empty ACL set denies volume data even if an unauthorized mount reaches an empty backing stub.

Unpublish removes a record only after the agent confirms the grant is revoked. If the revoke fails, the record stays and the call returns an error; if the `PillarAgent` object is gone, it returns `FAILED_PRECONDITION`. Either way the volume remains unavailable to other single-writer publishers until Kubernetes retries and the revoke succeeds. `DeleteVolume` also refuses to run while any publication is recorded.

The publication record is enforced by the controller. On the wire, `acl: true` enforces protocol-specific access: NVMe-oF admits published host NQNs, iSCSI admits published IQNs, and NFS admits published numeric node IPs. The default `acl: false` leaves the protocol open to reachable clients. ACLs are access control, not encryption; NFS RPC TLS is not offered.

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

### iSCSI LUN identity

An iSCSI initiator identifies a disk by the SCSI identifiers LUN 0 reports. LIO derives the disk's NAA WWN from the backstore's unit serial, which the kernel would otherwise pick at random when the backstore is created. The agent writes a unit serial derived from the target IQN, which is derived from the volume ID, and reads it back. The same volume therefore presents the same disk identity after an agent restart, a storage-node reboot, a local attach round trip or the loss of the agent's state, with no record on disk. The agent refuses to change the serial of a LUN that is exported and reports an error instead.

## Local attach

A volume provisioned with [`localAttach`](/docs/how-to/local-attach/) is used on the storage node through the backend device itself, and on every other node through the network export. This fencing path applies only to block zvol/LV volumes. NFS never uses localAttach: even on the storage node it uses the NFSv4.2 network mount and its normal ACL/recovery state.

**The export is off while the volume may be local.** A local publish first commits `status.localAttachNode` on the `PillarVolumeState`, in the same compare-and-swap that reserves the publication, and then asks the agent to disable the export for remote initiators. For NVMe-oF/TCP the agent writes `0` to the namespace's `enable` file and reads it back. For iSCSI the agent disables the portal group, which ends its sessions, and removes LUN 0 and its backstore. A remote host that kept its session, because it was force-detached and never disconnected, gets I/O errors instead of reaching the disk. Export resync sends the same flag, so the export comes back disabled after an agent restart or a storage-node reboot.

**A local stage holds the device.** The node plugin opens the backend device through a device-mapper linear target, `pillar-local-<16 hex>`, and mounts the filesystem on that target. The kernel holds the backend device exclusively for as long as the target exists. The target is not tied to the node plugin or the kubelet process; it disappears only when the volume is really unstaged or the storage node reboots.

**Both sides take the same exclusive claim.** The kernel grants an exclusive claim on a block device to one holder at a time, and both sides use it.

- The node plugin takes the claim first, by creating the device-mapper target, then reads the export state: the enable state of every nvmet namespace of the volume's subsystem, or whether LUN 0 of the volume's iSCSI target exists. If the export can still serve I/O, it backs off and fails the stage with `FAILED_PRECONDITION`. A stage that fails for any reason after the claim unmounts what it staged and then removes the target; if the unmount fails, it keeps the target, so no mount is left on a removed device. An unstage that finds no stage record removes a claim left for the volume.
- The agent enables the export only while it holds its own claim: it opens the backend device with `O_EXCL`, keeps it open while it enables the export and reads it back (the write of `1` to the namespace's `enable` file, or the re-creation of the iSCSI backstore and LUN 0), and closes it afterwards. If the device is already held, for example by the storage node's `pillar-local-*` target, the agent refuses with `FAILED_PRECONDITION` and `backend device ... is still held on the storage node (local attach in use)`, and the export stays disabled. The agent enables the export the same way when an agent restart or export resync would enable it.

The two claims exclude each other. While the agent is enabling the export, the node plugin cannot create its target. Once the agent has released its claim, the export already reads enabled, so a node plugin that claims the device afterwards sees that and backs off before it mounts anything. The agent never enables the export while the storage node holds the device, and the node plugin never mounts while the export is enabled.

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
