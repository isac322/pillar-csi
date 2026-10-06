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

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRecoveryCert generates a self-signed ECDSA certificate with the given
// CommonName and writes cert PEM and SEC1 key PEM into dir.
func writeRecoveryCert(t *testing.T, dir, commonName string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "agent.crt")
	err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "agent.key")
	err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// writeRecoveryAnchor writes a fresh operator public key as a PKIX
// "PUBLIC KEY" PEM block into dir.
func writeRecoveryAnchor(t *testing.T, dir string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "anchors.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The recovery authority loads only when everything it needs is present and
// coherent: a valid TLS identity, a certificate that can attest an
// identity, and at least one operator anchor.  Every failure is an error,
// never a half-configured option.
func TestLoadRecoveryAuthority(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath, keyPath := writeRecoveryCert(t, dir, "pillar-agent.test")
	anchorPath := writeRecoveryAnchor(t, dir)

	opt, err := loadRecoveryAuthority(certPath, keyPath, anchorPath)
	if err != nil {
		t.Fatalf("loadRecoveryAuthority: %v", err)
	}
	if opt == nil {
		t.Fatal("loadRecoveryAuthority returned a nil option")
	}

	for _, tc := range []struct {
		name              string
		cert, key, anchor string
		want              string
	}{
		{"missing cert", filepath.Join(dir, "no.crt"), keyPath, anchorPath, "read server cert"},
		{"missing key", certPath, filepath.Join(dir, "no.key"), anchorPath, "read server key"},
		{"missing anchor", certPath, keyPath, filepath.Join(dir, "no.pem"), "trust anchor"},
		// A private-key-only file holds no PUBLIC KEY or CERTIFICATE block.
		{"anchor without a public key", certPath, keyPath, keyPath, "trust anchor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotOpt, gotErr := loadRecoveryAuthority(tc.cert, tc.key, tc.anchor)
			if gotErr == nil || gotOpt != nil {
				t.Fatalf("%s: got (%v, %v), want an error", tc.name, gotOpt, gotErr)
			}
			if !strings.Contains(gotErr.Error(), tc.want) {
				t.Fatalf("%s: error %q does not name %q", tc.name, gotErr, tc.want)
			}
		})
	}

	// A certificate that attests no identity (no CN, no DNS SAN) is
	// rejected instead of signing unverifiable snapshots.
	noIDDir := t.TempDir()
	noIDCert, noIDKey := writeRecoveryCert(t, noIDDir, "")
	opt, err = loadRecoveryAuthority(noIDCert, noIDKey, anchorPath)
	if err == nil || opt != nil {
		t.Fatalf("identity-less certificate: got (%v, %v), want an error", opt, err)
	}
	if !strings.Contains(err.Error(), "attests no identity") {
		t.Fatalf("identity-less certificate: error %q", err)
	}
}
