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
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const (
	testInstallNamespace = "pillar-system"
	testChapSecretName   = "iscsi-chap"
	testChapPassword     = "initiator-secret-1"
	testChapMutualPass   = "target-secret-2222"
	testChapInitiatorIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
)

func chapData(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// TestParseISCSIChapSecret pins the Secret contract shared by controller and
// node: required keys per method, the RFC 7143 secret length bounds, the LIO
// storable-value rules and the distinct-secrets rule for MutualCHAP.
func TestParseISCSIChapSecret(t *testing.T) {
	t.Parallel()
	valid := chapData("username", "initiator", "password", testChapPassword)
	mutual := chapData("username", "initiator", "password", testChapPassword,
		"mutualUsername", "target", "mutualPassword", testChapMutualPass)

	tests := []struct {
		name    string
		method  v1alpha1.ISCSIAuthMethod
		data    map[string]string
		want    *agentv1.IscsiChap
		wantErr string
	}{
		{name: "None ignores data", method: v1alpha1.ISCSIAuthMethodNone, data: nil},
		{name: "empty method is None", method: "", data: chapData("password", "x")},
		{
			name: "CHAP", method: v1alpha1.ISCSIAuthMethodCHAP, data: valid,
			want: &agentv1.IscsiChap{Username: "initiator", Password: testChapPassword},
		},
		{
			name: "CHAP ignores mutual keys", method: v1alpha1.ISCSIAuthMethodCHAP,
			data: chapData("username", "initiator", "password", testChapPassword,
				"mutualUsername", "target", "mutualPassword", testChapPassword),
			want: &agentv1.IscsiChap{Username: "initiator", Password: testChapPassword},
		},
		{
			name: "MutualCHAP", method: v1alpha1.ISCSIAuthMethodMutualCHAP, data: mutual,
			want: &agentv1.IscsiChap{
				Username: "initiator", Password: testChapPassword,
				MutualUsername: "target", MutualPassword: testChapMutualPass,
			},
		},
		{
			name: "11-byte password", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("username", "u", "password", strings.Repeat("p", 11)),
			wantErr: `key "password" is 11 bytes, want 12 to 255`,
		},
		{
			name: "12-byte password", method: v1alpha1.ISCSIAuthMethodCHAP,
			data: chapData("username", "u", "password", strings.Repeat("p", 12)),
			want: &agentv1.IscsiChap{Username: "u", Password: strings.Repeat("p", 12)},
		},
		{
			name: "255-byte password", method: v1alpha1.ISCSIAuthMethodCHAP,
			data: chapData("username", "u", "password", strings.Repeat("p", 255)),
			want: &agentv1.IscsiChap{Username: "u", Password: strings.Repeat("p", 255)},
		},
		{
			name: "256-byte password", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("username", "u", "password", strings.Repeat("p", 256)),
			wantErr: `key "password" is 256 bytes, want 12 to 255`,
		},
		{
			name: "256-byte username", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("username", strings.Repeat("u", 256), "password", testChapPassword),
			wantErr: `key "username" is 256 bytes, want 1 to 255`,
		},
		{
			name: "missing username", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("password", testChapPassword),
			wantErr: `key "username" is required`,
		},
		{
			name: "missing password", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("username", "u"),
			wantErr: `key "password" is required`,
		},
		{
			name: "MutualCHAP missing mutualPassword", method: v1alpha1.ISCSIAuthMethodMutualCHAP,
			data:    chapData("username", "u", "password", testChapPassword, "mutualUsername", "t"),
			wantErr: `key "mutualPassword" is required`,
		},
		{
			name: "MutualCHAP missing mutualUsername", method: v1alpha1.ISCSIAuthMethodMutualCHAP,
			data:    chapData("username", "u", "password", testChapPassword, "mutualPassword", testChapMutualPass),
			wantErr: `key "mutualUsername" is required`,
		},
		{
			name: "mutual password equals password", method: v1alpha1.ISCSIAuthMethodMutualCHAP,
			data: chapData("username", "u", "password", testChapPassword,
				"mutualUsername", "t", "mutualPassword", testChapPassword),
			wantErr: `key "mutualPassword" must differ from key "password"`,
		},
		{
			name: "newline in password", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("username", "u", "password", "line-one\nline-two"),
			wantErr: `key "password" must not contain NUL or newline`,
		},
		{
			name: "NUL in username", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("username", "u\x00v", "password", testChapPassword),
			wantErr: `key "username" must not contain NUL or newline`,
		},
		{
			name: "NULL prefix is unset in LIO", method: v1alpha1.ISCSIAuthMethodCHAP,
			data:    chapData("username", "NULLuser", "password", testChapPassword),
			wantErr: `key "username" must not start with "NULL"`,
		},
		{
			name: "unknown method", method: "Kerberos", data: valid,
			wantErr: `unsupported auth method "Kerberos"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseISCSIChapSecret(tc.method, testChapSecretName, tc.data)
			if tc.wantErr != "" {
				assertChapSecretError(t, err, tc.wantErr, tc.data)
				return
			}
			if err != nil {
				t.Fatalf("ParseISCSIChapSecret: %v", err)
			}
			if got.GetUsername() != tc.want.GetUsername() || got.GetPassword() != tc.want.GetPassword() ||
				got.GetMutualUsername() != tc.want.GetMutualUsername() ||
				got.GetMutualPassword() != tc.want.GetMutualPassword() || (got == nil) != (tc.want == nil) {
				t.Errorf("chap = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// assertChapSecretError checks err names the Secret, contains want and
// leaks none of the secret values in data.
func assertChapSecretError(t *testing.T, err error, want string, data map[string]string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), testChapSecretName) {
		t.Fatalf("err = %v, want one naming secret %q and containing %q", err, testChapSecretName, want)
	}
	for _, v := range data {
		if len(v) >= 12 && strings.Contains(err.Error(), v) {
			t.Fatalf("error %q leaks a secret value", err)
		}
	}
}

// chapProtocol is an iSCSI PillarProtocol authenticating with method.
func chapProtocol(method v1alpha1.ISCSIAuthMethod) *v1alpha1.PillarProtocol {
	return iscsiProtocol(&v1alpha1.ISCSIConfig{
		ACL: true,
		Auth: &v1alpha1.ISCSIAuth{
			Method:    method,
			SecretRef: &v1alpha1.ISCSIAuthSecretReference{Name: testChapSecretName},
		},
	})
}

// chapSecretObject is the CHAP Secret in the installation namespace.
func chapSecretObject(kv ...string) *corev1.Secret {
	data := map[string][]byte{}
	for k, v := range chapData(kv...) {
		data[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testInstallNamespace, Name: testChapSecretName},
		Data:       data,
	}
}

// TestCreateVolume_ISCSIChapRecordsAuth verifies a CHAP protocol's volume:
// the export carries the Secret's credentials, the method is recorded in
// spec.resolved (fixed per volume) and handed to the node through the
// VolumeContext, while the durable exportSpec never stores the secret.
func TestCreateVolume_ISCSIChapRecordsAuth(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, chapProtocol(v1alpha1.ISCSIAuthMethodMutualCHAP),
		chapSecretObject("username", "initiator", "password", testChapPassword,
			"mutualUsername", "target", "mutualPassword", testChapMutualPass))
	env.srv.SetInstallNamespace(testInstallNamespace)
	env.agent.exportVolumeResp = iscsiExportResponse()
	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolRef] = testISCSIProtocolName

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	chap := env.agent.lastExportVolumeReq.GetExportParams().GetIscsi().GetChap()
	if chap.GetUsername() != "initiator" || chap.GetPassword() != testChapPassword ||
		chap.GetMutualUsername() != "target" || chap.GetMutualPassword() != testChapMutualPass {
		t.Errorf("ExportVolume chap = %+v, want the Secret's mutual credentials", chap)
	}
	if got := resp.GetVolume().GetVolumeContext()[VolumeContextKeyISCSIAuthMethod]; got != "MutualCHAP" {
		t.Errorf("VolumeContext[%s] = %q, want MutualCHAP", VolumeContextKeyISCSIAuthMethod, got)
	}
	auth := loadResolved(t, env, req.GetName()).Protocol.ISCSI.Auth
	if auth.EffectiveMethod() != v1alpha1.ISCSIAuthMethodMutualCHAP || auth.SecretRef.Name != testChapSecretName {
		t.Errorf("spec.resolved auth = %+v, want MutualCHAP with secret %q", auth, testChapSecretName)
	}
}

// TestCreateVolume_ISCSINoneOmitsAuthKey verifies a volume without auth
// carries no auth-method key and no CHAP credentials.
func TestCreateVolume_ISCSINoneOmitsAuthKey(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, iscsiProtocol(&v1alpha1.ISCSIConfig{ACL: true}))
	env.srv.SetInstallNamespace(testInstallNamespace)
	env.agent.exportVolumeResp = iscsiExportResponse()
	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolRef] = testISCSIProtocolName

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if v, ok := resp.GetVolume().GetVolumeContext()[VolumeContextKeyISCSIAuthMethod]; ok {
		t.Errorf("VolumeContext[%s] = %q, want absent for None", VolumeContextKeyISCSIAuthMethod, v)
	}
	if chap := env.agent.lastExportVolumeReq.GetExportParams().GetIscsi().GetChap(); chap != nil {
		t.Errorf("ExportVolume chap = %+v, want nil for None", chap)
	}
}

// TestCreateVolume_ISCSIChapSecretUnusable verifies a missing or invalid
// Secret (or an unknown installation namespace) fails CreateVolume with
// FailedPrecondition naming the Secret before anything is provisioned.
func TestCreateVolume_ISCSIChapSecretUnusable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		secret    *corev1.Secret
		namespace string
		fragment  string
	}{
		"missing secret": {namespace: testInstallNamespace, fragment: "not found"},
		"short password": {
			secret:    chapSecretObject("username", "u", "password", "short"),
			namespace: testInstallNamespace, fragment: `key "password"`,
		},
		"namespace unknown": {
			secret:   chapSecretObject("username", "u", "password", testChapPassword),
			fragment: "POD_NAMESPACE",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			objs := []ctrlclient.Object{chapProtocol(v1alpha1.ISCSIAuthMethodCHAP)}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			env := newControllerTestEnv(t, objs...)
			env.srv.SetInstallNamespace(tc.namespace)
			req := baseCreateVolumeRequest()
			req.Parameters[paramProtocolRef] = testISCSIProtocolName

			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), testChapSecretName) ||
				!strings.Contains(err.Error(), tc.fragment) {
				t.Fatalf("CreateVolume err = %v, want FailedPrecondition naming %q and %q", err, testChapSecretName, tc.fragment)
			}
			if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
				t.Errorf("agent CreateVolume/ExportVolume calls = %d/%d, want 0/0",
					env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
			}
			_, found, loadErr := env.srv.loadPillarVolumeState(context.Background(), req.GetName())
			if loadErr != nil {
				t.Fatalf("load PillarVolumeState: %v", loadErr)
			}
			if found {
				t.Error("PillarVolumeState created although the CHAP Secret is unusable")
			}
		})
	}
}

// TestResolveVolumeConfig_RetryKeepsRecordedAuth verifies auth is fixed per
// volume: a retry replays the recorded auth even after the PillarProtocol
// switched to another method.
func TestResolveVolumeConfig_RetryKeepsRecordedAuth(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, chapProtocol(v1alpha1.ISCSIAuthMethodMutualCHAP))
	recorded := &v1alpha1.ResolvedVolumeConfig{
		Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
			VolumeType: v1alpha1.ZFSVolumeTypeZvol, Pool: "tank",
		}},
		Protocol: v1alpha1.ProtocolSpec{ISCSI: &v1alpha1.ISCSIConfig{ACL: true}},
	}
	res, err := env.srv.resolveVolumeConfig(context.Background(), map[string]string{
		paramStoreRef:    testStoreName,
		paramProtocolRef: testISCSIProtocolName,
	}, nil, recorded)
	if err != nil {
		t.Fatalf("resolveVolumeConfig: %v", err)
	}
	if got := res.resolved.Protocol.ISCSI.Auth.EffectiveMethod(); got != v1alpha1.ISCSIAuthMethodNone {
		t.Errorf("replayed auth method = %s, want the recorded None", got)
	}
}

// chapPublishVolumeState is a published-ready iSCSI CHAP volume.
func chapPublishVolumeState(volumeID string) *v1alpha1.PillarVolumeState {
	pvs := volumeStateFor(volumeID)
	pvs.Status.ExportSpec = &v1alpha1.VolumeExportSpec{BindAddress: "192.168.1.10", Port: 3260, ACLEnabled: true}
	pvs.Spec.Resolved = &v1alpha1.ResolvedVolumeConfig{
		Backend:  v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{Pool: "tank"}},
		Protocol: chapProtocol(v1alpha1.ISCSIAuthMethodCHAP).Spec.Protocol,
	}
	return pvs
}

func chapCSINode(name string) *storagev1.CSINode {
	return &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{AnnotationISCSIInitiatorIQN: testChapInitiatorIQN},
	}}
}

// TestControllerPublishVolume_ISCSIChap verifies the grant of a CHAP volume
// carries the credentials read from the Secret at publish time, and that a
// missing Secret is FailedPrecondition with no grant and no publication
// record.
func TestControllerPublishVolume_ISCSIChap(t *testing.T) {
	t.Parallel()
	req := basePublishRequest()
	req.VolumeId = "storage-node-1/iscsi/zfs-zvol/tank/pvc-chap"

	t.Run("secret present", func(t *testing.T) {
		t.Parallel()
		env := newPublishTestEnv(t, chapCSINode(req.GetNodeId()), chapPublishVolumeState(req.GetVolumeId()),
			chapSecretObject("username", "initiator", "password", testChapPassword))
		env.srv.SetInstallNamespace(testInstallNamespace)
		if _, err := env.srv.ControllerPublishVolume(context.Background(), req); err != nil {
			t.Fatalf("ControllerPublishVolume: %v", err)
		}
		params := env.agent.lastAllowInitiator.GetExportParams().GetIscsi()
		if params.GetChap().GetUsername() != "initiator" || params.GetChap().GetPassword() != testChapPassword {
			t.Errorf("AllowInitiator chap = %+v, want the Secret's credentials", params.GetChap())
		}
		if params.GetBindAddress() != "192.168.1.10" || params.GetPort() != 3260 {
			t.Errorf("AllowInitiator portal = %s:%d, want the recorded 192.168.1.10:3260",
				params.GetBindAddress(), params.GetPort())
		}
	})

	t.Run("secret missing", func(t *testing.T) {
		t.Parallel()
		env := newPublishTestEnv(t, chapCSINode(req.GetNodeId()), chapPublishVolumeState(req.GetVolumeId()))
		env.srv.SetInstallNamespace(testInstallNamespace)
		_, err := env.srv.ControllerPublishVolume(context.Background(), req)
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), testChapSecretName) {
			t.Fatalf("ControllerPublishVolume err = %v, want FailedPrecondition naming %q", err, testChapSecretName)
		}
		if env.agent.allowInitiatorCalls != 0 {
			t.Errorf("AllowInitiator calls = %d, want 0", env.agent.allowInitiatorCalls)
		}
		pvsName := pillarVolumeStateNameFromVolumeID(req.GetVolumeId())
		pvs, _, err := env.srv.loadPillarVolumeState(context.Background(), pvsName)
		if err != nil {
			t.Fatalf("load PillarVolumeState: %v", err)
		}
		if len(pvs.Status.PublishedNodes) != 0 {
			t.Errorf("publication records = %+v, want none", pvs.Status.PublishedNodes)
		}
	})
}

// TestDesiredVolumeState_ISCSIChap verifies the resync and restore input of
// a CHAP volume carries the credentials on its iSCSI export parameters.
func TestDesiredVolumeState_ISCSIChap(t *testing.T) {
	t.Parallel()
	pvs := chapPublishVolumeState("storage-node-1/iscsi/zfs-zvol/tank/pvc-chap")
	pvs.UID = "uid-chap"
	chap := &agentv1.IscsiChap{Username: "initiator", Password: testChapPassword}
	desired, err := desiredVolumeState(pvs, chap)
	if err != nil {
		t.Fatalf("desiredVolumeState: %v", err)
	}
	params := desired.GetExports()[0].GetExportParams().GetIscsi()
	if params.GetChap().GetPassword() != testChapPassword || params.GetBindAddress() != "192.168.1.10" {
		t.Errorf("desired iscsi params = %+v, want the recorded portal with chap", params)
	}
}

// TestReconcileVolumeExport_ISCSIChapSecretMissing verifies the resync of a
// CHAP volume whose Secret is missing records AuthSecretInvalid without
// contacting the agent, so no export is restored without its auth.
func TestReconcileVolumeExport_ISCSIChapSecretMissing(t *testing.T) {
	t.Parallel()
	pvs := chapPublishVolumeState("storage-node-1/iscsi/zfs-zvol/tank/pvc-chap")
	env := newPublishTestEnv(t, pvs)
	env.srv.SetInstallNamespace(testInstallNamespace)

	if err := env.srv.ReconcileVolumeExport(context.Background(), pvs.Name); err == nil {
		t.Fatal("ReconcileVolumeExport succeeded although the CHAP Secret is missing")
	}
	got, _, err := env.srv.loadPillarVolumeState(context.Background(), pvs.Name)
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionExportReconciled)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonAuthSecretInvalid ||
		!strings.Contains(cond.Message, testChapSecretName) {
		t.Errorf("ExportReconciled = %+v, want False/%s naming %q", cond, reasonAuthSecretInvalid, testChapSecretName)
	}
}
