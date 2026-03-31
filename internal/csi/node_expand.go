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
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
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
	ResizeFS(mountPath, fsType string) error
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
	_ context.Context,
	req *csi.NodeExpandVolumeRequest,
) (*csi.NodeExpandVolumeResponse, error) {
	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: volume_id is required") //nolint:wrapcheck
	}
	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: volume_path is required") //nolint:wrapcheck
	}

	// ── Determine filesystem type ────────────────────────────────────────────
	// Prefer the fsType from the VolumeCapability when present; fall back to
	// the project default (ext4) when the CO does not supply a capability.
	fsType := defaultFsType
	volCap := req.GetVolumeCapability()
	if volCap != nil {
		if mnt := volCap.GetMount(); mnt != nil && mnt.GetFsType() != "" {
			fsType = mnt.GetFsType()
		}
	}

	// ── Run filesystem resize ────────────────────────────────────────────────
	r := n.resizer
	if r == nil {
		r = &execResizer{}
	}

	resizeErr := r.ResizeFS(volumePath, fsType)
	if resizeErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeExpandVolume: resize %s filesystem at %q: %v", fsType, volumePath, resizeErr)
	}

	// ── Return capacity ──────────────────────────────────────────────────────
	// Echo back the required_bytes from the capacity_range when provided so
	// the CO can update the PersistentVolume capacity field.  A zero value
	// means "fill the available block device capacity" — the CO accepts this.
	var capacityBytes int64
	if cr := req.GetCapacityRange(); cr != nil {
		capacityBytes = cr.GetRequiredBytes()
	}

	return &csi.NodeExpandVolumeResponse{CapacityBytes: capacityBytes}, nil
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
func (*execResizer) ResizeFS(mountPath, fsType string) error {
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

	switch fsType {
	case defaultFsType, "ext3", "ext2":
		resize2fs := findExecutable("resize2fs", "/usr/sbin/resize2fs", "/sbin/resize2fs")
		out, cmdErr := exec.Command(resize2fs, device).CombinedOutput() //nolint:gosec // device is from /proc/mounts
		if cmdErr != nil {
			return fmt.Errorf("resize2fs %q: %w: %s", device, cmdErr, strings.TrimSpace(string(out)))
		}

	case xfsFsType:
		xfsGrowfs := findExecutable("xfs_growfs", "/usr/sbin/xfs_growfs", "/sbin/xfs_growfs")
		out, cmdErr := exec.Command(xfsGrowfs, mountPath).CombinedOutput() //nolint:gosec // mountPath validated by caller
		if cmdErr != nil {
			return fmt.Errorf("xfs_growfs %q: %w: %s", mountPath, cmdErr, strings.TrimSpace(string(out)))
		}
	}

	return nil
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
	ctrlDev := ensureNVMeCtrlDev(ctrl)
	if ctrlDev == "" {
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

// ensureNVMeCtrlDev returns the path to the NVMe controller character device
// (e.g. "/dev/nvme2") for the given controller name (e.g. "nvme2").
//
// In containerised environments the devtmpfs may not contain the controller
// char device even though the kernel registered it.  When the device node is
// missing, ensureNVMeCtrlDev reads the major:minor numbers from sysfs and
// creates the node with mknod(2).  Returns "" if the device cannot be ensured.
func ensureNVMeCtrlDev(ctrl string) string {
	ctrlPath := "/dev/" + ctrl
	_, statErr := os.Stat(ctrlPath)
	if statErr == nil {
		return ctrlPath // already exists
	}

	// Read major:minor from sysfs (e.g. "234:2").
	devFile := "/sys/class/nvme/" + ctrl + "/dev"
	data, readErr := os.ReadFile(devFile) //nolint:gosec // sysfs path from controller name
	if readErr != nil {
		return ""
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	major, majErr := strconv.Atoi(parts[0])
	minor, minErr := strconv.Atoi(parts[1])
	if majErr != nil || minErr != nil {
		return ""
	}

	// Create the character device node.  Requires CAP_MKNOD (privileged).
	devNum := major<<8 | minor
	mknodErr := syscall.Mknod(ctrlPath, syscall.S_IFCHR|0o600, devNum)
	if mknodErr != nil {
		log.Printf("pillar-node: mknod %q (major=%d minor=%d): %v",
			ctrlPath, major, minor, mknodErr)
		return ""
	}
	log.Printf("pillar-node: created missing NVMe ctrl device %q (major=%d minor=%d)",
		ctrlPath, major, minor)
	return ctrlPath
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
