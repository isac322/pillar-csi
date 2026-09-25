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

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	csisvc "github.com/bhyoo/pillar-csi/internal/csi"
)

// TestBuildFabricsConnectOpts_IncludesHostNQNAndHostID locks down the option
// string format written to /dev/nvme-fabrics.  Both hostnqn= and hostid=
// must be present:
//
//   - hostnqn= because pillar-csi targets enforce ACLs by default
//     (attr_allow_any_host=0) and would reject the connect when the kernel
//     synthesizes a random host NQN on its behalf, surfacing EIO to the
//     userland write.
//   - hostid= because Linux ~6.x kernels parse-reject /dev/nvme-fabrics
//     writes that set hostnqn= without a matching hostid= UUID with EINVAL
//     before any TCP attempt; the matching UUID is what nvme-cli writes
//     and what pillar-csi's ReadHostID persists at /etc/nvme/hostid.
//
// A regression that drops either option re-enables every NodeStageVolume
// failure mode the PR #32 round-trips were chasing.  This regression test
// is comment-heavy because the failure modes (EIO, EINVAL) are kernel-side
// and not surfaced as Go errors anywhere a future contributor could see.
func TestBuildFabricsConnectOpts_IncludesHostNQNAndHostID(t *testing.T) {
	const (
		trAddr    = "10.0.0.7"
		trSvcID   = "4420"
		subsysNQN = "nqn.2026-01.io.pillar-csi:pvc-test"
		hostNQN   = "nqn.2014-08.org.nvmexpress:uuid:abc-host"
		hostID    = "11111111-2222-3333-4444-555555555555"
	)
	got := buildFabricsConnectOpts(trAddr, trSvcID, subsysNQN, hostNQN, hostID, csisvc.NVMeoFConnectOptions{})

	for _, want := range []string{
		"transport=tcp",
		"traddr=" + trAddr,
		"trsvcid=" + trSvcID,
		"nqn=" + subsysNQN,
		"hostnqn=" + hostNQN,
		"hostid=" + hostID,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("option string missing %q\nfull string: %q", want, got)
		}
	}
}

// TestBuildFabricsConnectOpts_NoTrailingNewline guards the contract that
// the caller (nvmeConnect) appends the framing newline itself via
// fmt.Fprintf "%s\n".  The kernel nvmf_dev_write parser tolerates a
// trailing newline but treats it as part of the last value when the
// builder emits one on its own, producing hostid=<uuid>\n which the UUID
// parser then rejects.
func TestBuildFabricsConnectOpts_NoTrailingNewline(t *testing.T) {
	got := buildFabricsConnectOpts("a", "1", "n", "h", "i", csisvc.NVMeoFConnectOptions{})
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("option string must not include CR/LF; got %q", got)
	}
}

// newTestFabricsConnector builds a fabricsConnector over a fake sysfs tree
// containing one subsystem for nqn with the given controllers (name → state;
// "" writes no state attribute) and a regular file standing in for
// /dev/nvme-fabrics.
func newTestFabricsConnector(
	t *testing.T, nqn string, controllers map[string]string,
) (conn *fabricsConnector, fabricsDev string) {
	t.Helper()
	root := t.TempDir()
	subsys := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0")
	if err := os.MkdirAll(filepath.Join(subsys, "nvme0n1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subsys, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, state := range controllers {
		if err := os.MkdirAll(filepath.Join(subsys, name), 0o750); err != nil {
			t.Fatal(err)
		}
		if state != "" {
			if err := os.WriteFile(filepath.Join(subsys, name, "state"), []byte(state+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	fabricsDev = filepath.Join(t.TempDir(), "nvme-fabrics")
	if err := os.WriteFile(fabricsDev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	conn = &fabricsConnector{sysfsRoot: root, fabricsDev: fabricsDev, hostNQN: "h", hostID: "i"}
	return conn, fabricsDev
}

// TestFabricsConnectorNvmeConnect_ControllerStates is the production-path
// regression for issue #85: once ctrl_loss_tmo removed every controller the
// lingering subsystem made nvmeConnect skip the connect, so restage waited
// 30 s for a namespace that never appeared.  A dead-only subsystem must
// reconnect; a live or reconnecting controller must not be duplicated.
func TestFabricsConnectorNvmeConnect_ControllerStates(t *testing.T) {
	const nqn = "nqn.2026-01.io.pillar-csi:pvc-test"
	cases := []struct {
		name        string
		controllers map[string]string
		wantConnect bool
	}{
		{name: "empty lingering subsystem", wantConnect: true},
		{name: "dead controller", controllers: map[string]string{"nvme0": "dead"}, wantConnect: true},
		{name: "live controller", controllers: map[string]string{"nvme0": "live"}, wantConnect: false},
		{name: "connecting controller", controllers: map[string]string{"nvme0": "connecting"}, wantConnect: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fabricsDev := newTestFabricsConnector(t, nqn, tc.controllers)
			err := c.nvmeConnect(context.Background(), nqn, "10.0.0.7", "4420", csisvc.NVMeoFConnectOptions{})
			if err != nil {
				t.Fatalf("nvmeConnect: %v", err)
			}
			content, err := os.ReadFile(fabricsDev) //nolint:gosec
			if err != nil {
				t.Fatal(err)
			}
			if gotConnect := len(content) != 0; gotConnect != tc.wantConnect {
				t.Fatalf("fabrics connect issued = %v, want %v (written %q)", gotConnect, tc.wantConnect, content)
			}
		})
	}
}

// TestFabricsConnectorAttach_ForwardsReconnectTuning verifies the production
// node appends VolumeContext ctrl_loss_tmo / reconnect_delay to the connect
// string, and omits them when absent so kernel defaults stay in force.
func TestFabricsConnectorAttach_ForwardsReconnectTuning(t *testing.T) {
	const nqn = "nqn.2026-01.io.pillar-csi:pvc-test"
	base := "transport=tcp,traddr=10.0.0.7,trsvcid=4420,nqn=" + nqn + ",hostnqn=h,hostid=i"
	cases := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{name: "absent keeps kernel defaults", extra: map[string]string{}, want: base},
		{
			name: "configured values",
			extra: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo":   "0",
				"pillar-csi.bhyoo.com/nvmeof-reconnect-delay": "5",
			},
			want: base + ",ctrl_loss_tmo=0,reconnect_delay=5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Empty nvme-subsystem class: Connect is issued, and the device
			// poll only sees "NQN not in sysfs" (no /dev mknod, no nvme-cli).
			// The canceled context ends the poll after the first scan.
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
				t.Fatal(err)
			}
			fabricsDev := filepath.Join(t.TempDir(), "nvme-fabrics")
			if err := os.WriteFile(fabricsDev, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			c := &fabricsConnector{sysfsRoot: root, fabricsDev: fabricsDev, hostNQN: "h", hostID: "i"}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _ = c.Attach(ctx, csisvc.AttachParams{ //nolint:errcheck // device never appears; only the write matters
				ProtocolType: csisvc.ProtocolNVMeoFTCP,
				ConnectionID: nqn,
				Address:      "10.0.0.7",
				Port:         "4420",
				Extra:        tc.extra,
			})
			content, err := os.ReadFile(fabricsDev) //nolint:gosec
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimRight(string(content), "\n"); got != tc.want {
				t.Fatalf("connect string\n  want: %q\n  got:  %q", tc.want, got)
			}
		})
	}
}

func TestMknodFromSysfsDevReplacesStaleDeviceNode(t *testing.T) {
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme0n1")
	devFile := filepath.Join(dir, "dev")
	if err := os.WriteFile(devPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale device node: %v", err)
	}
	if err := os.WriteFile(devFile, []byte("259:7\n"), 0o600); err != nil {
		t.Fatalf("seed sysfs dev: %v", err)
	}

	var gotPath string
	var gotMode uint32
	var gotDev int
	fakeMknod := func(path string, mode uint32, dev int) error {
		gotPath = path
		gotMode = mode
		gotDev = dev
		return nil
	}
	got, err := mknodFromSysfsDevWith(devPath, devFile, fakeMknod)
	if err != nil {
		t.Fatalf("mknodFromSysfsDevWith: %v", err)
	}
	if got != devPath {
		t.Errorf("device path = %q, want %q", got, devPath)
	}
	if _, statErr := os.Stat(devPath); !os.IsNotExist(statErr) {
		t.Errorf("stale device path still exists after replacement: %v", statErr)
	}
	wantDev := (7 & 0xff) | ((259 & 0xfff) << 8)
	if gotPath != devPath || gotMode != syscall.S_IFBLK|0o600 || gotDev != wantDev {
		t.Errorf(
			"mknod call = (%q, %#o, %d), want (%q, %#o, %d)",
			gotPath,
			gotMode,
			gotDev,
			devPath,
			uint32(syscall.S_IFBLK|0o600),
			wantDev,
		)
	}
}

func TestMknodFromSysfsDevKeepsMatchingDeviceNode(t *testing.T) {
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme0n1")
	devFile := filepath.Join(dir, "dev")
	if err := os.WriteFile(devPath, []byte("live"), 0o600); err != nil {
		t.Fatalf("seed matching device node: %v", err)
	}
	if err := os.WriteFile(devFile, []byte("0:0\n"), 0o600); err != nil {
		t.Fatalf("seed matching sysfs dev: %v", err)
	}

	calls := 0
	fakeMknod := func(string, uint32, int) error {
		calls++
		return os.ErrExist
	}
	got, err := mknodFromSysfsDevWith(devPath, devFile, fakeMknod)
	if err != nil {
		t.Fatalf("mknodFromSysfsDevWith: %v", err)
	}
	if got != devPath {
		t.Errorf("device path = %q, want %q", got, devPath)
	}
	if calls != 1 {
		t.Errorf("mknod calls = %d, want 1", calls)
	}
	data, readErr := os.ReadFile(devPath) //nolint:gosec // temp-dir path controlled by this test.
	if readErr != nil {
		t.Fatalf("matching device node was unlinked: %v", readErr)
	}
	if string(data) != "live" {
		t.Errorf("matching device node content = %q, want live", data)
	}
}
