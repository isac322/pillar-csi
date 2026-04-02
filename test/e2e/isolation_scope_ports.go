package e2e

// isolation_scope_ports.go — typed port reservation methods for TestCaseScope.
//
// These methods extend ReserveLoopbackPort with service-kind semantics so that
// test code can say "I need an iSCSI target port" rather than "I need any free
// port".  Each method:
//
//  1. Delegates to the process-scoped ports.Global registry for allocation.
//  2. Wraps the resulting ports.Allocation in the existing PortLease type so
//     that all callers share a uniform API.
//  3. Stores the allocation in the scope's portAllocs map for cleanup on
//     TestCaseScope.Close().
//
// The registry-backed allocations live alongside the existing portLeases map;
// both are drained in Close().

import (
	"errors"
	"fmt"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/ports"
)

// ReserveISCSITargetPort allocates a host TCP port for an iSCSI target that
// will bind inside a Kind container.
//
// The OS assigns a free port (via net.Listen(":0")), then the host listener is
// immediately released (probe-and-release strategy) so that the Kind container
// can bind to the same host port.  The returned lease carries the port number;
// its Addr field is "127.0.0.1:<port>".
//
// The port is tracked in the global ports.Registry until the scope is closed
// (TestCaseScope.Close), preventing this process from re-issuing the same
// number to another concurrent test.
//
// Typical usage:
//
//	iscsiLease, err := scope.ReserveISCSITargetPort("zfs-pool")
//	// … configure Kind portMapping with HostPort: int32(iscsiLease.Port) …
func (s *TestCaseScope) ReserveISCSITargetPort(label string) (*PortLease, error) {
	return s.reserveTypedPort(label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateISCSITarget(label)
	}, false /* probe-and-release: listener already closed */)
}

// RecreateISCSITargetPort closes any existing iSCSI target port lease for the
// label and allocates a fresh port.
func (s *TestCaseScope) RecreateISCSITargetPort(label string) (*PortLease, error) {
	return s.recreateTypedPort(label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateISCSITarget(label)
	}, false)
}

// ReserveCSIGRPCPort allocates a host-bound loopback port for a CSI driver
// gRPC endpoint.  Unlike ReserveISCSITargetPort, the host listener is held
// open until the scope is closed, guaranteeing that no other process can bind
// to the same address while the test is running.
//
// Typical usage:
//
//	csiLease, err := scope.ReserveCSIGRPCPort("driver")
//	grpcServer.Serve(csiLease.ToNetListener())
func (s *TestCaseScope) ReserveCSIGRPCPort(label string) (*PortLease, error) {
	return s.reserveTypedPort(label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateCSIGRPC(label)
	}, true /* listener held open */)
}

// RecreateCSIGRPCPort closes any existing CSI gRPC port lease for the label
// and allocates a fresh port.
func (s *TestCaseScope) RecreateCSIGRPCPort(label string) (*PortLease, error) {
	return s.recreateTypedPort(label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateCSIGRPC(label)
	}, true)
}

// ReserveAgentGRPCPort allocates a host-bound loopback port for a
// pillar-agent gRPC endpoint.  The host listener is held open until the scope
// is closed.
func (s *TestCaseScope) ReserveAgentGRPCPort(label string) (*PortLease, error) {
	return s.reserveTypedPort(label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateAgentGRPC(label)
	}, true)
}

// RecreateAgentGRPCPort closes any existing agent gRPC port lease for the
// label and allocates a fresh port.
func (s *TestCaseScope) RecreateAgentGRPCPort(label string) (*PortLease, error) {
	return s.recreateTypedPort(label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateAgentGRPC(label)
	}, true)
}

// ReserveISCSIPortRange assigns a non-overlapping block of host TCP ports to
// this test case using the global ISCSIRangeAllocator.
//
// The range is computed deterministically as:
//
//	Base = GlobalISCSIRangeAllocator.BasePort + CaseIndex * PortsPerCase
//	End  = Base + PortsPerCase  (exclusive)
//
// where CaseIndex is atomically assigned at the moment of the first call.
// Subsequent calls with the same label return the cached range, so this method
// is idempotent within a single scope.
//
// Unlike ReserveISCSITargetPort (which allocates a single random OS port per
// target), ReserveISCSIPortRange gives the test case an entire slice of ports
// so it can set up multiple simultaneous iSCSI targets without additional
// allocator calls:
//
//	portRange, err := scope.ReserveISCSIPortRange("targets")
//	// primary target:
//	kindConfig.Nodes[0].ExtraPortMappings = append(..., PortMapping{
//	    HostPort:      int32(portRange.Port(0)),
//	    ContainerPort: 3260,
//	})
//	// secondary target:
//	kindConfig.Nodes[0].ExtraPortMappings = append(..., PortMapping{
//	    HostPort:      int32(portRange.Port(1)),
//	    ContainerPort: 3261,
//	})
//
// The returned *ports.ISCSIPortRange holds no OS resources and does not need
// to be explicitly released.  The scope deregisters it from the internal
// tracking map when Close() is called.
func (s *TestCaseScope) ReserveISCSIPortRange(label string) (*ports.ISCSIPortRange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errors.New("test case scope is closed")
	}

	key := "range:" + pathToken(label)
	if existing, ok := s.iscsiRanges[key]; ok {
		return existing, nil
	}

	portRange := ports.GlobalISCSIRangeAllocator.Allocate()
	s.iscsiRanges[key] = portRange
	return portRange, nil
}

// RecreateISCSIPortRange discards any existing port range for the label and
// allocates a fresh range from the global allocator.  The new range has a
// higher CaseIndex than the previous one.
//
// This is useful for test cases that need to tear down and re-provision a set
// of iSCSI targets with a fresh, predictable port block.
func (s *TestCaseScope) RecreateISCSIPortRange(label string) (*ports.ISCSIPortRange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errors.New("test case scope is closed")
	}

	key := "range:" + pathToken(label)
	// Remove the old range (no OS resources to release).
	delete(s.iscsiRanges, key)

	portRange := ports.GlobalISCSIRangeAllocator.Allocate()
	s.iscsiRanges[key] = portRange
	return portRange, nil
}

// ─── internal helpers ────────────────────────────────────────────────────────

// reserveTypedPort is the common implementation for the typed port reservation
// methods.  allocFn performs the registry allocation; holdListener indicates
// whether the underlying net.Listener should be surfaced through the PortLease
// (host-bound case) or left nil (probe-and-release case).
func (s *TestCaseScope) reserveTypedPort(
	label string,
	allocFn func() (*ports.Allocation, error),
	holdListener bool,
) (*PortLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errors.New("test case scope is closed")
	}

	// Use a namespaced key to avoid collisions with plain ReserveLoopbackPort.
	key := "typed:" + pathToken(label)
	if existing, ok := s.portLeases[key]; ok {
		return existing, nil
	}

	alloc, err := allocFn()
	if err != nil {
		return nil, fmt.Errorf("reserve typed port for %s/%s: %w", s.TCID, label, err)
	}

	var listener interface{ Close() error }
	if holdListener {
		listener = alloc.Listener()
	}

	lease := &PortLease{
		TCID:     s.TCID,
		ScopeTag: s.ScopeTag,
		Label:    label,
		Host:     alloc.Host,
		Port:     alloc.Port,
		Addr:     alloc.Addr,
	}
	if holdListener && alloc.Listener() != nil {
		lease.listener = alloc.Listener()
	}
	_ = listener // consumed above

	s.portLeases[key] = lease
	s.portAllocs[key] = alloc
	return lease, nil
}

// recreateTypedPort releases any existing typed port lease for the label and
// allocates a fresh one.
func (s *TestCaseScope) recreateTypedPort(
	label string,
	allocFn func() (*ports.Allocation, error),
	holdListener bool,
) (*PortLease, error) {
	key := "typed:" + pathToken(label)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("test case scope is closed")
	}
	existingLease := s.portLeases[key]
	existingAlloc := s.portAllocs[key]
	delete(s.portLeases, key)
	delete(s.portAllocs, key)
	s.mu.Unlock()

	if existingLease != nil {
		if err := existingLease.Close(); err != nil {
			return nil, fmt.Errorf("release typed port lease for %s/%s: %w", s.TCID, label, err)
		}
	}
	if existingAlloc != nil {
		if err := existingAlloc.Release(); err != nil {
			return nil, fmt.Errorf("release typed port alloc for %s/%s: %w", s.TCID, label, err)
		}
	}

	return s.reserveTypedPort(label, allocFn, holdListener)
}
