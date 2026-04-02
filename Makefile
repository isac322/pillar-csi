# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest verify-tc-coverage ## Run tests (unit + integration, requires envtest).
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -tags=integration $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-fast
test-fast: fmt vet ## Run fast unit tests only (no envtest; completes in <10s).
	go test -parallel=8 $$(go list ./... | grep -v /e2e) -count=1

.PHONY: test-short
test-short: fmt vet ## Run fast unit tests in short mode (skips slow tests like ConcurrentSafety).
	go test -short -parallel=8 $$(go list ./... | grep -v /e2e) -count=1

##@ E2E Tests

# ── E2E configuration variables ───────────────────────────────────────────────
# Override any of these on the make command line, e.g.:
#   make test-e2e KIND_CLUSTER=my-cluster E2E_IMAGE_TAG=dev

## Kind cluster name used by TestMain for the e2e cluster.
KIND_CLUSTER ?= pillar-csi-e2e

## Image tag applied to pillar-csi-{controller,agent,node} images for e2e.
E2E_IMAGE_TAG ?= e2e

## Set to "true" to skip docker build + kind load (reuse images from a previous run).
## Useful for iterative test development when image content has not changed.
E2E_SKIP_IMAGE_BUILD ?=

## Set to "true" to skip Kind cluster creation and reuse an existing cluster.
## Requires KUBECONFIG and KIND_CLUSTER to point to the live cluster.
## Combine with E2E_SKIP_IMAGE_BUILD=true for the fastest possible iteration:
##   make test-e2e E2E_USE_EXISTING_CLUSTER=true E2E_SKIP_IMAGE_BUILD=true
## Sub-AC 5.4: skips the ~30-60s cluster-create stage.
E2E_USE_EXISTING_CLUSTER ?=

## Set to "true" to enable Docker BuildKit layer caching via --cache-from.
## Reuses unchanged layers from the previous docker build, reducing image build
## time from ~60-90s (fresh) to ~5-15s (cached).
## Sub-AC 5.4: DOCKER_BUILDKIT=1 is set automatically when this is enabled.
E2E_DOCKER_BUILD_CACHE ?=

## Set to "1" to emit a wall-clock pipeline stage timing summary to stderr.
## Shows time spent in cluster-create, image-build, backend-setup, test-exec
## and flags the bottleneck stage.  Sub-AC 5.4.
E2E_STAGE_TIMING ?=

## Helm release name and namespace for the e2e deployment.
E2E_HELM_RELEASE ?= pillar-csi
E2E_HELM_NAMESPACE ?= pillar-csi-system

## Filter tests by name pattern (go test -run value / ginkgo --focus regex).
## Example: make test-e2e E2E_RUN=TestAgentConnected
E2E_RUN ?=

## Override go test timeout per mode (default 30m each).
E2E_TIMEOUT ?= 30m

## Docker daemon endpoint.  When unset, Docker uses its default (local Unix
## socket).  Set explicitly when the daemon listens on TCP or a remote host.
## Example: make test-e2e DOCKER_HOST=tcp://localhost:2375

# Common env vars injected into every e2e invocation.
# DOCKER_HOST is forwarded only when explicitly set by the caller.
# GINKGO is forwarded so that TestMain's auto-parallel re-exec logic can find
# the locally-installed ginkgo binary without relying on PATH.
E2E_COMMON_ENV = $(if $(DOCKER_HOST),DOCKER_HOST=$(DOCKER_HOST)) \
	KIND_CLUSTER=$(KIND_CLUSTER) \
	E2E_IMAGE_TAG=$(E2E_IMAGE_TAG) \
	$(if $(E2E_SKIP_IMAGE_BUILD),E2E_SKIP_IMAGE_BUILD=$(E2E_SKIP_IMAGE_BUILD)) \
	$(if $(E2E_USE_EXISTING_CLUSTER),E2E_USE_EXISTING_CLUSTER=$(E2E_USE_EXISTING_CLUSTER)) \
	$(if $(E2E_DOCKER_BUILD_CACHE),E2E_DOCKER_BUILD_CACHE=$(E2E_DOCKER_BUILD_CACHE)) \
	$(if $(E2E_STAGE_TIMING),E2E_STAGE_TIMING=$(E2E_STAGE_TIMING)) \
	E2E_HELM_RELEASE=$(E2E_HELM_RELEASE) \
	E2E_HELM_NAMESPACE=$(E2E_HELM_NAMESPACE) \
	CERT_MANAGER_INSTALL_SKIP=true \
	GINKGO=$(GINKGO) \
	PILLAR_E2E_PROCS=$(E2E_PROCS) \
	E2E_FAILFAST=$(E2E_FAIL_FAST) \
	E2E_FAIL_FAST=$(E2E_FAIL_FAST)

# Common go test flags shared by every e2e invocation.
E2E_GO_FLAGS = -tags=e2e ./test/e2e/ -v -timeout=$(E2E_TIMEOUT) $(if $(E2E_RUN),-run $(E2E_RUN))

# ── ZFS + LVM co-execution strategy ──────────────────────────────────────────
#
# ZFS and LVM e2e tests run SEQUENTIALLY (not in parallel) in a single
# `go test` invocation.  They share:
#
#   1. The same Kind cluster (1 control-plane + 2 worker nodes).
#   2. The same storage worker node (both /dev/zfs and /dev/mapper mounted).
#   3. The same agent DaemonSet pod (both ZFS and LVM backends registered on
#      the same agent process via separate --backend flags).
#   4. The same NVMe-oF protocol stack (shared configfs paths and TCP ports).
#
# Running them in parallel across separate goroutines or processes would risk
# NVMe-oF port/configfs namespace collisions where one backend's target
# creation interferes with another's.  Ginkgo's default sequential spec
# execution within a single binary guarantees correct ordering: the ZFS
# Describe block fully completes before the LVM Describe block begins, with
# no additional synchronisation code required.
#
# Use E2E_RUN=<pattern> to filter individual backend suites for local debugging:
#   make test-e2e-internal E2E_RUN=LVM    # run only LVM specs
#   make test-e2e-internal E2E_RUN=ZFS    # run only ZFS specs
#
# test-e2e runs the full e2e suite in BOTH internal-agent and external-agent
# modes sequentially, reusing a single Kind cluster across both runs.
#
# The first run (internal-agent) creates the Kind cluster if it doesn't
# already exist, builds and loads images, and sets up storage pools.
# Teardown only uninstalls the Helm chart — the cluster, images, and
# storage pools are preserved.
#
# The second run (external-agent) reuses the existing cluster, skips image
# builds (images already loaded on Kind nodes), and only reinstalls the
# Helm chart with the external-agent overlay.  This saves ~130s vs
# creating a fresh cluster.
#
# The Kind cluster is deleted at the end by an explicit `kind delete cluster`.
# For iterative development, run test-e2e-internal or test-e2e-external
# directly — the cluster stays alive between runs.
# test-e2e-sequential runs the full e2e suite (Ginkgo + non-Ginkgo tests)
# in a single process via `go test`.  Slower but tests non-Ginkgo Test*
# functions that are excluded from the parallel runner.
.PHONY: test-e2e-sequential
test-e2e-sequential: manifests generate fmt vet ## Run e2e tests sequentially via go test.
	@echo "=== e2e: internal-agent mode (sequential) ==="
	$(E2E_COMMON_ENV) go test $(E2E_GO_FLAGS)
	@echo "=== e2e: external-agent mode (sequential) ==="
	$(E2E_COMMON_ENV) E2E_LAUNCH_EXTERNAL_AGENT=true go test $(E2E_GO_FLAGS)
	@echo "=== e2e: cleaning up Kind cluster ==="
	-kind delete cluster --name $(KIND_CLUSTER)

# test-e2e-internal runs only the internal-agent (DaemonSet) mode e2e tests.
# The pillar-agent runs as a DaemonSet inside the Kind cluster.
# Both ZFS and LVM backends are registered; all backend-specific specs execute
# sequentially within the single Ginkgo runner.
.PHONY: test-e2e-internal
test-e2e-internal: manifests generate fmt vet ## Run e2e tests in internal-agent (DaemonSet) mode only.
	$(E2E_COMMON_ENV) go test $(E2E_GO_FLAGS)

# test-e2e-external runs only the external-agent (out-of-cluster) mode e2e tests.
# TestMain starts a Docker container running the agent image, wires it to the
# Kind network, and installs the Helm chart with the external-agent overlay.
.PHONY: test-e2e-external
test-e2e-external: manifests generate fmt vet ## Run e2e tests in external-agent (out-of-cluster Docker container) mode only.
	$(E2E_COMMON_ENV) E2E_LAUNCH_EXTERNAL_AGENT=true go test $(E2E_GO_FLAGS)

# test-e2e-zfs runs only the ZFS-specific e2e specs in internal-agent mode.
# Uses E2E_RUN=ZFS to filter Ginkgo Describe blocks that match "ZFS".
# Useful for local debugging when only the ZFS backend needs verification.
.PHONY: test-e2e-zfs
test-e2e-zfs: manifests generate fmt vet ## Run ZFS-only e2e specs in internal-agent mode (debug helper).
	$(E2E_COMMON_ENV) E2E_RUN=ZFS go test $(E2E_GO_FLAGS)

# test-e2e-lvm runs only the LVM-specific e2e specs in internal-agent mode.
# Uses E2E_RUN=LVM to filter Ginkgo Describe blocks that match "LVM".
# Useful for local debugging when only the LVM backend needs verification.
# Prerequisites: lvm2 installed on host, dm_thin_pool module loaded.
.PHONY: test-e2e-lvm
test-e2e-lvm: manifests generate fmt vet ## Run LVM-only e2e specs in internal-agent mode (debug helper).
	$(E2E_COMMON_ENV) E2E_RUN=LVM go test $(E2E_GO_FLAGS)

## Number of parallel Ginkgo worker processes.  Defaults to nproc (all logical CPUs).
## Each worker gets a unique NVMe-oF TCP port (4421 + worker_index) so
## parallel tests never conflict on NVMe listeners or CRD names.
## Override on the command line: make test-e2e E2E_PROCS=4
E2E_PROCS ?= $(shell nproc)

## AC 10: default continue-on-failure so the full summary report is always emitted.
## E2E_FAILFAST=1 (canonical, no underscore) stops after the first spec failure.
## E2E_FAIL_FAST=true is the legacy alias accepted for backward compatibility.
## Both "1", "true", and "yes" values activate fail-fast mode.
E2E_FAILFAST ?=
E2E_FAIL_FAST ?= false
# If the caller set E2E_FAILFAST (no underscore), forward it as E2E_FAIL_FAST so
# the Ginkgo flag and the Go env var both pick it up.
ifneq ($(E2E_FAILFAST),)
E2E_FAIL_FAST := $(E2E_FAILFAST)
endif

## Directory for aggregated parallel Ginkgo reports.
E2E_REPORT_DIR ?= /tmp/pillar-csi-e2e-reports

## Optional Ginkgo spec focus regex used only by test-e2e-parallel.
## Defaults to E2E_RUN so existing workflows still work.
E2E_GINKGO_FOCUS ?= $(E2E_RUN)

## Optional Ginkgo label-filter expression to restrict which specs run.
## Example: make test-e2e E2E_LABEL_FILTER="TC-F-ZFS-001 || TC-F-LVM-001"
## Example: make test-e2e E2E_LABEL_FILTER="category:envtest"
E2E_LABEL_FILTER ?=

# Common Ginkgo CLI flags shared by the parallel-only suite runner.
E2E_GINKGO_FAIL_FAST_FLAG = $(if $(filter true TRUE 1 yes YES,$(E2E_FAIL_FAST)),--fail-fast,)
E2E_GINKGO_FLAGS = --tags=e2e --procs=$(E2E_PROCS) -v --timeout=2m $(E2E_GINKGO_FAIL_FAST_FLAG) \
	$(if $(E2E_GINKGO_FOCUS),--focus=$(E2E_GINKGO_FOCUS)) \
	$(if $(E2E_LABEL_FILTER),--label-filter=$(E2E_LABEL_FILTER))
E2E_GINKGO_TEST_FLAGS = -- -test.run='^TestE2E$$'

# test-e2e is the canonical e2e entry point.
#
# Phase-sequenced pipeline (all phases run inside a single `go test` invocation):
#
#   Phase 1 — prereq check      : Docker daemon reachable + kernel modules loaded (TestMain)
#   Phase 2 — cluster-create    : Kind cluster creation via bootstrapSuiteCluster (TestMain)
#   Phase 3 — image-build/load  : docker build + kind load for controller/agent/node (TestMain)
#   Phase 4 — backend-setup     : ZFS pool + LVM VG provisioned inside Kind container (TestMain)
#   Phase 5 — parallel test exec: TestMain re-execs the test binary via the ginkgo CLI,
#                                  spawning $(E2E_PROCS) parallel workers that inherit all
#                                  env vars (KUBECONFIG, KIND_CLUSTER, ZFS_POOL, LVM_VG).
#   Phase 6 — teardown          : cluster + backend cleaned up by TestMain deferred cleanup.
#
# Environment variables wired to go test ./test/e2e/...:
#   DOCKER_HOST              — daemon endpoint (env-only, never hardcoded)
#   KIND_CLUSTER             — Kind cluster name
#   E2E_IMAGE_TAG            — image tag for all three component images
#   E2E_SKIP_IMAGE_BUILD     — "true" skips docker build + kind load (reuse previous images)
#   E2E_USE_EXISTING_CLUSTER — "true" skips Kind cluster creation (reuse live cluster)
#   E2E_DOCKER_BUILD_CACHE   — "true" enables --cache-from for faster rebuilds
#   E2E_STAGE_TIMING         — "1" emits wall-clock breakdown per pipeline stage
#   E2E_HELM_RELEASE         — Helm release name
#   E2E_HELM_NAMESPACE       — Helm release namespace
#   GINKGO                   — absolute path to the ginkgo binary (used by reexecViaGinkgoCLI)
#   PILLAR_E2E_PROCS         — parallel worker count (default: nproc)
#   E2E_FAIL_FAST            — "true" stops after the first spec failure
#
# Common usage:
#   make test-e2e                                        # full pipeline, all phases
#   make test-e2e E2E_RUN=ZFS                            # ZFS specs only
#   make test-e2e E2E_RUN=TC-F-ZFS-001                  # single TC
#   make test-e2e E2E_STAGE_TIMING=1                     # emit stage timing summary
#   make test-e2e E2E_USE_EXISTING_CLUSTER=true \
#                 E2E_SKIP_IMAGE_BUILD=true              # fast iteration (skip phases 2-3)
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ginkgo ## Phase-sequenced e2e: prereq→cluster→images→backends→parallel tests→teardown.
	@mkdir -p "$(E2E_REPORT_DIR)"
	@echo "=== e2e pipeline: prereq → cluster-create → image-build → backend-setup → $(E2E_PROCS)-worker tests → teardown ==="
	@_e2e_pid=; \
	_e2e_cleanup() { \
		if [ -n "$$_e2e_pid" ]; then \
			kill -TERM "$$_e2e_pid" 2>/dev/null || true; \
			wait "$$_e2e_pid" 2>/dev/null || true; \
		fi; \
		$(if $(DOCKER_HOST),DOCKER_HOST=$(DOCKER_HOST) )kind delete cluster --name $(KIND_CLUSTER) 2>/dev/null || true; \
	}; \
	trap '_e2e_cleanup' EXIT INT TERM; \
	$(E2E_COMMON_ENV) E2E_DOCKER_BUILD_CACHE=true go test -tags=e2e -v -count=1 -timeout=$(E2E_TIMEOUT) \
		$(if $(E2E_RUN),-run $(E2E_RUN)) \
		./test/e2e/... & \
	_e2e_pid=$$!; \
	wait $$_e2e_pid

# test-e2e-parallel is kept as an alias for backward compatibility.
.PHONY: test-e2e-parallel
test-e2e-parallel: test-e2e ## Alias for test-e2e (backward compatibility).

## E2E_BENCH_LIMIT is the maximum wall-clock seconds allowed for a test-e2e-bench run.
## Override on the command line: make test-e2e-bench E2E_BENCH_LIMIT=90
E2E_BENCH_LIMIT ?= 120

# test-e2e-bench is the CI baseline benchmark target.
# It runs the in-process (no Kind cluster) e2e suite via plain `go test`
# (no -tags=e2e, so Kind bootstrap hooks are excluded) and fails the build if
# total wall-clock time exceeds E2E_BENCH_LIMIT seconds.
#
# Design rationale:
#   • PILLAR_E2E_SEQUENTIAL=true disables TestMain's auto-parallel re-exec so
#     specs run in-process without spawning ginkgo workers. This measures raw
#     sequential throughput and avoids the compilation overhead of re-exec.
#   • No -tags=e2e → SynchronizedBeforeSuite in kind_bootstrap_e2e_test.go is
#     excluded; only the in-process default-profile Ginkgo specs run.
#   • -count=1 disables test-result caching so every CI run measures real time.
#   • -timeout=2m matches suiteLevelTimeout defined in suite_test.go, giving
#     the Go testing runtime a hard ceiling independent of the wall-clock check.
#   • Wall-clock is measured with `date +%s` rather than Go's -benchtime because
#     this target validates end-to-end runner time including subprocess startup.
#   • Exit behaviour: test failure (non-zero rc) OR budget exceeded both cause
#     make to exit non-zero.  The budget check is printed to stderr so CI logs
#     surface the threshold violation clearly.
.PHONY: test-e2e-bench
test-e2e-bench: ## CI baseline benchmark: run in-process e2e suite sequentially, fail if wall-clock > E2E_BENCH_LIMIT seconds.
	@echo "=== e2e-bench: starting (limit=$(E2E_BENCH_LIMIT)s, timeout=2m, sequential) ==="
	@_start=$$(date +%s); \
	PILLAR_E2E_SEQUENTIAL=true go test ./test/e2e/... -count=1 -timeout=2m -v; \
	_rc=$$?; \
	_elapsed=$$(( $$(date +%s) - $$_start )); \
	echo "=== e2e-bench: wall-clock $${_elapsed}s (limit=$(E2E_BENCH_LIMIT)s) ==="; \
	if [ "$$_elapsed" -gt "$(E2E_BENCH_LIMIT)" ]; then \
		printf "FAIL: e2e bench exceeded $(E2E_BENCH_LIMIT)s budget (took %ds)\n" "$$_elapsed" >&2; \
		exit 1; \
	fi; \
	exit "$$_rc"

# ── Sub-AC 5.4: pipeline profiling and fast-iteration targets ─────────────────
#
# test-e2e-profile: full test-e2e with E2E_STAGE_TIMING=1 to emit a wall-clock
# breakdown of all pipeline stages (cluster-create, image-build, backend-setup,
# test-exec) and flag the bottleneck.  Useful for identifying where time is
# spent before applying the skip flags below.
#
# Usage:
#   make test-e2e-profile                          # profile all stages
#   make test-e2e-profile E2E_STAGE_TIMING=1       # explicit (same effect)
#
# Sample output written to stderr after all tests complete:
#   === E2E Pipeline Stage Timing ===
#   total pipeline: 95.2s
#     cluster-create:    34.1s  (35.8%)
#   ▶ image-build:       43.5s  (45.7%)
#     backend-setup:      2.8s  ( 2.9%)
#     test-exec:         14.8s  (15.5%)
#   bottleneck: image-build (43.5s, 45.7% of pipeline)
#   budget: WITHIN 120s (actual 95.2s)
.PHONY: test-e2e-profile
test-e2e-profile: manifests generate fmt vet ginkgo ## Profile make test-e2e pipeline stages; emit bottleneck summary.
	@mkdir -p "$(E2E_REPORT_DIR)"
	@echo "=== e2e-profile: stage timing enabled ==="
	$(E2E_COMMON_ENV) E2E_STAGE_TIMING=1 E2E_LAUNCH_EXTERNAL_AGENT=true \
		"$(GINKGO)" $(E2E_GINKGO_FLAGS) \
		--output-dir="$(E2E_REPORT_DIR)" \
		--json-report=e2e.json \
		--junit-report=e2e.xml \
		./test/e2e/... $(E2E_GINKGO_TEST_FLAGS)

# test-e2e-reuse: fastest iterative development loop.
#
# Skips BOTH cluster creation (E2E_USE_EXISTING_CLUSTER=true) AND docker build
# (E2E_SKIP_IMAGE_BUILD=true), so only test-exec stage runs.  Total wall-clock
# is typically 10-20 seconds (compared to 90-120s for a full run).
#
# Prerequisites:
#   1. A live Kind cluster: make test-e2e once (keeps cluster alive unless
#      KIND_CLUSTER is deleted between runs).
#   2. KUBECONFIG exported: eval $(kind get kubeconfig --name pillar-csi-e2e)
#   3. KIND_CLUSTER set: export KIND_CLUSTER=pillar-csi-e2e
#
# Usage (iterative development):
#   # First run — creates cluster + images:
#   make test-e2e
#
#   # Subsequent runs reuse everything (~10-20s):
#   make test-e2e-reuse KIND_CLUSTER=<cluster-name> KUBECONFIG=<path>
#
# E2E_DOCKER_BUILD_CACHE=true can be added to the first run to cache image
# layers and speed up future full runs:
#   make test-e2e E2E_DOCKER_BUILD_CACHE=true
.PHONY: test-e2e-reuse
test-e2e-reuse: ginkgo ## Reuse existing cluster + images (fastest iteration; skips cluster-create and image-build).
	@mkdir -p "$(E2E_REPORT_DIR)"
	@echo "=== e2e-reuse: USE_EXISTING_CLUSTER=true SKIP_IMAGE_BUILD=true ==="
	$(E2E_COMMON_ENV) \
		E2E_USE_EXISTING_CLUSTER=true \
		E2E_SKIP_IMAGE_BUILD=true \
		E2E_STAGE_TIMING=1 \
		E2E_LAUNCH_EXTERNAL_AGENT=true \
		"$(GINKGO)" $(E2E_GINKGO_FLAGS) \
		--output-dir="$(E2E_REPORT_DIR)" \
		--json-report=e2e.json \
		--junit-report=e2e.xml \
		./test/e2e/... $(E2E_GINKGO_TEST_FLAGS)

# test-e2e-cache: full test-e2e with Docker BuildKit layer caching enabled.
#
# On the first run this builds images normally and caches layers.
# On subsequent runs (when image layers are unchanged) docker build takes
# ~5-15s instead of ~60-90s, cutting total pipeline time significantly.
#
# Requires DOCKER_BUILDKIT support (Docker 18.09+ or Docker Desktop).
.PHONY: test-e2e-cache
test-e2e-cache: manifests generate fmt vet ginkgo ## Run test-e2e with Docker BuildKit layer caching enabled.
	@mkdir -p "$(E2E_REPORT_DIR)"
	@echo "=== e2e-cache: Docker build cache enabled (DOCKER_BUILDKIT=1 + --cache-from) ==="
	$(E2E_COMMON_ENV) \
		E2E_DOCKER_BUILD_CACHE=true \
		E2E_STAGE_TIMING=1 \
		E2E_LAUNCH_EXTERNAL_AGENT=true \
		"$(GINKGO)" $(E2E_GINKGO_FLAGS) \
		--output-dir="$(E2E_REPORT_DIR)" \
		--json-report=e2e.json \
		--junit-report=e2e.xml \
		./test/e2e/... $(E2E_GINKGO_TEST_FLAGS)

##@ TC Coverage Verification
#
# verify-tc-coverage performs a 1-to-1 coverage check between the 437 TC IDs
# documented in docs/E2E-TESTCASES.md and the Ginkgo node labels found in the
# compiled test binary (via ginkgo --dry-run) or in Go source literal strings.
#
# Three modes are available:
#
#   make verify-tc-coverage              → static scan (fast, no ginkgo needed)
#   make verify-tc-coverage-runtime      → runtime scan via ginkgo --dry-run
#   make verify-tc-coverage-strict       → static scan, exit 1 on any mismatch
#
# CI wires verify-tc-coverage into the test job so coverage regressions block
# pull-requests automatically.  Use [skip tc-verify] in the commit message to
# bypass the check during infrastructure-only changes.
#
# Output columns:
#   declared_total  — TC count from "총 테스트 케이스:" line in docs/E2E-TESTCASES.md
#   canonical_cases — de-duplicated TC count after symbol canonicalization
#   bound           — TC IDs with at least one matching Ginkgo node label
#   missing         — TC IDs with no matching Ginkgo node label (regression risk)
#   extra           — Ginkgo node labels whose TC ID is absent from the spec
#   duplicates      — TC IDs whose label appears in more than one node

## Catalogcheck binary path.  Rebuilt on each invocation if sources changed.
CATALOGCHECK_PKG = ./test/e2e/docspec/cmd/catalogcheck

.PHONY: verify-tc-coverage
verify-tc-coverage: ## Check 1-to-1 TC ID coverage (static source scan, report-only).
	@echo "=== TC Coverage Verification (static, report-only) ==="
	go run $(CATALOGCHECK_PKG) --ginkgo
	@echo "=== done ==="

.PHONY: verify-tc-coverage-runtime
verify-tc-coverage-runtime: ginkgo ## Check TC coverage via ginkgo --dry-run (precise, requires ginkgo CLI).
	@echo "=== TC Coverage Verification (runtime via ginkgo --dry-run) ==="
	@mkdir -p /tmp/pillar-csi-verify
	GINKGO_BIN="$(GINKGO)" go run $(CATALOGCHECK_PKG) --ginkgo --runtime
	@echo "=== done ==="

.PHONY: verify-tc-coverage-strict
verify-tc-coverage-strict: ## Check TC coverage (static) — exit 1 on MISSING/EXTRA/DUPLICATE.
	@echo "=== TC Coverage Verification (static, strict) ==="
	go run $(CATALOGCHECK_PKG) --ginkgo --strict
	@echo "=== done ==="

.PHONY: verify-tc-coverage-json
verify-tc-coverage-json: ## Emit TC coverage report as JSON to stdout.
	go run $(CATALOGCHECK_PKG) --ginkgo --json

# verify-tc-ids is the canonical Sub-AC 4 CI gate target.
#
# It re-parses docs/E2E-TESTCASES.md, extracts all 437 TC IDs, and asserts a
# strict 1-to-1 match with Ginkgo node names.  The runtime mode runs
# `ginkgo --dry-run ./test/e2e/` to enumerate actual node labels (including
# dynamically-generated ones via tc.tcNodeName()), then compares against the
# catalogue.  Exit 1 if any TC ID is MISSING, EXTRA, or DUPLICATE.
#
# Equivalent invocations:
#   make verify-tc-ids                   # runtime match, strict
#   go generate ./test/e2e/...           # same, via //go:generate in generate.go
#   ./hack/verify-tc-ids.sh              # same, portable shell wrapper
#   ./hack/verify-tc-ids.sh --json       # JSON-structured report
#
# Prerequisites: install ginkgo first (make ginkgo).
.PHONY: verify-tc-ids
verify-tc-ids: ginkgo ## Sub-AC 4 CI gate: re-parse spec and assert 1-to-1 TC ID ↔ Ginkgo node match (strict, runtime).
	@echo "=== TC ID Verification — spec ↔ Ginkgo node 1-to-1 match (strict, runtime) ==="
	GINKGO_BIN="$(GINKGO)" go run $(CATALOGCHECK_PKG) --ginkgo --runtime --strict
	@echo "=== verify-tc-ids: PASSED ==="

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Protobuf

.PHONY: proto-gen
proto-gen: buf protoc-gen-go protoc-gen-go-grpc ## Compile .proto files and generate Go bindings into gen/go/.
	@mkdir -p gen/go
	PATH="$(LOCALBIN):$$PATH" "$(BUF)" generate

.PHONY: proto-lint
proto-lint: buf ## Lint .proto files with buf (STANDARD rule set).
	"$(BUF)" lint proto

.PHONY: proto-breaking
proto-breaking: buf ## Check .proto files for wire-incompatible changes against the master branch.
	"$(BUF)" breaking proto --against '.git#branch=master'

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd/controller/

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/controller/

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/amd64,linux/arm64,linux/arm/v7,linux/s390x,linux/ppc64le,linux/riscv64
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	- $(CONTAINER_TOOL) buildx create --name pillar-csi-builder
	$(CONTAINER_TOOL) buildx use pillar-csi-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} .
	- $(CONTAINER_TOOL) buildx rm pillar-csi-builder

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
BUF ?= $(LOCALBIN)/buf
PROTOC_GEN_GO ?= $(LOCALBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC ?= $(LOCALBIN)/protoc-gen-go-grpc
GINKGO ?= $(LOCALBIN)/ginkgo

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.19.0
BUF_VERSION ?= v1.66.1
PROTOC_GEN_GO_VERSION ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.11.4
GINKGO_VERSION ?= $(shell go list -m -f '{{.Version}}' github.com/onsi/ginkgo/v2 2>/dev/null || echo v2.28.1)
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: buf
buf: $(BUF) ## Download buf locally if necessary.
$(BUF): $(LOCALBIN)
	$(call go-install-tool,$(BUF),github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION))

.PHONY: protoc-gen-go
protoc-gen-go: $(PROTOC_GEN_GO) ## Download protoc-gen-go locally if necessary.
$(PROTOC_GEN_GO): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO),google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION))

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: $(PROTOC_GEN_GO_GRPC) ## Download protoc-gen-go-grpc locally if necessary.
$(PROTOC_GEN_GO_GRPC): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO_GRPC),google.golang.org/grpc/cmd/protoc-gen-go-grpc,$(PROTOC_GEN_GO_GRPC_VERSION))

.PHONY: ginkgo
ginkgo: $(GINKGO) ## Download ginkgo CLI locally if necessary.
$(GINKGO): $(LOCALBIN)
	$(call go-install-tool,$(GINKGO),github.com/onsi/ginkgo/v2/ginkgo,$(GINKGO_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
