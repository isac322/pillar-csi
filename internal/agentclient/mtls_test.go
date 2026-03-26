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

// MTLS integration tests.
//
// These tests verify that:
//  1. A Manager configured with valid mTLS credentials can connect to an agent
//     gRPC server that requires mutual TLS and invoke RPCs successfully.
//  2. A Manager using plaintext (insecure) credentials is rejected by an mTLS
//     server — the RPC returns an error, not a successful response.
//  3. A Manager configured with mTLS credentials but pointing at a plaintext
//     server fails (prevents downgrade).
//
// All certificates are generated in-memory using testcerts.New, so no disk
// I/O or external tooling is required.

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agentclient"
	"github.com/bhyoo/pillar-csi/internal/testutil/testcerts"
	"github.com/bhyoo/pillar-csi/internal/tlscreds"
)

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------.

// healthyMockServer is a minimal AgentServiceServer that always returns a
// healthy HealthCheck response.  Reused across mTLS tests so we can focus on
// transport-level behavior.
type healthyMockServer struct {
	agentv1.UnimplementedAgentServiceServer
}

func (*healthyMockServer) HealthCheck(
	_ context.Context,
	_ *agentv1.HealthCheckRequest,
) (*agentv1.HealthCheckResponse, error) {
	return &agentv1.HealthCheckResponse{
		Healthy:      true,
		AgentVersion: "0.1.0-mtls-test",
		CheckedAt:    timestamppb.Now(),
	}, nil
}

// startMTLSServer starts a gRPC server with mTLS credentials derived from
// bundle on a random port on 127.0.0.1.  It returns the server address and
// registers a cleanup function that stops the server at the end of the test.
func startMTLSServer(t *testing.T, bundle *testcerts.Bundle) string {
	t.Helper()

	serverCreds, err := tlscreds.NewServerCredentials(bundle.ServerCert, bundle.ServerKey, bundle.CACert)
	if err != nil {
		t.Fatalf("NewServerCredentials: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	grpcSrv := grpc.NewServer(grpc.Creds(serverCreds))
	agentv1.RegisterAgentServiceServer(grpcSrv, &healthyMockServer{})

	go func() { _ = grpcSrv.Serve(lis) }() //nolint:errcheck // gRPC server error is logged by the server itself
	t.Cleanup(func() { grpcSrv.GracefulStop() })

	return lis.Addr().String()
}

// startPlaintextServer starts a gRPC server with no TLS on a random port.
func startPlaintextServer(t *testing.T) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, &healthyMockServer{})

	go func() { _ = grpcSrv.Serve(lis) }() //nolint:errcheck // gRPC server error is logged by the server itself
	t.Cleanup(func() { grpcSrv.GracefulStop() })

	return lis.Addr().String()
}

// shortCtx returns a context with a 5-second deadline for each test call.
func shortCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------.

// TestMTLS_ClientAndServerMatchingCreds verifies that a Manager holding valid
// mTLS client credentials can successfully call HealthCheck on a server that
// requires mTLS.
func TestMTLS_ClientAndServerMatchingCreds(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	addr := startMTLSServer(t, bundle)

	clientCreds, err := tlscreds.NewClientCredentials(
		bundle.ClientCert, bundle.ClientKey, bundle.CACert,
		"", /* serverName derived from dial address */
	)
	if err != nil {
		t.Fatalf("NewClientCredentials: %v", err)
	}

	m := agentclient.NewManagerWithOptions(grpc.WithTransportCredentials(clientCreds))
	t.Cleanup(func() { _ = m.Close() }) //nolint:errcheck // cleanup errors are non-actionable in test teardown

	resp, err := m.HealthCheck(shortCtx(t), addr)
	if err != nil {
		t.Fatalf("HealthCheck over mTLS: unexpected error: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("expected Healthy=true, got false")
	}
}

// TestMTLS_PlaintextClientRejectedByMTLSServer verifies that a Manager using
// insecure (plaintext) credentials cannot successfully invoke RPCs on a server
// that requires mTLS.  The HealthCheck call must return a non-nil error.
func TestMTLS_PlaintextClientRejectedByMTLSServer(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	addr := startMTLSServer(t, bundle)

	// Plaintext manager — should be rejected by the mTLS server.
	m := agentclient.NewManagerWithOptions(grpc.WithTransportCredentials(insecure.NewCredentials()))
	t.Cleanup(func() { _ = m.Close() }) //nolint:errcheck // cleanup errors are non-actionable in test teardown

	_, err = m.HealthCheck(shortCtx(t), addr)
	if err == nil {
		t.Fatal("expected HealthCheck to fail for a plaintext client connecting to an mTLS server, but got nil error")
	}
	t.Logf("received expected rejection error: %v", err)
}

// TestMTLS_MTLSClientRejectedByPlaintextServer verifies that a Manager
// configured with mTLS credentials fails to connect to a plaintext (insecure)
// server.  This prevents silent downgrade where the controller thinks it is
// speaking mTLS but the agent accepted a plaintext connection.
func TestMTLS_MTLSClientRejectedByPlaintextServer(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	addr := startPlaintextServer(t)

	clientCreds, err := tlscreds.NewClientCredentials(
		bundle.ClientCert, bundle.ClientKey, bundle.CACert,
		"", /* serverName derived from dial address */
	)
	if err != nil {
		t.Fatalf("NewClientCredentials: %v", err)
	}

	m := agentclient.NewManagerWithOptions(grpc.WithTransportCredentials(clientCreds))
	t.Cleanup(func() { _ = m.Close() }) //nolint:errcheck // cleanup errors are non-actionable in test teardown

	_, err = m.HealthCheck(shortCtx(t), addr)
	if err == nil {
		t.Fatal("expected HealthCheck to fail for an mTLS client connecting to a plaintext server, but got nil error")
	}
	t.Logf("received expected rejection error: %v", err)
}

// TestMTLS_WrongCAClientRejected verifies that a client presenting a
// certificate signed by a different CA is rejected by the mTLS server.
func TestMTLS_WrongCAClientRejected(t *testing.T) {
	// Server uses bundle1.
	bundle1, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New bundle1: %v", err)
	}

	// Client uses bundle2 — signed by a completely different CA.
	bundle2, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New bundle2: %v", err)
	}

	addr := startMTLSServer(t, bundle1)

	// Client credentials use bundle2's client cert but bundle1's CA for server
	// verification.  The server, however, only trusts bundle1's CA for client
	// certs, so the client cert from bundle2 will be rejected.
	clientCreds, err := tlscreds.NewClientCredentials(
		bundle2.ClientCert, bundle2.ClientKey,
		bundle1.CACert, // server is bundle1, so we trust its CA for the TLS handshake
		"",
	)
	if err != nil {
		t.Fatalf("NewClientCredentials: %v", err)
	}

	m := agentclient.NewManagerWithOptions(grpc.WithTransportCredentials(clientCreds))
	t.Cleanup(func() { _ = m.Close() }) //nolint:errcheck // cleanup errors are non-actionable in test teardown

	_, err = m.HealthCheck(shortCtx(t), addr)
	if err == nil {
		t.Fatal("expected HealthCheck to fail when client cert is signed by wrong CA, but got nil error")
	}
	t.Logf("received expected rejection error: %v", err)
}
