---
title: Adopt an existing LVM logical volume
description: Hand an existing linear or thin LVM logical volume to pillar-csi through a PVC annotation, keep its data with the PreserveOriginal policy, inspect what the storage node observes, and rebind a retained volume to a new claim.
sidebar:
  order: 14
---

If data already lives in an LVM logical volume (LV) on a pillar-csi storage node, pillar-csi can serve that LV in place instead of copying it into a new volume. You create a PVC with the annotation `pillar-csi.bhyoo.com/import-lv`, which names the LV and pins its LVM UUIDs. `CreateVolume` then adopts that LV instead of creating one. The PV, the `PillarVolumeState`, the NVMe-oF/TCP or iSCSI export and fencing work as for a new volume.

An adopted LV is either preserved or managed. Preserved is the default: pillar-csi never deletes, resizes, formats or checks the LV. Managed turns the LV into an ordinary pillar-csi volume, which `DeleteVolume` destroys.

LV adoption is not in a tagged release yet. It needs controller, node and agent images built from a revision that includes it. Do not create an `import-lv` claim against older images: they do not implement the annotation or the agent RPCs this page uses.

For ZFS zvols, see [Import a zvol from another CSI driver](/docs/how-to/import-zvol/). The two annotations cannot be combined on one claim.

## What import does and does not do

Import is read-only on the storage node. The agent runs `lvs`, a `blkid -p` signature probe and a short `O_RDONLY|O_EXCL` open of the device. It never runs `lvchange`, `lvcreate`, `lvextend`, `lvremove`, `mkfs`, `fsck` or `mount`, and it never changes the volume group or the LV: names, UUIDs, size and activation stay as they are. Backend settings from the store, the class or a `pillar-csi.bhyoo.com/backend` annotation are not applied to an adopted LV.

The two policies differ after the import:

| | `PreserveOriginal` (default) | `Managed` |
|---|---|---|
| Node stage | Mounts the filesystem already on the LV. Never runs `mkfs`, `fsck`, repair or a filesystem resize. A blank LV or a filesystem of another type refuses the stage. | The normal pillar-csi path: formats only a blank device, then mounts. |
| Expansion | Refused by the controller, the agent and the node. | Allowed, like any pillar-csi volume. |
| PVC deleted, PV `reclaimPolicy: Delete` | `ReleaseVolume`: removes the pillar-csi export, checks that the LV is still the pinned one and that nothing on the storage node uses it, then retires the lifecycle. The LV, its data and its thin pool stay. | `DeleteVolume` destroys the LV. |
| PVC deleted, PV `reclaimPolicy: Retain` | The PV, the `PillarVolumeState` and the export stay. pillar-csi still owns the LV; nothing is handed back. | Same. |

`PreserveOriginal` protects the LV from the driver, not from the workload. The kernel and the workload write to the volume as usual: a read-write mount replays the filesystem journal, and the application changes files. After a read-write mount, the LV is not byte-identical to its state before the import.

The policy is fixed at the first `CreateVolume` attempt. pillar-csi records the LV's names, UUIDs and policy in `PillarVolumeState.spec.lvmSource`, and the agent pins the same identity and policy in its durable fence mark for the volume ID `<vg>/<lv>`. A later import of that volume ID that names another LV, other UUIDs, or `Managed` after `PreserveOriginal` is refused, also after the first lifecycle has ended.

## Supported layouts

- The LV must be linear or thin, active, and at least as large as the PVC's request. Import never activates or resizes it.
- An agent backend without a `thinPool` adopts linear LVs only. A backend with a `thinPool` adopts thin LVs of that pool only.
- Snapshots (classic and thin), classic snapshot origins, thin pools and their metadata or data LVs, mirrors, RAID, pvmove and virtual LVs are refused, as is any LV whose type `lvs` does not report clearly.
- The LV must be idle on the storage node. The agent refuses an LV that is mounted, has a device-mapper holder, is exported through LIO or nvmet, or is held open exclusively.

Other LVM layouts, such as cache or VDO LVs, are not supported. Import does not offer every LVM operation either; it adopts one LV as it is.

## Before you start

- The LV is on a pillar-csi storage node, in the volume group of a `PillarStore` with an `lvm` backend. The store's `volumeGroup` and `thinPool` must match the agent's `agent.backends` entry. See [Prepare an LVM storage node](/docs/how-to/prepare-lvm-node/).
- Plan an outage. Every user of the LV must stop, and any other driver must release it, before pillar-csi can take it.
- Back up the data by your usual means. A classic LVM snapshot turns the LV into an origin, and pillar-csi refuses origin LVs, so remove such a snapshot before the import.
- If another CSI driver manages the LV, keep its PV on `Retain` and follow that driver's documentation to remove its claim on the LV without deleting it.

## Adopt one LV

The example adopts the LV `legacy` in the volume group `data-vg` on the storage node `storage-1`, and serves it to the PVC `data-legacy` in the namespace `db`.

### 1. Read the LV's identity

On the storage node, as root:

```sh
lvs -o vg_name,lv_name,vg_uuid,lv_uuid,lv_attr,segtype,pool_lv,origin,lv_size --units b data-vg/legacy
blkid -p -o export /dev/data-vg/legacy
```

Check the output:

- `segtype` is `linear`, or `thin` with `pool_lv` equal to the backend's `thinPool`.
- `origin` is empty.
- The fifth character of `lv_attr` is `a` (active).
- `blkid` reports the filesystem `TYPE`, for example `ext4`. A `PreserveOriginal` volume mounted as `Filesystem` must carry a filesystem of the type the claim requests. For a raw or partitioned device, use `volumeMode: Block`.

Build the annotation value `<vg>/<lv>:<vg_uuid>:<lv_uuid>`:

```sh
lvs --noheadings --separator : -o vg_name,lv_name,vg_uuid,lv_uuid data-vg/legacy \
  | tr -d ' ' | awk -F: '{print $1 "/" $2 ":" $3 ":" $4}'
```

```text
data-vg/legacy:Xk3pQm-1aB2-c3D4-e5F6-g7H8-i9J0-Kl1mNo:Rt7uVw-2cD3-e4F5-g6H7-i8J9-k0L1-Mn2oPq
```

The UUIDs make the adoption specific to this LV. If the name later points at a renamed or re-created LV, the agent refuses it.

### 2. Stop every user of the LV

Unmount the LV, stop the processes that use it, and remove any LIO or nvmet export of it on the storage node. If another driver served it, scale its workload down and wait until that driver has detached it. If you are not sure the LV is free, the agent checks it in the next step and refuses the claim with the reason.

### 3. Create the PVC

Use a StorageClass of the `lvm` store, preferably with `reclaimPolicy: Retain`. Request at most the LV size. Set the filesystem type to the one `blkid` reported:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-legacy
  namespace: db
  annotations:
    pillar-csi.bhyoo.com/import-lv: data-vg/legacy:Xk3pQm-1aB2-c3D4-e5F6-g7H8-i9J0-Kl1mNo:Rt7uVw-2cD3-e4F5-g6H7-i8J9-k0L1-Mn2oPq
    pillar-csi.bhyoo.com/filesystem: |
      fsType: ext4
spec:
  storageClassName: pillar-lvm-retain
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 20Gi
```

Without `pillar-csi.bhyoo.com/import-lv-policy`, the policy is `PreserveOriginal`. To make the LV an ordinary managed volume, add the policy explicitly. The value is case-sensitive, and any other value is refused:

```yaml
    pillar-csi.bhyoo.com/import-lv-policy: Managed
```

Do not choose `Managed` for data you cannot lose: with `reclaimPolicy: Delete`, deleting the PVC then destroys the LV.

Wait for the PVC to become `Bound`, then start the workload:

```sh
kubectl -n db get pvc data-legacy -w
```

If the PVC stays `Pending`, `kubectl -n db describe pvc data-legacy` shows the refusal; see [Refusals](#refusals).

### 4. Verify

The `PillarVolumeState` records the pinned source and the policy:

```sh
kubectl get pillarvolumestate -o custom-columns=NAME:.metadata.name,SOURCE:.spec.lvmSource.logicalVolumeUUID,PRESERVE:.spec.lvmSource.preserveOriginal
```

The agent volume ID is `<vg>/<lv>`, here `data-vg/legacy`, and the PV has the size of the LV. pillar-csi does not rename the LV.

## Roll back

**Before the PVC binds.** Delete the PVC. pillar-csi releases the lifecycle without touching the LV.

**After the PVC binds.** With `PreserveOriginal`, delete the workload and the PVC, with the PV's `reclaimPolicy` set to `Delete`. The controller calls `ReleaseVolume`, which removes the export and retires the lifecycle once the agent has verified the LV's identity and that nothing on the storage node uses it. If that verification fails, the PV stays and the controller retries; the LV is never deleted. Data the workload wrote stays on the LV. pillar-csi does not restore the LV's earlier content; use your backup for that.

## Inspect an LV

`InspectVolume` is an agent RPC that reports what the storage node observes about an LV and the agent's own record of it. It is read-only: it never activates, mounts or writes anything, does not take a fencing token, and does not decide who owns the LV. It supports `BACKEND_TYPE_LVM` only; other backends return `UNIMPLEMENTED`. pillar-csi ships no CLI for it. Call it with [grpcurl](https://github.com/fullstorydev/grpcurl) 1.9.4 and the agent's published `.proto` file. The agent does not serve gRPC reflection.

### Credentials

The agent listens on port `9500` on each storage node. Without mTLS the port is plaintext, which is acceptable only in a lab: use `-plaintext` instead of the certificate flags below.

With mTLS (see [Configure mTLS](/docs/how-to/configure-mtls/)), the agent accepts any client certificate signed by its CA. Every such certificate can call every agent RPC, including `DeleteVolume`; the agent has no read-only role, and `InspectVolume` being read-only does not make the credential read-only. You need:

- the CA's public certificate (`ca.crt`);
- a client certificate with `clientAuth` usage, and its private key, issued to you through the process your security owner approves.

Never use the CA's private key, and do not copy the controller's or the agent's key out of their Secrets. Keep the files in a directory only you can read:

```sh
umask 077
mkdir -p ~/.pillar-agent-client
kubectl -n pillar-csi get secret pillar-csi-agent-mtls -o jsonpath='{.data.ca\.crt}' | base64 -d > ~/.pillar-agent-client/ca.crt
# place your issued client.crt and client.key in the same directory
```

`pillar-csi-agent-mtls` is the agent Secret with the release name `pillar-csi` and cert-manager; with your own Secrets it is `pillar-agent-mtls` unless you renamed it. Only its public `ca.crt` key is read.

### The `.proto` file

Use the `agent.proto` of the exact source revision your agent image was built from: the release tag for a released image, or the commit for an image built from an untagged revision. A file from another revision can carry other fields and field numbers:

```sh
kubectl -n pillar-csi get ds pillar-csi-agent -o jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].image}'
mkdir -p proto/pillar_csi/agent/v1
curl -fsSL -o proto/pillar_csi/agent/v1/agent.proto \
  https://raw.githubusercontent.com/isac322/pillar-csi/<tag or commit of that image>/proto/pillar_csi/agent/v1/agent.proto
```

The file imports only `google/protobuf/timestamp.proto`, which grpcurl has built in. Revisions without LV import have no `InspectVolume`.

### Call it

Forward the agent port of the storage node to your machine:

```sh
AGENT_POD=$(kubectl -n pillar-csi get pod -l app.kubernetes.io/component=agent \
  --field-selector spec.nodeName=storage-1 -o name)
kubectl -n pillar-csi port-forward "$AGENT_POD" 9500:9500
```

In another shell, define the call once and inspect the LV. `-servername` is the name the agent certificate is issued for: `pillar-csi-agent.pillar-csi.svc` with cert-manager, otherwise your `mtls.serverName` or the storage node's IP address:

```sh
agent() {
  grpcurl -import-path proto -proto pillar_csi/agent/v1/agent.proto -emit-defaults \
    -cacert ~/.pillar-agent-client/ca.crt \
    -cert ~/.pillar-agent-client/client.crt -key ~/.pillar-agent-client/client.key \
    -servername pillar-csi-agent.pillar-csi.svc \
    -d @ 127.0.0.1:9500 "pillar_csi.agent.v1.AgentService/$1"
}

agent InspectVolume > inspect.json <<'EOF'
{"volume_id": "data-vg/legacy", "backend_type": "BACKEND_TYPE_LVM"}
EOF
```

grpcurl accepts the `.proto` field names in requests and prints JSON with camelCase names. 64-bit integers print as strings. An LV that does not exist returns `FAILED_PRECONDITION` with `missing`; a corrupt fence mark returns `INTERNAL`.

### Read the response

`InspectVolumeResponse` has these fields:

| Field | Type | Meaning |
|---|---|---|
| `lvm.identity` | `LvmSourceIdentity` | Observed `volumeGroup`, `logicalVolume`, `volumeGroupUuid`, `logicalVolumeUuid`. |
| `lvm.lvAttr`, `lvm.segtype`, `lvm.poolLv`, `lvm.origin` | string | As `lvs` reports them. |
| `lvm.sizeBytes` | int64 | LV size. |
| `lvm.active`, `lvm.devicePath`, `lvm.devMajorMinor` | bool, string, string | Activation and kernel device; the device fields are empty when inactive. |
| `lvm.exclusiveClaim` | string | `free` (an `O_EXCL` open succeeded and was closed at once), `busy` (`EBUSY`), or `unknown` (any other error, or the LV is inactive). |
| `filesystemType`, `filesystemUuid` | string | The `blkid -p` signature; set only when `filesystemProbeState` is `detected`. |
| `filesystemProbeState`, `filesystemProbeError` | string | `detected`, or `unknown` with the reason. |
| `consumers[]` | `DeviceConsumer` | `kind` (`mount`, `holder`, `exclusive_open`, `foreign_export`) and `detail`. |
| `exports[]` | `ExportObservation` | The agent's own configured exports of the volume ID: `targetId`, `namespaceEnabled`, `aclEnabled`, `allowedHosts`. |
| `fence` | `FenceObservation` | The agent's durable mark: `exists`, `volumeUid`, `generation` (uint64), `ended`, `endedUids`, `preserveOriginal`, `lvmSource`, and, after recovery, the durable transfer authorization digest and old/new lifecycle endpoints. |
| `snapshot` | `RecoverySnapshot` | Set only when the agent runs with `--recovery-trust-anchor`, the caller presented a verified mTLS client certificate, and the mark records a live lifecycle pinned to this LV. The agent signs it with its own server TLS key. See [Recover after metadata loss](#recover-after-metadata-loss). |

Read the evidence narrowly:

- `filesystemProbeState: unknown` does not mean blank. `blkid` exits the same way for an empty scan and for a failed read, and a partition table without a filesystem is also `unknown`.
- An empty `consumers` list does not mean the LV is unused. A mount in another mount namespace does not appear there, but it still makes `exclusiveClaim` `busy`. Treat the LV as idle only when `exclusiveClaim` is `free`, `consumers` is empty and `exports` is empty.
- `exports` lists configuration, not connected initiators. An export with an empty `allowedHosts` is not evidence that no initiator uses it.
- `fence.exists: false` means pillar-csi has no record for this volume ID. It does not prove that nothing else manages the LV.

### Find the former claim

`InspectVolume` never names an owner. Correlate the fence mark with Kubernetes yourself, and accept a match only when every field agrees:

```sh
FENCE_UID=$(jq -r '.fence.volumeUid' inspect.json)
LV_UUID=$(jq -r '.lvm.identity.logicalVolumeUuid' inspect.json)
jq -r '[.fence.exists, .fence.ended, .fence.lvmSource.logicalVolumeUuid] | @tsv' inspect.json

kubectl get pillarvolumestate -o json | jq -r \
  --arg uid "$FENCE_UID" --arg agent storage-1 --arg vid data-vg/legacy --arg lv "$LV_UUID" '
  [.items[] | select(.metadata.uid == $uid and .spec.agentRef == $agent and
     .spec.agentVolumeID == $vid and .spec.lvmSource.logicalVolumeUUID == $lv)]
  | if length == 1 then "claimed:" + .[0].metadata.name else "unknown" end'
```

The result is one of:

| Result | Meaning |
|---|---|
| `claimed:<name>` | The mark is live (`ended` is `false`), its `lvmSource.logicalVolumeUuid` equals the observed LV UUID, and exactly one `PillarVolumeState` matches. Its name is the PV name. |
| no mark | `fence.exists` is `false`. pillar-csi never adopted the volume ID. |
| retired | `fence.ended` is `true`. The lifecycle `volumeUid` was released or deleted; the LV has no live pillar-csi lifecycle. This is history, not an owner. |
| `unknown` | Any other case: no match, several matches, or a UUID that differs. Do not assign an owner. Find out what happened first. |

For a `claimed` result, read the PV and its former claim:

```sh
kubectl get pv <name> -o json | jq '{phase: .status.phase, reclaim: .spec.persistentVolumeReclaimPolicy,
  driver: .spec.csi.driver, handle: .spec.csi.volumeHandle, claim: .spec.claimRef}'
```

The PV's `spec.csi.driver` must be `pillar-csi.bhyoo.com` and `spec.csi.volumeHandle` must equal the `PillarVolumeState`'s `spec.volumeID`. `spec.claimRef` names the former claim by namespace, name and UID. A PVC with that name but another UID is a different claim.

## Rebind a retained volume

A preserved LV whose PVC was deleted under `reclaimPolicy: Retain` stays owned by its pillar-csi lifecycle. To serve it to a new claim, bind the same PV to a new, statically bound PVC. The PV, its `volumeHandle`, its `PillarVolumeState` and that state's UID stay; no `CreateVolume` runs and no new lifecycle starts. Do not re-import the LV with a new `import-lv` claim: pillar-csi refuses it, because the retained lifecycle still owns the LV.

The procedure stops the pillar-csi controller for a short time. No pillar-csi volume in the cluster can be provisioned, attached, detached or deleted while it is stopped.

The example rebinds the PV of the former claim `db/data-legacy` to the new claim `db/data-legacy-2`.

### 1. Check the Kubernetes state

Each check must pass. If one fails, stop: the old workload may still own the volume.

```sh
PV=<pv name>
kubectl get pv "$PV" -o json > pv.json
jq -r '.status.phase, .spec.persistentVolumeReclaimPolicy' pv.json   # Released, Retain
```

The former claim must be gone, including a claim that is still terminating. This prints nothing when the claim is gone:

```sh
OLD_NS=$(jq -r '.spec.claimRef.namespace' pv.json)
OLD_NAME=$(jq -r '.spec.claimRef.name' pv.json)
OLD_UID=$(jq -r '.spec.claimRef.uid' pv.json)
kubectl -n "$OLD_NS" get pvc "$OLD_NAME" -o json --ignore-not-found \
  | jq -r --arg uid "$OLD_UID" 'select(.metadata.uid == $uid) | "claim still exists"'
```

No `VolumeAttachment` may reference the PV, no Pod may still mount the former claim, and the `PillarVolumeState` must have no publication and must not be deleting:

```sh
kubectl get volumeattachment -o json \
  | jq -r --arg pv "$PV" '.items[] | select(.spec.source.persistentVolumeName == $pv) | .metadata.name'
kubectl -n "$OLD_NS" get pods -o json | jq -r --arg c "$OLD_NAME" \
  '.items[] | select(any(.spec.volumes[]?; .persistentVolumeClaim.claimName == $c)) | .metadata.name'
kubectl get pillarvolumestate "$PV" -o json \
  | jq '{published: (.status.publishedNodes // [] | length), deleting: (.status.deleting // false)}'
```

The first two commands must print nothing, and the last must show `0` and `false`.

Then confirm that the old node unstaged the volume. That node must be `Ready`, and no node may still report the volume in use:

```sh
HANDLE=$(jq -r '.spec.csi.volumeHandle' pv.json)
kubectl get nodes -o json | jq -r --arg v "kubernetes.io/csi/pillar-csi.bhyoo.com^$HANDLE" '
  .items[] | select((.status.volumesInUse // []) | index($v)) | .metadata.name'
kubectl get nodes
```

If the node that ran the old Pod is `NotReady`, unreachable, or carries the `node.kubernetes.io/out-of-service` taint, its unstage is unverified: stop here. If you do not know which node ran the old Pod, every node with the pillar-csi node plugin must be `Ready`. pillar-csi has no automatic guard against an old Pod that kept running outside Kubernetes' view on the same node; these checks are the guard.

### 2. Stop the controller

Record the replica count and arrange to restore it however the following steps end:

```sh
REPLICAS=$(kubectl -n pillar-csi get deploy pillar-csi-controller -o jsonpath='{.spec.replicas}')
trap 'kubectl -n pillar-csi scale deploy pillar-csi-controller --replicas="$REPLICAS"' EXIT
kubectl -n pillar-csi scale deploy pillar-csi-controller --replicas=0
until [ -z "$(kubectl -n pillar-csi get pod -o name \
    -l app.kubernetes.io/instance=pillar-csi,app.kubernetes.io/component=controller)" ]; do sleep 2; done
```

Run the remaining steps in the same shell. If you stop at a failed check, exit the shell or run the `scale` command from the trap; either way the controller comes back. If the shell might be lost, note `REPLICAS` so you can scale the controller back by hand.

### 3. Remove the export under the live lifecycle's token

Read the fencing token from the `PillarVolumeState`: its UID and its current `status.publicationGeneration` (absent means `0`). Read the volume ID and protocol from its spec:

```sh
kubectl get pillarvolumestate "$PV" -o json > pvs.json
PVS_UID=$(jq -r '.metadata.uid' pvs.json)
GEN=$(jq -r '.status.publicationGeneration // 0' pvs.json)
VID=$(jq -r '.spec.agentVolumeID' pvs.json)
jq -r '.spec.protocolType' pvs.json   # nvmeof-tcp → PROTOCOL_TYPE_NVMEOF_TCP, iscsi → PROTOCOL_TYPE_ISCSI
```

Call `UnexportVolume` with the `agent` function from [Inspect an LV](#inspect-an-lv):

```sh
agent UnexportVolume <<EOF
{"volume_id": "$VID", "protocol_type": "PROTOCOL_TYPE_NVMEOF_TCP",
 "fence": {"volume_uid": "$PVS_UID", "generation": "$GEN"}}
EOF
```

This removes the pillar-csi export so that the next step can prove the device is free. The lifecycle keeps owning the LV. Do not call `ReleaseVolume`: it retires the lifecycle, and the PV can then no longer be served. A `FAILED_PRECONDITION` about a stale fencing token means the state changed since you read it; stop and start over from step 1.

### 4. Inspect the LV

```sh
agent InspectVolume > inspect.json <<EOF
{"volume_id": "$VID", "backend_type": "BACKEND_TYPE_LVM"}
EOF
```

Compare the result with the `PillarVolumeState`. Every line must print `true`:

```sh
jq -n --slurpfile i inspect.json --slurpfile p pvs.json '
  def same(x; s): x.volumeGroup == s.volumeGroup and x.logicalVolume == s.logicalVolume and
                  x.volumeGroupUuid == s.volumeGroupUUID and x.logicalVolumeUuid == s.logicalVolumeUUID;
  $i[0] as $in | $p[0] as $pvs | $pvs.spec.lvmSource as $src |
  $in.fence.exists, $in.fence.volumeUid == $pvs.metadata.uid, ($in.fence.ended | not),
  same($in.fence.lvmSource; $src), same($in.lvm.identity; $src),
  ($in.exports | length) == 0, ($in.consumers | length) == 0, $in.lvm.exclusiveClaim == "free"'
```

If any line is `false`, do not bind. An empty `consumers` list alone is not enough; `exclusiveClaim` must be `free`.

### 5. Point the PV at the new claim

Read the PV again, repeat the checks of step 1 on this copy, then replace its `claimRef`. The body keeps the PV's `metadata.uid` and `metadata.resourceVersion`, so the API server refuses the replace with a conflict if the PV changed or was re-created since you read it:

```sh
kubectl get pv "$PV" -o json > pv.json
jq '.spec.claimRef = {apiVersion: "v1", kind: "PersistentVolumeClaim",
                      namespace: "db", name: "data-legacy-2"}' pv.json > pv-rebind.json
kubectl replace -f pv-rebind.json
```

On a conflict, start over from step 1. Do not retry with `--force`.

### 6. Create the static claim

Use the PV's StorageClass, name the PV in `volumeName`, and request at most its capacity. Leave out the `import-lv` annotations:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-legacy-2
  namespace: db
spec:
  storageClassName: pillar-lvm-retain
  volumeName: <pv name>
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 20Gi
```

```sh
kubectl -n db get pvc data-legacy-2 -w   # until Bound
```

### 7. Restart the controller and the workload

```sh
kubectl -n pillar-csi scale deploy pillar-csi-controller --replicas="$REPLICAS"
trap - EXIT
```

The controller's state resync re-creates the export from the recorded `exportSpec` under the same lifecycle. Inspect the LV again until `exports` is not empty and `fence.volumeUid` is still `$PVS_UID`, then start a Pod that uses `data-legacy-2` and check its data.

`PillarVolumeState.spec.claimRef` still names the original claim. It is an immutable record of the claim the volume was provisioned for; do not patch it. Publication conflicts work as for any volume: while another node holds the volume, the controller refuses to publish it elsewhere.

### What the operator must never do

- Delete or edit the agent's fence marks under `/var/lib/pillar-csi/agent/` on the storage node.
- Patch the `PillarVolumeState`'s spec or status by hand.
- Use `ReleaseVolume` or `DeleteVolume` to clear a lifecycle you want to keep. After a lifecycle is released, every mutating request with its token, including `DeleteVolume`, returns `FAILED_PRECONDITION`. The only exception is a `Managed` volume whose `DeleteVolume` already succeeded: a retry of that same request succeeds without touching the backend again.

## Recover after metadata loss

If the PVC, the PV and the `PillarVolumeState` of an adopted LV are lost while the agent's fence mark and the LV survive, a new `import-lv` claim is refused because the old lifecycle still owns the volume ID. Recovery moves that ownership to a new lifecycle. It applies only to LVs adopted through `import-lv`; zvols, datasets and volumes pillar-csi created are not covered.

Two signatures authorize the move, and nothing else does:

- **The agent's `RecoverySnapshot`.** `InspectVolume` returns it while the agent observes the LV: volume ID, LV identity, the old lifecycle's UID and exact generation, the preserve policy, exports, consumers, the exclusive-claim result, the agent identity and `issued_at`. The agent signs it with its own server TLS key and later verifies it against its serving certificate.
- **The operator's `RecoveryAuthorization`.** It names the snapshot's SHA-256 digest, the same volume and old lifecycle, the destination `PillarVolumeState` UID and generation, the preserve policy, and a validity window. You sign it with a key whose public half is in the agent's trust anchor file.

Being an mTLS client does not authorize anything: the agent never trusts who called it, only the two signatures. The CA's private key is never used, and no new PKI service is involved.

### Configure the trust anchor

Recovery requires mTLS (see [Configure mTLS](/docs/how-to/configure-mtls/)). Plaintext callers and TLS callers without a verified client certificate get no snapshot and every `TransferVolumeOwnership` is refused.

1. Generate the operator key pair (ECDSA P-256 or RSA) on a machine your security owner approves, and keep the private key there. Never put it on a node, in a Secret or in the cluster.
2. Write only the public half (`PUBLIC KEY` or `CERTIFICATE` PEM blocks) to the same path on every node that runs the agent, below the agent state directory, for example `/var/lib/pillar-csi/agent/recovery-trust-anchor.pem`.
3. Pass it to the agent and restart it. `--set-json` replaces the whole list, so include any other `agent.extraArgs` you already set:

   ```sh
   helm upgrade pillar-csi charts/pillar-csi -n pillar-csi --reuse-values \
     --set-json 'agent.extraArgs=["--recovery-trust-anchor=/var/lib/pillar-csi/agent/recovery-trust-anchor.pem"]'
   ```

The agent reads the file once, at startup, together with its server certificate and key, which sign the snapshots. A missing or unreadable file, a file without a usable key, a key that is neither RSA nor ECDSA, or a server certificate with no Subject CN and no DNS SAN stops the agent from starting, so recovery never runs half-configured. Without the flag, `InspectVolume` returns no snapshot and `TransferVolumeOwnership` returns `UNAVAILABLE`.

**Rotation.** The file may hold several keys; any one of them may sign. To rotate, add the new public key, restart the agents, sign new grants with the new key, then remove the old key and restart again. Removing a key invalidates every grant it signed that has not been committed yet.

**Expiry.** A grant's window is at most 24 hours (plus 2 minutes of clock skew), and it is refused after `expires_at` or before `issued_at`. A snapshot is refused when it is older than 15 minutes at transfer time, so take it just before you sign. Rotating the agent's server certificate invalidates every outstanding snapshot; inspect again.

### 1. Stop the old lifecycle

Make sure no workload uses the volume and the old node has unstaged it. Then stop the controller, so nothing provisions the replacement claim before its record exists. Record the replica count and arrange to restore it:

```sh
REPLICAS=$(kubectl -n pillar-csi get deploy pillar-csi-controller -o jsonpath='{.spec.replicas}')
kubectl -n pillar-csi scale deploy pillar-csi-controller --replicas=0
```

Inspect the LV as in [Call it](#call-it). If `exports` is not empty, remove the export under the old lifecycle's token, using `fence.volumeUid` and `fence.generation` from the response:

```sh
agent UnexportVolume <<EOF
{"volume_id": "data-vg/legacy", "protocol_type": "PROTOCOL_TYPE_NVMEOF_TCP",
 "fence": {"volume_uid": "$OLD_UID", "generation": "$OLD_GEN"}}
EOF
```

Continue only when `exports` and `consumers` are empty and `lvm.exclusiveClaim` is `free`.

### 2. Create the claim and the recovery record

Create a plain PVC of a StorageClass of the LV's store, without any `import-lv` annotation; a recovery claim with one is refused. Read its UID and create the record named after it. `oldVolumeUID`, `oldGeneration` and `source` come from `fence` and `lvm.identity`; `newGeneration` is the generation the new lifecycle starts at, for example `oldGeneration + 1`:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarVolumeState
metadata:
  name: pvc-<claim UID>
spec:
  volumeID: <agent>/nvmeof-tcp/lvm-lv/data-vg/legacy
  agentVolumeID: data-vg/legacy
  agentRef: <agent>
  backendType: lvm-lv
  protocolType: nvmeof-tcp
  capacityBytes: <LV size>
  claimRef: {uid: <claim UID>, namespace: db, name: data-legacy}
  recovery:
    oldVolumeUID: <fence.volumeUid>
    oldGeneration: <fence.generation>
    newGeneration: <fence.generation + 1>
    source:
      volumeGroup: data-vg
      logicalVolume: legacy
      volumeGroupUUID: <vg_uuid>
      logicalVolumeUUID: <lv_uuid>
      preserveOriginal: true
```

Leave out `lvmSource`; it cannot be combined with `recovery`. The record is born `RecoveryPending`: it serves nothing, refuses publish, expand and delete, and the abandoned-volume reaper never removes it.

### 3. Sign the grant and latch it

Inspect the LV again and keep the whole response (`inspect.json`); its `snapshot` is what you sign over. pillar-csi ships no signing CLI. The grant is the deterministic protobuf encoding (`proto.MarshalOptions{Deterministic: true}`) of `RecoveryAuthorization` with `signature` cleared, hashed with SHA-256 and signed with your key (ECDSA: ASN.1 DER; RSA: PKCS#1 v1.5). `snapshot_digest` is the same digest of the snapshot with its `signature` cleared. This Go program, built against the `gen/go` package of the same revision as the agent, prints the three values the record needs:

```go
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var det = proto.MarshalOptions{Deterministic: true}

func digest(m proto.Message) []byte {
	b, err := det.Marshal(m)
	if err != nil {
		panic(err)
	}
	d := sha256.Sum256(b)
	return d[:]
}

// usage: sign inspect.json operator-key.pem <new PVS uid> <new generation>
func main() {
	raw, _ := os.ReadFile(os.Args[1])
	var resp agentv1.InspectVolumeResponse
	if err := protojson.Unmarshal(raw, &resp); err != nil {
		panic(err)
	}
	snap := resp.GetSnapshot()
	unsigned := proto.Clone(snap).(*agentv1.RecoverySnapshot)
	unsigned.Signature = nil

	keyPEM, _ := os.ReadFile(os.Args[2])
	block, _ := pem.Decode(keyPEM)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	var newGen uint64
	fmt.Sscan(os.Args[4], &newGen)
	now := time.Now()
	auth := &agentv1.RecoveryAuthorization{
		SnapshotDigest:   digest(unsigned),
		VolumeId:         snap.GetVolumeId(),
		BackendType:      snap.GetBackendType(),
		LvmSource:        snap.GetLvmSource(),
		OldVolumeUid:     snap.GetOldVolumeUid(),
		OldGeneration:    snap.GetOldGeneration(),
		NewVolumeUid:     os.Args[3],
		NewGeneration:    newGen,
		PreserveOriginal: proto.Bool(snap.GetPreserveOriginal()),
		IssuedAt:         timestamppb.New(now.Add(-time.Minute)),
		ExpiresAt:        timestamppb.New(now.Add(time.Hour)),
	}
	d := digest(auth)
	sig, err := key.(crypto.Signer).Sign(rand.Reader, d, crypto.SHA256)
	if err != nil {
		panic(err)
	}
	auth.Signature = sig
	snapBytes, _ := det.Marshal(snap)
	authBytes, _ := det.Marshal(auth)
	fmt.Println("snapshot:", base64.StdEncoding.EncodeToString(snapBytes))
	fmt.Println("authorization:", base64.StdEncoding.EncodeToString(authBytes))
	fmt.Println("authorizationDigest:", hex.EncodeToString(d))
}
```

Use the record's `metadata.uid` as the new UID and its `spec.recovery.newGeneration` as the new generation. Then set the write-once fields and the snapshot in one patch. `authorization` is the base64 string the program printed (Kubernetes encodes byte fields as base64); the snapshot goes in the `pillar-csi.bhyoo.com/recovery-snapshot` annotation, also base64:

```sh
kubectl patch pillarvolumestate pvc-<claim UID> --type=merge -p "{
  \"metadata\": {\"annotations\": {\"pillar-csi.bhyoo.com/recovery-snapshot\": \"$SNAPSHOT\"}},
  \"spec\": {\"recovery\": {\"newVolumeUID\": \"$NEW_UID\",
    \"authorization\": \"$AUTHORIZATION\", \"authorizationDigest\": \"$DIGEST\"}}}"
```

`newVolumeUID`, `authorization` and `authorizationDigest` can be set once and never changed.

### 4. Restart the controller

```sh
kubectl -n pillar-csi scale deploy pillar-csi-controller --replicas="$REPLICAS"
```

The provisioner retries `CreateVolume` for the claim. The controller checks the grant against the record, the snapshot against the live observation, and calls `TransferVolumeOwnership`. Under the volume's lock the agent verifies both signatures, re-observes the LV (no export admitting an initiator, no mount or holder, the `O_EXCL` open succeeds) and writes one new mark: the new UID and generation, the old UID retired, the pinned LV and policy, and the grant's digest. The record turns `Ready` and the claim binds to the PV `pvc-<claim UID>` with the volume handle the lost PV had. `InspectVolume` then shows `fence.volumeUid` = the new UID with the old UID in `endedUids`.

If the mark write's outcome is uncertain (failure after the rename), the agent reports `TRANSFER_OUTCOME_UNKNOWN` and rolls nothing back; the controller retries the identical request once, and the next `CreateVolume` retries again. A retry of the exact committed request returns `TRANSFER_OUTCOME_ALREADY_COMMITTED`. Any other request for the volume after the commit is refused.

### Recovery refusals

`TransferVolumeOwnership` writes nothing when it refuses; the old lifecycle keeps the mark.

| Code | Cause |
|---|---|
| `UNAUTHENTICATED` | Plaintext or unverified client; snapshot signature or identity does not match the agent's certificate; grant not signed by a trust-anchor key. |
| `UNAVAILABLE` | The agent runs without `--recovery-trust-anchor`. |
| `INVALID_ARGUMENT` | Snapshot or grant missing; `preserve_original` unset; the grant names another snapshot digest, volume, LV, old UID or old generation. |
| `FAILED_PRECONDITION` | Grant expired, not yet valid or longer than 24 hours; snapshot older than 15 minutes; no mark, or the mark is ended, owned by another lifecycle or at another generation; LV identity differs from the pin; a `PreserveOriginal` pin would be downgraded; the old initiator is not proven stopped (export, ACL grant, mount, holder, `O_EXCL` busy or unknown); the destination already received a different grant. |

On the controller side, a record whose grant digest, UID, generation, source or policy disagrees with `spec.recovery`, or whose claim carries an `import-lv` annotation, stays `RecoveryPending` and the claim stays `Pending` with the reason in its events.

## What is not supported

- **Recovery without both signatures.** Metadata-loss recovery needs an agent-signed snapshot and an operator-signed grant, as described in [Recover after metadata loss](#recover-after-metadata-loss). There is no automatic takeover and no way around it: never delete fence marks, force an offline node's state, or import under another name.
- Rebinding when the old node is offline, or when the old Pod's unstage cannot be verified.
- Adopting LVs outside the layouts in [Supported layouts](#supported-layouts), changing the pinned LV or policy of a lifecycle, or downgrading a `PreserveOriginal` pin to `Managed`.
- Expanding a `PreserveOriginal` volume. Copy the data into a larger volume instead.
- A read-only agent credential. Any client certificate the agent accepts can call every RPC.

## Refusals

The PVC stays `Pending`, and its events show one of these messages. `<annotation>` stands for `pillar-csi.bhyoo.com/import-lv`.

| Message | Cause | Fix |
|---|---|---|
| `<annotation> must name an LVM logical volume as "<vg>/<lv>:<vg_uuid>:<lv_uuid>", got ...` | The value does not have three `:`-separated fields. | Build the value as in step 1. |
| `<annotation> locator ... must be exactly "<vg>/<lv>"`, or `<annotation> ...:` followed by an LVM name error | The first field is not one VG name, `/`, and one LV name. | Use the names `lvs` prints. |
| ``<annotation> ...: vg_uuid ... is not an LVM UUID (read it with `lvs -o vg_uuid,lv_uuid`)`` (or `lv_uuid`) | A UUID is not in LVM's text form. | Copy the UUIDs from `lvs`. |
| `pillar-csi.bhyoo.com/import-lv-policy must be "PreserveOriginal" or "Managed", got ...` | Unknown policy value. | Use one of the two values, or omit the annotation. |
| `<annotation> requires an lvm-lv PillarStore backend, got backend ...: other backends cannot adopt existing LVs` | The StorageClass's store is not LVM. | Use a StorageClass of an `lvm` store. |
| `<annotation> LV ... lives in volume group ... but the PillarStore selects volume group ...` | The VG differs from the store's `volumeGroup`. | Use a StorageClass of a store for that VG. |
| `<annotation>: LV ... (lv_uuid ...) is already managed by volume ... (PillarVolumeState ...); delete that volume first` | Another pillar-csi volume owns the LV, by its `<vg>/<lv>` volume ID or by its LV UUID. | Import each LV once. |
| `<annotation>: LV ... is reserved by ... (PillarVolumeReservation ...); delete that claim first, ...` | Another claim holds the LV's reservation, which is keyed by the LV UUID. | Import each LV once. If the named claim and its volume are gone, release the reservation as described in [Release a stale reservation](/docs/how-to/import-zvol/#release-a-stale-reservation). |
| `<annotation>: volume ... already adopted LV ... (vg_uuid ..., lv_uuid ..., policy ...); the import source and policy cannot be changed` | The annotation or policy changed after the import started. | Restore the first values, or delete the PVC and create it again. |
| `<annotation>: volume ... was already provisioned without an LV import; delete the PersistentVolumeClaim and re-create it to import an LV` | The annotation was added to a claim whose provisioning had already started. | Delete the PVC and create it again with the annotation. |
| `import of volume ... refused: layout: ...` | Wrong VG for the backend, an LV name that is not a single component, a segment type other than `linear`/`thin`, or a thin LV outside the backend's `thinPool` (or any thin LV on a backend without one, or a linear LV on a backend with one). | See [Supported layouts](#supported-layouts). |
| `import of volume ... refused: missing: ...` | No such LV. | Check the name with `lvs`. |
| `import of volume ... refused: identity: ...` | The UUIDs differ: the name now points at another LV, or the device changed during the import. | Read the UUIDs again and check that this is the LV you mean. |
| `import of volume ... refused: wrong type: ...` | Snapshot, pool, mirror, RAID, origin, pvmove or virtual LV. | Only plain linear or thin LVs can be adopted. |
| `import of volume ... refused: inactive: ...` | The LV is not active. | Activate it yourself if your site allows that; import never activates. |
| `import of volume ... refused: too small: ...` | The PVC requests more than the LV size. | Request at most the LV size. |
| `import of volume ... refused: in use: ...` | A mount, holder, LIO or nvmet export, or another exclusive opener holds the LV. | Stop that user, as in step 2. |
| `ImportVolume: volume ... is pinned to LV ...; refusing to import another source` or `... is pinned PreserveOriginal; refusing to downgrade the adoption to Managed` | The agent's mark for `<vg>/<lv>` pins another identity or the preserving policy. | Use the pinned identity and policy. |
| `cannot expand volume ...: it adopted LV ... under policy PreserveOriginal, which never resizes the original` | Expansion of a preserved LV. | Not supported. |
| `NodeStageVolume: preserved volume ... cannot mount ...` | The LV has no filesystem, or one of another type than the claim requests. | Set `fsType` to the `blkid` type, or use `volumeMode: Block`. |

Messages that start with `import of volume` or `ImportVolume` come from the agent on the storage node; the others come from the controller before it calls the agent.
