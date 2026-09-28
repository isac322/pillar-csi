---
title: Upgrade pillar-csi
description: Upgrade the pillar-csi Helm chart, including the 0.2 to 0.3 configuration cutover, the 0.1 to 0.2 fencing cutover and the PillarVolumeState CRD rename.
sidebar:
  order: 9
---

pillar-csi is pre-1.0, and minor releases have broken compatibility. Read the section for every version you cross before you run `helm upgrade`.

## Upgrade within a release line

The chart is published as an OCI artifact. Pass the same values you installed with:

```sh
helm upgrade pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \
  --version <chart-version> \
  --namespace pillar-csi \
  -f values.yaml
```

The CRDs are chart templates (`templates/crds.yaml`), so `helm upgrade` updates them when `installCRDs` is `true`, the default. Every CRD carries `helm.sh/resource-policy: keep`, so neither an upgrade nor `helm uninstall` deletes a CRD or the objects in it. If you manage CRDs yourself (`installCRDs=false`), apply the new CRDs from `config/crd/bases/` at the release tag before you upgrade the chart.

The controller, the node plugin and every agent belong to one Helm release, so one upgrade moves them together. The 0.1 to 0.2 and 0.2 to 0.3 cutovers below require that.

Restarting the agent pods does not remove kernel exports, so connected workers keep their sessions during the rollout. See [Maintain storage and worker nodes](/docs/how-to/node-maintenance/) for what happens on a storage node.

## 0.2 to 0.3: configuration cutover

0.3.0 gives every storage, protocol and filesystem setting one name and one nested shape wherever it appears. The change is incompatible. A 0.2 configuration cannot be upgraded in place, and volumes provisioned by 0.2 are not migrated.

### What changed

| Where | 0.2 | 0.3 |
|---|---|---|
| `PillarStore.spec.backend` | `type: zfs-zvol` plus a `zfs` or `lvm` block | exactly one member: `zfs: {volumeType, pool, parentDataset, properties}` or `lvm: {volumeGroup, thinPool, provisioningMode}`; `type` is gone |
| `PillarProtocol.spec` | `type: nvmeof-tcp`, `nvmeofTcp: {...}`, `fsType`, `mkfsOptions` | `protocol: {nvmeofTcp: {...}}`, transport settings only |
| Filesystem settings | on `PillarProtocol` and in `PillarStorageClass.spec.overrides` | `PillarStorageClass.spec.filesystem: {fsType, mkfsOptions, mountOptions}` |
| `PillarStorageClass.spec.overrides` | `backend`, `protocol`, `fsType`, `mkfsOptions` | `backend` and `protocol` only, each with the same nested shape as the CR it overrides |
| PVC annotations | `backend-override`, `protocol-override`, `fs-override`, `param.*` | YAML documents under `pillar-csi.bhyoo.com/backend`, `/protocol`, `/filesystem`; the old keys are rejected |
| Hand-written StorageClass | flat keys such as `zfs-prop.*`, `lvm-*`, `nvmeof-*`, `acl-enabled`, `backend-type` | `pillar-csi.bhyoo.com/store-ref`, `/protocol-ref` and the same three documents; the old keys are rejected |
| Helm `agent.backends` | `{type: zfs-zvol, pool, parent}`, `{type: lvm-lv, vg, thinpool}` | `{zfs: {pool, parentDataset}}`, `{lvm: {volumeGroup, thinPool}}`, rendered into the agent's `--config` file |
| `pillar-agent` flags | `--backend` | `--config`; `--backend` is gone. Default listen port `9500` |
| Served schema | schema-only placeholders for iSCSI, NFS, SMB, `zfs-dataset` and `dir`, never implemented | placeholders removed; the schema accepts only `zfs`, `lvm` and `nvmeofTcp`. iSCSI, NFS and SMB remain planned |

A 0.2 `PillarVolumeState` has `spec.nodeConnectParams`. 0.3 instead records the resolved configuration in `spec.resolved` at `CreateVolume`, and 0.2 StorageClasses carry the old parameters. No migration shim exists.

Examples of the new shapes:

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: storage-1-hot
spec:
  agentRef: storage-1
  backend:
    zfs:
      pool: tank
      parentDataset: k8s
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nvmeof-default
spec:
  protocol:
    nvmeofTcp:
      port: 4420
      acl: true
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: hot
spec:
  storeRef: storage-1-hot
  protocolRef: nvmeof-default
  storageClass:
    name: pillar-hot
  filesystem:
    fsType: xfs
```

```yaml
# values.yaml
agent:
  backends:
    - zfs: {pool: tank, parentDataset: k8s}
    - lvm: {volumeGroup: data-vg, thinPool: thin0}
```

### Procedure

pillar-csi has no snapshot support, so back up data with your application's own tools.

1. Rewrite your `PillarStore`, `PillarProtocol` and `PillarStorageClass` manifests, your Helm values, your hand-written StorageClasses and any PVC templates that carry pillar-csi annotations in the 0.3 shapes above.

2. Back up the data on every pillar-csi volume.

3. Delete every workload that uses a pillar-csi volume, then every such PVC. For a PV with `persistentVolumeReclaimPolicy: Retain`, `DeleteVolume` never runs, so its backend volume and `PillarVolumeState` stay. Switch the policy to `Delete` first if you do not need the backend volume:

   ```sh
   kubectl patch pv <pv-name> -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
   ```

4. Wait until no pillar-csi PV and no `PillarVolumeState` remains:

   ```sh
   kubectl get pv -o json | jq -r '.items[] | select(.spec.csi.driver=="pillar-csi.bhyoo.com") | .metadata.name'
   kubectl get pvst
   ```

5. While the 0.2 controller still runs, delete the 0.2 `PillarStorageClass`, `PillarStore` and `PillarProtocol` objects, and any hand-written StorageClass with `provisioner: pillar-csi.bhyoo.com`. The `PillarAgent` shape did not change, so you can keep it.

   ```sh
   kubectl delete pillarstorageclass --all
   kubectl delete pillarstore --all
   kubectl delete pillarprotocol --all
   ```

6. Upgrade with the rewritten values file. Do not pass `--reuse-values`: it would carry the 0.2 `agent.backends` entries, which no longer render.

   ```sh
   helm upgrade pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \
     --version 0.3.3 --namespace pillar-csi -f values.yaml
   ```

7. Apply the rewritten CRs and StorageClasses, and wait until they are Ready:

   ```sh
   kubectl get pillaragent,pillarstore,pillarprotocol,pillarstorageclass
   ```

8. Create the PVCs again and restore the data.

## 0.1 to 0.2: fencing cutover

0.2.0 added fencing tokens to every agent call that changes a volume. 0.1.x recorded no publications, lifecycles or generations, so a 0.2 controller cannot revoke access that 0.1 granted. There is no migration shim.

- Detach every volume before the upgrade: no `VolumeAttachment` for this driver may remain.
- Upgrade the controller and every agent in the same rollout. An upgraded agent rejects any volume-changing call that carries no token with `FAILED_PRECONDITION`, so an old controller paired with a new agent can neither create, export, grant nor delete anything.

To reach 0.3 from 0.1, follow the 0.2 to 0.3 procedure. It deletes every volume, which also satisfies the detach requirement.

## PillarVolumeState CRD rename (charts before 0.2.0)

Chart 0.2.0 started generating its CRD templates from the API types (issue [#58](https://github.com/isac322/pillar-csi/issues/58)). Earlier charts were maintained by hand and deployed names that differed from the API:

| Kind | Chart 0.1.x | Chart 0.2.0 and later |
|---|---|---|
| PillarVolumeState | CRD `pillarvolumestatestates.pillar-csi.bhyoo.com`, plural `pillarvolumestatestates`, short name `pv` | CRD `pillarvolumestates.pillar-csi.bhyoo.com`, plural `pillarvolumestates`, short name `pvst` |
| PillarAgent | short name `pt` | `pa` |
| PillarStore | short name `pp` | `pst` |
| PillarProtocol | short name `ppr` | `pstr` |
| PillarStorageClass | short name `pb` | `psc` |

The API group, version (`v1alpha1`), kinds and schema did not change with the rename, so no conversion webhook or storage version migration is needed. The `PillarVolumeState` REST path is now `/apis/pillar-csi.bhyoo.com/v1alpha1/pillarvolumestates`; update scripts or extra RBAC rules that use the old name. All chart CRDs now carry `helm.sh/resource-policy: keep`.

### Who is affected

With `installCRDs=true` (the default) on a 0.1.x chart, the old CRD has no keep annotation. `helm upgrade` deletes it because the new manifests no longer contain it, and the API server deletes every `PillarVolumeState` in it. The two CRDs cannot coexist because they share a kind: the new one reports `NamesAccepted=False` with `ListKindConflict` until the old one is gone. No automatic migration exists. If the cluster has no `PillarVolumeState` objects, or you can discard them, `helm uninstall` and a fresh install is enough. To keep them, follow the steps below.

With `installCRDs=false`, the result depends on where your CRDs came from. If you applied `config/crd/bases/`, the old chart's RBAC granted access only to `pillarvolumestatestates`, so the controller's `PillarVolumeState` calls failed with Forbidden; the new chart fixes that. If you applied CRDs extracted from the old chart, run steps 3 and 5 below yourself, and in step 4 apply the new CRD directly and delete the old one instead of running Helm.

If you are going on to 0.3, you delete every volume anyway, so the `PillarVolumeState` objects need no migration.

### Migration that keeps PillarVolumeState objects

Set `RELEASE` and `HELM_NAMESPACE` (the namespace the release is installed in) to your values. The workload namespace is `namespaceOverride` when set, otherwise `HELM_NAMESPACE`. `CreateVolume`, `DeleteVolume` and `ControllerPublishVolume` stop during the migration.

1. Find the controller Deployment and its replica count. The Deployment name can differ from the release name (`fullnameOverride`, `nameOverride`); the chart labels it `app.kubernetes.io/component=controller`. Exactly one must match. If zero or several do, stop and check the install values.

   ```bash
   WORKLOAD_NAMESPACE=$(helm get values "$RELEASE" -n "$HELM_NAMESPACE" -o json \
     | jq -r '.namespaceOverride // empty')
   WORKLOAD_NAMESPACE=${WORKLOAD_NAMESPACE:-$HELM_NAMESPACE}
   DEPLOY=$(kubectl -n "$WORKLOAD_NAMESPACE" get deploy \
     -l app.kubernetes.io/instance="$RELEASE",app.kubernetes.io/component=controller \
     -o jsonpath='{.items[*].metadata.name}')
   [ "$(wc -w <<<"$DEPLOY")" -eq 1 ] || { echo "expected exactly one controller Deployment: $DEPLOY"; exit 1; }
   REPLICAS=$(kubectl -n "$WORKLOAD_NAMESPACE" get deploy "$DEPLOY" -o jsonpath='{.spec.replicas}')
   echo "controller=$DEPLOY namespace=$WORKLOAD_NAMESPACE replicas=$REPLICAS"
   ```

2. Stop the controller so nothing writes `PillarVolumeState`:

   ```bash
   kubectl -n "$WORKLOAD_NAMESPACE" scale deployment/"$DEPLOY" --replicas=0
   kubectl -n "$WORKLOAD_NAMESPACE" rollout status deployment/"$DEPLOY"
   ```

3. Back up the old objects, spec and status:

   ```bash
   kubectl get pillarvolumestatestates.pillar-csi.bhyoo.com -o json > pvs-backup.json
   jq '.items | length' pvs-backup.json
   ```

4. Upgrade with the controller still at zero replicas, and wait for the new CRD. This step deletes the old CRD and its objects.

   ```bash
   helm upgrade "$RELEASE" <chart> -n "$HELM_NAMESPACE" --reuse-values --set controller.replicaCount=0
   kubectl wait --for=condition=Established --timeout=120s crd/pillarvolumestates.pillar-csi.bhyoo.com
   kubectl get crd pillarvolumestatestates.pillar-csi.bhyoo.com   # must be NotFound
   ```

5. Recreate the objects, then restore their status. Status is a subresource and a create request ignores it, so it needs a separate patch.

   ```bash
   jq '.items[] | del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
         .metadata.generation, .metadata.managedFields, .status)' pvs-backup.json \
     | kubectl create -f -
   jq -c '.items[] | select(.status != null) | {name: .metadata.name, status: .status}' pvs-backup.json \
     | while read -r obj; do
         kubectl patch pillarvolumestates "$(jq -r .name <<<"$obj")" --subresource=status --type=merge \
           -p "$(jq -c '{status: .status}' <<<"$obj")"
       done
   kubectl get pvst
   ```

6. Compare the restored count and phases with the backup, then return the controller to its replica count:

   ```bash
   helm upgrade "$RELEASE" <chart> -n "$HELM_NAMESPACE" --reuse-values --set controller.replicaCount="$REPLICAS"
   ```
