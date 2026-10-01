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

package nvmeof

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Shared-port maximum data transfer size contract: param_mdts belongs to the
// port every volume on the same address and port shares, Linux accepts it
// only while no subsystem is linked to the port, and kernels before 7.1 do
// not have it at all.

const fourMiB = 4 << 20

func mdtsTarget(root, name string, size *int32) *NvmetTarget {
	tgt := orderingTarget(root, name)
	tgt.MaxDataTransferSize = size
	return tgt
}

func mdtsPath(t *NvmetTarget) string {
	return filepath.Join(t.portDir(stablePortID(t.BindAddress, t.Port)), portMDTSAttr)
}

// withMDTSAttr emulates a Linux >= 7.1 port: the kernel creates param_mdts
// with every port directory.
func withMDTSAttr(t *testing.T, tgt *NvmetTarget, value string) {
	t.Helper()
	path := mdtsPath(tgt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestApply_MDTSWrittenOnIdlePort verifies that a port without linked
// subsystems is set to the requested limit as a param_mdts exponent of 4 KiB
// pages, and to no limit when the export requires none or accepts any value,
// so a value left on the port is not inherited.
func TestApply_MDTSWrittenOnIdlePort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		size    *int32
		initial string
		want    string
	}{
		{name: "4MiB", size: int32Ptr(fourMiB), initial: "0", want: "10"},
		{name: "8KiB", size: int32Ptr(MinMaxDataTransferSize), initial: "0", want: "1"},
		{name: "1GiB", size: int32Ptr(MaxMaxDataTransferSize), initial: "0", want: "18"},
		{name: "no limit", size: int32Ptr(0), initial: "10", want: "0"},
		{name: "unset resets leftover", size: nil, initial: "10", want: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tgt := mdtsTarget(t.TempDir(), "idle", tc.size)
			withMDTSAttr(t, tgt, tc.initial)

			if err := tgt.Apply(); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			assertFileContent(t, mdtsPath(tgt), tc.want)
			if !portLinked(tgt) {
				t.Fatal("subsystem not linked")
			}
		})
	}
}

// TestApply_MDTSMissingAttributeIsReported verifies that a kernel without
// param_mdts (Linux < 7.1) never fails the export and gets no attribute
// created, and that only a requested limit is reported as unsupported.
func TestApply_MDTSMissingAttributeIsReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		size     *int32
		reported bool
	}{
		{name: "limit", size: int32Ptr(fourMiB), reported: true},
		{name: "no limit", size: int32Ptr(0)},
		{name: "unset", size: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tgt := mdtsTarget(t.TempDir(), "old-kernel", tc.size)
			var reported []string
			tgt.MDTSUnsupported = func(port string) { reported = append(reported, port) }

			if err := tgt.Apply(); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !portLinked(tgt) {
				t.Fatal("subsystem not linked")
			}
			if _, err := os.Lstat(mdtsPath(tgt)); !os.IsNotExist(err) {
				t.Fatalf("param_mdts created on a kernel without it: %v", err)
			}
			var want []string
			if tc.reported {
				want = []string{"10.0.0.1:4420"}
			}
			if !slices.Equal(reported, want) {
				t.Fatalf("MDTSUnsupported calls = %q, want %q", reported, want)
			}
		})
	}
}

// TestApply_MDTSConflictOnActivePort verifies that an export requiring a
// limit other than the one its already active port advertises fails with
// ErrPortMDTSConflict and is not linked, while an export requiring the same
// limit, or none, joins the port unchanged.
func TestApply_MDTSConflictOnActivePort(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := mdtsTarget(root, "a", int32Ptr(fourMiB))
	withMDTSAttr(t, first, "0")
	if err := first.Apply(); err != nil {
		t.Fatalf("Apply first: %v", err)
	}

	for _, size := range []int32{2 * fourMiB, 0} {
		conflicting := mdtsTarget(root, "b", int32Ptr(size))
		err := conflicting.Apply()
		if !errors.Is(err, ErrPortMDTSConflict) {
			t.Fatalf("Apply requiring %d = %v, want ErrPortMDTSConflict", size, err)
		}
		if portLinked(conflicting) {
			t.Fatalf("export requiring %d was linked to the port", size)
		}
	}
	if !portLinked(first) {
		t.Fatal("first export lost its port link")
	}
	assertFileContent(t, mdtsPath(first), "10")

	for _, tgt := range []*NvmetTarget{
		mdtsTarget(root, "same", int32Ptr(fourMiB)),
		mdtsTarget(root, "unset", nil),
	} {
		if err := tgt.Apply(); err != nil {
			t.Fatalf("Apply %s: %v", tgt.SubsystemNQN, err)
		}
		if !portLinked(tgt) {
			t.Fatalf("%s not linked", tgt.SubsystemNQN)
		}
	}
	assertFileContent(t, mdtsPath(first), "10")
}

// TestApply_MDTSLimitJoinsUnlimitedActivePort verifies that a limit requested
// on an active port advertising no limit is accepted without a write: hosts
// see MDTS 0 and the node caps the device itself, so volumes provisioned
// after an upgrade are not rejected on a port older volumes keep enabled.
func TestApply_MDTSLimitJoinsUnlimitedActivePort(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	old := mdtsTarget(root, "old", nil)
	withMDTSAttr(t, old, "0")
	if err := old.Apply(); err != nil {
		t.Fatalf("Apply old: %v", err)
	}

	limited := mdtsTarget(root, "limited", int32Ptr(fourMiB))
	if err := limited.Apply(); err != nil {
		t.Fatalf("Apply limited: %v", err)
	}
	if !portLinked(limited) {
		t.Fatal("limited export not linked")
	}
	assertFileContent(t, mdtsPath(old), "0")
}

// TestRemove_PrunesPortWithMDTS verifies the last export's removal still
// prunes a port that carries param_mdts.
func TestRemove_PrunesPortWithMDTS(t *testing.T) {
	t.Parallel()
	tgt := mdtsTarget(t.TempDir(), "prune", int32Ptr(fourMiB))
	withMDTSAttr(t, tgt, "0")
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(mdtsPath(tgt))); !os.IsNotExist(err) {
		t.Fatalf("port directory not pruned: %v", err)
	}
}

// TestApply_RejectsInvalidMDTS verifies that a size param_mdts cannot
// express, i.e. not 0 and not a power of two from 8 KiB to 1 GiB, is
// rejected before any configfs change.  4 KiB is one page, exponent 0, which
// would advertise no limit instead.
func TestApply_RejectsInvalidMDTS(t *testing.T) {
	t.Parallel()
	for _, size := range []int32{-1, 2048, 4096, 5000, 3 << 20} {
		tgt := mdtsTarget(t.TempDir(), "invalid", int32Ptr(size))
		withMDTSAttr(t, tgt, "0")
		if _, err := tgt.Prepare(); err == nil {
			t.Errorf("Prepare accepted MaxDataTransferSize %d", size)
		}
		if err := tgt.Apply(); err == nil {
			t.Errorf("Apply accepted MaxDataTransferSize %d", size)
		}
		if _, err := os.Stat(tgt.subsystemDir()); !os.IsNotExist(err) {
			t.Errorf("subsystem created for MaxDataTransferSize %d: %v", size, err)
		}
		assertFileContent(t, mdtsPath(tgt), "0")
	}
}
