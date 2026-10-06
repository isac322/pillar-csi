package agent

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/recoveryauth"
)

// recoveryFakeBackend is the minimal LVM backend a transfer needs: an
// LVInspector reporting an idle pinned LV.  Every other VolumeBackend
// method is unused by the transfer path.
type recoveryFakeBackend struct {
	backend.VolumeBackend
	obs backend.LVObservation
	err error
}

func (*recoveryFakeBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_LVM
}

func (b *recoveryFakeBackend) InspectLV(_ context.Context, _ string) (backend.LVObservation, error) {
	return b.obs, b.err
}

var _ backend.LVInspector = (*recoveryFakeBackend)(nil)

var recoveryTestIdentity = backend.LVMIdentity{
	VolumeGroup:       "tank",
	LogicalVolume:     "pvc-xyz",
	VolumeGroupUUID:   "VGuuid-test",
	LogicalVolumeUUID: "LVuuid-test",
}

// recoveryInternalFixture holds the keys, certificate and server of one
// recovery-enabled agent on a fake LVM backend.
type recoveryInternalFixture struct {
	srv      *Server
	stateDir string
	signer   *ecdsa.PrivateKey
	cert     *x509.Certificate
	opKey    *ecdsa.PrivateKey
}

func newRecoveryInternalFixture(t *testing.T) *recoveryInternalFixture {
	t.Helper()
	signer, cert := recoveryInternalCertificate(t, "pillar-agent.test")
	opKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	b := &recoveryFakeBackend{obs: backend.LVObservation{
		Identity:       recoveryTestIdentity,
		Active:         true,
		ExclusiveClaim: backend.ExclusiveClaimFree,
	}}
	srv := NewServer(map[string]backend.VolumeBackend{"tank": b}, t.TempDir(),
		WithDrainStateDir(stateDir),
		WithRecoveryAuthority(signer, cert, []crypto.PublicKey{&opKey.PublicKey}))
	return &recoveryInternalFixture{srv: srv, stateDir: stateDir, signer: signer, cert: cert, opKey: opKey}
}

func recoveryInternalCertificate(t *testing.T, cn string) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
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

// verifiedPeer is a context whose peer verified an mTLS client chain.
func verifiedPeer() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}},
		},
	})
}

// recoveryRequest builds a valid signed snapshot/authorization pair moving
// fencingTestVolume from lifecycle-a generation 7 to lifecycle-b generation 1.
func (fx *recoveryInternalFixture) recoveryRequest(
	t *testing.T,
	newUID string,
	newGen uint64,
) *agentv1.TransferVolumeOwnershipRequest {
	t.Helper()
	snap := &agentv1.RecoverySnapshot{
		VolumeId:    fencingTestVolume,
		BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
		LvmSource: &agentv1.LvmSourceIdentity{
			VolumeGroup:       recoveryTestIdentity.VolumeGroup,
			LogicalVolume:     recoveryTestIdentity.LogicalVolume,
			VolumeGroupUuid:   recoveryTestIdentity.VolumeGroupUUID,
			LogicalVolumeUuid: recoveryTestIdentity.LogicalVolumeUUID,
		},
		OldVolumeUid:     "lifecycle-a",
		OldGeneration:    7,
		PreserveOriginal: true,
		ExclusiveClaim:   backend.ExclusiveClaimFree,
		Fence: &agentv1.FenceObservation{
			Exists:     true,
			VolumeUid:  "lifecycle-a",
			Generation: 7,
		},
		AgentIdentity: "pillar-agent.test",
		IssuedAt:      timestamppb.Now(),
	}
	if err := recoveryauth.SignSnapshot(snap, fx.signer); err != nil {
		t.Fatalf("sign snapshot: %v", err)
	}
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
		NewGeneration:    newGen,
		PreserveOriginal: new(true),
		IssuedAt:         timestamppb.Now(),
		ExpiresAt:        timestamppb.New(time.Now().Add(time.Hour)),
	}
	if err := recoveryauth.SignAuthorization(auth, fx.opKey); err != nil {
		t.Fatalf("sign authorization: %v", err)
	}
	return &agentv1.TransferVolumeOwnershipRequest{Snapshot: snap, Authorization: auth}
}

// A post-rename fsync failure makes the committed mark's durability unknown:
// the RPC reports UNKNOWN instead of refusing, nothing is rolled back, and a
// retry answers from whatever the mark actually records.
func TestTransferVolumeOwnership_PostRenameFailureReportsUnknown(t *testing.T) {
	fx := newRecoveryInternalFixture(t)
	req := fx.recoveryRequest(t, "lifecycle-b", 1)
	mark := fencingMark{
		VolumeUID:        "lifecycle-a",
		Generation:       7,
		LVMSource:        markLVMSourceFromProto(req.GetSnapshot().GetLvmSource()),
		PreserveOriginal: true,
	}
	if err := fx.srv.writeFencingMark(fencingTestVolume, mark); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("fsync: input/output error")
	old := fencingMarkSyncDirs
	fencingMarkSyncDirs = func(*os.Root) error { return injected }
	defer func() { fencingMarkSyncDirs = old }()

	resp, err := fx.srv.TransferVolumeOwnership(verifiedPeer(), req)
	if err != nil {
		t.Fatalf("post-rename failure must not error, got %v", err)
	}
	if resp.GetOutcome() != agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN {
		t.Fatalf("outcome = %v, want UNKNOWN", resp.GetOutcome())
	}

	// The rename did apply on this filesystem: the mark records the
	// transferred lifecycle.  Nothing rolled it back.
	stored, exists, err := fx.srv.readFencingMark(fencingTestVolume)
	if err != nil || !exists {
		t.Fatalf("mark after UNKNOWN outcome: exists=%v err=%v", exists, err)
	}
	if stored.VolumeUID != "lifecycle-b" || stored.Transfer == nil {
		t.Fatalf("mark after UNKNOWN outcome %+v, want the transferred lifecycle", stored)
	}

	// A retry while directory sync is still failing must remain UNKNOWN:
	// visibility of the rename is not durable acknowledgement.
	resp, err = fx.srv.TransferVolumeOwnership(verifiedPeer(), req)
	if err != nil {
		t.Fatalf("retry during continued fsync failure: %v", err)
	}
	if resp.GetOutcome() != agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN {
		t.Fatalf("retry during fsync failure = %v, want UNKNOWN", resp.GetOutcome())
	}

	// Once directory sync recovers, the next identical retry is idempotently
	// acknowledged as already committed.
	fencingMarkSyncDirs = old
	resp, err = fx.srv.TransferVolumeOwnership(verifiedPeer(), req)
	if err != nil {
		t.Fatalf("retry after fsync recovery: %v", err)
	}
	if resp.GetOutcome() != agentv1.TransferOutcome_TRANSFER_OUTCOME_ALREADY_COMMITTED {
		t.Fatalf("retry after fsync recovery = %v, want ALREADY_COMMITTED", resp.GetOutcome())
	}
}

// A failure before the rename refuses with an error, leaves the old mark
// intact, and is retryable once the cause is gone.
func TestTransferVolumeOwnership_PreRenameFailureRefuses(t *testing.T) {
	fx := newRecoveryInternalFixture(t)
	req := fx.recoveryRequest(t, "lifecycle-b", 1)
	mark := fencingMark{
		VolumeUID:        "lifecycle-a",
		Generation:       7,
		LVMSource:        markLVMSourceFromProto(req.GetSnapshot().GetLvmSource()),
		PreserveOriginal: true,
	}
	if err := fx.srv.writeFencingMark(fencingTestVolume, mark); err != nil {
		t.Fatal(err)
	}

	// Break the temp-file creation: a directory occupying the mark's .tmp
	// sibling makes writeFileSynced fail before the rename.
	root, err := fx.srv.openFencingRoot()
	if err != nil {
		t.Fatal(err)
	}
	name := "generations/" + fencingFilename(fencingTestVolume) + ".tmp"
	mkdirErr := root.Mkdir(name, 0o750)
	closeErr := root.Close()
	if mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if closeErr != nil {
		t.Fatalf("close fencing root: %v", closeErr)
	}

	_, err = fx.srv.TransferVolumeOwnership(verifiedPeer(), req)
	if code := status.Code(err); code != codes.Internal {
		t.Fatalf("pre-rename failure: code %v (err %v), want Internal", code, err)
	}
	stored, exists, err := fx.srv.readFencingMark(fencingTestVolume)
	if err != nil || !exists {
		t.Fatalf("mark after pre-commit refusal: exists=%v err=%v", exists, err)
	}
	if stored.VolumeUID != "lifecycle-a" || stored.Generation != 7 || stored.Transfer != nil {
		t.Fatalf("pre-commit refusal changed the mark to %+v", stored)
	}
}
