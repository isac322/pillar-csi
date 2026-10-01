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
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	chapUser         = "node-a"
	chapSecret       = "initiator-secret-1"
	chapMutualUser   = "target-x"
	chapMutualSecret = "target-secret-22"
)

func oneWayCreds() *CHAPCredentials {
	return &CHAPCredentials{Username: chapUser, Secret: chapSecret}
}

func mutualCreds() *CHAPCredentials {
	return &CHAPCredentials{
		Username: chapUser, Secret: chapSecret, MutualUsername: chapMutualUser, MutualSecret: chapMutualSecret,
	}
}

func chapTarget(mutual bool) *fakeTarget {
	tgt := newFakeTarget()
	tgt.chap = &fakeCHAP{user: chapUser, secret: chapSecret}
	if mutual {
		tgt.chap.mutualUser, tgt.chap.mutualSecret = chapMutualUser, chapMutualSecret
	}
	return tgt
}

func chapInput(c *CHAPCredentials) loginInput {
	in := testLoginInput()
	in.CHAP = c
	return in
}

// operationalRequests counts requests the target saw past the security
// stage: a failed authentication must never reach them.
func operationalRequests(tgt *fakeTarget) int {
	n := 0
	for _, r := range tgt.logins {
		if r.CSG != stageSecurity {
			n++
		}
	}
	return n
}

func TestCHAPLoginOneWay(t *testing.T) {
	tgt := chapTarget(false)
	res, err := loginSession(context.Background(), tgt.exchanger(), chapInput(oneWayCreds()), testPortal)
	if err != nil {
		t.Fatal(err)
	}
	if res.TSIH != 0x1234 || res.TPGT != 1 || res.Params.MaxXmitDLength != 65536 {
		t.Errorf("result = %+v", res)
	}
	if len(tgt.logins) != 4 {
		t.Fatalf("exchanges = %d, want 3 security + 1 operational", len(tgt.logins))
	}
	checkOneWayCHAPRequests(t, tgt.logins[0], tgt.logins[1], tgt.logins[2])
}

// checkOneWayCHAPRequests checks the three security requests of a one-way
// CHAP login: the CHAP-only offer, CHAP_A within the stage, the response.
func checkOneWayCHAPRequests(t *testing.T, offer, alg, resp loginRequestView) {
	t.Helper()
	if v := textValue(offer.Text, "AuthMethod"); v != "CHAP" || !offer.Transit {
		t.Errorf("first request AuthMethod=%q T=%v, want CHAP only with T=1", v, offer.Transit)
	}
	if v := textValue(alg.Text, "CHAP_A"); v != "5" || alg.Transit || alg.NSG != 0 {
		t.Errorf("algorithm request CHAP_A=%q T=%v NSG=%d, want 5 with T=0 NSG=0", v, alg.Transit, alg.NSG)
	}
	if textValue(resp.Text, "CHAP_N") != chapUser || !resp.Transit || resp.NSG != stageOperational {
		t.Errorf("response request = %+v", resp)
	}
	if textValue(resp.Text, "CHAP_I") != "" || textValue(resp.Text, "CHAP_C") != "" {
		t.Errorf("one-way CHAP must not challenge the target: %+v", resp.Text)
	}
}

func TestCHAPLoginMutual(t *testing.T) {
	tgt := chapTarget(true)
	_, err := loginSession(context.Background(), tgt.exchanger(), chapInput(mutualCreds()), testPortal)
	if err != nil {
		t.Fatal(err)
	}
	if len(tgt.initiatorChallenges) != 1 || len(tgt.initiatorChallenges[0]) < 16 {
		t.Fatalf("initiator challenges = %x, want one of at least 16 bytes", tgt.initiatorChallenges)
	}
	// A fresh random challenge per login.
	_, err = loginSession(context.Background(), tgt.exchanger(), chapInput(mutualCreds()), testPortal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(tgt.initiatorChallenges[0], tgt.initiatorChallenges[1]) {
		t.Error("mutual challenge reused across logins")
	}
}

func TestCHAPLoginAcceptsBase64Challenge(t *testing.T) {
	tgt := chapTarget(true)
	tgt.chap.base64Challenge = true
	tgt.chap.challenge = []byte("an odd-length challenge!") // base64 with padding
	_, err := loginSession(context.Background(), tgt.exchanger(), chapInput(mutualCreds()), testPortal)
	if err != nil {
		t.Fatalf("login with base64 CHAP_C: %v", err)
	}
}

func TestCHAPLoginRejections(t *testing.T) {
	for name, tc := range map[string]struct {
		target func(*fakeTarget)
		creds  *CHAPCredentials
		want   string
		// sentinel is the authentication error the failure matches; nil
		// for protocol violations, which match neither.
		sentinel error
	}{
		"wrong initiator secret": {
			target: func(tgt *fakeTarget) { tgt.chap.secret = "another-secret-1" },
			creds:  oneWayCreds(), want: "authentication failed", sentinel: ErrAuthenticationFailed,
		},
		"wrong mutual response": {
			target: func(tgt *fakeTarget) { tgt.chap.wrongMutual = true },
			creds:  mutualCreds(), want: "wrong CHAP_R", sentinel: ErrTargetAuthenticationFailed,
		},
		"missing mutual response": {
			target: func(tgt *fakeTarget) { tgt.chap.skipMutual = true },
			creds:  mutualCreds(), want: "did not answer the mutual CHAP challenge",
			sentinel: ErrTargetAuthenticationFailed,
		},
		"wrong mutual name": {
			target: func(tgt *fakeTarget) { tgt.chap.mutualUser = "impostor" },
			creds:  mutualCreds(), want: "CHAP_N does not match", sentinel: ErrTargetAuthenticationFailed,
		},
		"downgrade to None": {
			target: func(tgt *fakeTarget) { tgt.chap.selectNone = true },
			creds:  oneWayCreds(), want: "AuthMethod=None although CHAP",
		},
		"unsupported algorithm": {
			target: func(tgt *fakeTarget) { tgt.chap.algorithm = "7" },
			creds:  oneWayCreds(), want: "only MD5",
		},
		"malformed challenge": {
			target: func(tgt *fakeTarget) { tgt.chap.challenge = []byte{} },
			creds:  oneWayCreds(), want: "CHAP_C",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tgt := chapTarget(tc.creds.Mutual())
			tc.target(tgt)
			_, err := loginSession(context.Background(), tgt.exchanger(), chapInput(tc.creds), testPortal)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			for _, secret := range []string{chapSecret, chapMutualSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks a secret: %v", err)
				}
			}
			if n := operationalRequests(tgt); n != 0 {
				t.Errorf("%d operational requests after failed authentication", n)
			}
			checkAuthSentinel(t, err, tc.sentinel)
		})
	}
}

// checkAuthSentinel checks that err matches exactly the expected
// authentication sentinel (none when want is nil).
func checkAuthSentinel(t *testing.T, err, want error) {
	t.Helper()
	for _, s := range []error{ErrAuthenticationFailed, ErrTargetAuthenticationFailed} {
		wantIs := want != nil && errors.Is(want, s)
		if got := errors.Is(err, s); got != wantIs {
			t.Errorf("errors.Is(err, %q) = %v, want %v (err = %v)", s, got, wantIs, err)
		}
	}
}

func TestCHAPWrongSecretIsAuthenticationFailure(t *testing.T) {
	tgt := chapTarget(false)
	tgt.chap.secret = "another-secret-1"
	_, err := loginSession(context.Background(), tgt.exchanger(), chapInput(oneWayCreds()), testPortal)
	var le *LoginError
	if !errors.Is(err, ErrAuthenticationFailed) || !errors.As(err, &le) || le.Retryable() {
		t.Fatalf("err = %v, want non-retryable LoginError matching ErrAuthenticationFailed", err)
	}
	if errors.Is(&LoginError{StatusClass: 2, StatusDetail: 2}, ErrAuthenticationFailed) {
		t.Error("authorization failure (ACL) must not match ErrAuthenticationFailed")
	}
}

// A target that presents a challenge this initiator issued in another,
// still-running login tries to have the initiator answer it (reflection).
func TestCHAPRejectsReflectedChallenge(t *testing.T) {
	victim := chapTarget(true)
	var reflectedErr error
	victim.chap.beforeMutualAnswer = func(ours []byte) {
		attacker := chapTarget(false)
		attacker.chap.challenge = ours
		_, reflectedErr = loginSession(context.Background(), attacker.exchanger(), chapInput(mutualCreds()), testPortal)
	}
	_, err := loginSession(context.Background(), victim.exchanger(), chapInput(mutualCreds()), testPortal)
	if err != nil {
		t.Fatalf("original login: %v", err)
	}
	if reflectedErr == nil || !strings.Contains(reflectedErr.Error(), "reflected") {
		t.Fatalf("reflected challenge login err = %v, want reflection rejection", reflectedErr)
	}
	// Once the original login finished, its challenge is no longer
	// outstanding and an unrelated target may use the same bytes.
	again := chapTarget(false)
	again.chap.challenge = victim.initiatorChallenges[0]
	_, err = loginSession(context.Background(), again.exchanger(), chapInput(oneWayCreds()), testPortal)
	if err != nil {
		t.Errorf("login after the original completed: %v", err)
	}
}

func TestCHAPCredentialsNeverFormatted(t *testing.T) {
	p := SessionParams{TargetIQN: testTarget, CHAP: mutualCreds()}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, p)
		if strings.Contains(out, chapSecret) || strings.Contains(out, chapMutualSecret) || strings.Contains(out, chapUser) {
			t.Errorf("%s of SessionParams leaks credentials: %s", verb, out)
		}
	}
}

func TestSessionParamsRejectInvalidCHAP(t *testing.T) {
	for name, c := range map[string]CHAPCredentials{
		"no secret":            {Username: chapUser},
		"mutual without name":  {Username: chapUser, Secret: chapSecret, MutualSecret: chapMutualSecret},
		"same secret mutually": {Username: chapUser, Secret: chapSecret, MutualUsername: "t", MutualSecret: chapSecret},
	} {
		p := SessionParams{InitiatorIQN: testInitiator, TargetIQN: testTarget, Portal: Portal{Address: "10.0.0.1"}, CHAP: &c}
		if err := p.withDefaults().validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// chapLogins counts security requests that answered a CHAP challenge with
// the configured username.
func chapResponses(tgt *fakeTarget) int {
	tgt.mu.Lock()
	defer tgt.mu.Unlock()
	n := 0
	for _, r := range tgt.logins {
		if textValue(r.Text, "CHAP_N") == chapUser {
			n++
		}
	}
	return n
}

func TestRecoveryReloginReusesCHAP(t *testing.T) {
	h := newHarness(t)
	h.tgt.chap = &fakeCHAP{user: chapUser, secret: chapSecret, mutualUser: chapMutualUser, mutualSecret: chapMutualSecret}
	h.start()
	p := h.params()
	p.CHAP = mutualCreds()
	if _, err := h.ini.Login(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.CHAP.Secret = "caller-mutated" // the session keeps its own copy
	before := len(h.kern.callLog())
	h.kern.connError(1)
	eventually(t, "session recovered with CHAP", func() bool {
		return slices.Contains(h.kern.callLog()[before:], "START_CONN 1:0") && h.sessionPhase() == phaseEstablished
	})
	if n := chapResponses(h.tgt); n != 2 {
		t.Errorf("CHAP responses = %d, want initial login + re-login", n)
	}
}

// A session adopted after a restart has no credentials; the staged ones
// must be restorable by a repeated Login or by SetLoginParams so the next
// recovery re-login authenticates.
func TestAdoptedSessionRecoversWithRestoredCHAP(t *testing.T) {
	for name, restore := range map[string]func(h *harness) error{
		"Login": func(h *harness) error {
			p := h.params()
			p.CHAP = mutualCreds()
			_, err := h.ini.Login(context.Background(), p)
			return err
		},
		"SetLoginParams": func(h *harness) error {
			return h.ini.SetLoginParams(testTarget, h.params().Portal, 0, mutualCreds())
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.tgt.chap = &fakeCHAP{
				user: chapUser, secret: chapSecret, mutualUser: chapMutualUser, mutualSecret: chapMutualSecret,
			}
			h.kern.adoptExisting(3, 13, testTarget, testInitiator, "LOGGED_IN", "up", h.params().Portal)
			h.start()
			if err := restore(h); err != nil {
				t.Fatalf("restore CHAP: %v", err)
			}
			h.kern.connError(3)
			eventually(t, "adopted session recovered", func() bool {
				return slices.Contains(h.kern.callLog(), "START_CONN 3:0") && h.sessionPhase() == phaseEstablished
			})
			if n := chapResponses(h.tgt); n != 1 {
				t.Errorf("CHAP responses = %d, want the one recovery re-login", n)
			}
		})
	}
}
