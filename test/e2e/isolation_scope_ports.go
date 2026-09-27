package e2e

// isolation_scope_ports.go — typed port reservation methods for TestCaseScope.
//
// These methods extend ReserveLoopbackPort with service-kind semantics so that
// test code can say "I need a CSI gRPC port" rather than "I need any free
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

// ReserveCSIGRPCPort allocates a host-bound loopback port for a CSI driver
// gRPC endpoint.  The host listener is held open until the scope is closed,
// guaranteeing that no other process can bind to the same address while the
// test is running.
//
// Typical usage:
//
//	csiLease, err := scope.ReserveCSIGRPCPort("driver")
//	grpcServer.Serve(csiLease.ToNetListener())
func (s *TestCaseScope) ReserveCSIGRPCPort(label string) (*PortLease, error) {
	return s.reserveTypedPort("csi-grpc", label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateCSIGRPC(label)
	})
}

// RecreateCSIGRPCPort closes any existing CSI gRPC port lease for the label
// and allocates a fresh port.
func (s *TestCaseScope) RecreateCSIGRPCPort(label string) (*PortLease, error) {
	return s.recreateTypedPort("csi-grpc", label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateCSIGRPC(label)
	})
}

// ReserveAgentGRPCPort allocates a host-bound loopback port for a
// pillar-agent gRPC endpoint.  The host listener is held open until the scope
// is closed.
func (s *TestCaseScope) ReserveAgentGRPCPort(label string) (*PortLease, error) {
	return s.reserveTypedPort("agent-grpc", label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateAgentGRPC(label)
	})
}

// RecreateAgentGRPCPort closes any existing agent gRPC port lease for the
// label and allocates a fresh port.
func (s *TestCaseScope) RecreateAgentGRPCPort(label string) (*PortLease, error) {
	return s.recreateTypedPort("agent-grpc", label, func() (*ports.Allocation, error) {
		return ports.Global.AllocateAgentGRPC(label)
	})
}

// ─── internal helpers ────────────────────────────────────────────────────────

// reserveTypedPort is the common implementation for the typed port reservation
// methods.  serviceTag is the port service kind (e.g. "csi-grpc",
// "agent-grpc") used to namespace the lease key so that different service types
// with the same label don't collide.  allocFn performs the registry allocation;
// the allocation's host listener is surfaced through the PortLease.
func (s *TestCaseScope) reserveTypedPort(
	serviceTag string,
	label string,
	allocFn func() (*ports.Allocation, error),
) (*PortLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errors.New("test case scope is closed")
	}

	// Use a namespaced key including the service tag so that different port
	// types (e.g. CSI gRPC vs agent gRPC) with the same label don't collide.
	key := "typed:" + serviceTag + ":" + pathToken(label)
	if existing, ok := s.portLeases[key]; ok {
		return existing, nil
	}

	alloc, err := allocFn()
	if err != nil {
		return nil, fmt.Errorf("reserve typed port for %s/%s: %w", s.TCID, label, err)
	}

	lease := &PortLease{
		TCID:     s.TCID,
		ScopeTag: s.ScopeTag,
		Label:    label,
		Host:     alloc.Host,
		Port:     alloc.Port,
		Addr:     alloc.Addr,
	}
	if alloc.Listener() != nil {
		lease.listener = alloc.Listener()
	}

	s.portLeases[key] = lease
	s.portAllocs[key] = alloc
	return lease, nil
}

// recreateTypedPort releases any existing typed port lease for the label and
// allocates a fresh one.
func (s *TestCaseScope) recreateTypedPort(
	serviceTag string,
	label string,
	allocFn func() (*ports.Allocation, error),
) (*PortLease, error) {
	key := "typed:" + serviceTag + ":" + pathToken(label)

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

	return s.reserveTypedPort(serviceTag, label, allocFn)
}
