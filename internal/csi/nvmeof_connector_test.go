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

package csi

// Unit tests for NVMeoFConnector — Connect, Disconnect, GetDevicePath.
//
// All tests use a temporary directory as a fake sysfs root and a temporary
// file as a fake /dev/nvme-fabrics device, so no kernel NVMe modules or root
// privileges are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNVMeoFConnector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// fakeSysfs creates a minimal /sys/class/nvme-subsystem tree inside dir for
// the given NQN. If addNamespace is true it also creates an nvme0n1 entry.
func fakeSysfs(t *testing.T, nqn string, addNamespace bool) string { //nolint:unparam
	t.Helper()
	root := t.TempDir()
	subsysDir := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0")
	if err := os.MkdirAll(subsysDir, 0o750); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subsysDir, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
		t.Fatalf("write subsysnqn: %v", err)
	}
	if addNamespace {
		nsDir := filepath.Join(subsysDir, "nvme0n1")
		if err := os.MkdirAll(nsDir, 0o750); err != nil {
			t.Fatalf("mkdirall ns: %v", err)
		}
	}
	return root
}

// fakeSysfsWithController creates a sysfs tree that contains a controller
// entry (nvme0) inside the subsystem directory and a corresponding
// class/nvme/nvme0/ directory so that Disconnect can write delete_controller.
// Returns the sysfsRoot and the path to the delete_controller file.
func fakeSysfsWithController(t *testing.T, nqn, ctrlName string) (sysfsRoot, deleteCtrlPath string) {
	t.Helper()
	sysfsRoot = t.TempDir()
	subsysDir := filepath.Join(sysfsRoot, "class", "nvme-subsystem", "nvme-subsys0")
	if err := os.MkdirAll(subsysDir, 0o750); err != nil {
		t.Fatalf("mkdirall subsys: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subsysDir, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
		t.Fatalf("write subsysnqn: %v", err)
	}
	// Controller symlink-like entry inside the subsystem directory.
	ctrlDir := filepath.Join(subsysDir, ctrlName)
	if err := os.MkdirAll(ctrlDir, 0o750); err != nil {
		t.Fatalf("mkdirall ctrl: %v", err)
	}
	// class/nvme/<ctrlName>/ so Disconnect can create delete_controller there.
	nvmeClassDir := filepath.Join(sysfsRoot, "class", "nvme", ctrlName)
	if err := os.MkdirAll(nvmeClassDir, 0o750); err != nil {
		t.Fatalf("mkdirall nvme class: %v", err)
	}
	deleteCtrlPath = filepath.Join(nvmeClassDir, "delete_controller")
	return sysfsRoot, deleteCtrlPath
}

// fakeFabricsDev creates a temporary file that acts as /dev/nvme-fabrics.
// Returns the file path. The file is automatically cleaned up by t.TempDir.
func fakeFabricsDev(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "nvme-fabrics-*")
	if err != nil {
		t.Fatalf("create fake nvme-fabrics: %v", err)
	}
	f.Close() //nolint:errcheck,gosec
	return f.Name()
}

// newConnector constructs a NVMeoFConnector with injectable sysfs root and
// fabricsDev path — no exec command required.
func newConnector(sysfsRoot, fabricsDev string) *NVMeoFConnector {
	return &NVMeoFConnector{
		sysfsRoot:  sysfsRoot,
		fabricsDev: fabricsDev,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Disconnect tests
// ─────────────────────────────────────────────────────────────────────────────

// TestDisconnect_NotConnected_IsNoOp verifies that Disconnect on an NQN that
// has no sysfs entry (not connected) returns nil without touching any file.
func TestDisconnect_NotConnected_IsNoOp(t *testing.T) {
	root := t.TempDir() // empty — no nvme-subsystem entries
	// Create the nvme-subsystem directory so ReadDir doesn't fail with ENOENT.
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}

	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), "nqn.2024-01.com.example:vol1"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestDisconnect_SysfsAbsent_IsNoOp verifies that Disconnect when the whole
// nvme-subsystem directory is missing (no NVMe support in kernel) returns nil
// without error — idempotent disconnect.
func TestDisconnect_SysfsAbsent_IsNoOp(t *testing.T) {
	root := t.TempDir() // no class/nvme-subsystem directory at all
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), "nqn.2024-01.com.example:vol1"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestDisconnect_Connected_DeletesController verifies that Disconnect when
// the NQN is present in sysfs writes "1" to the controller's delete_controller
// sysfs entry (kernel-native teardown, no nvme-cli).
func TestDisconnect_Connected_DeletesController(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root, deleteCtrlPath := fakeSysfsWithController(t, nqn, "nvme0")
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), nqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	got, err := os.ReadFile(deleteCtrlPath) //nolint:gosec
	if err != nil {
		t.Fatalf("delete_controller not written: %v", err)
	}
	if string(got) != "1" {
		t.Fatalf("expected \"1\" written to delete_controller, got %q", string(got))
	}
}

// TestDisconnect_Connected_SkipsNamespaceEntries verifies that namespace
// entries (nvmeXnY pattern) inside the subsystem directory are NOT treated
// as controllers — no delete_controller write should occur for them.
func TestDisconnect_Connected_SkipsNamespaceEntries(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	// Use fakeSysfs which adds nvme0n1 (namespace), not a controller.
	root := fakeSysfs(t, nqn, true /* addNamespace */)
	// Ensure class/nvme/ exists — if a namespace were incorrectly treated as
	// a controller the code would attempt to write there.
	nvmeClassDir := filepath.Join(root, "class", "nvme", "nvme0n1")
	if err := os.MkdirAll(nvmeClassDir, 0o750); err != nil {
		t.Fatal(err)
	}
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), nqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// delete_controller must NOT have been created for the namespace entry.
	deleteCtrlPath := filepath.Join(nvmeClassDir, "delete_controller")
	if _, err := os.Stat(deleteCtrlPath); err == nil {
		t.Fatal("delete_controller must not be written for a namespace (nvme0n1) entry")
	}
}

// TestDisconnect_DifferentNQN_IsNoOp ensures we only disconnect when the
// sysfs NQN exactly matches — a different NQN in sysfs must not trigger
// any delete_controller write.
func TestDisconnect_DifferentNQN_IsNoOp(t *testing.T) {
	const sysnqn = "nqn.2024-01.com.example:vol1"
	const reqnqn = "nqn.2024-01.com.example:vol2"
	root, deleteCtrlPath := fakeSysfsWithController(t, sysnqn, "nvme0")
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), reqnqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// delete_controller must NOT have been written.
	if _, err := os.Stat(deleteCtrlPath); err == nil {
		data, _ := os.ReadFile(deleteCtrlPath) //nolint:errcheck,gosec
		t.Fatalf("delete_controller must not be written for a different NQN; got %q", data)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Connect tests
// ─────────────────────────────────────────────────────────────────────────────

// TestConnect_NotConnected_WritesFabricsDevice verifies that Connect on a new
// NQN opens the fabrics device and writes the correct connect string:
//
//	transport=tcp,traddr=<addr>,trsvcid=<port>,nqn=<nqn>
func TestConnect_NotConnected_WritesFabricsDevice(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}
	fabricsDev := fakeFabricsDev(t)
	c := newConnector(root, fabricsDev)

	const (
		nqn  = "nqn.2024-01.com.example:vol1"
		addr = "192.168.1.10"
		port = "4420"
	)
	if err := c.Connect(context.Background(), nqn, addr, port, NVMeoFConnectOptions{}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	content, err := os.ReadFile(fabricsDev) //nolint:gosec
	if err != nil {
		t.Fatalf("read fabricsDev: %v", err)
	}
	written := strings.TrimRight(string(content), "\n")
	want := "transport=tcp,traddr=" + addr + ",trsvcid=" + port + ",nqn=" + nqn
	if written != want {
		t.Fatalf("fabricsDev content mismatch:\n  want: %q\n  got:  %q", want, written)
	}
}

// TestConnect_AlreadyConnected_IsNoOp verifies that Connect on an NQN whose
// subsystem already has a live controller returns nil without writing to the
// fabrics device.
func TestConnect_AlreadyConnected_IsNoOp(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	addSubsysController(t, root, "nvme0", "live")
	fabricsDev := fakeFabricsDev(t)
	c := newConnector(root, fabricsDev)

	if err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420", NVMeoFConnectOptions{}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// fabricsDev must remain empty (no write occurred).
	content, err := os.ReadFile(fabricsDev) //nolint:gosec
	if err != nil {
		t.Fatalf("read fabricsDev: %v", err)
	}
	if len(content) != 0 {
		t.Fatalf("expected no write to fabricsDev for already-connected NQN, got %q", string(content))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetDevicePath tests
// ─────────────────────────────────────────────────────────────────────────────

// TestGetDevicePath_Found returns the /dev/nvmeXnY path when sysfs has it.
func TestGetDevicePath_Found(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	c := newConnector(root, "")

	path, err := c.GetDevicePath(context.Background(), nqn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/dev/nvme0n1" {
		t.Fatalf("expected /dev/nvme0n1, got %q", path)
	}
}

// TestGetDevicePath_NoNamespace returns ("", nil) when subsystem exists but
// no namespace block device entry is present yet.
func TestGetDevicePath_NoNamespace(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, false) // no namespace dir
	c := newConnector(root, "")

	path, err := c.GetDevicePath(context.Background(), nqn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
}

// TestGetDevicePath_NotConnected returns ("", nil) when there is no subsystem
// entry for the requested NQN.
func TestGetDevicePath_NotConnected(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := newConnector(root, "")

	path, err := c.GetDevicePath(context.Background(), "nqn.2024-01.com.example:vol1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
}

// addSubsysController adds a controller entry named ctrlName to the
// nvme-subsys0 fixture created by fakeSysfs.  A non-empty state is written to
// the controller's sysfs "state" attribute; an empty state leaves it absent.
func addSubsysController(t *testing.T, sysfsRoot, ctrlName, state string) {
	t.Helper()
	ctrlDir := filepath.Join(sysfsRoot, "class", "nvme-subsystem", "nvme-subsys0", ctrlName)
	if err := os.MkdirAll(ctrlDir, 0o750); err != nil {
		t.Fatalf("mkdirall ctrl: %v", err)
	}
	if state == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(ctrlDir, "state"), []byte(state+"\n"), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// TestConnect_SubsystemControllerStates pins the reconnect decision for a
// matching subsystem entry.  After ctrl_loss_tmo the kernel removes every
// controller but can leave the subsystem (and its subsysnqn) behind; Connect
// must then issue a fresh fabrics connect instead of reporting success and
// letting NodeStageVolume time out waiting for a namespace.  A controller the
// kernel is still reconnecting must not be duplicated.
func TestConnect_SubsystemControllerStates(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	cases := []struct {
		name        string
		controllers map[string]string // ctrlName → state ("" = no state file)
		wantConnect bool
	}{
		{name: "empty lingering subsystem", controllers: nil, wantConnect: true},
		{name: "only dead controller", controllers: map[string]string{"nvme0": "dead"}, wantConnect: true},
		{name: "only deleting controller", controllers: map[string]string{"nvme0": "deleting"}, wantConnect: true},
		{name: "deleting no IO controller", controllers: map[string]string{"nvme0": "deleting (no IO)"}, wantConnect: true},
		{name: "live controller", controllers: map[string]string{"nvme0": "live"}, wantConnect: false},
		{name: "connecting controller", controllers: map[string]string{"nvme0": "connecting"}, wantConnect: false},
		{name: "resetting controller", controllers: map[string]string{"nvme0": "resetting"}, wantConnect: false},
		{name: "controller without state attribute", controllers: map[string]string{"nvme0": ""}, wantConnect: false},
		{
			name:        "dead and connecting controllers",
			controllers: map[string]string{"nvme0": "dead", "nvme1": "connecting"},
			wantConnect: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeSysfs(t, nqn, true /* stale namespace entry must not count */)
			for name, state := range tc.controllers {
				addSubsysController(t, root, name, state)
			}
			fabricsDev := fakeFabricsDev(t)
			c := newConnector(root, fabricsDev)

			if err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420", NVMeoFConnectOptions{}); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			content, err := os.ReadFile(fabricsDev) //nolint:gosec
			if err != nil {
				t.Fatalf("read fabricsDev: %v", err)
			}
			if gotConnect := len(content) != 0; gotConnect != tc.wantConnect {
				t.Fatalf("fabrics connect issued = %v, want %v (written %q)", gotConnect, tc.wantConnect, content)
			}
		})
	}
}

// TestConnect_UnreadableControllerState_ReturnsError verifies that a state
// attribute that exists but cannot be read is reported instead of guessing.
func TestConnect_UnreadableControllerState_ReturnsError(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, false)
	addSubsysController(t, root, "nvme0", "")
	// A directory in place of the state file makes ReadFile fail with EISDIR.
	statePath := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0", "nvme0", "state")
	if err := os.MkdirAll(statePath, 0o750); err != nil {
		t.Fatal(err)
	}
	fabricsDev := fakeFabricsDev(t)
	c := newConnector(root, fabricsDev)

	err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420", NVMeoFConnectOptions{})
	if err == nil || !strings.Contains(err.Error(), "read controller state") {
		t.Fatalf("expected controller state read error, got %v", err)
	}
	content, _ := os.ReadFile(fabricsDev) //nolint:gosec,errcheck
	if len(content) != 0 {
		t.Fatalf("no connect must be issued on state read error, got %q", content)
	}
}

// TestConnect_ControllerRemovedDuringScan_Reconnects covers the teardown
// race: the subsystem still lists nvme0, but its sysfs link now dangles
// because the kernel removed the controller.  That must count as no
// controller (fresh connect), not as a live controller lacking a state file.
func TestConnect_ControllerRemovedDuringScan_Reconnects(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, false)
	link := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0", "nvme0")
	if err := os.Symlink(filepath.Join(root, "devices", "gone", "nvme0"), link); err != nil {
		t.Fatal(err)
	}
	fabricsDev := fakeFabricsDev(t)
	c := newConnector(root, fabricsDev)

	if err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420", NVMeoFConnectOptions{}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	content, err := os.ReadFile(fabricsDev) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Fatal("expected a fresh fabrics connect for a removed controller")
	}
}

// TestConnect_FabricsOptions verifies that configured reconnect tuning
// reaches the fabrics connect string verbatim (0 and -1 included) and that
// unset options are omitted so the kernel defaults apply.
func TestConnect_FabricsOptions(t *testing.T) {
	const (
		nqn  = "nqn.2024-01.com.example:vol1"
		base = "transport=tcp,traddr=192.168.1.10,trsvcid=4420,nqn=" + nqn
	)
	i32 := func(v int32) *int32 { return &v }
	cases := []struct {
		name string
		opts NVMeoFConnectOptions
		want string
	}{
		{name: "unset keeps kernel defaults", opts: NVMeoFConnectOptions{}, want: base},
		{
			name: "both set",
			opts: NVMeoFConnectOptions{CtrlLossTmo: i32(1800), ReconnectDelay: i32(5)},
			want: base + ",ctrl_loss_tmo=1800,reconnect_delay=5",
		},
		{name: "zero is explicit", opts: NVMeoFConnectOptions{CtrlLossTmo: i32(0)}, want: base + ",ctrl_loss_tmo=0"},
		{name: "minus one is explicit", opts: NVMeoFConnectOptions{CtrlLossTmo: i32(-1)}, want: base + ",ctrl_loss_tmo=-1"},
		{
			name: "reconnect delay only",
			opts: NVMeoFConnectOptions{ReconnectDelay: i32(15)},
			want: base + ",reconnect_delay=15",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
				t.Fatal(err)
			}
			fabricsDev := fakeFabricsDev(t)
			c := newConnector(root, fabricsDev)
			if err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420", tc.opts); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			content, err := os.ReadFile(fabricsDev) //nolint:gosec
			if err != nil {
				t.Fatalf("read fabricsDev: %v", err)
			}
			if got := strings.TrimRight(string(content), "\n"); got != tc.want {
				t.Fatalf("connect string\n  want: %q\n  got:  %q", tc.want, got)
			}
		})
	}
}

// TestParseNVMeoFConnectOptions covers VolumeContext parsing: absent/empty
// keys stay unset, explicit values (0 and -1 included) are preserved, and a
// malformed value is an error rather than a silent fallback to defaults.
func TestParseNVMeoFConnectOptions(t *testing.T) {
	got, err := ParseNVMeoFConnectOptions(map[string]string{
		paramNVMeOFCtrlLossTmo:    "-1",
		paramNVMeOFReconnectDelay: "5",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.CtrlLossTmo == nil || *got.CtrlLossTmo != -1 {
		t.Errorf("CtrlLossTmo = %v, want -1", got.CtrlLossTmo)
	}
	if got.ReconnectDelay == nil || *got.ReconnectDelay != 5 {
		t.Errorf("ReconnectDelay = %v, want 5", got.ReconnectDelay)
	}

	unset, err := ParseNVMeoFConnectOptions(map[string]string{paramNVMeOFCtrlLossTmo: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unset.CtrlLossTmo != nil || unset.ReconnectDelay != nil {
		t.Errorf("empty/absent keys must stay unset, got %+v", unset)
	}

	for _, bad := range []string{"ten", "1.5", "4294967296"} {
		_, err := ParseNVMeoFConnectOptions(map[string]string{paramNVMeOFReconnectDelay: bad})
		if err == nil || !strings.Contains(err.Error(), paramNVMeOFReconnectDelay) {
			t.Errorf("value %q: expected error naming %s, got %v", bad, paramNVMeOFReconnectDelay, err)
		}
	}
}
