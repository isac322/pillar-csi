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
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestLoginRequestBHS(t *testing.T) {
	req := &loginRequest{
		Transit: true, CSG: stageOperational, NSG: stageFullFeature,
		ISID: isid{0x80, 1, 2, 3, 4, 5}, TSIH: 0xbeef, ITT: 7, CID: 2, CmdSN: 9, ExpStatSN: 11,
		Data: []byte("a=b\x00"),
	}
	h, d, err := req.marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]byte{0: 0x43, 1: 0x87, 2: 0, 3: 0, 4: 0, 5: 0, 6: 0, 7: 4}
	for off, b := range want {
		if h[off] != b {
			t.Errorf("byte %d = %#x, want %#x", off, h[off], b)
		}
	}
	if !bytes.Equal(h[8:14], []byte{0x80, 1, 2, 3, 4, 5}) || binary.BigEndian.Uint16(h[14:]) != 0xbeef {
		t.Errorf("ISID/TSIH wrong: % x", h[8:16])
	}
	if binary.BigEndian.Uint32(h[16:]) != 7 || binary.BigEndian.Uint16(h[20:]) != 2 ||
		binary.BigEndian.Uint32(h[24:]) != 9 || binary.BigEndian.Uint32(h[28:]) != 11 {
		t.Errorf("ITT/CID/CmdSN/ExpStatSN wrong: % x", h[16:32])
	}
	if string(d) != "a=b\x00" {
		t.Errorf("data = %q (must be unpadded; the kernel pads)", d)
	}

	// Without T, NSG must be zero.
	h, _, err = (&loginRequest{CSG: stageSecurity, NSG: stageOperational}).marshal()
	if err != nil {
		t.Fatal(err)
	}
	if h[1] != 0x00 {
		t.Errorf("flags without transit = %#x", h[1])
	}
	if _, _, err := (&loginRequest{Data: make([]byte, maxLoginDataLen+1)}).marshal(); err == nil {
		t.Error("oversized login data accepted")
	}
}

func TestParseLoginResponse(t *testing.T) {
	req := &loginRequestView{ISID: isid{0x80, 9, 9, 9, 9, 9}}
	pdu := buildLoginResponse(req, true, stageOperational, stageFullFeature, 0x42, 77, 0,
		[]textKV{{"MaxRecvDataSegmentLength", "8192"}})
	pdu = append(pdu, 0, 0, 0) // padding delivered by the kernel must be ignored
	r, err := parseLoginResponse(pdu)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Transit || r.CSG != 1 || r.NSG != 3 || r.TSIH != 0x42 || r.StatSN != 77 || r.ISID != req.ISID {
		t.Errorf("parsed %+v", r)
	}
	kv, err := decodeText(r.Data)
	if err != nil || len(kv) != 1 || kv[0].Value != "8192" {
		t.Errorf("text = %+v, %v", kv, err)
	}

	rej := make([]byte, bhsLen)
	rej[0], rej[2] = opReject, 0x04
	if _, err := parseLoginResponse(rej); err == nil || !strings.Contains(err.Error(), "Reject") {
		t.Errorf("reject PDU -> %v", err)
	}
	short := buildLoginResponse(req, false, 0, 0, 0, 0, 0, []textKV{{"a", "b"}})
	if _, err := parseLoginResponse(short[:bhsLen+2]); err == nil {
		t.Error("truncated data segment accepted")
	}
}

func TestLogoutPDUs(t *testing.T) {
	h := marshalLogoutRequest()
	if h[0] != 0x46 || h[1] != 0x80 || len(h) != bhsLen {
		t.Errorf("logout request = % x", h[:4])
	}
	rsp := make([]byte, bhsLen)
	rsp[0], rsp[2] = opLogoutRsp, 1
	if code, err := parseLogoutResponse(rsp); err != nil || code != 1 {
		t.Errorf("logout response code = %d, %v", code, err)
	}
}

func TestTextCodec(t *testing.T) {
	kvs := []textKV{{"InitiatorName", "iqn.a:b"}, {"TargetAlias", ""}, {"X", "a=b"}}
	back, err := decodeText(append(encodeText(kvs), 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 || back[0] != kvs[0] || back[1] != kvs[1] || back[2] != kvs[2] {
		t.Errorf("round trip = %+v", back)
	}
	if _, err := decodeText([]byte("novalue\x00")); err == nil {
		t.Error("pair without '=' accepted")
	}
}

func testLoginInput() loginInput {
	return loginInput{
		InitiatorIQN: "iqn.2026-01.com.bhyoo.pillar-csi:node.a",
		TargetIQN:    "iqn.2026-01.com.bhyoo.pillar-csi:vol",
		ISID:         isid{0x80, 1, 2, 3, 4, 5},
	}
}

var testPortal = Portal{Address: "10.0.0.1", Port: 3260}

func textValue(kvs []textKV, key string) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

func TestLoginNegotiatesWithLIO(t *testing.T) {
	tgt := newFakeTarget()
	res, err := loginSession(context.Background(), tgt.exchanger(), testLoginInput(), testPortal)
	if err != nil {
		t.Fatal(err)
	}
	checkLIOLoginResult(t, res)

	if len(tgt.logins) != 2 {
		t.Fatalf("exchanges = %d, want 2", len(tgt.logins))
	}
	checkLIOLoginRequests(t, &tgt.logins[0], &tgt.logins[1])
}

func checkLIOLoginResult(t *testing.T, res *loginResult) {
	t.Helper()
	if res.TSIH != 0x1234 || res.TPGT != 1 || res.StatSN != 102 {
		t.Errorf("result TSIH=%#x TPGT=%d StatSN=%d", res.TSIH, res.TPGT, res.StatSN)
	}
	p := res.Params
	if p.MaxRecvDLength != 262144 {
		t.Errorf("MaxRecvDLength = %d, want our declared 262144", p.MaxRecvDLength)
	}
	if p.MaxXmitDLength != 65536 {
		t.Errorf("MaxXmitDLength = %d, want the target's declared 65536", p.MaxXmitDLength)
	}
	if !p.InitialR2T || !p.ImmediateData || p.MaxBurstLength != 262144 || p.FirstBurstLength != 65536 ||
		p.HeaderDigest || p.DataDigest || p.ErrorRecoveryLevel != 0 {
		t.Errorf("negotiated %+v", p)
	}
}

func checkLIOLoginRequests(t *testing.T, sec, op *loginRequestView) {
	t.Helper()
	if !sec.Transit || sec.CSG != 0 || sec.NSG != 1 || sec.TSIH != 0 {
		t.Errorf("security request %+v", sec)
	}
	for k, v := range map[string]string{
		"InitiatorName": "iqn.2026-01.com.bhyoo.pillar-csi:node.a",
		"TargetName":    "iqn.2026-01.com.bhyoo.pillar-csi:vol",
		"SessionType":   "Normal",
		"AuthMethod":    "None",
	} {
		if got := textValue(sec.Text, k); got != v {
			t.Errorf("security %s = %q, want %q", k, got, v)
		}
	}
	if !op.Transit || op.CSG != 1 || op.NSG != 3 {
		t.Errorf("operational request %+v", op)
	}
	if v := textValue(op.Text, "MaxRecvDataSegmentLength"); v != "262144" {
		t.Errorf("offered MaxRecvDataSegmentLength = %q", v)
	}
	if v := textValue(op.Text, "HeaderDigest"); v != "None" {
		t.Errorf("offered HeaderDigest = %q", v)
	}
}

func TestLoginTargetDeclaresSmallerLimits(t *testing.T) {
	tgt := newFakeTarget()
	tgt.answers["MaxRecvDataSegmentLength"] = "8192"
	tgt.answers["MaxBurstLength"] = "32768"
	tgt.answers["FirstBurstLength"] = "65536" // larger than MaxBurst: clamped
	tgt.answers["ImmediateData"] = "No"
	tgt.answers["InitialR2T"] = "No"
	tgt.answers["DataDigest"] = "NotUnderstood"
	res, err := loginSession(context.Background(), tgt.exchanger(), testLoginInput(), testPortal)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Params
	if p.MaxXmitDLength != 8192 || p.MaxBurstLength != 32768 || p.FirstBurstLength != 32768 ||
		p.ImmediateData || p.InitialR2T || p.DataDigest {
		t.Errorf("negotiated %+v", p)
	}
}

func TestLoginRejectsDigestAndBadValues(t *testing.T) {
	for name, tc := range map[string]struct{ key, val, want string }{
		"crc digest":     {"HeaderDigest", "CRC32C", "only None"},
		"bad bool":       {"ImmediateData", "Maybe", "invalid boolean"},
		"bad number":     {"MaxBurstLength", "lots", "invalid numeric"},
		"tiny recv size": {"MaxRecvDataSegmentLength", "100", "outside 512"},
	} {
		t.Run(name, func(t *testing.T) {
			tgt := newFakeTarget()
			tgt.answers[tc.key] = tc.val
			_, err := loginSession(context.Background(), tgt.exchanger(), testLoginInput(), testPortal)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoginStatusErrors(t *testing.T) {
	tgt := newFakeTarget()
	tgt.status = func(int, *loginRequestView) (uint16, []textKV) { return 0x0202, nil }
	_, err := loginSession(context.Background(), tgt.exchanger(), testLoginInput(), testPortal)
	var le *LoginError
	if !errors.As(err, &le) || le.StatusClass != 2 || le.StatusDetail != 2 || le.Retryable() {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "authorization failure") || !strings.Contains(err.Error(), "10.0.0.1:3260") {
		t.Errorf("message lacks detail/portal: %v", err)
	}

	tgt = newFakeTarget()
	tgt.status = func(int, *loginRequestView) (uint16, []textKV) { return 0x0301, nil }
	_, err = loginSession(context.Background(), tgt.exchanger(), testLoginInput(), testPortal)
	if !errors.As(err, &le) || !le.Retryable() {
		t.Errorf("service unavailable should be retryable: %v", err)
	}
}

func TestLoginRedirect(t *testing.T) {
	tgt := newFakeTarget()
	tgt.status = func(int, *loginRequestView) (uint16, []textKV) {
		return 0x0101, []textKV{{"TargetAddress", "[fd00::5]:3261,1"}}
	}
	_, err := loginSession(context.Background(), tgt.exchanger(), testLoginInput(), testPortal)
	var le *LoginError
	if !errors.As(err, &le) || le.Redirect == nil || *le.Redirect != (Portal{Address: "fd00::5", Port: 3261}) {
		t.Fatalf("err = %v", err)
	}

	tgt.status = func(int, *loginRequestView) (uint16, []textKV) { return 0x0102, nil }
	if _, err := loginSession(context.Background(), tgt.exchanger(), testLoginInput(), testPortal); err == nil ||
		!strings.Contains(err.Error(), "without TargetAddress") {
		t.Errorf("redirect without address -> %v", err)
	}
}

func TestParseTargetAddress(t *testing.T) {
	for in, want := range map[string]Portal{
		"10.1.1.1:3262,1":  {"10.1.1.1", 3262},
		"10.1.1.1":         {"10.1.1.1", 3260},
		"[fe80::1]":        {"fe80::1", 3260},
		"[fe80::1]:99,2":   {"fe80::1", 99},
		"target.local:860": {"target.local", 860},
	} {
		got, err := parseTargetAddress(in)
		if err != nil || got != want {
			t.Errorf("%q -> %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := parseTargetAddress("h:0"); err == nil {
		t.Error("port 0 accepted")
	}
}

func TestLoginRequiresNoAuth(t *testing.T) {
	ex := func(_ context.Context, req *loginRequest) (*loginResponse, error) {
		v := &loginRequestView{ISID: req.ISID}
		return parseLoginResponse(buildLoginResponse(v, false, stageSecurity, 0, 0, 1, 0,
			[]textKV{{"AuthMethod", "CHAP"}}))
	}
	_, err := loginSession(context.Background(), ex, testLoginInput(), testPortal)
	if err == nil || !strings.Contains(err.Error(), "AuthMethod=CHAP") {
		t.Errorf("err = %v", err)
	}
}

// LIO answers a login it rejects before parsing the request (TPG disabled)
// with a zeroed ISID; the status must still surface as a LoginError.
func TestLoginRejectWithZeroISID(t *testing.T) {
	ex := func(_ context.Context, _ *loginRequest) (*loginResponse, error) {
		return parseLoginResponse(buildLoginResponse(&loginRequestView{}, false, stageSecurity, 0, 0, 1, 0x0301, nil))
	}
	_, err := loginSession(context.Background(), ex, testLoginInput(), testPortal)
	var le *LoginError
	if !errors.As(err, &le) || le.StatusClass != 3 || le.StatusDetail != 1 || !le.Retryable() {
		t.Errorf("err = %v, want retryable service-unavailable LoginError", err)
	}
}

// A target that splits its response across PDUs (C bit) and asks for an
// extra round in the operational stage (T=0) must still complete.
func TestLoginContinueAndExtraRound(t *testing.T) {
	var reqs []*loginRequest
	step := 0
	ex := func(_ context.Context, req *loginRequest) (*loginResponse, error) {
		reqs = append(reqs, req)
		v := &loginRequestView{ISID: req.ISID}
		step++
		var pdu []byte
		switch step {
		case 1: // security, first half with C bit
			pdu = buildLoginResponse(v, false, 0, 0, 0, 1, 0, []textKV{{"AuthMethod", "None"}})
			pdu[1] |= flagContinue
		case 2: // rest of the security text
			pdu = buildLoginResponse(v, true, 0, 1, 0, 2, 0, []textKV{{"TargetPortalGroupTag", "7"}})
		case 3: // operational, not yet transiting; proposes an unknown key
			pdu = buildLoginResponse(v, false, 1, 0, 0, 3, 0, []textKV{{"MaxBurstLength", "4096"}, {"X-Vendor", "1"}})
		default:
			pdu = buildLoginResponse(v, true, 1, 3, 0x55, 4, 0, nil)
		}
		return parseLoginResponse(pdu)
	}
	res, err := loginSession(context.Background(), ex, testLoginInput(), testPortal)
	if err != nil {
		t.Fatal(err)
	}
	if res.TPGT != 7 || res.TSIH != 0x55 || res.Params.MaxBurstLength != 4096 || res.StatSN != 4 {
		t.Errorf("result %+v", res)
	}
	if len(reqs) != 4 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[1].Transit || len(reqs[1].Data) != 0 || reqs[1].ExpStatSN != 2 {
		t.Errorf("continue request must be empty with T=0 and ExpStatSN=2: %+v", reqs[1])
	}
	last, err := decodeText(reqs[3].Data)
	if err != nil {
		t.Fatal(err)
	}
	if v := textValue(last, "X-Vendor"); v != "NotUnderstood" {
		t.Errorf("unknown target key answered %q", v)
	}
}

func TestDeriveISIDStableAndRandomFormat(t *testing.T) {
	a := deriveISID("iqn.i", "iqn.t", Portal{"10.0.0.1", 3260})
	b := deriveISID("iqn.i", "iqn.t", Portal{"10.0.0.1", 3260})
	c := deriveISID("iqn.i", "iqn.t2", Portal{"10.0.0.1", 3260})
	if a != b || a == c {
		t.Errorf("ISIDs %s %s %s", a, b, c)
	}
	if a[0] != 0x80 {
		t.Errorf("ISID type byte = %#x, want random format 0x80", a[0])
	}
}
