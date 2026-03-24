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
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

// NodeGetVolumeStats returns capacity, used, and available bytes (and inodes)
// for the volume mounted at the given volume_path.  It calls syscall.Statfs on
// the mount point and converts the result into the CSI VolumeUsage format.
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
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats: volume_id is required")
	}
	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats: volume_path is required")
	}

	// ── Collect filesystem statistics via statfs ─────────────────────────────
	// syscall.Statfs populates a Statfs_t with block counts and inode counts
	// for the filesystem containing the given path.
	var statfs syscall.Statfs_t
	if err := syscall.Statfs(volumePath, &statfs); err != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeGetVolumeStats: statfs %q: %v", volumePath, err)
	}

	// Convert block counts to bytes using the fundamental block size (Bsize).
	// Note: Bsize is an int64 on Linux; cast to int64 for safe arithmetic.
	blockSize := int64(statfs.Bsize)
	totalBytes := int64(statfs.Blocks) * blockSize
	availableBytes := int64(statfs.Bavail) * blockSize
	usedBytes := (int64(statfs.Blocks) - int64(statfs.Bfree)) * blockSize

	// Inode counts (cast to int64; Ffree is the number of free inodes).
	totalInodes := int64(statfs.Files)
	availableInodes := int64(statfs.Ffree)
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
