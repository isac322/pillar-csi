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

package agent

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// iqnPrefix is the fixed IQN prefix used for iSCSI target names derived from a
// volume ID.
const iqnPrefix = "iqn.2026-01.com.bhyoo.pillar-csi:"

// volumeTargetID derives a protocol-specific target identifier from a volume
// ID when the target naming scheme is fully determined by protocol + volume.
//
// Formats:
//   - NVMe-oF TCP: nqn.2026-01.com.bhyoo.pillar-csi:<pool>.<name>
//   - iSCSI:       iqn.2026-01.com.bhyoo.pillar-csi:<pool>.<name>
//
// File protocols need additional export context beyond the volume ID alone, so
// their target identifiers are produced by protocol-specific handlers instead
// of this helper.
func volumeTargetID(protocol agentv1.ProtocolType, volumeID string) (string, error) {
	targetSuffix := strings.ReplaceAll(volumeID, "/", ".")

	switch protocol {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		return nqnPrefix + targetSuffix, nil
	case agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI:
		return iqnPrefix + targetSuffix, nil
	case agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED:
		return "", status.Errorf(codes.InvalidArgument, "volumeTargetID: protocol_type is required")
	case agentv1.ProtocolType_PROTOCOL_TYPE_NFS, agentv1.ProtocolType_PROTOCOL_TYPE_SMB:
		return "", status.Errorf(codes.Unimplemented,
			"volumeTargetID: protocol %s requires handler-specific target ID construction", protocol.String())
	default:
		return "", status.Errorf(codes.Unimplemented,
			"volumeTargetID: protocol %s target ID derivation is not implemented", protocol.String())
	}
}
