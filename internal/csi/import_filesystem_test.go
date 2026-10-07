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
	"io"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// This mock exercises the controller contract without provisioning a source.
type filesystemInspectionClient struct {
	agentv1.AgentServiceClient
	response *agentv1.InspectImportResponse
	err      error
	requests []*agentv1.InspectImportRequest
}

func (m *filesystemInspectionClient) InspectImport(_ context.Context, req *agentv1.InspectImportRequest,
	_ ...grpc.CallOption) (*agentv1.InspectImportResponse, error) {
	m.requests = append(m.requests, req)
	return m.response, m.err
}

func filesystemDirectoryFixture() (*v1alpha1.FilesystemAdoption, *v1alpha1.ResolvedVolumeConfig) {
	const uuid = "12345678-1234-1234-1234-123456789abc"
	return &v1alpha1.FilesystemAdoption{Kind: v1alpha1.FilesystemAdoptionKindDirectory,
			CanonicalSource: "/srv/imports/app", ResourceID: uuid + ":42", FilesystemType: "ext4",
			FilesystemID: uuid, Inode: "42", ProjectID: 77},
		&v1alpha1.ResolvedVolumeConfig{Backend: v1alpha1.BackendSpec{Directory: &v1alpha1.DirectoryBackendConfig{
			LogicalPool: "files-a", HostRoot: "/srv/imports"}}}
}

func requireFilesystemImportLeaf(t *testing.T, a *v1alpha1.FilesystemAdoption) string {
	t.Helper()
	leaf, err := filesystemImportLeaf(a)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func requireFilesystemResourceID(t *testing.T, a *v1alpha1.FilesystemAdoption) string {
	t.Helper()
	resourceID, err := filesystemResourceID(a)
	if err != nil {
		t.Fatal(err)
	}
	return resourceID
}

func filesystemInspectionEnv(
	t *testing.T, a *v1alpha1.FilesystemAdoption,
) (*controllerTestEnv, *filesystemInspectionClient) {
	t.Helper()
	env := newControllerTestEnv(t)
	env.srv.driverName = v1alpha1.FileCSIDriver
	adoption, err := filesystemAdoptionProto(a)
	if err != nil {
		t.Fatal(err)
	}
	mock := &filesystemInspectionClient{response: &agentv1.InspectImportResponse{
		FilesystemAdoption: adoption, CapacityBytes: 4096}}
	env.srv.dialAgent = func(_ context.Context, address string) (agentv1.AgentServiceClient, io.Closer, error) {
		if address != "192.168.1.10:9500" {
			t.Errorf("Inspect dial redirected to %q", address)
		}
		return mock, nopCloser{}, nil
	}
	return env, mock
}

func TestFilesystemImport_InspectExactIdentityAndQuota(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{
		"valid", "wrong quota", "wrong native identity", "outside root", "in-root redirect", "wrong kind", "inspect error",
	} {
		t.Run(mutation, func(t *testing.T) {
			a, resolved := filesystemDirectoryFixture()
			env, mock := filesystemInspectionEnv(t, a)
			switch mutation {
			case "wrong quota":
				mock.response.CapacityBytes++
			case "wrong native identity":
				mock.response.FilesystemAdoption.ResourceId = "volatile-device-7"
			case "outside root":
				mock.response.FilesystemAdoption.CanonicalSource = "/srv/imports-other/app"
			case "in-root redirect":
				mock.response.FilesystemAdoption.CanonicalSource = "/srv/imports/another-app"
			case "wrong kind":
				mock.response.FilesystemAdoption.Kind = "zfs-dataset"
			case "inspect error":
				mock.err = status.Error(codes.FailedPrecondition, "source missing")
			}
			adoption, leaf, err := env.srv.resolveFilesystemImportRequest(context.Background(), "pv", false, nil,
				resolved, a.CanonicalSource, "", "storage-node-1", 4096)
			requireFilesystemInspectionResult(t, mutation, adoption, leaf, err, a)
			req := mock.requests[0]
			if req.PoolName != "files-a" || req.ExpectedHostRoot != "/srv/imports" || req.RequiredBytes != 4096 ||
				req.BackendType != agentv1.BackendType_BACKEND_TYPE_DIRECTORY || req.Source != a.CanonicalSource {
				t.Fatalf("inspection lost trusted layout: %+v", req)
			}
			requireFilesystemInspectionReadOnly(t, env)
		})
	}
}

func requireFilesystemInspectionResult(
	t *testing.T, mutation string, adoption *v1alpha1.FilesystemAdoption, leaf string,
	err error, expected *v1alpha1.FilesystemAdoption,
) {
	t.Helper()
	if mutation != "valid" {
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("unsafe inspection accepted: adoption %+v, leaf %q, error %v", adoption, leaf, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if *adoption != *expected || leaf != requireFilesystemImportLeaf(t, expected) {
		t.Fatalf("identity changed: %+v leaf %q", adoption, leaf)
	}
}

func requireFilesystemInspectionReadOnly(t *testing.T, env *controllerTestEnv) {
	t.Helper()
	var states v1alpha1.PillarVolumeStateList
	var reservations v1alpha1.PillarVolumeReservationList
	if err := env.srv.k8sClient.List(context.Background(), &states); err != nil {
		t.Fatal(err)
	}
	if err := env.srv.k8sClient.List(context.Background(), &reservations); err != nil {
		t.Fatal(err)
	}
	if len(states.Items) != 0 || len(reservations.Items) != 0 {
		t.Fatal("read-only inspection wrote lifecycle or reservation")
	}
}

func TestFilesystemImport_RecordedSourceAndIdentityAreImmutable(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{
		"same", "absent selector", "source redirect", "kind redirect", "identity drift", "quota drift",
	} {
		t.Run(mutation, func(t *testing.T) {
			a, resolved := filesystemDirectoryFixture()
			env, mock := filesystemInspectionEnv(t, a)
			pvs := &v1alpha1.PillarVolumeState{Name: "pv", Spec: v1alpha1.PillarVolumeStateSpec{
				AgentRef: "storage-node-1", AgentVolumeID: "files-a/" + requireFilesystemImportLeaf(t, a),
				FilesystemAdoption: a, Resolved: resolved, CapacityBytes: 4096}}
			directory, dataset := a.CanonicalSource, ""
			want := codes.OK
			switch mutation {
			case "absent selector":
				directory = ""
			case "source redirect":
				directory, want = "/srv/imports/elsewhere", codes.InvalidArgument
			case "kind redirect":
				directory, dataset, want = "", "tank/app", codes.InvalidArgument
			case "identity drift":
				mock.response.FilesystemAdoption.ProjectId++
				want = codes.FailedPrecondition
			case "quota drift":
				mock.response.CapacityBytes++
				want = codes.FailedPrecondition
			}
			adoption, _, err := env.srv.resolveFilesystemImportRequest(context.Background(), "pv", true, pvs,
				resolved, directory, dataset, "different-agent", 4096)
			if status.Code(err) != want {
				t.Fatalf("retry code %v, want %v", err, want)
			}
			if want == codes.OK && *adoption != *a {
				t.Fatalf("retry changed stored identity %+v", adoption)
			}
			if len(mock.requests) > 0 && mock.requests[0].Source != a.CanonicalSource {
				t.Fatal("retry redirected source")
			}
		})
	}
}

func TestFilesystemImport_DriverIsolation(t *testing.T) {
	t.Parallel()
	a, resolved := filesystemDirectoryFixture()
	for _, tc := range []struct {
		name, driver, directory string
		existing                *v1alpha1.PillarVolumeState
		want                    codes.Code
	}{
		{name: "old dynamic", driver: v1alpha1.DefaultCSIDriver, want: codes.OK},
		{name: "old file selector", driver: v1alpha1.DefaultCSIDriver,
			directory: a.CanonicalSource, want: codes.InvalidArgument},
		{name: "old recorded file", driver: v1alpha1.DefaultCSIDriver,
			existing: &v1alpha1.PillarVolumeState{Spec: v1alpha1.PillarVolumeStateSpec{FilesystemAdoption: a}},
			want:     codes.InvalidArgument},
		{name: "files dynamic", driver: v1alpha1.FileCSIDriver, want: codes.InvalidArgument},
		{name: "files old lifecycle", driver: v1alpha1.FileCSIDriver,
			existing: &v1alpha1.PillarVolumeState{}, want: codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, mock := filesystemInspectionEnv(t, a)
			env.srv.driverName = tc.driver
			_, _, err := env.srv.resolveFilesystemImportRequest(context.Background(), "pv", tc.existing != nil, tc.existing,
				resolved, tc.directory, "", "storage-node-1", 4096)
			if status.Code(err) != tc.want {
				t.Fatalf("driver isolation err %v, want %v", err, tc.want)
			}
			if len(mock.requests) != 0 {
				t.Fatal("wrong driver reached inspection")
			}
		})
	}
	old := &v1alpha1.PillarVolumeState{}
	file := &v1alpha1.PillarVolumeState{Spec: v1alpha1.PillarVolumeStateSpec{FilesystemAdoption: a}}
	env, _ := filesystemInspectionEnv(t, a)
	if status.Code(env.srv.validateVolumeDriver(old)) != codes.NotFound {
		t.Fatal("files driver accepted old handle")
	}
	env.srv.driverName = v1alpha1.DefaultCSIDriver
	if status.Code(env.srv.validateVolumeDriver(file)) != codes.NotFound {
		t.Fatal("old driver accepted file handle")
	}
}

func TestFilesystemReservation_AliasPoolsHaveOneAtomicOwner(t *testing.T) {
	t.Parallel()
	a, resolved := filesystemDirectoryFixture()
	env, _ := filesystemInspectionEnv(t, a)
	native := requireFilesystemResourceID(t, a)
	leaf := requireFilesystemImportLeaf(t, a)
	claim := &v1alpha1.VolumeClaimRef{Namespace: "default", Name: "app", UID: "claim-uid"}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, pool := range []string{"files-a", "files-b"} {
		workers.Go(func() {
			<-start
			results <- env.srv.reserveBackendVolume(context.Background(), "pv-"+pool, backendReservation{
				agent: "storage-node-1", backendType: "directory", key: pool + "/" + leaf, resourceID: native,
			}, filesystemImportSubject(a.CanonicalSource), claim)
		})
	}
	close(start)
	workers.Wait()
	close(results)
	success, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case status.Code(err) == codes.FailedPrecondition:
			refused++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || refused != 1 {
		t.Fatalf("atomic native ownership: winners %d refused %d", success, refused)
	}
	var list v1alpha1.PillarVolumeReservationList
	if err := env.srv.k8sClient.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Spec.FilesystemResourceID != native {
		t.Fatalf("native reservation %+v", list.Items)
	}
	held := list.Items[0]
	if held.Spec.OwnerVolume == "pv-files-b" {
		resolved.Backend.Directory.LogicalPool = "files-b"
	}
	volumeID := "storage-node-1/nfs/directory/" + held.Spec.AgentVolumeID
	pvs := &v1alpha1.PillarVolumeState{Name: held.Spec.OwnerVolume, Spec: v1alpha1.PillarVolumeStateSpec{
		AgentRef: held.Spec.AgentRef, AgentVolumeID: held.Spec.AgentVolumeID, FilesystemAdoption: a, Resolved: resolved,
		ClaimRef: claim, BackendType: "directory", VolumeID: volumeID},
		Status: v1alpha1.PillarVolumeStateStatus{Deleting: true}}
	if err := env.srv.k8sClient.Create(context.Background(), pvs); err != nil {
		t.Fatal(err)
	}
	if err := env.srv.releaseBackendVolume(context.Background(), pvs.Name, volumeID, pvs.UID,
		reservationOf(pvs)); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.PillarVolumeReservation{}
	if err := env.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: held.Name}, got); err == nil {
		t.Fatal("canonical reservation was not released")
	}
	// The same identity on another agent is not the same backing resource.
	if err := env.srv.reserveBackendVolume(context.Background(), "pv-other", backendReservation{
		agent: "storage-node-2", backendType: "directory", key: "files-a/leaf", resourceID: native,
	}, filesystemImportSubject(a.CanonicalSource), nil); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemImport_ConflictScanIgnoresPoolAliases(t *testing.T) {
	t.Parallel()
	a, resolved := filesystemDirectoryFixture()
	env, _ := filesystemInspectionEnv(t, a)
	// The cache deliberately lacks the owner's newly committed state.
	authoritative := newControllerTestEnv(t)
	env.srv.apiReader = authoritative.srv.k8sClient
	owner := &v1alpha1.PillarVolumeState{Name: "owner", Spec: v1alpha1.PillarVolumeStateSpec{
		AgentRef: "storage-node-1", AgentVolumeID: "alias-pool/" + requireFilesystemImportLeaf(t, a),
		FilesystemAdoption: a,
		VolumeID:           "storage-node-1/nfs/directory/alias-pool/" + requireFilesystemImportLeaf(t, a)}}
	if err := authoritative.srv.k8sClient.Create(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	_, _, err := env.srv.resolveFilesystemImportRequest(context.Background(), "contender", false, nil, resolved,
		a.CanonicalSource, "", "storage-node-1", 4096)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("pool alias conflict accepted: %v", err)
	}
}

func TestFilesystemImport_RefusesLiveLegacyZFSDataset(t *testing.T) {
	t.Parallel()
	env, req := newFilesystemImportCreateEnv(
		t, filesystemImportZFSDatasetKey, "tank/app-data")
	legacyMeta := metav1.ObjectMeta{Name: "legacy-zfs-dataset"}
	legacy := &v1alpha1.PillarVolumeState{
		ObjectMeta: legacyMeta,
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nfs/zfs-dataset/tank/app-data",
			AgentVolumeID: "tank/app-data",
			AgentRef:      "storage-node-1",
			BackendType:   string(v1alpha1.BackendIDZFSDataset),
			ProtocolType:  string(v1alpha1.ProtocolIDNFS),
			Resolved: &v1alpha1.ResolvedVolumeConfig{
				Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
					Pool: "tank",
				}},
			},
		},
		Status: v1alpha1.PillarVolumeStateStatus{
			Phase:          v1alpha1.PillarVolumeStatePhaseReady,
			ImportAcquired: true,
		},
	}
	if err := env.srv.k8sClient.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	before := legacy.DeepCopy()

	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("legacy dataset conflict code = %v, want FailedPrecondition (err=%v)",
			status.Code(err), err)
	}
	if env.agent.createVolumeCalls != 0 || env.agent.importVolumeCalls != 0 ||
		env.agent.exportVolumeCalls != 0 {
		t.Fatalf("legacy conflict reached mutating backend calls: create=%d import=%d export=%d",
			env.agent.createVolumeCalls, env.agent.importVolumeCalls, env.agent.exportVolumeCalls)
	}
	after := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: legacy.Name}, after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Spec, before.Spec) ||
		!reflect.DeepEqual(after.Status, before.Status) {
		t.Fatalf("legacy lifecycle changed after refused adoption: before=%+v/%+v after=%+v/%+v",
			before.Spec, before.Status, after.Spec, after.Status)
	}
}

func TestFilesystemAdoption_DescriptorValidation(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"volatile ID", "uppercase UUID", "zero inode", "unbounded project",
		"duplicate host path", "native composite mismatch", "root itself", "noncanonical source"} {
		t.Run(mutation, func(t *testing.T) {
			a, resolved := filesystemDirectoryFixture()
			switch mutation {
			case "volatile ID":
				a.FilesystemID = "dev-8:1"
			case "uppercase UUID":
				a.FilesystemID = "12345678-1234-1234-1234-123456789ABC"
			case "zero inode":
				a.Inode = "0"
			case "unbounded project":
				a.ProjectID = 0
			case "duplicate host path":
				a.HostPath = a.CanonicalSource
			case "native composite mismatch":
				a.ResourceID = a.FilesystemID + ":43"
			case "root itself":
				resolved.Backend.Directory.HostRoot, a.CanonicalSource = "/", "/"
			case "noncanonical source":
				a.CanonicalSource = "/srv/imports/../imports/app"
			}
			if err := validateFilesystemAdoption(a, resolved); err == nil {
				t.Fatalf("unsafe directory descriptor accepted: %+v", a)
			}
		})
	}
}

func TestFilesystemAdoption_DatasetDescriptorValidation(t *testing.T) {
	t.Parallel()
	resolved := &v1alpha1.ResolvedVolumeConfig{Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
		Pool: "tank", ParentDataset: "imports", VolumeType: v1alpha1.ZFSVolumeTypeDataset}}}
	for _, mutation := range []string{
		"valid", "unmounted", "zero GUID", "leading zero GUID", "directory fields",
		"wrong pool", "nested source", "relative mount", "noncanonical mount",
	} {
		t.Run("dataset "+mutation, func(t *testing.T) {
			a := &v1alpha1.FilesystemAdoption{Kind: v1alpha1.FilesystemAdoptionKindZFSDataset,
				CanonicalSource: "tank/imports/app", ResourceID: "123456789", HostPath: "/mnt/tank/app", FilesystemType: "zfs"}
			switch mutation {
			case "unmounted":
				a.HostPath = ""
			case "zero GUID":
				a.ResourceID = "0"
			case "leading zero GUID":
				a.ResourceID = "0123456789"
			case "directory fields":
				a.ProjectID = 3
			case "wrong pool":
				a.CanonicalSource = "other/imports/app"
			case "nested source":
				a.CanonicalSource = "tank/imports/sub/app"
			case "relative mount":
				a.HostPath = "mnt/app"
			case "noncanonical mount":
				a.HostPath = "/mnt/tank/../app"
			}
			err := validateFilesystemAdoption(a, resolved)
			if (err == nil) != (mutation == "valid" || mutation == "unmounted") {
				t.Fatalf("dataset descriptor %+v: %v", a, err)
			}
		})
	}
}

func TestFilesystemImport_DirectoryInputMustBeCanonical(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"srv/imports/app", "/srv/imports/../imports/app", "/srv/imports//app", "/srv/imports/app/",
	} {
		t.Run(source, func(t *testing.T) {
			a, resolved := filesystemDirectoryFixture()
			env, mock := filesystemInspectionEnv(t, a)
			_, _, err := env.srv.resolveFilesystemImportRequest(context.Background(), "pv", false, nil, resolved,
				source, "", "storage-node-1", 4096)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("noncanonical source accepted: %v", err)
			}
			if len(mock.requests) != 0 {
				t.Fatal("noncanonical directory input reached agent inspection")
			}
		})
	}
}

func TestFilesystemImport_UnmountedDatasetKeepsNativeIdentityAndExactQuota(t *testing.T) {
	t.Parallel()
	a := &v1alpha1.FilesystemAdoption{Kind: v1alpha1.FilesystemAdoptionKindZFSDataset,
		CanonicalSource: "tank/imports/app", ResourceID: "123456789", FilesystemType: "zfs"}
	resolved := &v1alpha1.ResolvedVolumeConfig{Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
		Pool: "tank", ParentDataset: "imports", VolumeType: v1alpha1.ZFSVolumeTypeDataset}}}
	env, mock := filesystemInspectionEnv(t, a)
	got, leaf, err := env.srv.resolveFilesystemImportRequest(context.Background(), "pv", false, nil, resolved,
		"", a.CanonicalSource, "storage-node-1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *a || leaf != requireFilesystemImportLeaf(t, a) {
		t.Fatalf("unmounted dataset identity changed: %+v %q", got, leaf)
	}
	if mock.requests[0].ExpectedParentDataset != "imports" {
		t.Fatal("inspection lost trusted parent dataset")
	}
	mock.response.CapacityBytes++
	if _, _, err := env.srv.resolveFilesystemImportRequest(context.Background(), "pv", false, nil, resolved,
		"", a.CanonicalSource, "storage-node-1", 4096); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unmounted dataset quota mismatch accepted: %v", err)
	}
	mock.response.CapacityBytes = 4096
	mock.response.FilesystemAdoption.HostPath = "/mnt/tank/../app"
	if _, _, err := env.srv.resolveFilesystemImportRequest(context.Background(), "pv", false, nil, resolved,
		"", a.CanonicalSource, "storage-node-1", 4096); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("noncanonical dataset host path accepted: %v", err)
	}
}
