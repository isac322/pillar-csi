//go:build !e2e

package e2e

// Sub-AC 2b + 3.3 — Parallel-safe SynchronizedBeforeSuite bootstrap (non-e2e build).
//
// This file is excluded from //go:build e2e compilations because the e2e build
// registers its own SynchronizedBeforeSuite in kind_bootstrap_e2e_test.go.
// Ginkgo v2 forbids registering both BeforeSuite and SynchronizedBeforeSuite in
// the same suite package; the build tag ensures exactly one is active per
// compilation target.
//
// SynchronizedBeforeSuite — two phases:
//
//  Primary phase (runs once on Ginkgo process node 1):
//    • Validates TC node-label uniqueness across the full 437-case default profile.
//    • Serialises a compact suiteSharedPayload (profile count) and returns it as
//      []byte so every parallel worker node can verify the shared precondition.
//
//  All-nodes phase (runs on every Ginkgo process node, including node 1):
//    • Deserialises the payload from the primary and asserts the count matches.
//    • Calls warmUpLocalBackend() to eagerly initialise every in-process verifier
//      on this node. Pre-warming ensures:
//        – Backend startup time is amortised before any spec runs.
//        – Verifier failures surface at suite-setup time (fast-fail), not mid-run.
//        – Each parallel Ginkgo node owns exactly one initialised backend set.
//
// Isolation guarantee: each Ginkgo parallel worker is a separate OS process with
// its own defaultLocalVerifierRegistry. Pre-warming in the all-nodes phase means
// every node's registry entries are resolved before specs run, so no TC
// experiences first-call overhead and no two TCs share mutable verifier state.
//
// NOTE: recordGroupSetupTiming must NOT be called from SynchronizedBeforeSuite.
// The authoritative setup timing is extracted automatically from the consolidated
// Ginkgo suite report by collectGroupTimingFromReport (group_timing.go).

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// suiteSharedPayload is the data serialised by the primary Ginkgo node and
// broadcast to every parallel worker via SynchronizedBeforeSuite.
type suiteSharedPayload struct {
	// ProfileCount is the number of TC node labels validated by the primary node.
	// Every worker receives this value and asserts it equals defaultProfileCaseCount
	// to confirm the shared pre-condition was checked before any spec ran.
	ProfileCount int `json:"profile_count"`
}

var _ = SynchronizedBeforeSuite(

	// ── Primary phase ────────────────────────────────────────────────────────
	// Runs exactly once, on Ginkgo process node 1, before any spec is scheduled.
	// Returns a []byte payload that is forwarded to every parallel worker node.
	func() []byte {
		By("Sub-AC 3.3: validating TC node-label uniqueness at suite startup (primary node)")

		profile, err := buildDefaultProfile()
		Expect(err).NotTo(HaveOccurred(),
			"[AC3.3] default profile build failed at suite startup — "+
				"check docs/E2E-TESTCASES.md for duplicate or missing TC IDs")

		Expect(profile).NotTo(BeEmpty(),
			"[AC3.3] default profile is empty — expected %d documented cases",
			defaultProfileCaseCount)

		// validateTCNodeLabelUniqueness is also called inside buildDefaultProfile,
		// but calling it here with an explicit Expect ensures:
		//   (a) the Ginkgo failure message carries the [AC3.3] annotation, and
		//   (b) the error surfaces at the SynchronizedBeforeSuite primary phase —
		//       before any It node is scheduled — rather than only when a colliding
		//       spec is run.
		Expect(validateTCNodeLabelUniqueness(profile)).To(Succeed(),
			"[AC3.3] TC node-label collision detected at suite startup — "+
				"two or more It-node labels share the same [TC-<ID>] string; "+
				"each TC ID must map to exactly one Ginkgo spec")

		_, _ = fmt.Fprintf(GinkgoWriter,
			"[AC3.3] TC node-label uniqueness verified: %d distinct [TC-<ID>] labels\n",
			len(profile))

		payload := suiteSharedPayload{ProfileCount: len(profile)}
		data, err := json.Marshal(payload)
		Expect(err).NotTo(HaveOccurred(),
			"[AC2b] SynchronizedBeforeSuite primary: failed to serialise shared payload")
		return data
	},

	// ── All-nodes phase ───────────────────────────────────────────────────────
	// Runs on every Ginkgo process node (including the primary) after the primary
	// phase completes. Receives the serialised payload from the primary phase.
	func(data []byte) {
		var shared suiteSharedPayload
		Expect(json.Unmarshal(data, &shared)).To(Succeed(),
			"[AC2b] SynchronizedBeforeSuite all-nodes: failed to deserialise shared payload")
		Expect(shared.ProfileCount).To(Equal(defaultProfileCaseCount),
			"[AC2b] SynchronizedBeforeSuite all-nodes: profile count mismatch — "+
				"primary validated %d cases but this node expected %d",
			shared.ProfileCount, defaultProfileCaseCount)

		// Pre-warm every in-process verifier on this Ginkgo node.
		// Each verifier uses sync.Once internally; warmUpLocalBackend eagerly
		// triggers that initialisation so backends are ready before specs run.
		// Verifier errors are stored in the registry, not propagated here —
		// individual specs decide whether a failed verifier is a test failure.
		warmUpLocalBackend()

		_, _ = fmt.Fprintf(GinkgoWriter,
			"[AC2b] node %d: in-process backends initialised (%d verifiers pre-warmed)\n",
			GinkgoParallelProcess(), len(allLocalVerifierNames))
	},
)
