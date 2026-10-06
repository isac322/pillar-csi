package agent

import (
	"context"
	"errors"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

type unhealthyLegacyMetadataBackend struct{ *proxyTestBackend }

func (*unhealthyLegacyMetadataBackend) ExistingFilesystemIdentity(
	context.Context,
	string,
) (*agentv1.FilesystemAdoption, error) {
	return nil, errors.New("native pool suspended")
}

func TestLegacyRevokeDoesNotClaimFilesystemOrRequireNativeInspection(t *testing.T) {
	t.Parallel()
	fixture := newProxyLifecycleFixture(t)
	b, source := fixture.backend, fixture.source
	b.backendType = agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
	b.layout = backend.Layout{ParentDataset: "k8s"}
	b.adoption = &agentv1.FilesystemAdoption{
		Kind:            "zfs-dataset",
		CanonicalSource: "pool/k8s/data",
		ResourceId:      "42",
		FilesystemType:  "zfs",
	}
	native := &unhealthyLegacyMetadataBackend{proxyTestBackend: b}
	s := NewServer(
		map[string]backend.VolumeBackend{"pool": native},
		"",
		WithDrainStateDir(fixture.server.resolvedDrainStateDir()),
		WithFilesystemProxy(fixture.server.filesystemProxyRoot),
	)
	admitted := true
	ctx := context.Background()
	withdraw := func() error {
		admitted = false
		return nil
	}
	if err := s.fencedChecked(ctx, "pool/data", token("old-owner", 2), fenceRevoke, native, nil, withdraw); err != nil {
		t.Fatalf("unhealthy source prevented access withdrawal: %v", err)
	}
	if admitted {
		t.Fatal("legacy access remained admitted")
	}
	params := &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Zfs{
			Zfs: &agentv1.ZfsVolumeParams{ParentDataset: "k8s"},
		},
	}
	_, err := s.ImportVolume(ctx, &agentv1.ImportVolumeRequest{
		VolumeId:           "pool/native",
		BackendType:        agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		FilesystemAdoption: b.adoption,
		CapacityBytes:      b.capacity,
		BackendParams:      params,
		Fence:              token("file-owner", 1),
	})
	if err != nil {
		t.Fatalf("revoke-only protocol mark claimed original source: %v", err)
	}
	if _, err = os.Stat(source); err != nil {
		t.Fatalf("read-only import changed original source: %v", err)
	}
	_, err = s.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "pool/data",
		BackendType:   b.backendType,
		CapacityBytes: b.capacity,
		BackendParams: params,
		Fence:         token("old-owner", 1),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("revoke stopped fencing delayed grants: %v", err)
	}
}
