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
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoginEstablishesSessionAndPushesParams(t *testing.T) {
	h := newHarness(t)
	h.start()
	p := h.params()
	p.ReplacementTimeout = new(30 * time.Second)
	p.NoopOutInterval = new(time.Duration(0)) // explicit 0 disables NOP-Out, not the default

	s, err := h.ini.Login(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if s.SID != 1 || s.HostNo != 11 || s.State != "LOGGED_IN" || s.TargetIQN != testTarget {
		t.Errorf("session = %+v", s)
	}
	calls := h.kern.callLog()
	want := []string{"CREATE_SESSION -> 1", "CREATE_CONN 1:0", "BIND_CONN 1:0 fd=101",
		"SEND_PDU 1:0 op=0x03", "SEND_PDU 1:0 op=0x03", "START_CONN 1:0"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q\nwant    %q", calls, want)
	}
	params := h.kern.sessions[1].params
	for pr, v := range map[iscsiParam]string{
		paramTargetName:        testTarget,
		paramInitiatorName:     testInitiator,
		paramMaxRecvDLength:    "262144",
		paramMaxXmitDLength:    "65536",
		paramInitialR2TEn:      "1",
		paramImmDataEn:         "1",
		paramFirstBurst:        "65536",
		paramMaxBurst:          "262144",
		paramHdrDgstEn:         "0",
		paramExpStatSN:         "103", // last login response StatSN 102 + 1
		paramPersistentAddress: "10.0.0.1",
		paramPersistentPort:    "3260",
		paramTPGT:              "1",
		paramSessRecoveryTmo:   "30",
		paramPingTmo:           "5", // unset: default
		paramRecvTmo:           "0",
		paramLUResetTmo:        "30",
		paramTgtResetTmo:       "30",
		paramAbortTmo:          "15",
	} {
		if params[pr] != v {
			t.Errorf("SET_PARAM %s = %q, want %q", pr, params[pr], v)
		}
	}

	// Idempotent per (target, portal).
	again, err := h.ini.Login(context.Background(), p)
	if err != nil || again.SID != s.SID {
		t.Fatalf("second login = %+v, %v", again, err)
	}
	if n := len(h.kern.callLog()); n != len(want) {
		t.Errorf("second login issued kernel requests: %q", h.kern.callLog()[len(want):])
	}
	found, err := h.ini.FindSession(testTarget, Portal{Address: "10.0.0.1", Port: 3260})
	if err != nil || found == nil || found.SID != 1 {
		t.Errorf("FindSession = %+v, %v", found, err)
	}
}

func TestLoginValidation(t *testing.T) {
	h := newHarness(t)
	p := h.params()
	if _, err := h.ini.Login(context.Background(), p); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Errorf("login before Start -> %v", err)
	}
	h.start()
	p.InitiatorIQN = "iqn.other"
	if _, err := h.ini.Login(context.Background(), p); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Errorf("foreign initiator IQN -> %v", err)
	}
	if _, err := newInitiator(Options{}, deps{}); err == nil {
		t.Error("missing Options.InitiatorIQN accepted")
	}
}

func TestLoginFailureTearsDownKernelSession(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.tgt.status = func(int, *loginRequestView) (uint16, []textKV) { return 0x0202, nil }
	_, err := h.ini.Login(context.Background(), h.params())
	var le *LoginError
	if !errors.As(err, &le) || le.StatusDetail != 2 {
		t.Fatalf("err = %v", err)
	}
	calls := h.kern.callLog()
	for _, want := range []string{
		"STOP_CONN 1:0 flag=3", "STOP_CONN 1:0 flag=1", "DESTROY_CONN 1:0", "DESTROY_SESSION 1",
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing %q in %q", want, calls)
		}
	}
	_, statErr := os.Stat(filepath.Join(h.root, "class", "iscsi_session", "session1"))
	if !os.IsNotExist(statErr) {
		t.Errorf("sysfs session left behind: %v", statErr)
	}
	if h.dialer.closed != 1 {
		t.Errorf("socket closes = %d, want 1", h.dialer.closed)
	}
	s, err := h.ini.FindSession(testTarget, h.params().Portal)
	if err != nil {
		t.Errorf("find session: %v", err)
	}
	if s != nil {
		t.Errorf("failed session still tracked: %+v", s)
	}
}

func TestLoginFollowsRedirect(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.tgt.status = func(attempt int, _ *loginRequestView) (uint16, []textKV) {
		if attempt == 1 {
			return 0x0101, []textKV{{"TargetAddress", "10.0.0.9:3261,1"}}
		}
		return 0, nil
	}
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	if len(h.dialer.dials) != 2 || h.dialer.dials[1] != (Portal{"10.0.0.9", 3261}) {
		t.Errorf("dials = %+v", h.dialer.dials)
	}
	// The persistent portal stays the one the caller asked for.
	if got := h.kern.sessions[1].params[paramPersistentAddress]; got != "10.0.0.1" {
		t.Errorf("persistent address = %q", got)
	}
	if !slices.Contains(h.kern.callLog(), "STOP_CONN 1:0 flag=3") {
		t.Error("redirect must unbind the first socket before re-binding")
	}
}

func TestRecoveryReloginsAfterConnError(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	firstISID := h.tgt.logins[0].ISID
	h.dialer.mu.Lock()
	h.dialer.fail = 2 // first two reconnects fail: retry loop
	h.dialer.mu.Unlock()
	before := len(h.kern.callLog())

	h.kern.connError(1)
	eventually(t, "session recovered", func() bool {
		return slices.Contains(h.kern.callLog()[before:], "START_CONN 1:0") &&
			h.sessionPhase() == phaseEstablished
	})
	calls := h.kern.callLog()[before:]
	if calls[0] != "STOP_CONN 1:0 flag=3" {
		t.Errorf("recovery must start with STOP_CONN(RECOVER), got %q", calls)
	}
	h.dialer.mu.Lock()
	dials := len(h.dialer.dials)
	h.dialer.mu.Unlock()
	if dials != 4 {
		t.Errorf("dials = %d, want 1 initial + 2 failed + 1 successful", dials)
	}
	h.tgt.mu.Lock()
	relogin := h.tgt.logins[len(h.tgt.logins)-2]
	h.tgt.mu.Unlock()
	if relogin.ISID != firstISID || relogin.TSIH != 0x1234 {
		t.Errorf("re-login ISID=%s TSIH=%#x, want same ISID %s and previous TSIH 0x1234",
			relogin.ISID, relogin.TSIH, firstISID)
	}
	s, err := h.ini.FindSession(testTarget, h.params().Portal)
	if err != nil || s == nil || s.State != "LOGGED_IN" || s.SID != 1 {
		t.Errorf("after recovery FindSession = %+v, %v", s, err)
	}
}

func TestRecoveryFallsBackToNewSessionWhenTSIHUnknownToTarget(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	h.tgt.mu.Lock()
	h.tgt.tsih = 0x2222
	h.tgt.status = func(_ int, req *loginRequestView) (uint16, []textKV) {
		if req.TSIH != 0 {
			return 0x020a, nil // session does not exist
		}
		return 0, nil
	}
	h.tgt.mu.Unlock()
	h.kern.connError(1)
	eventually(t, "recovered", func() bool {
		return h.sessionPhase() == phaseEstablished && h.recoveredTSIH() == 0x2222
	})
}

func (h *harness) recoveredTSIH() uint16 {
	h.ini.mu.Lock()
	s := h.ini.byKey[sessionKey{target: testTarget, portal: h.params().Portal}]
	h.ini.mu.Unlock()
	if s == nil {
		return 0
	}
	if err := s.op.lock(context.Background()); err != nil {
		return 0
	}
	defer s.op.unlock()
	return s.tsih
}

func TestEventsForForeignSessionsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.start()
	// A session owned by someone else (e.g. another node sharing the kernel).
	h.kern.adoptExisting(7, 20, testPrefix+"other", "iqn.other:node", "LOGGED_IN", "up", Portal{"10.0.0.2", 3260})
	h.kern.emit(kernelEvent{Type: kEventConnError, SID: 7, CID: 0, Code: iscsiErrBase + 20})
	h.kern.emit(kernelEvent{Type: kEventRecvPDU, SID: 7, CID: 0, PDU: make([]byte, bhsLen)})
	time.Sleep(50 * time.Millisecond)
	if calls := h.kern.callLog(); len(calls) != 0 {
		t.Errorf("foreign session event triggered requests: %q", calls)
	}
	if s, err := h.ini.FindSession(testPrefix+"other", Portal{"10.0.0.2", 3260}); s != nil || err != nil {
		t.Errorf("foreign session visible: %+v, %v", s, err)
	}
}

func TestStartAdoptsAndRecoversOwnedSessions(t *testing.T) {
	h := newHarness(t)
	portal := Portal{Address: "10.0.0.1", Port: 3260}
	h.kern.adoptExisting(3, 13, testTarget, testInitiator, "LOGGED_IN", "up", portal)
	h.kern.adoptExisting(4, 14, testPrefix+"pool.vol2", testInitiator, "FAILED", "failed", portal)
	h.kern.adoptExisting(5, 15, testPrefix+"pool.vol3", "iqn.other:node", "FAILED", "failed", portal) // other node
	h.kern.adoptExisting(6, 16, "iqn.2000-01.com.example:foreign", testInitiator, "FAILED", "failed", portal)
	h.start()

	s, err := h.ini.FindSession(testTarget, portal)
	if err != nil || s == nil || s.SID != 3 || s.HostNo != 13 {
		t.Fatalf("adopted session = %+v, %v", s, err)
	}
	eventually(t, "failed owned session recovered", func() bool {
		return slices.Contains(h.kern.callLog(), "START_CONN 4:0")
	})
	calls := h.kern.callLog()
	if calls[0] != "STOP_CONN 4:0 flag=3" {
		t.Errorf("recovery of adopted session must STOP_CONN(RECOVER) first: %q", calls)
	}
	for _, c := range calls {
		if strings.Contains(c, " 3:0") || strings.Contains(c, " 5:0") || strings.Contains(c, " 6:0") {
			t.Errorf("unexpected request for a healthy or foreign session: %q", c)
		}
	}
	h.tgt.mu.Lock()
	last := h.tgt.logins[len(h.tgt.logins)-1]
	h.tgt.mu.Unlock()
	if last.TSIH != 0 || last.ISID != deriveISID(testInitiator, testPrefix+"pool.vol2", portal) {
		t.Errorf("adopted re-login TSIH=%#x ISID=%s", last.TSIH, last.ISID)
	}

	// Idempotent Login on an adopted session returns it without new requests.
	n := len(h.kern.callLog())
	got, err := h.ini.Login(context.Background(), h.params())
	if err != nil || got.SID != 3 || len(h.kern.callLog()) != n {
		t.Errorf("login on adopted session = %+v, %v", got, err)
	}
}

// The login timeout is userspace-only, so a session adopted after a
// restart starts with the default; the staged value must be restorable,
// either by a repeated Login or by SetLoginTimeout, and must bound the
// next recovery re-login.
func TestAdoptedSessionRecoversWithRestoredLoginTimeout(t *testing.T) {
	const configured = 45 * time.Second
	for name, restore := range map[string]func(h *harness) error{
		"Login": func(h *harness) error {
			p := h.params()
			p.LoginTimeout = configured
			_, err := h.ini.Login(context.Background(), p)
			return err
		},
		"SetLoginTimeout": func(h *harness) error {
			return h.ini.SetLoginTimeout(testTarget, h.params().Portal, configured)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.kern.adoptExisting(3, 13, testTarget, testInitiator, "LOGGED_IN", "up", h.params().Portal)
			h.start()
			if err := restore(h); err != nil {
				t.Fatalf("restore login timeout: %v", err)
			}

			h.kern.connError(3)
			eventually(t, "adopted session recovered", func() bool {
				return slices.Contains(h.kern.callLog(), "START_CONN 3:0") && h.sessionPhase() == phaseEstablished
			})
			h.dialer.mu.Lock()
			budgets := slices.Clone(h.dialer.budgets)
			h.dialer.mu.Unlock()
			if len(budgets) != 1 || budgets[0] <= DefaultLoginTimeout || budgets[0] > configured {
				t.Errorf("re-login budgets = %v, want one in (%v, %v]", budgets, DefaultLoginTimeout, configured)
			}
		})
	}
}

func TestSetLoginTimeoutWithoutSessionFails(t *testing.T) {
	h := newHarness(t)
	h.start()
	err := h.ini.SetLoginTimeout(testTarget, h.params().Portal, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "no iSCSI session") {
		t.Errorf("SetLoginTimeout without session = %v, want no-session error", err)
	}
}

func TestPeriodicScanRecoversSessionWithoutEvent(t *testing.T) {
	h := newHarness(t)
	h.ini.deps.scanInterval = 20 * time.Millisecond
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	before := len(h.kern.callLog())
	// The connection failed while no event reached us (e.g. process down).
	h.kern.mu.Lock()
	h.kern.setConnState(1, 0, "failed")
	writeFile(t, h.kern.sessionPath(1, "state"), "FAILED\n")
	h.kern.mu.Unlock()
	eventually(t, "scan-triggered recovery", func() bool {
		return slices.Contains(h.kern.callLog()[before:], "START_CONN 1:0")
	})
}

func TestLogoutTearsDownAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.start()
	s, err := h.ini.Login(context.Background(), h.params())
	if err != nil {
		t.Fatal(err)
	}
	mkSysfsLUN(t, h.root, uint32(s.SID), uint32(s.HostNo), 0, "sdb", "8:16") //nolint:gosec // G115: small test ids.
	writeFile(t, filepath.Join(h.root, "class", "scsi_host", "host11", "scan"), "")
	dev, err := h.ini.DeviceForLUN(context.Background(), s, 0)
	if err != nil {
		t.Fatal(err)
	}

	before := len(h.kern.callLog())
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Fatal(err)
	}
	calls := h.kern.callLog()[before:]
	want := []string{
		"FLUSH sdb", "DELETE 11:0:0:0",
		"SEND_PDU 1:0 op=0x06", "STOP_CONN 1:0 flag=1", "DESTROY_CONN 1:0", "DESTROY_SESSION 1",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("logout calls = %q, want %q", calls, want)
	}
	if h.tgt.logouts != 1 {
		t.Errorf("target saw %d logout PDUs", h.tgt.logouts)
	}
	if _, err := os.Stat(dev); !os.IsNotExist(err) {
		t.Errorf("device node %s created by us not removed: %v", dev, err)
	}
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Errorf("second logout = %v", err)
	}
	if n := len(h.kern.callLog()); n != before+len(want) {
		t.Errorf("second logout issued requests")
	}
}

// A failed flush-and-delete of a SCSI device of a live session is reported
// and the session is left intact, so a retry can still flush the data.
func TestLogoutReportsDeviceDeleteFailureWithoutDestroying(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	mkSysfsLUN(t, h.root, 1, 11, 0, "sdb", "8:16")
	hctl := mkSysfsLUN(t, h.root, 1, 11, 1, "sdc", "8:32")
	del := filepath.Join(h.root, "class", "scsi_device", hctl, "device", "delete")
	if err := os.Remove(del); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(del, 0o750); err != nil { // writes fail regardless of privileges
		t.Fatal(err)
	}

	before := len(h.kern.callLog())
	err := h.ini.Logout(context.Background(), testTarget, h.params().Portal)
	if err == nil || !strings.Contains(err.Error(), "flush and delete SCSI device 11:0:0:1") {
		t.Fatalf("logout error = %v", err)
	}
	if calls := h.kern.callLog()[before:]; !slices.Equal(calls, []string{"FLUSH sdb", "DELETE 11:0:0:0", "FLUSH sdc"}) {
		t.Errorf("calls after failed delete = %q, want only the first LUN's delete", calls)
	}
	if ph := h.sessionPhase(); ph != phaseEstablished {
		t.Fatalf("phase after failed logout = %v, want established (restored)", ph)
	}

	if err := os.Remove(del); err != nil {
		t.Fatal(err)
	}
	writeFile(t, del, "")
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Fatalf("retry logout = %v", err)
	}
	want := []string{
		"FLUSH sdb", "DELETE 11:0:0:0", "FLUSH sdc", "FLUSH sdc", "DELETE 11:0:0:1",
		"SEND_PDU 1:0 op=0x06", "STOP_CONN 1:0 flag=1", "DESTROY_CONN 1:0", "DESTROY_SESSION 1",
	}
	if calls := h.kern.callLog()[before:]; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// A failed flush of a live session's block device is reported before the
// device is deleted or the session touched.
func TestLogoutReportsFlushFailureWithoutDeleting(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	mkSysfsLUN(t, h.root, 1, 11, 0, "sdb", "8:16")
	h.nodes.mu.Lock()
	h.nodes.flushErr = errors.New("fsync /dev/sdb: input/output error")
	h.nodes.mu.Unlock()

	before := len(h.kern.callLog())
	err := h.ini.Logout(context.Background(), testTarget, h.params().Portal)
	if err == nil || !strings.Contains(err.Error(), "flush and delete SCSI device 11:0:0:0") ||
		!strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("logout error = %v", err)
	}
	if calls := h.kern.callLog()[before:]; len(calls) != 0 {
		t.Errorf("failed flush must not delete the device or tear down the session: %q", calls)
	}
	if ph := h.sessionPhase(); ph != phaseEstablished {
		t.Fatalf("phase after failed logout = %v, want established (restored)", ph)
	}
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatalf("login after failed logout = %v, want the existing session", err)
	}
}

func TestLogoutReportsKernelFailureAndCanBeRetried(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	h.kern.mu.Lock()
	h.kern.hook = func(ev *uevent) error {
		if ev.Type == uEventDestroySession {
			return errors.New("injected")
		}
		return nil
	}
	h.kern.mu.Unlock()
	err := h.ini.Logout(context.Background(), testTarget, h.params().Portal)
	if err == nil || !strings.Contains(err.Error(), "destroy session 1") {
		t.Fatalf("logout error = %v", err)
	}
	h.kern.mu.Lock()
	h.kern.hook = nil
	h.kern.mu.Unlock()
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Fatalf("retry logout = %v", err)
	}
	calls := h.kern.callLog()
	if n := strings.Count(strings.Join(calls, "\n"), "DESTROY_CONN"); n != 1 {
		t.Errorf("DESTROY_CONN issued %d times on retry: %q", n, calls)
	}
	if !slices.Contains(calls, "DESTROY_SESSION 1") {
		t.Errorf("session not destroyed on retry: %q", calls)
	}
}

// While a failed session's recovery timeout runs, the kernel queues the I/O
// of its SCSI devices; destroying the session would fail those writes.
// Logout refuses with ErrSessionRecovering and leaves recovery running, and
// once the connection is back a Logout flushes before destroying.
func TestLogoutRefusesFailedSessionWhileKernelQueuesIO(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	mkSysfsLUN(t, h.root, 1, 11, 0, "sdb", "8:16")
	h.dialer.mu.Lock()
	h.dialer.fail = 1 << 30 // target unreachable: recovery keeps retrying
	h.dialer.mu.Unlock()
	h.kern.connError(1)
	eventually(t, "recovery running", func() bool { return h.sessionPhase() == phaseRecovering })

	before := len(h.kern.callLog())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.ini.Logout(ctx, testTarget, h.params().Portal)
	if !errors.Is(err, ErrSessionRecovering) {
		t.Fatalf("logout of a FAILED session = %v, want ErrSessionRecovering", err)
	}
	for _, c := range h.kern.callLog()[before:] {
		if strings.HasPrefix(c, "DESTROY_") || strings.HasPrefix(c, "FLUSH") || strings.HasPrefix(c, "DELETE") ||
			c == "STOP_CONN 1:0 flag=1" || c == "SEND_PDU 1:0 op=0x06" {
			t.Errorf("refused logout issued %q", c)
		}
	}
	if ph := h.sessionPhase(); ph != phaseRecovering {
		t.Fatalf("phase after refused logout = %v, want recovering", ph)
	}
	h.dialer.mu.Lock()
	dials := len(h.dialer.dials)
	h.dialer.mu.Unlock()
	eventually(t, "recovery still retrying", func() bool {
		h.dialer.mu.Lock()
		defer h.dialer.mu.Unlock()
		return len(h.dialer.dials) > dials
	})

	h.dialer.mu.Lock()
	h.dialer.fail = 0 // connection restored
	h.dialer.mu.Unlock()
	eventually(t, "session recovered", func() bool { return h.sessionPhase() == phaseEstablished })
	before = len(h.kern.callLog())
	if err := h.ini.Logout(ctx, testTarget, h.params().Portal); err != nil {
		t.Fatalf("logout after recovery = %v", err)
	}
	want := []string{
		"FLUSH sdb", "DELETE 11:0:0:0",
		"SEND_PDU 1:0 op=0x06", "STOP_CONN 1:0 flag=1", "DESTROY_CONN 1:0", "DESTROY_SESSION 1",
	}
	if calls := h.kern.callLog()[before:]; !slices.Equal(calls, want) {
		t.Errorf("logout calls after recovery = %q, want %q", calls, want)
	}
}

// Once the recovery timeout expired (sysfs state FREE) the kernel already
// failed the queued I/O back, so there is nothing left to flush: Logout
// tears the session down without touching its devices and stops recovery.
func TestLogoutTearsDownSessionWhoseRecoveryTimedOut(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	hctl := mkSysfsLUN(t, h.root, 1, 11, 0, "sdb", "8:16")
	h.dialer.mu.Lock()
	h.dialer.fail = 1 << 30
	h.dialer.mu.Unlock()
	h.kern.connError(1)
	// The first attempt STOP_CONN(RECOVER)s the connection (rewriting FAILED);
	// later attempts fail at the dial.
	eventually(t, "recovery stopped the failed connection", func() bool {
		return slices.Contains(h.kern.callLog(), "STOP_CONN 1:0 flag=3")
	})
	h.kern.mu.Lock()
	writeFile(t, h.kern.sessionPath(1, "state"), "FREE\n") // session_recovery_timedout
	h.kern.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.ini.Logout(ctx, testTarget, h.params().Portal); err != nil {
		t.Fatal(err)
	}
	calls := h.kern.callLog()
	if !slices.Contains(calls, "DESTROY_SESSION 1") {
		t.Errorf("session not destroyed: %q", calls)
	}
	if slices.Contains(calls, "SEND_PDU 1:0 op=0x06") {
		t.Error("logout PDU must not be sent on a failed connection")
	}
	if slices.Contains(calls, "DELETE "+hctl) || slices.Contains(calls, "FLUSH sdb") {
		t.Error("SCSI devices whose I/O the kernel already failed must not be flushed or deleted")
	}
	if ph := h.sessionPhase(); ph != phaseRemoved {
		t.Errorf("phase after logout = %v, want removed", ph)
	}
}

// A FAILED session without SCSI devices has no queued I/O to protect (e.g.
// Attach rolling back after the LUN never appeared): Logout tears it down
// and cancels its recovery.
func TestLogoutTearsDownFailedSessionWithoutDevices(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	h.dialer.mu.Lock()
	h.dialer.fail = 1 << 30
	h.dialer.mu.Unlock()
	h.kern.connError(1)
	eventually(t, "recovery running", func() bool { return h.sessionPhase() == phaseRecovering })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.ini.Logout(ctx, testTarget, h.params().Portal); err != nil {
		t.Fatal(err)
	}
	if calls := h.kern.callLog(); !slices.Contains(calls, "DESTROY_SESSION 1") {
		t.Errorf("session not destroyed: %q", calls)
	}
	h.dialer.mu.Lock()
	dials := len(h.dialer.dials)
	h.dialer.mu.Unlock()
	time.Sleep(10 * h.ini.opts.RecoveryRetryInterval)
	h.dialer.mu.Lock()
	defer h.dialer.mu.Unlock()
	if n := len(h.dialer.dials); n != dials {
		t.Errorf("recovery dialed %d more times after logout", n-dials)
	}
}

// A Logout whose wait for an in-flight operation is canceled has not
// touched the session: Login still returns it and a later connection
// failure is recovered.
func TestLogoutCanceledWhileWaitingKeepsSessionUsable(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	h.ini.mu.Lock()
	s := h.ini.byKey[sessionKey{target: testTarget, portal: h.params().Portal}]
	h.ini.mu.Unlock()
	s.op <- struct{}{} // an in-flight operation holds the session

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := h.ini.Logout(ctx, testTarget, h.params().Portal)
	cancel()
	s.op.unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("logout = %v, want the lock wait's deadline error", err)
	}
	if ph := h.sessionPhase(); ph != phaseEstablished {
		t.Fatalf("phase after canceled logout = %v, want established", ph)
	}
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatalf("login after canceled logout = %v, want the existing session", err)
	}
	before := len(h.kern.callLog())
	h.kern.connError(1)
	eventually(t, "recovery after canceled logout", func() bool {
		return slices.Contains(h.kern.callLog()[before:], "START_CONN 1:0") && h.sessionPhase() == phaseEstablished
	})
}

// A session the kernel already removed (no event reached us) must not make
// Logout fail forever: STOP_CONN is rejected for the stale SID, sysfs
// confirms the absence, and the entry is forgotten.
func TestLogoutOfSessionTheKernelAlreadyRemoved(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	h.dialer.mu.Lock()
	closedBefore := h.dialer.closed
	h.dialer.mu.Unlock()
	h.kern.vanish(1)

	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Fatalf("logout of a vanished session = %v", err)
	}
	if ph := h.sessionPhase(); ph != phaseRemoved {
		t.Fatalf("session entry not forgotten: phase %v", ph)
	}
	h.dialer.mu.Lock()
	closed := h.dialer.closed - closedBefore
	h.dialer.mu.Unlock()
	if closed != 1 {
		t.Errorf("socket closed %d times, want 1", closed)
	}
	before := len(h.kern.callLog())
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Errorf("second logout = %v", err)
	}
	if calls := h.kern.callLog()[before:]; len(calls) != 0 {
		t.Errorf("second logout issued requests: %q", calls)
	}
}

// A rejected teardown request while the session still exists is a real
// failure: Logout reports it and keeps the entry for a retry.
func TestLogoutReportsStopConnFailureOfPresentSession(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	h.kern.mu.Lock()
	h.kern.hook = func(ev *uevent) error {
		if ev.Type == uEventStopConn {
			return errors.New("injected") // answered with EINVAL
		}
		return nil
	}
	h.kern.mu.Unlock()
	err := h.ini.Logout(context.Background(), testTarget, h.params().Portal)
	if err == nil || !strings.Contains(err.Error(), "stop connection 1:0") {
		t.Fatalf("logout error = %v", err)
	}
	if ph := h.sessionPhase(); ph != phaseLoggingOut {
		t.Fatalf("phase after failed logout = %v, want logging out", ph)
	}
	h.kern.mu.Lock()
	h.kern.hook = nil
	h.kern.mu.Unlock()
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Fatalf("retry logout = %v", err)
	}
	if !slices.Contains(h.kern.callLog(), "DESTROY_SESSION 1") {
		t.Errorf("session not destroyed on retry: %q", h.kern.callLog())
	}
}

// ISCSI_KEVENT_DESTROY_SESSION for an entry left logging out by a failed
// Logout forgets it, so the next Logout is a no-op.
func TestKernelDestroyForgetsSessionBeingLoggedOut(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	h.kern.mu.Lock()
	h.kern.hook = func(ev *uevent) error {
		if ev.Type == uEventStopConn {
			return errors.New("injected")
		}
		return nil
	}
	h.kern.mu.Unlock()
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err == nil {
		t.Fatal("logout succeeded despite STOP_CONN failure")
	}
	host := h.kern.vanish(1)
	h.kern.emit(kernelEvent{Type: kEventDestroySession, SID: 1, HostNo: host})
	eventually(t, "entry forgotten", func() bool { return h.sessionPhase() == phaseRemoved })

	before := len(h.kern.callLog())
	if err := h.ini.Logout(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Errorf("logout after kernel destroy = %v", err)
	}
	if calls := h.kern.callLog()[before:]; len(calls) != 0 {
		t.Errorf("logout after kernel destroy issued requests: %q", calls)
	}
}

func TestConcurrentLoginsShareOneSession(t *testing.T) {
	h := newHarness(t)
	h.start()
	var wg sync.WaitGroup
	sids := make([]int, 8)
	errs := make([]error, 8)
	for n := range 8 {
		wg.Go(func() {
			s, err := h.ini.Login(context.Background(), h.params())
			errs[n] = err
			if s != nil {
				sids[n] = s.SID
			}
		})
	}
	wg.Wait()
	for n := range 8 {
		if errs[n] != nil || sids[n] != 1 {
			t.Errorf("login %d = sid %d, %v", n, sids[n], errs[n])
		}
	}
	if n := strings.Count(strings.Join(h.kern.callLog(), "\n"), "CREATE_SESSION"); n != 1 {
		t.Errorf("CREATE_SESSION issued %d times", n)
	}
}

func TestDeviceForLUNScansAndCreatesNode(t *testing.T) {
	h := newHarness(t)
	h.start()
	s, err := h.ini.Login(context.Background(), h.params())
	if err != nil {
		t.Fatal(err)
	}
	scan := filepath.Join(h.root, "class", "scsi_host", "host11", "scan")
	writeFile(t, scan, "")
	go func() {
		time.Sleep(30 * time.Millisecond) // LUN shows up after the scan
		mkSysfsLUN(t, h.root, 1, 11, 2, "", "")
		time.Sleep(30 * time.Millisecond)
		mkSysfsLUN(t, h.root, 1, 11, 2, "sdc", "8:32")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dev, err := h.ini.DeviceForLUN(ctx, s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if dev != filepath.Join(h.dev, "sdc") {
		t.Errorf("device = %s", dev)
	}
	if got := readFileT(t, scan); got != "- - 2" {
		t.Errorf("scan write = %q", got)
	}
	if h.nodes.nodes[dev] != [2]uint32{8, 32} {
		t.Errorf("device node = %v", h.nodes.nodes[dev])
	}

	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := h.ini.DeviceForLUN(short, s, 5); err == nil || !strings.Contains(err.Error(), "LUN 5") {
		t.Errorf("missing LUN -> %v", err)
	}
}

// TestDeviceForLUNWaitsUntilDeviceOpens covers the kernel registration
// window: sysfs already lists the disk and its dev attribute while opening
// the disk still fails.  A device returned in that window reads as blank to
// blkid, and the node plugin would format it, so DeviceForLUN must keep
// waiting until the node opens, report the window when it times out, and
// fail on any other open error.
func TestDeviceForLUNWaitsUntilDeviceOpens(t *testing.T) {
	h := newHarness(t)
	h.start()
	s, err := h.ini.Login(context.Background(), h.params())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(h.root, "class", "scsi_host", "host11", "scan"), "")
	mkSysfsLUN(t, h.root, 1, 11, 0, "sda", "8:0")

	h.nodes.mu.Lock()
	h.nodes.unopenable = 3
	h.nodes.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dev, err := h.ini.DeviceForLUN(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dev != filepath.Join(h.dev, "sda") {
		t.Errorf("device = %s", dev)
	}
	h.nodes.mu.Lock()
	probes := h.nodes.usableProbes
	h.nodes.mu.Unlock()
	if probes != 4 {
		t.Errorf("usable probes = %d, want 4 (three unopenable, then ready)", probes)
	}

	h.nodes.mu.Lock()
	h.nodes.unopenable = 1 << 30
	h.nodes.mu.Unlock()
	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := h.ini.DeviceForLUN(short, s, 0); err == nil || !strings.Contains(err.Error(), "cannot be opened yet") {
		t.Errorf("never-openable device -> %v, want a timeout naming the unopenable device", err)
	}

	h.nodes.mu.Lock()
	h.nodes.unopenable = 0
	h.nodes.usableErr = errors.New("permission denied")
	h.nodes.mu.Unlock()
	if _, err := h.ini.DeviceForLUN(ctx, s, 0); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("open failure -> %v, want the open error", err)
	}
}

func TestRescanWritesEveryLUN(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.ini.Login(context.Background(), h.params()); err != nil {
		t.Fatal(err)
	}
	a := mkSysfsLUN(t, h.root, 1, 11, 0, "sdb", "8:16")
	b := mkSysfsLUN(t, h.root, 1, 11, 1, "sdc", "8:32")
	if err := h.ini.Rescan(context.Background(), testTarget, h.params().Portal); err != nil {
		t.Fatal(err)
	}
	for _, hctl := range []string{a, b} {
		if got := readFileT(t, filepath.Join(h.root, "class", "scsi_device", hctl, "device", "rescan")); got != "1" {
			t.Errorf("%s rescan = %q", hctl, got)
		}
	}
	if err := h.ini.Rescan(context.Background(), testPrefix+"absent", h.params().Portal); err == nil {
		t.Error("rescan of absent session succeeded")
	}
}
