//go:build linux

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
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

const (
	netlinkISCSI       = 8 // NETLINK_ISCSI
	iscsiNLGroupISCSID = 1 // ISCSI_NL_GRP_ISCSID

	// Socket receive buffer, sized so bursts of events (many sessions
	// failing at once) do not overrun the socket.
	netlinkRcvBuf = 4 << 20
	// Read buffer for one datagram: iscsi_uevent + 48-byte header + a login
	// data segment (<= 8 KiB) with ample margin.
	netlinkReadBuf = 64 << 10
	// Upper bound on a single request.  The kernel handles requests
	// synchronously in sendmsg, so the reply is normally already queued.
	replyTimeout = 60 * time.Second
)

// netlinkConn is a NETLINK_ISCSI socket bound to ISCSI_NL_GRP_ISCSID.  One
// reader goroutine splits unicast replies (nlmsg_type == request type) from
// multicast events (nlmsg_type 0).
type netlinkConn struct {
	f      *os.File
	rc     syscall.RawConn
	handle uint64
	log    logr.Logger

	replies replyRouter
	evCh    chan kernelEvent

	done      chan struct{}
	closeOnce sync.Once
	readerWG  sync.WaitGroup
}

func openNetlink(netnsPath string, handle uint64, log logr.Logger) (*netlinkConn, error) {
	fd, err := netlinkSocketIn(netnsPath)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "netlink-iscsi")
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("NETLINK_ISCSI raw conn: %w", errors.Join(err, f.Close()))
	}
	done := make(chan struct{})
	c := &netlinkConn{
		f:       f,
		rc:      rc,
		handle:  handle,
		log:     log,
		replies: replyRouter{log: log, timeout: replyTimeout, done: done},
		evCh:    make(chan kernelEvent, 1024),
		done:    done,
	}
	c.readerWG.Add(1)
	go c.readLoop()
	return c, nil
}

// netlinkSocketIn creates the socket inside netnsPath (when set).  The
// namespace switch happens on a dedicated, locked OS thread; if switching
// back fails the goroutine exits still locked, so the runtime discards that
// thread instead of reusing it in the wrong namespace.
func netlinkSocketIn(netnsPath string) (int, error) {
	if netnsPath == "" {
		return newNetlinkSocket()
	}
	type result struct {
		fd  int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		fd, err := socketInNetns(netnsPath)
		ch <- result{fd: fd, err: err}
	}()
	r := <-ch
	return r.fd, r.err
}

// socketInNetns creates the socket inside netnsPath on the calling
// goroutine's OS thread.  It must run on a dedicated goroutine: when
// restoring the original namespace fails it returns with the thread still
// locked, so the thread terminates with that goroutine.
func socketInNetns(netnsPath string) (int, error) {
	runtime.LockOSThread()
	orig, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		runtime.UnlockOSThread()
		return -1, fmt.Errorf("open current network namespace: %w", err)
	}
	defer orig.Close()                //nolint:errcheck // read-only namespace handle
	target, err := os.Open(netnsPath) //nolint:gosec // operator-provided namespace path
	if err != nil {
		runtime.UnlockOSThread()
		return -1, fmt.Errorf("open network namespace %s: %w", netnsPath, err)
	}
	defer target.Close() //nolint:errcheck // read-only namespace handle
	err = unix.Setns(int(target.Fd()), unix.CLONE_NEWNET)
	if err != nil {
		runtime.UnlockOSThread()
		return -1, fmt.Errorf("enter network namespace %s: %w", netnsPath, err)
	}
	fd, sockErr := newNetlinkSocket()
	err = unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET)
	if err != nil {
		// Leave the thread locked: it terminates with this goroutine.
		if sockErr == nil {
			err = errors.Join(err, unix.Close(fd))
		}
		return -1, fmt.Errorf("restore network namespace after creating NETLINK_ISCSI socket in %s: %w",
			netnsPath, err)
	}
	runtime.UnlockOSThread()
	if sockErr != nil {
		return fd, fmt.Errorf("in network namespace %s: %w", netnsPath, sockErr)
	}
	return fd, nil
}

func newNetlinkSocket() (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, netlinkISCSI)
	if err != nil {
		if errors.Is(err, unix.EPROTONOSUPPORT) {
			return -1, fmt.Errorf("create NETLINK_ISCSI socket (scsi_transport_iscsi not loaded?): %w", err)
		}
		return -1, fmt.Errorf("create NETLINK_ISCSI socket: %w", err)
	}
	err = unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1 << (iscsiNLGroupISCSID - 1)})
	if err != nil {
		return -1, fmt.Errorf("bind NETLINK_ISCSI socket to multicast group %d: %w",
			iscsiNLGroupISCSID, errors.Join(err, unix.Close(fd)))
	}
	err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, netlinkRcvBuf)
	if err != nil {
		// SO_RCVBUFFORCE needs CAP_NET_ADMIN; fall back to the capped size.
		err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, netlinkRcvBuf)
		if err != nil {
			return -1, fmt.Errorf("set NETLINK_ISCSI receive buffer: %w", errors.Join(err, unix.Close(fd)))
		}
	}
	return fd, nil
}

func (c *netlinkConn) events() <-chan kernelEvent { return c.evCh }

func (c *netlinkConn) close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.f.Close() // unblocks the poller-backed read
		c.readerWG.Wait()
		close(c.evCh)
	})
	if err != nil {
		return fmt.Errorf("close socket: %w", err)
	}
	return nil
}

func (c *netlinkConn) request(ctx context.Context, req *uevent) (*uevent, error) {
	return c.replies.roundTrip(ctx, req, c.send)
}

// send writes one netlink message to the kernel.
func (c *netlinkConn) send(msg []byte) error {
	var sendErr error
	werr := c.rc.Write(func(fd uintptr) bool {
		sendErr = unix.Sendto(int(fd), msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK})
		return !errors.Is(sendErr, unix.EAGAIN)
	})
	if werr != nil {
		return fmt.Errorf("write NETLINK_ISCSI socket: %w", werr)
	}
	if sendErr != nil {
		return fmt.Errorf("sendto NETLINK_ISCSI socket: %w", sendErr)
	}
	return nil
}

// datagram is the metadata of one received netlink datagram.
type datagram struct {
	n     int
	flags int
	from  unix.Sockaddr
}

func (c *netlinkConn) readLoop() {
	defer c.readerWG.Done()
	buf := make([]byte, netlinkReadBuf)
	for {
		d, err := c.recv(buf)
		if c.closed() {
			return
		}
		if errors.Is(err, unix.ENOBUFS) {
			c.emit(kernelEvent{Type: eventsLost})
			continue
		}
		if err != nil {
			c.log.Error(err, "read NETLINK_ISCSI socket")
			select {
			case <-c.done:
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		c.handleDatagram(buf[:d.n], d)
	}
}

// recv reads one datagram into buf.
func (c *netlinkConn) recv(buf []byte) (datagram, error) {
	var (
		d    datagram
		rerr error
	)
	err := c.rc.Read(func(fd uintptr) bool {
		d.n, _, d.flags, d.from, rerr = unix.Recvmsg(int(fd), buf, nil, 0)
		return !errors.Is(rerr, unix.EAGAIN)
	})
	if err == nil {
		err = rerr
	}
	if err != nil {
		return d, fmt.Errorf("recvmsg: %w", err)
	}
	return d, nil
}

func (c *netlinkConn) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// handleDatagram routes the netlink messages of one kernel datagram.
func (c *netlinkConn) handleDatagram(b []byte, d datagram) {
	if sa, ok := d.from.(*unix.SockaddrNetlink); !ok || sa.Pid != 0 {
		return // only the kernel may talk to us
	}
	if d.flags&unix.MSG_TRUNC != 0 {
		c.log.Error(errors.New("truncated datagram"), "NETLINK_ISCSI message exceeded read buffer",
			"size", netlinkReadBuf)
		c.emit(kernelEvent{Type: eventsLost})
		return
	}
	msgs, err := parseNlmsgs(b)
	if err != nil {
		c.log.Error(err, "decode NETLINK_ISCSI datagram")
	}
	for _, m := range msgs {
		c.handle1(m)
	}
}

func (c *netlinkConn) handle1(m nlmsg) {
	switch m.Type {
	case unix.NLMSG_NOOP, unix.NLMSG_DONE, unix.NLMSG_ERROR, unix.NLMSG_OVERRUN:
		return
	case 0:
		ev, err := unmarshalUevent(m.Data)
		if err != nil {
			c.log.Error(err, "decode NETLINK_ISCSI event")
			return
		}
		if ev.TransportHandle != c.handle {
			return // another transport (e.g. iser, offload HBAs)
		}
		ke, ok := decodeKernelEvent(ev)
		if !ok {
			return
		}
		if ke.PDU != nil {
			ke.PDU = append([]byte(nil), ke.PDU...)
		}
		c.emit(ke)
	default:
		c.replies.deliver(m.Type, m.Data)
	}
}

// emit blocks until the dispatcher takes the event (events are not
// dropped); the dispatcher never waits on the socket, so this cannot
// deadlock.
func (c *netlinkConn) emit(ev kernelEvent) {
	select {
	case c.evCh <- ev:
	case <-c.done:
	}
}
