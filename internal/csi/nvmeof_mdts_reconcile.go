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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// nvmeofTransferLimitInterval is the period of the transfer limit
// reconciler's passes over the staged NVMe-oF volumes.
const nvmeofTransferLimitInterval = 30 * time.Second

// nvmeofMDTSRecheckPasses bounds how long a non-zero MDTS answer is
// trusted: the live controllers are asked again after this many passes (5
// minutes), even when no pass saw them change.  A controller that
// reconnects between two passes keeps its name, and the target may answer
// differently after it restarted, e.g. on an older kernel.
const nvmeofMDTSRecheckPasses = 10

// transferLimitReconciler keeps the NVMe-oF request size cap (see
// LimitNVMeoFTransferSize) on the namespace devices of every staged NVMe-oF
// volume.  Kubelet does not repeat NodeStageVolume for a volume that stays
// mounted, so without it a volume staged by a release without the cap, or
// one whose controller was reconnecting when it was last checked, would
// never get the cap.
//
// It is not safe for concurrent use: one goroutine runs every pass.
type transferLimitReconciler struct {
	n         *NodeServer
	sysfsRoot string
	readMDTS  NVMeMDTSReader
	log       *slog.Logger
	tryLock   func(volumeID string) (unlock func(), ok bool)

	// kubeletDriverDir is kubelet's staging directory of this driver, read
	// to find the volume ID of a stage record written before the ID was
	// persisted (see kubeletStagingTarget).
	kubeletDriverDir string

	// passes counts the passes run so far.
	passes uint64

	// advertised maps a stage state key to the subsystem, limit and live
	// controllers (see limitSignature) last found to advertise an MDTS, and
	// the pass from which that answer is stale.  The kernel already splits
	// requests for such a subsystem, so its controllers are not asked again
	// until the signature changes, the answer is stale, or a pass finds the
	// subsystem disconnected or without a live controller.
	advertised map[string]advertisedMDTS

	// reported maps a stage state key to the outcomes last logged for it,
	// per device for a cap that failed on some devices and under "" for
	// any other outcome, so an unchanged failure or pending reason is
	// logged once.
	reported map[string]map[string]string
}

// advertisedMDTS is a cached answer that the live controllers of a stage
// record's subsystem advertise an MDTS.
type advertisedMDTS struct {
	signature string
	staleAt   uint64 // the pass number from which it is asked again
}

// nvmeofLimitTarget is what the reconciler applies for one stage record.
type nvmeofLimitTarget struct {
	volumeID string
	nqn      string
	size     int32
}

// StartNVMeoFTransferLimitReconciler starts, in the background, a pass
// over the NVMe-oF stage records now and then every 30 seconds until ctx
// is canceled; see transferLimitReconciler.  Kubelet's staging directories
// of the driver are found from driverName.  It returns at once: a target that
// does not answer Identify Controller never delays the caller.  The
// returned stop cancels the loop and waits for the pass in progress.
func (n *NodeServer) StartNVMeoFTransferLimitReconciler(
	ctx context.Context, driverName string, log *slog.Logger,
) (stop func()) {
	r := n.newTransferLimitReconciler(log, filepath.Join(kubeletCSIPluginDir, driverName))
	if r == nil {
		return func() {}
	}
	ticker := time.NewTicker(nvmeofTransferLimitInterval)
	stopLoop := r.start(ctx, ticker.C)
	return func() {
		stopLoop()
		ticker.Stop()
	}
}

// newTransferLimitReconciler returns the reconciler of n's NVMe-oF
// handler, or nil when the handler cannot cap a connected subsystem.
func (n *NodeServer) newTransferLimitReconciler(log *slog.Logger, kubeletDriverDir string) *transferLimitReconciler {
	limiter, ok := n.handlers[ProtocolNVMeoFTCP].(NVMeoFTransferLimiter)
	if !ok {
		return nil
	}
	sysfsRoot, readMDTS := limiter.TransferLimitSysfs()
	return &transferLimitReconciler{
		n:                n,
		sysfsRoot:        sysfsRoot,
		readMDTS:         readMDTS,
		log:              log,
		tryLock:          n.volumeLocks.tryLock,
		kubeletDriverDir: kubeletDriverDir,
		advertised:       make(map[string]advertisedMDTS),
		reported:         make(map[string]map[string]string),
	}
}

// start runs a pass now and one per value received from ticks in a new
// goroutine; the returned stop cancels it and waits for it to return.
func (r *transferLimitReconciler) start(ctx context.Context, ticks <-chan time.Time) (stop func()) {
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			r.pass(loopCtx)
			select {
			case <-loopCtx.Done():
				return
			case <-ticks:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// pass reconciles every stage record once and forgets the records that
// are gone.
func (r *transferLimitReconciler) pass(ctx context.Context) {
	r.passes++
	entries, err := os.ReadDir(r.n.stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		r.prune(nil) // nothing is staged on this node
		return
	}
	if err != nil {
		r.report("", "error:"+err.Error(), r.log.Error,
			"reconcile NVMe-oF transfer limits: read stage state dir", "dir", r.n.stateDir, "error", err)
		return
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() || filepath.Ext(e.Name()) != stateFileExt {
			continue
		}
		key := strings.TrimSuffix(e.Name(), stateFileExt)
		seen[key] = true
		r.reconcile(key)
	}
	r.prune(seen)
}

// prune drops the state of every key not in seen.
func (r *transferLimitReconciler) prune(seen map[string]bool) {
	for key := range r.advertised {
		if !seen[key] {
			delete(r.advertised, key)
		}
	}
	for key := range r.reported {
		if !seen[key] {
			delete(r.reported, key)
		}
	}
}

// reconcile caps the devices of the stage record key when they are above
// its limit and every live controller reports MDTS 0.  A device already at
// or below the limit costs no admin command.  A record whose subsystem is
// disconnected, has no live controller or no namespace device yet, or
// whose volume lock is held by a CSI call stays pending for the next pass,
// and its cached MDTS answer is dropped: the controllers that answer after
// a reconnect may keep their names but not their MDTS.
//
// The MDTS is read without the volume lock (Identify Controller can block
// for its timeout); the devices are then capped under the lock after
// re-reading the record, so a concurrent NodeStageVolume or
// NodeUnstageVolume never sees a half-applied cap.
func (r *transferLimitReconciler) reconcile(key string) {
	t, ok := r.target(key)
	if !ok {
		return
	}
	wantKB := nvmeofSizeKB(t.size)
	subsysPaths, err := nvmeofSubsystemPaths(r.sysfsRoot, t.nqn)
	if errors.Is(err, ErrNVMeoFSubsystemNotConnected) {
		r.pending(key, t, "subsystem not connected")
		return
	}
	if err != nil {
		r.fail(key, t, err)
		return
	}
	devices, err := nvmeNamespaceDevices(r.sysfsRoot, subsysPaths)
	if err != nil {
		r.fail(key, t, fmt.Errorf("list namespace devices of subsystem %q: %w", t.nqn, err))
		return
	}
	if len(devices) == 0 {
		r.pending(key, t, "no namespace device")
		return
	}
	if r.devicesWithin(devices, wantKB) {
		delete(r.reported, key)
		return
	}
	ctrls, err := liveControllersOf(subsysPaths)
	if err != nil {
		r.fail(key, t, fmt.Errorf("read MDTS of subsystem %q: %w", t.nqn, err))
		return
	}
	if len(ctrls) == 0 {
		r.pending(key, t, "no live controller")
		return
	}
	sig := limitSignature(t, ctrls)
	if a, ok := r.advertised[key]; ok && a.signature == sig && r.passes < a.staleAt {
		return
	}
	advertised, err := controllersAdvertiseMDTS(ctrls, r.readMDTS)
	if err != nil {
		r.fail(key, t, fmt.Errorf("read MDTS of subsystem %q: %w", t.nqn, err))
		return
	}
	if advertised {
		r.advertised[key] = advertisedMDTS{signature: sig, staleAt: r.passes + nvmeofMDTSRecheckPasses}
		delete(r.reported, key)
		return
	}
	delete(r.advertised, key)
	r.capLocked(key, t, wantKB)
}

// capLocked caps the devices of t under its volume lock, provided the
// stage record still asks for the same subsystem and limit.
func (r *transferLimitReconciler) capLocked(key string, t nvmeofLimitTarget, wantKB int64) {
	unlock, ok := r.tryLock(t.volumeID)
	if !ok {
		r.pending(key, t, "a CSI call holds the volume")
		return
	}
	defer unlock()
	cur, ok := r.target(key)
	if !ok || cur != t {
		return // unstaged or restaged meanwhile; the next pass re-evaluates
	}
	subsysPaths, err := nvmeofSubsystemPaths(r.sysfsRoot, t.nqn)
	if errors.Is(err, ErrNVMeoFSubsystemNotConnected) {
		r.pending(key, t, "subsystem not connected")
		return
	}
	if err == nil {
		err = capNVMeoFDevices(r.sysfsRoot, t.nqn, subsysPaths, wantKB)
	}
	if err != nil {
		r.fail(key, t, err)
		return
	}
	delete(r.reported, key)
	r.log.Info("capped NVMe-oF request size: the target advertises no MDTS",
		"volume", t.volumeID, "subsystem", t.nqn, "maxDataTransferSize", t.size)
}

// target reads the stage record key and returns what to apply, or false
// for a record that is gone, not an NVMe-oF remote attach, or has no
// limit.  An unreadable record, and one whose volume ID cannot be
// determined, is logged.
func (r *transferLimitReconciler) target(key string) (nvmeofLimitTarget, bool) {
	stateFile := filepath.Join(r.n.stateDir, key+stateFileExt)
	data, err := os.ReadFile(stateFile) //nolint:gosec // G304: entry of the controlled stateDir
	if errors.Is(err, fs.ErrNotExist) {
		return nvmeofLimitTarget{}, false // unstaged since the directory was listed
	}
	if err != nil {
		r.report(key, "error:"+err.Error(), r.log.Error, "reconcile NVMe-oF transfer limit: read stage state",
			"volume", key, "device", "", "stateFile", stateFile, "error", err)
		return nvmeofLimitTarget{}, false
	}
	state, err := decodeStageRecord(data)
	if err != nil {
		r.report(key, "error:"+err.Error(), r.log.Error, "reconcile NVMe-oF transfer limit: decode stage state",
			"volume", key, "device", "", "stateFile", stateFile, "error", err)
		return nvmeofLimitTarget{}, false
	}
	if state.ProtocolType != ProtocolNVMeoFTCP || state.isLocalAttach() ||
		state.NVMeoF == nil || state.NVMeoF.SubsysNQN == "" {
		return nvmeofLimitTarget{}, false
	}
	size := nvmeofStageLimit(state.NVMeoF)
	if size == 0 {
		return nvmeofLimitTarget{}, false
	}
	volumeID, err := r.volumeID(key, state)
	if err != nil {
		r.report(key, "skip:"+err.Error(), r.log.Warn,
			"NVMe-oF transfer limit skipped: cannot determine the volume ID of a stage record written "+
				"before it was recorded; the next NodeStageVolume of the volume records it",
			"stateFile", stateFile, "subsystem", state.NVMeoF.SubsysNQN, "error", err)
		return nvmeofLimitTarget{}, false
	}
	return nvmeofLimitTarget{volumeID: volumeID, nqn: state.NVMeoF.SubsysNQN, size: size}, true
}

// volumeID returns the CSI volume ID of the stage record key, the ID
// NodeStageVolume and NodeUnstageVolume lock the volume by.  A record
// written before the ID was persisted has only its state key, which
// stateFileKey derived by replacing "/" with "_": a key without "_" is the
// ID itself, any other is looked up in kubelet's staging directories.
func (r *transferLimitReconciler) volumeID(key string, state *nodeStageState) (string, error) {
	if state.VolumeID != "" {
		return state.VolumeID, nil
	}
	if !strings.Contains(key, "_") {
		return key, nil
	}
	volumeID, _, err := kubeletStagingTarget(r.kubeletDriverDir, key)
	if err != nil {
		return "", err
	}
	return volumeID, nil
}

// decodeStageRecord decodes a stage state file, converting the Phase 1
// {"subsys_nqn": …} format in memory.  Unlike readStageState it never
// rewrites the file: the reconciler does not hold the volume lock here.
func decodeStageRecord(data []byte) (*nodeStageState, error) {
	var state nodeStageState
	err := json.Unmarshal(data, &state)
	if err != nil {
		return nil, fmt.Errorf("decode stage state: %w", err)
	}
	if state.ProtocolType == "" {
		var raw legacyNodeStageState
		if json.Unmarshal(data, &raw) == nil && isLegacyFormat(&raw) {
			state = *migrateFromLegacy(&raw)
		}
	}
	return &state, nil
}

// devicesWithin reports whether queue/max_sectors_kb of every device is at
// or below wantKB.  An unreadable device counts as above, so the cap
// reports it.
func (r *transferLimitReconciler) devicesWithin(devices []string, wantKB int64) bool {
	for _, dev := range devices {
		path := filepath.Join(r.sysfsRoot, "block", dev, "queue", "max_sectors_kb")
		raw, err := os.ReadFile(path) //nolint:gosec // G304: sysfs path under connector-controlled root
		if err != nil {
			return false
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || kb > wantKB {
			return false
		}
	}
	return true
}

// limitSignature identifies the subsystem, limit and live controllers an
// MDTS answer applies to.
func limitSignature(t nvmeofLimitTarget, ctrls []string) string {
	return t.nqn + "\x00" + strconv.FormatInt(int64(t.size), 10) + "\x00" + strings.Join(ctrls, ",")
}

// pending logs, once per reason, that the record waits for the next pass,
// and drops its cached MDTS answer.
func (r *transferLimitReconciler) pending(key string, t nvmeofLimitTarget, reason string) {
	delete(r.advertised, key)
	r.report(key, "pending:"+reason, r.log.Info, "NVMe-oF transfer limit pending; retrying",
		"volume", t.volumeID, "subsystem", t.nqn, "reason", reason,
		"retryInterval", nvmeofTransferLimitInterval.String())
}

// fail logs err at error level: one line per device that could not be
// capped, each only while its own error changes, or one line for any other
// failure while it stays the same.
func (r *transferLimitReconciler) fail(key string, t nvmeofLimitTarget, err error) {
	const msg = "reconcile NVMe-oF transfer limit: cap request size"
	devErrs := nvmeofDeviceLimitErrors(err)
	if len(devErrs) == 0 {
		r.report(key, "error:"+err.Error(), r.log.Error, msg,
			"volume", t.volumeID, "subsystem", t.nqn, "device", t.nqn, "error", err)
		return
	}
	prev := r.reported[key]
	cur := make(map[string]string, len(devErrs))
	for _, d := range devErrs {
		outcome := "error:" + d.Err.Error()
		cur[d.Device] = outcome
		if prev[d.Device] != outcome {
			r.log.Error(msg, "volume", t.volumeID, "subsystem", t.nqn, "device", d.Device, "error", d.Err)
		}
	}
	r.reported[key] = cur
}

// report logs msg with logf unless outcome is what key last reported.
func (r *transferLimitReconciler) report(key, outcome string, logf func(string, ...any), msg string, args ...any) {
	if prev := r.reported[key]; len(prev) == 1 && prev[""] == outcome {
		return
	}
	r.reported[key] = map[string]string{"": outcome}
	logf(msg, args...)
}
