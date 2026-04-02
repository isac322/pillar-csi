package e2e

// tc_e8_inprocess_test.go — Per-TC assertions for E8: mTLS transport.

import (
	. "github.com/onsi/gomega"
)

func assertE8_MTLSHandshake(tc documentedCase) {
	result := defaultLocalVerifierRegistry.Result(localVerifierMTLS)
	Expect(result.Err).NotTo(HaveOccurred(),
		"%s: mTLS handshake verifier failed: %v", tc.tcNodeLabel(), result.Err)
}

func assertE8_MTLSPlaintextReject(tc documentedCase) {
	result := defaultLocalVerifierRegistry.Result(localVerifierMTLS)
	Expect(result.Err).NotTo(HaveOccurred(),
		"%s: mTLS plaintext-reject verifier failed: %v", tc.tcNodeLabel(), result.Err)
}

func assertE8_MTLSWrongCA(tc documentedCase) {
	result := defaultLocalVerifierRegistry.Result(localVerifierMTLS)
	Expect(result.Err).NotTo(HaveOccurred(),
		"%s: mTLS wrong-CA verifier failed: %v", tc.tcNodeLabel(), result.Err)
}
