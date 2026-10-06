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

package lvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/devidle"
	"github.com/isac322/pillar-csi/internal/agent/backend/lvm"
)

// Identity observed on the issue-163 QA guest (Ubuntu 24.04, kernel 6.8,
// LVM 2.03.16) for VG pillar163qa: `lvs --reportformat json --units b
// --nosuffix -o lv_name,vg_name,lv_uuid,vg_uuid,lv_attr,segtype,pool_lv,
// origin,lv_size,lv_kernel_major,lv_kernel_minor`.  The rows below are that
// output verbatim; every value, including lv_size and the kernel device
// numbers, is a JSON string.
const (
	impVG          = "pillar163qa"
	impVGUUID      = "nWurVS-h0BR-rMYX-OTac-D0Hc-22K9-1f3ieC"
	impLinearUUID  = "w5E1tT-RaLr-jwEQ-8wxS-HY1V-eLao-uwKADF"
	impThinUUID    = "c7exN5-uNv7-hdH2-c2W1-vdqg-79i8-ltcJHj"
	impLinearID    = impVG + "/legacy-linear"
	impThinID      = impVG + "/legacy-thin"
	impLVSize      = int64(134217728)
	impLinearPath  = "/dev/" + impVG + "/legacy-linear"
	impOtherLVUUID = "AAAAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAAAA"
)

func impLinearRow() map[string]string {
	return map[string]string{
		"lv_name": "legacy-linear", "vg_name": impVG,
		"lv_uuid": impLinearUUID, "vg_uuid": impVGUUID,
		"lv_attr": "-wi-a-----", "segtype": "linear", "pool_lv": "", "origin": "",
		"lv_size": "134217728", "lv_kernel_major": "252", "lv_kernel_minor": "0",
	}
}

func impThinRow() map[string]string {
	return map[string]string{
		"lv_name": "legacy-thin", "vg_name": impVG,
		"lv_uuid": impThinUUID, "vg_uuid": impVGUUID,
		"lv_attr": "Vwi-a-tz--", "segtype": "thin", "pool_lv": "pool", "origin": "",
		"lv_size": "134217728", "lv_kernel_major": "252", "lv_kernel_minor": "5",
	}
}

func impWith(row map[string]string, kv ...string) map[string]string {
	out := maps.Clone(row)
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// impDoc renders rows as an lvs JSON report document.
func impDoc(t *testing.T, rows ...map[string]string) string {
	t.Helper()
	if rows == nil {
		rows = []map[string]string{}
	}
	doc := map[string]any{"report": []any{map[string]any{"lv": rows}}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal lvs doc: %v", err)
	}
	return string(data)
}

func impWant(id string) backend.LVMIdentity {
	switch id {
	case impThinID:
		return backend.LVMIdentity{VolumeGroup: impVG, LogicalVolume: "legacy-thin",
			VolumeGroupUUID: impVGUUID, LogicalVolumeUUID: impThinUUID}
	default:
		return backend.LVMIdentity{VolumeGroup: impVG, LogicalVolume: "legacy-linear",
			VolumeGroupUUID: impVGUUID, LogicalVolumeUUID: impLinearUUID}
	}
}

// impDevice is a stateful block-device fake: it tracks how many exclusive
// claims are held, so a test can prove the claim spans the re-verification
// and is released exactly once.
type impDevice struct {
	mu         sync.Mutex
	rdev       uint64
	busy       bool
	openErr    error
	releaseErr error
	paths      []string
	held       int
	releases   int
}

type impClaim struct {
	d        *impDevice
	released bool
}

func (c *impClaim) Rdev() uint64 { return c.d.rdev }

func (c *impClaim) Release() error {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	if !c.released {
		c.released = true
		c.d.held--
		c.d.releases++
	}
	return c.d.releaseErr
}

func (d *impDevice) claim(path string) (devidle.Claim, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths = append(d.paths, path)
	if d.busy {
		return nil, errors.Join(errors.New("exclusive open "+path), devidle.ErrDeviceHeld)
	}
	if d.openErr != nil {
		return nil, d.openErr
	}
	d.held++
	return &impClaim{d: d}, nil
}

func (d *impDevice) heldNow() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.held
}

// impExec answers lvs from a queue (the last entry repeats) and blkid from
// fixed output, and records every command.  The blkid output defaults
// (newImpFixture) to the util-linux export record of an ext4 filesystem on
// impLinearPath; tests override blkidOut/blkidErr for other probe outcomes,
// e.g. exit 2 with empty output for an unknown signature.  Anything else is
// a mutation the read-only contract forbids; it fails and is reported by
// mutations().
type impExec struct {
	mu        sync.Mutex
	lvs       []string
	lvsErr    error
	lvsErrOut string
	blkidOut  string
	blkidErr  error
	dev       *impDevice
	calls     [][]string
	heldAtLVS []int
}

func (e *impExec) run(_ context.Context, name string, args ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, append([]string{name}, args...))
	switch name {
	case "lvs":
		e.heldAtLVS = append(e.heldAtLVS, e.dev.heldNow())
		if e.lvsErr != nil {
			return []byte(e.lvsErrOut), e.lvsErr
		}
		out := e.lvs[0]
		if len(e.lvs) > 1 {
			e.lvs = e.lvs[1:]
		}
		return []byte(out), nil
	case "blkid":
		return []byte(e.blkidOut), e.blkidErr
	default:
		return nil, errors.New("unexpected command " + name)
	}
}

func (e *impExec) count(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if c[0] == name {
			n++
		}
	}
	return n
}

func (e *impExec) mutations() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out [][]string
	for _, c := range e.calls {
		if c[0] != "lvs" && c[0] != "blkid" {
			out = append(out, c)
		}
	}
	return out
}

type impFixture struct {
	b    *lvm.Backend
	exec *impExec
	dev  *impDevice
	root string
	stat map[string]uint64
}

// impDMMajor is the device-mapper major number every fixture row reports in
// lv_kernel_major.
const impDMMajor = 252

// newImpFixture builds a backend for VG pillar163qa whose exclusive claims go
// to a device fake with kernel number impDMMajor:minor, and whose idle scan
// reads real (initially empty) kernel trees under a temp root.
func newImpFixture(t *testing.T, thinpool string, minor uint64, lvsDocs ...string) *impFixture {
	t.Helper()
	dev := &impDevice{rdev: devidle.EncodeLinuxDev(impDMMajor, minor)}
	ex := &impExec{lvs: lvsDocs, dev: dev, blkidOut: impUtilLinuxExt4Export()}
	b := lvm.NewWithExecFn(impVG, thinpool, ex.run)
	lvm.SetBackendClaimDevice(t, b, dev.claim)
	fx := &impFixture{b: b, exec: ex, dev: dev, root: t.TempDir(), stat: map[string]uint64{}}
	lvm.SetBackendIdleChecker(t, b, devidle.Checker{
		Roots: devidle.Roots{
			MountinfoPath: filepath.Join(fx.root, "mountinfo"),
			SysDevBlock:   filepath.Join(fx.root, "sys", "dev", "block"),
			ConfigfsRoot:  filepath.Join(fx.root, "config"),
		},
		StatRdev: func(path string) (uint64, error) {
			if rdev, ok := fx.stat[path]; ok {
				return rdev, nil
			}
			return 0, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
		},
	})
	return fx
}

func (fx *impFixture) writeFile(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(fx.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fx *impFixture) mkdir(t *testing.T, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fx.root, rel), 0o750); err != nil {
		t.Fatal(err)
	}
}

// addLIOExport configures a LIO iblock backstore recording devPath, bound to
// iqn's LUN 0 by the configfs symlink the kernel creates.
func (fx *impFixture) addLIOExport(t *testing.T, iqn, devPath string) {
	t.Helper()
	bs := filepath.Join("config", "target", "core", "iblock_0", "bs-legacy")
	fx.writeFile(t, filepath.Join(bs, "udev_path"), devPath+"\n")
	lunDir := filepath.Join(fx.root, "config", "target", "iscsi", iqn, "tpgt_1", "lun", "lun_0")
	fx.mkdir(t, filepath.Join("config", "target", "iscsi", iqn, "tpgt_1", "lun", "lun_0"))
	if err := os.Symlink(filepath.Join(fx.root, bs), filepath.Join(lunDir, "bs-link")); err != nil {
		t.Fatal(err)
	}
}

func impRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	refused, isRefused := errors.AsType[*backend.ImportRefusedError](err)
	if !isRefused {
		t.Fatalf("error = %v, want *backend.ImportRefusedError (%s)", err, reason)
	}
	if refused.Reason != reason {
		t.Fatalf("refusal reason = %q (%s), want %q", refused.Reason, refused.Detail, reason)
	}
}

func impExitError(t *testing.T, code string) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+code).Run() //nolint:gosec // fixed test command
	exitErr, isExit := errors.AsType[*exec.ExitError](err)
	if !isExit {
		t.Fatalf("sh exit %s: %v, want *exec.ExitError", code, err)
	}
	return exitErr
}

// The identity query is exactly the one the guest QA ran, so the parser is
// pinned to real lvs output, not to a fake's own format.
func TestImportLV_QueriesExactLVSColumns(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	if _, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID)); err != nil {
		t.Fatalf("ImportLV: %v", err)
	}
	want := []string{"lvs", "--reportformat", "json", "--units", "b", "--nosuffix", "-o",
		"lv_name,vg_name,lv_uuid,vg_uuid,lv_attr,segtype,pool_lv,origin,lv_size,lv_kernel_major,lv_kernel_minor",
		impLinearID}
	if !slices.Equal(fx.exec.calls[0], want) {
		t.Fatalf("lvs argv = %q, want %q", fx.exec.calls[0], want)
	}
}

func TestImportLV_AdoptsIdleLinearLV(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	path, size, err := fx.b.ImportLV(context.Background(), impLinearID, 1<<20, impWant(impLinearID))
	if err != nil {
		t.Fatalf("ImportLV: %v", err)
	}
	if path != impLinearPath || size != impLVSize {
		t.Fatalf("ImportLV = (%q, %d), want (%q, %d)", path, size, impLinearPath, impLVSize)
	}
	// Two identity probes; the second ran while the exclusive claim was held.
	if !slices.Equal(fx.exec.heldAtLVS, []int{0, 1}) {
		t.Fatalf("claims held at each lvs = %v, want [0 1] (re-verify under the claim)", fx.exec.heldAtLVS)
	}
	if fx.dev.heldNow() != 0 || fx.dev.releases != 1 {
		t.Fatalf("claim held=%d releases=%d after import, want 0/1", fx.dev.heldNow(), fx.dev.releases)
	}
	if !slices.Equal(fx.dev.paths, []string{impLinearPath}) {
		t.Fatalf("claimed paths = %q, want [%q]", fx.dev.paths, impLinearPath)
	}
	if m := fx.exec.mutations(); len(m) != 0 {
		t.Fatalf("import ran non-read-only commands: %q", m)
	}
}

func TestImportLV_AdoptsThinLVOfConfiguredPool(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "pool", 5, impDoc(t, impThinRow()))
	path, size, err := fx.b.ImportLV(context.Background(), impThinID, impLVSize, impWant(impThinID))
	if err != nil {
		t.Fatalf("ImportLV: %v", err)
	}
	if path != "/dev/"+impThinID || size != impLVSize {
		t.Fatalf("ImportLV = (%q, %d)", path, size)
	}
}

func TestImportLV_IdempotentRepeat(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	for range 2 {
		path, size, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		if err != nil || path != impLinearPath || size != impLVSize {
			t.Fatalf("ImportLV = (%q, %d, %v)", path, size, err)
		}
	}
	if fx.dev.heldNow() != 0 || fx.dev.releases != 2 {
		t.Fatalf("held=%d releases=%d, want 0/2", fx.dev.heldNow(), fx.dev.releases)
	}
}

// Refusals decided from the request or from lvs alone never take a claim.
func TestImportLV_RefusalsBeforeClaim(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		thinpool string
		id       string
		want     func() backend.LVMIdentity
		row      map[string]string
		reason   string
		noLVS    bool
	}{
		"other vg": {
			id: impLinearID, reason: "layout", noLVS: true, row: impLinearRow(),
			want: func() backend.LVMIdentity {
				w := impWant(impLinearID)
				w.VolumeGroup = "other-vg"
				return w
			},
		},
		"locator names another lv": {id: impVG + "/legacy-other", reason: "layout", noLVS: true,
			row: impLinearRow(), want: func() backend.LVMIdentity { return impWant(impLinearID) }},
		"vg uuid differs": {id: impLinearID, reason: backend.ImportRefusedReasonIdentity,
			row: impWith(impLinearRow(), "vg_uuid", "BBBBBB-BBBB-BBBB-BBBB-BBBB-BBBB-BBBBBB")},
		"lv uuid differs (same-name replacement)": {id: impLinearID, reason: backend.ImportRefusedReasonIdentity,
			row: impWith(impLinearRow(), "lv_uuid", impOtherLVUUID)},
		"thin pool lv": {id: impLinearID, reason: "layout",
			row: impWith(impLinearRow(), "segtype", "thin-pool", "lv_attr", "twi-aotz--")},
		"raid lv": {id: impLinearID, reason: "layout",
			row: impWith(impLinearRow(), "segtype", "raid1", "lv_attr", "rwi-a-r---")},
		"classic snapshot": {id: impLinearID, reason: "wrong type",
			row: impWith(impLinearRow(), "lv_attr", "swi-a-s---", "origin", "base")},
		"snapshot origin": {id: impLinearID, reason: "wrong type",
			row: impWith(impLinearRow(), "lv_attr", "owi-a-s---")},
		"thin snapshot (origin set)": {thinpool: "pool", id: impThinID, reason: "wrong type",
			row: impWith(impThinRow(), "origin", "legacy-base")},
		"thin lv on a linear backend": {id: impThinID, reason: "layout", row: impThinRow()},
		"linear lv on a thin backend": {thinpool: "pool", id: impLinearID, reason: "layout", row: impLinearRow()},
		"thin lv of another pool":     {thinpool: "pool2", id: impThinID, reason: "layout", row: impThinRow()},
		"inactive": {id: impLinearID, reason: "inactive",
			row: impWith(impLinearRow(), "lv_attr", "-wi-------", "lv_kernel_major", "-1", "lv_kernel_minor", "-1")},
		"too small": {id: impLinearID, reason: "too small",
			row: impWith(impLinearRow(), "lv_size", "1048576")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := newImpFixture(t, tc.thinpool, 0, impDoc(t, tc.row))
			want := impWant(tc.id)
			if tc.want != nil {
				want = tc.want()
			}
			path, size, err := fx.b.ImportLV(context.Background(), tc.id, 64<<20, want)
			impRefusal(t, err, tc.reason)
			if path != "" || size != 0 {
				t.Fatalf("refused import returned (%q, %d)", path, size)
			}
			if len(fx.dev.paths) != 0 {
				t.Fatalf("refusal took an exclusive claim on %q", fx.dev.paths)
			}
			if tc.noLVS && fx.exec.count("lvs") != 0 {
				t.Fatalf("request-side refusal ran lvs %d times", fx.exec.count("lvs"))
			}
			if m := fx.exec.mutations(); len(m) != 0 {
				t.Fatalf("refusal ran non-read-only commands: %q", m)
			}
		})
	}
}

func TestImportLV_MissingLV(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*impFixture){
		"lvs reports not found": func(fx *impFixture) {
			fx.exec.lvsErr = impExitError(t, "5")
			fx.exec.lvsErrOut = `  Failed to find logical volume "pillar163qa/legacy-linear"`
		},
		"empty report": func(fx *impFixture) { fx.exec.lvs = []string{impDoc(t)} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
			setup(fx)
			_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
			impRefusal(t, err, "missing")
		})
	}
}

// lvs failures that are not "not found" are errors, never refusals the
// caller could misread as a verdict about the LV.
func TestImportLV_ProbeErrorsAreNotRefusals(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*impFixture){
		"lvs fails":          func(fx *impFixture) { fx.exec.lvsErr = errors.New("lvm lock timeout") },
		"malformed json":     func(fx *impFixture) { fx.exec.lvs = []string{"{not json"} },
		"two rows":           func(fx *impFixture) { fx.exec.lvs = []string{impDoc(t, impLinearRow(), impThinRow())} },
		"row for another lv": func(fx *impFixture) { fx.exec.lvs = []string{impDoc(t, impThinRow())} },
		"non-numeric size": func(fx *impFixture) {
			fx.exec.lvs = []string{impDoc(t, impWith(impLinearRow(), "lv_size", "128m"))}
		},
		"garbled kernel minor": func(fx *impFixture) {
			fx.exec.lvs = []string{impDoc(t, impWith(impLinearRow(), "lv_kernel_minor", "x"))}
		},
		"active without device": func(fx *impFixture) {
			fx.exec.lvs = []string{impDoc(t, impWith(impLinearRow(), "lv_kernel_major", "-1", "lv_kernel_minor", "-1"))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
			setup(fx)
			_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
			var refused *backend.ImportRefusedError
			if err == nil || errors.As(err, &refused) {
				t.Fatalf("error = %v, want a non-refusal probe error", err)
			}
			if !strings.Contains(err.Error(), impLinearID) {
				t.Fatalf("error %q does not name the volume", err)
			}
		})
	}
}

func TestImportLV_ClaimBoundary(t *testing.T) {
	t.Parallel()
	t.Run("EBUSY is in use", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		fx.dev.busy = true
		_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		impRefusal(t, err, "in use")
	})
	t.Run("other open error is an error", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		fx.dev.openErr = devidle.ErrUnsupported
		_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		if !errors.Is(err, devidle.ErrUnsupported) {
			t.Fatalf("error = %v, want wrapped ErrUnsupported", err)
		}
	})
	t.Run("claimed device is a different kernel device", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 7, impDoc(t, impLinearRow()))
		_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		impRefusal(t, err, backend.ImportRefusedReasonIdentity)
		if fx.dev.heldNow() != 0 {
			t.Fatal("claim leaked after identity refusal")
		}
	})
	t.Run("replaced while the claim was held", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0,
			impDoc(t, impLinearRow()), impDoc(t, impWith(impLinearRow(), "lv_uuid", impOtherLVUUID)))
		_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		impRefusal(t, err, backend.ImportRefusedReasonIdentity)
		if fx.dev.heldNow() != 0 || fx.dev.releases != 1 {
			t.Fatalf("held=%d releases=%d, want 0/1", fx.dev.heldNow(), fx.dev.releases)
		}
	})
	t.Run("kernel device changed while the claim was held", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0,
			impDoc(t, impLinearRow()), impDoc(t, impWith(impLinearRow(), "lv_kernel_minor", "9")))
		_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		impRefusal(t, err, backend.ImportRefusedReasonIdentity)
	})
	t.Run("removed while the claim was held", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()), impDoc(t))
		_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		impRefusal(t, err, "missing")
	})
	t.Run("release failure is surfaced", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		closeErr := errors.New("close: EIO")
		fx.dev.releaseErr = closeErr
		path, size, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
		if !errors.Is(err, closeErr) || path != "" || size != 0 {
			t.Fatalf("ImportLV = (%q, %d, %v), want release error and no adoption", path, size, err)
		}
		if !strings.Contains(err.Error(), impLinearPath) {
			t.Fatalf("release error %q does not name the device", err)
		}
	})
}

// Non-exclusive consumers the O_EXCL claim cannot see refuse the import;
// they are matched on the kernel device number, not on a path spelling.
func TestImportLV_NonExclusiveConsumersRefused(t *testing.T) {
	t.Parallel()
	rdev := devidle.EncodeLinuxDev(252, 0)
	cases := map[string]func(*testing.T, *impFixture){
		"mount through a /dev/mapper alias": func(t *testing.T, fx *impFixture) {
			fx.writeFile(t, "mountinfo",
				"36 25 252:0 / /srv/legacy rw,relatime shared:1 - ext4 /dev/mapper/pillar163qa-legacy--linear rw\n")
		},
		"device-mapper holder": func(t *testing.T, fx *impFixture) {
			fx.writeFile(t, "sys/dev/block/252:0/holders/dm-9", "")
		},
		"LIO backstore via an alias path": func(t *testing.T, fx *impFixture) {
			fx.stat["/dev/mapper/pillar163qa-legacy--linear"] = rdev
			fx.addLIOExport(t, "iqn.2003-01.org.example:legacy", "/dev/mapper/pillar163qa-legacy--linear")
		},
		"nvmet namespace": func(t *testing.T, fx *impFixture) {
			fx.stat["/dev/dm-0"] = rdev
			fx.writeFile(t, "config/nvmet/subsystems/nqn.2014-08.org.example:legacy/namespaces/1/device_path",
				"/dev/dm-0\n")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
			setup(t, fx)
			_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
			impRefusal(t, err, "in use")
			if fx.dev.heldNow() != 0 || fx.dev.releases != 1 {
				t.Fatalf("held=%d releases=%d, want 0/1", fx.dev.heldNow(), fx.dev.releases)
			}
		})
	}
}

// A mount table line naming the LV's path but another device number is a
// stale/aliased name, not a consumer of this device.
func TestImportLV_PathSpellingIsNotIdentity(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	fx.writeFile(t, "mountinfo", "36 25 252:7 / /srv/other rw - ext4 "+impLinearPath+" rw\n")
	if _, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID)); err != nil {
		t.Fatalf("ImportLV: %v", err)
	}
}

// An idle scan that cannot read its kernel tree fails closed.
func TestImportLV_UnreadableKernelTreeFailsClosed(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	fx.mkdir(t, "mountinfo") // a directory: open succeeds, read fails
	_, _, err := fx.b.ImportLV(context.Background(), impLinearID, impLVSize, impWant(impLinearID))
	var refused *backend.ImportRefusedError
	if err == nil || errors.As(err, &refused) {
		t.Fatalf("error = %v, want a non-refusal idle-scan error", err)
	}
	if fx.dev.heldNow() != 0 {
		t.Fatal("claim leaked after scan error")
	}
}

func TestVerifyLV(t *testing.T) {
	t.Parallel()
	t.Run("pinned identity", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		if err := fx.b.VerifyLV(context.Background(), impLinearID, impWant(impLinearID)); err != nil {
			t.Fatalf("VerifyLV: %v", err)
		}
		// Verification observes; it never claims or touches the device.
		if len(fx.dev.paths) != 0 || len(fx.exec.mutations()) != 0 || fx.exec.count("blkid") != 0 {
			t.Fatalf("VerifyLV claimed %q / ran %q", fx.dev.paths, fx.exec.calls)
		}
	})
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t))
		impRefusal(t, fx.b.VerifyLV(context.Background(), impLinearID, impWant(impLinearID)), "missing")
	})
	for field, kv := range map[string][]string{
		"lv uuid": {"lv_uuid", impOtherLVUUID},
		"vg uuid": {"vg_uuid", "BBBBBB-BBBB-BBBB-BBBB-BBBB-BBBB-BBBBBB"},
	} {
		t.Run("replaced "+field, func(t *testing.T) {
			t.Parallel()
			fx := newImpFixture(t, "", 0, impDoc(t, impWith(impLinearRow(), kv...)))
			impRefusal(t, fx.b.VerifyLV(context.Background(), impLinearID, impWant(impLinearID)),
				backend.ImportRefusedReasonIdentity)
		})
	}
	t.Run("pin names another lv", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		impRefusal(t, fx.b.VerifyLV(context.Background(), impLinearID, impWant(impThinID)), "layout")
	})
	t.Run("lvs error is not a verdict", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		fx.exec.lvsErr = errors.New("lvm lock timeout")
		err := fx.b.VerifyLV(context.Background(), impLinearID, impWant(impLinearID))
		var refused *backend.ImportRefusedError
		if err == nil || errors.As(err, &refused) {
			t.Fatalf("VerifyLV = %v, want a non-refusal error", err)
		}
	})
}

func TestInspectLV_ActiveIdleObservation(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	fx.exec.blkidOut = "DEVNAME=" + impLinearPath + "\nUUID=1b2c\nBLOCK_SIZE=4096\nTYPE=ext4\n"
	obs, err := fx.b.InspectLV(context.Background(), impLinearID)
	if err != nil {
		t.Fatalf("InspectLV: %v", err)
	}
	want := backend.LVObservation{
		Identity: impWant(impLinearID), Attr: "-wi-a-----", Segtype: "linear",
		DevicePath: impLinearPath, DevMajorMinor: "252:0", FSType: "ext4", FSUUID: "1b2c",
		FSProbe:   backend.FSProbeDetected,
		SizeBytes: impLVSize, Active: true, ExclusiveClaim: backend.ExclusiveClaimFree,
	}
	if !impObsEqual(obs, want) {
		t.Fatalf("InspectLV = %+v\nwant %+v", obs, want)
	}
	if fx.dev.heldNow() != 0 || fx.dev.releases != 1 {
		t.Fatalf("transient claim held=%d releases=%d, want 0/1", fx.dev.heldNow(), fx.dev.releases)
	}
	blkid := []string{"blkid", "-p", "-o", "export", "-c", "/dev/null", impLinearPath}
	if !slices.ContainsFunc(fx.exec.calls, func(c []string) bool { return slices.Equal(c, blkid) }) {
		t.Fatalf("blkid probe argv missing; calls %q", fx.exec.calls)
	}
	if m := fx.exec.mutations(); len(m) != 0 {
		t.Fatalf("inspect ran non-read-only commands: %q", m)
	}
}

// impObsEqual compares every field of two observations; the slices compare
// element-wise (nil equals empty) and, with them cleared, the remaining
// fields compare by deep equality.
func impObsEqual(a, b backend.LVObservation) bool {
	if !slices.Equal(a.Consumers, b.Consumers) || !slices.Equal(a.Exports, b.Exports) {
		return false
	}
	a.Consumers, a.Exports = nil, nil
	b.Consumers, b.Exports = nil, nil
	return reflect.DeepEqual(a, b)
}

func TestInspectLV_ReportsConsumersAndExports(t *testing.T) {
	t.Parallel()
	rdev := devidle.EncodeLinuxDev(252, 0)
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	fx.dev.busy = true
	fx.writeFile(t, "mountinfo", "36 25 252:0 / /srv/legacy\\040data rw - ext4 /dev/dm-0 rw\n")
	fx.writeFile(t, "sys/dev/block/252:0/holders/dm-9", "")
	fx.stat[impLinearPath] = rdev
	const iqn = "iqn.2026-01.com.bhyoo.pillar-csi:pillar163qa.legacy-linear"
	fx.addLIOExport(t, iqn, impLinearPath)
	obs, err := fx.b.InspectLV(context.Background(), impLinearID)
	if err != nil {
		t.Fatalf("InspectLV: %v", err)
	}
	if obs.ExclusiveClaim != backend.ExclusiveClaimBusy {
		t.Fatalf("ExclusiveClaim = %q, want busy", obs.ExclusiveClaim)
	}
	wantConsumers := []backend.DeviceConsumer{
		{Kind: "mount", Detail: "/srv/legacy data"},
		{Kind: "holder", Detail: "dm-9"},
	}
	if !slices.Equal(obs.Consumers, wantConsumers) {
		t.Fatalf("Consumers = %+v, want %+v", obs.Consumers, wantConsumers)
	}
	// The export reports its target ID verbatim so the caller can tell its
	// own configured export from a foreign one.
	wantExports := []backend.DeviceConsumer{{Kind: "export", Detail: iqn}}
	if !slices.Equal(obs.Exports, wantExports) {
		t.Fatalf("Exports = %+v, want %+v", obs.Exports, wantExports)
	}
}

func TestInspectLV_ExclusiveClaimNeverGuessedFree(t *testing.T) {
	t.Parallel()
	t.Run("claimed fd is another device", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 9, impDoc(t, impLinearRow()))
		obs, err := fx.b.InspectLV(context.Background(), impLinearID)
		if err != nil || obs.ExclusiveClaim != backend.ExclusiveClaimUnknown {
			t.Fatalf("InspectLV = (%q, %v), want unknown", obs.ExclusiveClaim, err)
		}
		if fx.dev.heldNow() != 0 {
			t.Fatal("transient claim leaked")
		}
	})
	t.Run("open error", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		fx.dev.openErr = errors.New("EACCES")
		obs, err := fx.b.InspectLV(context.Background(), impLinearID)
		if err != nil || obs.ExclusiveClaim != backend.ExclusiveClaimUnknown {
			t.Fatalf("InspectLV = (%q, %v), want unknown", obs.ExclusiveClaim, err)
		}
	})
	t.Run("release failure fails the inspect", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		closeErr := errors.New("close: EIO")
		fx.dev.releaseErr = closeErr
		if _, err := fx.b.InspectLV(context.Background(), impLinearID); !errors.Is(err, closeErr) {
			t.Fatalf("InspectLV = %v, want the release error", err)
		}
	})
}

// An inactive LV is observed without activation: no claim, no blkid, no
// device number, an unknown claim and an unknown filesystem probe.
func TestInspectLV_InactiveIsNotActivated(t *testing.T) {
	t.Parallel()
	fx := newImpFixture(t, "", 0,
		impDoc(t, impWith(impLinearRow(), "lv_attr", "-wi-------", "lv_kernel_major", "-1", "lv_kernel_minor", "-1")))
	obs, err := fx.b.InspectLV(context.Background(), impLinearID)
	if err != nil {
		t.Fatalf("InspectLV: %v", err)
	}
	if obs.Active || obs.DevMajorMinor != "" || obs.ExclusiveClaim != backend.ExclusiveClaimUnknown ||
		obs.FSProbe != backend.FSProbeUnknown || obs.FSProbeError == "" || obs.FSType != "" || obs.FSUUID != "" {
		t.Fatalf("inactive observation = %+v", obs)
	}
	if len(fx.dev.paths) != 0 || fx.exec.count("blkid") != 0 || len(fx.exec.mutations()) != 0 {
		t.Fatalf("inactive inspect claimed %q / ran %q", fx.dev.paths, fx.exec.calls)
	}
}

// util-linux `blkid -p -o export -c /dev/null` output for an ext4 LV (the
// record shape util-linux emits: DEVNAME first, escaped KEY=VALUE lines).
const impUtilLinuxExt4UUID = "4f7e5b0a-2c1d-4e8f-9a3b-6c5d4e3f2a1b"

func impUtilLinuxExt4Export() string {
	return "DEVNAME=" + impLinearPath + "\nUUID=" + impUtilLinuxExt4UUID +
		"\nVERSION=1.0\nBLOCK_SIZE=4096\nTYPE=ext4\nUSAGE=filesystem\n"
}

// impInspectWithBlkid inspects the active linear LV with blkid printing out
// and, when exit is non-empty, failing with that exit status.
func impInspectWithBlkid(t *testing.T, out, exit string) (*impFixture, backend.LVObservation, error) {
	t.Helper()
	fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
	fx.exec.blkidOut = out
	if exit != "" {
		fx.exec.blkidErr = impExitError(t, exit)
	}
	obs, err := fx.b.InspectLV(context.Background(), impLinearID)
	return fx, obs, err
}

func TestInspectLV_SignatureProbe(t *testing.T) {
	t.Parallel()
	t.Run("util-linux export reports the filesystem", func(t *testing.T) {
		t.Parallel()
		_, obs, err := impInspectWithBlkid(t, impUtilLinuxExt4Export(), "")
		if err != nil || obs.FSType != "ext4" || obs.FSUUID != impUtilLinuxExt4UUID ||
			obs.FSProbe != backend.FSProbeDetected || obs.FSProbeError != "" {
			t.Fatalf("InspectLV = (%q/%q %q %q, %v), want detected ext4/%s",
				obs.FSType, obs.FSUUID, obs.FSProbe, obs.FSProbeError, err, impUtilLinuxExt4UUID)
		}
	})
	t.Run("unrelated export fields are not validated", func(t *testing.T) {
		t.Parallel()
		out := impUtilLinuxExt4Export() +
			"LABEL=my\\ data\nLABEL_ENC=my\\x20data\nPART_ENTRY_NAME=données $x\n"
		_, obs, err := impInspectWithBlkid(t, out, "")
		if err != nil || obs.FSType != "ext4" || obs.FSUUID != impUtilLinuxExt4UUID ||
			obs.FSProbe != backend.FSProbeDetected {
			t.Fatalf("InspectLV = (%q/%q %q, %v), want detected ext4/%s",
				obs.FSType, obs.FSUUID, obs.FSProbe, err, impUtilLinuxExt4UUID)
		}
	})
}

func TestInspectLV_SignatureProbeUnknown(t *testing.T) {
	t.Parallel()

	// Inconclusive probes complete the inspect as FSProbeUnknown with an
	// explanation: never a blank/unformatted observation, never an error
	// that would make a raw or partitioned block LV uninspectable.  An
	// empty exit 2 is how util-linux reports both "scan found nothing" and
	// a silent low-level probe failure, so it proves nothing either way.
	for _, tc := range []struct {
		name, out, exit, wantDetail string
	}{
		{name: "blkid exit 2, empty output", exit: "2", wantDetail: "not proven blank"},
		{name: "partition table without filesystem",
			out: "DEVNAME=" + impLinearPath + "\nPTUUID=0a1b2c3d\nPTTYPE=dos\n", wantDetail: `partition table "dos"`},
		{name: "records without TYPE", out: "DEVNAME=" + impLinearPath + "\nUSAGE=other\n", wantDetail: "USAGE=other"},
	} {
		t.Run("unknown: "+tc.name, func(t *testing.T) {
			t.Parallel()
			fx, obs, err := impInspectWithBlkid(t, tc.out, tc.exit)
			if err != nil {
				t.Fatalf("InspectLV: %v, want an unknown-probe observation", err)
			}
			if obs.FSProbe != backend.FSProbeUnknown || obs.FSType != "" || obs.FSUUID != "" ||
				!strings.Contains(obs.FSProbeError, impLinearPath) || !strings.Contains(obs.FSProbeError, tc.wantDetail) {
				t.Fatalf("InspectLV probe = %q %q/%q (%q), want unknown mentioning %q",
					obs.FSProbe, obs.FSType, obs.FSUUID, obs.FSProbeError, tc.wantDetail)
			}
			// The rest of the observation is unaffected by the unknown probe.
			if !obs.Active || obs.Identity != impWant(impLinearID) || obs.ExclusiveClaim != backend.ExclusiveClaimFree {
				t.Fatalf("InspectLV = %+v, want the active free LV", obs)
			}
			if m := fx.exec.mutations(); len(m) != 0 || fx.dev.heldNow() != 0 {
				t.Fatalf("inspect ran %q / holds %d claims", m, fx.dev.heldNow())
			}
		})
	}
}

func TestInspectLV_SignatureProbeErrors(t *testing.T) {
	t.Parallel()
	// Every case below must fail the inspect: none of them is evidence that
	// the device is blank, and none may be read as a filesystem either.
	for _, tc := range []struct {
		name string
		out  string
		exit string // "" = exit 0
	}{
		{
			// Observed BusyBox blkid: ignores -p/-o/-c, exits 0 and prints the
			// single-line cache format.  Must not read as "no signature".
			name: "BusyBox single-line output",
			out:  impLinearPath + `: UUID="` + impUtilLinuxExt4UUID + `" TYPE="ext4"` + "\n",
		},
		{name: "exit 0 with empty output", out: ""},
		{name: "exit 2 with output", out: "TYPE=ext4\n", exit: "2"},
		{name: "other exit status", out: "blkid: error: " + impLinearPath + ": Input/output error\n", exit: "4"},
		{
			name: "quoted export value",
			out:  "DEVNAME=" + impLinearPath + "\nUUID=\"" + impUtilLinuxExt4UUID + "\"\nTYPE=\"ext4\"\n",
		},
		{name: "lower-case key", out: "DEVNAME=" + impLinearPath + "\ntype=ext4\n"},
		{name: "record without =", out: "DEVNAME=" + impLinearPath + "\nTYPE ext4\n"},
		{name: "duplicate TYPE", out: "DEVNAME=" + impLinearPath + "\nTYPE=ext4\nTYPE=xfs\n"},
		{name: "empty TYPE", out: "DEVNAME=" + impLinearPath + "\nTYPE=\n"},
		{name: "missing DEVNAME", out: "UUID=" + impUtilLinuxExt4UUID + "\nTYPE=ext4\n"},
		{name: "other DEVNAME", out: "DEVNAME=/dev/other/lv\nTYPE=ext4\n"},
		{name: "empty TYPE with partition table", out: "DEVNAME=" + impLinearPath + "\nTYPE=\nPTTYPE=dos\n"},
		// TYPE-less records are unknown only when they describe this device.
		{name: "partition table of another DEVNAME", out: "DEVNAME=/dev/other/lv\nPTTYPE=dos\n"},
		{name: "partition table without DEVNAME", out: "PTUUID=0a1b2c3d\nPTTYPE=dos\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx, obs, err := impInspectWithBlkid(t, tc.out, tc.exit)
			if err == nil {
				t.Fatalf("InspectLV = %+v, want an error for blkid output %q", obs, tc.out)
			}
			for _, want := range []string{impLinearID, impLinearPath} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("InspectLV error %q lacks context %q", err, want)
				}
			}
			if obs.FSType != "" || obs.FSUUID != "" {
				t.Fatalf("failed InspectLV leaked FSType/FSUUID %q/%q", obs.FSType, obs.FSUUID)
			}
			if m := fx.exec.mutations(); len(m) != 0 || fx.dev.heldNow() != 0 {
				t.Fatalf("failed inspect ran %q / holds %d claims", m, fx.dev.heldNow())
			}
		})
	}
}

func TestInspectLV_FailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("missing lv", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t))
		_, err := fx.b.InspectLV(context.Background(), impLinearID)
		impRefusal(t, err, "missing")
	})
	t.Run("idle scan error", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		fx.mkdir(t, "mountinfo")
		if _, err := fx.b.InspectLV(context.Background(), impLinearID); err == nil {
			t.Fatal("InspectLV reported an observation despite an unreadable mount table")
		}
	})
	t.Run("recorded export path unstatable", func(t *testing.T) {
		t.Parallel()
		fx := newImpFixture(t, "", 0, impDoc(t, impLinearRow()))
		fx.addLIOExport(t, "iqn.2003-01.org.example:x", "/dev/denied")
		lvm.SetBackendIdleChecker(t, fx.b, devidle.Checker{
			Roots: devidle.Roots{ConfigfsRoot: filepath.Join(fx.root, "config"),
				MountinfoPath: filepath.Join(fx.root, "none"), SysDevBlock: filepath.Join(fx.root, "none")},
			StatRdev: func(string) (uint64, error) { return 0, os.ErrPermission },
		})
		if _, err := fx.b.InspectLV(context.Background(), impLinearID); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("InspectLV = %v, want the stat error", err)
		}
	})
}
