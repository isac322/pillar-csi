# Component Tests — 프레임워크 오케스트레이션 검증

CSI Controller/Node의 오케스트레이션 로직을 검증한다. 외부 의존성(Agent, K8s API,
커널 모듈)은 전부 mock/fake로 대체되어 있으며, 내부 모듈 간 호출 순서, 에러 전파,
멱등성, 상태 관리를 검증한다.

**실행:** `go test ./test/component/ -v`
**빌드 태그:** 없음
**CI:** ✅ 표준 CI 실행 가능
**테스트 더블:** mockAgentServer (실제 gRPC localhost:0), mockCSIConnector, mockCSIMounter, fake k8s client

---

## 목차

- [E1.1-E1.5: CreateVolume/DeleteVolume 오케스트레이션](#e11-e15-createvolumedeletevolume-오케스트레이션)
- [E1.8: PillarTarget 상태 검증](#e18-pillartarget-상태-및-agent-연결-검증)
- [E1.9: 부분 실패 복구](#e19-부분-실패-복구)
- [E1.10: PVC 어노테이션 오버라이드](#e110-pvc-어노테이션-오버라이드)
- [E2.1: ControllerPublishVolume 정상 경로](#e21-controllerpublishvolume--정상-경로)
- [E2.2: ControllerUnpublishVolume 정상 경로 및 오류](#e22-controllerunpublishvolume--정상-경로-및-오류)
- [E2.5: 노드 친화성](#e25-노드-친화성)
- [E2.6: 오류 처리 오케스트레이션 서브셋](#e26-오류-처리--오케스트레이션-서브셋)
- [E3: CSI Node 전체](#e3-csi-node--nodestage--nodepublish--nodeunpublish--nodeunstage)
- [E4: 교차-컴포넌트 CSI 라이프사이클](#e4-교차-컴포넌트-csi-라이프사이클)
- [E6: 부분 실패 영속성](#e6-부분-실패-영속성)
- [E7: 게시 멱등성](#e7-게시-멱등성)
- [E8: mTLS 핸드셰이크](#e8-mtls-핸드셰이크)
- [E9: Agent gRPC 디스패치](#e9-agent-grpc-디스패치)
- [E11: 볼륨 확장 오케스트레이션](#e11-볼륨-확장-오케스트레이션)
- [E15: 리소스 고갈 에러 전파](#e15-리소스-고갈-에러-전파)
- [E16: 동시 작업 안전성](#e16-동시-작업-안전성)
- [E17: 정리 검증](#e17-정리-검증)
- [E18: Agent 다운 에러 핸들링](#e18-agent-다운-에러-핸들링)
- [E21.1: 잘못된 CR 런타임 처리](#e211-잘못된-cr-런타임-처리)
- [E24: 8단계 전체 라이프사이클](#e24-8단계-전체-라이프사이클)
- [E29: LVM 파라미터 전파](#e29-lvm-파라미터-전파)
- [E30: LVM 중복 방지 최적화](#e30-lvm-중복-방지-최적화)
- [PRD 갭 — 추가 TC](#prd-갭--추가-tc)
  - [C-NEW-1: acl=false → ControllerPublish/Unpublish no-op](#c-new-1-aclfalse--controllerpublishunpublish-no-op)
  - [C-NEW-2: acl=false → ExportVolume allow_any_host=1](#c-new-2-aclfalse--exportvolume-allow_any_host1)
  - [C-NEW-3: modprobe 실패 → protocol capabilities 제외](#c-new-3-modprobe-실패--protocol-capabilities-제외)
  - [C-NEW-4: 커널 모듈 미로드 → NodeStageVolume 명확한 에러](#c-new-4-커널-모듈-미로드--nodestagevolume-명확한-에러)
  - [C-NEW-5: NVMe-oF 타임아웃 파라미터 개별 전파](#c-new-5-nvme-of-타임아웃-파라미터-개별-전파)
  - [C-NEW-6: mkfsOptions 전파](#c-new-6-mkfsoptions-전파)
  - [C-NEW-7: Exponential backoff 타이밍](#c-new-7-exponential-backoff-타이밍)
  - [C-NEW-8: gRPC 자동 재연결](#c-new-8-grpc-자동-재연결)
  - [C-NEW-9: NodeGetVolumeStats mock 기반](#c-new-9-nodegetvolumestats-mock-기반)
  - [C-NEW-10: Agent Snapshot RPC 에러](#c-new-10-agent-snapshot-rpc-에러)
  - [C-NEW-11: NodeGetInfo GetInitiatorID 형식 검증](#c-new-11-nodegetinfo-getinitiatorid-형식-검증)
  - [C-NEW-12: PVC annotation fs-override](#c-new-12-pvc-annotation-fs-override)
  - [C-NEW-13: K8s Event 기록](#c-new-13-k8s-event-기록)
  - [C-NEW-14: Prometheus 메트릭 카운터](#c-new-14-prometheus-메트릭-카운터)

---

## 테스트 더블 피델리티 노트

Component test는 외부 의존성을 아래 테스트 더블로 대체한다. 각 더블의 실제 대비 한계를 이해해야 테스트 결과를 올바르게 해석할 수 있다.

### mockAgentServer

실제 gRPC 리스너(localhost:0)를 사용하므로 직렬화/역직렬화는 실제와 동일. 그러나:
- 실제 ZFS/LVM 명령 미실행 — 사전 설정된 응답 필드 사용
- configfs 조작 없음 — ExportInfo는 하드코딩된 테스트 값
- 각 RPC의 호출 기록을 슬라이스로 보존하여 호출 순서/횟수 검증 가능
- 오류 시나리오는 테스트별 에러 필드로 주입

### mockCSIConnector

NVMe-oF connector.Connector 인터페이스 구현:
- 실제 `nvme connect` 명령 미실행
- DevicePath는 테스트가 제어하는 문자열 필드
- 커널 모듈(nvme-tcp) 불필요

### mockCSIMounter

mounter.Mounter 인터페이스 구현:
- 실제 `mount(8)`/`umount(8)` 시스템 콜 없음
- 인메모리 마운트 테이블로 상태 추적
- `mkfs`/`resize2fs` 등 포맷/리사이즈 명령 없음
- root 권한 불필요

### fake controller-runtime client

controller-runtime의 `fake.NewClientBuilder()`로 생성:
- 인메모리 객체 저장소; 실제 etcd 없음
- 낙관적 잠금(ResourceVersion) 부분 지원
- CRD 검증 웹훅 미실행 → Integration(envtest)에서 검증
- RBAC 미적용 → Integration(E27)에서 검증

---

## 범위 제약 — Component Test가 검증하지 못하는 것

| 검증 불가 항목 | 이유 | 대안 |
|--------------|------|------|
| 실제 ZFS zvol 생성/삭제 | mock backend 사용 | Integration (E28/ZFS-I 계획) |
| 실제 NVMe-oF configfs 바인딩 | t.TempDir() 사용 | Integration (NVMEOF-I 계획) |
| 실제 `nvme connect` / `mount(8)` | mock connector/mounter | E2E (E33-E35) |
| CRD 검증 웹훅 실행 | fake client | Integration (E21.2-E21.4) |
| RBAC 실제 검증 | fake client | Integration (E27) |
| Kubernetes etcd 일관성 | fake client 부분 지원 | Integration (envtest) |
| 커널 레벨 레이스 컨디션 | 인프로세스 고루틴만 | E2E (F-series) |
| PVC/Pod Kubernetes 프로비저닝 흐름 | kubectl 미사용 | E2E (E33-E35) |

---

## E1.1-E1.5: CreateVolume/DeleteVolume 오케스트레이션

> **Component test 이유:** CSI ControllerServer가 mockAgentServer(실제 gRPC localhost:0)와 fake k8s client를 통해
> agent.CreateVolume -> agent.ExportVolume 호출 순서, 에러 롤백, 멱등성, VolumeId 보존을 검증한다.
> 실제 ZFS/NVMe-oF 없이 오케스트레이션 로직만 테스트한다.

### E1.1 CreateVolume -- 정상 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 1 | `TestCSIController_CreateVolume` | CreateVolume이 agent.CreateVolume -> agent.ExportVolume을 순서대로 호출하고 올바른 VolumeId/VolumeContext를 반환 | PillarTarget="storage-1" fake 클라이언트에 등록; mockAgentServer 정상 동작; pool="tank"(PillarPool CRD); 프로토콜=nvmeof-tcp; 용량=1GiB | 1) CreateVolumeRequest 전송 | VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-create-test"; VolumeContext에 target_id/address/port/volume-ref/protocol-type 포함 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 2 | `TestCSIController_CreateVolume_Idempotency` | 동일한 볼륨 이름으로 CreateVolume을 두 번 호출하면 두 번째 호출은 agent.CreateVolume/ExportVolume을 재호출하지 않고 동일한 응답 반환 | 위와 동일; mockAgentServer 정상 동작 | 1) CreateVolumeRequest 전송; 2) 동일 파라미터로 CreateVolumeRequest 재전송 | 두 번째 호출 성공; 동일한 VolumeId 반환; agent CreateVolume은 1회, ExportVolume은 1회만 호출 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

### E1.2 CreateVolume -- 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 3 | `TestCSIController_CreateVolume_MissingParams` | StorageClass 파라미터 누락 시 InvalidArgument 반환 | ControllerServer 초기화; StorageClass Parameters에서 필수 키(target/backend-type/protocol-type/pool) 일부 또는 전부 제거 | 1) 파라미터 일부 누락한 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |

> ⚠️ CSI Sanity 도입 후 대체 가능 — "CreateVolume fails with no name/no capabilities" 테스트와 중복

| 4 | `TestCSIController_CreateVolume_PillarTargetNotFound` | 참조된 PillarTarget이 존재하지 않으면 NotFound 반환 | fake 클라이언트에 PillarTarget 미등록; Parameters["target"]="nonexistent" | 1) CreateVolumeRequest 전송 | gRPC NotFound 또는 Internal; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| 5 | `TestCSIController_CreateVolume_AgentCreateError` | agent.CreateVolume 실패 시 오류 전파 | mockAgentServer.CreateVolumeErr 설정; PillarTarget 정상 등록 | 1) CreateVolumeRequest 전송 | 비-OK gRPC 상태 반환; ExportVolume 미호출 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| 6 | `TestCSIController_CreateVolume_AgentExportError` | agent.CreateVolume 성공 후 agent.ExportVolume 실패 시 오류 전파 | mockAgentServer.ExportVolumeErr 설정; CreateVolume은 성공 | 1) CreateVolumeRequest 전송 | 비-OK gRPC 상태 반환; PillarVolume CRD에 PartialFailure 기록 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

### E1.3 DeleteVolume -- 정상 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 7 | `TestCSIController_DeleteVolume` | DeleteVolume이 agent.UnexportVolume -> agent.DeleteVolume을 순서대로 호출 | CreateVolume으로 볼륨 사전 생성 (PillarVolume CRD 존재); mockAgentServer 정상 동작 | 1) CreateVolumeRequest 전송; 2) DeleteVolumeRequest 전송 | 성공; UnexportVolume 1회, DeleteVolume 1회 호출 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| 8 | `TestCSIController_DeleteVolume_Idempotency` | 이미 삭제된 볼륨을 다시 DeleteVolume해도 성공 (멱등성) | 볼륨 생성 후 첫 DeleteVolume 완료 | 1) DeleteVolumeRequest 전송; 2) 동일 VolumeId로 DeleteVolumeRequest 재전송 | 두 번째 호출도 성공; 오류 없음 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| 9 | `TestCSIController_DeleteVolume_NotFoundIsIdempotent` | agent가 NotFound를 반환해도 DeleteVolume은 성공 처리 | mockAgentServer.UnexportVolumeErr = gRPC NotFound; PillarVolume CRD 없음 | 1) DeleteVolumeRequest 전송 | DeleteVolume 성공; CSI 명세상 Not-Found는 이미 삭제된 것으로 처리 | `CSI-C`, `Agent`, `gRPC` |

### E1.4 DeleteVolume -- 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 10 | `TestCSIController_DeleteVolume_MalformedID` | 잘못된 형식의 VolumeId는 InvalidArgument 반환 | ControllerServer 초기화 | 1) VolumeId="noslash"로 DeleteVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 11 | `TestCSIController_DeleteVolume_AgentError` | agent.UnexportVolume 또는 agent.DeleteVolume 실패 시 오류 전파 | mockAgentServer.DeleteVolumeErr 설정; PillarVolume CRD 존재 | 1) DeleteVolumeRequest 전송 | 비-OK gRPC 상태 반환 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

### E1.5 기본 프로비저닝 -- 전체 왕복(Full Round Trip)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 12 | `TestCSIController_FullRoundTrip` | CreateVolume -> ControllerPublishVolume -> ControllerUnpublishVolume -> DeleteVolume 전체 CSI Controller 왕복 테스트 | 단일 mockAgentServer; fake k8s 클라이언트; PillarTarget 등록; 정상 경로 설정 | 1) CreateVolume; 2) ControllerPublishVolume; 3) ControllerUnpublishVolume; 4) DeleteVolume | 모든 단계 성공; agent 호출 순서 검증; VolumeContext 키 검증 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 13 | `TestCSIController_VolumeIDFormatPreservation` | VolumeId 포맷("target/protocol/backend/pool/name")이 생성-게시-삭제 전 주기에서 보존됨 | CreateVolume 성공; PillarTarget 등록 | 1) CreateVolume; 2) ControllerPublishVolume; 3) ControllerUnpublishVolume; 4) DeleteVolume | 각 단계에서 동일한 VolumeId 포맷 사용; 파싱 오류 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

---

## E1.8: PillarTarget 상태 및 agent 연결 검증

> **Component test 이유:** CSI Controller가 PillarTarget CRD의 ResolvedAddress 상태를 조회하여
> agent 다이얼 가능 여부를 판단하는 오케스트레이션 로직을 검증한다.
> fake k8s client에 다양한 상태의 PillarTarget을 주입하여 에러 전파 경로를 테스트한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.8-1 | `TestCSIController_CreateVolume_PillarTargetEmptyAddress` | PillarTarget이 존재하지만 ResolvedAddress=""이면 Unavailable 반환 | fake 클라이언트에 PillarTarget 등록; Status.ResolvedAddress="" | 1) 해당 target을 참조하는 CreateVolumeRequest 전송 | gRPC Unavailable; "has no resolved address; agent may not be ready" 메시지; agent 다이얼 시도 없음 | `CSI-C`, `TgtCRD` |
| E1.8-2 | `TestCSIController_CreateVolume_PillarTargetNotFound` | Parameters["target"]이 존재하지 않는 PillarTarget을 참조하면 NotFound 반환 | fake 클라이언트에 PillarTarget 미등록; Parameters["pillar-csi.bhyoo.com/target"]="ghost-node" | 1) CreateVolumeRequest 전송 | gRPC NotFound; "PillarTarget ... not found" 메시지; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| E1.8-3 | `TestCSIController_CreateVolume_AgentDialFails` | agent 다이얼 자체가 실패하면 Unavailable 반환 | PillarTarget 등록 (ResolvedAddress=유효); dialAgent 함수에 연결 실패 에러 주입 | 1) CreateVolumeRequest 전송 | gRPC Unavailable; "failed to dial agent" 메시지; agent.CreateVolume 호출 없음 | `CSI-C`, `TgtCRD`, `gRPC` |

---

## E1.9: 부분 실패 복구

> **Component test 이유:** CreateVolume의 2단계 오케스트레이션(backend 생성 -> export)에서 중간 실패 시
> PillarVolume CRD에 CreatePartial 상태를 기록하고, 재시도 시 skipBackend 최적화로 backend 재호출을
> 건너뛰는 상태 머신 로직을 검증한다. fake k8s client와 mockAgentServer를 사용한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.9-1 | `TestCSIController_PartialFailure_CreateThenExportFail` | agent.CreateVolume 성공 후 agent.ExportVolume 실패 시 PillarVolume CRD에 CreatePartial 기록 | mockAgentServer: ExportVolumeErr=gRPC Internal; PillarTarget 정상 | 1) CreateVolumeRequest 전송 | gRPC Internal 반환; PillarVolume CRD에 phase=CreatePartial, BackendDevicePath 기록됨 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E1.9-2 | `TestCSIController_PartialFailure_ExportRetrySkipsBackend` | CreatePartial 상태에서 재시도 시 agent.CreateVolume 생략, agent.ExportVolume만 재호출 | PillarVolume CRD phase=CreatePartial; BackendDevicePath="/dev/zvol0" 사전 기록; 두 번째 호출에서 ExportVolumeErr 제거 | 1) CreateVolumeRequest 전송 (재시도) | 성공; agent.CreateVolume 호출 0회; agent.ExportVolume 호출 1회; 완성된 VolumeId/VolumeContext 반환 | `CSI-C`, `Agent`, `VolCRD`, `SM`, `gRPC` |
| E1.9-3 | `TestCSIController_PartialFailure_SelfHealing_TwoAttempts` | 첫 번째 호출(export 실패) -> 두 번째 호출(정상) 연속 시나리오 | ExportVolumeErr를 첫 번째 호출에만 설정; 두 번째 호출 전 제거 | 1) 첫 CreateVolumeRequest 전송 (export 실패 예상); 2) 두 번째 CreateVolumeRequest 전송 | 1단계: gRPC Internal; 2단계: 성공; agent.CreateVolume 누적 1회; agent.ExportVolume 누적 2회 | `CSI-C`, `Agent`, `VolCRD`, `SM`, `gRPC` |
| E1.9-4 | `TestCSIController_PartialFailure_PersistPartialFails` | persistCreatePartial CRD 저장 실패 시 Internal 반환 (zvol은 생성됐으나 상태 기록 불가) | fake 클라이언트에 Create 오류 주입 (status.WriteFailure); agent.CreateVolume 성공 | 1) CreateVolumeRequest 전송 | gRPC Internal; "failed to persist partial-failure state" 메시지 | `CSI-C`, `Agent`, `VolCRD` |
| E1.9-5 | `TestCSIController_PartialFailure_LoadStateFromCRD` | 컨트롤러 재기동 시 기존 PillarVolume CRD에서 상태 복원 | PillarVolume CRD phase=CreatePartial를 직접 fake 클라이언트에 삽입; `LoadStateFromPillarVolumes` 호출 | 1) `LoadStateFromPillarVolumes` 호출; 2) CreateVolumeRequest 전송 | LoadStateFromPillarVolumes 성공; 이후 CreateVolumeRequest는 StateCreatePartial 인식하여 backend 건너뜀 | `CSI-C`, `VolCRD`, `SM` |

---

## E1.10: PVC 어노테이션 오버라이드

> **Component test 이유:** StorageClass 기본 파라미터와 PVC 어노테이션 오버라이드(Layer 4)가 병합되어
> agent.CreateVolume의 BackendParams로 전달되는 파라미터 머지 오케스트레이션을 검증한다.
> fake k8s client에 PVC를 등록하고 어노테이션 값이 agent 요청에 정확히 반영되는지 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.10-1 | `TestCSIController_CreateVolume_PVCAnnotation_BackendOverride_Compression` | PVC 어노테이션의 ZFS compression 프로퍼티가 agent BackendParams에 반영 | fake 클라이언트에 PVC 등록 (annotation: `pillar-csi.bhyoo.com/backend-override`); StorageClass Parameters에 pvc-name/pvc-namespace 포함 | 1) CreateVolumeRequest 전송 | 성공; agent.CreateVolume의 BackendParams에 `pillar-csi.bhyoo.com/zfs-prop.compression=zstd` 포함 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E1.10-2 | `TestCSIController_CreateVolume_PVCAnnotation_StructuralFieldBlocked` | 구조적 필드(`zfs.pool`)를 어노테이션으로 오버라이드하면 InvalidArgument 반환 | fake 클라이언트에 PVC 등록 (annotation에 zfs.pool 오버라이드 시도) | 1) CreateVolumeRequest 전송 | gRPC InvalidArgument; `pvcAnnotationValidationError` 발생; agent 호출 없음 | `CSI-C`, `VolCRD` |
| E1.10-3 | `TestCSIController_CreateVolume_PVCAnnotation_PVCNotFound_GracefulFallback` | pvc-name에 해당하는 PVC가 없으면 어노테이션 오버라이드 없이 기본 파라미터로 진행 | fake 클라이언트에 PVC 미등록; StorageClass Parameters에 pvc-name/pvc-namespace 포함 | 1) CreateVolumeRequest 전송 | 성공 (PVC 어노테이션 미적용 상태로 기본 파라미터 사용); agent.CreateVolume 정상 호출 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E1.10-4 | `TestCSIController_CreateVolume_PVCAnnotation_FlatKeyOverride` | 저수준 어노테이션(`pillar-csi.bhyoo.com/param.zfs-prop.volblocksize`)이 반영 | fake 클라이언트에 PVC 등록 (annotation: volblocksize=16K) | 1) CreateVolumeRequest 전송 | 성공; agent.CreateVolume의 BackendParams에 `pillar-csi.bhyoo.com/zfs-prop.volblocksize=16K` 포함 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

## E2.1: ControllerPublishVolume -- 정상 경로

> **Component test 이유:** ControllerPublishVolume이 CSINode annotation에서 protocol-specific identity를
> 해석하여 agent.AllowInitiator에 전달하는 오케스트레이션 체인을 검증한다. fake CSINode와
> mockAgentServer를 사용하여 NVMe host NQN / iSCSI initiator IQN 해석 로직을 테스트한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 14 | `TestCSIController_ControllerPublishVolume` | ControllerPublishVolume이 `CSINode` annotation에서 NVMe host NQN을 해석해 agent.AllowInitiator를 호출 | PillarTarget="storage-1" fake 클라이언트에 등록; fake `CSINode` `worker-1`에 `pillar-csi.bhyoo.com/nvmeof-host-nqn=nqn.2014-08.org.nvmexpress:uuid:worker-1` annotation 설정; mockAgentServer(실제 gRPC 리스너) 정상; VolumeId=`storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-publish-test`; NodeId=`worker-1` | 1) ControllerPublishVolumeRequest 전송 | 성공; non-nil PublishContext 반환; AllowInitiator 1회; AllowInitiator.VolumeID=`tank/pvc-publish-test`; AllowInitiator.InitiatorID=`nqn.2014-08.org.nvmexpress:uuid:worker-1`; AllowInitiator.ProtocolType=NVMEOF_TCP | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| E2.1-2 | `TestCSIController_ControllerPublishVolume_ISCSIInitiatorFromCSINodeAnnotations` | iSCSI 볼륨 publish 시 controller가 `CSINode` annotation에서 initiator IQN을 해석해 AllowInitiator 호출 | `test/component/csi_controller_test.go`; `csiMockAgent`(in-process); PillarTarget fake 등록; fake `CSINode` `worker-2`에 `pillar-csi.bhyoo.com/iscsi-initiator-iqn=iqn.1993-08.org.debian:worker-2` annotation 설정; VolumeId=`pool/iscsi/zfs-zvol/tank/vol`; NodeId=`worker-2` | 1) ControllerPublishVolumeRequest 전송 | 성공; allowInitiatorCalls==1; agent.AllowInitiator.InitiatorID=`iqn.1993-08.org.debian:worker-2`; PublishContext 반환 | `CSI-C`, `Agent`, `TgtCRD` |
| 15 | `TestCSIController_ControllerPublishVolume_Idempotency` | 동일 node handle과 동일 `CSINode` annotation으로 두 번 호출해도 CSI 계층은 agent 중복 억제 없이 각 호출을 전달 | 유효한 VolumeId/NodeId; 대응 `CSINode` annotation 존재; mockAgentServer 정상 | 1) ControllerPublishVolumeRequest 전송; 2) 동일 인수로 재전송 | 두 호출 모두 성공; PublishContext 동일; AllowInitiator 각 1회씩 총 2회 | `CSI-C`, `Agent`, `gRPC` |
| E2.1-4 | `TestCSIController_ControllerPublishVolume_AlreadyPublished` | 이미 Publish된 볼륨/노드 조합으로 재호출 성공 (컴포넌트 테스트) | `test/component/csi_controller_test.go`; allowInitiatorFn=nil(항상 성공); `CSINode` annotation 준비 | 1) ControllerPublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; agent.AllowInitiator 총 2회 (CSI 계층은 억제 없음; 멱등성은 agent 책임) | `CSI-C`, `Agent` |

---

## E2.2: ControllerUnpublishVolume -- 정상 경로 및 오류

> **Component test 이유:** ControllerUnpublishVolume의 DenyInitiator 호출 오케스트레이션과
> CSI 명세 준수(NotFound는 성공으로 처리, 빈 NodeId는 no-op 등) 로직을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 17 | `TestCSIController_ControllerUnpublishVolume` | ControllerUnpublishVolume이 `CSINode` annotation에서 NVMe host NQN을 해석해 agent.DenyInitiator를 호출 | mockAgentServer 정상; fake `CSINode` `worker-1`에 `pillar-csi.bhyoo.com/nvmeof-host-nqn=nqn.2014-08.org.nvmexpress:uuid:worker-1` annotation 설정; VolumeId=`storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-unpublish-test`; NodeId=`worker-1` | 1) ControllerUnpublishVolumeRequest 전송 | 성공; DenyInitiator 1회; DenyInitiator.VolumeID=`tank/pvc-unpublish-test`; DenyInitiator.InitiatorID=`nqn.2014-08.org.nvmexpress:uuid:worker-1`; DenyInitiator.ProtocolType=NVMEOF_TCP | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| E2.2-2 | `TestCSIController_ControllerUnpublishVolume_Success` | ControllerUnpublishVolume 정상 경로 (컴포넌트 테스트) | `test/component/csi_controller_test.go`; `csiMockAgent`; denyInitiatorFn=nil | 1) ControllerUnpublishVolumeRequest 전송 | 성공; denyInitiatorCalls==1 | `CSI-C`, `Agent` |
| 18 | `TestCSIController_ControllerUnpublishVolume_NotFoundIsIdempotent` | agent.DenyInitiator가 NotFound 반환 시 Unpublish는 성공으로 처리 (CSI 명세 S4.3.4: NotFound = 이미 접근 제거됨) | mockAgentServer.DenyInitiatorErr = gRPC NotFound | 1) ControllerUnpublishVolumeRequest 전송 | 성공; gRPC OK 반환; CSI 호출자에게 오류 없음 | `CSI-C`, `Agent`, `gRPC` |
| E2.2-4 | `TestCSIController_ControllerUnpublishVolume_AlreadyUnpublished` | 이미 Unpublish된 볼륨에 재호출 성공 (컴포넌트 테스트) | `test/component/csi_controller_test.go`; denyInitiatorFn=nil | 1) ControllerUnpublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; DenyInitiator 총 2회 | `CSI-C`, `Agent` |
| E2.2-5 | `TestCSIController_ControllerUnpublishVolume_EmptyVolumeID` | VolumeId=""이면 InvalidArgument 반환; DenyInitiator 0회 (컴포넌트 테스트) | `test/component/csi_controller_extended_test.go`; VolumeId="" | 1) VolumeId=""로 ControllerUnpublishVolumeRequest 전송 | gRPC InvalidArgument; DenyInitiator 0회 | `CSI-C` |

> ⚠️ CSI Sanity 도입 후 대체 가능 — "ControllerUnpublishVolume fails with empty volume ID" 테스트와 중복

| E2.2-6 | `TestCSIController_ControllerUnpublishVolume_EmptyNodeID` | NodeId=""이면 성공 + no-op (CSI 명세 S4.3.4: 빈 NodeId = "모든 노드에서 Unpublish"; pillar-csi는 no-op) | `test/component/csi_controller_extended_test.go`; 유효한 VolumeId; NodeId="" | 1) NodeId=""로 ControllerUnpublishVolumeRequest 전송 | 성공; DenyInitiator 0회 (no-op 처리) | `CSI-C` |
| E2.2-7 | `TestCSIController_ControllerUnpublishVolume_MalformedVolumeID` | VolumeId="badformat"(슬래시 없음)이면 성공 반환 (컴포넌트 테스트; CSI 명세상 Unpublish malformed ID는 성공 no-op 허용) | `test/component/csi_controller_extended_test.go`; VolumeId="badformat" | 1) VolumeId="badformat"로 전송 | 성공; DenyInitiator 0회 | `CSI-C` |
| E2.2-8 | `TestCSIErrors_ControllerUnpublish_DenyInitiatorNonNotFound` | agent.DenyInitiator가 Internal 오류 반환 시 ControllerUnpublishVolume이 비-OK gRPC 상태 전파 (NotFound만 성공 처리) | `test/component/csi_controller_extended_test.go`; denyInitiatorFn = gRPC Internal("deny initiator failed") | 1) ControllerUnpublishVolumeRequest 전송 | 비-OK gRPC 상태; 오류 은폐 없음 | `CSI-C`, `Agent` |

---

## E2.5: 노드 친화성

> **Component test 이유:** NodeId -> CSINode annotation -> protocol-specific InitiatorID 해석 매핑이
> NVMe-oF/iSCSI 두 프로토콜에서 정확히 동작하는지, 그리고 서로 다른 노드에 대한 독립적
> AllowInitiator 항목 생성 로직을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E2.5-1 | `TestCSIController_ControllerPublishVolume` | NVMe-oF publish에서 `NodeId=worker-1`이 `CSINode` annotation의 host NQN으로 해석되어 AllowInitiator에 전달됨 | VolumeId=`storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-publish-test`; NodeId=`worker-1`; fake `CSINode` `worker-1`에 `pillar-csi.bhyoo.com/nvmeof-host-nqn` annotation 설정; PillarTarget 등록; mockAgentServer | 1) ControllerPublishVolumeRequest 전송; 2) AllowInitiator 호출 내용 검사 | AllowInitiator.InitiatorID == `CSINode` annotation의 host NQN; AllowInitiator.VolumeID==`tank/pvc-publish-test`; AllowInitiator.ProtocolType==NVMEOF_TCP | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| E2.5-2 | `TestCSIController_ControllerPublishVolume_ISCSIInitiatorFromCSINodeAnnotations` | iSCSI publish에서 `NodeId=worker-2`가 `CSINode` annotation의 initiator IQN으로 해석되어 AllowInitiator에 전달됨 | VolumeId=`storage-1/iscsi/zfs-zvol/tank/pvc-publish-test`; NodeId=`worker-2`; fake `CSINode` `worker-2`에 `pillar-csi.bhyoo.com/iscsi-initiator-iqn=iqn.1993-08.org.debian:worker-2` annotation 설정; PillarTarget 등록; mockAgentServer | 1) ControllerPublishVolumeRequest 전송; 2) AllowInitiator 호출 내용 검사 | AllowInitiator.InitiatorID == `CSINode` annotation의 initiator IQN; AllowInitiator.VolumeID==`tank/pvc-publish-test`; AllowInitiator.ProtocolType==ISCSI | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| E2.5-3 | `TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes` | 동일 볼륨에 대해 2개의 서로 다른 node handle(worker-a, worker-b)이 각각 다른 `CSINode` annotation을 통해 독립 AllowInitiator 항목을 생성 | VolumeId 동일; NodeId1=`worker-node-a`; NodeId2=`worker-node-b`; 두 `CSINode`에 서로 다른 protocol-specific annotation 설정; mockAgentServer | 1) ControllerPublishVolume(NodeId1); 2) ControllerPublishVolume(NodeId2) | AllowInitiator 총 2회; AllowInitiator[0].InitiatorID != AllowInitiator[1].InitiatorID; 두 호출 모두 성공 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |

---

## E2.6: 오류 처리 -- 오케스트레이션 서브셋

> **Component test 이유:** agent/CRD에서 발생하는 오류가 CSI Controller를 통해 CO로 정확히 전파되는지 검증한다.
> 입력 검증(E2.6-3~E2.6-6)은 별도 분류이므로 여기서는 agent 오류 전파(E2.6-1, E2.6-2)와
> CRD 상태 문제(E2.6-7, E2.6-8)만 포함한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E2.6-1 | `TestCSIErrors_ControllerPublish_AllowInitiatorFails` | agent.AllowInitiator가 Internal 오류 반환 시 ControllerPublishVolume이 비-OK gRPC 상태 반환; 오류 은폐 없음 (실제로는 configfs ACL 쓰기 실패 시 발생) | `test/component/csi_errors_test.go`; allowInitiatorFn=gRPC Internal("configfs write failed: permission denied") | 1) ControllerPublishVolumeRequest 전송 | 비-OK gRPC 상태(Internal); 오류 메시지 포함; 성공 은폐 없음 | `CSI-C`, `Agent` |
| E2.6-2 | `TestCSIErrors_ControllerPublish_MissingNodeIdentityAnnotation` | `CSINode`는 존재하지만 protocol별 initiator annotation이 없으면 ControllerPublishVolume이 FailedPrecondition 반환; agent 호출 없음 | `test/component/csi_errors_test.go`; fake `CSINode` `worker-1` 존재하지만 `pillar-csi.bhyoo.com/nvmeof-host-nqn` 또는 `pillar-csi.bhyoo.com/iscsi-initiator-iqn` 없음; 유효한 VolumeId/NodeId | 1) ControllerPublishVolumeRequest 전송 | gRPC FailedPrecondition; AllowInitiator 0회; "`CSINode` identity not ready" 계열 메시지 | `CSI-C`, `TgtCRD` |
| E2.6-7 | `TestCSIController_ControllerPublishVolume_TargetNotFound` | VolumeId의 target 이름이 PillarTarget CRD에 없음 -> NotFound; agent 호출 전 실패 | `test/component/csi_controller_extended_test.go`; fake 클라이언트에 PillarTarget 미등록; VolumeId=`nonexistent-node/nvmeof-tcp/zfs-zvol/tank/pvc-test` | 1) ControllerPublishVolumeRequest 전송 | gRPC NotFound; AllowInitiator 0회 | `CSI-C`, `TgtCRD` |
| E2.6-8 | `TestCSIController_ControllerPublishVolume_TargetNoResolvedAddress` | PillarTarget.Status.ResolvedAddress=""이면 Unavailable 반환; agent 다이얼 미시도 | `test/component/csi_controller_extended_test.go`; PillarTarget 등록; Status.ResolvedAddress="" | 1) ControllerPublishVolumeRequest 전송 | gRPC Unavailable; AllowInitiator 0회; "no resolved address" 메시지 | `CSI-C`, `TgtCRD` |

---

## E3: CSI Node -- NodeStage / NodePublish / NodeUnpublish / NodeUnstage

> **Component test 이유:** CSI NodeServer의 전체 오케스트레이션을 검증한다.
> mockCSIConnector(NVMe-oF 스텁), mockCSIMounter(인메모리 마운트 테이블), t.TempDir()(상태 디렉터리)를
> 사용하여 커널 모듈이나 root 권한 없이 Stage/Publish/Unstage/Unpublish의 호출 순서, 에러 롤백,
> 멱등성, 상태 파일 관리를 검증한다.

### E3.1 전체 왕복 -- 마운트 접근 모드

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 22 | `TestCSINode_FullRoundTrip_MountAccess` | NodeStageVolume -> NodePublishVolume -> NodeUnpublishVolume -> NodeUnstageVolume 전체 마운트 접근 라이프사이클 | mockConnector.DevicePath="/dev/nvme0n1"; mockMounter 초기화; VolumeContext에 NQN/address/port 설정; 접근 모드 MOUNT | 1) NodeStageVolume; 2) NodePublishVolume; 3) NodeUnpublishVolume; 4) NodeUnstageVolume | 모든 단계 성공; Connector.Connect 1회, FormatAndMount 1회, 바인드 마운트 1회, 언마운트 2회, Disconnect 1회 | `CSI-N`, `Conn`, `Mnt`, `State` |

### E3.2 전체 왕복 -- 블록 접근 모드

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 23 | `TestCSINode_FullRoundTrip_BlockAccess` | 블록 디바이스 접근 모드 전체 라이프사이클 | mockConnector.DevicePath="/dev/nvme0n1"; 접근 모드 BLOCK | 1) NodeStageVolume; 2) NodePublishVolume; 3) NodeUnpublishVolume; 4) NodeUnstageVolume | 성공; 포맷/파일시스템 마운트 없이 디바이스 직접 노출 | `CSI-N`, `Conn`, `Mnt`, `State` |

### E3.3 디바이스 디스커버리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 24 | `TestCSINode_DeviceDiscovery` | NodeStage 시 NVMe-oF 연결 후 디바이스 경로 탐색 | mockConnector: GetDevicePath가 처음 몇 번 ""를 반환하다가 "/dev/nvme0n1" 반환 (폴링 시뮬레이션) | 1) NodeStageVolumeRequest 전송 | 성공; 디바이스 경로가 스테이징 상태에 저장됨 | `CSI-N`, `Conn`, `State` |

### E3.4 멱등성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 25 | `TestCSINode_IdempotentStage` | NodeStageVolume 2회 호출: 두 번째는 no-op | mockConnector.DevicePath 설정; 상태 파일 없음 | 1) NodeStageVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; Connector.Connect 재호출 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 26 | `TestCSINode_IdempotentPublish` | NodePublishVolume 2회 호출: 두 번째는 no-op | NodeStage 성공; NodePublish 1회 완료 | 1) NodePublishVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; 중복 마운트 없음 | `CSI-N`, `Mnt` |
| 27 | `TestCSINode_IdempotentUnstage` | NodeUnstageVolume 2회 호출: 두 번째는 no-op | NodeStage 성공; NodeUnstage 1회 완료 | 1) NodeUnstageVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; 이중 언마운트/연결 해제 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 28 | `TestCSINode_IdempotentUnpublish` | NodeUnpublishVolume 2회 호출: 두 번째는 no-op | NodePublish 성공; NodeUnpublish 1회 완료 | 1) NodeUnpublishVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; 오류 없음 | `CSI-N`, `Mnt` |

### E3.5 읽기 전용 마운트

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 29 | `TestCSINode_ReadonlyPublish` | NodePublishVolume에서 readonly=true 플래그가 마운터에 전달됨 | NodeStage 성공; mockMounter 초기화 | 1) NodeStageVolume; 2) Readonly=true; 접근 모드 MOUNT 로 NodePublishVolume 전송 | 성공; mockMounter가 readonly 옵션으로 호출됨 | `CSI-N`, `Mnt` |

### E3.6 상태 파일 영속성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 30 | `TestCSINode_StateFilePersistence` | NodeStage 후 스테이징 상태 파일이 StateDir에 저장되고, 이후 NodeUnstage 시 제거됨 | mockConnector.DevicePath 설정; t.TempDir()를 StateDir로 사용 | 1) NodeStageVolume 호출; 2) StateDir 파일 존재 확인; 3) NodeUnstageVolume 호출; 4) StateDir 파일 제거 확인 | 상태 파일 생성/삭제 타이밍이 CSI 호출과 일치 | `CSI-N`, `Conn`, `Mnt`, `State` |

### E3.7 노드 정보 및 역량

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 31 | `TestCSINode_NodeGetInfo` | NodeGetInfo가 올바른 NodeId와 토폴로지 키 반환 | NodeServer 초기화: nodeID="worker-1" | 1) NodeGetInfoRequest 전송 | NodeId="worker-1"; 토폴로지 키 존재 | `CSI-N` |
| 32 | `TestCSINode_NodeGetCapabilities` | NodeGetCapabilities가 지원 역량 목록 반환 | NodeServer 기본 초기화 | 1) NodeGetCapabilitiesRequest 전송 | STAGE_UNSTAGE_VOLUME 포함; 비어있지 않은 역량 목록 | `CSI-N` |

### E3.8 유효성 검사 오류

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 33 | `TestCSINode_ValidationErrors` | 필수 파라미터 누락 시 InvalidArgument 반환 | NodeServer 초기화 | 1) VolumeId="" 로 전송; 2) VolumeContext 키 누락 요청; 3) StagingTargetPath="" 요청 | 각 케이스에서 gRPC InvalidArgument; 커넥터/마운터 호출 없음 | `CSI-N` |

> ⚠️ CSI Sanity 도입 후 부분 대체 가능 — VolumeId=""과 StagingTargetPath="" 검증은 "NodeStageVolume fails with empty volume ID / staging target path" 테스트와 중복. VolumeContext 키 누락 검증은 pillar-csi 고유이므로 유지 필요

### E3.9 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 34 | `TestCSINode_ConnectError` | NVMe-oF 연결 실패 시 NodeStage 실패 | mockConnector.ConnectErr 설정 | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; 마운트 미실행; 상태 파일 미생성 | `CSI-N`, `Conn` |
| 35 | `TestCSINode_DisconnectError` | NVMe-oF 연결 해제 실패 시 NodeUnstage 실패 | NodeStage 성공; mockConnector.DisconnectErr 설정 | 1) NodeUnstageVolumeRequest 전송 | 비-OK gRPC 상태 | `CSI-N`, `Conn`, `State` |
| 36 | `TestCSINode_MountError` | 파일시스템 마운트 실패 시 NodeStage 실패 | Connect 성공; mockMounter.FormatAndMountErr 설정 | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; Disconnect 롤백 호출 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 37 | `TestCSINode_PublishMountError` | 바인드 마운트 실패 시 NodePublish 실패 | NodeStage 성공; mockMounter.BindMountErr 설정 | 1) NodePublishVolumeRequest 전송 | 비-OK gRPC 상태 | `CSI-N`, `Mnt` |

### E3.10 다중 볼륨 동시 처리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 38 | `TestCSINode_MultipleVolumes` | 여러 볼륨이 독립적인 스테이징 상태를 유지 | mockConnector.DevicePath 설정; 3개 별도 StagingTargetPath 준비 | 1) 볼륨A NodeStage; 2) 볼륨B NodeStage; 3) 볼륨C NodeStage | 모든 볼륨 성공적으로 스테이지; 상태 파일 간 충돌 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |

### E3.11 NodeStageVolume -- 파일시스템 타입별 동작

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 72 | `TestCSINode_StageVolume_XFS` | `fsType=xfs`로 NodeStageVolume 호출 시 FormatAndMount에 "xfs" 전달됨 | mockConnector.DevicePath="/dev/nvme3n1"; mockMounter 초기화 | 1) mountVolumeCapability("xfs", SINGLE_NODE_WRITER) 로 NodeStageVolumeRequest 전송 | FormatAndMount 1회; FsType="xfs"; 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 73 | `TestCSINode_StageVolume_DefaultFilesystem` | `fsType=""` (빈 문자열) 시 기본 파일시스템(ext4)으로 포맷 | mockConnector.DevicePath 설정 | 1) mountVolumeCapability("", SINGLE_NODE_WRITER) 로 NodeStageVolumeRequest 전송 | FormatAndMount 1회; FsType="" 또는 "ext4"; 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 74 | `TestCSINode_StageVolume_BlockAccessNoFormatAndMount` | 블록 접근 모드에서 FormatAndMount가 호출되지 않음 | mockConnector.DevicePath 설정 | 1) blockVolumeCapability(SINGLE_NODE_WRITER) 로 NodeStageVolumeRequest 전송 | FormatAndMount 0회; 블록 디바이스 직접 노출; 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |

### E3.12 NodeStageVolume -- NVMe-oF 어태치 파라미터 상세 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 75 | `TestCSINode_StageVolume_ConnectParamsForwarded` | VolumeContext의 NQN/address/port가 Connector.Connect 호출 시 정확히 전달됨 | mockConnector.DevicePath 설정; VolumeContext: target_id="nqn.test", address="192.168.0.10", port="4420" | 1) NodeStageVolumeRequest 전송; 2) mockConnector.ConnectCalls 검사 | Connector.Connect: SubsysNQN="nqn.test", TrAddr="192.168.0.10", TrSvcID="4420" | `CSI-N`, `Conn`, `State` |
| 76 | `TestCSINode_StageVolume_CustomPort` | 비표준 포트(4421)도 정확히 전달됨 | mockConnector.DevicePath 설정; VolumeContext.port="4421" | 1) NodeStageVolumeRequest 전송; 2) Connect 인수 검사 | Connector.Connect: TrSvcID="4421" | `CSI-N`, `Conn`, `State` |
| 77 | `TestCSINode_StageVolume_MissingAddress` | VolumeContext에서 address 키 누락 시 InvalidArgument | VolumeContext에 target_id와 port만 있고 address 키 없음 | 1) NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connect 미호출 | `CSI-N` |
| 78 | `TestCSINode_StageVolume_MissingPort` | VolumeContext에서 port 키 누락 시 InvalidArgument | VolumeContext에 target_id와 address만 있고 port 키 없음 | 1) NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connect 미호출 | `CSI-N` |
| 79 | `TestCSINode_StageVolume_AttachThenStateSaved` | NVMe-oF 연결 성공 후 상태 파일에 NQN이 저장됨 (재시작 복구 지원) | mockConnector.DevicePath 설정; 유효한 VolumeContext | 1) NodeStageVolumeRequest 전송; 2) StateDir/*.json 내용 검증 | 상태 파일 생성; NQN 포함; Connect 1회 | `CSI-N`, `Conn`, `State` |

### E3.13 NodeUnstageVolume -- 디태치 시나리오 상세

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 80 | `TestCSINode_UnstageVolume_DetachCallsDisconnect` | NodeUnstageVolume이 Connector.Disconnect를 정확한 NQN으로 호출 | NodeStageVolume 성공; 상태 파일에 NQN 기록됨 | 1) NodeStageVolume; 2) NodeUnstageVolumeRequest 전송; 3) Disconnect 인수 검증 | Disconnect 1회; NQN이 Stage 시 사용한 값과 동일 | `CSI-N`, `Conn`, `State` |
| 81 | `TestCSINode_UnstageVolume_NeverStagedIsIdempotent` | 스테이지된 적 없는 볼륨에 NodeUnstageVolume 호출 시 성공 (멱등성) | StateDir에 해당 VolumeId 상태 파일 없음 | 1) NodeUnstageVolumeRequest 직접 전송 | 성공; Disconnect 0회; Unmount 0회 | `CSI-N`, `State` |
| 82 | `TestCSINode_UnstageVolume_DetachFailsOnDisconnectError` | Connector.Disconnect 실패 시 gRPC Internal 반환 | NodeStage 성공; mockConnector.DisconnectErr 주입 | 1) NodeUnstageVolumeRequest 전송 | gRPC Internal; 상태 파일 미제거 | `CSI-N`, `Conn`, `State` |
| 83 | `TestCSINode_UnstageVolume_StateFileRemovedAfterSuccessfulDetach` | 정상 디태치 후 상태 파일 제거 확인 | NodeStage 성공; 상태 파일 존재 | 1) NodeUnstageVolumeRequest 전송; 2) StateDir 파일 수 확인 | NodeUnstage 성공 후 StateDir에 *.json 파일 0개 | `CSI-N`, `Conn`, `State` |

### E3.14 NodePublishVolume -- 다중 타깃 마운트

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 84 | `TestCSINode_PublishVolume_MultipleTargets` | 동일 스테이징 경로에서 두 타깃 경로로 NodePublishVolume 각각 성공 | NodeStage 1회 완료; 두 개의 서로 다른 targetPath 준비 | 1) NodePublishVolume(targetPath1); 2) NodePublishVolume(targetPath2) | 두 Mount 호출 모두 성공; source는 동일 stagingPath; target은 각각 다름 | `CSI-N`, `Mnt` |
| 85 | `TestCSINode_PublishVolume_UnpublishOneKeepsOther` | 두 타깃 중 하나 NodeUnpublish 시 나머지 마운트는 유지됨 | NodeStage + NodePublish x 2 완료 | 1) NodeUnpublishVolume(targetPath1); 2) targetPath2 마운트 상태 검증 | Unmount 1회; 남은 타깃 경로 마운트 유지; 스테이징 경로도 유지 | `CSI-N`, `Mnt` |

### E3.15 NodePublishVolume -- 접근 모드별 동작

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 86 | `TestCSINode_PublishVolume_SingleNodeWriter` | SINGLE_NODE_WRITER 접근 모드에서 쓰기 가능 마운트 | NodeStage 성공; mockMounter 초기화 | 1) AccessMode=SINGLE_NODE_WRITER; Readonly=false 로 NodePublishVolumeRequest 전송 | 성공; 마운트 옵션에 "ro" 없음 | `CSI-N`, `Mnt` |
| 87 | `TestCSINode_PublishVolume_SingleNodeReaderOnly` | SINGLE_NODE_READER_ONLY 접근 모드에서 읽기 전용 마운트 | NodeStage 성공; mockMounter 초기화 | 1) AccessMode=SINGLE_NODE_READER_ONLY; Readonly=true 로 NodePublishVolumeRequest 전송 | 성공; 마운트 옵션에 "ro" 포함 | `CSI-N`, `Mnt` |

### E3.16 NodeStageVolume -- 디바이스 대기 및 GetDevicePath 유효성 검사

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 171 | `TestNodeStageVolume_DeviceNeverAppears` | GetDevicePath가 항상 `("", nil)`을 반환하면 폴링 루프가 컨텍스트 타임아웃으로 종료 | `mockConnector.devicePath=""`; 컨텍스트 타임아웃 = `devicePollInterval * 3` | 1) NodeStageVolumeRequest 전송 | gRPC `DeadlineExceeded` 또는 `Internal` (컨텍스트 취소 경쟁); `FormatAndMount` 0회 호출; 패닉 없음 | `CSI-N`, `Conn` |
| 172 | `TestCSINode_NodeStageVolume_DeviceTimeout` | 200 ms 타임아웃으로 디바이스 폴링 루프 타임아웃 검증 | `mockConnector.getDeviceFn` 항상 `("", nil)` 반환; `ctx` 200 ms 타임아웃 | 1) NodeStageVolumeRequest 전송 | gRPC `DeadlineExceeded`; `Mounter.FormatAndMount` 0회 | `CSI-N`, `Conn` |
| 173 | `TestNodeStageVolume_DevicePathError` | GetDevicePath가 오류(`"sysfs error"`)를 반환 -> 폴링 중단 후 Internal | `mockConnector.devicePathErr=errors.New("sysfs error")` | 1) NodeStageVolumeRequest 전송 | gRPC `Internal`; `FormatAndMount` 0회; `Connect` 1회 | `CSI-N`, `Conn` |
| 174 | `TestCSINode_NodeStageVolume_GetDevicePathError` | GetDevicePath 오류 -> NodeStage `Internal` (컴포넌트 패키지) | `mockConnector.getDeviceFn`이 `errors.New("permission denied")` 반환 | 1) NodeStageVolumeRequest 전송 | gRPC `Internal`; `Mounter.FormatAndMount` 미호출 | `CSI-N`, `Conn` |

### E3.17 NodeStageVolume -- 기본 파일시스템 타입 및 접근 유형 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 175 | `TestNodeStageVolume_DefaultFsType` | `FsType=""`(빈 문자열) 일 때 `FormatAndMount`에 기본 파일시스템 타입(`"ext4"`)이 전달됨 | `mockConnector.devicePath="/dev/nvme0n1"`; `mountCap("")` | 1) NodeStageVolumeRequest 전송 | `FormatAndMount` 1회; `fsType = "ext4"` (= `defaultFsType` 상수); 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 176 | `TestNodeStageVolume_NoAccessType` | `VolumeCapability.AccessType` 필드 자체가 nil(Mount/Block 미설정) -> `InvalidArgument` | `VolumeCapability{AccessMode=SINGLE_NODE_WRITER}` | 1) NodeStageVolumeRequest 전송 | gRPC `InvalidArgument`; `Connect` 미호출; `FormatAndMount` 미호출 | `CSI-N` |

> ⚠️ CSI Sanity 도입 후 대체 가능 — "NodeStageVolume fails with no volume capability" 테스트와 중복

### E3.18 NodeStageVolume -- 재부팅 후 재스테이징 멱등성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 177 | `TestNodeStageVolume_IdempotentAfterUnmount` | 상태 파일 있음 + 스테이징 경로 언마운트된 상태에서 재스테이징 -> 재마운트 성공 | NodeStageVolume 1회 성공 후 `mounter.mountedPaths`에서 stagingPath 직접 제거(재부팅 시뮬레이션) | 1) 동일 요청으로 NodeStageVolumeRequest 재전송 | 성공; `Connect` 추가 호출 없음; `FormatAndMount` 2회 총 호출(초기 1회 + 재마운트 1회); stagingPath 마운트 상태로 복구 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 178 | `TestCSINode_NodeStage_Idempotent_StateFileExists` | 상태 파일 있음 + 경로 마운트된 상태에서 재스테이징 -> `Connect` 재호출 없음 (CI 컴포넌트 테스트) | NodeStageVolume 1회 성공 | 1) 동일 요청으로 NodeStageVolumeRequest 재전송; 2) `connectCalls` 수 비교 | 성공; `connectCalls` 증가 없음; `FormatAndMount` 재호출 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |

### E3.19 NodeUnstageVolume -- 오류 경로 및 예외 시나리오 심화

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 179 | `TestNodeUnstageVolume_UnmountedPath` | 스테이징 경로가 이미 언마운트된 상태(노드 재부팅 시뮬레이션) -- NodeUnstageVolume 성공하며 Disconnect는 호출됨 | NodeStageVolume 성공 후 `mounter.mountedPaths`에서 stagingPath 직접 제거 | 1) NodeUnstageVolumeRequest 전송 | 성공; `Disconnect` 1회 (NQN 정확히 일치); `Unmount` 0회 또는 no-op; 상태 파일 제거 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 180 | `TestNodeUnstageVolume_UnmountError` | `Mounter.Unmount`가 `"device busy"` 오류 반환 -> NodeUnstageVolume `Internal` | NodeStageVolume 성공; `mounter.unmountErr=errors.New("device busy")` 설정 | 1) NodeUnstageVolumeRequest 전송 | gRPC `Internal`; `Disconnect` 미호출(Unmount 실패 후 중단) 또는 구현별 가변 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 181 | `TestNodeUnstageVolume_DisconnectError` | `Connector.Disconnect`가 `"NVMe transport error"` 반환 -> NodeUnstageVolume `Internal` | NodeStageVolume 성공; `connector.disconnectErr=errors.New("NVMe transport error")` 설정 | 1) NodeUnstageVolumeRequest 전송 | gRPC `Internal`; `Unmount` 호출 완료 후 `Disconnect` 실패 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 182 | `TestNodeUnstageVolume_IdempotentSecondCall` | Stage -> Unstage -> Unstage(2회) -- 두 번째 Unstage는 no-op, Disconnect는 총 1회만 | NodeStageVolume + NodeUnstageVolume 각 1회 완료; 상태 파일 없음 | 1) NodeUnstageVolumeRequest 재전송 | 성공; `disconnectCalls` 여전히 1; 오류 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |

### E3.20 스테이징 상태 파일 관리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 183 | `TestCSINode_NodeUnstage_CorruptStateFile` | stateDir에 유효하지 않은 JSON(`"not valid json {{{"`) 상태 파일 직접 기록 후 NodeUnstageVolume 호출 | `volumeID`에 대응하는 `stateDir/<safeID>.json`에 corrupt bytes 기록 | 1) NodeUnstageVolumeRequest 전송 | 비-OK gRPC 상태 반환; 패닉 없음; `Disconnect` 미호출 | `CSI-N`, `State` |
| 184 | `TestCSINode_NodeStage_StateDirUnwritable` | stateDir을 `0555`(읽기 전용)로 변경 후 NodeStageVolume -- 상태 파일 쓰기 실패 | `os.Chmod(stateDir, 0o555)`; root가 아닌 사용자 실행 시에만 유효 (`t.Skip if root`) | 1) NodeStageVolumeRequest 전송 | 오류 반환(non-nil); 패닉 없음; `FormatAndMount` 호출 성공 후 상태 파일 쓰기 단계에서 실패 | `CSI-N`, `State` |
| 185 | `TestCSINode_NodeUnstage_StateFileMissingIsOK` | 상태 파일 없음 + 스테이징 경로 언마운트 -> NodeUnstageVolume 성공 (no-op) | stateDir에 해당 volumeID 상태 파일 없음; `mounter.IsMounted` 항상 `false` 반환 | 1) NodeUnstageVolumeRequest 전송 | 성공; `Disconnect` 0회; `Unmount` 0회 | `CSI-N`, `State` |
| 186 | `TestStageState_WriteReadDelete` | 상태 파일 쓰기 -> 읽기 -> 삭제 -> 삭제 후 읽기 단위 기능 라운드트립 | `NewNodeServerWithStateDir("n", nil, nil, stateDir)`; 쓰기 가능 stateDir | 1) `writeStageState(volumeID, {SubsysNQN:"nqn.test:..."})` 호출; 2) `readStageState(volumeID)` 호출; 3) `deleteStageState(volumeID)` 호출; 4) `readStageState` 재호출 | 2단계: SubsysNQN 동일; 4단계: nil 반환; 오류 없음 | `CSI-N`, `State` |
| 187 | `TestStageState_DeleteIdempotent` | 존재하지 않는 상태 파일 삭제 -> `ErrNotExist` 무시하고 성공 | `NewNodeServerWithStateDir`; stateDir에 해당 volumeID 파일 없음 | 1) `deleteStageState("pool/nonexistent")` 호출 | 오류 없음(nil 반환); 패닉 없음 | `CSI-N`, `State` |
| 188 | `TestStageState_VolumeIDSanitization` | VolumeID에 슬래시(`/`) 포함 시 상태 파일명 안전하게 변환 -- 경로 탈출(path traversal) 방지 | `["pool/vol-a", "pool/vol-b", "other-pool/vol-c"]` 각 ID에 대해 `writeStageState` 호출 | 1) 각 volumeID로 `writeStageState` 호출; 2) `readStageState` 호출; 3) stateDir 파일 목록 확인 | 각 ID에 대해 독립적으로 상태 읽기 성공; stateDir의 파일명에 `/` 없음; 파일 간 데이터 혼동 없음 | `CSI-N`, `State` |

### E3.21 NodePublishVolume -- 바인드 마운트, 읽기 전용, 멱등성 및 오류 처리 (단위 테스트)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 189 | `TestNodePublishVolume_MountAccess` | MOUNT 접근 유형에서 NodePublishVolume이 스테이징 경로 -> 타깃 경로로 바인드 마운트를 수행 | `newNodeTestEnv(t)` 초기화; `stagingPath=t.TempDir()`; `targetPath=t.TempDir()`; VolumeCapability=`mountCap("ext4")` | 1) `NodePublishVolumeRequest{VolumeId, StagingTargetPath, TargetPath, VolumeCapability}` 전송 | 성공(nil 오류); `mounter.IsMounted(targetPath)=true`; `mounter.mountCalls` 길이=1; `mountCalls[0].source=stagingPath`; `mountCalls[0].options`에 `"bind"` 포함 | `CSI-N`, `Mnt` |
| 190 | `TestNodePublishVolume_BlockAccess` | BLOCK 접근 유형에서 NodePublishVolume이 스테이징 경로를 타깃 경로로 바인드 마운트 | `newNodeTestEnv(t)` 초기화; VolumeCapability=`blockCap()` | 1) `NodePublishVolumeRequest{VolumeCapability: blockCap()}` 전송 | 성공; `mounter.IsMounted(targetPath)=true`; `mountCalls[0].source=stagingPath`; Mount 1회 호출 | `CSI-N`, `Mnt` |
| 191 | `TestNodePublishVolume_Readonly` | `Readonly=true`인 요청에서 마운트 옵션에 `"ro"`가 추가됨 | `newNodeTestEnv(t)` 초기화; `Readonly: true`; VolumeCapability=`mountCap("ext4")` | 1) `NodePublishVolumeRequest{Readonly: true}` 전송 | 성공; `mountCalls[0].options`에 `"ro"` 포함 | `CSI-N`, `Mnt` |
| 192 | `TestNodePublishVolume_Idempotent` | 동일 요청을 2회 호출하면 두 번째는 마운트를 수행하지 않음 (멱등성) | `newNodeTestEnv(t)` 초기화; 동일 `NodePublishVolumeRequest` 객체 준비 | 1) 1차 `NodePublishVolume` 호출; 2) 동일 인수로 2차 호출 | 두 호출 모두 성공; `mounter.mountCalls` 길이=1 (중복 마운트 없음) | `CSI-N`, `Mnt` |
| 193 | `TestNodePublishVolume_MissingVolumeID` | `VolumeId` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `VolumeId=""` 로 `NodePublishVolumeRequest` 전송 | gRPC `InvalidArgument`; 마운터 미호출 | `CSI-N` |
| 194 | `TestNodePublishVolume_MissingStagingTargetPath` | `StagingTargetPath` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `StagingTargetPath=""` 로 전송 | gRPC `InvalidArgument` | `CSI-N` |
| 195 | `TestNodePublishVolume_MissingTargetPath` | `TargetPath` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `TargetPath=""` 로 전송 | gRPC `InvalidArgument` | `CSI-N` |
| 196 | `TestNodePublishVolume_MissingVolumeCapability` | `VolumeCapability` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `VolumeCapability=nil` 로 전송 | gRPC `InvalidArgument` | `CSI-N` |

> ⚠️ CSI Sanity 도입 후 대체 가능 — TC 193-196은 "NodePublishVolume fails with empty volume ID / missing staging target path / missing target path / no volume capability" 테스트와 중복

| 197 | `TestNodePublishVolume_MountError` | `Mounter.Mount` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.mountErr = errors.New("mount failed")` | 1) 정상 파라미터로 전송 | gRPC `Internal`; 패닉 없음 | `CSI-N`, `Mnt` |
| 198 | `TestNodePublishVolume_IsMountedError` | `Mounter.IsMounted` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.isMountedErr = errors.New("isMounted failed")` | 1) 정상 파라미터로 전송 | gRPC `Internal`; Mount 미호출 | `CSI-N`, `Mnt` |

### E3.22 NodeUnpublishVolume -- 언마운트, 멱등성 및 오류 처리 (단위 테스트)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 199 | `TestNodeUnpublishVolume_Unmounts` | NodePublishVolume으로 마운트한 타깃 경로를 NodeUnpublishVolume이 정확히 해제 | `newNodeTestEnv(t)` 초기화; `NodePublishVolume` 선행 호출로 타깃 경로 마운트 | 1) `NodePublishVolume` 호출; 2) `IsMounted(targetPath)=true` 확인; 3) `NodeUnpublishVolume{VolumeId, TargetPath}` 전송; 4) `IsMounted(targetPath)` 재확인 | 성공; `IsMounted=false`; `unmountCalls` 길이=1; `unmountCalls[0]=targetPath` | `CSI-N`, `Mnt` |
| 200 | `TestNodeUnpublishVolume_Idempotent` | 타깃 경로가 이미 마운트 해제된 상태에서 NodeUnpublishVolume 호출 시 성공 (no-op) | `newNodeTestEnv(t)` 초기화; 마운트 없이 직접 호출 | 1) 마운트되지 않은 `targetPath`로 전송 | 성공(nil 오류); `unmountCalls` 길이=0 (Unmount 미호출) | `CSI-N`, `Mnt` |
| 201 | `TestNodeUnpublishVolume_TwiceMountsOnce` | Publish -> Unpublish -> Unpublish 사이클에서 Unmount는 정확히 1회 | `newNodeTestEnv(t)` 초기화; NodePublishVolume 1회 완료 | 1) `NodePublishVolume` 호출; 2) `NodeUnpublishVolume` 1차; 3) `NodeUnpublishVolume` 2차 | 두 `NodeUnpublishVolume` 모두 성공; `unmountCalls` 길이=1 (2차는 no-op) | `CSI-N`, `Mnt` |
| 202 | `TestNodeUnpublishVolume_MissingVolumeID` | `VolumeId` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `VolumeId=""` 로 전송 | gRPC `InvalidArgument`; 언마운터 미호출 | `CSI-N` |
| 203 | `TestNodeUnpublishVolume_MissingTargetPath` | `TargetPath` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `TargetPath=""` 로 전송 | gRPC `InvalidArgument` | `CSI-N` |

> ⚠️ CSI Sanity 도입 후 대체 가능 — TC 202-203은 "NodeUnpublishVolume fails with empty volume ID / missing target path" 테스트와 중복

| 204 | `TestNodeUnpublishVolume_UnmountError` | `Mounter.Unmount` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.mountedPaths[targetPath]=true`; `env.mounter.unmountErr = errors.New("device busy")` | 1) 전송 | gRPC `Internal`; EBUSY 오류 전파; 마운트 상태 유지 | `CSI-N`, `Mnt` |
| 205 | `TestNodeUnpublishVolume_IsMountedError` | `Mounter.IsMounted` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.isMountedErr = errors.New("isMounted failed")` | 1) 전송 | gRPC `Internal`; Unmount 미호출 | `CSI-N`, `Mnt` |

### E3.23 NodePublish/NodeUnpublish -- 전체 노드 라이프사이클 (단위 테스트)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 206 | `TestNodeFullLifecycle` | Stage -> Publish -> Unpublish -> Unstage 전체 노드 라이프사이클 단위 검증 | `newNodeTestEnv(t)` 초기화; `env.connector.devicePath="/dev/nvme0n1"`; `stagingPath=t.TempDir()`; `targetPath=t.TempDir()`; `VolumeContext`: `nqn`, `addr="192.0.2.10"`, `port="4420"` | 1) `NodeStageVolume` 전송; 2) `NodePublishVolume` 전송; 3) `IsMounted(targetPath)=true` 확인; 4) `NodeUnpublishVolume` 전송; 5) `IsMounted(targetPath)=false` 확인; 6) `NodeUnstageVolume` 전송 | 전 단계 성공; Stage 후 스테이징 경로 마운트; Publish 후 타깃 경로 마운트; Unpublish 후 타깃 경로 해제; Unstage 후 스테이징 경로 해제; `disconnectCalls` 길이=1 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

## E4: 교차-컴포넌트 CSI 라이프사이클

> **Component test 이유:** CSI ControllerServer와 NodeServer를 단일 mockAgentServer에 연결하여
> VolumeContext(NQN, address, port)가 Controller -> Node로 키 변환 없이 전달되는 교차-컴포넌트
> 데이터 흐름과 순서 제약을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 39 | `TestCSILifecycle_FullCycle` | Controller -> Node 전체 경로: CreateVolume -> ControllerPublish -> NodeStage -> NodePublish -> NodeUnpublish -> NodeUnstage -> ControllerUnpublish -> DeleteVolume | 단일 mockAgentServer; mockConnector/mockMounter 초기화; PillarTarget 등록 | 1) CreateVolume; 2) ControllerPublish; 3) NodeStage; 4) NodePublish; 5) NodeUnpublish; 6) NodeUnstage; 7) ControllerUnpublish; 8) DeleteVolume | 모든 단계 성공; agent 호출 순서 검증; VolumeContext 키가 NodeStage에 올바르게 전달됨 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `TgtCRD`, `VolCRD`, `State`, `gRPC` |
| 40 | `TestCSILifecycle_OrderingConstraints` | ControllerPublish 전 NodeStage 호출 -> FailedPrecondition | 공유 VolumeStateMachine 초기화; CreateVolume 완료; ControllerPublish 미호출 | 1) NodeStageVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |
| 41 | `TestCSILifecycle_IdempotentSteps` | 라이프사이클 각 단계를 두 번씩 호출해도 최종 상태 동일 | 전체 라이프사이클 1회 완료 | 1) 전체 라이프사이클 재호출 (각 단계 2회씩) | 모든 재호출 성공; 중복 agent 호출 없음 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `SM`, `gRPC` |
| 42 | `TestCSILifecycle_VolumeContextFlowThrough` | CreateVolume이 설정한 VolumeContext(NQN/address/port)가 NodeStageVolume에 키 변환 없이 그대로 전달됨 | mockAgentServer.ExportVolumeInfo에 특정 NQN/address/port 설정; mockConnector 초기화 | 1) CreateVolume; 2) ControllerPublish; 3) NodeStage; 4) mockConnector.ConnectCalls 검사 | NodeStage 시 mockConnector가 동일한 NQN/address/port로 호출됨 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `gRPC` |

---

## E6: 부분 실패 영속성

> **Component test 이유:** PillarVolume CRD를 통한 부분 실패 상태 추적, skipBackend 최적화,
> CRD 정리 로직을 fake k8s client와 mockAgentServer로 검증한다.
> 실제 ZFS/etcd 없이 상태 머신 전이와 CRD CRUD 오케스트레이션을 테스트한다.

### E6.1 부분 실패 CRD 생성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 49 | `TestCSIController_PartialFailure_CRDCreatedOnExportFailure` | agent.CreateVolume 성공 + agent.ExportVolume 실패 시 PillarVolume CRD가 Phase=CreatePartial, BackendCreated=true로 생성됨 | mockAgentServer: CreateVolume 성공; ExportVolumeErr 설정; PillarTarget 등록 | 1) CreateVolumeRequest 전송 | CreateVolume gRPC 실패; PillarVolume CRD 존재; Phase=CreatePartial; BackendCreated=true | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 50 | `TestCSIController_PartialFailure_RetryAdvancesToReady` | 부분 실패 후 재시도 시 CRD가 Phase=Ready로 전환되고 ExportInfo 채워짐 | Phase=CreatePartial CRD 존재; ExportVolume 이번엔 성공 | 1) 동일 파라미터로 CreateVolumeRequest 재전송 | 성공; CRD Phase=Ready; ExportInfo 채워짐; PartialFailure 초기화 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 51 | `TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry` | 재시도 시 skipBackend 최적화로 agent.CreateVolume 재호출 없음 | Phase=CreatePartial CRD 존재; ExportVolume 이번엔 성공 | 1) CreateVolumeRequest 재전송; 2) mockAgentServer.CreateVolumeCalls 횟수 확인 | agent.CreateVolume 총 1회만 호출 (재시도 포함) | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

### E6.2 삭제 시 CRD 정리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 52 | `TestCSIController_DeleteVolume_CleansUpCRD` | 성공적인 DeleteVolume이 PillarVolume CRD를 삭제 | CreateVolume 성공 후 PillarVolume CRD 존재 | 1) DeleteVolumeRequest 전송; 2) fake 클라이언트에서 CRD 조회 | PillarVolume CRD가 클러스터에서 제거됨 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| 53 | `TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates` | 부분 생성 상태의 볼륨도 DeleteVolume으로 올바르게 정리됨 | Phase=CreatePartial인 PillarVolume CRD 존재; mockAgentServer 정상 동작 | 1) DeleteVolumeRequest 전송; 2) fake 클라이언트에서 CRD 재조회 | 성공; CRD 제거; BackendCreated=true면 agent.DeleteVolume 호출됨 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

## E7: 게시 멱등성

> **Component test 이유:** ControllerPublishVolume과 NodePublishVolume의 멱등성 계약(동일 인수 재호출 시
> no-op 보장, 응답 일관성)을 mockAgentServer와 mockMounter로 검증한다.

### E7.1 ControllerPublishVolume 멱등성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 54 | `TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs` | 동일 인수로 ControllerPublishVolume 2회 호출: 두 호출 모두 성공, AllowInitiator는 총 2회 | 유효한 VolumeId/NodeId/VolumeContext; mockAgentServer 정상 | 1) ControllerPublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; PublishContext 동일; CreateVolume/ExportVolume 미트리거 | `CSI-C`, `Agent`, `gRPC` |
| 55 | `TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes` | 서로 다른 node handle에 대한 ControllerPublishVolume은 각 `CSINode` annotation에서 해석된 identity로 독립적으로 성공 | 동일 VolumeId; 서로 다른 NodeId와 서로 다른 `CSINode` annotation 준비 | 1) ControllerPublishVolume(NodeId1); 2) ControllerPublishVolume(NodeId2) | 두 호출 모두 성공; AllowInitiator는 서로 다른 resolved identity로 각 1회씩 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |

### E7.2 NodePublishVolume 멱등성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 56 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget` | 동일 타깃 경로로 NodePublishVolume 2회 호출: 두 번째는 no-op | NodeStage 성공; mockMounter 초기화 | 1) NodePublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; 응답 동일; 중복 마운트 없음 | `CSI-N`, `Mnt` |
| 57 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleBlockAccess` | 블록 접근 모드에서도 NodePublishVolume 2회 호출 멱등성 보장 | NodeStage(BLOCK 접근 모드) 성공 | 1) NodePublishVolume(BLOCK) 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; 중복 블록 디바이스 노출 없음 | `CSI-N`, `Mnt` |
| 58 | `TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble` | 읽기 전용 NodePublishVolume 2회 호출 멱등성 | NodeStage 성공; Readonly=true 설정 | 1) NodePublishVolume(Readonly=true) 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; 응답 동일 | `CSI-N`, `Mnt` |

---

## E8: mTLS 핸드셰이크

> **Component test 이유:** PillarTarget 컨트롤러와 pillar-agent 간 mTLS 신뢰 경계를
> 인메모리 testcerts 패키지로 검증한다. 실제 cert-manager 없이 TLS 핸드셰이크 성공/실패 경로를 테스트한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 59 | `TestMTLSController_AgentConnectedAuthenticated` | 올바른 mTLS 자격증명으로 연결 시 PillarTarget 상태 AgentConnected=True/Authenticated | testcerts.New()로 동일 CA의 서버/클라이언트 인증서 생성; mTLS 서버 + 컨트롤러 설정 | 1) mTLS 서버 기동; 2) 컨트롤러 PillarTarget 조정 실행; 3) PillarTarget 조건 검사 | AgentConnected 조건 True; Reason=Authenticated | `mTLS`, `TgtCRD`, `gRPC` |
| 60 | `TestMTLSController_PlaintextDialRejected` | 평문 클라이언트가 mTLS 서버에 거부됨 | 서버: mTLS 설정; 클라이언트: insecure.NewCredentials() 사용 | 1) 평문 dial 시도; 2) 컨트롤러 조정 실행; 3) PillarTarget 조건 검사 | AgentConnected 조건 False; Reason=HealthCheckFailed 또는 TLSHandshakeFailed | `mTLS`, `TgtCRD`, `gRPC` |
| 61 | `TestMTLSController_WrongCAClientRejected` | 다른 CA가 서명한 클라이언트 인증서는 거부됨 | 서버: CA1 서명 인증서; 클라이언트: CA2 서명 인증서 생성 | 1) 잘못된 CA의 인증서로 dial 시도; 2) 컨트롤러 조정 실행; 3) PillarTarget 조건 검사 | AgentConnected 조건 False | `mTLS`, `TgtCRD`, `gRPC` |

---

## E9: Agent gRPC 디스패치

> **Component test 이유:** agent.Server의 gRPC 직렬화/역직렬화 레이어와 mock backend 디스패치를
> 실제 gRPC 리스너(localhost:0)를 통해 검증한다. 실제 ZFS 커널 모듈 없이 Agent의 RPC 라우팅,
> 에러 매핑, configfs 상태 관리를 테스트한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 62 | `TestAgent_GetCapabilities` | 실제 gRPC 연결을 통한 GetCapabilities 호출이 올바른 역량 목록 반환 | agentE2EMockBackend 초기화; 실제 gRPC 리스너(localhost:0) 기동 | 1) GetCapabilitiesRequest 전송 | ZFS_ZVOL backend + NVMEOF_TCP 프로토콜 포함 | `Agent`, `ZFS`, `gRPC` |
| 63 | `TestAgent_HealthCheck` | 실제 gRPC 연결을 통한 HealthCheck 호출 | sysModuleZFSPath를 tmpdir의 존재하는 파일로 설정; tmpdir configfs 루트 설정 | 1) HealthCheckRequest 전송 | ZFS 모듈 체크 HEALTHY; configfs 체크 결과 포함 | `Agent`, `NVMeF`, `gRPC` |
| 64 | `TestAgent_RoundTrip` | CreateVolume -> ExportVolume -> AllowInitiator -> DenyInitiator -> UnexportVolume -> DeleteVolume 전체 왕복을 실제 gRPC를 통해 검증 | agentE2EMockBackend; tmpdir configfs; 실제 gRPC 리스너 | 1) CreateVolume; 2) ExportVolume; 3) AllowInitiator; 4) DenyInitiator; 5) UnexportVolume; 6) DeleteVolume | 모든 단계 성공; configfs 상태 변화 검증 | `Agent`, `ZFS`, `NVMeF`, `gRPC` |
| 65 | `TestAgent_ReconcileStateRestoresExports` | ReconcileState가 재시작 후 configfs 엔트리를 올바르게 복원 | 빈 tmpdir configfs; 볼륨 목록 준비 | 1) ReconcileState(볼륨 목록) 호출; 2) tmpdir configfs 디렉터리 존재 확인 | ReconcileState 후 모든 볼륨의 configfs 서브시스템 디렉터리 존재 | `Agent`, `NVMeF`, `gRPC` |
| 66 | `TestAgent_ErrorHandling` | 다양한 오류 시나리오(잘못된 pool ID, backend 오류 등)가 적절한 gRPC 상태 코드로 매핑 | agentE2EMockBackend에 각 오류 조건 설정; 실제 gRPC 리스너 | 1) 잘못된 pool ID로 CreateVolume; 2) backend 오류 주입 후 각 RPC 호출; 3) 오류 코드 검증 | NotFound/InvalidArgument/Internal 등 명세에 맞는 gRPC 코드 반환 | `Agent`, `ZFS`, `gRPC` |
| 67 | `TestAgent_AllPhase1RPCs` | Phase 1에서 지원하는 모든 RPC를 한 테스트에서 순차적으로 검증 | agentE2EMockBackend; tmpdir configfs; 실제 gRPC 리스너 | 1) GetCapabilities; 2) HealthCheck; 3) CreateVolume; 4) ExportVolume; 5) AllowInitiator; 6) DenyInitiator; 7) UnexportVolume; 8) DeleteVolume | 모든 Phase 1 RPC 성공; 오류 없음 | `Agent`, `ZFS`, `NVMeF`, `gRPC` |

---

## E11: 볼륨 확장 오케스트레이션

> **Component test 이유:** CSI 볼륨 확장의 두 단계(ControllerExpandVolume -> agent.ExpandVolume,
> NodeExpandVolume -> Resizer.ResizeFS)를 mockAgentServer와 mockResizer로 연결하여
> 교차-컴포넌트 확장 경계를 검증한다.

### E11.1 ControllerExpandVolume -- 에이전트 위임 및 node_expansion_required

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 88 | `TestCSIExpand_ControllerExpandVolume_ForwardsToAgent` | ControllerExpandVolume이 agent.ExpandVolume을 올바른 VolumeId/BackendType/RequestedBytes로 호출하고 node_expansion_required=true를 반환 | mockAgentServer.ExpandVolumeResp.CapacityBytes=2GiB; 유효한 VolumeId | 1) CapacityRange.RequiredBytes=2GiB 로 ControllerExpandVolumeRequest 전송 | 성공; CapacityBytes=2GiB; NodeExpansionRequired=true; agent.ExpandVolume 1회 호출 | `CSI-C`, `Agent`, `gRPC` |
| 89 | `TestCSIExpand_ControllerExpandVolume_AgentReturnsZeroCapacity` | agent.ExpandVolume이 CapacityBytes=0을 반환하면 RequiredBytes를 폴백으로 사용 | mockAgentServer.ExpandVolumeResp.CapacityBytes=0; CapacityRange.RequiredBytes=3GiB | 1) ControllerExpandVolumeRequest 전송 | 성공; CapacityBytes=3GiB (RequiredBytes 폴백); NodeExpansionRequired=true | `CSI-C`, `Agent`, `gRPC` |

### E11.2 NodeExpandVolume -- 파일시스템 타입별 리사이즈

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 90 | `TestCSIExpand_NodeExpandVolume_Ext4` | NodeExpandVolume이 ext4 파일시스템에서 mockResizer.ResizeFS("ext4")를 호출 | mockResizer 주입; VolumeCapability.MountVolume.FsType="ext4"; VolumePath 설정 | 1) NodeExpandVolumeRequest 전송 | 성공; ResizeFS 1회 호출; FsType="ext4"; CapacityBytes=RequiredBytes | `CSI-N`, `Mnt` |
| 91 | `TestCSIExpand_NodeExpandVolume_XFS` | NodeExpandVolume이 xfs 파일시스템에서 mockResizer.ResizeFS("xfs")를 호출 | mockResizer 주입; VolumeCapability.MountVolume.FsType="xfs" | 1) NodeExpandVolumeRequest 전송 | 성공; ResizeFS 1회 호출; FsType="xfs" | `CSI-N`, `Mnt` |

### E11.3 전체 확장 왕복

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 92 | `TestCSIExpand_FullExpandRoundTrip` | CreateVolume -> ControllerExpandVolume -> NodeExpandVolume 전체 확장 흐름 | 단일 mockAgentServer; mockResizer 주입; VolumeId 동일 사용 | 1) CreateVolume; 2) ControllerExpandVolume(newSize); 3) NodeExpandVolume | ControllerExpandVolume: CapacityBytes=newSize, NodeExpansionRequired=true; NodeExpandVolume: ResizeFS 1회 | `CSI-C`, `CSI-N`, `Agent`, `Mnt`, `gRPC` |
| 93 | `TestCSIExpand_ControllerExpandVolume_Idempotent` | 이미 확장된 볼륨에 동일한 크기로 ControllerExpandVolume 재호출 -- 멱등성 | mockAgentServer: ExpandVolume 항상 현재 크기 반환 | 1) ControllerExpandVolume 1회; 2) 동일 RequiredBytes로 재호출 | 두 호출 모두 성공; agent.ExpandVolume 2회 호출; 오류 없음 | `CSI-C`, `Agent`, `gRPC` |

### E11.4 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 94 | `TestCSIExpand_ControllerExpandVolume_AgentFails` | agent.ExpandVolume 실패 시 ControllerExpandVolume이 오류 코드를 전파 | mockAgentServer.ExpandVolumeErr=gRPC ResourceExhausted | 1) ControllerExpandVolumeRequest 전송 | ControllerExpandVolume이 ResourceExhausted 반환; NodeExpansionRequired 없음 | `CSI-C`, `Agent`, `gRPC` |
| 95 | `TestCSIExpand_NodeExpandVolume_ResizerFails` | mockResizer.ResizeFS 실패 시 NodeExpandVolume이 Internal 반환 | mockResizer.ResizeFSErr 설정 | 1) NodeExpandVolumeRequest 전송 | gRPC Internal; 오류 메시지에 "resize" 포함 | `CSI-N`, `Mnt` |

---

## E15: 리소스 고갈 에러 전파

> **Component test 이유:** 스토리지 풀 용량 부족, 연결 타임아웃 등 리소스 고갈 시나리오에서
> mock에 주입된 에러 코드가 CSI 계층을 통해 CO로 정확히 전파되고 상태가 오염되지 않음을 검증한다.
> 실제 ZFS 풀 고갈 없이 에러 코드 매핑과 상태 일관성에 집중한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 117 | `TestCSIExhaustion_CreateVolume_PoolFull` | 스토리지 풀 가득 참 시 CreateVolume 실패 -- gRPC 오류 코드 전파 검증 | mockAgentServer.CreateVolumeErr=gRPC ResourceExhausted; PillarTarget 등록 | 1) CreateVolumeRequest 전송 | gRPC ResourceExhausted 또는 Internal 반환; PillarVolume CRD 미생성; ExportVolume 미호출 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| 118 | `TestCSIExhaustion_ExpandVolume_ExceedsPoolCapacity` | ControllerExpandVolume이 풀 용량 초과 시도 -- 오류 전파 검증 | mockAgentServer.ExpandVolumeErr=gRPC ResourceExhausted; 유효한 VolumeId | 1) ControllerExpandVolumeRequest 전송 | gRPC ResourceExhausted 반환; NodeExpansionRequired 없음 | `CSI-C`, `Agent`, `gRPC` |
| 119 | `TestCSIExhaustion_CreateVolume_InsufficientStorage` | 요청 용량이 사용 가능 용량보다 큰 경우 | mockAgentServer.CreateVolumeErr=gRPC OutOfRange; PillarTarget 등록 | 1) CreateVolumeRequest 전송 | 비-OK gRPC 상태; 볼륨 미생성; 패닉 없음 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| 120 | `TestCSIExhaustion_CreateVolume_ConsecutiveFailures` | agent.CreateVolume이 연속 5회 실패해도 상태 오염 없음 | mockAgentServer.CreateVolumeErr 항상 반환; PillarTarget 등록 | 1) CreateVolumeRequest 5회 반복 전송 | 5회 모두 비-OK gRPC 상태; PillarVolume CRD 0개; fake k8s 클라이언트 상태 오염 없음; 패닉 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 121 | `TestCSIExhaustion_NodeStage_ConnectTimeout` | NVMe-oF 연결 타임아웃 시 NodeStage 실패 -- 상태 파일 미생성 검증 | mockConnector.ConnectErr=errors.New("connection timed out") | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; StateDir에 상태 파일 0개; FormatAndMount 미호출 | `CSI-N`, `Conn`, `State` |
| 122 | `TestCSIExhaustion_NodeStage_DeviceNeverAppears` | NVMe-oF 연결 성공 후 디바이스가 폴링 타임아웃 내에 나타나지 않음 | mockConnector.Connect 성공; DevicePath="" (항상 빈 경로); 폴링 타임아웃=50ms | 1) NodeStageVolumeRequest 전송 (short timeout context 사용) | 비-OK gRPC 상태; 상태 파일 미생성 | `CSI-N`, `Conn`, `State` |

---

## E16: 동시 작업 안전성

> **Component test 이유:** 여러 고루틴이 동시에 CSI RPC를 호출할 때 데드락, 패닉, 데이터 손상이
> 발생하지 않음을 검증한다. mockAgentServer(mutex 보호), fake k8s client(thread-safe)를 사용하여
> 애플리케이션 레벨 동시성 안전성에 집중한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 123 | `TestCSIConcurrent_CreateVolume_SameNameNoPanic` | 동일 이름으로 5개 고루틴이 동시에 CreateVolume 호출해도 패닉/데드락 없음 | mockAgentServer 정상 동작; PillarTarget 등록; 5초 타임아웃 | 1) 5개 goroutine을 동시에 시작; 각각 동일 볼륨 이름으로 CreateVolumeRequest 전송; 2) WaitGroup 완료 대기 | 5개 고루틴 모두 5초 내 완료; 패닉 없음; 일부 성공/나머지 AlreadyExists 가능 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 124 | `TestCSIConcurrent_CreateVolume_DifferentNames` | 5개 고루틴이 각각 다른 이름의 볼륨을 동시에 생성 | mockAgentServer 정상 동작; PillarTarget 등록 | 1) 5개 goroutine 동시 시작; 2) WaitGroup 완료 대기 | 5개 볼륨 모두 성공; PillarVolume CRD 5개; 데이터 손상 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 125 | `TestCSIConcurrent_CreateDelete_Interleaved` | 볼륨 생성과 삭제를 동시에 수행 -- 최종 상태 일관성 검증 | mockAgentServer 정상 동작; PillarTarget 등록 | 1) goroutine A: CreateVolumeRequest; 2) goroutine B: 동시에 동일 VolumeId로 DeleteVolumeRequest; 3) 양측 완료 대기 | 두 연산 모두 완료; 최종 상태는 생성 또는 삭제 중 하나; CRD 상태 일관성; 패닉 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 126 | `TestCSIConcurrent_NodeStage_SameVolumeDifferentPaths` | 동일 VolumeId를 서로 다른 스테이징 경로로 동시에 NodeStage 호출 | mockConnector 정상 동작; 동일 VolumeId; 유효한 VolumeContext | 1) 2개 goroutine 동시 시작; 2) WaitGroup 완료 대기 | 두 호출 모두 완료; 데드락 없음; 각각 별도 상태 파일 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 127 | `TestCSIConcurrent_NodePublish_MultipleTargets` | 스테이지 완료 후 3개 고루틴이 서로 다른 targetPath로 동시 NodePublish | NodeStage 1회 성공 완료; mockMounter 정상 | 1) NodeStage; 2) 3개 goroutine 동시 시작; 3) WaitGroup 완료 대기 | 3개 NodePublish 모두 성공; mockMounter.MountCalls=3; 데드락 없음 | `CSI-N`, `Mnt`, `State` |
| 128 | `TestCSIConcurrent_AllowInitiator_MultipleNodes` | 다른 NodeId에 대해 동시에 ControllerPublishVolume 3회 호출 | mockAgentServer.AllowInitiator 정상; 동일 VolumeId; 3개 `CSINode` annotation 준비 | 1) 3개 goroutine 동시 시작; 2) WaitGroup 완료 대기 | 3개 모두 완료; 데드락 없음; AllowInitiator 3회; 각각 다른 resolved identity | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 129 | `TestCSIConcurrent_UnpublishVolume_Race` | 3개 노드에서 동시에 ControllerUnpublishVolume 호출 | mockAgentServer.DenyInitiator 정상; 3개 NodeId 각각 publish 완료 | 1) 3개 goroutine 동시 시작; 2) WaitGroup 완료 대기 | 3개 모두 완료; 패닉 없음; DenyInitiator 3회 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

## E17: 정리 검증

> **Component test 이유:** CSI 연산 실패 또는 성공 후 부가 상태(상태 파일, PillarVolume CRD,
> 마운트 테이블, NVMe-oF 연결)가 올바르게 정리되는지 검증한다.
> mock을 통해 리소스 누수가 없음을 확인하는 것이 핵심 목표이다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 130 | `TestCSICleanup_NodeStage_ConnectFailureNoStateFile` | NodeStageVolume에서 Connect 실패 시 상태 파일이 생성되지 않음 | mockConnector.ConnectErr=errors.New("connect refused"); 임시 StateDir | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; StateDir에 상태 파일 0개; FormatAndMount 미호출 | `CSI-N`, `Conn`, `State` |
| 131 | `TestCSICleanup_NodeStage_MountFailureDisconnects` | FormatAndMount 실패 시 이미 완료된 NVMe-oF 연결이 정리(롤백)됨 | mockConnector.Connect 성공; mockMounter.FormatAndMountErr 설정 | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; Connector.Disconnect 1회 호출(롤백); StateDir에 상태 파일 0개 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 132 | `TestCSICleanup_NodeUnstage_FailurePreservesStateFile` | Connector.Disconnect 실패 시 상태 파일이 보존됨 -- 재시도 가능 상태 유지 | NodeStage 성공; mockConnector.DisconnectErr=errors.New("disconnect failed") | 1) NodeUnstageVolumeRequest 전송 | 비-OK gRPC 상태; StateDir에 상태 파일 유지(재시도 보존) | `CSI-N`, `Conn`, `Mnt`, `State` |
| 133 | `TestCSICleanup_DeleteVolume_RemovesAllCRD` | 성공적 DeleteVolume 후 PillarVolume CRD가 fake k8s 클라이언트에서 완전히 삭제됨 | CreateVolume 성공; PillarVolume CRD 존재 확인 | 1) CreateVolumeRequest; 2) CRD 존재 확인; 3) DeleteVolumeRequest; 4) CRD 재조회 | DeleteVolume 성공; CRD 조회 시 NotFound | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 134 | `TestCSICleanup_CreatePartial_DeleteVolumeCleansCRD` | 부분 생성 상태(Phase=CreatePartial) CRD도 DeleteVolume으로 정리됨 | CreateVolume 실패(CreatePartial CRD 생성됨); DeleteVolume용 mockAgentServer 정상 | 1) CreateVolumeRequest(ExportVolume 실패); 2) Phase=CreatePartial CRD 존재 확인; 3) DeleteVolumeRequest | DeleteVolume 성공; CRD 제거 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `SM`, `gRPC` |
| 135 | `TestCSICleanup_FullLifecycle_NoResourceLeak` | 전체 라이프사이클 완료 후 모든 상태가 완전히 정리됨 | mockAgentServer/mockConnector/mockMounter 정상; PillarTarget 등록; 임시 StateDir | 1-8) 8단계 전체 라이프사이클 | StateDir 상태 파일 0개; PillarVolume CRD 0개; mockMounter 마운트 테이블 빈 상태; Connector.DisconnectCalls=1 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `TgtCRD`, `VolCRD`, `State`, `gRPC` |
| 136 | `TestCSICleanup_RepeatedCreateDelete` | 동일 이름의 볼륨을 10회 반복 생성/삭제해도 상태 오염 없음 | mockAgentServer 정상; PillarTarget 등록; 동일 볼륨 이름 재사용 | 1) 루프 10회: CreateVolume -> 성공 확인 -> CRD 존재 확인 -> DeleteVolume -> CRD 삭제 확인 | 모든 10회 성공; 매 반복 후 PillarVolume CRD 0개; 누적 오류 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 137 | `TestCSICleanup_RepeatedStageUnstage` | 동일 볼륨을 5회 반복 NodeStage/NodeUnstage해도 상태 파일 누적 없음 | mockConnector/mockMounter 정상; 임시 StateDir; 동일 VolumeId | 1) 루프 5회: NodeStage -> 상태 파일 확인 -> NodeUnstage -> StateDir 빈 상태 확인 | 모든 5회 성공; 매 반복 후 StateDir 빈 상태; Connect/Disconnect 각 5회씩 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

## E18: Agent 다운 에러 핸들링

> **Component test 이유:** CSI 에이전트가 응답 불가, 연결 거부, 타임아웃 등 "다운" 상태일 때
> CSI ControllerServer가 올바른 gRPC 상태 코드와 진단 메시지로 CO에 보고하는 에러 전파 경로와,
> 에이전트 재시작 후 ReconcileState를 통한 configfs 복원 로직을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 138 | `TestCSIController_CreateVolume_AgentUnreachable` | `AgentDialer`가 `codes.Unavailable("connection refused")` 반환 시 `CreateVolume`이 `Unavailable`을 그대로 전파 | `newCSIControllerTestEnvWithDialErr(t, status.Error(codes.Unavailable, "connection refused"))` 주입; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송 | `codes.Unavailable` 반환; PillarVolume CRD 미생성; `agent.CreateVolume` 미호출 | `CSI-C`, `gRPC` |
| 139 | `TestCSIErrors_CreateVolume_AgentUnreachable_PlainError` | `AgentDialer`가 평문 Go error 반환 시 오류가 CO에 전파됨 | `newCSIControllerTestEnvWithDialErr(t, errors.New("dial tcp 192.168.1.10:9500: connect: connection refused"))` 주입; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송 | 비-OK gRPC 상태; 에이전트 호출 없음; 패닉 없음 | `CSI-C`, `gRPC` |
| 140 | `TestCSIErrors_CreateVolume_AgentDeadlineExceeded` | `agent.CreateVolume`이 `codes.DeadlineExceeded` 반환 시 `ControllerServer`가 CO에 비-OK 상태 전파 | `csiMockAgent.createVolumeFn`이 `status.Error(codes.DeadlineExceeded, "agent: ZFS command timed out")` 반환 | 1) `CreateVolumeRequest` 전송 | 비-OK gRPC 상태; PillarVolume CRD 미생성 | `CSI-C`, `Agent`, `gRPC` |
| 141 | `TestCSIErrors_ControllerExpand_AgentDeadlineExceeded` | `agent.ExpandVolume`이 `codes.DeadlineExceeded` 반환 시 비-OK 전파 | `csiMockAgent.expandVolumeFn`이 DeadlineExceeded 반환 | 1) `ControllerExpandVolumeRequest` 전송 | 비-OK gRPC 상태; `NodeExpansionRequired` 없음 | `CSI-C`, `Agent`, `gRPC` |
| 142 | `TestCSIErrors_DeleteVolume_AgentDeadlineExceeded` | `agent.UnexportVolume`이 `codes.DeadlineExceeded` 반환 시 비-OK 전파 | `csiMockAgent.unexportVolumeFn`이 DeadlineExceeded 반환 | 1) `DeleteVolumeRequest` 전송 | 비-OK gRPC 상태; 삭제 작업 중단 | `CSI-C`, `Agent`, `gRPC` |
| 143 | `TestCSIErrors_ControllerPublish_AllowInitiatorFails` | `agent.AllowInitiator`가 configfs 쓰기 실패(`codes.Internal`) 반환 시 오류 전파 | `csiMockAgent.allowInitiatorFn`이 Internal 반환; 유효한 `CSINode` annotation 존재 | 1) `ControllerPublishVolumeRequest` 전송 | 비-OK gRPC 상태; 오류 삼킴 없음 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |

---

## E21.1: 잘못된 CR 런타임 처리

> **Component test 이유:** CSI 컨트롤러가 이미 존재하나 잘못된 상태를 가진 CR을 런타임에 조회했을 때
> 적절한 gRPC 오류 코드를 반환하고 패닉 없이 처리함을 검증한다.
> fake k8s client에 잘못된 필드 값을 직접 주입하여 테스트한다 (웹훅/스키마 검증 미실행).

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 145 | `TestCSIInvalidCR_CreateVolume_TargetResolvedAddressEmpty` | PillarTarget이 존재하나 `Status.ResolvedAddress`가 빈 문자열 | fake k8s 클라이언트에 `PillarTarget{status:{resolvedAddress:""}}` 등록 | 1) `CreateVolumeRequest` 전송 | `codes.Unavailable`; "no resolved address" 포함; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| 146 | `TestCSIInvalidCR_CreateVolume_TargetSpecBothNil` | PillarTarget의 `spec.nodeRef`와 `spec.external`이 모두 nil | fake k8s 클라이언트에 `PillarTarget{spec:{}, status:{resolvedAddress:""}}` 등록 | 1) `CreateVolumeRequest` 전송 | `codes.Unavailable`; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| 147 | `TestCSIInvalidCR_ControllerPublish_TargetNoAddress` | ControllerPublishVolume 시 PillarTarget의 `Status.ResolvedAddress`가 빈 문자열 | PillarVolume CRD 존재(Phase=Ready); PillarTarget `status.resolvedAddress=""`; `CSINode` annotation 준비 | 1) `ControllerPublishVolumeRequest` 전송 | `codes.Unavailable`; `agent.AllowInitiator` 미호출 | `CSI-C`, `TgtCRD`, `VolCRD` |
| 148 | `TestCSIInvalidCR_LoadState_UnknownPhase` | PillarVolume CRD가 정의되지 않은 Phase 값을 가질 때 `LoadStateFromPillarVolumes`가 `StateNonExistent`로 처리 | fake k8s 클라이언트에 `PillarVolume{status:{phase:"GarbagePhase"}}` 등록 | 1) `LoadStateFromPillarVolumes` 호출; 2) SM 상태 조회 | 오류 없음; SM 상태 = `StateNonExistent`; 패닉 없음 | `CSI-C`, `VolCRD` |
| 149 | `TestCSIInvalidCR_LoadState_ListFailure` | `k8sClient.List(PillarVolumeList)` 실패 시 오류를 반환하고 SM 상태를 오염시키지 않음 | fake k8s 클라이언트를 List 실패 mock으로 교체 | 1) `LoadStateFromPillarVolumes` 호출 | 오류 반환(non-nil); SM 상태 변경 없음; 패닉 없음 | `CSI-C`, `VolCRD` |
| 150 | `TestCSIInvalidCR_ControllerExpand_TargetNoAddress` | ControllerExpandVolume 시 PillarTarget `Status.ResolvedAddress`가 빈 문자열 | PillarVolume CRD(Phase=Ready); PillarTarget `status.resolvedAddress=""` | 1) `ControllerExpandVolumeRequest` 전송 | `codes.Unavailable`; `agent.ExpandVolume` 미호출 | `CSI-C`, `TgtCRD` |

---

## E24: 8단계 전체 라이프사이클

> **Component test 이유:** CSI 볼륨 라이프사이클의 8단계 전체 체인에서 부분 실패 발생 시의 오류 전파,
> 상태 일관성, 멱등성, 롤백/정리 경로를 통합적으로 검증한다.
> 각 단계의 실패 시나리오를 mock 에러 주입으로 시뮬레이션하고 교차-컴포넌트 동작을 확인한다.

### E24.1 정상 경로 -- 8단계 완전 체인

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.1-1 | `TestCSILifecycle_FullCycle` | 8단계 전체 라이프사이클 정상 경로 완전 검증 | `csiLifecycleEnv` 초기화: mockAgentServer, mockCSIConnector, mockCSIMounter, t.TempDir() StateDir; PillarTarget CRD 등록 | 1-8) CreateVolume -> ControllerPublish -> NodeStage -> NodePublish -> NodeUnpublish -> NodeUnstage -> ControllerUnpublish -> DeleteVolume | 모든 단계 성공; agent 6개 RPC 각 1회; mockConnector.Connect/Disconnect 각 1회; PillarVolume CRD 삭제됨 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `TgtCRD`, `VolCRD`, `State`, `gRPC` |
| E24.1-2 | `TestCSILifecycle_VolumeContextFlowThrough` | CreateVolume의 VolumeContext(NQN/address/port)가 키 변환 없이 NodeStageVolume의 mockConnector.Connect 인수로 전달됨 | `csiLifecycleEnv`; mockAgentServer.ExportVolumeInfo 사전 설정 | 1) CreateVolume; 2) VolumeContext 추출; 3) NodeStageVolume; 4) mockConnector.ConnectCalls[0] 검증 | Connect.SubsysNQN == VolumeContext["target_id"]; 키 변환 없음 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `gRPC` |
| E24.1-3 | `TestCSILifecycle_OrderingConstraints` | 8단계 체인에서 올바른 순서 준수 검증 | 동일한 `csiLifecycleEnv` | Phase 1-8 순차 실행, 각 Phase 후 중간 상태 검증 | Phase 3 후 ConnectCalls 1개; Phase 6 후 DisconnectCalls 1개; 최종 6개 agent RPC 각 1회 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `gRPC` |
| E24.1-4 | `TestCSILifecycle_IdempotentSteps` | 8단계 각 단계를 두 번씩 호출해도 오류 없이 최종 상태 동일 | `csiLifecycleEnv`; callTwice 헬퍼 | 각 단계 callTwice | 모든 재호출 성공; 두 번째 호출은 no-op | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `SM`, `gRPC` |

### E24.2 CreateVolume 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.2-1 | `TestCSIController_PartialFailure_CRDCreatedOnExportFailure` | agent.CreateVolume 성공 + agent.ExportVolume 실패 시 CRD Phase=CreatePartial | mockAgentServer: ExportVolumeErr 설정 | 1) CreateVolumeRequest | 오류; CRD Phase=CreatePartial; BackendCreated=true | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.2-2 | `TestCSIController_PartialFailure_RetryAdvancesToReady` | 부분 실패 후 재시도 시 CRD Phase=Ready 전환 | Phase=CreatePartial CRD 존재; ExportVolume 성공 | 1) CreateVolumeRequest 재전송 | 성공; Phase=Ready; ExportInfo 채워짐 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.2-3 | `TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry` | skipBackend 최적화 -- 재시도 시 agent.CreateVolume 재호출 없음 | Phase=CreatePartial CRD 존재 | 1) CreateVolumeRequest 재전송 | agent.CreateVolume 총 1회; agent.ExportVolume 총 2회 | `CSI-C`, `Agent`, `VolCRD`, `gRPC`, `SM` |
| E24.2-4 | `TestCSIZvolNoDup_ExactlyOneZvolAfterExportFailureRetry` | export 실패 후 재시도 시 zvol 중복 생성 없음 | statefulZvolAgentServer; ExportVolumeErr 주입/제거 | 1) CreateVolume(실패); 2) zvol 수=1; 3) CreateVolume(성공); 4) zvol 수=1 유지 | zvol 총 1개; skipBackend 발동; CRD Phase=Ready | `CSI-C`, `Agent`, `VolCRD`, `gRPC`, `SM` |

### E24.3 ControllerPublish 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.3-1 | `TestCSIController_ControllerPublishVolume_AgentAllowInitiatorFails` | AllowInitiator 실패 시 오류 반환 | mockAgentServer: AllowInitiatorErr=gRPC Internal | 1) ControllerPublishVolumeRequest | 비-OK gRPC 상태 | `CSI-C`, `Agent`, `gRPC` |
| E24.3-2 | `TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs` | 동일 인수 2회 호출 시 멱등 성공 | mockAgentServer 정상; 동일 인수 | 1) ControllerPublishVolume; 2) 재호출 | 두 호출 성공; AllowInitiator 2회 | `CSI-C`, `Agent`, `gRPC` |

### E24.4 NodeStage 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.4-1 | `TestCSINode_NodeStageVolume_ConnectFails` | NVMe-oF 연결 실패 시 오류 반환 | mockCSIConnector: ConnectErr 설정 | 1) NodeStageVolumeRequest | 비-OK gRPC 상태 | `CSI-N`, `Conn` |
| E24.4-2 | `TestCSINode_NodeStageVolume_FormatFails` | 디바이스 포맷 실패 시 오류 반환 | mockCSIMounter: FormatAndMountErr 설정 | 1) NodeStageVolumeRequest | 비-OK gRPC 상태 | `CSI-N`, `Conn`, `Mnt` |
| E24.4-3 | `TestCSINode_NodeStageVolume_IdempotentReStage` | 이미 스테이징된 볼륨 재호출 시 멱등 성공 | 스테이징 완료 상태 | 1) NodeStageVolume 재호출 | 성공; 연결/포맷 재시도 없음 | `CSI-N`, `Conn`, `Mnt` |

### E24.5 NodePublish 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.5-1 | `TestCSINode_NodePublishVolume_MountFails` | 바인드 마운트 실패 시 오류 반환 | mockCSIMounter: MountErr 설정 | 1) NodePublishVolumeRequest | 비-OK gRPC 상태 | `CSI-N`, `Mnt` |
| E24.5-2 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget` | 2회 호출 시 두 번째 no-op | 동일 TargetPath | 1) NodePublishVolume; 2) 재호출 | 두 호출 성공; Mount 1회 | `CSI-N`, `Mnt` |
| E24.5-3 | `TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble` | 읽기 전용 2회 호출 멱등성 | Readonly=true | 1) NodePublishVolume(ro); 2) 재호출 | 두 호출 성공; "ro" 마운트 1회 | `CSI-N`, `Mnt` |

### E24.6 NodeUnpublish 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.6-1 | `TestCSINode_NodeUnpublishVolume_UnmountFails` | 언마운트 실패 시 오류 반환 | mockCSIMounter: UnmountErr 설정 | 1) NodeUnpublishVolumeRequest | 비-OK gRPC 상태 | `CSI-N`, `Mnt` |
| E24.6-2 | `TestCSINode_NodeUnpublishVolume_AlreadyUnpublished` | 이미 언마운트된 경로에 멱등 성공 | TargetPath 이미 언마운트 | 1) NodeUnpublishVolumeRequest | 성공 | `CSI-N`, `Mnt` |

### E24.7 NodeUnstage 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.7-1 | `TestCSINode_NodeUnstageVolume_DisconnectFails` | NVMe-oF 연결 해제 실패 시 오류 반환 | mockCSIConnector: DisconnectErr 설정 | 1) NodeUnstageVolumeRequest | 비-OK gRPC 상태 | `CSI-N`, `Conn` |
| E24.7-2 | `TestCSINode_NodeUnstageVolume_AlreadyUnstaged` | 이미 언스테이징된 볼륨에 멱등 성공 | StagingTargetPath 이미 미마운트 | 1) NodeUnstageVolumeRequest | 성공 | `CSI-N`, `Conn`, `Mnt` |

### E24.8 ControllerUnpublish 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.8-1 | `TestCSIController_ControllerUnpublishVolume_AgentDenyInitiatorFails` | DenyInitiator 실패 시 오류 반환 | mockAgentServer: DenyInitiatorErr=gRPC Internal | 1) ControllerUnpublishVolumeRequest | 비-OK gRPC 상태 | `CSI-C`, `Agent`, `gRPC` |
| E24.8-2 | `TestCSIController_ControllerUnpublishVolume_NotFound` | 존재하지 않는 볼륨에 멱등 성공 | mockAgentServer: DenyInitiatorErr=NotFound | 1) ControllerUnpublishVolumeRequest | 성공 또는 NotFound (idempotent) | `CSI-C`, `Agent`, `gRPC` |

### E24.9 DeleteVolume 단계 실패/복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.9-1 | `TestCSIController_DeleteVolume_AgentDeleteVolumeFailsTransient` | agent.DeleteVolume 일시적 실패 시 오류 반환 | mockAgentServer: DeleteVolumeErr=gRPC Internal | 1) DeleteVolumeRequest | 비-OK gRPC 상태; CRD 미제거 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.9-2 | `TestCSIController_DeleteVolume_CleansUpCRD` | 성공적 DeleteVolume이 CRD 제거 | PillarVolume CRD Phase=Ready | 1) DeleteVolumeRequest; 2) CRD 조회 | 성공; CRD NotFound | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.9-3 | `TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates` | CreatePartial 상태 볼륨의 DeleteVolume 성공 및 CRD 정리 | PillarVolume CRD Phase=CreatePartial; BackendCreated=true | 1) DeleteVolumeRequest; 2) CRD 조회 | 성공; agent.DeleteVolume 호출; CRD NotFound | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.9-4 | `TestCSIZvolNoDup_ZvolRegistryReflectsDeleteAfterPartialCreate` | 부분 생성 상태 DeleteVolume 후 zvol 레지스트리 1->0 감소 | statefulZvolAgentServer; CreatePartial 상태 | 1) DeleteVolume; 2) zvol 수=0; 3) CRD NotFound | zvol 0개; CRD 제거됨 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

### E24.10 중단된 라이프사이클 정리 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.10-1 | `TestCSILifecycle_OutOfOrderOperationsDetected` | 순서 제약 위반 탐지 및 FailedPrecondition 반환 | SM 초기화; NodeStage 미완료 | 1) NodePublishVolume(NodeStageVolume 없이) | `codes.FailedPrecondition`; 상태 변경 없음 | `CSI-N`, `SM` |
| E24.10-2 | `TestCSIController_DeleteVolume_NonExistentVolume` | 존재하지 않는 VolumeId에 DeleteVolume 멱등 성공 | VolumeId가 CRD에 없음 | 1) DeleteVolumeRequest | 성공 (CSI 명세) | `CSI-C`, `Agent`, `gRPC` |

---

## E29: LVM 파라미터 전파

> **Component test 이유:** CSI ControllerServer가 LVM 백엔드 타입의 StorageClass 파라미터를 파싱하여
> agent gRPC 요청에 LvmVolumeParams로 전달하는 오케스트레이션과, 프로비저닝 모드 오버라이드
> 3계층(Pool -> Binding -> PVC annotation) 병합 로직을 mockAgentServer로 검증한다.

### E29.1 LVM CreateVolume -- 정상 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 263 | `TestCSIController_CreateVolume_LVM_Linear` | LVM linear 모드 CreateVolume 시 agent에 BackendType=LVM, LvmVolumeParams{VolumeGroup, ProvisionMode="linear"} 전달 | PillarTarget 등록; PillarPool(type=lvm-lv, lvm.volumeGroup="data-vg", lvm.provisioningMode=linear); PillarProtocol(nvmeof-tcp); PillarBinding; mockAgentServer 정상 | 1) CreateVolumeRequest 전송 | 성공; BackendType=BACKEND_TYPE_LVM; LvmVolumeParams.VolumeGroup="data-vg"; ProvisionMode="linear"; VolumeId에 "lvm-lv" 포함 | `CSI-C`, `Agent`, `LVM`, `TgtCRD`, `VolCRD`, `gRPC` |
| 264 | `TestCSIController_CreateVolume_LVM_Thin` | LVM thin 모드 CreateVolume 시 agent에 ProvisionMode="thin" 전달 | PillarPool(lvm.thinPool="thin-pool-0", lvm.provisioningMode=thin) | 1) CreateVolumeRequest 전송 | 성공; ProvisionMode="thin"; VolumeGroup="data-vg" | `CSI-C`, `Agent`, `LVM`, `TgtCRD`, `VolCRD`, `gRPC` |
| 265 | `TestCSIController_CreateVolume_LVM_VolumeIdFormat` | LVM VolumeId가 "target/nvmeof-tcp/lvm-lv/data-vg/pvc-xxx" 5세그먼트 형식 | 동일 | 1) CreateVolumeRequest; 2) VolumeId 세그먼트 파싱 | VolumeId에 5개 슬래시 구분 세그먼트; 3번째="lvm-lv"; 4번째="data-vg" | `CSI-C`, `VolCRD` |

### E29.2 LVM 프로비저닝 모드 오버라이드 3계층

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 266 | `TestCSIController_LVM_ModeOverride_PoolDefault` | Pool 레벨 provisioningMode가 기본값으로 agent에 전달 | PillarPool.lvm.provisioningMode="thin"; Binding에 오버라이드 없음; PVC annotation 없음 | 1) CreateVolumeRequest; 2) agent.CreateVolumeCalls[0] 확인 | LvmVolumeParams.ProvisionMode="thin" | `CSI-C`, `Agent`, `LVM` |
| 267 | `TestCSIController_LVM_ModeOverride_BindingOverridesPool` | Binding 레벨이 Pool 기본값 오버라이드 | Pool="thin"; Binding.overrides.backend.lvm.provisioningMode="linear" | 1) CreateVolumeRequest | ProvisionMode="linear" | `CSI-C`, `Agent`, `LVM` |
| 268 | `TestCSIController_LVM_ModeOverride_PVCAnnotationOverridesBinding` | PVC annotation이 Binding 레벨 오버라이드 | Binding="linear"; PVC annotation "pillar-csi.bhyoo.com/lvm-mode"="thin" | 1) CreateVolumeRequest(PVC annotation 포함) | ProvisionMode="thin" | `CSI-C`, `Agent`, `LVM` |
| 269 | `TestCSIController_LVM_ModeOverride_AbsentUsesBackendDefault` | 모든 레이어 미지정 시 빈 문자열 -> agent backend 기본값 | Pool.lvm.provisioningMode=""; Binding 없음 | 1) CreateVolumeRequest | ProvisionMode="" | `CSI-C`, `Agent`, `LVM` |
| 269a | `TestCSIController_LVM_ModeOverride_InvalidPVCAnnotation` | PVC annotation에 잘못된 lvm-mode("striped") 시 agent가 거부 | PVC annotation lvm-mode="striped" | 1) CreateVolumeRequest | gRPC InvalidArgument | `CSI-C`, `Agent`, `LVM` |
| 269b | `TestCSIController_LVM_ModeOverride_EmptyPVCAnnotation_FallsThrough` | PVC annotation lvm-mode="" 빈 문자열 시 Binding 레벨 값 사용 | Binding="thin"; PVC annotation lvm-mode="" | 1) CreateVolumeRequest | ProvisionMode="thin" | `CSI-C`, `Agent`, `LVM` |

### E29.3 LVM DeleteVolume 및 ControllerExpandVolume

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 270 | `TestCSIController_DeleteVolume_LVM` | LVM VolumeId로 DeleteVolume 시 agent.UnexportVolume -> agent.DeleteVolume 순서 호출 | LVM CreateVolume 성공; PillarVolume CRD 존재 | 1) DeleteVolumeRequest | 성공; UnexportVolume 1회, DeleteVolume 1회; CRD 제거 | `CSI-C`, `Agent`, `LVM`, `VolCRD`, `gRPC` |
| 271 | `TestCSIController_ControllerExpandVolume_LVM` | LVM 볼륨의 ControllerExpandVolume이 agent.ExpandVolume(BACKEND_TYPE_LVM) 호출 | LVM CreateVolume 성공; Phase=Ready | 1) ControllerExpandVolumeRequest(2GiB) | 성공; BackendType=BACKEND_TYPE_LVM; node_expansion_required=true | `CSI-C`, `Agent`, `LVM`, `gRPC` |

### E29.4 LVM 전체 왕복

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 272 | `TestCSIController_LVM_FullRoundTrip` | LVM 백엔드로 CreateVolume -> ControllerPublishVolume -> ControllerUnpublishVolume -> DeleteVolume 전체 왕복 | PillarTarget/Pool(lvm-lv)/Protocol(nvmeof-tcp)/Binding 등록; mockAgentServer 정상 | 1) CreateVolume; 2) ControllerPublishVolume; 3) ControllerUnpublishVolume; 4) DeleteVolume | 모든 단계 성공; agent 호출 순서/BackendType/LvmVolumeParams 검증 | `CSI-C`, `Agent`, `LVM`, `TgtCRD`, `VolCRD`, `gRPC` |

---

## E30: LVM 중복 방지 최적화

> **Component test 이유:** E6.3(zvol 중복 방지)과 동일한 skipBackend 최적화가 LVM LV에도 적용됨을 검증한다.
> statefulLVAgentServer의 lvRegistry를 통해 LV 존재 여부를 추적하고, 부분 실패 재시도 시
> agent.CreateVolume이 재호출되지 않는 오케스트레이션 로직을 테스트한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 273 | `TestCSILVMNoDup_ExactlyOneLVAfterExportFailureRetry` | export 실패 후 재시도 시 LV가 정확히 1개만 존재 -- skipBackend 동작 확인 | `statefulLVAgentServer` 초기화; `ExportVolumeErr` 주입 후 재시도 전 제거; LVM 백엔드 파라미터 | 1) CreateVolume(ExportVolume 실패) -> 오류; 2) LV 수=1, CreateVolume 호출=1, CRD Phase=CreatePartial; 3) CreateVolume 재시도(성공); 4) LV 수=1, CreateVolume 호출=1 유지, CRD Phase=Ready | 재시도 후 LV 총 1개; agent.CreateVolume 총 1회 (skipBackend); agent.ExportVolume 총 2회; CRD Phase=Ready | `CSI-C`, `Agent`, `LVM`, `VolCRD`, `gRPC`, `SM` |
| 274 | `TestCSILVMNoDup_LVRegistryReflectsDeleteAfterPartialCreate` | 부분 생성 후 DeleteVolume 시 LV 레지스트리 1->0 감소 | 부분 실패 후 PillarVolume CRD 존재; `statefulLVAgentServer` | 1) CreateVolume(ExportVolume 실패) -> LV 1개; 2) CRD에서 VolumeID 읽기; 3) DeleteVolume; 4) LV 수=0, CRD NotFound | DeleteVolume 성공; LV 레지스트리 0; CRD 제거 | `CSI-C`, `Agent`, `LVM`, `VolCRD`, `gRPC` |
| 275 | `TestCSILVMNoDup_MultipleRetriesNeverDuplicate` | 연속 3회 export 실패 후 최종 성공 -- 매 재시도마다 LV 수 1 유지 | `retryFails=3`; `statefulLVAgentServer`; 3회 실패 후 `ExportVolumeErr=nil` | 1) 3회 연속 CreateVolume(실패); 2) 각 실패 후 LV 수=1, CreateVolume 호출=1; 3) 4번째 성공 | 모든 재시도에서 LV 수 1; agent.CreateVolume 총 1회; agent.ExportVolume 총 4회; 최종 Ready | `CSI-C`, `Agent`, `LVM`, `VolCRD`, `gRPC`, `SM` |

---

## PRD 갭 — 추가 TC

### C-NEW-1: acl=false → ControllerPublish/Unpublish no-op

> **Component test 근거:** Controller가 PillarProtocol의 acl 필드를 읽고, false이면 mock agent의 AllowInitiator/DenyInitiator를 호출하지 않는 분기 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-1-1 | `TestCSIController_ControllerPublishVolume_ACLDisabled_NoOp` | acl=false일 때 ControllerPublishVolume이 AllowInitiator를 호출하지 않고 성공 반환 | PillarProtocol.spec.acl=false; PillarVolume CRD 존재(Phase=Ready); mockAgentServer; CSINode annotation 존재 | 1) ControllerPublishVolumeRequest 전송 | 성공(OK); AllowInitiator 호출 0회 | `CSI-C`, `Agent` |
| C-NEW-1-2 | `TestCSIController_ControllerUnpublishVolume_ACLDisabled_NoOp` | acl=false일 때 ControllerUnpublishVolume이 DenyInitiator를 호출하지 않고 성공 | PillarProtocol.spec.acl=false; PillarVolume CRD 존재; mockAgentServer | 1) ControllerUnpublishVolumeRequest 전송 | 성공(OK); DenyInitiator 호출 0회 | `CSI-C`, `Agent` |

---

### C-NEW-2: acl=false → ExportVolume allow_any_host=1

> **Component test 근거:** Agent가 acl=false 파라미터를 받으면 configfs attr_allow_any_host에 "1"을 기록하고, allowed_hosts를 설정하지 않는 분기 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-2-1 | `TestAgent_ExportVolume_ACLDisabled_AllowAnyHost` | acl=false로 ExportVolume 시 configfs의 attr_allow_any_host에 "1" 기록 | agent.NewServer(backends, t.TempDir()); mockVolumeBackend; acl=false 파라미터 | 1) ExportVolumeRequest(acl=false) 전송; 2) tmpdir/nvmet/subsystems/<NQN>/attr_allow_any_host 파일 읽기 | attr_allow_any_host = "1"; allowed_hosts 디렉토리 비어있음 | `Agent`, `NVMeF` |

---

### C-NEW-3: modprobe 실패 → protocol capabilities 제외

> **Component test 근거:** Agent가 커널 모듈 로드 상태를 확인하여 사용 불가능한 프로토콜을 capabilities에서 제외하는 런타임 감지 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-3-1 | `TestAgent_Capabilities_ModuleUnavailable_ProtocolExcluded` | 커널 모듈이 로드되지 않은 프로토콜은 capabilities에서 제외 | agent.NewServer에 모듈 체크 함수를 mock으로 주입; NVMe-oF 모듈 → false(미로드); ZFS backend → 정상 | 1) GetCapabilitiesRequest 전송 | capabilities.protocols에 nvmeof-tcp 없음; backends에 zfs-zvol 있음 | `Agent` |

---

### C-NEW-4: 커널 모듈 미로드 → NodeStageVolume 명확한 에러

> **Component test 근거:** CSI NodeServer가 커널 모듈 미로드 상태를 감지하여 명확한 FailedPrecondition 에러를 반환하는 사전 조건 검증 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-4-1 | `TestCSINode_NodeStageVolume_ModuleNotAvailable` | 커널 모듈이 로드되지 않은 상태에서 NodeStageVolume 호출 시 명확한 에러 반환 | NodeServer 초기화; 모듈 체크 함수 mock → false; 유효한 VolumeContext | 1) NodeStageVolumeRequest 전송 | gRPC FailedPrecondition; 에러 메시지에 "module not available" 포함; Connector.Connect 미호출 | `CSI-N` |

---

### C-NEW-5: NVMe-oF 타임아웃 파라미터 개별 전파

> **Component test 근거:** CSI NodeServer가 VolumeContext의 타임아웃 파라미터를 파싱하여 Connector.Connect 옵션에 개별 전달하는 오케스트레이션 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-5-1 | `TestCSINode_NodeStageVolume_CtrlLossTmoForwarded` | VolumeContext의 ctrl-loss-tmo가 Connector.Connect opts에 전달 | mockConnector; VolumeContext에 "ctrl-loss-tmo"="600" 설정 | 1) NodeStageVolumeRequest 전송; 2) mockConnector.ConnectCalls[0].Opts 검사 | ConnectOpts.CtrlLossTmo == 600 | `CSI-N`, `Conn` |
| C-NEW-5-2 | `TestCSINode_NodeStageVolume_ReconnectDelayForwarded` | VolumeContext의 reconnect-delay가 Connector.Connect opts에 전달 | mockConnector; VolumeContext에 "reconnect-delay"="10" 설정 | 1) NodeStageVolumeRequest 전송 | ConnectOpts.ReconnectDelay == 10 | `CSI-N`, `Conn` |

---

### C-NEW-6: mkfsOptions 전파

> **Component test 근거:** CSI NodeServer가 VolumeContext의 mkfs-options를 파싱하여 FormatAndMount에 전달하는 오케스트레이션 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-6-1 | `TestCSINode_NodeStageVolume_MkfsOptionsForwarded` | VolumeContext의 mkfs-options가 FormatAndMount에 전달 | mockMounter; VolumeContext에 "mkfs-options"="-E lazy_itable_init=0" 설정; AccessType=Mount | 1) NodeStageVolumeRequest 전송; 2) mockMounter.FormatAndMountCalls[0].MkfsOptions 검사 | MkfsOptions에 "-E", "lazy_itable_init=0" 포함 | `CSI-N`, `Mnt` |

---

### C-NEW-7: Exponential backoff 타이밍

> **Component test 근거:** CSI Controller의 agent 호출 재시도 정책이 지수적 백오프를 구현하고 최대 재시도 횟수를 준수하는 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-7-1 | `TestCSIController_RetryPolicy_ExponentialBackoff` | agent 에러 시 재시도 간격이 지수적으로 증가 | mockAgentServer.CreateVolumeErr=transient error; 재시도 관찰기(타임스탬프 기록) | 1) CreateVolumeRequest 전송; 2) 재시도 타임스탬프 간격 검사 | 2번째 재시도 간격 > 1번째; 3번째 > 2번째; 지수적 증가 패턴 | `CSI-C` |
| C-NEW-7-2 | `TestCSIController_RetryPolicy_MaxRetriesRespected` | 최대 재시도 횟수 초과 시 최종 에러 반환 | mockAgentServer.CreateVolumeErr=항상 실패; maxRetries=3 설정 | 1) CreateVolumeRequest 전송 | agent.CreateVolume 호출 ≤ maxRetries; 최종 에러 반환 | `CSI-C` |

---

### C-NEW-8: gRPC 자동 재연결

> **Component test 근거:** CSI Controller의 gRPC 클라이언트가 일시적 연결 끊김 후 자동으로 재연결하여 후속 RPC를 성공적으로 수행하는 복원력 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-8-1 | `TestCSIController_GRPCReconnect_AfterTransientFailure` | 일시적 연결 끊김 후 gRPC가 자동 재연결 | mockAgentServer 정상 시작; 첫 번째 CreateVolume 성공 확인 | 1) CreateVolume 성공; 2) mockAgentServer 리스너 종료; 3) mockAgentServer 재시작(동일 주소); 4) CreateVolume 재호출 | 4단계 CreateVolume 성공; gRPC가 자동 재연결됨 | `CSI-C`, `gRPC` |

---

### C-NEW-9: NodeGetVolumeStats mock 기반

> **Component test 근거:** CSI NodeServer의 NodeGetVolumeStats가 마운트된 볼륨의 파일시스템 통계를 올바르게 반환하고, 미존재 경로에 대해 NotFound를 반환하는 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-9-1 | `TestCSINode_NodeGetVolumeStats_Filesystem` | NodeGetVolumeStats가 마운트된 볼륨의 bytes/inodes 사용량 반환 | NodeStage 완료; mockMounter가 StatFS 결과 반환(total=10GiB, used=3GiB, available=7GiB, inodes 유사) | 1) NodeGetVolumeStatsRequest(volumeId, volumePath) 전송 | Usage[0].Unit=BYTES: total=10GiB, used=3GiB, available=7GiB; Usage[1].Unit=INODES: 해당 값 | `CSI-N`, `Mnt` |
| C-NEW-9-2 | `TestCSINode_NodeGetVolumeStats_VolumeNotFound` | 존재하지 않는 볼륨 경로 → NotFound | NodeServer 초기화; volumePath="/nonexistent" | 1) NodeGetVolumeStatsRequest 전송 | gRPC NotFound | `CSI-N` |

---

### C-NEW-10: Agent Snapshot RPC 에러

> **Component test 근거:** Agent의 Snapshot RPC가 미구현 상태에서 Unimplemented를 명시적으로 반환하여 CSI 명세를 준수하는 에러 처리 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-10-1 | `TestAgent_CreateSnapshot_Unimplemented` | Agent의 CreateSnapshot RPC가 Unimplemented 반환 | agent.NewServer(backends, t.TempDir()); mockVolumeBackend | 1) CreateSnapshotRequest 전송 | gRPC Unimplemented | `Agent` |
| C-NEW-10-2 | `TestAgent_DeleteSnapshot_Unimplemented` | Agent의 DeleteSnapshot RPC가 Unimplemented 반환 | 동일 | 1) DeleteSnapshotRequest 전송 | gRPC Unimplemented | `Agent` |

---

### C-NEW-11: NodeGetInfo GetInitiatorID 형식 검증

> **Component test 근거:** CSI NodeServer의 NodeGetInfo가 Connector의 GetInitiatorID 결과를 node_id로 반환할 때 NQN 형식을 준수하는 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-11-1 | `TestCSINode_NodeGetInfo_NQNFormat` | NodeGetInfo가 NVMe-oF NQN 형식의 node_id 반환 | mockConnector.GetInitiatorIDResult = "nqn.2014-08.org.nvmexpress:uuid:test-1234" | 1) NodeGetInfoRequest 전송; 2) NodeId 형식 검사 | NodeId가 "nqn." 접두사로 시작 | `CSI-N` |

---

### C-NEW-12: PVC annotation fs-override

> **Component test 근거:** CSI Controller가 PVC annotation의 fs-override를 파싱하여 PillarBinding 기본값을 오버라이드하고 VolumeContext에 반영하는 오케스트레이션 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-12-1 | `TestCSIController_CreateVolume_FsOverrideAnnotation` | PVC annotation `pillar-csi.bhyoo.com/fs-override`의 fsType/mkfsOptions가 VolumeContext에 반영 | PillarBinding.overrides.fsType="ext4"; PVC annotation fs-override: {fsType: "xfs", mkfsOptions: ["-K"]} | 1) CreateVolumeRequest(PVC annotation 포함) 전송 | VolumeContext["fs-type"]="xfs"; VolumeContext["mkfs-options"]="-K"; PillarBinding 기본값(ext4) 오버라이드됨 | `CSI-C`, `Agent` |

---

### C-NEW-13: K8s Event 기록

> **Component test 근거:** CSI Controller가 프로비저닝 실패 시 K8s Event를 기록하여 운영 가시성을 제공하는 오케스트레이션 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-13-1 | `TestCSIController_CreateVolume_FailureRecordsEvent` | CreateVolume 실패 시 K8s Event가 기록됨 | fake EventRecorder 주입; mockAgentServer.CreateVolumeErr 설정 | 1) CreateVolumeRequest 전송(실패 유도); 2) EventRecorder 채널에서 Event 확인 | Event에 "ProvisioningFailed" reason 포함; agent 에러 메시지 포함 | `CSI-C` |
| C-NEW-13-2 | `TestCSIController_DeleteVolume_FailureRecordsEvent` | DeleteVolume 실패 시 Event 기록 | fake EventRecorder; mockAgentServer.DeleteVolumeErr 설정 | 1) DeleteVolumeRequest 전송(실패) | Event에 실패 reason 포함 | `CSI-C` |

---

### C-NEW-14: Prometheus 메트릭 카운터

> **Component test 근거:** CSI Controller의 볼륨 생성/에러 Prometheus 메트릭 카운터가 올바르게 증가하는 관측성 로직.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| C-NEW-14-1 | `TestMetrics_CreateVolume_IncrementsCounter` | CreateVolume 성공 시 볼륨 수 메트릭 증가 | prometheus testutil; mockAgentServer; PillarTarget 등록 | 1) CreateVolumeRequest 3회 전송; 2) testutil.CollectAndCount(pillarVolumeCreatedTotal) | 카운터 값 = 3 | `CSI-C` |
| C-NEW-14-2 | `TestMetrics_CreateVolume_ErrorIncrementsErrorCounter` | CreateVolume 실패 시 에러율 메트릭 증가 | mockAgentServer.CreateVolumeErr 설정 | 1) CreateVolumeRequest 2회 전송(실패); 2) pillarVolumeErrorTotal 확인 | 에러 카운터 값 = 2 | `CSI-C` |
