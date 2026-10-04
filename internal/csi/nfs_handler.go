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

package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	// VolumeContextKeyNFSVersion carries the resolved NFS protocol version.
	VolumeContextKeyNFSVersion = "pillar-csi.bhyoo.com/nfs-version"

	defaultNFSVersion = "4.2"
	defaultNFSPort    = "2049"
	nfsTransportTCP   = "tcp"
	nfsTransportTCP6  = "tcp6"
)

// NFSProtocolState is the durable state needed to identify an NFS mount after
// a pillar-node restart. NFS has no client session to reconnect, but retaining
// the exact source makes unstage and idempotent restage safe when volume
// context is accidentally changed by the CO.
type NFSProtocolState struct {
	Address     string `json:"address"`
	ExportPath  string `json:"export_path"`
	Port        string `json:"port"`
	Version     string `json:"version"`
	MountSource string `json:"mount_source"`
}

// ProtocolType satisfies ProtocolState.
func (*NFSProtocolState) ProtocolType() string { return ProtocolNFS }

// NFSHandler implements the client-side NFS protocol. Attach validates the
// controller-supplied export identity and returns a kernel mount source;
// NodeStageVolume performs the actual mount through the injected Mounter.
type NFSHandler struct{}

// NewNFSHandler returns the production NFS protocol handler.
func NewNFSHandler() *NFSHandler { return &NFSHandler{} }

// Attach validates the NFS export and returns its direct mount source. No
// userspace NFS client is invoked here; the existing Mounter invokes the
// bundled mount helper during NodeStageVolume.
func (*NFSHandler) Attach(_ context.Context, params AttachParams) (*AttachResult, error) {
	state, err := nfsStateFromParams(params)
	if err != nil {
		return nil, err
	}
	return &AttachResult{MountSource: state.MountSource, State: state}, nil
}

// Detach is idempotent because NFS mounts are torn down by NodeUnstageVolume.
// The state is still type-checked so corrupt durable state cannot be silently
// accepted after a restart.
func (*NFSHandler) Detach(_ context.Context, state ProtocolState) error {
	st, ok := state.(*NFSProtocolState)
	if !ok || st == nil {
		return fmt.Errorf("nfs Detach: expected *NFSProtocolState, got %T", state)
	}
	if st.MountSource == "" {
		return fmt.Errorf("nfs Detach: state has empty mount source")
	}
	return nil
}

// Rescan is a no-op. NFS server-side quota growth is visible through the
// mounted filesystem without a block-device rescan.
func (*NFSHandler) Rescan(_ context.Context, state ProtocolState) error {
	st, ok := state.(*NFSProtocolState)
	if !ok || st == nil {
		return fmt.Errorf("nfs Rescan: expected *NFSProtocolState, got %T", state)
	}
	return nil
}

func nfsStateFromParams(params AttachParams) (*NFSProtocolState, error) {
	if params.ProtocolType != "" && params.ProtocolType != ProtocolNFS {
		return nil, fmt.Errorf("nfs: unexpected protocol type %q", params.ProtocolType)
	}
	version, port, err := nfsVersionAndPort(params)
	if err != nil {
		return nil, err
	}
	address, err := nfsServerAddress(params.Address)
	if err != nil {
		return nil, err
	}
	err = validateNFSExportPath(params.VolumeRef)
	if err != nil {
		return nil, err
	}
	mountSource := address + ":" + params.VolumeRef
	if ip := net.ParseIP(address); ip != nil && ip.To4() == nil {
		mountSource = "[" + address + "]:" + params.VolumeRef
	}
	return &NFSProtocolState{
		Address: address, ExportPath: params.VolumeRef, Port: port,
		Version: version, MountSource: mountSource,
	}, nil
}

func nfsVersionAndPort(params AttachParams) (version, port string, err error) {
	version = params.Extra[VolumeContextKeyNFSVersion]
	if version == "" {
		version = defaultNFSVersion
	}
	if version != defaultNFSVersion {
		return "", "", fmt.Errorf("nfs: unsupported version %q, only %s is supported", version, defaultNFSVersion)
	}
	port = params.Port
	if port == "" {
		port = defaultNFSPort
	}
	if port != defaultNFSPort {
		return "", "", fmt.Errorf("nfs: unsupported port %q, only %s is supported", port, defaultNFSPort)
	}
	return version, port, nil
}

func nfsServerAddress(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("nfs: server address is required")
	}
	if strings.ContainsAny(raw, "\x00\r\n/") {
		return "", fmt.Errorf("nfs: invalid server address %q", raw)
	}
	address := strings.TrimPrefix(strings.TrimSuffix(raw, "]"), "[")
	if address == "" || strings.Contains(address, "[") || strings.Contains(address, "]") {
		return "", fmt.Errorf("nfs: invalid server address %q", raw)
	}
	return address, nil
}

func validateNFSExportPath(path string) error {
	if path == "" || !strings.HasPrefix(path, "/") ||
		strings.ContainsAny(path, "\x00\r\n") {
		return fmt.Errorf("nfs: export path must be an absolute path")
	}
	if strings.Contains(path, " ") {
		return fmt.Errorf("nfs: export path must not contain spaces")
	}
	return nil
}

// nfsMountFlags resolves the existing mountOptions precedence rules and then
// enforces the protocol invariants. User options remain effective for every
// non-structural flag; contradictory structural options are rejected rather
// than allowed to weaken the supported NFS contract.
func nfsMountFlags(
	volCtx map[string]string,
	volCapMount *csi.VolumeCapability_MountVolume,
	serverAddr string,
) ([]string, error) {
	requiredProto := nfsMountTransport(serverAddr)
	flags, err := collectNFSMountFlags(volCtx, volCapMount)
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(flags)+4)
	for _, flag := range flags {
		structural, optionErr := validateNFSStructuralMountOption(flag, requiredProto)
		if optionErr != nil {
			return nil, optionErr
		}
		if !structural {
			filtered = append(filtered, flag)
		}
	}
	return append(filtered, "hard", "nfsvers=4.2", "proto="+requiredProto, "port=2049"), nil
}

func nfsMountTransport(serverAddr string) string {
	// NFS over IPv6 requires the tcp6 netid. The mount.nfs helper does not
	// reliably infer that from a bracketed source, so select it explicitly.
	if ip := net.ParseIP(serverAddr); ip != nil && ip.To4() == nil {
		return nfsTransportTCP6
	}
	return nfsTransportTCP
}

func collectNFSMountFlags(
	volCtx map[string]string,
	volCapMount *csi.VolumeCapability_MountVolume,
) ([]string, error) {
	// Preserve the existing VolumeContext-over-capability precedence,
	// including an explicitly empty option list replacing capability flags.
	raw, ok := volCtx[paramMountOptions]
	if !ok {
		return volCapMount.GetMountFlags(), nil
	}
	var flags []string
	err := decodeMountOptions(raw, &flags)
	if err != nil {
		return nil, err
	}
	return flags, nil
}

func validateNFSStructuralMountOption(flag, requiredProto string) (bool, error) {
	key, value, _ := strings.Cut(strings.TrimSpace(flag), "=")
	key = strings.ToLower(key)
	value = strings.ToLower(strings.TrimSpace(value))
	switch key {
	case "hard":
		return true, nil
	case nfsTransportTCP, nfsTransportTCP6:
		if key != requiredProto {
			return true, fmt.Errorf("NFS mount option %q conflicts with required %s transport", flag, requiredProto)
		}
	case "soft", "softerr", "softreval", "udp", "rdma":
		return true, fmt.Errorf("NFS mount option %q conflicts with required hard %s transport", flag, requiredProto)
	case "nfsvers", "vers":
		if value != defaultNFSVersion {
			return true, fmt.Errorf("NFS mount option %q conflicts with required NFS version %s", flag, defaultNFSVersion)
		}
	case "proto":
		if value != requiredProto {
			return true, fmt.Errorf("NFS mount option %q conflicts with required %s transport", flag, requiredProto)
		}
	case "proto6":
		// proto6=tcp is the IPv6 spelling accepted by mount.nfs; the
		// canonical emitted option remains proto=tcp6 for both direct
		// and durable restage paths.
		if requiredProto != nfsTransportTCP6 || value != nfsTransportTCP {
			return true, fmt.Errorf("NFS mount option %q conflicts with required %s transport", flag, requiredProto)
		}
	case "port":
		if value != defaultNFSPort {
			return true, fmt.Errorf("NFS mount option %q conflicts with required port %s", flag, defaultNFSPort)
		}
	default:
		return false, nil
	}
	return true, nil
}

func decodeMountOptions(raw string, dst *[]string) error {
	err := json.Unmarshal([]byte(raw), dst)
	if err != nil {
		return fmt.Errorf("volume_context: decode %s %q as a JSON string array: %w", paramMountOptions, raw, err)
	}
	return nil
}

// validateNFSStageFilesystem rejects block and formatting-only settings before
// any protocol or mount side effect. NFS exports are already filesystems.
func validateNFSStageFilesystem(volCtx map[string]string, mount *csi.VolumeCapability_MountVolume) error {
	if mount == nil {
		return fmt.Errorf("NFS volumes require mount access")
	}
	if fsType := mount.GetFsType(); fsType != "" && fsType != ProtocolNFS {
		return fmt.Errorf("NFS volumes require fsType %q, got %q", ProtocolNFS, fsType)
	}
	if fsType := volCtx[paramFSType]; fsType != "" && fsType != ProtocolNFS {
		return fmt.Errorf("NFS volumes require volume_context %s %q, got %q", paramFSType, ProtocolNFS, fsType)
	}
	if raw, ok := volCtx[paramMkfsOptions]; ok && raw != "" && raw != "[]" && raw != "null" {
		var opts []string
		err := json.Unmarshal([]byte(raw), &opts)
		if err != nil {
			return fmt.Errorf("NFS volumes reject invalid mkfs options: %w", err)
		}
		if len(opts) != 0 {
			return fmt.Errorf("NFS volumes do not support mkfsOptions")
		}
	}
	trim, err := parsePeriodicTrim(volCtx)
	if err != nil {
		return err
	}
	if trim != nil && *trim {
		return fmt.Errorf("NFS volumes do not support periodicTrim")
	}
	return nil
}

// NFSClientAvailable reports whether both the kernel NFS client and the
// bundled mount helper are available. It intentionally checks runtime state;
// registering an NFS handler when the kernel lacks NFS would make NodeGetInfo
// advertise a capability that can never stage successfully.
func NFSClientAvailable() bool {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	kernelNFS := false
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[1] == ProtocolNFS || fields[1] == "nfs4") {
			kernelNFS = true
			break
		}
	}
	if !kernelNFS {
		return false
	}
	_, nfsErr := exec.LookPath("mount.nfs")
	if nfsErr == nil {
		return true
	}
	_, nfs4Err := exec.LookPath("mount.nfs4")
	return nfs4Err == nil
}
