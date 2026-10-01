//go:build docker_e2e

package dockere2e

// CHAP and MutualCHAP on real kernels: the
// agent writes the credentials of the protocol's Secret onto the LIO node ACL
// of every published initiator, kubelet hands the same Secret to NodeStage
// through the generated StorageClass, and the in-process pillar-node
// initiator answers the target's CHAP challenge (and, for MutualCHAP,
// challenges the target back) before handing the session to iscsi_tcp.
//
// Every test creates its own Secret (in the pillar-csi installation
// namespace), PillarProtocol and StorageClass and removes them afterwards;
// the shared ACL-only (method None) protocol of run.sh stays untouched.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// The iscsiStatusDetailAuthFailed value is the Login Response status detail
	// (RFC 7143 §11.13.5) for "authentication failure"; with class 2 it is
	// what a target enforcing CHAP answers an initiator that offers no
	// acceptable AuthMethod.
	iscsiStatusDetailAuthFailed = 0x01

	// The iscsiAuthMethodAttribute key records a CHAP volume's auth method
	// in the VolumeContext.
	iscsiAuthMethodAttribute = "pillar-csi.bhyoo.com/iscsi-auth-method"

	// The chapInitiator* / chapTarget* constants are the CHAP names and
	// passwords of the test Secrets.  The passwords satisfy the 12-byte
	// minimum the controller enforces (RFC 7143 §12.1.3: at least 96-bit).
	chapInitiatorUsername = "pillar-e2e-initiator"
	chapInitiatorPassword = "pillar-e2e-initiator-secret"
	chapTargetUsername    = "pillar-e2e-target"
	chapTargetPassword    = "pillar-e2e-target-secret"
	chapWrongPassword     = "pillar-e2e-wrong-secret"

	// The chapAuthFailedEvent substring appears in the FailedMount event
	// kubelet records when NodeStage's login fails CHAP authentication.
	chapAuthFailedEvent = "authentication failed"
)

type iscsiCHAPConfig struct {
	iscsiConfig
	// installNamespace is the pillar-csi installation namespace: the only
	// namespace whose Secrets the controller may read.
	installNamespace string
}

func loadISCSICHAPConfig(t *testing.T) iscsiCHAPConfig {
	t.Helper()
	return iscsiCHAPConfig{
		iscsiConfig:      loadISCSIConfig(t),
		installNamespace: requireEnv(t, "PILLAR_E2E_INSTALL_NAMESPACE"),
	}
}

// chapCredentials is the content of a CHAP Secret; empty mutual fields mean
// one-way CHAP.
type chapCredentials struct {
	username       string
	password       string
	mutualUsername string
	mutualPassword string
}

func chapCredentialsFor(method string) chapCredentials {
	creds := chapCredentials{username: chapInitiatorUsername, password: chapInitiatorPassword}
	if method == "MutualCHAP" {
		creds.mutualUsername = chapTargetUsername
		creds.mutualPassword = chapTargetPassword
	}
	return creds
}

// TestISCSICHAPFilesystemCrossNode writes through a one-way CHAP volume on
// client node A and reads it on client node B.  Both logins answer the
// target's challenge with the Secret's credentials, which the agent wrote
// onto each node's ACL.
func TestISCSICHAPFilesystemCrossNode(t *testing.T) {
	runISCSICHAPCrossNode(t, "CHAP")
}

// TestISCSIMutualCHAPFilesystemCrossNode is the MutualCHAP variant: the
// initiator additionally verifies the target's response with the Secret's
// mutual credentials.
func TestISCSIMutualCHAPFilesystemCrossNode(t *testing.T) {
	runISCSICHAPCrossNode(t, "MutualCHAP")
}

func runISCSICHAPCrossNode(t *testing.T, method string) {
	cfg := loadISCSICHAPConfig(t)
	creds := chapCredentialsFor(method)
	suffix := strings.ToLower(method)
	secret := createCHAPSecret(t, cfg.installNamespace, "pillar-e2e-"+suffix, creds)
	storageClass := createCHAPStorageClass(t, cfg, "pillar-e2e-iscsi-"+suffix, method, secret)

	ns := createNamespace(t, "iscsi-"+suffix)
	defer deleteNamespace(t, ns)
	createISCSIPVC(t, ns, "data", storageClass, "Filesystem", iscsiVolumeSize)

	createFilesystemPod(t, ns, "writer", "data", cfg.clientNodeA)
	waitForPodReady(t, ns, "writer")
	assertPodNode(t, ns, "writer", cfg.clientNodeA)
	target := readPVISCSITarget(t, ns, "data", cfg.targetAddress)
	t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
	requirePVCHAPAttributes(t, ns, "data", method, secret, cfg.installNamespace)
	iqnA := readISCSIInitiatorIQN(t, cfg.clientNodeA)
	requireLIOCHAP(t, target, iqnA, creds)
	requirePodUsesISCSIDevice(t, ns, "writer", cfg.clientNodeA, target, iqnA, false)

	payload := "pillar-csi-iscsi-" + suffix
	kubectl(t, "-n", ns, "exec", "writer", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q > /data/payload && sync", payload))

	verifyISCSICrossNodeHandoff(t, ns, "writer", "reader", target, cfg.clientNodeA, cfg.clientNodeB, func() {
		createFilesystemPod(t, ns, "reader", "data", cfg.clientNodeB)
	})
	iqnB := readISCSIInitiatorIQN(t, cfg.clientNodeB)
	requireLIOCHAP(t, target, iqnB, creds)
	requirePodUsesISCSIDevice(t, ns, "reader", cfg.clientNodeB, target, iqnB, false)
	if got := kubectl(t, "-n", ns, "exec", "reader", "--", "cat", "/data/payload"); got != payload {
		t.Fatalf("%s cross-node reader read %q, want %q", method, got, payload)
	}
}

// TestISCSICHAPWrongNodeSecretRejected makes the node's credentials diverge
// from the target's: the protocol's Secret (what the controller writes onto
// the ACL) holds the right password, while a hand-written StorageClass hands
// NodeStage a second Secret with a wrong password.  The target must refuse
// the login, so the pod stays pending without any session or SCSI disk; once
// the node Secret is corrected, kubelet's NodeStage retry logs in.
func TestISCSICHAPWrongNodeSecretRejected(t *testing.T) {
	cfg := loadISCSICHAPConfig(t)
	creds := chapCredentialsFor("CHAP")
	targetSecret := createCHAPSecret(t, cfg.installNamespace, "pillar-e2e-chap-target-side", creds)
	wrong := creds
	wrong.password = chapWrongPassword
	nodeSecret := createCHAPSecret(t, cfg.installNamespace, "pillar-e2e-chap-node-side", wrong)
	protocol := createCHAPProtocol(t, "pillar-e2e-iscsi-chap-wrong", "CHAP", targetSecret)
	storageClass := createHandWrittenCHAPStorageClass(t, "pillar-e2e-iscsi-chap-wrong", protocol, nodeSecret,
		cfg.installNamespace)

	ns := createNamespace(t, "iscsi-chap-wrong")
	defer deleteNamespace(t, ns)
	createISCSIPVC(t, ns, "data", storageClass, "Filesystem", iscsiVolumeSize)
	createFilesystemPod(t, ns, "owner", "data", cfg.clientNodeA)

	target := readPVISCSITarget(t, ns, "data", cfg.targetAddress)
	t.Cleanup(func() { waitForLIOTargetRemoved(t, target) })
	iqnA := readISCSIInitiatorIQN(t, cfg.clientNodeA)
	// ControllerPublish succeeded: node A's ACL exists and carries the
	// target-side credentials.
	waitForLIOACLs(t, target, []string{iqnA})
	requireLIOCHAP(t, target, iqnA, creds)

	waitFor(t, fmt.Sprintf("a FailedMount event of pod %s/owner naming %q", ns, chapAuthFailedEvent),
		func() (bool, string) {
			events := podFailedMountEvents(t, ns, "owner")
			return strings.Contains(strings.ToLower(events), chapAuthFailedEvent), events
		})
	if phase := kubectl(t, "-n", ns, "get", "pod", "owner", "-o", "jsonpath={.status.phase}"); phase != "Pending" {
		t.Fatalf("pod %s/owner with a wrong node CHAP secret is %q, want Pending", ns, phase)
	}
	requireNoISCSISession(t, cfg.clientNodeA, target, iqnA)

	// The ACL exists, so the target does not answer "forbidden": it refuses a
	// login that skips CHAP as an authentication failure.
	class, detail := iscsiLoginStatus(t, cfg.clientNodeA, target, iqnA)
	if class != iscsiStatusClassInitiatorError || detail == iscsiStatusDetailForbidden {
		t.Fatalf("login without CHAP as %q from Kind node %q: status class %#x detail %#x, want class %#x with an "+
			"authentication (not %#x target forbidden) detail", iqnA, cfg.clientNodeA, class, detail,
			iscsiStatusClassInitiatorError, iscsiStatusDetailForbidden)
	}
	if detail != iscsiStatusDetailAuthFailed {
		t.Logf("login without CHAP refused with detail %#x (initiator error) instead of %#x (authentication failure)",
			detail, iscsiStatusDetailAuthFailed)
	}
	requireNoISCSISession(t, cfg.clientNodeA, target, iqnA)

	applyCHAPSecret(t, cfg.installNamespace, nodeSecret, creds)
	waitForPodReady(t, ns, "owner")
	requirePodUsesISCSIDevice(t, ns, "owner", cfg.clientNodeA, target, iqnA, false)
	const payload = "pillar-csi-iscsi-chap-recovered"
	kubectl(t, "-n", ns, "exec", "owner", "--", "sh", "-c",
		fmt.Sprintf("printf '%%s' %q > /data/payload && sync", payload))
	if got := kubectl(t, "-n", ns, "exec", "owner", "--", "cat", "/data/payload"); got != payload {
		t.Fatalf("recovered pod read %q, want %q", got, payload)
	}
}

// TestISCSICHAPProtocolAdmission checks that the API server (CRD CEL and the
// admission webhook) refuses a CHAP PillarProtocol without a secretRef or
// without acl — CHAP credentials live on node ACLs — and admits a complete one.
func TestISCSICHAPProtocolAdmission(t *testing.T) {
	const protocolTemplate = `apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: %s
spec:
  protocol:
    iscsi:
      port: 3260
      acl: %t
      auth:
        method: %s
%s`
	const secretRef = "        secretRef:\n          name: pillar-e2e-chap\n"
	for _, tc := range []struct {
		name    string
		acl     bool
		method  string
		ref     string
		wantErr string
	}{
		{name: "pillar-e2e-chap-no-secret", acl: true, method: "CHAP",
			wantErr: "auth.secretRef is required when auth.method is CHAP or MutualCHAP"},
		{name: "pillar-e2e-mutual-no-acl", acl: false, method: "MutualCHAP", ref: secretRef,
			wantErr: "auth.method CHAP and MutualCHAP require acl: true"},
		{name: "pillar-e2e-chap-valid", acl: true, method: "CHAP", ref: secretRef},
	} {
		err := dryRunApply(t, fmt.Sprintf(protocolTemplate, tc.name, tc.acl, tc.method, tc.ref))
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("PillarProtocol %s rejected: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("PillarProtocol %s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// Helpers shared by the CHAP tests follow.

// createCHAPSecret creates Secret name in namespace holding creds and deletes
// it when the test ends.
func createCHAPSecret(t *testing.T, namespace, name string, creds chapCredentials) string {
	t.Helper()
	applyCHAPSecret(t, namespace, name, creds)
	t.Cleanup(func() { deleteClusterObject(t, "-n", namespace, "secret", name) })
	return name
}

// applyCHAPSecret creates or replaces the content of Secret name.  The
// manifest goes through a file so a failing apply does not print the
// credentials.
func applyCHAPSecret(t *testing.T, namespace, name string, creds chapCredentials) {
	t.Helper()
	var manifest strings.Builder
	fmt.Fprintf(&manifest, `apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    pillar-csi.bhyoo.com/docker-e2e: "true"
type: Opaque
stringData:
  username: %q
  password: %q
`, name, namespace, creds.username, creds.password)
	if creds.mutualUsername != "" {
		fmt.Fprintf(&manifest, "  mutualUsername: %q\n  mutualPassword: %q\n", creds.mutualUsername, creds.mutualPassword)
	}
	path := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(path, []byte(manifest.String()), 0o600); err != nil {
		t.Fatalf("write Secret %s/%s manifest: %v", namespace, name, err)
	}
	kubectl(t, "apply", "-f", path)
}

// createCHAPProtocol creates an acl: true iscsi PillarProtocol authenticating
// with method through secret, waits until it is Ready (the controller
// validated the Secret) and deletes it when the test ends.
func createCHAPProtocol(t *testing.T, name, method, secret string) string {
	t.Helper()
	apply(t, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: %s
  labels:
    pillar-csi.bhyoo.com/docker-e2e: "true"
spec:
  protocol:
    iscsi:
      port: 3260
      acl: true
      auth:
        method: %s
        secretRef:
          name: %s
`, name, method, secret))
	t.Cleanup(func() { deleteClusterObject(t, "pillarprotocol", name) })
	kubectl(t, "wait", "--for=condition=Ready", "pillarprotocol/"+name, "--timeout=2m")
	return name
}

// createCHAPStorageClass creates a CHAP PillarProtocol and a PillarStorageClass
// over the E2E LVM store, waits for the generated StorageClass and returns
// its name.  The PillarStorageClass is deleted (and with it the generated
// StorageClass) before the protocol when the test ends.
func createCHAPStorageClass(t *testing.T, cfg iscsiCHAPConfig, name, method, secret string) string {
	t.Helper()
	protocol := createCHAPProtocol(t, name, method, secret)
	apply(t, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %s
  labels:
    pillar-csi.bhyoo.com/docker-e2e: "true"
spec:
  storeRef: pillar-e2e-store
  protocolRef: %s
  storageClass:
    name: %s
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
  filesystem:
    fsType: ext4
`, name, protocol, name))
	t.Cleanup(func() { deleteClusterObject(t, "pillarstorageclass", name) })
	kubectl(t, "wait", "--for=condition=Ready", "pillarstorageclass/"+name, "--timeout=2m")

	secretName := kubectl(t, "get", "storageclass", name, "-o",
		`jsonpath={.parameters.csi\.storage\.k8s\.io/node-stage-secret-name}`)
	secretNamespace := kubectl(t, "get", "storageclass", name, "-o",
		`jsonpath={.parameters.csi\.storage\.k8s\.io/node-stage-secret-namespace}`)
	if secretName != secret || secretNamespace != cfg.installNamespace {
		t.Fatalf("generated StorageClass %s node-stage secret = %s/%s, want %s/%s",
			name, secretNamespace, secretName, cfg.installNamespace, secret)
	}
	return name
}

// createHandWrittenCHAPStorageClass creates a StorageClass naming the E2E LVM
// store and protocol directly, with nodeSecret as its node-stage Secret, and
// deletes it when the test ends.
func createHandWrittenCHAPStorageClass(t *testing.T, name, protocol, nodeSecret, secretNamespace string) string {
	t.Helper()
	apply(t, fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
  labels:
    pillar-csi.bhyoo.com/docker-e2e: "true"
provisioner: pillar-csi.bhyoo.com
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
parameters:
  pillar-csi.bhyoo.com/store-ref: pillar-e2e-store
  pillar-csi.bhyoo.com/protocol-ref: %s
  csi.storage.k8s.io/fstype: ext4
  csi.storage.k8s.io/node-stage-secret-name: %s
  csi.storage.k8s.io/node-stage-secret-namespace: %s
`, name, protocol, nodeSecret, secretNamespace))
	t.Cleanup(func() { deleteClusterObject(t, "storageclass", name) })
	return name
}

// deleteClusterObject deletes an object the test created and waits for it to
// be gone; a PillarStorageClass waits for its finalizer, which holds until
// no PVC uses the generated StorageClass.
func deleteClusterObject(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	deleteArgs := append([]string{"delete", "--ignore-not-found", "--wait=true", "--timeout=3m"}, args...)
	if _, stderr, err := runKubectl(ctx, deleteArgs...); err != nil {
		t.Errorf("cleanup %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
}

// requirePVCHAPAttributes checks that claim's PV records the auth method in
// its VolumeContext and carries the node-stage Secret reference kubelet
// passes to NodeStage.
func requirePVCHAPAttributes(t *testing.T, namespace, claim, method, secret, secretNamespace string) {
	t.Helper()
	pv := kubectl(t, "-n", namespace, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
	got := kubectl(t, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeAttributes."+
		strings.ReplaceAll(iscsiAuthMethodAttribute, ".", `\.`)+"}")
	if got != method {
		t.Fatalf("PV %s %s = %q, want %q", pv, iscsiAuthMethodAttribute, got, method)
	}
	ref := kubectl(t, "get", "pv", pv, "-o",
		`jsonpath={.spec.csi.nodeStageSecretRef.namespace}{"/"}{.spec.csi.nodeStageSecretRef.name}`)
	if want := secretNamespace + "/" + secret; ref != want {
		t.Fatalf("PV %s nodeStageSecretRef = %q, want %q", pv, ref, want)
	}
}

// requireLIOCHAP checks that target's TPG enforces authentication and that
// the node ACL of initiator carries creds: the CHAP userid/password and, for
// MutualCHAP, the mutual pair (cleared for one-way CHAP).  Values are
// compared, never printed.
func requireLIOCHAP(t *testing.T, target iscsiTarget, initiator string, creds chapCredentials) {
	t.Helper()
	tpg := filepath.Join(lioISCSIRoot, target.iqn, lioTPG)
	if got := readLIOAttribute(t, filepath.Join(tpg, "attrib", "authentication")); got != "1" {
		t.Fatalf("LIO TPG of %s attrib/authentication = %q, want 1", target.iqn, got)
	}
	auth := filepath.Join(tpg, "acls", initiator, "auth")
	for _, field := range []struct {
		name   string
		want   string
		secret bool
	}{
		{name: "userid", want: creds.username},
		{name: "password", want: creds.password, secret: true},
		{name: "userid_mutual", want: creds.mutualUsername},
		{name: "password_mutual", want: creds.mutualPassword, secret: true},
	} {
		got := readLIOAttribute(t, filepath.Join(auth, field.name))
		if got == field.want {
			continue
		}
		if field.secret {
			t.Fatalf("LIO ACL %s of %s auth/%s does not hold the Secret's value", initiator, target.iqn, field.name)
		}
		t.Fatalf("LIO ACL %s of %s auth/%s = %q, want %q", initiator, target.iqn, field.name, got, field.want)
	}
}

// readLIOAttribute reads a host-global LIO configfs attribute.
func readLIOAttribute(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // The test builds every configfs path itself.
	if err != nil {
		t.Fatalf("read LIO attribute %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

// podFailedMountEvents returns the messages of pod's FailedMount events.
func podFailedMountEvents(t *testing.T, namespace, pod string) string {
	t.Helper()
	return kubectl(t, "-n", namespace, "get", "events",
		"--field-selector", "involvedObject.kind=Pod,involvedObject.name="+pod+",reason=FailedMount",
		"-o", `jsonpath={range .items[*]}{.message}{"\n"}{end}`)
}

// requireNoISCSISession checks over several kubelet NodeStage retries that
// node's initiator never holds a session (and therefore no SCSI disk) for
// target.  TCP connections are not checked: each retry opens one for its
// refused login.
func requireNoISCSISession(t *testing.T, node string, target iscsiTarget, iqn string) {
	t.Helper()
	for range 5 {
		state := readISCSINodeState(t, node, target, iqn)
		if len(state.inspectionErrors) != 0 {
			t.Fatalf("iSCSI state inspection on Kind node %q failed: %v", node, state.inspectionErrors)
		}
		if len(state.sessions) != 0 {
			t.Fatalf("Kind node %q holds a session to %s despite a wrong CHAP secret: %s", node, target.iqn, state)
		}
		time.Sleep(3 * time.Second)
	}
}
