# External Storage e2e

This directory drives the upstream
[SIG-Storage External Storage e2e suite](https://github.com/kubernetes/kubernetes/blob/master/test/e2e/storage/external/README.md)
against a deployed pillar-csi install. It is the second of the two industry
de-facto CSI conformance suites; the first is
[`test/sanity/`](../sanity/README.md).

While csi-sanity proves the driver's gRPC surface matches the CSI specification
in isolation, this suite proves that the **end-to-end Kubernetes storage
contract** (PVC binding, dynamic provisioning, fsGroup, volume expansion,
multi-pod attach, etc.) works in a real cluster.

## Prerequisites

1. A running Kubernetes cluster (Kind, kind-in-Docker, or real) with
   `KUBECONFIG` exported.
2. pillar-csi deployed into that cluster (e.g. via the
   [Helm chart](../../charts/pillar-csi/)).
3. A `PillarTarget` matching the StorageClass `parameters` block in
   [`storage-class.yaml`](./storage-class.yaml) — by default
   `pillar-target-default` with pool `tank`, ZFS zvol backend, NVMe-oF TCP
   protocol.
4. `curl`, `tar` and `kubectl` on PATH.

## Run

```bash
export KUBECONFIG=/path/to/kubeconfig
make test-external-e2e
# or
./test/external-e2e/run.sh
```

`run.sh` will:

1. Discover (or download) the matching `e2e.test` and `ginkgo` binaries from
   `dl.k8s.io` and cache them under `$HOME/.cache/pillar-csi/external-e2e/`.
2. `kubectl apply` the StorageClass.
3. Invoke `e2e.test -storage.testdriver=external-driver.yaml -ginkgo.focus='External.Storage'`.

## Environment overrides

| Variable | Purpose | Default |
| --- | --- | --- |
| `K8S_VERSION` | Kubernetes test bundle version to download. | resolved from `https://dl.k8s.io/release/stable.txt` |
| `GINKGO_FOCUS` | Ginkgo `-focus` regex. | `External.Storage` |
| `GINKGO_SKIP` | Ginkgo `-skip` regex. | _empty_ |
| `E2E_TEST_BIN` | Pre-extracted `e2e.test` binary path; skips download. | _empty_ |
| `CACHE_DIR` | Where to cache downloads. | `$HOME/.cache/pillar-csi/external-e2e` |

## Files

| File | Purpose |
| --- | --- |
| `external-driver.yaml` | Capability declaration consumed by `e2e.test -storage.testdriver=`. Toggle capability flags as the driver implements them. |
| `storage-class.yaml` | StorageClass that the suite uses to provision dynamic PVCs. Edit `parameters` to match your `PillarTarget` / pool / backend / protocol. |
| `run.sh` | Downloads `e2e.test`, applies the StorageClass, runs the suite. |

## Scope

This suite is **the** SIG-Storage conformance gate — it is the same test set
used to certify in-tree storage plugins. Passing it (with the capability flags
honestly set in `external-driver.yaml`) is the strongest external signal that
pillar-csi behaves like a real CSI driver in a real Kubernetes cluster.

If you are bringing up a new backend (LVM, ZFS, etc.) duplicate
`storage-class.yaml` and point `external-driver.yaml.StorageClass.FromFile` at
the copy. The suite can be run per-backend by varying the StorageClass.

## CI

The suite is wired into [`.github/workflows/external-e2e.yml`](../../.github/workflows/external-e2e.yml)
on a `schedule: '0 7 * * *'` (07:00 UTC daily) plus `workflow_dispatch` for
manual on-demand runs. The workflow:

1. Installs Kind, Helm, ZFS / LVM / NVMe-oF kernel modules and pre-pulls all
   sidecar images, identical to the in-PR `e2e` job.
2. Invokes [`bootstrap.sh`](./bootstrap.sh) — creates a Kind cluster,
   provisions a ZFS pool inside the control-plane node container, builds and
   `kind load`s the controller / agent / node images, `helm install`s
   pillar-csi, and applies the PillarTarget / PillarPool / PillarProtocol
   triple that the StorageClass references.
3. Runs `make test-external-e2e` against the prepared cluster.
4. Deletes the Kind cluster on completion (success or failure).

Why scheduled instead of per-PR: the upstream suite is heavy (~10 minutes,
large download, real backend required) and would dominate PR-cycle time
without proportional regression-signal value. csi-sanity covers the gRPC
surface on every PR; external-e2e covers the in-cluster contract nightly.
That matches the cadence chosen by Ceph-CSI, AWS EBS CSI, Longhorn and the
other public CSI drivers that run this suite.
