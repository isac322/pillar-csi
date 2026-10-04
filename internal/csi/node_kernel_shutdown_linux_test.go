//go:build linux && kernel_integration

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

package csi

// Real-kernel proof for issues #168, #172 and #175: an XFS on a loop device
// is staged and published through the production NodeServer and KubeMounter,
// shut down with xfs_io, and driven through kubelet's pod-delete sequence.
//
// The test mounts filesystems and needs root, a loop device, and xfsprogs.
// It is compiled only with the kernel_integration build tag and fails
// (never skips) when a prerequisite is missing:
//
//	sudo -E go test -tags=kernel_integration -count=1 -v \
//	    -run TestKernel ./internal/csi/

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
)

const kernelVolumeID = "tank/pvc-kernel-shutdown"

// kernelPayload is written through the first pod bind before the shutdown
// and must survive the log replay of the repair.
var kernelPayload = []byte("pillar-csi issue 175 payload\n")

// kernelRun runs a host command and fails the test on error.
func kernelRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // G204: fixed test tools, test-owned args
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// kernelMounted reports whether path has a /proc/self/mountinfo entry; a
// lookup error counts as unmounted so cleanup loops always terminate.
func kernelMounted(path string) bool {
	ok, err := NewKubeMounter().MountEntryExists(path)
	return err == nil && ok
}

// kernelFixture is one loop-device XFS volume served by the production
// NodeServer and KubeMounter.
type kernelFixture struct {
	t           *testing.T
	ctx         context.Context
	srv         *NodeServer
	dev         string
	stagingPath string
	targetA     string
	targetB     string
	stageReq    *csi.NodeStageVolumeRequest
}

// newKernelFixture checks prerequisites, creates and formats the loop
// device, and registers cleanup that leaves the host without stray mounts
// or loop devices even when an assertion fails mid-sequence.
func newKernelFixture(t *testing.T) *kernelFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("kernel_integration tests need root: they mount filesystems on a loop device")
	}
	for _, tool := range []string{"losetup", "mkfs.xfs", "xfs_io", "blkid", "umount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("kernel_integration tests need %s: %v", tool, err)
		}
	}
	t.Logf("kernel: %s", kernelRun(t, "uname", "-srm"))

	base := t.TempDir()
	img := filepath.Join(base, "volume.img")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 512<<20); err != nil { //nolint:mnd // above the mkfs.xfs minimum size
		t.Fatal(err)
	}
	dev := kernelRun(t, "losetup", "--find", "--show", img)
	t.Cleanup(func() { kernelRun(t, "losetup", "-d", dev) })
	kernelRun(t, "mkfs.xfs", "-q", "-f", dev)

	// Kubelet paths go through a symlinked root, like a node whose
	// /var/lib/kubelet points at another disk: mountinfo records the
	// resolved mount points, so every mount-table lookup must resolve too.
	kubeletRoot := filepath.Join(t.TempDir(), "kubelet")
	if err := os.Symlink(base, kubeletRoot); err != nil {
		t.Fatal(err)
	}
	fx := &kernelFixture{
		t:           t,
		ctx:         context.Background(),
		dev:         dev,
		stagingPath: filepath.Join(kubeletRoot, "globalmount"),
		targetA:     filepath.Join(kubeletRoot, "pod-a"),
		targetB:     filepath.Join(kubeletRoot, "pod-b"),
	}
	stateDir := filepath.Join(base, "state")
	for _, d := range []string{fx.stagingPath, fx.targetA, fx.targetB, stateDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// Runs before the loop device is detached: cleanups are LIFO.
	t.Cleanup(func() {
		for _, p := range []string{fx.targetB, fx.targetA, fx.stagingPath} {
			for kernelMounted(p) {
				out, err := exec.Command("umount", "-l", p).CombinedOutput() //nolint:gosec // G204: test-owned path
				if err != nil {
					t.Errorf("cleanup umount %s: %v: %s", p, err, out)
					break
				}
			}
		}
	})

	fx.srv = NewNodeServerWithStateDir("kernel-node", &mockConnector{devicePath: dev}, NewKubeMounter(), stateDir)
	fx.stageReq = &csi.NodeStageVolumeRequest{
		VolumeId: kernelVolumeID, StagingTargetPath: fx.stagingPath,
		VolumeCapability: mountCap("xfs"),
		VolumeContext:    mountVolumeContext("nqn.test:kernel-shutdown", testStorageAddr),
	}
	return fx
}

func (fx *kernelFixture) stage() error {
	_, err := fx.srv.NodeStageVolume(fx.ctx, fx.stageReq)
	return err
}

func (fx *kernelFixture) publish(target string) error {
	_, err := fx.srv.NodePublishVolume(fx.ctx, &csi.NodePublishVolumeRequest{
		VolumeId: kernelVolumeID, StagingTargetPath: fx.stagingPath, TargetPath: target,
		VolumeCapability: mountCap("xfs"), VolumeContext: fx.stageReq.GetVolumeContext(),
	})
	return err
}

func (fx *kernelFixture) unpublish(target string) error {
	_, err := fx.srv.NodeUnpublishVolume(fx.ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId: kernelVolumeID, TargetPath: target,
	})
	return err
}

// requireMounted fails unless path's mount-table presence equals want.
func (fx *kernelFixture) requireMounted(path string, want bool, why string) {
	fx.t.Helper()
	ok, err := NewKubeMounter().MountEntryExists(path)
	if err != nil {
		fx.t.Fatalf("MountEntryExists(%s): %v", path, err)
	}
	if ok != want {
		fx.t.Fatalf("%s: %s mounted=%v, want %v", why, path, ok, want)
	}
}

// writeDurable writes kernelPayload to dir/data and fsyncs it.
func (fx *kernelFixture) writeDurable(dir string) {
	fx.t.Helper()
	f, err := os.Create(filepath.Join(dir, "data")) //nolint:gosec // G304: test-owned path
	if err != nil {
		fx.t.Fatalf("write through bind %s: %v", dir, err)
	}
	defer f.Close() //nolint:errcheck // synced below; close error irrelevant after fsync
	if _, err = f.Write(kernelPayload); err != nil {
		fx.t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		fx.t.Fatal(err)
	}
}

// shutdown forces an XFS shutdown and records the kernel's view of it.
func (fx *kernelFixture) shutdown() {
	fx.t.Helper()
	kernelRun(fx.t, "xfs_io", "-x", "-c", "shutdown", fx.stagingPath)
	var st unix.Stat_t
	fx.t.Logf("after shutdown: stat(%s) = %v", fx.stagingPath, unix.Stat(fx.stagingPath, &st))
	fx.requireMounted(fx.stagingPath, true, "shutdown must keep the dead staged mount listed")
	fx.requireMounted(fx.targetA, true, "shutdown must keep the dead bind listed")
	km := NewKubeMounter()
	if readErr := km.CheckMountReadable(fx.stagingPath); !errors.Is(readErr, ErrMountUnhealthy) {
		fx.t.Fatalf("CheckMountReadable after shutdown = %v, want ErrMountUnhealthy", readErr)
	}
	if readErr := km.CheckMountReadable(fx.targetA); !errors.Is(readErr, ErrMountUnhealthy) {
		fx.t.Fatalf("CheckMountReadable(bind A) after shutdown = %v, want ErrMountUnhealthy", readErr)
	}
	if healthErr := km.CheckMountHealth(fx.stagingPath); !errors.Is(healthErr, ErrMountUnhealthy) {
		fx.t.Fatalf("CheckMountHealth after shutdown = %v, want ErrMountUnhealthy", healthErr)
	}
}

// requirePinnedRefusal checks that publish and stage both refuse to touch
// the dead staged mount while bind A still pins its superblock.
func (fx *kernelFixture) requirePinnedRefusal() {
	fx.t.Helper()
	err := fx.publish(fx.targetB)
	requireGRPCCode(fx.t, err, codes.Internal)
	fx.t.Logf("publish(B) while pinned: %v", err)
	fx.requireMounted(fx.stagingPath, true, "publish must not unmount a pinned dead staged mount")
	fx.requireMounted(fx.targetB, false, "publish must not bind the dead filesystem")

	err = fx.stage()
	requireGRPCCode(fx.t, err, codes.Internal)
	fx.t.Logf("NodeStageVolume while pinned: %v", err)
	fx.requireMounted(fx.stagingPath, true, "stage must not unmount a pinned dead staged mount")
}

// requireRepaired checks the repaired staged filesystem through bind B:
// healthy, the pre-shutdown data intact after log replay, and writable.
func (fx *kernelFixture) requireRepaired() {
	fx.t.Helper()
	if err := NewKubeMounter().CheckMountHealth(fx.stagingPath); err != nil {
		fx.t.Fatalf("staged XFS still unhealthy after the publish repair: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(fx.targetB, "data"))
	if err != nil {
		fx.t.Fatalf("read data through repaired bind B: %v", err)
	}
	if !bytes.Equal(got, kernelPayload) {
		fx.t.Fatalf("data after log replay = %q, want %q", got, kernelPayload)
	}
	if err = os.WriteFile(filepath.Join(fx.targetB, "after-repair"), kernelPayload, 0o600); err != nil {
		fx.t.Fatalf("write through repaired bind B: %v", err)
	}
}

// logXFSKernelMessages copies the loop device's XFS kernel log lines (mount,
// shutdown, log recovery) into the test output as evidence.
func (fx *kernelFixture) logXFSKernelMessages() {
	out, err := exec.Command("dmesg").CombinedOutput()
	if err != nil {
		fx.t.Logf("dmesg unavailable: %v", err)
		return
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "XFS ("+filepath.Base(fx.dev)+")") {
			fx.t.Logf("dmesg: %s", strings.TrimSpace(line))
		}
	}
}

// TestKernel_PublishRepairsShutdownXFS drives the live failure sequence of
// issue #175 against the real kernel:
//
//  1. stage + publish target A, write and fsync data through the bind;
//  2. xfs_io -x shutdown on the staged filesystem: stat answers EIO, the
//     mount entries stay;
//  3. publish target B and re-stage while A pins the dead superblock:
//     retryable Internal, nothing unmounted;
//  4. NodeUnpublishVolume(A), then publish target B again: the staged
//     mount is repaired in place (unmount + fresh mount replays the log),
//     the data written in step 1 is intact, and B is writable;
//  5. unpublish + unstage leave nothing mounted.
func TestKernel_PublishRepairsShutdownXFS(t *testing.T) {
	fx := newKernelFixture(t)

	if err := fx.stage(); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if err := fx.publish(fx.targetA); err != nil {
		t.Fatalf("NodePublishVolume(A): %v", err)
	}
	fx.writeDurable(fx.targetA)

	fx.shutdown()
	fx.requirePinnedRefusal()

	if err := fx.unpublish(fx.targetA); err != nil {
		t.Fatalf("NodeUnpublishVolume(A) on a dead bind: %v", err)
	}
	fx.requireMounted(fx.targetA, false, "NodeUnpublishVolume must remove the dead bind")
	if err := fx.publish(fx.targetB); err != nil {
		t.Fatalf("NodePublishVolume(B) after teardown must repair the staged XFS in place: %v", err)
	}
	fx.requireRepaired()
	fx.logXFSKernelMessages()

	if err := fx.unpublish(fx.targetB); err != nil {
		t.Fatalf("NodeUnpublishVolume(B): %v", err)
	}
	if _, err := fx.srv.NodeUnstageVolume(fx.ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: kernelVolumeID, StagingTargetPath: fx.stagingPath,
	}); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	for _, p := range []string{fx.stagingPath, fx.targetA, fx.targetB} {
		fx.requireMounted(p, false, "teardown must leave nothing mounted")
	}
}
