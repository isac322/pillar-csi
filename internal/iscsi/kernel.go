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
	"crypto/sha256"
	"fmt"
)

// eventsLost is a synthetic kernelEvent type reported when the netlink
// receive buffer overflowed and multicast events may have been dropped.
const eventsLost = 0

// kernelConn is the NETLINK_ISCSI channel.  The Linux implementation is a
// netlink socket; tests inject a fake kernel.
type kernelConn interface {
	// request sends one iscsi_uevent and returns the kernel's reply.
	// Requests are serialized by the implementation.
	request(ctx context.Context, req *uevent) (*uevent, error)
	// events delivers asynchronous kernel events.  Closed by close.
	events() <-chan kernelEvent
	close() error
}

// kops issues typed NETLINK_ISCSI requests for one transport.
type kops struct {
	k      kernelConn
	handle uint64
}

func (o kops) do(ctx context.Context, req *uevent, checkRetcode bool) (*uevent, error) {
	rep, err := o.k.request(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("send ISCSI_UEVENT_%s: %w", uEventName(req.Type), err)
	}
	err = rep.replyErr(req.Type, checkRetcode)
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// Software iSCSI session sizing, matching open-iscsi defaults
// (node.session.cmds_max, node.session.queue_depth).
const (
	sessionCmdsMax    = 128
	sessionQueueDepth = 32
)

func (o kops) createSession(ctx context.Context) (sid, hostNo uint32, err error) {
	rep, err := o.do(ctx, newCreateSessionReq(o.handle, 0, sessionCmdsMax, sessionQueueDepth), false)
	if err != nil {
		return 0, 0, err
	}
	// r.c_session_ret {u32 sid; u32 host_no}
	return getU32(rep.R[:], 0), getU32(rep.R[:], 4), nil
}

func (o kops) createConn(ctx context.Context, sid, cid uint32) (uint32, error) {
	rep, err := o.do(ctx, newSidCidReq(uEventCreateConn, o.handle, sid, cid), false)
	if err != nil {
		return 0, err
	}
	// r.c_conn_ret {u32 sid; u32 cid}
	return getU32(rep.R[:], 4), nil
}

func (o kops) bindConn(ctx context.Context, sid, cid uint32, fd uintptr, leading bool) error {
	_, err := o.do(ctx, newBindConnReq(o.handle, sid, cid, uint64(fd), leading), true)
	return err
}

func (o kops) sendPDU(ctx context.Context, sid, cid uint32, hdr, data []byte) error {
	_, err := o.do(ctx, newSendPDUReq(o.handle, sid, cid, hdr, data), true)
	return err
}

func (o kops) setParam(ctx context.Context, sid, cid uint32, p iscsiParam, v string) error {
	_, err := o.do(ctx, newSetParamReq(o.handle, sid, cid, p, v), false)
	if err != nil {
		return fmt.Errorf("set %s=%q on session %d: %w", p, v, sid, err)
	}
	return nil
}

func (o kops) startConn(ctx context.Context, sid, cid uint32) error {
	_, err := o.do(ctx, newSidCidReq(uEventStartConn, o.handle, sid, cid), true)
	return err
}

func (o kops) stopConn(ctx context.Context, sid, cid, flag uint32) error {
	_, err := o.do(ctx, newStopConnReq(o.handle, sid, cid, flag), false)
	return err
}

func (o kops) destroyConn(ctx context.Context, sid, cid uint32) error {
	_, err := o.do(ctx, newSidCidReq(uEventDestroyConn, o.handle, sid, cid), false)
	return err
}

func (o kops) destroySession(ctx context.Context, sid uint32) error {
	_, err := o.do(ctx, newSidReq(uEventDestroySession, o.handle, sid), false)
	return err
}

// endpoint is a connected TCP socket whose descriptor is handed to the
// kernel with BIND_CONN.  The descriptor must stay open until the
// connection is stopped.
type endpoint interface {
	fd() uintptr
	Close() error
}

type dialFunc func(ctx context.Context, p Portal) (endpoint, error)

// deriveISID returns a stable ISID for (initiator, target, portal) in the
// RFC 7143 "random" format (T=10b, A=0, B/C random, D qualifier).  Stability
// across process restarts lets a re-login reinstate the target-side session
// instead of leaving a stale one behind.
func deriveISID(initiator, target string, portal Portal) isid {
	h := sha256.Sum256([]byte(initiator + "\x00" + target + "\x00" + portal.normalized().String()))
	var id isid
	id[0] = 0x80
	copy(id[1:], h[:5])
	return id
}

// devNodes ensures block device nodes exist and are usable.  Linux uses
// mknod and open(2); tests use a fake.
type devNodes interface {
	// ensure makes path a block device node with the given number.  created
	// reports whether a node was (re)created.
	ensure(path string, major, minor uint32) (created bool, err error)
	// usable reports whether the device behind the node at path can be
	// opened and has a non-zero capacity.  The kernel publishes a disk's
	// sysfs entry (and its dev attribute) before the disk can be opened, so
	// a missing device (ENXIO, ENODEV, ENOMEDIUM) or a zero capacity is
	// reported through missing rather than as an error: the caller keeps
	// waiting.  Any other failure is an error.
	usable(path string) (missing string, err error)
	// flush writes back the page cache of the device behind the node at
	// path and flushes the device's volatile write cache.  A device that
	// cannot be opened (ENXIO, ENODEV, ENOMEDIUM) is reported through
	// missing; any other failure is an error.
	flush(path string) (missing string, err error)
}
