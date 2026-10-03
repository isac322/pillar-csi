package csi

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// FileTopologyNodeKey identifies the Kubernetes node on which a single-node
// adopted filesystem is directly accessible.  It is deliberately separate
// from protocol capability topology: the controller uses this key to restrict
// local filesystem placement to the agent's NodeRef.
const FileTopologyNodeKey = "files.pillar-csi.bhyoo.com/node"

// File CSI context keys carry immutable, controller-validated adoption data to
// the node.  Values are informational routing records, not user-controlled
// paths; NodeServer still validates every path before mounting it.
const (
	VolumeContextKeyCSIDriver                 = "pillar-csi.bhyoo.com/csi-driver"
	VolumeContextKeyFilesystemAdoption        = "pillar-csi.bhyoo.com/filesystem-adoption"
	VolumeContextKeyFilesystemCapacity        = "pillar-csi.bhyoo.com/filesystem-capacity-bytes"
	VolumeContextKeyFilesystemCanonicalSource = "pillar-csi.bhyoo.com/filesystem-canonical-source"
	VolumeContextKeyFilesystemLayout          = "pillar-csi.bhyoo.com/filesystem-layout"
	VolumeContextKeyFilesystemLocalAttach     = "pillar-csi.bhyoo.com/filesystem-local-attach"

	PublishContextKeyFilesystemAdoption        = "pillar-csi.bhyoo.com/filesystem-adoption"
	PublishContextKeyFilesystemCapacity        = "pillar-csi.bhyoo.com/filesystem-capacity-bytes"
	PublishContextKeyFilesystemCanonicalSource = "pillar-csi.bhyoo.com/filesystem-canonical-source"
	PublishContextKeyFilesystemLayout          = "pillar-csi.bhyoo.com/filesystem-layout"
	PublishContextKeyFilesystemProxyPath       = "pillar-csi.bhyoo.com/filesystem-proxy-path"
	PublishContextKeyFilesystemLocalNode       = "pillar-csi.bhyoo.com/filesystem-local-node"

	fileContextAgentAddress  = "filesystem_agent_endpoint"
	fileContextAgentName     = "filesystem_agent_name"
	fileContextAgentVolumeID = "filesystem_agent_volume_id"
)

// filesystemContextAdoption is the JSON wire shape shared by VolumeContext
// and PublishContext.  It intentionally mirrors the additive agent message,
// while keeping the node independent of generated protobuf JSON behavior.
type filesystemContextAdoption struct {
	Kind            string `json:"kind"`
	CanonicalSource string `json:"canonicalSource"`
	ResourceID      string `json:"resourceId"`
	HostPath        string `json:"hostPath,omitempty"`
	FilesystemType  string `json:"filesystemType"`
	FilesystemID    string `json:"filesystemId,omitempty"`
	Inode           uint64 `json:"inode,omitempty"`
	ProjectID       uint32 `json:"projectId,omitempty"`
}

func marshalFilesystemAdoption(a *agentv1.FilesystemAdoption) (string, error) {
	if a == nil {
		return "", nil
	}
	if a.GetKind() == "" || a.GetCanonicalSource() == "" || a.GetResourceId() == "" || a.GetFilesystemType() == "" {
		return "", fmt.Errorf("filesystem adoption descriptor is incomplete")
	}
	data, err := json.Marshal(filesystemContextAdoption{
		Kind: a.GetKind(), CanonicalSource: a.GetCanonicalSource(), ResourceID: a.GetResourceId(),
		HostPath: a.GetHostPath(), FilesystemType: a.GetFilesystemType(), FilesystemID: a.GetFilesystemId(),
		Inode: a.GetInode(), ProjectID: a.GetProjectId(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal filesystem adoption: %w", err)
	}
	return string(data), nil
}

// addFilesystemPublishContext writes the immutable filesystem adoption record
// used by NodeStageVolume. A non-filesystem lifecycle is a no-op. ProxyPath
// is populated only by same-node SetLocalAttach; an empty path never causes the
// node to infer a source from volume_id or another user field.
func addFilesystemPublishContext(
	pvs *v1alpha1.PillarVolumeState, ctx map[string]string, proxyPath, agentEndpoint string,
) error {
	if pvs == nil || pvs.Spec.FilesystemAdoption == nil {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("filesystem publish context map is nil")
	}
	a, err := filesystemAdoptionProto(pvs.Spec.FilesystemAdoption)
	if err != nil {
		return fmt.Errorf("convert persisted filesystem adoption context: %w", err)
	}
	adoptionJSON, err := marshalFilesystemAdoption(a)
	if err != nil {
		return fmt.Errorf("marshal filesystem adoption context: %w", err)
	}
	if pvs.Spec.CapacityBytes <= 0 {
		return fmt.Errorf("filesystem adoption requires positive exact capacity")
	}
	ctx[PublishContextKeyFilesystemAdoption] = adoptionJSON
	ctx[PublishContextKeyFilesystemCapacity] = strconv.FormatInt(pvs.Spec.CapacityBytes, 10)
	ctx[PublishContextKeyFilesystemCanonicalSource] = pvs.Spec.FilesystemAdoption.CanonicalSource
	ctx[VolumeContextKeyCSIDriver] = v1alpha1.FileCSIDriver
	ctx[fileContextAgentName] = pvs.Spec.AgentRef
	ctx[fileContextAgentVolumeID] = pvs.Spec.AgentVolumeID
	if agentEndpoint != "" {
		ctx[fileContextAgentAddress] = agentEndpoint
	}
	localOnly := pvs.Spec.Resolved != nil && pvs.Spec.Resolved.LocalAttach
	ctx[VolumeContextKeyFilesystemLocalAttach] = strconv.FormatBool(localOnly)
	err = addFilesystemLayoutContext(pvs.Spec.Resolved, ctx)
	if err != nil {
		return err
	}
	return addFilesystemProxyContext(ctx, proxyPath)
}

func addFilesystemLayoutContext(resolved *v1alpha1.ResolvedVolumeConfig, ctx map[string]string) error {
	if resolved == nil {
		return nil
	}
	params := backendParamsFromResolved(resolved.Backend)
	if params == nil {
		return nil
	}
	layoutJSONBytes, err := protojson.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal filesystem backend layout: %w", err)
	}
	ctx[PublishContextKeyFilesystemLayout] = string(layoutJSONBytes)
	return nil
}

func addFilesystemProxyContext(ctx map[string]string, proxyPath string) error {
	if proxyPath == "" {
		return nil
	}
	if !filepath.IsAbs(proxyPath) || filepath.Clean(proxyPath) != proxyPath || strings.ContainsRune(proxyPath, '\x00') {
		return fmt.Errorf("invalid filesystem proxy path %q", proxyPath)
	}
	if ctx[PublishContextKeyFilesystemLocalNode] == "" {
		return fmt.Errorf("filesystem proxy path requires controller-selected local node")
	}
	ctx[PublishContextKeyFilesystemProxyPath] = proxyPath
	return nil
}
