package main

import (
	"context"
	"sync/atomic"
	"testing"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"

	csisvc "github.com/bhyoo/pillar-csi/internal/csi"
)

func TestProbe_Controller_NotReadyUntilStarted(t *testing.T) {
	var started atomic.Bool
	server := csisvc.NewIdentityServerWithReadyFn(driverName, "test", managerStartedReadyFn(&started))

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe before manager start returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready before manager start = true, want false")
	}

	started.Store(true)
	response, err = server.Probe(context.Background(), &csispec.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe after manager start returned error: %v", err)
	}
	if !response.GetReady().GetValue() {
		t.Fatal("Probe ready after manager start = false, want true")
	}
}
