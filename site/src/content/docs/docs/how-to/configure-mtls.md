---
title: Configure mTLS between controller and agent
description: Turn on mutual TLS for the pillar-csi controller-to-agent gRPC channel, with certificates issued by cert-manager or supplied as your own Kubernetes Secrets.
sidebar:
  order: 4
---

The controller sends every volume operation to `pillar-agent` over gRPC on port `9500`. By default that channel is plaintext. Set `mtls.enabled=true` to make both ends present certificates signed by one CA. The agent then requires and verifies a client certificate, the controller verifies the agent's server certificate, and both sides accept TLS 1.3 only.

mTLS covers the control channel only. NVMe-oF/TCP and iSCSI data traffic between worker and storage nodes is not encrypted by this setting.

Pick one of two certificate sources: cert-manager, or Secrets you create yourself.

## Option 1: cert-manager

You need cert-manager and its CRDs in the cluster. Then install or upgrade with:

```sh
helm upgrade --install pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \
  --version 0.3.4 \
  --namespace pillar-csi --create-namespace \
  --reuse-values \
  --set mtls.enabled=true \
  --set mtls.certManager.enabled=true
```

With no issuer configured, the chart builds a private CA in the release namespace:

1. a self-signed bootstrap `Issuer` named `<fullname>-mtls-bootstrap`;
2. a CA `Certificate` stored in the Secret `<fullname>-mtls-ca`;
3. a CA `Issuer` named `<fullname>-mtls-ca` that signs the two leaf certificates.

The leaf Secrets are `<fullname>-controller-mtls` (client auth) and `<fullname>-agent-mtls` (server and client auth). With the release name `pillar-csi`, `<fullname>` is `pillar-csi`.

The agent certificate is issued for the DNS name `<fullname>-agent.<namespace>.svc`, while the controller dials the agent by node IP. The chart therefore passes that DNS name to the controller as the TLS server name. You do not set `mtls.serverName` in this mode.

To sign with an issuer you already run, point the chart at it. The chart then skips the bootstrap CA:

```yaml
mtls:
  enabled: true
  certManager:
    enabled: true
    issuerRef:
      name: my-ca-issuer
      kind: ClusterIssuer
      group: cert-manager.io
    duration: 2160h     # default, 90 days
    renewBefore: 360h   # default, 15 days
```

## Option 2: your own Secrets

Create two Secrets in the release namespace before you install. Each must contain the keys `tls.crt`, `tls.key` and `ca.crt`, and both `ca.crt` values must be the CA that signed both certificates.

| Secret | Default name | Certificate needs |
|---|---|---|
| Controller | `pillar-controller-mtls` | extended key usage `clientAuth` |
| Agent | `pillar-agent-mtls` | extended key usage `serverAuth`, and a SAN the controller can match (see below) |

```sh
kubectl -n pillar-csi create secret generic pillar-controller-mtls \
  --from-file=tls.crt=controller.crt \
  --from-file=tls.key=controller.key \
  --from-file=ca.crt=ca.crt
kubectl -n pillar-csi create secret generic pillar-agent-mtls \
  --from-file=tls.crt=agent.crt \
  --from-file=tls.key=agent.key \
  --from-file=ca.crt=ca.crt
```

```sh
helm upgrade --install pillar-csi oci://ghcr.io/isac322/charts/pillar-csi \
  --version 0.3.4 \
  --namespace pillar-csi --create-namespace \
  --reuse-values \
  --set mtls.enabled=true
```

To use other Secret names, set `mtls.secretRefs.controller.secretName` and `mtls.secretRefs.agent.secretName`.

The controller dials each agent at the address it resolves from the `PillarAgent`: the node's `InternalIP` by default (`spec.nodeRef.addressType`), or `spec.external` for an agent outside the cluster. With `mtls.serverName` empty, it checks the agent certificate against that address. You have two ways to make the check pass:

- Put the IP address of every storage node in the agent certificate's SANs.
- Issue the agent certificate for one DNS name and set `mtls.serverName` to that name. The name does not need to resolve; the controller uses it only for certificate verification.

## What the chart changes

With `mtls.enabled=true` the chart mounts the Secrets at `mtls.certDir` (default `/etc/pillar-csi/mtls`) and adds these flags:

| Workload | Flags |
|---|---|
| Controller | `--agent-tls-cert`, `--agent-tls-key`, `--agent-tls-ca`, and `--agent-tls-server-name` when a server name is set |
| Agent | `--tls-cert`, `--tls-key`, `--tls-ca` |

Each binary refuses to start if only some of its three file flags are set. The controller and the agent read the same `mtls.enabled` value, so one Helm release cannot enable mTLS on one side only. An agent outside the cluster (`PillarAgent.spec.external`) is not managed by the chart, so start it with the three `--tls-*` flags yourself.

## Check that mTLS is active

The `AgentConnected` condition on each `PillarAgent` shows how the controller reached the agent. The reason is `Authenticated` when the controller connected over mTLS, or `Dialed` when it connected in plaintext.

```sh
kubectl get pillaragent <name> \
  -o jsonpath='{.status.conditions[?(@.type=="AgentConnected")].reason}'
```

If the handshake fails, `AgentConnected` is `False` with reason `TLSHandshakeFailed`, and the message starts with `mTLS handshake to agent at "<address>" failed; verify certificate chain and CA`. The usual causes are a `ca.crt` that did not sign the other side's certificate, or an agent certificate whose SANs match neither the node IP nor `mtls.serverName`.

Without certificates, the agent also logs a plaintext warning at startup:

```sh
kubectl -n pillar-csi logs ds/pillar-csi-agent -c agent | grep -i plaintext
```

See [Troubleshooting](/docs/how-to/troubleshooting/) for other agent connection failures.

For every chart key used here, see the [Helm values reference](/docs/reference/helm-values/).
