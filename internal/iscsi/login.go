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

// RFC 7143 login phase: security negotiation (AuthMethod=None, or CHAP when
// credentials are configured; see chap.go) followed by operational
// parameter negotiation, for a Normal session.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

// Initiator offers during operational negotiation.  They mirror open-iscsi's
// defaults so LIO sees the same session shape it sees from iscsiadm.
const (
	offerMaxRecvDataSegmentLength = 262144
	offerMaxBurstLength           = 16776192
	offerFirstBurstLength         = 262144
	offerMaxOutstandingR2T        = 1
	offerDefaultTime2Wait         = 2
	offerDefaultTime2Retain       = 0
	offerErrorRecoveryLevel       = 0

	// Upper bound on request/response exchanges, so a misbehaving target
	// cannot keep the login going forever.
	maxLoginRounds = 32
)

// Login text keys and values used in more than one place.
const (
	keyMaxRecvDataSegmentLength = "MaxRecvDataSegmentLength"
	valueNone                   = "None"
	valueYes                    = "Yes"
	valueNo                     = "No"
)

// negotiated holds the operational parameters agreed with the target.
type negotiated struct {
	HeaderDigest        bool
	DataDigest          bool
	MaxRecvDLength      uint32 // our declared MaxRecvDataSegmentLength
	MaxXmitDLength      uint32 // the target's declared MaxRecvDataSegmentLength
	InitialR2T          bool
	ImmediateData       bool
	MaxBurstLength      uint32
	FirstBurstLength    uint32
	MaxOutstandingR2T   uint32
	DataPDUInOrder      bool
	DataSequenceInOrder bool
	ErrorRecoveryLevel  uint32
	IFMarker            bool
	OFMarker            bool
	DefaultTime2Wait    uint32
	DefaultTime2Retain  uint32
}

// rfcDefaults are the values RFC 7143 section 13 assigns when a key is not
// negotiated (absent, Irrelevant, NotUnderstood or Reject).
func rfcDefaults() negotiated {
	return negotiated{
		MaxRecvDLength:      offerMaxRecvDataSegmentLength,
		MaxXmitDLength:      8192,
		InitialR2T:          true,
		ImmediateData:       true,
		MaxBurstLength:      262144,
		FirstBurstLength:    65536,
		MaxOutstandingR2T:   1,
		DataPDUInOrder:      true,
		DataSequenceInOrder: true,
		DefaultTime2Wait:    2,
		DefaultTime2Retain:  20,
	}
}

type loginInput struct {
	InitiatorIQN string
	TargetIQN    string
	ISID         isid
	TSIH         uint16
	CID          uint16
	// CHAP, when set, makes the login offer only AuthMethod=CHAP.
	CHAP *CHAPCredentials
}

type loginResult struct {
	TSIH        uint16
	TPGT        int // -1 when the target did not declare TargetPortalGroupTag
	StatSN      uint32
	TargetAlias string
	Params      negotiated
}

// loginExchanger sends one login request and returns the target's response.
type loginExchanger func(ctx context.Context, req *loginRequest) (*loginResponse, error)

// LoginError is a login the target refused (non-zero status class).
type LoginError struct {
	Target       string
	Portal       Portal
	StatusClass  uint8
	StatusDetail uint8
	// Redirect is the TargetAddress of a class 1 (redirection) response.
	Redirect *Portal
}

var loginStatusText = map[uint16]string{
	0x0101: "target moved temporarily",
	0x0102: "target moved permanently",
	0x0200: "initiator error",
	0x0201: "authentication failed: the target rejected the CHAP credentials or requires CHAP",
	0x0202: "authorization failure (initiator IQN not permitted by the target ACL)",
	0x0203: "target not found",
	0x0204: "target removed",
	0x0205: "unsupported iSCSI version",
	0x0206: "too many connections",
	0x0207: "missing parameter",
	0x0208: "cannot include connection in session",
	0x0209: "session type not supported",
	0x020a: "session does not exist",
	0x020b: "invalid request during login",
	0x0300: "target error",
	0x0301: "service unavailable",
	0x0302: "out of resources",
}

func (e *LoginError) Error() string {
	class := map[uint8]string{1: "redirection", 2: "initiator error", 3: "target error"}[e.StatusClass]
	if class == "" {
		class = "unknown class"
	}
	detail := loginStatusText[uint16(e.StatusClass)<<8|uint16(e.StatusDetail)]
	if detail == "" {
		detail = "unrecognized status detail"
	}
	msg := fmt.Sprintf("login to target %s at %s rejected: status class 0x%02x (%s) detail 0x%02x (%s)",
		e.Target, e.Portal, e.StatusClass, class, e.StatusDetail, detail)
	if e.Redirect != nil {
		msg += " to " + e.Redirect.String()
	}
	return msg
}

// Retryable reports whether a later attempt can succeed without operator
// action (redirects and target-side errors).
func (e *LoginError) Retryable() bool {
	return e.StatusClass == 1 || e.StatusClass == 3
}

// Is matches ErrAuthenticationFailed for status class 2 detail 1.
func (e *LoginError) Is(target error) bool {
	return target == ErrAuthenticationFailed && e.StatusClass == 2 && e.StatusDetail == 0x01
}

func (e *LoginError) sessionDoesNotExist() bool {
	return e.StatusClass == 2 && e.StatusDetail == 0x0a
}

// negotiation rules for operational keys.
type keyRule int

const (
	ruleMin keyRule = iota
	ruleMax
	ruleOr
	ruleAnd
	ruleDigest
)

type operationalKey struct {
	name  string
	rule  keyRule
	offer string
}

func operationalOffers() []operationalKey {
	return []operationalKey{
		{"HeaderDigest", ruleDigest, valueNone},
		{"DataDigest", ruleDigest, valueNone},
		{"MaxConnections", ruleMin, "1"},
		{"InitialR2T", ruleOr, valueNo},
		{"ImmediateData", ruleAnd, valueYes},
		{"MaxBurstLength", ruleMin, strconv.Itoa(offerMaxBurstLength)},
		{"FirstBurstLength", ruleMin, strconv.Itoa(offerFirstBurstLength)},
		{"DefaultTime2Wait", ruleMax, strconv.Itoa(offerDefaultTime2Wait)},
		{"DefaultTime2Retain", ruleMin, strconv.Itoa(offerDefaultTime2Retain)},
		{"MaxOutstandingR2T", ruleMin, strconv.Itoa(offerMaxOutstandingR2T)},
		{"DataPDUInOrder", ruleOr, valueYes},
		{"DataSequenceInOrder", ruleOr, valueYes},
		{"ErrorRecoveryLevel", ruleMin, strconv.Itoa(offerErrorRecoveryLevel)},
		{"IFMarker", ruleAnd, valueNo},
		{"OFMarker", ruleAnd, valueNo},
	}
}

func isNonValue(v string) bool {
	switch v {
	case "Irrelevant", "NotUnderstood", "Reject":
		return true
	}
	return false
}

func parseBoolKey(key, v string) (bool, error) {
	switch v {
	case valueYes:
		return true, nil
	case valueNo:
		return false, nil
	}
	return false, fmt.Errorf("login key %s: invalid boolean value %q", key, v)
}

func parseNumKey(key, v string) (uint32, error) {
	n, err := strconv.ParseUint(v, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("login key %s: invalid numeric value %q: %w", key, v, err)
	}
	return uint32(n), nil
}

// resolveKey combines our offer with the target's answer.
func resolveKey(k operationalKey, answer string) (string, error) {
	switch k.rule {
	case ruleDigest:
		if slices.Contains(strings.Split(answer, ","), valueNone) {
			return valueNone, nil
		}
		return "", fmt.Errorf("login key %s: target selected %q, only None is supported", k.name, answer)
	case ruleOr, ruleAnd:
		return resolveBoolKey(k, answer)
	default:
		return resolveNumKey(k, answer)
	}
}

func resolveBoolKey(k operationalKey, answer string) (string, error) {
	ours, err := parseBoolKey(k.name, k.offer)
	if err != nil {
		return "", fmt.Errorf("our offer: %w", err)
	}
	theirs, err := parseBoolKey(k.name, answer)
	if err != nil {
		return "", err
	}
	res := ours && theirs
	if k.rule == ruleOr {
		res = ours || theirs
	}
	if res {
		return valueYes, nil
	}
	return valueNo, nil
}

func resolveNumKey(k operationalKey, answer string) (string, error) {
	ours, err := parseNumKey(k.name, k.offer)
	if err != nil {
		return "", fmt.Errorf("our offer: %w", err)
	}
	theirs, err := parseNumKey(k.name, answer)
	if err != nil {
		return "", err
	}
	res := min(ours, theirs)
	if k.rule == ruleMax {
		res = max(ours, theirs)
	}
	return strconv.FormatUint(uint64(res), 10), nil
}

// boolField returns the boolean parameter negotiated by key, or nil.
func boolField(n *negotiated, key string) *bool {
	switch key {
	case "InitialR2T":
		return &n.InitialR2T
	case "ImmediateData":
		return &n.ImmediateData
	case "DataPDUInOrder":
		return &n.DataPDUInOrder
	case "DataSequenceInOrder":
		return &n.DataSequenceInOrder
	case "IFMarker":
		return &n.IFMarker
	case "OFMarker":
		return &n.OFMarker
	}
	return nil
}

// numField returns the numeric parameter negotiated by key, or nil.
func numField(n *negotiated, key string) *uint32 {
	switch key {
	case "MaxBurstLength":
		return &n.MaxBurstLength
	case "FirstBurstLength":
		return &n.FirstBurstLength
	case "DefaultTime2Wait":
		return &n.DefaultTime2Wait
	case "DefaultTime2Retain":
		return &n.DefaultTime2Retain
	case "MaxOutstandingR2T":
		return &n.MaxOutstandingR2T
	case "ErrorRecoveryLevel":
		return &n.ErrorRecoveryLevel
	}
	return nil
}

// applyNegotiated stores one resolved key into the result struct.
func applyNegotiated(n *negotiated, key, v string) error {
	switch key {
	case "HeaderDigest":
		n.HeaderDigest = v != valueNone
		return nil
	case "DataDigest":
		n.DataDigest = v != valueNone
		return nil
	case keyMaxRecvDataSegmentLength:
		x, err := parseNumKey(key, v)
		if err != nil {
			return err
		}
		n.MaxXmitDLength = x
		if x < 512 || x > 16777215 {
			return fmt.Errorf("login key %s: target declared %d, outside 512-16777215", key, x)
		}
		return nil
	}
	if dst := boolField(n, key); dst != nil {
		b, err := parseBoolKey(key, v)
		if err != nil {
			return err
		}
		*dst = b
		return nil
	}
	if dst := numField(n, key); dst != nil {
		x, err := parseNumKey(key, v)
		if err != nil {
			return err
		}
		*dst = x
	}
	return nil
}

// parseTargetAddress parses a TargetAddress value "host[:port][,tpgt]".
func parseTargetAddress(v string) (Portal, error) {
	addr, _, _ := strings.Cut(v, ",")
	if addr == "" {
		return Portal{}, fmt.Errorf("parse TargetAddress %q: empty address", v)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// No port: bare hostname / IPv4, or a bracketed IPv6 literal.
		host = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
		//nolint:nilerr // intentional: a missing port selects the default iSCSI port.
		return Portal{Address: host, Port: DefaultPort}.normalized(), nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return Portal{}, fmt.Errorf("parse TargetAddress %q: invalid port %q", v, portStr)
	}
	return Portal{Address: host, Port: port}.normalized(), nil
}

// loginState is the progress of one login phase.
type loginState struct {
	in       loginInput
	portal   Portal
	offers   []operationalKey
	offerIdx map[string]int
	res      *loginResult

	csg                uint8
	pending            []textKV
	offeredOperational bool
	// answered holds operational keys the target proposed before we made
	// our own offer; they are answered in place and not offered again.
	answered     map[string]bool
	authAccepted bool
	// chap is the CHAP exchange; nil when the login offers AuthMethod=None.
	chap *chapState
	// holdTransit keeps the next request in the current stage (T=0).
	holdTransit bool
	expStatSN   uint32
}

func newLoginState(in loginInput, portal Portal) *loginState {
	offers := operationalOffers()
	offerIdx := make(map[string]int, len(offers))
	for i, k := range offers {
		offerIdx[k.name] = i
	}
	authMethod := valueNone
	var chap *chapState
	if in.CHAP != nil {
		authMethod = authMethodCHAP // never None too: that would allow a downgrade
		chap = &chapState{creds: in.CHAP, step: chapAwaitMethod}
	}
	return &loginState{
		in:       in,
		portal:   portal,
		offers:   offers,
		offerIdx: offerIdx,
		res:      &loginResult{TPGT: -1, Params: rfcDefaults()},
		csg:      stageSecurity,
		pending: []textKV{
			{"InitiatorName", in.InitiatorIQN},
			{"TargetName", in.TargetIQN},
			{"SessionType", "Normal"},
			{keyAuthMethod, authMethod},
		},
		answered: map[string]bool{},
		chap:     chap,
	}
}

// errorf prefixes an error with the login target and portal.
func (st *loginState) errorf(format string, args ...any) error {
	return fmt.Errorf("login to target %s at %s: "+format,
		append([]any{st.in.TargetIQN, st.portal}, args...)...)
}

// loginSession runs the login phase to completion (full feature phase).
func loginSession(ctx context.Context, ex loginExchanger, in loginInput, portal Portal) (*loginResult, error) {
	st := newLoginState(in, portal)
	if st.chap != nil {
		defer st.chap.close()
	}
	for range maxLoginRounds {
		req := st.nextRequest()
		resp, text, err := exchangeComplete(ctx, ex, req, &st.expStatSN)
		if err != nil {
			return nil, err
		}
		err = st.checkResponse(resp, text)
		if err != nil {
			return nil, err
		}
		err = st.applyText(text)
		if err != nil {
			return nil, err
		}
		err = st.advanceCHAP()
		if err != nil {
			return nil, err
		}
		if !resp.Transit {
			continue // target wants another exchange in this stage
		}
		done, err := st.transition(resp)
		if err != nil {
			return nil, err
		}
		if done {
			return st.res, nil
		}
	}
	return nil, st.errorf("no full feature phase after %d exchanges", maxLoginRounds)
}

// nextRequest builds the next Login Request from the pending keys.
func (st *loginState) nextRequest() *loginRequest {
	transit := !st.holdTransit
	st.holdTransit = false
	nsg := uint8(stageOperational)
	if st.csg == stageOperational {
		nsg = stageFullFeature
		st.offerOperational()
	}
	if !transit {
		nsg = 0 // reserved when T=0
	}
	req := &loginRequest{
		Transit:   transit,
		CSG:       st.csg,
		NSG:       nsg,
		ISID:      st.in.ISID,
		TSIH:      st.in.TSIH,
		CID:       st.in.CID,
		ExpStatSN: st.expStatSN,
		Data:      encodeText(st.pending),
	}
	st.pending = nil
	return req
}

// offerOperational queues our operational offers once, skipping keys the
// target already proposed.
func (st *loginState) offerOperational() {
	if st.offeredOperational {
		return
	}
	for _, k := range st.offers {
		if !st.answered[k.name] {
			st.pending = append(st.pending, textKV{k.name, k.offer})
		}
	}
	st.pending = append(st.pending,
		textKV{keyMaxRecvDataSegmentLength, strconv.Itoa(offerMaxRecvDataSegmentLength)})
	st.offeredOperational = true
}

// checkResponse rejects non-success statuses and malformed responses.
func (st *loginState) checkResponse(resp *loginResponse, text []textKV) error {
	if resp.StatusClass != 0 {
		return st.rejection(resp, text)
	}
	if resp.VersionActive != 0 {
		return st.errorf("target selected unsupported iSCSI version %d", resp.VersionActive)
	}
	if resp.CSG != st.csg {
		return st.errorf("response stage %d does not match request stage %d", resp.CSG, st.csg)
	}
	return nil
}

// rejection builds the LoginError for a non-success status, including the
// redirect portal of a class 1 response.
func (st *loginState) rejection(resp *loginResponse, text []textKV) error {
	le := &LoginError{
		Target:       st.in.TargetIQN,
		Portal:       st.portal,
		StatusClass:  resp.StatusClass,
		StatusDetail: resp.StatusDetail,
	}
	if resp.StatusClass != 1 {
		return le
	}
	for _, kv := range text {
		if kv.Key != "TargetAddress" {
			continue
		}
		p, err := parseTargetAddress(kv.Value)
		if err != nil {
			return st.errorf("redirect: %w", err)
		}
		le.Redirect = &p
	}
	if le.Redirect == nil {
		return fmt.Errorf("%w: redirect without TargetAddress", le)
	}
	return le
}

// applyText processes the keys of one successful response.
func (st *loginState) applyText(text []textKV) error {
	for _, kv := range text {
		err := st.applyKey(kv)
		if err != nil {
			return err
		}
	}
	return nil
}

func (st *loginState) applyKey(kv textKV) error {
	switch kv.Key {
	case keyAuthMethod, keyCHAPA, keyCHAPI, keyCHAPC, keyCHAPN, keyCHAPR:
		return st.applyAuthKey(kv)
	case "TargetPortalGroupTag":
		tpgt, err := strconv.ParseUint(kv.Value, 0, 16)
		if err != nil {
			return st.errorf("invalid TargetPortalGroupTag %q: %w", kv.Value, err)
		}
		st.res.TPGT = int(tpgt)
	case "TargetAlias":
		st.res.TargetAlias = kv.Value
	case "TargetAddress":
		// Declarative; only meaningful on redirects.
	case keyMaxRecvDataSegmentLength:
		err := applyNegotiated(&st.res.Params, kv.Key, kv.Value)
		if err != nil {
			return st.errorf("%w", err)
		}
	default:
		return st.applyOperational(kv)
	}
	return nil
}

// applyAuthKey handles AuthMethod and the CHAP_* keys.  Without CHAP
// credentials only AuthMethod=None is acceptable; with them the keys are
// collected for advanceCHAP.
func (st *loginState) applyAuthKey(kv textKV) error {
	if st.chap != nil {
		err := st.chap.store(kv.Key, kv.Value)
		if err != nil {
			return st.errorf("%w", err)
		}
		return nil
	}
	if kv.Key != keyAuthMethod {
		return st.errorf("target sent %s although AuthMethod=None was offered", kv.Key)
	}
	if kv.Value != valueNone {
		return st.errorf("target requires AuthMethod=%s but no CHAP credentials are configured for this volume "+
			"(set auth on the PillarProtocol, or disable authentication on the target)", kv.Value)
	}
	st.authAccepted = true
	return nil
}

// advanceCHAP runs the CHAP step for the authentication keys of the
// response just applied and queues our answer.
func (st *loginState) advanceCHAP() error {
	if st.chap == nil {
		return nil
	}
	if st.csg != stageSecurity {
		err := st.chap.unexpected()
		st.chap.recv = nil
		if err != nil {
			return st.errorf("%w", err)
		}
		return nil
	}
	send, hold, err := st.chap.advance()
	if err != nil {
		return st.errorf("%w", err)
	}
	st.pending = append(st.pending, send...)
	st.holdTransit = hold
	return nil
}

// applyOperational negotiates one operational key, answering keys the
// target proposed before our offer and declining keys we do not implement.
func (st *loginState) applyOperational(kv textKV) error {
	i, known := st.offerIdx[kv.Key]
	if !known {
		// Target-initiated key we do not implement.
		if !isNonValue(kv.Value) {
			st.pending = append(st.pending, textKV{kv.Key, "NotUnderstood"})
		}
		return nil
	}
	if isNonValue(kv.Value) {
		return nil // RFC default stays in effect.
	}
	v, err := resolveKey(st.offers[i], kv.Value)
	if err != nil {
		return st.errorf("%w", err)
	}
	if !st.offeredOperational {
		// Target proposed the key before we offered it: answer.
		st.pending = append(st.pending, textKV{kv.Key, v})
		st.answered[kv.Key] = true
	}
	err = applyNegotiated(&st.res.Params, kv.Key, v)
	if err != nil {
		return st.errorf("%w", err)
	}
	return nil
}

// transition follows the target's stage change after a transit response.
// The done result is true once the full feature phase is reached.
func (st *loginState) transition(resp *loginResponse) (done bool, err error) {
	switch resp.NSG {
	case stageFullFeature:
		err = st.finish(resp)
		if err != nil {
			return false, err
		}
		return true, nil
	case stageOperational:
		if st.csg == stageSecurity && !st.securityDone() {
			return false, st.errorf("target left the security stage before authentication completed")
		}
		st.csg = stageOperational
		return false, nil
	default:
		return false, st.errorf("target requested invalid next stage %d", resp.NSG)
	}
}

// securityDone reports whether the security stage may end: AuthMethod=None
// was accepted, or CHAP (including the mutual step) completed.
func (st *loginState) securityDone() bool {
	if st.chap != nil {
		return st.chap.step == chapDone
	}
	return st.authAccepted
}

// finish validates the final login response and records the session.
func (st *loginState) finish(resp *loginResponse) error {
	if !st.offeredOperational || st.csg != stageOperational {
		return st.errorf("target skipped operational negotiation")
	}
	if len(st.pending) != 0 {
		return st.errorf("target entered full feature phase with unanswered keys %v", st.pending)
	}
	if resp.TSIH == 0 {
		return st.errorf("final login response carries TSIH 0")
	}
	st.res.TSIH = resp.TSIH
	st.res.StatSN = resp.StatSN
	finalizeNegotiated(&st.res.Params)
	return nil
}

// exchangeComplete performs one login exchange, following C (continue)
// bits until the target's text is complete.  The counter *expStatSN is
// advanced from each response.
func exchangeComplete(
	ctx context.Context, ex loginExchanger, req *loginRequest, expStatSN *uint32,
) (*loginResponse, []textKV, error) {
	var buf []byte
	for range maxLoginRounds {
		resp, err := ex(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		// A target rejecting the login before it parsed the request (e.g.
		// the TPG is disabled) answers with a zeroed ISID, so the ISID is
		// only checked on successful responses.
		if resp.StatusClass == 0 && req.ISID != resp.ISID {
			return nil, nil, fmt.Errorf("login response ISID %s does not match request ISID %s", resp.ISID, req.ISID)
		}
		*expStatSN = resp.StatSN + 1
		buf = append(buf, resp.Data...)
		if resp.StatusClass != 0 || !resp.Continue {
			text, derr := decodeText(buf)
			if derr != nil {
				return nil, nil, derr
			}
			return resp, text, nil
		}
		// Ask for the rest of the text: empty request, same stage, T=0.
		req = &loginRequest{
			CSG: req.CSG, ISID: req.ISID, TSIH: req.TSIH, CID: req.CID, ExpStatSN: *expStatSN,
		}
	}
	return nil, nil, errors.New("login response text continued beyond the exchange limit")
}

// finalizeNegotiated enforces cross-key constraints the kernel checks at
// START_CONN (libiscsi iscsi_conn_start).
func finalizeNegotiated(n *negotiated) {
	if n.FirstBurstLength > n.MaxBurstLength {
		n.FirstBurstLength = n.MaxBurstLength
	}
	if n.MaxOutstandingR2T == 0 {
		n.MaxOutstandingR2T = 1
	}
}
