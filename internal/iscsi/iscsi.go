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

// Package iscsi is a pure-Go, in-process iSCSI initiator for the Linux
// software iSCSI transport (iscsi_tcp).
//
// It performs what open-iscsi's iscsid does, without any external binary:
// sessions and connections are created through the NETLINK_ISCSI kernel
// interface (include/scsi/iscsi_if.h), the TCP socket is dialed in userspace
// and handed to the kernel with ISCSI_UEVENT_BIND_CONN, the RFC 7143 login is
// carried over ISCSI_UEVENT_SEND_PDU / ISCSI_KEVENT_RECV_PDU, the negotiated
// parameters are pushed with ISCSI_UEVENT_SET_PARAM, and the kernel then runs
// the full-feature phase itself.  A supervisor re-logs in when the kernel
// reports a connection error, so no iscsid is needed on the node.
//
// The netlink transport and device-node creation are Linux-only; on other
// platforms NewInitiator and Available return an error.  All protocol,
// negotiation, sysfs and recovery logic is platform independent so it is unit
// tested everywhere.
package iscsi

import (
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/go-logr/logr"
)

// Defaults applied when the corresponding SessionParams / Options field is
// zero.
const (
	DefaultLoginTimeout          = 15 * time.Second
	DefaultReplacementTimeout    = 120 * time.Second
	DefaultNoopOutInterval       = 5 * time.Second
	DefaultNoopOutTimeout        = 5 * time.Second
	DefaultRecoveryRetryInterval = 5 * time.Second
	DefaultSysfsRoot             = "/sys"
	DefaultDevRoot               = "/dev"
	// DefaultPort is the IANA-assigned iSCSI port.
	DefaultPort = 3260
)

// ErrNotSupported is returned by NewInitiator and Available on non-Linux
// platforms.
var ErrNotSupported = errors.New("iSCSI initiator requires Linux")

// ErrSessionRecovering is wrapped by the error Logout returns when it
// refuses to destroy a session whose connection failed while the kernel
// still queues its I/O (sysfs state FAILED, recovery timeout not expired).
// The session keeps being recovered; retry the Logout later.
var ErrSessionRecovering = errors.New("session is recovering from a connection failure")

// Portal is an iSCSI network portal (target address and TCP port).
type Portal struct {
	Address string
	Port    int
}

// String renders the portal as "addr:port", bracketing IPv6 literals.
func (p Portal) String() string {
	return net.JoinHostPort(p.Address, strconv.Itoa(p.Port))
}

// normalized canonicalizes IP literals so "::1" and "0:0::1" compare equal.
func (p Portal) normalized() Portal {
	if ip := net.ParseIP(p.Address); ip != nil {
		p.Address = ip.String()
	}
	return p
}

// SessionParams describes one iSCSI session to log in.
type SessionParams struct {
	InitiatorIQN string
	TargetIQN    string
	Portal       Portal

	// LoginTimeout bounds TCP connect plus the whole login exchange.
	// Zero or negative selects DefaultLoginTimeout.
	LoginTimeout time.Duration
	// ReplacementTimeout is the kernel session recovery timeout
	// (ISCSI_PARAM_SESS_RECOVERY_TMO): how long I/O is queued while the
	// connection is being re-established before it is failed upward.  nil
	// selects DefaultReplacementTimeout; an explicit 0 fails queued I/O as
	// soon as the connection fails.
	ReplacementTimeout *time.Duration
	// NoopOutInterval is the idle time after which the kernel sends a
	// NOP-Out ping (ISCSI_PARAM_RECV_TMO).  nil selects
	// DefaultNoopOutInterval; an explicit 0 disables NOP-Out pings.
	NoopOutInterval *time.Duration
	// NoopOutTimeout is how long the kernel waits for the NOP-In before it
	// declares the connection failed (ISCSI_PARAM_PING_TMO).  nil selects
	// DefaultNoopOutTimeout; an explicit 0 disables the ping timeout.
	NoopOutTimeout *time.Duration
	// CHAP, when set, authenticates the login with CHAP (mutual CHAP when
	// its mutual pair is set) and refuses targets that do not run it.  nil
	// logs in with AuthMethod=None.  The credentials are kept in memory
	// for re-logins after connection loss; a session adopted from sysfs
	// after a restart has none until Login or SetLoginParams supplies them.
	CHAP *CHAPCredentials
}

// withDefaults fills unset fields.  Every timeout pointer of the result is
// non-nil and points to a copy, so the caller's values are never aliased.
func (p SessionParams) withDefaults() SessionParams {
	if p.LoginTimeout <= 0 {
		p.LoginTimeout = DefaultLoginTimeout
	}
	p.ReplacementTimeout = durationOr(p.ReplacementTimeout, DefaultReplacementTimeout)
	p.NoopOutInterval = durationOr(p.NoopOutInterval, DefaultNoopOutInterval)
	p.NoopOutTimeout = durationOr(p.NoopOutTimeout, DefaultNoopOutTimeout)
	if p.Portal.Port == 0 {
		p.Portal.Port = DefaultPort
	}
	p.Portal = p.Portal.normalized()
	p.CHAP = cloneCHAP(p.CHAP)
	return p
}

func durationOr(d *time.Duration, def time.Duration) *time.Duration {
	v := def
	if d != nil {
		v = *d
	}
	return &v
}

func (p SessionParams) validate() error {
	switch {
	case p.InitiatorIQN == "":
		return errors.New("initiator IQN is empty")
	case p.TargetIQN == "":
		return errors.New("target IQN is empty")
	case p.Portal.Address == "":
		return errors.New("portal address is empty")
	case p.Portal.Port < 1 || p.Portal.Port > 65535:
		return errors.New("portal port " + strconv.Itoa(p.Portal.Port) + " is outside 1-65535")
	case *p.ReplacementTimeout < 0:
		return errors.New("replacement timeout " + p.ReplacementTimeout.String() + " is negative")
	case *p.NoopOutInterval < 0:
		return errors.New("NOP-Out interval " + p.NoopOutInterval.String() + " is negative")
	case *p.NoopOutTimeout < 0:
		return errors.New("NOP-Out timeout " + p.NoopOutTimeout.String() + " is negative")
	case p.CHAP != nil:
		return p.CHAP.validate()
	}
	return nil
}

// Session is a snapshot of a kernel iSCSI session owned by this initiator.
type Session struct {
	// SID is the kernel session id (/sys/class/iscsi_session/session<SID>).
	SID int
	// HostNo is the SCSI host number the session is attached to.
	HostNo    int
	TargetIQN string
	Portal    Portal
	// State is the sysfs session state: LOGGED_IN, FAILED or FREE.
	State string
}

// Options configures an Initiator.
type Options struct {
	// SysfsRoot is the sysfs mount point.  Default "/sys".
	SysfsRoot string
	// DevRoot is the directory holding block device nodes.  Default "/dev".
	DevRoot string
	// NetlinkNetnsPath, when set, is a network-namespace file (e.g.
	// /proc/1/ns/net) in which the NETLINK_ISCSI socket is created.  The
	// kernel only serves NETLINK_ISCSI in init_net.  TCP connections are
	// always dialed from the caller's own namespace.
	NetlinkNetnsPath string
	// OwnedTargetPrefix selects the pre-existing kernel sessions (by target
	// IQN prefix) that this process adopts at Start and keeps recovered.
	// Sessions created through Login are always owned.  Empty adopts none.
	OwnedTargetPrefix string
	// RecoveryRetryInterval is the delay between re-login attempts.
	// Default 5s.
	RecoveryRetryInterval time.Duration
	// InitiatorIQN is this node's initiator name.  Every Login must use it,
	// and a pre-existing kernel session is owned only when its sysfs
	// initiatorname equals it (several nodes may share one kernel, e.g.
	// Kind).  Required.
	InitiatorIQN string
	// Logger receives recovery and cleanup diagnostics.  The zero value
	// discards.
	Logger logr.Logger
}

func (o Options) withDefaults() Options {
	if o.SysfsRoot == "" {
		o.SysfsRoot = DefaultSysfsRoot
	}
	if o.DevRoot == "" {
		o.DevRoot = DefaultDevRoot
	}
	if o.RecoveryRetryInterval <= 0 {
		o.RecoveryRetryInterval = DefaultRecoveryRetryInterval
	}
	return o
}
