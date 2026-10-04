---
title: Install pillar-csi with Helm
description: Install the pillar-csi Helm chart from its OCI registry, set agent.backends for your ZFS pools or LVM volume groups, then upgrade or uninstall the release.
sidebar:
  order: 1
---

This guide installs the pillar-csi chart from `oci://ghcr.io/isac322/charts/pillar-csi` and configures the storage agent. It assumes your storage nodes are ready: the pool or volume group exists and the kernel modules load. If not, start with [Prepare a ZFS storage node](/docs/how-to/prepare-zfs-node/) or [Prepare an LVM storage node](/docs/how-to/prepare-lvm-node/).

You need Kubernetes 1.24 or later and Helm 3.8 or later. The hosts need nothing beyond the NVMe-oF or iSCSI kernel modules and the pool: the chart's images carry the ZFS, LVM, and filesystem tools, and the driver needs no SSH access, `nvme-cli`, `targetcli`, `iscsiadm` or `open-iscsi`. [Prerequisites](/docs/reference/prerequisites/) lists the details.

## Write a values file

The chart installs nothing useful until you set `agent.backends`. The default is an empty list, and the agent exits at startup when it has no backend. Each entry sets exactly one of `zfs` or `lvm`, using the same keys as `PillarStore.spec.backend`.

A ZFS pool with volumes under the `tank/k8s` dataset:

```yaml
# values.yaml
agent:
  backends:
    - zfs:
        pool: tank
        parentDataset: k8s
```

An LVM volume group with a thin pool:

```yaml
# values.yaml
agent:
  backends:
    - lvm:
        volumeGroup: data-vg
        thinPool: thin0
```

One agent can serve several pools. List one entry per ZFS pool or LVM volume group:

```yaml
# values.yaml
agent:
  backends:
    - zfs:
        pool: tank
        parentDataset: k8s
    - lvm:
        volumeGroup: data-vg
```

Keep these rules in mind:

- `zfs.pool` and `lvm.volumeGroup` are required. Each name may appear only once in the list, and a ZFS pool and an LVM volume group must not share a name. The chart refuses to render a list that breaks either rule.
- `zfs.parentDataset` and `lvm.thinPool` must match the `PillarStore` that uses the pool. On a mismatch the store reports `PoolDiscovered=False` with reason `BackendLayoutMismatch`, and volume creation fails.
- ZFS properties such as `compression` belong on the `PillarStore`, not here. The chart passes the list through unchanged, and the agent rejects a `properties` key.
- Every agent Pod reads the same list, so all storage nodes in one release share one `agent.backends` setting.

## Install the chart

```sh
helm install pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \
  --version 0.5.1 \
  --namespace pillar-csi --create-namespace \
  --values values.yaml \
  --wait
```

Check the workloads:

```sh
kubectl get csidriver pillar-csi.bhyoo.com
kubectl -n pillar-csi rollout status deploy/pillar-csi-controller
kubectl -n pillar-csi rollout status ds/pillar-csi-node
```

The agent DaemonSet, `ds/pillar-csi-agent`, only schedules on nodes labelled `pillar-csi.bhyoo.com/agent-node=true`. The controller adds that label when you create a `PillarAgent` for the node. Until then the DaemonSet has no Pods.

To register storage and create a StorageClass, apply a `PillarAgent`, `PillarStore`, `PillarProtocol`, and `PillarStorageClass`. The [first PVC tutorial](/docs/tutorials/first-pvc/) walks through a complete set, and the [CRD reference](/docs/reference/crd/) documents every field.

## Adjust common settings

Run the controller on two nodes. The manager and the CSI sidecars use leader election, so extra replicas are safe:

```yaml
controller:
  replicaCount: 2
  affinity:
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              app.kubernetes.io/component: controller
          topologyKey: kubernetes.io/hostname
```

Schedule the agent or node plugin on tainted nodes with `agent.tolerations` and `node.tolerations`. The node plugin runs on every schedulable node by default; restrict it with `node.nodeSelector`.

Manage CRDs outside Helm, for example from a GitOps repository, with `installCRDs: false`.

Controller-to-agent gRPC is plaintext by default. To turn on mutual TLS with cert-manager or your own Secrets, see [Configure mTLS](/docs/how-to/configure-mtls/).

[Helm values](/docs/reference/helm-values/) lists every value and its default.

## Change values on a running release

```sh
helm upgrade pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \
  --version 0.5.1 \
  --namespace pillar-csi \
  --values values.yaml \
  --wait
```

The agent DaemonSet carries a checksum of the rendered backend list, so changing `agent.backends` restarts the agent Pods. To move between pillar-csi versions, read [Upgrade pillar-csi](/docs/how-to/upgrade/) first.

## Uninstall

Remove the volumes and pillar-csi resources before the chart, while the controller and agents can still clean up behind them.

1. Delete every PVC that uses a pillar-csi StorageClass, and the Pods that mount them. With `reclaimPolicy: Delete` the driver deletes the zvol or logical volume. Check the `STORAGECLASS` column:

   ```sh
   kubectl get pvc --all-namespaces
   ```

2. Delete the pillar-csi resources in dependency order:

   ```sh
   kubectl delete pillarstorageclass --all
   kubectl delete pillarstore --all
   kubectl delete pillarprotocol --all
   kubectl delete pillaragent --all
   ```

   Deleting a `PillarAgent` also removes the `pillar-csi.bhyoo.com/agent-node` label from its node.

3. Uninstall the release:

   ```sh
   helm uninstall pillar-csi --namespace pillar-csi
   ```

4. The CRDs carry `helm.sh/resource-policy: keep`, so Helm leaves them in place. Delete them when no pillar-csi resources remain:

   ```sh
   kubectl delete crd \
     pillaragents.pillar-csi.bhyoo.com \
     pillarstores.pillar-csi.bhyoo.com \
     pillarprotocols.pillar-csi.bhyoo.com \
     pillarstorageclasses.pillar-csi.bhyoo.com \
     pillarvolumestates.pillar-csi.bhyoo.com
   ```

The chart also leaves host state behind. Storage nodes keep `/var/lib/pillar-csi/agent`, and nodes that ran the node plugin keep `/var/lib/pillar-csi/node`. If a node had no `/etc/nvme/hostnqn` or `/etc/nvme/hostid`, the node plugin created them, and they stay too. The same holds for `/etc/iscsi/initiatorname.iscsi`. Your pools and volume groups are not touched.
