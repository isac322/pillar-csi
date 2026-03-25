# E2E 테스트 케이스 명세

이 문서는 pillar-csi E2E 테스트의 **권위 있는 명세**이다. E2E 테스트는 여러 서브시스템을
**실제 컴포넌트 경계**에서 연결하여, 개별 컴포넌트 테스트가 검증할 수 없는
**교차-컴포넌트 동작**을 검증한다.

**규칙:**
- 모든 테스트 함수는 이 문서의 항목에 추적 가능해야 한다.
- 모든 항목은 대응하는 테스트 함수 구현이 있어야 한다.
- CI 실행 가능 여부는 각 섹션에 명시한다.
- 실제 커널 모듈, 실제 ZFS, 실제 NVMe-oF 장치를 요구하는 테스트는
  별도로 표시하고 현실적인 인프라 요구사항을 함께 기술한다.

**총 테스트 케이스: 87** (인프로세스 84개 + 클러스터 레벨 3개)

---

## 테스트 환경 분류

pillar-csi E2E 테스트는 실행 환경에 따라 두 가지로 분류된다.

### 유형 A: 인프로세스(In-Process) E2E 테스트 ✅ CI 실행 가능

빌드 태그 없음. `go test ./test/e2e/ -v` 로 실행.

실제 Kubernetes 클러스터 없이, 실제 커널 모듈 없이 실행된다. 아래 테스트
더블(test double)을 활용한다:

```
┌─────────────────────────────────────────────────────────────────┐
│  CSI ControllerServer / NodeServer (내부 컴포넌트, 실제 코드)      │
│                         │                                       │
│              ┌──────────┼──────────────┐                        │
│              ▼          ▼              ▼                        │
│   mockAgentServer   mockCSIConnector  mockCSIMounter             │
│   (실제 gRPC 리스너,  (NVMe-oF 스텁,   (인메모리 마운트            │
│    localhost:0,     커널 모듈 불필요)   테이블, mount(8) 불필요)   │
│    모의 ZFS backend)                                             │
│              │                                                  │
│      fake k8s client                                            │
│      (controller-runtime 페이크,                                 │
│       실제 kube-apiserver 불필요)                                 │
│              │                                                  │
│      t.TempDir() as configfs root                               │
│      (실제 /sys/kernel/config 불필요)                             │
└─────────────────────────────────────────────────────────────────┘
```

**최소 요구사항:** Go 빌드 도구체인, Linux/macOS (tmpfs 지원)

---

### 유형 B: 클러스터 레벨(Cluster-Level) E2E 테스트 ❌ 표준 CI 불가

빌드 태그 `//go:build e2e`. `go test ./test/e2e/ -tags=e2e -v` 로 실행.

실제 Kubernetes 클러스터(Kind), 실제 Docker, cert-manager가 필요하다.

**필수 인프라 요구사항:**
| 구성 요소 | 버전 | 비고 |
|-----------|------|------|
| Kind | v0.23+ | 로컬 클러스터 생성 |
| Docker / Podman | 24+ | 컨테이너 이미지 빌드 및 Kind 로드 |
| kubectl | v1.29+ | 클러스터 리소스 조작 |
| cert-manager | v1.14+ | TLS 인증서 관리 |
| pillar-csi 이미지 | `example.com/pillar-csi:v0.0.1` | `make docker-build` 로 빌드 |

**현실적 한계:** 표준 GitHub Actions, GitLab CI 등 컨테이너 기반 CI에서는
Docker-in-Docker(DinD) 또는 Kind 지원 러너가 없으면 실행 불가.
전용 self-hosted 러너(베어메탈 또는 네스티드 가상화 지원 VM)가 필요하다.

---

## 테스트 더블 피델리티 노트

### mockAgentServer (CSI Controller/Lifecycle 테스트에서 사용)

```go
// mockAgentServer는 AgentServiceServer를 함수 필드 + 콜 카운터로 구현한다.
// 실제 pillar-agent 대비 단순화:
//   - 실제 ZFS 명령 없음; 사전 설정된 응답 필드 사용.
//   - CreateVolume/ExportVolume 등 각 RPC의 호출 기록을 슬라이스로 보존.
//   - 오류 시나리오는 테스트별 에러 필드로 주입.
//   - 실제 configfs 조작 없음; ExportInfo는 하드코딩된 테스트 값 사용.
//   - 실제 gRPC 리스너(localhost:0)를 사용하므로 네트워크 직렬화/역직렬화는 실제와 동일.
```

### mockCSIConnector (CSI Node 테스트에서 사용)

```go
// mockCSIConnector는 NVMe-oF connector.Connector 인터페이스를 구현한다.
// 실제 NVMe-oF 커넥터 대비 단순화:
//   - 실제 nvme connect 명령 없음.
//   - DevicePath는 테스트가 제어하는 문자열 필드.
//   - Connect/Disconnect 호출 횟수를 카운터로 기록.
//   - 커널 모듈(nvme-tcp) 불필요.
```

### mockCSIMounter (CSI Node 테스트에서 사용)

```go
// mockCSIMounter는 mounter.Mounter 인터페이스를 구현한다.
// 실제 마운터 대비 단순화:
//   - 실제 mount(8)/umount(8) 시스템 콜 없음.
//   - 인메모리 마운트 테이블로 상태 추적.
//   - mkfs/mkfs.ext4 등 포맷 명령 없음.
//   - root 권한 불필요.
```

### agentE2EMockBackend (Agent gRPC E2E 테스트에서 사용)

```go
// agentE2EMockBackend는 backend.VolumeBackend 인터페이스를 구현한다.
// 실제 ZFS backend 대비 단순화:
//   - 실제 zfs(8) 명령 없음.
//   - 설정 가능한 오류 필드로 각 메서드의 실패를 시뮬레이션.
//   - 실제 gRPC 리스너를 통해 agent.Server에 연결되므로
//     직렬화 레이어는 실제와 동일하게 테스트됨.
```

### fake controller-runtime client (CSI Controller 테스트에서 사용)

```go
// controller-runtime의 fake.NewClientBuilder()로 생성.
// 실제 kube-apiserver 대비 단순화:
//   - 인메모리 객체 저장소; 실제 etcd 없음.
//   - 낙관적 잠금(ResourceVersion) 부분 지원.
//   - Watch/List 인포머 없음; 필요 시 직접 Get 사용.
//   - 실제 CRD 검증 웹훅 미실행.
```

---

## E1: 볼륨 라이프사이클 — CreateVolume / DeleteVolume

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**아키텍처:**
```
CSI ControllerServer → (실제 gRPC, localhost:0) → mockAgentServer
                    ↓
             fake k8s client (PillarTarget 사전 등록)
```

**서브시스템 경계 설정:** `newCSIControllerE2EEnv(t, "storage-1")` 호출로
구성. PillarTarget CRD를 fake 클라이언트에 사전 등록하고, mockAgentServer를
실제 gRPC 리스너에 바인딩한다.

---

### E1.1 CreateVolume — 정상 경로

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 1 | `TestCSIController_CreateVolume` | CreateVolume이 agent.CreateVolume → agent.ExportVolume을 순서대로 호출하고 올바른 VolumeId/VolumeContext를 반환 | PillarTarget="storage-1"; zfs-pool="tank"; 프로토콜=nvmeof-tcp; 용량=1GiB | VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-create-test"; VolumeContext에 target_id/address/port/volume-ref/protocol-type 포함 |
| 2 | `TestCSIController_CreateVolume_Idempotency` | 동일한 볼륨 이름으로 CreateVolume을 두 번 호출하면 두 번째 호출은 agent.CreateVolume/ExportVolume을 재호출하지 않고 동일한 응답 반환 | 첫 번째 CreateVolume 성공 후 동일 파라미터로 재호출 | 두 번째 호출 성공; 동일한 VolumeId 반환; agent CreateVolume은 1회, ExportVolume은 1회만 호출 |

---

### E1.2 CreateVolume — 오류 경로

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 3 | `TestCSIController_CreateVolume_MissingParams` | StorageClass 파라미터 누락 시 InvalidArgument 반환 | 필수 파라미터(target, backend-type, protocol-type, zfs-pool) 일부 또는 전부 제거 | gRPC InvalidArgument; agent 호출 없음 |
| 4 | `TestCSIController_CreateVolume_PillarTargetNotFound` | 참조된 PillarTarget이 존재하지 않으면 NotFound 반환 | Parameters["target"]="nonexistent"; fake 클라이언트에 해당 PillarTarget 미등록 | gRPC NotFound 또는 Internal; agent 호출 없음 |
| 5 | `TestCSIController_CreateVolume_AgentCreateError` | agent.CreateVolume 실패 시 오류 전파 | mockAgentServer.CreateVolumeErr 설정 | 비-OK gRPC 상태 반환; ExportVolume 미호출 |
| 6 | `TestCSIController_CreateVolume_AgentExportError` | agent.CreateVolume 성공 후 agent.ExportVolume 실패 시 오류 전파 | mockAgentServer.ExportVolumeErr 설정 | 비-OK gRPC 상태 반환; PillarVolume CRD에 PartialFailure 기록 |

---

### E1.3 DeleteVolume — 정상 경로

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 7 | `TestCSIController_DeleteVolume` | DeleteVolume이 agent.UnexportVolume → agent.DeleteVolume을 순서대로 호출 | 사전 CreateVolume으로 볼륨 생성; VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-delete-test" | 성공; UnexportVolume 1회, DeleteVolume 1회 호출 |
| 8 | `TestCSIController_DeleteVolume_Idempotency` | 이미 삭제된 볼륨을 다시 DeleteVolume해도 성공 (멱등성) | 볼륨 삭제 후 동일 VolumeId로 재호출 | 두 번째 호출도 성공; 오류 없음 |
| 9 | `TestCSIController_DeleteVolume_NotFoundIsIdempotent` | agent가 NotFound를 반환해도 DeleteVolume은 성공 처리 | mockAgentServer: UnexportVolumeErr = gRPC NotFound | DeleteVolume 성공; CSI 명세상 Not-Found는 이미 삭제된 것으로 처리 |

---

### E1.4 DeleteVolume — 오류 경로

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 10 | `TestCSIController_DeleteVolume_MalformedID` | 잘못된 형식의 VolumeId는 InvalidArgument 반환 | VolumeId="noslash" (슬래시 없음) | gRPC InvalidArgument; agent 호출 없음 |
| 11 | `TestCSIController_DeleteVolume_AgentError` | agent.UnexportVolume 또는 agent.DeleteVolume 실패 시 오류 전파 | mockAgentServer.DeleteVolumeErr 설정 | 비-OK gRPC 상태 반환 |

---

### E1.5 기본 프로비저닝 — 전체 왕복(Full Round Trip)

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 12 | `TestCSIController_FullRoundTrip` | CreateVolume → ControllerPublishVolume → ControllerUnpublishVolume → DeleteVolume 전체 CSI Controller 왕복 테스트 | 단일 mockAgentServer; fake k8s 클라이언트; 정상 경로 설정 | 모든 단계 성공; agent 호출 순서 검증; VolumeContext 키 검증 |
| 13 | `TestCSIController_VolumeIDFormatPreservation` | VolumeId 포맷("target/protocol/backend/pool/name")이 생성-게시-삭제 전 주기에서 보존됨 | CreateVolume 후 ControllerPublish/Unpublish/Delete 호출 | 각 단계에서 동일한 VolumeId 포맷 사용; 파싱 오류 없음 |

---

## E2: CSI Controller — ControllerPublish / ControllerUnpublish / ControllerExpandVolume

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

---

### E2.1 ControllerPublishVolume

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 14 | `TestCSIController_ControllerPublishVolume` | ControllerPublishVolume이 agent.AllowInitiator를 올바른 파라미터로 호출 | 유효한 VolumeId와 NodeId 제공; VolumeContext에 target_id/address/port 포함 | 성공; PublishContext 반환; AllowInitiator 1회 호출 |
| 15 | `TestCSIController_ControllerPublishVolume_Idempotency` | 동일 파라미터로 두 번 호출해도 각각 성공 | ControllerPublishVolume 동일 인수로 2회 호출 | 두 호출 모두 성공; PublishContext 동일; AllowInitiator 각 호출마다 1회씩 총 2회 |
| 16 | `TestCSIController_ControllerPublishVolume_MissingFields` | 필수 필드(VolumeId, NodeId, VolumeContext) 누락 시 InvalidArgument | 각 필드를 빈 값으로 설정 | gRPC InvalidArgument; agent 호출 없음 |

---

### E2.2 ControllerUnpublishVolume

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 17 | `TestCSIController_ControllerUnpublishVolume` | ControllerUnpublishVolume이 agent.DenyInitiator를 올바르게 호출 | Publish 후 Unpublish 호출 | 성공; DenyInitiator 1회 호출 |
| 18 | `TestCSIController_ControllerUnpublishVolume_NotFoundIsIdempotent` | agent가 NotFound를 반환해도 Unpublish는 성공 | mockAgentServer.DenyInitiatorErr = NotFound | 성공; CSI 명세상 Not-Found는 이미 접근이 제거된 것으로 처리 |

---

### E2.3 ControllerExpandVolume

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 19 | `TestCSIController_ControllerExpandVolume` | ControllerExpandVolume이 agent.ExpandVolume을 올바른 새 용량으로 호출 | VolumeId 유효; CapacityRange.RequiredBytes=2GiB | 성공; CapacityBytes=2GiB 반환; agent.ExpandVolume 1회 호출 |
| 20 | `TestCSIController_ControllerExpandVolume_MissingCapacityRange` | CapacityRange 없으면 InvalidArgument | CapacityRange=nil | gRPC InvalidArgument |

---

### E2.4 ValidateVolumeCapabilities

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 21 | `TestCSIController_ValidateVolumeCapabilities` | 지원 가능한 접근 모드(SINGLE_NODE_WRITER 등)는 확인됨, 지원 불가 모드(MULTI_NODE_MULTI_WRITER 등)는 거부됨 | 다양한 AccessMode 조합 테스트 | 지원 모드: 빈 메시지 반환; 비지원 모드: 메시지 필드에 이유 포함 |

---

## E3: CSI Node — NodeStage / NodePublish / NodeUnpublish / NodeUnstage

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**아키텍처:**
```
CSI NodeServer → mockCSIConnector (NVMe-oF 스텁)
             → mockCSIMounter   (인메모리 마운트 테이블)
             → t.TempDir()      (스테이징 상태 디렉터리)
```

---

### E3.1 전체 왕복 — 마운트 접근 모드

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 22 | `TestCSINode_FullRoundTrip_MountAccess` | NodeStageVolume → NodePublishVolume → NodeUnpublishVolume → NodeUnstageVolume 전체 마운트 접근 라이프사이클 | VolumeContext: NQN/address/port; 접근 모드 MOUNT; mockConnector.DevicePath="/dev/nvme0n1" | 모든 단계 성공; Connector.Connect 1회, FormatAndMount 1회, 바인드 마운트 1회, 언마운트 2회, Disconnect 1회 |

---

### E3.2 전체 왕복 — 블록 접근 모드

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 23 | `TestCSINode_FullRoundTrip_BlockAccess` | 블록 디바이스 접근 모드 전체 라이프사이클 | 접근 모드 BLOCK; 나머지 동일 | 성공; 포맷/파일시스템 마운트 없이 디바이스 직접 노출 |

---

### E3.3 디바이스 디스커버리

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 24 | `TestCSINode_DeviceDiscovery` | NodeStage 시 NVMe-oF 연결 후 디바이스 경로 탐색 | mockConnector: 여러 폴링 시도 후 디바이스 경로 반환 | 성공; 디바이스 경로가 스테이징 상태에 저장됨 |

---

### E3.4 멱등성 (Idempotency)

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 25 | `TestCSINode_IdempotentStage` | NodeStageVolume 2회 호출: 두 번째는 no-op | NodeStage 성공 후 동일 파라미터로 재호출 | 두 번째 호출 성공; Connector.Connect 재호출 없음 |
| 26 | `TestCSINode_IdempotentPublish` | NodePublishVolume 2회 호출: 두 번째는 no-op | NodePublish 성공 후 동일 파라미터로 재호출 | 두 번째 호출 성공; 중복 마운트 없음 |
| 27 | `TestCSINode_IdempotentUnstage` | NodeUnstageVolume 2회 호출: 두 번째는 no-op | NodeUnstage 성공 후 동일 파라미터로 재호출 | 두 번째 호출 성공; 이중 언마운트/연결 해제 없음 |
| 28 | `TestCSINode_IdempotentUnpublish` | NodeUnpublishVolume 2회 호출: 두 번째는 no-op | NodeUnpublish 성공 후 동일 파라미터로 재호출 | 두 번째 호출 성공; 오류 없음 |

---

### E3.5 읽기 전용 마운트

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 29 | `TestCSINode_ReadonlyPublish` | NodePublishVolume에서 readonly=true 플래그가 마운터에 전달됨 | Readonly=true; 접근 모드 MOUNT | 성공; mockMounter가 readonly 옵션으로 호출됨 |

---

### E3.6 상태 파일 영속성

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 30 | `TestCSINode_StateFilePersistence` | NodeStage 후 스테이징 상태 파일이 StateDir에 저장되고, 이후 NodeUnstage 시 제거됨 | NodeStage → 상태 파일 존재 확인 → NodeUnstage → 파일 제거 확인 | 상태 파일 생성/삭제 타이밍이 CSI 호출과 일치 |

---

### E3.7 노드 정보 및 역량

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 31 | `TestCSINode_NodeGetInfo` | NodeGetInfo가 올바른 NodeId와 토폴로지 키 반환 | NodeServer 초기화 시 nodeID="worker-1" | NodeId="worker-1"; 토폴로지 키 존재 |
| 32 | `TestCSINode_NodeGetCapabilities` | NodeGetCapabilities가 지원 역량 목록 반환 | 기본 설정 | STAGE_UNSTAGE_VOLUME 포함; 비어있지 않은 역량 목록 |

---

### E3.8 유효성 검사 오류

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 33 | `TestCSINode_ValidationErrors` | 필수 파라미터 누락 시 InvalidArgument 반환 | VolumeId 빈값, VolumeContext 키 누락, StagingTargetPath 빈값 등 | 각 케이스에서 gRPC InvalidArgument; 커넥터/마운터 호출 없음 |

---

### E3.9 오류 경로

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 34 | `TestCSINode_ConnectError` | NVMe-oF 연결 실패 시 NodeStage 실패 | mockConnector.ConnectErr 설정 | 비-OK gRPC 상태; 마운트 미실행 |
| 35 | `TestCSINode_DisconnectError` | NVMe-oF 연결 해제 실패 시 NodeUnstage 실패 | mockConnector.DisconnectErr 설정 | 비-OK gRPC 상태 |
| 36 | `TestCSINode_MountError` | 파일시스템 마운트 실패 시 NodeStage 실패 | mockMounter.FormatAndMountErr 설정 | 비-OK gRPC 상태; Disconnect 정리 동작 |
| 37 | `TestCSINode_PublishMountError` | 바인드 마운트 실패 시 NodePublish 실패 | mockMounter.BindMountErr 설정 | 비-OK gRPC 상태 |

---

### E3.10 다중 볼륨 동시 처리

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 38 | `TestCSINode_MultipleVolumes` | 여러 볼륨이 독립적인 스테이징 상태를 유지 | 3개 볼륨을 순차적으로 Stage; 각각 독립 StagingTargetPath 사용 | 모든 볼륨 성공적으로 스테이지; 상태 파일 간 충돌 없음 |

---

### E3.11 NodeStageVolume — 파일시스템 타입별 동작

**설명:** `NodeStageVolume`은 `VolumeCapability.MountVolume.FsType` 필드를
`FormatAndMount`에 그대로 전달해야 한다. 다양한 파일시스템 타입에서 동일한
흐름이 동작하는지 확인한다.

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 72 | `TestCSINode_StageVolume_XFS` | `fsType=xfs`로 NodeStageVolume 호출 시 FormatAndMount에 "xfs" 전달됨 | mountVolumeCapability("xfs", SINGLE_NODE_WRITER); mockConnector.DevicePath="/dev/nvme3n1" | FormatAndMount 1회; FsType="xfs"; 성공 |
| 73 | `TestCSINode_StageVolume_DefaultFilesystem` | `fsType=""` (빈 문자열) 시 기본 파일시스템(ext4)으로 포맷 | mountVolumeCapability("", SINGLE_NODE_WRITER) | FormatAndMount 1회; FsType="" 또는 "ext4"; 성공 |
| 74 | `TestCSINode_StageVolume_BlockAccessNoFormatAndMount` | 블록 접근 모드에서 FormatAndMount가 호출되지 않음 (블록 디바이스는 파일시스템 포맷 불필요) | blockVolumeCapability(SINGLE_NODE_WRITER) | FormatAndMount 0회; Mount 1회 (bind); 성공 |

---

### E3.12 NodeStageVolume — NVMe-oF 어태치(Attach) 파라미터 상세 검증

**설명:** CSI Controller가 생성한 VolumeContext(NQN, address, port)가
NodeStageVolume에서 NVMe-oF Connect 호출에 정확하게 전달되는지 검증한다.
이 섹션은 **어태치(NVMe-oF connect) 경계**를 집중적으로 테스트한다.

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 75 | `TestCSINode_StageVolume_ConnectParamsForwarded` | VolumeContext의 NQN/address/port가 Connector.Connect 호출 시 정확히 전달됨 | VolumeContext: target_id="nqn.test", address="192.168.0.10", port="4420" | Connector.Connect: SubsysNQN="nqn.test", TrAddr="192.168.0.10", TrSvcID="4420" |
| 76 | `TestCSINode_StageVolume_CustomPort` | 비표준 포트(4421)도 정확히 전달됨 | VolumeContext.port="4421" | Connector.Connect: TrSvcID="4421" |
| 77 | `TestCSINode_StageVolume_MissingAddress` | VolumeContext에서 address 키 누락 시 InvalidArgument | VolumeContext에 target_id와 port만 있고 address 없음 | gRPC InvalidArgument; Connect 미호출 |
| 78 | `TestCSINode_StageVolume_MissingPort` | VolumeContext에서 port 키 누락 시 InvalidArgument | VolumeContext에 target_id와 address만 있고 port 없음 | gRPC InvalidArgument; Connect 미호출 |
| 79 | `TestCSINode_StageVolume_AttachThenStateSaved` | NVMe-oF 연결 성공 후 상태 파일에 NQN이 저장됨 (재시작 복구 지원) | 정상 NodeStageVolume 호출 | 상태 파일 생성; 상태 파일 내용에 NQN 포함; Connector.Connect 1회 |

---

### E3.13 NodeUnstageVolume — 디태치(Detach) 시나리오 상세

**설명:** NodeUnstageVolume은 NVMe-oF 연결을 해제(디태치)하고 상태 파일을
제거해야 한다. 다양한 비정상 상황에서도 올바르게 동작해야 한다.

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 80 | `TestCSINode_UnstageVolume_DetachCallsDisconnect` | NodeUnstageVolume이 Connector.Disconnect를 정확한 NQN으로 호출 | NodeStageVolume 성공 후 NodeUnstageVolume 호출 | Disconnect 1회; 인수 NQN이 Stage 시 사용한 NQN과 동일 |
| 81 | `TestCSINode_UnstageVolume_NeverStagedIsIdempotent` | 스테이지된 적 없는 볼륨에 NodeUnstageVolume 호출 시 성공 (멱등성) | 사전 NodeStageVolume 없이 NodeUnstageVolume 직접 호출 | 성공; Disconnect 0회; Unmount 0회 |
| 82 | `TestCSINode_UnstageVolume_DetachFailsOnDisconnectError` | Connector.Disconnect 실패 시 gRPC Internal 반환 | NodeStage 성공 후 DisconnectErr 주입 | gRPC Internal; 상태 파일 미제거 (정리 실패 명시) |
| 83 | `TestCSINode_UnstageVolume_StateFileRemovedAfterSuccessfulDetach` | 정상 디태치 후 상태 파일 제거 확인 | NodeStage → NodeUnstage 순서 | NodeUnstage 성공 후 StateDir에 *.json 파일 0개 |

---

### E3.14 NodePublishVolume — 다중 타깃 마운트

**설명:** 하나의 스테이지된 볼륨(하나의 NVMe-oF 연결)에서 여러 컨테이너 타깃
경로로 바인드 마운트를 생성하는 시나리오를 검증한다.

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 84 | `TestCSINode_PublishVolume_MultipleTargets` | 동일 스테이징 경로에서 두 타깃 경로로 NodePublishVolume 각각 성공 | NodeStage 1회; NodePublish×2 (서로 다른 targetPath) | 두 Mount 호출 모두 성공; source는 동일 stagingPath; target은 각각 다름 |
| 85 | `TestCSINode_PublishVolume_UnpublishOneKeepsOther` | 두 타깃 중 하나 NodeUnpublish 시 나머지 마운트는 유지됨 | NodePublish×2 후 NodeUnpublish×1 | Unmount 1회; 남은 타깃 경로는 여전히 마운트 상태; 스테이징 경로도 마운트 유지 |

---

### E3.15 NodePublishVolume — 접근 모드(Access Mode)별 동작

**설명:** CSI 명세상 접근 모드(AccessMode)에 따라 마운트 옵션이 달라져야 한다.
SINGLE_NODE_READER_ONLY와 MULTI_NODE_READER_ONLY는 읽기 전용 마운트여야 한다.

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 86 | `TestCSINode_PublishVolume_SingleNodeWriter` | SINGLE_NODE_WRITER 접근 모드에서 쓰기 가능 마운트 | AccessMode=SINGLE_NODE_WRITER; Readonly=false | 성공; 마운트 옵션에 "ro" 없음 |
| 87 | `TestCSINode_PublishVolume_SingleNodeReaderOnly` | SINGLE_NODE_READER_ONLY 접근 모드에서 읽기 전용 마운트 | AccessMode=SINGLE_NODE_READER_ONLY; Readonly=true | 성공; 마운트 옵션에 "ro" 포함 |

---

## E4: 교차-컴포넌트 CSI 라이프사이클

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**아키텍처:**
```
CSI ControllerServer ──── gRPC (localhost:0) ────► mockAgentServer
         │
    VolumeContext 전달
    (NQN, address, port)
         │
         ▼
CSI NodeServer ────► mockCSIConnector
               ────► mockCSIMounter
```

---

### E4.1 전체 라이프사이클

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 39 | `TestCSILifecycle_FullCycle` | Controller→Node 전체 경로: CreateVolume → ControllerPublish → NodeStage → NodePublish → NodeUnpublish → NodeUnstage → ControllerUnpublish → DeleteVolume | 단일 mockAgentServer; 공유 VolumeContext 전달 | 모든 단계 성공; agent 호출 순서 검증; VolumeContext 키가 NodeStage에 올바르게 전달됨 |

---

### E4.2 순서 제약 (Ordering Constraints)

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 40 | `TestCSILifecycle_OrderingConstraints` | ControllerPublish 전 NodeStage 호출 → FailedPrecondition | 공유 VolumeStateMachine; NodeStage를 ControllerPublish 없이 호출 | gRPC FailedPrecondition |

---

### E4.3 멱등성

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 41 | `TestCSILifecycle_IdempotentSteps` | 라이프사이클 각 단계를 두 번씩 호출해도 최종 상태 동일 | 전체 라이프사이클 후 각 단계 재호출 | 모든 재호출 성공; 중복 agent 호출 없음 |

---

### E4.4 VolumeContext 흐름

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 42 | `TestCSILifecycle_VolumeContextFlowThrough` | CreateVolume이 설정한 VolumeContext(NQN/address/port)가 NodeStageVolume에 키 변환 없이 그대로 전달됨 | mockAgentServer.ExportVolumeInfo에 특정 NQN/address/port 설정 | NodeStage 시 mockConnector가 동일한 NQN/address/port로 호출됨 |

---

## E5: 순서 제약 (Ordering Constraints)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

공유 VolumeStateMachine을 통해 컨트롤러와 노드 서버 간 순서 제약을 검증한다.

---

### E5.1 역순 호출 거부

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 43 | `TestCSIOrdering_NodeStageBeforeControllerPublish` | ControllerPublish 없이 NodeStage 호출 → FailedPrecondition | 공유 SM; ControllerPublish 미호출 상태에서 NodeStage | gRPC FailedPrecondition |
| 44 | `TestCSIOrdering_NodePublishBeforeNodeStage` | NodeStage 없이 NodePublish 호출 → FailedPrecondition | 공유 SM; NodeStage 미완료 상태에서 NodePublish | gRPC FailedPrecondition |
| 45 | `TestCSIOrdering_NodeUnstageBeforeNodeUnpublish` | NodeUnpublish 없이 NodeUnstage 호출 → FailedPrecondition | 공유 SM; NodePublish 후 NodeUnstage 직접 호출 | gRPC FailedPrecondition |
| 46 | `TestCSIOrdering_NodePublishAfterUnstage` | NodeUnstage 후 NodePublish 재시도 → FailedPrecondition | 전체 정상 라이프사이클 완료 후 NodePublish 재호출 | gRPC FailedPrecondition |

---

### E5.2 정상 순서 통과

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 47 | `TestCSIOrdering_FullLifecycleWithSM` | 공유 SM을 사용한 전체 순서 라이프사이클 성공 | 모든 단계를 올바른 순서로 호출 | 모든 단계 성공; SM 상태 전이 검증 |
| 48 | `TestCSIOrdering_IdempotencyWithSM` | 올바른 상태에서의 재호출은 순서 제약 위반 아님 | 각 단계를 현재 상태에서 허용되는 재호출 | 성공; FailedPrecondition 미발생 |

---

## E6: 부분 실패 영속성 (Partial Failure Persistence)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

PillarVolume CRD를 통한 부분 실패 상태 추적을 검증한다.

---

### E6.1 부분 실패 CRD 생성

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 49 | `TestCSIController_PartialFailure_CRDCreatedOnExportFailure` | agent.CreateVolume 성공 + agent.ExportVolume 실패 시 PillarVolume CRD가 Phase=CreatePartial, PartialFailure.BackendCreated=true로 생성됨 | mockAgentServer: CreateVolume 성공, ExportVolume 실패 | CreateVolume gRPC 실패; PillarVolume CRD 존재; Phase=CreatePartial; BackendCreated=true |
| 50 | `TestCSIController_PartialFailure_RetryAdvancesToReady` | 부분 실패 후 재시도(ExportVolume 이번엔 성공) 시 CRD가 Phase=Ready로 전환되고 ExportInfo 채워짐 | 위 케이스 후 ExportVolume 성공 설정; CreateVolume 재호출 | 성공; CRD Phase=Ready; ExportInfo 채워짐; PartialFailure 초기화 |
| 51 | `TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry` | 재시도 시 controller의 skipBackend 최적화가 동작하여 agent.CreateVolume 재호출 없음 | 위와 동일 | agent.CreateVolume 총 1회만 호출 (재시도 포함) |

---

### E6.2 삭제 시 CRD 정리

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 52 | `TestCSIController_DeleteVolume_CleansUpCRD` | 성공적인 DeleteVolume이 PillarVolume CRD를 삭제 | 볼륨 생성 후 삭제 | PillarVolume CRD가 클러스터에서 제거됨 |
| 53 | `TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates` | 부분 생성 상태의 볼륨도 DeleteVolume으로 올바르게 정리됨 | Phase=CreatePartial인 PillarVolume CRD; DeleteVolume 호출 | 성공; CRD 제거; agent.DeleteVolume 호출 여부는 BackendCreated 플래그에 따라 결정 |

---

## E7: 게시 멱등성 (Publish Idempotency)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

---

### E7.1 ControllerPublishVolume 멱등성

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 54 | `TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs` | 동일 인수로 ControllerPublishVolume 2회 호출: 두 호출 모두 성공, 응답 동일, agent.AllowInitiator는 각 호출당 1회씩 총 2회 | 동일 VolumeId/NodeId로 2회 호출 | 두 호출 모두 성공; PublishContext 동일; CreateVolume/ExportVolume 미트리거 |
| 55 | `TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes` | 서로 다른 노드에 대한 ControllerPublishVolume은 각각 독립적으로 성공 | 동일 VolumeId, 다른 NodeId로 2회 호출 | 두 호출 모두 성공; AllowInitiator는 서로 다른 호스트 NQN으로 각 1회씩 |

---

### E7.2 NodePublishVolume 멱등성

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 56 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget` | 동일 타깃 경로로 NodePublishVolume 2회 호출: 두 번째는 no-op | NodeStage 후 NodePublish 동일 인수로 2회 | 두 호출 모두 성공; 응답 동일; 중복 마운트 없음 |
| 57 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleBlockAccess` | 블록 접근 모드에서도 NodePublishVolume 2회 호출 멱등성 보장 | BLOCK 접근 모드; NodePublish 동일 인수로 2회 | 두 호출 모두 성공; 중복 블록 디바이스 노출 없음 |
| 58 | `TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble` | 읽기 전용 NodePublishVolume 2회 호출 멱등성 | Readonly=true; 2회 호출 | 두 호출 모두 성공; 응답 동일 |

---

## E8: mTLS 컨트롤러 통합 테스트

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

PillarTarget 컨트롤러 ↔ pillar-agent mTLS 신뢰 경계를 실제 gRPC 리스너와
인메모리 인증서로 검증한다. 실제 Kubernetes 클러스터 불필요.

---

### E8.1 mTLS 인증

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 59 | `TestMTLSController_AgentConnectedAuthenticated` | 올바른 mTLS 자격증명으로 연결 시 PillarTarget 상태 AgentConnected=True/Authenticated | testcerts.New()로 동일 CA의 서버/클라이언트 인증서 생성; mTLS 서버 + 컨트롤러 설정 | AgentConnected 조건 True; Reason=Authenticated |
| 60 | `TestMTLSController_PlaintextDialRejected` | 평문 클라이언트가 mTLS 서버에 거부됨 | 서버: mTLS 설정; 클라이언트: insecure.NewCredentials() | AgentConnected 조건 False; Reason=HealthCheckFailed 또는 TLSHandshakeFailed |
| 61 | `TestMTLSController_WrongCAClientRejected` | 다른 CA가 서명한 클라이언트 인증서는 거부됨 | 서버: CA1; 클라이언트: CA2 서명 인증서 | AgentConnected 조건 False |

---

## E9: Agent gRPC E2E 테스트

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

실제 gRPC 리스너(localhost:0)와 mock ZFS backend로 agent.Server의
네트워크 직렬화/역직렬화 레이어까지 포함하여 검증한다.
실제 ZFS 커널 모듈 불필요.

---

### E9.1 역량 및 헬스체크

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 62 | `TestAgent_GetCapabilities` | 실제 gRPC 연결을 통한 GetCapabilities 호출이 올바른 역량 목록 반환 | agentE2EMockBackend; 실제 gRPC 리스너 | ZFS_ZVOL backend + NVMEOF_TCP 프로토콜 포함 |
| 63 | `TestAgent_HealthCheck` | 실제 gRPC 연결을 통한 HealthCheck 호출 | sysModuleZFSPath를 tmpdir의 존재하는 파일로 설정 | ZFS 모듈 체크 HEALTHY; configfs 체크 결과 포함 |

---

### E9.2 전체 왕복

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 64 | `TestAgent_RoundTrip` | CreateVolume → ExportVolume → AllowInitiator → DenyInitiator → UnexportVolume → DeleteVolume 전체 왕복을 실제 gRPC를 통해 검증 | mock backend + tmpdir configfs + 실제 gRPC 리스너 | 모든 단계 성공; configfs 상태 변화 검증 |

---

### E9.3 재조정 복구

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 65 | `TestAgent_ReconcileStateRestoresExports` | ReconcileState가 재시작 후 configfs 엔트리를 올바르게 복원 | 볼륨 목록을 포함한 ReconcileState 호출; 빈 tmpdir configfs | ReconcileState 후 모든 볼륨의 configfs 서브시스템 디렉터리 존재 |

---

### E9.4 오류 처리

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 66 | `TestAgent_ErrorHandling` | 다양한 오류 시나리오(잘못된 pool ID, backend 오류 등)가 적절한 gRPC 상태 코드로 매핑 | 각 오류 조건에서 해당 RPC 호출 | NotFound/InvalidArgument/Internal 등 명세에 맞는 gRPC 코드 반환 |

---

### E9.5 Phase 1 전체 RPC

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 67 | `TestAgent_AllPhase1RPCs` | Phase 1에서 지원하는 모든 RPC를 한 테스트에서 순차적으로 검증 | mock backend + tmpdir configfs + 실제 gRPC 리스너 | 모든 Phase 1 RPC 성공; 오류 없음 |

---

## E10: 클러스터 레벨 E2E 테스트

**테스트 유형:** B (클러스터 레벨) ❌ 표준 CI 불가

**빌드 태그:** `//go:build e2e`

**실행 방법:**
```bash
# Kind 클러스터 준비 후
go test ./test/e2e/ -tags=e2e -v -run TestE2E
```

**필수 인프라:** [유형 B 섹션 참조](#유형-b-클러스터-레벨cluster-level-e2e-테스트--표준-ci-불가)

---

### E10.1 매니저 배포 검증

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 68 | `TestE2E/Manager_컨트롤러_파드_실행_확인` | pillar-csi-controller-manager 파드가 `pillar-csi-system` 네임스페이스에서 정상 실행됨 | Kind 클러스터; `make docker-build` 후 Kind에 이미지 로드; CRD 및 매니저 배포 | 컨트롤러 파드가 Running 상태; 재시작 없음 |
| 69 | `TestE2E/매니저_메트릭스_서비스_접근_가능` | RBAC RoleBinding 생성 후 `/metrics` 엔드포인트에서 메트릭 수집 가능 | 메트릭 RoleBinding 생성; curl로 메트릭 엔드포인트 접근 | HTTP 200 응답; Go 런타임 메트릭 포함 |
| 70 | `TestE2E/cert-manager_통합` | cert-manager가 설치된 환경에서 TLS 인증서 발급 동작 | cert-manager CRD 설치 확인; 클러스터 배포 | 인증서 발급 성공 |

---

## 향후 추가 예정 테스트 (실제 하드웨어 필요)

아래 테스트는 현재 구현되지 않았으며, 실제 ZFS, 실제 NVMe-oF 하드웨어,
또는 실제 Kubernetes 스토리지 노드가 필요하다. 표준 CI 환경에서 실행 불가능하다.

| # | 테스트 이름 (미구현) | 필요 인프라 | 설명 |
|---|---------------------|------------|------|
| F1 | `TestRealZFS_CreateVolume` | ZFS 커널 모듈, `zfs-utils` | 실제 ZFS pool에서 zvol 생성/삭제 |
| F2 | `TestRealNVMeoF_Export` | `/sys/kernel/config/nvmet/` (nvmet 커널 모듈) | 실제 NVMe-oF configfs 조작 |
| F3 | `TestRealNVMeoF_Connect` | NVMe-oF TCP 대상 서버 + 클라이언트, `nvme-tcp` 커널 모듈 | 실제 NVMe-oF TCP 연결 및 블록 디바이스 탑재 |
| F4 | `TestKubernetes_StorageClass_PVC` | 실제 Kubernetes 클러스터, pillar-agent DaemonSet, 스토리지 노드 | StorageClass → PVC → Pod 전체 Kubernetes 프로비저닝 흐름 |
| F5 | `TestKubernetes_VolumeExpansion` | 위와 동일 | 실행 중인 Pod의 볼륨 온라인 확장 |
| F6 | `TestKubernetes_NodeFailover` | 다중 노드 클러스터 | 스토리지 노드 재시작 후 agent ReconcileState 자동 복구 |
| F7 | `TestRealMTLS_CertRotation` | cert-manager, 실제 TLS 인증서 갱신 주기 | mTLS 인증서 자동 갱신 후 연결 유지 |
| F8 | `TestRealNode_NodeStageVolume_ActualMount` | 실제 NVMe-oF 디바이스, 루트 권한, `nvme-tcp` 커널 모듈, `mkfs.ext4` | 실제 NodeStageVolume: NVMe-oF connect → /dev/nvme* 블록 디바이스 → ext4 포맷 → 스테이징 경로 마운트 |
| F9 | `TestRealNode_NodePublishVolume_BindMount` | 위와 동일, 추가로 컨테이너 네임스페이스 | 실제 NodePublishVolume: 스테이징 → 컨테이너 타깃 경로 바인드 마운트 |
| F10 | `TestRealNode_NodeUnstageVolume_ActualDetach` | 위와 동일 | 실제 NodeUnstageVolume: 마운트 해제 → nvme disconnect → /dev/nvme* 디바이스 노드 제거 확인 |
| F11 | `TestRealNode_NodeStageVolume_DeviceAppearDelay` | 실제 NVMe-oF 대상, udev 지연 환경 | NVMe connect 후 /dev/nvme* 노드가 수 초 후에 나타나는 환경에서 폴링 로직 검증 |
| F12 | `TestRealNode_MultiPathAttach` | 다중 네트워크 인터페이스, multipath 설정 | 동일 NVMe-oF 대상에 두 경로 연결 후 NodeStageVolume에서 올바른 디바이스 선택 |

---

## 부록: 테스트 실행 참조

### 인프로세스 E2E 테스트 전체 실행

```bash
go test ./test/e2e/ -v -timeout 120s
```

### 특정 그룹만 실행

```bash
# E1: CreateVolume/DeleteVolume
go test ./test/e2e/ -v -run TestCSIController_CreateVolume
go test ./test/e2e/ -v -run TestCSIController_DeleteVolume

# E2: Controller 전체
go test ./test/e2e/ -v -run TestCSIController

# E3: Node 전체 (E3.1–E3.15 포함)
go test ./test/e2e/ -v -run TestCSINode

# E3.11: 파일시스템 타입별 동작
go test ./test/e2e/ -v -run TestCSINode_StageVolume

# E3.12: NVMe-oF Attach 파라미터 검증
go test ./test/e2e/ -v -run TestCSINode_StageVolume_Connect

# E3.13: NodeUnstage/Detach 시나리오
go test ./test/e2e/ -v -run TestCSINode_UnstageVolume

# E3.14–E3.15: NodePublish 다중 타깃 및 접근 모드
go test ./test/e2e/ -v -run TestCSINode_PublishVolume

# E4+E5: Lifecycle + Ordering
go test ./test/e2e/ -v -run TestCSILifecycle
go test ./test/e2e/ -v -run TestCSIOrdering

# E6: Partial Failure
go test ./test/e2e/ -v -run TestCSIController_PartialFailure
go test ./test/e2e/ -v -run TestCSIZvolNoDup

# E7: Publish Idempotency
go test ./test/e2e/ -v -run TestCSIPublishIdempotency

# E8: mTLS
go test ./test/e2e/ -v -run TestMTLS

# E9: Agent gRPC E2E
go test ./test/e2e/ -v -run TestAgent
```

### 클러스터 E2E 테스트 실행 (Kind 필요)

```bash
# Kind 클러스터 사전 준비 필요
make docker-build IMG=example.com/pillar-csi:v0.0.1
kind load docker-image example.com/pillar-csi:v0.0.1

go test ./test/e2e/ -tags=e2e -v -timeout 600s
```
