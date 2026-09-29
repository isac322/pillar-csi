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

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/nvmeofnqn"
)

// volumeTargetID derives a protocol-specific target identifier from a volume
// ID.  Only NVMe-oF TCP is implemented:
//
//	nqn.2026-01.com.bhyoo.pillar-csi:<pool>.<name>
//
// Every other protocol is rejected as unimplemented.
func volumeTargetID(protocol agentv1.ProtocolType, volumeID string) (string, error) {
	switch protocol {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		return nvmeofnqn.Prefix + strings.ReplaceAll(volumeID, "/", "."), nil
	case agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED:
		return "", status.Errorf(codes.InvalidArgument, "volumeTargetID: protocol_type is required")
	default:
		return "", status.Errorf(codes.Unimplemented,
			"volumeTargetID: protocol %s target ID derivation is not implemented", protocol.String())
	}
}
