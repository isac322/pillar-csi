# csi-sanity

This directory wraps the upstream
[`kubernetes-csi/csi-test`](https://github.com/kubernetes-csi/csi-test) sanity
suite so it can be executed against an **in-process** pillar-csi driver. It is
one of the two industry de-facto CSI conformance suites; the other lives in
[`test/external-e2e/`](../external-e2e/README.md).

## What it verifies

For every RPC that pillar-csi advertises through
`Identity.GetPluginCapabilities`, `Controller.ControllerGetCapabilities` and
`Node.NodeGetCapabilities`, csi-sanity runs a battery of spec-derived
assertions:

* request validation (missing `volume_id`, missing capability, etc.)
* error-code mapping against the CSI specification
* idempotency of `Create*` / `Delete*` / `Publish*` / `Stage*`
* end-to-end happy path (`Create` → `ControllerPublish` → `NodeStage` →
  `NodePublish` → reverse).

No real backend or Kubernetes cluster is required: the controller talks to an
in-process bufconn `AgentService` fake, and the node service runs against
in-memory `Connector` / `Mounter` / `Resizer` fakes.

## Run

```bash
make test-csi-sanity
# or raw:
go test -tags=csi_sanity -timeout=180s -v ./test/sanity/...
```

The `csi_sanity` build tag keeps the `kubernetes-csi/csi-test` dependency out
of the default test/build matrix.

## Files

| File | Purpose |
| --- | --- |
| `sanity_test.go` | `TestCSISanity` entry point — builds the driver, exposes it on a Unix socket, invokes `sanity.Test`. |
| `fake_agent.go` | Minimal `agentv1.AgentServiceServer` that returns success-shaped replies for every RPC the controller dials. |
| `fakes.go` | In-memory `Connector` / `Mounter` / `Resizer` fakes for the node service. |
| `doc.go` | Package documentation (always compiled so `go list` works without the build tag). |

## Coverage

Every spec the driver's advertised capabilities exercise is asserted on every
CI run.  Adding a new RPC or capability is enough to enrol it: csi-sanity
picks the spec set up from `Identity.GetPluginCapabilities`,
`Controller.ControllerGetCapabilities` and `Node.NodeGetCapabilities`.

A single spec is currently `Pending` upstream (CSI `ListVolumes` pagination
edge case); it is owned by the csi-test maintainers, not by this driver.
