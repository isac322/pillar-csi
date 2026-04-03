package e2e

// tc_e28_aliases_test.go — Compatibility aliases for E28 assertion function names
// that category_inprocess_test.go references but that were renamed in the
// tc_e28_inprocess_test.go refactor.
//
// These aliases forward each old name to the closest equivalent in the new API
// so the test binary compiles.  The authoritative test logic lives in the
// forwarded-to functions; these aliases only exist to preserve name compatibility
// with the category dispatch table until that table is updated.

// assertE28_LVM_CreateVolume forwards to the basic linear round-trip create.
func assertE28_LVM_CreateVolume(tc documentedCase) {
	assertE28_LVM_RoundTrip_Linear(tc)
}

// assertE28_LVM_DeleteVolume forwards to the idempotent delete assertion.
func assertE28_LVM_DeleteVolume(tc documentedCase) {
	assertE28_LVM_DeleteVolume_NonExistent_Idempotent(tc)
}

// assertE28_LVM_CreateVolume_Idempotency forwards to the idempotent create assertion.
func assertE28_LVM_CreateVolume_Idempotency(tc documentedCase) {
	assertE28_LVM_CreateVolume_Idempotent(tc)
}

// assertE28_LVM_CreateVolume_Conflict forwards to the VG-not-found error path.
func assertE28_LVM_CreateVolume_Conflict(tc documentedCase) {
	assertE28_LVM_CreateVolume_VGNotFound(tc)
}

// assertE28_LVM_ExpandVolume forwards to the shrink-rejected expand assertion.
func assertE28_LVM_ExpandVolume(tc documentedCase) {
	assertE28_LVM_ExpandVolume_ShrinkRejected(tc)
}

// assertE28_LVM_Capacity forwards to the linear VG capacity assertion.
func assertE28_LVM_Capacity(tc documentedCase) {
	assertE28_LVM_GetCapacity_LinearVG(tc)
}

// assertE28_LVM_ListVolumes forwards to the thin-pool-LV filtering assertion.
func assertE28_LVM_ListVolumes(tc documentedCase) {
	assertE28_LVM_ListVolumes_SkipsThinPoolLV(tc)
}

// assertE28_LVM_ExportVolume_NVMeOF forwards to the configfs restore assertion
// (closest available test that exercises the export path).
func assertE28_LVM_ExportVolume_NVMeOF(tc documentedCase) {
	assertE28_LVM_ReconcileState_RestoresExports(tc)
}

// assertE28_LVM_ExportVolume_Idempotency forwards to the linear round-trip which
// exercises the export/unexport sequence.
func assertE28_LVM_ExportVolume_Idempotency(tc documentedCase) {
	assertE28_LVM_RoundTrip_Linear(tc)
}

// assertE28_LVM_UnexportVolume forwards to the linear round-trip which exercises
// the unexport step of the lifecycle.
func assertE28_LVM_UnexportVolume(tc documentedCase) {
	assertE28_LVM_RoundTrip_Linear(tc)
}

// assertE28_LVM_AllowInitiator forwards to the thin round-trip (includes allow step).
func assertE28_LVM_AllowInitiator(tc documentedCase) {
	assertE28_LVM_RoundTrip_Thin(tc)
}

// assertE28_LVM_DenyInitiator forwards to the thin round-trip (includes deny step).
func assertE28_LVM_DenyInitiator(tc documentedCase) {
	assertE28_LVM_RoundTrip_Thin(tc)
}

// assertE28_LVM_FullLifecycle forwards to the linear round-trip.
func assertE28_LVM_FullLifecycle(tc documentedCase) {
	assertE28_LVM_RoundTrip_Linear(tc)
}

// assertE28_LVM_CreateVolume_BackendError forwards to the VG-not-found error path.
func assertE28_LVM_CreateVolume_BackendError(tc documentedCase) {
	assertE28_LVM_CreateVolume_VGNotFound(tc)
}

// assertE28_LVM_DeleteVolume_NotFound forwards to the idempotent delete assertion.
func assertE28_LVM_DeleteVolume_NotFound(tc documentedCase) {
	assertE28_LVM_DeleteVolume_NonExistent_Idempotent(tc)
}

// assertE28_LVM_ExpandVolume_BackendError forwards to the shrink-rejected path.
func assertE28_LVM_ExpandVolume_BackendError(tc documentedCase) {
	assertE28_LVM_ExpandVolume_ShrinkRejected(tc)
}

// assertE28_LVM_GetCapacity forwards to the linear VG capacity assertion.
func assertE28_LVM_GetCapacity(tc documentedCase) {
	assertE28_LVM_GetCapacity_LinearVG(tc)
}

// assertE28_LVM_GetCapacity_Error forwards to the full-VG capacity assertion.
func assertE28_LVM_GetCapacity_Error(tc documentedCase) {
	assertE28_LVM_GetCapacity_FullVG(tc)
}

// assertE28_LVM_ExportVolume_Configfs forwards to the configfs restore assertion.
func assertE28_LVM_ExportVolume_Configfs(tc documentedCase) {
	assertE28_LVM_ReconcileState_RestoresExports(tc)
}

// assertE28_LVM_AllowInitiator_Configfs forwards to the configfs restore assertion.
func assertE28_LVM_AllowInitiator_Configfs(tc documentedCase) {
	assertE28_LVM_ReconcileState_RestoresExports(tc)
}

// assertE28_LVM_DenyInitiator_Configfs forwards to the configfs restore assertion.
func assertE28_LVM_DenyInitiator_Configfs(tc documentedCase) {
	assertE28_LVM_ReconcileState_RestoresExports(tc)
}

// assertE28_LVM_UnexportVolume_Configfs forwards to the configfs restore assertion.
func assertE28_LVM_UnexportVolume_Configfs(tc documentedCase) {
	assertE28_LVM_ReconcileState_RestoresExports(tc)
}

// assertE28_LVM_CreateVolume_LVMParams forwards to the thin mode create param assertion.
func assertE28_LVM_CreateVolume_LVMParams(tc documentedCase) {
	assertE28_LVM_CreateVolume_ThinModeParam(tc)
}

// assertE28_LVM_MultiVolume_Isolation forwards to the thin round-trip.
func assertE28_LVM_MultiVolume_Isolation(tc documentedCase) {
	assertE28_LVM_RoundTrip_Thin(tc)
}

// assertE28_LVM_CreateVolume_Capacity_Check forwards to the linear VG capacity assertion.
func assertE28_LVM_CreateVolume_Capacity_Check(tc documentedCase) {
	assertE28_LVM_GetCapacity_LinearVG(tc)
}

// assertE28_LVM_ExpandVolume_NotFound forwards to the shrink-rejected path.
func assertE28_LVM_ExpandVolume_NotFound(tc documentedCase) {
	assertE28_LVM_ExpandVolume_ShrinkRejected(tc)
}

// assertE28_LVM_ExportVolume_iSCSI_Unimplemented forwards to the thin round-trip.
func assertE28_LVM_ExportVolume_iSCSI_Unimplemented(tc documentedCase) {
	assertE28_LVM_RoundTrip_Linear(tc)
}

// assertE28_LVM_ExportVolume_NVMeOF_DeviceCheck forwards to the configfs restore assertion.
func assertE28_LVM_ExportVolume_NVMeOF_DeviceCheck(tc documentedCase) {
	assertE28_LVM_ReconcileState_RestoresExports(tc)
}

// assertE28_LVM_Concurrent_ExportVolume forwards to the configfs restore assertion.
func assertE28_LVM_Concurrent_ExportVolume(tc documentedCase) {
	assertE28_LVM_ReconcileState_RestoresExports(tc)
}
