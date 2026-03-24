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

// Package e2e (this file) contains mTLS integration tests for the
// PillarTarget controller ↔ pillar-agent trust boundary.
//
// Tests in this file exercise the full mTLS path without requiring a
// Kubernetes cluster.  They use:
//
//   - testcerts.New() to generate an ephemeral self-signed CA, server cert,
//     and client cert entirely in memory.
//   - A real gRPC listener on localhost:0, configured with the generated
//     server-side mTLS credentials.
//   - A fake controller-runtime client pre-populated with a single
//     PillarTarget resource.
//   - A real PillarTargetReconciler wired with either an mTLS or plaintext
//     agentclient.Manager.
//
// Scenarios covered:
//
//  1. TestMTLSController_AgentConnectedAuthenticated — matching mTLS creds
//     result in AgentConnected=True/Authenticated.
//  2. TestMTLSController_PlaintextDialRejected — a plaintext client is
//     rejected by the mTLS server; AgentConnected=False/HealthCheckFailed or
//     TLSHandshakeFailed.
//  3. TestMTLSController_WrongCAClientRejected — a client whose certificate
//     is signed by a different CA is rejected; AgentConnected=False.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestMTLS
package e2e

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agentclient"
	"github.com/bhyoo/pillar-csi/internal/controller"
	"github.com/bhyoo/pillar-csi/internal/testutil/testcerts"
	"github.com/bhyoo/pillar-csi/internal/tlscreds"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test-only gRPC server double
// ─────────────────────────────────────────────────────────────────────────────

// mtlsHealthyAgentServer is a minimal AgentService gRPC server used in mTLS
// E2E tests.  It always reports healthy so tests can focus on the transport
// (TLS handshake) rather than application-level behaviour.
type mtlsHealthyAgentServer struct {
	agentv1.UnimplementedAgentServiceServer
}

func (m *mtlsHealthyAgentServer) HealthCheck(
	_ context.Context,
	_ *agentv1.HealthCheckRequest,
) (*agentv1.HealthCheckResponse, error) {
	return &agentv1.HealthCheckResponse{
		Healthy:      true,
		AgentVersion: "0.1.0-mtls-e2e",
		CheckedAt:    timestamppb.Now(),
	}, nil
}

// Compile-time interface check.
var _ agentv1.AgentServiceServer = (*mtlsHealthyAgentServer)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Server / environment setup helpers
// ─────────────────────────────────────────────────────────────────────────────

// startMTLSAgentForTest starts a real gRPC server with mTLS credentials
// derived from bundle on a random port on 127.0.0.1.  The server is stopped
// via t.Cleanup and the listening address ("host:port") is returned.
func startMTLSAgentForTest(t *testing.T, bundle *testcerts.Bundle) string {
	t.Helper()

	serverCreds, err := tlscreds.NewServerCredentials(
		bundle.ServerCert, bundle.ServerKey, bundle.CACert,
	)
	if err != nil {
		t.Fatalf("startMTLSAgentForTest: NewServerCredentials: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startMTLSAgentForTest: net.Listen: %v", err)
	}

	grpcSrv := grpc.NewServer(grpc.Creds(serverCreds))
	agentv1.RegisterAgentServiceServer(grpcSrv, &mtlsHealthyAgentServer{})

	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(func() { grpcSrv.GracefulStop() })

	return lis.Addr().String()
}

// mtlsControllerEnv is a self-contained test environment for the
// PillarTargetReconciler.  It wraps a real reconciler wired to a fake
// Kubernetes client that holds a single PillarTarget in external mode.
type mtlsControllerEnv struct {
	// Reconciler is the PillarTargetReconciler under test.
	Reconciler *controller.PillarTargetReconciler

	// TargetName is the name of the pre-created PillarTarget.
	TargetName string

	// Req is the reconcile.Request for the single PillarTarget.
	Req reconcile.Request
}

// newMTLSControllerEnv builds an mtlsControllerEnv that points the
// PillarTarget at agentAddr and uses the supplied dialer for health checks.
//
// The fake Kubernetes client is pre-populated with a single PillarTarget whose
// spec.external.address/port resolve to agentAddr.
func newMTLSControllerEnv(
	t *testing.T,
	agentAddr string,
	dialer agentclient.Dialer,
) *mtlsControllerEnv {
	t.Helper()

	// Split "host:port" from the listener address so we can populate the
	// ExternalSpec fields individually.
	agentHost, agentPortStr, err := net.SplitHostPort(agentAddr)
	if err != nil {
		t.Fatalf("newMTLSControllerEnv: SplitHostPort(%q): %v", agentAddr, err)
	}
	agentPort64, err := strconv.ParseInt(agentPortStr, 10, 32)
	if err != nil {
		t.Fatalf("newMTLSControllerEnv: parse port %q: %v", agentPortStr, err)
	}

	const targetName = "test-mtls-target"

	// Build a minimal scheme that contains only the pillar-csi CRD types.
	// The External reconcile path does not look up corev1.Node objects.
	sch := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("newMTLSControllerEnv: AddToScheme: %v", err)
	}

	// Pre-create the PillarTarget with an empty status so the reconciler
	// populates it on the second reconcile pass.
	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: targetName},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{
				Address: agentHost,
				Port:    int32(agentPort64),
			},
		},
	}

	// WithStatusSubresource ensures that r.Status().Update() in the reconciler
	// persists the status conditions to the fake store, mirroring real
	// Kubernetes subresource behaviour.
	fakeClient := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&v1alpha1.PillarTarget{}).
		WithObjects(target).
		Build()

	reconciler := &controller.PillarTargetReconciler{
		Client: fakeClient,
		Scheme: sch,
		Dialer: dialer,
	}

	return &mtlsControllerEnv{
		Reconciler: reconciler,
		TargetName: targetName,
		Req:        reconcile.Request{NamespacedName: types.NamespacedName{Name: targetName}},
	}
}

// reconcileUntilCondition calls Reconcile until the named condition is set on
// the PillarTarget or the context deadline is exceeded.
//
// The PillarTargetReconciler requires two reconcile passes for a freshly
// created object:
//
//  1. First pass: adds the deletion-protection finalizer and returns.
//  2. Second pass: resolves the agent address, calls HealthCheck, and sets
//     the AgentConnected / Ready conditions.
//
// A fixed number of passes (3) is used instead of a loop to keep the test
// deterministic and fast.
func reconcileUntilCondition(t *testing.T, env *mtlsControllerEnv) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const maxPasses = 3
	for i := range maxPasses {
		if _, err := env.Reconciler.Reconcile(ctx, env.Req); err != nil {
			t.Fatalf("Reconcile pass %d: %v", i+1, err)
		}
	}
}

// fetchCondition reads the named status condition from the PillarTarget stored
// in the fake Kubernetes client.  Returns nil if the condition is absent.
func fetchCondition(
	t *testing.T,
	env *mtlsControllerEnv,
	condType string,
) *metav1.Condition {
	t.Helper()

	updated := &v1alpha1.PillarTarget{}
	if err := env.Reconciler.Get(
		context.Background(),
		types.NamespacedName{Name: env.TargetName},
		updated,
	); err != nil {
		t.Fatalf("fetchCondition: Get %q: %v", env.TargetName, err)
	}
	return apimeta.FindStatusCondition(updated.Status.Conditions, condType)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestMTLSController_AgentConnectedAuthenticated
// ─────────────────────────────────────────────────────────────────────────────

// TestMTLSController_AgentConnectedAuthenticated verifies the happy path:
// when the PillarTarget controller is configured with mTLS credentials that
// match those of the running agent, the reconciler sets
//
//	AgentConnected.Status = "True"
//	AgentConnected.Reason = "Authenticated"
//
// and
//
//	Ready.Status = "True"
//
// This asserts that the trust boundary is properly enforced: the controller
// presents a client certificate that the agent verifies, and the agent
// presents a server certificate that the controller verifies.
func TestMTLSController_AgentConnectedAuthenticated(t *testing.T) {
	t.Parallel()

	// 1. Generate an ephemeral mTLS cert bundle for 127.0.0.1.
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	// 2. Start a real mTLS gRPC agent on a random loopback port.
	agentAddr := startMTLSAgentForTest(t, bundle)
	t.Logf("mTLS agent listening at %s", agentAddr)

	// 3. Build the controller-side mTLS dialer with matching client credentials.
	//    Empty serverName lets gRPC derive the authority from the dial address
	//    so the IP SAN on the server cert is used for verification.
	clientCreds, err := tlscreds.NewClientCredentials(
		bundle.ClientCert, bundle.ClientKey, bundle.CACert,
		"" /* serverName derived from dial address */,
	)
	if err != nil {
		t.Fatalf("NewClientCredentials: %v", err)
	}
	dialer := agentclient.NewManagerWithTLSCredentials(clientCreds)
	t.Cleanup(func() { _ = dialer.Close() })

	// IsMTLS must be true so the controller emits Reason="Authenticated".
	if !dialer.IsMTLS() {
		t.Fatal("expected dialer.IsMTLS() == true for mTLS manager")
	}

	// 4. Wire the reconciler against the fake k8s client and reconcile.
	env := newMTLSControllerEnv(t, agentAddr, dialer)
	reconcileUntilCondition(t, env)

	// 5. Assert AgentConnected=True/Authenticated.
	cond := fetchCondition(t, env, "AgentConnected")
	if cond == nil {
		t.Fatal("AgentConnected condition not found after reconcile")
	}
	t.Logf("AgentConnected: status=%s reason=%q message=%q",
		cond.Status, cond.Reason, cond.Message)

	if cond.Status != metav1.ConditionTrue {
		t.Errorf("AgentConnected.Status = %s, want True", cond.Status)
	}
	if cond.Reason != "Authenticated" {
		t.Errorf("AgentConnected.Reason = %q, want %q", cond.Reason, "Authenticated")
	}

	// 6. Ready must also be True when mTLS succeeds.
	ready := fetchCondition(t, env, "Ready")
	if ready == nil {
		t.Fatal("Ready condition not found after reconcile")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status = %s, want True; message: %q", ready.Status, ready.Message)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestMTLSController_PlaintextDialRejected
// ─────────────────────────────────────────────────────────────────────────────

// TestMTLSController_PlaintextDialRejected verifies that a controller that
// dials the agent with plaintext (insecure) credentials is rejected by an
// mTLS-protected agent.  The PillarTarget controller must set
//
//	AgentConnected.Status = "False"
//	AgentConnected.Reason ∈ {"TLSHandshakeFailed", "HealthCheckFailed"}
//
// This enforces the trust boundary: no controller without a valid client
// certificate may reach the agent.
func TestMTLSController_PlaintextDialRejected(t *testing.T) {
	t.Parallel()

	// 1. Generate cert bundle for the mTLS server.
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	// 2. Start the mTLS agent.
	agentAddr := startMTLSAgentForTest(t, bundle)
	t.Logf("mTLS agent listening at %s", agentAddr)

	// 3. Create a PLAINTEXT (insecure) dialer — no client certificate.
	//    The mTLS server will refuse the connection.
	plainDialer := agentclient.NewManager()
	t.Cleanup(func() { _ = plainDialer.Close() })

	if plainDialer.IsMTLS() {
		t.Fatal("expected plainDialer.IsMTLS() == false")
	}

	// 4. Reconcile with the plaintext dialer.
	env := newMTLSControllerEnv(t, agentAddr, plainDialer)
	reconcileUntilCondition(t, env)

	// 5. AgentConnected must be False because the mTLS server rejects the
	//    connection attempt.
	cond := fetchCondition(t, env, "AgentConnected")
	if cond == nil {
		t.Fatal("AgentConnected condition not found after reconcile")
	}
	t.Logf("AgentConnected (plaintext): status=%s reason=%q message=%q",
		cond.Status, cond.Reason, cond.Message)

	if cond.Status != metav1.ConditionFalse {
		t.Errorf("AgentConnected.Status = %s, want False (plaintext should be rejected)", cond.Status)
	}

	// The reason is either TLSHandshakeFailed (when the dialer reports IsMTLS
	// and the handshake fails) or HealthCheckFailed (when the transport error
	// is caught at the RPC level).  Both indicate the plaintext dial was
	// blocked by the mTLS server.
	validReasons := map[string]bool{
		"TLSHandshakeFailed": true,
		"HealthCheckFailed":  true,
	}
	if !validReasons[cond.Reason] {
		t.Errorf("AgentConnected.Reason = %q, want TLSHandshakeFailed or HealthCheckFailed",
			cond.Reason)
	}

	// 6. Ready must be False when the agent is unreachable.
	ready := fetchCondition(t, env, "Ready")
	if ready == nil {
		t.Fatal("Ready condition not found after reconcile")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", ready.Status)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestMTLSController_WrongCAClientRejected
// ─────────────────────────────────────────────────────────────────────────────

// TestMTLSController_WrongCAClientRejected verifies that a client presenting a
// certificate signed by a different CA than the one the server trusts is
// rejected.  This exercises the "mutual" in mutual TLS: the server must also
// authenticate the client.
//
// Setup:
//   - bundle1: the CA used by the agent server (trusted by server).
//   - bundle2: a completely different CA used to sign the client cert.
//
// The client presents a cert signed by bundle2 but the server only accepts
// certs signed by bundle1.  The mTLS handshake must fail, setting
//
//	AgentConnected.Status = "False"
func TestMTLSController_WrongCAClientRejected(t *testing.T) {
	t.Parallel()

	// Bundle used by the server.
	bundle1, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New (bundle1): %v", err)
	}

	// Bundle whose client cert is signed by a different CA (bundle2).
	bundle2, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New (bundle2): %v", err)
	}

	// Start agent with bundle1's server credentials.
	agentAddr := startMTLSAgentForTest(t, bundle1)
	t.Logf("mTLS agent listening at %s", agentAddr)

	// Build client credentials: client cert from bundle2, but we trust
	// bundle1's CA for the server verification (so the server TLS cert is
	// verifiable).  The server, however, only trusts bundle1's CA for client
	// certs, so bundle2's client cert will be rejected during the handshake.
	clientCreds, err := tlscreds.NewClientCredentials(
		bundle2.ClientCert, bundle2.ClientKey,
		bundle1.CACert, // trust bundle1 CA to verify the server cert
		"",
	)
	if err != nil {
		t.Fatalf("NewClientCredentials (wrong CA): %v", err)
	}
	dialer := agentclient.NewManagerWithTLSCredentials(clientCreds)
	t.Cleanup(func() { _ = dialer.Close() })

	env := newMTLSControllerEnv(t, agentAddr, dialer)
	reconcileUntilCondition(t, env)

	cond := fetchCondition(t, env, "AgentConnected")
	if cond == nil {
		t.Fatal("AgentConnected condition not found after reconcile")
	}
	t.Logf("AgentConnected (wrong CA): status=%s reason=%q message=%q",
		cond.Status, cond.Reason, cond.Message)

	if cond.Status != metav1.ConditionFalse {
		t.Errorf("AgentConnected.Status = %s, want False (wrong-CA client should be rejected)",
			cond.Status)
	}

	// When IsMTLS()==true and the error looks like a TLS handshake failure the
	// controller emits TLSHandshakeFailed; otherwise HealthCheckFailed.  Either
	// is acceptable as long as the condition is False.
	validReasons := map[string]bool{
		"TLSHandshakeFailed": true,
		"HealthCheckFailed":  true,
	}
	if !validReasons[cond.Reason] {
		t.Errorf("AgentConnected.Reason = %q, want TLSHandshakeFailed or HealthCheckFailed",
			cond.Reason)
	}
}
