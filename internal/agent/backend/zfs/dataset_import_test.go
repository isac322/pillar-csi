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

package zfs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

func TestDatasetFilesystemInspectAndImportAreReadOnlyAndStable(t *testing.T) {
	const dataset = "tank/k8s/existing"
	const guid = "184467440737095516"
	const quota = int64(4096)

	f := newImportDatasetFixture(t)
	f.properties["guid"] = guid
	found := f.inspect()
	if found.CapacityBytes != quota ||
		found.Filesystem.GetCanonicalSource() != dataset ||
		found.Filesystem.GetResourceId() != guid ||
		found.Filesystem.GetHostPath() != "" {
		t.Fatalf("InspectImport() = %#v, want dataset=%q guid=%q exact quota=%d with no host path",
			found, dataset, guid, quota)
	}
	pin, err := f.backend.ImportFilesystem(
		context.Background(), "pool/hashed-resource", quota, found.Filesystem,
		backend.Layout{ParentDataset: "k8s"},
	)
	if err != nil {
		t.Fatalf("ImportFilesystem() error = %v", err)
	}
	if pin.MountSource() != dataset ||
		pin.CapacityBytes() != quota ||
		pin.Adoption().GetResourceId() != guid {
		t.Fatalf("pin = source %q capacity %d adoption %#v",
			pin.MountSource(), pin.CapacityBytes(), pin.Adoption())
	}
	closeErr := pin.Close()
	if closeErr != nil {
		t.Fatalf("pin.Close() error = %v", closeErr)
	}
}

func TestDatasetFilesystemImportRejectsIdentityAndQuotaDrift(t *testing.T) {
	f := newImportDatasetFixture(t)
	recorded := f.inspect().Filesystem
	pin, err := f.backend.ImportFilesystem(
		context.Background(), "tank/volume", 4096, recorded,
		backend.Layout{ParentDataset: "k8s"},
	)
	if err != nil {
		t.Fatalf("healthy baseline: %v", err)
	}
	closeErr := pin.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	recorded.ResourceId = "100"
	_, err = f.backend.ImportFilesystem(
		context.Background(), "tank/volume", 4096, recorded,
		backend.Layout{ParentDataset: "k8s"},
	)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != reasonLayout {
		t.Fatalf("GUID drift error = %v, want layout refusal", err)
	}
	recorded.ResourceId = "99"
	_, err = f.backend.ImportFilesystem(
		context.Background(), "tank/volume", 2048, recorded,
		backend.Layout{ParentDataset: "k8s"},
	)
	var conflict *backend.ConflictError
	if !errors.As(err, &conflict) ||
		conflict.ExistingBytes != 4096 ||
		conflict.RequestedBytes != 2048 {
		t.Fatalf("quota drift error = %v, want exact-bound conflict", err)
	}
}

func TestZFSVolumeIdentityGuardDistinguishesFilesystemFromZvol(t *testing.T) {
	mode := "filesystem"
	b := NewWithExecFn("tank", "k8s", func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] != "get" {
			return nil, fmt.Errorf("unexpected zfs operation %q", args[0])
		}
		return []byte(fmt.Sprintf("type\t%s\nguid\t77\n", mode)), nil
	})
	identity, err := b.ExistingFilesystemIdentity(context.Background(), "tank/volume")
	if err != nil ||
		identity == nil ||
		identity.GetCanonicalSource() != "tank/k8s/volume" ||
		identity.GetResourceId() != "77" {
		t.Fatalf("filesystem identity = %#v/%v", identity, err)
	}
	mode = "volume"
	identity, err = b.ExistingFilesystemIdentity(context.Background(), "tank/volume")
	if err != nil || identity != nil {
		t.Fatalf("zvol identity = %#v/%v, want nil", identity, err)
	}
}

// importDatasetFixture changes native property/mount state without pretending
// to perform a kernel mount. Parent-owned E71 covers physical ZFS mounts.
type importDatasetFixture struct {
	t                    *testing.T
	backend              *DatasetBackend
	properties           map[string]string
	mountinfo, proxyRoot string
	ancestorQuota        string
	childRows            string
	mutations            []string
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temporary root: %v", err)
	}
	return root
}

func newImportDatasetFixture(t *testing.T) *importDatasetFixture {
	t.Helper()
	root := canonicalTempDir(t)
	f := &importDatasetFixture{
		t:             t,
		mountinfo:     filepath.Join(root, "mountinfo"),
		proxyRoot:     filepath.Join(root, "proxies"),
		ancestorQuota: "none",
		properties: map[string]string{
			"type":                 "filesystem",
			"guid":                 "99",
			"mountpoint":           "legacy",
			"mounted":              "no",
			"canmount":             "noauto",
			"readonly":             "off",
			"refquota":             "4096",
			"quota":                "none",
			"used":                 "0",
			"usedbydataset":        "0",
			"usedbysnapshots":      "0",
			"usedbychildren":       "0",
			"usedbyrefreservation": "0",
		},
	}
	f.writeMounts("")
	datasetsRoot := filepath.Join(root, "datasets")
	f.backend = NewDatasetWithExecFn(
		"tank", "k8s", datasetsRoot, f.run,
		WithDatasetMountInfoPath(f.mountinfo),
		WithFilesystemProxyRoot(f.proxyRoot),
	)
	t.Cleanup(func() {
		if len(f.mutations) != 0 {
			t.Errorf("source mutations: %v", f.mutations)
		}
	})
	return f
}

func (f *importDatasetFixture) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("missing operation")
	}
	switch args[0] {
	case "get":
		if len(args) > 4 && args[4] == zfsAdoptionProperties {
			var out strings.Builder
			for property := range strings.SplitSeq(zfsAdoptionProperties, ",") {
				fmt.Fprintf(&out, "%s\t%s\n", property, f.properties[property])
			}
			return []byte(out.String()), nil
		}
		quotaRows := fmt.Sprintf(
			"tank/k8s/existing\tquota\t%s\n"+
				"tank/k8s/existing\tused\t%s\n"+
				"tank/k8s\tquota\t%s\n"+
				"tank/k8s\tused\t%s\n"+
				"tank\tquota\tnone\n"+
				"tank\tused\t%s\n",
			f.properties["quota"], f.properties["used"], f.ancestorQuota,
			f.properties["used"], f.properties["used"],
		)
		return []byte(quotaRows), nil
	case "list":
		listRows := fmt.Sprintf(
			"tank/k8s/existing\t%s\t%s\n%s",
			f.properties["mountpoint"], f.properties["mounted"], f.childRows,
		)
		return []byte(listRows), nil
	default:
		f.mutations = append(f.mutations, args[0])
		return nil, fmt.Errorf("source operation %q forbidden", args[0])
	}
}

func (f *importDatasetFixture) writeMounts(value string) {
	f.t.Helper()
	err := os.WriteFile(f.mountinfo, []byte(value), 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
}

func datasetMountRow(t *testing.T, path, root string) string {
	t.Helper()
	err := os.MkdirAll(path, 0o750)
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	err = syscall.Stat(path, &st)
	if err != nil {
		t.Fatal(err)
	}
	device, err := datasetDeviceNumber(st.Dev)
	if err != nil {
		t.Fatal(err)
	}
	escape := strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`)
	return fmt.Sprintf(
		"41 1 %d:%d %s %s rw - zfs tank/k8s/existing rw\n",
		unix.Major(device), unix.Minor(device), escape.Replace(root), escape.Replace(path),
	)
}

func (f *importDatasetFixture) inspect() *backend.ImportInspection {
	f.t.Helper()
	found, err := f.backend.InspectImport(
		context.Background(), "tank/k8s/existing", 4096, nil,
		backend.Layout{ParentDataset: "k8s"},
	)
	if err != nil {
		f.t.Fatalf("healthy inspection: %v", err)
	}
	if found.CapacityBytes != 4096 ||
		found.Filesystem.GetCanonicalSource() != "tank/k8s/existing" ||
		found.Filesystem.GetResourceId() != f.properties["guid"] {
		f.t.Fatalf("native exact-bound inspection = %#v", found)
	}
	return found
}

func (f *importDatasetFixture) newBackend() *DatasetBackend {
	return NewDatasetWithExecFn(
		"tank", "k8s", filepath.Join(canonicalTempDir(f.t), "datasets"), f.run,
		WithDatasetMountInfoPath(f.mountinfo),
		WithFilesystemProxyRoot(f.proxyRoot),
	)
}

func verifyAndClosePin(t *testing.T, pin backend.PinnedFilesystem, target string) {
	t.Helper()
	verifyErr := pin.VerifyMount(context.Background(), target)
	if verifyErr != nil {
		t.Fatal(verifyErr)
	}
	closeErr := pin.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func reopenLegacyPins(
	t *testing.T,
	f *importDatasetFixture,
	recorded *agentv1.FilesystemAdoption,
	target string,
) {
	t.Helper()
	for i := range 2 {
		// A fresh backend models recovery with no cached descriptor or FD.
		reopened := f.newBackend()
		next, err := reopened.ImportFilesystem(
			context.Background(), "tank/id", 4096, recorded,
			backend.Layout{ParentDataset: "k8s"},
		)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		if next.Adoption().GetHostPath() != "" || recorded.GetHostPath() != "" {
			t.Fatal("owned mount changed original provenance")
		}
		if !strings.HasPrefix(next.MountSource(), "/proc/self/fd/") {
			t.Fatal("reopen did not pin owned root")
		}
		verifyAndClosePin(t, next, target)
	}
}

func recoverLegacyPin(
	t *testing.T,
	f *importDatasetFixture,
	recorded *agentv1.FilesystemAdoption,
	target, row string,
) {
	t.Helper()
	f.properties["mounted"] = "no"
	f.writeMounts("")
	rebuild, err := f.backend.ImportFilesystem(
		context.Background(), "tank/id", 4096, recorded,
		backend.Layout{ParentDataset: "k8s"},
	)
	if err != nil {
		t.Fatalf("missing proxy recovery: %v", err)
	}
	if rebuild.MountSource() != recorded.GetCanonicalSource() ||
		rebuild.Adoption().GetHostPath() != "" {
		t.Fatal("proxy recovery lost unmounted native source")
	}
	f.properties["mounted"] = "yes"
	f.writeMounts(row)
	verifyAndClosePin(t, rebuild, target)
}

func rejectExternalLegacyMount(
	t *testing.T,
	f *importDatasetFixture,
	recorded *agentv1.FilesystemAdoption,
	row string,
) {
	t.Helper()
	external := filepath.Join(canonicalTempDir(t), "external")
	f.writeMounts(row + datasetMountRow(t, external, "/"))
	_, err := f.backend.ImportFilesystem(
		context.Background(), "tank/id", 4096, recorded,
		backend.Layout{ParentDataset: "k8s"},
	)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != reasonInUse {
		t.Fatalf("external mount error = %v", err)
	}
}

func TestDatasetLegacyOwnedProxyReopensWithoutChangingProvenance(t *testing.T) {
	for _, mountpoint := range []string{"legacy", "/original/noauto"} {
		t.Run(mountpoint, func(t *testing.T) {
			f := newImportDatasetFixture(t)
			f.properties["mountpoint"] = mountpoint
			recorded := f.inspect().Filesystem
			target := filepath.Join(f.proxyRoot, backend.FilesystemFenceID(recorded))
			row := datasetMountRow(t, target, "/")
			f.properties["mounted"] = "yes"
			f.writeMounts(row)
			pin, err := f.backend.ImportFilesystem(
				context.Background(), "tank/id", 4096, recorded,
				backend.Layout{ParentDataset: "k8s"},
			)
			if err != nil {
				t.Fatal(err)
			}
			verifyAndClosePin(t, pin, target)
			reopenLegacyPins(t, f, recorded, target)
			got := f.inspect().Filesystem
			if got.GetHostPath() != "" ||
				got.GetResourceId() != recorded.GetResourceId() {
				t.Fatalf("owned mounted stats descriptor = %v", got)
			}
			recoverLegacyPin(t, f, recorded, target, row)
			rejectExternalLegacyMount(t, f, recorded, row)
		})
	}
}

func TestDatasetMountedNamespacePinAndPostMountIdentity(t *testing.T) {
	f := newImportDatasetFixture(t)
	prefix := canonicalTempDir(t)
	host := "/native/existing"
	source := filepath.Join(prefix, "native", "existing")
	f.backend.hostRootPrefix = prefix
	f.properties["mountpoint"] = host
	f.properties["mounted"] = "yes"
	f.properties["canmount"] = "on"
	f.writeMounts(datasetMountRow(t, source, "/"))
	found := f.inspect()
	if found.Filesystem.GetHostPath() != host || f.backend.Layout().HostRoot != "" {
		t.Fatal("namespace prefix escaped into durable layout")
	}
	pin, err := f.backend.ImportFilesystem(
		context.Background(), "tank/id", 4096, found.Filesystem,
		backend.Layout{ParentDataset: "k8s"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeErr := pin.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	}()
	pinned, ok := pin.(*pinnedDataset)
	if !ok || pinned.file == nil {
		t.Fatal("mounted import did not retain an opened dataset root")
	}
	opened, err := pinned.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(opened, actual) {
		t.Fatal("pin does not refer to prefixed native root")
	}
	target := filepath.Join(f.proxyRoot, backend.FilesystemFenceID(found.Filesystem))
	f.writeMounts(
		datasetMountRow(t, source, "/") +
			datasetMountRow(t, target, "/subdir"),
	)
	verifyErr := pin.VerifyMount(context.Background(), target)
	if verifyErr == nil {
		t.Fatal("accepted a same-dataset subdirectory proxy")
	}
	f.writeMounts(datasetMountRow(t, source, "/") + datasetMountRow(t, target, "/"))
	verifyErr = pin.VerifyMount(context.Background(), target)
	if verifyErr == nil {
		t.Fatal("accepted proxy directory with a different pinned inode")
	}
	verifyErr = pin.VerifyMount(context.Background(), source)
	if verifyErr == nil {
		t.Fatal("accepted native path as owned proxy target")
	}
}

func TestDatasetOpenedPinRejectsMountRootReplacement(t *testing.T) {
	f := newImportDatasetFixture(t)
	path := filepath.Join(canonicalTempDir(t), "root")
	f.writeMounts(datasetMountRow(t, path, "/"))
	fd, err := syscall.Open(
		path,
		syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		closeErr := syscall.Close(fd)
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		t.Fatal("syscall.Open returned an invalid file descriptor")
	}
	defer func() {
		closeErr := file.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	}()
	verifyErr := f.backend.verifyPinnedDatasetMount("tank/k8s/existing", path, file)
	if verifyErr != nil {
		t.Fatalf("healthy root pin: %v", verifyErr)
	}
	err = os.Rename(path, path+".old")
	if err != nil {
		t.Fatal(err)
	}
	f.writeMounts(datasetMountRow(t, path, "/"))
	verifyErr = f.backend.verifyPinnedDatasetMount("tank/k8s/existing", path, file)
	if verifyErr == nil {
		t.Fatal("accepted opened backing directory after mount root replacement")
	}
}

func TestDatasetMountedChildBeforeAnotherRowRefusesUnmountedParentAdoption(t *testing.T) {
	f := newImportDatasetFixture(t)
	recorded := f.inspect().Filesystem
	layout := backend.Layout{ParentDataset: "k8s"}
	pin, err := f.backend.ImportFilesystem(context.Background(), "tank/id", 4096, recorded, layout)
	if err != nil {
		t.Fatalf("healthy unmounted parent import: %v", err)
	}
	closeErr := pin.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	originalProperties := maps.Clone(f.properties)
	// The parent is unmounted and the mounted child is outside its path.
	// Empty mountinfo cannot reject this second native quota scope for us.
	f.childRows = "tank/k8s/existing/mounted\t/external/child\tyes\n" +
		"tank/k8s/existing/later\tlegacy\tno\n"
	inspection, inspectErr := f.backend.InspectImport(context.Background(), "tank/k8s/existing", 4096, nil, layout)
	var inspectRefusal *backend.ImportRefusedError
	if inspection != nil || !errors.As(inspectErr, &inspectRefusal) || inspectRefusal.Reason != reasonInUse {
		t.Fatalf("mounted child inspection = %v, error = %v", inspection, inspectErr)
	}
	rejectedPin, importErr := f.backend.ImportFilesystem(context.Background(), "tank/id", 4096, recorded, layout)
	if rejectedPin != nil {
		closeErr = rejectedPin.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("mounted child adoption returned a filesystem pin")
	}
	var importRefusal *backend.ImportRefusedError
	if !errors.As(importErr, &importRefusal) || importRefusal.Reason != reasonInUse {
		t.Fatalf("mounted child import error = %v", importErr)
	}
	if !maps.Equal(f.properties, originalProperties) {
		t.Fatal("mounted child refusal changed original source properties, identity, or quota")
	}
}

func TestDatasetExactQuotaRequiresOwnLiveScope(t *testing.T) {
	for _, tc := range []struct {
		name, refquota, quota, other, ancestor string
		accepted                               bool
	}{
		{"own-refquota", "4096", "none", "0", "none", true},
		{"own-aggregate-unmixed", "none", "4096", "0", "none", true},
		{"own-refquota-with-snapshots", "4096", "8192", "1024", "none", true},
		{"binding-aggregate-with-snapshots", "none", "4096", "1024", "none", false},
		{"aggregate-constrains-refquota", "4096", "4096", "1024", "none", false},
		{"smaller-shared-ancestor", "4096", "none", "0", "2048", false},
		{"only-shared-ancestor", "none", "none", "0", "4096", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportDatasetFixture(t)
			f.inspect() // All negative cases start from complete healthy evidence.
			f.properties["refquota"], f.properties["quota"] = tc.refquota, tc.quota
			f.properties["usedbysnapshots"], f.properties["used"] = tc.other, tc.other
			f.ancestorQuota = tc.ancestor
			_, err := f.backend.InspectImport(
				context.Background(), "tank/k8s/existing", 4096, nil,
				backend.Layout{ParentDataset: "k8s"},
			)
			if tc.accepted {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var refused *backend.ImportRefusedError
			if !errors.As(err, &refused) || refused.Reason != reasonTooSmall {
				t.Fatalf("quota scope error = %v", err)
			}
		})
	}
}

func TestDatasetPostMountVerificationRejectsGUIDQuotaAndSubmountDrift(t *testing.T) {
	for _, drift := range []string{"guid", "quota", "submount", "subdirectory"} {
		t.Run(drift, func(t *testing.T) {
			f := newImportDatasetFixture(t)
			recorded := f.inspect().Filesystem
			target := filepath.Join(f.proxyRoot, backend.FilesystemFenceID(recorded))
			f.properties["mounted"] = "yes"
			f.writeMounts(datasetMountRow(t, target, "/"))
			pin, err := f.backend.ImportFilesystem(
				context.Background(), "tank/id", 4096, recorded,
				backend.Layout{ParentDataset: "k8s"},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				closeErr := pin.Close()
				if closeErr != nil {
					t.Error(closeErr)
				}
			}()
			verifyErr := pin.VerifyMount(context.Background(), target)
			if verifyErr != nil {
				t.Fatalf("healthy verification: %v", verifyErr)
			}
			switch drift {
			case "guid":
				f.properties["guid"] = "100"
			case "quota":
				f.properties["refquota"] = "8192"
			case "submount":
				f.writeMounts(
					datasetMountRow(t, target, "/") +
						datasetMountRow(t, filepath.Join(target, "child"), "/"),
				)
			case "subdirectory":
				f.writeMounts(datasetMountRow(t, target, "/child"))
			}
			verifyErr = pin.VerifyMount(context.Background(), target)
			if verifyErr == nil {
				t.Fatalf("accepted post-mount %s drift", drift)
			}
		})
	}
}

func TestDatasetQuotaAccountingScopesAndSourceAbsence(t *testing.T) {
	for _, scope := range []string{"usedbysnapshots", "usedbychildren", "usedbyrefreservation"} {
		t.Run(scope, func(t *testing.T) {
			f := newImportDatasetFixture(t)
			f.inspect()
			f.properties["refquota"] = "none"
			f.properties["quota"] = "4096"
			f.properties[scope] = "1024"
			f.properties["used"] = "1024"
			_, err := f.backend.InspectImport(
				context.Background(), "tank/k8s/existing", 4096, nil,
				backend.Layout{ParentDataset: "k8s"},
			)
			var refused *backend.ImportRefusedError
			if !errors.As(err, &refused) || refused.Reason != reasonTooSmall {
				t.Fatalf("mixed %s error = %v", scope, err)
			}
			// A larger aggregate quota leaves the own live refquota binding.
			f.properties["refquota"] = "4096"
			f.properties["quota"] = "8192"
			f.inspect()
		})
	}
	t.Run("readonly-native-source", func(t *testing.T) {
		f := newImportDatasetFixture(t)
		f.properties["readonly"] = "on"
		f.inspect()
	})
	t.Run("missing-after-healthy-inspection", func(t *testing.T) {
		f := newImportDatasetFixture(t)
		recorded := f.inspect().Filesystem
		f.backend.exec = execFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) == 0 || args[0] != "get" {
				t.Fatalf("missing source issued non-read operation: %v", args)
			}
			return []byte("cannot open 'tank/k8s/existing': dataset does not exist"), errors.New("exit status 1")
		})
		_, err := f.backend.ImportFilesystem(
			context.Background(), "tank/id", 4096, recorded,
			backend.Layout{ParentDataset: "k8s"},
		)
		var refused *backend.ImportRefusedError
		if !errors.As(err, &refused) || refused.Reason != reasonMissing {
			t.Fatalf("missing source error = %v", err)
		}
	})
}
