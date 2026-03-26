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

// fixtures.go — High-level CR builder fixtures for Kind cluster e2e tests.
//
// This file provides opinionated, realistic helper functions that create
// PillarTarget, PillarPool, PillarProtocol, and PillarBinding objects with
// field values tuned for a Kind cluster running against a real ZFS pool and a
// real NVMe-oF TCP stack.
//
// # Design rationale
//
// The low-level builders in cr.go (NewExternalPillarTarget, NewZFSZvolPool,
// etc.) accept a raw spec and are maximally flexible.  The helpers in this file
// layer sensible defaults on top, reducing boilerplate in test files and
// ensuring consistent configuration across all Kind-based e2e specs.
//
// # Naming convention
//
//   - KindExternal*  — resources that reference an out-of-cluster agent
//   - KindNVMeOF*    — NVMe-oF TCP protocol objects
//   - KindZFS*       — ZFS-backed storage pool objects
//   - KindE2E*       / KindE2EStack — composite "stack" helpers
//
// # Kind-specific choices
//
//   - The NVMe-oF port is 4421 instead of the standard 4420 to avoid conflicts
//     with any production target that may already be bound to 4420 on the host.
//   - ACL is disabled (allow_any_host) so tests do not need to supply an
//     initiator NQN or call AllowInitiator.
//   - ZFS zvol volblocksize=4096 matches the default Kubernetes block-device
//     sector size and minimises amplification on loopback-backed pools.
//   - compression=lz4 reduces I/O on CI hosts without CPU overhead.
//   - sync=disabled eliminates synchronous-write latency on test pools backed
//     by sparse image files.
//
// # Typical usage in a Ginkgo Ordered Describe block
//
//	agentAddr := testEnv.ExternalAgentAddr  // e.g. "10.111.0.1:9500"
//	poolName  := testEnv.ZFSPoolName        // e.g. "e2e-pool"
//
//	target   := framework.KindExternalTarget("my-target", agentAddr)
//	pool     := framework.KindZFSZvolPool("my-pool", target.Name, poolName)
//	proto    := framework.KindNVMeOFTCPProtocol("my-proto")
//	binding  := framework.KindNVMeOFBinding("my-binding", pool.Name, proto.Name)
//
//	Expect(framework.Apply(ctx, suite.Client, target)).To(Succeed())
//	Expect(framework.Apply(ctx, suite.Client, pool)).To(Succeed())
//	Expect(framework.Apply(ctx, suite.Client, proto)).To(Succeed())
//	Expect(framework.Apply(ctx, suite.Client, binding)).To(Succeed())
//
// Or use the all-in-one KindE2EStack helper:
//
//	stack := framework.NewKindE2EStack("my-stack", agentAddr, poolName)
//	for _, obj := range stack.Objects() {
//	    Expect(framework.Apply(ctx, suite.Client, obj)).To(Succeed())
//	}
//	DeferCleanup(func(dctx SpecContext) {
//	    for _, obj := range stack.ReverseObjects() {
//	        _ = framework.EnsureGone(dctx, suite.Client, obj, 2*time.Minute)
//	    }
//	})

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// Address helpers
// ─────────────────────────────────────────────────────────────────────────────

// ParseAgentAddr splits an agent address string of the form "host:port" into
// its host and port components.  It returns (host, port, true) on success and
// ("", 0, false) when addr is empty or cannot be parsed.
//
// Example:
//
//	host, port, ok := framework.ParseAgentAddr("10.111.0.1:9500")
//	// host="10.111.0.1", port=9500, ok=true
func ParseAgentAddr(addr string) (host string, port int32, ok bool) {
	if addr == "" {
		return "", 0, false
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, false
	}
	portU64, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return "", 0, false
	}
	return h, int32(portU64), true //nolint:gosec // port validated as uint16 above
}

// ─────────────────────────────────────────────────────────────────────────────
// KindExternalTarget — PillarTarget pointing at an out-of-cluster agent
// ─────────────────────────────────────────────────────────────────────────────

// KindExternalTarget creates a PillarTarget with spec.external populated from
// the given agent address string ("host:port").
//
// The returned object is ready to be passed to Apply.  If addr cannot be
// parsed the function panics with a descriptive message so test authors
// receive an immediate, actionable error rather than a cryptic Kubernetes
// validation failure later.
//
// Example:
//
//	target := framework.KindExternalTarget("e2e-target", testEnv.ExternalAgentAddr)
func KindExternalTarget(name, addr string) *v1alpha1.PillarTarget {
	host, port, ok := ParseAgentAddr(addr)
	if !ok {
		panic(fmt.Sprintf(
			"framework.KindExternalTarget(%q): cannot parse agent address %q — "+
				"expected \"host:port\" format (e.g. \"10.111.0.1:9500\")",
			name, addr,
		))
	}
	return NewExternalPillarTarget(name, host, port)
}

// ─────────────────────────────────────────────────────────────────────────────
// KindZFSZvolPool — PillarPool with realistic ZFS zvol defaults for Kind
// ─────────────────────────────────────────────────────────────────────────────

// DefaultKindZFSProperties contains the ZFS dataset properties applied to
// every volume created through a Kind e2e pool.
//
// These values are chosen for efficient operation on loopback-backed test pools:
//
//   - volblocksize=4096  — matches the Kubernetes block-device sector size and
//     minimises write amplification when Kubernetes formats a 4 KiB filesystem
//     block on the zvol.
//
//   - compression=lz4    — reduces I/O to the sparse loopback image.  lz4
//     has negligible CPU overhead and is broadly available.
//
//   - sync=disabled      — eliminates synchronous-write latency on test pools
//     that are backed by sparse image files.  The tradeoff (data loss on
//     crash) is acceptable for ephemeral e2e environments.
var DefaultKindZFSProperties = map[string]string{
	"volblocksize": "4096",
	"compression":  "lz4",
	"sync":         "disabled",
}

// KindZFSZvolPool creates a PillarPool backed by ZFS zvols with the
// DefaultKindZFSProperties applied to every provisioned volume.
//
// Parameters:
//
//	name      — Kubernetes name of the PillarPool CR
//	targetRef — name of the PillarTarget this pool lives on
//	zfsPool   — ZFS pool name on the agent host (e.g. "e2e-pool")
//
// The pool uses no parent dataset (volumes are created directly under zfsPool)
// so volume IDs take the form "<zfsPool>/<volumeName>" which is exactly what
// the existing ZFS zvol e2e tests expect.
//
// Example:
//
//	pool := framework.KindZFSZvolPool("e2e-pool-cr", "e2e-target", "e2e-pool")
func KindZFSZvolPool(name, targetRef, zfsPool string) *v1alpha1.PillarPool {
	props := make(map[string]string, len(DefaultKindZFSProperties))
	for k, v := range DefaultKindZFSProperties {
		props[k] = v
	}
	return NewPillarPool(name, v1alpha1.PillarPoolSpec{
		TargetRef: targetRef,
		Backend: v1alpha1.BackendSpec{
			Type: v1alpha1.BackendTypeZFSZvol,
			ZFS: &v1alpha1.ZFSBackendConfig{
				Pool:       zfsPool,
				Properties: props,
			},
		},
	})
}

// KindZFSZvolPoolWithParent is like KindZFSZvolPool but places volumes under
// the given parent dataset.  Use this when the ZFS pool is shared between
// multiple consumers and you want volumes isolated under a dedicated dataset.
//
// Example:
//
//	pool := framework.KindZFSZvolPoolWithParent("e2e-pool-cr", "e2e-target", "e2e-pool", "k8s")
func KindZFSZvolPoolWithParent(name, targetRef, zfsPool, parentDataset string) *v1alpha1.PillarPool {
	props := make(map[string]string, len(DefaultKindZFSProperties))
	for k, v := range DefaultKindZFSProperties {
		props[k] = v
	}
	return NewPillarPool(name, v1alpha1.PillarPoolSpec{
		TargetRef: targetRef,
		Backend: v1alpha1.BackendSpec{
			Type: v1alpha1.BackendTypeZFSZvol,
			ZFS: &v1alpha1.ZFSBackendConfig{
				Pool:          zfsPool,
				ParentDataset: parentDataset,
				Properties:    props,
			},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// KindNVMeOFTCPProtocol — PillarProtocol for NVMe-oF/TCP with Kind-safe defaults
// ─────────────────────────────────────────────────────────────────────────────

// KindNVMeOFPort is the TCP port used for NVMe-oF in Kind e2e tests.
//
// Port 4421 is chosen instead of the standard 4420 to avoid conflicts with any
// production NVMe-oF target that may already be bound to 4420 on the host or
// Kind worker nodes.
const KindNVMeOFPort int32 = 4421

// KindNVMeOFTCPProtocol creates a PillarProtocol for NVMe-oF over TCP with
// settings suitable for Kind cluster testing:
//
//   - Port 4421 (avoids conflicts with standard port 4420)
//   - ACL disabled (allow_any_host = 1) so tests do not need to register
//     an initiator NQN
//   - FSType ext4 (default, broadest compatibility)
//
// Example:
//
//	proto := framework.KindNVMeOFTCPProtocol("e2e-nvme-proto")
func KindNVMeOFTCPProtocol(name string) *v1alpha1.PillarProtocol {
	return NewPillarProtocol(name, v1alpha1.PillarProtocolSpec{
		Type: v1alpha1.ProtocolTypeNVMeOFTCP,
		NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{
			Port: KindNVMeOFPort,
			ACL:  false, // allow_any_host — no AllowInitiator call required
		},
		FSType: "ext4",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// KindNVMeOFBinding — PillarBinding wiring a ZFS pool to NVMe-oF TCP
// ─────────────────────────────────────────────────────────────────────────────

// KindNVMeOFBinding creates a PillarBinding that wires the given pool to the
// given NVMe-oF TCP protocol.  The generated StorageClass takes the binding's
// name.
//
// Example:
//
//	binding := framework.KindNVMeOFBinding("e2e-binding", "e2e-pool-cr", "e2e-nvme-proto")
func KindNVMeOFBinding(name, poolRef, protocolRef string) *v1alpha1.PillarBinding {
	return NewSimplePillarBinding(name, poolRef, protocolRef)
}

// ─────────────────────────────────────────────────────────────────────────────
// KindE2EStack — composite fixture for a complete Target + Pool + Protocol + Binding
// ─────────────────────────────────────────────────────────────────────────────

// KindE2EStack is a pre-built set of CRs that represents a complete
// pillar-csi storage stack for Kind e2e testing.
//
// It bundles:
//   - a PillarTarget (external, pointing at the running agent)
//   - a PillarPool (ZFS zvol, backed by the loopback test pool)
//   - a PillarProtocol (NVMe-oF TCP, port 4421, ACL off)
//   - a PillarBinding (wiring pool to protocol)
//
// All CRs share a common name prefix to make logs and kubectl output readable.
// The prefix must be a valid Kubernetes name segment (lowercase alphanumeric
// and hyphens, start/end with alphanumeric).
//
// Example:
//
//	stack := framework.NewKindE2EStack("my-stack", testEnv.ExternalAgentAddr, testEnv.ZFSPoolName)
//	for _, obj := range stack.Objects() {
//	    Expect(framework.Apply(ctx, suite.Client, obj)).To(Succeed())
//	}
//	DeferCleanup(func(dctx SpecContext) {
//	    for _, obj := range stack.ReverseObjects() {
//	        _ = framework.EnsureGone(dctx, suite.Client, obj, 2*time.Minute)
//	    }
//	})
type KindE2EStack struct {
	// Target is the PillarTarget CR pointing at the out-of-cluster agent.
	Target *v1alpha1.PillarTarget

	// Pool is the PillarPool CR backed by ZFS zvols on the test pool.
	Pool *v1alpha1.PillarPool

	// Proto is the PillarProtocol CR configured for NVMe-oF/TCP on port 4421.
	Proto *v1alpha1.PillarProtocol

	// Binding is the PillarBinding CR wiring Pool to Proto.
	Binding *v1alpha1.PillarBinding
}

// NewKindE2EStack builds a KindE2EStack whose CR names are derived from prefix.
//
// CR names:
//   - Target:  "<prefix>-target"
//   - Pool:    "<prefix>-pool"
//   - Proto:   "<prefix>-proto"
//   - Binding: "<prefix>-binding"
//
// agentAddr must be a "host:port" string (e.g. "10.111.0.1:9500").
// zfsPool must be the ZFS pool name on the agent host (e.g. "e2e-pool").
func NewKindE2EStack(prefix, agentAddr, zfsPool string) *KindE2EStack {
	targetName  := prefix + "-target"
	poolName    := prefix + "-pool"
	protoName   := prefix + "-proto"
	bindingName := prefix + "-binding"

	return &KindE2EStack{
		Target:  KindExternalTarget(targetName, agentAddr),
		Pool:    KindZFSZvolPool(poolName, targetName, zfsPool),
		Proto:   KindNVMeOFTCPProtocol(protoName),
		Binding: KindNVMeOFBinding(bindingName, poolName, protoName),
	}
}

// Objects returns all CRs in dependency order: Target, Pool, Proto, Binding.
//
// Create resources in this order so that references resolve correctly during
// controller reconciliation.
func (s *KindE2EStack) Objects() []client.Object {
	return []client.Object{s.Target, s.Pool, s.Proto, s.Binding}
}

// ReverseObjects returns all CRs in reverse dependency order: Binding, Proto,
// Pool, Target.
//
// Delete resources in this order so that owner/reference chains are unwound
// correctly (innermost dependents first).
func (s *KindE2EStack) ReverseObjects() []client.Object {
	return []client.Object{s.Binding, s.Proto, s.Pool, s.Target}
}

// ─────────────────────────────────────────────────────────────────────────────
// UniqueName — test-safe unique name generator
// ─────────────────────────────────────────────────────────────────────────────

// UniqueName generates a Kubernetes-safe name by appending a millisecond
// timestamp suffix to the given prefix.  The suffix ensures that parallel
// test runs on the same cluster do not collide on resource names.
//
// The returned string is safe as a Kubernetes object name: lower-case
// alphanumeric characters and hyphens, at most 63 characters total.
//
// Example:
//
//	name := framework.UniqueName("e2e-target")
//	// e.g. "e2e-target-823741"
func UniqueName(prefix string) string {
	suffix := time.Now().UnixMilli() % 1_000_000
	name := fmt.Sprintf("%s-%d", prefix, suffix)
	// Truncate to 63 characters (Kubernetes label/name limit).
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// ─────────────────────────────────────────────────────────────────────────────
// Label constants for storage-node scheduling
// ─────────────────────────────────────────────────────────────────────────────

// StorageNodeLabel is the label applied to Kind worker nodes that are
// designated for pillar-agent scheduling.  It matches the label used in
// testdata/kind-config.yaml and the Helm chart's DaemonSet nodeSelector.
const StorageNodeLabel = "pillar-csi.bhyoo.com/storage-node"

// StorageNodeLabelValue is the value of the storage-node label.
const StorageNodeLabelValue = "true"

// ─────────────────────────────────────────────────────────────────────────────
// ObjectKey — convenience wrapper around client.ObjectKeyFromObject
// ─────────────────────────────────────────────────────────────────────────────

// ObjectKey returns the client.ObjectKey (namespace + name) for the given
// object.  It is a thin wrapper around client.ObjectKeyFromObject provided for
// ergonomics so that test files only need to import the framework package.
//
// Example:
//
//	got := &v1alpha1.PillarTarget{}
//	Expect(suite.Client.Get(ctx, framework.ObjectKey(target), got)).To(Succeed())
func ObjectKey(obj client.Object) client.ObjectKey {
	return client.ObjectKeyFromObject(obj)
}

// ─────────────────────────────────────────────────────────────────────────────
// Condition type constants
// ─────────────────────────────────────────────────────────────────────────────

// PillarTarget condition type names used in e2e wait helpers.
const (
	// ConditionAgentConnected is True when the controller has established a
	// gRPC connection to the agent referenced by the PillarTarget.
	ConditionAgentConnected = "AgentConnected"

	// ConditionNodeExists is True when the Kubernetes Node named in
	// spec.nodeRef.name is present in the cluster.
	ConditionNodeExists = "NodeExists"

	// ConditionReady is True when all prerequisite checks have passed and
	// the resource is fully operational.
	ConditionReady = "Ready"
)

// PillarPool condition type names used in e2e wait helpers.
const (
	// ConditionTargetReady is True when the PillarTarget referenced by the
	// pool's spec.targetRef is itself in the Ready state.
	ConditionTargetReady = "TargetReady"

	// ConditionPoolDiscovered is True when the ZFS/LVM pool named in
	// spec.backend has been found on the connected agent.
	ConditionPoolDiscovered = "PoolDiscovered"

	// ConditionBackendSupported is True when the backend type listed in
	// spec.backend.type is among the agent's reported capabilities.
	ConditionBackendSupported = "BackendSupported"
)
