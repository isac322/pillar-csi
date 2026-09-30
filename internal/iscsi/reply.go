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
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// replyTagOffset locates the request tag inside struct iscsi_uevent: the last
// 4 bytes of union u.  They are tail padding in every request message
// (msg_stop_conn, the largest, ends at byte 20 of the 24-byte union), so the
// kernel never interprets them.  The reply iscsi_if_rx() sends is the request's own
// iscsi_uevent after rewriting only type, iferror and union r, so the tag
// comes back verbatim in success and ISCSI_KEVENT_IF_ERROR replies alike.
// The nlmsghdr sequence number cannot correlate replies: iscsi_if_send_reply()
// always sends nlmsg_seq 0.
const replyTagOffset = uEventUnionUOffset + 20

// replyRouter correlates NETLINK_ISCSI requests with their unicast replies.
// Round trips are serialized; each carries a unique non-zero tag, and only a
// reply echoing the in-flight tag is delivered.  Replies of abandoned
// (canceled or timed-out) requests are dropped instead of answering a later
// request.
type replyRouter struct {
	log     logr.Logger
	timeout time.Duration
	done    <-chan struct{} // closed when the socket closes

	reqMu sync.Mutex // serializes round trips

	mu      sync.Mutex
	lastTag uint32
	waitTag uint32      // tag of the in-flight request; 0 when idle
	waitCh  chan []byte // reply slot of the in-flight request
}

// roundTrip sends req through send and waits for the reply carrying its tag.
func (r *replyRouter) roundTrip(ctx context.Context, req *uevent, send func(msg []byte) error) (*uevent, error) {
	r.reqMu.Lock()
	defer r.reqMu.Unlock()
	tag, ch := r.expect()
	defer r.forget()
	payload := req.marshal()
	hostEndian.PutUint32(payload[replyTagOffset:], tag)
	//nolint:gosec // G115: uevent types are small enum iscsi_uevent_e values that fit nlmsg_type.
	err := send(encodeNlmsg(uint16(req.Type), nlmFRequest, tag, 0, payload))
	if err != nil {
		return nil, err
	}
	t := time.NewTimer(r.timeout)
	defer t.Stop()
	for {
		select {
		case b := <-ch:
			rep, err := unmarshalUevent(b)
			if err != nil {
				return nil, err
			}
			if rep.Type != req.Type && rep.Type != kEventIfError {
				r.log.Error(fmt.Errorf("unexpected reply %s", uEventName(rep.Type)), "discarding NETLINK_ISCSI reply",
					"request", uEventName(req.Type), "tag", tag)
				continue
			}
			return rep, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for kernel reply: %w", ctx.Err())
		case <-t.C:
			return nil, errors.New("wait for kernel reply: timed out")
		case <-r.done:
			return nil, errors.New("wait for kernel reply: NETLINK_ISCSI socket closed")
		}
	}
}

// expect registers a new in-flight request.
func (r *replyRouter) expect() (tag uint32, reply <-chan []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastTag++
	if r.lastTag == 0 { // 0 marks "no request"; skip it on wrap-around
		r.lastTag++
	}
	r.waitTag = r.lastTag
	r.waitCh = make(chan []byte, 1)
	return r.waitTag, r.waitCh
}

// forget ends the in-flight request; its reply, if still to come, is stale.
func (r *replyRouter) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waitTag = 0
	r.waitCh = nil
}

// deliver routes one unicast reply (the nlmsg payload of type typ) to the
// in-flight request.  The data is copied.
func (r *replyRouter) deliver(typ uint16, data []byte) {
	if len(data) < uEventLen {
		r.log.Error(fmt.Errorf("reply of %d bytes, need %d", len(data), uEventLen),
			"discarding malformed NETLINK_ISCSI reply", "type", uEventName(uint32(typ)))
		return
	}
	tag := hostEndian.Uint32(data[replyTagOffset:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waitCh == nil || tag != r.waitTag {
		r.log.V(1).Info("discarding stale NETLINK_ISCSI reply of an abandoned request",
			"type", uEventName(uint32(typ)), "tag", tag, "awaiting", r.waitTag)
		return
	}
	select {
	case r.waitCh <- append([]byte(nil), data...):
	default:
		r.log.Error(errors.New("duplicate reply"), "discarding NETLINK_ISCSI reply",
			"type", uEventName(uint32(typ)), "tag", tag)
	}
}
