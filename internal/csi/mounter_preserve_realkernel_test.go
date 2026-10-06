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

package csi

// Real-kernel lane for the preserve-original mount path (issue #163):
// KubeMounter.MountExisting runs against loop devices, device-mapper and
// the host's real blkid/mount/umount.  It needs root, the loop and dm_mod
// kernel modules and util-linux/e2fsprogs/dmsetup; a missing capability
// fails the test hard — this lane never skips.  Run it only in its
// dedicated lane:
//
//	sudo -E go test -tags realkernel -count=1 -v \
//	    -run 'TestRealKernel_MountExisting|TestRealKernel_FormatAndMount' ./internal/csi/
//
// Command-level negatives come from a PATH tripwire: every filesystem
// writer (mkfs*, mke2fs, fsck*, e2fsck, resize2fs, xfs_growfs, ...) and the
// probe/mount tools are shimmed by scripts that append their invocation to
// a log and then exec the real binary.  Nothing is faked; the shims only
// record what the production code really ran.  The log must show blkid
// (proving the shims are live) and never a writer.
//
// A read-write mount legitimately writes ext4 superblock/journal metadata
// (mount count, mount time, orphan list), so the full-block digest is only
// promised unchanged for refusals and for read-only mounts of a cleanly
// unmounted filesystem.  Read-write mounts are checked by filesystem
// identity instead: UUID, label, creation time, geometry, last-check time,
// device size and the original files' bytes and inode numbers.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// pmRun runs a host command and fails the test on error.
func pmRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed test tooling
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// pmRequire fails hard unless every capability the lane needs is present.
func pmRequire(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("real-kernel preserve-mount lane requires root (euid %d)", os.Geteuid())
	}
	for _, tool := range []string{
		"truncate", "losetup", "mkfs.ext4", "dumpe2fs", "blkid", "blockdev",
		"mount", "umount", "dmsetup",
	} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("real-kernel preserve-mount lane needs %s: %v", tool, err)
		}
	}
	for _, mod := range []string{"loop", "dm_mod"} {
		if _, err := os.Stat(filepath.Join("/sys/module", mod)); err != nil {
			t.Fatalf("real-kernel preserve-mount lane needs kernel module %s loaded: %v", mod, err)
		}
	}
	if _, err := os.Stat("/proc/self/mountinfo"); err != nil {
		t.Fatalf("real-kernel preserve-mount lane needs /proc/self/mountinfo: %v", err)
	}
}

// pmSettle waits for udev to release freshly probed devices so teardown
// does not race a transient opener.  Hosts without udevadm have no udev
// openers to wait for.
func pmSettle() {
	if _, err := exec.LookPath("udevadm"); err == nil {
		_ = exec.Command("udevadm", "settle").Run() //nolint:errcheck // best-effort wait, teardown errors are reported
	}
}

// pmNewLoop attaches a zero-filled backing file of size bytes to a new loop
// device.  Cleanup detaches exactly that device, reports (never swallows)
// teardown failures, and then proves the backing file has no loop left.
func pmNewLoop(t *testing.T, size string) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "disk.img")
	pmRun(t, "truncate", "-s", size, img)
	// Registered first so it runs after the detach below (cleanups are LIFO).
	t.Cleanup(func() {
		out, err := exec.Command("losetup", "-j", img).CombinedOutput()
		if err != nil {
			t.Errorf("leftover check losetup -j %s: %v\n%s", img, err, out)
			return
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			t.Errorf("loop device left attached to %s: %s", img, s)
		}
	})
	dev := pmRun(t, "losetup", "--find", "--show", img)
	if !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("losetup returned %q, want a /dev/loopN device", dev)
	}
	t.Cleanup(func() {
		pmSettle()
		if out, err := exec.Command("losetup", "-d", dev).CombinedOutput(); err != nil {
			t.Errorf("cleanup losetup -d %s: %v\n%s", dev, err, out)
		}
	})
	return dev
}

// pmFormatExt4 builds the "original" ext4 with lazy initialisation off, so
// no background itable/journal zeroing changes the device after mkfs.
func pmFormatExt4(t *testing.T, dev, label string) {
	t.Helper()
	pmRun(t, "mkfs.ext4", "-q", "-F", "-L", label,
		"-E", "lazy_itable_init=0,lazy_journal_init=0", dev)
}

// pmFile is one known original file: its bytes and its inode identity.
type pmFile struct {
	name string
	data []byte
	ino  uint64
}

// pmPayload returns n deterministic pseudo-random bytes.
func pmPayload(n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	block := sha256.Sum256([]byte("pillar-csi issue 163 preserve-original payload"))
	for len(out) < n {
		out = append(out, block[:]...)
		block = sha256.Sum256(block[:])
	}
	return out[:n]
}

// pmMountEntry reports the /proc/self/mountinfo entry for target: its
// source, filesystem type and per-mount options.
func pmMountEntry(t *testing.T, target string) (source, fsType string, opts []string, ok bool) {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("read mountinfo: %v", err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if sep < 6 || len(fields) < sep+3 || fields[4] != target {
			continue
		}
		source, fsType, opts = fields[sep+2], fields[sep+1], strings.Split(fields[5], ",")
		ok = true // keep scanning: the last entry is the visible one
	}
	return source, fsType, opts, ok
}

// pmRequireUnmounted fails unless target has no mount entry.
func pmRequireUnmounted(t *testing.T, target, why string) {
	t.Helper()
	if src, _, _, ok := pmMountEntry(t, target); ok {
		t.Fatalf("%s: %s is mounted from %s, want nothing mounted", why, target, src)
	}
}

// pmTarget creates a mount target and registers a cleanup that unmounts
// whatever this test mounted there through km, reporting failures.
func pmTarget(t *testing.T, km *KubeMounter) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	target := filepath.Join(dir, "mnt")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	t.Cleanup(func() {
		if err := km.Unmount(target); err != nil {
			t.Errorf("cleanup unmount %s: %v", target, err)
		}
		if src, _, _, ok := pmMountEntry(t, target); ok {
			t.Errorf("cleanup left %s mounted from %s", target, src)
		}
	})
	return target
}

// pmUnmount unmounts target through km and proves it is gone.
func pmUnmount(t *testing.T, km *KubeMounter, target string) {
	t.Helper()
	if err := km.Unmount(target); err != nil {
		t.Fatalf("Unmount %s: %v", target, err)
	}
	pmRequireUnmounted(t, target, "after Unmount")
}

// pmSeed mounts the freshly formatted dev with a plain mount(8), writes the
// known original files durably, records their inode numbers and cleanly
// unmounts again.
func pmSeed(t *testing.T, km *KubeMounter, dev string) []pmFile {
	t.Helper()
	target := pmTarget(t, km)
	pmRun(t, "mount", "-t", defaultFsType, dev, target)
	files := []pmFile{
		{name: "original.txt", data: []byte("issue163 preserve-original data\n")},
		{name: "payload.bin", data: pmPayload(4 << 20)},
	}
	for i := range files {
		p := filepath.Join(target, files[i].name)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: test-owned path
		if err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
		if _, err := f.Write(files[i].data); err != nil {
			_ = f.Close() //nolint:errcheck // the write error is reported
			t.Fatalf("write %s: %v", p, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close() //nolint:errcheck // the sync error is reported
			t.Fatalf("fsync %s: %v", p, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close %s: %v", p, err)
		}
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		files[i].ino = st.Ino
	}
	pmUnmount(t, km, target)
	return files
}

// pmRequireFiles proves every original file under target has its original
// bytes, size and inode number.
func pmRequireFiles(t *testing.T, target string, files []pmFile) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(target, f.name)
		got, err := os.ReadFile(p) //nolint:gosec // G304: test-owned path
		if err != nil {
			t.Fatalf("read original %s: %v", p, err)
		}
		if !bytes.Equal(got, f.data) {
			t.Fatalf("original %s changed: %d bytes sha256 %x, want %d bytes sha256 %x",
				p, len(got), sha256.Sum256(got), len(f.data), sha256.Sum256(f.data))
		}
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Ino != f.ino || st.Size != int64(len(f.data)) {
			t.Fatalf("original %s identity changed: ino %d size %d, want ino %d size %d",
				p, st.Ino, st.Size, f.ino, len(f.data))
		}
	}
}

// pmDigest hashes every byte of dev.
func pmDigest(t *testing.T, dev string) string {
	t.Helper()
	f, err := os.Open(dev) //nolint:gosec // G304: test device
	if err != nil {
		t.Fatalf("open %s for digest: %v", dev, err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("digest %s: %v", dev, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pmIdentity is the on-disk identity of an ext4 filesystem that mkfs,
// fsck or resize would change but an ordinary mount/unmount does not.
type pmIdentity struct {
	blkid string
	size  string
	super map[string]string
}

var pmSuperFields = []string{
	"Filesystem volume name", "Filesystem UUID", "Filesystem features",
	"Filesystem created", "Filesystem state", "Last checked", "Inode count",
	"Block count", "Reserved block count", "Block size",
}

func pmIdentityOf(t *testing.T, dev string) pmIdentity {
	t.Helper()
	id := pmIdentity{
		blkid: pmRun(t, "blkid", "-p", "-o", "export", "-s", "TYPE", "-s", "UUID", "-s", "LABEL", dev),
		size:  pmRun(t, "blockdev", "--getsize64", dev),
		super: map[string]string{},
	}
	for line := range strings.SplitSeq(pmRun(t, "dumpe2fs", "-h", dev), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if ok && slices.Contains(pmSuperFields, key) {
			id.super[key] = strings.TrimSpace(val)
		}
	}
	if len(id.super) != len(pmSuperFields) {
		t.Fatalf("dumpe2fs -h %s: got fields %v, want all of %v", dev, id.super, pmSuperFields)
	}
	return id
}

func pmRequireIdentity(t *testing.T, dev string, want pmIdentity) {
	t.Helper()
	got := pmIdentityOf(t, dev)
	if got.blkid != want.blkid || got.size != want.size {
		t.Fatalf("filesystem identity of %s changed:\nblkid %q size %s\nwant blkid %q size %s",
			dev, got.blkid, got.size, want.blkid, want.size)
	}
	for _, k := range pmSuperFields {
		if got.super[k] != want.super[k] {
			t.Fatalf("superblock %q of %s changed: %q, want %q", k, dev, got.super[k], want.super[k])
		}
	}
}

// pmWriters are the tools that can rewrite a device or its filesystem.
var pmWriters = []string{
	"mkfs", "mkfs.ext2", "mkfs.ext3", "mkfs.ext4", "mkfs.xfs", "mke2fs",
	"fsck", "fsck.ext2", "fsck.ext3", "fsck.ext4", "fsck.xfs", "e2fsck",
	"xfs_repair", "resize2fs", "xfs_growfs", "tune2fs", "xfs_admin",
	"wipefs", "sfdisk", "sgdisk", "parted", "dd",
}

// pmTripwire shims pmWriters and the probe/mount tools on PATH.
type pmTripwire struct {
	log string
}

// pmInstallTripwire puts logging shims in front of PATH for the rest of the
// test.  Each shim appends "<tool> <args>" to the log and execs the real
// binary found on the original PATH.
func pmInstallTripwire(t *testing.T) *pmTripwire {
	t.Helper()
	dir := t.TempDir()
	tw := &pmTripwire{log: filepath.Join(dir, "calls.log")}
	orig := os.Getenv("PATH")
	for _, s := range []string{dir, tw.log, orig} {
		if strings.ContainsAny(s, "'\n") {
			t.Fatalf("tripwire path %q cannot be shell-quoted", s)
		}
	}
	shimmed := append(slices.Clone(pmWriters), "blkid", "mount", "umount")
	for _, tool := range shimmed {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"%s $*\" >> '%s'\nPATH='%s'\nexport PATH\nexec %s \"$@\"\n",
			tool, tw.log, orig, tool)
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o700); err != nil { //nolint:gosec // G306: test shim
			t.Fatalf("write shim %s: %v", tool, err)
		}
	}
	if err := os.WriteFile(tw.log, nil, 0o600); err != nil {
		t.Fatalf("create tripwire log: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+orig)
	return tw
}

// mark returns the current number of logged calls.
func (tw *pmTripwire) mark(t *testing.T) int {
	t.Helper()
	return len(tw.calls(t, 0))
}

func (tw *pmTripwire) calls(t *testing.T, from int) []string {
	t.Helper()
	raw, err := os.ReadFile(tw.log)
	if err != nil {
		t.Fatalf("read tripwire log: %v", err)
	}
	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out[min(from, len(out)):]
}

// requireCalls checks the tools run since mark: never a writer, blkid
// exactly when wantProbe, and — when refused — no mount(8).  A successful
// mount is proven by the mount table instead of the log, because mount(8)
// may be launched through systemd-run.
func (tw *pmTripwire) requireCalls(t *testing.T, mark int, wantProbe, refused bool) {
	t.Helper()
	calls := tw.calls(t, mark)
	var probed, mounted bool
	for _, c := range calls {
		tool, _, _ := strings.Cut(c, " ")
		switch {
		case slices.Contains(pmWriters, tool) || strings.HasPrefix(tool, "mkfs") || strings.HasPrefix(tool, "fsck"):
			t.Fatalf("device writer ran: %q (all calls %q)", c, calls)
		case tool == "blkid":
			probed = true
		case tool == "mount":
			mounted = true
		}
	}
	if probed != wantProbe || (refused && mounted) {
		t.Fatalf("calls %q: blkid ran=%v mount ran=%v, want blkid=%v and no mount on refusal=%v",
			calls, probed, mounted, wantProbe, refused)
	}
}

// pmOriginal is a loop device holding a cleanly unmounted ext4 with known
// files, plus its identity and full-block digest.
type pmOriginal struct {
	dev    string
	files  []pmFile
	id     pmIdentity
	digest string
}

func pmNewOriginal(t *testing.T, km *KubeMounter) pmOriginal {
	t.Helper()
	dev := pmNewLoop(t, "128M")
	pmFormatExt4(t, dev, "pcsi163orig")
	files := pmSeed(t, km, dev)
	return pmOriginal{dev: dev, files: files, id: pmIdentityOf(t, dev), digest: pmDigest(t, dev)}
}

// TestRealKernel_MountExistingPreservesOriginal mounts a pre-existing ext4
// read-write through MountExisting, with the default and the explicit type:
// the original files are served with their bytes and inode identity, no
// device writer runs, and after an unmount the filesystem keeps its UUID,
// label, creation time, geometry, last-check time and device size.
func TestRealKernel_MountExistingPreservesOriginal(t *testing.T) {
	pmRequire(t)
	km := NewKubeMounter()
	orig := pmNewOriginal(t, km)
	tw := pmInstallTripwire(t)

	for _, fsType := range []string{"", defaultFsType} {
		target := pmTarget(t, km)
		mark := tw.mark(t)
		if err := km.MountExisting(t.Context(), orig.dev, target, fsType, nil); err != nil {
			t.Fatalf("MountExisting(fsType=%q): %v", fsType, err)
		}
		src, gotType, opts, ok := pmMountEntry(t, target)
		if !ok || src != orig.dev || gotType != defaultFsType || !slices.Contains(opts, "rw") {
			t.Fatalf("mount entry of %s = (%q, %q, %v, %v), want (%q, ext4, rw)",
				target, src, gotType, opts, ok, orig.dev)
		}
		pmRequireFiles(t, target, orig.files)
		// An ordinary read-write mount: new data is accepted.
		if err := os.WriteFile(filepath.Join(target, "new-"+fsType+".txt"), []byte("ordinary write\n"), 0o600); err != nil {
			t.Fatalf("write through read-write MountExisting: %v", err)
		}
		pmUnmount(t, km, target)
		tw.requireCalls(t, mark, true, false)
		pmRequireIdentity(t, orig.dev, orig.id)
	}

	// The original files are still intact after both read-write sessions.
	target := pmTarget(t, km)
	if err := km.MountExisting(t.Context(), orig.dev, target, defaultFsType, []string{"ro"}); err != nil {
		t.Fatalf("MountExisting ro re-check: %v", err)
	}
	pmRequireFiles(t, target, orig.files)
	pmUnmount(t, km, target)
	pmRequireIdentity(t, orig.dev, orig.id)
}

// TestRealKernel_MountExistingReadOnly mounts the original read-only: the
// kernel serves the original files and refuses every write with EROFS, and
// the device is bit-for-bit unchanged after the unmount.
func TestRealKernel_MountExistingReadOnly(t *testing.T) {
	pmRequire(t)
	km := NewKubeMounter()
	orig := pmNewOriginal(t, km)
	tw := pmInstallTripwire(t)

	target := pmTarget(t, km)
	mark := tw.mark(t)
	if err := km.MountExisting(t.Context(), orig.dev, target, defaultFsType, []string{"ro"}); err != nil {
		t.Fatalf("MountExisting ro: %v", err)
	}
	src, gotType, opts, ok := pmMountEntry(t, target)
	if !ok || src != orig.dev || gotType != defaultFsType || !slices.Contains(opts, "ro") {
		t.Fatalf("mount entry of %s = (%q, %q, %v, %v), want (%q, ext4, ro)",
			target, src, gotType, opts, ok, orig.dev)
	}
	pmRequireFiles(t, target, orig.files)

	err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("x"), 0o600)
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("create on read-only mount: %v, want EROFS", err)
	}
	f, err := os.OpenFile(filepath.Join(target, orig.files[0].name), os.O_WRONLY|os.O_TRUNC, 0) //nolint:gosec // G304: test-owned path
	if err == nil {
		_ = f.Close() //nolint:errcheck // already failing
		t.Fatal("open original for truncating write on read-only mount succeeded, want EROFS")
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("open original for write on read-only mount: %v, want EROFS", err)
	}
	if err := os.Remove(filepath.Join(target, orig.files[1].name)); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("remove original on read-only mount: %v, want EROFS", err)
	}
	pmRequireFiles(t, target, orig.files)

	pmUnmount(t, km, target)
	tw.requireCalls(t, mark, true, false)
	if got := pmDigest(t, orig.dev); got != orig.digest {
		t.Fatalf("read-only mount changed the device: digest %s, want %s", got, orig.digest)
	}
	pmRequireIdentity(t, orig.dev, orig.id)
}

// TestRealKernel_MountExistingRefusesBlankDevice proves a device without a
// filesystem is refused with ErrNoFilesystem — whatever the requested type
// or access mode — without mounting it, formatting it or writing a byte.
func TestRealKernel_MountExistingRefusesBlankDevice(t *testing.T) {
	pmRequire(t)
	km := NewKubeMounter()
	dev := pmNewLoop(t, "64M")
	digest := pmDigest(t, dev)
	tw := pmInstallTripwire(t)

	for _, tc := range []struct {
		fsType string
		opts   []string
	}{
		{fsType: "", opts: nil},
		{fsType: defaultFsType, opts: nil},
		{fsType: defaultFsType, opts: []string{"ro"}},
		{fsType: xfsFsType, opts: nil},
	} {
		target := pmTarget(t, km)
		mark := tw.mark(t)
		err := km.MountExisting(t.Context(), dev, target, tc.fsType, tc.opts)
		if !errors.Is(err, ErrNoFilesystem) || errors.Is(err, ErrFilesystemMismatch) {
			t.Fatalf("MountExisting blank (fsType=%q opts=%v) = %v, want ErrNoFilesystem", tc.fsType, tc.opts, err)
		}
		pmRequireUnmounted(t, target, "blank refusal")
		tw.requireCalls(t, mark, true, true)
		if got := pmDigest(t, dev); got != digest {
			t.Fatalf("blank refusal (fsType=%q opts=%v) changed the device: digest %s, want %s",
				tc.fsType, tc.opts, got, digest)
		}
	}
	if out, err := exec.Command("blkid", "-p", dev).CombinedOutput(); err == nil {
		t.Fatalf("blank device gained a signature: %s", out)
	}
}

// TestRealKernel_MountExistingRefusesTypeMismatch proves an ext4 device
// requested as xfs is refused with ErrFilesystemMismatch without being
// mounted or written, and its original data stays readable.
func TestRealKernel_MountExistingRefusesTypeMismatch(t *testing.T) {
	pmRequire(t)
	km := NewKubeMounter()
	orig := pmNewOriginal(t, km)
	tw := pmInstallTripwire(t)

	for _, opts := range [][]string{nil, {"ro"}} {
		target := pmTarget(t, km)
		mark := tw.mark(t)
		err := km.MountExisting(t.Context(), orig.dev, target, xfsFsType, opts)
		if !errors.Is(err, ErrFilesystemMismatch) || errors.Is(err, ErrNoFilesystem) {
			t.Fatalf("MountExisting ext4 as xfs (opts=%v) = %v, want ErrFilesystemMismatch", opts, err)
		}
		pmRequireUnmounted(t, target, "mismatch refusal")
		tw.requireCalls(t, mark, true, true)
		if got := pmDigest(t, orig.dev); got != orig.digest {
			t.Fatalf("mismatch refusal (opts=%v) changed the device: digest %s, want %s", opts, got, orig.digest)
		}
	}

	target := pmTarget(t, km)
	if err := km.MountExisting(t.Context(), orig.dev, target, defaultFsType, []string{"ro"}); err != nil {
		t.Fatalf("MountExisting with the recorded type after the refusal: %v", err)
	}
	pmRequireFiles(t, target, orig.files)
	pmUnmount(t, km, target)
	if got := pmDigest(t, orig.dev); got != orig.digest {
		t.Fatalf("device changed after mismatch refusals: digest %s, want %s", got, orig.digest)
	}
}

// TestRealKernel_MountExistingProbeIOErrorNeverFormats puts a device-mapper
// device over the original whose first 64 KiB answer EIO, the region the
// readability probe and blkid read.  blkid would call such a device blank;
// MountExisting must fail with the I/O error before blkid runs, and the
// original underneath must be untouched.
func TestRealKernel_MountExistingProbeIOErrorNeverFormats(t *testing.T) {
	pmRequire(t)
	km := NewKubeMounter()
	orig := pmNewOriginal(t, km)

	const errSectors = readProbeBytes / 512
	sectors, err := strconv.ParseInt(pmRun(t, "blockdev", "--getsz", orig.dev), 10, 64)
	if err != nil || sectors <= errSectors {
		t.Fatalf("blockdev --getsz %s = %d (%v), want more than %d sectors", orig.dev, sectors, err, errSectors)
	}
	name := fmt.Sprintf("pcsi163pm%d", os.Getpid()%100000)
	if out, err := exec.Command("dmsetup", "info", name).CombinedOutput(); err == nil {
		t.Fatalf("dm device %s already exists; refusing to touch it:\n%s", name, out)
	}
	// Registered first so it runs after the remove below (cleanups are LIFO).
	t.Cleanup(func() {
		if out, err := exec.Command("dmsetup", "info", name).CombinedOutput(); err == nil {
			t.Errorf("dm device %s left behind:\n%s", name, out)
		}
	})
	table := fmt.Sprintf("0 %d error\n%d %d linear %s %d\n",
		errSectors, errSectors, sectors-errSectors, orig.dev, errSectors)
	create := exec.Command("dmsetup", "create", name)
	create.Stdin = strings.NewReader(table)
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("dmsetup create %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		pmSettle()
		if out, err := exec.Command("dmsetup", "remove", "--retry", name).CombinedOutput(); err != nil {
			t.Errorf("cleanup dmsetup remove %s: %v\n%s", name, err, out)
		}
	})
	dmDev := "/dev/" + pmRun(t, "dmsetup", "info", "-c", "--noheadings", "-o", "blkdevname", name)
	pmSettle()
	tw := pmInstallTripwire(t)

	for _, opts := range [][]string{nil, {"ro"}} {
		target := pmTarget(t, km)
		mark := tw.mark(t)
		err := km.MountExisting(t.Context(), dmDev, target, defaultFsType, opts)
		if err == nil || errors.Is(err, ErrNoFilesystem) || errors.Is(err, ErrFilesystemMismatch) {
			t.Fatalf("MountExisting over EIO region (opts=%v) = %v, want a readability error", opts, err)
		}
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("MountExisting over EIO region (opts=%v) = %v, want it to carry EIO", opts, err)
		}
		pmRequireUnmounted(t, target, "probe I/O error")
		tw.requireCalls(t, mark, false, true)
	}
	if got := pmDigest(t, orig.dev); got != orig.digest {
		t.Fatalf("probe I/O error changed the original device: digest %s, want %s", got, orig.digest)
	}
	pmRequireIdentity(t, orig.dev, orig.id)
}

// TestRealKernel_FormatAndMountUnchanged pins the ordinary (non-preserve)
// mount mode on the same kernel: a blank device is formatted and mounted
// writable, and a device that already carries ext4 keeps its filesystem and
// files.
func TestRealKernel_FormatAndMountUnchanged(t *testing.T) {
	pmRequire(t)
	km := NewKubeMounter()

	blank := pmNewLoop(t, "64M")
	target := pmTarget(t, km)
	if err := km.FormatAndMount(t.Context(), blank, target, defaultFsType, nil, nil); err != nil {
		t.Fatalf("FormatAndMount blank: %v", err)
	}
	if src, gotType, opts, ok := pmMountEntry(t, target); !ok || src != blank || gotType != defaultFsType ||
		!slices.Contains(opts, "rw") {
		t.Fatalf("mount entry of %s = (%q, %q, %v, %v), want (%q, ext4, rw)", target, src, gotType, opts, ok, blank)
	}
	if err := os.WriteFile(filepath.Join(target, "data"), []byte("formatted\n"), 0o600); err != nil {
		t.Fatalf("write to formatted volume: %v", err)
	}
	pmUnmount(t, km, target)
	if got := pmRun(t, "blkid", "-p", "-o", "value", "-s", "TYPE", blank); got != defaultFsType {
		t.Fatalf("blank device after FormatAndMount has type %q, want ext4", got)
	}

	orig := pmNewOriginal(t, km)
	target = pmTarget(t, km)
	if err := km.FormatAndMount(t.Context(), orig.dev, target, defaultFsType, nil, nil); err != nil {
		t.Fatalf("FormatAndMount existing: %v", err)
	}
	pmRequireFiles(t, target, orig.files)
	pmUnmount(t, km, target)
	if got, want := pmIdentityOf(t, orig.dev), orig.id; got.super["Filesystem UUID"] != want.super["Filesystem UUID"] ||
		got.super["Filesystem created"] != want.super["Filesystem created"] {
		t.Fatalf("FormatAndMount reformatted an existing filesystem: %v, want %v", got.super, want.super)
	}
}
