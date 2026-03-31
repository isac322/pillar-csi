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
	"time"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func TestLockTarget_SerializesSameProtocolAndTarget(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, "")
	unlockFirst := srv.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, "shared-target")

	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlockSecond := srv.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, "shared-target")
		close(acquired)
		unlockSecond()
		close(done)
	}()

	select {
	case <-acquired:
		t.Fatal("same protocol/target lock acquired before first lock was released")
	case <-time.After(25 * time.Millisecond):
	}

	unlockFirst()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("same protocol/target lock was not acquired after release")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("same protocol/target lock holder did not finish")
	}
}

func TestLockTarget_DistinguishesProtocolsForSameTargetID(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, "")
	unlockFirst := srv.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, "shared-target")
	defer unlockFirst()

	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlockSecond := srv.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, "shared-target")
		close(acquired)
		unlockSecond()
		close(done)
	}()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("different protocol lock blocked on the same target ID")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("different protocol lock holder did not finish")
	}
}

func TestLockTarget_SerializesDerivedISCSITargetID(t *testing.T) {
	t.Parallel()

	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, "tank/pvc-abc")
	if err != nil {
		t.Fatalf("volumeTargetID unexpected error: %v", err)
	}

	srv := NewServer(nil, "")
	unlockFirst := srv.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, targetID)

	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlockSecond := srv.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, targetID)
		close(acquired)
		unlockSecond()
		close(done)
	}()

	select {
	case <-acquired:
		t.Fatal("derived iSCSI target lock acquired before first lock was released")
	case <-time.After(25 * time.Millisecond):
	}

	unlockFirst()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("derived iSCSI target lock was not acquired after release")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("derived iSCSI target lock holder did not finish")
	}
}
