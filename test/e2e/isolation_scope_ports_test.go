package e2e

// isolation_scope_ports_test.go verifies the typed port reservation methods
// added to TestCaseScope: ReserveISCSITargetPort, ReserveCSIGRPCPort, and
// ReserveAgentGRPCPort.
//
// These tests cover the acceptance criterion that concurrent tests never
// receive conflicting ports for named service types (iSCSI target, CSI gRPC,
// agent gRPC).

import (
	"fmt"
	"net"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("TC isolation scope — typed port allocation", Label("ac:4b", "framework"), func() {
	newScope := func(tcID string) *TestCaseScope {
		GinkgoHelper()
		scope, err := NewTestCaseScope(tcID)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(scope.Close()).To(Succeed())
		})
		return scope
	}

	// ─── ReserveISCSITargetPort ───────────────────────────────────────────────

	It("AC4b.1 iSCSI target port is unique per scope and in valid range", func() {
		left := newScope("E35.1")
		right := newScope("E35.2")

		leftLease, err := left.ReserveISCSITargetPort("zfs-pool")
		Expect(err).NotTo(HaveOccurred())
		rightLease, err := right.ReserveISCSITargetPort("zfs-pool")
		Expect(err).NotTo(HaveOccurred())

		Expect(leftLease.Port).NotTo(Equal(0))
		Expect(rightLease.Port).NotTo(Equal(0))
		Expect(leftLease.Port).NotTo(Equal(rightLease.Port),
			"concurrent iSCSI target ports must be distinct")

		Expect(leftLease.Host).To(Equal("127.0.0.1"))
		Expect(leftLease.Port).To(BeNumerically(">=", 1))
		Expect(leftLease.Port).To(BeNumerically("<=", 65535))
	})

	It("AC4b.2 iSCSI target port is probe-and-release: rebindable immediately", func() {
		scope := newScope("E35.rebind")
		lease, err := scope.ReserveISCSITargetPort("target")
		Expect(err).NotTo(HaveOccurred())

		// Because the host listener is closed immediately, a new listener
		// on the same address must succeed (simulating a container binding).
		ln, bindErr := net.Listen("tcp", lease.Addr)
		Expect(bindErr).NotTo(HaveOccurred(),
			"expected iSCSI target port %s to be rebindable after probe-and-release", lease.Addr)
		Expect(ln.Close()).To(Succeed())
	})

	It("AC4b.3 same iSCSI label within one scope returns the same port", func() {
		scope := newScope("E35.idempotent")
		first, err := scope.ReserveISCSITargetPort("pool")
		Expect(err).NotTo(HaveOccurred())
		second, err := scope.ReserveISCSITargetPort("pool")
		Expect(err).NotTo(HaveOccurred())

		Expect(first.Port).To(Equal(second.Port), "same label should return same lease")
	})

	It("AC4b.4 RecreateISCSITargetPort returns a fresh port on each call", func() {
		scope := newScope("E35.recreate")
		first, err := scope.ReserveISCSITargetPort("pool")
		Expect(err).NotTo(HaveOccurred())

		second, err := scope.RecreateISCSITargetPort("pool")
		Expect(err).NotTo(HaveOccurred())

		Expect(second.Port).NotTo(Equal(first.Port),
			"RecreateISCSITargetPort should allocate a new port")
	})

	// ─── ReserveCSIGRPCPort ───────────────────────────────────────────────────

	It("AC4b.5 CSI gRPC port is host-bound and connectable", func() {
		scope := newScope("E10.csi-grpc")
		lease, err := scope.ReserveCSIGRPCPort("driver")
		Expect(err).NotTo(HaveOccurred())

		// The underlying listener must be accepting connections.
		conn, dialErr := net.Dial("tcp", lease.Addr)
		Expect(dialErr).NotTo(HaveOccurred(),
			"CSI gRPC port should be connectable while scope is open")
		Expect(conn.Close()).To(Succeed())
	})

	It("AC4b.6 CSI gRPC ports are unique across concurrent scopes", func() {
		left := newScope("E10.csi.left")
		right := newScope("E10.csi.right")

		leftLease, err := left.ReserveCSIGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())
		rightLease, err := right.ReserveCSIGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())

		Expect(leftLease.Port).NotTo(Equal(rightLease.Port))
	})

	// ─── ReserveAgentGRPCPort ─────────────────────────────────────────────────

	It("AC4b.7 agent gRPC port is host-bound and connectable", func() {
		scope := newScope("E9.agent-grpc")
		lease, err := scope.ReserveAgentGRPCPort("primary")
		Expect(err).NotTo(HaveOccurred())

		conn, dialErr := net.Dial("tcp", lease.Addr)
		Expect(dialErr).NotTo(HaveOccurred(),
			"agent gRPC port should be connectable while scope is open")
		Expect(conn.Close()).To(Succeed())
	})

	It("AC4b.8 agent gRPC ports are unique across concurrent scopes", func() {
		left := newScope("E9.agent.left")
		right := newScope("E9.agent.right")

		leftLease, err := left.ReserveAgentGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())
		rightLease, err := right.ReserveAgentGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())

		Expect(leftLease.Port).NotTo(Equal(rightLease.Port))
	})

	// ─── Cross-service uniqueness ─────────────────────────────────────────────

	It("AC4b.9 iSCSI and CSI gRPC ports are distinct even with the same label", func() {
		scope := newScope("E34.cross")
		iscsiLease, err := scope.ReserveISCSITargetPort("primary")
		Expect(err).NotTo(HaveOccurred())
		csiLease, err := scope.ReserveCSIGRPCPort("primary")
		Expect(err).NotTo(HaveOccurred())

		Expect(iscsiLease.Port).NotTo(Equal(csiLease.Port),
			"iSCSI and CSI gRPC must never share a port even with the same label")
	})

	It("AC4b.10 all service port types are distinct across same scope", func() {
		scope := newScope("E34.all-types")
		iscsiLease, err := scope.ReserveISCSITargetPort("svc")
		Expect(err).NotTo(HaveOccurred())
		csiLease, err := scope.ReserveCSIGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())
		agentLease, err := scope.ReserveAgentGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())

		ports := map[int]string{
			iscsiLease.Port: "iscsi",
			csiLease.Port:   "csi-grpc",
			agentLease.Port: "agent-grpc",
		}
		Expect(ports).To(HaveLen(3),
			"all three service port types must have distinct ports")
	})

	// ─── Concurrent parallelism ───────────────────────────────────────────────

	It("AC4b.11 40 concurrent iSCSI target port allocations are all unique", func() {
		const count = 40

		type portResult struct {
			port int
			err  error
		}
		results := make(chan portResult, count)

		var wg sync.WaitGroup
		for i := range count {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				scope, err := NewTestCaseScope(fmt.Sprintf("E35.concurrent.%d", i))
				if err != nil {
					results <- portResult{err: err}
					return
				}
				defer scope.Close() //nolint:errcheck
				lease, err := scope.ReserveISCSITargetPort("target")
				if err != nil {
					results <- portResult{err: err}
					return
				}
				results <- portResult{port: lease.Port}
			}(i)
		}
		wg.Wait()
		close(results)

		seen := make(map[int]int)
		for r := range results {
			Expect(r.err).NotTo(HaveOccurred(), "concurrent iSCSI allocation must not error")
			seen[r.port]++
		}
		for port, count := range seen {
			Expect(count).To(Equal(1),
				"port %d was allocated %d times; must be unique", port, count)
		}
	})

	// ─── Cleanup ─────────────────────────────────────────────────────────────

	It("AC4b.12 typed port allocations are released on scope.Close", func() {
		scope, err := NewTestCaseScope("E35.cleanup")
		Expect(err).NotTo(HaveOccurred())

		iscsiLease, err := scope.ReserveISCSITargetPort("t1")
		Expect(err).NotTo(HaveOccurred())
		csiLease, err := scope.ReserveCSIGRPCPort("t2")
		Expect(err).NotTo(HaveOccurred())

		iscsiAddr := iscsiLease.Addr
		csiAddr := csiLease.Addr

		Expect(scope.Close()).To(Succeed())

		// iSCSI port (probe-and-release) should already have been free;
		// verify it is still bindable after scope close.
		ln1, err := net.Listen("tcp", iscsiAddr)
		Expect(err).NotTo(HaveOccurred(), "iSCSI port should be bindable after scope.Close")
		Expect(ln1.Close()).To(Succeed())

		// CSI gRPC port (host-bound) should be released by scope.Close.
		ln2, err := net.Listen("tcp", csiAddr)
		Expect(err).NotTo(HaveOccurred(), "CSI gRPC port should be freed by scope.Close")
		Expect(ln2.Close()).To(Succeed())
	})
})

// ─── ReserveISCSIPortRange ───────────────────────────────────────────────────

var _ = Describe("TC isolation scope — iSCSI port range allocator", Label("ac:5b", "framework"), func() {
	newScope := func(tcID string) *TestCaseScope {
		GinkgoHelper()
		scope, err := NewTestCaseScope(tcID)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(scope.Close()).To(Succeed())
		})
		return scope
	}

	// ─── Basic allocation ─────────────────────────────────────────────────────

	It("AC5b.1 range has non-zero base port and valid count", func() {
		scope := newScope("E36.basic")
		r, err := scope.ReserveISCSIPortRange("targets")
		Expect(err).NotTo(HaveOccurred())
		Expect(r).NotTo(BeNil())
		Expect(r.Base).To(BeNumerically(">", 0))
		Expect(r.Count).To(BeNumerically(">", 0))
		Expect(r.End).To(Equal(r.Base + r.Count))
	})

	It("AC5b.2 all ports in the range are within 1–65535", func() {
		scope := newScope("E36.port-range")
		r, err := scope.ReserveISCSIPortRange("range-check")
		Expect(err).NotTo(HaveOccurred())

		Expect(r.Base).To(BeNumerically(">=", 1024), "base port must be unprivileged")
		Expect(r.End-1).To(BeNumerically("<=", 65535), "last port must be ≤ 65535")
	})

	It("AC5b.3 Port(0) returns the base port", func() {
		scope := newScope("E36.port0")
		r, err := scope.ReserveISCSIPortRange("p0-check")
		Expect(err).NotTo(HaveOccurred())

		Expect(r.Port(0)).To(Equal(r.Base))
	})

	It("AC5b.4 Port(Count-1) returns the last port in the range", func() {
		scope := newScope("E36.last-port")
		r, err := scope.ReserveISCSIPortRange("last-check")
		Expect(err).NotTo(HaveOccurred())

		Expect(r.Port(r.Count - 1)).To(Equal(r.End - 1))
	})

	// ─── Idempotency ─────────────────────────────────────────────────────────

	It("AC5b.5 same label within one scope returns the same range", func() {
		scope := newScope("E36.idempotent")
		first, err := scope.ReserveISCSIPortRange("pool")
		Expect(err).NotTo(HaveOccurred())
		second, err := scope.ReserveISCSIPortRange("pool")
		Expect(err).NotTo(HaveOccurred())

		Expect(first.Base).To(Equal(second.Base), "same label must return the same range")
		Expect(first.CaseIndex).To(Equal(second.CaseIndex))
	})

	It("AC5b.6 different labels within one scope return different ranges", func() {
		scope := newScope("E36.multi-label")
		r1, err := scope.ReserveISCSIPortRange("pool-a")
		Expect(err).NotTo(HaveOccurred())
		r2, err := scope.ReserveISCSIPortRange("pool-b")
		Expect(err).NotTo(HaveOccurred())

		Expect(r1.Base).NotTo(Equal(r2.Base), "different labels must produce non-overlapping ranges")
		Expect(r1.Overlaps(r2)).To(BeFalse(), "ranges for different labels within the same scope must not overlap")
	})

	// ─── Non-overlapping across scopes ───────────────────────────────────────

	It("AC5b.7 ranges across two concurrent scopes do not overlap", func() {
		left := newScope("E36.overlap.left")
		right := newScope("E36.overlap.right")

		leftRange, err := left.ReserveISCSIPortRange("target")
		Expect(err).NotTo(HaveOccurred())
		rightRange, err := right.ReserveISCSIPortRange("target")
		Expect(err).NotTo(HaveOccurred())

		Expect(leftRange.Overlaps(rightRange)).To(BeFalse(),
			"iSCSI port ranges from concurrent test cases must not overlap")
		Expect(leftRange.CaseIndex).NotTo(Equal(rightRange.CaseIndex),
			"concurrent scopes must receive different CaseIndex values")
	})

	// ─── RecreateISCSIPortRange ───────────────────────────────────────────────

	It("AC5b.8 RecreateISCSIPortRange returns a fresh range", func() {
		scope := newScope("E36.recreate")
		first, err := scope.ReserveISCSIPortRange("pool")
		Expect(err).NotTo(HaveOccurred())

		second, err := scope.RecreateISCSIPortRange("pool")
		Expect(err).NotTo(HaveOccurred())

		Expect(second.CaseIndex).NotTo(Equal(first.CaseIndex),
			"RecreateISCSIPortRange must allocate a new case index")
		Expect(second.Base).NotTo(Equal(first.Base),
			"RecreateISCSIPortRange must allocate a new base port")
		Expect(first.Overlaps(second)).To(BeFalse(),
			"old and new ranges must not overlap after recreate")
	})

	It("AC5b.9 RecreateISCSIPortRange on a fresh label returns valid range", func() {
		scope := newScope("E36.recreate-fresh")
		r, err := scope.RecreateISCSIPortRange("new-label")
		Expect(err).NotTo(HaveOccurred())
		Expect(r).NotTo(BeNil())
		Expect(r.Count).To(BeNumerically(">", 0))
	})

	// ─── Closed scope ─────────────────────────────────────────────────────────

	It("AC5b.10 ReserveISCSIPortRange errors after scope is closed", func() {
		scope, err := NewTestCaseScope("E36.closed-reserve")
		Expect(err).NotTo(HaveOccurred())
		Expect(scope.Close()).To(Succeed())

		_, err = scope.ReserveISCSIPortRange("late")
		Expect(err).To(HaveOccurred(), "ReserveISCSIPortRange on closed scope must return an error")
	})

	It("AC5b.11 RecreateISCSIPortRange errors after scope is closed", func() {
		scope, err := NewTestCaseScope("E36.closed-recreate")
		Expect(err).NotTo(HaveOccurred())
		Expect(scope.Close()).To(Succeed())

		_, err = scope.RecreateISCSIPortRange("late")
		Expect(err).To(HaveOccurred(), "RecreateISCSIPortRange on closed scope must return an error")
	})

	// ─── Concurrent parallelism ───────────────────────────────────────────────

	It("AC5b.12 40 concurrent ReserveISCSIPortRange calls produce non-overlapping ranges", func() {
		const count = 40
		type rangeResult struct {
			r   *ISCSIPortRangeResult
			err error
		}
		results := make(chan rangeResult, count)

		var wg sync.WaitGroup
		for i := range count {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				scope, err := NewTestCaseScope(fmt.Sprintf("E36.concurrent.%d", i))
				if err != nil {
					results <- rangeResult{err: err}
					return
				}
				defer scope.Close() //nolint:errcheck
				r, err := scope.ReserveISCSIPortRange("target")
				if err != nil {
					results <- rangeResult{err: err}
					return
				}
				results <- rangeResult{r: &ISCSIPortRangeResult{Base: r.Base, End: r.End, CaseIndex: r.CaseIndex}}
			}(i)
		}
		wg.Wait()
		close(results)

		var collected []ISCSIPortRangeResult
		for res := range results {
			Expect(res.err).NotTo(HaveOccurred(), "concurrent iSCSI range allocation must not error")
			if res.r != nil {
				collected = append(collected, *res.r)
			}
		}

		// All CaseIndex values must be distinct.
		seenIdx := make(map[int]int)
		for _, r := range collected {
			seenIdx[r.CaseIndex]++
		}
		for idx, cnt := range seenIdx {
			Expect(cnt).To(Equal(1),
				"CaseIndex %d assigned to %d test cases; must be unique", idx, cnt)
		}

		// No two ranges may share a port.
		for i := range len(collected) {
			for j := i + 1; j < len(collected); j++ {
				a, b := collected[i], collected[j]
				// Overlap: a.Base < b.End && b.Base < a.End
				if a.Base < b.End && b.Base < a.End {
					Fail(fmt.Sprintf("range[%d] [%d,%d) overlaps range[%d] [%d,%d)",
						i, a.Base, a.End, j, b.Base, b.End))
				}
			}
		}
	})

	// ─── Contains integration ────────────────────────────────────────────────

	It("AC5b.13 Port(0) is contained in the range and Port(-1) equivalent is not", func() {
		scope := newScope("E36.contains")
		r, err := scope.ReserveISCSIPortRange("contains-check")
		Expect(err).NotTo(HaveOccurred())

		Expect(r.Contains(r.Port(0))).To(BeTrue())
		Expect(r.Contains(r.Base)).To(BeTrue())
		Expect(r.Contains(r.End)).To(BeFalse(), "End is exclusive, must not be contained")
		Expect(r.Contains(r.Base-1)).To(BeFalse(), "port before Base must not be contained")
	})
})

// ISCSIPortRangeResult is a plain-data copy of ISCSIPortRange fields used in
// concurrent tests to avoid retaining pointers to the allocator's output after
// the allocating goroutine has returned.
type ISCSIPortRangeResult struct {
	Base      int
	End       int
	CaseIndex int
}
