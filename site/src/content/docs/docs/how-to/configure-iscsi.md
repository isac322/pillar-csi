---
title: Configure iSCSI
description: Export pillar-csi volumes over iSCSI. Set the port, initiator ACL, CHAP authentication, login, replacement and NOP-Out timeouts per PillarProtocol, binding or volume, and prepare the nodes.
sidebar:
  order: 5
---

A `PillarProtocol` with an `iscsi` member exports volumes over iSCSI instead of NVMe-oF/TCP. The storage node serves each volume from the kernel LIO target, and pillar-node logs in with its own initiator and hands the session to the kernel. No host needs `targetcli`, `iscsiadm`, `iscsid` or the `open-iscsi` package; the hosts need only kernel modules.

iSCSI volumes support the same features as NVMe-oF/TCP volumes: ZFS zvols and LVM logical volumes, `Filesystem` (ext4 or xfs) and `Block` volume modes, RWO and RWOP, online expansion, volume stats and [local attach](/docs/how-to/local-attach/). Logins can be authenticated with [CHAP or mutual CHAP](#authenticate-logins-with-chap). Multipath and multiple portals are not supported. iSCSI traffic is not encrypted.

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

The target advertises thin provisioning (SCSI UNMAP) when the zvol or logical volume supports discard, so the worker's disk accepts discards. Freed space goes back to the pool when the node discards it; pillar-node trims staged filesystem volumes weekly by default, see [Reclaiming freed space](/docs/how-to/volume-overrides/#reclaiming-freed-space). If the backing device does not support discard, the volume is still exported without UNMAP, and the agent logs `iSCSI export without thin provisioning` with the volume and device.

## Fields

| Field | Side | Default | Allowed | Per binding or per volume |
|---|---|---|---|---|
| `port` | target | `3260` | 1 to 65535 | no (structural) |
| `acl` | target | `false` | `true`, `false` | no (structural) |
| `auth.method` | both | `None` | `None`, `CHAP`, `MutualCHAP` | no (structural) |
| `auth.secretRef.name` | both | unset | a Secret name; required unless `method` is `None` | no (structural) |
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

`port`, `acl` and `auth` are rejected in both places. A PVC that sets one fails provisioning with a message such as `pillar-csi.bhyoo.com/protocol: iscsi.acl is structural and cannot be set per volume`. The override member must match the protocol: an `iscsi` document on a volume whose `PillarProtocol` uses `nvmeofTcp` fails with `iscsi overrides do not apply to a nvmeof-tcp protocol`, and the reverse fails the same way. To use a different port, ACL or authentication policy, create a second `PillarProtocol` and a `PillarStorageClass` that references it.

[Per-volume overrides](/docs/how-to/volume-overrides/) covers precedence across all layers.

## Authenticate logins with CHAP

With `acl: true` the target admits a node by its initiator IQN, and anyone who can reach the portal can claim any IQN. CHAP adds a shared secret to the login. pillar-node answers the challenge itself; the nodes still need no iSCSI tools.

| `auth.method` | What is checked |
|---|---|
| `None` (default) | Nothing beyond the ACL. Same as leaving out `auth`. |
| `CHAP` | The target checks the initiator's username and password. |
| `MutualCHAP` | The target checks the initiator, and the initiator checks the target with a second username and password. |

CHAP credentials are set on each node ACL, so `CHAP` and `MutualCHAP` require `acl: true`. The API server rejects a protocol that breaks either rule with `auth.method CHAP and MutualCHAP require acl: true` or `auth.secretRef is required when auth.method is CHAP or MutualCHAP`.

### Create the Secret, protocol and binding

Put the Secret in the namespace pillar-csi is installed in (`pillar-csi` here). The controller reads Secrets only from that namespace.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: iscsi-chap
  namespace: pillar-csi
type: Opaque
stringData:
  username: pillar-initiator
  password: replace-with-a-long-random-secret
  # MutualCHAP only:
  mutualUsername: pillar-target
  mutualPassword: replace-with-another-long-random-secret
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: iscsi-chap
spec:
  protocol:
    iscsi:
      port: 3260
      acl: true
      auth:
        method: MutualCHAP
        secretRef:
          name: iscsi-chap
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: pillar-iscsi-chap
spec:
  storeRef: storage-1-hot
  protocolRef: iscsi-chap
```

Generate the passwords, for example with `openssl rand -base64 24`. For one-way `CHAP`, leave out `mutualUsername` and `mutualPassword`; they are ignored if present.

| Key | Needed for | Rules |
|---|---|---|
| `username` | `CHAP`, `MutualCHAP` | 1 to 255 bytes of valid UTF-8, no NUL or newline, not starting with `NULL` |
| `password` | `CHAP`, `MutualCHAP` | 12 to 255 bytes, otherwise the same rules as `username` |
| `mutualUsername` | `MutualCHAP` | same as `username` |
| `mutualPassword` | `MutualCHAP` | same as `password`, and different from `password` |

The 12-byte minimum is the 96-bit secret length RFC 7143 requires. The same secret in both directions is forbidden by the same RFC.

The generated StorageClass gets two extra parameters, `csi.storage.k8s.io/node-stage-secret-name: iscsi-chap` and `csi.storage.k8s.io/node-stage-secret-namespace: pillar-csi`. kubelet uses them to pass the Secret to pillar-node when it stages a volume. A StorageClass you write by hand must set both parameters itself.

### Check the setup

The protocol reports a missing or invalid Secret in its `Ready` condition with reason `AuthSecretInvalid`. The message names the Secret and the key, never the value:

```sh
kubectl get pillarprotocol iscsi-chap -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
```

Provisioning or attaching a CHAP volume while the Secret is broken fails with `FailedPrecondition` naming the Secret and key, and the target gets no export or ACL without credentials. A node-stage Secret that lacks a key fails staging with `InvalidArgument` naming the key.

Each volume records its method in the volume attribute `pillar-csi.bhyoo.com/iscsi-auth-method` (absent for `None`). The method is fixed when the volume is created: changing `auth` on the `PillarProtocol` later does not change existing volumes.

On the storage node, the target of a CHAP volume has `attrib/authentication` set to `1`, and each ACL holds the username:

```sh
cat /sys/kernel/config/target/iscsi/<target-iqn>/tpgt_1/attrib/authentication
cat /sys/kernel/config/target/iscsi/<target-iqn>/tpgt_1/acls/<initiator-iqn>/auth/userid
```

If the node's credentials do not match the target's, the Pod stays in `ContainerCreating`, no session or disk appears on the node, and the Pod's events show a `FailedMount` with `authentication failed`. kubelet keeps retrying, so the volume mounts once the Secret is fixed.

### Rotate the credentials

pillar-csi reads the Secret each time it needs it. New values reach the target the next time the volume is published to a node (or the agent restores its exports after a restart), and reach the node the next time it stages the volume. A session that is already logged in keeps running, because CHAP is checked only at login.

A session that loses its connection logs in again with the credentials it was staged with. If the target already has the new values by then, for example after an agent restart, that login fails. So after you change the Secret, restart the Pods that use CHAP volumes. Their volumes are unstaged and unpublished, then published and staged again, and both sides pick up the new values.

### Security notes

- CHAP authenticates the login only. It does not encrypt or integrity-protect the data. Keep iSCSI on a separate storage network.
- CHAP uses MD5 challenge-response. The password never crosses the wire, but a captured login exchange can be attacked offline, so use long random passwords.
- The controller sends the credentials to the agent over gRPC, which is plaintext unless you [turn on mTLS](/docs/how-to/configure-mtls/). Turn it on when you use CHAP. pillar-csi never logs the credentials.
- The credentials are stored on the storage node in the LIO configuration under `/sys/kernel/config/target`, and on each worker in pillar-node's stage state under `/var/lib/pillar-csi/node` with mode `0600`, so a restarted pillar-node can log in again. Both are readable only by root.

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
