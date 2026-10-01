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

// The iSCSI CHAP credentials: the Secret format shared by the controller
// (which configures the target ACLs) and the node (which logs in with them),
// and the controller's per-call Secret read.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const (
	// VolumeContextKeyISCSIAuthMethod carries the volume's iSCSI
	// authentication method ("CHAP" or "MutualCHAP") from CreateVolume to
	// NodeStageVolume.  Absent means None.
	VolumeContextKeyISCSIAuthMethod = "pillar-csi.bhyoo.com/iscsi-auth-method"

	// ISCSIChapSecretKeyUsername is the Secret key of the initiator's CHAP
	// name (CHAP and MutualCHAP).
	ISCSIChapSecretKeyUsername = "username"

	// ISCSIChapSecretKeyPassword is the Secret key of the initiator's CHAP
	// secret (CHAP and MutualCHAP).
	ISCSIChapSecretKeyPassword = "password"

	// ISCSIChapSecretKeyMutualUsername is the Secret key of the target's
	// CHAP name (MutualCHAP only).
	ISCSIChapSecretKeyMutualUsername = "mutualUsername"

	// ISCSIChapSecretKeyMutualPassword is the Secret key of the target's
	// CHAP secret (MutualCHAP only).
	ISCSIChapSecretKeyMutualPassword = "mutualPassword"
)

// CHAP value bounds.  RFC 7143 §12.1.3 requires secrets of at least 96 bits
// (12 bytes); 255 bytes is the longest value LIO's configfs auth attributes
// accept.
const (
	iscsiChapMaxLen       = 255
	iscsiChapMinSecretLen = 12
)

// StorageClass parameters through which the external-provisioner records
// the node-stage Secret on a PersistentVolume; kubelet then passes the
// Secret's data to NodeStageVolume as NodeStageVolumeRequest.secrets.
const (
	StorageClassParamNodeStageSecretName      = "csi.storage.k8s.io/node-stage-secret-name"
	StorageClassParamNodeStageSecretNamespace = "csi.storage.k8s.io/node-stage-secret-namespace"
)

// ParseISCSIChapSecret validates the secret data for the method and returns
// the credentials; method None returns (nil, nil).  CHAP requires username
// and password and ignores the mutual keys; MutualCHAP also requires
// mutualUsername and mutualPassword, and mutualPassword must differ from
// password (RFC 7143 §12.1.3 forbids one secret in both directions).
// Every value is at most 255 bytes, contains no NUL or newline and does not
// start with "NULL" (LIO stores such a value as unset); passwords are at
// least 12 bytes.  SecretName is used only in error messages.  Errors name
// the offending key but never contain a secret value.
func ParseISCSIChapSecret(
	method v1alpha1.ISCSIAuthMethod,
	secretName string,
	data map[string]string,
) (*agentv1.IscsiChap, error) {
	switch method {
	case v1alpha1.ISCSIAuthMethodNone, "":
		return nil, nil //nolint:nilnil // contract: method None means "no credentials", not an error
	case v1alpha1.ISCSIAuthMethodCHAP, v1alpha1.ISCSIAuthMethodMutualCHAP:
	default:
		return nil, fmt.Errorf("iSCSI CHAP secret %q: unsupported auth method %q", secretName, method)
	}

	chap := &agentv1.IscsiChap{}
	var err error
	chap.Username, err = chapName(secretName, ISCSIChapSecretKeyUsername, data)
	if err != nil {
		return nil, err
	}
	chap.Password, err = chapSecret(secretName, ISCSIChapSecretKeyPassword, data)
	if err != nil {
		return nil, err
	}
	if method == v1alpha1.ISCSIAuthMethodCHAP {
		return chap, nil
	}

	chap.MutualUsername, err = chapName(secretName, ISCSIChapSecretKeyMutualUsername, data)
	if err != nil {
		return nil, err
	}
	chap.MutualPassword, err = chapSecret(secretName, ISCSIChapSecretKeyMutualPassword, data)
	if err != nil {
		return nil, err
	}
	if chap.MutualPassword == chap.Password {
		return nil, fmt.Errorf("iSCSI CHAP secret %q: key %q must differ from key %q",
			secretName, ISCSIChapSecretKeyMutualPassword, ISCSIChapSecretKeyPassword)
	}
	return chap, nil
}

// chapName returns the CHAP name stored under key.
func chapName(secretName, key string, data map[string]string) (string, error) {
	return chapValue(secretName, key, data, 1)
}

// chapSecret returns the CHAP secret stored under key.
func chapSecret(secretName, key string, data map[string]string) (string, error) {
	return chapValue(secretName, key, data, iscsiChapMinSecretLen)
}

// chapValue returns the value stored under key after checking it is
// minLen..255 bytes and storable in an LIO ACL auth attribute.
func chapValue(secretName, key string, data map[string]string, minLen int) (string, error) {
	v, ok := data[key]
	switch {
	case !ok || v == "":
		return "", fmt.Errorf("iSCSI CHAP secret %q: key %q is required", secretName, key)
	case len(v) < minLen || len(v) > iscsiChapMaxLen:
		return "", fmt.Errorf("iSCSI CHAP secret %q: key %q is %d bytes, want %d to %d",
			secretName, key, len(v), minLen, iscsiChapMaxLen)
	case strings.ContainsAny(v, "\x00\n"):
		return "", fmt.Errorf("iSCSI CHAP secret %q: key %q must not contain NUL or newline", secretName, key)
	case strings.HasPrefix(v, "NULL"):
		return "", fmt.Errorf("iSCSI CHAP secret %q: key %q must not start with \"NULL\"", secretName, key)
	}
	return v, nil
}

// StringSecretData converts Secret.data to the string map
// ParseISCSIChapSecret takes.
func StringSecretData(data map[string][]byte) map[string]string {
	out := make(map[string]string, len(data))
	for k, v := range data {
		out[k] = string(v)
	}
	return out
}

// errInstallNamespaceUnknown reports a CHAP Secret lookup without a known
// installation namespace.
var errInstallNamespaceUnknown = errors.New(
	"the pillar-csi installation namespace is unknown (POD_NAMESPACE is unset)")

// ReadISCSIChapSecret reads the CHAP Secret named by auth from namespace
// through reader and validates it for auth's method.  Method None returns
// (nil, nil) without reading anything.  A missing Secret, a missing
// namespace or invalid contents are reported as errors naming the Secret
// and key, never a value; IsISCSIChapSecretUnusable distinguishes them from
// API errors.
func ReadISCSIChapSecret(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	auth *v1alpha1.ISCSIAuth,
) (*agentv1.IscsiChap, error) {
	method := auth.EffectiveMethod()
	if method == v1alpha1.ISCSIAuthMethodNone {
		return nil, nil //nolint:nilnil // no credentials is the meaningful answer for method None
	}
	if auth.SecretRef == nil || auth.SecretRef.Name == "" {
		return nil, &chapSecretUnusableError{err: fmt.Errorf(
			"iSCSI auth method %s requires auth.secretRef", method)}
	}
	name := auth.SecretRef.Name
	if namespace == "" {
		return nil, &chapSecretUnusableError{err: fmt.Errorf(
			"read iSCSI CHAP secret %q: %w", name, errInstallNamespaceUnknown)}
	}
	secret := &corev1.Secret{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret)
	if k8serrors.IsNotFound(err) {
		return nil, &chapSecretUnusableError{err: fmt.Errorf(
			"iSCSI CHAP secret %q not found in namespace %q", name, namespace)}
	}
	if err != nil {
		return nil, fmt.Errorf("read iSCSI CHAP secret %s/%s: %w", namespace, name, err)
	}
	chap, err := ParseISCSIChapSecret(method, name, StringSecretData(secret.Data))
	if err != nil {
		return nil, &chapSecretUnusableError{err: err}
	}
	return chap, nil
}

// chapSecretUnusableError marks a CHAP Secret that is missing or invalid, as
// opposed to an API error reading it.
type chapSecretUnusableError struct{ err error }

func (e *chapSecretUnusableError) Error() string { return e.err.Error() }
func (e *chapSecretUnusableError) Unwrap() error { return e.err }

// IsISCSIChapSecretUnusable reports whether err from ReadISCSIChapSecret
// means the Secret is missing or invalid (a configuration problem) rather
// than unreadable.
func IsISCSIChapSecretUnusable(err error) bool {
	var unusable *chapSecretUnusableError
	return errors.As(err, &unusable)
}

// iscsiChapFor returns the CHAP credentials an agent call for a volume with
// the resolved protocol must carry: nil for a non-iSCSI protocol or method
// None, else the Secret's validated contents read uncached at call time.  A
// missing or invalid Secret is FailedPrecondition (the CO retries once the
// Secret is fixed), so no export or ACL is ever configured without the
// authentication the volume was provisioned with.
func (s *ControllerServer) iscsiChapFor(
	ctx context.Context, protocol *v1alpha1.ProtocolSpec,
) (*agentv1.IscsiChap, error) {
	if protocol == nil || protocol.ISCSI == nil {
		return nil, nil //nolint:nilnil // no credentials for a non-iSCSI volume is the meaningful answer
	}
	chap, err := ReadISCSIChapSecret(ctx, s.apiReader, s.installNamespace, protocol.ISCSI.Auth)
	switch {
	case err == nil:
		return chap, nil
	case IsISCSIChapSecretUnusable(err):
		//nolint:wrapcheck // a missing or invalid Secret is a FailedPrecondition by contract
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	default:
		//nolint:wrapcheck // agent-call preparation returns gRPC status errors
		return nil, status.Error(codes.Internal, err.Error())
	}
}

// withISCSIChap returns params with chap set on its iSCSI member; params is
// returned unchanged when chap is nil or params carries no iSCSI member.
// The input is not modified.
func withISCSIChap(params *agentv1.ExportParams, chap *agentv1.IscsiChap) *agentv1.ExportParams {
	iscsi := params.GetIscsi()
	if chap == nil || iscsi == nil {
		return params
	}
	return &agentv1.ExportParams{Params: &agentv1.ExportParams_Iscsi{Iscsi: &agentv1.IscsiExportParams{
		BindAddress: iscsi.GetBindAddress(),
		Port:        iscsi.GetPort(),
		Chap:        chap,
	}}}
}
