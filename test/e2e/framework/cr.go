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

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// CR CRUD operations
// ─────────────────────────────────────────────────────────────────────────────

// Apply creates obj in the cluster if it does not yet exist, or updates it
// if a resource with the same name already exists.  The object's Name (and
// Namespace, when applicable) must be populated before calling Apply.
//
// Apply attempts a Create first; on AlreadyExists it retrieves the current
// resource version and issues an Update.  Status subresource fields are not
// touched by this path — use the client's Status().Update() directly when
// needed.
func Apply(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Create(ctx, obj); err == nil {
		return nil
	} else if !errors.IsAlreadyExists(err) {
		return fmt.Errorf("framework Apply: create %T %q: %w", obj, obj.GetName(), err)
	}

	// Object already exists — refresh resource-version, then update.
	existing := obj.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
		return fmt.Errorf("framework Apply: get existing %T %q: %w", obj, obj.GetName(), err)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	if err := c.Update(ctx, obj); err != nil {
		return fmt.Errorf("framework Apply: update %T %q: %w", obj, obj.GetName(), err)
	}
	return nil
}

// Delete removes obj from the cluster.  It returns nil if the object is
// already absent (idempotent).  Pass client.GracePeriodSeconds(0) or other
// DeleteOptions to customise the deletion behaviour.
func Delete(ctx context.Context, c client.Client, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.Delete(ctx, obj, opts...); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("framework Delete: %T %q: %w", obj, obj.GetName(), err)
	}
	return nil
}

// EnsureGone deletes obj (if it still exists) and then blocks until it is
// fully removed from the API server.  If the object is already absent the
// function returns immediately.
//
// The timeout applies to the wait phase only; the initial Delete call is
// not bounded.  Pass 0 to use DefaultWaitTimeout.
func EnsureGone(ctx context.Context, c client.Client, obj client.Object, timeout time.Duration) error {
	if err := Delete(ctx, c, obj, client.GracePeriodSeconds(0)); err != nil {
		return err
	}
	return WaitForDeletion(ctx, c, obj, timeout)
}

// ─────────────────────────────────────────────────────────────────────────────
// Type-specific builder helpers
// ─────────────────────────────────────────────────────────────────────────────

// NewPillarTarget returns a PillarTarget with the given name and spec, ready
// to be passed to Apply.
func NewPillarTarget(name string, spec v1alpha1.PillarTargetSpec) *v1alpha1.PillarTarget {
	return &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

// NewExternalPillarTarget is a convenience builder for a PillarTarget that
// reaches an agent outside the Kubernetes cluster at address:port.
//
// Example:
//
//	pt := framework.NewExternalPillarTarget("agent-1", "192.168.1.10", 9500)
func NewExternalPillarTarget(name, address string, port int32) *v1alpha1.PillarTarget {
	return NewPillarTarget(name, v1alpha1.PillarTargetSpec{
		External: &v1alpha1.ExternalSpec{
			Address: address,
			Port:    port,
		},
	})
}

// NewNodeRefPillarTarget is a convenience builder for a PillarTarget that
// resolves its agent address from a Kubernetes Node's InternalIP.
//
// Pass a non-nil port to override the default agent gRPC port (9500).
//
// Example:
//
//	pt := framework.NewNodeRefPillarTarget("worker-0-target", "worker-0", nil)
func NewNodeRefPillarTarget(name, nodeName string, port *int32) *v1alpha1.PillarTarget {
	return NewPillarTarget(name, v1alpha1.PillarTargetSpec{
		NodeRef: &v1alpha1.NodeRefSpec{
			Name: nodeName,
			Port: port,
		},
	})
}

// NewPillarPool returns a PillarPool with the given name and spec, ready to
// be passed to Apply.
func NewPillarPool(name string, spec v1alpha1.PillarPoolSpec) *v1alpha1.PillarPool {
	return &v1alpha1.PillarPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

// NewZFSZvolPool is a convenience builder for a PillarPool backed by ZFS zvols.
//
// Example:
//
//	pool := framework.NewZFSZvolPool("pool-1", "agent-1", "tank")
func NewZFSZvolPool(name, targetRef, zfsPool string) *v1alpha1.PillarPool {
	return NewPillarPool(name, v1alpha1.PillarPoolSpec{
		TargetRef: targetRef,
		Backend: v1alpha1.BackendSpec{
			Type: v1alpha1.BackendTypeZFSZvol,
			ZFS:  &v1alpha1.ZFSBackendConfig{Pool: zfsPool},
		},
	})
}

// NewZFSDatasetPool is a convenience builder for a PillarPool backed by ZFS
// datasets.
//
// Example:
//
//	pool := framework.NewZFSDatasetPool("ds-pool-1", "agent-1", "tank")
func NewZFSDatasetPool(name, targetRef, zfsPool string) *v1alpha1.PillarPool {
	return NewPillarPool(name, v1alpha1.PillarPoolSpec{
		TargetRef: targetRef,
		Backend: v1alpha1.BackendSpec{
			Type: v1alpha1.BackendTypeZFSDataset,
			ZFS:  &v1alpha1.ZFSBackendConfig{Pool: zfsPool},
		},
	})
}

// NewLVMLinearPool is a convenience builder for a PillarPool backed by LVM
// linear logical volumes.  Volumes are fully allocated from the Volume Group
// immediately on creation (lvcreate -L).
//
// Example:
//
//	pool := framework.NewLVMLinearPool("lvm-pool-1", "agent-1", "data-vg")
func NewLVMLinearPool(name, targetRef, vgName string) *v1alpha1.PillarPool {
	return NewPillarPool(name, v1alpha1.PillarPoolSpec{
		TargetRef: targetRef,
		Backend: v1alpha1.BackendSpec{
			Type: v1alpha1.BackendTypeLVMLV,
			LVM: &v1alpha1.LVMBackendConfig{
				VolumeGroup:      vgName,
				ProvisioningMode: v1alpha1.LVMProvisioningModeLinear,
			},
		},
	})
}

// NewLVMThinPool is a convenience builder for a PillarPool backed by LVM
// thin-provisioned logical volumes.  Volumes are created inside the named thin
// pool LV (lvcreate -V --thinpool), enabling over-provisioning.
//
// Example:
//
//	pool := framework.NewLVMThinPool("lvm-thin-1", "agent-1", "data-vg", "thin-pool-0")
func NewLVMThinPool(name, targetRef, vgName, thinPoolName string) *v1alpha1.PillarPool {
	return NewPillarPool(name, v1alpha1.PillarPoolSpec{
		TargetRef: targetRef,
		Backend: v1alpha1.BackendSpec{
			Type: v1alpha1.BackendTypeLVMLV,
			LVM: &v1alpha1.LVMBackendConfig{
				VolumeGroup:      vgName,
				ThinPool:         thinPoolName,
				ProvisioningMode: v1alpha1.LVMProvisioningModeThin,
			},
		},
	})
}

// NewPillarProtocol returns a PillarProtocol with the given name and spec,
// ready to be passed to Apply.
func NewPillarProtocol(name string, spec v1alpha1.PillarProtocolSpec) *v1alpha1.PillarProtocol {
	return &v1alpha1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

// NewNVMeOFTCPProtocol is a convenience builder for a PillarProtocol that
// uses NVMe-oF over TCP with default settings.
//
// Example:
//
//	proto := framework.NewNVMeOFTCPProtocol("nvme-tcp-1")
func NewNVMeOFTCPProtocol(name string) *v1alpha1.PillarProtocol {
	return NewPillarProtocol(name, v1alpha1.PillarProtocolSpec{
		Type:      v1alpha1.ProtocolTypeNVMeOFTCP,
		NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{},
	})
}

// NewISCSIProtocol is a convenience builder for a PillarProtocol that uses
// iSCSI with default settings.
//
// Example:
//
//	proto := framework.NewISCSIProtocol("iscsi-1")
func NewISCSIProtocol(name string) *v1alpha1.PillarProtocol {
	return NewPillarProtocol(name, v1alpha1.PillarProtocolSpec{
		Type:  v1alpha1.ProtocolTypeISCSI,
		ISCSI: &v1alpha1.ISCSIConfig{},
	})
}

// NewPillarBinding returns a PillarBinding with the given name and spec,
// ready to be passed to Apply.
func NewPillarBinding(name string, spec v1alpha1.PillarBindingSpec) *v1alpha1.PillarBinding {
	return &v1alpha1.PillarBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

// NewSimplePillarBinding is a convenience builder for a PillarBinding that
// wires a pool to a protocol and generates a StorageClass with the binding's
// name.
//
// Example:
//
//	binding := framework.NewSimplePillarBinding("binding-1", "pool-1", "nvme-tcp-1")
func NewSimplePillarBinding(name, poolRef, protocolRef string) *v1alpha1.PillarBinding {
	return NewPillarBinding(name, v1alpha1.PillarBindingSpec{
		PoolRef:     poolRef,
		ProtocolRef: protocolRef,
	})
}
