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

// Filesystem adoption contract tests cover both directory and ZFS-dataset
// selectors.  The mock agent performs the same two-step read-only inspection
// and import calls as a real agent; no create call is a valid adoption path.

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	kserrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const (
	filesystemImportDriverName    = "files.pillar-csi.bhyoo.com"
	filesystemImportDirectoryKey  = "pillar-csi.bhyoo.com/import-directory"
	filesystemImportZFSDatasetKey = "pillar-csi.bhyoo.com/import-zfs-dataset"
)

func newFilesystemImportCreateEnv(
	t *testing.T,
	selector, source string,
) (*controllerTestEnv, *csipb.CreateVolumeRequest) {
	t.Helper()
	env := newFilesystemImportEnv(t, selector, source)
	req := baseCreateVolumeRequest()
	req.Name = "pvc-filesystem-import"
	req.Parameters[paramStoreRef] = "nfs-datasets"
	req.Parameters[paramProtocolRef] = "nfs"
	req.Parameters[paramPVCNameMeta] = "filesystem-import"
	req.Parameters[paramPVCNamespaceMeta] = "default"
	req.VolumeCapabilities = []*csipb.VolumeCapability{
		nfsVolumeCapability(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	}
	return env, req
}

func newFilesystemImportEnv(t *testing.T, selector, source string) *controllerTestEnv {
	t.Helper()

	backendSpec := v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
		VolumeType: v1alpha1.ZFSVolumeTypeDataset,
		Pool:       "tank",
	}}
	inspectResp := &agentv1.InspectImportResponse{
		FilesystemAdoption: &agentv1.FilesystemAdoption{
			Kind:            string(v1alpha1.FilesystemAdoptionKindZFSDataset),
			CanonicalSource: source,
			ResourceId:      "12345",
			HostPath:        "/var/lib/pillar-csi/datasets/app-data",
			FilesystemType:  "zfs",
		},
		CapacityBytes: 1073741824,
	}
	importDevicePath := "/var/lib/pillar-csi/datasets/app-data"
	if selector == filesystemImportDirectoryKey {
		backendSpec = v1alpha1.BackendSpec{Directory: &v1alpha1.DirectoryBackendConfig{
			LogicalPool: "files",
			HostRoot:    "/var/lib/pillar-csi/datasets",
		}}
		const filesystemID = "01234567-89ab-cdef-0123-456789abcdef"
		inspectResp.FilesystemAdoption = &agentv1.FilesystemAdoption{
			Kind:            string(v1alpha1.FilesystemAdoptionKindDirectory),
			CanonicalSource: source,
			ResourceId:      filesystemID + ":42",
			FilesystemType:  "ext4",
			FilesystemId:    filesystemID,
			Inode:           42,
			ProjectId:       1234,
		}
		importDevicePath = source
	}
	workerNode1Meta := metav1.ObjectMeta{
		Name:   "worker-node-1",
		Labels: map[string]string{corev1.LabelHostname: "worker-node-1"},
	}
	workerNode2Meta := metav1.ObjectMeta{
		Name:   "worker-node-2",
		Labels: map[string]string{corev1.LabelHostname: "worker-node-2"},
	}
	env := newControllerTestEnv(t,
		&v1alpha1.PillarStore{
			Name: "nfs-datasets",
			Spec: v1alpha1.PillarStoreSpec{
				AgentRef: "storage-node-1",
				Backend:  backendSpec,
			},
		},
		&v1alpha1.PillarProtocol{
			Name: "nfs",
			Spec: v1alpha1.PillarProtocolSpec{
				Protocol: v1alpha1.ProtocolSpec{NFS: &v1alpha1.NFSConfig{}},
			},
		},
		&corev1.Node{
			ObjectMeta: workerNode1Meta,
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.20"},
			}},
		},
		&corev1.Node{
			ObjectMeta: workerNode2Meta,
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.21"},
			}},
		},
	)
	env.srv.driverName = filesystemImportDriverName
	env.agent.inspectImportResp = inspectResp
	env.agent.importVolumeResp = &agentv1.ImportVolumeResponse{
		DevicePath:    importDevicePath,
		CapacityBytes: 1073741824,
	}
	env.agent.exportVolumeResp = &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  "/app-data",
			Address:   "192.168.1.10",
			Port:      2049,
			VolumeRef: "/app-data",
		},
	}
	const (
		pvcNamespace = "default"
		pvcName      = "filesystem-import"
	)
	pvcMeta := metav1.ObjectMeta{
		Name: pvcName, Namespace: pvcNamespace, UID: types.UID("11111111-2222-3333-4444-555566667777"),
		Annotations: map[string]string{selector: source},
	}
	if err := env.srv.k8sClient.Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: pvcMeta,
	}); err != nil {
		t.Fatalf("create PVC fixture: %v", err)
	}
	return env
}

func TestCreateVolume_FilesystemAdoptionSelectors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		key    string
		source string
	}{
		"directory": {
			key:    filesystemImportDirectoryKey,
			source: "/var/lib/pillar-csi/datasets/app-data",
		},
		"zfs filesystem dataset": {
			key:    filesystemImportZFSDatasetKey,
			source: "tank/app-data",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, req := newFilesystemImportCreateEnv(t, tc.key, tc.source)
			resp, err := env.srv.CreateVolume(context.Background(), req)
			if err != nil {
				t.Fatalf("CreateVolume(%q): valid filesystem adoption intent was refused: %v", tc.key, err)
			}
			if resp == nil || resp.GetVolume() == nil {
				t.Fatalf("CreateVolume(%q) returned no volume for a valid adoption intent: %+v", tc.key, resp)
			}
			if got, want := resp.GetVolume().GetCapacityBytes(), req.GetCapacityRange().GetRequiredBytes(); got != want {
				t.Fatalf("CreateVolume(%q) capacity = %d, want exact %d", tc.key, got, want)
			}
			if env.agent.createVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
				t.Fatalf("destructive backend calls = create %d/delete %d, want none for existing filesystem adoption",
					env.agent.createVolumeCalls, env.agent.deleteVolumeCalls)
			}

			pvs := filesystemPVS(t, env, req.GetName())
			requireFilesystemAdoptionState(t, env, req, resp, pvs, tc.source)
			requireFilesystemAdoptionInspection(t, env, req, tc.key, tc.source)
		})
	}
}

func requireFilesystemAdoptionState(
	t *testing.T, env *controllerTestEnv, req *csipb.CreateVolumeRequest,
	resp *csipb.CreateVolumeResponse, pvs *v1alpha1.PillarVolumeState, source string,
) {
	t.Helper()
	adoption := pvs.Spec.FilesystemAdoption
	if adoption == nil {
		t.Fatalf("adopted PillarVolumeState has no filesystem descriptor: %+v", pvs.Spec)
	}
	if adoption.CanonicalSource != source {
		t.Errorf("canonical source = %q, want requested source %q", adoption.CanonicalSource, source)
	}
	if pvs.Spec.CapacityBytes != req.GetCapacityRange().GetRequiredBytes() {
		t.Errorf("recorded capacity = %d, want exact %d", pvs.Spec.CapacityBytes, req.GetCapacityRange().GetRequiredBytes())
	}
	sourcePath := adoption.CanonicalSource
	if adoption.Kind == v1alpha1.FilesystemAdoptionKindZFSDataset {
		sourcePath = adoption.HostPath
	}
	if sourcePath != env.agent.importVolumeResp.GetDevicePath() {
		t.Errorf("durable filesystem source path = %q, want imported path %q",
			sourcePath, env.agent.importVolumeResp.GetDevicePath())
	}

	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady {
		t.Fatalf("adopted PillarVolumeState phase = %q, want Ready", pvs.Status.Phase)
	}
	if pvs.Spec.AgentVolumeID == "" || pvs.Spec.BackendType == "" || pvs.Spec.ProtocolType == "" {
		t.Fatalf("adopted PillarVolumeState lacks persisted ownership routing: %+v", pvs.Spec)
	}
	wantAgentID := pvs.Spec.Resolved.Backend.PoolName() + "/" + requireFilesystemImportLeaf(t, adoption)
	if pvs.Spec.AgentVolumeID != wantAgentID {
		t.Errorf("agent volume ID = %q, want native identity ID %q", pvs.Spec.AgentVolumeID, wantAgentID)
	}
	if resp.GetVolume().GetVolumeId() != pvs.Spec.VolumeID || !isFilesystemVolumeID(pvs) {
		t.Errorf("CSI volume ID = %q, want persisted opaque filesystem handle %q",
			resp.GetVolume().GetVolumeId(), pvs.Spec.VolumeID)
	}
}

func requireFilesystemAdoptionInspection(
	t *testing.T, env *controllerTestEnv, req *csipb.CreateVolumeRequest, selector, source string,
) {
	t.Helper()
	inspectReq := env.agent.lastInspectImportReq
	if inspectReq == nil || inspectReq.GetRequiredBytes() != req.GetCapacityRange().GetRequiredBytes() ||
		inspectReq.GetSource() != source {
		t.Fatalf("InspectImport request = %+v, want exact source and requested capacity", inspectReq)
	}
	if got := env.agent.lastImportVolumeReq.GetFilesystemAdoption().GetCanonicalSource(); got != source {
		t.Errorf("ImportVolume canonical source = %q, want %q", got, source)
	}
	switch selector {
	case filesystemImportDirectoryKey:
		if inspectReq.GetExpectedHostRoot() != "/var/lib/pillar-csi/datasets" {
			t.Errorf("InspectImport expected host root = %q, want configured directory root",
				inspectReq.GetExpectedHostRoot())
		}
		directory := env.agent.lastImportVolumeReq.GetBackendParams().GetDirectory()
		if directory == nil || directory.GetLogicalPool() != "files" ||
			directory.GetHostRoot() != "/var/lib/pillar-csi/datasets" {
			t.Errorf("directory backend params = %+v, want files and configured host root", directory)
		}
	case filesystemImportZFSDatasetKey:
		if inspectReq.GetExpectedParentDataset() != "" {
			t.Errorf("InspectImport expected parent dataset = %q, want empty configured parent",
				inspectReq.GetExpectedParentDataset())
		}
		zfs := env.agent.lastImportVolumeReq.GetBackendParams().GetZfs()
		if zfs == nil || zfs.GetPool() != "tank" {
			t.Errorf("zfs backend params = %+v, want pool tank", zfs)
		}
	}
}

func TestCreateVolume_FilesystemAdoption_RejectsCapacityLimitBelowExactQuota(t *testing.T) {
	t.Parallel()

	env, req := newFilesystemImportCreateEnv(t, filesystemImportDirectoryKey,
		"/var/lib/pillar-csi/datasets/app-data")
	req.GetCapacityRange().LimitBytes = req.GetCapacityRange().RequiredBytes - 1

	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume capacity-limit code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
	if env.agent.createVolumeCalls != 0 || env.agent.importVolumeCalls != 0 {
		t.Fatalf("backend lifecycle calls = create %d/import %d, want none before capacity-limit refusal",
			env.agent.createVolumeCalls, env.agent.importVolumeCalls)
	}
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(), ctrlKey(req.GetName()), pvs); !kserrors.IsNotFound(err) {
		t.Fatalf("PillarVolumeState after capacity-limit refusal: err=%v, object=%+v; want no lifecycle", err, pvs)
	}
}

func TestDecodePVCAnnotations_FilesystemAdoptionSelectors_ExclusiveWithImportZvol(t *testing.T) {
	t.Parallel()

	for name, selectors := range map[string][]string{
		"directory and zvol":        {filesystemImportDirectoryKey, v1alpha1.AnnotationImportZvol},
		"zfs dataset and zvol":      {filesystemImportZFSDatasetKey, v1alpha1.AnnotationImportZvol},
		"directory and zfs dataset": {filesystemImportDirectoryKey, filesystemImportZFSDatasetKey},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			annotations := map[string]string{}
			for _, key := range selectors {
				switch key {
				case filesystemImportDirectoryKey:
					annotations[key] = "/var/lib/pillar-csi/datasets/app-data"
				case filesystemImportZFSDatasetKey, v1alpha1.AnnotationImportZvol:
					annotations[key] = "tank/app-data"
				}
			}
			_, err := decodePVCAnnotations(annotations)
			if err == nil {
				t.Fatalf("decodePVCAnnotations(%v): mutually exclusive adoption selectors were accepted", selectors)
			}
		})
	}
}

func TestDecodePVCAnnotations_NormalBlockAnnotationsRemainHealthy(t *testing.T) {
	t.Parallel()

	docs, err := decodePVCAnnotations(map[string]string{
		v1alpha1.AnnotationBackendDoc:  "zfs:\n  properties:\n    compression: zstd\n",
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  maxQueueSize: 64\n",
	})
	if err != nil {
		t.Fatalf("decodePVCAnnotations(normal block annotations): %v", err)
	}
	if docs.Backend == nil || docs.Backend.ZFS == nil || docs.Backend.ZFS.Properties["compression"] != "zstd" {
		t.Fatalf("Backend = %+v, want the existing zfs block override", docs.Backend)
	}
	if docs.Protocol == nil || docs.Protocol.NVMeOFTCP == nil ||
		docs.Protocol.NVMeOFTCP.MaxQueueSize == nil || *docs.Protocol.NVMeOFTCP.MaxQueueSize != 64 {
		t.Fatalf("Protocol = %+v, want the existing nvmeofTcp block override", docs.Protocol)
	}
	if docs.ImportZvol != "" {
		t.Fatalf("ImportZvol = %q, want no adoption selector", docs.ImportZvol)
	}
}

func TestCreateVolume_NormalBlockAnnotationsRemainHealthy(t *testing.T) {
	t.Parallel()

	env, req := newControllerTestEnvWithPVC(t, "default", "normal-block", map[string]string{
		v1alpha1.AnnotationBackendDoc:  "zfs:\n  properties:\n    compression: zstd\n",
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  maxQueueSize: 64\n",
	})
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume(normal block annotations): %v", err)
	}
	if resp == nil || resp.GetVolume() == nil || resp.GetVolume().GetVolumeId() == "" {
		t.Fatalf("CreateVolume(normal block annotations) returned no usable volume: %+v", resp)
	}
	if got, want := resp.GetVolume().GetCapacityBytes(), req.GetCapacityRange().GetRequiredBytes(); got < want {
		t.Fatalf("normal block capacity = %d, want at least %d", got, want)
	}
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(), ctrlKey(req.GetName()), pvs); err != nil {
		t.Fatalf("read normal block PillarVolumeState: %v", err)
	}
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady {
		t.Fatalf("normal block PillarVolumeState phase = %q, want Ready", pvs.Status.Phase)
	}
}
