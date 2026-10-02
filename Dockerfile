# syntax=docker/dockerfile:1.27

# Unified multi-target Dockerfile for all pillar-csi components.
#
# Targets:
#   controller — CSI controller (distroless, no runtime deps)
#   agent      — pillar-agent gRPC server (alpine + ZFS + LVM2 + NFS server)
#   node       — CSI node plugin (alpine + mount/NFS utils + e2fsprogs + xfsprogs)
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

# ── Builder stage ─────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24 AS builder

WORKDIR /workspace

# Download dependencies first to maximise build-cache hits on source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
# TARGETVARIANT is set by buildx for sub-architecture levels (e.g. "v3" for
# linux/amd64/v3, "v7" for linux/arm/v7, "v8.2" for linux/arm64/v8.2).  Empty
# for the architecture default (linux/amd64, linux/arm64).
ARG TARGETVARIANT

# Build all binaries under cmd/ in one invocation.  The -o flag with a
# directory target writes each binary (named after its package directory)
# into that directory.  This works with cross-compilation unlike go install
# which rejects GOBIN when GOOS/GOARCH differ from the host.
#
# Map the BuildKit TARGETVARIANT to the matching Go toolchain env var so each
# manifest in the multi-platform image carries a binary tuned for that
# instruction-set level:
#   amd64/v1..v4 → GOAMD64 (Go 1.21+; default v1)
#   arm/v6,v7    → GOARM   (strip the leading 'v')
#   arm64/v8.0..v9.5 → GOARM64 (Go 1.23+; default v8.0)
RUN --mount=type=cache,target=/root/.cache/go-build set -eu; \
    GOAMD64=""; GOARM=""; GOARM64=""; \
    case "$TARGETARCH" in \
      amd64) GOAMD64="${TARGETVARIANT:-v1}" ;; \
      arm)   GOARM="${TARGETVARIANT#v}" ;; \
      arm64) GOARM64="${TARGETVARIANT:-v8.0}" ;; \
    esac; \
    CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" \
    GOAMD64="$GOAMD64" GOARM="$GOARM" GOARM64="$GOARM64" \
    go build \
      -trimpath \
      -pgo=auto \
      -ldflags="-s -w" \
      -o /workspace/bin/ \
      ./cmd/...

# ── Runtime: controller ───────────────────────────────────────────────────────
# Distroless — no shell, no package manager, minimal attack surface.
FROM gcr.io/distroless/static:nonroot AS controller
COPY --from=builder --link /workspace/bin/controller /usr/bin/manager
USER 65532:65532
ENTRYPOINT ["/usr/bin/manager"]

# ── Runtime: agent ────────────────────────────────────────────────────────────
# Alpine + ZFS + LVM2 + nfs-utils userspace tools. The agent invokes zfs(8),
# zpool(8), lvcreate(8), lvremove(8), exportfs(8), rpc.mountd(8) and nfsdcld(8).
# NVMe-oF uses configfs directly; NFS uses the host kernel nfsd and a supervised
# userspace NFSv4 client-recovery daemon with private persistent state.
#
# Runtime security (enforced in the DaemonSet manifest):
#   --security-opt=no-new-privileges:true
#   --read-only  (combine with tmpfs mounts for /tmp, /run)
#   --cap-drop ALL --cap-add SYS_ADMIN  (ZFS + configfs need SYS_ADMIN)
FROM alpine:3.24.2 AS agent
RUN set -eux \
    && apk add --no-cache 'zfs~=2.4' lvm2 nfs-utils \
    # Configure LVM for container environments where udevd is not running.
    && sed -i 's/obtain_device_list_from_udev = 1/obtain_device_list_from_udev = 0/' /etc/lvm/lvm.conf \
    && sed -i 's/udev_sync = 1/udev_sync = 0/' /etc/lvm/lvm.conf \
    && sed -i 's/udev_rules = 1/udev_rules = 0/' /etc/lvm/lvm.conf \
    && addgroup -g 65532 nonroot \
    && adduser  -u 65532 -G nonroot -s /sbin/nologin -D nonroot \
    # Strip SUID/SGID bits from every file on the root filesystem.
    && (find / -xdev \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} + 2>/dev/null || true) \
    # Remove package manager (prevents `apk add` at runtime).
    && rm -rf /sbin/apk /etc/apk /lib/apk /usr/share/apk /var/lib/apk \
    # Remove shell (no interactive escape path).
    && rm -f /bin/sh /bin/bash /usr/bin/env
COPY --from=builder --link --chmod=0555 /workspace/bin/agent /usr/bin/pillar-agent
USER 65532:65532
EXPOSE 2049/tcp
EXPOSE 9500
ENTRYPOINT ["/usr/bin/pillar-agent"]

# ── Runtime: node ─────────────────────────────────────────────────────────────
# Alpine + mount utilities (util-linux) + ext4/XFS formatting and resize tools
# (e2fsprogs, xfsprogs).  xfsprogs-extra carries xfs_growfs on Alpine, and the
# mkfs.xfs LTS profiles under /usr/share/xfsprogs/mkfs: the node plugin formats
# XFS with the Linux 5.15 one so every supported node kernel can mount the
# volume.  hack/verify-mkfs-baseline.sh fails the build when the image's mkfs
# would create a filesystem Linux 5.15 cannot mount.
# device-mapper carries dmsetup, which holds the backend device of a local
# attach on the storage node through a linear target.
# nfs-utils supplies mount.nfs for NFSv4.2 stages without host package installs.
#
# Runtime security (enforced in the DaemonSet manifest):
#   --security-opt=no-new-privileges:true
#   --read-only  (combine with tmpfs mounts for /tmp, /run)
#   --cap-drop ALL --cap-add SYS_ADMIN  (mount(8) and NVMe-oF need SYS_ADMIN)
FROM alpine:3.24.2 AS node
RUN --mount=type=bind,source=hack/verify-mkfs-baseline.sh,target=/tmp/verify-mkfs-baseline.sh \
    set -eux \
    && apk add --no-cache \
         'util-linux~=2.42' \
         'e2fsprogs~=1.47' \
         'e2fsprogs-extra~=1.47' \
         'xfsprogs~=7.0' \
         'xfsprogs-extra~=7.0' \
         'device-mapper~=2.03' \
         nfs-utils; \
    # A separate command: the "|| true" below would mask a failure in the chain.
    sh /tmp/verify-mkfs-baseline.sh; \
    addgroup -g 65532 nonroot \
    && adduser  -u 65532 -G nonroot -s /sbin/nologin -D nonroot \
    # Strip SUID/SGID bits from every file on the root filesystem.
    && find / -xdev \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} + 2>/dev/null || true \
    # Remove package manager (prevents `apk add` at runtime).
    && rm -rf /sbin/apk /etc/apk /lib/apk /usr/share/apk /var/lib/apk \
    # Remove shell (no interactive escape path).
    && rm -f /bin/sh /bin/bash /usr/bin/env
COPY --from=builder --link --chmod=0555 /workspace/bin/node /usr/bin/pillar-node
USER 65532:65532
ENTRYPOINT ["/usr/bin/pillar-node"]
