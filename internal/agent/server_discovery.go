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

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/health"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// GetCapabilities returns the backend types and protocols supported by this
// agent, together with the storage pools it has been configured with.
//
// SupportedBackends is derived from the registered backends so that the
// response reflects the actual configuration rather than hardcoded values.
func (s *Server) GetCapabilities(
	ctx context.Context,
	_ *agentv1.GetCapabilitiesRequest,
) (*agentv1.GetCapabilitiesResponse, error) {
	return &agentv1.GetCapabilitiesResponse{
		AgentVersion:      agentVersion,
		SupportedBackends: s.collectSupportedBackendTypes(),
		SupportedProtocols: []agentv1.ProtocolType{
			agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		},
		DiscoveredPools: s.collectPoolInfo(ctx),
	}, nil
}

// collectSupportedBackendTypes returns the deduplicated set of BackendType
// values reported by every registered backend.  Using each backend's own
// Type() method ensures that the list is always consistent with the actual
// runtime configuration.
func (s *Server) collectSupportedBackendTypes() []agentv1.BackendType {
	seen := make(map[agentv1.BackendType]struct{}, len(s.backends))
	for _, b := range s.backends {
		seen[b.Type()] = struct{}{}
	}
	types := make([]agentv1.BackendType, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	return types
}

// collectPoolInfo queries each registered backend for its capacity and builds
// the list of PoolInfo records for GetCapabilities.  BackendType is read from
// the backend's own Type() method so that the response is correct regardless
// of which backend implementation is registered for each pool.
func (s *Server) collectPoolInfo(ctx context.Context) []*agentv1.PoolInfo {
	pools := make([]*agentv1.PoolInfo, 0, len(s.backends))
	for name, b := range s.backends {
		total, avail, err := b.Capacity(ctx)
		if err != nil {
			continue // Best-effort: skip pools whose capacity cannot be queried.
		}
		pools = append(pools, &agentv1.PoolInfo{
			Name:           name,
			BackendType:    b.Type(),
			TotalBytes:     total,
			AvailableBytes: avail,
		})
	}
	return pools
}

// GetCapacity returns the total and available capacity of a specific pool.
func (s *Server) GetCapacity(
	ctx context.Context,
	req *agentv1.GetCapacityRequest,
) (*agentv1.GetCapacityResponse, error) {
	poolName := req.GetPoolName()
	b, ok := s.backends[poolName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no backend registered for pool %q", poolName)
	}
	total, avail, err := b.Capacity(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "GetCapacity: %v", err)
	}
	return &agentv1.GetCapacityResponse{
		TotalBytes:     total,
		AvailableBytes: avail,
		UsedBytes:      total - avail,
	}, nil
}

// ListVolumes returns all zvols currently present in the given pool.
func (s *Server) ListVolumes(
	ctx context.Context,
	req *agentv1.ListVolumesRequest,
) (*agentv1.ListVolumesResponse, error) {
	poolName := req.GetPoolName()
	b, ok := s.backends[poolName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no backend registered for pool %q", poolName)
	}
	vols, err := b.ListVolumes(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ListVolumes: %v", err)
	}
	return &agentv1.ListVolumesResponse{Volumes: vols}, nil
}

// ListExports returns all active NVMe-oF TCP exports from configfs.
func (s *Server) ListExports(
	_ context.Context,
	_ *agentv1.ListExportsRequest,
) (*agentv1.ListExportsResponse, error) {
	subsystems, err := nvmeof.ListExports(s.configfsRoot)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list exports: scan subsystems: %v", err)
	}
	portsByNQN, err := scanPorts(s.configfsRoot)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list exports: scan ports: %v", err)
	}

	exports := make(map[string]*agentv1.ExportInfo, len(subsystems))
	for _, subsystem := range subsystems {
		volumeID := volumeIDFromNQN(subsystem.NQN)
		if volumeID == "" {
			continue
		}

		port := portsByNQN[subsystem.NQN]
		exports[volumeID] = &agentv1.ExportInfo{
			TargetId:  subsystem.NQN,
			Address:   port.addr,
			Port:      port.port,
			VolumeRef: subsystem.NamespaceDevicePaths[1],
		}
	}

	return &agentv1.ListExportsResponse{Exports: exports}, nil
}

type portEntry struct {
	addr string
	port int32
}

func scanPorts(configfsRoot string) (map[string]portEntry, error) {
	if configfsRoot == "" {
		configfsRoot = nvmeof.DefaultConfigfsRoot
	}
	portsRoot := filepath.Join(configfsRoot, "nvmet", "ports")
	entries, err := os.ReadDir(portsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]portEntry{}, nil
		}
		return nil, fmt.Errorf("read ports dir %q: %w", portsRoot, err)
	}

	portsByNQN := make(map[string]portEntry)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		portDir := filepath.Join(portsRoot, entry.Name())
		portSubsystems, err := os.ReadDir(filepath.Join(portDir, "subsystems"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read port %s subsystems: %w", entry.Name(), err)
		}
		if len(portSubsystems) == 0 {
			continue
		}

		port, err := readPortEntry(portDir)
		if err != nil {
			return nil, fmt.Errorf("read port %s attrs: %w", entry.Name(), err)
		}
		for _, subsystemLink := range portSubsystems {
			portsByNQN[subsystemLink.Name()] = port
		}
	}
	return portsByNQN, nil
}

func readPortEntry(portDir string) (portEntry, error) {
	addr, err := readTrimmedConfigfsFile(filepath.Join(portDir, "addr_traddr"))
	if err != nil {
		return portEntry{}, fmt.Errorf("addr_traddr: %w", err)
	}
	portText, err := readTrimmedConfigfsFile(filepath.Join(portDir, "addr_trsvcid"))
	if err != nil {
		return portEntry{}, fmt.Errorf("addr_trsvcid: %w", err)
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil {
		return portEntry{}, fmt.Errorf("addr_trsvcid %q: %w", portText, err)
	}
	return portEntry{addr: addr, port: int32(port)}, nil
}

func readTrimmedConfigfsFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from the configured configfs root.
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func volumeIDFromNQN(nqn string) string {
	suffix, ok := strings.CutPrefix(nqn, nqnPrefix)
	if !ok {
		return ""
	}
	pool, name, ok := strings.Cut(suffix, ".")
	if !ok {
		return ""
	}
	return pool + "/" + name
}

// HealthCheck reports the liveness of the NVMe-oF subsystem as well as
// per-pool backend reachability.  The response is built from the structured
// health.HealthStatus model so that individual components can be queried by
// name without fragile string comparisons.
func (s *Server) HealthCheck(
	ctx context.Context,
	_ *agentv1.HealthCheckRequest,
) (*agentv1.HealthCheckResponse, error) {
	hs := s.collectHealthStatus(ctx)
	return &agentv1.HealthCheckResponse{
		Healthy:      hs.AllHealthy(),
		Subsystems:   hs.ToProtoSubsystems(),
		AgentVersion: agentVersion,
		CheckedAt:    timestamppb.Now(),
	}, nil
}

// collectHealthStatus builds the structured health.HealthStatus for this
// agent instance by probing each known subsystem.
func (s *Server) collectHealthStatus(ctx context.Context) health.HealthStatus {
	return health.HealthStatus{
		NvmetConfigfs: s.checkNvmetConfigfs(),
		PerPoolStatus: s.checkPerPoolStatus(ctx),
	}
}

// checkNvmetConfigfs reports whether the nvmet configfs directory tree is
// mounted and accessible at the expected path.
func (s *Server) checkNvmetConfigfs() health.ComponentStatus {
	root := s.configfsRoot
	if root == "" {
		root = nvmeof.DefaultConfigfsRoot
	}
	nvmetDir := filepath.Join(root, "nvmet")
	_, err := os.Stat(nvmetDir)
	if err == nil {
		return health.OK("nvmet configfs directory accessible.")
	}
	return health.Degraded(fmt.Sprintf("nvmet configfs check failed: %v", err))
}

// checkPerPoolStatus probes each registered backend with a Capacity call to
// verify that the pool is reachable and responsive.
func (s *Server) checkPerPoolStatus(ctx context.Context) []health.PoolStatus {
	poolStatuses := make([]health.PoolStatus, 0, len(s.backends))
	for name, b := range s.backends {
		_, _, err := b.Capacity(ctx)
		var cs health.ComponentStatus
		if err == nil {
			cs = health.OK(fmt.Sprintf("pool %q is reachable.", name))
		} else {
			cs = health.Degraded(fmt.Sprintf("pool %q capacity check failed: %v", name, err))
		}
		poolStatuses = append(poolStatuses, health.PoolStatus{Pool: name, Status: cs})
	}
	return poolStatuses
}
