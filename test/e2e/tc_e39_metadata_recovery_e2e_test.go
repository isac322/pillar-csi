//go:build e2e && e2e_helm

package e2e

// tc_e39_metadata_recovery_e2e_test.go - E39: recovering an adopted LVM
// volume whose Kubernetes/CSI metadata (PVC, PV and PillarVolumeState) was
// lost while the agent's durable fence mark and the LV survived, through
// the operator-authorized TransferVolumeOwnership flow on a real
// multi-node Kind cluster with the Helm-installed pillar-csi images.
//
// Authority is two signatures and nothing else: the agent signs a
// RecoverySnapshot of what it observed with its own server TLS key, and the
// operator signs a RecoveryAuthorization over that snapshot's digest with a
// key whose public half is the agent's --recovery-trust-anchor.  The test
// provisions ephemeral mTLS credentials and an ephemeral operator key,
// upgrades the Helm release to mTLS plus the trust anchor, and rolls it back
// at the end.  The cases prove that:
//   - InspectVolume over verified mTLS returns a snapshot signed by the
//     agent's server certificate; plaintext and client-certificate-less
//     callers are refused at the transport;
//   - a snapshot of a live, exported, ACL-granted volume and a local
//     consumer on the storage node refuse the transfer;
//   - missing, untrusted, expired, future, over-long, tampered,
//     wrong-generation and wrong-snapshot grants all refuse without
//     changing the LV bytes or the fence mark;
//   - the exact operator workflow (plain claim + RecoveryPending
//     PillarVolumeState + signed grant) commits the transfer, a replacement
//     workload reads the original data, and a RecoveryPending record whose
//     claim never existed is neither reaped nor served;
//   - after the commit, an exact replay is answered ALREADY_COMMITTED, while
//     a stale pre-loss snapshot, a different destination, a different grant
//     for the same destination and the retired lifecycle's token refuse.
//
// The lane is dedicated and Serial: it rewrites the shared Helm release.
// A missing prerequisite fails the spec; nothing is skipped.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/recoveryauth"
	"github.com/isac322/pillar-csi/internal/testutil/testcerts"
	"github.com/isac322/pillar-csi/internal/tlscreds"
)

const (
	// e39AgentServerName is the DNS SAN and Subject CN of the ephemeral
	// agent server certificate; it is also the agent_identity every
	// RecoverySnapshot the agent signs must carry.
	e39AgentServerName = "pillar-agent"
	// e39TrustAnchorPath is where the operator public key is written on
	// every Kind node.  It lies below the chart's agent-state hostPath, so
	// the agent container reads it at the same path.
	e39TrustAnchorPath = e38AgentStateDir + "/e39-recovery-trust-anchor.pem"
	// e39AnnRecoverySnapshot carries base64(deterministic proto) of the
	// agent-signed RecoverySnapshot the operator's grant names (wire
	// contract shared with internal/csi/recovery.go).
	e39AnnRecoverySnapshot = "pillar-csi.bhyoo.com/recovery-snapshot"
	// e39AgentSecret / e39ControllerSecret are the chart's default
	// mtls.secretRefs names (keys tls.crt, tls.key, ca.crt).
	e39AgentSecret      = "pillar-agent-mtls"
	e39ControllerSecret = "pillar-controller-mtls"
	// e39BackendToken / e39ProtocolToken are the PillarVolumeState routing
	// tokens of an LVM volume exported over NVMe-oF/TCP.
	e39BackendToken  = "lvm-lv"
	e39ProtocolToken = "nvmeof-tcp"
)

// e39FailIfNoInfra fails the spec when the E39 environment is incomplete.
func e39FailIfNoInfra() {
	e38FailIfNoInfra()
	if _, err := exec.LookPath("helm"); err != nil {
		Fail("[E39] MISSING PREREQUISITE: the helm binary is not on PATH; E39 upgrades the " +
			"suite's pillar-csi release to mTLS plus --recovery-trust-anchor and rolls it back.")
	}
}

// e39HelmRelease names the single pillar-csi Helm release in the suite
// namespace and its current revision.
func e39HelmRelease(ctx context.Context) (name string, revision int, err error) {
	ns := resolveHelmNamespace()
	out, _, err := e27HelmOutput(ctx, "list", "-n", ns, "-o", "json")
	if err != nil {
		return "", 0, err
	}
	var list []struct {
		Name     string `json:"name"`
		Chart    string `json:"chart"`
		Revision string `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return "", 0, fmt.Errorf("decode helm list: %w", err)
	}
	var found []string
	for _, r := range list {
		if strings.HasPrefix(r.Chart, "pillar-csi-") && r.Status == "deployed" {
			found = append(found, r.Name)
			name = r.Name
			revision, err = strconv.Atoi(r.Revision)
			if err != nil {
				return "", 0, fmt.Errorf("helm list revision %q: %w", r.Revision, err)
			}
		}
	}
	if len(found) != 1 {
		return "", 0, fmt.Errorf("want exactly one deployed pillar-csi release in %s, got %v", ns, found)
	}
	return name, revision, nil
}

// e39ReleaseValues reads the user-supplied values of release that E39
// changes: mtls.enabled and agent.extraArgs.
func e39ReleaseValues(ctx context.Context, release string) (mtls bool, extraArgs []string, err error) {
	out, _, err := e27HelmOutput(ctx, "get", "values", release, "-n", resolveHelmNamespace(), "-o", "json")
	if err != nil {
		return false, nil, err
	}
	var vals struct {
		MTLS *struct {
			Enabled bool `json:"enabled"`
		} `json:"mtls"`
		Agent *struct {
			ExtraArgs []string `json:"extraArgs"`
		} `json:"agent"`
	}
	if trimmed := strings.TrimSpace(out); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &vals); err != nil {
			return false, nil, fmt.Errorf("decode helm values: %w", err)
		}
	}
	if vals.MTLS != nil {
		mtls = vals.MTLS.Enabled
	}
	if vals.Agent != nil {
		extraArgs = vals.Agent.ExtraArgs
	}
	return mtls, extraArgs, nil
}

// e39TLSSecretManifest renders a kubernetes.io/tls Secret in the chart's
// mtls.secretRefs layout.
func e39TLSSecretManifest(name, namespace string, cert, key, ca []byte) string {
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
  ca.crt: %s
`, name, namespace, enc(cert), enc(key), enc(ca))
}

// e39KindNodes lists the Kubernetes node names, which in Kind are also the
// node container names.
func e39KindNodes(ctx context.Context) ([]string, error) {
	out, err := e36Kubectl(ctx, "", "get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, err
	}
	nodes := strings.Fields(out)
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes listed")
	}
	return nodes, nil
}

// e39PlainPVCManifest renders a claim without any import annotation: the
// recovery record, not the claim, pins the source.
func e39PlainPVCManifest(name, namespace, storageClass string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: %s
  resources:
    requests:
      storage: %s
`, name, namespace, storageClass, e38PVCRequest)
}

// e39ForceDelete deletes a (possibly cluster-scoped) object and strips its
// finalizers: the metadata loss E39 simulates, and the cleanup of records no
// controller is meant to end.  The delete comes first so no finalizer can be
// re-added after the strip; the wait is bounded.
func e39ForceDelete(ctx context.Context, kind, name, namespace string) error {
	scope := []string{}
	if namespace != "" {
		scope = []string{"-n", namespace}
	}
	out, err := e36Kubectl(ctx, "", append([]string{"get", kind, name, "--ignore-not-found=true", "-o", "name"}, scope...)...)
	if err != nil {
		return fmt.Errorf("get %s %s: %w", kind, name, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	if _, err := e36Kubectl(ctx, "", append([]string{"delete", kind, name, "--ignore-not-found=true",
		"--wait=false"}, scope...)...); err != nil {
		return fmt.Errorf("delete %s %s: %w", kind, name, err)
	}
	out, err = e36Kubectl(ctx, "", append([]string{"get", kind, name, "--ignore-not-found=true", "-o", "name"}, scope...)...)
	if err != nil {
		return fmt.Errorf("get %s %s: %w", kind, name, err)
	}
	if strings.TrimSpace(out) != "" {
		if _, err := e36Kubectl(ctx, "", append([]string{"patch", kind, name, "--type=merge",
			"-p", `{"metadata":{"finalizers":null}}`}, scope...)...); err != nil &&
			!strings.Contains(err.Error(), "NotFound") {
			return fmt.Errorf("strip finalizers of %s %s: %w", kind, name, err)
		}
	}
	if _, err := e36Kubectl(ctx, "", append([]string{"wait", "--for=delete", kind + "/" + name,
		"--timeout=90s"}, scope...)...); err != nil && !strings.Contains(err.Error(), "NotFound") {
		return fmt.Errorf("wait for %s %s to be deleted: %w", kind, name, err)
	}
	return nil
}

// e39WaitGone polls until the PV and the PillarVolumeState named pvName
// are both gone, returning an error naming what is left at the deadline.
func e39WaitGone(ctx context.Context, pvName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pv, err := e38GetPV(ctx, pvName)
		if err != nil {
			return fmt.Errorf("get PV %s: %w", pvName, err)
		}
		pvs, err := e38GetPVS(ctx, pvName)
		if err != nil {
			return fmt.Errorf("get PillarVolumeState %s: %w", pvName, err)
		}
		if pv == nil && pvs == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: PV %s present=%t (phase %v), PillarVolumeState present=%t",
				timeout, pvName, pv != nil, func() any {
					if pv == nil {
						return ""
					}
					return pv.Status.Phase
				}(), pvs != nil)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for PV/PillarVolumeState %s: %w", pvName, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// e39StorageClassBlockers lists this run's objects that keep the
// PillarStorageClass, PillarStore or PillarProtocol from being deleted:
// PVCs of the run's namespace or StorageClass; PVs of the StorageClass,
// bound to the namespace, or whose volume handle names the test LV; and the
// agent's PillarVolumeStates that the run created, that name the test LV,
// whose claim lived in the run's namespace, or whose same-named PV uses the
// StorageClass (the attribution the PillarStorageClass finalizer applies).
// Nothing outside that scope is ever listed.
func e39StorageClassBlockers(ctx context.Context, sc e39RunScope) (e39Leftovers, error) {
	var left e39Leftovers
	var pvcs corev1.PersistentVolumeClaimList
	if _, err := e38GetJSON(ctx, &pvcs, "pvc", "--all-namespaces"); err != nil {
		return left, fmt.Errorf("list PVCs: %w", err)
	}
	for _, c := range pvcs.Items {
		if c.Namespace == sc.namespace || (c.Spec.StorageClassName != nil && *c.Spec.StorageClassName == sc.scName) {
			left.pvcs = append(left.pvcs, [2]string{c.Namespace, c.Name})
		}
	}
	var pvList corev1.PersistentVolumeList
	if _, err := e38GetJSON(ctx, &pvList, "pv"); err != nil {
		return left, fmt.Errorf("list PVs: %w", err)
	}
	pvClass := map[string]string{}
	for _, p := range pvList.Items {
		pvClass[p.Name] = p.Spec.StorageClassName
		ofRun := p.Spec.StorageClassName == sc.scName ||
			(p.Spec.ClaimRef != nil && p.Spec.ClaimRef.Namespace == sc.namespace) ||
			(sc.volumeID != "" && p.Spec.CSI != nil && strings.HasSuffix(p.Spec.CSI.VolumeHandle, "/"+sc.volumeID))
		if ofRun {
			left.pvs = append(left.pvs, p.Name)
		}
	}
	records, err := e38ListPVS(ctx)
	if err != nil {
		return left, fmt.Errorf("list PillarVolumeStates: %w", err)
	}
	for _, r := range records {
		if r.Spec.AgentRef != sc.agentName {
			continue
		}
		ofRun := slices.Contains(sc.records, r.Name) ||
			(sc.volumeID != "" && r.Spec.AgentVolumeID == sc.volumeID) ||
			(r.Spec.ClaimRef != nil && r.Spec.ClaimRef.Namespace == sc.namespace) ||
			pvClass[r.Name] == sc.scName
		if !ofRun {
			continue
		}
		left.records = append(left.records, r.Name)
		// A record of this run naming another LV was provisioned empty
		// for a test claim (e.g. a cleanup claim that found no PV to
		// bind); its LV is a test artifact under the run's VG.
		if r.Spec.AgentVolumeID != sc.volumeID && sc.vg != "" &&
			strings.HasPrefix(r.Spec.AgentVolumeID, sc.vg+"/pvc-") {
			left.extraLVs = append(left.extraLVs, r.Spec.AgentVolumeID)
		}
	}
	return left, nil
}

// e39RunScope bounds every cleanup sweep to this run's objects: its test
// namespace, its StorageClass, its PillarAgent and LV, and the record
// names the run created.
type e39RunScope struct {
	namespace, scName, agentName, volumeID, vg string
	records                                    []string
}

// e39Leftovers are the run's objects that keep the PillarStorageClass,
// PillarStore or PillarProtocol from being deleted.
type e39Leftovers struct {
	pvcs     [][2]string // namespace, name
	pvs      []string
	records  []string
	extraLVs []string // "<vg>/<lv>" provisioned empty for test claims
}

func (l e39Leftovers) empty() bool {
	return len(l.pvcs) == 0 && len(l.pvs) == 0 && len(l.records) == 0
}

func (l e39Leftovers) String() string {
	return fmt.Sprintf("pvcs=%v pvs=%v pillarvolumestates=%v lvs=%v", l.pvcs, l.pvs, l.records, l.extraLVs)
}

// e39Sweep force-removes the run's leftover claims, PVs and records, in
// that order, until none is listed or timeout passes; LVs provisioned
// empty for test claims are removed once their record is gone.  It
// returns what it found first (empty when the normal release path left
// nothing) and an error naming what is still present at the deadline.
func e39Sweep(ctx context.Context, storageNode string, sc e39RunScope, timeout time.Duration) (e39Leftovers, error) {
	first, err := e39StorageClassBlockers(ctx, sc)
	if err != nil {
		return first, err
	}
	deadline := time.Now().Add(timeout)
	cur := first
	lvs := map[string]bool{}
	for {
		var errs []error
		for _, c := range cur.pvcs {
			if err := e39ForceDelete(ctx, "pvc", c[1], c[0]); err != nil {
				errs = append(errs, err)
			}
		}
		for _, pv := range cur.pvs {
			if err := e39ForceDelete(ctx, "pv", pv, ""); err != nil {
				errs = append(errs, err)
			}
		}
		for _, r := range cur.records {
			if err := e39ForceDelete(ctx, "pillarvolumestate", r, ""); err != nil {
				errs = append(errs, err)
			}
		}
		for _, lv := range cur.extraLVs {
			lvs[lv] = true
		}
		next, lerr := e39StorageClassBlockers(ctx, sc)
		if lerr != nil {
			return first, lerr
		}
		if next.empty() {
			for lv := range lvs {
				vg, name, _ := strings.Cut(lv, "/")
				if err := e38RemoveLV(ctx, storageNode, vg, name); err != nil {
					errs = append(errs, fmt.Errorf("remove test-provisioned LV %s: %w", lv, err))
				}
			}
			return first, errors.Join(errs...)
		}
		if time.Now().After(deadline) {
			errs = append(errs, fmt.Errorf("after %s still present: %s", timeout, next))
			return first, errors.Join(errs...)
		}
		cur = next
		select {
		case <-ctx.Done():
			return first, fmt.Errorf("sweep: %w (still present: %s)", ctx.Err(), next)
		case <-time.After(5 * time.Second):
		}
	}
}

// e39CreatePVS creates a cluster-scoped PillarVolumeState from pvs and
// returns it as stored (with its server-assigned metadata.uid).
func e39CreatePVS(ctx context.Context, pvs *pillarv1.PillarVolumeState) (*pillarv1.PillarVolumeState, error) {
	pvs.TypeMeta = metav1.TypeMeta{APIVersion: "pillar-csi.bhyoo.com/v1alpha1", Kind: "PillarVolumeState"}
	raw, err := json.Marshal(pvs)
	if err != nil {
		return nil, err
	}
	if _, err := e36Kubectl(ctx, string(raw), "create", "-f", "-"); err != nil {
		return nil, err
	}
	stored, err := e38GetPVS(ctx, pvs.Name)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("PillarVolumeState %s vanished after create", pvs.Name)
	}
	return stored, nil
}

// e39RecoveryPatch is the operator's second step: the write-once latch
// fields (destination UID, grant, grant digest) plus the snapshot the grant
// names, applied in one merge patch.
func e39RecoveryPatch(ctx context.Context, pvsName string, snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization, newUID string) error {
	snapRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(snap)
	if err != nil {
		return err
	}
	authRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(auth)
	if err != nil {
		return err
	}
	digest, err := recoveryauth.AuthorizationDigest(auth)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				e39AnnRecoverySnapshot: base64.StdEncoding.EncodeToString(snapRaw),
			},
		},
		"spec": map[string]any{
			"recovery": map[string]any{
				"newVolumeUID":        newUID,
				"authorization":       base64.StdEncoding.EncodeToString(authRaw),
				"authorizationDigest": recoveryauth.DigestHex(digest),
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = e36Kubectl(ctx, "", "patch", "pillarvolumestate", pvsName, "--type=merge", "-p", string(patch))
	return err
}

// e39Grant describes one operator authorization over snapshot snap.
type e39Grant struct {
	newUID   string
	newGen   uint64
	preserve *bool
	issued   time.Time
	expires  time.Time
	// mutate edits the payload before signing (refusal cases).
	mutate func(*agentv1.RecoveryAuthorization)
}

// e39SignGrant builds and signs g over snap with key.
func e39SignGrant(snap *agentv1.RecoverySnapshot, g e39Grant, key *ecdsa.PrivateKey) *agentv1.RecoveryAuthorization {
	GinkgoHelper()
	digest, err := recoveryauth.SnapshotDigest(snap)
	Expect(err).NotTo(HaveOccurred(), "snapshot digest")
	auth := &agentv1.RecoveryAuthorization{
		SnapshotDigest:   digest[:],
		VolumeId:         snap.GetVolumeId(),
		BackendType:      snap.GetBackendType(),
		LvmSource:        proto.Clone(snap.GetLvmSource()).(*agentv1.LvmSourceIdentity),
		OldVolumeUid:     snap.GetOldVolumeUid(),
		OldGeneration:    snap.GetOldGeneration(),
		NewVolumeUid:     g.newUID,
		NewGeneration:    g.newGen,
		PreserveOriginal: g.preserve,
		IssuedAt:         timestamppb.New(g.issued),
		ExpiresAt:        timestamppb.New(g.expires),
	}
	if g.mutate != nil {
		g.mutate(auth)
	}
	Expect(recoveryauth.SignAuthorization(auth, key)).To(Succeed(), "sign authorization")
	return auth
}

var _ = Describe("E39: 메타데이터 유실 후 서명 기반 LVM 볼륨 소유권 복구 (Kind 클러스터 E2E)",
	Label("lvm", "recovery", "e39"), Serial,
	func() {
		Describe("E39.1 에이전트 서명 스냅샷 + 운영자 승인으로 유실된 import-lv 볼륨 복구", Ordered, func() {
			var (
				proc  = GinkgoParallelProcess()
				nonce = strings.ToLower(agentFenceRunNonce[:6])

				storageNode string
				workers     []string
				namespace   string
				agentName   string
				linearVG    string

				// Cluster-scoped objects carry the run nonce so a rerun never
				// collides with state an aborted run left behind.
				storeName    = fmt.Sprintf("e39-linear-%d-%s", proc, nonce)
				protocolName = fmt.Sprintf("e39-proto-%d-%s", proc, nonce)
				pscName      = fmt.Sprintf("e39-retain-%d-%s", proc, nonce)
				scName       = fmt.Sprintf("e39-lvm-retain-%d-%s", proc, nonce)
				busyMnt      = fmt.Sprintf("/tmp/e39-busy-%d-%s", proc, nonce)

				pvcOrig     = fmt.Sprintf("e39-orig-%d", proc)
				pvcRecover  = fmt.Sprintf("e39-recover-%d", proc)
				podOrig     = fmt.Sprintf("e39-orig-reader-%d", proc)
				podRecover  = fmt.Sprintf("e39-recover-reader-%d", proc)
				decoyName   = fmt.Sprintf("pvc-e39d%04d-%s-4000-8000-000000000000", proc, nonce[:4])
				decoyUID    = fmt.Sprintf("e39-decoy-claim-%d-%s", proc, nonce)
				decoyClaim  = fmt.Sprintf("e39-decoy-missing-%d", proc)
				fx          *e38Fixture
				bundle      *testcerts.Bundle
				serverCert  *x509.Certificate
				operatorKey *ecdsa.PrivateKey
				rogueKey    *ecdsa.PrivateKey
				agentAddr   string
				agent       agentv1.AgentServiceClient

				controllerDeploy   string
				controllerReplicas int32
				controllerScaled   bool

				// Lifecycle facts carried across the ordered cases.
				oldPVName    string
				oldPVS       *pillarv1.PillarVolumeState
				oldGen       uint64
				healthySnap  *agentv1.RecoverySnapshot
				lossSnap     *agentv1.RecoverySnapshot
				marksAtLoss  string
				recoverPVS   *pillarv1.PillarVolumeState
				committedReq *agentv1.TransferVolumeOwnershipRequest
				trackedPVs   []string
				trackedPVS   []string
			)

			// marksUnchanged asserts the fence mark files of the LV still
			// equal want.
			marksUnchanged := func(ctx context.Context, tc, want string) {
				GinkgoHelper()
				got, err := e38MarkFiles(ctx, storageNode, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[%s] read mark files", tc)
				Expect(got).To(Equal(want), "[%s] the fence mark must be unchanged", tc)
			}

			// freshSnapshot inspects the LV over mTLS and verifies the signed
			// snapshot against the agent's server certificate.
			freshSnapshot := func(ctx context.Context, tc string) (*agentv1.InspectVolumeResponse, *agentv1.RecoverySnapshot) {
				GinkgoHelper()
				resp, err := e38Inspect(ctx, agent, fx.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[%s] InspectVolume over mTLS", tc)
				snap := resp.GetSnapshot()
				Expect(snap).NotTo(BeNil(), "[%s] a recovery-configured agent signs a snapshot for a verified caller", tc)
				Expect(recoveryauth.VerifySnapshotWithCertificate(snap, serverCert, time.Now())).
					To(Succeed(), "[%s] the snapshot verifies against the agent's server certificate", tc)
				Expect(snap.GetAgentIdentity()).To(Equal(e39AgentServerName), "[%s] agent identity", tc)
				return resp, snap
			}

			grantFor := func(snap *agentv1.RecoverySnapshot, newUID string) e39Grant {
				now := time.Now()
				return e39Grant{
					newUID:   newUID,
					newGen:   snap.GetOldGeneration() + 1,
					preserve: proto.Bool(true),
					issued:   now.Add(-time.Minute),
					expires:  now.Add(30 * time.Minute),
				}
			}

			transfer := func(ctx context.Context, snap *agentv1.RecoverySnapshot, auth *agentv1.RecoveryAuthorization) (*agentv1.TransferVolumeOwnershipResponse, error) {
				return agent.TransferVolumeOwnership(ctx, &agentv1.TransferVolumeOwnershipRequest{
					Snapshot: snap, Authorization: auth,
				})
			}

			BeforeAll(func() {
				e39FailIfNoInfra()
				storageNode = os.Getenv(suiteBackendContainerEnvVar)
				namespace = fmt.Sprintf("e39-lvm-%d-%s", proc, nonce)
				helmNS := resolveHelmNamespace()

				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
				defer cancel()

				By("checking storage-node tools")
				_, err := e36NodeSh(ctx, storageNode,
					"for c in lvs vgs lvcreate lvremove mkfs.ext4 mount umount mountpoint dd sha256sum stat find awk base64; do "+
						"command -v $c >/dev/null || { echo missing $c; exit 1; }; done")
				Expect(err).NotTo(HaveOccurred(), "[E39] MISSING PREREQUISITE: storage node %s lacks LVM/e2fsprogs/coreutils tools", storageNode)

				By("picking a schedulable worker for the workloads")
				workers, err = e36PickWorkers(ctx, storageNode)
				Expect(err).NotTo(HaveOccurred(), "[E39] list nodes")
				Expect(workers).NotTo(BeEmpty(), "[E39] MISSING PREREQUISITE: need a Ready, schedulable worker other than %s", storageNode)

				By("locating the suite's pillar-csi Helm release")
				release, revision, err := e39HelmRelease(ctx)
				Expect(err).NotTo(HaveOccurred(), "[E39] MISSING PREREQUISITE: Helm release")
				mtlsOn, extraArgs, err := e39ReleaseValues(ctx, release)
				Expect(err).NotTo(HaveOccurred(), "[E39] read Helm values")
				Expect(mtlsOn).To(BeFalse(), "[E39] MISSING PREREQUISITE: E39 provisions its own ephemeral "+
					"mTLS credentials and needs a release installed with mtls.enabled=false")
				for _, a := range extraArgs {
					Expect(a).NotTo(HavePrefix("--recovery-trust-anchor"),
						"[E39] MISSING PREREQUISITE: the release already configures a recovery trust anchor")
				}
				for _, s := range []string{e39AgentSecret, e39ControllerSecret} {
					out, gerr := e36Kubectl(ctx, "", "get", "secret", s, "-n", helmNS,
						"--ignore-not-found=true", "-o", "name")
					Expect(gerr).NotTo(HaveOccurred(), "[E39] get secret %s", s)
					Expect(strings.TrimSpace(out)).To(BeEmpty(),
						"[E39] MISSING PREREQUISITE: Secret %s/%s exists; E39 never overwrites operator credentials", helmNS, s)
				}

				By("generating ephemeral mTLS credentials and an operator signing key")
				nodeIP, err := e36Kubectl(ctx, "", "get", "node", storageNode,
					"-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
				Expect(err).NotTo(HaveOccurred(), "[E39] storage node InternalIP")
				Expect(nodeIP).NotTo(BeEmpty(), "[E39] storage node InternalIP")
				bundle, err = testcerts.New(e39AgentServerName, nodeIP)
				Expect(err).NotTo(HaveOccurred(), "[E39] generate mTLS bundle")
				block, _ := pem.Decode(bundle.ServerCert)
				Expect(block).NotTo(BeNil(), "[E39] decode server certificate")
				serverCert, err = x509.ParseCertificate(block.Bytes)
				Expect(err).NotTo(HaveOccurred(), "[E39] parse server certificate")
				operatorKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				Expect(err).NotTo(HaveOccurred(), "[E39] operator key")
				rogueKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				Expect(err).NotTo(HaveOccurred(), "[E39] untrusted key")
				pubDER, err := x509.MarshalPKIXPublicKey(&operatorKey.PublicKey)
				Expect(err).NotTo(HaveOccurred(), "[E39] marshal operator public key")
				anchorPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

				// Registered first so it runs last: the release returns to its
				// recorded revision only after every volume and CR is gone.
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 12*time.Minute)
					defer ccancel()
					_, stderr, rerr := e27HelmOutput(cctx, "rollback", release, strconv.Itoa(revision),
						"-n", helmNS, "--wait", "--timeout", "8m")
					Expect(rerr).NotTo(HaveOccurred(), "[E39] roll back release %s to revision %d: %s", release, revision, stderr)
					for _, s := range []string{e39AgentSecret, e39ControllerSecret} {
						_, derr := e36Kubectl(cctx, "", "delete", "secret", s, "-n", helmNS, "--ignore-not-found=true")
						Expect(derr).NotTo(HaveOccurred(), "[E39] delete secret %s", s)
					}
					nodes, nerr := e39KindNodes(cctx)
					Expect(nerr).NotTo(HaveOccurred(), "[E39] list nodes")
					for _, n := range nodes {
						_, xerr := kindContainerExec(cctx, n, "rm", "-f", e39TrustAnchorPath)
						Expect(xerr).NotTo(HaveOccurred(), "[E39] remove trust anchor on %s", n)
					}
				})

				By("writing the operator public key as the trust anchor on every node")
				nodes, err := e39KindNodes(ctx)
				Expect(err).NotTo(HaveOccurred(), "[E39] list nodes")
				for _, n := range nodes {
					_, xerr := kindContainerExec(ctx, n, "sh", "-c", fmt.Sprintf(
						"mkdir -p %s && printf '%%s' %s | base64 -d > %s && chmod 0644 %s",
						e38AgentStateDir, base64.StdEncoding.EncodeToString(anchorPEM),
						e39TrustAnchorPath, e39TrustAnchorPath))
					Expect(xerr).NotTo(HaveOccurred(), "[E39] write trust anchor on %s", n)
				}

				By("creating the mTLS Secrets and upgrading the release to mTLS + --recovery-trust-anchor")
				Expect(e36Apply(ctx, e39TLSSecretManifest(e39AgentSecret, helmNS,
					bundle.ServerCert, bundle.ServerKey, bundle.CACert)+"---\n"+
					e39TLSSecretManifest(e39ControllerSecret, helmNS,
						bundle.ClientCert, bundle.ClientKey, bundle.CACert))).
					To(Succeed(), "[E39] apply mTLS Secrets")
				args, err := json.Marshal(append(slices.Clone(extraArgs), "--recovery-trust-anchor="+e39TrustAnchorPath))
				Expect(err).NotTo(HaveOccurred(), "[E39] marshal extraArgs")
				_, stderr, err := e27HelmOutput(ctx, "upgrade", release, e27ChartPath(), "-n", helmNS,
					"--reuse-values", "--set", "mtls.enabled=true",
					"--set-json", "agent.extraArgs="+string(args),
					"--wait", "--timeout", "8m")
				Expect(err).NotTo(HaveOccurred(), "[E39] helm upgrade: %s", stderr)

				By("creating the test namespace")
				Expect(e36Apply(ctx, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", namespace))).
					To(Succeed(), "[E39] create namespace")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
					defer ccancel()
					_, derr := e36Kubectl(cctx, "", "delete", "namespace", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=150s")
					Expect(derr).NotTo(HaveOccurred(), "[E39] delete namespace")
				})

				By("resolving the PillarAgent of the storage node")
				agentName, err = e36ExistingAgentFor(ctx, storageNode)
				Expect(err).NotTo(HaveOccurred(), "[E39] list PillarAgents")
				if agentName == "" {
					agentName = fmt.Sprintf("e39-agent-%d", proc)
					Expect(e36Apply(ctx, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
metadata:
  name: %s
spec:
  nodeRef:
    name: %s
    addressType: InternalIP
`, agentName, storageNode))).To(Succeed(), "[E39] create PillarAgent")
					created := agentName
					DeferCleanup(func() {
						cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer ccancel()
						_, derr := e36Kubectl(cctx, "", "delete", "pillaragent", created,
							"--ignore-not-found=true", "--wait=true", "--timeout=90s")
						Expect(derr).NotTo(HaveOccurred(), "[E39] delete PillarAgent")
					})
				}
				Eventually(func(g Gomega) {
					st, gerr := e36ReadyCondition(ctx, "pillaragent", agentName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(st).To(Equal("True"))
				}).WithContext(ctx).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[E39] PillarAgent %s must become Ready through the mTLS controller", agentName)
				var pa pillarv1.PillarAgent
				found, err := e38GetJSON(ctx, &pa, "pillaragent", agentName)
				Expect(err).NotTo(HaveOccurred(), "[E39] read PillarAgent")
				Expect(found).To(BeTrue(), "[E39] PillarAgent %s vanished", agentName)
				for _, p := range pa.Status.DiscoveredPools {
					if p.Type == string(pillarv1.BackendIDLVMLV) && p.ThinPool == "" {
						linearVG = p.Name
						break
					}
				}
				Expect(linearVG).NotTo(BeEmpty(), "[E39] MISSING PREREQUISITE: the agent must serve an LVM VG "+
					"without thinPool; discovered %+v", pa.Status.DiscoveredPools)

				By("connecting to the agent gRPC API over verified mTLS through kubectl port-forward")
				var agentPod = func() string {
					var name string
					Eventually(func(g Gomega) {
						p, perr := e38AgentPod(ctx, storageNode)
						g.Expect(perr).NotTo(HaveOccurred())
						g.Expect(p.Spec.Containers).NotTo(BeEmpty())
						var anchored bool
						for _, c := range p.Spec.Containers {
							if slices.Contains(c.Args, "--recovery-trust-anchor="+e39TrustAnchorPath) {
								anchored = true
							}
						}
						g.Expect(anchored).To(BeTrue(), "agent Pod %s must run with the trust anchor", p.Name)
						port, perr := e38AgentGRPCPort(p)
						g.Expect(perr).NotTo(HaveOccurred())
						pfCtx, pfCancel := context.WithTimeout(ctx, 30*time.Second)
						defer pfCancel()
						addr, stop, perr := e38PortForward(pfCtx, p.Name, port)
						g.Expect(perr).NotTo(HaveOccurred())
						DeferCleanup(stop)
						agentAddr = addr
						name = p.Name
					}).WithContext(ctx).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
						"[E39] the Helm agent Pod must run on %s with --recovery-trust-anchor", storageNode)
					return name
				}()
				creds, err := tlscreds.NewClientCredentials(bundle.ClientCert, bundle.ClientKey, bundle.CACert, e39AgentServerName)
				Expect(err).NotTo(HaveOccurred(), "[E39] client credentials")
				conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(creds))
				Expect(err).NotTo(HaveOccurred(), "[E39] mTLS client for %s (%s)", agentAddr, agentPod)
				DeferCleanup(func() { _ = conn.Close() })
				agent = agentv1.NewAgentServiceClient(conn)
				Eventually(func(g Gomega) {
					_, cerr := agent.GetCapacity(ctx, &agentv1.GetCapacityRequest{
						BackendType: agentv1.BackendType_BACKEND_TYPE_LVM, PoolName: linearVG,
					})
					g.Expect(cerr).NotTo(HaveOccurred())
				}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(3*time.Second).Should(Succeed(),
					"[E39] the agent must answer a verified mTLS client")

				By("creating the PillarStore / PillarProtocol (acl) / PillarStorageClass (Retain) stack")
				Expect(e36Apply(ctx, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: %[1]s
spec:
  agentRef: %[2]s
  backend:
    lvm:
      volumeGroup: %[3]s
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: %[4]s
spec:
  protocol:
    nvmeofTcp:
      port: 4420
      acl: true
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %[5]s
spec:
  storeRef: %[1]s
  protocolRef: %[4]s
  storageClass:
    name: %[6]s
    reclaimPolicy: Retain
    volumeBindingMode: Immediate
  filesystem:
    fsType: ext4
`, storeName, agentName, linearVG, protocolName, pscName, scName))).To(Succeed(), "[E39] apply CR stack")
				// runScope is read when a cleanup runs, so it carries every
				// record name the specs created.
				runScope := func() e39RunScope {
					s := e39RunScope{
						namespace: namespace, scName: scName, agentName: agentName, vg: linearVG,
						records: append(slices.Clone(trackedPVS), decoyName),
					}
					if oldPVName != "" {
						s.records = append(s.records, oldPVName)
					}
					if fx != nil {
						s.volumeID = fx.volumeID()
					}
					return s
				}
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer ccancel()
					// The workload cleanup (which runs first) removed every
					// volume object; anything still attributed to the class
					// would hold the PillarStorageClass finalizer forever.
					blockers, berr := e39StorageClassBlockers(cctx, runScope())
					Expect(berr).NotTo(HaveOccurred(), "[E39] list PillarStorageClass blockers")
					Expect(blockers.empty()).To(BeTrue(), "[E39] objects still block PillarStorageClass %s: %s", pscName, blockers)
					for _, r := range [][2]string{
						{"pillarstorageclass", pscName}, {"pillarprotocol", protocolName}, {"pillarstore", storeName},
					} {
						_, derr := e36Kubectl(cctx, "", "delete", r[0], r[1],
							"--ignore-not-found=true", "--wait=true", "--timeout=90s")
						if derr != nil {
							cond, _ := e36Kubectl(cctx, "", "get", r[0], r[1], "--ignore-not-found=true",
								"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].message}`)
							Fail(fmt.Sprintf("[E39] delete %s %s: %v; Ready condition: %q", r[0], r[1], derr, cond))
						}
					}
				})
				Eventually(func(g Gomega) {
					st, gerr := e36ReadyCondition(ctx, "pillarstorageclass", pscName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(st).To(Equal("True"))
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[E39] PillarStorageClass %s must be Ready", pscName)

				By("creating the LV outside pillar-csi and writing the proof file")
				fx = &e38Fixture{vg: linearVG, lv: fmt.Sprintf("e39-main-%d-%s", proc, nonce)}
				Expect(e38CreateLV(ctx, storageNode, fx.vg, fx.lv, "")).To(Succeed(), "[E39] create LV")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer ccancel()
					_, uerr := e36NodeSh(cctx, storageNode, fmt.Sprintf(
						"if mountpoint -q %[1]s; then umount %[1]s; fi; rmdir %[1]s 2>/dev/null || true", busyMnt))
					Expect(uerr).NotTo(HaveOccurred(), "[E39] unmount busy mount")
					Expect(e38RemoveLV(cctx, storageNode, fx.vg, fx.lv)).To(Succeed(), "[E39] remove LV %s", fx.volumeID())
				})
				fx.proofSHA, err = e38FormatAndFill(ctx, storageNode, fx.device(), "/tmp/e39-fill-"+fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[E39] format and fill LV")
				fx.row, err = e38LVRow(ctx, storageNode, fx.vg, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[E39] lvs LV")

				controllerDeploy, controllerReplicas, err = e38ControllerDeployment(ctx)
				Expect(err).NotTo(HaveOccurred(), "[E39] controller Deployment")

				// Registered last so it runs first: workloads, claims and
				// lifecycles go before the LV, CRs, namespace and release.
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 14*time.Minute)
					defer ccancel()
					// Every step runs even when an earlier one fails; the
					// collected errors fail the cleanup at the end, after the
					// sweep left nothing that blocks the CR stack.
					var errs []error
					note := func(err error, format string, args ...any) {
						if err != nil {
							errs = append(errs, fmt.Errorf(format+": %w", append(args, err)...))
						}
					}
					if controllerScaled {
						note(e38ScaleController(cctx, controllerDeploy, controllerReplicas), "restore controller replicas")
						controllerScaled = false
					}
					_, derr := e36Kubectl(cctx, "", "delete", "pods", "--all", "-n", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=180s")
					note(derr, "delete Pods")

					// A RecoveryPending record that never served is not a
					// lifecycle any CSI call ends: remove it directly.
					note(e39ForceDelete(cctx, "pillarvolumestate", decoyName, ""), "remove unclaimed recovery record %s", decoyName)

					// Served volumes end through CSI DeleteVolume (release of
					// the preserved LV) before any forced removal.
					for i, pv := range trackedPVs {
						note(e38PrepareRelease(cctx, namespace, pv, fmt.Sprintf("e39-cleanup-%d-%d", proc, i)),
							"prepare release of PV %s", pv)
					}
					_, derr = e36Kubectl(cctx, "", "delete", "pvc", "--all", "-n", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=180s")
					note(derr, "delete PVCs")
					for _, pv := range trackedPVs {
						note(e39WaitGone(cctx, pv, 4*time.Minute), "release volume %s", pv)
					}

					// Sweep: whatever the release path left behind is removed
					// so it cannot hold the CR finalizers — every record,
					// PV and claim of this run, re-listed until none remains.
					leftover, serr := e39Sweep(cctx, storageNode, runScope(), 3*time.Minute)
					note(serr, "sweep leftover volume objects")
					if !leftover.empty() {
						// Recorded, not failed: the sweep is the planned
						// removal of records no CSI call ends (RecoveryPending
						// records, empty volumes of cleanup claims).
						AddReportEntry("E39 cleanup force-removed", leftover.String())
					}
					Expect(errors.Join(errs...)).NotTo(HaveOccurred(), "[E39] workload cleanup")
				})
			})

			// -- TC-E39.1 ----------------------------------------------------
			It("[TC-E39.1] an adopted LV served over mTLS gets a healthy lifecycle and a remote workload reads its data", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
				defer cancel()

				By("adopting the LV through an import-lv claim under the Retain class")
				Expect(e36Apply(ctx, e38PVCManifest(pvcOrig, namespace, scName, fx.importValue()))).
					To(Succeed(), "[TC-E39.1] apply import-lv PVC")
				oldPVName = e38WaitBound(ctx, "TC-E39.1", namespace, pvcOrig)
				trackedPVs = append(trackedPVs, oldPVName)

				var err error
				oldPVS, err = e38GetPVS(ctx, oldPVName)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.1] get PillarVolumeState")
				Expect(oldPVS).NotTo(BeNil(), "[TC-E39.1] PillarVolumeState %s", oldPVName)
				Expect(oldPVS.Spec.AgentVolumeID).To(Equal(fx.volumeID()), "[TC-E39.1] agentVolumeID")
				Expect(oldPVS.Spec.LVMSource).NotTo(BeNil(), "[TC-E39.1] lvmSource pinned")
				Expect(oldPVS.Spec.LVMSource.PreserveOriginal).To(BeTrue(), "[TC-E39.1] default PreserveOriginal")
				Expect(oldPVS.Spec.LVMSource.LogicalVolumeUUID).To(Equal(fx.row.LVUUID), "[TC-E39.1] pinned LV UUID")

				By("reading the proof file from a workload on a remote worker")
				e38RunPodAndVerify("TC-E39.1", namespace, podOrig, workers[0], pvcOrig, fx.proofSHA)
			})

			// -- TC-E39.2 ----------------------------------------------------
			It("[TC-E39.2] the agent signs snapshots only for verified mTLS callers and refuses to transfer a live, ACL-granted volume", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				marks, err := e38MarkFiles(ctx, storageNode, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.2] mark files before")

				By("refusing a plaintext caller at the transport")
				plain, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.2] plaintext client")
				defer func() { _ = plain.Close() }()
				_, err = agentv1.NewAgentServiceClient(plain).InspectVolume(ctx, &agentv1.InspectVolumeRequest{
					VolumeId: fx.volumeID(), BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				})
				Expect(status.Code(err)).To(BeElementOf(codes.Unavailable, codes.Unauthenticated),
					"[TC-E39.2] plaintext InspectVolume must never be answered: %v", err)

				By("refusing a TLS caller without a client certificate at the handshake")
				roots := x509.NewCertPool()
				Expect(roots.AppendCertsFromPEM(bundle.CACert)).To(BeTrue(), "[TC-E39.2] CA pool")
				anon, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
					RootCAs: roots, ServerName: e39AgentServerName, MinVersion: tls.VersionTLS13,
				})))
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.2] certificate-less TLS client")
				defer func() { _ = anon.Close() }()
				_, err = agentv1.NewAgentServiceClient(anon).TransferVolumeOwnership(ctx,
					&agentv1.TransferVolumeOwnershipRequest{})
				Expect(status.Code(err)).To(BeElementOf(codes.Unavailable, codes.Unauthenticated),
					"[TC-E39.2] a client without a verified certificate must be refused: %v", err)

				By("inspecting the live volume over verified mTLS")
				resp, snap := freshSnapshot(ctx, "TC-E39.2")
				Expect(resp.GetFence().GetExists()).To(BeTrue(), "[TC-E39.2] fence mark exists")
				Expect(resp.GetFence().GetVolumeUid()).To(Equal(string(oldPVS.UID)), "[TC-E39.2] mark records the live lifecycle")
				Expect(resp.GetFence().GetEnded()).To(BeFalse(), "[TC-E39.2] mark is live")
				Expect(snap.GetOldVolumeUid()).To(Equal(string(oldPVS.UID)), "[TC-E39.2] snapshot names the live lifecycle")
				Expect(snap.GetOldGeneration()).To(Equal(resp.GetFence().GetGeneration()), "[TC-E39.2] snapshot generation = mark")
				Expect(snap.GetPreserveOriginal()).To(BeTrue(), "[TC-E39.2] snapshot preserve policy")
				Expect(snap.GetLvmSource().GetLogicalVolumeUuid()).To(Equal(fx.row.LVUUID), "[TC-E39.2] snapshot LV UUID")
				Expect(snap.GetExports()).NotTo(BeEmpty(), "[TC-E39.2] the served volume is exported")
				Expect(snap.GetExports()[0].GetAclEnabled()).To(BeTrue(), "[TC-E39.2] the export enforces the ACL")
				Expect(snap.GetExports()[0].GetAllowedHosts()).NotTo(BeEmpty(), "[TC-E39.2] the old initiator is granted")
				healthySnap = snap

				By("refusing a correctly signed grant while the old initiator is granted")
				auth := e39SignGrant(snap, grantFor(snap, "e39-direct-live-"+nonce), operatorKey)
				_, err = transfer(ctx, snap, auth)
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
					"[TC-E39.2] a live export/ACL grant proves the old lifecycle is not stopped: %v", err)
				marksUnchanged(ctx, "TC-E39.2", marks)
			})

			// -- TC-E39.3 ----------------------------------------------------
			It("[TC-E39.3] losing the PVC, PV and PillarVolumeState keeps the LV bytes and the old lifecycle's fence mark", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()

				By("stopping the workload and waiting for the unpublish")
				Expect(e36DeletePod(ctx, namespace, podOrig)).To(Succeed(), "[TC-E39.3] delete workload Pod")
				Eventually(func(g Gomega) {
					vas, gerr := e38AttachmentsFor(ctx, oldPVName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(vas).To(BeEmpty(), "VolumeAttachments of %s", oldPVName)
					pvs, gerr := e38GetPVS(ctx, oldPVName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(pvs).NotTo(BeNil())
					g.Expect(pvs.Status.PublishedNodes).To(BeEmpty(), "publications of %s", oldPVName)
				}).WithContext(ctx).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E39.3] the workload must be unpublished")

				By("quiescing the controller so nothing re-creates or reaps the lifecycle")
				Expect(e38ScaleController(ctx, controllerDeploy, 0)).To(Succeed(), "[TC-E39.3] scale controller to 0")
				controllerScaled = true

				By("removing the old lifecycle's export with its own fence token")
				before, err := e38Inspect(ctx, agent, fx.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.3] InspectVolume")
				oldGen = before.GetFence().GetGeneration()
				_, err = agent.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
					VolumeId:     fx.volumeID(),
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
					Fence:        &agentv1.FencingToken{VolumeUid: string(oldPVS.UID), Generation: oldGen},
				})
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.3] fenced UnexportVolume of the old lifecycle")

				devSHA, err := e38DeviceSHA(ctx, storageNode, fx.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.3] device sha256 before loss")
				marksAtLoss, err = e38MarkFiles(ctx, storageNode, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.3] mark files before loss")
				Expect(marksAtLoss).NotTo(BeEmpty(), "[TC-E39.3] the adoption left a fence mark")

				By("deleting the claim, the PV and the PillarVolumeState (metadata loss)")
				Expect(e38DeletePVC(ctx, namespace, pvcOrig)).To(Succeed(), "[TC-E39.3] delete PVC")
				Expect(e39ForceDelete(ctx, "pv", oldPVName, "")).To(Succeed(), "[TC-E39.3] delete PV")
				Expect(e39ForceDelete(ctx, "pillarvolumestate", oldPVName, "")).To(Succeed(), "[TC-E39.3] delete PillarVolumeState")
				trackedPVs = slices.DeleteFunc(trackedPVs, func(pv string) bool { return pv == oldPVName })
				pvs, err := e38GetPVS(ctx, oldPVName)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.3] get PillarVolumeState")
				Expect(pvs).To(BeNil(), "[TC-E39.3] the PillarVolumeState is lost")

				By("proving the LV, its bytes and the fence mark survived")
				row, err := e38LVRow(ctx, storageNode, fx.vg, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.3] lvs")
				Expect(row.LVUUID).To(Equal(fx.row.LVUUID), "[TC-E39.3] LV UUID")
				after, err := e38DeviceSHA(ctx, storageNode, fx.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.3] device sha256 after loss")
				Expect(after).To(Equal(devSHA), "[TC-E39.3] the LV bytes are unchanged")
				marksUnchanged(ctx, "TC-E39.3", marksAtLoss)

				resp, snap := freshSnapshot(ctx, "TC-E39.3")
				Expect(resp.GetFence().GetVolumeUid()).To(Equal(string(oldPVS.UID)), "[TC-E39.3] the old lifecycle still owns the mark")
				Expect(resp.GetFence().GetGeneration()).To(Equal(oldGen), "[TC-E39.3] exact old generation")
				Expect(resp.GetFence().GetEnded()).To(BeFalse(), "[TC-E39.3] the mark was never released")
				Expect(resp.GetExports()).To(BeEmpty(), "[TC-E39.3] no export remains")
				Expect(resp.GetConsumers()).To(BeEmpty(), "[TC-E39.3] no consumer remains")
				Expect(resp.GetLvm().GetExclusiveClaim()).To(Equal("free"), "[TC-E39.3] the device is idle")
				Expect(snap.GetExports()).To(BeEmpty(), "[TC-E39.3] the snapshot records no export")
				lossSnap = snap
			})

			// -- TC-E39.4 ----------------------------------------------------
			It("[TC-E39.4] missing, untrusted, expired, tampered and mismatched grants refuse without touching the LV or the mark", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
				defer cancel()
				devSHA, err := e38DeviceSHA(ctx, storageNode, fx.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.4] device sha256 before")
				_, snap := freshSnapshot(ctx, "TC-E39.4")
				dest := "e39-direct-dest-" + nonce
				valid := grantFor(snap, dest)

				otherSnap := proto.Clone(snap).(*agentv1.RecoverySnapshot)
				otherSnap.IssuedAt = timestamppb.New(snap.GetIssuedAt().AsTime().Add(-time.Second))
				forged := proto.Clone(snap).(*agentv1.RecoverySnapshot)
				forged.Consumers = nil
				forged.ExclusiveClaim = "free"
				forged.OldGeneration = snap.GetOldGeneration() + 7
				forged.Fence = proto.Clone(snap.GetFence()).(*agentv1.FenceObservation)
				forged.Fence.Generation = forged.OldGeneration
				badSig := proto.Clone(snap).(*agentv1.RecoverySnapshot)
				badSig.Signature = slices.Clone(snap.GetSignature())
				badSig.Signature[len(badSig.Signature)/2] ^= 0xff

				with := func(edit func(*e39Grant)) e39Grant {
					g := valid
					edit(&g)
					return g
				}
				cases := []struct {
					name string
					snap *agentv1.RecoverySnapshot
					auth *agentv1.RecoveryAuthorization
					want codes.Code
				}{
					{"no snapshot", nil, e39SignGrant(snap, valid, operatorKey), codes.InvalidArgument},
					{"no authorization", snap, nil, codes.InvalidArgument},
					{"untrusted operator key", snap, e39SignGrant(snap, valid, rogueKey), codes.Unauthenticated},
					{"expired grant", snap, e39SignGrant(snap, with(func(g *e39Grant) {
						g.issued, g.expires = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
					}), operatorKey), codes.FailedPrecondition},
					{"future grant", snap, e39SignGrant(snap, with(func(g *e39Grant) {
						g.issued, g.expires = time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
					}), operatorKey), codes.FailedPrecondition},
					{"over-long validity window", snap, e39SignGrant(snap, with(func(g *e39Grant) {
						g.expires = g.issued.Add(recoveryauth.MaxAuthorizationLifetime + time.Hour)
					}), operatorKey), codes.FailedPrecondition},
					{"unset preserve policy", snap, e39SignGrant(snap, with(func(g *e39Grant) {
						g.preserve = nil
					}), operatorKey), codes.InvalidArgument},
					{"grant names another snapshot", snap, e39SignGrant(otherSnap, valid, operatorKey), codes.InvalidArgument},
					{"grant names another old generation", snap, e39SignGrant(snap, with(func(g *e39Grant) {
						g.mutate = func(a *agentv1.RecoveryAuthorization) { a.OldGeneration++ }
					}), operatorKey), codes.InvalidArgument},
					{"grant names another old lifecycle", snap, e39SignGrant(snap, with(func(g *e39Grant) {
						g.mutate = func(a *agentv1.RecoveryAuthorization) { a.OldVolumeUid = "e39-direct-other-" + nonce }
					}), operatorKey), codes.InvalidArgument},
					{"tampered snapshot signature", badSig, e39SignGrant(badSig, valid, operatorKey), codes.Unauthenticated},
					{"forged snapshot payload", forged, e39SignGrant(forged, valid, operatorKey), codes.Unauthenticated},
				}
				for _, tc := range cases {
					By("refusing: " + tc.name)
					_, err := transfer(ctx, tc.snap, tc.auth)
					Expect(status.Code(err)).To(Equal(tc.want), "[TC-E39.4] %s: %v", tc.name, err)
					marksUnchanged(ctx, "TC-E39.4 "+tc.name, marksAtLoss)
				}

				By("proving the LV bytes and the old lifecycle are untouched")
				after, err := e38DeviceSHA(ctx, storageNode, fx.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.4] device sha256 after")
				Expect(after).To(Equal(devSHA), "[TC-E39.4] the LV bytes are unchanged")
				resp, err := e38Inspect(ctx, agent, fx.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.4] InspectVolume")
				Expect(resp.GetFence().GetVolumeUid()).To(Equal(string(oldPVS.UID)), "[TC-E39.4] the old lifecycle still owns the mark")
				Expect(resp.GetFence().GetGeneration()).To(Equal(oldGen), "[TC-E39.4] generation unchanged")
			})

			// -- TC-E39.5 ----------------------------------------------------
			It("[TC-E39.5] a local consumer on the storage node refuses a valid grant until it stops", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
				defer cancel()

				By("mounting the LV on the storage node outside pillar-csi")
				_, err := e36NodeSh(ctx, storageNode, fmt.Sprintf(
					"mkdir -p %[2]s && mount -t ext4 -o ro,noload %[1]s %[2]s", fx.device(), busyMnt))
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.5] mount LV")

				By("refusing a correctly signed grant over the idle snapshot while the mount is live")
				auth := e39SignGrant(lossSnap, grantFor(lossSnap, "e39-direct-busy-"+nonce), operatorKey)
				_, err = transfer(ctx, lossSnap, auth)
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
					"[TC-E39.5] the live re-observation must find the device busy: %v", err)
				marksUnchanged(ctx, "TC-E39.5", marksAtLoss)
				resp, err := e38Inspect(ctx, agent, fx.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.5] InspectVolume")
				// The agent's mount namespace does not contain the storage
				// node's mount (the same boundary as E38.8), so `consumers`
				// may stay empty; the failed O_EXCL open is the real proof
				// that the old initiator is not stopped.
				Expect(resp.GetLvm().GetExclusiveClaim()).To(Equal("busy"),
					"[TC-E39.5] the mounted LV cannot be claimed exclusively")

				By("stopping the consumer")
				_, err = e36NodeSh(ctx, storageNode, fmt.Sprintf("umount %[1]s && rmdir %[1]s", busyMnt))
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.5] unmount LV")
				Eventually(func(g Gomega) {
					r, ierr := e38Inspect(ctx, agent, fx.volumeID())
					g.Expect(ierr).NotTo(HaveOccurred())
					g.Expect(r.GetLvm().GetExclusiveClaim()).To(Equal("free"))
				}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(3*time.Second).Should(Succeed(),
					"[TC-E39.5] the LV must be idle again")
			})

			// -- TC-E39.6 ----------------------------------------------------
			It("[TC-E39.6] the operator workflow commits the exact transfer and a never-claimed RecoveryPending record is neither reaped nor served", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
				defer cancel()
				Expect(controllerScaled).To(BeTrue(), "[TC-E39.6] TC-E39.3 quiesced the controller")

				By("creating a plain replacement claim while the controller is down")
				Expect(e36Apply(ctx, e39PlainPVCManifest(pvcRecover, namespace, scName))).
					To(Succeed(), "[TC-E39.6] apply recovery PVC")
				claim, err := e38GetPVC(ctx, namespace, pvcRecover)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.6] get recovery PVC")
				Expect(claim).NotTo(BeNil(), "[TC-E39.6] recovery PVC")
				pvName := "pvc-" + string(claim.UID)

				intent := func(newGen uint64) *pillarv1.VolumeRecoveryIntent {
					src := *oldPVS.Spec.LVMSource
					return &pillarv1.VolumeRecoveryIntent{
						OldVolumeUID:  string(oldPVS.UID),
						OldGeneration: int64(oldGen),
						Source:        &src,
						NewGeneration: int64(newGen),
					}
				}
				newGen := oldGen + 1

				By("creating the RecoveryPending PillarVolumeState named after the claim")
				recoverPVS, err = e39CreatePVS(ctx, &pillarv1.PillarVolumeState{
					ObjectMeta: metav1.ObjectMeta{Name: pvName},
					Spec: pillarv1.PillarVolumeStateSpec{
						VolumeID:      oldPVS.Spec.VolumeID,
						AgentVolumeID: oldPVS.Spec.AgentVolumeID,
						AgentRef:      oldPVS.Spec.AgentRef,
						BackendType:   e39BackendToken,
						ProtocolType:  e39ProtocolToken,
						CapacityBytes: oldPVS.Spec.CapacityBytes,
						ClaimRef: &pillarv1.VolumeClaimRef{
							UID: string(claim.UID), Namespace: namespace, Name: pvcRecover,
						},
						Recovery: intent(newGen),
					},
				})
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.6] create recovery PillarVolumeState")
				trackedPVS = append(trackedPVS, pvName)

				By("creating a RecoveryPending record whose claim never existed")
				_, err = e39CreatePVS(ctx, &pillarv1.PillarVolumeState{
					ObjectMeta: metav1.ObjectMeta{Name: decoyName},
					Spec: pillarv1.PillarVolumeStateSpec{
						VolumeID:      oldPVS.Spec.VolumeID,
						AgentVolumeID: oldPVS.Spec.AgentVolumeID,
						AgentRef:      oldPVS.Spec.AgentRef,
						BackendType:   e39BackendToken,
						ProtocolType:  e39ProtocolToken,
						CapacityBytes: oldPVS.Spec.CapacityBytes,
						ClaimRef:      &pillarv1.VolumeClaimRef{UID: decoyUID, Namespace: namespace, Name: decoyClaim},
						Recovery:      intent(newGen),
					},
				})
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.6] create unclaimed recovery record")
				trackedPVS = append(trackedPVS, decoyName)

				By("signing the grant over a fresh agent snapshot and latching it on the record")
				_, snap := freshSnapshot(ctx, "TC-E39.6")
				Expect(snap.GetOldVolumeUid()).To(Equal(string(oldPVS.UID)), "[TC-E39.6] snapshot names the old lifecycle")
				Expect(snap.GetOldGeneration()).To(Equal(oldGen), "[TC-E39.6] exact old generation")
				g := grantFor(snap, string(recoverPVS.UID))
				g.newGen = newGen
				auth := e39SignGrant(snap, g, operatorKey)
				Expect(e39RecoveryPatch(ctx, pvName, snap, auth, string(recoverPVS.UID))).
					To(Succeed(), "[TC-E39.6] patch the write-once latch fields")
				committedReq = &agentv1.TransferVolumeOwnershipRequest{Snapshot: snap, Authorization: auth}
				marksUnchanged(ctx, "TC-E39.6 before controller", marksAtLoss)

				By("restoring the controller so CreateVolume drives the transfer")
				Expect(e38ScaleController(ctx, controllerDeploy, controllerReplicas)).To(Succeed(), "[TC-E39.6] restore controller")
				controllerScaled = false
				bound := e38WaitBound(ctx, "TC-E39.6", namespace, pvcRecover)
				Expect(bound).To(Equal(pvName), "[TC-E39.6] the claim binds the record it is named after")
				trackedPVs = append(trackedPVs, pvName)
				Eventually(func(g Gomega) {
					pvs, gerr := e38GetPVS(ctx, pvName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(pvs).NotTo(BeNil())
					g.Expect(pvs.Status.Phase).To(Equal(pillarv1.PillarVolumeStatePhaseReady))
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E39.6] the recovery record becomes Ready only after the transfer")

				By("proving the mark moved exactly once to the authorized lifecycle")
				resp, err := e38Inspect(ctx, agent, fx.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.6] InspectVolume")
				Expect(resp.GetFence().GetVolumeUid()).To(Equal(string(recoverPVS.UID)), "[TC-E39.6] new lifecycle owns the mark")
				Expect(resp.GetFence().GetGeneration()).To(BeNumerically(">=", newGen), "[TC-E39.6] new generation")
				Expect(resp.GetFence().GetEnded()).To(BeFalse(), "[TC-E39.6] the new lifecycle is live")
				Expect(resp.GetFence().GetEndedUids()).To(ContainElement(string(oldPVS.UID)), "[TC-E39.6] the old lifecycle is retired")
				Expect(resp.GetFence().GetPreserveOriginal()).To(BeTrue(), "[TC-E39.6] PreserveOriginal kept")
				Expect(resp.GetFence().GetLvmSource().GetLogicalVolumeUuid()).To(Equal(fx.row.LVUUID), "[TC-E39.6] same pinned LV")
				pv, err := e38GetPV(ctx, pvName)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.6] get PV")
				Expect(pv).NotTo(BeNil(), "[TC-E39.6] PV")
				Expect(pv.Spec.CSI.VolumeHandle).To(Equal(oldPVS.Spec.VolumeID), "[TC-E39.6] same volume handle as the lost PV")
				Expect(pv.Spec.CSI.VolumeAttributes).To(HaveKeyWithValue(e38VCPreserve, "true"),
					"[TC-E39.6] the node is told to preserve the original")

				By("keeping the never-claimed RecoveryPending record unreaped and unserved")
				// The record is observed every 10s for 90s. Each observation
				// retries a failed API read (a transport timeout is not a
				// product result) for up to 30s; every observation that was
				// read must show the record present, not Ready, unpublished.
				holdUntil := time.Now().Add(90 * time.Second)
				observed := 0
				for {
					var decoy *pillarv1.PillarVolumeState
					Eventually(func() error {
						var gerr error
						decoy, gerr = e38GetPVS(ctx, decoyName)
						return gerr
					}).WithContext(ctx).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(Succeed(),
						"[TC-E39.6] read the unclaimed recovery record")
					Expect(decoy).NotTo(BeNil(), "[TC-E39.6] the reaper must never end a recovery record")
					Expect(decoy.Status.Phase).NotTo(Equal(pillarv1.PillarVolumeStatePhaseReady),
						"[TC-E39.6] a RecoveryPending record is non-serving")
					Expect(decoy.Status.PublishedNodes).To(BeEmpty(), "[TC-E39.6] a RecoveryPending record is never published")
					observed++
					if time.Now().After(holdUntil) {
						break
					}
					time.Sleep(10 * time.Second)
				}
				r, err := e38Inspect(ctx, agent, fx.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.6] InspectVolume")
				Expect(r.GetFence().GetVolumeUid()).To(Equal(string(recoverPVS.UID)), "[TC-E39.6] the decoy never took the mark")
			})

			// -- TC-E39.7 ----------------------------------------------------
			It("[TC-E39.7] a replacement workload on a remote node reads the original data from the recovered volume", func() {
				Expect(recoverPVS).NotTo(BeNil(), "[TC-E39.7] TC-E39.6 recovered the volume")
				e38RunPodAndVerify("TC-E39.7", namespace, podRecover, workers[0], pvcRecover, fx.proofSHA)

				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				row, err := e38LVRow(ctx, storageNode, fx.vg, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.7] lvs")
				Expect(row.LVUUID).To(Equal(fx.row.LVUUID), "[TC-E39.7] same LV UUID")
				Expect(row.Size).To(Equal(fx.row.Size), "[TC-E39.7] same LV size")
			})

			// -- TC-E39.8 ----------------------------------------------------
			It("[TC-E39.8] after the commit an exact replay is idempotent while stale, re-targeted, re-signed and retired-lifecycle requests refuse", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
				defer cancel()
				Expect(committedReq).NotTo(BeNil(), "[TC-E39.8] TC-E39.6 committed a transfer")
				marks, err := e38MarkFiles(ctx, storageNode, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.8] mark files")

				By("answering the exact committed request from the durable record")
				resp, err := agent.TransferVolumeOwnership(ctx, committedReq)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.8] exact replay")
				Expect(resp.GetOutcome()).To(Equal(agentv1.TransferOutcome_TRANSFER_OUTCOME_ALREADY_COMMITTED), "[TC-E39.8] outcome")
				digest, err := recoveryauth.AuthorizationDigest(committedReq.GetAuthorization())
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.8] digest")
				Expect(resp.GetAuthorizationDigest()).To(Equal(digest[:]), "[TC-E39.8] the committed grant is echoed")
				marksUnchanged(ctx, "TC-E39.8 replay", marks)

				snap := committedReq.GetSnapshot()
				cases := []struct {
					name string
					snap *agentv1.RecoverySnapshot
					auth *agentv1.RecoveryAuthorization
				}{
					{"a different destination", snap,
						e39SignGrant(snap, grantFor(snap, "e39-direct-elsewhere-"+nonce), operatorKey)},
					{"a different grant for the same destination", snap, func() *agentv1.RecoveryAuthorization {
						g := grantFor(snap, string(recoverPVS.UID))
						g.newGen = committedReq.GetAuthorization().GetNewGeneration()
						g.expires = g.expires.Add(time.Minute)
						return e39SignGrant(snap, g, operatorKey)
					}()},
					{"the stale pre-loss snapshot", healthySnap,
						e39SignGrant(healthySnap, grantFor(healthySnap, "e39-direct-stale-"+nonce), operatorKey)},
				}
				for _, tc := range cases {
					By("refusing " + tc.name)
					_, err := transfer(ctx, tc.snap, tc.auth)
					Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E39.8] %s: %v", tc.name, err)
					marksUnchanged(ctx, "TC-E39.8 "+tc.name, marks)
				}

				By("refusing the retired lifecycle's fence token")
				_, err = agent.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
					VolumeId:    fx.volumeID(),
					BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					Fence:       &agentv1.FencingToken{VolumeUid: string(oldPVS.UID), Generation: oldGen},
				})
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E39.8] retired-lifecycle Delete: %v", err)
				marksUnchanged(ctx, "TC-E39.8 retired token", marks)
				row, err := e38LVRow(ctx, storageNode, fx.vg, fx.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.8] lvs")
				Expect(row.LVUUID).To(Equal(fx.row.LVUUID), "[TC-E39.8] the LV survives")

				By("keeping the unclaimed recovery record pending after the commit")
				decoy, err := e38GetPVS(ctx, decoyName)
				Expect(err).NotTo(HaveOccurred(), "[TC-E39.8] get unclaimed record")
				Expect(decoy).NotTo(BeNil(), "[TC-E39.8] the unclaimed record still exists")
				Expect(decoy.Status.Phase).NotTo(Equal(pillarv1.PillarVolumeStatePhaseReady), "[TC-E39.8] never Ready")
			})
		})
	})
