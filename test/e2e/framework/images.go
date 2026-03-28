//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

// images.go — Centralized registry of all third-party container images used
// in pillar-csi e2e tests.
//
// # Why centralize?
//
// Third-party image references were previously scattered across:
//   - test/e2e/setup_test.go  (thirdPartyImages slice)
//   - test/e2e/framework/dockerexec.go  (hostExecImage constant)
//   - test/e2e/internal_agent_functional_test.go  (inline strings)
//
// Centralizing them here gives a single place to:
//   1. Audit which external images the test suite depends on.
//   2. Update versions without grep-and-replace across files.
//   3. Keep the pre-load list (ThirdPartyImages) in sync with chart values.
//
// # Image categories
//
//   - Utility images  — general-purpose shells / tools used by helper containers
//   - CSI sidecars    — registry.k8s.io/sig-storage/* sidecars bundled with the
//                       Helm chart; must match charts/pillar-csi/values.yaml
//
// Keep this file in sync with charts/pillar-csi/values.yaml.

// ─────────────────────────────────────────────────────────────────────────────
// Utility images
// ─────────────────────────────────────────────────────────────────────────────

const (
	// ImageBusybox is used as:
	//   - init-container image in the Helm chart (kmod / modprobe)
	//   - test pod image in e2e specs that mount PVCs (minimises pull time)
	//
	// The Docker Hub fully-qualified name (docker.io/library/busybox:1.36) is
	// required when passing the image to containerd's `ctr images pull` CLI
	// inside a Kind node; the short form works for Kubernetes pod specs and
	// docker CLI commands.
	ImageBusybox                   = "busybox:1.36"
	ImageBusyboxFullyQualified     = "docker.io/library/busybox:1.36"

	// ImageDebianBookwormSlim is used by DockerHostExec as the privileged
	// helper container image.  debian:bookworm-slim ships nsenter (util-linux)
	// on both amd64 and arm64, which is required for host-namespace operations
	// (zfs, modprobe, nvmetcli).
	ImageDebianBookwormSlim = "debian:bookworm-slim"
)

// ─────────────────────────────────────────────────────────────────────────────
// CSI sidecar images
// ─────────────────────────────────────────────────────────────────────────────

const (
	// CSI external-provisioner watches for PVCs and calls CreateVolume.
	ImageCSIProvisioner = "registry.k8s.io/sig-storage/csi-provisioner:v5.2.0"

	// CSI external-attacher binds/unbinds volumes to nodes (ControllerPublish).
	ImageCSIAttacher = "registry.k8s.io/sig-storage/csi-attacher:v4.8.1"

	// CSI external-resizer triggers ControllerExpandVolume on PVC capacity changes.
	ImageCSIResizer = "registry.k8s.io/sig-storage/csi-resizer:v1.13.2"

	// CSI liveness-probe exposes /healthz and triggers container restarts.
	ImageCSILivenessProbe = "registry.k8s.io/sig-storage/livenessprobe:v2.15.0"

	// CSI node-driver-registrar registers the CSI driver socket with kubelet.
	ImageCSINodeDriverRegistrar = "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0"
)

// ─────────────────────────────────────────────────────────────────────────────
// ThirdPartyImages — complete list for Kind pre-loading
// ─────────────────────────────────────────────────────────────────────────────

// ThirdPartyImages is the authoritative list of all third-party images that
// must be pre-pulled and loaded into every Kind cluster node before Helm
// install.  Pre-loading prevents pod startup delays caused by registry
// rate-limits (Docker Hub HTTP 429) or slow pulls inside Kind containers.
//
// This list must stay in sync with charts/pillar-csi/values.yaml.
// It intentionally excludes:
//   - pillar-csi images (controller, agent, node) — built locally and loaded
//     separately by buildAndLoadImages.
//   - debian:bookworm-slim — pulled on demand by DockerHostExec on the remote
//     Docker host; not run inside Kind nodes.
var ThirdPartyImages = []string{
	// Utility — init containers in the Helm chart DaemonSet/Deployment
	ImageBusybox,

	// CSI sidecars — bundled with the Helm chart
	ImageCSIProvisioner,
	ImageCSIAttacher,
	ImageCSIResizer,
	ImageCSILivenessProbe,
	ImageCSINodeDriverRegistrar,
}
