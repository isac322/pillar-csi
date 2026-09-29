package csi

// PublishContext keys ControllerPublishVolume sets when it publishes a volume
// to the node that hosts the volume's PillarAgent with localAttach enabled.
// NodeStageVolume then attaches the backend device directly instead of
// connecting through the network protocol.  A publish without these keys is
// a protocol attach.
const (
	// PublishContextKeyAttachMode selects the node attach path; the only
	// value is AttachModeLocal.  Absent means a protocol attach.
	PublishContextKeyAttachMode = "pillar-csi.bhyoo.com/attach-mode"

	// PublishContextKeyLocalNode is the node the controller decided is the
	// storage node.  NodeStageVolume refuses a local attach on any other
	// node.
	PublishContextKeyLocalNode = "pillar-csi.bhyoo.com/local-node"

	// PublishContextKeyLocalDevicePath is the host path of the backend block
	// device (zvol or logical volume) returned by the agent.
	PublishContextKeyLocalDevicePath = "pillar-csi.bhyoo.com/local-device-path"

	// AttachModeLocal is the PublishContextKeyAttachMode value for a direct
	// attach on the storage node.
	AttachModeLocal = "local"
)
