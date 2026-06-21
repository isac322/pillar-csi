package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"

	csisvc "github.com/bhyoo/pillar-csi/internal/csi"
)

func TestProbe_Node_RequiresNvmeFabrics(t *testing.T) {
	stateDir := t.TempDir()
	missingFabrics := filepath.Join(t.TempDir(), "nvme-fabrics")
	server := csisvc.NewIdentityServerWithReadyFn(driverName, "test", nodeReadyFn(missingFabrics, stateDir))

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready without nvme-fabrics = true, want false")
	}
}

func TestProbe_Node_RequiresWritableStateDir(t *testing.T) {
	fabricsDevice := filepath.Join(t.TempDir(), "nvme-fabrics")
	if err := os.WriteFile(fabricsDevice, []byte("ok"), 0o600); err != nil {
		t.Fatalf("write fake nvme-fabrics: %v", err)
	}
	stateDirFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateDirFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write non-directory state path: %v", err)
	}
	server := csisvc.NewIdentityServerWithReadyFn(driverName, "test", nodeReadyFn(fabricsDevice, stateDirFile))

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready with non-writable state dir = true, want false")
	}
}
