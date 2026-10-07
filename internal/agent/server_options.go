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

package agent

import (
	"crypto"
	"crypto/x509"
	"time"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/lio"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// ServerOption is a functional option that configures a Server at construction
// time.  Options are applied in order by NewServer after the base Server is
// initialized.
type ServerOption func(*Server)

// WithDeviceChecker overrides the DeviceChecker used by ExportVolume to probe
// whether the zvol block device is present before writing configfs state.
//
// In production the server defaults to nvmeof.OsStatDeviceChecker (os.Stat).
// Pass nvmeof.AlwaysPresentChecker in tests to skip the device-presence check
// without requiring real block devices in the test environment.
func WithDeviceChecker(c nvmeof.DeviceChecker) ServerOption {
	return func(s *Server) { s.deviceChecker = c }
}

// WithDevicePollParams overrides the poll interval and timeout used by
// ExportVolume when waiting for the zvol block device to appear.
//
// In production the server defaults to nvmeof.DefaultDevicePollInterval (500 ms)
// and nvmeof.DefaultDevicePollTimeout (5 s).  Pass small values in tests to
// exercise timeout paths without blocking for seconds.
func WithDevicePollParams(interval, timeout time.Duration) ServerOption {
	return func(s *Server) {
		s.devicePollInterval = interval
		s.devicePollTimeout = timeout
	}
}

// WithDeviceClaimer overrides the claimer that takes an exclusive claim on
// the backend device before a disabled NVMe-oF namespace is enabled; the
// claim is held across the enable write so a local attach on the storage
// node cannot claim the device in the gap.
//
// In production the server defaults to nvmeof.ClaimDeviceExclusively (an
// O_EXCL open held for the enable).  Tests pass a fake to simulate a held
// device or observe the claim ordering.
func WithDeviceClaimer(c nvmeof.DeviceClaimer) ServerOption {
	return func(s *Server) { s.deviceClaimer = c }
}

// WithDrainStateDir overrides where Drain writes the .drained marker.
func WithDrainStateDir(dir string) ServerOption {
	return func(s *Server) { s.drainStateDir = dir }
}

// WithExportRestoreGate starts the server with the export restore pending:
// ExportVolume, AllowInitiator and ReconcileState without complete are
// rejected with UNAVAILABLE until a complete ReconcileState was processed.
// The agent binary always sets it; see server_export_restore.go.
func WithExportRestoreGate() ServerOption {
	return func(s *Server) { s.exportRestorePending.Store(true) }
}

// WithLIOFS overrides the configfs operations of the iSCSI (LIO) handler.
// Production uses lio.OSFS; tests pass an emulated kernel (package
// lio/liotest) rooted at the server's configfs root.
func WithLIOFS(fsys lio.FS) ServerOption {
	return func(s *Server) { s.lioFS = fsys }
}

// WithNFSManager enables NFS protocol dispatch for filesystem-capable pools.
func WithNFSManager(manager *nfs.Manager) ServerOption {
	return func(s *Server) { s.nfsManager = manager }
}

// WithBackendVariants supplies the explicit backend-type registry used when
// multiple backend kinds share one physical pool.
func WithBackendVariants(variants map[string]map[agentv1.BackendType]backend.VolumeBackend) ServerOption {
	return func(s *Server) { s.backendVariants = variants }
}

// WithFilesystemProxy configures the host-owned bind mount root. Host source
// prefixes belong to the backend that opens and pins the native source.
func WithFilesystemProxy(root string) ServerOption {
	return func(s *Server) {
		s.filesystemProxyRoot = root
	}
}

// WithRecoveryAuthority configures the volume-recovery authority (see
// server_recovery.go): the signer and leaf certificate are the agent's own
// server TLS identity (from --tls-cert/--tls-key), anchors are the operator
// public keys loaded from --recovery-trust-anchor that may sign a
// RecoveryAuthorization.
//
// The agent binary passes all three together and only when recovery is
// configured.  Calling it with a nil signer or certificate, or no anchors,
// leaves that capability fail-closed: snapshots are not issued and every
// TransferVolumeOwnership is refused.  The cert MUST be the certificate
// actually serving TLS; an identity or public key that differs from signer's
// makes issued snapshots unverifiable, which also fails closed.
func WithRecoveryAuthority(signer crypto.Signer, cert *x509.Certificate, anchors []crypto.PublicKey) ServerOption {
	return func(s *Server) {
		s.recoverySigner = signer
		s.recoveryCert = cert
		s.recoveryAnchors = anchors
	}
}

// RecoveryAgentIdentity returns the identity this agent embeds in a signed
// RecoverySnapshot: the certificate's Subject CommonName, or its first DNS
// SAN when the CommonName is empty.  "" means cert cannot attest an
// identity, so the agent must not issue snapshots and must refuse transfers.
func RecoveryAgentIdentity(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	return ""
}
