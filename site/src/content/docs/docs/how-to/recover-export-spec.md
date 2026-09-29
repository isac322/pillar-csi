---
title: Recover a volume stuck at ExportSpecMissing
description: Manual pillar-csi runbook for legacy volumes whose PillarVolumeState lacks exportSpec, so the NVMe-oF/TCP export cannot be rebuilt after a storage node restart.
sidebar:
  order: 11
---

Each `PillarVolumeState` has an `ExportReconciled` condition. It reports whether the kernel export on the storage node still matches the export the controller recorded for the volume in `status.exportSpec`. After an agent restart, a storage node reboot or an `nvmet` reload, the controller rebuilds the export from that record.

A volume provisioned before the controller started recording `exportSpec` (issue [#83](https://github.com/isac322/pillar-csi/issues/83)) has no record to rebuild from. When its storage node loses target state, the controller refuses to guess and sets `ExportReconciled=False` with reason `ExportSpecMissing`. The volume stays offline until you supply the record by hand. After that one repair, the volume recovers on its own like any other.

This page is that repair. Volumes created by 0.2.0 or later record `exportSpec` at `CreateVolume` and never need it.

## Why the controller does not fill the record in

`status.exportInfo` looks like a source, but it only describes the last live endpoint (`targetID`, `address`, `port`, `volumeRef`). It has no ACL field, and the address or port may have changed since provisioning. The current `PillarProtocol`, `PillarStorageClass` and `PillarAgent` may have changed too. A wrong ACL value fails in either direction: turning ACL on for a volume that ran open locks out every consumer, and turning it off for a volume that relied on it exposes the namespace to the whole network. So every value below comes from provisioning-time evidence or an explicit decision you record.

## Before you start

- The procedure applies only when `status.exportSpec` is absent and the `ExportReconciled` reason is `ExportSpecMissing`. For `AgentUnavailable`, `StaleGeneration` or `ReconcileFailed`, a record exists or another fault comes first. Fix that fault and do not patch.
- You need `patch` on `pillarvolumestates/status`. The resource is cluster scoped, so this usually means cluster-admin.
- The volume's `PillarAgent` must be reachable: `AgentConnected` is `True` in `kubectl describe pillaragent <spec.agentRef>`.
- You need `kubectl` and `jq`, and the commands use bash.
- Repair one volume at a time and verify it before the next.
- Stop if any check fails, if an object changes under you, or if you cannot justify a value. Leave the volume at `ExportSpecMissing`. An offline volume can still be recovered; a wrongly exported one may not be.

## 1. Find affected volumes and pin the context

Choose the cluster context once and use it in every command. A context switch halfway would send reads and writes to another cluster.

```sh
kubectl config get-contexts          # choose deliberately
CTX=<cluster-context>
```

List volumes where both conditions hold:

```sh
kubectl --context "$CTX" get pvst -o json | jq -r '
  .items[]
  | select(.status.exportSpec == null)
  | select([.status.conditions[]? | select(.type=="ExportReconciled" and .reason=="ExportSpecMissing")] | length > 0)
  | .metadata.name'
```

Read the chosen `PillarVolumeState` once and derive every identity from that single read. Step 2 verifies this snapshot, and step 4 pins its patch to it. Do not resolve identity again from a second read.

```sh
PVST=<pvst-name>
PVS_JSON=$(kubectl --context "$CTX" get pvst "$PVST" -o json)
PVS_UID=$(jq -r '.metadata.uid' <<<"$PVS_JSON")             # lifecycle identity
PVS_RV=$(jq -r '.metadata.resourceVersion' <<<"$PVS_JSON") # snapshot for the CAS guard
VID=$(jq -r '.spec.volumeID' <<<"$PVS_JSON")               # CSI volume ID == PV volumeHandle
```

Resolve the PV and its claim from one PV read. Exactly one PV must match the volume handle. If jq reports zero or several, stop.

```sh
PV_JSON=$(kubectl --context "$CTX" get pv -o json)
PV=$(jq -r --arg vid "$VID" '[.items[]
     | select(.spec.csi.driver=="pillar-csi.bhyoo.com" and .spec.csi.volumeHandle==$vid)]
     | if length == 1 then .[0].metadata.name
       else error("PV identity ambiguous: " + (length|tostring) + " matches") end' <<<"$PV_JSON")
read -r CLAIM_NS CLAIM_NAME CLAIM_UID < <(jq -r --arg pv "$PV" '
     .items[] | select(.metadata.name==$pv)
     | "\(.spec.claimRef.namespace) \(.spec.claimRef.name) \(.spec.claimRef.uid)"' <<<"$PV_JSON")
PVC_JSON=$(kubectl --context "$CTX" get pvc "$CLAIM_NAME" -n "$CLAIM_NS" -o json)
```

## 2. Back up the snapshots and verify the identity chain

```sh
mkdir -p pvs-recovery-"$PVST" && cd pvs-recovery-"$PVST"
jq . <<<"$PVS_JSON" > pvst-backup.json
jq --arg pv "$PV" '.items[] | select(.metadata.name==$pv)' <<<"$PV_JSON" > pv-backup.json
jq . <<<"$PVC_JSON" > pvc-backup.json
```

Check every link in both directions against the saved files. Each `jq -e` exits non-zero if its check fails.

```sh
# PVS: same lifecycle UID and volumeID, still without exportSpec, not being deleted
jq -e --arg uid "$PVS_UID" --arg vid "$VID" 'select(
    .metadata.uid == $uid and .spec.volumeID == $vid
    and .status.exportSpec == null
    and .metadata.deletionTimestamp == null and .status.deleting != true)' pvst-backup.json
# PV: same name, this driver, this volumeHandle, claimRef UID is the captured claim
jq -e --arg pv "$PV" --arg vid "$VID" --arg claim "$CLAIM_UID" 'select(
    .metadata.name == $pv and .spec.csi.driver=="pillar-csi.bhyoo.com"
    and .spec.csi.volumeHandle == $vid and .spec.claimRef.uid == $claim)' pv-backup.json
# PVC: the claimed UID is a live PVC Bound to this PV
jq -e --arg pv "$PV" --arg uid "$CLAIM_UID" 'select(
    .metadata.uid == $uid and .status.phase == "Bound" and .spec.volumeName == $pv)' pvc-backup.json
# Print where the backend volume should be
jq -r '.spec.agentVolumeID, .spec.agentRef' pvst-backup.json
```

`spec.agentVolumeID` is `<pool>/<volume>` for ZFS or `<vg>/<volume>` for LVM. On the storage node, confirm the backend volume exists with `zfs list <agentVolumeID>` or `lvs <agentVolumeID>`. The repair re-creates only the export and never creates a backend volume.

A non-empty `status.publishedNodes` is normal if consumers were attached when the target state was lost. Those entries are the ACL list the controller will re-apply. Do not edit them.

## 3. Decide the export record

The schema requires three fields: `bindAddress` (non-empty), `port` (0 to 65535) and `aclEnabled` (boolean). None has a default here. Decide each from provisioning-time evidence, never from `status.exportInfo` alone and never from the current CRs.

### bindAddress

No durable record of the requested bind address exists for these volumes. `pv.spec.csi.volumeAttributes["address"]` and `status.exportInfo.address` show the address the agent actually exported at provisioning time. They support a decision but do not make it. Normally you choose that same address.

This patch does not update the PV, and consumers keep connecting to `volumeAttributes.address` and `port`. A different bind address can therefore reach `ExportReconciled=True` while consumers still dial the old endpoint. Moving the endpoint needs its own connectivity plan outside this procedure.

### port

Choose it explicitly. `volumeAttributes["port"]` and `exportInfo.port` show what the export used; compare them with the class default of `4420` before you reuse either. No PVC annotation ever changed the port, so the port came from the protocol. The current `PillarProtocol` port may have changed since.

### aclEnabled

No provisioning-time record exists, and that gap is the reason the controller stops at `ExportSpecMissing`. Reconstruct the value from the ACL flag in the parameters of the StorageClass the volume was provisioned from. Controllers before the 0.3 configuration redesign copied the protocol's ACL setting into generated StorageClasses. That evidence counts only if you can prove the protocol and binding have not changed since, through GitOps history, a snapshot or an audit log. Otherwise make an explicit operator decision and write it down. No PVC annotation ever changed ACL. If the evidence supports neither `true` nor `false`, stop and do not patch.

What each ACL value does:

- `true` admits only the initiators in `publishedNodes`, excluding entries marked `revoking`. An empty list admits nobody, and the controller can still report `Reconciled` while consumers missing from the list stay locked out.
- `false` sets `attr_allow_any_host=1`, which exposes the volume to every host that can reach the port.

You may deliberately change the historical intent, but only after recording the choice and its connectivity and security consequences.

Leave out the optional `inCapsuleDataSize`. Controllers that recorded no `exportSpec` never set an in-capsule data size on the export, so the restored export keeps accepting the port's value as before.

Look at the evidence:

```sh
kubectl --context "$CTX" get pv "$PV" -o jsonpath='{.spec.csi.volumeAttributes}'
kubectl --context "$CTX" get pvc "$CLAIM_NAME" -n "$CLAIM_NS" -o jsonpath='{.metadata.annotations}'
```

Then set the values. The checks abort on an unset or malformed value:

```sh
BIND_ADDRESS=   PORT=   ACL_ENABLED=   # REQUIRED: explicit decision, no defaults
: "${BIND_ADDRESS:?set from step 3 evidence}" \
  "${PORT:?set from step 3 evidence}" \
  "${ACL_ENABLED:?set to true or false explicitly}"
case "$ACL_ENABLED" in true|false) ;; *) echo "ACL_ENABLED must be 'true' or 'false'"; exit 1;; esac
case "$PORT" in ''|*[!0-9]*) echo "PORT must be a non-negative integer"; exit 1;; esac
```

## 4. Patch the status, pinned to the snapshot

The two `test` operations compare the object's UID and `resourceVersion` with the snapshot you verified in step 2. If either differs, the API server rejects the whole patch, so the write cannot land on another lifecycle or on an object that changed.

```sh
kubectl --context "$CTX" patch pvst "$PVST" --subresource=status --type=json -p "$(jq -cn \
  --arg uid "$PVS_UID" --arg rv "$PVS_RV" \
  --arg bind "$BIND_ADDRESS" --argjson port "$PORT" --argjson acl "$ACL_ENABLED" '[
    {op:"test", path:"/metadata/uid",            value:$uid},
    {op:"test", path:"/metadata/resourceVersion", value:$rv},
    {op:"add",  path:"/status/exportSpec",
     value:{bindAddress:$bind, port:$port, aclEnabled:$acl}}
  ]')"
```

If the patch fails with `the server rejected our request` or a conflict error, the object changed after your snapshot. Stop, read it again, and restart from step 2. Do not refresh `PVS_RV` just to make a retry pass: a changed object invalidates the identity and the decision you verified.

The patch writes only `/status/exportSpec`. Leave everything else alone:

- `status.publishedNodes`, `status.publicationGeneration`, `status.deleting`, the conditions and `spec`
- the agent's fencing marks under `/var/lib/pillar-csi/agent/generations/` on the storage node
- the backend volume

The controller then re-creates only the kernel export and its ACL through the agent, fenced by the `PillarVolumeState` UID and `publicationGeneration`. It never creates, formats or deletes backend storage.

## 5. Wait for convergence and check the data

The status change triggers a resync, and a periodic resync every 30 seconds is the fallback, so you do not restart the controller.

```sh
kubectl --context "$CTX" wait pvst/"$PVST" \
  --for=jsonpath='{.status.conditions[?(@.type=="ExportReconciled")].status}'=True --timeout=60s
kubectl --context "$CTX" get pvst "$PVST" \
  -o jsonpath='{.status.conditions[?(@.type=="ExportReconciled")].reason}'
# must print: Reconciled
```

If the condition stays `False`, its `reason` and `message` name the next problem (`AgentUnavailable`, `StaleGeneration` or `ReconcileFailed`). Fix that cause. The controller keeps retrying from the record you added, so do not patch again.

`ExportReconciled=True` means the kernel export and ACL exist again. It does not prove the data is reachable or intact. Check through the consumer that already runs. If its node kept the session and the mount, this may already pass and you are done:

```sh
kubectl --context "$CTX" -n "$CLAIM_NS" exec <consumer-pod> -- sha256sum <known-file-or-device-offset>
```

Restart the consumer only if connectivity is really lost. Stop the old consumer, wait until its pod and its `VolumeAttachment` are gone, then start the replacement. Never run two consumers at once. `ReadWriteOnce` does not limit a volume to one pod: pods on the same node can share the mount, while a second node cannot publish a volume already published elsewhere. Rolling restarts and unpinned pod deletes are therefore unsafe here. A new pod re-stages and reconnects only if the stage record or the session is actually gone; otherwise it reuses the staged mount. Run the hash check again on the replacement before you call the volume recovered.

## Scope of a repair

A successful repair of one volume says nothing about another volume's intent. Each repaired volume keeps its `exportSpec` in etcd, so later target-state losses heal on their own. Every other volume at `ExportSpecMissing` needs the same procedure with its own verified values, because the controller does not infer a missing record.
