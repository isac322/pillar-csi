// Package ports provides a process-scoped port registry for E2E test suites.
// It ensures that concurrent tests never allocate conflicting TCP ports.
//
// # Port allocation strategies
//
// Host-bound allocation (default): opens a net.Listener on 127.0.0.1:0 so the
// OS picks a free ephemeral port, then holds the listener open to prevent any
// other goroutine or process from grabbing the same number.  The caller
// receives the assigned port number; when the Allocation is released (via
// Release or Close), the listener is closed.
//
// Probe-and-release allocation: used when the actual TCP binding will happen
// inside a container (e.g. an iSCSI target inside a Kind node).  A listener is
// opened on :0 to obtain a free OS-assigned port, immediately closed to yield
// the port to the container binder, and the port number is returned.  There is
// an inherent TOCTOU window between the release and the container bind; this is
// acceptable in test environments where the window is negligible.
//
// # Service kinds
//
// Different service kinds are tracked with metadata so that test failures can
// report which logical service was involved.  All allocation strategies use the
// OS port 0 trick; the only difference is whether the listener is held open.
//
// # Usage
//
//	// In-process service (listener stays open):
//	alloc, err := ports.Global.Allocate(ports.KindAgentGRPC, "primary")
//	defer alloc.Release()
//	grpcServer.Serve(alloc.Listener())
//
//	// Container service (port yielded immediately):
//	alloc, err := ports.Global.AllocateForContainer(ports.KindISCSITarget, "zfs-pool")
//	defer alloc.Release()
//	kindConfig.Nodes[0].ExtraPortMappings = append(..., PortMapping{
//	    HostPort:      int32(alloc.Port),
//	    ContainerPort: 3260,
//	})
package ports

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
)

// ServiceKind identifies the type of service that needs a port.  It is used
// for documentation and debugging only — all service kinds use the same
// underlying OS-level allocation.
type ServiceKind string

const (
	// KindISCSITarget is used for iSCSI target services (LIO, TGT, etc.)
	// running inside Kind containers.  The allocation is probe-and-release so
	// that the Kind container can bind to the same host port.
	KindISCSITarget ServiceKind = "iscsi-target"

	// KindCSIGRPC is used for CSI driver gRPC endpoints listening on the host.
	// The listener is held open until explicitly released.
	KindCSIGRPC ServiceKind = "csi-grpc"

	// KindAgentGRPC is used for pillar-agent gRPC endpoints listening on the
	// host.  The listener is held open until explicitly released.
	KindAgentGRPC ServiceKind = "agent-grpc"

	// KindGeneric is used for any other TCP service that needs a unique port.
	KindGeneric ServiceKind = "generic"
)

// Allocation holds a single port reservation returned by the Registry.
// Call Close or Release when the port is no longer needed.
type Allocation struct {
	// Service is the logical service kind this port was allocated for.
	Service ServiceKind

	// Label is the caller-supplied logical label (e.g. "agent", "primary").
	Label string

	// Host is always "127.0.0.1".
	Host string

	// Port is the allocated TCP port number.
	Port int

	// Addr is Host:Port in a form suitable for net.Dial / net.Listen.
	Addr string

	mu       sync.Mutex
	listener net.Listener // non-nil when a host listener holds the port
	reg      *Registry    // back-pointer for deregistration
	released bool
}

// Listener returns the underlying net.Listener held for this allocation.
// Returns nil if the allocation was created with probe-and-release semantics
// (i.e. the listener was already closed to yield the port to a container).
// The caller must NOT close the returned listener directly; use Release instead.
func (a *Allocation) Listener() net.Listener {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listener
}

// Close releases the port reservation.  It deregisters the allocation from the
// owning Registry and closes the underlying listener (if any).  It is safe to
// call multiple times; subsequent calls are no-ops.
func (a *Allocation) Close() error {
	return a.Release()
}

// Release is an alias for Close that reads more naturally in defer/cleanup
// contexts.
func (a *Allocation) Release() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.released {
		return nil
	}
	a.released = true

	var errs []error
	if a.listener != nil {
		if err := a.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close listener for %s/%s port %d: %w",
				a.Service, a.Label, a.Port, err))
		}
		a.listener = nil
	}
	if a.reg != nil {
		a.reg.deregister(a.Port)
	}
	return errors.Join(errs...)
}

// Registry is a process-scoped port registry.  Use NewRegistry to create one.
// The package-level Global is appropriate for most tests.  All methods are safe
// for concurrent use.
type Registry struct {
	mu          sync.Mutex
	active      map[int]*Allocation
	totalIssued atomic.Int64
}

// NewRegistry creates a new, empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		active: make(map[int]*Allocation),
	}
}

// Global is the process-scoped port registry shared by all E2E tests running
// inside a single test binary invocation.  Tests using TestCaseScope should
// prefer scope.ReserveLoopbackPort (which delegates here) over calling Global
// directly; direct use is appropriate only for test-level bootstrap that
// happens outside the per-TC scope lifecycle.
var Global = NewRegistry()

// allocMaxAttempts is the maximum number of OS port picks attempted before
// giving up.  In practice the first attempt succeeds; retries are only needed
// when the OS returns a port that a previous probe-and-release allocation
// already registered in this registry.
const allocMaxAttempts = 20

// Allocate reserves a free loopback TCP port for the given service kind and
// label.  The OS assigns the port (via net.Listen("tcp","127.0.0.1:0")) so
// the number is guaranteed unique within this process.  The returned Allocation
// holds an open listener; callers MUST call a.Close() or a.Release() when the
// port is no longer needed.
//
// This is the preferred strategy for in-process services (gRPC servers, HTTP
// servers, etc.) started directly in the test binary.
//
// Conflict avoidance: when the OS returns a port that was previously issued by
// a probe-and-release AllocateForContainer call (and is therefore still tracked
// in the registry), Allocate closes the listener and retries.  This prevents
// duplicate-port collisions between host-bound and container allocations.
func (r *Registry) Allocate(service ServiceKind, label string) (*Allocation, error) {
	for range allocMaxAttempts {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("ports: allocate %s/%s: %w", service, label, err)
		}

		port, err := listenerPort(ln)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("ports: parse listener port %s/%s: %w", service, label, err)
		}

		// Check and register atomically while holding the registry lock.
		// We still hold the OS listener, so the port cannot be re-assigned to
		// any other goroutine during the check.
		r.mu.Lock()
		if _, conflict := r.active[port]; conflict {
			// Port already tracked (e.g. from a prior probe-and-release
			// allocation).  Release and try again.
			r.mu.Unlock()
			_ = ln.Close()
			continue
		}
		a := &Allocation{
			Service:  service,
			Label:    label,
			Host:     "127.0.0.1",
			Port:     port,
			Addr:     ln.Addr().String(),
			listener: ln,
			reg:      r,
		}
		r.active[port] = a
		r.mu.Unlock()

		r.totalIssued.Add(1)
		return a, nil
	}
	return nil, fmt.Errorf("ports: allocate %s/%s: exhausted %d attempts (registry conflict on every OS pick)", service, label, allocMaxAttempts)
}

// AllocateForContainer reserves a free loopback TCP port and immediately
// releases the host listener so the target container can bind to the same
// port.  The returned Allocation carries the port number but has a nil
// Listener(); Release is a no-op (it only deregisters from the Registry).
//
// Use this for services that bind inside a Kind container (e.g. iSCSI LIO
// targets) whose host port mapping must be configured at cluster-creation time.
// There is a brief TOCTOU window between listener release and container bind;
// this is acceptable in test environments where the window is negligible.
//
// Collision safety: the registry is updated while the OS listener is still
// open (before Close), so no concurrent net.Listen(":0") call within this
// process can return the same port number during the registration step.
// A conflict-checking retry loop guards against the rare case where the OS
// re-offers a port that is still tracked in the registry from a prior call.
func (r *Registry) AllocateForContainer(service ServiceKind, label string) (*Allocation, error) {
	for range allocMaxAttempts {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("ports: allocate-for-container %s/%s: %w", service, label, err)
		}

		port, err := listenerPort(ln)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("ports: parse listener port %s/%s: %w", service, label, err)
		}

		// Check and register atomically while still holding the OS listener.
		// The OS cannot re-assign this port to any other goroutine while the
		// listener is open, so the registry update is race-free.
		r.mu.Lock()
		if _, conflict := r.active[port]; conflict {
			// Already tracked; close and retry for a different port.
			r.mu.Unlock()
			_ = ln.Close()
			continue
		}
		a := &Allocation{
			Service: service,
			Label:   label,
			Host:    "127.0.0.1",
			Port:    port,
			Addr:    fmt.Sprintf("127.0.0.1:%d", port),
			reg:     r,
			// listener is nil — probe-and-release allocation
		}
		r.active[port] = a
		r.mu.Unlock()

		// Release the host listener so the container can bind to this port.
		// The registry entry is already committed; a post-close conflict from
		// another goroutine getting this port is caught by its own retry loop.
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			r.deregister(port)
			return nil, fmt.Errorf("ports: release probe listener %s/%s: %w", service, label, err)
		}

		r.totalIssued.Add(1)
		return a, nil
	}
	return nil, fmt.Errorf("ports: allocate-for-container %s/%s: exhausted %d attempts (registry conflict on every OS pick)", service, label, allocMaxAttempts)
}

// AllocateISCSITarget is a convenience wrapper that allocates a port for an
// iSCSI target that will bind inside a Kind container.
//
// The caller should use a.Port as the Kind portMapping.hostPort value and
// 3260 (or a custom container port) as portMapping.containerPort.
func (r *Registry) AllocateISCSITarget(label string) (*Allocation, error) {
	return r.AllocateForContainer(KindISCSITarget, label)
}

// AllocateCSIGRPC is a convenience wrapper that allocates a host-bound port
// for a CSI driver gRPC endpoint.
func (r *Registry) AllocateCSIGRPC(label string) (*Allocation, error) {
	return r.Allocate(KindCSIGRPC, label)
}

// AllocateAgentGRPC is a convenience wrapper that allocates a host-bound port
// for a pillar-agent gRPC endpoint.
func (r *Registry) AllocateAgentGRPC(label string) (*Allocation, error) {
	return r.Allocate(KindAgentGRPC, label)
}

// ActiveCount returns the number of currently active allocations tracked by
// this Registry.  Useful for asserting no port leaks in tests.
func (r *Registry) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

// TotalIssued returns the cumulative number of allocations ever issued by this
// Registry (including already-released ones).
func (r *Registry) TotalIssued() int64 {
	return r.totalIssued.Load()
}

// Snapshot returns a copy of the currently active Allocations.  Intended for
// diagnostics in test teardown hooks.
func (r *Registry) Snapshot() []*Allocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Allocation, 0, len(r.active))
	for _, a := range r.active {
		out = append(out, a)
	}
	return out
}

// ─── internal helpers ────────────────────────────────────────────────────────

func (r *Registry) deregister(port int) {
	r.mu.Lock()
	delete(r.active, port)
	r.mu.Unlock()
}

// listenerPort extracts the numeric port from a net.Listener's address.
func listenerPort(ln net.Listener) (int, error) {
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return 0, fmt.Errorf("split host/port from %q: %w", ln.Addr().String(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("parse port %q: %w", portStr, err)
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("port %d out of valid range", port)
	}
	return port, nil
}
