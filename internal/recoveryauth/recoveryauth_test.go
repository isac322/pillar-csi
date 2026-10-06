package recoveryauth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

var testNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func rsaSigner(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

func ecdsaSigner(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	return k
}

func ed25519Signer(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return k
}

func testLvmSource() *agentv1.LvmSourceIdentity {
	return &agentv1.LvmSourceIdentity{
		VolumeGroup:       "vg0",
		LogicalVolume:     "pvc-abc",
		VolumeGroupUuid:   "AAAAAA-1111-2222-3333-4444-5555-BBBBBB",
		LogicalVolumeUuid: "CCCCCC-1111-2222-3333-4444-5555-DDDDDD",
	}
}

func testSnapshot() *agentv1.RecoverySnapshot {
	return &agentv1.RecoverySnapshot{
		VolumeId:         "vg0/pvc-abc",
		BackendType:      agentv1.BackendType_BACKEND_TYPE_LVM,
		LvmSource:        testLvmSource(),
		OldVolumeUid:     "uid-old-1",
		OldGeneration:    7,
		PreserveOriginal: true,
		ExclusiveClaim:   "free",
		Fence: &agentv1.FenceObservation{
			Exists:           true,
			VolumeUid:        "uid-old-1",
			Generation:       7,
			PreserveOriginal: true,
			LvmSource:        testLvmSource(),
		},
		AgentIdentity: "pillar-agent.test",
		IssuedAt:      timestamppb.New(testNow),
	}
}

func testAuthorizationFor(t *testing.T, snap *agentv1.RecoverySnapshot) *agentv1.RecoveryAuthorization {
	t.Helper()
	d, err := SnapshotDigest(snap)
	if err != nil {
		t.Fatalf("SnapshotDigest: %v", err)
	}
	preserve := true
	return &agentv1.RecoveryAuthorization{
		SnapshotDigest:   d[:],
		VolumeId:         snap.GetVolumeId(),
		BackendType:      snap.GetBackendType(),
		LvmSource:        snap.GetLvmSource(),
		OldVolumeUid:     snap.GetOldVolumeUid(),
		OldGeneration:    snap.GetOldGeneration(),
		NewVolumeUid:     "uid-new-1",
		NewGeneration:    1,
		PreserveOriginal: &preserve,
		IssuedAt:         timestamppb.New(testNow.Add(-time.Minute)),
		ExpiresAt:        timestamppb.New(testNow.Add(time.Hour)),
	}
}

func signSnapOrFail(t *testing.T, snap *agentv1.RecoverySnapshot, signer crypto.Signer) {
	t.Helper()
	if err := SignSnapshot(snap, signer); err != nil {
		t.Fatalf("SignSnapshot: %v", err)
	}
	if len(snap.GetSignature()) == 0 {
		t.Fatal("SignSnapshot left empty signature")
	}
}

func signAuthOrFail(t *testing.T, auth *agentv1.RecoveryAuthorization, signer crypto.Signer) {
	t.Helper()
	if err := SignAuthorization(auth, signer); err != nil {
		t.Fatalf("SignAuthorization: %v", err)
	}
	if len(auth.GetSignature()) == 0 {
		t.Fatal("SignAuthorization left empty signature")
	}
}

// signers covers the key types the deployment's TLS cert conventions use.
func signers(t *testing.T) map[string]crypto.Signer {
	return map[string]crypto.Signer{
		"rsa":   rsaSigner(t),
		"ecdsa": ecdsaSigner(t),
	}
}

func TestSnapshot_SignVerify_RoundTrip(t *testing.T) {
	for name, signer := range signers(t) {
		t.Run(name, func(t *testing.T) {
			snap := testSnapshot()
			signSnapOrFail(t, snap, signer)
			if err := VerifySnapshot(snap, signer.Public(), testNow); err != nil {
				t.Fatalf("VerifySnapshot: %v", err)
			}
		})
	}
}

func TestSnapshot_CanonicalExcludesSignature(t *testing.T) {
	signer := ecdsaSigner(t)
	snap := testSnapshot()
	before, err := CanonicalSnapshotBytes(snap)
	if err != nil {
		t.Fatalf("CanonicalSnapshotBytes: %v", err)
	}
	signSnapOrFail(t, snap, signer)
	after, err := CanonicalSnapshotBytes(snap)
	if err != nil {
		t.Fatalf("CanonicalSnapshotBytes: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("canonical bytes changed after signing")
	}
	// The original message kept its signature (no clearing side effects on
	// canonical digest input, signature stays on the artifact).
	d1, err := SnapshotDigest(snap)
	if err != nil {
		t.Fatalf("SnapshotDigest: %v", err)
	}
	c := proto.CloneOf(snap)
	c.Signature = []byte{0xde, 0xad}
	d2, err := SnapshotDigest(c)
	if err != nil {
		t.Fatalf("SnapshotDigest of clone with altered signature: %v", err)
	}
	if d1 != d2 {
		t.Fatal("SnapshotDigest depends on signature bytes")
	}
}

func TestSnapshot_VerifyRejectsTamperedPayload(t *testing.T) {
	signer := ecdsaSigner(t)
	cases := map[string]func(*agentv1.RecoverySnapshot){
		"volume_id":      func(s *agentv1.RecoverySnapshot) { s.VolumeId = "vg0/other" },
		"old_uid":        func(s *agentv1.RecoverySnapshot) { s.OldVolumeUid = "uid-other" },
		"old_generation": func(s *agentv1.RecoverySnapshot) { s.OldGeneration = 8 },
		"consumers_added": func(s *agentv1.RecoverySnapshot) {
			s.Consumers = append(s.Consumers, &agentv1.DeviceConsumer{Kind: "mount", Detail: "/x"})
		},
		"preserve_flipped": func(s *agentv1.RecoverySnapshot) { s.PreserveOriginal = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			snap := testSnapshot()
			signSnapOrFail(t, snap, signer)
			mutate(snap)
			if err := VerifySnapshot(snap, signer.Public(), testNow); !errors.Is(err, ErrInvalidSignature) {
				// old_uid/old_generation mutations may also trip the
				// fence-consistency check (ErrMalformed); either is a reject.
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("expected rejection, got %v", err)
				}
			}
		})
	}
}

func TestSnapshot_VerifyRejectsWrongKey(t *testing.T) {
	signer := ecdsaSigner(t)
	snap := testSnapshot()
	signSnapOrFail(t, snap, signer)
	other := ecdsaSigner(t)
	if err := VerifySnapshot(snap, other.Public(), testNow); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestSnapshot_VerifyRejectsUnsupportedKey(t *testing.T) {
	ed := ed25519Signer(t)
	snap := testSnapshot()
	if err := SignSnapshot(snap, ed); !errors.Is(err, ErrUnsupportedKeyType) {
		t.Fatalf("SignSnapshot(ed25519): expected ErrUnsupportedKeyType, got %v", err)
	}
	signSnapOrFail(t, snap, ecdsaSigner(t))
	if err := VerifySnapshot(snap, ed.Public(), testNow); !errors.Is(err, ErrUnsupportedKeyType) {
		t.Fatalf("VerifySnapshot(ed25519): expected ErrUnsupportedKeyType, got %v", err)
	}
}

func TestSnapshot_VerifyRejectsMissingFields(t *testing.T) {
	signer := ecdsaSigner(t)
	cases := map[string]func(*agentv1.RecoverySnapshot){
		"no_signature":   func(s *agentv1.RecoverySnapshot) { s.Signature = nil },
		"no_issued_at":   func(s *agentv1.RecoverySnapshot) { s.IssuedAt = nil },
		"no_agent_id":    func(s *agentv1.RecoverySnapshot) { s.AgentIdentity = "" },
		"no_old_uid":     func(s *agentv1.RecoverySnapshot) { s.OldVolumeUid = "" },
		"no_lvm_source":  func(s *agentv1.RecoverySnapshot) { s.LvmSource = nil },
		"wrong_backend":  func(s *agentv1.RecoverySnapshot) { s.BackendType = agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL },
		"fence_mismatch": func(s *agentv1.RecoverySnapshot) { s.Fence.Generation = 99 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			snap := testSnapshot()
			mutate(snap)
			if snap.Signature != nil {
				signSnapOrFail(t, snap, signer)
				mutate(snap) // mutate after signing too, so signature still present
			}
			if err := VerifySnapshot(snap, signer.Public(), testNow); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestSnapshot_IssuedAtBounds(t *testing.T) {
	signer := ecdsaSigner(t)
	cases := []struct {
		name    string
		issued  time.Time
		wantErr error
	}{
		{"fresh", testNow, nil},
		{"within_skew", testNow.Add(-MaxSnapshotAge), nil},
		{"stale", testNow.Add(-MaxSnapshotAge - ClockSkew - time.Second), ErrExpired},
		{"future", testNow.Add(ClockSkew + time.Second), ErrNotYetValid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := testSnapshot()
			snap.IssuedAt = timestamppb.New(tc.issued)
			signSnapOrFail(t, snap, signer)
			err := VerifySnapshot(snap, signer.Public(), testNow)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestSnapshot_VerifyWithCertificate(t *testing.T) {
	signer := ecdsaSigner(t)
	cert := selfSignedCert(t, signer, "pillar-agent.test")

	snap := testSnapshot()
	signSnapOrFail(t, snap, signer)
	if err := VerifySnapshotWithCertificate(snap, cert, testNow); err != nil {
		t.Fatalf("VerifySnapshotWithCertificate: %v", err)
	}

	// Identity mismatch is rejected even with a valid signature.
	bad := proto.CloneOf(snap)
	bad.AgentIdentity = "other-agent.test"
	signSnapOrFail(t, bad, signer)
	if err := VerifySnapshotWithCertificate(bad, cert, testNow); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("expected ErrIdentityMismatch, got %v", err)
	}
}

func TestAuthorization_SignVerify_RoundTrip(t *testing.T) {
	for name, opKey := range signers(t) {
		t.Run(name, func(t *testing.T) {
			snap := testSnapshot()
			signSnapOrFail(t, snap, ecdsaSigner(t))
			auth := testAuthorizationFor(t, snap)
			signAuthOrFail(t, auth, opKey)
			if err := VerifyAuthorization(auth, []crypto.PublicKey{opKey.Public()}, testNow); err != nil {
				t.Fatalf("VerifyAuthorization: %v", err)
			}
			if err := CheckAuthorizationMatchesSnapshot(auth, snap); err != nil {
				t.Fatalf("CheckAuthorizationMatchesSnapshot: %v", err)
			}
		})
	}
}

func TestAuthorization_TrustAnchorSet(t *testing.T) {
	good := ecdsaSigner(t)
	other := rsaSigner(t)
	unused := ecdsaSigner(t)
	snap := testSnapshot()
	auth := testAuthorizationFor(t, snap)
	signAuthOrFail(t, auth, good)

	// Any configured anchor key satisfies.
	anchors := []crypto.PublicKey{other.Public(), good.Public(), unused.Public()}
	if err := VerifyAuthorization(auth, anchors, testNow); err != nil {
		t.Fatalf("expected acceptance with anchor in set, got %v", err)
	}
	// A set without the signer rejects.
	withoutSigner := []crypto.PublicKey{other.Public(), unused.Public()}
	if err := VerifyAuthorization(auth, withoutSigner, testNow); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
	// An empty anchor set fails closed.
	if err := VerifyAuthorization(auth, nil, testNow); !errors.Is(err, ErrMissingField) {
		t.Fatalf("expected ErrMissingField, got %v", err)
	}
}

func TestAuthorization_ReplayedAgainstDifferentSnapshot(t *testing.T) {
	opKey := ecdsaSigner(t)
	snap := testSnapshot()
	auth := testAuthorizationFor(t, snap)
	signAuthOrFail(t, auth, opKey)

	// A second, differently-signed snapshot of the same observation must
	// not match (digest covers the payload — here we change the payload).
	replayed := testSnapshot()
	replayed.OldGeneration = 8
	replayed.Fence.Generation = 8
	if err := CheckAuthorizationMatchesSnapshot(auth, replayed); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch, got %v", err)
	}
}

func TestAuthorization_FieldMismatch(t *testing.T) {
	opKey := ecdsaSigner(t)
	snap := testSnapshot()
	auth := testAuthorizationFor(t, snap)
	signAuthOrFail(t, auth, opKey)

	cases := map[string]func(*agentv1.RecoveryAuthorization){
		"volume_id":      func(a *agentv1.RecoveryAuthorization) { a.VolumeId = "vg0/other" },
		"old_generation": func(a *agentv1.RecoveryAuthorization) { a.OldGeneration = 8 },
		"lvm_source": func(a *agentv1.RecoveryAuthorization) {
			a.LvmSource.LogicalVolumeUuid = "XXXXXX-1111-2222-3333-4444-5555-YYYYYY"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := proto.CloneOf(auth)
			mutate(a)
			// Re-sign so signature passes; matching must still fail.
			signAuthOrFail(t, a, opKey)
			if err := CheckAuthorizationMatchesSnapshot(a, snap); err == nil {
				t.Fatal("expected mismatch rejection")
			}
		})
	}
}

func TestAuthorization_ExpiryAndWindow(t *testing.T) {
	opKey := ecdsaSigner(t)
	snap := testSnapshot()

	cases := []struct {
		name    string
		issued  time.Time
		expires time.Time
		wantErr error
	}{
		{"valid", testNow.Add(-time.Minute), testNow.Add(time.Hour), nil},
		{"expired", testNow.Add(-2 * time.Hour), testNow.Add(-time.Hour), ErrExpired},
		{"not_yet_valid", testNow.Add(time.Hour), testNow.Add(2 * time.Hour), ErrNotYetValid},
		{
			"window_too_long",
			testNow.Add(-time.Minute),
			testNow.Add(MaxAuthorizationLifetime + ClockSkew + time.Minute),
			ErrValidityWindowTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := testAuthorizationFor(t, snap)
			auth.IssuedAt = timestamppb.New(tc.issued)
			auth.ExpiresAt = timestamppb.New(tc.expires)
			signAuthOrFail(t, auth, opKey)
			err := VerifyAuthorization(auth, []crypto.PublicKey{opKey.Public()}, testNow)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestAuthorization_RejectsMissingFields(t *testing.T) {
	opKey := ecdsaSigner(t)
	snap := testSnapshot()
	cases := map[string]func(*agentv1.RecoveryAuthorization){
		"no_preserve":    func(a *agentv1.RecoveryAuthorization) { a.PreserveOriginal = nil },
		"no_new_uid":     func(a *agentv1.RecoveryAuthorization) { a.NewVolumeUid = "" },
		"new_eq_old":     func(a *agentv1.RecoveryAuthorization) { a.NewVolumeUid = a.OldVolumeUid },
		"bad_digest_len": func(a *agentv1.RecoveryAuthorization) { a.SnapshotDigest = []byte{1, 2, 3} },
		"no_expiry":      func(a *agentv1.RecoveryAuthorization) { a.ExpiresAt = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			auth := testAuthorizationFor(t, snap)
			mutate(auth)
			signAuthOrFail(t, auth, opKey)
			if err := VerifyAuthorization(auth, []crypto.PublicKey{opKey.Public()}, testNow); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestAuthorization_DigestAndSerialization(t *testing.T) {
	opKey := ecdsaSigner(t)
	snap := testSnapshot()
	auth := testAuthorizationFor(t, snap)
	signAuthOrFail(t, auth, opKey)

	d1, err := AuthorizationDigest(auth)
	if err != nil {
		t.Fatalf("AuthorizationDigest: %v", err)
	}
	raw, err := proto.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d2, err := AuthorizationDigestBytes(raw)
	if err != nil {
		t.Fatalf("AuthorizationDigestBytes: %v", err)
	}
	if d1 != d2 {
		t.Fatal("digest differs across serialization round trip")
	}
	if got := DigestHex(d1); len(got) != 64 {
		t.Fatalf("DigestHex length %d", len(got))
	}
	parsed, err := ParseDigestHex(DigestHex(d1))
	if err != nil || parsed != d1 {
		t.Fatalf("ParseDigestHex round trip: %v", err)
	}
	if _, err := ParseDigestHex("zz"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
	if _, err := ParseDigestHex("abcd"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed for short digest, got %v", err)
	}
	if _, err := AuthorizationDigestBytes([]byte{0xff, 0xff}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed for garbage bytes, got %v", err)
	}
}

func TestLoadPublicKeysPEM(t *testing.T) {
	dir := t.TempDir()

	rsaKey := rsaSigner(t)
	ecKey := ecdsaSigner(t)

	pubDER, err := x509.MarshalPKIXPublicKey(rsaKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: selfSignedCert(t, ecKey, "op.test").Raw})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	// Mixed file: RSA PKIX key + ECDSA certificate both load.
	path := filepath.Join(dir, "anchors.pem")
	err = os.WriteFile(path, append(pubPEM, certPEM...), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadPublicKeysPEM(path)
	if err != nil {
		t.Fatalf("LoadPublicKeysPEM: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}

	// Garbage file fails closed.
	bad := filepath.Join(dir, "bad.pem")
	err = os.WriteFile(bad, []byte("not pem"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadPublicKeysPEM(bad)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}

	// Private-key-only file fails closed — never silently trusted.
	privDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	privPath := filepath.Join(dir, "priv.pem")
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	err = os.WriteFile(privPath, privPEM, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadPublicKeysPEM(privPath)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed for private-key file, got %v", err)
	}

	// Missing file fails closed.
	_, err = LoadPublicKeysPEM(filepath.Join(dir, "absent.pem"))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed for missing file, got %v", err)
	}
}

// selfSignedCert builds an ephemeral signing cert in-memory — never persisted.
func selfSignedCert(t *testing.T, signer crypto.Signer, cn string) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             testNow.Add(-time.Hour),
		NotAfter:              testNow.Add(time.Hour),
		BasicConstraintsValid: true,
	}
	var pub crypto.PublicKey
	switch k := signer.(type) {
	case *rsa.PrivateKey:
		pub = &k.PublicKey
	case *ecdsa.PrivateKey:
		pub = &k.PublicKey
	default:
		t.Fatalf("unsupported signer %T", signer)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, signer)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}
