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

// Periodic filesystem trim.
//
// Thin zvols and LVs only get freed blocks back when the node issues
// discards, and neither Kubernetes nor the host fstrim.timer trims kubelet
// staging mounts.  The pillar-node process therefore runs one loop that
// issues FITRIM on the staging path of every Filesystem-mode volume staged
// on the node, sequentially, once per trim interval.
//
// Safety rules:
//   - Only the staging path is trimmed, never a publish path.
//   - Before every chunk the staging path must be a mount point whose device
//     is the volume's staged device: FITRIM on an unmounted directory would
//     trim the node's root filesystem.
//   - Each chunk runs under the per-volume lock NodeStageVolume,
//     NodeUnstageVolume and NodeExpandVolume take; the lock is released
//     between chunks so an unstage waits for at most one chunk.  A busy lock
//     skips the volume until the next round.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// trimChunkBytes is the byte range one FITRIM call covers.  The volume lock
// is held for one chunk at a time.
const trimChunkBytes uint64 = 16 << 30

// trimMaxWake caps the period of the loop: it wakes every
// min(interval, trimMaxWake) and trims every due volume.
const trimMaxWake = time.Minute

// kubeletCSIPluginDir is kubelet's CSI plugin directory; a staging path is
// <dir>/<driver>/<sha256hex(volumeID)>/globalmount.
const kubeletCSIPluginDir = "/var/lib/kubelet/plugins/kubernetes.io/csi"

// kubeletGlobalMountDir is the last element of a kubelet staging path.
const kubeletGlobalMountDir = "globalmount"

// kubeletVolDataFile is the file kubelet writes next to globalmount
// recording the volume handle of the staging directory.
const kubeletVolDataFile = "vol_data.json"

// procMountInfoPath is the mount table of the pillar-node process.
const procMountInfoPath = "/proc/self/mountinfo"

// trimSysfsRoot is the sysfs root the device check reads.
const trimSysfsRoot = "/sys"

// TrimResult is the outcome of one periodic trim attempt.
type TrimResult string

// Periodic trim outcomes, the result label of the node's trim metrics.
const (
	// TrimResultSuccess: the whole filesystem was trimmed.
	TrimResultSuccess TrimResult = "success"
	// TrimResultSkipped: the filesystem is read-only, or the staging path
	// is not the volume's mounted filesystem.
	TrimResultSkipped TrimResult = "skipped"
	// TrimResultUnsupported: the device or filesystem does not support
	// discard (EOPNOTSUPP / ENOTTY).
	TrimResultUnsupported TrimResult = "unsupported"
	// TrimResultError: any other failure.
	TrimResultError TrimResult = "error"
)

// TrimObserver receives the outcome of every finished trim attempt: the
// bytes the kernel reported discarded (also for an attempt that failed
// after some chunks) and the duration of the whole attempt.
type TrimObserver interface {
	ObserveTrim(result TrimResult, trimmedBytes uint64, duration time.Duration)
}

// TrimOptions configures StartTrimmer.
type TrimOptions struct {
	// Interval is the time between two trims of a volume; must be positive.
	Interval time.Duration
	// DriverName is the CSI driver name; it names kubelet's staging
	// directory of legacy stage records that lack the staging path.
	DriverName string
	// Logger receives the trim log; must not be nil.
	Logger *slog.Logger
	// Observer receives every attempt's outcome; nil discards them.
	Observer TrimObserver
}

// errTrimUnverified marks a staging path that is not (or no longer
// provably) the volume's mounted filesystem.  Such a volume is skipped.
var errTrimUnverified = errors.New("staging path is not verifiably the volume's mounted filesystem")

// errTrimStageGone marks a volume that was unstaged, restaged at another
// path, or switched out of periodic trim during an attempt.
var errTrimStageGone = errors.New("volume is no longer staged for periodic trim at this path")

// fitrimFunc issues FITRIM for [start, start+length) on the filesystem
// mounted at path, after checking the opened directory is on device
// major:minor.  It returns the bytes the kernel reported trimmed.
type fitrimFunc func(path string, major, minor uint32, start, length uint64) (uint64, error)

// fsSizeFunc returns the size in bytes of the filesystem mounted at path.
type fsSizeFunc func(path string) (uint64, error)

// mountInfoEntry is one line of /proc/<pid>/mountinfo.
type mountInfoEntry struct {
	Major, Minor uint32
	Root         string
	MountPoint   string
	FsType       string
	Source       string
}

// trimmer is the periodic trim loop.  Every dependency on time, randomness,
// the kernel and the lock is a field so the scheduler is testable anywhere.
type trimmer struct {
	n                *NodeServer
	interval         time.Duration
	chunk            uint64
	kubeletDriverDir string
	log              *slog.Logger
	obs              TrimObserver

	now           func() time.Time
	jitter        func(key string) time.Duration
	tryLock       func(volumeID string) (unlock func(), ok bool)
	fitrim        fitrimFunc
	fsSize        fsSizeFunc
	mounts        func() ([]mountInfoEntry, error)
	deviceMatches func(state *nodeStageState, mount mountInfoEntry) error

	// noticed throttles the log of records that cannot be attempted (see
	// notice): key → time of the last log line.
	noticed map[string]time.Time
}

// StartTrimmer starts the periodic trim loop of the node's staged
// Filesystem-mode volumes in the background.  The returned stop cancels the
// loop and waits for it to return; an attempt in progress stops after its
// current chunk.  It fails when opts is invalid or the platform has no
// FITRIM.
func (n *NodeServer) StartTrimmer(ctx context.Context, opts TrimOptions) (stop func(), err error) {
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("start periodic trim: interval %s must be positive", opts.Interval)
	}
	if opts.DriverName == "" {
		return nil, errors.New("start periodic trim: driver name is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("start periodic trim: logger is required")
	}
	fitrim, fsSize, err := platformTrimFuncs()
	if err != nil {
		return nil, fmt.Errorf("start periodic trim: %w", err)
	}
	t := newTrimmer(n, opts, fitrim, fsSize)
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.run(loopCtx)
	}()
	opts.Logger.Info("periodic trim enabled", "interval", opts.Interval.String())
	return func() {
		cancel()
		<-done
	}, nil
}

// newTrimmer builds a trimmer with the production dependencies.
func newTrimmer(n *NodeServer, opts TrimOptions, fitrim fitrimFunc, fsSize fsSizeFunc) *trimmer {
	obs := opts.Observer
	if obs == nil {
		obs = discardTrimObserver{}
	}
	return &trimmer{
		n:                n,
		interval:         opts.Interval,
		chunk:            trimChunkBytes,
		kubeletDriverDir: filepath.Join(kubeletCSIPluginDir, opts.DriverName),
		log:              opts.Logger,
		obs:              obs,
		now:              time.Now,
		jitter:           hashJitter(opts.Interval),
		tryLock:          n.volumeLocks.tryLock,
		fitrim:           fitrim,
		fsSize:           fsSize,
		mounts:           func() ([]mountInfoEntry, error) { return readMountInfoFile(procMountInfoPath) },
		deviceMatches:    sysfsDeviceMatcher(trimSysfsRoot),
		noticed:          make(map[string]time.Time),
	}
}

type discardTrimObserver struct{}

func (discardTrimObserver) ObserveTrim(TrimResult, uint64, time.Duration) {}

// hashJitter returns the first-trim delay of a stage record: uniform over
// [0, interval) across records, and stable for one record so a restart of
// pillar-node does not move a volume's first trim.
func hashJitter(interval time.Duration) func(key string) time.Duration {
	return func(key string) time.Duration {
		sum := sha256.Sum256([]byte(key))
		//nolint:gosec // G115: interval > 0, so the result is in [0, interval)
		return time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(interval))
	}
}

// run trims every due volume now and then every min(interval, 1m) until
// ctx is canceled.
func (t *trimmer) run(ctx context.Context) {
	ticker := time.NewTicker(min(t.interval, trimMaxWake))
	defer ticker.Stop()
	for {
		t.runRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runRound attempts every due volume once, sequentially.
func (t *trimmer) runRound(ctx context.Context) {
	t.pruneNoticed()
	entries, err := os.ReadDir(t.n.stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return // nothing was ever staged on this node
	}
	if err != nil {
		t.notice(ctx, "", slog.LevelError, "periodic trim: list stage state directory",
			"dir", t.n.stateDir, "error", err)
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() || filepath.Ext(e.Name()) != stateFileExt {
			continue
		}
		t.considerRecord(ctx, strings.TrimSuffix(e.Name(), stateFileExt))
	}
}

// considerRecord attempts the volume of the stage state file key when it is
// a Filesystem-mode volume that has not opted out and whose trim is due.
func (t *trimmer) considerRecord(ctx context.Context, key string) {
	path := filepath.Join(t.n.stateDir, key+stateFileExt)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return // unstaged since the directory was listed
	}
	if err != nil {
		t.notice(ctx, key, slog.LevelError, "periodic trim: stat stage state", "stateFile", path, "error", err)
		return
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: entry of the controlled stateDir
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.notice(ctx, key, slog.LevelError, "periodic trim: read stage state", "stateFile", path, "error", err)
		return
	}
	var rec nodeStageState
	err = json.Unmarshal(data, &rec)
	if err != nil {
		t.notice(ctx, key, slog.LevelError, "periodic trim: decode stage state", "stateFile", path, "error", err)
		return
	}
	if !trimEligible(&rec) || t.now().Before(t.dueAt(&rec, info.ModTime(), key)) {
		return
	}
	volumeID, stagingPath, err := t.target(&rec, key)
	if err != nil {
		t.notice(ctx, key, slog.LevelInfo, "periodic trim skipped: cannot determine the staging path",
			"stateFile", path, "error", err)
		return
	}
	t.trimVolume(ctx, volumeID, stagingPath)
}

// trimEligible reports whether rec is a Filesystem-mode volume that has
// not opted out.  An empty access type is a record written before Block
// mode existed, i.e. Filesystem mode.
func trimEligible(rec *nodeStageState) bool {
	if rec.ProtocolType == ProtocolNFS {
		return false
	}
	if rec.AccessType != "" && rec.AccessType != AccessTypeFilesystem {
		return false
	}
	return rec.PeriodicTrim == nil || *rec.PeriodicTrim
}

// dueAt is when rec's next trim is due: one interval after the last
// attempt, or — before the first attempt — a stable random delay in
// [0, interval) after the time the record was staged (StagedAt; the state
// file's modification time for records written before it was persisted).
func (t *trimmer) dueAt(rec *nodeStageState, fileModTime time.Time, key string) time.Time {
	if rec.LastTrim != nil {
		return rec.LastTrim.Add(t.interval)
	}
	if rec.StagedAt != nil {
		return rec.StagedAt.Add(t.jitter(key))
	}
	return fileModTime.Add(t.jitter(key))
}

// target returns the volume ID and the staging path of rec.  Records
// written before both were persisted fall back to kubeletStagingTarget.
func (t *trimmer) target(rec *nodeStageState, key string) (volumeID, stagingPath string, err error) {
	if rec.VolumeID != "" && rec.StagingPath != "" {
		return rec.VolumeID, rec.StagingPath, nil
	}
	return kubeletStagingTarget(t.kubeletDriverDir, key)
}

// kubeletStagingTarget derives the volume ID and kubelet's staging path
// <kubeletDriverDir>/<sha256hex(volumeID)>/globalmount of the stage record
// key when the record lacks them.  The state file name is not invertible
// ("/" became "_"), so the volume handle is read from the vol_data.json
// kubelet keeps in each staging directory.  The path is only a candidate:
// the periodic trim's mount check before every chunk still decides whether
// it is trimmed.
func kubeletStagingTarget(kubeletDriverDir, key string) (volumeID, stagingPath string, err error) {
	entries, err := os.ReadDir(kubeletDriverDir)
	if err != nil {
		return "", "", fmt.Errorf("list kubelet staging directories in %q: %w", kubeletDriverDir, err)
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dataPath := filepath.Join(kubeletDriverDir, e.Name(), kubeletVolDataFile)
		handle, readErr := readKubeletVolumeHandle(dataPath)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			errs = append(errs, readErr)
			continue
		}
		sum := sha256.Sum256([]byte(handle))
		if stateFileKey(handle) != key || e.Name() != hex.EncodeToString(sum[:]) {
			continue
		}
		return handle, filepath.Join(kubeletDriverDir, e.Name(), kubeletGlobalMountDir), nil
	}
	notFound := fmt.Errorf("no kubelet staging directory in %q records a volume handle for stage state %q",
		kubeletDriverDir, key)
	return "", "", errors.Join(append([]error{notFound}, errs...)...) //nolint:wrapcheck // items wrapped
}

// readKubeletVolumeHandle reads the volumeHandle of a kubelet vol_data.json.
func readKubeletVolumeHandle(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: kubelet staging directory entry
	if err != nil {
		return "", fmt.Errorf("read kubelet volume data %q: %w", path, err)
	}
	var volData struct {
		VolumeHandle string `json:"volumeHandle"`
	}
	err = json.Unmarshal(data, &volData)
	if err != nil {
		return "", fmt.Errorf("decode kubelet volume data %q: %w", path, err)
	}
	return volData.VolumeHandle, nil
}

// trimVolume trims the filesystem at stagingPath chunk by chunk, taking the
// volume lock for each chunk.  It stops without recording an attempt when
// ctx is canceled or the lock is busy; the volume stays due.
func (t *trimmer) trimVolume(ctx context.Context, volumeID, stagingPath string) {
	started := t.now()
	var trimmed, size uint64
	for offset := uint64(0); ; offset += t.chunk {
		if ctx.Err() != nil {
			t.log.Info("periodic trim interrupted by shutdown; the volume stays due",
				"volume", volumeID, "path", stagingPath, "trimmedBytes", trimmed)
			return
		}
		unlock, ok := t.tryLock(volumeID)
		if !ok {
			t.log.Info("periodic trim deferred: a CSI operation holds the volume; retrying next round",
				"volume", volumeID, "path", stagingPath)
			return
		}
		last, n, err := t.trimChunk(volumeID, stagingPath, offset, &size)
		trimmed += n
		if err != nil || last {
			t.finish(volumeID, stagingPath, started, trimmed, err)
		}
		unlock()
		if err != nil || last {
			return
		}
	}
}

// trimChunk trims the chunk at offset.  The caller holds the volume lock.
// The stage record and the mount are re-checked first; *size is measured
// with the first chunk.  The last chunk extends to the end of the
// filesystem, so blocks beyond the statfs size are covered too.
func (t *trimmer) trimChunk(
	volumeID, stagingPath string, offset uint64, size *uint64,
) (last bool, n uint64, err error) {
	state, err := t.n.readStageState(volumeID)
	if err != nil {
		return false, 0, fmt.Errorf("read stage state of volume %q: %w", volumeID, err)
	}
	if state == nil || !trimEligible(state) || (state.StagingPath != "" && state.StagingPath != stagingPath) {
		return false, 0, errTrimStageGone
	}
	mount, err := t.verifyMount(stagingPath, state)
	if err != nil {
		return false, 0, err
	}
	if offset == 0 {
		*size, err = t.fsSize(stagingPath)
		if err != nil {
			return false, 0, fmt.Errorf("measure filesystem at %q: %w", stagingPath, err)
		}
	}
	length := t.chunk
	last = *size <= t.chunk || offset >= *size-t.chunk
	if last {
		length = math.MaxUint64 - offset
	}
	n, err = t.fitrim(stagingPath, mount.Major, mount.Minor, offset, length)
	if err != nil {
		return last, n, fmt.Errorf("trim %q from byte %d: %w", stagingPath, offset, err)
	}
	return last, n, nil
}

// verifyMount checks that path is a mount point of state's filesystem type
// whose device is the volume's staged device, and returns its mount entry.
func (t *trimmer) verifyMount(path string, state *nodeStageState) (mountInfoEntry, error) {
	mounts, err := t.mounts()
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("read mount table: %w", err)
	}
	mount, ok := findMount(mounts, path)
	if !ok {
		return mountInfoEntry{}, fmt.Errorf("%w: %q is not a mount point", errTrimUnverified, path)
	}
	if state.FsType != "" && mount.FsType != state.FsType {
		return mountInfoEntry{}, fmt.Errorf("%w: %q is a %s mount, the volume was staged as %s",
			errTrimUnverified, path, mount.FsType, state.FsType)
	}
	err = t.deviceMatches(state, mount)
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("verify device %d:%d mounted at %q: %w",
			mount.Major, mount.Minor, path, err)
	}
	return mount, nil
}

// finish logs, observes and records a finished attempt.  The caller holds
// the volume lock.
func (t *trimmer) finish(volumeID, stagingPath string, started time.Time, trimmed uint64, err error) {
	if errors.Is(err, errTrimStageGone) {
		t.log.Info("periodic trim stopped: the volume was unstaged or changed",
			"volume", volumeID, "path", stagingPath)
		return
	}
	result := classifyTrimError(err)
	duration := t.now().Sub(started)
	switch result {
	case TrimResultSuccess:
		t.log.Info("periodic trim finished", "volume", volumeID, "path", stagingPath,
			"trimmedBytes", trimmed, "duration", duration.String())
	case TrimResultUnsupported:
		t.log.Info("periodic trim skipped: the device or filesystem does not support discard",
			"volume", volumeID, "path", stagingPath, "error", err)
	case TrimResultSkipped:
		t.log.Info("periodic trim skipped", "volume", volumeID, "path", stagingPath, "error", err)
	case TrimResultError:
		t.log.Error("periodic trim failed", "volume", volumeID, "path", stagingPath,
			"trimmedBytes", trimmed, "error", err)
	}
	t.obs.ObserveTrim(result, trimmed, duration)
	recordErr := t.recordAttempt(volumeID, started)
	if recordErr != nil {
		t.log.Error("periodic trim: persist last trim time; the volume is retried next round",
			"volume", volumeID, "error", recordErr)
	}
}

// recordAttempt persists at as the volume's last trim attempt through the
// atomic stage state write.  The caller holds the volume lock, so the
// record cannot be unstaged or restaged in between; a record that is gone
// is not recreated.
func (t *trimmer) recordAttempt(volumeID string, at time.Time) error {
	state, err := t.n.readStageState(volumeID)
	if err != nil {
		return fmt.Errorf("read stage state of volume %q: %w", volumeID, err)
	}
	if state == nil {
		return nil
	}
	state.LastTrim = &at
	err = t.n.writeStageState(volumeID, state)
	if err != nil {
		return fmt.Errorf("write stage state of volume %q: %w", volumeID, err)
	}
	return nil
}

// classifyTrimError maps an attempt's error to its result.
func classifyTrimError(err error) TrimResult {
	switch {
	case err == nil:
		return TrimResultSuccess
	case errors.Is(err, errTrimUnverified), errors.Is(err, syscall.EROFS):
		return TrimResultSkipped
	case errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.ENOTTY):
		return TrimResultUnsupported
	default:
		return TrimResultError
	}
}

// notice logs a problem that keeps a record from being attempted at most
// once per interval per record and message, since the loop retries every
// minute.
func (t *trimmer) notice(ctx context.Context, key string, level slog.Level, msg string, args ...any) {
	id := key + "\x00" + msg
	now := t.now()
	if last, ok := t.noticed[id]; ok && now.Sub(last) < t.interval {
		return
	}
	t.noticed[id] = now
	t.log.Log(ctx, level, msg, args...)
}

// pruneNoticed forgets throttle entries older than one interval.
func (t *trimmer) pruneNoticed() {
	now := t.now()
	for id, last := range t.noticed {
		if now.Sub(last) >= t.interval {
			delete(t.noticed, id)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Mount and device verification
// ─────────────────────────────────────────────────────────────────────────────

// findMount returns the topmost mount at path: the last mountinfo entry for
// it, since later entries are stacked over earlier ones.
func findMount(mounts []mountInfoEntry, path string) (mountInfoEntry, bool) {
	path = filepath.Clean(path)
	for _, m := range slices.Backward(mounts) {
		if m.MountPoint == path {
			return m, true
		}
	}
	return mountInfoEntry{}, false
}

// readMountInfoFile parses the mountinfo file at path.
func readMountInfoFile(path string) ([]mountInfoEntry, error) {
	f, err := os.Open(path) //nolint:gosec // G304: fixed procfs path
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only file
	mounts, err := parseMountInfo(f)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	return mounts, nil
}

// parseMountInfo parses the proc(5) mountinfo format:
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// Fields: mount ID, parent ID, major:minor, root, mount point, options, any
// number of optional fields, "-", filesystem type, source, super options.
func parseMountInfo(r io.Reader) ([]mountInfoEntry, error) {
	var mounts []mountInfoEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lineNo := 1; sc.Scan(); lineNo++ {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		sep := slices.Index(fields, "-")
		if sep < 6 || len(fields) < sep+3 {
			return nil, fmt.Errorf("line %d: malformed mountinfo entry %q", lineNo, sc.Text())
		}
		majStr, minStr, ok := strings.Cut(fields[2], ":")
		if !ok {
			return nil, fmt.Errorf("line %d: malformed device number %q", lineNo, fields[2])
		}
		major, majErr := strconv.ParseUint(majStr, 10, 32)
		minor, minErr := strconv.ParseUint(minStr, 10, 32)
		if majErr != nil || minErr != nil {
			return nil, fmt.Errorf("line %d: malformed device number %q: %w",
				lineNo, fields[2], errors.Join(majErr, minErr))
		}
		mounts = append(mounts, mountInfoEntry{
			Major:      uint32(major),
			Minor:      uint32(minor),
			Root:       unescapeMountInfo(fields[3]),
			MountPoint: unescapeMountInfo(fields[4]),
			FsType:     fields[sep+1],
			Source:     unescapeMountInfo(fields[sep+2]),
		})
	}
	err := sc.Err()
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	return mounts, nil
}

// unescapeMountInfo decodes the \ooo octal escapes the kernel writes for
// space, tab, newline and backslash in mountinfo paths.
func unescapeMountInfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// sysfsDeviceMatcher returns the check that the block device major:minor
// of a mount is the device the stage record names, read from sysfs under
// root:
//   - local attach: the device-mapper name (dm/name),
//   - NVMe-oF: the subsystem NQN of the namespace (device/subsysnqn),
//   - iSCSI: the target name of the session and the LUN of the SCSI device.
//
// A device that is not the volume's yields errTrimUnverified.
func sysfsDeviceMatcher(root string) func(state *nodeStageState, mount mountInfoEntry) error {
	return func(state *nodeStageState, mount mountInfoEntry) error {
		dev := filepath.Join(root, "dev", "block", fmt.Sprintf("%d:%d", mount.Major, mount.Minor))
		switch {
		case state.isLocalAttach():
			if state.Local == nil || state.Local.DMName == "" {
				return fmt.Errorf("%w: stage state has no device-mapper name", errTrimUnverified)
			}
			return expectSysfsAttr(filepath.Join(dev, "dm", "name"), state.Local.DMName, "device-mapper name")
		case state.ProtocolType == ProtocolNVMeoFTCP:
			if state.NVMeoF == nil || state.NVMeoF.SubsysNQN == "" {
				return fmt.Errorf("%w: stage state has no NVMe subsystem NQN", errTrimUnverified)
			}
			return expectSysfsAttr(filepath.Join(dev, "device", "subsysnqn"), state.NVMeoF.SubsysNQN,
				"NVMe subsystem NQN")
		case state.ProtocolType == ProtocolISCSI:
			if state.ISCSI == nil || state.ISCSI.TargetIQN == "" {
				return fmt.Errorf("%w: stage state has no iSCSI target", errTrimUnverified)
			}
			return matchISCSIDevice(root, dev, state.ISCSI)
		default:
			return fmt.Errorf("%w: no device check for protocol %q", errTrimUnverified, state.ProtocolType)
		}
	}
}

// matchISCSIDevice checks that the SCSI device behind the block device dev
// (…/session<N>/target<H:C:T>/<H:C:T:L>) is LUN st.LUN of a session to
// st.TargetIQN.
func matchISCSIDevice(root, dev string, st *ISCSIStageState) error {
	scsiDev, err := filepath.EvalSymlinks(filepath.Join(dev, "device"))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: mounted device is not a SCSI device (%w)", errTrimUnverified, err)
	}
	if err != nil {
		return fmt.Errorf("resolve SCSI device of %q: %w", dev, err)
	}
	parts := strings.Split(filepath.ToSlash(scsiDev), "/")
	hctl := strings.Split(parts[len(parts)-1], ":")
	if len(hctl) != 4 {
		return fmt.Errorf("%w: mounted device %q is not a SCSI device", errTrimUnverified, scsiDev)
	}
	lun, err := strconv.Atoi(hctl[3])
	if err != nil || lun != st.LUN {
		return fmt.Errorf("%w: mounted device %q is not LUN %d", errTrimUnverified, scsiDev, st.LUN)
	}
	idx := slices.IndexFunc(parts, func(p string) bool {
		n, ok := strings.CutPrefix(p, "session")
		if !ok || n == "" {
			return false
		}
		_, convErr := strconv.Atoi(n)
		return convErr == nil
	})
	if idx < 0 {
		return fmt.Errorf("%w: mounted device %q is not on an iSCSI session", errTrimUnverified, scsiDev)
	}
	return expectSysfsAttr(filepath.Join(root, "class", "iscsi_session", parts[idx], "targetname"),
		st.TargetIQN, "iSCSI target name")
}

// expectSysfsAttr checks that the sysfs attribute at path equals want.
func expectSysfsAttr(path, want, what string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: sysfs path built from the mount's device number
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: mounted device has no %s (%w)", errTrimUnverified, what, err)
	}
	if err != nil {
		return fmt.Errorf("read %s %q: %w", what, path, err)
	}
	got := strings.TrimSpace(string(data))
	if got != want {
		return fmt.Errorf("%w: mounted device has %s %q, the volume's is %q", errTrimUnverified, what, got, want)
	}
	return nil
}
