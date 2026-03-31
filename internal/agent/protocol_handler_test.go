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
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func TestHandlerForProtocol_NVMeoFTCP(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, "")
	handler, err := srv.handlerForProtocol(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP)
	if err != nil {
		t.Fatalf("handlerForProtocol unexpected error: %v", err)
	}
	if _, ok := handler.(*NVMeoFTCPAgentHandler); !ok {
		t.Fatalf("handler type = %T, want *NVMeoFTCPAgentHandler", handler)
	}
}

func TestHandlerForProtocol_Unspecified(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, "")
	_, err := srv.handlerForProtocol(agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED)
	if err == nil {
		t.Fatal("expected error for unspecified protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestHandlerForProtocol_Unsupported(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, "")
	_, err := srv.handlerForProtocol(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI)
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", st.Code())
	}
}
