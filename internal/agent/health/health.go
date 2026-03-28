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

// Package health defines the structured internal health model for the pillar
// agent.  It provides named, typed fields for every subsystem rather than a
// flat list of generic {name, bool, message} tuples, which makes call-sites
// unambiguous and allows callers to pattern-match on specific components
// without string comparisons.
//
// The public types are decoupled from the protobuf wire format.  The
// HealthStatus.ToProtoSubsystems helper converts them to the flat
// []*agentv1.SubsystemStatus slice that the gRPC response expects.
package health

import (
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ComponentStatus represents the health of a single agent subsystem component.
// Healthy is true when the component is fully operational.  Message carries a
// human-readable explanation — it MUST be non-empty when Healthy is false so
// that operators have actionable information.
type ComponentStatus struct {
	// Healthy is true when this component is fully operational.
	Healthy bool
	// Message is a human-readable description of the component's state.
	// Must be non-empty when Healthy is false.
	Message string
}

// OK constructs a healthy ComponentStatus with the given informational message.
func OK(message string) ComponentStatus {
	return ComponentStatus{Healthy: true, Message: message}
}

// Degraded constructs an unhealthy ComponentStatus carrying the given error
// description.
func Degraded(msg string) ComponentStatus {
	return ComponentStatus{Healthy: false, Message: msg}
}

// PoolStatus pairs a pool name with the health status of that pool's backend.
type PoolStatus struct {
	// Pool is the storage pool name (e.g. "tank", "hot-data").
	Pool string
	// Status is the operational health of this pool's backend.
	Status ComponentStatus
}

// HealthStatus is the structured internal representation of agent health.
//
// Each known subsystem has a dedicated, named field so that callers can
// inspect individual components without iterating over a generic list or
// performing string comparisons on names.
//
//	NvmetConfigfs — nvmet configfs tree accessibility
//	PerPoolStatus — per-pool backend reachability (one entry per registered pool)
//
// Backend-specific prerequisite checks (e.g. kernel-module presence) are
// intentionally omitted from this struct: they are backend-private concerns
// that do not belong in a generic health model.  Per-pool reachability
// (PerPoolStatus) serves as the authoritative signal for backend health.
//
//nolint:revive // "health.HealthStatus" is intentional: avoids ambiguity with ComponentStatus/PoolStatus.
type HealthStatus struct {
	// NvmetConfigfs reports whether the nvmet configfs directory tree is
	// mounted and accessible at the expected path.
	NvmetConfigfs ComponentStatus

	// PerPoolStatus holds one entry per registered storage pool, reporting
	// whether the pool's backend is reachable and responsive.
	PerPoolStatus []PoolStatus
}

// AllHealthy returns true only when every component — including all pools —
// is healthy.
func (h HealthStatus) AllHealthy() bool {
	if !h.NvmetConfigfs.Healthy {
		return false
	}
	for _, p := range h.PerPoolStatus {
		if !p.Status.Healthy {
			return false
		}
	}
	return true
}

// ToProtoSubsystems converts the structured HealthStatus into the flat
// []*agentv1.SubsystemStatus slice consumed by agentv1.HealthCheckResponse.
//
// Name conventions used in the output (stable, used by callers that parse the
// response):
//   - "nvmet_configfs"    — nvmet configfs check
//   - "pool/<pool-name>"  — per-pool backend check
func (h HealthStatus) ToProtoSubsystems() []*agentv1.SubsystemStatus {
	result := append(
		make([]*agentv1.SubsystemStatus, 0, 1+len(h.PerPoolStatus)),
		&agentv1.SubsystemStatus{
			Name:    "nvmet_configfs",
			Healthy: h.NvmetConfigfs.Healthy,
			Message: h.NvmetConfigfs.Message,
		},
	)

	for _, ps := range h.PerPoolStatus {
		result = append(result, &agentv1.SubsystemStatus{
			Name:    "pool/" + ps.Pool,
			Healthy: ps.Status.Healthy,
			Message: ps.Status.Message,
		})
	}

	return result
}
