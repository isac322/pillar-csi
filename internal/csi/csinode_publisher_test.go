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

package csi

// Unit tests for the CSINode annotation publisher (csinode_publisher.go).
//
// Tests cover:
//   - PublishNodeIdentity: generate-and-persist when the host NQN file is missing
//   - PublishNodeIdentity: regenerate when the host NQN file is empty
//   - PublishNodeIdentity: iSCSI initiator IQN published only when enabled
//   - PublishNodeIdentity: error wrapping when patcher returns NotFound
//   - PublishNodeIdentity: error wrapping when patcher returns a generic error
//   - KubeCSINodePatcher.PatchAnnotations: success via fake k8s client
//   - KubeCSINodePatcher.PatchAnnotations: error propagated on patch failure
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestPublishNodeIdentity
//	go test ./internal/csi/ -v -run TestKubeCSINodePatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// ─────────────────────────────────────────────────────────────────────────────
// mockNodeAnnotationPatcher — test double for NodeAnnotationPatcher
// ─────────────────────────────────────────────────────────────────────────────

// mockNodeAnnotationPatcher is a configurable test double for
// NodeAnnotationPatcher.  It records the most recent call arguments and
// returns the pre-configured error.
type mockNodeAnnotationPatcher struct {
	err             error
	lastNodeName    string
	lastAnnotations map[string]string
	callCount       int
}

// Compile-time check.
var _ NodeAnnotationPatcher = (*mockNodeAnnotationPatcher)(nil)

func (m *mockNodeAnnotationPatcher) PatchAnnotations(
	_ context.Context,
	nodeName string,
	annotations map[string]string,
) error {
	m.callCount++
	m.lastNodeName = nodeName
	m.lastAnnotations = annotations
	return m.err
}

// ─────────────────────────────────────────────────────────────────────────────
// PublishNodeIdentity tests
// ─────────────────────────────────────────────────────────────────────────────

// TestPublishNodeIdentity_MissingNQNFileGenerates verifies that
// PublishNodeIdentity transparently generates a host NQN when the file is
// absent, persists it, and publishes it through the patcher.  This is the
// out-of-the-box path on containerized hosts where nvme-cli has not seeded
// /etc/nvme/hostnqn.
func TestPublishNodeIdentity_MissingNQNFileGenerates(t *testing.T) {
	t.Parallel()

	patcher := &mockNodeAnnotationPatcher{}
	dir := t.TempDir()
	f := filepath.Join(dir, "nvme", "hostnqn")

	err := publishNodeIdentity(
		context.Background(), patcher, "node-missing-nqn-file", f, "",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patcher.callCount != 1 {
		t.Fatalf("patcher.callCount = %d, want 1", patcher.callCount)
	}
	gotNQN := patcher.lastAnnotations[AnnotationNVMeOFHostNQN]
	if !strings.HasPrefix(gotNQN, hostNQNUUIDPrefix) {
		t.Errorf("published NQN %q does not start with %q", gotNQN, hostNQNUUIDPrefix)
	}
	if _, statErr := os.Stat(f); statErr != nil {
		t.Errorf("generated NQN file was not persisted: %v", statErr)
	}
}

// TestPublishNodeIdentity_EmptyNQNFileRegenerates verifies that an existing
// but empty hostnqn file is overwritten with a freshly-generated NQN rather
// than surfacing as an error.
func TestPublishNodeIdentity_EmptyNQNFileRegenerates(t *testing.T) {
	t.Parallel()

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("   \n  "), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}
	patcher := &mockNodeAnnotationPatcher{}
	err := publishNodeIdentity(context.Background(), patcher, "node-empty-nqn", f, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patcher.callCount != 1 {
		t.Fatalf("patcher.callCount = %d, want 1", patcher.callCount)
	}
	gotNQN := patcher.lastAnnotations[AnnotationNVMeOFHostNQN]
	if !strings.HasPrefix(gotNQN, hostNQNUUIDPrefix) {
		t.Errorf("published NQN %q does not start with %q", gotNQN, hostNQNUUIDPrefix)
	}
}

// TestPublishNodeIdentity_Success verifies that when both the NQN file is
// valid and the patcher succeeds, the patcher is called exactly once with the
// correct node name and annotation key.
func TestPublishNodeIdentity_Success(t *testing.T) {
	t.Parallel()

	const (
		wantNQN  = "nqn.2014-08.org.nvmexpress:uuid:publish-success-test"
		nodeName = "worker-node-1"
	)

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte(wantNQN+"\n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	patcher := &mockNodeAnnotationPatcher{}
	err := publishNodeIdentity(context.Background(), patcher, nodeName, f, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if patcher.callCount != 1 {
		t.Errorf("patcher.callCount = %d, want 1", patcher.callCount)
	}
	if patcher.lastNodeName != nodeName {
		t.Errorf("patcher.lastNodeName = %q, want %q", patcher.lastNodeName, nodeName)
	}
	gotNQN := patcher.lastAnnotations[AnnotationNVMeOFHostNQN]
	if gotNQN != wantNQN {
		t.Errorf("annotation[%q] = %q, want %q", AnnotationNVMeOFHostNQN, gotNQN, wantNQN)
	}
}

// TestPublishNodeIdentity_ISCSIInitiatorIQN verifies that the iSCSI initiator
// IQN is published in the same patch as the host NQN when the iSCSI handler
// is enabled, and that no iSCSI annotation is written when it is disabled
// (so the controller never whitelists an initiator that cannot log in).
func TestPublishNodeIdentity_ISCSIInitiatorIQN(t *testing.T) {
	t.Parallel()

	const (
		wantNQN = "nqn.2014-08.org.nvmexpress:uuid:iscsi-publish"
		wantIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
	)
	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte(wantNQN+"\n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	enabled := &mockNodeAnnotationPatcher{}
	if err := publishNodeIdentity(context.Background(), enabled, "worker-iscsi", f, wantIQN); err != nil {
		t.Fatalf("publish with iSCSI enabled: %v", err)
	}
	if enabled.callCount != 1 {
		t.Fatalf("patcher.callCount = %d, want 1 (single merge patch)", enabled.callCount)
	}
	if got := enabled.lastAnnotations[AnnotationISCSIInitiatorIQN]; got != wantIQN {
		t.Errorf("annotation[%q] = %q, want %q", AnnotationISCSIInitiatorIQN, got, wantIQN)
	}
	if got := enabled.lastAnnotations[AnnotationNVMeOFHostNQN]; got != wantNQN {
		t.Errorf("annotation[%q] = %q, want %q", AnnotationNVMeOFHostNQN, got, wantNQN)
	}

	disabled := &mockNodeAnnotationPatcher{}
	if err := publishNodeIdentity(context.Background(), disabled, "worker-no-iscsi", f, ""); err != nil {
		t.Fatalf("publish with iSCSI disabled: %v", err)
	}
	if _, ok := disabled.lastAnnotations[AnnotationISCSIInitiatorIQN]; ok {
		t.Errorf("iSCSI disabled: annotation %q must not be published: %v",
			AnnotationISCSIInitiatorIQN, disabled.lastAnnotations)
	}
}

// TestPublishNodeIdentity_PatcherNotFound verifies that a NotFound error
// from the patcher is wrapped and returned as an error wrapping the original.
// The caller can use k8serrors.IsNotFound to detect this case.
func TestPublishNodeIdentity_PatcherNotFound(t *testing.T) {
	t.Parallel()

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("nqn.2014-08.org.nvmexpress:uuid:not-found-test\n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	notFoundErr := k8serrors.NewNotFound(
		schema.GroupResource{Group: "storage.k8s.io", Resource: "csinodes"}, "node-not-found",
	)
	patcher := &mockNodeAnnotationPatcher{err: notFoundErr}
	err := publishNodeIdentity(context.Background(), patcher, "node-not-found", f, "")
	if err == nil {
		t.Fatal("expected error for patcher NotFound, got nil")
	}
	// The underlying NotFound error must be preserved in the chain.
	if !errors.Is(err, notFoundErr) {
		t.Errorf("expected errors.Is(err, notFoundErr) = true; err = %v", err)
	}
}

// TestPublishNodeIdentity_PatcherGenericError verifies that a generic error
// from the patcher is propagated wrapped.
func TestPublishNodeIdentity_PatcherGenericError(t *testing.T) {
	t.Parallel()

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("nqn.2014-08.org.nvmexpress:uuid:generic-err\n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	patcherErr := errors.New("transient API error")
	patcher := &mockNodeAnnotationPatcher{err: patcherErr}
	err := publishNodeIdentity(context.Background(), patcher, "node-generic-error", f, "")
	if err == nil {
		t.Fatal("expected error from patcher, got nil")
	}
	if !errors.Is(err, patcherErr) {
		t.Errorf("expected errors.Is(err, patcherErr) = true; err = %v", err)
	}
}

// TestPublishNodeIdentity_NQNWhitespaceIsTrimmed verifies that leading and
// trailing whitespace in the NQN file is stripped before writing to the annotation.
func TestPublishNodeIdentity_NQNWhitespaceIsTrimmed(t *testing.T) {
	t.Parallel()

	const wantNQN = "nqn.2014-08.org.nvmexpress:uuid:trim-test"
	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("  \n"+wantNQN+"  \n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	patcher := &mockNodeAnnotationPatcher{}
	err := publishNodeIdentity(context.Background(), patcher, "worker-node-1", f, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotNQN := patcher.lastAnnotations[AnnotationNVMeOFHostNQN]
	if gotNQN != wantNQN {
		t.Errorf("annotation NQN = %q, want trimmed %q", gotNQN, wantNQN)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// KubeCSINodePatcher tests
// ─────────────────────────────────────────────────────────────────────────────

// TestKubeCSINodePatcher_PatchAnnotations_Success verifies that
// KubeCSINodePatcher.PatchAnnotations calls the Kubernetes API without error
// when the CSINode object exists.
func TestKubeCSINodePatcher_PatchAnnotations_Success(t *testing.T) {
	t.Parallel()

	const (
		nodeName = "worker-node-1"
		wantNQN  = "nqn.2014-08.org.nvmexpress:uuid:patcher-test"
	)

	// Create the fake client pre-seeded with a CSINode.
	existingCSINode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	}
	fakeClient := kubefake.NewSimpleClientset(existingCSINode)
	patcher := NewKubeCSINodePatcher(fakeClient)

	err := patcher.PatchAnnotations(context.Background(), nodeName, map[string]string{
		AnnotationNVMeOFHostNQN: wantNQN,
	})
	if err != nil {
		t.Fatalf("PatchAnnotations returned unexpected error: %v", err)
	}
}

// TestKubeCSINodePatcher_PatchAnnotations_MultipleAnnotations verifies that
// PatchAnnotations can write multiple annotation keys in a single call.
func TestKubeCSINodePatcher_PatchAnnotations_MultipleAnnotations(t *testing.T) {
	t.Parallel()

	const nodeName = "multi-anno-node"
	existingCSINode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	}
	fakeClient := kubefake.NewSimpleClientset(existingCSINode)
	patcher := NewKubeCSINodePatcher(fakeClient)

	annotations := map[string]string{
		AnnotationNVMeOFHostNQN:        "nqn.2014-08.org.nvmexpress:uuid:multi-anno",
		"example.com/other-annotation": "kept",
	}
	err := patcher.PatchAnnotations(context.Background(), nodeName, annotations)
	if err != nil {
		t.Fatalf("PatchAnnotations with multiple annotations returned unexpected error: %v", err)
	}
}

// TestKubeCSINodePatcher_PatchAnnotations_NodeNotFound verifies that
// PatchAnnotations returns an error (wrapping a NotFound status) when the
// named CSINode does not exist.
func TestKubeCSINodePatcher_PatchAnnotations_NodeNotFound(t *testing.T) {
	t.Parallel()

	// Empty fake client — CSINode does not exist.
	fakeClient := kubefake.NewSimpleClientset()
	patcher := NewKubeCSINodePatcher(fakeClient)

	err := patcher.PatchAnnotations(context.Background(), "nonexistent-node", map[string]string{
		AnnotationNVMeOFHostNQN: "nqn.2014-08.org.nvmexpress:uuid:nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent CSINode, got nil")
	}
	// Verify the error wraps a NotFound condition.
	if !k8serrors.IsNotFound(err) {
		t.Errorf("expected k8serrors.IsNotFound(err) = true; err = %v", err)
	}
}

// TestKubeCSINodePatcher_PatchAnnotations_PreservesSpecDrivers is the
// regression test for issue #128: "node plugin restart wipes CSINode driver
// registrations".
//
// The bug: PatchAnnotations marshaled a whole storagev1.CSINode carrying
// only ObjectMeta.Annotations.  CSINodeSpec.Drivers has no omitempty, so the
// body was {"metadata":{"annotations":{…}},"spec":{"drivers":null},"status":{}}.
// Under a strategic merge patch "drivers":null deletes the entire
// spec.drivers list, so every node-plugin (re)start wiped every CSI driver
// registration kubelet had written — including other drivers — and broke
// attach with "CSINode <node> does not contain driver pillar-csi.bhyoo.com".
// The fake clientset applies real strategic-merge/merge patch semantics
// (client-go testing fixture → strategicpatch.StrategicMergePatch /
// jsonpatch.MergePatch), so this test would have caught the bug.
//
// The test seeds a CSINode whose spec.drivers already contains pillar-csi AND
// an unrelated driver (as kubelet leaves it after registration), patches the
// NQN annotation twice (initial publish + node-plugin restart), and asserts
// both drivers and a pre-existing annotation survive while the NQN
// annotation is written.
func TestKubeCSINodePatcher_PatchAnnotations_PreservesSpecDrivers(t *testing.T) {
	t.Parallel()

	const (
		nodeName        = "macmini"
		wantNQN         = "nqn.2014-08.org.nvmexpress:uuid:issue-128"
		otherDriver     = "org.democratic-csi.iscsi"
		pillarDriver    = "pillar-csi.bhyoo.com"
		otherAnnotation = "example.com/unrelated"
		otherAnnoValue  = "must-survive"
	)

	// Seed the CSINode the way kubelet leaves it: spec.drivers populated for
	// our driver and an unrelated one, plus an unrelated annotation.
	seeded := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{otherAnnotation: otherAnnoValue},
		},
		Spec: storagev1.CSINodeSpec{
			Drivers: []storagev1.CSINodeDriver{
				{
					Name:         pillarDriver,
					NodeID:       nodeName,
					TopologyKeys: []string{"kubernetes.io/hostname"},
				},
				{
					Name:   otherDriver,
					NodeID: "iqn.2003-01.org.other:node",
				},
			},
		},
	}
	fakeClient := kubefake.NewSimpleClientset(seeded)
	patcher := NewKubeCSINodePatcher(fakeClient)

	// Patch twice: initial startup publish, then a node-plugin restart
	// (the sequence that wiped spec.drivers in #128).
	for attempt := 1; attempt <= 2; attempt++ {
		err := patcher.PatchAnnotations(context.Background(), nodeName, map[string]string{
			AnnotationNVMeOFHostNQN: wantNQN,
		})
		if err != nil {
			t.Fatalf("PatchAnnotations attempt %d returned unexpected error: %v", attempt, err)
		}
	}

	got, err := fakeClient.StorageV1().CSINodes().Get(
		context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CSINode %q: %v", nodeName, err)
	}

	// The NQN annotation was written...
	if gotNQN := got.Annotations[AnnotationNVMeOFHostNQN]; gotNQN != wantNQN {
		t.Errorf("annotation[%q] = %q, want %q", AnnotationNVMeOFHostNQN, gotNQN, wantNQN)
	}
	// ...without touching the pre-existing annotation...
	if gotOther := got.Annotations[otherAnnotation]; gotOther != otherAnnoValue {
		t.Errorf("unrelated annotation[%q] = %q, want %q", otherAnnotation, gotOther, otherAnnoValue)
	}
	// ...and without wiping spec.drivers (#128).
	if len(got.Spec.Drivers) != 2 {
		t.Fatalf("spec.drivers wiped by annotation patch (#128): got %d entries %+v, want 2",
			len(got.Spec.Drivers), got.Spec.Drivers)
	}
	byName := make(map[string]storagev1.CSINodeDriver, len(got.Spec.Drivers))
	for _, d := range got.Spec.Drivers {
		byName[d.Name] = d
	}
	pillar, ok := byName[pillarDriver]
	if !ok {
		t.Fatalf("spec.drivers missing %q after annotation patch (#128): %+v",
			pillarDriver, got.Spec.Drivers)
	}
	if pillar.NodeID != nodeName || len(pillar.TopologyKeys) != 1 {
		t.Errorf("driver %q fields mangled by patch: %+v", pillarDriver, pillar)
	}
	if _, ok := byName[otherDriver]; !ok {
		t.Errorf("unrelated driver %q wiped by annotation patch (#128): %+v",
			otherDriver, got.Spec.Drivers)
	}
}
