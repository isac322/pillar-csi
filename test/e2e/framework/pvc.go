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

// pvc.go — PersistentVolumeClaim / PersistentVolume lifecycle helpers for
// pillar-csi e2e tests.
//
// This file provides:
//
//   - Builder functions (NewPVC, NewPillarPVC) that assemble PVC objects with
//     sensible defaults so individual tests only need to specify the fields
//     that vary.
//
//   - CRUD helpers (CreatePVC, DeletePVCAndWait, EnsurePVCGone) that wrap
//     controller-runtime client calls with structured error messages.
//
//   - Wait helpers (WaitForPVCBound, WaitForPVCPhase) that poll the API server
//     until the PVC reaches the expected phase, using the same poll interval
//     and timeout constants as the rest of the framework.
//
//   - PV inspection helpers (GetBoundPV, AssertPVCapacity,
//     AssertPVStorageClass, AssertPVReclaimPolicy) for asserting storage
//     provisioning outcomes after a PVC becomes Bound.
//
// Typical usage in a Ginkgo It block:
//
//	pvc := framework.NewPillarPVC("test-vol", "default", "my-binding", resource.MustParse("1Gi"))
//	Expect(framework.CreatePVC(ctx, c, pvc)).To(Succeed())
//	Expect(framework.WaitForPVCBound(ctx, c, pvc, 3*time.Minute)).To(Succeed())
//
//	pv, err := framework.GetBoundPV(ctx, c, pvc)
//	Expect(err).NotTo(HaveOccurred())
//	Expect(framework.AssertPVCapacity(pv, resource.MustParse("1Gi"))).To(Succeed())
//
//	Expect(framework.EnsurePVCGone(ctx, c, pvc, 2*time.Minute)).To(Succeed())

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/fields"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ─────────────────────────────────────────────────────────────────────────────
// PVC builder helpers
// ─────────────────────────────────────────────────────────────────────────────

// NewPVC returns a PersistentVolumeClaim with the given name, namespace,
// StorageClass, access modes, and storage request.  The object is ready to be
// passed to CreatePVC or Apply.
//
// Pass nil for accessModes to use the default [ReadWriteOnce].
//
// Example:
//
//	pvc := framework.NewPVC(
//	    "my-pvc", "default", "my-storage-class",
//	    resource.MustParse("5Gi"),
//	    []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
//	)
func NewPVC(
	name, namespace, storageClassName string,
	capacity resource.Quantity,
	accessModes []corev1.PersistentVolumeAccessMode,
) *corev1.PersistentVolumeClaim {
	if len(accessModes) == 0 {
		accessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	sc := storageClassName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			AccessModes:      accessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: capacity,
				},
			},
		},
	}
}

// NewPillarPVC is a convenience builder for a ReadWriteOnce PVC that targets a
// StorageClass created by a PillarBinding.
//
// pillar-csi generates a StorageClass whose name matches the PillarBinding
// name, so pass the PillarBinding name as storageClassName.
//
// Example:
//
//	pvc := framework.NewPillarPVC("vol-1", "default", "my-binding", resource.MustParse("10Gi"))
func NewPillarPVC(name, namespace, bindingName string, capacity resource.Quantity) *corev1.PersistentVolumeClaim {
	return NewPVC(name, namespace, bindingName, capacity,
		[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce})
}

// ─────────────────────────────────────────────────────────────────────────────
// PVC CRUD operations
// ─────────────────────────────────────────────────────────────────────────────

// CreatePVC creates pvc in the cluster.  It returns an error if the PVC
// already exists or if the API server rejects the request.
//
// Use Apply instead of CreatePVC when an idempotent create-or-update is needed.
func CreatePVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) error {
	if err := c.Create(ctx, pvc); err != nil {
		return fmt.Errorf("framework CreatePVC %q/%q: %w", pvc.Namespace, pvc.Name, err)
	}
	return nil
}

// DeletePVC removes pvc from the cluster.  It returns nil if the PVC is
// already absent (idempotent).
func DeletePVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim, opts ...client.DeleteOption) error {
	if err := c.Delete(ctx, pvc, opts...); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("framework DeletePVC %q/%q: %w", pvc.Namespace, pvc.Name, err)
	}
	return nil
}

// DeletePVCAndWait deletes pvc and then blocks until it is fully removed from
// the API server.  If the PVC is already absent the function returns
// immediately.
//
// Pass 0 for timeout to use WaitTimeout.
func DeletePVCAndWait(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim, timeout time.Duration) error {
	if err := DeletePVC(ctx, c, pvc, client.GracePeriodSeconds(0)); err != nil {
		return err
	}
	return waitForPVCDeletion(ctx, c, pvc, timeout)
}

// EnsurePVCGone is a convenience wrapper that combines DeletePVC and
// waitForPVCDeletion: it deletes pvc (if still present) and waits for full
// removal.  Suitable for AfterEach cleanup blocks where the PVC may or may not
// exist at cleanup time.
//
// Pass 0 for timeout to use WaitTimeout.
func EnsurePVCGone(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim, timeout time.Duration) error {
	if err := DeletePVC(ctx, c, pvc, client.GracePeriodSeconds(0)); err != nil {
		return err
	}
	return waitForPVCDeletion(ctx, c, pvc, timeout)
}

// EnsurePVCAndPVGone deletes pvc and waits for both the PVC and its bound PV
// to be fully removed.  This ensures that the CSI DeleteVolume RPC completes
// before returning, preventing stale backend resources (e.g. ZFS zvols) from
// accumulating across test runs.
func EnsurePVCAndPVGone(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim, timeout time.Duration) error {
	if timeout == 0 {
		timeout = WaitTimeout
	}
	// Capture the bound PV name before deleting the PVC.
	key := client.ObjectKeyFromObject(pvc)
	if err := c.Get(ctx, key, pvc); err != nil {
		if errors.IsNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("framework EnsurePVCAndPVGone: get PVC %q/%q: %w", key.Namespace, key.Name, err)
	}
	pvName := pvc.Spec.VolumeName

	// Delete PVC and wait for it to disappear.
	if err := EnsurePVCGone(ctx, c, pvc, timeout); err != nil {
		return err
	}

	// If the PVC was bound to a PV, wait for the PV to be deleted too.
	if pvName == "" {
		return nil
	}
	pv := &corev1.PersistentVolume{}
	pv.Name = pvName
	return waitForPVDeletion(ctx, c, pv, timeout)
}

// waitForPVDeletion polls until pv is fully removed from the API server.
func waitForPVDeletion(ctx context.Context, c client.Client, pv *corev1.PersistentVolume, timeout time.Duration) error {
	if timeout == 0 {
		timeout = WaitTimeout
	}
	key := client.ObjectKeyFromObject(pv)
	err := wait.PollUntilContextTimeout(ctx, PollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if fetchErr := c.Get(ctx, key, pv); fetchErr != nil {
				if errors.IsNotFound(fetchErr) {
					return true, nil
				}
				return false, fetchErr
			}
			return false, nil
		},
	)
	if err != nil {
		return fmt.Errorf("waitForPVDeletion %q: %w", key.Name, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PVC wait helpers
// ─────────────────────────────────────────────────────────────────────────────

// WaitForPVCBound polls until pvc reaches corev1.ClaimBound phase, or the
// context / timeout is exceeded.
//
// pvc is updated in-place with the latest server state on each poll cycle.
// On success the caller can inspect pvc.Spec.VolumeName to obtain the bound
// PV name.
//
// Pass 0 for timeout to use WaitTimeout.
//
// Example:
//
//	Expect(framework.WaitForPVCBound(ctx, c, pvc, 3*time.Minute)).To(Succeed())
func WaitForPVCBound(
	ctx context.Context,
	c client.Client,
	pvc *corev1.PersistentVolumeClaim,
	timeout time.Duration,
) error {
	return WaitForPVCPhase(ctx, c, pvc, corev1.ClaimBound, timeout)
}

// WaitForPVCPhase polls until pvc.Status.Phase equals wantPhase, or the
// context / timeout is exceeded.
//
// pvc is updated in-place with the latest server state on each poll cycle.
//
// Pass 0 for timeout to use WaitTimeout.
func WaitForPVCPhase(
	ctx context.Context,
	c client.Client,
	pvc *corev1.PersistentVolumeClaim,
	wantPhase corev1.PersistentVolumeClaimPhase,
	timeout time.Duration,
) error {
	if timeout == 0 {
		timeout = WaitTimeout
	}

	key := client.ObjectKeyFromObject(pvc)
	var lastPhase corev1.PersistentVolumeClaimPhase

	err := wait.PollUntilContextTimeout(ctx, PollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if fetchErr := c.Get(ctx, key, pvc); fetchErr != nil {
				if errors.IsNotFound(fetchErr) {
					lastPhase = ""
					return false, nil
				}
				return false, fetchErr
			}
			lastPhase = pvc.Status.Phase
			return pvc.Status.Phase == wantPhase, nil
		},
	)
	if err != nil {
		// Dump PVC events to help diagnose why provisioning didn't happen.
		dumpPVCEvents(ctx, c, key)
		return fmt.Errorf(
			"WaitForPVCPhase %q/%q: want phase=%s, last observed phase=%s: %w",
			key.Namespace, key.Name, wantPhase, lastPhase, err,
		)
	}
	return nil
}

// dumpPVCEvents prints Kubernetes events for the PVC to stdout for diagnostics.
func dumpPVCEvents(ctx context.Context, c client.Client, key client.ObjectKey) {
	evList := &corev1.EventList{}
	listOpts := &client.ListOptions{
		Namespace: key.Namespace,
		FieldSelector: fields.SelectorFromSet(fields.Set{
			"involvedObject.name": key.Name,
			"involvedObject.kind": "PersistentVolumeClaim",
		}),
	}
	diagCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if listErr := c.List(diagCtx, evList, listOpts); listErr != nil {
		fmt.Fprintf(os.Stdout, "  [diag] failed to list events for PVC %s/%s: %v\n", key.Namespace, key.Name, listErr)
		return
	}
	if len(evList.Items) == 0 {
		fmt.Fprintf(os.Stdout, "  [diag] no events found for PVC %s/%s\n", key.Namespace, key.Name)
		return
	}
	fmt.Fprintf(os.Stdout, "  [diag] events for PVC %s/%s (%d events):\n", key.Namespace, key.Name, len(evList.Items))
	for i := range evList.Items {
		ev := &evList.Items[i]
		fmt.Fprintf(os.Stdout, "    %s %s/%s: %s (count=%d, age=%s)\n",
			ev.Reason, ev.InvolvedObject.Kind, ev.InvolvedObject.Name,
			ev.Message, ev.Count, time.Since(ev.LastTimestamp.Time).Round(time.Second))
	}
}

// waitForPVCDeletion polls until pvc is fully removed from the API server.
func waitForPVCDeletion(
	ctx context.Context,
	c client.Client,
	pvc *corev1.PersistentVolumeClaim,
	timeout time.Duration,
) error {
	if timeout == 0 {
		timeout = WaitTimeout
	}

	key := client.ObjectKeyFromObject(pvc)

	err := wait.PollUntilContextTimeout(ctx, PollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if fetchErr := c.Get(ctx, key, pvc); fetchErr != nil {
				if errors.IsNotFound(fetchErr) {
					return true, nil
				}
				return false, fetchErr
			}
			return false, nil
		},
	)
	if err != nil {
		return fmt.Errorf("waitForPVCDeletion %q/%q: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PV inspection helpers
// ─────────────────────────────────────────────────────────────────────────────

// GetBoundPV retrieves the PersistentVolume that pvc is bound to.  It returns
// an error if:
//   - pvc.Spec.VolumeName is empty (PVC is not yet Bound)
//   - the PV cannot be fetched from the API server
//
// Callers typically call WaitForPVCBound before GetBoundPV to ensure the PVC
// is in the Bound phase.
func GetBoundPV(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolume, error) {
	if pvc.Spec.VolumeName == "" {
		return nil, fmt.Errorf(
			"framework GetBoundPV: PVC %q/%q has no bound volume (phase=%s)",
			pvc.Namespace, pvc.Name, pvc.Status.Phase,
		)
	}

	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return nil, fmt.Errorf(
			"framework GetBoundPV: get PV %q for PVC %q/%q: %w",
			pvc.Spec.VolumeName, pvc.Namespace, pvc.Name, err,
		)
	}
	return pv, nil
}

// AssertPVCapacity checks that pv has at least wantCapacity of storage.
// It returns nil when the actual capacity is ≥ wantCapacity, or a descriptive
// error otherwise.
//
// Example:
//
//	Expect(framework.AssertPVCapacity(pv, resource.MustParse("10Gi"))).To(Succeed())
func AssertPVCapacity(pv *corev1.PersistentVolume, wantCapacity resource.Quantity) error {
	actual, ok := pv.Spec.Capacity[corev1.ResourceStorage]
	if !ok {
		return fmt.Errorf(
			"framework AssertPVCapacity: PV %q has no storage capacity set",
			pv.Name,
		)
	}

	// actual.Cmp returns -1 when actual < want
	if actual.Cmp(wantCapacity) < 0 {
		return fmt.Errorf(
			"framework AssertPVCapacity: PV %q capacity %s < requested %s",
			pv.Name, actual.String(), wantCapacity.String(),
		)
	}
	return nil
}

// AssertPVStorageClass checks that pv.Spec.StorageClassName equals
// wantStorageClass.  Returns nil on match, or a descriptive error on mismatch.
//
// pillar-csi sets the StorageClass to the PillarBinding name, so pass the
// PillarBinding name as wantStorageClass.
func AssertPVStorageClass(pv *corev1.PersistentVolume, wantStorageClass string) error {
	if pv.Spec.StorageClassName != wantStorageClass {
		return fmt.Errorf(
			"framework AssertPVStorageClass: PV %q has StorageClass %q, want %q",
			pv.Name, pv.Spec.StorageClassName, wantStorageClass,
		)
	}
	return nil
}

// AssertPVReclaimPolicy checks that pv.Spec.PersistentVolumeReclaimPolicy
// equals wantPolicy.  Returns nil on match, or a descriptive error on
// mismatch.
//
// CSI drivers typically set the reclaim policy from the StorageClass (Delete
// or Retain).  Use this helper to verify the provisioner respected the
// StorageClass reclaimPolicy field.
func AssertPVReclaimPolicy(pv *corev1.PersistentVolume, wantPolicy corev1.PersistentVolumeReclaimPolicy) error {
	if pv.Spec.PersistentVolumeReclaimPolicy != wantPolicy {
		return fmt.Errorf(
			"framework AssertPVReclaimPolicy: PV %q has reclaimPolicy=%s, want %s",
			pv.Name, pv.Spec.PersistentVolumeReclaimPolicy, wantPolicy,
		)
	}
	return nil
}

// AssertPVAccessModes checks that pv has at least the requested access modes.
// It returns nil when all wantModes are present in pv.Spec.AccessModes, or a
// descriptive error listing the missing modes.
func AssertPVAccessModes(pv *corev1.PersistentVolume, wantModes []corev1.PersistentVolumeAccessMode) error {
	have := make(map[corev1.PersistentVolumeAccessMode]bool, len(pv.Spec.AccessModes))
	for _, m := range pv.Spec.AccessModes {
		have[m] = true
	}

	var missing []corev1.PersistentVolumeAccessMode
	for _, m := range wantModes {
		if !have[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"framework AssertPVAccessModes: PV %q is missing access modes %v (have %v)",
			pv.Name, missing, pv.Spec.AccessModes,
		)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Composite provisioning assertion
// ─────────────────────────────────────────────────────────────────────────────

// ProvisioningOutcome collects the expected outcomes of a successful storage
// provisioning request.  All fields are optional; only non-zero values are
// checked by AssertProvisioningOutcome.
type ProvisioningOutcome struct {
	// WantCapacity, when non-zero, asserts that the PV capacity is ≥ this value.
	WantCapacity resource.Quantity

	// WantStorageClass, when non-empty, asserts that the PV StorageClass name
	// equals this value.
	WantStorageClass string

	// WantReclaimPolicy, when non-empty, asserts that the PV reclaim policy
	// equals this value.
	WantReclaimPolicy corev1.PersistentVolumeReclaimPolicy

	// WantAccessModes, when non-nil, asserts that the PV has at least these
	// access modes.
	WantAccessModes []corev1.PersistentVolumeAccessMode
}

// AssertProvisioningOutcome waits for pvc to become Bound (within timeout),
// retrieves the bound PV, and validates it against the fields set in outcome.
//
// This is the primary composite assertion for storage provisioning e2e tests:
// a single call covers the full "PVC created → PV provisioned → PV correct"
// flow.
//
// Pass 0 for timeout to use WaitTimeout.
//
// Example:
//
//	Expect(framework.AssertProvisioningOutcome(ctx, c, pvc, 3*time.Minute,
//	    framework.ProvisioningOutcome{
//	        WantCapacity:      resource.MustParse("10Gi"),
//	        WantStorageClass:  "my-binding",
//	        WantReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
//	        WantAccessModes:   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
//	    },
//	)).To(Succeed())
func AssertProvisioningOutcome(
	ctx context.Context,
	c client.Client,
	pvc *corev1.PersistentVolumeClaim,
	timeout time.Duration,
	outcome ProvisioningOutcome,
) error {
	// Step 1: Wait for the PVC to become Bound.
	if err := WaitForPVCBound(ctx, c, pvc, timeout); err != nil {
		return fmt.Errorf("AssertProvisioningOutcome: %w", err)
	}

	// Step 2: Fetch the bound PV.
	pv, err := GetBoundPV(ctx, c, pvc)
	if err != nil {
		return fmt.Errorf("AssertProvisioningOutcome: %w", err)
	}

	// Step 3: Run all requested assertions.
	if !outcome.WantCapacity.IsZero() {
		if err := AssertPVCapacity(pv, outcome.WantCapacity); err != nil {
			return fmt.Errorf("AssertProvisioningOutcome: %w", err)
		}
	}
	if outcome.WantStorageClass != "" {
		if err := AssertPVStorageClass(pv, outcome.WantStorageClass); err != nil {
			return fmt.Errorf("AssertProvisioningOutcome: %w", err)
		}
	}
	if outcome.WantReclaimPolicy != "" {
		if err := AssertPVReclaimPolicy(pv, outcome.WantReclaimPolicy); err != nil {
			return fmt.Errorf("AssertProvisioningOutcome: %w", err)
		}
	}
	if len(outcome.WantAccessModes) > 0 {
		if err := AssertPVAccessModes(pv, outcome.WantAccessModes); err != nil {
			return fmt.Errorf("AssertProvisioningOutcome: %w", err)
		}
	}
	return nil
}
