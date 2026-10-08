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

// NVMe/TCP header and data digests.
//
// The digests are negotiated by the host: it requests them in its ICReq
// (the hdr_digest / data_digest fabrics options) and the Linux nvmet-tcp
// target enables whatever the host requested, so the storage node needs no
// configuration.  Unlike the other nvmeofTcp tunables they are not recorded
// in the immutable VolumeContext at CreateVolume: ControllerPublishVolume
// reads them from the volume's current PillarProtocol and hands them to
// NodeStageVolume in the PublishContext, so enabling them on a protocol
// reaches every volume of it, including volumes provisioned before, at
// their next publish.
//
// A volume does not record the PillarProtocol it was provisioned through, so
// the protocol is found the way CreateVolume found it: through the
// StorageClass of the volume's PersistentVolume, either a generated class
// naming its PillarStorageClass (whose spec.protocolRef is followed) or a
// hand-written class naming the protocol directly.  Every PersistentVolume
// carries its StorageClass name, including volumes provisioned before any
// claim reference was recorded.

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// The publish path reads the PersistentVolume, its StorageClass and the
// PillarStorageClass / PillarProtocol the class names.
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses;pillarprotocols,verbs=get;list;watch

// nvmeofDigestPublishContext returns the PublishContext entries carrying the
// digest settings of the PillarProtocol that volume pvName (CSI handle
// volumeID) was provisioned through, or nil when no digest is enabled.
//
// When the protocol cannot be determined (the PersistentVolume, its
// StorageClass, the PillarStorageClass or the PillarProtocol is gone, or the
// class is not a pillar-csi class) the volume is published without digests
// and the reason is logged: the operator's intent is unknown, and refusing
// the publish would make an otherwise healthy volume unusable.  Any other
// API error is returned as Internal so the attacher retries instead of
// publishing without a digest the protocol may request.
func (s *ControllerServer) nvmeofDigestPublishContext(
	ctx context.Context,
	pvName, volumeID string,
) (map[string]string, error) {
	protocol, reason, err := s.publishedProtocol(ctx, pvName, volumeID)
	if err != nil {
		return nil, err
	}
	if protocol == nil {
		logf.FromContext(ctx).Info("Publishing without NVMe/TCP digests: the volume's PillarProtocol is unknown",
			"volume", volumeID, "reason", reason)
		return nil, nil //nolint:nilnil // a nil map is the meaningful answer: no digest entries
	}
	cfg := protocol.Spec.Protocol.NVMeOFTCP
	if cfg == nil {
		logf.FromContext(ctx).Info("Publishing without NVMe/TCP digests: the volume's PillarProtocol is not nvmeofTcp",
			"volume", volumeID, "protocol", protocol.Name, "kind", protocol.Spec.Protocol.Kind())
		return nil, nil //nolint:nilnil // a nil map is the meaningful answer: no digest entries
	}
	return nvmeofDigestContext(cfg), nil
}

// nvmeofDigestContext maps the digest flags of cfg to PublishContext
// entries; it returns nil when neither digest is enabled.
func nvmeofDigestContext(cfg *v1alpha1.NVMeOFTCPConfig) map[string]string {
	if !cfg.HdrDigest && !cfg.DataDigest {
		return nil
	}
	out := make(map[string]string, 2)
	if cfg.HdrDigest {
		out[PublishContextKeyNVMeOFHdrDigest] = paramValueTrue
	}
	if cfg.DataDigest {
		out[PublishContextKeyNVMeOFDataDigest] = paramValueTrue
	}
	return out
}

// publishedProtocol follows PersistentVolume -> StorageClass ->
// [PillarStorageClass ->] PillarProtocol for volume pvName.  A missing link
// returns a nil protocol and the reason; any other read error is returned as
// an Internal status.
func (s *ControllerServer) publishedProtocol(
	ctx context.Context,
	pvName, volumeID string,
) (*v1alpha1.PillarProtocol, string, error) {
	pv := &corev1.PersistentVolume{}
	found, err := getOptional(ctx, s.uncachedReader(), pvName, pv, "PersistentVolume")
	if !found || err != nil {
		return nil, fmt.Sprintf("PersistentVolume %q not found", pvName), err
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle != volumeID {
		return nil, fmt.Sprintf("PersistentVolume %q does not carry volume handle %q", pvName, volumeID), nil
	}
	scName := pv.Spec.StorageClassName
	if scName == "" {
		return nil, fmt.Sprintf("PersistentVolume %q names no StorageClass", pvName), nil
	}
	sc := &storagev1.StorageClass{}
	found, err = getOptional(ctx, s.k8sClient, scName, sc, "StorageClass")
	if !found || err != nil {
		return nil, fmt.Sprintf("StorageClass %q not found", scName), err
	}
	if sc.Provisioner != pv.Spec.CSI.Driver {
		return nil, fmt.Sprintf("StorageClass %q provisions with %q, not %q",
			scName, sc.Provisioner, pv.Spec.CSI.Driver), nil
	}

	protocolName := sc.Parameters[paramProtocolRef]
	if bindingName := sc.Parameters[paramBinding]; bindingName != "" {
		binding := &v1alpha1.PillarStorageClass{}
		found, err = getOptional(ctx, s.k8sClient, bindingName, binding, "PillarStorageClass")
		if !found || err != nil {
			return nil, fmt.Sprintf("PillarStorageClass %q (StorageClass %q) not found", bindingName, scName), err
		}
		protocolName = binding.Spec.ProtocolRef
	}
	if protocolName == "" {
		return nil, fmt.Sprintf("StorageClass %q names no PillarProtocol", scName), nil
	}
	protocol := &v1alpha1.PillarProtocol{}
	found, err = getOptional(ctx, s.k8sClient, protocolName, protocol, "PillarProtocol")
	if !found || err != nil {
		return nil, fmt.Sprintf("PillarProtocol %q not found", protocolName), err
	}
	return protocol, "", nil
}

// getOptional reads the cluster-scoped object name into obj.  NotFound
// reports found=false without an error; any other error is an Internal
// status.
func getOptional(ctx context.Context, r client.Reader, name string, obj client.Object, kind string) (bool, error) {
	err := r.Get(ctx, types.NamespacedName{Name: name}, obj)
	if err == nil {
		return true, nil
	}
	if k8serrors.IsNotFound(err) {
		return false, nil
	}
	return false, status.Errorf(codes.Internal, "get %s %q: %v", kind, name, err)
}
