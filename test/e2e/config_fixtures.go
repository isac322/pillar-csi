package e2e

// config_fixtures.go — shared fixtures for the pillar-csi configuration
// interface used by the in-process CSI controller TCs.
//
// CreateVolume resolves the effective volume configuration from live CRs:
//
//   - a generated StorageClass carries only the PillarStorageClass name
//     (e2eParamStorageClass); store, protocol, overrides and filesystem come
//     from that binding and the PillarStore / PillarProtocol it references;
//   - a hand-written StorageClass carries the PillarStore and PillarProtocol
//     names (e2eParamStoreRef / e2eParamProtocolRef) plus optional backend,
//     protocol and filesystem YAML documents.
//
// Every in-process controller environment therefore seeds real PillarStore and
// PillarProtocol objects into its fake Kubernetes client instead of passing
// flat backend / protocol / pool keys as StorageClass parameters.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pillarv1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// StorageClass parameter and PVC annotation keys of the configuration
// interface.  The three document keys are shared by hand-written StorageClass
// parameters and PVC annotations.
const (
	e2eParamStorageClass = "pillar-csi.bhyoo.com/storage-class"
	e2eParamStoreRef     = "pillar-csi.bhyoo.com/store-ref"
	e2eParamProtocolRef  = "pillar-csi.bhyoo.com/protocol-ref"

	e2eDocBackend    = pillarv1.AnnotationBackendDoc
	e2eDocProtocol   = pillarv1.AnnotationProtocolDoc
	e2eDocFilesystem = pillarv1.AnnotationFilesystemDoc

	// external-provisioner --extra-create-metadata keys naming the PVC whose
	// annotations form the per-volume override layer.
	e2eParamPVCName      = "csi.storage.k8s.io/pvc/name"
	e2eParamPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
)

// Default fixture names seeded by the in-process controller environments.
const (
	e2eDefaultStoreName       = "tank"
	e2eDefaultZFSPool         = "tank"
	e2eDefaultProtocolName    = "nvmeof"
	e2eDefaultACLProtocolName = "nvmeof-acl"
)

// e2eZFSStore returns a PillarStore whose backend is a ZFS pool on agent.
func e2eZFSStore(name, agent, pool string) *pillarv1.PillarStore {
	return &pillarv1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarv1.PillarStoreSpec{
			AgentRef: agent,
			Backend: pillarv1.BackendSpec{
				ZFS: &pillarv1.ZFSBackendConfig{
					VolumeType: pillarv1.ZFSVolumeTypeZvol,
					Pool:       pool,
				},
			},
		},
	}
}

// e2eLVMStore returns a PillarStore whose backend is an LVM volume group on
// agent.  thinPool may be empty; mode empty means the linear default.
func e2eLVMStore(name, agent, vg, thinPool string, mode pillarv1.LVMProvisioningMode) *pillarv1.PillarStore {
	if mode == "" {
		mode = pillarv1.LVMProvisioningModeLinear
	}
	return &pillarv1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarv1.PillarStoreSpec{
			AgentRef: agent,
			Backend: pillarv1.BackendSpec{
				LVM: &pillarv1.LVMBackendConfig{
					VolumeGroup:      vg,
					ThinPool:         thinPool,
					ProvisioningMode: mode,
				},
			},
		},
	}
}

// e2eNVMeOFProtocol returns a PillarProtocol selecting NVMe-oF/TCP on the
// default port with ACL disabled (the default on every layer).  The
// controller skips the AllowInitiator/DenyInitiator RPCs for volumes created
// over this protocol; tests asserting the initiator grant/revoke path must
// use e2eNVMeOFACLProtocol instead.
func e2eNVMeOFProtocol(name string) *pillarv1.PillarProtocol {
	return &pillarv1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarv1.PillarProtocolSpec{
			Protocol: pillarv1.ProtocolSpec{
				NVMeOFTCP: &pillarv1.NVMeOFTCPConfig{Port: 4420},
			},
		},
	}
}

// e2eNVMeOFACLProtocol returns a PillarProtocol selecting NVMe-oF/TCP on the
// default port with ACL enabled (acl: true), so the controller calls
// agent.AllowInitiator/DenyInitiator on publish/unpublish.
func e2eNVMeOFACLProtocol(name string) *pillarv1.PillarProtocol {
	p := e2eNVMeOFProtocol(name)
	p.Spec.Protocol.NVMeOFTCP.ACL = true
	return p
}

// e2eBinding returns a PillarStorageClass binding store and protocol.
func e2eBinding(name, store, protocol string) *pillarv1.PillarStorageClass {
	return &pillarv1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pillarv1.PillarStorageClassSpec{
			StoreRef:    store,
			ProtocolRef: protocol,
		},
	}
}

// e2eHandWrittenParams returns the identity parameters of a hand-written
// StorageClass referencing store and protocol.
func e2eHandWrittenParams(store, protocol string) map[string]string {
	return map[string]string{
		e2eParamStoreRef:    store,
		e2eParamProtocolRef: protocol,
	}
}

// e2eBindingParams returns the parameters of a generated StorageClass for the
// named PillarStorageClass.
func e2eBindingParams(binding string) map[string]string {
	return map[string]string{e2eParamStorageClass: binding}
}
