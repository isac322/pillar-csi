// Package ports — range_allocator.go
//
// ISCSIRangeAllocator assigns a non-overlapping slice of host TCP ports to each
// parallel test case so that concurrent iSCSI targets never compete for the
// same port.  The assignment is deterministic: test case index N receives ports
// [Base + N*PortsPerCase, Base + (N+1)*PortsPerCase).
//
// Motivation:
//
// When many parallel test cases each spin up one or more iSCSI targets inside a
// Kind container they must forward different host ports (via Kind's portMapping)
// to the container's iSCSI daemon.  Using a shared OS :0 listener (the strategy
// in Registry.AllocateISCSITarget) guarantees uniqueness but produces
// non-deterministic port numbers that can make test logs hard to correlate.
// The range allocator complements the OS-random strategy by offering a
// predictable, index-based layout:
//
//	case 0 → ports [30100, 30110)
//	case 1 → ports [30110, 30120)
//	case 2 → ports [30120, 30130)
//	…
//
// All ports are above 1024 so they can be forwarded on the host without
// elevated privileges.  The 437-TC suite with the default 10-port stride
// occupies [30100, 34470) — well within the ephemeral range.
//
// Usage:
//
//	// Obtain a range from the global allocator:
//	r := ports.GlobalISCSIRangeAllocator.Allocate()
//
//	// Use Port(0) as the Kind host-port for the primary iSCSI target:
//	kindConfig.Nodes[0].ExtraPortMappings = append(kindConfig.Nodes[0].ExtraPortMappings,
//	    kindapi.PortMapping{
//	        HostPort:      int32(r.Port(0)),
//	        ContainerPort: 3260,  // standard iSCSI port inside the container
//	    },
//	)
//
//	// Via TestCaseScope (preferred):
//	portRange, err := scope.ReserveISCSIPortRange("iscsi")
//	hostPort := portRange.Port(0)
package ports

import (
	"fmt"
	"sync/atomic"
)

// ─── constants ────────────────────────────────────────────────────────────────

const (
	// ISCSIRangeBasePort is the first host port in the global iSCSI range pool.
	// 30100 sits comfortably within the Linux ephemeral range (32768–60999 on
	// most distributions) while avoiding the common 30000–30099 block used by
	// some Kubernetes NodePort defaults.  No elevated privileges are required.
	ISCSIRangeBasePort = 30100

	// ISCSIPortsPerCase is the default number of host ports reserved per parallel
	// test case.  Ten ports covers test cases that set up multiple simultaneous
	// iSCSI targets (e.g. one per backend pool) with room to spare.
	ISCSIPortsPerCase = 10
)

// ─── ISCSIPortRange ───────────────────────────────────────────────────────────

// ISCSIPortRange describes a contiguous block of host TCP ports exclusively
// assigned to one parallel test case for iSCSI target services.
//
// The range is [Base, End) where End = Base + Count.  Individual ports are
// accessed by index via Port(n).
//
// ISCSIPortRange holds no OS resources; it does not need to be explicitly
// released.  The TestCaseScope that owns it deregisters it on Close() for
// bookkeeping purposes only.
type ISCSIPortRange struct {
	// CaseIndex is the 0-based sequential index assigned by the allocator.
	// The first Allocate() call returns CaseIndex 0, the second returns 1, etc.
	CaseIndex int

	// Base is the first (inclusive) host port in the range.
	// Base = allocator.BasePort + CaseIndex * allocator.PortsPerCase.
	Base int

	// Count is the number of ports in the range.  Equal to the allocator's
	// PortsPerCase at the time of allocation.
	Count int

	// End is the first (exclusive) port beyond the range: Base + Count.
	// No port in the range equals End.
	End int
}

// Port returns the n-th host port in this range (0-based).
//
// For example, if Base is 30110 and Count is 10:
//
//	Port(0) == 30110   ← use for the first iSCSI target
//	Port(1) == 30111   ← use for a second iSCSI target, if needed
//	…
//	Port(9) == 30119   ← last port in the range
//
// Port panics if n is not in [0, Count).  Callers should use Contains to
// validate untrusted indices.
func (r *ISCSIPortRange) Port(n int) int {
	if r == nil {
		panic("ISCSIPortRange.Port: nil receiver")
	}
	if n < 0 || n >= r.Count {
		panic(fmt.Sprintf("ISCSIPortRange.Port: index %d out of range [0, %d)", n, r.Count))
	}
	return r.Base + n
}

// Contains reports whether port p falls within [Base, End).
func (r *ISCSIPortRange) Contains(port int) bool {
	if r == nil {
		return false
	}
	return port >= r.Base && port < r.End
}

// Overlaps reports whether r and other share at least one port.
// Two ranges overlap when they are both non-nil and their intervals intersect.
func (r *ISCSIPortRange) Overlaps(other *ISCSIPortRange) bool {
	if r == nil || other == nil {
		return false
	}
	return r.Base < other.End && other.Base < r.End
}

// String returns a human-readable description of the port range, suitable for
// use in test log messages and error output.
func (r *ISCSIPortRange) String() string {
	if r == nil {
		return "<nil ISCSIPortRange>"
	}
	return fmt.Sprintf("iscsi-range(case=%d, ports=%d–%d, count=%d)",
		r.CaseIndex, r.Base, r.End-1, r.Count)
}

// ─── ISCSIRangeAllocator ──────────────────────────────────────────────────────

// ISCSIRangeAllocator assigns non-overlapping port ranges to parallel test
// cases.  An atomic counter tracks the next available case index; each call to
// Allocate increments the counter and computes the corresponding range.
//
// The allocator is safe for concurrent use without additional locking.
// It holds no OS resources, so there is nothing to close or release at the
// allocator level.
type ISCSIRangeAllocator struct {
	// BasePort is the first host port in the allocation pool.  Must be ≥ 1024
	// (unprivileged) and ≤ 65535 - PortsPerCase.
	BasePort int

	// PortsPerCase is the number of consecutive ports reserved per test case.
	// Must be ≥ 1.
	PortsPerCase int

	nextIndex atomic.Int64
}

// NewISCSIRangeAllocator returns an ISCSIRangeAllocator rooted at basePort
// with portsPerCase ports reserved per test case.
//
// Typical values: basePort=30100, portsPerCase=10.
//
// Panics if basePort < 1024, portsPerCase < 1, or
// basePort+portsPerCase > 65535.
func NewISCSIRangeAllocator(basePort, portsPerCase int) *ISCSIRangeAllocator {
	if basePort < 1024 {
		panic(fmt.Sprintf("NewISCSIRangeAllocator: basePort %d < 1024 (unprivileged port required)", basePort))
	}
	if portsPerCase < 1 {
		panic(fmt.Sprintf("NewISCSIRangeAllocator: portsPerCase %d < 1", portsPerCase))
	}
	if basePort+portsPerCase > 65535 {
		panic(fmt.Sprintf("NewISCSIRangeAllocator: basePort %d + portsPerCase %d > 65535",
			basePort, portsPerCase))
	}
	return &ISCSIRangeAllocator{
		BasePort:     basePort,
		PortsPerCase: portsPerCase,
	}
}

// Allocate atomically claims the next available port range and returns it.
// Concurrent callers are guaranteed to receive distinct, non-overlapping ranges.
//
// The returned ISCSIPortRange is fully initialised with:
//
//	CaseIndex = monotonically increasing 0-based index
//	Base      = BasePort + CaseIndex * PortsPerCase
//	Count     = PortsPerCase
//	End       = Base + Count
//
// Allocate never returns nil.  If so many ranges are allocated that Base would
// exceed 65535, the returned range will have ports outside the valid TCP range;
// this indicates a test configuration error and must be caught by test setup
// validation.
func (a *ISCSIRangeAllocator) Allocate() *ISCSIPortRange {
	idx := int(a.nextIndex.Add(1) - 1) // atomic: 0-based, never negative
	base := a.BasePort + idx*a.PortsPerCase
	return &ISCSIPortRange{
		CaseIndex: idx,
		Base:      base,
		Count:     a.PortsPerCase,
		End:       base + a.PortsPerCase,
	}
}

// TotalAllocated returns the cumulative number of ranges ever allocated by this
// allocator.  Safe for concurrent access; intended for diagnostics.
func (a *ISCSIRangeAllocator) TotalAllocated() int {
	return int(a.nextIndex.Load())
}

// ─── package-level default ────────────────────────────────────────────────────

// GlobalISCSIRangeAllocator is the process-scoped iSCSI range allocator shared
// by all E2E tests in a single test binary invocation.
//
// It uses ISCSIRangeBasePort (30100) and ISCSIPortsPerCase (10), which
// accommodates up to 3543 parallel test cases before exhausting the 16-bit port
// space — far above the current suite size of 437 test cases.
//
// TestCaseScope.ReserveISCSIPortRange delegates to this allocator.  Tests
// that need isolated range accounting (e.g. unit tests of the allocator itself)
// should create a private ISCSIRangeAllocator with NewISCSIRangeAllocator
// rather than using GlobalISCSIRangeAllocator.
var GlobalISCSIRangeAllocator = NewISCSIRangeAllocator(ISCSIRangeBasePort, ISCSIPortsPerCase)
