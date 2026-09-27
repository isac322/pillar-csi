//go:build integration

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// driftTestMountOptions are the spec.filesystem.mountOptions of the binding
// newDriftTestReconciler generates its StorageClass from.
var driftTestMountOptions = []string{"discard"}

// driftTestTopologies is an unmanaged immutable StorageClass field that a
// re-create carries over.
var driftTestTopologies = []corev1.TopologySelectorTerm{{
	MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{
		Key:    "topology.kubernetes.io/zone",
		Values: []string{"zone-a"},
	}},
}}

// newDriftTestReconciler returns a reconciler over a fake client, holding the
// returned binding, whose StorageClass has already been created from the
// returned binding.  funcs intercept the fake client.
func newDriftTestReconciler(t *testing.T, funcs interceptor.Funcs) (
	*PillarStorageClassReconciler,
	*events.FakeRecorder,
	*pillarcsiv1alpha1.PillarStorageClass,
) {
	t.Helper()
	testScheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := pillarcsiv1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("add pillar-csi scheme: %v", err)
	}

	mountOptions := append([]string{}, driftTestMountOptions...)
	binding := &pillarcsiv1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "drift-binding",
			UID:  types.UID("drift-binding-uid"),
		},
		Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
			StoreRef:    "drift-pool",
			ProtocolRef: "drift-protocol",
			Filesystem:  &pillarcsiv1alpha1.FilesystemConfig{MountOptions: &mountOptions},
		},
	}
	recorder := events.NewFakeRecorder(4)
	reconciler := &PillarStorageClassReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(binding.DeepCopy()).
			WithInterceptorFuncs(funcs).
			Build(),
		Scheme:   testScheme,
		Recorder: recorder,
	}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Name: binding.Name}, binding); err != nil {
		t.Fatalf("get binding: %v", err)
	}

	if err := reconciler.reconcileStorageClass(context.Background(), binding, binding.Name); err != nil {
		t.Fatalf("create StorageClass: %v", err)
	}
	return reconciler, recorder, binding
}

func expectEvent(t *testing.T, recorder *events.FakeRecorder, reason string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, reason) {
			t.Fatalf("event = %q, want %s", event, reason)
		}
	default:
		t.Fatalf("expected %s event, got none", reason)
	}
}

// Parameters are immutable, so drift in them is reverted by re-creating the
// StorageClass rather than by an update the API server would reject.  User
// metadata and unmanaged immutable fields survive the re-create.
func TestPillarStorageClass_ParameterDrift_RecreatesStorageClass(t *testing.T) {
	ctx := context.Background()
	reconciler, recorder, binding := newDriftTestReconciler(t, interceptor.Funcs{})

	sc := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, sc); err != nil {
		t.Fatalf("get StorageClass: %v", err)
	}
	sc.Parameters["csi.storage.k8s.io/fstype"] = "xfs"
	sc.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	sc.AllowedTopologies = driftTestTopologies
	if err := reconciler.Update(ctx, sc); err != nil {
		t.Fatalf("drift StorageClass: %v", err)
	}

	if err := reconciler.reconcileStorageClass(ctx, binding, binding.Name); err != nil {
		t.Fatalf("revert StorageClass drift: %v", err)
	}
	expectEvent(t, recorder, "StorageClassRecreated")

	got := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, got); err != nil {
		t.Fatalf("get re-created StorageClass: %v", err)
	}
	if fsType := got.Parameters["csi.storage.k8s.io/fstype"]; fsType != "ext4" {
		t.Errorf("fstype parameter = %q, want the desired %q", fsType, "ext4")
	}
	if got.Annotations["storageclass.kubernetes.io/is-default-class"] != "true" {
		t.Errorf("default-class annotation lost on re-create: %v", got.Annotations)
	}
	if !equality.Semantic.DeepEqual(got.AllowedTopologies, driftTestTopologies) {
		t.Errorf("allowedTopologies = %v, want the unmanaged %v carried over", got.AllowedTopologies, driftTestTopologies)
	}
	if !equality.Semantic.DeepEqual(got.MountOptions, driftTestMountOptions) {
		t.Errorf("mountOptions = %v, want the binding's %v", got.MountOptions, driftTestMountOptions)
	}
	if !metav1.IsControlledBy(got, binding) {
		t.Errorf("re-created StorageClass is not controlled by the binding: %v", got.OwnerReferences)
	}
}

// mountOptions are managed from spec.filesystem.mountOptions and immutable,
// so a hand edit is reverted by a re-create instead of being carried over.
func TestPillarStorageClass_MountOptionsDrift_RecreatesWithBindingOptions(t *testing.T) {
	ctx := context.Background()
	reconciler, recorder, binding := newDriftTestReconciler(t, interceptor.Funcs{})

	sc := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, sc); err != nil {
		t.Fatalf("get StorageClass: %v", err)
	}
	sc.MountOptions = []string{"noatime"}
	if err := reconciler.Update(ctx, sc); err != nil {
		t.Fatalf("drift StorageClass: %v", err)
	}

	if err := reconciler.reconcileStorageClass(ctx, binding, binding.Name); err != nil {
		t.Fatalf("revert StorageClass drift: %v", err)
	}
	expectEvent(t, recorder, "StorageClassRecreated")

	got := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, got); err != nil {
		t.Fatalf("get re-created StorageClass: %v", err)
	}
	if !equality.Semantic.DeepEqual(got.MountOptions, driftTestMountOptions) {
		t.Errorf("mountOptions = %v, want the binding's %v", got.MountOptions, driftTestMountOptions)
	}
}

// A StorageClass generated before the controller named its PillarStorageClass
// in the parameters (issue #112) lacks that immutable parameter; the upgrade
// re-creates it so CreateVolume can resolve the store and binding settings.
func TestPillarStorageClass_UpgradeAddsBindingParameter(t *testing.T) {
	ctx := context.Background()
	reconciler, recorder, binding := newDriftTestReconciler(t, interceptor.Funcs{})

	sc := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, sc); err != nil {
		t.Fatalf("get StorageClass: %v", err)
	}
	delete(sc.Parameters, "pillar-csi.bhyoo.com/storage-class")
	if err := reconciler.Update(ctx, sc); err != nil {
		t.Fatalf("revert StorageClass to its pre-#112 parameters: %v", err)
	}

	if err := reconciler.reconcileStorageClass(ctx, binding, binding.Name); err != nil {
		t.Fatalf("reconcile pre-#112 StorageClass: %v", err)
	}
	expectEvent(t, recorder, "StorageClassRecreated")

	got := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, got); err != nil {
		t.Fatalf("get re-created StorageClass: %v", err)
	}
	if name := got.Parameters["pillar-csi.bhyoo.com/storage-class"]; name != binding.Name {
		t.Errorf("storage-class parameter = %q, want the binding name %q", name, binding.Name)
	}
}

// allowVolumeExpansion is the one mutable managed field; drift in it alone is
// reverted in place.
func TestPillarStorageClass_MutableDrift_UpdatesInPlace(t *testing.T) {
	ctx := context.Background()
	reconciler, recorder, binding := newDriftTestReconciler(t, interceptor.Funcs{})

	sc := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, sc); err != nil {
		t.Fatalf("get StorageClass: %v", err)
	}
	disallow := false
	sc.AllowVolumeExpansion = &disallow
	if err := reconciler.Update(ctx, sc); err != nil {
		t.Fatalf("drift StorageClass: %v", err)
	}
	driftedUID := sc.UID

	if err := reconciler.reconcileStorageClass(ctx, binding, binding.Name); err != nil {
		t.Fatalf("revert StorageClass drift: %v", err)
	}
	expectEvent(t, recorder, "StorageClassReverted")

	got := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, got); err != nil {
		t.Fatalf("get StorageClass: %v", err)
	}
	if got.UID != driftedUID {
		t.Errorf("StorageClass was re-created (UID %s -> %s); a mutable drift must update in place", driftedUID, got.UID)
	}
	if got.AllowVolumeExpansion == nil || !*got.AllowVolumeExpansion {
		t.Errorf("allowVolumeExpansion = %v, want reverted to true", got.AllowVolumeExpansion)
	}
}

// A re-create interrupted between deleting the old StorageClass and creating
// the replacement must not lose the carried-over fields: the next reconcile
// finds the StorageClass absent and restores them.
func TestPillarStorageClass_InterruptedRecreate_KeepsCarriedOverFields(t *testing.T) {
	ctx := context.Background()
	failCreate := false
	reconciler, _, binding := newDriftTestReconciler(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*storagev1.StorageClass); ok && failCreate {
				return errors.NewServiceUnavailable("injected create failure")
			}
			return c.Create(ctx, obj, opts...)
		},
	})

	sc := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, sc); err != nil {
		t.Fatalf("get StorageClass: %v", err)
	}
	sc.Parameters["csi.storage.k8s.io/fstype"] = "xfs"
	sc.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	sc.AllowedTopologies = driftTestTopologies
	if err := reconciler.Update(ctx, sc); err != nil {
		t.Fatalf("drift StorageClass: %v", err)
	}

	// The informer cache may still serve the binding as it was before the
	// carry-over record was written, while already showing the deletion.
	staleBinding := binding.DeepCopy()

	failCreate = true
	if err := reconciler.reconcileStorageClass(ctx, binding, binding.Name); err == nil {
		t.Fatal("reconcileStorageClass succeeded despite the injected create failure")
	}
	err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, &storagev1.StorageClass{})
	if !errors.IsNotFound(err) {
		t.Fatalf("StorageClass after the failed create: err = %v, want NotFound", err)
	}

	failCreate = false
	if err := reconciler.reconcileStorageClass(ctx, staleBinding, binding.Name); err != nil {
		t.Fatalf("complete the interrupted re-create: %v", err)
	}
	stored := &pillarcsiv1alpha1.PillarStorageClass{}

	got := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, got); err != nil {
		t.Fatalf("get re-created StorageClass: %v", err)
	}
	if got.Parameters["csi.storage.k8s.io/fstype"] != "ext4" {
		t.Errorf("fstype parameter = %q, want the desired %q", got.Parameters["csi.storage.k8s.io/fstype"], "ext4")
	}
	if got.Annotations["storageclass.kubernetes.io/is-default-class"] != "true" {
		t.Errorf("default-class annotation lost by the interrupted re-create: %v", got.Annotations)
	}
	if !equality.Semantic.DeepEqual(got.AllowedTopologies, driftTestTopologies) {
		t.Errorf("allowedTopologies = %v, want %v restored", got.AllowedTopologies, driftTestTopologies)
	}
	if !equality.Semantic.DeepEqual(got.MountOptions, driftTestMountOptions) {
		t.Errorf("mountOptions = %v, want the binding's %v", got.MountOptions, driftTestMountOptions)
	}

	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, stored); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if _, ok := stored.Annotations[storageClassCarryOverAnnotation]; ok {
		t.Errorf("carry-over record left on the binding after the re-create completed: %v", stored.Annotations)
	}
}

// A StorageClass that another object controls is never deleted, even when its
// immutable fields differ from this binding's desired state.
func TestPillarStorageClass_ImmutableDrift_ForeignControllerNotDeleted(t *testing.T) {
	ctx := context.Background()
	reconciler, _, binding := newDriftTestReconciler(t, interceptor.Funcs{})

	sc := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, sc); err != nil {
		t.Fatalf("get StorageClass: %v", err)
	}
	controller := true
	sc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "pillar-csi.bhyoo.com/v1alpha1",
		Kind:       "PillarStorageClass",
		Name:       "other-binding",
		UID:        types.UID("other-binding-uid"),
		Controller: &controller,
	}}
	sc.Parameters["csi.storage.k8s.io/fstype"] = "xfs"
	if err := reconciler.Update(ctx, sc); err != nil {
		t.Fatalf("hand StorageClass to another controller: %v", err)
	}
	foreignUID := sc.UID

	err := reconciler.reconcileStorageClass(ctx, binding, binding.Name)
	if err == nil || !strings.Contains(err.Error(), "other-binding") {
		t.Fatalf("reconcileStorageClass error = %v, want refusal naming the controlling owner", err)
	}

	got := &storagev1.StorageClass{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: binding.Name}, got); err != nil {
		t.Fatalf("foreign StorageClass was deleted: %v", err)
	}
	if got.UID != foreignUID || got.Parameters["csi.storage.k8s.io/fstype"] != "xfs" {
		t.Errorf("foreign StorageClass was replaced: UID %s -> %s, params %v", foreignUID, got.UID, got.Parameters)
	}
}

var _ = Describe("PillarStorageClass Controller", func() {
	const (
		bindingName  = "test-binding"
		poolName     = "test-binding-pool"
		protocolName = "test-binding-protocol"
	)

	var (
		bctx                  context.Context
		reconciler            *PillarStorageClassReconciler
		bindingNamespacedName types.NamespacedName
	)

	BeforeEach(func() {
		bctx = context.Background()
		reconciler = &PillarStorageClassReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		bindingNamespacedName = types.NamespacedName{Name: bindingName}
	})

	// doReconcile triggers a single reconcile pass and returns result + error.
	doReconcile := func() (reconcile.Result, error) {
		return reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: bindingNamespacedName})
	}

	// createBinding creates a minimal PillarStorageClass referencing poolName and protocolName.
	createBinding := func() {
		binding := &pillarcsiv1alpha1.PillarStorageClass{}
		err := k8sClient.Get(bctx, bindingNamespacedName, binding)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: bindingName,
				},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    poolName,
					ProtocolRef: protocolName,
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
	}

	// deleteBinding deletes the PillarStorageClass (ignoring not-found).
	deleteBinding := func() {
		resource := &pillarcsiv1alpha1.PillarStorageClass{}
		if err := k8sClient.Get(bctx, bindingNamespacedName, resource); err == nil {
			Expect(k8sClient.Delete(bctx, resource)).To(Succeed())
		}
	}

	// forceRemoveBindingFinalizer strips the finalizer so the object can be GC'd.
	forceRemoveBindingFinalizer := func() {
		resource := &pillarcsiv1alpha1.PillarStorageClass{}
		if err := k8sClient.Get(bctx, bindingNamespacedName, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarStorageClassFinalizer)
			Expect(k8sClient.Update(bctx, resource)).To(Succeed())
		}
	}

	// createPool creates a PillarStore with an optional Ready condition.
	// readyStatus == nil means no condition is set on the pool.
	createPool := func(readyStatus *metav1.ConditionStatus, msg string) {
		pool := &pillarcsiv1alpha1.PillarStore{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, pool)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{
					Name: poolName,
				},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: "some-target",
					Backend: pillarcsiv1alpha1.BackendSpec{
						ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"},
					},
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
		if readyStatus != nil {
			fetched := &pillarcsiv1alpha1.PillarStore{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, fetched)).To(Succeed())
			fetched.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             *readyStatus,
					Reason:             "TestReason",
					Message:            msg,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())
		}
	}

	// deletePool deletes the PillarStore, removing any finalizer first.
	deletePool := func() {
		resource := &pillarcsiv1alpha1.PillarStore{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: poolName}, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarStoreFinalizer)
			Expect(k8sClient.Update(bctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(bctx, resource)).To(Succeed())
		}
	}

	// createProtocol creates a PillarProtocol with an optional Ready condition.
	// readyStatus == nil means no condition is set on the protocol.
	createProtocol := func(readyStatus *metav1.ConditionStatus, msg string) {
		protocol := &pillarcsiv1alpha1.PillarProtocol{}
		err := k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, protocol)
		if err != nil && errors.IsNotFound(err) {
			resource := &pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{
					Name: protocolName,
				},
				Spec: pillarcsiv1alpha1.PillarProtocolSpec{
					Protocol: pillarcsiv1alpha1.ProtocolSpec{NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{}},
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
		}
		if readyStatus != nil {
			fetched := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, fetched)).To(Succeed())
			fetched.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             *readyStatus,
					Reason:             "TestReason",
					Message:            msg,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(bctx, fetched)).To(Succeed())
		}
	}

	// deleteProtocol deletes the PillarProtocol, removing any finalizer first.
	deleteProtocol := func() {
		resource := &pillarcsiv1alpha1.PillarProtocol{}
		if err := k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, resource); err == nil {
			controllerutil.RemoveFinalizer(resource, pillarProtocolFinalizer)
			Expect(k8sClient.Update(bctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(bctx, resource)).To(Succeed())
		}
	}

	// fetchBinding fetches the current PillarStorageClass from the API server.
	fetchBinding := func() *pillarcsiv1alpha1.PillarStorageClass {
		fetched := &pillarcsiv1alpha1.PillarStorageClass{}
		Expect(k8sClient.Get(bctx, bindingNamespacedName, fetched)).To(Succeed())
		return fetched
	}

	// findBindingCondition returns the named condition from a binding, or nil.
	findBindingCondition := func(binding *pillarcsiv1alpha1.PillarStorageClass, condType string) *metav1.Condition {
		return apimeta.FindStatusCondition(binding.Status.Conditions, condType)
	}

	trueStatus := metav1.ConditionTrue
	falseStatus := metav1.ConditionFalse

	// -------------------------------------------------------------------------
	Context("Finalizer management", func() {
		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
		})

		It("should add the binding-protection finalizer on first reconcile", func() {
			createBinding()

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// After adding finalizer the reconciler returns immediately without requeue.
			Expect(result.RequeueAfter).To(BeZero())

			fetched := fetchBinding()
			Expect(controllerutil.ContainsFinalizer(fetched, pillarStorageClassFinalizer)).To(BeTrue(),
				"finalizer %q should be present after first reconcile", pillarStorageClassFinalizer)
		})

		It("should not duplicate the finalizer on subsequent reconciles", func() {
			createBinding()

			// First reconcile adds the finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile (normal path) should not duplicate.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			count := 0
			for _, f := range fetched.Finalizers {
				if f == pillarStorageClassFinalizer {
					count++
				}
			}
			Expect(count).To(Equal(1), "finalizer should appear exactly once")
		})
	})

	// -------------------------------------------------------------------------
	Context("PoolReady condition — pool does not exist", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
		})

		It("should set PoolReady=False with reason PoolNotFound when PillarStore is absent", func() {
			// No pool created — binding references a non-existent pool.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotFound"))
			Expect(cond.Message).To(ContainSubstring(poolName))

			// Requeue after a delay is expected when pool is not found.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when pool is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotFound"))
		})
	})

	// -------------------------------------------------------------------------
	Context("PoolReady condition — pool exists but is not Ready", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create pool with Ready=False.
			createPool(&falseStatus, "target not found")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
		})

		It("should set PoolReady=False with reason PoolNotReady", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotReady"))
			Expect(cond.Message).To(ContainSubstring(poolName))

			// Requeue after a delay is expected when pool is not ready.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when pool is not ready", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotReady"))
		})

		It("should include the pool's failure message in PoolReady condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("target not found"))
		})
	})

	// -------------------------------------------------------------------------
	Context("PoolReady condition — pool exists with no Ready condition yet", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create pool without any condition (nil readyStatus).
			createPool(nil, "")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
		})

		It("should set PoolReady=False when pool has no Ready condition", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PoolNotReady"))

			// Requeue expected.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})
	})

	// -------------------------------------------------------------------------
	Context("ProtocolValid condition — protocol does not exist", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create a ready pool so pool validation passes.
			createPool(&trueStatus, "pool is ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set ProtocolValid=False with reason ProtocolNotFound when PillarProtocol is absent", func() {
			// No protocol created — binding references a non-existent protocol.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotFound"))
			Expect(cond.Message).To(ContainSubstring(protocolName))

			// Requeue after a delay is expected when protocol is not found.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when protocol is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotFound"))
		})

		It("should set PoolReady=True even when protocol is absent", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionPoolReady)
			Expect(cond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// -------------------------------------------------------------------------
	Context("ProtocolValid condition — protocol exists but is not Ready", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create a ready pool so pool validation passes.
			createPool(&trueStatus, "pool is ready")
			// Create protocol with Ready=False.
			createProtocol(&falseStatus, "protocol initialization failed")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set ProtocolValid=False with reason ProtocolNotReady", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotReady"))
			Expect(cond.Message).To(ContainSubstring(protocolName))

			// Requeue after a delay is expected when protocol is not ready.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})

		It("should set Ready=False when protocol is not ready", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotReady"))
		})

		It("should include the protocol's failure message in ProtocolValid condition message", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("protocol initialization failed"))
		})
	})

	// -------------------------------------------------------------------------
	Context("ProtocolValid condition — protocol exists with no Ready condition yet", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool is ready")
			// Create protocol without any condition.
			createProtocol(nil, "")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
		})

		It("should set ProtocolValid=False when protocol has no Ready condition", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(cond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ProtocolNotReady"))

			// Requeue expected.
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingNotReady))
		})
	})

	// -------------------------------------------------------------------------
	Context("Both pool and protocol are Ready", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile to add finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Create ready pool and ready protocol.
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			// Clean up any StorageClass that may have been created.
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should set PoolReady=True and ProtocolValid=True", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			poolCond := findBindingCondition(fetched, conditionPoolReady)
			Expect(poolCond).NotTo(BeNil(), "PoolReady condition should be set")
			Expect(poolCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(poolCond.Reason).To(Equal("StoreReady"))

			protoCond := findBindingCondition(fetched, conditionProtocolValid)
			Expect(protoCond).NotTo(BeNil(), "ProtocolValid condition should be set")
			Expect(protoCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(protoCond.Reason).To(Equal("ProtocolValid"))
		})

		It("should set Compatible=True for compatible backend/protocol types", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			compatCond := findBindingCondition(fetched, conditionCompatible)
			Expect(compatCond).NotTo(BeNil(), "Compatible condition should be set")
			Expect(compatCond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should set StorageClassCreated=True and create the StorageClass", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			scCond := findBindingCondition(fetched, conditionStorageClassCreated)
			Expect(scCond).NotTo(BeNil(), "StorageClassCreated condition should be set")
			Expect(scCond.Status).To(Equal(metav1.ConditionTrue))

			// Verify the StorageClass was actually created.
			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())
			Expect(sc.Provisioner).To(Equal(pillarCSIProvisioner))
		})

		It("should set Ready=True when all conditions pass", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			readyCond := findBindingCondition(fetched, conditionReady)
			Expect(readyCond).NotTo(BeNil(), "Ready condition should be set")
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal("AllConditionsMet"))
		})

		It("should set status.storageClassName when ready", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			Expect(fetched.Status.StorageClassName).To(Equal(bindingName))
		})

		It("should not requeue when everything is ready", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	// -------------------------------------------------------------------------
	// Issue #94: editing a CRD that feeds immutable StorageClass fields must
	// converge.  The real API server of envtest rejects any update of
	// StorageClass parameters/reclaimPolicy/volumeBindingMode, which is what
	// wedged the binding in StorageClassError forever.
	// -------------------------------------------------------------------------
	Context("Editing CRDs that feed immutable StorageClass fields", func() {
		getSC := func() *storagev1.StorageClass {
			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())
			return sc
		}

		expectReady := func() {
			fetched := fetchBinding()
			scCond := findBindingCondition(fetched, conditionStorageClassCreated)
			Expect(scCond).NotTo(BeNil())
			Expect(scCond.Status).To(Equal(metav1.ConditionTrue), scCond.Message)
			readyCond := findBindingCondition(fetched, conditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), readyCond.Message)
		}

		BeforeEach(func() {
			createBinding()
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")

			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
			expectReady()
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("keeps the StorageClass after a PillarProtocol tunable edit (tunables resolve at CreateVolume)", func() {
			before := getSC()

			protocol := &pillarcsiv1alpha1.PillarProtocol{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: protocolName}, protocol)).To(Succeed())
			ctrlLossTmo := int32(1500)
			protocol.Spec.Protocol.NVMeOFTCP.CtrlLossTmo = &ctrlLossTmo
			Expect(k8sClient.Update(bctx, protocol)).To(Succeed())

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			after := getSC()
			Expect(after.UID).To(Equal(before.UID), "a tunable edit must not re-create the StorageClass")
			Expect(after.Parameters).To(Equal(before.Parameters))
			expectReady()
		})

		It("keeps the StorageClass after a binding override edit", func() {
			before := getSC()

			binding := fetchBinding()
			maxQueueSize := int32(64)
			binding.Spec.Overrides = &pillarcsiv1alpha1.StorageClassOverrides{
				Protocol: &pillarcsiv1alpha1.ProtocolOverrides{
					NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPOverrides{MaxQueueSize: &maxQueueSize},
				},
			}
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			Expect(getSC().UID).To(Equal(before.UID), "an override edit must not re-create the StorageClass")
			expectReady()
		})

		It("re-creates the StorageClass with the new fstype after a binding filesystem edit", func() {
			before := getSC()
			Expect(before.Parameters).To(HaveKeyWithValue("csi.storage.k8s.io/fstype", "ext4"))

			binding := fetchBinding()
			binding.Spec.Filesystem = &pillarcsiv1alpha1.FilesystemConfig{FSType: "xfs"}
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())

			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			after := getSC()
			Expect(after.UID).NotTo(Equal(before.UID), "an immutable change must re-create the StorageClass")
			Expect(after.Parameters).To(HaveKeyWithValue("csi.storage.k8s.io/fstype", "xfs"))
			Expect(metav1.IsControlledBy(after, fetchBinding())).To(BeTrue())
			expectReady()

			By("converging: a further reconcile leaves the re-created StorageClass alone")
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(getSC().UID).To(Equal(after.UID))
		})

		It("re-creates the StorageClass with the binding's mountOptions, and clears them with an explicit []", func() {
			before := getSC()
			Expect(before.MountOptions).To(BeEmpty())

			binding := fetchBinding()
			mountOptions := []string{"noatime", "discard"}
			binding.Spec.Filesystem = &pillarcsiv1alpha1.FilesystemConfig{MountOptions: &mountOptions}
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			withOptions := getSC()
			Expect(withOptions.UID).NotTo(Equal(before.UID))
			Expect(withOptions.MountOptions).To(Equal([]string{"noatime", "discard"}))
			expectReady()

			binding = fetchBinding()
			cleared := []string{}
			binding.Spec.Filesystem.MountOptions = &cleared
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())

			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			after := getSC()
			Expect(after.UID).NotTo(Equal(withOptions.UID))
			Expect(after.MountOptions).To(BeEmpty())
			expectReady()
		})

		It("re-creates the StorageClass when the binding's reclaimPolicy changes", func() {
			before := getSC()
			Expect(*before.ReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete))

			binding := fetchBinding()
			binding.Spec.StorageClass.ReclaimPolicy = pillarcsiv1alpha1.ReclaimPolicyRetain
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			after := getSC()
			Expect(after.UID).NotTo(Equal(before.UID))
			Expect(*after.ReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
			expectReady()
		})

		It("keeps user metadata and unmanaged immutable fields across the re-create", func() {
			sc := getSC()
			sc.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
			sc.Labels = map[string]string{"team": "storage"}
			Expect(k8sClient.Update(bctx, sc)).To(Succeed())

			binding := fetchBinding()
			binding.Spec.Filesystem = &pillarcsiv1alpha1.FilesystemConfig{FSType: "xfs"}
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			after := getSC()
			Expect(after.UID).NotTo(Equal(sc.UID))
			Expect(after.Annotations).To(HaveKeyWithValue("storageclass.kubernetes.io/is-default-class", "true"))
			Expect(after.Labels).To(HaveKeyWithValue("team", "storage"))
		})

		It("updates allowVolumeExpansion in place without re-creating the StorageClass", func() {
			before := getSC()
			Expect(before.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*before.AllowVolumeExpansion).To(BeTrue())

			binding := fetchBinding()
			disallow := false
			binding.Spec.StorageClass.AllowVolumeExpansion = &disallow
			Expect(k8sClient.Update(bctx, binding)).To(Succeed())

			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			after := getSC()
			Expect(after.UID).To(Equal(before.UID))
			Expect(*after.AllowVolumeExpansion).To(BeFalse())
			expectReady()
		})
	})

	// -------------------------------------------------------------------------
	// Sub-AC 4d: StorageClass ownerReference and parameter derivation tests
	// -------------------------------------------------------------------------

	Context("StorageClass ownerReference and key properties", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should set an ownerReference pointing to the PillarStorageClass", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())

			Expect(sc.OwnerReferences).To(HaveLen(1), "StorageClass should have exactly one owner reference")
			ownerRef := sc.OwnerReferences[0]
			Expect(ownerRef.Kind).To(Equal("PillarStorageClass"))
			Expect(ownerRef.Name).To(Equal(bindingName))
			Expect(ownerRef.Controller).NotTo(BeNil())
			Expect(*ownerRef.Controller).To(BeTrue(), "ownerReference should have controller=true")
		})

		It("should set provisioner to pillar-csi.bhyoo.com", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())
			Expect(sc.Provisioner).To(Equal(pillarCSIProvisioner))
		})

		It("should carry only the binding identity and the default fsType in StorageClass parameters", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())

			Expect(sc.Parameters).To(Equal(map[string]string{
				"pillar-csi.bhyoo.com/storage-class": bindingName,
				"csi.storage.k8s.io/fstype":          "ext4",
			}))
			Expect(sc.MountOptions).To(BeEmpty())
		})

		It("should default ReclaimPolicy to Delete", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())

			Expect(sc.ReclaimPolicy).NotTo(BeNil())
			Expect(*sc.ReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete))
		})

		It("should default VolumeBindingMode to Immediate", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())

			Expect(sc.VolumeBindingMode).NotTo(BeNil())
			Expect(*sc.VolumeBindingMode).To(Equal(storagev1.VolumeBindingImmediate))
		})

		It("should default AllowVolumeExpansion to true", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())

			Expect(sc.AllowVolumeExpansion).NotTo(BeNil())
			Expect(*sc.AllowVolumeExpansion).To(BeTrue(),
				"AllowVolumeExpansion should default to true: every served backend is an expandable block backend")
		})
	})

	// -------------------------------------------------------------------------
	Context("StorageClass with custom name (spec.storageClass.name)", func() {
		const customSCName = "my-custom-sc"

		BeforeEach(func() {
			// Create binding with an explicit StorageClass name.
			resource := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    poolName,
					ProtocolRef: protocolName,
					StorageClass: pillarcsiv1alpha1.StorageClassTemplate{
						Name: customSCName,
					},
				},
			}
			Expect(k8sClient.Create(bctx, resource)).To(Succeed())
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: customSCName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should create the StorageClass under the custom name", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: customSCName}, sc)).To(Succeed(),
				"StorageClass %q should be created", customSCName)
			Expect(sc.Provisioner).To(Equal(pillarCSIProvisioner))
		})

		It("should set status.storageClassName to the custom name", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			Expect(fetched.Status.StorageClassName).To(Equal(customSCName))
		})

		It("should NOT create a StorageClass under the binding name", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			err = k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"StorageClass %q should NOT be created when custom name is set", bindingName)
		})
	})

	// -------------------------------------------------------------------------
	Context("StorageClass with ReclaimPolicy=Retain", func() {
		BeforeEach(func() {
			res := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    poolName,
					ProtocolRef: protocolName,
					StorageClass: pillarcsiv1alpha1.StorageClassTemplate{
						ReclaimPolicy: pillarcsiv1alpha1.ReclaimPolicyRetain,
					},
				},
			}
			Expect(k8sClient.Create(bctx, res)).To(Succeed())
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should set StorageClass ReclaimPolicy to Retain", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())

			Expect(sc.ReclaimPolicy).NotTo(BeNil())
			Expect(*sc.ReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
		})
	})

	// -------------------------------------------------------------------------
	Context("StorageClass with VolumeBindingMode=WaitForFirstConsumer", func() {
		BeforeEach(func() {
			res := &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    poolName,
					ProtocolRef: protocolName,
					StorageClass: pillarcsiv1alpha1.StorageClassTemplate{
						VolumeBindingMode: pillarcsiv1alpha1.VolumeBindingWaitForFirstConsumer,
					},
				},
			}
			Expect(k8sClient.Create(bctx, res)).To(Succeed())
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should set StorageClass VolumeBindingMode to WaitForFirstConsumer", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)).To(Succeed())

			Expect(sc.VolumeBindingMode).NotTo(BeNil())
			Expect(*sc.VolumeBindingMode).To(Equal(storagev1.VolumeBindingWaitForFirstConsumer))
		})
	})

	// -------------------------------------------------------------------------
	Context("Deletion path — no blocking PVCs", func() {
		BeforeEach(func() {
			createBinding()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
			// Second reconcile creates the StorageClass and sets status.storageClassName.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())
			// Mark the binding for deletion (sets DeletionTimestamp).
			deleteBinding()
		})

		AfterEach(func() {
			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should delete the owned StorageClass and remove the finalizer", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should not requeue after clean deletion")

			// StorageClass should be deleted.
			sc := &storagev1.StorageClass{}
			err = k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "owned StorageClass should be deleted")

			// Finalizer should be removed so the binding can be garbage-collected.
			fetched := &pillarcsiv1alpha1.PillarStorageClass{}
			err = k8sClient.Get(bctx, bindingNamespacedName, fetched)
			if err == nil {
				Expect(controllerutil.ContainsFinalizer(fetched, pillarStorageClassFinalizer)).To(BeFalse(),
					"finalizer should be removed after clean deletion")
			}
		})
	})

	// -------------------------------------------------------------------------
	Context("Deletion path — blocked by PVCs referencing the StorageClass", func() {
		const testNamespace = "default"
		const pvcName = "binding-deletion-blocker"

		BeforeEach(func() {
			// Guard: ensure any PVC left over from a previous run of this
			// BeforeEach (within the same Context) is fully gone before
			// attempting to create a new one with the same name, to avoid
			// "object is being deleted" conflicts.
			Eventually(func() bool {
				p := &corev1.PersistentVolumeClaim{}
				err := k8sClient.Get(bctx, types.NamespacedName{Name: pvcName, Namespace: testNamespace}, p)
				return errors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue(), "PVC should not exist at BeforeEach start")

			createBinding()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
			// Second reconcile creates the StorageClass.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Create a PVC that references the generated StorageClass.
			scName := bindingName
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: testNamespace,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: &scName,
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			}
			Expect(k8sClient.Create(bctx, pvc)).To(Succeed())

			// Mark the binding for deletion.
			deleteBinding()
		})

		AfterEach(func() {
			// Remove any finalizers and delete the blocking PVC, then wait
			// until the object is fully gone so the next BeforeEach can
			// re-create it with the same name without hitting a "being deleted"
			// conflict from the API server.
			pvc := &corev1.PersistentVolumeClaim{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: pvcName, Namespace: testNamespace}, pvc); err == nil {
				pvc.Finalizers = nil
				Expect(k8sClient.Update(bctx, pvc)).To(Succeed())
				Expect(k8sClient.Delete(bctx, pvc)).To(Succeed())
			}
			Eventually(func() bool {
				p := &corev1.PersistentVolumeClaim{}
				err := k8sClient.Get(bctx, types.NamespacedName{Name: pvcName, Namespace: testNamespace}, p)
				return errors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue(), "PVC should be fully deleted before next test")

			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should block deletion and requeue while PVCs are present", func() {
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingDeletionBlock))
		})

		It("should set Ready=False with reason DeletionBlocked", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := fetchBinding()
			cond := findBindingCondition(fetched, conditionReady)
			Expect(cond).NotTo(BeNil(), "Ready condition should be set during deletion block")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("DeletionBlocked"))
			Expect(cond.Message).To(ContainSubstring(pvcName))
		})

		It("should not remove the finalizer while PVCs are present", func() {
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarStorageClass{}
			Expect(k8sClient.Get(bctx, bindingNamespacedName, fetched)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(fetched, pillarStorageClassFinalizer)).To(BeTrue(),
				"finalizer should remain while PVCs are blocking deletion")
		})
	})

	// -------------------------------------------------------------------------
	// E25.11.4 — DeletionAllowed_AfterPVCRemoval
	// Verify that once blocking PVCs are removed, the next reconcile removes the
	// finalizer and deletes the owned StorageClass.
	Context("Deletion path — unblocked after PVC removal", func() {
		const testNamespacePVCRemoval = "default"
		const pvcNamePVCRemoval = "binding-deletion-unblock"

		BeforeEach(func() {
			// Guard: ensure any leftover PVC from a previous run is fully gone.
			Eventually(func() bool {
				p := &corev1.PersistentVolumeClaim{}
				err := k8sClient.Get(bctx, types.NamespacedName{Name: pvcNamePVCRemoval, Namespace: testNamespacePVCRemoval}, p)
				return errors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue(), "PVC should not exist at BeforeEach start")

			createBinding()
			// First reconcile adds finalizer.
			_, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			createPool(&trueStatus, "pool ready")
			createProtocol(&trueStatus, "protocol ready")
			// Second reconcile creates the StorageClass.
			_, err = doReconcile()
			Expect(err).NotTo(HaveOccurred())

			// Create a PVC that references the generated StorageClass.
			scName := bindingName
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcNamePVCRemoval,
					Namespace: testNamespacePVCRemoval,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: &scName,
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			}
			Expect(k8sClient.Create(bctx, pvc)).To(Succeed())

			// Mark the binding for deletion.
			deleteBinding()
		})

		AfterEach(func() {
			// Remove any remaining PVC (with finalizer strip so it can be deleted).
			pvc := &corev1.PersistentVolumeClaim{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: pvcNamePVCRemoval, Namespace: testNamespacePVCRemoval}, pvc); err == nil {
				pvc.Finalizers = nil
				Expect(k8sClient.Update(bctx, pvc)).To(Succeed())
				Expect(k8sClient.Delete(bctx, pvc)).To(Succeed())
			}
			Eventually(func() bool {
				p := &corev1.PersistentVolumeClaim{}
				err := k8sClient.Get(bctx, types.NamespacedName{Name: pvcNamePVCRemoval, Namespace: testNamespacePVCRemoval}, p)
				return errors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue(), "PVC should be fully deleted after test")

			forceRemoveBindingFinalizer()
			deleteBinding()
			deletePool()
			deleteProtocol()
			sc := &storagev1.StorageClass{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc); err == nil {
				Expect(k8sClient.Delete(bctx, sc)).To(Succeed())
			}
		})

		It("should remove finalizer and delete StorageClass after all PVCs are removed", func() {
			// First reconcile while PVC is present: deletion should be blocked.
			result, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueAfterBindingDeletionBlock),
				"first reconcile should requeue while the blocking PVC exists")

			// Confirm finalizer is still present after the blocked reconcile.
			fetched := &pillarcsiv1alpha1.PillarStorageClass{}
			Expect(k8sClient.Get(bctx, bindingNamespacedName, fetched)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(fetched, pillarStorageClassFinalizer)).To(BeTrue(),
				"finalizer should remain while PVC is blocking deletion")

			// Now remove the blocking PVC (strip finalizers first to ensure immediate removal).
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(bctx, types.NamespacedName{Name: pvcNamePVCRemoval, Namespace: testNamespacePVCRemoval}, pvc)).To(Succeed())
			pvc.Finalizers = nil
			Expect(k8sClient.Update(bctx, pvc)).To(Succeed())
			Expect(k8sClient.Delete(bctx, pvc)).To(Succeed())
			Eventually(func() bool {
				p := &corev1.PersistentVolumeClaim{}
				err := k8sClient.Get(bctx, types.NamespacedName{Name: pvcNamePVCRemoval, Namespace: testNamespacePVCRemoval}, p)
				return errors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue(), "PVC should be fully removed before second reconcile")

			// Second reconcile: no PVCs → finalizer should be removed; StorageClass deleted.
			result2, err := doReconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result2.RequeueAfter).To(BeZero(),
				"should not requeue after successful deletion")

			// StorageClass should be deleted.
			sc := &storagev1.StorageClass{}
			err = k8sClient.Get(bctx, types.NamespacedName{Name: bindingName}, sc)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"owned StorageClass should be deleted after blocking PVC is removed")

			// The binding should either be fully gone or have no finalizer.
			bound := &pillarcsiv1alpha1.PillarStorageClass{}
			if err := k8sClient.Get(bctx, bindingNamespacedName, bound); err == nil {
				Expect(controllerutil.ContainsFinalizer(bound, pillarStorageClassFinalizer)).To(BeFalse(),
					"finalizer should be removed after PVC deletion unblocks deletion")
			}
		})
	})

})

// Unit tests for evaluateCompatibility — no envtest / API server required.
// Every served backend/protocol union member is a block kind, so the served
// combinations are compatible; an object whose union selects no member (one
// that bypassed the CRD schema) is reported incompatible with a message
// instead of passing silently.
var _ = Describe("evaluateCompatibility", func() {
	zfsPool := &pillarcsiv1alpha1.PillarStore{Spec: pillarcsiv1alpha1.PillarStoreSpec{
		Backend: pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"}},
	}}
	lvmPool := &pillarcsiv1alpha1.PillarStore{Spec: pillarcsiv1alpha1.PillarStoreSpec{
		Backend: pillarcsiv1alpha1.BackendSpec{LVM: &pillarcsiv1alpha1.LVMBackendConfig{VolumeGroup: "vg0"}},
	}}
	nvmeof := &pillarcsiv1alpha1.PillarProtocol{Spec: pillarcsiv1alpha1.PillarProtocolSpec{
		Protocol: pillarcsiv1alpha1.ProtocolSpec{NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{}},
	}}

	DescribeTable("served combinations are compatible",
		func(pool *pillarcsiv1alpha1.PillarStore, backend string) {
			compat := evaluateCompatibility(pool, nvmeof)
			Expect(compat.OK).To(BeTrue(), "expected compatible, got: %s", compat.Message)
			Expect(compat.Message).To(ContainSubstring(backend))
			Expect(compat.Message).To(ContainSubstring("nvmeof-tcp"))
		},
		Entry("zfs + nvmeofTcp", zfsPool, "zfs-zvol"),
		Entry("lvm + nvmeofTcp", lvmPool, "lvm-lv"),
	)

	It("reports a backend union without a member as incompatible", func() {
		compat := evaluateCompatibility(&pillarcsiv1alpha1.PillarStore{}, nvmeof)
		Expect(compat.OK).To(BeFalse())
		Expect(compat.Message).NotTo(BeEmpty())
	})

	It("reports a protocol union without a member as incompatible", func() {
		compat := evaluateCompatibility(zfsPool, &pillarcsiv1alpha1.PillarProtocol{})
		Expect(compat.OK).To(BeFalse())
		Expect(compat.Message).NotTo(BeEmpty())
	})
})

// Unit tests for the generated StorageClass fields — no envtest / API server
// required.  The generated StorageClass carries only the binding identity and
// what Kubernetes itself needs (fstype, mountOptions); backend and protocol
// tunables resolve from live CRs at CreateVolume.
var _ = Describe("desiredStorageClassFor", func() {
	makeBinding := func(fs *pillarcsiv1alpha1.FilesystemConfig) *pillarcsiv1alpha1.PillarStorageClass {
		maxQueueSize := int32(64)
		return &pillarcsiv1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-binding"},
			Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef:    "store",
				ProtocolRef: "proto",
				Filesystem:  fs,
				Overrides: &pillarcsiv1alpha1.StorageClassOverrides{
					Backend: &pillarcsiv1alpha1.BackendOverrides{
						ZFS: &pillarcsiv1alpha1.ZFSBackendOverrides{Properties: map[string]string{"compression": "zstd"}},
					},
					Protocol: &pillarcsiv1alpha1.ProtocolOverrides{
						NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPOverrides{MaxQueueSize: &maxQueueSize},
					},
				},
			},
		}
	}

	It("emits only the binding identity and the default ext4 fstype, whatever the overrides", func() {
		desired := desiredStorageClassFor(makeBinding(nil))
		Expect(desired.params).To(Equal(map[string]string{
			"pillar-csi.bhyoo.com/storage-class": "test-binding",
			"csi.storage.k8s.io/fstype":          "ext4",
		}))
		Expect(desired.mountOptions).To(BeNil())
	})

	It("uses spec.filesystem.fsType as the fstype parameter", func() {
		desired := desiredStorageClassFor(makeBinding(&pillarcsiv1alpha1.FilesystemConfig{FSType: "xfs"}))
		Expect(desired.params).To(HaveKeyWithValue("csi.storage.k8s.io/fstype", "xfs"))
	})

	It("does not put mkfsOptions on the StorageClass", func() {
		mkfs := []string{"-K"}
		desired := desiredStorageClassFor(makeBinding(&pillarcsiv1alpha1.FilesystemConfig{MkfsOptions: &mkfs}))
		Expect(desired.params).To(HaveLen(2))
	})

	It("copies spec.filesystem.mountOptions: omitted → nil, [] → empty, list → list", func() {
		Expect(desiredStorageClassFor(makeBinding(&pillarcsiv1alpha1.FilesystemConfig{FSType: "xfs"})).mountOptions).
			To(BeNil())

		cleared := []string{}
		desired := desiredStorageClassFor(makeBinding(&pillarcsiv1alpha1.FilesystemConfig{MountOptions: &cleared}))
		Expect(desired.mountOptions).NotTo(BeNil())
		Expect(desired.mountOptions).To(BeEmpty())

		opts := []string{"noatime", "discard"}
		desired = desiredStorageClassFor(makeBinding(&pillarcsiv1alpha1.FilesystemConfig{MountOptions: &opts}))
		Expect(desired.mountOptions).To(Equal([]string{"noatime", "discard"}))
		desired.mountOptions[0] = "mutated"
		Expect(opts[0]).To(Equal("noatime"), "the StorageClass must not alias the binding's list")
	})

	It("defaults allowVolumeExpansion to true and honours an explicit false", func() {
		desired := desiredStorageClassFor(makeBinding(nil))
		Expect(desired.allowVolumeExpansion).NotTo(BeNil())
		Expect(*desired.allowVolumeExpansion).To(BeTrue())

		binding := makeBinding(nil)
		disallow := false
		binding.Spec.StorageClass.AllowVolumeExpansion = &disallow
		desired = desiredStorageClassFor(binding)
		Expect(*desired.allowVolumeExpansion).To(BeFalse())
	})
})
