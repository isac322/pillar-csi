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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
)

// kernelEcho builds the reply iscsi_if_rx() sends for one request datagram:
// the request's own iscsi_uevent (union u, including the tag, untouched)
// with type/iferror rewritten on failure, in an nlmsg with seq 0.
func kernelEcho(t *testing.T, msg []byte, errno int32) (typ uint16, payload []byte) {
	t.Helper()
	msgs, err := parseNlmsgs(msg)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parse request datagram: %v (%d messages)", err, len(msgs))
	}
	req, err := unmarshalUevent(msgs[0].Data)
	if err != nil {
		t.Fatalf("decode request: %v", err)
	}
	rep := *req
	rep.Payload = nil
	if errno != 0 {
		rep = *errReply(req, errno)
	}
	return msgs[0].Type, rep.marshal()
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) add(prefix, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, prefix+args)
}

func (s *logSink) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// A reply of an abandoned request must not answer the next request of the
// same type, whether it is read before or during the next round trip.
func TestReplyRouterDropsStaleReply(t *testing.T) {
	sink := &logSink{}
	r := &replyRouter{
		log:     funcr.New(sink.add, funcr.Options{Verbosity: 1}),
		timeout: 5 * time.Second,
		done:    make(chan struct{}),
	}
	req := newStopConnReq(0xfeed, 3, 0, stopConnTerm)

	// Request 1 is abandoned after the kernel queued its (failure) reply,
	// but before the reader delivered it.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var abandoned []byte
	_, err := r.roundTrip(canceled, req, func(msg []byte) error {
		abandoned = msg
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned request: got %v, want context.Canceled", err)
	}
	staleTyp, stale := kernelEcho(t, abandoned, linuxEINVAL)

	// The reader delivers the stale reply between requests...
	r.deliver(staleTyp, stale)

	// ...and again queued ahead of request 2's own reply.
	rep, err := r.roundTrip(context.Background(), req, func(msg []byte) error {
		r.deliver(staleTyp, stale)
		r.deliver(kernelEcho(t, msg, 0))
		return nil
	})
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	err = rep.replyErr(uEventStopConn, false)
	if err != nil {
		t.Fatalf("request 2 got the stale reply of request 1: %v", err)
	}
	if n := sink.count("discarding stale NETLINK_ISCSI reply"); n != 2 {
		t.Fatalf("stale replies logged %d times, want 2; log: %q", n, sink.lines)
	}
}
