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

package iscsi

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReadSessionAndAdoptionParams(t *testing.T) {
	root := t.TempDir()
	mkSysfsSession(t, root, 12, 7, testTarget, testInitiator, "FAILED")
	mkSysfsConn(t, root, 12, 0, "failed", "fd00::0001", 3261)
	writeFile(t, filepath.Join(root, "class", "iscsi_session", "session12", "recovery_tmo"), "45\n")
	// An explicit 0 (NOP-Out disabled) must survive adoption, not become the default.
	writeFile(t, filepath.Join(root, "class", "iscsi_connection", "connection12:0", "recv_tmo"), "0\n")
	// Must not be picked up as a connection of session 12.
	mkSysfsConn(t, root, 120, 0, "up", "10.0.0.1", 3260)

	fsys := sysfs{root: root}
	list, err := fsys.listSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("sessions = %d", len(list))
	}
	ss := list[0]
	if ss.SID != 12 || ss.HostNo != 7 || ss.TargetName != testTarget || ss.InitiatorName != testInitiator ||
		ss.State != "FAILED" || ss.TPGT != 1 || ss.RecoveryTmo != 45 || len(ss.Conns) != 1 {
		t.Fatalf("session = %+v", ss)
	}
	if ss.healthy() {
		t.Error("FAILED session reported healthy")
	}

	s, err := sessionFromSysfs(ss)
	if err != nil {
		t.Fatal(err)
	}
	checkAdoptedSession(t, s)
}

func checkAdoptedSession(t *testing.T, s *session) {
	t.Helper()
	if s.key.portal != (Portal{Address: "fd00::1", Port: 3261}) {
		t.Errorf("portal = %+v (IPv6 must be canonicalized)", s.key.portal)
	}
	if *s.params.ReplacementTimeout != 45*time.Second || *s.params.NoopOutInterval != 0 ||
		*s.params.NoopOutTimeout != DefaultNoopOutTimeout || s.params.LoginTimeout != DefaultLoginTimeout {
		t.Errorf("params = replacement %v, noop interval %v, noop timeout %v, login %v",
			*s.params.ReplacementTimeout, *s.params.NoopOutInterval, *s.params.NoopOutTimeout, s.params.LoginTimeout)
	}
	if !s.needStop || s.phase != phaseEstablished || s.sid != 12 || s.hostNo != 7 {
		t.Errorf("session bookkeeping = sid %d host %d phase %d needStop %v", s.sid, s.hostNo, s.phase, s.needStop)
	}
	if s.key.portal.String() != "[fd00::1]:3261" {
		t.Errorf("portal string = %s", s.key.portal)
	}
}

func TestAdoptionRequiresPortal(t *testing.T) {
	root := t.TempDir()
	mkSysfsSession(t, root, 2, 3, testTarget, testInitiator, "LOGGED_IN")
	mkSysfsConn(t, root, 2, 0, "up", "", 0)
	ss, err := sysfs{root: root}.readSession(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionFromSysfs(ss); err == nil {
		t.Error("session without persistent portal adopted")
	}
	if !ss.healthy() {
		t.Error("LOGGED_IN/up session reported unhealthy")
	}
}

func TestSessionLUNsAndDevNumber(t *testing.T) {
	root := t.TempDir()
	mkSysfsSession(t, root, 1, 4, testTarget, testInitiator, "LOGGED_IN")
	mkSysfsLUN(t, root, 1, 4, 3, "sdd", "8:48")
	mkSysfsLUN(t, root, 1, 4, 0, "", "")
	fsys := sysfs{root: root}
	luns, err := fsys.sessionLUNs(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(luns) != 2 || luns[0].LUN != 0 || luns[0].Block != "" || luns[1].HCTL != "4:0:0:3" || luns[1].Block != "sdd" {
		t.Fatalf("luns = %+v", luns)
	}
	ma, mi, err := fsys.blockDevNumber(1, luns[1])
	if err != nil || ma != 8 || mi != 48 {
		t.Errorf("dev = %d:%d, %v", ma, mi, err)
	}
	for _, bad := range []string{"8", "x:1", "8:y"} {
		if _, _, err := parseDevNumber(bad); err == nil {
			t.Errorf("parseDevNumber(%q) accepted", bad)
		}
	}
}

func TestTransportHandleMissingModule(t *testing.T) {
	if _, err := (sysfs{root: t.TempDir()}).transportHandle(); err == nil {
		t.Error("missing iscsi_tcp accepted")
	}
}
