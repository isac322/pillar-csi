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

// A fake of the kernel side of NETLINK_ISCSI plus the sysfs it maintains,
// modeled on drivers/scsi/scsi_transport_iscsi.c and libiscsi semantics
// (ENOSYS for unsupported params, EBUSY when destroying a session with
// connections, BIND requires a stopped connection, SEND_PDU requires a bound
// one).  Requests go through marshal/unmarshal so the wire encoding is
// exercised end to end.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeConn struct {
	state string // down, bound, up, failed
}

type fakeKSession struct {
	hostNo uint32
	conns  map[uint32]*fakeConn
	params map[iscsiParam]string
}

type fakeKernel struct {
	t      *testing.T
	root   string
	handle uint64
	target *fakeTarget

	mu       sync.Mutex
	evCh     chan kernelEvent
	nextSID  uint32
	sessions map[uint32]*fakeKSession
	calls    []string
	closed   bool
	// hook may fail a request before it is applied.
	hook func(ev *uevent) error

	// replies correlates requests and replies exactly like netlinkConn.
	replies replyRouter
	done    chan struct{}
}

func newFakeKernel(t *testing.T, root string, tgt *fakeTarget) *fakeKernel {
	t.Helper()
	done := make(chan struct{})
	k := &fakeKernel{
		t: t, root: root, handle: 0xfeed, target: tgt,
		evCh: make(chan kernelEvent, 64), nextSID: 1, sessions: map[uint32]*fakeKSession{},
		replies: replyRouter{timeout: 5 * time.Second, done: done}, done: done,
	}
	writeFile(t, filepath.Join(root, "class", "iscsi_transport", "tcp", "handle"), "65261\n")
	return k
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The sysfs helpers below are shared with adoption tests.

func mkSysfsSession(t *testing.T, root string, sid, host uint32, target, initiator, state string) {
	t.Helper()
	dev := filepath.Join(root, "devices", "platform", fmt.Sprintf("host%d", host), fmt.Sprintf("session%d", sid))
	if err := os.MkdirAll(dev, 0o750); err != nil {
		t.Fatal(err)
	}
	cls := filepath.Join(root, "class", "iscsi_session", fmt.Sprintf("session%d", sid))
	writeFile(t, filepath.Join(cls, "targetname"), target+"\n")
	writeFile(t, filepath.Join(cls, "initiatorname"), initiator+"\n")
	writeFile(t, filepath.Join(cls, "state"), state+"\n")
	writeFile(t, filepath.Join(cls, "tpgt"), "1\n")
	writeFile(t, filepath.Join(cls, "recovery_tmo"), "120\n")
	if err := os.Symlink(dev, filepath.Join(cls, "device")); err != nil {
		t.Fatal(err)
	}
}

func mkSysfsConn(t *testing.T, root string, sid, cid uint32, state, addr string, port int) {
	t.Helper()
	d := filepath.Join(root, "class", "iscsi_connection", fmt.Sprintf("connection%d:%d", sid, cid))
	writeFile(t, filepath.Join(d, "state"), state+"\n")
	writeFile(t, filepath.Join(d, "persistent_address"), addr+"\n")
	p := ""
	if port != 0 {
		p = strconv.Itoa(port)
	}
	writeFile(t, filepath.Join(d, "persistent_port"), p+"\n")
	writeFile(t, filepath.Join(d, "ping_tmo"), "5\n")
	writeFile(t, filepath.Join(d, "recv_tmo"), "5\n")
}

// mkSysfsLUN adds a SCSI device (and optionally its block device) below a
// session.
func mkSysfsLUN(t *testing.T, root string, sid, host uint32, lun int, block, dev string) string {
	t.Helper()
	hctl := fmt.Sprintf("%d:0:0:%d", host, lun)
	sdev := filepath.Join(root, "devices", "platform", fmt.Sprintf("host%d", host), fmt.Sprintf("session%d", sid),
		fmt.Sprintf("target%d:0:0", host), hctl)
	if err := os.MkdirAll(sdev, 0o750); err != nil {
		t.Fatal(err)
	}
	if block != "" {
		writeFile(t, filepath.Join(sdev, "block", block, "dev"), dev+"\n")
	}
	writeFile(t, filepath.Join(root, "class", "scsi_device", hctl, "device", "state"), "running\n")
	writeFile(t, filepath.Join(root, "class", "scsi_device", hctl, "device", "rescan"), "")
	writeFile(t, filepath.Join(root, "class", "scsi_device", hctl, "device", "delete"), "")
	return hctl
}

// watchDeletes emulates the SCSI midlayer's "delete" attribute: a device
// whose delete file was written "1" is recorded as "DELETE <H:C:T:L>" and
// removed from sysfs.  It runs until the test ends.
func (k *fakeKernel) watchDeletes() {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	k.t.Cleanup(func() { close(stop); <-stopped })
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if err := k.applyDeletes(); err != nil {
				k.t.Errorf("emulate SCSI device delete: %v", err)
				return
			}
		}
	}()
}

func (k *fakeKernel) applyDeletes() error {
	files, err := filepath.Glob(filepath.Join(k.root, "class", "scsi_device", "*", "device", "delete"))
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // test path
		if err != nil || strings.TrimSpace(string(b)) != "1" {
			continue // not requested, or a test made the attribute unwritable
		}
		hctl := filepath.Base(filepath.Dir(filepath.Dir(f)))
		k.mu.Lock()
		k.record("DELETE %s", hctl)
		k.mu.Unlock()
		dirs, err := filepath.Glob(filepath.Join(k.root, "devices", "platform", "host*", "session*", "target*", hctl))
		if err != nil {
			return err
		}
		for _, d := range append(dirs, filepath.Join(k.root, "class", "scsi_device", hctl)) {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
		}
	}
	return nil
}

func (k *fakeKernel) sessionPath(sid uint32, f string) string {
	return filepath.Join(k.root, "class", "iscsi_session", fmt.Sprintf("session%d", sid), f)
}

func (k *fakeKernel) connPath(sid, cid uint32, f string) string {
	return filepath.Join(k.root, "class", "iscsi_connection", fmt.Sprintf("connection%d:%d", sid, cid), f)
}

func (k *fakeKernel) setConnState(sid, cid uint32, st string) {
	k.sessions[sid].conns[cid].state = st
	writeFile(k.t, k.connPath(sid, cid, "state"), st+"\n")
}

func (k *fakeKernel) record(format string, args ...any) {
	k.calls = append(k.calls, fmt.Sprintf(format, args...))
}

func (k *fakeKernel) callLog() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.calls...)
}

func (k *fakeKernel) events() <-chan kernelEvent { return k.evCh }

func (k *fakeKernel) close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.closed {
		k.closed = true
		close(k.evCh)
		close(k.done)
	}
	return nil
}

func (k *fakeKernel) emit(ev kernelEvent) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.closed {
		k.evCh <- ev
	}
}

// adoptExisting registers a pre-existing kernel session (created by a
// previous process) in the fake kernel and sysfs.
func (k *fakeKernel) adoptExisting(sid, host uint32, target, initiator, sessState, connState string, portal Portal) {
	k.mu.Lock()
	defer k.mu.Unlock()
	mkSysfsSession(k.t, k.root, sid, host, target, initiator, sessState)
	mkSysfsConn(k.t, k.root, sid, 0, connState, portal.Address, portal.Port)
	k.sessions[sid] = &fakeKSession{
		hostNo: host, conns: map[uint32]*fakeConn{0: {state: connState}}, params: map[iscsiParam]string{},
	}
	if sid >= k.nextSID {
		k.nextSID = sid + 1
	}
}

// errReply is the kernel's ISCSI_KEVENT_IF_ERROR reply: iscsi_if_rx()
// rewrites type and iferror of the request's own iscsi_uevent, so union u
// (and the request tag in it) is echoed.
func errReply(req *uevent, errno int32) *uevent {
	rep := *req
	rep.Payload = nil
	rep.Type = kEventIfError
	rep.IfError = -errno
	return &rep
}

func retcodeReply(req *uevent, errno int32) *uevent {
	rep := *req
	rep.Payload = nil
	putU32(rep.R[:], 0, uint32(-errno)) //nolint:gosec // G115: negative errno as s32 retcode wire bits.
	return &rep
}

func (k *fakeKernel) request(ctx context.Context, req *uevent) (*uevent, error) {
	return k.replies.roundTrip(ctx, req, k.sendmsg)
}

// sendmsg is the kernel side of a NETLINK_ISCSI sendmsg: like iscsi_if_rx()
// it handles the request synchronously and queues the reply, the request's
// own iscsi_uevent with nlmsg_seq 0, before sendmsg returns.
func (k *fakeKernel) sendmsg(msg []byte) error {
	msgs, err := parseNlmsgs(msg)
	if err != nil {
		return err
	}
	if len(msgs) != 1 {
		return fmt.Errorf("fake kernel: %d netlink messages in one datagram", len(msgs))
	}
	req, err := unmarshalUevent(msgs[0].Data)
	if err != nil {
		return err
	}
	rep := k.handleReq(req)
	k.replies.deliver(msgs[0].Type, rep.marshal())
	return nil
}

func (k *fakeKernel) handleReq(req *uevent) *uevent {
	k.mu.Lock()
	defer k.mu.Unlock()
	if req.TransportHandle != k.handle {
		return errReply(req, linuxEINVAL)
	}
	if k.hook != nil {
		hookErr := k.hook(req)
		if hookErr != nil {
			k.record("%s FAILED", uEventName(req.Type))
			return errReply(req, linuxEINVAL)
		}
	}
	rep := *req
	rep.Payload = nil
	failure := k.apply(req, &rep)
	if failure != nil {
		return failure
	}
	return &rep
}

// fakeReq is one decoded request and the kernel objects it addresses.
type fakeReq struct {
	req, rep *uevent
	sid, cid uint32
	s        *fakeKSession
	c        *fakeConn
}

// apply executes a request, filling rep.  It returns the failure reply, or
// nil on success.
func (k *fakeKernel) apply(req, rep *uevent) *uevent {
	r := fakeReq{req: req, rep: rep, sid: getU32(req.U[:], 0), cid: getU32(req.U[:], 4)}
	r.s = k.sessions[r.sid]
	if r.s != nil {
		r.c = r.s.conns[r.cid]
	}
	switch req.Type {
	case uEventCreateSession:
		k.createSession(r)
		return nil
	case uEventCreateConn:
		return k.createConn(r)
	case uEventBindConn:
		return k.bindConn(r)
	case uEventSendPDU:
		return k.sendPDU(r)
	case uEventSetParam:
		return k.setParam(r)
	case uEventStartConn:
		return k.startConn(r)
	case uEventStopConn:
		return k.stopConn(r)
	case uEventDestroyConn:
		return k.destroyConn(r)
	case uEventDestroySession:
		return k.destroySession(r)
	default:
		return errReply(req, linuxENOSYS)
	}
}

func (k *fakeKernel) createSession(r fakeReq) {
	sid := k.nextSID
	k.nextSID++
	host := sid + 10
	mkSysfsSession(k.t, k.root, sid, host, "", "", "FREE")
	k.sessions[sid] = &fakeKSession{hostNo: host, conns: map[uint32]*fakeConn{}, params: map[iscsiParam]string{}}
	// msg_create_session_ret {u32 sid; u32 host_no}
	putU32(r.rep.R[:], 0, sid)
	putU32(r.rep.R[:], 4, host)
	k.record("CREATE_SESSION -> %d", sid)
}

func (k *fakeKernel) createConn(r fakeReq) *uevent {
	if r.s == nil {
		return errReply(r.req, linuxEINVAL)
	}
	r.s.conns[r.cid] = &fakeConn{state: "down"}
	mkSysfsConn(k.t, k.root, r.sid, r.cid, "down", "", 0)
	putU32(r.rep.R[:], 0, r.sid)
	putU32(r.rep.R[:], 4, r.cid)
	k.record("CREATE_CONN %d:%d", r.sid, r.cid)
	return nil
}

func (k *fakeKernel) bindConn(r fakeReq) *uevent {
	if r.c == nil {
		return retcodeReply(r.req, linuxEINVAL)
	}
	if r.c.state == "bound" || r.c.state == "up" {
		return retcodeReply(r.req, linuxEEXIST)
	}
	k.setConnState(r.sid, r.cid, "bound")
	k.record("BIND_CONN %d:%d fd=%d", r.sid, r.cid, getU32(r.req.U[:], 8))
	return nil
}

func (k *fakeKernel) sendPDU(r fakeReq) *uevent {
	if r.c == nil || (r.c.state != "bound" && r.c.state != "up") {
		return retcodeReply(r.req, linuxENOTCONN)
	}
	hl, dl := getU32(r.req.U[:], 8), getU32(r.req.U[:], 12)
	hdr, data := r.req.Payload[:hl], r.req.Payload[hl:hl+dl]
	rsp, err := k.target.respond(hdr, data)
	if err != nil {
		k.t.Errorf("fake target: %v", err)
		return retcodeReply(r.req, linuxEINVAL)
	}
	k.record("SEND_PDU %d:%d op=0x%02x", r.sid, r.cid, hdr[0]&opcodeMask)
	ev := kernelEvent{Type: kEventRecvPDU, SID: r.sid, CID: r.cid, PDU: rsp}
	go k.emit(ev) // delivered asynchronously like a multicast
	return nil
}

func (k *fakeKernel) setParam(r fakeReq) *uevent {
	if r.c == nil {
		return errReply(r.req, linuxEINVAL)
	}
	p := iscsiParam(getU32(r.req.U[:], 8))
	n := getU32(r.req.U[:], 12)
	val := string(r.req.Payload[:n-1])
	if p != paramSessRecoveryTmo && r.c.state != "bound" && r.c.state != "up" {
		return errReply(r.req, linuxENOTCONN)
	}
	switch p {
	case paramISID, paramIFMarkerEn, paramOFMarkerEn:
		return errReply(r.req, linuxENOSYS)
	case paramTargetName:
		writeFile(k.t, k.sessionPath(r.sid, "targetname"), val+"\n")
	case paramInitiatorName:
		writeFile(k.t, k.sessionPath(r.sid, "initiatorname"), val+"\n")
	case paramSessRecoveryTmo:
		writeFile(k.t, k.sessionPath(r.sid, "recovery_tmo"), val+"\n")
	case paramPersistentAddress:
		writeFile(k.t, k.connPath(r.sid, r.cid, "persistent_address"), val+"\n")
	case paramPersistentPort:
		writeFile(k.t, k.connPath(r.sid, r.cid, "persistent_port"), val+"\n")
	}
	r.s.params[p] = val
	return nil
}

func (k *fakeKernel) startConn(r fakeReq) *uevent {
	if r.c == nil || r.c.state != "bound" {
		return retcodeReply(r.req, linuxENOTCONN)
	}
	k.setConnState(r.sid, r.cid, "up")
	writeFile(k.t, k.sessionPath(r.sid, "state"), "LOGGED_IN\n")
	k.record("START_CONN %d:%d", r.sid, r.cid)
	return nil
}

func (k *fakeKernel) stopConn(r fakeReq) *uevent {
	if r.c == nil {
		return errReply(r.req, linuxEINVAL)
	}
	flag := getU32(r.req.U[:], 16)
	if flag == stopConnRecover {
		k.setConnState(r.sid, r.cid, "failed")
		writeFile(k.t, k.sessionPath(r.sid, "state"), "FAILED\n")
	} else {
		k.setConnState(r.sid, r.cid, "down")
	}
	k.record("STOP_CONN %d:%d flag=%d", r.sid, r.cid, flag)
	return nil
}

func (k *fakeKernel) destroyConn(r fakeReq) *uevent {
	if r.c == nil {
		return errReply(r.req, linuxEINVAL)
	}
	delete(r.s.conns, r.cid)
	dir := filepath.Dir(k.connPath(r.sid, r.cid, "x"))
	err := os.RemoveAll(dir)
	if err != nil {
		k.t.Errorf("remove fake sysfs %s: %v", dir, err)
	}
	k.record("DESTROY_CONN %d:%d", r.sid, r.cid)
	return nil
}

func (k *fakeKernel) destroySession(r fakeReq) *uevent {
	if r.s == nil {
		return errReply(r.req, linuxEINVAL)
	}
	if len(r.s.conns) != 0 {
		return errReply(r.req, linuxEBUSY)
	}
	delete(k.sessions, r.sid)
	dir := filepath.Dir(k.sessionPath(r.sid, "x"))
	err := os.RemoveAll(dir)
	if err != nil {
		k.t.Errorf("remove fake sysfs %s: %v", dir, err)
	}
	k.record("DESTROY_SESSION %d", r.sid)
	return nil
}

// connError simulates the kernel detecting a broken connection.
func (k *fakeKernel) connError(sid, cid uint32) {
	k.mu.Lock()
	k.setConnState(sid, cid, "failed")
	writeFile(k.t, k.sessionPath(sid, "state"), "FAILED\n")
	k.mu.Unlock()
	k.emit(kernelEvent{Type: kEventConnError, SID: sid, CID: cid, Code: iscsiErrBase + 20})
}

// vanish removes session sid and its connections from the kernel and sysfs
// behind the initiator's back (the target or a recovery timeout destroyed
// it), without emitting ISCSI_KEVENT_DESTROY_SESSION.  It returns the host
// number the session had.
func (k *fakeKernel) vanish(sid uint32) uint32 {
	k.mu.Lock()
	defer k.mu.Unlock()
	s := k.sessions[sid]
	if s == nil {
		k.t.Fatalf("fake kernel: no session %d", sid)
	}
	for cid := range s.conns {
		dir := filepath.Dir(k.connPath(sid, cid, "x"))
		err := os.RemoveAll(dir)
		if err != nil {
			k.t.Fatalf("remove fake sysfs %s: %v", dir, err)
		}
	}
	delete(k.sessions, sid)
	dir := filepath.Dir(k.sessionPath(sid, "x"))
	err := os.RemoveAll(dir)
	if err != nil {
		k.t.Fatalf("remove fake sysfs %s: %v", dir, err)
	}
	return s.hostNo
}

type fakeEndpoint struct {
	n      int
	closed *int
	mu     *sync.Mutex
}

func (e fakeEndpoint) fd() uintptr { return uintptr(100 + e.n) }
func (e fakeEndpoint) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	*e.closed++
	return nil
}

type fakeDialer struct {
	mu     sync.Mutex
	dials  []Portal
	fail   int // fail this many upcoming dials
	closed int
}

func (d *fakeDialer) dial(_ context.Context, p Portal) (endpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials = append(d.dials, p)
	if d.fail > 0 {
		d.fail--
		return nil, errors.New("connection refused")
	}
	return fakeEndpoint{n: len(d.dials), closed: &d.closed, mu: &d.mu}, nil
}

type fakeDevNodes struct {
	mu    sync.Mutex
	nodes map[string][2]uint32
	// unopenable is how many more usable probes report the device as not
	// openable yet (the kernel registration window).
	unopenable int
	// usableErr, when set, fails every usable probe.
	usableErr error
	// usableProbes counts usable calls.
	usableProbes int
	// kern records "FLUSH <node>" for every flush, ordered with requests.
	kern *fakeKernel
	// flushErr, when set, fails every flush.
	flushErr error
}

func (f *fakeDevNodes) flush(path string) (string, error) {
	f.mu.Lock()
	flushErr := f.flushErr
	f.mu.Unlock()
	if flushErr != nil {
		return "", flushErr
	}
	f.kern.mu.Lock()
	f.kern.record("FLUSH %s", filepath.Base(path))
	f.kern.mu.Unlock()
	return "", nil
}

func (f *fakeDevNodes) usable(path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usableProbes++
	if f.usableErr != nil {
		return "", f.usableErr
	}
	if f.unopenable > 0 {
		f.unopenable--
		return "block device " + path + " cannot be opened yet", nil
	}
	return "", nil
}

func (f *fakeDevNodes) ensure(path string, major, minor uint32) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodes == nil {
		f.nodes = map[string][2]uint32{}
	}
	if cur, ok := f.nodes[path]; ok && cur == [2]uint32{major, minor} {
		return false, nil
	}
	f.nodes[path] = [2]uint32{major, minor}
	return true, os.WriteFile(path, nil, 0o600)
}

const (
	testPrefix    = "iqn.2026-01.com.bhyoo.pillar-csi:"
	testInitiator = "iqn.2026-01.com.bhyoo.pillar-csi:node.a"
	testTarget    = testPrefix + "pool.vol1"
)

type harness struct {
	t      *testing.T
	root   string
	dev    string
	tgt    *fakeTarget
	kern   *fakeKernel
	dialer *fakeDialer
	nodes  *fakeDevNodes
	ini    *Initiator
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root, dev := t.TempDir(), t.TempDir()
	h := &harness{t: t, root: root, dev: dev, tgt: newFakeTarget(), dialer: &fakeDialer{}, nodes: &fakeDevNodes{}}
	h.kern = newFakeKernel(t, root, h.tgt)
	h.kern.watchDeletes()
	h.nodes.kern = h.kern
	ini, err := newInitiator(Options{
		SysfsRoot:             root,
		DevRoot:               dev,
		OwnedTargetPrefix:     testPrefix,
		InitiatorIQN:          testInitiator,
		RecoveryRetryInterval: 10 * time.Millisecond,
	}, deps{
		openKernel:   func(uint64) (kernelConn, error) { return h.kern, nil },
		dial:         h.dialer.dial,
		devNodes:     h.nodes,
		scanInterval: time.Hour,
		pollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.ini = ini
	return h
}

func (h *harness) start() {
	h.t.Helper()
	if err := h.ini.Start(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		if err := h.ini.Close(); err != nil {
			h.t.Errorf("close initiator: %v", err)
		}
	})
}

func (*harness) params() SessionParams {
	return SessionParams{
		InitiatorIQN: testInitiator, TargetIQN: testTarget, Portal: Portal{Address: "10.0.0.1", Port: 3260},
	}
}

// eventually polls cond for up to 5s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// sessionPhase returns the phase of the testTarget session at the test portal.
func (h *harness) sessionPhase() phase {
	h.ini.mu.Lock()
	defer h.ini.mu.Unlock()
	s := h.ini.byKey[sessionKey{target: testTarget, portal: h.params().Portal}]
	if s == nil {
		return phaseRemoved
	}
	return s.phase
}
