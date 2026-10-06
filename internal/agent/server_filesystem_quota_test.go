package agent_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/zfs"
)

// nativeQuotaDataset models one existing, unmounted ZFS dataset through the
// zfs(8) boundary only, so the real dataset importer and agent RPC error
// translation run unchanged.
type nativeQuotaDataset struct {
	refquota  string
	mutations []string
}

func (d *nativeQuotaDataset) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("missing zfs operation")
	}
	switch args[0] {
	case "get":
		if strings.Contains(strings.Join(args, " "), "usedbyrefreservation") {
			props := map[string]string{
				"type": "filesystem", "guid": "99", "mountpoint": "legacy", "mounted": "no",
				"canmount": "noauto", "readonly": "off", "refquota": d.refquota, "quota": "none",
				"used": "0", "usedbydataset": "0", "usedbysnapshots": "0", "usedbychildren": "0",
				"usedbyrefreservation": "0",
			}
			var out strings.Builder
			for _, property := range []string{
				"type", "guid", "mountpoint", "mounted", "canmount", "readonly", "refquota", "quota",
				"used", "usedbydataset", "usedbysnapshots", "usedbychildren", "usedbyrefreservation",
			} {
				fmt.Fprintf(&out, "%s\t%s\n", property, props[property])
			}
			return []byte(out.String()), nil
		}
		return []byte("tank/k8s/existing\tquota\tnone\ntank/k8s/existing\tused\t0\n" +
			"tank/k8s\tquota\tnone\ntank/k8s\tused\t0\n" +
			"tank\tquota\tnone\ntank\tused\t0\n"), nil
	case "list":
		return []byte("tank/k8s/existing\tlegacy\tno\n"), nil
	default:
		d.mutations = append(d.mutations, args[0])
		return nil, fmt.Errorf("source operation %q forbidden", args[0])
	}
}

func agentStateFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[rel+"/"] = ""
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads its own temporary state tree.
		if err != nil {
			return err
		}
		files[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestZFSFilesystemExactQuotaDriftIsFailedPrecondition(t *testing.T) {
	t.Parallel()
	const quota = int64(4096)
	root := canonicalTempDir(t)
	state := filepath.Join(root, "state")
	proxies := filepath.Join(state, "proxies")
	mountinfo := filepath.Join(root, "mountinfo")
	if err := os.WriteFile(mountinfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	native := &nativeQuotaDataset{refquota: fmt.Sprint(quota)}
	dataset := zfs.NewDatasetWithExecFn("tank", "k8s", filepath.Join(root, "datasets"), native.run,
		zfs.WithDatasetMountInfoPath(mountinfo), zfs.WithFilesystemProxyRoot(proxies))
	srv := agent.NewServer(map[string]backend.VolumeBackend{"tank": dataset}, "",
		agent.WithDrainStateDir(state), agent.WithFilesystemProxy(proxies))
	inspect := &agentv1.InspectImportRequest{
		PoolName: "tank", Source: "tank/k8s/existing", BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		RequiredBytes: quota, ExpectedParentDataset: "k8s",
	}
	found, err := srv.InspectImport(context.Background(), inspect)
	if err != nil {
		t.Fatalf("healthy exact quota inspection: %v", err)
	}
	if found.GetCapacityBytes() != quota {
		t.Fatalf("healthy inspection capacity = %d, want %d", found.GetCapacityBytes(), quota)
	}
	attach := &agentv1.ImportVolumeRequest{
		VolumeId: "tank/native", BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		CapacityBytes: quota, FilesystemAdoption: found.GetFilesystemAdoption(),
		Fence: &agentv1.FencingToken{VolumeUid: "owner", Generation: 1},
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Zfs{Zfs: &agentv1.ZfsVolumeParams{ParentDataset: "k8s"}},
		},
	}
	if _, err = srv.ImportVolume(context.Background(), attach); err != nil {
		t.Fatalf("healthy exact quota import: %v", err)
	}
	owned := agentStateFiles(t, state)

	native.refquota = "2048"
	if _, err = srv.InspectImport(context.Background(), inspect); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("drifted quota inspection = %v, want FailedPrecondition", err)
	}
	if _, err = srv.ImportVolume(context.Background(), attach); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("drifted quota revalidation = %v, want FailedPrecondition", err)
	}
	if after := agentStateFiles(t, state); !maps.Equal(after, owned) {
		t.Fatalf("quota refusal changed owner or proxy state:\nbefore %v\nafter  %v", owned, after)
	}
	if len(native.mutations) != 0 {
		t.Fatalf("quota refusal mutated the source: %v", native.mutations)
	}
}
