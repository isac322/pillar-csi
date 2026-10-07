package csi

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agentclient"
)

// FilesystemStatsGateway revalidates the admitted filesystem through the same
// cached, authenticated agent transport used by the controller. No mutable
// ownership RPC is issued, and a dataplane NFS address is never used for dialing.
type FilesystemStatsGateway struct {
	dialer *agentclient.Manager
}

const (
	filesystemKindDirectory  = "directory"
	filesystemKindZFSDataset = "zfs-dataset"
	zfsFsType                = "zfs"
)

// NewFilesystemStatsGateway uses plaintext only when no TLS option is set.
// Any configured TLS transport must supply valid certificate, key and CA files;
// an invalid or partial configuration never falls back to plaintext.
func NewFilesystemStatsGateway(certFile, keyFile, caFile, serverName string) (*FilesystemStatsGateway, error) {
	anyTLS := certFile != "" || keyFile != "" || caFile != "" || serverName != ""
	if !anyTLS {
		return &FilesystemStatsGateway{dialer: agentclient.NewManager()}, nil
	}
	if certFile == "" || keyFile == "" || caFile == "" {
		return nil, fmt.Errorf(
			"filesystem stats gateway: --agent-tls-cert, --agent-tls-key and " +
				"--agent-tls-ca must all be set together")
	}
	dialer, err := agentclient.NewManagerFromFiles(certFile, keyFile, caFile, serverName)
	if err != nil {
		return nil, fmt.Errorf("filesystem stats gateway: configure agent mTLS: %w", err)
	}
	return &FilesystemStatsGateway{dialer: dialer}, nil
}

// Close releases cached agent connections after the node stops serving RPCs.
func (g *FilesystemStatsGateway) Close() error {
	if g == nil || g.dialer == nil {
		return nil
	}
	err := g.dialer.Close()
	if err != nil {
		return fmt.Errorf("filesystem stats gateway: close agent connections: %w", err)
	}
	return nil
}

func validateFilesystemAgentEndpoint(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("invalid agent control-plane host:port %q: %w", endpoint, err)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 || host == "" || strings.ContainsAny(host, " /\\\x00\t\r\n") {
		return fmt.Errorf("invalid agent control-plane host:port %q", endpoint)
	}
	return nil
}

// Read matches WithFilesystemStatsReader. The admitted total is returned only
// after native identity, frozen backend layout and exact quota are revalidated.
// InspectImport does not expose quota-scoped allocations, so Used/Available and
// inode usage are deliberately not inferred from shared-filesystem statfs.
func (g *FilesystemStatsGateway) Read(
	ctx context.Context, volumePath string, state *FileStageState,
) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := filesystemStatsVolumeID(state)
	if g == nil || g.dialer == nil {
		return nil, filesystemStatsRefusal(volumeID, "agent gateway is not configured")
	}
	req, err := filesystemStatsInspectRequest(state)
	if err != nil {
		return nil, filesystemStatsRefusal(volumeID, "%v", err)
	}
	err = validatePublishedFilesystemPath(volumePath, volumeID)
	if err != nil {
		return nil, err
	}
	inspection, err := g.inspectFilesystemStats(ctx, state, req, volumeID)
	if err != nil {
		return nil, err
	}
	err = validateFilesystemStatsInspection(inspection, state, volumeID)
	if err != nil {
		return nil, err
	}
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{{
			Unit:  csi.VolumeUsage_BYTES,
			Total: state.CapacityBytes,
		}},
	}, nil
}

func filesystemStatsVolumeID(state *FileStageState) string {
	if state == nil {
		return ""
	}
	return state.VolumeID
}

func filesystemStatsRefusal(volumeID, format string, args ...any) error {
	return status.Errorf(
		codes.FailedPrecondition,
		"NodeGetVolumeStats: InspectImport volume %q: %s",
		volumeID,
		fmt.Sprintf(format, args...),
	)
}

func validatePublishedFilesystemPath(volumePath, volumeID string) error {
	info, err := os.Stat(volumePath)
	if err != nil {
		code := codes.Internal
		if os.IsNotExist(err) {
			code = codes.NotFound
		}
		return status.Errorf(
			code,
			"NodeGetVolumeStats: InspectImport volume %q: stat published path %q: %v",
			volumeID,
			volumePath,
			err,
		)
	}
	if !info.IsDir() {
		return filesystemStatsRefusal(
			volumeID,
			"published filesystem path %q is not a directory",
			volumePath,
		)
	}
	return nil
}

func (g *FilesystemStatsGateway) inspectFilesystemStats(
	ctx context.Context,
	state *FileStageState,
	req *agentv1.InspectImportRequest,
	volumeID string,
) (*agentv1.InspectImportResponse, error) {
	// Bound a dead agent independently of kubelet's optional RPC deadline.
	ctx, cancel := context.WithTimeout(
		agentclient.WithAgentName(ctx, state.AgentName),
		10*time.Second,
	)
	defer cancel()
	client, err := g.dialer.Dial(ctx, state.AgentEndpoint)
	if err != nil {
		return nil, status.Errorf(
			codes.Unavailable,
			"NodeGetVolumeStats: InspectImport volume %q: dial agent %q: %v",
			volumeID,
			state.AgentEndpoint,
			err,
		)
	}
	inspection, err := client.InspectImport(ctx, req)
	if err != nil {
		return nil, status.Errorf(
			status.Code(err),
			"NodeGetVolumeStats: InspectImport volume %q at agent %q: %v",
			volumeID,
			state.AgentEndpoint,
			err,
		)
	}
	return inspection, nil
}

func validateFilesystemStatsInspection(
	inspection *agentv1.InspectImportResponse,
	state *FileStageState,
	volumeID string,
) error {
	expected := &agentv1.FilesystemAdoption{
		Kind: state.Kind, CanonicalSource: state.CanonicalSource, ResourceId: state.ResourceID,
		HostPath: state.HostPath, FilesystemType: state.FilesystemType, FilesystemId: state.FilesystemID,
		Inode: state.Inode, ProjectId: state.ProjectID,
	}
	if inspection == nil || !proto.Equal(inspection.GetFilesystemAdoption(), expected) {
		return filesystemStatsRefusal(volumeID, "native filesystem identity drifted from the recorded descriptor")
	}
	if inspection.GetCapacityBytes() != state.CapacityBytes {
		return filesystemStatsRefusal(
			volumeID,
			"exact quota drifted: recorded %d bytes, inspected %d bytes",
			state.CapacityBytes,
			inspection.GetCapacityBytes(),
		)
	}
	return nil
}

func filesystemStatsInspectRequest(state *FileStageState) (*agentv1.InspectImportRequest, error) {
	if state == nil || state.PoolName == "" || state.CanonicalSource == "" ||
		state.ResourceID == "" || state.CapacityBytes <= 0 {
		return nil, fmt.Errorf(
			"persisted filesystem inspection metadata is incomplete; restage through the file controller",
		)
	}
	err := validateFilesystemAgentEndpoint(state.AgentEndpoint)
	if err != nil {
		return nil, err
	}
	expected := &agentv1.FilesystemAdoption{
		Kind: state.Kind, CanonicalSource: state.CanonicalSource, ResourceId: state.ResourceID,
		HostPath: state.HostPath, FilesystemType: state.FilesystemType, FilesystemId: state.FilesystemID,
		Inode: state.Inode, ProjectId: state.ProjectID,
	}
	req := &agentv1.InspectImportRequest{
		PoolName:                   state.PoolName,
		Source:                     state.CanonicalSource,
		RequiredBytes:              state.CapacityBytes,
		ExpectedParentDataset:      state.ExpectedParentDataset,
		ExpectedHostRoot:           state.ExpectedHostRoot,
		ExpectedFilesystemAdoption: expected,
	}
	switch state.Kind {
	case filesystemKindDirectory:
		err = validateDirectoryStatsIdentity(state)
		if err != nil {
			return nil, err
		}
		req.BackendType = agentv1.BackendType_BACKEND_TYPE_DIRECTORY
	case filesystemKindZFSDataset:
		err = validateZFSDatasetStatsIdentity(state)
		if err != nil {
			return nil, err
		}
		req.BackendType = agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
	default:
		return nil, fmt.Errorf("unsupported filesystem adoption kind %q", state.Kind)
	}
	return req, nil
}

func validateDirectoryStatsIdentity(state *FileStageState) error {
	if state.BackendType != filesystemKindDirectory || state.ExpectedHostRoot == "" ||
		state.ExpectedParentDataset != "" || state.HostPath != "" ||
		(state.FilesystemType != defaultFsType && state.FilesystemType != xfsFsType) ||
		state.FilesystemID == "" || state.Inode == 0 || state.ProjectID == 0 {
		return fmt.Errorf("persisted directory identity or backend layout is incomplete")
	}
	return nil
}

func validateZFSDatasetStatsIdentity(state *FileStageState) error {
	if state.BackendType != filesystemKindZFSDataset || state.ExpectedHostRoot != "" ||
		state.FilesystemType != zfsFsType || state.FilesystemID != "" ||
		state.Inode != 0 || state.ProjectID != 0 {
		return fmt.Errorf("persisted ZFS identity or backend layout is invalid")
	}
	return nil
}
