//go:build linux

// Command iscsi-session-cleanup tears down the kernel iSCSI initiator
// sessions whose target name starts with a given prefix.
//
// The Docker E2E harness uses it to return the host to zero state: Kind nodes
// share one host kernel, so iscsi_tcp sessions opened by pillar-node survive
// the deletion of the Kind cluster.  Sessions are torn down the way iscsid
// does it (no userspace tools are installed): over NETLINK_ISCSI, which only
// exists in the host's initial network namespace, send STOP_CONN(term) and
// DESTROY_CONN for every connection, then DESTROY_SESSION.  Every removal is
// read back from sysfs; any session that remains is an error.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// struct iscsi_uevent (include/scsi/iscsi_if.h): type u32, iferror u32,
// transport_handle u64, union u (24 bytes) at 16, union r (16 bytes) at 40.
const (
	netlinkISCSI  = 8
	nlmsgHdrLen   = 16
	uEventLen     = 56
	uEventUOffset = 16

	uEventBase           = 10
	uEventDestroySession = uEventBase + 2
	uEventDestroyConn    = uEventBase + 4
	uEventStopConn       = uEventBase + 8
	kEventIfError        = 100 + 3

	stopConnTerm = 0x1

	replyTimeout = 10 * time.Second
	goneTimeout  = 30 * time.Second
)

// Netlink payloads use the host byte order.
var hostEndian = binary.NativeEndian

type session struct {
	name   string // sessionN
	sid    uint32
	target string
	cids   []uint32
}

func main() {
	netnsPath := flag.String("netns", "", "network namespace file of the host init netns (NETLINK_ISCSI lives there)")
	sysfsRoot := flag.String("sysfs", "/sys", "sysfs root")
	prefix := flag.String("target-prefix", "", "tear down sessions whose targetname starts with this prefix")
	flag.Parse()
	if *netnsPath == "" || *prefix == "" {
		fmt.Fprintln(os.Stderr, "iscsi-session-cleanup: --netns and --target-prefix are required")
		os.Exit(2)
	}
	err := run(*netnsPath, *sysfsRoot, *prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iscsi-session-cleanup: %v\n", err)
		os.Exit(1)
	}
}

func run(netnsPath, sysfsRoot, prefix string) (err error) {
	sessions, err := ownedSessions(sysfsRoot, prefix)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}
	handle, err := transportHandle(sysfsRoot)
	if err != nil {
		return err
	}
	fd, err := netlinkSocketIn(netnsPath)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := unix.Close(fd)
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close NETLINK_ISCSI socket: %w", closeErr))
		}
	}()

	var errs []error
	for _, s := range sessions {
		destroyErr := destroySession(fd, handle, s)
		if destroyErr != nil {
			errs = append(errs, destroyErr)
			continue
		}
		fmt.Printf("destroyed iSCSI %s (sid %d) to target %s\n", s.name, s.sid, s.target)
	}
	err = errors.Join(errs...)
	if err != nil {
		return err
	}
	return waitSessionsGone(sysfsRoot, sessions)
}

// ownedSessions lists sessions whose targetname starts with prefix together
// with their connection IDs.
func ownedSessions(sysfsRoot, prefix string) ([]session, error) {
	sessionRoot := filepath.Join(sysfsRoot, "class", "iscsi_session")
	entries, err := os.ReadDir(sessionRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // scsi_transport_iscsi not loaded: no sessions
	}
	if err != nil {
		return nil, fmt.Errorf("list iSCSI sessions in %s: %w", sessionRoot, err)
	}
	var out []session
	for _, e := range entries {
		name := e.Name()
		sidText, ok := strings.CutPrefix(name, "session")
		if !ok {
			continue
		}
		sid, err := strconv.ParseUint(sidText, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse iSCSI session id of %s: %w", name, err)
		}
		//nolint:gosec // G304: path is constructed from the sysfs root and a kernel-listed session entry.
		raw, err := os.ReadFile(filepath.Join(sessionRoot, name, "targetname"))
		if err != nil {
			return nil, fmt.Errorf("read targetname of iSCSI %s: %w", name, err)
		}
		target := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(target, prefix) {
			continue
		}
		cids, err := connectionIDs(sysfsRoot, uint32(sid))
		if err != nil {
			return nil, err
		}
		out = append(out, session{name: name, sid: uint32(sid), target: target, cids: cids})
	}
	return out, nil
}

// connectionIDs returns the CIDs of /sys/class/iscsi_connection/connection<sid>:<cid>.
func connectionIDs(sysfsRoot string, sid uint32) ([]uint32, error) {
	connRoot := filepath.Join(sysfsRoot, "class", "iscsi_connection")
	entries, err := os.ReadDir(connRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list iSCSI connections in %s: %w", connRoot, err)
	}
	want := fmt.Sprintf("connection%d:", sid)
	var cids []uint32
	for _, e := range entries {
		cidText, ok := strings.CutPrefix(e.Name(), want)
		if !ok {
			continue
		}
		cid, err := strconv.ParseUint(cidText, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse iSCSI connection id of %s: %w", e.Name(), err)
		}
		cids = append(cids, uint32(cid))
	}
	return cids, nil
}

func transportHandle(sysfsRoot string) (uint64, error) {
	path := filepath.Join(sysfsRoot, "class", "iscsi_transport", "tcp", "handle")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from the sysfs root.
	if err != nil {
		return 0, fmt.Errorf("read iSCSI tcp transport handle %s (is iscsi_tcp loaded?): %w", path, err)
	}
	handle, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse iSCSI tcp transport handle %s: %w", path, err)
	}
	return handle, nil
}

// netlinkSocketIn opens a NETLINK_ISCSI socket inside the network namespace
// at netnsPath and returns to the original namespace.
func netlinkSocketIn(netnsPath string) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open current network namespace: %w", err)
	}
	defer unix.Close(orig) //nolint:errcheck // read-only namespace handle
	target, err := unix.Open(netnsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open network namespace %s: %w", netnsPath, err)
	}
	defer unix.Close(target) //nolint:errcheck // read-only namespace handle
	err = unix.Setns(target, unix.CLONE_NEWNET)
	if err != nil {
		return -1, fmt.Errorf("enter network namespace %s: %w", netnsPath, err)
	}
	fd, sockErr := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, netlinkISCSI)
	err = unix.Setns(orig, unix.CLONE_NEWNET)
	if err != nil {
		// The thread stays locked and dies with the process; never keep
		// running in the wrong namespace.
		panic(fmt.Sprintf("return to the original network namespace: %v", err))
	}
	if sockErr != nil {
		return -1, fmt.Errorf("create NETLINK_ISCSI socket in %s: %w", netnsPath, sockErr)
	}
	err = unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK})
	if err != nil {
		return -1, closeOnError(fd, fmt.Errorf("bind NETLINK_ISCSI socket: %w", err))
	}
	tv := unix.NsecToTimeval(replyTimeout.Nanoseconds())
	err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	if err != nil {
		return -1, closeOnError(fd, fmt.Errorf("set NETLINK_ISCSI receive timeout: %w", err))
	}
	return fd, nil
}

// closeOnError closes fd after a setup failure and reports both errors.
func closeOnError(fd int, cause error) error {
	closeErr := unix.Close(fd)
	if closeErr != nil {
		return errors.Join(cause, fmt.Errorf("close NETLINK_ISCSI socket: %w", closeErr))
	}
	return cause
}

func destroySession(fd int, handle uint64, s session) error {
	for _, cid := range s.cids {
		stop := newEvent(uEventStopConn, handle)
		putU32(stop, uEventUOffset, s.sid)
		putU32(stop, uEventUOffset+4, cid)
		putU32(stop, uEventUOffset+16, stopConnTerm)
		err := request(fd, stop)
		if err != nil {
			return fmt.Errorf("stop iSCSI connection %d:%d of target %s: %w", s.sid, cid, s.target, err)
		}
		destroy := newEvent(uEventDestroyConn, handle)
		putU32(destroy, uEventUOffset, s.sid)
		putU32(destroy, uEventUOffset+4, cid)
		err = request(fd, destroy)
		if err != nil {
			return fmt.Errorf("destroy iSCSI connection %d:%d of target %s: %w", s.sid, cid, s.target, err)
		}
	}
	destroy := newEvent(uEventDestroySession, handle)
	putU32(destroy, uEventUOffset, s.sid)
	err := request(fd, destroy)
	if err != nil {
		return fmt.Errorf("destroy iSCSI session %d of target %s: %w", s.sid, s.target, err)
	}
	return nil
}

func newEvent(typ uint32, handle uint64) []byte {
	ev := make([]byte, uEventLen)
	hostEndian.PutUint32(ev[0:], typ)
	hostEndian.PutUint64(ev[8:], handle)
	return ev
}

func putU32(b []byte, off int, v uint32) { hostEndian.PutUint32(b[off:], v) }

// request sends one uevent and waits for the kernel's unicast reply of the
// same type, returning its iferror.
func request(fd int, ev []byte) error {
	typ := hostEndian.Uint32(ev[0:])
	msg := make([]byte, nlmsgHdrLen+len(ev))
	hostEndian.PutUint32(msg[0:], uint32(len(msg))) //nolint:gosec // G115: nlmsg length is nlmsgHdrLen+uEventLen.
	hostEndian.PutUint16(msg[4:], uint16(typ))      //nolint:gosec // G115: nlmsg_type is u16; iSCSI uevent types fit.
	hostEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST)
	copy(msg[nlmsgHdrLen:], ev)
	err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK})
	if err != nil {
		return fmt.Errorf("send uevent %d: %w", typ, err)
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return fmt.Errorf("receive reply to uevent %d: %w", typ, err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return fmt.Errorf("parse reply to uevent %d: %w", typ, err)
		}
		for _, m := range msgs {
			if len(m.Data) < uEventLen {
				continue
			}
			replyType := hostEndian.Uint32(m.Data[0:])
			ifErr := int32(hostEndian.Uint32(m.Data[4:])) //nolint:gosec // G115: kernel ABI: iferror is a signed errno.
			if replyType == kEventIfError || (replyType == typ && ifErr != 0) {
				//nolint:gosec // G115: iferror is a negative errno, so -ifErr is a positive errno.
				return fmt.Errorf("kernel rejected uevent %d: %w", typ, unix.Errno(-ifErr))
			}
			if replyType == typ {
				return nil
			}
		}
	}
}

func waitSessionsGone(sysfsRoot string, sessions []session) error {
	deadline := time.Now().Add(goneTimeout)
	for {
		var remaining []string
		for _, s := range sessions {
			path := filepath.Join(sysfsRoot, "class", "iscsi_session", s.name)
			_, err := os.Lstat(path)
			if err == nil {
				remaining = append(remaining, s.name+"("+s.target+")")
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read back iSCSI %s: %w", s.name, err)
			}
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("iSCSI sessions still present after destroy: %s", strings.Join(remaining, ", "))
		}
		time.Sleep(500 * time.Millisecond)
	}
}
