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

// Tests for NodeGetInfo — verifying the node identity contract.
//
// AC 1: NodeGetInfo.node_id must return the Kubernetes node name, not a
// transport-level identity such as an NVMe host NQN or iSCSI initiator IQN.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodeGetInfo

import (
	"context"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestNodeGetInfo_NodeIDIsKubernetesNodeName verifies that NodeGetInfo returns
// the Kubernetes node name as NodeId — not an NVMe host NQN or any other
// transport-level identity (RFC §5.1).
func TestNodeGetInfo_NodeIDIsKubernetesNodeName(t *testing.T) {
	t.Parallel()

	const k8sNodeName = "worker-node-1"

	srv := NewNodeServerWithStateDir(k8sNodeName, &mockConnector{}, &mockMounter{}, t.TempDir())
	resp, err := srv.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo returned unexpected error: %v", err)
	}
	if resp.NodeId != k8sNodeName {
		t.Errorf("NodeGetInfo.NodeId = %q, want Kubernetes node name %q", resp.NodeId, k8sNodeName)
	}
}

// TestNodeGetInfo_EmptyNodeID verifies that NodeGetInfo returns an Internal
// gRPC error when the NodeServer was constructed with an empty nodeID.
// This guards against accidental misconfiguration where NODE_NAME env var was
// not injected and --node-id flag was not provided.
func TestNodeGetInfo_EmptyNodeID(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("", &mockConnector{}, &mockMounter{}, t.TempDir())
	_, err := srv.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err == nil {
		t.Fatal("NodeGetInfo with empty nodeID: expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("NodeGetInfo with empty nodeID: error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("NodeGetInfo with empty nodeID: code = %v, want %v", st.Code(), codes.Internal)
	}
}

// TestNodeGetInfo_NodeIDNotNQN verifies that a node name that looks like an
// NVMe NQN is preserved verbatim but that the typical K8s node name (a DNS
// label) round-trips correctly.  This test makes the contract visible: the
// caller is responsible for passing the correct node name; the server does not
// interpret or transform the value.
func TestNodeGetInfo_NodeIDRoundTrips(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		nodeID string
	}{
		{"simple DNS label", "worker-1"},
		{"FQDN-like", "worker-1.us-east-1.compute.internal"},
		{"numeric suffix", "node-42"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := NewNodeServerWithStateDir(tc.nodeID, &mockConnector{}, &mockMounter{}, t.TempDir())
			resp, err := srv.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
			if err != nil {
				t.Fatalf("NodeGetInfo(%q): unexpected error: %v", tc.nodeID, err)
			}
			if resp.NodeId != tc.nodeID {
				t.Errorf("NodeGetInfo(%q): NodeId = %q, want %q", tc.nodeID, resp.NodeId, tc.nodeID)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Topology reporting tests (RFC §5.8)
// ─────────────────────────────────────────────────────────────────────────────.

// topologyTrue aliases the production constant for use in topology assertions.
const topologyTrue = topologyValueTrue

// stubProber is a test ProtocolProber with a configurable availability flag.
type stubProber struct {
	nvmeof bool
}

func (s *stubProber) NVMeoFAvailable() bool { return s.nvmeof }

// TestNodeGetInfo_TopologyNVMeoF verifies that when NVMe-oF is available the
// AccessibleTopology contains exactly the NVMe-oF key.
func TestNodeGetInfo_TopologyNVMeoF(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("worker-1", &mockConnector{}, &mockMounter{}, t.TempDir()).
		WithTopologyProber(&stubProber{nvmeof: true})

	resp, err := srv.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: unexpected error: %v", err)
	}
	if resp.AccessibleTopology == nil {
		t.Fatal("AccessibleTopology is nil, expected non-nil topology")
	}
	segs := resp.AccessibleTopology.GetSegments()
	if segs[TopologyKeyNVMeoF] != topologyTrue {
		t.Errorf("expected %q = \"true\", got %q", TopologyKeyNVMeoF, segs[TopologyKeyNVMeoF])
	}
	if len(segs) != 1 {
		t.Errorf("segments = %v, want only %q", segs, TopologyKeyNVMeoF)
	}
}

// TestNodeGetInfo_TopologyNoProtocols verifies that when no protocols are
// available AccessibleTopology is nil (no segments to report).
func TestNodeGetInfo_TopologyNoProtocols(t *testing.T) {
	t.Parallel()

	srv := NewNodeServerWithStateDir("worker-1", &mockConnector{}, &mockMounter{}, t.TempDir()).
		WithTopologyProber(&stubProber{})

	resp, err := srv.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: unexpected error: %v", err)
	}
	if resp.AccessibleTopology != nil {
		t.Errorf("expected AccessibleTopology nil when no protocols available, got %v",
			resp.AccessibleTopology)
	}
}
