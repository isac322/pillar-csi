package main

import (
	"context"
	"testing"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"

	csisvc "github.com/isac322/pillar-csi/internal/csi"
)

// TestProbe_Controller_FollowsSocketServing pins the CSI Probe semantics that
// issue #96 depends on: readiness tracks the pod-local CSI socket, not leader
// election.  Standby replicas must answer Ready so their sidecars pass
// ProbeForever and the liveness-probe sidecar keeps the pod's liveness check
// green; shutdown must revoke readiness again.
func TestProbe_Controller_FollowsSocketServing(t *testing.T) {
	srv := &csiGRPCServer{}
	server := csisvc.NewIdentityServerWithReadyFn(driverName, "test", srv.probeReady)

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe before serving returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready before socket is serving = true, want false")
	}

	srv.serving.Store(true)
	response, err = server.Probe(context.Background(), &csispec.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe after serving returned error: %v", err)
	}
	if !response.GetReady().GetValue() {
		t.Fatal("Probe ready after socket is serving = false, want true")
	}

	srv.serving.Store(false)
	response, err = server.Probe(context.Background(), &csispec.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe after shutdown returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready after socket stopped = true, want false")
	}
}
