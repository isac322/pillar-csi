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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Export restore gate.
//
// Every subsystem of this storage node shares a few NVMe/TCP ports, and nvmet
// starts listening on a port as soon as the first subsystem is linked to it.
// After a storage-node reboot (or nvmet reload) configfs is empty and hosts
// keep retrying their connects, which the kernel refuses while nothing
// listens.  If one export were re-created and linked before the others, the
// port would listen and nvmet would answer the other hosts' connects with
// "Connect Invalid Data Parameter" and the do-not-retry bit, and Linux hosts
// delete the controller for good (issue #92).
//
// The agent alone knows when it (re)started, and it cannot know which exports
// are due, so it starts gated: it links no subsystem on behalf of a single
// volume until the controller sent the complete export state of this agent in
// one ReconcileState (complete=true).  That request prepares every export
// before linking any, so a host sees either a refused connection or its fully
// configured subsystem.  Revoke-only RPCs (UnexportVolume, DenyInitiator) are
// not gated: they never make a port listen.  The gate stays closed on an agent
// restart without reboot too, where it costs only a short delay for new
// exports and also covers a restart that interrupted an earlier restore.

// exportRestorePendingMsg prefixes the UNAVAILABLE status of gated RPCs.
const exportRestorePendingMsg = "export restore pending"

// checkExportRestoreDone rejects an export-creating RPC while the export
// restore is pending.
func (s *Server) checkExportRestoreDone(rpc string) error {
	if s.exportRestorePending.Load() {
		return status.Errorf(codes.Unavailable,
			"%s: %s: awaiting a complete ReconcileState from the controller before exporting",
			exportRestorePendingMsg, rpc)
	}
	return nil
}
