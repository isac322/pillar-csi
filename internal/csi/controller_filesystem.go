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
	"crypto/rand"
	"encoding/hex"
	"strings"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func filesystemMultiNodeMode(mode csipb.VolumeCapability_AccessMode_Mode) bool {
	switch mode {
	case csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csipb.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return true
	default:
		return false
	}
}

func filesystemCapabilitiesMultiNode(caps []*csipb.VolumeCapability) bool {
	for _, capability := range caps {
		if filesystemMultiNodeMode(capability.GetAccessMode().GetMode()) {
			return true
		}
	}
	return false
}

func localOnlyFilesystem(pvs *v1alpha1.PillarVolumeState) bool {
	return pvs != nil && pvs.Spec.FilesystemAdoption != nil && pvs.Spec.Resolved != nil &&
		pvs.Spec.Resolved.LocalAttach
}

// Local file topology describes only the node-local route. Its actual keys
// must be present in a requested segment, while unrelated requested keys
// (such as protocol-specific block keys) remain unconstrained. Topologies
// without the file node key retain the original CSI subset direction.

func validateFilesystemTopology(requirements *csipb.TopologyRequirement, accessible []*csipb.Topology) error {
	requisite := requirements.GetRequisite()
	if len(requisite) == 0 || len(accessible) == 0 {
		return nil
	}
	localFilesystem := filesystemTopologyHasLocalNode(accessible)
	for _, requested := range requisite {
		for _, actual := range accessible {
			if filesystemTopologyMatches(requested, actual, localFilesystem) {
				return nil
			}
		}
	}
	return status.Errorf(codes.ResourceExhausted, "adopted filesystem storage node is outside requisite topology")
}

func filesystemTopologyHasLocalNode(accessible []*csipb.Topology) bool {
	for _, actual := range accessible {
		if _, ok := actual.GetSegments()[FileTopologyNodeKey]; ok {
			return true
		}
	}
	return false
}

func filesystemTopologyMatches(requested, actual *csipb.Topology, localFilesystem bool) bool {
	required, available := requested.GetSegments(), actual.GetSegments()
	if localFilesystem {
		required, available = available, required
	}
	for key, value := range required {
		if available[key] != value {
			return false
		}
	}
	return true
}

// Single-node adoption is a direct host mount, not network block attachment.
// Only a real in-cluster agent's NodeRef can identify its accessible node.
func (s *ControllerServer) filesystemAccessibleTopology(
	ctx context.Context, pvs *v1alpha1.PillarVolumeState, caps []*csipb.VolumeCapability,
) ([]*csipb.Topology, error) {
	if pvs.Spec.FilesystemAdoption == nil {
		return nil, nil
	}
	for _, capability := range caps {
		if filesystemMultiNodeMode(capability.GetAccessMode().GetMode()) {
			return nil, nil
		}
	}
	agent, err := s.getReadyAgent(ctx, pvs.Spec.AgentRef)
	if err != nil {
		return nil, err
	}
	if agent.Spec.External != nil || agent.Spec.NodeRef == nil || agent.Spec.NodeRef.Name == "" {
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"local filesystem adoption requires an agent with a Kubernetes NodeRef",
		)
	}
	return []*csipb.Topology{{Segments: map[string]string{FileTopologyNodeKey: agent.Spec.NodeRef.Name}}}, nil
}

func recordedFilesystemAdoption(recorded *v1alpha1.PillarVolumeState) *v1alpha1.FilesystemAdoption {
	if recorded == nil {
		return nil
	}
	return recorded.Spec.FilesystemAdoption
}

func recordedFilesystemCapacity(recorded *v1alpha1.PillarVolumeState) int64 {
	if recordedFilesystemAdoption(recorded) == nil {
		return 0
	}
	return recorded.Spec.CapacityBytes
}

func recordedBackendParams(recorded *v1alpha1.PillarVolumeState) *agentv1.BackendParams {
	if recordedFilesystemAdoption(recorded) == nil || recorded.Spec.Resolved == nil {
		return nil
	}
	return backendParamsFromResolved(recorded.Spec.Resolved.Backend)
}

func invalidFilesystemDescriptor(err error) error {
	return status.Errorf(codes.FailedPrecondition, "invalid filesystem adoption descriptor: %v", err)
}

func completedFilesystemAdoption(pvs *v1alpha1.PillarVolumeState) bool {
	if pvs.Spec.FilesystemAdoption == nil || !pvs.Status.ImportAcquired {
		return false
	}
	switch pvs.Status.Phase {
	case v1alpha1.PillarVolumeStatePhaseReady,
		v1alpha1.PillarVolumeStatePhaseControllerPublished,
		v1alpha1.PillarVolumeStatePhaseNodeStagePartial,
		v1alpha1.PillarVolumeStatePhaseNodeStaged,
		v1alpha1.PillarVolumeStatePhaseNodePublished:
		return true
	default:
		return false
	}
}

func filesystemVolumeIDBase(spec *v1alpha1.PillarVolumeStateSpec) string {
	return strings.Join([]string{spec.AgentRef, spec.ProtocolType, spec.BackendType, spec.AgentVolumeID}, "/")
}

// Backend routing stays native-resource based; the public handle additionally
// identifies this lifecycle so a retired handle cannot address a later owner.
func newFilesystemVolumeID(base string) (string, error) {
	var nonce [16]byte
	_, err := rand.Read(nonce[:])
	if err != nil {
		return "", status.Errorf(codes.Internal, "generate filesystem lifecycle identity: %v", err)
	}
	return base + "." + hex.EncodeToString(nonce[:]), nil
}

func isFilesystemVolumeID(pvs *v1alpha1.PillarVolumeState) bool {
	if pvs == nil || pvs.Spec.FilesystemAdoption == nil {
		return false
	}
	spec := &pvs.Spec
	if spec.AgentRef == "" || spec.ProtocolType == "" || spec.BackendType == "" || spec.AgentVolumeID == "" {
		return false
	}
	suffix, ok := strings.CutPrefix(spec.VolumeID, filesystemVolumeIDBase(spec))
	if !ok || len(suffix) != 33 || suffix[0] != '.' {
		return false
	}
	return validFilesystemLifecycleNonce(suffix[1:])
}

func validFilesystemLifecycleNonce(nonce string) bool {
	if len(nonce) != 32 {
		return false
	}
	for _, char := range nonce {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
