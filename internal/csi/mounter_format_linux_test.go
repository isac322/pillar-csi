//go:build linux

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

// Tests for KubeMounter.FormatAndMount with mkfs options (issue #115).  The
// host tools are replaced by a scripted fake exec that models one block
// device: blkid reports it blank until an mkfs call creates a filesystem,
// after which it reports that filesystem.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	utilexec "k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
	"k8s.io/utils/mount"
)

const fakeDevice = "/dev/nvme0n1"

// execCall is one recorded command of fakeDeviceExec.
type execCall struct {
	cmd  string
	args []string
}

// fakeDeviceExec scripts blkid / mkfs.* / fsck for a single device.
type fakeDeviceExec struct {
	fsType   string // current filesystem on the device; "" = blank
	mkfsErr  error  // returned by mkfs.* when non-nil
	mkfsNoop bool   // mkfs.* exits 0 without creating a filesystem
	calls    []execCall
}

func (f *fakeDeviceExec) exec() *testingexec.FakeExec {
	fe := &testingexec.FakeExec{}
	for range 16 {
		fe.CommandScript = append(fe.CommandScript, func(cmd string, args ...string) utilexec.Cmd {
			f.calls = append(f.calls, execCall{cmd: cmd, args: slices.Clone(args)})
			fc := &testingexec.FakeCmd{CombinedOutputScript: []testingexec.FakeAction{
				func() ([]byte, []byte, error) { return f.run(cmd) },
			}}
			return testingexec.InitFakeCmd(fc, cmd, args...)
		})
	}
	return fe
}

func (f *fakeDeviceExec) run(cmd string) (stdout, stderr []byte, err error) {
	switch {
	case cmd == "blkid":
		if f.fsType == "" {
			return nil, nil, testingexec.FakeExitError{Status: 2}
		}
		return []byte("DEVNAME=" + fakeDevice + "\nTYPE=" + f.fsType + "\n"), nil, nil
	case strings.HasPrefix(cmd, "mkfs."):
		if f.mkfsErr != nil {
			return []byte("mkfs: bad option"), nil, f.mkfsErr
		}
		if !f.mkfsNoop {
			f.fsType = strings.TrimPrefix(cmd, "mkfs.")
		}
		return nil, nil, nil
	default: // fsck
		return nil, nil, nil
	}
}

func (f *fakeDeviceExec) mkfsCalls() []execCall {
	var out []execCall
	for _, c := range f.calls {
		if strings.HasPrefix(c.cmd, "mkfs.") {
			out = append(out, c)
		}
	}
	return out
}

// xfsLTS515Profile is mkfs/lts_5.15.conf of xfsprogs 7.0.1, the profile the
// node image ships at xfsCompatProfile.
const xfsLTS515Profile = `# V5 features that were the mkfs defaults when the upstream Linux 5.15 LTS
# kernel was released at the end of 2021.

[metadata]
bigtime=1
crc=1
finobt=1
inobtcount=1
metadir=0
reflink=1
rmapbt=0
autofsck=0

[inode]
sparse=1
nrext64=0
exchange=0

[naming]
parent=0
`

// writeXFSProfile writes an mkfs.xfs configuration file and returns its path.
func writeXFSProfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lts.conf")
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write xfs profile: %v", err)
	}
	return path
}

func newFormatTestMounter(t *testing.T, dev *fakeDeviceExec) (*KubeMounter, *mount.FakeMounter) {
	t.Helper()
	fake := mount.NewFakeMounter(nil)
	return &KubeMounter{
		inner:      mount.SafeFormatAndMount{Interface: fake, Exec: dev.exec()},
		xfsProfile: writeXFSProfile(t, xfsLTS515Profile),
	}, fake
}

// TestKubeMounter_FormatAndMount_BlankDeviceUsesMkfsOptions verifies that a
// blank device is formatted exactly once with the default arguments followed
// by the configured mkfs options, and is then mounted with the requested type.
// The xfs defaults are the Linux 5.15 LTS profile (issue #133) minus every
// key the configured options set, since mkfs.xfs rejects a respecified key.
func TestKubeMounter_FormatAndMount_BlankDeviceUsesMkfsOptions(t *testing.T) {
	t.Parallel()
	const (
		ltsMetadata = "bigtime=1,crc=1,finobt=1,inobtcount=1,metadir=0,reflink=1,rmapbt=0,autofsck=0"
		ltsInode    = "sparse=1,nrext64=0,exchange=0"
	)
	for _, tc := range []struct {
		fsType string
		opts   []string
		want   []string
	}{
		{"ext4", []string{"-E", "lazy_itable_init=0", "-m", "1"},
			[]string{"-F", "-m0", "-E", "lazy_itable_init=0", "-m", "1", fakeDevice}},
		{"ext4", nil, []string{"-F", "-m0", fakeDevice}},
		{"xfs", nil,
			[]string{"-m", ltsMetadata, "-i", ltsInode, "-n", "parent=0", fakeDevice}},
		{"xfs", []string{"-m", "reflink=0", "-L", "data vol"},
			[]string{"-m", "bigtime=1,crc=1,finobt=1,inobtcount=1,metadir=0,rmapbt=0,autofsck=0",
				"-i", ltsInode, "-n", "parent=0", "-m", "reflink=0", "-L", "data vol", fakeDevice}},
		// Opting in to newer features, attached and separate value forms.
		{"xfs", []string{"-iexchange=1,nrext64=1", "-n", "parent=1", "-mrmapbt=1"},
			[]string{"-m", "bigtime=1,crc=1,finobt=1,inobtcount=1,metadir=0,reflink=1,autofsck=0",
				"-i", "sparse=1", "-iexchange=1,nrext64=1", "-n", "parent=1", "-mrmapbt=1", fakeDevice}},
		// Options outside the profile leave it whole.
		{"xfs", []string{"-K", "-i", "size=512", "-n", "size=8192"},
			[]string{"-m", ltsMetadata, "-i", ltsInode, "-n", "parent=0",
				"-K", "-i", "size=512", "-n", "size=8192", fakeDevice}},
	} {
		t.Run(tc.fsType+"/"+strings.Join(tc.opts, ","), func(t *testing.T) {
			t.Parallel()
			dev := &fakeDeviceExec{}
			km, fake := newFormatTestMounter(t, dev)

			err := km.FormatAndMount(fakeDevice, t.TempDir(), tc.fsType, nil, tc.opts)
			if err != nil {
				t.Fatalf("FormatAndMount: %v", err)
			}
			mkfs := dev.mkfsCalls()
			if len(mkfs) != 1 {
				t.Fatalf("mkfs calls = %d, want 1", len(mkfs))
			}
			if mkfs[0].cmd != "mkfs."+tc.fsType || !slices.Equal(mkfs[0].args, tc.want) {
				t.Errorf("mkfs = %s %q, want mkfs.%s %q", mkfs[0].cmd, mkfs[0].args, tc.fsType, tc.want)
			}
			if len(fake.MountPoints) != 1 || fake.MountPoints[0].Type != tc.fsType {
				t.Errorf("mount table = %+v, want one %s mount", fake.MountPoints, tc.fsType)
			}
		})
	}
}

// TestKubeMounter_FormatAndMount_XFSProfileUnusable verifies that a blank
// device is not formatted as xfs when the compatibility profile is missing or
// unreadable: formatting with the mkfs.xfs defaults could create a filesystem
// that older supported kernels refuse to mount.
func TestKubeMounter_FormatAndMount_XFSProfileUnusable(t *testing.T) {
	t.Parallel()
	for name, profile := range map[string]string{
		"missing":                  "",
		"unknown section":          "[proto]\nslashes_are_spaces=1\n",
		"option outside a section": "crc=1\n",
		"option without value":     "[metadata]\ncrc\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dev := &fakeDeviceExec{}
			km, fake := newFormatTestMounter(t, dev)
			km.xfsProfile = filepath.Join(t.TempDir(), "absent.conf")
			if profile != "" {
				km.xfsProfile = writeXFSProfile(t, profile)
			}

			err := km.FormatAndMount(fakeDevice, t.TempDir(), "xfs", nil, nil)
			if err == nil || !strings.Contains(err.Error(), "compatibility profile") {
				t.Fatalf("FormatAndMount error = %v, want a compatibility profile error", err)
			}
			if n := len(dev.mkfsCalls()); n != 0 || len(fake.MountPoints) != 0 {
				t.Errorf("mkfs calls = %d, mounts = %d, want none", n, len(fake.MountPoints))
			}
		})
	}
}

// TestKubeMounter_FormatAndMount_ExistingFilesystemNeverReformatted verifies
// that a device that already carries a filesystem is mounted without any mkfs
// call even when mkfs options are configured.
func TestKubeMounter_FormatAndMount_ExistingFilesystemNeverReformatted(t *testing.T) {
	t.Parallel()
	dev := &fakeDeviceExec{fsType: "ext4"}
	km, fake := newFormatTestMounter(t, dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "ext4", nil, []string{"-E", "lazy_itable_init=0"})
	if err != nil {
		t.Fatalf("FormatAndMount: %v", err)
	}
	if n := len(dev.mkfsCalls()); n != 0 {
		t.Fatalf("mkfs calls = %d, want 0 for a formatted device", n)
	}
	if len(fake.MountPoints) != 1 {
		t.Errorf("mount table = %+v, want the existing filesystem mounted", fake.MountPoints)
	}
}

// TestKubeMounter_FormatAndMount_ReadOnlyBlankNotFormatted verifies that a
// read-only mount of a blank device neither formats it nor mounts it.
func TestKubeMounter_FormatAndMount_ReadOnlyBlankNotFormatted(t *testing.T) {
	t.Parallel()
	dev := &fakeDeviceExec{}
	km, fake := newFormatTestMounter(t, dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "xfs", []string{"ro"}, []string{"-K"})
	if err == nil {
		t.Fatal("FormatAndMount of a blank device read-only: want error, got nil")
	}
	if n := len(dev.mkfsCalls()); n != 0 || len(fake.MountPoints) != 0 {
		t.Fatalf("mkfs calls = %d, mounts = %d, want none", n, len(fake.MountPoints))
	}
}

// TestKubeMounter_FormatAndMount_MkfsFailure verifies that an mkfs failure is
// returned with the tool output and nothing is mounted.
func TestKubeMounter_FormatAndMount_MkfsFailure(t *testing.T) {
	t.Parallel()
	dev := &fakeDeviceExec{mkfsErr: errors.New("exit status 1")}
	km, fake := newFormatTestMounter(t, dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "xfs", nil, []string{"-b", "size=3"})
	if err == nil || !strings.Contains(err.Error(), "mkfs: bad option") {
		t.Fatalf("FormatAndMount error = %v, want the mkfs failure with its output", err)
	}
	if len(fake.MountPoints) != 0 {
		t.Errorf("mount table = %+v, want nothing mounted", fake.MountPoints)
	}
}

// TestKubeMounter_FormatAndMount_MkfsLeavesDeviceBlank verifies that an mkfs
// run that exits 0 without creating the filesystem fails the call instead of
// letting SafeFormatAndMount format the device again without the options.
func TestKubeMounter_FormatAndMount_MkfsLeavesDeviceBlank(t *testing.T) {
	t.Parallel()
	dev := &fakeDeviceExec{mkfsNoop: true}
	km, fake := newFormatTestMounter(t, dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "ext4", nil, []string{"-L", "data"})
	if err == nil {
		t.Fatal("FormatAndMount after a no-op mkfs: want error, got nil")
	}
	if n := len(dev.mkfsCalls()); n != 1 {
		t.Errorf("mkfs calls = %d, want exactly the configured one", n)
	}
	if len(fake.MountPoints) != 0 {
		t.Errorf("mount table = %+v, want nothing mounted", fake.MountPoints)
	}
}

// TestKubeMounter_FormatAndMount_RejectsUnsafeOptions verifies that the
// mounter itself refuses an mkfs option outside the allowlist without
// running any command.
func TestKubeMounter_FormatAndMount_RejectsUnsafeOptions(t *testing.T) {
	t.Parallel()
	dev := &fakeDeviceExec{}
	km, _ := newFormatTestMounter(t, dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "ext4", nil, []string{"-J", "device=LABEL=journal"})
	if err == nil {
		t.Fatal("FormatAndMount with an external-journal mkfs option: want error, got nil")
	}
	if len(dev.calls) != 0 {
		t.Errorf("commands run = %d, want 0", len(dev.calls))
	}
}
