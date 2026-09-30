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
	"errors"
	"net"
	"slices"
	"testing"

	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agentclient"
	"github.com/isac322/pillar-csi/internal/testutil/fakeuid"
	"github.com/isac322/pillar-csi/internal/testutil/testcerts"
	"github.com/isac322/pillar-csi/internal/tlscreds"
)

// startTCPAgent serves a real agent.Server, started with its export restore
// pending, on a loopback TCP port with the given server options (transport
// credentials). It returns the listen address and the agent's configfs root.
func startTCPAgent(t *testing.T, opts ...grpc.ServerOption) (addr, cfgRoot string) {
	t.Helper()
	cfgRoot = t.TempDir()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	gs := grpc.NewServer(opts...)
	agentv1.RegisterAgentServiceServer(gs,
		agent.NewServer(map[string]backend.VolumeBackend{"tank": resyncBackend{}}, cfgRoot,
			agent.WithDrainStateDir(t.TempDir()), agent.WithExportRestoreGate()))
	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()
	t.Cleanup(func() {
		gs.Stop()
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("agent gRPC server: %v", err)
		}
	})
	return lis.Addr().String(), cfgRoot
}

// The tests below verify that the production constructor routes export
// restoration and later per-volume export reconciliation through the injected
// agent connection manager, with mutual TLS or plaintext transport (issue
// #141). The second call runs after the first call's closer, so they also
// prove the manager's cached connection survives per-call cleanup.

func TestNewControllerServerWithAgentDialer_MutualTLSAgent(t *testing.T) {
	t.Parallel()
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}
	serverCreds, err := tlscreds.NewServerCredentials(bundle.ServerCert, bundle.ServerKey, bundle.CACert)
	if err != nil {
		t.Fatalf("NewServerCredentials: %v", err)
	}
	clientCreds, err := tlscreds.NewClientCredentials(bundle.ClientCert, bundle.ClientKey, bundle.CACert, "")
	if err != nil {
		t.Fatalf("NewClientCredentials: %v", err)
	}
	addr, cfgRoot := startTCPAgent(t, grpc.Creds(serverCreds))
	assertSharedDialerServesAgent(t, addr, cfgRoot, agentclient.NewManagerWithTLSCredentials(clientCreds))
}

func TestNewControllerServerWithAgentDialer_PlaintextAgent(t *testing.T) {
	t.Parallel()
	addr, cfgRoot := startTCPAgent(t)
	assertSharedDialerServesAgent(t, addr, cfgRoot, agentclient.NewManager())
}

// assertSharedDialerServesAgent restores the exports of the agent at addr and
// then resyncs one volume, both through a ControllerServer built on manager.
func assertSharedDialerServesAgent(t *testing.T, addr, cfgRoot string, manager *agentclient.Manager) {
	t.Helper()
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close agent manager: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	vol := restorePVS(resyncAgentName, "pvc-shared-dialer", "aaaaaaaa-0000-0000-0000-000000000141")
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&v1alpha1.PillarAgent{
			ObjectMeta: metav1.ObjectMeta{Name: resyncAgentName},
			Status:     v1alpha1.PillarAgentStatus{ResolvedAddress: addr},
		}, vol).
		WithStatusSubresource(&v1alpha1.PillarAgent{}, &v1alpha1.PillarVolumeState{}).
		WithInterceptorFuncs(fakeuid.Interceptor()).
		Build()
	env := &resyncEnv{
		srv:     NewControllerServerWithAgentDialer(k8s, k8s, "pillar-csi.bhyoo.com", manager),
		cfgRoot: cfgRoot,
	}
	ctx := context.Background()

	if err := env.srv.RestoreAgentExports(ctx, resyncAgentName); err != nil {
		t.Fatalf("RestoreAgentExports: %v", err)
	}
	want := []string{restoreNQN(vol.Name)}
	if linked := env.linkedSubsystems(t); !slices.Equal(linked, want) {
		t.Fatalf("linked subsystems = %v, want %v", linked, want)
	}

	if err := env.srv.ReconcileVolumeExport(ctx, vol.Name); err != nil {
		t.Fatalf("ReconcileVolumeExport after RestoreAgentExports: %v", err)
	}
	cond := env.conditionOf(t, vol.Name)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonExportReconciled {
		t.Errorf("ExportReconciled = %+v, want True/%s", cond, reasonExportReconciled)
	}
}
