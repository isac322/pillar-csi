// Package csi exposes Linux device paths shared between the CSI services
// and binaries (cmd/controller, cmd/node) so the literal is defined once.
package csi

// NvmeFabricsDevice is the kernel control device used by the NVMe-oF
// fabrics initiator to manage connections. The CSI node binary requires
// this path to exist before advertising readiness, and the NVMe-oF
// connector reads from it to issue connect/disconnect operations.
const NvmeFabricsDevice = "/dev/nvme-fabrics"
