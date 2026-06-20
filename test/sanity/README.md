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
# or
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

## Known driver compliance gaps

Running the suite today surfaces real CSI spec deviations that the driver
itself needs to address (these are not test infrastructure problems):

* `Controller.GetCapacity` rejects empty `parameters`; the spec allows it.
* `Controller.ValidateVolumeCapabilities` returns OK for a non-existent volume;
  the spec mandates `NotFound`.
* `Controller.ControllerPublishVolume` returns `InvalidArgument` for unknown
  volume / node IDs; the spec mandates `NotFound`.
* `Node.NodeExpandVolume` returns the wrong code when the volume is missing.

Fix these in the driver and the corresponding spec entries will turn green.
