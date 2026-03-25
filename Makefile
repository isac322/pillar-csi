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
test: manifests generate fmt vet setup-envtest ## Run tests (unit + integration, requires envtest).
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -tags=integration -parallel=8 $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-fast
test-fast: fmt vet ## Run fast unit tests only (no envtest; completes in <10s).
	go test -parallel=8 $$(go list ./... | grep -v /e2e) -count=1

.PHONY: test-short
test-short: fmt vet ## Run fast unit tests in short mode (skips slow tests like ConcurrentSafety).
	go test -short -parallel=8 $$(go list ./... | grep -v /e2e) -count=1

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= pillar-csi-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

##@ E2E Tests (multi-node Kind, internal + external agent modes)

# ── E2E configuration variables ───────────────────────────────────────────────
# Override any of these on the make command line, e.g.:
#   make test-e2e-setup KIND_CLUSTER=my-cluster E2E_IMAGE_TAG=dev

## Image tag applied to pillar-csi-{controller,agent,node} images for e2e.
E2E_IMAGE_TAG ?= e2e

## Helm release name and namespace for the e2e deployment.
E2E_HELM_RELEASE ?= pillar-csi
E2E_HELM_NAMESPACE ?= pillar-csi-system

## Extra flags forwarded verbatim to hack/e2e-setup.sh.
## Example: E2E_SETUP_EXTRA_ARGS=--skip-image-build
E2E_SETUP_EXTRA_ARGS ?=

## Extra flags forwarded verbatim to go test (both test-e2e-run and test-e2e-external).
## Example: E2E_TEST_ARGS="-run TestMyFeature -timeout 45m"
E2E_TEST_ARGS ?=

## Extra flags forwarded verbatim to hack/e2e-teardown.sh.
## Example: E2E_TEARDOWN_ARGS=--images
E2E_TEARDOWN_ARGS ?=

## External agent Docker container name and gRPC port (used by test-e2e-external).
EXTERNAL_AGENT_NAME ?= pillar-csi-external-agent
EXTERNAL_AGENT_PORT ?= 9500

.PHONY: test-e2e-setup
test-e2e-setup: ## Bootstrap Kind cluster, build+load images, deploy Helm chart (internal-agent mode)
	@chmod +x hack/e2e-setup.sh
	KIND_CLUSTER=$(KIND_CLUSTER) \
	IMAGE_TAG=$(E2E_IMAGE_TAG) \
	HELM_RELEASE=$(E2E_HELM_RELEASE) \
	HELM_NAMESPACE=$(E2E_HELM_NAMESPACE) \
	  hack/e2e-setup.sh $(E2E_SETUP_EXTRA_ARGS)

.PHONY: test-e2e-run
test-e2e-run: ## Run e2e tests against an already-bootstrapped Kind cluster (run test-e2e-setup first)
	KIND_CLUSTER=$(KIND_CLUSTER) \
	E2E_HELM_NAMESPACE=$(E2E_HELM_NAMESPACE) \
	  go test -tags=e2e ./test/e2e/... -v -ginkgo.v \
	  -count=1 -timeout=30m $(E2E_TEST_ARGS)

.PHONY: test-e2e-teardown
test-e2e-teardown: ## Tear down the e2e Kind cluster and remove temporary resources
	@chmod +x hack/e2e-teardown.sh
	KIND_CLUSTER=$(KIND_CLUSTER) \
	EXTERNAL_AGENT_NAME=$(EXTERNAL_AGENT_NAME) \
	  hack/e2e-teardown.sh $(E2E_TEARDOWN_ARGS)

.PHONY: test-e2e-external
test-e2e-external: ## Full lifecycle: setup cluster → start external agent → run external-agent tests → teardown
	@chmod +x hack/e2e-setup.sh hack/e2e-external-agent.sh hack/e2e-teardown.sh
	@# ── Step 1: bootstrap the cluster and load images ─────────────────────────
	KIND_CLUSTER=$(KIND_CLUSTER) \
	IMAGE_TAG=$(E2E_IMAGE_TAG) \
	HELM_RELEASE=$(E2E_HELM_RELEASE) \
	HELM_NAMESPACE=$(E2E_HELM_NAMESPACE) \
	  hack/e2e-setup.sh $(E2E_SETUP_EXTRA_ARGS)
	@# ── Step 2: start external agent + run tests + teardown (single shell so ──
	@#           we can capture the agent address and always run teardown) ───────
	@set -e; \
	echo "==> Starting external agent container '$(EXTERNAL_AGENT_NAME)'..."; \
	KIND_CLUSTER=$(KIND_CLUSTER) \
	EXTERNAL_AGENT_NAME=$(EXTERNAL_AGENT_NAME) \
	EXTERNAL_AGENT_PORT=$(EXTERNAL_AGENT_PORT) \
	  hack/e2e-external-agent.sh; \
	CONTAINER_IP=$$(docker inspect \
	  --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
	  $(EXTERNAL_AGENT_NAME) 2>/dev/null | head -1); \
	EXTERNAL_AGENT_ADDR="$${CONTAINER_IP}:$(EXTERNAL_AGENT_PORT)"; \
	echo "==> External agent address: $${EXTERNAL_AGENT_ADDR}"; \
	echo "==> Running external-agent e2e tests..."; \
	_test_rc=0; \
	KIND_CLUSTER=$(KIND_CLUSTER) \
	EXTERNAL_AGENT_ADDR=$${EXTERNAL_AGENT_ADDR} \
	E2E_HELM_NAMESPACE=$(E2E_HELM_NAMESPACE) \
	  go test -tags=e2e ./test/e2e/... -v -ginkgo.v \
	  -count=1 -timeout=30m \
	  --ginkgo.label-filter=external-agent \
	  $(E2E_TEST_ARGS) \
	|| _test_rc=$$?; \
	echo "==> Tearing down e2e cluster (test exit code: $${_test_rc})..."; \
	KIND_CLUSTER=$(KIND_CLUSTER) \
	EXTERNAL_AGENT_NAME=$(EXTERNAL_AGENT_NAME) \
	  hack/e2e-teardown.sh $(E2E_TEARDOWN_ARGS); \
	exit $${_test_rc}

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
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

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
