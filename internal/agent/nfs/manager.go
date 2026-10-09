// Package nfs manages only Pillar-owned kernel NFS exports and their recovery state.
package nfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Port is the fixed NFSv4 TCP listener port.
const Port int32 = 2049

const (
	rootVolumeID     = "::pillar-nfs-root::"
	nfsVersion       = "4.2"
	squashRoot       = "root"
	squashNone       = "none"
	squashAll        = "all"
	optionRootSquash = "root_squash"
	optionNoSquash   = "no_root_squash"
	operationTimeout = 30 * time.Second
)

// Config locates durable ownership and the advertised numeric server address.
type Config struct {
	StateDir       string
	BindAddress    string
	ExportRoot     string
	BeforeActivate func(context.Context, Export) error
}

// Export is the durable admission policy for one filesystem.
type Export struct {
	VolumeID    string   `json:"volumeID"`
	Path        string   `json:"path"`
	BindAddress string   `json:"bindAddress"`
	Version     string   `json:"version"`
	Squash      string   `json:"squash"`
	ReadOnly    bool     `json:"readOnly"`
	ACLEnabled  bool     `json:"aclEnabled"`
	Clients     []string `json:"clients,omitempty"`
	Active      bool     `json:"active"`
	// SourceKey and FenceUID are opaque durable ownership hints for adopted
	// filesystems. Both must be present for adoption validation; when both are
	// absent, legacy dynamically-created NFS exports retain their old path.
	SourceKey string `json:"sourceKey,omitempty"`
	FenceUID  string `json:"fenceUID,omitempty"`
}

// FSID survives dataset remounts, server restarts and device-number changes.
func FSID(volumeID string) string {
	sum := sha256.Sum256([]byte("pillar-csi/nfs/" + volumeID))
	s := hex.EncodeToString(sum[:16])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func safeExportPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != "/" && !strings.ContainsAny(path, " \t\r\n\x00:()\\")
}

func unicastAddress(address string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse NFS address %q: %w", address, err)
	}
	if ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
		return netip.Addr{}, errors.New("NFS address must be a unicast numeric IP without a zone")
	}
	return ip.Unmap(), nil
}

func (e Export) normalized() (Export, error) {
	if e.VolumeID == "" {
		return e, errors.New("NFS volume ID is required")
	}
	if !safeExportPath(e.Path) {
		return e, fmt.Errorf("unsafe NFS export path %q", e.Path)
	}
	addr, err := unicastAddress(e.BindAddress)
	if err != nil {
		return e, err
	}
	e.BindAddress = addr.String()
	err = e.normalizePolicy()
	if err != nil {
		return e, err
	}
	e.Clients, err = normalizeClients(e.Clients, addr)
	if err != nil {
		return e, err
	}
	if !e.ACLEnabled {
		e.Clients = nil
	}
	return e, nil
}

func (e *Export) normalizePolicy() error {
	if e.Version == "" {
		e.Version = nfsVersion
	}
	if e.Version != nfsVersion {
		return fmt.Errorf("NFS version %q is unsupported; require 4.2", e.Version)
	}
	if e.Squash == "" {
		e.Squash = squashRoot
	}
	switch e.Squash {
	case squashRoot, squashNone, squashAll:
		return nil
	default:
		return fmt.Errorf("invalid NFS squash %q", e.Squash)
	}
}

func normalizeClients(clients []string, bind netip.Addr) ([]string, error) {
	clients = slices.Clone(clients)
	for i, client := range clients {
		ip, err := unicastAddress(client)
		if err != nil {
			return nil, fmt.Errorf("NFS client %q: %w", client, err)
		}
		if ip.Is4() != bind.Is4() {
			return nil, fmt.Errorf("NFS client %q must be in the server address family", client)
		}
		clients[i] = ip.String()
	}
	slices.Sort(clients)
	return slices.Compact(clients), nil
}

func (e Export) clients() []string {
	if !e.Active {
		return nil
	}
	if !e.ACLEnabled {
		return []string{"*"}
	}
	return e.Clients
}

func (e Export) fsid() string {
	if e.VolumeID == rootVolumeID {
		return "0"
	}
	return FSID(e.VolumeID)
}

func (e Export) options() string {
	mode := "rw"
	if e.ReadOnly {
		mode = "ro"
	}
	squash := optionRootSquash
	switch e.Squash {
	case squashNone:
		squash = optionNoSquash
	case squashAll:
		squash = "all_squash"
	}
	traversal := ""
	if e.VolumeID != rootVolumeID && e.SourceKey != "" && e.FenceUID != "" {
		// Adopted datasets can be separate mounts beneath the pseudoroot.
		// Admit traversal through their own exports, never global crossmnt.
		traversal = ",nohide"
	}
	return mode + ",sync,no_subtree_check,secure,sec=sys,fsid=" + e.fsid() + "," + squash + traversal
}

type entry struct{ Path, Client, Options string }

type runtime interface {
	identity() (string, error)
	start(context.Context, *diskState, func() error, func(error)) error
	list(context.Context) ([]entry, error)
	validateExport(Export) error
	grant(context.Context, Export, string) error
	revoke(context.Context, string, string) error
	health() error
	close() error
}

type diskState struct {
	Schema       int               `json:"schema"`
	Identity     string            `json:"identity"`
	BindAddress  string            `json:"bindAddress"`
	ExportRoot   string            `json:"exportRoot"`
	TrackerPID   int               `json:"trackerPID,omitempty"`
	TrackerStart string            `json:"trackerStart,omitempty"`
	MountdPID    int               `json:"mountdPID,omitempty"`
	MountdStart  string            `json:"mountdStart,omitempty"`
	Desired      map[string]Export `json:"desired"`
	// Ledger is a write-ahead ownership record. It includes grants that may
	// have reached etab before a crash, not only acknowledged exports.
	Ledger []Export `json:"ledger"`
}

// Manager serializes export changes with daemon lifecycle and durable ownership.
type Manager struct {
	mu           sync.Mutex
	config       Config
	runtime      runtime
	bind         netip.Addr
	state        diskState
	lock         *os.File
	started      bool
	closed       bool
	failure      error
	exportErrors map[string]error
	closeErr     error
}

// NewManager validates the dedicated ownership directory and numeric listener.
func NewManager(config Config) (*Manager, error) {
	if config.StateDir == "" || !filepath.IsAbs(config.StateDir) {
		return nil, errors.New("absolute NFS state directory required")
	}
	if config.ExportRoot == "" {
		config.ExportRoot = filepath.Join(filepath.Dir(config.StateDir), "datasets")
	}
	if !safeExportPath(config.ExportRoot) {
		return nil, errors.New("absolute safe dedicated NFS export root required")
	}
	addr, err := unicastAddress(config.BindAddress)
	if err != nil {
		return nil, err
	}
	config.BindAddress = addr.String()
	m := &Manager{config: config, bind: addr}
	m.runtime = newRuntime(config)
	return m, nil
}

// ExportPath is the client-visible path relative to the dedicated fsid=0 root.
func (m *Manager) ExportPath(path string) (string, error) {
	rel, err := filepath.Rel(m.config.ExportRoot, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", errors.New("NFS dataset path must be a child of the dedicated export root")
	}
	return "/" + filepath.ToSlash(rel), nil
}

// ValidateClient accepts only exact numeric addresses in the listener family.
func (m *Manager) ValidateClient(client string) error {
	ip, err := unicastAddress(client)
	if err != nil {
		return fmt.Errorf("NFS initiator %q: %w", client, err)
	}
	if ip.Is4() != m.bind.Is4() {
		return fmt.Errorf("NFS initiator %q must be in the listener address family", client)
	}
	return nil
}

func (m *Manager) updateRoot() {
	root := Export{
		VolumeID: rootVolumeID, Path: m.config.ExportRoot, BindAddress: m.config.BindAddress,
		Version: nfsVersion, Squash: squashRoot, ReadOnly: true, ACLEnabled: true,
	}
	for id := range m.state.Desired {
		e := m.state.Desired[id]
		if id == rootVolumeID || !e.Active || m.exportErrors[id] != nil {
			continue
		}
		if !e.ACLEnabled {
			root.ACLEnabled = false
			root.Active = true
			root.Clients = nil
			break
		}
		root.Clients = append(root.Clients, e.Clients...)
	}
	if root.ACLEnabled {
		slices.Sort(root.Clients)
		root.Clients = slices.Compact(root.Clients)
		root.Active = len(root.Clients) != 0
	}
	m.state.Desired[rootVolumeID] = root
	if root.Active {
		m.state.Ledger = append(m.state.Ledger, root)
	}
}

func (m *Manager) validateOwned(e Export) error {
	if e.VolumeID == rootVolumeID {
		if e.Path != m.config.ExportRoot || !e.ReadOnly || e.Squash != squashRoot {
			return errors.New("invalid persisted NFS pseudoroot policy")
		}
		return nil
	}
	_, err := m.ExportPath(e.Path)
	return err
}

func (m *Manager) persist() (persistErr error) {
	data, err := json.Marshal(m.state)
	if err != nil {
		return fmt.Errorf("encode NFS ownership: %w", err)
	}
	f, err := os.CreateTemp(m.config.StateDir, ".owner-*")
	if err != nil {
		return fmt.Errorf("create NFS ownership transaction: %w", err)
	}
	name := f.Name()
	defer func() {
		removeErr := os.Remove(name)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			persistErr = fmt.Errorf("remove NFS ownership transaction: %w", errors.Join(persistErr, removeErr))
		}
	}()
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	err = errors.Join(writeErr, syncErr, closeErr)
	if err != nil {
		return fmt.Errorf("write NFS ownership transaction: %w", err)
	}
	err = os.Rename(name, filepath.Join(m.config.StateDir, "owner.json"))
	if err != nil {
		return fmt.Errorf("commit NFS ownership transaction: %w", err)
	}
	err = syncDirectory(m.config.StateDir)
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(m.config.StateDir))
}

func syncDirectory(path string) error {
	//nolint:gosec // G304: persist supplies only the configured ownership directory or its parent for fsync.
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open NFS ownership directory %q: %w", path, err)
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err != nil {
		return fmt.Errorf("sync NFS ownership directory %q: %w", path, err)
	}
	return nil
}

// Start establishes exclusive ownership before changing any kernel configuration.
func (m *Manager) Start(ctx context.Context) (startErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("start NFS manager: %w", err)
	}
	if m.closed {
		return errors.New("NFS manager is closed")
	}
	if m.started {
		return m.healthLocked()
	}
	if m.failure != nil {
		return m.failure
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	err = os.MkdirAll(m.config.StateDir, 0o700)
	if err != nil {
		return fmt.Errorf("create NFS ownership directory: %w", err)
	}
	m.lock, err = acquireLock(filepath.Join(m.config.StateDir, "owner.lock"))
	if err != nil {
		return err
	}
	defer func() {
		if !m.started {
			lockErr := releaseLock(m.lock)
			m.lock = nil
			if lockErr != nil {
				startErr = fmt.Errorf("release NFS startup ownership: %w", errors.Join(startErr, lockErr))
			}
		}
	}()
	return m.startLocked(opCtx)
}

func (m *Manager) startLocked(ctx context.Context) error {
	identity, err := m.runtime.identity()
	if err != nil {
		return err
	}
	err = m.loadState()
	if err != nil {
		return err
	}
	m.updateRoot()
	err = m.preflightActivation(ctx)
	if err != nil {
		return err
	}
	// start compares the old identity before replacing it: a previous boot
	// does not authorize adopting a listener in this boot.
	err = m.runtime.start(ctx, &m.state, m.persist, m.daemonFailed)
	if err != nil {
		return err
	}
	m.state.Identity = identity
	err = m.persist()
	if err != nil {
		return fmt.Errorf("persist NFS startup ownership: %w", errors.Join(err, m.runtime.close()))
	}
	m.started = true
	return m.apply(ctx)
}

func (m *Manager) loadState() error {
	data, err := os.ReadFile(filepath.Join(m.config.StateDir, "owner.json"))
	if errors.Is(err, os.ErrNotExist) {
		m.state = diskState{
			Schema: 1, BindAddress: m.config.BindAddress, ExportRoot: m.config.ExportRoot,
			Desired: map[string]Export{},
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read NFS ownership: %w", err)
	}
	err = json.Unmarshal(data, &m.state)
	if err != nil {
		return fmt.Errorf("corrupt NFS ownership: %w", err)
	}
	if m.state.Schema != 1 || m.state.BindAddress != m.config.BindAddress ||
		m.state.ExportRoot != m.config.ExportRoot || m.state.Desired == nil {
		return errors.New("NFS ownership configuration mismatch")
	}
	return m.validateState()
}

func (m *Manager) validateState() error {
	for id := range m.state.Desired {
		e := m.state.Desired[id]
		validated, err := m.validatePersisted(e)
		if err != nil {
			return err
		}
		if id != e.VolumeID {
			return errors.New("invalid persisted NFS export identity")
		}
		m.state.Desired[id] = validated
	}
	for i := range m.state.Ledger {
		validated, err := m.validatePersisted(m.state.Ledger[i])
		if err != nil {
			return err
		}
		m.state.Ledger[i] = validated
	}
	return nil
}

func (m *Manager) validatePersisted(e Export) (Export, error) {
	validated, err := e.normalized()
	if err != nil {
		return e, fmt.Errorf("invalid persisted NFS export: %w", err)
	}
	if validated.BindAddress != m.config.BindAddress {
		return e, errors.New("invalid persisted NFS listener address")
	}
	err = m.validateOwned(validated)
	return validated, err
}

func (m *Manager) apply(ctx context.Context) error {
	err := m.converge(ctx)
	if err == nil {
		return nil
	}
	return m.failClosed(ctx, err)
}

func (m *Manager) failClosed(ctx context.Context, err error) error {
	// Admission cleanup must survive request cancellation, but retain its values.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	m.failure = errors.Join(err, m.revokeOwned(cleanupCtx), m.runtime.close())
	slog.Error("NFS runtime unavailable after export-state failure", "error", m.failure)
	return m.failure
}

func (m *Manager) healthLocked() error {
	if m.failure != nil {
		return m.failure
	}
	if !m.started || m.closed {
		return errors.New("NFS runtime is not running")
	}
	return m.runtime.health()
}

// Health reports unavailable exports as well as failures of the shared runtime.
func (m *Manager) Health() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	errs := make([]error, 1, 1+len(m.exportErrors))
	errs[0] = m.healthLocked()
	for _, err := range m.exportErrors {
		errs = append(errs, err)
	}
	err := errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("check NFS health: %w", err)
	}
	return nil
}

func (m *Manager) daemonFailed(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if !m.started {
		m.failure = fmt.Errorf("NFS helper failed during initialization: %w", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	m.failure = errors.Join(fmt.Errorf("NFS helper failed: %w", err), m.revokeOwned(ctx), m.runtime.close())
	slog.Error("NFS runtime unavailable after helper failure", "error", m.failure)
}

func (m *Manager) revokeOwned(ctx context.Context) error {
	entries, err := m.runtime.list(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range entries {
		if m.owns(row) {
			errs = append(errs, m.runtime.revoke(ctx, row.Path, row.Client))
		}
	}
	err = errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("revoke owned NFS admissions: %w", err)
	}
	return nil
}

func (m *Manager) owns(row entry) bool {
	for i := range m.state.Ledger {
		e := &m.state.Ledger[i]
		if e.Path == row.Path && slices.Contains(e.clients(), row.Client) && optionValue(row.Options, "fsid") == e.fsid() {
			return true
		}
	}
	return false
}

// preflightActivation withdraws unsafe persisted admissions while the runtime
// is still stopped. This is required because etab can remain active across a
// daemon restart and runtime.start verifies existing rows before convergence.
func (m *Manager) preflightActivation(ctx context.Context) error {
	guarded := false
	for id := range m.state.Desired {
		e := m.state.Desired[id]
		if id != rootVolumeID && e.Active && (e.SourceKey != "" || e.FenceUID != "") {
			guarded = true
			break
		}
	}
	if !guarded {
		for i := range m.state.Ledger {
			e := &m.state.Ledger[i]
			if e.VolumeID != rootVolumeID && (e.SourceKey != "" || e.FenceUID != "") {
				guarded = true
				break
			}
		}
	}
	if !guarded {
		return nil
	}
	rows, err := m.runtime.list(ctx)
	if err != nil {
		return fmt.Errorf("read NFS admissions before adopted activation: %w", err)
	}
	wanted, err := m.desiredEntries(ctx)
	if err != nil {
		return err
	}
	err = m.revokeObsolete(ctx, rows, wanted)
	if err != nil {
		return fmt.Errorf("withdraw unsafe NFS admissions before activation: %w", err)
	}
	m.rebuildLedger()
	return m.persist()
}

func (m *Manager) rebuildLedger() {
	m.state.Ledger = m.state.Ledger[:0]
	for id := range m.state.Desired {
		e := m.state.Desired[id]
		if !e.Active || (id != rootVolumeID && m.exportErrors[id] != nil) {
			continue
		}
		m.state.Ledger = append(m.state.Ledger, e)
	}
}

func (m *Manager) converge(ctx context.Context) error {
	rows, err := m.runtime.list(ctx)
	if err != nil {
		return err
	}
	wanted, err := m.desiredEntries(ctx)
	if err != nil {
		return err
	}
	// desiredEntries narrows the pseudoroot to validated datasets. Commit its
	// ownership proof before any newly recovered root client can be granted.
	err = m.persist()
	if err != nil {
		return err
	}
	err = m.revokeObsolete(ctx, rows, wanted)
	if err != nil {
		return err
	}
	err = m.grantDesired(ctx, rows, wanted)
	if err != nil {
		return err
	}
	err = m.verifyAdmissions(ctx, wanted)
	if err != nil {
		return err
	}
	err = m.healthLocked()
	if err != nil {
		return err
	}
	m.rebuildLedger()
	err = m.persist()
	if err != nil {
		m.failure = err
	}
	return err
}

// beforeActivate validates opaque durable ownership hints before any kernel
// admission is granted. Exports without hints are legacy dynamic NFS state and
// deliberately retain their existing recovery behavior.
func (m *Manager) beforeActivate(ctx context.Context, e Export) error {
	if e.SourceKey == "" && e.FenceUID == "" {
		return nil
	}
	if e.SourceKey == "" || e.FenceUID == "" {
		return errors.New("incomplete adopted NFS ownership hint")
	}
	if m.config.BeforeActivate == nil {
		return errors.New("adopted NFS activation guard is unavailable")
	}
	guarded := e
	guarded.Clients = slices.Clone(e.Clients)
	err := m.config.BeforeActivate(ctx, guarded)
	if err != nil {
		return fmt.Errorf("validate adopted NFS export: %w", err)
	}
	return nil
}

func (m *Manager) desiredEntries(ctx context.Context) (map[[2]string]entry, error) {
	wanted := make(map[[2]string]entry)
	previous := m.exportErrors
	m.exportErrors = make(map[string]error)
	for id := range m.state.Desired {
		e := m.state.Desired[id]
		if id == rootVolumeID || !e.Active {
			continue
		}
		usable, err := m.validateActivation(ctx, e, previous)
		if err != nil {
			return nil, err
		}
		if !usable {
			continue
		}
		m.admitExport(e, previous, wanted)
	}
	err := m.rootEntries(wanted)
	if err != nil {
		return nil, err
	}
	return wanted, nil
}

// validateActivation checks one active export for this convergence pass. A
// shared mount-table read failure is fatal; an isolated failure is recorded
// in exportErrors and logged once per transition, and the export is skipped.
func (m *Manager) validateActivation(ctx context.Context, e Export, previous map[string]error) (bool, error) {
	err := m.runtime.validateExport(e)
	if err == nil {
		err = m.beforeActivate(ctx, e)
	}
	if err == nil {
		return true, nil
	}
	//nolint:errcheck // AsType's bool is checked; preserve the original error and its wrapping.
	if _, ok := errors.AsType[*runtimeValidationError](err); ok {
		return false, err
	}
	m.exportErrors[e.VolumeID] = fmt.Errorf("NFS volume %q unavailable: %w", e.VolumeID, err)
	if previous[e.VolumeID] == nil {
		slog.Error("NFS export unavailable; excluded from convergence", "volume", e.VolumeID, "path", e.Path, "error", err)
	}
	return false, nil
}

// admitExport re-establishes write-ahead ownership for a validated export:
// rebuildLedger drops exports while they are unavailable, so a recovered
// export must re-enter the Ledger before the caller persists and
// grantDesired admits it. Otherwise its rows are foreign to owns() and
// verifyAdmissions fails closed.
func (m *Manager) admitExport(e Export, previous map[string]error, wanted map[[2]string]entry) {
	if previous[e.VolumeID] != nil {
		slog.Info("NFS export recovered", "volume", e.VolumeID, "path", e.Path)
	}
	m.state.Ledger = append(m.state.Ledger, e)
	for _, client := range e.clients() {
		wanted[[2]string{e.Path, client}] = entry{e.Path, client, e.options()}
	}
}

// rootEntries narrows the pseudoroot to clients of exports validated in this
// pass and adds its wanted rows.
func (m *Manager) rootEntries(wanted map[[2]string]entry) error {
	m.updateRoot()
	root := m.state.Desired[rootVolumeID]
	if !root.Active {
		return nil
	}
	err := m.runtime.validateExport(root)
	if err != nil {
		return err
	}
	for _, client := range root.clients() {
		wanted[[2]string{root.Path, client}] = entry{root.Path, client, root.options()}
	}
	return nil
}

// A mount-table read failure is shared runtime failure, not a bad dataset.
type runtimeValidationError struct{ error }

func (m *Manager) foreignConflict(row entry) error {
	// A foreign path or fsid conflicts even when its admitted client differs.
	for i := range m.state.Ledger {
		e := &m.state.Ledger[i]
		if row.Path == e.Path || optionValue(row.Options, "fsid") == e.fsid() {
			return fmt.Errorf("foreign NFS export conflicts with volume %q", e.VolumeID)
		}
	}
	return nil
}

func (m *Manager) revokeObsolete(ctx context.Context, rows []entry, wanted map[[2]string]entry) error {
	for _, row := range rows {
		if !m.owns(row) {
			err := m.foreignConflict(row)
			if err != nil {
				return err
			}
			continue
		}
		want, exists := wanted[[2]string{row.Path, row.Client}]
		if exists && optionsMatch(row.Options, want.Options) {
			continue
		}
		err := m.runtime.revoke(ctx, row.Path, row.Client)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) grantDesired(ctx context.Context, rows []entry, wanted map[[2]string]entry) error {
	existing := make(map[[2]string]entry, len(rows))
	for _, row := range rows {
		if m.owns(row) {
			existing[[2]string{row.Path, row.Client}] = row
		}
	}
	for id := range m.state.Desired {
		e := m.state.Desired[id]
		for _, client := range e.clients() {
			key := [2]string{e.Path, client}
			want, needed := wanted[key]
			if !needed {
				continue
			}
			if row, exists := existing[key]; exists && optionsMatch(row.Options, want.Options) {
				continue
			}
			err := m.runtime.grant(ctx, e, client)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) verifyAdmissions(ctx context.Context, wanted map[[2]string]entry) error {
	rows, err := m.runtime.list(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !m.owns(row) {
			err := m.foreignConflict(row)
			if err != nil {
				return err
			}
			continue
		}
		key := [2]string{row.Path, row.Client}
		want, exists := wanted[key]
		if !exists || !optionsMatch(row.Options, want.Options) {
			return fmt.Errorf("NFS export readback mismatch for %q", row.Path)
		}
		delete(wanted, key)
	}
	if len(wanted) != 0 {
		return errors.New("NFS export readback missing admitted clients")
	}
	return nil
}

func optionValue(options, key string) string {
	for option := range strings.SplitSeq(options, ",") {
		value, found := strings.CutPrefix(option, key+"=")
		if found {
			return value
		}
	}
	return ""
}

func optionsMatch(actual, wanted string) bool {
	flags := strings.Split(actual, ",")
	if slices.Contains(flags, "crossmnt") {
		return false
	}
	wantNohide := false
	for option := range strings.SplitSeq(wanted, ",") {
		if !slices.Contains(flags, option) {
			return false
		}
		var opposite string
		switch option {
		case "ro":
			opposite = "rw"
		case "rw":
			opposite = "ro"
		case optionRootSquash:
			opposite = optionNoSquash
		case optionNoSquash:
			opposite = optionRootSquash
		case "all_squash":
			opposite = "no_all_squash"
		case "secure":
			opposite = "insecure"
		case "sync":
			opposite = "async"
		case "no_subtree_check":
			opposite = "subtree_check"
		case "nohide":
			wantNohide = true
			opposite = "hide"
		}
		if opposite != "" && slices.Contains(flags, opposite) {
			return false
		}
	}
	return slices.Contains(flags, "nohide") == wantNohide
}

// Put preserves grants on ordinary export retries; exact=true replaces the ACL.
// Active=false validates preparation without starting or changing the runtime.
func (m *Manager) Put(ctx context.Context, e Export, exact, active bool) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("put NFS export %q at %q: %w", e.VolumeID, e.Path, err)
	}
	e, err = e.normalized()
	if err != nil {
		return err
	}
	err = m.validatePut(e)
	if err != nil {
		if active {
			return m.rejectExport(ctx, e, err)
		}
		return err
	}
	if active {
		err = m.Start(ctx)
		if err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("put NFS export %q at %q: %w", e.VolumeID, e.Path, ctxErr)
	}
	if active {
		err = m.healthLocked()
		if err != nil {
			return err
		}
	}
	e, err = m.putPolicy(e, exact)
	if err != nil || !active {
		return err
	}
	// Once the desired policy is committed, cancellation cannot interrupt its
	// bounded kernel convergence while the caller still holds the fence.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	e.Active = true
	m.state.Desired[e.VolumeID] = e
	m.state.Ledger = append(m.state.Ledger, e)
	m.updateRoot()
	err = m.persist()
	if err != nil {
		return m.failClosed(opCtx, err)
	}
	applyErr := m.apply(opCtx)
	err = errors.Join(applyErr, m.exportErrors[e.VolumeID])
	if err != nil {
		return fmt.Errorf("put NFS export %q at %q: %w", e.VolumeID, e.Path, err)
	}
	return nil
}

func (m *Manager) putPolicy(e Export, exact bool) (Export, error) {
	if old, exists := m.state.Desired[e.VolumeID]; exists {
		if old.Path != e.Path {
			return e, errors.New("NFS volume export path cannot change")
		}
		if old.SourceKey != "" || old.FenceUID != "" {
			if e.SourceKey == "" && e.FenceUID == "" {
				e.SourceKey, e.FenceUID = old.SourceKey, old.FenceUID
			} else if e.SourceKey != old.SourceKey || e.FenceUID != old.FenceUID {
				return e, errors.New("NFS adopted ownership hint cannot change")
			}
		}
		if !exact && old.ACLEnabled && e.ACLEnabled {
			e.Clients = slices.Clone(old.Clients)
		}
	}
	for id := range m.state.Desired {
		old := m.state.Desired[id]
		if id != e.VolumeID && old.Path == e.Path {
			return e, errors.New("NFS export path already belongs to another volume")
		}
	}
	return e, nil
}

func (m *Manager) rejectExport(ctx context.Context, e Export, validationErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.state.Desired[e.VolumeID]
	if !exists || old.Path != e.Path || m.healthLocked() != nil {
		return validationErr
	}
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("reject NFS export %q at %q: %w", e.VolumeID, e.Path, ctxErr)
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	// Re-evaluate the committed policy, withdrawing only invalid admissions.
	err := errors.Join(validationErr, m.apply(opCtx))
	return fmt.Errorf("reject NFS export %q at %q: %w", e.VolumeID, e.Path, err)
}

func (m *Manager) validatePut(e Export) error {
	if e.VolumeID == rootVolumeID {
		return errors.New("reserved NFS volume identity")
	}
	_, err := m.ExportPath(e.Path)
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(e.Path)
	if err != nil {
		return fmt.Errorf("resolve NFS export path: %w", err)
	}
	if canonical != e.Path {
		return errors.New("NFS export path must not traverse symlinks")
	}
	info, err := os.Stat(e.Path)
	if err != nil {
		return fmt.Errorf("inspect NFS export path: %w", err)
	}
	if !info.IsDir() {
		return errors.New("NFS exports require a mounted filesystem directory")
	}
	err = m.runtime.validateExport(e)
	if err != nil {
		return err
	}
	if e.BindAddress != m.config.BindAddress {
		return errors.New("NFS export bind address conflicts with owned listener")
	}
	return nil
}

// ChangeClient durably changes one exact address without discarding peer grants.
func (m *Manager) ChangeClient(ctx context.Context, volumeID, client string, allow bool) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("change NFS client for volume %q: %w", volumeID, err)
	}
	startErr := m.Start(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if startErr != nil && (allow || !m.started || m.lock == nil) {
		return startErr
	}
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("change NFS client for volume %q: %w", volumeID, ctxErr)
	}
	if allow {
		err = m.healthLocked()
		if err != nil {
			return err
		}
	}
	e, exists := m.state.Desired[volumeID]
	if !exists {
		if !allow {
			return nil
		}
		return errors.New("NFS volume is not exported")
	}
	e, err = e.changedClient(client, allow)
	if err != nil {
		return err
	}
	// Node grants/revokes have no admission effect without ACL enforcement.
	if !e.ACLEnabled {
		return nil
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	return m.commitClientChange(opCtx, volumeID, &e, allow)
}

func (m *Manager) commitClientChange(ctx context.Context, volumeID string, e *Export, allow bool) error {
	m.state.Desired[volumeID] = *e
	m.state.Ledger = append(m.state.Ledger, *e)
	m.updateRoot()
	err := m.persist()
	if err != nil {
		return m.failClosed(ctx, err)
	}
	if !allow {
		return m.applyOrStop(ctx)
	}
	applyErr := m.apply(ctx)
	err = errors.Join(applyErr, m.exportErrors[volumeID])
	if err != nil {
		return fmt.Errorf("change NFS client for volume %q at %q: %w", volumeID, e.Path, err)
	}
	return nil
}

func (e Export) changedClient(client string, allow bool) (Export, error) {
	ip, err := unicastAddress(client)
	if err != nil {
		return e, err
	}
	client = ip.String()
	e.Clients = append(slices.Clone(e.Clients), client)
	e, err = e.normalized()
	if err != nil {
		return e, err
	}
	if !allow {
		e.Clients = slices.DeleteFunc(e.Clients, func(s string) bool { return s == client })
	}
	return e, nil
}

// Remove durably withdraws a volume even when the supervised runtime has failed.
func (m *Manager) Remove(ctx context.Context, volumeID string) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("remove NFS volume %q: %w", volumeID, err)
	}
	startErr := m.Start(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if startErr != nil && (!m.started || m.lock == nil) {
		return startErr
	}
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("remove NFS volume %q: %w", volumeID, ctxErr)
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	delete(m.state.Desired, volumeID)
	m.updateRoot()
	err = m.persist()
	if err != nil {
		return m.failClosed(opCtx, err)
	}
	return m.applyOrStop(opCtx)
}

func (m *Manager) applyOrStop(ctx context.Context) error {
	if m.healthLocked() != nil {
		return m.stopOwned(ctx)
	}
	return m.apply(ctx)
}

func (m *Manager) stopOwned(ctx context.Context) error {
	// Cleanup must not inherit an expired request deadline or cancellation.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	err := errors.Join(m.revokeOwned(cleanupCtx), m.runtime.close())
	if err != nil {
		return fmt.Errorf("stop owned NFS runtime: %w", err)
	}
	return nil
}

// Close removes only owned admissions and stops only its supervised helper.
// Desired policies stay durable for restart recovery.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	var err error
	if m.started {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		err = m.revokeOwned(ctx)
		cancel()
	}
	err = errors.Join(err, m.runtime.close())
	if m.lock != nil {
		err = errors.Join(err, releaseLock(m.lock))
		m.lock = nil
	}
	if err != nil {
		err = fmt.Errorf("close NFS manager: %w", err)
	}
	m.closeErr = err
	return err
}
