package telemetry

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
)

// ExemplarFromContext returns a {trace_id} exemplar for the span in ctx when
// it is sampled, else nil (an unsampled trace ID points to no stored trace).
// Its signature matches the go-grpc-middleware prometheus provider's
// WithExemplarFromContext.
func ExemplarFromContext(ctx context.Context) prometheus.Labels {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsSampled() {
		return nil
	}
	return prometheus.Labels{"trace_id": sc.TraceID().String()}
}

// observeWithExemplar observes v on o, attaching [ExemplarFromContext] when
// available.
func observeWithExemplar(ctx context.Context, o prometheus.Observer, v float64) {
	if ex := ExemplarFromContext(ctx); ex != nil {
		if eo, ok := o.(prometheus.ExemplarObserver); ok {
			eo.ObserveWithExemplar(v, ex)
			return
		}
	}
	o.Observe(v)
}

// BuildVersion returns the module version and VCS revision embedded in the
// binary. Version is "dev" for (devel) or unset builds, the same rule the
// controller and node mains use; revision is "unknown" when absent.
func BuildVersion() (version, revision string) {
	version, revision = "dev", "unknown"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version, revision
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			revision = s.Value
		}
	}
	return version, revision
}

// BuildInfoCollector returns pillar_csi_build_info (M18), a constant 1 gauge
// with component, version, VCS revision and Go version labels.
func BuildInfoCollector(component, version string) prometheus.Collector {
	_, revision := BuildVersion()
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pillar_csi_build_info",
		Help: "A constant 1 with the pillar-csi component, version, VCS revision and Go version as labels.",
		ConstLabels: prometheus.Labels{
			"component":  component,
			"version":    version,
			"revision":   revision,
			"go_version": runtime.Version(),
		},
	})
	g.Set(1)
	return g
}

// Certificate roles (role label of M16).
const (
	CertRoleAgentServer      = "agent_server"
	CertRoleControllerClient = "controller_client"
	CertRoleCA               = "ca"
)

// certNotAfter is M16, pillar_csi_tls_certificate_not_after_timestamp_seconds.
var certNotAfter = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "pillar_csi_tls_certificate_not_after_timestamp_seconds",
	Help: "NotAfter (Unix seconds) of the mTLS certificate this process loaded into memory at startup, by role.",
}, []string{"role"})

// RegisterCertificateMetrics registers
// pillar_csi_tls_certificate_not_after_timestamp_seconds (M16). The series
// stay absent until [SetCertificateNotAfter] is called, i.e. without mTLS.
func RegisterCertificateMetrics(reg prometheus.Registerer) error {
	err := reg.Register(certNotAfter)
	if err != nil {
		return fmt.Errorf("register pillar_csi_tls_certificate_not_after_timestamp_seconds: %w", err)
	}
	return nil
}

// SetCertificateNotAfter records the NotAfter of the certificate in certPEM
// for role (a CertRole* constant). Call it with the same PEM bytes that were
// loaded into the TLS config. For agent_server and controller_client the
// first CERTIFICATE block (the leaf) is used; for ca the earliest NotAfter
// across the bundle, since the first CA to expire breaks verification.
func SetCertificateNotAfter(role string, certPEM []byte) error {
	var (
		found    bool
		notAfter int64
	)
	for rest := certPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("certificate not-after metric: parse %s certificate: %w", role, err)
		}
		na := cert.NotAfter.Unix()
		if !found || na < notAfter {
			notAfter = na
		}
		found = true
		if role != CertRoleCA {
			break
		}
	}
	if !found {
		return fmt.Errorf("certificate not-after metric: %s PEM: %w", role, errNoCertificate)
	}
	certNotAfter.WithLabelValues(role).Set(float64(notAfter))
	return nil
}

var errNoCertificate = errors.New("no CERTIFICATE block")
