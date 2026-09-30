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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// ─────────────────────────────────────────────────────────────────────────────
// DeviceMapper — the kernel claim of a local attach
// ─────────────────────────────────────────────────────────────────────────────

// localDMPrefix prefixes every device-mapper device a local attach creates.
const localDMPrefix = "pillar-local-"

// localDMHashLen is the number of hex characters of sha256(volumeID) in a
// local device-mapper name.
const localDMHashLen = 16

// sectorSize is the unit of device-mapper table lengths.
const sectorSize = 512

// ErrLocalDeviceUnavailable reports that the backend device of a local attach
// does not exist or is not a block device.  NodeStageVolume maps it to
// FailedPrecondition; every other DeviceMapper error is Internal.
var ErrLocalDeviceUnavailable = errors.New("local backend device unavailable")

// DeviceMapper manages the device-mapper linear target through which a local
// attach uses the backend device.  The target keeps an exclusive kernel claim
// on the backend device for as long as the volume is staged — surviving
// node-plugin and kubelet death — which the agent checks before it re-enables
// the network export (see docs of SetLocalAttach).
type DeviceMapper interface {
	// EnsureLinear creates the linear target name over the whole of
	// backingDevice, or verifies that an existing target of that name maps
	// the same device, and ensures its device node exists.  It returns the
	// path of the device-mapper block device.  A missing or non-block
	// backingDevice yields an error wrapping ErrLocalDeviceUnavailable.
	EnsureLinear(ctx context.Context, name, backingDevice string) (string, error)

	// ReloadLinear reloads the table of the existing target name to the
	// current size of backingDevice and resumes it, so an expanded backend
	// becomes visible through the target.
	ReloadLinear(ctx context.Context, name, backingDevice string) error

	// Remove removes the target name.  An absent target is success.
	Remove(ctx context.Context, name string) error
}

// LocalDMName returns the device-mapper name of the local attach of
// volumeID: "pillar-local-" followed by the first 16 hex characters of
// sha256(volumeID).  The hash keeps the name within the kernel's DM name
// limit and free of characters (such as "/") that volume IDs contain.
func LocalDMName(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return localDMPrefix + hex.EncodeToString(sum[:])[:localDMHashLen]
}

// WithDeviceMapper replaces the DeviceMapper used by local attach and returns
// the receiver.
//
//	srv := NewNodeServer(nodeID, handlers, mnt).WithDeviceMapper(NewExecDeviceMapper())
func (n *NodeServer) WithDeviceMapper(dm DeviceMapper) *NodeServer {
	n.dm = dm
	return n
}

// deviceMapper returns the configured DeviceMapper, defaulting to dmsetup.
func (n *NodeServer) deviceMapper() DeviceMapper {
	if n.dm == nil {
		return NewExecDeviceMapper()
	}
	return n.dm
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeServer local attach steps
// ─────────────────────────────────────────────────────────────────────────────

// localAttachRequest inspects the PublishContext of NodeStageVolume.  It
// reports whether the publish is a local attach and, if so, the backend
// device path.  A local attach published for another node is refused with
// FailedPrecondition: only the storage node can reach the backend device, and
// attaching it anywhere else would bypass the export fencing.
func (n *NodeServer) localAttachRequest(pubCtx map[string]string) (isLocal bool, devicePath string, err error) {
	mode := pubCtx[PublishContextKeyAttachMode]
	if mode == "" {
		return false, "", nil
	}
	if mode != AttachModeLocal {
		return false, "", status.Errorf(codes.InvalidArgument,
			"NodeStageVolume: publish_context %q has unknown value %q", PublishContextKeyAttachMode, mode)
	}
	localNode := pubCtx[PublishContextKeyLocalNode]
	if localNode != n.nodeID {
		return false, "", status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: volume published for local attach on node %q, but this node is %q",
			localNode, n.nodeID)
	}
	device := pubCtx[PublishContextKeyLocalDevicePath]
	if device == "" {
		return false, "", status.Errorf(codes.InvalidArgument,
			"NodeStageVolume: publish_context missing required key %q for local attach",
			PublishContextKeyLocalDevicePath)
	}
	return true, device, nil
}

// attachLocal claims backingDevice for volumeID through its device-mapper
// linear target and returns the path of the device-mapper device.
//
// After the claim exists it reads the network export state of targetID:
// the nvmet namespace enable state of the NVMe-oF subsystem (see
// nvmet_export_state.go) or, for protocolType "iscsi", the presence of LUN 0
// of the LIO target (see lio_export_state.go).  A live export means remote
// initiators may still be served; the stage is then rolled back (see
// abortLocal) and refused with FailedPrecondition.  A failed check likewise
// rolls the stage back.  The rollback unmounts the staged surface named by
// stagingPath and volCap before it releases the claim.
func (n *NodeServer) attachLocal(
	ctx context.Context,
	volumeID, protocolType, targetID, backingDevice, stagingPath string,
	volCap *csi.VolumeCapability,
) (string, error) {
	name := LocalDMName(volumeID)
	dmPath, err := n.deviceMapper().EnsureLinear(ctx, name, backingDevice)
	if err != nil {
		code := codes.Internal
		if errors.Is(err, ErrLocalDeviceUnavailable) {
			code = codes.FailedPrecondition
		}
		return "", status.Errorf(code,
			"NodeStageVolume: local attach volume %q: device-mapper %s over %q: %v",
			volumeID, name, backingDevice, err)
	}

	root, kind, unit := n.nvmetConfigfsRoot(), "nvmet subsystem", "namespace(s)"
	check := enabledNvmetNamespaces
	if protocolType == ProtocolISCSI {
		root, kind, unit = n.lioConfigfsRoot(), "LIO iSCSI target", "LUN(s)"
		check = enabledLIOLUNs
	}
	enabled, checkErr := check(root, targetID)
	if checkErr != nil {
		return "", n.abortLocal(ctx, volumeID, stagingPath, volCap, status.Errorf(codes.Internal,
			"NodeStageVolume: local attach volume %q: verify network export %s under %s is disabled: %v",
			volumeID, targetID, root, checkErr))
	}
	if len(enabled) > 0 {
		return "", n.abortLocal(ctx, volumeID, stagingPath, volCap, status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: network export of volume %q is still serving remote initiators "+
				"(%s %s %s %s enabled)",
			volumeID, kind, targetID, unit, strings.Join(enabled, ",")))
	}
	return dmPath, nil
}

// abortLocal rolls back a local stage of volumeID that failed past
// EnsureLinear, so the device-mapper claim never outlives a failed stage
// and no mount ever references a removed device-mapper device.
//
// The staged surface of volCap (stagingPath for MOUNT, the block sentinel
// file for BLOCK; see stageBindTarget) is unmounted first: a bind of the
// device node does not hold the dm device open, so removing the claim under
// a live bind would succeed and leave the bind on a dead dev_t, and a
// filesystem mount would make the removal fail busy.  Unmount is
// idempotent, so a surface this call never mounted is a no-op.  When the
// unmount fails the claim is kept — it is safe under a live mount — and so
// is any stage state file, which lets NodeUnstageVolume tear both down.
//
// After the claim is removed the stage state file is deleted, so the next
// attempt starts clean and no record outlives its claim; a failed removal
// keeps the file as the record of the remaining claim.
//
// It returns cause, joined with every rollback failure; the gRPC code of
// cause is kept.
func (n *NodeServer) abortLocal(
	ctx context.Context,
	volumeID, stagingPath string,
	volCap *csi.VolumeCapability,
	cause error,
) error {
	// cause is the gRPC status of the failed stage; Join keeps its code
	// (status.FromError unwraps) and appends the rollback failures.
	surface := stageBindTarget(stagingPath, volCap)
	unmountErr := n.mounter.Unmount(surface)
	if unmountErr != nil {
		return errors.Join(cause, //nolint:wrapcheck // both operands are wrapped/annotated
			fmt.Errorf("unmount %q of volume %q after the failed stage (device-mapper %s kept): %w",
				surface, volumeID, LocalDMName(volumeID), unmountErr))
	}
	if volCap.GetBlock() != nil {
		// Like NodeUnstageVolume: drop the regular-file sentinel so the
		// kubelet's rmdir of stagingPath is not refused.
		_ = os.Remove(surface) //nolint:errcheck // best-effort cleanup of an unmounted placeholder
	}

	name := LocalDMName(volumeID)
	rmErr := n.deviceMapper().Remove(ctx, name)
	if rmErr != nil {
		return errors.Join(cause, //nolint:wrapcheck // both operands are wrapped/annotated
			fmt.Errorf("release device-mapper %s of volume %q after the failed stage: %w", name, volumeID, rmErr))
	}
	deleteErr := n.deleteStageState(volumeID)
	if deleteErr != nil {
		return errors.Join(cause, //nolint:wrapcheck // both operands are wrapped/annotated
			fmt.Errorf("delete stage state of volume %q after the failed stage: %w", volumeID, deleteErr))
	}
	return cause
}

// removeOrphanLocal removes the device-mapper target a local stage of
// volumeID left without a stage state file (the claim's removal failed on a
// failed stage, or the state directory was lost).  The target name is
// deterministic, so no state is needed to find it.  An absent target is
// success.
func (n *NodeServer) removeOrphanLocal(ctx context.Context, volumeID string) error {
	name := LocalDMName(volumeID)
	present, err := n.dmTargetPresent(name)
	if err != nil {
		return status.Errorf(codes.Internal,
			"NodeUnstageVolume: stage state for %q is missing; look up orphan device-mapper %s: %v",
			volumeID, name, err)
	}
	if !present {
		return nil
	}
	rmErr := n.deviceMapper().Remove(ctx, name)
	if rmErr != nil {
		return status.Errorf(codes.Internal,
			"NodeUnstageVolume: stage state for %q is missing; remove orphan device-mapper %s: %v",
			volumeID, name, rmErr)
	}
	return nil
}

// dmTargetPresent reports whether device-mapper target name exists.
func (n *NodeServer) dmTargetPresent(name string) (bool, error) {
	if n.dmTargetPresentFn != nil {
		return n.dmTargetPresentFn(name)
	}
	return dmTargetInSysfs(dmSysfsBlockRoot, name)
}

// releaseLocal removes the device-mapper target of a local attach, releasing
// the exclusive claim on the backend device.
func (n *NodeServer) releaseLocal(ctx context.Context, volumeID string, state *nodeStageState) error {
	if state.Local == nil || state.Local.DMName == "" {
		return status.Errorf(codes.Internal,
			"NodeUnstageVolume: stage state for %q is a local attach without a device-mapper name", volumeID)
	}
	err := n.deviceMapper().Remove(ctx, state.Local.DMName)
	if err != nil {
		return status.Errorf(codes.Internal,
			"NodeUnstageVolume: remove device-mapper %s of volume %q: %v", state.Local.DMName, volumeID, err)
	}
	return nil
}

// expandLocal grows the device-mapper target of a local attach to the
// current size of its backend device.
func (n *NodeServer) expandLocal(ctx context.Context, volumeID string, state *nodeStageState) error {
	if state.Local == nil || state.Local.DMName == "" || state.Local.BackingDevice == "" {
		return status.Errorf(codes.Internal,
			"NodeExpandVolume: stage state for %q is a local attach without a device-mapper target", volumeID)
	}
	err := n.deviceMapper().ReloadLinear(ctx, state.Local.DMName, state.Local.BackingDevice)
	if err != nil {
		return status.Errorf(codes.Internal,
			"NodeExpandVolume: reload device-mapper %s over %q of volume %q: %v",
			state.Local.DMName, state.Local.BackingDevice, volumeID, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// execDeviceMapper — production DeviceMapper backed by dmsetup(8)
// ─────────────────────────────────────────────────────────────────────────────

// backingDeviceInfo is the identity and size of a backend block device.
type backingDeviceInfo struct {
	major, minor uint32
	sectors      int64
}

// execDeviceMapper implements DeviceMapper with dmsetup(8).  Udev does not
// run inside the node container, so every invocation passes --noudevsync and
// DM_DISABLE_UDEV=1 (libdevmapper then manages /dev/mapper itself), and the
// device node is additionally verified against sysfs.
type execDeviceMapper struct {
	// run executes dmsetup with the given arguments and returns its
	// combined output.
	run func(ctx context.Context, args ...string) ([]byte, error)
	// probe returns the dev_t and size of a backend block device.
	probe func(path string) (backingDeviceInfo, error)
	// ensureNode makes /dev/mapper/<name> a block device node matching the
	// kernel's dev_t for the target and returns its path.
	ensureNode func(name string) (string, error)
}

// NewExecDeviceMapper returns the production DeviceMapper backed by dmsetup.
func NewExecDeviceMapper() DeviceMapper {
	return &execDeviceMapper{
		run:        runDmsetup,
		probe:      probeBackingDevice,
		ensureNode: ensureDMDeviceNode,
	}
}

// EnsureLinear implements DeviceMapper.
func (d *execDeviceMapper) EnsureLinear(ctx context.Context, name, backingDevice string) (string, error) {
	info, err := d.probe(backingDevice)
	if err != nil {
		return "", err
	}

	existing, found, err := d.table(ctx, name)
	if err != nil {
		return "", err
	}
	if found {
		verifyErr := verifyLinearTable(name, existing, info)
		if verifyErr != nil {
			return "", verifyErr
		}
	} else {
		want := linearTable(info)
		out, createErr := d.run(ctx, "create", name, "--table", want)
		if createErr != nil {
			return "", fmt.Errorf("dmsetup create %s (table %q): %w: %s",
				name, want, createErr, strings.TrimSpace(string(out)))
		}
		// Read back: the target must now exist and map the backend device.
		created, ok, readErr := d.table(ctx, name)
		if readErr != nil {
			return "", readErr
		}
		if !ok {
			return "", fmt.Errorf("dmsetup create %s succeeded but the device does not exist", name)
		}
		verifyErr := verifyLinearTable(name, created, info)
		if verifyErr != nil {
			return "", verifyErr
		}
	}

	return d.ensureNode(name)
}

// ReloadLinear implements DeviceMapper.
func (d *execDeviceMapper) ReloadLinear(ctx context.Context, name, backingDevice string) error {
	info, err := d.probe(backingDevice)
	if err != nil {
		return err
	}
	existing, found, err := d.table(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("reload device-mapper %s: device does not exist", name)
	}
	verifyErr := verifyLinearTable(name, existing, info)
	if verifyErr != nil {
		return verifyErr
	}

	want := linearTable(info)
	if strings.TrimSpace(existing) == want {
		return nil // already at the backend size
	}
	out, reloadErr := d.run(ctx, "reload", name, "--table", want)
	if reloadErr != nil {
		return fmt.Errorf("dmsetup reload %s (table %q): %w: %s",
			name, want, reloadErr, strings.TrimSpace(string(out)))
	}
	out, resumeErr := d.run(ctx, "resume", name)
	if resumeErr != nil {
		return fmt.Errorf("dmsetup resume %s: %w: %s", name, resumeErr, strings.TrimSpace(string(out)))
	}

	reloaded, ok, readErr := d.table(ctx, name)
	if readErr != nil {
		return readErr
	}
	if !ok || strings.TrimSpace(reloaded) != want {
		return fmt.Errorf("reload device-mapper %s: live table %q after resume, want %q",
			name, strings.TrimSpace(reloaded), want)
	}
	return nil
}

// Remove implements DeviceMapper.
func (d *execDeviceMapper) Remove(ctx context.Context, name string) error {
	_, found, err := d.table(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	out, removeErr := d.run(ctx, "remove", name)
	if removeErr != nil {
		if dmNotFound(out) {
			return nil
		}
		return fmt.Errorf("dmsetup remove %s: %w: %s", name, removeErr, strings.TrimSpace(string(out)))
	}
	// Read back: the claim on the backend device is released only when the
	// target is really gone.
	_, stillThere, readErr := d.table(ctx, name)
	if readErr != nil {
		return readErr
	}
	if stillThere {
		return fmt.Errorf("dmsetup remove %s succeeded but the device still exists", name)
	}
	return nil
}

// table returns the live table of target name and whether the target exists.
func (d *execDeviceMapper) table(ctx context.Context, name string) (text string, exists bool, err error) {
	out, err := d.run(ctx, "table", name)
	if err != nil {
		if dmNotFound(out) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("dmsetup table %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), true, nil
}

// linearTable returns the single-segment linear table mapping all of info.
func linearTable(info backingDeviceInfo) string {
	return fmt.Sprintf("0 %d linear %d:%d 0", info.sectors, info.major, info.minor)
}

// verifyLinearTable checks that table is a single linear segment over the
// device described by info.  A target that maps anything else belongs to
// someone else (or a stale volume) and must not be reused.
func verifyLinearTable(name, table string, info backingDeviceInfo) error {
	lines := strings.Split(strings.TrimSpace(table), "\n")
	fields := strings.Fields(lines[0])
	wantDev := fmt.Sprintf("%d:%d", info.major, info.minor)
	if len(lines) != 1 || len(fields) != 5 || fields[0] != "0" || fields[2] != "linear" ||
		fields[3] != wantDev || fields[4] != "0" {
		return fmt.Errorf("device-mapper %s has table %q, want a linear map over %s",
			name, strings.TrimSpace(table), wantDev)
	}
	return nil
}

// dmNotFound reports whether dmsetup output says the target does not exist.
func dmNotFound(out []byte) bool {
	msg := strings.ToLower(string(out))
	return strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "not found")
}

// runDmsetup runs dmsetup without udev synchronization.  Every run is
// observed in M9 and, inside a traced RPC, as an SP8 child span.
func runDmsetup(ctx context.Context, args ...string) ([]byte, error) {
	bin := findExecutable("dmsetup", "/usr/sbin/dmsetup", "/sbin/dmsetup")
	argv := append([]string{"--noudevsync"}, args...)
	obs := telemetry.StartExec(ctx, bin, argv...)
	cmd := exec.CommandContext(ctx, bin, argv...) //nolint:gosec // fixed binary, internal args
	cmd.Env = append(os.Environ(), "DM_DISABLE_UDEV=1")
	out, err := cmd.CombinedOutput()
	obs.End(out, err)
	if err != nil {
		return out, fmt.Errorf("exec %s: %w", bin, err)
	}
	return out, nil
}

// probeBackingDevice stats path (following symlinks such as those under
// /dev/zvol) and returns its dev_t and size.  A missing or non-block path
// wraps ErrLocalDeviceUnavailable.
func probeBackingDevice(path string) (backingDeviceInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return backingDeviceInfo{}, fmt.Errorf("stat %q: %w: %w", path, ErrLocalDeviceUnavailable, err)
		}
		return backingDeviceInfo{}, fmt.Errorf("stat %q: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return backingDeviceInfo{}, fmt.Errorf("%q (mode %s): not a block device: %w",
			path, fi.Mode(), ErrLocalDeviceUnavailable)
	}
	major, minor := linuxDevMajorMinor(uint64(st.Rdev)) //nolint:gosec,unconvert,nolintlint // Rdev width differs by OS

	size, err := linuxBlockDeviceSize(path)
	if err != nil {
		return backingDeviceInfo{}, err
	}
	if size <= 0 || size%sectorSize != 0 {
		return backingDeviceInfo{}, fmt.Errorf("block device %q reports size %d bytes, not a positive multiple of %d",
			path, size, sectorSize)
	}
	return backingDeviceInfo{major: major, minor: minor, sectors: size / sectorSize}, nil
}

// linuxDevMajorMinor splits a Linux dev_t (glibc gnu_dev_major/minor).
func linuxDevMajorMinor(dev uint64) (major, minor uint32) {
	major = uint32(((dev >> 8) & 0xfff) | ((dev >> 32) &^ 0xfff)) //nolint:gosec // G115: dev_t bit-unpacking
	minor = uint32((dev & 0xff) | ((dev >> 12) &^ 0xff))          //nolint:gosec // G115: dev_t bit-unpacking
	return major, minor
}

// linuxMkdev packs major:minor into a Linux dev_t (glibc gnu_dev_makedev).
func linuxMkdev(major, minor uint32) uint64 {
	maj, mnr := uint64(major), uint64(minor)
	return (mnr & 0xff) | ((maj & 0xfff) << 8) | ((mnr &^ 0xff) << 12) | ((maj &^ 0xfff) << 32)
}

// dmSysfsBlockRoot is where the kernel lists block devices; device-mapper
// devices appear as dm-N with their name in dm/name.
const dmSysfsBlockRoot = "/sys/block"

// dmDevRoot is the directory of device-mapper device nodes.
const dmDevRoot = "/dev/mapper"

// ensureDMDeviceNode finds target name in sysfs and makes /dev/mapper/<name>
// a block device node with the kernel's current dev_t, replacing a stale
// node.  The container's /dev has no udev, so libdevmapper's own node
// handling is not trusted blindly; the result is verified by stat.
func ensureDMDeviceNode(name string) (string, error) {
	nodePath, err := dmDeviceNodePath(name)
	if err != nil {
		return "", err
	}
	major, minor, err := dmDevFromSysfs(dmSysfsBlockRoot, name)
	if err != nil {
		return "", err
	}
	dev := linuxMkdev(major, minor)

	matches, statErr := dmDeviceNodeMatches(nodePath, dev)
	if statErr == nil {
		if matches {
			return nodePath, nil
		}
		rmErr := os.Remove(nodePath)
		if rmErr != nil && !os.IsNotExist(rmErr) {
			return "", fmt.Errorf("remove stale device node %s: %w", nodePath, rmErr)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("device node %s: %w", nodePath, statErr)
	}

	mkdirErr := os.MkdirAll(dmDevRoot, 0o755) //nolint:gosec,mnd // standard /dev/mapper mode
	if mkdirErr != nil {
		return "", fmt.Errorf("create %s: %w", dmDevRoot, mkdirErr)
	}
	mknodErr := syscall.Mknod(nodePath, syscall.S_IFBLK|0o600, int(dev)) //nolint:gosec // G115: dev_t fits int on Linux
	if mknodErr != nil && !os.IsExist(mknodErr) {
		return "", fmt.Errorf("mknod %s (%d:%d): %w", nodePath, major, minor, mknodErr)
	}

	matches, statErr = dmDeviceNodeMatches(nodePath, dev)
	if statErr != nil {
		return "", fmt.Errorf("device node %s after mknod: %w", nodePath, statErr)
	}
	if !matches {
		return "", fmt.Errorf("device node %s does not match %d:%d after mknod", nodePath, major, minor)
	}
	return nodePath, nil
}

// dmDeviceNodePath joins dmDevRoot with name and refuses a name that could
// escape the device-mapper root: DM names are bare basenames (never a path
// separator or "..").
func dmDeviceNodePath(name string) (string, error) {
	nodePath := filepath.Join(dmDevRoot, name)
	if filepath.Dir(nodePath) != dmDevRoot {
		return "", fmt.Errorf("device-mapper name %q escapes %s", name, dmDevRoot)
	}
	return nodePath, nil
}

// dmDeviceNodeMatches reports whether path is a block device node with
// dev_t dev.  The os.Stat error is returned instead: not-exist means the
// node is absent, anything else must be reported to the caller.
func dmDeviceNodeMatches(path string, dev uint64) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	matches := ok && st.Mode&syscall.S_IFMT == syscall.S_IFBLK &&
		uint64(st.Rdev) == dev //nolint:gosec,unconvert,nolintlint // Rdev width differs by OS
	return matches, nil
}

// dmDevFromSysfs returns the major:minor of device-mapper target name by
// scanning <blockRoot>/dm-*/dm/name.
func dmDevFromSysfs(blockRoot, name string) (major, minor uint32, err error) {
	dir, err := findDMSysfsDir(blockRoot, name)
	if err != nil {
		return 0, 0, err
	}
	if dir == "" {
		return 0, 0, fmt.Errorf("device-mapper %s not found under %s", name, blockRoot)
	}
	devFile := filepath.Join(dir, "dev")
	devBytes, devErr := os.ReadFile(devFile) //nolint:gosec // sysfs path
	if devErr != nil {
		return 0, 0, fmt.Errorf("read %s: %w", devFile, devErr)
	}
	return parseMajorMinor(devFile, strings.TrimSpace(string(devBytes)))
}

// dmTargetInSysfs reports whether device-mapper target name exists under
// blockRoot.  A missing blockRoot means no block device — and therefore no
// target — is visible, which is reported as absent.
func dmTargetInSysfs(blockRoot, name string) (bool, error) {
	_, statErr := os.Stat(blockRoot)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", blockRoot, statErr)
	}
	dir, err := findDMSysfsDir(blockRoot, name)
	if err != nil {
		return false, err
	}
	return dir != "", nil
}

// findDMSysfsDir returns the <blockRoot>/dm-N directory whose dm/name is
// name, or "" when no such target exists.
func findDMSysfsDir(blockRoot, name string) (string, error) {
	entries, err := os.ReadDir(blockRoot)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", blockRoot, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "dm-") {
			continue
		}
		nameFile := filepath.Join(blockRoot, e.Name(), "dm", "name")
		got, readErr := os.ReadFile(nameFile) //nolint:gosec // sysfs path
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue // removed while scanning
			}
			return "", fmt.Errorf("read %s: %w", nameFile, readErr)
		}
		if strings.TrimSpace(string(got)) == name {
			return filepath.Join(blockRoot, e.Name()), nil
		}
	}
	return "", nil
}

// parseMajorMinor parses a "major:minor" sysfs dev value read from source.
func parseMajorMinor(source, value string) (major, minor uint32, err error) {
	majStr, minStr, ok := strings.Cut(value, ":")
	if !ok {
		return 0, 0, fmt.Errorf("malformed dev %q in %s", value, source)
	}
	maj64, majErr := strconv.ParseUint(majStr, 10, 32)
	if majErr != nil {
		return 0, 0, fmt.Errorf("parse major %q in %s: %w", value, source, majErr)
	}
	min64, minErr := strconv.ParseUint(minStr, 10, 32)
	if minErr != nil {
		return 0, 0, fmt.Errorf("parse minor %q in %s: %w", value, source, minErr)
	}
	return uint32(maj64), uint32(min64), nil
}
