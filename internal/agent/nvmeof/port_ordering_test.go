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

// Port ordering contract tests (issue #92): a subsystem becomes reachable
// only through its port link, and it must be linked only once its ACL,
// namespace identity, device and enable are in place.

const orderingHost = "nqn.2023-01.io.example:ordering-host"

func orderingTarget(root, name string) *NvmetTarget {
	return &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.example:" + name,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/" + name,
		BindAddress:  "10.0.0.1",
		Port:         4420,
		ACLEnabled:   true,
		AllowedHosts: []string{orderingHost},
	}
}

func portLinked(t *NvmetTarget) bool {
	_, err := os.Lstat(t.portSubsystemLink(stablePortID(t.BindAddress, t.Port)))
	return err == nil
}

// TestPrepare_ConfiguresEverythingButThePortLink verifies that Prepare leaves
// the subsystem unreachable yet complete, and that Link then only adds the
// port link.
func TestPrepare_ConfiguresEverythingButThePortLink(t *testing.T) {
	t.Parallel()
	tgt := orderingTarget(t.TempDir(), "prepare")

	prepared, err := tgt.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if portLinked(tgt) {
		t.Fatal("Prepare linked the subsystem to its port")
	}
	id := DeriveIdentity(tgt.SubsystemNQN, 1)
	assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_allow_any_host"), "0")
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "enable"), "1")
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_uuid"), id.UUID)
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_nguid"), id.NGUID)
	_, err = os.Lstat(tgt.allowedHostLink(orderingHost))
	if err != nil {
		t.Fatalf("Prepare did not add the allowed host: %v", err)
	}

	err = prepared.Link()
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !portLinked(tgt) {
		t.Fatal("Link did not link the subsystem to its port")
	}
}

// TestLink_RefusesSubsystemAHostCouldNotUse verifies the read-back guard of
// Link: every state a host would be rejected or lose its namespace for keeps
// the subsystem unlinked.
func TestLink_RefusesSubsystemAHostCouldNotUse(t *testing.T) {
	t.Parallel()
	cases := map[string]func(t *testing.T, tgt *NvmetTarget){
		"namespace disabled": func(t *testing.T, tgt *NvmetTarget) {
			t.Helper()
			mustWrite(t, filepath.Join(tgt.namespaceDir(), "enable"), "0")
		},
		"identity differs": func(t *testing.T, tgt *NvmetTarget) {
			t.Helper()
			mustWrite(t, filepath.Join(tgt.namespaceDir(), "device_uuid"), "00000000-0000-0000-0000-000000000001")
		},
		"device differs": func(t *testing.T, tgt *NvmetTarget) {
			t.Helper()
			mustWrite(t, filepath.Join(tgt.namespaceDir(), "device_path"), "/dev/other")
		},
		"any host allowed": func(t *testing.T, tgt *NvmetTarget) {
			t.Helper()
			mustWrite(t, filepath.Join(tgt.subsystemDir(), "attr_allow_any_host"), "1")
		},
		"allowed host missing": func(t *testing.T, tgt *NvmetTarget) {
			t.Helper()
			err := os.Remove(tgt.allowedHostLink(orderingHost))
			if err != nil {
				t.Fatalf("remove allowed host: %v", err)
			}
		},
	}
	for name, breakTarget := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tgt := orderingTarget(t.TempDir(), "guard")
			prepared, err := tgt.Prepare()
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			breakTarget(t, tgt)

			err = prepared.Link()
			if err == nil {
				t.Fatal("Link succeeded for a subsystem a host could not use")
			}
			if portLinked(tgt) {
				t.Fatal("Link linked a subsystem a host could not use")
			}
		})
	}
}

// TestApply_NoPortLinkWhenACLFails verifies the ACL is applied before the
// link: when adding the allowed host fails, the subsystem stays unreachable
// instead of answering its host with a do-not-retry rejection.
func TestApply_NoPortLinkWhenACLFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tgt := orderingTarget(root, "acl-fails")
	// A regular file where the hosts/ directory belongs makes AllowHost fail.
	err := os.MkdirAll(filepath.Join(root, "nvmet"), 0o750)
	if err != nil {
		t.Fatalf("mkdir nvmet: %v", err)
	}
	mustWrite(t, filepath.Join(root, "nvmet", "hosts"), "")

	err = tgt.Apply()
	if err == nil {
		t.Fatal("Apply succeeded although the ACL could not be applied")
	}
	if portLinked(tgt) {
		t.Fatal("subsystem linked to its port before its ACL was in place")
	}
}

// TestApply_NoPortLinkWhenNamespaceFails verifies the namespace is enabled
// before the link.
func TestApply_NoPortLinkWhenNamespaceFails(t *testing.T) {
	t.Parallel()
	tgt := orderingTarget(t.TempDir(), "ns-fails")
	// A regular file where the namespaces/ directory belongs makes the
	// namespace creation fail.
	err := os.MkdirAll(tgt.subsystemDir(), 0o750)
	if err != nil {
		t.Fatalf("mkdir subsystem: %v", err)
	}
	mustWrite(t, filepath.Join(tgt.subsystemDir(), "namespaces"), "")

	err = tgt.Apply()
	if err == nil {
		t.Fatal("Apply succeeded although the namespace could not be created")
	}
	if portLinked(tgt) {
		t.Fatal("subsystem linked to its port before its namespace was enabled")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
