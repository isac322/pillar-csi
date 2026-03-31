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
	"os"
	"os/exec"
	"strings"

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
// Ext4 (and ext3/ext2):
//
//	Parses /proc/mounts to find the block device that backs mountPath, then
//	runs: resize2fs <device>
//
//	resize2fs operates on the device node, not the mount point.  The kernel
//	block-layer expands the device after ControllerExpandVolume; resize2fs
//	then grows the ext filesystem metadata to fill the new device size.
//
// xfs:
//
//	Runs: xfs_growfs <mountPath>
//
//	xfs_growfs operates on the mount point.  It communicates with the kernel
//	XFS driver directly via ioctl and does not need the raw device path.
func (*execResizer) ResizeFS(mountPath, fsType string) error {
	switch fsType {
	case defaultFsType, "ext3", "ext2":
		device, err := deviceFromMount(mountPath)
		if err != nil {
			return fmt.Errorf("find block device for mount %q: %w", mountPath, err)
		}
		resize2fs := findExecutable("resize2fs", "/usr/sbin/resize2fs", "/sbin/resize2fs")
		out, cmdErr := exec.Command(resize2fs, device).CombinedOutput() //nolint:gosec // device is from /proc/mounts
		if cmdErr != nil {
			return fmt.Errorf("resize2fs %q: %w: %s", device, cmdErr, strings.TrimSpace(string(out)))
		}
		return nil

	case xfsFsType:
		xfsGrowfs := findExecutable("xfs_growfs", "/usr/sbin/xfs_growfs", "/sbin/xfs_growfs")
		out, cmdErr := exec.Command(xfsGrowfs, mountPath).CombinedOutput() //nolint:gosec // mountPath validated by caller
		if cmdErr != nil {
			return fmt.Errorf("xfs_growfs %q: %w: %s", mountPath, cmdErr, strings.TrimSpace(string(out)))
		}
		return nil

	default:
		return fmt.Errorf("unsupported filesystem type %q: only ext4 and xfs are supported for online resize", fsType)
	}
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
