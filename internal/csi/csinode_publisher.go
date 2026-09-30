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

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// NodeAnnotationPatcher patches annotations on a CSINode object.
// The interface is narrow so that tests can inject a fake without standing up
// a full API server.
type NodeAnnotationPatcher interface {
	// PatchAnnotations merges the given key-value pairs into
	// CSINode.metadata.annotations via a JSON merge patch.  nodeName is the
	// Kubernetes node name (== CSINode.metadata.name).
	PatchAnnotations(ctx context.Context, nodeName string, annotations map[string]string) error
}

// KubeCSINodePatcher implements NodeAnnotationPatcher via the Kubernetes API.
type KubeCSINodePatcher struct {
	client kubernetes.Interface
}

// NewKubeCSINodePatcher constructs a KubeCSINodePatcher using the supplied
// client.
func NewKubeCSINodePatcher(client kubernetes.Interface) *KubeCSINodePatcher {
	return &KubeCSINodePatcher{client: client}
}

// PatchAnnotations applies a JSON merge patch (RFC 7386) to the named CSINode
// object, adding or updating the given annotation key-value pairs.
//
// The patch body is an untyped document that contains only
// metadata.annotations.  It is deliberately NOT built by marshaling a
// storagev1.CSINode: CSINode.Spec and CSINodeSpec.Drivers lack omitempty, so
// the marshaled body would carry {"spec":{"drivers":null},"status":{}},
// and under a merge patch "drivers":null deletes the entire spec.drivers
// list — wiping every CSI driver registration kubelet wrote on the node
// (issue #128).  Keeping the body untyped guarantees no other CSINode field
// can ever be sent.
//
// Merge semantics preserve pre-existing annotations and all other fields of
// the object, and the patch is idempotent: if the annotation already carries
// the expected value the API server accepts the no-op patch without error.
//
// Returns a wrapped error if the patch fails.  Callers should log the error
// and retry; the controller will return FailedPrecondition until the
// annotation is present (RFC §5.2).
func (p *KubeCSINodePatcher) PatchAnnotations(
	ctx context.Context,
	nodeName string,
	annotations map[string]string,
) error {
	// Untyped body so that exactly one field — metadata.annotations — is
	// present in the patch.  See the doc comment above.
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal CSINode annotation patch for node %q: %w", nodeName, err)
	}

	_, err = p.client.StorageV1().CSINodes().Patch(
		ctx,
		nodeName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{FieldManager: "pillar-node"},
	)
	if err != nil {
		return fmt.Errorf("patch CSINode %q annotations: %w", nodeName, err)
	}
	return nil
}

// PublishNodeIdentity reads the host NQN from the standard system file and
// writes it as the pillar-csi.bhyoo.com/nvmeof-host-nqn annotation on the
// node's CSINode object.  When iscsiInitiatorIQN is non-empty (the iSCSI
// handler is enabled on this node) the same merge patch also writes it as
// the pillar-csi.bhyoo.com/iscsi-initiator-iqn annotation.
//
// This function is called during pillar-node startup so that the controller
// plugin can look up the initiator identities by Kubernetes node name when
// processing ControllerPublishVolume requests (RFC §5.2 node-side publisher
// contract).
//
// Preconditions:
//   - The node plugin ServiceAccount must have get+update+patch permissions on
//     storage.k8s.io/csinodes.
//   - The CSINode object must already exist (kubelet creates it during driver
//     registration; there is a short window at first boot where it may be
//     absent — callers should retry).
//
// Returns a NotFound-wrapped error when the CSINode does not yet exist so that
// callers can distinguish "CSINode not yet created" from other errors.
func PublishNodeIdentity(ctx context.Context, patcher NodeAnnotationPatcher, nodeName, iscsiInitiatorIQN string) error {
	return publishNodeIdentity(ctx, patcher, nodeName, hostNQNFile, iscsiInitiatorIQN)
}

// publishNodeIdentity is PublishNodeIdentity with the host NQN file path
// injectable for tests.
func publishNodeIdentity(
	ctx context.Context,
	patcher NodeAnnotationPatcher,
	nodeName, nqnFile, iscsiInitiatorIQN string,
) error {
	nqn, err := readOrGenerateHostNQN(nqnFile)
	if err != nil {
		return fmt.Errorf("PublishNodeIdentity: read host NQN: %w", err)
	}

	annotations := map[string]string{
		AnnotationNVMeOFHostNQN: nqn,
	}
	if iscsiInitiatorIQN != "" {
		annotations[AnnotationISCSIInitiatorIQN] = iscsiInitiatorIQN
	}
	patchErr := patcher.PatchAnnotations(ctx, nodeName, annotations)
	if patchErr != nil {
		if errors.IsNotFound(patchErr) {
			return fmt.Errorf(
				"PublishNodeIdentity: CSINode %q not found "+
					"(kubelet may not have registered the driver yet): %w",
				nodeName, patchErr)
		}
		return fmt.Errorf("PublishNodeIdentity: patch CSINode %q: %w", nodeName, patchErr)
	}
	return nil
}
