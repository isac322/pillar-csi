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

package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

const ownNQN = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"

func inspectRequest() *agentv1.InspectVolumeRequest {
	return &agentv1.InspectVolumeRequest{VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_LVM}
}

// stateSnapshot lists every file under dir with its bytes and mtime.
func stateSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := info.ModTime().Format(time.RFC3339Nano)
		if !d.IsDir() {
			data, readErr := os.ReadFile(path) //nolint:gosec // G304: test reads a file under t.TempDir().
			if readErr != nil {
				return readErr
			}
			entry += " " + string(data)
		}
		snap[path] = entry
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return snap
}

func requireSnapshotUnchanged(t *testing.T, before, after map[string]string, what string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s changed the state dir: before %v, after %v", what, before, after)
	}
	for path, entry := range before {
		if after[path] != entry {
			t.Fatalf("%s changed %s", what, path)
		}
	}
}

// InspectVolume reports the mark and the LV as observed and writes nothing:
// the mark keeps its bytes and mtime, no temp file appears, an absent mark
// is reported absent without creating any file or directory, a corrupt
// mark is Internal, and a non-LVM backend is Unimplemented.
func TestInspectVolume_ReportsWithoutWriting(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	b.consumers = []backend.DeviceConsumer{{Kind: "mount", Detail: "/mnt/old"}}
	srv, stateDir, _ := newLVTestServer(t, b)
	ctx := context.Background()
	mark := endedPinnedMark(3, testLVSource(), true)
	seedMark(t, stateDir, mark)
	before := stateSnapshot(t, stateDir)

	resp, err := srv.InspectVolume(ctx, inspectRequest())
	if err != nil {
		t.Fatalf("InspectVolume: %v", err)
	}
	requireInspectedEndedPin(t, resp)
	requireSnapshotUnchanged(t, before, stateSnapshot(t, stateDir), "InspectVolume")

	// Absent history: reported absent; nothing is created.
	freshState := filepath.Join(t.TempDir(), "state")
	fresh := agent.NewServer(map[string]backend.VolumeBackend{testPool: b}, t.TempDir(),
		agent.WithDrainStateDir(freshState))
	resp, err = fresh.InspectVolume(ctx, inspectRequest())
	if err != nil {
		t.Fatalf("InspectVolume without a mark: %v", err)
	}
	if resp.GetFence().GetExists() || resp.GetFence().GetVolumeUid() != "" {
		t.Errorf("fence without a mark = %v, want absent", resp.GetFence())
	}
	if _, statErr := os.Stat(freshState); !os.IsNotExist(statErr) {
		t.Errorf("InspectVolume created the state dir (stat err %v)", statErr)
	}

	// Corrupt mark: Internal, bytes untouched.
	err = os.WriteFile(markFilePath(stateDir), []byte("{broken"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := stateSnapshot(t, stateDir)
	_, err = srv.InspectVolume(ctx, inspectRequest())
	requireCode(t, "InspectVolume of a corrupt mark", err, codes.Internal)
	requireSnapshotUnchanged(t, corrupt, stateSnapshot(t, stateDir), "InspectVolume of a corrupt mark")

	// Non-LVM backend.
	zvol := newTestServer(t, &mockBackend{})
	_, err = zvol.InspectVolume(ctx, &agentv1.InspectVolumeRequest{
		VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
	})
	requireCode(t, "InspectVolume of a zvol", err, codes.Unimplemented)
}

// requireInspectedEndedPin asserts resp reports the seeded ended pin of
// lifecycle-a and the backend's LV, filesystem and consumer observation.
func requireInspectedEndedPin(t *testing.T, resp *agentv1.InspectVolumeResponse) {
	t.Helper()
	wantFence := &agentv1.FenceObservation{
		Exists: true, VolumeUid: "lifecycle-a", Generation: 3, Ended: true,
		EndedUids: []string{"lifecycle-a"}, PreserveOriginal: true, LvmSource: testLVSource(),
	}
	if !proto.Equal(resp.GetFence(), wantFence) {
		t.Errorf("fence = %v, want %v", resp.GetFence(), wantFence)
	}
	if !proto.Equal(resp.GetLvm().GetIdentity(), testLVSource()) || !resp.GetLvm().GetActive() ||
		resp.GetLvm().GetExclusiveClaim() != backend.ExclusiveClaimFree ||
		resp.GetLvm().GetDevicePath() != testLVDevicePath {
		t.Errorf("lvm = %v, want the backend's observation", resp.GetLvm())
	}
	if resp.GetFilesystemType() != "ext4" || resp.GetFilesystemUuid() != "fs-uuid-1" ||
		resp.GetFilesystemProbeState() != backend.FSProbeDetected || resp.GetFilesystemProbeError() != "" {
		t.Errorf("filesystem = %q/%q probe %q (%q), want detected ext4", resp.GetFilesystemType(),
			resp.GetFilesystemUuid(), resp.GetFilesystemProbeState(), resp.GetFilesystemProbeError())
	}
	if c := resp.GetConsumers(); len(c) != 1 || c[0].GetKind() != "mount" || c[0].GetDetail() != "/mnt/old" {
		t.Errorf("consumers = %v, want the observed mount", c)
	}
}

// An inconclusive filesystem signature probe is reported as "unknown" with
// its explanation and no filesystem type or UUID — never as a blank device —
// while the rest of the observation is still reported, and InspectVolume
// writes nothing and never touches the LV.
func TestInspectVolume_ReportsUnknownFilesystemProbe(t *testing.T) {
	t.Parallel()
	const why = "blkid -p /dev/tank/pvc-abc: exit status 2 with no output: not proven blank"
	b := newMockLVBackend()
	b.fsProbeErr = why
	srv, stateDir, _ := newLVTestServer(t, b)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 1, testLVSource(), true))
	before := stateSnapshot(t, stateDir)

	resp, err := srv.InspectVolume(context.Background(), inspectRequest())
	if err != nil {
		t.Fatalf("InspectVolume: %v", err)
	}
	if resp.GetFilesystemProbeState() != backend.FSProbeUnknown || resp.GetFilesystemProbeError() != why ||
		resp.GetFilesystemType() != "" || resp.GetFilesystemUuid() != "" {
		t.Errorf("filesystem = %q/%q probe %q (%q), want unknown with %q", resp.GetFilesystemType(),
			resp.GetFilesystemUuid(), resp.GetFilesystemProbeState(), resp.GetFilesystemProbeError(), why)
	}
	if !proto.Equal(resp.GetLvm().GetIdentity(), testLVSource()) || !resp.GetLvm().GetActive() ||
		resp.GetLvm().GetExclusiveClaim() != backend.ExclusiveClaimFree || !resp.GetFence().GetExists() {
		t.Errorf("lvm = %v fence = %v, want the rest of the observation", resp.GetLvm(), resp.GetFence())
	}
	requireSnapshotUnchanged(t, before, stateSnapshot(t, stateDir), "InspectVolume")
	requireLVUntouched(t, b.mockBackend, "InspectVolume with an unknown filesystem probe")
}

// The agent's own configured export of the volume is reported as an export
// (read from configfs), never as a consumer and never as idle evidence; an
// export under another target ID is a foreign_export consumer; the
// exclusive-claim observation is passed through.
func TestInspectVolume_ClassifiesOwnExportVsForeign(t *testing.T) {
	t.Parallel()
	const foreign = "iqn.2003-01.org.other:legacy"
	for _, claim := range []string{backend.ExclusiveClaimBusy, backend.ExclusiveClaimUnknown, backend.ExclusiveClaimFree} {
		t.Run(claim, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			srv, stateDir, _ := newLVTestServer(t, b)
			ctx := context.Background()
			seedMark(t, stateDir, pinnedMark("lifecycle-a", 1, testLVSource(), true))
			exportAndAllowPinnedLV(t, srv)
			b.claim = claim
			b.exports = []backend.DeviceConsumer{{Kind: "export", Detail: ownNQN}, {Kind: "export", Detail: foreign}}
			before := stateSnapshot(t, stateDir)

			resp, err := srv.InspectVolume(ctx, inspectRequest())
			if err != nil {
				t.Fatalf("InspectVolume: %v", err)
			}
			exports := resp.GetExports()
			if len(exports) != 1 || exports[0].GetTargetId() != ownNQN || !exports[0].GetAclEnabled() ||
				!slices.Equal(exports[0].GetAllowedHosts(), []string{"nqn.host:worker-1"}) {
				t.Errorf("exports = %v, want the own ACL-enabled NVMe-oF export", exports)
			}
			consumers := resp.GetConsumers()
			if len(consumers) != 1 || consumers[0].GetKind() != "foreign_export" || consumers[0].GetDetail() != foreign {
				t.Errorf("consumers = %v, want only the foreign export", consumers)
			}
			if got := resp.GetLvm().GetExclusiveClaim(); got != claim {
				t.Errorf("exclusive_claim = %q, want %q", got, claim)
			}
			requireSnapshotUnchanged(t, before, stateSnapshot(t, stateDir), "InspectVolume")
		})
	}
}

// exportAndAllowPinnedLV exports testVolumeID's pinned LV over NVMe-oF with
// ACLs enabled for lifecycle-a and admits host nqn.host:worker-1.
func exportAndAllowPinnedLV(t *testing.T, srv *agent.Server) {
	t.Helper()
	ctx := context.Background()
	_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testLVDevicePath,
		AclEnabled: true, Fence: token2("lifecycle-a", 1),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	_, err = srv.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: "nqn.host:worker-1", Fence: token2("lifecycle-a", 1),
	})
	if err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}
}

// Rebind runbook step 6-7: a fenced unexport under the active lifecycle
// removes the export without ending or retiring the lifecycle, a stale or
// other-lifecycle token cannot remove it, and InspectVolume then reports no
// own export, writing nothing.
func TestRebindRunbook_FencedUnexportKeepsLifecycle(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	srv, stateDir, cfgRoot := newLVTestServer(t, b)
	ctx := context.Background()
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testLVDevicePath,
		Fence: token2("lifecycle-a", 7),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	unexport := func(fence *agentv1.FencingToken) error {
		_, unexportErr := srv.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
			VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, Fence: fence,
		})
		return unexportErr
	}
	for _, stale := range []*agentv1.FencingToken{token2("lifecycle-a", 6), token2("lifecycle-b", 9)} {
		requireCode(t, "UnexportVolume with a stale token", unexport(stale), codes.FailedPrecondition)
		if n := nvmetSubsystems(t, cfgRoot); n != 1 {
			t.Fatalf("stale unexport removed the export (subsystems %d)", n)
		}
	}
	err = unexport(token2("lifecycle-a", 7))
	if err != nil {
		t.Fatalf("fenced UnexportVolume: %v", err)
	}
	m := readMark(t, stateDir)
	if m.VolumeUID != "lifecycle-a" || m.Ended || len(m.EndedUIDs) != 0 || !m.PreserveOriginal ||
		m.LVMSource["logicalVolumeUUID"] != testLVUUID {
		t.Fatalf("mark after unexport %+v, want lifecycle-a still owning with the pin", m)
	}

	before := stateSnapshot(t, stateDir)
	resp, err := srv.InspectVolume(ctx, inspectRequest())
	if err != nil {
		t.Fatalf("InspectVolume: %v", err)
	}
	if len(resp.GetExports()) != 0 || len(resp.GetConsumers()) != 0 ||
		resp.GetLvm().GetExclusiveClaim() != backend.ExclusiveClaimFree {
		t.Errorf("inspect after unexport: exports %v consumers %v claim %q, want idle",
			resp.GetExports(), resp.GetConsumers(), resp.GetLvm().GetExclusiveClaim())
	}
	requireSnapshotUnchanged(t, before, stateSnapshot(t, stateDir), "InspectVolume")
}
