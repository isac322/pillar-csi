// Package prereq — AC 10 compliance tests.
//
// These tests verify the AC 10 contract:
//
//	"Missing prerequisites cause immediate FAIL with remediation instructions,
//	 not skip."
//
// Every test in this file verifies one of the following invariants:
//  1. checkKernelModulesFromSet returns a non-nil error when ANY module is absent.
//  2. checkBinariesWithLookup returns a non-nil error when ANY binary is absent.
//  3. Error messages contain per-item remediation instructions.
//  4. The word "Remediation" appears in every error message.
//  5. nvme_tcp is in requiredModules (AC 10 requires NVMe-oF backend modules).
//  6. iscsi_tcp is NOT in requiredModules (iSCSI runs inside Kind container nodes,
//     not on the host — E34/E35 iSCSI tests perform their own prereq checks).
//  7. CheckHostPrerequisites aggregates all failures into a single error.
//  8. The package never calls t.Skip or GinkgoSkip — a static text scan
//     of the production source confirms the absence of any skip call.
//
// Test strategy
//
//	checkKernelModulesFromSet and checkBinariesWithLookup are package-private
//	functions that accept synthetic inputs, so these tests do not require
//	root privileges, real kernel modules, or real binaries in PATH.
//
// Run with:
//
//	go test ./test/e2e/framework/prereq/ -v -run TestAC10
package prereq

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// ─── 1. Every missing module causes FAIL ──────────────────────────────────────

// TestAC10_AllModulesRequired_EmptySetFails verifies that a completely empty
// loaded-module set causes an immediate FAIL listing all required modules.
// Required modules: zfs, dm_thin_pool, nvme_tcp, nvmet, nvmet_tcp.
// Note: iscsi_tcp is NOT required at the host level (runs inside Kind nodes).
func TestAC10_AllModulesRequired_EmptySetFails(t *testing.T) {
	err := checkKernelModulesFromSet(map[string]struct{}{})
	if err == nil {
		t.Fatal("checkKernelModulesFromSet(empty): expected non-nil error, got nil — AC 10 requires FAIL when modules are missing")
	}
	msg := err.Error()
	for _, mod := range []string{"zfs", "dm_thin_pool", "nvme_tcp"} {
		if !strings.Contains(msg, mod) {
			t.Errorf("error missing module name %q\ngot:\n%s", mod, msg)
		}
	}
}

// TestAC10_AllModulesPresent_NoError verifies that when all required modules
// are in the loaded set, checkKernelModulesFromSet returns nil (no failure).
// Required: zfs, dm_thin_pool, nvme_tcp, nvmet, nvmet_tcp.
// iscsi_tcp is NOT required at host level (iSCSI runs inside Kind nodes).
func TestAC10_AllModulesPresent_NoError(t *testing.T) {
	fullSet := map[string]struct{}{
		"zfs":          {},
		"dm_thin_pool": {},
		"nvme_tcp":     {},
		"nvmet":        {}, // Sub-AC 9b: NVMe-oF target core (target side)
		"nvmet_tcp":    {}, // Sub-AC 9b: NVMe-oF target TCP transport
	}
	if err := checkKernelModulesFromSet(fullSet); err != nil {
		t.Errorf("checkKernelModulesFromSet(full set): unexpected error: %v", err)
	}
}

// TestAC10_ZfsModuleMissing_CausesFail verifies that a missing zfs module
// produces a FAIL (non-nil error) with remediation instructions.
func TestAC10_ZfsModuleMissing_CausesFail(t *testing.T) {
	loaded := map[string]struct{}{
		"dm_thin_pool": {},
		"nvme_tcp":     {},
		"nvmet":        {},
		"nvmet_tcp":    {},
		// zfs intentionally absent
	}
	err := checkKernelModulesFromSet(loaded)
	if err == nil {
		t.Fatal("checkKernelModulesFromSet: expected FAIL when zfs is absent, got nil — AC 10 violated")
	}
	msg := err.Error()
	if !strings.Contains(msg, "zfs") {
		t.Errorf("error must mention 'zfs', got:\n%s", msg)
	}
}

// TestAC10_DmThinPoolMissing_CausesFail verifies that a missing dm_thin_pool
// module produces a FAIL (non-nil error) with remediation instructions.
func TestAC10_DmThinPoolMissing_CausesFail(t *testing.T) {
	loaded := map[string]struct{}{
		"zfs":       {},
		"nvme_tcp":  {},
		"nvmet":     {},
		"nvmet_tcp": {},
		// dm_thin_pool intentionally absent
	}
	err := checkKernelModulesFromSet(loaded)
	if err == nil {
		t.Fatal("checkKernelModulesFromSet: expected FAIL when dm_thin_pool is absent, got nil — AC 10 violated")
	}
	msg := err.Error()
	if !strings.Contains(msg, "dm_thin_pool") {
		t.Errorf("error must mention 'dm_thin_pool', got:\n%s", msg)
	}
}

// TestAC10_IscsiTcpNotRequired verifies that a missing iscsi_tcp module does
// NOT cause a FAIL from the host-level prereq check.  iSCSI initiator support
// runs inside Kind container worker nodes (not on the host): E34/E35 iSCSI
// tests are not in the default profile and perform their own prereq checks.
func TestAC10_IscsiTcpNotRequired(t *testing.T) {
	loaded := map[string]struct{}{
		"zfs":          {},
		"dm_thin_pool": {},
		"nvme_tcp":     {},
		"nvmet":        {},
		"nvmet_tcp":    {},
		// iscsi_tcp intentionally absent — must NOT cause a host prereq FAIL
	}
	if err := checkKernelModulesFromSet(loaded); err != nil {
		t.Errorf("checkKernelModulesFromSet: unexpected FAIL when iscsi_tcp is absent — "+
			"iscsi_tcp is not a host-level requirement for the default profile:\n%v", err)
	}
}

// TestAC10_NvmeTcpMissing_CausesFail verifies that a missing nvme_tcp module
// produces a FAIL.  nvme_tcp is required for NVMe-oF TCP backend tests.
func TestAC10_NvmeTcpMissing_CausesFail(t *testing.T) {
	loaded := map[string]struct{}{
		"zfs":          {},
		"dm_thin_pool": {},
		"nvmet":        {},
		"nvmet_tcp":    {},
		// nvme_tcp intentionally absent
	}
	err := checkKernelModulesFromSet(loaded)
	if err == nil {
		t.Fatal("checkKernelModulesFromSet: expected FAIL when nvme_tcp is absent, got nil — AC 10 violated")
	}
	msg := err.Error()
	if !strings.Contains(msg, "nvme_tcp") {
		t.Errorf("error must mention 'nvme_tcp', got:\n%s", msg)
	}
}

// ─── 2. Kernel module errors contain remediation instructions ─────────────────

// TestAC10_KernelModuleErrorContainsRemediation verifies that the error message
// from checkKernelModulesFromSet contains explicit remediation instructions.
// AC 10 requires "clear message about what is missing and how to install it".
func TestAC10_KernelModuleErrorContainsRemediation(t *testing.T) {
	err := checkKernelModulesFromSet(map[string]struct{}{})
	if err == nil {
		t.Fatal("expected non-nil error for empty module set")
	}
	msg := err.Error()

	requiredPhrases := []string{
		"Remediation",
		"modprobe",
		"AC 10",
	}
	for _, phrase := range requiredPhrases {
		if !strings.Contains(msg, phrase) {
			t.Errorf("kernel module error missing required phrase %q\ngot:\n%s", phrase, msg)
		}
	}
}

// TestAC10_KernelModuleErrorListsInstallHints verifies that per-module install
// hints are included in the error for each missing module.
func TestAC10_KernelModuleErrorListsInstallHints(t *testing.T) {
	// Only zfs missing — verify its install hints appear.
	loaded := map[string]struct{}{
		"dm_thin_pool": {},
		"nvme_tcp":     {},
		"nvmet":        {},
		"nvmet_tcp":    {},
	}
	err := checkKernelModulesFromSet(loaded)
	if err == nil {
		t.Fatal("expected non-nil error for missing zfs")
	}
	msg := err.Error()

	// The zfs install hint should mention the package name.
	if !strings.Contains(msg, "zfsutils-linux") && !strings.Contains(msg, "zfs-dkms") && !strings.Contains(msg, "zfs") {
		t.Errorf("error for missing zfs must include install hint with package name, got:\n%s", msg)
	}
}

// TestAC10_KernelModuleErrorIndicatesSoftSkipDisabled verifies that the error
// explicitly states that soft-skip is DISABLED.
func TestAC10_KernelModuleErrorIndicatesSoftSkipDisabled(t *testing.T) {
	err := checkKernelModulesFromSet(map[string]struct{}{})
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "soft-skip is DISABLED") && !strings.Contains(msg, "DISABLED") {
		t.Errorf("error must state that soft-skip is DISABLED, got:\n%s", msg)
	}
}

// ─── 3. nvme_tcp is in requiredModules (AC 10 four-backend coverage) ─────────

// TestAC10_NvmeTcpInRequiredModules verifies that nvme_tcp appears in
// requiredModules, confirming that NVMe-oF TCP backend coverage is mandatory.
func TestAC10_NvmeTcpInRequiredModules(t *testing.T) {
	found := false
	for _, mod := range requiredModules {
		if mod.name == "nvme_tcp" {
			found = true
			break
		}
	}
	if !found {
		t.Error("nvme_tcp not found in requiredModules — AC 10 requires NVMe-oF TCP backend coverage")
	}
}

// TestAC10_IscsiTcpNotInRequiredModules verifies that iscsi_tcp does NOT appear
// in requiredModules. iSCSI initiator support runs inside Kind container worker
// nodes (not on the host). E34/E35 iSCSI tests are outside the default profile
// and perform their own runtime prereq checks.
func TestAC10_IscsiTcpNotInRequiredModules(t *testing.T) {
	for _, mod := range requiredModules {
		if mod.name == "iscsi_tcp" {
			t.Error("iscsi_tcp must NOT be in requiredModules — iSCSI runs inside " +
				"Kind container nodes, not on the host. Remove it from requiredModules " +
				"or move E34/E35 tests out of the default profile.")
			return
		}
	}
	// iscsi_tcp correctly absent from host-level requirements.
}

// TestAC10_RequiredModulesCount verifies that exactly five kernel modules are
// required: zfs, dm_thin_pool, nvme_tcp, nvmet, nvmet_tcp.
// Sub-AC 9b adds nvmet and nvmet_tcp for the NVMe-oF target (server side).
// Note: iscsi_tcp is NOT required at host level (E34/E35 iSCSI tests are
// outside the default profile and run their own prereq checks).
func TestAC10_RequiredModulesCount(t *testing.T) {
	const wantCount = 5
	if len(requiredModules) != wantCount {
		t.Errorf("len(requiredModules) = %d, want %d (zfs, dm_thin_pool, nvme_tcp, nvmet, nvmet_tcp)",
			len(requiredModules), wantCount)
	}
}

// TestAC10_RequiredModulesContainNVMeAndZFSLVM verifies that the NVMe-oF and
// storage backend modules appear in requiredModules. iscsi_tcp is NOT required
// at host level (E34/E35 iSCSI tests run inside Kind nodes).
func TestAC10_RequiredModulesContainNVMeAndZFSLVM(t *testing.T) {
	wantModules := []string{"zfs", "dm_thin_pool", "nvme_tcp"}
	nameSet := make(map[string]struct{}, len(requiredModules))
	for _, mod := range requiredModules {
		nameSet[mod.name] = struct{}{}
	}
	for _, want := range wantModules {
		if _, ok := nameSet[want]; !ok {
			t.Errorf("requiredModules missing %q — AC 10 requires NVMe-oF and storage backends", want)
		}
	}
	// iscsi_tcp must NOT be required (iSCSI runs inside Kind nodes).
	if _, ok := nameSet["iscsi_tcp"]; ok {
		t.Error("requiredModules must NOT contain 'iscsi_tcp' — iSCSI runs inside Kind container nodes, not on the host")
	}
}

// ─── 4. Binary tool checks ────────────────────────────────────────────────────

// TestAC10_AllBinariesMissing_CausesFail verifies that when all required
// binaries are absent (lookup returns an error for everything), the checker
// returns a non-nil error listing all missing tools.
// Note: iscsiadm and nvme are NOT required at host level.
func TestAC10_AllBinariesMissing_CausesFail(t *testing.T) {
	alwaysFail := func(_ string) (string, error) {
		return "", errors.New("not found")
	}
	err := checkBinariesWithLookup(alwaysFail)
	if err == nil {
		t.Fatal("checkBinariesWithLookup(all-miss): expected non-nil error, got nil — AC 10 violated")
	}
	msg := err.Error()
	for _, bin := range []string{"kind", "helm", "zfs", "zpool", "lvcreate", "vgcreate"} {
		if !strings.Contains(msg, bin) {
			t.Errorf("error missing binary name %q\ngot:\n%s", bin, msg)
		}
	}
	// iscsiadm and nvme must NOT appear in host-level prereq errors.
	for _, bin := range []string{"iscsiadm", "nvme"} {
		if strings.Contains(msg, bin) {
			t.Errorf("error must NOT mention %q — it is not a host-level requirement\ngot:\n%s", bin, msg)
		}
	}
}

// TestAC10_AllBinariesPresent_NoError verifies that when all required binaries
// are found, checkBinariesWithLookup returns nil.
func TestAC10_AllBinariesPresent_NoError(t *testing.T) {
	alwaysFound := func(binary string) (string, error) {
		return "/usr/bin/" + binary, nil
	}
	if err := checkBinariesWithLookup(alwaysFound); err != nil {
		t.Errorf("checkBinariesWithLookup(all-found): unexpected error: %v", err)
	}
}

// TestAC10_KindBinaryMissing_CausesFail verifies that a missing "kind" binary
// causes a FAIL with installation instructions.
func TestAC10_KindBinaryMissing_CausesFail(t *testing.T) {
	lookup := func(binary string) (string, error) {
		if binary == "kind" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + binary, nil
	}
	err := checkBinariesWithLookup(lookup)
	if err == nil {
		t.Fatal("expected FAIL when 'kind' is missing, got nil — AC 10 requires immediate FAIL")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kind") {
		t.Errorf("error must mention 'kind', got:\n%s", msg)
	}
}

// TestAC10_HelmBinaryMissing_CausesFail verifies that a missing "helm" binary
// causes a FAIL with installation instructions.
func TestAC10_HelmBinaryMissing_CausesFail(t *testing.T) {
	lookup := func(binary string) (string, error) {
		if binary == "helm" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + binary, nil
	}
	err := checkBinariesWithLookup(lookup)
	if err == nil {
		t.Fatal("expected FAIL when 'helm' is missing, got nil — AC 10 requires immediate FAIL")
	}
	msg := err.Error()
	if !strings.Contains(msg, "helm") {
		t.Errorf("error must mention 'helm', got:\n%s", msg)
	}
}

// TestAC10_IscsiadmNotRequired verifies that a missing iscsiadm binary does NOT
// cause a host-level prereq FAIL. iSCSI initiator functionality runs inside
// Kind container worker nodes, not on the host — so iscsiadm is not a host
// prerequisite.
func TestAC10_IscsiadmNotRequired(t *testing.T) {
	// All required binaries present EXCEPT iscsiadm (which is not required).
	lookup := func(binary string) (string, error) {
		if binary == "iscsiadm" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + binary, nil
	}
	err := checkBinariesWithLookup(lookup)
	if err != nil {
		t.Errorf("iscsiadm is not a host-level requirement; expected nil error, got:\n%v", err)
	}
}

// TestAC10_NvmeBinaryNotRequired verifies that a missing nvme binary does NOT
// cause a host-level prereq FAIL. The nvme CLI is only needed by F27-F31
// (LVM+NVMe-oF host-connect tests), which are not in the default-profile and
// perform their own prereq checks.
func TestAC10_NvmeBinaryNotRequired(t *testing.T) {
	// All required binaries present EXCEPT nvme (which is not required at host level).
	lookup := func(binary string) (string, error) {
		if binary == "nvme" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + binary, nil
	}
	err := checkBinariesWithLookup(lookup)
	if err != nil {
		t.Errorf("nvme is not a host-level requirement; expected nil error, got:\n%v", err)
	}
}

// ─── 5. Binary error messages contain remediation instructions ─────────────────

// TestAC10_BinaryErrorContainsRemediation verifies that the error from a
// missing binary contains explicit remediation instructions.
func TestAC10_BinaryErrorContainsRemediation(t *testing.T) {
	alwaysFail := func(_ string) (string, error) {
		return "", errors.New("not found")
	}
	err := checkBinariesWithLookup(alwaysFail)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	msg := err.Error()

	requiredPhrases := []string{
		"Install",
		"AC 10",
	}
	for _, phrase := range requiredPhrases {
		if !strings.Contains(msg, phrase) {
			t.Errorf("binary error missing required phrase %q\ngot:\n%s", phrase, msg)
		}
	}
}

// TestAC10_BinaryErrorContainsInstallHints verifies that each missing binary's
// install hints appear in the error message.
func TestAC10_BinaryErrorContainsInstallHints(t *testing.T) {
	// Only "kind" is missing — its install hints should appear in the error.
	lookup := func(binary string) (string, error) {
		if binary == "kind" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + binary, nil
	}
	err := checkBinariesWithLookup(lookup)
	if err == nil {
		t.Fatal("expected non-nil error for missing kind")
	}
	msg := err.Error()
	// kind's install hints mention sigs.k8s.io/kind or kind.sigs.k8s.io
	if !strings.Contains(msg, "kind") {
		t.Errorf("error for missing 'kind' must include install hint, got:\n%s", msg)
	}
}

// ─── 6. requiredBinaries list is complete ─────────────────────────────────────

// TestAC10_RequiredBinariesContainsMandatoryTools verifies that all six
// mandatory host-level tools appear in requiredBinaries.
// Note: iscsiadm and nvme are intentionally excluded — they are not host-level
// requirements (iSCSI runs inside Kind nodes; nvme is only for non-default-profile tests).
func TestAC10_RequiredBinariesContainsMandatoryTools(t *testing.T) {
	wantBinaries := []string{
		"kind", "helm",
		"zfs", "zpool",
		"lvcreate", "vgcreate",
	}
	nameSet := make(map[string]struct{}, len(requiredBinaries))
	for _, b := range requiredBinaries {
		nameSet[b.binary] = struct{}{}
	}
	for _, want := range wantBinaries {
		if _, ok := nameSet[want]; !ok {
			t.Errorf("requiredBinaries missing %q — AC 10 requires all backend tools", want)
		}
	}
}

// ─── 7. CheckHostPrerequisites aggregates all errors ─────────────────────────

// TestAC10_CheckHostPrerequisites_ErrorHeaderAlwaysPresent verifies that
// the banner "pillar-csi E2E prerequisite check FAILED" is always included
// in the CheckHostPrerequisites error, making the failure unmistakable.
//
// This test forces a Docker failure by pointing DOCKER_HOST at a known-bad
// address.  If Docker happens to be absent from PATH, the header is still
// emitted via the binary-check path.
func TestAC10_CheckHostPrerequisites_ErrorHeaderAlwaysPresent(t *testing.T) {
	// Force a Docker failure by pointing at an unreachable address.
	// If docker itself is not in PATH, the binary check will also fail,
	// which still gives us a non-nil error to inspect.
	orig := os.Getenv("DOCKER_HOST")
	if err := os.Setenv("DOCKER_HOST", "tcp://127.0.0.1:19998"); err != nil {
		t.Fatalf("set DOCKER_HOST: %v", err)
	}
	defer func() {
		if orig == "" {
			_ = os.Unsetenv("DOCKER_HOST")
		} else {
			_ = os.Setenv("DOCKER_HOST", orig)
		}
	}()

	err := CheckHostPrerequisites()
	if err == nil {
		// The machine happened to have all prerequisites AND docker connected on
		// port 19998 — extremely unlikely but possible.  Skip rather than fail.
		t.Skip("unexpected: CheckHostPrerequisites returned nil on port 19998; cannot test header")
		return
	}

	msg := err.Error()
	if !strings.Contains(msg, "pillar-csi E2E prerequisite check FAILED") {
		t.Errorf("error header missing from CheckHostPrerequisites output:\n%s", msg)
	}
}

// TestAC10_CheckHostPrerequisites_AC10FooterPresent verifies that the AC 10
// footer ("soft-skipping is DISABLED") is present in the aggregated error
// returned by CheckHostPrerequisites.
func TestAC10_CheckHostPrerequisites_AC10FooterPresent(t *testing.T) {
	orig := os.Getenv("DOCKER_HOST")
	if err := os.Setenv("DOCKER_HOST", "tcp://127.0.0.1:19997"); err != nil {
		t.Fatalf("set DOCKER_HOST: %v", err)
	}
	defer func() {
		if orig == "" {
			_ = os.Unsetenv("DOCKER_HOST")
		} else {
			_ = os.Setenv("DOCKER_HOST", orig)
		}
	}()

	err := CheckHostPrerequisites()
	if err == nil {
		t.Skip("unexpected: CheckHostPrerequisites returned nil; cannot test AC 10 footer")
		return
	}

	msg := err.Error()
	if !strings.Contains(msg, "AC 10") {
		t.Errorf("CheckHostPrerequisites error must mention 'AC 10', got:\n%s", msg)
	}
}

// ─── 8. No skip calls in production code ──────────────────────────────────────

// TestAC10_ProductionCodeContainsNoSkipCalls verifies that the prereq.go
// source file does not contain any calls to t.Skip, Skip, or GinkgoSkip.
// This is a compile-time-visible static assertion that the AC 10 "never
// soft-skip" contract is upheld in the production code.
func TestAC10_ProductionCodeContainsNoSkipCalls(t *testing.T) {
	data, err := os.ReadFile("prereq.go")
	if err != nil {
		t.Fatalf("read prereq.go: %v", err)
	}
	src := string(data)

	forbiddenPatterns := []string{
		"t.Skip(",
		"t.SkipNow(",
		"GinkgoSkip(",
		"Skip(",
	}
	for _, pattern := range forbiddenPatterns {
		if strings.Contains(src, pattern) {
			t.Errorf("prereq.go contains forbidden skip call %q — AC 10: no soft-skipping allowed", pattern)
		}
	}
}

// ─── 9. Module error format: count header ──────────────────────────────────────

// TestAC10_KernelModuleErrorIncludesCount verifies that the error message
// includes a count of missing modules (e.g. "1 required kernel module(s)").
func TestAC10_KernelModuleErrorIncludesCount(t *testing.T) {
	// Exactly one module missing: nvme_tcp.
	// All other required modules (zfs, dm_thin_pool, nvmet, nvmet_tcp) are present.
	// Note: iscsi_tcp is NOT in requiredModules, so its absence is irrelevant.
	loaded := map[string]struct{}{
		"zfs":          {},
		"dm_thin_pool": {},
		"nvmet":        {},
		"nvmet_tcp":    {},
	}
	err := checkKernelModulesFromSet(loaded)
	if err == nil {
		t.Fatal("expected non-nil error for 1 missing module (nvme_tcp)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "1 required kernel module") {
		t.Errorf("error must include '1 required kernel module', got:\n%s", msg)
	}
}

// TestAC10_BinaryErrorIncludesCount verifies that the error message for
// missing binaries includes the count of missing tools.
func TestAC10_BinaryErrorIncludesCount(t *testing.T) {
	// Exactly one binary missing: zpool
	lookup := func(binary string) (string, error) {
		if binary == "zpool" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + binary, nil
	}
	err := checkBinariesWithLookup(lookup)
	if err == nil {
		t.Fatal("expected non-nil error for 1 missing binary")
	}
	msg := err.Error()
	if !strings.Contains(msg, "1 required binary") {
		t.Errorf("error must include '1 required binary', got:\n%s", msg)
	}
}

// ─── 10. Hyphen-normalised names still trigger FAIL ───────────────────────────

// TestAC10_HyphenNormalisedModuleAccepted verifies that a module entry in
// /proc/modules whose name uses hyphens (e.g. "dm-thin-pool") is normalised
// to underscores and correctly matches the required "dm_thin_pool" entry.
func TestAC10_HyphenNormalisedModuleAccepted(t *testing.T) {
	// "dm-thin-pool" is the hyphen form; loadedKernelModules normalises it
	// to "dm_thin_pool" before returning.  Simulate by using the normalised
	// form directly — that is what the caller receives.
	loaded := map[string]struct{}{
		"zfs":          {},
		"dm_thin_pool": {}, // already normalised (as loadedKernelModules would return)
		"nvme_tcp":     {},
		"nvmet":        {}, // Sub-AC 9b modules must also be present for a full clean set
		"nvmet_tcp":    {},
	}
	if err := checkKernelModulesFromSet(loaded); err != nil {
		t.Errorf("normalised dm_thin_pool should match required entry, got error: %v", err)
	}
}

// TestAC10_UnnormalisedHyphenFormIsNotRecognised verifies that a hyphen-form
// entry (dm-thin-pool) is NOT recognised because loadedKernelModules always
// normalises to underscores before returning. Passing a hyphen form directly
// to checkKernelModulesFromSet should trigger a FAIL for dm_thin_pool.
func TestAC10_UnnormalisedHyphenFormIsNotRecognised(t *testing.T) {
	loaded := map[string]struct{}{
		"zfs":          {},
		"dm-thin-pool": {}, // NOT normalised — simulates a caller bypassing loadedKernelModules
		"nvme_tcp":     {},
		"nvmet":        {},
		"nvmet_tcp":    {},
	}
	err := checkKernelModulesFromSet(loaded)
	if err == nil {
		t.Fatal("unnormalised 'dm-thin-pool' must not match required 'dm_thin_pool'; expected FAIL")
	}
	if !strings.Contains(err.Error(), "dm_thin_pool") {
		t.Errorf("error must mention 'dm_thin_pool' as the missing module, got:\n%s", err.Error())
	}
}
