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

Scaffold new CRDs, controllers, and webhooks with the `kubebuilder` CLI; the `PROJECT` file tracks the scaffolding. Hand-written code lives in `api/v1alpha1/*_types.go`, `internal/controller/`, `internal/webhook/v1alpha1/`, `internal/agent/`, `internal/csi/`, and `proto/`.

When you change `proto/`, run `make proto-lint` and `make proto-breaking` to check wire compatibility.

## Handling failures

pillar-csi writes to kernel interfaces, and a swallowed error there leaves storage in a state nobody expects.

- Never drop a failed `configfs` or `sysfs` write with `continue`, a silent ignore, or a debug log.
- When the resulting state matters, read the value back right after the write and return an error if it differs.
- On cleanup and rollback paths, return any failure that affects storage or connection state to the caller, or at least log it with `log.Error`.
- Include the operation (`write`, `disconnect`, `expand`), the target (`path`, `NQN`, `device`, `volumeID`), and the cause (`%w`) in every error message.

## End-to-end test cases

Each TC ID in [docs/E2E-TESTCASES.md](docs/E2E-TESTCASES.md) maps one-to-one to a Ginkgo node name in `test/e2e/`. When you add or remove a test case, update both and run `make verify-tc-ids`. Gate test cases with `Fail()` or `Expect()`; do not use a conditional `Skip()`.

## Before you commit

- `make lint` must report 0 issues.
- If you changed a `_types.go` file, an RBAC marker, or `proto/`, run the matching generator and commit its output with your change.
- Run `make test-fast`, and `make test` for changes that touch controllers or webhooks.
- Start the commit subject with a conventional prefix such as `fix:`, `feat:`, `test:`, `docs:`, or `build(deps):`, and explain why the change is needed rather than restating the diff.

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
