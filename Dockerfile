# syntax=docker/dockerfile:1

# Unified multi-target Dockerfile for all pillar-csi components.
#
# Targets:
#   controller — CSI controller (distroless, no runtime deps)
#   agent      — pillar-agent gRPC server (alpine + ZFS + LVM2)
#   node       — CSI node plugin (alpine + mount utils + e2fsprogs)
#
# Usage:
#   docker buildx bake                  # build all 3 images
#   docker buildx bake controller       # build controller only
#   docker build --target=agent .       # build agent only
#
# Security posture (all targets):
#   - Non-root default user: UID/GID 65532 ("nonroot" convention)
#   - Statically-linked Go binary (CGO_ENABLED=0), -trimpath
#   - Versions pinned; update SHA digests when bumping
#
# See docker-bake.hcl for the build matrix and CI integration.

# ── Shared builder stage ──────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26-alpine3.23 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Download dependencies first to maximise build-cache hits on source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# ── Build stages (one per binary) ────────────────────────────────────────────
# BuildKit builds only the stages reachable from the requested --target,
# and deduplicates the shared builder stage when building multiple targets
# via `docker buildx bake`.

FROM builder AS build-controller
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -pgo=auto \
      -ldflags="-s" \
      -o manager \
      cmd/main.go

FROM builder AS build-agent
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -pgo=auto \
      -ldflags="-s -w" \
      -o pillar-agent \
      ./cmd/agent/

FROM builder AS build-node
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -pgo=auto \
      -ldflags="-s -w" \
      -o pillar-node \
      ./cmd/node/

# ── Runtime: controller ───────────────────────────────────────────────────────
# Distroless — no shell, no package manager, minimal attack surface.
FROM gcr.io/distroless/static:nonroot AS controller
COPY --from=build-controller --link /workspace/manager /usr/bin/manager
USER 65532:65532
ENTRYPOINT ["/usr/bin/manager"]

# ── Runtime: agent ────────────────────────────────────────────────────────────
# Alpine + ZFS + LVM2 userspace tools.  The agent invokes zfs(8), zpool(8),
# lvcreate(8), lvremove(8), etc. via os/exec.  NVMe-oF uses configfs directly.
#
# Runtime security (enforced in the DaemonSet manifest):
#   --security-opt=no-new-privileges:true
#   --read-only  (combine with tmpfs mounts for /tmp, /run)
#   --cap-drop ALL --cap-add SYS_ADMIN  (ZFS + configfs need SYS_ADMIN)
FROM alpine:3.21 AS agent
RUN set -eux \
    && apk add --no-cache 'zfs~=2.2' lvm2 \
    # Configure LVM for container environments where udevd is not running.
    && sed -i 's/obtain_device_list_from_udev = 1/obtain_device_list_from_udev = 0/' /etc/lvm/lvm.conf \
    && sed -i 's/udev_sync = 1/udev_sync = 0/' /etc/lvm/lvm.conf \
    && sed -i 's/udev_rules = 1/udev_rules = 0/' /etc/lvm/lvm.conf \
    && addgroup -g 65532 nonroot \
    && adduser  -u 65532 -G nonroot -s /sbin/nologin -D nonroot \
    # Strip SUID/SGID bits from every file on the root filesystem.
    && find / -xdev \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} + 2>/dev/null || true \
    # Remove package manager (prevents `apk add` at runtime).
    && rm -rf /sbin/apk /etc/apk /lib/apk /usr/share/apk /var/lib/apk \
    # Remove shell (no interactive escape path).
    && rm -f /bin/sh /bin/bash /usr/bin/env
COPY --from=build-agent --link --chmod=0555 /workspace/pillar-agent /usr/bin/pillar-agent
USER 65532:65532
EXPOSE 50051
ENTRYPOINT ["/usr/bin/pillar-agent"]

# ── Runtime: node ─────────────────────────────────────────────────────────────
# Alpine + mount utilities (util-linux) + ext4 formatting (e2fsprogs).
#
# Runtime security (enforced in the DaemonSet manifest):
#   --security-opt=no-new-privileges:true
#   --read-only  (combine with tmpfs mounts for /tmp, /run)
#   --cap-drop ALL --cap-add SYS_ADMIN  (mount(8) and NVMe-oF need SYS_ADMIN)
FROM alpine:3.21 AS node
RUN set -eux \
    && apk add --no-cache \
         'util-linux~=2.40' \
         'e2fsprogs~=1.47' \
         'e2fsprogs-extra~=1.47' \
    && addgroup -g 65532 nonroot \
    && adduser  -u 65532 -G nonroot -s /sbin/nologin -D nonroot \
    # Strip SUID/SGID bits from every file on the root filesystem.
    && find / -xdev \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} + 2>/dev/null || true \
    # Remove package manager (prevents `apk add` at runtime).
    && rm -rf /sbin/apk /etc/apk /lib/apk /usr/share/apk /var/lib/apk \
    # Remove shell (no interactive escape path).
    && rm -f /bin/sh /bin/bash /usr/bin/env
COPY --from=build-node --link --chmod=0555 /workspace/pillar-node /usr/bin/pillar-node
USER 65532:65532
ENTRYPOINT ["/usr/bin/pillar-node"]
