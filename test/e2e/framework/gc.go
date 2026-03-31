//go:build e2e
// +build e2e

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

package framework

// gc.go — Resource garbage collector for pillar-csi e2e tests.
//
// ResourceTracker accumulates Kubernetes objects and namespaces created during
// a test and deletes them all in reverse registration order when Cleanup is
// called.  This lets each test register its resources once instead of
// duplicating AfterEach logic, and guarantees orderly teardown even when a
// test panics or fails early.
//
// Deletion is attempted for every registered resource regardless of earlier
// failures; all errors are collected and returned together so that cleanup
// failures do not suppress other cleanup operations.
//
// Typical usage in a Ginkgo Describe/BeforeEach block:
//
//	var (
//	    ctx     context.Context
//	    c       client.Client
//	    tracker *framework.ResourceTracker
//	    ns      *corev1.Namespace
//	)
//
//	BeforeEach(func() {
//	    ctx = context.Background()
//	    tracker = framework.NewResourceTracker()
//	    DeferCleanup(tracker.Cleanup, ctx, c)
//
//	    var err error
//	    ns, err = framework.CreateTestNamespace(ctx, c, "csi-lifecycle")
//	    Expect(err).NotTo(HaveOccurred())
//	    tracker.TrackNamespace(ns.Name)
//	})
//
//	It("provisions a volume", func() {
//	    pvc := framework.NewPillarPVC("vol-1", ns.Name, "binding-1", resource.MustParse("1Gi"))
//	    Expect(framework.CreatePVC(ctx, c, pvc)).To(Succeed())
//	    tracker.TrackPVC(pvc)
//	    // … assertions …
//	})
//
// Note: for resources that live inside a tracked Namespace, you only need to
// call TrackNamespace — the namespace deletion cascades to all namespaced
// resources it contains.  Call TrackPVC / Track for individually created
// resources in the "default" namespace or other non-test namespaces.

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ─────────────────────────────────────────────────────────────────────────────
// ResourceTracker
// ─────────────────────────────────────────────────────────────────────────────

// ResourceTracker records objects created during a test and deletes them in
// reverse order when Cleanup is called.
//
// The zero value is not usable — construct with NewResourceTracker.
type ResourceTracker struct {
	// namespaces holds the names of Namespaces to delete during Cleanup.
	// Namespace deletion cascades to all namespaced resources inside, so
	// individual PVCs/Pods/etc. inside a tracked Namespace do not need to be
	// registered separately.
	namespaces []string

	// pvcs holds individual PersistentVolumeClaims to delete during Cleanup.
	// Typically used for PVCs in non-test namespaces (e.g. "default").
	pvcs []*corev1.PersistentVolumeClaim

	// crs holds arbitrary cluster-scoped or individually tracked namespaced
	// client.Objects to delete during Cleanup.
	crs []client.Object

	// cleanupTimeout is the per-resource deletion wait timeout.
	cleanupTimeout time.Duration
}

// NewResourceTracker returns a ResourceTracker with WaitTimeout as the
// per-resource cleanup timeout.
func NewResourceTracker() *ResourceTracker {
	return &ResourceTracker{
		cleanupTimeout: WaitTimeout,
	}
}

// SetCleanupTimeout overrides the per-resource deletion wait timeout used by
// Cleanup.  Pass 0 to restore WaitTimeout.
func (rt *ResourceTracker) SetCleanupTimeout(d time.Duration) {
	if d == 0 {
		rt.cleanupTimeout = WaitTimeout
		return
	}
	rt.cleanupTimeout = d
}

// ─────────────────────────────────────────────────────────────────────────────
// Registration methods
// ─────────────────────────────────────────────────────────────────────────────

// TrackNamespace registers a Namespace name for deletion during Cleanup.
//
// Deleting the Namespace cascades to all namespaced resources it contains
// (Pods, PVCs, ConfigMaps, Secrets, …), so registering the namespace is
// sufficient — there is no need to also register the individual namespaced
// resources created inside it.
func (rt *ResourceTracker) TrackNamespace(name string) {
	rt.namespaces = append(rt.namespaces, name)
}

// TrackPVC registers a PersistentVolumeClaim for deletion during Cleanup.
//
// Use TrackNamespace instead when the PVC lives in a test Namespace that will
// be deleted — the namespace deletion cascades to the PVC automatically.
// Use TrackPVC only for PVCs that live outside any tracked namespace.
func (rt *ResourceTracker) TrackPVC(pvc *corev1.PersistentVolumeClaim) {
	rt.pvcs = append(rt.pvcs, pvc)
}

// Track registers any controller-runtime client.Object (cluster-scoped CR,
// individually tracked namespaced resource, etc.) for deletion during Cleanup.
//
// The registered object must have its Name (and Namespace, if namespaced)
// populated.  Track uses EnsureGone which issues a zero-grace-period Delete
// followed by a wait loop.
func (rt *ResourceTracker) Track(obj client.Object) {
	rt.crs = append(rt.crs, obj)
}

// ─────────────────────────────────────────────────────────────────────────────
// Cleanup
// ─────────────────────────────────────────────────────────────────────────────

// Cleanup deletes all tracked resources in reverse registration order and
// waits for each to be fully removed from the API server.
//
// Cleanup attempts deletion for every registered resource regardless of
// earlier failures within the same Cleanup call, so a single stuck resource
// does not prevent the remaining resources from being cleaned up.  All errors
// are collected and returned as a single formatted multi-error.
//
// Cleanup is safe to call multiple times; resources already absent are
// silently ignored.  After Cleanup completes (successfully or not) all
// registration lists are cleared, so a subsequent call is a no-op.
//
// Intended to be passed directly to Ginkgo's DeferCleanup:
//
//	DeferCleanup(tracker.Cleanup, ctx, c)
func (rt *ResourceTracker) Cleanup(ctx context.Context, c client.Client) error {
	var errs []string

	// ── 1. Delete individually tracked CRs (reverse order) ─────────────────
	for i := len(rt.crs) - 1; i >= 0; i-- {
		obj := rt.crs[i]
		if err := EnsureGone(ctx, c, obj, rt.cleanupTimeout); err != nil {
			errs = append(errs, fmt.Sprintf("CR %T %q: %v", obj, obj.GetName(), err))
		}
	}
	rt.crs = nil

	// ── 2. Delete individually tracked PVCs (reverse order) ─────────────────
	for i := len(rt.pvcs) - 1; i >= 0; i-- {
		pvc := rt.pvcs[i]
		if err := EnsurePVCGone(ctx, c, pvc, rt.cleanupTimeout); err != nil {
			errs = append(errs, fmt.Sprintf("PVC %q/%q: %v", pvc.Namespace, pvc.Name, err))
		}
	}
	rt.pvcs = nil

	// ── 3. Delete tracked Namespaces (reverse order) ─────────────────────────
	// Namespaces are last so that any namespace-scoped resources not explicitly
	// tracked are still cleaned up via cascade before their namespace goes away.
	for i := len(rt.namespaces) - 1; i >= 0; i-- {
		ns := rt.namespaces[i]
		if err := EnsureNamespaceGone(ctx, c, ns, rt.cleanupTimeout); err != nil {
			errs = append(errs, fmt.Sprintf("Namespace %q: %v", ns, err))
		}
	}
	rt.namespaces = nil

	if len(errs) > 0 {
		return fmt.Errorf("ResourceTracker.Cleanup: %d error(s):\n  %s",
			len(errs), strings.Join(errs, "\n  "))
	}
	return nil
}
