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
	"sync"
	"testing"
)

// Cleanup tests (issue #98): a port is removed once no subsystem is linked to
// it and a host entry once no subsystem allows it, without breaking the port
// ordering contract.

func ownPortDir(t *NvmetTarget) string {
	return t.portDir(stablePortID(t.BindAddress, t.Port))
}

func assertExists(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s missing: %v", what, err)
	}
}

func assertGone(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still present (err %v)", what, err)
	}
}

// TestRemove_PrunesPortWhenLastSubsystemUnlinked verifies a shared port
// survives the removal of one subsystem and is removed with the last one.
func TestRemove_PrunesPortWhenLastSubsystemUnlinked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := orderingTarget(root, "shared-a")
	b := orderingTarget(root, "shared-b")
	for _, tgt := range []*NvmetTarget{a, b} {
		if err := tgt.Apply(); err != nil {
			t.Fatalf("Apply %s: %v", tgt.SubsystemNQN, err)
		}
	}
	pDir := ownPortDir(a)

	if err := a.Remove(); err != nil {
		t.Fatalf("Remove a: %v", err)
	}
	assertExists(t, pDir, "port shared with b")
	if !portLinked(b) {
		t.Fatal("removing a unlinked b from the shared port")
	}
	assertFileContent(t, filepath.Join(pDir, "addr_trsvcid"), "4420")

	if err := b.Remove(); err != nil {
		t.Fatalf("Remove b: %v", err)
	}
	assertGone(t, pDir, "port without subsystems")
	assertGone(t, filepath.Join(root, "nvmet", "hosts", orderingHost), "unreferenced host")

	if err := b.Remove(); err != nil {
		t.Fatalf("idempotent Remove b: %v", err)
	}
}

// TestRemove_LeavesPortsItNeverUsed verifies Remove prunes only ports the
// subsystem was linked to or its own port, never unrelated empty ports.
func TestRemove_LeavesPortsItNeverUsed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tgt := orderingTarget(root, "own")
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	foreign := filepath.Join(root, "nvmet", "ports", "7", "subsystems")
	if err := os.MkdirAll(foreign, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertGone(t, ownPortDir(tgt), "own port")
	assertExists(t, foreign, "unrelated empty port")
}

// TestRemove_PrunesOwnPortPreparedButNeverLinked verifies a port created by
// Prepare is not leaked when the export is removed before Link.
func TestRemove_PrunesOwnPortPreparedButNeverLinked(t *testing.T) {
	t.Parallel()
	tgt := orderingTarget(t.TempDir(), "unlinked")
	if _, err := tgt.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	assertExists(t, ownPortDir(tgt), "prepared port")

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertGone(t, ownPortDir(tgt), "port of a removed, never linked export")
}

// TestLink_RecreatesPortPrunedAfterPrepare verifies the ordering contract
// survives pruning: when the last linked subsystem is removed between another
// export's Prepare and Link, the port stops existing (and listening) until
// Link re-creates it with its attributes and links the prepared subsystem.
func TestLink_RecreatesPortPrunedAfterPrepare(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := orderingTarget(root, "linked")
	b := orderingTarget(root, "pending")
	if err := a.Apply(); err != nil {
		t.Fatalf("Apply a: %v", err)
	}
	prepared, err := b.Prepare()
	if err != nil {
		t.Fatalf("Prepare b: %v", err)
	}

	if err := a.Remove(); err != nil {
		t.Fatalf("Remove a: %v", err)
	}
	pDir := ownPortDir(b)
	assertGone(t, pDir, "port whose only linked subsystem was removed")
	assertExists(t, b.hostDir(orderingHost), "host still allowed by b")

	if err := prepared.Link(); err != nil {
		t.Fatalf("Link b: %v", err)
	}
	if !portLinked(b) {
		t.Fatal("Link did not link b to the re-created port")
	}
	assertFileContent(t, filepath.Join(pDir, "addr_trtype"), "tcp")
	assertFileContent(t, filepath.Join(pDir, "addr_adrfam"), "ipv4")
	assertFileContent(t, filepath.Join(pDir, "addr_traddr"), listenWildcard)
	assertFileContent(t, filepath.Join(pDir, "addr_trsvcid"), "4420")
}

// TestHostEntry_PrunedOnlyWhenUnreferenced verifies DenyHost and
// RevokeHostsExcept remove a host entry only once no subsystem allows it.
func TestHostEntry_PrunedOnlyWhenUnreferenced(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := orderingTarget(root, "acl-a")
	b := orderingTarget(root, "acl-b")
	for _, tgt := range []*NvmetTarget{a, b} {
		if err := tgt.Apply(); err != nil {
			t.Fatalf("Apply %s: %v", tgt.SubsystemNQN, err)
		}
	}
	hDir := a.hostDir(orderingHost)

	if err := a.DenyHost(orderingHost); err != nil {
		t.Fatalf("DenyHost a: %v", err)
	}
	assertExists(t, hDir, "host still allowed by b")

	if err := b.RevokeHostsExcept(nil); err != nil {
		t.Fatalf("RevokeHostsExcept b: %v", err)
	}
	assertGone(t, hDir, "host no subsystem allows")
}

// TestRevokeHostsExcept_PrunesOnlyRevokedHosts verifies revocation never
// removes host entries it did not revoke, even unreferenced ones.
func TestRevokeHostsExcept_PrunesOnlyRevokedHosts(t *testing.T) {
	t.Parallel()
	tgt := orderingTarget(t.TempDir(), "revoke")
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	stray := tgt.hostDir("nqn.2023-01.io.example:stray")
	if err := os.MkdirAll(stray, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := tgt.RevokeHostsExcept(nil); err != nil {
		t.Fatalf("RevokeHostsExcept: %v", err)
	}
	assertGone(t, tgt.hostDir(orderingHost), "revoked host")
	assertExists(t, stray, "host entry not revoked by this call")
}

// TestRemove_RevokesHostsGrantedOutsideAllowedHosts verifies Remove revokes
// the allowed_hosts links actually present, such as hosts granted by
// AllowInitiator, not only the ones in the target description.
func TestRemove_RevokesHostsGrantedOutsideAllowedHosts(t *testing.T) {
	t.Parallel()
	tgt := orderingTarget(t.TempDir(), "granted")
	tgt.AllowedHosts = nil
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tgt.AllowHost(orderingHost); err != nil {
		t.Fatalf("AllowHost: %v", err)
	}

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertGone(t, tgt.subsystemDir(), "subsystem")
	assertGone(t, tgt.hostDir(orderingHost), "host granted outside AllowedHosts")
}

// TestHostEntry_AllowRacingDenyKeepsGrant verifies the host lock: pruning the
// host entry for one subsystem never removes it under a concurrent grant to
// another subsystem.
func TestHostEntry_AllowRacingDenyKeepsGrant(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := orderingTarget(root, "race-a")
	b := orderingTarget(root, "race-b")
	for _, tgt := range []*NvmetTarget{a, b} {
		tgt.AllowedHosts = nil
		if err := tgt.createSubsystem(); err != nil {
			t.Fatalf("createSubsystem: %v", err)
		}
	}

	for range 200 {
		if err := a.AllowHost(orderingHost); err != nil {
			t.Fatalf("AllowHost a: %v", err)
		}
		var wg sync.WaitGroup
		var denyErr, allowErr error
		wg.Add(2)
		go func() { defer wg.Done(); denyErr = a.DenyHost(orderingHost) }()
		go func() { defer wg.Done(); allowErr = b.AllowHost(orderingHost) }()
		wg.Wait()
		if denyErr != nil || allowErr != nil {
			t.Fatalf("DenyHost a: %v, AllowHost b: %v", denyErr, allowErr)
		}
		assertExists(t, b.hostDir(orderingHost), "host granted to b")
		if err := b.DenyHost(orderingHost); err != nil {
			t.Fatalf("DenyHost b: %v", err)
		}
		assertGone(t, b.hostDir(orderingHost), "host no subsystem allows")
	}
}
