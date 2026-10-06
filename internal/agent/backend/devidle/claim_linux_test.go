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

package devidle_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/backend/devidle"
)

// A missing device is an open error, never a claim of nothing: callers
// must not treat an absent path as an idle device.
func TestClaimDevice_MissingPathIsAnError(t *testing.T) {
	t.Parallel()
	_, err := devidle.ClaimDevice(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, syscall.ENOENT) || errors.Is(err, devidle.ErrDeviceHeld) {
		t.Fatalf("ClaimDevice = %v, want ENOENT (not ErrDeviceHeld)", err)
	}
}

// The claim reports the held descriptor's st_rdev and releases once.
func TestClaimDevice_ReportsHeldRdev(t *testing.T) {
	t.Parallel()
	claim, err := devidle.ClaimDevice("/dev/null")
	if err != nil {
		t.Fatalf("ClaimDevice(/dev/null): %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat("/dev/null", &st); err != nil {
		t.Fatal(err)
	}
	if claim.Rdev() != st.Rdev {
		t.Fatalf("Rdev = %#x, want %#x", claim.Rdev(), st.Rdev)
	}
	if major, minor := devidle.DecodeLinuxDev(claim.Rdev()); major != 1 || minor != 3 {
		t.Fatalf("/dev/null decodes to %d:%d, want 1:3", major, minor)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Fatal(err)
	}
}
