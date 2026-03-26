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
	"syscall"
	"unsafe"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

// blkgetsize64 is the Linux ioctl request number to query the size of a block
// device in bytes.  The constant is the same on Linux/amd64 and arm64, the two
// supported platforms for pillar-csi nodes.
//
// Reference: <linux/fs.h> BLKGETSIZE64 = _IOR(0x12, 114, size_t).
const blkgetsize64 = uintptr(0x80081272)

// linuxBlockDeviceSize queries the total capacity of the block device at path
// using the BLKGETSIZE64 ioctl and returns it in bytes.
//
// The caller is responsible for ensuring that path refers to a block device
// (i.e. os.Stat(path).Mode()&os.ModeDevice != 0).
func linuxBlockDeviceSize(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is a validated block device path from caller
	if err != nil {
		return 0, fmt.Errorf("open block device %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	var size uint64
	// SYS_IOCTL / BLKGETSIZE64 writes an 8-byte unsigned int into *size.
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		blkgetsize64,
		uintptr(unsafe.Pointer(&size)), //nolint:gosec // G103: unsafe.Pointer required for ioctl ABI
	)
	if errno != 0 {
		return 0, fmt.Errorf("ioctl BLKGETSIZE64 on %q: %w", path, errno)
	}
	return int64(size), nil
}

// NodeGetVolumeStats returns capacity statistics for the volume accessible at
// volume_path.
//
// The function auto-detects the volume type by inspecting the file mode at
// volume_path:
//
//   - Block device (os.ModeDevice set): the BLKGETSIZE64 ioctl is used to read
//     the total device capacity in bytes.  Only a BYTES usage entry is returned
//     (used/available are undefined for a raw block device — there is no
//     filesystem to track allocations).
//
//   - Filesystem mount point (regular directory / file): syscall.Statfs is
//     called and both BYTES and INODES usage entries are returned.
//
// The CO calls this RPC periodically to populate PersistentVolumeClaim status
// capacity fields and to drive node-level storage pressure eviction decisions.
//
// Capability: NodeServiceCapability_RPC_GET_VOLUME_STATS must be advertised in
// NodeGetCapabilities for the CO to invoke this RPC.
func (n *NodeServer) NodeGetVolumeStats(
	_ context.Context,
	req *csi.NodeGetVolumeStatsRequest,
) (*csi.NodeGetVolumeStatsResponse, error) {
	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats: volume_id is required") //nolint:wrapcheck
	}
	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats: volume_path is required") //nolint:wrapcheck
	}

	// Select the stat function: use the injected override when present
	// (set by tests via n.statFn), otherwise fall back to os.Stat.
	statFn := n.statFn
	if statFn == nil {
		statFn = os.Stat
	}

	// ── Detect volume type ──────────────────────────────────────────────────
	// Stat the path to determine whether this is a raw block device or a
	// filesystem mount point.  For block volumes the CO sets volume_path to
	// the block device special file (e.g. /dev/nvme0n1); for filesystem
	// volumes it points to a directory.
	fi, statErr := statFn(volumePath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, status.Errorf(codes.NotFound,
				"NodeGetVolumeStats: volume_path %q does not exist", volumePath)
		}
		return nil, status.Errorf(codes.Internal,
			"NodeGetVolumeStats: stat %q: %v", volumePath, statErr)
	}

	// ── Block device path ───────────────────────────────────────────────────
	// For raw block volumes (CSI VolumeCapability with AccessType = Block) the
	// staging target is a bind-mount of the NVMe block device; the CO passes
	// the path to the block device special file.  We report only the total
	// capacity because used / available bytes are not meaningful for a raw
	// device (no filesystem metadata exists to track allocations).
	if fi.Mode()&os.ModeDevice != 0 {
		// Select the block device size function: use the injected override
		// when present (set by tests via n.blockDeviceSizeFn), otherwise
		// fall back to the real BLKGETSIZE64 ioctl implementation.
		sizeFn := n.blockDeviceSizeFn
		if sizeFn == nil {
			sizeFn = linuxBlockDeviceSize
		}

		totalBytes, sizeErr := sizeFn(volumePath)
		if sizeErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeGetVolumeStats: get block device size for %q: %v",
				volumePath, sizeErr)
		}

		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				{
					Unit:  csi.VolumeUsage_BYTES,
					Total: totalBytes,
					// Used and Available are not reported for raw block volumes:
					// there is no filesystem to track allocated vs free blocks.
				},
			},
		}, nil
	}

	// ── Filesystem mount point ───────────────────────────────────────────────
	// syscall.Statfs populates a Statfs_t with block counts and inode counts
	// for the filesystem containing the given path.
	var statfs syscall.Statfs_t
	err := syscall.Statfs(volumePath, &statfs)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeGetVolumeStats: statfs %q: %v", volumePath, err)
	}

	// Convert block counts to bytes using the fundamental block size (Bsize).
	// Statfs_t field widths vary by architecture (int64 on amd64, int32 on
	// arm/v7 and ppc64le), so every field is cast to int64 for portability.
	blockSize := int64(statfs.Bsize) //nolint:unconvert // Bsize is int64 on amd64 but int32 on arm/ppc64le
	blocks := int64(statfs.Blocks)   //nolint:gosec // G115: fits in int64
	bavail := int64(statfs.Bavail)   //nolint:gosec // G115: fits in int64
	bfree := int64(statfs.Bfree)     //nolint:gosec // G115: fits in int64
	totalBytes := blocks * blockSize
	availableBytes := bavail * blockSize
	usedBytes := (blocks - bfree) * blockSize

	// Inode counts.
	totalInodes := int64(statfs.Files)     //nolint:gosec // G115: fits in int64
	availableInodes := int64(statfs.Ffree) //nolint:gosec // G115: fits in int64
	usedInodes := totalInodes - availableInodes

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     totalBytes,
				Available: availableBytes,
				Used:      usedBytes,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Total:     totalInodes,
				Available: availableInodes,
				Used:      usedInodes,
			},
		},
	}, nil
}
