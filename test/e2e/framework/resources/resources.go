// Package resources provides a per-TC Kubernetes resource factory for the
// pillar-csi E2E test suite.
//
// # Design
//
// Every documented test case (TC) must be fully isolated: two TCs that run
// concurrently must never share any mutable Kubernetes state. The [Factory]
// type achieves this by binding a single TC ID to a set of deterministic,
// collision-free resource identifiers (names, labels, annotations). Every
// Kubernetes object created through the same Factory carries the same TC
// label, making it trivial to audit, filter, and clean up per-TC artefacts.
//
// # Naming invariants (see also framework/names)
//
//   - All names are valid Kubernetes DNS labels (≤ 63 characters, ^[a-z0-9][a-z0-9-]*[a-z0-9]$).
//   - Names are deterministic: calling the same factory method with the same
//     arguments always produces the same name, even across process restarts.
//   - Names are unique across TC IDs: two different TC IDs never produce the
//     same name for the same suffix, even when their raw IDs look similar after
//     normalization (e.g. "E1-1" vs "E1.1"), because an 8-hex-char SHA-256
//     digest of the raw TC ID is embedded in every name.
//
// # Label / annotation keys
//
// The following well-known keys are attached to every object created through
// the Factory:
//
//   - Label   "pillar-csi.bhyoo.com/managed-by"  = "e2e-fixture"
//   - Label   "pillar-csi.bhyoo.com/tc-id"       = <URL-encoded TC ID>
//   - Label   "pillar-csi.bhyoo.com/tc-prefix"   = ObjectPrefix(tcID)
//   - Annotation "pillar-csi.bhyoo.com/tc-id"    = <raw TC ID> (unencoded)
//   - Annotation "pillar-csi.bhyoo.com/tc-prefix" = ObjectPrefix(tcID)
//
// The TC ID is stored in both a label (DNS-safe value for kubectl -l selectors)
// and an annotation (raw value for human readability).
//
// # Resource factory methods
//
// Each factory method returns a fully-configured Kubernetes API object
// (pointer) with pre-populated ObjectMeta (name, namespace, labels,
// annotations). The caller is responsible for submitting the object to the
// Kubernetes API and cleaning it up after the test.
//
// # Usage example
//
//	f := resources.New("E1.1")
//
//	pvc := f.PVC("data", ns.Name(), storageClassName, "1Gi", corev1.ReadWriteOnce)
//	_, err := clientset.CoreV1().PersistentVolumeClaims(ns.Name()).Create(ctx, pvc, metav1.CreateOptions{})
//
//	sc := f.StorageClass("sc", "pillar-csi.bhyoo.com", map[string]string{"storeRef": "my-pool"})
//	_, err = clientset.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
//
//	pt := f.PillarAgent("target", v1alpha1.PillarAgentSpec{External: &v1alpha1.ExternalSpec{Address: "10.0.0.1", Port: 9500}})
//	_, err = crdClient.PillarAgents().Create(ctx, pt, metav1.CreateOptions{})
package resources

import (
	"net/url"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/names"
)

// ── Well-known metadata keys ──────────────────────────────────────────────────

const (
	// LabelManagedBy is set to "e2e-fixture" on every object created through
	// the factory, enabling bulk cleanup of all E2E artefacts with kubectl.
	LabelManagedBy = "pillar-csi.bhyoo.com/managed-by"

	// LabelTCID carries the TC ID in DNS-label-safe form (dots replaced by
	// hyphens, length-bounded).  Use this label for kubectl -l selectors.
	LabelTCID = "pillar-csi.bhyoo.com/tc-id"

	// LabelTCPrefix carries the ObjectPrefix derived from the TC ID.  It is a
	// stable short key that also uniquely identifies all objects belonging to
	// one TC, since the prefix embeds an 8-char hash of the raw TC ID.
	LabelTCPrefix = "pillar-csi.bhyoo.com/tc-prefix"

	// AnnotationTCID carries the raw (unencoded) TC ID, preserved for human
	// readability in kubectl describe / JSON output.
	AnnotationTCID = "pillar-csi.bhyoo.com/tc-id"

	// AnnotationTCPrefix mirrors LabelTCPrefix in the annotations map for
	// cross-reference (annotations allow arbitrary string values, including
	// longer IDs than DNS labels permit).
	AnnotationTCPrefix = "pillar-csi.bhyoo.com/tc-prefix"

	// managedByValue is the constant value for LabelManagedBy.
	managedByValue = "e2e-fixture"
)

// ── Factory ───────────────────────────────────────────────────────────────────

// Factory is a per-TC Kubernetes resource identifier and object factory.
// It is bound to a single TC ID at construction time; all names, labels, and
// annotations it produces are uniquely derived from that ID.
//
// Factory values are safe to copy and use concurrently — all methods are
// pure functions with no mutable state.
type Factory struct {
	tcID    string // raw (un-normalized) TC ID
	prefix  string // cached ObjectPrefix(tcID)
	ns      string // cached Namespace(tcID)
	tcLabel string // DNS-safe label value for LabelTCID
}

// New creates a Factory bound to tcID.
//
// tcID must not be empty; passing an empty string will produce identifiers
// that are indistinguishable from other empty-ID factories and will fail
// Kubernetes DNS-label validation. The factory does not validate tcID
// (validation is deferred to names.Namespace / names.ObjectPrefix).
func New(tcID string) *Factory {
	prefix := names.ObjectPrefix(tcID)
	ns := names.Namespace(tcID)
	return &Factory{
		tcID:    tcID,
		prefix:  prefix,
		ns:      ns,
		tcLabel: safeLabelValue(tcID),
	}
}

// ── Identity accessors ────────────────────────────────────────────────────────

// TCID returns the raw TC ID this factory was constructed with.
func (f *Factory) TCID() string { return f.tcID }

// Namespace returns the deterministic Kubernetes namespace name for this TC.
// Format: e2e-tc-{slug}-{hash8} (≤ 63 chars, valid DNS label).
//
// This is the same value as names.Namespace(tcID) — exposed here so callers
// do not need to import the names package separately.
func (f *Factory) Namespace() string { return f.ns }

// ObjectPrefix returns the deterministic object name prefix for this TC.
// Format: tc-{slug}-{hash8} (≤ 63 chars, valid DNS label).
//
// This is the same value as names.ObjectPrefix(tcID).
func (f *Factory) ObjectPrefix() string { return f.prefix }

// ResourceName returns a complete Kubernetes object name for this TC with the
// given suffix. The result is ObjectPrefix() + "-" + suffix, truncated to 63
// characters. A trailing hyphen produced by truncation is removed.
//
// This is the same value as names.ResourceName(tcID, suffix).
func (f *Factory) ResourceName(suffix string) string {
	return names.ResourceName(f.tcID, suffix)
}

// ── Labels / Annotations ──────────────────────────────────────────────────────

// Labels returns the standard set of Kubernetes labels for this TC.
//
// The returned map always contains:
//
//	pillar-csi.bhyoo.com/managed-by  = "e2e-fixture"
//	pillar-csi.bhyoo.com/tc-id       = <DNS-safe TC ID slug>
//	pillar-csi.bhyoo.com/tc-prefix   = ObjectPrefix()
//
// Callers may add extra entries to the returned map; it is a fresh copy on
// every call, so modifications do not affect subsequent calls.
func (f *Factory) Labels() map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelTCID:      f.tcLabel,
		LabelTCPrefix:  f.prefix,
	}
}

// LabelsWithExtra returns the standard labels merged with the caller-supplied
// extra labels.  extra keys override the standard labels when they collide.
// The returned map is a fresh copy on every call.
func (f *Factory) LabelsWithExtra(extra map[string]string) map[string]string {
	m := f.Labels()
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// Annotations returns the standard set of Kubernetes annotations for this TC.
//
// The returned map always contains:
//
//	pillar-csi.bhyoo.com/tc-id      = <raw TC ID> (unencoded, for human readability)
//	pillar-csi.bhyoo.com/tc-prefix  = ObjectPrefix()
//
// The raw TC ID is stored in the annotations (not the labels) because
// annotations accept arbitrary values while label values must be valid
// Kubernetes label syntax.
func (f *Factory) Annotations() map[string]string {
	return map[string]string{
		AnnotationTCID:     f.tcID,
		AnnotationTCPrefix: f.prefix,
	}
}

// AnnotationsWithExtra returns the standard annotations merged with the
// caller-supplied extra annotations.  extra keys override the standard
// annotations when they collide.  The returned map is a fresh copy.
func (f *Factory) AnnotationsWithExtra(extra map[string]string) map[string]string {
	m := f.Annotations()
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// ── ObjectMeta factories ──────────────────────────────────────────────────────

// ObjectMeta returns a metav1.ObjectMeta suitable for cluster-scoped
// Kubernetes objects (StorageClass, PersistentVolume, PillarAgent, …).
//
//   - Name = ResourceName(suffix)
//   - Namespace = "" (cluster-scoped)
//   - Labels = Labels()
//   - Annotations = Annotations()
func (f *Factory) ObjectMeta(suffix string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        f.ResourceName(suffix),
		Labels:      f.Labels(),
		Annotations: f.Annotations(),
	}
}

// NamespacedObjectMeta returns a metav1.ObjectMeta suitable for
// namespace-scoped objects (PVC, Pod, Secret, …).
//
//   - Name      = ResourceName(suffix)
//   - Namespace = namespace (caller-provided; typically from namespace.Fixture.Name())
//   - Labels    = Labels()
//   - Annotations = Annotations()
func (f *Factory) NamespacedObjectMeta(suffix, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        f.ResourceName(suffix),
		Namespace:   namespace,
		Labels:      f.Labels(),
		Annotations: f.Annotations(),
	}
}

// ── Kubernetes core resource factories ───────────────────────────────────────

// PVCOptions holds optional configuration for PVC construction.
// Use the zero value to accept all defaults.
type PVCOptions struct {
	// VolumeMode selects block (Block) or filesystem (Filesystem) mode.
	// Defaults to corev1.PersistentVolumeFilesystem when zero.
	VolumeMode *corev1.PersistentVolumeMode

	// ExtraLabels are merged on top of the standard TC labels.
	ExtraLabels map[string]string

	// ExtraAnnotations are merged on top of the standard TC annotations.
	ExtraAnnotations map[string]string
}

// PVC returns a *corev1.PersistentVolumeClaim with TC-scoped name, labels,
// and annotations.
//
// Parameters:
//   - suffix       — resource name suffix (e.g. "data", "pvc-0")
//   - namespace    — Kubernetes namespace (typically from namespace.Fixture.Name())
//   - storageClass — StorageClass name to reference in spec.storageClassName
//   - storageRequest — requested storage (e.g. "1Gi") — must be parseable by resource.MustParse
//   - accessMode   — access mode (e.g. corev1.ReadWriteOnce)
//   - opts         — optional additional configuration (nil is safe)
func (f *Factory) PVC(
	suffix, namespace, storageClass, storageRequest string,
	accessMode corev1.PersistentVolumeAccessMode,
	opts *PVCOptions,
) *corev1.PersistentVolumeClaim {
	meta := f.NamespacedObjectMeta(suffix, namespace)
	if opts != nil {
		meta.Labels = mergeLabels(meta.Labels, opts.ExtraLabels)
		meta.Annotations = mergeAnnotations(meta.Annotations, opts.ExtraAnnotations)
	}

	qty := resource.MustParse(storageRequest)
	sc := storageClass

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: meta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: qty,
				},
			},
			StorageClassName: &sc,
		},
	}

	if opts != nil && opts.VolumeMode != nil {
		pvc.Spec.VolumeMode = opts.VolumeMode
	}

	return pvc
}

// StorageClassOptions holds optional configuration for StorageClass construction.
type StorageClassOptions struct {
	// ReclaimPolicy defaults to corev1.PersistentVolumeReclaimDelete.
	ReclaimPolicy *corev1.PersistentVolumeReclaimPolicy

	// VolumeBindingMode defaults to storagev1.VolumeBindingImmediate.
	VolumeBindingMode *storagev1.VolumeBindingMode

	// AllowVolumeExpansion defaults to true.
	AllowVolumeExpansion *bool

	// MountOptions are passed through verbatim.
	MountOptions []string

	// ExtraLabels are merged on top of the standard TC labels.
	ExtraLabels map[string]string

	// ExtraAnnotations are merged on top of the standard TC annotations.
	ExtraAnnotations map[string]string
}

// StorageClass returns a *storagev1.StorageClass with TC-scoped name, labels,
// and annotations.
//
// Parameters:
//   - suffix     — resource name suffix (e.g. "sc", "sc-lvm")
//   - provisioner — CSI driver name (e.g. "pillar-csi.bhyoo.com")
//   - parameters  — StorageClass parameters map (e.g. storeRef, backendType)
//   - opts        — optional additional configuration (nil is safe)
func (f *Factory) StorageClass(
	suffix, provisioner string,
	parameters map[string]string,
	opts *StorageClassOptions,
) *storagev1.StorageClass {
	meta := f.ObjectMeta(suffix)
	if opts != nil {
		meta.Labels = mergeLabels(meta.Labels, opts.ExtraLabels)
		meta.Annotations = mergeAnnotations(meta.Annotations, opts.ExtraAnnotations)
	}

	// Defaults.
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	bindingMode := storagev1.VolumeBindingImmediate
	allowExpansion := true

	if opts != nil {
		if opts.ReclaimPolicy != nil {
			reclaimPolicy = *opts.ReclaimPolicy
		}
		if opts.VolumeBindingMode != nil {
			bindingMode = *opts.VolumeBindingMode
		}
		if opts.AllowVolumeExpansion != nil {
			allowExpansion = *opts.AllowVolumeExpansion
		}
	}

	sc := &storagev1.StorageClass{
		ObjectMeta:           meta,
		Provisioner:          provisioner,
		Parameters:           parameters,
		ReclaimPolicy:        &reclaimPolicy,
		VolumeBindingMode:    &bindingMode,
		AllowVolumeExpansion: &allowExpansion,
	}

	if opts != nil && len(opts.MountOptions) > 0 {
		sc.MountOptions = opts.MountOptions
	}

	return sc
}

// PodOptions holds optional configuration for Pod construction.
type PodOptions struct {
	// NodeName pins the Pod to a specific node.
	NodeName string

	// NodeSelector is an optional node selector map.
	NodeSelector map[string]string

	// ExtraLabels are merged on top of the standard TC labels.
	ExtraLabels map[string]string

	// ExtraAnnotations are merged on top of the standard TC annotations.
	ExtraAnnotations map[string]string

	// RestartPolicy defaults to corev1.RestartPolicyNever.
	RestartPolicy corev1.RestartPolicy
}

// Pod returns a *corev1.Pod with a single container that mounts the PVC
// identified by pvcName at mountPath.  It is designed for simple mount
// verification tests (write-a-file, read-it-back).
//
// Parameters:
//   - suffix    — resource name suffix (e.g. "pod", "writer")
//   - namespace — Kubernetes namespace
//   - pvcName   — PVC to mount (must exist in the same namespace)
//   - mountPath — container mount path (e.g. "/data")
//   - image     — container image (e.g. "busybox:1.36")
//   - command   — container command (e.g. []string{"sh", "-c", "echo ok > /data/out"})
//   - opts      — optional additional configuration (nil is safe)
func (f *Factory) Pod(
	suffix, namespace, pvcName, mountPath, image string,
	command []string,
	opts *PodOptions,
) *corev1.Pod {
	meta := f.NamespacedObjectMeta(suffix, namespace)
	if opts != nil {
		meta.Labels = mergeLabels(meta.Labels, opts.ExtraLabels)
		meta.Annotations = mergeAnnotations(meta.Annotations, opts.ExtraAnnotations)
	}

	restartPolicy := corev1.RestartPolicyNever
	if opts != nil && opts.RestartPolicy != "" {
		restartPolicy = opts.RestartPolicy
	}

	pod := &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			RestartPolicy: restartPolicy,
			Containers: []corev1.Container{
				{
					Name:    "test",
					Image:   image,
					Command: command,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: mountPath,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}

	if opts != nil {
		if opts.NodeName != "" {
			pod.Spec.NodeName = opts.NodeName
		}
		if len(opts.NodeSelector) > 0 {
			pod.Spec.NodeSelector = opts.NodeSelector
		}
	}

	return pod
}

// ── pillar-csi CRD factories ──────────────────────────────────────────────────

// PillarAgentOptions holds optional extra metadata for PillarAgent.
type PillarAgentOptions struct {
	ExtraLabels      map[string]string
	ExtraAnnotations map[string]string
}

// PillarAgent returns a *pillarv1alpha1.PillarAgent with TC-scoped name,
// labels, and annotations.
//
// Parameters:
//   - suffix — resource name suffix (e.g. "target", "agent-0")
//   - spec   — PillarAgentSpec; caller populates NodeRef or External
//   - opts   — optional extra metadata (nil is safe)
func (f *Factory) PillarAgent(
	suffix string,
	spec pillarv1alpha1.PillarAgentSpec,
	opts *PillarAgentOptions,
) *pillarv1alpha1.PillarAgent {
	meta := f.ObjectMeta(suffix)
	if opts != nil {
		meta.Labels = mergeLabels(meta.Labels, opts.ExtraLabels)
		meta.Annotations = mergeAnnotations(meta.Annotations, opts.ExtraAnnotations)
	}
	return &pillarv1alpha1.PillarAgent{
		ObjectMeta: meta,
		Spec:       spec,
	}
}

// PillarStoreOptions holds optional extra metadata for PillarStore.
type PillarStoreOptions struct {
	ExtraLabels      map[string]string
	ExtraAnnotations map[string]string
}

// PillarStore returns a *pillarv1alpha1.PillarStore with TC-scoped name, labels,
// and annotations.
//
// Parameters:
//   - suffix    — resource name suffix (e.g. "pool", "zfs-pool")
//   - agentRef — name of the PillarAgent this pool references
//   - backend   — BackendSpec (type + ZFS or LVM config)
//   - opts      — optional extra metadata (nil is safe)
func (f *Factory) PillarStore(
	suffix, agentRef string,
	backend pillarv1alpha1.BackendSpec,
	opts *PillarStoreOptions,
) *pillarv1alpha1.PillarStore {
	meta := f.ObjectMeta(suffix)
	if opts != nil {
		meta.Labels = mergeLabels(meta.Labels, opts.ExtraLabels)
		meta.Annotations = mergeAnnotations(meta.Annotations, opts.ExtraAnnotations)
	}
	return &pillarv1alpha1.PillarStore{
		ObjectMeta: meta,
		Spec: pillarv1alpha1.PillarStoreSpec{
			AgentRef: agentRef,
			Backend:  backend,
		},
	}
}

// PillarProtocolOptions holds optional extra metadata for PillarProtocol.
type PillarProtocolOptions struct {
	ExtraLabels      map[string]string
	ExtraAnnotations map[string]string
}

// PillarProtocol returns a *pillarv1alpha1.PillarProtocol with TC-scoped name,
// labels, and annotations.
//
// PillarProtocol is protocol-level and does not reference a PillarAgent
// directly; the target association happens through PillarStorageClass → PillarStore →
// PillarAgent.  Callers supply the full spec (including Type and the
// matching config field: NVMeOFTCP, ISCSI, NFS, or SMB).
//
// Parameters:
//   - suffix — resource name suffix (e.g. "proto", "nvmeof", "iscsi")
//   - spec   — PillarProtocolSpec (protocol type + config)
//   - opts   — optional extra metadata (nil is safe)
func (f *Factory) PillarProtocol(
	suffix string,
	spec pillarv1alpha1.PillarProtocolSpec,
	opts *PillarProtocolOptions,
) *pillarv1alpha1.PillarProtocol {
	meta := f.ObjectMeta(suffix)
	if opts != nil {
		meta.Labels = mergeLabels(meta.Labels, opts.ExtraLabels)
		meta.Annotations = mergeAnnotations(meta.Annotations, opts.ExtraAnnotations)
	}
	return &pillarv1alpha1.PillarProtocol{
		ObjectMeta: meta,
		Spec:       spec,
	}
}

// PillarStorageClassOptions holds optional extra metadata for PillarStorageClass.
type PillarStorageClassOptions struct {
	ExtraLabels      map[string]string
	ExtraAnnotations map[string]string
}

// PillarStorageClass returns a *pillarv1alpha1.PillarStorageClass with TC-scoped name,
// labels, and annotations.
//
// Parameters:
//   - suffix      — resource name suffix (e.g. "binding", "bind-0")
//   - storeRef     — name of the PillarStore to bind
//   - protocolRef — name of the PillarProtocol to bind
//   - scTemplate  — StorageClassTemplate (name, reclaimPolicy, bindingMode)
//   - overrides   — optional BackendOverrides/ProtocolOverrides (nil is fine)
//   - opts        — optional extra metadata (nil is safe)
func (f *Factory) PillarStorageClass(
	suffix, storeRef, protocolRef string,
	scTemplate pillarv1alpha1.StorageClassTemplate,
	overrides *pillarv1alpha1.StorageClassOverrides,
	opts *PillarStorageClassOptions,
) *pillarv1alpha1.PillarStorageClass {
	meta := f.ObjectMeta(suffix)
	if opts != nil {
		meta.Labels = mergeLabels(meta.Labels, opts.ExtraLabels)
		meta.Annotations = mergeAnnotations(meta.Annotations, opts.ExtraAnnotations)
	}
	return &pillarv1alpha1.PillarStorageClass{
		ObjectMeta: meta,
		Spec: pillarv1alpha1.PillarStorageClassSpec{
			StoreRef:     storeRef,
			ProtocolRef:  protocolRef,
			StorageClass: scTemplate,
			Overrides:    overrides,
		},
	}
}

// ── LabelSelector helper ──────────────────────────────────────────────────────

// LabelSelector returns a *metav1.LabelSelector that selects all objects
// belonging to this TC (those with LabelTCPrefix == ObjectPrefix()).
//
// This is suitable for use in client-go List calls, Informer factories, and
// Watch calls to filter results to this TC's artefacts only.
func (f *Factory) LabelSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: map[string]string{
			LabelTCPrefix: f.prefix,
		},
	}
}

// ListOptions returns a metav1.ListOptions that scopes List / Watch calls to
// objects belonging to this TC.
func (f *Factory) ListOptions() metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: LabelTCPrefix + "=" + f.prefix,
	}
}

// ── internal helpers ──────────────────────────────────────────────────────────

// safeLabelValue converts a TC ID to a Kubernetes-safe label value.
//
// Kubernetes label values must be empty or:
//
//	^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$   (max 63 chars)
//
// We URL-query-escape the TC ID so it remains human-readable (dots become %2E,
// slashes become %2F, etc.) but then further restrict to the safe character
// set by replacing every disallowed character with a hyphen, which is the same
// strategy used by names.normalize.
//
// For the majority of TC IDs ("E1.1", "F2.3", "M4.5") the dot is the only
// character requiring substitution; URL-encoding dots to %2E would embed
// percent signs that are also disallowed. Therefore we take the simpler path
// of reusing the names package's normalization: lowercase + non-alnum → hyphen.
func safeLabelValue(tcID string) string {
	// URL-percent-encode for reference (not used directly, but informative).
	_ = url.QueryEscape(tcID)
	// Delegate to the same normalization as names.ObjectPrefix for consistency.
	// ObjectPrefix produces "tc-{slug}-{hash8}" — strip the leading "tc-" and
	// the trailing "-{hash8}" to recover the slug, then append the hash as-is.
	// Simpler: just use the ObjectPrefix itself (≤ 63 chars, valid label value).
	return names.ObjectPrefix(tcID)
}

// mergeLabels returns dst with src merged in. src keys override dst.
// dst must not be nil (it is the standard TC label map returned by Labels()).
func mergeLabels(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	out := make(map[string]string, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

// mergeAnnotations returns dst with src merged in. src keys override dst.
func mergeAnnotations(dst, src map[string]string) map[string]string {
	return mergeLabels(dst, src) // same semantics
}
