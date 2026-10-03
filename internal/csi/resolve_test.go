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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"

	"github.com/isac322/pillar-csi/api/v1alpha1"
)

func TestApplyProtocolOverride_NFSMemberCompatibility(t *testing.T) {
	t.Parallel()

	override := &v1alpha1.ProtocolOverrides{NFS: &v1alpha1.NFSOverrides{}}
	if err := applyProtocolOverride(
		&v1alpha1.ProtocolSpec{NFS: &v1alpha1.NFSConfig{}},
		override,
		"binding",
	); err != nil {
		t.Fatalf("matching NFS override: %v", err)
	}

	err := applyProtocolOverride(
		&v1alpha1.ProtocolSpec{ISCSI: &v1alpha1.ISCSIConfig{}},
		override,
		"binding",
	)
	if err == nil {
		t.Fatal("mismatched NFS override was accepted")
	}
}

// TestResolveFilesystem_GeneratedClassMountOptions verifies a generated class
// resolves an explicit mount list even when the live binding sets none, so
// the node applies no stale StorageClass mountOptions from the capability.
func TestResolveFilesystem_GeneratedClassMountOptions(t *testing.T) {
	t.Parallel()

	fs, err := resolveFilesystem(map[string]string{paramBinding: "b"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("resolveFilesystem: %v", err)
	}
	volCtx := map[string]string{}
	filesystemVolumeContext(fs, volCtx)
	flags, err := resolveMountFlags(volCtx, &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: []string{"noatime"}}},
	})
	if err != nil {
		t.Fatalf("resolveMountFlags: %v", err)
	}
	if len(flags) != 0 {
		t.Errorf("mount flags = %v, want none (stale StorageClass mountOptions)", flags)
	}

	hand, err := resolveFilesystem(map[string]string{paramStoreRef: "s", paramProtocolRef: "p"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("resolveFilesystem: %v", err)
	}
	if hand.MountOptions != nil {
		t.Errorf("hand-written class mountOptions = %v, want nil (StorageClass mountOptions apply)", *hand.MountOptions)
	}
}

// mountCaps returns a mount capability with fsType, as the
// external-provisioner builds it from csi.storage.k8s.io/fstype.
func mountCaps(fsType string) []*csi.VolumeCapability {
	return []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fsType}},
	}}
}

// TestResolveFilesystem_FSTypeSources pins where the resolved fsType comes
// from: a generated class follows the live binding (its StorageClass fstype,
// delivered as the capability fsType, is only a possibly stale copy), a
// hand-written class uses its fstype (the capability fsType, or the
// parameter when a caller sends it directly) over the ext4 default, and the
// PVC document wins over both.
func TestResolveFilesystem_FSTypeSources(t *testing.T) {
	t.Parallel()

	xfs := &v1alpha1.FilesystemConfig{FSType: xfsFsType}
	ext4 := &v1alpha1.FilesystemConfig{FSType: defaultFsType}
	hand := map[string]string{paramStoreRef: "s", paramProtocolRef: "p"}
	tests := []struct {
		name     string
		scParams map[string]string
		capFS    string
		classFS  *v1alpha1.FilesystemConfig
		pvcFS    *v1alpha1.FilesystemConfig
		want     string
	}{
		{
			name:     "generated class ignores stale fstype when binding has no filesystem",
			scParams: map[string]string{paramBinding: "b"},
			capFS:    xfsFsType,
			want:     defaultFsType,
		},
		{
			name:     "generated class follows live binding",
			scParams: map[string]string{paramBinding: "b"},
			capFS:    xfsFsType,
			classFS:  ext4,
			want:     defaultFsType,
		},
		{name: "hand-written class uses capability fstype", scParams: hand, capFS: xfsFsType, want: xfsFsType},
		{name: "hand-written class without fstype defaults to ext4", scParams: hand, want: defaultFsType},
		{
			name:     "hand-written class uses fstype parameter sent directly",
			scParams: map[string]string{paramStoreRef: "s", paramProtocolRef: "p", paramFSTypeSC: xfsFsType},
			want:     xfsFsType,
		},
		{name: "PVC document beats capability fstype", scParams: hand, capFS: xfsFsType, pvcFS: ext4, want: defaultFsType},
		{
			name:     "PVC document wins",
			scParams: map[string]string{paramBinding: "b"},
			classFS:  ext4,
			pvcFS:    xfs,
			want:     xfsFsType,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs, err := resolveFilesystem(tt.scParams, mountCaps(tt.capFS), tt.classFS, tt.pvcFS)
			if err != nil {
				t.Fatalf("resolveFilesystem: %v", err)
			}
			if fs.FSType != tt.want {
				t.Errorf("fsType = %q, want %q", fs.FSType, tt.want)
			}
		})
	}
}

// TestResolveFilesystem_FSTypeConflicts verifies a hand-written class's
// fstype must agree with the filesystem document and with a directly sent
// parameter, and must be supported.
func TestResolveFilesystem_FSTypeConflicts(t *testing.T) {
	t.Parallel()
	hand := map[string]string{paramStoreRef: "s", paramProtocolRef: "p"}
	for name, tc := range map[string]struct {
		scParams map[string]string
		caps     []*csi.VolumeCapability
		classFS  *v1alpha1.FilesystemConfig
	}{
		"capability vs class document": {
			scParams: hand, caps: mountCaps(xfsFsType),
			classFS: &v1alpha1.FilesystemConfig{FSType: defaultFsType},
		},
		"capability vs parameter": {
			scParams: map[string]string{paramStoreRef: "s", paramProtocolRef: "p", paramFSTypeSC: defaultFsType},
			caps:     mountCaps(xfsFsType),
		},
		"unsupported capability fstype": {scParams: hand, caps: mountCaps("btrfs")},
		"capabilities disagree":         {scParams: hand, caps: append(mountCaps(xfsFsType), mountCaps(defaultFsType)...)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveFilesystem(tc.scParams, tc.caps, tc.classFS, nil)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("resolveFilesystem err = %v, want InvalidArgument", err)
			}
		})
	}
}

// provisionerMountRequest turns req into what the external-provisioner sends
// for a hand-written class with csi.storage.k8s.io/fstype fsType: the key is
// stripped from the parameters and its value is the mount capability fsType.
func provisionerMountRequest(req *csi.CreateVolumeRequest, fsType string) *csi.CreateVolumeRequest {
	delete(req.Parameters, paramFSTypeSC)
	req.VolumeCapabilities = []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fsType}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
	return req
}

// TestCreateVolume_HandWrittenClassFSTypeFromCapability is the regression
// test for a hand-written class with csi.storage.k8s.io/fstype: xfs that
// provisioned ext4: the fsType reaches CreateVolume only as the mount
// capability fsType, which must be resolved and handed to the node.
func TestCreateVolume_HandWrittenClassFSTypeFromCapability(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := provisionerMountRequest(baseCreateVolumeRequest(), xfsFsType)

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetVolumeContext()[paramFSType]; got != xfsFsType {
		t.Errorf("VolumeContext[%s] = %q, want xfs", paramFSType, got)
	}
	if got := loadResolved(t, env, req.GetName()).Filesystem.FSType; got != xfsFsType {
		t.Errorf("spec.resolved.filesystem.fsType = %q, want xfs", got)
	}
}

// TestCreateVolume_PVCFilesystemDocBeatsCapabilityFSType verifies the PVC
// filesystem document overrides the class fstype carried by the capability.
func TestCreateVolume_PVCFilesystemDocBeatsCapabilityFSType(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-fs",
		map[string]string{paramFilesystemDoc: "fsType: ext4\n"})
	req = provisionerMountRequest(req, xfsFsType)

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetVolumeContext()[paramFSType]; got != defaultFsType {
		t.Errorf("VolumeContext[%s] = %q, want the PVC document's ext4", paramFSType, got)
	}
}

func TestResolveClassLayer_ExplicitDriverSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, selected, controller string
		want                       codes.Code
	}{
		{"default old", "", v1alpha1.DefaultCSIDriver, codes.OK},
		{"default cannot route files", "", v1alpha1.FileCSIDriver, codes.InvalidArgument},
		{"explicit files", v1alpha1.FileCSIDriver, v1alpha1.FileCSIDriver, codes.OK},
		{"explicit files cannot route old", v1alpha1.FileCSIDriver, v1alpha1.DefaultCSIDriver, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := &v1alpha1.PillarStorageClass{Name: "selected", Spec: v1alpha1.PillarStorageClassSpec{
				CSIDriver: tc.selected, StoreRef: testStoreName, ProtocolRef: testProtocolName}}
			env := newControllerTestEnv(t, binding)
			env.srv.driverName = tc.controller
			_, err := env.srv.resolveClassLayer(context.Background(), map[string]string{paramBinding: "selected"}, true)
			if status.Code(err) != tc.want {
				t.Fatalf("binding routing error %v, want %v", err, tc.want)
			}
			// A replay must also reject a live binding pointed at another driver.
			_, err = env.srv.resolveClassLayer(context.Background(), map[string]string{paramBinding: "selected"}, false)
			if status.Code(err) != tc.want {
				t.Fatalf("binding retry routing error %v, want %v", err, tc.want)
			}
		})
	}
}

func TestResolveFilesystem_FileDriverLocalAttachDoesNotChangeOldNFS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		driver string
		want   codes.Code
	}{
		{v1alpha1.DefaultCSIDriver, codes.InvalidArgument},
		{v1alpha1.FileCSIDriver, codes.OK},
	} {
		class := &classLayer{driverName: tc.driver, localAttach: true}
		fs, err := resolveProtocolFilesystem(nil, nil, class, nil, v1alpha1.ProtocolIDNFS)
		if status.Code(err) != tc.want {
			t.Fatalf("%s localAttach: %v", tc.driver, err)
		}
		if err == nil && fs.FSType != ProtocolNFS {
			t.Fatalf("NFS resolution changed to %+v", fs)
		}
	}
}

func TestApplyBackendOverride_DirectoryMemberCompatibility(t *testing.T) {
	t.Parallel()
	_, resolved := filesystemDirectoryFixture()
	override := &v1alpha1.BackendOverrides{Directory: &v1alpha1.DirectoryBackendOverrides{}}
	if err := applyBackendOverride(&resolved.Backend, override, "binding"); err != nil {
		t.Fatalf("matching directory override refused: %v", err)
	}
	other := v1alpha1.BackendSpec{LVM: &v1alpha1.LVMBackendConfig{VolumeGroup: "data"}}
	if err := applyBackendOverride(&other, override, "binding"); err == nil {
		t.Fatal("directory override silently applied to LVM")
	}
}

func TestResolveVolumeConfig_DirectoryRequiresFilesDriver(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{v1alpha1.DefaultCSIDriver, v1alpha1.FileCSIDriver} {
		t.Run(driver, func(t *testing.T) {
			env := newControllerTestEnv(t,
				&v1alpha1.PillarStore{Name: "directories", Spec: v1alpha1.PillarStoreSpec{
					AgentRef: "storage-node-1", Backend: v1alpha1.BackendSpec{Directory: &v1alpha1.DirectoryBackendConfig{
						LogicalPool: "imports", HostRoot: "/srv/imports"}}}},
				&v1alpha1.PillarProtocol{Name: "file-nfs", Spec: v1alpha1.PillarProtocolSpec{
					Protocol: v1alpha1.ProtocolSpec{NFS: &v1alpha1.NFSConfig{}}}})
			env.srv.driverName = driver
			params := map[string]string{paramStoreRef: "directories", paramProtocolRef: "file-nfs"}
			got, err := env.srv.resolveVolumeConfig(context.Background(), params, nil, nil)
			if driver == v1alpha1.DefaultCSIDriver {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("old driver directory resolution: %v", err)
				}
			} else if err != nil || got.resolved.Backend.Directory.HostRoot != "/srv/imports" {
				t.Fatalf("files driver lost trusted directory layout: %+v %v", got, err)
			}
		})
	}
}

func TestReplayResolution_PreservesFilesystemSelectors(t *testing.T) {
	t.Parallel()
	class := &classLayer{protocol: &v1alpha1.PillarProtocol{Name: "nfs"}}
	protocol := v1alpha1.ProtocolSpec{NFS: &v1alpha1.NFSConfig{}}
	_, recorded := filesystemDirectoryFixture()
	recorded.Protocol = protocol
	replayed, err := replayResolution(class, pvcDocs{ImportDirectory: "/srv/imports/app"}, protocol, recorded)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.importDirectory != "/srv/imports/app" || replayed.importZFSDataset != "" || replayed.importDataset != "" {
		t.Fatalf("retry lost filesystem adoption intent: %+v", replayed)
	}
}

func TestClaimDocs_CompletedLifecycleAllowsOnlyMissingClaim(t *testing.T) {
	t.Parallel()
	params := map[string]string{paramPVCNameMeta: "retired", paramPVCNamespaceMeta: "default"}
	env := newControllerTestEnv(t)
	if _, err := env.srv.claimDocs(context.Background(), params, false); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("initial resolution silently dropped absent claim: %v", err)
	}
	docs, err := env.srv.claimDocs(context.Background(), params, true)
	if err != nil || docs.ImportDirectory != "" || docs.ImportZFSDataset != "" || docs.ImportZvol != "" {
		t.Fatalf("completed lifecycle cannot reuse stored adoption after claim removal: %+v %v", docs, err)
	}
	pvc := &corev1.PersistentVolumeClaim{Name: "retired", Namespace: "default",
		Annotations: map[string]string{v1alpha1.AnnotationImportDirectory: ""}}
	if err := env.srv.k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	for _, allowAbsent := range []bool{false, true} {
		if _, err := env.srv.claimDocs(context.Background(), params, allowAbsent); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("allowAbsent=%v bypassed current claim selector validation: %v", allowAbsent, err)
		}
	}
}
