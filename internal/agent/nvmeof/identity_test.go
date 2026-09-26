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
	"os"
	"path/filepath"
	"testing"
)

const identityTestNQN = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"

// TestDeriveIdentity_Golden pins the derivation.  Changing it would give every
// already-exported volume a new identity on the next storage-node reboot, and
// connected hosts would drop the namespace ("identifiers changed for nsid").
func TestDeriveIdentity_Golden(t *testing.T) {
	t.Parallel()
	want := Identity{
		UUID:   "88493119-6566-539d-816d-99052f1ca5ab",
		NGUID:  "7e4138f3-491d-5624-8e27-5b6d4732d078",
		Serial: "0a3b1905140d875fdeeb",
	}
	if got := DeriveIdentity(identityTestNQN, 1); got != want {
		t.Fatalf("DeriveIdentity = %+v, want %+v", got, want)
	}
	if err := want.Validate(); err != nil {
		t.Fatalf("derived identity is not accepted by Validate: %v", err)
	}
}

func TestDeriveIdentity_ScopedToSubsystemAndNamespace(t *testing.T) {
	t.Parallel()
	base := DeriveIdentity(identityTestNQN, 1)
	otherNS := DeriveIdentity(identityTestNQN, 2)
	otherVol := DeriveIdentity("nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-def", 1)

	if otherNS.UUID == base.UUID || otherNS.NGUID == base.NGUID {
		t.Errorf("namespaces of one subsystem share an identifier: %+v vs %+v", base, otherNS)
	}
	if otherNS.Serial != base.Serial {
		t.Errorf("serial is a subsystem attribute and must not depend on nsid: %q vs %q", base.Serial, otherNS.Serial)
	}
	if otherVol.UUID == base.UUID || otherVol.NGUID == base.NGUID || otherVol.Serial == base.Serial {
		t.Errorf("different volumes share an identifier: %+v vs %+v", base, otherVol)
	}
	if base.UUID == base.NGUID {
		t.Errorf("uuid and nguid must be independent identifiers, both %q", base.UUID)
	}
}

func TestIdentityValidate_RejectsForms(t *testing.T) {
	t.Parallel()
	valid := DeriveIdentity(identityTestNQN, 1)
	cases := map[string]Identity{
		"uppercase uuid":   {UUID: "88493119-6566-539D-816D-99052F1CA5AB", NGUID: valid.NGUID, Serial: valid.Serial},
		"missing nguid":    {UUID: valid.UUID, Serial: valid.Serial},
		"serial too long":  {UUID: valid.UUID, NGUID: valid.NGUID, Serial: "0123456789abcdef01234"},
		"serial has space": {UUID: valid.UUID, NGUID: valid.NGUID, Serial: "abc def"},
		"empty serial":     {UUID: valid.UUID, NGUID: valid.NGUID},
	}
	for name, id := range cases {
		if err := id.Validate(); err == nil {
			t.Errorf("%s: Validate(%+v) = nil, want error", name, id)
		}
	}
	legacy := Identity{UUID: valid.UUID, NGUID: zeroNGUID, Serial: "6c1a5e0b2f9d4a31"}
	if err := legacy.Validate(); err != nil {
		t.Errorf("0.2.0 identity with zero nguid rejected: %v", err)
	}
}

// TestApply_WritesDerivedIdentity: a fresh export carries the derived
// identity, and re-creating it after the whole configfs tree is lost (a
// storage-node reboot) reproduces exactly the same identifiers.
func TestApply_WritesDerivedIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: identityTestNQN,
		NamespaceID:  1,
		DevicePath:   "/dev/vg0/pvc-abc",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
	}
	want := DeriveIdentity(identityTestNQN, 1)

	for round := range 2 {
		if err := tgt.Apply(); err != nil {
			t.Fatalf("round %d: Apply: %v", round, err)
		}
		assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_uuid"), want.UUID)
		assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_nguid"), want.NGUID)
		assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_serial"), want.Serial)
		assertFileContent(t, filepath.Join(tgt.namespaceDir(), "enable"), "1")
		if err := os.RemoveAll(filepath.Join(root, "nvmet")); err != nil {
			t.Fatalf("simulate reboot: %v", err)
		}
	}
}

func TestApply_WritesExplicitIdentity(t *testing.T) {
	t.Parallel()
	explicit := Identity{
		UUID:   "0b6f3c52-1d0e-4c55-9a27-77d9f1f7c0aa",
		NGUID:  zeroNGUID,
		Serial: "6c1a5e0b2f9d4a31",
	}
	tgt := &NvmetTarget{
		ConfigfsRoot: t.TempDir(),
		SubsystemNQN: identityTestNQN,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
		Identity:     explicit,
	}
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_uuid"), explicit.UUID)
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_nguid"), explicit.NGUID)
	assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_serial"), explicit.Serial)
}

func TestApply_RejectsInvalidExplicitIdentity(t *testing.T) {
	t.Parallel()
	tgt := &NvmetTarget{
		ConfigfsRoot: t.TempDir(),
		SubsystemNQN: identityTestNQN,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
		Identity:     Identity{UUID: "not-a-uuid"},
	}
	if err := tgt.Apply(); err == nil {
		t.Fatal("Apply with invalid identity: expected error, got nil")
	}
	if _, err := os.Stat(tgt.subsystemDir()); !os.IsNotExist(err) {
		t.Errorf("invalid identity must fail before any configfs object is created (stat err %v)", err)
	}
}

// writeLiveNamespace lays out a subsystem and enabled namespace the way a
// kernel-random 0.2.0 export looks in configfs.
func writeLiveNamespace(t *testing.T, tgt *NvmetTarget, live Identity, enable string) {
	t.Helper()
	if err := os.MkdirAll(tgt.namespaceDir(), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files := map[string]string{
		filepath.Join(tgt.subsystemDir(), "attr_serial"):  live.Serial + "    \n",
		filepath.Join(tgt.namespaceDir(), "device_path"):  tgt.DevicePath + "\n",
		filepath.Join(tgt.namespaceDir(), "device_uuid"):  live.UUID + "\n",
		filepath.Join(tgt.namespaceDir(), "device_nguid"): live.NGUID + "\n",
		filepath.Join(tgt.namespaceDir(), "enable"):       enable + "\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

var legacyIdentity = Identity{
	UUID:   "d1f0c7e4-3b0a-4f8e-9c55-2a9b6e7d1c03",
	NGUID:  zeroNGUID,
	Serial: "6c1a5e0b2f9d4a31",
}

// TestApply_RefusesToChangeLiveNamespaceIdentity: an enabled namespace whose
// identity differs from the desired one is neither rewritten nor disabled,
// because connected hosts cached it.
func TestApply_RefusesToChangeLiveNamespaceIdentity(t *testing.T) {
	t.Parallel()
	tgt := &NvmetTarget{
		ConfigfsRoot: t.TempDir(),
		SubsystemNQN: identityTestNQN,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
		Identity: Identity{
			UUID:   DeriveIdentity(identityTestNQN, 1).UUID,
			NGUID:  zeroNGUID,
			Serial: legacyIdentity.Serial,
		},
	}
	writeLiveNamespace(t, tgt, legacyIdentity, "1")

	if err := tgt.Apply(); err == nil {
		t.Fatal("Apply over a live namespace with a different uuid: expected error, got nil")
	}
	// Untouched: still the bytes writeLiveNamespace laid out.
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_uuid"), legacyIdentity.UUID+"\n")
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "enable"), "1\n")
}

// TestApply_KeepsMatchingLiveIdentityWithoutWriting: re-applying a live export
// with its own identity succeeds even though nvmet rejects writes to the
// locked attributes (read-only files here).
func TestApply_KeepsMatchingLiveIdentityWithoutWriting(t *testing.T) {
	t.Parallel()
	tgt := &NvmetTarget{
		ConfigfsRoot: t.TempDir(),
		SubsystemNQN: identityTestNQN,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
		Identity:     legacyIdentity,
	}
	writeLiveNamespace(t, tgt, legacyIdentity, "1")
	for _, path := range []string{
		filepath.Join(tgt.subsystemDir(), "attr_serial"),
		filepath.Join(tgt.namespaceDir(), "device_uuid"),
		filepath.Join(tgt.namespaceDir(), "device_nguid"),
	} {
		makeFileReadOnly(t, path)
	}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply with the live identity: %v", err)
	}
}

// TestApply_RewritesDisabledNamespaceIdentity: a namespace that is not enabled
// was never visible to hosts, so its kernel-random identity is replaced.
func TestApply_RewritesDisabledNamespaceIdentity(t *testing.T) {
	t.Parallel()
	tgt := &NvmetTarget{
		ConfigfsRoot: t.TempDir(),
		SubsystemNQN: identityTestNQN,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
	}
	writeLiveNamespace(t, tgt, legacyIdentity, "0")
	want := DeriveIdentity(identityTestNQN, 1)
	tgt.Identity = Identity{UUID: want.UUID, NGUID: want.NGUID, Serial: legacyIdentity.Serial}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_uuid"), want.UUID)
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_nguid"), want.NGUID)
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "enable"), "1")
}

func TestLiveIdentity(t *testing.T) {
	t.Parallel()
	tgt := &NvmetTarget{
		ConfigfsRoot: t.TempDir(),
		SubsystemNQN: identityTestNQN,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc",
	}

	got, err := tgt.LiveIdentity()
	if err != nil || !got.IsZero() {
		t.Fatalf("no target: LiveIdentity = %+v, %v; want zero, nil", got, err)
	}

	writeLiveNamespace(t, tgt, legacyIdentity, "0")
	got, err = tgt.LiveIdentity()
	if err != nil || got != (Identity{Serial: legacyIdentity.Serial}) {
		t.Fatalf("disabled namespace: LiveIdentity = %+v, %v; want only the serial", got, err)
	}

	writeLiveNamespace(t, tgt, legacyIdentity, "1")
	got, err = tgt.LiveIdentity()
	if err != nil || got != legacyIdentity {
		t.Fatalf("enabled namespace: LiveIdentity = %+v, %v; want %+v", got, err, legacyIdentity)
	}
}
