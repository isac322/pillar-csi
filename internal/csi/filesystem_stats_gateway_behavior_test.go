package csi

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/testutil/testcerts"
	"github.com/isac322/pillar-csi/internal/tlscreds"
)

// Only the native backend is fake: requests pass through the real cached
// transport and production AgentService.InspectImport validation.
type statsNativeBackend struct {
	backend.VolumeBackend
	mu       sync.Mutex
	kind     agentv1.BackendType
	layout   backend.Layout
	adoption *agentv1.FilesystemAdoption
	capacity int64
}

func (b *statsNativeBackend) Type() agentv1.BackendType { return b.kind }
func (b *statsNativeBackend) Layout() backend.Layout    { return b.layout }
func (b *statsNativeBackend) InspectImport(
	_ context.Context, source string, required int64, layout backend.Layout,
) (*backend.ImportInspection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if source != b.adoption.GetCanonicalSource() || layout != b.layout || required != b.capacity {
		return nil, &backend.ImportRefusedError{Reason: "native source, layout or exact quota changed"}
	}
	adoption, ok := proto.Clone(b.adoption).(*agentv1.FilesystemAdoption)
	if !ok {
		return nil, fmt.Errorf("clone native filesystem descriptor")
	}
	return &backend.ImportInspection{Filesystem: adoption, CapacityBytes: b.capacity}, nil
}

func statsGatewayPEMFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFilesystemStatsGatewayRealTransport(t *testing.T) {
	for _, transport := range []string{"plaintext", "mtls"} {
		for _, kind := range []string{"directory", "zfs-dataset"} {
			t.Run(transport+"/"+kind, func(t *testing.T) {
				testStatsGatewayTransport(t, transport, kind)
			})
		}
	}
}

func statsNativeFixture(kind string) (*statsNativeBackend, *FileStageState) {
	a := &agentv1.FilesystemAdoption{
		Kind: kind, CanonicalSource: "/existing/data", ResourceId: "uuid:42",
		FilesystemType: "ext4", FilesystemId: "uuid", Inode: 42, ProjectId: 7,
	}
	b := &statsNativeBackend{
		kind:   agentv1.BackendType_BACKEND_TYPE_DIRECTORY,
		layout: backend.Layout{HostRoot: "/existing"}, adoption: a, capacity: 1 << 20,
	}
	state := &FileStageState{
		VolumeID: "agent/nfs/directory/pool/native", Kind: kind,
		CanonicalSource: a.CanonicalSource, ResourceID: a.ResourceId,
		FilesystemType: a.FilesystemType, FilesystemID: a.FilesystemId,
		Inode: a.Inode, ProjectID: a.ProjectId, BackendType: kind,
		PoolName: "pool", ExpectedHostRoot: "/existing", CapacityBytes: b.capacity, AgentName: "agent",
	}
	if kind == "zfs-dataset" {
		a.CanonicalSource = "pool/legacy/data"
		a.ResourceId = "123456789"
		a.FilesystemType = "zfs"
		a.FilesystemId, a.Inode, a.ProjectId = "", 0, 0
		b.kind = agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
		b.layout = backend.Layout{ParentDataset: "legacy"}
		state.CanonicalSource, state.ResourceID, state.FilesystemType = a.CanonicalSource, a.ResourceId, a.FilesystemType
		state.FilesystemID, state.Inode, state.ProjectID = "", 0, 0
		state.ExpectedHostRoot, state.ExpectedParentDataset = "", "legacy"
	}
	return b, state
}

type statsTransportConfig struct {
	options                   []grpc.ServerOption
	cert, key, ca, serverName string
}

func newStatsTransportConfig(t *testing.T, transport string) statsTransportConfig {
	t.Helper()
	if transport != "mtls" {
		return statsTransportConfig{}
	}
	bundle, err := testcerts.New("agent.metadata.test")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := tlscreds.NewServerCredentials(bundle.ServerCert, bundle.ServerKey, bundle.CACert)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return statsTransportConfig{
		options: []grpc.ServerOption{grpc.Creds(creds)},
		cert:    statsGatewayPEMFile(t, dir, "client.pem", bundle.ClientCert),
		key:     statsGatewayPEMFile(t, dir, "client-key.pem", bundle.ClientKey),
		ca:      statsGatewayPEMFile(t, dir, "ca.pem", bundle.CACert), serverName: "agent.metadata.test",
	}
}

func startStatsTransport(t *testing.T, b *statsNativeBackend, config statsTransportConfig) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(config.options...)
	agentv1.RegisterAgentServiceServer(srv, agent.NewServer(
		map[string]backend.VolumeBackend{"pool": b}, "",
		agent.WithDrainStateDir(filepath.Join(root, "drain")),
		agent.WithFilesystemProxy(filepath.Join(root, "proxies")),
	))
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		serveErr := <-serveDone
		if serveErr != nil {
			t.Errorf("agent serve: %v", serveErr)
		}
	})
	return lis.Addr().String()
}

func statsTestGateway(t *testing.T, config statsTransportConfig) *FilesystemStatsGateway {
	t.Helper()
	gateway, err := NewFilesystemStatsGateway(config.cert, config.key, config.ca, config.serverName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeErr := gateway.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	})
	return gateway
}

func testStatsGatewayTransport(t *testing.T, transport, kind string) {
	t.Helper()
	b, state := statsNativeFixture(kind)
	config := newStatsTransportConfig(t, transport)
	state.AgentEndpoint = startStatsTransport(t, b, config)
	gateway := statsTestGateway(t, config)
	mountPath := t.TempDir()
	node := NewNodeServer("consumer", nil, nil).WithStateDir(t.TempDir()).
		WithDriverName("files.pillar-csi.bhyoo.com").WithFilesystemStatsReader(gateway.Read)
	err := node.writeStageState(state.VolumeID, &nodeStageState{VolumeID: state.VolumeID, File: state})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := &csi.NodeGetVolumeStatsRequest{VolumeId: state.VolumeID, VolumePath: mountPath}
	got, err := node.NodeGetVolumeStats(ctx, request)
	if err != nil {
		t.Fatalf("production stats: %v", err)
	}
	if len(got.GetUsage()) != 1 || got.GetUsage()[0].GetUnit() != csi.VolumeUsage_BYTES ||
		got.GetUsage()[0].GetTotal() != state.CapacityBytes {
		t.Fatalf("stats lost admitted bound: %v", got)
	}
	if transport == "mtls" {
		config.serverName = "other-agent.test"
		wrongNameGateway := statsTestGateway(t, config)
		badCtx, badCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, badErr := wrongNameGateway.Read(badCtx, mountPath, state)
		badCancel()
		if badErr == nil {
			t.Fatal("wrong TLS server identity was accepted")
		}
	}
	assertStatsNativeDriftRefused(ctx, t, node, gateway, request, b, state)
}

func TestFilesystemStatsGatewayPublishedPathErrors(t *testing.T) {
	gateway := statsTestGateway(t, statsTransportConfig{})
	root := t.TempDir()
	loopPath := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loopPath); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"directory", "zfs-dataset"} {
		t.Run(kind, func(t *testing.T) {
			_, state := statsNativeFixture(kind)
			state.AgentEndpoint = "127.0.0.1:9500"
			node := NewNodeServer("consumer", nil, nil).WithStateDir(t.TempDir()).
				WithDriverName("files.pillar-csi.bhyoo.com").WithFilesystemStatsReader(gateway.Read)
			if err := node.writeStageState(state.VolumeID, &nodeStageState{VolumeID: state.VolumeID, File: state}); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name string
				path string
				code codes.Code
			}{
				{name: "missing", path: filepath.Join(root, "missing"), code: codes.NotFound},
				{name: "symlink-loop", path: loopPath, code: codes.Internal},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := node.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
						VolumeId: state.VolumeID, VolumePath: tc.path,
					})
					if status.Code(err) != tc.code {
						t.Fatalf("published path %q: got %v, want %v", tc.path, err, tc.code)
					}
				})
			}
		})
	}
}

func assertStatsNativeDriftRefused(
	ctx context.Context, t *testing.T, node *NodeServer, gateway *FilesystemStatsGateway,
	request *csi.NodeGetVolumeStatsRequest, b *statsNativeBackend, state *FileStageState,
) {
	t.Helper()
	b.mu.Lock()
	b.adoption.ResourceId = "replacement-native-identity"
	b.mu.Unlock()
	_, err := node.NodeGetVolumeStats(ctx, request)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("native replacement was not refused: %v", err)
	}
	b.mu.Lock()
	b.adoption.ResourceId = state.ResourceID
	b.capacity++
	b.mu.Unlock()
	_, err = node.NodeGetVolumeStats(ctx, request)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("quota drift was not refused: %v", err)
	}
	b.mu.Lock()
	b.capacity = state.CapacityBytes
	b.mu.Unlock()
	wrongLayout := *state
	if state.Kind == "directory" {
		wrongLayout.ExpectedHostRoot = "/broader"
	} else {
		wrongLayout.ExpectedParentDataset = "other"
	}
	_, err = gateway.Read(ctx, request.VolumePath, &wrongLayout)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("frozen backend layout drift was not refused: %v", err)
	}
}

func TestFilesystemStatsGatewayRejectsMisconfiguredTLS(t *testing.T) {
	for _, config := range []struct{ cert, key, ca, name string }{
		{cert: "/missing/cert"},
		{key: "/missing/key"},
		{ca: "/missing/ca"},
		{name: "agent.metadata.test"},
		{cert: "/missing/cert", key: "/missing/key", ca: "/missing/ca"},
	} {
		gateway, err := NewFilesystemStatsGateway(config.cert, config.key, config.ca, config.name)
		if err == nil || gateway != nil {
			t.Fatalf("invalid TLS config silently selected a transport: gateway=%v error=%v", gateway, err)
		}
	}
}

func TestFilesystemStatsGatewayRejectsUntrustedEndpointAndOldMetadata(t *testing.T) {
	gateway, err := NewFilesystemStatsGateway("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := gateway.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, endpoint := range []string{
		"", "nfs://agent:2049", "dns:///agent:9500", "agent", "agent:0", "agent:65536", ":9500",
	} {
		state := &FileStageState{
			VolumeID: "native", PoolName: "pool", CanonicalSource: "/existing/data",
			ResourceID: "uuid:42", CapacityBytes: 1 << 20, AgentEndpoint: endpoint,
		}
		if _, err := gateway.Read(context.Background(), t.TempDir(), state); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("invalid endpoint %q did not fail closed: %v", endpoint, err)
		}
	}
	oldState := &FileStageState{CanonicalSource: "/existing/data", ResourceID: "uuid:42", CapacityBytes: 1 << 20}
	if _, err := gateway.Read(context.Background(), t.TempDir(), oldState); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("old metadata fell back to statfs: %v", err)
	}
}

func TestFileStageContextTrustsPublishRoutingOnly(t *testing.T) {
	node := NewNodeServer("consumer", nil, nil).WithDriverName("files.pillar-csi.bhyoo.com")
	request := &csi.NodeStageVolumeRequest{
		VolumeId: "agent/nfs/directory/pool/native/lifecycle",
		VolumeContext: map[string]string{
			fileContextAgentAddress:  "attacker:9500",
			fileContextAgentName:     "attacker",
			fileContextAgentVolumeID: "attacker/alias",
			VolumeContextKeyFilesystemAdoption: `{"kind":"directory","canonicalSource":"/wrong",` +
				`"resourceId":"wrong","filesystemType":"xfs"}`,
			VolumeContextKeyFilesystemCapacity: "999",
		},
		PublishContext: map[string]string{
			fileContextAgentAddress:  "trusted-agent:9500",
			fileContextAgentName:     "trusted-agent",
			fileContextAgentVolumeID: "pool/native",
			PublishContextKeyFilesystemAdoption: `{"kind":"directory","canonicalSource":"/existing/data",` +
				`"resourceId":"uuid:42","filesystemType":"ext4","filesystemId":"uuid","inode":42,"projectId":7}`,
			PublishContextKeyFilesystemCapacity: "1048576",
			PublishContextKeyFilesystemLayout:   `{"directory":{"logicalPool":"pool","hostRoot":"/existing"}}`,
		},
	}
	fileCtx, err := node.parseFileStageContext(request)
	if err != nil {
		t.Fatal(err)
	}
	record := fileCtx.state
	if record.AgentEndpoint != "trusted-agent:9500" || record.AgentName != "trusted-agent" ||
		record.AgentVolumeID != "pool/native" || record.VolumeID != request.VolumeId ||
		record.CanonicalSource != "/existing/data" || record.CapacityBytes != 1048576 ||
		record.PoolName != "pool" || record.ExpectedHostRoot != "/existing" {
		t.Fatalf("untrusted VC changed admitted identity, scope or routing: %#v", record)
	}
	delete(request.PublishContext, fileContextAgentAddress)
	fileCtx, err = node.parseFileStageContext(request)
	if err != nil {
		t.Fatal(err)
	}
	if fileCtx.state.AgentEndpoint != "" {
		t.Fatalf("PV userdata selected agent endpoint %q", fileCtx.state.AgentEndpoint)
	}
}

func TestFileStageMetadataMigrationPreservesBoundAndNativeIdentity(t *testing.T) {
	old := &FileStageState{
		ProxyPath: "/proxy/data", Local: true, CanonicalSource: "/existing/data", ResourceID: "uuid:42",
		FilesystemType: "ext4", FilesystemID: "uuid", Inode: 42, ProjectID: 7, CapacityBytes: 1048576,
	}
	current := *old
	current.VolumeID, current.Kind = "public/lifecycle", "directory"
	current.BackendType, current.PoolName, current.ExpectedHostRoot = "directory", "pool", "/existing"
	current.AgentEndpoint, current.AgentVolumeID = "trusted:9500", "pool/native"
	existing := &nodeStageState{File: old}
	if err := refreshFileStageMetadata(existing, &current); err != nil {
		t.Fatal(err)
	}
	replacement := current
	replacement.ResourceID = "uuid:43"
	if err := refreshFileStageMetadata(existing, &replacement); err == nil {
		t.Fatal("restage silently repinned replacement native identity")
	}
	replacement = current
	replacement.CapacityBytes++
	if err := refreshFileStageMetadata(existing, &replacement); err == nil {
		t.Fatal("restage silently changed admitted quota")
	}
	replacement = current
	replacement.ExpectedHostRoot = "/"
	if err := refreshFileStageMetadata(existing, &replacement); err == nil {
		t.Fatal("restage silently broadened frozen layout")
	}
}

func TestDriverScopedStateRestoreIgnoresOtherProfile(t *testing.T) {
	blockRoot := t.TempDir()
	fileRoot := filepath.Join(blockRoot, "files")
	fileNode := NewNodeServer("consumer", nil, nil).
		WithDriverName("files.pillar-csi.bhyoo.com").WithStateDir(fileRoot)
	if err := fileNode.writeStageState("file/lifecycle", &nodeStageState{
		ProtocolType: ProtocolNFS, VolumeID: "file/lifecycle",
		File: &FileStageState{ResourceID: "uuid:42", CapacityBytes: 1048576},
	}); err != nil {
		t.Fatal(err)
	}
	oldBlockRecord := filepath.Join(blockRoot, "old-block.json")
	if err := os.WriteFile(oldBlockRecord, []byte("malformed old block state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fileNode.RestoreProtocolSessions(t.Logf); err != nil {
		t.Fatalf("file startup read old block profile state: %v", err)
	}
	blockNode := NewNodeServer("consumer", nil, nil).WithStateDir(blockRoot)
	if err := blockNode.RestoreProtocolSessions(t.Logf); err == nil {
		t.Fatal("fixture did not exercise actual stage-state decoding")
	}
	if err := os.Remove(oldBlockRecord); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileNode.stateFilePath("file/lifecycle"), []byte("malformed file state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := blockNode.RestoreProtocolSessions(t.Logf); err != nil {
		t.Fatalf("block startup recursively read nested file profile state: %v", err)
	}
}
