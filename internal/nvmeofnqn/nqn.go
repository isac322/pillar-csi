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

// Package nvmeofnqn holds the NVMe Qualified Name conventions shared by the
// agent (which names the subsystems it exports) and the node (which
// recognizes pillar subsystems among all connected ones).
package nvmeofnqn

// Prefix is the fixed NQN prefix of every NVMe subsystem pillar-csi exports.
const Prefix = "nqn.2026-01.com.bhyoo.pillar-csi:"
