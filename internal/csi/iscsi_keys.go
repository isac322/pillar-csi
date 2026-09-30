package csi

// iSCSI VolumeContext keys.  CreateVolume writes them from the resolved
// iscsi protocol configuration (only when the resolved value is set) and
// NodeStageVolume reads them to tune the initiator session.  Values are
// decimal seconds; an absent key keeps the node default.
const (
	// VolumeContextKeyISCSILoginTimeout is the login timeout in seconds
	// (resolved iscsi.loginTimeout; node default 15).
	VolumeContextKeyISCSILoginTimeout = "pillar-csi.bhyoo.com/iscsi-login-timeout"

	// VolumeContextKeyISCSIReplacementTimeout is the session replacement
	// timeout in seconds (resolved iscsi.replacementTimeout; node default 120).
	VolumeContextKeyISCSIReplacementTimeout = "pillar-csi.bhyoo.com/iscsi-replacement-timeout"

	// VolumeContextKeyISCSINoopOutInterval is the NOP-Out ping interval in
	// seconds (resolved iscsi.noopOutInterval; node default 5).
	VolumeContextKeyISCSINoopOutInterval = "pillar-csi.bhyoo.com/iscsi-noop-out-interval"

	// VolumeContextKeyISCSINoopOutTimeout is the NOP-In reply timeout in
	// seconds (resolved iscsi.noopOutTimeout; node default 5).
	VolumeContextKeyISCSINoopOutTimeout = "pillar-csi.bhyoo.com/iscsi-noop-out-timeout"
)
