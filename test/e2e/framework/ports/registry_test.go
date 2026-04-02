package ports_test

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/ports"
)

// ─── Allocate (host-bound) ───────────────────────────────────────────────────

func TestAllocate_ReturnsUniquePortsPerCall(t *testing.T) {
	reg := ports.NewRegistry()

	a1, err := reg.Allocate(ports.KindAgentGRPC, "srv1")
	if err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	defer a1.Release()

	a2, err := reg.Allocate(ports.KindAgentGRPC, "srv2")
	if err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	defer a2.Release()

	if a1.Port == a2.Port {
		t.Errorf("expected distinct ports, both got %d", a1.Port)
	}
}

func TestAllocate_PortIsInValidRange(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.Allocate(ports.KindGeneric, "range-check")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	defer a.Release()

	if a.Port < 1 || a.Port > 65535 {
		t.Errorf("port %d out of 1-65535 range", a.Port)
	}
}

func TestAllocate_AddrIsLocalhostPort(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.Allocate(ports.KindCSIGRPC, "grpc")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	defer a.Release()

	if a.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", a.Host)
	}
	expected := fmt.Sprintf("127.0.0.1:%d", a.Port)
	if a.Addr != expected {
		t.Errorf("Addr = %q, want %q", a.Addr, expected)
	}
}

func TestAllocate_ListenerIsConnectable(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.Allocate(ports.KindGeneric, "connectable")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	defer a.Release()

	// The listener must accept connections on the allocated address.
	ln := a.Listener()
	if ln == nil {
		t.Fatal("Listener() returned nil for host-bound allocation")
	}

	conn, err := net.Dial("tcp", a.Addr)
	if err != nil {
		t.Fatalf("Dial %s: %v", a.Addr, err)
	}
	conn.Close()
}

func TestAllocate_TrackedInRegistry(t *testing.T) {
	reg := ports.NewRegistry()
	before := reg.ActiveCount()

	a, err := reg.Allocate(ports.KindGeneric, "tracked")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	if got := reg.ActiveCount(); got != before+1 {
		t.Errorf("ActiveCount after Allocate = %d, want %d", got, before+1)
	}

	_ = a.Release()

	if got := reg.ActiveCount(); got != before {
		t.Errorf("ActiveCount after Release = %d, want %d (before)", got, before)
	}
}

// ─── Release ─────────────────────────────────────────────────────────────────

func TestRelease_PortBecomesFreeAfterRelease(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.Allocate(ports.KindGeneric, "to-release")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	addr := a.Addr

	if err := a.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// After release the OS should allow a new listener on the same address.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Errorf("Listen on released port %s: %v", addr, err)
	} else {
		ln.Close()
	}
}

func TestRelease_IdempotentMultipleCalls(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.Allocate(ports.KindGeneric, "idempotent")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	for i := range 5 {
		if err := a.Release(); err != nil {
			t.Errorf("Release call %d: unexpected error: %v", i, err)
		}
	}
}

func TestRelease_NilAllocationIsNoop(t *testing.T) {
	var a *ports.Allocation
	if err := a.Release(); err != nil {
		t.Errorf("Release on nil Allocation: %v", err)
	}
}

// ─── AllocateForContainer ────────────────────────────────────────────────────

func TestAllocateForContainer_ReturnsUniquePort(t *testing.T) {
	reg := ports.NewRegistry()

	a1, err := reg.AllocateForContainer(ports.KindISCSITarget, "iscsi1")
	if err != nil {
		t.Fatalf("first AllocateForContainer: %v", err)
	}
	defer a1.Release()

	a2, err := reg.AllocateForContainer(ports.KindISCSITarget, "iscsi2")
	if err != nil {
		t.Fatalf("second AllocateForContainer: %v", err)
	}
	defer a2.Release()

	if a1.Port == a2.Port {
		t.Errorf("expected distinct ports, both got %d", a1.Port)
	}
}

func TestAllocateForContainer_ListenerIsNil(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.AllocateForContainer(ports.KindISCSITarget, "no-listener")
	if err != nil {
		t.Fatalf("AllocateForContainer: %v", err)
	}
	defer a.Release()

	if a.Listener() != nil {
		t.Error("expected nil Listener() for probe-and-release allocation")
	}
}

func TestAllocateForContainer_PortIsRebindable(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.AllocateForContainer(ports.KindISCSITarget, "rebind")
	if err != nil {
		t.Fatalf("AllocateForContainer: %v", err)
	}
	defer a.Release()

	// Because the host listener was closed, a container (or test code) should
	// be able to bind to the same port immediately.
	ln, err := net.Listen("tcp", a.Addr)
	if err != nil {
		t.Errorf("Listen on container-allocated port %s: %v", a.Addr, err)
	} else {
		ln.Close()
	}
}

func TestAllocateForContainer_TrackedUntilRelease(t *testing.T) {
	reg := ports.NewRegistry()
	before := reg.ActiveCount()

	a, err := reg.AllocateForContainer(ports.KindGeneric, "tracked-container")
	if err != nil {
		t.Fatalf("AllocateForContainer: %v", err)
	}

	if got := reg.ActiveCount(); got != before+1 {
		t.Errorf("ActiveCount after AllocateForContainer = %d, want %d", got, before+1)
	}
	_ = a.Release()
	if got := reg.ActiveCount(); got != before {
		t.Errorf("ActiveCount after Release = %d, want %d", got, before)
	}
}

// ─── Convenience helpers ─────────────────────────────────────────────────────

func TestAllocateISCSITarget_WrapsAllocateForContainer(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.AllocateISCSITarget("my-target")
	if err != nil {
		t.Fatalf("AllocateISCSITarget: %v", err)
	}
	defer a.Release()

	if a.Service != ports.KindISCSITarget {
		t.Errorf("Service = %q, want %q", a.Service, ports.KindISCSITarget)
	}
	if a.Label != "my-target" {
		t.Errorf("Label = %q, want %q", a.Label, "my-target")
	}
	if a.Listener() != nil {
		t.Error("AllocateISCSITarget should use probe-and-release (nil Listener)")
	}
}

func TestAllocateCSIGRPC_WrapsAllocate(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.AllocateCSIGRPC("driver")
	if err != nil {
		t.Fatalf("AllocateCSIGRPC: %v", err)
	}
	defer a.Release()

	if a.Service != ports.KindCSIGRPC {
		t.Errorf("Service = %q, want %q", a.Service, ports.KindCSIGRPC)
	}
	if a.Listener() == nil {
		t.Error("AllocateCSIGRPC should be host-bound (non-nil Listener)")
	}
}

func TestAllocateAgentGRPC_WrapsAllocate(t *testing.T) {
	reg := ports.NewRegistry()
	a, err := reg.AllocateAgentGRPC("primary")
	if err != nil {
		t.Fatalf("AllocateAgentGRPC: %v", err)
	}
	defer a.Release()

	if a.Service != ports.KindAgentGRPC {
		t.Errorf("Service = %q, want %q", a.Service, ports.KindAgentGRPC)
	}
	if a.Listener() == nil {
		t.Error("AllocateAgentGRPC should be host-bound (non-nil Listener)")
	}
}

// ─── Concurrency ─────────────────────────────────────────────────────────────

func TestAllocate_ConcurrentAllocationNeverCollides(t *testing.T) {
	const goroutines = 20
	reg := ports.NewRegistry()

	type result struct {
		alloc *ports.Allocation
		err   error
	}

	// Use a buffered channel large enough to hold all results without blocking.
	results := make(chan result, goroutines*2)
	var wg sync.WaitGroup

	// Host-bound allocations — keep listener open until we release below.
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := reg.Allocate(ports.KindGeneric, fmt.Sprintf("concurrent-%d", i))
			results <- result{alloc: a, err: err}
		}(i)
	}

	// Container allocations — port is released from the host perspective but
	// remains registered in the registry until a.Release() is called.
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := reg.AllocateForContainer(ports.KindISCSITarget, fmt.Sprintf("container-%d", i))
			results <- result{alloc: a, err: err}
		}(i)
	}

	wg.Wait()
	close(results)

	// Collect all allocations before checking for duplicates.
	// Releasing allocations eagerly (e.g. via defer inside goroutines) would
	// allow ports to be reused by still-running goroutines, causing false
	// duplicate reports.  We hold all allocations here and release after the
	// uniqueness check.
	var allocs []*ports.Allocation
	seen := make(map[int]int) // port → count
	for r := range results {
		if r.err != nil {
			t.Errorf("concurrent allocation error: %v", r.err)
			continue
		}
		seen[r.alloc.Port]++
		allocs = append(allocs, r.alloc)
	}

	// Release all allocations now that the uniqueness check has captured the
	// port numbers.
	for _, a := range allocs {
		_ = a.Release()
	}

	for port, count := range seen {
		if count > 1 {
			t.Errorf("port %d allocated %d times; expected at most 1", port, count)
		}
	}
}

// ─── TotalIssued / Snapshot ───────────────────────────────────────────────────

func TestTotalIssued_MonotonicallyIncreases(t *testing.T) {
	reg := ports.NewRegistry()

	for i := range 5 {
		a, err := reg.Allocate(ports.KindGeneric, fmt.Sprintf("cnt-%d", i))
		if err != nil {
			t.Fatalf("Allocate #%d: %v", i, err)
		}
		if err := a.Release(); err != nil {
			t.Fatalf("Release #%d: %v", i, err)
		}
		if got := reg.TotalIssued(); got != int64(i+1) {
			t.Errorf("TotalIssued after %d allocs = %d, want %d", i+1, got, i+1)
		}
	}
}

func TestSnapshot_ReturnsActiveAllocations(t *testing.T) {
	reg := ports.NewRegistry()
	a1, _ := reg.Allocate(ports.KindGeneric, "snap1")
	a2, _ := reg.Allocate(ports.KindGeneric, "snap2")
	defer a1.Release()
	defer a2.Release()

	snap := reg.Snapshot()
	if len(snap) < 2 {
		t.Errorf("Snapshot len = %d, want >= 2", len(snap))
	}
}

// ─── Global registry ─────────────────────────────────────────────────────────

func TestGlobal_IsNonNil(t *testing.T) {
	if ports.Global == nil {
		t.Fatal("ports.Global is nil")
	}
}

func TestGlobal_AllocatesDistinctPorts(t *testing.T) {
	a1, err := ports.Global.Allocate(ports.KindGeneric, "g1")
	if err != nil {
		t.Fatalf("Global.Allocate: %v", err)
	}
	defer a1.Release()

	a2, err := ports.Global.Allocate(ports.KindGeneric, "g2")
	if err != nil {
		t.Fatalf("Global.Allocate: %v", err)
	}
	defer a2.Release()

	if a1.Port == a2.Port {
		t.Errorf("Global allocated the same port %d twice", a1.Port)
	}
}
