---
title: Contributing
description: "How to contribute to pillar-csi, the Kubernetes CSI driver for ZFS and LVM over NVMe-oF/TCP, and where to find its design documents, RFCs and test specs."
sidebar:
  order: 2
---

pillar-csi is developed on GitHub at [isac322/pillar-csi](https://github.com/isac322/pillar-csi). Bug reports, questions and pull requests go there.

## Before you start

Read [CONTRIBUTING.md](https://github.com/isac322/pillar-csi/blob/master/CONTRIBUTING.md) for the build setup, the checks a pull request must pass and the commit conventions. Report security problems privately as described in [SECURITY.md](https://github.com/isac322/pillar-csi/blob/master/SECURITY.md), not in a public issue.

To report a bug, [open an issue](https://github.com/isac322/pillar-csi/issues) with the pillar-csi version, your Kubernetes version, the storage node's kernel and backend (ZFS or LVM), and the output of `kubectl describe` for the affected PVC and its `PillarVolumeState`.

## Design documents

The repository keeps its design notes under `docs/`. Several are written in Korean. Some describe protocols and backends that are not implemented, such as NFS and SMB, and they mark those parts as design notes.

| Document | Contents | Language |
| --- | --- | --- |
| [PRD.md](https://github.com/isac322/pillar-csi/blob/master/docs/PRD.md) | Product requirements: CRD design, parameter override layers, components, volume lifecycle | Korean |
| [decisions/](https://github.com/isac322/pillar-csi/tree/master/docs/decisions) | Architecture decision records, starting with the reconciler design | English |
| [RFC-multi-protocol-driver-foundation.md](https://github.com/isac322/pillar-csi/blob/master/docs/RFC-multi-protocol-driver-foundation.md) | How the controller, node plugin and agent dispatch by protocol. NVMe-oF/TCP and iSCSI are implemented | Korean |
| [PRD-iscsi.md](https://github.com/isac322/pillar-csi/blob/master/docs/PRD-iscsi.md) | Design of the iSCSI protocol: configuration, LIO target, in-process initiator, what is supported, and why pillar-node does not use `open-iscsi` | Korean |
| [E2E-TESTCASES.md](https://github.com/isac322/pillar-csi/blob/master/docs/E2E-TESTCASES.md) | The specification every end-to-end test traces back to | Korean |
| [upgrade-crd-names.md](https://github.com/isac322/pillar-csi/blob/master/docs/upgrade-crd-names.md) | Upgrading across the CRD name changes of [issue #58](https://github.com/isac322/pillar-csi/issues/58) | Korean |

The [architecture](/docs/explanation/architecture/) and [fencing and consistency](/docs/explanation/fencing-and-consistency/) pages summarize the parts of these documents that match the current code in English.
