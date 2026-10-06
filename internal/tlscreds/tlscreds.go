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

// Package tlscreds builds gRPC TransportCredentials for mutual TLS (mTLS)
// between the pillar-controller (gRPC client) and pillar-agent (gRPC server).
//
// Both ends present certificates signed by a shared cluster-internal CA.  The
// server requires a client certificate (RequireAndVerifyClientCert) so that the
// controller must authenticate itself; the client verifies the server certificate
// against the same CA.
//
// # In-memory constructors (for tests)
//
//   - [NewServerCredentials] — build server-side credentials from PEM bytes.
//   - [NewClientCredentials] — build client-side credentials from PEM bytes.
//
// # File-based constructors (for production binary)
//
//   - [LoadServerCredentials] — read cert/key/CA files from disk.
//   - [LoadClientCredentials] — read cert/key/CA files from disk.
package tlscreds

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// NewServerCredentials builds mTLS [credentials.TransportCredentials] for the
// agent gRPC server.
//
// The server presents serverCertPEM/serverKeyPEM to connecting clients and
// requires clients to present a certificate signed by caCertPEM.
// TLS 1.3 is the minimum negotiated version.
func NewServerCredentials(serverCertPEM, serverKeyPEM, caCertPEM []byte) (credentials.TransportCredentials, error) {
	cert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("tlscreds: parse server cert/key: %w", err)
	}

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("tlscreds: failed to append CA certificate to client CA pool")
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Require and cryptographically verify the client's certificate.
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
		MinVersion: tls.VersionTLS13,
	}
	return credentials.NewTLS(cfg), nil
}

// NewClientCredentials builds mTLS [credentials.TransportCredentials] for the
// controller gRPC client.
//
// The client presents clientCertPEM/clientKeyPEM to the server and verifies
// the server certificate against caCertPEM.
//
// ServerName overrides the TLS server-name used for SAN verification.  Pass an
// empty string to let gRPC derive the server name from the dial target address
// (typically the agent's IP or hostname).
func NewClientCredentials(
	clientCertPEM, clientKeyPEM, caCertPEM []byte,
	serverName string,
) (credentials.TransportCredentials, error) {
	cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("tlscreds: parse client cert/key: %w", err)
	}

	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("tlscreds: failed to append CA certificate to root CA pool")
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      rootCAs,
		// ServerName is intentionally left empty here when not overridden so
		// that gRPC fills it in from the dial target authority.
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}
	return credentials.NewTLS(cfg), nil
}

// LoadServerCredentials reads the TLS certificate, private key, and CA
// certificate from disk and delegates to [NewServerCredentials].
//
// CertFile is the PEM file containing the server certificate (and optionally
// intermediate chain). KeyFile is the corresponding PEM private key.
// CaFile is the PEM CA certificate that signed the expected client certificates.
func LoadServerCredentials(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	certPEM, err := os.ReadFile(certFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, fmt.Errorf("tlscreds: read server cert %q: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, fmt.Errorf("tlscreds: read server key %q: %w", keyFile, err)
	}
	caPEM, err := os.ReadFile(caFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, fmt.Errorf("tlscreds: read CA cert %q: %w", caFile, err)
	}
	creds, err := NewServerCredentials(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, err
	}
	err = recordCertificates(telemetry.CertRoleAgentServer, certPEM, caPEM)
	if err != nil {
		return nil, err
	}
	return creds, nil
}

// LoadServerIdentity reads the server certificate and private key from disk
// and returns the parsed leaf certificate and its private key as a
// [crypto.Signer].  The volume-recovery path uses exactly this identity — the
// certificate the agent serves — to sign and verify RecoverySnapshot
// attestations, so the signer and the certificate always belong together.
// A non-signing or unparsable key is an error: the caller must fail closed.
func LoadServerIdentity(certFile, keyFile string) (crypto.Signer, *x509.Certificate, error) {
	certPEM, err := os.ReadFile(certFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, nil, fmt.Errorf("tlscreds: read server cert %q: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, nil, fmt.Errorf("tlscreds: read server key %q: %w", keyFile, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscreds: parse server cert/key: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, nil, fmt.Errorf("tlscreds: server cert %q holds no certificate", certFile)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("tlscreds: parse server leaf certificate %q: %w", certFile, err)
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("tlscreds: server key %q (%T) cannot sign recovery snapshots", keyFile, pair.PrivateKey)
	}
	return signer, leaf, nil
}

// recordCertificates sets the M16 NotAfter gauges for the leaf certificate
// (under role) and the CA bundle from the PEM bytes just loaded into the TLS
// config.
func recordCertificates(role string, certPEM, caPEM []byte) error {
	err := telemetry.SetCertificateNotAfter(role, certPEM)
	if err != nil {
		return fmt.Errorf("tlscreds: record %s certificate expiry: %w", role, err)
	}
	err = telemetry.SetCertificateNotAfter(telemetry.CertRoleCA, caPEM)
	if err != nil {
		return fmt.Errorf("tlscreds: record CA certificate expiry: %w", err)
	}
	return nil
}

// LoadClientCredentials reads the TLS certificate, private key, and CA
// certificate from disk and delegates to [NewClientCredentials].
//
// CertFile is the PEM file containing the client certificate. KeyFile is the
// corresponding PEM private key. CaFile is the PEM CA certificate that signed
// the expected server certificate. ServerName overrides TLS SAN verification
// target; pass an empty string to derive it from the dial address.
func LoadClientCredentials(certFile, keyFile, caFile, serverName string) (credentials.TransportCredentials, error) {
	certPEM, err := os.ReadFile(certFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, fmt.Errorf("tlscreds: read client cert %q: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, fmt.Errorf("tlscreds: read client key %q: %w", keyFile, err)
	}
	caPEM, err := os.ReadFile(caFile) //nolint:gosec // G304: paths are operator-supplied TLS credential files
	if err != nil {
		return nil, fmt.Errorf("tlscreds: read CA cert %q: %w", caFile, err)
	}
	creds, err := NewClientCredentials(certPEM, keyPEM, caPEM, serverName)
	if err != nil {
		return nil, err
	}
	err = recordCertificates(telemetry.CertRoleControllerClient, certPEM, caPEM)
	if err != nil {
		return nil, err
	}
	return creds, nil
}
