package e2e

// isolation_scope_ports_test.go verifies the typed port reservation methods
// added to TestCaseScope: ReserveCSIGRPCPort and ReserveAgentGRPCPort.
//
// These tests cover the acceptance criterion that concurrent tests never
// receive conflicting ports for named service types (CSI gRPC, agent gRPC).

import (
	"fmt"
	"net"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("TC isolation scope — typed port allocation", Label("ac:4b", "framework", "default-profile"), func() {
	newScope := func(tcID string) *TestCaseScope {
		GinkgoHelper()
		scope, err := NewTestCaseScope(tcID)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(scope.Close()).To(Succeed())
		})
		return scope
	}

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

	It("AC4b.9 agent gRPC and CSI gRPC ports are distinct even with the same label", func() {
		scope := newScope("E10.cross")
		agentLease, err := scope.ReserveAgentGRPCPort("primary")
		Expect(err).NotTo(HaveOccurred())
		csiLease, err := scope.ReserveCSIGRPCPort("primary")
		Expect(err).NotTo(HaveOccurred())

		Expect(agentLease.Port).NotTo(Equal(csiLease.Port),
			"agent gRPC and CSI gRPC must never share a port even with the same label")
	})

	It("AC4b.10 recreating a typed port yields a fresh, distinct port", func() {
		scope := newScope("E10.recreate")
		first, err := scope.ReserveCSIGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())
		second, err := scope.RecreateCSIGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())
		agentLease, err := scope.ReserveAgentGRPCPort("svc")
		Expect(err).NotTo(HaveOccurred())

		Expect(second.Port).NotTo(Equal(first.Port),
			"RecreateCSIGRPCPort should allocate a new port")
		Expect(agentLease.Port).NotTo(Equal(second.Port),
			"typed ports of different service kinds must be distinct")
	})

	// ─── Concurrent parallelism ───────────────────────────────────────────────

	It("AC4b.11 40 concurrent CSI gRPC port allocations are all unique", func() {
		const count = 40

		type portResult struct {
			port  int
			err   error
			scope *TestCaseScope // kept alive until uniqueness check completes
		}
		results := make(chan portResult, count)

		var wg sync.WaitGroup
		for i := range count {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				scope, err := NewTestCaseScope(fmt.Sprintf("E10.concurrent.%d", i))
				if err != nil {
					results <- portResult{err: err}
					return
				}
				lease, err := scope.ReserveCSIGRPCPort("driver")
				if err != nil {
					_ = scope.Close()
					results <- portResult{err: err}
					return
				}
				// Keep the scope open until the uniqueness check completes so
				// the port stays registered in ports.Global.
				results <- portResult{port: lease.Port, scope: scope}
			}(i)
		}
		wg.Wait()
		close(results)

		seen := make(map[int]int)
		var openScopes []*TestCaseScope
		for r := range results {
			Expect(r.err).NotTo(HaveOccurred(), "concurrent CSI gRPC allocation must not error")
			seen[r.port]++
			if r.scope != nil {
				openScopes = append(openScopes, r.scope)
			}
		}

		for port, count := range seen {
			Expect(count).To(Equal(1),
				"port %d was allocated %d times; must be unique", port, count)
		}

		for _, s := range openScopes {
			Expect(s.Close()).To(Succeed())
		}
	})

	// ─── Cleanup ─────────────────────────────────────────────────────────────

	It("AC4b.12 typed port allocations are released on scope.Close", func() {
		scope := newScope("E10.cleanup")

		agentLease, err := scope.ReserveAgentGRPCPort("t1")
		Expect(err).NotTo(HaveOccurred())
		csiLease, err := scope.ReserveCSIGRPCPort("t2")
		Expect(err).NotTo(HaveOccurred())

		// Keep the owned listeners: another process may reuse their addresses
		// as soon as Close releases them.
		agentListener := agentLease.listener.(*net.TCPListener)
		csiListener := csiLease.listener.(*net.TCPListener)
		// A leaked listener must fail promptly rather than block in Accept.
		Expect(agentListener.SetDeadline(time.Now())).To(Succeed())
		Expect(csiListener.SetDeadline(time.Now())).To(Succeed())

		Expect(scope.Close()).To(Succeed())

		_, err = agentListener.Accept()
		Expect(err).To(MatchError(net.ErrClosed), "agent gRPC listener should be closed by scope.Close")
		_, err = csiListener.Accept()
		Expect(err).To(MatchError(net.ErrClosed), "CSI gRPC listener should be closed by scope.Close")
	})
})
