---
title: Fencing and consistency
description: "How pillar-csi fences stale operations with publication generations, keeps RWO volumes on one node, and pins NVMe namespace identity across storage reboots."
sidebar:
  order: 2
---

A CSI driver that exports block devices over the network can corrupt data in ways a local driver cannot. Two nodes can mount the same ext4 filesystem. A delayed request from an old controller can re-grant access that a newer request revoked. A reboot can change a namespace's identity under a connected host. This page describes how pillar-csi prevents each of these, and why it refuses to proceed when it cannot prove an operation is safe.

The mechanisms live in `internal/csi/volume_fencing.go` (controller side), `internal/agent/fencing.go` and `internal/agent/nvme_identity.go` (storage-node side).

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

A controller can issue a request and then lose its leader lease, pause, or sit behind a slow network while a new leader handles the same volume. When the old request finally reaches the agent, it must not undo the newer work. pillar-csi solves this with a fencing token on every agent call that changes a volume: backend create, expand and delete; export and unexport; initiator grant and revoke; and export resync.

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

The kernel target assigns a random UUID and subsystem serial each time a namespace is created, and a storage-node reboot recreates every namespace. The agent therefore sets the identity explicitly before it enables a namespace and reads it back to confirm. The namespace UUID, the NGUID and the subsystem serial are derived from the subsystem NQN, which is derived from the volume ID. The same volume gets the same identity after an agent restart, after a storage-node reboot, and after the agent's state directory is lost. No CRD or RPC field is involved (`internal/agent/nvmeof/identity.go`).

The agent never changes the identity of a namespace that is enabled, because connected hosts have already cached it and `nvmet` cannot change it without disabling the namespace. Exports created by pillar-csi 0.2.0 or earlier carry kernel-random identifiers. When the agent finds such a live export, it keeps the identity and records it under `/var/lib/pillar-csi/agent/nvmet-identity/`. Later re-creations reproduce the recorded identity. The record is deleted when the volume is unexported, after which the derived identity applies.

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
- [Troubleshooting](/docs/how-to/troubleshooting/)
