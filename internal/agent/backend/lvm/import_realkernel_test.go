//go:build linux && realkernel

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

package lvm_test

// Real-kernel lane for LV adoption (Tier K / K-thin of the issue-163
// contract).  It needs root, loop, dm_mod, dm_thin_pool and lvm2/e2fsprogs
// tools; a missing capability fails the test hard — this lane never skips.
// Run it only in its dedicated lane:
//
//	go test -tags realkernel -run 'TestRealKernel' ./internal/agent/backend/lvm/

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/lvm"
)

type rkVG struct {
	name string
	dir  string
}

func rkRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed test tooling
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// rkRequire fails hard unless every capability the lane needs is present.
func rkRequire(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("real-kernel LVM lane requires root (euid %d)", os.Geteuid())
	}
	for _, tool := range []string{"losetup", "pvcreate", "vgcreate", "lvcreate", "lvchange", "lvs", "vgs",
		"lvremove", "vgremove", "pvremove", "mkfs.ext4", "mount", "umount", "dmsetup", "blkid", "blockdev"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("real-kernel LVM lane needs %s: %v", tool, err)
		}
	}
	for _, mod := range []string{"dm_mod", "dm_thin_pool", "loop"} {
		if _, err := os.Stat(filepath.Join("/sys/module", mod)); err != nil {
			t.Fatalf("real-kernel LVM lane needs kernel module %s loaded: %v", mod, err)
		}
	}
}

// rkNewVG builds a disposable VG on a loop device and tears it down in
// cleanup, reporting (never swallowing) teardown failures.
func rkNewVG(t *testing.T) rkVG {
	t.Helper()
	dir := t.TempDir()
	img := filepath.Join(dir, "pv.img")
	rkRun(t, "truncate", "-s", "512M", img)
	loop := rkRun(t, "losetup", "--find", "--show", img)
	vg := rkVG{name: fmt.Sprintf("pcsi163rk%d", os.Getpid()%100000), dir: dir}
	t.Cleanup(func() {
		for _, cmd := range [][]string{
			{"vgremove", "-f", "-y", vg.name},
			{"pvremove", "-f", "-y", loop},
			{"losetup", "-d", loop},
		} {
			if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil { //nolint:gosec // fixed tooling
				t.Errorf("cleanup %s: %v\n%s", strings.Join(cmd, " "), err, out)
			}
		}
	})
	rkRun(t, "pvcreate", "-y", loop)
	rkRun(t, "vgcreate", "-y", vg.name, loop)
	return vg
}

func rkIdentity(t *testing.T, vg, lv string) backend.LVMIdentity {
	t.Helper()
	out := rkRun(t, "lvs", "--noheadings", "-o", "vg_uuid,lv_uuid", vg+"/"+lv)
	fields := strings.Fields(out)
	if len(fields) != 2 {
		t.Fatalf("lvs uuids for %s/%s: %q", vg, lv, out)
	}
	return backend.LVMIdentity{VolumeGroup: vg, LogicalVolume: lv, VolumeGroupUUID: fields[0], LogicalVolumeUUID: fields[1]}
}

func rkDigest(t *testing.T, dev string) string {
	t.Helper()
	f, err := os.Open(dev) //nolint:gosec // test device
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("digest %s: %v", dev, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func rkSeqno(t *testing.T, vg string) string {
	t.Helper()
	return rkRun(t, "vgs", "--noheadings", "-o", "vg_seqno", vg)
}

func rkReason(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("error = %v, want refusal %q", err, reason)
	}
}

// TestRealKernel_LinearAdoptionAndRefusals adopts a prewritten linear LV and
// proves the refusals and the no-write invariant on a real kernel: the LVM
// metadata seqno and the device's full-block digest are unchanged by
// Import/Verify/Inspect and by every refusal.
func TestRealKernel_LinearAdoptionAndRefusals(t *testing.T) {
	rkRequire(t)
	vg := rkNewVG(t)
	rkRun(t, "lvcreate", "-y", "-L", "64m", "-n", "legacy", vg.name)
	dev := "/dev/" + vg.name + "/legacy"
	rkRun(t, "mkfs.ext4", "-q", dev)
	mnt := filepath.Join(vg.dir, "mnt")
	if err := os.Mkdir(mnt, 0o750); err != nil {
		t.Fatal(err)
	}
	rkRun(t, "mount", dev, mnt)
	if err := os.WriteFile(filepath.Join(mnt, "original.txt"), []byte("issue163 original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rkRun(t, "umount", mnt)

	ctx := context.Background()
	b := lvm.New(vg.name, "")
	id := vg.name + "/legacy"
	want := rkIdentity(t, vg.name, "legacy")
	digest, seqno := rkDigest(t, dev), rkSeqno(t, vg.name)

	path, size, err := b.ImportLV(ctx, id, 32<<20, want)
	if err != nil || path != dev || size != 64<<20 {
		t.Fatalf("ImportLV = (%q, %d, %v), want (%q, %d)", path, size, err, dev, 64<<20)
	}
	if err := b.VerifyLV(ctx, id, want); err != nil {
		t.Fatalf("VerifyLV: %v", err)
	}
	obs, err := b.InspectLV(ctx, id)
	if err != nil {
		t.Fatalf("InspectLV: %v", err)
	}
	if obs.FSType != "ext4" || obs.FSUUID != rkRun(t, "blkid", "-p", "-s", "UUID", "-o", "value", dev) ||
		obs.FSProbe != backend.FSProbeDetected || obs.FSProbeError != "" ||
		obs.ExclusiveClaim != backend.ExclusiveClaimFree ||
		len(obs.Consumers) != 0 || len(obs.Exports) != 0 || !obs.Active || obs.Identity != want {
		t.Fatalf("idle observation = %+v", obs)
	}

	// Identity: a pin with another LV UUID is refused.
	other := want
	other.LogicalVolumeUUID = "AAAAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAAAA"
	_, _, err = b.ImportLV(ctx, id, 32<<20, other)
	rkReason(t, err, backend.ImportRefusedReasonIdentity)
	rkReason(t, b.VerifyLV(ctx, id, other), backend.ImportRefusedReasonIdentity)
	_, _, err = b.ImportLV(ctx, id, 128<<20, want)
	rkReason(t, err, "too small")

	// A mount is an exclusive claimant and a mountinfo consumer.
	rkRun(t, "mount", "-o", "ro", dev, mnt)
	_, _, err = b.ImportLV(ctx, id, 32<<20, want)
	rkReason(t, err, "in use")
	obs, err = b.InspectLV(ctx, id)
	if err != nil || obs.ExclusiveClaim != backend.ExclusiveClaimBusy || len(obs.Consumers) == 0 ||
		obs.Consumers[0].Kind != "mount" || obs.Consumers[0].Detail != mnt {
		t.Fatalf("mounted observation = (%+v, %v)", obs, err)
	}
	rkRun(t, "umount", mnt)

	// A stacked dm-linear device is a holder and an exclusive claimant.
	sectors := rkRun(t, "blockdev", "--getsz", dev)
	holder := vg.name + "-rkholder"
	rkRun(t, "dmsetup", "create", holder, "--table", "0 "+sectors+" linear "+dev+" 0")
	_, _, err = b.ImportLV(ctx, id, 32<<20, want)
	rkReason(t, err, "in use")
	obs, err = b.InspectLV(ctx, id)
	if err != nil || obs.ExclusiveClaim != backend.ExclusiveClaimBusy ||
		len(obs.Consumers) != 1 || obs.Consumers[0].Kind != "holder" {
		t.Fatalf("held observation = (%+v, %v)", obs, err)
	}
	rkRun(t, "dmsetup", "remove", holder)

	// Inactive: refused without activation, and inspect does not activate.
	rkRun(t, "lvchange", "-an", id)
	seqnoInactive := rkSeqno(t, vg.name)
	_, _, err = b.ImportLV(ctx, id, 32<<20, want)
	rkReason(t, err, "inactive")
	obs, err = b.InspectLV(ctx, id)
	if err != nil || obs.Active || obs.ExclusiveClaim != backend.ExclusiveClaimUnknown ||
		obs.FSProbe != backend.FSProbeUnknown || obs.FSType != "" {
		t.Fatalf("inactive observation = (%+v, %v)", obs, err)
	}
	if attr := rkRun(t, "lvs", "--noheadings", "-o", "lv_attr", id); len(attr) < 5 || attr[4] == 'a' {
		t.Fatalf("inactive LV was activated: lv_attr %q", attr)
	}
	if got := rkSeqno(t, vg.name); got != seqnoInactive {
		t.Fatalf("inactive refusal changed VG seqno %s → %s", seqnoInactive, got)
	}
	rkRun(t, "lvchange", "-ay", id)

	if got := rkDigest(t, dev); got != digest {
		t.Fatalf("device digest changed: %s → %s", digest, got)
	}
	// The test's own lvchange -an/-ay toggles activation only, which writes
	// no VG metadata, so the seqno must still equal the starting value.
	if got := rkSeqno(t, vg.name); got != seqno {
		t.Fatalf("VG seqno changed %s → %s: adoption wrote LVM metadata", seqno, got)
	}
}

// TestRealKernel_ThinAdoptionAndSnapshotRefusal adopts a thin LV of the
// configured pool and refuses its thin snapshot by origin.
func TestRealKernel_ThinAdoptionAndSnapshotRefusal(t *testing.T) {
	rkRequire(t)
	vg := rkNewVG(t)
	rkRun(t, "lvcreate", "-y", "-L", "128m", "-T", vg.name+"/pool")
	rkRun(t, "lvcreate", "-y", "-V", "64m", "-T", vg.name+"/pool", "-n", "thinlv")
	rkRun(t, "lvcreate", "-y", "-s", "-n", "thinsnap", vg.name+"/thinlv")
	dev := "/dev/" + vg.name + "/thinlv"
	rkRun(t, "mkfs.ext4", "-q", dev)

	ctx := context.Background()
	b := lvm.New(vg.name, "pool")
	digest, seqno := rkDigest(t, dev), rkSeqno(t, vg.name)
	want := rkIdentity(t, vg.name, "thinlv")
	if _, _, err := b.ImportLV(ctx, vg.name+"/thinlv", 32<<20, want); err != nil {
		t.Fatalf("ImportLV thin: %v", err)
	}
	_, _, err := b.ImportLV(ctx, vg.name+"/thinsnap", 32<<20, rkIdentity(t, vg.name, "thinsnap"))
	rkReason(t, err, "wrong type")
	_, _, err = lvm.New(vg.name, "").ImportLV(ctx, vg.name+"/thinlv", 32<<20, want)
	rkReason(t, err, "layout")
	if got := rkDigest(t, dev); got != digest {
		t.Fatalf("thin device digest changed: %s → %s", digest, got)
	}
	if got := rkSeqno(t, vg.name); got != seqno {
		t.Fatalf("VG seqno changed %s → %s", seqno, got)
	}
}
