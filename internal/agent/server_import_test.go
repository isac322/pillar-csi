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

package agent_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

// mockImporterBackend adds an Import implementation to mockBackend, making it
// satisfy backend.VolumeImporter.
type mockImporterBackend struct {
	*mockBackend
	importDevicePath string
	importSize       int64
	importErr        error
	importCalledWith []string
}

func (m *mockImporterBackend) Import(
	_ context.Context,
	volumeID string,
	_ int64,
) (devicePath string, sizeBytes int64, err error) {
	m.importCalledWith = append(m.importCalledWith, volumeID)
	return m.importDevicePath, m.importSize, m.importErr
}

var _ backend.VolumeImporter = (*mockImporterBackend)(nil)

// newImportTestServer registers mb (a full importer) as the backend for
// testPool, so the volume path exercises the real VolumeImporter assertion.
func newImportTestServer(t *testing.T, mb *mockImporterBackend) *agent.Server {
	t.Helper()
	backends := map[string]backend.VolumeBackend{
		testPool: mb,
	}
	return agent.NewServer(backends, "", agent.WithDrainStateDir(t.TempDir()))
}
func importRequest(t *testing.T) *agentv1.ImportVolumeRequest {
	t.Helper()
	return &agentv1.ImportVolumeRequest{
		VolumeId:      testVolumeID,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		CapacityBytes: 1 << 30,
		Fence:         testFence(t),
	}
}

func TestImportVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: "/dev/zvol/tank/k8s/pvc-abc",
		importSize:       2 << 30, // larger than requested — adopted as-is
	}
	srv := newImportTestServer(t, mb)

	resp, err := srv.ImportVolume(context.Background(), importRequest(t))
	if err != nil {
		t.Fatalf("ImportVolume unexpected error: %v", err)
	}
	if resp.GetDevicePath() != "/dev/zvol/tank/k8s/pvc-abc" {
		t.Errorf("DevicePath = %q", resp.GetDevicePath())
	}
	if resp.GetCapacityBytes() != 2<<30 {
		t.Errorf("CapacityBytes = %d, want the zvol's existing size", resp.GetCapacityBytes())
	}
	if len(mb.importCalledWith) != 1 || mb.importCalledWith[0] != testVolumeID {
		t.Errorf("Import called with %v, want one call for %q", mb.importCalledWith, testVolumeID)
	}
}

// The import RPC must never create: a backend without the importer contract
// answers Unimplemented.
func TestImportVolume_BackendCannotImport(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &mockBackend{})

	_, err := srv.ImportVolume(context.Background(), importRequest(t))
	if err == nil {
		t.Fatal("expected Unimplemented for a non-importable backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

func TestImportVolume_RefusedIsFailedPrecondition(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend: &mockBackend{},
		importErr: &backend.ImportRefusedError{
			VolumeID: testVolumeID,
			Reason:   "in use",
			Detail:   "device is mounted at /data",
		},
	}
	srv := newImportTestServer(t, mb)

	_, err := srv.ImportVolume(context.Background(), importRequest(t))
	if err == nil {
		t.Fatal("expected FailedPrecondition, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

func TestImportVolume_InvalidVolumeID(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{mockBackend: &mockBackend{}}
	srv := newImportTestServer(t, mb)

	req := importRequest(t)
	req.VolumeId = "no-slash"
	_, err := srv.ImportVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for invalid volumeID, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
	if len(mb.importCalledWith) != 0 {
		t.Errorf("Import was called %d times for an invalid volumeID", len(mb.importCalledWith))
	}
}

func TestImportVolume_BackendTypeMismatch(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{mockBackend: &mockBackend{}}
	srv := newImportTestServer(t, mb)

	req := importRequest(t)
	req.BackendType = agentv1.BackendType_BACKEND_TYPE_LVM
	_, err := srv.ImportVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for mismatched backend_type, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// A second lifecycle must not be able to re-import the same zvol: the fence
// mark recorded by the first import rejects a different uid for the same
// agent volume ID.
func TestImportVolume_FencedAgainstSecondLifecycle(t *testing.T) {
	t.Parallel()
	mb := &mockImporterBackend{
		mockBackend:      &mockBackend{},
		importDevicePath: "/dev/zvol/tank/pvc-abc",
		importSize:       1 << 30,
	}
	srv := newImportTestServer(t, mb)

	if _, err := srv.ImportVolume(context.Background(), importRequest(t)); err != nil {
		t.Fatalf("first ImportVolume: %v", err)
	}
	req := importRequest(t)
	req.Fence = &agentv1.FencingToken{VolumeUid: "another-lifecycle", Generation: 1}
	_, err := srv.ImportVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected a fencing error for a second lifecycle, got nil")
	}
	if st, _ := status.FromError(err); st.Code() == codes.OK {
		t.Error("import by a second lifecycle must not succeed")
	}
}
