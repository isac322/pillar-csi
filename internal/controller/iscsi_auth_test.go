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

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

const (
	authTestNamespace = "pillar-system"
	authTestSecret    = "iscsi-chap"
	authTestProtocol  = "iscsi-chap-proto"
)

func authTestProtocolObject(method pillarcsiv1alpha1.ISCSIAuthMethod) *pillarcsiv1alpha1.PillarProtocol {
	return &pillarcsiv1alpha1.PillarProtocol{
		Name:       authTestProtocol,
		Finalizers: []string{pillarProtocolFinalizer},
		Spec: pillarcsiv1alpha1.PillarProtocolSpec{Protocol: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{
				Port: 3260,
				ACL:  true,
				Auth: &pillarcsiv1alpha1.ISCSIAuth{
					Method:    method,
					SecretRef: &pillarcsiv1alpha1.ISCSIAuthSecretReference{Name: authTestSecret},
				},
			},
		}},
	}
}

func authTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := pillarcsiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add pillar-csi scheme: %v", err)
	}
	return scheme
}

// readyCondition reconciles the protocol and returns its Ready condition.
func readyCondition(t *testing.T, r *PillarProtocolReconciler) *metav1.Condition {
	t.Helper()
	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{Name: authTestProtocol})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	if err = r.Get(ctx, types.NamespacedName{Name: authTestProtocol}, protocol); err != nil {
		t.Fatalf("get protocol: %v", err)
	}
	return apimeta.FindStatusCondition(protocol.Status.Conditions, "Ready")
}

// TestPillarProtocol_AuthSecretCondition verifies the Ready condition of a
// CHAP protocol follows its Secret: False/AuthSecretInvalid while the Secret
// is missing or invalid (naming the Secret and key), True once it is valid.
func TestPillarProtocol_AuthSecretCondition(t *testing.T) {
	ctx := context.Background()
	scheme := authTestScheme(t)
	r := &PillarProtocolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(authTestProtocolObject(pillarcsiv1alpha1.ISCSIAuthMethodMutualCHAP)).
			WithStatusSubresource(&pillarcsiv1alpha1.PillarProtocol{}).
			Build(),
		Scheme:    scheme,
		Namespace: authTestNamespace,
	}

	cond := readyCondition(t, r)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonAuthSecretInvalid ||
		!strings.Contains(cond.Message, authTestSecret) {
		t.Fatalf("Ready with Secret missing = %+v, want False/%s naming %q", cond, ReasonAuthSecretInvalid, authTestSecret)
	}

	secret := &corev1.Secret{
		Namespace: authTestNamespace, Name: authTestSecret,
		Data: map[string][]byte{
			"username": []byte("initiator"), "password": []byte("initiator-secret"),
			"mutualUsername": []byte("target"), "mutualPassword": []byte("target-secret-0"),
		},
	}
	if err := r.Create(ctx, secret); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	if cond = readyCondition(t, r); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready with a valid Secret = %+v, want True", cond)
	}

	delete(secret.Data, "mutualPassword")
	if err := r.Update(ctx, secret); err != nil {
		t.Fatalf("update Secret: %v", err)
	}
	cond = readyCondition(t, r)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonAuthSecretInvalid ||
		!strings.Contains(cond.Message, `"mutualPassword"`) {
		t.Fatalf("Ready after dropping mutualPassword = %+v, want False/%s naming the key", cond, ReasonAuthSecretInvalid)
	}
}

// TestPillarProtocol_AuthSecretNamespaceUnknown verifies a CHAP protocol is
// not Ready when the installation namespace is unknown, since no Secret can
// be read.
func TestPillarProtocol_AuthSecretNamespaceUnknown(t *testing.T) {
	scheme := authTestScheme(t)
	r := &PillarProtocolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(authTestProtocolObject(pillarcsiv1alpha1.ISCSIAuthMethodCHAP)).
			WithStatusSubresource(&pillarcsiv1alpha1.PillarProtocol{}).
			Build(),
		Scheme: scheme,
	}
	cond := readyCondition(t, r)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonAuthSecretInvalid ||
		!strings.Contains(cond.Message, "POD_NAMESPACE") {
		t.Fatalf("Ready = %+v, want False/%s naming POD_NAMESPACE", cond, ReasonAuthSecretInvalid)
	}
}

// TestPillarProtocol_MapSecretToProtocols verifies a Secret event enqueues
// exactly the protocols whose auth names it in the installation namespace.
func TestPillarProtocol_MapSecretToProtocols(t *testing.T) {
	scheme := authTestScheme(t)
	other := authTestProtocolObject(pillarcsiv1alpha1.ISCSIAuthMethodCHAP)
	other.Name = "other-secret-proto"
	other.Spec.Protocol.ISCSI.Auth.SecretRef.Name = "another-secret"
	r := &PillarProtocolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(authTestProtocolObject(pillarcsiv1alpha1.ISCSIAuthMethodCHAP), other).
			Build(),
		Scheme:    scheme,
		Namespace: authTestNamespace,
	}
	secretIn := func(namespace string) client.Object {
		return &corev1.Secret{Namespace: namespace, Name: authTestSecret}
	}
	reqs := r.mapSecretToProtocols(context.Background(), secretIn(authTestNamespace))
	if len(reqs) != 1 || reqs[0].Name != authTestProtocol {
		t.Errorf("requests = %v, want only %s", reqs, authTestProtocol)
	}
	if reqs = r.mapSecretToProtocols(context.Background(), secretIn("default")); len(reqs) != 0 {
		t.Errorf("requests for a Secret outside the installation namespace = %v, want none", reqs)
	}
}

// newAuthSCReconciler returns a StorageClass reconciler over a fake client
// holding one binding, and that binding.
func newAuthSCReconciler(t *testing.T) (*PillarStorageClassReconciler, *pillarcsiv1alpha1.PillarStorageClass) {
	t.Helper()
	scheme := authTestScheme(t)
	binding := &pillarcsiv1alpha1.PillarStorageClass{
		Name: "chap-binding", UID: types.UID("chap-binding-uid"),
		Spec: pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: "pool", ProtocolRef: authTestProtocol},
	}
	r := &PillarStorageClassReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(binding.DeepCopy()).Build(),
		Scheme: scheme,
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: binding.Name}, binding); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	return r, binding
}

// TestPillarStorageClass_NodeStageSecretParams verifies the generated
// StorageClass names the node-stage Secret only for a CHAP protocol and
// follows a change of the protocol's auth.
func TestPillarStorageClass_NodeStageSecretParams(t *testing.T) {
	ctx := context.Background()
	reconciler, binding := newAuthSCReconciler(t)
	reconciler.Namespace = authTestNamespace
	paramsOf := func() map[string]string {
		t.Helper()
		sc := &storagev1.StorageClass{}
		if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, sc); err != nil {
			t.Fatalf("get StorageClass: %v", err)
		}
		return sc.Parameters
	}
	const nameKey = "csi.storage.k8s.io/node-stage-secret-name"
	const nsKey = "csi.storage.k8s.io/node-stage-secret-namespace"

	for _, method := range []pillarcsiv1alpha1.ISCSIAuthMethod{
		pillarcsiv1alpha1.ISCSIAuthMethodCHAP, pillarcsiv1alpha1.ISCSIAuthMethodMutualCHAP,
	} {
		if err := reconciler.reconcileStorageClass(ctx, binding, authTestProtocolObject(method), binding.Name); err != nil {
			t.Fatalf("reconcile %s StorageClass: %v", method, err)
		}
		if p := paramsOf(); p[nameKey] != authTestSecret || p[nsKey] != authTestNamespace {
			t.Errorf("%s params = %v, want node-stage secret %s/%s", method, p, authTestNamespace, authTestSecret)
		}
	}

	for name, protocol := range map[string]*pillarcsiv1alpha1.PillarProtocol{
		"None": authTestProtocolObject(pillarcsiv1alpha1.ISCSIAuthMethodNone),
		"auth unset": {Spec: pillarcsiv1alpha1.PillarProtocolSpec{Protocol: pillarcsiv1alpha1.ProtocolSpec{
			ISCSI: &pillarcsiv1alpha1.ISCSIConfig{Port: 3260},
		}}},
		"nvmeof": {Spec: pillarcsiv1alpha1.PillarProtocolSpec{Protocol: pillarcsiv1alpha1.ProtocolSpec{
			NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{Port: 4420},
		}}},
	} {
		if err := reconciler.reconcileStorageClass(ctx, binding, protocol, binding.Name); err != nil {
			t.Fatalf("reconcile %s StorageClass: %v", name, err)
		}
		p := paramsOf()
		if _, ok := p[nameKey]; ok {
			t.Errorf("%s params = %v, want no node-stage secret", name, p)
		}
		if _, ok := p[nsKey]; ok {
			t.Errorf("%s params = %v, want no node-stage secret namespace", name, p)
		}
	}
}

// TestPillarStorageClass_NodeStageSecretNeedsNamespace verifies a CHAP
// protocol cannot generate a StorageClass without a known installation
// namespace instead of naming an empty namespace.
func TestPillarStorageClass_NodeStageSecretNeedsNamespace(t *testing.T) {
	reconciler, binding := newAuthSCReconciler(t)
	err := reconciler.reconcileStorageClass(context.Background(), binding,
		authTestProtocolObject(pillarcsiv1alpha1.ISCSIAuthMethodCHAP), binding.Name)
	if err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
		t.Fatalf("reconcileStorageClass err = %v, want one naming POD_NAMESPACE", err)
	}
}
