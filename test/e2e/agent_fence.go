package e2e

import (
	"crypto/rand"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// agentFenceRunNonce makes lifecycle UIDs unique to this test process.  An
// agent's fencing marks outlive a run, so a long-lived agent (the Helm-deployed
// pillar-agent pod) rejects a UID that an earlier run already ended with
// DeleteVolume.
var agentFenceRunNonce = rand.Text()

// agentLifecycleFence returns the fencing token for the volume lifecycle named
// by key, standing in for the PillarVolumeState UID the controller would send.
// Pass the volume ID as key and reuse the token for every operation on that
// create; a test that re-creates the same volume ID after deleting it must use
// a different key.  The generation stays at 1 because these direct callers
// never supersede their own operations.
func agentLifecycleFence(key string) *agentv1.FencingToken {
	return &agentv1.FencingToken{VolumeUid: key + "@" + agentFenceRunNonce, Generation: 1}
}
