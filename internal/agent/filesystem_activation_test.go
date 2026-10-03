package agent

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
)

func adoptedActivationFixture(t *testing.T) (*Server, *proxyTestBackend, nfs.Export) {
	t.Helper()
	fixture := newProxyLifecycleFixture(t)
	s, b, adoption, params := fixture.server, fixture.backend, fixture.adoption, fixture.params
	if _, err := s.ImportVolume(context.Background(), proxyImportRequest(adoption, params, "owner")); err != nil {
		t.Fatal(err)
	}
	local, err := s.SetLocalAttach(context.Background(), proxyLocalRequest(adoption, params, "owner", 2, true))
	if err != nil {
		t.Fatal(err)
	}
	key := backend.FilesystemFenceID(adoption)
	mark, exists, err := s.readFencingMark(key)
	if err != nil || !exists {
		t.Fatalf("native mark unavailable: %v", err)
	}
	// Model the completed durable admission step before the manager grants.
	mark.FilesystemExported = true
	mark.FilesystemClients = []string{"192.0.2.20"}
	if err := s.writeFencingMark(key, mark); err != nil {
		t.Fatal(err)
	}
	e := nfs.Export{
		VolumeID:   "pool/volume",
		Path:       local.GetDevicePath(),
		SourceKey:  key,
		FenceUID:   "owner",
		ACLEnabled: true,
		Clients:    []string{"192.0.2.20"},
		Active:     true,
	}
	return s, b, e
}

func TestFilesystemActivationRevalidatesNativeQuotaAndReplacement(t *testing.T) {
	t.Parallel()
	s, b, e := adoptedActivationFixture(t)
	if err := s.ValidateFilesystemExport(context.Background(), e); err != nil {
		t.Fatalf("valid owner denied: %v", err)
	}
	b.capacity *= 2
	if err := s.ValidateFilesystemExport(context.Background(), e); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("activation admitted changed native quota: %v", err)
	}
	b.capacity /= 2
	original, ok := proto.Clone(b.adoption).(*agentv1.FilesystemAdoption)
	if !ok {
		t.Fatal("cloned adoption has unexpected type")
	}
	b.adoption.ResourceId = "replacement-native-resource"
	if err := s.ValidateFilesystemExport(context.Background(), e); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("activation admitted replacement source: %v", err)
	}
	b.adoption = original
	b.present = false
	if err := s.ValidateFilesystemExport(context.Background(), e); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("activation admitted missing source: %v", err)
	}
}

func TestFilesystemActivationRejectsStaleOwnerAndBorrowedProxy(t *testing.T) {
	t.Parallel()
	s, _, e := adoptedActivationFixture(t)
	e.FenceUID = "former-owner"
	if err := s.ValidateFilesystemExport(context.Background(), e); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale native owner activated: %v", err)
	}
	e.FenceUID = "owner"
	e.Path = filepath.Join(s.filesystemProxyRoot, "different-proxy")
	if err := s.ValidateFilesystemExport(context.Background(), e); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("borrowed proxy activated: %v", err)
	}
}
