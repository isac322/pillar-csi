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
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// fakeCHAP configures the fake target's CHAP authentication, modeled on
// LIO's iscsi_target_auth.c.
type fakeCHAP struct {
	// The initiator credentials the ACL accepts.
	user, secret string
	// Non-empty mutualUser makes the target require and answer a mutual
	// challenge, with mutualSecret.
	mutualUser, mutualSecret string

	// Misbehavior knobs.
	selectNone      bool   // answer AuthMethod=None to a CHAP-only offer (downgrade)
	algorithm       string // CHAP_A to select instead of 5
	base64Challenge bool   // encode CHAP_C as 0b base64
	challenge       []byte // fixed challenge instead of the default
	skipMutual      bool   // transit without answering the mutual challenge
	wrongMutual     bool   // answer the mutual challenge with a wrong secret
	// beforeMutualAnswer runs (without the target lock) once the initiator's
	// mutual challenge is known, before the target answers it.
	beforeMutualAnswer func(challenge []byte)
}

// fakeCHAPLogin is the per-login CHAP progress of the fake target.
type fakeCHAPLogin struct {
	step      int // 0 await AuthMethod, 1 await CHAP_A, 2 await CHAP_N/R, 3 done
	id        byte
	challenge []byte
}

// fakeTarget is a scripted LIO-like login responder.
type fakeTarget struct {
	mu sync.Mutex
	// operational answers keyed by key name; overrides lioAnswers.
	answers map[string]string
	// status returns a non-zero status (class<<8|detail) for a login
	// attempt, plus extra text (e.g. TargetAddress); attempt counts
	// security-stage requests from 1.
	status func(attempt int, req *loginRequestView) (uint16, []textKV)
	tsih   uint16
	statSN uint32
	chap   *fakeCHAP

	attempts int
	logins   []loginRequestView // every request seen
	logouts  int
	// chapLogin is the CHAP progress of the login in flight.
	chapLogin fakeCHAPLogin
	// initiatorChallenges records every mutual challenge the initiator sent.
	initiatorChallenges [][]byte
}

type loginRequestView struct {
	Transit  bool
	CSG, NSG uint8
	ISID     isid
	TSIH     uint16
	CID      uint16
	Text     []textKV
}

func lioAnswers() map[string]string {
	return map[string]string{
		"HeaderDigest":             "None",
		"DataDigest":               "None",
		"MaxConnections":           "1",
		"InitialR2T":               "Yes",
		"ImmediateData":            "Yes",
		"MaxBurstLength":           "262144",
		"FirstBurstLength":         "65536",
		"DefaultTime2Wait":         "2",
		"DefaultTime2Retain":       "0",
		"MaxOutstandingR2T":        "1",
		"DataPDUInOrder":           "Yes",
		"DataSequenceInOrder":      "Yes",
		"ErrorRecoveryLevel":       "0",
		"IFMarker":                 "No",
		"OFMarker":                 "No",
		"MaxRecvDataSegmentLength": "65536",
	}
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{answers: lioAnswers(), tsih: 0x1234, statSN: 100}
}

func parseLoginRequestPDU(hdr, data []byte) (*loginRequestView, error) {
	if hdr[0]&opcodeMask != opLoginReq {
		return nil, fmt.Errorf("opcode 0x%02x is not a login request", hdr[0])
	}
	if hdr[0]&opImmediate == 0 {
		return nil, fmt.Errorf("login request without immediate bit")
	}
	if got := dataSegmentLength(hdr); got != len(data) {
		return nil, fmt.Errorf("DataSegmentLength %d != data %d", got, len(data))
	}
	text, err := decodeText(data)
	if err != nil {
		return nil, err
	}
	v := &loginRequestView{
		Transit: hdr[1]&flagTransit != 0,
		CSG:     (hdr[1] >> 2) & 3,
		NSG:     hdr[1] & 3,
		TSIH:    binary.BigEndian.Uint16(hdr[14:]),
		CID:     binary.BigEndian.Uint16(hdr[20:]),
		Text:    text,
	}
	copy(v.ISID[:], hdr[8:14])
	return v, nil
}

func buildLoginResponse(
	req *loginRequestView, transit bool, csg, nsg uint8, tsih uint16, statSN uint32, status uint16, text []textKV,
) []byte {
	data := encodeText(text)
	h := make([]byte, bhsLen, bhsLen+len(data))
	h[0] = opLoginRsp
	if transit {
		h[1] = flagTransit
	}
	h[1] |= (csg&3)<<2 | nsg&3
	putDataSegmentLength(h, len(data))
	copy(h[8:14], req.ISID[:])
	binary.BigEndian.PutUint16(h[14:], tsih)
	binary.BigEndian.PutUint32(h[24:], statSN)
	binary.BigEndian.PutUint16(h[36:], status) // status class, status detail
	return append(h, data...)
}

// respond answers one PDU (header + data as sent through SEND_PDU).
func (t *fakeTarget) respond(hdr, data []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if hdr[0]&opcodeMask == opLogoutReq {
		t.logouts++
		h := make([]byte, bhsLen)
		h[0] = opLogoutRsp
		h[1] = flagFinal
		return h, nil
	}
	req, err := parseLoginRequestPDU(hdr, data)
	if err != nil {
		return nil, err
	}
	t.logins = append(t.logins, *req)
	t.statSN++
	switch req.CSG {
	case stageSecurity:
		t.attempts++
		if t.status != nil {
			if st, extra := t.status(t.attempts, req); st != 0 {
				return buildLoginResponse(req, false, stageSecurity, 0, 0, t.statSN, st, extra), nil
			}
		}
		if t.chap != nil {
			return t.respondCHAP(req)
		}
		return buildLoginResponse(req, true, stageSecurity, stageOperational, 0, t.statSN, 0,
			[]textKV{{"AuthMethod", "None"}, {"TargetPortalGroupTag", "1"}}), nil
	case stageOperational:
		var text []textKV
		for _, kv := range req.Text {
			if a, ok := t.answers[kv.Key]; ok {
				text = append(text, textKV{kv.Key, a})
			}
		}
		tsih := req.TSIH
		if tsih == 0 {
			tsih = t.tsih
		}
		return buildLoginResponse(req, true, stageOperational, stageFullFeature, tsih, t.statSN, 0, text), nil
	}
	return nil, fmt.Errorf("unexpected CSG %d", req.CSG)
}

// authFailure is LIO's answer to a failed CHAP step: status class 2
// detail 1.
func (t *fakeTarget) authFailure(req *loginRequestView) []byte {
	t.chapLogin = fakeCHAPLogin{}
	return buildLoginResponse(req, false, stageSecurity, 0, 0, t.statSN, 0x0201, nil)
}

// respondCHAP runs one security-stage step of the CHAP exchange.  The
// caller holds t.mu.
func (t *fakeTarget) respondCHAP(req *loginRequestView) ([]byte, error) {
	c := t.chap
	if textValue(req.Text, "InitiatorName") != "" {
		t.chapLogin = fakeCHAPLogin{} // first request of a new login
	}
	l := &t.chapLogin
	switch l.step {
	case 0:
		offer := strings.Split(textValue(req.Text, "AuthMethod"), ",")
		if c.selectNone {
			return buildLoginResponse(req, true, stageSecurity, stageOperational, 0, t.statSN, 0,
				[]textKV{{"AuthMethod", "None"}, {"TargetPortalGroupTag", "1"}}), nil
		}
		if !slices.Contains(offer, "CHAP") {
			return t.authFailure(req), nil
		}
		l.step = 1
		return buildLoginResponse(req, false, stageSecurity, 0, 0, t.statSN, 0,
			[]textKV{{"AuthMethod", "CHAP"}, {"TargetPortalGroupTag", "1"}}), nil
	case 1:
		if !slices.Contains(strings.Split(textValue(req.Text, "CHAP_A"), ","), "5") {
			return t.authFailure(req), nil
		}
		l.step = 2
		l.id = 0x2a
		l.challenge = c.challenge
		if l.challenge == nil {
			l.challenge = bytes.Repeat([]byte{0x5a, 0xa5}, 8)
		}
		alg := c.algorithm
		if alg == "" {
			alg = "5"
		}
		enc := encodeCHAPBinary(l.challenge)
		if c.base64Challenge {
			enc = "0b" + base64.StdEncoding.EncodeToString(l.challenge)
		}
		return buildLoginResponse(req, false, stageSecurity, 0, 0, t.statSN, 0,
			[]textKV{{"CHAP_A", alg}, {"CHAP_I", strconv.Itoa(int(l.id))}, {"CHAP_C", enc}}), nil
	case 2:
		return t.verifyCHAPResponse(req)
	}
	return nil, fmt.Errorf("security request after CHAP completed")
}

// verifyCHAPResponse checks the initiator's CHAP_N/CHAP_R and answers its
// mutual challenge.  The caller holds t.mu; beforeMutualAnswer runs with it
// released.
func (t *fakeTarget) verifyCHAPResponse(req *loginRequestView) ([]byte, error) {
	c, l := t.chap, &t.chapLogin
	if !t.initiatorResponseValid(req) {
		return t.authFailure(req), nil
	}
	var text []textKV
	if c.mutualUser != "" {
		id, chal, ok := mutualChallengeOf(req)
		if !ok || bytes.Equal(chal, l.challenge) {
			return t.authFailure(req), nil // LIO also refuses a reflected challenge
		}
		t.initiatorChallenges = append(t.initiatorChallenges, chal)
		if c.beforeMutualAnswer != nil {
			t.mu.Unlock()
			c.beforeMutualAnswer(chal)
			t.mu.Lock()
		}
		secret := c.mutualSecret
		if c.wrongMutual {
			secret += "-wrong"
		}
		if !c.skipMutual {
			text = []textKV{{"CHAP_N", c.mutualUser}, {"CHAP_R", encodeCHAPBinary(chapResponse(id, secret, chal))}}
		}
	}
	l.step = 3
	return buildLoginResponse(req, req.Transit, stageSecurity, stageOperational, 0, t.statSN, 0, text), nil
}

// initiatorResponseValid reports whether the request carries the expected
// CHAP_N and CHAP_R.  The caller holds t.mu.
func (t *fakeTarget) initiatorResponseValid(req *loginRequestView) bool {
	c, l := t.chap, &t.chapLogin
	got, err := decodeCHAPBinary("CHAP_R", textValue(req.Text, "CHAP_R"))
	return err == nil && textValue(req.Text, "CHAP_N") == c.user &&
		bytes.Equal(got, chapResponse(l.id, c.secret, l.challenge))
}

// mutualChallengeOf parses the initiator's mutual CHAP_I and CHAP_C.
func mutualChallengeOf(req *loginRequestView) (id byte, challenge []byte, ok bool) {
	id, idErr := parseCHAPID(textValue(req.Text, "CHAP_I"))
	challenge, chalErr := decodeCHAPBinary("CHAP_C", textValue(req.Text, "CHAP_C"))
	return id, challenge, idErr == nil && chalErr == nil
}

// exchanger adapts the fake target to a loginExchanger.
func (t *fakeTarget) exchanger() loginExchanger {
	return func(_ context.Context, req *loginRequest) (*loginResponse, error) {
		hdr, data, err := req.marshal()
		if err != nil {
			return nil, err
		}
		pdu, err := t.respond(hdr, data)
		if err != nil {
			return nil, err
		}
		return parseLoginResponse(pdu)
	}
}
