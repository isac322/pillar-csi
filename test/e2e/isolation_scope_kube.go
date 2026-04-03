package e2e

// isolation_scope_kube.go — per-TC Kubernetes isolation helpers.
//
// This file extends TestCaseScope with three capabilities that together
// ensure no two parallel test cases ever share Kubernetes mutable state:
//
//  1. CopyKubeconfigFrom — copies a source kubeconfig file to the TC-private
//     kubeconfig path so child processes (kubectl, helmfile, …) spawned by
//     this TC use an exclusively-owned kubeconfig file that no other TC can
//     modify.
//
//  2. BuildRestConfig — constructs a *rest.Config from the TC-private
//     kubeconfig file.  Callers should write the kubeconfig first via
//     CopyKubeconfigFrom, then call BuildRestConfig to obtain the config
//     ready for use with client-go or controller-runtime.
//
//  3. RegisterKubernetesNamespace / KubernetesNamespace — stores and
//     retrieves the UUID-based Kubernetes namespace name that was created by
//     framework/namespace for this TC.  The name is stored inside the scope
//     so the It() body can call scope.KubernetesNamespace() without
//     capturing a second binding variable from UseKubeNamespaceLifecycle.
//
// # Isolation contract
//
// Every TC that requires cluster access MUST:
//
//  1. Obtain a private kubeconfig via CopyKubeconfigFrom (or write its own).
//  2. Build a private rest.Config via BuildRestConfig rather than reusing the
//     shared suiteRestConfig pointer (which is immutable but shared).
//  3. Create Kubernetes objects only inside the UUID-based namespace returned
//     by scope.KubernetesNamespace() (or by the framework/namespace package).
//
// # /tmp constraint
//
// All kubeconfig paths returned by KubeconfigPath / CopyKubeconfigFrom are
// rooted under /tmp (os.TempDir()).  assertPathUnderTempRoot is called on
// every path before any file I/O to enforce this at runtime.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// CopyKubeconfigFrom copies the kubeconfig file at srcPath into this TC's
// private kubeconfig path for the given label.
//
// The destination path is obtained from scope.KubeconfigPath(label); any
// existing file at that path is removed first (RecreateKubeconfigPath
// semantics).  The parent directory is created if it does not exist.
//
// Use this when a TC spawns a subprocess (kubectl, helm, csi-controller,
// pillar-agent) that reads KUBECONFIG from the environment: set
// KUBECONFIG=destPath in the subprocess's environment so it reads only its
// own kubeconfig copy and cannot observe in-place mutations made by a
// concurrently running TC.
//
// The destination path stays under os.TempDir() and is cleaned up when
// scope.Close() removes the entire RootDir tree.
//
// Returns the absolute destination path on success.
func (s *TestCaseScope) CopyKubeconfigFrom(label, srcPath string) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("test case scope is closed")
	}
	s.mu.Unlock()

	srcPath = strings.TrimSpace(srcPath)
	if srcPath == "" {
		return "", fmt.Errorf("CopyKubeconfigFrom %s/%s: source kubeconfig path is empty", s.TCID, label)
	}

	// Ensure destination directory exists and remove any stale copy.
	destPath, err := s.RecreateKubeconfigPath(label)
	if err != nil {
		return "", fmt.Errorf("CopyKubeconfigFrom %s/%s: prepare dest path: %w", s.TCID, label, err)
	}

	assertPathUnderTempRoot(destPath)

	// Open source.
	src, err := os.Open(srcPath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("CopyKubeconfigFrom %s/%s: open source %q: %w", s.TCID, label, srcPath, err)
	}
	defer src.Close() //nolint:errcheck

	// Create destination.
	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("CopyKubeconfigFrom %s/%s: create dest %q: %w", s.TCID, label, destPath, err)
	}
	defer dst.Close() //nolint:errcheck

	if _, err := io.Copy(dst, src); err != nil {
		return "", fmt.Errorf("CopyKubeconfigFrom %s/%s: copy %q → %q: %w", s.TCID, label, srcPath, destPath, err)
	}

	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("CopyKubeconfigFrom %s/%s: flush dest %q: %w", s.TCID, label, destPath, err)
	}

	return destPath, nil
}

// WriteKubeconfig writes content to this TC's private kubeconfig path for
// the given label.  Any existing file at that path is overwritten.
//
// Use this when a TC generates a kubeconfig from scratch (e.g. for an
// in-process mock cluster or envtest) rather than copying the suite
// kubeconfig.
//
// Returns the absolute destination path on success.
func (s *TestCaseScope) WriteKubeconfig(label string, content []byte) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("test case scope is closed")
	}
	s.mu.Unlock()

	destPath, err := s.RecreateKubeconfigPath(label)
	if err != nil {
		return "", fmt.Errorf("WriteKubeconfig %s/%s: prepare path: %w", s.TCID, label, err)
	}

	assertPathUnderTempRoot(destPath)

	if err := os.WriteFile(destPath, content, 0o600); err != nil {
		return "", fmt.Errorf("WriteKubeconfig %s/%s: write %q: %w", s.TCID, label, destPath, err)
	}

	return destPath, nil
}

// BuildRestConfig constructs a *rest.Config from the TC-private kubeconfig
// file written under the given label (via CopyKubeconfigFrom or
// WriteKubeconfig).
//
// The kubeconfig file must exist and be valid before BuildRestConfig is
// called.  BuildRestConfig is stateless — it re-reads and re-parses the
// kubeconfig file on every call, so any change made to the file between
// calls is reflected in the returned config.
//
// The returned config connects to the cluster described by the
// current-context in the kubeconfig.  For Kind clusters this is typically
// "kind-<cluster-name>".
//
// Returns an error if the kubeconfig path is empty, the file does not
// exist, or clientcmd cannot parse the file.
func (s *TestCaseScope) BuildRestConfig(label string) (*rest.Config, error) {
	if s == nil {
		return nil, errors.New("BuildRestConfig: scope is nil")
	}

	kubeconfigPath := s.KubeconfigPath(label)
	if strings.TrimSpace(kubeconfigPath) == "" {
		return nil, fmt.Errorf("BuildRestConfig %s/%s: kubeconfig path is empty", s.TCID, label)
	}
	assertPathUnderTempRoot(kubeconfigPath)

	if _, err := os.Stat(kubeconfigPath); err != nil {
		return nil, fmt.Errorf("BuildRestConfig %s/%s: kubeconfig file %q: %w", s.TCID, label, kubeconfigPath, err)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("BuildRestConfig %s/%s: parse %q: %w", s.TCID, label, kubeconfigPath, err)
	}
	return cfg, nil
}

// ─── Kubernetes namespace binding ────────────────────────────────────────────

// RegisterKubernetesNamespace stores a UUID-based Kubernetes namespace name
// (created by framework/namespace or another namespace factory) within the
// scope so that test bodies can retrieve it via KubernetesNamespace() without
// having to capture a separate binding variable.
//
// Each label maps to exactly one namespace name.  Registering the same label
// twice returns an error unless the names are identical (idempotent
// registration for the same factory call).
//
// The namespace name is NOT created or deleted by this method — it is purely
// a name registration so the scope can surface it through a single API.
func (s *TestCaseScope) RegisterKubernetesNamespace(label, namespaceName string) error {
	if s == nil {
		return errors.New("RegisterKubernetesNamespace: scope is nil")
	}

	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("RegisterKubernetesNamespace %s: label is required", s.TCID)
	}
	namespaceName = strings.TrimSpace(namespaceName)
	if namespaceName == "" {
		return fmt.Errorf("RegisterKubernetesNamespace %s/%s: namespace name is required", s.TCID, label)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("RegisterKubernetesNamespace: scope is closed")
	}

	key := "kube-ns:" + pathToken(label)

	existing, found := s.backendObjects[key]
	if found {
		if existing.Name == namespaceName {
			return nil // idempotent: same name, same label
		}
		return fmt.Errorf("RegisterKubernetesNamespace %s/%s: already registered as %q, cannot register as %q",
			s.TCID, label, existing.Name, namespaceName)
	}

	// Reuse backendObjects map as a generic string-keyed store by encoding the
	// namespace name in the BackendObject.Name field.  No filesystem directory
	// is created (RootDir is empty) — this is purely a name registry entry.
	s.backendObjects[key] = &BackendObject{
		TCID:     s.TCID,
		ScopeTag: s.ScopeTag,
		Kind:     "kubernetes-namespace",
		Label:    label,
		Name:     namespaceName,
		RootDir:  "",
	}
	return nil
}

// KubernetesNamespace returns the Kubernetes namespace name registered for
// the given label via RegisterKubernetesNamespace.
//
// Returns an empty string if no namespace has been registered for the label,
// or if the scope is nil or closed.
func (s *TestCaseScope) KubernetesNamespace(label string) string {
	if s == nil {
		return ""
	}

	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ""
	}

	key := "kube-ns:" + pathToken(label)
	obj, found := s.backendObjects[key]
	if !found {
		return ""
	}
	return obj.Name
}
