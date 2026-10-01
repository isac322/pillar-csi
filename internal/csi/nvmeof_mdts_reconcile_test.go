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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const reconcileTestVolume = "tank/pvc-reconcile"

// reconcileTestServer returns a NodeServer with one staged NVMe-oF volume
// (4 MiB default limit) on the mdtsSysfs fixture (r.sysfsRoot), and a
// reconciler logging to buf whose MDTS reads go to readMDTS.
func reconcileTestServer(
	t *testing.T, readMDTS NVMeMDTSReader,
) (srv *NodeServer, r *transferLimitReconciler, buf *bytes.Buffer) {
	t.Helper()
	srv, _, _ = mdtsNodeServer(t, mdtsSysfs(t, ""), readMDTS)
	stageReconcileTestVolume(t, srv)
	buf = &bytes.Buffer{}
	r = srv.newTransferLimitReconciler(slog.New(slog.NewJSONHandler(buf, nil)), t.TempDir())
	if r == nil {
		t.Fatal("NVMe-oF handler cannot cap a connected subsystem")
	}
	return srv, r, buf
}

func stageReconcileTestVolume(t *testing.T, srv *NodeServer) {
	t.Helper()
	err := srv.writeStageState(reconcileTestVolume,
		legacyNVMeoFStageState(reconcileTestVolume, mdtsTestNQN, "/staging"))
	if err != nil {
		t.Fatal(err)
	}
}

// countingMDTS returns an MDTS reader reporting mdts and the number of
// reads it served.
func countingMDTS(mdts uint8) (NVMeMDTSReader, *atomic.Int32) {
	var calls atomic.Int32
	return func(string) (uint8, error) {
		calls.Add(1)
		return mdts, nil
	}, &calls
}

var (
	reconcileUncapped = map[string]string{"nvme0n1": "32768", "nvme0c0n1": "32768"}
	reconcileCapped   = map[string]string{"nvme0n1": "4096", "nvme0c0n1": "4096"}
)

// Starting the reconciler returns while its first pass is still blocked
// in an Identify Controller command: pillar-node serves CSI calls before
// any target answers.  The pass then completes in the background.
func TestStartNVMeoFTransferLimitReconciler_DoesNotWaitForFirstPass(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	readMDTS := func(string) (uint8, error) {
		close(entered)
		<-release
		return 0, nil
	}
	root := mdtsSysfs(t, "")
	srv, _, _ := mdtsNodeServer(t, root, readMDTS)
	stageReconcileTestVolume(t, srv)

	stop := srv.StartNVMeoFTransferLimitReconciler(context.Background(), "pillar-csi.bhyoo.com",
		slog.New(slog.DiscardHandler))

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first pass never read the MDTS")
	}
	requireMaxSectorsKB(t, root, reconcileUncapped)
	close(release)
	stop()
	requireMaxSectorsKB(t, root, reconcileCapped)
}

// The loop runs a pass at start and one per tick of the injected ticker
// (a failed MDTS read is retried on every pass), until it is stopped.
func TestTransferLimitReconciler_PassPerTick(t *testing.T) {
	reads := make(chan struct{})
	readMDTS := func(string) (uint8, error) {
		reads <- struct{}{}
		return 0, errors.New("identify failed")
	}
	_, r, _ := reconcileTestServer(t, readMDTS)
	root := r.sysfsRoot
	ticks := make(chan time.Time)
	stop := r.start(context.Background(), ticks)

	<-reads              // the first pass runs without a tick
	ticks <- time.Time{} // accepted once the first pass is done
	<-reads              // the tick ran a second pass
	stop()

	requireMaxSectorsKB(t, root, reconcileUncapped)
}

// A volume whose controller is reconnecting is left pending without an
// admin command, logged once, and capped by a later pass once the
// controller is live again.
func TestTransferLimitReconciler_CapsWhenControllerBecomesLive(t *testing.T) {
	readMDTS, calls := countingMDTS(0)
	_, r, buf := reconcileTestServer(t, readMDTS)
	root := r.sysfsRoot
	addSubsysController(t, root, "nvme0", "connecting")
	ctx := context.Background()

	r.pass(ctx)
	r.pass(ctx)

	requireMaxSectorsKB(t, root, reconcileUncapped)
	if got := calls.Load(); got != 0 {
		t.Errorf("MDTS reads while connecting = %d, want 0", got)
	}
	lines := decodeLogLines(t, buf)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["reason"] != "no live controller" ||
		lines[0]["volume"] != reconcileTestVolume {
		t.Fatalf("log lines = %v, want one INFO pending line for %q", lines, reconcileTestVolume)
	}

	addSubsysController(t, root, "nvme0", "live")
	buf.Reset()
	r.pass(ctx)

	requireMaxSectorsKB(t, root, reconcileCapped)
	lines = decodeLogLines(t, buf)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["volume"] != reconcileTestVolume {
		t.Fatalf("log lines = %v, want one INFO line announcing the cap", lines)
	}
}

// A device already at or below the limit costs no admin command, and a
// subsystem whose target advertises MDTS is not queried again while its
// live controllers stay the same.
func TestTransferLimitReconciler_NoIoctlWhenSettled(t *testing.T) {
	t.Run("already capped", func(t *testing.T) {
		readMDTS, calls := countingMDTS(0)
		_, r, buf := reconcileTestServer(t, readMDTS)
		root := r.sysfsRoot
		writeTestFile(t, filepath.Join(root, "block", "nvme0n1", "queue", "max_sectors_kb"), "4096\n")
		writeTestFile(t, filepath.Join(root, "block", "nvme0c0n1", "queue", "max_sectors_kb"), "512\n")

		r.pass(context.Background())
		r.pass(context.Background())

		if got := calls.Load(); got != 0 {
			t.Errorf("MDTS reads = %d, want 0", got)
		}
		requireMaxSectorsKB(t, root, map[string]string{"nvme0n1": "4096", "nvme0c0n1": "512"})
		if buf.Len() != 0 {
			t.Errorf("unexpected log output: %s", buf)
		}
	})
	t.Run("capped by an earlier pass", func(t *testing.T) {
		readMDTS, calls := countingMDTS(0)
		_, r, _ := reconcileTestServer(t, readMDTS)
		root := r.sysfsRoot

		r.pass(context.Background())
		r.pass(context.Background())

		if got := calls.Load(); got != 1 {
			t.Errorf("MDTS reads = %d, want 1", got)
		}
		requireMaxSectorsKB(t, root, reconcileCapped)
	})
	t.Run("MDTS advertised", func(t *testing.T) {
		readMDTS, calls := countingMDTS(5)
		_, r, _ := reconcileTestServer(t, readMDTS)
		root := r.sysfsRoot

		r.pass(context.Background())
		r.pass(context.Background())

		if got := calls.Load(); got != 1 {
			t.Errorf("MDTS reads = %d, want 1", got)
		}
		requireMaxSectorsKB(t, root, reconcileUncapped)
	})
}

// An unchanged failure is logged once; once the volume is unstaged its
// state is dropped, so a later stage that fails the same way is logged
// again.
func TestTransferLimitReconciler_LogsOnceAndDropsUnstaged(t *testing.T) {
	readMDTS, _ := countingMDTS(0)
	srv, r, buf := reconcileTestServer(t, readMDTS)
	root := r.sysfsRoot
	breakMaxSectorsKB(t, root, "nvme0c0n1")
	ctx := context.Background()

	r.pass(ctx)
	r.pass(ctx)
	requireReconcileLog(t, decodeLogLines(t, buf), reconcileTestVolume, "nvme0c0n1")

	if err := os.Remove(srv.stateFilePath(reconcileTestVolume)); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	r.pass(ctx)
	if len(r.reported) != 0 || len(r.advertised) != 0 {
		t.Fatalf("state of the unstaged volume kept: reported=%v advertised=%v", r.reported, r.advertised)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log output for an unstaged volume: %s", buf)
	}

	stageReconcileTestVolume(t, srv)
	r.pass(ctx)
	requireReconcileLog(t, decodeLogLines(t, buf), reconcileTestVolume, "nvme0c0n1")
}

// The reconciler never caps a volume while NodeStageVolume or
// NodeUnstageVolume holds its lock, and it does not hold the lock while
// it waits for Identify Controller.
func TestTransferLimitReconciler_VolumeLock(t *testing.T) {
	var srv *NodeServer
	var lockFreeDuringRead atomic.Bool
	readMDTS := func(string) (uint8, error) {
		unlock, ok := srv.volumeLocks.tryLock(reconcileTestVolume)
		if ok {
			unlock()
		}
		lockFreeDuringRead.Store(ok)
		return 0, nil
	}
	srv, r, _ := reconcileTestServer(t, readMDTS)
	root := r.sysfsRoot
	ctx := context.Background()

	unlock := srv.volumeLocks.lock(reconcileTestVolume)
	r.pass(ctx)
	requireMaxSectorsKB(t, root, reconcileUncapped)
	unlock()

	r.pass(ctx)
	requireMaxSectorsKB(t, root, reconcileCapped)
	if !lockFreeDuringRead.Load() {
		t.Error("the volume lock was held while reading the MDTS")
	}
}

// A cached non-zero MDTS is dropped once a pass finds the controller not
// live, and is re-read after a bounded number of passes even when no pass
// saw the controller change: a controller reconnecting under the same name
// to a target that now advertises MDTS 0 gets the cap.
func TestTransferLimitReconciler_RereadsMDTSAfterReconnect(t *testing.T) {
	// r.pass reads the MDTS synchronously, so a plain variable suffices.
	setup := func(t *testing.T) (*transferLimitReconciler, *uint8) {
		t.Helper()
		mdts := uint8(5)
		_, r, _ := reconcileTestServer(t, func(string) (uint8, error) { return mdts, nil })
		r.pass(context.Background())
		requireMaxSectorsKB(t, r.sysfsRoot, reconcileUncapped)
		return r, &mdts
	}
	t.Run("reconnect seen", func(t *testing.T) {
		r, mdts := setup(t)
		addSubsysController(t, r.sysfsRoot, "nvme0", "connecting")
		r.pass(context.Background())
		*mdts = 0
		addSubsysController(t, r.sysfsRoot, "nvme0", "live")
		r.pass(context.Background())
		requireMaxSectorsKB(t, r.sysfsRoot, reconcileCapped)
	})
	t.Run("reconnect between passes", func(t *testing.T) {
		r, mdts := setup(t)
		*mdts = 0
		for range nvmeofMDTSRecheckPasses {
			r.pass(context.Background())
		}
		requireMaxSectorsKB(t, r.sysfsRoot, reconcileCapped)
	})
}

// A stage record written before the volume ID was persisted is capped
// under the lock NodeStageVolume and NodeUnstageVolume take, the volume ID
// kubelet records for its staging directory, not the lock of its state
// file name.  A record whose volume ID cannot be found is skipped and
// logged once.
func TestTransferLimitReconciler_LegacyRecordVolumeLock(t *testing.T) {
	const volumeID = "tank/pvc-legacy"
	setup := func(t *testing.T) (*NodeServer, *transferLimitReconciler, *bytes.Buffer) {
		t.Helper()
		readMDTS, _ := countingMDTS(0)
		srv, r, buf := reconcileTestServer(t, readMDTS)
		if err := os.Remove(srv.stateFilePath(reconcileTestVolume)); err != nil {
			t.Fatal(err)
		}
		s := legacyNVMeoFStageState("", mdtsTestNQN, "")
		if err := srv.writeStageState(volumeID, s); err != nil {
			t.Fatal(err)
		}
		return srv, r, buf
	}
	t.Run("volume ID from kubelet", func(t *testing.T) {
		srv, r, _ := setup(t)
		sum := sha256.Sum256([]byte(volumeID))
		dir := filepath.Join(r.kubeletDriverDir, hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, kubeletVolDataFile), `{"volumeHandle":"`+volumeID+`"}`)

		unlock := srv.volumeLocks.lock(volumeID)
		r.pass(context.Background())
		requireMaxSectorsKB(t, r.sysfsRoot, reconcileUncapped)
		unlock()

		r.pass(context.Background())
		requireMaxSectorsKB(t, r.sysfsRoot, reconcileCapped)
	})
	t.Run("volume ID unknown", func(t *testing.T) {
		_, r, buf := setup(t)
		r.pass(context.Background())
		r.pass(context.Background())
		requireMaxSectorsKB(t, r.sysfsRoot, reconcileUncapped)
		lines := decodeLogLines(t, buf)
		if len(lines) != 1 || lines[0]["level"] != "WARN" {
			t.Fatalf("log lines = %v, want one WARN line", lines)
		}
	})
}

// A device whose cap keeps failing the same way is logged once even when
// another device of the volume fails differently on a later pass.
func TestTransferLimitReconciler_LogsOncePerDevice(t *testing.T) {
	readMDTS, _ := countingMDTS(0)
	_, r, buf := reconcileTestServer(t, readMDTS)
	root := r.sysfsRoot
	breakMaxSectorsKB(t, root, "nvme0n1")
	breakMaxSectorsKB(t, root, "nvme0c0n1")
	r.pass(context.Background())
	if n := len(errorLogLines(decodeLogLines(t, buf))); n != 2 {
		t.Fatalf("error lines = %d, want one per device", n)
	}

	queue := filepath.Join(root, "block", "nvme0c0n1", "queue")
	if err := os.Remove(filepath.Join(queue, "max_sectors_kb")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(queue, "max_sectors_kb"), "32768\n")
	writeTestFile(t, filepath.Join(queue, "max_hw_sectors_kb"), "bogus\n")
	buf.Reset()
	r.pass(context.Background())

	lines := errorLogLines(decodeLogLines(t, buf))
	if len(lines) != 1 || lines[0]["device"] != "nvme0c0n1" {
		t.Fatalf("error lines = %v, want one for the device whose error changed", lines)
	}
}
