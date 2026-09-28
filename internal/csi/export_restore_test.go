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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
)

// These tests drive RestoreAgentExports against a real agent.Server started
// with its export restore pending, as the agent binary starts after a
// storage-node reboot (issue #92).

// restorePVS is a volume of agentName, named name, exported on the shared
// aclSpec port with one published host.
func restorePVS(agentName, name, uid string) *v1alpha1.PillarVolumeState {
	agentVolID := "tank/" + name
	return &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      agentName + "/nvmeof-tcp/zfs-zvol/" + agentVolID,
			AgentVolumeID: agentVolID, AgentRef: agentName,
			BackendType: "zfs-zvol", ProtocolType: "nvmeof-tcp",
		},
		Status: v1alpha1.PillarVolumeStateStatus{
			Phase: v1alpha1.PillarVolumeStatePhaseReady, ExportSpec: aclSpec,
			PublicationGeneration: 1,
			PublishedNodes: []v1alpha1.VolumePublication{{
				NodeID: "node-a", InitiatorID: resyncHostA, AccessMode: "SINGLE_NODE_WRITER",
			}},
		},
	}
}

func restoreNQN(name string) string {
	return "nqn.2026-01.com.bhyoo.pillar-csi:tank." + name
}

// linkedSubsystems returns the NQNs linked to any nvmet port.
func (e *resyncEnv) linkedSubsystems(t *testing.T) []string {
	t.Helper()
	links, err := filepath.Glob(filepath.Join(e.cfgRoot, "nvmet", "ports", "*", "subsystems", "*"))
	if err != nil {
		t.Fatalf("glob port links: %v", err)
	}
	nqns := make([]string, 0, len(links))
	for _, link := range links {
		nqns = append(nqns, filepath.Base(link))
	}
	slices.Sort(nqns)
	return nqns
}

func (e *resyncEnv) restorePending(t *testing.T) bool {
	t.Helper()
	resp, err := e.agentClient.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	return resp.GetExportRestorePending()
}

func (e *resyncEnv) conditionOf(t *testing.T, name string) *metav1.Condition {
	t.Helper()
	pvs := &v1alpha1.PillarVolumeState{}
	err := e.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, pvs)
	if err != nil {
		t.Fatalf("get PillarVolumeState %s: %v", name, err)
	}
	return meta.FindStatusCondition(pvs.Status.Conditions, ConditionExportReconciled)
}

func TestRestoreAgentExports_LinksEveryExportOfTheAgentInOneRequest(t *testing.T) {
	t.Parallel()
	const otherAgent = "storage-2"
	vol1 := restorePVS(resyncAgentName, "pvc-restore-1", "aaaaaaaa-0000-0000-0000-000000000001")
	vol2 := restorePVS(resyncAgentName, "pvc-restore-2", "aaaaaaaa-0000-0000-0000-000000000002")
	foreign := restorePVS(otherAgent, "pvc-foreign", "aaaaaaaa-0000-0000-0000-000000000003")
	env := newResyncEnvObjects(t, t.TempDir(), []client.Object{vol1, vol2, foreign},
		agent.WithExportRestoreGate())
	ctx := context.Background()

	assertPendingGate(t, env, vol1)

	calls := env.reconcileCalls.Load()
	err := env.srv.RestoreAgentExports(ctx, resyncAgentName)
	if err != nil {
		t.Fatalf("RestoreAgentExports: %v", err)
	}
	if got := env.reconcileCalls.Load() - calls; got != 1 {
		t.Errorf("ReconcileState calls = %d, want exactly one batch request", got)
	}

	want := []string{restoreNQN(vol1.Name), restoreNQN(vol2.Name)}
	if linked := env.linkedSubsystems(t); !slices.Equal(linked, want) {
		t.Errorf("linked subsystems = %v, want %v (the other agent's volume excluded)", linked, want)
	}
	if env.restorePending(t) {
		t.Error("export restore still pending after RestoreAgentExports")
	}
	for _, name := range []string{vol1.Name, vol2.Name} {
		cond := env.conditionOf(t, name)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonExportReconciled {
			t.Errorf("%s ExportReconciled = %+v, want True/%s", name, cond, reasonExportReconciled)
		}
	}
	if cond := env.conditionOf(t, foreign.Name); cond != nil {
		t.Errorf("volume of another agent got ExportReconciled %+v, want untouched", cond)
	}
}

// assertPendingGate verifies that while the export restore is pending, a
// non-complete ReconcileState and the per-volume resync path are refused and
// no subsystem is linked.
func assertPendingGate(t *testing.T, env *resyncEnv, vol *v1alpha1.PillarVolumeState) {
	t.Helper()
	ctx := context.Background()
	if !env.restorePending(t) {
		t.Fatal("gated agent must start with the export restore pending")
	}
	// The per-volume resync path is refused while the restore is pending,
	// so no single volume can make the shared port listen early.
	desired, err := desiredVolumeState(vol)
	if err != nil {
		t.Fatalf("desiredVolumeState: %v", err)
	}
	_, err = env.agentClient.ReconcileState(ctx, &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{desired},
	})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "export restore pending") {
		t.Fatalf("per-volume ReconcileState while pending: err = %v, want Unavailable export restore pending", err)
	}
	if err = env.srv.ReconcileVolumeExport(ctx, vol.Name); err == nil {
		t.Fatal("ReconcileVolumeExport succeeded while the agent export restore is pending")
	}
	if linked := env.linkedSubsystems(t); len(linked) != 0 {
		t.Fatalf("subsystems linked before restore: %v", linked)
	}
}

func TestRestoreAgentExports_EmptyAgentStillClearsPending(t *testing.T) {
	t.Parallel()
	foreign := restorePVS("storage-2", "pvc-foreign", "aaaaaaaa-0000-0000-0000-000000000004")
	env := newResyncEnvObjects(t, t.TempDir(), []client.Object{foreign}, agent.WithExportRestoreGate())

	if err := env.srv.RestoreAgentExports(context.Background(), resyncAgentName); err != nil {
		t.Fatalf("RestoreAgentExports: %v", err)
	}
	if env.restorePending(t) {
		t.Error("export restore still pending after an empty complete restore")
	}
	if linked := env.linkedSubsystems(t); len(linked) != 0 {
		t.Errorf("linked subsystems = %v, want none", linked)
	}
}

// A volume the agent rejects fails the restore (so the caller retries) and is
// reported on its own condition, while the other volumes are still restored.
func TestRestoreAgentExports_ItemFailureIsReportedAndRetried(t *testing.T) {
	t.Parallel()
	good := restorePVS(resyncAgentName, "pvc-restore-good", "aaaaaaaa-0000-0000-0000-000000000005")
	bad := restorePVS(resyncAgentName, "pvc-restore-bad", "aaaaaaaa-0000-0000-0000-000000000006")
	// Unknown pool: the agent cannot resolve the backend device.
	bad.Spec.AgentVolumeID = "nopool/pvc-restore-bad"
	env := newResyncEnvObjects(t, t.TempDir(), []client.Object{good, bad}, agent.WithExportRestoreGate())

	if err := env.srv.RestoreAgentExports(context.Background(), resyncAgentName); err == nil {
		t.Fatal("RestoreAgentExports succeeded although the agent rejected a volume")
	}
	if cond := env.conditionOf(t, bad.Name); cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != reasonReconcileFailed {
		t.Errorf("rejected volume ExportReconciled = %+v, want False/%s", cond, reasonReconcileFailed)
	}
	if cond := env.conditionOf(t, good.Name); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("restored volume ExportReconciled = %+v, want True", cond)
	}
	if linked := env.linkedSubsystems(t); !slices.Equal(linked, []string{restoreNQN(good.Name)}) {
		t.Errorf("linked subsystems = %v, want only %s", linked, restoreNQN(good.Name))
	}
}

// TestRestoreAgentExports_EstablishedInCapsuleSizeWinsTheSharedPort verifies
// the restore order on a shared port: a volume whose CreateVolume completed
// fixes the port's param_inline_data_size before a volume whose export never
// succeeded (CreatePartial after a conflict), even when the partial volume
// is listed first, so a failed attempt cannot lock working volumes out.
func TestRestoreAgentExports_EstablishedInCapsuleSizeWinsTheSharedPort(t *testing.T) {
	t.Parallel()
	established, conflicting := int32(8192), int32(4096)
	partial := restorePVS(resyncAgentName, "pvc-a-partial", "aaaaaaaa-0000-0000-0000-000000000007")
	partial.Status.Phase = v1alpha1.PillarVolumeStatePhaseCreatePartial
	partial.Status.ExportSpec = &v1alpha1.VolumeExportSpec{
		BindAddress: "10.0.0.1", Port: 4420, ACLEnabled: true, InCapsuleDataSize: &conflicting,
	}
	unset := restorePVS(resyncAgentName, "pvc-b-unset", "aaaaaaaa-0000-0000-0000-000000000008")
	ready := restorePVS(resyncAgentName, "pvc-c-ready", "aaaaaaaa-0000-0000-0000-000000000009")
	ready.Status.ExportSpec = &v1alpha1.VolumeExportSpec{
		BindAddress: "10.0.0.1", Port: 4420, ACLEnabled: true, InCapsuleDataSize: &established,
	}
	env := newResyncEnvObjects(t, t.TempDir(), []client.Object{partial, unset, ready}, agent.WithExportRestoreGate())

	if err := env.srv.RestoreAgentExports(context.Background(), resyncAgentName); err == nil {
		t.Fatal("RestoreAgentExports succeeded although the partial volume conflicts with the port")
	}

	want := []string{restoreNQN(unset.Name), restoreNQN(ready.Name)}
	if linked := env.linkedSubsystems(t); !slices.Equal(linked, want) {
		t.Errorf("linked subsystems = %v, want %v", linked, want)
	}
	sizes, err := filepath.Glob(filepath.Join(env.cfgRoot, "nvmet", "ports", "*", "param_inline_data_size"))
	if err != nil || len(sizes) != 1 {
		t.Fatalf("param_inline_data_size files = %v (err %v), want exactly one", sizes, err)
	}
	raw, err := os.ReadFile(sizes[0])
	if err != nil || strings.TrimSpace(string(raw)) != "8192" {
		t.Errorf("param_inline_data_size = %q err=%v, want the established 8192", raw, err)
	}
	if cond := env.conditionOf(t, partial.Name); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("partial volume ExportReconciled = %+v, want False", cond)
	}
}

// TestRestoreAgentExports_RequiredInCapsuleSizeLinksBeforeUnrestricted
// verifies that a volume accepting any in-capsule data size does not enable
// the shared port with the transport default ahead of a CreatePartial volume
// that requires a value: both are restored and the port carries the value.
func TestRestoreAgentExports_RequiredInCapsuleSizeLinksBeforeUnrestricted(t *testing.T) {
	t.Parallel()
	required := int32(8192)
	unset := restorePVS(resyncAgentName, "pvc-a-unset", "aaaaaaaa-0000-0000-0000-000000000010")
	partial := restorePVS(resyncAgentName, "pvc-b-partial", "aaaaaaaa-0000-0000-0000-000000000011")
	partial.Status.Phase = v1alpha1.PillarVolumeStatePhaseCreatePartial
	partial.Status.ExportSpec = &v1alpha1.VolumeExportSpec{
		BindAddress: "10.0.0.1", Port: 4420, ACLEnabled: true, InCapsuleDataSize: &required,
	}
	env := newResyncEnvObjects(t, t.TempDir(), []client.Object{unset, partial}, agent.WithExportRestoreGate())

	if err := env.srv.RestoreAgentExports(context.Background(), resyncAgentName); err != nil {
		t.Fatalf("RestoreAgentExports: %v", err)
	}
	want := []string{restoreNQN(unset.Name), restoreNQN(partial.Name)}
	if linked := env.linkedSubsystems(t); !slices.Equal(linked, want) {
		t.Errorf("linked subsystems = %v, want %v", linked, want)
	}
}
