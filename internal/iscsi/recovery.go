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

// Session recovery: what iscsid does when the kernel reports a connection
// error.  The kernel keeps the SCSI host and block devices, queues I/O for
// the session recovery timeout, and waits for userspace to STOP_CONN,
// re-bind a new socket, log in again and START_CONN.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// scheduleRecovery starts a recovery loop for an established session.
// It is a no-op when the session is in any other phase (already
// recovering, logging in or out, removed).
func (i *Initiator) scheduleRecovery(ctx context.Context, s *session, reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.ctx == nil || i.ctx.Err() != nil || s.phase != phaseEstablished {
		return
	}
	s.phase = phaseRecovering
	rctx, cancel := context.WithCancel(ctx)
	s.cancelRecovery = cancel
	i.wg.Add(1)
	go i.recoverLoop(rctx, s, reason)
}

func (i *Initiator) recoverLoop(ctx context.Context, s *session, reason string) {
	defer i.wg.Done()
	sid, _, _ := i.ids(s)
	log := i.log.WithValues("sid", sid, "target", s.key.target, "portal", s.key.portal.String())
	log.Info("recovering iSCSI session", "reason", reason)
	for attempt := 1; ; attempt++ {
		err := s.op.lock(ctx)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			s.op.unlock()
			return
		}
		err = i.recoverOnce(ctx, s)
		s.op.unlock()
		if errors.Is(err, errSessionGone) {
			log.Info("iSCSI session disappeared from the kernel; stopping recovery")
			return
		}
		if err == nil {
			i.mu.Lock()
			if s.phase == phaseRecovering {
				s.phase = phaseEstablished
				s.cancelRecovery = nil
			}
			i.mu.Unlock()
			log.Info("iSCSI session recovered", "attempts", attempt)
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Error(err, "iSCSI session re-login failed; retrying", "attempt", attempt,
			"retryIn", i.opts.RecoveryRetryInterval.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(i.opts.RecoveryRetryInterval):
		}
	}
}

// errSessionGone reports that the kernel session vanished during recovery.
var errSessionGone = errors.New("kernel session no longer exists")

// recoverOnce performs one re-login attempt.  The caller holds s.op.
func (i *Initiator) recoverOnce(ctx context.Context, s *session) error {
	sid, cid, _ := i.ids(s)
	exists, err := i.fs.sessionExists(int(sid))
	if err != nil {
		return err
	}
	if !exists {
		i.forget(s)
		return errSessionGone
	}
	if s.needStop {
		// Unbinds the failed socket and flushes the kernel's conn cleanup
		// so BIND_CONN is accepted again (ISCSI_CLS_CONN_BIT_CLEANUP).
		err = i.kern.stopConn(ctx, sid, cid, stopConnRecover)
		if err != nil {
			return fmt.Errorf("stop failed connection %d:%d for recovery: %w", sid, cid, err)
		}
		s.needStop = false
		if s.ep != nil {
			closeErr := s.ep.Close()
			if closeErr != nil {
				i.log.Error(closeErr, "close socket of failed connection", "sid", sid)
			}
			s.ep = nil
		}
	}
	return i.connectAndLogin(ctx, s)
}

// supervise periodically re-reads sysfs to adopt owned sessions and to
// recover those the kernel reports as failed (e.g. the connection broke
// while this process was not running, or an event was lost).
func (i *Initiator) supervise(ctx context.Context) {
	defer i.wg.Done()
	i.scanOnce(ctx)
	t := time.NewTicker(i.deps.scanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-i.scanNow:
		}
		i.scanOnce(ctx)
	}
}

func (i *Initiator) scanOnce(ctx context.Context) {
	err := i.adopt()
	if err != nil {
		i.log.Error(err, "adopt owned iSCSI sessions from sysfs")
	}
	i.mu.Lock()
	var candidates []*session
	for _, s := range i.byKey {
		if s.phase == phaseEstablished {
			candidates = append(candidates, s)
		}
	}
	i.mu.Unlock()
	for _, s := range candidates {
		i.checkSession(ctx, s)
	}
}

// checkSession forgets an owned session that vanished from the kernel and
// schedules recovery of one sysfs reports as unhealthy.
func (i *Initiator) checkSession(ctx context.Context, s *session) {
	sid, _, _ := i.ids(s)
	ss, err := i.fs.readSession(int(sid))
	if errors.Is(err, fs.ErrNotExist) {
		exists, existsErr := i.fs.sessionExists(int(sid))
		if existsErr != nil {
			i.log.Error(existsErr, "check whether owned iSCSI session still exists", "sid", sid)
		}
		if !exists {
			i.log.Info("owned iSCSI session no longer exists in the kernel", "sid", sid, "target", s.key.target)
			i.forget(s)
			return
		}
	}
	if err != nil {
		i.log.Error(err, "read owned iSCSI session state", "sid", sid)
		return
	}
	if !ss.healthy() {
		i.scheduleRecovery(ctx, s, fmt.Sprintf("sysfs session state %s, connections %v", ss.State, connStates(ss)))
	}
}

func connStates(s *sysfsSession) []string {
	out := make([]string, 0, len(s.Conns))
	for _, c := range s.Conns {
		out = append(out, fmt.Sprintf("%d:%s", c.CID, c.State))
	}
	return out
}

// adopt registers owned kernel sessions that this process does not know
// yet, so they can be found, rescanned, logged out and recovered.
func (i *Initiator) adopt() error {
	list, err := i.fs.listSessions()
	if err != nil {
		return err
	}
	for _, ss := range list {
		if !i.owned(ss) {
			continue
		}
		i.mu.Lock()
		_, known := i.bySID[uint32(ss.SID)] //nolint:gosec // G115: sysfs session ids are the kernel's uint32 sid.
		i.mu.Unlock()
		if known {
			continue
		}
		s, err := sessionFromSysfs(ss)
		if err != nil {
			i.log.Error(err, "cannot adopt owned iSCSI session", "sid", ss.SID, "target", ss.TargetName)
			continue
		}
		i.mu.Lock()
		if other := i.byKey[s.key]; other != nil {
			i.mu.Unlock()
			i.log.Error(errors.New("duplicate session"), "owned iSCSI session duplicates a known session; not adopting",
				"sid", ss.SID, "knownSID", other.sid, "target", ss.TargetName)
			continue
		}
		s.registered = true
		i.byKey[s.key] = s
		i.bySID[s.sid] = s
		i.mu.Unlock()
		i.log.Info("adopted existing iSCSI session", "sid", ss.SID, "target", ss.TargetName,
			"portal", s.key.portal.String(), "state", ss.State)
	}
	return nil
}

// sessionFromSysfs rebuilds session bookkeeping from sysfs.  The TSIH is
// not exported by the software transport, so the first re-login uses TSIH
// 0 with the (deterministic) ISID, which the target treats as session
// reinstatement.
func sessionFromSysfs(ss *sysfsSession) (*session, error) {
	portal, ok := ss.portal()
	if !ok {
		return nil, fmt.Errorf("session %d has no persistent portal in sysfs", ss.SID)
	}
	if len(ss.Conns) == 0 {
		return nil, fmt.Errorf("session %d has no connection in sysfs", ss.SID)
	}
	p := SessionParams{
		InitiatorIQN: ss.InitiatorName,
		TargetIQN:    ss.TargetName,
		Portal:       portal,
	}
	// sysfs reports -1 for an attribute that was never set; 0 is an
	// explicit value (no recovery wait / NOP-Out disabled).
	if ss.RecoveryTmo >= 0 {
		d := time.Duration(ss.RecoveryTmo) * time.Second
		p.ReplacementTimeout = &d
	}
	c := ss.Conns[0]
	if c.RecvTmo >= 0 {
		d := time.Duration(c.RecvTmo) * time.Second
		p.NoopOutInterval = &d
	}
	if c.PingTmo >= 0 {
		d := time.Duration(c.PingTmo) * time.Second
		p.NoopOutTimeout = &d
	}
	p = p.withDefaults()
	s := newSession(sessionKey{target: ss.TargetName, portal: portal}, p)
	//nolint:gosec // G115: sysfs ids are the kernel's uint32 sid, cid and host_no.
	s.sid, s.cid, s.hostNo = uint32(ss.SID), uint32(c.CID), uint32(ss.HostNo)
	s.phase = phaseEstablished
	// Unknown kernel conn state: always STOP_CONN(RECOVER) before re-binding.
	s.needStop = true
	return s, nil
}
