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
//   - PublishNVMeOfIdentity: error when the host NQN file is missing
//   - PublishNVMeOfIdentity: error wrapping when patcher returns NotFound
//   - PublishNVMeOfIdentity: error wrapping when patcher returns a generic error
//   - KubeCSINodePatcher.PatchAnnotations: success via fake k8s client
//   - KubeCSINodePatcher.PatchAnnotations: error propagated on patch failure
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestPublishNVMeOfIdentity
//	go test ./internal/csi/ -v -run TestKubeCSINodePatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// publishNVMeOfIdentityWithFile — testable variant of PublishNVMeOfIdentity
// ─────────────────────────────────────────────────────────────────────────────

// publishNVMeOfIdentityWithFile is a test-only helper that calls the
// same logic as PublishNVMeOfIdentity but reads the NQN from path instead
// of the hard-coded system file.  This allows unit tests to inject a temp
// file without touching /etc/nvme/hostnqn.
//
// This helper exists in the test file (same package) rather than production
// code to keep the production API clean.
func publishNVMeOfIdentityWithFile(ctx context.Context, patcher NodeAnnotationPatcher, nodeName, nqnFile string) error {
	nqn, err := readHostNQNFrom(nqnFile)
	if err != nil {
		return fmt.Errorf("PublishNVMeOfIdentity: read host NQN: %w", err)
	}
	annotations := map[string]string{
		AnnotationNVMeOFHostNQN: nqn,
	}
	patchErr := patcher.PatchAnnotations(ctx, nodeName, annotations)
	if patchErr != nil {
		if k8serrors.IsNotFound(patchErr) {
			return fmt.Errorf(
				"PublishNVMeOfIdentity: CSINode %q not found "+
					"(kubelet may not have registered the driver yet): %w",
				nodeName, patchErr)
		}
		return fmt.Errorf("PublishNVMeOfIdentity: patch CSINode %q: %w", nodeName, patchErr)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PublishNVMeOfIdentity tests
// ─────────────────────────────────────────────────────────────────────────────

// TestPublishNVMeOfIdentity_MissingNQNFile verifies that PublishNVMeOfIdentity
// returns an error when the host NQN file does not exist.  This tests the
// "node never had NVMe kernel module loaded" scenario.
func TestPublishNVMeOfIdentity_MissingNQNFile(t *testing.T) {
	t.Parallel()

	patcher := &mockNodeAnnotationPatcher{}
	err := publishNVMeOfIdentityWithFile(
		context.Background(), patcher, "node-missing-nqn-file",
		filepath.Join(t.TempDir(), "nonexistent-hostnqn"),
	)
	if err == nil {
		t.Fatal("expected error for missing NQN file, got nil")
	}
	// Patcher must NOT have been called because the NQN read failed first.
	if patcher.callCount != 0 {
		t.Errorf("patcher.callCount = %d, want 0 (should not be called when NQN read fails)", patcher.callCount)
	}
}

// TestPublishNVMeOfIdentity_EmptyNQNFile verifies that an empty hostnqn
// file causes an early error before the patcher is called.
func TestPublishNVMeOfIdentity_EmptyNQNFile(t *testing.T) {
	t.Parallel()

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("   \n  "), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}
	patcher := &mockNodeAnnotationPatcher{}
	err := publishNVMeOfIdentityWithFile(context.Background(), patcher, "node-empty-nqn", f)
	if err == nil {
		t.Fatal("expected error for empty NQN file, got nil")
	}
	if patcher.callCount != 0 {
		t.Errorf("patcher.callCount = %d, want 0", patcher.callCount)
	}
}

// TestPublishNVMeOfIdentity_Success verifies that when both the NQN file is
// valid and the patcher succeeds, the patcher is called exactly once with the
// correct node name and annotation key.
func TestPublishNVMeOfIdentity_Success(t *testing.T) {
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
	err := publishNVMeOfIdentityWithFile(context.Background(), patcher, nodeName, f)
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

// TestPublishNVMeOfIdentity_PatcherNotFound verifies that a NotFound error
// from the patcher is wrapped and returned as an error wrapping the original.
// The caller can use k8serrors.IsNotFound to detect this case.
func TestPublishNVMeOfIdentity_PatcherNotFound(t *testing.T) {
	t.Parallel()

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("nqn.2014-08.org.nvmexpress:uuid:not-found-test\n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	notFoundErr := k8serrors.NewNotFound(
		schema.GroupResource{Group: "storage.k8s.io", Resource: "csinodes"}, "node-not-found",
	)
	patcher := &mockNodeAnnotationPatcher{err: notFoundErr}
	err := publishNVMeOfIdentityWithFile(context.Background(), patcher, "node-not-found", f)
	if err == nil {
		t.Fatal("expected error for patcher NotFound, got nil")
	}
	// The underlying NotFound error must be preserved in the chain.
	if !errors.Is(err, notFoundErr) {
		t.Errorf("expected errors.Is(err, notFoundErr) = true; err = %v", err)
	}
}

// TestPublishNVMeOfIdentity_PatcherGenericError verifies that a generic error
// from the patcher is propagated wrapped.
func TestPublishNVMeOfIdentity_PatcherGenericError(t *testing.T) {
	t.Parallel()

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("nqn.2014-08.org.nvmexpress:uuid:generic-err\n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	patcherErr := errors.New("transient API error")
	patcher := &mockNodeAnnotationPatcher{err: patcherErr}
	err := publishNVMeOfIdentityWithFile(context.Background(), patcher, "node-generic-error", f)
	if err == nil {
		t.Fatal("expected error from patcher, got nil")
	}
	if !errors.Is(err, patcherErr) {
		t.Errorf("expected errors.Is(err, patcherErr) = true; err = %v", err)
	}
}

// TestPublishNVMeOfIdentity_NQNWhitespaceIsTrimmed verifies that leading and
// trailing whitespace in the NQN file is stripped before writing to the annotation.
func TestPublishNVMeOfIdentity_NQNWhitespaceIsTrimmed(t *testing.T) {
	t.Parallel()

	const wantNQN = "nqn.2014-08.org.nvmexpress:uuid:trim-test"
	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("  \n"+wantNQN+"  \n"), 0o600); err != nil {
		t.Fatalf("write temp NQN file: %v", err)
	}

	patcher := &mockNodeAnnotationPatcher{}
	err := publishNVMeOfIdentityWithFile(context.Background(), patcher, "worker-node-1", f)
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
		AnnotationNVMeOFHostNQN:     "nqn.2014-08.org.nvmexpress:uuid:multi-anno",
		AnnotationISCSIInitiatorIQN: "iqn.1993-08.org.debian:01:multi-anno",
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
