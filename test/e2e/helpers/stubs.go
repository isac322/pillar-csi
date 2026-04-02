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

// Package helpers provides fake/in-memory backend stubs used by the pillar-csi
// E2E test suite (test/e2e/...).
//
// Design goals
//
//  1. Zero real I/O — no os.Exec, no network, no disk reads or writes outside
//     of os.TempDir(). All state is held in Go maps behind a sync.RWMutex so
//     every stub call completes in microseconds, keeping each TC well under the
//     250ms budget even when many TCs run in parallel.
//
//  2. Full interface compliance — every stub implements the corresponding
//     production interface so that test helpers can be dropped in wherever the
//     real implementation would be used, and compile-time var-_ checks verify
//     correctness.
//
//  3. Configurable fault injection — SetError(op, err) lets individual TCs
//     inject transient or permanent errors for a named operation without
//     touching global state. Reset() wipes the stub back to factory state for
//     inter-TC isolation.
//
//  4. Call recording — CallLog() returns an ordered slice of "op:argument"
//     strings so assertions can verify that specific methods were (or were not)
//     called without having to run real backends.
//
//  5. Central StubRegistry — NewStubRegistry() returns a fully wired registry
//     that vends a FakeZFSBackend, FakeLVMBackend, FakeNVMeBackend,
//     FakeiSCSIBackend, and a TimingRecorder. Call registry.Reset() between
//     TCs to return every stub to a clean slate.
package helpers

import (
	"context"
	"fmt"
	"sync"
	"time"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
)

// ─────────────────────────────────────────────────────────────────────────────
// Interface compliance assertions (compile-time)
// ─────────────────────────────────────────────────────────────────────────────

var (
	_ backend.VolumeBackend = (*FakeZFSBackend)(nil)
	_ backend.VolumeBackend = (*FakeLVMBackend)(nil)
)

// ─────────────────────────────────────────────────────────────────────────────
// fakeVolume is the internal representation of a provisioned volume inside any
// fake backend.
// ─────────────────────────────────────────────────────────────────────────────

type fakeVolume struct {
	id            string
	capacityBytes int64
	devicePath    string
	exported      bool
}

// ─────────────────────────────────────────────────────────────────────────────
// stubBase is embedded by all fake backends to provide common functionality:
// error injection, call logging, and mutex-guarded state access.
// ─────────────────────────────────────────────────────────────────────────────

type stubBase struct {
	mu      sync.RWMutex
	errors  map[string]error // op → injected error
	callLog []string         // ordered "op:arg" entries
}

func newStubBase() stubBase {
	return stubBase{
		errors:  make(map[string]error),
		callLog: nil,
	}
}

// SetError injects err for the named operation op. Use op == "" to inject a
// catch-all error that fires for any unmatched operation.
func (s *stubBase) SetError(op string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.errors, op)
	} else {
		s.errors[op] = err
	}
}

// ClearErrors removes all injected errors.
func (s *stubBase) ClearErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = make(map[string]error)
}

// CallLog returns a snapshot of all recorded calls in order.
func (s *stubBase) CallLog() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.callLog))
	copy(out, s.callLog)
	return out
}

// ClearCallLog empties the call log.
func (s *stubBase) ClearCallLog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callLog = nil
}

// record appends a "op:arg" entry to the call log. Must be called with the
// write lock held or within a lock-free helper that already holds it.
func (s *stubBase) record(op, arg string) {
	s.callLog = append(s.callLog, op+":"+arg)
}

// checkError returns the injected error for op (or the catch-all ""), nil if
// none is set.  Must be called with at least a read lock held.
func (s *stubBase) checkError(op string) error {
	if err, ok := s.errors[op]; ok {
		return err
	}
	if err, ok := s.errors[""]; ok {
		return err
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// FakeZFSBackend
// ─────────────────────────────────────────────────────────────────────────────

// FakeZFSBackend is a fully in-memory implementation of backend.VolumeBackend
// that simulates a ZFS zvol pool. All state is held in a plain map; no ZFS
// commands are ever executed.
type FakeZFSBackend struct {
	stubBase
	pool    string
	volumes map[string]*fakeVolume // volumeID → volume
	total   int64                  // advertised total capacity in bytes
	avail   int64                  // advertised available capacity in bytes
}

// NewFakeZFSBackend returns a FakeZFSBackend bound to the given pool name.
// totalBytes and availBytes initialise the values returned by Capacity().
func NewFakeZFSBackend(pool string, totalBytes, availBytes int64) *FakeZFSBackend {
	return &FakeZFSBackend{
		stubBase: newStubBase(),
		pool:     pool,
		volumes:  make(map[string]*fakeVolume),
		total:    totalBytes,
		avail:    availBytes,
	}
}

// Reset wipes all provisioned volumes, injected errors, and the call log.
func (b *FakeZFSBackend) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.volumes = make(map[string]*fakeVolume)
	b.errors = make(map[string]error)
	b.callLog = nil
}

// Create implements backend.VolumeBackend.
func (b *FakeZFSBackend) Create(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	_ *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Create", volumeID)

	if err := b.checkError("Create"); err != nil {
		return "", 0, err
	}

	if existing, ok := b.volumes[volumeID]; ok {
		if existing.capacityBytes != capacityBytes && capacityBytes > 0 {
			return "", 0, &backend.ConflictError{
				VolumeID:       volumeID,
				ExistingBytes:  existing.capacityBytes,
				RequestedBytes: capacityBytes,
			}
		}
		return existing.devicePath, existing.capacityBytes, nil
	}

	dp := fmt.Sprintf("/dev/zvol/%s/%s", b.pool, volumeID)
	v := &fakeVolume{
		id:            volumeID,
		capacityBytes: capacityBytes,
		devicePath:    dp,
	}
	b.volumes[volumeID] = v
	b.avail -= capacityBytes
	return dp, capacityBytes, nil
}

// Delete implements backend.VolumeBackend.
func (b *FakeZFSBackend) Delete(ctx context.Context, volumeID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Delete", volumeID)

	if err := b.checkError("Delete"); err != nil {
		return err
	}

	if v, ok := b.volumes[volumeID]; ok {
		b.avail += v.capacityBytes
		delete(b.volumes, volumeID)
	}
	return nil
}

// Expand implements backend.VolumeBackend.
func (b *FakeZFSBackend) Expand(ctx context.Context, volumeID string, requestedBytes int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Expand", volumeID)

	if err := b.checkError("Expand"); err != nil {
		return 0, err
	}

	v, ok := b.volumes[volumeID]
	if !ok {
		return 0, fmt.Errorf("ZFS volume %q not found", volumeID)
	}
	delta := requestedBytes - v.capacityBytes
	if delta > 0 {
		v.capacityBytes = requestedBytes
		b.avail -= delta
	}
	return v.capacityBytes, nil
}

// Capacity implements backend.VolumeBackend.
func (b *FakeZFSBackend) Capacity(_ context.Context) (int64, int64, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.callLog = append(b.callLog, "Capacity:")
	if err := b.checkError("Capacity"); err != nil {
		return 0, 0, err
	}
	return b.total, b.avail, nil
}

// ListVolumes implements backend.VolumeBackend.
func (b *FakeZFSBackend) ListVolumes(_ context.Context) ([]*agentv1.VolumeInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.callLog = append(b.callLog, "ListVolumes:")
	if err := b.checkError("ListVolumes"); err != nil {
		return nil, err
	}
	out := make([]*agentv1.VolumeInfo, 0, len(b.volumes))
	for _, v := range b.volumes {
		out = append(out, &agentv1.VolumeInfo{
			VolumeId:      v.id,
			CapacityBytes: v.capacityBytes,
			DevicePath:    v.devicePath,
			Exported:      v.exported,
		})
	}
	return out, nil
}

// DevicePath implements backend.VolumeBackend.
func (b *FakeZFSBackend) DevicePath(volumeID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if v, ok := b.volumes[volumeID]; ok {
		return v.devicePath
	}
	return fmt.Sprintf("/dev/zvol/%s/%s", b.pool, volumeID)
}

// Type implements backend.VolumeBackend.
func (b *FakeZFSBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

// SetExported marks a volume as exported (for ListVolumes response).
func (b *FakeZFSBackend) SetExported(volumeID string, exported bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.volumes[volumeID]; ok {
		v.exported = exported
	}
}

// VolumeExists returns true if a volume with the given ID exists in the stub.
func (b *FakeZFSBackend) VolumeExists(volumeID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.volumes[volumeID]
	return ok
}

// ─────────────────────────────────────────────────────────────────────────────
// FakeLVMBackend
// ─────────────────────────────────────────────────────────────────────────────

// FakeLVMBackend is a fully in-memory implementation of backend.VolumeBackend
// that simulates an LVM volume group. No LVM commands are executed.
type FakeLVMBackend struct {
	stubBase
	vg      string
	volumes map[string]*fakeVolume
	total   int64
	avail   int64
}

// NewFakeLVMBackend returns a FakeLVMBackend bound to the given volume group.
func NewFakeLVMBackend(vg string, totalBytes, availBytes int64) *FakeLVMBackend {
	return &FakeLVMBackend{
		stubBase: newStubBase(),
		vg:       vg,
		volumes:  make(map[string]*fakeVolume),
		total:    totalBytes,
		avail:    availBytes,
	}
}

// Reset wipes all provisioned volumes, injected errors, and the call log.
func (b *FakeLVMBackend) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.volumes = make(map[string]*fakeVolume)
	b.errors = make(map[string]error)
	b.callLog = nil
}

// Create implements backend.VolumeBackend.
func (b *FakeLVMBackend) Create(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	_ *agentv1.BackendParams,
) (string, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Create", volumeID)

	if err := b.checkError("Create"); err != nil {
		return "", 0, err
	}

	if existing, ok := b.volumes[volumeID]; ok {
		if existing.capacityBytes != capacityBytes && capacityBytes > 0 {
			return "", 0, &backend.ConflictError{
				VolumeID:       volumeID,
				ExistingBytes:  existing.capacityBytes,
				RequestedBytes: capacityBytes,
			}
		}
		return existing.devicePath, existing.capacityBytes, nil
	}

	dp := fmt.Sprintf("/dev/%s/%s", b.vg, volumeID)
	v := &fakeVolume{
		id:            volumeID,
		capacityBytes: capacityBytes,
		devicePath:    dp,
	}
	b.volumes[volumeID] = v
	b.avail -= capacityBytes
	return dp, capacityBytes, nil
}

// Delete implements backend.VolumeBackend.
func (b *FakeLVMBackend) Delete(ctx context.Context, volumeID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Delete", volumeID)

	if err := b.checkError("Delete"); err != nil {
		return err
	}

	if v, ok := b.volumes[volumeID]; ok {
		b.avail += v.capacityBytes
		delete(b.volumes, volumeID)
	}
	return nil
}

// Expand implements backend.VolumeBackend.
func (b *FakeLVMBackend) Expand(ctx context.Context, volumeID string, requestedBytes int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Expand", volumeID)

	if err := b.checkError("Expand"); err != nil {
		return 0, err
	}

	v, ok := b.volumes[volumeID]
	if !ok {
		return 0, fmt.Errorf("LVM volume %q not found", volumeID)
	}
	delta := requestedBytes - v.capacityBytes
	if delta > 0 {
		v.capacityBytes = requestedBytes
		b.avail -= delta
	}
	return v.capacityBytes, nil
}

// Capacity implements backend.VolumeBackend.
func (b *FakeLVMBackend) Capacity(_ context.Context) (int64, int64, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.callLog = append(b.callLog, "Capacity:")
	if err := b.checkError("Capacity"); err != nil {
		return 0, 0, err
	}
	return b.total, b.avail, nil
}

// ListVolumes implements backend.VolumeBackend.
func (b *FakeLVMBackend) ListVolumes(_ context.Context) ([]*agentv1.VolumeInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.callLog = append(b.callLog, "ListVolumes:")
	if err := b.checkError("ListVolumes"); err != nil {
		return nil, err
	}
	out := make([]*agentv1.VolumeInfo, 0, len(b.volumes))
	for _, v := range b.volumes {
		out = append(out, &agentv1.VolumeInfo{
			VolumeId:      v.id,
			CapacityBytes: v.capacityBytes,
			DevicePath:    v.devicePath,
			Exported:      v.exported,
		})
	}
	return out, nil
}

// DevicePath implements backend.VolumeBackend.
func (b *FakeLVMBackend) DevicePath(volumeID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if v, ok := b.volumes[volumeID]; ok {
		return v.devicePath
	}
	return fmt.Sprintf("/dev/%s/%s", b.vg, volumeID)
}

// Type implements backend.VolumeBackend.
func (b *FakeLVMBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_LVM
}

// VolumeExists returns true if the volume exists in the stub.
func (b *FakeLVMBackend) VolumeExists(volumeID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.volumes[volumeID]
	return ok
}

// ─────────────────────────────────────────────────────────────────────────────
// FakeNVMeBackend
// ─────────────────────────────────────────────────────────────────────────────

// nvmeSubsystem holds in-memory state for a single NVMe-oF subsystem.
type nvmeSubsystem struct {
	nqn        string
	namespaces map[string]string // nsid → devicePath
	initiators map[string]bool   // allowed initiator NQNs
	connected  bool
}

// FakeNVMeBackend simulates NVMe-oF TCP target operations in-memory.
// No kernel configfs or nvme CLI is used.
type FakeNVMeBackend struct {
	stubBase
	subsystems map[string]*nvmeSubsystem // nqn → subsystem
}

// NewFakeNVMeBackend returns a new FakeNVMeBackend.
func NewFakeNVMeBackend() *FakeNVMeBackend {
	return &FakeNVMeBackend{
		stubBase:   newStubBase(),
		subsystems: make(map[string]*nvmeSubsystem),
	}
}

// Reset wipes all state, injected errors, and the call log.
func (b *FakeNVMeBackend) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subsystems = make(map[string]*nvmeSubsystem)
	b.errors = make(map[string]error)
	b.callLog = nil
}

// CreateSubsystem creates an NVMe subsystem with the given NQN.
func (b *FakeNVMeBackend) CreateSubsystem(nqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("CreateSubsystem", nqn)
	if err := b.checkError("CreateSubsystem"); err != nil {
		return err
	}
	if _, ok := b.subsystems[nqn]; !ok {
		b.subsystems[nqn] = &nvmeSubsystem{
			nqn:        nqn,
			namespaces: make(map[string]string),
			initiators: make(map[string]bool),
		}
	}
	return nil
}

// DeleteSubsystem removes an NVMe subsystem.
func (b *FakeNVMeBackend) DeleteSubsystem(nqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("DeleteSubsystem", nqn)
	if err := b.checkError("DeleteSubsystem"); err != nil {
		return err
	}
	delete(b.subsystems, nqn)
	return nil
}

// AddNamespace attaches a block device path as a namespace inside the subsystem.
func (b *FakeNVMeBackend) AddNamespace(nqn, nsid, devicePath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("AddNamespace", nqn+"/"+nsid)
	if err := b.checkError("AddNamespace"); err != nil {
		return err
	}
	sub, ok := b.subsystems[nqn]
	if !ok {
		return fmt.Errorf("NVMe subsystem %q not found", nqn)
	}
	sub.namespaces[nsid] = devicePath
	return nil
}

// RemoveNamespace detaches a namespace from the subsystem.
func (b *FakeNVMeBackend) RemoveNamespace(nqn, nsid string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("RemoveNamespace", nqn+"/"+nsid)
	if err := b.checkError("RemoveNamespace"); err != nil {
		return err
	}
	if sub, ok := b.subsystems[nqn]; ok {
		delete(sub.namespaces, nsid)
	}
	return nil
}

// AllowInitiator grants an initiator NQN access to the subsystem.
func (b *FakeNVMeBackend) AllowInitiator(nqn, initiatorNQN string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("AllowInitiator", nqn+"/"+initiatorNQN)
	if err := b.checkError("AllowInitiator"); err != nil {
		return err
	}
	sub, ok := b.subsystems[nqn]
	if !ok {
		return fmt.Errorf("NVMe subsystem %q not found", nqn)
	}
	sub.initiators[initiatorNQN] = true
	return nil
}

// DenyInitiator revokes an initiator NQN's access.
func (b *FakeNVMeBackend) DenyInitiator(nqn, initiatorNQN string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("DenyInitiator", nqn+"/"+initiatorNQN)
	if err := b.checkError("DenyInitiator"); err != nil {
		return err
	}
	if sub, ok := b.subsystems[nqn]; ok {
		delete(sub.initiators, initiatorNQN)
	}
	return nil
}

// Connect simulates an initiator connecting to a subsystem.
func (b *FakeNVMeBackend) Connect(nqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Connect", nqn)
	if err := b.checkError("Connect"); err != nil {
		return err
	}
	sub, ok := b.subsystems[nqn]
	if !ok {
		return fmt.Errorf("NVMe subsystem %q not found", nqn)
	}
	sub.connected = true
	return nil
}

// Disconnect simulates an initiator disconnecting from a subsystem.
func (b *FakeNVMeBackend) Disconnect(nqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Disconnect", nqn)
	if err := b.checkError("Disconnect"); err != nil {
		return err
	}
	if sub, ok := b.subsystems[nqn]; ok {
		sub.connected = false
	}
	return nil
}

// SubsystemExists returns true if a subsystem with the given NQN was created.
func (b *FakeNVMeBackend) SubsystemExists(nqn string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.subsystems[nqn]
	return ok
}

// InitiatorAllowed returns true if the initiator is in the subsystem's ACL.
func (b *FakeNVMeBackend) InitiatorAllowed(nqn, initiatorNQN string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sub, ok := b.subsystems[nqn]
	if !ok {
		return false
	}
	return sub.initiators[initiatorNQN]
}

// ─────────────────────────────────────────────────────────────────────────────
// FakeiSCSIBackend
// ─────────────────────────────────────────────────────────────────────────────

// iscsiTarget holds in-memory state for a single iSCSI target.
type iscsiTarget struct {
	iqn        string
	luns       map[string]string // lun-id → devicePath
	initiators map[string]bool   // allowed initiator IQNs
	loggedIn   bool
}

// FakeiSCSIBackend simulates iSCSI target operations in-memory.
// No iscsiadm, targetcli, or kernel LIO interactions occur.
type FakeiSCSIBackend struct {
	stubBase
	targets map[string]*iscsiTarget // iqn → target
}

// NewFakeiSCSIBackend returns a new FakeiSCSIBackend.
func NewFakeiSCSIBackend() *FakeiSCSIBackend {
	return &FakeiSCSIBackend{
		stubBase: newStubBase(),
		targets:  make(map[string]*iscsiTarget),
	}
}

// Reset wipes all state, injected errors, and the call log.
func (b *FakeiSCSIBackend) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.targets = make(map[string]*iscsiTarget)
	b.errors = make(map[string]error)
	b.callLog = nil
}

// CreateTarget creates an iSCSI target with the given IQN.
func (b *FakeiSCSIBackend) CreateTarget(iqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("CreateTarget", iqn)
	if err := b.checkError("CreateTarget"); err != nil {
		return err
	}
	if _, ok := b.targets[iqn]; !ok {
		b.targets[iqn] = &iscsiTarget{
			iqn:        iqn,
			luns:       make(map[string]string),
			initiators: make(map[string]bool),
		}
	}
	return nil
}

// DeleteTarget removes an iSCSI target.
func (b *FakeiSCSIBackend) DeleteTarget(iqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("DeleteTarget", iqn)
	if err := b.checkError("DeleteTarget"); err != nil {
		return err
	}
	delete(b.targets, iqn)
	return nil
}

// AddLUN attaches a block device as a LUN under the target.
func (b *FakeiSCSIBackend) AddLUN(iqn, lunID, devicePath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("AddLUN", iqn+"/"+lunID)
	if err := b.checkError("AddLUN"); err != nil {
		return err
	}
	t, ok := b.targets[iqn]
	if !ok {
		return fmt.Errorf("iSCSI target %q not found", iqn)
	}
	t.luns[lunID] = devicePath
	return nil
}

// RemoveLUN detaches a LUN from the target.
func (b *FakeiSCSIBackend) RemoveLUN(iqn, lunID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("RemoveLUN", iqn+"/"+lunID)
	if err := b.checkError("RemoveLUN"); err != nil {
		return err
	}
	if t, ok := b.targets[iqn]; ok {
		delete(t.luns, lunID)
	}
	return nil
}

// AllowInitiator grants access to an initiator IQN.
func (b *FakeiSCSIBackend) AllowInitiator(iqn, initiatorIQN string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("AllowInitiator", iqn+"/"+initiatorIQN)
	if err := b.checkError("AllowInitiator"); err != nil {
		return err
	}
	t, ok := b.targets[iqn]
	if !ok {
		return fmt.Errorf("iSCSI target %q not found", iqn)
	}
	t.initiators[initiatorIQN] = true
	return nil
}

// DenyInitiator revokes an initiator IQN's access.
func (b *FakeiSCSIBackend) DenyInitiator(iqn, initiatorIQN string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("DenyInitiator", iqn+"/"+initiatorIQN)
	if err := b.checkError("DenyInitiator"); err != nil {
		return err
	}
	if t, ok := b.targets[iqn]; ok {
		delete(t.initiators, initiatorIQN)
	}
	return nil
}

// Login simulates an initiator logging into the target.
func (b *FakeiSCSIBackend) Login(iqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Login", iqn)
	if err := b.checkError("Login"); err != nil {
		return err
	}
	t, ok := b.targets[iqn]
	if !ok {
		return fmt.Errorf("iSCSI target %q not found", iqn)
	}
	t.loggedIn = true
	return nil
}

// Logout simulates an initiator logging out of the target.
func (b *FakeiSCSIBackend) Logout(iqn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("Logout", iqn)
	if err := b.checkError("Logout"); err != nil {
		return err
	}
	if t, ok := b.targets[iqn]; ok {
		t.loggedIn = false
	}
	return nil
}

// TargetExists returns true if the target with the given IQN was created.
func (b *FakeiSCSIBackend) TargetExists(iqn string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.targets[iqn]
	return ok
}

// InitiatorAllowed returns true if the initiator is in the target's ACL.
func (b *FakeiSCSIBackend) InitiatorAllowed(iqn, initiatorIQN string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	t, ok := b.targets[iqn]
	if !ok {
		return false
	}
	return t.initiators[initiatorIQN]
}

// ─────────────────────────────────────────────────────────────────────────────
// TCTiming — per-test-case timing data
// ─────────────────────────────────────────────────────────────────────────────

// TCTiming records the wall-clock duration of a single test case across its
// named execution phases.
type TCTiming struct {
	TCID    string
	Total   time.Duration
	Phases  map[string]time.Duration
	Slowest bool // flagged by TimingRecorder.Report if in top-5 slowest
}

// ─────────────────────────────────────────────────────────────────────────────
// TimingRecorder
// ─────────────────────────────────────────────────────────────────────────────

// TimingRecorder measures per-TC durations across labelled execution phases
// (group-setup, tc-setup, tc-execute, tc-teardown, group-teardown). It is safe
// for concurrent use and imposes no I/O.
type TimingRecorder struct {
	mu      sync.Mutex
	now     func() time.Time
	starts  map[string]time.Time            // tcID → wall-clock start
	phases  map[string]map[string]time.Time // tcID → phaseName → start
	results []TCTiming
}

// NewTimingRecorder returns a TimingRecorder using time.Now as the clock.
func NewTimingRecorder() *TimingRecorder {
	return &TimingRecorder{
		now:    time.Now,
		starts: make(map[string]time.Time),
		phases: make(map[string]map[string]time.Time),
	}
}

// Start marks the beginning of TC tcID.
func (r *TimingRecorder) Start(tcID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts[tcID] = r.now()
	r.phases[tcID] = make(map[string]time.Time)
}

// BeginPhase marks the start of a named phase within TC tcID.
func (r *TimingRecorder) BeginPhase(tcID, phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pm, ok := r.phases[tcID]; ok {
		pm[phase+".start"] = r.now()
	}
}

// EndPhase marks the end of a named phase within TC tcID.
func (r *TimingRecorder) EndPhase(tcID, phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pm, ok := r.phases[tcID]
	if !ok {
		return
	}
	startKey := phase + ".start"
	start, ok := pm[startKey]
	if !ok {
		return
	}
	elapsed := r.now().Sub(start)
	pm[phase] = time.Time{}.Add(elapsed) // store duration as time.Time offset
	delete(pm, startKey)
}

// Stop marks the end of TC tcID and records the TCTiming.
func (r *TimingRecorder) Stop(tcID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	start, ok := r.starts[tcID]
	if !ok {
		return
	}
	total := r.now().Sub(start)
	delete(r.starts, tcID)

	phases := make(map[string]time.Duration)
	if pm, ok := r.phases[tcID]; ok {
		for k, t := range pm {
			// Only record completed phases (not in-flight .start keys)
			if len(k) < 6 || k[len(k)-6:] != ".start" {
				phases[k] = time.Duration(t.UnixNano())
			}
		}
		delete(r.phases, tcID)
	}

	r.results = append(r.results, TCTiming{
		TCID:   tcID,
		Total:  total,
		Phases: phases,
	})
}

// Report returns all recorded TCTimings. The five slowest TCs have their
// Slowest flag set to true.
func (r *TimingRecorder) Report() []TCTiming {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]TCTiming, len(r.results))
	copy(out, r.results)

	// Find and flag the 5 slowest.
	const topN = 5
	indices := make([]int, len(out))
	for i := range indices {
		indices[i] = i
	}
	// Simple selection of top-N by total duration.
	for i := 0; i < topN && i < len(out); i++ {
		maxIdx := i
		for j := i + 1; j < len(out); j++ {
			if out[indices[j]].Total > out[indices[maxIdx]].Total {
				maxIdx = j
			}
		}
		indices[i], indices[maxIdx] = indices[maxIdx], indices[i]
		out[indices[i]].Slowest = true
	}

	return out
}

// Reset clears all recorded timings.
func (r *TimingRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = make(map[string]time.Time)
	r.phases = make(map[string]map[string]time.Time)
	r.results = nil
}

// ─────────────────────────────────────────────────────────────────────────────
// StubRegistry — central wiring point for all fake backends
// ─────────────────────────────────────────────────────────────────────────────

// StubRegistry is the central registry that test suites use to obtain
// pre-wired fake backends. Call NewStubRegistry() once per suite (or once per
// TC for full isolation) and Reset() between TCs to return every stub to a
// clean slate without allocating new objects.
type StubRegistry struct {
	ZFS    *FakeZFSBackend
	LVM    *FakeLVMBackend
	NVMe   *FakeNVMeBackend
	ISCSI  *FakeiSCSIBackend
	Timing *TimingRecorder
}

// defaultCapacity is the total / available byte count used when the caller
// does not supply explicit values. 10 TiB is large enough to prevent capacity
// exhaustion during normal test runs.
const defaultCapacity = 10 * 1024 * 1024 * 1024 * 1024 // 10 TiB

// NewStubRegistry returns a StubRegistry with all fake backends initialised to
// their default, error-free states. Pool names and VG names default to
// "fake-pool" and "fake-vg" respectively; override by creating the individual
// fake backends directly if per-TC names are required.
func NewStubRegistry() *StubRegistry {
	return &StubRegistry{
		ZFS:    NewFakeZFSBackend("fake-pool", defaultCapacity, defaultCapacity),
		LVM:    NewFakeLVMBackend("fake-vg", defaultCapacity, defaultCapacity),
		NVMe:   NewFakeNVMeBackend(),
		ISCSI:  NewFakeiSCSIBackend(),
		Timing: NewTimingRecorder(),
	}
}

// Reset returns every stub and the timing recorder to a clean, empty state.
// Call this in BeforeEach / AfterEach to enforce inter-TC isolation without
// the overhead of allocating new objects.
func (r *StubRegistry) Reset() {
	r.ZFS.Reset()
	r.LVM.Reset()
	r.NVMe.Reset()
	r.ISCSI.Reset()
	r.Timing.Reset()
}

// GlobalStubs is a package-level StubRegistry that test files can import
// directly without constructing their own registry. It is initialised once at
// package init time and must be Reset() between TCs.
var GlobalStubs = NewStubRegistry()
