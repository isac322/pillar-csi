package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
)

const testListExportNQN = "nqn.2026-01.com.bhyoo.pillar-csi:tank.vol1"

func TestListExports_ReturnsConfigfsExports(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stageListExportConfigfs(t, root)

	srv := agent.NewServer(nil, root)
	resp, err := srv.ListExports(context.Background(), &agentv1.ListExportsRequest{})
	if err != nil {
		t.Fatalf("ListExports unexpected error: %v", err)
	}
	if len(resp.GetExports()) != 1 {
		t.Fatalf("exports len = %d, want 1", len(resp.GetExports()))
	}
	e := resp.GetExports()["tank/vol1"]
	if e == nil {
		t.Fatal("export tank/vol1 not found")
	}
	if e.GetTargetId() != testListExportNQN {
		t.Errorf("TargetId = %q, want %q", e.GetTargetId(), testListExportNQN)
	}
	if e.GetAddress() != "10.0.0.1" {
		t.Errorf("Address = %q, want 10.0.0.1", e.GetAddress())
	}
	if e.GetPort() != 4420 {
		t.Errorf("Port = %d, want 4420", e.GetPort())
	}
	if e.GetVolumeRef() != "/dev/zvol/tank/vol1" {
		t.Errorf("VolumeRef = %q, want /dev/zvol/tank/vol1", e.GetVolumeRef())
	}
}

func stageListExportConfigfs(t *testing.T, root string) {
	t.Helper()
	subsysDir := filepath.Join(root, "nvmet", "subsystems", testListExportNQN)
	nsDir := filepath.Join(subsysDir, "namespaces", "1")
	mustMkdirAll(t, nsDir)
	mustWriteFile(t, filepath.Join(nsDir, "device_path"), "/dev/zvol/tank/vol1\n")
	mustWriteFile(t, filepath.Join(nsDir, "enable"), "1\n")
	mustMkdirAll(t, filepath.Join(subsysDir, "allowed_hosts"))

	portDir := filepath.Join(root, "nvmet", "ports", "1")
	portSubsysDir := filepath.Join(portDir, "subsystems")
	mustMkdirAll(t, portSubsysDir)
	mustWriteFile(t, filepath.Join(portDir, "addr_traddr"), "10.0.0.1\n")
	mustWriteFile(t, filepath.Join(portDir, "addr_trsvcid"), "4420\n")
	mustSymlink(t, subsysDir, filepath.Join(portSubsysDir, testListExportNQN))
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	err := os.MkdirAll(path, 0o750)
	if err != nil {
		t.Fatalf("create dir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	err := os.Symlink(oldname, newname)
	if err != nil {
		t.Fatalf("symlink %s -> %s: %v", newname, oldname, err)
	}
}

func TestListExports_EmptyConfigfs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	srv := agent.NewServer(nil, root)

	resp, err := srv.ListExports(context.Background(), &agentv1.ListExportsRequest{})
	if err != nil {
		t.Fatalf("ListExports unexpected error: %v", err)
	}
	if len(resp.GetExports()) != 0 {
		t.Errorf("expected empty exports map, got %d entries", len(resp.GetExports()))
	}
}
