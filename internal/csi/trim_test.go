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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	trimTestInterval = time.Hour
	trimTestJitter   = 20 * time.Minute
	trimTestChunk    = 10
)

// trimCall is one FITRIM issued by the trimmer under test.
type trimCall struct {
	path          string
	start, length uint64
}

// trimObservation is one attempt reported to the observer.
type trimObservation struct {
	result TrimResult
	bytes  uint64
}

// trimHarness drives a trimmer whose clock, jitter, mount table, device
// check and FITRIM are fakes.  All fakes run on the trimmer goroutine.
type trimHarness struct {
	t        *testing.T
	srv      *NodeServer
	tr       *trimmer
	stagedAt time.Time
	now      time.Time

	mu         sync.Mutex
	mounts     []mountInfoEntry
	mountsErr  error
	mountReads int
	onMounts   func(read int)
	deviceErr  error
	size       uint64
	calls      []trimCall
	trimResult func(call trimCall, n int) (uint64, error)
	observed   []trimObservation
}

func newTrimHarness(t *testing.T, srv *NodeServer) *trimHarness {
	t.Helper()
	if srv == nil {
		srv = NewNodeServerWithStateDir("test-node", nil, nil, t.TempDir())
	}
	stagedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	h := &trimHarness{t: t, srv: srv, stagedAt: stagedAt, now: stagedAt, size: 5}
	h.tr = &trimmer{
		n:                srv,
		interval:         trimTestInterval,
		chunk:            trimTestChunk,
		kubeletDriverDir: t.TempDir(),
		log:              slog.New(slog.DiscardHandler),
		obs:              h,
		now:              func() time.Time { return h.now },
		jitter:           func(string) time.Duration { return trimTestJitter },
		tryLock:          srv.volumeLocks.tryLock,
		fitrim:           h.fitrim,
		fsSize:           func(string) (uint64, error) { return h.size, nil },
		mounts:           h.readMounts,
		deviceMatches:    func(*nodeStageState, mountInfoEntry) error { return h.deviceErr },
		noticed:          make(map[string]time.Time),
	}
	return h
}

func (h *trimHarness) ObserveTrim(result TrimResult, trimmedBytes uint64, _ time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observed = append(h.observed, trimObservation{result, trimmedBytes})
}

func (h *trimHarness) readMounts() ([]mountInfoEntry, error) {
	h.mountReads++
	if h.onMounts != nil {
		h.onMounts(h.mountReads)
	}
	return slices.Clone(h.mounts), h.mountsErr
}

func (h *trimHarness) fitrim(path string, _, _ uint32, start, length uint64) (uint64, error) {
	h.mu.Lock()
	call := trimCall{path, start, length}
	h.calls = append(h.calls, call)
	n := len(h.calls)
	h.mu.Unlock()
	if h.trimResult != nil {
		return h.trimResult(call, n)
	}
	return 1, nil
}

func (h *trimHarness) trimCalls() []trimCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func (h *trimHarness) observations() []trimObservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.observed)
}

// mount adds a mount of an ext4 filesystem at path.
func (h *trimHarness) mount(path string) {
	h.mounts = append(h.mounts, mountInfoEntry{
		Major: 259, Minor: 1, MountPoint: path, FsType: "ext4", Source: "/dev/nvme0n1",
	})
}

// stage writes the stage record of a Filesystem-mode NVMe-oF volume staged
// at stagingPath, modified by mods, with the file's time set to stagedAt.
func (h *trimHarness) stage(volumeID, stagingPath string, mods ...func(*nodeStageState)) {
	h.t.Helper()
	rec := &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		AccessType:   AccessTypeFilesystem,
		FsType:       "ext4",
		NVMeoF:       &NVMeoFStageState{SubsysNQN: "nqn.test:" + volumeID, Address: testStorageAddr, Port: "4420"},
		VolumeID:     volumeID,
		StagingPath:  stagingPath,
	}
	for _, m := range mods {
		m(rec)
	}
	if err := h.srv.writeStageState(volumeID, rec); err != nil {
		h.t.Fatalf("write stage state: %v", err)
	}
	if err := os.Chtimes(h.srv.stateFilePath(volumeID), h.stagedAt, h.stagedAt); err != nil {
		h.t.Fatalf("set stage state time: %v", err)
	}
}

func (h *trimHarness) lastTrim(volumeID string) *time.Time {
	h.t.Helper()
	st, err := h.srv.readStageState(volumeID)
	if err != nil || st == nil {
		h.t.Fatalf("read stage state of %q = %+v, %v", volumeID, st, err)
	}
	return st.LastTrim
}

func (h *trimHarness) round() {
	h.tr.runRound(context.Background())
}

// TestTrim_FirstTrimAfterJitterThenEveryInterval verifies the schedule: the
// first trim is due at stage time + jitter, every later one one interval
// after the last attempt, and a restarted trimmer continues from the
// persisted last_trim instead of trimming again.
func TestTrim_FirstTrimAfterJitterThenEveryInterval(t *testing.T) {
	t.Parallel()
	h := newTrimHarness(t, nil)
	const vol = "tank/pvc-sched"
	path := filepath.Join(t.TempDir(), "globalmount")
	h.stage(vol, path)
	h.mount(path)

	h.now = h.stagedAt.Add(trimTestJitter - time.Second)
	h.round()
	if n := len(h.trimCalls()); n != 0 {
		t.Fatalf("trims before stage time + jitter = %d, want 0", n)
	}

	firstDue := h.stagedAt.Add(trimTestJitter)
	h.now = firstDue
	h.round()
	if n := len(h.trimCalls()); n != 1 {
		t.Fatalf("trims at stage time + jitter = %d, want 1", n)
	}
	if got := h.lastTrim(vol); got == nil || !got.Equal(firstDue) {
		t.Fatalf("last_trim = %v, want %v", got, firstDue)
	}

	// pillar-node restarts: a new trimmer reads the persisted last_trim.
	restarted := newTrimHarness(t, h.srv)
	restarted.mounts = h.mounts
	restarted.now = firstDue.Add(trimTestInterval - time.Second)
	restarted.round()
	if n := len(restarted.trimCalls()); n != 0 {
		t.Fatalf("trims within one interval of last_trim after restart = %d, want 0", n)
	}
	restarted.now = firstDue.Add(trimTestInterval)
	restarted.round()
	if n := len(restarted.trimCalls()); n != 1 {
		t.Fatalf("trims one interval after last_trim = %d, want 1", n)
	}
}

// TestHashJitter_BoundedStableAndSpread verifies the default first-trim
// delay lies in [0, interval), is the same for a record across restarts,
// and spreads distinct records over the interval.
func TestHashJitter_BoundedStableAndSpread(t *testing.T) {
	t.Parallel()
	const interval = 168 * time.Hour
	jitter, afterRestart := hashJitter(interval), hashJitter(interval)
	var early, late int
	for i := range 200 {
		key := fmt.Sprintf("pool_pvc-%d", i)
		d := jitter(key)
		if d < 0 || d >= interval {
			t.Fatalf("jitter(%q) = %s, want within [0, %s)", key, d, interval)
		}
		if again := afterRestart(key); again != d {
			t.Fatalf("jitter(%q) = %s after restart, was %s", key, again, d)
		}
		if d < interval/2 {
			early++
		} else {
			late++
		}
	}
	if early < 50 || late < 50 {
		t.Errorf("jitter of 200 records: %d in the first half, %d in the second; want spread", early, late)
	}
	if got := hashJitter(time.Nanosecond)("x"); got != 0 {
		t.Errorf("jitter with a 1ns interval = %s, want 0", got)
	}
}

// TestTrim_OnlyFilesystemVolumesNotOptedOut verifies block volumes and
// volumes with periodic_trim=false are never trimmed, while unset and true
// follow the node setting.
func TestTrim_OnlyFilesystemVolumesNotOptedOut(t *testing.T) {
	t.Parallel()
	h := newTrimHarness(t, nil)
	dir := t.TempDir()
	cases := map[string]func(*nodeStageState){
		"default":   func(*nodeStageState) {},
		"opted-in":  func(s *nodeStageState) { s.PeriodicTrim = new(true) },
		"opted-out": func(s *nodeStageState) { s.PeriodicTrim = new(false) },
		"block":     func(s *nodeStageState) { s.AccessType = AccessTypeBlock; s.FsType = "" },
	}
	for name, mod := range cases {
		path := filepath.Join(dir, name)
		h.stage("tank/"+name, path, mod)
		h.mount(path)
	}
	h.now = h.stagedAt.Add(trimTestInterval)
	h.round()

	calls := h.trimCalls()
	trimmed := make([]string, 0, len(calls))
	for _, c := range calls {
		trimmed = append(trimmed, filepath.Base(c.path))
	}
	slices.Sort(trimmed)
	if want := []string{"default", "opted-in"}; !slices.Equal(trimmed, want) {
		t.Errorf("trimmed %v, want %v", trimmed, want)
	}
}

// TestTrim_BusyVolumeSkippedUntilNextRound verifies a volume whose lock a
// CSI operation holds is skipped without recording an attempt and trimmed
// in the next round.
func TestTrim_BusyVolumeSkippedUntilNextRound(t *testing.T) {
	t.Parallel()
	h := newTrimHarness(t, nil)
	const vol = "tank/pvc-busy"
	path := filepath.Join(t.TempDir(), "globalmount")
	h.stage(vol, path)
	h.mount(path)
	h.now = h.stagedAt.Add(trimTestInterval)

	unlock := h.srv.volumeLocks.lock(vol)
	h.round()
	unlock()
	if n := len(h.trimCalls()); n != 0 {
		t.Fatalf("trims while the volume lock is held = %d, want 0", n)
	}
	if got := h.lastTrim(vol); got != nil {
		t.Fatalf("last_trim after a busy skip = %v, want unset", got)
	}
	if obs := h.observations(); len(obs) != 0 {
		t.Fatalf("observed %v for a busy skip, want nothing", obs)
	}

	h.round()
	if n := len(h.trimCalls()); n != 1 {
		t.Errorf("trims in the round after the lock was released = %d, want 1", n)
	}
}

// TestTrim_ChunksCoverFilesystemAndReverifyMount verifies the filesystem
// is trimmed in chunks, the last one extending to the end, with the mount
// verified before every chunk, the lock released between chunks, and the
// kernel-reported bytes summed.
func TestTrim_ChunksCoverFilesystemAndReverifyMount(t *testing.T) {
	t.Parallel()
	h := newTrimHarness(t, nil)
	const vol = "tank/pvc-chunks"
	path := filepath.Join(t.TempDir(), "globalmount")
	h.stage(vol, path)
	h.mount(path)
	h.size = 2*trimTestChunk + 5
	h.trimResult = func(trimCall, int) (uint64, error) {
		// FITRIM runs under the volume lock.
		if unlock, ok := h.srv.volumeLocks.tryLock(vol); ok {
			unlock()
			t.Error("volume lock not held during a FITRIM chunk")
		}
		return 7, nil
	}
	h.now = h.stagedAt.Add(trimTestInterval)
	h.round()

	want := []trimCall{
		{path, 0, trimTestChunk},
		{path, trimTestChunk, trimTestChunk},
		{path, 2 * trimTestChunk, math.MaxUint64 - 2*trimTestChunk},
	}
	if got := h.trimCalls(); !slices.Equal(got, want) {
		t.Fatalf("FITRIM calls = %v, want %v", got, want)
	}
	if h.mountReads != len(want) {
		t.Errorf("mount table read %d times for %d chunks, want once per chunk", h.mountReads, len(want))
	}
	if obs := h.observations(); !slices.Equal(obs, []trimObservation{{TrimResultSuccess, 21}}) {
		t.Errorf("observed %v, want one success of 21 bytes", obs)
	}
	if unlock, ok := h.srv.volumeLocks.tryLock(vol); !ok {
		t.Error("volume lock still held after the trim")
	} else {
		unlock()
	}
}

// TestTrim_CancelStopsBetweenChunks verifies node shutdown stops a trim
// after the current chunk without recording the attempt.
func TestTrim_CancelStopsBetweenChunks(t *testing.T) {
	t.Parallel()
	h := newTrimHarness(t, nil)
	const vol = "tank/pvc-cancel"
	path := filepath.Join(t.TempDir(), "globalmount")
	h.stage(vol, path)
	h.mount(path)
	h.size = 5 * trimTestChunk
	ctx, cancel := context.WithCancel(context.Background())
	h.trimResult = func(trimCall, int) (uint64, error) {
		cancel()
		return 1, nil
	}
	h.now = h.stagedAt.Add(trimTestInterval)
	h.tr.runRound(ctx)

	if n := len(h.trimCalls()); n != 1 {
		t.Errorf("FITRIM calls after cancel in the first chunk = %d, want 1", n)
	}
	if got := h.lastTrim(vol); got != nil {
		t.Errorf("last_trim of an interrupted trim = %v, want unset (still due)", got)
	}
	if obs := h.observations(); len(obs) != 0 {
		t.Errorf("observed %v for an interrupted trim, want nothing", obs)
	}
}

// TestTrim_MountGoneMidTrimStops verifies a staging path unmounted between
// chunks is not trimmed further and the attempt counts as skipped.
func TestTrim_MountGoneMidTrimStops(t *testing.T) {
	t.Parallel()
	h := newTrimHarness(t, nil)
	const vol = "tank/pvc-vanish"
	path := filepath.Join(t.TempDir(), "globalmount")
	h.stage(vol, path)
	h.mount(path)
	h.size = 5 * trimTestChunk
	h.onMounts = func(read int) {
		if read == 2 {
			h.mounts = nil
		}
	}
	h.now = h.stagedAt.Add(trimTestInterval)
	h.round()

	if n := len(h.trimCalls()); n != 1 {
		t.Errorf("FITRIM calls = %d, want 1 (only the chunk before the unmount)", n)
	}
	if obs := h.observations(); len(obs) != 1 || obs[0].result != TrimResultSkipped {
		t.Errorf("observed %v, want one skipped attempt", obs)
	}
	if got := h.lastTrim(vol); got == nil {
		t.Error("last_trim unset after a skipped attempt, want recorded")
	}
}

// TestTrim_UnverifiedMountNeverTrimmed verifies FITRIM is never issued
// unless the staging path itself is a mount of the volume's filesystem type
// on the volume's device.
func TestTrim_UnverifiedMountNeverTrimmed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup      func(h *trimHarness, path string)
		wantResult TrimResult
	}{
		"not mounted": {
			setup:      func(*trimHarness, string) {},
			wantResult: TrimResultSkipped,
		},
		"only the parent is mounted": {
			setup:      func(h *trimHarness, path string) { h.mount(filepath.Dir(path)) },
			wantResult: TrimResultSkipped,
		},
		"stacked mount of another filesystem type": {
			setup: func(h *trimHarness, path string) {
				h.mount(path)
				h.mounts = append(h.mounts, mountInfoEntry{Major: 0, Minor: 50, MountPoint: path, FsType: "tmpfs"})
			},
			wantResult: TrimResultSkipped,
		},
		"another device": {
			setup: func(h *trimHarness, path string) {
				h.mount(path)
				h.deviceErr = fmt.Errorf("%w: other device", errTrimUnverified)
			},
			wantResult: TrimResultSkipped,
		},
		"mount table unreadable": {
			setup: func(h *trimHarness, path string) {
				h.mount(path)
				h.mountsErr = errors.New("permission denied")
			},
			wantResult: TrimResultError,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newTrimHarness(t, nil)
			const vol = "tank/pvc-unverified"
			path := filepath.Join(t.TempDir(), "globalmount")
			h.stage(vol, path)
			tc.setup(h, path)
			h.now = h.stagedAt.Add(trimTestInterval)
			h.round()

			if calls := h.trimCalls(); len(calls) != 0 {
				t.Fatalf("FITRIM calls = %v, want none", calls)
			}
			if obs := h.observations(); len(obs) != 1 || obs[0].result != tc.wantResult {
				t.Errorf("observed %v, want one %s attempt", obs, tc.wantResult)
			}
		})
	}
}

// TestTrim_ErrnoClassification verifies FITRIM errors map to results and
// every finished attempt is recorded so it is not retried before the next
// interval.
func TestTrim_ErrnoClassification(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want TrimResult
	}{
		"success":           {nil, TrimResultSuccess},
		"no discard":        {syscall.EOPNOTSUPP, TrimResultUnsupported},
		"no FITRIM ioctl":   {syscall.ENOTTY, TrimResultUnsupported},
		"read-only":         {syscall.EROFS, TrimResultSkipped},
		"I/O error":         {syscall.EIO, TrimResultError},
		"device mismatched": {fmt.Errorf("%w: device changed", errTrimUnverified), TrimResultSkipped},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newTrimHarness(t, nil)
			const vol = "tank/pvc-errno"
			path := filepath.Join(t.TempDir(), "globalmount")
			h.stage(vol, path)
			h.mount(path)
			h.trimResult = func(trimCall, int) (uint64, error) { return 0, tc.err }
			h.now = h.stagedAt.Add(trimTestInterval)
			h.round()

			if obs := h.observations(); len(obs) != 1 || obs[0].result != tc.want {
				t.Errorf("observed %v, want one %s attempt", obs, tc.want)
			}
			if got := h.lastTrim(vol); got == nil || !got.Equal(h.now) {
				t.Errorf("last_trim = %v, want %v", got, h.now)
			}
		})
	}
}

// TestTrim_LegacyRecordUsesKubeletPathOnlyWhenMounted verifies a record
// written before staging_path existed is trimmed at kubelet's
// <driver dir>/<sha256hex(volumeID)>/globalmount, under the lock of its
// real volume ID, and only when that path passes the mount check.
func TestTrim_LegacyRecordUsesKubeletPathOnlyWhenMounted(t *testing.T) {
	t.Parallel()
	const vol = "tank/pvc-legacy"
	sum := sha256.Sum256([]byte(vol))
	legacy := func(s *nodeStageState) { s.VolumeID, s.StagingPath = "", "" }
	setupKubeletDir := func(t *testing.T, h *trimHarness) string {
		t.Helper()
		dir := filepath.Join(h.tr.kubeletDriverDir, hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		data := `{"driverName":"pillar-csi.bhyoo.com","volumeHandle":"` + vol + `"}`
		if err := os.WriteFile(filepath.Join(dir, kubeletVolDataFile), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, kubeletGlobalMountDir)
	}

	t.Run("mounted", func(t *testing.T) {
		t.Parallel()
		h := newTrimHarness(t, nil)
		h.stage(vol, "", legacy)
		path := setupKubeletDir(t, h)
		h.mount(path)
		h.now = h.stagedAt.Add(trimTestInterval)

		unlock := h.srv.volumeLocks.lock(vol)
		h.round()
		unlock()
		if n := len(h.trimCalls()); n != 0 {
			t.Fatalf("trims while the lock of %q is held = %d, want 0", vol, n)
		}
		h.round()
		if calls := h.trimCalls(); len(calls) != 1 || calls[0].path != path {
			t.Fatalf("FITRIM calls = %v, want one at %s", calls, path)
		}
	})
	t.Run("not mounted", func(t *testing.T) {
		t.Parallel()
		h := newTrimHarness(t, nil)
		h.stage(vol, "", legacy)
		setupKubeletDir(t, h)
		h.now = h.stagedAt.Add(trimTestInterval)
		h.round()
		if calls := h.trimCalls(); len(calls) != 0 {
			t.Fatalf("FITRIM calls = %v, want none", calls)
		}
		if obs := h.observations(); len(obs) != 1 || obs[0].result != TrimResultSkipped {
			t.Errorf("observed %v, want one skipped attempt", obs)
		}
	})
	t.Run("no kubelet staging directory", func(t *testing.T) {
		t.Parallel()
		h := newTrimHarness(t, nil)
		h.stage(vol, "", legacy)
		h.now = h.stagedAt.Add(trimTestInterval)
		h.round()
		if calls := h.trimCalls(); len(calls) != 0 {
			t.Fatalf("FITRIM calls = %v, want none", calls)
		}
		if got := h.lastTrim(vol); got != nil {
			t.Errorf("last_trim = %v, want unset", got)
		}
	})
}

// TestTrim_StagedVolumeTrimmedAtItsStagingPath verifies NodeStageVolume
// records what the trimmer needs: the staging path is trimmed, and a volume
// staged with periodic-trim=false is not.
func TestTrim_StagedVolumeTrimmedAtItsStagingPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	h := newTrimHarness(t, env.srv)
	stage := func(volumeID string, volCtx map[string]string) string {
		stagingPath := t.TempDir()
		_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountCap("ext4"),
			VolumeContext:     volCtx,
		})
		if err != nil {
			t.Fatalf("NodeStageVolume %s: %v", volumeID, err)
		}
		h.mount(stagingPath)
		return stagingPath
	}
	trimmedPath := stage("tank/pvc-default", mountVolumeContext("nqn.test:default", testStorageAddr))
	optedOut := mountVolumeContext("nqn.test:opted-out", testStorageAddr)
	optedOut[paramPeriodicTrim] = "false"
	stage("tank/pvc-opted-out", optedOut)

	h.now = time.Now().Add(trimTestInterval)
	h.round()
	if calls := h.trimCalls(); len(calls) != 1 || calls[0].path != trimmedPath {
		t.Errorf("FITRIM calls = %v, want one at %s", calls, trimmedPath)
	}
}

// TestTrim_UnstageWaitsForTheCurrentChunk verifies NodeUnstageVolume waits
// while a chunk is being trimmed and the trim does not resurrect the stage
// record it removes.
func TestTrim_UnstageWaitsForTheCurrentChunk(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	h := newTrimHarness(t, env.srv)
	const vol = "tank/pvc-unstage"
	stagingPath := t.TempDir()
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          vol,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     mountVolumeContext("nqn.test:unstage", testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	h.mount(stagingPath)
	inChunk, release := make(chan struct{}), make(chan struct{})
	h.trimResult = func(trimCall, int) (uint64, error) {
		close(inChunk)
		<-release
		return 1, nil
	}
	h.now = time.Now().Add(trimTestInterval)
	trimDone := make(chan struct{})
	go func() {
		defer close(trimDone)
		h.round()
	}()
	<-inChunk

	unstageDone := make(chan error, 1)
	go func() {
		_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
			VolumeId: vol, StagingTargetPath: stagingPath,
		})
		unstageDone <- err
	}()
	select {
	case err := <-unstageDone:
		t.Fatalf("NodeUnstageVolume returned (%v) while a trim chunk held the volume", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-trimDone
	if err := <-unstageDone; err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	if st, err := env.srv.readStageState(vol); err != nil || st != nil {
		t.Errorf("stage state after unstage = %+v, %v; want removed", st, err)
	}
}

// TestParseMountInfo verifies optional fields, octal escapes and stacked
// mounts (the last entry wins), and that malformed lines are errors.
func TestParseMountInfo(t *testing.T) {
	t.Parallel()
	const table = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
600 22 259:3 / /var/lib/kubelet/plugins/x/globalmount rw,relatime shared:300 master:2 - xfs /dev/nvme1n1 rw
601 600 0:55 / /var/lib/kubelet/plugins/x/globalmount rw - tmpfs tmpfs rw
602 22 253:4 / /mnt/with\040space rw - ext4 /dev/mapper/a\134b rw
`
	mounts, err := parseMountInfo(strings.NewReader(table))
	if err != nil {
		t.Fatalf("parseMountInfo: %v", err)
	}
	got, ok := findMount(mounts, "/var/lib/kubelet/plugins/x/globalmount/")
	if !ok || got.FsType != "tmpfs" || got.Major != 0 || got.Minor != 55 {
		t.Errorf("stacked mount = %+v (found %v), want the tmpfs 0:55 on top", got, ok)
	}
	got, ok = findMount(mounts, "/mnt/with space")
	want := mountInfoEntry{Major: 253, Minor: 4, MountPoint: "/mnt/with space", FsType: "ext4", Source: `/dev/mapper/a\b`}
	if !ok || got != want {
		t.Errorf("escaped mount = %+v (found %v), want %+v", got, ok, want)
	}
	if _, ok := findMount(mounts, "/var/lib/kubelet"); ok {
		t.Error("found a mount at a path that is only a parent of mount points")
	}

	for name, line := range map[string]string{
		"no separator":   "22 1 8:1 / / rw shared:1 ext4 /dev/sda1 rw\n",
		"bad device":     "22 1 8-1 / / rw - ext4 /dev/sda1 rw\n",
		"missing source": "22 1 8:1 / / rw - ext4\n",
	} {
		if _, err := parseMountInfo(strings.NewReader(line)); err == nil {
			t.Errorf("%s: parseMountInfo(%q) succeeded, want error", name, line)
		}
	}
}

// TestSysfsDeviceMatcher verifies the device check of each attach kind
// accepts the volume's device and rejects another one as unverified.
func TestSysfsDeviceMatcher(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	symlink := func(target, rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	write("dev/block/253:0/dm/name", "pillar-local-a")
	write("dev/block/259:1/device/subsysnqn", "nqn.test:a")
	// iSCSI: /sys/dev/block/8:16 -> …/session3/target6:0:0/6:0:0:1/block/sdb,
	// whose device link points at the SCSI device 6:0:0:1.
	sdb := "devices/platform/host6/session3/target6:0:0/6:0:0:1/block/sdb"
	if err := os.MkdirAll(filepath.Join(root, sdb), 0o750); err != nil {
		t.Fatal(err)
	}
	symlink("../../../6:0:0:1", sdb+"/device")
	symlink("../../"+sdb, "dev/block/8:16")
	write("class/iscsi_session/session3/targetname", "iqn.test:a")

	match := sysfsDeviceMatcher(root)
	local := func(name string) *nodeStageState {
		return localStageState(ProtocolNVMeoFTCP, AccessTypeFilesystem, name, "/dev/zvol/tank/a")
	}
	nvme := func(nqn string) *nodeStageState {
		return &nodeStageState{ProtocolType: ProtocolNVMeoFTCP, NVMeoF: &NVMeoFStageState{SubsysNQN: nqn}}
	}
	iscsi := func(iqn string, lun int) *nodeStageState {
		return &nodeStageState{ProtocolType: ProtocolISCSI, ISCSI: &ISCSIStageState{TargetIQN: iqn, LUN: lun}}
	}
	dev := func(major, minor uint32) mountInfoEntry { return mountInfoEntry{Major: major, Minor: minor} }
	for name, tc := range map[string]struct {
		state   *nodeStageState
		mount   mountInfoEntry
		matches bool
	}{
		"local":                  {local("pillar-local-a"), dev(253, 0), true},
		"local other dm":         {local("pillar-local-b"), dev(253, 0), false},
		"local on a non-dm disk": {local("pillar-local-a"), dev(259, 1), false},
		"nvme":                   {nvme("nqn.test:a"), dev(259, 1), true},
		"nvme other subsystem":   {nvme("nqn.test:b"), dev(259, 1), false},
		"nvme on the root disk":  {nvme("nqn.test:a"), dev(8, 1), false},
		"iscsi":                  {iscsi("iqn.test:a", 1), dev(8, 16), true},
		"iscsi other target":     {iscsi("iqn.test:b", 1), dev(8, 16), false},
		"iscsi other lun":        {iscsi("iqn.test:a", 0), dev(8, 16), false},
		"iscsi on an nvme disk":  {iscsi("iqn.test:a", 1), dev(259, 1), false},
	} {
		err := match(tc.state, tc.mount)
		switch {
		case tc.matches && err != nil:
			t.Errorf("%s: device check failed: %v", name, err)
		case !tc.matches && !errors.Is(err, errTrimUnverified):
			t.Errorf("%s: device check = %v, want errTrimUnverified", name, err)
		}
	}
}

// TestVolumeLockSet_TryLock verifies tryLock fails while the lock is held
// by lock or tryLock, succeeds once released, and leaves no idle entry.
func TestVolumeLockSet_TryLock(t *testing.T) {
	t.Parallel()
	var l volumeLockSet
	unlock := l.lock("a")
	if _, ok := l.tryLock("a"); ok {
		t.Fatal("tryLock succeeded while lock holds the volume")
	}
	if u, ok := l.tryLock("b"); !ok {
		t.Fatal("tryLock of another volume failed")
	} else {
		u()
	}
	unlock()
	u, ok := l.tryLock("a")
	if !ok {
		t.Fatal("tryLock failed after unlock")
	}
	if _, again := l.tryLock("a"); again {
		t.Fatal("tryLock succeeded while tryLock holds the volume")
	}
	u()
	if n := len(l.locks); n != 0 {
		t.Errorf("idle lock entries = %d, want 0", n)
	}
}
