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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// ─────────────────────────────────────────────────────────────────────────────
// Resizer interface
// ─────────────────────────────────────────────────────────────────────────────.

// Resizer is the interface for online filesystem expand operations.
// A real implementation shells out to resize2fs(8) or xfs_growfs(8).
// A test implementation records calls without touching the filesystem.
type Resizer interface {
	// ResizeFS expands the filesystem at mountPath to fill the current extent
	// of the underlying block device.  fsType controls which resize tool is
	// used; supported values are "ext4" (also "ext3"/"ext2") and "xfs".
	// Implementations should return a non-nil error when the resize tool
	// exits with a non-zero status or the filesystem type is unsupported.
	ResizeFS(ctx context.Context, mountPath, fsType string) error
}

// ─────────────────────────────────────────────────────────────────────────────
// WithResizer
// ─────────────────────────────────────────────────────────────────────────────.

// WithResizer replaces the Resizer used by NodeExpandVolume and returns the
// receiver.  Call this during construction in tests to inject a mock resizer
// without needing real resize tools or a formatted block device.
//
//	srv := NewNodeServerWithStateDir(nodeID, conn, mnt, dir).WithResizer(mock)
func (n *NodeServer) WithResizer(r Resizer) *NodeServer {
	n.resizer = r
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeExpandVolume
// ─────────────────────────────────────────────────────────────────────────────.

// NodeExpandVolume runs the filesystem-specific resize tool so the filesystem
// fills the newly expanded block device after a ControllerExpandVolume call.
//
// The CO calls this RPC on the node where the volume is staged.  It passes the
// volume_path (staging or target mount point) and optionally the
// volume_capability so the node plugin knows the filesystem type.
//
// Filesystem-specific behavior:
//   - ext2 / ext3 / ext4: finds the block device backing volume_path by
//     parsing /proc/mounts, then runs `resize2fs <device>` for an online
//     resize.
//   - xfs: runs `xfs_growfs <volume_path>` directly on the mount point
//     (xfs_growfs requires the mount point, not the device).
//
// Capability: NodeServiceCapability_RPC_EXPAND_VOLUME must be advertised in
// NodeGetCapabilities for the CO to invoke this RPC.
func (n *NodeServer) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest,
) (*csi.NodeExpandVolumeResponse, error) {
	telemetry.SetVolumeAttributes(ctx, req.GetVolumeId())
	// The file driver does not advertise EXPAND_VOLUME: an adopted
	// filesystem's admitted quota is immutable, so a stray call is refused.
	if n.effectiveDriverName() == v1alpha1.FileCSIDriver {
		return nil, status.Error(codes.Unimplemented, //nolint:wrapcheck
			"NodeExpandVolume: adopted filesystems do not support expansion")
	}

	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: volume_id is required") //nolint:wrapcheck
	}
	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: volume_path is required") //nolint:wrapcheck
	}

	// Serialize with NodeStageVolume, NodeUnstageVolume and the periodic
	// trim of the same volume (see trim.go).
	unlock := n.volumeLocks.lock(req.GetVolumeId())
	defer unlock()

	// CSI spec §4.16: NodeExpandVolume must return NotFound when the volume
	// the CO references does not exist on this node — i.e. nothing is mounted
	// at volume_path.  Without this check the call falls through to the
	// resizer, which returns Internal for an absent device and breaks the
	// "should fail when volume is not found" sanity test.  We accept the
	// path as "present" if it exists at all (file or directory), matching
	// the staging-target semantics; the resizer below handles the mount
	// lookup and surfaces a real Internal error when the path exists but
	// isn't a mount point.
	fi, err := os.Stat(volumePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound,
				"NodeExpandVolume: volume_path %q does not exist", volumePath)
		}
		return nil, status.Errorf(codes.Internal,
			"NodeExpandVolume: stat %q: %v", volumePath, err)
	}

	stageState, done, err := n.prepareNodeExpansion(ctx, req, fi)
	if err != nil {
		return nil, err
	}
	if done {
		return &csi.NodeExpandVolumeResponse{CapacityBytes: blockExpandCapacity(req)}, nil
	}

	// ── Determine filesystem type ────────────────────────────────────────────
	fsType := expandFsType(stageState, req.GetVolumeCapability())
	trace.SpanFromContext(ctx).SetAttributes(telemetry.KeyFSType.String(fsType))

	// ── Run filesystem resize ────────────────────────────────────────────────
	r := n.resizer
	if r == nil {
		r = &execResizer{}
	}

	resizeErr := r.ResizeFS(ctx, volumePath, fsType)
	if resizeErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeExpandVolume: resize %s filesystem at %q: %v", fsType, volumePath, resizeErr)
	}

	// ── Return capacity ──────────────────────────────────────────────────────
	// Echo back the required_bytes from the capacity_range when provided so
	// the CO can update the PersistentVolume capacity field.  A zero value
	// means "fill the available block device capacity" — the CO accepts this.
	return &csi.NodeExpandVolumeResponse{CapacityBytes: blockExpandCapacity(req)}, nil
}

func (n *NodeServer) prepareNodeExpansion(
	ctx context.Context, req *csi.NodeExpandVolumeRequest, fi os.FileInfo,
) (*nodeStageState, bool, error) {
	stageState, err := n.growStagedDevice(ctx, req.GetVolumeId())
	if err != nil {
		return nil, false, err
	}
	if stageState != nil && stageState.File != nil {
		return nil, false, status.Errorf(
			codes.FailedPrecondition,
			"%s",
			"NodeExpandVolume: adopted filesystem expansion is unsupported",
		)
	}
	if stageState != nil && stageState.ProtocolType == ProtocolNFS {
		return stageState, true, nil
	}
	volCap := req.GetVolumeCapability()
	if (volCap != nil && volCap.GetBlock() != nil) ||
		(volCap == nil && !fi.IsDir()) {
		return stageState, true, nil
	}
	return stageState, false, nil
}

// growStagedDevice makes an online resize of the staged volume visible to the
// block layer and returns the stage state (nil when the volume has none).
func (n *NodeServer) growStagedDevice(ctx context.Context, volumeID string) (*nodeStageState, error) {
	stageState, stateErr := n.readStageState(volumeID)
	if stateErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeExpandVolume: read stage state for %q: %v", volumeID, stateErr)
	}
	setSpanAttachMode(ctx, stageState)
	if stageState != nil && stageState.PreserveOriginal {
		// The preserve-original pin refuses expansion from the stage
		// record alone — NodeExpandVolume carries no VolumeContext — in
		// the same process and after a plugin restart, before any device
		// reload, rescan or resize runs.
		return nil, preserveOriginalRefusal(volumeID)
	}
	switch {
	case stageState.isLocalAttach():
		// ── Local attach: grow the device-mapper target ─────────────────────
		// A local attach presents the backend device through a device-mapper
		// linear target whose table fixes its length, so the target must be
		// reloaded to the backend's new size before the block device (and any
		// filesystem on it) can grow.  No NVMe rescan is involved.
		expandErr := n.expandLocal(ctx, volumeID, stageState)
		if expandErr != nil {
			return nil, expandErr
		}
	case stageState != nil && stageState.ProtocolType == ProtocolISCSI:
		// ── iSCSI: rescan the LUN ───────────────────────────────────────────
		// Unlike NVMe-oF (asynchronous namespace-change event plus the
		// controller rescan in ResizeFS), the SCSI midlayer only logs the
		// target's "capacity data has changed" unit attention; the new size
		// reaches /dev/sdX only after an explicit device rescan.  Run it
		// before both the Block-mode short-circuit and the filesystem resize.
		// A SCSI rescan keeps the disk's dev_t, so no device-node refresh
		// (see refreshNVMeBlockDeviceNode) is needed afterwards.
		rescanErr := n.rescanProtocol(ctx, volumeID, stageState)
		if rescanErr != nil {
			return nil, rescanErr
		}
	}
	return stageState, nil
}

// preserveOriginalRefusal reports a FailedPrecondition for any expansion of
// a preserve-original volume: the adopted LV's extent and data must never
// change, so the refusal fires before the local-attach reload, the protocol
// rescan, the block-mode short-circuit and the filesystem resize.
func preserveOriginalRefusal(volumeID string) error {
	return status.Errorf(codes.FailedPrecondition,
		"NodeExpandVolume: volume %q is preserve-original and is never expanded", volumeID)
}

// rescanProtocol asks the ProtocolHandler of the staged volume to rescan its
// device so an online resize becomes visible to the block layer.
func (n *NodeServer) rescanProtocol(ctx context.Context, volumeID string, state *nodeStageState) error {
	handler := n.handlers[state.ProtocolType]
	if handler == nil {
		return status.Errorf(codes.FailedPrecondition,
			"NodeExpandVolume: no handler registered for protocol %q%s",
			state.ProtocolType, missingHandlerHint(state.ProtocolType))
	}
	protoState, err := state.ToProtocolState()
	if err != nil {
		return status.Errorf(codes.Internal,
			"NodeExpandVolume: convert stage state for %q: %v", volumeID, err)
	}
	rescanErr := handler.Rescan(ctx, protoState)
	if rescanErr != nil {
		return status.Errorf(codes.Internal,
			"NodeExpandVolume: rescan volume %q (protocol %q): %v", volumeID, state.ProtocolType, rescanErr)
	}
	return nil
}

// expandFsType returns the filesystem type NodeExpandVolume resizes.  The
// type NodeStageVolume recorded in the stage state wins: a filesystem document
// can format the volume with a type other than the PV's csi.fsType, and
// NodeExpandVolume carries no VolumeContext.  Volumes staged before the stage
// state recorded the type fall back to the VolumeCapability, then to the
// project default (ext4).
func expandFsType(stageState *nodeStageState, volCap *csi.VolumeCapability) string {
	if stageState != nil && stageState.FsType != "" {
		return stageState.FsType
	}
	if fsType := volCap.GetMount().GetFsType(); fsType != "" {
		return fsType
	}
	return defaultFsType
}

// blockExpandCapacity echoes required_bytes from the request's capacity_range,
// or 0 when the CO omitted it.  A zero capacity tells the CO to fill the
// available block-device capacity.
func blockExpandCapacity(req *csi.NodeExpandVolumeRequest) int64 {
	if cr := req.GetCapacityRange(); cr != nil {
		return cr.GetRequiredBytes()
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// execResizer — production Resizer backed by os/exec
// ─────────────────────────────────────────────────────────────────────────────.

// execResizer is the default Resizer used in production.  It shells out to the
// filesystem-specific resize tool (resize2fs or xfs_growfs).
type execResizer struct{}

// ResizeFS implements Resizer for execResizer.
//
// Before performing the filesystem resize, ResizeFS triggers an NVMe
// controller rescan when the backing device is an NVMe namespace
// (/dev/nvmeXnY).  This ensures the kernel block layer reflects any size
// changes made by the remote NVMe-oF target after ControllerExpandVolume.
// Non-NVMe devices are unaffected.
//
// Ext4 (and ext3/ext2):
//
//	Parses /proc/mounts to find the block device that backs mountPath, then
//	runs: resize2fs <device>
//
// xfs:
//
//	Runs: xfs_growfs <mountPath>
//
//	xfs_growfs operates on the mount point; it communicates with the kernel
//	XFS driver directly via ioctl and does not need the raw device path.
//
// Each resize tool run is observed in M9 and, inside a traced RPC, as an SP8
// child span.
func (*execResizer) ResizeFS(ctx context.Context, mountPath, fsType string) error {
	// Reject unsupported filesystem types early, before doing any I/O.
	switch fsType {
	case defaultFsType, "ext3", "ext2", xfsFsType:
		// supported — proceed below
	default:
		return fmt.Errorf("unsupported filesystem type %q: only ext4 and xfs are supported for online resize", fsType)
	}

	// Find the block device backing this mount — needed both for the NVMe
	// rescan check and for resize2fs (ext4).
	device, err := deviceFromMount(mountPath)
	if err != nil {
		return fmt.Errorf("find block device for mount %q: %w", mountPath, err)
	}

	// If the backing device is NVMe, trigger a controller rescan so the
	// kernel block layer picks up size changes from the remote target.
	rescanNVMeDevice(device)

	// After the rescan, the kernel may have re-registered the namespace
	// with a fresh dev_t (same name, new major:minor) — common when the
	// target re-publishes the namespace as part of the size update.  In
	// that case the existing /dev/<dev> node still carries the *old*
	// major:minor and open(2) returns ENXIO ("No such device or address").
	// Re-mknod the node from the current sysfs dev so resize2fs can open
	// the live kernel block device.  Best-effort: errors fall through to
	// resize2fs which will surface the real problem.
	refreshNVMeBlockDeviceNode(device)

	switch fsType {
	case defaultFsType, "ext3", "ext2":
		resize2fs := findExecutable("resize2fs", "/usr/sbin/resize2fs", "/sbin/resize2fs")
		out, cmdErr := runObserved(ctx, resize2fs, device)
		if cmdErr != nil {
			return fmt.Errorf("resize2fs %q: %w: %s", device, cmdErr, strings.TrimSpace(string(out)))
		}

	case xfsFsType:
		xfsGrowfs := findExecutable("xfs_growfs", "/usr/sbin/xfs_growfs", "/sbin/xfs_growfs")
		out, cmdErr := runObserved(ctx, xfsGrowfs, mountPath)
		if cmdErr != nil {
			return fmt.Errorf("xfs_growfs %q: %w: %s", mountPath, cmdErr, strings.TrimSpace(string(out)))
		}
	}

	return nil
}

// runObserved runs name with args through exec.CommandContext and returns
// its combined output, observing the run in M9 and as an SP8 span.
func runObserved(ctx context.Context, name string, args ...string) ([]byte, error) {
	obs := telemetry.StartExec(ctx, name, args...)
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // callers pass fixed tools
	obs.End(out, err)
	return out, err //nolint:wrapcheck // callers wrap with the tool name and output
}

// ─────────────────────────────────────────────────────────────────────────────
// NVMe rescan helpers
// ─────────────────────────────────────────────────────────────────────────────.

// nvmeControllerName extracts the NVMe controller name from a device path.
// For example, "/dev/nvme0n1" returns "nvme0", "/dev/nvme10n1" returns
// "nvme10".  Returns "" for non-NVMe devices (e.g. "/dev/sda").
func nvmeControllerName(device string) string {
	base := filepath.Base(device) // e.g. "nvme0n1"
	if !strings.HasPrefix(base, "nvme") {
		return ""
	}
	// NVMe device naming: nvme<ctrl>n<ns> — find the first 'n' after "nvme"
	// that separates the controller ID from the namespace ID.
	rest := base[len("nvme"):] // e.g. "0n1"
	nIdx := strings.IndexByte(rest, 'n')
	if nIdx <= 0 { // no 'n' or 'n' at position 0 means malformed
		return ""
	}
	return "nvme" + rest[:nIdx] // e.g. "nvme0"
}

// nvmeBlockDeviceSize returns the size of the given block device in bytes by
// reading /sys/block/<dev>/size (which reports 512-byte sectors).
// Returns 0 if the size cannot be determined.
func nvmeBlockDeviceSize(device string) int64 {
	base := filepath.Base(device)
	sysfsPath := "/sys/block/" + base + "/size"
	data, readErr := os.ReadFile(sysfsPath) //nolint:gosec // path derived from /proc/mounts device name
	if readErr != nil {
		return 0
	}
	sectors, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if parseErr != nil {
		return 0
	}
	return sectors * 512 //nolint:mnd // 512 is the kernel-defined sector size for /sys/block/*/size
}

// nvmeIoctlRescan is the Linux NVME_IOCTL_RESCAN constant: _IO('N', 0x46).
// Sending this ioctl to /dev/nvmeX triggers the kernel NVMe driver to
// re-identify all namespaces on the controller and update block device sizes.
const nvmeIoctlRescan = 0x4e46

// rescanNVMeDevice triggers an NVMe controller rescan for the given device so
// the kernel block layer reflects any size changes made by the remote NVMe-oF
// target (e.g. after ControllerExpandVolume extended the backing volume).
//
// The rescan uses an ioctl on the controller character device (/dev/nvmeX)
// rather than writing to sysfs, because sysfs is typically mounted read-only
// inside containers.
//
// This is a no-op for non-NVMe devices.  Best-effort: if the ioctl or size
// polling fails, the caller proceeds with resize2fs anyway (which will report
// the real error if the device is still the old size).
func rescanNVMeDevice(device string) {
	ctrl := nvmeControllerName(device)
	if ctrl == "" {
		return
	}

	origSize := nvmeBlockDeviceSize(device)

	// Send NVME_IOCTL_RESCAN via the controller character device.
	// In containerised environments (Kind, Docker) the controller char device
	// (/dev/nvmeX) may not exist in devtmpfs even though the kernel registered
	// the controller.  In that case we create the device node on-demand from
	// the major:minor numbers in sysfs.
	ctrlDev, err := ensureNVMeCtrlDev(ctrl)
	if err != nil {
		return
	}
	ctrlFd, openErr := os.OpenFile(ctrlDev, os.O_RDONLY, 0) //nolint:gosec // ctrl path
	if openErr != nil {
		return
	}
	_, _, errno := syscall.Syscall( //nolint:recvcheck // raw ioctl
		syscall.SYS_IOCTL, ctrlFd.Fd(), nvmeIoctlRescan, 0,
	)
	closeErr := ctrlFd.Close()
	if errno != 0 || closeErr != nil {
		return
	}

	// The kernel processes the rescan asynchronously via a work queue.
	// Poll until the block device size changes or a timeout expires.
	if origSize <= 0 {
		time.Sleep(time.Second)
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		newSize := nvmeBlockDeviceSize(device)
		if newSize != origSize {
			return
		}
	}
}

// refreshNVMeBlockDeviceNode rebuilds /dev/<base> from /sys/block/<base>/dev
// when the existing node's major:minor no longer matches the kernel's current
// dev_t for the namespace.  This is the block-device counterpart of
// mknodFromSysfsDev in cmd/node/main.go, but invoked from NodeExpandVolume so
// that an online expand can survive a kernel-side namespace re-registration
// (which keeps the name nvmeXnY but assigns a new minor) without resize2fs
// receiving ENXIO on open(2).
//
// This is a no-op for non-NVMe devices.  All errors are intentionally
// swallowed: the caller (ResizeFS) will surface the real failure from the
// subsequent resize2fs invocation if refreshing the node could not help.
func refreshNVMeBlockDeviceNode(device string) {
	base := filepath.Base(device)
	if !strings.HasPrefix(base, "nvme") {
		return
	}

	devFile := "/sys/block/" + base + "/dev"
	devBytes, readErr := os.ReadFile(devFile) //nolint:gosec // sysfs path derived from device basename
	if readErr != nil {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(string(devBytes)), ":", 2)
	if len(parts) != 2 {
		return
	}
	major, majErr := strconv.ParseUint(parts[0], 10, 32)
	if majErr != nil {
		return
	}
	minor, minErr := strconv.ParseUint(parts[1], 10, 32)
	if minErr != nil {
		return
	}

	// Linux makedev bit-packing — same as mknodFromSysfsDev in cmd/node/main.go.
	expectedDev := int((minor & 0xff) | ((major & 0xfff) << 8) | //nolint:gosec // G115: makedev bit-packing
		((minor &^ 0xff) << 12) | ((major &^ 0xfff) << 32))

	st, statErr := os.Stat(device)
	if statErr == nil {
		sysSt, ok := st.Sys().(*syscall.Stat_t)
		//nolint:gosec,nolintlint // G115 only applies on Linux; dev_t safely fits int.
		if ok && int(sysSt.Rdev) == expectedDev {
			return // node already matches the live kernel dev_t
		}
		rmErr := os.Remove(device)
		if rmErr != nil && !os.IsNotExist(rmErr) {
			return
		}
	}

	_ = syscall.Mknod(device, syscall.S_IFBLK|0o600, expectedDev) //nolint:errcheck // best-effort
}

// ensureNVMeCtrlDev returns the path to the NVMe controller character device
// (e.g. "/dev/nvme2") for the given controller name (e.g. "nvme2").
//
// In containerised environments the devtmpfs may not contain the controller
// char device even though the kernel registered it.  When the device node is
// missing, ensureNVMeCtrlDev reads the major:minor numbers from sysfs and
// creates the node with mknod(2).
func ensureNVMeCtrlDev(ctrl string) (string, error) {
	ctrlPath := "/dev/" + ctrl
	_, statErr := os.Stat(ctrlPath)
	if statErr == nil {
		return ctrlPath, nil // already exists
	}

	// Read major:minor from sysfs (e.g. "234:2").
	devFile := "/sys/class/nvme/" + ctrl + "/dev"
	data, readErr := os.ReadFile(devFile) //nolint:gosec // sysfs path from controller name
	if readErr != nil {
		return "", fmt.Errorf("create %s: read %s: %w", ctrlPath, devFile, readErr)
	}
	raw := strings.TrimSpace(string(data))
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("create %s: %s holds %q, want major:minor", ctrlPath, devFile, raw)
	}
	major, majErr := strconv.Atoi(parts[0])
	minor, minErr := strconv.Atoi(parts[1])
	if majErr != nil || minErr != nil {
		return "", fmt.Errorf("create %s: parse %s %q: %w", ctrlPath, devFile, raw, errors.Join(majErr, minErr))
	}

	// Create the character device node.  Requires CAP_MKNOD (privileged).
	// Use the full Linux dev_t encoding so minor numbers >= 256 work.
	devNum := (minor & 0xff) | (major << 8) | ((minor & 0xfff00) << 12)
	mknodErr := syscall.Mknod(ctrlPath, syscall.S_IFCHR|0o600, devNum)
	if mknodErr != nil && !errors.Is(mknodErr, syscall.EEXIST) {
		return "", fmt.Errorf("mknod %s (%d:%d): %w", ctrlPath, major, minor, mknodErr)
	}
	return ctrlPath, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// deviceFromMount helper
// ─────────────────────────────────────────────────────────────────────────────.

// findExecutable locates filesystem utilities that may live in non-standard
// paths inside minimal container images (e.g. /usr/sbin vs /sbin in Alpine).
func findExecutable(baseName string, candidates ...string) string {
	for _, p := range candidates {
		_, statErr := os.Stat(p)
		if statErr == nil {
			return p
		}
	}
	return baseName
}

// deviceFromMount parses /proc/mounts and returns the block device (source)
// that backs the given mountPath.  Returns an error if mountPath is not found.
//
// /proc/mounts format (space-separated):
//
//	<device> <mountpoint> <fstype> <options> <dump> <pass>
func deviceFromMount(mountPath string) (string, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("read /proc/mounts: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == mountPath {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("mount point %q not found in /proc/mounts", mountPath)
}
