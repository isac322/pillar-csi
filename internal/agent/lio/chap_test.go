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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/lio"
)

const testInitiator2 = "iqn.2026-01.com.bhyoo.pillar-csi:node.fedcba9876543210fedcba9876543210"

var (
	oneWayCHAP = &lio.CHAP{Username: "node-user", Password: "initiator-secret-1"}
	mutualCHAP = &lio.CHAP{
		Username: "node-user", Password: "initiator-secret-1",
		MutualUsername: "target-user", MutualPassword: "target-secret-12",
	}
)

func (e env) chapTarget(chap *lio.CHAP, initiators ...string) *lio.Target {
	target := e.target(testIQN)
	target.ACLEnabled = true
	target.AllowedInitiators = initiators
	target.CHAP = chap
	return target
}

// readAuth returns the ACL auth attribute as LIO shows it, minus the one
// trailing newline.
func (e env) readAuth(t *testing.T, iqn, name string) string {
	t.Helper()
	path := filepath.Join(e.tpg(testIQN), "acls", iqn, "auth", name)
	data, err := os.ReadFile(path) //nolint:gosec // test tree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSuffix(string(data), "\n")
}

func (e env) assertAuth(t *testing.T, iqn string, want map[string]string) {
	t.Helper()
	for name, value := range want {
		if got := e.readAuth(t, iqn, name); got != value {
			t.Errorf("acls/%s/auth/%s = %q, want %q", iqn, name, got, value)
		}
	}
}

func (e env) authentication(t *testing.T) string {
	t.Helper()
	return e.read(t, filepath.Join(e.tpg(testIQN), "attrib", "authentication"))
}

// One-way CHAP turns the TPG's authentication on and puts the initiator
// credentials on every node ACL; the mutual pair stays unset, so LIO cannot
// authenticate itself to an initiator asking for it.
func TestCHAP_OneWay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.chapTarget(oneWayCHAP, testInitiator))

	if got := e.authentication(t); got != "1" {
		t.Errorf("attrib/authentication = %q, want 1", got)
	}
	e.assertAuth(t, testInitiator, map[string]string{
		"userid": "node-user", "password": "initiator-secret-1",
		"userid_mutual": "", "password_mutual": "", "authenticate_target": "0",
	})
	acl := filepath.Join(e.tpg(testIQN), "acls", testInitiator, "attrib", "authentication")
	if got := e.read(t, acl); got != "-1" {
		t.Errorf("ACL attrib/authentication = %q, want -1 (inherit the TPG switch)", got)
	}
}

func TestCHAP_Mutual(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.chapTarget(mutualCHAP, testInitiator))

	if got := e.authentication(t); got != "1" {
		t.Errorf("attrib/authentication = %q, want 1", got)
	}
	e.assertAuth(t, testInitiator, map[string]string{
		"userid": "node-user", "password": "initiator-secret-1",
		"userid_mutual": "target-user", "password_mutual": "target-secret-12", "authenticate_target": "1",
	})
}

// Switching an export from mutual to one-way CHAP clears the mutual pair
// with LIO's "NULL", which disables target authentication.
func TestCHAP_OneWayAfterMutualClearsMutual(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.chapTarget(mutualCHAP, testInitiator))
	mustApply(t, e.chapTarget(oneWayCHAP, testInitiator))

	e.assertAuth(t, testInitiator, map[string]string{
		"userid": "node-user", "password": "initiator-secret-1",
		"userid_mutual": "NULL", "password_mutual": "NULL", "authenticate_target": "0",
	})
	// Converged: a repeat leaves the cleared values alone.
	mustApply(t, e.chapTarget(oneWayCHAP, testInitiator))
}

// AllowInitiator with changed credentials rewrites an existing ACL; a
// re-export (Prepare) rotates every existing ACL, also those it was not
// asked to allow.
func TestCHAP_CredentialChangeUpdatesExistingACLs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.chapTarget(oneWayCHAP, testInitiator))
	if err := e.chapTarget(oneWayCHAP).AllowInitiator(testInitiator2); err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}

	rotated := &lio.CHAP{Username: "node-user-2", Password: "rotated-secret-22"}
	if err := e.chapTarget(rotated).AllowInitiator(testInitiator); err != nil {
		t.Fatalf("AllowInitiator (rotated): %v", err)
	}
	e.assertAuth(t, testInitiator, map[string]string{"userid": "node-user-2", "password": "rotated-secret-22"})
	e.assertAuth(t, testInitiator2, map[string]string{"userid": "node-user", "password": "initiator-secret-1"})

	mustApply(t, e.chapTarget(rotated))
	e.assertAuth(t, testInitiator2, map[string]string{"userid": "node-user-2", "password": "rotated-secret-22"})
}

// A credential write the kernel does not keep is an error that does not
// reveal the secret.
func TestCHAP_ReadBackMismatchIsError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.chapTarget(oneWayCHAP, testInitiator))
	path := filepath.Join(e.tpg(testIQN), "acls", testInitiator, "auth", "password")
	e.k.DropWrite(path)

	err := e.chapTarget(&lio.CHAP{Username: "node-user", Password: "another-secret-3"}).AllowInitiator(testInitiator)
	if err == nil || !strings.Contains(err.Error(), "read-back differs") {
		t.Fatalf("AllowInitiator with a dropped password write = %v, want read-back error", err)
	}
	if strings.Contains(err.Error(), "another-secret-3") {
		t.Fatalf("error leaks the secret: %v", err)
	}
}

func TestCHAP_NoneKeepsAuthenticationOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mustApply(t, e.chapTarget(nil, testInitiator))

	if got := e.authentication(t); got != "0" {
		t.Errorf("attrib/authentication = %q, want 0", got)
	}
	e.assertAuth(t, testInitiator, map[string]string{"userid": "", "password": ""})
}

func TestCHAP_Validate(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", lio.MaxCHAPValueLength+1)
	for name, tc := range map[string]struct {
		chap  lio.CHAP
		field string
	}{
		"missing username":    {lio.CHAP{Password: "initiator-secret-1"}, "username"},
		"missing password":    {lio.CHAP{Username: "u"}, "password"},
		"too long password":   {lio.CHAP{Username: "u", Password: long}, "password"},
		"NULL prefix":         {lio.CHAP{Username: "NULLuser", Password: "initiator-secret-1"}, "username"},
		"newline":             {lio.CHAP{Username: "u", Password: "secret\nsecret"}, "password"},
		"half mutual":         {lio.CHAP{Username: "u", Password: "p", MutualUsername: "m"}, "mutual password"},
		"missing mutual user": {lio.CHAP{Username: "u", Password: "p", MutualPassword: "x"}, "mutual username"},
	} {
		err := tc.chap.Validate()
		if err == nil || !strings.Contains(err.Error(), "CHAP "+tc.field+" ") {
			t.Errorf("%s: Validate = %v, want an error naming %q", name, err, tc.field)
		}
	}
	ok := lio.CHAP{Username: "u", Password: strings.Repeat("p", lio.MaxCHAPValueLength)}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate of a %d-byte secret = %v, want nil", lio.MaxCHAPValueLength, err)
	}
}

// CHAP lives on explicit node ACLs; a demo-mode export with CHAP is refused
// before configfs is touched.
func TestCHAP_RequiresACL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := e.chapTarget(oneWayCHAP)
	target.ACLEnabled = false
	if err := target.Apply(); err == nil || !strings.Contains(err.Error(), "CHAP requires explicit node ACLs") {
		t.Fatalf("Apply = %v, want CHAP-requires-ACL error", err)
	}
	if e.exists(e.tpg(testIQN)) {
		t.Error("TPG created for a refused export")
	}
}
