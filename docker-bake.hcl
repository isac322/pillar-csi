// docker-bake.hcl — Build matrix for all pillar-csi container images.
//
// Usage:
//   docker buildx bake                     # build all 3 images (parallel)
//   docker buildx bake controller          # build controller only
//   docker buildx bake --load              # build + load into Docker daemon
//   TAG=v1.0.0 docker buildx bake         # override image tag
//
// CI usage (GitHub Actions):
//   docker buildx bake --set "*.cache-from=type=gha" --set "*.cache-to=type=gha,mode=max"
//
// All runtime targets share a single builder stage that compiles every binary
// in one `go install` invocation — go mod download, COPY, and build run once.

variable "TAG" {
  default = "latest"
}

variable "REGISTRY" {
  default = "ghcr.io/bhyoo/pillar-csi"
}

// All platforms supported for release builds.
variable "PLATFORMS" {
  default = ""
}

// ── Shared base configuration ────────────────────────────────────────────────

target "_common" {
  dockerfile = "Dockerfile"
  context    = "."
  platforms  = PLATFORMS != "" ? split(",", PLATFORMS) : []
}

// ── Build targets ────────────────────────────────────────────────────────────

group "default" {
  targets = ["controller", "agent", "node"]
}

target "controller" {
  inherits = ["_common"]
  target   = "controller"
  tags     = ["${REGISTRY}/controller:${TAG}"]
}

target "agent" {
  inherits = ["_common"]
  target   = "agent"
  tags     = ["${REGISTRY}/agent:${TAG}"]
}

target "node" {
  inherits = ["_common"]
  target   = "node"
  tags     = ["${REGISTRY}/node:${TAG}"]
}
