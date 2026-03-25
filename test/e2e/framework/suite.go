//go:build e2e
// +build e2e

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

package framework

// suite.go — Shared Ginkgo test suite setup/teardown helpers for pillar-csi
// cluster-dependent e2e tests.
//
// Suite encapsulates the state shared across all specs in a single Ginkgo
// suite: the controller-runtime client connected to the test cluster and the
// per-suite configuration (connect timeout, etc.).
//
// Typical usage:
//
//	package mytest_test
//
//	import (
//	    "context"
//	    "testing"
//
//	    . "github.com/onsi/ginkgo/v2"
//	    . "github.com/onsi/gomega"
//	    "github.com/bhyoo/pillar-csi/test/e2e/framework"
//	)
//
//	var suite *framework.Suite
//
//	func TestMyTest(t *testing.T) {
//	    RegisterFailHandler(Fail)
//	    RunSpecs(t, "MyTest Suite")
//	}
//
//	var _ = BeforeSuite(func() {
//	    suite = framework.MustSetupSuite()
//	})
//
//	var _ = AfterSuite(func() {
//	    suite.TeardownSuite()
//	})
//
//	var _ = Describe("my feature", func() {
//	    var (
//	        ctx     context.Context
//	        tracker *framework.ResourceTracker
//	    )
//
//	    BeforeEach(func() {
//	        ctx = context.Background()
//	        tracker = suite.NewTracker()
//	        DeferCleanup(tracker.Cleanup, ctx, suite.Client)
//	    })
//
//	    It("does something", func() { /* … */ })
//	})

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ─────────────────────────────────────────────────────────────────────────────
// Suite
// ─────────────────────────────────────────────────────────────────────────────

// Suite holds state shared across all specs in a single Ginkgo suite.
// Construct it with SetupSuite or MustSetupSuite in BeforeSuite.
type Suite struct {
	// Client is the controller-runtime client connected to the test cluster.
	// Tests should use this directly for Get/List/Create/Delete operations.
	Client client.Client

	// connectTimeout is how long SetupSuite waits for the initial cluster
	// connectivity check.
	connectTimeout time.Duration
}

// ─────────────────────────────────────────────────────────────────────────────
// SuiteOption
// ─────────────────────────────────────────────────────────────────────────────

// SuiteOption customises Suite construction via functional options.
type SuiteOption func(*Suite)

// WithConnectTimeout sets the maximum wait duration for the initial cluster
// connectivity check performed during SetupSuite.  Defaults to 30 s.
//
// Example:
//
//	suite = framework.MustSetupSuite(framework.WithConnectTimeout(60 * time.Second))
func WithConnectTimeout(d time.Duration) SuiteOption {
	return func(s *Suite) { s.connectTimeout = d }
}

// ─────────────────────────────────────────────────────────────────────────────
// SetupSuite / MustSetupSuite
// ─────────────────────────────────────────────────────────────────────────────

// SetupSuite builds a Suite by creating a controller-runtime client from the
// active kubeconfig and verifying that the API server is reachable within the
// connect timeout.
//
// Returns an error if the client cannot be created or if the cluster is not
// reachable within the configured timeout (default 30 s).
//
// Use MustSetupSuite in Ginkgo BeforeSuite blocks where setup failures are
// fatal.
func SetupSuite(opts ...SuiteOption) (*Suite, error) {
	s := &Suite{
		connectTimeout: 30 * time.Second,
	}
	for _, o := range opts {
		o(s)
	}

	c, err := NewClient()
	if err != nil {
		return nil, fmt.Errorf("framework SetupSuite: build client: %w", err)
	}
	s.Client = c

	ctx, cancel := context.WithTimeout(context.Background(), s.connectTimeout)
	defer cancel()
	if err := verifyClusterConnectivity(ctx, c); err != nil {
		return nil, fmt.Errorf("framework SetupSuite: cluster not reachable within %s: %w",
			s.connectTimeout, err)
	}

	return s, nil
}

// MustSetupSuite is like SetupSuite but panics on error.  It is suitable for
// use in Ginkgo BeforeSuite blocks where setup failures are always fatal.
//
// Example:
//
//	var _ = BeforeSuite(func() {
//	    suite = framework.MustSetupSuite()
//	})
func MustSetupSuite(opts ...SuiteOption) *Suite {
	s, err := SetupSuite(opts...)
	if err != nil {
		panic(fmt.Sprintf("framework MustSetupSuite: %v", err))
	}
	return s
}

// ─────────────────────────────────────────────────────────────────────────────
// TeardownSuite
// ─────────────────────────────────────────────────────────────────────────────

// TeardownSuite performs suite-level cleanup.  In the current implementation
// this is a no-op (the controller-runtime client has no persistent resources
// to release), but calling it from AfterSuite is encouraged so that future
// cleanup logic (e.g. closing background goroutines, draining channels) can be
// added without changing call sites.
func (s *Suite) TeardownSuite() {
	// Reserved for future use.
}

// ─────────────────────────────────────────────────────────────────────────────
// Per-spec helpers
// ─────────────────────────────────────────────────────────────────────────────

// NewTracker returns a fresh ResourceTracker.  The tracker can be registered
// with Ginkgo's DeferCleanup so that all resources it accumulates are deleted
// at the end of the spec in which it was created.
//
// Example:
//
//	BeforeEach(func() {
//	    tracker = suite.NewTracker()
//	    DeferCleanup(tracker.Cleanup, ctx, suite.Client)
//	})
func (s *Suite) NewTracker() *ResourceTracker {
	return NewResourceTracker()
}

// CreateTestNamespace creates a uniquely named Namespace using the given
// prefix, registers it with tracker for automatic deletion, and returns the
// created Namespace.  It is a convenience wrapper around
// framework.CreateTestNamespace + tracker.TrackNamespace.
//
// Example:
//
//	ns := suite.CreateTestNamespaceTracked(ctx, tracker, "csi-lifecycle")
//	// ns.Name == "csi-lifecycle-7b4xz" (server-generated suffix)
func (s *Suite) CreateTestNamespaceTracked(
	ctx context.Context,
	tracker *ResourceTracker,
	prefix string,
) (*corev1.Namespace, error) {
	ns, err := CreateTestNamespace(ctx, s.Client, prefix)
	if err != nil {
		return nil, err
	}
	tracker.TrackNamespace(ns.Name)
	return ns, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// verifyClusterConnectivity performs a lightweight, read-only API server call
// to confirm that the cluster is reachable.  It lists the well-known "default"
// Namespace which is present on every Kubernetes cluster and requires only
// basic RBAC read permissions.
func verifyClusterConnectivity(ctx context.Context, c client.Client) error {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: "default"}, ns); err != nil {
		return fmt.Errorf("get default namespace: %w", err)
	}
	return nil
}
