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

package lvm

// This file implements the optional backend.LVImporter, backend.LVVerifier
// and backend.LVInspector contracts: adopting a pre-existing logical volume
// into a pillar-csi lifecycle (ImportLV), re-checking that a volume ID still
// resolves to the LV pinned at adoption (VerifyLV), and reporting a read-only
// observation of an LV (InspectLV).
//
// All three are strictly read-only on the backend: they run lvs(8) and
// blkid(8) probes and take an O_RDONLY|O_EXCL claim on the device; nothing
// ever calls lvchange, lvcreate, lvextend, lvremove, mkfs, fsck or mount.
//
// ImportLV refuses with *backend.ImportRefusedError in this order:
//
//   - "layout":      want.VolumeGroup differs from the backend's VG, the
//     volume ID's LV name differs from want.LogicalVolume, or the LV name is
//     not a single component;
//   - "missing":     lvs finds no such LV;
//   - "identity":    vg_uuid or lv_uuid differs from want — the name now
//     resolves to a different (renamed, recreated, aliased) LV;
//   - "layout":      segtype is not linear/thin, or the LV lives in a
//     different pool than the backend's configured thin pool;
//   - "wrong type":  the LV is a snapshot, pool, mirror, raid, origin, pvmove
//     or virtual LV (lv_attr[0] or a non-empty origin);
//   - "inactive":    the LV is not active — import never auto-activates;
//   - "too small":   lv_size below the requested capacity;
//   - "in use":      an exclusive O_EXCL claim fails (EBUSY), or devidle
//     finds a mount, holder, or configured LIO/nvmet export;
//   - "identity":    the held descriptor's device number differs from
//     lv_kernel_major:lv_kernel_minor (alias reuse or a race).
//
// The exclusive claim is held through the idle scan and a second lvs probe;
// the import succeeds only when the identity is unchanged on re-check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/devidle"
)

// Refusal reason codes recorded on backend.ImportRefusedError so operators
// can filter the refusal class without parsing the detail text.  The
// "identity" reason is backend.ImportRefusedReasonIdentity.
const (
	reasonLVLayout   = "layout"
	reasonLVMissing  = "missing"
	reasonLVWrongTyp = "wrong type"
	reasonLVInactive = "inactive"
	reasonLVTooSmall = "too small"
	reasonLVInUse    = "in use"
)

// lvsImportColumns is the exact column set the identity query requests.
// The lv_kernel_major/lv_kernel_minor columns pin the device the name
// resolved to so an alias or a same-name replacement can be detected.
const lvsImportColumns = "lv_name,vg_name,lv_uuid,vg_uuid,lv_attr,segtype,pool_lv,origin," +
	"lv_size,lv_kernel_major,lv_kernel_minor"

// Compile-time checks that Backend implements the optional LV contracts.
var (
	_ backend.LVImporter  = (*Backend)(nil)
	_ backend.LVVerifier  = (*Backend)(nil)
	_ backend.LVInspector = (*Backend)(nil)
)

// errLVNotFound is returned internally by lvsRow when the report contains
// no LV; callers translate it into a "missing" refusal.
var errLVNotFound = errors.New("lvm: lvs report lists no logical volume")

// lvsRow is one lv entry of an `lvs --reportformat json` document.  Every
// field arrives as a string, including lv_size and the kernel device
// numbers.
type lvsRow struct {
	LVName        string `json:"lv_name"`
	VGName        string `json:"vg_name"`
	LVUUID        string `json:"lv_uuid"`
	VGUUID        string `json:"vg_uuid"`
	LVAttr        string `json:"lv_attr"`
	Segtype       string `json:"segtype"`
	PoolLV        string `json:"pool_lv"`
	Origin        string `json:"origin"`
	LVSize        string `json:"lv_size"`
	LVKernelMajor string `json:"lv_kernel_major"`
	LVKernelMinor string `json:"lv_kernel_minor"`
}

// identity returns the observed name/UUID identity of this row.
func (r lvsRow) identity() backend.LVMIdentity {
	return backend.LVMIdentity{
		VolumeGroup:       r.VGName,
		LogicalVolume:     r.LVName,
		VolumeGroupUUID:   r.VGUUID,
		LogicalVolumeUUID: r.LVUUID,
	}
}

// sizeBytes parses lv_size.  With --units b --nosuffix it is a plain integer;
// a non-numeric value fails the whole observation rather than guessing.
func (r lvsRow) sizeBytes() (int64, error) {
	size, err := strconv.ParseInt(strings.TrimSpace(r.LVSize), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("lvm: parsing lv_size %q for %s/%s: %w", r.LVSize, r.VGName, r.LVName, err)
	}
	return size, nil
}

// lvKernelDev is the kernel device an lvs row reports for its LV.
type lvKernelDev struct {
	// majorMinor is the "major:minor" spelling of the device number.
	majorMinor string
	// rdev is the Linux-encoded device number.
	rdev uint64
}

// kernelDev parses lv_kernel_major:lv_kernel_minor, with ok=false when the
// LV has no kernel device (lvs reports "-1", or "" for an inactive LV).  Any
// other unparseable value fails the observation rather than being read as
// "no device".
func (r lvsRow) kernelDev() (dev lvKernelDev, ok bool, err error) {
	mjRaw := strings.TrimSpace(r.LVKernelMajor)
	mnRaw := strings.TrimSpace(r.LVKernelMinor)
	if mjRaw == "-1" || mjRaw == "" || mnRaw == "-1" || mnRaw == "" {
		return lvKernelDev{}, false, nil
	}
	mj, err1 := strconv.ParseUint(mjRaw, 10, 32)
	mn, err2 := strconv.ParseUint(mnRaw, 10, 32)
	if err1 != nil || err2 != nil {
		return lvKernelDev{}, false, fmt.Errorf("lvm: parsing lv_kernel_major:lv_kernel_minor %q:%q for %s/%s: %w",
			mjRaw, mnRaw, r.VGName, r.LVName, errors.Join(err1, err2))
	}
	return lvKernelDev{
		majorMinor: fmt.Sprintf("%d:%d", mj, mn),
		rdev:       devidle.EncodeLinuxDev(mj, mn),
	}, true, nil
}

// active reports whether the LV is active: lv_attr bit 5 is 'a'.
// A malformed attr is not active — never guess the device is usable.
func (r lvsRow) active() bool {
	return len(r.LVAttr) > 4 && r.LVAttr[4] == 'a'
}

// lvsReport is the JSON document lvs --reportformat json emits.
type lvsReport struct {
	Report []struct {
		LV []lvsRow `json:"lv"`
	} `json:"report"`
}

// parseLVSReport extracts the single LV row of an lvs JSON report.  More
// than one reported LV is an internal error: the query names exactly one
// <vg>/<lv>, so a multi-row answer means the observation is not what the
// caller asked about.
func parseLVSReport(volumeID string, out []byte) (lvsRow, error) {
	var doc lvsReport
	err := json.Unmarshal(out, &doc)
	if err != nil {
		return lvsRow{}, fmt.Errorf("lvm: parsing lvs JSON for %q: %w", volumeID, err)
	}
	var rows []lvsRow
	for _, rep := range doc.Report {
		rows = append(rows, rep.LV...)
	}
	switch len(rows) {
	case 0:
		return lvsRow{}, errLVNotFound
	case 1:
		return rows[0], nil
	default:
		return lvsRow{}, fmt.Errorf("lvm: lvs report for %q lists %d LVs, expected exactly one",
			volumeID, len(rows))
	}
}

// lvsRow runs the single identity query for the LV behind volumeID and
// returns its row.  A missing LV yields errLVNotFound; any other lvs failure
// is an internal error, never a silent "missing".
func (b *Backend) lvsRow(ctx context.Context, volumeID string) (lvsRow, error) {
	lv := b.lvName(volumeID)
	out, err := b.exec.run(ctx, "lvs",
		"--reportformat", "json",
		"--units", "b", "--nosuffix",
		"-o", lvsImportColumns,
		b.vg+"/"+lv,
	)
	if err != nil {
		if isNotExistOutput(out) {
			return lvsRow{}, errLVNotFound
		}
		return lvsRow{}, fmt.Errorf("lvs %s/%s: %w\n%s",
			b.vg, lv, err, strings.TrimSpace(string(out)))
	}
	row, err := parseLVSReport(volumeID, out)
	if err != nil {
		return lvsRow{}, err
	}
	// The query names b.vg/<lv>; a row naming anything else means the
	// observation does not describe the requested LV — an internal
	// inconsistency, not evidence about the volume.
	if row.VGName != b.vg || row.LVName != lv {
		return lvsRow{}, fmt.Errorf("lvm: lvs report for %q returned %s/%s, expected %s/%s",
			volumeID, row.VGName, row.LVName, b.vg, lv)
	}
	return row, nil
}

// lookupLV runs the identity query for op ("import", "verify", "inspect"),
// refusing "missing" when lvs reports no such LV.  Any other lvs failure is
// an internal error, never a refusal.
func (b *Backend) lookupLV(ctx context.Context, volumeID, op string) (lvsRow, error) {
	row, err := b.lvsRow(ctx, volumeID)
	if err != nil {
		if errors.Is(err, errLVNotFound) {
			return lvsRow{}, refuseImport(volumeID, reasonLVMissing,
				"LV %s does not exist", b.vg+"/"+b.lvName(volumeID))
		}
		return lvsRow{}, fmt.Errorf("lvm: %s %q: %w", op, volumeID, err)
	}
	return row, nil
}

// refuseImport builds the *backend.ImportRefusedError for a refused
// adoption or verification of volumeID.
func refuseImport(volumeID, reason, format string, args ...any) *backend.ImportRefusedError {
	return &backend.ImportRefusedError{
		VolumeID: volumeID,
		Reason:   reason,
		Detail:   fmt.Sprintf(format, args...),
	}
}

// checkImportLayout applies the request-side layout checks: the named VG
// must be this backend's, the volume ID must resolve to want.LogicalVolume,
// and that name must be a single component.  It runs before any lvs call.
func (b *Backend) checkImportLayout(volumeID string, want backend.LVMIdentity) error {
	if want.VolumeGroup != b.vg {
		return refuseImport(volumeID, reasonLVLayout,
			"expected volume group %q is not this backend's volume group %q; "+
				"the claim names an LV outside this PillarStore",
			want.VolumeGroup, b.vg)
	}
	lv := b.lvName(volumeID)
	if lv != want.LogicalVolume {
		return refuseImport(volumeID, reasonLVLayout,
			"volume ID %q resolves to LV %q but the expected source names %q; "+
				"the agent must adopt exactly the LV the claim names",
			volumeID, lv, want.LogicalVolume)
	}
	return b.checkLVComponent(volumeID, lv)
}

// checkLVComponent refuses with reason "layout" when lv is not a valid,
// single-component LV name.
func (b *Backend) checkLVComponent(volumeID, lv string) error {
	err := ValidateLVName(lv)
	if err == nil && strings.ContainsAny(lv, `/\`) {
		err = fmt.Errorf("name %q is not a single LV component", lv) //nolint:goerr113 // refusal detail
	}
	if err != nil {
		return refuseImport(volumeID, reasonLVLayout,
			"LV name %q is not a single component in volume group %q: %v", lv, b.vg, err)
	}
	return nil
}

// checkIdentity refuses with reason "identity" when the observed row's
// UUIDs differ from want.
func checkIdentity(volumeID string, row lvsRow, want backend.LVMIdentity) error {
	switch {
	case row.VGUUID != want.VolumeGroupUUID:
		return refuseImport(volumeID, backend.ImportRefusedReasonIdentity,
			"volume group UUID %q of %s/%s differs from the expected %q; "+
				"the name now resolves to a different VG (recreated or aliased)",
			row.VGUUID, row.VGName, row.LVName, want.VolumeGroupUUID)
	case row.LVUUID != want.LogicalVolumeUUID:
		return refuseImport(volumeID, backend.ImportRefusedReasonIdentity,
			"LV UUID %q of %s/%s differs from the expected %q; "+
				"the name now resolves to a different LV (renamed, recreated or aliased)",
			row.LVUUID, row.VGName, row.LVName, want.LogicalVolumeUUID)
	}
	return nil
}

// lvWrongTypeAttr lists lv_attr[0] values that can never be adopted: a
// snapshot (s/S), thin or thin-pool metadata (t/T), mirror (m), raid (r/R),
// origin volume (o/O), pvmove (p) or virtual (v) LV.
const lvWrongTypeAttr = "sStTmrRoOpv"

// checkLVLayout verifies the observed row is an adoptable LV in this
// backend's layout: a linear or thin segment, not a snapshot/pool/mirror/
// raid/origin/virtual LV, and resident in the backend's configured thin
// pool when it has one.
func (b *Backend) checkLVLayout(volumeID string, row lvsRow) error {
	if row.Segtype != segtypeLinear && row.Segtype != segtypeThin {
		return refuseImport(volumeID, reasonLVLayout,
			"LV %s/%s has segment type %q; only linear and thin LVs are adoptable",
			row.VGName, row.LVName, row.Segtype)
	}
	if row.LVAttr == "" {
		return refuseImport(volumeID, reasonLVWrongTyp,
			"LV %s/%s reports an empty lv_attr; its type cannot be determined",
			row.VGName, row.LVName)
	}
	if strings.ContainsRune(lvWrongTypeAttr, rune(row.LVAttr[0])) {
		return refuseImport(volumeID, reasonLVWrongTyp,
			"LV %s/%s has lv_attr %q: snapshots, pools, mirrors, raids, origin, "+
				"pvmove and virtual LVs cannot be adopted",
			row.VGName, row.LVName, row.LVAttr)
	}
	if row.Origin != "" {
		return refuseImport(volumeID, reasonLVWrongTyp,
			"LV %s/%s is a snapshot of %q; snapshot LVs cannot be adopted",
			row.VGName, row.LVName, row.Origin)
	}
	if b.thinpool != "" {
		if row.Segtype != segtypeThin || row.PoolLV != b.thinpool {
			return refuseImport(volumeID, reasonLVLayout,
				"LV %s/%s (segtype %q, pool %q) is not a thin LV of this backend's "+
					"thin pool %q", row.VGName, row.LVName, row.Segtype, row.PoolLV, b.thinpool)
		}
	} else if row.Segtype != segtypeLinear {
		return refuseImport(volumeID, reasonLVLayout,
			"LV %s/%s is a %q LV but this backend has no thin pool configured; "+
				"only linear LVs are adoptable",
			row.VGName, row.LVName, row.Segtype)
	}
	return nil
}

// importCandidate is an LV that passed every lvs-only import check: its row,
// its kernel device, and its size in bytes.
type importCandidate struct {
	row  lvsRow
	dev  lvKernelDev
	size int64
}

// ImportLV implements backend.LVImporter.  See the file header for the
// ordered refusal contract; the method is strictly read-only.
func (b *Backend) ImportLV(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	want backend.LVMIdentity,
) (devicePath string, sizeBytes int64, err error) {
	// Steps 1-6 decide from the request and one lvs query; no claim yet.
	cand, err := b.checkImportCandidate(ctx, volumeID, capacityBytes, want)
	if err != nil {
		return "", 0, err
	}

	// 7. Exclusive claim boundary, held through the re-verification in
	// step 9.  While held the kernel refuses competing exclusive claims
	// (mount, dm table, export backstore, another O_EXCL opener).
	devPath := b.DevicePath(volumeID)
	claim, err := b.claimDevice(devPath)
	if err != nil {
		if errors.Is(err, devidle.ErrDeviceHeld) {
			return "", 0, refuseImport(volumeID, reasonLVInUse,
				"device %q is held exclusively on the storage node "+
					"(mounted, exported, or opened by another consumer)", devPath)
		}
		return "", 0, fmt.Errorf("lvm: import %q: exclusive claim on %q: %w", volumeID, devPath, err)
	}
	defer func() {
		relErr := claim.Release()
		if relErr != nil {
			err = errors.Join(err, fmt.Errorf("lvm: import %q: release claim on %q: %w",
				volumeID, devPath, relErr))
		}
		if err != nil {
			devicePath, sizeBytes = "", 0
		}
	}()

	// 7b. The descriptor we hold must be the device lvs reported: an alias
	// or a raced rename must fail the import, not adopt a different LV.
	if claim.Rdev() != cand.dev.rdev {
		mj, mn := devidle.DecodeLinuxDev(claim.Rdev())
		return "", 0, refuseImport(volumeID, backend.ImportRefusedReasonIdentity,
			"device %q is kernel device %d:%d but %s/%s reports %s; "+
				"the path aliases a different LV",
			devPath, mj, mn, cand.row.VGName, cand.row.LVName, cand.dev.majorMinor)
	}

	// 8. Non-exclusive consumers the O_EXCL claim cannot see.
	err = b.checkImportIdle(volumeID, devPath, claim.Rdev())
	if err != nil {
		return "", 0, err
	}

	// 9. Re-verify: the identity must be unchanged while the claim was
	// held, so a same-name replacement can never slip through.
	err = b.reverifyImport(ctx, volumeID, want, cand.dev.majorMinor)
	if err != nil {
		return "", 0, err
	}
	return devPath, cand.size, nil
}

// checkImportCandidate runs import steps 1-6: the request-side layout
// checks, one lvs identity query, exact UUID identity, observed layout and
// type, activity with a kernel device, and capacity.
func (b *Backend) checkImportCandidate(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	want backend.LVMIdentity,
) (importCandidate, error) {
	// 1. Request-side layout checks, before any lvs call.
	err := b.checkImportLayout(volumeID, want)
	if err != nil {
		return importCandidate{}, err
	}

	// 2. One lvs identity query.
	row, err := b.lookupLV(ctx, volumeID, "import")
	if err != nil {
		return importCandidate{}, err
	}

	// 3. Exact UUID identity.
	err = checkIdentity(volumeID, row, want)
	if err != nil {
		return importCandidate{}, err
	}

	// 4. Observed layout and type.
	err = b.checkLVLayout(volumeID, row)
	if err != nil {
		return importCandidate{}, err
	}

	// 5. The LV must already be active; import never runs lvchange.
	if !row.active() {
		return importCandidate{}, refuseImport(volumeID, reasonLVInactive,
			"LV %s/%s is not active (lv_attr %q); activate it explicitly first — "+
				"import never auto-activates", row.VGName, row.LVName, row.LVAttr)
	}
	dev, hasDev, err := row.kernelDev()
	if err != nil {
		return importCandidate{}, fmt.Errorf("lvm: import %q: %w", volumeID, err)
	}
	if !hasDev {
		return importCandidate{}, fmt.Errorf(
			"lvm: import %q: active LV %s/%s reports no kernel device number "+
				"(lv_kernel_major=%q); the lvs observation is inconsistent",
			volumeID, row.VGName, row.LVName, row.LVKernelMajor)
	}

	// 6. Capacity: import never resizes.
	size, err := row.sizeBytes()
	if err != nil {
		return importCandidate{}, fmt.Errorf("lvm: import %q: %w", volumeID, err)
	}
	if size < capacityBytes {
		return importCandidate{}, refuseImport(volumeID, reasonLVTooSmall,
			"LV %s/%s is %d bytes, requested capacity is %d bytes; import never resizes",
			row.VGName, row.LVName, size, capacityBytes)
	}
	return importCandidate{row: row, dev: dev, size: size}, nil
}

// checkImportIdle runs the devidle scan for the claimed device rdev,
// refusing "in use" on any finding.  A probe failure is an internal error.
func (b *Backend) checkImportIdle(volumeID, devPath string, rdev uint64) error {
	err := b.idleChecker().Check(volumeID, rdev)
	if err == nil {
		return nil
	}
	if inUse, isInUse := errors.AsType[*devidle.InUseError](err); isInUse {
		return refuseImport(volumeID, reasonLVInUse, "%s", inUse.Error())
	}
	return fmt.Errorf("lvm: import %q: idle check on %q: %w", volumeID, devPath, err)
}

// reverifyImport repeats the identity query while the claim is held and
// refuses unless the LV still has the wanted UUIDs and kernel device devMM.
func (b *Backend) reverifyImport(ctx context.Context, volumeID string, want backend.LVMIdentity, devMM string) error {
	row, err := b.lvsRow(ctx, volumeID)
	if err != nil {
		if errors.Is(err, errLVNotFound) {
			return refuseImport(volumeID, reasonLVMissing,
				"LV %s disappeared while the exclusive claim was held",
				b.vg+"/"+b.lvName(volumeID))
		}
		return fmt.Errorf("lvm: import %q: re-verify: %w", volumeID, err)
	}
	err = checkIdentity(volumeID, row, want)
	if err != nil {
		return err
	}
	dev, hasDev, err := row.kernelDev()
	if err != nil {
		return fmt.Errorf("lvm: import %q: re-verify: %w", volumeID, err)
	}
	if !hasDev || dev.majorMinor != devMM {
		return refuseImport(volumeID, backend.ImportRefusedReasonIdentity,
			"LV %s/%s kernel device changed from %s to %q during the claim; "+
				"the LV was replaced while importing", row.VGName, row.LVName, devMM, dev.majorMinor)
	}
	return nil
}

// VerifyLV implements backend.LVVerifier: it re-runs the identity query and
// refuses "missing" when the LV is gone or "identity" when any of the four
// identity fields differs.  It never guesses ownership — the caller passes
// the pinned identity observed at import.
func (b *Backend) VerifyLV(ctx context.Context, volumeID string, want backend.LVMIdentity) error {
	err := b.checkImportLayout(volumeID, want)
	if err != nil {
		return err
	}
	row, err := b.lookupLV(ctx, volumeID, "verify")
	if err != nil {
		return err
	}
	return checkIdentity(volumeID, row, want)
}

// InspectLV implements backend.LVInspector: a read-only observation of the
// LV behind volumeID.  It runs the same lvs query, a blkid signature probe
// (only on an active LV; blkid -p reads without writing), one ephemeral
// O_EXCL claim for ExclusiveClaim, and a devidle report for consumers and
// configured exports.  It never activates, mounts or writes.  An
// inconclusive signature probe is reported as FSProbeUnknown, never as a
// blank device; a blkid execution or format failure fails the inspect.
func (b *Backend) InspectLV(ctx context.Context, volumeID string) (backend.LVObservation, error) {
	err := b.checkLVComponent(volumeID, b.lvName(volumeID))
	if err != nil {
		return backend.LVObservation{}, err
	}
	row, err := b.lookupLV(ctx, volumeID, "inspect")
	if err != nil {
		return backend.LVObservation{}, err
	}
	size, err := row.sizeBytes()
	if err != nil {
		return backend.LVObservation{}, fmt.Errorf("lvm: inspect %q: %w", volumeID, err)
	}
	dev, hasDev, err := row.kernelDev()
	if err != nil {
		return backend.LVObservation{}, fmt.Errorf("lvm: inspect %q: %w", volumeID, err)
	}
	obs := backend.LVObservation{
		Identity:       row.identity(),
		Attr:           row.LVAttr,
		Segtype:        row.Segtype,
		PoolLV:         row.PoolLV,
		Origin:         row.Origin,
		SizeBytes:      size,
		Active:         row.active(),
		ExclusiveClaim: backend.ExclusiveClaimUnknown,
	}
	if !obs.Active {
		obs.FSProbe = backend.FSProbeUnknown
		obs.FSProbeError = "the LV is inactive; no signature probe ran"
		return obs, nil
	}
	if !hasDev {
		return backend.LVObservation{}, fmt.Errorf(
			"lvm: inspect %q: active LV %s/%s reports no kernel device number",
			volumeID, row.VGName, row.LVName)
	}
	err = b.observeActiveLV(ctx, volumeID, dev, &obs)
	if err != nil {
		return backend.LVObservation{}, err
	}
	return obs, nil
}

// observeActiveLV fills obs with what InspectLV observes on the device of
// an active LV: its signature probe, an ephemeral exclusive claim, and the
// devidle report of consumers and configured exports.
func (b *Backend) observeActiveLV(
	ctx context.Context,
	volumeID string,
	dev lvKernelDev,
	obs *backend.LVObservation,
) error {
	devPath := b.DevicePath(volumeID)
	obs.DevicePath = devPath
	obs.DevMajorMinor = dev.majorMinor
	probe, err := b.blkidProbe(ctx, devPath)
	if err != nil {
		return fmt.Errorf("lvm: inspect %q: blkid %q: %w", volumeID, devPath, err)
	}
	obs.FSType, obs.FSUUID = probe.fsType, probe.fsUUID
	obs.FSProbe, obs.FSProbeError = probe.state, probe.detail

	// One ephemeral exclusive claim, closed immediately with no data
	// read: free only when the open succeeded on exactly the device lvs
	// reported; busy on EBUSY; unknown for any other result.
	claim, err := b.claimDevice(devPath)
	switch {
	case err == nil:
		if claim.Rdev() == dev.rdev {
			obs.ExclusiveClaim = backend.ExclusiveClaimFree
		}
		relErr := claim.Release()
		if relErr != nil {
			return fmt.Errorf("lvm: inspect %q: release claim on %q: %w", volumeID, devPath, relErr)
		}
	case errors.Is(err, devidle.ErrDeviceHeld):
		obs.ExclusiveClaim = backend.ExclusiveClaimBusy
	}

	// Non-exclusive consumers and configured exports of the device lvs
	// reported, in report mode.  A probe error fails the inspect: an
	// empty list must only ever mean "observed none".
	findings, err := b.idleChecker().Report(dev.rdev)
	if err != nil {
		return fmt.Errorf("lvm: inspect %q: idle report: %w", volumeID, err)
	}
	for _, f := range findings {
		switch f.Kind {
		case devidle.KindExport:
			detail := f.TargetID
			if detail == "" {
				detail = f.Detail
			}
			obs.Exports = append(obs.Exports, backend.DeviceConsumer{
				Kind: devidle.KindExport, Detail: detail,
			})
		default:
			obs.Consumers = append(obs.Consumers, backend.DeviceConsumer{
				Kind: f.Kind, Detail: f.Detail,
			})
		}
	}
	return nil
}

// fsProbeResult is one blkid signature probe outcome: state is
// backend.FSProbeDetected with fsType (and fsUUID when reported), or
// backend.FSProbeUnknown with detail explaining why nothing is known.
type fsProbeResult struct {
	state  string
	fsType string
	fsUUID string
	detail string
}

// blkidProbe runs `blkid -p -o export -c /dev/null <dev>` (util-linux
// low-level probe, export format) and classifies what it reports.
//
//   - exit 0 with well-formed export records (see parseBlkidExport) naming
//     devPath in DEVNAME and carrying a TYPE is FSProbeDetected with that
//     TYPE and UUID;
//   - exit 0 with well-formed records naming devPath but no TYPE (a
//     partition table only: PTTYPE/PTUUID, or other TYPE-less records) is
//     FSProbeUnknown: the device holds something the probe does not
//     classify as a filesystem, which is neither blank nor an error;
//   - exit 2 with empty output is FSProbeUnknown, never "no signature":
//     util-linux lowprobe_device exits 2 without any diagnostic both when
//     a complete scan found nothing and when the low-level probe itself
//     failed (e.g. EIO reading the device), so blankness is not proven;
//   - exit 0 with empty output, exit 2 with output, malformed records, a
//     DEVNAME other than devPath, any other exit status, or a failure to
//     run blkid at all is an error.
//
// Output in another format — e.g. BusyBox blkid, which ignores -p/-o/-c and
// prints `<dev>: UUID="..." TYPE="..."` with exit 0 — is therefore an
// error, never an observation.
func (b *Backend) blkidProbe(ctx context.Context, devPath string) (fsProbeResult, error) {
	out, runErr := b.exec.run(ctx, "blkid", "-p", "-o", "export", "-c", "/dev/null", devPath)
	text := strings.TrimSpace(string(out))
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 2 {
			if text == "" {
				return fsProbeResult{state: backend.FSProbeUnknown, detail: fmt.Sprintf(
					"blkid -p %s: %v with no output: util-linux reports both a scan that "+
						"found no signature and a failed low-level probe this way, so the "+
						"device is not proven blank", devPath, runErr)}, nil
			}
			return fsProbeResult{}, fmt.Errorf("%w: exit 2 (no signature) with unexpected output %q", runErr, text)
		}
		return fsProbeResult{}, fmt.Errorf("%w\n%s", runErr, text)
	}
	if text == "" {
		return fsProbeResult{}, errors.New("exit 0 with empty output: not util-linux " +
			"`blkid -p -o export` (which reports no signature as exit 2)")
	}
	rec, err := parseBlkidExport(text)
	if err != nil {
		return fsProbeResult{}, err
	}
	if dev := rec["DEVNAME"]; dev != devPath {
		return fsProbeResult{}, fmt.Errorf("export output names DEVNAME %q, want %q; output %q", dev, devPath, text)
	}
	if fsType := rec["TYPE"]; fsType != "" {
		return fsProbeResult{state: backend.FSProbeDetected, fsType: fsType, fsUUID: rec["UUID"]}, nil
	}
	if pt := rec["PTTYPE"]; pt != "" {
		return fsProbeResult{state: backend.FSProbeUnknown, detail: fmt.Sprintf(
			"blkid -p %s: partition table %q found but no filesystem TYPE; the device "+
				"is not unformatted; output %q", devPath, pt, text)}, nil
	}
	return fsProbeResult{state: backend.FSProbeUnknown, detail: fmt.Sprintf(
		"blkid -p %s: signature records without a filesystem TYPE; the device "+
			"is not unformatted; output %q", devPath, text)}, nil
}

// parseBlkidExport parses util-linux `blkid -o export` output: every
// non-empty line must be KEY=VALUE with an upper-case KEY
// ([A-Z][A-Z0-9_]*).  Values of other keys (LABEL, PART_ENTRY_NAME and the
// like) are kept as printed and not validated.  The fields the probe relies on
// (DEVNAME, TYPE, UUID, PTTYPE) must not repeat and must be plain non-empty
// tokens: valid UTF-8 without whitespace, quotes, backslash escapes or
// control characters.  Any deviation is an error quoting the offending line.
func parseBlkidExport(text string) (map[string]string, error) {
	rec := make(map[string]string)
	for raw := range strings.Lines(text) {
		line := strings.TrimRight(raw, "\r\n")
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found || !validBlkidKey(k) {
			return nil, fmt.Errorf("malformed export record %q: want KEY=VALUE with an upper-case KEY", line)
		}
		switch k {
		case "DEVNAME", "TYPE", "UUID", "PTTYPE":
			if _, dup := rec[k]; dup {
				return nil, fmt.Errorf("duplicate export key %q in record %q", k, line)
			}
			if !plainBlkidToken(v) {
				return nil, fmt.Errorf("malformed export record %q: %s must be a non-empty plain token", line, k)
			}
		}
		rec[k] = v
	}
	return rec, nil
}

func validBlkidKey(k string) bool {
	if k == "" || k[0] < 'A' || k[0] > 'Z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func plainBlkidToken(v string) bool {
	if v == "" || !utf8.ValidString(v) {
		return false
	}
	return !strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("\"'\\", r)
	})
}
