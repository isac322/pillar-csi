package e2e

// Sub-AC 3.3 — TC ID uniqueness guard.
//
// This file provides the runtime primitive validateTCNodeLabelUniqueness that
// is called by buildDefaultProfile() as the last step before returning the
// resolved 437-case set.  A companion Ginkgo BeforeSuite node in
// tc_id_uniqueness_guard_test.go re-runs the same check explicitly at suite
// startup (before any It node executes) so that accidental collisions added
// by future contributors surface immediately with a human-readable error that
// identifies the colliding IDs.
//
// Why DocID-only uniqueness on top of the existing composite traceKey check?
//
//   The composite key used by the older check is:
//
//     GroupKey | DocID | TestName
//
//   Two cases with the same DocID but different TestName pass the composite
//   check but would produce identical [TC-<DocID>] It-node labels.  Ginkgo
//   would then register two specs with the same full name; one would silently
//   shadow the other when running with --focus or -run=TC-<ID>.  The DocID-only
//   check eliminates this class of silent collision entirely.

import "fmt"

// validateTCNodeLabelUniqueness inspects the resolved profile slice and
// returns an error if any two cases share the same Ginkgo node label
// ("[TC-<DocID>]").  The error message names both colliding TC IDs and their
// TestName symbols so the caller can trace the collision back to the spec
// document without any additional tooling.
//
// This function is deterministic and allocation-bounded: it allocates exactly
// one map entry per case and exits on the first collision found.
func validateTCNodeLabelUniqueness(cases []documentedCase) error {
	// seenLabel maps each node label to the first TestName that claimed it.
	// Capacity pre-allocation avoids incremental map grows for the common
	// 437-case profile.
	seenLabel := make(map[string]string, len(cases))

	for _, tc := range cases {
		label := tc.tcNodeLabel() // e.g. "[TC-E1.1]"
		if prev, exists := seenLabel[label]; exists {
			return fmt.Errorf(
				"TC node label collision: %s is claimed by both %q and %q — "+
					"each It-node label must be unique so that -run=TC-<ID> "+
					"selects exactly one spec",
				label, prev, tc.TestName,
			)
		}
		seenLabel[label] = tc.TestName
	}
	return nil
}
