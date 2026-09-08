# syntax=docker/dockerfile:1.11

ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.24
ARG KIND_VERSION=v0.33.0

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS kind

ARG KIND_VERSION
# kind v0.33.0 waits only 30 seconds for each node container's systemd boot.
# Nested Docker under an emulated Linux VM can legitimately exceed that while
# still making progress, so build the pinned release with a bounded 5-minute
# provider wait.
RUN set -eux; \
    for attempt in 1 2 3; do \
      if apk add --no-cache git; then break; fi; \
      if [ "${attempt}" = 3 ]; then exit 1; fi; \
      sleep "$((attempt * 3))"; \
    done; \
    for attempt in 1 2 3; do \
      rm -rf /src/kind; \
      if git clone --quiet --depth 1 --branch "${KIND_VERSION}" https://github.com/kubernetes-sigs/kind.git /src/kind; then break; fi; \
      if [ "${attempt}" = 3 ]; then exit 1; fi; \
      sleep "$((attempt * 3))"; \
    done; \
    sed -i \
      's/context.WithTimeout(context.Background(), 30\*time.Second)/context.WithTimeout(context.Background(), 5*time.Minute)/' \
      /src/kind/pkg/cluster/internal/providers/docker/provision.go; \
    grep -q 'context.WithTimeout(context.Background(), 5\*time.Minute)' \
      /src/kind/pkg/cluster/internal/providers/docker/provision.go
WORKDIR /src/kind
RUN go build -trimpath -o /kind ./cmd/kind

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS go

FROM docker:29-dind

ARG KUBERNETES_VERSION=v1.37.0
ARG HELM_VERSION=v3.18.4
ARG TARGETARCH

RUN set -eux; \
    for attempt in 1 2 3; do \
      if apk add --no-cache \
        bash \
        ca-certificates \
        coreutils \
        curl \
        e2fsprogs \
        git \
        jq \
        kmod \
        lvm2 \
        make \
        util-linux; then \
        break; \
      fi; \
      if [ "${attempt}" = 3 ]; then exit 1; fi; \
      sleep "$((attempt * 3))"; \
    done; \
    case "${TARGETARCH}" in \
      amd64|arm64) tool_arch="${TARGETARCH}" ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl --retry 3 --retry-all-errors --retry-delay 2 -fsSL -o /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/${tool_arch}/kubectl"; \
    curl --retry 3 --retry-all-errors --retry-delay 2 -fsSL \
      "https://get.helm.sh/helm-${HELM_VERSION}-linux-${tool_arch}.tar.gz" \
      | tar -xz -C /tmp; \
    mv "/tmp/linux-${tool_arch}/helm" /usr/local/bin/helm; \
    rm -rf "/tmp/linux-${tool_arch}"; \
    chmod 0555 /usr/local/bin/kubectl /usr/local/bin/helm

COPY --from=kind /kind /usr/local/bin/kind
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH \
    DOCKER_TLS_CERTDIR= \
    GOCACHE=/var/cache/pillar-csi/go-build \
    GOMODCACHE=/var/cache/pillar-csi/go-mod

COPY test/docker-e2e/entrypoint.sh /usr/local/bin/pillar-csi-docker-e2e
COPY test/docker-e2e/run.sh /usr/local/bin/pillar-csi-docker-e2e-run
RUN chmod 0555 /usr/local/bin/pillar-csi-docker-e2e /usr/local/bin/pillar-csi-docker-e2e-run \
    && mkdir -p /var/cache/pillar-csi/go-build /var/cache/pillar-csi/go-mod

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/pillar-csi-docker-e2e"]
