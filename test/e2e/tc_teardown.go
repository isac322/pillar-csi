package e2e

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// PresenceProbe reports whether a tracked TC resource still exists.
type PresenceProbe func() (bool, error)

// CleanupFunc removes one tracked TC resource.
type CleanupFunc func() error

// MountResourceSpec declares one TC-scoped mount target that must be unmounted
// and verified absent during teardown.
type MountResourceSpec struct {
	TargetPath string
	Cleanup    CleanupFunc
	IsPresent  PresenceProbe
}

// PathResourceSpec declares one path-backed TC resource such as a volume,
// snapshot, or backend record.
type PathResourceSpec struct {
	Path      string
	Cleanup   CleanupFunc
	IsPresent PresenceProbe
}

// ProcessResourceSpec declares one process spawned by a TC.
type ProcessResourceSpec struct {
	PID       int
	Process   *os.Process
	Cleanup   CleanupFunc
	IsPresent PresenceProbe
}

type teardownResourceKind string

const (
	resourceKindMount         teardownResourceKind = "mount"
	resourceKindVolume        teardownResourceKind = "volume"
	resourceKindSnapshot      teardownResourceKind = "snapshot"
	resourceKindProcess       teardownResourceKind = "process"
	resourceKindBackendRecord teardownResourceKind = "backend record"
)

type trackedResource struct {
	kind      teardownResourceKind
	label     string
	display   string
	cleanup   CleanupFunc
	isPresent PresenceProbe
}

// TrackMount registers one mount target for per-TC teardown enforcement.
func (s *TestCaseScope) TrackMount(label string, spec MountResourceSpec) error {
	targetPath := strings.TrimSpace(spec.TargetPath)
	cleanup := spec.Cleanup
	if cleanup == nil && targetPath != "" {
		cleanup = defaultMountCleanup(targetPath)
	}

	isPresent := spec.IsPresent
	if isPresent == nil && targetPath != "" {
		isPresent = defaultMountPresenceProbe(targetPath)
	}

	if cleanup == nil || isPresent == nil {
		return fmt.Errorf("track mount %q: target path or custom cleanup/probe is required", label)
	}

	return s.registerTrackedResource(resourceKindMount, label, targetPath, cleanup, isPresent)
}

// TrackVolume registers one TC-created volume for teardown enforcement.
func (s *TestCaseScope) TrackVolume(label string, spec PathResourceSpec) error {
	return s.trackPathResource(resourceKindVolume, label, spec)
}

// TrackSnapshot registers one TC-created snapshot for teardown enforcement.
func (s *TestCaseScope) TrackSnapshot(label string, spec PathResourceSpec) error {
	return s.trackPathResource(resourceKindSnapshot, label, spec)
}

// TrackBackendRecord registers one TC-created backend record for teardown
// enforcement.
func (s *TestCaseScope) TrackBackendRecord(label string, spec PathResourceSpec) error {
	return s.trackPathResource(resourceKindBackendRecord, label, spec)
}

// TrackProcess registers one spawned process for teardown enforcement.
func (s *TestCaseScope) TrackProcess(label string, spec ProcessResourceSpec) error {
	pid := spec.PID
	if pid == 0 && spec.Process != nil {
		pid = spec.Process.Pid
	}

	cleanup := spec.Cleanup
	if cleanup == nil && pid > 0 {
		cleanup = defaultProcessCleanup(ProcessResourceSpec{
			PID:     pid,
			Process: spec.Process,
		})
	}

	isPresent := spec.IsPresent
	if isPresent == nil && pid > 0 {
		isPresent = defaultProcessPresenceProbe(ProcessResourceSpec{
			PID:     pid,
			Process: spec.Process,
		})
	}

	if cleanup == nil || isPresent == nil {
		return fmt.Errorf("track process %q: pid/process or custom cleanup/probe is required", label)
	}

	display := fmt.Sprintf("pid %d", pid)
	if pid <= 0 {
		display = label
	}

	return s.registerTrackedResource(resourceKindProcess, label, display, cleanup, isPresent)
}

func (s *TestCaseScope) trackPathResource(kind teardownResourceKind, label string, spec PathResourceSpec) error {
	path := strings.TrimSpace(spec.Path)
	cleanup := spec.Cleanup
	if cleanup == nil && path != "" {
		cleanup = defaultPathCleanup(path)
	}

	isPresent := spec.IsPresent
	if isPresent == nil && path != "" {
		isPresent = defaultPathPresenceProbe(path)
	}

	if cleanup == nil || isPresent == nil {
		return fmt.Errorf("track %s %q: path or custom cleanup/probe is required", kind, label)
	}

	return s.registerTrackedResource(kind, label, path, cleanup, isPresent)
}

func (s *TestCaseScope) registerTrackedResource(
	kind teardownResourceKind,
	label, display string,
	cleanup CleanupFunc,
	isPresent PresenceProbe,
) error {
	if s == nil {
		return errors.New("test case scope is nil")
	}
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("track %s: label is required", kind)
	}

	key := string(kind) + ":" + pathToken(label)
	if display == "" {
		display = label
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("test case scope is closed")
	}
	if _, exists := s.resources[key]; exists {
		return fmt.Errorf("%s %q already tracked for %s", kind, label, s.TCID)
	}

	s.resources[key] = &trackedResource{
		kind:      kind,
		label:     label,
		display:   display,
		cleanup:   cleanup,
		isPresent: isPresent,
	}
	s.resourceOrder = append(s.resourceOrder, key)
	return nil
}

func teardownTrackedResources(resources []*trackedResource) error {
	var errs []error
	for _, resource := range resources {
		if resource == nil {
			continue
		}

		if resource.cleanup != nil {
			if err := resource.cleanup(); err != nil {
				errs = append(errs, fmt.Errorf("%s %q cleanup: %w", resource.kind, resource.label, err))
			}
		}

		if resource.isPresent == nil {
			continue
		}

		present, err := resource.isPresent()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q teardown verification: %w", resource.kind, resource.label, err))
			continue
		}
		if present {
			errs = append(errs, fmt.Errorf(
				"%s %q remained after teardown (%s)",
				resource.kind,
				resource.label,
				resource.display,
			))
		}
	}

	return errors.Join(errs...)
}

func defaultMountCleanup(targetPath string) CleanupFunc {
	cleanPath := filepath.Clean(targetPath)
	return func() error {
		var errs []error
		if err := syscall.Unmount(cleanPath, 0); err != nil &&
			!errors.Is(err, syscall.EINVAL) &&
			!errors.Is(err, syscall.ENOENT) {
			errs = append(errs, fmt.Errorf("unmount %s: %w", cleanPath, err))
		}
		if err := os.RemoveAll(cleanPath); err != nil {
			errs = append(errs, fmt.Errorf("remove mount path %s: %w", cleanPath, err))
		}
		return errors.Join(errs...)
	}
}

func defaultMountPresenceProbe(targetPath string) PresenceProbe {
	cleanPath := filepath.Clean(targetPath)
	return func() (bool, error) {
		file, err := os.Open("/proc/self/mountinfo")
		if err != nil {
			return false, fmt.Errorf("open /proc/self/mountinfo: %w", err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 5 {
				continue
			}
			if fields[4] == cleanPath {
				return true, nil
			}
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("scan /proc/self/mountinfo: %w", err)
		}
		return false, nil
	}
}

func defaultPathCleanup(path string) CleanupFunc {
	cleanPath := filepath.Clean(path)
	return func() error {
		if err := os.RemoveAll(cleanPath); err != nil {
			return fmt.Errorf("remove %s: %w", cleanPath, err)
		}
		return nil
	}
}

func defaultPathPresenceProbe(path string) PresenceProbe {
	cleanPath := filepath.Clean(path)
	return func() (bool, error) {
		_, err := os.Lstat(cleanPath)
		if err == nil {
			return true, nil
		}
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", cleanPath, err)
	}
}

func defaultProcessCleanup(spec ProcessResourceSpec) CleanupFunc {
	pid := spec.PID
	process := spec.Process
	if pid == 0 && process != nil {
		pid = process.Pid
	}

	return func() error {
		if pid <= 0 {
			return nil
		}

		if err := reapProcess(pid); err != nil {
			return fmt.Errorf("reap pid %d: %w", pid, err)
		}

		present, err := processExists(pid)
		if err != nil {
			return fmt.Errorf("check pid %d: %w", pid, err)
		}
		if !present {
			return nil
		}

		if process == nil {
			process, err = os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find pid %d: %w", pid, err)
			}
		}

		if err := signalProcess(process, syscall.SIGTERM); err != nil {
			return fmt.Errorf("terminate pid %d: %w", pid, err)
		}
		exited, err := waitForProcessExit(pid, 500*time.Millisecond)
		if err != nil {
			return fmt.Errorf("wait for pid %d after SIGTERM: %w", pid, err)
		}
		if exited {
			return nil
		}

		if err := signalProcess(process, syscall.SIGKILL); err != nil {
			return fmt.Errorf("kill pid %d: %w", pid, err)
		}
		exited, err = waitForProcessExit(pid, 2*time.Second)
		if err != nil {
			return fmt.Errorf("wait for pid %d after SIGKILL: %w", pid, err)
		}
		if exited {
			return nil
		}

		return fmt.Errorf("pid %d still running after SIGKILL", pid)
	}
}

func defaultProcessPresenceProbe(spec ProcessResourceSpec) PresenceProbe {
	pid := spec.PID
	if pid == 0 && spec.Process != nil {
		pid = spec.Process.Pid
	}

	return func() (bool, error) {
		if pid <= 0 {
			return false, nil
		}
		if err := reapProcess(pid); err != nil {
			return false, fmt.Errorf("reap pid %d: %w", pid, err)
		}
		return processExists(pid)
	}
}

func signalProcess(process *os.Process, signal syscall.Signal) error {
	if process == nil {
		return nil
	}

	if err := process.Signal(signal); err != nil &&
		!errors.Is(err, os.ErrProcessDone) &&
		!errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func waitForProcessExit(pid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := reapProcess(pid); err != nil {
			return false, err
		}

		present, err := processExists(pid)
		if err != nil {
			return false, err
		}
		if !present {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func processExists(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("pid must be positive, got %d", pid)
	}

	if err := syscall.Kill(pid, 0); err != nil {
		switch {
		case errors.Is(err, syscall.ESRCH):
			return false, nil
		case errors.Is(err, syscall.EPERM):
			return true, nil
		default:
			return false, err
		}
	}
	return true, nil
}

func reapProcess(pid int) error {
	if pid <= 0 {
		return nil
	}

	var status syscall.WaitStatus
	_, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if err != nil && !errors.Is(err, syscall.ECHILD) {
		return err
	}
	return nil
}
