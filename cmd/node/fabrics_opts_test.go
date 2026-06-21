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

package main

import (
	"strings"
	"testing"
)

// TestBuildFabricsConnectOpts_IncludesHostNQNAndHostID locks down the option
// string format written to /dev/nvme-fabrics.  Both hostnqn= and hostid=
// must be present:
//
//   - hostnqn= because pillar-csi targets enforce ACLs by default
//     (attr_allow_any_host=0) and would reject the connect when the kernel
//     synthesizes a random host NQN on its behalf, surfacing EIO to the
//     userland write.
//   - hostid= because Linux ~6.x kernels parse-reject /dev/nvme-fabrics
//     writes that set hostnqn= without a matching hostid= UUID with EINVAL
//     before any TCP attempt; the matching UUID is what nvme-cli writes
//     and what pillar-csi's ReadHostID persists at /etc/nvme/hostid.
//
// A regression that drops either option re-enables every NodeStageVolume
// failure mode the PR #32 round-trips were chasing.  This regression test
// is comment-heavy because the failure modes (EIO, EINVAL) are kernel-side
// and not surfaced as Go errors anywhere a future contributor could see.
func TestBuildFabricsConnectOpts_IncludesHostNQNAndHostID(t *testing.T) {
	const (
		trAddr    = "10.0.0.7"
		trSvcID   = "4420"
		subsysNQN = "nqn.2026-01.io.pillar-csi:pvc-test"
		hostNQN   = "nqn.2014-08.org.nvmexpress:uuid:abc-host"
		hostID    = "11111111-2222-3333-4444-555555555555"
	)
	got := buildFabricsConnectOpts(trAddr, trSvcID, subsysNQN, hostNQN, hostID)

	for _, want := range []string{
		"transport=tcp",
		"traddr=" + trAddr,
		"trsvcid=" + trSvcID,
		"nqn=" + subsysNQN,
		"hostnqn=" + hostNQN,
		"hostid=" + hostID,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("option string missing %q\nfull string: %q", want, got)
		}
	}
}

// TestBuildFabricsConnectOpts_NoTrailingNewline guards the contract that
// the caller (nvmeConnect) appends the framing newline itself via
// fmt.Fprintf "%s\n".  The kernel nvmf_dev_write parser tolerates a
// trailing newline but treats it as part of the last value when the
// builder emits one on its own, producing hostid=<uuid>\n which the UUID
// parser then rejects.
func TestBuildFabricsConnectOpts_NoTrailingNewline(t *testing.T) {
	got := buildFabricsConnectOpts("a", "1", "n", "h", "i")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("option string must not include CR/LF; got %q", got)
	}
}
