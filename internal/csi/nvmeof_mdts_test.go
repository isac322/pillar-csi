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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// mdtsNodeServer returns a NodeServer whose NVMe-oF handler works on the
// fake sysfs root and reads controller MDTS with readMDTS, with a temporary
// state directory, and the fabrics device that records connects.
func mdtsNodeServer(t *testing.T, root string, readMDTS NVMeMDTSReader) (*NodeServer, *mockMounter, string) {
	t.Helper()
	fabricsDev := fakeFabricsDev(t)
	h := newTestHandler(root, fabricsDev)
	h.readMDTS = readMDTS
	mnt := newMockMounter()
	srv := NewNodeServer("test-node", map[string]ProtocolHandler{ProtocolNVMeoFTCP: h}, mnt)
	srv.stateDir = t.TempDir()
	return srv, mnt, fabricsDev
}

// legacyNVMeoFStageState is a record written by a release that did not
// persist the transfer limit.
func legacyNVMeoFStageState(volumeID, nqn, stagingPath string) *nodeStageState {
	s := stageStateFromAttachResult(ProtocolNVMeoFTCP, AccessTypeFilesystem, nqn, "192.168.1.10", "4420", nil)
	s.VolumeID = volumeID
	s.StagingPath = stagingPath
	return s
}

func mdtsVolumeContext(size string) map[string]string {
	volCtx := map[string]string{
		VolumeContextKeyTargetID: mdtsTestNQN,
		VolumeContextKeyAddress:  "192.168.1.10",
		VolumeContextKeyPort:     "4420",
	}
	if size != "" {
		volCtx[paramNVMeOFMaxDataTransferSize] = size
	}
	return volCtx
}

func requireNoConnect(t *testing.T, fabricsDev string) {
	t.Helper()
	content, _ := os.ReadFile(fabricsDev) //nolint:gosec,errcheck // temp file
	if len(content) != 0 {
		t.Fatalf("no connect must be issued, got %q", content)
	}
}

// A volume staged by a release without the limit stays mounted across the
// upgrade; a repeated NodeStageVolume returns early without Attach, so it
// must cap the connected devices itself (only when the target advertises
// no MDTS), record the limit, and fail when the cap cannot be applied.
func TestNodeStageVolume_AlreadyStagedNVMeoF_CapsTransferSize(t *testing.T) {
	const volumeID = "tank/pvc-staged"
	cases := []struct {
		name     string
		readMDTS NVMeMDTSReader
		size     string
		want     string
		wantSize int32
		wantCode codes.Code
	}{
		{name: "no MDTS, key absent", readMDTS: fixedMDTS(0), want: "4096", wantSize: 4 << 20},
		{name: "no MDTS, explicit 1 MiB", readMDTS: fixedMDTS(0), size: "1048576", want: "1024", wantSize: 1 << 20},
		{name: "MDTS advertised", readMDTS: fixedMDTS(5), want: "32768", wantSize: 4 << 20},
		{
			name:     "MDTS read fails",
			readMDTS: func(string) (uint8, error) { return 0, errors.New("identify failed") },
			want:     "32768",
			wantCode: codes.Internal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := mdtsSysfs(t, "")
			srv, mnt, fabricsDev := mdtsNodeServer(t, root, tc.readMDTS)
			stagingPath := t.TempDir()
			mnt.mountedPaths[stagingPath] = true
			err := srv.writeStageState(volumeID, legacyNVMeoFStageState(volumeID, mdtsTestNQN, stagingPath))
			if err != nil {
				t.Fatal(err)
			}

			_, err = srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          volumeID,
				StagingTargetPath: stagingPath,
				VolumeCapability:  mountCap("ext4"),
				VolumeContext:     mdtsVolumeContext(tc.size),
			})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("NodeStageVolume error = %v, want code %v", err, tc.wantCode)
			}
			requireNoConnect(t, fabricsDev)
			if len(mnt.formatAndMountCalls) != 0 {
				t.Errorf("FormatAndMount called %d times on an already-staged volume", len(mnt.formatAndMountCalls))
			}
			requireMaxSectorsKB(t, root, map[string]string{"nvme0n1": tc.want, "nvme0c0n1": tc.want})
			if tc.wantCode == codes.OK {
				requireRecordedTransferSize(t, srv, volumeID, tc.wantSize)
			}
		})
	}
}

func fixedMDTS(mdts uint8) NVMeMDTSReader {
	return func(string) (uint8, error) { return mdts, nil }
}

func requireMaxSectorsKB(t *testing.T, root string, want map[string]string) {
	t.Helper()
	for dev, w := range want {
		if got := maxSectorsKB(t, root, dev); got != w {
			t.Errorf("%s max_sectors_kb = %q, want %q", dev, got, w)
		}
	}
}

func requireRecordedTransferSize(t *testing.T, srv *NodeServer, volumeID string, want int32) {
	t.Helper()
	state, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.NVMeoF.MaxDataTransferSize; got == nil || *got != want {
		t.Errorf("recorded max data transfer size = %v, want %d", got, want)
	}
}

// A fresh stage records the limit it applied so the startup reconcile can
// re-apply it.
func TestNodeStageVolume_NVMeoF_RecordsTransferSize(t *testing.T) {
	const volumeID = "tank/pvc-new"
	root := mdtsSysfs(t, "")
	srv, _, _ := mdtsNodeServer(t, root, fixedMDTS(0))
	_, err := srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mdtsVolumeContext("2097152"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	requireRecordedTransferSize(t, srv, volumeID, 2097152)
	requireMaxSectorsKB(t, root, map[string]string{"nvme0n1": "2048", "nvme0c0n1": "2048"})
}

// decodeLogLines parses the JSON lines slog wrote to buf.
func decodeLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// pillar-node startup caps the devices of NVMe-oF volumes that stayed
// staged across the upgrade: a record without the persisted limit (both
// discriminated-union and Phase 1 formats) gets the 4 MiB default, a
// persisted limit wins, a target advertising MDTS, a subsystem that is no
// longer connected and a local attach are left alone, and a device that
// cannot be capped is logged at error level with the volume, device and
// cause while the other devices are still capped.
func TestReconcileNVMeoFTransferLimits(t *testing.T) {
	const volumeID = "tank/pvc-upgrade"
	oneMiB := int32(1 << 20)
	legacy := func(nqn string) func(srv *NodeServer) error {
		return func(srv *NodeServer) error {
			return srv.writeStageState(volumeID, legacyNVMeoFStageState(volumeID, nqn, "/staging"))
		}
	}
	untouched := map[string]string{"nvme0n1": "32768", "nvme0c0n1": "32768"}
	cases := []struct {
		name       string
		mdts       uint8
		record     func(srv *NodeServer) error
		breakDev   string
		want       map[string]string
		wantErrDev string
	}{
		{
			name:   "legacy record, no MDTS",
			record: legacy(mdtsTestNQN),
			want:   map[string]string{"nvme0n1": "4096", "nvme0c0n1": "4096"},
		},
		{
			name: "phase 1 record, no MDTS",
			record: func(srv *NodeServer) error {
				return os.WriteFile(filepath.Join(srv.stateDir, "pvc-phase1.json"),
					[]byte(`{"subsys_nqn":"`+mdtsTestNQN+`"}`), 0o600)
			},
			want: map[string]string{"nvme0n1": "4096", "nvme0c0n1": "4096"},
		},
		{
			name: "persisted limit",
			record: func(srv *NodeServer) error {
				s := legacyNVMeoFStageState(volumeID, mdtsTestNQN, "/staging")
				s.NVMeoF.MaxDataTransferSize = &oneMiB
				return srv.writeStageState(volumeID, s)
			},
			want: map[string]string{"nvme0n1": "1024", "nvme0c0n1": "1024"},
		},
		{name: "MDTS advertised", mdts: 5, record: legacy(mdtsTestNQN), want: untouched},
		{name: "subsystem not connected", record: legacy("nqn.2024-01.com.example:gone"), want: untouched},
		{
			name: "local attach",
			record: func(srv *NodeServer) error {
				return srv.writeStageState(volumeID,
					localStageState(ProtocolNVMeoFTCP, AccessTypeFilesystem, "pillar-x", "/dev/zvol/x"))
			},
			want: untouched,
		},
		{
			name:       "one device cannot be capped",
			record:     legacy(mdtsTestNQN),
			breakDev:   "nvme0c0n1",
			want:       map[string]string{"nvme0n1": "4096"},
			wantErrDev: "nvme0c0n1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := mdtsSysfs(t, "")
			if tc.breakDev != "" {
				breakMaxSectorsKB(t, root, tc.breakDev)
			}
			srv, _, _ := mdtsNodeServer(t, root, fixedMDTS(tc.mdts))
			if err := tc.record(srv); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer

			srv.ReconcileNVMeoFTransferLimits(slog.New(slog.NewJSONHandler(&buf, nil)))

			requireMaxSectorsKB(t, root, tc.want)
			requireReconcileLog(t, decodeLogLines(t, &buf), volumeID, tc.wantErrDev)
		})
	}
}

// breakMaxSectorsKB replaces queue/max_sectors_kb of dev with a directory,
// so reading it fails.
func breakMaxSectorsKB(t *testing.T, root, dev string) {
	t.Helper()
	attr := filepath.Join(root, "block", dev, "queue", "max_sectors_kb")
	if err := os.Remove(attr); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(attr, 0o750); err != nil {
		t.Fatal(err)
	}
}

// requireReconcileLog checks that the reconcile logged nothing, or exactly
// one error naming volumeID, errDev and a cause mentioning errDev.
func requireReconcileLog(t *testing.T, lines []map[string]any, volumeID, errDev string) {
	t.Helper()
	if errDev == "" {
		if len(lines) != 0 {
			t.Fatalf("unexpected log lines: %v", lines)
		}
		return
	}
	if len(lines) != 1 {
		t.Fatalf("log lines = %v, want one error", lines)
	}
	l := lines[0]
	if l["level"] != "ERROR" || l["volume"] != volumeID || l["device"] != errDev ||
		!strings.Contains(fmt.Sprint(l["error"]), errDev) {
		t.Errorf("log line = %v, want ERROR for volume %q device %q with its cause", l, volumeID, errDev)
	}
}

// The kernel refuses queue/max_sectors_kb below one page, so a limit
// smaller than the worker's page size (16 KiB or 64 KiB pages on arm64)
// fails NodeStage with InvalidArgument before connecting instead of being
// rounded up; a limit of at least one page, or no limit, is accepted.
func TestNodeStageVolume_NVMeoFTransferSizeBelowPageSize_Rejected(t *testing.T) {
	cases := []struct {
		size     string
		pageSize int
	}{
		{size: "8192", pageSize: 16384},
		{size: "32768", pageSize: 65536},
	}
	for _, tc := range cases {
		t.Run(tc.size, func(t *testing.T) {
			srv, mnt, fabricsDev := mdtsNodeServer(t, mdtsSysfs(t, ""), fixedMDTS(0))
			srv.pageSize = tc.pageSize

			_, err := srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          "tank/pvc-page",
				StagingTargetPath: t.TempDir(),
				VolumeCapability:  mountCap("ext4"),
				VolumeContext:     mdtsVolumeContext(tc.size),
			})
			requireGRPCCode(t, err, codes.InvalidArgument)
			msg := status.Convert(err).Message()
			if !strings.Contains(msg, tc.size) || !strings.Contains(msg, strconv.Itoa(tc.pageSize)) {
				t.Errorf("message %q must name the size %s and the page size %d", msg, tc.size, tc.pageSize)
			}
			requireNoConnect(t, fabricsDev)
			if len(mnt.formatAndMountCalls) != 0 {
				t.Error("FormatAndMount must not run")
			}
		})
	}

	for _, tc := range []struct {
		size     int32
		pageSize int
	}{{0, 65536}, {8192, 4096}, {16384, 16384}} {
		if err := CheckNVMeoFTransferSizePageSize(tc.size, tc.pageSize); err != nil {
			t.Errorf("CheckNVMeoFTransferSizePageSize(%d, %d) = %v, want accepted", tc.size, tc.pageSize, err)
		}
	}
}
