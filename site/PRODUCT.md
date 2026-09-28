# Product

<!-- impeccable:product-schema 1 -->

Sources: README.md, /Users/bhyoo/pillar-brand-report/research/consensus.md, revision-brief-1/2/3.md, and the owner's decisions relayed by the orchestrator on 2026-09-28. No live interview took place in this session; every fact below comes from those written owner decisions or from the code at v0.3.0.

## Platform

web

## Users

Primary: homelab operators and self-hosters who run Kubernetes (often k3s) on a few machines, one of which holds disks in a ZFS pool or an LVM volume group. They want that storage available to pods as persistent volumes without running a distributed storage system.

Secondary: small teams running bare-metal Kubernetes who have a storage server and want block volumes from it.

Both groups evaluate on a laptop or desktop before installing, then return to the docs while working on a node over SSH or kubectl.

## Product Purpose

pillar-csi is one CSI driver for the storage you already run. It exports ZFS zvols and LVM volumes from a storage node to pods on other nodes over kernel NVMe-oF/TCP. Every backend and protocol uses the same configuration shape (PillarStore and PillarProtocol defaults, PillarStorageClass overrides, PVC annotations). Success for the site: a visitor understands this in one screen, installs from the OCI Helm chart, and gets a first PVC mounted by following the quickstart.

## Positioning

The data path is kernel to kernel. pillar-agent writes the Linux nvmet target through configfs and reads every write back; the worker's kernel nvme_tcp initiator connects to it. No userspace process sits in the data path, no SSH keys are needed, and the container images carry their own storage and filesystem tools, so hosts need only the kernel modules and an existing pool.

## Operating Context

- Install: `helm install` from `oci://ghcr.io/isac322/charts/pillar-csi`. `agent.backends` defaults to `[]` and must be configured.
- Host requirements: nvme_tcp and nvme_fabrics on workers; nvmet and nvmet_tcp on storage nodes; an existing ZFS pool or LVM VG.
- Components: pillar-agent (DaemonSet on storage nodes), pillar-controller (Deployment), pillar-node (DaemonSet). Cluster-scoped CRDs PillarAgent, PillarStore, PillarProtocol, PillarStorageClass.
- Site: landing at pillar-csi.bhyoo.com, docs under /docs/ (Diataxis sections), English only, deployed from site/ by GitHub Pages.

## Capabilities and Constraints

- Shipped in v0.3.0: ZFS zvol and LVM LV (linear and thin) backends; NVMe-oF/TCP only; ext4 and xfs; raw block; online expansion; RWO, RWOP, ROX (RWX rejected); opt-in mTLS (off by default).
- Planned, never shown as available: iSCSI, NFS, SMB, ZFS datasets. Planned items may appear in body content labeled planned; never in titles, meta descriptions, or H1s. No dates or phase status.
- Not a distributed filesystem: no replication. When a storage node is down its volumes are unavailable. The site says this plainly.
- No published benchmarks: never state performance numbers.

## Brand Commitments

- Name: pillar-csi, lowercase.
- H1: "Export your ZFS and LVM pools to Kubernetes."
- Visual world chosen by the owner on 2026-09-28: "Machinist / Workshop Enamel" (logo sheet: /Users/bhyoo/pillar-brand-report/logo-v5/ImpeccableBrandA/b-machinist/sheet.png). A column turned from one steel bar with a brass collar and a stepped cast foot, in machine-green enamel. Lockup typeface Archivo Expanded Bold. Palette: Deep Enamel #15352B, Machine Green #2D5F4C, Enamel Cream #F1EAD6, Steel #AFBAB2, Brass #D4A849 (one accent on dark), Brass Deep #8A6414 (accent on white), Ink #112A23 (text on white). The earlier navy, cyan and amber world is retired.
- Landing structure agreed in consensus.md section 5 and revision-brief-2: hero with install command and data-path schematic, facts strip, backend by protocol matrix, three steps, comparison table, limits line, footer CTA. Section labels are numbered.
- GitHub star nudge on the landing: prominent and sticky, but no fake numbers, no false urgency, no blocking modal. The live star count appears only once the repository has 100 stars.
- Voice: plain, specific, no puffery. Prose follows the writing-clearly-and-concisely and humanizer skills.

## Evidence on Hand

- Code references for every landing fact (landing.ts `negations[].evidence` links into the repo).
- The comparison table with a "checked on" date.
- No testimonials, customer logos, benchmarks, or verified hardware list. Do not invent them.

## Product Principles

1. Tell the truth about scope: shipped and planned are always distinguishable in words.
2. Show the mechanism: the kernel-to-kernel data path is the product's proof.
3. Use what the visitor already has: their pool, their kernel, one Helm release.
4. One maintainer: prefer static, low-maintenance pages; the only JavaScript is copy buttons, the star count, and scroll hints.

## Accessibility & Inclusion

WCAG 2.1 AA contrast; landing usable at 360px; status never carried by color alone; semantic landmarks; SVG diagrams carry title and desc; prefers-reduced-motion respected.
