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

// Issue #95: a PillarStorageClass, PillarStore or PillarAgent must not be
// deleted while a PersistentVolume or PillarVolumeState of a volume
// provisioned through it remains.  Otherwise the deletion cascades down to the
// PillarAgent and a later DeleteVolume fails FailedPrecondition, leaking the
// backend volume and its export on the storage node.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var _ = Describe("Deletion guards account for PersistentVolumes and PillarVolumeStates", func() {
	var bctx context.Context
	var cleanup []client.Object

	BeforeEach(func() {
		bctx = context.Background()
		cleanup = nil
	})

	AfterEach(func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			obj := cleanup[i]
			key := client.ObjectKeyFromObject(obj)
			if err := k8sClient.Get(bctx, key, obj); err != nil {
				continue
			}
			if len(obj.GetFinalizers()) > 0 {
				obj.SetFinalizers(nil)
				Expect(client.IgnoreNotFound(k8sClient.Update(bctx, obj))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(bctx, obj))).To(Succeed())
		}
	})

	track := func(obj client.Object) client.Object {
		Expect(k8sClient.Create(bctx, obj)).To(Succeed())
		cleanup = append(cleanup, obj)
		return obj
	}

	// createPV creates a retained, unclaimed pillar-csi PV like one left
	// behind after its PVC was deleted.
	createPV := func(name, storageClassName, driver, handle string) *corev1.PersistentVolume {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				StorageClassName:              storageClassName,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: handle},
				},
			},
		}
		track(pv)
		return pv
	}

	createPVS := func(name, agent, backend, protocol, agentVolumeID string) *pillarcsiv1alpha1.PillarVolumeState {
		pvs := &pillarcsiv1alpha1.PillarVolumeState{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
				VolumeID:      agent + "/" + protocol + "/" + backend + "/" + agentVolumeID,
				AgentVolumeID: agentVolumeID,
				AgentRef:      agent,
				BackendType:   backend,
				ProtocolType:  protocol,
			},
		}
		track(pvs)
		return pvs
	}

	gone := func(obj client.Object) bool {
		err := k8sClient.Get(bctx, client.ObjectKeyFromObject(obj), obj)
		return errors.IsNotFound(err)
	}

	// remove deletes obj and, standing in for the controllers envtest lacks
	// (e.g. PV protection), completes the deletion of any volume object.
	remove := func(obj client.Object) {
		Expect(k8sClient.Delete(bctx, obj)).To(Succeed())
		switch obj.(type) {
		case *corev1.PersistentVolume, *pillarcsiv1alpha1.PillarVolumeState:
			if err := k8sClient.Get(bctx, client.ObjectKeyFromObject(obj), obj); err == nil && len(obj.GetFinalizers()) > 0 {
				obj.SetFinalizers(nil)
				Expect(k8sClient.Update(bctx, obj)).To(Succeed())
			}
		}
	}

	expectBlocked := func(obj client.Object, conditions func() []metav1.Condition, names ...string) {
		Expect(gone(obj)).To(BeFalse(), "deletion must stay blocked")
		cond := apimeta.FindStatusCondition(conditions(), "Ready")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("DeletionBlocked"))
		for _, name := range names {
			Expect(cond.Message).To(ContainSubstring(name))
		}
	}

	// ── PillarAgent ─────────────────────────────────────────────────────────
	Context("PillarAgent", func() {
		const agentName = "guard-agent"
		var (
			agent      *pillarcsiv1alpha1.PillarAgent
			reconciler *PillarAgentReconciler
		)

		reconcileAgent := func() reconcile.Result {
			result, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: agentName}})
			Expect(err).NotTo(HaveOccurred())
			return result
		}

		BeforeEach(func() {
			reconciler = &PillarAgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			agent = &pillarcsiv1alpha1.PillarAgent{
				ObjectMeta: metav1.ObjectMeta{Name: agentName},
				Spec: pillarcsiv1alpha1.PillarAgentSpec{
					External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.10", Port: 9500},
				},
			}
			track(agent)
			reconcileAgent() // adds the finalizer
		})

		agentConditions := func() []metav1.Condition { return agent.Status.Conditions }

		It("stays blocked while a PillarVolumeState on the agent remains", func() {
			pvs := createPVS("pvc-guard-agent-pvs", agentName, "lvm-lv", "nvmeof-tcp", "vg0/pvc-guard-agent-pvs")
			remove(agent)

			Expect(reconcileAgent().RequeueAfter).To(Equal(requeueAfterTargetDeletionBlock))
			expectBlocked(agent, agentConditions, pvs.Name)

			remove(pvs)
			Expect(reconcileAgent().RequeueAfter).To(BeZero())
			Expect(gone(agent)).To(BeTrue(), "PillarAgent must be released once its last volume is gone")
		})

		It("stays blocked while a PV provisioned on the agent remains, ignoring other agents and drivers", func() {
			pv := createPV("pvc-guard-agent-pv", "some-class", pillarCSIProvisioner,
				agentName+"/nvmeof-tcp/lvm-lv/vg0/pvc-guard-agent-pv")
			createPV("pvc-guard-agent-other", "some-class", pillarCSIProvisioner,
				"other-agent/nvmeof-tcp/lvm-lv/vg0/pvc-guard-agent-other")
			createPV("pvc-guard-agent-foreign", "some-class", "other.csi.example.com",
				agentName+"/nvmeof-tcp/lvm-lv/vg0/pvc-guard-agent-foreign")
			remove(agent)

			reconcileAgent()
			expectBlocked(agent, agentConditions, pv.Name)
			cond := apimeta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(cond.Message).NotTo(ContainSubstring("pvc-guard-agent-other"))
			Expect(cond.Message).NotTo(ContainSubstring("pvc-guard-agent-foreign"))

			remove(pv)
			reconcileAgent()
			Expect(gone(agent)).To(BeTrue())
		})
	})

	// ── PillarStore ─────────────────────────────────────────────────────────
	Context("PillarStore", func() {
		const (
			storeName  = "guard-store"
			storeAgent = "guard-store-agent"
		)
		var (
			store      *pillarcsiv1alpha1.PillarStore
			reconciler *PillarStoreReconciler
		)

		reconcileStore := func() reconcile.Result {
			result, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: storeName}})
			Expect(err).NotTo(HaveOccurred())
			return result
		}

		BeforeEach(func() {
			reconciler = &PillarStoreReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			store = &pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{Name: storeName},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: storeAgent,
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeLVMLV,
						LVM:  &pillarcsiv1alpha1.LVMBackendConfig{VolumeGroup: "vg-guard"},
					},
				},
			}
			track(store)
			reconcileStore() // adds the finalizer
		})

		storeConditions := func() []metav1.Condition { return store.Status.Conditions }

		It("stays blocked while a PillarVolumeState in the store remains, ignoring other volume groups", func() {
			pvs := createPVS("pvc-guard-store-pvs", storeAgent, "lvm-lv", "nvmeof-tcp", "vg-guard/pvc-guard-store-pvs")
			createPVS("pvc-guard-store-othervg", storeAgent, "lvm-lv", "nvmeof-tcp", "vg-other/pvc-guard-store-othervg")
			remove(store)

			Expect(reconcileStore().RequeueAfter).To(Equal(requeueAfterPoolDeletionBlock))
			expectBlocked(store, storeConditions, pvs.Name)
			cond := apimeta.FindStatusCondition(store.Status.Conditions, "Ready")
			Expect(cond.Message).NotTo(ContainSubstring("pvc-guard-store-othervg"))

			remove(pvs)
			Expect(reconcileStore().RequeueAfter).To(BeZero())
			Expect(gone(store)).To(BeTrue(), "PillarStore must be released once its last volume is gone")
		})

		It("stays blocked while a PV of the store remains", func() {
			pv := createPV("pvc-guard-store-pv", "some-class", pillarCSIProvisioner,
				storeAgent+"/nvmeof-tcp/lvm-lv/vg-guard/pvc-guard-store-pv")
			remove(store)

			reconcileStore()
			expectBlocked(store, storeConditions, pv.Name)

			remove(pv)
			reconcileStore()
			Expect(gone(store)).To(BeTrue())
		})
	})

	// ── PillarStorageClass ──────────────────────────────────────────────────
	Context("PillarStorageClass", func() {
		const (
			bindingName    = "guard-binding"
			bindingStore   = "guard-binding-store"
			bindingProto   = "guard-binding-proto"
			bindingAgent   = "guard-binding-agent"
			bindingZFSPool = "guard-tank"
		)
		var (
			binding    *pillarcsiv1alpha1.PillarStorageClass
			reconciler *PillarStorageClassReconciler
		)

		reconcileBinding := func() reconcile.Result {
			result, err := reconciler.Reconcile(bctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: bindingName}})
			Expect(err).NotTo(HaveOccurred())
			return result
		}

		BeforeEach(func() {
			reconciler = &PillarStorageClassReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			track(&pillarcsiv1alpha1.PillarStore{
				ObjectMeta: metav1.ObjectMeta{Name: bindingStore},
				Spec: pillarcsiv1alpha1.PillarStoreSpec{
					AgentRef: bindingAgent,
					Backend: pillarcsiv1alpha1.BackendSpec{
						Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
						ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: bindingZFSPool},
					},
				},
			})
			track(&pillarcsiv1alpha1.PillarProtocol{
				ObjectMeta: metav1.ObjectMeta{Name: bindingProto},
				Spec:       pillarcsiv1alpha1.PillarProtocolSpec{Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP},
			})
			binding = &pillarcsiv1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
					StoreRef:    bindingStore,
					ProtocolRef: bindingProto,
				},
			}
			track(binding)
			reconcileBinding() // adds the finalizer
			Expect(k8sClient.Get(bctx, client.ObjectKeyFromObject(binding), binding)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(binding, pillarStorageClassFinalizer)).To(BeTrue())
		})

		bindingConditions := func() []metav1.Condition { return binding.Status.Conditions }

		It("stays blocked while a retained PV of the StorageClass remains after its PVC is gone", func() {
			pv := createPV("pvc-guard-binding-pv", bindingName, pillarCSIProvisioner,
				bindingAgent+"/nvmeof-tcp/zfs-zvol/"+bindingZFSPool+"/pvc-guard-binding-pv")
			remove(binding)

			Expect(reconcileBinding().RequeueAfter).To(Equal(requeueAfterBindingDeletionBlock))
			expectBlocked(binding, bindingConditions, pv.Name)

			remove(pv)
			Expect(reconcileBinding().RequeueAfter).To(BeZero())
			Expect(gone(binding)).To(BeTrue(), "PillarStorageClass must be released once its last volume is gone")
		})

		It("stays blocked while a PillarVolumeState of the binding remains without a PV", func() {
			pvs := createPVS("pvc-guard-binding-pvs", bindingAgent, "zfs-zvol", "nvmeof-tcp",
				bindingZFSPool+"/pvc-guard-binding-pvs")
			// Same store and protocol, but its PV belongs to another StorageClass.
			createPVS("pvc-guard-binding-otherclass", bindingAgent, "zfs-zvol", "nvmeof-tcp",
				bindingZFSPool+"/pvc-guard-binding-otherclass")
			createPV("pvc-guard-binding-otherclass", "other-class", pillarCSIProvisioner,
				bindingAgent+"/nvmeof-tcp/zfs-zvol/"+bindingZFSPool+"/pvc-guard-binding-otherclass")
			remove(binding)

			reconcileBinding()
			expectBlocked(binding, bindingConditions, pvs.Name)
			cond := apimeta.FindStatusCondition(binding.Status.Conditions, "Ready")
			Expect(cond.Message).NotTo(ContainSubstring("pvc-guard-binding-otherclass"))

			remove(pvs)
			reconcileBinding()
			Expect(gone(binding)).To(BeTrue())
		})
	})
})
