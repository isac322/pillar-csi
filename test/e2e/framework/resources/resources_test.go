package resources_test

import (
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	pillarv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/resources"
)

// dnsLabelRe matches a valid Kubernetes DNS label.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

func isValidDNSLabel(s string) bool {
	if len(s) < 2 || len(s) > 63 {
		return false
	}
	return dnsLabelRe.MatchString(s)
}

// ── Factory construction ──────────────────────────────────────────────────────

func TestNew_ReturnsBoundFactory(t *testing.T) {
	f := resources.New("E1.1")
	if f == nil {
		t.Fatal("New returned nil")
	}
	if f.TCID() != "E1.1" {
		t.Errorf("TCID() = %q, want %q", f.TCID(), "E1.1")
	}
}

func TestNew_EmptyID(t *testing.T) {
	// Empty TC IDs are allowed at construction time; validation is deferred.
	// This test simply ensures New does not panic.
	f := resources.New("")
	if f == nil {
		t.Fatal("New(\"\") returned nil")
	}
}

// ── Namespace / ObjectPrefix / ResourceName ───────────────────────────────────

func TestNamespace_ValidDNSLabel(t *testing.T) {
	for _, tcID := range []string{"E1.1", "F2.3", "M4.5", "E10.100"} {
		f := resources.New(tcID)
		ns := f.Namespace()
		if !isValidDNSLabel(ns) {
			t.Errorf("[%s] Namespace() = %q: not a valid DNS label", tcID, ns)
		}
	}
}

func TestNamespace_HasPrefix(t *testing.T) {
	for _, tcID := range []string{"E1.1", "F2.3", "M4.5"} {
		f := resources.New(tcID)
		if !strings.HasPrefix(f.Namespace(), "e2e-tc-") {
			t.Errorf("[%s] Namespace() = %q: expected prefix e2e-tc-", tcID, f.Namespace())
		}
	}
}

func TestNamespace_Determinism(t *testing.T) {
	for _, tcID := range []string{"E1.1", "F2.3", "M4.5"} {
		a := resources.New(tcID).Namespace()
		b := resources.New(tcID).Namespace()
		if a != b {
			t.Errorf("[%s] Namespace() not deterministic: %q vs %q", tcID, a, b)
		}
	}
}

func TestNamespace_Uniqueness(t *testing.T) {
	tcIDs := []string{"E1.1", "E1.2", "E1.3", "F2.3", "F2.4", "M4.5"}
	seen := make(map[string]string)
	for _, id := range tcIDs {
		ns := resources.New(id).Namespace()
		if prev, ok := seen[ns]; ok {
			t.Errorf("Namespace collision: %q and %q both produce %q", prev, id, ns)
		}
		seen[ns] = id
	}
}

func TestObjectPrefix_ValidDNSLabel(t *testing.T) {
	for _, tcID := range []string{"E1.1", "F2.3", "M4.5"} {
		f := resources.New(tcID)
		p := f.ObjectPrefix()
		if !isValidDNSLabel(p) {
			t.Errorf("[%s] ObjectPrefix() = %q: not a valid DNS label", tcID, p)
		}
	}
}

func TestResourceName_MaxLength(t *testing.T) {
	suffixes := []string{"pvc", "sc", "pod", "a-very-long-suffix-that-could-push-over-63-chars-xyzabc"}
	for _, tcID := range []string{"E1.1", "E10.100"} {
		f := resources.New(tcID)
		for _, s := range suffixes {
			rn := f.ResourceName(s)
			if len(rn) > 63 {
				t.Errorf("[%s] ResourceName(%q) = %q: len %d > 63", tcID, s, rn, len(rn))
			}
		}
	}
}

func TestResourceName_NoTrailingHyphen(t *testing.T) {
	for _, tcID := range []string{"E1.1", "E10.100"} {
		f := resources.New(tcID)
		rn := f.ResourceName("a-very-long-suffix-that-could-push-over-the-kubernetes-limit-abc")
		if strings.HasSuffix(rn, "-") {
			t.Errorf("[%s] ResourceName has trailing hyphen: %q", tcID, rn)
		}
	}
}

// ── Labels ────────────────────────────────────────────────────────────────────

func TestLabels_ContainsRequiredKeys(t *testing.T) {
	f := resources.New("E1.1")
	labels := f.Labels()

	requiredKeys := []string{
		resources.LabelManagedBy,
		resources.LabelTCID,
		resources.LabelTCPrefix,
	}
	for _, k := range requiredKeys {
		if _, ok := labels[k]; !ok {
			t.Errorf("Labels() missing key %q", k)
		}
	}
}

func TestLabels_ManagedByValue(t *testing.T) {
	f := resources.New("E1.1")
	labels := f.Labels()
	if got := labels[resources.LabelManagedBy]; got != "e2e-fixture" {
		t.Errorf("%s = %q, want %q", resources.LabelManagedBy, got, "e2e-fixture")
	}
}

func TestLabels_TCIDValueIsDNSSafe(t *testing.T) {
	for _, tcID := range []string{"E1.1", "F2.3", "E10.100"} {
		f := resources.New(tcID)
		v := f.Labels()[resources.LabelTCID]
		if !isValidDNSLabel(v) && v != "" {
			t.Errorf("[%s] label %s=%q: not a valid DNS label value", tcID, resources.LabelTCID, v)
		}
	}
}

func TestLabels_TCPrefixMatchesObjectPrefix(t *testing.T) {
	for _, tcID := range []string{"E1.1", "F2.3", "M4.5"} {
		f := resources.New(tcID)
		if got := f.Labels()[resources.LabelTCPrefix]; got != f.ObjectPrefix() {
			t.Errorf("[%s] label %s=%q != ObjectPrefix()=%q", tcID, resources.LabelTCPrefix, got, f.ObjectPrefix())
		}
	}
}

func TestLabels_FreshCopyOnEachCall(t *testing.T) {
	f := resources.New("E1.1")
	m1 := f.Labels()
	m2 := f.Labels()
	m1["mutate"] = "yes"
	if _, ok := m2["mutate"]; ok {
		t.Error("Labels() returned the same map reference; modifications leaked between calls")
	}
}

func TestLabels_Uniqueness_AcrossTCIDs(t *testing.T) {
	ids := []string{"E1.1", "E1.2", "F2.3", "M4.5"}
	prefixes := make([]string, len(ids))
	for i, id := range ids {
		prefixes[i] = resources.New(id).Labels()[resources.LabelTCPrefix]
	}
	for i := range prefixes {
		for j := i + 1; j < len(prefixes); j++ {
			if prefixes[i] == prefixes[j] {
				t.Errorf("label collision: %q and %q both produce prefix %q",
					ids[i], ids[j], prefixes[i])
			}
		}
	}
}

func TestLabelsWithExtra_MergesCorrectly(t *testing.T) {
	f := resources.New("E1.1")
	extra := map[string]string{"custom-key": "custom-val"}
	m := f.LabelsWithExtra(extra)
	if m["custom-key"] != "custom-val" {
		t.Errorf("LabelsWithExtra: extra key not present")
	}
	// Standard keys must still be present.
	if m[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("LabelsWithExtra: standard key lost")
	}
}

func TestLabelsWithExtra_ExtraOverridesStandard(t *testing.T) {
	f := resources.New("E1.1")
	extra := map[string]string{resources.LabelManagedBy: "override"}
	m := f.LabelsWithExtra(extra)
	if m[resources.LabelManagedBy] != "override" {
		t.Errorf("LabelsWithExtra: extra key did not override standard key")
	}
}

// ── Annotations ───────────────────────────────────────────────────────────────

func TestAnnotations_ContainsRequiredKeys(t *testing.T) {
	f := resources.New("E1.1")
	ann := f.Annotations()
	for _, k := range []string{resources.AnnotationTCID, resources.AnnotationTCPrefix} {
		if _, ok := ann[k]; !ok {
			t.Errorf("Annotations() missing key %q", k)
		}
	}
}

func TestAnnotations_TCIDIsRawValue(t *testing.T) {
	// Annotations carry the raw TC ID (including dots), not the URL-encoded or
	// normalized form.
	for _, tcID := range []string{"E1.1", "F2.3", "M4.5"} {
		f := resources.New(tcID)
		if got := f.Annotations()[resources.AnnotationTCID]; got != tcID {
			t.Errorf("[%s] annotation %s=%q, want raw TC ID %q",
				tcID, resources.AnnotationTCID, got, tcID)
		}
	}
}

func TestAnnotations_FreshCopyOnEachCall(t *testing.T) {
	f := resources.New("E1.1")
	a1 := f.Annotations()
	a2 := f.Annotations()
	a1["mutate"] = "yes"
	if _, ok := a2["mutate"]; ok {
		t.Error("Annotations() returned the same map reference; modifications leaked")
	}
}

// ── ObjectMeta ────────────────────────────────────────────────────────────────

func TestObjectMeta_NameFromResourceName(t *testing.T) {
	f := resources.New("E1.1")
	meta := f.ObjectMeta("pvc")
	if meta.Name != f.ResourceName("pvc") {
		t.Errorf("ObjectMeta name = %q, want %q", meta.Name, f.ResourceName("pvc"))
	}
}

func TestObjectMeta_NoNamespace(t *testing.T) {
	f := resources.New("E1.1")
	meta := f.ObjectMeta("sc")
	if meta.Namespace != "" {
		t.Errorf("ObjectMeta.Namespace = %q, want empty (cluster-scoped)", meta.Namespace)
	}
}

func TestObjectMeta_HasStandardLabels(t *testing.T) {
	f := resources.New("E1.1")
	meta := f.ObjectMeta("sc")
	if meta.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("ObjectMeta.Labels missing %s", resources.LabelManagedBy)
	}
}

func TestObjectMeta_HasStandardAnnotations(t *testing.T) {
	f := resources.New("E1.1")
	meta := f.ObjectMeta("sc")
	if meta.Annotations[resources.AnnotationTCID] != "E1.1" {
		t.Errorf("ObjectMeta.Annotations missing %s=E1.1", resources.AnnotationTCID)
	}
}

func TestNamespacedObjectMeta_NamespaceIsSet(t *testing.T) {
	f := resources.New("E1.1")
	meta := f.NamespacedObjectMeta("pvc", "test-ns-abc")
	if meta.Namespace != "test-ns-abc" {
		t.Errorf("NamespacedObjectMeta.Namespace = %q, want %q", meta.Namespace, "test-ns-abc")
	}
}

func TestNamespacedObjectMeta_NameFromResourceName(t *testing.T) {
	f := resources.New("E1.1")
	meta := f.NamespacedObjectMeta("pvc", "test-ns")
	if meta.Name != f.ResourceName("pvc") {
		t.Errorf("NamespacedObjectMeta name = %q, want %q", meta.Name, f.ResourceName("pvc"))
	}
}

// ── PVC factory ───────────────────────────────────────────────────────────────

func TestPVC_BasicFields(t *testing.T) {
	f := resources.New("E1.1")
	pvc := f.PVC("data", "default", "my-sc", "1Gi", corev1.ReadWriteOnce, nil)

	if pvc == nil {
		t.Fatal("PVC() returned nil")
	}
	if pvc.Name != f.ResourceName("data") {
		t.Errorf("pvc.Name = %q, want %q", pvc.Name, f.ResourceName("data"))
	}
	if pvc.Namespace != "default" {
		t.Errorf("pvc.Namespace = %q, want default", pvc.Namespace)
	}
	if *pvc.Spec.StorageClassName != "my-sc" {
		t.Errorf("pvc.Spec.StorageClassName = %q, want my-sc", *pvc.Spec.StorageClassName)
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("pvc.Spec.AccessModes[0] = %v, want ReadWriteOnce", pvc.Spec.AccessModes[0])
	}
	qty := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if qty.String() != "1Gi" {
		t.Errorf("pvc storage request = %q, want 1Gi", qty.String())
	}
}

func TestPVC_HasTCLabels(t *testing.T) {
	f := resources.New("E1.1")
	pvc := f.PVC("data", "default", "sc", "1Gi", corev1.ReadWriteOnce, nil)
	if pvc.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("PVC missing TC label %s", resources.LabelManagedBy)
	}
}

func TestPVC_HasTCAnnotation(t *testing.T) {
	f := resources.New("E1.1")
	pvc := f.PVC("data", "default", "sc", "1Gi", corev1.ReadWriteOnce, nil)
	if pvc.Annotations[resources.AnnotationTCID] != "E1.1" {
		t.Errorf("PVC missing TC annotation %s", resources.AnnotationTCID)
	}
}

func TestPVC_VolumeMode_Default(t *testing.T) {
	f := resources.New("E1.1")
	pvc := f.PVC("data", "default", "sc", "1Gi", corev1.ReadWriteOnce, nil)
	// When opts is nil, VolumeMode is not set (nil pointer → k8s defaults to Filesystem).
	if pvc.Spec.VolumeMode != nil {
		t.Errorf("pvc.Spec.VolumeMode should be nil by default, got %v", *pvc.Spec.VolumeMode)
	}
}

func TestPVC_VolumeMode_Block(t *testing.T) {
	f := resources.New("E1.1")
	mode := corev1.PersistentVolumeBlock
	pvc := f.PVC("data", "default", "sc", "1Gi", corev1.ReadWriteOnce, &resources.PVCOptions{
		VolumeMode: &mode,
	})
	if pvc.Spec.VolumeMode == nil || *pvc.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("pvc.Spec.VolumeMode = %v, want Block", pvc.Spec.VolumeMode)
	}
}

func TestPVC_ExtraLabels(t *testing.T) {
	f := resources.New("E1.1")
	pvc := f.PVC("data", "default", "sc", "1Gi", corev1.ReadWriteOnce, &resources.PVCOptions{
		ExtraLabels: map[string]string{"app": "test"},
	})
	if pvc.Labels["app"] != "test" {
		t.Errorf("PVC extra label not present")
	}
}

// ── StorageClass factory ──────────────────────────────────────────────────────

func TestStorageClass_BasicFields(t *testing.T) {
	f := resources.New("E1.1")
	params := map[string]string{"poolRef": "my-pool", "backendType": "lvm-lv"}
	sc := f.StorageClass("sc", "pillar-csi.bhyoo.com", params, nil)

	if sc == nil {
		t.Fatal("StorageClass() returned nil")
	}
	if sc.Provisioner != "pillar-csi.bhyoo.com" {
		t.Errorf("sc.Provisioner = %q, want pillar-csi.bhyoo.com", sc.Provisioner)
	}
	if sc.Parameters["poolRef"] != "my-pool" {
		t.Errorf("sc.Parameters[poolRef] = %q, want my-pool", sc.Parameters["poolRef"])
	}
	if sc.Name != f.ResourceName("sc") {
		t.Errorf("sc.Name = %q, want %q", sc.Name, f.ResourceName("sc"))
	}
}

func TestStorageClass_DefaultReclaimPolicy(t *testing.T) {
	f := resources.New("E1.1")
	sc := f.StorageClass("sc", "pillar-csi.bhyoo.com", nil, nil)
	if *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("default reclaim policy = %v, want Delete", *sc.ReclaimPolicy)
	}
}

func TestStorageClass_DefaultAllowVolumeExpansion(t *testing.T) {
	f := resources.New("E1.1")
	sc := f.StorageClass("sc", "pillar-csi.bhyoo.com", nil, nil)
	if !*sc.AllowVolumeExpansion {
		t.Errorf("AllowVolumeExpansion default should be true")
	}
}

func TestStorageClass_DefaultVolumeBindingMode(t *testing.T) {
	f := resources.New("E1.1")
	sc := f.StorageClass("sc", "pillar-csi.bhyoo.com", nil, nil)
	if *sc.VolumeBindingMode != storagev1.VolumeBindingImmediate {
		t.Errorf("VolumeBindingMode default = %v, want Immediate", *sc.VolumeBindingMode)
	}
}

func TestStorageClass_CustomOptions(t *testing.T) {
	f := resources.New("E1.1")
	retain := corev1.PersistentVolumeReclaimRetain
	wffc := storagev1.VolumeBindingWaitForFirstConsumer
	noExp := false
	sc := f.StorageClass("sc", "pillar-csi.bhyoo.com", nil, &resources.StorageClassOptions{
		ReclaimPolicy:        &retain,
		VolumeBindingMode:    &wffc,
		AllowVolumeExpansion: &noExp,
		MountOptions:         []string{"ro"},
	})
	if *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("reclaim policy = %v, want Retain", *sc.ReclaimPolicy)
	}
	if *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		t.Errorf("binding mode = %v, want WaitForFirstConsumer", *sc.VolumeBindingMode)
	}
	if *sc.AllowVolumeExpansion {
		t.Errorf("AllowVolumeExpansion should be false")
	}
	if len(sc.MountOptions) != 1 || sc.MountOptions[0] != "ro" {
		t.Errorf("MountOptions = %v, want [ro]", sc.MountOptions)
	}
}

func TestStorageClass_HasTCLabels(t *testing.T) {
	f := resources.New("E1.1")
	sc := f.StorageClass("sc", "pillar-csi.bhyoo.com", nil, nil)
	if sc.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("StorageClass missing TC label")
	}
}

// ── Pod factory ───────────────────────────────────────────────────────────────

func TestPod_BasicFields(t *testing.T) {
	f := resources.New("E1.1")
	pod := f.Pod("pod", "default", "my-pvc", "/data", "busybox:1.36",
		[]string{"sh", "-c", "echo ok"}, nil)

	if pod == nil {
		t.Fatal("Pod() returned nil")
	}
	if pod.Namespace != "default" {
		t.Errorf("pod.Namespace = %q, want default", pod.Namespace)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod.Spec.RestartPolicy = %v, want Never", pod.Spec.RestartPolicy)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("pod has %d containers, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != "busybox:1.36" {
		t.Errorf("container image = %q, want busybox:1.36", c.Image)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/data" {
		t.Errorf("container mount path = %v, want /data", c.VolumeMounts)
	}
}

func TestPod_PVCVolumeRef(t *testing.T) {
	f := resources.New("E1.1")
	pod := f.Pod("pod", "default", "my-pvc", "/data", "busybox:1.36",
		[]string{"sh", "-c", "echo ok"}, nil)
	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("pod has %d volumes, want 1", len(pod.Spec.Volumes))
	}
	vol := pod.Spec.Volumes[0]
	if vol.PersistentVolumeClaim.ClaimName != "my-pvc" {
		t.Errorf("volume PVC claimName = %q, want my-pvc", vol.PersistentVolumeClaim.ClaimName)
	}
}

func TestPod_NodeName(t *testing.T) {
	f := resources.New("E1.1")
	pod := f.Pod("pod", "default", "pvc", "/mnt", "busybox:1.36",
		nil, &resources.PodOptions{NodeName: "worker-0"})
	if pod.Spec.NodeName != "worker-0" {
		t.Errorf("pod.Spec.NodeName = %q, want worker-0", pod.Spec.NodeName)
	}
}

func TestPod_HasTCLabels(t *testing.T) {
	f := resources.New("E1.1")
	pod := f.Pod("pod", "default", "pvc", "/mnt", "busybox:1.36", nil, nil)
	if pod.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("Pod missing TC label")
	}
}

// ── PillarTarget factory ──────────────────────────────────────────────────────

func TestPillarTarget_BasicFields(t *testing.T) {
	f := resources.New("E1.1")
	spec := pillarv1alpha1.PillarTargetSpec{
		External: &pillarv1alpha1.ExternalSpec{Address: "10.0.0.1", Port: 9500},
	}
	pt := f.PillarTarget("target", spec, nil)

	if pt == nil {
		t.Fatal("PillarTarget() returned nil")
	}
	if pt.Namespace != "" {
		t.Errorf("PillarTarget.Namespace = %q, want empty (cluster-scoped)", pt.Namespace)
	}
	if pt.Spec.External == nil || pt.Spec.External.Address != "10.0.0.1" {
		t.Errorf("PillarTarget spec not preserved: %+v", pt.Spec)
	}
	if pt.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("PillarTarget missing TC label")
	}
}

func TestPillarTarget_NameFromResourceName(t *testing.T) {
	f := resources.New("E1.1")
	pt := f.PillarTarget("target", pillarv1alpha1.PillarTargetSpec{}, nil)
	if pt.Name != f.ResourceName("target") {
		t.Errorf("PillarTarget.Name = %q, want %q", pt.Name, f.ResourceName("target"))
	}
}

// ── PillarPool factory ────────────────────────────────────────────────────────

func TestPillarPool_BasicFields(t *testing.T) {
	f := resources.New("E1.1")
	backend := pillarv1alpha1.BackendSpec{
		Type: pillarv1alpha1.BackendTypeLVMLV,
		LVM:  &pillarv1alpha1.LVMBackendConfig{VolumeGroup: "data-vg"},
	}
	pp := f.PillarPool("pool", "my-target", backend, nil)

	if pp == nil {
		t.Fatal("PillarPool() returned nil")
	}
	if pp.Spec.TargetRef != "my-target" {
		t.Errorf("PillarPool TargetRef = %q, want my-target", pp.Spec.TargetRef)
	}
	if pp.Spec.Backend.Type != pillarv1alpha1.BackendTypeLVMLV {
		t.Errorf("PillarPool backend type = %q, want lvm-lv", pp.Spec.Backend.Type)
	}
	if pp.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("PillarPool missing TC label")
	}
}

// ── PillarProtocol factory ────────────────────────────────────────────────────

func TestPillarProtocol_BasicFields(t *testing.T) {
	f := resources.New("E1.1")
	spec := pillarv1alpha1.PillarProtocolSpec{
		Type:      pillarv1alpha1.ProtocolTypeNVMeOFTCP,
		NVMeOFTCP: &pillarv1alpha1.NVMeOFTCPConfig{Port: 4420},
	}
	ppr := f.PillarProtocol("proto", spec, nil)

	if ppr == nil {
		t.Fatal("PillarProtocol() returned nil")
	}
	if ppr.Spec.Type != pillarv1alpha1.ProtocolTypeNVMeOFTCP {
		t.Errorf("PillarProtocol type = %q, want nvmeof-tcp", ppr.Spec.Type)
	}
	if ppr.Namespace != "" {
		t.Errorf("PillarProtocol.Namespace should be empty (cluster-scoped)")
	}
	if ppr.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("PillarProtocol missing TC label")
	}
}

// ── PillarBinding factory ─────────────────────────────────────────────────────

func TestPillarBinding_BasicFields(t *testing.T) {
	f := resources.New("E1.1")
	scTemplate := pillarv1alpha1.StorageClassTemplate{
		ReclaimPolicy:     pillarv1alpha1.ReclaimPolicyDelete,
		VolumeBindingMode: pillarv1alpha1.VolumeBindingImmediate,
	}
	pb := f.PillarBinding("binding", "my-pool", "my-proto", scTemplate, nil, nil)

	if pb == nil {
		t.Fatal("PillarBinding() returned nil")
	}
	if pb.Spec.PoolRef != "my-pool" {
		t.Errorf("PillarBinding PoolRef = %q, want my-pool", pb.Spec.PoolRef)
	}
	if pb.Spec.ProtocolRef != "my-proto" {
		t.Errorf("PillarBinding ProtocolRef = %q, want my-proto", pb.Spec.ProtocolRef)
	}
	if pb.Labels[resources.LabelManagedBy] != "e2e-fixture" {
		t.Errorf("PillarBinding missing TC label")
	}
}

// ── LabelSelector / ListOptions ───────────────────────────────────────────────

func TestLabelSelector_MatchLabels(t *testing.T) {
	f := resources.New("E1.1")
	sel := f.LabelSelector()
	if sel == nil {
		t.Fatal("LabelSelector() returned nil")
	}
	if sel.MatchLabels[resources.LabelTCPrefix] != f.ObjectPrefix() {
		t.Errorf("LabelSelector.MatchLabels[%s] = %q, want %q",
			resources.LabelTCPrefix, sel.MatchLabels[resources.LabelTCPrefix], f.ObjectPrefix())
	}
}

func TestListOptions_LabelSelectorString(t *testing.T) {
	f := resources.New("E1.1")
	lo := f.ListOptions()
	expected := resources.LabelTCPrefix + "=" + f.ObjectPrefix()
	if lo.LabelSelector != expected {
		t.Errorf("ListOptions.LabelSelector = %q, want %q", lo.LabelSelector, expected)
	}
}

// ── Cross-TC isolation ────────────────────────────────────────────────────────

// Two different TC IDs must produce non-overlapping names, labels, and
// namespace names so that they cannot interfere when running concurrently.
func TestCrossTCIsolation(t *testing.T) {
	pairs := [][2]string{
		{"E1.1", "E1.2"},
		{"F2.3", "F2.4"},
		// Normalization-collision candidates (dot vs hyphen; hash distinguishes them).
		{"E1-1", "E1.1"},
	}
	for _, pair := range pairs {
		a := resources.New(pair[0])
		b := resources.New(pair[1])

		if a.Namespace() == b.Namespace() {
			t.Errorf("Namespace(%q)==Namespace(%q)==%q: collision", pair[0], pair[1], a.Namespace())
		}
		if a.ObjectPrefix() == b.ObjectPrefix() {
			t.Errorf("ObjectPrefix(%q)==ObjectPrefix(%q)==%q: collision", pair[0], pair[1], a.ObjectPrefix())
		}
		if a.ResourceName("pvc") == b.ResourceName("pvc") {
			t.Errorf("ResourceName(%q,pvc)==ResourceName(%q,pvc): collision", pair[0], pair[1])
		}
		if a.Labels()[resources.LabelTCPrefix] == b.Labels()[resources.LabelTCPrefix] {
			t.Errorf("label tc-prefix collision between %q and %q", pair[0], pair[1])
		}
	}
}

// ── Determinism across Factory instances ──────────────────────────────────────

// Creating two independent Factory instances with the same TC ID must produce
// identical names, labels, and annotations.
func TestDeterminism_TwoInstances(t *testing.T) {
	tcIDs := []string{"E1.1", "F2.3", "M4.5", "E10.100"}
	for _, tcID := range tcIDs {
		a := resources.New(tcID)
		b := resources.New(tcID)

		if a.Namespace() != b.Namespace() {
			t.Errorf("[%s] Namespace() not deterministic: %q vs %q", tcID, a.Namespace(), b.Namespace())
		}
		if a.ObjectPrefix() != b.ObjectPrefix() {
			t.Errorf("[%s] ObjectPrefix() not deterministic", tcID)
		}
		if a.ResourceName("pvc") != b.ResourceName("pvc") {
			t.Errorf("[%s] ResourceName('pvc') not deterministic", tcID)
		}
		if a.Labels()[resources.LabelTCPrefix] != b.Labels()[resources.LabelTCPrefix] {
			t.Errorf("[%s] Labels()[%s] not deterministic", tcID, resources.LabelTCPrefix)
		}
	}
}
