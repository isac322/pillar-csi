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

// Package lvm implements the VolumeBackend interface for LVM logical volumes.
// All LVM operations are executed via os/exec calls to lvcreate(8), lvremove(8),
// lvextend(8), lvs(8), and vgs(8); no CGO or lvm2app bindings are used, so the
// agent binary carries zero CGO dependencies and can be cross-compiled easily.
//
// LVM naming convention used by this backend:
//
//	VolumeID format: "<vg>/<lv-name>"
//	Block device path: /dev/<vg>/<lv-name>
//
// Two provisioning modes are supported:
//
//	Linear LV: a standard linear logical volume created directly in the VG.
//	Thin LV:   a thin-provisioned LV created inside a pre-existing thin pool.
//
// The Volume Group and (for thin mode) the thin pool MUST be pre-created on the
// node before starting the agent; this backend does NOT manage VG or thin pool
// lifecycle.
//
// Only block volume mode is supported; filesystem formatting is out of scope
// (the CSI node layer handles fs creation on top of the block device).
package lvm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"

	"github.com/bhyoo/pillar-csi/internal/agent/backend"
)

// ─────────────────────────────────────────────────────────────────────────────
// Name validation
// ─────────────────────────────────────────────────────────────────────────────

// lvmNameRe matches valid LVM volume group and logical volume name characters.
//
// LVM naming rules (from lvm(8) and dm-name constraints):
//   - Must NOT start with a hyphen or dot (device mapper interprets leading
//     hyphens as options and dots have special meaning).
//   - May contain: letters, digits, underscores, hyphens, plus signs, dots.
//   - Must NOT be the reserved paths "." or "..".
//   - Maximum length: 128 characters.
//
// This regexp enforces the first-character constraint and the character
// allowlist.  The reserved-name and length checks are applied separately.
var lvmNameRe = regexp.MustCompile(`^[a-zA-Z0-9_+][a-zA-Z0-9_+.\-]*$`)

// lvmReservedLVPrefixes lists LV name prefixes reserved by LVM for its own
// internal snapshot and mirror bookkeeping.  User-created LVs whose names
// start with any of these prefixes may collide with LVM metadata and must be
// rejected at request time rather than relying on lvcreate(8) to catch them.
var lvmReservedLVPrefixes = []string{"snapshot", "pvmove"}

// ValidateVGName returns a descriptive error if vg is not a legal LVM volume-
// group name.  A valid name must:
//   - be non-empty and at most 128 characters long,
//   - not be the reserved path "." or "..",
//   - start with a letter, digit, underscore, or plus sign (not a hyphen or dot),
//   - contain only letters, digits, underscores, hyphens, plus signs, or dots.
//
// The function is exported so that callers outside the lvm package (e.g. the
// agent CLI flag parser) can reuse the same validation logic.
func ValidateVGName(vg string) error {
	if vg == "" {
		return errors.New("lvm: volume group name must not be empty")
	}
	if len(vg) > 128 {
		return fmt.Errorf("lvm: volume group name %q exceeds 128-character limit", vg)
	}
	if vg == "." || vg == ".." {
		return fmt.Errorf("lvm: volume group name %q is a reserved path", vg)
	}
	if !lvmNameRe.MatchString(vg) {
		return fmt.Errorf(
			"lvm: invalid volume group name %q: "+
				"must start with a letter, digit, underscore, or plus sign "+
				"and contain only letters, digits, underscores, hyphens, "+
				"plus signs, or dots",
			vg,
		)
	}
	return nil
}

// ValidateLVName returns a descriptive error if lv is not a legal LVM logical-
// volume name.  The same character rules as ValidateVGName apply, with the
// additional constraint that lv must not start with a reserved LVM prefix
// ("snapshot", "pvmove").
//
// The function is exported so that callers outside the lvm package can reuse
// the same validation logic without importing a private helper.
func ValidateLVName(lv string) error {
	if lv == "" {
		return errors.New("lvm: logical volume name must not be empty")
	}
	if len(lv) > 128 {
		return fmt.Errorf("lvm: logical volume name %q exceeds 128-character limit", lv)
	}
	if lv == "." || lv == ".." {
		return fmt.Errorf("lvm: logical volume name %q is a reserved path", lv)
	}
	if !lvmNameRe.MatchString(lv) {
		return fmt.Errorf(
			"lvm: invalid logical volume name %q: "+
				"must start with a letter, digit, underscore, or plus sign "+
				"and contain only letters, digits, underscores, hyphens, "+
				"plus signs, or dots",
			lv,
		)
	}
	lower := strings.ToLower(lv)
	for _, prefix := range lvmReservedLVPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf(
				"lvm: logical volume name %q must not begin with reserved prefix %q",
				lv, prefix,
			)
		}
	}
	return nil
}

// defaultDevBase is the production /dev directory prefix under which the kernel
// exposes LVM LV block devices as /dev/<vg>/<lv>.
const defaultDevBase = "/dev"

// ─────────────────────────────────────────────────────────────────────────────
// ProvisionMode
// ─────────────────────────────────────────────────────────────────────────────

// ProvisionMode selects between a fully-allocated linear LV and a
// thin-provisioned LV within a pre-existing thin pool.
type ProvisionMode int

const (
	// ProvisionModeLinear allocates a fully provisioned logical volume directly
	// within the volume group.  Equivalent to `lvcreate -L <size>b -n <name> <vg>`.
	ProvisionModeLinear ProvisionMode = iota

	// ProvisionModeThin allocates a thin-provisioned logical volume within a
	// pre-existing LVM thin pool.  Equivalent to
	// `lvcreate -V <size>b -n <name> --thinpool <pool> <vg>`.
	ProvisionModeThin
)

// String returns a human-readable label for the provisioning mode.
func (m ProvisionMode) String() string {
	switch m {
	case ProvisionModeLinear:
		return "linear"
	case ProvisionModeThin:
		return "thin"
	default:
		return fmt.Sprintf("ProvisionMode(%d)", int(m))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Per-volume parameters
// ─────────────────────────────────────────────────────────────────────────────

// Params holds per-volume LVM parameters extracted from the LvmVolumeParams
// gRPC message.  These act as optional per-volume overrides on top of the
// backend-level defaults configured at agent startup via --backend flags.
type Params struct {
	// ExtraFlags are additional lvcreate arguments forwarded verbatim to the
	// lvcreate(8) invocation.  Example: ["--addtag", "owner=team-a"].
	ExtraFlags []string

	// VGOverride, when non-empty, explicitly names the volume group for this
	// volume.  It must equal the backend VG; cross-VG provisioning is not
	// supported.  Use ValidateParams to check consistency.
	VGOverride string

	// AccessType captures the CSI volume access type (block or mount) that
	// the consumer requests.  The LVM backend is block-only; MOUNT requests
	// are accepted but filesystem formatting is handled by the CSI node layer.
	AccessType agentv1.VolumeAccessType

	// ProvisionModeOverride, when non-empty, overrides the backend-level
	// provisioning mode for this individual volume.  Valid values are
	// ProvisionModeLinear and ProvisionModeThin.  A zero value means "use the
	// backend default" (thin when the backend has a thinpool configured,
	// linear otherwise).
	//
	// Use ParseProvisionMode to convert the wire string to a ProvisionMode.
	ProvisionModeOverride ProvisionMode

	// hasModeOverride is true when ProvisionModeOverride was explicitly set by
	// the caller; false means "no override, use backend default".
	hasModeOverride bool
}

// HasModeOverride reports whether the Params carry an explicit provisioning
// mode that should override the backend-level default.
func (p Params) HasModeOverride() bool { return p.hasModeOverride }

// ParseProvisionMode converts a wire-level provisioning mode string (as carried
// in LvmVolumeParams.provision_mode) to the typed ProvisionMode enum.  It
// returns the mode and a boolean indicating whether the string was a recognized
// non-empty value.
//
// Recognized values (case-insensitive):
//   - "linear" → ProvisionModeLinear, true
//   - "thin"   → ProvisionModeThin, true
//   - ""       → ProvisionModeLinear, false  (caller should treat as "no override")
//
// Any other non-empty string returns ProvisionModeLinear and false so that
// callers can detect and reject unknown values.
func ParseProvisionMode(s string) (mode ProvisionMode, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "linear":
		return ProvisionModeLinear, true
	case "thin":
		return ProvisionModeThin, true
	default:
		return ProvisionModeLinear, false
	}
}

// ParseParams extracts LVM-specific per-volume parameters from an
// LvmVolumeParams proto message.  A nil proto is valid and returns a
// zero-value Params (no extra flags, no VG override, unspecified access type,
// no provisioning mode override).
func ParseParams(p *agentv1.LvmVolumeParams) Params {
	if p == nil {
		return Params{}
	}
	params := Params{
		VGOverride: strings.TrimSpace(p.GetVolumeGroup()),
		ExtraFlags: p.GetExtraFlags(),
	}
	if raw := strings.TrimSpace(p.GetProvisionMode()); raw != "" {
		mode, ok := ParseProvisionMode(raw)
		if ok {
			params.ProvisionModeOverride = mode
			params.hasModeOverride = true
		}
		// Unknown values: hasModeOverride remains false; ValidateParams will
		// surface the error so the caller can reject with InvalidArgument.
	}
	return params
}

// ValidateParams checks that the per-volume Params are consistent with the
// backend VG and thinpool configuration.  It returns an error if:
//   - VGOverride is non-empty and refers to a different VG than the one the
//     backend was started with.
//   - ProvisionModeOverride is ProvisionModeThin but the backend has no thin
//     pool configured (no thinpool= CLI flag was given).
//   - The raw provision_mode string from the proto was non-empty but not a
//     recognized value (detected via HasModeOverride() being false).
func ValidateParams(p Params, backendVG, backendThinPool string) error {
	if p.VGOverride != "" && p.VGOverride != backendVG {
		return fmt.Errorf(
			"lvm: volume_group %q in params does not match backend VG %q; "+
				"cross-VG provisioning is not supported",
			p.VGOverride, backendVG,
		)
	}
	if p.hasModeOverride && p.ProvisionModeOverride == ProvisionModeThin && backendThinPool == "" {
		return fmt.Errorf(
			"lvm: provision_mode %q requested but backend has no thin pool configured; "+
				"start the agent with --backend type=lvm-lv,vg=%s,thinpool=<name>",
			"thin", backendVG,
		)
	}
	return nil
}

// validateProvisionModeString returns an error when s is a non-empty string
// that is not a recognized provisioning mode.  An empty s (meaning "use
// backend default") is always valid.
func validateProvisionModeString(s string) error {
	if s == "" {
		return nil
	}
	_, ok := ParseProvisionMode(s)
	if !ok {
		return fmt.Errorf(
			"lvm: unknown provision_mode %q; accepted values are %q and %q",
			s, "linear", "thin",
		)
	}
	return nil
}

// executor abstracts os/exec so that unit tests can inject a fake without
// running real LVM commands.  The production path always uses osExecutor.
type executor interface {
	run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// osExecutor is the real executor that delegates to os/exec.
type osExecutor struct{}

func (osExecutor) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	//nolint:gosec,wrapcheck // G204: intentional; raw exit error returned with output.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// execFunc adapts a bare function to the executor interface, making it
// convenient to supply inline fakes in tests.
type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f execFunc) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

// notExistError is returned internally when an LV is confirmed to not exist.
type notExistError struct {
	lvPath string
}

func (e *notExistError) Error() string {
	return fmt.Sprintf("lvm: logical volume %q does not exist", e.lvPath)
}

// isNotExist reports whether err is (or wraps) a notExistError.
func isNotExist(err error) bool {
	var e *notExistError
	return errors.As(err, &e)
}

// isNotExistOutput returns true when LVM command output indicates that the
// logical volume or volume group was not found.
func isNotExistOutput(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "failed to find logical volume") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "volume group") && strings.Contains(s, "not found") ||
		strings.Contains(s, "no such logical volume")
}

// isAlreadyExistsOutput returns true when LVM command output indicates that the
// logical volume already exists in the volume group.  Lvcreate(8) exits non-zero
// with a message like "Logical volume <name> already exists in volume group <vg>"
// when asked to create an LV that is already present; callers can use this
// predicate to distinguish that benign case from genuine failures.
func isAlreadyExistsOutput(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "already exists")
}

// Backend implements backend.VolumeBackend using LVM logical volumes.
//
// A single Backend instance is scoped to one LVM Volume Group and one optional
// thin pool within that VG.  This mirrors the PillarPool CRD concept where each
// pool maps to exactly one backend instance on an agent.
//
// When thinpool is empty, the backend operates in linear mode (standard LVs).
// When thinpool is non-empty, the backend creates thin-provisioned LVs inside
// the named thin pool.
type Backend struct {
	// vg is the LVM Volume Group name (e.g. "data-vg").
	vg string

	// thinpool is the LVM thin pool LV name within vg (e.g. "thin-pool-0").
	// Empty string means linear (non-thin) provisioning mode.
	thinpool string

	// exec is the executor used to run LVM commands.  It defaults to
	// osExecutor{} and can be overridden in tests via SetBackendExec.
	// Storing the executor per-instance ensures that parallel tests using
	// different Backend values do not share state.
	exec executor

	// devBase is the /dev directory prefix.  It defaults to defaultDevBase
	// ("/dev") and can be overridden per-instance in tests via
	// SetBackendDevBase, keeping parallel tests isolated.
	devBase string
}

// Verify at compile time that Backend satisfies the VolumeBackend interface.
var _ backend.VolumeBackend = (*Backend)(nil)

// New creates a Backend bound to the given volume group.
// Thinpool may be empty for linear provisioning mode, or non-empty for thin-
// provisioned LVs within the named thin pool.
// Neither argument is validated here; callers should supply values that have
// already been sanitized (no path separators, no empty vg).
func New(vg, thinpool string) *Backend {
	return &Backend{
		vg:       vg,
		thinpool: thinpool,
		exec:     osExecutor{},
		devBase:  defaultDevBase,
	}
}

// NewWithExecFn creates a Backend that delegates all LVM command execution to
// fn instead of running real lvcreate/lvremove/etc binaries.  This constructor
// is intended for use in component and integration tests that need to simulate
// LVM command output without requiring an LVM-capable host.
//
// Example:
//
//	b := lvm.NewWithExecFn("data-vg", "", func(ctx context.Context, name string, args ...string) ([]byte, error) {
//	    return []byte(""), nil
//	})
func NewWithExecFn(
	vg, thinpool string,
	fn func(ctx context.Context, name string, args ...string) ([]byte, error),
) *Backend {
	return &Backend{
		vg:       vg,
		thinpool: thinpool,
		exec:     execFunc(fn),
		devBase:  defaultDevBase,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend configuration accessors and validation
// ─────────────────────────────────────────────────────────────────────────────

// VG returns the volume group name this backend is bound to.
func (b *Backend) VG() string { return b.vg }

// ThinPool returns the thin pool LV name (empty when mode is linear).
func (b *Backend) ThinPool() string { return b.thinpool }

// Mode returns the provisioning mode (ProvisionModeLinear or ProvisionModeThin).
// It is derived from the thinpool field: non-empty thinpool implies thin mode.
func (b *Backend) Mode() ProvisionMode {
	if b.thinpool != "" {
		return ProvisionModeThin
	}
	return ProvisionModeLinear
}

// Validate checks that the backend configuration is internally consistent.
// It returns an error if:
//   - the VG name is empty or blank
//   - the Mode() is ProvisionModeThin but thinpool is empty (should not happen
//     via New(), but guards against direct struct construction in tests)
//
// Validate does NOT probe the host; VG/thin-pool existence is checked at
// runtime when LVM commands are executed.
func (b *Backend) Validate() error {
	if strings.TrimSpace(b.vg) == "" {
		return fmt.Errorf("lvm: volume group name must not be empty")
	}
	// thinpool == "" means linear; a non-blank thinpool is always valid.
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// VolumeID helpers
// ─────────────────────────────────────────────────────────────────────────────

// lvName extracts the bare LV name from a volumeID.
// VolumeID is expected to be in the format "<vg>/<lv-name>" as used throughout
// the agent gRPC API; the vg prefix is stripped to obtain the bare LV name.
//
// Examples:
//
//	vg="data-vg"  volumeID="data-vg/pvc-abc"  → "pvc-abc"
//	vg="vg0"      volumeID="vg0/pvc-xyz"       → "pvc-xyz"
func (b *Backend) lvName(volumeID string) string {
	return strings.TrimPrefix(volumeID, b.vg+"/")
}

// DevicePath returns the host path to the LV block device for volumeID.
// The path is constructed purely from vg/lv name metadata — no kernel or
// filesystem calls are made.
//
// The returned path follows the kernel convention:
//
//	/dev/<vg>/<lv-name>
func (b *Backend) DevicePath(volumeID string) string {
	return b.devBase + "/" + b.vg + "/" + b.lvName(volumeID)
}

// Type returns BACKEND_TYPE_LVM, identifying this backend as an LVM logical
// volume implementation.  It satisfies the backend.VolumeBackend interface so
// that callers (e.g. GetCapabilities) can report supported backend types
// dynamically without hardcoding LVM-specific values.
func (*Backend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_LVM
}

// lvsBytes queries LVM for the current size of an LV in bytes.
// It returns a *notExistError when the LV does not exist at all, which allows
// callers to distinguish "missing" from other errors.
func (b *Backend) lvsBytes(ctx context.Context, volumeID string) (int64, error) {
	lv := b.lvName(volumeID)
	// lvs --noheadings -o lv_size --units b --nosuffix vg/lv
	out, err := b.exec.run(ctx, "lvs",
		"--noheadings", "-o", "lv_size",
		"--units", "b", "--nosuffix",
		b.vg+"/"+lv,
	)
	if err != nil {
		if isNotExistOutput(out) {
			return 0, &notExistError{lvPath: b.vg + "/" + lv}
		}
		return 0, fmt.Errorf("lvs %s/%s: %w\n%s", b.vg, lv, err, strings.TrimSpace(string(out)))
	}

	sizeStr := strings.TrimSpace(string(out))
	size, parseErr := strconv.ParseInt(sizeStr, 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("lvm: parsing lv_size output %q for %s/%s: %w", sizeStr, b.vg, lv, parseErr)
	}
	return size, nil
}

// createThinLV creates a thin-provisioned logical volume inside thinPool within vg.
//
// The command invoked is:
//
//	lvcreate --virtualsize <sizeBytes>b --thinpool <thinPool> -n <lvName> [extraFlags...] <vg>
//
// The long form --virtualsize is used (rather than the short alias -V) to make
// the intent unambiguous in logs and audit trails.
//
// Idempotency: if lvcreate exits non-zero and its combined output contains
// "already exists", createThinLV treats this as success and returns nil.  The
// caller (Create) first performs an existence check via lvsBytes before calling
// this helper, so in normal operation the LV should not exist yet; the "already
// exists" check is an extra safety net against concurrent creation races.
//
// All other lvcreate errors are wrapped with contextual information (vg, lv
// name, thinpool, size) and returned to the caller.
func (b *Backend) createThinLV(
	ctx context.Context,
	vg, lvName, thinPool string,
	sizeBytes int64,
	extraFlags []string,
) error {
	// Build lvcreate arguments for a thin-provisioned LV.
	// Argument order: flags first, then the positional VG argument last.
	args := make([]string, 0, 6+len(extraFlags)+1)
	args = append(args,
		"--virtualsize", strconv.FormatInt(sizeBytes, 10)+"b",
		"--thinpool", thinPool,
		"-n", lvName,
	)
	// Caller-supplied extra flags are appended before the VG so they can
	// include additional --option value pairs without disrupting argument
	// parsing.
	args = append(args, extraFlags...)
	// VG is always the final positional argument to lvcreate.
	args = append(args, vg)

	out, err := b.exec.run(ctx, "lvcreate", args...)
	if err != nil {
		// Idempotency: if the LV already exists lvcreate will exit non-zero
		// with a message that contains "already exists".  Treat this as
		// success so that repeated CreateVolume calls with the same volumeID
		// are safe even under concurrent execution.
		if isAlreadyExistsOutput(out) {
			return nil
		}
		return fmt.Errorf("lvcreate --virtualsize %s/%s in thinpool %s: %w\n%s",
			vg, lvName, thinPool, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Create provisions an LVM logical volume with at least capacityBytes of storage.
//
// If an LV with this volumeID already exists, Create returns its current device
// path and size without error (idempotent).  If it does not exist, lvcreate is
// called — either for a linear LV or a thin-provisioned LV depending on the
// effective provisioning mode (see below).
//
// Effective provisioning mode selection (highest-priority wins):
//  1. params.GetLvm().GetProvisionMode() — per-volume override from StorageClass
//     or PillarBinding.  "linear" forces a linear LV (ignores backend thinpool);
//     "thin" forces a thin LV (requires backend thinpool to be configured).
//  2. Backend default — thin when the backend was started with thinpool=<name>,
//     linear otherwise.
//
// params is the backend-specific oneof wrapper; the LVM backend reads
// params.GetLvm() if non-nil for:
//   - VGOverride: validated against the backend VG; cross-VG provisioning is
//     not supported and returns an error.
//   - ExtraFlags: additional lvcreate arguments appended verbatim before the
//     final VG positional argument.
//   - ProvisionMode: "linear" or "thin" override (empty = use backend default).
//
// A nil params or nil params.GetLvm() is treated as no per-volume overrides.
//
// The actual allocated size is read back after creation and returned; LVM may
// round the requested size up to the extent boundary.
func (b *Backend) Create(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	params *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, err error) {
	// Validate: capacityBytes must be positive.
	if capacityBytes <= 0 {
		return "", 0, fmt.Errorf("lvm: create %q: capacityBytes must be positive, got %d",
			volumeID, capacityBytes)
	}

	lv := b.lvName(volumeID)

	// Validate: LV name must not be empty (would happen if volumeID == b.vg+"/").
	if lv == "" {
		return "", 0, fmt.Errorf("lvm: create %q: volume name (LV name) must not be empty", volumeID)
	}

	// Parse and validate per-volume LVM parameters from the BackendParams wrapper.
	// A nil params or nil params.GetLvm() is valid — treated as no overrides.
	lvmProto := params.GetLvm()
	if lvmProto != nil {
		modeErr := validateProvisionModeString(lvmProto.GetProvisionMode())
		if modeErr != nil {
			return "", 0, fmt.Errorf("lvm: create %q: %w", volumeID, modeErr)
		}
	}
	lvmParams := ParseParams(lvmProto)
	validateErr := ValidateParams(lvmParams, b.vg, b.thinpool)
	if validateErr != nil {
		return "", 0, fmt.Errorf("lvm: create %q: %w", volumeID, validateErr)
	}

	// Determine the effective thin pool to use for this volume.
	// Start from the backend default (b.thinpool), then apply any per-volume
	// provision_mode override:
	//   - "linear" override → effectiveThinPool = "" (force linear lvcreate path)
	//   - "thin"   override → effectiveThinPool = b.thinpool (already validated non-empty)
	//   - no override       → effectiveThinPool = b.thinpool (backend default)
	effectiveThinPool := b.thinpool
	if lvmParams.hasModeOverride && lvmParams.ProvisionModeOverride == ProvisionModeLinear {
		effectiveThinPool = ""
	}

	// Idempotency check: if the LV already exists and has compatible size, return as-is.
	existing, err := b.lvsBytes(ctx, volumeID)
	if err == nil {
		if existing != capacityBytes {
			return "", existing, &backend.ConflictError{
				VolumeID:       volumeID,
				ExistingBytes:  existing,
				RequestedBytes: capacityBytes,
			}
		}
		return b.DevicePath(volumeID), existing, nil
	}
	if !isNotExist(err) {
		return "", 0, fmt.Errorf("lvm: pre-create existence check for %s/%s: %w", b.vg, lv, err)
	}

	// Dispatch to the appropriate creation helper based on effective provisioning mode.
	if effectiveThinPool != "" {
		// Thin-provisioned LV: delegate to the dedicated helper which uses
		// lvcreate --virtualsize and handles its own idempotency guard.
		createErr := b.createThinLV(ctx, b.vg, lv, effectiveThinPool, capacityBytes, lvmParams.ExtraFlags)
		if createErr != nil {
			return "", 0, createErr
		}
	} else {
		// Linear LV: lvcreate -n <lv> -L <size>b [extraFlags...] <vg>
		args := make([]string, 0, 4+len(lvmParams.ExtraFlags)+1)
		args = append(args,
			"-n", lv,
			"-L", strconv.FormatInt(capacityBytes, 10)+"b",
		)
		// Append any caller-supplied extra flags before the VG positional argument.
		args = append(args, lvmParams.ExtraFlags...)
		// VG is always the final positional argument to lvcreate.
		args = append(args, b.vg)

		out, runErr := b.exec.run(ctx, "lvcreate", args...)
		if runErr != nil {
			return "", 0, fmt.Errorf("lvcreate %s/%s: %w\n%s",
				b.vg, lv, runErr, strings.TrimSpace(string(out)))
		}
	}

	// LVM rounds sizes up to extent boundaries, so read back the actual size.
	allocated, err := b.lvsBytes(ctx, volumeID)
	if err != nil {
		return "", 0, fmt.Errorf("lvm: reading lv_size after create of %s/%s: %w", b.vg, lv, err)
	}

	return b.DevicePath(volumeID), allocated, nil
}

// Delete destroys the LVM logical volume identified by volumeID.
//
// If the LV does not exist, Delete returns nil (idempotent) — this covers both
// the case where the LV was never created and the case where a previous Delete
// call succeeded but the controller retries due to a transient network error.
//
// A volume that is still in use (e.g. open by a device mapper target) will
// cause lvremove to fail; in that case an error is returned so the caller can
// surface it and retry after the consumer has detached.
func (b *Backend) Delete(ctx context.Context, volumeID string) error {
	lv := b.lvName(volumeID)

	// lvremove -y <vg>/<lv>: -y skips the interactive confirmation prompt.
	out, err := b.exec.run(ctx, "lvremove", "-y", b.vg+"/"+lv)
	if err != nil {
		// lvremove exits non-zero when the LV does not exist; the output
		// contains "Failed to find logical volume" or similar.  Treat this
		// as success so the operation is idempotent.
		if isNotExistOutput(out) {
			return nil
		}
		return fmt.Errorf("lvremove %s/%s: %w\n%s", b.vg, lv, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Expand grows the LVM logical volume to at least requestedBytes.
//
// The method validates the request before invoking the LVM CLI:
//   - requestedBytes must be positive (> 0).
//   - The LV must already exist; if it does not, an error is returned.
//   - If the LV is already at or above requestedBytes the call is a no-op
//     and the current size is returned (idempotent).
//
// For actual growth, `lvextend -L <size>b <vg>/<lv>` is called; LVM enforces
// that the new size must be larger than the current size.  The actual size
// after the operation is read back and returned — it may be larger than
// requestedBytes due to LVM extent rounding.
func (b *Backend) Expand(ctx context.Context, volumeID string, requestedBytes int64) (int64, error) {
	lv := b.lvName(volumeID)

	// Validate: requestedBytes must be positive.
	if requestedBytes <= 0 {
		return 0, fmt.Errorf("lvm: expand %s/%s: requestedBytes must be positive, got %d",
			b.vg, lv, requestedBytes)
	}

	// Idempotency check: read the current size.
	currentBytes, err := b.lvsBytes(ctx, volumeID)
	if err != nil {
		return 0, fmt.Errorf("lvm: pre-expand size check for %s/%s: %w", b.vg, lv, err)
	}
	if currentBytes == requestedBytes {
		// Already exactly at the requested size — idempotent no-op.
		return currentBytes, nil
	}
	if currentBytes > requestedBytes {
		// Shrinking a volume is not supported; return an explicit error so
		// operators understand that ExpandVolume can only grow volumes.
		return currentBytes, fmt.Errorf(
			"lvm: expand %s/%s: cannot shrink from %d to %d bytes; "+
				"shrink is not supported by ExpandVolume",
			b.vg, lv, currentBytes, requestedBytes,
		)
	}

	// lvextend -L <size>b <vg>/<lv>
	out, runErr := b.exec.run(ctx, "lvextend",
		"-L", strconv.FormatInt(requestedBytes, 10)+"b",
		b.vg+"/"+lv,
	)
	if runErr != nil {
		return 0, fmt.Errorf("lvextend %s/%s to %d: %w\n%s",
			b.vg, lv, requestedBytes, runErr, strings.TrimSpace(string(out)))
	}

	// Read back the actual size after extent rounding.
	actual, err := b.lvsBytes(ctx, volumeID)
	if err != nil {
		return 0, fmt.Errorf("lvm: reading lv_size after expand of %s/%s: %w", b.vg, lv, err)
	}
	return actual, nil
}

// Capacity returns the total and available byte counts for the volume group (or
// thin pool when in thin provisioning mode).
//
// Linear mode: queries the VG using `vgs --noheadings -o vg_size,vg_free
//
//	--units b --nosuffix <vg>`.
//
// Thin mode: queries the thin pool LV using `lvs --noheadings -o
//
//	lv_size,data_percent --units b --nosuffix <vg>/<thinpool>` and derives
//	available bytes from the LV size and data usage percentage.
func (b *Backend) Capacity(ctx context.Context) (totalBytes, availableBytes int64, err error) {
	if b.thinpool != "" {
		return b.capacityThin(ctx)
	}
	return b.capacityLinear(ctx)
}

// capacityLinear queries the VG for total and free space.
func (b *Backend) capacityLinear(ctx context.Context) (totalBytes, availableBytes int64, err error) {
	out, err := b.exec.run(ctx, "vgs",
		"--noheadings", "-o", "vg_size,vg_free",
		"--units", "b", "--nosuffix",
		b.vg,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("vgs %s: %w\n%s", b.vg, err, strings.TrimSpace(string(out)))
	}

	line := strings.TrimSpace(string(out))
	parts := strings.Fields(line)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("lvm: unexpected vgs output for VG %q: %q", b.vg, line)
	}

	totalBytes, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("lvm: parsing vg_size %q: %w", parts[0], err)
	}
	availableBytes, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("lvm: parsing vg_free %q: %w", parts[1], err)
	}
	return totalBytes, availableBytes, nil
}

// capacityThin queries the thin pool LV for its total size and calculates
// available space from the data usage percentage.
func (b *Backend) capacityThin(ctx context.Context) (totalBytes, availableBytes int64, err error) {
	out, err := b.exec.run(ctx, "lvs",
		"--noheadings", "-o", "lv_size,data_percent",
		"--units", "b", "--nosuffix",
		b.vg+"/"+b.thinpool,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("lvs %s/%s: %w\n%s", b.vg, b.thinpool, err, strings.TrimSpace(string(out)))
	}

	line := strings.TrimSpace(string(out))
	parts := strings.Fields(line)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("lvm: unexpected lvs output for thin pool %s/%s: %q", b.vg, b.thinpool, line)
	}

	totalBytes, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("lvm: parsing lv_size %q for thin pool: %w", parts[0], err)
	}

	dataPercent, parseErr := strconv.ParseFloat(parts[1], 64)
	if parseErr != nil {
		return 0, 0, fmt.Errorf("lvm: parsing data_percent %q for thin pool: %w", parts[1], parseErr)
	}

	usedBytes := int64(float64(totalBytes) * dataPercent / 100.0)
	availableBytes = totalBytes - usedBytes
	return totalBytes, availableBytes, nil
}

// ListVolumes enumerates all LVs managed by pillar-csi in the volume group.
//
// It runs `lvs --noheadings -o lv_name,lv_size --units b --nosuffix <vg>`
// which emits one space-separated line per LV:
//
//	<lv-name>  <sizeBytes>
//
// The -H (--noheadings) flag suppresses the header, --units b and --nosuffix
// request exact byte counts without suffix letters.
//
// If the VG is empty the function returns an empty slice (not an error).
func (b *Backend) ListVolumes(ctx context.Context) ([]*agentv1.VolumeInfo, error) {
	out, err := b.exec.run(ctx, "lvs",
		"--noheadings", "-o", "lv_name,lv_size",
		"--units", "b", "--nosuffix",
		b.vg,
	)
	if err != nil {
		if isNotExistOutput(out) {
			return []*agentv1.VolumeInfo{}, nil
		}
		return nil, fmt.Errorf("lvs %s: %w\n%s", b.vg, err, strings.TrimSpace(string(out)))
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return []*agentv1.VolumeInfo{}, nil
	}

	lines := strings.Split(trimmed, "\n")
	volumes := make([]*agentv1.VolumeInfo, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Skip thin pool LVs themselves (they appear in lvs output too);
		// they are not data volumes managed by pillar-csi.
		if b.thinpool != "" && strings.TrimSpace(strings.Fields(line)[0]) == b.thinpool {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("lvm: unexpected lvs output line %q", line)
		}

		lvName := parts[0]
		sizeStr := parts[1]

		volumeID := b.vg + "/" + lvName

		sizeBytes, parseErr := strconv.ParseInt(sizeStr, 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("lvm: parsing lv_size %q for LV %s/%s: %w", sizeStr, b.vg, lvName, parseErr)
		}

		volumes = append(volumes, &agentv1.VolumeInfo{
			VolumeId:      volumeID,
			CapacityBytes: sizeBytes,
			DevicePath:    b.DevicePath(volumeID),
		})
	}

	return volumes, nil
}
