# Unit Tests — 순수 로직 검증

순수 함수/모듈 로직을 검증한다. 외부 의존성(K8s API, gRPC, 커널 모듈) 없이
입출력만으로 정확성을 판단할 수 있는 테스트.

**실행:** `go test ./test/unit/ -v`
**빌드 태그:** 없음
**CI:** ✅ 표준 CI 실행 가능 (Go 빌드 도구체인만 필요)

---

## 목차

| 섹션 | TC 수 | 검증 대상 |
|------|-------|----------|
| [E1.6 접근 모드 유효성 검증](#e16-접근-모드-유효성-검증-access-mode-validation) | 8 | `isSupportedAccessMode` 함수 |
| [E1.7 용량 범위 검증](#e17-용량-범위-검증-capacity-range-validation) | 7 | `RequiredBytes ≤ LimitBytes` 산술 |
| [E1.11 VolumeId 형식 및 파라미터 검증](#e111-volumeid-형식-및-파라미터-검증) | 6 | `strings.SplitN(id, "/", 4)` 파싱 |
| [E2.6 입력 검증 (게시 단계)](#e26-입력-검증--게시-단계-input-validation-subset) | 4 | 빈 VolumeID/NodeID/Capability/MalformedID |
| [E5 순서 제약](#e5-순서-제약-ordering-constraints) | 6 | VolumeStateMachine 상태 전이 |
| [E12 스냅샷 미구현](#e12-csi-스냅샷-미구현-unimplemented) | 4 | Unimplemented gRPC 코드 |
| [E13 클론 미구현](#e13-볼륨-클론-미구현-unimplemented) | 2 | VolumeContentSource 무시 동작 |
| [E14 잘못된 입력값 및 엣지 케이스](#e14-잘못된-입력값-및-엣지-케이스-invalid-inputs--edge-cases) | 15 | 입력 검증, 경계값 |
| [E22 비호환 백엔드-프로토콜](#e22-비호환-백엔드-프로토콜-오류-시나리오-incompatible-backend-protocol) | 12 | 호환성 매트릭스, 미지원 타입 |
| [NEW-U1 addressSelector CIDR 매칭](#new-u1-addressselector-cidr-매칭) | 4 | `resolveAddress` CIDR 필터링 |
| [NEW-U2 CSI Topology Capability 미선언](#new-u2-csi-topology-capability-미선언) | 1 | GetPluginCapabilities topology |
| [NEW-U3 구조화된 로깅 형식](#new-u3-구조화된-로깅-형식-json-slog) | 2 | slog JSON 출력 형식 |
| [NEW-U4 NQN 형식 생성](#new-u4-nqn-형식-생성) | 2 | GenerateNQN 포맷/길이 |
| [NEW-U5 NQN/IQN 형식 검증](#new-u5-nqniqn-형식-검증-getinitiatorid용) | 3 | IsValidNQN/IsValidIQN |
| **합계** | **76** | |

---

## E1.6 접근 모드 유효성 검증 (Access Mode Validation)

> **Unit test 근거:** `isSupportedAccessMode` 함수는 CSI `AccessMode` 열거값을 받아
> 불리언을 반환하는 순수 함수다. 외부 상태(K8s, agent) 없이 입력-출력만으로 정확성을 판단할 수 있다.

접근 모드 검증은 `CreateVolume` 진입 시점에서 수행되며, 요청된 모든 `VolumeCapability`의
`AccessMode`가 드라이버가 지원하는 모드인지 확인한다 (`controller.go: isSupportedAccessMode`).

**pillar-csi 지원 접근 모드:**

| CSI 상수 | Kubernetes PVC accessMode | 설명 |
|----------|--------------------------|------|
| `SINGLE_NODE_WRITER` | ReadWriteOnce (RWO) | 단일 노드 읽기-쓰기 |
| `SINGLE_NODE_SINGLE_WRITER` | ReadWriteOncePod (RWOP) | 단일 파드 읽기-쓰기 (CSI spec v1.5+) |
| `MULTI_NODE_READER_ONLY` | ReadOnlyMany (ROX) | 다중 노드 읽기 전용 |

**pillar-csi 미지원 접근 모드:**
- `MULTI_NODE_MULTI_WRITER` (ReadWriteMany / RWX) — NVMe-oF 블록 장치는 다중 쓰기 조정 미지원

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.6-1 | `TestCSIController_CreateVolume_AccessMode_RWO` | SINGLE_NODE_WRITER(RWO) 접근 모드로 CreateVolume 성공 | PillarTarget="storage-1" fake 클라이언트에 등록; mockAgentServer 정상; pool="tank"(PillarPool CRD) | 1) AccessMode=SINGLE_NODE_WRITER, VolumeCapabilities 포함 CreateVolumeRequest 전송 | 성공 (gRPC OK); VolumeId/VolumeContext 반환; agent.CreateVolume 1회, agent.ExportVolume 1회 호출 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| E1.6-2 | `TestCSIController_CreateVolume_AccessMode_RWOP` | SINGLE_NODE_SINGLE_WRITER(RWOP) 접근 모드로 CreateVolume 성공 | 위와 동일 | 1) AccessMode=SINGLE_NODE_SINGLE_WRITER로 CreateVolumeRequest 전송 | 성공; VolumeId/VolumeContext 반환; agent 호출 정상 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| E1.6-3 | `TestCSIController_CreateVolume_AccessMode_ROX` | MULTI_NODE_READER_ONLY(ROX) 접근 모드로 CreateVolume 성공 | 위와 동일 | 1) AccessMode=MULTI_NODE_READER_ONLY로 CreateVolumeRequest 전송 | 성공; VolumeId/VolumeContext 반환; agent 호출 정상 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| E1.6-4 | `TestCSIController_CreateVolume_AccessMode_RWX_Rejected` | MULTI_NODE_MULTI_WRITER(RWX) 접근 모드는 드라이버 수준에서 거부 | ControllerServer 초기화만 필요; PillarTarget/agent 연결 불필요 | 1) AccessMode=MULTI_NODE_MULTI_WRITER로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "unsupported access mode" 메시지 포함; agent.CreateVolume 호출 없음 | `CSI-C` |
| E1.6-5 | `TestCSIController_CreateVolume_AccessMode_Unknown_Rejected` | 정의되지 않은(UNKNOWN=0) 접근 모드는 거부 | ControllerServer 초기화만 필요 | 1) AccessMode=UNKNOWN(0)으로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| E1.6-6 | `TestCSIController_CreateVolume_AccessMode_Missing_InCapability` | VolumeCapability에 AccessMode 필드 자체가 없으면 InvalidArgument | ControllerServer 초기화만 필요 | 1) VolumeCapability{AccessMode: nil}로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "must specify an access_mode" 메시지; agent 호출 없음 | `CSI-C` |
| E1.6-7 | `TestCSIController_CreateVolume_VolumeCapabilities_Empty` | VolumeCapabilities가 빈 슬라이스이면 InvalidArgument | ControllerServer 초기화만 필요 | 1) VolumeCapabilities=[]로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "volume_capabilities must not be empty" 메시지; agent 호출 없음 | `CSI-C` |
| E1.6-8 | `TestCSIController_CreateVolume_MultipleCapabilities_AnyUnsupported` | 여러 VolumeCapability 중 하나라도 미지원 모드이면 전체 거부 | ControllerServer 초기화만 필요 | 1) [SINGLE_NODE_WRITER, MULTI_NODE_MULTI_WRITER] 두 개의 VolumeCapability를 포함한 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |

---

## E1.7 용량 범위 검증 (Capacity Range Validation)

> **Unit test 근거:** `RequiredBytes ≤ LimitBytes` 산술 비교와 기존 볼륨 용량 범위 충돌 판정은
> 순수 산술 로직이다. 외부 I/O 없이 입력 숫자만으로 결과를 결정할 수 있다.

`CapacityRange.RequiredBytes`와 `LimitBytes`는 CSI 명세상 선택 사항이다.
두 값이 모두 제공되면 `RequiredBytes ≤ LimitBytes`를 만족해야 한다.
기존 볼륨(PillarVolume CRD Ready 상태)과 용량 충돌 시 `AlreadyExists`를 반환한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.7-1 | `TestCSIController_CreateVolume_Capacity_NoRange` | CapacityRange 미지정 시 드라이버가 기본 크기 선택 | PillarTarget 등록; mockAgentServer 정상 (CapacityBytes=1GiB 에코) | 1) CapacityRange=nil로 CreateVolumeRequest 전송 | 성공; CapacityBytes ≥ 0 반환; agent.CreateVolume의 CapacityBytes=0 전달 | `CSI-C`, `Agent`, `gRPC` |
| E1.7-2 | `TestCSIController_CreateVolume_Capacity_RequiredOnly` | RequiredBytes만 지정, LimitBytes 미지정 | PillarTarget 등록; mockAgentServer가 요청된 용량을 그대로 반환 | 1) CapacityRange{RequiredBytes: 1GiB}로 CreateVolumeRequest 전송 | 성공; CapacityBytes = 1GiB | `CSI-C`, `Agent`, `gRPC` |
| E1.7-3 | `TestCSIController_CreateVolume_Capacity_LimitOnly` | LimitBytes만 지정, RequiredBytes 미지정 | PillarTarget 등록; mockAgentServer가 LimitBytes 이하 용량 반환 | 1) CapacityRange{LimitBytes: 2GiB}로 CreateVolumeRequest 전송 | 성공; CapacityBytes ≤ 2GiB | `CSI-C`, `Agent`, `gRPC` |
| E1.7-4 | `TestCSIController_CreateVolume_Capacity_ValidRange` | RequiredBytes=1GiB, LimitBytes=2GiB 동시 지정 (유효 범위) | PillarTarget 등록; mockAgentServer 정상 | 1) CapacityRange{RequiredBytes: 1GiB, LimitBytes: 2GiB}로 CreateVolumeRequest 전송 | 성공; 1GiB ≤ CapacityBytes ≤ 2GiB | `CSI-C`, `Agent`, `gRPC` |
| E1.7-5 | `TestCSIController_CreateVolume_Capacity_ExistingTooSmall` | 기존 볼륨(1GiB)이 새 RequiredBytes(2GiB)보다 작으면 AlreadyExists | PillarVolume CRD Ready 상태; CapacityBytes=1GiB; ExportInfo 존재 | 1) 동일 볼륨 이름으로 RequiredBytes=2GiB CreateVolumeRequest 전송 | gRPC AlreadyExists; "already exists with capacity … less than …" 메시지; agent 재호출 없음 | `CSI-C`, `VolCRD` |
| E1.7-6 | `TestCSIController_CreateVolume_Capacity_ExistingTooLarge` | 기존 볼륨(4GiB)이 새 LimitBytes(2GiB)보다 크면 AlreadyExists | PillarVolume CRD Ready 상태; CapacityBytes=4GiB; ExportInfo 존재 | 1) 동일 볼륨 이름으로 LimitBytes=2GiB CreateVolumeRequest 전송 | gRPC AlreadyExists; "already exists with capacity … exceeds …" 메시지; agent 재호출 없음 | `CSI-C`, `VolCRD` |
| E1.7-7 | `TestCSIController_CreateVolume_Capacity_ExistingWithinRange` | 기존 볼륨(2GiB)이 RequiredBytes=1GiB, LimitBytes=3GiB 범위 내이면 캐시 반환 | PillarVolume CRD Ready 상태; CapacityBytes=2GiB; ExportInfo 존재 | 1) 동일 볼륨 이름으로 CapacityRange{1GiB, 3GiB} CreateVolumeRequest 전송 | 성공; CapacityBytes=2GiB 반환 (캐시); agent 재호출 없음 | `CSI-C`, `VolCRD` |

---

## E1.11 VolumeId 형식 및 파라미터 검증

> **Unit test 근거:** VolumeId 파싱(`strings.SplitN(id, "/", 4)`)은 문자열 처리 순수 함수다.
> 필수 파라미터 존재 여부 검증도 맵 키 확인 로직에 불과하다. 외부 의존성 없이 테스트 가능하다.

**VolumeId 형식:** `<target-name>/<protocol-type>/<backend-type>/<agent-vol-id>`

ZFS zvol의 `agent-vol-id`는 `<pool>/<volume-name>` 형식(PillarPool CRD의 pool 이름)이므로 전체 VolumeId에
슬래시가 5개 포함될 수 있다. `strings.SplitN(id, "/", 4)` 로 정확히 4 파트로 분리한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.11-1 | `TestCSIController_CreateVolume_VolumeID_ZFSPoolWithSlash` | ZFS pool 이름에 슬래시 포함 시 agent-vol-id 파싱 정확성 | pool="tank"(PillarPool CRD); volume-name="pvc-abc" | 1) CreateVolumeRequest 전송; 2) 반환된 VolumeId로 DeleteVolumeRequest 전송 | CreateVolume: VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc"; DeleteVolume: agent-vol-id="tank/pvc-abc" 정확 파싱 | `CSI-C`, `Agent`, `gRPC` |
| E1.11-2 | `TestCSIController_CreateVolume_VolumeID_ZFSParentDataset` | ZFS parent dataset 파라미터 설정 시 agent-vol-id에 반영 | pool="tank"(PillarPool CRD); zfs-parent-dataset="volumes"; volume-name="pvc-abc" | 1) CreateVolumeRequest 전송 | agent-vol-id="tank/volumes/pvc-abc"; VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/volumes/pvc-abc" | `CSI-C`, `Agent`, `gRPC` |
| E1.11-3 | `TestCSIController_CreateVolume_MissingVolumeName` | 볼륨 이름이 빈 문자열이면 InvalidArgument | ControllerServer 초기화만 필요 | 1) Name=""로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "volume name is required" 메시지; agent 호출 없음 | `CSI-C` |
| E1.11-4 | `TestCSIController_CreateVolume_MissingTargetParam` | StorageClass parameter에 target 키 없으면 InvalidArgument | ControllerServer 초기화; Parameters에서 `pillar-csi.bhyoo.com/target` 제거 | 1) target 파라미터 없는 CreateVolumeRequest 전송 | gRPC InvalidArgument; "parameter … is required" 메시지 | `CSI-C` |
| E1.11-5 | `TestCSIController_CreateVolume_MissingBackendTypeParam` | StorageClass parameter에 backend-type 키 없으면 InvalidArgument | ControllerServer 초기화; Parameters에서 `pillar-csi.bhyoo.com/backend-type` 제거 | 1) backend-type 파라미터 없는 CreateVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |
| E1.11-6 | `TestCSIController_CreateVolume_MissingProtocolTypeParam` | StorageClass parameter에 protocol-type 키 없으면 InvalidArgument | ControllerServer 초기화; Parameters에서 `pillar-csi.bhyoo.com/protocol-type` 제거 | 1) protocol-type 파라미터 없는 CreateVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |

---

## E2.6 입력 검증 — 게시 단계 (Input Validation Subset)

> **Unit test 근거:** ControllerPublishVolume 진입부의 필수 필드 존재 확인(빈 문자열, nil, malformed ID)은
> 외부 호출 이전에 수행되는 순수 입력 검증 로직이다. agent/K8s API 호출 없이 즉시 InvalidArgument를 반환한다.

E2.6 전체 8개 TC 중, agent나 K8s 조회 **이전에** 순수 입력 검증으로 거부되는 4개 TC만 포함한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E2.6-3 | `TestCSIController_ControllerPublishVolume_EmptyVolumeID` | VolumeId="" — agent 호출 전 입력 검증 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; VolumeId=""; 유효한 NodeId/VolumeCapability | 1) VolumeId=""로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument; AllowInitiator 0회 | `CSI-C` |
| E2.6-4 | `TestCSIController_ControllerPublishVolume_EmptyNodeID` | NodeId="" — agent 호출 전 입력 검증 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; 유효한 VolumeId; NodeId="" | 1) NodeId=""로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument; AllowInitiator 0회 | `CSI-C` |
| E2.6-5 | `TestCSIController_ControllerPublishVolume_NilVolumeCapability` | VolumeCapability=nil — 입력 검증 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; VolumeCapability=nil | 1) VolumeCapability=nil로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |
| E2.6-6 | `TestCSIController_ControllerPublishVolume_MalformedVolumeID` | VolumeId="badformat"(슬래시 없음) — VolumeId 파싱 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; VolumeId="badformat" | 1) VolumeId="badformat"로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |

---

## E5 순서 제약 (Ordering Constraints)

> **Unit test 근거:** `VolumeStateMachine`은 현재 상태와 요청된 전이만으로 허용/거부를 결정하는
> 순수 상태 머신이다. 외부 I/O 없이 상태 전이 규칙의 정확성을 검증할 수 있다.

공유 VolumeStateMachine을 통해 컨트롤러와 노드 서버 간 순서 제약을 검증한다.

### E5.1 역순 호출 거부

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 43 | `TestCSIOrdering_NodeStageBeforeControllerPublish` | ControllerPublish 없이 NodeStage 호출 → FailedPrecondition | 공유 SM 초기화; CreateVolume 완료; ControllerPublish 미호출 | 1) NodeStageVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |
| 44 | `TestCSIOrdering_NodePublishBeforeNodeStage` | NodeStage 없이 NodePublish 호출 → FailedPrecondition | 공유 SM; ControllerPublish 완료; NodeStage 미완료 | 1) NodePublishVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |
| 45 | `TestCSIOrdering_NodeUnstageBeforeNodeUnpublish` | NodeUnpublish 없이 NodeUnstage 호출 → FailedPrecondition | 공유 SM; NodePublish 완료; NodeUnpublish 미호출 | 1) NodeUnstageVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |
| 46 | `TestCSIOrdering_NodePublishAfterUnstage` | NodeUnstage 후 NodePublish 재시도 → FailedPrecondition | 공유 SM; 전체 정상 라이프사이클 완료 | 1) NodePublishVolumeRequest 재호출 | gRPC FailedPrecondition | `CSI-N`, `SM` |

### E5.2 정상 순서 통과

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 47 | `TestCSIOrdering_FullLifecycleWithSM` | 공유 SM을 사용한 전체 순서 라이프사이클 성공 | 공유 SM 초기화; mockConnector/mockMounter/mockAgentServer 준비 | 1) CreateVolume; 2) ControllerPublish; 3) NodeStage; 4) NodePublish; 5) NodeUnpublish; 6) NodeUnstage; 7) ControllerUnpublish; 8) DeleteVolume | 모든 단계 성공; SM 상태 전이 검증 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `SM`, `gRPC` |
| 48 | `TestCSIOrdering_IdempotencyWithSM` | 올바른 상태에서의 재호출은 순서 제약 위반 아님 | 공유 SM; 각 단계 정상 완료 | 1) 현재 상태에서 허용되는 RPC 재호출 | 성공; FailedPrecondition 미발생 | `CSI-C`, `CSI-N`, `SM` |

---

## E12 CSI 스냅샷 미구현 (Unimplemented)

> **Unit test 근거:** `UnimplementedControllerServer` 임베딩에 의한 자동 `Unimplemented` 반환은
> 구현 코드가 없는 상태에서의 기본 동작이다. 외부 의존성 없이 gRPC 상태 코드만 확인하면 된다.

pillar-csi는 현재 CSI VolumeSnapshot 역량을 구현하지 않는다.
`ControllerServer`는 `csi.UnimplementedControllerServer`를 임베드하므로
`CreateSnapshot`, `DeleteSnapshot`, `ListSnapshots` RPC는 자동으로
gRPC `Unimplemented` 상태를 반환한다.

### E12.1 미구현 스냅샷 RPC — Unimplemented 반환 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 96 | `TestCSISnapshot_CreateSnapshot_ReturnsUnimplemented` | CSI CreateSnapshot이 현재 미구현으로 gRPC Unimplemented를 반환 | ControllerServer 초기화 | 1) CreateSnapshotRequest(유효 파라미터) 전송 | gRPC Unimplemented; 에이전트 호출 없음 | `CSI-C` |
| 97 | `TestCSISnapshot_DeleteSnapshot_ReturnsUnimplemented` | CSI DeleteSnapshot이 현재 미구현으로 gRPC Unimplemented를 반환 | ControllerServer 초기화 | 1) DeleteSnapshotRequest(SnapshotId="storage-1/snap-test") 전송 | gRPC Unimplemented | `CSI-C` |
| 98 | `TestCSISnapshot_ListSnapshots_ReturnsUnimplemented` | CSI ListSnapshots이 현재 미구현으로 gRPC Unimplemented를 반환 | ControllerServer 초기화 | 1) ListSnapshotsRequest(빈 요청) 전송 | gRPC Unimplemented | `CSI-C` |

### E12.2 GetPluginCapabilities — 스냅샷 역량 미선언 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 99 | `TestCSISnapshot_PluginCapabilities_NoSnapshotCapability` | GetPluginCapabilities 응답에 VolumeSnapshot 역량이 포함되지 않음 | IdentityServer 초기화 | 1) GetPluginCapabilitiesRequest 전송; 2) 응답 역량 목록 검사 | VolumeExpansion_ONLINE은 있으나 스냅샷 관련 역량 없음 | `CSI-C` |

---

## E13 볼륨 클론 미구현 (Unimplemented)

> **Unit test 근거:** `VolumeContentSource` 필드를 무시하는 현재 동작은 코드 경로 고정(pinning) 테스트다.
> 외부 데이터 소스 없이, 입력에 VolumeContentSource가 있어도 빈 볼륨을 생성하는지만 확인한다.

CSI 명세의 `CreateVolume` 요청은 `VolumeContentSource` 필드를 통해
볼륨 클론이나 스냅샷 복원을 요청할 수 있다. 현재 pillar-csi는 이 필드를 파싱하지 않으며
무시한다 — 항상 빈 볼륨을 생성한다.

### E13.1 VolumeContentSource 미처리 동작 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 100 | `TestCSIClone_CreateVolume_SnapshotSourceIgnored` | VolumeContentSource.Snapshot이 포함된 CreateVolume 호출 시 스냅샷 소스를 무시하고 빈 볼륨을 생성 (현재 동작 고정 테스트) | PillarTarget 등록; mockAgentServer 정상 동작 | 1) VolumeContentSource.Snapshot="snap-A" 를 포함한 CreateVolumeRequest 전송 | CreateVolume 성공; agent.CreateVolume 1회 (VolumeContentSource 없이); 빈 볼륨 생성 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 101 | `TestCSIClone_CreateVolume_VolumeSourceIgnored` | VolumeContentSource.Volume이 포함된 CreateVolume 호출 시 소스 볼륨을 무시하고 빈 볼륨을 생성 (현재 동작 고정 테스트) | PillarTarget 등록; mockAgentServer 정상 동작 | 1) VolumeContentSource.Volume="src-pvc-id" 를 포함한 CreateVolumeRequest 전송 | CreateVolume 성공; 소스 데이터 복사 없이 빈 볼륨; agent.CreateVolume 1회 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

---

## E14 잘못된 입력값 및 엣지 케이스 (Invalid Inputs & Edge Cases)

> **Unit test 근거:** 입력 검증(빈 문자열, 경계값, 잘못된 타입 조합)은 외부 호출 이전에 수행되는
> 가드 로직이다. agent 호출 없이 gRPC 오류 코드만 반환하므로 순수 로직 테스트에 해당한다.

잘못된 입력, 경계값, 지원하지 않는 파라미터 조합이 CSI 명세에 맞는
오류 코드로 올바르게 거부되는지 검증한다. 각 케이스에서 agent 호출이
발생하지 않아야 하며, 서버 패닉이 없어야 한다.

### E14.1 VolumeId 형식 위반

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 102 | `TestCSIEdge_CreateVolume_ExtremelyLongVolumeName` | 극도로 긴 볼륨 이름(2048자)으로 CreateVolume 호출 | ControllerServer 초기화; PillarTarget 등록 | 1) name="pvc-"+2000자 문자열 로 CreateVolumeRequest 전송 | gRPC InvalidArgument 또는 성공; 패닉 없음 | `CSI-C` |
| 103 | `TestCSIEdge_CreateVolume_SpecialCharactersInName` | 볼륨 이름에 슬래시("/") 포함 — VolumeId 파싱 혼동 유발 시도 | ControllerServer 초기화; PillarTarget 등록 | 1) name="pvc/with/slashes" 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음; VolumeId 파싱 혼동 없음 | `CSI-C` |
| 104 | `TestCSIEdge_DeleteVolume_EmptyVolumeId` | 빈 VolumeId로 DeleteVolume 호출 | ControllerServer 초기화 | 1) VolumeId="" 로 DeleteVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 105 | `TestCSIEdge_ControllerPublish_EmptyNodeId` | NodeId가 빈 문자열인 ControllerPublishVolume | ControllerServer 초기화; 유효한 VolumeId/VolumeContext | 1) NodeId="" 로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument; agent.AllowInitiator 호출 없음 | `CSI-C` |

### E14.2 CapacityRange 경계값

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 106 | `TestCSIEdge_CreateVolume_LimitLessThanRequired` | LimitBytes < RequiredBytes로 CreateVolume | ControllerServer 초기화; PillarTarget 등록 | 1) CapacityRange(RequiredBytes=2GiB, LimitBytes=1GiB) 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 107 | `TestCSIEdge_ControllerExpand_ZeroRequiredBytes` | ControllerExpandVolume에서 RequiredBytes=0 | ControllerServer 초기화; 유효한 VolumeId | 1) CapacityRange(RequiredBytes=0, LimitBytes=0) 로 ControllerExpandVolumeRequest 전송 | gRPC InvalidArgument; agent.ExpandVolume 호출 없음 | `CSI-C` |
| 108 | `TestCSIEdge_ControllerExpand_ShrinkRequest` | 현재 크기보다 작은 RequiredBytes로 ControllerExpandVolume | mockAgentServer.ExpandVolumeErr에 "volsize cannot be decreased" 설정 | 1) ControllerExpandVolumeRequest 전송 | 비-OK gRPC 상태 (Internal) | `CSI-C`, `Agent`, `gRPC` |
| 109 | `TestCSIEdge_CreateVolume_ExactLimitEqualsRequired` | RequiredBytes == LimitBytes (경계값) 로 CreateVolume | PillarTarget 등록; mockAgentServer 정상 | 1) CapacityRange(RequiredBytes=LimitBytes=1GiB) 로 CreateVolumeRequest 전송 | 성공; agent.CreateVolume이 1GiB로 호출됨 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |

### E14.3 VolumeContext 값 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 110 | `TestCSIEdge_NodeStage_InvalidPort` | VolumeContext.port가 숫자가 아닌 문자열 | NodeServer 초기화 | 1) VolumeContext.port="not-a-port" 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connector.Connect 미호출 | `CSI-N` |
| 111 | `TestCSIEdge_NodeStage_EmptyNQN` | VolumeContext.target_id(NQN)가 빈 문자열 | NodeServer 초기화 | 1) VolumeContext.target_id="" 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connector.Connect 미호출 | `CSI-N` |
| 112 | `TestCSIEdge_NodeStage_MissingVolumeContext` | VolumeContext 자체가 nil인 NodeStageVolume | NodeServer 초기화 | 1) VolumeContext=nil 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connector.Connect 미호출 | `CSI-N` |

### E14.4 StorageClass 파라미터 조합 오류

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 113 | `TestCSIEdge_CreateVolume_UnsupportedBackendType` | 알 수 없는 backend-type 파라미터로 CreateVolume | ControllerServer 초기화; PillarTarget 등록 | 1) parameters["backend-type"]="lvm" 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 114 | `TestCSIEdge_CreateVolume_EmptyProtocolType` | protocol-type 파라미터 값이 빈 문자열 | ControllerServer 초기화; PillarTarget 등록 | 1) parameters["protocol-type"]="" 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |

### E14.5 접근 모드(Access Mode) 조합 오류

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 115 | `TestCSIEdge_NodeStage_BlockAccessWithFsType` | 블록 접근 모드 VolumeCapability에 FsType 지정 (잘못된 조합) | NodeServer 초기화; mockConnector.DevicePath 설정 | 1) VolumeCapability(AccessType=Block, FsType="ext4") 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument 또는 FsType 무시 후 블록 접근 성공; FormatAndMount 미호출 | `CSI-N`, `Conn` |
| 116 | `TestCSIEdge_CreateVolume_MultiNodeMultiWriter` | MULTI_NODE_MULTI_WRITER 접근 모드로 ValidateVolumeCapabilities | ControllerServer 초기화 | 1) VolumeCapabilities(AccessMode=MULTI_NODE_MULTI_WRITER) 로 ValidateVolumeCapabilitiesRequest 전송 | Message 필드에 미지원 이유 기록; CreateVolume 불가 | `CSI-C` |

---

## E22 비호환 백엔드-프로토콜 오류 시나리오 (Incompatible Backend-Protocol)

> **Unit test 근거:** `mapProtocolType()`, `mapBackendType()` 매핑 함수와 agent 서버의
> 프로토콜 타입 가드 검사는 열거값 비교 순수 로직이다. configfs 등 실제 커널 사이드 이펙트 없이
> 오류 코드 전파 경로를 검증할 수 있다. (E22.4 수동 검증 시나리오는 제외)

CSI 컨트롤러 또는 Agent가 현재 미지원 프로토콜 타입, 알 수 없는 백엔드 타입,
또는 실제 배포 환경에서의 버전 불일치를 처리할 때의 오류 전파 경로를 검증한다.

### E22.1 CSI Controller — StorageClass 미지원 프로토콜 타입 지정

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 171 | `TestCSIProtocol_CreateVolume_ISCSIUnimplemented` | `protocol-type="iscsi"`로 CreateVolume 호출 시 agent.ExportVolume이 `codes.Unimplemented`("only NVMe-oF TCP is supported") 반환 → CSI 컨트롤러가 비-OK 상태 전파 | `mockAgentServer.ExportVolumeErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; StorageClass params에 `protocol-type: "iscsi"` 설정; PillarTarget CRD 등록; `agent.CreateVolume` 성공(백엔드 zvol 생성 후 export 단계에서 실패) | 1) `CreateVolumeRequest` 전송; 2) 반환 오류 gRPC 코드 확인 | 비-OK gRPC 상태(`codes.OK` 불가); agent의 `Unimplemented` 오류 전파; CreateVolume 실패 시 부분 생성된 zvol 정리 여부는 구현 의존 | `CSI-C`, `Agent`, `gRPC` |
| 172 | `TestCSIProtocol_CreateVolume_NFSUnimplemented` | `protocol-type="nfs"`로 CreateVolume 호출 시 agent.ExportVolume이 `codes.Unimplemented` 반환 | `mockAgentServer.ExportVolumeErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; StorageClass params에 `protocol-type: "nfs"` 설정; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송 | 비-OK gRPC 상태; NFS export 미지원으로 인한 오류 전파 | `CSI-C`, `Agent`, `gRPC` |
| 173 | `TestCSIProtocol_CreateVolume_UnknownProtocol_MapsToUnspecified` | `protocol-type="smb-v3-unknown"` — 알 수 없는 프로토콜 문자열이 `PROTOCOL_TYPE_UNSPECIFIED(0)`으로 매핑되어 agent에 전달됨 → agent가 Unimplemented 반환 | `mockAgentServer.ExportVolumeErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; StorageClass params에 `protocol-type: "smb-v3-unknown"` 설정 | 1) `CreateVolumeRequest` 전송; 2) `env.AgentMock.ExportVolumeCalls[0].ProtocolType` 값 확인 | 비-OK gRPC 상태; `ExportVolumeCalls[0].ProtocolType == PROTOCOL_TYPE_UNSPECIFIED` (UNSPECIFIED로 매핑 확인); agent Unimplemented 전파 | `CSI-C`, `Agent`, `gRPC` |
| 174 | `TestCSIProtocol_ControllerPublish_ISCSIUnimplemented` | ControllerPublishVolume에서 `protocol-type="iscsi"` 볼륨 ID를 가진 PillarVolume CRD 존재 시 agent.AllowInitiator가 `codes.Unimplemented` 반환 → ControllerPublishVolume이 오류 전파 | `mockAgentServer.AllowInitiatorErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; PillarVolume CRD 존재(Phase=Ready, VolumeId에 `iscsi` 포함); PillarTarget CRD 등록; fake Node에 `pillar-csi.bhyoo.com/iscsi-initiator-iqn` annotation 설정 | 1) `ControllerPublishVolumeRequest`(NodeId=`worker-1`) 전송 | 비-OK gRPC 상태; agent AllowInitiator Unimplemented 전파; 오류 은폐 없음 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |

### E22.2 Agent gRPC — 미지원 프로토콜 타입 거부 (RPC별)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 175 | `TestAgentErrors_ExportVolume_InvalidProtocol_NoConfigfsSideEffects` *(기존)* | `ExportVolume`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 및 configfs 사이드 이펙트 없음 — `server_export.go:51` 경계 검사 동작 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend`; `AlwaysPresentChecker` | 1) `ExportVolumeRequest{ProtocolType=ISCSI, VolumeId=compTestVolumeID}` 전송; 2) `configfsRoot/nvmet` 디렉터리 존재 여부 확인 | `codes.Unimplemented`; `nvmet` 디렉터리 미생성(configfs 사이드 이펙트 없음) | `Agent`, `NVMeF` |
| 176 | `TestAgentProtocol_ExportVolume_UNSPECIFIED_Unimplemented` | `ExportVolume`에 `PROTOCOL_TYPE_UNSPECIFIED(0)` 지정 시 `codes.Unimplemented` 반환 — `mapProtocolType`이 알 수 없는 문자열을 UNSPECIFIED로 변환하는 엔드투엔드 경로 커버 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `ExportVolumeRequest{ProtocolType=PROTOCOL_TYPE_UNSPECIFIED}` 전송; 2) configfs 사이드 이펙트 확인 | `codes.Unimplemented`; configfs 미수정; 오류 메시지에 "only NVMe-oF TCP is supported" 포함 | `Agent` |
| 177 | `TestAgentErrors_AllowInitiator_InvalidProtocol` *(기존)* | `AllowInitiator`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 및 `nvmet/hosts` 디렉터리 미생성 — `server_export.go:163` 경계 검사 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `AllowInitiatorRequest{ProtocolType=ISCSI, VolumeId, InitiatorId=compTestHostNQN}` 전송; 2) `nvmet/hosts` 디렉터리 존재 확인 | `codes.Unimplemented`; `nvmet/hosts` 디렉터리 미생성 | `Agent`, `NVMeF` |
| 178 | `TestAgentErrors_DenyInitiator_InvalidProtocol` *(기존)* | `DenyInitiator`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 — `server_export.go:146` 경계 검사 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `DenyInitiatorRequest{ProtocolType=ISCSI, VolumeId, InitiatorId=compTestHostNQN}` 전송 | `codes.Unimplemented` | `Agent` |
| 179 | `TestAgentErrors_UnexportVolume_InvalidProtocol` *(기존)* | `UnexportVolume`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 — 존재하지 않는 iSCSI 서브시스템 삭제 시도 없음; `server_export.go:129` 경계 검사 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `UnexportVolumeRequest{ProtocolType=ISCSI, VolumeId}` 전송 | `codes.Unimplemented`; configfs 미수정 | `Agent` |
| 180 | `TestAgentProtocol_ReconcileState_UnsupportedProtocol_SkipAndReport` | `ReconcileState`에 NVMe-oF TCP 이외 프로토콜 엔트리 포함 시 해당 항목 `success=false`로 보고하고, NVMe-oF TCP 항목은 정상 처리 — `server_reconcile.go:72` 프로토콜 타입 검사 동작 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-mixed"}`; 볼륨 2개 포함 `ReconcileStateRequest`: `v1`(NVMe-oF TCP 수출, `AllowedInitiators=[hostNQN]`), `v2`(iSCSI 수출) | 1) `ReconcileState({volumes: [v1(NVMeOF), v2(ISCSI)]})` 호출; 2) `results` 슬라이스 검사; 3) configfs 서브시스템 디렉터리 확인 | `results[v1].Success=true`; `results[v2].Success=false`; `results[v2].ErrorMessage` 비어 있지 않음; `tmpdir/nvmet/subsystems/<NQN>` 생성됨(v1 처리 성공); 패닉 없음 | `Agent`, `NVMeF`, `gRPC` |

### E22.3 CSI Controller — StorageClass 미지원 백엔드 타입 지정

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 181 | `TestCSIProtocol_CreateVolume_UnknownBackendType_MapsToUnspecified` | `backend-type="fuse-experimental"` — 알 수 없는 백엔드 타입 문자열이 `BACKEND_TYPE_UNSPECIFIED(0)`으로 매핑되어 `agent.CreateVolume` 요청에 전달됨 | `mockAgentServer` 기본 설정(CreateVolume 성공 반환); StorageClass params에 `backend-type: "fuse-experimental"` 설정; PillarTarget CRD 등록; `protocol-type: "nvmeof-tcp"` | 1) `CreateVolumeRequest` 전송; 2) `env.AgentMock.CreateVolumeCalls[0].BackendType` 값 확인 | `CreateVolumeCalls[0].BackendType == BACKEND_TYPE_UNSPECIFIED` (UNSPECIFIED로 매핑 확인); CreateVolume 자체는 mock 기준 성공 반환; 감사 목적 — UNSPECIFIED 백엔드 타입이 agent에 도달함을 문서화 | `CSI-C`, `Agent` |
| 182 | `TestCSIProtocol_CreateVolume_LVMBackendUnimplemented` | `backend-type="lvm"`으로 CreateVolume 호출 시 agent.CreateVolume이 `codes.Unimplemented` 반환 — 현재 단일 ZFS 스토리지 노드에서 LVM 백엔드를 지원하지 않는 시나리오 | `mockAgentServer.CreateVolumeErr = status.Errorf(codes.Unimplemented, "LVM backend not supported in this deployment")`; StorageClass params에 `backend-type: "lvm"` 설정; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송 | 비-OK gRPC 상태; agent의 `Unimplemented` 전파; PillarVolume CRD 미생성 | `CSI-C`, `Agent`, `gRPC` |

---

## NEW-U1 addressSelector CIDR 매칭

> **Unit test 근거:** CIDR 매칭은 IP 문자열과 CIDR 문자열을 받아 bool을 반환하는 순수 함수다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| NEW-U1-1 | `TestResolveAddress_CIDRFilter_MatchesSubnet` | addressSelector CIDR에 매칭되는 IP만 반환 | `addresses=["10.0.0.5/InternalIP", "192.168.219.6/InternalIP"]`, `addressSelector="192.168.219.0/24"` | 1) resolveAddress(addresses, "InternalIP", "192.168.219.0/24") 호출 | 반환값 = "192.168.219.6" | `CSI-C` |
| NEW-U1-2 | `TestResolveAddress_CIDRFilter_NoMatch` | CIDR에 매칭되는 IP가 없으면 빈 문자열 반환 | `addresses=["10.0.0.5/InternalIP"]`, `addressSelector="192.168.0.0/16"` | 1) resolveAddress 호출 | 반환값 = "" (빈 문자열) | `CSI-C` |
| NEW-U1-3 | `TestResolveAddress_NoCIDR_FirstMatch` | addressSelector 미지정 시 addressType 매칭되는 첫 번째 IP 반환 | `addresses=["10.0.0.5/InternalIP", "192.168.1.1/InternalIP"]`, `addressSelector=""` | 1) resolveAddress(addresses, "InternalIP", "") 호출 | 반환값 = "10.0.0.5" (첫 번째 InternalIP) | `CSI-C` |
| NEW-U1-4 | `TestResolveAddress_InvalidCIDR` | 잘못된 CIDR 형식 시 에러 또는 빈 문자열 | `addressSelector="not-a-cidr"` | 1) resolveAddress 호출 | 에러 반환 또는 빈 문자열; 패닉 없음 | `CSI-C` |

---

## NEW-U2 CSI Topology Capability 미선언

> **Unit test 근거:** GetPluginCapabilities 응답에 포함되는 capability 상수 목록의 정확성 검증.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| NEW-U2-1 | `TestPluginCapabilities_NoTopology` | GetPluginCapabilities에 VOLUME_ACCESSIBILITY_CONSTRAINTS가 포함되지 않음 (Phase 1 미지원) | IdentityServer 초기화 | 1) GetPluginCapabilitiesRequest 전송; 2) 응답 capabilities 목록 검사 | VOLUME_ACCESSIBILITY_CONSTRAINTS 없음; VolumeExpansion_ONLINE은 있음 | `CSI-C` |

---

## NEW-U3 구조화된 로깅 형식 (JSON, slog)

> **Unit test 근거:** 로그 출력 형식은 slog 설정의 결과이며, 출력 문자열을 JSON 파싱하여 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| NEW-U3-1 | `TestStructuredLogging_JSONFormat` | slog 기본 핸들러가 JSON 형식 로그를 출력 | slog.JSONHandler로 초기화된 로거; 버퍼 Writer | 1) logger.Info("test", "key", "value") 호출; 2) 출력 버퍼를 json.Unmarshal | JSON 파싱 성공; "msg"="test", "key"="value" 필드 존재 | — |
| NEW-U3-2 | `TestStructuredLogging_ErrorLevel` | 에러 로그에 "level":"ERROR"와 에러 메시지 포함 | slog.JSONHandler; 버퍼 Writer | 1) logger.Error("fail", "err", errors.New("boom")) 호출 | JSON에 "level":"ERROR", "err":"boom" 포함 | — |

---

## NEW-U4 NQN 형식 생성

> **Unit test 근거:** NQN 문자열 생성은 target 이름과 volume 이름을 받아 고정 포맷 문자열을 반환하는 순수 함수다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| NEW-U4-1 | `TestGenerateNQN_Format` | NQN이 `nqn.2024-01.com.bhyoo.pillar-csi:<target>:<volume>` 형식 | target="rock5bp", volume="pvc-abc123" | 1) GenerateNQN(target, volume) 호출 | 반환값 = "nqn.2024-01.com.bhyoo.pillar-csi:rock5bp:pvc-abc123" | `NVMeF` |
| NEW-U4-2 | `TestGenerateNQN_MaxLength` | NQN이 223자(커널 제한) 이내인지 | target="very-long-name-...", volume="very-long-pvc-..." | 1) GenerateNQN 호출; 2) len(result) 확인 | len ≤ 223; 초과 시 truncation 또는 에러 | `NVMeF` |

---

## NEW-U5 NQN/IQN 형식 검증 (GetInitiatorID용)

> **Unit test 근거:** NQN/IQN 형식 문자열이 스펙에 맞는지 검증하는 순수 함수다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| NEW-U5-1 | `TestIsValidNQN_ValidFormat` | 유효한 NQN 형식 인식 | nqn="nqn.2014-08.org.nvmexpress:uuid:1234-5678" | 1) IsValidNQN(nqn) 호출 | true | `NVMeF` |
| NEW-U5-2 | `TestIsValidNQN_InvalidFormat` | 유효하지 않은 NQN 거부 | nqn="not-a-nqn" | 1) IsValidNQN(nqn) 호출 | false | `NVMeF` |
| NEW-U5-3 | `TestIsValidIQN_ValidFormat` | 유효한 IQN 형식 인식 | iqn="iqn.1993-08.org.debian:01:abcdef" | 1) IsValidIQN(iqn) 호출 | true | — |
