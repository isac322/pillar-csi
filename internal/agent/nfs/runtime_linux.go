//go:build linux

package nfs

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const cleanupTimeout = 15 * time.Second
const nfsdRoot = "/proc/fs/nfsd"
const pipefsDir = "/var/lib/nfs/rpc_pipefs"
const trackerDir = "/var/lib/nfs/nfsdcld"
const trackerName = "nfsdcld"
const etabPath = "/var/lib/nfs/etab"
const nfsTCPPort = "tcp 2049"

//nolint:misspell // Exact nfs-utils executable name, not the English word "exports".
const exportUtility = "exportfs"
const nfsdMagic = 0x6e667364
const rpcPipefsMagic = 0x67596969

type kernelRuntime struct {
	config        Config
	mu            sync.Mutex
	cmd           *exec.Cmd
	done          chan struct{}
	exit          error
	stopping      bool
	name          string
	startTime     string
	tracker       *kernelRuntime
	serverClaimed bool
	mountedNfsd   bool
	mountedPipefs bool
}

func newRuntime(config Config) runtime { return &kernelRuntime{config: config} }

func acquireLock(path string) (*os.File, error) {
	// Only Manager.Start supplies this path; the configured state directory is not a request parameter.
	//nolint:gosec // G304: private owner.lock path; O_NOFOLLOW rejects symlinks.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open NFS ownership lock %q: %w", path, err)
	}
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		lockErr := fmt.Errorf("NFS ownership already locked: %w", err)
		closeErr := f.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("close NFS ownership lock %q: %w", path, closeErr)
		}
		joined := errors.Join(lockErr, closeErr)
		if joined == nil {
			return nil, errors.New("NFS ownership lock failure without an error")
		}
		return nil, fmt.Errorf("acquire NFS ownership lock: %w", joined)
	}
	return f, nil
}

func releaseLock(f *os.File) error {
	unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if unlockErr != nil {
		unlockErr = fmt.Errorf("unlock NFS ownership lock: %w", unlockErr)
	}
	closeErr := f.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close NFS ownership lock: %w", closeErr)
	}
	joined := errors.Join(unlockErr, closeErr)
	if joined == nil {
		return nil
	}
	return fmt.Errorf("release NFS ownership lock: %w", joined)
}

func (*kernelRuntime) identity() (string, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read kernel boot identity: %w", err)
	}
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return "", fmt.Errorf("read network namespace identity: %w", err)
	}
	return strings.TrimSpace(string(boot)) + "/" + ns, nil
}

func readControl(name string) (string, error) {
	//nolint:gosec // G304: private callers supply literal nfsd control names under the fixed procfs root.
	data, err := os.ReadFile(filepath.Join(nfsdRoot, name))
	if err != nil {
		return "", fmt.Errorf("read NFS kernel control %q: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func writeControl(name, value string) error {
	//nolint:gosec // G304: private callers supply literal nfsd control names under the fixed procfs root.
	f, err := os.OpenFile(filepath.Join(nfsdRoot, name), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open NFS kernel control %q: %w", name, err)
	}
	n, writeErr := f.WriteString(value + "\n")
	if n != len(value)+1 && writeErr == nil {
		writeErr = errors.New("short kernel control write")
	}
	closeErr := f.Close()
	if writeErr != nil {
		writeErr = fmt.Errorf("write NFS kernel control %q: %w", name, writeErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close NFS kernel control %q: %w", name, closeErr)
	}
	joined := errors.Join(writeErr, closeErr)
	if joined == nil {
		return nil
	}
	return fmt.Errorf("write NFS kernel control: %w", joined)
}

func processInfo(pid int) (state, start string, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", fmt.Errorf("read process %d stat: %w", pid, err)
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", "", errors.New("malformed process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", "", errors.New("incomplete process stat")
	}
	return fields[0], fields[19], nil
}

func processStart(pid int) (string, error) {
	_, start, err := processInfo(pid)
	if err != nil {
		return "", fmt.Errorf("read process %d start time: %w", pid, err)
	}
	return start, nil
}

func processGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, unix.ENOENT) ||
		errors.Is(err, unix.ESRCH) ||
		errors.Is(err, os.ErrProcessDone)
}

// checkDaemons requires host PID visibility. No PID or start-time alone is
// sufficient evidence: the helper must also be rpc.mountd in our netns.
func (r *kernelRuntime) checkDaemons(state *diskState, owned bool) error {
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("read agent network namespace: %w", err)
	}
	pids, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("read process directory: %w", err)
	}
	for _, item := range pids {
		err = r.inspectDaemon(state, owned, ns, item)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *kernelRuntime) inspectDaemon(
	state *diskState,
	owned bool,
	ns string,
	item os.DirEntry,
) error {
	pid, err := strconv.Atoi(item.Name())
	if err != nil {
		//nolint:nilerr // Non-PID procfs entries (self, thread-self) are expected, not inspection failures.
		return nil
	}
	procPath := fmt.Sprintf("/proc/%d", pid)
	//nolint:gosec // G304: pid was parsed from a kernel-owned /proc entry; the final component is literal.
	comm, err := os.ReadFile(filepath.Join(procPath, "comm"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect process %d: %w", pid, err)
	}
	name := strings.TrimSpace(string(comm))
	if name != "rpc.mountd" && name != trackerName {
		return nil
	}
	processNS, err := os.Readlink(filepath.Join(procPath, "ns", "net"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s network namespace: %w", name, err)
	}
	if processNS != ns {
		return nil
	}
	start, err := processStart(pid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s process identity: %w", name, err)
	}
	ownerPID, ownerStart := state.MountdPID, state.MountdStart
	if name == trackerName {
		ownerPID, ownerStart = state.TrackerPID, state.TrackerStart
	}
	if !owned || pid != ownerPID || start != ownerStart {
		return fmt.Errorf("foreign %s process %d in agent network namespace", name, pid)
	}
	return r.stopOwnedDaemon(name, pid, start)
}

func (*kernelRuntime) stopOwnedDaemon(name string, pid int, start string) error {
	// pidfd makes PID reuse between inspection and signaling harmless;
	// re-read identity after obtaining the handle before sending a signal.
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if processGone(err) {
			return nil
		}
		return fmt.Errorf("open pidfd for owned %s %d: %w", name, pid, err)
	}
	verified, verifyErr := processStart(pid)
	if verifyErr != nil || verified != start {
		closeErr := unix.Close(fd)
		if closeErr != nil {
			closeErr = fmt.Errorf("close pidfd for owned %s %d: %w", name, pid, closeErr)
		}
		if processGone(verifyErr) {
			return closeErr
		}
		identityErr := errors.New("owned daemon process identity changed")
		if verifyErr != nil {
			identityErr = fmt.Errorf("re-read owned %s identity: %w", name, verifyErr)
		}
		joined := errors.Join(identityErr, closeErr)
		if joined == nil {
			return nil
		}
		return fmt.Errorf("verify owned daemon identity: %w", joined)
	}
	signalErr := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
	if processGone(signalErr) {
		signalErr = nil
	}
	if signalErr != nil {
		signalErr = fmt.Errorf("signal owned %s %d: %w", name, pid, signalErr)
	}
	closeErr := unix.Close(fd)
	if closeErr != nil {
		closeErr = fmt.Errorf("close pidfd for owned %s %d: %w", name, pid, closeErr)
	}
	joined := errors.Join(signalErr, closeErr)
	if joined != nil {
		return fmt.Errorf("signal owned daemon: %w", joined)
	}
	return waitOwnedDaemonExit(name, pid, start, processStart, time.Now)
}

func waitOwnedDaemonExit(
	name string,
	pid int,
	start string,
	readStart func(int) (string, error),
	now func() time.Time,
) error {
	deadline := now().Add(5 * time.Second)
	for now().Before(deadline) {
		current, currentErr := readStart(pid)
		if currentErr != nil {
			if processGone(currentErr) {
				return nil
			}
			return fmt.Errorf("wait for owned %s exit: %w", name, currentErr)
		}
		if current != start {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	current, currentErr := readStart(pid)
	if currentErr != nil {
		if processGone(currentErr) {
			return nil
		}
		return fmt.Errorf("verify owned %s exit: %w", name, currentErr)
	}
	if current == start {
		return fmt.Errorf("owned %s did not exit", name)
	}
	return nil
}

func parseListeners(data string, udp bool) ([]netip.Addr, error) {
	var listeners []netip.Addr
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		address := strings.Split(fields[1], ":")
		if len(address) != 2 || address[1] != "0801" || (!udp && fields[3] != "0A") {
			continue
		}
		if udp {
			return nil, errors.New("foreign UDP NFS listener")
		}
		if len(address[0]) != 8 && len(address[0]) != 32 {
			return nil, errors.New("unrecognized NFS listener address")
		}
		var bytes [16]byte
		length := len(address[0]) / 2
		for i := 0; i < length; i += 4 {
			word, err := strconv.ParseUint(address[0][i*2:i*2+8], 16, 32)
			if err != nil {
				return nil, fmt.Errorf("parse NFS listener address %q: %w", address[0], err)
			}
			binary.NativeEndian.PutUint32(bytes[i:i+4], uint32(word))
		}
		ip, ok := netip.AddrFromSlice(bytes[:length])
		if !ok {
			return nil, errors.New("invalid NFS listener address")
		}
		listeners = append(listeners, ip.Unmap())
	}
	return listeners, nil
}

func nfsListeners() ([]netip.Addr, error) {
	var listeners []netip.Addr
	for _, name := range []string{"tcp", "tcp6", "udp", "udp6"} {
		//nolint:gosec // G304: name is one of the four literal kernel socket tables enumerated above.
		data, err := os.ReadFile("/proc/net/" + name)
		if err != nil {
			return nil, fmt.Errorf("read %s NFS listeners: %w", name, err)
		}
		ips, err := parseListeners(string(data), strings.HasPrefix(name, "udp"))
		if err != nil {
			return nil, fmt.Errorf("parse %s NFS listeners: %w", name, err)
		}
		listeners = append(listeners, ips...)
	}
	return listeners, nil
}

// Kernel cache renders UUID fsids as uuid=xxxxxxxx:xxxxxxxx:xxxxxxxx:xxxxxxxx,
// unlike nfs-utils etab's fsid=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.
func kernelIdentityMatches(e Export, options string) bool {
	if e.VolumeID == rootVolumeID {
		return optionValue(options, "fsid") == "0"
	}
	uuid := optionValue(options, "uuid")
	expected := e.fsid()
	if len(uuid) != 35 || uuid[8] != ':' || uuid[17] != ':' || uuid[26] != ':' {
		return false
	}
	j := 0
	for i := range len(uuid) {
		if uuid[i] == ':' {
			continue
		}
		for expected[j] == '-' {
			j++
		}
		if uuid[i]|0x20 != expected[j]|0x20 {
			return false
		}
		j++
	}
	return j == len(expected)
}

func (r *kernelRuntime) start(
	ctx context.Context,
	state *diskState,
	save func() error,
	failed func(error),
) (retErr error) {
	identity, err := r.identity()
	if err != nil {
		return err
	}
	owned := state.Identity == identity
	defer func() {
		if retErr != nil {
			cleanupErr := r.close()
			if cleanupErr != nil {
				joined := errors.Join(retErr, cleanupErr)
				if joined != nil {
					retErr = fmt.Errorf("NFS startup cleanup: %w", joined)
				}
			}
		}
	}()
	err = r.checkPrerequisites(state, owned)
	if err != nil {
		return err
	}
	threadCount, ports, err := r.prepareKernel(state, owned)
	if err != nil {
		return err
	}
	err = r.preparePaths()
	if err != nil {
		return err
	}
	err = r.verifyExports(ctx, state)
	if err != nil {
		return err
	}
	state.Identity = identity
	state.MountdPID = 0
	state.MountdStart = ""
	state.TrackerPID = 0
	state.TrackerStart = ""
	err = save()
	if err != nil {
		return err
	}
	r.serverClaimed = true
	err = r.configureVersions(threadCount)
	if err != nil {
		return err
	}
	_, err = r.configureListener(ctx, ports)
	if err != nil {
		return err
	}
	err = r.startHelpers(state, save, failed, threadCount)
	if err != nil {
		return err
	}
	return r.health()
}

func (r *kernelRuntime) checkPrerequisites(state *diskState, owned bool) error {
	for _, name := range []string{exportUtility, "rpc.mountd", trackerName} {
		_, err := exec.LookPath(name)
		if err != nil {
			return fmt.Errorf("locate NFS helper %q: %w", name, err)
		}
	}
	return r.checkDaemons(state, owned)
}

func (r *kernelRuntime) prepareKernel(_ *diskState, owned bool) (threadCount int, ports string, prepareErr error) {
	err := r.ensureNfsdMount()
	if err != nil {
		return 0, "", fmt.Errorf("prepare nfsd filesystem: %w", err)
	}
	threads, err := readControl("threads")
	if err != nil {
		return 0, "", fmt.Errorf("read nfsd threads: %w", err)
	}
	threadCount, err = strconv.Atoi(threads)
	if err != nil || threadCount < 0 {
		return 0, "", errors.New("invalid nfsd thread readback")
	}
	ports, err = readControl("portlist")
	if err != nil {
		return 0, "", fmt.Errorf("read nfsd port list: %w", err)
	}
	listeners, err := nfsListeners()
	if err != nil {
		return 0, "", fmt.Errorf("inspect NFS listeners: %w", err)
	}
	err = validateKernelListeners(r.config.BindAddress, owned, ports, threadCount, listeners)
	if err != nil {
		return 0, "", err
	}
	return threadCount, ports, nil
}

func (r *kernelRuntime) ensureNfsdMount() error {
	//nolint:gosec // G301: fixed procfs mountpoint needs kernel traversal, not private-state permissions.
	err := os.MkdirAll(nfsdRoot, 0o755)
	if err != nil {
		return fmt.Errorf("create nfsd mountpoint: %w", err)
	}
	var stat unix.Statfs_t
	err = unix.Statfs(nfsdRoot, &stat)
	if err != nil {
		return fmt.Errorf("inspect nfsd filesystem: %w", err)
	}
	if stat.Type == nfsdMagic {
		return nil
	}
	err = unix.Mount("nfsd", nfsdRoot, "nfsd", 0, "")
	if err != nil {
		return fmt.Errorf("mount nfsd control filesystem: %w", err)
	}
	r.mountedNfsd = true
	err = unix.Statfs(nfsdRoot, &stat)
	if err != nil {
		return fmt.Errorf("read back nfsd filesystem: %w", err)
	}
	if stat.Type != nfsdMagic {
		return errors.New("nfsd filesystem mount readback failed")
	}
	return nil
}

func validateKernelListeners(
	bindAddress string,
	owned bool,
	ports string,
	threadCount int,
	listeners []netip.Addr,
) error {
	if !owned && (threadCount != 0 || ports != "" || len(listeners) != 0) {
		return errors.New("foreign NFS listener or kernel nfsd is already active")
	}
	if owned && len(listeners) != 0 {
		bind, err := netip.ParseAddr(bindAddress)
		if err != nil {
			return fmt.Errorf("parse configured NFS bind address: %w", err)
		}
		if ports == "" || len(listeners) != 1 || listeners[0] != bind {
			return errors.New("foreign listener or changed owned NFS bind address")
		}
	}
	for line := range strings.SplitSeq(ports, "\n") {
		if line != "" && line != nfsTCPPort {
			return fmt.Errorf("foreign nfsd listener configuration %q", line)
		}
	}
	return nil
}

func (r *kernelRuntime) verifyExports(ctx context.Context, state *diskState) error {
	rows, err := r.list(ctx)
	if err != nil {
		return fmt.Errorf("read export admission table: %w", err)
	}
	err = verifyLedgerRows(rows, state.Ledger, false)
	if err != nil {
		return err
	}
	cacheData, err := os.ReadFile(filepath.Join(nfsdRoot, "exports"))
	if err != nil {
		return fmt.Errorf("read kernel export cache: %w", err)
	}
	cacheRows, err := parseEtab(string(cacheData))
	if err != nil {
		return fmt.Errorf("cannot establish kernel export ownership: %w", err)
	}
	return verifyLedgerRows(cacheRows, state.Ledger, true)
}

func verifyLedgerRows(rows []entry, ledger []Export, kernel bool) error {
	for _, row := range rows {
		known := false
		for i := range ledger {
			e := &ledger[i]
			identityMatches := optionValue(row.Options, "fsid") == e.fsid()
			if kernel {
				identityMatches = kernelIdentityMatches(*e, row.Options)
			}
			if row.Path == e.Path && identityMatches && slices.Contains(e.clients(), row.Client) {
				known = true
				break
			}
		}
		if !known {
			if kernel {
				return fmt.Errorf("foreign kernel NFS export at %q", row.Path)
			}
			return fmt.Errorf("foreign NFS export at %q; refusing server ownership", row.Path)
		}
	}
	return nil
}

func (r *kernelRuntime) preparePaths() error {
	// Export admissions enforce policy; private recovery state lives in the separate state directory.
	//nolint:gosec // G301: NFS clients need traversal through the pseudoroot.
	err := os.MkdirAll(r.config.ExportRoot, 0o755)
	if err != nil {
		return fmt.Errorf("create NFS pseudoroot: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(r.config.ExportRoot)
	if err != nil {
		return fmt.Errorf("resolve NFS pseudoroot: %w", err)
	}
	if canonical != r.config.ExportRoot {
		return errors.New("NFS pseudoroot must not traverse symlinks")
	}
	err = r.validateExport(Export{VolumeID: rootVolumeID, Path: r.config.ExportRoot})
	if err != nil {
		return fmt.Errorf("validate NFS pseudoroot: %w", err)
	}
	for _, dir := range []string{pipefsDir, trackerDir} {
		err = os.MkdirAll(dir, 0o700)
		if err != nil {
			return fmt.Errorf("create NFS tracking directory %q: %w", dir, err)
		}
	}
	err = ensurePrivateEtab(etabPath)
	if err != nil {
		return err
	}
	var stat unix.Statfs_t
	err = unix.Statfs(pipefsDir, &stat)
	if err != nil {
		return fmt.Errorf("inspect NFS tracking pipefs: %w", err)
	}
	if stat.Type == rpcPipefsMagic {
		return nil
	}
	err = unix.Mount("sunrpc", pipefsDir, "rpc_pipefs", 0, "")
	if err != nil {
		return fmt.Errorf("mount private NFS tracking pipefs: %w", err)
	}
	r.mountedPipefs = true
	err = unix.Mount("", pipefsDir, "", unix.MS_PRIVATE|unix.MS_REC, "")
	if err != nil {
		return fmt.Errorf("isolate tracking pipefs propagation: %w", err)
	}
	err = unix.Statfs(pipefsDir, &stat)
	if err != nil {
		return fmt.Errorf("read back NFS tracking pipefs: %w", err)
	}
	if stat.Type != rpcPipefsMagic {
		return errors.New("NFS tracking pipefs mount readback failed")
	}
	return nil
}

func (*kernelRuntime) configureVersions(threadCount int) error {
	if threadCount == 0 {
		versionsBefore, err := readControl("versions")
		if err != nil {
			return err
		}
		flags := []string{"+4", "+4.2", "-4.0"}
		for _, version := range []string{"2", "3", "4.1"} {
			enabled := slices.Contains(strings.Fields(versionsBefore), "+"+version)
			disabled := slices.Contains(strings.Fields(versionsBefore), "-"+version)
			if enabled || disabled {
				flags = append(flags, "-"+version)
			}
		}
		err = writeControl("versions", strings.Join(flags, " "))
		if err != nil {
			return fmt.Errorf("configure NFSv4.2: %w", err)
		}
	}
	versions, err := readControl("versions")
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(versions), "+4.2") {
		return fmt.Errorf("kernel NFS version readback %q lacks +4.2", versions)
	}
	if !slices.Contains(strings.Fields(versions), "-4.0") {
		return fmt.Errorf("kernel NFSv4.0 remains enabled: %q", versions)
	}
	for _, unsupported := range []string{"+2", "+3", "+4.0", "+4.1"} {
		if slices.Contains(strings.Fields(versions), unsupported) {
			return fmt.Errorf("kernel NFS version readback %q enables unsupported %s", versions, unsupported)
		}
	}
	return nil
}

func (r *kernelRuntime) configureListener(ctx context.Context, ports string) (string, error) {
	if ports != "" {
		return ports, nil
	}
	family := "tcp4"
	if strings.Contains(r.config.BindAddress, ":") {
		family = "tcp6"
	}
	// Linux nfsd accepts only TCP/UDP listener sockets; Go enables MPTCP
	// listeners by default when supported, and nfsd rejects protocol 262.
	var listenConfig net.ListenConfig
	listenConfig.SetMultipathTCP(false)
	listener, err := listenConfig.Listen(ctx, family, net.JoinHostPort(r.config.BindAddress, "2049"))
	if err != nil {
		return "", fmt.Errorf("NFS listener unavailable: %w", err)
	}
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		closeErr := listener.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("close non-TCP NFS listener: %w", closeErr)
		}
		joined := errors.Join(errors.New("NFS listener is not TCP"), closeErr)
		if joined == nil {
			return "", errors.New("NFS listener type assertion failed without an error")
		}
		return "", fmt.Errorf("close non-TCP NFS listener: %w", joined)
	}
	file, err := tcp.File()
	if err != nil {
		closeErr := listener.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("close NFS listener after file conversion: %w", closeErr)
		}
		listenerErr := fmt.Errorf("extract NFS listener file: %w", err)
		joined := errors.Join(listenerErr, closeErr)
		if joined == nil {
			return "", errors.New("NFS listener file conversion failed without an error")
		}
		return "", fmt.Errorf("close NFS listener after file conversion: %w", joined)
	}
	writeErr := writeControl("portlist", strconv.FormatUint(uint64(file.Fd()), 10))
	fileCloseErr := file.Close()
	if fileCloseErr != nil {
		fileCloseErr = fmt.Errorf("close NFS listener file: %w", fileCloseErr)
	}
	listenerCloseErr := listener.Close()
	if listenerCloseErr != nil {
		listenerCloseErr = fmt.Errorf("close NFS listener: %w", listenerCloseErr)
	}
	joined := errors.Join(writeErr, fileCloseErr, listenerCloseErr)
	if joined != nil {
		return "", fmt.Errorf("configure NFS listener: %w", joined)
	}
	ports, err = readControl("portlist")
	if err != nil {
		return "", fmt.Errorf("read NFS listener port list: %w", err)
	}
	if ports != nfsTCPPort {
		return "", fmt.Errorf("NFS TCP listener readback failed: %q", ports)
	}
	return ports, nil
}

func (r *kernelRuntime) startHelpers(
	state *diskState,
	save func() error,
	failed func(error),
	threadCount int,
) error {
	r.tracker = &kernelRuntime{config: r.config}
	trackerArgs := []string{"-F", "-p", pipefsDir, "-s", trackerDir}
	err := r.tracker.startHelper(
		trackerName,
		trackerArgs,
		&state.TrackerPID,
		&state.TrackerStart,
		save,
		failed,
	)
	if err != nil {
		return fmt.Errorf("start nfsdcld: %w", err)
	}
	err = r.tracker.waitTrackerReady()
	if err != nil {
		return fmt.Errorf("wait for nfsdcld readiness: %w", err)
	}
	mountdArgs := []string{"-F", "-N", "2", "-N", "3", "--no-udp"}
	err = r.startHelper(
		"rpc.mountd",
		mountdArgs,
		&state.MountdPID,
		&state.MountdStart,
		save,
		failed,
	)
	if err != nil {
		return fmt.Errorf("start rpc.mountd: %w", err)
	}
	err = r.waitChannels([]string{"/auth.unix.ip/channel", "/nfsd.export/channel"})
	if err != nil {
		return fmt.Errorf("wait for rpc.mountd channels: %w", err)
	}
	if threadCount == 0 {
		err = writeControl("threads", "8")
		if err != nil {
			return fmt.Errorf("enable nfsd threads: %w", err)
		}
	}
	err = r.tracker.waitChannels([]string{"/nfsd/cld"})
	if err != nil {
		return fmt.Errorf("wait for nfsdcld channel: %w", err)
	}
	return nil
}

func (r *kernelRuntime) startHelper(
	name string,
	args []string,
	pid *int,
	startTime *string,
	save func() error,
	failed func(error),
) error {
	//nolint:gosec // G204: private callers supply only rpc.mountd/nfsdcld and build argv without a shell.
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	r.mu.Lock()
	r.cmd = cmd
	r.done = make(chan struct{})
	r.stopping = false
	r.exit = nil
	r.name = name
	done := r.done
	r.mu.Unlock()
	go func() {
		waitErr := cmd.Wait()
		if waitErr == nil {
			waitErr = fmt.Errorf("%s exited unexpectedly", name)
		}
		r.mu.Lock()
		r.exit = waitErr
		stopping := r.stopping
		close(done)
		r.mu.Unlock()
		if !stopping {
			slog.Error("NFS helper stopped", "helper", name, "error", waitErr)
			failed(fmt.Errorf("%s: %w", name, waitErr))
		}
	}()
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		identityErr := fmt.Errorf("read %s process identity: %w", name, err)
		joined := errors.Join(identityErr, r.close())
		if joined == nil {
			return identityErr
		}
		return fmt.Errorf("read helper process identity and cleanup: %w", joined)
	}
	r.mu.Lock()
	r.startTime = start
	r.mu.Unlock()
	*pid = cmd.Process.Pid
	*startTime = start
	err = save()
	if err != nil {
		saveErr := fmt.Errorf("persist %s helper state: %w", name, err)
		joined := errors.Join(saveErr, r.close())
		if joined == nil {
			return saveErr
		}
		return fmt.Errorf("persist helper state and cleanup: %w", joined)
	}
	return r.processHealth()
}

func (r *kernelRuntime) waitChannels(paths []string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := r.processHealth()
		if err != nil {
			return fmt.Errorf("check NFS helper health: %w", err)
		}
		ready, err := helperChannelsReady(r.cmd.Process.Pid, paths)
		if err != nil {
			return fmt.Errorf("inspect NFS helper channels: %w", err)
		}
		if ready {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("%s did not open kernel NFS channels", r.name)
}

// nfsd waits only about a second for the tracker to open its newly-created
// pipe. Verify the database and registered IN_CREATE watch before enabling
// threads, then verify the actual cld pipe descriptor after the handshake.
func (r *kernelRuntime) waitTrackerReady() error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := r.processHealth()
		if err != nil {
			return fmt.Errorf("check nfsdcld health: %w", err)
		}
		ready, err := trackerReady(r.cmd.Process.Pid)
		if err != nil {
			return fmt.Errorf("inspect nfsdcld readiness: %w", err)
		}
		if ready {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("nfsdcld database and pipe-directory watch not ready")
}

func trackerReady(pid int) (bool, error) {
	ready, err := trackerDatabaseReady()
	if err != nil || !ready {
		return false, err
	}
	var stat unix.Stat_t
	err = unix.Stat(filepath.Join(pipefsDir, "nfsd"), &stat)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect nfsdcld pipe: %w", err)
	}
	return trackerWatchReady(pid, strconv.FormatUint(stat.Ino, 16))
}

func trackerDatabaseReady() (bool, error) {
	db, err := os.Open(filepath.Join(trackerDir, "main.sqlite"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open nfsdcld durable database: %w", err)
	}
	header := make([]byte, 16)
	n, readErr := db.Read(header)
	closeErr := db.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close nfsdcld durable database: %w", closeErr)
	}
	if n != len(header) {
		return false, closeErr
	}
	if readErr != nil {
		readErr = fmt.Errorf("read nfsdcld durable database: %w", readErr)
	}
	err = errors.Join(readErr, closeErr)
	if err != nil {
		return false, fmt.Errorf("read nfsdcld durable database: %w", err)
	}
	if string(header) != "SQLite format 3\x00" {
		return false, errors.New("invalid nfsdcld durable database header")
	}
	return true, nil
}

func trackerWatchReady(pid int, inode string) (bool, error) {
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fdinfo", pid))
	if err != nil {
		return false, fmt.Errorf("read nfsdcld fdinfo: %w", err)
	}
	for _, fd := range fds {
		data, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, fd.Name()))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return false, fmt.Errorf("read nfsdcld fdinfo %s: %w", fd.Name(), readErr)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if !strings.HasPrefix(line, "inotify wd:") {
				continue
			}
			fields := strings.Fields(line)
			if slices.Contains(fields, "ino:"+inode) && slices.Contains(fields, "mask:100") {
				return true, nil
			}
		}
	}
	return false, nil
}

func helperChannelsReady(pid int, channels []string) (bool, error) {
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return false, fmt.Errorf("read helper fd directory: %w", err)
	}
	found := make([]bool, len(channels))
	for _, fd := range fds {
		path, readErr := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return false, fmt.Errorf("read helper fd %s: %w", fd.Name(), readErr)
		}
		for i, channel := range channels {
			if strings.HasSuffix(path, channel) {
				found[i] = true
			}
		}
	}
	for _, present := range found {
		if !present {
			return false, nil
		}
	}
	return true, nil
}

func (r *kernelRuntime) processHealth() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd == nil || r.stopping {
		return fmt.Errorf("%s is not running", r.name)
	}
	select {
	case <-r.done:
		return fmt.Errorf("%s failed: %w", r.name, r.exit)
	default:
	}
	state, start, err := processInfo(r.cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("%s process health: %w", r.name, err)
	}
	if start != r.startTime || state == "Z" || state == "X" {
		return fmt.Errorf("%s process exited or identity changed", r.name)
	}
	return nil
}

func (r *kernelRuntime) health() error {
	err := r.processHealth()
	if err != nil {
		return err
	}
	if r.tracker == nil {
		return errors.New("NFS recovery daemon not running")
	}
	err = r.tracker.processHealth()
	if err != nil {
		return err
	}
	threads, err := readControl("threads")
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(threads)
	if err != nil || n <= 0 {
		return errors.New("kernel NFS server has no running threads")
	}
	ports, err := readControl("portlist")
	if err != nil || ports != nfsTCPPort {
		return errors.New("kernel NFS listener readback changed")
	}
	return nil
}

func (r *kernelRuntime) close() error {
	errs := make([]error, 0, 5)
	errs = append(errs, r.stopKernel(), r.stopHelper())
	if r.tracker != nil {
		errs = append(errs, r.tracker.close())
	}
	if !r.serverClaimed {
		errs = append(errs, r.unmountFilesystems())
	}
	err := errors.Join(errs...)
	if err == nil {
		return nil
	}
	return fmt.Errorf("NFS runtime cleanup: %w", err)
}

func (r *kernelRuntime) stopKernel() error {
	if !r.serverClaimed {
		return nil
	}
	err := writeControl("threads", "0")
	if err == nil {
		threads, readErr := readControl("threads")
		err = readErr
		if err == nil && threads != "0" {
			err = errors.New("owned nfsd shutdown readback failed")
		}
	}
	if err != nil {
		return fmt.Errorf("stop owned nfsd: %w", err)
	}
	r.serverClaimed = false
	return nil
}

func (r *kernelRuntime) unmountFilesystems() error {
	var pipefsErr, nfsdErr error
	if r.mountedPipefs {
		pipefsErr = unix.Unmount(pipefsDir, 0)
		if pipefsErr != nil {
			pipefsErr = fmt.Errorf("unmount NFS tracking pipefs: %w", pipefsErr)
		} else {
			r.mountedPipefs = false
		}
	}
	if r.mountedNfsd {
		nfsdErr = unix.Unmount(nfsdRoot, 0)
		if nfsdErr != nil {
			nfsdErr = fmt.Errorf("unmount nfsd filesystem: %w", nfsdErr)
		} else {
			r.mountedNfsd = false
		}
	}
	err := errors.Join(pipefsErr, nfsdErr)
	if err == nil {
		return nil
	}
	return fmt.Errorf("unmount NFS filesystems: %w", err)
}

func (r *kernelRuntime) stopHelper() error {
	r.mu.Lock()
	if r.cmd == nil {
		r.mu.Unlock()
		return nil
	}
	r.stopping = true
	cmd, done := r.cmd, r.done
	r.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	err := cmd.Process.Signal(syscall.SIGTERM)
	if errors.Is(err, os.ErrProcessDone) {
		err = nil
	} else if err != nil {
		err = fmt.Errorf("signal %s helper: %w", r.name, err)
	}
	select {
	case <-done:
		return err
	case <-time.After(5 * time.Second):
		killErr := cmd.Process.Kill()
		if killErr != nil {
			killErr = fmt.Errorf("kill %s helper: %w", r.name, killErr)
		}
		select {
		case <-done:
			joined := errors.Join(err, killErr)
			if joined != nil {
				return fmt.Errorf("terminate NFS helper: %w", joined)
			}
			return nil
		case <-time.After(5 * time.Second):
			joined := errors.Join(err, killErr, fmt.Errorf("%s cleanup timed out", r.name))
			if joined == nil {
				return nil
			}
			return fmt.Errorf("terminate NFS helper: %w", joined)
		}
	}
}

// etab is the NFS utility's authoritative admission table; /proc/fs/nfsd/exports is
// only a demand-populated cache and cannot prove an empty ACL or exact set.
func parseEtab(data string) ([]entry, error) {
	var entries []entry
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, errors.New("unrecognized NFS admission table row")
		}
		open := strings.IndexByte(fields[1], '(')
		if open < 1 || !strings.HasSuffix(fields[1], ")") {
			return nil, errors.New("unrecognized NFS admission table client options")
		}
		client := fields[1][:open]
		client = strings.TrimPrefix(strings.TrimSuffix(client, "]"), "[")
		entries = append(entries, entry{fields[0], client, fields[1][open+1 : len(fields[1])-1]})
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		return nil, fmt.Errorf("scan NFS admission table: %w", scanErr)
	}
	return entries, nil
}

func ensurePrivateEtab(path string) error {
	//nolint:gosec // G304: fixed private nfs-utils state path; O_NOFOLLOW rejects symlinks.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open private NFS admission table %q: %w", path, err)
	}
	closeFile := func(operationErr error) error {
		closeErr := file.Close()
		if closeErr == nil {
			return operationErr
		}
		closeContext := fmt.Errorf("close private NFS admission table %q: %w", path, closeErr)
		if operationErr == nil {
			return closeContext
		}
		return errors.Join(operationErr, closeContext)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return closeFile(fmt.Errorf("stat private NFS admission table %q: %w", path, statErr))
	}
	if !info.Mode().IsRegular() {
		return closeFile(fmt.Errorf("private NFS admission table %q is not a regular file", path))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return closeFile(fmt.Errorf("private NFS admission table %q must be root-owned", path))
	}
	if info.Mode().Perm() != 0o600 {
		chmodErr := file.Chmod(0o600)
		if chmodErr != nil {
			return closeFile(fmt.Errorf("chmod private NFS admission table %q: %w", path, chmodErr))
		}
		verified, verifyErr := file.Stat()
		if verifyErr != nil {
			return closeFile(fmt.Errorf("stat private NFS admission table %q after chmod: %w", path, verifyErr))
		}
		if verified.Mode().Perm() != 0o600 {
			return closeFile(fmt.Errorf("private NFS admission table %q mode after chmod = %o, want 600",
				path, verified.Mode().Perm()))
		}
	}
	return closeFile(nil)
}

func (*kernelRuntime) list(_ context.Context) ([]entry, error) {
	data, err := os.ReadFile(etabPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read NFS admission table: %w", err)
	}
	return parseEtab(string(data))
}

func exportOperand(client, path string) string {
	if strings.Contains(client, ":") {
		client = "[" + client + "]"
	}
	return client + ":" + path
}

func runExportfs(ctx context.Context, args ...string) error {
	err := ensurePrivateEtab(etabPath)
	if err != nil {
		return fmt.Errorf("prepare NFS admission table for export operation: %w", err)
	}
	// Private grant/revoke callers construct options and delimit operands with "--"; no shell is involved.
	//nolint:gosec // G204: the executable is the fixed nfs-utils export utility.
	output, err := exec.CommandContext(ctx, exportUtility, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("NFS export operation failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (*kernelRuntime) validateExport(e Export) error {
	canonical, err := filepath.EvalSymlinks(e.Path)
	if err != nil {
		return fmt.Errorf("resolve NFS export path %q: %w", e.Path, err)
	}
	if canonical != e.Path {
		return errors.New("NFS export path traverses a symlink")
	}
	info, err := os.Stat(e.Path)
	if err != nil {
		return fmt.Errorf("stat NFS export path %q: %w", e.Path, err)
	}
	if !info.IsDir() {
		return errors.New("NFS export is not a filesystem directory")
	}
	if e.VolumeID == rootVolumeID {
		_, _, err = unix.NameToHandleAt(unix.AT_FDCWD, e.Path, 0)
		if err != nil {
			return fmt.Errorf("NFS pseudoroot %q cannot encode export filehandles: %w", e.Path, err)
		}
		return nil
	}
	// Do not silently export the empty parent directory when its dataset is
	// absent after reboot or a failed mount. Each child is a real mountpoint.
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return &runtimeValidationError{fmt.Errorf("read mount table for NFS dataset %q: %w", e.VolumeID, err)}
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 6 && fields[4] == e.Path {
			return nil
		}
	}
	return fmt.Errorf("NFS dataset %q is not mounted at %q", e.VolumeID, e.Path)
}

func (r *kernelRuntime) grant(ctx context.Context, e Export, client string) error {
	err := r.validateExport(e)
	if err != nil {
		return err
	}
	return runExportfs(ctx, "-i", "-o", e.options(), "--", exportOperand(client, e.Path))
}

func (*kernelRuntime) revoke(ctx context.Context, path, client string) error {
	return runExportfs(ctx, "-u", "--", exportOperand(client, path))
}
