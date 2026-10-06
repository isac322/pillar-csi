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

package agent_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/recoveryauth"
)

// recoveryAuthority is the recovery configuration of a test agent: its own
// signing identity (a TLS certificate stand-in) and the operator trust
// anchors it verifies authorizations against.
type recoveryAuthority struct {
	signer  *ecdsa.PrivateKey
	cert    *x509.Certificate
	opKey   *ecdsa.PrivateKey
	anchors []crypto.PublicKey
}

// recoveryTestCertificate issues a self-signed certificate whose CommonName
// is the attested agent identity, plus its ECDSA key.
func recoveryTestCertificate(t *testing.T, commonName string) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

// newRecoveryAuthority generates an agent identity (CN "pillar-agent.test")
// and a distinct operator key trusted via anchors.
func newRecoveryAuthority(t *testing.T) recoveryAuthority {
	t.Helper()
	signer, cert := recoveryTestCertificate(t, "pillar-agent.test")
	opKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return recoveryAuthority{
		signer:  signer,
		cert:    cert,
		opKey:   opKey,
		anchors: []crypto.PublicKey{&opKey.PublicKey},
	}
}

// recoveryCtx is the context of a verified mTLS peer: AuthInfo carries TLS
// connection state with one verified chain.
func recoveryCtx() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}},
		},
	})
}

// newRecoveryTestServer is newLVTestServer with the recovery authority
// configured.  The cfgRoot is returned for tests that build exports.
func newRecoveryTestServer(
	t *testing.T,
	b backend.VolumeBackend,
	ra recoveryAuthority,
) (srv *agent.Server, stateDir, cfgRoot string) {
	t.Helper()
	stateDir, cfgRoot = t.TempDir(), t.TempDir()
	srv = agent.NewServer(map[string]backend.VolumeBackend{testPool: b}, cfgRoot,
		agent.WithDrainStateDir(stateDir),
		agent.WithRecoveryAuthority(ra.signer, ra.cert, ra.anchors))
	agent.SetDeviceChecker(t, srv, nvmeof.AlwaysPresentChecker)
	return srv, stateDir, cfgRoot
}

// recoveryMark is the on-disk mark the transfer commits, decoded verbatim.
type recoveryMark struct {
	VolumeUID        string         `json:"volumeUID"`
	Generation       uint64         `json:"generation"`
	Ended            bool           `json:"ended"`
	EndedUIDs        []string       `json:"endedUIDs"`
	LVMSource        map[string]any `json:"lvmSource"`
	PreserveOriginal bool           `json:"preserveOriginal"`
	Transfer         *struct {
		AuthorizationDigest string `json:"authorizationDigest"`
		FromUID             string `json:"fromUID"`
		FromGeneration      uint64 `json:"fromGeneration"`
		ToUID               string `json:"toUID"`
		ToGeneration        uint64 `json:"toGeneration"`
	} `json:"transfer"`
}

func readRecoveryMark(t *testing.T, stateDir string) recoveryMark {
	t.Helper()
	var m recoveryMark
	if err := json.Unmarshal(markBytes(t, stateDir), &m); err != nil {
		t.Fatalf("decode mark: %v", err)
	}
	return m
}

// snapshotOf returns the signed snapshot InspectVolume issues over the
// verified-mTLS test context; the caller must have seeded a live pinned
// lifecycle for one to be issued at all.
func snapshotOf(t *testing.T, srv *agent.Server) *agentv1.RecoverySnapshot {
	t.Helper()
	resp, err := srv.InspectVolume(recoveryCtx(), inspectRequest())
	if err != nil {
		t.Fatalf("InspectVolume: %v", err)
	}
	snap := resp.GetSnapshot()
	if snap == nil {
		t.Fatal("InspectVolume issued no signed snapshot")
	}
	return snap
}

// manualSnapshot signs a RecoverySnapshot of lifecycle-a generation 7 with
// PreserveOriginal set, as this agent would have issued it while healthy —
// for tests whose seeded mark cannot produce one (absent, ended or unpinned
// history, or a backend without Inspect).
func manualSnapshot(
	t *testing.T,
	ra recoveryAuthority,
	src *agentv1.LvmSourceIdentity,
) *agentv1.RecoverySnapshot {
	t.Helper()
	const (
		uid = "lifecycle-a"
		gen = 7
	)
	snap := &agentv1.RecoverySnapshot{
		VolumeId:         testVolumeID,
		BackendType:      agentv1.BackendType_BACKEND_TYPE_LVM,
		LvmSource:        src,
		OldVolumeUid:     uid,
		OldGeneration:    gen,
		PreserveOriginal: true,
		ExclusiveClaim:   backend.ExclusiveClaimFree,
		Fence: &agentv1.FenceObservation{
			Exists:     true,
			VolumeUid:  uid,
			Generation: gen,
		},
		AgentIdentity: "pillar-agent.test",
		IssuedAt:      timestamppb.Now(),
	}
	if err := recoveryauth.SignSnapshot(snap, ra.signer); err != nil {
		t.Fatalf("sign snapshot: %v", err)
	}
	return snap
}

// cloneSnapshot deep-copies s so a test can tamper with or re-sign the copy.
func cloneSnapshot(t *testing.T, s *agentv1.RecoverySnapshot) *agentv1.RecoverySnapshot {
	t.Helper()
	cloned := proto.Clone(s)
	c, ok := cloned.(*agentv1.RecoverySnapshot)
	if !ok {
		t.Fatalf("proto.Clone returned %T, want *RecoverySnapshot", cloned)
	}
	return c
}

// transferAuthorization builds the operator-signed authorization for snap
// naming destination newUID at generation 1.  The edit hook may mutate the
// payload before signing (it stays a well-formed authorization afterwards).
func transferAuthorization(
	t *testing.T,
	snap *agentv1.RecoverySnapshot,
	opKey *ecdsa.PrivateKey,
	newUID string,
	edit func(*agentv1.RecoveryAuthorization),
) *agentv1.RecoveryAuthorization {
	t.Helper()
	digest, err := recoveryauth.SnapshotDigest(snap)
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	auth := &agentv1.RecoveryAuthorization{
		SnapshotDigest:   digest[:],
		VolumeId:         snap.GetVolumeId(),
		BackendType:      snap.GetBackendType(),
		LvmSource:        snap.GetLvmSource(),
		OldVolumeUid:     snap.GetOldVolumeUid(),
		OldGeneration:    snap.GetOldGeneration(),
		NewVolumeUid:     newUID,
		NewGeneration:    1,
		PreserveOriginal: new(snap.GetPreserveOriginal()),
		IssuedAt:         timestamppb.Now(),
		ExpiresAt:        timestamppb.New(time.Now().Add(time.Hour)),
	}
	if edit != nil {
		edit(auth)
	}
	if err := recoveryauth.SignAuthorization(auth, opKey); err != nil {
		t.Fatalf("sign authorization: %v", err)
	}
	return auth
}

// transfer calls the RPC over the verified-mTLS test context.
func transfer(
	srv *agent.Server,
	snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization,
) (*agentv1.TransferVolumeOwnershipResponse, error) {
	return srv.TransferVolumeOwnership(recoveryCtx(), &agentv1.TransferVolumeOwnershipRequest{
		Snapshot: snap, Authorization: auth,
	})
}

// The happy path: a verified mTLS caller presents this agent's signed
// snapshot and an operator-signed authorization for lifecycle-b; one atomic
// mark write retires lifecycle-a and records the transfer; the destination
// lifecycle then fences normally while the old one is retired; an exact
// retry is idempotent from the durable record; and a different destination
// or a different authorization is refused.
func TestTransferVolumeOwnership_CommitsAndRetriesIdempotently(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	ra := newRecoveryAuthority(t)
	srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	snap := snapshotOf(t, srv)

	auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
	wantDigest, err := recoveryauth.AuthorizationDigest(auth)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transfer(srv, snap, auth)
	if err != nil {
		t.Fatalf("TransferVolumeOwnership: %v", err)
	}
	if resp.GetOutcome() != agentv1.TransferOutcome_TRANSFER_OUTCOME_COMMITTED {
		t.Fatalf("outcome = %v, want COMMITTED", resp.GetOutcome())
	}
	if !slices.Equal(resp.GetAuthorizationDigest(), wantDigest[:]) {
		t.Fatalf("authorization_digest = %x, want %x", resp.GetAuthorizationDigest(), wantDigest)
	}

	requireCommittedTransferMark(t, stateDir, wantDigest)
	requireDestinationOwnsVolume(t, srv)

	// Exact retry: the durable record answers identically and the visible
	// mark bytes stay unchanged; the retry intentionally re-syncs directory
	// metadata, so filesystem mtimes may advance.
	beforeMark := append([]byte(nil), markBytes(t, stateDir)...)
	resp, err = transfer(srv, snap, auth)
	if err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if resp.GetOutcome() != agentv1.TransferOutcome_TRANSFER_OUTCOME_ALREADY_COMMITTED {
		t.Fatalf("retry outcome = %v, want ALREADY_COMMITTED", resp.GetOutcome())
	}
	if !slices.Equal(resp.GetAuthorizationDigest(), wantDigest[:]) {
		t.Fatalf("retry digest = %x, want %x", resp.GetAuthorizationDigest(), wantDigest)
	}
	requireMarkUnchanged(t, stateDir, beforeMark, "idempotent retry")

	// A different destination, and the same destination under a different
	// authorization digest, are both refused.
	other := transferAuthorization(t, snap, ra.opKey, "lifecycle-c", nil)
	_, err = transfer(srv, snap, other)
	requireCode(t, "different destination", err, codes.FailedPrecondition)
	changed := transferAuthorization(t, snap, ra.opKey, "lifecycle-b",
		func(a *agentv1.RecoveryAuthorization) {
			a.ExpiresAt = timestamppb.New(time.Now().Add(2 * time.Hour))
		})
	_, err = transfer(srv, snap, changed)
	requireCode(t, "same destination, different authorization", err, codes.FailedPrecondition)
	m := readRecoveryMark(t, stateDir)
	if m.VolumeUID != "lifecycle-b" {
		t.Fatalf("refused transfers moved the mark to %q", m.VolumeUID)
	}
}

// requireCommittedTransferMark asserts the on-disk mark records the
// committed a/7→b/1 transfer under wantDigest with lifecycle-a retired.
func requireCommittedTransferMark(t *testing.T, stateDir string, wantDigest [recoveryauth.DigestSize]byte) {
	t.Helper()
	m := readRecoveryMark(t, stateDir)
	if m.VolumeUID != "lifecycle-b" || m.Generation != 1 || m.Ended ||
		len(m.EndedUIDs) != 1 || m.EndedUIDs[0] != "lifecycle-a" || !m.PreserveOriginal {
		t.Fatalf("mark after transfer %+v, want lifecycle-b gen 1 with lifecycle-a retired", m)
	}
	if m.Transfer == nil {
		t.Fatal("mark records no transfer")
	}
	if m.Transfer.AuthorizationDigest != recoveryauth.DigestHex(wantDigest) ||
		m.Transfer.FromUID != "lifecycle-a" || m.Transfer.FromGeneration != 7 ||
		m.Transfer.ToUID != "lifecycle-b" || m.Transfer.ToGeneration != 1 {
		t.Fatalf("transfer record %+v, want a/7→b/1 with the authorization digest", m.Transfer)
	}
}

// requireDestinationOwnsVolume asserts that, after the transfer, the
// destination lifecycle acts on the volume ID and the retired one cannot.
func requireDestinationOwnsVolume(t *testing.T, srv *agent.Server) {
	t.Helper()
	ctx := context.Background()
	_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testLVDevicePath,
		Fence: token2("lifecycle-b", 1),
	})
	if err != nil {
		t.Fatalf("destination lifecycle cannot export: %v", err)
	}
	_, err = srv.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Fence: token2("lifecycle-b", 1),
	})
	if err != nil {
		t.Fatalf("destination lifecycle cannot unexport: %v", err)
	}
	_, err = srv.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId: testVolumeID, RequestedBytes: 1 << 30,
		BackendType: agentv1.BackendType_BACKEND_TYPE_LVM, Fence: token2("lifecycle-a", 9),
	})
	requireCode(t, "retired lifecycle mutating", err, codes.FailedPrecondition)
}

// No TLS peer, a TLS peer without a verified chain, and a server without a
// configured recovery authority are all refused before anything is
// inspected — and write nothing.
func TestTransferVolumeOwnership_RequiresVerifiedMTLSAndConfiguration(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	ra := newRecoveryAuthority(t)
	srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	snap := snapshotOf(t, srv)
	auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
	req := &agentv1.TransferVolumeOwnershipRequest{Snapshot: snap, Authorization: auth}

	// No peer at all: the plaintext/direct-call case.
	_, err := srv.TransferVolumeOwnership(context.Background(), req)
	requireCode(t, "plaintext caller", err, codes.Unauthenticated)
	// A TLS peer whose client certificate was never verified.
	unverified := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{}},
	})
	_, err = srv.TransferVolumeOwnership(unverified, req)
	requireCode(t, "unverified TLS client", err, codes.Unauthenticated)

	// Recovery requested on an agent without --recovery-trust-anchor.
	plain, _, _ := newLVTestServer(t, b)
	_, err = plain.TransferVolumeOwnership(recoveryCtx(), req)
	requireCode(t, "unconfigured agent", err, codes.Unavailable)

	m := readRecoveryMark(t, stateDir)
	if m.VolumeUID != "lifecycle-a" || m.Generation != 7 {
		t.Fatalf("refused transfers changed the mark to %+v", m)
	}
}

// A request without its two payloads is malformed.
func TestTransferVolumeOwnership_RefusesMissingPayloads(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	ra := newRecoveryAuthority(t)
	srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	snap := snapshotOf(t, srv)
	auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)

	for _, tc := range []struct {
		name string
		req  *agentv1.TransferVolumeOwnershipRequest
	}{
		{"no snapshot", &agentv1.TransferVolumeOwnershipRequest{Authorization: auth}},
		{"no authorization", &agentv1.TransferVolumeOwnershipRequest{Snapshot: snap}},
		{"neither", &agentv1.TransferVolumeOwnershipRequest{}},
	} {
		_, err := srv.TransferVolumeOwnership(recoveryCtx(), tc.req)
		requireCode(t, tc.name, err, codes.InvalidArgument)
	}
	m := readRecoveryMark(t, stateDir)
	if m.VolumeUID != "lifecycle-a" {
		t.Fatalf("malformed requests moved the mark to %q", m.VolumeUID)
	}
}

// Forged, mismatched or stale credentials are refused before the mark is
// consulted: a snapshot inconsistent with itself, signed by another key,
// for another identity or too old; an authorization from an unknown
// operator key, expired, missing fields, or digesting another snapshot.
func TestTransferVolumeOwnership_RefusesBadCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		snap func(*testing.T, *agentv1.RecoverySnapshot, recoveryAuthority) *agentv1.RecoverySnapshot
		auth func(*testing.T, *agentv1.RecoverySnapshot, recoveryAuthority) *agentv1.RecoveryAuthorization
		want codes.Code
	}{
		{
			"snapshot tampered after signing",
			func(t *testing.T, s *agentv1.RecoverySnapshot, _ recoveryAuthority) *agentv1.RecoverySnapshot {
				tampered := cloneSnapshot(t, s)
				tampered.OldGeneration = 99
				return tampered
			},
			nil,
			codes.InvalidArgument,
		},
		{
			"snapshot signed by another key",
			func(t *testing.T, s *agentv1.RecoverySnapshot, _ recoveryAuthority) *agentv1.RecoverySnapshot {
				forged := cloneSnapshot(t, s)
				rogue, _ := recoveryTestCertificate(t, "pillar-agent.test")
				forged.Signature = nil
				if err := recoveryauth.SignSnapshot(forged, rogue); err != nil {
					t.Fatal(err)
				}
				return forged
			},
			nil,
			codes.Unauthenticated,
		},
		{
			"snapshot attesting another agent identity",
			func(t *testing.T, s *agentv1.RecoverySnapshot, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				forged := cloneSnapshot(t, s)
				forged.AgentIdentity = "other-agent.test"
				forged.Signature = nil
				if err := recoveryauth.SignSnapshot(forged, ra.signer); err != nil {
					t.Fatal(err)
				}
				return forged
			},
			nil,
			codes.Unauthenticated,
		},
		{
			"stale snapshot replay",
			func(t *testing.T, s *agentv1.RecoverySnapshot, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				stale := cloneSnapshot(t, s)
				stale.IssuedAt = timestamppb.New(time.Now().Add(-time.Hour))
				stale.Signature = nil
				if err := recoveryauth.SignSnapshot(stale, ra.signer); err != nil {
					t.Fatal(err)
				}
				return stale
			},
			nil,
			codes.FailedPrecondition,
		},
		{
			"authorization signed by an unknown operator",
			nil,
			func(t *testing.T, s *agentv1.RecoverySnapshot, _ recoveryAuthority) *agentv1.RecoveryAuthorization {
				rogue, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				return transferAuthorization(t, s, rogue, "lifecycle-b", nil)
			},
			codes.Unauthenticated,
		},
		{
			"expired authorization",
			nil,
			func(t *testing.T, s *agentv1.RecoverySnapshot, ra recoveryAuthority) *agentv1.RecoveryAuthorization {
				return transferAuthorization(t, s, ra.opKey, "lifecycle-b",
					func(a *agentv1.RecoveryAuthorization) {
						a.IssuedAt = timestamppb.New(time.Now().Add(-2 * time.Hour))
						a.ExpiresAt = timestamppb.New(time.Now().Add(-time.Hour))
					})
			},
			codes.FailedPrecondition,
		},
		{
			"authorization missing fields",
			nil,
			func(t *testing.T, s *agentv1.RecoverySnapshot, ra recoveryAuthority) *agentv1.RecoveryAuthorization {
				return transferAuthorization(t, s, ra.opKey, "lifecycle-b",
					func(a *agentv1.RecoveryAuthorization) { a.PreserveOriginal = nil })
			},
			codes.InvalidArgument,
		},
		{
			"authorization digests a different snapshot",
			nil,
			func(t *testing.T, s *agentv1.RecoverySnapshot, ra recoveryAuthority) *agentv1.RecoveryAuthorization {
				return transferAuthorization(t, s, ra.opKey, "lifecycle-b",
					func(a *agentv1.RecoveryAuthorization) { a.SnapshotDigest = make([]byte, 32) })
			},
			codes.InvalidArgument,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			ra := newRecoveryAuthority(t)
			srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
			seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
			snap := snapshotOf(t, srv)
			if tc.snap != nil {
				snap = tc.snap(t, snap, ra)
			}
			auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
			if tc.auth != nil {
				auth = tc.auth(t, snap, ra)
			}
			_, err := transfer(srv, snap, auth)
			requireCode(t, tc.name, err, tc.want)
			m := readRecoveryMark(t, stateDir)
			if m.VolumeUID != "lifecycle-a" {
				t.Fatalf("%s moved the mark to %q", tc.name, m.VolumeUID)
			}
		})
	}
}

// The durable mark is evidence of record: absent, corrupt, ended,
// generation-mismatched, destination-retired, unpinned, source-mismatched
// or preserve-downgraded history all refuse and write nothing.
func TestTransferVolumeOwnership_RefusesWrongHistory(t *testing.T) {
	t.Parallel()

	t.Run("no fencing history", func(t *testing.T) {
		t.Parallel()
		b := newMockLVBackend()
		ra := newRecoveryAuthority(t)
		srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
		snap := manualSnapshot(t, ra, testLVSource())
		auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
		_, err := transfer(srv, snap, auth)
		requireCode(t, "transfer without a mark", err, codes.FailedPrecondition)
		requireNoMarks(t, stateDir)
	})

	t.Run("corrupt mark", func(t *testing.T) {
		t.Parallel()
		b := newMockLVBackend()
		ra := newRecoveryAuthority(t)
		srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
		seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
		snap := snapshotOf(t, srv)
		auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
		if err := os.WriteFile(markFilePath(stateDir), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := transfer(srv, snap, auth)
		requireCode(t, "corrupt mark", err, codes.Internal)
	})

	for _, tc := range []struct {
		name string
		mark func() map[string]any
		snap func(*testing.T, recoveryAuthority) *agentv1.RecoverySnapshot
		auth func(*agentv1.RecoveryAuthorization)
	}{
		{
			"ended lifecycle",
			func() map[string]any { return endedPinnedMark(7, testLVSource(), true) },
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			nil,
		},
		{
			"different lifecycle owns the mark",
			func() map[string]any { return pinnedMark("lifecycle-z", 7, testLVSource(), true) },
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			nil,
		},
		{
			"generation moved since the snapshot",
			func() map[string]any { return pinnedMark("lifecycle-a", 9, testLVSource(), true) },
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			nil,
		},
		{
			"destination lifecycle already retired",
			func() map[string]any {
				m := pinnedMark("lifecycle-a", 7, testLVSource(), true)
				m["endedUIDs"] = []string{"lifecycle-b"}
				return m
			},
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			nil,
		},
		{
			"owner gained the mark without a transfer",
			func() map[string]any { return pinnedMark("lifecycle-b", 3, testLVSource(), true) },
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			nil,
		},
		{
			"volume pins no adopted LV",
			func() map[string]any {
				return map[string]any{"volumeUID": "lifecycle-a", "generation": 7}
			},
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			nil,
		},
		{
			"authorization names another LV",
			func() map[string]any { return pinnedMark("lifecycle-a", 7, testLVSource(), true) },
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, lvSourceWith(otherLVUUID))
			},
			nil,
		},
		{
			"preserve downgrade",
			func() map[string]any { return pinnedMark("lifecycle-a", 7, testLVSource(), true) },
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			func(a *agentv1.RecoveryAuthorization) { a.PreserveOriginal = new(false) },
		},
		{
			"snapshot disagrees with the mark's policy",
			func() map[string]any { return pinnedMark("lifecycle-a", 7, testLVSource(), false) },
			func(t *testing.T, ra recoveryAuthority) *agentv1.RecoverySnapshot {
				return manualSnapshot(t, ra, testLVSource())
			},
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			ra := newRecoveryAuthority(t)
			srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
			seedMark(t, stateDir, tc.mark())
			snap := tc.snap(t, ra)
			auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", tc.auth)
			_, err := transfer(srv, snap, auth)
			requireCode(t, tc.name, err, codes.FailedPrecondition)
		})
	}
}

// The live observation is re-checked under the fencing lock: a busy or
// unknown exclusive claim, local consumers, foreign exports, an LV that
// moved, vanished or cannot be inspected, and a backend without LVInspector
// all refuse — none of them proves the old initiator stopped.
func TestTransferVolumeOwnership_RefusesLiveConsumers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  func(*mockLVBackend)
	}{
		{"exclusive claim busy", func(b *mockLVBackend) { b.claim = backend.ExclusiveClaimBusy }},
		{"exclusive claim unknown", func(b *mockLVBackend) { b.claim = backend.ExclusiveClaimUnknown }},
		{"mounted LV", func(b *mockLVBackend) {
			b.consumers = []backend.DeviceConsumer{{Kind: "mount", Detail: "/var/lib/old"}}
		}},
		{"device-mapper holder", func(b *mockLVBackend) {
			b.consumers = []backend.DeviceConsumer{{Kind: "holder", Detail: "dm-11"}}
		}},
		{"foreign export", func(b *mockLVBackend) {
			b.exports = []backend.DeviceConsumer{{Kind: "nvmet", Detail: "nqn.2003-01.org.other:legacy"}}
		}},
		{"LV replaced", func(b *mockLVBackend) { b.replaceLV() }},
		{"LV missing", func(b *mockLVBackend) { delete(b.lvs, testVolumeID) }},
		{"inspect failure", func(b *mockLVBackend) { b.inspectErr = errors.New("lvs: exit status 5") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			ra := newRecoveryAuthority(t)
			srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
			seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
			snap := snapshotOf(t, srv)
			auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
			tc.set(b)
			_, err := transfer(srv, snap, auth)
			requireCode(t, tc.name, err, codes.FailedPrecondition)
			m := readRecoveryMark(t, stateDir)
			if m.VolumeUID != "lifecycle-a" {
				t.Fatalf("%s moved the mark to %q", tc.name, m.VolumeUID)
			}
		})
	}

	t.Run("backend without LVInspector", func(t *testing.T) {
		t.Parallel()
		ra := newRecoveryAuthority(t)
		srv, stateDir, _ := newRecoveryTestServer(t, newVerifyOnlyLVBackend(), ra)
		seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
		snap := manualSnapshot(t, ra, testLVSource())
		auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
		_, err := transfer(srv, snap, auth)
		requireCode(t, "backend without LVInspector", err, codes.FailedPrecondition)
	})
}

// The old lifecycle's own export still admitting an initiator is live
// evidence and refuses; after DenyInitiator empties the enforced ACL the
// export no longer admits anyone and the transfer commits.
func TestTransferVolumeOwnership_OwnExportAdmissionGatesTransfer(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	ra := newRecoveryAuthority(t)
	srv, stateDir, cfgRoot := newRecoveryTestServer(t, b, ra)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	ctx := context.Background()
	_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testLVDevicePath,
		AclEnabled: true, Fence: token2("lifecycle-a", 7),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	_, err = srv.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: "nqn.host:worker-1", Fence: token2("lifecycle-a", 7),
	})
	if err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}
	snap := snapshotOf(t, srv)
	auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
	_, err = transfer(srv, snap, auth)
	requireCode(t, "export still admitting a host", err, codes.FailedPrecondition)

	_, err = srv.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: "nqn.host:worker-1", Fence: token2("lifecycle-a", 7),
	})
	if err != nil {
		t.Fatalf("DenyInitiator: %v", err)
	}
	resp, err := transfer(srv, snap, auth)
	if err != nil {
		t.Fatalf("transfer after ACL exclusion: %v", err)
	}
	if resp.GetOutcome() != agentv1.TransferOutcome_TRANSFER_OUTCOME_COMMITTED {
		t.Fatalf("outcome = %v, want COMMITTED", resp.GetOutcome())
	}
	if n := nvmetSubsystems(t, cfgRoot); n != 1 {
		t.Fatalf("the ACL-excluded export was removed (subsystems %d)", n)
	}
}

// A configured own export is not a live consumer when local attach has
// disabled its namespace.  The backend may still report the configured export,
// so recovery must distinguish it from a foreign export before checking
// admission state.
func TestTransferVolumeOwnership_AllowsStoppedOwnExport(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	ra := newRecoveryAuthority(t)
	srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	ctx := context.Background()
	fence := token2("lifecycle-a", 7)
	_, err := srv.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testLVDevicePath,
		AclEnabled: true, Fence: fence,
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	if _, err = srv.SetLocalAttach(ctx, &agentv1.SetLocalAttachRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Local: true, Fence: fence,
	}); err != nil {
		t.Fatalf("SetLocalAttach: %v", err)
	}
	ownID := testVolumeNQN
	b.exports = []backend.DeviceConsumer{{Kind: "export", Detail: ownID}}
	snap := snapshotOf(t, srv)
	if len(snap.GetExports()) != 1 || snap.GetExports()[0].GetNamespaceEnabled() {
		t.Fatalf("snapshot exports = %v, want one disabled own export", snap.GetExports())
	}
	auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)
	resp, err := transfer(srv, snap, auth)
	if err != nil {
		t.Fatalf("transfer with stopped own export: %v", err)
	}
	if resp.GetOutcome() != agentv1.TransferOutcome_TRANSFER_OUTCOME_COMMITTED {
		t.Fatalf("outcome = %v, want COMMITTED", resp.GetOutcome())
	}
}

// A failure before the rename leaves the mark byte-identical and keeps the
// volume with its owner.  (The post-rename fsync injection lives in
// server_recovery_internal_test.go.)
func TestTransferVolumeOwnership_PreCommitFailureWritesNothing(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	ra := newRecoveryAuthority(t)
	srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
	seeded := seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	snap := snapshotOf(t, srv)
	auth := transferAuthorization(t, snap, ra.opKey, "lifecycle-b", nil)

	// A directory where the mark's temp file must go makes the write fail
	// before the rename.
	if err := os.Mkdir(markFilePath(stateDir)+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	_, err := transfer(srv, snap, auth)
	requireCode(t, "pre-commit write failure", err, codes.Internal)
	data, err := os.ReadFile(markFilePath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, seeded) {
		t.Fatal("a refused transfer rewrote the mark")
	}
}

// InspectVolume signs when the agent is configured, the caller is a
// verified mTLS peer, and the mark records a live lifecycle pinned to the
// observed LV.
func TestInspectVolume_SignsEligibleSnapshot(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	ra := newRecoveryAuthority(t)
	srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 7, testLVSource(), true))
	resp, err := srv.InspectVolume(recoveryCtx(), inspectRequest())
	if err != nil {
		t.Fatalf("InspectVolume: %v", err)
	}
	snap := resp.GetSnapshot()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	if snap.GetOldVolumeUid() != "lifecycle-a" || snap.GetOldGeneration() != 7 ||
		snap.GetAgentIdentity() != "pillar-agent.test" || !snap.GetPreserveOriginal() ||
		snap.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM ||
		!proto.Equal(snap.GetLvmSource(), testLVSource()) {
		t.Fatalf("snapshot = %v, want lifecycle-a/7 preserved attested by pillar-agent.test", snap)
	}
	if snap.GetFence().GetVolumeUid() != "lifecycle-a" || snap.GetFence().GetGeneration() != 7 {
		t.Fatalf("snapshot fence = %v, want the observed mark", snap.GetFence())
	}
	if err := recoveryauth.VerifySnapshotWithCertificate(snap, ra.cert, time.Now()); err != nil {
		t.Fatalf("snapshot does not verify: %v", err)
	}
}

// InspectVolume signs only when the agent is configured, the caller is a
// verified mTLS peer, and the mark records a live lifecycle pinned to the
// observed LV; every other report is unsigned.
func TestInspectVolume_SignedSnapshotEligibility(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(*testing.T) (*agent.Server, string)
		mark  func() map[string]any
		ctx   func() context.Context
	}{
		{
			"agent not configured",
			func(t *testing.T) (*agent.Server, string) {
				b := newMockLVBackend()
				srv, stateDir, _ := newLVTestServer(t, b)
				return srv, stateDir
			},
			func() map[string]any { return pinnedMark("lifecycle-a", 7, testLVSource(), true) },
			recoveryCtx,
		},
		{
			"plaintext caller",
			func(t *testing.T) (*agent.Server, string) {
				b := newMockLVBackend()
				ra := newRecoveryAuthority(t)
				srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
				return srv, stateDir
			},
			func() map[string]any { return pinnedMark("lifecycle-a", 7, testLVSource(), true) },
			context.Background,
		},
		{
			"ended mark",
			func(t *testing.T) (*agent.Server, string) {
				b := newMockLVBackend()
				ra := newRecoveryAuthority(t)
				srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
				return srv, stateDir
			},
			func() map[string]any { return endedPinnedMark(7, testLVSource(), true) },
			recoveryCtx,
		},
		{
			"unpinned mark",
			func(t *testing.T) (*agent.Server, string) {
				b := newMockLVBackend()
				ra := newRecoveryAuthority(t)
				srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
				return srv, stateDir
			},
			func() map[string]any { return map[string]any{"volumeUID": "lifecycle-a", "generation": 7} },
			recoveryCtx,
		},
		{
			"observed LV differs from the pin",
			func(t *testing.T) (*agent.Server, string) {
				b := newMockLVBackend()
				b.replaceLV()
				ra := newRecoveryAuthority(t)
				srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
				return srv, stateDir
			},
			func() map[string]any { return pinnedMark("lifecycle-a", 7, testLVSource(), true) },
			recoveryCtx,
		},
		{
			"no mark",
			func(t *testing.T) (*agent.Server, string) {
				b := newMockLVBackend()
				ra := newRecoveryAuthority(t)
				srv, stateDir, _ := newRecoveryTestServer(t, b, ra)
				return srv, stateDir
			},
			nil,
			recoveryCtx,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, stateDir := tc.setup(t)
			if tc.mark != nil {
				seedMark(t, stateDir, tc.mark())
			}
			resp, err := srv.InspectVolume(tc.ctx(), inspectRequest())
			if err != nil {
				t.Fatalf("InspectVolume: %v", err)
			}
			if resp.GetSnapshot() != nil {
				t.Fatalf("%s issued a signed snapshot: %v", tc.name, resp.GetSnapshot())
			}
		})
	}
}
