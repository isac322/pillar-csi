package agent

import (
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
)

func nfsTestHandler(t *testing.T) (*NFSAgentHandler, *Server) {
	t.Helper()
	srv := newDrainTestServer(t.TempDir())
	manager, err := nfs.NewManager(nfs.Config{StateDir: filepath.Join(t.TempDir(), "nfs"), BindAddress: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	return NewNFSAgentHandler(srv, manager), srv
}

func nfsTestParams() *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_Nfs{
			Nfs: &agentv1.NfsExportParams{Version: "4.2", BindAddress: "192.0.2.10", Squash: "root"},
		},
	}
}

// Invalid protocol combinations must fail before a fencing generation is
// advanced; otherwise a malformed request could supersede a valid controller.
func TestNFSRejectsInvalidExportBeforeFence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*ExportParams)
	}{
		{"missing NFS params", func(p *ExportParams) { p.ProtocolParams = nil }},
		{"version", func(p *ExportParams) { p.ProtocolParams.GetNfs().Version = "3" }},
		{"squash", func(p *ExportParams) { p.ProtocolParams.GetNfs().Squash = "unsafe" }},
		{"DNS address", func(p *ExportParams) { p.ProtocolParams.GetNfs().BindAddress = "example.test" }},
		{"port", func(p *ExportParams) { p.Port = 111 }},
		{"address conflict", func(p *ExportParams) { p.BindAddress = "192.0.2.11" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, srv := nfsTestHandler(t)
			p := ExportParams{
				VolumeID: fencingTestVolume, DevicePath: t.TempDir(),
				ProtocolParams: nfsTestParams(), Fence: token("owner", 8),
			}
			tc.mutate(&p)
			if _, err := h.Export(t.Context(), p); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("Export error=%v, want InvalidArgument", err)
			}
			if err := srv.fenced(t.Context(), fencingTestVolume, token("owner", 1), fenceGrant, nil); err != nil {
				t.Fatalf("invalid export advanced fence: %v", err)
			}
		})
	}
}

func TestNFSStaleMutationsRejectedBeforeRuntime(t *testing.T) {
	t.Parallel()
	h, srv := nfsTestHandler(t)
	if err := srv.fenced(t.Context(), fencingTestVolume, token("owner", 9), fenceGrant, nil); err != nil {
		t.Fatal(err)
	}
	stale := token("owner", 8)
	p := ExportParams{VolumeID: fencingTestVolume, DevicePath: t.TempDir(), ProtocolParams: nfsTestParams(), Fence: stale}
	_, err := h.Export(t.Context(), p)
	errs := make([]error, 0, 5)
	errs = append(errs,
		err, h.Unexport(t.Context(), fencingTestVolume, stale),
		h.AllowInitiator(t.Context(), fencingTestVolume, "192.0.2.20", nil, stale),
		h.DenyInitiator(t.Context(), fencingTestVolume, "192.0.2.20", stale),
	)
	desired := ExportDesiredState{
		VolumeID: fencingTestVolume, DevicePath: p.DevicePath, ProtocolParams: p.ProtocolParams, Fence: stale,
	}
	errs = append(errs, h.Reconcile(t.Context(), []ExportDesiredState{desired})...)
	for i, err := range errs {
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("mutation %d error=%v, want stale fencing rejection", i, err)
		}
	}
}

func TestNFSRejectsLocalAttachAndUnsafeInitiators(t *testing.T) {
	t.Parallel()
	h, _ := nfsTestHandler(t)
	for _, local := range []bool{true, false} {
		_, err := h.SetLocalAttach(t.Context(), fencingTestVolume, local, token("owner", 1))
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("local=%t: %v", local, err)
		}
	}
	clients := []string{
		"*", "192.0.2.0/24", "host.test", "-o", "192.0.2.1\n",
		"2001:db8::1", "fe80::1%eth0", "0.0.0.0", "224.0.0.1",
	}
	for _, client := range clients {
		err := h.AllowInitiator(t.Context(), fencingTestVolume, client, nil, token("owner", 1))
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("client %q: %v", client, err)
		}
		err = h.DenyInitiator(t.Context(), fencingTestVolume, client, token("owner", 1))
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("client %q: %v", client, err)
		}
	}
	result := h.Reconcile(t.Context(), []ExportDesiredState{{VolumeID: fencingTestVolume, LocalAttach: true}})
	if status.Code(result[0]) != codes.FailedPrecondition {
		t.Fatalf("local reconcile: %v", result[0])
	}
	duplicate := ExportDesiredState{
		VolumeID: fencingTestVolume, DevicePath: t.TempDir(),
		ProtocolParams: nfsTestParams(), Fence: token("owner", 1),
	}
	for _, err := range h.Reconcile(t.Context(), []ExportDesiredState{duplicate, duplicate}) {
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("duplicate reconcile: %v", err)
		}
	}
}

func TestNFSReconcileReportsEachVolumeFailureIndependently(t *testing.T) {
	t.Parallel()
	h, srv := nfsTestHandler(t)
	if err := srv.fenced(t.Context(), fencingTestVolume, token("owner", 9), fenceGrant, nil); err != nil {
		t.Fatal(err)
	}
	bad := ExportDesiredState{VolumeID: "pool/bad", LocalAttach: true}
	stale := ExportDesiredState{
		VolumeID: fencingTestVolume, DevicePath: t.TempDir(),
		ProtocolParams: nfsTestParams(), Fence: token("owner", 8),
	}
	for _, desired := range [][]ExportDesiredState{{bad, stale}, {stale, bad}} {
		errs := h.Reconcile(t.Context(), desired)
		for i, err := range errs {
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("volume %q got batch-abort error instead of its own failure: %v", desired[i].VolumeID, err)
			}
		}
	}
	if err := srv.fenced(t.Context(), fencingTestVolume, token("owner", 9), fenceGrant, nil); err != nil {
		t.Fatalf("peer failure changed the surviving volume fence: %v", err)
	}
}

func TestLegacyNFSRejectsBorrowedFileProxyBeforeFencing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	expected := filepath.Join(root, "pool", "legacy")
	b := &proxyTestBackend{
		backendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		source:      expected, present: true,
	}
	s := NewServer(map[string]backend.VolumeBackend{"pool": b}, "", WithDrainStateDir(t.TempDir()))
	manager, err := nfs.NewManager(nfs.Config{StateDir: t.TempDir(), BindAddress: "192.0.2.10", ExportRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	h := NewNFSAgentHandler(s, manager)
	params := ExportParams{
		VolumeID: "pool/legacy", DevicePath: filepath.Join(root, "filesystem", "borrowed-native-resource"),
		ProtocolParams: nfsTestParams(), Fence: token("legacy-owner", 8),
	}
	if _, err := h.Export(t.Context(), params); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("legacy routing ID exposed another filesystem proxy: %v", err)
	}
	if _, exists, err := s.readFencingMark(params.VolumeID); err != nil || exists {
		t.Fatalf("borrowed path left a phantom legacy owner: exists=%t, err=%v", exists, err)
	}
	params.DevicePath = expected
	if _, err := h.exportSpec(params); err != nil {
		t.Fatalf("configured legacy dataset path rejected before normal NFS validation: %v", err)
	}
}
