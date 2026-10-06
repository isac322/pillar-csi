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

// Contract tests for the file driver profile (files.pillar-csi.bhyoo.com):
// adopted filesystems publish directly — NodeStageVolume was never advertised
// for them and the profile reports only GET_VOLUME_STATS.  NodePublishVolume
// accepts an empty staging_target_path, binds the controller-owned proxy for
// same-node consumers and mounts the NFS export per target for remote ones.
// Every RPC surface is exercised through the public methods on real or mock
// mounters; no private lifecycle helper is referenced.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/isac322/pillar-csi/api/v1alpha1"
)

const filePublishVolumeID = "agent/nfs/directory/pool/native/lifecycle"

// fileDirectoryAdoption returns the immutable adoption descriptor JSON of a
// directory adoption bound to the native identity described by fixture data.
func fileDirectoryAdoption(source, fsType string, inode uint64) string {
	data, err := json.Marshal(filesystemContextAdoption{
		Kind:            "directory",
		CanonicalSource: source,
		ResourceID:      "uuid:42",
		FilesystemType:  fsType,
		FilesystemID:    "uuid",
		Inode:           inode,
		ProjectID:       7,
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}

// fileRemotePublishContext returns the PublishContext a controller produces
// for a remote consumer: the same immutable adoption record plus the trusted
// agent routing triple (which VolumeContext must never be able to override).
func fileRemotePublishContext(adoption, layout string) map[string]string {
	return map[string]string{
		PublishContextKeyFilesystemAdoption: adoption,
		PublishContextKeyFilesystemCapacity: "1048576",
		PublishContextKeyFilesystemLayout:   layout,
		fileContextAgentAddress:             "storage-node:9500",
		fileContextAgentName:                "storage-node",
		fileContextAgentVolumeID:            "pool/native",
	}
}

// nfsTransportRoute is the VolumeContext of an adopted PV whose remote
// consumers mount it over NFS; its csi.fsType is the "nfs" transport type.
func nfsTransportRoute() map[string]string {
	return map[string]string{VolumeContextKeyProtocolType: ProtocolNFS, paramFSType: ProtocolNFS}
}

// filePublishRequest builds a NodePublish request the kubelet issues for an
// adopted filesystem: no staging path exists because the profile never staged.
func filePublishRequest(volumeID, target string, capability *csipb.VolumeCapability) *csipb.NodePublishVolumeRequest {
	return &csipb.NodePublishVolumeRequest{
		VolumeId:         volumeID,
		TargetPath:       target,
		VolumeCapability: capability,
		VolumeContext:    nfsTransportRoute(),
	}
}

// fileRemotePublishRequest is a remote consumer publish: the node mounts the
// controller-advertised NFS export directly at the pod target path.
func fileRemotePublishRequest(
	volumeID, target string, capability *csipb.VolumeCapability,
) *csipb.NodePublishVolumeRequest {
	req := filePublishRequest(volumeID, target, capability)
	req.PublishContext = fileRemotePublishContext(
		fileDirectoryAdoption("/existing/data", "ext4", 42),
		`{"directory":{"logicalPool":"pool","hostRoot":"/existing"}}`)
	return req
}

// clonePublishRequest returns a checked deep copy of a publish request; a
// failed assertion fails the test instead of panicking in an RPC call.
func clonePublishRequest(t *testing.T, req *csipb.NodePublishVolumeRequest) *csipb.NodePublishVolumeRequest {
	t.Helper()
	clone, ok := proto.Clone(req).(*csipb.NodePublishVolumeRequest)
	if !ok {
		t.Fatalf("clone of %T changed type", req)
	}
	return clone
}

// fileRemoteVolumeContext returns the VolumeContext the controller resolves
// for a remote NFS consumer: the export identity and NFS mount contract.
func fileRemoteVolumeContext() map[string]string {
	vc := nfsTransportRoute()
	vc[VolumeContextKeyAddress] = "192.0.2.10"
	vc[VolumeContextKeyPort] = "2049"
	vc[vcVolumeRef] = "/export/native"
	return vc
}

// newFilePublishNode builds a file-profile node wired to the given mounter
// and a fresh state dir; tests use it for the direct-publish contract.
func newFilePublishNode(t *testing.T, mounter Mounter) (node *NodeServer, stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	node = NewNodeServer("storage-node", nil, mounter).
		WithDriverName(v1alpha1.FileCSIDriver).WithStateDir(stateDir)
	return node, stateDir
}

// filePublishStateFilePath reports the durable record path the stats gateway
// and republish idempotence share; the record's filename derives from the
// volume handle exactly like every other node state file.
func filePublishStateFilePath(stateDir, volumeID string) string {
	return filepath.Join(stateDir, stateFileKey(volumeID)+".json")
}

func assertFileRecordAbsent(t *testing.T, node *NodeServer, stateDir, volumeID string) {
	t.Helper()
	if _, err := os.Stat(filePublishStateFilePath(stateDir, volumeID)); !os.IsNotExist(err) {
		t.Fatalf("file publish left a durable record at %s: %v", filePublishStateFilePath(stateDir, volumeID), err)
	}
	if state, err := node.readStageState(volumeID); err != nil || state != nil {
		t.Fatalf("readStageState(%q) = %+v, %v; want no record", volumeID, state, err)
	}
}

// assertFileRecordPresent checks the shared fixture volume's durable
// record exists both through the node and on disk.
func assertFileRecordPresent(t *testing.T, node *NodeServer, stateDir string) *nodeStageState {
	t.Helper()
	volumeID := filePublishVolumeID
	state, err := node.readStageState(volumeID)
	if err != nil || state == nil || state.File == nil {
		t.Fatalf("durable file record = %+v, %v; want an adopted filesystem record", state, err)
	}
	if _, err := os.Stat(filePublishStateFilePath(stateDir, volumeID)); err != nil {
		t.Fatalf("record missing on disk at %s: %v", filePublishStateFilePath(stateDir, volumeID), err)
	}
	return state
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeGetCapabilities — the file profile advertises direct publish only
// ─────────────────────────────────────────────────────────────────────────────

// TestFileNodeGetCapabilities pins the NoStage profile contract: the file
// driver never advertises STAGE_UNSTAGE_VOLUME or EXPAND_VOLUME so the CO
// calls NodePublishVolume directly, while stats stay available for the CO's
// periodic usage queries. The legacy block profile keeps its full set.
func TestFileNodeGetCapabilities(t *testing.T) {
	t.Parallel()

	fileNode, _ := newFilePublishNode(t, newMockMounter())
	resp, err := fileNode.NodeGetCapabilities(context.Background(), &csipb.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("file driver NodeGetCapabilities: %v", err)
	}
	got := map[csipb.NodeServiceCapability_RPC_Type]bool{}
	for _, capability := range resp.GetCapabilities() {
		got[capability.GetRpc().GetType()] = true
	}
	want := map[csipb.NodeServiceCapability_RPC_Type]bool{
		csipb.NodeServiceCapability_RPC_GET_VOLUME_STATS: true,
	}
	if !slices.EqualFunc(
		slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)),
		func(a, b csipb.NodeServiceCapability_RPC_Type) bool { return a == b },
	) {
		t.Fatalf("file driver capabilities = %v, want exactly %v", got, want)
	}
	if got[csipb.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME] {
		t.Fatal("file driver must not advertise STAGE_UNSTAGE_VOLUME: kubelet would stage a globalmount")
	}
	if got[csipb.NodeServiceCapability_RPC_EXPAND_VOLUME] {
		t.Fatal("file driver must not advertise EXPAND_VOLUME: adopted quota is fixed")
	}

	legacy := newNodeTestEnv(t)
	legacyResp, err := legacy.srv.NodeGetCapabilities(context.Background(), &csipb.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("legacy driver NodeGetCapabilities: %v", err)
	}
	legacyCaps := map[csipb.NodeServiceCapability_RPC_Type]bool{}
	for _, capability := range legacyResp.GetCapabilities() {
		legacyCaps[capability.GetRpc().GetType()] = true
	}
	for _, capability := range []csipb.NodeServiceCapability_RPC_Type{
		csipb.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csipb.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csipb.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	} {
		if !legacyCaps[capability] {
			t.Errorf("legacy driver lost capability %v", capability)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Stage/Unstage/Expand — honest Unimplemented on the file profile
// ─────────────────────────────────────────────────────────────────────────────

// TestFileNodeStageLifecycleUnimplemented pins kubelet's NoStage flow: a file
// node must never accept a stage RPC — a staging path it mounted would still
// appear in kubelet's GetDeviceMountRefs teardown walk. Unimplemented is
// returned before any request-shape validation and leaves no state behind.
func TestFileNodeStageLifecycleUnimplemented(t *testing.T) {
	t.Parallel()

	node, stateDir := newFilePublishNode(t, newMockMounter())
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"NodeStageVolume": func() error {
			_, err := node.NodeStageVolume(ctx, &csipb.NodeStageVolumeRequest{
				VolumeId:          filePublishVolumeID,
				StagingTargetPath: filepath.Join(t.TempDir(), "globalmount"),
				VolumeCapability:  mountCap(""),
				PublishContext: fileRemotePublishContext(
					fileDirectoryAdoption("/existing/data", "ext4", 42),
					`{"directory":{"logicalPool":"pool","hostRoot":"/existing"}}`),
			})
			return err
		},
		"NodeUnstageVolume": func() error {
			_, err := node.NodeUnstageVolume(ctx, &csipb.NodeUnstageVolumeRequest{
				VolumeId:          filePublishVolumeID,
				StagingTargetPath: filepath.Join(t.TempDir(), "globalmount"),
			})
			return err
		},
		"NodeExpandVolume": func() error {
			_, err := node.NodeExpandVolume(ctx, &csipb.NodeExpandVolumeRequest{
				VolumeId:   filePublishVolumeID,
				VolumePath: filepath.Join(t.TempDir(), "pod-target"),
			})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if status.Code(err) != codes.Unimplemented {
				t.Fatalf("file driver %s = %v, want Unimplemented", name, err)
			}
		})
	}
	assertFileRecordAbsent(t, node, stateDir, filePublishVolumeID)
}

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume — request and context validation
// ─────────────────────────────────────────────────────────────────────────────

// TestFileNodePublishVolume_RequestValidation pins the input contract: the
// file profile requires volume_id, target_path and a mount capability — but
// an empty staging_target_path is legal because the profile never stages.
func TestFileNodePublishVolume_RequestValidation(t *testing.T) {
	t.Parallel()

	node, stateDir := newFilePublishNode(t, newMockMounter())
	target := filepath.Join(t.TempDir(), "pod-target")
	base := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	base.VolumeContext = fileRemoteVolumeContext()

	for name, mutate := range map[string]func(*csipb.NodePublishVolumeRequest){
		"missing volume_id":   func(r *csipb.NodePublishVolumeRequest) { r.VolumeId = "" },
		"missing target_path": func(r *csipb.NodePublishVolumeRequest) { r.TargetPath = "" },
		"missing capability":  func(r *csipb.NodePublishVolumeRequest) { r.VolumeCapability = nil },
		"block access":        func(r *csipb.NodePublishVolumeRequest) { r.VolumeCapability = blockCap() },
		"bad mount options": func(r *csipb.NodePublishVolumeRequest) {
			r.VolumeContext[paramMountOptions] = "not-json"
		},
		"structural nfs option conflict": func(r *csipb.NodePublishVolumeRequest) {
			r.VolumeContext[paramMountOptions] = `["soft"]`
		},
		"formatting fs-type": func(r *csipb.NodePublishVolumeRequest) {
			r.VolumeContext[paramFSType] = "xfs"
		},
		"mkfs options": func(r *csipb.NodePublishVolumeRequest) {
			r.VolumeContext[paramMkfsOptions] = `["-E","nodiscard"]`
		},
		"periodic trim": func(r *csipb.NodePublishVolumeRequest) {
			r.VolumeContext[paramPeriodicTrim] = "true"
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := clonePublishRequest(t, base)
			mutate(req)
			_, err := node.NodePublishVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("NodePublishVolume %s = %v, want InvalidArgument", name, err)
			}
			if mounted, probeErr := mountEntryExists(node, target); probeErr != nil || mounted {
				t.Fatalf("%s left %q mounted=%v err=%v", name, target, mounted, probeErr)
			}
		})
	}
	assertFileRecordAbsent(t, node, stateDir, filePublishVolumeID)
}

// TestFileNodePublishVolume_AdoptionContextRequired pins the trust boundary:
// the file profile serves adopted filesystems only, and only the
// controller-written PublishContext may supply identity or routing. A
// VolumeContext alone never admits a publish.
func TestFileNodePublishVolume_AdoptionContextRequired(t *testing.T) {
	t.Parallel()

	node, stateDir := newFilePublishNode(t, newMockMounter())
	target := filepath.Join(t.TempDir(), "pod-target")

	for name, mutate := range map[string]func(*csipb.NodePublishVolumeRequest){
		"no adoption anywhere": func(r *csipb.NodePublishVolumeRequest) {
			r.PublishContext = nil
		},
		"driver scoped to another csi driver": func(r *csipb.NodePublishVolumeRequest) {
			r.VolumeContext[VolumeContextKeyCSIDriver] = "other-csi.example.com"
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := fileRemotePublishRequest(filePublishVolumeID+name, target+name, mountCap(ProtocolNFS))
			req.VolumeContext = fileRemoteVolumeContext()
			mutate(req)
			_, err := node.NodePublishVolume(context.Background(), req)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("NodePublishVolume %s = %v, want FailedPrecondition", name, err)
			}
			if mounted, probeErr := mountEntryExists(node, target+name); probeErr != nil || mounted {
				t.Fatalf("%s left %q mounted=%v err=%v", name, target+name, mounted, probeErr)
			}
		})
	}
	assertFileRecordAbsent(t, node, stateDir, filePublishVolumeID+"no adoption anywhere")
}

// TestLegacyNodePublishVolume_StillRequiresStaging is the healthy legacy
// control for the NoStage cutover: the block profile keeps advertising
// STAGE_UNSTAGE_VOLUME, so a publish without a staging path is still a
// malformed request there and nothing is mounted.
func TestLegacyNodePublishVolume_StillRequiresStaging(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	target := filepath.Join(t.TempDir(), "pod-target")
	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	_, err := env.srv.NodePublishVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("block driver publish without staging path = %v, want InvalidArgument", err)
	}
	if mounted, _ := env.mounter.MountEntryExists(target); mounted { //nolint:errcheck // mock never errors
		t.Fatalf("block driver mounted %q without a stage", target)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume — remote NFS route (mock mounter, no privileges needed)
// ─────────────────────────────────────────────────────────────────────────────

// mountEntryExists is a small readability shim over the Mounter interface so
// assertions read the same against the mock and the production mounter.
func mountEntryExists(node *NodeServer, target string) (bool, error) {
	return node.mounter.MountEntryExists(target)
}

// TestFileNodePublishVolume_RemoteNFSMount verifies that a remote consumer's
// publish mounts the controller-advertised export directly at the pod target
// with the enforced hard-mount NFS contract — no staging path is consulted.
func TestFileNodePublishVolume_RemoteNFSMount(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("remote NodePublishVolume: %v", err)
	}

	if got := len(mounter.mountCalls); got != 1 {
		t.Fatalf("mount calls = %d, want one direct target mount", got)
	}
	call := mounter.mountCalls[0]
	if call.source != "192.0.2.10:/export/native" {
		t.Errorf("mount source = %q, want 192.0.2.10:/export/native", call.source)
	}
	if call.target != target {
		t.Errorf("mount target = %q, want pod target %q (never a staging path)", call.target, target)
	}
	for _, opt := range []string{"hard", "nfsvers=4.2", "proto=tcp", "port=2049"} {
		if !slices.Contains(call.options, opt) {
			t.Errorf("NFS mount options %v missing required %q", call.options, opt)
		}
	}
	if slices.Contains(call.options, "ro") {
		t.Errorf("rw publish mounted read-only: %v", call.options)
	}

	state := assertFileRecordPresent(t, node, stateDir)
	if state.File.CanonicalSource != "/existing/data" || state.File.ResourceID != "uuid:42" ||
		state.File.CapacityBytes != 1048576 {
		t.Fatalf("record does not preserve the adopted native identity/quota: %+v", state.File)
	}
	if state.NFS == nil || state.NFS.MountSource != "192.0.2.10:/export/native" {
		t.Fatalf("record lacks the durable NFS mount source: %+v", state.NFS)
	}
	if mounted, err := mountEntryExists(node, target); err != nil || !mounted {
		t.Fatalf("pod target mounted=%v err=%v, want mounted", mounted, err)
	}
}

// TestFileNodePublishVolume_RemoteNFSReadonly verifies a read-only publish is
// recorded and mounted read-only while sharing the volume with rw peers.
func TestFileNodePublishVolume_RemoteNFSReadonly(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, _ := newFilePublishNode(t, mounter)
	root := t.TempDir()
	rwTarget := filepath.Join(root, "rw")
	roTarget := filepath.Join(root, "ro")

	rw := fileRemotePublishRequest(filePublishVolumeID, rwTarget, mountCap(ProtocolNFS))
	rw.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), rw); err != nil {
		t.Fatalf("rw publish: %v", err)
	}
	ro := fileRemotePublishRequest(filePublishVolumeID, roTarget, mountCap(ProtocolNFS))
	ro.VolumeContext = fileRemoteVolumeContext()
	ro.Readonly = true
	if _, err := node.NodePublishVolume(context.Background(), ro); err != nil {
		t.Fatalf("ro publish: %v", err)
	}

	// Identical ro republish is idempotent: no new mount, recorded flag kept.
	roIdentical := clonePublishRequest(t, ro)
	if _, err := node.NodePublishVolume(context.Background(), roIdentical); err != nil {
		t.Fatalf("identical ro republish: %v", err)
	}
	if len(mounter.mountCalls) != 2 {
		t.Fatalf("ro republish issued %d extra mounts", len(mounter.mountCalls)-2)
	}
	if !mounter.mountRO[roTarget] || mounter.mountRO[rwTarget] {
		t.Fatalf("ro republish changed mount flags: ro=%v rw=%v", mounter.mountRO[roTarget], mounter.mountRO[rwTarget])
	}

	if len(mounter.mountCalls) != 2 {
		t.Fatalf("mount calls = %d, want one per target", len(mounter.mountCalls))
	}
	if !slices.Contains(mounter.mountCalls[1].options, "ro") {
		t.Errorf("ro publish options %v missing \"ro\"", mounter.mountCalls[1].options)
	}

	// Re-publishing the recorded ro target as rw must fail: the durable record
	// is the authority on per-target readonly, not the retry's flag.
	ro.Readonly = false
	if _, err := node.NodePublishVolume(context.Background(), ro); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ro target republished rw = %v, want FailedPrecondition", err)
	}
	if mounted, err := mountEntryExists(node, roTarget); err != nil || !mounted {
		t.Fatalf("ro target mounted=%v err=%v after rejected rw retry", mounted, err)
	}
}

// TestFileNodePublishVolume_RemoteNFSSourceDriftRejected pins mount-source
// durability: once a publish records its NFS export, a retry that resolves to
// a different source must be refused rather than silently repointed.
func TestFileNodePublishVolume_RemoteNFSSourceDriftRejected(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, _ := newFilePublishNode(t, mounter)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	republish := clonePublishRequest(t, req)
	for name, vc := range map[string]map[string]string{
		"server moved":   withNFSVolumeContext(fileRemoteVolumeContext(), VolumeContextKeyAddress, "192.0.2.11"),
		"export changed": withNFSVolumeContext(fileRemoteVolumeContext(), vcVolumeRef, "/export/other"),
	} {
		t.Run(name, func(t *testing.T) {
			republish.VolumeContext = vc
			if _, err := node.NodePublishVolume(context.Background(), republish); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("%s republish = %v, want FailedPrecondition", name, err)
			}
			if mounted, err := mountEntryExists(node, target); err != nil || !mounted {
				t.Fatalf("recorded target lost after refused %s republish", name)
			}
		})
	}
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("refused republish issued %d extra mounts", len(mounter.mountCalls)-1)
	}
}

// TestFileNodePublishVolume_MountIdentityDriftRefused pins the observation
// seam: an existing mount at a requested target is adopted only when the
// kernel-visible facts (filesystem type, export source, read-only flag)
// still match the durable record.  Any drift is FailedPrecondition, leaves
// the mount untouched and never rewrites the record.
func TestFileNodePublishVolume_MountIdentityDriftRefused(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}
	recordPath := filePublishStateFilePath(stateDir, filePublishVolumeID)
	recordBefore, err := os.ReadFile(recordPath) //nolint:gosec // G304: path under the test's own t.TempDir state dir
	if err != nil {
		t.Fatalf("read record: %v", err)
	}

	republish := clonePublishRequest(t, req)
	for name, tamper := range map[string]func(){
		"read-only drift": func() { mounter.mountRO[target] = true },
		"filesystem type drift": func() {
			setRecordedMountFsType(mounter, target, "ext4")
		},
		"export source drift": func() { mounter.mountSource[target] = "192.0.2.10:/export/foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			// Each case starts from the healthy publish and restores it, so a
			// refusal can only come from this case's single drift.
			defer func() {
				mounter.mountRO[target] = false
				mounter.mountSource[target] = "192.0.2.10:/export/native"
				setRecordedMountFsType(mounter, target, "nfs")
			}()
			tamper()
			assertMountDriftRefused(t, node, mounter, republish, name)
		})
	}

	recordAfter, err := os.ReadFile(recordPath) //nolint:gosec // G304: path under the test's own t.TempDir state dir
	if err != nil || !bytes.Equal(recordAfter, recordBefore) {
		t.Fatalf("drift refusals rewrote the record: %v\nbefore=%s\nafter=%s", err, recordBefore, recordAfter)
	}
	if len(mounter.unmountCalls) != 0 || len(mounter.mountCalls) != 1 {
		t.Fatalf("drift refusals changed mounts: mounts=%v unmounts=%v", mounter.mountCalls, mounter.unmountCalls)
	}
}

// setRecordedMountFsType rewrites the filesystem type the mock reports for
// target's mounts, modeling a mount-table entry of a different type.
func setRecordedMountFsType(mounter *mockMounter, target, fsType string) {
	for i := range mounter.mountCalls {
		if mounter.mountCalls[i].target == target {
			mounter.mountCalls[i].fsType = fsType
		}
	}
}

// assertMountDriftRefused checks that both republish and unpublish refuse a
// recorded target whose observed mount no longer matches the record, and
// that the mount survives the refusal.
func assertMountDriftRefused(
	t *testing.T, node *NodeServer, mounter *mockMounter, republish *csipb.NodePublishVolumeRequest, drift string,
) {
	t.Helper()
	target := republish.GetTargetPath()
	_, err := node.NodePublishVolume(context.Background(), republish)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("republish over %s = %v, want FailedPrecondition", drift, err)
	}
	// Unpublish refuses too: the mount is recorded but no longer provably
	// this volume's publish.
	_, err = node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: republish.GetVolumeId(), TargetPath: target,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unpublish over %s = %v, want FailedPrecondition", drift, err)
	}
	if !mounter.mountedPaths[target] {
		t.Fatalf("mount removed after %s refusal", drift)
	}
}

// intentWitnessMounter proves the ordering claim the crash test depends on:
// every Mount for a file publish must find the target already recorded in the
// durable record.  A mount(2) racing ahead of the intent write is the defect
// that left orphan mounts ununpublishable; this mock fails the mount when the
// record does not already name the target.  The record is read through its
// JSON wire form ("file"."targets"[]."target_path") so the witness compiles
// against any record schema version.
type intentWitnessMounter struct {
	*mockMounter
	recordPath string
}

func (m *intentWitnessMounter) Mount(source, target, fsType string, options []string) error {
	data, err := os.ReadFile(m.recordPath)
	if err != nil {
		return fmt.Errorf("mount %q before durable intent was recorded: %w", target, err)
	}
	var record struct {
		File struct {
			Targets []struct {
				TargetPath string `json:"target_path"`
			} `json:"targets"`
		} `json:"file"`
	}
	if err = json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("mount %q: unreadable intent record: %w", target, err)
	}
	for _, entry := range record.File.Targets {
		if filepath.Clean(entry.TargetPath) == filepath.Clean(target) {
			return m.mockMounter.Mount(source, target, fsType, options)
		}
	}
	return fmt.Errorf("mount %q before durable intent was recorded", target)
}

// TestFileNodePublishVolume_IntentRecoveryAfterCrash exercises the durable
// intent contract end to end.  The witness mounter proves Mount is only ever
// called after the target is already persisted in the record (the ordering a
// crash-safe publish needs).  The crash itself is modeled faithfully: the
// record is seeded with a target entry the process died before mounting —
// writeFilePublishRecord writes the exact wire shape publish persists — and a
// fresh NodeServer over the same state dir performs the recovery, the same
// way a restarted node would.  Both directions converge: unpublish drops the
// intent without an unmount call, publish mounts it for real.
func TestFileNodePublishVolume_IntentRecoveryAfterCrash(t *testing.T) {
	t.Parallel()

	mounter := &intentWitnessMounter{mockMounter: newMockMounter()}
	node, stateDir := newFilePublishNode(t, mounter)
	mounter.recordPath = filePublishStateFilePath(stateDir, filePublishVolumeID)
	root := t.TempDir()
	peerTarget := filepath.Join(root, "peer-target")
	intentOnly := filepath.Join(root, "intent-only-target")

	// Publish one real target: the witness fails the mount if the durable
	// record does not already name it, so a successful publish proves the
	// ordering claim rather than assuming it.
	peerReq := fileRemotePublishRequest(filePublishVolumeID, peerTarget, mountCap(ProtocolNFS))
	peerReq.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), peerReq); err != nil {
		t.Fatalf("witnessed publish: %v", err)
	}

	// Crash window: intentOnly was appended to the record but mount(2) never
	// ran — seed exactly that wire state, then restart.
	intentState := &FileStageState{VolumeID: filePublishVolumeID,
		CanonicalSource: "/existing/data", ResourceID: "uuid:42", FilesystemType: "ext4",
		FilesystemID: "uuid", Inode: 42, ProjectID: 7, BackendType: "directory",
		PoolName: "pool", ExpectedHostRoot: "/existing", CapacityBytes: 1048576,
		AgentName: "agent", AgentEndpoint: "agent:9500", AgentVolumeID: "pool/native"}
	writeFilePublishRecord(t, node, intentState, "192.0.2.10:/export/native", peerTarget, intentOnly)

	restarted := NewNodeServer("storage-node", nil, mounter).
		WithDriverName(v1alpha1.FileCSIDriver).WithStateDir(stateDir)

	// Unpublish of the never-mounted intent: succeeds, no unmount call, and
	// the peer's record entry survives.
	mounter.unmountCalls = nil
	if _, err := restarted.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: intentOnly,
	}); err != nil {
		t.Fatalf("unpublish of unmounted recorded intent: %v", err)
	}
	if len(mounter.unmountCalls) != 0 {
		t.Fatalf("unpublish of unmounted intent called unmount: %v", mounter.unmountCalls)
	}
	assertFileRecordPresent(t, restarted, stateDir)
	if mounted, err := mountEntryExists(node, peerTarget); err != nil || !mounted {
		t.Fatalf("peer mount lost while dropping stale intent: %v %v", mounted, err)
	}

	// The same intent state republished mounts the target for real and the
	// record still serves both flows afterwards.
	intentReq := fileRemotePublishRequest(filePublishVolumeID, intentOnly, mountCap(ProtocolNFS))
	intentReq.VolumeContext = fileRemoteVolumeContext()
	if _, err := restarted.NodePublishVolume(context.Background(), intentReq); err != nil {
		t.Fatalf("publish of recorded-but-unmounted intent: %v", err)
	}
	if mounted, err := mountEntryExists(node, intentOnly); err != nil || !mounted {
		t.Fatalf("recovered intent not mounted: %v %v", mounted, err)
	}

	// Full teardown leaves no residue.
	for _, target := range []string{intentOnly, peerTarget} {
		if _, err := restarted.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
			VolumeId: filePublishVolumeID, TargetPath: target,
		}); err != nil {
			t.Fatalf("unpublish %q: %v", target, err)
		}
	}
	assertFileRecordAbsent(t, restarted, stateDir, filePublishVolumeID)
}

func withNFSVolumeContext(base map[string]string, key, value string) map[string]string {
	vc := make(map[string]string, len(base))
	maps.Copy(vc, base)
	vc[key] = value
	return vc
}

// TestFileNodePublishVolume_AdoptionDriftRejected pins descriptor
// immutability: a republish whose adoption JSON, capacity or routing differs
// from the recorded lifecycle must fail closed, preserving mount and record.
func TestFileNodePublishVolume_AdoptionDriftRejected(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	before := assertFileRecordPresent(t, node, stateDir)

	for name, mutate := range map[string]func(*csipb.NodePublishVolumeRequest){
		"replacement native identity": func(r *csipb.NodePublishVolumeRequest) {
			r.PublishContext[PublishContextKeyFilesystemAdoption] =
				fileDirectoryAdoption("/existing/data", "ext4", 43)
		},
		"coherent identity replacement": func(r *csipb.NodePublishVolumeRequest) {
			data, err := json.Marshal(filesystemContextAdoption{
				Kind: "directory", CanonicalSource: "/existing/data",
				ResourceID: "uuid:43", FilesystemType: "ext4", FilesystemID: "uuid",
				Inode: 43, ProjectID: 7,
			})
			if err != nil {
				panic(err)
			}
			r.PublishContext[PublishContextKeyFilesystemAdoption] = string(data)
		},
		"admitted quota changed": func(r *csipb.NodePublishVolumeRequest) {
			r.PublishContext[PublishContextKeyFilesystemCapacity] = "1048577"
		},
		"backend layout broadened": func(r *csipb.NodePublishVolumeRequest) {
			r.PublishContext[PublishContextKeyFilesystemLayout] =
				`{"directory":{"logicalPool":"pool","hostRoot":"/"}}`
		},
		"agent volume rerouted": func(r *csipb.NodePublishVolumeRequest) {
			r.PublishContext[fileContextAgentVolumeID] = "pool/other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			drifted := clonePublishRequest(t, req)
			mutate(drifted)
			if _, err := node.NodePublishVolume(context.Background(), drifted); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("%s republish = %v, want FailedPrecondition", name, err)
			}
			if mounted, err := mountEntryExists(node, target); err != nil || !mounted {
				t.Fatalf("target unmounted after refused %s republish", name)
			}
		})
	}
	after := assertFileRecordPresent(t, node, stateDir)
	if after.File.CanonicalSource != before.File.CanonicalSource ||
		after.File.ResourceID != before.File.ResourceID ||
		after.File.Inode != before.File.Inode ||
		after.File.CapacityBytes != before.File.CapacityBytes ||
		after.File.ExpectedHostRoot != before.File.ExpectedHostRoot ||
		after.File.AgentVolumeID != before.File.AgentVolumeID {
		t.Fatalf("refused republish rewrote durable identity: before=%+v after=%+v", before.File, after.File)
	}
}

// TestFileNodePublishVolume_UntrustedVolumeContextCannotRoute reproduces the
// trust boundary the old stage-context test pinned — now observed through the
// public publish RPC and the stats reader the record feeds. Untrusted PV
// user-data must never select the agent endpoint, name or volume used for
// identity revalidation.
func TestFileNodePublishVolume_UntrustedVolumeContextCannotRoute(t *testing.T) {
	t.Parallel()

	var got *FileStageState
	statsReader := func(_ context.Context, _ string, state *FileStageState) (*csipb.NodeGetVolumeStatsResponse, error) {
		got = state
		return &csipb.NodeGetVolumeStatsResponse{Usage: []*csipb.VolumeUsage{
			{Unit: csipb.VolumeUsage_BYTES, Total: state.CapacityBytes},
		}}, nil
	}
	mounter := newMockMounter()
	stateDir := t.TempDir()
	node := NewNodeServer("consumer", nil, mounter).WithStateDir(stateDir).
		WithDriverName(v1alpha1.FileCSIDriver).WithFilesystemStatsReader(statsReader)

	target := filepath.Join(t.TempDir(), "pod-target")
	req := fileRemotePublishRequest("agent/nfs/directory/pool/native/lifecycle", target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	req.VolumeContext[fileContextAgentAddress] = "attacker:9500"
	req.VolumeContext[fileContextAgentName] = "attacker"
	req.VolumeContext[fileContextAgentVolumeID] = "attacker/alias"
	req.VolumeContext[VolumeContextKeyFilesystemAdoption] =
		`{"kind":"directory","canonicalSource":"/wrong","resourceId":"wrong","filesystemType":"xfs"}`
	req.VolumeContext[VolumeContextKeyFilesystemCapacity] = "999"
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish with attacker VolumeContext: %v", err)
	}
	if _, err := node.NodeGetVolumeStats(context.Background(), &csipb.NodeGetVolumeStatsRequest{
		VolumeId: req.VolumeId, VolumePath: target,
	}); err != nil {
		t.Fatalf("stats after publish: %v", err)
	}
	if got == nil || got.AgentEndpoint != "storage-node:9500" || got.AgentName != "storage-node" ||
		got.AgentVolumeID != "pool/native" || got.CanonicalSource != "/existing/data" ||
		got.CapacityBytes != 1048576 || got.PoolName != "pool" || got.ExpectedHostRoot != "/existing" {
		t.Fatalf("untrusted VolumeContext changed admitted identity, scope or routing: %#v", got)
	}

	// A publish context without an agent endpoint must stay endpoint-less:
	// VolumeContext may not smuggle one in for the durable record.
	target2 := target + "-second"
	req2 := fileRemotePublishRequest(req.VolumeId+"/second", target2, mountCap(ProtocolNFS))
	req2.VolumeContext = fileRemoteVolumeContext()
	req2.VolumeContext[fileContextAgentAddress] = "attacker:9500"
	delete(req2.PublishContext, fileContextAgentAddress)
	if _, err := node.NodePublishVolume(context.Background(), req2); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if _, err := node.NodeGetVolumeStats(context.Background(), &csipb.NodeGetVolumeStatsRequest{
		VolumeId: req2.VolumeId, VolumePath: target2,
	}); err != nil {
		t.Fatalf("stats on second publish: %v", err)
	}
	if got.AgentEndpoint != "" {
		t.Fatalf("PV userdata selected agent endpoint %q", got.AgentEndpoint)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeUnpublishVolume — per-target teardown and last-target record cleanup
// ─────────────────────────────────────────────────────────────────────────────

// TestFileNodeUnpublishVolume_OnePeerRetained verifies each unpublish removes
// only its own target: the peer's direct mount, the durable record and the
// source stay live until the last target goes away.
func TestFileNodeUnpublishVolume_OnePeerRetained(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	root := t.TempDir()
	targetA := filepath.Join(root, "pod-a")
	targetB := filepath.Join(root, "pod-b")

	for _, target := range []string{targetA, targetB} {
		req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
		req.VolumeContext = fileRemoteVolumeContext()
		if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
			t.Fatalf("publish %q: %v", target, err)
		}
	}
	assertFileRecordPresent(t, node, stateDir)

	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: targetA,
	}); err != nil {
		t.Fatalf("unpublish peer A: %v", err)
	}
	if mounted, err := mountEntryExists(node, targetA); err != nil || mounted {
		t.Fatalf("unpublished target still mounted=%v err=%v", mounted, err)
	}
	if mounted, err := mountEntryExists(node, targetB); err != nil || !mounted {
		t.Fatalf("peer target lost its mount: mounted=%v err=%v", mounted, err)
	}
	assertFileRecordPresent(t, node, stateDir)

	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: targetB,
	}); err != nil {
		t.Fatalf("unpublish last target: %v", err)
	}
	assertFileRecordAbsent(t, node, stateDir, filePublishVolumeID)

	// The source is never node-owned: unpublish must not unmount anything but
	// its own recorded target.
	if len(mounter.unmountCalls) != 2 {
		t.Fatalf("unmount calls = %v, want exactly the two pod targets", mounter.unmountCalls)
	}
}

// TestFileNodeUnpublishVolume_Idempotent verifies the CSI §5.4.2 contract on
// the direct-publish profile: unpublish of a never-published, already-removed
// or foreign target succeeds without touching other mounts or the record.
func TestFileNodeUnpublishVolume_Idempotent(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	target := filepath.Join(t.TempDir(), "pod-target")

	// Unknown volume: succeed silently.
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: target,
	}); err != nil {
		t.Fatalf("unpublish unknown volume: %v", err)
	}

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: target,
	}); err != nil {
		t.Fatalf("first unpublish: %v", err)
	}
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: target,
	}); err != nil {
		t.Fatalf("repeat unpublish of removed target: %v", err)
	}
	assertFileRecordAbsent(t, node, stateDir, filePublishVolumeID)
}

// TestFileNodeUnpublishVolume_UnmountFailurePreservesState verifies a real
// unmount failure surfaces as Internal for kubelet retry while the recorded
// target and its peer mounts stay registered for the next attempt.
func TestFileNodeUnpublishVolume_UnmountFailurePreservesState(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	root := t.TempDir()
	targetA, targetB := filepath.Join(root, "a"), filepath.Join(root, "b")

	for _, target := range []string{targetA, targetB} {
		req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
		req.VolumeContext = fileRemoteVolumeContext()
		if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
			t.Fatalf("publish %q: %v", target, err)
		}
	}

	mounter.unmountErr = errors.New("umount: target is busy")
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: targetA,
	}); status.Code(err) != codes.Internal {
		t.Fatalf("failed unpublish = %v, want Internal for kubelet retry", err)
	}
	mounter.unmountErr = nil

	if mounted, err := mountEntryExists(node, targetA); err != nil || !mounted {
		t.Fatalf("failed-unmount target mounted=%v err=%v, want still mounted", mounted, err)
	}
	state := assertFileRecordPresent(t, node, stateDir)
	if state == nil {
		t.Fatal("failed unpublish dropped the durable record")
	}

	// The preserved record makes the retry converge.
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: targetA,
	}); err != nil {
		t.Fatalf("retry unpublish: %v", err)
	}
	if mounted, err := mountEntryExists(node, targetA); err != nil || mounted {
		t.Fatalf("retried target still mounted=%v err=%v", mounted, err)
	}
	if mounted, err := mountEntryExists(node, targetB); err != nil || !mounted {
		t.Fatalf("peer target lost after failed+retried unpublish: mounted=%v", mounted)
	}
}

// foreignMountMounter injects a mount entry the node never created: the
// target is occupied by an unrelated filesystem (another volume's mount,
// hand-mounted host path) which unpublish must refuse to tear down.
type foreignMountMounter struct {
	*mockMounter
	foreignMounts map[string]string // target → foreign source
}

func (m *foreignMountMounter) MountEntryExists(target string) (bool, error) {
	if _, ok := m.foreignMounts[target]; ok {
		return true, nil
	}
	return m.mockMounter.MountEntryExists(target)
}

func (m *foreignMountMounter) MountSource(target string) (string, error) {
	if source, ok := m.foreignMounts[target]; ok {
		return source, nil
	}
	return m.mockMounter.MountSource(target)
}

func (m *foreignMountMounter) ObserveMount(target string) (MountObservation, error) {
	if source, ok := m.foreignMounts[target]; ok {
		return MountObservation{Source: source, FsType: "nfs4"}, nil
	}
	return m.mockMounter.ObserveMount(target)
}

// observeErrorMounter models a mount-table read failure on a target that
// still holds its mount entry: MountEntryExists answers true while
// ObserveMount returns no facts — the all-or-nothing KubeMounter shape after
// the statfs probe was dropped.
type observeErrorMounter struct {
	*mockMounter
	blind map[string]bool // target -> ObserveMount fails
}

func (m *observeErrorMounter) ObserveMount(target string) (MountObservation, error) {
	if m.blind[target] {
		return MountObservation{}, fmt.Errorf("read mount entry %q: %w", target, syscall.EIO)
	}
	return m.mockMounter.ObserveMount(target)
}

// TestFileNodeUnpublishVolume_ObserveFailure pins unpublish when the mount at
// a recorded target cannot be observed: an observation error keeps the mount
// and the record (Internal), and an observed mount whose facts contradict the
// record refuses with FailedPrecondition.
func TestFileNodeUnpublishVolume_ObserveFailure(t *testing.T) {
	t.Parallel()

	mounter := &observeErrorMounter{mockMounter: newMockMounter(), blind: map[string]bool{}}
	node, stateDir := newFilePublishNode(t, mounter)
	root := t.TempDir()
	targetDead := filepath.Join(root, "unobservable-mount")
	targetDrift := filepath.Join(root, "drifted-mount")
	for _, target := range []string{targetDead, targetDrift} {
		req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
		req.VolumeContext = fileRemoteVolumeContext()
		if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
			t.Fatalf("publish %q: %v", target, err)
		}
	}
	mounter.blind[targetDead] = true
	mounter.mountSource[targetDrift] = "10.9.9.9:/export/foreign"

	// No mount-table facts at all → Internal, mount and record survive.
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: targetDead,
	}); status.Code(err) != codes.Internal {
		t.Fatalf("unpublish of unobservable mount = %v, want Internal", err)
	}
	if !mounter.mountedPaths[targetDead] {
		t.Fatal("unobservable mount was torn down")
	}

	// Observed facts that contradict the record → FailedPrecondition.
	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: targetDrift,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unpublish of drifted mount = %v, want FailedPrecondition", err)
	}
	if !mounter.mountedPaths[targetDrift] {
		t.Fatal("drifted mount was torn down")
	}

	assertFileRecordPresent(t, node, stateDir)
	if len(mounter.unmountCalls) != 0 {
		t.Fatalf("unmount attempted on unverified mounts: %v", mounter.unmountCalls)
	}
}

// corruptFileRecordNFS rewrites the durable record on disk to drop or blank
// the "nfs" mount-source entry — wire surgery on the exact format publish
// persists, modeling a record whose export identity was lost.
func corruptFileRecordNFS(t *testing.T, node *NodeServer, volumeID string, corrupt func(map[string]any)) []byte {
	t.Helper()
	path := node.stateFilePath(volumeID)
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled state dir
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	corrupt(record)
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The corrupted bytes are the reference: a refused republish must not
	// rewrite, repair or normalize the record.
	return data
}

// TestFileNodePublishVolume_CorruptRecordFailsClosed pins that a publish
// never backfills a remote record that lost its NFS export identity: the
// retry is FailedPrecondition before any mount, the record bytes and the
// actual mount are untouched, and no mount or unmount is attempted.
func TestFileNodePublishVolume_CorruptRecordFailsClosed(t *testing.T) {
	t.Parallel()

	for name, corrupt := range map[string]func(map[string]any){
		"missing nfs section": func(record map[string]any) { delete(record, "nfs") },
		"blank mount source": func(record map[string]any) {
			record["nfs"] = map[string]any{"mount_source": ""}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mounter := newMockMounter()
			node, _ := newFilePublishNode(t, mounter)
			target := filepath.Join(t.TempDir(), "pod-target")

			req := fileRemotePublishRequest(filePublishVolumeID+name, target, mountCap(ProtocolNFS))
			req.VolumeContext = fileRemoteVolumeContext()
			if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
				t.Fatalf("publish: %v", err)
			}
			// corruptFileRecordNFS returns the corrupted bytes; a refused
			// republish must leave them untouched.
			corrupted := corruptFileRecordNFS(t, node, filePublishVolumeID+name, corrupt)

			if _, err := node.NodePublishVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("republish over %s record = %v, want FailedPrecondition", name, err)
			}
			after, err := os.ReadFile(node.stateFilePath(filePublishVolumeID + name))
			if err != nil || !bytes.Equal(after, corrupted) {
				t.Fatalf("refused republish rewrote a corrupt record: %v\ncorrupt=%s\nafter=%s", err, corrupted, after)
			}
			if !mounter.mountedPaths[target] {
				t.Fatal("refused republish tore down the real mount")
			}
			if len(mounter.mountCalls) != 1 || len(mounter.unmountCalls) != 0 {
				t.Fatalf("refused republish touched mounts: mounts=%v unmounts=%v", mounter.mountCalls, mounter.unmountCalls)
			}
		})
	}
}

// TestFileNodePublishVolume_StagedRecordFailsClosed pins the retired staging
// contract: a record left by the old stage flow (staging_path set) is never
// converted, extended or torn down by the direct-publish RPCs — every file
// RPC on it fails closed and the bytes stay exactly as written.
func TestFileNodePublishVolume_StagedRecordFailsClosed(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	volumeID := "agent/nfs/directory/pool/native/staged-legacy"
	target := filepath.Join(t.TempDir(), "pod-target")

	legacy := fmt.Sprintf(`{"protocol_type":"nfs","access_type":"filesystem","volume_id":%q,`+
		`"staging_path":"/var/lib/kubelet/plugins/kubernetes.io/csi/pv/staged/globalmount","file":`+
		`{"proxy_path":"/agent/proxy","canonical_source":"/existing/data","resource_id":"uuid:42",`+
		`"filesystem_type":"ext4","capacity_bytes":1048576,"local":true}}`, volumeID)
	recordPath := filePublishStateFilePath(stateDir, volumeID)
	if err := os.WriteFile(recordPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() error{
		"NodePublishVolume": func() error {
			req := fileRemotePublishRequest(volumeID, target, mountCap(ProtocolNFS))
			req.VolumeContext = fileRemoteVolumeContext()
			_, err := node.NodePublishVolume(context.Background(), req)
			return err
		},
		"NodeUnpublishVolume": func() error {
			_, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
				VolumeId: volumeID, TargetPath: target,
			})
			return err
		},
		"NodeGetVolumeStats": func() error {
			_, err := node.NodeGetVolumeStats(context.Background(), &csipb.NodeGetVolumeStatsRequest{
				VolumeId: volumeID, VolumePath: target,
			})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("%s on a staged record = %v, want FailedPrecondition", name, err)
			}
			if !strings.Contains(status.Convert(err).Message(), "staged filesystem record") {
				t.Fatalf("%s error %q missing the staged-record marker", name, err)
			}
		})
	}

	after, err := os.ReadFile(recordPath) //nolint:gosec // test state dir
	if err != nil || string(after) != legacy {
		t.Fatalf("staged record touched: %v\nbefore=%s\nafter=%s", err, legacy, after)
	}
	if len(mounter.mountCalls) != 0 || len(mounter.unmountCalls) != 0 || len(mounter.mountEntryExistsCalls) != 0 {
		t.Fatalf("staged record triggered mounter activity: mounts=%v unmounts=%v probes=%v",
			mounter.mountCalls, mounter.unmountCalls, mounter.mountEntryExistsCalls)
	}
}

// TestFileNodeUnpublishVolume_ForeignMountPreserved verifies the node never
// claims a mount it did not create: when the recorded target now carries an
// unrelated mount, unpublish fails closed and keeps the record so the
// conflict is investigated rather than silently released.
func TestFileNodeUnpublishVolume_ForeignMountPreserved(t *testing.T) {
	t.Parallel()

	mounter := &foreignMountMounter{mockMounter: newMockMounter(), foreignMounts: map[string]string{}}
	node, stateDir := newFilePublishNode(t, mounter)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// The node's own mount vanished and something else mounted at the target.
	delete(mounter.mountedPaths, target)
	delete(mounter.mountSource, target)
	mounter.foreignMounts[target] = "10.9.9.9:/unrelated"

	if _, err := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: target,
	}); status.Code(err) != unpublishWrongTargetCode {
		t.Fatalf("unpublish over a foreign mount = %v, want %s", err, unpublishWrongTargetCode)
	}
	if _, foreign := mounter.foreignMounts[target]; !foreign {
		t.Fatal("foreign mount at target was removed")
	}
	assertFileRecordPresent(t, node, stateDir)
	if len(mounter.unmountCalls) != 0 {
		t.Fatalf("unmount attempted on foreign mount: %v", mounter.unmountCalls)
	}
}

// unpublishWrongTargetCode is the fail-closed answer when a file-profile
// unpublish names a mounted path the volume's durable record does not own.
const unpublishWrongTargetCode = codes.FailedPrecondition

// TestFileNodeUnpublishVolume_OwnTargetOnly pins that unpublish tears down
// only a target this volume's record owns. A mounted path outside the record
// (an owned proxy, a peer volume's target, a hand mount) is refused without
// any unmount, leaving every real mount and the durable record byte-identical;
// an unmounted unknown path stays an idempotent success.
func TestFileNodeUnpublishVolume_OwnTargetOnly(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, stateDir := newFilePublishNode(t, mounter)
	root := t.TempDir()
	targetA, targetB := filepath.Join(root, "pod-a"), filepath.Join(root, "pod-b")
	for _, target := range []string{targetA, targetB} {
		req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
		req.VolumeContext = fileRemoteVolumeContext()
		if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
			t.Fatalf("publish %q: %v", target, err)
		}
	}
	recordPath := filePublishStateFilePath(stateDir, filePublishVolumeID)
	recordBefore, err := os.ReadFile(recordPath) //nolint:gosec // G304: path under the test's own t.TempDir state dir
	if err != nil {
		t.Fatalf("read durable record: %v", err)
	}

	// Mounted paths the record never owned: one carrying the very same export
	// (indistinguishable by source alone) and one unrelated hand mount.
	sameExport := filepath.Join(root, "unrecorded-same-export")
	foreign := filepath.Join(root, "unrecorded-foreign")
	mounter.mountedPaths[sameExport] = true
	mounter.mountSource[sameExport] = "192.0.2.10:/export/native"
	mounter.mountedPaths[foreign] = true
	mounter.mountSource[foreign] = "/dev/sdz1"
	otherVolume := "agent/nfs/directory/pool/native/never-published"
	noRecordMounted := filepath.Join(root, "no-record-mounted")
	mounter.mountedPaths[noRecordMounted] = true
	mounter.mountSource[noRecordMounted] = "192.0.2.10:/export/native"

	for _, tc := range []struct{ name, volumeID, target string }{
		{"unrecorded target with same export", filePublishVolumeID, sameExport},
		{"unrecorded foreign mount", filePublishVolumeID, foreign},
		{"peer target under another volume handle", otherVolume, targetA},
		{"no record, mounted target", otherVolume, noRecordMounted},
	} {
		_, unpublishErr := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
			VolumeId: tc.volumeID, TargetPath: tc.target,
		})
		if status.Code(unpublishErr) != unpublishWrongTargetCode {
			t.Fatalf("%s: unpublish = %v, want %s", tc.name, unpublishErr, unpublishWrongTargetCode)
		}
		if !mounter.mountedPaths[tc.target] {
			t.Fatalf("%s: refused unpublish still removed the mount at %q", tc.name, tc.target)
		}
	}

	// Unknown, unmounted paths remain idempotent successes.
	for _, tc := range []struct{ name, volumeID string }{
		{"recorded volume", filePublishVolumeID},
		{"unknown volume", otherVolume},
	} {
		if _, unpublishErr := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
			VolumeId: tc.volumeID, TargetPath: filepath.Join(root, "never-mounted"),
		}); unpublishErr != nil {
			t.Fatalf("%s: unpublish of unknown unmounted target = %v, want success", tc.name, unpublishErr)
		}
	}

	if len(mounter.unmountCalls) != 0 {
		t.Fatalf("unpublish of non-owned targets issued unmounts: %v", mounter.unmountCalls)
	}
	for _, path := range []string{targetA, targetB, sameExport, foreign, noRecordMounted} {
		if !mounter.mountedPaths[path] {
			t.Fatalf("mount at %q disappeared", path)
		}
	}
	recordAfter, err := os.ReadFile(recordPath) //nolint:gosec // G304: path under the test's own t.TempDir state dir
	if err != nil {
		t.Fatalf("durable record lost: %v", err)
	}
	if !bytes.Equal(recordAfter, recordBefore) {
		t.Fatalf("refused unpublish rewrote the durable record:\nbefore=%s\nafter=%s", recordBefore, recordAfter)
	}
	assertFileRecordAbsent(t, node, stateDir, otherVolume)
}

// ─────────────────────────────────────────────────────────────────────────────
// Restart recovery and stats contract on the publish record
// ─────────────────────────────────────────────────────────────────────────────

// TestFileNodePublishVolume_RestartRecoversRecord verifies the durable
// publish descriptor survives a process restart: a fresh NodeServer on the
// same state dir still serves exact-quota stats for the published target and
// unpublishes it without re-learning the request context.  The restarted node
// runs the same startup restore the binary performs, which must skip file
// records (nothing userspace to re-apply) before the publish RPC is retried.
func TestFileNodePublishVolume_RestartRecoversRecord(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	stateDir := t.TempDir()
	node := NewNodeServer("storage-node", nil, mounter).
		WithDriverName(v1alpha1.FileCSIDriver).WithStateDir(stateDir)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var statsFor *FileStageState
	restarted := NewNodeServer("storage-node", nil, mounter).
		WithDriverName(v1alpha1.FileCSIDriver).WithStateDir(stateDir).
		WithFilesystemStatsReader(
			func(_ context.Context, _ string, state *FileStageState) (*csipb.NodeGetVolumeStatsResponse, error) {
				statsFor = state
				return &csipb.NodeGetVolumeStatsResponse{Usage: []*csipb.VolumeUsage{
					{Unit: csipb.VolumeUsage_BYTES, Total: state.CapacityBytes},
				}}, nil
			})
	// File publish records carry no userspace session state, so the startup
	// restore must skip them rather than fail on the unfamiliar protocol type.
	if err := restarted.RestoreProtocolSessions(func(string, ...any) {}); err != nil {
		t.Fatalf("startup restore rejected the file record: %v", err)
	}

	// A republish of the same target after restart is idempotent: the durable
	// record already admits the target, so the retry re-attaches without
	// changing the existing mount.
	if _, err := restarted.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("republish of recorded target after restart: %v", err)
	}

	resp, err := restarted.NodeGetVolumeStats(context.Background(), &csipb.NodeGetVolumeStatsRequest{
		VolumeId: filePublishVolumeID, VolumePath: target,
	})
	if err != nil {
		t.Fatalf("stats after restart: %v", err)
	}
	if got := resp.GetUsage(); len(got) != 1 || got[0].GetTotal() != 1048576 {
		t.Fatalf("restarted stats lost the admitted bound: %v", got)
	}
	if statsFor == nil || statsFor.CanonicalSource != "/existing/data" || statsFor.ResourceID != "uuid:42" {
		t.Fatalf("restart did not recover the adopted identity: %+v", statsFor)
	}

	if _, unpublishErr := restarted.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: target,
	}); unpublishErr != nil {
		t.Fatalf("unpublish after restart: %v", unpublishErr)
	}
	assertFileRecordAbsent(t, restarted, stateDir, filePublishVolumeID)
}

// TestFileNodeGetVolumeStats_RecordedTargetRequired pins the stats contract
// on published records: the admitted quota answers only for paths that are
// recorded publish targets, so a foreign or stale path fails closed.
func TestFileNodeGetVolumeStats_RecordedTargetRequired(t *testing.T) {
	t.Parallel()

	statsReader := func(_ context.Context, _ string, state *FileStageState) (*csipb.NodeGetVolumeStatsResponse, error) {
		return &csipb.NodeGetVolumeStatsResponse{Usage: []*csipb.VolumeUsage{
			{Unit: csipb.VolumeUsage_BYTES, Total: state.CapacityBytes},
		}}, nil
	}
	mounter := newMockMounter()
	node, _ := newFilePublishNode(t, mounter)
	node.WithFilesystemStatsReader(statsReader)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()
	if _, err := node.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("publish: %v", err)
	}

	resp, err := node.NodeGetVolumeStats(context.Background(), &csipb.NodeGetVolumeStatsRequest{
		VolumeId: filePublishVolumeID, VolumePath: target,
	})
	if err != nil {
		t.Fatalf("stats for recorded target: %v", err)
	}
	if got := resp.GetUsage(); len(got) != 1 || got[0].GetTotal() != 1048576 {
		t.Fatalf("recorded target stats lost exact bound: %v", got)
	}
	// Ordered: the foreign path must fail membership while the record still
	// exists, so a missing-targets guard cannot mask as a no-record failure.
	foreignPath := filepath.Join(t.TempDir(), "other-target")
	_, err = node.NodeGetVolumeStats(context.Background(), &csipb.NodeGetVolumeStatsRequest{
		VolumeId: filePublishVolumeID, VolumePath: foreignPath,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stats for never-published path = %v, want FailedPrecondition", err)
	}

	// After the last target is unpublished the record is gone entirely; the
	// same path then fails on metadata, never on a stale mount.
	if _, unpublishErr := node.NodeUnpublishVolume(context.Background(), &csipb.NodeUnpublishVolumeRequest{
		VolumeId: filePublishVolumeID, TargetPath: target,
	}); unpublishErr != nil {
		t.Fatalf("unpublish before stale-path stat: %v", unpublishErr)
	}
	_, err = node.NodeGetVolumeStats(context.Background(), &csipb.NodeGetVolumeStatsRequest{
		VolumeId: filePublishVolumeID, VolumePath: target,
	})
	if err == nil {
		t.Fatal("stats answered for a fully unpublished target")
	}
}

// TestFileNodePublishVolume_ConcurrentSameTarget pins volume-lock
// serialization: two publishes racing the same target converge to exactly
// one mount entry and one recorded target instead of double-mounting.
func TestFileNodePublishVolume_ConcurrentSameTarget(t *testing.T) {
	t.Parallel()

	mounter := newMockMounter()
	node, _ := newFilePublishNode(t, mounter)
	target := filepath.Join(t.TempDir(), "pod-target")

	req := fileRemotePublishRequest(filePublishVolumeID, target, mountCap(ProtocolNFS))
	req.VolumeContext = fileRemoteVolumeContext()

	const racers = 4
	// Clone in the test goroutine: the checked helper may call t.Fatalf.
	reqs := make([]*csipb.NodePublishVolumeRequest, racers)
	for i := range racers {
		reqs[i] = clonePublishRequest(t, req)
	}
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Go(func() {
			_, errs[i] = node.NodePublishVolume(context.Background(), reqs[i])
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if got := len(mounter.mountCalls); got != 1 {
		t.Fatalf("raced publishes mounted %d times, want one", got)
	}
	if mounted, err := mountEntryExists(node, target); err != nil || !mounted {
		t.Fatalf("target mounted=%v err=%v after concurrent publish", mounted, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Remote NFS request validation edges
// ─────────────────────────────────────────────────────────────────────────────

// TestFileNodePublishVolume_RemoteMissingExport pins remote validation: an
// adopted filesystem served over NFS still needs the export identity the
// mount source is built from — there is no inferred default.
func TestFileNodePublishVolume_RemoteMissingExport(t *testing.T) {
	t.Parallel()

	node, _ := newFilePublishNode(t, newMockMounter())
	target := filepath.Join(t.TempDir(), "pod-target")

	for name, vc := range map[string]map[string]string{
		"missing address":     withNFSVolumeContext(fileRemoteVolumeContext(), VolumeContextKeyAddress, ""),
		"missing export path": withNFSVolumeContext(fileRemoteVolumeContext(), vcVolumeRef, ""),
		"relative export":     withNFSVolumeContext(fileRemoteVolumeContext(), vcVolumeRef, "relative/export"),
		"unsupported version": withNFSVolumeContext(fileRemoteVolumeContext(), VolumeContextKeyNFSVersion, "3"),
		"unsupported port":    withNFSVolumeContext(fileRemoteVolumeContext(), VolumeContextKeyPort, "111"),
		"wrong protocol": {
			VolumeContextKeyProtocolType: ProtocolNVMeoFTCP,
			paramFSType:                  ProtocolNFS,
			VolumeContextKeyAddress:      "192.0.2.10",
			VolumeContextKeyPort:         "4420",
			vcVolumeRef:                  "/export/native",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := fileRemotePublishRequest(filePublishVolumeID+name, target+name, mountCap(ProtocolNFS))
			req.VolumeContext = vc
			_, err := node.NodePublishVolume(context.Background(), req)
			code := status.Code(err)
			if code != codes.InvalidArgument && code != codes.FailedPrecondition {
				t.Fatalf("remote publish %s = %v, want InvalidArgument/FailedPrecondition, never success", name, err)
			}
			if mounted, probeErr := mountEntryExists(node, target+name); probeErr != nil || mounted {
				t.Fatalf("%s left %q mounted=%v err=%v", name, target+name, mounted, probeErr)
			}
		})
	}
}
