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

package csi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/isac322/pillar-csi/internal/iscsi"
)

const (
	testInitiatorIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
	testTargetIQN    = "iqn.2026-01.com.bhyoo.pillar-csi:storage-1.tank.pvc-iscsi"
)

// fakeISCSIInitiator records calls and models a single initiator's session
// table so Login/Logout idempotency is observable.
type fakeISCSIInitiator struct {
	loginErr, deviceErr, rescanErr, logoutErr error

	device string

	logins        []iscsi.SessionParams
	luns          []int
	rescans       []iscsi.Portal
	logouts       []iscsi.Portal
	sessions      map[string]*iscsi.Session
	loginTimeouts map[string]time.Duration // SetLoginTimeout calls by target@portal
}

var _ ISCSIInitiator = (*fakeISCSIInitiator)(nil)

func newFakeISCSIInitiator() *fakeISCSIInitiator {
	return &fakeISCSIInitiator{
		device: "/dev/sdb", sessions: map[string]*iscsi.Session{}, loginTimeouts: map[string]time.Duration{},
	}
}

func (f *fakeISCSIInitiator) Login(_ context.Context, p iscsi.SessionParams) (*iscsi.Session, error) {
	f.logins = append(f.logins, p)
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	key := p.TargetIQN + "@" + p.Portal.String()
	s, ok := f.sessions[key]
	if !ok {
		s = &iscsi.Session{SID: len(f.sessions) + 1, TargetIQN: p.TargetIQN, Portal: p.Portal, State: "LOGGED_IN"}
		f.sessions[key] = s
	}
	return s, nil
}

func (f *fakeISCSIInitiator) DeviceForLUN(_ context.Context, _ *iscsi.Session, lun int) (string, error) {
	f.luns = append(f.luns, lun)
	return f.device, f.deviceErr
}

func (f *fakeISCSIInitiator) Rescan(_ context.Context, targetIQN string, portal iscsi.Portal) error {
	f.rescans = append(f.rescans, portal)
	if targetIQN == "" {
		return errors.New("empty target")
	}
	return f.rescanErr
}

func (f *fakeISCSIInitiator) Logout(_ context.Context, targetIQN string, portal iscsi.Portal) error {
	f.logouts = append(f.logouts, portal)
	if f.logoutErr != nil {
		return f.logoutErr
	}
	delete(f.sessions, targetIQN+"@"+portal.String())
	return nil
}

func (f *fakeISCSIInitiator) SetLoginTimeout(targetIQN string, portal iscsi.Portal, d time.Duration) error {
	key := targetIQN + "@" + portal.String()
	if _, ok := f.sessions[key]; !ok {
		return errors.New("no iSCSI session")
	}
	f.loginTimeouts[key] = d
	return nil
}

func iscsiAttachParams() AttachParams {
	return AttachParams{
		ProtocolType: ProtocolISCSI,
		ConnectionID: testTargetIQN,
		Address:      "192.168.1.10",
		Port:         "3260",
		VolumeRef:    "0",
	}
}

func TestISCSIHandler_AttachLogsInAndReturnsDevice(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	h := NewISCSIHandler(ini, testInitiatorIQN)

	params := iscsiAttachParams()
	params.VolumeRef = "3"
	params.Extra = map[string]string{
		VolumeContextKeyISCSILoginTimeout:       "30",
		VolumeContextKeyISCSIReplacementTimeout: "0",
		VolumeContextKeyISCSINoopOutInterval:    "10",
		VolumeContextKeyISCSINoopOutTimeout:     "7",
	}
	res, err := h.Attach(context.Background(), params)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if res.DevicePath != "/dev/sdb" || res.MountSource != "" {
		t.Errorf("result = %+v, want DevicePath /dev/sdb only", res)
	}
	want := iscsi.SessionParams{
		InitiatorIQN:       testInitiatorIQN,
		TargetIQN:          testTargetIQN,
		Portal:             iscsi.Portal{Address: "192.168.1.10", Port: 3260},
		LoginTimeout:       30 * time.Second,
		ReplacementTimeout: new(time.Duration(0)),
		NoopOutInterval:    new(10 * time.Second),
		NoopOutTimeout:     new(7 * time.Second),
	}
	if len(ini.logins) != 1 || !reflect.DeepEqual(ini.logins[0], want) {
		t.Fatalf("Login calls = %+v, want [%+v]", ini.logins, want)
	}
	if len(ini.luns) != 1 || ini.luns[0] != 3 {
		t.Errorf("DeviceForLUN luns = %v, want [3]", ini.luns)
	}
	wantState := &ISCSIProtocolState{
		TargetIQN: testTargetIQN, Address: "192.168.1.10", Port: "3260", LUN: 3, LoginTimeout: 30 * time.Second,
	}
	if st, ok := res.State.(*ISCSIProtocolState); !ok || *st != *wantState {
		t.Errorf("State = %#v, want %#v", res.State, wantState)
	}
}

// TestISCSIHandler_AttachDefaults verifies that an empty VolumeRef means LUN 0
// and absent timeout keys leave the timeouts unset (initiator defaults).
func TestISCSIHandler_AttachDefaults(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	h := NewISCSIHandler(ini, testInitiatorIQN)

	params := iscsiAttachParams()
	params.VolumeRef = ""
	if _, err := h.Attach(context.Background(), params); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	p := ini.logins[0]
	if p.LoginTimeout != 0 || p.ReplacementTimeout != nil || p.NoopOutInterval != nil || p.NoopOutTimeout != nil {
		t.Errorf("timeouts = %+v, want all unset (initiator defaults)", p)
	}
	if ini.luns[0] != 0 {
		t.Errorf("LUN = %d, want 0", ini.luns[0])
	}
}

// TestISCSIHandler_AttachIdempotent verifies that re-attaching (NodeStage
// retry) reuses the session instead of creating a second one.
func TestISCSIHandler_AttachIdempotent(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	h := NewISCSIHandler(ini, testInitiatorIQN)

	for range 2 {
		if _, err := h.Attach(context.Background(), iscsiAttachParams()); err != nil {
			t.Fatalf("Attach: %v", err)
		}
	}
	if len(ini.sessions) != 1 {
		t.Errorf("sessions = %d, want 1", len(ini.sessions))
	}
}

func TestISCSIHandler_AttachRejectsInvalidParams(t *testing.T) {
	t.Parallel()
	cases := map[string]func(p *AttachParams){
		"missing target IQN": func(p *AttachParams) { p.ConnectionID = "" },
		"missing address":    func(p *AttachParams) { p.Address = "" },
		"missing port":       func(p *AttachParams) { p.Port = "" },
		"port zero":          func(p *AttachParams) { p.Port = "0" },
		"port too large":     func(p *AttachParams) { p.Port = "65536" },
		"negative LUN":       func(p *AttachParams) { p.VolumeRef = "-1" },
		"LUN not a number":   func(p *AttachParams) { p.VolumeRef = "pvc-x" },
		"login timeout below one": func(p *AttachParams) {
			p.Extra = map[string]string{VolumeContextKeyISCSILoginTimeout: "0"}
		},
		"negative replacement": func(p *AttachParams) {
			p.Extra = map[string]string{VolumeContextKeyISCSIReplacementTimeout: "-5"}
		},
		"non-numeric noop timeout": func(p *AttachParams) {
			p.Extra = map[string]string{VolumeContextKeyISCSINoopOutTimeout: "5s"}
		},
		"noop interval overflow": func(p *AttachParams) {
			p.Extra = map[string]string{VolumeContextKeyISCSINoopOutInterval: "4294967296"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ini := newFakeISCSIInitiator()
			h := NewISCSIHandler(ini, testInitiatorIQN)
			p := iscsiAttachParams()
			mutate(&p)
			if _, err := h.Attach(context.Background(), p); err == nil {
				t.Fatal("expected error, got nil")
			}
			if len(ini.logins) != 0 {
				t.Errorf("Login called %d times for invalid params, want 0", len(ini.logins))
			}
		})
	}
}

// TestISCSIHandler_AttachPropagatesInitiatorErrors verifies initiator errors
// reach the caller and that a login whose LUN device never appears is
// rolled back: a failed NodeStageVolume gets no NodeUnstageVolume, so the
// session would otherwise outlive the volume.
func TestISCSIHandler_AttachPropagatesInitiatorErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("login rejected: authorization failure")

	ini := newFakeISCSIInitiator()
	ini.loginErr = sentinel
	_, err := NewISCSIHandler(ini, testInitiatorIQN).Attach(context.Background(), iscsiAttachParams())
	if !errors.Is(err, sentinel) {
		t.Errorf("login failure: got %v, want wrapping %v", err, sentinel)
	}
	if len(ini.logouts) != 0 {
		t.Errorf("login failure: logouts = %v, want none", ini.logouts)
	}

	ini = newFakeISCSIInitiator()
	ini.deviceErr = sentinel
	_, err = NewISCSIHandler(ini, testInitiatorIQN).Attach(context.Background(), iscsiAttachParams())
	if !errors.Is(err, sentinel) {
		t.Errorf("device failure: got %v, want wrapping %v", err, sentinel)
	}
	if len(ini.sessions) != 0 {
		t.Errorf("device failure: sessions = %v, want the login rolled back", ini.sessions)
	}

	logoutErr := errors.New("netlink: destroy session failed")
	ini = newFakeISCSIInitiator()
	ini.deviceErr = sentinel
	ini.logoutErr = logoutErr
	_, err = NewISCSIHandler(ini, testInitiatorIQN).Attach(context.Background(), iscsiAttachParams())
	if !errors.Is(err, sentinel) || !errors.Is(err, logoutErr) {
		t.Errorf("device and rollback failure: got %v, want wrapping %v and %v", err, sentinel, logoutErr)
	}
}

// TestISCSIHandler_DetachLogsOutIdempotently verifies Detach logs out of the
// session named by the state and that a second Detach (session gone) succeeds.
func TestISCSIHandler_DetachLogsOutIdempotently(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	h := NewISCSIHandler(ini, testInitiatorIQN)

	res, err := h.Attach(context.Background(), iscsiAttachParams())
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	for i := range 2 {
		if err := h.Detach(context.Background(), res.State); err != nil {
			t.Fatalf("Detach #%d: %v", i+1, err)
		}
	}
	want := iscsi.Portal{Address: "192.168.1.10", Port: 3260}
	if len(ini.logouts) != 2 || ini.logouts[0] != want {
		t.Errorf("Logout portals = %v, want [%v %v]", ini.logouts, want, want)
	}
	if len(ini.sessions) != 0 {
		t.Errorf("sessions after Detach = %d, want 0", len(ini.sessions))
	}
}

func TestISCSIHandler_DetachErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("netlink: destroy session failed")
	ini := newFakeISCSIInitiator()
	ini.logoutErr = sentinel
	h := NewISCSIHandler(ini, testInitiatorIQN)

	good := &ISCSIProtocolState{TargetIQN: testTargetIQN, Address: "10.0.0.1", Port: "3260"}
	if err := h.Detach(context.Background(), good); !errors.Is(err, sentinel) {
		t.Errorf("logout failure: got %v, want wrapping %v", err, sentinel)
	}
	for name, st := range map[string]ProtocolState{
		"nil":        nil,
		"nvme state": &NVMeoFProtocolState{SubsysNQN: "nqn.x"},
		"empty IQN":  &ISCSIProtocolState{Address: "10.0.0.1", Port: "3260"},
		"bad port":   &ISCSIProtocolState{TargetIQN: testTargetIQN, Address: "10.0.0.1", Port: "x"},
	} {
		if err := h.Detach(context.Background(), st); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestISCSIHandler_Rescan(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	h := NewISCSIHandler(ini, testInitiatorIQN)

	st := &ISCSIProtocolState{TargetIQN: testTargetIQN, Address: "fd00::10", Port: "3260"}
	if err := h.Rescan(context.Background(), st); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if want := (iscsi.Portal{Address: "fd00::10", Port: 3260}); len(ini.rescans) != 1 || ini.rescans[0] != want {
		t.Errorf("Rescan portals = %v, want [%v]", ini.rescans, want)
	}

	ini.rescanErr = errors.New("write rescan: permission denied")
	if err := h.Rescan(context.Background(), st); !errors.Is(err, ini.rescanErr) {
		t.Errorf("got %v, want wrapping %v", err, ini.rescanErr)
	}
}

// TestISCSIStageState_RoundTrip verifies the persisted iscsi sub-struct
// reconstructs exactly the ProtocolState Attach returned, so Detach after a
// node restart targets the same session.
func TestISCSIStageState_RoundTrip(t *testing.T) {
	t.Parallel()
	orig := &ISCSIProtocolState{
		TargetIQN: testTargetIQN, Address: "192.168.1.10", Port: "3260", LUN: 2, LoginTimeout: 45 * time.Second,
	}
	s := stageStateFromAttachResult(ProtocolISCSI, AccessTypeBlock, "ignored-iqn", "ignored", "1",
		&AttachResult{DevicePath: "/dev/sdc", State: orig})

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	if err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["iscsi"]; !ok {
		t.Fatalf("JSON lacks the iscsi key: %s", data)
	}
	if _, ok := raw["nvmeof"]; ok {
		t.Errorf("JSON of an iscsi state must not carry nvmeof: %s", data)
	}

	var back nodeStageState
	err = json.Unmarshal(data, &back)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ps, err := back.ToProtocolState()
	if err != nil {
		t.Fatalf("ToProtocolState: %v", err)
	}
	got, ok := ps.(*ISCSIProtocolState)
	if !ok || *got != *orig {
		t.Errorf("round trip = %#v, want %#v", ps, orig)
	}
}

// TestISCSIStageState_FallbackToVolumeContext verifies that without a concrete
// state the VolumeContext values and LUN 0 are persisted.
func TestISCSIStageState_FallbackToVolumeContext(t *testing.T) {
	t.Parallel()
	s := stageStateFromAttachResult(ProtocolISCSI, AccessTypeFilesystem, testTargetIQN, "10.0.0.2", "3260", nil)
	want := ISCSIStageState{TargetIQN: testTargetIQN, Address: "10.0.0.2", Port: "3260"}
	if s.ISCSI == nil || *s.ISCSI != want {
		t.Errorf("ISCSI = %+v, want %+v", s.ISCSI, want)
	}
	if _, err := (&nodeStageState{ProtocolType: ProtocolISCSI}).ToProtocolState(); err == nil {
		t.Error("ToProtocolState with nil iscsi sub-struct: expected error")
	}
}

// TestNodeStageUnstage_ISCSI drives NodeStageVolume and NodeUnstageVolume
// through the real ISCSIHandler: the LUN comes from the volume-ref key, the
// timeouts from the iscsi VolumeContext keys, and unstage logs out of the
// session recorded in the persisted state.
func TestNodeStageUnstage_ISCSI(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolISCSI: NewISCSIHandler(ini, testInitiatorIQN),
	})
	const volumeID = "storage-1/iscsi/zfs-zvol/tank/pvc-iscsi"
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID:          testTargetIQN,
			VolumeContextKeyAddress:           "192.168.1.10",
			VolumeContextKeyPort:              "3260",
			VolumeContextKeyProtocolType:      ProtocolISCSI,
			vcVolumeRef:                       "0",
			VolumeContextKeyISCSILoginTimeout: "20",
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if len(ini.logins) != 1 || ini.logins[0].LoginTimeout != 20*time.Second ||
		ini.logins[0].InitiatorIQN != testInitiatorIQN {
		t.Fatalf("Login calls = %+v", ini.logins)
	}

	st, err := env.srv.readStageState(volumeID)
	if err != nil || st == nil || st.ISCSI == nil {
		t.Fatalf("stage state = %+v, %v; want iscsi sub-struct", st, err)
	}
	if st.ISCSI.TargetIQN != testTargetIQN || st.ISCSI.LUN != 0 {
		t.Errorf("persisted iscsi state = %+v", st.ISCSI)
	}

	_, err = env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	if len(ini.logouts) != 1 || len(ini.sessions) != 0 {
		t.Errorf("logouts = %v, sessions = %d; want one logout and no session", ini.logouts, len(ini.sessions))
	}
}

// TestRestoreProtocolSessions_ISCSI verifies that after a pillar-node restart
// the login timeout a volume was staged with is re-applied to the session
// the initiator adopted from sysfs (the kernel does not keep it), and that a
// stage record written before the field existed falls back to the default
// with a log line.
func TestRestoreProtocolSessions_ISCSI(t *testing.T) {
	t.Parallel()
	before := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolISCSI: NewISCSIHandler(newFakeISCSIInitiator(), testInitiatorIQN),
	})
	_, err := before.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "storage-1/iscsi/zfs-zvol/tank/pvc-45",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID:          testTargetIQN,
			VolumeContextKeyAddress:           "192.168.1.10",
			VolumeContextKeyPort:              "3260",
			VolumeContextKeyProtocolType:      ProtocolISCSI,
			VolumeContextKeyISCSILoginTimeout: "45",
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	const legacyIQN = testTargetIQN + "-legacy"
	err = before.srv.writeStageState("tank/pvc-legacy", stageStateFromAttachResult(
		ProtocolISCSI, AccessTypeFilesystem, legacyIQN, "192.168.1.11", "3260", nil))
	if err != nil {
		t.Fatalf("writeStageState: %v", err)
	}

	// Restart: a new initiator that adopted both sessions from sysfs.
	ini := newFakeISCSIInitiator()
	ini.sessions[testTargetIQN+"@192.168.1.10:3260"] = &iscsi.Session{SID: 1}
	ini.sessions[legacyIQN+"@192.168.1.11:3260"] = &iscsi.Session{SID: 2}
	after := &NodeServer{
		handlers: map[string]ProtocolHandler{ProtocolISCSI: NewISCSIHandler(ini, testInitiatorIQN)},
		stateDir: before.stateDir,
	}
	var logs []string
	err = after.RestoreProtocolSessions(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("RestoreProtocolSessions: %v", err)
	}
	want := map[string]time.Duration{
		testTargetIQN + "@192.168.1.10:3260": 45 * time.Second,
		legacyIQN + "@192.168.1.11:3260":     0, // initiator default
	}
	if !reflect.DeepEqual(ini.loginTimeouts, want) {
		t.Errorf("restored login timeouts = %v, want %v", ini.loginTimeouts, want)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], legacyIQN) {
		t.Errorf("logs = %q, want one fallback line for %s", logs, legacyIQN)
	}

	// A staged volume whose session did not survive is reported, not skipped.
	delete(ini.sessions, legacyIQN+"@192.168.1.11:3260")
	err = after.RestoreProtocolSessions(func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), legacyIQN) {
		t.Errorf("restore with a vanished session = %v, want an error naming %s", err, legacyIQN)
	}
}

// TestNodeExpandVolume_ISCSIRescansBeforeResize verifies that NodeExpandVolume
// rescans the iSCSI LUN (the SCSI layer does not pick up a capacity change on
// its own) for both Block and Filesystem volumes, and that a rescan failure
// aborts the expansion.
func TestNodeExpandVolume_ISCSIRescansBeforeResize(t *testing.T) {
	t.Parallel()
	for _, block := range []bool{false, true} {
		ini := newFakeISCSIInitiator()
		env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
			ProtocolISCSI: NewISCSIHandler(ini, testInitiatorIQN),
		})
		resizer := &mockResizer{}
		env.srv.WithResizer(resizer)
		const volumeID = "tank/pvc-iscsi-expand"
		accessType := AccessTypeFilesystem
		volCap := mountCap("ext4")
		if block {
			accessType = AccessTypeBlock
			volCap = &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{
				Block: &csi.VolumeCapability_BlockVolume{},
			}}
		}
		if err := env.srv.writeStageState(volumeID, stageStateFromAttachResult(
			ProtocolISCSI, accessType, testTargetIQN, "10.0.0.3", "3260", nil)); err != nil {
			t.Fatalf("writeStageState: %v", err)
		}
		req := &csi.NodeExpandVolumeRequest{
			VolumeId:         volumeID,
			VolumePath:       t.TempDir(),
			VolumeCapability: volCap,
		}
		if _, err := env.srv.NodeExpandVolume(context.Background(), req); err != nil {
			t.Fatalf("block=%v: NodeExpandVolume: %v", block, err)
		}
		if want := (iscsi.Portal{Address: "10.0.0.3", Port: 3260}); len(ini.rescans) != 1 || ini.rescans[0] != want {
			t.Errorf("block=%v: rescans = %v, want [%v]", block, ini.rescans, want)
		}
		if wantResize := map[bool]int{false: 1, true: 0}[block]; resizer.called != wantResize {
			t.Errorf("block=%v: ResizeFS called %d times, want %d", block, resizer.called, wantResize)
		}

		ini.rescanErr = errors.New("rescan failed")
		_, err := env.srv.NodeExpandVolume(context.Background(), req)
		requireGRPCCode(t, err, codes.Internal)
	}
}

func TestEnabledLIOLUNs(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "target")
	lunDir := filepath.Join(root, "iscsi", testTargetIQN, "tpgt_1", "lun", "lun_0")

	if _, err := enabledLIOLUNs(root, testTargetIQN); err == nil {
		t.Error("absent configfs root: expected error (export cannot be verified)")
	}
	if err := os.MkdirAll(filepath.Join(root, "iscsi"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got, err := enabledLIOLUNs(root, testTargetIQN); err != nil || len(got) != 0 {
		t.Errorf("absent target: got %v, %v; want none, nil", got, err)
	}
	if err := os.MkdirAll(filepath.Dir(lunDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if got, err := enabledLIOLUNs(root, testTargetIQN); err != nil || len(got) != 0 {
		t.Errorf("target without lun_0: got %v, %v; want none, nil", got, err)
	}
	if err := os.Mkdir(lunDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if got, err := enabledLIOLUNs(root, testTargetIQN); err != nil || len(got) != 1 || got[0] != "lun_0" {
		t.Errorf("exported lun_0: got %v, %v; want [lun_0], nil", got, err)
	}
	if _, err := enabledLIOLUNs(root, "../escape"); err == nil {
		t.Error("IQN with a path separator: expected error")
	}
}
