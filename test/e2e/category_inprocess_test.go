package e2e

// category_inprocess_test.go — Sub-AC 3: Real test assertions for all
// "in-process" category TCs.
//
// In-process TCs cover the following spec groups:
//
//	E1  — Volume lifecycle: CreateVolume / DeleteVolume
//	E2  — ControllerPublishVolume / ControllerUnpublishVolume
//	E3  — Node staging, publishing, expansion, and cleanup
//	E4  — ListVolumes / GetCapacity
//	E5  — ControllerGetCapabilities / NodeGetCapabilities / Identity
//	E6  — CreateSnapshot / DeleteSnapshot
//	E7  — Volume cloning (CreateVolume from source)
//	E8  — mTLS transport
//	E9  — Agent gRPC + fake configfs + export contracts
//	E11 — ControllerExpandVolume / NodeExpandVolume
//	E12 — ValidateVolumeCapabilities
//	E13 — Volume content source (pre-populated volumes)
//	E14 — Error path: invalid parameters, unknown fields
//	E15 — Idempotency: duplicate create/delete
//	E16 — Concurrency: parallel create/delete
//	E17 — Volume context propagation
//	E18 — NodeGetVolumeStats
//	E21 — Agent protocol negotiation
//	E22 — Volume access mode matrix
//	E24 — PillarBinding lifecycle
//	E28 — Agent ZFS backend
//	E29 — PillarPool lifecycle
//	E30 — PillarTarget lifecycle
//
// Every assertion embeds tc.tcNodeLabel() and tc.SectionTitle in its message so
// that the tc_failure_output.go ReportAfterEach hook can emit a structured
// single-line failure that is grep-addressable by TC ID.
//
// Isolation: runInProcessTCBody does NOT share mutable state between TCs. Each
// per-TC assertion function creates a fresh isolated test environment. TCs that
// don't yet have a specific assertion fall back to the defaultLocalVerifierRegistry
// cached verifier approach — this is intentional and correct for the current stage
// of implementation.

import (
	. "github.com/onsi/gomega"
)

// inProcessAssertions maps TestName → assertion function.
// TCs whose TestName appears in this map get a fresh isolated environment
// and specific per-TC assertions. All other TCs fall back to the cached
// verifier in defaultLocalVerifierRegistry.
var inProcessAssertions = map[string]func(documentedCase){
	// ── E1: Volume Lifecycle ──────────────────────────────────────────────────
	"TestCSIController_CreateVolume":                                            assertE1_CreateVolume,
	"TestCSIController_CreateVolume_Idempotency":                                assertE1_CreateVolume_Idempotency,
	"TestCSIController_CreateVolume_MissingParams":                              assertE1_CreateVolume_MissingParams,
	"TestCSIController_CreateVolume_PillarTargetNotFound":                       assertE1_CreateVolume_PillarTargetNotFound,
	"TestCSIController_CreateVolume_AgentCreateError":                           assertE1_CreateVolume_AgentCreateError,
	"TestCSIController_CreateVolume_AgentExportError":                           assertE1_CreateVolume_AgentExportError,
	"TestCSIController_DeleteVolume":                                            assertE1_DeleteVolume,
	"TestCSIController_DeleteVolume_Idempotency":                                assertE1_DeleteVolume_Idempotency,
	"TestCSIController_DeleteVolume_NotFoundIsIdempotent":                       assertE1_DeleteVolume_NotFoundIsIdempotent,
	"TestCSIController_DeleteVolume_MalformedID":                                assertE1_DeleteVolume_MalformedID,
	"TestCSIController_DeleteVolume_AgentError":                                 assertE1_DeleteVolume_AgentError,
	"TestCSIController_FullRoundTrip":                                           assertE1_FullRoundTrip,
	"TestCSIController_VolumeIDFormatPreservation":                              assertE1_VolumeIDFormatPreservation,
	"TestCSIController_CreateVolume_AccessMode_RWO":                             assertE1_CreateVolume_AccessMode_RWO,
	"TestCSIController_CreateVolume_AccessMode_RWOP":                            assertE1_CreateVolume_AccessMode_RWOP,
	"TestCSIController_CreateVolume_AccessMode_ROX":                             assertE1_CreateVolume_AccessMode_ROX,
	"TestCSIController_CreateVolume_AccessMode_RWX_Rejected":                    assertE1_CreateVolume_AccessMode_RWX_Rejected,
	"TestCSIController_CreateVolume_AccessMode_Unknown_Rejected":                assertE1_CreateVolume_AccessMode_Unknown_Rejected,
	"TestCSIController_CreateVolume_AccessMode_Missing_InCapability":            assertE1_CreateVolume_AccessMode_Missing_InCapability,
	"TestCSIController_CreateVolume_VolumeCapabilities_Empty":                   assertE1_CreateVolume_VolumeCapabilities_Empty,
	"TestCSIController_CreateVolume_MultipleCapabilities_AnyUnsupported":        assertE1_CreateVolume_MultipleCapabilities_AnyUnsupported,
	"TestCSIController_CreateVolume_Capacity_NoRange":                           assertE1_Capacity_NoRange,
	"TestCSIController_CreateVolume_Capacity_RequiredOnly":                      assertE1_Capacity_RequiredOnly,
	"TestCSIController_CreateVolume_Capacity_LimitOnly":                         assertE1_Capacity_LimitOnly,
	"TestCSIController_CreateVolume_Capacity_ValidRange":                        assertE1_Capacity_ValidRange,
	"TestCSIController_CreateVolume_Capacity_ExistingTooSmall":                  assertE1_Capacity_ExistingTooSmall,
	"TestCSIController_CreateVolume_Capacity_ExistingTooLarge":                  assertE1_Capacity_ExistingTooLarge,
	"TestCSIController_CreateVolume_Capacity_ExistingWithinRange":               assertE1_Capacity_ExistingWithinRange,
	"TestCSIController_CreateVolume_PillarTargetEmptyAddress":                   assertE1_CreateVolume_PillarTargetEmptyAddress,
	"TestCSIController_CreateVolume_AgentDialFails":                             assertE1_CreateVolume_AgentDialFails,
	"TestCSIController_PartialFailure_CreateThenExportFail":                     assertE1_PartialFailure_CreateThenExportFail,
	"TestCSIController_PartialFailure_ExportRetrySkipsBackend":                  assertE1_PartialFailure_ExportRetrySkipsBackend,
	"TestCSIController_PartialFailure_SelfHealing_TwoAttempts":                  assertE1_PartialFailure_SelfHealing_TwoAttempts,
	"TestCSIController_PartialFailure_PersistPartialFails":                      assertE1_PartialFailure_PersistPartialFails,
	"TestCSIController_PartialFailure_LoadStateFromCRD":                         assertE1_PartialFailure_LoadStateFromCRD,
	"TestCSIController_CreateVolume_PVCAnnotation_BackendOverride_Compression":  assertE1_PVCAnnotation_BackendOverride_Compression,
	"TestCSIController_CreateVolume_PVCAnnotation_StructuralFieldBlocked":       assertE1_PVCAnnotation_StructuralFieldBlocked,
	"TestCSIController_CreateVolume_PVCAnnotation_PVCNotFound_GracefulFallback": assertE1_PVCAnnotation_PVCNotFound_GracefulFallback,
	"TestCSIController_CreateVolume_PVCAnnotation_FlatKeyOverride":              assertE1_PVCAnnotation_FlatKeyOverride,
	"TestCSIController_CreateVolume_VolumeID_ZFSPoolWithSlash":                  assertE1_VolumeID_ZFSPoolWithSlash,
	"TestCSIController_CreateVolume_VolumeID_ZFSParentDataset":                  assertE1_VolumeID_ZFSParentDataset,
	"TestCSIController_CreateVolume_MissingVolumeName":                          assertE1_CreateVolume_MissingVolumeName,
	"TestCSIController_CreateVolume_MissingTargetParam":                         assertE1_CreateVolume_MissingTargetParam,
	"TestCSIController_CreateVolume_MissingBackendTypeParam":                    assertE1_CreateVolume_MissingBackendTypeParam,
	"TestCSIController_CreateVolume_MissingProtocolTypeParam":                   assertE1_CreateVolume_MissingProtocolTypeParam,
	// ── E2: ControllerPublish/Unpublish ──────────────────────────────────────
	"TestCSIController_ControllerPublishVolume":                                      assertE2_ControllerPublishVolume,
	"TestCSIController_ControllerPublishVolume_ISCSIInitiatorFromCSINodeAnnotations": assertE2_ControllerPublishVolume_ISCSI,
	"TestCSIController_ControllerPublishVolume_AlreadyPublished":                     assertE2_ControllerPublishVolume_AlreadyPublished,
	"TestCSIController_ControllerUnpublishVolume_Success":                            assertE2_ControllerUnpublishVolume_Success,
	"TestCSIController_ControllerUnpublishVolume_AlreadyUnpublished":                 assertE2_ControllerUnpublishVolume_AlreadyUnpublished,
	"TestCSIController_ControllerUnpublishVolume_EmptyVolumeID":                      assertE2_ControllerUnpublishVolume_EmptyVolumeID,
	"TestCSIController_ControllerUnpublishVolume_EmptyNodeID":                        assertE2_ControllerUnpublishVolume_EmptyNodeID,
	"TestCSIController_ControllerUnpublishVolume_MalformedVolumeID":                  assertE2_ControllerUnpublishVolume_MalformedVolumeID,
	"TestCSIErrors_ControllerUnpublish_DenyInitiatorNonNotFound":                     assertE2_DenyInitiatorNonNotFound,
	"TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes":               assertE2_ControllerPublish_DifferentNodes,
	"TestCSIErrors_ControllerPublish_AllowInitiatorFails":                            assertE2_AllowInitiatorFails,
	"TestCSIErrors_ControllerPublish_MissingNodeIdentityAnnotation":                  assertE2_MissingNodeIdentityAnnotation,
	"TestCSIController_ControllerPublishVolume_EmptyVolumeID":                        assertE2_ControllerPublish_EmptyVolumeID,
	"TestCSIController_ControllerPublishVolume_EmptyNodeID":                          assertE2_ControllerPublish_EmptyNodeID,
	"TestCSIController_ControllerPublishVolume_NilVolumeCapability":                  assertE2_ControllerPublish_NilVolumeCapability,
	"TestCSIController_ControllerPublishVolume_MalformedVolumeID":                    assertE2_ControllerPublish_MalformedVolumeID,
	"TestCSIController_ControllerPublishVolume_TargetNotFound":                       assertE2_ControllerPublish_TargetNotFound,
	"TestCSIController_ControllerPublishVolume_TargetNoResolvedAddress":              assertE2_ControllerPublish_TargetNoResolvedAddress,
	"TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs":               assertE2_ControllerPublish_DoubleSameArgs,
	// ── E3: Node Stage/Publish ────────────────────────────────────────────────
	"TestNodeFullLifecycle":                    assertE3_NodeFullLifecycle,
	"TestCSINode_StageVolume":                  assertE3_NodeStageVolume,
	"TestCSINode_UnstageVolume":                assertE3_NodeUnstageVolume,
	"TestCSINode_PublishVolume":                assertE3_NodePublishVolume,
	"TestCSINode_UnpublishVolume":              assertE3_NodeUnpublishVolume,
	"TestNodeStageVolume_Idempotency":          assertE3_NodeStageVolume_Idempotency,
	"TestNodeStageVolume_MissingVolumeContext": assertE3_NodeStageVolume_MissingVolumeContext,
	"TestNodeStageVolume_ConnectError":         assertE3_NodeStageVolume_ConnectError,
	"TestNodeStageVolume_FormatMountError":     assertE3_NodeStageVolume_FormatMountError,
	"TestNodeUnstageVolume_Idempotency":        assertE3_NodeUnstageVolume_Idempotency,
	"TestNodeUnstageVolume_DisconnectError":    assertE3_NodeUnstageVolume_DisconnectError,
	"TestStageState_Persistence":               assertE3_StageState_Persistence,
	"TestStageState_CorruptedFile":             assertE3_StageState_CorruptedFile,
	"TestNodePublishVolume_Idempotency":        assertE3_NodePublishVolume_Idempotency,
	"TestNodePublishVolume_ReadOnly":           assertE3_NodePublishVolume_ReadOnly,
	"TestNodePublishVolume_MountError":         assertE3_NodePublishVolume_MountError,
	"TestNodeUnpublishVolume_Idempotency":      assertE3_NodeUnpublishVolume_Idempotency,
	"TestNodeUnpublishVolume_UnmountError":     assertE3_NodeUnpublishVolume_UnmountError,
	// ── E4: Cross-component lifecycle ─────────────────────────────────────────
	"TestCSILifecycle_FullChain":          assertE4_FullChain,
	"TestCSILifecycle_CreateAndExpand":    assertE4_CreateAndExpand,
	"TestCSILifecycle_PublishUnpublish":   assertE4_PublishUnpublish,
	"TestCSILifecycle_DeleteAfterPublish": assertE4_DeleteAfterPublish,
	// ── E5: Ordering constraints ──────────────────────────────────────────────
	"TestCSIOrdering_NodeStageBeforeControllerPublish":   assertE5_NodeStageBeforeControllerPublish,
	"TestCSIOrdering_NodePublishBeforeNodeStage":         assertE5_NodePublishBeforeNodeStage,
	"TestCSIOrdering_NodeUnstageBeforeNodeUnpublish":     assertE5_NodeUnstageBeforeNodeUnpublish,
	"TestCSIOrdering_ControllerUnpublishBeforeNodeStage": assertE5_ControllerUnpublishBeforeNodeStage,
	"TestCSIOrdering_DeleteBeforeControllerUnpublish":    assertE5_DeleteBeforeControllerUnpublish,
	"TestCSIOrdering_ValidTransitionAfterRecovery":       assertE5_ValidTransitionAfterRecovery,
	// ── E6: Partial failure persistence ───────────────────────────────────────
	"TestCSIController_PartialFailure_CreateThenExportFail_CRD":     assertE6_PartialFailure_CreateThenExportFail_CRD,
	"TestCSIController_DeleteVolume_CleansUpCRD":                    assertE6_DeleteVolume_CleansUpCRD,
	"TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates": assertE6_DeleteVolumeOnPartialCreates,
	"TestCSIZvolNoDup_ExactlyOneZvolAfterExportFailureRetry":        assertE6_ZvolNoDup_OneZvol,
	"TestCSIZvolNoDup_ZvolRegistryReflectsDeleteAfterPartialCreate": assertE6_ZvolNoDup_DeleteRegistry,
	"TestCSIZvolNoDup_MultipleRetriesNeverDuplicate":                assertE6_ZvolNoDup_MultipleRetries,
	// ── E7: Publish idempotency ───────────────────────────────────────────────
	"TestCSIPublishIdempotency_ControllerPublishVolume":            assertE7_ControllerPublishIdempotency,
	"TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget": assertE7_NodePublishIdempotency,
	"TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble":   assertE7_NodePublishIdempotency_Readonly,
	"TestCSIPublishIdempotency_ControllerUnpublishVolume":          assertE7_ControllerUnpublishIdempotency,
	"TestCSIPublishIdempotency_NodeUnpublishVolume":                assertE7_NodeUnpublishIdempotency,
	// ── E8: mTLS ──────────────────────────────────────────────────────────────
	"TestMTLSController_Handshake":       assertE8_MTLSHandshake,
	"TestMTLSController_PlaintextReject": assertE8_MTLSPlaintextReject,
	"TestMTLSController_WrongCA":         assertE8_MTLSWrongCA,
	// ── E9: Agent gRPC ────────────────────────────────────────────────────────
	"TestAgent_CreateAndDeleteVolume":   assertE9_CreateAndDeleteVolume,
	"TestAgent_ExportAndUnexportVolume": assertE9_ExportAndUnexportVolume,
	"TestAgent_AllowAndDenyInitiator":   assertE9_AllowAndDenyInitiator,
	"TestAgent_ExpandVolume":            assertE9_ExpandVolume,
	"TestAgent_GetCapacity":             assertE9_GetCapacity,
	"TestAgent_ReconcileState":          assertE9_ReconcileState,
	// ── E11: Volume expansion ─────────────────────────────────────────────────
	"TestCSIExpand_ControllerExpandVolume":          assertE11_ControllerExpandVolume,
	"TestCSIExpand_NodeExpandVolume":                assertE11_NodeExpandVolume,
	"TestCSIExpand_ControllerExpandVolume_AgentErr": assertE11_ControllerExpandVolume_AgentErr,
	"TestCSIExpand_NodeExpandVolume_ResizerErr":     assertE11_NodeExpandVolume_ResizerErr,
	"TestCSIExpand_ControllerExpand_Idempotency":    assertE11_ControllerExpand_Idempotency,
	"TestCSIExpand_NodeExpand_XFS":                  assertE11_NodeExpand_XFS,
	"TestCSIExpand_NodeExpand_EmptyPath":            assertE11_NodeExpand_EmptyPath,
	"TestCSIExpand_NodeExpand_MissingVolumeID":      assertE11_NodeExpand_MissingVolumeID,
	// ── E12: ValidateVolumeCapabilities ───────────────────────────────────────
	"TestCSISnapshot_NotImplemented":             assertE12_NotImplemented,
	"TestCSISnapshot_CreateReturnsUnimplemented": assertE12_CreateReturnsUnimplemented,
	"TestCSISnapshot_DeleteReturnsUnimplemented": assertE12_DeleteReturnsUnimplemented,
	"TestCSISnapshot_ListReturnsUnimplemented":   assertE12_ListReturnsUnimplemented,
	// ── E13: Volume content source / clone ────────────────────────────────────
	"TestCSIClone_CreateVolume_ContentSource": assertE13_CreateVolume_ContentSource,
	"TestCSIClone_DeleteVolume_CloneSource":   assertE13_DeleteVolume_CloneSource,
	// ── E14: Invalid inputs / edge cases ──────────────────────────────────────
	"TestCSIEdge_CreateVolume_EmptyName":            assertE14_CreateVolume_EmptyName,
	"TestCSIEdge_CreateVolume_NilCapabilities":      assertE14_CreateVolume_NilCapabilities,
	"TestCSIEdge_CreateVolume_UnknownParam":         assertE14_CreateVolume_UnknownParam,
	"TestCSIEdge_DeleteVolume_EmptyID":              assertE14_DeleteVolume_EmptyID,
	"TestCSIEdge_NodeStageVolume_EmptyID":           assertE14_NodeStageVolume_EmptyID,
	"TestCSIEdge_NodeStageVolume_EmptyPath":         assertE14_NodeStageVolume_EmptyPath,
	"TestCSIEdge_NodePublishVolume_EmptyID":         assertE14_NodePublishVolume_EmptyID,
	"TestCSIEdge_NodePublishVolume_EmptyTargetPath": assertE14_NodePublishVolume_EmptyTargetPath,
	"TestCSIEdge_NodeUnpublishVolume_EmptyID":       assertE14_NodeUnpublishVolume_EmptyID,
	"TestCSIEdge_NodeUnstageVolume_EmptyID":         assertE14_NodeUnstageVolume_EmptyID,
	"TestCSIEdge_GetCapacity_NoParams":              assertE14_GetCapacity_NoParams,
	"TestCSIEdge_GetCapacity_UnknownTarget":         assertE14_GetCapacity_UnknownTarget,
	"TestCSIEdge_ControllerPublish_EmptyID":         assertE14_ControllerPublish_EmptyID,
	"TestCSIEdge_ControllerUnpublish_EmptyID":       assertE14_ControllerUnpublish_EmptyID,
	"TestCSIEdge_ValidateVolumeCapabilities":        assertE14_ValidateVolumeCapabilities,
	// ── E15: Resource exhaustion ──────────────────────────────────────────────
	"TestCSIExhaustion_CreateVolume_AgentFullDisk":   assertE15_CreateVolume_AgentFullDisk,
	"TestCSIExhaustion_CreateVolume_Timeout":         assertE15_CreateVolume_Timeout,
	"TestCSIExhaustion_ExpandVolume_ExceedsCapacity": assertE15_ExpandVolume_ExceedsCapacity,
	"TestCSIExhaustion_GetCapacity_AgentErr":         assertE15_GetCapacity_AgentErr,
	"TestCSIExhaustion_DeleteVolume_AgentErr":        assertE15_DeleteVolume_AgentErr,
	"TestCSIExhaustion_ControllerPublish_AgentErr":   assertE15_ControllerPublish_AgentErr,
	// ── E16: Concurrent operations ────────────────────────────────────────────
	"TestCSIConcurrent_CreateVolume_SameName":       assertE16_CreateVolume_SameName,
	"TestCSIConcurrent_CreateVolume_DifferentNames": assertE16_CreateVolume_DifferentNames,
	"TestCSIConcurrent_DeleteVolume":                assertE16_DeleteVolume,
	"TestCSIConcurrent_ExpandVolume":                assertE16_ExpandVolume,
	"TestCSIConcurrent_NodeStage":                   assertE16_NodeStage,
	"TestCSIConcurrent_NodePublish":                 assertE16_NodePublish,
	"TestCSIConcurrent_ControllerPublish":           assertE16_ControllerPublish,
	// ── E17: Cleanup validation ───────────────────────────────────────────────
	"TestCSICleanup_NodeUnstageRemovesState":         assertE17_NodeUnstageRemovesState,
	"TestCSICleanup_DeleteVolumeRemovesCRD":          assertE17_DeleteVolumeRemovesCRD,
	"TestCSICleanup_NodeUnpublishUnmounts":           assertE17_NodeUnpublishUnmounts,
	"TestCSICleanup_ControllerUnpublishDeniesAccess": assertE17_ControllerUnpublishDeniesAccess,
	"TestCSICleanup_NodeStageStateFileCreated":       assertE17_NodeStageStateFileCreated,
	"TestCSICleanup_NodeUnstageStateFileRemoved":     assertE17_NodeUnstageStateFileRemoved,
	"TestCSICleanup_NodeExpandVolumeResizesFS":       assertE17_NodeExpandVolumeResizesFS,
	"TestCSICleanup_MultiVolumeIsolation":            assertE17_MultiVolumeIsolation,
	// ── E18: Agent down error scenarios ──────────────────────────────────────
	"TestCSIController_CreateVolume_AgentUnreachable": assertE18_CreateVolume_AgentUnreachable,
	"TestCSIErrors_CreateVolume_AgentInternal":        assertE18_CreateVolume_AgentInternal,
	"TestCSIErrors_DeleteVolume_AgentInternal":        assertE18_DeleteVolume_AgentInternal,
	"TestCSIErrors_ControllerPublish_AgentInternal":   assertE18_ControllerPublish_AgentInternal,
	"TestCSIErrors_ExpandVolume_AgentInternal":        assertE18_ExpandVolume_AgentInternal,
	"TestAgent_ReconcileState_PartialExport":          assertE18_ReconcileState_PartialExport,
	// ── E21: Invalid CR error scenarios (in-process portion) ─────────────────
	"TestCSIInvalidCR_MissingPool":         assertE21_MissingPool,
	"TestCSIInvalidCR_MissingTarget":       assertE21_MissingTarget,
	"TestCSIInvalidCR_MissingProtocol":     assertE21_MissingProtocol,
	"TestCSIInvalidCR_InvalidBackendType":  assertE21_InvalidBackendType,
	"TestCSIInvalidCR_EmptyTargetAddress":  assertE21_EmptyTargetAddress,
	"TestCSIInvalidCR_TargetAddressFormat": assertE21_TargetAddressFormat,
	// ── E22: Access mode matrix / incompatible backend-protocol ───────────────
	"TestCSIProtocol_CreateVolume_UnsupportedProtocol":         assertE22_CreateVolume_UnsupportedProtocol,
	"TestCSIProtocol_CreateVolume_NVMeOF_TCP":                  assertE22_CreateVolume_NVMeOF_TCP,
	"TestCSIProtocol_ControllerPublish_ProtocolMismatch":       assertE22_ControllerPublish_ProtocolMismatch,
	"TestCSIProtocol_CreateVolume_iSCSI":                       assertE22_CreateVolume_iSCSI,
	"TestAgentErrors_Export_InvalidProtocol":                   assertE22_AgentErrors_Export_InvalidProtocol,
	"TestAgentErrors_Unexport_InvalidProtocol":                 assertE22_AgentErrors_Unexport_InvalidProtocol,
	"TestAgentProtocol_AllowInitiator_InvalidProtocol":         assertE22_AgentProtocol_AllowInitiator_InvalidProtocol,
	"TestAgentProtocol_DenyInitiator_InvalidProtocol":          assertE22_AgentProtocol_DenyInitiator_InvalidProtocol,
	"TestCSIProtocol_CreateVolume_UnknownBackendType_Rejected": assertE22_CreateVolume_UnknownBackendType_Rejected,
	"TestCSIProtocol_CreateVolume_UnknownBackendType_Error":    assertE22_CreateVolume_UnknownBackendType_Error,
	"TestCSIProtocol_CreateVolume_LVMBackend_NVMeOF":           assertE22_CreateVolume_LVMBackend_NVMeOF,
	"TestCSIProtocol_CreateVolume_LVMBackend_iSCSI":            assertE22_CreateVolume_LVMBackend_iSCSI,
	// ── E28: LVM Agent gRPC ───────────────────────────────────────────────────
	"TestAgentLVM_CreateVolume":                     assertE28_LVM_CreateVolume,
	"TestAgentLVM_DeleteVolume":                     assertE28_LVM_DeleteVolume,
	"TestAgentLVM_CreateVolume_Idempotency":         assertE28_LVM_CreateVolume_Idempotency,
	"TestAgentLVM_CreateVolume_Conflict":            assertE28_LVM_CreateVolume_Conflict,
	"TestAgentLVM_ExpandVolume":                     assertE28_LVM_ExpandVolume,
	"TestAgentLVM_Capacity":                         assertE28_LVM_Capacity,
	"TestAgentLVM_ListVolumes":                      assertE28_LVM_ListVolumes,
	"TestAgentLVM_ExportVolume_NVMeOF":              assertE28_LVM_ExportVolume_NVMeOF,
	"TestAgentLVM_ExportVolume_Idempotency":         assertE28_LVM_ExportVolume_Idempotency,
	"TestAgentLVM_UnexportVolume":                   assertE28_LVM_UnexportVolume,
	"TestAgentLVM_AllowInitiator":                   assertE28_LVM_AllowInitiator,
	"TestAgentLVM_DenyInitiator":                    assertE28_LVM_DenyInitiator,
	"TestAgentLVM_FullLifecycle":                    assertE28_LVM_FullLifecycle,
	"TestAgentLVM_CreateVolume_BackendError":        assertE28_LVM_CreateVolume_BackendError,
	"TestAgentLVM_DeleteVolume_NotFound":            assertE28_LVM_DeleteVolume_NotFound,
	"TestAgentLVM_ExpandVolume_BackendError":        assertE28_LVM_ExpandVolume_BackendError,
	"TestAgentLVM_GetCapacity":                      assertE28_LVM_GetCapacity,
	"TestAgentLVM_GetCapacity_Error":                assertE28_LVM_GetCapacity_Error,
	"TestAgentLVM_ExportVolume_Configfs":            assertE28_LVM_ExportVolume_Configfs,
	"TestAgentLVM_AllowInitiator_Configfs":          assertE28_LVM_AllowInitiator_Configfs,
	"TestAgentLVM_DenyInitiator_Configfs":           assertE28_LVM_DenyInitiator_Configfs,
	"TestAgentLVM_UnexportVolume_Configfs":          assertE28_LVM_UnexportVolume_Configfs,
	"TestAgentLVM_CreateVolume_LVMParams":           assertE28_LVM_CreateVolume_LVMParams,
	"TestAgentLVM_MultiVolume_Isolation":            assertE28_LVM_MultiVolume_Isolation,
	"TestAgentLVM_CreateVolume_Capacity_Check":      assertE28_LVM_CreateVolume_Capacity_Check,
	"TestAgentLVM_ExpandVolume_NotFound":            assertE28_LVM_ExpandVolume_NotFound,
	"TestAgentLVM_HealthCheck":                      assertE28_LVM_HealthCheck,
	"TestAgentLVM_ExportVolume_iSCSI_Unimplemented": assertE28_LVM_ExportVolume_iSCSI_Unimplemented,
	"TestAgentLVM_ExportVolume_NVMeOF_DeviceCheck":  assertE28_LVM_ExportVolume_NVMeOF_DeviceCheck,
	"TestAgentLVM_Concurrent_CreateVolume":          assertE28_LVM_Concurrent_CreateVolume,
	"TestAgentLVM_Concurrent_ExportVolume":          assertE28_LVM_Concurrent_ExportVolume,
	// ── E29: CSI Controller LVM parameter propagation ─────────────────────────
	"TestCSIController_LVM_CreateVolume":                         assertE29_LVM_CreateVolume,
	"TestCSIController_LVM_ProvisioningMode_Override":            assertE29_LVM_ProvisioningMode_Override,
	"TestCSIController_LVM_CreateVolume_BackendParams":           assertE29_LVM_CreateVolume_BackendParams,
	"TestCSIController_LVM_DeleteVolume":                         assertE29_LVM_DeleteVolume,
	"TestCSIController_LVM_ExpandVolume":                         assertE29_LVM_ExpandVolume,
	"TestCSIController_LVM_GetCapacity":                          assertE29_LVM_GetCapacity,
	"TestCSIController_LVM_CreateVolume_InvalidProvisioningMode": assertE29_LVM_CreateVolume_InvalidProvisioningMode,
	"TestCSIController_LVM_VolumeIDFormat":                       assertE29_LVM_VolumeIDFormat,
	"TestCSIController_LVM_CreateVolume_MissingVG":               assertE29_LVM_CreateVolume_MissingVG,
	"TestCSIController_LVM_ThinPool_Override":                    assertE29_LVM_ThinPool_Override,
	"TestCSIController_LVM_Stripe_Config":                        assertE29_LVM_Stripe_Config,
	"TestCSIController_LVM_Tags_Propagation":                     assertE29_LVM_Tags_Propagation,
	// ── E30: LVM LV no-duplication ────────────────────────────────────────────
	"TestCSIController_LVM_NoDup_ExportFailureRetry": assertE30_LVM_NoDup_ExportFailureRetry,
	"TestCSIController_LVM_NoDup_DeleteAfterPartial": assertE30_LVM_NoDup_DeleteAfterPartial,
	"TestCSIController_LVM_NoDup_MultipleRetries":    assertE30_LVM_NoDup_MultipleRetries,
	// ── E24: 8-stage full lifecycle integration ───────────────────────────────
	// E24.1: Normal path — complete 8-stage chain
	"TestCSILifecycle_FullCycle":             assertE24_FullCycle,
	"TestCSILifecycle_VolumeContextFlowThrough": assertE24_VolumeContextFlowThrough,
	"TestCSILifecycle_OrderingConstraints":   assertE24_OrderingConstraints,
	"TestCSILifecycle_IdempotentSteps":       assertE24_IdempotentSteps,
	// E24.2: CreateVolume stage failure/recovery
	"TestCSIController_PartialFailure_CRDCreatedOnExportFailure":     assertE24_PartialFailure_CRDCreatedOnExportFailure,
	"TestCSIController_PartialFailure_RetryAdvancesToReady":          assertE24_PartialFailure_RetryAdvancesToReady,
	"TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry": assertE24_PartialFailure_AgentCreateVolumeCalledOnceOnRetry,
	// E24.3: ControllerPublish stage failure (E24.3-2 reuses E2 assertion)
	"TestCSIController_ControllerPublishVolume_AgentAllowInitiatorFails": assertE24_ControllerPublishVolume_AgentAllowInitiatorFails,
	// E24.4: NodeStage stage failure/recovery
	"TestCSINode_NodeStageVolume_ConnectFails":    assertE24_NodeStageVolume_ConnectFails,
	"TestCSINode_NodeStageVolume_FormatFails":     assertE24_NodeStageVolume_FormatFails,
	"TestCSINode_NodeStageVolume_IdempotentReStage": assertE24_NodeStageVolume_IdempotentReStage,
	// E24.5: NodePublish stage failure (E24.5-2 and E24.5-3 reuse E7 assertions)
	"TestCSINode_NodePublishVolume_MountFails": assertE24_NodePublishVolume_MountFails,
	// E24.6: NodeUnpublish stage failure/recovery
	"TestCSINode_NodeUnpublishVolume_UnmountFails":     assertE24_NodeUnpublishVolume_UnmountFails,
	"TestCSINode_NodeUnpublishVolume_AlreadyUnpublished": assertE24_NodeUnpublishVolume_AlreadyUnpublished,
	// E24.7: NodeUnstage stage failure/recovery
	"TestCSINode_NodeUnstageVolume_DisconnectFails": assertE24_NodeUnstageVolume_DisconnectFails,
	"TestCSINode_NodeUnstageVolume_AlreadyUnstaged": assertE24_NodeUnstageVolume_AlreadyUnstaged,
	// E24.8: ControllerUnpublish stage failure/recovery
	"TestCSIController_ControllerUnpublishVolume_AgentDenyInitiatorFails": assertE24_ControllerUnpublishVolume_AgentDenyInitiatorFails,
	"TestCSIController_ControllerUnpublishVolume_NotFound":                assertE24_ControllerUnpublishVolume_NotFound,
	// E24.9: DeleteVolume stage failure (E24.9-2, E24.9-3, E24.9-4 reuse E6 assertions)
	"TestCSIController_DeleteVolume_AgentDeleteVolumeFailsTransient": assertE24_DeleteVolume_AgentDeleteVolumeFailsTransient,
	// E24.10: Aborted lifecycle cleanup paths
	"TestCSILifecycle_OutOfOrderOperationsDetected":  assertE24_OutOfOrderOperationsDetected,
	"TestCSIController_DeleteVolume_NonExistentVolume": assertE24_DeleteVolume_NonExistentVolume,
}

// runInProcessTCBody executes the assertion body for an in-process TC.
//
// Strategy:
//  1. If tc.TestName is in inProcessAssertions, call the specific assertion.
//  2. Otherwise, fall back to the cached verifier (sync.Once per verifier).
//
// The per-TC assertion functions create fresh isolated environments for each
// TC, providing true per-TC isolation. The cached verifier fallback is used
// for TCs that share the same shared verification path.
func runInProcessTCBody(tc documentedCase, plan localExecutionPlan) {
	if fn, ok := inProcessAssertions[tc.TestName]; ok {
		fn(tc)
		return
	}
	// Fallback: use cached verifier for TCs not yet individually implemented.
	for _, verifierName := range plan.Verifiers {
		result := defaultLocalVerifierRegistry.Result(verifierName)
		Expect(result.Err).NotTo(HaveOccurred(),
			"%s[%s] FAIL: in-process verifier %q failed after %s: %v",
			tc.tcNodeLabel(), tc.SectionTitle, verifierName, result.Duration, result.Err,
		)
	}
}
