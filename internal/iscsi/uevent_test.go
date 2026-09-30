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

// Byte-layout tests for struct iscsi_uevent (include/scsi/iscsi_if.h).  The
// expected offsets are derived by hand from the header for LP64 (amd64 and
// arm64 share them): 4+4+8 header, 24-byte union u at 16, 16-byte union r
// at 40, total 56.

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

func TestUeventHeaderLayout(t *testing.T) {
	ev := &uevent{Type: uEventStartConn, IfError: -22, TransportHandle: 0x1122334455667788}
	b := ev.marshal()
	if len(b) != 56 {
		t.Fatalf("sizeof(iscsi_uevent) = %d, want 56", len(b))
	}
	if le32(b, 0) != 17 {
		t.Errorf("type at 0 = %d, want ISCSI_UEVENT_START_CONN=17", le32(b, 0))
	}
	if iferror := int32(le32(b, 4)); iferror != -22 { //nolint:gosec // G115: s32 iferror wire bits.
		t.Errorf("iferror at 4 = %d", iferror)
	}
	if binary.LittleEndian.Uint64(b[8:]) != 0x1122334455667788 {
		t.Errorf("transport_handle at 8 = %#x", binary.LittleEndian.Uint64(b[8:]))
	}
	back, err := unmarshalUevent(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Type != ev.Type || back.IfError != ev.IfError || back.TransportHandle != ev.TransportHandle {
		t.Errorf("round trip mismatch: %+v", back)
	}
}

func TestUeventNumbersMatchKernelABI(t *testing.T) {
	for name, got := range map[string]uint32{
		"CREATE_SESSION":   uEventCreateSession,
		"DESTROY_SESSION":  uEventDestroySession,
		"CREATE_CONN":      uEventCreateConn,
		"DESTROY_CONN":     uEventDestroyConn,
		"BIND_CONN":        uEventBindConn,
		"SET_PARAM":        uEventSetParam,
		"START_CONN":       uEventStartConn,
		"STOP_CONN":        uEventStopConn,
		"SEND_PDU":         uEventSendPDU,
		"RECV_PDU":         kEventRecvPDU,
		"CONN_ERROR":       kEventConnError,
		"IF_ERROR":         kEventIfError,
		"K_DESTROY":        kEventDestroySession,
		"UNBIND_SESSION":   kEventUnbindSession,
		"CONN_LOGIN_STATE": kEventConnLoginState,
	} {
		want := map[string]uint32{
			"CREATE_SESSION": 11, "DESTROY_SESSION": 12, "CREATE_CONN": 13, "DESTROY_CONN": 14,
			"BIND_CONN": 15, "SET_PARAM": 16, "START_CONN": 17, "STOP_CONN": 18, "SEND_PDU": 19,
			"RECV_PDU": 101, "CONN_ERROR": 102, "IF_ERROR": 103, "K_DESTROY": 104, "UNBIND_SESSION": 105,
			"CONN_LOGIN_STATE": 109,
		}[name]
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	for p, want := range map[iscsiParam]uint32{
		paramMaxRecvDLength: 0, paramExpStatSN: 14, paramTargetName: 15, paramTPGT: 16,
		paramPersistentAddress: 17, paramPersistentPort: 18, paramSessRecoveryTmo: 19,
		paramAbortTmo: 27, paramLUResetTmo: 28, paramPingTmo: 30, paramRecvTmo: 31,
		paramISID: 33, paramInitiatorName: 34, paramTgtResetTmo: 35,
	} {
		if uint32(p) != want {
			t.Errorf("%s = %d, want %d", p, uint32(p), want)
		}
	}
}

func TestRequestUnionLayouts(t *testing.T) {
	t.Run("create_session", testCreateSessionLayout)
	t.Run("bind_conn", testBindConnLayout)
	t.Run("stop_conn", testStopConnLayout)
	t.Run("set_param", testSetParamLayout)
	t.Run("send_pdu", testSendPDULayout)
}

func testCreateSessionLayout(t *testing.T) {
	b := newCreateSessionReq(7, 0xaabbccdd, 128, 32).marshal()
	if le32(b, 16) != 0xaabbccdd || binary.LittleEndian.Uint16(b[20:]) != 128 ||
		binary.LittleEndian.Uint16(b[22:]) != 32 {
		t.Errorf("c_session layout wrong: % x", b[16:24])
	}
}

func testBindConnLayout(t *testing.T) {
	b := newBindConnReq(7, 3, 1, 0x0102030405060708, true).marshal()
	if le32(b, 16) != 3 || le32(b, 20) != 1 {
		t.Errorf("sid/cid = %d/%d", le32(b, 16), le32(b, 20))
	}
	if binary.LittleEndian.Uint64(b[24:]) != 0x0102030405060708 {
		t.Errorf("transport_eph at 24 = %#x", binary.LittleEndian.Uint64(b[24:]))
	}
	if le32(b, 32) != 1 {
		t.Errorf("is_leading at 32 = %d", le32(b, 32))
	}
}

func testStopConnLayout(t *testing.T) {
	b := newStopConnReq(7, 3, 1, stopConnRecover).marshal()
	if le32(b, 16) != 3 || le32(b, 20) != 1 || binary.LittleEndian.Uint64(b[24:]) != 0 || le32(b, 32) != 3 {
		t.Errorf("stop_conn layout wrong: % x", b[16:40])
	}
}

func testSetParamLayout(t *testing.T) {
	b := newSetParamReq(7, 3, 1, paramTargetName, "iqn.x").marshal()
	if le32(b, 24) != 15 || le32(b, 28) != 6 {
		t.Errorf("param/len = %d/%d, want 15/6", le32(b, 24), le32(b, 28))
	}
	if !bytes.Equal(b[56:], []byte("iqn.x\x00")) {
		t.Errorf("payload = %q", b[56:])
	}
}

func testSendPDULayout(t *testing.T) {
	hdr := bytes.Repeat([]byte{0xab}, 48)
	b := newSendPDUReq(7, 3, 1, hdr, []byte("k=v\x00")).marshal()
	if le32(b, 24) != 48 || le32(b, 28) != 4 {
		t.Errorf("hdr_size/data_size = %d/%d", le32(b, 24), le32(b, 28))
	}
	if !bytes.Equal(b[56:104], hdr) || string(b[104:]) != "k=v\x00" {
		t.Errorf("payload layout wrong")
	}
}

func eventBytes(typ uint32, r ...uint32) []byte {
	b := make([]byte, 56)
	binary.LittleEndian.PutUint32(b[0:], typ)
	for i, v := range r {
		binary.LittleEndian.PutUint32(b[40+4*i:], v)
	}
	return b
}

// decodeEventBytes unmarshals and decodes one multicast event.
func decodeEventBytes(t *testing.T, b []byte) (kernelEvent, bool) {
	t.Helper()
	ev, err := unmarshalUevent(b)
	if err != nil {
		t.Fatal(err)
	}
	return decodeKernelEvent(ev)
}

func TestDecodeKernelEvents(t *testing.T) {
	ke, ok := decodeEventBytes(t, eventBytes(kEventConnError, 5, 0, iscsiErrBase+20))
	if !ok || ke.SID != 5 || ke.CID != 0 || connErrorName(ke.Code) != "ISCSI_ERR_TCP_CONN_CLOSE" {
		t.Errorf("conn error decoded as %+v", ke)
	}
	pdu := append(eventBytes(kEventRecvPDU, 9, 2, 0xdead, 0), []byte("hdr+data")...)
	ke, ok = decodeEventBytes(t, pdu)
	if !ok || ke.SID != 9 || ke.CID != 2 || string(ke.PDU) != "hdr+data" {
		t.Errorf("recv pdu decoded as %+v", ke)
	}
	for name, b := range map[string][]byte{
		"destroy session": eventBytes(kEventDestroySession, 4, 9),
		"unbind session":  eventBytes(kEventUnbindSession, 9, 4),
		// msg_create_session_ret is {sid, host_no}, the reverse of msg_session_destroyed.
		"create session": eventBytes(kEventCreateSession, 9, 4),
	} {
		if ke, _ := decodeEventBytes(t, b); ke.HostNo != 4 || ke.SID != 9 {
			t.Errorf("%s decoded as %+v", name, ke)
		}
	}
	if _, ok := decodeEventBytes(t, eventBytes(110)); ok {
		t.Error("host event must be ignored")
	}
	if _, err := unmarshalUevent(make([]byte, 55)); err == nil {
		t.Error("short uevent accepted")
	}
}

func TestReplyErr(t *testing.T) {
	ifErr := &uevent{Type: kEventIfError, IfError: -linuxEINVAL}
	if err := ifErr.replyErr(uEventCreateConn, false); !isKernelErrno(err, linuxEINVAL) {
		t.Errorf("IF_ERROR -> %v", err)
	}
	rc := &uevent{Type: uEventBindConn}
	putU32(rc.R[:], 0, uint32(0xffffffff-linuxEEXIST+1)) // -EEXIST
	if err := rc.replyErr(uEventBindConn, true); !isKernelErrno(err, linuxEEXIST) {
		t.Errorf("retcode -> %v", err)
	}
	if err := rc.replyErr(uEventBindConn, false); err != nil {
		t.Errorf("retcode ignored when not checked, got %v", err)
	}
}

func TestNlmsgFraming(t *testing.T) {
	a := encodeNlmsg(uEventSetParam, nlmFRequest, 1, 0, []byte{1, 2, 3, 4, 5})
	if len(a) != 24 || le32(a, 0) != 24 {
		t.Fatalf("nlmsg_len = %d / buffer %d, want aligned 24", le32(a, 0), len(a))
	}
	if binary.LittleEndian.Uint16(a[4:]) != uEventSetParam || binary.LittleEndian.Uint16(a[6:]) != 1 ||
		le32(a, 8) != 1 {
		t.Errorf("header fields wrong: % x", a[:16])
	}
	b := encodeNlmsg(0, 0, 0, 0, bytes.Repeat([]byte{9}, 56))
	msgs, err := parseNlmsgs(append(a, b...))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Type != uEventSetParam || len(msgs[0].Data) != 8 ||
		msgs[1].Type != 0 || len(msgs[1].Data) != 56 {
		t.Errorf("parsed %+v", msgs)
	}
	bad := append([]byte(nil), a...)
	binary.LittleEndian.PutUint32(bad[0:], 999)
	if _, err := parseNlmsgs(bad); err == nil {
		t.Error("oversized nlmsg_len accepted")
	}
}
