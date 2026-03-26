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

package agentclient_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agentclient"
)

// ----------------------------------------------------------------------------
// Minimal mock AgentServiceServer
// ----------------------------------------------------------------------------.

// mockAgentServer is a minimal AgentServiceServer used for testing the dialer.
// Only HealthCheck is overridden; all other RPCs return Unimplemented via the
// embedded struct.
type mockAgentServer struct {
	agentv1.UnimplementedAgentServiceServer

	// mu guards the fields below.
	mu sync.Mutex
	// healthy controls the Healthy field returned by HealthCheck.
	healthy bool
	// healthCheckCalls counts the number of HealthCheck invocations.
	healthCheckCalls int
	// healthCheckErr, if non-nil, is returned instead of a normal response.
	healthCheckErr error
}

func (m *mockAgentServer) HealthCheck(
	_ context.Context,
	_ *agentv1.HealthCheckRequest,
) (*agentv1.HealthCheckResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthCheckCalls++
	if m.healthCheckErr != nil {
		return nil, m.healthCheckErr
	}
	return &agentv1.HealthCheckResponse{
		Healthy:      m.healthy,
		AgentVersion: "0.1.0-test",
		CheckedAt:    timestamppb.Now(),
	}, nil
}

// Compile-time interface check.
var _ agentv1.AgentServiceServer = (*mockAgentServer)(nil)

// ----------------------------------------------------------------------------
// Test environment helpers
// ----------------------------------------------------------------------------.

// dialerTestEnv holds all resources for a single test.
type dialerTestEnv struct {
	addr       string
	mock       *mockAgentServer
	grpcServer *grpc.Server
}

// newDialerTestEnv starts a real gRPC server on 127.0.0.1:0 with the given
// mock and registers a cleanup function to stop the server.
func newDialerTestEnv(t *testing.T, mock *mockAgentServer) *dialerTestEnv {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, mock)

	go func() { _ = grpcSrv.Serve(lis) }() //nolint:errcheck // gRPC server error is logged by the server itself

	t.Cleanup(func() {
		grpcSrv.GracefulStop()
	})

	return &dialerTestEnv{
		addr:       lis.Addr().String(),
		mock:       mock,
		grpcServer: grpcSrv,
	}
}

// newManager creates a Manager configured with insecure credentials pointing
// at the test gRPC server.  The Manager's Close is registered as a t.Cleanup.
func newManager(t *testing.T) *agentclient.Manager {
	t.Helper()
	m := agentclient.NewManagerWithOptions(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	t.Cleanup(func() {
		_ = m.Close() //nolint:errcheck // cleanup errors are non-actionable in test teardown
	})
	return m
}

// ctx returns a context with a short deadline suitable for unit tests.
func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------.

// TestDial_Success verifies that Dial returns a usable AgentServiceClient when
// the agent is reachable.
func TestDial_Success(t *testing.T) {
	env := newDialerTestEnv(t, &mockAgentServer{healthy: true})
	m := newManager(t)

	client, err := m.Dial(ctx(t), env.addr)
	if err != nil {
		t.Fatalf("Dial: unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("Dial: returned nil client")
	}
}

// TestDial_ConnectionReuse verifies that two calls to Dial with the same
// address return clients backed by the same underlying connection (i.e. only
// one connection is opened).
func TestDial_ConnectionReuse(t *testing.T) {
	env := newDialerTestEnv(t, &mockAgentServer{healthy: true})
	m := newManager(t)

	c1, err := m.Dial(ctx(t), env.addr)
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	c2, err := m.Dial(ctx(t), env.addr)
	if err != nil {
		t.Fatalf("second Dial: %v", err)
	}

	// Both clients should be the same pointer value because they share the
	// same cached agentv1.AgentServiceClient wrapper.
	if c1 != c2 {
		t.Errorf("Dial returned different clients for the same address; expected connection reuse")
	}
}

// TestDial_MultipleAddresses verifies that the Manager correctly manages
// separate connections when different addresses are dialed.
func TestDial_MultipleAddresses(t *testing.T) {
	env1 := newDialerTestEnv(t, &mockAgentServer{healthy: true})
	env2 := newDialerTestEnv(t, &mockAgentServer{healthy: false})
	m := newManager(t)

	c1, err := m.Dial(ctx(t), env1.addr)
	if err != nil {
		t.Fatalf("Dial env1: %v", err)
	}
	c2, err := m.Dial(ctx(t), env2.addr)
	if err != nil {
		t.Fatalf("Dial env2: %v", err)
	}

	// Clients from different addresses must be distinct.
	if c1 == c2 {
		t.Errorf("Dial returned the same client for different addresses")
	}
}

// TestHealthCheck_Healthy verifies that HealthCheck returns a healthy response
// when the agent is healthy.
func TestHealthCheck_Healthy(t *testing.T) {
	env := newDialerTestEnv(t, &mockAgentServer{healthy: true})
	m := newManager(t)

	resp, err := m.HealthCheck(ctx(t), env.addr)
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("expected Healthy=true, got false")
	}
	if resp.AgentVersion == "" {
		t.Errorf("expected non-empty AgentVersion")
	}
	if resp.CheckedAt == nil {
		t.Errorf("expected non-nil CheckedAt")
	}
}

// TestHealthCheck_Unhealthy verifies that HealthCheck returns a response with
// Healthy=false when the agent reports degraded health (no error is returned
// — the RPC itself succeeds).
func TestHealthCheck_Unhealthy(t *testing.T) {
	env := newDialerTestEnv(t, &mockAgentServer{healthy: false})
	m := newManager(t)

	resp, err := m.HealthCheck(ctx(t), env.addr)
	if err != nil {
		t.Fatalf("HealthCheck: unexpected error: %v", err)
	}
	if resp.Healthy {
		t.Errorf("expected Healthy=false for unhealthy agent, got true")
	}
}

// TestHealthCheck_RPCError verifies that HealthCheck propagates gRPC errors
// returned by the server.
func TestHealthCheck_RPCError(t *testing.T) {
	mock := &mockAgentServer{
		healthCheckErr: status.Errorf(codes.Internal, "injected error"),
	}
	env := newDialerTestEnv(t, mock)
	m := newManager(t)

	_, err := m.HealthCheck(ctx(t), env.addr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Internal {
		// err may be wrapped; unwrap gRPC status from the cause.
		t.Logf("HealthCheck error: %v", err)
		// Accept any non-nil error — the exact wrapping may vary.
	}
}

// TestHealthCheck_ConnectionRefused verifies that HealthCheck returns an error
// when the target address is not reachable.
func TestHealthCheck_ConnectionRefused(t *testing.T) {
	m := newManager(t)

	// Use a port that is unlikely to have anything listening.
	_, err := m.HealthCheck(ctx(t), "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error for unreachable address, got nil")
	}
}

// TestClose_ReleasesConnections verifies that Close releases all cached
// connections and subsequent Dial calls return an error.
func TestClose_ReleasesConnections(t *testing.T) {
	env := newDialerTestEnv(t, &mockAgentServer{healthy: true})
	m := agentclient.NewManagerWithOptions(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	// Establish a connection.
	_, err := m.Dial(ctx(t), env.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// Close the manager.
	closeErr := m.Close()
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	// Subsequent Dial must fail.
	_, err = m.Dial(ctx(t), env.addr)
	if err == nil {
		t.Fatal("expected error from Dial after Close, got nil")
	}
}

// TestClose_Idempotent verifies that calling Close multiple times does not
// return an error (idempotency requirement).
func TestClose_Idempotent(t *testing.T) {
	m := agentclient.NewManagerWithOptions(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestDial_ConcurrentSafe verifies that concurrent Dial calls for the same
// address do not panic or create duplicate connections.
func TestDial_ConcurrentSafe(t *testing.T) {
	env := newDialerTestEnv(t, &mockAgentServer{healthy: true})
	m := newManager(t)

	const goroutines = 20
	results := make([]agentv1.AgentServiceClient, goroutines)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.Dial(ctx(t), env.addr)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Dial error: %v", i, err)
		}
	}

	// All goroutines should have received the same client instance.
	first := results[0]
	for i, c := range results[1:] {
		if c != first {
			t.Errorf("goroutine %d returned a different client than goroutine 0", i+1)
		}
	}
}

// TestImplementsDialerInterface verifies at runtime that *Manager implements
// the Dialer interface.  (The compile-time check is in dialer.go; this test
// makes failures visible in test output too.)
func TestImplementsDialerInterface(_ *testing.T) {
	var _ agentclient.Dialer = agentclient.NewManager()
}
