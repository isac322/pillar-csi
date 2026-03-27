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

// Package framework provides test helpers for pillar-csi e2e tests that
// require a live Kubernetes cluster.  It wraps controller-runtime client
// operations to create, delete, and wait on Custom Resources.
//
// All symbols in this package are compiled only when the "e2e" build tag is
// active (go test -tags=e2e …).  Do not call any function from this package
// in unit tests.
//
// Typical usage in a Ginkgo BeforeSuite:
//
//	var client client.Client
//
//	var _ = BeforeSuite(func() {
//	    var err error
//	    client, err = framework.NewClient()
//	    Expect(err).NotTo(HaveOccurred())
//	})
package framework

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// testClientQPS and testClientBurst set generous rate-limiter parameters on
// the controller-runtime clients used by e2e tests.  The default values
// (QPS=5, Burst=10) are designed for production controllers; they are far too
// restrictive for tests that make many sequential API calls in tight loops.
// Using higher values prevents spurious "client rate limiter Wait returned an
// error: context deadline exceeded" failures when many test specs run back to
// back on the same client instance.
const (
	testClientQPS   = 100
	testClientBurst = 200
)

// Scheme contains all pillar-csi Custom Resource types (PillarTarget,
// PillarPool, PillarProtocol, PillarBinding, PillarVolume) plus the core
// Kubernetes API types (Pods, Services, …).
//
// Pass it to NewClient, or use it when constructing a fake client for
// lower-level unit tests.
var Scheme *runtime.Scheme

func init() {
	Scheme = runtime.NewScheme()

	if err := clientgoscheme.AddToScheme(Scheme); err != nil {
		panic(fmt.Sprintf("framework: register client-go scheme: %v", err))
	}
	if err := v1alpha1.AddToScheme(Scheme); err != nil {
		panic(fmt.Sprintf("framework: register v1alpha1 scheme: %v", err))
	}
}

// NewClient creates a controller-runtime client that connects to the cluster
// identified by the active kubeconfig.  The KUBECONFIG environment variable
// is consulted first; if absent, ~/.kube/config is used (same precedence as
// kubectl).
func NewClient() (client.Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	)

	restConfig, err := cfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("framework: load kubeconfig: %w", err)
	}

	// Override rate-limiter settings: tests make many calls in tight loops and
	// the production defaults (QPS=5, burst=10) cause spurious rate-limiter
	// timeouts when many BeforeAll / It blocks execute back-to-back.
	restConfig.QPS = testClientQPS
	restConfig.Burst = testClientBurst

	c, err := client.New(restConfig, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, fmt.Errorf("framework: build client: %w", err)
	}
	return c, nil
}

// MustNewClient is like NewClient but panics on error.  Suitable for
// BeforeSuite or TestMain where a missing cluster connection is fatal.
func MustNewClient() client.Client {
	c, err := NewClient()
	if err != nil {
		panic(fmt.Sprintf("framework: MustNewClient: %v", err))
	}
	return c
}
