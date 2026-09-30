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

// Import (issue #146) controller tests: the pillar-csi.bhyoo.com/import-zvol
// PVC annotation drives the adopt-not-create path.  All checks that do not
// need the storage node run here, on the controller; the agent-side checks
// are covered by internal/agent tests and the E36 e2e case.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/testutil/fakeuid"
)

// importStoreName is the PillarStore these tests seed: pool "hot-data" with
// parent dataset "k8s", the democratic-csi/openebs layout from the issue.
const importStoreName = "import-store"

// newImportTestEnv builds a controller test env whose PillarStore selects
// pool "hot-data" parent "k8s" and whose PVC carries the import annotation.
func newImportTestEnv(
	t *testing.T,
	pvcName, importDataset string,
	extra ...ctrlclient.Object,
) (*controllerTestEnv, *csi.CreateVolumeRequest) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}

	store := &v1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: importStoreName},
		Spec: v1alpha1.PillarStoreSpec{
			AgentRef: "storage-node-1",
			Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
				VolumeType:    v1alpha1.ZFSVolumeTypeZvol,
				Pool:          "hot-data",
				ParentDataset: "k8s",
			}},
		},
	}
	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarAgentSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarAgentStatus{ResolvedAddress: "192.168.1.10:9500"},
	}
	annotations := map[string]string{}
	if importDataset != "" {
		annotations[v1alpha1.AnnotationImportZvol] = importDataset
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   "default",
			Annotations: annotations,
		},
	}

	objs := append(testConfigObjects(), store, target, pvc)
	objs = append(objs, extra...)
	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.PillarVolumeState{}, &v1alpha1.PillarAgent{}).
		Build()

	agent := &mockAgentClient{}
	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com",
		func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
			return agent, nopCloser{}, nil
		})

	req := baseCreateVolumeRequest()
	req.Name = "pvc-" + pvcName
	req.Parameters[paramStoreRef] = importStoreName
	req.Parameters[paramPVCNameMeta] = pvcName
	req.Parameters[paramPVCNamespaceMeta] = "default"
	return &controllerTestEnv{srv: srv, agent: agent, scheme: scheme}, req
}

// seedImportedPVS records a PillarVolumeState that already adopted
// "hot-data/k8s/<leaf>" — the record a first import attempt leaves behind.
func seedImportedPVS(
	t *testing.T,
	env *controllerTestEnv,
	name, leaf, phase string,
) {
	t.Helper()
	pvs := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/" + leaf,
			AgentVolumeID: "hot-data/" + leaf,
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
			ImportedFrom:  "hot-data/k8s/" + leaf,
			Resolved: &v1alpha1.ResolvedVolumeConfig{
				Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
					VolumeType:    v1alpha1.ZFSVolumeTypeZvol,
					Pool:          "hot-data",
					ParentDataset: "k8s",
				}},
				Protocol: v1alpha1.ProtocolSpec{NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{Port: 4420}},
			},
		},
		Status: v1alpha1.PillarVolumeStateStatus{
			Phase:             v1alpha1.PillarVolumeStatePhase(phase),
			BackendDevicePath: "/dev/zvol/hot-data/k8s/" + leaf,
		},
	}
	if err := env.srv.k8sClient.Create(context.Background(), pvs); err != nil {
		t.Fatalf("seed PillarVolumeState %q: %v", name, err)
	}
	pvs.Status.Phase = v1alpha1.PillarVolumeStatePhase(phase)
	if err := env.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
		t.Fatalf("seed PVS status: %v", err)
	}
}

func TestCreateVolume_ImportZvol_Success(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if env.agent.importVolumeCalls != 1 {
		t.Fatalf("ImportVolume calls = %d, want 1", env.agent.importVolumeCalls)
	}
	if env.agent.createVolumeCalls != 0 {
		t.Fatalf("CreateVolume called %d times; import must never create", env.agent.createVolumeCalls)
	}
	got := env.agent.lastImportVolumeReq
	if got.GetVolumeId() != "hot-data/legacy-vol" {
		t.Errorf("agent volume ID = %q, want hot-data/legacy-vol", got.GetVolumeId())
	}
	wantVolID := "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol"
	if resp.GetVolume().GetVolumeId() != wantVolID {
		t.Errorf("CSI VolumeId = %q, want %q", resp.GetVolume().GetVolumeId(), wantVolID)
	}
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: req.GetName()}, pvs); err != nil {
		t.Fatalf("get PillarVolumeState: %v", err)
	}
	if pvs.Spec.ImportedFrom != "hot-data/k8s/legacy-vol" {
		t.Errorf("spec.importedFrom = %q", pvs.Spec.ImportedFrom)
	}
	if pvs.Spec.AgentVolumeID != "hot-data/legacy-vol" {
		t.Errorf("spec.agentVolumeID = %q", pvs.Spec.AgentVolumeID)
	}
}

func TestCreateVolume_ImportZvol_Refusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		annotation string
		code       codes.Code
		wantErr    string
	}{
		"malformed": {
			annotation: "not-a-dataset",
			code:       codes.InvalidArgument,
		},
		"wrong pool": {
			annotation: "nas/k8s/legacy-vol",
			code:       codes.InvalidArgument,
			wantErr:    "pool",
		},
		"outside parent dataset": {
			annotation: "hot-data/other/legacy-vol",
			code:       codes.InvalidArgument,
			wantErr:    "parent dataset",
		},
		"snapshot not a zvol name": {
			annotation: "hot-data/k8s/legacy-vol@snap",
			code:       codes.InvalidArgument,
		},
		"empty annotation": {
			annotation: "  ",
			code:       codes.InvalidArgument,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, req := newImportTestEnv(t, "data-"+name, tc.annotation)
			_, err := env.srv.CreateVolume(context.Background(), req)
			if err == nil {
				t.Fatalf("CreateVolume with %q succeeded; want refusal", tc.annotation)
			}
			if st, _ := status.FromError(err); st.Code() != tc.code {
				t.Fatalf("code = %v, want %v (err: %v)", st.Code(), tc.code, err)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should mention %q", err, tc.wantErr)
			}
			if env.agent.importVolumeCalls != 0 || env.agent.createVolumeCalls != 0 {
				t.Errorf("agent RPCs on a refused claim: import=%d create=%d",
					env.agent.importVolumeCalls, env.agent.createVolumeCalls)
			}
		})
	}
}

func TestCreateVolume_ImportZvol_NonZFSStore(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "data-vg/lv-0")
	req.Parameters[paramStoreRef] = testLVMStoreName

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestCreateVolume_ImportZvol_DuplicateAgentVolumeID(t *testing.T) {
	t.Parallel()
	// Another PillarVolumeState already owns hot-data/legacy-vol — a second
	// lifecycle must never manage the same zvol.
	owner := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-other"},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol",
			AgentVolumeID: "hot-data/legacy-vol",
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
			ImportedFrom:  "hot-data/k8s/legacy-vol",
		},
	}
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol", owner)

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if env.agent.importVolumeCalls != 0 || env.agent.createVolumeCalls != 0 {
		t.Fatal("agent RPCs ran on a duplicate import")
	}
}

func TestCreateVolume_ImportZvol_AgentRefusalPropagates(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
	env.agent.importVolumeErr = status.Error(codes.FailedPrecondition, "in use: mounted at /data")

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

// A retry whose claim was re-applied without the annotation keeps importing:
// the recorded spec is authoritative.
func TestCreateVolume_ImportZvol_RetryWithoutAnnotation(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "")
	seedImportedPVS(t, env, req.GetName(), "legacy-vol", "")

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume retry: %v", err)
	}
	if env.agent.importVolumeCalls != 1 {
		t.Fatalf("ImportVolume calls = %d, want 1", env.agent.importVolumeCalls)
	}
	if resp.GetVolume().GetVolumeId() != "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol" {
		t.Errorf("VolumeId = %q", resp.GetVolume().GetVolumeId())
	}
}

// A lifecycle that was never an import cannot become one by adding the
// annotation later; the claim must be re-created.
func TestCreateVolume_ImportZvol_RefusesLateAnnotation(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
	// Pre-seed a non-import lifecycle for the same volume name.
	pvs := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: req.GetName()},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/" + req.GetName(),
			AgentVolumeID: "hot-data/" + req.GetName(),
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
			Resolved: &v1alpha1.ResolvedVolumeConfig{
				Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
					VolumeType:    v1alpha1.ZFSVolumeTypeZvol,
					Pool:          "hot-data",
					ParentDataset: "k8s",
				}},
				Protocol: v1alpha1.ProtocolSpec{NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{Port: 4420}},
			},
		},
	}
	if err := env.srv.k8sClient.Create(context.Background(), pvs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if env.agent.importVolumeCalls != 0 {
		t.Fatal("import RPC ran for a lifecycle that was never an import")
	}
}

func TestCreateVolume_ImportZvol_RefusesChangedSource(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/other-vol")
	seedImportedPVS(t, env, req.GetName(), "legacy-vol", "")

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if env.agent.importVolumeCalls != 0 {
		t.Fatal("import RPC ran with a changed import source")
	}
}

// A normal create must refuse when the zvol its agent volume ID names is
// already owned by an imported lifecycle.
func TestCreateVolume_NormalCreate_RefusesImportedCollision(t *testing.T) {
	t.Parallel()
	owner := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-imported"},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/pvc-data",
			AgentVolumeID: "hot-data/pvc-data",
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
			ImportedFrom:  "hot-data/k8s/pvc-data",
		},
	}
	env, req := newImportTestEnv(t, "data", "", owner)
	// No annotation: a normal create for agentVolID hot-data/pvc-data.
	req.Parameters[paramStoreRef] = importStoreName

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if env.agent.createVolumeCalls != 0 {
		t.Fatal("CreateVolume ran despite the imported-zvol collision")
	}
}

// The CSI volume ID of an imported volume resolves its PillarVolumeState via
// spec.agentVolumeID, so DeleteVolume works end to end.
func TestDeleteVolume_ImportedVolume(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := resp.GetVolume().GetVolumeId()

	if _, err := env.srv.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if env.agent.deleteVolumeCalls != 1 {
		t.Fatalf("DeleteVolume agent calls = %d, want 1", env.agent.deleteVolumeCalls)
	}
}

// resolveImportDataset accepts exactly "<store pool>/<store parentDataset>/<leaf>":
// a zvol directly under the pool when the store has no parentDataset, and
// never a sibling, nested, or non-canonical path that merely shares a prefix.
func TestResolveImportDataset_Layout(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		parent     string
		annotation string
		wantLeaf   string // "" → InvalidArgument expected
	}{
		"pool root zvol":            {parent: "", annotation: "hot-data/legacy-vol", wantLeaf: "legacy-vol"},
		"under parent":              {parent: "k8s", annotation: "hot-data/k8s/legacy-vol", wantLeaf: "legacy-vol"},
		"nested parent":             {parent: "k8s/csi", annotation: "hot-data/k8s/csi/pvc-1", wantLeaf: "pvc-1"},
		"store parent trailing /":   {parent: "k8s/", annotation: "hot-data/k8s/legacy-vol", wantLeaf: "legacy-vol"},
		"leaf with . : _ -":         {parent: "k8s", annotation: "hot-data/k8s/a.b:c_d-e", wantLeaf: "a.b:c_d-e"},
		"pool root vs parent store": {parent: "k8s", annotation: "hot-data/legacy-vol"},
		"parent vs pool root store": {parent: "", annotation: "hot-data/k8s/legacy-vol"},
		"sibling parent prefix":     {parent: "k8s", annotation: "hot-data/k8s-other/legacy-vol"},
		"deeper than parent":        {parent: "k8s", annotation: "hot-data/k8s/a/b"},
		"shallower than parent":     {parent: "k8s/csi", annotation: "hot-data/k8s/pvc-1"},
		"pool prefix":               {parent: "k8s", annotation: "hot-data-2/k8s/legacy-vol"},
		"double slash in parent":    {parent: "k8s", annotation: "hot-data/k8s//legacy-vol"},
		"double slash after pool":   {parent: "k8s", annotation: "hot-data//k8s/legacy-vol"},
		"double slash pool root":    {parent: "", annotation: "hot-data//legacy-vol"},
		"leading slash":             {parent: "k8s", annotation: "/hot-data/k8s/legacy-vol"},
		"trailing slash":            {parent: "k8s", annotation: "hot-data/k8s/legacy-vol/"},
		"dot component":             {parent: "k8s", annotation: "hot-data/k8s/./legacy-vol"},
		"dotdot leaf":               {parent: "k8s", annotation: "hot-data/k8s/.."},
		"dotdot escape":             {parent: "k8s", annotation: "hot-data/k8s/../k8s/legacy-vol"},
		"bookmark":                  {parent: "k8s", annotation: "hot-data/k8s/legacy-vol#b"},
		"non-ASCII leaf":            {parent: "k8s", annotation: "hot-data/k8s/vøl"},
		"space in leaf":             {parent: "k8s", annotation: "hot-data/k8s/legacy vol"},
		"pool only":                 {parent: "", annotation: "hot-data"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resolved := &v1alpha1.ResolvedVolumeConfig{
				Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
					VolumeType: v1alpha1.ZFSVolumeTypeZvol, Pool: "hot-data", ParentDataset: tc.parent,
				}},
			}
			leaf, err := resolveImportDataset(resolved, tc.annotation)
			if tc.wantLeaf != "" {
				if err != nil || leaf != tc.wantLeaf {
					t.Fatalf("resolveImportDataset(%q) = %q, %v; want %q", tc.annotation, leaf, err, tc.wantLeaf)
				}
				return
			}
			if st, _ := status.FromError(err); err == nil || st.Code() != codes.InvalidArgument {
				t.Fatalf("resolveImportDataset(%q) = %q, %v; want InvalidArgument", tc.annotation, leaf, err)
			}
		})
	}
}

// stalePVS is a PillarVolumeState named like an imported zvol's leaf that
// owns a different backend volume (another pool), e.g. a normal volume
// "legacy-vol" provisioned on store "cold".
func stalePVS(name string) *v1alpha1.PillarVolumeState {
	return &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nvmeof-tcp/zfs-zvol/cold/" + name,
			AgentVolumeID: "cold/" + name,
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
		},
	}
}

// A PillarVolumeState whose name equals the volume ID's leaf but whose
// spec.agentVolumeID differs does not own the volume: the resolver must find
// the real (imported) owner by agentVolumeID instead of the name collision.
func TestVolumeStateNameForID_LeafNamedStateOwnsOtherVolume(t *testing.T) {
	t.Parallel()
	owner := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-data"},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol",
			AgentVolumeID: "hot-data/legacy-vol",
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
			ImportedFrom:  "hot-data/k8s/legacy-vol",
		},
	}
	env, _ := newImportTestEnv(t, "data", "", stalePVS("legacy-vol"), owner)
	got, err := env.srv.volumeStateNameForID(context.Background(),
		"storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol")
	if err != nil || got != "pvc-data" {
		t.Fatalf("volumeStateNameForID = %q, %v; want pvc-data", got, err)
	}
}

// With no owner at all, a leaf-named state that owns a different backend
// volume must not be attributed the volume: DeleteVolume treats the ID as
// unknown and never deletes, fences, or marks the unrelated state.
func TestDeleteVolume_LeafNamedStateOwnsOtherVolume(t *testing.T) {
	t.Parallel()
	env, _ := newImportTestEnv(t, "data", "", stalePVS("legacy-vol"))
	volumeID := "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol"
	got, err := env.srv.volumeStateNameForID(context.Background(), volumeID)
	if err != nil || got != "" {
		t.Fatalf("volumeStateNameForID = %q, %v; want no owner", got, err)
	}
	if _, err := env.srv.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("DeleteVolume agent calls = %d, want 0", env.agent.deleteVolumeCalls)
	}
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "legacy-vol"}, pvs); err != nil {
		t.Fatalf("unrelated PillarVolumeState: %v", err)
	}
	if pvs.Status.Deleting || pvs.DeletionTimestamp != nil {
		t.Fatalf("unrelated PillarVolumeState was marked for deletion: %+v", pvs.Status)
	}
}
