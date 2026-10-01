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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// volumeExportResyncInterval bounds how long a volume's export and ACL may
// stay missing after the storage node loses its target state without any
// Kubernetes event (e.g. nvmet reload or node reboot with the agent reachable
// at the same address).
const volumeExportResyncInterval = 30 * time.Second

// VolumeExportReconciler converges the storage-node export and ACL of one
// volume to its durable desired state.  Implemented by the CSI controller
// server, which owns the agent RPCs and the per-volume lock shared with
// ControllerPublish/Unpublish and DeleteVolume.
type VolumeExportReconciler interface {
	ReconcileVolumeExport(ctx context.Context, pvsName string) error
}

// VolumeReaper ends the lifecycle of a volume whose provisioning was
// abandoned: its claim was deleted before any PersistentVolume existed, so
// CSI DeleteVolume is never called for it.  Implemented by the CSI controller
// server, which runs the same fenced teardown as DeleteVolume.
type VolumeReaper interface {
	ReapAbandonedVolume(ctx context.Context, pvsName string) (bool, error)
}

// PillarVolumeStateReconciler keeps the exports and ACLs of provisioned
// volumes present on their storage nodes and ends the lifecycle of volumes
// whose provisioning was abandoned.  It is level-triggered: every
// PillarVolumeState is resynced on its own changes, on changes of its
// PillarAgent, on changes of the claim it is named after, and periodically,
// so target state lost to an agent restart, node reboot, or nvmet reload is
// restored without a dedicated restart signal.
type PillarVolumeStateReconciler struct {
	client.Client
	Exports VolumeExportReconciler
	Reaper  VolumeReaper
}

// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

// Reconcile ends an abandoned volume's lifecycle, or else resyncs the export
// and ACL of the volume and schedules the next periodic resync.
func (r *PillarVolumeStateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pvs := &pillarcsiv1alpha1.PillarVolumeState{}
	err := r.Get(ctx, req.NamespacedName, pvs)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pvs.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	reaped, err := r.Reaper.ReapAbandonedVolume(ctx, req.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reap abandoned PillarVolumeState %q: %w", req.Name, err)
	}
	if reaped {
		return ctrl.Result{}, nil
	}

	err = r.Exports.ReconcileVolumeExport(ctx, req.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile export of PillarVolumeState %q: %w", req.Name, err)
	}
	return ctrl.Result{RequeueAfter: volumeExportResyncInterval}, nil
}

// SetupWithManager registers the reconciler, re-enqueueing every volume of a
// PillarAgent whenever that agent changes (e.g. it becomes reachable again)
// and the volume of a claim whenever that claim changes (its deletion may
// abandon the volume's provisioning).
func (r *PillarVolumeStateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapVolumes := func(ctx context.Context, what string, match func(*pillarcsiv1alpha1.PillarVolumeState) bool,
	) []reconcile.Request {
		volumes := &pillarcsiv1alpha1.PillarVolumeStateList{}
		err := mgr.GetClient().List(ctx, volumes)
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "list PillarVolumeStates", "for", what)
			return nil
		}
		var requests []reconcile.Request
		for i := range volumes.Items {
			if match(&volumes.Items[i]) {
				requests = append(requests, reconcile.Request{
					Name: volumes.Items[i].Name,
				})
			}
		}
		return requests
	}
	mapAgentToVolumes := func(ctx context.Context, obj client.Object) []reconcile.Request {
		return mapVolumes(ctx, "PillarAgent "+obj.GetName(), func(pvs *pillarcsiv1alpha1.PillarVolumeState) bool {
			return pvs.Spec.AgentRef == obj.GetName()
		})
	}
	mapClaimToVolumes := func(ctx context.Context, obj client.Object) []reconcile.Request {
		uid := string(obj.GetUID())
		return mapVolumes(ctx, "PersistentVolumeClaim "+obj.GetNamespace()+"/"+obj.GetName(),
			func(pvs *pillarcsiv1alpha1.PillarVolumeState) bool {
				ref := pvs.Spec.ClaimRef
				return pvs.Name == "pvc-"+uid || (ref != nil && ref.UID == uid)
			})
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarVolumeState{}).
		Watches(
			&pillarcsiv1alpha1.PillarAgent{},
			handler.EnqueueRequestsFromMapFunc(mapAgentToVolumes),
		).
		Watches(
			&corev1.PersistentVolumeClaim{},
			handler.EnqueueRequestsFromMapFunc(mapClaimToVolumes),
		).
		Named("pillarvolumestate").
		Complete(r)
}
