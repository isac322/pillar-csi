---
title: Configure iSCSI
description: Export pillar-csi volumes over iSCSI. Set the port, initiator ACL, login, replacement and NOP-Out timeouts per PillarProtocol, binding or volume, and prepare the nodes.
sidebar:
  order: 5
---

A `PillarProtocol` with an `iscsi` member exports volumes over iSCSI instead of NVMe-oF/TCP. The storage node serves each volume from the kernel LIO target, and pillar-node logs in with its own initiator and hands the session to the kernel. No host needs `targetcli`, `iscsiadm`, `iscsid` or the `open-iscsi` package; the hosts need only kernel modules.

iSCSI volumes support the same features as NVMe-oF/TCP volumes: ZFS zvols and LVM logical volumes, `Filesystem` (ext4 or xfs) and `Block` volume modes, RWO and RWOP, online expansion, volume stats and [local attach](/docs/how-to/local-attach/). CHAP authentication, multipath and multiple portals are not supported. iSCSI traffic is not encrypted.

## Create the protocol and a binding

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: iscsi-default
spec:
  protocol:
    iscsi:
      port: 3260
      acl: true
      replacementTimeout: 120
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: pillar-iscsi
spec:
  storeRef: storage-1-hot
  protocolRef: iscsi-default
  overrides:
    protocol:
      iscsi: {loginTimeout: 30}
```

A `PillarProtocol` sets exactly one protocol member. One with both `nvmeofTcp` and `iscsi`, or neither, is rejected with `exactly one protocol member must be set (supported: nvmeofTcp, iscsi)`. To offer both protocols, create two `PillarProtocol` objects and a `PillarStorageClass` for each.

The agent exports one iSCSI target per volume. Its IQN is `iqn.2026-01.com.bhyoo.pillar-csi:` followed by the volume's pool path with `/` replaced by `.`, for example `iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc`. The target has one portal group (`tpgt_1`) and serves the zvol or logical volume as LUN 0.

## Fields

| Field | Side | Default | Allowed | Per binding or per volume |
|---|---|---|---|---|
| `port` | target | `3260` | 1 to 65535 | no (structural) |
| `acl` | target | `false` | `true`, `false` | no (structural) |
| `loginTimeout` | initiator | unset: 15 seconds | 1 or more | yes |
| `replacementTimeout` | initiator | unset: 120 seconds | 0 or more | yes |
| `noopOutInterval` | initiator | unset: 5 seconds | 0 or more | yes |
| `noopOutTimeout` | initiator | unset: 5 seconds | 0 or more | yes |

`port` is the TCP port of the target's network portal. The agent binds it on the storage node address that the controller resolves from the `PillarAgent`; `PillarProtocol` has no address field.

`acl: true` makes the target admit only the initiator IQNs of nodes the volume is published to. The controller adds a node's IQN to the target on `ControllerPublishVolume` and removes it on unpublish. `acl: false` runs the portal group in demo mode (`generate_node_acls`), so any initiator that can reach the port can log in. Turn on `acl` on any network you do not fully trust.

`loginTimeout` is how many seconds pillar-node waits for a login, the TCP connect plus the iSCSI login exchange, to finish.

`replacementTimeout` is how many seconds the kernel keeps I/O queued while pillar-node re-establishes a failed session. After that, queued I/O fails back to the filesystem. pillar-node keeps trying to log in again after the timeout.

`noopOutInterval` is how many idle seconds pass before the kernel sends a NOP-Out ping on the connection. `noopOutTimeout` is how many seconds it waits for the reply before it declares the connection failed and pillar-node starts recovery.

## When a change takes effect

The controller resolves the effective settings once, in `CreateVolume`, and records them in the volume's `PillarVolumeState` under `spec.resolved`. It also writes the initiator values into the PersistentVolume's volume attributes, which the node reads when it logs in. A change to a `PillarProtocol` therefore applies to volumes provisioned after the change. Existing volumes keep the values they were created with.

To see what a volume actually uses (the `PillarVolumeState` has the same name as the PV):

```sh
kubectl get pvst <pv-name> -o jsonpath='{.spec.resolved.protocol.iscsi}'
kubectl get pv <pv-name> -o jsonpath='{.spec.csi.volumeAttributes}'
```

The volume attributes carry the target IQN as `target_id`, the portal as `address` and `port`, and `pillar-csi.bhyoo.com/protocol-type: iscsi`. The timeouts appear as `pillar-csi.bhyoo.com/iscsi-login-timeout`, `pillar-csi.bhyoo.com/iscsi-replacement-timeout`, `pillar-csi.bhyoo.com/iscsi-noop-out-interval` and `pillar-csi.bhyoo.com/iscsi-noop-out-timeout`, and only when set. See the [annotations reference](/docs/reference/annotations/#volume-attributes).

## Override per binding or per volume

The four timeouts can be overridden with the same nested shape at two more layers. On a `PillarStorageClass`:

```yaml
spec:
  overrides:
    protocol:
      iscsi:
        replacementTimeout: 300
```

On a PVC, as a YAML document in an annotation:

```yaml
metadata:
  annotations:
    pillar-csi.bhyoo.com/protocol: |
      iscsi: {loginTimeout: 30}
```

`port` and `acl` are rejected in both places. A PVC that sets one fails provisioning with a message such as `pillar-csi.bhyoo.com/protocol: iscsi.acl is structural and cannot be set per volume`. The override member must match the protocol: an `iscsi` document on a volume whose `PillarProtocol` uses `nvmeofTcp` fails with `iscsi overrides do not apply to a nvmeof-tcp protocol`, and the reverse fails the same way. To use a different port or ACL policy, create a second `PillarProtocol` and a `PillarStorageClass` that references it.

[Per-volume overrides](/docs/how-to/volume-overrides/) covers precedence across all layers.

## Prepare the nodes

Storage nodes need the LIO modules `target_core_mod`, `target_core_iblock` and `iscsi_target_mod`. Workers need `iscsi_tcp`, which pulls in `libiscsi`, `libiscsi_tcp` and `scsi_transport_iscsi`. The chart's init containers load all of them by default, but they cannot install a module the host lacks. Load them at boot:

```sh
# storage node
printf '%s\n' target_core_mod target_core_iblock iscsi_target_mod > /etc/modules-load.d/pillar-csi-iscsi.conf
# worker
printf '%s\n' iscsi_tcp > /etc/modules-load.d/pillar-csi-iscsi.conf
```

The agent reports `iscsi` in the `PillarAgent` status protocols only when the LIO iSCSI target works on its node:

```sh
kubectl get pillaragent <name> -o jsonpath='{.status.capabilities.protocols}'
```

On each worker, pillar-node needs:

- `iscsi_tcp` loaded before pillar-node starts. If it is missing, pillar-node logs `iSCSI initiator disabled: kernel module iscsi_tcp is not loaded` and cannot stage iSCSI volumes. Load the module and restart the pillar-node Pod.
- `hostNetwork: true`, the chart default. The kernel serves the `NETLINK_ISCSI` socket that pillar-node uses to hand over sessions only in the host's initial network namespace. On Kind or other nodes that run in containers, set `node.iscsi.netlinkNetnsPath` to a host init network namespace file, for example `/host/proc/1/ns/net`.
- An initiator IQN. pillar-node reads `InitiatorName=` from `/etc/iscsi/initiatorname.iscsi` on the host. If the file is missing, it generates `iqn.2026-01.com.bhyoo.pillar-csi:node.<32 hex digits>` and writes it there. It publishes the IQN on the node's CSINode as `pillar-csi.bhyoo.com/iscsi-initiator-iqn`. The controller refuses to publish an iSCSI volume to a node without that annotation and retries.
- A unique IQN on every node. Nodes cloned from one image often share a baked-in `/etc/iscsi/initiatorname.iscsi` (the Kind node image ships one, for example). With `acl: true` the target admits every node that presents an allowed IQN, so a shared IQN defeats the ACL. On nodes that share one kernel, such as Kind nodes, each pillar-node would also treat the other nodes' sessions as its own. Delete the duplicated file before installing and pillar-node generates a unique IQN.

Check the annotation:

```sh
kubectl get csinode <node> -o jsonpath='{.metadata.annotations.pillar-csi\.bhyoo\.com/iscsi-initiator-iqn}'
```

A host can keep running `open-iscsi` and `iscsid` for other storage. pillar-node uses the same initiator IQN and manages only sessions to targets whose IQN starts with `iqn.2026-01.com.bhyoo.pillar-csi:` and that were logged in with that IQN. It leaves every other session alone.

Firewalls must allow the iSCSI port, 3260 by default, from workers to storage nodes.

## Check the result on a worker

pillar-node does not need `iscsiadm`. Read the sessions from sysfs instead. Each `sessionN` directory is one session, and its `targetname` file names the volume's target:

```sh
grep . /sys/class/iscsi_session/session*/targetname
cat /sys/class/iscsi_session/session1/state
cat /sys/class/iscsi_session/session1/recovery_tmo
```

`state` is `LOGGED_IN` for a healthy session. `recovery_tmo` is the `replacementTimeout` the session uses.

When pillar-node restarts, it adopts the pillar-csi sessions that already exist on the node, so staged volumes keep working. If a connection fails, pillar-node logs in again every few seconds until it succeeds or the volume is unstaged.
