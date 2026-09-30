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

// RFC 7143 Basic Header Segment encoding for the PDUs userspace exchanges
// with the target through the kernel: Login, Logout, and the Reject the
// target may answer with.  All iSCSI headers are big endian.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

const (
	bhsLen = 48

	opLoginReq   = 0x03
	opLogoutReq  = 0x06
	opLoginRsp   = 0x23
	opLogoutRsp  = 0x26
	opReject     = 0x3f
	opImmediate  = 0x40
	opcodeMask   = 0x3f
	flagTransit  = 0x80
	flagContinue = 0x40
	flagFinal    = 0x80

	// Login stages (CSG / NSG).
	stageSecurity    = 0
	stageOperational = 1
	stageFullFeature = 3

	// Kernel cap on a login PDU data segment
	// (ISCSI_DEF_MAX_RECV_SEG_LEN, libiscsi iscsi_alloc_mgmt_task).
	maxLoginDataLen = 8192

	logoutReasonCloseSession = 0
)

type isid [6]byte

func (i isid) String() string {
	return fmt.Sprintf("%02x%02x%02x%02x%02x%02x", i[0], i[1], i[2], i[3], i[4], i[5])
}

// loginRequest is a Login Request PDU.
type loginRequest struct {
	Transit   bool
	Continue  bool
	CSG, NSG  uint8
	ISID      isid
	TSIH      uint16
	ITT       uint32
	CID       uint16
	CmdSN     uint32
	ExpStatSN uint32
	Data      []byte
}

// marshal returns the 48-byte BHS and the unpadded data segment.  The
// kernel (libiscsi_tcp) adds the data-segment padding on transmit and
// rewrites ITT and CmdSN (__iscsi_conn_send_pdu).
func (r *loginRequest) marshal() (hdr, data []byte, err error) {
	if len(r.Data) > maxLoginDataLen {
		return nil, nil, fmt.Errorf("login request data segment is %d bytes, kernel limit is %d",
			len(r.Data), maxLoginDataLen)
	}
	h := make([]byte, bhsLen)
	h[0] = opImmediate | opLoginReq
	var f byte
	if r.Transit {
		f |= flagTransit
	}
	if r.Continue {
		f |= flagContinue
	}
	f |= (r.CSG & 0x3) << 2
	if r.Transit {
		f |= r.NSG & 0x3
	}
	h[1] = f
	h[2], h[3] = 0x00, 0x00 // VersionMax, VersionMin
	putDataSegmentLength(h, len(r.Data))
	copy(h[8:14], r.ISID[:])
	binary.BigEndian.PutUint16(h[14:], r.TSIH)
	binary.BigEndian.PutUint32(h[16:], r.ITT)
	binary.BigEndian.PutUint16(h[20:], r.CID)
	binary.BigEndian.PutUint32(h[24:], r.CmdSN)
	binary.BigEndian.PutUint32(h[28:], r.ExpStatSN)
	return h, r.Data, nil
}

// putDataSegmentLength stores the 24-bit DataSegmentLength field; only the
// low 24 bits of n are representable on the wire.
func putDataSegmentLength(h []byte, n int) {
	h[5] = byte(n >> 16) //nolint:gosec // G115: 24-bit wire field, truncation intended.
	h[6] = byte(n >> 8)  //nolint:gosec // G115: 24-bit wire field, truncation intended.
	h[7] = byte(n)       //nolint:gosec // G115: 24-bit wire field, truncation intended.
}

func dataSegmentLength(h []byte) int {
	return int(h[5])<<16 | int(h[6])<<8 | int(h[7])
}

// loginResponse is a parsed Login Response PDU.
type loginResponse struct {
	Transit       bool
	Continue      bool
	CSG, NSG      uint8
	VersionMax    uint8
	VersionActive uint8
	ISID          isid
	TSIH          uint16
	ITT           uint32
	StatSN        uint32
	ExpCmdSN      uint32
	MaxCmdSN      uint32
	StatusClass   uint8
	StatusDetail  uint8
	Data          []byte
}

// splitPDU separates a received PDU (as delivered by ISCSI_KEVENT_RECV_PDU:
// 48-byte header followed by the data segment) into header and data.
func splitPDU(pdu []byte) (hdr, data []byte, err error) {
	if len(pdu) < bhsLen {
		return nil, nil, fmt.Errorf("decode iSCSI PDU: %d bytes, need a %d-byte header", len(pdu), bhsLen)
	}
	hdr = pdu[:bhsLen]
	if ahs := int(hdr[4]) * 4; ahs != 0 {
		return nil, nil, fmt.Errorf("decode iSCSI PDU opcode 0x%02x: unexpected %d-byte AHS", hdr[0]&opcodeMask, ahs)
	}
	dl := dataSegmentLength(hdr)
	rest := pdu[bhsLen:]
	if dl > len(rest) {
		return nil, nil, fmt.Errorf("decode iSCSI PDU opcode 0x%02x: data segment length %d exceeds %d received bytes",
			hdr[0]&opcodeMask, dl, len(rest))
	}
	return hdr, rest[:dl], nil
}

func pduOpcode(pdu []byte) uint8 {
	if len(pdu) == 0 {
		return 0
	}
	return pdu[0] & opcodeMask
}

func parseLoginResponse(pdu []byte) (*loginResponse, error) {
	hdr, data, err := splitPDU(pdu)
	if err != nil {
		return nil, err
	}
	switch op := hdr[0] & opcodeMask; op {
	case opLoginRsp:
	case opReject:
		return nil, fmt.Errorf("target sent Reject PDU (reason 0x%02x) instead of a login response", hdr[2])
	default:
		return nil, fmt.Errorf("expected login response opcode 0x%02x, got 0x%02x", opLoginRsp, op)
	}
	r := &loginResponse{
		Transit:       hdr[1]&flagTransit != 0,
		Continue:      hdr[1]&flagContinue != 0,
		CSG:           (hdr[1] >> 2) & 0x3,
		NSG:           hdr[1] & 0x3,
		VersionMax:    hdr[2],
		VersionActive: hdr[3],
		TSIH:          binary.BigEndian.Uint16(hdr[14:]),
		ITT:           binary.BigEndian.Uint32(hdr[16:]),
		StatSN:        binary.BigEndian.Uint32(hdr[24:]),
		ExpCmdSN:      binary.BigEndian.Uint32(hdr[28:]),
		MaxCmdSN:      binary.BigEndian.Uint32(hdr[32:]),
		StatusClass:   hdr[36],
		StatusDetail:  hdr[37],
		Data:          data,
	}
	copy(r.ISID[:], hdr[8:14])
	if r.Transit && r.Continue {
		return nil, fmt.Errorf("decode login response: T and C bits both set")
	}
	return r, nil
}

// marshalLogoutRequest builds a Logout Request (close the session).  The
// kernel fills ITT, CmdSN and ExpStatSN.
func marshalLogoutRequest() []byte {
	h := make([]byte, bhsLen)
	h[0] = opImmediate | opLogoutReq
	h[1] = flagFinal | logoutReasonCloseSession
	return h
}

// parseLogoutResponse returns the Logout Response code (0 = success).
func parseLogoutResponse(pdu []byte) (uint8, error) {
	hdr, _, err := splitPDU(pdu)
	if err != nil {
		return 0, err
	}
	if op := hdr[0] & opcodeMask; op != opLogoutRsp {
		return 0, fmt.Errorf("expected logout response opcode 0x%02x, got 0x%02x", opLogoutRsp, op)
	}
	return hdr[2], nil
}

// textKV is one key=value pair of a login text segment.  Order matters on
// the wire, so pairs are kept as a slice.
type textKV struct {
	Key, Value string
}

func encodeText(kvs []textKV) []byte {
	var b bytes.Buffer
	for _, kv := range kvs {
		b.WriteString(kv.Key)
		b.WriteByte('=')
		b.WriteString(kv.Value)
		b.WriteByte(0)
	}
	return b.Bytes()
}

// decodeText parses NUL-separated key=value pairs, ignoring padding NULs.
func decodeText(data []byte) ([]textKV, error) {
	var kvs []textKV
	for f := range bytes.SplitSeq(data, []byte{0}) {
		if len(f) == 0 {
			continue
		}
		k, v, ok := strings.Cut(string(f), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("decode login text: malformed pair %q", f)
		}
		kvs = append(kvs, textKV{Key: k, Value: v})
	}
	return kvs, nil
}
