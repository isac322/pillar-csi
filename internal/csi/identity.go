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

// Package csi implements the CSI (Container Storage Interface) gRPC services
// for pillar-csi: the ControllerServer (runs in the Kubernetes controller pod),
// the NodeServer (runs as a DaemonSet on every storage consumer node), and the
// IdentityServer (served by both controller and node binaries, announcing
// driver capabilities to the Container Orchestrator).
package csi

import (
	"context"
	"errors"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
)

// ─────────────────────────────────────────────────────────────────────────────
// IdentityServer
// ─────────────────────────────────────────────────────────────────────────────.

// IdentityServer implements the CSI Identity service (csi.IdentityServer).
// It is a thin, stateless layer that announces the driver name, version, and
// capabilities to the Container Orchestrator (CO).
//
// Field invariants:
//   - driverName is never empty after construction (validated in constructors).
//   - driverVersion is never empty after construction.
//   - readyFn is never nil after construction; a nil argument to constructors
//     is replaced by a default function that always returns (true, nil).
type IdentityServer struct {
	csi.UnimplementedIdentityServer

	// driverName is the CSI driver name, e.g. "pillar-csi.bhyoo.com".
	driverName string

	// driverVersion is the semantic version string of this driver build,
	// e.g. "0.1.0".
	driverVersion string

	// readyFn is called by Probe to determine driver readiness.  The function
	// receives the request context so a long-running readiness check can be
	// canceled.  A nil readyFn is replaced by an always-ready function during
	// construction.
	//
	// Returning (false, nil) indicates the driver is temporarily not ready
	// (the CO will retry).  Returning a non-nil error causes Probe to return
	// codes.Internal.
	readyFn func(ctx context.Context) (bool, error)
}

// NewIdentityServer constructs an IdentityServer that always reports ready.
//
// DriverName must be non-empty (e.g. "pillar-csi.bhyoo.com").
// DriverVersion must be non-empty (e.g. "0.1.0").
func NewIdentityServer(driverName, driverVersion string) *IdentityServer {
	return newIdentityServerWithReadyFn(driverName, driverVersion, nil)
}

// NewIdentityServerWithReadyFn constructs an IdentityServer with an injectable
// readiness function.  This constructor is used in tests and in production code
// that wants to gate Probe on an actual health check.
//
// If readyFn is nil, the server always returns Ready=true from Probe.
func NewIdentityServerWithReadyFn(
	driverName, driverVersion string,
	readyFn func(ctx context.Context) (bool, error),
) *IdentityServer {
	return newIdentityServerWithReadyFn(driverName, driverVersion, readyFn)
}

// newIdentityServerWithReadyFn is the internal shared constructor.
func newIdentityServerWithReadyFn(
	driverName, driverVersion string,
	readyFn func(ctx context.Context) (bool, error),
) *IdentityServer {
	if readyFn == nil {
		readyFn = func(_ context.Context) (bool, error) { return true, nil }
	}
	return &IdentityServer{
		driverName:    driverName,
		driverVersion: driverVersion,
		readyFn:       readyFn,
	}
}

// GetPluginInfo returns the driver name and vendor version to the CO.
//
// The CO calls this RPC at plugin registration time.  It must never fail in
// a well-configured deployment; the only possible errors are context
// cancellation / deadline exceeded (returned when the incoming gRPC context
// is already done before the handler starts).
func (s *IdentityServer) GetPluginInfo(
	ctx context.Context,
	_ *csi.GetPluginInfoRequest,
) (*csi.GetPluginInfoResponse, error) {
	// Respect an already-expired or canceled context so that the CO's
	// deadline is honored even for this trivial handler.
	err := ctx.Err()
	if err != nil {
		return nil, status.FromContextError(err).Err() //nolint:wrapcheck // CSI gRPC status errors must not be re-wrapped
	}
	return &csi.GetPluginInfoResponse{
		Name:          s.driverName,
		VendorVersion: s.driverVersion,
	}, nil
}

// GetPluginCapabilities returns the set of capabilities this driver exposes.
//
// Pillar-csi supports:
//   - CONTROLLER_SERVICE: CreateVolume, DeleteVolume, Publish/Unpublish, Expand.
//   - VOLUME_ACCESSIBILITY_CONSTRAINTS: volumes are pinned to a specific storage
//     node (topology label).
//
// NODE_SERVICE is implicit (every node binary serves the CSI Node service) and
// does not appear as a plugin-level capability per the CSI spec §3.5.1.
func (*IdentityServer) GetPluginCapabilities(
	ctx context.Context,
	_ *csi.GetPluginCapabilitiesRequest,
) (*csi.GetPluginCapabilitiesResponse, error) {
	err := ctx.Err()
	if err != nil {
		return nil, status.FromContextError(err).Err() //nolint:wrapcheck // CSI gRPC status errors must not be re-wrapped
	}
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_ONLINE,
					},
				},
			},
		},
	}, nil
}

// Probe reports the readiness of the driver to the CO.
//
// The CO calls Probe periodically (and on startup) to gate volume operations.
// A Ready=false response causes the CO to retry; a non-OK gRPC error causes
// the CO to treat the probe as failed.
//
// Error paths:
//   - The request context is already canceled/expired: returns the
//     corresponding gRPC DeadlineExceeded or Canceled status.
//   - readyFn returns a non-nil error: returns codes.Internal with the
//     error message included.
//   - readyFn returns (false, nil): returns Ready=false (CO will retry).
func (s *IdentityServer) Probe(
	ctx context.Context,
	_ *csi.ProbeRequest,
) (*csi.ProbeResponse, error) {
	// Respect an already-expired or canceled context.
	err := ctx.Err()
	if err != nil {
		return nil, status.FromContextError(err).Err() //nolint:wrapcheck // CSI gRPC status errors must not be re-wrapped
	}

	var ready bool
	ready, err = s.readyFn(ctx)
	if err != nil {
		// An error from the readiness check is an internal problem, not
		// a "not ready" state.  The CO must see a non-OK status so that
		// it distinguishes "temporarily not ready" (Ready=false) from
		// "health-check failed" (Internal error).
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, status.FromContextError(err).Err() //nolint:wrapcheck // CSI gRPC status errors must not be re-wrapped
		}
		return nil, status.Errorf(codes.Internal, "Probe: readiness check failed: %v", err)
	}

	return &csi.ProbeResponse{
		Ready: wrapperspb.Bool(ready),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// gRPC server registration helpers
// ─────────────────────────────────────────────────────────────────────────────.

// RegisterGRPC registers an IdentityServer and a ControllerServer with s.
//
// This convenience wrapper is used by the controller binary (cmd/main.go) so
// that it does not need to import the CSI spec library directly.  The node
// binary (cmd/node/main.go) calls RegisterNodeGRPC instead.
func RegisterGRPC(s *grpc.Server, identity *IdentityServer, ctrl *ControllerServer) {
	csi.RegisterIdentityServer(s, identity)
	csi.RegisterControllerServer(s, ctrl)
}

// RegisterNodeGRPC registers an IdentityServer and a NodeServer with s.
//
// Used by the node binary (cmd/node/main.go).
func RegisterNodeGRPC(s *grpc.Server, identity *IdentityServer, node *NodeServer) {
	csi.RegisterIdentityServer(s, identity)
	csi.RegisterNodeServer(s, node)
}
