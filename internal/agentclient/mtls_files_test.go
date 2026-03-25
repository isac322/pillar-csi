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

package agentclient_test

// File-based mTLS tests for NewManagerFromFiles and NewManagerWithTLSCredentials.
//
// These tests verify that:
//  1. NewManagerFromFiles loads credentials from PEM files on disk and
//     successfully connects to an mTLS server.
//  2. NewManagerFromFiles returns an error when the cert files do not exist.
//  3. NewManagerFromFiles returns an error when only some flags are valid (wrong
//     CA scenario — connection fails at RPC time, not construction time).
//  4. NewManagerWithTLSCredentials accepts pre-built credentials and connects.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/agentclient"
	"github.com/bhyoo/pillar-csi/internal/testutil/testcerts"
	"github.com/bhyoo/pillar-csi/internal/tlscreds"
)

// writeTempFile writes data to a temporary file and returns its path.
// The file is removed at the end of the test via t.Cleanup.
func writeTempFile(t *testing.T, dir, pattern string, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		t.Fatalf("os.CreateTemp: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

// writeBundleToTempDir writes all PEM files from bundle into a temporary
// directory and returns the individual file paths.
func writeBundleToTempDir(t *testing.T, bundle *testcerts.Bundle) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = writeTempFile(t, dir, "client-cert-*.pem", bundle.ClientCert)
	keyFile = writeTempFile(t, dir, "client-key-*.pem", bundle.ClientKey)
	caFile = writeTempFile(t, dir, "ca-cert-*.pem", bundle.CACert)
	return certFile, keyFile, caFile
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

// TestNewManagerFromFiles_Success verifies that NewManagerFromFiles loads the
// certificate files from disk and produces a Manager that can establish a
// mutually-authenticated gRPC connection to an mTLS agent server.
func TestNewManagerFromFiles_Success(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	// Write bundle to temp files.
	certFile, keyFile, caFile := writeBundleToTempDir(t, bundle)

	// Start an mTLS server.
	addr := startMTLSServer(t, bundle)

	// Build Manager from files.
	m, err := agentclient.NewManagerFromFiles(certFile, keyFile, caFile, "")
	if err != nil {
		t.Fatalf("NewManagerFromFiles: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	// The Manager should be able to invoke a HealthCheck.
	resp, err := m.HealthCheck(shortCtx(t), addr)
	if err != nil {
		t.Fatalf("HealthCheck over mTLS (from files): unexpected error: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("expected Healthy=true, got false")
	}
}

// TestNewManagerFromFiles_MissingCertFile verifies that NewManagerFromFiles
// returns a non-nil error when the certificate file path does not exist.
func TestNewManagerFromFiles_MissingCertFile(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	dir := t.TempDir()
	_, keyFile, caFile := writeBundleToTempDir(t, bundle)

	// Use a nonexistent cert path.
	missingCert := filepath.Join(dir, "does-not-exist.pem")

	_, err = agentclient.NewManagerFromFiles(missingCert, keyFile, caFile, "")
	if err == nil {
		t.Fatal("expected error for missing cert file, got nil")
	}
	t.Logf("received expected error: %v", err)
}

// TestNewManagerFromFiles_MissingKeyFile verifies that NewManagerFromFiles
// returns a non-nil error when the key file path does not exist.
func TestNewManagerFromFiles_MissingKeyFile(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	dir := t.TempDir()
	certFile, _, caFile := writeBundleToTempDir(t, bundle)
	missingKey := filepath.Join(dir, "does-not-exist.pem")

	_, err = agentclient.NewManagerFromFiles(certFile, missingKey, caFile, "")
	if err == nil {
		t.Fatal("expected error for missing key file, got nil")
	}
	t.Logf("received expected error: %v", err)
}

// TestNewManagerFromFiles_MissingCAFile verifies that NewManagerFromFiles
// returns a non-nil error when the CA file path does not exist.
func TestNewManagerFromFiles_MissingCAFile(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	dir := t.TempDir()
	certFile, keyFile, _ := writeBundleToTempDir(t, bundle)
	missingCA := filepath.Join(dir, "does-not-exist.pem")

	_, err = agentclient.NewManagerFromFiles(certFile, keyFile, missingCA, "")
	if err == nil {
		t.Fatal("expected error for missing CA file, got nil")
	}
	t.Logf("received expected error: %v", err)
}

// TestNewManagerFromFiles_WrongCA verifies that a Manager loaded from files
// that contain a client certificate signed by a different CA is rejected by
// the mTLS server at RPC time (not at construction time — the wrong-CA error
// is detected during the TLS handshake).
func TestNewManagerFromFiles_WrongCA(t *testing.T) {
	// Server uses bundle1.
	bundle1, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New bundle1: %v", err)
	}
	// Client uses bundle2 — signed by a completely different CA.
	bundle2, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New bundle2: %v", err)
	}

	addr := startMTLSServer(t, bundle1)

	// Write bundle2's client cert/key to temp files, but use bundle1's CA
	// (so the TLS dial succeeds at the TCP level but the server rejects the
	// client cert because it is not signed by bundle1's CA).
	dir := t.TempDir()
	certFile := writeTempFile(t, dir, "client-cert-*.pem", bundle2.ClientCert)
	keyFile := writeTempFile(t, dir, "client-key-*.pem", bundle2.ClientKey)
	caFile := writeTempFile(t, dir, "ca-cert-*.pem", bundle1.CACert) // server's CA for server-cert verification

	m, err := agentclient.NewManagerFromFiles(certFile, keyFile, caFile, "")
	if err != nil {
		// Construction should succeed — wrong CA is only detected at handshake.
		t.Fatalf("NewManagerFromFiles: unexpected construction error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	_, err = m.HealthCheck(shortCtx(t), addr)
	if err == nil {
		t.Fatal("expected HealthCheck to fail when client cert is signed by wrong CA, but got nil error")
	}
	t.Logf("received expected rejection error: %v", err)
}

// TestNewManagerWithTLSCredentials_Success verifies that
// NewManagerWithTLSCredentials accepts pre-built credentials and connects.
func TestNewManagerWithTLSCredentials_Success(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	addr := startMTLSServer(t, bundle)

	creds, err := tlscreds.NewClientCredentials(
		bundle.ClientCert, bundle.ClientKey, bundle.CACert, "",
	)
	if err != nil {
		t.Fatalf("NewClientCredentials: %v", err)
	}

	m := agentclient.NewManagerWithTLSCredentials(creds)
	t.Cleanup(func() { _ = m.Close() })

	resp, err := m.HealthCheck(shortCtx(t), addr)
	if err != nil {
		t.Fatalf("HealthCheck via NewManagerWithTLSCredentials: unexpected error: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("expected Healthy=true, got false")
	}
}

// TestNewManagerWithTLSCredentials_PlaintextServerRejected verifies that a
// Manager built with TLS credentials fails when connecting to a plaintext
// server (prevents silent TLS downgrade).
func TestNewManagerWithTLSCredentials_PlaintextServerRejected(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}

	addr := startPlaintextServer(t)

	creds, err := tlscreds.NewClientCredentials(
		bundle.ClientCert, bundle.ClientKey, bundle.CACert, "",
	)
	if err != nil {
		t.Fatalf("NewClientCredentials: %v", err)
	}

	m := agentclient.NewManagerWithTLSCredentials(creds)
	t.Cleanup(func() { _ = m.Close() })

	_, err = m.HealthCheck(shortCtx(t), addr)
	if err == nil {
		t.Fatal("expected HealthCheck to fail for mTLS client connecting to plaintext server, got nil error")
	}
	t.Logf("received expected rejection error: %v", err)
}

// TestNewManagerFromFiles_ImplementsDialerInterface verifies that *Manager
// returned by NewManagerFromFiles satisfies the Dialer interface.
func TestNewManagerFromFiles_ImplementsDialerInterface(t *testing.T) {
	bundle, err := testcerts.New("127.0.0.1")
	if err != nil {
		t.Fatalf("testcerts.New: %v", err)
	}
	certFile, keyFile, caFile := writeBundleToTempDir(t, bundle)

	m, err := agentclient.NewManagerFromFiles(certFile, keyFile, caFile, "")
	if err != nil {
		t.Fatalf("NewManagerFromFiles: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	// Compile-time check via interface assignment.
	var _ agentclient.Dialer = m

	// Runtime check via type assertion on the Dialer interface.
	var d agentclient.Dialer = m
	if d == nil {
		t.Error("NewManagerFromFiles returned a nil Dialer")
	}
}
