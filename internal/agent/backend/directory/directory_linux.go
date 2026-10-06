//go:build linux

package directory

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

const (
	adoptionKind    = "directory"
	reasonMissing   = "missing"
	reasonWrongType = "wrong type"
	reasonTooSmall  = "too small"
	reasonLayout    = "layout"
	reasonQuota     = "quota"
	reasonIdentity  = "identity"
)

const (
	fsIoctlGetXattr    = 0x801c581f
	fsXFlagProjInherit = 0x00000200
	qcmdExt4GetQuota   = 0x80000702
	qcmdXFSGetQuota    = 0x580302
	qcmdGetQuotaStatV  = 0x580802
	qifBHard           = 1 << 0
	qifInodes          = 1 << 3
	fsQstatVVersion1   = 1
	fsQuotaPDQAcct     = 1 << 4
	fsQuotaPDQEnfd     = 1 << 5
	// FS_PROJ_QUOTA identifies fs_disk_quota.d_flags in linux/dqblk_xfs.h;
	// FS_QUOTA_PDQ_ENFD above separately identifies enforcement state.
	fsProjectQuota int8 = 1 << 1
)

const (
	fsQuotaProjectOption = "prjquota"
)

// Blank fields retain unused Linux UAPI members, including their alignment.
type fsxattr struct {
	xflags uint32
	_      uint32 // fsx_extsize
	_      uint32 // fsx_nextents
	projid uint32
	_      uint32  // fsx_cowextsize
	_      [8]byte // fsx_pad
}

type ifDqblk struct {
	bhardlimit uint64
	_          uint64 // dqb_bsoftlimit
	_          uint64 // dqb_curspace
	_          uint64 // dqb_ihardlimit
	_          uint64 // dqb_isoftlimit
	curinodes  uint64
	_          uint64 // dqb_btime
	_          uint64 // dqb_itime
	valid      uint32
	_          uint32 // trailing padding
}

type fsDiskQuota struct {
	_            int8 // d_version
	flags        int8
	_            uint16 // d_fieldmask
	_            uint32 // d_id
	blkHardlimit uint64
	_            uint64 // d_blk_softlimit
	_            uint64 // d_ino_hardlimit
	_            uint64 // d_ino_softlimit
	_            uint64 // d_bcount
	icount       uint64
	_            int32   // d_itimer
	_            int32   // d_btimer
	_            uint16  // d_iwarns
	_            uint16  // d_bwarns
	_            int32   // d_padding2
	_            uint64  // d_rtb_hardlimit
	_            uint64  // d_rtb_softlimit
	_            uint64  // d_rtbcount
	_            int32   // d_rtbtimer
	_            uint16  // d_rtbwarns
	_            int16   // d_padding3
	_            [8]byte // d_padding4
}

// fsQuotaStatV mirrors struct fs_quota_statv from linux/dqblk_xfs.h.
type fsQuotaStatV struct {
	version int8
	_       uint8 // qs_pad1
	flags   uint16
	_       uint32 // qs_incoredqs
	_       [3]struct {
		_ uint64 // qfs_ino
		_ uint64 // qfs_nblks
		_ uint32 // qfs_nextents
		_ uint32 // qfs_pad
	} // qs_uquota, qs_gquota, qs_pquota
	_ int32     // qs_btimelimit
	_ int32     // qs_itimelimit
	_ int32     // qs_rtbtimelimit
	_ uint16    // qs_bwarnlimit
	_ uint16    // qs_iwarnlimit
	_ uint16    // qs_rtbwarnlimit
	_ uint16    // qs_pad3
	_ uint32    // qs_pad4
	_ [7]uint64 // qs_pad2
}

type dirPin struct {
	fd          int
	mountSource string
}

type mountRecord struct {
	mountPoint string
	major      uint32
	minor      uint32
	fsType     string
	source     string
	options    string
}

func (b *Backend) inspect(
	ctx context.Context,
	source string,
	requiredBytes int64,
	expected *agentv1.FilesystemAdoption,
	pin bool,
) (inspection *backend.ImportInspection, resultPin *dirPin, retErr error) {
	err := ctx.Err()
	if err != nil {
		return nil, nil, fmt.Errorf("directory source %q: %w", source, err)
	}
	if requiredBytes <= 0 {
		return nil, nil, fmt.Errorf("directory import: required capacity must be positive")
	}
	paths, err := b.resolveSource(source)
	if err != nil {
		return nil, nil, err
	}
	fd, err := openPinnedDir(paths.rootSys, paths.sourceSys)
	if err != nil {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonMissing,
			Detail: fmt.Sprintf("secure open %q: %v", paths.canonicalHost, err),
		}
	}
	closed := false
	defer func() {
		if !closed {
			closeErr := unix.Close(fd)
			if closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf(
					"directory source %q: close inspection pin: %w", paths.canonicalHost, closeErr,
				))
				inspection = nil
			}
		}
	}()
	inspection, err = b.inspectPinnedSource(ctx, fd, paths, requiredBytes, expected)
	if err != nil {
		return nil, nil, err
	}
	if !pin {
		return inspection, nil, nil
	}
	closed = true
	return inspection, &dirPin{fd: fd, mountSource: fmt.Sprintf("/proc/self/fd/%d", fd)}, nil
}

func (b *Backend) inspectPinnedSource(
	ctx context.Context,
	fd int,
	paths resolvedSource,
	requiredBytes int64,
	expected *agentv1.FilesystemAdoption,
) (*backend.ImportInspection, error) {
	st, err := validatePinnedSource(fd, paths.sourceSys, paths.canonicalHost)
	if err != nil {
		return nil, err
	}
	filesystem, err := b.inspectFilesystem(fd, paths.sourceSys, paths.canonicalHost, st)
	if err != nil {
		return nil, err
	}
	err = validateQuota(filesystem.quota, requiredBytes)
	if err != nil {
		return nil, err
	}
	if expected == nil {
		err = verifyProjectTree(ctx, fd, filesystem.project, filesystem.quota.inodes, readProject)
		if err != nil {
			return nil, err
		}
	}
	adoption := &agentv1.FilesystemAdoption{
		Kind:            adoptionKind,
		CanonicalSource: filepath.Clean(paths.canonicalHost),
		ResourceId:      filesystem.fsUUID + ":" + strconv.FormatUint(st.Ino, 10),
		FilesystemType:  filesystem.fsType,
		FilesystemId:    filesystem.fsUUID,
		Inode:           st.Ino,
		ProjectId:       filesystem.project,
	}
	if expected != nil {
		err = equalAdoption(adoption, expected)
		if err != nil {
			return nil, err
		}
	}
	return &backend.ImportInspection{Filesystem: adoption, CapacityBytes: filesystem.quota.capacity}, nil
}

type resolvedSource struct {
	rootSys       string
	sourceSys     string
	canonicalHost string
}

func (b *Backend) resolveSource(source string) (resolvedSource, error) {
	if source == "" || !filepath.IsAbs(source) {
		return resolvedSource{}, &backend.ImportRefusedError{
			Reason: reasonMissing,
			Detail: fmt.Sprintf("source %q is not absolute", source),
		}
	}
	rootHost, err := filepath.Abs(filepath.Clean(b.hostRoot))
	if err != nil {
		return resolvedSource{}, fmt.Errorf("directory root: %w", err)
	}
	rootSys, err := filepath.EvalSymlinks(b.sysPath(rootHost))
	if err != nil {
		return resolvedSource{}, fmt.Errorf("directory root %q: %w", rootHost, err)
	}
	sourceHost, err := filepath.Abs(filepath.Clean(source))
	if err != nil {
		return resolvedSource{}, fmt.Errorf("directory source: %w", err)
	}
	sourceSys, err := filepath.EvalSymlinks(b.sysPath(sourceHost))
	if err != nil {
		return resolvedSource{}, &backend.ImportRefusedError{
			Reason: reasonMissing,
			Detail: fmt.Sprintf("source %q: %v", source, err),
		}
	}
	if !within(rootSys, sourceSys) {
		return resolvedSource{}, &backend.ImportRefusedError{
			Reason: reasonLayout,
			Detail: fmt.Sprintf(
				"canonical source %q is outside configured host root %q",
				sourceSys,
				rootSys,
			),
		}
	}
	paths := resolvedSource{rootSys: rootSys, sourceSys: sourceSys, canonicalHost: sourceSys}
	if b.hostRootPrefix == "" {
		return paths, nil
	}
	prefixSys, err := filepath.EvalSymlinks(b.hostRootPrefix)
	if err != nil {
		return resolvedSource{}, fmt.Errorf("directory host root prefix %q: %w", b.hostRootPrefix, err)
	}
	if !within(prefixSys, sourceSys) {
		return resolvedSource{}, fmt.Errorf("directory source %q escaped host prefix %q", sourceSys, prefixSys)
	}
	paths.canonicalHost = "/" + strings.TrimPrefix(strings.TrimPrefix(sourceSys, prefixSys), string(filepath.Separator))
	if paths.canonicalHost == "//" {
		paths.canonicalHost = "/"
	}
	return paths, nil
}

func validatePinnedSource(fd int, sourceSys, canonicalHost string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstat(fd, &st)
	if err != nil {
		return unix.Stat_t{}, fmt.Errorf("directory source %q: fstat: %w", canonicalHost, err)
	}
	var pathStat unix.Stat_t
	err = unix.Stat(sourceSys, &pathStat)
	if err != nil {
		return unix.Stat_t{}, fmt.Errorf("directory source %q: stat after secure open: %w", canonicalHost, err)
	}
	if st.Dev != pathStat.Dev || st.Ino != pathStat.Ino {
		return unix.Stat_t{}, &backend.ImportRefusedError{
			Reason: reasonIdentity,
			Detail: fmt.Sprintf("source %q changed while being pinned", canonicalHost),
		}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Stat_t{}, &backend.ImportRefusedError{Reason: reasonWrongType, Detail: "source is not a directory"}
	}
	return st, nil
}

type filesystemInspection struct {
	fsType  string
	fsUUID  string
	project uint32
	quota   quotaResult
}

func (b *Backend) inspectFilesystem(
	fd int,
	sourceSys, canonicalHost string,
	st unix.Stat_t,
) (filesystemInspection, error) {
	mount, err := findMount(b.mountInfoPath, sourceSys, unix.Major(st.Dev), unix.Minor(st.Dev))
	if err != nil {
		return filesystemInspection{}, err
	}
	err = rejectSubmounts(b.mountInfoPath, sourceSys, mount.mountPoint)
	if err != nil {
		return filesystemInspection{}, err
	}
	fsType := strings.ToLower(mount.fsType)
	if fsType != "ext4" && fsType != "xfs" {
		return filesystemInspection{}, &backend.ImportRefusedError{
			Reason: reasonWrongType,
			Detail: fmt.Sprintf(
				"filesystem type %q is unsupported; only ext4 and xfs are supported",
				mount.fsType,
			),
		}
	}
	fsUUID, err := readNativeUUID(mount, b.hostRootPrefix)
	if err != nil {
		return filesystemInspection{}, fmt.Errorf(
			"directory source %q: native %s UUID: %w",
			canonicalHost,
			fsType,
			err,
		)
	}
	project, inherit, err := readProject(fd)
	if err != nil {
		return filesystemInspection{}, fmt.Errorf(
			"directory source %q: project attributes: %w",
			canonicalHost,
			err,
		)
	}
	if project == 0 || !inherit {
		return filesystemInspection{}, &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: fmt.Sprintf("project %d is zero or not inheritable", project),
		}
	}
	quota, err := readProjectQuota(mount, project, b.hostRootPrefix)
	if err != nil {
		return filesystemInspection{}, fmt.Errorf(
			"directory source %q: project quota: %w",
			canonicalHost,
			err,
		)
	}
	return filesystemInspection{fsType: fsType, fsUUID: fsUUID, project: project, quota: quota}, nil
}

func validateQuota(quota quotaResult, requiredBytes int64) error {
	if quota.capacity <= 0 {
		return &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: "project quota is unbounded or has no hard limit",
		}
	}
	if quota.capacity != requiredBytes {
		return &backend.ImportRefusedError{
			Reason: reasonTooSmall,
			Detail: fmt.Sprintf(
				"effective project hard bound is %d bytes, requested %d",
				quota.capacity,
				requiredBytes,
			),
		}
	}
	if quota.inodes == 0 {
		return &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: "project quota reports zero or unknown inode scope",
		}
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func openPinnedDir(root, source string) (int, error) {
	rootFD, err := unix.Open(
		root,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return -1, fmt.Errorf("open directory root pin: %w", err)
	}
	rel, err := filepath.Rel(root, source)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if err == nil {
			err = syscall.EPERM
		}
		return -1, closePinnedDir(rootFD, err)
	}
	if rel == "." {
		return rootFD, nil
	}
	current := rootFD
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		next, openErr := openPinnedComponent(current, component)
		if openErr != nil {
			return -1, openErr
		}
		current = next
	}
	return current, nil
}

func closePinnedDir(fd int, cause error) error {
	closeErr := unix.Close(fd)
	if closeErr != nil {
		cause = errors.Join(cause, fmt.Errorf("close directory pin: %w", closeErr))
	}
	return fmt.Errorf("open pinned directory: %w", cause)
}

func openPinnedComponent(current int, component string) (int, error) {
	if component == "" || component == "." || component == ".." {
		return -1, closePinnedDir(current, syscall.EPERM)
	}
	next, openErr := unix.Openat(
		current,
		component,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK,
		0,
	)
	closeErr := unix.Close(current)
	if openErr != nil {
		if closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("close directory pin: %w", closeErr))
		}
		return -1, fmt.Errorf("open directory component %q: %w", component, openErr)
	}
	if closeErr != nil {
		nextCloseErr := unix.Close(next)
		closeErr = fmt.Errorf("close directory pin: %w", closeErr)
		if nextCloseErr != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close next directory pin: %w", nextCloseErr))
		}
		return -1, fmt.Errorf("open directory component %q: %w", component, closeErr)
	}
	return next, nil
}

func openMountInfo(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		closeErr := unix.Close(fd)
		if closeErr != nil {
			return nil, fmt.Errorf("create mountinfo file %q: %w", path, errors.Join(
				errors.New("invalid mountinfo file descriptor"),
				fmt.Errorf("close mountinfo: %w", closeErr),
			))
		}
		return nil, errors.New("invalid mountinfo file descriptor")
	}
	return file, nil
}

func findMount(path, target string, major, minor uint32) (best mountRecord, retErr error) {
	f, err := openMountInfo(path)
	if err != nil {
		return mountRecord{}, err
	}
	defer func() {
		closeErr := f.Close()
		if closeErr != nil {
			best = mountRecord{}
			retErr = errors.Join(retErr, fmt.Errorf("close mountinfo %q: %w", path, closeErr))
		}
	}()
	bestLen := -1
	s := bufio.NewScanner(f)
	for s.Scan() {
		record, ok := parseMountRecord(s.Text(), target, major, minor)
		if !ok {
			continue
		}
		if len(record.mountPoint) > bestLen {
			bestLen = len(record.mountPoint)
			best = record
		}
	}
	err = s.Err()
	if err != nil {
		return mountRecord{}, fmt.Errorf("scan mountinfo %q: %w", path, err)
	}
	if bestLen < 0 {
		return mountRecord{}, fmt.Errorf(
			"directory source %q: no mountinfo entry with matching device %d:%d",
			target,
			major,
			minor,
		)
	}
	return best, nil
}

func parseMountRecord(line, target string, major, minor uint32) (mountRecord, bool) {
	fields := strings.Fields(line)
	sep := -1
	for i, field := range fields {
		if field == "-" {
			sep = i
			break
		}
	}
	if sep < 6 || len(fields) <= sep+2 {
		return mountRecord{}, false
	}
	majorText, minorText, ok := strings.Cut(fields[2], ":")
	if !ok {
		return mountRecord{}, false
	}
	mj, majorErr := strconv.ParseUint(majorText, 10, 32)
	mn, minorErr := strconv.ParseUint(minorText, 10, 32)
	if majorErr != nil || minorErr != nil || mj != uint64(major) || mn != uint64(minor) {
		return mountRecord{}, false
	}
	mp := decodeMountField(fields[4])
	if !withinOrEqual(mp, target) {
		return mountRecord{}, false
	}
	options := fields[5]
	if len(fields) > sep+3 {
		options += "," + fields[sep+3]
	}
	return mountRecord{
		mountPoint: mp,
		major:      major,
		minor:      minor,
		fsType:     fields[sep+1],
		source:     decodeMountField(fields[sep+2]),
		options:    options,
	}, true
}

func rejectSubmounts(path, source, rootMount string) (retErr error) {
	f, err := openMountInfo(path)
	if err != nil {
		return fmt.Errorf("open mountinfo for submount validation: %w", err)
	}
	defer func() {
		closeErr := f.Close()
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close mountinfo %q: %w", path, closeErr))
		}
	}()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 5 {
			continue
		}
		mp := decodeMountField(fields[4])
		if mp != rootMount && within(source, mp) {
			return &backend.ImportRefusedError{
				Reason: reasonLayout,
				Detail: fmt.Sprintf("submount %q exists below source %q", mp, source),
			}
		}
	}
	err = s.Err()
	if err != nil {
		return fmt.Errorf("scan mountinfo for submounts: %w", err)
	}
	return nil
}

func withinOrEqual(root, path string) bool {
	return within(root, path) || filepath.Clean(root) == filepath.Clean(path)
}

func decodeMountField(s string) string {
	s = strings.ReplaceAll(s, `\040`, " ")
	s = strings.ReplaceAll(s, `\011`, "\t")
	return strings.ReplaceAll(s, `\134`, `\`)
}

func readNativeUUID(m mountRecord, prefix string) (uuid string, retErr error) {
	source := m.source
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("mount source %q is not an absolute block path", source)
	}
	fd, err := openNativeSource(source, prefix)
	if err != nil {
		return "", err
	}
	defer func() {
		closeErr := unix.Close(fd)
		if closeErr != nil {
			uuid = ""
			retErr = errors.Join(retErr, fmt.Errorf("close mount source %q: %w", source, closeErr))
		}
	}()
	var ds unix.Stat_t
	err = unix.Fstat(fd, &ds)
	if err != nil {
		return "", fmt.Errorf("stat mount source %q: %w", source, err)
	}
	if ds.Mode&unix.S_IFMT != unix.S_IFBLK {
		return "", fmt.Errorf("mount source %q is not a block device", source)
	}
	if unix.Major(ds.Rdev) != m.major || unix.Minor(ds.Rdev) != m.minor {
		return "", fmt.Errorf(
			"mount source %q device identity does not match mountinfo %d:%d",
			source,
			m.major,
			m.minor,
		)
	}
	return readNativeSuperblock(fd, m.fsType)
}

func openNativeSource(source, prefix string) (int, error) {
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err == nil || prefix == "" {
		if err != nil {
			return -1, fmt.Errorf("open mount source %q: %w", source, err)
		}
		return fd, nil
	}
	firstErr := err
	fd, err = unix.Open(filepath.Join(prefix, source), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		err = errors.Join(firstErr, fmt.Errorf("open prefixed mount source: %w", err))
		return -1, fmt.Errorf("open mount source %q: %w", source, err)
	}
	return fd, nil
}

func readNativeSuperblock(fd int, fsType string) (string, error) {
	if strings.EqualFold(fsType, "ext4") {
		return readExt4UUID(fd)
	}
	if strings.EqualFold(fsType, "xfs") {
		return readXFSUUID(fd)
	}
	return "", fmt.Errorf("unsupported filesystem type %q", fsType)
}

func readExt4UUID(fd int) (string, error) {
	var buf [1144]byte
	n, err := unix.Pread(fd, buf[:], 0)
	if err != nil || n != len(buf) {
		if err == nil {
			err = errors.New("short ext4 superblock read")
		}
		return "", fmt.Errorf("read ext4 superblock: %w", err)
	}
	if binary.LittleEndian.Uint16(buf[1080:]) != 0xef53 {
		return "", fmt.Errorf("invalid ext4 superblock magic")
	}
	return formatUUID(buf[1128:1144]), nil
}

func readXFSUUID(fd int) (string, error) {
	var buf [48]byte
	n, err := unix.Pread(fd, buf[:], 0)
	if err != nil || n != len(buf) {
		if err == nil {
			err = errors.New("short XFS superblock read")
		}
		return "", fmt.Errorf("read XFS superblock: %w", err)
	}
	if string(buf[:4]) != "XFSB" {
		return "", fmt.Errorf("invalid XFS superblock magic")
	}
	return formatUUID(buf[32:48]), nil
}

func formatUUID(raw []byte) string {
	if len(raw) != 16 {
		return ""
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i, v := range raw {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hex[v>>4]
		out[j+1] = hex[v&15]
		j += 2
	}
	return string(out)
}

func readProject(fd int) (projectID uint32, inherit bool, retErr error) {
	var attr fsxattr
	attrPointer := reflect.ValueOf(&attr).Pointer()
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(fsIoctlGetXattr), attrPointer)
	runtime.KeepAlive(&attr)
	if errno != 0 {
		return 0, false, fmt.Errorf("read project attributes ioctl: %w", errno)
	}
	return attr.projid, attr.xflags&fsXFlagProjInherit != 0, nil
}

type quotaResult struct {
	capacity int64
	inodes   uint64
}

func readQuotaStatus(device string) error {
	var status fsQuotaStatV
	status.version = fsQstatVVersion1
	err := quotaCtl(qcmdGetQuotaStatV, device, 0, &status)
	if err != nil {
		return fmt.Errorf("read project quota status: %w", err)
	}
	if status.version != fsQstatVVersion1 {
		return fmt.Errorf("unsupported project quota status version %d", status.version)
	}
	if status.flags&(fsQuotaPDQAcct|fsQuotaPDQEnfd) != (fsQuotaPDQAcct | fsQuotaPDQEnfd) {
		return errors.New("project quota accounting or enforcement is disabled")
	}
	return nil
}

func readProjectQuota(m mountRecord, project uint32, prefix string) (quotaResult, error) {
	device, err := resolveQuotaDevice(m.source, prefix)
	if err != nil {
		return quotaResult{}, err
	}
	if device == "" {
		return quotaResult{}, errors.New("mount has no quota device")
	}
	if strings.EqualFold(m.fsType, "ext4") {
		return readExt4Quota(m.options, device, project)
	}
	if strings.EqualFold(m.fsType, "xfs") {
		return readXFSQuota(device, project)
	}
	return quotaResult{}, fmt.Errorf("unsupported quota filesystem %q", m.fsType)
}

func resolveQuotaDevice(device, prefix string) (string, error) {
	if prefix == "" || !filepath.IsAbs(device) {
		return device, nil
	}
	candidate := filepath.Join(prefix, device)
	_, err := os.Stat(candidate)
	if err == nil {
		return candidate, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat quota device %q: %w", candidate, err)
	}
	return device, nil
}

func readExt4Quota(options, device string, project uint32) (quotaResult, error) {
	if !hasMountOption(options, fsQuotaProjectOption) {
		return quotaResult{}, errors.New("ext4 project quota enforcement is not enabled")
	}
	err := readQuotaStatus(device)
	if err != nil {
		return quotaResult{}, fmt.Errorf("read ext4 project quota status: %w", err)
	}
	var q ifDqblk
	err = quotaCtl(qcmdExt4GetQuota, device, uintptr(project), &q)
	if err != nil {
		return quotaResult{}, fmt.Errorf("read ext4 project quota: %w", err)
	}
	if q.valid&qifBHard == 0 || q.valid&qifInodes == 0 {
		return quotaResult{}, errors.New("project hard-limit or inode-usage fields are not valid")
	}
	capacity, err := checkedQuotaBytes(q.bhardlimit, 10)
	return quotaResult{capacity: capacity, inodes: q.curinodes}, err
}

func readXFSQuota(device string, project uint32) (quotaResult, error) {
	err := readQuotaStatus(device)
	if err != nil {
		return quotaResult{}, fmt.Errorf("read XFS project quota status: %w", err)
	}
	var q fsDiskQuota
	err = quotaCtl(qcmdXFSGetQuota, device, uintptr(project), &q)
	if err != nil {
		return quotaResult{}, fmt.Errorf("read XFS project quota: %w", err)
	}
	if q.flags&fsProjectQuota == 0 {
		return quotaResult{}, errors.New("XFS project quota flag is absent")
	}
	capacity, err := checkedQuotaBytes(q.blkHardlimit, 9)
	return quotaResult{capacity: capacity, inodes: q.icount}, err
}

func hasMountOption(options, want string) bool {
	return slices.Contains(strings.Split(options, ","), want)
}

func quotaCtl(command uint32, device string, id uintptr, result any) error {
	dev, err := syscall.BytePtrFromString(device)
	if err != nil {
		return fmt.Errorf("encode quota device %q: %w", device, err)
	}
	resultPointer := reflect.ValueOf(result).Pointer()
	_, _, errno := unix.Syscall6(
		unix.SYS_QUOTACTL,
		uintptr(command),
		reflect.ValueOf(dev).Pointer(),
		id,
		resultPointer,
		0,
		0,
	)
	runtime.KeepAlive(dev)
	runtime.KeepAlive(result)
	if errno != 0 {
		return fmt.Errorf("quota ioctl %d on %q: %w", command, device, errno)
	}
	return nil
}

func checkedQuotaBytes(blocks uint64, shift uint) (int64, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if blocks == 0 || shift >= 63 || blocks > maxInt64>>shift {
		return 0, errors.New("quota hard-limit conversion overflows or is unbounded")
	}
	bytes := blocks << shift
	if bytes > maxInt64 {
		return 0, errors.New("quota hard-limit conversion overflows or is unbounded")
	}
	return int64(bytes), nil
}

// projectWalk proves that a source tree owns its whole project-quota scope.
// The verified field counts unique inodes whose project ID was read and
// matched. The unverified field counts unique symlink inodes, whose project ID
// cannot be read through an O_PATH descriptor. Only verified inodes may match
// the kernel project inode count, so an unverifiable symlink never stands in
// for a project member outside the source.
type projectWalk struct {
	ctx         context.Context
	project     uint32
	readProject func(fd int) (projectID uint32, inherit bool, err error)
	verified    uint64
	unverified  uint64
	multilink   map[[2]uint64]*linkCount
}

type linkCount struct {
	total uint64
	seen  uint64
}

func normalizeLinkCount[T ~uint32 | ~uint64](links T) uint64 {
	return uint64(links)
}

// verifyProjectTree refuses the source unless every directory and regular file
// carries the project ID, every hard link of a non-directory inode lies inside
// the source, and the kernel project inode count equals the verified source
// inodes. The readAttrs function reads an inode's project ID and inheritance
// flag.
func verifyProjectTree(
	ctx context.Context,
	rootFD int,
	project uint32,
	quotaInodes uint64,
	readAttrs func(fd int) (uint32, bool, error),
) error {
	w := &projectWalk{
		ctx:         ctx,
		project:     project,
		readProject: readAttrs,
		multilink:   make(map[[2]uint64]*linkCount),
	}
	var root unix.Stat_t
	err := unix.Fstat(rootFD, &root)
	if err != nil {
		return fmt.Errorf("stat pinned source: %w", err)
	}
	if root.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("pinned source is not a directory")
	}
	err = w.visit(rootFD, root, true, "source")
	if err != nil {
		return &backend.ImportRefusedError{Reason: reasonQuota, Detail: err.Error()}
	}
	for key, links := range w.multilink {
		if links.seen != links.total {
			return &backend.ImportRefusedError{
				Reason: reasonQuota,
				Detail: fmt.Sprintf(
					"inode %d:%d has %d links in source but %d links on filesystem",
					key[0],
					key[1],
					links.seen,
					links.total,
				),
			}
		}
	}
	if w.verified == quotaInodes {
		return nil
	}
	if w.verified > quotaInodes {
		return &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: fmt.Sprintf(
				"project %d quota inode count %d is below verified source inode count %d",
				project,
				quotaInodes,
				w.verified,
			),
		}
	}
	return &backend.ImportRefusedError{
		Reason: reasonQuota,
		Detail: fmt.Sprintf(
			"project %d quota inode count %d exceeds verified source inode count %d; "+
				"the extra %d may be outside the source or among %d source symlink inodes "+
				"whose project ID cannot be read",
			project,
			quotaInodes,
			w.verified,
			quotaInodes-w.verified,
			w.unverified,
		),
	}
}

func (w *projectWalk) visit(fd int, st unix.Stat_t, isDir bool, label string) error {
	err := w.ctx.Err()
	if err != nil {
		return fmt.Errorf("project tree %q: %w", label, err)
	}
	pid, inherit, err := w.readProject(fd)
	if err != nil {
		return fmt.Errorf("project attributes %q: %w", label, err)
	}
	if pid != w.project {
		return fmt.Errorf("project scope mismatch at %q: got %d want %d", label, pid, w.project)
	}
	if isDir && !inherit {
		return fmt.Errorf("directory %q lacks project inheritance", label)
	}
	w.record(st, true)
	if isDir {
		return w.walkDir(fd, label)
	}
	return nil
}

// record tracks one directory entry. Every hard link of a non-directory inode
// is counted toward link completeness, but the inode is counted once, as
// verified or unverified, when it is first seen.
func (w *projectWalk) record(st unix.Stat_t, verified bool) {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Nlink > 1 {
		key := [2]uint64{st.Dev, st.Ino}
		links := w.multilink[key]
		if links == nil {
			links = &linkCount{total: normalizeLinkCount(st.Nlink)}
			w.multilink[key] = links
			w.countInode(verified)
		}
		links.seen++
		return
	}
	w.countInode(verified)
}

func (w *projectWalk) countInode(verified bool) {
	if verified {
		w.verified++
		return
	}
	w.unverified++
}

func (w *projectWalk) walkDir(fd int, label string) error {
	var buf [64 * 1024]byte
	for {
		err := w.checkContext(label)
		if err != nil {
			return err
		}
		n, err := unix.Getdents(fd, buf[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read directory %q: %w", label, err)
		}
		if n == 0 {
			return nil
		}
		err = w.walkDirEntries(fd, label, buf[:n])
		if err != nil {
			return err
		}
	}
}

func (w *projectWalk) checkContext(label string) error {
	err := w.ctx.Err()
	if err != nil {
		return fmt.Errorf("walk directory %q: %w", label, err)
	}
	return nil
}

func (w *projectWalk) walkDirEntries(fd int, label string, buf []byte) error {
	for offset := 0; offset < len(buf); {
		err := w.checkContext(label)
		if err != nil {
			return err
		}
		recordLen, name, err := parseLinuxDirent(buf[offset:])
		if err != nil {
			return err
		}
		offset += recordLen
		if name == "" || name == "." || name == ".." {
			continue
		}
		err = w.visitEntry(fd, name)
		if err != nil {
			return fmt.Errorf("inspect entry %q: %w", name, err)
		}
	}
	return nil
}

func parseLinuxDirent(buf []byte) (recordLen int, name string, retErr error) {
	if len(buf) < 19 {
		return 0, "", errors.New("short linux directory entry")
	}
	recordLen = int(binary.LittleEndian.Uint16(buf[16:18]))
	if recordLen < 19 || recordLen > len(buf) {
		return 0, "", errors.New("invalid linux directory entry length")
	}
	nameBytes := buf[19:recordLen]
	end := 0
	for end < len(nameBytes) && nameBytes[end] != 0 {
		end++
	}
	return recordLen, string(nameBytes[:end]), nil
}

func (w *projectWalk) visitEntry(parentFD int, name string) (retErr error) {
	var expected unix.Stat_t
	err := unix.Fstatat(parentFD, name, &expected, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return fmt.Errorf("stat entry %q: %w", name, err)
	}
	mode := expected.Mode & unix.S_IFMT
	var flags int
	isDir := mode == unix.S_IFDIR
	switch mode {
	case unix.S_IFDIR:
		flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	case unix.S_IFREG:
		flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	case unix.S_IFLNK:
		flags = unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	default:
		return fmt.Errorf("unsupported special entry %q (mode %#o)", name, mode)
	}
	err = w.ctx.Err()
	if err != nil {
		return fmt.Errorf("inspect entry %q: %w", name, err)
	}
	fd, err := unix.Openat(parentFD, name, flags, 0)
	if err != nil {
		return fmt.Errorf("open entry %q without following links: %w", name, err)
	}
	defer func() {
		closeErr := unix.Close(fd)
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close entry %q: %w", name, closeErr))
		}
	}()
	var actual unix.Stat_t
	err = unix.Fstat(fd, &actual)
	if err != nil {
		return fmt.Errorf("fstat entry %q: %w", name, err)
	}
	if actual.Dev != expected.Dev || actual.Ino != expected.Ino || actual.Mode&unix.S_IFMT != mode {
		return fmt.Errorf("entry %q changed while being inspected", name)
	}
	if mode == unix.S_IFLNK {
		w.record(actual, false)
		return nil
	}
	return w.visit(fd, actual, isDir, name)
}

func equalAdoption(actual, expected *agentv1.FilesystemAdoption) error {
	if expected.GetKind() != adoptionKind ||
		expected.GetCanonicalSource() != actual.GetCanonicalSource() ||
		expected.GetResourceId() != actual.GetResourceId() ||
		expected.GetFilesystemType() != actual.GetFilesystemType() ||
		expected.GetFilesystemId() != actual.GetFilesystemId() ||
		expected.GetInode() != actual.GetInode() ||
		expected.GetProjectId() != actual.GetProjectId() {
		return &backend.ImportRefusedError{
			Reason: reasonIdentity,
			Detail: fmt.Sprintf(
				"recorded filesystem identity does not match current source: current=%s:%d recorded=%s:%d",
				actual.GetFilesystemId(),
				actual.GetInode(),
				expected.GetFilesystemId(),
				expected.GetInode(),
			),
		}
	}
	return nil
}

func makePinned(
	pin *dirPin,
	adoption *agentv1.FilesystemAdoption,
	capacityBytes int64,
	b *Backend,
) (backend.PinnedFilesystem, error) {
	if pin == nil {
		return nil, errors.New("directory backend: nil filesystem pin")
	}
	return &pinnedFilesystem{
		fd:            pin.fd,
		mountSource:   pin.mountSource,
		adoption:      adoption,
		capacityBytes: capacityBytes,
		backend:       b,
	}, nil
}

type pinnedFilesystem struct {
	fd            int
	mountSource   string
	adoption      *agentv1.FilesystemAdoption
	capacityBytes int64
	backend       *Backend
}

// Adoption returns the validated native filesystem descriptor.
func (p *pinnedFilesystem) Adoption() *agentv1.FilesystemAdoption { return p.adoption }

// CapacityBytes returns the exact effective project-quota bound.
func (p *pinnedFilesystem) CapacityBytes() int64 { return p.capacityBytes }

// MountSource returns the operation-scoped proc-fd mount source.
func (p *pinnedFilesystem) MountSource() string { return p.mountSource }

// VerifyMount confirms that the target mount still matches the pinned source.
func (p *pinnedFilesystem) VerifyMount(ctx context.Context, targetPath string) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("verify target %q: %w", targetPath, err)
	}
	target := filepath.Clean(targetPath)
	if !filepath.IsAbs(target) {
		return fmt.Errorf("verify target %q: target path is not absolute", targetPath)
	}
	return p.verifyPinnedTarget(ctx, targetPath, target)
}

func (p *pinnedFilesystem) verifyPinnedTarget(ctx context.Context, targetPath, target string) error {
	var source, dst unix.Stat_t
	err := unix.Fstat(p.fd, &source)
	if err != nil {
		return fmt.Errorf("verify pinned source: %w", err)
	}
	err = unix.Stat(target, &dst)
	if err != nil {
		return fmt.Errorf("verify target %q: %w", targetPath, err)
	}
	if source.Dev != dst.Dev || source.Ino != dst.Ino {
		return fmt.Errorf("verify target %q: bind target identity differs from pinned source", targetPath)
	}
	mount, err := findMount(
		p.backend.mountInfoPath,
		target,
		unix.Major(dst.Dev),
		unix.Minor(dst.Dev),
	)
	if err != nil {
		return fmt.Errorf("verify target %q: %w", targetPath, err)
	}
	if filepath.Clean(mount.mountPoint) != target {
		return fmt.Errorf("verify target %q: target is not a mountpoint", targetPath)
	}
	err = rejectSubmounts(p.backend.mountInfoPath, target, target)
	if err != nil {
		return fmt.Errorf("verify target %q: %w", targetPath, err)
	}
	err = ctx.Err()
	if err != nil {
		return fmt.Errorf("verify target %q: %w", targetPath, err)
	}
	project, inherit, err := readProject(p.fd)
	if err != nil || project != p.adoption.GetProjectId() || !inherit {
		return fmt.Errorf("verify target %q: project scope changed", targetPath)
	}
	quota, err := readProjectQuota(mount, project, "")
	if err != nil {
		return fmt.Errorf("verify target %q: %w", targetPath, err)
	}
	if quota.capacity != p.capacityBytes {
		return fmt.Errorf("verify target %q: quota bound changed from %d to %d", targetPath, p.capacityBytes, quota.capacity)
	}
	return nil
}

// Close releases the operation-scoped source descriptor.
func (p *pinnedFilesystem) Close() error {
	if p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	if err != nil {
		return fmt.Errorf("close pinned directory: %w", err)
	}
	return nil
}

func capacity(ctx context.Context, path string) (totalBytes, availableBytes int64, retErr error) {
	err := ctx.Err()
	if err != nil {
		return 0, 0, fmt.Errorf("directory capacity %q: %w", path, err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("open directory root %q: %w", path, err)
	}
	defer func() {
		closeErr := unix.Close(fd)
		if closeErr != nil {
			totalBytes = 0
			availableBytes = 0
			retErr = errors.Join(retErr, fmt.Errorf("close directory root %q: %w", path, closeErr))
		}
	}()
	var st unix.Statfs_t
	err = unix.Fstatfs(fd, &st)
	if err != nil {
		return 0, 0, fmt.Errorf("statfs directory root %q: %w", path, err)
	}
	if st.Bsize <= 0 {
		return 0, 0, errors.New("statfs returned invalid block size")
	}
	total, err := checkedFSBytes(st.Blocks, uint64(st.Bsize))
	if err != nil {
		return 0, 0, err
	}
	if total <= 0 {
		return 0, 0, errors.New("filesystem capacity is zero")
	}
	avail, err := checkedFSBytes(st.Bavail, uint64(st.Bsize))
	if err != nil {
		return 0, 0, err
	}
	return total, avail, nil
}

func checkedFSBytes(blocks, size uint64) (int64, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if size == 0 || blocks > maxInt64/size {
		return 0, errors.New("filesystem capacity conversion overflows")
	}
	bytes := blocks * size
	if bytes > maxInt64 {
		return 0, errors.New("filesystem capacity conversion overflows")
	}
	return int64(bytes), nil
}
