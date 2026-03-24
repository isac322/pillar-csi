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

// Package testcerts generates ephemeral self-signed TLS certificate bundles
// for use in unit and integration tests.
//
// All keys and certificates are generated in memory using ECDSA P-256; nothing
// is written to disk unless the caller explicitly does so.  A Bundle contains a
// self-signed CA certificate, a server certificate (with IP/DNS SANs), and a
// client certificate, all signed by that CA.
//
// Typical usage:
//
//	bundle, err := testcerts.New("127.0.0.1")
//	serverCreds, _ := tlscreds.NewServerCredentials(bundle.ServerCert, bundle.ServerKey, bundle.CACert)
//	clientCreds, _ := tlscreds.NewClientCredentials(bundle.ClientCert, bundle.ClientKey, bundle.CACert, "")
package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Bundle holds PEM-encoded certificate and private key material for a
// self-signed CA and the server/client leaf certificates it signed.
type Bundle struct {
	// CACert is the PEM-encoded CA certificate (self-signed).
	CACert []byte
	// CAKey is the PEM-encoded CA ECDSA private key.
	CAKey []byte

	// ServerCert is the PEM-encoded server certificate signed by the CA.
	// SANs are populated from the serverAddrs passed to New.
	ServerCert []byte
	// ServerKey is the PEM-encoded server ECDSA private key.
	ServerKey []byte

	// ClientCert is the PEM-encoded client certificate signed by the CA.
	ClientCert []byte
	// ClientKey is the PEM-encoded client ECDSA private key.
	ClientKey []byte
}

// New generates a fresh ephemeral Bundle.
//
// serverAddrs is used to populate the SAN extension of the server certificate.
// Each element is interpreted as an IP address if net.ParseIP succeeds; otherwise
// it is treated as a DNS name.  If no addresses are provided the server certificate
// is issued with a single IP SAN for 127.0.0.1.
//
// The generated certificates are valid for one hour from the time of generation,
// which is more than sufficient for any in-process test.
func New(serverAddrs ...string) (*Bundle, error) {
	// -----------------------------------------------------------------------
	// CA key + self-signed certificate
	// -----------------------------------------------------------------------
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("testcerts: generate CA key: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"pillar-csi test"},
			CommonName:   "test-ca",
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("testcerts: create CA certificate: %w", err)
	}
	caCertParsed, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("testcerts: parse CA certificate: %w", err)
	}

	// -----------------------------------------------------------------------
	// Server leaf certificate
	// -----------------------------------------------------------------------
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("testcerts: generate server key: %w", err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"pillar-csi"},
			CommonName:   "pillar-agent",
		},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// Populate SANs.
	addrs := serverAddrs
	if len(addrs) == 0 {
		addrs = []string{"127.0.0.1"}
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil {
			serverTemplate.IPAddresses = append(serverTemplate.IPAddresses, ip)
		} else {
			serverTemplate.DNSNames = append(serverTemplate.DNSNames, addr)
		}
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertParsed, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("testcerts: create server certificate: %w", err)
	}

	// -----------------------------------------------------------------------
	// Client leaf certificate
	// -----------------------------------------------------------------------
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("testcerts: generate client key: %w", err)
	}

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject: pkix.Name{
			Organization: []string{"pillar-csi"},
			CommonName:   "pillar-controller",
		},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientCertDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCertParsed, &clientKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("testcerts: create client certificate: %w", err)
	}

	// -----------------------------------------------------------------------
	// PEM encode everything
	// -----------------------------------------------------------------------
	b := &Bundle{}

	b.CACert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, fmt.Errorf("testcerts: marshal CA key: %w", err)
	}
	b.CAKey = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER})

	b.ServerCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, fmt.Errorf("testcerts: marshal server key: %w", err)
	}
	b.ServerKey = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})

	b.ClientCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientCertDER})
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return nil, fmt.Errorf("testcerts: marshal client key: %w", err)
	}
	b.ClientKey = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER})

	return b, nil
}
