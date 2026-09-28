# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

delegated: Astro + MDX static build (owner mandated `npm ci && npm run build` → `site/dist`, GitHub Pages). Framework choice delegated by owner; Astro chosen for first-class MDX, Shiki code blocks, Pagefind search, sitemap.

## Users

Primary: homelab and self-hosting people who already run ZFS pools or LVM volume groups and want persistent Kubernetes volumes without bolting on a second storage stack. Secondary: small bare-metal teams. They read docs before they install, distrust storage claims that omit the data path, and own hardware from mini-PCs to racked servers.

## Product Purpose

pillar-csi is a Kubernetes CSI driver that exports ZFS zvols and LVM logical volumes from storage nodes to pods over the kernel NVMe-oF/TCP target. One driver, one Helm release, one configuration model for every backend and protocol. Success: a pod mounts a PVC carved from a pool the user already runs, with no per-driver reinstall when backends or protocols multiply.

## Positioning

The mechanism a neighboring driver cannot copy: the data path is 100% kernel code. The storage node exports through in-kernel `nvmet`; the worker connects through the in-kernel NVMe/TCP initiator via `/dev/nvme-fabrics`. pillar-csi configures both ends, then leaves the I/O path — components can restart without touching connected volumes. One driver replaces the several-per-backend sprawl (one for ZFS, one for LVM, one per protocol) with a single install and a single YAML configuration model shared across backends, protocols, and filesystems.

## Operating Context

- Install: Helm chart (`kubeVersion >= 1.24.0`, Helm 3.8+ for OCI). One release deploys three workloads: `pillar-controller` (Deployment), `pillar-agent` (DaemonSet on labeled storage nodes), `pillar-node` (DaemonSet on workers).
- Host prep is kernel-side only: `nvmet`/`nvmet_tcp` on storage nodes, `nvme_fabrics`/`nvme_tcp` on workers, plus an existing ZFS pool or LVM VG (and `dm_thin_pool` for thin). Images carry every userspace tool (OpenZFS 2.4, lvm2, e2fsprogs, xfsprogs); no SSH, no `nvme-cli`/`nvmetcli`/`targetcli` needed anywhere.
- Configuration: cluster-scoped CRDs `PillarAgent` → `PillarStore` → `PillarProtocol` → `PillarStorageClass`; per-volume overrides via PVC annotations. Docs follow Diátaxis (tutorials / how-to / reference / explanation) plus community.
- Honest limits (must never be hidden): volumes are not replicated — a volume is unavailable while its storage node is down. Planned protocols (iSCSI, NFS, SMB) are not implemented; only `nvmeofTcp` protocol + `zfs`/`lvm` backends exist today. Controller→agent gRPC is plaintext unless mTLS is enabled.

## Capabilities and Constraints

- Backends today: ZFS zvols, LVM logical volumes. Protocol today: NVMe-oF over TCP (port 4420 default, ACL optional). Planned, never claimable as shipped: iSCSI, NFS, SMB.
- ext4 (default) or xfs; Filesystem and Block volume modes; expansion, fencing, durable state via `PillarVolumeState`.
- Site: English only, GitHub Pages static, canonical domain https://pillar-csi.bhyoo.com.
- Never state performance numbers. Never name boards/devices (kernel requirements only).

## Brand Commitments

- Logo "Machinist": `public/brand/mark.svg`, `mark-light.svg`, `logo.svg`, `logo-light.svg`; favicons and `og.png` in `public/`. The mark is a fluted column: cream capital, brass collar, steel shaft, stepped base.
- Palette "Workshop Enamel": Deep Enamel #15352B, Machine Green #2D5F4C, Enamel Cream #F1EAD6, Steel #AFBAB2, Brass #D4A849, Brass Deep #8A6414, Ink #112A23.
- Lockup typeface: Archivo Expanded Bold.
- Owner-mandated landing H1: "Export your ZFS and LVM pools to Kubernetes."
- GitHub star nudge must be unmissable; live star count shown only once the repo has ≥100 stars.

## Evidence on Hand

- Real docs corpus in `src/content/docs/docs/**` (Diátaxis, ~4,700 lines; text must ship byte-identical).
- Real facts: three workloads, CRD table, kernel module table, port table (9500 gRPC, 4420 NVMe-oF), image contents.
- No testimonials, benchmarks, or customer logos exist; none may be fabricated.

## Product Principles

1. Kernel-native honesty: the data path being all-kernel is the claim; say it plainly and never inflate beyond it.
2. One driver, one YAML shape: every communication reinforces "install once, configure every backend/protocol the same way."
3. Planned is labeled: future protocols are a roadmap promise, always marked planned.
4. Respect the operator: kernel modules, ports, and failure modes named exactly; no hand-waving.
5. Homelab first: approachable without dumbing down; the docs corpus is the product's proof.
