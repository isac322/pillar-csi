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

package iscsi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

const (
	// Interval at which sysfs is re-read for owned sessions that need
	// recovery (covers connection failures that happened while this
	// process was not running).
	defaultScanInterval = 30 * time.Second
	// Bound on teardown requests issued after the caller's context expired.
	cleanupTimeout = 30 * time.Second
	// Bound on the best-effort Logout Request exchange.
	logoutPDUTimeout = 10 * time.Second
	// Bound on waiting for sysfs to drop a destroyed session.
	sessionGoneTimeout = 10 * time.Second
	// Bound on waiting for sysfs to drop a deleted SCSI device.
	deviceGoneTimeout = 10 * time.Second
	// Timeout DeviceForLUN applies when ctx has no deadline.
	defaultDeviceTimeout = 60 * time.Second
	// Interval at which the host scan is re-triggered while waiting for a LUN.
	rescanEvery = 2 * time.Second
	// Upper bound on login redirections.
	maxRedirects = 4

	// Task-management timeouts (seconds), open-iscsi defaults.
	luResetTimeout  = 30
	tgtResetTimeout = 30
	abortTimeout    = 15
)

// deps are the injectable environment of an Initiator.
type deps struct {
	openKernel   func(handle uint64) (kernelConn, error)
	dial         dialFunc
	devNodes     devNodes
	scanInterval time.Duration
	pollInterval time.Duration
}

type phase int

const (
	phaseLoggingIn phase = iota
	phaseEstablished
	phaseRecovering
	phaseLoggingOut
	phaseRemoved
)

type sessionKey struct {
	target string
	portal Portal
}

// opLock serializes Login / Logout / recovery work on one session and can
// be abandoned when a context ends.
type opLock chan struct{}

func (l opLock) lock(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("acquire session lock: %w", ctx.Err())
	}
}

func (l opLock) unlock() { <-l }

type session struct {
	op     opLock
	key    sessionKey
	params SessionParams
	isid   isid

	// Guarded by Initiator.mu.
	sid, cid, hostNo uint32
	registered       bool
	phase            phase
	cancelRecovery   context.CancelFunc

	// Guarded by op.
	tsih             uint16
	ep               endpoint
	needStop         bool // conn must be STOP_CONN(RECOVER)ed before re-binding
	connDestroyed    bool
	sessionDestroyed bool
	devNodes         map[string]struct{} // guarded by Initiator.mu

	// Kernel event routing for the in-flight login/logout exchange.
	pduCh chan []byte
	errCh chan uint32
}

func newSession(key sessionKey, p SessionParams) *session {
	return &session{
		op:       make(opLock, 1),
		key:      key,
		params:   p,
		isid:     deriveISID(p.InitiatorIQN, key.target, key.portal),
		devNodes: map[string]struct{}{},
		pduCh:    make(chan []byte, 8),
		errCh:    make(chan uint32, 8),
	}
}

// Initiator manages iSCSI sessions through the kernel software iSCSI
// transport.  All methods are safe for concurrent use.
type Initiator struct {
	opts Options
	log  logr.Logger
	fs   sysfs
	deps deps

	mu      sync.Mutex
	started bool
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	kern    kops
	byKey   map[sessionKey]*session
	bySID   map[uint32]*session
	scanNow chan struct{}
	wg      sync.WaitGroup
}

func newInitiator(opts Options, d deps) (*Initiator, error) {
	opts = opts.withDefaults()
	if opts.InitiatorIQN == "" {
		return nil, errors.New("create iSCSI initiator: Options.InitiatorIQN is empty")
	}
	if d.scanInterval <= 0 {
		d.scanInterval = defaultScanInterval
	}
	if d.pollInterval <= 0 {
		d.pollInterval = 200 * time.Millisecond
	}
	return &Initiator{
		opts:    opts,
		log:     opts.Logger.WithName("iscsi"),
		fs:      sysfs{root: opts.SysfsRoot},
		deps:    d,
		byKey:   map[sessionKey]*session{},
		bySID:   map[uint32]*session{},
		scanNow: make(chan struct{}, 1),
	}, nil
}

// Start opens the NETLINK_ISCSI channel, adopts owned sessions that already
// exist in the kernel, and runs the event dispatcher and the recovery
// supervisor until ctx ends or Close is called.
func (i *Initiator) Start(ctx context.Context) error {
	i.mu.Lock()
	if i.started || i.closed {
		i.mu.Unlock()
		return errors.New("start iSCSI initiator: already started or closed")
	}
	handle, err := i.fs.transportHandle()
	if err != nil {
		i.mu.Unlock()
		return fmt.Errorf("start iSCSI initiator: %w", err)
	}
	k, err := i.deps.openKernel(handle)
	if err != nil {
		i.mu.Unlock()
		return fmt.Errorf("start iSCSI initiator: open NETLINK_ISCSI: %w", err)
	}
	i.kern = kops{k: k, handle: handle}
	runCtx, cancel := context.WithCancel(ctx)
	i.ctx, i.cancel = runCtx, cancel
	i.started = true
	i.mu.Unlock()

	i.wg.Add(1)
	go i.dispatch(runCtx, k.events())

	err = i.adopt()
	if err != nil {
		closeErr := i.Close()
		if closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return fmt.Errorf("start iSCSI initiator: adopt existing sessions: %w", err)
	}
	i.wg.Add(1)
	go i.supervise(runCtx)
	return nil
}

// Close stops the dispatcher and supervisor and closes the netlink socket.
// Kernel sessions are left running.
func (i *Initiator) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	started := i.started
	if i.cancel != nil {
		i.cancel()
	}
	i.mu.Unlock()
	if !started {
		return nil
	}
	err := i.kern.k.close()
	i.wg.Wait()
	if err != nil {
		return fmt.Errorf("close NETLINK_ISCSI socket: %w", err)
	}
	return nil
}

func (i *Initiator) running() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	switch {
	case !i.started:
		return errors.New("iSCSI initiator not started")
	case i.closed:
		return errors.New("iSCSI initiator closed")
	case i.ctx.Err() != nil:
		return fmt.Errorf("iSCSI initiator stopped: %w", i.ctx.Err())
	}
	return nil
}

// owned reports whether a sysfs session belongs to this initiator.
func (i *Initiator) owned(s *sysfsSession) bool {
	return i.opts.OwnedTargetPrefix != "" &&
		strings.HasPrefix(s.TargetName, i.opts.OwnedTargetPrefix) &&
		s.InitiatorName == i.opts.InitiatorIQN
}

// dispatch routes kernel events to sessions.  It never issues requests, so
// it cannot deadlock against a request waiting for its reply.
func (i *Initiator) dispatch(ctx context.Context, events <-chan kernelEvent) {
	defer i.wg.Done()
	for ev := range events {
		if ev.Type == eventsLost {
			i.log.Error(errors.New("netlink receive buffer overrun"),
				"iSCSI kernel events may have been lost; rescanning sysfs")
			i.triggerScan()
			continue
		}
		i.route(ctx, ev)
	}
}

// route delivers one kernel event to the owned session it concerns.
func (i *Initiator) route(ctx context.Context, ev kernelEvent) {
	i.mu.Lock()
	s := i.bySID[ev.SID]
	var ph phase
	var cid uint32
	if s != nil {
		ph, cid = s.phase, s.cid
	}
	i.mu.Unlock()
	if s == nil {
		return // not ours (e.g. a host iscsid or another node's session)
	}
	switch ev.Type {
	case kEventRecvPDU:
		if ev.CID != cid {
			return
		}
		select {
		case s.pduCh <- ev.PDU:
		default:
			i.log.Error(errors.New("PDU queue full"), "dropping iSCSI PDU", "sid", ev.SID, "opcode", pduOpcode(ev.PDU))
		}
	case kEventConnError:
		i.connError(ctx, s, ph, ev)
	case kEventConnLoginState:
		i.log.V(1).Info("connection login state", "sid", ev.SID, "state", ev.Code)
	case kEventDestroySession, kEventUnbindSession:
		switch {
		case ph == phaseEstablished || ph == phaseRecovering:
			i.log.Info("kernel removed an owned session", "sid", ev.SID, "event", uEventName(ev.Type),
				"target", s.key.target, "portal", s.key.portal.String())
			if ev.Type == kEventDestroySession {
				i.forget(s)
			}
		case ph == phaseLoggingOut && ev.Type == kEventDestroySession:
			i.log.V(1).Info("kernel destroyed a session being logged out", "sid", ev.SID,
				"target", s.key.target, "portal", s.key.portal.String())
			i.forget(s)
		}
	}
}

// connError wakes an in-flight exchange and starts recovery of an
// established session.
func (i *Initiator) connError(ctx context.Context, s *session, ph phase, ev kernelEvent) {
	select {
	case s.errCh <- ev.Code:
	default:
	}
	if ph == phaseEstablished {
		i.scheduleRecovery(ctx, s, "kernel reported "+connErrorName(ev.Code))
		return
	}
	i.log.V(1).Info("connection error during session transition", "sid", ev.SID, "error", connErrorName(ev.Code))
}

func (i *Initiator) triggerScan() {
	select {
	case i.scanNow <- struct{}{}:
	default:
	}
}

// register makes kernel events for sid route to s.
func (i *Initiator) register(s *session, sid, hostNo uint32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s.sid, s.hostNo, s.registered = sid, hostNo, true
	i.bySID[sid] = s
}

// forget drops a session from all indexes and cancels its recovery.
func (i *Initiator) forget(s *session) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if s.cancelRecovery != nil {
		s.cancelRecovery()
		s.cancelRecovery = nil
	}
	s.phase = phaseRemoved
	if i.byKey[s.key] == s {
		delete(i.byKey, s.key)
	}
	if s.registered && i.bySID[s.sid] == s {
		delete(i.bySID, s.sid)
	}
}

func (i *Initiator) ids(s *session) (sid, cid, hostNo uint32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return s.sid, s.cid, s.hostNo
}

func (i *Initiator) setPhase(s *session, from, to phase) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if s.phase != from {
		return false
	}
	s.phase = to
	return true
}

// Login establishes a session, or returns the existing one for the same
// (TargetIQN, Portal).
func (i *Initiator) Login(ctx context.Context, p SessionParams) (*Session, error) {
	p = p.withDefaults()
	err := p.validate()
	if err != nil {
		return nil, fmt.Errorf("login to target %q: %w", p.TargetIQN, err)
	}
	if p.InitiatorIQN != i.opts.InitiatorIQN {
		return nil, fmt.Errorf("login to target %s: initiator IQN %q differs from this node's %q",
			p.TargetIQN, p.InitiatorIQN, i.opts.InitiatorIQN)
	}
	err = i.running()
	if err != nil {
		return nil, fmt.Errorf("login to target %s at %s: %w", p.TargetIQN, p.Portal, err)
	}
	key := sessionKey{target: p.TargetIQN, portal: p.Portal}
	for {
		i.mu.Lock()
		s := i.byKey[key]
		if s == nil {
			s = newSession(key, p)
			s.phase = phaseLoggingIn
			s.op <- struct{}{} // cannot block: fresh lock
			i.byKey[key] = s
			i.mu.Unlock()
			err = i.establish(ctx, s)
			s.op.unlock()
			if err != nil {
				return nil, err
			}
			return i.snapshot(s)
		}
		i.mu.Unlock()

		err = s.op.lock(ctx)
		if err != nil {
			return nil, fmt.Errorf("login to target %s at %s: wait for in-flight operation: %w",
				p.TargetIQN, p.Portal, err)
		}
		i.mu.Lock()
		ph := s.phase
		i.mu.Unlock()
		s.op.unlock()
		switch ph {
		case phaseRemoved:
			continue
		case phaseLoggingOut:
			return nil, fmt.Errorf("login to target %s at %s: session is being logged out; retry after Logout completes",
				p.TargetIQN, p.Portal)
		}
		return i.snapshot(s)
	}
}

// establish creates the kernel session and connection and logs in.  The
// caller holds s.op.
func (i *Initiator) establish(ctx context.Context, s *session) error {
	k := i.kern
	target, portal := s.key.target, s.key.portal
	fail := func(err error) error {
		cerr := i.destroyKernelSession(ctx, s)
		i.forget(s)
		return errors.Join(err, cerr)
	}

	sid, hostNo, err := k.createSession(ctx)
	if err != nil {
		i.forget(s)
		return fmt.Errorf("login to target %s at %s: create kernel session: %w", target, portal, err)
	}
	i.register(s, sid, hostNo)
	s.connDestroyed = true // no connection yet
	cid, err := k.createConn(ctx, sid, 0)
	if err != nil {
		return fail(fmt.Errorf("login to target %s at %s: create connection on session %d: %w", target, portal, sid, err))
	}
	i.mu.Lock()
	s.cid = cid
	i.mu.Unlock()
	s.connDestroyed = false

	err = i.connectAndLogin(ctx, s)
	if err != nil {
		return fail(err)
	}
	if !i.setPhase(s, phaseLoggingIn, phaseEstablished) {
		return fmt.Errorf("login to target %s at %s: session %d was logged out concurrently", target, portal, sid)
	}
	return nil
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// loginAttempt is the progress of one connectAndLogin call across
// redirects and session-reinstatement retries.
type loginAttempt struct {
	s         *session
	sid, cid  uint32
	portal    Portal
	tsih      uint16
	redirects int
}

// connectAndLogin dials the portal, binds the socket to the kernel
// connection, runs the login, pushes the negotiated parameters and starts
// the connection.  On failure the connection is left stopped (unbound) and
// the socket closed.  The caller holds s.op.
func (i *Initiator) connectAndLogin(ctx context.Context, s *session) error {
	sid, cid, _ := i.ids(s)
	lctx, cancel := context.WithTimeout(ctx, s.params.LoginTimeout)
	defer cancel()

	a := &loginAttempt{s: s, sid: sid, cid: cid, portal: s.key.portal, tsih: s.tsih}
	for {
		retry, err := i.loginOnce(lctx, a)
		if !retry {
			return err
		}
	}
}

// loginOnce performs one dial/bind/login/start round.  The retry result is
// true when the target asked for another attempt (redirect or dropped
// session).
func (i *Initiator) loginOnce(ctx context.Context, a *loginAttempt) (retry bool, err error) {
	drain(a.s.pduCh)
	drainErr(a.s.errCh)
	ep, err := i.bindEndpoint(ctx, a)
	if err != nil {
		return false, err
	}
	res, err := loginSession(ctx, i.exchanger(a.s, a.sid, a.cid), loginInput{
		InitiatorIQN: a.s.params.InitiatorIQN,
		TargetIQN:    a.s.key.target,
		ISID:         a.s.isid,
		TSIH:         a.tsih,
		CID:          uint16(a.cid), //nolint:gosec // G115: the kernel allocates CIDs from 0 per session.
	}, a.portal)
	if err != nil {
		return i.retryLogin(ctx, a, ep, err)
	}
	err = i.pushParams(ctx, a.s, a.sid, a.cid, res)
	if err != nil {
		return false, i.unbind(ctx, a, ep, fmt.Errorf("login to target %s at %s: %w", a.s.key.target, a.portal, err))
	}
	err = i.kern.startConn(ctx, a.sid, a.cid)
	if err != nil {
		return false, i.unbind(ctx, a, ep, fmt.Errorf("login to target %s at %s: start connection %d:%d: %w",
			a.s.key.target, a.portal, a.sid, a.cid, err))
	}
	if a.s.ep != nil {
		closeErr := a.s.ep.Close()
		if closeErr != nil {
			i.log.Error(closeErr, "close previous socket", "sid", a.sid)
		}
	}
	a.s.ep = ep
	a.s.tsih = res.TSIH
	return false, nil
}

// bindEndpoint dials the current portal and binds the socket to the kernel
// connection.
func (i *Initiator) bindEndpoint(ctx context.Context, a *loginAttempt) (endpoint, error) {
	ep, err := i.deps.dial(ctx, a.portal)
	if err != nil {
		return nil, fmt.Errorf("login to target %s: %w", a.s.key.target, err)
	}
	err = i.kern.bindConn(ctx, a.sid, a.cid, ep.fd(), true)
	if err != nil {
		return nil, fmt.Errorf("login to target %s at %s: bind socket to connection %d:%d: %w",
			a.s.key.target, a.portal, a.sid, a.cid, errors.Join(err, ep.Close()))
	}
	a.s.needStop = true
	return ep, nil
}

// retryLogin decides whether a failed login is retried: redirects are
// followed and a dropped session is re-established as a new one.  In every
// case the connection is unbound.
func (i *Initiator) retryLogin(ctx context.Context, a *loginAttempt, ep endpoint, err error) (bool, error) {
	le, ok := errors.AsType[*LoginError](err)
	if !ok {
		return false, i.unbind(ctx, a, ep, err)
	}
	switch {
	case le.Redirect != nil && a.redirects < maxRedirects:
		uerr := i.unbind(ctx, a, ep, nil)
		if uerr != nil {
			return false, errors.Join(err, uerr) //nolint:wrapcheck // both operands are wrapped/annotated
		}
		i.log.Info("login redirected", "target", a.s.key.target, "from", a.portal.String(), "to", le.Redirect.String())
		a.portal = *le.Redirect
		a.redirects++
		return true, nil
	case le.sessionDoesNotExist() && a.tsih != 0:
		uerr := i.unbind(ctx, a, ep, nil)
		if uerr != nil {
			return false, errors.Join(err, uerr) //nolint:wrapcheck // both operands are wrapped/annotated
		}
		i.log.Info("target dropped the previous session; logging in as a new session", "target", a.s.key.target)
		a.tsih = 0
		return true, nil
	}
	return false, i.unbind(ctx, a, ep, err)
}

// unbind stops the connection after a failed login and closes its socket,
// returning cause joined with any cleanup failure.
func (i *Initiator) unbind(ctx context.Context, a *loginAttempt, ep endpoint, cause error) error {
	cctx, cancel := detached(ctx)
	defer cancel()
	errs := []error{cause}
	err := i.kern.stopConn(cctx, a.sid, a.cid, stopConnRecover)
	if err != nil {
		errs = append(errs, fmt.Errorf("stop connection %d:%d after failed login: %w", a.sid, a.cid, err))
	} else {
		a.s.needStop = false
	}
	err = ep.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("close socket to %s: %w", a.portal, err))
	}
	return errors.Join(errs...) //nolint:wrapcheck // multi-error is already wrapped per item
}

func drain(ch chan []byte) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func drainErr(ch chan uint32) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// exchanger sends a login PDU through the kernel and waits for the
// response routed by the dispatcher.
func (i *Initiator) exchanger(s *session, sid, cid uint32) loginExchanger {
	return func(ctx context.Context, req *loginRequest) (*loginResponse, error) {
		req.CID = uint16(cid) //nolint:gosec // G115: the kernel allocates CIDs from 0 per session.
		hdr, data, err := req.marshal()
		if err != nil {
			return nil, err
		}
		pdu, err := i.roundTrip(ctx, s, sid, cid, hdr, data)
		if err != nil {
			return nil, err
		}
		return parseLoginResponse(pdu)
	}
}

func (i *Initiator) roundTrip(ctx context.Context, s *session, sid, cid uint32, hdr, data []byte) ([]byte, error) {
	drain(s.pduCh)
	err := i.kern.sendPDU(ctx, sid, cid, hdr, data)
	if err != nil {
		return nil, fmt.Errorf("send PDU on connection %d:%d: %w", sid, cid, err)
	}
	select {
	case pdu := <-s.pduCh:
		return pdu, nil
	case code := <-s.errCh:
		return nil, fmt.Errorf("connection %d:%d failed while waiting for a response: %s", sid, cid, connErrorName(code))
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for response on connection %d:%d: %w", sid, cid, ctx.Err())
	}
}

type paramValue struct {
	p iscsiParam
	v string
	// optional parameters are not implemented by every transport; the
	// software transport answers ENOSYS (libiscsi iscsi_set_param default).
	optional bool
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func seconds(d time.Duration) string {
	return strconv.Itoa(int(d / time.Second))
}

// negotiatedParams lists the SET_PARAM calls after a successful login, in
// the order open-iscsi issues them.
func negotiatedParams(s *session, res *loginResult) []paramValue {
	n := res.Params
	ps := []paramValue{
		{p: paramInitiatorName, v: s.params.InitiatorIQN},
		{p: paramTargetName, v: s.key.target},
		{p: paramMaxRecvDLength, v: strconv.FormatUint(uint64(n.MaxRecvDLength), 10)},
		{p: paramMaxXmitDLength, v: strconv.FormatUint(uint64(n.MaxXmitDLength), 10)},
		{p: paramHdrDgstEn, v: boolParam(n.HeaderDigest)},
		{p: paramDataDgstEn, v: boolParam(n.DataDigest)},
		{p: paramInitialR2TEn, v: boolParam(n.InitialR2T)},
		{p: paramMaxR2T, v: strconv.FormatUint(uint64(n.MaxOutstandingR2T), 10)},
		{p: paramImmDataEn, v: boolParam(n.ImmediateData)},
		{p: paramFirstBurst, v: strconv.FormatUint(uint64(n.FirstBurstLength), 10)},
		{p: paramMaxBurst, v: strconv.FormatUint(uint64(n.MaxBurstLength), 10)},
		{p: paramPDUInorderEn, v: boolParam(n.DataPDUInOrder)},
		{p: paramDataSeqInorderEn, v: boolParam(n.DataSequenceInOrder)},
		{p: paramERL, v: strconv.FormatUint(uint64(n.ErrorRecoveryLevel), 10)},
		{p: paramIFMarkerEn, v: boolParam(n.IFMarker), optional: true},
		{p: paramOFMarkerEn, v: boolParam(n.OFMarker), optional: true},
		{p: paramExpStatSN, v: strconv.FormatUint(uint64(res.StatSN+1), 10)},
		{p: paramPersistentAddress, v: s.key.portal.Address},
		{p: paramPersistentPort, v: strconv.Itoa(s.key.portal.Port)},
		{p: paramISID, v: s.isid.String(), optional: true},
		{p: paramSessRecoveryTmo, v: seconds(*s.params.ReplacementTimeout)},
		{p: paramPingTmo, v: seconds(*s.params.NoopOutTimeout)},
		{p: paramRecvTmo, v: seconds(*s.params.NoopOutInterval)},
		{p: paramLUResetTmo, v: strconv.Itoa(luResetTimeout)},
		{p: paramTgtResetTmo, v: strconv.Itoa(tgtResetTimeout)},
		{p: paramAbortTmo, v: strconv.Itoa(abortTimeout)},
	}
	if res.TPGT >= 0 {
		ps = append(ps, paramValue{p: paramTPGT, v: strconv.Itoa(res.TPGT)})
	}
	return ps
}

func (i *Initiator) pushParams(ctx context.Context, s *session, sid, cid uint32, res *loginResult) error {
	for _, pv := range negotiatedParams(s, res) {
		err := i.kern.setParam(ctx, sid, cid, pv.p, pv.v)
		if err != nil && pv.optional && isKernelErrno(err, linuxENOSYS) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// destroyKernelSession tears down the kernel objects of s that still exist.
// A session the kernel already removed (the target or recovery timeout
// destroyed it) counts as torn down.  The caller holds s.op.
func (i *Initiator) destroyKernelSession(ctx context.Context, s *session) error {
	cctx, cancel := detached(ctx)
	defer cancel()
	sid, cid, _ := i.ids(s)
	if !s.connDestroyed {
		err := i.destroyKernelConn(cctx, s, sid, cid)
		if err != nil {
			return err
		}
	}
	if !s.sessionDestroyed {
		err := i.kern.destroySession(cctx, sid)
		if err != nil {
			gone, gerr := i.sessionVanished(sid, err)
			if !gone {
				return fmt.Errorf("destroy session %d: %w", sid, gerr)
			}
		}
		s.sessionDestroyed = true
	}
	return nil
}

// destroyKernelConn stops and destroys the connection of s.  When the kernel
// rejects that because the whole session is gone, the session is marked
// destroyed.  The caller holds s.op.
func (i *Initiator) destroyKernelConn(ctx context.Context, s *session, sid, cid uint32) error {
	err := i.kern.stopConn(ctx, sid, cid, stopConnTerm)
	if err != nil {
		gone, gerr := i.sessionVanished(sid, err)
		if !gone {
			return fmt.Errorf("stop connection %d:%d: %w", sid, cid, gerr)
		}
		i.log.Info("session already removed from the kernel", "sid", sid, "target", s.key.target,
			"portal", s.key.portal.String())
		s.sessionDestroyed = true
	}
	s.needStop = false
	if s.ep != nil {
		closeErr := s.ep.Close()
		if closeErr != nil {
			i.log.Error(closeErr, "close socket of stopped connection", "sid", sid)
		}
		s.ep = nil
	}
	if !s.sessionDestroyed {
		err = i.kern.destroyConn(ctx, sid, cid)
		if err != nil {
			gone, gerr := i.sessionVanished(sid, err)
			if !gone {
				return fmt.Errorf("destroy connection %d:%d: %w", sid, cid, gerr)
			}
			s.sessionDestroyed = true
		}
	}
	s.connDestroyed = true
	return nil
}

// sessionVanished reports whether a failed teardown request (err) failed
// because the kernel no longer has session sid: the kernel answers EINVAL
// for unknown sessions and connections, and sysfs confirms the absence.
// Otherwise it returns err, joined with any sysfs read failure.
func (i *Initiator) sessionVanished(sid uint32, err error) (bool, error) {
	if !isKernelErrno(err, linuxEINVAL) {
		return false, err
	}
	exists, serr := i.fs.sessionExists(int(sid))
	if serr != nil {
		return false, fmt.Errorf("%w (check session %d in sysfs: %w)", err, sid, serr)
	}
	return !exists, err
}

// snapshot returns the public view of s with the current sysfs state.
func (i *Initiator) snapshot(s *session) (*Session, error) {
	sid, _, hostNo := i.ids(s)
	state, err := readTrimmed(filepath.Join(i.fs.sessionDir(int(sid)), "state"))
	if err != nil {
		return nil, fmt.Errorf("read state of session %d (target %s at %s): %w", sid, s.key.target, s.key.portal, err)
	}
	return &Session{
		SID:       int(sid),
		HostNo:    int(hostNo),
		TargetIQN: s.key.target,
		Portal:    s.key.portal,
		State:     state,
	}, nil
}

// lookup finds the owned session for key, adopting it from sysfs if it
// appeared since the last scan.
func (i *Initiator) lookup(key sessionKey) (*session, error) {
	i.mu.Lock()
	s := i.byKey[key]
	i.mu.Unlock()
	if s != nil {
		return s, nil
	}
	err := i.adopt()
	if err != nil {
		return nil, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.byKey[key], nil
}

// FindSession returns the owned session for (targetIQN, portal), or nil
// when there is none.
func (i *Initiator) FindSession(targetIQN string, portal Portal) (*Session, error) {
	err := i.running()
	if err != nil {
		return nil, fmt.Errorf("find session for target %s at %s: %w", targetIQN, portal, err)
	}
	key := sessionKey{target: targetIQN, portal: portal.normalized()}
	s, err := i.lookup(key)
	if err != nil {
		return nil, fmt.Errorf("find session for target %s at %s: %w", targetIQN, portal, err)
	}
	if s == nil {
		return nil, nil //nolint:nilnil // documented: a nil session means none exists
	}
	i.mu.Lock()
	ph := s.phase
	i.mu.Unlock()
	if ph == phaseLoggingIn || ph == phaseRemoved {
		return nil, nil //nolint:nilnil // documented: a nil session means none exists
	}
	sid, _, _ := i.ids(s)
	exists, err := i.fs.sessionExists(int(sid))
	if err != nil {
		return nil, fmt.Errorf("find session for target %s at %s: %w", targetIQN, portal, err)
	}
	if !exists {
		if ph != phaseLoggingOut {
			i.log.Info("owned session vanished from sysfs", "sid", sid, "target", targetIQN)
			i.forget(s)
		}
		return nil, nil //nolint:nilnil // documented: a nil session means none exists
	}
	return i.snapshot(s)
}

// DeviceForLUN scans the session's SCSI host for lun, waits for the block
// device, and makes sure its node exists under DevRoot.
func (i *Initiator) DeviceForLUN(ctx context.Context, s *Session, lun int) (string, error) {
	if s == nil {
		return "", errors.New("find device for LUN: nil session")
	}
	if lun < 0 {
		return "", fmt.Errorf("find device for LUN %d of session %d: negative LUN", lun, s.SID)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultDeviceTimeout)
		defer cancel()
	}
	var lastScan time.Time
	for {
		if time.Since(lastScan) >= rescanEvery {
			err := i.fs.scanHost(s.HostNo, lun)
			if err != nil {
				return "", fmt.Errorf("scan LUN %d of session %d (target %s): %w", lun, s.SID, s.TargetIQN, err)
			}
			lastScan = time.Now()
		}
		dev, obs, err := i.probeLUN(s, lun)
		if err != nil {
			return "", err
		}
		if dev != "" {
			return dev, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for LUN %d of session %d (target %s at %s): %s: %w",
				lun, s.SID, s.TargetIQN, s.Portal, obs, ctx.Err())
		case <-time.After(i.deps.pollInterval):
		}
	}
}

// probeLUN returns the device path when the LUN is ready, or a description
// of what is still missing.
func (i *Initiator) probeLUN(s *Session, lun int) (path, missing string, err error) {
	devs, err := i.fs.sessionLUNs(s.SID)
	if err != nil {
		return "", "", err
	}
	for _, d := range devs {
		if d.LUN == lun {
			return i.probeDevice(s, lun, d)
		}
	}
	return "", "no SCSI device for the LUN yet", nil
}

// probeDevice checks one SCSI device of the LUN and ensures its block
// device node once the device is running.  The device is ready only once
// the node can be opened with a non-zero capacity: sysfs exposes the disk
// while the kernel is still registering it, and a probe in that window
// (blkid before mkfs) cannot tell an unopenable disk from a blank one.
func (i *Initiator) probeDevice(s *Session, lun int, d lunDevice) (path, missing string, err error) {
	if d.Block == "" {
		return "", "SCSI device " + d.HCTL + " has no block device yet", nil
	}
	state, err := i.fs.scsiDeviceState(d.HCTL)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("read state of SCSI device %s: %w", d.HCTL, err)
	}
	if err == nil && state != "running" {
		return "", "SCSI device " + d.HCTL + " state is " + state, nil
	}
	path, err = i.ensureDevNode(s.SID, d)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "block device " + d.Block + " not registered yet", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("LUN %d of session %d: %w", lun, s.SID, err)
	}
	missing, err = i.deps.devNodes.usable(path)
	if err != nil {
		return "", "", fmt.Errorf("check block device %s for LUN %d of session %d: %w", path, lun, s.SID, err)
	}
	if missing != "" {
		return "", missing, nil
	}
	return path, "", nil
}

// ensureDevNode makes sure the block device node of a SCSI device exists
// under DevRoot and returns its path.  A node it creates is recorded on the
// session for removal at logout.  It returns an error satisfying
// errors.Is(err, fs.ErrNotExist) when the block device is not registered.
func (i *Initiator) ensureDevNode(sid int, d lunDevice) (string, error) {
	major, minor, err := i.fs.blockDevNumber(sid, d)
	if err != nil {
		return "", err
	}
	path := filepath.Join(i.opts.DevRoot, d.Block)
	created, err := i.deps.devNodes.ensure(path, major, minor)
	if err != nil {
		return "", fmt.Errorf("create device node %s (%d:%d): %w", path, major, minor, err)
	}
	if created {
		i.log.Info("created block device node", "path", path, "major", major, "minor", minor, "sid", sid)
		i.mu.Lock()
		//nolint:gosec // G115: session ids come from the kernel's uint32 sid.
		if sess := i.bySID[uint32(sid)]; sess != nil {
			sess.devNodes[path] = struct{}{}
		}
		i.mu.Unlock()
	}
	return path, nil
}

// Rescan re-reads the capacity of every LUN of the session.
func (i *Initiator) Rescan(ctx context.Context, targetIQN string, portal Portal) error {
	err := i.running()
	if err != nil {
		return fmt.Errorf("rescan target %s at %s: %w", targetIQN, portal, err)
	}
	s, err := i.lookup(sessionKey{target: targetIQN, portal: portal.normalized()})
	if err != nil {
		return fmt.Errorf("rescan target %s at %s: %w", targetIQN, portal, err)
	}
	if s == nil {
		return fmt.Errorf("rescan target %s at %s: no iSCSI session", targetIQN, portal)
	}
	sid, _, _ := i.ids(s)
	devs, err := i.fs.sessionLUNs(int(sid))
	if err != nil {
		return fmt.Errorf("rescan target %s at %s: %w", targetIQN, portal, err)
	}
	if len(devs) == 0 {
		return fmt.Errorf("rescan target %s at %s: session %d has no SCSI devices", targetIQN, portal, sid)
	}
	var errs []error
	for _, d := range devs {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return fmt.Errorf("rescan target %s at %s: %w", targetIQN, portal, ctxErr)
		}
		err = i.fs.rescanDevice(d.HCTL)
		if err != nil {
			errs = append(errs, fmt.Errorf("rescan target %s at %s: %w", targetIQN, portal, err))
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // multi-error is already wrapped per item
}

// Logout logs out and destroys the session for (targetIQN, portal).  It is
// a no-op when no such session exists.  The session's SCSI devices are
// flushed and deleted first, while the connection is still live.
func (i *Initiator) Logout(ctx context.Context, targetIQN string, portal Portal) error {
	err := i.running()
	if err != nil {
		return fmt.Errorf("logout from target %s at %s: %w", targetIQN, portal, err)
	}
	portal = portal.normalized()
	s, err := i.lookup(sessionKey{target: targetIQN, portal: portal})
	if err != nil {
		return fmt.Errorf("logout from target %s at %s: %w", targetIQN, portal, err)
	}
	if s == nil {
		return nil
	}
	i.mu.Lock()
	prev := s.phase
	if prev == phaseRemoved {
		i.mu.Unlock()
		return nil
	}
	s.phase = phaseLoggingOut
	if s.cancelRecovery != nil {
		s.cancelRecovery()
		s.cancelRecovery = nil
	}
	i.mu.Unlock()

	err = s.op.lock(ctx)
	if err != nil {
		return fmt.Errorf("logout from target %s at %s: wait for in-flight operation: %w", targetIQN, portal, err)
	}
	defer s.op.unlock()
	i.mu.Lock()
	if s.phase == phaseRemoved {
		i.mu.Unlock()
		return nil
	}
	s.phase = phaseLoggingOut // an in-flight Login may have raced us
	i.mu.Unlock()

	sid, cid, _ := i.ids(s)
	err = i.removeSCSIDevices(ctx, s, sid)
	if err != nil {
		return fmt.Errorf("logout from target %s at %s: %w", targetIQN, portal, err)
	}
	if !s.connDestroyed {
		i.sendLogoutPDU(ctx, s, sid, cid)
	}
	err = i.destroyKernelSession(ctx, s)
	if err != nil {
		return fmt.Errorf("logout from target %s at %s: %w", targetIQN, portal, err)
	}
	err = i.waitSessionGone(ctx, sid)
	if err != nil {
		return fmt.Errorf("logout from target %s at %s: %w", targetIQN, portal, err)
	}
	i.removeDevNodes(s, sid)
	i.forget(s)
	return nil
}

// removeSCSIDevices flushes and deletes the session's SCSI devices while the
// session is still logged in, so the flush reaches the target.  Tearing the
// session down first removes the devices after the transport is gone: their
// dirty page cache (kept when a holder still has the disk open; only the
// last close writes it back) and the removal-time SYNCHRONIZE CACHE fail,
// losing data of a raw block volume.  The explicit fsync is needed because
// deleting a device refuses page-cache write-back once removal has started.
// A session that is not logged in cannot be flushed (I/O would block until
// the replacement timeout and then fail), so its devices are left for the
// kernel to drop with the session.  Open holders do not block deletion;
// their later I/O fails with ENXIO.
func (i *Initiator) removeSCSIDevices(ctx context.Context, s *session, sid uint32) error {
	devs, err := i.fs.sessionLUNs(int(sid))
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return nil
	}
	state, err := readTrimmed(filepath.Join(i.fs.sessionDir(int(sid)), "state"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // session already gone with its devices
	}
	if err != nil {
		return fmt.Errorf("read state of session %d: %w", sid, err)
	}
	if s.connDestroyed || state != sessionStateLoggedIn {
		hctls := make([]string, 0, len(devs))
		for _, d := range devs {
			hctls = append(hctls, d.HCTL)
		}
		i.log.Info("session is not logged in; SCSI devices cannot be flushed before logout "+
			"and data still cached for them is lost", "sid", sid, "state", state, "devices", hctls,
			"target", s.key.target, "portal", s.key.portal.String())
		return nil
	}
	for _, d := range devs {
		err = i.flushAndDeleteDevice(ctx, int(sid), d)
		if err != nil {
			return fmt.Errorf("flush and delete SCSI device %s of session %d: %w", d.HCTL, sid, err)
		}
	}
	return nil
}

func (i *Initiator) flushAndDeleteDevice(ctx context.Context, sid int, d lunDevice) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("before flush: %w", err)
	}
	if d.Block != "" {
		var path, missing string
		path, err = i.ensureDevNode(sid, d)
		if err != nil {
			return err
		}
		missing, err = i.deps.devNodes.flush(path)
		if err != nil {
			return fmt.Errorf("flush block device: %w", err)
		}
		if missing != "" {
			i.log.Info("block device not flushed before delete", "sid", sid, "device", d.HCTL, "reason", missing)
		}
	}
	err = i.fs.deleteDevice(d.HCTL)
	if err != nil {
		return err
	}
	return i.waitDeviceGone(ctx, d.HCTL)
}

func (i *Initiator) waitDeviceGone(ctx context.Context, hctl string) error {
	wctx, cancel := context.WithTimeout(ctx, deviceGoneTimeout)
	defer cancel()
	for {
		exists, err := i.fs.scsiDeviceExists(hctl)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("SCSI device %s still present in sysfs after delete: %w", hctl, wctx.Err())
		case <-time.After(i.deps.pollInterval):
		}
	}
}

// removeDevNodes removes the block device nodes this initiator created for
// the session.  Failures are logged: stale nodes are replaced on reuse.
func (i *Initiator) removeDevNodes(s *session, sid uint32) {
	i.mu.Lock()
	nodes := make([]string, 0, len(s.devNodes))
	for path := range s.devNodes {
		nodes = append(nodes, path)
	}
	i.mu.Unlock()
	for _, path := range nodes {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			i.log.Error(err, "remove device node created for the session", "path", path, "sid", sid)
		}
	}
}

// sendLogoutPDU performs the best-effort Logout Request exchange while the
// connection is logged in.  Failures are logged: the session is torn down
// by STOP_CONN(TERM) regardless.
func (i *Initiator) sendLogoutPDU(ctx context.Context, s *session, sid, cid uint32) {
	connState, err := readTrimmed(
		i.fs.path("class", "iscsi_connection", fmt.Sprintf("connection%d:%d", sid, cid), "state"))
	if err != nil || connState != connStateUp {
		i.log.V(1).Info("skipping logout PDU; connection not up", "sid", sid, "state", connState, "err", err)
		return
	}
	lctx, cancel := context.WithTimeout(ctx, logoutPDUTimeout)
	defer cancel()
	drainErr(s.errCh)
	pdu, err := i.roundTrip(lctx, s, sid, cid, marshalLogoutRequest(), nil)
	if err == nil {
		var code uint8
		code, err = parseLogoutResponse(pdu)
		if err == nil && code != 0 {
			err = fmt.Errorf("target answered logout with response code %d", code)
		}
	}
	if err != nil {
		i.log.Error(err, "logout PDU failed; terminating the connection anyway", "sid", sid, "target", s.key.target)
	}
}

func (i *Initiator) waitSessionGone(ctx context.Context, sid uint32) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionGoneTimeout)
	defer cancel()
	for {
		exists, err := i.fs.sessionExists(int(sid))
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("session %d still present in sysfs after DESTROY_SESSION: %w", sid, wctx.Err())
		case <-time.After(i.deps.pollInterval):
		}
	}
}
