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
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"golang.org/x/sys/unix"
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

type fsxattr struct {
	xflags     uint32
	extsize    uint32
	nextents   uint32
	projid     uint32
	cowextsize uint32
	pad        [8]byte
}

type ifDqblk struct {
	bhardlimit uint64
	bsoftlimit uint64
	curspace   uint64
	ihardlimit uint64
	isoftlimit uint64
	curinodes  uint64
	btime      uint64
	itime      uint64
	valid      uint32
	pad        uint32
}

type fsDiskQuota struct {
	version      int8
	flags        int8
	fieldmask    uint16
	id           uint32
	blkHardlimit uint64
	blkSoftlimit uint64
	inoHardlimit uint64
	inoSoftlimit uint64
	bcount       uint64
	icount       uint64
	itimer       int32
	btimer       int32
	iwarns       uint16
	bwarns       uint16
	padding2     int32
	rtbHardlimit uint64
	rtbSoftlimit uint64
	rtbcount     uint64
	rtbtimer     int32
	rtbwarns     uint16
	padding3     int16
	padding4     [8]byte
}
type fsQfilestatV struct {
	ino      uint64
	nblks    uint64
	nextents uint32
	pad      uint32
}

// fsQuotaStatV mirrors struct fs_quota_statv from linux/dqblk_xfs.h.
type fsQuotaStatV struct {
	version      int8
	pad1         uint8
	flags        uint16
	incoreDquots uint32
	userQuota    fsQfilestatV
	groupQuota   fsQfilestatV
	projectQuota fsQfilestatV
	btimeLimit   int32
	itimeLimit   int32
	rtbtimeLimit int32
	bwarnLimit   uint16
	iwarnLimit   uint16
	rtbWarnLimit uint16
	pad3         uint16
	pad4         uint32
	pad2         [7]uint64
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
	if source == "" || !filepath.IsAbs(source) {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonMissing,
			Detail: fmt.Sprintf("source %q is not absolute", source),
		}
	}
	rootHost, err := filepath.Abs(filepath.Clean(b.hostRoot))
	if err != nil {
		return nil, nil, fmt.Errorf("directory root: %w", err)
	}
	rootSys, err := filepath.EvalSymlinks(b.sysPath(rootHost))
	if err != nil {
		return nil, nil, fmt.Errorf("directory root %q: %w", rootHost, err)
	}
	sourceHost, err := filepath.Abs(filepath.Clean(source))
	if err != nil {
		return nil, nil, fmt.Errorf("directory source: %w", err)
	}
	sourceSys, err := filepath.EvalSymlinks(b.sysPath(sourceHost))
	if err != nil {
		return nil, nil, &backend.ImportRefusedError{Reason: reasonMissing, Detail: fmt.Sprintf("source %q: %v", source, err)}
	}
	if !within(rootSys, sourceSys) {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonLayout,
			Detail: fmt.Sprintf(
				"canonical source %q is outside configured host root %q",
				sourceSys,
				rootSys,
			),
		}
	}
	canonicalHost := sourceSys
	if b.hostRootPrefix != "" {
		prefixSys, prefixErr := filepath.EvalSymlinks(b.hostRootPrefix)
		if prefixErr != nil {
			return nil, nil, fmt.Errorf("directory host root prefix %q: %w", b.hostRootPrefix, prefixErr)
		}
		if !within(prefixSys, sourceSys) {
			return nil, nil, fmt.Errorf("directory source %q escaped host prefix %q", sourceSys, prefixSys)
		}
		canonicalHost = "/" + strings.TrimPrefix(strings.TrimPrefix(sourceSys, prefixSys), string(filepath.Separator))
		if canonicalHost == "//" {
			canonicalHost = "/"
		}
	}
	fd, err := openPinnedDir(rootSys, sourceSys)
	if err != nil {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonMissing,
			Detail: fmt.Sprintf("secure open %q: %v", canonicalHost, err),
		}
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := unix.Close(fd); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("directory source %q: close inspection pin: %w", canonicalHost, closeErr))
				inspection = nil
			}
		}
	}()
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	if err != nil {
		return nil, nil, fmt.Errorf("directory source %q: fstat: %w", canonicalHost, err)
	}
	var pathStat unix.Stat_t
	err = unix.Stat(sourceSys, &pathStat)
	if err != nil {
		return nil, nil, fmt.Errorf("directory source %q: stat after secure open: %w", canonicalHost, err)
	}
	if st.Dev != pathStat.Dev || st.Ino != pathStat.Ino {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonIdentity,
			Detail: fmt.Sprintf("source %q changed while being pinned", canonicalHost),
		}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, nil, &backend.ImportRefusedError{Reason: reasonWrongType, Detail: "source is not a directory"}
	}
	mount, err := findMount(
		b.mountInfoPath,
		sourceSys,
		uint32(unix.Major(uint64(st.Dev))),
		uint32(unix.Minor(uint64(st.Dev))),
	)
	if err != nil {
		return nil, nil, err
	}
	err = rejectSubmounts(b.mountInfoPath, sourceSys, mount.mountPoint)
	if err != nil {
		return nil, nil, err
	}
	fsType := strings.ToLower(mount.fsType)
	if fsType != "ext4" && fsType != "xfs" {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonWrongType,
			Detail: fmt.Sprintf(
				"filesystem type %q is unsupported; only ext4 and xfs are supported",
				mount.fsType,
			),
		}
	}
	fsUUID, err := readNativeUUID(mount, b.hostRootPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("directory source %q: native %s UUID: %w", canonicalHost, fsType, err)
	}
	project, inherit, err := readProject(fd)
	if err != nil {
		return nil, nil, fmt.Errorf("directory source %q: project attributes: %w", canonicalHost, err)
	}
	if project == 0 || !inherit {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: fmt.Sprintf("project %d is zero or not inheritable", project),
		}
	}
	quota, err := readProjectQuota(mount, project, b.hostRootPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("directory source %q: project quota: %w", canonicalHost, err)
	}
	if quota.capacity <= 0 {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: "project quota is unbounded or has no hard limit",
		}
	}
	if quota.capacity != requiredBytes {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonTooSmall,
			Detail: fmt.Sprintf(
				"effective project hard bound is %d bytes, requested %d",
				quota.capacity,
				requiredBytes,
			),
		}
	}
	if quota.inodes == 0 {
		return nil, nil, &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: "project quota reports zero or unknown inode scope",
		}
	}
	if expected == nil {
		err = verifyProjectTree(ctx, fd, project, quota.inodes)
		if err != nil {
			return nil, nil, err
		}
	}
	adoption := &agentv1.FilesystemAdoption{
		Kind:            adoptionKind,
		CanonicalSource: filepath.Clean(canonicalHost),
		ResourceId:      fsUUID + ":" + strconv.FormatUint(uint64(st.Ino), 10),
		FilesystemType:  fsType,
		FilesystemId:    fsUUID,
		Inode:           uint64(st.Ino),
		ProjectId:       project,
	}
	if expected != nil {
		err = equalAdoption(adoption, expected)
		if err != nil {
			return nil, nil, err
		}
	}
	inspection = &backend.ImportInspection{Filesystem: adoption, CapacityBytes: quota.capacity}
	if !pin {
		return inspection, nil, nil
	}
	closed = true
	return inspection, &dirPin{fd: fd, mountSource: fmt.Sprintf("/proc/self/fd/%d", fd)}, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func openPinnedDir(root, source string) (int, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, err
	}
	rel, err := filepath.Rel(root, source)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		closeErr := unix.Close(rootFD)
		if err == nil {
			err = syscall.EPERM
		}
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close directory root pin: %w", closeErr))
		}
		return -1, err
	}
	if rel == "." {
		return rootFD, nil
	}
	current := rootFD
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			closeErr := unix.Close(current)
			if closeErr != nil {
				return -1, errors.Join(syscall.EPERM, fmt.Errorf("close directory pin: %w", closeErr))
			}
			return -1, syscall.EPERM
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
			return -1, openErr
		}
		if closeErr != nil {
			nextCloseErr := unix.Close(next)
			closeErr = fmt.Errorf("close directory pin: %w", closeErr)
			if nextCloseErr != nil {
				closeErr = errors.Join(closeErr, fmt.Errorf("close next directory pin: %w", nextCloseErr))
			}
			return -1, closeErr
		}
		current = next
	}
	return current, nil
}

func findMount(path, target string, major, minor uint32) (best mountRecord, retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return mountRecord{}, fmt.Errorf("open mountinfo %q: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			best = mountRecord{}
			retErr = errors.Join(retErr, fmt.Errorf("close mountinfo %q: %w", path, closeErr))
		}
	}()
	bestLen := -1
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		sep := -1
		for i, field := range fields {
			if field == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) <= sep+2 {
			continue
		}
		dev := strings.Split(fields[2], ":")
		if len(dev) != 2 {
			continue
		}
		mj, e1 := strconv.ParseUint(dev[0], 10, 32)
		mn, e2 := strconv.ParseUint(dev[1], 10, 32)
		if e1 != nil || e2 != nil || uint32(mj) != major || uint32(mn) != minor {
			continue
		}
		mp := decodeMountField(fields[4])
		if !withinOrEqual(mp, target) {
			continue
		}
		if len(mp) > bestLen {
			bestLen = len(mp)
			options := fields[5]
			if len(fields) > sep+3 {
				options += "," + fields[sep+3]
			}
			best = mountRecord{
				mountPoint: mp,
				major:      uint32(mj),
				minor:      uint32(mn),
				fsType:     fields[sep+1],
				source:     decodeMountField(fields[sep+2]),
				options:    options,
			}
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

func rejectSubmounts(path, source, rootMount string) (retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open mountinfo for submount validation: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
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
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil && prefix != "" {
		firstErr := err
		fd, err = unix.Open(filepath.Join(prefix, source), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			err = errors.Join(firstErr, fmt.Errorf("open prefixed mount source: %w", err))
		}
	}
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := unix.Close(fd); closeErr != nil {
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
	if uint32(unix.Major(uint64(ds.Rdev))) != m.major || uint32(unix.Minor(uint64(ds.Rdev))) != m.minor {
		return "", fmt.Errorf(
			"mount source %q device identity does not match mountinfo %d:%d",
			source,
			m.major,
			m.minor,
		)
	}
	if strings.EqualFold(m.fsType, "ext4") {
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
	if strings.EqualFold(m.fsType, "xfs") {
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
	return "", fmt.Errorf("unsupported filesystem type %q", m.fsType)
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

func readProject(fd int) (uint32, bool, error) {
	var attr fsxattr
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(fsIoctlGetXattr), uintptr(unsafe.Pointer(&attr)))
	if errno != 0 {
		return 0, false, errno
	}
	return attr.projid, attr.xflags&fsXFlagProjInherit != 0, nil
}

type quotaResult struct {
	capacity int64
	inodes   uint64
}

func readQuotaStatus(device string) (fsQuotaStatV, error) {
	var status fsQuotaStatV
	status.version = fsQstatVVersion1
	err := quotaCtl(qcmdGetQuotaStatV, device, 0, unsafe.Pointer(&status))
	if err != nil {
		return fsQuotaStatV{}, fmt.Errorf("read project quota status: %w", err)
	}
	if status.version != fsQstatVVersion1 {
		return fsQuotaStatV{}, fmt.Errorf("unsupported project quota status version %d", status.version)
	}
	if status.flags&(fsQuotaPDQAcct|fsQuotaPDQEnfd) != (fsQuotaPDQAcct | fsQuotaPDQEnfd) {
		return fsQuotaStatV{}, errors.New("project quota accounting or enforcement is disabled")
	}
	return status, nil
}

func readProjectQuota(m mountRecord, project uint32, prefix string) (quotaResult, error) {
	device := m.source
	if prefix != "" && filepath.IsAbs(device) {
		candidate := filepath.Join(prefix, device)
		_, err := os.Stat(candidate)
		if err == nil {
			device = candidate
		} else if !errors.Is(err, os.ErrNotExist) {
			return quotaResult{}, fmt.Errorf("stat quota device %q: %w", candidate, err)
		}
	}
	if device == "" {
		return quotaResult{}, errors.New("mount has no quota device")
	}
	if strings.EqualFold(m.fsType, "ext4") {
		if !hasMountOption(m.options, fsQuotaProjectOption) {
			return quotaResult{}, errors.New("ext4 project quota enforcement is not enabled")
		}
		_, statusErr := readQuotaStatus(device)
		if statusErr != nil {
			return quotaResult{}, fmt.Errorf("read ext4 project quota status: %w", statusErr)
		}
		var q ifDqblk
		err := quotaCtl(qcmdExt4GetQuota, device, uintptr(project), unsafe.Pointer(&q))
		if err != nil {
			return quotaResult{}, err
		}
		if q.valid&qifBHard == 0 || q.valid&qifInodes == 0 {
			return quotaResult{}, errors.New("project hard-limit or inode-usage fields are not valid")
		}
		capacity, err := checkedQuotaBytes(q.bhardlimit, 10)
		return quotaResult{capacity: capacity, inodes: q.curinodes}, err
	}
	if !strings.EqualFold(m.fsType, "xfs") {
		return quotaResult{}, fmt.Errorf("unsupported quota filesystem %q", m.fsType)
	}
	_, statusErr := readQuotaStatus(device)
	if statusErr != nil {
		return quotaResult{}, fmt.Errorf("read XFS project quota status: %w", statusErr)
	}
	var q fsDiskQuota
	err := quotaCtl(qcmdXFSGetQuota, device, uintptr(project), unsafe.Pointer(&q))
	if err != nil {
		return quotaResult{}, err
	}
	if q.flags&fsProjectQuota == 0 {
		return quotaResult{}, errors.New("XFS project quota flag is absent")
	}
	capacity, err := checkedQuotaBytes(q.blkHardlimit, 9)
	return quotaResult{capacity: capacity, inodes: q.icount}, err
}

func hasMountOption(options, want string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

func quotaCtl(command uint32, device string, id uintptr, result unsafe.Pointer) error {
	dev, err := syscall.BytePtrFromString(device)
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_QUOTACTL,
		uintptr(command),
		uintptr(unsafe.Pointer(dev)),
		id,
		uintptr(result),
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func checkedQuotaBytes(blocks uint64, shift uint) (int64, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if blocks == 0 || shift >= 63 || blocks > maxInt64>>shift {
		return 0, errors.New("quota hard-limit conversion overflows or is unbounded")
	}
	return int64(blocks << shift), nil
}

type projectWalk struct {
	ctx       context.Context
	project   uint32
	count     uint64
	multilink map[[2]uint64]*linkCount
}

type linkCount struct {
	total uint64
	seen  uint64
}

func verifyProjectTree(ctx context.Context, rootFD int, project uint32, quotaInodes uint64) error {
	w := &projectWalk{ctx: ctx, project: project, multilink: make(map[[2]uint64]*linkCount)}
	var root unix.Stat_t
	err := unix.Fstat(rootFD, &root)
	if err != nil {
		return err
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
	if quotaInodes != 0 && w.count != quotaInodes {
		if w.count < quotaInodes {
			return &backend.ImportRefusedError{
				Reason: reasonQuota,
				Detail: fmt.Sprintf(
					"project %d includes %d inodes outside source (source has %d of %d)",
					project,
					quotaInodes-w.count,
					w.count,
					quotaInodes,
				),
			}
		}
		return &backend.ImportRefusedError{
			Reason: reasonQuota,
			Detail: fmt.Sprintf(
				"project %d quota inode count %d is below source unique inode count %d",
				project,
				quotaInodes,
				w.count,
			),
		}
	}
	return nil
}

func (w *projectWalk) visit(fd int, st unix.Stat_t, isDir bool, label string) error {
	err := w.ctx.Err()
	if err != nil {
		return fmt.Errorf("project tree %q: %w", label, err)
	}
	pid, inherit, err := readProject(fd)
	if err != nil {
		return fmt.Errorf("project attributes %q: %w", label, err)
	}
	if pid != w.project {
		return fmt.Errorf("project scope mismatch at %q: got %d want %d", label, pid, w.project)
	}
	if isDir && !inherit {
		return fmt.Errorf("directory %q lacks project inheritance", label)
	}
	w.record(st)
	if isDir {
		return w.walkDir(fd, label)
	}
	return nil
}

func (w *projectWalk) record(st unix.Stat_t) {
	key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Nlink > 1 {
		links := w.multilink[key]
		if links == nil {
			links = &linkCount{total: uint64(st.Nlink)}
			w.multilink[key] = links
			w.count++
		}
		links.seen++
		return
	}
	w.count++
}

func (w *projectWalk) walkDir(fd int, label string) error {
	var buf [64 * 1024]byte
	for {
		err := w.ctx.Err()
		if err != nil {
			return fmt.Errorf("walk directory %q: %w", label, err)
		}
		n, err := unix.Getdents(fd, buf[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("read directory %q: %w", label, err)
		}
		if n == 0 {
			return nil
		}
		for offset := 0; offset < n; {
			err = w.ctx.Err()
			if err != nil {
				return fmt.Errorf("walk directory %q: %w", label, err)
			}
			if n-offset < 19 {
				return errors.New("short linux directory entry")
			}
			recordLen := int(binary.LittleEndian.Uint16(buf[offset+16 : offset+18]))
			if recordLen < 19 || offset+recordLen > n {
				return errors.New("invalid linux directory entry length")
			}
			nameBytes := buf[offset+19 : offset+recordLen]
			end := 0
			for end < len(nameBytes) && nameBytes[end] != 0 {
				end++
			}
			if end == 0 {
				offset += recordLen
				continue
			}
			name := string(nameBytes[:end])
			offset += recordLen
			if name == "." || name == ".." {
				continue
			}
			err = w.visitEntry(fd, name)
			if err != nil {
				return fmt.Errorf("inspect entry %q: %w", name, err)
			}
		}
	}
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
		if closeErr := unix.Close(fd); closeErr != nil {
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
		w.record(actual)
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
	var source, dst unix.Stat_t
	err = unix.Fstat(p.fd, &source)
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
		uint32(unix.Major(uint64(dst.Dev))),
		uint32(unix.Minor(uint64(dst.Dev))),
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

func capacity(ctx context.Context, path string) (totalBytes int64, availableBytes int64, retErr error) {
	err := ctx.Err()
	if err != nil {
		return 0, 0, fmt.Errorf("directory capacity %q: %w", path, err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("open directory root %q: %w", path, err)
	}
	defer func() {
		if closeErr := unix.Close(fd); closeErr != nil {
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
	return int64(blocks * size), nil
}
