# syntax=docker/dockerfile:1.11

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.24

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS go

FROM docker:29-dind

ARG KIND_VERSION=v0.32.0
ARG KUBERNETES_VERSION=v1.36.2
ARG HELM_VERSION=v3.18.4
ARG TARGETARCH

RUN set -eux; \
    apk add --no-cache \
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
      util-linux; \
    case "${TARGETARCH}" in \
      amd64|arm64) tool_arch="${TARGETARCH}" ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/kind \
      "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${tool_arch}"; \
    curl -fsSL -o /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/${tool_arch}/kubectl"; \
    curl -fsSL \
      "https://get.helm.sh/helm-${HELM_VERSION}-linux-${tool_arch}.tar.gz" \
      | tar -xz -C /tmp; \
    mv "/tmp/linux-${tool_arch}/helm" /usr/local/bin/helm; \
    rm -rf "/tmp/linux-${tool_arch}"; \
    chmod 0555 /usr/local/bin/kind /usr/local/bin/kubectl /usr/local/bin/helm

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
