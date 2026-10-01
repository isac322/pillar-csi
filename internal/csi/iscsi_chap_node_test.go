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
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/isac322/pillar-csi/internal/iscsi"
)

func chapStageRequest(volumeID, stagingPath, method string, secrets map[string]string) *csi.NodeStageVolumeRequest {
	vc := map[string]string{
		VolumeContextKeyTargetID:     testTargetIQN,
		VolumeContextKeyAddress:      "192.168.1.10",
		VolumeContextKeyPort:         "3260",
		VolumeContextKeyProtocolType: ProtocolISCSI,
		vcVolumeRef:                  "0",
	}
	if method != "" {
		vc[VolumeContextKeyISCSIAuthMethod] = method
	}
	return &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext:     vc,
		Secrets:           secrets,
	}
}

func mutualCHAPSecrets() map[string]string {
	return map[string]string{
		ISCSIChapSecretKeyUsername:       "node-user",
		ISCSIChapSecretKeyPassword:       "initiator-secret-1",
		ISCSIChapSecretKeyMutualUsername: "target-user",
		ISCSIChapSecretKeyMutualPassword: "target-secret-22",
	}
}

func TestNodeStage_ISCSICHAPMissingSecretKeyIsInvalidArgument(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		method, missing string
	}{
		"CHAP password":          {"CHAP", ISCSIChapSecretKeyPassword},
		"CHAP username":          {"CHAP", ISCSIChapSecretKeyUsername},
		"MutualCHAP mutual pass": {"MutualCHAP", ISCSIChapSecretKeyMutualPassword},
		"MutualCHAP mutual user": {"MutualCHAP", ISCSIChapSecretKeyMutualUsername},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ini := newFakeISCSIInitiator()
			env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{ProtocolISCSI: NewISCSIHandler(ini, testInitiatorIQN)})
			secrets := mutualCHAPSecrets()
			delete(secrets, tc.missing)
			_, err := env.srv.NodeStageVolume(context.Background(),
				chapStageRequest("storage-1/iscsi/zfs-zvol/tank/pvc-c", t.TempDir(), tc.method, secrets))
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), `"`+tc.missing+`"`) {
				t.Fatalf("err = %v, want InvalidArgument naming key %q", err, tc.missing)
			}
			for _, v := range secrets {
				if strings.Contains(err.Error(), v) {
					t.Errorf("error leaks a secret value: %v", err)
				}
			}
			if len(ini.logins) != 0 {
				t.Errorf("Login called %d times before the secrets were validated", len(ini.logins))
			}
		})
	}
}

// NodeStage logs in with the node-stage secrets, persists them in a 0600
// stage state file, and a restarted pillar-node restores them to the
// adopted session.
func TestNodeStage_ISCSIMutualCHAPPersistsAndRestoresCredentials(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{ProtocolISCSI: NewISCSIHandler(ini, testInitiatorIQN)})
	const volumeID = "storage-1/iscsi/zfs-zvol/tank/pvc-m"
	_, err := env.srv.NodeStageVolume(context.Background(),
		chapStageRequest(volumeID, t.TempDir(), "MutualCHAP", mutualCHAPSecrets()))
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	want := iscsi.CHAPCredentials{
		Username: "node-user", Secret: "initiator-secret-1",
		MutualUsername: "target-user", MutualSecret: "target-secret-22",
	}
	if len(ini.logins) != 1 || ini.logins[0].CHAP == nil || *ini.logins[0].CHAP != want {
		t.Fatalf("Login CHAP = %+v, want the node-stage secrets", ini.logins)
	}

	info, err := os.Stat(env.srv.stateFilePath(volumeID))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("stage state file mode = %#o, want 0600 (it holds CHAP secrets)", mode)
	}
	st, err := env.srv.readStageState(volumeID)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := st.ToProtocolState()
	if err != nil {
		t.Fatal(err)
	}
	iscsiPS, ok := ps.(*ISCSIProtocolState)
	if !ok {
		t.Fatalf("protocol state = %T, want *ISCSIProtocolState", ps)
	}
	if got := iscsiPS.CHAP; got == nil || *got != want {
		t.Errorf("persisted CHAP round trip = %v, want the staged credentials", got)
	}

	// Restart: a new initiator that adopted the session from sysfs.
	restarted := newFakeISCSIInitiator()
	key := testTargetIQN + "@192.168.1.10:3260"
	restarted.sessions[key] = &iscsi.Session{SID: 1}
	after := &NodeServer{
		handlers: map[string]ProtocolHandler{ProtocolISCSI: NewISCSIHandler(restarted, testInitiatorIQN)},
		stateDir: env.stateDir,
	}
	err = after.RestoreProtocolSessions(func(string, ...any) {})
	if err != nil {
		t.Fatalf("RestoreProtocolSessions: %v", err)
	}
	if got := restarted.loginCHAP[key]; got == nil || *got != want {
		t.Errorf("restored CHAP = %v, want the staged credentials", got)
	}
}

// Without an auth method the node-stage secrets are ignored and nothing
// is persisted.
func TestNodeStage_ISCSINoAuthIgnoresSecrets(t *testing.T) {
	t.Parallel()
	ini := newFakeISCSIInitiator()
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{ProtocolISCSI: NewISCSIHandler(ini, testInitiatorIQN)})
	const volumeID = "storage-1/iscsi/zfs-zvol/tank/pvc-n"
	_, err := env.srv.NodeStageVolume(context.Background(),
		chapStageRequest(volumeID, t.TempDir(), "", map[string]string{"username": "u"}))
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if len(ini.logins) != 1 || ini.logins[0].CHAP != nil {
		t.Fatalf("Login CHAP = %v, want none", ini.logins[0].CHAP)
	}
	data, err := os.ReadFile(env.srv.stateFilePath(volumeID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "chap") {
		t.Errorf("stage state of an AuthMethod=None volume carries CHAP: %s", data)
	}
}

// CHAP failures in either direction are Unauthenticated; other login
// failures keep Internal.  The initiator's message is kept.
func TestNodeStage_ISCSIAuthenticationFailureCode(t *testing.T) {
	t.Parallel()
	portal := iscsi.Portal{Address: "192.168.1.10", Port: 3260}
	for name, tc := range map[string]struct {
		loginErr error
		want     codes.Code
	}{
		"target rejected credentials": {
			loginErr: &iscsi.LoginError{Target: testTargetIQN, Portal: portal, StatusClass: 2, StatusDetail: 1},
			want:     codes.Unauthenticated,
		},
		"target failed mutual CHAP": {
			loginErr: fmt.Errorf("login to target %s at %s: %w: wrong CHAP_R (mutual secret mismatch)",
				testTargetIQN, portal, iscsi.ErrTargetAuthenticationFailed),
			want: codes.Unauthenticated,
		},
		"ACL authorization failure": {
			loginErr: &iscsi.LoginError{Target: testTargetIQN, Portal: portal, StatusClass: 2, StatusDetail: 2},
			want:     codes.Internal,
		},
		"downgrade refused": {
			loginErr: errors.New("target selected AuthMethod=None although CHAP credentials are configured"),
			want:     codes.Internal,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ini := newFakeISCSIInitiator()
			ini.loginErr = tc.loginErr
			env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{ProtocolISCSI: NewISCSIHandler(ini, testInitiatorIQN)})
			_, err := env.srv.NodeStageVolume(context.Background(),
				chapStageRequest("storage-1/iscsi/zfs-zvol/tank/pvc-a", t.TempDir(), "MutualCHAP", mutualCHAPSecrets()))
			if status.Code(err) != tc.want || !strings.Contains(err.Error(), tc.loginErr.Error()) {
				t.Fatalf("err = %v, want code %v carrying %q", err, tc.want, tc.loginErr)
			}
		})
	}
}
