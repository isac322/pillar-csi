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
	"testing"
)

// Shared-port in-capsule data size contract: param_inline_data_size belongs
// to the port every volume on the same address and port shares, and Linux
// accepts it only while no subsystem is linked to the port.

func inlineTarget(root, name string, size *int32) *NvmetTarget {
	tgt := orderingTarget(root, name)
	tgt.InlineDataSize = size
	return tgt
}

func inlineSizePath(t *NvmetTarget) string {
	return filepath.Join(t.portDir(stablePortID(t.BindAddress, t.Port)), portInlineDataSizeAttr)
}

func int32Ptr(v int32) *int32 { return new(v) }

func TestApply_InlineDataSizeWrittenOnFirstExport(t *testing.T) {
	t.Parallel()
	tgt := inlineTarget(t.TempDir(), "first", int32Ptr(8192))

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertFileContent(t, inlineSizePath(tgt), "8192")
	if !portLinked(tgt) {
		t.Fatal("subsystem not linked")
	}
}

// TestApply_InlineDataSizeExplicitZero verifies 0 (no in-capsule data) is
// applied as a value, not treated as "unset".
func TestApply_InlineDataSizeExplicitZero(t *testing.T) {
	t.Parallel()
	tgt := inlineTarget(t.TempDir(), "zero", int32Ptr(0))

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertFileContent(t, inlineSizePath(tgt), "0")
}

// TestApply_InlineDataSizeConflictOnActivePort verifies that an export
// requiring a value other than the one of its already active port fails
// with ErrPortInlineDataSizeConflict and is not linked, while an export
// requiring the same value, or none, joins the port unchanged.
func TestApply_InlineDataSizeConflictOnActivePort(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := inlineTarget(root, "a", int32Ptr(8192))
	if err := first.Apply(); err != nil {
		t.Fatalf("Apply first: %v", err)
	}

	conflicting := inlineTarget(root, "b", int32Ptr(4096))
	err := conflicting.Apply()
	if !errors.Is(err, ErrPortInlineDataSizeConflict) {
		t.Fatalf("Apply conflicting = %v, want ErrPortInlineDataSizeConflict", err)
	}
	if portLinked(conflicting) {
		t.Fatal("conflicting export was linked to the port")
	}
	if !portLinked(first) {
		t.Fatal("first export lost its port link")
	}
	assertFileContent(t, inlineSizePath(first), "8192")

	for _, tgt := range []*NvmetTarget{
		inlineTarget(root, "same", int32Ptr(8192)),
		inlineTarget(root, "unset", nil),
	} {
		if err := tgt.Apply(); err != nil {
			t.Fatalf("Apply %s: %v", tgt.SubsystemNQN, err)
		}
		if !portLinked(tgt) {
			t.Fatalf("%s not linked", tgt.SubsystemNQN)
		}
	}
	assertFileContent(t, inlineSizePath(first), "8192")
}

// TestApply_InlineDataSizeUnsetResetsIdlePort verifies that an export
// without a requested value does not inherit a value left on a port without
// linked subsystems: the port is reset to the transport default.
func TestApply_InlineDataSizeUnsetResetsIdlePort(t *testing.T) {
	t.Parallel()
	tgt := inlineTarget(t.TempDir(), "idle", nil)
	path := inlineSizePath(tgt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("8192\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertFileContent(t, path, transportDefaultInlineDataSize)
}

// TestApply_InlineDataSizeUnsetLeavesFreshPort verifies that an export
// without a requested value writes nothing to a port that holds no value.
func TestApply_InlineDataSizeUnsetLeavesFreshPort(t *testing.T) {
	t.Parallel()
	tgt := inlineTarget(t.TempDir(), "fresh", nil)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(inlineSizePath(tgt)); !os.IsNotExist(err) {
		t.Fatalf("param_inline_data_size written for an export without a value: %v", err)
	}
}

// TestRemove_PrunesPortWithInlineDataSize verifies the last export's removal
// still prunes a port that carries param_inline_data_size.
func TestRemove_PrunesPortWithInlineDataSize(t *testing.T) {
	t.Parallel()
	tgt := inlineTarget(t.TempDir(), "prune", int32Ptr(8192))
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(inlineSizePath(tgt))); !os.IsNotExist(err) {
		t.Fatalf("port directory not pruned: %v", err)
	}
}

func TestPrepare_RejectsNegativeInlineDataSize(t *testing.T) {
	t.Parallel()
	tgt := inlineTarget(t.TempDir(), "negative", int32Ptr(-1))

	if _, err := tgt.Prepare(); err == nil {
		t.Fatal("Prepare accepted a negative InlineDataSize")
	}
}
