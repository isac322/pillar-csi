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

// CHAP authentication in the login security stage (RFC 7143 section 12.1.3,
// RFC 1994) with the MD5 algorithm (CHAP_A=5), the one every LIO kernel
// accepts:
//
//	I->T AuthMethod=CHAP
//	T->I AuthMethod=CHAP
//	I->T CHAP_A=5                                    (T=0)
//	T->I CHAP_A=5 CHAP_I=<id> CHAP_C=<challenge>
//	I->T CHAP_N=<name> CHAP_R=<md5(id|secret|chal)>   (T=1)
//	     [mutual: CHAP_I=<our id> CHAP_C=<our challenge>]
//	T->I [mutual: CHAP_N=<target name> CHAP_R=<md5(our id|mutual secret|our chal)>]
//
// Secrets never appear in errors or logs.

import (
	"crypto/md5" //nolint:gosec // G501: CHAP_A=5 is MD5 by definition (RFC 1994); it is what LIO verifies.
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	authMethodCHAP   = "CHAP"
	chapAlgorithmMD5 = "5"

	keyAuthMethod = "AuthMethod"
	keyCHAPA      = "CHAP_A"
	keyCHAPI      = "CHAP_I"
	keyCHAPC      = "CHAP_C"
	keyCHAPN      = "CHAP_N"
	keyCHAPR      = "CHAP_R"

	// The challenge the initiator sends for mutual CHAP is
	// chapChallengeLength bytes (LIO also uses 16 bytes).
	chapChallengeLength = 16
	// A target challenge is at most maxCHAPChallengeLength bytes (RFC 7143
	// limits binary values to 1024 bytes in the login phase).
	maxCHAPChallengeLength = 1024
)

// ErrAuthenticationFailed is matched (errors.Is) by the LoginError of a
// login the target rejected with status class 2 detail 1: the CHAP
// credentials are wrong, or the target expects credentials the initiator
// did not present.
var ErrAuthenticationFailed = errors.New("authentication failed")

// CHAPCredentials are the CHAP secrets of one session.  Username and
// Secret authenticate the initiator to the target.  MutualUsername and
// MutualSecret, when set, make the authentication mutual: the target must
// prove it knows MutualSecret under the name MutualUsername.
type CHAPCredentials struct {
	Username       string
	Secret         string
	MutualUsername string
	MutualSecret   string
}

// String redacts the secrets, so a credential formatted by mistake
// (including as a field of SessionParams) leaks nothing.
func (c CHAPCredentials) String() string {
	return "CHAP{secrets redacted, mutual=" + strconv.FormatBool(c.Mutual()) + "}"
}

// GoString redacts the secrets for %#v.
func (c CHAPCredentials) GoString() string {
	return c.String()
}

// Mutual reports whether the target must authenticate itself too.
func (c CHAPCredentials) Mutual() bool {
	return c.MutualUsername != "" || c.MutualSecret != ""
}

func (c CHAPCredentials) validate() error {
	switch {
	case c.Username == "":
		return errors.New("CHAP username is empty")
	case c.Secret == "":
		return errors.New("CHAP secret is empty")
	case c.Mutual() && c.MutualUsername == "":
		return errors.New("mutual CHAP username is empty")
	case c.Mutual() && c.MutualSecret == "":
		return errors.New("mutual CHAP secret is empty")
	case c.Mutual() && c.MutualSecret == c.Secret:
		return errors.New("mutual CHAP secret equals the CHAP secret (forbidden by RFC 7143 section 12.1.3)")
	}
	return nil
}

// cloneCHAP returns a copy of c, so a session never aliases the caller's
// credentials.
func cloneCHAP(c *CHAPCredentials) *CHAPCredentials {
	if c == nil {
		return nil
	}
	v := *c
	return &v
}

// chapStep is the progress of the CHAP exchange of one login.
type chapStep int

const (
	chapAwaitMethod    chapStep = iota // AuthMethod=CHAP offered
	chapAwaitChallenge                 // CHAP_A sent
	chapAwaitResult                    // one-way response sent
	chapAwaitMutual                    // response and our challenge sent
	chapDone                           // authenticated
)

// chapResponse is md5(id || secret || challenge) (RFC 1994 section 4.1).
func chapResponse(id byte, secret string, challenge []byte) []byte {
	h := md5.New() //nolint:gosec // G401: CHAP_A=5 mandates MD5.
	h.Write([]byte{id})
	h.Write([]byte(secret))
	h.Write(challenge)
	return h.Sum(nil)
}

// decodeCHAPBinary decodes a binary value in the 0x (hex) or 0b (base64)
// encoding of RFC 7143 section 6.1.  The value is not echoed in errors.
func decodeCHAPBinary(key, v string) ([]byte, error) {
	if len(v) < 3 || v[0] != '0' {
		return nil, fmt.Errorf("%s is not a 0x (hex) or 0b (base64) encoded binary value", key)
	}
	body := v[2:]
	var (
		b   []byte
		err error
	)
	switch v[1] {
	case 'x', 'X':
		if len(body)%2 == 1 {
			body = "0" + body // RFC 7143: an odd digit count has an implied leading zero
		}
		b, err = hex.DecodeString(body)
	case 'b', 'B':
		b, err = base64.StdEncoding.DecodeString(body)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(body)
		}
	default:
		return nil, fmt.Errorf("%s is not a 0x (hex) or 0b (base64) encoded binary value", key)
	}
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", key, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%s is empty", key)
	}
	return b, nil
}

func encodeCHAPBinary(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

// parseCHAPID parses CHAP_I: a number 0-255 in decimal or 0x hex.
func parseCHAPID(v string) (byte, error) {
	base, digits := 10, v
	if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
		base, digits = 16, v[2:]
	}
	n, err := strconv.ParseUint(digits, base, 8)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", keyCHAPI, v, err)
	}
	return byte(n), nil
}

// outstandingChallenges holds the mutual-CHAP challenges this process sent
// in logins that are still running.  A target that presents one of them as
// its own challenge tries to make this initiator compute the answer it owes
// (reflection attack); such a login is refused.
var outstandingChallenges = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

// registerChallenge records c as outstanding until the returned release
// function runs.
func registerChallenge(c []byte) (release func()) {
	k := string(c)
	outstandingChallenges.Lock()
	outstandingChallenges.m[k]++
	outstandingChallenges.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			outstandingChallenges.Lock()
			defer outstandingChallenges.Unlock()
			outstandingChallenges.m[k]--
			if outstandingChallenges.m[k] <= 0 {
				delete(outstandingChallenges.m, k)
			}
		})
	}
}

func isOutstandingChallenge(c []byte) bool {
	outstandingChallenges.Lock()
	defer outstandingChallenges.Unlock()
	return outstandingChallenges.m[string(c)] > 0
}

// newMutualChallenge draws our identifier and challenge from crypto/rand,
// never equal to the target's challenge.
func newMutualChallenge(targetChallenge []byte) (id byte, challenge []byte, err error) {
	buf := make([]byte, 1+chapChallengeLength)
	for {
		_, err = rand.Read(buf)
		if err != nil {
			return 0, nil, fmt.Errorf("generate mutual CHAP challenge: %w", err)
		}
		if subtle.ConstantTimeCompare(buf[1:], targetChallenge) == 0 {
			return buf[0], buf[1:], nil
		}
	}
}

// chapState is the CHAP part of one login's state.
type chapState struct {
	creds *CHAPCredentials
	step  chapStep
	// recv holds AuthMethod and the CHAP_* keys of the current response.
	recv map[string]string
	// Our mutual challenge, valid from chapAwaitMutual on.
	id        byte
	challenge []byte
	release   func()
}

// store records one authentication key of the current response.
func (c *chapState) store(key, value string) error {
	if c.recv == nil {
		c.recv = map[string]string{}
	}
	if _, dup := c.recv[key]; dup {
		return fmt.Errorf("target sent %s twice in one response", key)
	}
	c.recv[key] = value
	return nil
}

// close releases our outstanding challenge, if any.
func (c *chapState) close() {
	if c.release != nil {
		c.release()
		c.release = nil
	}
}

// unexpected rejects keys of the current response not in allowed.
func (c *chapState) unexpected(allowed ...string) error {
	for k := range c.recv {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("target sent unexpected %s during CHAP authentication", k)
		}
	}
	return nil
}

// The advance method consumes the authentication keys of one target
// response and returns the keys to send next.  HoldTransit is true when the
// next request must keep the security stage (T=0).
func (c *chapState) advance() (send []textKV, holdTransit bool, err error) {
	defer func() { c.recv = nil }()
	switch c.step {
	case chapAwaitMethod:
		return c.onMethod()
	case chapAwaitChallenge:
		return c.onChallenge()
	case chapAwaitMutual:
		return nil, false, c.onMutualResponse()
	default: // chapAwaitResult, chapDone
		err = c.unexpected()
		if err != nil {
			return nil, false, err
		}
		c.step = chapDone
		return nil, false, nil
	}
}

func (c *chapState) onMethod() ([]textKV, bool, error) {
	err := c.unexpected(keyAuthMethod)
	if err != nil {
		return nil, false, err
	}
	method, ok := c.recv[keyAuthMethod]
	switch {
	case !ok:
		return nil, false, errors.New("target did not answer AuthMethod=CHAP")
	case method == valueNone:
		return nil, false, errors.New("target selected AuthMethod=None although CHAP credentials are configured; " +
			"refusing an unauthenticated session (enable CHAP on the target)")
	case method != authMethodCHAP:
		return nil, false, fmt.Errorf("target answered AuthMethod=%s to the CHAP offer", method)
	}
	c.step = chapAwaitChallenge
	return []textKV{{keyCHAPA, chapAlgorithmMD5}}, true, nil
}

func (c *chapState) onChallenge() ([]textKV, bool, error) {
	err := c.unexpected(keyCHAPA, keyCHAPI, keyCHAPC)
	if err != nil {
		return nil, false, err
	}
	for _, k := range []string{keyCHAPA, keyCHAPI, keyCHAPC} {
		if _, ok := c.recv[k]; !ok {
			return nil, false, fmt.Errorf("target CHAP challenge lacks %s", k)
		}
	}
	if a := c.recv[keyCHAPA]; a != chapAlgorithmMD5 {
		return nil, false, fmt.Errorf("target selected CHAP_A=%s; only MD5 (CHAP_A=5) is supported", a)
	}
	id, err := parseCHAPID(c.recv[keyCHAPI])
	if err != nil {
		return nil, false, err
	}
	targetChallenge, err := decodeCHAPBinary(keyCHAPC, c.recv[keyCHAPC])
	if err != nil {
		return nil, false, err
	}
	if len(targetChallenge) > maxCHAPChallengeLength {
		return nil, false, fmt.Errorf("target CHAP challenge of %d bytes exceeds %d",
			len(targetChallenge), maxCHAPChallengeLength)
	}
	if isOutstandingChallenge(targetChallenge) {
		return nil, false, errors.New("target reflected a CHAP challenge this initiator issued; refusing the login")
	}
	response := encodeCHAPBinary(chapResponse(id, c.creds.Secret, targetChallenge))
	if !c.creds.Mutual() {
		c.step = chapAwaitResult
		return []textKV{{keyCHAPN, c.creds.Username}, {keyCHAPR, response}}, false, nil
	}
	c.id, c.challenge, err = newMutualChallenge(targetChallenge)
	if err != nil {
		return nil, false, err
	}
	c.release = registerChallenge(c.challenge)
	c.step = chapAwaitMutual
	return []textKV{
		{keyCHAPN, c.creds.Username},
		{keyCHAPR, response},
		{keyCHAPI, strconv.Itoa(int(c.id))},
		{keyCHAPC, encodeCHAPBinary(c.challenge)},
	}, false, nil
}

func (c *chapState) onMutualResponse() error {
	err := c.unexpected(keyCHAPN, keyCHAPR)
	if err != nil {
		return err
	}
	name, okN := c.recv[keyCHAPN]
	resp, okR := c.recv[keyCHAPR]
	if !okN || !okR {
		return errors.New("target did not answer the mutual CHAP challenge (CHAP_N/CHAP_R missing)")
	}
	if subtle.ConstantTimeCompare([]byte(name), []byte(c.creds.MutualUsername)) != 1 {
		return errors.New("mutual CHAP authentication of the target failed: CHAP_N does not match the mutual username")
	}
	got, err := decodeCHAPBinary(keyCHAPR, resp)
	if err != nil {
		return fmt.Errorf("mutual CHAP authentication of the target failed: %w", err)
	}
	want := chapResponse(c.id, c.creds.MutualSecret, c.challenge)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("mutual CHAP authentication of the target failed: wrong CHAP_R (mutual secret mismatch)")
	}
	c.close()
	c.step = chapDone
	return nil
}
