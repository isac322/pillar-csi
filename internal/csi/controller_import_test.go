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
	"maps"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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
	annotations := map[string]string{}
	if importDataset != "" {
		annotations[v1alpha1.AnnotationImportZvol] = importDataset
	}
	return newImportTestEnvWithAnnotations(t, pvcName, annotations, extra...)
}

// newImportTestEnvWithAnnotations is newImportTestEnv with the claim's
// annotations given verbatim.
func newImportTestEnvWithAnnotations(
	t *testing.T,
	pvcName string,
	annotations map[string]string,
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
		Name: importStoreName,
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
		Name: "storage-node-1",
		Spec: v1alpha1.PillarAgentSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarAgentStatus{ResolvedAddress: "192.168.1.10:9500"},
	}
	pvc := &corev1.PersistentVolumeClaim{
		Name:        pvcName,
		Namespace:   "default",
		Annotations: annotations,
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
		Name: name,
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
		Name: "pvc-other",
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
		Name: req.GetName(),
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
		Name: "pvc-imported",
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

// A PillarVolumeState written by an older version records only spec.volumeID
// (no spec.agentVolumeID).  It still owns its volume: DeleteVolume must find
// it, delete the backend volume, and remove the state — not report success
// for an "unknown" volume and leak the zvol.
func TestDeleteVolume_LegacyStateWithoutAgentVolumeID(t *testing.T) {
	t.Parallel()
	const name = "pvc-legacy"
	volumeID := "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/" + name
	legacy := &v1alpha1.PillarVolumeState{
		Name: name,
		Spec: v1alpha1.PillarVolumeStateSpec{VolumeID: volumeID},
	}
	env, _ := newImportTestEnv(t, "data", "")
	if err := env.srv.k8sClient.Create(context.Background(), legacy); err != nil {
		t.Fatalf("seed legacy PillarVolumeState: %v", err)
	}
	got, err := env.srv.volumeStateNameForID(context.Background(), volumeID)
	if err != nil || got != name {
		t.Fatalf("volumeStateNameForID = %q, %v; want %q", got, err, name)
	}
	_, err = env.srv.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: volumeID})
	if err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if env.agent.deleteVolumeCalls != 1 {
		t.Fatalf("DeleteVolume agent calls = %d, want 1", env.agent.deleteVolumeCalls)
	}
	err = env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: name}, &v1alpha1.PillarVolumeState{})
	if err == nil {
		t.Fatal("legacy PillarVolumeState still exists after DeleteVolume")
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
		Name: name,
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
		Name: "pvc-data",
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

// ─────────────────────────────────────────────────────────────────────────────
// Review regressions: reservations, unadopted-import teardown, agent scoping
// ─────────────────────────────────────────────────────────────────────────────

// legacyVolReservation is the reservation name of hot-data/legacy-vol.
var legacyVolReservation = reservationName("storage-node-1", "zfs-zvol", "hot-data/legacy-vol")

// legacyVolReservationOf returns the PillarVolumeReservation of the backend
// volume hot-data/legacy-vol on storage-node-1, or nil when it does not
// exist.
func legacyVolReservationOf(t *testing.T, env *controllerTestEnv) *v1alpha1.PillarVolumeReservation {
	t.Helper()
	res := &v1alpha1.PillarVolumeReservation{}
	err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: legacyVolReservation}, res)
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get reservation: %v", err)
	}
	return res
}

// An import takes the backend-volume reservation before its PillarVolumeState
// exists; a second claim importing the same zvol is refused by the
// reservation even though no state lists the zvol yet (the TOCTOU the
// List-before-create scan cannot close).
func TestCreateVolume_ImportZvol_ReservationBlocksSecondClaim(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")

	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	res := legacyVolReservationOf(t, env)
	if res == nil {
		t.Fatal("no PillarVolumeReservation was created for the import")
	}
	if res.Spec.OwnerVolume != req.GetName() {
		t.Fatalf("reservation owner = %q, want %q", res.Spec.OwnerVolume, req.GetName())
	}
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: req.GetName()}, pvs); err != nil {
		t.Fatalf("get PillarVolumeState: %v", err)
	}
	if !pvs.Status.ImportAcquired {
		t.Error("status.importAcquired must be set once ImportVolume succeeded")
	}

	// A second claim for the same zvol — its own PVC, own volume name — is
	// refused even though the owner's PillarVolumeState scan could pass first.
	env2, req2 := newImportTestEnv(t, "data-2", "hot-data/k8s/legacy-vol")
	env2.srv.k8sClient = env.srv.k8sClient // share the API
	env2.srv.apiReader = env.srv.apiReader
	_, err := env2.srv.CreateVolume(context.Background(), req2)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("second import: err = %v, want FailedPrecondition", err)
	}
	if env2.agent.importVolumeCalls != 0 {
		t.Fatal("a refused second import must not reach the agent")
	}
}

// reservationHookReader wraps the uncached reader and runs before/after
// around its n-th PillarVolumeReservation Get (1-based), so a test can change
// the stored reservation at an exact point of a CreateVolume or release.
type reservationHookReader struct {
	ctrlclient.Reader
	gets          int
	before, after func(n int)
}

func (r *reservationHookReader) Get(
	ctx context.Context, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption,
) error {
	_, isReservation := obj.(*v1alpha1.PillarVolumeReservation)
	if isReservation {
		r.gets++
		if r.before != nil {
			r.before(r.gets)
		}
	}
	err := r.Reader.Get(ctx, key, obj, opts...)
	if isReservation && r.after != nil {
		r.after(r.gets)
	}
	return err
}

// replaceReservation deletes the stored reservation of hot-data/legacy-vol
// and re-creates it for owner, updating it until its resourceVersion differs
// from the replaced one's (the fake client restarts versions per object) —
// what another lifecycle taking the reservation over looks like on the API
// server, where a re-created object never repeats a resourceVersion.
func replaceReservation(t *testing.T, env *controllerTestEnv, owner string) {
	t.Helper()
	ctx := context.Background()
	old := &v1alpha1.PillarVolumeReservation{}
	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: legacyVolReservation}, old); err == nil {
		if err := env.srv.k8sClient.Delete(ctx, old); err != nil {
			t.Fatalf("delete reservation: %v", err)
		}
	}
	res := &v1alpha1.PillarVolumeReservation{
		Name: legacyVolReservation,
		Spec: v1alpha1.PillarVolumeReservationSpec{
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			AgentVolumeID: "hot-data/legacy-vol",
			OwnerVolume:   owner,
			ClaimRef:      &v1alpha1.VolumeClaimRef{UID: "uid-" + owner, Namespace: "default", Name: owner},
		},
	}
	if err := env.srv.k8sClient.Create(ctx, res); err != nil {
		t.Fatalf("create replacement reservation: %v", err)
	}
	for i := 0; res.ResourceVersion == old.ResourceVersion; i++ {
		res.Labels = map[string]string{"touched": strconv.Itoa(i)}
		if err := env.srv.k8sClient.Update(ctx, res); err != nil {
			t.Fatalf("update replacement reservation: %v", err)
		}
	}
}

// A reservation held by another claim is never reclaimed automatically —
// its creator may only be paused and could still reach the agent.  The
// import is refused with FailedPrecondition naming the owner and the exact
// command an operator runs after verifying that claim is gone.  On the
// reclaiming code the first attempt deleted the reservation.
func TestCreateVolume_ImportZvol_ForeignReservationRefused(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
	replaceReservation(t, env, "pvc-ghost")

	for attempt := range 2 {
		_, err := env.srv.CreateVolume(context.Background(), req)
		if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
			t.Fatalf("attempt %d: err = %v, want FailedPrecondition", attempt, err)
		}
		want := "kubectl delete pillarvolumereservation " + legacyVolReservation
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "pvc-ghost") {
			t.Fatalf("attempt %d: refusal %q must name the owner and %q", attempt, err, want)
		}
	}
	res := legacyVolReservationOf(t, env)
	if res == nil || res.Spec.OwnerVolume != "pvc-ghost" {
		t.Fatalf("foreign reservation was reclaimed: %+v", res)
	}
	if env.agent.importVolumeCalls != 0 {
		t.Fatal("a claim without the reservation reached the agent")
	}
}

// The reservation is re-read uncached right before ImportVolume: an attempt
// whose reservation was taken over since it reserved (an operator deleted it
// and another claim reserved the zvol) must not bind the zvol at the agent.
func TestCreateVolume_ImportZvol_ReservationRecheckedBeforeImport(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
	hook := &reservationHookReader{Reader: env.srv.k8sClient}
	hook.before = func(n int) {
		if n == 2 { // after reserveBackendVolume created it
			replaceReservation(t, env, "pvc-other")
		}
	}
	env.srv.apiReader = hook

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("CreateVolume: err = %v, want FailedPrecondition (reservation lost)", err)
	}
	if env.agent.importVolumeCalls != 0 {
		t.Fatal("ImportVolume ran although the reservation belongs to another claim")
	}
}

// Releasing a reservation deletes it with the UID and resourceVersion that
// were read: a reservation replaced between the read and the delete belongs
// to another lifecycle and survives.  The unconditional delete removed it.
func TestReleaseBackendVolume_StaleReadKeepsReplacement(t *testing.T) {
	t.Parallel()
	env, _ := newImportTestEnv(t, "data", "")
	replaceReservation(t, env, "pvc-data")
	hook := &reservationHookReader{Reader: env.srv.k8sClient}
	hook.after = func(n int) {
		if n == 1 {
			replaceReservation(t, env, "pvc-new")
		}
	}
	env.srv.apiReader = hook

	err := env.srv.releaseBackendVolume(context.Background(),
		"pvc-data", "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol", "",
		backendReservation{agent: "storage-node-1", backendType: "zfs-zvol", key: "hot-data/legacy-vol"})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("release over a replaced reservation: err = %v, want Aborted", err)
	}
	res := legacyVolReservationOf(t, env)
	if res == nil || res.Spec.OwnerVolume != "pvc-new" {
		t.Fatalf("stale release deleted the replacement reservation (now %+v)", res)
	}
}

// A refused import was never adopted: when the claim is deleted the
// abandoned lifecycle is ended with ReleaseVolume only.  On the buggy
// ordering the fenced teardown ran UnexportVolume+DeleteVolume and destroyed
// the pre-existing zvol the agent refused to adopt.
func TestReapAbandonedVolume_RefusedImportNeverDestroys(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")

	// The PVC must carry the UID the "pvc-<claimUID>" volume name encodes so
	// the abandoned-attempt detection can attribute the lifecycle.
	seeded := &corev1.PersistentVolumeClaim{}
	if err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "data"}, seeded); err != nil {
		t.Fatalf("get seeded PVC: %v", err)
	}
	if err := env.srv.k8sClient.Delete(context.Background(), seeded); err != nil {
		t.Fatalf("delete seeded PVC: %v", err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		Name: "data", Namespace: "default", UID: types.UID(reapClaimUID),
		Annotations: map[string]string{v1alpha1.AnnotationImportZvol: "hot-data/k8s/legacy-vol"},
	}
	if err := env.srv.k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatalf("recreate PVC with UID: %v", err)
	}
	req.Name = "pvc-" + reapClaimUID

	env.agent.importVolumeErr = status.Error(codes.FailedPrecondition, "in use: mounted at /data")
	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("CreateVolume: err = %v, want FailedPrecondition", err)
	}
	if volumeState(t, env, req.GetName()) == nil {
		t.Fatal("refused import left no PillarVolumeState")
	}
	env.agent.importVolumeErr = nil
	importCalls := env.agent.importVolumeCalls

	deleteClaim(t, env, pvc)
	if !reap(t, env, req.GetName()) {
		t.Fatal("abandoned refused-import attempt was not reaped")
	}
	if env.agent.unexportVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("reap called the agent on a never-adopted import: "+
			"unexport=%d delete=%d — that path runs zfs destroy on the pre-existing zvol",
			env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	if env.agent.releaseVolumeCalls != 1 {
		t.Fatalf("reap called ReleaseVolume %d times, want 1", env.agent.releaseVolumeCalls)
	}
	if env.agent.importVolumeCalls != importCalls {
		t.Fatalf("reap re-ran import: calls %d → %d", importCalls, env.agent.importVolumeCalls)
	}
	if volumeState(t, env, req.GetName()) != nil {
		t.Fatal("PillarVolumeState still exists after the reap")
	}
	if res := legacyVolReservationOf(t, env); res != nil {
		t.Fatalf("reservation %q outlived its lifecycle", res.Name)
	}
}

// The same guarantee through DeleteVolume: an import lifecycle that never
// durably adopted the zvol is ended with ReleaseVolume — never
// UnexportVolume or DeleteVolume — carrying the deletion's fencing token, so
// the agent retires the lifecycle and a delayed ImportVolume cannot land.
// While the agent is unreachable the record and the reservation are kept
// for the retry; afterwards a new claim can import the zvol.
func TestDeleteVolume_UnadoptedImportReleasesWithoutDestroy(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
	env.agent.importVolumeErr = status.Error(codes.FailedPrecondition, "missing: dataset does not exist")

	_, err := env.srv.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.FailedPrecondition {
		t.Fatalf("CreateVolume: err = %v, want FailedPrecondition", err)
	}
	env.agent.importVolumeErr = nil
	pvs := volumeState(t, env, req.GetName())
	if pvs == nil {
		t.Fatal("refused import left no PillarVolumeState")
	}

	volumeID := "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol"
	env.agent.releaseVolumeErr = status.Error(codes.Unavailable, "agent down")
	_, err = env.srv.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("DeleteVolume with the agent down: err = %v, want Unavailable", err)
	}
	if volumeState(t, env, req.GetName()) == nil {
		t.Fatal("the record was forgotten although the agent never released the lifecycle")
	}
	if legacyVolReservationOf(t, env) == nil {
		t.Fatal("the reservation was released although the agent never released the lifecycle")
	}

	env.agent.releaseVolumeErr = nil
	if _, err := env.srv.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if env.agent.unexportVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("delete called destructive agent RPCs on a never-adopted import: "+
			"unexport=%d delete=%d", env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	rel := env.agent.lastReleaseVolumeReq
	if env.agent.releaseVolumeCalls != 2 || rel.GetVolumeId() != "hot-data/legacy-vol" ||
		rel.GetFence().GetVolumeUid() != string(pvs.UID) {
		t.Fatalf("ReleaseVolume calls=%d last=%v, want 2 calls for hot-data/legacy-vol under uid %s",
			env.agent.releaseVolumeCalls, rel, pvs.UID)
	}
	if volumeState(t, env, req.GetName()) != nil {
		t.Fatal("PillarVolumeState still exists after DeleteVolume")
	}
	if res := legacyVolReservationOf(t, env); res != nil {
		t.Fatalf("reservation %q outlived its lifecycle", res.Name)
	}

	requireImportByNewClaim(t, env, req, pvs.UID)
}

// requireImportByNewClaim creates a second claim "data-2" importing the same
// zvol as req and requires its CreateVolume to succeed under a lifecycle
// other than released.
func requireImportByNewClaim(
	t *testing.T, env *controllerTestEnv, req *csi.CreateVolumeRequest, released types.UID,
) {
	t.Helper()
	if err := env.srv.k8sClient.Create(context.Background(), &corev1.PersistentVolumeClaim{
		Name: "data-2", Namespace: "default",
		Annotations: map[string]string{v1alpha1.AnnotationImportZvol: "hot-data/k8s/legacy-vol"},
	}); err != nil {
		t.Fatalf("create second claim: %v", err)
	}
	req2, ok := proto.Clone(req).(*csi.CreateVolumeRequest)
	if !ok {
		t.Fatal("clone CreateVolumeRequest")
	}
	req2.Name = "pvc-data-2"
	req2.Parameters[paramPVCNameMeta] = "data-2"
	if _, err := env.srv.CreateVolume(context.Background(), req2); err != nil {
		t.Fatalf("re-import by a new claim after release: %v", err)
	}
	if env.agent.lastImportVolumeReq.GetFence().GetVolumeUid() == string(released) {
		t.Fatal("re-import reused the released lifecycle's token")
	}
}

// An import lifecycle that never adopted its zvol must not resize it or
// expose it to a node: ControllerExpandVolume and ControllerPublishVolume
// refuse it before any agent call.  The expand used to reach agent
// ExpandVolume, which runs `zfs set volsize` on the pre-existing zvol.
func TestControllerExpandPublish_UnadoptedImportRefused(t *testing.T) {
	t.Parallel()
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
	env.agent.importVolumeErr = status.Error(codes.FailedPrecondition, "in use: mounted at /data")
	if _, err := env.srv.CreateVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateVolume: err = %v, want FailedPrecondition", err)
	}
	volumeID := "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol"

	_, err := env.srv.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ControllerExpandVolume: err = %v, want FailedPrecondition", err)
	}
	if env.agent.expandVolumeCalls != 0 {
		t.Fatal("agent ExpandVolume ran on a zvol whose import was never adopted")
	}

	pub := basePublishRequest()
	pub.VolumeId = volumeID
	_, err = env.srv.ControllerPublishVolume(context.Background(), pub)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "never adopted") {
		t.Fatalf("ControllerPublishVolume: err = %v, want FailedPrecondition (never adopted)", err)
	}
	if env.agent.allowInitiatorCalls != 0 || len(env.agent.setLocalAttachCalls) != 0 {
		t.Fatal("publish granted access to a zvol whose import was never adopted")
	}
}

// Agent volume IDs are only unique per storage node: a same-named backend
// volume owned by a different agent's lifecycle must never be attributed to
// a delete or lookup of this agent's volume ID.
func TestVolumeStateNameForID_IgnoresSameVolumeOnOtherAgent(t *testing.T) {
	t.Parallel()
	remote := &v1alpha1.PillarVolumeState{
		Name: "pvc-remote",
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-2/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol",
			AgentVolumeID: "hot-data/legacy-vol",
			AgentRef:      "storage-node-2",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
		},
	}
	env, _ := newImportTestEnv(t, "data", "", remote)

	volumeID := "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol"
	got, err := env.srv.volumeStateNameForID(context.Background(), volumeID)
	if err != nil || got != "" {
		t.Fatalf("volumeStateNameForID = %q, %v; want no owner on this agent", got, err)
	}
	if _, err := env.srv.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if env.agent.deleteVolumeCalls != 0 || env.agent.unexportVolumeCalls != 0 {
		t.Fatalf("DeleteVolume touched the agent for a volume on another node: "+
			"unexport=%d delete=%d", env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	if got := volumeState(t, env, "pvc-remote"); got == nil || got.Status.Deleting {
		t.Fatal("the other agent's lifecycle was marked deleting")
	}
}

// An agent-scoped conflict: a PillarVolumeState on another agent with the
// same agent volume ID does not block an import here.
func TestCreateVolume_ImportZvol_SameDatasetOtherAgentAllowed(t *testing.T) {
	t.Parallel()
	remote := &v1alpha1.PillarVolumeState{
		Name: "pvc-remote",
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-2/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol",
			AgentVolumeID: "hot-data/legacy-vol",
			AgentRef:      "storage-node-2",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
			ImportedFrom:  "hot-data/k8s/legacy-vol",
		},
	}
	env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol", remote)

	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume must not refuse a same-named zvol on another agent: %v", err)
	}
}

// The ownership scan must read the API server, not the informer cache: a
// state committed moments ago (visible to apiReader but not yet to the
// cached client) still owns its volume.
func TestVolumeStateNameForID_ReadsUncachedReader(t *testing.T) {
	t.Parallel()
	owner := &v1alpha1.PillarVolumeState{
		Name: "pvc-data",
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol",
			AgentVolumeID: "hot-data/legacy-vol",
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
			ImportedFrom:  "hot-data/k8s/legacy-vol",
		},
	}
	env, _ := newImportTestEnv(t, "data", "", stalePVS("legacy-vol"))
	// apiReader sees the owner; the (cache-stand-in) k8sClient does not.
	env.srv.apiReader = fake.NewClientBuilder().
		WithScheme(env.scheme).
		WithObjects(owner).
		Build()

	got, err := env.srv.volumeStateNameForID(context.Background(),
		"storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/legacy-vol")
	if err != nil || got != "pvc-data" {
		t.Fatalf("volumeStateNameForID = %q, %v; want pvc-data from the uncached reader", got, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Issue #163: adopting an existing LVM logical volume (import-lv annotation).
// ─────────────────────────────────────────────────────────────────────────────

const (
	// Well-formed LVM UUIDs (6-4-4-4-4-4-6 alphanumerics) for the adopted
	// LV data-vg/legacy.
	lvVGUUID    = "Ab12Cd-Ef34-Gh56-Ij78-Kl90-Mn12-Op34Qr"
	lvLVUUID    = "Zy98Xw-Vu76-Ts54-Rq32-Po10-Nm98-Lk76Ji"
	lvOtherUUID = "Qq11Ww-Ee22-Rr33-Tt44-Yy55-Uu66-Ii77Oo"

	// The CSI volume ID of the adopted data-vg/legacy on the seeded LVM
	// store (testLVMStoreName, volume group "data-vg").
	lvVolumeID = "storage-node-1/nvmeof-tcp/lvm-lv/data-vg/legacy"

	// The VolumeContext key the node reads to stage a preserving volume
	// without formatting it.
	lvPreserveContextKey = "pillar-csi.bhyoo.com/preserve-original"
)

// importLVValue renders an import-lv annotation value.
func importLVValue(vg, lv, vgUUID, lvUUID string) string {
	return vg + "/" + lv + ":" + vgUUID + ":" + lvUUID
}

// importLVAnnotations returns the claim annotations adopting data-vg/legacy;
// an empty policy leaves the policy annotation absent.
func importLVAnnotations(policy string) map[string]string {
	a := map[string]string{
		v1alpha1.AnnotationImportLV: importLVValue("data-vg", "legacy", lvVGUUID, lvLVUUID),
	}
	if policy != "" {
		a[v1alpha1.AnnotationImportLVPolicy] = policy
	}
	return a
}

// wantLVSource is the spec.lvmSource an adoption of data-vg/legacy records.
func wantLVSource(preserve bool) *v1alpha1.LVMSourceRef {
	return &v1alpha1.LVMSourceRef{
		VolumeGroup:       "data-vg",
		LogicalVolume:     "legacy",
		VolumeGroupUUID:   lvVGUUID,
		LogicalVolumeUUID: lvLVUUID,
		PreserveOriginal:  preserve,
	}
}

// lvClaimName is the claim (namespace default) every import-lv fixture
// adopts data-vg/legacy through.
const lvClaimName = "data"

// newImportLVTestEnv builds an import test env whose claim lvClaimName
// carries annotations and whose request selects the seeded LVM store.
func newImportLVTestEnv(
	t *testing.T,
	annotations map[string]string,
	extra ...ctrlclient.Object,
) (*controllerTestEnv, *csi.CreateVolumeRequest) {
	t.Helper()
	env, req := newImportTestEnvWithAnnotations(t, lvClaimName, annotations, extra...)
	req.Parameters[paramStoreRef] = testLVMStoreName
	return env, req
}

// setClaimAnnotations replaces the annotations of claim default/lvClaimName.
func setClaimAnnotations(t *testing.T, env *controllerTestEnv, annotations map[string]string) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: lvClaimName}, pvc)
	if err != nil {
		t.Fatalf("get PVC %q: %v", lvClaimName, err)
	}
	pvc.Annotations = annotations
	if err := env.srv.k8sClient.Update(context.Background(), pvc); err != nil {
		t.Fatalf("update PVC %q annotations: %v", lvClaimName, err)
	}
}

// lvReservationName is the reservation of data-vg/legacy: keyed by the LV
// UUID, never by the renameable vg/lv locator.
func lvReservationName() string {
	return reservationName("storage-node-1", "lvm-lv", lvLVUUID)
}

// reservationNamed returns the named PillarVolumeReservation, or nil.
func reservationNamed(t *testing.T, env *controllerTestEnv, name string) *v1alpha1.PillarVolumeReservation {
	t.Helper()
	res := &v1alpha1.PillarVolumeReservation{}
	err := env.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, res)
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get reservation %q: %v", name, err)
	}
	return res
}

// requireLVImportRequest checks the last agent.ImportVolume adopted
// data-vg/legacy pinned to its UUIDs with the given policy.
func requireLVImportRequest(t *testing.T, env *controllerTestEnv, preserve bool) {
	t.Helper()
	got := env.agent.lastImportVolumeReq
	if got.GetVolumeId() != "data-vg/legacy" {
		t.Errorf("ImportVolume volume_id = %q, want data-vg/legacy", got.GetVolumeId())
	}
	if got.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM {
		t.Errorf("ImportVolume backend_type = %v, want LVM", got.GetBackendType())
	}
	src := got.GetExpectedLvmSource()
	if src.GetVolumeGroup() != "data-vg" || src.GetLogicalVolume() != "legacy" ||
		src.GetVolumeGroupUuid() != lvVGUUID || src.GetLogicalVolumeUuid() != lvLVUUID {
		t.Errorf("ImportVolume expected_lvm_source = %+v, want data-vg/legacy pinned to %s/%s",
			src, lvVGUUID, lvLVUUID)
	}
	if got.GetPreserveOriginal() != preserve {
		t.Errorf("ImportVolume preserve_original = %v, want %v", got.GetPreserveOriginal(), preserve)
	}
	if got.GetExpectedDataset() != "" {
		t.Errorf("ImportVolume expected_dataset = %q, want empty for an LV", got.GetExpectedDataset())
	}
}

// adoptLV runs a successful first CreateVolume of an import-lv claim.
func adoptLV(t *testing.T, env *controllerTestEnv, req *csi.CreateVolumeRequest) *csi.CreateVolumeResponse {
	t.Helper()
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume (import-lv): %v", err)
	}
	return resp
}

func TestCreateVolume_ImportLV_AdoptsAndPinsPolicy(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(""))

	resp := adoptLV(t, env, req)
	if env.agent.importVolumeCalls != 1 || env.agent.createVolumeCalls != 0 {
		t.Fatalf("agent calls: import=%d create=%d, want import=1 create=0",
			env.agent.importVolumeCalls, env.agent.createVolumeCalls)
	}
	requireLVImportRequest(t, env, true)
	if got := resp.GetVolume().GetVolumeId(); got != lvVolumeID {
		t.Errorf("CSI VolumeId = %q, want %q", got, lvVolumeID)
	}
	if got := resp.GetVolume().GetVolumeContext()[lvPreserveContextKey]; got != "true" {
		t.Errorf("VolumeContext[%s] = %q, want \"true\"", lvPreserveContextKey, got)
	}

	pvs := volumeState(t, env, req.GetName())
	if pvs == nil {
		t.Fatal("no PillarVolumeState after a successful import-lv")
	}
	if !reflect.DeepEqual(pvs.Spec.LVMSource, wantLVSource(true)) {
		t.Errorf("spec.lvmSource = %+v, want %+v", pvs.Spec.LVMSource, wantLVSource(true))
	}
	if pvs.Spec.ImportedFrom != "" {
		t.Errorf("spec.importedFrom = %q, want empty for an LV import", pvs.Spec.ImportedFrom)
	}
	if pvs.Spec.AgentVolumeID != "data-vg/legacy" {
		t.Errorf("spec.agentVolumeID = %q, want data-vg/legacy", pvs.Spec.AgentVolumeID)
	}
	if !pvs.Status.ImportAcquired || pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady {
		t.Errorf("status importAcquired=%v phase=%q, want true/Ready", pvs.Status.ImportAcquired, pvs.Status.Phase)
	}
	res := reservationNamed(t, env, lvReservationName())
	if res == nil || res.Spec.OwnerVolume != req.GetName() {
		t.Errorf("LV-UUID reservation = %+v, want owned by %q", res, req.GetName())
	}
}

func TestCreateVolume_ImportLV_ManagedPolicy(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(v1alpha1.ImportLVPolicyManaged))

	resp := adoptLV(t, env, req)
	if env.agent.importVolumeCalls != 1 || env.agent.createVolumeCalls != 0 {
		t.Fatalf("agent calls: import=%d create=%d, want import=1 create=0",
			env.agent.importVolumeCalls, env.agent.createVolumeCalls)
	}
	requireLVImportRequest(t, env, false)
	if v, ok := resp.GetVolume().GetVolumeContext()[lvPreserveContextKey]; ok {
		t.Errorf("Managed adoption carries VolumeContext[%s]=%q; the node must treat it as a normal volume",
			lvPreserveContextKey, v)
	}
	pvs := volumeState(t, env, req.GetName())
	if pvs == nil || !reflect.DeepEqual(pvs.Spec.LVMSource, wantLVSource(false)) {
		t.Fatalf("spec.lvmSource = %+v, want %+v", pvs, wantLVSource(false))
	}
}

// TestCreateVolume_ImportLV_AnnotationRefusals: a malformed import-lv claim
// is refused before any durable state, reservation or agent call exists.
func TestCreateVolume_ImportLV_AnnotationRefusals(t *testing.T) {
	t.Parallel()
	valid := importLVValue("data-vg", "legacy", lvVGUUID, lvLVUUID)
	symbolUUID := "Zy98X!-Vu76-Ts54-Rq32-Po10-Nm98-Lk76Ji" // well-formed but for one '!'
	// lvAnno and lvParts build the import-lv-only claim annotations of one case.
	lvAnno := func(value string) map[string]string {
		return map[string]string{v1alpha1.AnnotationImportLV: value}
	}
	lvParts := func(vg, lv, vgUUID, lvUUID string) map[string]string {
		return lvAnno(importLVValue(vg, lv, vgUUID, lvUUID))
	}
	for name, tc := range map[string]struct {
		annotations map[string]string
		zfsStore    bool
	}{
		"empty value":         {annotations: lvAnno("  ")},
		"locator only":        {annotations: lvAnno("data-vg/legacy")},
		"missing lv uuid":     {annotations: lvAnno("data-vg/legacy:" + lvVGUUID)},
		"empty vg uuid":       {annotations: lvAnno("data-vg/legacy::" + lvLVUUID)},
		"extra colon":         {annotations: lvAnno(valid + ":" + lvOtherUUID)},
		"no vg in locator":    {annotations: lvAnno("legacy:" + lvVGUUID + ":" + lvLVUUID)},
		"nested lv":           {annotations: lvParts("data-vg", "a/b", lvVGUUID, lvLVUUID)},
		"bad vg uuid":         {annotations: lvParts("data-vg", "legacy", "not-a-uuid", lvLVUUID)},
		"bad lv uuid":         {annotations: lvParts("data-vg", "legacy", lvVGUUID, "Zy98Xw-Vu76")},
		"uuid symbol":         {annotations: lvParts("data-vg", "legacy", lvVGUUID, symbolUUID)},
		"invalid vg name":     {annotations: lvParts("data vg", "legacy", lvVGUUID, lvLVUUID)},
		"invalid lv name":     {annotations: lvParts("data-vg", "bad*lv", lvVGUUID, lvLVUUID)},
		"reserved lv name":    {annotations: lvParts("data-vg", "..", lvVGUUID, lvLVUUID)},
		"vg not the store vg": {annotations: lvParts("other-vg", "legacy", lvVGUUID, lvLVUUID)},
		"unknown policy": {annotations: map[string]string{
			v1alpha1.AnnotationImportLV:       valid,
			v1alpha1.AnnotationImportLVPolicy: "Delete",
		}},
		"policy wrong case": {annotations: map[string]string{
			v1alpha1.AnnotationImportLV:       valid,
			v1alpha1.AnnotationImportLVPolicy: "managed",
		}},
		"with import-zvol on lvm store": {annotations: map[string]string{
			v1alpha1.AnnotationImportLV:   valid,
			v1alpha1.AnnotationImportZvol: "hot-data/k8s/legacy-vol",
		}},
		"with import-zvol on zfs store": {zfsStore: true, annotations: map[string]string{
			v1alpha1.AnnotationImportLV:   valid,
			v1alpha1.AnnotationImportZvol: "hot-data/k8s/legacy-vol",
		}},
		"zfs store": {zfsStore: true, annotations: lvAnno(valid)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, req := newImportLVTestEnv(t, tc.annotations)
			if tc.zfsStore {
				req.Parameters[paramStoreRef] = importStoreName
			}
			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("CreateVolume: err = %v, want InvalidArgument", err)
			}
			if env.agent.importVolumeCalls != 0 || env.agent.createVolumeCalls != 0 ||
				env.agent.exportVolumeCalls != 0 {
				t.Fatalf("agent RPCs on a refused claim: import=%d create=%d export=%d",
					env.agent.importVolumeCalls, env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
			}
			if pvs := volumeState(t, env, req.GetName()); pvs != nil {
				t.Fatalf("refused claim left PillarVolumeState %+v", pvs.Spec)
			}
			if res := reservationNamed(t, env, lvReservationName()); res != nil {
				t.Fatalf("refused claim reserved the LV: %+v", res.Spec)
			}
		})
	}
}

// TestCreateVolume_ImportLV_RecordedSourceWins: once spec.lvmSource is
// recorded, a retry whose annotation names a different LV, UUID or policy
// is refused without reaching the agent, and the record never changes.
func TestCreateVolume_ImportLV_RecordedSourceWins(t *testing.T) {
	t.Parallel()
	for name, annotations := range map[string]map[string]string{
		"other vg": {v1alpha1.AnnotationImportLV: importLVValue("data-vg2", "legacy", lvVGUUID, lvLVUUID)},
		"other lv": {v1alpha1.AnnotationImportLV: importLVValue("data-vg", "legacy2", lvVGUUID, lvLVUUID)},
		"other vg uuid": {
			v1alpha1.AnnotationImportLV: importLVValue("data-vg", "legacy", lvOtherUUID, lvLVUUID),
		},
		"other lv uuid": {
			v1alpha1.AnnotationImportLV: importLVValue("data-vg", "legacy", lvVGUUID, lvOtherUUID),
		},
		"policy downgraded to Managed": importLVAnnotations(v1alpha1.ImportLVPolicyManaged),
		"zvol annotation instead":      {v1alpha1.AnnotationImportZvol: "hot-data/k8s/legacy"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, req := newImportLVTestEnv(t, importLVAnnotations(""))
			adoptLV(t, env, req)
			before := volumeState(t, env, req.GetName())

			setClaimAnnotations(t, env, annotations)
			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("retry with changed source: err = %v, want InvalidArgument", err)
			}
			if env.agent.importVolumeCalls != 1 || env.agent.createVolumeCalls != 0 {
				t.Fatalf("retry reached the agent: import=%d create=%d",
					env.agent.importVolumeCalls, env.agent.createVolumeCalls)
			}
			after := volumeState(t, env, req.GetName())
			if after == nil || !reflect.DeepEqual(after.Spec, before.Spec) {
				t.Fatalf("PillarVolumeState spec changed by a refused retry:\nbefore %+v\nafter  %+v",
					before.Spec, after)
			}
		})
	}
}

// TestCreateVolume_ImportLV_RecordedPolicyManagedRetry: a Managed record is
// not silently upgraded either — a retry whose policy annotation is absent
// (meaning PreserveOriginal) differs from the record.
func TestCreateVolume_ImportLV_RecordedPolicyManagedRetry(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(v1alpha1.ImportLVPolicyManaged))
	adoptLV(t, env, req)

	setClaimAnnotations(t, env, importLVAnnotations(""))
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("retry with policy changed to PreserveOriginal: err = %v, want InvalidArgument", err)
	}
	if env.agent.importVolumeCalls != 1 {
		t.Fatalf("ImportVolume calls = %d, want 1", env.agent.importVolumeCalls)
	}
	if pvs := volumeState(t, env, req.GetName()); !reflect.DeepEqual(pvs.Spec.LVMSource, wantLVSource(false)) {
		t.Fatalf("spec.lvmSource = %+v, want unchanged %+v", pvs.Spec.LVMSource, wantLVSource(false))
	}
}

// TestCreateVolume_ImportLV_IdempotentRetry: retrying with the identical
// annotation, or after the annotation was removed, keeps the recorded
// adoption and returns the same volume.
func TestCreateVolume_ImportLV_IdempotentRetry(t *testing.T) {
	t.Parallel()
	for name, annotations := range map[string]map[string]string{
		"identical annotation":      importLVAnnotations(""),
		"identical explicit policy": importLVAnnotations(v1alpha1.ImportLVPolicyPreserveOriginal),
		"annotation removed":        {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, req := newImportLVTestEnv(t, importLVAnnotations(""))
			first := adoptLV(t, env, req)

			setClaimAnnotations(t, env, annotations)
			second, err := env.srv.CreateVolume(context.Background(), req)
			if err != nil {
				t.Fatalf("idempotent retry: %v", err)
			}
			if second.GetVolume().GetVolumeId() != first.GetVolume().GetVolumeId() {
				t.Fatalf("retry VolumeId = %q, want %q",
					second.GetVolume().GetVolumeId(), first.GetVolume().GetVolumeId())
			}
			if got := second.GetVolume().GetVolumeContext()[lvPreserveContextKey]; got != "true" {
				t.Errorf("retry VolumeContext[%s] = %q, want \"true\"", lvPreserveContextKey, got)
			}
			if env.agent.createVolumeCalls != 0 {
				t.Fatalf("retry created a fresh volume (CreateVolume calls = %d)", env.agent.createVolumeCalls)
			}
			// Any re-issued import still pins the recorded source.
			requireLVImportRequest(t, env, true)
			if pvs := volumeState(t, env, req.GetName()); !reflect.DeepEqual(pvs.Spec.LVMSource, wantLVSource(true)) {
				t.Fatalf("spec.lvmSource = %+v, want %+v", pvs.Spec.LVMSource, wantLVSource(true))
			}
		})
	}
}

// TestCreateVolume_ImportLV_RefusesLateAnnotation: a lifecycle provisioned
// empty never becomes an adoption mid-lifecycle.
func TestCreateVolume_ImportLV_RefusesLateAnnotation(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, map[string]string{})
	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("normal LVM CreateVolume: %v", err)
	}
	before := volumeState(t, env, req.GetName())

	setClaimAnnotations(t, env, importLVAnnotations(""))
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("late import-lv annotation: err = %v, want InvalidArgument", err)
	}
	if env.agent.importVolumeCalls != 0 {
		t.Fatal("ImportVolume ran for a lifecycle that was never an import")
	}
	after := volumeState(t, env, req.GetName())
	if after.Spec.LVMSource != nil || !reflect.DeepEqual(after.Spec, before.Spec) {
		t.Fatalf("late annotation changed the record: %+v", after.Spec)
	}
}

// TestCreateVolume_ReadyRetry_RefusesLateFilesystemSelector: a Ready
// lifecycle without a recorded filesystem adoption (plain LVM create, LV
// adoption, zvol adoption) answered from its durable record refuses a late
// import-directory or import-zfs-dataset selector, and a mixed selector set,
// exactly like the slow path.  The PillarProtocol is deleted before the
// retry so only the Ready fast path can answer: the healthy control proves
// the retry is served from the record, and the slow path would fail with
// FailedPrecondition instead of the asserted InvalidArgument.
func TestCreateVolume_ReadyRetry_RefusesLateFilesystemSelector(t *testing.T) {
	t.Parallel()
	for lifecycleName, lifecycle := range readyRetryLifecycles() {
		t.Run(lifecycleName+"/healthy replay", func(t *testing.T) {
			t.Parallel()
			requireReadyReplay(t, lifecycle)
		})
		for name, annotations := range lateFilesystemSelectorCases(lifecycle) {
			t.Run(lifecycleName+"/"+name, func(t *testing.T) {
				t.Parallel()
				requireReadyRetryRefused(t, lifecycle, annotations)
			})
		}
	}
}

// readyRetryLifecycle is a non-filesystem lifecycle fixture: setup builds
// its env, claim is the annotation set of a healthy replay, and source the
// recorded import selector (nil for a plain create).
type readyRetryLifecycle struct {
	setup  func(t *testing.T) (*controllerTestEnv, *csi.CreateVolumeRequest)
	claim  map[string]string
	source map[string]string
}

func readyRetryLifecycles() map[string]readyRetryLifecycle {
	zvol := map[string]string{v1alpha1.AnnotationImportZvol: "hot-data/k8s/legacy-vol"}
	return map[string]readyRetryLifecycle{
		"plain lvm": {
			setup: func(t *testing.T) (*controllerTestEnv, *csi.CreateVolumeRequest) {
				t.Helper()
				return newImportLVTestEnv(t, map[string]string{})
			},
			claim: map[string]string{},
		},
		"lv adoption": {
			setup: func(t *testing.T) (*controllerTestEnv, *csi.CreateVolumeRequest) {
				t.Helper()
				return newImportLVTestEnv(t, importLVAnnotations(""))
			},
			claim:  importLVAnnotations(""),
			source: importLVAnnotations(""),
		},
		"zvol adoption": {
			setup: func(t *testing.T) (*controllerTestEnv, *csi.CreateVolumeRequest) {
				t.Helper()
				return newImportTestEnv(t, lvClaimName, "hot-data/k8s/legacy-vol")
			},
			claim:  zvol,
			source: zvol,
		},
	}
}

// lateFilesystemSelectorCases returns the late filesystem selectors alone
// and, for an adoption, mixed with its recorded source selector.
func lateFilesystemSelectorCases(lifecycle readyRetryLifecycle) map[string]map[string]string {
	cases := map[string]map[string]string{}
	for name, selector := range map[string]map[string]string{
		"late directory":   {v1alpha1.AnnotationImportDirectory: "/srv/legacy"},
		"late zfs dataset": {v1alpha1.AnnotationImportZFSDataset: "hot-data/files/legacy"},
	} {
		cases[name] = selector
		if lifecycle.source != nil {
			mixed := maps.Clone(lifecycle.source)
			maps.Copy(mixed, selector)
			cases["mixed "+name] = mixed
		}
	}
	return cases
}

// requireReadyReplay checks that a healthy retry is answered from the Ready
// record with the original volume.
func requireReadyReplay(t *testing.T, lifecycle readyRetryLifecycle) {
	t.Helper()
	env, req := lifecycle.setup(t)
	first := readyLifecycleWithoutProtocol(t, env, req)
	setClaimAnnotations(t, env, lifecycle.claim)
	second, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("Ready replay: %v", err)
	}
	if second.GetVolume().GetVolumeId() != first.GetVolume().GetVolumeId() {
		t.Fatalf("replay VolumeId = %q, want %q",
			second.GetVolume().GetVolumeId(), first.GetVolume().GetVolumeId())
	}
}

// requireReadyRetryRefused checks that a Ready retry carrying annotations
// is refused with InvalidArgument without reaching the agent or changing the
// record.
func requireReadyRetryRefused(t *testing.T, lifecycle readyRetryLifecycle, annotations map[string]string) {
	t.Helper()
	env, req := lifecycle.setup(t)
	readyLifecycleWithoutProtocol(t, env, req)
	before := volumeState(t, env, req.GetName())
	imports, creates, exports := env.agent.importVolumeCalls, env.agent.createVolumeCalls,
		env.agent.exportVolumeCalls
	setClaimAnnotations(t, env, annotations)
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Ready retry with %v: err = %v, want InvalidArgument", annotations, err)
	}
	if env.agent.importVolumeCalls != imports || env.agent.createVolumeCalls != creates ||
		env.agent.exportVolumeCalls != exports {
		t.Fatal("refused Ready retry reached the agent")
	}
	after := volumeState(t, env, req.GetName())
	if !reflect.DeepEqual(after.Spec, before.Spec) || after.Spec.FilesystemAdoption != nil {
		t.Fatalf("refused Ready retry changed the record: %+v", after.Spec)
	}
}

// readyLifecycleWithoutProtocol provisions req to Ready and deletes the
// PillarProtocol it resolved, so a later retry can only be answered by the
// Ready fast path.
func readyLifecycleWithoutProtocol(
	t *testing.T, env *controllerTestEnv, req *csi.CreateVolumeRequest,
) *csi.CreateVolumeResponse {
	t.Helper()
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	protocol := &v1alpha1.PillarProtocol{}
	err = env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: req.GetParameters()[paramProtocolRef]}, protocol)
	if err != nil {
		t.Fatalf("get PillarProtocol: %v", err)
	}
	if err := env.srv.k8sClient.Delete(context.Background(), protocol); err != nil {
		t.Fatalf("delete PillarProtocol: %v", err)
	}
	return resp
}

// TestCreateVolume_ImportLV_AgentRefusalPropagates: an agent refusal (LV in
// use, identity mismatch, and the like) fails the attempt without adopting.
func TestCreateVolume_ImportLV_AgentRefusalPropagates(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(""))
	env.agent.importVolumeErr = status.Error(codes.FailedPrecondition, "identity: lv_uuid differs")
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
		t.Fatalf("refused adoption continued: create=%d export=%d",
			env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
	}
	pvs := volumeState(t, env, req.GetName())
	if pvs == nil || pvs.Status.ImportAcquired {
		t.Fatalf("refused adoption state = %+v, want recorded but not importAcquired", pvs)
	}
	if !reflect.DeepEqual(pvs.Spec.LVMSource, wantLVSource(true)) {
		t.Fatalf("spec.lvmSource = %+v, want pinned %+v", pvs.Spec.LVMSource, wantLVSource(true))
	}
}

// TestCreateVolume_ImportZvol_NoLVMSourcePinned: zvol imports and normal
// creates keep their existing request and record shapes.
func TestCreateVolume_ImportZvol_NoLVMSourcePinned(t *testing.T) {
	t.Parallel()
	t.Run("zvol import", func(t *testing.T) {
		t.Parallel()
		env, req := newImportTestEnv(t, "data", "hot-data/k8s/legacy-vol")
		resp, err := env.srv.CreateVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateVolume: %v", err)
		}
		got := env.agent.lastImportVolumeReq
		if got.GetExpectedLvmSource() != nil || got.GetPreserveOriginal() {
			t.Errorf("zvol ImportVolume carries LVM pin: %+v", got)
		}
		if got.GetExpectedDataset() != "hot-data/k8s/legacy-vol" {
			t.Errorf("expected_dataset = %q, want hot-data/k8s/legacy-vol", got.GetExpectedDataset())
		}
		if _, ok := resp.GetVolume().GetVolumeContext()[lvPreserveContextKey]; ok {
			t.Errorf("zvol import VolumeContext carries %s", lvPreserveContextKey)
		}
		if pvs := volumeState(t, env, req.GetName()); pvs.Spec.LVMSource != nil {
			t.Errorf("zvol PVS spec.lvmSource = %+v, want nil", pvs.Spec.LVMSource)
		}
	})
	t.Run("normal lvm create", func(t *testing.T) {
		t.Parallel()
		env, req := newImportLVTestEnv(t, map[string]string{})
		resp, err := env.srv.CreateVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateVolume: %v", err)
		}
		if env.agent.createVolumeCalls != 1 || env.agent.importVolumeCalls != 0 {
			t.Fatalf("agent calls: create=%d import=%d, want 1/0",
				env.agent.createVolumeCalls, env.agent.importVolumeCalls)
		}
		if _, ok := resp.GetVolume().GetVolumeContext()[lvPreserveContextKey]; ok {
			t.Errorf("managed LV VolumeContext carries %s", lvPreserveContextKey)
		}
		if pvs := volumeState(t, env, req.GetName()); pvs.Spec.LVMSource != nil {
			t.Errorf("managed PVS spec.lvmSource = %+v, want nil", pvs.Spec.LVMSource)
		}
	})
}

// ── Delete / expand of an adopted LV ─────────────────────────────────────────.

// TestDeleteVolume_PreservedLV_ReleasesOnly: a preserving adoption ends with
// ReleaseVolume under the deletion fence; the LV is never unexported-and-
// removed by the destructive path, and the record survives a refusal.
func TestDeleteVolume_PreservedLV_ReleasesOnly(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(""))
	resp := adoptLV(t, env, req)
	volumeID := resp.GetVolume().GetVolumeId()
	uid := volumeState(t, env, req.GetName()).UID

	// A consumer still holds the LV: the agent refuses the release.
	env.agent.releaseVolumeErr = status.Error(codes.FailedPrecondition, "in use: mounted at /mnt/legacy")
	_, err := env.srv.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteVolume with a consumer present: err = %v, want FailedPrecondition", err)
	}
	if env.agent.unexportVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("preserving delete ran destructive RPCs: unexport=%d delete=%d",
			env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	kept := volumeState(t, env, req.GetName())
	if kept == nil || !kept.Status.Deleting {
		t.Fatalf("refused release: PillarVolumeState = %+v, want kept with deleting=true", kept)
	}
	if res := reservationNamed(t, env, lvReservationName()); res == nil {
		t.Fatal("refused release dropped the LV reservation")
	}

	// The consumer is gone: the retry releases and retires the lifecycle.
	env.agent.releaseVolumeErr = nil
	if _, err := env.srv.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume retry: %v", err)
	}
	if env.agent.unexportVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("preserving delete ran destructive RPCs: unexport=%d delete=%d",
			env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	rel := env.agent.lastReleaseVolumeReq
	if env.agent.releaseVolumeCalls != 2 || rel.GetVolumeId() != "data-vg/legacy" ||
		rel.GetFence().GetVolumeUid() != string(uid) {
		t.Fatalf("ReleaseVolume calls=%d last=%+v, want 2 calls for data-vg/legacy under uid %s",
			env.agent.releaseVolumeCalls, rel, uid)
	}
	if volumeState(t, env, req.GetName()) != nil {
		t.Fatal("PillarVolumeState still exists after a successful release")
	}
}

// TestDeleteVolume_PreservedLV_PublishedRefused: a volume still published is
// refused before any agent call, whatever its policy.
func TestDeleteVolume_PreservedLV_PublishedRefused(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(""))
	resp := adoptLV(t, env, req)
	volumeID := resp.GetVolume().GetVolumeId()
	// Record a publication the way ControllerPublishVolume leaves it.
	pvs := volumeState(t, env, req.GetName())
	pvs.Status.PublishedNodes = []v1alpha1.VolumePublication{{NodeID: "worker-node-1"}}
	if err := env.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
		t.Fatalf("record publication: %v", err)
	}

	_, err := env.srv.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteVolume while published: err = %v, want FailedPrecondition", err)
	}
	if env.agent.releaseVolumeCalls != 0 || env.agent.unexportVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("published delete reached the agent: release=%d unexport=%d delete=%d",
			env.agent.releaseVolumeCalls, env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	pvs = volumeState(t, env, req.GetName())
	if pvs == nil || pvs.Status.Deleting {
		t.Fatalf("published delete: PillarVolumeState = %+v, want kept and not deleting", pvs)
	}
}

// TestDeleteVolume_ManagedLV_DestroysLikeManagedVolume: a Managed adoption
// is a normal volume — DeleteVolume removes it through the existing path.
func TestDeleteVolume_ManagedLV_DestroysLikeManagedVolume(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(v1alpha1.ImportLVPolicyManaged))
	resp := adoptLV(t, env, req)

	_, err := env.srv.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: resp.GetVolume().GetVolumeId()})
	if err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if env.agent.deleteVolumeCalls != 1 || env.agent.releaseVolumeCalls != 0 {
		t.Fatalf("Managed LV delete: delete=%d release=%d, want 1/0",
			env.agent.deleteVolumeCalls, env.agent.releaseVolumeCalls)
	}
	if volumeState(t, env, req.GetName()) != nil {
		t.Fatal("PillarVolumeState still exists after DeleteVolume")
	}
	if res := reservationNamed(t, env, lvReservationName()); res != nil {
		t.Fatalf("LV reservation outlived its lifecycle: %+v", res.Spec)
	}
}

// TestControllerExpandVolume_PreservedLV_Refused: a preserving adoption is
// never resized; the refusal happens before any agent call or fence commit.
func TestControllerExpandVolume_PreservedLV_Refused(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(""))
	resp := adoptLV(t, env, req)
	before := volumeState(t, env, req.GetName())

	_, err := env.srv.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      resp.GetVolume().GetVolumeId(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ControllerExpandVolume: err = %v, want FailedPrecondition", err)
	}
	if env.agent.expandVolumeCalls != 0 {
		t.Fatal("agent ExpandVolume ran on a preserving LV")
	}
	after := volumeState(t, env, req.GetName())
	if after.Status.PublicationGeneration != before.Status.PublicationGeneration ||
		after.Spec.CapacityBytes != before.Spec.CapacityBytes {
		t.Fatalf("refused expand mutated the record: generation %d -> %d, capacity %d -> %d",
			before.Status.PublicationGeneration, after.Status.PublicationGeneration,
			before.Spec.CapacityBytes, after.Spec.CapacityBytes)
	}
}

// TestControllerExpandVolume_ManagedLV_Expands: the Managed policy keeps the
// normal expand path.
func TestControllerExpandVolume_ManagedLV_Expands(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(v1alpha1.ImportLVPolicyManaged))
	resp := adoptLV(t, env, req)

	_, err := env.srv.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      resp.GetVolume().GetVolumeId(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume (Managed LV): %v", err)
	}
	if env.agent.expandVolumeCalls != 1 {
		t.Fatalf("agent ExpandVolume calls = %d, want 1", env.agent.expandVolumeCalls)
	}
}
