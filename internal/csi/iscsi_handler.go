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

package csi

// ISCSIHandler implements ProtocolHandler for iSCSI over TCP.
//
// It drives the in-process pure-Go initiator of package internal/iscsi, which
// performs the login phase itself and hands the connection to the kernel
// iscsi_tcp driver over NETLINK_ISCSI — no iscsiadm/iscsid on the node.
//
// The three layers map as follows:
//   - Attach  → Initiator.Login + Initiator.DeviceForLUN (Layer 1)
//   - Detach  → Initiator.Logout (idempotent)
//   - Rescan  → Initiator.Rescan (SCSI device rescan after online resize)

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/iscsi"
)

// iscsiRollbackTimeout bounds the logout Attach performs after the LUN's
// device failed to appear.
const iscsiRollbackTimeout = 60 * time.Second

// ISCSIInitiator is the subset of *iscsi.Initiator used by ISCSIHandler.
// The interface lets unit tests inject a fake without NETLINK_ISCSI, the
// iscsi_tcp module or root privileges.
type ISCSIInitiator interface {
	Login(ctx context.Context, p iscsi.SessionParams) (*iscsi.Session, error)
	DeviceForLUN(ctx context.Context, s *iscsi.Session, lun int) (string, error)
	Rescan(ctx context.Context, targetIQN string, portal iscsi.Portal) error
	Logout(ctx context.Context, targetIQN string, portal iscsi.Portal) error
	SetLoginParams(targetIQN string, portal iscsi.Portal, loginTimeout time.Duration, chap *iscsi.CHAPCredentials) error
}

var _ ISCSIInitiator = (*iscsi.Initiator)(nil)

// ISCSIProtocolState is the concrete ProtocolState produced by
// ISCSIHandler.Attach and consumed by ISCSIHandler.Detach/Rescan.
type ISCSIProtocolState struct {
	// TargetIQN is the iSCSI Qualified Name of the target.
	TargetIQN string
	// Address is the IP address of the target portal.
	Address string
	// Port is the TCP port of the target portal (decimal string).
	Port string
	// LUN is the logical unit number of the volume within the target.
	LUN int
	// LoginTimeout is the login timeout the session was logged in with;
	// zero means unknown (a stage record older than the field) and selects
	// the initiator default.
	LoginTimeout time.Duration
	// CHAP holds the credentials the session authenticates with; nil for
	// AuthMethod=None.
	CHAP *iscsi.CHAPCredentials
}

// ProtocolType satisfies the ProtocolState interface.
func (*ISCSIProtocolState) ProtocolType() string { return ProtocolISCSI }

// ISCSIHandler implements ProtocolHandler for iSCSI.
type ISCSIHandler struct {
	initiator    ISCSIInitiator
	initiatorIQN string
}

var _ ProtocolHandler = (*ISCSIHandler)(nil)

// NewISCSIHandler constructs an ISCSIHandler that logs in as initiatorIQN
// (the IQN published on the CSINode annotation; see ReadInitiatorIQN).
func NewISCSIHandler(initiator ISCSIInitiator, initiatorIQN string) *ISCSIHandler {
	return &ISCSIHandler{initiator: initiator, initiatorIQN: initiatorIQN}
}

// iscsiAttachSpec is the validated form of the AttachParams of an iSCSI
// volume.
type iscsiAttachSpec struct {
	targetIQN string
	portal    iscsi.Portal
	lun       int
	timeouts  iscsiSessionTimeouts
	chap      *iscsi.CHAPCredentials
}

// iscsiSessionTimeouts carries the optional session timeouts from the
// VolumeContext.  A zero login or a nil pointer keeps the initiator default
// (login 15 s, replacement 120 s, NOP-Out interval 5 s, NOP-Out timeout
// 5 s); a non-nil pointer to 0 is an explicit 0 passed to the kernel.
type iscsiSessionTimeouts struct {
	login                                        time.Duration
	replacement, noopOutInterval, noopOutTimeout *time.Duration
}

// parseISCSIAttachParams validates the AttachParams of an iSCSI volume:
// ConnectionID (target IQN), Address and Port are required, Port must lie in
// [1, 65535], VolumeRef is the LUN (empty means LUN 0), Extra may carry
// the iscsi-* timeout keys (see parseISCSISessionTimeouts) and the auth
// method, whose credentials Secrets must hold (see parseISCSICHAP).
// NodeStageVolume calls it before any attach side effect so a malformed
// VolumeContext or missing secret key is reported as InvalidArgument.
func parseISCSIAttachParams(params AttachParams) (iscsiAttachSpec, error) {
	if params.ConnectionID == "" {
		return iscsiAttachSpec{}, fmt.Errorf("ConnectionID (target IQN) is required")
	}
	if params.Address == "" {
		return iscsiAttachSpec{}, fmt.Errorf("address of target %s is required", params.ConnectionID)
	}
	port, err := strconv.Atoi(params.Port)
	if err != nil || port < 1 || port > 65535 {
		return iscsiAttachSpec{}, fmt.Errorf("port %q of target %s must be an integer in [1, 65535]",
			params.Port, params.ConnectionID)
	}
	lun := 0
	if params.VolumeRef != "" {
		lun, err = strconv.Atoi(params.VolumeRef)
		if err != nil || lun < 0 {
			return iscsiAttachSpec{}, fmt.Errorf("LUN %q of target %s must be a non-negative integer",
				params.VolumeRef, params.ConnectionID)
		}
	}
	timeouts, err := parseISCSISessionTimeouts(params.Extra)
	if err != nil {
		return iscsiAttachSpec{}, fmt.Errorf("target %s: %w", params.ConnectionID, err)
	}
	chap, err := parseISCSICHAP(params)
	if err != nil {
		return iscsiAttachSpec{}, fmt.Errorf("target %s: %w", params.ConnectionID, err)
	}
	return iscsiAttachSpec{
		targetIQN: params.ConnectionID,
		portal:    iscsi.Portal{Address: params.Address, Port: port},
		lun:       lun,
		timeouts:  timeouts,
		chap:      chap,
	}, nil
}

// iscsiNodeStageSecretsName names NodeStageVolumeRequest.secrets in errors.
const iscsiNodeStageSecretsName = "NodeStageVolume secrets"

// parseISCSICHAP returns the CHAP credentials for the auth method in the
// VolumeContext (absent or None: nil, secrets ignored), read from the
// node-stage secrets with the Secret format of ParseISCSIChapSecret.
// Errors name the missing or invalid key, never a value.
func parseISCSICHAP(params AttachParams) (*iscsi.CHAPCredentials, error) {
	method := v1alpha1.ISCSIAuthMethod(params.Extra[VolumeContextKeyISCSIAuthMethod])
	chap, err := ParseISCSIChapSecret(method, iscsiNodeStageSecretsName, params.Secrets)
	if err != nil {
		return nil, fmt.Errorf("iSCSI auth method %s: %w", method, err)
	}
	return chapCredentials(chap), nil
}

// chapCredentials converts the validated Secret contents to initiator
// credentials; nil (method None) stays nil.
func chapCredentials(chap *agentv1.IscsiChap) *iscsi.CHAPCredentials {
	if chap == nil {
		return nil
	}
	return &iscsi.CHAPCredentials{
		Username:       chap.GetUsername(),
		Secret:         chap.GetPassword(),
		MutualUsername: chap.GetMutualUsername(),
		MutualSecret:   chap.GetMutualPassword(),
	}
}

// isISCSIAuthenticationError reports whether an Attach error is a CHAP
// authentication failure in either direction: the target refused the
// initiator's credentials (login status class 2 detail 1), or the target
// failed mutual CHAP.  NodeStageVolume reports these as Unauthenticated.
func isISCSIAuthenticationError(err error) bool {
	return errors.Is(err, iscsi.ErrAuthenticationFailed) || errors.Is(err, iscsi.ErrTargetAuthenticationFailed)
}

// parseISCSISessionTimeouts reads the iscsi-* timeout keys CreateVolume
// copied into the VolumeContext.  Absent or empty keys keep the initiator
// default; a present value must be a base-10 number of seconds within the
// CRD bounds (loginTimeout ≥ 1, the others ≥ 0) so a misconfiguration is
// never silently replaced by the default.
func parseISCSISessionTimeouts(volCtx map[string]string) (iscsiSessionTimeouts, error) {
	var t iscsiSessionTimeouts
	for _, f := range []struct {
		key string
		min int64
		dst **time.Duration
	}{
		{VolumeContextKeyISCSILoginTimeout, 1, nil},
		{VolumeContextKeyISCSIReplacementTimeout, 0, &t.replacement},
		{VolumeContextKeyISCSINoopOutInterval, 0, &t.noopOutInterval},
		{VolumeContextKeyISCSINoopOutTimeout, 0, &t.noopOutTimeout},
	} {
		raw := volCtx[f.key]
		if raw == "" {
			continue
		}
		v, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return iscsiSessionTimeouts{}, fmt.Errorf("parse %s=%q: %w", f.key, raw, err)
		}
		if v < f.min {
			return iscsiSessionTimeouts{}, fmt.Errorf("parse %s=%d: must be at least %d seconds", f.key, v, f.min)
		}
		d := time.Duration(v) * time.Second
		if f.dst == nil {
			t.login = d
		} else {
			*f.dst = &d
		}
	}
	return t, nil
}

// Attach logs in to the iSCSI target (idempotent per target and portal) and
// waits for the block device of the LUN.  When the device does not appear,
// the session is logged out again: a failed NodeStageVolume is not followed
// by NodeUnstageVolume, so a session left behind would never be released
// and would keep re-logging in after the target is gone.
//
// AttachParams fields used by this handler:
//   - ConnectionID — the target IQN
//   - Address      — the target portal IP address
//   - Port         — the target portal TCP port (e.g. "3260")
//   - VolumeRef    — the LUN (decimal; empty means 0)
//   - Extra        — optional iscsi-* session timeouts (seconds) and auth method
//   - Secrets      — CHAP credentials when the auth method is CHAP or MutualCHAP
func (h *ISCSIHandler) Attach(ctx context.Context, params AttachParams) (*AttachResult, error) {
	spec, err := parseISCSIAttachParams(params)
	if err != nil {
		return nil, fmt.Errorf("iscsi Attach: %w", err)
	}

	sess, err := h.initiator.Login(ctx, iscsi.SessionParams{
		InitiatorIQN:       h.initiatorIQN,
		TargetIQN:          spec.targetIQN,
		Portal:             spec.portal,
		LoginTimeout:       spec.timeouts.login,
		ReplacementTimeout: spec.timeouts.replacement,
		NoopOutInterval:    spec.timeouts.noopOutInterval,
		NoopOutTimeout:     spec.timeouts.noopOutTimeout,
		CHAP:               spec.chap,
	})
	if err != nil {
		return nil, fmt.Errorf("iscsi Attach: login to %s at %s: %w", spec.targetIQN, spec.portal, err)
	}

	devicePath, err := h.initiator.DeviceForLUN(ctx, sess, spec.lun)
	if err != nil {
		err = fmt.Errorf("iscsi Attach: block device for LUN %d of %s at %s: %w",
			spec.lun, spec.targetIQN, spec.portal, err)
		// The caller's context may already be expired; the rollback gets
		// its own bounded budget.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), iscsiRollbackTimeout)
		defer cancel()
		logoutErr := h.initiator.Logout(rctx, spec.targetIQN, spec.portal)
		if logoutErr != nil {
			//nolint:wrapcheck // both operands are wrapped/annotated
			return nil, errors.Join(err, fmt.Errorf("iscsi Attach: roll back login to %s at %s: %w",
				spec.targetIQN, spec.portal, logoutErr))
		}
		return nil, err
	}

	loginTimeout := spec.timeouts.login
	if loginTimeout <= 0 {
		loginTimeout = iscsi.DefaultLoginTimeout
	}
	return &AttachResult{
		DevicePath: devicePath,
		State: &ISCSIProtocolState{
			TargetIQN:    spec.targetIQN,
			Address:      spec.portal.Address,
			Port:         strconv.Itoa(spec.portal.Port),
			LUN:          spec.lun,
			LoginTimeout: loginTimeout,
			CHAP:         spec.chap,
		},
	}, nil
}

// Detach logs out of the iSCSI session identified by state.  Logging out of
// a session that no longer exists is a no-op.
func (h *ISCSIHandler) Detach(ctx context.Context, state ProtocolState) error {
	targetIQN, portal, err := iscsiStatePortal("Detach", state)
	if err != nil {
		return err
	}
	logoutErr := h.initiator.Logout(ctx, targetIQN, portal)
	if logoutErr != nil {
		return fmt.Errorf("iscsi Detach: logout %s at %s: %w", targetIQN, portal, logoutErr)
	}
	return nil
}

// Rescan re-reads the capacity of the LUN devices of the session identified
// by state, so an online resize by ControllerExpandVolume becomes visible to
// the block layer before the filesystem is grown.
func (h *ISCSIHandler) Rescan(ctx context.Context, state ProtocolState) error {
	targetIQN, portal, err := iscsiStatePortal("Rescan", state)
	if err != nil {
		return err
	}
	rescanErr := h.initiator.Rescan(ctx, targetIQN, portal)
	if rescanErr != nil {
		return fmt.Errorf("iscsi Rescan: rescan %s at %s: %w", targetIQN, portal, rescanErr)
	}
	return nil
}

// RestoreSession re-applies to the session identified by state the
// userspace-only session parameters the kernel does not keep: the login
// timeout and the CHAP credentials.  The pillar-node process calls it at
// startup for every staged iSCSI volume, because a session adopted from
// sysfs after a restart otherwise re-logs in with the initiator default
// and without credentials, and kubelet does not repeat NodeStageVolume for
// a volume that stays mounted.  A zero state.LoginTimeout (a record older
// than the field) applies the initiator default.
func (h *ISCSIHandler) RestoreSession(state ProtocolState) error {
	targetIQN, portal, err := iscsiStatePortal("RestoreSession", state)
	if err != nil {
		return err
	}
	st, ok := state.(*ISCSIProtocolState)
	if !ok {
		return fmt.Errorf("iscsi RestoreSession: unexpected state type %T (want *ISCSIProtocolState)", state)
	}
	setErr := h.initiator.SetLoginParams(targetIQN, portal, st.LoginTimeout, st.CHAP)
	if setErr != nil {
		return fmt.Errorf("iscsi RestoreSession: set login parameters (timeout %v, CHAP %t) of %s at %s: %w",
			st.LoginTimeout, st.CHAP != nil, targetIQN, portal, setErr)
	}
	return nil
}

// iscsiStatePortal extracts the target IQN and portal from an
// *ISCSIProtocolState for operation op.
func iscsiStatePortal(op string, state ProtocolState) (string, iscsi.Portal, error) {
	st, ok := state.(*ISCSIProtocolState)
	if !ok || st == nil {
		return "", iscsi.Portal{}, fmt.Errorf("iscsi %s: unexpected state type %T (want *ISCSIProtocolState)", op, state)
	}
	if st.TargetIQN == "" {
		return "", iscsi.Portal{}, fmt.Errorf("iscsi %s: state has empty TargetIQN", op)
	}
	port, err := strconv.Atoi(st.Port)
	if err != nil || port < 1 || port > 65535 {
		return "", iscsi.Portal{}, fmt.Errorf("iscsi %s: state of target %s has invalid port %q",
			op, st.TargetIQN, st.Port)
	}
	if st.Address == "" {
		return "", iscsi.Portal{}, fmt.Errorf("iscsi %s: state of target %s has empty address", op, st.TargetIQN)
	}
	return st.TargetIQN, iscsi.Portal{Address: st.Address, Port: port}, nil
}

// missingHandlerHint explains why no ProtocolHandler is registered for
// protocolType.  The pillar-node process registers the iSCSI handler only when the kernel
// iscsi_tcp transport is present at startup (see iscsi.Available).
func missingHandlerHint(protocolType string) string {
	if protocolType != ProtocolISCSI {
		return ""
	}
	return ": the iSCSI initiator is disabled on this node because kernel module iscsi_tcp " +
		"was not loaded when pillar-node started (load iscsi_tcp and restart the pillar-node pod)"
}
