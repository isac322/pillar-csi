package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	csisvc "github.com/isac322/pillar-csi/internal/csi"
)

// TestCSIGRPCServer_DoesNotNeedLeaderElection is the regression guard for
// issue #96: the CSI runnable must stay out of the manager's leader-election
// group or standby replicas never bind the socket and their sidecars
// crash-loop.  On the code that caused the bug this test does not compile,
// because csiGRPCServer had no NeedLeaderElection method.
func TestCSIGRPCServer_DoesNotNeedLeaderElection(t *testing.T) {
	if (&csiGRPCServer{}).NeedLeaderElection() {
		t.Fatal("csiGRPCServer must serve on every replica; " +
			"leader-electing it leaves standby pods without a CSI socket")
	}
}

// TestCSIGRPCServer_ServesProbeOnSocket proves the runnable path end-to-end
// without a manager: Start binds the unix socket, LoadState runs, and a real
// CSI client Probe over the socket reports Ready.  This mirrors what the
// liveness-probe sidecar and ProbeForever do on every replica.
func TestCSIGRPCServer_ServesProbeOnSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "csi.sock")

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctrlSrv := csisvc.NewControllerServer(fakeClient, fakeClient, driverName)
	srv := &csiGRPCServer{
		endpoint: "unix://" + sockPath,
		grpcSrv:  grpc.NewServer(),
		ctrlSrv:  ctrlSrv,
	}
	identitySrv := csisvc.NewIdentityServerWithReadyFn(driverName, "test", srv.probeReady)
	csisvc.RegisterGRPC(srv.grpcSrv, identitySrv, ctrlSrv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	conn, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	identity := csispec.NewIdentityClient(conn)

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, probeErr := identity.Probe(ctx, &csispec.ProbeRequest{})
		if probeErr == nil && resp.GetReady().GetValue() {
			break
		}
		select {
		case serveErr := <-errCh:
			t.Fatalf("CSI server exited before serving: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Probe never reported ready over %q (last err: %v)", sockPath, probeErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	info, err := identity.GetPluginInfo(ctx, &csispec.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo over served socket: %v", err)
	}
	if info.GetName() != driverName {
		t.Fatalf("GetPluginInfo name = %q, want %q", info.GetName(), driverName)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("CSI server returned error on shutdown: %v", err)
	}
	ready, readyErr := srv.probeReady(context.Background())
	if readyErr != nil || ready {
		t.Fatalf("probeReady after shutdown = (%v, %v), want (false, nil)", ready, readyErr)
	}
}
