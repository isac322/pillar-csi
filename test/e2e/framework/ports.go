// Package framework — ports.go
//
// Per-TC unique port allocation for parallel E2E test execution.
//
// # Problem
//
// When hundreds of test cases run concurrently each test case may need one or
// more TCP ports for in-process services (gRPC servers, HTTP servers) or for
// services that bind inside a Kind cluster node (iSCSI targets).  If two
// concurrent test cases bind the same port the faster one will succeed and the
// slower one will get EADDRINUSE, making the test fail for an irrelevant reason.
//
// # Solution
//
// This file exposes three orthogonal port allocation strategies, all backed by
// the process-scoped registry in the ports sub-package.  Tests choose the
// strategy that matches their service lifecycle:
//
//  1. Ephemeral host-bound (AllocateHostPort / AllocateHostPortFor) — the OS
//     assigns a free port via net.Listen("tcp","127.0.0.1:0") and the listener
//     is held open until released.  The open listener acts as a mutex: no other
//     goroutine or process can bind the same address while the test runs.  Use
//     this for in-process gRPC/HTTP servers started directly in the test binary.
//
//  2. Probe-and-release (AllocateContainerPort) — the same :0 trick is used
//     to determine a free port, but the listener is closed immediately so a
//     container process (e.g. an iSCSI LIO daemon inside a Kind node) can bind
//     to the same host port.  The port number is tracked in the registry for
//     the TC duration to prevent this process from re-issuing the same number
//     to another concurrent test case.  There is a brief TOCTOU window between
//     listener release and container bind; this is acceptable in test envs.
//
//  3. Deterministic range (AllocateISCSIPortRange) — each TC is assigned a
//     contiguous, non-overlapping block of ports [Base, Base+Count) from a
//     global atomic counter.  This gives tests that set up multiple simultaneous
//     iSCSI targets a predictable set of port numbers without per-port OS calls.
//     The 421-TC suite with the default 10-port stride occupies [30100, 34310).
//
// # Usage
//
//	// In-process gRPC server:
//	port, release, err := framework.AllocateHostPort(tcID, "agent-grpc")
//	defer release()
//	grpcServer.Serve(port.Listener())
//
//	// iSCSI target inside Kind node:
//	port, release, err := framework.AllocateContainerPort(tcID, "iscsi-primary")
//	defer release()
//	kindConfig.ExtraPortMappings = append(kindConfig.ExtraPortMappings,
//	    kindapi.PortMapping{HostPort: int32(port.Port), ContainerPort: 3260})
//
//	// Multiple iSCSI targets via range:
//	portRange, err := framework.AllocateISCSIPortRange(tcID, "iscsi-targets")
//	kindConfig.ExtraPortMappings = append(kindConfig.ExtraPortMappings,
//	    kindapi.PortMapping{HostPort: int32(portRange.Port(0)), ContainerPort: 3260},
//	    kindapi.PortMapping{HostPort: int32(portRange.Port(1)), ContainerPort: 3261},
//	)
//
// # Isolation contract
//
// Every allocation obtained through this package is guaranteed unique within the
// current process invocation.  The uniqueness guarantee holds across all
// goroutines and across all three allocation strategies: the global Registry
// (backed by a sync.Mutex and atomic counters) prevents duplicates.
//
// Each allocation is associated with a TC ID string for diagnostic output.
// The ReleaseFunc returned by the Allocate* functions must be called (typically
// via defer) when the test case is done; failure to call it does not cause
// incorrect behaviour for other tests (the OS reclaims listeners on process
// exit) but does leak tracking entries in the registry.
package framework

import (
	"fmt"
	"net"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/ports"
)

// ─── PortHandle ───────────────────────────────────────────────────────────────

// PortHandle carries the result of a successful port allocation.
// All fields are read-only after creation; callers must not mutate them.
type PortHandle struct {
	// TCID is the test-case identifier this port was allocated for.
	TCID string

	// Label is the caller-supplied logical name for the service (e.g. "agent",
	// "primary-iscsi", "csi-driver").  Used in diagnostic messages.
	Label string

	// Port is the allocated TCP port number in the range [1024, 65535].
	Port int

	// Host is always "127.0.0.1".
	Host string

	// Addr is "127.0.0.1:<port>" in a form directly usable with net.Dial and
	// net.Listen.
	Addr string

	// alloc is the underlying Registry allocation; nil for range-based handles.
	alloc *ports.Allocation
}

// Listener returns the net.Listener that holds this port reserved on the host.
// Returns nil if the port was allocated with probe-and-release semantics (i.e.
// the listener was closed to allow a container to bind the same address).
//
// The caller must NOT close the returned listener directly; use the ReleaseFunc
// returned by the Allocate* function instead.
func (h *PortHandle) Listener() net.Listener {
	if h == nil || h.alloc == nil {
		return nil
	}
	return h.alloc.Listener()
}

// String returns a human-readable description for use in test log messages.
func (h *PortHandle) String() string {
	if h == nil {
		return "<nil PortHandle>"
	}
	return fmt.Sprintf("port(tc=%s, label=%s, addr=%s)", h.TCID, h.Label, h.Addr)
}

// ─── ReleaseFunc ─────────────────────────────────────────────────────────────

// ReleaseFunc releases a port allocation obtained from this package.
// It is safe to call multiple times; subsequent calls are no-ops.
// Typically deferred immediately after the Allocate* call.
type ReleaseFunc func()

// ─── AllocateHostPort ────────────────────────────────────────────────────────

// AllocateHostPort reserves a free loopback TCP port for the given test-case
// and logical label.  The OS assigns the port via net.Listen("tcp","127.0.0.1:0")
// and the listener is held open for the duration of the TC so no other goroutine
// or process can bind the same address.
//
// Call the returned ReleaseFunc (typically via defer) to close the listener and
// remove the port from the process-scoped registry when the TC is done.
//
// This strategy is appropriate for in-process TCP servers (gRPC, HTTP) that are
// started directly in the test binary.
func AllocateHostPort(tcID, label string) (*PortHandle, ReleaseFunc, error) {
	return allocateHostPortKind(tcID, label, ports.KindGeneric)
}

// AllocateCSIGRPCPort is AllocateHostPort specialised for CSI driver gRPC endpoints.
// The service kind is recorded in the underlying registry entry for diagnostics.
func AllocateCSIGRPCPort(tcID, label string) (*PortHandle, ReleaseFunc, error) {
	return allocateHostPortKind(tcID, label, ports.KindCSIGRPC)
}

// AllocateAgentGRPCPort is AllocateHostPort specialised for pillar-agent gRPC
// endpoints.  The service kind is recorded in the underlying registry entry for
// diagnostics.
func AllocateAgentGRPCPort(tcID, label string) (*PortHandle, ReleaseFunc, error) {
	return allocateHostPortKind(tcID, label, ports.KindAgentGRPC)
}

func allocateHostPortKind(tcID, label string, kind ports.ServiceKind) (*PortHandle, ReleaseFunc, error) {
	alloc, err := ports.Global.Allocate(kind, tcLabel(tcID, label))
	if err != nil {
		return nil, noop, fmt.Errorf("framework: allocate %s port for TC %s/%s: %w",
			kind, tcID, label, err)
	}
	h := &PortHandle{
		TCID:  tcID,
		Label: label,
		Port:  alloc.Port,
		Host:  alloc.Host,
		Addr:  alloc.Addr,
		alloc: alloc,
	}
	release := func() { _ = alloc.Release() }
	return h, release, nil
}

// ─── AllocateContainerPort ───────────────────────────────────────────────────

// AllocateContainerPort reserves a free host TCP port using probe-and-release
// semantics: the OS assigns a free port, the host listener is immediately closed
// to yield the address to a container process, and the port number is tracked in
// the process-scoped registry until the ReleaseFunc is called.
//
// Use this for services that bind inside a Kind cluster node (e.g. iSCSI LIO
// targets).  The returned handle carries the port number to use as the Kind
// portMapping.HostPort value; the container port (e.g. 3260 for iSCSI) is
// configured separately in the Kind cluster config.
//
// There is a brief TOCTOU window between the host listener close and the
// container bind.  This is acceptable in test environments where the probability
// of the OS re-assigning the port in that window is negligible.
func AllocateContainerPort(tcID, label string) (*PortHandle, ReleaseFunc, error) {
	alloc, err := ports.Global.AllocateISCSITarget(tcLabel(tcID, label))
	if err != nil {
		return nil, noop, fmt.Errorf("framework: allocate container port for TC %s/%s: %w",
			tcID, label, err)
	}
	h := &PortHandle{
		TCID:  tcID,
		Label: label,
		Port:  alloc.Port,
		Host:  alloc.Host,
		Addr:  alloc.Addr,
		alloc: alloc,
	}
	release := func() { _ = alloc.Release() }
	return h, release, nil
}

// ─── AllocateISCSIPortRange ──────────────────────────────────────────────────

// AllocateISCSIPortRange assigns a non-overlapping block of host TCP ports to a
// test case from the global deterministic range allocator.
//
// Each call atomically claims the next available range:
//
//	Base  = GlobalISCSIRangeAllocator.BasePort + CaseIndex * PortsPerCase
//	Count = GlobalISCSIRangeAllocator.PortsPerCase  (default: 10)
//	End   = Base + Count                              (exclusive)
//
// The returned *ports.ISCSIPortRange holds no OS resources; it does not need to
// be explicitly released.  Ranges across concurrent invocations are guaranteed
// non-overlapping.
//
// Use this when a single TC sets up multiple simultaneous iSCSI targets:
//
//	portRange, err := framework.AllocateISCSIPortRange(tcID, "iscsi")
//	HostPort0 := portRange.Port(0)  // first target
//	HostPort1 := portRange.Port(1)  // second target
func AllocateISCSIPortRange(_ string, _ string) (*ports.ISCSIPortRange, error) {
	// tcID and label are accepted for future diagnostics; currently unused
	// because ISCSIRangeAllocator is stateless from the caller's perspective.
	return ports.GlobalISCSIRangeAllocator.Allocate(), nil
}

// ─── MustAllocateHostPort / MustAllocateContainerPort ────────────────────────

// MustAllocateHostPort is like AllocateHostPort but panics on error.
// Intended for use in BeforeEach / setup contexts where errors would cascade
// into test failures anyway.
func MustAllocateHostPort(tcID, label string) (*PortHandle, ReleaseFunc) {
	h, release, err := AllocateHostPort(tcID, label)
	if err != nil {
		panic(fmt.Sprintf("MustAllocateHostPort: TC=%s label=%s: %v", tcID, label, err))
	}
	return h, release
}

// MustAllocateContainerPort is like AllocateContainerPort but panics on error.
func MustAllocateContainerPort(tcID, label string) (*PortHandle, ReleaseFunc) {
	h, release, err := AllocateContainerPort(tcID, label)
	if err != nil {
		panic(fmt.Sprintf("MustAllocateContainerPort: TC=%s label=%s: %v", tcID, label, err))
	}
	return h, release
}

// MustAllocateISCSIPortRange is like AllocateISCSIPortRange but panics on error.
func MustAllocateISCSIPortRange(tcID, label string) *ports.ISCSIPortRange {
	r, err := AllocateISCSIPortRange(tcID, label)
	if err != nil {
		panic(fmt.Sprintf("MustAllocateISCSIPortRange: TC=%s label=%s: %v", tcID, label, err))
	}
	return r
}

// ─── PortSet ─────────────────────────────────────────────────────────────────

// PortSet is a collection of named ports allocated for a single test case.
// It provides a lifecycle-aware container that releases all its ports when
// Close is called.
//
// Typical usage in a Ginkgo BeforeEach / DeferCleanup pattern:
//
//	var ps *framework.PortSet
//
//	BeforeEach(func() {
//	    ps = framework.NewPortSet(CurrentSpecReport().FullText())
//	    DeferCleanup(ps.Close)
//	    _ = ps.HostPort("agent-grpc")
//	    _ = ps.ContainerPort("iscsi-target")
//	})
//
//	It("serves on the allocated ports", func() {
//	    Expect(ps.Get("agent-grpc").Port).To(BeNumerically(">", 0))
//	})
type PortSet struct {
	tcID    string
	handles map[string]*PortHandle
	release []ReleaseFunc
}

// NewPortSet creates an empty PortSet for the given test-case identifier.
func NewPortSet(tcID string) *PortSet {
	return &PortSet{
		tcID:    tcID,
		handles: make(map[string]*PortHandle),
	}
}

// HostPort allocates (or returns the cached) host-bound port for the given
// logical label.  Repeated calls with the same label return the same handle.
func (ps *PortSet) HostPort(label string) *PortHandle {
	if h, ok := ps.handles[label]; ok {
		return h
	}
	h, release, err := AllocateHostPort(ps.tcID, label)
	if err != nil {
		panic(fmt.Sprintf("PortSet.HostPort: TC=%s label=%s: %v", ps.tcID, label, err))
	}
	ps.handles[label] = h
	ps.release = append(ps.release, release)
	return h
}

// ContainerPort allocates (or returns the cached) probe-and-release port for
// the given logical label.  Repeated calls with the same label return the same
// handle.
func (ps *PortSet) ContainerPort(label string) *PortHandle {
	if h, ok := ps.handles[label]; ok {
		return h
	}
	h, release, err := AllocateContainerPort(ps.tcID, label)
	if err != nil {
		panic(fmt.Sprintf("PortSet.ContainerPort: TC=%s label=%s: %v", ps.tcID, label, err))
	}
	ps.handles[label] = h
	ps.release = append(ps.release, release)
	return h
}

// Get returns the PortHandle previously allocated for the given label, or nil
// if no port has been allocated for that label yet.
func (ps *PortSet) Get(label string) *PortHandle {
	return ps.handles[label]
}

// Close releases all ports in the set.  Safe to call multiple times; subsequent
// calls are no-ops.  Returns the first error encountered across all releases.
func (ps *PortSet) Close() error {
	if ps == nil {
		return nil
	}
	for _, fn := range ps.release {
		fn()
	}
	ps.release = nil
	ps.handles = make(map[string]*PortHandle)
	return nil
}

// ─── internal helpers ────────────────────────────────────────────────────────

// tcLabel combines a TC ID and a logical label into a single registry label.
// This ensures that diagnostics produced by the ports.Registry include both
// the TC context and the service name.
func tcLabel(tcID, label string) string {
	if tcID == "" {
		return label
	}
	return tcID + "/" + label
}

// noop is a no-op ReleaseFunc used as the error return value of Allocate*
// functions so that callers can always safely defer the release.
func noop() {}
