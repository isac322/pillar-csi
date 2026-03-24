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

// Package main is the entry point for the pillar-csi controller manager.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/internal/agentclient"
	"github.com/bhyoo/pillar-csi/internal/controller"
	webhookv1alpha1 "github.com/bhyoo/pillar-csi/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(pillarcsiv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// setupControllers registers all pillar-csi controllers with the manager.
// Extracted from main to keep the entry point under the funlen statement limit.
//
// agentDialer is the gRPC connection manager injected into the
// PillarTargetReconciler so that it can perform live HealthCheck calls against
// pillar-agent instances and reflect the results in AgentConnected conditions.
func setupControllers(mgr ctrl.Manager, agentDialer agentclient.Dialer) error {
	err := (&controller.PillarTargetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Dialer: agentDialer,
	}).SetupWithManager(mgr)
	if err != nil {
		return fmt.Errorf("PillarTarget controller: %w", err)
	}
	err = (&controller.PillarPoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		return fmt.Errorf("PillarPool controller: %w", err)
	}
	err = (&controller.PillarProtocolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		return fmt.Errorf("PillarProtocol controller: %w", err)
	}
	err = (&controller.PillarBindingReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		return fmt.Errorf("PillarBinding controller: %w", err)
	}
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		err = webhookv1alpha1.SetupPillarTargetWebhookWithManager(mgr)
		if err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PillarTarget")
			os.Exit(1)
		}
		err = webhookv1alpha1.SetupPillarPoolWebhookWithManager(mgr)
		if err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PillarPool")
			os.Exit(1)
		}
		err = webhookv1alpha1.SetupPillarProtocolWebhookWithManager(mgr)
		if err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PillarProtocol")
			os.Exit(1)
		}
		err = webhookv1alpha1.SetupPillarBindingWebhookWithManager(mgr)
		if err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PillarBinding")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder
	return nil
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	// mTLS flags for the controller→agent gRPC connection.
	// When all three cert flags are provided the controller uses mutual TLS;
	// when they are omitted it falls back to a plaintext connection and logs
	// a warning suitable for development / pre-PKI deployments.
	var agentTLSCert, agentTLSKey, agentTLSCA, agentTLSServerName string

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&agentTLSCert, "agent-tls-cert", "",
		"Path to the PEM-encoded client certificate used for mTLS with pillar-agent. "+
			"Must be set together with --agent-tls-key and --agent-tls-ca to enable mTLS.")
	flag.StringVar(&agentTLSKey, "agent-tls-key", "",
		"Path to the PEM-encoded private key for the agent mTLS client certificate.")
	flag.StringVar(&agentTLSCA, "agent-tls-ca", "",
		"Path to the PEM-encoded CA certificate that signed the pillar-agent server certificates.")
	flag.StringVar(&agentTLSServerName, "agent-tls-server-name", "",
		"Override the TLS server name used for SAN verification when connecting to pillar-agent. "+
			"Leave empty to derive the server name from the resolved agent address.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if webhookCertPath != "" {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if metricsCertPath != "" {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "bf3a431e.pillar-csi.bhyoo.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Create the gRPC connection manager for agent health-checks.
	// The manager caches one *grpc.ClientConn per resolved address and is
	// closed gracefully when the process exits.
	//
	// When --agent-tls-cert, --agent-tls-key, and --agent-tls-ca are all
	// provided, the controller establishes mutually-authenticated TLS
	// connections to every pillar-agent.  Otherwise it falls back to plaintext,
	// which is acceptable only in development or environments where TLS is
	// terminated by an external proxy.
	var agentDialer *agentclient.Manager
	allTLSFlagsSet := agentTLSCert != "" && agentTLSKey != "" && agentTLSCA != ""
	anyTLSFlagSet := agentTLSCert != "" || agentTLSKey != "" || agentTLSCA != ""
	switch {
	case allTLSFlagsSet:
		setupLog.Info("Initializing agent gRPC connection manager with mTLS credentials",
			"cert", agentTLSCert, "key", agentTLSKey, "ca", agentTLSCA,
			"serverName", agentTLSServerName)
		var dialErr error
		agentDialer, dialErr = agentclient.NewManagerFromFiles(
			agentTLSCert, agentTLSKey, agentTLSCA, agentTLSServerName,
		)
		if dialErr != nil {
			setupLog.Error(dialErr, "failed to initialise mTLS agent dialer")
			os.Exit(1)
		}
	case anyTLSFlagSet:
		// Partial flag set — operator misconfiguration; surface a clear error
		// rather than silently ignoring partial input.
		setupLog.Error(fmt.Errorf("incomplete mTLS flags"),
			"--agent-tls-cert, --agent-tls-key, and --agent-tls-ca must all be set "+
				"together to enable mTLS; got a partial set",
			"agent-tls-cert", agentTLSCert, "agent-tls-key", agentTLSKey,
			"agent-tls-ca", agentTLSCA)
		os.Exit(1)
	default:
		setupLog.Info("WARNING: agent gRPC connections will use plaintext transport; " +
			"set --agent-tls-cert, --agent-tls-key, and --agent-tls-ca to enable mTLS")
		agentDialer = agentclient.NewManager()
	}
	defer func() {
		if closeErr := agentDialer.Close(); closeErr != nil {
			setupLog.Error(closeErr, "failed to close agent gRPC connection manager")
		}
	}()

	err = setupControllers(mgr, agentDialer)
	if err != nil {
		setupLog.Error(err, "unable to create controllers")
		os.Exit(1)
	}

	err = mgr.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	err = mgr.AddReadyzCheck("readyz", healthz.Ping)
	if err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	err = mgr.Start(ctrl.SetupSignalHandler())
	if err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
