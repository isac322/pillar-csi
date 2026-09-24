package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const fencingTestVolume = drainTestPool + "/pvc-fence"

func token(uid string, gen uint64) *agentv1.FencingToken {
	return &agentv1.FencingToken{VolumeUid: uid, Generation: gen}
}

type fenceStep struct {
	name  string
	token *agentv1.FencingToken
	op    fenceOp
	fail  bool // the mutation itself fails
	want  codes.Code
}

func runFenceSteps(t *testing.T, srv *Server, steps []fenceStep) {
	t.Helper()
	for _, step := range steps {
		ran := false
		err := srv.fenced(fencingTestVolume, step.token, step.op, func() error {
			ran = true
			if step.fail {
				return status.Error(codes.Internal, "backend failure")
			}
			return nil
		})
		code := status.Code(err)
		if step.fail && step.want == codes.Internal {
			if code != codes.Internal || !ran {
				t.Fatalf("%s: code %v ran=%t, want Internal from the mutation", step.name, code, ran)
			}
			continue
		}
		if code != step.want {
			t.Fatalf("%s: code %v (err=%v), want %v", step.name, code, err, step.want)
		}
		if ran != (code == codes.OK) {
			t.Fatalf("%s: mutation ran=%t with code %v; a rejected request must not mutate", step.name, ran, code)
		}
	}
}

// Within one lifecycle generations only move forward; an equal generation is
// a retry; a request without a token is always refused.
func TestFenced_GenerationOrdering(t *testing.T) {
	t.Parallel()
	runFenceSteps(t, newDrainTestServer(t.TempDir()), []fenceStep{
		{"no token without history", nil, fenceGrant, false, codes.FailedPrecondition},
		{"empty uid without history", token("", 1), fenceDestroy, false, codes.FailedPrecondition},
		{"first grant", token("u1", 3), fenceGrant, false, codes.OK},
		{"retry", token("u1", 3), fenceGrant, false, codes.OK},
		{"stale grant", token("u1", 2), fenceGrant, false, codes.FailedPrecondition},
		{"stale revoke", token("u1", 2), fenceRevoke, false, codes.FailedPrecondition},
		{"no token with history", nil, fenceRevoke, false, codes.FailedPrecondition},
		{"newer revoke", token("u1", 5), fenceRevoke, false, codes.OK},
		{"stale after revoke", token("u1", 4), fenceGrant, false, codes.FailedPrecondition},
	})
}

// Every mutating RPC refuses a request without a token before touching the
// backend or configfs, even for a volume the agent has never seen.
func TestServer_RequiresFencingToken(t *testing.T) {
	t.Parallel()
	srv := newDrainTestServer(t.TempDir())
	ctx := context.Background()
	calls := map[string]func() error{
		"CreateVolume": func() error {
			_, err := srv.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId: fencingTestVolume, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
			})
			return err
		},
		"ExpandVolume": func() error {
			_, err := srv.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
				VolumeId: fencingTestVolume, RequestedBytes: 1 << 30,
			})
			return err
		},
		"DeleteVolume": func() error {
			_, err := srv.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
				VolumeId: fencingTestVolume, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
			})
			return err
		},
	}
	for name, call := range calls {
		if code := status.Code(call()); code != codes.FailedPrecondition {
			t.Errorf("%s without token: %v, want FailedPrecondition", name, code)
		}
	}
	if _, exists, err := srv.readFencingMark(fencingTestVolume); err != nil || exists {
		t.Errorf("mark after refused requests: exists=%t err=%v, want none", exists, err)
	}
}

// A lifecycle ends only after its backend deletion succeeded; then nothing
// may be granted for it, only the terminal operation may be retried, a new
// lifecycle may claim the volume ID, and the retired one stays refused even
// after the next lifecycle also ended.
func TestFenced_LifecycleTransitions(t *testing.T) {
	t.Parallel()
	runFenceSteps(t, newDrainTestServer(t.TempDir()), []fenceStep{
		{"u1 create", token("u1", 1), fenceGrant, false, codes.OK},
		{"u2 while u1 open", token("u2", 1), fenceGrant, false, codes.FailedPrecondition},
		{"u1 destroy fails", token("u1", 4), fenceDestroy, true, codes.Internal},
		{"u2 after failed destroy", token("u2", 9), fenceGrant, false, codes.FailedPrecondition},
		{"u1 destroy", token("u1", 4), fenceDestroy, false, codes.OK},
		{"u1 grant at equal gen after end", token("u1", 4), fenceGrant, false, codes.FailedPrecondition},
		{"u1 grant at higher gen after end", token("u1", 7), fenceGrant, false, codes.FailedPrecondition},
		{"u1 destroy retry", token("u1", 4), fenceDestroy, false, codes.OK},
		{"u1 revoke at other gen after end", token("u1", 5), fenceRevoke, false, codes.FailedPrecondition},
		{"u2 new lifecycle", token("u2", 1), fenceGrant, false, codes.OK},
		{"u1 retired", token("u1", 4), fenceDestroy, false, codes.FailedPrecondition},
		{"u2 destroy", token("u2", 3), fenceDestroy, false, codes.OK},
		{"u1 still retired after u2 ended", token("u1", 100), fenceGrant, false, codes.FailedPrecondition},
		{"u3 new lifecycle", token("u3", 1), fenceGrant, false, codes.OK},
		{"u2 retired", token("u2", 3), fenceRevoke, false, codes.FailedPrecondition},
	})
}

// Marks are independent per volume, and escaping keeps distinct IDs apart.
func TestFenced_PerVolume(t *testing.T) {
	t.Parallel()
	srv := newDrainTestServer(t.TempDir())
	for _, id := range []string{"a/b", "a_b", drainTestPool + "/x"} {
		if err := srv.fenced(id, token("u-"+id, 9), fenceGrant, nil); err != nil {
			t.Fatalf("%s gen 9: %v", id, err)
		}
	}
	if err := srv.fenced("a_b", token("u-a_b", 1), fenceGrant, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a_b gen 1 after 9: %v, want FailedPrecondition", err)
	}
	if err := srv.fenced("a/c", token("u-a/c", 1), fenceGrant, nil); err != nil {
		t.Fatalf("a/c must not share a mark with a/b: %v", err)
	}
}

// The mark is durable: a new agent process on the same state dir (restart or
// node reboot) still rejects stale and retired requests.
func TestFenced_SurvivesRestart(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	first := newDrainTestServer(stateDir)
	runFenceSteps(t, first, []fenceStep{
		{"u1 create", token("u1", 1), fenceGrant, false, codes.OK},
		{"u1 destroy", token("u1", 6), fenceDestroy, false, codes.OK},
		{"u2 create", token("u2", 2), fenceGrant, false, codes.OK},
	})
	runFenceSteps(t, newDrainTestServer(stateDir), []fenceStep{
		{"u2 stale after restart", token("u2", 1), fenceGrant, false, codes.FailedPrecondition},
		{"u1 retired after restart", token("u1", 6), fenceDestroy, false, codes.FailedPrecondition},
		{"u2 retry after restart", token("u2", 2), fenceGrant, false, codes.OK},
	})
}

// An unreadable mark fails closed: guessing could re-admit a stale operation.
func TestFenced_CorruptMarkFailsClosed(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, fencingDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	markPath := filepath.Join(dir, fencingFilename(fencingTestVolume))
	if err := os.WriteFile(markPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := newDrainTestServer(stateDir).fenced(fencingTestVolume, token("u1", 100), fenceGrant, func() error {
		return errors.New("mutation must not run")
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("corrupt mark: %v, want Internal", err)
	}
}

// A mark that cannot be persisted rejects the operation before it mutates.
func TestFenced_PersistFailureFailsClosed(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// A regular file where the generations directory must be created.
	if err := os.WriteFile(filepath.Join(stateDir, fencingDirName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := newDrainTestServer(stateDir).fenced(fencingTestVolume, token("u1", 1), fenceGrant, func() error {
		return errors.New("mutation must not run")
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("unpersistable mark: %v, want Internal", err)
	}
}

// The DeleteVolume RPC ends the lifecycle: the same UID can no longer create
// or grant, a delete retry still succeeds, and a new UID may start.
func TestDeleteVolume_EndsLifecycle(t *testing.T) {
	t.Parallel()
	srv := newDrainTestServer(t.TempDir())
	ctx := context.Background()
	del := &agentv1.DeleteVolumeRequest{
		VolumeId:    fencingTestVolume,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:       token("u1", 3),
	}

	if _, err := srv.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId: fencingTestVolume, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL, Fence: token("u1", 1),
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, err := srv.DeleteVolume(ctx, del); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := srv.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId: fencingTestVolume, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL, Fence: token("u1", 3),
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateVolume of ended lifecycle: %v, want FailedPrecondition", err)
	}
	if _, err := srv.DeleteVolume(ctx, del); err != nil {
		t.Fatalf("DeleteVolume retry: %v", err)
	}
	if _, err := srv.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId: fencingTestVolume, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL, Fence: token("u2", 1),
	}); err != nil {
		t.Fatalf("CreateVolume of new lifecycle: %v", err)
	}
}
