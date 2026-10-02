//go:build linux

package nfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func procAddress(ip netip.Addr) string {
	bytes := ip.AsSlice()
	var result strings.Builder
	for i := 0; i < len(bytes); i += 4 {
		fmt.Fprintf(&result, "%08X", binary.NativeEndian.Uint32(bytes[i:i+4]))
	}
	return result.String()
}

func TestListenerOwnershipDetectsForeignAddressesAndTransports(t *testing.T) {
	t.Parallel()
	ipv4, ipv6 := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")
	data := " sl local_address rem_address st\n" +
		"0: " + procAddress(ipv4) + ":0801 00000000:0000 0A\n" +
		"1: " + procAddress(ipv6) + ":0801 00000000:0000 0A\n" +
		"2: " + procAddress(ipv4) + ":0801 00000000:0000 01\n" +
		"3: 00000000:0016 00000000:0000 0A\n"
	listeners, err := parseListeners(data, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listeners, []netip.Addr{ipv4, ipv6}) {
		t.Fatalf("foreign/IPv6 listener detection=%v", listeners)
	}
	if _, err := parseListeners("0: 00000000:0801 00000000:0000 07", true); err == nil {
		t.Fatal("UDP server was silently adopted")
	}
	if _, err := parseListeners("0: INVALID:0801 00000000:0000 0A", false); err == nil {
		t.Fatal("unprovable listener address accepted")
	}
}

func TestKernelUUIDIdentitySurvivesExportCacheRendering(t *testing.T) {
	t.Parallel()
	e := Export{VolumeID: "pool/volume"}
	uuid := strings.ReplaceAll(FSID(e.VolumeID), "-", "")
	kernel := uuid[:8] + ":" + uuid[8:16] + ":" + uuid[16:24] + ":" + uuid[24:]
	if !kernelIdentityMatches(e, "rw,uuid="+kernel+",sec=1") {
		t.Fatal("owned UUID export would be rejected after restart")
	}
	if kernelIdentityMatches(e, "rw,uuid=00000000:00000000:00000000:00000000,sec=1") {
		t.Fatal("foreign UUID adopted")
	}
	if kernelIdentityMatches(e, "rw,fsid=0,sec=1") {
		t.Fatal("child dataset confused with pseudoroot")
	}
	root := Export{VolumeID: rootVolumeID}
	if !kernelIdentityMatches(root, "ro,fsid=0,sec=1") || kernelIdentityMatches(root, "ro,fsid=37,sec=1") {
		t.Fatal("pseudoroot ownership mismatch")
	}
}

func TestAdmissionTableParsingRefusesUnprovableOwnership(t *testing.T) {
	t.Parallel()
	data := "# Path Client(Flags) # IPs\n" +
		"/data 192.0.2.20(ro,root_squash,fsid=1,sec=sys)\n" +
		"/data [2001:db8::20](rw,fsid=2)\n"
	rows, err := parseEtab(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{
		{"/data", "192.0.2.20", "ro,root_squash,fsid=1,sec=sys"},
		{"/data", "2001:db8::20", "rw,fsid=2"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("admission policy decoding=%#v", rows)
	}
	for _, data := range []string{
		"/data client",
		"/data (rw)",
		"/data client(rw",
		"/data client(rw) extra",
	} {
		if _, err := parseEtab(data); err == nil {
			t.Fatalf("unprovable admission table accepted: %q", data)
		}
	}
}

func TestHelperHealthRejectsExitBeforeWaitNotification(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if killErr := cmd.Process.Kill(); killErr != nil &&
			!errors.Is(killErr, os.ErrProcessDone) {
			t.Errorf("kill helper during cleanup: %v", killErr)
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			t.Logf("wait for killed helper during cleanup: %v", waitErr)
		}
	})
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	r := &kernelRuntime{name: "rpc.mountd", cmd: cmd, done: make(chan struct{}), startTime: start}
	if err = r.processHealth(); err != nil {
		t.Fatalf("live supervised helper: %v", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, _, readErr := processInfo(cmd.Process.Pid)
		if readErr != nil || state == "Z" || state == "X" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminated helper did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	// done deliberately remains open: the Go Wait goroutine may not have
	// run yet, but an exited mountd must already be reported unhealthy.
	if err = r.processHealth(); err == nil {
		t.Fatal("exited helper advertised healthy before Wait notification")
	}
}

func TestWaitOwnedDaemonExitPropagatesReadFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		reads []error
		want  error
	}{
		{name: "first read", reads: []error{syscall.EACCES}, want: syscall.EACCES},
		{
			name:  "stable identity read",
			reads: []error{nil, syscall.EIO},
			want:  syscall.EIO,
		},
		{
			name:  "final verification read",
			reads: []error{nil, nil, syscall.EPERM},
			want:  syscall.EPERM,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			readIndex := 0
			readStart := func(int) (string, error) {
				if readIndex >= len(test.reads) {
					return "start", nil
				}
				err := test.reads[readIndex]
				readIndex++
				return "start", err
			}
			nowIndex := 0
			now := func() time.Time {
				nowIndex++
				if test.name == "final verification read" && nowIndex > 3 {
					return time.Now().Add(6 * time.Second)
				}
				return time.Now()
			}
			err := waitOwnedDaemonExit("rpc.mountd", 123, "start", readStart, now)
			if !errors.Is(err, test.want) {
				t.Fatalf("wait helper error = %v, want %v", err, test.want)
			}
		})
	}
}
