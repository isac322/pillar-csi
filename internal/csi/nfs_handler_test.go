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
	"reflect"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestNFSHandlerAttachIPv6Source(t *testing.T) {
	h := NewNFSHandler()
	got, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: ProtocolNFS,
		Address:      "2001:db8::10",
		Port:         "2049",
		VolumeRef:    "/child",
		Extra:        map[string]string{VolumeContextKeyNFSVersion: "4.2"},
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got.MountSource != "[2001:db8::10]:/child" {
		t.Fatalf("MountSource = %q, want bracketed IPv6 source", got.MountSource)
	}
	state, ok := got.State.(*NFSProtocolState)
	if !ok || state.MountSource != got.MountSource {
		t.Fatalf("Attach state = %#v, want typed state retaining source", got.State)
	}
}

func TestNFSHandlerRejectsUnsupportedSourceParameters(t *testing.T) {
	base := AttachParams{
		ProtocolType: ProtocolNFS,
		Address:      "10.0.0.8",
		Port:         "2049",
		VolumeRef:    "/child",
	}
	for name, mutate := range map[string]func(*AttachParams){
		"version": func(p *AttachParams) { p.Extra = map[string]string{VolumeContextKeyNFSVersion: "3"} },
		"port":    func(p *AttachParams) { p.Port = "2050" },
		"path":    func(p *AttachParams) { p.VolumeRef = "child" },
		"address": func(p *AttachParams) { p.Address = "bad/server" },
	} {
		t.Run(name, func(t *testing.T) {
			params := base
			mutate(&params)
			if _, err := NewNFSHandler().Attach(context.Background(), params); err == nil {
				t.Fatal("Attach succeeded for unsupported NFS source parameters")
			}
		})
	}
}

func TestNFSMountFlagsResolveAddressFamilyAndRejectInvariantOverrides(t *testing.T) {
	mount := &csi.VolumeCapability_MountVolume{MountFlags: []string{"noatime", "hard", "nfsvers=4.2"}}
	got, err := nfsMountFlags(map[string]string{}, mount, "10.0.0.8")
	if err != nil {
		t.Fatalf("nfsMountFlags IPv4: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"noatime", "hard", "nfsvers=4.2", "proto=tcp", "port=2049"}) {
		t.Fatalf("IPv4 flags = %#v, want merged flags with enforced IPv4 invariants", got)
	}

	got, err = nfsMountFlags(map[string]string{}, mount, "2001:db8::10")
	if err != nil {
		t.Fatalf("nfsMountFlags IPv6: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"noatime", "hard", "nfsvers=4.2", "proto=tcp6", "port=2049"}) {
		t.Fatalf("IPv6 flags = %#v, want merged flags with enforced IPv6 invariants", got)
	}

	for name, tc := range map[string]struct {
		raw     string
		address string
	}{
		"soft":         {raw: `["soft"]`, address: "10.0.0.8"},
		"vers3":        {raw: `["vers=3"]`, address: "10.0.0.8"},
		"udp":          {raw: `["proto=udp"]`, address: "10.0.0.8"},
		"rdma":         {raw: `["rdma"]`, address: "10.0.0.8"},
		"tcp6-on-ipv4": {raw: `["proto=tcp6"]`, address: "10.0.0.8"},
		"tcp-on-ipv6":  {raw: `["proto=tcp"]`, address: "2001:db8::10"},
	} {
		t.Run(name, func(t *testing.T) {
			_, mountErr := nfsMountFlags(map[string]string{paramMountOptions: tc.raw}, mount, tc.address)
			if mountErr == nil {
				t.Fatalf("nfsMountFlags(%s): error = %v, want invariant conflict", tc.raw, mountErr)
			}
		})
	}

	for name, tc := range map[string]struct {
		flags   []string
		address string
		want    string
	}{
		"tcp-on-ipv4":  {flags: []string{"tcp"}, address: "10.0.0.8", want: "proto=tcp"},
		"tcp6-on-ipv6": {flags: []string{"tcp6"}, address: "2001:db8::10", want: "proto=tcp6"},
	} {
		t.Run(name, func(t *testing.T) {
			capability := &csi.VolumeCapability_MountVolume{MountFlags: tc.flags}
			flags, mountErr := nfsMountFlags(map[string]string{}, capability, tc.address)
			if mountErr != nil {
				t.Fatalf("nfsMountFlags(%v): %v", tc.flags, mountErr)
			}
			if !reflect.DeepEqual(flags, []string{"hard", "nfsvers=4.2", tc.want, "port=2049"}) {
				t.Fatalf("flags = %#v, want canonical transport %q", flags, tc.want)
			}
		})
	}
}

func TestNFSNodeStageIPv6UsesBracketedSourceAndTCP6(t *testing.T) {
	mounter := newMockMounter()
	srv := &NodeServer{
		nodeID:   "node-a",
		handlers: map[string]ProtocolHandler{ProtocolNFS: NewNFSHandler()},
		mounter:  mounter,
		stateDir: t.TempDir(),
	}
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "node-a/nfs/zfs-dataset/tank/pvc-nfs-ipv6",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("nfs"),
		VolumeContext: map[string]string{
			VolumeContextKeyProtocolType: ProtocolNFS,
			VolumeContextKeyAddress:      "2001:db8::10",
			VolumeContextKeyPort:         "2049",
			VolumeContextKeyNFSVersion:   "4.2",
			vcVolumeRef:                  "/child",
			paramFSType:                  "nfs",
		},
	}
	if _, err := srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume IPv6: %v", err)
	}
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("mount calls = %#v, want one NFS mount", mounter.mountCalls)
	}
	call := mounter.mountCalls[0]
	if call.source != "[2001:db8::10]:/child" {
		t.Fatalf("mount source = %q, want bracketed IPv6 source", call.source)
	}
	if !reflect.DeepEqual(call.options, []string{"hard", "nfsvers=4.2", "proto=tcp6", "port=2049"}) {
		t.Fatalf("mount options = %#v, want IPv6 NFS options", call.options)
	}
}

func TestNFSNodeStagePersistsSourceAndRejectsMismatchedRestage(t *testing.T) {
	fixture := newNFSStageFixture(t)
	fixture.assertIdempotentRestage(t)
	fixture.assertMismatchedRestage(t)
	fixture.cleanup(t)
}

type nfsStageFixture struct {
	mounter     *mockMounter
	stateDir    string
	stagingPath string
	req         *csi.NodeStageVolumeRequest
	restarted   *NodeServer
}

func newNFSStageFixture(t *testing.T) *nfsStageFixture {
	t.Helper()
	mounter := newMockMounter()
	stateDir := t.TempDir()
	handlers := map[string]ProtocolHandler{ProtocolNFS: NewNFSHandler()}
	srv := &NodeServer{nodeID: "node-a", handlers: handlers, mounter: mounter, stateDir: stateDir}
	stagingPath := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "node-a/nfs/zfs-dataset/tank/pvc-nfs",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("nfs"),
		VolumeContext: map[string]string{
			VolumeContextKeyProtocolType: ProtocolNFS,
			VolumeContextKeyAddress:      "10.0.0.8",
			VolumeContextKeyPort:         "2049",
			VolumeContextKeyNFSVersion:   "4.2",
			vcVolumeRef:                  "/child",
			paramFSType:                  "nfs",
		},
	}
	if _, err := srv.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if len(mounter.mountCalls) != 1 || mounter.mountCalls[0].source != "10.0.0.8:/child" {
		t.Fatalf("mount calls = %#v, want direct NFS source mount", mounter.mountCalls)
	}
	state, err := srv.readStageState(req.VolumeId)
	if err != nil || state == nil || state.NFS == nil || state.NFS.MountSource != "10.0.0.8:/child" {
		t.Fatalf("persisted state = %#v, err=%v, want typed NFS source", state, err)
	}
	return &nfsStageFixture{
		mounter:     mounter,
		stateDir:    stateDir,
		stagingPath: stagingPath,
		req:         req,
		restarted: &NodeServer{
			nodeID: "node-a", handlers: handlers, mounter: mounter, stateDir: stateDir,
		},
	}
}

func (f *nfsStageFixture) assertIdempotentRestage(t *testing.T) {
	t.Helper()
	if _, err := f.restarted.NodeStageVolume(context.Background(), f.req); err != nil {
		t.Fatalf("idempotent restage after restart: %v", err)
	}
	if len(f.mounter.mountCalls) != 1 {
		t.Fatalf("idempotent restage remounted NFS source: %#v", f.mounter.mountCalls)
	}
}

func (f *nfsStageFixture) assertMismatchedRestage(t *testing.T) {
	t.Helper()
	cloned := proto.Clone(f.req)
	mismatch, ok := cloned.(*csi.NodeStageVolumeRequest)
	if !ok {
		t.Fatalf("proto.Clone type = %T, want *csi.NodeStageVolumeRequest", cloned)
	}
	mismatch.VolumeContext[VolumeContextKeyAddress] = "10.0.0.9"
	_, err := f.restarted.NodeStageVolume(context.Background(), mismatch)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mismatched restage code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}

func (f *nfsStageFixture) cleanup(t *testing.T) {
	t.Helper()
	cleaned := &NodeServer{nodeID: "node-a", mounter: f.mounter, stateDir: f.stateDir}
	if _, err := cleaned.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          f.req.VolumeId,
		StagingTargetPath: f.stagingPath,
	}); err != nil {
		t.Fatalf("NodeUnstageVolume after handler restart: %v", err)
	}
	mounted, mountErr := f.mounter.IsMounted(f.stagingPath)
	if mountErr != nil {
		t.Fatalf("IsMounted(%q): %v", f.stagingPath, mountErr)
	}
	if mounted {
		t.Fatal("NFS staging path remained mounted after restart cleanup")
	}
	if state, err := cleaned.readStageState(f.req.VolumeId); err != nil || state != nil {
		t.Fatalf("stage state after cleanup = %#v, err=%v, want absent", state, err)
	}
}
