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

// Package agentclient provides a gRPC connection manager for pillar-agent
// instances.  Controllers use this package to obtain an AgentServiceClient
// bound to a specific storage node, identified by its resolved address
// (e.g. "192.168.1.10:9500" from PillarTarget.status.resolvedAddress).
//
// # Trust boundary
//
// Production deployments should use [NewManagerFromFiles] or
// [NewManagerWithTLSCredentials] to supply mTLS credentials so that the
// controller presents a client certificate (signed by a cluster-internal CA)
// to every pillar-agent and verifies the agent's server certificate in return.
//
// [NewManager] still exists for environments where TLS is terminated upstream
// or for development / CI setups where certificate infrastructure is not yet
// available.  It produces plaintext (insecure) gRPC connections.
//
// # Connection reuse
//
// [Manager] caches one *grpc.ClientConn per address.  Repeated calls to
// [Manager.Dial] with the same address return a client backed by the same
// underlying connection, which is created lazily on the first call and reused
// thereafter.  Call [Manager.Close] to release all cached connections when the
// controller shuts down.
package agentclient

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/tlscreds"
)

// Dialer is the testable interface for obtaining an AgentServiceClient
// connected to a given pillar-agent endpoint.
//
// Implementations are expected to:
//   - Cache connections and reuse them across calls to the same address.
//   - Be safe for concurrent use from multiple goroutines.
//   - Return idiomatic gRPC status errors on failure.
type Dialer interface {
	// Dial returns an AgentServiceClient connected to the agent at address.
	// address must be a host:port string, e.g. "192.168.1.10:9500".
	//
	// The returned client is backed by a cached connection.  Callers MUST NOT
	// close or modify the underlying connection — call Close on the Dialer
	// itself when it is no longer needed.
	Dial(ctx context.Context, address string) (agentv1.AgentServiceClient, error)

	// HealthCheck dials address, calls the agent's HealthCheck RPC, and
	// returns the response.  It is a convenience wrapper around Dial that
	// allows callers to verify connectivity and populate
	// PillarTarget.status.conditions without importing agentv1 directly.
	HealthCheck(ctx context.Context, address string) (*agentv1.HealthCheckResponse, error)

	// Close releases all cached gRPC connections managed by this Dialer.
	// It is safe to call Close multiple times; subsequent calls are no-ops.
	// After Close returns, calling Dial or HealthCheck returns an error.
	Close() error

	// GetCapabilities dials address, calls the agent's GetCapabilities RPC,
	// and returns the response.  It is a convenience wrapper around Dial that
	// allows callers to query agent capabilities and populate
	// PillarTarget.status.capabilities / discoveredPools without importing
	// agentv1 or the gRPC client directly.
	GetCapabilities(ctx context.Context, address string) (*agentv1.GetCapabilitiesResponse, error)

	// IsMTLS reports whether connections produced by this Dialer use mutual
	// TLS authentication.  The PillarTarget controller uses this to populate
	// the AgentConnected condition reason:
	//   - true  → reason "Authenticated" (mTLS handshake verified both sides)
	//   - false → reason "Dialed"        (TCP reachable, no TLS)
	IsMTLS() bool
}

// connEntry holds a cached gRPC client connection together with its derived
// AgentServiceClient.
type connEntry struct {
	conn   *grpc.ClientConn
	client agentv1.AgentServiceClient
}

// Manager implements [Dialer] using real gRPC connections.  Connections are
// created lazily on the first call to Dial for a given address and cached for
// reuse.
//
// Use [NewManagerFromFiles] or [NewManagerWithTLSCredentials] to create a
// Manager with mTLS transport security.  Use [NewManager] only when TLS is not
// required (development / plaintext environments).
type Manager struct {
	mu       sync.Mutex
	conns    map[string]*connEntry
	dialOpts []grpc.DialOption
	closed   bool
	// mtls is true when the Manager was constructed with mTLS credentials.
	// It is reported via IsMTLS() so callers (e.g. the PillarTarget controller)
	// can distinguish "Dialed" (plaintext) from "Authenticated" (mTLS) in the
	// AgentConnected status condition.
	mtls bool
}

// Ensure Manager satisfies the Dialer interface at compile time.
var _ Dialer = (*Manager)(nil)

// NewManager constructs a Manager with plaintext (insecure) gRPC transport.
//
// This constructor is suitable for development, CI environments where TLS is
// terminated upstream, and unit tests.  For production deployments with a
// cluster-internal CA, prefer [NewManagerFromFiles] or
// [NewManagerWithTLSCredentials] which enforce mutual TLS.
//
// To override options — for example to inject a test server address via
// grpc.WithContextDialer — use [NewManagerWithOptions].
func NewManager() *Manager {
	return NewManagerWithOptions(defaultDialOptions()...)
}

// NewManagerFromFiles constructs a Manager with mTLS transport credentials
// loaded from the given PEM-encoded certificate files.
//
// CertFile is the path to the controller's client certificate (PEM).
// KeyFile is the path to the corresponding private key (PEM).
// CaFile is the path to the CA certificate that signed the agent's server
// certificate (PEM).
// ServerName overrides the TLS server-name used for SAN verification; pass
// an empty string to derive the server name from the dial target address
// (typically the agent IP or hostname from PillarTarget.status.resolvedAddress).
//
// Returns an error if any file cannot be read or the certificate material
// cannot be parsed.  On success the returned Manager enforces mutual TLS for
// every connection it creates; agents that do not present a certificate signed
// by the same CA will be rejected.
//
// The returned Manager reports IsMTLS() == true, which causes the
// PillarTarget controller to surface reason "Authenticated" in the
// AgentConnected status condition on a successful health-check.
func NewManagerFromFiles(certFile, keyFile, caFile, serverName string) (*Manager, error) {
	creds, err := tlscreds.LoadClientCredentials(certFile, keyFile, caFile, serverName)
	if err != nil {
		return nil, fmt.Errorf("agentclient: load mTLS credentials from files: %w", err)
	}
	m := NewManagerWithOptions(grpc.WithTransportCredentials(creds))
	m.mtls = true
	return m, nil
}

// NewManagerWithTLSCredentials constructs a Manager with the supplied
// pre-built mTLS [credentials.TransportCredentials].
//
// This constructor is useful when the caller has already loaded certificate
// material from a Kubernetes Secret or cert-manager CertificateRequest and
// wishes to avoid writing PEM files to disk.  The credentials must have been
// built with [tlscreds.NewClientCredentials] or an equivalent that configures
// mutual TLS.
//
// The returned Manager reports IsMTLS() == true, which causes the
// PillarTarget controller to surface reason "Authenticated" in the
// AgentConnected status condition on a successful health-check.
func NewManagerWithTLSCredentials(creds credentials.TransportCredentials) *Manager {
	m := NewManagerWithOptions(grpc.WithTransportCredentials(creds))
	m.mtls = true
	return m
}

// NewManagerWithOptions constructs a Manager with the given gRPC dial options.
// The caller is responsible for supplying appropriate credentials.  This
// constructor exists primarily to allow unit tests to inject a custom dialer
// (e.g. grpc.WithContextDialer pointing at a bufconn.Listener).
func NewManagerWithOptions(opts ...grpc.DialOption) *Manager {
	return &Manager{
		conns:    make(map[string]*connEntry),
		dialOpts: opts,
	}
}

// defaultDialOptions returns the plaintext gRPC dial options used by
// [NewManager].
//
// Use [NewManagerFromFiles] or [NewManagerWithTLSCredentials] for mTLS.
func defaultDialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
}

// Dial returns an AgentServiceClient connected to the agent at address.
//
// The first call for a given address creates a new *grpc.ClientConn and caches
// it; subsequent calls return a client backed by the same connection.  The
// gRPC library manages the underlying TCP connection, performing reconnects
// automatically if the server becomes temporarily unavailable.
//
// Address must be a host:port string, e.g. "192.168.1.10:9500".
func (m *Manager) Dial(_ context.Context, address string) (agentv1.AgentServiceClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, fmt.Errorf("agentclient: Manager is closed")
	}

	if entry, ok := m.conns[address]; ok {
		return entry.client, nil
	}

	// grpc.NewClient (formerly grpc.DialContext) does not block on connection
	// establishment when grpc.WithBlock is absent — the underlying TCP handshake
	// happens in the background.  The context parameter is not consumed here
	// because grpc.NewClient does not accept a context; it is retained in the
	// interface signature for consistency with callers that use it for timeouts.
	conn, err := grpc.NewClient(address, m.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("agentclient: dial %q: %w", address, err)
	}

	entry := &connEntry{
		conn:   conn,
		client: agentv1.NewAgentServiceClient(conn),
	}
	m.conns[address] = entry
	return entry.client, nil
}

// HealthCheck dials address, issues a HealthCheck RPC, and returns the
// response.  On success the caller may inspect the Healthy field and the
// per-subsystem status list to determine whether the agent is ready to serve
// requests and to populate PillarTarget.status conditions.
//
// Any gRPC transport error (connection refused, timeout, etc.) is returned
// as-is and can be treated as AgentConnected=False by the caller.
func (m *Manager) HealthCheck(
	ctx context.Context,
	address string,
) (*agentv1.HealthCheckResponse, error) {
	client, err := m.Dial(ctx, address)
	if err != nil {
		return nil, err
	}

	resp, err := client.HealthCheck(ctx, &agentv1.HealthCheckRequest{})
	if err != nil {
		return nil, fmt.Errorf("agentclient: HealthCheck %q: %w", address, err)
	}
	return resp, nil
}

// GetCapabilities dials address, issues a GetCapabilities RPC, and returns
// the response.  The caller may use the result to populate
// PillarTarget.status.capabilities and .discoveredPools.
//
// Any gRPC transport error (connection refused, timeout, etc.) is returned
// as-is.  The caller should treat such errors as a non-fatal best-effort
// failure and log accordingly rather than propagating them as reconcile errors.
func (m *Manager) GetCapabilities(
	ctx context.Context,
	address string,
) (*agentv1.GetCapabilitiesResponse, error) {
	client, err := m.Dial(ctx, address)
	if err != nil {
		return nil, err
	}

	resp, err := client.GetCapabilities(ctx, &agentv1.GetCapabilitiesRequest{})
	if err != nil {
		return nil, fmt.Errorf("agentclient: GetCapabilities %q: %w", address, err)
	}
	return resp, nil
}

// Close releases all cached gRPC connections.  After Close returns, calls to
// Dial or HealthCheck will return an error.  It is safe to call Close more
// than once; subsequent calls are no-ops that return nil.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	var firstErr error
	for addr, entry := range m.conns {
		err := entry.conn.Close()
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("agentclient: close connection to %q: %w", addr, err)
		}
	}
	m.conns = nil
	return firstErr
}

// IsMTLS reports whether this Manager was constructed with mTLS transport
// credentials.  It returns true for Managers created by [NewManagerFromFiles]
// or [NewManagerWithTLSCredentials], and false for those created by [NewManager]
// or [NewManagerWithOptions].
//
// The PillarTarget controller uses this to set the AgentConnected condition
// reason: "Authenticated" when mTLS is active, "Dialed" when using plain TCP.
func (m *Manager) IsMTLS() bool {
	return m.mtls
}
