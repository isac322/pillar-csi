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

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
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

// PillarVolumeStateReconciler keeps the exports and ACLs of provisioned
// volumes present on their storage nodes.  It is level-triggered: every
// PillarVolumeState is resynced on its own changes, on changes of its
// PillarAgent, and periodically, so target state lost to an agent restart,
// node reboot, or nvmet reload is restored without a dedicated restart signal.
type PillarVolumeStateReconciler struct {
	client.Client
	Exports VolumeExportReconciler
}

// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates/status,verbs=get;update;patch

// Reconcile resyncs the export and ACL of one volume and schedules the next
// periodic resync.
func (r *PillarVolumeStateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pvs := &pillarcsiv1alpha1.PillarVolumeState{}
	err := r.Get(ctx, req.NamespacedName, pvs)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pvs.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	err = r.Exports.ReconcileVolumeExport(ctx, req.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile export of PillarVolumeState %q: %w", req.Name, err)
	}
	return ctrl.Result{RequeueAfter: volumeExportResyncInterval}, nil
}

// SetupWithManager registers the reconciler, re-enqueueing every volume of a
// PillarAgent whenever that agent changes (e.g. it becomes reachable again).
func (r *PillarVolumeStateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapAgentToVolumes := func(ctx context.Context, obj client.Object) []reconcile.Request {
		volumes := &pillarcsiv1alpha1.PillarVolumeStateList{}
		err := mgr.GetClient().List(ctx, volumes)
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "list PillarVolumeStates for PillarAgent", "agent", obj.GetName())
			return nil
		}
		var requests []reconcile.Request
		for i := range volumes.Items {
			if volumes.Items[i].Spec.AgentRef == obj.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: volumes.Items[i].Name},
				})
			}
		}
		return requests
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarVolumeState{}).
		Watches(
			&pillarcsiv1alpha1.PillarAgent{},
			handler.EnqueueRequestsFromMapFunc(mapAgentToVolumes),
		).
		Named("pillarvolumestate").
		Complete(r)
}
