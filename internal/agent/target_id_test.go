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
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func TestVolumeTargetID_NVMeoFTCP(t *testing.T) {
	t.Parallel()

	got, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, "tank/pvc-abc")
	if err != nil {
		t.Fatalf("volumeTargetID unexpected error: %v", err)
	}

	const want = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"
	if got != want {
		t.Fatalf("volumeTargetID = %q, want %q", got, want)
	}
}

func TestVolumeTargetID_ISCSI(t *testing.T) {
	t.Parallel()

	got, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, "tank/pvc-abc")
	if err != nil {
		t.Fatalf("volumeTargetID unexpected error: %v", err)
	}

	const want = "iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"
	if got != want {
		t.Fatalf("volumeTargetID = %q, want %q", got, want)
	}
}

func TestVolumeTargetID_UsesSharedVolumeIDSuffixAcrossBlockProtocols(t *testing.T) {
	t.Parallel()

	const (
		volumeID   = "pool-alpha/volume.with-mixed_chars-01"
		wantSuffix = "pool-alpha.volume.with-mixed_chars-01"
	)

	for _, protocol := range []agentv1.ProtocolType{
		agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	} {
		got, err := volumeTargetID(protocol, volumeID)
		if err != nil {
			t.Fatalf("volumeTargetID(%s) unexpected error: %v", protocol.String(), err)
		}
		if !strings.HasSuffix(got, wantSuffix) {
			t.Fatalf("volumeTargetID(%s) = %q, want suffix %q", protocol.String(), got, wantSuffix)
		}
	}
}

func TestVolumeTargetID_FileProtocolsNeedHandlerSpecificContext(t *testing.T) {
	t.Parallel()

	for _, protocol := range []agentv1.ProtocolType{
		agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
		agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
	} {
		_, err := volumeTargetID(protocol, "tank/pvc-abc")
		if err == nil {
			t.Fatalf("volumeTargetID(%s) error = nil, want error", protocol.String())
		}

		st, _ := status.FromError(err)
		if st.Code() != codes.Unimplemented {
			t.Fatalf("volumeTargetID(%s) code = %v, want Unimplemented", protocol.String(), st.Code())
		}
	}
}

func TestVolumeTargetID_UnspecifiedProtocol(t *testing.T) {
	t.Parallel()

	_, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED, "tank/pvc-abc")
	if err == nil {
		t.Fatal("volumeTargetID error = nil, want error")
	}

	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("volumeTargetID code = %v, want InvalidArgument", st.Code())
	}
}
