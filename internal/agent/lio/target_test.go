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

package lio_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/lio"
	"github.com/isac322/pillar-csi/internal/agent/lio/liotest"
)

const (
	testIQN       = lio.OwnedIQNPrefix + "tank.pvc-a"
	testDevice    = "/dev/zvol/tank/pvc-a"
	testInitiator = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
)

type env struct {
	k    *liotest.Kernel
	root string
}

func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	k, err := liotest.New(root)
	if err != nil {
		t.Fatalf("liotest.New: %v", err)
	}
	return env{k: k, root: root}
}

func (e env) target(iqn string) *lio.Target {
	return &lio.Target{
		ConfigfsRoot:  e.root,
		FS:            e.k,
		IQN:           iqn,
		DevicePath:    "/dev/zvol/" + strings.ReplaceAll(strings.TrimPrefix(iqn, lio.OwnedIQNPrefix), ".", "/"),
		BindAddress:   "192.0.2.10",
		Port:          3260,
		DeviceClaimer: e.k.Claimer(),
	}
}

func (e env) tpg(iqn string) string {
	return filepath.Join(e.root, "target", "iscsi", iqn, "tpgt_1")
}

func (e env) backstore(iqn string) string {
	return filepath.Join(e.root, "target", "core", "iblock_3260", lio.BackstoreNameForIQN(iqn))
}

func (env) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test tree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

func (e env) exists(path string) bool { return e.k.Exists(path) }

func mustApply(t *testing.T, target *lio.Target) {
	t.Helper()
	if err := target.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// Export without ACL builds the complete tree: backstore on the device with
// the derived serial, LUN 0 bound to it, demo-mode policy, portal, enabled
// TPG.
func TestApply_DemoMode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	mustApply(t, target)

	bs := e.backstore(testIQN)
	if got := e.read(t, filepath.Join(bs, "udev_path")); got != target.DevicePath {
		t.Errorf("udev_path = %q, want %q", got, target.DevicePath)
	}
	if got := e.read(t, filepath.Join(bs, "enable")); got != "1" {
		t.Errorf("backstore enable = %q, want 1", got)
	}
	wantSerial := "T10 VPD Unit Serial Number: " + lio.DeriveUnitSerial(testIQN)
	if got := e.read(t, filepath.Join(bs, "wwn", "vpd_unit_serial")); got != wantSerial {
		t.Errorf("vpd_unit_serial = %q, want %q", got, wantSerial)
	}
	tpg := e.tpg(testIQN)
	link, err := os.Readlink(filepath.Join(tpg, "lun", "lun_0", "backstore"))
	if err != nil || link != bs {
		t.Errorf("lun_0 link = %q, %v; want %q", link, err, bs)
	}
	for name, want := range map[string]string{
		"authentication": "0", "generate_node_acls": "1", "cache_dynamic_acls": "1", "demo_mode_write_protect": "0",
	} {
		if got := e.read(t, filepath.Join(tpg, "attrib", name)); got != want {
			t.Errorf("attrib/%s = %q, want %q", name, got, want)
		}
	}
	if !e.exists(filepath.Join(tpg, "np", "0.0.0.0:3260")) {
		t.Error("portal 0.0.0.0:3260 missing")
	}
	if got := e.read(t, filepath.Join(tpg, "enable")); got != "1" {
		t.Errorf("TPG enable = %q, want 1", got)
	}
}

// Re-applying an exported target changes nothing and succeeds even though
// LIO refuses a unit-serial write while a LUN uses the backstore.
func TestApply_Idempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.target(testIQN))
	mustApply(t, e.target(testIQN))
	if got := e.read(t, filepath.Join(e.tpg(testIQN), "enable")); got != "1" {
		t.Errorf("TPG enable = %q, want 1", got)
	}
}

// The unit serial is derived from the IQN alone, so a target re-created
// after its configfs state was lost presents the same LUN identity.
func TestApply_SerialSurvivesRecreation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.target(testIQN))
	serialPath := filepath.Join(e.backstore(testIQN), "wwn", "vpd_unit_serial")
	before := e.read(t, serialPath)
	if err := e.target(testIQN).Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mustApply(t, e.target(testIQN))
	if after := e.read(t, serialPath); after != before {
		t.Fatalf("serial after re-creation = %q, want %q", after, before)
	}
	if lio.DeriveUnitSerial(testIQN) == lio.DeriveUnitSerial(lio.OwnedIQNPrefix+"tank.pvc-b") {
		t.Fatal("different targets derived the same unit serial")
	}
}

// An exported backstore carrying another serial cannot be changed under the
// initiators; Apply reports it instead of silently keeping or rewriting it.
func TestApply_SerialMismatchWhileExportedFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.target(testIQN))
	target := e.target(testIQN)
	target.UnitSerial = "other-serial"
	err := target.Apply()
	if err == nil || !strings.Contains(err.Error(), "cannot change it while exported") {
		t.Fatalf("Apply with another serial = %v, want exported-serial error", err)
	}
}

func TestApply_IPv6PortalName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	target.BindAddress = "2001:db8::10"
	target.Port = 3261
	mustApply(t, target)
	if !e.exists(filepath.Join(e.tpg(testIQN), "np", "[::]:3261")) {
		t.Fatal("IPv6 portal [::]:3261 missing")
	}
}

// Two targets on the same port share one portal name; a port change moves
// the target's portal.
func TestApply_SharedPortalAndPortChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	other := lio.OwnedIQNPrefix + "tank.pvc-b"
	mustApply(t, e.target(testIQN))
	mustApply(t, e.target(other))
	for _, iqn := range []string{testIQN, other} {
		if !e.exists(filepath.Join(e.tpg(iqn), "np", "0.0.0.0:3260")) {
			t.Fatalf("%s: portal missing", iqn)
		}
	}
	moved := e.target(testIQN)
	moved.Port = 3270
	mustApply(t, moved)
	if e.exists(filepath.Join(e.tpg(testIQN), "np", "0.0.0.0:3260")) {
		t.Error("old portal still present after port change")
	}
	if !e.exists(filepath.Join(e.tpg(testIQN), "np", "0.0.0.0:3270")) {
		t.Error("new portal missing after port change")
	}
}

// With ACL enforcement demo mode is off and only mapped node ACLs admit
// initiators; AllowInitiator maps LUN 0, DenyInitiator removes the ACL.
func TestACL_AllowDeny(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	target.ACLEnabled = true
	mustApply(t, target)
	tpg := e.tpg(testIQN)
	if got := e.read(t, filepath.Join(tpg, "attrib", "generate_node_acls")); got != "0" {
		t.Fatalf("generate_node_acls = %q, want 0", got)
	}

	if err := target.AllowInitiator(testInitiator); err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}
	if err := target.AllowInitiator(testInitiator); err != nil {
		t.Fatalf("AllowInitiator (repeat): %v", err)
	}
	mapped := filepath.Join(tpg, "acls", testInitiator, "lun_0", "lun")
	link, err := os.Readlink(mapped)
	if err != nil || link != filepath.Join(tpg, "lun", "lun_0") {
		t.Fatalf("mapped LUN link = %q, %v", link, err)
	}

	if err = target.DenyInitiator(testInitiator); err != nil {
		t.Fatalf("DenyInitiator: %v", err)
	}
	if e.exists(filepath.Join(tpg, "acls", testInitiator)) {
		t.Fatal("node ACL still present after DenyInitiator")
	}
	if err = target.DenyInitiator(testInitiator); err != nil {
		t.Fatalf("DenyInitiator (repeat): %v", err)
	}
}

func TestAllowInitiator_MissingTarget(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	err := e.target(testIQN).AllowInitiator(testInitiator)
	if !errors.Is(err, lio.ErrTargetNotFound) {
		t.Fatalf("AllowInitiator on missing target = %v, want ErrTargetNotFound", err)
	}
}

// Reconcile-style Prepare with ACL makes the ACL set exactly the allowed
// initiators.
func TestRevokeInitiatorsExcept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const other = "iqn.2026-01.com.bhyoo.pillar-csi:node.other"
	target := e.target(testIQN)
	target.ACLEnabled = true
	target.AllowedInitiators = []string{testInitiator, other}
	mustApply(t, target)
	if err := target.RevokeInitiatorsExcept([]string{testInitiator}); err != nil {
		t.Fatalf("RevokeInitiatorsExcept: %v", err)
	}
	got, err := target.Initiators()
	if err != nil || !slices.Equal(got, []string{testInitiator}) {
		t.Fatalf("Initiators = %v, %v; want [%s]", got, err, testInitiator)
	}
}

// Remove tears down everything, including ACLs, portal and backstore, and a
// second Remove is a no-op.
func TestRemove(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	target.ACLEnabled = true
	target.AllowedInitiators = []string{testInitiator}
	mustApply(t, target)
	if err := target.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, p := range []string{filepath.Join(e.root, "target", "iscsi", testIQN), e.backstore(testIQN)} {
		if e.exists(p) {
			t.Errorf("%s still present after Remove", p)
		}
	}
	if err := target.Remove(); err != nil {
		t.Fatalf("Remove (repeat): %v", err)
	}
}

// A real teardown failure is returned with the failing path.
func TestRemove_ReportsFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	mustApply(t, target)
	enable := filepath.Join(e.tpg(testIQN), "enable")
	e.k.FailWrite(enable, syscall.EIO)
	err := target.Remove()
	if err == nil || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), enable) {
		t.Fatalf("Remove = %v, want EIO naming %s", err, enable)
	}
	if !e.exists(e.backstore(testIQN)) {
		t.Fatal("backstore removed although the teardown failed earlier")
	}
}

// Local attach removes LUN 0 (the node-side invariant), mapped LUNs and the
// backstore, so the device is free; leaving it restores the export once the
// node released the device.
func TestLocalAttach_RoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	target.ACLEnabled = true
	target.AllowedInitiators = []string{testInitiator}
	mustApply(t, target)
	tpg := e.tpg(testIQN)

	dev, err := target.EnterLocalAttach()
	if err != nil || dev != target.DevicePath {
		t.Fatalf("EnterLocalAttach = %q, %v; want %q", dev, err, target.DevicePath)
	}
	e.assertLocallyAttached(t, testIQN)
	if dev, err = target.EnterLocalAttach(); err != nil || dev != target.DevicePath {
		t.Fatalf("EnterLocalAttach (repeat) = %q, %v", dev, err)
	}

	// The node attaches the device locally: leaving must be refused.
	e.k.HoldDevice(target.DevicePath)
	err = target.LeaveLocalAttach()
	if !errors.Is(err, lio.ErrDeviceHeld) {
		t.Fatalf("LeaveLocalAttach while held = %v, want ErrDeviceHeld", err)
	}
	if e.exists(filepath.Join(tpg, "lun", "lun_0")) || e.exists(e.backstore(testIQN)) {
		t.Fatal("refused LeaveLocalAttach left a LUN or backstore behind")
	}

	e.k.ReleaseDevice(target.DevicePath)
	if err = target.LeaveLocalAttach(); err != nil {
		t.Fatalf("LeaveLocalAttach: %v", err)
	}
	if !e.exists(filepath.Join(tpg, "acls", testInitiator, "lun_0", "lun")) {
		t.Fatal("mapped LUN not restored")
	}
	if got := e.read(t, filepath.Join(tpg, "enable")); got != "1" {
		t.Fatalf("TPG enable = %q, want 1", got)
	}
	if err = target.LeaveLocalAttach(); err != nil {
		t.Fatalf("LeaveLocalAttach (repeat): %v", err)
	}
}

// assertLocallyAttached checks the local-attach state of iqn: no LUN 0, no
// mapped LUN, no backstore and a disabled TPG.
func (e env) assertLocallyAttached(t *testing.T, iqn string) {
	t.Helper()
	tpg := e.tpg(iqn)
	if e.exists(filepath.Join(tpg, "lun", "lun_0")) {
		t.Fatal("tpgt_1/lun/lun_0 present in local attach")
	}
	if e.exists(e.backstore(iqn)) {
		t.Fatal("backstore present in local attach")
	}
	if e.exists(filepath.Join(tpg, "acls", testInitiator, "lun_0")) {
		t.Fatal("mapped LUN present in local attach")
	}
	if got := e.read(t, filepath.Join(tpg, "enable")); got != "0" {
		t.Fatalf("TPG enable = %q, want 0", got)
	}
}

// The backstore enable is LIO's own exclusive open: an EBUSY from the kernel
// is reported as ErrDeviceHeld even when the claim probe passed.
func TestApply_EnableEBUSYIsDeviceHeld(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	target.DeviceClaimer = func(string) (func() error, error) { return func() error { return nil }, nil }
	e.k.HoldDevice(target.DevicePath)
	err := target.Apply()
	if !errors.Is(err, lio.ErrDeviceHeld) {
		t.Fatalf("Apply on held device = %v, want ErrDeviceHeld", err)
	}
	if e.exists(e.backstore(testIQN)) {
		t.Fatal("unconfigured backstore left behind")
	}
}

// emulateTPU is the thin-provisioning attribute of testIQN's backstore.
func (e env) emulateTPU() string {
	return filepath.Join(e.backstore(testIQN), "attrib", "emulate_tpu")
}

// A new backstore advertises thin provisioning, so initiators see UNMAP
// support and discards reach the backing device.
func TestApply_EnablesThinProvisioning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.target(testIQN))
	if got := e.read(t, e.emulateTPU()); got != "1" {
		t.Fatalf("emulate_tpu = %q, want 1", got)
	}
}

// A backstore left enabled without thin provisioning (by an agent predating
// it) is upgraded in place when the target is prepared again after a
// restart.
func TestPrepare_UpgradesEnabledBackstoreThinProvisioning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.target(testIQN))
	if err := os.WriteFile(e.emulateTPU(), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.target(testIQN).Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := e.read(t, e.emulateTPU()); got != "1" {
		t.Fatalf("emulate_tpu after Prepare = %q, want 1", got)
	}
}

// A device without discard support is still exported, without UNMAP, and
// the caller is told which device lacks it.
func TestApply_DiscardUnsupportedExportsWithoutUnmap(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	e.k.DisableDiscard(target.DevicePath)
	var reported []string
	target.DiscardUnsupported = func(device string) { reported = append(reported, device) }
	mustApply(t, target)
	if got := e.read(t, e.emulateTPU()); got != "0" {
		t.Errorf("emulate_tpu = %q, want 0", got)
	}
	if got := e.read(t, filepath.Join(e.tpg(testIQN), "enable")); got != "1" {
		t.Errorf("TPG enable = %q, want 1", got)
	}
	if !slices.Equal(reported, []string{target.DevicePath}) {
		t.Errorf("DiscardUnsupported calls = %q, want [%s]", reported, target.DevicePath)
	}
}

// Any other failure to enable thin provisioning fails the export with the
// attribute path: a write error, or a write the read-back does not confirm.
// A new backstore is rolled back; an existing one is kept.
func TestPrepare_ThinProvisioningFailures(t *testing.T) {
	t.Parallel()
	failEIO := func(k *liotest.Kernel, p string) { k.FailWrite(p, syscall.EIO) }
	drop := func(k *liotest.Kernel, p string) { k.DropWrite(p) }
	for _, tc := range []struct {
		name     string
		existing bool
		inject   func(k *liotest.Kernel, path string)
		errno    error
	}{
		{name: "write error on new backstore", inject: failEIO, errno: syscall.EIO},
		{name: "read-back mismatch on new backstore", inject: drop},
		{name: "write error on enabled backstore", existing: true, inject: failEIO, errno: syscall.EIO},
		{name: "read-back mismatch on enabled backstore", existing: true, inject: drop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			path := e.emulateTPU()
			if tc.existing {
				mustApply(t, e.target(testIQN))
				if err := os.WriteFile(path, []byte("0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tc.inject(e.k, path)
			err := e.target(testIQN).Prepare()
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("Prepare = %v, want error naming %s", err, path)
			}
			if tc.errno != nil && !errors.Is(err, tc.errno) {
				t.Fatalf("Prepare = %v, want %v", err, tc.errno)
			}
			if got := e.exists(e.backstore(testIQN)); got != tc.existing {
				t.Fatalf("backstore exists = %t, want %t", got, tc.existing)
			}
		})
	}
}

func TestPrepareLocalAttach_NoLUN(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.target(testIQN)
	target.LocalAttach = true
	mustApply(t, target)
	if e.exists(filepath.Join(e.tpg(testIQN), "lun", "lun_0")) || e.exists(e.backstore(testIQN)) {
		t.Fatal("local-attach export created a LUN or backstore")
	}
	if got := e.read(t, filepath.Join(e.tpg(testIQN), "enable")); got != "0" {
		t.Fatalf("TPG enable = %q, want 0", got)
	}
}

func TestListTargets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.target(testIQN))
	targets, err := lio.ListTargets(e.k, e.root)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListTargets = %v, %v", targets, err)
	}
	got := targets[0]
	if got.IQN != testIQN || got.DevicePath != testDevice || !slices.Equal(got.Portals, []string{"0.0.0.0:3260"}) {
		t.Fatalf("ListTargets[0] = %+v", got)
	}
}

// Loading the modules does not create target/iscsi; Available registers the
// fabric by creating it, and fails when target_core_mod is not loaded.
func TestAvailable(t *testing.T) {
	t.Parallel()
	unloaded := liotest.NewUnloaded(t.TempDir())
	if err := lio.Available(unloaded, unloaded.Root); err == nil {
		t.Fatal("Available without target_core_mod = nil, want error")
	}
	e := newEnv(t)
	if e.exists(filepath.Join(e.root, "target", "iscsi")) {
		t.Fatal("fake kernel pre-created target/iscsi")
	}
	if err := lio.Available(e.k, e.root); err != nil {
		t.Fatalf("Available: %v", err)
	}
	if !e.exists(filepath.Join(e.root, "target", "iscsi")) {
		t.Fatal("Available did not register the iscsi fabric")
	}
	if err := lio.Available(e.k, e.root); err != nil {
		t.Fatalf("Available (repeat): %v", err)
	}
}

func TestParsePortal(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"0.0.0.0:3260": "0.0.0.0", "[::]:3261": "::"} {
		addr, _, err := lio.ParsePortal(name)
		if err != nil || addr != want {
			t.Errorf("ParsePortal(%q) = %q, %v; want %q", name, addr, err, want)
		}
	}
	if _, _, err := lio.ParsePortal("::1:3260"); err == nil {
		t.Error("ParsePortal accepted an unbracketed IPv6 portal")
	}
}
