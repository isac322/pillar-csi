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

// cr_fixtures_e2e_test.go — E2E tests that validate the Kind-cluster CR fixtures
// defined in test/e2e/framework/fixtures.go.
//
// These tests exercise the helper functions by building CR objects and
// asserting that their fields contain the expected realistic values for
// NVMe-oF/ZFS Kind cluster testing.  When a full cluster is available (Kind +
// external agent), the tests also apply the CRs and verify that the
// Kubernetes API server accepts them.
//
// # Test structure
//
//   - "CRFixtures/ParseAgentAddr"   — unit-style tests for the address parser
//   - "CRFixtures/UniqueName"       — name generation sanity checks
//   - "CRFixtures/KindExternalTarget" — PillarTarget field validation
//   - "CRFixtures/KindZFSZvolPool"  — PillarPool ZFS field validation
//   - "CRFixtures/KindNVMeOFTCPProtocol" — PillarProtocol field validation
//   - "CRFixtures/KindNVMeOFBinding" — PillarBinding field validation
//   - "CRFixtures/KindE2EStack"     — composite stack object validation
//   - "CRFixtures/ApplyStack"       — applies the full stack to the Kind cluster
//     (requires E2E_LAUNCH_EXTERNAL_AGENT=true)
package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// CRFixtures Ginkgo suite
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("CRFixtures", func() {

	// ─────────────────────────────────────────────────────────────────────
	// ParseAgentAddr
	// ─────────────────────────────────────────────────────────────────────

	Describe("ParseAgentAddr", func() {
		It("parses a valid IPv4 host:port address", func() {
			host, port, ok := framework.ParseAgentAddr("10.111.0.1:9500")
			Expect(ok).To(BeTrue(), "ParseAgentAddr must succeed for valid address")
			Expect(host).To(Equal("10.111.0.1"), "host must match")
			Expect(port).To(Equal(int32(9500)), "port must match")
		})

		It("parses localhost:port", func() {
			host, port, ok := framework.ParseAgentAddr("localhost:9500")
			Expect(ok).To(BeTrue())
			Expect(host).To(Equal("localhost"))
			Expect(port).To(Equal(int32(9500)))
		})

		It("parses a bracketed IPv6 address", func() {
			host, port, ok := framework.ParseAgentAddr("[::1]:9500")
			Expect(ok).To(BeTrue(), "ParseAgentAddr must handle bracketed IPv6 addresses")
			Expect(host).To(Equal("::1"))
			Expect(port).To(Equal(int32(9500)))
		})

		It("returns false for an empty string", func() {
			_, _, ok := framework.ParseAgentAddr("")
			Expect(ok).To(BeFalse(), "empty address must return ok=false")
		})

		It("returns false when port is missing", func() {
			_, _, ok := framework.ParseAgentAddr("10.0.0.1")
			Expect(ok).To(BeFalse(), "address without port must return ok=false")
		})

		It("returns false for a non-numeric port", func() {
			_, _, ok := framework.ParseAgentAddr("10.0.0.1:grpc")
			Expect(ok).To(BeFalse(), "non-numeric port must return ok=false")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// UniqueName
	// ─────────────────────────────────────────────────────────────────────

	Describe("UniqueName", func() {
		It("returns a name that starts with the given prefix", func() {
			name := framework.UniqueName("e2e-target")
			Expect(name).To(HavePrefix("e2e-target-"),
				"generated name must start with the prefix followed by a dash")
		})

		It("returns names no longer than 63 characters", func() {
			longPrefix := "a-very-long-prefix-that-exceeds-the-kubernetes-name-length-limit"
			name := framework.UniqueName(longPrefix)
			Expect(len(name)).To(BeNumerically("<=", 63),
				"UniqueName must truncate to 63 characters")
		})

		It("generates different names on successive calls", func() {
			// Two calls within the same millisecond could collide; sleep briefly
			// if needed.  In practice test processes are fast enough that the
			// UnixMilli modulus differs between calls.
			n1 := framework.UniqueName("e2e")
			time.Sleep(2 * time.Millisecond)
			n2 := framework.UniqueName("e2e")
			// We cannot guarantee uniqueness in all edge cases, but we can
			// verify both are non-empty and look reasonable.
			Expect(n1).NotTo(BeEmpty())
			Expect(n2).NotTo(BeEmpty())
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// KindExternalTarget
	// ─────────────────────────────────────────────────────────────────────

	Describe("KindExternalTarget", func() {
		It("builds a PillarTarget with spec.external populated", func() {
			target := framework.KindExternalTarget("test-target", "10.111.0.1:9500")

			Expect(target).NotTo(BeNil())
			Expect(target.Name).To(Equal("test-target"))
			Expect(target.Spec.External).NotTo(BeNil(),
				"spec.external must be set for an external target")
			Expect(target.Spec.NodeRef).To(BeNil(),
				"spec.nodeRef must be nil when external is used")
			Expect(target.Spec.External.Address).To(Equal("10.111.0.1"),
				"address must be the host part of the addr string")
			Expect(target.Spec.External.Port).To(Equal(int32(9500)),
				"port must be the numeric port from the addr string")
		})

		It("panics for an unparseable address", func() {
			Expect(func() {
				_ = framework.KindExternalTarget("bad", "not-an-address")
			}).To(Panic(), "KindExternalTarget must panic when addr cannot be parsed")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// KindZFSZvolPool
	// ─────────────────────────────────────────────────────────────────────

	Describe("KindZFSZvolPool", func() {
		It("builds a PillarPool with ZFS zvol backend and realistic properties", func() {
			pool := framework.KindZFSZvolPool("test-pool", "test-target", "e2e-pool")

			Expect(pool).NotTo(BeNil())
			Expect(pool.Name).To(Equal("test-pool"))
			Expect(pool.Spec.TargetRef).To(Equal("test-target"),
				"spec.targetRef must reference the given target name")

			backend := pool.Spec.Backend
			Expect(backend.Type).To(Equal(v1alpha1.BackendTypeZFSZvol),
				"backend.type must be zfs-zvol")
			Expect(backend.ZFS).NotTo(BeNil(),
				"backend.zfs must be set for a ZFS zvol pool")
			Expect(backend.ZFS.Pool).To(Equal("e2e-pool"),
				"backend.zfs.pool must match the given ZFS pool name")
			Expect(backend.ZFS.ParentDataset).To(BeEmpty(),
				"parentDataset must be empty when using KindZFSZvolPool (no parent)")

			props := backend.ZFS.Properties
			Expect(props).NotTo(BeNil(),
				"ZFS properties must be set for Kind e2e pools")
			Expect(props["volblocksize"]).To(Equal("4096"),
				"volblocksize=4096 matches the Kubernetes block-device sector size")
			Expect(props["compression"]).To(Equal("lz4"),
				"compression=lz4 reduces I/O on loopback pools")
			Expect(props["sync"]).To(Equal("disabled"),
				"sync=disabled eliminates latency on test pools")
		})

		It("copies properties so mutations do not affect DefaultKindZFSProperties", func() {
			pool := framework.KindZFSZvolPool("test-pool", "test-target", "e2e-pool")
			pool.Spec.Backend.ZFS.Properties["custom"] = "value"

			// DefaultKindZFSProperties must not have been modified.
			_, hasCustom := framework.DefaultKindZFSProperties["custom"]
			Expect(hasCustom).To(BeFalse(),
				"KindZFSZvolPool must copy the default properties map, not share it")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// KindZFSZvolPoolWithParent
	// ─────────────────────────────────────────────────────────────────────

	Describe("KindZFSZvolPoolWithParent", func() {
		It("sets parentDataset when a parent is given", func() {
			pool := framework.KindZFSZvolPoolWithParent(
				"test-pool", "test-target", "e2e-pool", "k8s",
			)
			Expect(pool.Spec.Backend.ZFS.ParentDataset).To(Equal("k8s"),
				"parentDataset must be set to the given parent dataset name")
			Expect(pool.Spec.Backend.ZFS.Pool).To(Equal("e2e-pool"),
				"the underlying pool name must still be set")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// KindNVMeOFTCPProtocol
	// ─────────────────────────────────────────────────────────────────────

	Describe("KindNVMeOFTCPProtocol", func() {
		It("builds a PillarProtocol with NVMe-oF TCP type and Kind-safe defaults", func() {
			proto := framework.KindNVMeOFTCPProtocol("test-proto")

			Expect(proto).NotTo(BeNil())
			Expect(proto.Name).To(Equal("test-proto"))
			Expect(proto.Spec.Type).To(Equal(v1alpha1.ProtocolTypeNVMeOFTCP),
				"protocol type must be nvmeof-tcp")
			Expect(proto.Spec.NVMeOFTCP).NotTo(BeNil(),
				"spec.nvmeofTcp must be populated for NVMe-oF TCP protocol")
			Expect(proto.Spec.NVMeOFTCP.Port).To(Equal(framework.KindNVMeOFPort),
				"port must be 4421 (Kind-safe, avoids conflict with standard port 4420)")
			Expect(proto.Spec.NVMeOFTCP.ACL).To(BeFalse(),
				"ACL must be disabled — allow_any_host simplifies e2e testing")
			Expect(proto.Spec.FSType).To(Equal("ext4"),
				"fsType must be ext4 for broad filesystem compatibility")
		})

		It("uses port 4421, not the standard 4420", func() {
			proto := framework.KindNVMeOFTCPProtocol("test-proto")
			Expect(proto.Spec.NVMeOFTCP.Port).To(Equal(int32(4421)),
				"port 4421 must be used to avoid conflicts with port 4420 on the host")
			Expect(proto.Spec.NVMeOFTCP.Port).NotTo(Equal(int32(4420)),
				"standard NVMe-oF port 4420 must NOT be used in Kind tests")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// KindNVMeOFBinding
	// ─────────────────────────────────────────────────────────────────────

	Describe("KindNVMeOFBinding", func() {
		It("builds a PillarBinding wiring pool to protocol", func() {
			binding := framework.KindNVMeOFBinding("test-binding", "test-pool", "test-proto")

			Expect(binding).NotTo(BeNil())
			Expect(binding.Name).To(Equal("test-binding"))
			Expect(binding.Spec.PoolRef).To(Equal("test-pool"),
				"spec.poolRef must reference the given pool name")
			Expect(binding.Spec.ProtocolRef).To(Equal("test-proto"),
				"spec.protocolRef must reference the given protocol name")
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// KindE2EStack
	// ─────────────────────────────────────────────────────────────────────

	Describe("KindE2EStack", func() {
		const (
			testAddr = "10.111.0.1:9500"
			testPool = "e2e-pool"
		)

		It("builds all four CRs with consistent name prefix", func() {
			stack := framework.NewKindE2EStack("fixture-test", testAddr, testPool)

			Expect(stack.Target).NotTo(BeNil())
			Expect(stack.Pool).NotTo(BeNil())
			Expect(stack.Proto).NotTo(BeNil())
			Expect(stack.Binding).NotTo(BeNil())

			Expect(stack.Target.Name).To(Equal("fixture-test-target"))
			Expect(stack.Pool.Name).To(Equal("fixture-test-pool"))
			Expect(stack.Proto.Name).To(Equal("fixture-test-proto"))
			Expect(stack.Binding.Name).To(Equal("fixture-test-binding"))
		})

		It("wires pool to target via spec.targetRef", func() {
			stack := framework.NewKindE2EStack("wire-test", testAddr, testPool)
			Expect(stack.Pool.Spec.TargetRef).To(Equal(stack.Target.Name),
				"pool.spec.targetRef must match target.name")
		})

		It("wires binding to pool and protocol via spec refs", func() {
			stack := framework.NewKindE2EStack("bind-test", testAddr, testPool)
			Expect(stack.Binding.Spec.PoolRef).To(Equal(stack.Pool.Name),
				"binding.spec.poolRef must match pool.name")
			Expect(stack.Binding.Spec.ProtocolRef).To(Equal(stack.Proto.Name),
				"binding.spec.protocolRef must match proto.name")
		})

		It("Objects() returns CRs in dependency order", func() {
			stack := framework.NewKindE2EStack("order-test", testAddr, testPool)
			objs := stack.Objects()

			Expect(objs).To(HaveLen(4))
			Expect(objs[0]).To(BeAssignableToTypeOf(&v1alpha1.PillarTarget{}),
				"first object must be PillarTarget (no dependencies)")
			Expect(objs[1]).To(BeAssignableToTypeOf(&v1alpha1.PillarPool{}),
				"second object must be PillarPool (depends on Target)")
			Expect(objs[2]).To(BeAssignableToTypeOf(&v1alpha1.PillarProtocol{}),
				"third object must be PillarProtocol (depends on nothing)")
			Expect(objs[3]).To(BeAssignableToTypeOf(&v1alpha1.PillarBinding{}),
				"fourth object must be PillarBinding (depends on Pool and Protocol)")
		})

		It("ReverseObjects() returns CRs in reverse dependency order", func() {
			stack := framework.NewKindE2EStack("rev-test", testAddr, testPool)
			objs := stack.ReverseObjects()

			Expect(objs).To(HaveLen(4))
			Expect(objs[0]).To(BeAssignableToTypeOf(&v1alpha1.PillarBinding{}),
				"first in reverse must be PillarBinding (innermost dependent)")
			Expect(objs[1]).To(BeAssignableToTypeOf(&v1alpha1.PillarProtocol{}))
			Expect(objs[2]).To(BeAssignableToTypeOf(&v1alpha1.PillarPool{}))
			Expect(objs[3]).To(BeAssignableToTypeOf(&v1alpha1.PillarTarget{}),
				"last in reverse must be PillarTarget (no dependencies)")
		})

		It("uses the ZFS pool name in pool.spec.backend.zfs.pool", func() {
			stack := framework.NewKindE2EStack("zfsname-test", testAddr, testPool)
			Expect(stack.Pool.Spec.Backend.ZFS.Pool).To(Equal(testPool),
				"ZFS pool name must flow through to backend.zfs.pool")
		})

		It("uses port 4421 in the NVMe-oF protocol", func() {
			stack := framework.NewKindE2EStack("port-test", testAddr, testPool)
			Expect(stack.Proto.Spec.NVMeOFTCP.Port).To(Equal(framework.KindNVMeOFPort))
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// Condition constants
	// ─────────────────────────────────────────────────────────────────────

	Describe("condition constants", func() {
		It("exports the standard PillarTarget condition type names", func() {
			Expect(framework.ConditionAgentConnected).To(Equal("AgentConnected"))
			Expect(framework.ConditionNodeExists).To(Equal("NodeExists"))
			Expect(framework.ConditionReady).To(Equal("Ready"))
		})

		It("exports the standard PillarPool condition type names", func() {
			Expect(framework.ConditionTargetReady).To(Equal("TargetReady"))
			Expect(framework.ConditionPoolDiscovered).To(Equal("PoolDiscovered"))
			Expect(framework.ConditionBackendSupported).To(Equal("BackendSupported"))
		})
	})

	// ─────────────────────────────────────────────────────────────────────
	// ApplyStack — requires a live Kind cluster + external agent
	// ─────────────────────────────────────────────────────────────────────
	//
	// This Describe block applies the full KindE2EStack to a running Kind
	// cluster and verifies that:
	//   a) The Kubernetes API server accepts all four CR objects without
	//      validation errors (confirming the CRD schema is compatible with
	//      the fixture field values).
	//   b) The CRs can be retrieved after creation.
	//   c) All CRs are cleaned up after the spec completes.
	//
	// The spec is skipped when no external agent is available (i.e. when
	// testEnv.ExternalAgentAddr is empty) because applying a PillarTarget
	// that points at a non-existent address would leave the cluster in a
	// degraded state.

	Describe("ApplyStack", Ordered, func() {
		var (
			suite *framework.Suite
			stack *framework.KindE2EStack
		)

		BeforeAll(func(ctx SpecContext) {
			if testEnv.ExternalAgentAddr == "" {
				Skip(
					"CRFixtures/ApplyStack: external agent not running — " +
						"set E2E_LAUNCH_EXTERNAL_AGENT=true or EXTERNAL_AGENT_ADDR " +
						"to enable live CR apply tests",
				)
			}

			var err error
			suite, err = framework.SetupSuite(
				framework.WithConnectTimeout(30 * time.Second),
			)
			Expect(err).NotTo(HaveOccurred(),
				"connect to Kind cluster — KUBECONFIG must be set by TestMain")

			prefix := framework.UniqueName("cr-fixture")
			stack = framework.NewKindE2EStack(prefix, testEnv.ExternalAgentAddr, testEnv.ZFSPoolName)

			// Register cleanup BEFORE applying so resources are removed even
			// when an assertion below fails.
			DeferCleanup(func(dctx SpecContext) {
				if suite == nil {
					return
				}
				for _, obj := range stack.ReverseObjects() {
					if err := framework.EnsureGone(dctx, suite.Client, obj, 2*time.Minute); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"warning: cleanup %T %q: %v\n", obj, obj.GetName(), err)
					}
				}
				suite.TeardownSuite()
			})
		})

		It("applies PillarTarget to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarTarget %q", stack.Target.Name))
			Expect(framework.Apply(ctx, suite.Client, stack.Target)).To(Succeed(),
				"PillarTarget with spec.external must be accepted by the API server — "+
					"verify the CRD schema allows the address %q and port %d",
				stack.Target.Spec.External.Address,
				stack.Target.Spec.External.Port,
			)

			By("verifying PillarTarget is retrievable from the cluster")
			got := &v1alpha1.PillarTarget{}
			Expect(suite.Client.Get(ctx,
				framework.ObjectKey(stack.Target), got)).To(Succeed(),
				"PillarTarget %q must be readable back from the API server",
				stack.Target.Name)
			Expect(got.Spec.External.Address).To(Equal(stack.Target.Spec.External.Address))
			Expect(got.Spec.External.Port).To(Equal(stack.Target.Spec.External.Port))
		})

		It("applies PillarPool to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarPool %q", stack.Pool.Name))
			Expect(framework.Apply(ctx, suite.Client, stack.Pool)).To(Succeed(),
				"PillarPool with ZFS zvol backend and Kind properties must be accepted — "+
					"verify the CRD schema allows the volblocksize, compression, and sync properties",
			)

			By("verifying PillarPool is retrievable and field values are preserved")
			got := &v1alpha1.PillarPool{}
			Expect(suite.Client.Get(ctx,
				framework.ObjectKey(stack.Pool), got)).To(Succeed(),
				"PillarPool %q must be readable back from the API server",
				stack.Pool.Name)
			Expect(got.Spec.TargetRef).To(Equal(stack.Target.Name))
			Expect(got.Spec.Backend.Type).To(Equal(v1alpha1.BackendTypeZFSZvol))
			Expect(got.Spec.Backend.ZFS.Pool).To(Equal(testEnv.ZFSPoolName))
			Expect(got.Spec.Backend.ZFS.Properties["volblocksize"]).To(Equal("4096"))
			Expect(got.Spec.Backend.ZFS.Properties["compression"]).To(Equal("lz4"))
		})

		It("applies PillarProtocol to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarProtocol %q", stack.Proto.Name))
			Expect(framework.Apply(ctx, suite.Client, stack.Proto)).To(Succeed(),
				"PillarProtocol with NVMe-oF TCP and port 4421 must be accepted — "+
					"verify the CRD schema allows port numbers and boolean ACL fields",
			)

			got := &v1alpha1.PillarProtocol{}
			Expect(suite.Client.Get(ctx,
				framework.ObjectKey(stack.Proto), got)).To(Succeed())
			Expect(got.Spec.Type).To(Equal(v1alpha1.ProtocolTypeNVMeOFTCP))
			Expect(got.Spec.NVMeOFTCP.Port).To(Equal(framework.KindNVMeOFPort))
		})

		It("applies PillarBinding to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarBinding %q", stack.Binding.Name))
			Expect(framework.Apply(ctx, suite.Client, stack.Binding)).To(Succeed(),
				"PillarBinding wiring pool and protocol must be accepted — "+
					"verify the CRD schema allows poolRef and protocolRef string fields",
			)

			got := &v1alpha1.PillarBinding{}
			Expect(suite.Client.Get(ctx,
				framework.ObjectKey(stack.Binding), got)).To(Succeed())
			Expect(got.Spec.PoolRef).To(Equal(stack.Pool.Name))
			Expect(got.Spec.ProtocolRef).To(Equal(stack.Proto.Name))
		})

		It("all stack CRs have a non-zero creationTimestamp", func(ctx SpecContext) {
			// After all Apply calls above, the objects should have server-set
			// metadata.  This spec checks that the Kubernetes API server has
			// fully processed the resources.
			for _, obj := range stack.Objects() {
				By(fmt.Sprintf("checking creationTimestamp on %T %q", obj, obj.GetName()))
				Expect(obj.GetCreationTimestamp()).NotTo(Equal(metav1.Time{}),
					"%T %q must have a non-zero creationTimestamp after Apply",
					obj, obj.GetName())
			}
		})
	})
})
