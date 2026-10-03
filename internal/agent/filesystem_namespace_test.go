package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func TestFilesystemInspectionAndImportRejectReservedSourceOverlap(t *testing.T) {
	for _, tc := range []struct {
		name         string
		relativePath string
	}{
		{name: "inside", relativePath: filepath.Join("filesystem", "original")},
		{name: "equal", relativePath: "filesystem"},
		{name: "ancestor", relativePath: "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newProxyLifecycleFixture(t)
			s, b, adoption, params := fixture.server, fixture.backend, fixture.adoption, fixture.params
			source := filepath.Join(s.filesystemProxyRoot, tc.relativePath)
			if err := os.MkdirAll(source, 0o750); err != nil {
				t.Fatal(err)
			}
			adoption.CanonicalSource = source
			b.source = source
			b.layout.HostRoot = filepath.Dir(s.filesystemProxyRoot)
			params.GetDirectory().HostRoot = b.layout.HostRoot
			_, err := s.InspectImport(context.Background(), &agentv1.InspectImportRequest{
				PoolName:         "pool",
				Source:           source,
				BackendType:      b.backendType,
				RequiredBytes:    b.capacity,
				ExpectedHostRoot: b.layout.HostRoot,
			})
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("reserved source inspection admitted: %v", err)
			}
			_, err = s.ImportVolume(context.Background(), proxyImportRequest(adoption, params, "owner"))
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("reserved source ownership admitted: %v", err)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("refusal changed original source: %v", err)
			}
			if _, err := os.Stat(filepath.Join(s.resolvedDrainStateDir(), fencingDirName)); !os.IsNotExist(err) {
				t.Fatalf("refused source gained ownership: %v", err)
			}
		})
	}
}

func TestFilesystemBroadAllowRootStillAdmitsUnrelatedNamespaceSibling(t *testing.T) {
	fixture := newProxyLifecycleFixture(t)
	s, b, adoption, params := fixture.server, fixture.backend, fixture.adoption, fixture.params
	source := filepath.Join(s.filesystemProxyRoot, "filesystem-source")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	adoption.CanonicalSource = source
	b.source = source
	b.layout.HostRoot = filepath.Dir(s.filesystemProxyRoot)
	params.GetDirectory().HostRoot = b.layout.HostRoot
	if _, err := s.ImportVolume(context.Background(), proxyImportRequest(adoption, params, "owner")); err != nil {
		t.Fatalf("safe broad-root source rejected: %v", err)
	}
	if _, err := s.SetLocalAttach(
		context.Background(), proxyLocalRequest(adoption, params, "owner", 2, true),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLocalAttach(
		context.Background(), proxyLocalRequest(adoption, params, "owner", 3, false),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseVolume(context.Background(), proxyReleaseRequest(adoption, "owner", 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("unrelated original source removed: %v", err)
	}
}
