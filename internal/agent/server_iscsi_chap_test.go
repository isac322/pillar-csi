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

package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
)

func iscsiCHAPParams(chap *agentv1.IscsiChap) *agentv1.ExportParams {
	params := iscsiExportParams("10.0.0.1", 3260)
	params.GetIscsi().Chap = chap
	return params
}

func (e iscsiEnv) readACLAuth(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(e.tpg(), "acls", testInitiatorIQN, "auth", name)
	data, err := os.ReadFile(path) //nolint:gosec // test tree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSuffix(string(data), "\n")
}

func (e iscsiEnv) tpgAuthentication(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.tpg(), "attrib", "authentication"))
	if err != nil {
		t.Fatalf("read attrib/authentication: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func (e iscsiEnv) assertACLAuth(t *testing.T, want map[string]string) {
	t.Helper()
	for name, value := range want {
		if got := e.readACLAuth(t, name); got != value {
			t.Errorf("ACL auth/%s = %q, want %q", name, got, value)
		}
	}
}

func exportISCSICHAP(t *testing.T, srv *agent.Server, chap *agentv1.IscsiChap, acl bool) error {
	t.Helper()
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, DevicePath: testDevicePath, Fence: testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		ExportParams: iscsiCHAPParams(chap), AclEnabled: acl,
	})
	return err
}

func (e iscsiEnv) allowCHAP(t *testing.T, chap *agentv1.IscsiChap) error {
	t.Helper()
	_, err := e.srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId: testVolumeID, Fence: testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, InitiatorId: testInitiatorIQN,
		ExportParams: iscsiCHAPParams(chap),
	})
	return err
}

// ExportVolume and AllowInitiator with mutual CHAP turn on the TPG's
// authentication and write both credential pairs to the new ACL; a later
// grant with rotated one-way credentials rewrites the existing ACL and
// clears the mutual pair.
func TestISCSI_CHAPExportAndAllow(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	mutual := &agentv1.IscsiChap{
		Username: "node-user", Password: "initiator-secret-1",
		MutualUsername: "target-user", MutualPassword: "target-secret-12",
	}
	if err := exportISCSICHAP(t, env.srv, mutual, true); err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	if got := env.tpgAuthentication(t); got != "1" {
		t.Fatalf("attrib/authentication = %q, want 1", got)
	}
	if err := env.allowCHAP(t, mutual); err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}
	env.assertACLAuth(t, map[string]string{
		"userid": "node-user", "password": "initiator-secret-1",
		"userid_mutual": "target-user", "password_mutual": "target-secret-12",
	})

	rotated := &agentv1.IscsiChap{Username: "node-user", Password: "rotated-secret-22"}
	if err := env.allowCHAP(t, rotated); err != nil {
		t.Fatalf("AllowInitiator (rotated): %v", err)
	}
	env.assertACLAuth(t, map[string]string{
		"userid": "node-user", "password": "rotated-secret-22",
		"userid_mutual": "NULL", "password_mutual": "NULL",
	})
}

// A restore/reconcile carrying credentials re-applies them to the kept ACLs.
func TestReconcileState_ISCSICHAPRotates(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	desired := func(password string) *agentv1.VolumeDesiredState {
		vol := iscsiDesired(testInitiatorIQN)
		vol.GetExports()[0].ExportParams = iscsiCHAPParams(
			&agentv1.IscsiChap{Username: "node-user", Password: password})
		return vol
	}
	reconcileISCSI(t, env.srv, false, desired("initiator-secret-1"))
	if got := env.tpgAuthentication(t); got != "1" {
		t.Fatalf("attrib/authentication = %q, want 1", got)
	}
	env.assertACLAuth(t, map[string]string{"userid": "node-user", "password": "initiator-secret-1"})

	reconcileISCSI(t, env.srv, false, desired("rotated-secret-22"))
	env.assertACLAuth(t, map[string]string{"userid": "node-user", "password": "rotated-secret-22"})
}

func TestISCSI_NoCHAPKeepsAuthenticationOff(t *testing.T) {
	t.Parallel()
	env := newISCSIServer(t)
	exportISCSI(t, env.srv, true)
	if got := env.tpgAuthentication(t); got != "0" {
		t.Fatalf("attrib/authentication = %q, want 0", got)
	}
}

// Unusable credentials, or CHAP on an export without node ACLs, are
// InvalidArgument whose message never carries a secret, and nothing is
// exported.
func TestISCSI_CHAPInvalid(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		chap *agentv1.IscsiChap
		acl  bool
	}{
		"missing password": {&agentv1.IscsiChap{Username: "node-user"}, true},
		"half mutual": {&agentv1.IscsiChap{
			Username: "node-user", Password: "initiator-secret-1", MutualPassword: "target-secret-12",
		}, true},
		"without ACL": {&agentv1.IscsiChap{Username: "node-user", Password: "initiator-secret-1"}, false},
	} {
		env := newISCSIServer(t)
		err := exportISCSICHAP(t, env.srv, tc.chap, tc.acl)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: ExportVolume code = %v (%v), want InvalidArgument", name, status.Code(err), err)
		}
		if err != nil && (strings.Contains(err.Error(), "initiator-secret-1") ||
			strings.Contains(err.Error(), "target-secret-12")) {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
		if env.k.Exists(env.tpg()) {
			t.Errorf("%s: target created for a refused export", name)
		}
	}
}
