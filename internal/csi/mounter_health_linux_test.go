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

// Unit tests for KubeMounter.CheckMountHealth and KubeMounter.HasOtherMounts.
//
// CheckMountHealth issues a real O_TMPFILE syscall against t.TempDir() and
// against read-only mounts already present on the host, so it runs without
// root.  The kernel-shutdown errnos themselves (XFS forced shutdown → EIO,
// ext4 remount-ro → EROFS) were reproduced with xfs_io -x shutdown on a
// live VM; their mapping to ErrMountUnhealthy is pinned through the
// checkMountHealth fault-injection seam (injected probe syscalls).
// HasOtherMounts' pinning rule is checked against parsed mountinfo
// fixtures through hasOtherMounts.
//
// Run with:
//
//	go test ./internal/csi/ -v -run 'TestKubeMounter_(CheckMountHealth|HasOtherMounts)'

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestKubeMounter_CheckMountHealth_WritableDir verifies that the health
// probe reports a writable filesystem directory as healthy and leaves no
// files behind (O_TMPFILE primary path, CreateTemp fallback alike).
func TestKubeMounter_CheckMountHealth_WritableDir(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km := NewKubeMounter()

	if err := km.CheckMountHealth(target); err != nil {
		t.Fatalf("CheckMountHealth on writable dir: %v", err)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pillar-health-") {
			t.Errorf("health probe left file %q behind", e.Name())
		}
	}
}

// TestKubeMounter_CheckMountHealth_MissingDir verifies that a nonexistent
// target surfaces a plain probe error, not ErrMountUnhealthy: nothing about
// the filesystem's health can be concluded.
func TestKubeMounter_CheckMountHealth_MissingDir(t *testing.T) {
	t.Parallel()

	km := NewKubeMounter()
	err := km.CheckMountHealth(filepath.Join(t.TempDir(), "gone"))
	if err == nil {
		t.Fatal("CheckMountHealth on missing dir: expected error, got nil")
	}
	if errors.Is(err, ErrMountUnhealthy) {
		t.Error("missing dir must not be classified as a dead filesystem")
	}
}

// TestKubeMounter_CheckMountHealth_UnwritableDir verifies that a directory
// the probe cannot write (EACCES) is an inconclusive probe error — only
// EIO/EROFS map to ErrMountUnhealthy.
func TestKubeMounter_CheckMountHealth_UnwritableDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks are bypassed")
	}

	target := t.TempDir()
	if err := os.Chmod(target, 0o500); err != nil { //nolint:gosec // G302: a deliberately unwritable test fixture
		t.Fatalf("chmod: %v", err)
	}
	km := NewKubeMounter()

	err := km.CheckMountHealth(target)
	if err == nil {
		t.Fatal("CheckMountHealth on unwritable dir: expected error, got nil")
	}
	if errors.Is(err, ErrMountUnhealthy) {
		t.Error("EACCES probe failure must not be classified as a dead filesystem")
	}
}

// TestKubeMounter_HasOtherMounts_NotMountPoint verifies that probing a
// path that is not a mount point is an error, never a silent false: a
// failed check must not let callers drop a mount.
func TestKubeMounter_HasOtherMounts_NotMountPoint(t *testing.T) {
	t.Parallel()

	km := NewKubeMounter()
	others, err := km.HasOtherMounts(t.TempDir())
	if err == nil {
		t.Fatal("HasOtherMounts on a non-mount path: expected error, got nil")
	}
	if others {
		t.Error("non-mount path reported as having other mounts")
	}
}

// probeErrno is a syscall failure in the *os.PathError form os.CreateTemp
// returns, for feeding the injected checkMountHealth probe syscalls.
func probeErrno(errno unix.Errno) error {
	return &os.PathError{Op: "open", Path: "/probe", Err: errno}
}

// TestKubeMounter_CheckMountHealth_ShutdownErrno pins the errno → verdict
// mapping on both probe paths: the primary O_TMPFILE open and the
// named-file fallback taken when O_TMPFILE is unsupported (EOPNOTSUPP,
// EISDIR).  EIO (XFS forced shutdown) and EROFS (ext4 remount-ro abort)
// must map to ErrMountUnhealthy; any other failure is inconclusive and
// must surface as a plain probe error, never as a dead filesystem.
func TestKubeMounter_CheckMountHealth_ShutdownErrno(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		openErr       unix.Errno
		createErr     unix.Errno // 0: fallback not taken
		wantUnhealthy bool
	}{
		{name: "O_TMPFILE EIO", openErr: unix.EIO, wantUnhealthy: true},
		{name: "O_TMPFILE EROFS", openErr: unix.EROFS, wantUnhealthy: true},
		{name: "O_TMPFILE EACCES", openErr: unix.EACCES},
		{name: "fallback EIO", openErr: unix.EOPNOTSUPP, createErr: unix.EIO, wantUnhealthy: true},
		{name: "fallback EROFS", openErr: unix.EISDIR, createErr: unix.EROFS, wantUnhealthy: true},
		{name: "fallback EACCES", openErr: unix.EOPNOTSUPP, createErr: unix.EACCES},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			createCalls := 0
			open := func(string) (int, error) { return -1, probeErrno(tc.openErr) }
			create := func(string) (string, error) {
				createCalls++
				return "", probeErrno(tc.createErr)
			}

			err := checkMountHealth(t.TempDir(), open, create)
			if err == nil {
				t.Fatal("CheckMountHealth: expected an error, got nil")
			}
			if got := errors.Is(err, ErrMountUnhealthy); got != tc.wantUnhealthy {
				t.Errorf("errors.Is(err, ErrMountUnhealthy) = %v, want %v (err = %v)", got, tc.wantUnhealthy, err)
			}
			wantErrno := tc.openErr
			wantCreateCalls := 0
			if tc.createErr != 0 {
				wantErrno, wantCreateCalls = tc.createErr, 1
			}
			if !errors.Is(err, wantErrno) {
				t.Errorf("error %v does not wrap %v", err, wantErrno)
			}
			if createCalls != wantCreateCalls {
				t.Errorf("fallback probe called %d times, want %d", createCalls, wantCreateCalls)
			}
		})
	}
}

// TestKubeMounter_CheckMountHealth_ReadonlyFilesystem probes real
// filesystems the kernel mounted read-only, end to end through the
// unmodified probe: a metadata write that the kernel rejects with EROFS
// must surface as ErrMountUnhealthy — the verdict an ext4 remount-ro
// shutdown produces, and the reason callers never probe mounts that are
// read-only by request.  Needs no root: the "ro" mounts already present
// in /proc/self/mountinfo serve as targets.  Mounts whose probe fails
// with another errno (ENOTDIR on a bind-mounted file, EACCES) are
// inconclusive and ignored; the test skips when no probe reaches EROFS.
func TestKubeMounter_CheckMountHealth_ReadonlyFilesystem(t *testing.T) {
	t.Parallel()

	f, err := os.Open(procMountInfoPath)
	if err != nil {
		t.Skipf("cannot open %s: %v", procMountInfoPath, err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only file

	// Only the topmost mount at a path answers a probe of it, so a later
	// entry stacked on the same mount point overrides an earlier one.
	topReadOnly := make(map[string]bool)
	var order []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		mountPoint := unescapeMountInfo(fields[4])
		if _, seen := topReadOnly[mountPoint]; !seen {
			order = append(order, mountPoint)
		}
		// Per-mount options are the sixth field, before the "-" separator.
		topReadOnly[mountPoint] = slices.Contains(strings.Split(fields[5], ","), "ro")
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", procMountInfoPath, err)
	}

	km := NewKubeMounter()
	probed := 0
	for _, mountPoint := range order {
		if !topReadOnly[mountPoint] {
			continue
		}
		err := km.CheckMountHealth(mountPoint)
		if !errors.Is(err, unix.EROFS) {
			continue
		}
		probed++
		if !errors.Is(err, ErrMountUnhealthy) {
			t.Errorf("CheckMountHealth(%q) = %v: EROFS must map to ErrMountUnhealthy", mountPoint, err)
		}
	}
	if probed == 0 {
		t.Skip("no read-only mount in /proc/self/mountinfo answered the probe with EROFS")
	}
	t.Logf("%d read-only mount(s) answered EROFS and were classified as dead", probed)
}

// TestKubeMounter_HasOtherMounts_SubdirBindCountsAsPinning verifies that a
// bind mount of a subdirectory of the staged filesystem — mountinfo root
// field "/subdir" rather than "/" — counts as pinning the staged
// superblock.  Kubelet binds only the volume's subdirectory inside a
// shared staging filesystem, so the pinning test must look at the shared
// device number, not at identical mount roots or mount points.
func TestKubeMounter_HasOtherMounts_SubdirBindCountsAsPinning(t *testing.T) {
	t.Parallel()

	const staging = "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/pvc-1/globalmount"
	cases := []struct {
		name      string
		mountInfo string
		want      bool
	}{
		{
			name: "subdirectory bind of the staged device",
			mountInfo: "" +
				"40 30 8:17 / " + staging + " rw,relatime - ext4 /dev/sdb1 rw\n" +
				"55 40 8:17 /subdir /pod/volumes/vol rw,relatime - ext4 /dev/sdb1 rw\n",
			want: true,
		},
		{
			name: "whole-filesystem bind of the staged device",
			mountInfo: "" +
				"40 30 8:17 / " + staging + " rw,relatime - ext4 /dev/sdb1 rw\n" +
				"55 40 8:17 / /pod/volumes/vol rw,relatime - ext4 /dev/sdb1 rw\n",
			want: true,
		},
		{
			name: "mount of a different device at another path",
			mountInfo: "" +
				"40 30 8:17 / " + staging + " rw,relatime - ext4 /dev/sdb1 rw\n" +
				"55 40 0:55 / /pod/volumes/other rw,relatime - tmpfs tmpfs rw\n",
			want: false,
		},
		{
			name: "only the staged mount",
			mountInfo: "" +
				"40 30 8:17 / " + staging + " rw,relatime - ext4 /dev/sdb1 rw\n",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mountsFile := filepath.Join(t.TempDir(), "mountinfo")
			if err := os.WriteFile(mountsFile, []byte(tc.mountInfo), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			mounts, err := readMountInfoFile(mountsFile)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}

			got, err := hasOtherMounts(mounts, staging)
			if err != nil {
				t.Fatalf("HasOtherMounts: %v", err)
			}
			if got != tc.want {
				t.Errorf("HasOtherMounts = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestKubeMounter_MountEntryExists_SymlinkedParent pins the mount-table
// lookups against a kubelet root reached through a symlink: mountinfo
// records resolved mount points, so /link/proc must still find the /proc
// mount (a stat-based check follows symlinks; a literal mountinfo match
// would report a live mount as absent and let NodeStageVolume mount twice).
// /proc is a mount point on every Linux host, so no root is needed.
func TestKubeMounter_MountEntryExists_SymlinkedParent(t *testing.T) {
	t.Parallel()

	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	km := NewKubeMounter()

	ok, err := km.MountEntryExists(filepath.Join(link, "proc"))
	if err != nil || !ok {
		t.Fatalf("MountEntryExists(<symlink to />/proc) = %v, %v; want true", ok, err)
	}
	source, err := km.MountSource(filepath.Join(link, "proc"))
	if err != nil || source != "proc" {
		t.Errorf("MountSource(<symlink to />/proc) = %q, %v; want \"proc\"", source, err)
	}
	ok, err = km.MountEntryExists(filepath.Join(link, "proc", "no-such-mount"))
	if err != nil || ok {
		t.Errorf("MountEntryExists of a non-mount under the link = %v, %v; want false", ok, err)
	}
}
