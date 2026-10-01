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

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const mdtsTestNQN = "nqn.2024-01.com.example:mdts"

// mdtsSysfs builds a connected multipath subsystem: live controller nvme0,
// head namespace nvme0n1 and path device nvme0c0n1, each with a
// queue/max_sectors_kb of 32768 and a max_hw_sectors_kb of hwKB ("" omits
// the attribute).
func mdtsSysfs(t *testing.T, hwKB string) string {
	t.Helper()
	root := fakeSysfs(t, mdtsTestNQN, true)
	addSubsysController(t, root, "nvme0", "live")
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme", "nvme0", "nvme0c0n1"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, dev := range []string{"nvme0n1", "nvme0c0n1"} {
		queue := filepath.Join(root, "block", dev, "queue")
		if err := os.MkdirAll(queue, 0o750); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(queue, "max_sectors_kb"), "32768\n")
		if hwKB != "" {
			writeTestFile(t, filepath.Join(queue, "max_hw_sectors_kb"), hwKB+"\n")
		}
	}
	return root
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func maxSectorsKB(t *testing.T, root, dev string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, "block", dev, "queue", "max_sectors_kb")) //nolint:gosec // temp dir
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(got))
}

func attachMDTS(h *NVMeoFTCPHandler, extra map[string]string) error {
	_, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: ProtocolNVMeoFTCP,
		ConnectionID: mdtsTestNQN,
		Address:      "192.168.1.10",
		Port:         "4420",
		Extra:        extra,
	})
	return err
}

// A target without MDTS (Linux nvmet before 7.1) gets the configured size,
// the 4 MiB default when the VolumeContext predates the key, on both the
// multipath head and the path device; a hardware limit below it wins
// because the kernel rejects max_sectors_kb above max_hw_sectors_kb; a
// target advertising MDTS and an explicit "no limit" leave the queue alone.
func TestNVMeoFTCPHandler_Attach_MaxDataTransferSize(t *testing.T) {
	cases := []struct {
		name  string
		mdts  uint8
		hwKB  string
		extra map[string]string
		want  string
	}{
		{name: "no MDTS, key absent", hwKB: "32768", want: "4096"},
		{
			name:  "no MDTS, explicit 1 MiB",
			extra: map[string]string{paramNVMeOFMaxDataTransferSize: "1048576"},
			want:  "1024",
		},
		{name: "no MDTS, smaller hardware limit", hwKB: "512", want: "512"},
		{name: "MDTS advertised", mdts: 5, want: "32768"},
		{
			name:  "no limit configured",
			extra: map[string]string{paramNVMeOFMaxDataTransferSize: "0"},
			want:  "32768",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := mdtsSysfs(t, tc.hwKB)
			h := newTestHandler(root, fakeFabricsDev(t))
			h.readMDTS = func(string) (uint8, error) { return tc.mdts, nil }

			if err := attachMDTS(h, tc.extra); err != nil {
				t.Fatalf("Attach: %v", err)
			}
			for _, dev := range []string{"nvme0n1", "nvme0c0n1"} {
				if got := maxSectorsKB(t, root, dev); got != tc.want {
					t.Errorf("%s max_sectors_kb = %q, want %q", dev, got, tc.want)
				}
			}
		})
	}
}

// Only live controllers can answer Identify; a reconnecting one must not
// fail the stage, and its MDTS must not decide the cap.
func TestNVMeoFTCPHandler_Attach_MDTSQueriesLiveControllersOnly(t *testing.T) {
	root := mdtsSysfs(t, "")
	addSubsysController(t, root, "nvme1", "connecting")
	h := newTestHandler(root, fakeFabricsDev(t))
	var queried []string
	h.readMDTS = func(ctrl string) (uint8, error) {
		queried = append(queried, ctrl)
		if ctrl != "nvme0" {
			return 0, errors.New("not live")
		}
		return 0, nil
	}

	if err := attachMDTS(h, nil); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !slices.Equal(queried, []string{"nvme0"}) {
		t.Errorf("queried controllers = %v, want [nvme0]", queried)
	}
	if got := maxSectorsKB(t, root, "nvme0n1"); got != "4096" {
		t.Errorf("nvme0n1 max_sectors_kb = %q, want 4096", got)
	}
}

// The cap cannot be decided, or did not take effect: NodeStage must fail
// rather than leave the device sending commands the target cannot allocate.
func TestNVMeoFTCPHandler_Attach_MDTSFailures(t *testing.T) {
	t.Run("MDTS read error", func(t *testing.T) {
		root := mdtsSysfs(t, "")
		h := newTestHandler(root, fakeFabricsDev(t))
		sentinel := errors.New("identify failed")
		h.readMDTS = func(string) (uint8, error) { return 0, sentinel }

		err := attachMDTS(h, nil)
		if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), mdtsTestNQN) {
			t.Fatalf("Attach error = %v, want %v naming %s", err, sentinel, mdtsTestNQN)
		}
	})

	t.Run("no live controller", func(t *testing.T) {
		root := fakeSysfs(t, mdtsTestNQN, true)
		addSubsysController(t, root, "nvme0", "connecting")
		h := newTestHandler(root, fakeFabricsDev(t))
		h.readMDTS = func(string) (uint8, error) { return 0, nil }

		if err := attachMDTS(h, nil); err == nil || !strings.Contains(err.Error(), "no live controller") {
			t.Fatalf("Attach error = %v, want no live controller", err)
		}
	})

	t.Run("read-back mismatch", func(t *testing.T) {
		root := mdtsSysfs(t, "")
		attr := filepath.Join(root, "block", "nvme0n1", "queue", "max_sectors_kb")
		if err := os.Remove(attr); err != nil {
			t.Fatal(err)
		}
		// Writes to /dev/null succeed but read back empty, like a sysfs
		// attribute that silently ignored the value.
		if err := os.Symlink(os.DevNull, attr); err != nil {
			t.Fatal(err)
		}
		h := newTestHandler(root, fakeFabricsDev(t))
		h.readMDTS = func(string) (uint8, error) { return 0, nil }

		err := attachMDTS(h, nil)
		if err == nil || !strings.Contains(err.Error(), "nvme0n1") || !strings.Contains(err.Error(), `"4096"`) {
			t.Fatalf("Attach error = %v, want read-back mismatch naming nvme0n1 and 4096", err)
		}
	})
}

// A malformed or out-of-domain size fails before any fabrics connect, like
// the other VolumeContext tuning keys.
func TestNVMeoFTCPHandler_Attach_InvalidMaxDataTransferSize_NoConnect(t *testing.T) {
	for _, raw := range []string{"abc", "5000", "4096", "-8192", "4294967296"} {
		t.Run(raw, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
				t.Fatal(err)
			}
			fabricsDev := fakeFabricsDev(t)
			h := newTestHandler(root, fabricsDev)

			err := attachMDTS(h, map[string]string{paramNVMeOFMaxDataTransferSize: raw})
			if err == nil || !strings.Contains(err.Error(), paramNVMeOFMaxDataTransferSize) {
				t.Fatalf("Attach error = %v, want one naming %s", err, paramNVMeOFMaxDataTransferSize)
			}
			content, _ := os.ReadFile(fabricsDev) //nolint:gosec,errcheck // temp file
			if len(content) != 0 {
				t.Fatalf("no connect must be issued, got %q", content)
			}
		})
	}
}
