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
// device: blkid reports it blank until an mkfs call succeeds, after which it
// reports the new filesystem.

import (
	"errors"
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
	cmdh *testingexec.FakeCmd
}

// fakeDeviceExec scripts blkid / mkfs.* / fsck for a single device.
type fakeDeviceExec struct {
	fsType  string // current filesystem on the device; "" = blank
	mkfsErr error  // returned by mkfs.* when non-nil
	calls   []*execCall
}

func (f *fakeDeviceExec) exec() *testingexec.FakeExec {
	fe := &testingexec.FakeExec{}
	for range 16 {
		fe.CommandScript = append(fe.CommandScript, func(cmd string, args ...string) utilexec.Cmd {
			call := &execCall{cmd: cmd, args: slices.Clone(args), cmdh: &testingexec.FakeCmd{}}
			f.calls = append(f.calls, call)
			call.cmdh.CombinedOutputScript = []testingexec.FakeAction{func() ([]byte, []byte, error) {
				return f.run(cmd)
			}}
			return testingexec.InitFakeCmd(call.cmdh, cmd, args...)
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
		f.fsType = strings.TrimPrefix(cmd, "mkfs.")
		return nil, nil, nil
	default: // fsck
		return nil, nil, nil
	}
}

func (f *fakeDeviceExec) mkfsCalls() []*execCall {
	var out []*execCall
	for _, c := range f.calls {
		if strings.HasPrefix(c.cmd, "mkfs.") {
			out = append(out, c)
		}
	}
	return out
}

func newFormatTestMounter(dev *fakeDeviceExec) (*KubeMounter, *mount.FakeMounter) {
	fake := mount.NewFakeMounter(nil)
	return &KubeMounter{inner: mount.SafeFormatAndMount{Interface: fake, Exec: dev.exec()}}, fake
}

// TestKubeMounter_FormatAndMount_BlankDeviceUsesMkfsOptions verifies that a
// blank device is formatted exactly once with the default arguments followed
// by the configured mkfs options, from an isolated working directory, and is
// then mounted with the requested type.
func TestKubeMounter_FormatAndMount_BlankDeviceUsesMkfsOptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fsType string
		opts   []string
		want   []string
	}{
		{"ext4", []string{"-E", "lazy_itable_init=0", "-m", "1"},
			[]string{"-F", "-m0", "-E", "lazy_itable_init=0", "-m", "1", fakeDevice}},
		{"xfs", []string{"-m", "reflink=1", "-L", "data vol"},
			[]string{"-m", "reflink=1", "-L", "data vol", fakeDevice}},
		{"ext4", nil, []string{"-F", "-m0", fakeDevice}},
	} {
		t.Run(tc.fsType+"/"+strings.Join(tc.opts, ","), func(t *testing.T) {
			t.Parallel()
			dev := &fakeDeviceExec{}
			km, fake := newFormatTestMounter(dev)
			target := t.TempDir()

			err := km.FormatAndMount(fakeDevice, target, tc.fsType, nil, tc.opts)
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
			if dirs := mkfs[0].cmdh.Dirs; len(dirs) != 1 || dirs[0] == "" || dirs[0] == "/" {
				t.Errorf("mkfs working directory = %q, want one isolated scratch directory", dirs)
			}
			if len(fake.MountPoints) != 1 || fake.MountPoints[0].Type != tc.fsType {
				t.Errorf("mount table = %+v, want one %s mount", fake.MountPoints, tc.fsType)
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
	km, fake := newFormatTestMounter(dev)

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
	km, fake := newFormatTestMounter(dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "ext4", []string{"ro"}, []string{"-K"})
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
	km, fake := newFormatTestMounter(dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "xfs", nil, []string{"-b", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "mkfs: bad option") {
		t.Fatalf("FormatAndMount error = %v, want the mkfs failure with its output", err)
	}
	if len(fake.MountPoints) != 0 {
		t.Errorf("mount table = %+v, want nothing mounted", fake.MountPoints)
	}
}

// TestKubeMounter_FormatAndMount_RejectsUnsafeOptions verifies that the
// mounter itself refuses an mkfs option that references another file or
// device, without running any command.
func TestKubeMounter_FormatAndMount_RejectsUnsafeOptions(t *testing.T) {
	t.Parallel()
	dev := &fakeDeviceExec{}
	km, _ := newFormatTestMounter(dev)

	err := km.FormatAndMount(fakeDevice, t.TempDir(), "xfs", nil, []string{"-l", "logdev=/dev/sda"})
	if err == nil {
		t.Fatal("FormatAndMount with a device-path mkfs option: want error, got nil")
	}
	if len(dev.calls) != 0 {
		t.Errorf("commands run = %d, want 0", len(dev.calls))
	}
}
