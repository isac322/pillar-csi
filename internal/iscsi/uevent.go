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

// Wire format of the NETLINK_ISCSI protocol (include/scsi/iscsi_if.h).
//
// The struct iscsi_uevent is laid out identically on every LP64 Linux ABI we
// target (amd64, arm64), in host byte order (little endian on both):
//
//	offset size field
//	     0    4 type              (enum iscsi_uevent_e)
//	     4    4 iferror           (negative errno on ISCSI_KEVENT_IF_ERROR)
//	     8    8 transport_handle
//	    16   24 union u           (userspace -> kernel request arguments)
//	    40   16 union r           (kernel -> userspace results / events)
//	    56      end; the struct is __aligned(sizeof(uint64_t))
//
// The union u is 24 bytes because its largest members are msg_stop_conn
// {u32 sid; u32 cid; u64 conn_handle; u32 flag} (20 bytes + 4 tail padding to
// 8-byte alignment) and msg_create_bound_session {u64; u32; u16; u16} (16).
// The union r is 16 bytes: msg_recv_req {u32 sid; u32 cid; u64 recv_handle}.
// Variable data (PDU header+data, parameter strings) follows the struct.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// hostEndian is the byte order of the kernel ABI.  Both supported Linux
// architectures (amd64, arm64) are little endian.
var hostEndian = binary.LittleEndian

const (
	nlmsgHdrLen = 16 // sizeof(struct nlmsghdr)
	uEventLen   = 56 // sizeof(struct iscsi_uevent)

	nlmFRequest = 0x1 // NLM_F_REQUEST

	uEventUnionUOffset = 16
	uEventUnionROffset = 40
)

// enum iscsi_uevent_e.
const (
	uEventBase   = 10
	kEventBase   = 100
	iscsiErrBase = 1000

	uEventCreateSession  = uEventBase + 1
	uEventDestroySession = uEventBase + 2
	uEventCreateConn     = uEventBase + 3
	uEventDestroyConn    = uEventBase + 4
	uEventBindConn       = uEventBase + 5
	uEventSetParam       = uEventBase + 6
	uEventStartConn      = uEventBase + 7
	uEventStopConn       = uEventBase + 8
	uEventSendPDU        = uEventBase + 9

	kEventRecvPDU        = kEventBase + 1
	kEventConnError      = kEventBase + 2
	kEventIfError        = kEventBase + 3
	kEventDestroySession = kEventBase + 4
	kEventUnbindSession  = kEventBase + 5
	kEventCreateSession  = kEventBase + 6
	kEventConnLoginState = kEventBase + 9
)

// STOP_CONN flags.
const (
	stopConnTerm    = 0x1
	stopConnRecover = 0x3
)

// iscsiParam mirrors enum iscsi_param.  The order is ABI; never reorder.
type iscsiParam uint32

const (
	paramMaxRecvDLength iscsiParam = iota
	paramMaxXmitDLength
	paramHdrDgstEn
	paramDataDgstEn
	paramInitialR2TEn
	paramMaxR2T
	paramImmDataEn
	paramFirstBurst
	paramMaxBurst
	paramPDUInorderEn
	paramDataSeqInorderEn
	paramERL
	paramIFMarkerEn
	paramOFMarkerEn
	paramExpStatSN
	paramTargetName
	paramTPGT
	paramPersistentAddress
	paramPersistentPort
	paramSessRecoveryTmo
	paramConnPort
	paramConnAddress
	paramUsername
	paramUsernameIn
	paramPassword
	paramPasswordIn
	paramFastAbort
	paramAbortTmo
	paramLUResetTmo
	paramHostResetTmo
	paramPingTmo
	paramRecvTmo
	paramIfaceName
	paramISID
	paramInitiatorName
	paramTgtResetTmo
	paramTargetAlias
)

var paramNames = map[iscsiParam]string{
	paramMaxRecvDLength:    "MAX_RECV_DLENGTH",
	paramMaxXmitDLength:    "MAX_XMIT_DLENGTH",
	paramHdrDgstEn:         "HDRDGST_EN",
	paramDataDgstEn:        "DATADGST_EN",
	paramInitialR2TEn:      "INITIAL_R2T_EN",
	paramMaxR2T:            "MAX_R2T",
	paramImmDataEn:         "IMM_DATA_EN",
	paramFirstBurst:        "FIRST_BURST",
	paramMaxBurst:          "MAX_BURST",
	paramPDUInorderEn:      "PDU_INORDER_EN",
	paramDataSeqInorderEn:  "DATASEQ_INORDER_EN",
	paramERL:               "ERL",
	paramIFMarkerEn:        "IFMARKER_EN",
	paramOFMarkerEn:        "OFMARKER_EN",
	paramExpStatSN:         "EXP_STATSN",
	paramTargetName:        "TARGET_NAME",
	paramTPGT:              "TPGT",
	paramPersistentAddress: "PERSISTENT_ADDRESS",
	paramPersistentPort:    "PERSISTENT_PORT",
	paramSessRecoveryTmo:   "SESS_RECOVERY_TMO",
	paramAbortTmo:          "ABORT_TMO",
	paramLUResetTmo:        "LU_RESET_TMO",
	paramPingTmo:           "PING_TMO",
	paramRecvTmo:           "RECV_TMO",
	paramISID:              "ISID",
	paramInitiatorName:     "INITIATOR_NAME",
	paramTgtResetTmo:       "TGT_RESET_TMO",
}

func (p iscsiParam) String() string {
	if n, ok := paramNames[p]; ok {
		return n
	}
	return fmt.Sprintf("param(%d)", uint32(p))
}

var uEventNames = map[uint32]string{
	uEventCreateSession:  "CREATE_SESSION",
	uEventDestroySession: "DESTROY_SESSION",
	uEventCreateConn:     "CREATE_CONN",
	uEventDestroyConn:    "DESTROY_CONN",
	uEventBindConn:       "BIND_CONN",
	uEventSetParam:       "SET_PARAM",
	uEventStartConn:      "START_CONN",
	uEventStopConn:       "STOP_CONN",
	uEventSendPDU:        "SEND_PDU",
	kEventRecvPDU:        "RECV_PDU",
	kEventConnError:      "CONN_ERROR",
	kEventIfError:        "IF_ERROR",
	kEventDestroySession: "KEVENT_DESTROY_SESSION",
	kEventUnbindSession:  "UNBIND_SESSION",
	kEventCreateSession:  "KEVENT_CREATE_SESSION",
	kEventConnLoginState: "CONN_LOGIN_STATE",
}

func uEventName(t uint32) string {
	if n, ok := uEventNames[t]; ok {
		return n
	}
	return fmt.Sprintf("uevent(%d)", t)
}

// enum iscsi_err (reported in ISCSI_KEVENT_CONN_ERROR).
var iscsiErrNames = map[uint32]string{
	iscsiErrBase + 1:  "ISCSI_ERR_DATASN",
	iscsiErrBase + 2:  "ISCSI_ERR_DATA_OFFSET",
	iscsiErrBase + 3:  "ISCSI_ERR_MAX_CMDSN",
	iscsiErrBase + 4:  "ISCSI_ERR_EXP_CMDSN",
	iscsiErrBase + 5:  "ISCSI_ERR_BAD_OPCODE",
	iscsiErrBase + 6:  "ISCSI_ERR_DATALEN",
	iscsiErrBase + 7:  "ISCSI_ERR_AHSLEN",
	iscsiErrBase + 8:  "ISCSI_ERR_PROTO",
	iscsiErrBase + 9:  "ISCSI_ERR_LUN",
	iscsiErrBase + 10: "ISCSI_ERR_BAD_ITT",
	iscsiErrBase + 11: "ISCSI_ERR_CONN_FAILED",
	iscsiErrBase + 12: "ISCSI_ERR_R2TSN",
	iscsiErrBase + 13: "ISCSI_ERR_SESSION_FAILED",
	iscsiErrBase + 14: "ISCSI_ERR_HDR_DGST",
	iscsiErrBase + 15: "ISCSI_ERR_DATA_DGST",
	iscsiErrBase + 16: "ISCSI_ERR_PARAM_NOT_FOUND",
	iscsiErrBase + 17: "ISCSI_ERR_NO_SCSI_CMD",
	iscsiErrBase + 18: "ISCSI_ERR_INVALID_HOST",
	iscsiErrBase + 19: "ISCSI_ERR_XMIT_FAILED",
	iscsiErrBase + 20: "ISCSI_ERR_TCP_CONN_CLOSE",
	iscsiErrBase + 21: "ISCSI_ERR_SCSI_EH_SESSION_RST",
	iscsiErrBase + 22: "ISCSI_ERR_NOP_TIMEDOUT",
}

func connErrorName(code uint32) string {
	if n, ok := iscsiErrNames[code]; ok {
		return n
	}
	return fmt.Sprintf("iscsi_err(%d)", code)
}

// Linux errno values the kernel reports in iferror / retcode.  They are the
// generic asm-generic values shared by amd64 and arm64; kept here as
// constants so the pure-Go logic compiles and tests on every host OS.
const (
	linuxEPERM    = 1
	linuxENOENT   = 2
	linuxENOMEM   = 12
	linuxEBUSY    = 16
	linuxEEXIST   = 17
	linuxENODEV   = 19
	linuxEINVAL   = 22
	linuxENOSYS   = 38
	linuxENOTCONN = 107
)

var linuxErrnoNames = map[int32]string{
	linuxEPERM:    "EPERM",
	linuxENOENT:   "ENOENT",
	linuxENOMEM:   "ENOMEM",
	linuxEBUSY:    "EBUSY",
	linuxEEXIST:   "EEXIST",
	linuxENODEV:   "ENODEV",
	linuxEINVAL:   "EINVAL",
	linuxENOSYS:   "ENOSYS",
	linuxENOTCONN: "ENOTCONN",
}

// KernelError is a failed NETLINK_ISCSI request.
type KernelError struct {
	// Op is the request type name, e.g. "BIND_CONN".
	Op string
	// Errno is the positive Linux errno the kernel returned.
	Errno int32
}

func (e *KernelError) Error() string {
	name, ok := linuxErrnoNames[e.Errno]
	if !ok {
		name = fmt.Sprintf("errno %d", e.Errno)
	}
	return fmt.Sprintf("kernel rejected ISCSI_UEVENT_%s: %s", e.Op, name)
}

func isKernelErrno(err error, errno int32) bool {
	var ke *KernelError
	return errors.As(err, &ke) && ke.Errno == errno
}

// uevent is a decoded struct iscsi_uevent plus trailing payload.
type uevent struct {
	Type            uint32
	IfError         int32
	TransportHandle uint64
	U               [24]byte
	R               [16]byte
	Payload         []byte
}

func (ev *uevent) marshal() []byte {
	b := make([]byte, uEventLen+len(ev.Payload))
	hostEndian.PutUint32(b[0:], ev.Type)
	hostEndian.PutUint32(b[4:], uint32(ev.IfError)) //nolint:gosec // G115: s32 iferror reinterpreted as its wire bits.
	hostEndian.PutUint64(b[8:], ev.TransportHandle)
	copy(b[uEventUnionUOffset:], ev.U[:])
	copy(b[uEventUnionROffset:], ev.R[:])
	copy(b[uEventLen:], ev.Payload)
	return b
}

func unmarshalUevent(b []byte) (*uevent, error) {
	if len(b) < uEventLen {
		return nil, fmt.Errorf("decode iscsi_uevent: %d bytes, need %d", len(b), uEventLen)
	}
	ev := &uevent{
		Type:            hostEndian.Uint32(b[0:]),
		IfError:         int32(hostEndian.Uint32(b[4:])), //nolint:gosec // G115: wire bits of the s32 iferror.
		TransportHandle: hostEndian.Uint64(b[8:]),
		Payload:         b[uEventLen:],
	}
	copy(ev.U[:], b[uEventUnionUOffset:uEventUnionROffset])
	copy(ev.R[:], b[uEventUnionROffset:uEventLen])
	return ev, nil
}

// u32 helpers for the unions.
func putU32(b []byte, off int, v uint32) { hostEndian.PutUint32(b[off:], v) }
func getU32(b []byte, off int) uint32    { return hostEndian.Uint32(b[off:]) }

// Request builders.  Offsets below are within union u (see layout above).

func newCreateSessionReq(handle uint64, initialCmdSN uint32, cmdsMax, queueDepth uint16) *uevent {
	ev := &uevent{Type: uEventCreateSession, TransportHandle: handle}
	putU32(ev.U[:], 0, initialCmdSN)
	hostEndian.PutUint16(ev.U[4:], cmdsMax)
	hostEndian.PutUint16(ev.U[6:], queueDepth)
	return ev
}

func newSidReq(typ uint32, handle uint64, sid uint32) *uevent {
	ev := &uevent{Type: typ, TransportHandle: handle}
	putU32(ev.U[:], 0, sid)
	return ev
}

func newSidCidReq(typ uint32, handle uint64, sid, cid uint32) *uevent {
	ev := newSidReq(typ, handle, sid)
	putU32(ev.U[:], 4, cid)
	return ev
}

// msg_bind_conn {u32 sid; u32 cid; u64 transport_eph; u32 is_leading}.
func newBindConnReq(handle uint64, sid, cid uint32, fd uint64, leading bool) *uevent {
	ev := newSidCidReq(uEventBindConn, handle, sid, cid)
	hostEndian.PutUint64(ev.U[8:], fd)
	if leading {
		putU32(ev.U[:], 16, 1)
	}
	return ev
}

// msg_stop_conn {u32 sid; u32 cid; u64 conn_handle; u32 flag}.
func newStopConnReq(handle uint64, sid, cid, flag uint32) *uevent {
	ev := newSidCidReq(uEventStopConn, handle, sid, cid)
	putU32(ev.U[:], 16, flag)
	return ev
}

// msg_set_param {u32 sid; u32 cid; u32 param; u32 len} + NUL-terminated
// value.  The kernel requires strlen(value) <= len.
func newSetParamReq(handle uint64, sid, cid uint32, p iscsiParam, value string) *uevent {
	ev := newSidCidReq(uEventSetParam, handle, sid, cid)
	data := append([]byte(value), 0)
	putU32(ev.U[:], 8, uint32(p))
	putU32(ev.U[:], 12, uint32(len(data))) //nolint:gosec // G115: parameter strings are short.
	ev.Payload = data
	return ev
}

// msg_send_pdu {u32 sid; u32 cid; u32 hdr_size; u32 data_size} + hdr + data.
func newSendPDUReq(handle uint64, sid, cid uint32, hdr, data []byte) *uevent {
	ev := newSidCidReq(uEventSendPDU, handle, sid, cid)
	putU32(ev.U[:], 8, uint32(len(hdr)))   //nolint:gosec // G115: BHS is 48 bytes.
	putU32(ev.U[:], 12, uint32(len(data))) //nolint:gosec // G115: login data is capped at maxLoginDataLen.
	ev.Payload = append(append(make([]byte, 0, len(hdr)+len(data)), hdr...), data...)
	return ev
}

// replyErr converts a kernel reply into an error.  The kernel reports
// failures of iscsi_if_recv_msg by rewriting the type to
// ISCSI_KEVENT_IF_ERROR with a negative iferror; transport callbacks
// (BIND_CONN, START_CONN, SEND_PDU) report through r.retcode instead.
func (ev *uevent) replyErr(req uint32, checkRetcode bool) error {
	if ev.Type == kEventIfError || ev.IfError != 0 {
		return &KernelError{Op: uEventName(req), Errno: -ev.IfError}
	}
	if ev.Type != req {
		return fmt.Errorf("unexpected reply type %s to ISCSI_UEVENT_%s", uEventName(ev.Type), uEventName(req))
	}
	if checkRetcode {
		if rc := int32(getU32(ev.R[:], 0)); rc != 0 { //nolint:gosec // G115: wire bits of the s32 retcode.
			return &KernelError{Op: uEventName(req), Errno: -rc}
		}
	}
	return nil
}

// kernelEvent is an asynchronous (multicast) kernel notification.
type kernelEvent struct {
	Type   uint32
	SID    uint32
	CID    uint32
	HostNo uint32
	// Code is the enum iscsi_err for CONN_ERROR, or the enum
	// iscsi_conn_state for CONN_LOGIN_STATE.
	Code uint32
	// PDU is the received iSCSI header (48 bytes) plus data for RECV_PDU.
	PDU []byte
}

// decodeKernelEvent parses a multicast event.  The second result is false
// for event types this initiator does not consume.
func decodeKernelEvent(ev *uevent) (kernelEvent, bool) {
	r := ev.R[:]
	ke := kernelEvent{Type: ev.Type}
	switch ev.Type {
	case kEventRecvPDU: // msg_recv_req {sid, cid, u64 recv_handle}
		ke.SID, ke.CID = getU32(r, 0), getU32(r, 4)
		ke.PDU = ev.Payload
	case kEventConnError: // msg_conn_error {sid, cid, error}
		ke.SID, ke.CID, ke.Code = getU32(r, 0), getU32(r, 4), getU32(r, 8)
	case kEventConnLoginState: // msg_conn_login {sid, cid, state}
		ke.SID, ke.CID, ke.Code = getU32(r, 0), getU32(r, 4), getU32(r, 8)
	case kEventCreateSession: // msg_create_session_ret {sid, host_no}
		ke.SID, ke.HostNo = getU32(r, 0), getU32(r, 4)
	case kEventDestroySession: // msg_session_destroyed {host_no, sid}
		ke.HostNo, ke.SID = getU32(r, 0), getU32(r, 4)
	case kEventUnbindSession: // msg_unbind_session {sid, host_no}
		ke.SID, ke.HostNo = getU32(r, 0), getU32(r, 4)
	default:
		return ke, false
	}
	return ke, true
}

// Netlink message (nlmsg) framing.

func nlmsgAlign(n int) int { return (n + 3) &^ 3 }

// encodeNlmsg wraps payload in a struct nlmsghdr.  The nlmsg_len field
// covers the 4-byte-aligned payload (NLMSG_SPACE), matching open-iscsi.
func encodeNlmsg(typ, flags uint16, seq, pid uint32, payload []byte) []byte {
	total := nlmsgAlign(nlmsgHdrLen + len(payload))
	b := make([]byte, total)
	hostEndian.PutUint32(b[0:], uint32(total)) //nolint:gosec // G115: bounded by the netlink datagram size.
	hostEndian.PutUint16(b[4:], typ)
	hostEndian.PutUint16(b[6:], flags)
	hostEndian.PutUint32(b[8:], seq)
	hostEndian.PutUint32(b[12:], pid)
	copy(b[nlmsgHdrLen:], payload)
	return b
}

type nlmsg struct {
	Type uint16
	Seq  uint32
	Data []byte
}

// parseNlmsgs splits a netlink datagram into messages.
func parseNlmsgs(b []byte) ([]nlmsg, error) {
	var msgs []nlmsg
	for len(b) >= nlmsgHdrLen {
		l := int(hostEndian.Uint32(b[0:]))
		if l < nlmsgHdrLen || l > len(b) {
			return msgs, fmt.Errorf("decode netlink message: length %d outside [%d,%d]", l, nlmsgHdrLen, len(b))
		}
		msgs = append(msgs, nlmsg{
			Type: hostEndian.Uint16(b[4:]),
			Seq:  hostEndian.Uint32(b[8:]),
			Data: b[nlmsgHdrLen:l],
		})
		adv := nlmsgAlign(l)
		if adv > len(b) {
			break
		}
		b = b[adv:]
	}
	return msgs, nil
}
