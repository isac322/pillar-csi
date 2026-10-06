// Package recoveryauth implements the signature and digest scheme for
// operator-authorized volume ownership recovery (the
// TransferVolumeOwnership RPC and PillarVolumeState.spec.recovery).
//
// Two artifacts exist:
//
//   - RecoverySnapshot: the agent's signed self-attestation of what it
//     observed about a backend volume while healthy.  Signed with the
//     agent's own server TLS identity key.
//   - RecoveryAuthorization: the operator's signed grant to transfer that
//     exact observation into a new lifecycle.  Signed with an operator key
//     whose public half is configured through --recovery-trust-anchor.
//
// Canonical encoding: both artifacts are marshaled with
// proto.MarshalOptions{Deterministic: true} after clearing their signature
// field, so the signed payload and the digests are stable across protobuf
// implementations and serializers.  Digests are SHA-256 over those
// canonical bytes.  Signatures use SHA-256 with RSA PKCS#1 v1.5 or ECDSA
// (ASN.1 DER); every other key type is rejected with ErrUnsupportedKeyType.
//
// This package never accepts identity assertions from an mTLS peer: a
// signature verifies only against an explicitly supplied public key or
// certificate.
package recoveryauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// Error sentinels.  Every returned error wraps one of these with %w so
// callers can classify with errors.Is while still reading the detail.
var (
	// ErrUnsupportedKeyType means a signer or public key is neither
	// *rsa.PrivateKey/*rsa.PublicKey nor *ecdsa.PrivateKey/*ecdsa.PublicKey.
	ErrUnsupportedKeyType = errors.New("recoveryauth: unsupported key type")

	// ErrInvalidSignature means signature verification failed.
	ErrInvalidSignature = errors.New("recoveryauth: invalid signature")

	// ErrExpired means the artifact's validity window has passed.
	ErrExpired = errors.New("recoveryauth: artifact expired")

	// ErrNotYetValid means issued_at is in the future beyond ClockSkew.
	ErrNotYetValid = errors.New("recoveryauth: artifact not yet valid")

	// ErrValidityWindowTooLong means issued_at→expires_at exceeds
	// MaxAuthorizationLifetime.
	ErrValidityWindowTooLong = errors.New("recoveryauth: validity window too long")

	// ErrMissingField means a required field is absent or empty.
	ErrMissingField = errors.New("recoveryauth: missing required field")

	// ErrDigestMismatch means an expected digest does not equal the
	// computed one (compared in constant time).
	ErrDigestMismatch = errors.New("recoveryauth: digest mismatch")

	// ErrIdentityMismatch means agent_identity does not match the
	// verifying certificate's subject CN or first DNS SAN.
	ErrIdentityMismatch = errors.New("recoveryauth: agent identity mismatch")

	// ErrKeyMismatch means a signer does not correspond to the public key
	// it is claimed to match.
	ErrKeyMismatch = errors.New("recoveryauth: signer does not match public key")

	// ErrMalformed means bytes could not be parsed (PEM, DER, protobuf,
	// hex digest).
	ErrMalformed = errors.New("recoveryauth: malformed input")
)

// Bounded timeliness policy.
const (
	// ClockSkew tolerates issuer/verifier clock disagreement when
	// interpreting issued_at (and expires_at) boundaries.
	ClockSkew = 2 * time.Minute

	// MaxSnapshotAge is how old a snapshot may be at verify time.  A
	// snapshot describes an observation made while the agent was healthy;
	// stale observations must be re-taken via InspectVolume, never replayed.
	MaxSnapshotAge = 15 * time.Minute

	// MaxAuthorizationLifetime bounds issued_at→expires_at so an operator
	// cannot mint a permanently valid grant.
	MaxAuthorizationLifetime = 24 * time.Hour
)

// DigestSize is the byte length of every digest this package produces.
const DigestSize = sha256.Size

var marshalCanonical = proto.MarshalOptions{Deterministic: true}

// canonicalBytes marshals msg deterministically — the single marshaling
// rule shared by both signed artifacts.  Callers pass a clone whose
// signature field has already been cleared.
func canonicalBytes(msg proto.Message) ([]byte, error) {
	b, err := marshalCanonical.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical marshal: %w", ErrMalformed, err)
	}
	return b, nil
}

// digestOf returns the SHA-256 of canonical canonical bytes.
func digestOf(canonical []byte) [DigestSize]byte {
	return sha256.Sum256(canonical)
}

// checkKeySupported validates that pub is a supported verification key.
func checkKeySupported(pub crypto.PublicKey) error {
	switch pub.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey:
		return nil
	default:
		return fmt.Errorf("%w: %T (only RSA and ECDSA are supported)", ErrUnsupportedKeyType, pub)
	}
}

// checkSignerSupported validates that signer's public key is supported.
func checkSignerSupported(signer crypto.Signer) error {
	if signer == nil {
		return fmt.Errorf("%w: nil signer", ErrMissingField)
	}
	return checkKeySupported(signer.Public())
}

// signDigest signs a SHA-256 digest.  RSA uses PKCS#1 v1.5, ECDSA uses
// ASN.1 DER; both are what crypto.Signer.Sign produces for
// crypto.SHA256, so one call covers every supported key.
func signDigest(signer crypto.Signer, digest [DigestSize]byte) ([]byte, error) {
	sig, err := signer.Sign(nil, digest[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("recoveryauth: sign: %w", err)
	}
	return sig, nil
}

// verifySignature verifies sig over digest in constant time.  RSA uses
// PKCS#1 v1.5, ECDSA uses ASN.1 DER — matching crypto.Signer.Sign output.
func verifySignature(pub crypto.PublicKey, digest [DigestSize]byte, sig []byte) error {
	err := checkKeySupported(pub)
	if err != nil {
		return err
	}
	if len(sig) == 0 {
		return fmt.Errorf("%w: signature", ErrMissingField)
	}
	switch k := pub.(type) {
	case *rsa.PublicKey:
		err = rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig)
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(k, digest[:], sig) {
			err = errors.New("ecdsa signature did not verify")
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSignature, err)
	}
	return nil
}

// equalDigest compares two digests in constant time.
func equalDigest(a [DigestSize]byte, b []byte) bool {
	if len(b) != DigestSize {
		return false
	}
	return subtle.ConstantTimeCompare(a[:], b) == 1
}

// ── RecoverySnapshot ────────────────────────────────────────────────────────.

// CanonicalSnapshotBytes returns the deterministic encoding of snap with its
// signature field cleared — the exact bytes that are signed and digested.
// The snap argument is not mutated.
func CanonicalSnapshotBytes(snap *agentv1.RecoverySnapshot) ([]byte, error) {
	if snap == nil {
		return nil, fmt.Errorf("%w: nil snapshot", ErrMissingField)
	}
	c := proto.CloneOf(snap)
	c.Signature = nil
	return canonicalBytes(c)
}

// SnapshotDigest returns SHA-256(CanonicalSnapshotBytes(snap)).  This is the
// digest RecoveryAuthorization.snapshot_digest commits to.
func SnapshotDigest(snap *agentv1.RecoverySnapshot) ([DigestSize]byte, error) {
	b, err := CanonicalSnapshotBytes(snap)
	if err != nil {
		return [DigestSize]byte{}, err
	}
	return digestOf(b), nil
}

// SignSnapshot sets snap.Signature to signer's signature over the canonical
// snapshot payload.  The signer's public key must be RSA or ECDSA.
func SignSnapshot(snap *agentv1.RecoverySnapshot, signer crypto.Signer) error {
	err := checkSignerSupported(signer)
	if err != nil {
		return err
	}
	if snap == nil {
		return fmt.Errorf("%w: nil snapshot", ErrMissingField)
	}
	snap.Signature = nil
	payload, err := canonicalBytes(snap)
	if err != nil {
		return err
	}
	sig, err := signDigest(signer, digestOf(payload))
	if err != nil {
		return err
	}
	snap.Signature = sig
	return nil
}

// verifySnapshotFields checks every field a snapshot must carry to be
// meaningful, independent of the signature.
func verifySnapshotFields(snap *agentv1.RecoverySnapshot) error {
	switch {
	case snap.GetVolumeId() == "":
		return fmt.Errorf("%w: volume_id", ErrMissingField)
	case snap.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM:
		return fmt.Errorf("%w: backend_type must be BACKEND_TYPE_LVM, got %v", ErrMalformed, snap.GetBackendType())
	case snap.GetLvmSource() == nil:
		return fmt.Errorf("%w: lvm_source", ErrMissingField)
	case snap.GetOldVolumeUid() == "":
		return fmt.Errorf("%w: old_volume_uid", ErrMissingField)
	case snap.GetAgentIdentity() == "":
		return fmt.Errorf("%w: agent_identity", ErrMissingField)
	case snap.GetIssuedAt() == nil:
		return fmt.Errorf("%w: issued_at", ErrMissingField)
	}
	err := snap.GetIssuedAt().CheckValid()
	if err != nil {
		return fmt.Errorf("%w: issued_at: %w", ErrMalformed, err)
	}
	// Top-level old uid/generation MUST equal the observed fence mark's;
	// a snapshot that disagrees with itself is malformed.
	if f := snap.GetFence(); f != nil && f.GetExists() {
		if snap.GetOldVolumeUid() != f.GetVolumeUid() {
			return fmt.Errorf("%w: old_volume_uid %q != fence.volume_uid %q",
				ErrMalformed, snap.GetOldVolumeUid(), f.GetVolumeUid())
		}
		if snap.GetOldGeneration() != f.GetGeneration() {
			return fmt.Errorf("%w: old_generation %d != fence.generation %d",
				ErrMalformed, snap.GetOldGeneration(), f.GetGeneration())
		}
	}
	return nil
}

// VerifySnapshot verifies snap's signature under pub and checks required
// fields plus issued_at freshness (not future beyond ClockSkew, not older
// than MaxSnapshotAge) at instant now.
func VerifySnapshot(snap *agentv1.RecoverySnapshot, pub crypto.PublicKey, now time.Time) error {
	err := checkKeySupported(pub)
	if err != nil {
		return err
	}
	if snap == nil {
		return fmt.Errorf("%w: nil snapshot", ErrMissingField)
	}
	err = verifySnapshotFields(snap)
	if err != nil {
		return err
	}
	err = checkIssuedAt(snap.GetIssuedAt().AsTime(), now)
	if err != nil {
		return err
	}
	payload, err := CanonicalSnapshotBytes(snap)
	if err != nil {
		return err
	}
	return verifySignature(pub, digestOf(payload), snap.GetSignature())
}

// VerifySnapshotWithCertificate is VerifySnapshot against cert's public key,
// additionally requiring snap.AgentIdentity to equal the certificate's
// Subject Common Name or, when that is empty, its first DNS SAN — the same
// identity the agent would present over its server TLS endpoint.
func VerifySnapshotWithCertificate(snap *agentv1.RecoverySnapshot, cert *x509.Certificate, now time.Time) error {
	if cert == nil {
		return fmt.Errorf("%w: nil certificate", ErrMissingField)
	}
	if snap != nil {
		want := cert.Subject.CommonName
		if want == "" && len(cert.DNSNames) > 0 {
			want = cert.DNSNames[0]
		}
		if want == "" {
			return fmt.Errorf("%w: certificate carries no CN or DNS SAN to compare with agent_identity", ErrMalformed)
		}
		if snap.GetAgentIdentity() != want {
			return fmt.Errorf("%w: agent_identity %q, certificate identity %q",
				ErrIdentityMismatch, snap.GetAgentIdentity(), want)
		}
	}
	return VerifySnapshot(snap, cert.PublicKey, now)
}

// checkIssuedAt bounds an issued_at instant relative to now.
func checkIssuedAt(issued, now time.Time) error {
	if issued.After(now.Add(ClockSkew)) {
		return fmt.Errorf("%w: issued_at %s is in the future (now %s, skew %s)",
			ErrNotYetValid, issued.UTC(), now.UTC(), ClockSkew)
	}
	if issued.Before(now.Add(-MaxSnapshotAge - ClockSkew)) {
		return fmt.Errorf("%w: issued_at %s older than MaxSnapshotAge %s",
			ErrExpired, issued.UTC(), MaxSnapshotAge)
	}
	return nil
}

// ── RecoveryAuthorization ───────────────────────────────────────────────────.

// CanonicalAuthorizationBytes returns the deterministic encoding of auth
// with its signature field cleared — the exact bytes that are signed and
// digested.  The auth argument is not mutated.
func CanonicalAuthorizationBytes(auth *agentv1.RecoveryAuthorization) ([]byte, error) {
	if auth == nil {
		return nil, fmt.Errorf("%w: nil authorization", ErrMissingField)
	}
	c := proto.CloneOf(auth)
	c.Signature = nil
	return canonicalBytes(c)
}

// AuthorizationDigest returns SHA-256(CanonicalAuthorizationBytes(auth)).
// This is the digest recorded in PillarVolumeState.spec.recovery.
// The same digest is recorded in authorizationDigest and in the transferred
// fence mark.
func AuthorizationDigest(auth *agentv1.RecoveryAuthorization) ([DigestSize]byte, error) {
	b, err := CanonicalAuthorizationBytes(auth)
	if err != nil {
		return [DigestSize]byte{}, err
	}
	return digestOf(b), nil
}

// AuthorizationDigestBytes digests a serialized RecoveryAuthorization (as
// stored in spec.recovery.authorization): it unmarshals, then digests the
// canonical payload, so any protobuf serialization of the same message
// yields the same digest.
func AuthorizationDigestBytes(serialized []byte) ([DigestSize]byte, error) {
	var auth agentv1.RecoveryAuthorization
	err := proto.Unmarshal(serialized, &auth)
	if err != nil {
		return [DigestSize]byte{}, fmt.Errorf("%w: unmarshal authorization: %w", ErrMalformed, err)
	}
	return AuthorizationDigest(&auth)
}

// SignAuthorization sets auth.Signature to signer's signature over the
// canonical authorization payload.  The signer's public key must be RSA or
// ECDSA.
func SignAuthorization(auth *agentv1.RecoveryAuthorization, signer crypto.Signer) error {
	err := checkSignerSupported(signer)
	if err != nil {
		return err
	}
	if auth == nil {
		return fmt.Errorf("%w: nil authorization", ErrMissingField)
	}
	auth.Signature = nil
	payload, err := canonicalBytes(auth)
	if err != nil {
		return err
	}
	sig, err := signDigest(signer, digestOf(payload))
	if err != nil {
		return err
	}
	auth.Signature = sig
	return nil
}

// verifyAuthorizationFields checks every field an authorization must carry.
// Expiry and signature are checked separately.
func verifyAuthorizationFields(auth *agentv1.RecoveryAuthorization) error {
	switch {
	case len(auth.GetSnapshotDigest()) != DigestSize:
		return fmt.Errorf("%w: snapshot_digest must be %d bytes, got %d",
			ErrMalformed, DigestSize, len(auth.GetSnapshotDigest()))
	case auth.GetVolumeId() == "":
		return fmt.Errorf("%w: volume_id", ErrMissingField)
	case auth.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM:
		return fmt.Errorf("%w: backend_type must be BACKEND_TYPE_LVM, got %v", ErrMalformed, auth.GetBackendType())
	case auth.GetLvmSource() == nil:
		return fmt.Errorf("%w: lvm_source", ErrMissingField)
	case auth.GetOldVolumeUid() == "":
		return fmt.Errorf("%w: old_volume_uid", ErrMissingField)
	case auth.GetNewVolumeUid() == "":
		return fmt.Errorf("%w: new_volume_uid", ErrMissingField)
	case auth.GetNewVolumeUid() == auth.GetOldVolumeUid():
		return fmt.Errorf("%w: new_volume_uid equals old_volume_uid", ErrMalformed)
	case auth.PreserveOriginal == nil:
		return fmt.Errorf("%w: preserve_original (explicit true or false required)", ErrMissingField)
	case auth.GetIssuedAt() == nil:
		return fmt.Errorf("%w: issued_at", ErrMissingField)
	case auth.GetExpiresAt() == nil:
		return fmt.Errorf("%w: expires_at", ErrMissingField)
	}
	err := auth.GetIssuedAt().CheckValid()
	if err != nil {
		return fmt.Errorf("%w: issued_at: %w", ErrMalformed, err)
	}
	err = auth.GetExpiresAt().CheckValid()
	if err != nil {
		return fmt.Errorf("%w: expires_at: %w", ErrMalformed, err)
	}
	return nil
}

// VerifyAuthorization verifies auth's signature under at least one of keys
// (the configured trust anchors) and checks required fields plus the
// issued_at/expires_at window at instant now.
func VerifyAuthorization(auth *agentv1.RecoveryAuthorization, keys []crypto.PublicKey, now time.Time) error {
	if len(keys) == 0 {
		return fmt.Errorf("%w: no trust anchor keys configured", ErrMissingField)
	}
	if auth == nil {
		return fmt.Errorf("%w: nil authorization", ErrMissingField)
	}
	err := verifyAuthorizationFields(auth)
	if err != nil {
		return err
	}
	issued := auth.GetIssuedAt().AsTime()
	expires := auth.GetExpiresAt().AsTime()
	if !expires.After(issued) {
		return fmt.Errorf("%w: expires_at %s not after issued_at %s",
			ErrMalformed, expires.UTC(), issued.UTC())
	}
	if expires.Sub(issued) > MaxAuthorizationLifetime+ClockSkew {
		return fmt.Errorf("%w: %s exceeds MaxAuthorizationLifetime %s",
			ErrValidityWindowTooLong, expires.Sub(issued), MaxAuthorizationLifetime)
	}
	if issued.After(now.Add(ClockSkew)) {
		return fmt.Errorf("%w: issued_at %s is in the future (now %s, skew %s)",
			ErrNotYetValid, issued.UTC(), now.UTC(), ClockSkew)
	}
	if !expires.After(now.Add(-ClockSkew)) {
		return fmt.Errorf("%w: expires_at %s passed (now %s)",
			ErrExpired, expires.UTC(), now.UTC())
	}
	payload, err := CanonicalAuthorizationBytes(auth)
	if err != nil {
		return err
	}
	digest := digestOf(payload)
	var lastErr error
	for _, pub := range keys {
		err = checkKeySupported(pub)
		if err != nil {
			lastErr = err
			continue
		}
		err = verifySignature(pub, digest, auth.GetSignature())
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrInvalidSignature
	}
	return lastErr
}

// CheckAuthorizationMatchesSnapshot verifies that auth commits to snap:
// SnapshotDigest must equal SnapshotDigest(snap) in constant time and every
// echoed identity field must equal the snapshot's exactly.
func CheckAuthorizationMatchesSnapshot(auth *agentv1.RecoveryAuthorization, snap *agentv1.RecoverySnapshot) error {
	if auth == nil || snap == nil {
		return fmt.Errorf("%w: nil authorization or snapshot", ErrMissingField)
	}
	d, err := SnapshotDigest(snap)
	if err != nil {
		return err
	}
	if !equalDigest(d, auth.GetSnapshotDigest()) {
		return fmt.Errorf("%w: authorization snapshot_digest does not match presented snapshot",
			ErrDigestMismatch)
	}
	if auth.GetVolumeId() != snap.GetVolumeId() ||
		auth.GetBackendType() != snap.GetBackendType() ||
		auth.GetOldVolumeUid() != snap.GetOldVolumeUid() ||
		auth.GetOldGeneration() != snap.GetOldGeneration() ||
		!lvmSourceEqual(auth.GetLvmSource(), snap.GetLvmSource()) {
		return fmt.Errorf("%w: authorization identity fields do not match snapshot", ErrMalformed)
	}
	return nil
}

func lvmSourceEqual(a, b *agentv1.LvmSourceIdentity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.GetVolumeGroup() == b.GetVolumeGroup() &&
		a.GetLogicalVolume() == b.GetLogicalVolume() &&
		a.GetVolumeGroupUuid() == b.GetVolumeGroupUuid() &&
		a.GetLogicalVolumeUuid() == b.GetLogicalVolumeUuid()
}

// ── Digest encoding ─────────────────────────────────────────────────────────.

// DigestHex encodes a digest as lowercase hex — the representation used in
// PillarVolumeState.spec.recovery.authorizationDigest.
func DigestHex(d [DigestSize]byte) string {
	return hex.EncodeToString(d[:])
}

// ParseDigestHex decodes a lowercase or mixed-case hex digest of exactly
// DigestSize bytes.
func ParseDigestHex(s string) ([DigestSize]byte, error) {
	var out [DigestSize]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, fmt.Errorf("%w: digest hex: %w", ErrMalformed, err)
	}
	if len(b) != DigestSize {
		return out, fmt.Errorf("%w: digest must be %d bytes, got %d", ErrMalformed, DigestSize, len(b))
	}
	copy(out[:], b)
	return out, nil
}

// ── Trust anchor loading ────────────────────────────────────────────────────.

// LoadPublicKeysPEM loads one or more operator public keys from a PEM file.
// Accepted blocks: "PUBLIC KEY" (PKIX) and "CERTIFICATE" (the certificate's
// public key is used).  Only RSA and ECDSA keys are kept; a block that
// parses to an unsupported key type, or a file with no usable key, is an
// error — recovery must fail closed, never silently trust nothing.
func LoadPublicKeysPEM(path string) ([]crypto.PublicKey, error) {
	//nolint:gosec // G304: path is the operator-configured trust-anchor file, not request input.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrMalformed, path, err)
	}
	var keys []crypto.PublicKey
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		var pub crypto.PublicKey
		switch block.Type {
		case "PUBLIC KEY":
			var k any
			k, err = x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: PUBLIC KEY block in %s: %w", ErrMalformed, path, err)
			}
			pub = k
		case "CERTIFICATE":
			var c *x509.Certificate
			c, err = x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: CERTIFICATE block in %s: %w", ErrMalformed, path, err)
			}
			pub = c.PublicKey
		default:
			// PRIVATE KEY, RSA PUBLIC KEY (PKCS#1), EC PARAMETERS, comments:
			// never a trust anchor.  A file that contains only such blocks
			// errors below as having no usable key.
			continue
		}
		err = checkKeySupported(pub)
		if err != nil {
			return nil, fmt.Errorf("%w in %s", err, path)
		}
		keys = append(keys, pub)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no usable PUBLIC KEY or CERTIFICATE block in %s", ErrMalformed, path)
	}
	return keys, nil
}
