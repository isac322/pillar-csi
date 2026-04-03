package e2e

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/names"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/ports"
)

const tcTempRoot = "/tmp"

var globalScopeSeq atomic.Uint64
var globalSyntheticPortSeq atomic.Uint32

// TestCaseScope owns every mutable identifier and filesystem location that a
// single documented test case is allowed to touch.
//
// The scope is intentionally small and opinionated:
//   - all temp roots are created under /tmp
//   - all names are DNS-safe so they can be used for namespaces, pods, PVCs,
//     CRs, and backend object identifiers
//   - backend objects and loopback ports are cached per logical label inside a
//     single TC, but never shared across TCs
//
// Derived namespace lifecycle:
//
//	DerivedNamespace is the deterministic Kubernetes-safe namespace name for
//	this TC, computed from names.Namespace(tcID). It is created as a
//	filesystem isolation directory under RootDir/namespaces/ in
//	NewTestCaseScope (the BeforeEach phase) and removed automatically when
//	Close() is called (the AfterEach/DeferCleanup phase), so no two TCs ever
//	share namespace-scoped mutable state.
type TestCaseScope struct {
	TCID             string
	RootDir          string
	ScopeTag         string
	DerivedNamespace string // deterministic K8s-safe namespace name from names.Namespace(tcID)

	mu             sync.Mutex
	namespaceDir   string // filesystem path: RootDir/namespaces/DerivedNamespace
	backendObjects map[string]*BackendObject
	portLeases     map[string]*PortLease
	portAllocs     map[string]*ports.Allocation     // registry-backed typed allocations
	iscsiRanges    map[string]*ports.ISCSIPortRange // range-based iSCSI port allocations; see ReserveISCSIPortRange
	resources      map[string]*trackedResource
	resourceOrder  []string
	closed         bool
}

// BackendObject represents a TC-private backend fixture such as a ZFS dataset,
// LVM LV, fake configfs tree, or export root.
type BackendObject struct {
	TCID     string
	ScopeTag string
	Kind     string
	Label    string
	Name     string
	RootDir  string
}

// Path resolves a path below the backend object's private root.
func (o *BackendObject) Path(parts ...string) string {
	clean := []string{o.RootDir}
	for _, part := range parts {
		clean = append(clean, pathToken(part))
	}
	return filepath.Join(clean...)
}

// PortLease pins a loopback TCP port to a single test-case scope until the
// scope is closed.
type PortLease struct {
	TCID      string
	ScopeTag  string
	Label     string
	Host      string
	Port      int
	Addr      string
	Synthetic bool

	listener net.Listener
}

// Close releases the reserved port.
func (l *PortLease) Close() error {
	if l == nil || l.listener == nil {
		return nil
	}
	err := l.listener.Close()
	l.listener = nil
	return err
}

// NewTestCaseScope creates a fresh TC isolation domain rooted under /tmp.
//
// Namespace lifecycle (BeforeEach phase):
//
//	NewTestCaseScope computes names.Namespace(tcID) to obtain the TC's
//	deterministic Kubernetes-safe namespace name and immediately creates
//	the corresponding filesystem isolation directory at
//	RootDir/namespaces/<derived-namespace>. This directory is the TC's
//	private namespace root — test bodies write all namespace-scoped
//	artifacts there. The directory is removed by Close() (the
//	AfterEach/DeferCleanup phase) regardless of whether the spec passes or
//	fails, ensuring no leaked state crosses TC boundaries.
func NewTestCaseScope(tcID string) (*TestCaseScope, error) {
	if strings.TrimSpace(tcID) == "" {
		return nil, errors.New("tcID is required")
	}

	tcSlug := dnsLabelToken(tcID)
	seq := globalScopeSeq.Add(1)
	rootDir, err := os.MkdirTemp(
		tcTempRoot,
		fmt.Sprintf("pillar-csi-%s-p%d-s%d-", tcSlug, os.Getpid(), seq),
	)
	if err != nil {
		return nil, fmt.Errorf("create TC root for %q: %w", tcID, err)
	}

	// Derive the deterministic Kubernetes-safe namespace name from the TC ID
	// and create its filesystem isolation directory under the scope root.
	// This integrates names.Namespace() into the BeforeEach lifecycle: the
	// directory exists for the entire duration of the spec body and is
	// removed atomically with the rest of the scope root in Close().
	derivedNamespace := names.Namespace(tcID)
	nsDir := filepath.Join(rootDir, "namespaces", derivedNamespace)
	if err := os.MkdirAll(nsDir, 0o755); err != nil {
		_ = os.RemoveAll(rootDir)
		return nil, fmt.Errorf("create namespace dir for %q: %w", tcID, err)
	}

	scopeTag := dnsLabel("tc", tcSlug, filepath.Base(rootDir))

	// Register with the isolation checker so the directory is tracked as an
	// active scope. The checker's AfterEach hook will flag the directory as
	// "orphaned" if Close() (or CloseBackground) is not called, or if Close()
	// fails to remove it.
	framework.RegisterActiveScope(rootDir, tcID, scopeTag)

	return &TestCaseScope{
		TCID:             tcID,
		RootDir:          rootDir,
		ScopeTag:         scopeTag,
		DerivedNamespace: derivedNamespace,
		namespaceDir:     nsDir,
		backendObjects:   make(map[string]*BackendObject),
		portLeases:       make(map[string]*PortLease),
		portAllocs:       make(map[string]*ports.Allocation),
		iscsiRanges:      make(map[string]*ports.ISCSIPortRange),
		resources:        make(map[string]*trackedResource),
	}, nil
}

// Name returns a DNS-safe identifier namespaced to this TC.
func (s *TestCaseScope) Name(parts ...string) string {
	return dnsLabel(append([]string{s.ScopeTag}, parts...)...)
}

// Namespace returns a TC-private namespace name.
func (s *TestCaseScope) Namespace(label string) string {
	return s.Name("ns", label)
}

// NamespaceDir returns the filesystem isolation directory for the derived
// namespace. This directory is created by NewTestCaseScope (BeforeEach) at
// RootDir/namespaces/DerivedNamespace and removed by Close() (AfterEach /
// DeferCleanup), so it is available exactly during the spec body lifetime.
//
// Callers may write any namespace-scoped in-process artifacts here; the
// entire subtree is swept away by Close() regardless of spec outcome.
func (s *TestCaseScope) NamespaceDir() string {
	if s == nil {
		return ""
	}
	return s.namespaceDir
}

// Path resolves a filesystem path under the TC's private /tmp root.
func (s *TestCaseScope) Path(parts ...string) string {
	clean := []string{s.RootDir}
	for _, part := range parts {
		clean = append(clean, pathToken(part))
	}
	return filepath.Join(clean...)
}

// TempDir returns a stable, TC-private temp directory for the given label.
func (s *TestCaseScope) TempDir(label string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("test case scope is closed")
	}

	dir := s.Path("tmp", s.Name("tmp", label))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create temp dir %q for %s: %w", dir, s.TCID, err)
	}
	return dir, nil
}

// RecreateTempDir removes any prior contents for the labeled temp directory and
// recreates the baseline directory from scratch.
func (s *TestCaseScope) RecreateTempDir(label string) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("test case scope is closed")
	}
	dir := s.Path("tmp", s.Name("tmp", label))
	s.mu.Unlock()

	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("reset temp dir %q for %s: %w", dir, s.TCID, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create temp dir %q for %s: %w", dir, s.TCID, err)
	}
	return dir, nil
}

// KubeconfigPath returns a TC-private kubeconfig path under /tmp.
func (s *TestCaseScope) KubeconfigPath(label string) string {
	return filepath.Join(s.Path("kubeconfig"), pathToken(s.Name("kcfg", label))+".yaml")
}

// RecreateKubeconfigPath removes any existing kubeconfig file for the label and
// ensures its parent directory exists.
func (s *TestCaseScope) RecreateKubeconfigPath(label string) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("test case scope is closed")
	}
	path := s.KubeconfigPath(label)
	s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create kubeconfig dir for %s/%s: %w", s.TCID, label, err)
	}
	if err := os.RemoveAll(path); err != nil {
		return "", fmt.Errorf("reset kubeconfig path %q for %s: %w", path, s.TCID, err)
	}
	return path, nil
}

// BackendObject returns a TC-private backend fixture handle for the given kind
// and logical label. Repeated calls within the same TC return the same fixture.
func (s *TestCaseScope) BackendObject(kind, label string) (*BackendObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("test case scope is closed")
	}

	key := pathToken(kind) + ":" + pathToken(label)
	if existing, ok := s.backendObjects[key]; ok {
		return existing, nil
	}

	name := s.Name("backend", kind, label)
	rootDir := s.Path("backend", pathToken(kind), name)
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("create backend fixture %q for %s: %w", name, s.TCID, err)
	}

	obj := &BackendObject{
		TCID:     s.TCID,
		ScopeTag: s.ScopeTag,
		Kind:     kind,
		Label:    label,
		Name:     name,
		RootDir:  rootDir,
	}
	s.backendObjects[key] = obj
	return obj, nil
}

// RecreateBackendObject removes any prior contents for the labeled backend
// fixture and recreates an empty root directory. The logical fixture identity
// remains stable inside the current TC scope.
func (s *TestCaseScope) RecreateBackendObject(kind, label string) (*BackendObject, error) {
	obj, err := s.BackendObject(kind, label)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(obj.RootDir); err != nil {
		return nil, fmt.Errorf("reset backend fixture %q for %s: %w", obj.Name, s.TCID, err)
	}
	if err := os.MkdirAll(obj.RootDir, 0o755); err != nil {
		return nil, fmt.Errorf("create backend fixture %q for %s: %w", obj.Name, s.TCID, err)
	}
	return obj, nil
}

// ReserveLoopbackPort reserves a unique loopback TCP port for this TC until
// the scope is closed.
func (s *TestCaseScope) ReserveLoopbackPort(label string) (*PortLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("test case scope is closed")
	}

	key := pathToken(label)
	if existing, ok := s.portLeases[key]; ok {
		return existing, nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			lease := newSyntheticPortLease(s, label)
			s.portLeases[key] = lease
			return lease, nil
		}
		return nil, fmt.Errorf("reserve loopback port for %s/%s: %w", s.TCID, label, err)
	}

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("parse loopback listener address for %s/%s: %w", s.TCID, label, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("parse loopback listener port for %s/%s: %w", s.TCID, label, err)
	}

	lease := &PortLease{
		TCID:     s.TCID,
		ScopeTag: s.ScopeTag,
		Label:    label,
		Host:     host,
		Port:     port,
		Addr:     listener.Addr().String(),
		listener: listener,
	}
	s.portLeases[key] = lease
	return lease, nil
}

// RecreateLoopbackPort closes any existing lease for the label and reserves a
// fresh loopback port for the next baseline.
func (s *TestCaseScope) RecreateLoopbackPort(label string) (*PortLease, error) {
	key := pathToken(label)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("test case scope is closed")
	}
	existing := s.portLeases[key]
	delete(s.portLeases, key)
	s.mu.Unlock()

	if err := existing.Close(); err != nil {
		return nil, fmt.Errorf("release loopback port for %s/%s: %w", s.TCID, label, err)
	}
	return s.ReserveLoopbackPort(label)
}

func newSyntheticPortLease(s *TestCaseScope, label string) *PortLease {
	port := 30000 + int(globalSyntheticPortSeq.Add(1))
	host := "127.0.0.1"
	return &PortLease{
		TCID:      s.TCID,
		ScopeTag:  s.ScopeTag,
		Label:     label,
		Host:      host,
		Port:      port,
		Addr:      net.JoinHostPort(host, strconv.Itoa(port)),
		Synthetic: true,
	}
}

// Close releases all TC-private resources and removes the /tmp root.
func (s *TestCaseScope) Close() error {
	var (
		leases    []*PortLease
		allocs    []*ports.Allocation
		resources []*trackedResource
		rootDir   string
	)

	return measureTimingPhaseErr(phaseTeardownTotal, func() error {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil
		}
		s.closed = true

		leases = make([]*PortLease, 0, len(s.portLeases))
		for _, lease := range s.portLeases {
			leases = append(leases, lease)
		}
		allocs = make([]*ports.Allocation, 0, len(s.portAllocs))
		for _, alloc := range s.portAllocs {
			allocs = append(allocs, alloc)
		}
		resources = make([]*trackedResource, 0, len(s.resourceOrder))
		for i := len(s.resourceOrder) - 1; i >= 0; i-- {
			key := s.resourceOrder[i]
			resource, ok := s.resources[key]
			if !ok {
				continue
			}
			resources = append(resources, resource)
		}
		rootDir = s.RootDir
		s.mu.Unlock()

		// Deregister from the isolation checker immediately after marking the
		// scope as closed. From this point on, if the root directory still
		// exists on disk (e.g. because the os.RemoveAll below fails), it will
		// be detected as an orphan by the next AfterEach isolation scan.
		framework.DeregisterActiveScope(rootDir)

		var errs []error
		if len(resources) > 0 {
			if err := measureTimingPhaseErr(phaseTeardownResources, func() error {
				return teardownTrackedResources(resources)
			}); err != nil {
				errs = append(errs, err)
			}
		}
		if len(leases) > 0 {
			if err := measureTimingPhaseErr(phaseTeardownPortLeases, func() error {
				var leaseErrs []error
				for _, lease := range leases {
					if err := lease.Close(); err != nil {
						leaseErrs = append(leaseErrs, err)
					}
				}
				return errors.Join(leaseErrs...)
			}); err != nil {
				errs = append(errs, err)
			}
		}
		// Release typed registry allocations (e.g. iSCSI target ports allocated
		// via ReserveISCSITargetPort / ReserveCSIGRPCPort).
		if len(allocs) > 0 {
			if err := measureTimingPhaseErr(phaseTeardownPortLeases, func() error {
				var allocErrs []error
				for _, alloc := range allocs {
					if err := alloc.Release(); err != nil {
						allocErrs = append(allocErrs, err)
					}
				}
				return errors.Join(allocErrs...)
			}); err != nil {
				errs = append(errs, err)
			}
		}
		if err := measureTimingPhaseErr(phaseTeardownRootDir, func() error {
			if err := os.RemoveAll(rootDir); err != nil {
				return fmt.Errorf("remove TC root %q: %w", rootDir, err)
			}
			return nil
		}); err != nil {
			errs = append(errs, err)
		}

		return errors.Join(errs...)
	})
}

func dnsLabel(parts ...string) string {
	tokens := make([]string, 0, len(parts))
	for _, part := range parts {
		tokens = append(tokens, dnsLabelToken(part))
	}

	label := strings.Join(tokens, "-")
	label = strings.Trim(label, "-")
	if label == "" {
		label = "x"
	}
	if len(label) <= 63 {
		return label
	}

	hash := shortHash(label)
	head := strings.Trim(label[:63-len(hash)-1], "-")
	if head == "" {
		head = "x"
	}
	return head + "-" + hash
}

func dnsLabelToken(input string) string {
	var b strings.Builder
	b.Grow(len(input))

	hyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(input)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			hyphen = false
			continue
		}
		if !hyphen {
			b.WriteByte('-')
			hyphen = true
		}
	}

	token := strings.Trim(b.String(), "-")
	if token == "" {
		return "x"
	}
	return token
}

func pathToken(input string) string {
	return dnsLabelToken(input)
}

func shortHash(input string) string {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(input))
	return fmt.Sprintf("%08x", hasher.Sum32())
}
