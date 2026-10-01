# Contributing to pillar-csi

Bug reports, fixes, and documentation changes are welcome. For a larger change, open an issue first so we can agree on the approach before you write the code.

Report security problems privately as described in [SECURITY.md](SECURITY.md), not in a public issue.

## Development setup

You need Go (the version in [go.mod](go.mod)), `make`, and Docker for the end-to-end tests. The Makefile downloads its other tools (controller-gen, golangci-lint, buf, envtest, ginkgo) into `./bin` on first use. Run `make help` to list every target.

```sh
make test-fast   # unit tests without envtest
make test        # full unit and integration suite, including envtest
make lint        # golangci-lint
```

The API group and CSI provisioner name is `pillar-csi.bhyoo.com`. All CRDs are cluster-scoped: `PillarAgent`, `PillarStore`, `PillarProtocol`, `PillarStorageClass`, and the internal `PillarVolumeState`.

## Generated files

Do not edit these files by hand. CI regenerates them and fails on `git diff --exit-code` if they differ.

| Generated files | Source | Regenerate with |
|---|---|---|
| `config/crd/`, `config/rbac/role.yaml`, `config/webhook/`, `charts/pillar-csi/templates/crds.yaml` | Markers in `api/v1alpha1/*_types.go` and `//+kubebuilder:rbac` markers in the controllers | `make manifests` |
| `api/v1alpha1/zz_generated.deepcopy.go` | Fields in `api/v1alpha1/*_types.go` | `make generate` |
| `gen/go/` | `proto/` | `make proto-gen` |

Scaffold new CRDs, controllers, and webhooks with the `kubebuilder` CLI; the `PROJECT` file tracks the scaffolding. Hand-written code lives in `api/v1alpha1/*_types.go`, `internal/controller/`, `internal/webhook/v1alpha1/`, `internal/agent/` (including the LIO iSCSI target in `internal/agent/lio/`), `internal/csi/`, `internal/iscsi/` (the node's in-process iSCSI initiator), and `proto/`.

When you change `proto/`, run `make proto-lint` and `make proto-breaking` to check wire compatibility.

## Handling failures

pillar-csi writes to kernel interfaces, and a swallowed error there leaves storage in a state nobody expects.

- Never drop a failed `configfs` or `sysfs` write with `continue`, a silent ignore, or a debug log.
- When the resulting state matters, read the value back right after the write and return an error if it differs.
- On cleanup and rollback paths, return any failure that affects storage or connection state to the caller, or at least log it with `log.Error`.
- Include the operation (`write`, `disconnect`, `expand`), the target (`path`, `NQN`, `IQN`, `device`, `volumeID`), and the cause (`%w`) in every error message.

## End-to-end test cases

Each TC ID in [docs/E2E-TESTCASES.md](docs/E2E-TESTCASES.md) maps one-to-one to a Ginkgo node name in `test/e2e/`. When you add or remove a test case, update both and run `make verify-tc-ids`. Gate test cases with `Fail()` or `Expect()`; do not use a conditional `Skip()`.

Check cleanup against the resource owned by the test. For TCP listeners, verify that the retained listener rejects `Accept()` with `net.ErrClosed`; rebinding a released ephemeral address can race with another parallel worker.

## CI checks

Pull requests and pushes to `master` run the same CI workflow. Code changes run lint, unit and integration tests, CSI conformance, chart rendering, benchmarks, and both Kind and upstream External Storage E2E suites. Ordinary source PRs compile all eight platforms through the Dockerfile's builder stage. Image-contract changes, master pushes, and failed change detection run the full eight-platform OCI build. Documentation-only changes skip the code checks; `docs/E2E-TESTCASES.md` still counts as code. Failed change detection runs the checks rather than skipping them. Superseded pull-request runs are cancelled; master and daily runs are retained.

TC coverage verification runs once through `make test`, and its report appears in the Test job summary. The daily workflow also runs the separate Docker multi-node data-path suite.

CI builds the exact checkout's amd64 runtime images once in a job parallel to the platform build. Kind and External Storage E2E consume a run-scoped archive after verifying the source SHA, image tag, and image IDs, then load the images into fresh clusters. Local and daily runs still build their own images. Set `E2E_PREBUILT_IMAGES=true` only when the expected images are already loaded in the Docker daemon; the harness still performs Kind loading. CI also supports manual dispatch for measurements.

The csi-sanity job may opt in with `make test-csi-sanity CSI_SANITY_SKIP_VET=true`. The flag drops only the duplicate untagged `go vet ./...` prerequisite; the required Test job remains the vet authority through `make test`, and the tagged `go test` run still vets the sanity package itself. Local and default runs keep vet.

The Docker suite's internal and external topologies run as independent matrix jobs on separate GitHub-hosted VMs, so storage kernel and configfs state are isolated. Both jobs run the existing NVMe-oF, iSCSI, and data-path scenarios and clean up independently; a failure in one does not cancel the other. Each job repeats image builds and setup, which may increase total job minutes; wall-clock savings must be measured. Local `make test-docker-e2e` still runs both topologies sequentially by default.

Docker suite tests run serially by default. `PILLAR_E2E_PARALLEL=true make test-docker-e2e` runs the top-level tests that call `parallelDockerE2E` (topology, admission, NVMe and iSCSI online expansion, periodic and iSCSI trim, and the wrong-CHAP-secret check) up to four at a time (`go test -parallel=4`). An opted-in test owns its namespace, volumes, targets, and Secrets and never asserts that a node has no connection or controller to a target port, because those checks see every socket to the shared NVMe/TCP and iSCSI ports. Every other test runs alone: Go runs all serial top-level tests while the opted-in tests are paused, so `TestLocalAttachForceDetachFencing`, which stops the storage kubelet and taints the node, and the handoff, disconnect, local-attach, and initiator-rejection tests never overlap another test. New tests stay serial unless they call `parallelDockerE2E` and meet those rules. The flag is opt-in only; the daily workflow keeps the serial default.

Local `make test-docker-e2e` runs keep the nested Docker's pulled images, BuildKit cache (including the Go build cache mount), and the harness's Go module and build caches in Compose volumes, so a rerun rebuilds only what changed. Each run still starts and ends without Kind clusters, containers, networks, or E2E-owned host storage state. Floating base image tags such as `golang:1.27-alpine3.24` are not re-pulled while cached; run `make clean-docker-e2e-cache` to drop every cache and start from scratch. CI runners start with empty volumes.

External Storage E2E uses two workers and a 15 GiB sparse LVM backing file in CI. Local defaults remain one worker and 5 GiB. On a Linux host with the required storage modules, set `VG_SIZE=15G` when bootstrapping and `GINKGO_PROCS=2` when running `make test-external-e2e` to use the CI settings. Parallel execution uses the matching Ginkgo binary from the upstream Kubernetes test bundle and keeps the same focus and skip filters.

CI caches versioned tools and test bundles. The multi-platform build reuses a Go compiler cache keyed by the Dockerfile and Go dependencies, not each commit. The runtime-image job has a separate amd64 compiler cache under the same dependency-key scheme. Go still recompiles changed source packages. Pull requests and manual non-master runs restore these caches without exporting them; master refreshes them when their dependency keys change. Runtime image layers use target-specific caches for the full matrix and a separate scope for the shared amd64 artifact, exported by master. Release builds restore the full-matrix compiler and target-specific runtime caches as read-only consumers; they do not export either cache.

Before installing host kernel modules, the Kind, External Storage, and daily Docker E2E jobs run `hack/ci-prefer-ubuntu-mirror.sh`. On GitHub-hosted Ubuntu runners it changes only the priorities in `/etc/apt/apt-mirrors.txt` so APT tries `archive.ubuntu.com` before `azure.archive.ubuntu.com`. Every mirror stays as a fallback, and sources, signing keys, and packages are unchanged. If the file layout is anything else, the script leaves it alone and logs a notice.

## Before you commit

- `make lint` must report 0 issues.
- If you changed a `_types.go` file, an RBAC marker, or `proto/`, run the matching generator and commit its output with your change.
- Run `make test-fast`, and `make test` for changes that touch controllers or webhooks.
- Start the commit subject with a conventional prefix such as `fix:`, `feat:`, `test:`, `docs:`, or `build(deps):`, and explain why the change is needed rather than restating the diff.

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
