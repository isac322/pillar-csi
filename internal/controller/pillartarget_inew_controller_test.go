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

// Package controller — PRD-gap integration tests for PillarTarget lifecycle.
//
// This file implements the I-NEW-1 through I-NEW-4 test cases that cover
// PillarTarget lifecycle gaps identified during PRD gap analysis:
//
//   - I-NEW-1: gRPC address resolution (ExternalIP, CIDR filter, default addressType)
//   - I-NEW-2: addressType CRD default value
//   - I-NEW-3: nodeRef.port override and default
//
// All tests require envtest (//go:build integration).
package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// I-NEW-1: gRPC Address Resolution — ExternalIP, CIDR filter, default addressType
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("PillarTarget Controller — I-NEW address resolution", func() {
	var bctx context.Context

	BeforeEach(func() {
		bctx = context.Background()
	})

	// ─── I-NEW-1-2 ────────────────────────────────────────────────────────────
	// TestPillarTargetReconciler_ResolvesExternalIP
	//
	// When addressType=ExternalIP the reconciler must select the node's ExternalIP
	// and populate status.resolvedAddress with that IP (not the InternalIP).
	// ───────────────────────────────────────────────────────────────────────────
	Context("I-NEW-1-2 ExternalIP selection", func() {
		const (
			inewNodeName   = "inew-node-externalip"
			inewTargetName = "inew-target-externalip"
			internalIP     = "10.0.0.5"
			externalIP     = "203.0.113.10"
		)
		inewTargetNN := types.NamespacedName{Name: inewTargetName}

		BeforeEach(func() {
			// Create a Node with both InternalIP and ExternalIP.
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: inewNodeName},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: internalIP},
						{Type: corev1.NodeExternalIP, Address: externalIP},
					},
				},
			}
			Expect(k8sClient.Create(bctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(bctx, node)).To(Succeed())
		})

		AfterEach(func() {
			// Remove target (force-remove finalizer first).
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, inewTargetNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
			// Remove node.
			n := &corev1.Node{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: inewNodeName}, n); err == nil {
				if n.Labels != nil {
					delete(n.Labels, storageNodeLabel)
					Expect(k8sClient.Update(bctx, n)).To(Succeed())
				}
				Expect(k8sClient.Delete(bctx, n)).To(Succeed())
			}
		})

		It("I-NEW-1-2 TestPillarTargetReconciler_ResolvesExternalIP: should resolve ExternalIP when addressType=ExternalIP", func() {
			// Create PillarTarget with addressType=ExternalIP.
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: inewTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name:        inewNodeName,
						AddressType: "ExternalIP",
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())

			r := &PillarTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile adds finalizer.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: inewTargetNN})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile sets status.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: inewTargetNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, inewTargetNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(HavePrefix(externalIP+":"),
				"resolvedAddress should use the ExternalIP (%s), not the InternalIP (%s)", externalIP, internalIP)
		})
	})

	// ─── I-NEW-1-3 ────────────────────────────────────────────────────────────
	// TestPillarTargetReconciler_CIDRFilterSelectsCorrectIP
	//
	// When multiple InternalIPs exist and addressSelector is a CIDR, only the IP
	// within that CIDR must be selected.
	// ───────────────────────────────────────────────────────────────────────────
	Context("I-NEW-1-3 CIDR filter selects correct IP", func() {
		const (
			cidrNodeName   = "inew-node-cidr"
			cidrTargetName = "inew-target-cidr"
			cidrIP1        = "10.0.0.5"
			cidrIP2        = "192.168.219.6"
			cidrSelector   = "192.168.219.0/24"
		)
		cidrTargetNN := types.NamespacedName{Name: cidrTargetName}

		BeforeEach(func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: cidrNodeName},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: cidrIP1},
						{Type: corev1.NodeInternalIP, Address: cidrIP2},
					},
				},
			}
			Expect(k8sClient.Create(bctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(bctx, node)).To(Succeed())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, cidrTargetNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
			n := &corev1.Node{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: cidrNodeName}, n); err == nil {
				if n.Labels != nil {
					delete(n.Labels, storageNodeLabel)
					Expect(k8sClient.Update(bctx, n)).To(Succeed())
				}
				Expect(k8sClient.Delete(bctx, n)).To(Succeed())
			}
		})

		It("I-NEW-1-3 TestPillarTargetReconciler_CIDRFilterSelectsCorrectIP: should select the IP within the CIDR when multiple InternalIPs exist", func() {
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: cidrTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name:            cidrNodeName,
						AddressType:     "InternalIP",
						AddressSelector: cidrSelector,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())

			r := &PillarTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile adds finalizer.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: cidrTargetNN})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile sets status.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: cidrTargetNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, cidrTargetNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(HavePrefix(cidrIP2+":"),
				"resolvedAddress should be the IP within CIDR %s (%s), not %s", cidrSelector, cidrIP2, cidrIP1)
		})
	})

	// ─── I-NEW-1-4 ────────────────────────────────────────────────────────────
	// TestPillarTargetReconciler_DefaultAddressType_InternalIP
	//
	// When addressType is not explicitly set (empty), the reconciler defaults to
	// NodeInternalIP.  Concretely: the CRD has //+kubebuilder:default=InternalIP
	// so the API server sets addressType="InternalIP" on creation; additionally the
	// reconciler itself defaults to NodeInternalIP when addressType is empty.
	// Either way, the InternalIP must be selected.
	// ───────────────────────────────────────────────────────────────────────────
	Context("I-NEW-1-4 Default addressType resolves InternalIP", func() {
		const (
			defaultNodeName   = "inew-node-default-addr"
			defaultTargetName = "inew-target-default-addr"
			defaultIntIP      = "10.0.1.5"
			defaultExtIP      = "203.0.113.20"
		)
		defaultTargetNN := types.NamespacedName{Name: defaultTargetName}

		BeforeEach(func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: defaultNodeName},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: defaultIntIP},
						{Type: corev1.NodeExternalIP, Address: defaultExtIP},
					},
				},
			}
			Expect(k8sClient.Create(bctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(bctx, node)).To(Succeed())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, defaultTargetNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
			n := &corev1.Node{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: defaultNodeName}, n); err == nil {
				if n.Labels != nil {
					delete(n.Labels, storageNodeLabel)
					Expect(k8sClient.Update(bctx, n)).To(Succeed())
				}
				Expect(k8sClient.Delete(bctx, n)).To(Succeed())
			}
		})

		It("I-NEW-1-4 TestPillarTargetReconciler_DefaultAddressType_InternalIP: should resolve InternalIP when addressType is not explicitly set", func() {
			// Do NOT set addressType — the CRD default (InternalIP) or reconciler
			// fallback will be used.
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: defaultTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name: defaultNodeName,
						// AddressType intentionally omitted — defaults to InternalIP.
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())

			r := &PillarTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile adds finalizer.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: defaultTargetNN})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile sets status.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: defaultTargetNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, defaultTargetNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(HavePrefix(defaultIntIP+":"),
				"resolvedAddress should use InternalIP (%s) by default, not ExternalIP (%s)",
				defaultIntIP, defaultExtIP)
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// I-NEW-2: addressType CRD Default Value
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("PillarTarget CRD — I-NEW-2 addressType default", func() {
	var bctx context.Context

	BeforeEach(func() {
		bctx = context.Background()
	})

	// ─── I-NEW-2-1 ────────────────────────────────────────────────────────────
	// TestPillarTarget_DefaultAddressType
	//
	// The CRD has //+kubebuilder:default=InternalIP on nodeRef.addressType.  When a
	// PillarTarget is created without setting addressType the API server applies the
	// default, so a subsequent Get must return addressType="InternalIP".
	// ───────────────────────────────────────────────────────────────────────────
	Context("I-NEW-2-1 CRD default for addressType", func() {
		const (
			defaultAddrTypeName = "inew-target-default-addrtype"
		)
		targetNN := types.NamespacedName{Name: defaultAddrTypeName}

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, targetNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				_ = k8sClient.Update(bctx, t)
				_ = k8sClient.Delete(bctx, t)
			}
		})

		It("I-NEW-2-1 TestPillarTarget_DefaultAddressType: should apply default addressType=InternalIP when addressType is omitted", func() {
			// Create PillarTarget with nodeRef.name set but addressType omitted.
			// The omitempty json tag causes the field to be excluded from the JSON
			// payload, triggering the CRD default.
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: defaultAddrTypeName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name: "n1",
						// AddressType intentionally omitted.
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, targetNN, fetched)).To(Succeed())

			Expect(fetched.Spec.NodeRef).NotTo(BeNil(), "nodeRef should be present")
			Expect(fetched.Spec.NodeRef.AddressType).To(Equal("InternalIP"),
				"API server should default addressType to 'InternalIP' when omitted")
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// I-NEW-3: nodeRef.port Override and Default
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("PillarTarget Controller — I-NEW-3 port override and default", func() {
	var bctx context.Context

	BeforeEach(func() {
		bctx = context.Background()
	})

	// ─── I-NEW-3-1 ────────────────────────────────────────────────────────────
	// TestPillarTarget_PortOverride_StoredInSpec
	//
	// When nodeRef.port is explicitly set (e.g. 9600), the CRD must persist it.
	// A subsequent Get must return the exact port value that was set.
	// ───────────────────────────────────────────────────────────────────────────
	Context("I-NEW-3-1 port override stored in CRD spec", func() {
		const (
			portOverrideName = "inew-target-port-override"
			overridePort     = int32(9600)
		)
		targetNN := types.NamespacedName{Name: portOverrideName}

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, targetNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				_ = k8sClient.Update(bctx, t)
				_ = k8sClient.Delete(bctx, t)
			}
		})

		It("I-NEW-3-1 TestPillarTarget_PortOverride_StoredInSpec: should store nodeRef.port=9600 in spec", func() {
			port := overridePort
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: portOverrideName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name: "n1",
						Port: &port,
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, targetNN, fetched)).To(Succeed())

			Expect(fetched.Spec.NodeRef).NotTo(BeNil(), "nodeRef should be present")
			Expect(fetched.Spec.NodeRef.Port).NotTo(BeNil(), "nodeRef.port should be stored")
			Expect(*fetched.Spec.NodeRef.Port).To(Equal(overridePort),
				"nodeRef.port should be stored exactly as provided")
		})
	})

	// ─── I-NEW-3-2 ────────────────────────────────────────────────────────────
	// TestPillarTarget_PortDefault_9500
	//
	// When nodeRef.port is not set the reconciler must use the default port 9500
	// when constructing status.resolvedAddress.
	// ───────────────────────────────────────────────────────────────────────────
	Context("I-NEW-3-2 default port 9500 used when port not set", func() {
		const (
			portDefaultNodeName   = "inew-node-port-default"
			portDefaultTargetName = "inew-target-port-default"
			portDefaultIntIP      = "10.0.2.5"
		)
		targetNN := types.NamespacedName{Name: portDefaultTargetName}

		BeforeEach(func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: portDefaultNodeName},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: portDefaultIntIP},
					},
				},
			}
			Expect(k8sClient.Create(bctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(bctx, node)).To(Succeed())
		})

		AfterEach(func() {
			t := &pillarcsiv1alpha1.PillarTarget{}
			if err := k8sClient.Get(bctx, targetNN, t); err == nil {
				controllerutil.RemoveFinalizer(t, pillarTargetFinalizer)
				Expect(k8sClient.Update(bctx, t)).To(Succeed())
				Expect(k8sClient.Delete(bctx, t)).To(Succeed())
			}
			n := &corev1.Node{}
			if err := k8sClient.Get(bctx, types.NamespacedName{Name: portDefaultNodeName}, n); err == nil {
				if n.Labels != nil {
					delete(n.Labels, storageNodeLabel)
					Expect(k8sClient.Update(bctx, n)).To(Succeed())
				}
				Expect(k8sClient.Delete(bctx, n)).To(Succeed())
			}
		})

		It("I-NEW-3-2 TestPillarTarget_PortDefault_9500: should use default port 9500 when nodeRef.port is not set", func() {
			// Create PillarTarget without setting nodeRef.port.
			obj := &pillarcsiv1alpha1.PillarTarget{
				ObjectMeta: metav1.ObjectMeta{Name: portDefaultTargetName},
				Spec: pillarcsiv1alpha1.PillarTargetSpec{
					NodeRef: &pillarcsiv1alpha1.NodeRefSpec{
						Name: portDefaultNodeName,
						// Port intentionally omitted — default 9500 should be used.
					},
				},
			}
			Expect(k8sClient.Create(bctx, obj)).To(Succeed())

			r := &PillarTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile adds finalizer.
			_, err := r.Reconcile(bctx, reconcile.Request{NamespacedName: targetNN})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile sets status.
			_, err = r.Reconcile(bctx, reconcile.Request{NamespacedName: targetNN})
			Expect(err).NotTo(HaveOccurred())

			fetched := &pillarcsiv1alpha1.PillarTarget{}
			Expect(k8sClient.Get(bctx, targetNN, fetched)).To(Succeed())
			Expect(fetched.Status.ResolvedAddress).To(
				Equal(fmt.Sprintf("%s:%d", portDefaultIntIP, defaultAgentPort)),
				"resolvedAddress should use default port %d when nodeRef.port is not set", defaultAgentPort,
			)
		})
	})

})
