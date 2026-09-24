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

// Cross-process fencing regression tests for the export resync (issues #55,
// #56).  Two independent *ControllerServer values (separate volumeLocks and
// state machines, i.e. two controller processes) share one Kubernetes API and
// one real agent.Server over bufconn with a temp configfs root and a temp
// durable state dir.  Controller A is a stale leader whose ReconcileState RPC
// is delayed in flight after it has already read its desired state — the
// interleaving a paused process, a lost lease, or a slow network produces.
// Controller B is the new leader.
//
// B's publish/unpublish/delete are emulated by their durable effects (PVS
// status writes through the shared API client, each bumping
// publicationGeneration and marking revoking records) plus the real fenced
// agent RPCs.  Every scenario asserts the stale resync is rejected and B's
// state survives.

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// gatedAgentClient delays ReconcileState until release is closed, after the
// caller has already read its desired state (entered is closed first).
type gatedAgentClient struct {
	agentv1.AgentServiceClient
	entered chan struct{}
	release chan struct{}
}

func (g *gatedAgentClient) ReconcileState(
	ctx context.Context, req *agentv1.ReconcileStateRequest, opts ...grpc.CallOption,
) (*agentv1.ReconcileStateResponse, error) {
	close(g.entered)
	<-g.release
	return g.AgentServiceClient.ReconcileState(ctx, req, opts...)
}

// staleLeader builds controller process A: its own ControllerServer (own
// in-process locks) against the SAME API and agent as env.srv (process B).
func staleLeader(env *resyncEnv) (*ControllerServer, *gatedAgentClient) {
	g := &gatedAgentClient{
		AgentServiceClient: env.agentClient,
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	dialer := func(context.Context, string) (agentv1.AgentServiceClient, io.Closer, error) {
		return g, nopCloser{}, nil
	}
	return NewControllerServerWithDialer(env.srv.k8sClient, "pillar-csi.bhyoo.com", dialer), g
}

func runStaleResync(t *testing.T, a *ControllerServer, g *gatedAgentClient) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- a.ReconcileVolumeExport(context.Background(), resyncPVSName) }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stale resync never reached the agent")
	}
	return done
}

// finish releases the stale resync and requires the agent to reject it.
func finish(t *testing.T, g *gatedAgentClient, done chan error) {
	t.Helper()
	close(g.release)
	select {
	case err := <-done:
		if err == nil {
			t.Error("stale resync succeeded; the agent must reject its fencing token")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale resync did not finish")
	}
}

// casPublication applies the new leader's publication transition the way
// ControllerPublishVolume/ControllerUnpublishVolume do: read, mutate
// publishedNodes (revoking marks a record being unpublished), bump
// publicationGeneration, write back.  It returns the committed generation.
func casPublication(t *testing.T, env *resyncEnv, keep []string, revoking ...string) uint64 {
	t.Helper()
	ctx := context.Background()
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: resyncPVSName}, pvs); err != nil {
		t.Fatalf("get PVS: %v", err)
	}
	pvs.Status.PublishedNodes = nil
	for i, id := range keep {
		pvs.Status.PublishedNodes = append(pvs.Status.PublishedNodes, v1alpha1.VolumePublication{
			NodeID: "node-" + string(rune('a'+i)), InitiatorID: id, AccessMode: "SINGLE_NODE_WRITER",
		})
	}
	for _, id := range revoking {
		pvs.Status.PublishedNodes = append(pvs.Status.PublishedNodes, v1alpha1.VolumePublication{
			NodeID: "node-revoking", InitiatorID: id, AccessMode: "SINGLE_NODE_WRITER", Revoking: true,
		})
	}
	pvs.Status.PublicationGeneration++
	if err := env.srv.k8sClient.Status().Update(ctx, pvs); err != nil {
		t.Fatalf("CAS PVS publishedNodes: %v", err)
	}
	return uint64(pvs.Status.PublicationGeneration) //nolint:gosec // G115: test generations are small and positive
}

// A stale resync must not revoke a grant the new leader committed at a newer
// generation.
func TestReconcileFencing_StaleResyncCannotRevokeNewGrant(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA))
	env.exportWithHosts(t, resyncHostA)
	a, g := staleLeader(env)
	done := runStaleResync(t, a, g) // A read desired = {hostA} at gen 1

	gen := casPublication(t, env, []string{resyncHostA, resyncHostB})
	if _, err := env.agentClient.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: resyncHostB, Fence: resyncFence(gen),
	}); err != nil {
		t.Fatalf("B AllowInitiator: %v", err)
	}

	finish(t, g, done)
	if !env.hostAllowed(resyncHostB) {
		t.Errorf("stale resync revoked hostB although it was granted at a newer generation")
	}
}

// A stale resync must not re-grant an initiator the new leader unpublished.
func TestReconcileFencing_StaleResyncCannotRegrantRevokedHost(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA, resyncHostB))
	env.exportWithHosts(t, resyncHostA, resyncHostB)
	a, g := staleLeader(env)
	done := runStaleResync(t, a, g) // A read desired = {hostA, hostB} at gen 1

	gen := casPublication(t, env, []string{resyncHostA}, resyncHostB)
	if _, err := env.agentClient.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: resyncHostB, Fence: resyncFence(gen),
	}); err != nil {
		t.Fatalf("B DenyInitiator: %v", err)
	}

	finish(t, g, done)
	if env.hostAllowed(resyncHostB) {
		t.Errorf("stale resync re-granted unpublished hostB (ACL leak)")
	}
}

// A stale resync must not resurrect the export of a volume the new leader
// deleted (deleting flag + fenced UnexportVolume).
func TestReconcileFencing_StaleResyncCannotResurrectDeletedExport(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec))
	env.exportWithHosts(t)
	a, g := staleLeader(env)
	done := runStaleResync(t, a, g) // A read the PVS at gen 1

	ctx := context.Background()
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: resyncPVSName}, pvs); err != nil {
		t.Fatalf("get PVS: %v", err)
	}
	pvs.Status.Deleting = true
	pvs.Status.PublicationGeneration++
	if err := env.srv.k8sClient.Status().Update(ctx, pvs); err != nil {
		t.Fatalf("CAS PVS deleting: %v", err)
	}
	if _, err := env.agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Fence: resyncFence(uint64(pvs.Status.PublicationGeneration)), //nolint:gosec // G115: small positive
	}); err != nil {
		t.Fatalf("B UnexportVolume: %v", err)
	}
	if err := env.srv.k8sClient.Delete(ctx, pvs); err != nil {
		t.Fatalf("B delete PVS: %v", err)
	}

	finish(t, g, done)
	if _, err := os.Stat(env.subsystemPath()); err == nil {
		t.Errorf("stale resync resurrected the export of a deleted volume")
	}
}
