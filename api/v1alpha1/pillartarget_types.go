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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeRefSpec references a Kubernetes Node for agent address resolution.
type NodeRefSpec struct {
	// name is the Kubernetes Node name.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// addressType selects which address type to use from the node's status.addresses.
	// Defaults to InternalIP.
	// +optional
	// +kubebuilder:default=InternalIP
	// +kubebuilder:validation:Enum=InternalIP;ExternalIP
	AddressType string `json:"addressType,omitempty"`

	// addressSelector is an optional CIDR filter applied when multiple addresses of
	// the same type exist on the node.
	// +optional
	AddressSelector string `json:"addressSelector,omitempty"`

	// port overrides the default agent gRPC port (9500).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`
}

// ExternalSpec defines a storage agent outside the Kubernetes cluster.
type ExternalSpec struct {
	// address is the IP or hostname of the external agent.
	// +required
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`

	// port is the agent gRPC port.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// PillarTargetSpec defines the desired state of PillarTarget.
// Exactly one of nodeRef or external must be set (discriminated union).
type PillarTargetSpec struct {
	// nodeRef references a Kubernetes Node whose agent is accessible via the node's IP.
	// Mutually exclusive with external.
	// +optional
	NodeRef *NodeRefSpec `json:"nodeRef,omitempty"`

	// external addresses a storage agent that lives outside the Kubernetes cluster.
	// Mutually exclusive with nodeRef.
	// +optional
	External *ExternalSpec `json:"external,omitempty"`
}

// DiscoveredPool is a storage pool reported by the remote agent.
type DiscoveredPool struct {
	// name is the pool name as known to the storage subsystem (e.g. ZFS pool name).
	// +required
	Name string `json:"name"`

	// type is the storage technology (e.g. zfs, lvm).
	// +required
	Type string `json:"type"`

	// total is the total raw capacity of the pool.
	// +optional
	Total *resource.Quantity `json:"total,omitempty"`

	// available is the free capacity of the pool.
	// +optional
	Available *resource.Quantity `json:"available,omitempty"`
}

// AgentCapabilities describes what backends and protocols the remote agent supports.
type AgentCapabilities struct {
	// backends lists the backend driver types the agent can manage
	// (e.g. zfs-zvol, zfs-dataset, lvm-lv).
	// +optional
	Backends []string `json:"backends,omitempty"`

	// protocols lists the network protocols the agent can export storage over
	// (e.g. nvmeof-tcp, iscsi, nfs).
	// +optional
	Protocols []string `json:"protocols,omitempty"`
}

// PillarTargetStatus defines the observed state of PillarTarget.
type PillarTargetStatus struct {
	// resolvedAddress is the IP address selected for gRPC communication with the agent.
	// +optional
	ResolvedAddress string `json:"resolvedAddress,omitempty"`

	// agentVersion is the version string reported by the connected agent.
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`

	// capabilities summarises what the connected agent is capable of.
	// +optional
	Capabilities *AgentCapabilities `json:"capabilities,omitempty"`

	// discoveredPools lists storage pools found on the agent at last reconcile.
	// +optional
	DiscoveredPools []DiscoveredPool `json:"discoveredPools,omitempty"`

	// conditions represent the current state of the PillarTarget resource.
	//
	// Known condition types:
	// - "NodeExists"     – the referenced K8s Node is present in the cluster.
	// - "AgentConnected" – the gRPC connection to the agent is healthy.
	// - "Ready"          – all checks pass; the target is ready to serve pools.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pt
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.resolvedAddress`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.status.agentVersion`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PillarTarget represents a storage agent instance that pillar-csi controller
// manages.  Users create PillarTarget resources; the controller reconciles gRPC
// connectivity and populates status fields.
type PillarTarget struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PillarTarget.
	// +required
	Spec PillarTargetSpec `json:"spec"`

	// status defines the observed state of PillarTarget.
	// +optional
	Status PillarTargetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PillarTargetList contains a list of PillarTarget.
type PillarTargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PillarTarget `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PillarTarget{}, &PillarTargetList{})
}
