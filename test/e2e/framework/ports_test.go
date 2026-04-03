package framework_test

// ports_test.go validates the per-TC unique port allocation contract defined in
// ports.go.  Every test uses a private ports.Registry instance (via the
// framework package) so the global registry is not polluted.  Tests verify:
//
//   - uniqueness guarantees for all three allocation strategies
//   - host-bound listeners block re-use while open
//   - probe-and-release ports are immediately rebindable by a container
//   - iSCSI port ranges are non-overlapping across concurrent callers
//   - PortSet lifecycle (allocate → use → Close → release)
//   - concurrent safety: 50 goroutines allocate ports without collision

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/ports"
)

// ─── AllocateHostPort ────────────────────────────────────────────────────────

func TestAllocateHostPort_ReturnsValidHandle(t *testing.T) {
	h, release, err := framework.AllocateHostPort("TC-1", "grpc")
	if err != nil {
		t.Fatalf("AllocateHostPort: %v", err)
	}
	defer release()

	if h == nil {
		t.Fatal("AllocateHostPort returned nil handle")
	}
	if h.Port < 1 || h.Port > 65535 {
		t.Errorf("port %d out of valid range", h.Port)
	}
	if h.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", h.Host)
	}
	wantAddr := fmt.Sprintf("127.0.0.1:%d", h.Port)
	if h.Addr != wantAddr {
		t.Errorf("Addr = %q, want %q", h.Addr, wantAddr)
	}
}

func TestAllocateHostPort_ListenerIsOpen(t *testing.T) {
	h, release, err := framework.AllocateHostPort("TC-2", "listen")
	if err != nil {
		t.Fatalf("AllocateHostPort: %v", err)
	}
	defer release()

	ln := h.Listener()
	if ln == nil {
		t.Fatal("Listener() returned nil for host-bound allocation")
	}

	// The listener must accept connections.
	conn, dialErr := net.Dial("tcp", h.Addr)
	if dialErr != nil {
		t.Fatalf("Dial %s: %v", h.Addr, dialErr)
	}
	conn.Close()
}

func TestAllocateHostPort_PortBlockedUntilRelease(t *testing.T) {
	h, release, err := framework.AllocateHostPort("TC-3", "blocked")
	if err != nil {
		t.Fatalf("AllocateHostPort: %v", err)
	}

	addr := h.Addr

	// While the listener is open, binding the same address must fail.
	ln, bindErr := net.Listen("tcp", addr)
	if bindErr == nil {
		ln.Close()
		release()
		t.Fatal("expected bind to fail while host-bound listener is open")
	}

	// After release, the address must be available again.
	release()

	ln2, bindErr2 := net.Listen("tcp", addr)
	if bindErr2 != nil {
		t.Errorf("bind on released port %s: %v", addr, bindErr2)
	} else {
		ln2.Close()
	}
}

func TestAllocateHostPort_TwoConcurrentScopes_UniquePortsGuaranteed(t *testing.T) {
	h1, r1, err := framework.AllocateHostPort("TC-4a", "svc")
	if err != nil {
		t.Fatalf("AllocateHostPort #1: %v", err)
	}
	defer r1()

	h2, r2, err := framework.AllocateHostPort("TC-4b", "svc")
	if err != nil {
		t.Fatalf("AllocateHostPort #2: %v", err)
	}
	defer r2()

	if h1.Port == h2.Port {
		t.Errorf("concurrent host-bound allocations returned the same port %d", h1.Port)
	}
}

func TestAllocateCSIGRPCPort_ReturnsNonNilListener(t *testing.T) {
	h, release, err := framework.AllocateCSIGRPCPort("TC-5", "csi")
	if err != nil {
		t.Fatalf("AllocateCSIGRPCPort: %v", err)
	}
	defer release()

	if h.Listener() == nil {
		t.Error("CSI gRPC port should hold a listener open")
	}
}

func TestAllocateAgentGRPCPort_ReturnsNonNilListener(t *testing.T) {
	h, release, err := framework.AllocateAgentGRPCPort("TC-6", "agent")
	if err != nil {
		t.Fatalf("AllocateAgentGRPCPort: %v", err)
	}
	defer release()

	if h.Listener() == nil {
		t.Error("agent gRPC port should hold a listener open")
	}
}

// ─── AllocateContainerPort ───────────────────────────────────────────────────

func TestAllocateContainerPort_ReturnsValidHandle(t *testing.T) {
	h, release, err := framework.AllocateContainerPort("TC-10", "iscsi")
	if err != nil {
		t.Fatalf("AllocateContainerPort: %v", err)
	}
	defer release()

	if h.Port < 1 || h.Port > 65535 {
		t.Errorf("port %d out of valid range", h.Port)
	}
}

func TestAllocateContainerPort_ListenerIsNil(t *testing.T) {
	h, release, err := framework.AllocateContainerPort("TC-11", "iscsi-nil-listener")
	if err != nil {
		t.Fatalf("AllocateContainerPort: %v", err)
	}
	defer release()

	if ln := h.Listener(); ln != nil {
		ln.Close()
		t.Error("probe-and-release port should have nil Listener()")
	}
}

func TestAllocateContainerPort_PortRebindable(t *testing.T) {
	h, release, err := framework.AllocateContainerPort("TC-12", "rebind")
	if err != nil {
		t.Fatalf("AllocateContainerPort: %v", err)
	}
	defer release()

	// Because the host listener was released immediately, a new bind must succeed
	// (simulating a container process binding the port).
	ln, bindErr := net.Listen("tcp", h.Addr)
	if bindErr != nil {
		t.Errorf("Listen on container-allocated port %s: %v", h.Addr, bindErr)
	} else {
		ln.Close()
	}
}

func TestAllocateContainerPort_UniqueAcrossConcurrentCalls(t *testing.T) {
	h1, r1, err := framework.AllocateContainerPort("TC-13a", "iscsi")
	if err != nil {
		t.Fatalf("first AllocateContainerPort: %v", err)
	}
	defer r1()

	h2, r2, err := framework.AllocateContainerPort("TC-13b", "iscsi")
	if err != nil {
		t.Fatalf("second AllocateContainerPort: %v", err)
	}
	defer r2()

	if h1.Port == h2.Port {
		t.Errorf("concurrent container port allocations returned the same port %d", h1.Port)
	}
}

// ─── AllocateISCSIPortRange ──────────────────────────────────────────────────

func TestAllocateISCSIPortRange_ReturnsNonNil(t *testing.T) {
	r, err := framework.AllocateISCSIPortRange("TC-20", "targets")
	if err != nil {
		t.Fatalf("AllocateISCSIPortRange: %v", err)
	}
	if r == nil {
		t.Fatal("AllocateISCSIPortRange returned nil")
	}
}

func TestAllocateISCSIPortRange_ValidPortRange(t *testing.T) {
	r, err := framework.AllocateISCSIPortRange("TC-21", "validate")
	if err != nil {
		t.Fatalf("AllocateISCSIPortRange: %v", err)
	}

	if r.Base < 1024 {
		t.Errorf("Base %d is below 1024 (privileged port)", r.Base)
	}
	if r.End > 65535 {
		t.Errorf("End %d exceeds 65535", r.End)
	}
	if r.Count <= 0 {
		t.Errorf("Count %d must be positive", r.Count)
	}
	if r.End != r.Base+r.Count {
		t.Errorf("End %d != Base %d + Count %d", r.End, r.Base, r.Count)
	}
}

func TestAllocateISCSIPortRange_NonOverlappingAcrossCallersForTCLevel(t *testing.T) {
	// Collect several ranges and verify no two share a port.
	const n = 10
	collected := make([]*ports.ISCSIPortRange, n)
	for i := range n {
		r, err := framework.AllocateISCSIPortRange(fmt.Sprintf("TC-22.%d", i), "pool")
		if err != nil {
			t.Fatalf("AllocateISCSIPortRange #%d: %v", i, err)
		}
		collected[i] = r
	}

	for i := range n {
		for j := i + 1; j < n; j++ {
			if collected[i].Overlaps(collected[j]) {
				t.Errorf("range[%d]=%s overlaps range[%d]=%s",
					i, collected[i], j, collected[j])
			}
		}
	}
}

// ─── Must* variants ──────────────────────────────────────────────────────────

func TestMustAllocateHostPort_DoesNotPanicOnSuccess(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("MustAllocateHostPort panicked: %v", r)
		}
	}()
	h, release := framework.MustAllocateHostPort("TC-30", "must")
	defer release()
	if h == nil {
		t.Fatal("MustAllocateHostPort returned nil handle")
	}
}

func TestMustAllocateContainerPort_DoesNotPanicOnSuccess(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("MustAllocateContainerPort panicked: %v", r)
		}
	}()
	h, release := framework.MustAllocateContainerPort("TC-31", "must-container")
	defer release()
	if h == nil {
		t.Fatal("MustAllocateContainerPort returned nil handle")
	}
}

func TestMustAllocateISCSIPortRange_DoesNotPanicOnSuccess(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("MustAllocateISCSIPortRange panicked: %v", r)
		}
	}()
	r := framework.MustAllocateISCSIPortRange("TC-32", "must-range")
	if r == nil {
		t.Fatal("MustAllocateISCSIPortRange returned nil")
	}
}

// ─── PortSet ─────────────────────────────────────────────────────────────────

func TestPortSet_HostPort_ReturnsDeterministicHandle(t *testing.T) {
	ps := framework.NewPortSet("TC-40")
	defer ps.Close() //nolint:errcheck

	h1 := ps.HostPort("grpc")
	h2 := ps.HostPort("grpc")

	if h1 == nil {
		t.Fatal("first HostPort returned nil")
	}
	if h1.Port != h2.Port {
		t.Errorf("same label returned different ports: %d vs %d", h1.Port, h2.Port)
	}
}

func TestPortSet_ContainerPort_ReturnsDeterministicHandle(t *testing.T) {
	ps := framework.NewPortSet("TC-41")
	defer ps.Close() //nolint:errcheck

	h1 := ps.ContainerPort("iscsi")
	h2 := ps.ContainerPort("iscsi")

	if h1 == nil {
		t.Fatal("first ContainerPort returned nil")
	}
	if h1.Port != h2.Port {
		t.Errorf("same label returned different ports: %d vs %d", h1.Port, h2.Port)
	}
}

func TestPortSet_Get_ReturnsNilForUnallocatedLabel(t *testing.T) {
	ps := framework.NewPortSet("TC-42")
	defer ps.Close() //nolint:errcheck

	if got := ps.Get("nonexistent"); got != nil {
		t.Errorf("Get(nonexistent) = %v, want nil", got)
	}
}

func TestPortSet_Get_ReturnsHandleAfterAllocation(t *testing.T) {
	ps := framework.NewPortSet("TC-43")
	defer ps.Close() //nolint:errcheck

	allocated := ps.HostPort("svc")
	got := ps.Get("svc")
	if got == nil {
		t.Fatal("Get after HostPort returned nil")
	}
	if got.Port != allocated.Port {
		t.Errorf("Get port %d != HostPort port %d", got.Port, allocated.Port)
	}
}

func TestPortSet_DifferentLabels_UniquePortsWithinSet(t *testing.T) {
	ps := framework.NewPortSet("TC-44")
	defer ps.Close() //nolint:errcheck

	h1 := ps.HostPort("grpc-a")
	h2 := ps.HostPort("grpc-b")

	if h1.Port == h2.Port {
		t.Errorf("different labels returned same port %d", h1.Port)
	}
}

func TestPortSet_HostPort_PortReleasedAfterClose(t *testing.T) {
	ps := framework.NewPortSet("TC-45")
	h := ps.HostPort("to-release")
	addr := h.Addr

	if err := ps.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close the OS listener must be released; re-bind must succeed.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Errorf("Listen on released port %s: %v", addr, err)
	} else {
		ln.Close()
	}
}

func TestPortSet_CloseIsIdempotent(t *testing.T) {
	ps := framework.NewPortSet("TC-46")
	_ = ps.HostPort("idempotent")

	for i := range 3 {
		if err := ps.Close(); err != nil {
			t.Errorf("Close call %d: unexpected error: %v", i, err)
		}
	}
}

func TestPortSet_NilClose_Noop(t *testing.T) {
	var ps *framework.PortSet
	if err := ps.Close(); err != nil {
		t.Errorf("nil PortSet.Close: %v", err)
	}
}

// ─── Concurrency ─────────────────────────────────────────────────────────────

func TestConcurrentHostPortAllocations_AllUnique(t *testing.T) {
	const goroutines = 50

	type result struct {
		port    int
		release func()
		err     error
	}
	ch := make(chan result, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, release, err := framework.AllocateHostPort(
				fmt.Sprintf("TC-concurrent-%d", i), "svc",
			)
			if err != nil {
				ch <- result{err: err}
				return
			}
			// Hold the port — do NOT release yet. Send the release function to
			// the caller so it can release AFTER uniqueness is verified.
			// Releasing inside the goroutine would allow reuse of port numbers
			// by later goroutines, causing false duplicates in the seen map.
			ch <- result{port: h.Port, release: release}
		}(i)
	}
	wg.Wait()
	close(ch)

	var releases []func()
	seen := make(map[int]int)
	for r := range ch {
		if r.err != nil {
			t.Errorf("concurrent AllocateHostPort error: %v", r.err)
			continue
		}
		seen[r.port]++
		if r.release != nil {
			releases = append(releases, r.release)
		}
	}
	// Release all ports only after uniqueness is verified.
	for _, release := range releases {
		release()
	}
	for port, count := range seen {
		if count > 1 {
			t.Errorf("port %d allocated %d times; must be unique", port, count)
		}
	}
}

func TestConcurrentContainerPortAllocations_AllUnique(t *testing.T) {
	const goroutines = 50

	type result struct {
		port    int
		release func()
		err     error
	}
	ch := make(chan result, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, release, err := framework.AllocateContainerPort(
				fmt.Sprintf("TC-cpx-%d", i), "iscsi",
			)
			if err != nil {
				ch <- result{err: err}
				return
			}
			// Do NOT defer release here: hold the registry entry open until after
			// the uniqueness check. Probe-and-release ports are deregistered on
			// release(), making the port number available to the OS again. If we
			// released inside the goroutine, a concurrent goroutine could receive
			// the same port number before it has been observed by the collector,
			// causing a spurious duplicate.
			ch <- result{port: h.Port, release: release}
		}(i)
	}
	wg.Wait()
	close(ch)

	seen := make(map[int]int)
	var releases []func()
	for r := range ch {
		if r.err != nil {
			t.Errorf("concurrent AllocateContainerPort error: %v", r.err)
			continue
		}
		seen[r.port]++
		if r.release != nil {
			releases = append(releases, r.release)
		}
	}

	// Release all ports AFTER uniqueness check so that no allocation is
	// deregistered while a concurrent goroutine is still running.
	for _, rel := range releases {
		rel()
	}

	for port, count := range seen {
		if count > 1 {
			t.Errorf("port %d allocated %d times; must be unique", port, count)
		}
	}
}

func TestConcurrentISCSIPortRanges_AllNonOverlapping(t *testing.T) {
	const goroutines = 50

	type rangeResult struct {
		base, end int
		caseIdx   int
		err       error
	}
	ch := make(chan rangeResult, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := framework.AllocateISCSIPortRange(
				fmt.Sprintf("TC-range-%d", i), "targets",
			)
			if err != nil {
				ch <- rangeResult{err: err}
				return
			}
			ch <- rangeResult{base: r.Base, end: r.End, caseIdx: r.CaseIndex}
		}(i)
	}
	wg.Wait()
	close(ch)

	type rangeItem struct{ base, end, caseIdx int }
	var collected []rangeItem
	for r := range ch {
		if r.err != nil {
			t.Errorf("concurrent AllocateISCSIPortRange error: %v", r.err)
			continue
		}
		collected = append(collected, rangeItem{r.base, r.end, r.caseIdx})
	}

	// All CaseIndex values must be distinct.
	seenIdx := make(map[int]int)
	for _, r := range collected {
		seenIdx[r.caseIdx]++
	}
	for idx, count := range seenIdx {
		if count > 1 {
			t.Errorf("CaseIndex %d assigned %d times; must be unique", idx, count)
		}
	}

	// No two ranges may overlap: [a.base, a.end) ∩ [b.base, b.end) = ∅.
	for i := range len(collected) {
		for j := i + 1; j < len(collected); j++ {
			a, b := collected[i], collected[j]
			if a.base < b.end && b.base < a.end {
				t.Errorf("range[%d] [%d,%d) overlaps range[%d] [%d,%d)",
					i, a.base, a.end, j, b.base, b.end)
			}
		}
	}
}

// ─── PortHandle.String ───────────────────────────────────────────────────────

func TestPortHandle_String_ContainsTCIDAndLabel(t *testing.T) {
	h, release, err := framework.AllocateHostPort("TC-99", "diagnostic-label")
	if err != nil {
		t.Fatalf("AllocateHostPort: %v", err)
	}
	defer release()

	s := h.String()
	if len(s) == 0 {
		t.Fatal("String() returned empty string")
	}
	for _, want := range []string{"TC-99", "diagnostic-label"} {
		found := false
		for i := 0; i+len(want) <= len(s); i++ {
			if s[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("String() = %q, missing substring %q", s, want)
		}
	}
}

func TestPortHandle_String_NilReturnsNonEmpty(t *testing.T) {
	var h *framework.PortHandle
	if s := h.String(); s == "" {
		t.Error("nil PortHandle.String() returned empty string")
	}
}
