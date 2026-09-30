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
	"encoding/binary"
	"fmt"
	"sync"
)

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

	attempts int
	logins   []loginRequestView // every request seen
	logouts  int
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
