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

**총 테스트 케이스: 305** (인프로세스 194개 + envtest 통합 108개 + 클러스터 레벨 3개; E18 Agent 다운 시나리오 7개 · E21 잘못된 CR 시나리오 26개 · E22 비호환 프로토콜 시나리오 12개 · E3.16–E3.20 스테이징 단계 심화 18개 · E3.21–E3.23 게시 단계 단위 테스트 18개 · E26 교차-CRD 라이프사이클 상호작용 23개 포함 / 수동 AD 시나리오 3개 · BP 시나리오 4개 별도)

---

## 목차

### 문서 구조
- [테스트 케이스 필드 정의](#테스트-케이스-필드-정의)
- [테스트 환경 분류](#테스트-환경-분류)
- [CI 실행 가능 테스트 카탈로그](#ci-실행-가능-테스트-카탈로그)
- [테스트 더블 피델리티 노트](#테스트-더블-피델리티-노트)

### 카테고리 1 — 표준 CI 실행 가능 테스트 (유형 A: 인프로세스 E2E) ✅
> 빌드 태그 없음 | `go test ./test/e2e/ -v` | 총 170개 테스트 (E3.16–E3.20 스테이징 심화 18개 · E3.21–E3.23 게시 단계 단위 테스트 18개 포함)

- [E1: 볼륨 라이프사이클 — CreateVolume / DeleteVolume](#e1-볼륨-라이프사이클--createvolume--deletevolume)
  - [E1.6: 접근 모드 유효성 검증](#e16-접근-모드-유효성-검증-access-mode-validation)
  - [E1.7: 용량 범위 검증](#e17-용량-범위-검증-capacity-range-validation)
  - [E1.8: PillarTarget 상태 및 agent 연결 검증](#e18-pillartarget-상태-및-agent-연결-검증)
  - [E1.9: 부분 실패 복구](#e19-부분-실패-복구-partial-failure-recovery)
  - [E1.10: PVC 어노테이션 오버라이드](#e110-pvc-어노테이션-오버라이드-pvc-annotation-override)
  - [E1.11: VolumeId 형식 및 파라미터 검증 심화](#e111-volumeid-형식-및-파라미터-검증-심화)
- [E2: CSI Controller — ControllerPublish / ControllerUnpublish / ControllerExpandVolume](#e2-csi-controller--controllerpublish--controllerunpublish--controllerexpandvolume)
  - [E2.1: ControllerPublishVolume — 정상 경로](#e21-controllerpublishvolume--정상-경로)
  - [E2.2: ControllerUnpublishVolume — 정상 경로 및 오류](#e22-controllerunpublishvolume--정상-경로-및-오류)
  - [E2.5: 노드 친화성 (Node Affinity)](#e25-노드-친화성-node-affinity)
  - [E2.6: 오류 처리 — 게시 단계 (Error Handling)](#e26-오류-처리--게시-단계-error-handling)
- [E3: CSI Node — NodeStage / NodePublish / NodeUnpublish / NodeUnstage](#e3-csi-node--nodestage--nodepublish--nodeunpublish--nodeunstage)
  - [E3.16: NodeStageVolume — 디바이스 대기 및 GetDevicePath 유효성 검사](#e316-nodestagesvolume--디바이스-대기-및-getdevicepath-유효성-검사)
  - [E3.17: NodeStageVolume — 기본 파일시스템 타입 및 접근 유형 검증](#e317-nodestagesvolume--기본-파일시스템-타입-및-접근-유형-검증)
  - [E3.18: NodeStageVolume — 재부팅 후 재스테이징 멱등성](#e318-nodestagesvolume--재부팅-후-재스테이징-멱등성-re-stage-after-unmount)
  - [E3.19: NodeUnstageVolume — 오류 경로 및 예외 시나리오 심화](#e319-nodeunstagesvolume--오류-경로-및-예외-시나리오-심화)
  - [E3.20: 스테이징 상태 파일 관리](#e320-스테이징-상태-파일-관리-stage-state-file-management)
  - [E3.21: NodePublishVolume — 바인드 마운트, 읽기 전용, 멱등성 및 오류 처리 (단위 테스트)](#e321-nodepublishvolume--바인드-마운트-읽기-전용-멱등성-및-오류-처리-단위-테스트)
  - [E3.22: NodeUnpublishVolume — 언마운트, 멱등성 및 오류 처리 (단위 테스트)](#e322-nodeunpublishvolume--언마운트-멱등성-및-오류-처리-단위-테스트)
  - [E3.23: NodePublish/NodeUnpublish — 전체 노드 라이프사이클 (단위 테스트)](#e323-nodepublishnodeunpublish--전체-노드-라이프사이클-단위-테스트)
- [E4: 교차-컴포넌트 CSI 라이프사이클](#e4-교차-컴포넌트-csi-라이프사이클)
- [E5: 순서 제약 (Ordering Constraints)](#e5-순서-제약-ordering-constraints)
- [E6: 부분 실패 영속성 (Partial Failure Persistence)](#e6-부분-실패-영속성-partial-failure-persistence)
  - [E6.1: 부분 실패 CRD 생성](#e61-부분-실패-crd-생성)
  - [E6.2: 삭제 시 CRD 정리](#e62-삭제-시-crd-정리)
  - [E6.3: zvol 중복 방지 — skipBackend 최적화](#e63-zvol-중복-방지--skipbackend-최적화-no-duplication)
- [E7: 게시 멱등성 (Publish Idempotency)](#e7-게시-멱등성-publish-idempotency)
- [E8: mTLS 컨트롤러 통합 테스트](#e8-mtls-컨트롤러-통합-테스트)
- [E9: Agent gRPC E2E 테스트](#e9-agent-grpc-e2e-테스트)
- [E11: 볼륨 확장(Volume Expansion) 통합 E2E](#e11-볼륨-확장volume-expansion-통합-e2e)
- [E12: CSI 스냅샷 (현재 미구현)](#e12-csi-스냅샷-현재-미구현)
- [E13: 볼륨 클론 및 데이터 마이그레이션 (현재 부분 구현)](#e13-볼륨-클론-및-데이터-마이그레이션-현재-부분-구현)
- [E14: 잘못된 입력값 및 엣지 케이스 (Invalid Inputs & Edge Cases)](#e14-잘못된-입력값-및-엣지-케이스-invalid-inputs--edge-cases)
- [E15: 리소스 고갈 (Resource Exhaustion)](#e15-리소스-고갈-resource-exhaustion)
- [E16: 동시 작업 (Concurrent Operations)](#e16-동시-작업-concurrent-operations)
- [E17: 정리 검증 (Cleanup Validation)](#e17-정리-검증-cleanup-validation)
- [E18: Agent 다운 오류 시나리오 (Agent Down Error Scenarios)](#e18-agent-다운-오류-시나리오-agent-down-error-scenarios)
- [E21: 잘못된 CR 오류 시나리오 (Invalid CR Error Scenarios)](#e21-잘못된-cr-오류-시나리오-invalid-cr-error-scenarios) _(유형 A+C 혼합)_
- [E22: 비호환 백엔드-프로토콜 오류 시나리오 (Incompatible Backend-Protocol Error Scenarios)](#e22-비호환-백엔드-프로토콜-오류-시나리오-incompatible-backend-protocol-error-scenarios)
- [E24: 8단계 전체 라이프사이클 통합 시나리오 (Full Lifecycle Integration)](#e24-8단계-전체-라이프사이클-통합-시나리오-full-lifecycle-integration)
  - [E24.1: 정상 경로 — 8단계 완전 체인](#e241-정상-경로--8단계-완전-체인)
  - [E24.2: CreateVolume 단계 실패/복구](#e242-createvolume-단계-실패복구)
  - [E24.3: ControllerPublish 단계 실패/복구](#e243-controllerpublish-단계-실패복구)
  - [E24.4: NodeStage 단계 실패/복구](#e244-nodestage-단계-실패복구)
  - [E24.5: NodePublish 단계 실패/복구](#e245-nodepublish-단계-실패복구)
  - [E24.6: NodeUnpublish 단계 실패/복구](#e246-nodeunpublish-단계-실패복구)
  - [E24.7: NodeUnstage 단계 실패/복구](#e247-nodeunstage-단계-실패복구)
  - [E24.8: ControllerUnpublish 단계 실패/복구](#e248-controllerunpublish-단계-실패복구)
  - [E24.9: DeleteVolume 단계 실패/복구](#e249-deletevolume-단계-실패복구)
  - [E24.10: 중단된 라이프사이클 정리 경로](#e2410-중단된-라이프사이클-정리-경로)
  - [E21.1: 컨트롤러 런타임 잘못된 CR 처리 (Type A — in-process)](#e211-컨트롤러-런타임-잘못된-cr-처리-type-a--in-process-)
  - [E21.2: PillarTarget 웹훅 — 불변 필드 수정 거부 (Type C — envtest)](#e212-pillartarget-웹훅--불변-필드-수정-거부-type-c--envtest-)
  - [E21.3: PillarPool 웹훅 — 불변 필드 수정 거부 (Type C — envtest)](#e213-pillarpool-웹훅--불변-필드-수정-거부-type-c--envtest-)
  - [E21.4: CRD OpenAPI 스키마 검증 — 필드 범위/형식 위반 (Type C — envtest)](#e214-crd-openapi-스키마-검증--필드-범위형식-위반-type-c--envtest-)

### 카테고리 1.5 — Envtest 통합 테스트 (유형 C: envtest 필요) ⚠️
> 빌드 태그: `//go:build integration` | `make setup-envtest && go test -tags=integration ./internal/...` | envtest API 서버 · Docker/Kind 불필요 · CI 실행 가능

- [E19: PillarTarget CRD 라이프사이클](#e19-pillartarget-crd-라이프사이클)
- [E20: PillarPool CRD 라이프사이클](#e20-pillarpool-crd-라이프사이클)
- [E23: PillarProtocol CRD 라이프사이클](#e23-pillarprotocol-crd-라이프사이클)
- [E25: PillarBinding CRD 라이프사이클](#e25-pillarbinding-crd-라이프사이클)
- [E26: 교차-CRD 라이프사이클 상호작용](#e26-교차-crd-라이프사이클-상호작용)
- [E21.2–E21.4: 잘못된 CR 웹훅·스키마 검증](#e212-pillartarget-웹훅--불변-필드-수정-거부-type-c--envtest-) _(E21 중 envtest 소섹션)_

### 카테고리 2 — 클러스터 레벨 E2E 테스트 (유형 B: Kind 클러스터 필요) ⚠️
> 빌드 태그: `//go:build e2e` | `go test ./test/e2e/ -tags=e2e -v` | 총 3개 테스트 (E10); Helm 설치 검증 29개 테스트 (E26.1–E26.12, ID 207–243) 포함

- [E10: 클러스터 레벨 E2E 테스트](#e10-클러스터-레벨-e2e-테스트)
- [E26: Helm 차트 설치 및 릴리스 검증](#e26-helm-차트-설치-및-릴리스-검증)
  - [E26.1: Helm 차트 기본값 설치 성공](#e261-helm-차트-기본값-설치-성공)
  - [E26.2: Helm 릴리스 상태 검증 (helm status)](#e262-helm-릴리스-상태-검증-helm-status)
  - [E26.3: Helm 릴리스 목록 검증 (helm list)](#e263-helm-릴리스-목록-검증-helm-list)
  - [E26.4: 배포된 Kubernetes 리소스 정상 동작 검증](#e264-배포된-kubernetes-리소스-정상-동작-검증)
  - [E26.5: CRD 등록 및 가용성 검증](#e265-crd-등록-및-가용성-검증)
    - [E26.5.1: CRD 4종 일괄 존재 및 Established 상태 검증](#e2651-crd-4종-일괄-존재-및-established-상태-검증)
    - [E26.5.2: 각 CRD 메타데이터 상세 검증 (그룹·버전·범위·shortName)](#e2652-각-crd-메타데이터-상세-검증--그룹버전범위shortname)
    - [E26.5.3: kubectl api-resources를 통한 API 가용성 검증](#e2653-kubectl-api-resources를-통한-api-가용성-검증)
    - [E26.5.4: 각 CRD OpenAPI v3 스키마 존재 검증](#e2654-각-crd-openapi-v3-스키마-존재-검증)
    - [E26.5.5: 각 CRD 프린터 컬럼 검증](#e2655-각-crd-프린터-컬럼additionalprintercolumns-검증)
    - [E26.5.6: Helm resource-policy: keep 어노테이션 검증](#e2656-helm-resource-policy-keep-어노테이션-검증)
    - [E26.5.7: CRD CRUD 기본 동작 (샘플 오브젝트 생성/조회/삭제)](#e2657-crd-crud-기본-동작--샘플-오브젝트-생성조회삭제)
  - [E26.6: 커스텀 values 오버라이드 설치](#e266-커스텀-values-오버라이드-설치)
  - [E26.7: installCRDs=false 설치 모드 검증](#e267-installcrdsfalse-설치-모드-검증)
  - [E26.8: 중복 설치 시도 오류 검증](#e268-중복-설치-시도-오류-검증)
  - [E26.9: Helm 차트 업그레이드 (helm upgrade)](#e269-helm-차트-업그레이드-helm-upgrade)
  - [E26.10: Helm 차트 설치 해제 및 리소스 정리](#e2610-helm-차트-설치-해제-및-리소스-정리)
  - [E26.11: Helm 배포 후 전체 파드 Running 상태 종합 검증](#e2611-helm-배포-후-전체-파드-running-상태-종합-검증)
  - [E26.12: CSIDriver 객체 생성 및 설정 검증](#e2612-csidriver-객체-생성-및-설정-검증)
    - [E26.12.1: CSIDriver 존재 및 이름 검증](#e26121-csidriver-존재-및-이름-검증)
    - [E26.12.2: attachRequired 필드 검증](#e26122-attachedrequired-필드-검증)
    - [E26.12.3: podInfoOnMount 필드 검증](#e26123-podinfoonmount-필드-검증)
    - [E26.12.4: fsGroupPolicy 필드 검증](#e26124-fsgrouppolicy-필드-검증)
    - [E26.12.5: volumeLifecycleModes 필드 검증](#e26125-volumelifecyclemodes-필드-검증)
    - [E26.12.6: Helm 레이블 및 어노테이션 검증](#e26126-helm-레이블-및-어노테이션-검증)
    - [E26.12.7: csiDriver.create=false 시 CSIDriver 미생성 검증](#e26127-csidrivercreatefalse-시-csidriver-미생성-검증)
    - [E26.12.8: 커스텀 values로 CSIDriver 설정 오버라이드 검증](#e26128-커스텀-values로-csidriver-설정-오버라이드-검증)

### 카테고리 3 — 완전 E2E / 수동 스테이징 테스트 (유형 F) ❌
> 빌드 태그: `//go:build e2e_full` | 실제 ZFS/NVMe-oF 커널 모듈 필요 | 베어메탈/KVM 서버 필요

- [유형 F: 완전 E2E 테스트 (Full E2E)](#유형-f-완전-e2e-테스트-full-e2e--표준-ci-불가)
- [수동/스테이징 테스트 카탈로그 (Manual/Staging Tests)](#수동스테이징-테스트-카탈로그-manualstaging-tests)

### 부록
- [부록: 테스트 실행 참조](#부록-테스트-실행-참조)
- [부록: CI 환경에서 루프백 장치를 이용한 ZFS 스토리지 모킹](#부록-ci-환경에서-루프백-장치를-이용한-zfs-스토리지-모킹)
- [부록: Fake Configfs — NVMe-oF 설정 파일시스템 CI 시뮬레이션](#부록-fake-configfs--nvme-of-설정-파일시스템-ci-시뮬레이션)
- [부록: Mock Agent 모드 — CI에서 CSI 에이전트 시뮬레이션](#부록-mock-agent-모드--ci에서-csi-에이전트-시뮬레이션)

---

## 테스트 케이스 필드 정의

각 테스트 케이스는 아래 **6가지 필드**를 포함한다. 테스트 유형(E/F vs M)에 따라 열 이름이 다를 수 있지만, 6가지 필드는 모든 테스트 케이스에 반드시 존재한다.

### E/F 섹션 (자동화 테스트) 필드 매핑

| 필드 | 열 이름 | 설명 |
|------|---------|------|
| **ID** | `ID` | 섹션 내 고유 번호 (예: `E1.1`, `F3.2`) |
| **이름** | `테스트 함수` | Go 테스트 함수 이름; 구현 파일에서 직접 추적 가능 |
| **설명** | `설명` | 테스트가 검증하는 동작의 간결한 서술 |
| **사전 조건** | `사전 조건` | 테스트 실행 전 충족되어야 할 환경 상태 (mock 설정, CRD 사전 등록, 오류 주입 등) |
| **단계** | `단계` | 테스트 수행 중 실행되는 호출 순서 (RPC 호출 시퀀스) |
| **기대 결과** | `기대 결과` | 테스트 통과를 위해 충족되어야 할 반환값, 상태, 호출 횟수 |
| **컴포넌트 커버리지** | `커버리지` | 이 테스트가 실질적인 실행 경로를 통과시키는 컴포넌트 |

### M 섹션 (수동/스테이징 테스트) 필드 매핑

M 섹션은 자동화가 불가능한 수동 시나리오를 다루므로 열 이름이 다르다. 6가지 필수 필드는 동일하게 포함되며, 아래와 같이 매핑된다.

| 필드 | M 섹션 열 이름 | 설명 |
|------|--------------|------|
| **ID** | `ID` | 섹션 내 고유 번호 (예: `M1.1`, `M5.3`) |
| **이름** | `시나리오` | Go 테스트 함수가 없으므로 시나리오 명칭으로 대체 |
| **설명** | `사전 조건` 내 포함 | 시나리오 제목이 설명 역할을 겸함 |
| **사전 조건** | `사전 조건` | 테스트 환경 구성 요건 (클러스터 상태, 노드 수, 하드웨어 등) |
| **단계** | `수동 실행 절차` | 검증자가 직접 수행하는 단계별 CLI/kubectl 명령 시퀀스 |
| **기대 결과** | `허용 기준` | 시나리오 통과를 판단하는 정량적·정성적 기준 |
| **컴포넌트 커버리지** | `커버리지` | 이 시나리오가 실행 경로를 통과시키는 컴포넌트 |

**컴포넌트 약어표:**

| 약어 | 컴포넌트 경로 |
|------|-------------|
| `CSI-C` | `internal/csi.ControllerServer` |
| `CSI-N` | `internal/csi.NodeServer` |
| `Agent` | `internal/agent.Server` (또는 mockAgentServer gRPC stub) |
| `ZFS` | `internal/backend.ZFSBackend` |
| `NVMeF` | `internal/nvmeof.NvmetTarget` |
| `Conn` | `internal/connector.Connector` (또는 mockCSIConnector) |
| `Mnt` | `internal/mounter.Mounter` (또는 mockCSIMounter) |
| `VolCRD` | `api/v1alpha1.PillarVolume` CRD 상태 관리 |
| `TgtCRD` | `api/v1alpha1.PillarTarget` CRD 조회 |
| `mTLS` | `internal/controller.MTLSController` |
| `SM` | VolumeStateMachine (볼륨 순서 상태 머신) |
| `State` | 노드 스테이징 상태 파일 (`StateDir/*.json`) |
| `gRPC` | gRPC 직렬화/역직렬화 레이어 (실제 TCP 리스너) |

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

## CI 실행 가능 테스트 카탈로그

이 섹션은 **표준 CI 환경에서 실행 가능한 테스트 전체 목록**을 정의하고,
각 테스트 유형에 필요한 인프라 설정 요구사항과 범위 제약을 명시한다.

---

### CI 실행 가능 테스트 요약 (유형 A: 인프로세스 E2E)

빌드 태그 없이 `go test ./test/e2e/ -v` 명령으로 실행 가능한 모든 테스트.
실제 Kubernetes 클러스터, 커널 모듈, root 권한이 **전혀 불필요**하다.

| 섹션 | 테스트 그룹 | 테스트 수 | 실행 패턴 |
|------|------------|----------|-----------|
| E1 | 볼륨 라이프사이클 — CreateVolume / DeleteVolume | 13 | `TestCSIController_CreateVolume`, `TestCSIController_DeleteVolume`, `TestCSIController_FullRoundTrip` |
| E2 | CSI Controller — ControllerPublish / ControllerUnpublish / ControllerExpand / Validate (+ E2.5 노드친화성 + E2.6 오류처리) | 8 (e2e) + 컴포넌트 | `TestCSIController_ControllerPublish*`, `TestCSIController_ControllerUnpublish*`, `TestCSIController_ControllerExpand*`, `TestCSIController_Validate*`, `TestCSIPublishIdempotency_ControllerPublish*`, `TestCSIErrors_ControllerPublish*`, `TestCSIErrors_ControllerUnpublish*` |
| E3 | CSI Node — Stage / Publish / Unstage / Unpublish (E3.1–E3.23) | 70 | `TestCSINode_*`, `TestNodeStageVolume_*`, `TestNodeUnstageVolume_*`, `TestStageState_*`, `TestNodePublishVolume_*`, `TestNodeUnpublishVolume_*`, `TestNodeFullLifecycle` |
| E4 | 교차-컴포넌트 라이프사이클 | 4 | `TestCSILifecycle_*` |
| E5 | 순서 제약 (Ordering Constraints) | 6 | `TestCSIOrdering_*` |
| E6 | 부분 실패 영속성 | 5 | `TestCSIController_PartialFailure_*`, `TestCSIController_DeleteVolume_CleansUpCRD` |
| E7 | 게시 멱등성 (Publish Idempotency) | 5 | `TestCSIPublishIdempotency_*` |
| E8 | mTLS 컨트롤러 통합 | 3 | `TestMTLSController_*` |
| E9 | Agent gRPC E2E | 6 | `TestAgent_*` |
| E11 | 볼륨 확장 통합 E2E | 8 | `TestCSIExpand_*` |
| E12 | CSI 스냅샷 미구현 검증 | 4 | `TestCSISnapshot_*` |
| E13 | 볼륨 클론 미처리 동작 검증 | 2 | `TestCSIClone_*` |
| E14 | 잘못된 입력값 및 엣지 케이스 | 15 | `TestCSIEdge_*` |
| E15 | 리소스 고갈 오류 전파 | 6 | `TestCSIExhaustion_*` |
| E16 | 동시 작업 안전성 | 7 | `TestCSIConcurrent_*` |
| E17 | 정리 검증 | 8 | `TestCSICleanup_*` |
| E18 | Agent 다운 오류 시나리오 | 6 | `TestCSIController_CreateVolume_AgentUnreachable`, `TestCSIErrors_*Agent*`, `TestAgent_ReconcileState*` |
| E21.1 | 잘못된 CR 런타임 처리 (in-process) | 6 | `TestCSIInvalidCR_*` |
| E22.1 | CSI Controller — 미지원 프로토콜 타입 전파 (in-process) | 4 | `TestCSIProtocol_CreateVolume_*`, `TestCSIProtocol_ControllerPublish_*` |
| E22.2 | Agent gRPC — 미지원 프로토콜 거부 (기존 4 + 신규 2) | 6 | `TestAgentErrors_*_InvalidProtocol`, `TestAgentProtocol_*` |
| E22.3 | CSI Controller — 미지원 백엔드 타입 전파 (in-process) | 2 | `TestCSIProtocol_CreateVolume_UnknownBackendType_*`, `TestCSIProtocol_CreateVolume_LVMBackend*` |

**Type C (envtest 통합, integration 빌드 태그):**

| # | 이름 | 테스트 수 | 테스트 함수 패턴 |
|---|------|----------|----------------|
| E21.2 | PillarTarget 웹훅 검증 | 7 | `TestPillarTargetWebhook_*` |
| E21.3 | PillarPool 웹훅 검증 | 5 | `TestPillarPoolWebhook_*` |
| E21.4 | CRD OpenAPI 스키마 검증 | 8 | `TestCRDSchema_*` |
| E26 | 교차-CRD 라이프사이클 상호작용 | 23 | `TestCrossLifecycle_*` |

| **합계** | | **201** (in-process 158 + envtest 43) | |

---

### CI 설정 요구사항 (유형 A)

#### 최소 요구사항

| 항목 | 버전 / 사양 | 비고 |
|------|------------|------|
| Go 빌드 도구체인 | 1.22+ | `go test ./test/e2e/ -v` 실행 |
| OS | Linux (amd64/arm64) 또는 macOS | tmpfs/tmpdir 지원 필요 |
| RAM | 512 MiB 이상 | 동시 테스트 고루틴 수용 |
| 디스크 | 200 MiB 여유 공간 | `t.TempDir()` 임시 파일 |
| 네트워크 | localhost loopback만 필요 | 외부 네트워크 불필요 |

#### GitHub Actions 설정 예시

```yaml
jobs:
  e2e-inprocess:
    name: "In-Process E2E Tests"
    runs-on: ubuntu-22.04          # 또는 ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.22"
          cache: true
      - name: Run In-Process E2E Tests
        run: |
          go test ./test/e2e/ -v -timeout 120s \
            -count=1 \
            2>&1 | tee e2e-inprocess.log
      - name: Upload test logs
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: e2e-inprocess-logs
          path: e2e-inprocess.log
```

#### GitLab CI 설정 예시

```yaml
e2e-inprocess:
  stage: test
  image: golang:1.22-bookworm
  script:
    - go test ./test/e2e/ -v -timeout 120s -count=1
  artifacts:
    when: always
    reports:
      junit: e2e-inprocess-report.xml
```

---

### 범위 제약 (유형 A — 인프로세스 E2E)

유형 A 테스트는 아래 항목을 **검증하지 못한다**. 각 항목은 실제 하드웨어
또는 클러스터 환경이 필요하다.

| 검증 불가 항목 | 이유 | 대안 (향후) |
|--------------|------|------------|
| 실제 ZFS zvol 생성/삭제 | `zfs(8)` 명령 미실행; mock backend 사용 | F1 (`TestRealZFS_CreateVolume`) |
| 실제 NVMe-oF configfs 바인딩 | `/sys/kernel/config/nvmet` 없음; `t.TempDir()` 사용 | F2 (`TestRealNVMeoF_Export`) |
| 실제 `nvme connect` / `nvme disconnect` | `nvme-tcp` 커널 모듈 불필요; mockConnector 사용 | F3 (`TestRealNVMeoF_Connect`) |
| 실제 `mount(8)` / `umount(8)` 시스템 콜 | root 권한 불필요; mockMounter 사용 | F8–F10 |
| 실제 `mkfs.ext4` / `mkfs.xfs` | 포맷 명령 미실행; mockMounter 사용 | F8 |
| 실제 `resize2fs` / `xfs_growfs` | 리사이즈 명령 미실행; mockResizer 사용 | F21 |
| Kubernetes etcd 일관성 (옵티미스틱 잠금) | fake client는 충돌 시나리오 부분 지원 | F4 |
| CRD 검증 웹훅 실제 실행 | fake client는 웹훅 미실행 | E10 (유형 B) |
| RBAC 실제 검증 | fake client는 RBAC 미적용 | E10 (유형 B) |
| cert-manager 인증서 발급 (실제 PKI) | 인메모리 `testcerts` 패키지 사용 | F7 |
| NVMe-oF multipath | mockConnector 단일 경로만 | F12 |
| 노드 재시작 후 agent ReconcileState 자동 복구 | in-process 환경에서 재시작 불가 | F6 |
| 대용량 데이터 마이그레이션 (SendVolume/ReceiveVolume) | 스트리밍 RPC + 실제 데이터 필요 | F17–F19 |
| 실제 `flock` / 커널 레벨 레이스 컨디션 | in-process 고루틴으로 애플리케이션 레벨만 검증 | F24 |
| PVC/Pod Kubernetes 프로비저닝 흐름 | kubectl, external-provisioner 미사용 | F4 |
| 용량 100개 PVC 동시 생성 확장성 | mockAgentServer는 실제 처리 시간 없음 | F25 |

---

### 유형 A-Kind: Kind 클러스터 + Mock 백엔드 테스트 ⚠️ CI 실행 가능 (Kind 지원 러너 필요)

**정의:** Kind 클러스터를 사용하지만 **실제 스토리지 하드웨어(ZFS/NVMe-oF)를 쓰지 않는** 테스트.
Kubernetes API 서버와 etcd의 실제 동작(CRD 검증 웹훅, RBAC, 어드미션 컨트롤러)을
검증하되, 스토리지 백엔드는 여전히 스텁/모의 구현을 사용한다.

유형 A-Kind 테스트는 현재 **E10 (클러스터 레벨 E2E)** 의 일부를 표준 CI에서도
실행 가능하게 하는 목표를 가진다.

#### 유형 A-Kind vs 유형 A vs 유형 B 비교

| 속성 | 유형 A (인프로세스) | 유형 A-Kind | 유형 B (클러스터 레벨) |
|------|-------------------|------------|----------------------|
| Kubernetes 클러스터 | 불필요 (fake client) | Kind 클러스터 필요 | Kind 클러스터 필요 |
| CRD 설치 | fake client에 스키마만 등록 | 실제 CRD YAML 설치 | 실제 CRD YAML 설치 |
| 어드미션 웹훅 | 미실행 | 실행 (pillar-csi 웹훅 서버 필요) | 실행 |
| RBAC 검증 | 미실행 | 실행 | 실행 |
| 스토리지 백엔드 | mockAgentServer | mockAgentServer (out-of-cluster) | 해당 없음 (볼륨 I/O 테스트 없음) |
| pillar-csi 컨테이너 이미지 | 불필요 | 필요 (웹훅 서버용) | 필요 |
| cert-manager | 불필요 | 선택적 (`CERT_MANAGER_INSTALL_SKIP=true`) | 필요 |
| 실제 ZFS / NVMe-oF | 불필요 | 불필요 | 불필요 |
| 표준 GitHub Actions | ✅ (ubuntu runner) | ✅ (ubuntu + `kind-action`) | ❌ (self-hosted 러너 권장) |

#### 유형 A-Kind 테스트 목록

아래 테스트는 Kind 클러스터에서 mock 백엔드를 사용하여 Kubernetes 통합을
검증한다. 현재 이 테스트들은 유형 B (`//go:build e2e`) 안에 포함되어 있으나,
별도 빌드 태그(`//go:build e2e_kind_mock`)로 분리하면 표준 CI에서 실행 가능하다.

| # | 테스트 이름 (계획) | 검증 항목 | 필요 구성 요소 |
|---|----------------|----------|---------------|
| K1 | `TestKindMock_CRD_PillarTarget_Validation` | PillarTarget CRD 스키마 검증 웹훅이 잘못된 필드를 거부 | Kind + CRD + 웹훅 서버 |
| K2 | `TestKindMock_CRD_PillarVolume_Lifecycle` | PillarVolume CRD Create/Update/Delete 전체 생명주기 | Kind + CRD |
| K3 | `TestKindMock_RBAC_ControllerManager` | controller-manager ServiceAccount가 PillarTarget/PillarVolume에 대한 RBAC 권한만 가짐 | Kind + RBAC 설정 |
| K4 | `TestKindMock_ControllerManager_Startup` | pillar-csi controller-manager 파드가 정상 기동 및 헬스체크 응답 | Kind + 컨테이너 이미지 |
| K5 | `TestKindMock_Metrics_Endpoint` | `/metrics` 엔드포인트가 RBAC 토큰으로 접근 가능; Go 런타임 메트릭 포함 | Kind + 컨테이너 이미지 |
| K6 | `TestKindMock_Webhook_CertManager` | cert-manager가 webhook-server-cert Secret을 발급하고 웹훅 caBundle에 주입 | Kind + cert-manager + 컨테이너 이미지 |
| K7 | `TestKindMock_PillarTarget_AgentConnected_False` | PillarTarget 생성 시 pillar-agent 미도달 상태에서 AgentConnected=False 조건 설정 | Kind + 컨테이너 이미지 (mock agent endpoint) |
| K8 | `TestKindMock_PillarTarget_AgentConnected_True` | mock pillar-agent gRPC 서버(out-of-cluster)를 지정한 PillarTarget에서 AgentConnected=True 조건 설정 | Kind + 컨테이너 이미지 + mock agent |

#### 유형 A-Kind CI 설정 요구사항

| 구성 요소 | 버전 | 비고 |
|-----------|------|------|
| Kind | v0.23+ | `helm/kind-action@v1` 또는 직접 설치 |
| Docker | 24+ | Kind 클러스터 노드 컨테이너용 |
| kubectl | v1.29+ | CRD/RBAC 설치 및 검증 |
| pillar-csi 이미지 | `example.com/pillar-csi:v0.0.1` | `make docker-build` 후 `kind load` |
| cert-manager | v1.14+ (선택) | `CERT_MANAGER_INSTALL_SKIP=true`로 스킵 가능 |
| Go 빌드 도구체인 | 1.22+ | mock agent 컴파일 및 테스트 실행 |

#### GitHub Actions — 유형 A-Kind 설정 예시

```yaml
jobs:
  e2e-kind-mock:
    name: "Kind + Mock Backend E2E Tests"
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.22"
          cache: true
      - name: Create Kind cluster
        uses: helm/kind-action@v1
        with:
          version: v0.23.0
          cluster_name: pillar-csi-test
      - name: Build and load controller image
        run: |
          make docker-build IMG=example.com/pillar-csi:v0.0.1
          kind load docker-image example.com/pillar-csi:v0.0.1 \
            --name pillar-csi-test
      - name: Install CRDs
        run: make install
      - name: Install cert-manager (선택)
        run: |
          kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.14.5/cert-manager.yaml
          kubectl rollout status deployment/cert-manager -n cert-manager --timeout=120s
      - name: Run Kind+Mock E2E Tests
        run: |
          go test ./test/e2e/ -tags=e2e -v -timeout 300s \
            -count=1 \
            2>&1 | tee e2e-kind-mock.log
        env:
          CERT_MANAGER_INSTALL_SKIP: "true"
      - name: Cleanup
        if: always()
        run: kind delete cluster --name pillar-csi-test
```

#### 유형 A-Kind 범위 제약

유형 A-Kind 테스트는 아래 항목을 **검증하지 못한다**:

| 검증 불가 항목 | 이유 |
|--------------|------|
| 실제 스토리지 I/O (ZFS zvol, NVMe-oF 블록 디바이스) | mock backend 사용; 커널 모듈 불필요 |
| NodeStage/NodePublish 실제 마운트 동작 | CSI Node plugin이 DaemonSet으로 실행되지 않음 |
| 다중 노드 스토리지 노드 페일오버 | Kind 단일 노드 또는 Kind 멀티 노드 (스토리지 없음) |
| 실제 `nvme connect` 결과 검증 | mock connector |
| 대규모 PVC 프로비저닝 (100개 이상) 성능 | CI 환경 리소스 제한 |
| NVMe-oF multipath / 이중화 구성 | mock connector 단일 경로 |

---

### 테스트 유형별 실행 결정 트리

```
테스트를 추가해야 하는가?
    │
    ├── 실제 ZFS/NVMe-oF/mount(8) 동작 필요?
    │       ├── 예 ──► 유형 F (향후 하드웨어 테스트)
    │       └── 아니오
    │               │
    │               ├── 실제 Kubernetes API (CRD 웹훅/RBAC/어드미션) 검증 필요?
    │               │       ├── 예 ──► 유형 A-Kind 또는 유형 B
    │               │       └── 아니오
    │               │               │
    │               │               └── 유형 A (인프로세스) ← 대부분의 E2E는 여기에 해당
    │               │
    │               └── 실제 pillar-csi 이미지 빌드/배포 필요?
    │                       ├── 예 ──► 유형 B (표준 CI 불가 → self-hosted 러너)
    │                       └── 아니오 ──► 유형 A-Kind (Kind + mock)
```

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

# 카테고리 1 — 표준 CI 실행 가능 테스트 (유형 A: 인프로세스 E2E) ✅

> **빌드 태그:** 없음 | **실행:** `go test ./test/e2e/ -v`
>
> Kubernetes 클러스터 불필요 · 커널 모듈 불필요 · root 권한 불필요 · 총 **134개** 테스트

이 카테고리의 테스트는 **표준 CI 환경(GitHub Actions `ubuntu-22.04` 러너 등)** 에서
아무런 추가 인프라 없이 실행된다. 실제 ZFS, NVMe-oF, Kubernetes API 서버 대신
in-process 테스트 더블(mockAgentServer, mockCSIConnector, mockCSIMounter, fake k8s client)을
사용하여 교차-컴포넌트 동작을 검증한다.

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

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 1 | `TestCSIController_CreateVolume` | CreateVolume이 agent.CreateVolume → agent.ExportVolume을 순서대로 호출하고 올바른 VolumeId/VolumeContext를 반환 | PillarTarget="storage-1" fake 클라이언트에 등록; mockAgentServer 정상 동작; zfs-pool="tank"; 프로토콜=nvmeof-tcp; 용량=1GiB | 1) CreateVolumeRequest 전송 | VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-create-test"; VolumeContext에 target_id/address/port/volume-ref/protocol-type 포함 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 2 | `TestCSIController_CreateVolume_Idempotency` | 동일한 볼륨 이름으로 CreateVolume을 두 번 호출하면 두 번째 호출은 agent.CreateVolume/ExportVolume을 재호출하지 않고 동일한 응답 반환 | 위와 동일; mockAgentServer 정상 동작 | 1) CreateVolumeRequest 전송; 2) 동일 파라미터로 CreateVolumeRequest 재전송 | 두 번째 호출 성공; 동일한 VolumeId 반환; agent CreateVolume은 1회, ExportVolume은 1회만 호출 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

---

### E1.2 CreateVolume — 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 3 | `TestCSIController_CreateVolume_MissingParams` | StorageClass 파라미터 누락 시 InvalidArgument 반환 | ControllerServer 초기화; StorageClass Parameters에서 필수 키(target/backend-type/protocol-type/zfs-pool) 일부 또는 전부 제거 | 1) 파라미터 일부 누락한 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 4 | `TestCSIController_CreateVolume_PillarTargetNotFound` | 참조된 PillarTarget이 존재하지 않으면 NotFound 반환 | fake 클라이언트에 PillarTarget 미등록; Parameters["target"]="nonexistent" | 1) CreateVolumeRequest 전송 | gRPC NotFound 또는 Internal; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| 5 | `TestCSIController_CreateVolume_AgentCreateError` | agent.CreateVolume 실패 시 오류 전파 | mockAgentServer.CreateVolumeErr 설정; PillarTarget 정상 등록 | 1) CreateVolumeRequest 전송 | 비-OK gRPC 상태 반환; ExportVolume 미호출 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| 6 | `TestCSIController_CreateVolume_AgentExportError` | agent.CreateVolume 성공 후 agent.ExportVolume 실패 시 오류 전파 | mockAgentServer.ExportVolumeErr 설정; CreateVolume은 성공 | 1) CreateVolumeRequest 전송 | 비-OK gRPC 상태 반환; PillarVolume CRD에 PartialFailure 기록 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

---

### E1.3 DeleteVolume — 정상 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 7 | `TestCSIController_DeleteVolume` | DeleteVolume이 agent.UnexportVolume → agent.DeleteVolume을 순서대로 호출 | CreateVolume으로 볼륨 사전 생성 (PillarVolume CRD 존재); mockAgentServer 정상 동작 | 1) CreateVolumeRequest 전송; 2) DeleteVolumeRequest 전송 | 성공; UnexportVolume 1회, DeleteVolume 1회 호출 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| 8 | `TestCSIController_DeleteVolume_Idempotency` | 이미 삭제된 볼륨을 다시 DeleteVolume해도 성공 (멱등성) | 볼륨 생성 후 첫 DeleteVolume 완료 | 1) DeleteVolumeRequest 전송; 2) 동일 VolumeId로 DeleteVolumeRequest 재전송 | 두 번째 호출도 성공; 오류 없음 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| 9 | `TestCSIController_DeleteVolume_NotFoundIsIdempotent` | agent가 NotFound를 반환해도 DeleteVolume은 성공 처리 | mockAgentServer.UnexportVolumeErr = gRPC NotFound; PillarVolume CRD 없음 | 1) DeleteVolumeRequest 전송 | DeleteVolume 성공; CSI 명세상 Not-Found는 이미 삭제된 것으로 처리 | `CSI-C`, `Agent`, `gRPC` |

---

### E1.4 DeleteVolume — 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 10 | `TestCSIController_DeleteVolume_MalformedID` | 잘못된 형식의 VolumeId는 InvalidArgument 반환 | ControllerServer 초기화 | 1) VolumeId="noslash"로 DeleteVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 11 | `TestCSIController_DeleteVolume_AgentError` | agent.UnexportVolume 또는 agent.DeleteVolume 실패 시 오류 전파 | mockAgentServer.DeleteVolumeErr 설정; PillarVolume CRD 존재 | 1) DeleteVolumeRequest 전송 | 비-OK gRPC 상태 반환 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

### E1.5 기본 프로비저닝 — 전체 왕복(Full Round Trip)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 12 | `TestCSIController_FullRoundTrip` | CreateVolume → ControllerPublishVolume → ControllerUnpublishVolume → DeleteVolume 전체 CSI Controller 왕복 테스트 | 단일 mockAgentServer; fake k8s 클라이언트; PillarTarget 등록; 정상 경로 설정 | 1) CreateVolume; 2) ControllerPublishVolume; 3) ControllerUnpublishVolume; 4) DeleteVolume | 모든 단계 성공; agent 호출 순서 검증; VolumeContext 키 검증 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 13 | `TestCSIController_VolumeIDFormatPreservation` | VolumeId 포맷("target/protocol/backend/pool/name")이 생성-게시-삭제 전 주기에서 보존됨 | CreateVolume 성공; PillarTarget 등록 | 1) CreateVolume; 2) ControllerPublishVolume; 3) ControllerUnpublishVolume; 4) DeleteVolume | 각 단계에서 동일한 VolumeId 포맷 사용; 파싱 오류 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

---

### E1.6 접근 모드 유효성 검증 (Access Mode Validation)

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

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.6-1 | `TestCSIController_CreateVolume_AccessMode_RWO` | SINGLE_NODE_WRITER(RWO) 접근 모드로 CreateVolume 성공 | PillarTarget="storage-1" fake 클라이언트에 등록; mockAgentServer 정상; zfs-pool="tank" | 1) AccessMode=SINGLE_NODE_WRITER, VolumeCapabilities 포함 CreateVolumeRequest 전송 | 성공 (gRPC OK); VolumeId/VolumeContext 반환; agent.CreateVolume 1회, agent.ExportVolume 1회 호출 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| E1.6-2 | `TestCSIController_CreateVolume_AccessMode_RWOP` | SINGLE_NODE_SINGLE_WRITER(RWOP) 접근 모드로 CreateVolume 성공 | 위와 동일 | 1) AccessMode=SINGLE_NODE_SINGLE_WRITER로 CreateVolumeRequest 전송 | 성공; VolumeId/VolumeContext 반환; agent 호출 정상 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| E1.6-3 | `TestCSIController_CreateVolume_AccessMode_ROX` | MULTI_NODE_READER_ONLY(ROX) 접근 모드로 CreateVolume 성공 | 위와 동일 | 1) AccessMode=MULTI_NODE_READER_ONLY로 CreateVolumeRequest 전송 | 성공; VolumeId/VolumeContext 반환; agent 호출 정상 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| E1.6-4 | `TestCSIController_CreateVolume_AccessMode_RWX_Rejected` | MULTI_NODE_MULTI_WRITER(RWX) 접근 모드는 드라이버 수준에서 거부 | ControllerServer 초기화만 필요; PillarTarget/agent 연결 불필요 | 1) AccessMode=MULTI_NODE_MULTI_WRITER로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "unsupported access mode" 메시지 포함; agent.CreateVolume 호출 없음 | `CSI-C` |
| E1.6-5 | `TestCSIController_CreateVolume_AccessMode_Unknown_Rejected` | 정의되지 않은(UNKNOWN=0) 접근 모드는 거부 | ControllerServer 초기화만 필요 | 1) AccessMode=UNKNOWN(0)으로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| E1.6-6 | `TestCSIController_CreateVolume_AccessMode_Missing_InCapability` | VolumeCapability에 AccessMode 필드 자체가 없으면 InvalidArgument | ControllerServer 초기화만 필요 | 1) VolumeCapability{AccessMode: nil}로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "must specify an access_mode" 메시지; agent 호출 없음 | `CSI-C` |
| E1.6-7 | `TestCSIController_CreateVolume_VolumeCapabilities_Empty` | VolumeCapabilities가 빈 슬라이스이면 InvalidArgument | ControllerServer 초기화만 필요 | 1) VolumeCapabilities=[]로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "volume_capabilities must not be empty" 메시지; agent 호출 없음 | `CSI-C` |
| E1.6-8 | `TestCSIController_CreateVolume_MultipleCapabilities_AnyUnsupported` | 여러 VolumeCapability 중 하나라도 미지원 모드이면 전체 거부 | ControllerServer 초기화만 필요 | 1) [SINGLE_NODE_WRITER, MULTI_NODE_MULTI_WRITER] 두 개의 VolumeCapability를 포함한 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |

---

### E1.7 용량 범위 검증 (Capacity Range Validation)

`CapacityRange.RequiredBytes`와 `LimitBytes`는 CSI 명세상 선택 사항이다.
두 값이 모두 제공되면 `RequiredBytes ≤ LimitBytes`를 만족해야 한다.
기존 볼륨(PillarVolume CRD Ready 상태)과 용량 충돌 시 `AlreadyExists`를 반환한다.

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요

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

### E1.8 PillarTarget 상태 및 agent 연결 검증

`CreateVolume`은 `PillarTarget`을 조회하여 agent 주소를 얻는다.
대상이 없거나 주소가 비어 있으면 요청이 즉시 실패한다.

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.8-1 | `TestCSIController_CreateVolume_PillarTargetEmptyAddress` | PillarTarget이 존재하지만 ResolvedAddress=""이면 Unavailable 반환 | fake 클라이언트에 PillarTarget 등록; Status.ResolvedAddress="" | 1) 해당 target을 참조하는 CreateVolumeRequest 전송 | gRPC Unavailable; "has no resolved address; agent may not be ready" 메시지; agent 다이얼 시도 없음 | `CSI-C`, `TgtCRD` |
| E1.8-2 | `TestCSIController_CreateVolume_PillarTargetNotFound` | Parameters["target"]이 존재하지 않는 PillarTarget을 참조하면 NotFound 반환 | fake 클라이언트에 PillarTarget 미등록; Parameters["pillar-csi.bhyoo.com/target"]="ghost-node" | 1) CreateVolumeRequest 전송 | gRPC NotFound; "PillarTarget … not found" 메시지; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| E1.8-3 | `TestCSIController_CreateVolume_AgentDialFails` | agent 다이얼 자체가 실패하면 Unavailable 반환 | PillarTarget 등록 (ResolvedAddress=유효); dialAgent 함수에 연결 실패 에러 주입 | 1) CreateVolumeRequest 전송 | gRPC Unavailable; "failed to dial agent" 메시지; agent.CreateVolume 호출 없음 | `CSI-C`, `TgtCRD`, `gRPC` |

---

### E1.9 부분 실패 복구 (Partial Failure Recovery)

`CreateVolume`은 `agent.CreateVolume` 성공 후 `agent.ExportVolume` 호출 직전에
`PillarVolume CRD`를 `CreatePartial` 단계로 기록한다.
컨트롤러 재시작 또는 일시적 오류 후 CO가 동일 요청을 재시도하면:

1. 상태 머신에 `StateCreatePartial`가 로드된다.
2. `agent.CreateVolume`은 **건너뛰고** `agent.ExportVolume`만 재호출한다.

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.9-1 | `TestCSIController_PartialFailure_CreateThenExportFail` | agent.CreateVolume 성공 후 agent.ExportVolume 실패 시 PillarVolume CRD에 CreatePartial 기록 | mockAgentServer: ExportVolumeErr=gRPC Internal; PillarTarget 정상 | 1) CreateVolumeRequest 전송 | gRPC Internal 반환; PillarVolume CRD에 phase=CreatePartial, BackendDevicePath 기록됨 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E1.9-2 | `TestCSIController_PartialFailure_ExportRetrySkipsBackend` | CreatePartial 상태에서 재시도 시 agent.CreateVolume 생략, agent.ExportVolume만 재호출 | PillarVolume CRD phase=CreatePartial; BackendDevicePath="/dev/zvol0" 사전 기록; 두 번째 호출에서 ExportVolumeErr 제거 | 1) CreateVolumeRequest 전송 (재시도) | 성공; agent.CreateVolume 호출 0회; agent.ExportVolume 호출 1회; 완성된 VolumeId/VolumeContext 반환 | `CSI-C`, `Agent`, `VolCRD`, `SM`, `gRPC` |
| E1.9-3 | `TestCSIController_PartialFailure_SelfHealing_TwoAttempts` | 첫 번째 호출(export 실패) → 두 번째 호출(정상) 연속 시나리오 | ExportVolumeErr를 첫 번째 호출에만 설정; 두 번째 호출 전 제거 | 1) 첫 CreateVolumeRequest 전송 (export 실패 예상); 2) 두 번째 CreateVolumeRequest 전송 | 1단계: gRPC Internal; 2단계: 성공; agent.CreateVolume 누적 1회; agent.ExportVolume 누적 2회 | `CSI-C`, `Agent`, `VolCRD`, `SM`, `gRPC` |
| E1.9-4 | `TestCSIController_PartialFailure_PersistPartialFails` | persistCreatePartial CRD 저장 실패 시 Internal 반환 (zvol은 생성됐으나 상태 기록 불가) | fake 클라이언트에 Create 오류 주입 (status.WriteFailure); agent.CreateVolume 성공 | 1) CreateVolumeRequest 전송 | gRPC Internal; "failed to persist partial-failure state" 메시지 | `CSI-C`, `Agent`, `VolCRD` |
| E1.9-5 | `TestCSIController_PartialFailure_LoadStateFromCRD` | 컨트롤러 재기동 시 기존 PillarVolume CRD에서 상태 복원 | PillarVolume CRD phase=CreatePartial를 직접 fake 클라이언트에 삽입; `LoadStateFromPillarVolumes` 호출 | 1) `LoadStateFromPillarVolumes` 호출; 2) CreateVolumeRequest 전송 | LoadStateFromPillarVolumes 성공; 이후 CreateVolumeRequest는 StateCreatePartial 인식하여 backend 건너뜀 | `CSI-C`, `VolCRD`, `SM` |

---

### E1.10 PVC 어노테이션 오버라이드 (PVC Annotation Override)

StorageClass 파라미터와 별도로, `external-provisioner`의 `--extra-create-metadata` 플래그가
활성화된 경우 `csi.storage.k8s.io/pvc-name` / `pvc-namespace` 파라미터가 CreateVolume 요청에
포함되어 PVC 어노테이션 오버라이드(Layer 4)가 적용된다.

**지원하는 오버라이드 어노테이션:**
- `pillar-csi.bhyoo.com/backend-override` — ZFS 프로퍼티 오버라이드 (YAML)
- `pillar-csi.bhyoo.com/protocol-override` — NVMe-oF / iSCSI 파라미터 오버라이드 (YAML)
- `pillar-csi.bhyoo.com/fs-override` — 파일시스템 포맷 파라미터 오버라이드 (YAML)
- `pillar-csi.bhyoo.com/param.<key>` — 저수준 플랫 키-값 오버라이드

**차단되는 구조적 필드:** `zfs.pool`, `zfs.parentDataset`, port 번호, protocol 종류 등

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — PVC는 fake 클라이언트에 등록

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.10-1 | `TestCSIController_CreateVolume_PVCAnnotation_BackendOverride_Compression` | PVC 어노테이션의 ZFS compression 프로퍼티가 agent BackendParams에 반영 | fake 클라이언트에 PVC 등록 (annotation: `pillar-csi.bhyoo.com/backend-override`); StorageClass Parameters에 pvc-name/pvc-namespace 포함 | 1) CreateVolumeRequest 전송 | 성공; agent.CreateVolume의 BackendParams에 `pillar-csi.bhyoo.com/zfs-prop.compression=zstd` 포함 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E1.10-2 | `TestCSIController_CreateVolume_PVCAnnotation_StructuralFieldBlocked` | 구조적 필드(`zfs.pool`)를 어노테이션으로 오버라이드하면 InvalidArgument 반환 | fake 클라이언트에 PVC 등록 (annotation에 zfs.pool 오버라이드 시도) | 1) CreateVolumeRequest 전송 | gRPC InvalidArgument; `pvcAnnotationValidationError` 발생; agent 호출 없음 | `CSI-C`, `VolCRD` |
| E1.10-3 | `TestCSIController_CreateVolume_PVCAnnotation_PVCNotFound_GracefulFallback` | pvc-name에 해당하는 PVC가 없으면 어노테이션 오버라이드 없이 기본 파라미터로 진행 | fake 클라이언트에 PVC 미등록; StorageClass Parameters에 pvc-name/pvc-namespace 포함 | 1) CreateVolumeRequest 전송 | 성공 (PVC 어노테이션 미적용 상태로 기본 파라미터 사용); agent.CreateVolume 정상 호출 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E1.10-4 | `TestCSIController_CreateVolume_PVCAnnotation_FlatKeyOverride` | 저수준 어노테이션(`pillar-csi.bhyoo.com/param.zfs-prop.volblocksize`)이 반영 | fake 클라이언트에 PVC 등록 (annotation: volblocksize=16K) | 1) CreateVolumeRequest 전송 | 성공; agent.CreateVolume의 BackendParams에 `pillar-csi.bhyoo.com/zfs-prop.volblocksize=16K` 포함 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

### E1.11 VolumeId 형식 및 파라미터 검증 심화

**VolumeId 형식:** `<target-name>/<protocol-type>/<backend-type>/<agent-vol-id>`

ZFS zvol의 `agent-vol-id`는 `<zfs-pool>/<volume-name>` 형식이므로 전체 VolumeId에
슬래시가 5개 포함될 수 있다. `strings.SplitN(id, "/", 4)` 로 정확히 4 파트로 분리한다.

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E1.11-1 | `TestCSIController_CreateVolume_VolumeID_ZFSPoolWithSlash` | ZFS pool 이름에 슬래시 포함 시 agent-vol-id 파싱 정확성 | zfs-pool="tank"; volume-name="pvc-abc" | 1) CreateVolumeRequest 전송; 2) 반환된 VolumeId로 DeleteVolumeRequest 전송 | CreateVolume: VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc"; DeleteVolume: agent-vol-id="tank/pvc-abc" 정확 파싱 | `CSI-C`, `Agent`, `gRPC` |
| E1.11-2 | `TestCSIController_CreateVolume_VolumeID_ZFSParentDataset` | ZFS parent dataset 파라미터 설정 시 agent-vol-id에 반영 | zfs-pool="tank"; zfs-parent-dataset="volumes"; volume-name="pvc-abc" | 1) CreateVolumeRequest 전송 | agent-vol-id="tank/volumes/pvc-abc"; VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/volumes/pvc-abc" | `CSI-C`, `Agent`, `gRPC` |
| E1.11-3 | `TestCSIController_CreateVolume_MissingVolumeName` | 볼륨 이름이 빈 문자열이면 InvalidArgument | ControllerServer 초기화만 필요 | 1) Name=""로 CreateVolumeRequest 전송 | gRPC InvalidArgument; "volume name is required" 메시지; agent 호출 없음 | `CSI-C` |
| E1.11-4 | `TestCSIController_CreateVolume_MissingTargetParam` | StorageClass parameter에 target 키 없으면 InvalidArgument | ControllerServer 초기화; Parameters에서 `pillar-csi.bhyoo.com/target` 제거 | 1) target 파라미터 없는 CreateVolumeRequest 전송 | gRPC InvalidArgument; "parameter … is required" 메시지 | `CSI-C` |
| E1.11-5 | `TestCSIController_CreateVolume_MissingBackendTypeParam` | StorageClass parameter에 backend-type 키 없으면 InvalidArgument | ControllerServer 초기화; Parameters에서 `pillar-csi.bhyoo.com/backend-type` 제거 | 1) backend-type 파라미터 없는 CreateVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |
| E1.11-6 | `TestCSIController_CreateVolume_MissingProtocolTypeParam` | StorageClass parameter에 protocol-type 키 없으면 InvalidArgument | ControllerServer 초기화; Parameters에서 `pillar-csi.bhyoo.com/protocol-type` 제거 | 1) protocol-type 파라미터 없는 CreateVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |

---

## E2: CSI Controller — ControllerPublish / ControllerUnpublish / ControllerExpandVolume

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

### 컨트롤러 측 어태치(Attachment) 단계 개요

`ControllerPublishVolume` / `ControllerUnpublishVolume`은 **CSI CO(external-attacher)가
스토리지 볼륨을 특정 노드에 어태치(attach)/언어태치(detach)하는 단계**이다.
pillar-csi의 어태치 메커니즘은 NVMe-oF 이니시에이터 ACL(Access Control List) 기반이다:

```
CO (external-attacher)
    │
    ▼  ControllerPublishVolume(VolumeId, NodeId, VolumeCapability)
    │
CSI ControllerServer
    │  1. VolumeId 파싱: "target/protocol/backend/agent-vol-id"
    │  2. PillarTarget CRD에서 agent 주소(ResolvedAddress) 조회
    │  3. NodeId를 InitiatorID(Host NQN)로 사용
    ▼
agent.AllowInitiator(VolumeID=agent-vol-id, InitiatorID=NodeId, ProtocolType)
    │
    ▼
pillar-agent (스토리지 노드)
    └── NVMe-oF configfs에 이니시에이터 ACL 항목 추가
        → 해당 NodeId(NQN) 노드만 볼륨에 물리적으로 접근 가능
```

**언어태치 흐름:** `ControllerUnpublishVolume` → `agent.DenyInitiator(VolumeID, InitiatorID)` →
configfs에서 이니시에이터 항목 제거

**CI 실행 가능성:** 실제 NVMe-oF configfs 없이, mockAgentServer(실제 gRPC 리스너 +
인메모리 ACL 추적)로 모든 경로 검증 가능.

---

### E2.1 ControllerPublishVolume — 정상 경로

> **테스트 파일:** `test/e2e/csi_controller_e2e_test.go` (E2E) + `test/component/csi_controller_test.go` (컴포넌트)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 14 | `TestCSIController_ControllerPublishVolume` | ControllerPublishVolume이 agent.AllowInitiator를 올바른 파라미터로 호출; NodeId가 InitiatorID로 정확히 전달됨 | PillarTarget="storage-1" fake 클라이언트에 등록; mockAgentServer(실제 gRPC 리스너) 정상; VolumeId=`storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-publish-test`; NodeId=`nqn.2014-08.org.nvmexpress:uuid:worker-1` | 1) ControllerPublishVolumeRequest 전송 | 성공; non-nil PublishContext 반환; AllowInitiator 1회; AllowInitiator.VolumeID=`tank/pvc-publish-test`; AllowInitiator.InitiatorID=NodeId; AllowInitiator.ProtocolType=NVMEOF_TCP | `CSI-C`, `Agent`, `gRPC` |
| E2.1-2 | `TestCSIController_ControllerPublishVolume_Success` | ControllerPublishVolume 정상 경로 (컴포넌트 테스트; in-process 모의 agent) | `test/component/csi_controller_test.go`; `csiMockAgent`(in-process); PillarTarget fake 등록; VolumeId=`pool/nvmeof-tcp/zfs-zvol/tank/vol` | 1) ControllerPublishVolumeRequest 전송 | 성공; allowInitiatorCalls==1; PublishContext 반환 | `CSI-C`, `Agent` |
| 15 | `TestCSIController_ControllerPublishVolume_Idempotency` | 동일 파라미터로 두 번 호출 — CSI 계층은 agent 중복 억제 없이 각 호출을 agent에 전달 | 유효한 VolumeId/NodeId; mockAgentServer 정상 | 1) ControllerPublishVolumeRequest 전송; 2) 동일 인수로 재전송 | 두 호출 모두 성공; PublishContext 동일; AllowInitiator 각 1회씩 총 2회 | `CSI-C`, `Agent`, `gRPC` |
| E2.1-4 | `TestCSIController_ControllerPublishVolume_AlreadyPublished` | 이미 Publish된 볼륨·노드 조합으로 재호출 성공 (컴포넌트 테스트) | `test/component/csi_controller_test.go`; allowInitiatorFn=nil(항상 성공) | 1) ControllerPublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; agent.AllowInitiator 총 2회 (CSI 계층은 억제 없음; 멱등성은 agent 책임) | `CSI-C`, `Agent` |

---

### E2.2 ControllerUnpublishVolume — 정상 경로 및 오류

> **테스트 파일:** `test/e2e/csi_controller_e2e_test.go` (E2E) + `test/component/csi_controller_test.go` + `test/component/csi_controller_extended_test.go` (컴포넌트)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 17 | `TestCSIController_ControllerUnpublishVolume` | ControllerUnpublishVolume이 agent.DenyInitiator를 NodeId로 정확히 호출; DenyInitiator.VolumeID=agent볼륨ID; DenyInitiator.InitiatorID=NodeId | mockAgentServer 정상; VolumeId=`storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-unpublish-test`; NodeId=`nqn.2014-08.org.nvmexpress:uuid:worker-1` | 1) ControllerUnpublishVolumeRequest 전송 | 성공; DenyInitiator 1회; DenyInitiator.VolumeID=`tank/pvc-unpublish-test`; DenyInitiator.InitiatorID=NodeId; DenyInitiator.ProtocolType=NVMEOF_TCP | `CSI-C`, `Agent`, `gRPC` |
| E2.2-2 | `TestCSIController_ControllerUnpublishVolume_Success` | ControllerUnpublishVolume 정상 경로 (컴포넌트 테스트) | `test/component/csi_controller_test.go`; `csiMockAgent`; denyInitiatorFn=nil | 1) ControllerUnpublishVolumeRequest 전송 | 성공; denyInitiatorCalls==1 | `CSI-C`, `Agent` |
| 18 | `TestCSIController_ControllerUnpublishVolume_NotFoundIsIdempotent` | agent.DenyInitiator가 NotFound 반환 시 Unpublish는 성공으로 처리 (CSI 명세 §4.3.4: NotFound = 이미 접근 제거됨) | mockAgentServer.DenyInitiatorErr = gRPC NotFound | 1) ControllerUnpublishVolumeRequest 전송 | 성공; gRPC OK 반환; CSI 호출자에게 오류 없음 | `CSI-C`, `Agent`, `gRPC` |
| E2.2-4 | `TestCSIController_ControllerUnpublishVolume_AlreadyUnpublished` | 이미 Unpublish된 볼륨에 재호출 성공 (컴포넌트 테스트) | `test/component/csi_controller_test.go`; denyInitiatorFn=nil | 1) ControllerUnpublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; DenyInitiator 총 2회 | `CSI-C`, `Agent` |
| E2.2-5 | `TestCSIController_ControllerUnpublishVolume_EmptyVolumeID` | VolumeId=""이면 InvalidArgument 반환; DenyInitiator 0회 (컴포넌트 테스트) | `test/component/csi_controller_extended_test.go`; VolumeId="" | 1) VolumeId=""로 ControllerUnpublishVolumeRequest 전송 | gRPC InvalidArgument; DenyInitiator 0회 | `CSI-C` |
| E2.2-6 | `TestCSIController_ControllerUnpublishVolume_EmptyNodeID` | NodeId=""이면 성공 + no-op (CSI 명세 §4.3.4: 빈 NodeId = "모든 노드에서 Unpublish"; pillar-csi는 no-op) | `test/component/csi_controller_extended_test.go`; 유효한 VolumeId; NodeId="" | 1) NodeId=""로 ControllerUnpublishVolumeRequest 전송 | 성공; DenyInitiator 0회 (no-op 처리) | `CSI-C` |
| E2.2-7 | `TestCSIController_ControllerUnpublishVolume_MalformedVolumeID` | VolumeId="badformat"(슬래시 없음)이면 성공 반환 (컴포넌트 테스트; CSI 명세상 Unpublish malformed ID는 성공 no-op 허용) | `test/component/csi_controller_extended_test.go`; VolumeId="badformat" | 1) VolumeId="badformat"로 전송 | 성공; DenyInitiator 0회 | `CSI-C` |
| E2.2-8 | `TestCSIErrors_ControllerUnpublish_DenyInitiatorNonNotFound` | agent.DenyInitiator가 Internal 오류 반환 시 ControllerUnpublishVolume이 비-OK gRPC 상태 전파 (NotFound만 성공 처리) | `test/component/csi_controller_extended_test.go`; denyInitiatorFn = gRPC Internal("deny initiator failed") | 1) ControllerUnpublishVolumeRequest 전송 | 비-OK gRPC 상태; 오류 은폐 없음 | `CSI-C`, `Agent` |

---

### E2.3 ControllerExpandVolume

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 19 | `TestCSIController_ControllerExpandVolume` | ControllerExpandVolume이 agent.ExpandVolume을 올바른 새 용량으로 호출 | 유효한 VolumeId; mockAgentServer 정상 동작 | 1) CapacityRange.RequiredBytes=2GiB 로 ControllerExpandVolumeRequest 전송 | 성공; CapacityBytes=2GiB 반환; agent.ExpandVolume 1회 호출 | `CSI-C`, `Agent`, `gRPC` |
| 20 | `TestCSIController_ControllerExpandVolume_MissingCapacityRange` | CapacityRange 없으면 InvalidArgument | 유효한 VolumeId; CapacityRange=nil | 1) CapacityRange 없는 ControllerExpandVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |

---

### E2.4 ValidateVolumeCapabilities

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 21 | `TestCSIController_ValidateVolumeCapabilities` | 지원 가능한 접근 모드(SINGLE_NODE_WRITER 등)는 확인됨, 지원 불가 모드(MULTI_NODE_MULTI_WRITER 등)는 거부됨 | ControllerServer 초기화 | 1) AccessMode=SINGLE_NODE_WRITER 로 요청; 2) AccessMode=MULTI_NODE_MULTI_WRITER 로 요청 | 지원 모드: 빈 메시지 반환; 비지원 모드: 메시지 필드에 이유 포함 | `CSI-C` |

---

### E2.5 노드 친화성 (Node Affinity)

pillar-csi의 "노드 친화성"은 NVMe-oF 이니시에이터 ACL(Access Control List)을 통해 구현된다.
Kubernetes CO가 `ControllerPublishVolume(NodeId=<Host NQN>)`을 호출하면, pillar-csi는
이를 `agent.AllowInitiator(InitiatorID=<Host NQN>)`으로 변환하여 스토리지 노드의 configfs에
이니시에이터 허용 항목을 추가한다. 그 결과 해당 NQN을 가진 노드만 그 볼륨에 접근할 수 있다.

**노드 친화성 아키텍처:**
```
K8s 워커 노드 (Host NQN: nqn.2014-08.org.nvmexpress:uuid:<uuid>)
    │
    ▼ ControllerPublishVolume(NodeId = Host NQN)
CSI ControllerServer
    │  NodeId → InitiatorID 변환 (1:1 매핑)
    ▼
agent.AllowInitiator(VolumeID, InitiatorID=Host NQN, ProtocolType)
    │
    ▼
pillar-agent → configfs → NVMe-oF 이니시에이터 ACL 추가
    └── 해당 NQN 노드만 볼륨 접근 가능 (물리적 어태치)
```

**CSI 명세와의 정합성:**
- `SINGLE_NODE_WRITER`: 한 노드에만 Publish → 1개 AllowInitiator 항목
- `MULTI_NODE_READER_ONLY`: 여러 노드에 각각 Publish 호출 → N개 AllowInitiator 항목 (독립 호출)
- Kubernetes Node NQN 형식: `nqn.2014-08.org.nvmexpress:uuid:<UUID>`

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요 (mockAgentServer 기반)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E2.5-1 | `TestCSIController_ControllerPublishVolume` | NodeId(NQN 형식)가 agent.AllowInitiator의 InitiatorID로 1:1 매핑; VolumeId에서 agent 볼륨 ID(`tank/pvc-publish-test`) 파싱; ProtocolType=NVMEOF_TCP | VolumeId=`storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-publish-test`; NodeId=`nqn.2014-08.org.nvmexpress:uuid:worker-1`; PillarTarget 등록; mockAgentServer | 1) ControllerPublishVolumeRequest 전송; 2) AllowInitiator 호출 내용 검사 | AllowInitiator.InitiatorID == NodeId; AllowInitiator.VolumeID==`tank/pvc-publish-test`; AllowInitiator.ProtocolType==NVMEOF_TCP | `CSI-C`, `Agent`, `gRPC` |
| E2.5-2 | `TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes` | 동일 볼륨에 대해 2개의 서로 다른 노드(NQN-A, NQN-B)가 각각 Publish → 2개의 독립 AllowInitiator 항목 생성 (다중 이니시에이터 지원) | VolumeId 동일; NodeId1=`nqn.2014-08.org.nvmexpress:uuid:worker-node-a`; NodeId2=`nqn.2014-08.org.nvmexpress:uuid:worker-node-b`; mockAgentServer | 1) ControllerPublishVolume(NodeId1); 2) ControllerPublishVolume(NodeId2) | AllowInitiator 총 2회; AllowInitiator[0].InitiatorID ≠ AllowInitiator[1].InitiatorID; 두 호출 모두 성공 | `CSI-C`, `Agent`, `gRPC` |
| E2.5-3 | `TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs` | 동일 노드에 대해 ControllerPublishVolume 2회 호출 — CSI 계층은 중복 억제 없이 각각 agent에 전달; 멱등성은 agent 책임 | VolumeId/NodeId/VolumeCapability 동일; mockAgentServer | 1) ControllerPublishVolume(args); 2) 동일 인수로 재호출 | AllowInitiator 총 2회; PublishContext 동일(응답 일관성); CreateVolume/ExportVolume 0회(Publish는 볼륨 생성 트리거 없음) | `CSI-C`, `Agent`, `gRPC` |

> ℹ️ E2.5-2, E2.5-3은 [E7: 게시 멱등성](#e7-게시-멱등성-publish-idempotency) 섹션과 동일한 테스트 함수를 다른 관점에서 서술한다.
> E7에서는 **멱등성 계약(no-op 보장, 응답 일관성)** 을 검증하고, E2.5에서는 **노드 친화성 매핑(NodeId→InitiatorID)**을 검증한다.

---

### E2.6 오류 처리 — 게시 단계 (Error Handling — Attachment Phase)

컨트롤러 측 어태치/언어태치 오류의 전파 경로를 검증한다.
CSI 명세에 따라:
- `ControllerPublishVolume`: 비-OK 오류는 그대로 반환 (오류 은폐 금지)
- `ControllerUnpublishVolume`: `NotFound` 이외의 오류만 전파; `NotFound`는 성공으로 처리 (§4.3.4)

**검증 불가 범위(CI):** 실제 configfs 쓰기 실패(permission denied, read-only fs)는 CI에서
재현 불가 — mock agent에서 오류 코드 주입으로 오류 전파 경로만 검증

> **CI 실행 가능 여부:** ✅ 컴포넌트 테스트 (`test/component/`) — 실제 커널 모듈 불필요

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E2.6-1 | `TestCSIErrors_ControllerPublish_AllowInitiatorFails` | agent.AllowInitiator가 Internal 오류 반환 시 ControllerPublishVolume이 비-OK gRPC 상태 반환; 오류 은폐 없음 (실제로는 configfs ACL 쓰기 실패 시 발생) | `test/component/csi_errors_test.go`; allowInitiatorFn=gRPC Internal("configfs write failed: permission denied") | 1) ControllerPublishVolumeRequest 전송 | 비-OK gRPC 상태(Internal); 오류 메시지 포함; 성공 은폐 없음 | `CSI-C`, `Agent` |
| E2.6-2 | `TestCSIErrors_ControllerUnpublish_DenyInitiatorNonNotFound` | agent.DenyInitiator가 Internal 오류 반환 시 ControllerUnpublishVolume이 비-OK gRPC 상태 반환 (NotFound만 성공; 그 외 오류는 전파) | `test/component/csi_controller_extended_test.go`; denyInitiatorFn=gRPC Internal("deny initiator failed: internal error") | 1) ControllerUnpublishVolumeRequest 전송 | 비-OK gRPC 상태; 오류 은폐 없음 (CSI 명세 §4.3.4 준수) | `CSI-C`, `Agent` |
| E2.6-3 | `TestCSIController_ControllerPublishVolume_EmptyVolumeID` | VolumeId="" — agent 호출 전 입력 검증 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; VolumeId=""; 유효한 NodeId/VolumeCapability | 1) VolumeId=""로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument; AllowInitiator 0회 | `CSI-C` |
| E2.6-4 | `TestCSIController_ControllerPublishVolume_EmptyNodeID` | NodeId="" — agent 호출 전 입력 검증 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; 유효한 VolumeId; NodeId="" | 1) NodeId=""로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument; AllowInitiator 0회 | `CSI-C` |
| E2.6-5 | `TestCSIController_ControllerPublishVolume_NilVolumeCapability` | VolumeCapability=nil — 입력 검증 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; VolumeCapability=nil | 1) VolumeCapability=nil로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |
| E2.6-6 | `TestCSIController_ControllerPublishVolume_MalformedVolumeID` | VolumeId="badformat"(슬래시 없음) — VolumeId 파싱 실패 → InvalidArgument | `test/component/csi_controller_extended_test.go`; VolumeId="badformat" | 1) VolumeId="badformat"로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument | `CSI-C` |
| E2.6-7 | `TestCSIController_ControllerPublishVolume_TargetNotFound` | VolumeId의 target 이름이 PillarTarget CRD에 없음 → NotFound; agent 호출 전 실패 | `test/component/csi_controller_extended_test.go`; fake 클라이언트에 PillarTarget 미등록; VolumeId=`nonexistent-node/nvmeof-tcp/zfs-zvol/tank/pvc-test` | 1) ControllerPublishVolumeRequest 전송 | gRPC NotFound; AllowInitiator 0회 | `CSI-C`, `TgtCRD` |
| E2.6-8 | `TestCSIController_ControllerPublishVolume_TargetNoResolvedAddress` | PillarTarget.Status.ResolvedAddress=""이면 Unavailable 반환; agent 다이얼 미시도 | `test/component/csi_controller_extended_test.go`; PillarTarget 등록; Status.ResolvedAddress="" | 1) ControllerPublishVolumeRequest 전송 | gRPC Unavailable; AllowInitiator 0회; "no resolved address" 메시지 | `CSI-C`, `TgtCRD` |

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

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 22 | `TestCSINode_FullRoundTrip_MountAccess` | NodeStageVolume → NodePublishVolume → NodeUnpublishVolume → NodeUnstageVolume 전체 마운트 접근 라이프사이클 | mockConnector.DevicePath="/dev/nvme0n1"; mockMounter 초기화; VolumeContext에 NQN/address/port 설정; 접근 모드 MOUNT | 1) NodeStageVolume; 2) NodePublishVolume; 3) NodeUnpublishVolume; 4) NodeUnstageVolume | 모든 단계 성공; Connector.Connect 1회, FormatAndMount 1회, 바인드 마운트 1회, 언마운트 2회, Disconnect 1회 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E3.2 전체 왕복 — 블록 접근 모드

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 23 | `TestCSINode_FullRoundTrip_BlockAccess` | 블록 디바이스 접근 모드 전체 라이프사이클 | mockConnector.DevicePath="/dev/nvme0n1"; 접근 모드 BLOCK | 1) NodeStageVolume; 2) NodePublishVolume; 3) NodeUnpublishVolume; 4) NodeUnstageVolume | 성공; 포맷/파일시스템 마운트 없이 디바이스 직접 노출 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E3.3 디바이스 디스커버리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 24 | `TestCSINode_DeviceDiscovery` | NodeStage 시 NVMe-oF 연결 후 디바이스 경로 탐색 | mockConnector: GetDevicePath가 처음 몇 번 ""를 반환하다가 "/dev/nvme0n1" 반환 (폴링 시뮬레이션) | 1) NodeStageVolumeRequest 전송 | 성공; 디바이스 경로가 스테이징 상태에 저장됨 | `CSI-N`, `Conn`, `State` |

---

### E3.4 멱등성 (Idempotency)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 25 | `TestCSINode_IdempotentStage` | NodeStageVolume 2회 호출: 두 번째는 no-op | mockConnector.DevicePath 설정; 상태 파일 없음 | 1) NodeStageVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; Connector.Connect 재호출 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 26 | `TestCSINode_IdempotentPublish` | NodePublishVolume 2회 호출: 두 번째는 no-op | NodeStage 성공; NodePublish 1회 완료 | 1) NodePublishVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; 중복 마운트 없음 | `CSI-N`, `Mnt` |
| 27 | `TestCSINode_IdempotentUnstage` | NodeUnstageVolume 2회 호출: 두 번째는 no-op | NodeStage 성공; NodeUnstage 1회 완료 | 1) NodeUnstageVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; 이중 언마운트/연결 해제 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 28 | `TestCSINode_IdempotentUnpublish` | NodeUnpublishVolume 2회 호출: 두 번째는 no-op | NodePublish 성공; NodeUnpublish 1회 완료 | 1) NodeUnpublishVolume 호출; 2) 동일 파라미터로 재호출 | 두 번째 호출 성공; 오류 없음 | `CSI-N`, `Mnt` |

---

### E3.5 읽기 전용 마운트

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 29 | `TestCSINode_ReadonlyPublish` | NodePublishVolume에서 readonly=true 플래그가 마운터에 전달됨 | NodeStage 성공; mockMounter 초기화 | 1) NodeStageVolume; 2) Readonly=true; 접근 모드 MOUNT 로 NodePublishVolume 전송 | 성공; mockMounter가 readonly 옵션으로 호출됨 | `CSI-N`, `Mnt` |

---

### E3.6 상태 파일 영속성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 30 | `TestCSINode_StateFilePersistence` | NodeStage 후 스테이징 상태 파일이 StateDir에 저장되고, 이후 NodeUnstage 시 제거됨 | mockConnector.DevicePath 설정; t.TempDir()를 StateDir로 사용 | 1) NodeStageVolume 호출; 2) StateDir 파일 존재 확인; 3) NodeUnstageVolume 호출; 4) StateDir 파일 제거 확인 | 상태 파일 생성/삭제 타이밍이 CSI 호출과 일치 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E3.7 노드 정보 및 역량

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 31 | `TestCSINode_NodeGetInfo` | NodeGetInfo가 올바른 NodeId와 토폴로지 키 반환 | NodeServer 초기화: nodeID="worker-1" | 1) NodeGetInfoRequest 전송 | NodeId="worker-1"; 토폴로지 키 존재 | `CSI-N` |
| 32 | `TestCSINode_NodeGetCapabilities` | NodeGetCapabilities가 지원 역량 목록 반환 | NodeServer 기본 초기화 | 1) NodeGetCapabilitiesRequest 전송 | STAGE_UNSTAGE_VOLUME 포함; 비어있지 않은 역량 목록 | `CSI-N` |

---

### E3.8 유효성 검사 오류

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 33 | `TestCSINode_ValidationErrors` | 필수 파라미터 누락 시 InvalidArgument 반환 | NodeServer 초기화 | 1) VolumeId="" 로 전송; 2) VolumeContext 키 누락 요청; 3) StagingTargetPath="" 요청 | 각 케이스에서 gRPC InvalidArgument; 커넥터/마운터 호출 없음 | `CSI-N` |

---

### E3.9 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 34 | `TestCSINode_ConnectError` | NVMe-oF 연결 실패 시 NodeStage 실패 | mockConnector.ConnectErr 설정 | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; 마운트 미실행; 상태 파일 미생성 | `CSI-N`, `Conn` |
| 35 | `TestCSINode_DisconnectError` | NVMe-oF 연결 해제 실패 시 NodeUnstage 실패 | NodeStage 성공; mockConnector.DisconnectErr 설정 | 1) NodeUnstageVolumeRequest 전송 | 비-OK gRPC 상태 | `CSI-N`, `Conn`, `State` |
| 36 | `TestCSINode_MountError` | 파일시스템 마운트 실패 시 NodeStage 실패 | Connect 성공; mockMounter.FormatAndMountErr 설정 | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; Disconnect 롤백 호출 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 37 | `TestCSINode_PublishMountError` | 바인드 마운트 실패 시 NodePublish 실패 | NodeStage 성공; mockMounter.BindMountErr 설정 | 1) NodePublishVolumeRequest 전송 | 비-OK gRPC 상태 | `CSI-N`, `Mnt` |

---

### E3.10 다중 볼륨 동시 처리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 38 | `TestCSINode_MultipleVolumes` | 여러 볼륨이 독립적인 스테이징 상태를 유지 | mockConnector.DevicePath 설정; 3개 별도 StagingTargetPath 준비 | 1) 볼륨A NodeStage; 2) 볼륨B NodeStage; 3) 볼륨C NodeStage | 모든 볼륨 성공적으로 스테이지; 상태 파일 간 충돌 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E3.11 NodeStageVolume — 파일시스템 타입별 동작

**설명:** `NodeStageVolume`은 `VolumeCapability.MountVolume.FsType` 필드를
`FormatAndMount`에 그대로 전달해야 한다. 다양한 파일시스템 타입에서 동일한
흐름이 동작하는지 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 72 | `TestCSINode_StageVolume_XFS` | `fsType=xfs`로 NodeStageVolume 호출 시 FormatAndMount에 "xfs" 전달됨 | mockConnector.DevicePath="/dev/nvme3n1"; mockMounter 초기화 | 1) mountVolumeCapability("xfs", SINGLE_NODE_WRITER) 로 NodeStageVolumeRequest 전송 | FormatAndMount 1회; FsType="xfs"; 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 73 | `TestCSINode_StageVolume_DefaultFilesystem` | `fsType=""` (빈 문자열) 시 기본 파일시스템(ext4)으로 포맷 | mockConnector.DevicePath 설정 | 1) mountVolumeCapability("", SINGLE_NODE_WRITER) 로 NodeStageVolumeRequest 전송 | FormatAndMount 1회; FsType="" 또는 "ext4"; 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 74 | `TestCSINode_StageVolume_BlockAccessNoFormatAndMount` | 블록 접근 모드에서 FormatAndMount가 호출되지 않음 | mockConnector.DevicePath 설정 | 1) blockVolumeCapability(SINGLE_NODE_WRITER) 로 NodeStageVolumeRequest 전송 | FormatAndMount 0회; 블록 디바이스 직접 노출; 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E3.12 NodeStageVolume — NVMe-oF 어태치(Attach) 파라미터 상세 검증

**설명:** CSI Controller가 생성한 VolumeContext(NQN, address, port)가
NodeStageVolume에서 NVMe-oF Connect 호출에 정확하게 전달되는지 검증한다.
이 섹션은 **어태치(NVMe-oF connect) 경계**를 집중적으로 테스트한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 75 | `TestCSINode_StageVolume_ConnectParamsForwarded` | VolumeContext의 NQN/address/port가 Connector.Connect 호출 시 정확히 전달됨 | mockConnector.DevicePath 설정; VolumeContext: target_id="nqn.test", address="192.168.0.10", port="4420" | 1) NodeStageVolumeRequest 전송; 2) mockConnector.ConnectCalls 검사 | Connector.Connect: SubsysNQN="nqn.test", TrAddr="192.168.0.10", TrSvcID="4420" | `CSI-N`, `Conn`, `State` |
| 76 | `TestCSINode_StageVolume_CustomPort` | 비표준 포트(4421)도 정확히 전달됨 | mockConnector.DevicePath 설정; VolumeContext.port="4421" | 1) NodeStageVolumeRequest 전송; 2) Connect 인수 검사 | Connector.Connect: TrSvcID="4421" | `CSI-N`, `Conn`, `State` |
| 77 | `TestCSINode_StageVolume_MissingAddress` | VolumeContext에서 address 키 누락 시 InvalidArgument | VolumeContext에 target_id와 port만 있고 address 키 없음 | 1) NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connect 미호출 | `CSI-N` |
| 78 | `TestCSINode_StageVolume_MissingPort` | VolumeContext에서 port 키 누락 시 InvalidArgument | VolumeContext에 target_id와 address만 있고 port 키 없음 | 1) NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connect 미호출 | `CSI-N` |
| 79 | `TestCSINode_StageVolume_AttachThenStateSaved` | NVMe-oF 연결 성공 후 상태 파일에 NQN이 저장됨 (재시작 복구 지원) | mockConnector.DevicePath 설정; 유효한 VolumeContext | 1) NodeStageVolumeRequest 전송; 2) StateDir/*.json 내용 검증 | 상태 파일 생성; NQN 포함; Connect 1회 | `CSI-N`, `Conn`, `State` |

---

### E3.13 NodeUnstageVolume — 디태치(Detach) 시나리오 상세

**설명:** NodeUnstageVolume은 NVMe-oF 연결을 해제(디태치)하고 상태 파일을
제거해야 한다. 다양한 비정상 상황에서도 올바르게 동작해야 한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 80 | `TestCSINode_UnstageVolume_DetachCallsDisconnect` | NodeUnstageVolume이 Connector.Disconnect를 정확한 NQN으로 호출 | NodeStageVolume 성공; 상태 파일에 NQN 기록됨 | 1) NodeStageVolume; 2) NodeUnstageVolumeRequest 전송; 3) Disconnect 인수 검증 | Disconnect 1회; NQN이 Stage 시 사용한 값과 동일 | `CSI-N`, `Conn`, `State` |
| 81 | `TestCSINode_UnstageVolume_NeverStagedIsIdempotent` | 스테이지된 적 없는 볼륨에 NodeUnstageVolume 호출 시 성공 (멱등성) | StateDir에 해당 VolumeId 상태 파일 없음 | 1) NodeUnstageVolumeRequest 직접 전송 | 성공; Disconnect 0회; Unmount 0회 | `CSI-N`, `State` |
| 82 | `TestCSINode_UnstageVolume_DetachFailsOnDisconnectError` | Connector.Disconnect 실패 시 gRPC Internal 반환 | NodeStage 성공; mockConnector.DisconnectErr 주입 | 1) NodeUnstageVolumeRequest 전송 | gRPC Internal; 상태 파일 미제거 | `CSI-N`, `Conn`, `State` |
| 83 | `TestCSINode_UnstageVolume_StateFileRemovedAfterSuccessfulDetach` | 정상 디태치 후 상태 파일 제거 확인 | NodeStage 성공; 상태 파일 존재 | 1) NodeUnstageVolumeRequest 전송; 2) StateDir 파일 수 확인 | NodeUnstage 성공 후 StateDir에 *.json 파일 0개 | `CSI-N`, `Conn`, `State` |

---

### E3.14 NodePublishVolume — 다중 타깃 마운트

**설명:** 하나의 스테이지된 볼륨(하나의 NVMe-oF 연결)에서 여러 컨테이너 타깃
경로로 바인드 마운트를 생성하는 시나리오를 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 84 | `TestCSINode_PublishVolume_MultipleTargets` | 동일 스테이징 경로에서 두 타깃 경로로 NodePublishVolume 각각 성공 | NodeStage 1회 완료; 두 개의 서로 다른 targetPath 준비 | 1) NodePublishVolume(targetPath1); 2) NodePublishVolume(targetPath2) | 두 Mount 호출 모두 성공; source는 동일 stagingPath; target은 각각 다름 | `CSI-N`, `Mnt` |
| 85 | `TestCSINode_PublishVolume_UnpublishOneKeepsOther` | 두 타깃 중 하나 NodeUnpublish 시 나머지 마운트는 유지됨 | NodeStage + NodePublish×2 완료 | 1) NodeUnpublishVolume(targetPath1); 2) targetPath2 마운트 상태 검증 | Unmount 1회; 남은 타깃 경로 마운트 유지; 스테이징 경로도 유지 | `CSI-N`, `Mnt` |

---

### E3.15 NodePublishVolume — 접근 모드(Access Mode)별 동작

**설명:** CSI 명세상 접근 모드(AccessMode)에 따라 마운트 옵션이 달라져야 한다.
SINGLE_NODE_READER_ONLY와 MULTI_NODE_READER_ONLY는 읽기 전용 마운트여야 한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 86 | `TestCSINode_PublishVolume_SingleNodeWriter` | SINGLE_NODE_WRITER 접근 모드에서 쓰기 가능 마운트 | NodeStage 성공; mockMounter 초기화 | 1) AccessMode=SINGLE_NODE_WRITER; Readonly=false 로 NodePublishVolumeRequest 전송 | 성공; 마운트 옵션에 "ro" 없음 | `CSI-N`, `Mnt` |
| 87 | `TestCSINode_PublishVolume_SingleNodeReaderOnly` | SINGLE_NODE_READER_ONLY 접근 모드에서 읽기 전용 마운트 | NodeStage 성공; mockMounter 초기화 | 1) AccessMode=SINGLE_NODE_READER_ONLY; Readonly=true 로 NodePublishVolumeRequest 전송 | 성공; 마운트 옵션에 "ro" 포함 | `CSI-N`, `Mnt` |

---

### E3.16 NodeStageVolume — 디바이스 대기 및 GetDevicePath 유효성 검사

**설명:** NodeStageVolume은 NVMe-oF Connect 성공 후 커널이 `/dev/nvme*` 블록 디바이스를
생성할 때까지 `GetDevicePath`를 폴링한다. 이 섹션은 폴링 루프의 타임아웃 동작 및
`GetDevicePath` 오류 처리를 집중 검증한다.

**소스 파일:**
- `internal/csi/node_stage_test.go` — `TestNodeStageVolume_DeviceNeverAppears`, `TestNodeStageVolume_DevicePathError`
- `test/component/csi_node_test.go` — `TestCSINode_NodeStageVolume_DeviceTimeout`, `TestCSINode_NodeStageVolume_GetDevicePathError`

**CI 실행 가능 여부:** ✅ 가능 (실제 블록 디바이스 불필요; 짧은 컨텍스트 타임아웃으로 폴링 루프 가속)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 171 | `TestNodeStageVolume_DeviceNeverAppears` | GetDevicePath가 항상 `("", nil)`을 반환하면 폴링 루프가 컨텍스트 타임아웃으로 종료 | `mockConnector.devicePath=""`; 컨텍스트 타임아웃 = `devicePollInterval * 3` (≈1.5 s) | 1) NodeStageVolumeRequest 전송 | gRPC `DeadlineExceeded` 또는 `Internal` (컨텍스트 취소 경쟁); `FormatAndMount` 0회 호출; 패닉 없음 | `CSI-N`, `Conn` |
| 172 | `TestCSINode_NodeStageVolume_DeviceTimeout` | 200 ms 타임아웃으로 디바이스 폴링 루프 타임아웃 검증 | `mockConnector.getDeviceFn` 항상 `("", nil)` 반환; `ctx` 200 ms 타임아웃 | 1) NodeStageVolumeRequest 전송 | gRPC `DeadlineExceeded`; `Mounter.FormatAndMount` 0회 | `CSI-N`, `Conn` |
| 173 | `TestNodeStageVolume_DevicePathError` | GetDevicePath가 오류(`"sysfs error"`)를 반환 → 폴링 중단 후 Internal | `mockConnector.devicePathErr=errors.New("sysfs error")` | 1) NodeStageVolumeRequest 전송 | gRPC `Internal`; `FormatAndMount` 0회; `Connect` 1회 | `CSI-N`, `Conn` |
| 174 | `TestCSINode_NodeStageVolume_GetDevicePathError` | GetDevicePath 오류 → NodeStage `Internal` (컴포넌트 패키지) | `mockConnector.getDeviceFn`이 `errors.New("permission denied")` 반환 | 1) NodeStageVolumeRequest 전송 | gRPC `Internal`; `Mounter.FormatAndMount` 미호출 | `CSI-N`, `Conn` |

---

### E3.17 NodeStageVolume — 기본 파일시스템 타입 및 접근 유형 검증

**설명:** `VolumeCapability.MountVolume.FsType`이 빈 문자열일 때 구현이 기본값(ext4)을
사용하는지, 그리고 `AccessType`이 전혀 설정되지 않은 경우 적절하게 거부하는지 검증한다.

**소스 파일:** `internal/csi/node_stage_test.go`

**CI 실행 가능 여부:** ✅ 가능

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 175 | `TestNodeStageVolume_DefaultFsType` | `FsType=""`(빈 문자열) 일 때 `FormatAndMount`에 기본 파일시스템 타입(`"ext4"`)이 전달됨 | `mockConnector.devicePath="/dev/nvme0n1"`; `mountCap("")` — FsType 필드 빈 문자열 | 1) NodeStageVolumeRequest 전송 | `FormatAndMount` 1회; `fsType = "ext4"` (= `defaultFsType` 상수); 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 176 | `TestNodeStageVolume_NoAccessType` | `VolumeCapability.AccessType` 필드 자체가 nil(Mount/Block 미설정) → `InvalidArgument` | `VolumeCapability{AccessMode=SINGLE_NODE_WRITER}` — AccessType 설정 없음 | 1) NodeStageVolumeRequest 전송 | gRPC `InvalidArgument`; `Connect` 미호출; `FormatAndMount` 미호출 | `CSI-N` |

---

### E3.18 NodeStageVolume — 재부팅 후 재스테이징 멱등성 (Re-stage after Unmount)

**설명:** 상태 파일은 존재하지만 스테이징 경로가 이미 언마운트된 상황(노드 재부팅 등)에서
NodeStageVolume을 재호출할 때의 동작을 검증한다. 이 시나리오는 `TestNodeStageVolume_Idempotent`
(이미 마운트된 경우의 no-op)와 다르다.

**소스 파일:**
- `internal/csi/node_stage_test.go` — `TestNodeStageVolume_IdempotentAfterUnmount`
- `test/component/csi_node_test.go` — `TestCSINode_NodeStage_Idempotent_StateFileExists`

**CI 실행 가능 여부:** ✅ 가능

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 177 | `TestNodeStageVolume_IdempotentAfterUnmount` | 상태 파일 있음 + 스테이징 경로 언마운트된 상태에서 재스테이징 → 재마운트 성공 | NodeStageVolume 1회 성공 후 `mounter.mountedPaths`에서 stagingPath 직접 제거(재부팅 시뮬레이션) | 1) 동일 요청으로 NodeStageVolumeRequest 재전송 | 성공; `Connect` 추가 호출 없음; `FormatAndMount` 2회 총 호출(초기 1회 + 재마운트 1회); stagingPath 마운트 상태로 복구 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 178 | `TestCSINode_NodeStage_Idempotent_StateFileExists` | 상태 파일 있음 + 경로 마운트된 상태에서 재스테이징 → `Connect` 재호출 없음 (CI 컴포넌트 테스트) | NodeStageVolume 1회 성공 (stateDir에 상태 파일 기록됨; mounter에 stagingPath 마운트됨) | 1) 동일 요청으로 NodeStageVolumeRequest 재전송; 2) `connectCalls` 수 비교 | 성공; `connectCalls` 증가 없음; `FormatAndMount` 재호출 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E3.19 NodeUnstageVolume — 오류 경로 및 예외 시나리오 심화

**설명:** NodeUnstageVolume의 오류 경로 — Unmount 실패, Disconnect 실패, 이미 언마운트된
경로, 두 번째 언스테이지 호출(멱등성) — 를 집중 검증한다.

**소스 파일:** `internal/csi/node_stage_test.go`

**CI 실행 가능 여부:** ✅ 가능

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 179 | `TestNodeUnstageVolume_UnmountedPath` | 스테이징 경로가 이미 언마운트된 상태(노드 재부팅 시뮬레이션) — NodeUnstageVolume 성공하며 Disconnect는 호출됨 | NodeStageVolume 성공 후 `mounter.mountedPaths`에서 stagingPath 직접 제거 | 1) NodeUnstageVolumeRequest 전송 | 성공; `Disconnect` 1회 (NQN 정확히 일치); `Unmount` 0회 또는 no-op; 상태 파일 제거 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 180 | `TestNodeUnstageVolume_UnmountError` | `Mounter.Unmount`가 `"device busy"` 오류 반환 → NodeUnstageVolume `Internal` | NodeStageVolume 성공; `mounter.unmountErr=errors.New("device busy")` 설정 | 1) NodeUnstageVolumeRequest 전송 | gRPC `Internal`; `Disconnect` 미호출(Unmount 실패 후 중단) 또는 구현별 가변 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 181 | `TestNodeUnstageVolume_DisconnectError` | `Connector.Disconnect`가 `"NVMe transport error"` 반환 → NodeUnstageVolume `Internal` | NodeStageVolume 성공; `connector.disconnectErr=errors.New("NVMe transport error")` 설정 | 1) NodeUnstageVolumeRequest 전송 | gRPC `Internal`; `Unmount` 호출 완료 후 `Disconnect` 실패 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 182 | `TestNodeUnstageVolume_IdempotentSecondCall` | Stage → Unstage → Unstage(2회) — 두 번째 Unstage는 no-op, Disconnect는 총 1회만 | NodeStageVolume + NodeUnstageVolume 각 1회 완료; 상태 파일 없음 | 1) NodeUnstageVolumeRequest 재전송 | 성공; `disconnectCalls` 여전히 1; 오류 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E3.20 스테이징 상태 파일 관리 (Stage State File Management)

**설명:** 스테이징 상태 파일(`/var/lib/pillar-csi/node/<safeID>.json`)의 생성, 읽기,
삭제 및 예외 케이스를 검증한다. 상태 파일은 NodeStageVolume이 쓰고 NodeUnstageVolume이
삭제하며, Kubelet 재시작 후에도 NQN 정보를 복구하는 핵심 메커니즘이다.

**소스 파일:**
- `internal/csi/node_stage_test.go` — `TestStageState_WriteReadDelete`, `TestStageState_DeleteIdempotent`, `TestStageState_VolumeIDSanitization`
- `test/component/csi_node_test.go` — `TestCSINode_NodeUnstage_CorruptStateFile`, `TestCSINode_NodeStage_StateDirUnwritable`, `TestCSINode_NodeUnstage_StateFileMissingIsOK`

**CI 실행 가능 여부:** ✅ 가능 (`TestCSINode_NodeStage_StateDirUnwritable`은 root로 실행 시 `t.Skip`)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 183 | `TestCSINode_NodeUnstage_CorruptStateFile` | stateDir에 유효하지 않은 JSON(`"not valid json {{{"`) 상태 파일 직접 기록 후 NodeUnstageVolume 호출 | `volumeID`에 대응하는 `stateDir/<safeID>.json`에 corrupt bytes 기록 | 1) NodeUnstageVolumeRequest 전송 | 비-OK gRPC 상태 반환; 패닉 없음; `Disconnect` 미호출 | `CSI-N`, `State` |
| 184 | `TestCSINode_NodeStage_StateDirUnwritable` | stateDir을 `0555`(읽기 전용)로 변경 후 NodeStageVolume — 상태 파일 쓰기 실패 | `os.Chmod(stateDir, 0o555)`; root가 아닌 사용자 실행 시에만 유효 (`t.Skip if root`) | 1) NodeStageVolumeRequest 전송 | 오류 반환(non-nil); 패닉 없음; `FormatAndMount` 호출 성공 후 상태 파일 쓰기 단계에서 실패 | `CSI-N`, `State` |
| 185 | `TestCSINode_NodeUnstage_StateFileMissingIsOK` | 상태 파일 없음 + 스테이징 경로 언마운트 → NodeUnstageVolume 성공 (no-op) | stateDir에 해당 volumeID 상태 파일 없음; `mounter.IsMounted` 항상 `false` 반환 | 1) NodeUnstageVolumeRequest 전송 | 성공; `Disconnect` 0회; `Unmount` 0회 | `CSI-N`, `State` |
| 186 | `TestStageState_WriteReadDelete` | 상태 파일 쓰기 → 읽기 → 삭제 → 삭제 후 읽기 단위 기능 라운드트립 | `NewNodeServerWithStateDir("n", nil, nil, stateDir)`; 쓰기 가능 stateDir | 1) `writeStageState(volumeID, {SubsysNQN:"nqn.test:..."})` 호출; 2) `readStageState(volumeID)` 호출; 3) `deleteStageState(volumeID)` 호출; 4) `readStageState` 재호출 | 2단계: SubsysNQN 동일; 4단계: nil 반환; 오류 없음 | `CSI-N`, `State` |
| 187 | `TestStageState_DeleteIdempotent` | 존재하지 않는 상태 파일 삭제 → `ErrNotExist` 무시하고 성공 | `NewNodeServerWithStateDir`; stateDir에 해당 volumeID 파일 없음 | 1) `deleteStageState("pool/nonexistent")` 호출 | 오류 없음(nil 반환); 패닉 없음 | `CSI-N`, `State` |
| 188 | `TestStageState_VolumeIDSanitization` | VolumeID에 슬래시(`/`) 포함 시 상태 파일명 안전하게 변환 — 경로 탈출(path traversal) 방지 | `["pool/vol-a", "pool/vol-b", "other-pool/vol-c"]` 각 ID에 대해 `writeStageState` 호출 | 1) 각 volumeID로 `writeStageState` 호출; 2) `readStageState` 호출; 3) stateDir 파일 목록 확인 | 각 ID에 대해 독립적으로 상태 읽기 성공; stateDir의 파일명에 `/` 없음; 파일 간 데이터 혼동 없음 | `CSI-N`, `State` |

---

### E3.21 NodePublishVolume — 바인드 마운트, 읽기 전용, 멱등성 및 오류 처리 (단위 테스트)

**설명:** `internal/csi/node_publish_test.go`에 위치한 `NodePublishVolume` 단위 테스트.
주입 가능한 `mockConnector`/`mockMounter`를 사용하여 NVMe-oF 커널 모듈, 실제 블록
디바이스, root 권한 없이 바인드 마운트 로직 전체를 인프로세스로 검증한다.

**소스 파일:** `internal/csi/node_publish_test.go`

**CI 실행 가능 여부:** ✅ 가능 (실제 마운트 없음; mock 기반)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 189 | `TestNodePublishVolume_MountAccess` | MOUNT 접근 유형에서 NodePublishVolume이 스테이징 경로 → 타깃 경로로 바인드 마운트를 수행 | `newNodeTestEnv(t)` 초기화; `stagingPath=t.TempDir()`; `targetPath=t.TempDir()`; VolumeCapability=`mountCap("ext4")` | 1) `NodePublishVolumeRequest{VolumeId, StagingTargetPath, TargetPath, VolumeCapability}` 전송 | 성공(nil 오류); `mounter.IsMounted(targetPath)=true`; `mounter.mountCalls` 길이=1; `mountCalls[0].source=stagingPath`; `mountCalls[0].options`에 `"bind"` 포함 | `CSI-N`, `Mnt` |
| 190 | `TestNodePublishVolume_BlockAccess` | BLOCK 접근 유형에서 NodePublishVolume이 스테이징 경로를 타깃 경로로 바인드 마운트 | `newNodeTestEnv(t)` 초기화; VolumeCapability=`blockCap()` | 1) `NodePublishVolumeRequest{VolumeCapability: blockCap()}` 전송 | 성공; `mounter.IsMounted(targetPath)=true`; `mountCalls[0].source=stagingPath`; Mount 1회 호출 | `CSI-N`, `Mnt` |
| 191 | `TestNodePublishVolume_Readonly` | `Readonly=true`인 요청에서 마운트 옵션에 `"ro"`가 추가됨 | `newNodeTestEnv(t)` 초기화; `Readonly: true`; VolumeCapability=`mountCap("ext4")` | 1) `NodePublishVolumeRequest{Readonly: true}` 전송 | 성공; `mountCalls[0].options`에 `"ro"` 포함 | `CSI-N`, `Mnt` |
| 192 | `TestNodePublishVolume_Idempotent` | 동일 요청을 2회 호출하면 두 번째는 마운트를 수행하지 않음 (멱등성) | `newNodeTestEnv(t)` 초기화; 동일 `NodePublishVolumeRequest` 객체 준비 | 1) 1차 `NodePublishVolume` 호출; 2) 동일 인수로 2차 호출 | 두 호출 모두 성공; `mounter.mountCalls` 길이=1 (중복 마운트 없음) | `CSI-N`, `Mnt` |
| 193 | `TestNodePublishVolume_MissingVolumeID` | `VolumeId` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `VolumeId=""` 로 `NodePublishVolumeRequest` 전송 | gRPC `InvalidArgument`; 마운터 미호출 | `CSI-N` |
| 194 | `TestNodePublishVolume_MissingStagingTargetPath` | `StagingTargetPath` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `StagingTargetPath=""` 로 `NodePublishVolumeRequest` 전송 | gRPC `InvalidArgument` | `CSI-N` |
| 195 | `TestNodePublishVolume_MissingTargetPath` | `TargetPath` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `TargetPath=""` 로 `NodePublishVolumeRequest` 전송 | gRPC `InvalidArgument` | `CSI-N` |
| 196 | `TestNodePublishVolume_MissingVolumeCapability` | `VolumeCapability` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `VolumeCapability=nil` 로 `NodePublishVolumeRequest` 전송 | gRPC `InvalidArgument` | `CSI-N` |
| 197 | `TestNodePublishVolume_MountError` | `Mounter.Mount` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.mountErr = errors.New("mount failed")` | 1) 정상 파라미터로 `NodePublishVolumeRequest` 전송 | gRPC `Internal`; 패닉 없음 | `CSI-N`, `Mnt` |
| 198 | `TestNodePublishVolume_IsMountedError` | `Mounter.IsMounted` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.isMountedErr = errors.New("isMounted failed")` | 1) 정상 파라미터로 `NodePublishVolumeRequest` 전송 | gRPC `Internal`; Mount 미호출 | `CSI-N`, `Mnt` |

---

### E3.22 NodeUnpublishVolume — 언마운트, 멱등성 및 오류 처리 (단위 테스트)

**설명:** `internal/csi/node_publish_test.go`에 위치한 `NodeUnpublishVolume` 단위 테스트.
주입 가능한 `mockMounter`를 사용하여 언마운트 로직, 멱등성(이미 해제된 경우 no-op),
오류 전파를 인프로세스로 검증한다.

> **⚠️ CI 제약:** `Mounter.Unmount`는 mock이 추적하지만 실제 커널 언마운트는 수행하지
> 않는다. EBUSY(장치 사용 중) 시나리오는 mock으로 재현 가능하지만, 실제 프로세스가
> 마운트 포인트를 점유하는 시나리오는 유형 F(실제 HW) 테스트에서만 검증 가능하다.

**소스 파일:** `internal/csi/node_publish_test.go`

**CI 실행 가능 여부:** ✅ 가능 (실제 마운트 없음; mock 기반)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 199 | `TestNodeUnpublishVolume_Unmounts` | NodePublishVolume으로 마운트한 타깃 경로를 NodeUnpublishVolume이 정확히 해제 | `newNodeTestEnv(t)` 초기화; `NodePublishVolume` 선행 호출로 타깃 경로 마운트 | 1) `NodePublishVolume` 호출 (stagingPath→targetPath); 2) `IsMounted(targetPath)=true` 확인; 3) `NodeUnpublishVolume{VolumeId, TargetPath}` 전송; 4) `IsMounted(targetPath)` 재확인 | `NodeUnpublishVolume` 성공; `IsMounted=false`; `unmountCalls` 길이=1; `unmountCalls[0]=targetPath` | `CSI-N`, `Mnt` |
| 200 | `TestNodeUnpublishVolume_Idempotent` | 타깃 경로가 이미 마운트 해제된 상태에서 NodeUnpublishVolume 호출 시 성공 (no-op) | `newNodeTestEnv(t)` 초기화; `targetPath=t.TempDir()`; 마운트 없이 직접 `NodeUnpublishVolume` 호출 | 1) 마운트되지 않은 `targetPath`로 `NodeUnpublishVolumeRequest` 전송 | 성공(nil 오류); `unmountCalls` 길이=0 (Unmount 미호출) | `CSI-N`, `Mnt` |
| 201 | `TestNodeUnpublishVolume_TwiceMountsOnce` | Publish→Unpublish→Unpublish 사이클에서 Unmount는 정확히 1회 | `newNodeTestEnv(t)` 초기화; NodePublishVolume 1회 완료 | 1) `NodePublishVolume` 호출; 2) `NodeUnpublishVolume` 1차 호출; 3) 동일 인수로 `NodeUnpublishVolume` 2차 호출 | 두 `NodeUnpublishVolume` 모두 성공; `unmountCalls` 길이=1 (2차는 no-op) | `CSI-N`, `Mnt` |
| 202 | `TestNodeUnpublishVolume_MissingVolumeID` | `VolumeId` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `VolumeId=""` 로 `NodeUnpublishVolumeRequest` 전송 | gRPC `InvalidArgument`; 언마운터 미호출 | `CSI-N` |
| 203 | `TestNodeUnpublishVolume_MissingTargetPath` | `TargetPath` 누락 시 `InvalidArgument` 반환 | `newNodeTestEnv(t)` 초기화 | 1) `TargetPath=""` 로 `NodeUnpublishVolumeRequest` 전송 | gRPC `InvalidArgument` | `CSI-N` |
| 204 | `TestNodeUnpublishVolume_UnmountError` | `Mounter.Unmount` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.mountedPaths[targetPath]=true`; `env.mounter.unmountErr = errors.New("device busy")` | 1) `NodeUnpublishVolumeRequest{VolumeId, TargetPath}` 전송 | gRPC `Internal`; EBUSY 오류 전파; 마운트 상태 유지 | `CSI-N`, `Mnt` |
| 205 | `TestNodeUnpublishVolume_IsMountedError` | `Mounter.IsMounted` 오류 발생 시 `Internal` 반환 | `newNodeTestEnv(t)` 초기화; `env.mounter.isMountedErr = errors.New("isMounted failed")` | 1) `NodeUnpublishVolumeRequest{VolumeId, TargetPath}` 전송 | gRPC `Internal`; Unmount 미호출 | `CSI-N`, `Mnt` |

---

### E3.23 NodePublish/NodeUnpublish — 전체 노드 라이프사이클 (단위 테스트)

**설명:** `internal/csi/node_publish_test.go`의 `TestNodeFullLifecycle`은
NodeStageVolume → NodePublishVolume → NodeUnpublishVolume → NodeUnstageVolume
전체 4단계를 단일 테스트로 연결하여 검증한다. 이 테스트는 노드 측면의 완전한
연쇄 동작을 단위 수준에서 확인하는 회귀 기준점이다.

**소스 파일:** `internal/csi/node_publish_test.go`

**CI 실행 가능 여부:** ✅ 가능

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 206 | `TestNodeFullLifecycle` | Stage → Publish → Unpublish → Unstage 전체 노드 라이프사이클 단위 검증 | `newNodeTestEnv(t)` 초기화; `env.connector.devicePath="/dev/nvme0n1"`; `stagingPath=t.TempDir()`; `targetPath=t.TempDir()`; `VolumeContext`: `nqn`, `addr="192.0.2.10"`, `port="4420"` | 1) `NodeStageVolume{VolumeId, stagingPath, VolumeContext, mountCap("ext4")}` 전송; 2) `NodePublishVolume{VolumeId, stagingPath, targetPath, mountCap("ext4")}` 전송; 3) `IsMounted(targetPath)=true` 확인; 4) `NodeUnpublishVolume{VolumeId, targetPath}` 전송; 5) `IsMounted(targetPath)=false` 확인; 6) `NodeUnstageVolume{VolumeId, stagingPath}` 전송 | 전 단계 성공; Stage 후 스테이징 경로 마운트; Publish 후 타깃 경로 마운트; Unpublish 후 타깃 경로 해제; Unstage 후 스테이징 경로 해제; `disconnectCalls` 길이=1 | `CSI-N`, `Conn`, `Mnt`, `State` |

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

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 39 | `TestCSILifecycle_FullCycle` | Controller→Node 전체 경로: CreateVolume → ControllerPublish → NodeStage → NodePublish → NodeUnpublish → NodeUnstage → ControllerUnpublish → DeleteVolume | 단일 mockAgentServer; mockConnector/mockMounter 초기화; PillarTarget 등록 | 1) CreateVolume; 2) ControllerPublish; 3) NodeStage; 4) NodePublish; 5) NodeUnpublish; 6) NodeUnstage; 7) ControllerUnpublish; 8) DeleteVolume | 모든 단계 성공; agent 호출 순서 검증; VolumeContext 키가 NodeStage에 올바르게 전달됨 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `TgtCRD`, `VolCRD`, `State`, `gRPC` |

---

### E4.2 순서 제약 (Ordering Constraints)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 40 | `TestCSILifecycle_OrderingConstraints` | ControllerPublish 전 NodeStage 호출 → FailedPrecondition | 공유 VolumeStateMachine 초기화; CreateVolume 완료; ControllerPublish 미호출 | 1) NodeStageVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |

---

### E4.3 멱등성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 41 | `TestCSILifecycle_IdempotentSteps` | 라이프사이클 각 단계를 두 번씩 호출해도 최종 상태 동일 | 전체 라이프사이클 1회 완료 | 1) 전체 라이프사이클 재호출 (각 단계 2회씩) | 모든 재호출 성공; 중복 agent 호출 없음 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `SM`, `gRPC` |

---

### E4.4 VolumeContext 흐름

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 42 | `TestCSILifecycle_VolumeContextFlowThrough` | CreateVolume이 설정한 VolumeContext(NQN/address/port)가 NodeStageVolume에 키 변환 없이 그대로 전달됨 | mockAgentServer.ExportVolumeInfo에 특정 NQN/address/port 설정; mockConnector 초기화 | 1) CreateVolume; 2) ControllerPublish; 3) NodeStage; 4) mockConnector.ConnectCalls 검사 | NodeStage 시 mockConnector가 동일한 NQN/address/port로 호출됨 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `gRPC` |

---

## E5: 순서 제약 (Ordering Constraints)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

공유 VolumeStateMachine을 통해 컨트롤러와 노드 서버 간 순서 제약을 검증한다.

---

### E5.1 역순 호출 거부

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 43 | `TestCSIOrdering_NodeStageBeforeControllerPublish` | ControllerPublish 없이 NodeStage 호출 → FailedPrecondition | 공유 SM 초기화; CreateVolume 완료; ControllerPublish 미호출 | 1) NodeStageVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |
| 44 | `TestCSIOrdering_NodePublishBeforeNodeStage` | NodeStage 없이 NodePublish 호출 → FailedPrecondition | 공유 SM; ControllerPublish 완료; NodeStage 미완료 | 1) NodePublishVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |
| 45 | `TestCSIOrdering_NodeUnstageBeforeNodeUnpublish` | NodeUnpublish 없이 NodeUnstage 호출 → FailedPrecondition | 공유 SM; NodePublish 완료; NodeUnpublish 미호출 | 1) NodeUnstageVolumeRequest 전송 | gRPC FailedPrecondition | `CSI-N`, `SM` |
| 46 | `TestCSIOrdering_NodePublishAfterUnstage` | NodeUnstage 후 NodePublish 재시도 → FailedPrecondition | 공유 SM; 전체 정상 라이프사이클 완료 | 1) NodePublishVolumeRequest 재호출 | gRPC FailedPrecondition | `CSI-N`, `SM` |

---

### E5.2 정상 순서 통과

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 47 | `TestCSIOrdering_FullLifecycleWithSM` | 공유 SM을 사용한 전체 순서 라이프사이클 성공 | 공유 SM 초기화; mockConnector/mockMounter/mockAgentServer 준비 | 1) CreateVolume; 2) ControllerPublish; 3) NodeStage; 4) NodePublish; 5) NodeUnpublish; 6) NodeUnstage; 7) ControllerUnpublish; 8) DeleteVolume | 모든 단계 성공; SM 상태 전이 검증 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `SM`, `gRPC` |
| 48 | `TestCSIOrdering_IdempotencyWithSM` | 올바른 상태에서의 재호출은 순서 제약 위반 아님 | 공유 SM; 각 단계 정상 완료 | 1) 현재 상태에서 허용되는 RPC 재호출 | 성공; FailedPrecondition 미발생 | `CSI-C`, `CSI-N`, `SM` |

---

## E6: 부분 실패 영속성 (Partial Failure Persistence)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

PillarVolume CRD를 통한 부분 실패 상태 추적을 검증한다.

---

### E6.1 부분 실패 CRD 생성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 49 | `TestCSIController_PartialFailure_CRDCreatedOnExportFailure` | agent.CreateVolume 성공 + agent.ExportVolume 실패 시 PillarVolume CRD가 Phase=CreatePartial, BackendCreated=true로 생성됨 | mockAgentServer: CreateVolume 성공; ExportVolumeErr 설정; PillarTarget 등록 | 1) CreateVolumeRequest 전송 | CreateVolume gRPC 실패; PillarVolume CRD 존재; Phase=CreatePartial; BackendCreated=true | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 50 | `TestCSIController_PartialFailure_RetryAdvancesToReady` | 부분 실패 후 재시도 시 CRD가 Phase=Ready로 전환되고 ExportInfo 채워짐 | Phase=CreatePartial CRD 존재; ExportVolume 이번엔 성공 | 1) 동일 파라미터로 CreateVolumeRequest 재전송 | 성공; CRD Phase=Ready; ExportInfo 채워짐; PartialFailure 초기화 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 51 | `TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry` | 재시도 시 skipBackend 최적화로 agent.CreateVolume 재호출 없음 | Phase=CreatePartial CRD 존재; ExportVolume 이번엔 성공 | 1) CreateVolumeRequest 재전송; 2) mockAgentServer.CreateVolumeCalls 횟수 확인 | agent.CreateVolume 총 1회만 호출 (재시도 포함) | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

### E6.2 삭제 시 CRD 정리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 52 | `TestCSIController_DeleteVolume_CleansUpCRD` | 성공적인 DeleteVolume이 PillarVolume CRD를 삭제 | CreateVolume 성공 후 PillarVolume CRD 존재 | 1) DeleteVolumeRequest 전송; 2) fake 클라이언트에서 CRD 조회 | PillarVolume CRD가 클러스터에서 제거됨 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| 53 | `TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates` | 부분 생성 상태의 볼륨도 DeleteVolume으로 올바르게 정리됨 | Phase=CreatePartial인 PillarVolume CRD 존재; mockAgentServer 정상 동작 | 1) DeleteVolumeRequest 전송; 2) fake 클라이언트에서 CRD 재조회 | 성공; CRD 제거; BackendCreated=true면 agent.DeleteVolume 호출됨 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

### E6.3 zvol 중복 방지 — skipBackend 최적화 (No-Duplication)

**위치:** `test/e2e/csi_zvol_nodup_e2e_test.go`

**실행 명령:**
```bash
go test ./test/e2e/ -v -run TestCSIZvolNoDup
```

**핵심 설계 — `statefulZvolAgentServer`:**
`statefulZvolAgentServer`는 `mockAgentServer`를 내부에 포함하고, 별도의 `zvolRegistry`
(`map[string]struct{}`)를 유지하여 실제 ZFS 에이전트의 zvol 존재 여부를 추적한다.

- `CreateVolume` 성공 시 `zvolRegistry`에 `agentVolumeID` 추가 (멱등 — 동일 키 재삽입 시 `len == 1` 유지)
- `DeleteVolume` 성공 시 `zvolRegistry`에서 제거

이를 통해 "정확히 1개의 zvol만 존재함"을 RPC 호출 횟수만이 아니라 레지스트리 크기로도 직접 검증한다.

**핵심 동작 — `skipBackend` 최적화:**
컨트롤러는 `PillarVolume CRD`에서 `Phase=CreatePartial`과 `BackendDevicePath`가 이미 기록되어
있으면 `agent.CreateVolume`을 **재호출하지 않고** `agent.ExportVolume`만 재시도한다.
재시도마다 `agent.CreateVolume`을 호출하면 동일한 zvol이 중복 생성되어 데이터 손상이 발생한다.

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E6.3-1 | `TestCSIZvolNoDup_ExactlyOneZvolAfterExportFailureRetry` | export 실패 후 재시도 시 zvol이 정확히 1개만 존재 — skipBackend 최적화 동작 확인 | `statefulZvolAgentServer` 초기화; `ExportVolumeErr` 주입 후 재시도 전 제거; `newZvolTestEnv("storage-1")` | 1) CreateVolume(ExportVolume 실패) → 오류 확인; 2) zvol 수=1, agent.CreateVolume 호출=1, CRD Phase=CreatePartial, BackendDevicePath 비어 있지 않음 확인; 3) CreateVolume 재시도(ExportVolume 성공); 4) zvol 수=1, agent.CreateVolume 호출=1 유지, CRD Phase=Ready, PartialFailure=nil, ExportInfo 채워짐 확인 | 재시도 후 zvol 총 1개; agent.CreateVolume 총 1회 (skipBackend 발동); agent.ExportVolume 총 2회 (실패1+성공1); CRD Phase=Ready; BackendDevicePath="" (Ready 단계에서 소거) | `CSI-C`, `Agent`, `VolCRD`, `gRPC`, `SM` |
| E6.3-2 | `TestCSIZvolNoDup_ZvolRegistryReflectsDeleteAfterPartialCreate` | 부분 생성 상태에서 DeleteVolume 시 zvol 레지스트리가 1→0으로 정확히 감소 | 부분 실패(CreatePartial) 이후 PillarVolume CRD에서 `Spec.VolumeID` 추출; `ExportVolumeErr` 주입; `statefulZvolAgentServer` | 1) CreateVolume(ExportVolume 실패)로 zvol 1개 생성; 2) CRD에서 VolumeID 읽기; 3) DeleteVolume 호출; 4) zvol 수=0, CRD NotFound 확인 | DeleteVolume 성공; zvol 레지스트리 크기 0; PillarVolume CRD 제거됨 (`k8serrors.IsNotFound` 확인) | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E6.3-3 | `TestCSIZvolNoDup_MultipleRetriesNeverDuplicate` | 연속 3회 export 실패 후 최종 성공 — 매 재시도마다 zvol 수 1 유지 | `retryFails=3`; `statefulZvolAgentServer`; 3회 실패 후 `ExportVolumeErr=nil` 설정 | 1) 3회 연속 CreateVolume(ExportVolume 실패); 2) 각 실패 후 zvol 수=1, agent.CreateVolume 호출=1, agent.ExportVolume 호출 증가 확인; 3) 4번째 CreateVolume(ExportVolume 성공) | 모든 재시도에서 zvol 수 1 유지 (중복 없음); agent.CreateVolume 총 1회; agent.ExportVolume 총 `retryFails+1`=4회; 최종 CRD Phase=Ready; PartialFailure=nil; ExportInfo 채워짐 | `CSI-C`, `Agent`, `VolCRD`, `gRPC`, `SM` |

---

### E6 커버리지 요약

| 소섹션 | 검증 내용 | 테스트 수 | CI 실행 |
|--------|---------|----------|--------|
| E6.1 | 부분 실패 시 CRD Phase=CreatePartial 생성, 재시도 시 Ready 전환, skipBackend 호출 횟수 | 3개 | ✅ 표준 CI |
| E6.2 | DeleteVolume 성공 후 CRD 제거, 부분 생성 상태 볼륨 정리 | 2개 | ✅ 표준 CI |
| E6.3 | zvol 중복 방지 — 단일 재시도, 삭제 후 레지스트리 검증, 다중 재시도 무중복 | 3개 | ✅ 표준 CI |
| **합계** | | **8개** | ✅ |

**CI에서 검증 불가 항목 (정직한 평가):**

| 항목 | 이유 | 대안 |
|------|------|------|
| 실제 ZFS zvol 중복 생성 방지 | 실제 ZFS 커널 모듈 + root 권한 필요 | 유형 F 완전 E2E 또는 수동 스테이징 |
| CreatePartial 상태의 컨트롤러 재시작 복원 | 실제 프로세스 재시작 + Kubernetes API 서버 필요 | envtest 또는 수동 스테이징 |
| agent.ExportVolume 타임아웃 후 부분 실패 | 실제 네트워크 지연 + 타임아웃 필요 | 수동 스테이징 |
| PillarVolume CRD 저장 실패 시 데이터 일관성 | 실제 etcd 장애 또는 API 서버 불안정 필요 | 카오스 엔지니어링 도구 (예: Litmus) |

---

## E7: 게시 멱등성 (Publish Idempotency)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

---

### E7.1 ControllerPublishVolume 멱등성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 54 | `TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs` | 동일 인수로 ControllerPublishVolume 2회 호출: 두 호출 모두 성공, AllowInitiator는 총 2회 | 유효한 VolumeId/NodeId/VolumeContext; mockAgentServer 정상 | 1) ControllerPublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; PublishContext 동일; CreateVolume/ExportVolume 미트리거 | `CSI-C`, `Agent`, `gRPC` |
| 55 | `TestCSIPublishIdempotency_ControllerPublishVolume_DifferentNodes` | 서로 다른 노드에 대한 ControllerPublishVolume은 각각 독립적으로 성공 | 동일 VolumeId; 서로 다른 NodeId 준비 | 1) ControllerPublishVolume(NodeId1); 2) ControllerPublishVolume(NodeId2) | 두 호출 모두 성공; AllowInitiator 서로 다른 호스트 NQN으로 각 1회씩 | `CSI-C`, `Agent`, `gRPC` |

---

### E7.2 NodePublishVolume 멱등성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 56 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget` | 동일 타깃 경로로 NodePublishVolume 2회 호출: 두 번째는 no-op | NodeStage 성공; mockMounter 초기화 | 1) NodePublishVolume 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; 응답 동일; 중복 마운트 없음 | `CSI-N`, `Mnt` |
| 57 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleBlockAccess` | 블록 접근 모드에서도 NodePublishVolume 2회 호출 멱등성 보장 | NodeStage(BLOCK 접근 모드) 성공 | 1) NodePublishVolume(BLOCK) 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; 중복 블록 디바이스 노출 없음 | `CSI-N`, `Mnt` |
| 58 | `TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble` | 읽기 전용 NodePublishVolume 2회 호출 멱등성 | NodeStage 성공; Readonly=true 설정 | 1) NodePublishVolume(Readonly=true) 1회; 2) 동일 인수로 재호출 | 두 호출 모두 성공; 응답 동일 | `CSI-N`, `Mnt` |

---

## E8: mTLS 컨트롤러 통합 테스트

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

PillarTarget 컨트롤러 ↔ pillar-agent mTLS 신뢰 경계를 실제 gRPC 리스너와
인메모리 인증서로 검증한다. 실제 Kubernetes 클러스터 불필요.

---

### E8.1 mTLS 인증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 59 | `TestMTLSController_AgentConnectedAuthenticated` | 올바른 mTLS 자격증명으로 연결 시 PillarTarget 상태 AgentConnected=True/Authenticated | testcerts.New()로 동일 CA의 서버/클라이언트 인증서 생성; mTLS 서버 + 컨트롤러 설정 | 1) mTLS 서버 기동; 2) 컨트롤러 PillarTarget 조정 실행; 3) PillarTarget 조건 검사 | AgentConnected 조건 True; Reason=Authenticated | `mTLS`, `TgtCRD`, `gRPC` |
| 60 | `TestMTLSController_PlaintextDialRejected` | 평문 클라이언트가 mTLS 서버에 거부됨 | 서버: mTLS 설정; 클라이언트: insecure.NewCredentials() 사용 | 1) 평문 dial 시도; 2) 컨트롤러 조정 실행; 3) PillarTarget 조건 검사 | AgentConnected 조건 False; Reason=HealthCheckFailed 또는 TLSHandshakeFailed | `mTLS`, `TgtCRD`, `gRPC` |
| 61 | `TestMTLSController_WrongCAClientRejected` | 다른 CA가 서명한 클라이언트 인증서는 거부됨 | 서버: CA1 서명 인증서; 클라이언트: CA2 서명 인증서 생성 | 1) 잘못된 CA의 인증서로 dial 시도; 2) 컨트롤러 조정 실행; 3) PillarTarget 조건 검사 | AgentConnected 조건 False | `mTLS`, `TgtCRD`, `gRPC` |

---

## E9: Agent gRPC E2E 테스트

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

실제 gRPC 리스너(localhost:0)와 mock ZFS backend로 agent.Server의
네트워크 직렬화/역직렬화 레이어까지 포함하여 검증한다.
실제 ZFS 커널 모듈 불필요.

---

### E9.1 역량 및 헬스체크

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 62 | `TestAgent_GetCapabilities` | 실제 gRPC 연결을 통한 GetCapabilities 호출이 올바른 역량 목록 반환 | agentE2EMockBackend 초기화; 실제 gRPC 리스너(localhost:0) 기동 | 1) GetCapabilitiesRequest 전송 | ZFS_ZVOL backend + NVMEOF_TCP 프로토콜 포함 | `Agent`, `ZFS`, `gRPC` |
| 63 | `TestAgent_HealthCheck` | 실제 gRPC 연결을 통한 HealthCheck 호출 | sysModuleZFSPath를 tmpdir의 존재하는 파일로 설정; tmpdir configfs 루트 설정 | 1) HealthCheckRequest 전송 | ZFS 모듈 체크 HEALTHY; configfs 체크 결과 포함 | `Agent`, `NVMeF`, `gRPC` |

---

### E9.2 전체 왕복

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 64 | `TestAgent_RoundTrip` | CreateVolume → ExportVolume → AllowInitiator → DenyInitiator → UnexportVolume → DeleteVolume 전체 왕복을 실제 gRPC를 통해 검증 | agentE2EMockBackend; tmpdir configfs; 실제 gRPC 리스너 | 1) CreateVolume; 2) ExportVolume; 3) AllowInitiator; 4) DenyInitiator; 5) UnexportVolume; 6) DeleteVolume | 모든 단계 성공; configfs 상태 변화 검증 | `Agent`, `ZFS`, `NVMeF`, `gRPC` |

---

### E9.3 재조정 복구

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 65 | `TestAgent_ReconcileStateRestoresExports` | ReconcileState가 재시작 후 configfs 엔트리를 올바르게 복원 | 빈 tmpdir configfs; 볼륨 목록 준비 | 1) ReconcileState(볼륨 목록) 호출; 2) tmpdir configfs 디렉터리 존재 확인 | ReconcileState 후 모든 볼륨의 configfs 서브시스템 디렉터리 존재 | `Agent`, `NVMeF`, `gRPC` |

---

### E9.4 오류 처리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 66 | `TestAgent_ErrorHandling` | 다양한 오류 시나리오(잘못된 pool ID, backend 오류 등)가 적절한 gRPC 상태 코드로 매핑 | agentE2EMockBackend에 각 오류 조건 설정; 실제 gRPC 리스너 | 1) 잘못된 pool ID로 CreateVolume; 2) backend 오류 주입 후 각 RPC 호출; 3) 오류 코드 검증 | NotFound/InvalidArgument/Internal 등 명세에 맞는 gRPC 코드 반환 | `Agent`, `ZFS`, `gRPC` |

---

### E9.5 Phase 1 전체 RPC

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 67 | `TestAgent_AllPhase1RPCs` | Phase 1에서 지원하는 모든 RPC를 한 테스트에서 순차적으로 검증 | agentE2EMockBackend; tmpdir configfs; 실제 gRPC 리스너 | 1) GetCapabilities; 2) HealthCheck; 3) CreateVolume; 4) ExportVolume; 5) AllowInitiator; 6) DenyInitiator; 7) UnexportVolume; 8) DeleteVolume | 모든 Phase 1 RPC 성공; 오류 없음 | `Agent`, `ZFS`, `NVMeF`, `gRPC` |

---

> **📋 참고:** E10 (클러스터 레벨 E2E 테스트) 은 **유형 B 테스트** 이므로
> 카테고리 1과 분리하여 [카테고리 2 섹션](#카테고리-2--클러스터-레벨-e2e-테스트-유형-b-kind-클러스터-필요-)에 기술되어 있다.

---

## E11: 볼륨 확장(Volume Expansion) 통합 E2E

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**아키텍처:**
```
CSI ControllerServer → (실제 gRPC, localhost:0) → mockAgentServer
                        (ControllerExpandVolume → agent.ExpandVolume)

CSI NodeServer → mockResizer (인메모리, resize2fs/xfs_growfs 없음)
                 (NodeExpandVolume → Resizer.ResizeFS)
```

**배경:** CSI 명세에서 볼륨 확장은 두 단계로 이루어진다.
1. `ControllerExpandVolume` — 스토리지 백엔드(블록 레이어)를 새 크기로 확장하고
   `node_expansion_required=true`를 반환하여 노드 측 파일시스템 리사이즈 필요성을 알린다.
2. `NodeExpandVolume` — 노드에서 파일시스템(`resize2fs`/`xfs_growfs`)을 확장하여
   블록 디바이스가 커진 만큼 파일시스템도 채운다.

이 섹션은 두 단계를 **한 E2E 흐름으로 연결**하는 테스트를 다룬다. 개별 단계 검증은
E2.3(ControllerExpandVolume)과 `internal/csi/node_expand_test.go`에서 다루며,
이 섹션은 **교차-컴포넌트 확장 경계**를 집중적으로 검증한다.

---

### E11.1 ControllerExpandVolume — 에이전트 위임 및 node_expansion_required

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 88 | `TestCSIExpand_ControllerExpandVolume_ForwardsToAgent` | ControllerExpandVolume이 agent.ExpandVolume을 올바른 VolumeId·BackendType·RequestedBytes로 호출하고 node_expansion_required=true를 반환 | mockAgentServer.ExpandVolumeResp.CapacityBytes=2GiB; 유효한 VolumeId | 1) CapacityRange.RequiredBytes=2GiB 로 ControllerExpandVolumeRequest 전송 | 성공; CapacityBytes=2GiB; NodeExpansionRequired=true; agent.ExpandVolume 1회 호출 | `CSI-C`, `Agent`, `gRPC` |
| 89 | `TestCSIExpand_ControllerExpandVolume_AgentReturnsZeroCapacity` | agent.ExpandVolume이 CapacityBytes=0을 반환하면 RequiredBytes를 폴백으로 사용 | mockAgentServer.ExpandVolumeResp.CapacityBytes=0; CapacityRange.RequiredBytes=3GiB | 1) ControllerExpandVolumeRequest 전송 | 성공; CapacityBytes=3GiB (RequiredBytes 폴백); NodeExpansionRequired=true | `CSI-C`, `Agent`, `gRPC` |

---

### E11.2 NodeExpandVolume — 파일시스템 타입별 리사이즈

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 90 | `TestCSIExpand_NodeExpandVolume_Ext4` | NodeExpandVolume이 ext4 파일시스템에서 mockResizer.ResizeFS("ext4")를 호출 | mockResizer 주입; VolumeCapability.MountVolume.FsType="ext4"; VolumePath 설정 | 1) NodeExpandVolumeRequest 전송 | 성공; ResizeFS 1회 호출; FsType="ext4"; CapacityBytes=RequiredBytes | `CSI-N`, `Mnt` |
| 91 | `TestCSIExpand_NodeExpandVolume_XFS` | NodeExpandVolume이 xfs 파일시스템에서 mockResizer.ResizeFS("xfs")를 호출 | mockResizer 주입; VolumeCapability.MountVolume.FsType="xfs" | 1) NodeExpandVolumeRequest 전송 | 성공; ResizeFS 1회 호출; FsType="xfs" | `CSI-N`, `Mnt` |

---

### E11.3 전체 확장 왕복(Full Expand Round Trip)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 92 | `TestCSIExpand_FullExpandRoundTrip` | CreateVolume → ControllerExpandVolume → NodeExpandVolume 전체 확장 흐름 | 단일 mockAgentServer; mockResizer 주입; VolumeId 동일 사용 | 1) CreateVolume; 2) ControllerExpandVolume(newSize); 3) NodeExpandVolume | ControllerExpandVolume: CapacityBytes=newSize, NodeExpansionRequired=true; NodeExpandVolume: ResizeFS 1회 | `CSI-C`, `CSI-N`, `Agent`, `Mnt`, `gRPC` |
| 93 | `TestCSIExpand_ControllerExpandVolume_Idempotent` | 이미 확장된 볼륨에 동일한 크기로 ControllerExpandVolume 재호출 — 멱등성 | mockAgentServer: ExpandVolume 항상 현재 크기 반환 | 1) ControllerExpandVolume 1회; 2) 동일 RequiredBytes로 재호출 | 두 호출 모두 성공; agent.ExpandVolume 2회 호출; 오류 없음 | `CSI-C`, `Agent`, `gRPC` |

---

### E11.4 오류 경로

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 94 | `TestCSIExpand_ControllerExpandVolume_AgentFails` | agent.ExpandVolume 실패 시 ControllerExpandVolume이 오류 코드를 전파 | mockAgentServer.ExpandVolumeErr=gRPC ResourceExhausted | 1) ControllerExpandVolumeRequest 전송 | ControllerExpandVolume이 ResourceExhausted 반환; NodeExpansionRequired 없음 | `CSI-C`, `Agent`, `gRPC` |
| 95 | `TestCSIExpand_NodeExpandVolume_ResizerFails` | mockResizer.ResizeFS 실패 시 NodeExpandVolume이 Internal 반환 | mockResizer.ResizeFSErr 설정 | 1) NodeExpandVolumeRequest 전송 | gRPC Internal; 오류 메시지에 "resize" 포함 | `CSI-N`, `Mnt` |

---

## E12: CSI 스냅샷 (현재 미구현)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능 (미구현 검증 한정)

**현재 구현 상태:**

pillar-csi는 현재 **CSI VolumeSnapshot 역량을 구현하지 않는다.**
`ControllerServer`는 `csi.UnimplementedControllerServer`를 임베드하므로
`CreateSnapshot`, `DeleteSnapshot`, `ListSnapshots` RPC는 자동으로
gRPC `Unimplemented` 상태를 반환한다.

**플러그인 역량 선언:** `GetPluginCapabilities`에 `VolumeSnapshot` 역량이
포함되지 않으므로, 규격을 준수하는 CO는 스냅샷 RPC를 호출하지 않아야 한다.

**아키텍처 참고 (에이전트 프로토콜 수준):**

에이전트 프로토콜(`proto/pillar_csi/agent/v1/agent.proto`)에는 볼륨 데이터를
스트리밍하는 `SendVolume` / `ReceiveVolume` RPC가 있으며, `SendVolumeRequest`는
선택적 `snapshot_name` 필드를 지원한다:

```proto
// Optional snapshot name to send from.
//   ZFS   → snapshot component, e.g. "snap0" → zfs send pool/vol@snap0
//   LVM   → LV snapshot name
string snapshot_name = 3;
```

이 RPC는 **CSI VolumeSnapshot과 별개의 out-of-band 데이터 마이그레이션 채널**이며,
현재 CSI 컨트롤러 레이어와 연결되어 있지 않다. 실제 ZFS 커널 모듈과 스트리밍
gRPC가 필요하다.

---

### E12.1 미구현 스냅샷 RPC — Unimplemented 반환 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 96 | `TestCSISnapshot_CreateSnapshot_ReturnsUnimplemented` | CSI CreateSnapshot이 현재 미구현으로 gRPC Unimplemented를 반환 | ControllerServer 초기화 | 1) CreateSnapshotRequest(유효 파라미터) 전송 | gRPC Unimplemented; 에이전트 호출 없음 | `CSI-C` |
| 97 | `TestCSISnapshot_DeleteSnapshot_ReturnsUnimplemented` | CSI DeleteSnapshot이 현재 미구현으로 gRPC Unimplemented를 반환 | ControllerServer 초기화 | 1) DeleteSnapshotRequest(SnapshotId="storage-1/snap-test") 전송 | gRPC Unimplemented | `CSI-C` |
| 98 | `TestCSISnapshot_ListSnapshots_ReturnsUnimplemented` | CSI ListSnapshots이 현재 미구현으로 gRPC Unimplemented를 반환 | ControllerServer 초기화 | 1) ListSnapshotsRequest(빈 요청) 전송 | gRPC Unimplemented | `CSI-C` |

---

### E12.2 GetPluginCapabilities — 스냅샷 역량 미선언 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 99 | `TestCSISnapshot_PluginCapabilities_NoSnapshotCapability` | GetPluginCapabilities 응답에 VolumeSnapshot 역량이 포함되지 않음 | IdentityServer 초기화 | 1) GetPluginCapabilitiesRequest 전송; 2) 응답 역량 목록 검사 | VolumeExpansion_ONLINE은 있으나 스냅샷 관련 역량 없음 | `CSI-C` |

---

**⚠️ 현실적 한계:**

표준 CSI VolumeSnapshot 기능(Kubernetes `VolumeSnapshotClass` / `VolumeSnapshot` CRD)을
완전히 지원하려면 다음이 필요하다:

| 필요 항목 | 설명 |
|----------|------|
| CSI CreateSnapshot RPC 구현 | controller.go에 CreateSnapshot 추가; agent에 ZFS `zfs snapshot` 호출 추가 |
| CSI DeleteSnapshot RPC 구현 | agent에 ZFS `zfs destroy pool/vol@snap` 추가 |
| Kubernetes external-snapshotter | sidecar 컨테이너 추가; VolumeSnapshotClass CRD 배포 |
| 실제 ZFS 커널 모듈 | CI에서 실행 불가; 전용 스토리지 노드 필요 |

현재 Phase에서 스냅샷 관련 실질적 E2E 테스트는 `향후 추가 예정 테스트` 섹션(F13–F16)을 참조한다.

---

## E13: 볼륨 클론 및 데이터 마이그레이션 (현재 부분 구현)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능 (미구현 동작 검증 한정)

**현재 구현 상태:**

CSI 명세의 `CreateVolume` 요청은 `VolumeContentSource` 필드를 통해
**볼륨 클론**(기존 볼륨 복사)이나 **스냅샷 복원**을 요청할 수 있다.
현재 pillar-csi `ControllerServer.CreateVolume`은 이 필드를 **파싱하지 않으며
무시**한다 — 항상 빈 볼륨을 생성한다.

```
CreateVolume(VolumeContentSource: {Snapshot: "snap-A"})
    → agent.CreateVolume(empty new volume)   ← 클론/복원 없음
    → agent.ExportVolume
```

에이전트 프로토콜에는 **out-of-band 데이터 마이그레이션** 채널이 존재한다:
- `SendVolume(RPC)` — 서버-스트림: `zfs send` / `dd if=<lv>`
- `ReceiveVolume(RPC)` — 클라이언트-스트림: `zfs receive` / `dd of=<lv>`

이 채널은 CSI ControllerServer와 연결되어 있지 않으며, 실제 ZFS 커널 모듈과
스트리밍 gRPC 연결이 필요하다. CI에서 테스트할 수 없다.

---

### E13.1 VolumeContentSource 미처리 동작 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 100 | `TestCSIClone_CreateVolume_SnapshotSourceIgnored` | VolumeContentSource.Snapshot이 포함된 CreateVolume 호출 시 스냅샷 소스를 무시하고 빈 볼륨을 생성 (현재 동작 고정 테스트) | PillarTarget 등록; mockAgentServer 정상 동작 | 1) VolumeContentSource.Snapshot="snap-A" 를 포함한 CreateVolumeRequest 전송 | CreateVolume 성공; agent.CreateVolume 1회 (VolumeContentSource 없이); 빈 볼륨 생성 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 101 | `TestCSIClone_CreateVolume_VolumeSourceIgnored` | VolumeContentSource.Volume이 포함된 CreateVolume 호출 시 소스 볼륨을 무시하고 빈 볼륨을 생성 (현재 동작 고정 테스트) | PillarTarget 등록; mockAgentServer 정상 동작 | 1) VolumeContentSource.Volume="src-pvc-id" 를 포함한 CreateVolumeRequest 전송 | CreateVolume 성공; 소스 데이터 복사 없이 빈 볼륨; agent.CreateVolume 1회 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

---

### E13.2 에이전트 SendVolume/ReceiveVolume — 스트리밍 RPC 역량 검증

**테스트 유형:** ❌ 인프로세스 E2E 불가 (스트리밍 gRPC + 실제 데이터 필요)

| 제약 사항 | 설명 |
|----------|------|
| 서버-스트림 RPC | `SendVolume`은 서버가 청크 단위로 데이터를 스트리밍; mockAgentServer에서 스텁 구현 가능하나 실제 ZFS 데이터 없이는 의미 없음 |
| 클라이언트-스트림 RPC | `ReceiveVolume`은 클라이언트가 청크 단위로 데이터를 전송; 대용량 데이터 처리 검증에 실제 블록 장치 필요 |
| 실제 ZFS 필요 | `zfs send pool/vol@snap0` 출력이 있어야 snapshot_name 필드 동작 검증 가능 |

실제 SendVolume/ReceiveVolume E2E 테스트는 `향후 추가 예정 테스트` 섹션(F17–F19)을 참조한다.

---

**⚠️ 현실적 한계:**

완전한 CSI 볼륨 클론 지원을 위해서는 다음이 필요하다:

| 필요 항목 | 설명 |
|----------|------|
| CreateVolume VolumeContentSource 처리 | controller.go가 Snapshot/Volume 소스를 파싱하고 agent에 ZFS clone 요청 |
| agent ZFS clone RPC 추가 | `zfs clone pool/vol@snap pool/new-vol` 를 실행하는 새 RPC 또는 CreateVolume 확장 |
| 실제 ZFS 커널 모듈 + 스냅샷 | CI에서 실행 불가 |
| Kubernetes `external-provisioner` 클론 플로우 | StorageClass `dataSource` 처리 |

---

## E14: 잘못된 입력값 및 엣지 케이스 (Invalid Inputs & Edge Cases)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

잘못된 입력, 경계값, 지원하지 않는 파라미터 조합이 CSI 명세에 맞는
오류 코드로 올바르게 거부되는지 검증한다. 각 케이스에서 agent 호출이
발생하지 않아야 하며, 서버 패닉이 없어야 한다.

---

### E14.1 VolumeId 형식 위반

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 102 | `TestCSIEdge_CreateVolume_ExtremelyLongVolumeName` | 극도로 긴 볼륨 이름(2048자)으로 CreateVolume 호출 | ControllerServer 초기화; PillarTarget 등록 | 1) name="pvc-"+2000자 문자열 로 CreateVolumeRequest 전송 | gRPC InvalidArgument 또는 성공; 패닉 없음 | `CSI-C` |
| 103 | `TestCSIEdge_CreateVolume_SpecialCharactersInName` | 볼륨 이름에 슬래시("/") 포함 — VolumeId 파싱 혼동 유발 시도 | ControllerServer 초기화; PillarTarget 등록 | 1) name="pvc/with/slashes" 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음; VolumeId 파싱 혼동 없음 | `CSI-C` |
| 104 | `TestCSIEdge_DeleteVolume_EmptyVolumeId` | 빈 VolumeId로 DeleteVolume 호출 | ControllerServer 초기화 | 1) VolumeId="" 로 DeleteVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 105 | `TestCSIEdge_ControllerPublish_EmptyNodeId` | NodeId가 빈 문자열인 ControllerPublishVolume | ControllerServer 초기화; 유효한 VolumeId/VolumeContext | 1) NodeId="" 로 ControllerPublishVolumeRequest 전송 | gRPC InvalidArgument; agent.AllowInitiator 호출 없음 | `CSI-C` |

---

### E14.2 CapacityRange 경계값

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 106 | `TestCSIEdge_CreateVolume_LimitLessThanRequired` | LimitBytes < RequiredBytes로 CreateVolume | ControllerServer 초기화; PillarTarget 등록 | 1) CapacityRange(RequiredBytes=2GiB, LimitBytes=1GiB) 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 107 | `TestCSIEdge_ControllerExpand_ZeroRequiredBytes` | ControllerExpandVolume에서 RequiredBytes=0 | ControllerServer 초기화; 유효한 VolumeId | 1) CapacityRange(RequiredBytes=0, LimitBytes=0) 로 ControllerExpandVolumeRequest 전송 | gRPC InvalidArgument; agent.ExpandVolume 호출 없음 | `CSI-C` |
| 108 | `TestCSIEdge_ControllerExpand_ShrinkRequest` | 현재 크기보다 작은 RequiredBytes로 ControllerExpandVolume | mockAgentServer.ExpandVolumeErr에 "volsize cannot be decreased" 설정 | 1) ControllerExpandVolumeRequest 전송 | 비-OK gRPC 상태 (Internal) | `CSI-C`, `Agent`, `gRPC` |
| 109 | `TestCSIEdge_CreateVolume_ExactLimitEqualsRequired` | RequiredBytes == LimitBytes (경계값) 로 CreateVolume | PillarTarget 등록; mockAgentServer 정상 | 1) CapacityRange(RequiredBytes=LimitBytes=1GiB) 로 CreateVolumeRequest 전송 | 성공; agent.CreateVolume이 1GiB로 호출됨 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |

---

### E14.3 VolumeContext 값 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 110 | `TestCSIEdge_NodeStage_InvalidPort` | VolumeContext.port가 숫자가 아닌 문자열 | NodeServer 초기화 | 1) VolumeContext.port="not-a-port" 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connector.Connect 미호출 | `CSI-N` |
| 111 | `TestCSIEdge_NodeStage_EmptyNQN` | VolumeContext.target_id(NQN)가 빈 문자열 | NodeServer 초기화 | 1) VolumeContext.target_id="" 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connector.Connect 미호출 | `CSI-N` |
| 112 | `TestCSIEdge_NodeStage_MissingVolumeContext` | VolumeContext 자체가 nil인 NodeStageVolume | NodeServer 초기화 | 1) VolumeContext=nil 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument; Connector.Connect 미호출 | `CSI-N` |

---

### E14.4 StorageClass 파라미터 조합 오류

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 113 | `TestCSIEdge_CreateVolume_UnsupportedBackendType` | 알 수 없는 backend-type 파라미터로 CreateVolume | ControllerServer 초기화; PillarTarget 등록 | 1) parameters["backend-type"]="lvm" 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |
| 114 | `TestCSIEdge_CreateVolume_EmptyProtocolType` | protocol-type 파라미터 값이 빈 문자열 | ControllerServer 초기화; PillarTarget 등록 | 1) parameters["protocol-type"]="" 로 CreateVolumeRequest 전송 | gRPC InvalidArgument; agent 호출 없음 | `CSI-C` |

---

### E14.5 접근 모드(Access Mode) 조합 오류

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 115 | `TestCSIEdge_NodeStage_BlockAccessWithFsType` | 블록 접근 모드 VolumeCapability에 FsType 지정 (잘못된 조합) | NodeServer 초기화; mockConnector.DevicePath 설정 | 1) VolumeCapability(AccessType=Block, FsType="ext4") 로 NodeStageVolumeRequest 전송 | gRPC InvalidArgument 또는 FsType 무시 후 블록 접근 성공; FormatAndMount 미호출 | `CSI-N`, `Conn` |
| 116 | `TestCSIEdge_CreateVolume_MultiNodeMultiWriter` | MULTI_NODE_MULTI_WRITER 접근 모드로 ValidateVolumeCapabilities | ControllerServer 초기화 | 1) VolumeCapabilities(AccessMode=MULTI_NODE_MULTI_WRITER) 로 ValidateVolumeCapabilitiesRequest 전송 | Message 필드에 미지원 이유 기록; CreateVolume 불가 | `CSI-C` |

---

## E15: 리소스 고갈 (Resource Exhaustion)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

스토리지 풀 용량 부족, 연결 타임아웃 등 리소스 고갈 시나리오에서
올바른 gRPC 오류 코드가 반환되고 상태가 오염되지 않음을 검증한다.

**주의:** 실제 ZFS 풀 용량 고갈 테스트(F22–F23)는 실제 하드웨어가 필요하다.
이 섹션은 mock을 통한 오류 코드 매핑 및 상태 일관성에 집중한다.

---

### E15.1 풀 용량 고갈 오류 전파

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 117 | `TestCSIExhaustion_CreateVolume_PoolFull` | 스토리지 풀 가득 참 시 CreateVolume 실패 — gRPC 오류 코드 전파 검증 | mockAgentServer.CreateVolumeErr=gRPC ResourceExhausted; PillarTarget 등록 | 1) CreateVolumeRequest 전송 | gRPC ResourceExhausted 또는 Internal 반환; PillarVolume CRD 미생성; ExportVolume 미호출 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |
| 118 | `TestCSIExhaustion_ExpandVolume_ExceedsPoolCapacity` | ControllerExpandVolume이 풀 용량 초과 시도 — 오류 전파 검증 | mockAgentServer.ExpandVolumeErr=gRPC ResourceExhausted; 유효한 VolumeId | 1) ControllerExpandVolumeRequest 전송 | gRPC ResourceExhausted 반환; NodeExpansionRequired 없음 | `CSI-C`, `Agent`, `gRPC` |
| 119 | `TestCSIExhaustion_CreateVolume_InsufficientStorage` | 요청 용량이 사용 가능 용량보다 큰 경우 | mockAgentServer.CreateVolumeErr=gRPC OutOfRange; PillarTarget 등록 | 1) CreateVolumeRequest 전송 | 비-OK gRPC 상태; 볼륨 미생성; 패닉 없음 | `CSI-C`, `Agent`, `TgtCRD`, `gRPC` |

---

### E15.2 연속 실패 시나리오 — 상태 일관성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 120 | `TestCSIExhaustion_CreateVolume_ConsecutiveFailures` | agent.CreateVolume이 연속 5회 실패해도 상태 오염 없음 | mockAgentServer.CreateVolumeErr 항상 반환; PillarTarget 등록 | 1) CreateVolumeRequest 5회 반복 전송 | 5회 모두 비-OK gRPC 상태; PillarVolume CRD 0개; fake k8s 클라이언트 상태 오염 없음; 패닉 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 121 | `TestCSIExhaustion_NodeStage_ConnectTimeout` | NVMe-oF 연결 타임아웃 시 NodeStage 실패 — 상태 파일 미생성 검증 | mockConnector.ConnectErr=errors.New("connection timed out") | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; StateDir에 상태 파일 0개; FormatAndMount 미호출 | `CSI-N`, `Conn`, `State` |
| 122 | `TestCSIExhaustion_NodeStage_DeviceNeverAppears` | NVMe-oF 연결 성공 후 디바이스가 폴링 타임아웃 내에 나타나지 않음 | mockConnector.Connect 성공; DevicePath="" (항상 빈 경로); 폴링 타임아웃=50ms | 1) NodeStageVolumeRequest 전송 (short timeout context 사용) | 비-OK gRPC 상태; 상태 파일 미생성 | `CSI-N`, `Conn`, `State` |

---

## E16: 동시 작업 (Concurrent Operations)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

여러 고루틴이 동시에 CSI RPC를 호출할 때 데드락, 패닉, 데이터 손상이
발생하지 않음을 검증한다. 최종 상태의 정확성보다 **안전성(safety)**에
초점을 맞춘다.

**아키텍처:**
```
여러 고루틴 ──────► CSI ControllerServer / NodeServer
                           │
                    mockAgentServer (mutex 보호)
                    fake k8s client (thread-safe)
                    mockCSIConnector / mockCSIMounter
```

---

### E16.1 동시 볼륨 생성/삭제

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 123 | `TestCSIConcurrent_CreateVolume_SameNameNoPanic` | 동일 이름으로 5개 고루틴이 동시에 CreateVolume 호출해도 패닉/데드락 없음 | mockAgentServer 정상 동작; PillarTarget 등록; 5초 타임아웃 | 1) 5개 goroutine을 동시에 시작; 각각 동일 볼륨 이름으로 CreateVolumeRequest 전송; 2) WaitGroup 완료 대기 | 5개 고루틴 모두 5초 내 완료; 패닉 없음; 일부 성공/나머지 AlreadyExists 가능 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 124 | `TestCSIConcurrent_CreateVolume_DifferentNames` | 5개 고루틴이 각각 다른 이름의 볼륨을 동시에 생성 | mockAgentServer 정상 동작; PillarTarget 등록 | 1) 5개 goroutine을 동시에 시작; 각각 고유한 볼륨 이름으로 CreateVolumeRequest 전송; 2) WaitGroup 완료 대기 | 5개 볼륨 모두 성공; PillarVolume CRD 5개; 데이터 손상 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 125 | `TestCSIConcurrent_CreateDelete_Interleaved` | 볼륨 생성과 삭제를 동시에 수행 — 최종 상태 일관성 검증 | mockAgentServer 정상 동작; PillarTarget 등록 | 1) goroutine A: CreateVolumeRequest 전송; 2) goroutine B: 동시에 동일 VolumeId로 DeleteVolumeRequest 전송; 3) 양측 완료 대기 | 두 연산 모두 완료; 최종 상태는 생성 또는 삭제 중 하나; CRD 상태 일관성; 패닉 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |

---

### E16.2 동시 노드 마운트/언마운트

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 126 | `TestCSIConcurrent_NodeStage_SameVolumeDifferentPaths` | 동일 VolumeId를 서로 다른 스테이징 경로로 동시에 NodeStage 호출 | mockConnector 정상 동작; 동일 VolumeId; 유효한 VolumeContext | 1) 2개 goroutine 동시 시작; 각각 다른 StagingTargetPath로 NodeStageVolumeRequest 전송; 2) WaitGroup 완료 대기 | 두 호출 모두 완료; 데드락 없음; Connector.Connect 각 경로별 독립 호출; 각각 별도 상태 파일 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 127 | `TestCSIConcurrent_NodePublish_MultipleTargets` | 스테이지 완료 후 3개 고루틴이 서로 다른 targetPath로 동시 NodePublish | NodeStage 1회 성공 완료; 상태 파일 존재; mockMounter 정상 | 1) NodeStage 호출; 2) 3개 goroutine 동시 시작; 각각 다른 TargetPath로 NodePublishVolumeRequest 전송; 3) WaitGroup 완료 대기 | 3개 NodePublish 모두 성공; mockMounter.MountCalls=3; 데드락 없음; 마운트 테이블 오염 없음 | `CSI-N`, `Mnt`, `State` |

---

### E16.3 동시 ACL 관리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 128 | `TestCSIConcurrent_AllowInitiator_MultipleNodes` | 다른 NodeId에 대해 동시에 ControllerPublishVolume 3회 호출 | mockAgentServer.AllowInitiator 정상; PillarVolume CRD 존재; 동일 VolumeId | 1) 3개 goroutine 동시 시작; 각각 다른 NodeId(호스트 NQN)로 ControllerPublishVolumeRequest 전송; 2) WaitGroup 완료 대기 | 3개 모두 완료; 데드락 없음; AllowInitiator 3회 호출; 각각 다른 호스트 NQN | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| 129 | `TestCSIConcurrent_UnpublishVolume_Race` | 3개 노드에서 동시에 ControllerUnpublishVolume 호출 | mockAgentServer.DenyInitiator 정상; PillarVolume CRD 존재; 3개 NodeId 각각 publish 완료 상태 | 1) 3개 goroutine 동시 시작; 각기 다른 NodeId로 ControllerUnpublishVolumeRequest 전송; 2) WaitGroup 완료 대기 | 3개 모두 완료; 패닉 없음; DenyInitiator 3회 호출 (각 goroutine별 1회) | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

## E17: 정리 검증 (Cleanup Validation)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

CSI 연산 실패 또는 성공 후 부가 상태(상태 파일, PillarVolume CRD,
마운트 테이블, NVMe-oF 연결)가 올바르게 정리되는지 검증한다.
리소스 누수가 없음을 확인하는 것이 이 섹션의 핵심 목표이다.

---

### E17.1 실패 후 상태 파일 정리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 130 | `TestCSICleanup_NodeStage_ConnectFailureNoStateFile` | NodeStageVolume에서 Connect 실패 시 상태 파일이 생성되지 않음 | mockConnector.ConnectErr=errors.New("connect refused"); 임시 StateDir; 유효한 VolumeContext | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; StateDir에 상태 파일 0개; Mounter.FormatAndMount 미호출 | `CSI-N`, `Conn`, `State` |
| 131 | `TestCSICleanup_NodeStage_MountFailureDisconnects` | FormatAndMount 실패 시 이미 완료된 NVMe-oF 연결이 정리(롤백)됨 | mockConnector.Connect 성공(DevicePath="/dev/nvme0n1"); mockMounter.FormatAndMountErr 설정 | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; Connector.Disconnect 1회 호출(롤백); StateDir에 상태 파일 0개 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 132 | `TestCSICleanup_NodeUnstage_FailurePreservesStateFile` | Connector.Disconnect 실패 시 상태 파일이 보존됨 — 재시도 가능 상태 유지 | NodeStage 성공(상태 파일 존재); mockConnector.DisconnectErr=errors.New("disconnect failed") | 1) NodeUnstageVolumeRequest 전송 | 비-OK gRPC 상태; StateDir에 상태 파일 유지(재시도 보존); Disconnect 1회 시도 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E17.2 PillarVolume CRD 정리 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 133 | `TestCSICleanup_DeleteVolume_RemovesAllCRD` | 성공적 DeleteVolume 후 PillarVolume CRD가 fake k8s 클라이언트에서 완전히 삭제됨 | CreateVolume 성공; PillarVolume CRD 존재 확인; mockAgentServer.DeleteVolume 정상 | 1) CreateVolumeRequest 전송; 2) CRD 존재 확인; 3) DeleteVolumeRequest 전송; 4) fake 클라이언트로 CRD 재조회 | DeleteVolume 성공; CRD 조회 시 NotFound; agent.DeleteVolume 1회 호출 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 134 | `TestCSICleanup_CreatePartial_DeleteVolumeCleansCRD` | 부분 생성 상태(Phase=CreatePartial) CRD도 DeleteVolume으로 정리됨 | mockAgentServer.ExportVolumeErr 설정으로 CreateVolume 실패(Phase=CreatePartial CRD 생성됨); DeleteVolume용 mockAgentServer.DeleteVolume 정상 | 1) CreateVolumeRequest 전송(ExportVolume 단계에서 실패); 2) Phase=CreatePartial CRD 존재 확인; 3) DeleteVolumeRequest 전송 | DeleteVolume 성공; CRD 제거; BackendCreated=true이면 agent.DeleteVolume 호출 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `SM`, `gRPC` |
| 135 | `TestCSICleanup_FullLifecycle_NoResourceLeak` | 전체 라이프사이클 완료 후 모든 상태가 완전히 정리됨 | mockAgentServer 정상; mockConnector 정상; mockMounter 정상; PillarTarget 등록; 임시 StateDir | 1) CreateVolume; 2) ControllerPublish; 3) NodeStage; 4) NodePublish; 5) NodeUnpublish; 6) NodeUnstage; 7) ControllerUnpublish; 8) DeleteVolume | StateDir 상태 파일 0개; PillarVolume CRD 0개; mockMounter 마운트 테이블 빈 상태; Connector.DisconnectCalls=1 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `TgtCRD`, `VolCRD`, `State`, `gRPC` |

---

### E17.3 반복 생성/삭제 — 누적 상태 오염 없음

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 136 | `TestCSICleanup_RepeatedCreateDelete` | 동일 이름의 볼륨을 10회 반복 생성/삭제해도 상태 오염 없음 | mockAgentServer 정상; PillarTarget 등록; 동일 볼륨 이름 재사용 | 1) 루프 10회: CreateVolumeRequest → 성공 확인 → CRD 존재 확인 → DeleteVolumeRequest → CRD 삭제 확인 | 모든 10회 성공; 매 반복 후 PillarVolume CRD 0개; 누적 오류 없음; 패닉 없음 | `CSI-C`, `Agent`, `TgtCRD`, `VolCRD`, `gRPC` |
| 137 | `TestCSICleanup_RepeatedStageUnstage` | 동일 볼륨을 5회 반복 NodeStage/NodeUnstage해도 상태 파일 누적 없음 | mockConnector 정상; mockMounter 정상; 임시 StateDir; 동일 VolumeId 및 StagingTargetPath | 1) 루프 5회: NodeStageVolumeRequest → 상태 파일 존재 확인 → NodeUnstageVolumeRequest → StateDir 빈 상태 확인 | 모든 5회 성공; 매 반복 후 StateDir 빈 상태; Connect/Disconnect 각 5회씩 호출 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

## E18: Agent 다운 오류 시나리오 (Agent Down Error Scenarios)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

CSI 에이전트(`pillar-csi-agent`)가 응답 불가, 연결 거부, 타임아웃 등 "다운" 상태일 때
CSI ControllerServer와 NodeServer가 올바르게:
1. **감지(Detection)**: 에이전트 불가 상태를 탐지한다.
2. **보고(Reporting)**: 적절한 gRPC 상태 코드와 진단 메시지로 CO(Container Orchestrator)에 보고한다.
3. **복구(Recovery)**: 에이전트가 재시작되면 `ReconcileState`를 통해 configfs 상태를 복원한다.

> **설계 원칙:** CSI 명세에서 일시적 에이전트 실패는 Kubernetes 재시도 메커니즘(kubelet, CSI sidecar)이
> 담당한다. CSI 컨트롤러/노드 서버는 에이전트 실패를 **삼키지 않고** CO에 명확히 보고해야 한다.

**아키텍처 (E18 전용):**
```
CSI ControllerServer
        │
        ├─── AgentDialer ──► 연결 거부 / 타임아웃  ←─ 에이전트 다운 감지
        │                        │
        │              gRPC Unavailable / DeadlineExceeded 반환
        │                        │
        └───────────────────────────────────► CO에 비-OK 상태 보고

에이전트 재시작 복구:
  agent.NewServer() (인메모리 상태 없음)
        │
        └─► ReconcileState() ──► NvmetTarget.Apply() ──► configfs 재구성
```

---

### E18.1 에이전트 연결 불가 감지

CSI 컨트롤러가 에이전트 gRPC 다이얼 실패를 감지하고 적절한 오류를 반환하는지 검증한다.
다이얼 실패 유형은 두 가지: (1) gRPC 상태 오류(`Unavailable`), (2) 평문 Go error.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 138 | `TestCSIController_CreateVolume_AgentUnreachable` | `AgentDialer`가 `codes.Unavailable("connection refused")` 반환 시 `CreateVolume`이 `Unavailable`을 그대로 전파 — 에이전트 프로세스가 완전히 다운된 경우 시뮬레이션 | `newCSIControllerTestEnvWithDialErr(t, status.Error(codes.Unavailable, "connection refused"))` 주입; 실제 에이전트 없이 다이얼 오류만 주입; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송; 2) 반환 오류의 gRPC 코드 확인 | `codes.Unavailable` 반환; PillarVolume CRD 미생성; `agent.CreateVolume` 미호출 | `CSI-C`, `gRPC` |
| 139 | `TestCSIErrors_CreateVolume_AgentUnreachable_PlainError` | `AgentDialer`가 평문 Go error 반환 시(DNS 실패, 즉각적 연결 거부) 오류가 CO에 전파됨 — gRPC 상태 코드가 아닌 네트워크 레이어 오류 시뮬레이션 | `newCSIControllerTestEnvWithDialErr(t, errors.New("dial tcp 192.168.1.10:9500: connect: connection refused"))` 주입; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송; 2) 오류 유무 확인; 3) gRPC 상태 코드 확인 | 비-OK gRPC 상태(`codes.OK` 불가); 에이전트 호출 없음; 패닉 없음 | `CSI-C`, `gRPC` |

---

### E18.2 에이전트 타임아웃 및 오류 보고

에이전트가 요청은 수신했으나 내부 처리 중 타임아웃이 발생하는 경우(에이전트는 살아있으나 ZFS I/O가 멈춘 상황),
또는 에이전트 내부 서브시스템(configfs)이 부분 장애인 경우의 오류 보고를 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 140 | `TestCSIErrors_CreateVolume_AgentDeadlineExceeded` | `agent.CreateVolume`이 `codes.DeadlineExceeded`("agent: ZFS command timed out") 반환 시 `ControllerServer`가 CO에 비-OK 상태 전파 — 에이전트 내부 ZFS 작업 타임아웃(스토리지 노드 과부하) 시뮬레이션 | `csiMockAgent.createVolumeFn`이 `status.Error(codes.DeadlineExceeded, "agent: ZFS command timed out")` 반환; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송 | 비-OK gRPC 상태(`codes.OK` 불가); PillarVolume CRD 미생성; 오류 메시지에 타임아웃 문맥 포함 가능 | `CSI-C`, `Agent`, `gRPC` |
| 141 | `TestCSIErrors_ControllerExpand_AgentDeadlineExceeded` | `agent.ExpandVolume`이 `codes.DeadlineExceeded`("agent: ZFS expand timed out") 반환 시 `ControllerExpandVolume`이 비-OK 전파 — 확장 작업 중 에이전트 타임아웃 | `csiMockAgent.expandVolumeFn`이 `status.Error(codes.DeadlineExceeded, "agent: ZFS expand timed out")` 반환 | 1) `ControllerExpandVolumeRequest`(RequiredBytes=20GiB) 전송 | 비-OK gRPC 상태; `NodeExpansionRequired` 없음 | `CSI-C`, `Agent`, `gRPC` |
| 142 | `TestCSIErrors_DeleteVolume_AgentDeadlineExceeded` | `agent.UnexportVolume`이 `codes.DeadlineExceeded`("agent: unexport timed out") 반환 시 `DeleteVolume`이 비-OK 전파 — 정리 작업 중 에이전트 타임아웃 | `csiMockAgent.unexportVolumeFn`이 `status.Error(codes.DeadlineExceeded, "agent: unexport timed out")` 반환 | 1) `DeleteVolumeRequest` 전송 | 비-OK gRPC 상태; 삭제 작업 중단; CRD 정리 롤백 여부는 구현 의존 | `CSI-C`, `Agent`, `gRPC` |
| 143 | `TestCSIErrors_ControllerPublish_AllowInitiatorFails` | `agent.AllowInitiator`가 configfs 쓰기 실패(`codes.Internal`) 반환 시 `ControllerPublishVolume`이 오류 전파 — 에이전트 프로세스는 살아있으나 configfs가 손상된 부분 장애 시뮬레이션 | `csiMockAgent.allowInitiatorFn`이 `status.Error(codes.Internal, "AllowInitiator: configfs write failed: permission denied")` 반환 | 1) `ControllerPublishVolumeRequest`(NodeId=호스트 NQN) 전송 | 비-OK gRPC 상태; `ControllerPublishVolume` 오류 전파; 오류 삼킴 없음 | `CSI-C`, `Agent`, `gRPC` |

---

### E18.3 에이전트 재시작 복구

에이전트 재시작 시뮬레이션: 새 `agent.Server` 인스턴스(인메모리 상태 없음)에
`ReconcileState` RPC를 호출하여 configfs 상태가 복원되는지 검증한다.

> **참고:** `TestAgent_ReconcileStateRestoresExports`는 [E9.3](#e93-재조정-복구)에 이미 기록되어 있다.
> 여기서는 E18 문맥(**에이전트 다운 복구 시나리오**)에서의 의미를 추가 설명한다: 에이전트가 재시작되면
> CSI 컨트롤러가 `ReconcileState`를 호출하여 인메모리 상태가 사라진 에이전트에 원하는 상태를 강제 적용한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 144 | `TestAgent_ReconcileStateRestoresExports` | 에이전트 재시작 후 `ReconcileState` 호출이 configfs NVMe-oF 서브시스템 엔트리를 재구성 — 프로세스 재시작으로 인메모리 상태가 사라진 후에도 configfs에 원하는 상태를 강제 적용 | `agent.NewServer(backends, tmpdir)`로 신선한 서버 생성(재시작 시뮬레이션, 기존 인메모리 상태 없음); tmpdir configfs 루트(초기 빈 상태); 볼륨 ID·디바이스 경로·ExportDesiredState·AllowedInitiators 목록 준비 | 1) `ReconcileState({volumes: [{VolumeId, DevicePath, Exports: [{NvmeofTcp params, AllowedInitiators: [hostNQN]}]}]})` 호출; 2) `tmpdir/nvmet/subsystems/<NQN>` 디렉터리 존재 확인; 3) `tmpdir/nvmet/hosts/<NQN>` 디렉터리 존재 확인; 4) `allowed_hosts/<NQN>` 심볼릭 링크 존재 확인; 5) 동일 요청으로 `ReconcileState` 재호출(멱등성 검증) | (1) `results[0].Success=true`; `results[0].VolumeId=volumeID`; (2) configfs 서브시스템 디렉터리 복원 완료; (3) 허용 이니시에이터 ACL 심볼릭 링크 존재; (4) `ReconcileState` 재호출 시 성공(멱등성) — 컨트롤러가 반복 호출해도 안전 | `Agent`, `NVMeF`, `gRPC` |

---

### E18.4 에이전트 다운 — CI에서 검증 불가 시나리오

> **⚠️ CI 실행 불가** — 실제 에이전트 프로세스 재시작, 노드 재부팅, 실제 커널 NVMe-oF 상태가 필요하다.
> 수동 스테이징 환경에서만 검증 가능.

아래 시나리오는 **자동화 불가 이유**와 **수동 검증 방법**을 문서화하여,
릴리스 전 스테이징 환경 체크리스트로 활용한다.

**자동화 불가 이유:**
- 실제 프로세스 종료/재시작은 `os.Exit()` 또는 시그널 전송이 필요하며, 인프로세스 테스트에서는 테스트 프로세스 자체가 종료된다.
- 실제 커널 NVMe-oF 상태(`/sys/kernel/config/nvmet`)는 루트 권한 + `nvmet` 커널 모듈이 필요하다.
- Kubernetes DaemonSet 재시작 동작은 실제 API 서버와 kubelet이 필요하다.

| ID | 시나리오 | 사전 조건 | 수동 실행 절차 | 허용 기준 | 커버리지 |
|----|---------|----------|--------------|---------|---------|
| AD-1 | **에이전트 프로세스 강제 종료 후 재시작 — NVMe-oF 수출 지속성 검증** | 실제 NVMe-oF 수출 설정이 완료된 스토리지 노드; 실제 `/sys/kernel/config` 마운트; 진행 중인 클라이언트 NVMe 연결 | 1) `kill -9 <agent-pid>` 실행; 2) `/sys/kernel/config/nvmet/subsystems/` 아래 엔트리 유지 확인(`ls`); 3) 클라이언트 노드에서 NVMe 연결 지속성 확인(`nvme list`); 4) 에이전트 재시작(`systemctl start pillar-agent`); 5) CSI 컨트롤러의 `ReconcileState` 호출 확인(에이전트 로그); 6) 볼륨 I/O 정상 확인 | 에이전트 종료 중에도 커널 NVMe-oF 상태 유지; 재시작 후 `ReconcileState`로 상태 동기화 완료; 클라이언트 I/O 무중단(또는 짧은 중단 후 자동 재연결) | `Agent`, `NVMeF`, `실제 커널` |
| AD-2 | **CSI 컨트롤러가 에이전트 다운 감지 후 PillarTarget 상태 갱신** | 실제 Kubernetes 클러스터; cert-manager; mTLS 설정 완료; `PillarTarget` CRD 존재; 에이전트 정상 실행 중 | 1) `kubectl get pillartarget -o yaml`로 초기 `AgentConnected=True` 확인; 2) 에이전트 중지(`systemctl stop` 또는 `kill -STOP`); 3) CSI 컨트롤러 `HealthCheck` 폴링 주기(~30초) 대기; 4) `kubectl get pillartarget -o yaml` 재확인; 5) 에이전트 재시작 후 상태 복원 확인 | 에이전트 중지 후: `PillarTarget.Status.Conditions[AgentConnected].Status=False`, `Reason=HealthCheckFailed` 또는 `ConnectionLost`; 에이전트 재시작 후: `AgentConnected=True` 복원 | `mTLS`, `TgtCRD`, `Agent` |
| AD-3 | **에이전트 OOM Kill 후 Kubernetes 자동 복구** | Kubernetes 스토리지 노드에 배포된 에이전트 DaemonSet; `restartPolicy: Always`; 진행 중인 PVC 사용 파드 | 1) `kubectl exec -n pillar-csi <agent-pod> -- kill -9 1`(PID 1 강제 종료); 2) `kubectl get pod -n pillar-csi -w`로 재시작 관찰; 3) 재시작 후 `kubectl describe pod`에서 Restart Count 확인; 4) 기존 PVC를 사용하는 파드의 I/O 정상 확인 | Kubernetes가 에이전트 파드를 자동 재시작; 재시작 후 `ReconcileState`로 상태 복원; 기존 PVC 마운트는 커널 NVMe 레이어에서 지속됨(I/O 무중단 또는 짧은 중단 후 자동 복구) | `Agent`, `NVMeF`, `실제 커널`, `Kubernetes클러스터` |

---

## E21: 잘못된 CR 오류 시나리오 (Invalid CR Error Scenarios)

**테스트 유형:** A (인프로세스) + C (envtest 통합) — **혼합**

이 섹션은 Custom Resource(CR)의 잘못된 필드, 누락된 필수 값, 불변 필드 수정 시도,
API 서버 스키마 위반 등 다양한 CR 수준 오류 시나리오를 정의한다.
E14(CSI 요청 파라미터 오류)와 달리 이 섹션은 **Kubernetes 객체(CRD 인스턴스)
자체의 유효성 문제**에 집중한다.

**영향 범위:**
- `PillarTarget` — 에이전트 주소 및 연결 정보를 담는 CR
- `PillarPool` — 스토리지 풀을 나타내는 CR
- `PillarVolume` — CSI 볼륨 생명주기 상태를 추적하는 CR

---

### E21 소섹션 분류 요약

| 소섹션 | 테스트 유형 | 빌드 태그 | CI 실행 | 테스트 위치 |
|--------|-----------|----------|--------|------------|
| E21.1 | A (in-process) | 없음 | ✅ 표준 CI | `test/e2e/` |
| E21.2 | C (envtest) | `integration` | ✅ envtest 필요 | `internal/webhook/v1alpha1/` |
| E21.3 | C (envtest) | `integration` | ✅ envtest 필요 | `internal/webhook/v1alpha1/` |
| E21.4 | C (envtest) | `integration` | ✅ envtest 필요 | `internal/controller/` |

> **⚠️ Fake client 한계:** E21.1은 fake k8s 클라이언트를 사용하므로
> CRD OpenAPI 스키마 검증 및 웹훅 어드미션이 실제로 실행되지 않는다.
> 스키마 검증은 E21.4(envtest), 웹훅은 E21.2–E21.3(envtest)에서 별도 검증한다.

---

### E21.1 컨트롤러 런타임 잘못된 CR 처리 (Type A — in-process) ✅

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**빌드 태그:** 없음

**실행 명령:**
```bash
go test ./test/e2e/ -v -run TestCSIInvalidCR
```

CSI 컨트롤러가 **이미 존재하나 잘못된 상태**를 가진 CR을 런타임에 조회했을 때
적절한 gRPC 오류 코드를 반환하고 패닉 없이 처리함을 검증한다.
fake k8s 클라이언트는 웹훅/스키마 검증을 실행하지 않으므로,
잘못된 필드 값을 직접 조작하여 주입한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 145 | `TestCSIInvalidCR_CreateVolume_TargetResolvedAddressEmpty` | PillarTarget이 존재하나 `Status.ResolvedAddress`가 빈 문자열 — 에이전트 주소 미확정 상태 | fake k8s 클라이언트에 `PillarTarget{spec:{nodeRef:{name:"storage-node"}}, status:{resolvedAddress:""}}` 등록; ControllerServer 초기화; 유효한 StorageClass 파라미터(target="storage-node") | 1) `CreateVolumeRequest` 전송 | `codes.Unavailable`; 오류 메시지에 "no resolved address" 포함; `dialAgent` 미호출; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| 146 | `TestCSIInvalidCR_CreateVolume_TargetSpecBothNil` | PillarTarget의 `spec.nodeRef`와 `spec.external`이 모두 nil — 연결 정보 없음 (fake client에서만 가능; 웹훅 미실행) | fake k8s 클라이언트에 `PillarTarget{spec:{}, status:{resolvedAddress:""}}` 등록; 유효한 StorageClass 파라미터 | 1) `CreateVolumeRequest` 전송 | `codes.Unavailable`; "no resolved address" 오류; agent 호출 없음 | `CSI-C`, `TgtCRD` |
| 147 | `TestCSIInvalidCR_ControllerPublish_TargetNoAddress` | ControllerPublishVolume 시 PillarTarget의 `Status.ResolvedAddress`가 빈 문자열 | PillarVolume CRD 존재(Phase=Ready, ExportInfo 채워짐); PillarTarget 등록 but `status.resolvedAddress=""`; 유효한 VolumeId | 1) `ControllerPublishVolumeRequest`(NodeId=호스트 NQN) 전송 | `codes.Unavailable`; `agent.AllowInitiator` 미호출 | `CSI-C`, `TgtCRD`, `VolCRD` |
| 148 | `TestCSIInvalidCR_LoadState_UnknownPhase` | PillarVolume CRD가 정의되지 않은 Phase 값을 가질 때 `LoadStateFromPillarVolumes`가 `StateNonExistent`로 처리하고 패닉 없음 | fake k8s 클라이언트에 `PillarVolume{spec:{volumeID:"t1/nvmeof-tcp/zfs-zvol/pool/pvc-abc"}, status:{phase:"GarbagePhase"}}` 등록; ControllerServer 초기화 | 1) `LoadStateFromPillarVolumes` 호출; 2) 해당 VolumeId의 SM 상태 조회 | 오류 반환 없음(nil); 해당 볼륨 SM 상태 = `StateNonExistent`; 패닉 없음; 다른 볼륨 상태 영향 없음 | `CSI-C`, `VolCRD` |
| 149 | `TestCSIInvalidCR_LoadState_ListFailure` | `k8sClient.List(PillarVolumeList)` 실패 시 `LoadStateFromPillarVolumes`가 오류를 반환하고 SM 상태를 오염시키지 않음 | fake k8s 클라이언트를 List 실패를 반환하는 mock으로 교체 | 1) `LoadStateFromPillarVolumes` 호출 | 오류 반환(non-nil); SM 상태 변경 없음; 패닉 없음 | `CSI-C`, `VolCRD` |
| 150 | `TestCSIInvalidCR_ControllerExpand_TargetNoAddress` | ControllerExpandVolume 시 PillarTarget `Status.ResolvedAddress`가 빈 문자열 | 유효한 PillarVolume CRD(Phase=Ready); PillarTarget `status.resolvedAddress=""`; StorageClass 파라미터 유효 | 1) `ControllerExpandVolumeRequest`(RequiredBytes=20GiB) 전송 | `codes.Unavailable`; `agent.ExpandVolume` 미호출 | `CSI-C`, `TgtCRD` |

---

### E21.2 PillarTarget 웹훅 — 불변 필드 수정 거부 (Type C — envtest) ⚠️

**테스트 유형:** C (envtest 통합 테스트)

**빌드 태그:** `//go:build integration`

**실행 명령:**
```bash
make setup-envtest
go test -tags=integration ./internal/webhook/v1alpha1/ -v -run PillarTarget
```

**위치:** `internal/webhook/v1alpha1/pillartarget_webhook_test.go`

**자동화 가능 여부:** ✅ CI 실행 가능 — envtest 바이너리(`kube-apiserver`, `etcd`)만 필요. Docker/Kind 불필요.

`PillarTargetSpec`은 **판별 유니온(discriminated union)** 구조이다:
`spec.nodeRef` 또는 `spec.external` 중 정확히 하나만 지정해야 한다.
생성 후 이 필드를 변경하면 연결된 모든 PillarPool/PillarBinding이 다른 물리 서버를
가리키게 되어 데이터 손실 위험이 있다.
`PillarTargetCustomValidator.ValidateUpdate()`가 이를 방지한다.

> **⚠️ CI에서 테스트 가능 여부:** `ValidateUpdate`는 직접 메서드 호출로 검증 가능하나,
> 전체 admission webhook 플로우(API 서버 → webhook 서버 → 실제 k8s.Update)는
> `envtest` 환경에서만 확인된다. 이 섹션의 테스트들은 `PillarTargetCustomValidator`를
> 직접 호출하므로 envtest 바이너리 없이도 단독으로 실행 가능하다.
> 단, 빌드 태그 `integration` 하에서만 컴파일된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 151 | `TestPillarTargetWebhook_Update_DiscriminantSwitch_NodeToExternal` | `spec.nodeRef` → `spec.external` 전환 시도 거부 | `validator = PillarTargetCustomValidator{}`; `oldObj.spec.nodeRef={name:"node1"}`; `newObj.spec.external={address:"1.2.3.4", port:9500}` (nodeRef=nil) | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환(non-nil); `field.Forbidden` 포함; 메시지에 "cannot switch between nodeRef and external" 포함 | `Webhook`, `TgtCRD` |
| 152 | `TestPillarTargetWebhook_Update_DiscriminantSwitch_ExternalToNode` | `spec.external` → `spec.nodeRef` 전환 시도 거부 | `oldObj.spec.external={address:"1.2.3.4", port:9500}`; `newObj.spec.nodeRef={name:"node1"}` (external=nil) | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환; `spec` 경로에 `Forbidden` 포함 | `Webhook`, `TgtCRD` |
| 153 | `TestPillarTargetWebhook_Update_NodeRefNameImmutable` | `spec.nodeRef.name` 변경 시도 거부 | `oldObj.spec.nodeRef={name:"node-a"}`; `newObj.spec.nodeRef={name:"node-b"}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환; `field.Forbidden(spec.nodeRef.name, ...)` 포함; 이전값 "node-a", 신값 "node-b" 언급 | `Webhook`, `TgtCRD` |
| 154 | `TestPillarTargetWebhook_Update_ExternalAddressImmutable` | `spec.external.address` 변경 시도 거부 | `oldObj.spec.external={address:"1.2.3.4", port:9500}`; `newObj.spec.external={address:"5.6.7.8", port:9500}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환; `field.Forbidden(spec.external.address, ...)` 포함; 이전값 "1.2.3.4", 신값 "5.6.7.8" 언급 | `Webhook`, `TgtCRD` |
| 155 | `TestPillarTargetWebhook_Update_ExternalPortImmutable` | `spec.external.port` 변경 시도 거부 | `oldObj.spec.external={address:"1.2.3.4", port:9500}`; `newObj.spec.external={address:"1.2.3.4", port:9600}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환; `field.Forbidden(spec.external.port, ...)` 포함; 이전값 9500, 신값 9600 언급 | `Webhook`, `TgtCRD` |
| 156 | `TestPillarTargetWebhook_Update_NodeRefNonIdentityFieldChange_OK` | `spec.nodeRef.name`이 변경되지 않고 비식별 필드(`addressType`)만 변경된 업데이트는 허용됨 | `oldObj.spec.nodeRef={name:"node-a", addressType:"InternalIP"}`; `newObj.spec.nodeRef={name:"node-a", addressType:"ExternalIP"}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 없음(nil 반환); 업데이트 허용됨 | `Webhook`, `TgtCRD` |
| 157 | `TestPillarTargetWebhook_Create_Valid` | 유효한 PillarTarget 생성 시 웹훅이 허용 (현재 ValidateCreate는 no-op 스캐폴딩) | `obj.spec.nodeRef={name:"storage-node-1"}`; 유효한 PillarTarget 객체 | 1) `validator.ValidateCreate(ctx, obj)` 호출 | 오류 없음(nil 반환) — 현재 구현은 스캐폴딩(TODO); 향후 검증 추가 시 갱신 필요 | `Webhook`, `TgtCRD` |

---

### E21.3 PillarPool 웹훅 — 불변 필드 수정 거부 (Type C — envtest) ⚠️

**테스트 유형:** C (envtest 통합 테스트)

**빌드 태그:** `//go:build integration`

**실행 명령:**
```bash
make setup-envtest
go test -tags=integration ./internal/webhook/v1alpha1/ -v -run PillarPool
```

**위치:** `internal/webhook/v1alpha1/pillarpool_webhook_test.go`

**자동화 가능 여부:** ✅ CI 실행 가능

`PillarPool`의 `spec.targetRef`와 `spec.backend.type`은 생성 시 고정된다.
이 필드들을 변경하면 해당 풀에서 이미 프로비저닝된 모든 볼륨이 잘못된 백엔드/타깃을
가리키게 된다. `PillarPoolCustomValidator.ValidateUpdate()`가 이를 방지한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 158 | `TestPillarPoolWebhook_Update_TargetRefImmutable` | `spec.targetRef` 변경 시도 거부 | `validator = PillarPoolCustomValidator{}`; `oldObj.spec={targetRef:"target-a", backend:{type:"zfs-zvol"}}`; `newObj.spec={targetRef:"target-b", backend:{type:"zfs-zvol"}}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환; `field.Forbidden(spec.targetRef, ...)` 포함; 이전값 "target-a", 신값 "target-b" 언급 | `Webhook`, `TgtCRD`, `VolCRD` |
| 159 | `TestPillarPoolWebhook_Update_BackendTypeImmutable` | `spec.backend.type` 변경 시도 거부 | `oldObj.spec={targetRef:"t1", backend:{type:"zfs-zvol"}}`; `newObj.spec={targetRef:"t1", backend:{type:"lvm-lv"}}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환; `field.Forbidden(spec.backend.type, ...)` 포함; 이전값 "zfs-zvol", 신값 "lvm-lv" 언급 | `Webhook`, `TgtCRD`, `VolCRD` |
| 160 | `TestPillarPoolWebhook_Update_ZFSPoolChange_OK` | `spec.backend.type` 변경 없이 ZFS 풀 이름만 변경된 업데이트는 허용됨 | `oldObj.spec={targetRef:"t1", backend:{type:"zfs-zvol", zfs:{pool:"tank"}}}`; `newObj.spec={targetRef:"t1", backend:{type:"zfs-zvol", zfs:{pool:"new-tank"}}}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 없음(nil 반환); 업데이트 허용됨 | `Webhook`, `TgtCRD`, `VolCRD` |
| 161 | `TestPillarPoolWebhook_Update_BothFieldsChanged_MultipleErrors` | `spec.targetRef`와 `spec.backend.type` 모두 변경 시도 → 두 필드 모두 Forbidden 오류 포함 | `oldObj.spec={targetRef:"t1", backend:{type:"zfs-zvol"}}`; `newObj.spec={targetRef:"t2", backend:{type:"lvm-lv"}}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | 오류 반환; `field.ErrorList` 길이 = 2; `spec.targetRef`와 `spec.backend.type` 모두 Forbidden | `Webhook`, `TgtCRD`, `VolCRD` |
| 162 | `TestPillarPoolWebhook_Create_Valid` | 유효한 PillarPool 생성 시 웹훅이 허용 (현재 ValidateCreate는 no-op 스캐폴딩) | `obj.spec={targetRef:"target-1", backend:{type:"zfs-zvol", zfs:{pool:"tank"}}}` | 1) `validator.ValidateCreate(ctx, obj)` 호출 | 오류 없음(nil 반환) — 현재 구현은 스캐폴딩(TODO); 향후 검증 추가 시 갱신 필요 | `Webhook`, `TgtCRD`, `VolCRD` |

---

### E21.4 CRD OpenAPI 스키마 검증 — 필드 범위/형식 위반 (Type C — envtest) ⚠️

**테스트 유형:** C (envtest 통합 테스트)

**빌드 태그:** `//go:build integration`

**실행 명령:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/ -v -run TestCRDSchema
```

**위치:** `internal/controller/pillartarget_controller_test.go` 또는 `internal/controller/pillarpool_controller_test.go`

**자동화 가능 여부:** ✅ CI 실행 가능 (`make setup-envtest` 후)

이 테스트들은 **실제 envtest API 서버**를 통해 CRD 오브젝트를 생성/패치하고,
`kubebuilder:validation` 마커가 생성한 OpenAPI v3 스키마 규칙이 실제로 동작함을 확인한다.
fake client와 달리 envtest API 서버는 CRD 스키마 검증을 실제로 수행한다.

> **⚠️ CI 실행 가능 여부 — 정직한 평가:**
> - ✅ `make setup-envtest` 후 표준 GitHub Actions에서 실행 가능
> - ✅ Docker/Kind/실제 하드웨어 불필요
> - ⚠️ envtest 바이너리(`kube-apiserver`, `etcd`) 다운로드 필요 (~100MB)
> - ⚠️ 일부 envtest 버전과 kubebuilder CRD OpenAPI v3 스키마 사이에 호환성 문제 발생 가능
> - ❌ fake client 기반 표준 단위 테스트에서는 이 검증이 실행되지 않음

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 163 | `TestCRDSchema_PillarTarget_NodeRefName_Empty` | `spec.nodeRef.name=""`인 PillarTarget 생성 시도 → `+kubebuilder:validation:MinLength=1` 위반 | envtest API 서버 실행 중; CRD 설치 완료 (`config/crd/bases/`); `PillarTarget{spec:{nodeRef:{name:""}}}` 준비 | 1) `k8sClient.Create(ctx, target)` 호출 | 422 Unprocessable Entity 반환; 오류에 `spec.nodeRef.name` 언급; 리소스 미생성 | `TgtCRD`, `API서버스키마` |
| 164 | `TestCRDSchema_PillarTarget_ExternalPort_Zero` | `spec.external.port=0` → `+kubebuilder:validation:Minimum=1` 위반 | envtest API 서버; `PillarTarget{spec:{external:{address:"1.2.3.4", port:0}}}` | 1) `k8sClient.Create(ctx, target)` 호출 | 422 반환; `spec.external.port` 최솟값(1) 미만 오류; 리소스 미생성 | `TgtCRD`, `API서버스키마` |
| 165 | `TestCRDSchema_PillarTarget_ExternalAddress_Empty` | `spec.external.address=""` → `+kubebuilder:validation:MinLength=1` 위반 | envtest API 서버; `PillarTarget{spec:{external:{address:"", port:9500}}}` | 1) `k8sClient.Create(ctx, target)` 호출 | 422 반환; `spec.external.address` 길이 오류; 리소스 미생성 | `TgtCRD`, `API서버스키마` |
| 166 | `TestCRDSchema_PillarTarget_NodeRefAddressType_Invalid` | `spec.nodeRef.addressType="FooType"` → `+kubebuilder:validation:Enum=InternalIP;ExternalIP` 위반 | envtest API 서버; `PillarTarget{spec:{nodeRef:{name:"n1", addressType:"FooType"}}}` | 1) `k8sClient.Create(ctx, target)` 호출 | 422 반환; `spec.nodeRef.addressType` Enum 위반 오류; "FooType" 불허, 허용값("InternalIP", "ExternalIP") 표시 | `TgtCRD`, `API서버스키마` |
| 167 | `TestCRDSchema_PillarPool_TargetRef_Empty` | `spec.targetRef=""`인 PillarPool 생성 → `+kubebuilder:validation:MinLength=1` 위반 | envtest API 서버; `PillarPool{spec:{targetRef:"", backend:{type:"zfs-zvol"}}}` | 1) `k8sClient.Create(ctx, pool)` 호출 | 422 반환; `spec.targetRef` 길이 오류; 리소스 미생성 | `TgtCRD`, `VolCRD`, `API서버스키마` |
| 168 | `TestCRDSchema_PillarPool_BackendType_Invalid` | `spec.backend.type="not-supported"` → `+kubebuilder:validation:Enum=zfs-zvol;zfs-dataset;lvm-lv;dir` 위반 | envtest API 서버; `PillarPool{spec:{targetRef:"t1", backend:{type:"not-supported"}}}` | 1) `k8sClient.Create(ctx, pool)` 호출 | 422 반환; `spec.backend.type` Enum 위반; 허용값(zfs-zvol, zfs-dataset, lvm-lv, dir) 표시 | `TgtCRD`, `VolCRD`, `API서버스키마` |
| 169 | `TestCRDSchema_PillarVolume_Phase_Invalid` | `status.phase="GarbagePhase"` — `+kubebuilder:validation:Enum=Provisioning;CreatePartial;Ready;...` 위반 | envtest API 서버; 유효한 PillarVolume 생성 완료; `status.phase="GarbagePhase"` 패치 시도 | 1) `k8sClient.Status().Patch(ctx, pv, client.MergeFrom(original))` 호출; phase를 "GarbagePhase"로 변경 | 422 반환; `status.phase` Enum 위반 오류; 기존 상태 유지됨 | `VolCRD`, `API서버스키마` |
| 170 | `TestCRDSchema_PillarVolume_CapacityBytes_Negative` | `spec.capacityBytes=-1` → `+kubebuilder:validation:Minimum=0` 위반 | envtest API 서버; `PillarVolume{spec:{volumeID:"t/p/b/v", agentVolumeID:"p/v", targetRef:"t1", backendType:"zfs-zvol", protocolType:"nvmeof-tcp", capacityBytes:-1}}` | 1) `k8sClient.Create(ctx, pv)` 호출 | 422 반환; `spec.capacityBytes` Minimum(0) 위반 오류; 리소스 미생성 | `VolCRD`, `API서버스키마` |

---

### E21 커버리지 요약

이 섹션이 커버하는 CR 유효성 검증 레이어를 정리한다.

| 검증 레이어 | 담당 소섹션 | 테스트 수 | CI 실행 |
|-----------|-----------|----------|--------|
| 컨트롤러 런타임 CR 상태 검증 | E21.1 | 6개 | ✅ 표준 CI |
| 웹훅 어드미션 — PillarTarget 불변 필드 | E21.2 | 7개 | ✅ envtest |
| 웹훅 어드미션 — PillarPool 불변 필드 | E21.3 | 5개 | ✅ envtest |
| OpenAPI CRD 스키마 — 필드 범위/형식 | E21.4 | 8개 | ✅ envtest |
| **합계** | | **26개** | ✅ 모두 CI 가능 |

**CI에서 테스트 불가 항목 (현재 구현 한계):**

| 검증 항목 | 이유 | 대안 |
|---------|------|------|
| PillarTarget `spec.nodeRef`와 `spec.external` 동시 설정 거부 | `ValidateCreate` 미구현(TODO 스캐폴딩); 현재 Create는 항상 허용 | Create 웹훅 구현 후 E21.2에 추가 |
| PillarTarget 삭제 시 참조 PillarPool/PillarBinding 보호 | `ValidateDelete` 미구현(TODO 스캐폴딩) | Delete 웹훅 구현 후 추가 |
| PillarPool 삭제 시 참조 PillarVolume 존재 여부 확인 | `ValidateDelete` 미구현 | Delete 웹훅 구현 후 추가 |
| 실제 Kubernetes admission controller 전체 플로우 | fake client/직접 호출 한계; webhook 서버와 kube-apiserver 간 HTTP 통신 미실행 | Kind 클러스터 테스트(K1) |

---

## E22: 비호환 백엔드-프로토콜 오류 시나리오 (Incompatible Backend-Protocol Error Scenarios)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능 (E22.1–E22.3) / ❌ CI 실행 불가 (E22.4)

CSI 컨트롤러 또는 Agent가 **현재 미지원 프로토콜 타입**, **알 수 없는 백엔드 타입**,
또는 **실제 배포 환경에서의 버전 불일치**를 처리할 때의 오류 전파 경로를 검증한다.

> **E14·E1.11과의 차이점:**
> - **E1.11 / E14** — StorageClass 파라미터 키 **자체가 누락**된 경우(`protocol-type` 키 없음) → `InvalidArgument`
> - **E22** — 파라미터 키는 존재하나 **에이전트가 지원하지 않는 값** 지정 (예: `"iscsi"`, `"nfs"`) 또는
>   실제 에이전트 바이너리와의 **버전·기능 불일치** → `Unimplemented` 또는 전파된 에이전트 오류

**오류 시나리오 분류:**

| 소섹션 | 테스트 유형 | CI 실행 | 핵심 시나리오 |
|--------|-----------|--------|------------|
| E22.1 | A (in-process) | ✅ 표준 CI | CSI Controller — StorageClass에 미지원 프로토콜 타입 지정 |
| E22.2 | A (in-process) | ✅ 표준 CI | Agent gRPC — 각 RPC에서 미지원 프로토콜 거부 |
| E22.3 | A (in-process) | ✅ 표준 CI | CSI Controller — StorageClass에 미지원 백엔드 타입 지정 |
| E22.4 | 수동/스테이징 | ❌ CI 불가 | 실제 버전 불일치·커널 모듈 미로드 시나리오 |

**아키텍처 (E22 전용):**
```
CSI Controller
        │
        │  StorageClass params: protocol-type="iscsi" (미지원)
        │       │
        │  mapProtocolType("iscsi") → PROTOCOL_TYPE_ISCSI (또는 "unknown" → UNSPECIFIED)
        │       │
        │  agent.ExportVolume(ProtocolType=ISCSI)
        │       │
        │       └─► agent.Server: "only NVMe-oF TCP is supported"
        │                   └─► codes.Unimplemented 반환
        │       │
        └───────────────────────────────────► CO에 비-OK 상태 전파

Agent gRPC Server (직접 호출 경로):
  ExportVolume(iSCSI)   → Unimplemented (configfs 사이드 이펙트 없음)
  AllowInitiator(iSCSI) → Unimplemented (nvmet/hosts 디렉터리 미생성)
  DenyInitiator(iSCSI)  → Unimplemented
  UnexportVolume(iSCSI) → Unimplemented
  ReconcileState(iSCSI export) → results[].success=false (타 볼륨 계속 처리)
```

---

### E22.1 CSI Controller — StorageClass 미지원 프로토콜 타입 지정

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**실행 명령:**
```bash
go test ./test/e2e/ -v -run TestCSIProtocol
```

**위치:** `test/e2e/csi_controller_e2e_test.go`

**핵심 동작:** CSI 컨트롤러는 `StorageClass`의 `protocol-type` 파라미터를 `mapProtocolType()`으로
agent protobuf 열거형으로 변환한다. 인식되지 않는 문자열은 `PROTOCOL_TYPE_UNSPECIFIED(0)`으로
매핑된다. agent 서버는 현재 `PROTOCOL_TYPE_NVMEOF_TCP` 외의 모든 프로토콜 타입에 대해
`codes.Unimplemented`를 반환한다 (`internal/agent/server_export.go:51-52`).

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 171 | `TestCSIProtocol_CreateVolume_ISCSIUnimplemented` | `protocol-type="iscsi"`로 CreateVolume 호출 시 agent.ExportVolume이 `codes.Unimplemented`("only NVMe-oF TCP is supported") 반환 → CSI 컨트롤러가 비-OK 상태 전파 | `mockAgentServer.ExportVolumeErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; StorageClass params에 `protocol-type: "iscsi"` 설정; PillarTarget CRD 등록; `agent.CreateVolume` 성공(백엔드 zvol 생성 후 export 단계에서 실패) | 1) `CreateVolumeRequest` 전송; 2) 반환 오류 gRPC 코드 확인 | 비-OK gRPC 상태(`codes.OK` 불가); agent의 `Unimplemented` 오류 전파; CreateVolume 실패 시 부분 생성된 zvol 정리 여부는 구현 의존 | `CSI-C`, `Agent`, `gRPC` |
| 172 | `TestCSIProtocol_CreateVolume_NFSUnimplemented` | `protocol-type="nfs"`로 CreateVolume 호출 시 agent.ExportVolume이 `codes.Unimplemented` 반환 | `mockAgentServer.ExportVolumeErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; StorageClass params에 `protocol-type: "nfs"` 설정; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송 | 비-OK gRPC 상태; NFS export 미지원으로 인한 오류 전파 | `CSI-C`, `Agent`, `gRPC` |
| 173 | `TestCSIProtocol_CreateVolume_UnknownProtocol_MapsToUnspecified` | `protocol-type="smb-v3-unknown"` — 알 수 없는 프로토콜 문자열이 `PROTOCOL_TYPE_UNSPECIFIED(0)`으로 매핑되어 agent에 전달됨 → agent가 Unimplemented 반환 | `mockAgentServer.ExportVolumeErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; StorageClass params에 `protocol-type: "smb-v3-unknown"` 설정 | 1) `CreateVolumeRequest` 전송; 2) `env.AgentMock.ExportVolumeCalls[0].ProtocolType` 값 확인 | 비-OK gRPC 상태; `ExportVolumeCalls[0].ProtocolType == PROTOCOL_TYPE_UNSPECIFIED` (UNSPECIFIED로 매핑 확인); agent Unimplemented 전파 | `CSI-C`, `Agent`, `gRPC` |
| 174 | `TestCSIProtocol_ControllerPublish_ISCSIUnimplemented` | ControllerPublishVolume에서 `protocol-type="iscsi"` 볼륨 ID를 가진 PillarVolume CRD 존재 시 agent.AllowInitiator가 `codes.Unimplemented` 반환 → ControllerPublishVolume이 오류 전파 | `mockAgentServer.AllowInitiatorErr = status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")`; PillarVolume CRD 존재(Phase=Ready, VolumeId에 `iscsi` 포함); PillarTarget CRD 등록 | 1) `ControllerPublishVolumeRequest`(NodeId=호스트 IQN) 전송 | 비-OK gRPC 상태; agent AllowInitiator Unimplemented 전파; 오류 은폐 없음 | `CSI-C`, `Agent`, `gRPC` |

---

### E22.2 Agent gRPC — 미지원 프로토콜 타입 거부 (RPC별)

**테스트 유형:** A (인프로세스, 컴포넌트 경계 테스트) ✅ CI 실행 가능

**실행 명령:**
```bash
# 기존 구현된 테스트
go test ./test/component/ -v -run 'TestAgentErrors_ExportVolume_InvalidProtocol|TestAgentErrors_AllowInitiator_InvalidProtocol|TestAgentErrors_DenyInitiator_InvalidProtocol|TestAgentErrors_UnexportVolume_InvalidProtocol'
# 신규 테스트
go test ./test/component/ -v -run 'TestAgentProtocol'
```

**위치:** `test/component/agent_errors_test.go`

**핵심 동작:** `agent.Server`는 각 export/initiator RPC에서 프로토콜 타입이
`PROTOCOL_TYPE_NVMEOF_TCP`가 아닌 경우 즉시 `codes.Unimplemented`와 함께
`"only NVMe-oF TCP is supported"` 메시지를 반환한다 (`internal/agent/server.go:39`의 `errOnlyNvmeofTCP` 상수).
이 검사는 모든 configfs 작업 **이전에** 수행되므로 사이드 이펙트가 없다.

> **기존 구현 테스트 참조:**
> `TestAgentErrors_ExportVolume_InvalidProtocol_NoConfigfsSideEffects` (iSCSI),
> `TestAgentErrors_AllowInitiator_InvalidProtocol` (iSCSI),
> `TestAgentErrors_DenyInitiator_InvalidProtocol` (iSCSI),
> `TestAgentErrors_UnexportVolume_InvalidProtocol` (iSCSI) —
> 이 4개 테스트는 **이미 구현되어 있으며** `test/component/agent_errors_test.go`에 존재한다.
> 아래 E22.2 표는 이들을 E2E 문맥에서 추적 가능하도록 정의하고,
> 추가적인 UNSPECIFIED 및 ReconcileState 시나리오를 보완한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 175 | `TestAgentErrors_ExportVolume_InvalidProtocol_NoConfigfsSideEffects` *(기존)* | `ExportVolume`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 및 configfs 사이드 이펙트 없음 — `server_export.go:51` 경계 검사 동작 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend`; `AlwaysPresentChecker` | 1) `ExportVolumeRequest{ProtocolType=ISCSI, VolumeId=compTestVolumeID}` 전송; 2) `configfsRoot/nvmet` 디렉터리 존재 여부 확인 | `codes.Unimplemented`; `nvmet` 디렉터리 미생성(configfs 사이드 이펙트 없음) | `Agent`, `NVMeF` |
| 176 | `TestAgentProtocol_ExportVolume_UNSPECIFIED_Unimplemented` | `ExportVolume`에 `PROTOCOL_TYPE_UNSPECIFIED(0)` 지정 시 `codes.Unimplemented` 반환 — `mapProtocolType`이 알 수 없는 문자열을 UNSPECIFIED로 변환하는 엔드투엔드 경로 커버 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `ExportVolumeRequest{ProtocolType=PROTOCOL_TYPE_UNSPECIFIED}` 전송; 2) configfs 사이드 이펙트 확인 | `codes.Unimplemented`; configfs 미수정; 오류 메시지에 "only NVMe-oF TCP is supported" 포함 | `Agent` |
| 177 | `TestAgentErrors_AllowInitiator_InvalidProtocol` *(기존)* | `AllowInitiator`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 및 `nvmet/hosts` 디렉터리 미생성 — `server_export.go:163` 경계 검사 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `AllowInitiatorRequest{ProtocolType=ISCSI, VolumeId, InitiatorId=compTestHostNQN}` 전송; 2) `nvmet/hosts` 디렉터리 존재 확인 | `codes.Unimplemented`; `nvmet/hosts` 디렉터리 미생성 | `Agent`, `NVMeF` |
| 178 | `TestAgentErrors_DenyInitiator_InvalidProtocol` *(기존)* | `DenyInitiator`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 — `server_export.go:146` 경계 검사 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `DenyInitiatorRequest{ProtocolType=ISCSI, VolumeId, InitiatorId=compTestHostNQN}` 전송 | `codes.Unimplemented` | `Agent` |
| 179 | `TestAgentErrors_UnexportVolume_InvalidProtocol` *(기존)* | `UnexportVolume`에 `PROTOCOL_TYPE_ISCSI` 지정 시 `codes.Unimplemented` 반환 — 존재하지 않는 iSCSI 서브시스템 삭제 시도 없음; `server_export.go:129` 경계 검사 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend` | 1) `UnexportVolumeRequest{ProtocolType=ISCSI, VolumeId}` 전송 | `codes.Unimplemented`; configfs 미수정 | `Agent` |
| 180 | `TestAgentProtocol_ReconcileState_UnsupportedProtocol_SkipAndReport` | `ReconcileState`에 NVMe-oF TCP 이외 프로토콜 엔트리 포함 시 해당 항목 `success=false`로 보고하고, NVMe-oF TCP 항목은 정상 처리 — `server_reconcile.go:72` 프로토콜 타입 검사 동작 | `agent.NewServer(backends, t.TempDir())`; `mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-mixed"}`; 볼륨 2개 포함 `ReconcileStateRequest`: `v1`(NVMe-oF TCP 수출, `AllowedInitiators=[hostNQN]`), `v2`(iSCSI 수출) | 1) `ReconcileState({volumes: [v1(NVMeOF), v2(ISCSI)]})` 호출; 2) `results` 슬라이스 검사; 3) configfs 서브시스템 디렉터리 확인 | `results[v1].Success=true`; `results[v2].Success=false`; `results[v2].ErrorMessage` 비어 있지 않음; `tmpdir/nvmet/subsystems/<NQN>` 생성됨(v1 처리 성공); 패닉 없음 | `Agent`, `NVMeF`, `gRPC` |

---

### E22.3 CSI Controller — StorageClass 미지원 백엔드 타입 지정

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**실행 명령:**
```bash
go test ./test/e2e/ -v -run TestCSIProtocol
```

**위치:** `test/e2e/csi_controller_e2e_test.go`

**핵심 동작:** CSI 컨트롤러는 `StorageClass`의 `backend-type` 파라미터를 `mapBackendType()`으로
agent protobuf 열거형으로 변환한다. 인식되지 않는 문자열(예: `"lvm-unknown"`, `"fuse"`)은
`BACKEND_TYPE_UNSPECIFIED(0)`으로 매핑되어 agent에 전달된다
(`internal/csi/controller.go`의 `mapBackendType` default 분기).

> **E1.11-5와의 차이점:**
> - **E1.11-5** (`TestCSIController_CreateVolume_MissingBackendTypeParam`): `backend-type` 키 자체 **누락** → `InvalidArgument` (agent 호출 전 검증)
> - **E22.3** (신규): `backend-type` 키는 **존재**하지만 알 수 없는 값 → `BACKEND_TYPE_UNSPECIFIED`으로 매핑 → agent에 전달되어 agent의 동작에 위임

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 181 | `TestCSIProtocol_CreateVolume_UnknownBackendType_MapsToUnspecified` | `backend-type="fuse-experimental"` — 알 수 없는 백엔드 타입 문자열이 `BACKEND_TYPE_UNSPECIFIED(0)`으로 매핑되어 `agent.CreateVolume` 요청에 전달됨 | `mockAgentServer` 기본 설정(CreateVolume 성공 반환); StorageClass params에 `backend-type: "fuse-experimental"` 설정; PillarTarget CRD 등록; `protocol-type: "nvmeof-tcp"` | 1) `CreateVolumeRequest` 전송; 2) `env.AgentMock.CreateVolumeCalls[0].BackendType` 값 확인 | `CreateVolumeCalls[0].BackendType == BACKEND_TYPE_UNSPECIFIED` (UNSPECIFIED로 매핑 확인); CreateVolume 자체는 mock 기준 성공 반환; 감사 목적 — UNSPECIFIED 백엔드 타입이 agent에 도달함을 문서화 | `CSI-C`, `Agent` |
| 182 | `TestCSIProtocol_CreateVolume_LVMBackendUnimplemented` | `backend-type="lvm"`으로 CreateVolume 호출 시 agent.CreateVolume이 `codes.Unimplemented` 반환 — 현재 단일 ZFS 스토리지 노드에서 LVM 백엔드를 지원하지 않는 시나리오 | `mockAgentServer.CreateVolumeErr = status.Errorf(codes.Unimplemented, "LVM backend not supported in this deployment")`; StorageClass params에 `backend-type: "lvm"` 설정; PillarTarget CRD 등록 | 1) `CreateVolumeRequest` 전송 | 비-OK gRPC 상태; agent의 `Unimplemented` 전파; PillarVolume CRD 미생성 | `CSI-C`, `Agent`, `gRPC` |

---

### E22.4 CI에서 검증 불가 — 실제 버전 불일치 및 커널 모듈 미로드 시나리오

> **⚠️ CI 실행 불가** — 실제 pillar-agent 바이너리 배포, 실제 커널 모듈 로드/언로드,
> 실제 Kubernetes 클러스터가 필요하다. 수동 스테이징 환경에서만 검증 가능.

**자동화 불가 이유:**
- `GetCapabilitiesResponse.agent_version` 필드는 실제 에이전트 바이너리에서만 의미 있는 값을 반환한다.
- 커널 모듈(`nvmet`, `nvme-fabrics`) 로드/언로드는 root 권한과 실제 커널이 필요하다.
- 컨트롤러-에이전트 간 실제 gRPC 연결과 프로토콜 협상은 실제 네트워크와 mTLS 인증이 필요하다.
- `GetCapabilitiesResponse.supported_protocols` 필드를 기반으로 한 프로토콜 사전 검증 로직이
  현재 CSI 컨트롤러에 **구현되어 있지 않다** — 버전 협상은 향후 구현 예정이다.

| ID | 시나리오 | 사전 조건 | 수동 실행 절차 | 허용 기준 | 커버리지 |
|----|---------|----------|--------------|---------|---------|
| BP-1 | **Controller-Agent 에이전트 버전 확인 — `GetCapabilitiesResponse.agent_version` 필드 기록 여부** | 실제 Kubernetes 클러스터; pillar-csi-controller 배포; pillar-agent 배포 (`agent_version="0.1.0"` 내장, `internal/agent/server.go:36` 상수) | 1) PillarTarget CRD 등록 후 컨트롤러 재조정 대기; 2) `kubectl get pillartarget <name> -o yaml`로 `status.agentVersion` 또는 관련 조건 메시지 확인; 3) 에이전트 바이너리를 이전 버전으로 교체 후 컨트롤러 반응 확인 | `PillarTarget.status` 또는 이벤트에 에이전트 버전 정보 기록됨; 버전 불일치 경고는 현재 미구현(향후 구현 예정); 버전 불일치 시에도 볼륨 생성 시도 가능 — 미지원 RPC 호출 시 `Unimplemented` 반환으로 오류 감지 | `Agent`, `TgtCRD`, `gRPC` |
| BP-2 | **스토리지 노드에서 nvmet 커널 모듈 미로드 — HealthCheck 경고 및 ExportVolume 실패** | 실제 스토리지 노드; ZFS 커널 모듈 로드됨; nvmet/nvme-fabrics 모듈 **미로드** (`modprobe -r nvmet nvme-fabrics`) | 1) pillar-agent 프로세스 시작; 2) `agent.HealthCheck()` 응답의 `subsystems` 배열 확인 — `nvmet-configfs` 서브시스템 `healthy` 필드 값 확인; 3) PVC 생성 시도(CSI CreateVolume → `agent.CreateVolume` 성공 → `agent.ExportVolume` 실패 예상); 4) `kubectl describe pvc`에서 오류 이벤트 확인 | `HealthCheck` 응답에 `nvmet-configfs.healthy=false` 표시; `ExportVolume` 호출 시 configfs 디렉터리 생성 실패로 `codes.Internal` 또는 `codes.FailedPrecondition` 반환; PVC가 `Pending` 상태 유지; 오류 메시지에 configfs 관련 진단 정보 포함 | `Agent`, `NVMeF`, `TgtCRD` |
| BP-3 | **프로토콜 협상 실패 엔드투엔드 — StorageClass `protocol-type: iscsi`로 PVC 생성 시 오류 전파** | 실제 Kubernetes 클러스터; StorageClass `protocol-type: iscsi`로 구성; 실제 pillar-agent 배포 (NVMe-oF TCP 전용) | 1) `kubectl apply -f storageclass-iscsi.yaml`; 2) `kubectl apply -f pvc-iscsi.yaml`; 3) PVC 이벤트 확인 (`kubectl describe pvc <name>`); 4) CSI 컨트롤러 로그에서 `Unimplemented` 오류 확인 | PVC가 `Pending` 상태 유지; CSI CreateVolume 오류 이벤트에 `Unimplemented: only NVMe-oF TCP is supported` 메시지; 지속적인 재시도 없이 명확한 오류 보고; PillarVolume CRD 미생성 | `CSI-C`, `Agent`, `gRPC`, `실제 Kubernetes클러스터` |
| BP-4 | **향후 iSCSI 지원 추가 시 회귀 검증 체크리스트** | iSCSI 지원 버전의 pillar-agent 배포 후; LIO 커널 모듈 로드됨 (`iscsi_target_mod`, `target_core_mod`, `configfs`) | 1) StorageClass에 `protocol-type: "iscsi"` 설정; 2) PVC 생성; 3) `kubectl describe pvc`로 `Bound` 확인; 4) 스토리지 노드에서 `targetcli ls` 실행하여 iSCSI 타깃 생성 확인 | PVC `Bound` 상태; iSCSI LIO 타깃 생성 확인; **현재 E22.1 테스트(171-172)가 `Unimplemented` 예상에서 `OK` 예상으로 갱신 필요**; `TestAgentErrors_*_InvalidProtocol` 시리즈 삭제 또는 프로토콜 목록 업데이트 필요 | `Agent`, `CSI-C`, `실제 커널`, `실제 Kubernetes클러스터` |

---

### E22 커버리지 요약

| 소섹션 | 검증 내용 | 테스트 수 | CI 실행 |
|--------|---------|----------|--------|
| E22.1 | CSI Controller → agent 미지원 프로토콜 전파 | 4개 | ✅ 표준 CI |
| E22.2 | Agent 서버 각 RPC에서 미지원 프로토콜 거부 (기존 4개 + 신규 2개) | 6개 | ✅ 표준 CI |
| E22.3 | CSI Controller → agent 미지원 백엔드 타입 전파 | 2개 | ✅ 표준 CI |
| E22.4 | 실제 버전 불일치·커널 모듈 미로드 수동 검증 | 4개 시나리오 | ❌ CI 불가 |
| **합계** | | **12개 자동 + 4개 수동** | — |

**CI에서 검증 불가 항목 (정직한 평가):**

| 항목 | 이유 | 대안 |
|------|------|------|
| 실제 iSCSI 프로토콜 협상 | LIO 커널 모듈 + root 권한 필요 | BP-3 수동 스테이징 검증 |
| 실제 NFS 마운트 익스포트 | `nfsd` 커널 서비스 + root 권한 필요 | 수동 스테이징 검증 |
| `agent_version` 필드 기반 버전 체크 | 현재 컨트롤러에 버전 검증 로직 **미구현** | BP-1 수동 확인; 버전 체크 구현 후 E22.1에 추가 |
| `GetCapabilitiesResponse.supported_protocols` 사전 검증 | 컨트롤러가 CreateVolume 전 지원 프로토콜 목록을 확인하지 않음 | 향후 사전 검증 로직 추가 후 E22.1에 `InvalidArgument` 테스트 추가 |
| gRPC 스트리밍 RPC 프로토콜 협상 (`SendVolume`, `ReceiveVolume`) | 멀티-청크 스트리밍 + 실제 ZFS send/recv 필요 | 유형 F 완전 E2E 테스트 |

---

## E24: 8단계 전체 라이프사이클 통합 시나리오 (Full Lifecycle Integration)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

**위치:** `test/e2e/csi_lifecycle_e2e_test.go`, `test/e2e/csi_partial_failure_e2e_test.go`, `test/e2e/csi_zvol_nodup_e2e_test.go`

**실행 명령:**
```bash
go test ./test/e2e/ -v -run "TestCSILifecycle|TestCSIOrdering|TestCSIController_PartialFailure|TestCSIZvolNoDup"
```

이 섹션은 CSI 볼륨 라이프사이클의 **8단계 전체 체인**에서 부분 실패 발생 시의 동작을 통합적으로 검증한다.

```
CreateVolume → ControllerPublish → NodeStage → NodePublish →
NodeUnpublish → NodeUnstage → ControllerUnpublish → DeleteVolume
```

각 단계에서 실패가 발생했을 때:
1. 오류가 CO(Container Orchestrator)로 올바르게 전파되는가?
2. 시스템이 일관된 상태를 유지하는가?
3. 재시도 시 멱등성이 보장되는가?
4. 롤백 또는 정리 경로가 올바르게 동작하는가?

---

### E24.1 정상 경로 — 8단계 완전 체인

**설명:** 모든 컴포넌트가 정상 동작할 때 8단계 라이프사이클이 순서대로 성공적으로 완료됨을 검증한다.

> **CI 실행 가능 여부:** ✅ 인프로세스 E2E — 별도 인프라 불필요

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.1-1 | `TestCSILifecycle_FullCycle` _(기존 구현)_ | 8단계 전체 라이프사이클 정상 경로 완전 검증. `csiLifecycleEnv`를 통해 ControllerServer와 NodeServer가 단일 mockAgentServer를 공유하며 전체 체인을 인프로세스로 실행 | `csiLifecycleEnv` 초기화: `mockAgentServer`(ExportVolumeInfo 사전 설정), `mockCSIConnector`(DevicePath=`/dev/nvme0n1`), `mockCSIMounter`, `t.TempDir()` StateDir; PillarTarget CRD 등록 | 1) `CreateVolumeRequest{Name="pvc-lifecycle-full", CapacityRange=1GiB, Parameters{target, backend-type=zfs-zvol, protocol-type=nvmeof-tcp, zfs-pool}}` 전송; 2) `ControllerPublishVolumeRequest{VolumeId, NodeId="worker-1"}` 전송; 3) `NodeStageVolumeRequest{VolumeId, StagingTargetPath, VolumeContext}` 전송; 4) `NodePublishVolumeRequest{VolumeId, StagingTargetPath, TargetPath}` 전송; 5) `NodeUnpublishVolumeRequest{VolumeId, TargetPath}` 전송; 6) `NodeUnstageVolumeRequest{VolumeId, StagingTargetPath}` 전송; 7) `ControllerUnpublishVolumeRequest{VolumeId, NodeId}` 전송; 8) `DeleteVolumeRequest{VolumeId}` 전송 | 모든 단계 성공; `VolumeContext`(NQN, address, port)가 CreateVolume → NodeStageVolume으로 키 변환 없이 전달; `agent.CreateVolume` 1회 · `agent.ExportVolume` 1회 · `agent.AllowInitiator` 1회 · `agent.DenyInitiator` 1회 · `agent.UnexportVolume` 1회 · `agent.DeleteVolume` 1회; `mockConnector.Connect` 1회 · `mockConnector.Disconnect` 1회; PillarVolume CRD 삭제됨(NotFound) | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `TgtCRD`, `VolCRD`, `State`, `gRPC` |
| E24.1-2 | `TestCSILifecycle_VolumeContextFlowThrough` _(기존 구현)_ | CreateVolume의 VolumeContext(NQN/address/port)가 키 변환 없이 NodeStageVolume의 `mockConnector.Connect` 인수로 전달됨을 검증. 컨트롤러와 노드 서버가 VolumeContext 키 이름에 합의(no translation)되어 있어야 함 | `csiLifecycleEnv`; `mockAgentServer.ExportVolumeInfo`: `TargetId=lifecycleTestNQN`, `Address=127.0.0.1`, `Port=4420` | 1) `CreateVolumeRequest` 전송; 2) `VolumeContext` 추출; 3) `NodeStageVolumeRequest{VolumeContext: 그대로 전달}` 전송; 4) `mockConnector.ConnectCalls[0]` 검증 | `mockConnector.Connect.SubsysNQN == VolumeContext["target_id"]`; `TrAddr == VolumeContext["address"]`; `TrSvcID == VolumeContext["port"]`; 키 변환 없음 확인 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `gRPC` |
| E24.1-3 | `TestCSILifecycle_OrderingConstraints` _(기존 구현)_ | 8단계 체인에서 올바른 순서 준수: 각 단계 완료 후 다음 단계 진행 시 모든 agent RPC가 정확히 1회씩 호출됨 | 동일한 `csiLifecycleEnv`; 각 단계를 Phase 1~8로 명시 | Phase 1: CreateVolume; Phase 2: ControllerPublish; Phase 3: NodeStage; Phase 4: NodePublish; Phase 5: NodeUnpublish; Phase 6: NodeUnstage; Phase 7: ControllerUnpublish; Phase 8: DeleteVolume — 각 Phase 후 중간 상태 검증 | Phase 3 후: `mockConnector.ConnectCalls` 1개; Phase 4 후: `targetPath` 마운트됨; Phase 5 후: `targetPath` 언마운트됨 · `stagingPath` 유지; Phase 6 후: `stagingPath` 언마운트됨 · `mockConnector.DisconnectCalls` 1개; 최종: 6개 agent RPC 각 1회 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `gRPC` |
| E24.1-4 | `TestCSILifecycle_IdempotentSteps` _(기존 구현)_ | 8단계 각 단계를 두 번씩 동일 인수로 호출해도 오류 없이 최종 상태 동일 — CSI 명세의 멱등성 요구 통합 검증 | `csiLifecycleEnv` 초기화; `callTwice` 헬퍼 함수 사용 | 각 단계 `callTwice(step, fn)` — CreateVolume 2회 · ControllerPublish 2회 · NodeStage 2회 · NodePublish 2회 · NodeUnpublish 2회 · NodeUnstage 2회 · ControllerUnpublish 2회 · DeleteVolume 2회 | 모든 재호출 성공; 오류 없음; 두 번째 호출은 no-op 처리 | `CSI-C`, `CSI-N`, `Agent`, `Conn`, `Mnt`, `SM`, `gRPC` |

---

### E24.2 CreateVolume 단계 실패/복구

**설명:** CreateVolume 단계에서 부분 실패(백엔드 생성 성공 + 익스포트 실패)가 발생했을 때 시스템 상태와 복구 경로를 검증한다. 이는 E6.1의 상위 통합 뷰를 제공한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.2-1 | `TestCSIController_PartialFailure_CRDCreatedOnExportFailure` | agent.CreateVolume 성공 + agent.ExportVolume 실패 시 PillarVolume CRD Phase=CreatePartial | mockAgentServer: ExportVolumeErr 설정; PillarTarget 등록 | 1) CreateVolumeRequest 전송 | 오류 반환 (CO 재시도 트리거); CRD Phase=CreatePartial; BackendCreated=true; FailedOperation="ExportVolume"; ExportInfo=nil | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.2-2 | `TestCSIController_PartialFailure_RetryAdvancesToReady` | 부분 실패 후 재시도 시 CRD Phase=Ready 전환 및 ExportInfo 채워짐 | Phase=CreatePartial CRD 존재; ExportVolume 이번엔 성공 | 1) 동일 인수로 CreateVolumeRequest 재전송 | 성공; CRD Phase=Ready; ExportInfo(TargetID, Address) 채워짐; PartialFailure=nil | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.2-3 | `TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry` | skipBackend 최적화 — 재시도 시 agent.CreateVolume 재호출 없음 | Phase=CreatePartial CRD 존재; skipBackend 활성화 조건 충족 | 1) CreateVolumeRequest 재전송; 2) agent 호출 횟수 검증 | agent.CreateVolume 총 1회 (재시도 포함); agent.ExportVolume 총 2회 | `CSI-C`, `Agent`, `VolCRD`, `gRPC`, `SM` |
| E24.2-4 | `TestCSIZvolNoDup_ExactlyOneZvolAfterExportFailureRetry` | export 실패 후 재시도 시 zvol 중복 생성 없음 (E6.3-1 상위 통합 뷰) | `statefulZvolAgentServer`; ExportVolumeErr 주입/제거 | 1) CreateVolume(실패); 2) zvol 수=1 확인; 3) CreateVolume(성공); 4) zvol 수=1 유지 확인 | zvol 총 1개; skipBackend 발동; CRD Phase=Ready | `CSI-C`, `Agent`, `VolCRD`, `gRPC`, `SM` |

---

### E24.3 ControllerPublish 단계 실패/복구

**설명:** ControllerPublishVolume에서 AllowInitiator 호출 실패 시 오류 전파 및 멱등성을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.3-1 | `TestCSIController_ControllerPublishVolume_AgentAllowInitiatorFails` | AllowInitiator 실패 시 ControllerPublishVolume이 오류 반환 | mockAgentServer: AllowInitiatorErr=gRPC Internal; 유효한 VolumeId/NodeId | 1) ControllerPublishVolumeRequest 전송 | 비-OK gRPC 상태; PublishContext 없음 | `CSI-C`, `Agent`, `gRPC` |
| E24.3-2 | `TestCSIPublishIdempotency_ControllerPublishVolume_DoubleSameArgs` | 동일 인수로 ControllerPublishVolume 2회 호출 시 멱등 성공 (E7.1 상위 통합 뷰) | mockAgentServer 정상; 동일 VolumeId/NodeId | 1) ControllerPublishVolume; 2) 동일 인수로 재호출 | 두 호출 모두 성공; PublishContext 동일; AllowInitiator 2회 (CSI 레이어는 중복 제거 안 함) | `CSI-C`, `Agent`, `gRPC` |

---

### E24.4 NodeStage 단계 실패/복구

**설명:** NodeStageVolume에서 NVMe-oF 연결 실패 또는 포맷 실패 시 오류 전파와 재시도 가능성을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.4-1 | `TestCSINode_NodeStageVolume_ConnectFails` | NVMe-oF 연결 실패 시 NodeStageVolume이 오류 반환 | mockCSIConnector: ConnectErr 설정; 유효한 VolumeContext(NQN, address) | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; 스테이징 디렉터리 미생성 또는 정리됨 | `CSI-N`, `Conn` |
| E24.4-2 | `TestCSINode_NodeStageVolume_FormatFails` | 디바이스 포맷 실패 시 NodeStageVolume이 오류 반환 | mockCSIMounter: FormatAndMountErr 설정; mockCSIConnector 정상 | 1) NodeStageVolumeRequest 전송 | 비-OK gRPC 상태; Connect는 성공 후 포맷 실패 | `CSI-N`, `Conn`, `Mnt` |
| E24.4-3 | `TestCSINode_NodeStageVolume_IdempotentReStage` | 이미 스테이징된 볼륨을 다시 NodeStageVolume 호출 시 멱등 성공 | 스테이징 완료 상태; 동일 StagingTargetPath | 1) NodeStageVolume 재호출 | 성공; 연결/포맷 재시도 없음 (이미 마운트됨) | `CSI-N`, `Conn`, `Mnt` |

---

### E24.5 NodePublish 단계 실패/복구

**설명:** NodePublishVolume에서 바인드 마운트 실패 시 오류 전파와 멱등성을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.5-1 | `TestCSINode_NodePublishVolume_MountFails` | 바인드 마운트 실패 시 NodePublishVolume이 오류 반환 | NodeStageVolume 완료; mockCSIMounter: MountErr 설정 | 1) NodePublishVolumeRequest 전송 | 비-OK gRPC 상태; TargetPath 마운트 안 됨 | `CSI-N`, `Mnt` |
| E24.5-2 | `TestCSIPublishIdempotency_NodePublishVolume_DoubleSameTarget` | NodePublishVolume 2회 호출 시 두 번째는 no-op (E7.2 상위 통합 뷰) | NodeStageVolume 완료; 동일 TargetPath | 1) NodePublishVolume; 2) 동일 인수로 재호출 | 두 호출 모두 성공; Mount 1회 실행; 두 번째 호출은 이미 마운트됨 감지 | `CSI-N`, `Mnt` |
| E24.5-3 | `TestCSIPublishIdempotency_NodePublishVolume_ReadonlyDouble` | 읽기 전용 NodePublishVolume 2회 호출 멱등성 (E7.2 상위 통합 뷰) | NodeStageVolume 완료; Readonly=true | 1) NodePublishVolume(ro); 2) 동일 인수로 재호출 | 두 호출 모두 성공; "ro" 옵션 포함 마운트 1회만; 두 번째는 no-op | `CSI-N`, `Mnt` |

---

### E24.6 NodeUnpublish 단계 실패/복구

**설명:** NodeUnpublishVolume에서 언마운트 실패 또는 이미 언마운트된 상태의 멱등성을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.6-1 | `TestCSINode_NodeUnpublishVolume_UnmountFails` | 언마운트 실패 시 NodeUnpublishVolume이 오류 반환 | NodePublishVolume 완료; mockCSIMounter: UnmountErr 설정 | 1) NodeUnpublishVolumeRequest 전송 | 비-OK gRPC 상태 | `CSI-N`, `Mnt` |
| E24.6-2 | `TestCSINode_NodeUnpublishVolume_AlreadyUnpublished` | 이미 언마운트된 TargetPath에 NodeUnpublishVolume 멱등 성공 | TargetPath 이미 언마운트 또는 미존재; CSI 명세 상 idempotent 요구 | 1) NodeUnpublishVolumeRequest 전송 | 성공 (NotFound는 오류가 아닌 멱등 완료로 처리) | `CSI-N`, `Mnt` |

---

### E24.7 NodeUnstage 단계 실패/복구

**설명:** NodeUnstageVolume에서 NVMe-oF 연결 해제 실패 또는 이미 언스테이징된 상태의 멱등성을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.7-1 | `TestCSINode_NodeUnstageVolume_DisconnectFails` | NVMe-oF 연결 해제 실패 시 NodeUnstageVolume이 오류 반환 | NodeStageVolume 완료; NodeUnpublishVolume 완료; mockCSIConnector: DisconnectErr 설정 | 1) NodeUnstageVolumeRequest 전송 | 비-OK gRPC 상태; StagingTargetPath 정리 미완료 | `CSI-N`, `Conn` |
| E24.7-2 | `TestCSINode_NodeUnstageVolume_AlreadyUnstaged` | 이미 언스테이징된 볼륨에 NodeUnstageVolume 멱등 성공 | StagingTargetPath 이미 미마운트/미존재; CSI 명세 상 idempotent 요구 | 1) NodeUnstageVolumeRequest 전송 | 성공 | `CSI-N`, `Conn`, `Mnt` |

---

### E24.8 ControllerUnpublish 단계 실패/복구

**설명:** ControllerUnpublishVolume에서 DenyInitiator 호출 실패 시 오류 전파와 멱등성을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.8-1 | `TestCSIController_ControllerUnpublishVolume_AgentDenyInitiatorFails` | DenyInitiator 실패 시 ControllerUnpublishVolume이 오류 반환 | mockAgentServer: DenyInitiatorErr=gRPC Internal; 유효한 VolumeId/NodeId | 1) ControllerUnpublishVolumeRequest 전송 | 비-OK gRPC 상태 | `CSI-C`, `Agent`, `gRPC` |
| E24.8-2 | `TestCSIController_ControllerUnpublishVolume_NotFound` | 존재하지 않는 볼륨에 ControllerUnpublishVolume 시 NotFound 또는 성공 | 유효하지 않은 VolumeId; mockAgentServer: DenyInitiatorErr=NotFound | 1) ControllerUnpublishVolumeRequest 전송 | CSI 명세 상 성공 또는 NotFound 허용 (idempotent) | `CSI-C`, `Agent`, `gRPC` |

---

### E24.9 DeleteVolume 단계 실패/복구

**설명:** DeleteVolume에서 agent.DeleteVolume 실패 시 오류 전파와 재시도 가능성을 검증한다. 부분 생성 상태의 볼륨 삭제도 포함한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.9-1 | `TestCSIController_DeleteVolume_AgentDeleteVolumeFailsTransient` | agent.DeleteVolume 일시적 실패 시 DeleteVolume이 오류 반환 (CO 재시도 허용) | CreateVolume 성공; mockAgentServer: DeleteVolumeErr=gRPC Internal | 1) DeleteVolumeRequest 전송 | 비-OK gRPC 상태; PillarVolume CRD 미제거 (롤백 없음, 재시도 대기) | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.9-2 | `TestCSIController_DeleteVolume_CleansUpCRD` | 성공적인 DeleteVolume이 PillarVolume CRD를 제거 (E6.2 상위 통합 뷰) | CreateVolume 성공; PillarVolume CRD Phase=Ready | 1) DeleteVolumeRequest 전송; 2) CRD 조회 | 성공; CRD NotFound | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.9-3 | `TestCSIController_PartialFailure_DeleteVolumeOnPartialCreates` | CreatePartial 상태 볼륨의 DeleteVolume 성공 및 CRD 정리 (E6.2 상위 통합 뷰) | PillarVolume CRD Phase=CreatePartial; BackendCreated=true | 1) DeleteVolumeRequest 전송; 2) CRD 조회 | 성공; agent.DeleteVolume 호출 (BackendCreated=true이므로); CRD NotFound | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |
| E24.9-4 | `TestCSIZvolNoDup_ZvolRegistryReflectsDeleteAfterPartialCreate` | 부분 생성 상태 DeleteVolume 후 zvol 레지스트리 정확한 1→0 감소 (E6.3 상위 통합 뷰) | `statefulZvolAgentServer`; CreatePartial 상태 | 1) DeleteVolume; 2) zvol 수 확인; 3) CRD 확인 | zvol 0개; CRD NotFound | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

### E24.10 중단된 라이프사이클 정리 경로

**설명:** 라이프사이클 중간에 중단이 발생했을 때 적절한 정리(cleanup)가 이루어지는지 검증한다.
이는 쿠버네티스 파드 삭제, PVC 삭제, 노드 장애 등의 시나리오에서 발생한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E24.10-1 | `TestCSILifecycle_OutOfOrderOperationsDetected` | 순서 제약 위반 (NodeStage 전 NodePublish 등) 탐지 및 FailedPrecondition 반환 | SM 초기화; 정상 스테이지 미완료 상태 | 1) NodePublishVolume(NodeStageVolume 없이); 2) 오류 코드 확인 | `codes.FailedPrecondition`; 시스템 상태 변경 없음 | `CSI-N`, `SM` |
| E24.10-2 | `TestCSIController_DeleteVolume_NonExistentVolume` | 존재하지 않는 VolumeId에 DeleteVolume 멱등 성공 | VolumeId가 CRD에 없음; agent에도 없음 | 1) DeleteVolumeRequest 전송 | 성공 (CSI 명세: 없는 볼륨 삭제는 성공으로 처리) | `CSI-C`, `Agent`, `gRPC` |

---

### E24 커버리지 요약

| 소섹션 | 테스트 수 | 검증 내용 | 신규/참조 | CI 실행 |
|--------|---------|---------|----------|--------|
| E24.1 | 4개 | 8단계 정상 경로 완전 체인 (FullCycle), VolumeContext 전파, 순서 준수, 멱등성 | 기존 구현 참조 (E4.1·E4.3·E4.4) | ✅ 표준 CI |
| E24.2 | 4개 | CreateVolume 부분 실패/복구 (E6 통합 뷰) | E6.1 참조 | ✅ 표준 CI |
| E24.3 | 2개 | ControllerPublish 실패/멱등성 (E7 통합 뷰) | E7.1 참조 + 신규 | ✅ 표준 CI |
| E24.4 | 3개 | NodeStage 연결/포맷 실패/멱등성 | 신규 | ✅ 표준 CI |
| E24.5 | 3개 | NodePublish 마운트 실패/멱등성 (E7 통합 뷰) | E7.2 참조 + 신규 | ✅ 표준 CI |
| E24.6 | 2개 | NodeUnpublish 언마운트 실패/멱등성 | 신규 | ✅ 표준 CI |
| E24.7 | 2개 | NodeUnstage 연결 해제 실패/멱등성 | 신규 | ✅ 표준 CI |
| E24.8 | 2개 | ControllerUnpublish 실패/멱등성 | 신규 | ✅ 표준 CI |
| E24.9 | 4개 | DeleteVolume 실패/부분 생성 정리 (E6 통합 뷰) | E6.2 참조 + 신규 | ✅ 표준 CI |
| E24.10 | 2개 | 중단된 라이프사이클 정리 경로 | 신규 | ✅ 표준 CI |
| **합계** | **28개** | | | ✅ |

> **참고:** E24.1 항목 4개는 기존 구현된 테스트(`TestCSILifecycle_FullCycle`, `TestCSILifecycle_VolumeContextFlowThrough`, `TestCSILifecycle_OrderingConstraints`, `TestCSILifecycle_IdempotentSteps`)를 8단계 체인 통합 관점에서 재명세한 것이다. E24.2~E24.9의 일부 항목도 기존 E6·E7 테스트를 통합 뷰로 참조한다. **순수 신규 명세 테스트: 약 12개.**

**CI에서 검증 불가 항목 (정직한 평가):**

| 항목 | 이유 | 대안 |
|------|------|------|
| 실제 노드 장애 시 NodeStage/NodePublish 자동 정리 | 실제 Kubernetes 노드 제거 + Volume Attachment 삭제 필요 | Kind 클러스터 E2E (유형 B) 또는 수동 스테이징 |
| PVC 삭제 → CSI DeleteVolume 전체 플로우 | external-provisioner + 실제 Kubernetes API 서버 필요 | Kind 클러스터 E2E (유형 B: E10) |
| agent 재시작 후 ExportVolume 상태 자동 복원 | 실제 프로세스 재시작 + ReconcileState 호출 필요 | 수동 스테이징 또는 유형 F 테스트 |
| etcd 일시적 불가 시 CRD 쓰기 실패 및 일관성 | 실제 etcd 장애 주입 필요 | 카오스 엔지니어링 도구 |

---


# 카테고리 1.5 — Envtest 통합 테스트 (유형 C: envtest 필요) ⚠️

> **빌드 태그:** `//go:build integration` | **실행:** `make setup-envtest && go test -tags=integration ./internal/controller/... ./internal/webhook/...`
>
> controller-runtime envtest 바이너리 필요 · Kind/Docker 불필요 · 표준 GitHub Actions CI에서 실행 가능

이 카테고리의 테스트는 **실제 Kubernetes API 서버 바이너리(envtest)**를 사용하여
CRD 컨트롤러의 조정(reconcile) 루프와 웹훅 검증기를 검증한다.
스토리지 백엔드(ZFS, NVMe-oF)는 mock을 사용하므로 실제 스토리지 하드웨어는 불필요하다.
`make setup-envtest` 실행 후 `bin/k8s/` 디렉터리에 envtest 바이너리가 준비되어야 한다.

**CI 실행 가능 여부:** ✅ 가능 — GitHub Actions `ubuntu-latest` 러너에서 `make setup-envtest` 후 실행 가능

```bash
# envtest 바이너리 준비
make setup-envtest

# 컨트롤러 통합 테스트 실행
go test -tags=integration ./internal/controller/... -v

# 웹훅 통합 테스트 실행
go test -tags=integration ./internal/webhook/... -v
```

**총 카테고리 1.5 테스트 케이스: 127개** (E19: 19개, E20: 20개, E23: 24개, E25: 41개, E26: 23개)

---

## E19: PillarTarget CRD 라이프사이클

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarTarget'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarTarget'
```

**목적:**
PillarTarget CRD의 전체 라이프사이클을 검증한다. 이 CRD는 스토리지 에이전트(pillar-agent)가
실행 중인 노드 또는 외부 주소를 식별하는 클러스터-스코프 리소스이다. 다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.nodeRef` / `spec.external` 판별 유니온(discriminated union) 검증
2. **상태 조건 전이** — `NodeExists`, `AgentConnected`, `Ready` 조건의 정확한 설정
3. **삭제 보호 동작** — PillarPool이 참조하는 동안 파이널라이저가 삭제를 차단

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `TgtCRD` | `api/v1alpha1.PillarTarget` CRD 및 상태 |
| `TgtCtrl` | `internal/controller.PillarTargetReconciler` |
| `TgtWH` | `internal/webhook/v1alpha1.PillarTargetCustomValidator` |
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD |
| `MockDialer` | `internal/controller.mockDialer` (테스트 더블) |

---

### E19.1 유효한 스펙으로 생성

**목적:** 다양한 유효한 스펙으로 PillarTarget을 생성하거나 검증기를 통과할 수 있음을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.1.1 | `TestPillarTargetWebhook_ValidCreate_External` | `spec.external.address` + `spec.external.port`가 모두 설정된 external 스펙으로 ValidateCreate 통과 | envtest API 서버; PillarTarget CRD 설치; `PillarTargetCustomValidator` 인스턴스 생성 | 1) `spec.external.address="10.0.0.1"`, `spec.external.port=9500`으로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `TgtWH` |
| E19.1.2 | `TestPillarTargetWebhook_ValidCreate_NodeRef` | `spec.nodeRef.name`만 설정된 nodeRef 스펙으로 ValidateCreate 통과 | envtest API 서버; PillarTarget CRD 설치; `PillarTargetCustomValidator` 인스턴스 생성 | 1) `spec.nodeRef.name="worker-1"`으로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `TgtWH` |
| E19.1.3 | `TestPillarTargetController_FinalizerAddedOnFirstReconcile` | PillarTarget 생성 후 첫 번째 `Reconcile` 호출에서 `pillar-target-protection` 파이널라이저 자동 추가 | envtest API 서버; `PillarTargetReconciler` 초기화 (`Dialer=nil`); `spec.external` PillarTarget 생성 | 1) `k8sClient.Create(ctx, target)` 실행; 2) `reconciler.Reconcile(ctx, req)` 1회 호출 | PillarTarget에 `pillar-csi.bhyoo.com/pillar-target-protection` 파이널라이저 존재; `result.RequeueAfter==0` | `TgtCRD`, `TgtCtrl` |

---

### E19.2 잘못된 스펙으로 생성 거부 — CRD 스키마 검증

**목적:** kubebuilder 마커(`+kubebuilder:validation:*`)에 의해 잘못된 필드 값이
Kubernetes API 서버 수준에서 거부됨을 확인한다. 웹훅이 아닌 CRD 스키마 검증으로 처리된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.2.1 | `TestPillarTargetCRD_InvalidCreate_EmptyNodeRefName` | `spec.nodeRef.name`이 빈 문자열인 경우 API 서버가 HTTP 422로 거부 | envtest API 서버; PillarTarget CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.nodeRef.name=""`으로 `k8sClient.Create(ctx, target)` 호출 | `k8sClient.Create` 오류 반환; HTTP 422 UnprocessableEntity; `spec.nodeRef.name` 필드 검증 실패 메시지 포함 | `TgtCRD` |
| E19.2.2 | `TestPillarTargetCRD_InvalidCreate_ExternalPortTooLow` | `spec.external.port=0` (최솟값 미달) 시 API 서버가 거부 | envtest API 서버; PillarTarget CRD 설치 (`Minimum=1` 마커 포함) | 1) `spec.external.address="10.0.0.1"`, `spec.external.port=0`으로 Create 호출 | 오류 반환; `spec.external.port` 값 범위 검증 실패 | `TgtCRD` |
| E19.2.3 | `TestPillarTargetCRD_InvalidCreate_ExternalPortTooHigh` | `spec.external.port=65536` (최댓값 초과) 시 API 서버가 거부 | envtest API 서버; PillarTarget CRD 설치 (`Maximum=65535` 마커 포함) | 1) `spec.external.port=65536`으로 Create 호출 | 오류 반환; `spec.external.port` 값 범위 검증 실패 | `TgtCRD` |
| E19.2.4 | `TestPillarTargetCRD_InvalidCreate_EmptyExternalAddress` | `spec.external.address`가 빈 문자열인 경우 거부 | envtest API 서버; PillarTarget CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.external.address=""`으로 Create 호출 | 오류 반환; `spec.external.address` 필드 검증 실패 | `TgtCRD` |

---

### E19.3 불변 필드 업데이트 거부 — 웹훅 검증

**목적:** PillarTarget의 핵심 식별 필드(에이전트 호스트를 바꾸는 변경)는
`ValidateUpdate`에서 `field.Forbidden` 오류로 거부됨을 확인한다.
이 검증은 `internal/webhook/v1alpha1.PillarTargetCustomValidator.ValidateUpdate`에서 수행된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.3.1 | `TestPillarTargetWebhook_ImmutableUpdate_NodeRefToExternal` | `spec.nodeRef` → `spec.external`로 판별자(discriminant) 전환 시 거부 | `oldObj.spec.nodeRef.name="node-1"`; `newObj.spec.external.address="10.0.0.1"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; 오류 메시지에 `"cannot switch between nodeRef and external"` 포함; `field.Forbidden` 타입; `spec` 경로 | `TgtWH` |
| E19.3.2 | `TestPillarTargetWebhook_ImmutableUpdate_ExternalToNodeRef` | `spec.external` → `spec.nodeRef`로 역전환 시 거부 | `oldObj.spec.external.address="10.0.0.1"`; `newObj.spec.nodeRef.name="node-1"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec` 경로 | `TgtWH` |
| E19.3.3 | `TestPillarTargetWebhook_ImmutableUpdate_NodeRefNameChange` | `spec.nodeRef.name` 변경 시 거부 — 에이전트 호스트 변경 방지 | `oldObj.spec.nodeRef.name="node-1"`; `newObj.spec.nodeRef.name="node-2"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.nodeRef.name` 경로; 오류 메시지에 이전값 `"node-1"`과 신규값 `"node-2"` 모두 포함 | `TgtWH` |
| E19.3.4 | `TestPillarTargetWebhook_ImmutableUpdate_ExternalAddressChange` | `spec.external.address` 변경 시 거부 | `oldObj.spec.external.address="10.0.0.1"`; `newObj.spec.external.address="10.0.0.2"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.external.address` 경로 | `TgtWH` |
| E19.3.5 | `TestPillarTargetWebhook_ImmutableUpdate_ExternalPortChange` | `spec.external.port` 변경 시 거부 | `oldObj.spec.external.port=9500`; `newObj.spec.external.port=9501` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.external.port` 경로 | `TgtWH` |
| E19.3.6 | `TestPillarTargetWebhook_MutableUpdate_AddressTypeChange` | `spec.nodeRef.addressType` 변경은 허용 (식별 필드 아님; 접속 방법만 변경) | `oldObj.spec.nodeRef.name="node-1"`, `addressType="InternalIP"`; `newObj.spec.nodeRef.name="node-1"`, `addressType="ExternalIP"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err=nil`; `warnings=nil`; 허용 | `TgtWH` |

---

### E19.4 상태 조건 전이 — NodeExists

**목적:** `NodeExists` 조건이 PillarTarget 모드(external vs nodeRef)와 K8s Node 존재 여부에 따라
올바르게 설정됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.4.1 | `TestPillarTargetController_NodeExists_Unknown_ExternalMode` | external 모드 PillarTarget에서 `NodeExists=Unknown/ExternalMode` 설정 — external 모드는 K8s Node를 참조하지 않음 | envtest; external PillarTarget 생성; 파이널라이저 추가 조정 1회 완료 | 1) 두 번째 `reconciler.Reconcile(ctx, req)` 호출 | `NodeExists.Status=Unknown`; `Reason="ExternalMode"` | `TgtCRD`, `TgtCtrl` |
| E19.4.2 | `TestPillarTargetController_NodeExists_True_NodePresent` | nodeRef 모드에서 참조된 K8s Node가 존재하면 `NodeExists=True/NodeFound` | envtest; `spec.nodeRef.name="worker-1"` PillarTarget; `worker-1` Node 오브젝트 사전 생성 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `NodeExists.Status=True`; `Reason="NodeFound"` | `TgtCRD`, `TgtCtrl` |
| E19.4.3 | `TestPillarTargetController_NodeExists_False_NodeMissing` | nodeRef 모드에서 참조된 K8s Node가 없으면 `NodeExists=False/NodeNotFound` | envtest; `spec.nodeRef.name="missing-node"` PillarTarget; `missing-node` Node 오브젝트 없음 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `NodeExists.Status=False`; `Reason="NodeNotFound"`; `Message`에 `"missing-node"` 포함 | `TgtCRD`, `TgtCtrl` |

---

### E19.5 상태 조건 전이 — AgentConnected

**목적:** `AgentConnected` 조건이 `mockDialer`의 응답에 따라 올바르게 설정됨을 확인한다.
`mockDialer`는 실제 gRPC 연결 없이 다양한 응답을 시뮬레이션하는 테스트 더블이다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.5.1 | `TestPillarTargetController_AgentConnected_False_DialerNil` | `reconciler.Dialer=nil`이면 `AgentConnected=False/DialerNotConfigured` — 개발/테스트 환경 | envtest; external PillarTarget; `reconciler.Dialer=nil` (기본값) | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `AgentConnected.Status=False`; `Reason="DialerNotConfigured"` | `TgtCRD`, `TgtCtrl` |
| E19.5.2 | `TestPillarTargetController_AgentConnected_True_PlainTCP` | `mockDialer{healthy:true, mtls:false}` → `AgentConnected=True/Dialed` — 평문 TCP 연결 시뮬레이션 | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{healthy:true, mtls:false}` | 1) 파이널라이저 조정; 2) mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=True`; `Reason="Dialed"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.5.3 | `TestPillarTargetController_AgentConnected_True_MTLS` | `mockDialer{healthy:true, mtls:true}` → `AgentConnected=True/Authenticated` — mTLS 연결 시뮬레이션 | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{healthy:true, mtls:true}` | 1) 파이널라이저 조정; 2) mTLS mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=True`; `Reason="Authenticated"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.5.4 | `TestPillarTargetController_AgentConnected_False_HealthCheckError` | `mockDialer.err != nil` → `AgentConnected=False/HealthCheckFailed` — 네트워크 오류 시뮬레이션 | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{err:errors.New("connection refused")}` | 1) 파이널라이저 조정; 2) 오류 반환 mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=False`; `Reason="HealthCheckFailed"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.5.5 | `TestPillarTargetController_AgentConnected_False_AgentUnhealthy` | `mockDialer{healthy:false}` → `AgentConnected=False/AgentUnhealthy` — 에이전트 자가 보고 비정상 상태 | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{healthy:false}` | 1) 파이널라이저 조정; 2) 비정상 응답 mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=False`; `Reason="AgentUnhealthy"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |

---

### E19.6 상태 조건 전이 — Ready 종합

**목적:** `Ready` 최상위 조건이 모든 하위 조건(`NodeExists`, `AgentConnected`)의 결과를
올바르게 종합함을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.6.1 | `TestPillarTargetController_Ready_True_AllConditionsMet` | `NodeExists=True` + `AgentConnected=True` → `Ready=True/AllConditionsMet` | envtest; nodeRef PillarTarget; 해당 Node 오브젝트 존재; `mockDialer{healthy:true}` 설정 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `Ready.Status=True`; `Reason="AllConditionsMet"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.6.2 | `TestPillarTargetController_Ready_False_NodeMissing` | `NodeExists=False` → `Ready=False` — 노드 없음 시 전체 준비 불가 | envtest; nodeRef PillarTarget; 해당 Node 없음 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `Ready.Status=False`; `NodeExists.Status=False` | `TgtCRD`, `TgtCtrl` |
| E19.6.3 | `TestPillarTargetController_Ready_False_AgentUnreachable` | `AgentConnected=False` → `Ready=False` — 에이전트 미도달 시 전체 준비 불가 | envtest; external PillarTarget; `mockDialer{err:errors.New("timeout")}` | 1) 파이널라이저 조정; 2) 오류 반환 mockDialer 설정 후 일반 조정 실행 | `Ready.Status=False`; `AgentConnected.Status=False`도 설정됨 | `TgtCRD`, `TgtCtrl`, `MockDialer` |

---

### E19.7 삭제 보호 동작

**목적:** `pillar-target-protection` 파이널라이저가 PillarPool이 참조하는 동안 PillarTarget
삭제를 차단하고, 모든 참조가 제거된 후에야 파이널라이저가 제거되어 오브젝트가 GC됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.7.1 | `TestPillarTargetController_DeletionBlocked_ReferencingPoolExists` | 참조 PillarPool이 존재하는 동안 삭제 차단; `result.RequeueAfter=10s` | envtest; PillarTarget + 파이널라이저; `spec.targetRef=<target>` PillarPool 존재 | 1) `k8sClient.Delete(ctx, target)` 호출; 2) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=10s`; 파이널라이저 여전히 존재; `k8sClient.Get` 성공 (오브젝트 미삭제) | `TgtCRD`, `TgtCtrl`, `PoolCRD` |
| E19.7.2 | `TestPillarTargetController_DeletionAllowed_NoReferencingPools` | 참조 PillarPool 없을 때 즉시 파이널라이저 제거 및 삭제 진행 | envtest; PillarTarget + 파이널라이저; 참조 PillarPool 없음; 삭제 요청 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=0`; 파이널라이저 제거; `k8sClient.Get` → NotFound | `TgtCRD`, `TgtCtrl` |
| E19.7.3 | `TestPillarTargetController_DeletionAllowed_AfterPoolRemoval` | 참조 PillarPool 제거 후 다음 조정에서 파이널라이저 제거 및 삭제 완료 | envtest; PillarTarget + 파이널라이저; 삭제 요청 후 첫 조정에서 차단 확인; 이후 참조 PillarPool 삭제 | 1) 첫 조정: `RequeueAfter=10s` 확인; 2) 참조 PillarPool 삭제; 3) 두 번째 조정 실행 | 두 번째 조정 후 파이널라이저 제거; `k8sClient.Get` → NotFound | `TgtCRD`, `TgtCtrl`, `PoolCRD` |
| E19.7.4 | `TestPillarTargetController_DeletionBlocked_MultiplePoolsRequireAllRemoved` | 여러 PillarPool이 동일 PillarTarget을 참조할 때, 하나라도 남아 있으면 차단 지속 | envtest; PillarTarget; PillarPool A, B 모두 `targetRef=<target>`; 삭제 요청 | 1) 첫 조정: 차단 (pool A, B 존재); 2) PillarPool A 삭제; 3) 두 번째 조정: 여전히 차단 (pool B 존재); 4) PillarPool B 삭제; 5) 세 번째 조정 | 첫 두 조정에서 `RequeueAfter=10s`; 세 번째 조정 후 파이널라이저 제거; 오브젝트 삭제 | `TgtCRD`, `TgtCtrl`, `PoolCRD` |

---

## E20: PillarPool CRD 라이프사이클

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarPool'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarPool'
```

**목적:**
PillarPool CRD의 전체 라이프사이클을 검증한다. 이 CRD는 특정 스토리지 에이전트의
특정 스토리지 풀(ZFS pool, LVM volume group 등)을 나타내는 클러스터-스코프 리소스이다.
다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.targetRef`, `spec.backend.type` 필드 검증
2. **상태 조건 전이** — `TargetReady`, `PoolDiscovered`, `BackendSupported`, `Ready` 조건의 정확한 전이
3. **용량 동기화** — PillarTarget의 `DiscoveredPools`에서 `status.capacity`로의 자동 동기화
4. **삭제 보호 동작** — PillarBinding이 참조하는 동안 파이널라이저가 삭제를 차단

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD 및 상태 |
| `PoolCtrl` | `internal/controller.PillarPoolReconciler` |
| `PoolWH` | `internal/webhook/v1alpha1.PillarPoolCustomValidator` |
| `TgtCRD` | `api/v1alpha1.PillarTarget` CRD 및 상태 |
| `BindCRD` | `api/v1alpha1.PillarBinding` CRD |

---

### E20.1 유효한 스펙으로 생성

**목적:** 다양한 `backend.type`으로 유효한 PillarPool을 생성하거나 검증기를 통과할 수 있음을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.1.1 | `TestPillarPoolWebhook_ValidCreate_ZFSZvol` | `backend.type="zfs-zvol"` + ZFS 설정으로 ValidateCreate 통과 | envtest; PillarPool CRD 설치; `PillarPoolCustomValidator` 인스턴스 생성 | 1) `spec.targetRef="target-a"`, `spec.backend.type="zfs-zvol"`, `spec.backend.zfs.pool="hot-data"`로 `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `PoolWH` |
| E20.1.2 | `TestPillarPoolWebhook_ValidCreate_Dir` | `backend.type="dir"`로 ValidateCreate 통과 (ZFS 설정 불필요) | envtest; PillarPool CRD 설치; `PillarPoolCustomValidator` 인스턴스 생성 | 1) `spec.targetRef="target-a"`, `spec.backend.type="dir"`로 `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `PoolWH` |
| E20.1.3 | `TestPillarPoolController_FinalizerAddedOnFirstReconcile` | PillarPool 생성 후 첫 번째 `Reconcile` 호출에서 `pool-protection` 파이널라이저 자동 추가 | envtest; `PillarPoolReconciler` 초기화; zfs-zvol 스펙으로 PillarPool 생성 | 1) `k8sClient.Create(ctx, pool)` 실행; 2) `reconciler.Reconcile(ctx, req)` 1회 호출 | PillarPool에 `pillar-csi.bhyoo.com/pool-protection` 파이널라이저 존재; `result.RequeueAfter==0` | `PoolCRD`, `PoolCtrl` |
| E20.1.4 | `TestPillarPoolController_FinalizerNotDuplicated` | 동일 PillarPool을 두 번 조정해도 파이널라이저 중복 없음 | envtest; PillarPool 생성; 첫 조정으로 파이널라이저 추가 완료 | 1) 두 번째 `reconciler.Reconcile(ctx, req)` 호출 | 파이널라이저 개수 정확히 1개; 중복 없음 | `PoolCRD`, `PoolCtrl` |

---

### E20.2 잘못된 스펙으로 생성 거부 — CRD 스키마 검증

**목적:** kubebuilder 마커에 의해 잘못된 필드 값이 Kubernetes API 서버 수준에서 거부됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.2.1 | `TestPillarPoolCRD_InvalidCreate_EmptyTargetRef` | `spec.targetRef`가 빈 문자열인 경우 API 서버가 거부 | envtest; PillarPool CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.targetRef=""`로 `k8sClient.Create(ctx, pool)` 호출 | 오류 반환; HTTP 422; `spec.targetRef` 필드 검증 실패 | `PoolCRD` |
| E20.2.2 | `TestPillarPoolCRD_InvalidCreate_InvalidBackendType` | `spec.backend.type`에 열거형 외 값 설정 시 거부 | envtest; PillarPool CRD 설치 (`Enum=zfs-zvol;zfs-dataset;lvm-lv;dir` 마커 포함) | 1) `spec.backend.type="unknown-backend"`로 Create 호출 | 오류 반환; HTTP 422; `spec.backend.type` 열거형 검증 실패 | `PoolCRD` |
| E20.2.3 | `TestPillarPoolCRD_InvalidCreate_EmptyBackendType` | `spec.backend.type`이 빈 문자열인 경우 거부 | envtest; PillarPool CRD 설치 | 1) `spec.backend.type=""`로 Create 호출 | 오류 반환; `spec.backend.type` 필수 필드 오류 | `PoolCRD` |

---

### E20.3 불변 필드 업데이트 거부 — 웹훅 검증

**목적:** `spec.targetRef`와 `spec.backend.type`은 생성 후 변경할 수 없음을 확인한다.
기존 볼륨이 원래 backend 드라이버와 target에 묶여 있기 때문이다.
이 검증은 `internal/webhook/v1alpha1.PillarPoolCustomValidator.ValidateUpdate`에서 수행된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.3.1 | `TestPillarPoolWebhook_ImmutableUpdate_TargetRefChange` | `spec.targetRef` 변경 시 거부 — 풀이 묶인 스토리지 노드 변경 방지 | `oldObj.spec.targetRef="target-a"`; `newObj.spec.targetRef="target-b"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.targetRef` 경로; 오류 메시지에 이전값 `"target-a"`과 신규값 `"target-b"` 모두 포함 | `PoolWH` |
| E20.3.2 | `TestPillarPoolWebhook_ImmutableUpdate_BackendTypeChange` | `spec.backend.type` 변경 시 거부 — 기존 볼륨의 드라이버 변경 방지 | `oldObj.spec.backend.type="zfs-zvol"`; `newObj.spec.backend.type="lvm-lv"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.backend.type` 경로 | `PoolWH` |
| E20.3.3 | `TestPillarPoolWebhook_ImmutableUpdate_BothFieldsChange` | `spec.targetRef`와 `spec.backend.type` 동시 변경 시 두 오류 모두 반환 | `oldObj.spec.targetRef="target-a"`, `backend.type="zfs-zvol"`; `newObj.spec.targetRef="target-b"`, `backend.type="lvm-lv"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; 오류 집계(Aggregate)에 2개 오류; `spec.targetRef`와 `spec.backend.type` 모두 Forbidden | `PoolWH` |
| E20.3.4 | `TestPillarPoolWebhook_MutableUpdate_ZFSPropertiesChange` | `spec.backend.zfs.properties` 변경은 허용 (불변 필드 아님) | `oldObj.spec.backend.type="zfs-zvol"`, `zfs.pool="hot-data"`, `properties={"compression":"off"}`; `newObj.properties={"compression":"lz4"}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err=nil`; 허용; `backend.type`과 `targetRef`는 동일 | `PoolWH` |

---

### E20.4 상태 조건 전이 — TargetReady

**목적:** `TargetReady` 조건이 PillarTarget 존재 여부 및 Ready 상태에 따라 올바르게 설정됨을 확인한다.
PillarTarget이 없거나 Not-Ready이면 하위 조건(`PoolDiscovered`, `BackendSupported`)도 영향을 받는다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.4.1 | `TestPillarPoolController_TargetReady_False_TargetAbsent` | 참조 PillarTarget이 없으면 `TargetReady=False/TargetNotFound`; 하위 조건도 모두 False | envtest; PillarPool 생성 (`targetRef="nonexistent"`); 해당 PillarTarget 없음 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `TargetReady.Status=False`; `Reason="TargetNotFound"`; `PoolDiscovered.Status=False`; `BackendSupported.Status=False`; `Ready.Status=False` (모두 `Reason="TargetNotFound"`) | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.4.2 | `TestPillarPoolController_TargetReady_False_TargetNotReady` | 참조 PillarTarget이 존재하지만 `Ready=False`이면 `TargetReady=False/TargetNotReady`; 하위 조건 Unknown | envtest; PillarPool 생성; PillarTarget 존재하되 `Ready.Status=False`로 상태 패치 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `TargetReady.Status=False`; `Reason="TargetNotReady"`; `PoolDiscovered.Status=Unknown`; `BackendSupported.Status=Unknown`; `Ready.Status=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.4.3 | `TestPillarPoolController_TargetReady_True_TargetReady` | 참조 PillarTarget이 `Ready=True`이면 `TargetReady=True` | envtest; PillarPool 생성; PillarTarget `Ready.Status=True`, `resolvedAddress="192.0.2.10:9500"`으로 상태 패치 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `TargetReady.Status=True`; `Reason="TargetReady"`; `Message`에 resolvedAddress 포함 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

---

### E20.5 상태 조건 전이 — PoolDiscovered

**목적:** `PoolDiscovered` 조건이 PillarTarget의 `status.discoveredPools` 목록에 따라
올바르게 설정됨을 확인한다. ZFS 백엔드는 이름 매칭이 필요하고, 다른 백엔드(dir, lvm-lv)는 다른 규칙을 따른다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.5.1 | `TestPillarPoolController_PoolDiscovered_Unknown_EmptyDiscoveredPools` | PillarTarget `Ready=True`이지만 `discoveredPools=[]`이면 `PoolDiscovered=Unknown/WaitingForAgentData` | envtest; PillarPool(zfs-zvol, `zfs.pool="hot-data"`); PillarTarget Ready=True이나 `discoveredPools=[]` | 1) 일반 조정 실행 | `PoolDiscovered.Status=Unknown`; `Reason="WaitingForAgentData"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.5.2 | `TestPillarPoolController_PoolDiscovered_True_ZFSPoolNameMatch` | `discoveredPools`에 ZFS 풀 이름이 일치하는 항목이 있으면 `PoolDiscovered=True` | envtest; PillarPool(zfs-zvol, `zfs.pool="hot-data"`); PillarTarget `discoveredPools=[{name:"hot-data", type:"zfs"}]` | 1) 일반 조정 실행 | `PoolDiscovered.Status=True`; `Reason="PoolDiscovered"`; `Message`에 `"hot-data"` 포함 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.5.3 | `TestPillarPoolController_PoolDiscovered_False_ZFSPoolNameMismatch` | `discoveredPools`에 ZFS 풀 이름이 없으면 `PoolDiscovered=False/PoolNotFound` | envtest; PillarPool(zfs-zvol, `zfs.pool="hot-data"`); PillarTarget `discoveredPools=[{name:"cold-data", type:"zfs"}]` | 1) 일반 조정 실행 | `PoolDiscovered.Status=False`; `Reason="PoolNotFound"`; `Message`에 `"hot-data"`와 발견된 풀 이름 목록(`["cold-data"]`) 포함 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.5.4 | `TestPillarPoolController_PoolDiscovered_True_DirBackend_NoNameRequired` | `backend.type="dir"` 백엔드는 명시적 풀 이름 없이 `discoveredPools`에 항목만 있으면 `PoolDiscovered=True` | envtest; PillarPool(dir); PillarTarget `discoveredPools=[{name:"any-entry", type:"dir"}]` | 1) 일반 조정 실행 | `PoolDiscovered.Status=True`; `Reason="PoolDiscovered"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

---

### E20.6 상태 조건 전이 — BackendSupported

**목적:** `BackendSupported` 조건이 PillarTarget의 `status.capabilities.backends` 목록에 따라
올바르게 설정됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.6.1 | `TestPillarPoolController_BackendSupported_Unknown_NoCapabilities` | PillarTarget `Ready=True`이지만 `capabilities=nil`이면 `BackendSupported=Unknown/WaitingForAgentData` | envtest; PillarPool(zfs-zvol); PillarTarget Ready=True이나 `capabilities=nil` | 1) 일반 조정 실행 | `BackendSupported.Status=Unknown`; `Reason="WaitingForAgentData"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.6.2 | `TestPillarPoolController_BackendSupported_True_BackendInCapabilities` | `capabilities.backends`에 해당 `backend.type`이 있으면 `BackendSupported=True` | envtest; PillarPool(zfs-zvol); PillarTarget `capabilities.backends=["zfs-zvol","zfs-dataset"]` | 1) 일반 조정 실행 | `BackendSupported.Status=True`; `Reason="BackendSupported"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.6.3 | `TestPillarPoolController_BackendSupported_False_BackendNotInCapabilities` | `capabilities.backends`에 해당 `backend.type`이 없으면 `BackendSupported=False/BackendNotSupported` | envtest; PillarPool(lvm-lv); PillarTarget `capabilities.backends=["zfs-zvol","zfs-dataset"]` | 1) 일반 조정 실행 | `BackendSupported.Status=False`; `Reason="BackendNotSupported"`; `Message`에 `"lvm-lv"`와 지원 목록(`["zfs-zvol","zfs-dataset"]`) 포함 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

---

### E20.7 Ready 종합 조건 전이

**목적:** `Ready` 최상위 조건이 `TargetReady`, `PoolDiscovered`, `BackendSupported` 세 조건의
논리적 AND 결과로 올바르게 설정됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.7.1 | `TestPillarPoolController_Ready_True_AllConditionsMet` | 모든 조건 True → `Ready=True/AllConditionsMet` | envtest; PillarPool(zfs-zvol, `pool="hot-data"`); PillarTarget Ready=True; `discoveredPools=[{name:"hot-data"}]`; `capabilities.backends=["zfs-zvol"]` | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `Ready.Status=True`; `Reason="AllConditionsMet"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.7.2 | `TestPillarPoolController_Ready_False_PoolNotDiscovered` | `PoolDiscovered=False` → `Ready=False/ConditionsNotMet` (`TargetReady=True`, `BackendSupported=True`이어도) | envtest; PillarPool(zfs-zvol, `pool="missing-pool"`); PillarTarget Ready=True; `discoveredPools=[{name:"other-pool"}]`; `capabilities.backends=["zfs-zvol"]` | 1) 일반 조정 실행 | `Ready.Status=False`; `Reason="ConditionsNotMet"`; `Message`에 `PoolDiscovered` 실패 이유 포함 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.7.3 | `TestPillarPoolController_Ready_False_BackendUnsupported` | `BackendSupported=False` → `Ready=False/ConditionsNotMet` | envtest; PillarPool(lvm-lv); PillarTarget Ready=True; discoveredPools 존재; `capabilities.backends=["zfs-zvol"]` | 1) 일반 조정 실행 | `Ready.Status=False`; `Reason="ConditionsNotMet"`; `Message`에 `BackendSupported` 실패 이유 포함 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

---

### E20.8 용량 동기화 (DiscoveredPool → status.capacity)

**목적:** PillarTarget의 `status.discoveredPools`에서 용량 정보가 PillarPool의
`status.capacity`로 자동 동기화됨을 확인한다. `Used = Total - Available` 계산과
음수 방지 클램핑 동작을 포함한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.8.1 | `TestPillarPoolController_CapacitySync_TotalAndAvailableSet` | `DiscoveredPool`에 Total, Available 있으면 `status.capacity`에 반영; `Used=Total-Available` 자동 계산 | envtest; PillarPool(zfs-zvol, `pool="hot-data"`); PillarTarget Ready=True; `discoveredPools=[{name:"hot-data", total:"100Gi", available:"60Gi"}]` | 1) 일반 조정 실행 | `status.capacity.total="100Gi"`; `status.capacity.available="60Gi"`; `status.capacity.used="40Gi"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.8.2 | `TestPillarPoolController_CapacitySync_UsedClampedAtZero` | `Available > Total` 비정상 데이터에서 `Used=0`으로 클램핑 (음수 방지) | envtest; `discoveredPools=[{name:"hot-data", total:"50Gi", available:"80Gi"}]` (Available > Total) | 1) 일반 조정 실행 | `status.capacity.used="0"` (클램핑); `total="50Gi"`; `available="80Gi"` 그대로 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.8.3 | `TestPillarPoolController_CapacitySync_ClearedWhenPoolNotDiscovered` | `PoolDiscovered=False` 또는 Unknown이면 기존 capacity 제거 | envtest; PillarPool(zfs-zvol, `pool="hot-data"`); PillarTarget `discoveredPools=[{name:"other-pool"}]` (이름 불일치) | 1) 일반 조정 실행 | `status.capacity=nil`; 이전에 설정된 capacity 값 제거됨 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

---

### E20.9 삭제 보호 동작

**목적:** `pool-protection` 파이널라이저가 PillarBinding이 참조하는 동안 PillarPool
삭제를 차단하고, 모든 참조가 제거된 후에야 파이널라이저가 제거되어 오브젝트가 GC됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.9.1 | `TestPillarPoolController_DeletionBlocked_ReferencingBindingExists` | 참조 PillarBinding이 존재하는 동안 삭제 차단; `Ready=False/DeletionBlocked` 설정 | envtest; PillarPool + 파이널라이저; `spec.poolRef=<pool>` PillarBinding 존재; 삭제 요청 | 1) `k8sClient.Delete(ctx, pool)` 호출; 2) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=10s`; 파이널라이저 존재; `Ready.Reason="DeletionBlocked"`; `Message`에 PillarBinding 이름 포함 | `PoolCRD`, `PoolCtrl`, `BindCRD` |
| E20.9.2 | `TestPillarPoolController_DeletionAllowed_NoReferencingBindings` | 참조 PillarBinding 없을 때 즉시 파이널라이저 제거 및 삭제 진행 | envtest; PillarPool + 파이널라이저; 참조 PillarBinding 없음; 삭제 요청 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=0`; 파이널라이저 제거; `k8sClient.Get` → NotFound | `PoolCRD`, `PoolCtrl` |
| E20.9.3 | `TestPillarPoolController_DeletionAllowed_AfterBindingRemoval` | 참조 PillarBinding 제거 후 다음 조정에서 파이널라이저 제거 및 삭제 완료 | envtest; PillarPool + 파이널라이저; 첫 조정에서 차단 확인; 이후 참조 PillarBinding 삭제 | 1) 첫 조정: 차단; 2) 참조 PillarBinding 삭제; 3) 두 번째 조정 실행 | 두 번째 조정 후 파이널라이저 제거; `k8sClient.Get` → NotFound | `PoolCRD`, `PoolCtrl`, `BindCRD` |
| E20.9.4 | `TestPillarPoolController_DeletionBlocked_StatusMessageContainsAllBindingNames` | 여러 PillarBinding이 참조할 때 상태 메시지에 모든 이름이 나열됨 | envtest; PillarPool + 파이널라이저; PillarBinding `binding-a`, `binding-b` 모두 `poolRef=<pool>`; 삭제 요청 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `Ready.Message`에 `"binding-a"`, `"binding-b"` 모두 포함; `result.RequeueAfter=10s` | `PoolCRD`, `PoolCtrl`, `BindCRD` |

---

### E20.10 PillarTarget 상태 변경 시 PillarPool 재조정

**목적:** PillarTarget의 상태가 변경될 때 해당 target을 참조하는 PillarPool이
자동으로 재조정되어 `TargetReady` 조건이 최신 상태로 유지됨을 확인한다.
이 동작은 `SetupWithManager`에서 PillarTarget 변경에 대한 Watch를 등록하여 구현된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.10.1 | `TestPillarPoolController_TargetReadyTransition_NotReadyToReady` | PillarTarget이 Not-Ready → Ready로 전이 후 PillarPool 재조정 시 `TargetReady=True`로 갱신 | envtest; PillarPool + PillarTarget(Ready=False); PillarPool 조정 후 `TargetReady=False` 확인 | 1) PillarTarget `Ready=True`로 상태 패치; 2) PillarPool 재조정 실행 | PillarPool `TargetReady.Status=True`로 업데이트됨 | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.10.2 | `TestPillarPoolController_TargetReadyTransition_ReadyToNotReady` | PillarTarget이 Ready → Not-Ready로 전이 후 PillarPool 재조정 시 `TargetReady=False`; 하위 조건 Unknown으로 후퇴 | envtest; PillarPool + PillarTarget(Ready=True); PillarPool 조정 후 `TargetReady=True` 확인 | 1) PillarTarget `Ready=False`로 상태 패치; 2) PillarPool 재조정 실행 | PillarPool `TargetReady.Status=False`; `PoolDiscovered.Status=Unknown`; `BackendSupported.Status=Unknown` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

---

## E23: PillarProtocol CRD 라이프사이클

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarProtocol'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarProtocol'
```

**목적:**
PillarProtocol CRD의 전체 라이프사이클을 검증한다. 이 CRD는 스토리지 볼륨을 노출할 때
사용할 네트워크 프로토콜 구성(NVMe-oF/TCP, iSCSI, NFS)을 정의하는 클러스터-스코프 리소스이다.
동일한 PillarProtocol을 여러 PillarBinding이 참조할 수 있으며, 다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.type` 열거형 검증 및 프로토콜별 포트 범위 검증
2. **불변 필드 업데이트 거부** — `spec.type`은 생성 후 변경 불가 (웹훅 검증)
3. **상태 조건 전이** — `Ready` 조건, `BindingCount`, `ActiveTargets` 상태 필드의 정확한 전이
4. **삭제 보호 동작** — PillarBinding이 참조하는 동안 파이널라이저가 삭제를 차단

> **CI 실행 가능 여부:** ✅ CI에서 실행 가능 — envtest는 실제 K8s 클러스터 없이
> 인메모리 API 서버만 구동하므로 도커/Kind 불필요.
>
> **단, PillarProtocol 웹훅(ValidateCreate, ValidateDelete) 구현은 현재 스캐폴딩 수준으로 TODO 상태다.**
> `ValidateUpdate`만 `spec.type` 불변 검증이 구현되어 있다.

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `PProtCRD` | `api/v1alpha1.PillarProtocol` CRD 및 상태 |
| `PProtCtrl` | `internal/controller.PillarProtocolReconciler` |
| `PProtWH` | `internal/webhook/v1alpha1.PillarProtocolCustomValidator` |
| `BindCRD` | `api/v1alpha1.PillarBinding` CRD |
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD |

---

### E23.1 유효한 스펙으로 생성

**목적:** 다양한 유효한 프로토콜 타입으로 PillarProtocol을 생성하거나 검증기를 통과할 수 있음을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.1.1 | `TestPillarProtocolWebhook_ValidCreate_NVMeOFTCP` | `spec.type="nvmeof-tcp"` 스펙으로 ValidateCreate 통과 (현재 구현은 항상 허용) | envtest API 서버; PillarProtocol CRD 설치; `PillarProtocolCustomValidator` 인스턴스 생성 | 1) `spec.type="nvmeof-tcp"`, `spec.nvmeofTcp.port=4420`으로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `PProtWH` |
| E23.1.2 | `TestPillarProtocolWebhook_ValidCreate_ISCSI` | `spec.type="iscsi"` 스펙으로 ValidateCreate 통과 | envtest API 서버; PillarProtocol CRD 설치; `PillarProtocolCustomValidator` 인스턴스 생성 | 1) `spec.type="iscsi"`, `spec.iscsi.port=3260`으로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `PProtWH` |
| E23.1.3 | `TestPillarProtocolWebhook_ValidCreate_NFS` | `spec.type="nfs"` 스펙으로 ValidateCreate 통과 | envtest API 서버; PillarProtocol CRD 설치; `PillarProtocolCustomValidator` 인스턴스 생성 | 1) `spec.type="nfs"`, `spec.nfs.version="4.2"`으로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `PProtWH` |
| E23.1.4 | `TestPillarProtocolController_FinalizerAddedOnFirstReconcile` | PillarProtocol 생성 후 첫 번째 `Reconcile` 호출에서 `protocol-protection` 파이널라이저 자동 추가 | envtest; `PillarProtocolReconciler` 초기화; `spec.type="nvmeof-tcp"` PillarProtocol 생성 | 1) `k8sClient.Create(ctx, protocol)` 실행; 2) `reconciler.Reconcile(ctx, req)` 1회 호출 | PillarProtocol에 `pillar-csi.bhyoo.com/protocol-protection` 파이널라이저 존재; `result.RequeueAfter==0` | `PProtCRD`, `PProtCtrl` |
| E23.1.5 | `TestPillarProtocolController_FinalizerNotDuplicated` | 동일 PillarProtocol을 두 번 조정해도 파이널라이저 중복 없음 | envtest; PillarProtocol 생성; 첫 조정으로 파이널라이저 추가 완료 | 1) 두 번째 `reconciler.Reconcile(ctx, req)` 호출 | 파이널라이저 개수 정확히 1개; 중복 없음 | `PProtCRD`, `PProtCtrl` |

---

### E23.2 잘못된 스펙으로 생성 거부 — CRD 스키마 검증

**목적:** kubebuilder 마커(`+kubebuilder:validation:Enum=nvmeof-tcp;iscsi;nfs` 등)에 의해
잘못된 필드 값이 Kubernetes API 서버 수준에서 거부됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.2.1 | `TestPillarProtocolCRD_InvalidCreate_UnknownType` | `spec.type`에 열거형 외 값 설정 시 API 서버가 HTTP 422로 거부 | envtest; PillarProtocol CRD 설치 (`Enum=nvmeof-tcp;iscsi;nfs` 마커 포함) | 1) `spec.type="unknown-protocol"`으로 `k8sClient.Create(ctx, protocol)` 호출 | 오류 반환; HTTP 422 UnprocessableEntity; `spec.type` 열거형 검증 실패 메시지 포함 | `PProtCRD` |
| E23.2.2 | `TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooLow` | `spec.nvmeofTcp.port=0` (최솟값 미달) 시 API 서버가 거부 | envtest; PillarProtocol CRD 설치 (`Minimum=1` 마커 포함) | 1) `spec.type="nvmeof-tcp"`, `spec.nvmeofTcp.port=0`으로 Create 호출 | 오류 반환; `spec.nvmeofTcp.port` 값 범위 검증 실패 | `PProtCRD` |
| E23.2.3 | `TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooHigh` | `spec.nvmeofTcp.port=65536` (최댓값 초과) 시 API 서버가 거부 | envtest; PillarProtocol CRD 설치 (`Maximum=65535` 마커 포함) | 1) `spec.type="nvmeof-tcp"`, `spec.nvmeofTcp.port=65536`으로 Create 호출 | 오류 반환; `spec.nvmeofTcp.port` 값 범위 검증 실패 | `PProtCRD` |
| E23.2.4 | `TestPillarProtocolCRD_InvalidCreate_InvalidFSType` | `spec.fsType`에 허용 외 값(`ext4`, `xfs` 외) 설정 시 거부 | envtest; PillarProtocol CRD 설치 (`Enum=ext4;xfs` 마커 포함) | 1) `spec.type="nvmeof-tcp"`, `spec.fsType="btrfs"`으로 Create 호출 | 오류 반환; HTTP 422; `spec.fsType` 열거형 검증 실패 | `PProtCRD` |

---

### E23.3 불변 필드 업데이트 거부 — 웹훅 검증

**목적:** `spec.type`은 생성 후 변경할 수 없음을 확인한다. 각 프로토콜 타입은 서로 다른
커널 서브시스템(NVMe-oF vs iSCSI vs NFS)을 사용하므로, 타입 변경은 모든 기존 볼륨을
orphan 상태로 만드는 치명적 변경이다.
이 검증은 `internal/webhook/v1alpha1.PillarProtocolCustomValidator.ValidateUpdate`에서 수행된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.3.1 | `TestPillarProtocolWebhook_ImmutableUpdate_TypeChange_NVMeToISCSI` | `spec.type="nvmeof-tcp"` → `"iscsi"` 변경 시 `field.Forbidden` 오류 반환 | `oldObj.spec.type="nvmeof-tcp"`; `newObj.spec.type="iscsi"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; 오류 메시지에 `"immutable"` 포함; `spec.type` 경로의 `field.Forbidden`; 이전값 `"nvmeof-tcp"`, 신규값 `"iscsi"` 모두 언급 | `PProtWH` |
| E23.3.2 | `TestPillarProtocolWebhook_ImmutableUpdate_TypeChange_ISCSIToNFS` | `spec.type="iscsi"` → `"nfs"` 변경 시 거부 | `oldObj.spec.type="iscsi"`; `newObj.spec.type="nfs"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.type` 경로 | `PProtWH` |
| E23.3.3 | `TestPillarProtocolWebhook_MutableUpdate_PortChange` | `spec.nvmeofTcp.port` 변경은 허용 (식별 필드 아님) | `oldObj.spec.type="nvmeof-tcp"`, `port=4420`; `newObj.spec.type="nvmeof-tcp"`, `port=4421` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err=nil`; `warnings=nil`; 허용 | `PProtWH` |

---

### E23.4 상태 조건 전이 — Ready 조건

**목적:** `Ready` 조건이 PillarProtocol의 정상 조정 경로에서 올바르게 설정됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.4.1 | `TestPillarProtocolController_Ready_True_NoBindings` | 참조 PillarBinding 없는 정상 조정에서 `Ready=True/ProtocolConfigured` | envtest; PillarProtocol 생성; 파이널라이저 추가 조정 완료 | 1) 두 번째 `reconciler.Reconcile(ctx, req)` 호출 | `Ready.Status=True`; `Ready.Reason="ProtocolConfigured"` | `PProtCRD`, `PProtCtrl` |
| E23.4.2 | `TestPillarProtocolController_Ready_True_WithBindings` | 참조 PillarBinding이 존재하는 정상 조정에서도 `Ready=True` | envtest; PillarProtocol + PillarBinding(참조) 생성; 파이널라이저 추가 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `Ready.Status=True`; `Ready.Reason="ProtocolConfigured"` | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.4.3 | `TestPillarProtocolController_Ready_Message_ContainsType` | `Ready` 조건 메시지에 `spec.type` 값이 포함됨 | envtest; `spec.type="nvmeof-tcp"` PillarProtocol; 파이널라이저 추가 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `Ready.Message`에 `"nvmeof-tcp"` 포함 | `PProtCRD`, `PProtCtrl` |
| E23.4.4 | `TestPillarProtocolController_Ready_False_DeletionBlocked` | 삭제 요청 중 참조 PillarBinding 존재 시 `Ready=False/DeletionBlocked` | envtest; PillarProtocol + 파이널라이저; 참조 PillarBinding 존재; 삭제 요청 | 1) `k8sClient.Delete(ctx, protocol)` 호출; 2) `reconciler.Reconcile(ctx, req)` 호출 | `Ready.Status=False`; `Ready.Reason="DeletionBlocked"`; `Ready.Message`에 참조 PillarBinding 이름 포함 | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.4.5 | `TestPillarProtocolController_NoRequeue_WhenReady` | 정상 상태에서 `result.RequeueAfter==0` — 불필요한 재조정 없음 | envtest; PillarProtocol; 파이널라이저 추가 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter==0` | `PProtCRD`, `PProtCtrl` |

---

### E23.5 상태 필드 — BindingCount 및 ActiveTargets

**목적:** `status.bindingCount`와 `status.activeTargets`가 참조 PillarBinding 및
연결된 PillarPool의 `targetRef`로부터 올바르게 계산됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.5.1 | `TestPillarProtocolController_BindingCount_Zero_NoBindings` | 참조 PillarBinding 없을 때 `BindingCount=0`, `ActiveTargets=[]` | envtest; PillarProtocol; 파이널라이저 추가 완료; 참조 PillarBinding 없음 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `status.bindingCount=0`; `status.activeTargets=[]` | `PProtCRD`, `PProtCtrl` |
| E23.5.2 | `TestPillarProtocolController_BindingCount_One_SingleBinding` | 참조 PillarBinding 1개 시 `BindingCount=1` | envtest; PillarProtocol + PillarPool + PillarBinding(참조); 파이널라이저 추가 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `status.bindingCount=1` | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.5.3 | `TestPillarProtocolController_ActiveTargets_PopulatedFromPool` | PillarBinding → PillarPool → `spec.targetRef` 체인으로 `ActiveTargets` 자동 계산 | envtest; PillarProtocol; PillarPool(`spec.targetRef="node-1"`); PillarBinding(poolRef=pool, protocolRef=protocol) | 1) `reconciler.Reconcile(ctx, req)` 호출 | `status.activeTargets=["node-1"]` | `PProtCRD`, `PProtCtrl`, `BindCRD`, `PoolCRD` |
| E23.5.4 | `TestPillarProtocolController_ActiveTargets_DeduplicatedSorted` | 여러 PillarBinding이 동일 풀 참조해도 `ActiveTargets`에 중복 없이 정렬된 목록 | envtest; PillarProtocol; PillarPool(`targetRef="node-1"`); PillarBinding A, B 모두 동일 풀 참조 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `status.activeTargets=["node-1"]` (중복 없음) | `PProtCRD`, `PProtCtrl`, `BindCRD`, `PoolCRD` |
| E23.5.5 | `TestPillarProtocolController_ActiveTargets_EmptyWhenPoolNotFound` | 참조 PillarPool이 없을 때 `BindingCount`는 정확히 집계되나 `ActiveTargets=[]` (우아한 저하) | envtest; PillarProtocol; PillarBinding(참조); PillarPool 없음 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `status.bindingCount=1`; `status.activeTargets=[]`; `Ready=True` (저하 상태지만 오류 아님) | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.5.6 | `TestPillarProtocolController_BindingCount_Decremented_AfterBindingRemoval` | PillarBinding 삭제 후 조정 시 `BindingCount` 감소 | envtest; PillarProtocol + PillarBinding; 초기 `BindingCount=1` 확인 후 PillarBinding 삭제 | 1) PillarBinding 삭제; 2) `reconciler.Reconcile(ctx, req)` 호출 | `status.bindingCount=0`; `status.activeTargets=[]` | `PProtCRD`, `PProtCtrl`, `BindCRD` |

---

### E23.6 삭제 보호 동작

**목적:** `pillar-csi.bhyoo.com/protocol-protection` 파이널라이저가 PillarBinding이
참조하는 동안 PillarProtocol 삭제를 차단하고, 모든 참조가 제거된 후에야
파이널라이저가 제거되어 오브젝트가 GC됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.6.1 | `TestPillarProtocolController_DeletionBlocked_ReferencingBindingExists` | 참조 PillarBinding이 존재하는 동안 삭제 차단; `result.RequeueAfter=10s` | envtest; PillarProtocol + 파이널라이저; `spec.protocolRef=<protocol>` PillarBinding 존재; 삭제 요청 | 1) `k8sClient.Delete(ctx, protocol)` 호출; 2) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=10s`; 파이널라이저 존재; `k8sClient.Get` 성공 (오브젝트 미삭제) | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.6.2 | `TestPillarProtocolController_DeletionBlocked_StatusUpdated` | 삭제 차단 시 `Ready=False/DeletionBlocked` 설정; `BindingCount`에 차단 바인딩 수 반영 | envtest; PillarProtocol + 파이널라이저; PillarBinding `binding-x` 참조; 삭제 요청 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `Ready.Status=False`; `Ready.Reason="DeletionBlocked"`; `Ready.Message`에 `"binding-x"` 포함; `status.bindingCount=1` | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.6.3 | `TestPillarProtocolController_DeletionBlocked_FinalizerKept` | 삭제 차단 중 파이널라이저 제거되지 않음 | envtest; 동일 사전 조건 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `controllerutil.ContainsFinalizer(fetched, pillarProtocolFinalizer)=true` | `PProtCRD`, `PProtCtrl` |
| E23.6.4 | `TestPillarProtocolController_DeletionAllowed_NoReferencingBindings` | 참조 PillarBinding 없을 때 즉시 파이널라이저 제거 및 삭제 진행 | envtest; PillarProtocol + 파이널라이저; 참조 PillarBinding 없음; 삭제 요청 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=0`; 파이널라이저 제거; `k8sClient.Get` → NotFound | `PProtCRD`, `PProtCtrl` |
| E23.6.5 | `TestPillarProtocolController_DeletionAllowed_AfterBindingRemoval` | 참조 PillarBinding 제거 후 다음 조정에서 파이널라이저 제거 및 삭제 완료 | envtest; PillarProtocol + 파이널라이저; 첫 조정에서 차단 확인; 이후 참조 PillarBinding 삭제 | 1) 첫 조정: `RequeueAfter=10s` 확인; 2) 참조 PillarBinding 삭제; 3) 두 번째 조정 실행 | 두 번째 조정 후 파이널라이저 제거; `k8sClient.Get` → NotFound | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.6.6 | `TestPillarProtocolController_DeletionBlocked_MultipleBindingsAllNamed` | 여러 PillarBinding이 참조할 때 상태 메시지에 모든 이름이 나열됨 | envtest; PillarProtocol; PillarBinding `binding-a`, `binding-b` 모두 참조; 삭제 요청 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `Ready.Message`에 `"binding-a"`, `"binding-b"` 모두 포함; `result.RequeueAfter=10s` | `PProtCRD`, `PProtCtrl`, `BindCRD` |

---

**총 E23 테스트 케이스: 24개** (E23.1: 5개, E23.2: 4개, E23.3: 3개, E23.4: 5개, E23.5: 6개, E23.6: 6개)

---

## E25: PillarBinding CRD 라이프사이클

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarBinding'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarBinding'
```

**목적:**
PillarBinding CRD의 전체 라이프사이클을 검증한다. 이 CRD는 PillarPool(스토리지 백엔드)과
PillarProtocol(네트워크 프로토콜)을 결합하여 Kubernetes StorageClass를 자동으로 생성한다.
다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.poolRef`, `spec.protocolRef` 필수 필드 검증
2. **불변 필드 업데이트 거부** — `spec.poolRef`, `spec.protocolRef`는 생성 후 변경 불가 (웹훅)
3. **Defaulting 웹훅** — `allowVolumeExpansion` 자동 설정 (백엔드 타입 기반)
4. **백엔드-프로토콜 호환성 웹훅** — 블록 백엔드 + 파일 프로토콜 등 비호환 조합 거부
5. **상태 조건 전이** — `PoolReady`, `ProtocolValid`, `Compatible`, `StorageClassCreated`, `Ready` 조건
6. **StorageClass 라이프사이클** — 소유권, 파라미터, 커스텀 이름, ReclaimPolicy 반영
7. **삭제 보호 동작** — StorageClass를 참조하는 PVC가 존재하는 동안 차단

> **CI 실행 가능 여부:** ✅ CI에서 실행 가능 — envtest 사용.
>
> **참고:** 백엔드-프로토콜 호환성 검증은 웹훅(admission time)과 컨트롤러(reconcile time)
> 두 곳에서 모두 수행된다. 웹훅은 참조된 CRD가 존재할 때만 검사하며, 미존재 시
> 컨트롤러가 `Compatible` 상태 조건으로 처리한다.

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `BindCRD` | `api/v1alpha1.PillarBinding` CRD 및 상태 |
| `BindCtrl` | `internal/controller.PillarBindingReconciler` |
| `BindWH` | `internal/webhook/v1alpha1.PillarBindingCustomValidator` |
| `BindDef` | `internal/webhook/v1alpha1.PillarBindingCustomDefaulter` |
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD |
| `PProtCRD` | `api/v1alpha1.PillarProtocol` CRD |
| `SC` | `storage.k8s.io/v1.StorageClass` |

---

### E25.1 유효한 스펙으로 생성

**목적:** 유효한 `poolRef`와 `protocolRef`로 PillarBinding을 생성할 수 있음을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.1.1 | `TestPillarBindingWebhook_ValidCreate_BasicSpec` | `poolRef`와 `protocolRef`가 설정된 유효한 스펙으로 ValidateCreate 통과 | envtest; PillarBinding CRD 설치; `PillarBindingCustomValidator` 인스턴스 생성 | 1) `spec.poolRef="some-pool"`, `spec.protocolRef="some-protocol"`로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `BindWH` |
| E25.1.2 | `TestPillarBindingController_FinalizerAddedOnFirstReconcile` | PillarBinding 생성 후 첫 번째 `Reconcile` 호출에서 `binding-protection` 파이널라이저 자동 추가 | envtest; `PillarBindingReconciler` 초기화; PillarBinding 생성 | 1) `k8sClient.Create(ctx, binding)` 실행; 2) `reconciler.Reconcile(ctx, req)` 1회 호출 | PillarBinding에 `pillar-csi.bhyoo.com/binding-protection` 파이널라이저 존재; `result.RequeueAfter==0` | `BindCRD`, `BindCtrl` |
| E25.1.3 | `TestPillarBindingController_FinalizerNotDuplicated` | 동일 PillarBinding을 두 번 조정해도 파이널라이저 중복 없음 | envtest; PillarBinding 생성; 첫 조정으로 파이널라이저 추가 완료 | 1) 두 번째 `reconciler.Reconcile(ctx, req)` 호출 | 파이널라이저 개수 정확히 1개 | `BindCRD`, `BindCtrl` |

---

### E25.2 잘못된 스펙으로 생성 거부 — CRD 스키마 검증

**목적:** kubebuilder 마커에 의해 필수 참조 필드가 빈 문자열이거나 열거형 외 값인 경우
Kubernetes API 서버 수준에서 거부됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.2.1 | `TestPillarBindingCRD_InvalidCreate_EmptyPoolRef` | `spec.poolRef`가 빈 문자열인 경우 API 서버가 HTTP 422로 거부 | envtest; PillarBinding CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.poolRef=""`, `spec.protocolRef="some-protocol"`로 `k8sClient.Create(ctx, binding)` 호출 | 오류 반환; HTTP 422 UnprocessableEntity; `spec.poolRef` 필드 검증 실패 메시지 포함 | `BindCRD` |
| E25.2.2 | `TestPillarBindingCRD_InvalidCreate_EmptyProtocolRef` | `spec.protocolRef`가 빈 문자열인 경우 API 서버가 거부 | envtest; PillarBinding CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.poolRef="some-pool"`, `spec.protocolRef=""`로 Create 호출 | 오류 반환; HTTP 422; `spec.protocolRef` 필드 검증 실패 | `BindCRD` |
| E25.2.3 | `TestPillarBindingCRD_InvalidCreate_InvalidReclaimPolicy` | `spec.storageClass.reclaimPolicy`에 허용 외 값 설정 시 거부 | envtest; PillarBinding CRD 설치 (`Enum=Delete;Retain` 마커 포함) | 1) `spec.storageClass.reclaimPolicy="Archive"`으로 Create 호출 | 오류 반환; HTTP 422; `spec.storageClass.reclaimPolicy` 열거형 검증 실패 | `BindCRD` |

---

### E25.3 불변 필드 업데이트 거부 — 웹훅 검증

**목적:** `spec.poolRef`와 `spec.protocolRef`는 생성 후 변경할 수 없음을 확인한다.
StorageClass는 특정 풀과 프로토콜에 묶여 있어, 변경 시 기존 PVC 프로비저닝이
침묵 속에 다른 백엔드로 리디렉션될 수 있다.
이 검증은 `internal/webhook/v1alpha1.PillarBindingCustomValidator.ValidateUpdate`에서 수행된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.3.1 | `TestPillarBindingWebhook_ImmutableUpdate_PoolRefChange` | `spec.poolRef` 변경 시 `field.Forbidden` 오류 반환 | `oldObj.spec.poolRef="pool-a"`; `newObj.spec.poolRef="pool-b"` (변경); `protocolRef` 동일 | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; 오류 메시지에 `"poolRef"` 포함; 이전값 `"pool-a"`, 신규값 `"pool-b"` 언급 | `BindWH` |
| E25.3.2 | `TestPillarBindingWebhook_ImmutableUpdate_ProtocolRefChange` | `spec.protocolRef` 변경 시 `field.Forbidden` 오류 반환 | `oldObj.spec.protocolRef="proto-a"`; `newObj.spec.protocolRef="proto-b"` (변경); `poolRef` 동일 | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; 오류 메시지에 `"protocolRef"` 포함 | `BindWH` |
| E25.3.3 | `TestPillarBindingWebhook_MutableUpdate_StorageClassFieldsChange` | `spec.storageClass.reclaimPolicy` 변경은 허용 (비식별 필드) | `oldObj.spec.storageClass.reclaimPolicy="Delete"`; `newObj.spec.storageClass.reclaimPolicy="Retain"`; `poolRef`, `protocolRef` 동일 | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err=nil`; `warnings=nil`; 허용 | `BindWH` |

---

### E25.4 Defaulting 웹훅 — allowVolumeExpansion 자동 설정

**목적:** PillarBinding 생성 시 `spec.storageClass.allowVolumeExpansion`이 명시적으로
설정되지 않은 경우, Defaulting 웹훅이 참조된 PillarPool의 백엔드 타입을 조회하여
자동으로 적절한 값을 설정함을 확인한다.
이 로직은 `internal/webhook/v1alpha1.PillarBindingCustomDefaulter.Default`에서 수행된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.4.1 | `TestPillarBindingDefaulter_AllowVolumeExpansion_True_ZFSZvol` | `backend.type="zfs-zvol"` 풀 참조 시 `allowVolumeExpansion=true` 자동 설정 | envtest; `backend.type="zfs-zvol"` PillarPool 생성; PillarBinding에 `allowVolumeExpansion` 미설정 | 1) `defaulter.Default(ctx, obj)` 호출 | `spec.storageClass.allowVolumeExpansion=true` | `BindDef`, `PoolCRD` |
| E25.4.2 | `TestPillarBindingDefaulter_AllowVolumeExpansion_True_LVMLV` | `backend.type="lvm-lv"` 풀 참조 시 `allowVolumeExpansion=true` 자동 설정 | envtest; `backend.type="lvm-lv"` PillarPool 생성; PillarBinding에 `allowVolumeExpansion` 미설정 | 1) `defaulter.Default(ctx, obj)` 호출 | `spec.storageClass.allowVolumeExpansion=true` | `BindDef`, `PoolCRD` |
| E25.4.3 | `TestPillarBindingDefaulter_AllowVolumeExpansion_False_ZFSDataset` | `backend.type="zfs-dataset"` 풀 참조 시 `allowVolumeExpansion=false` 자동 설정 | envtest; `backend.type="zfs-dataset"` PillarPool 생성; PillarBinding에 `allowVolumeExpansion` 미설정 | 1) `defaulter.Default(ctx, obj)` 호출 | `spec.storageClass.allowVolumeExpansion=false` | `BindDef`, `PoolCRD` |
| E25.4.4 | `TestPillarBindingDefaulter_AllowVolumeExpansion_False_Dir` | `backend.type="dir"` 풀 참조 시 `allowVolumeExpansion=false` 자동 설정 | envtest; `backend.type="dir"` PillarPool 생성; PillarBinding에 `allowVolumeExpansion` 미설정 | 1) `defaulter.Default(ctx, obj)` 호출 | `spec.storageClass.allowVolumeExpansion=false` | `BindDef`, `PoolCRD` |
| E25.4.5 | `TestPillarBindingDefaulter_AllowVolumeExpansion_NotOverridden_Explicit` | `allowVolumeExpansion`이 명시적으로 설정된 경우 Defaulter가 덮어쓰지 않음 | envtest; `backend.type="zfs-zvol"` PillarPool(기본값은 true); PillarBinding에 `allowVolumeExpansion=false` 명시 | 1) `defaulter.Default(ctx, obj)` 호출 | `spec.storageClass.allowVolumeExpansion=false` (명시값 유지) | `BindDef`, `PoolCRD` |
| E25.4.6 | `TestPillarBindingDefaulter_AllowVolumeExpansion_NilWhenPoolNotFound` | 참조 PillarPool이 없을 때 `allowVolumeExpansion` 설정 건너뜀 (nil 유지, 오류 없음) | envtest; `poolRef="nonexistent-pool"` — pool 미존재; PillarBinding에 `allowVolumeExpansion` 미설정 | 1) `defaulter.Default(ctx, obj)` 호출 | `spec.storageClass.allowVolumeExpansion=nil`; 오류 없음 (graceful skip) | `BindDef` |

---

### E25.5 백엔드-프로토콜 호환성 웹훅 검증

**목적:** Validating 웹훅이 참조된 PillarPool의 백엔드 타입과 PillarProtocol의 프로토콜 타입의
호환성을 검증함을 확인한다. 블록 백엔드(zfs-zvol, lvm-lv)는 블록 프로토콜(nvmeof-tcp, iscsi)만
허용하고, 파일 백엔드(zfs-dataset, dir)는 파일 프로토콜(nfs)만 허용한다.

**호환성 매트릭스:**

| 백엔드 타입 | nvmeof-tcp | iscsi | nfs |
|------------|:---------:|:-----:|:---:|
| zfs-zvol   | ✅ | ✅ | ❌ |
| lvm-lv     | ✅ | ✅ | ❌ |
| zfs-dataset| ❌ | ❌ | ✅ |
| dir        | ❌ | ❌ | ✅ |

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.5.1 | `TestPillarBindingWebhook_Compatible_ZFSZvol_NVMeOFTCP` | 블록 백엔드(zfs-zvol) + 블록 프로토콜(nvmeof-tcp) → 허용 | envtest; `backend.type="zfs-zvol"` PillarPool; `type="nvmeof-tcp"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.2 | `TestPillarBindingWebhook_Compatible_LVMLV_ISCSI` | 블록 백엔드(lvm-lv) + 블록 프로토콜(iscsi) → 허용 | envtest; `backend.type="lvm-lv"` PillarPool; `type="iscsi"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.3 | `TestPillarBindingWebhook_Compatible_ZFSDataset_NFS` | 파일 백엔드(zfs-dataset) + 파일 프로토콜(nfs) → 허용 | envtest; `backend.type="zfs-dataset"` PillarPool; `type="nfs"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.4 | `TestPillarBindingWebhook_Compatible_Dir_NFS` | 파일 백엔드(dir) + 파일 프로토콜(nfs) → 허용 | envtest; `backend.type="dir"` PillarPool; `type="nfs"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.5 | `TestPillarBindingWebhook_Incompatible_ZFSZvol_NFS` | 블록 백엔드(zfs-zvol) + 파일 프로토콜(nfs) → 거부; `spec.protocolRef` 경로 오류 | envtest; `backend.type="zfs-zvol"` PillarPool; `type="nfs"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err != nil`; 오류 메시지에 `"incompatible"` 포함; `spec.protocolRef` 경로 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.6 | `TestPillarBindingWebhook_Incompatible_LVMLV_NFS` | 블록 백엔드(lvm-lv) + 파일 프로토콜(nfs) → 거부 | envtest; `backend.type="lvm-lv"` PillarPool; `type="nfs"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err != nil`; `"incompatible"` 포함 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.7 | `TestPillarBindingWebhook_Incompatible_ZFSDataset_NVMeOFTCP` | 파일 백엔드(zfs-dataset) + 블록 프로토콜(nvmeof-tcp) → 거부 | envtest; `backend.type="zfs-dataset"` PillarPool; `type="nvmeof-tcp"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err != nil`; `"incompatible"` 포함 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.8 | `TestPillarBindingWebhook_Incompatible_Dir_ISCSI` | 파일 백엔드(dir) + 블록 프로토콜(iscsi) → 거부 | envtest; `backend.type="dir"` PillarPool; `type="iscsi"` PillarProtocol | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err != nil`; `"incompatible"` 포함 | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.9 | `TestPillarBindingWebhook_CompatibilitySkipped_PoolNotFound` | pool 미존재 시 호환성 검사 건너뜀 — 컨트롤러 `Compatible` 조건으로 위임 | envtest; PillarProtocol 존재; PillarPool 미존재 | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 (graceful skip) | `BindWH` |
| E25.5.10 | `TestPillarBindingWebhook_CompatibilitySkipped_ProtocolNotFound` | protocol 미존재 시 호환성 검사 건너뜀 — 컨트롤러 `Compatible` 조건으로 위임 | envtest; PillarPool 존재; PillarProtocol 미존재 | 1) `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 (graceful skip) | `BindWH` |

---

### E25.6 상태 조건 전이 — PoolReady

**목적:** `PoolReady` 조건이 참조된 PillarPool의 존재 여부와 Ready 상태에 따라
올바르게 설정됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.6.1 | `TestPillarBindingController_PoolReady_False_PoolNotFound` | 참조 PillarPool 미존재 시 `PoolReady=False/PoolNotFound`; `RequeueAfter=15s`; `Ready.Reason="PoolNotFound"` | envtest; PillarBinding + 파이널라이저; PillarPool 미존재 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `PoolReady.Status=False`; `PoolReady.Reason="PoolNotFound"`; `PoolReady.Message`에 pool 이름 포함; `result.RequeueAfter=15s` | `BindCRD`, `BindCtrl`, `PoolCRD` |
| E25.6.2 | `TestPillarBindingController_PoolReady_False_PoolNotReady` | 참조 PillarPool의 Ready 조건이 False일 때 `PoolReady=False/PoolNotReady`; pool의 오류 메시지 전파 | envtest; PillarBinding + 파이널라이저; PillarPool(Ready=False, message="target not found") | 1) `reconciler.Reconcile(ctx, req)` 호출 | `PoolReady.Status=False`; `PoolReady.Reason="PoolNotReady"`; `PoolReady.Message`에 `"target not found"` 포함; `result.RequeueAfter=15s` | `BindCRD`, `BindCtrl`, `PoolCRD` |
| E25.6.3 | `TestPillarBindingController_PoolReady_False_PoolNoCondition` | PillarPool에 Ready 조건이 아직 없을 때 `PoolReady=False/PoolNotReady` | envtest; PillarBinding + 파이널라이저; PillarPool(조건 없음) | 1) `reconciler.Reconcile(ctx, req)` 호출 | `PoolReady.Status=False`; `PoolReady.Reason="PoolNotReady"`; `result.RequeueAfter=15s` | `BindCRD`, `BindCtrl`, `PoolCRD` |

---

### E25.7 상태 조건 전이 — ProtocolValid

**목적:** `ProtocolValid` 조건이 참조된 PillarProtocol의 존재 여부와 Ready 상태에 따라
올바르게 설정됨을 확인한다. Pool이 Ready인 상태에서만 Protocol 검증이 진행된다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.7.1 | `TestPillarBindingController_ProtocolValid_False_ProtocolNotFound` | PillarPool Ready + PillarProtocol 미존재 시 `ProtocolValid=False/ProtocolNotFound`; `PoolReady=True` 유지 | envtest; PillarBinding + 파이널라이저; PillarPool(Ready=True); PillarProtocol 미존재 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `ProtocolValid.Status=False`; `Reason="ProtocolNotFound"`; `ProtocolValid.Message`에 protocol 이름 포함; `PoolReady.Status=True`; `result.RequeueAfter=15s` | `BindCRD`, `BindCtrl`, `PProtCRD` |
| E25.7.2 | `TestPillarBindingController_ProtocolValid_False_ProtocolNotReady` | PillarProtocol Ready 조건이 False일 때 `ProtocolValid=False/ProtocolNotReady`; protocol 오류 메시지 전파 | envtest; PillarBinding + 파이널라이저; PillarPool(Ready=True); PillarProtocol(Ready=False, message="initialization failed") | 1) `reconciler.Reconcile(ctx, req)` 호출 | `ProtocolValid.Status=False`; `Reason="ProtocolNotReady"`; `ProtocolValid.Message`에 `"initialization failed"` 포함 | `BindCRD`, `BindCtrl`, `PProtCRD` |
| E25.7.3 | `TestPillarBindingController_ProtocolValid_False_ProtocolNoCondition` | PillarProtocol에 Ready 조건이 없을 때 `ProtocolValid=False/ProtocolNotReady` | envtest; PillarBinding + 파이널라이저; PillarPool(Ready=True); PillarProtocol(조건 없음) | 1) `reconciler.Reconcile(ctx, req)` 호출 | `ProtocolValid.Status=False`; `Reason="ProtocolNotReady"`; `result.RequeueAfter=15s` | `BindCRD`, `BindCtrl`, `PProtCRD` |

---

### E25.8 상태 조건 전이 — Compatible 및 Ready 종합

**목적:** `Compatible` 조건이 백엔드-프로토콜 호환성을 컨트롤러 측에서 검증하고,
`Ready` 최상위 조건이 모든 하위 조건을 올바르게 종합함을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.8.1 | `TestPillarBindingController_Compatible_True_AllConditionsMet` | zfs-zvol + nvmeof-tcp 조합 → `Compatible=True`; `Ready=True/AllConditionsMet`; StorageClass 생성 | envtest; PillarBinding + 파이널라이저; PillarPool(zfs-zvol, Ready=True); PillarProtocol(nvmeof-tcp, Ready=True) | 1) `reconciler.Reconcile(ctx, req)` 호출 | `PoolReady=True`; `ProtocolValid=True`; `Compatible=True`; `StorageClassCreated=True`; `Ready=True`; `Ready.Reason="AllConditionsMet"` | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD`, `SC` |
| E25.8.2 | `TestPillarBindingController_Compatible_False_BlockBackend_FileProtocol` | zfs-zvol + nfs 비호환 → `Compatible=False/Incompatible`; `Ready=False`; StorageClass 미생성 | envtest; PillarBinding + 파이널라이저; PillarPool(zfs-zvol, Ready=True); PillarProtocol(nfs, Ready=True) | 1) `reconciler.Reconcile(ctx, req)` 호출 | `Compatible.Status=False`; `Compatible.Reason="Incompatible"`; `Compatible.Message`에 `"zfs-zvol"` 포함; `Ready.Status=False`; StorageClass 미생성 | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD` |
| E25.8.3 | `TestPillarBindingController_NoRequeue_WhenReady` | 모든 조건 충족 시 `result.RequeueAfter==0` — 불필요한 재조정 없음 | envtest; 모든 조건 충족 (PillarPool/Protocol Ready, 호환 가능) | 1) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter==0` | `BindCRD`, `BindCtrl` |

---

### E25.9 StorageClass 생성 및 소유권

**목적:** PillarBinding이 Ready 상태가 되면 자동으로 StorageClass를 생성하고,
해당 StorageClass에 올바른 ownerReference, provisioner, 파라미터가 설정됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.9.1 | `TestPillarBindingController_StorageClass_OwnerReference` | 생성된 StorageClass에 PillarBinding을 가리키는 ownerReference(`Kind=PillarBinding`, `controller=true`) 설정 | envtest; PillarBinding + 파이널라이저; PillarPool(Ready=True); PillarProtocol(Ready=True) | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) StorageClass 조회 | `len(sc.OwnerReferences)==1`; `sc.OwnerReferences[0].Kind="PillarBinding"`; `*sc.OwnerReferences[0].Controller=true` | `BindCRD`, `BindCtrl`, `SC` |
| E25.9.2 | `TestPillarBindingController_StorageClass_Provisioner` | 생성된 StorageClass의 provisioner가 `"pillar-csi.bhyoo.com"` | envtest; 동일 사전 조건 | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) StorageClass 조회 | `sc.Provisioner="pillar-csi.bhyoo.com"` | `BindCRD`, `BindCtrl`, `SC` |
| E25.9.3 | `TestPillarBindingController_StorageClass_Parameters` | 생성된 StorageClass의 parameters에 pool, protocol, backend-type, protocol-type 파라미터 포함 | envtest; PillarPool(zfs-zvol, Ready=True); PillarProtocol(nvmeof-tcp, Ready=True) | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) `sc.Parameters` 검사 | `sc.Parameters["pillar-csi.bhyoo.com/pool"]=poolName`; `"pillar-csi.bhyoo.com/protocol"=protocolName`; `"pillar-csi.bhyoo.com/backend-type"="zfs-zvol"`; `"pillar-csi.bhyoo.com/protocol-type"="nvmeof-tcp"` | `BindCRD`, `BindCtrl`, `SC` |
| E25.9.4 | `TestPillarBindingController_StorageClass_DefaultReclaimPolicy` | StorageClass ReclaimPolicy 기본값 `Delete` | envtest; 기본 PillarBinding (reclaimPolicy 미설정); PillarPool/Protocol Ready | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) `sc.ReclaimPolicy` 검사 | `*sc.ReclaimPolicy=PersistentVolumeReclaimDelete` | `BindCRD`, `BindCtrl`, `SC` |
| E25.9.5 | `TestPillarBindingController_StorageClass_DefaultVolumeBindingMode` | StorageClass VolumeBindingMode 기본값 `Immediate` | envtest; 기본 PillarBinding (volumeBindingMode 미설정); PillarPool/Protocol Ready | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) `sc.VolumeBindingMode` 검사 | `*sc.VolumeBindingMode=VolumeBindingImmediate` | `BindCRD`, `BindCtrl`, `SC` |
| E25.9.6 | `TestPillarBindingController_StorageClass_StatusStorageClassName` | StorageClass 생성 후 `status.storageClassName`에 이름 반영 | envtest; 기본 PillarBinding; PillarPool/Protocol Ready | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) PillarBinding 상태 조회 | `binding.status.storageClassName=bindingName` | `BindCRD`, `BindCtrl`, `SC` |
| E25.9.7 | `TestPillarBindingController_StorageClass_CustomName` | `spec.storageClass.name` 설정 시 해당 이름으로 StorageClass 생성; 바인딩 이름으로는 미생성; `status.storageClassName` 업데이트 | envtest; PillarBinding(`spec.storageClass.name="my-custom-sc"`); PillarPool/Protocol Ready | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) `"my-custom-sc"` StorageClass 조회; 3) bindingName StorageClass 조회 | `"my-custom-sc"` StorageClass 존재; bindingName StorageClass 미존재(NotFound); `status.storageClassName="my-custom-sc"` | `BindCRD`, `BindCtrl`, `SC` |

---

### E25.10 StorageClass 커스텀 설정

**목적:** `spec.storageClass` 필드의 커스텀 설정이 생성된 StorageClass에 정확히 반영됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.10.1 | `TestPillarBindingController_StorageClass_ReclaimPolicy_Retain` | `spec.storageClass.reclaimPolicy="Retain"` 설정 시 StorageClass에 반영 | envtest; PillarBinding(`reclaimPolicy=Retain`); PillarPool/Protocol Ready | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) StorageClass 조회 | `*sc.ReclaimPolicy=PersistentVolumeReclaimRetain` | `BindCRD`, `BindCtrl`, `SC` |
| E25.10.2 | `TestPillarBindingController_StorageClass_VolumeBindingMode_WaitForFirstConsumer` | `spec.storageClass.volumeBindingMode="WaitForFirstConsumer"` 설정 시 StorageClass에 반영 | envtest; PillarBinding(`volumeBindingMode=WaitForFirstConsumer`); PillarPool/Protocol Ready | 1) `reconciler.Reconcile(ctx, req)` 호출; 2) StorageClass 조회 | `*sc.VolumeBindingMode=VolumeBindingWaitForFirstConsumer` | `BindCRD`, `BindCtrl`, `SC` |

---

### E25.11 삭제 보호 동작

**목적:** `pillar-csi.bhyoo.com/binding-protection` 파이널라이저가 StorageClass를
참조하는 PVC가 존재하는 동안 PillarBinding 삭제를 차단하고, PVC 삭제 후에야
파이널라이저가 제거되어 StorageClass도 함께 삭제됨을 확인한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.11.1 | `TestPillarBindingController_DeletionBlocked_PVCExists` | StorageClass를 참조하는 PVC 존재 시 삭제 차단; `result.RequeueAfter=10s`; `Ready=False/DeletionBlocked` | envtest; PillarBinding(StorageClass 생성 완료); PVC가 해당 StorageClass 참조; 삭제 요청 | 1) `k8sClient.Delete(ctx, binding)` 호출; 2) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=10s`; `Ready.Reason="DeletionBlocked"`; `Ready.Message`에 PVC 이름 포함 | `BindCRD`, `BindCtrl`, `SC` |
| E25.11.2 | `TestPillarBindingController_DeletionBlocked_FinalizerKept` | 삭제 차단 중 파이널라이저 제거되지 않음 | envtest; 동일 사전 조건 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `controllerutil.ContainsFinalizer(fetched, pillarBindingFinalizer)=true` | `BindCRD`, `BindCtrl` |
| E25.11.3 | `TestPillarBindingController_DeletionAllowed_NoPVCs` | 참조 PVC 없을 때 StorageClass 삭제 후 파이널라이저 제거; `result.RequeueAfter==0` | envtest; PillarBinding(StorageClass 생성 완료); PVC 없음; 삭제 요청 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter==0`; StorageClass 삭제(NotFound); 파이널라이저 제거 | `BindCRD`, `BindCtrl`, `SC` |
| E25.11.4 | `TestPillarBindingController_DeletionAllowed_AfterPVCRemoval` | PVC 제거 후 다음 조정에서 파이널라이저 제거 및 StorageClass 삭제 완료 | envtest; 첫 조정에서 PVC로 차단 확인; 이후 PVC 삭제 | 1) 첫 조정: 차단; 2) PVC 삭제; 3) 두 번째 조정 실행 | 두 번째 조정 후 파이널라이저 제거; StorageClass 삭제; `k8sClient.Get(binding)` → NotFound | `BindCRD`, `BindCtrl`, `SC` |

---

**총 E25 테스트 케이스: 41개** (E25.1: 3개, E25.2: 3개, E25.3: 3개, E25.4: 6개, E25.5: 10개, E25.6: 3개, E25.7: 3개, E25.8: 3개, E25.9: 7개, E25.10: 2개, E25.11: 4개)

---

**총 카테고리 1.5 테스트 케이스 (업데이트): 127개** (E19: 19개, E20: 20개, E23: 24개, E25: 41개, E26: 23개 → E21은 다른 카탈로그에 포함)

---

## E26: 교차-CRD 라이프사이클 상호작용

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/CrossCRD'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/CrossCRD'
```

**목적:**
여러 CRD(PillarTarget → PillarPool → PillarBinding ← PillarProtocol)의
**교차-CRD 라이프사이클 상호작용**을 검증한다.
단일 CRD의 생성·삭제를 검증하는 E19/E20/E23/E25와 달리, 이 섹션은 아래
세 가지 측면을 집중 검증한다:

1. **의존 순서 (Dependency Ordering)** — 참조 CRD가 없거나 Not-Ready일 때
   하위 CRD의 상태 조건이 올바르게 `False`로 설정됨
2. **연쇄 상태 업데이트 (Cascading Status Updates)** — 상위 CRD 상태 변화가
   하위 CRD에 전파됨
3. **삭제 보호 (Deletion Protection)** — 의존 리소스가 존재할 때 삭제가
   파이널라이저 메커니즘으로 차단됨

> **CI 실행 가능 여부:** ✅ CI에서 실행 가능 — envtest 사용.
>
> **한계:** envtest에서는 컨트롤러 간 자동 watch/event 전파가 실제 환경보다
> 느리다. 각 Reconcile을 명시적으로 순차 호출하여 전파를 재현한다.
> 실제 운영 환경의 즉각적 전파는 Kind 클러스터(유형 B) 또는 수동 스테이징에서
> 검증한다.
>
> **미구현 기능 문서화:** 컨트롤러가 삭제 보호 파이널라이저를 아직 구현하지
> 않은 경우, E26.3.x 테스트는 실패한다. 이는 의도된 동작으로, 미구현 기능의
> 명세를 문서화한다.

**컴포넌트 의존성 그래프:**

```
PillarTarget (pt)
  └─(targetRef)──► PillarPool (pp)
                     └─(poolRef)────► PillarBinding (pb) ──► StorageClass ──► PVC
PillarProtocol (ppr)
  └─(protocolRef)──► PillarBinding (pb)
```

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `TgtCRD` | `api/v1alpha1.PillarTarget` CRD 및 상태 |
| `TgtCtrl` | `internal/controller.PillarTargetReconciler` |
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD 및 상태 |
| `PoolCtrl` | `internal/controller.PillarPoolReconciler` |
| `PProtCRD` | `api/v1alpha1.PillarProtocol` CRD 및 상태 |
| `PProtCtrl` | `internal/controller.PillarProtocolReconciler` |
| `BindCRD` | `api/v1alpha1.PillarBinding` CRD 및 상태 |
| `BindCtrl` | `internal/controller.PillarBindingReconciler` |
| `SC` | `storage.k8s.io/v1.StorageClass` |

---

### E26.1 의존 순서 — 참조 CRD 없음/Not-Ready 시 하위 조건 차단

**목적:** 상위 CRD가 존재하지 않거나 Not-Ready 상태일 때 하위 CRD의 상태 조건이
올바르게 `False`로 설정되고 `Ready` 조건도 `False`로 유지됨을 확인한다.

**PillarPool 상태 조건 검증 (targetRef 의존성):**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.1.1 | `TestCrossLifecycle_Pool_TargetMissing_TargetReadyFalse` | PillarPool 생성 시 참조 PillarTarget이 없으면 `TargetReady=False` 조건 설정 | envtest; PillarPool(`targetRef="nonexistent-target"`) 생성; PillarTarget 미등록 | 1) PillarPool 생성; 2) `poolReconciler.Reconcile(ctx, req)` 호출; 3) PillarPool 상태 조회 | `TargetReady.Status=False`; `TargetReady.Reason="TargetNotFound"`; `Ready.Status=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.1.2 | `TestCrossLifecycle_Pool_TargetNotReady_TargetReadyFalse` | 참조 PillarTarget이 존재하지만 `Ready=False`이면 `TargetReady=False` 조건 설정 | envtest; PillarTarget(`Ready=False, reason="AgentUnhealthy"`) 등록; PillarPool(`targetRef=target`) 생성 | 1) PillarPool 생성; 2) `poolReconciler.Reconcile(ctx, req)` 호출; 3) PillarPool 상태 조회 | `TargetReady.Status=False`; `TargetReady.Reason="TargetNotReady"`; `Ready.Status=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.1.3 | `TestCrossLifecycle_Pool_TargetReady_TargetReadyTrue` | 참조 PillarTarget이 `Ready=True`이면 `TargetReady=True` 조건 설정 | envtest; PillarTarget(`Ready=True, reason="Authenticated"`) 등록; PillarPool(`targetRef=target`) 생성 | 1) PillarPool 생성; 2) `poolReconciler.Reconcile(ctx, req)` 호출; 3) PillarPool 상태 조회 | `TargetReady.Status=True`; `TargetReady.Reason="TargetReady"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

**PillarBinding 상태 조건 검증 (poolRef / protocolRef 의존성):**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.1.4 | `TestCrossLifecycle_Binding_PoolMissing_PoolReadyFalse` | PillarBinding 생성 시 참조 PillarPool이 없으면 `PoolReady=False` 조건 설정 | envtest; PillarBinding(`poolRef="nonexistent-pool"`, `protocolRef="valid-proto"`) 생성; PillarPool 미등록; PillarProtocol 등록 | 1) PillarBinding 생성; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) PillarBinding 상태 조회 | `PoolReady.Status=False`; `PoolReady.Reason="PoolNotFound"`; `Ready.Status=False`; StorageClass 미생성 | `BindCRD`, `BindCtrl`, `PoolCRD` |
| E26.1.5 | `TestCrossLifecycle_Binding_PoolNotReady_PoolReadyFalse` | 참조 PillarPool이 존재하지만 `Ready=False`이면 `PoolReady=False` 조건 설정 | envtest; PillarPool(`Ready=False`) 등록; PillarBinding(`poolRef=pool`) 생성 | 1) PillarBinding 생성; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) PillarBinding 상태 조회 | `PoolReady.Status=False`; `PoolReady.Reason="PoolNotReady"`; `Ready.Status=False` | `BindCRD`, `BindCtrl`, `PoolCRD` |
| E26.1.6 | `TestCrossLifecycle_Binding_ProtocolMissing_ProtocolValidFalse` | PillarBinding 생성 시 참조 PillarProtocol이 없으면 `ProtocolValid=False` 조건 설정 | envtest; PillarBinding(`poolRef="valid-pool"`, `protocolRef="nonexistent-protocol"`) 생성; PillarProtocol 미등록; PillarPool 등록 | 1) PillarBinding 생성; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) PillarBinding 상태 조회 | `ProtocolValid.Status=False`; `ProtocolValid.Reason="ProtocolNotFound"`; `Ready.Status=False` | `BindCRD`, `BindCtrl`, `PProtCRD` |
| E26.1.7 | `TestCrossLifecycle_Binding_BothMissing_BothConditionsFalse` | PillarPool과 PillarProtocol 둘 다 없을 때 두 조건 모두 `False` | envtest; PillarBinding(`poolRef="missing-pool"`, `protocolRef="missing-proto"`) 생성; 둘 다 미등록 | 1) PillarBinding 생성; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) PillarBinding 상태 조회 | `PoolReady.Status=False`; `ProtocolValid.Status=False`; `Ready.Status=False`; StorageClass 미생성 | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD` |
| E26.1.8 | `TestCrossLifecycle_Binding_PoolReadyProtocolReady_BecomeReady` | Pool `Ready=True` + Protocol `Ready=True` → Binding `Ready=True`, StorageClass 생성 | envtest; PillarPool(`Ready=True`, `backend.type="zfs-zvol"`) 등록; PillarProtocol(`Ready=True`, `type="nvmeof-tcp"`) 등록; PillarBinding 생성 | 1) PillarBinding 생성; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) PillarBinding 상태 및 StorageClass 조회 | `PoolReady.Status=True`; `ProtocolValid.Status=True`; `Compatible.Status=True`; `StorageClassCreated.Status=True`; `Ready.Status=True`; StorageClass 존재 | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD`, `SC` |

---

### E26.2 연쇄 상태 업데이트 — 상위 CRD 상태 변화 하위 전파

**목적:** 상위 CRD의 Ready 상태 변화가 하위 CRD 상태 조건에 올바르게 전파됨을 확인한다.
이 테스트는 envtest에서 각 컨트롤러를 순차적으로 호출하여 상태 전파 시나리오를 재현한다.

> **CI 실행 가능 여부:** ✅ envtest 사용.
>
> **한계:** 실제 운영 환경에서는 Watch 이벤트로 자동 전파되나, envtest에서는
> 각 Reconcile을 명시적으로 호출하여 단계별로 검증한다.

**PillarTarget → PillarPool 연쇄 상태 업데이트:**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.2.1 | `TestCrossLifecycle_Cascade_TargetLosesReady_PoolConditionUpdates` | PillarTarget이 `Ready=True→False`로 전환 시 PillarPool의 `TargetReady` 조건이 `False`로 전이 | envtest; PillarTarget(`Ready=True`) + PillarPool(`TargetReady=True`, `Ready=True`) 초기 상태 | 1) PillarTarget 상태를 `Ready=False`로 갱신; 2) `poolReconciler.Reconcile(ctx, req)` 호출; 3) PillarPool 상태 조회 | `PoolCRD.TargetReady.Status=False`; `PoolCRD.Ready.Status=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.2.2 | `TestCrossLifecycle_Cascade_TargetRecovery_PoolConditionRestores` | PillarTarget이 `Ready=False→True`로 회복 시 PillarPool `TargetReady=True` 복원 | envtest; PillarTarget(`Ready=False`) + PillarPool(`TargetReady=False`) 초기 상태 | 1) PillarTarget 상태를 `Ready=True`로 갱신; 2) `poolReconciler.Reconcile(ctx, req)` 호출; 3) PillarPool 상태 조회 | `PoolCRD.TargetReady.Status=True`; `PoolCRD.Ready.Status=True` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

**PillarPool → PillarBinding 연쇄 상태 업데이트:**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.2.3 | `TestCrossLifecycle_Cascade_PoolLosesReady_BindingConditionUpdates` | PillarPool이 `Ready=True→False`로 전환 시 PillarBinding의 `PoolReady` 조건이 `False`로 전이 | envtest; PillarPool(`Ready=True`) + PillarBinding(`PoolReady=True`, `Ready=True`) 초기 상태; StorageClass 이미 생성됨 | 1) PillarPool 상태를 `Ready=False`로 갱신; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) PillarBinding 상태 조회; 4) StorageClass 조회 | `BindCRD.PoolReady.Status=False`; `BindCRD.Ready.Status=False`; StorageClass는 기존 PVC 보호를 위해 유지(삭제 안 됨) | `BindCRD`, `BindCtrl`, `PoolCRD`, `SC` |
| E26.2.4 | `TestCrossLifecycle_Cascade_ProtocolBecomesInvalid_BindingNotReady` | PillarProtocol이 `Ready=True→False`로 전환 시 PillarBinding `ProtocolValid=False` 전이 | envtest; PillarProtocol(`Ready=True`) + PillarBinding(`ProtocolValid=True`, `Ready=True`) 초기 상태 | 1) PillarProtocol 상태를 `Ready=False`로 갱신; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) PillarBinding 상태 조회 | `BindCRD.ProtocolValid.Status=False`; `BindCRD.Ready.Status=False` | `BindCRD`, `BindCtrl`, `PProtCRD` |

**전체 체인 연쇄 복원:**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.2.5 | `TestCrossLifecycle_Cascade_FullChainRecovery` | PillarTarget 회복 시 Pool→Binding 전체 체인 Ready 복원 | envtest; Target(`Ready=False`) → Pool(`TargetReady=False`, `Ready=False`) → Binding(`PoolReady=False`, `Ready=False`) 초기 상태 | 1) PillarTarget `Ready=True`로 갱신; 2) `poolReconciler.Reconcile(ctx, req)` 호출; 3) `bindingReconciler.Reconcile(ctx, req)` 호출; 4) 두 CRD 상태 조회 | `PoolCRD.TargetReady.Status=True`; `PoolCRD.Ready.Status=True`; `BindCRD.PoolReady.Status=True`; `BindCRD.Ready.Status=True` | `PoolCRD`, `PoolCtrl`, `BindCRD`, `BindCtrl`, `TgtCRD` |
| E26.2.6 | `TestCrossLifecycle_Cascade_BindingBecomesReady_StorageClassCreated` | 모든 상위 의존성 Ready 후 Binding Ready 전이 시 StorageClass 생성 | envtest; PillarPool(`Ready=False`) + PillarProtocol(`Ready=True`) + PillarBinding(`Ready=False`, StorageClass 미존재) 초기 상태 | 1) PillarPool `Ready=True`로 갱신; 2) `bindingReconciler.Reconcile(ctx, req)` 호출; 3) StorageClass 조회 | StorageClass 생성 확인; `BindCRD.StorageClassCreated.Status=True`; `BindCRD.Ready.Status=True` | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD`, `SC` |

**PillarProtocol bindingCount 연쇄 업데이트:**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.2.7 | `TestCrossLifecycle_Cascade_ProtocolBindingCount_IncrementOnCreate` | PillarBinding 조정 완료 시 PillarProtocol `status.bindingCount` 증가 | envtest; PillarProtocol(`bindingCount=0`) 등록; PillarBinding Ready 전이 완료 | 1) `protocolReconciler.Reconcile(ctx, req)` 호출; 2) PillarProtocol 상태 조회 | `PProtCRD.Status.BindingCount=1` | `PProtCRD`, `PProtCtrl`, `BindCRD` |

---

### E26.3 삭제 보호 — 의존 리소스 존재 시 삭제 차단

**목적:** 하위 CRD가 상위 CRD를 참조하고 있을 때 상위 CRD의 삭제가 파이널라이저 메커니즘으로
차단되고, 하위 CRD가 모두 삭제된 후에야 상위 CRD가 실제로 삭제됨을 확인한다.

> **CI 실행 가능 여부:** ✅ envtest 사용.
>
> **미구현 기능 문서화:** 파이널라이저 기반 삭제 보호(PillarTarget/PillarPool/PillarProtocol)는
> 현재 컨트롤러가 구현하지 않은 경우 E26.3.1~E26.3.7 테스트가 실패한다.
> 이 실패는 **의도된 동작**으로, 구현 누락을 명시적으로 드러낸다.
> E26.3.8(의존 없는 즉시 삭제)은 파이널라이저 미구현 환경에서도 통과해야 한다.

**삭제 보호 의존 그래프:**

```
PillarTarget ◄─────(참조) PillarPool ◄────(참조) PillarBinding ◄── PVC
     ↑ 차단                  ↑ 차단                   ↑ 차단
```

**PillarTarget 삭제 보호 (PillarPool이 참조 중):**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.3.1 | `TestCrossLifecycle_DeleteProtection_Target_BlockedByPool` | PillarPool이 `targetRef`로 참조하는 PillarTarget 삭제 시 파이널라이저로 차단 | envtest; PillarTarget(`target-1`) + PillarPool(`targetRef="target-1"`) 생성 완료; PillarTarget에 `pillar-csi.bhyoo.com/target-protection` 파이널라이저 존재 | 1) `k8sClient.Delete(ctx, target)` 호출; 2) `targetReconciler.Reconcile(ctx, req)` 호출; 3) PillarTarget 조회 | PillarTarget에 `DeletionTimestamp` 설정됨; 파이널라이저 유지; PillarTarget 여전히 존재(NotFound 아님) | `TgtCRD`, `TgtCtrl`, `PoolCRD` |
| E26.3.2 | `TestCrossLifecycle_DeleteProtection_Target_AllowedAfterPoolRemoved` | 참조 PillarPool 삭제 후 PillarTarget 삭제 완료 | envtest; E26.3.1 상태에서 시작; PillarPool 삭제 완료(finalizer 제거 포함) | 1) PillarPool 삭제 완료; 2) `targetReconciler.Reconcile(ctx, req)` 재호출; 3) PillarTarget 조회 | PillarTarget 삭제 완료(NotFound); 파이널라이저 제거 확인 | `TgtCRD`, `TgtCtrl`, `PoolCRD` |

**PillarPool 삭제 보호 (PillarBinding이 참조 중):**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.3.3 | `TestCrossLifecycle_DeleteProtection_Pool_BlockedByBinding` | PillarBinding이 `poolRef`로 참조하는 PillarPool 삭제 시 파이널라이저로 차단 | envtest; PillarPool(`pool-1`) + PillarBinding(`poolRef="pool-1"`) 생성 완료; PillarPool에 `pillar-csi.bhyoo.com/pool-protection` 파이널라이저 존재 | 1) `k8sClient.Delete(ctx, pool)` 호출; 2) `poolReconciler.Reconcile(ctx, req)` 호출; 3) PillarPool 조회 | PillarPool에 `DeletionTimestamp` 설정됨; 파이널라이저 유지; PillarPool 여전히 존재 | `PoolCRD`, `PoolCtrl`, `BindCRD` |
| E26.3.4 | `TestCrossLifecycle_DeleteProtection_Pool_AllowedAfterBindingRemoved` | 참조 PillarBinding 삭제 후 PillarPool 삭제 완료 | envtest; E26.3.3 상태에서 시작; PillarBinding 삭제 완료 | 1) PillarBinding 삭제 완료; 2) `poolReconciler.Reconcile(ctx, req)` 재호출; 3) PillarPool 조회 | PillarPool 삭제 완료(NotFound); 파이널라이저 제거 확인 | `PoolCRD`, `PoolCtrl`, `BindCRD` |

**PillarProtocol 삭제 보호 (PillarBinding이 참조 중):**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.3.5 | `TestCrossLifecycle_DeleteProtection_Protocol_BlockedByBinding` | PillarBinding이 `protocolRef`로 참조하는 PillarProtocol 삭제 시 파이널라이저로 차단 | envtest; PillarProtocol(`proto-1`) + PillarBinding(`protocolRef="proto-1"`) 생성 완료; PillarProtocol에 `pillar-csi.bhyoo.com/protocol-protection` 파이널라이저 존재 | 1) `k8sClient.Delete(ctx, protocol)` 호출; 2) `protocolReconciler.Reconcile(ctx, req)` 호출; 3) PillarProtocol 조회 | PillarProtocol에 `DeletionTimestamp` 설정됨; 파이널라이저 유지; PillarProtocol 여전히 존재 | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E26.3.6 | `TestCrossLifecycle_DeleteProtection_Protocol_AllowedAfterBindingRemoved` | 참조 PillarBinding 삭제 후 PillarProtocol 삭제 완료 | envtest; E26.3.5 상태에서 시작; PillarBinding 삭제 완료 | 1) PillarBinding 삭제 완료; 2) `protocolReconciler.Reconcile(ctx, req)` 재호출; 3) PillarProtocol 조회 | PillarProtocol 삭제 완료(NotFound); 파이널라이저 제거 확인 | `PProtCRD`, `PProtCtrl`, `BindCRD` |

**전체 체인 역순 삭제 (Binding → Pool → Target):**

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E26.3.7 | `TestCrossLifecycle_DeleteProtection_FullChain_ReverseOrderDeletion` | 의존 역순(Binding→Pool→Target) 삭제 시 전체 체인 정상 삭제 완료 | envtest; PillarTarget + PillarPool + PillarBinding 모두 생성 완료; 각 CRD에 파이널라이저 존재 | 1) PillarBinding 삭제 및 조정 완료; 2) PillarPool 조정 → 파이널라이저 해제; 3) PillarPool 삭제 완료; 4) PillarTarget 조정 → 파이널라이저 해제; 5) PillarTarget 삭제 완료 | 역순 삭제로 모든 파이널라이저 순차 해제; 최종적으로 세 CRD 모두 삭제(NotFound) | `TgtCRD`, `TgtCtrl`, `PoolCRD`, `PoolCtrl`, `BindCRD`, `BindCtrl` |
| E26.3.8 | `TestCrossLifecycle_DeleteProtection_NoDependent_ImmediateDeletion` | 참조 하위 CRD가 없는 CRD는 파이널라이저 즉시 제거 후 삭제 완료 | envtest; PillarTarget 단독 생성(참조 PillarPool 없음) | 1) `k8sClient.Delete(ctx, target)` 호출; 2) `targetReconciler.Reconcile(ctx, req)` 호출; 3) PillarTarget 조회 | PillarTarget 즉시 삭제(NotFound); 파이널라이저 제거 확인 | `TgtCRD`, `TgtCtrl` |

---

**총 E26 테스트 케이스: 23개** (E26.1: 8개, E26.2: 7개, E26.3: 8개)

**CI 실행 가능 여부 요약:**

| 섹션 | 테스트 수 | CI 가능 여부 | 비고 |
|------|----------|:----------:|------|
| E26.1 의존 순서 | 8 | ✅ | envtest 필요; ZFS/NVMe-oF 하드웨어 불필요 |
| E26.2 연쇄 상태 업데이트 | 7 | ✅ | envtest 필요; 수동 Reconcile 순차 호출 |
| E26.3 삭제 보호 | 8 | ✅ (구현 필요) | envtest 필요; 파이널라이저 로직 미구현 시 실패 |
| **합계** | **23** | **✅** | `go test -tags=integration ./internal/controller/...` |

**검증 불가 항목 (이 섹션에서):**

| 검증 불가 항목 | 이유 | 대안 (향후) |
|--------------|------|------------|
| Watch 이벤트 자동 전파 지연 측정 | envtest에서는 명시적 Reconcile 호출 필요 | Kind 클러스터 E2E (유형 B) |
| 실제 StorageClass → PVC → PV 프로비저닝 흐름 | external-provisioner 미사용; envtest 범위 밖 | Kind 클러스터 E2E (유형 B) |
| 동시 다수 바인딩 bindingCount 경쟁 조건 | envtest 단일 스레드 순차 실행 | F25 확장 (`TestScalability_MultipleBindings`) |
| 노드 장애 시 PillarTarget 자동 Not-Ready 전환 | 실제 노드 제거 필요; Kubernetes 노드 컨트롤러 동작 | 수동 스테이징 (M2 확장) |
| cert-manager 인증서 만료 → PillarTarget AgentConnected 조건 갱신 | 실제 TLS 핸드셰이크 + 인증서 TTL 필요 | M10 확장 |

---

# 카테고리 2 — 클러스터 레벨 E2E 테스트 (유형 B: Kind 클러스터 필요) ⚠️

> **빌드 태그:** `//go:build e2e` | **실행:** `go test ./test/e2e/ -tags=e2e -v`
>
> Kind 클러스터 필요 · pillar-csi 컨테이너 이미지 필요 · Docker 필요 · 총 **3개** 테스트

이 카테고리의 테스트는 **실제 Kubernetes API 서버, CRD 검증 웹훅, RBAC** 동작을 검증한다.
실제 ZFS/NVMe-oF 스토리지 백엔드는 여전히 mock을 사용하므로 스토리지 하드웨어는 불필요하다.
표준 GitHub Actions에서 `helm/kind-action@v1`을 사용하면 실행 가능하다.

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

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 68 | `TestE2E/Manager_컨트롤러_파드_실행_확인` | pillar-csi-controller-manager 파드가 `pillar-csi-system` 네임스페이스에서 정상 실행됨 | Kind 클러스터; `make docker-build` 후 이미지 로드; CRD 설치; 매니저 배포 완료 | 1) pillar-csi-system 네임스페이스에서 파드 목록 조회; 2) 파드 상태 확인 | 컨트롤러 파드가 Running 상태; 재시작 없음 | `전체시스템`, `Kubernetes클러스터` |
| 69 | `TestE2E/매니저_메트릭스_서비스_접근_가능` | RBAC RoleBinding 생성 후 `/metrics` 엔드포인트에서 메트릭 수집 가능 | Kind 클러스터; 컨트롤러 파드 Running; 메트릭 RoleBinding 생성 | 1) kubectl port-forward 또는 직접 curl로 /metrics 접근 | HTTP 200 응답; Go 런타임 메트릭 포함 | `전체시스템`, `Kubernetes클러스터` |
| 70 | `TestE2E/cert-manager_통합` | cert-manager가 설치된 환경에서 TLS 인증서 발급 동작 | Kind 클러스터; cert-manager v1.14+ 설치 완료; 클러스터 배포 | 1) cert-manager Certificate 리소스 상태 확인 | 인증서 발급 성공; Secret에 tls.crt/tls.key 존재 | `전체시스템`, `cert-manager`, `TgtCRD` |

---

## E26: Helm 차트 설치 및 릴리스 검증

**테스트 유형:** B (클러스터 레벨) ❌ 표준 CI 불가

**빌드 태그:** `//go:build e2e`

**현재 구현 상태:** 미구현(planned). 이 문서는 구현 전 설계 사양이다.

**실행 방법:**
```bash
# Kind 클러스터 준비 및 Helm v3.12+ 설치 후
go test ./test/e2e/ -tags=e2e -v -run TestHelm
```

**필수 인프라:**
- [유형 B 섹션 참조](#유형-b-클러스터-레벨cluster-level-e2e-테스트--표준-ci-불가)
- `helm` CLI v3.12 이상
- Kind 클러스터 또는 kubeconfig 설정된 K8s 클러스터
- pillar-csi 컨테이너 이미지 (Kind 로컬 로드 또는 레지스트리 접근 가능)

**검증 대상 리소스:** Helm 차트(`charts/pillar-csi`)가 기본값으로 설치될 때 아래 리소스가 생성된다.

| 리소스 종류 | 이름 패턴 | 설명 |
|-----------|---------|------|
| `Deployment` | `<release>-controller` | CSI 컨트롤러 + 사이드카(provisioner, attacher, resizer, livenessprobe) |
| `DaemonSet` | `<release>-node` | CSI 노드 서비스 (모든 워커 노드에 배포) |
| `DaemonSet` | `<release>-agent` | pillar-agent (스토리지 레이블 노드에만 배포) |
| `ServiceAccount` | `<release>-controller`, `<release>-node`, `<release>-agent` | 각 컴포넌트별 서비스 계정 3개 |
| `ClusterRole` | `<release>` | provisioner/attacher/resizer/controller 권한 통합 ClusterRole |
| `ClusterRoleBinding` | `<release>` | ClusterRole 바인딩 |
| `CSIDriver` | `pillar-csi.bhyoo.com` | CSI 드라이버 등록 객체 |
| `CustomResourceDefinition` | `pillarbindings.pillar-csi.bhyoo.com` 외 4종 | pillar-csi CRD 5종 |

---

### E26.1 Helm 차트 기본값 설치 성공

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 207 | `TestHelm/Helm_차트_기본값_설치_성공` | `helm install`을 기본값으로 실행하면 릴리스가 `deployed` 상태로 완료된다 | Kind 클러스터 실행 중; `helm` v3.12+ 설치됨; `charts/pillar-csi/` 디렉터리 존재; pillar-csi 이미지 접근 가능(Kind 로드 또는 레지스트리) | 1) `helm install pillar-csi ./charts/pillar-csi --namespace pillar-csi-system --create-namespace --wait --timeout 5m` 실행; 2) 명령 종료 코드 확인 | 종료 코드 0; stdout에 `STATUS: deployed` 포함; stdout에 `REVISION: 1` 포함; `pillar-csi-system` 네임스페이스 생성됨 | `전체시스템`, `Kubernetes클러스터` |

**설치 명령 전체 예시:**
```bash
helm install pillar-csi ./charts/pillar-csi \
  --namespace pillar-csi-system \
  --create-namespace \
  --wait \
  --timeout 5m
```

**기대 stdout 출력 (예시):**
```
NAME: pillar-csi
LAST DEPLOYED: Wed Mar 25 12:00:00 2026
NAMESPACE: pillar-csi-system
STATUS: deployed
REVISION: 1
TEST SUITE: None
```

---

### E26.2 Helm 릴리스 상태 검증 (helm status)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 208 | `TestHelm/Helm_릴리스_상태_검증` | `helm status` 명령이 릴리스 메타데이터를 올바르게 반환한다 | E26.1 완료 후 상태 (pillar-csi 릴리스 `deployed` 상태) | 1) `helm status pillar-csi --namespace pillar-csi-system` 실행; 2) 출력 파싱 | `NAME: pillar-csi`; `NAMESPACE: pillar-csi-system`; `STATUS: deployed`; `REVISION: 1`; 종료 코드 0 | `전체시스템`, `Kubernetes클러스터` |
| 209 | `TestHelm/Helm_릴리스_상태_JSON_검증` | `helm status --output json` 출력이 파싱 가능한 JSON이고 필수 필드를 포함한다 | E26.1 완료 후 상태 | 1) `helm status pillar-csi --namespace pillar-csi-system --output json` 실행; 2) JSON 파싱; 3) 필드 검증 | JSON 파싱 성공; `.info.status == "deployed"`; `.name == "pillar-csi"`; `.namespace == "pillar-csi-system"`; `.version == 1` | `전체시스템`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# 텍스트 출력 확인
helm status pillar-csi --namespace pillar-csi-system

# JSON 출력으로 파싱 가능성 확인
helm status pillar-csi --namespace pillar-csi-system --output json \
  | jq '.info.status'
# 기대 출력: "deployed"

helm status pillar-csi --namespace pillar-csi-system --output json \
  | jq '.version'
# 기대 출력: 1
```

---

### E26.3 Helm 릴리스 목록 검증 (helm list)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 210 | `TestHelm/Helm_릴리스_목록_검증` | `helm list`가 설치된 릴리스 항목을 반환한다 | E26.1 완료 후 상태 | 1) `helm list --namespace pillar-csi-system` 실행; 2) 출력에서 `pillar-csi` 항목 존재 여부 확인 | 출력에 `pillar-csi` 행 존재; `CHART` 열에 `pillar-csi-0.1.0`; `STATUS` 열에 `deployed` | `전체시스템`, `Kubernetes클러스터` |
| 211 | `TestHelm/Helm_릴리스_목록_JSON_검증` | `helm list --output json`이 파싱 가능한 배열을 반환하고 릴리스가 포함된다 | E26.1 완료 후 상태 | 1) `helm list --namespace pillar-csi-system --output json` 실행; 2) JSON 배열 파싱; 3) `pillar-csi` 항목 검색 | 배열 길이 ≥ 1; 첫 번째 항목 `.name == "pillar-csi"`; `.status == "deployed"`; `.chart` 에 `pillar-csi` 포함 | `전체시스템`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
helm list --namespace pillar-csi-system

# 기대 출력 (헤더 + 데이터 행):
# NAME        NAMESPACE           REVISION  UPDATED                   STATUS    CHART              APP VERSION
# pillar-csi  pillar-csi-system  1         2026-03-25 12:00:00 ...   deployed  pillar-csi-0.1.0   0.1.0

helm list --namespace pillar-csi-system --output json \
  | jq '.[0].status'
# 기대 출력: "deployed"
```

---

### E26.4 배포된 Kubernetes 리소스 정상 동작 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 212 | `TestHelm/컨트롤러_Deployment_Running_검증` | Helm 설치 후 controller Deployment가 Available 상태이고 파드가 Running이다 | E26.1 완료; `--wait` 플래그로 설치하여 파드 Ready 대기 완료 | 1) `kubectl get deployment -n pillar-csi-system -l app.kubernetes.io/component=controller -o json`; 2) `.status.availableReplicas` 확인; 3) 파드 상태 확인 | Deployment `availableReplicas == 1`; 컨트롤러 파드 `status.phase == Running`; 컨테이너 재시작 횟수 == 0 | `CSI-C`, `Kubernetes클러스터` |
| 213 | `TestHelm/노드_DaemonSet_배포_검증` | Helm 설치 후 node DaemonSet이 존재하고 스케줄 가능한 노드 수만큼 파드가 Running이다 | E26.1 완료; Kind 클러스터 워커 노드 수 확인 | 1) `kubectl get daemonset -n pillar-csi-system -l app.kubernetes.io/component=node -o json`; 2) `.status.numberReady` 확인 | `numberReady == numberDesired`; 각 파드 `status.phase == Running`; node-driver-registrar 및 livenessprobe 사이드카 포함 | `CSI-N`, `Kubernetes클러스터` |
| 214 | `TestHelm/에이전트_DaemonSet_배포_검증` | Helm 설치 후 agent DaemonSet이 존재하고 스토리지 레이블 노드에만 파드가 스케줄된다 | E26.1 완료; 스토리지 레이블 노드 없는 상태(기본값) | 1) `kubectl get daemonset -n pillar-csi-system -l app.kubernetes.io/component=agent -o json`; 2) `.status.desiredNumberScheduled` 확인 | DaemonSet 존재; 스토리지 레이블(`pillar-csi.bhyoo.com/storage-node=true`) 없는 환경에서 `desiredNumberScheduled == 0`(파드 없음이 정상); DaemonSet 자체는 `Running` 상태 아님 — `desiredNumberScheduled == 0`이어야 함 | `Agent`, `Kubernetes클러스터` |
| 215 | `TestHelm/ServiceAccount_3종_존재_검증` | 컨트롤러·노드·에이전트 ServiceAccount 3개가 생성된다 | E26.1 완료 | 1) `kubectl get serviceaccount -n pillar-csi-system -o name`; 2) 이름 목록에서 3개 ServiceAccount 존재 여부 확인 | `pillar-csi-controller`(또는 fullname 패턴) SA 존재; `pillar-csi-node` SA 존재; `pillar-csi-agent` SA 존재 | `전체시스템`, `Kubernetes클러스터` |
| 216 | `TestHelm/CSIDriver_등록_검증` | CSIDriver 객체 `pillar-csi.bhyoo.com`이 올바른 스펙으로 등록된다 | E26.1 완료 | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o json`; 2) `.spec` 필드 검증 | CSIDriver 존재; `.spec.attachRequired == true`; `.spec.podInfoOnMount == true`; `.spec.fsGroupPolicy == "File"`; `.spec.volumeLifecycleModes` 에 `"Persistent"` 포함 | `전체시스템`, `Kubernetes클러스터` |

---

### E26.5 CRD 등록 및 가용성 검증

> **범위:** Helm 설치 후(`installCRDs: true` 기본값) API 서버에 등록된 pillar-csi CRD의 **존재·상태·메타데이터·API 가용성**을 포괄 검증한다.
>
> **실제 배포 CRD 목록 (4종):** Helm 차트(`charts/pillar-csi/templates/crds.yaml`)는 아래 4개 CRD를 배포한다. CRD 이름의 그룹 부분은 `pillar-csi.pillar-csi.bhyoo.com`이다.
>
> | 순번 | CRD 이름 | kind | 짧은 이름 | 범위 |
> |------|---------|------|---------|------|
> | 1 | `pillartargets.pillar-csi.pillar-csi.bhyoo.com` | `PillarTarget` | `pt` | Cluster |
> | 2 | `pillarpools.pillar-csi.pillar-csi.bhyoo.com` | `PillarPool` | `pp` | Cluster |
> | 3 | `pillarprotocols.pillar-csi.pillar-csi.bhyoo.com` | `PillarProtocol` | `ppr` | Cluster |
> | 4 | `pillarbindings.pillar-csi.pillar-csi.bhyoo.com` | `PillarBinding` | `pb` | Cluster |
>
> **주의:** `PillarVolume`은 Go 타입 및 내부 스키마 등록(`SchemeBuilder.Register`)에는 포함되지만, 현재 Helm 차트에는 CRD YAML로 배포되지 않는다(`config/crd/bases/` 및 `charts/pillar-csi/templates/crds.yaml` 에 미포함). 따라서 클러스터에서 `kubectl get crd pillarvolumes.*`를 실행하면 NotFound 응답을 받는 것이 정상이다.

#### E26.5.1 CRD 4종 일괄 존재 및 Established 상태 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 217 | `TestHelm/CRD_4종_설치_검증` | Helm 설치 후 pillar-csi CRD 4종이 모두 등록되고 Established=True 이다 | E26.1 완료 (`installCRDs: true` 기본값); `kubectl` 설정된 Kind 클러스터 컨텍스트 | 1) `kubectl get crd -o json \| jq '[.items[].metadata.name] \| map(select(contains("pillar-csi.pillar-csi.bhyoo.com")))'`; 2) 반환 배열 길이 == 4 확인; 3) 각 CRD에 대해 `.status.conditions[?(@.type=="Established")].status == "True"` 검증 | 배열에 `pillartargets.pillar-csi.pillar-csi.bhyoo.com`, `pillarpools.pillar-csi.pillar-csi.bhyoo.com`, `pillarprotocols.pillar-csi.pillar-csi.bhyoo.com`, `pillarbindings.pillar-csi.pillar-csi.bhyoo.com` 4개 항목 모두 포함; 각 CRD `Established=True`; `NamesAccepted=True` | `VolCRD`, `TgtCRD`, `Kubernetes클러스터` |
| 217a | `TestHelm/CRD_Established_PillarTarget` | PillarTarget CRD가 `Established=True`이고 `NamesAccepted=True`이다 | E26.1 완료 | 1) `kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.status.conditions` 배열 검증 | `conditions` 중 `type=="Established"` 항목의 `status=="True"`; `type=="NamesAccepted"` 항목의 `status=="True"`; `.status.acceptedNames.kind == "PillarTarget"` | `TgtCRD`, `Kubernetes클러스터` |
| 217b | `TestHelm/CRD_Established_PillarPool` | PillarPool CRD가 `Established=True`이고 `NamesAccepted=True`이다 | E26.1 완료 | 1) `kubectl get crd pillarpools.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.status.conditions` 배열 검증 | `Established=True`; `NamesAccepted=True`; `.status.acceptedNames.kind == "PillarPool"` | `VolCRD`, `Kubernetes클러스터` |
| 217c | `TestHelm/CRD_Established_PillarProtocol` | PillarProtocol CRD가 `Established=True`이고 `NamesAccepted=True`이다 | E26.1 완료 | 1) `kubectl get crd pillarprotocols.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.status.conditions` 배열 검증 | `Established=True`; `NamesAccepted=True`; `.status.acceptedNames.kind == "PillarProtocol"` | `VolCRD`, `Kubernetes클러스터` |
| 217d | `TestHelm/CRD_Established_PillarBinding` | PillarBinding CRD가 `Established=True`이고 `NamesAccepted=True`이다 | E26.1 완료 | 1) `kubectl get crd pillarbindings.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.status.conditions` 배열 검증 | `Established=True`; `NamesAccepted=True`; `.status.acceptedNames.kind == "PillarBinding"` | `VolCRD`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# CRD 4종 존재 확인 (올바른 그룹 이름: pillar-csi.pillar-csi.bhyoo.com)
kubectl get crd | grep 'pillar-csi\.pillar-csi\.bhyoo\.com'
# 기대 출력:
# pillarbindings.pillar-csi.pillar-csi.bhyoo.com    2026-03-25T12:00:00Z
# pillarpools.pillar-csi.pillar-csi.bhyoo.com       2026-03-25T12:00:00Z
# pillarprotocols.pillar-csi.pillar-csi.bhyoo.com   2026-03-25T12:00:00Z
# pillartargets.pillar-csi.pillar-csi.bhyoo.com     2026-03-25T12:00:00Z

# PillarVolume CRD는 존재하지 않음을 확인 (정상)
kubectl get crd pillarvolumes.pillar-csi.pillar-csi.bhyoo.com 2>&1 | grep -i "not found"
# 기대 출력: Error from server (NotFound): ...

# 각 CRD Established 상태 확인 (4종 일괄)
for crd in pillartargets pillarpools pillarprotocols pillarbindings; do
  STATUS=$(kubectl get crd ${crd}.pillar-csi.pillar-csi.bhyoo.com \
    -o jsonpath='{.status.conditions[?(@.type=="Established")].status}')
  echo "${crd}: Established=${STATUS}"
done
# 기대 출력:
# pillartargets: Established=True
# pillarpools: Established=True
# pillarprotocols: Established=True
# pillarbindings: Established=True
```

---

#### E26.5.2 각 CRD 메타데이터 상세 검증 — 그룹·버전·범위·shortName

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 217e | `TestHelm/CRD_Metadata_PillarTarget` | PillarTarget CRD 스펙의 그룹·버전·범위·shortName이 올바르다 | E26.1 완료 | 1) `kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec` 필드 전체 검증 | `.spec.group == "pillar-csi.pillar-csi.bhyoo.com"`; `.spec.names.kind == "PillarTarget"`; `.spec.names.plural == "pillartargets"`; `.spec.names.singular == "pillartarget"`; `.spec.names.shortNames` 에 `"pt"` 포함; `.spec.scope == "Cluster"`; `.spec.versions[0].name == "v1alpha1"` | `TgtCRD`, `Kubernetes클러스터` |
| 217f | `TestHelm/CRD_Metadata_PillarPool` | PillarPool CRD 스펙의 그룹·버전·범위·shortName이 올바르다 | E26.1 완료 | 1) `kubectl get crd pillarpools.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec` 필드 전체 검증 | `.spec.group == "pillar-csi.pillar-csi.bhyoo.com"`; `.spec.names.kind == "PillarPool"`; `.spec.names.plural == "pillarpools"`; `.spec.names.singular == "pillarpool"`; `.spec.names.shortNames` 에 `"pp"` 포함; `.spec.scope == "Cluster"`; `.spec.versions[0].name == "v1alpha1"` | `VolCRD`, `Kubernetes클러스터` |
| 217g | `TestHelm/CRD_Metadata_PillarProtocol` | PillarProtocol CRD 스펙의 그룹·버전·범위·shortName이 올바르다 | E26.1 완료 | 1) `kubectl get crd pillarprotocols.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec` 필드 전체 검증 | `.spec.group == "pillar-csi.pillar-csi.bhyoo.com"`; `.spec.names.kind == "PillarProtocol"`; `.spec.names.plural == "pillarprotocols"`; `.spec.names.singular == "pillarprotocol"`; `.spec.names.shortNames` 에 `"ppr"` 포함; `.spec.scope == "Cluster"`; `.spec.versions[0].name == "v1alpha1"` | `VolCRD`, `Kubernetes클러스터` |
| 217h | `TestHelm/CRD_Metadata_PillarBinding` | PillarBinding CRD 스펙의 그룹·버전·범위·shortName이 올바르다 | E26.1 완료 | 1) `kubectl get crd pillarbindings.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec` 필드 전체 검증 | `.spec.group == "pillar-csi.pillar-csi.bhyoo.com"`; `.spec.names.kind == "PillarBinding"`; `.spec.names.plural == "pillarbindings"`; `.spec.names.singular == "pillarbinding"`; `.spec.names.shortNames` 에 `"pb"` 포함; `.spec.scope == "Cluster"`; `.spec.versions[0].name == "v1alpha1"` | `VolCRD`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# PillarTarget 메타데이터 일괄 검증
kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com -o jsonpath='{
  "group": "{.spec.group}",
  "kind": "{.spec.names.kind}",
  "plural": "{.spec.names.plural}",
  "shortNames": "{.spec.names.shortNames}",
  "scope": "{.spec.scope}",
  "version": "{.spec.versions[0].name}"
}'
# 기대 출력 요약:
# group: pillar-csi.pillar-csi.bhyoo.com
# kind: PillarTarget
# plural: pillartargets
# shortNames: [pt]
# scope: Cluster
# version: v1alpha1
```

---

#### E26.5.3 kubectl api-resources를 통한 API 가용성 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 217i | `TestHelm/API_Resources_그룹_등록_검증` | `kubectl api-resources` 출력에서 pillar-csi 그룹의 4종 리소스가 모두 노출된다 | E26.1 완료 | 1) `kubectl api-resources --api-group=pillar-csi.pillar-csi.bhyoo.com -o wide`; 2) 출력 파싱하여 리소스 이름·kind·shortNames 검증 | 4개 리소스 행 반환: `pillartargets`(pt), `pillarpools`(pp), `pillarprotocols`(ppr), `pillarbindings`(pb); 모든 행의 `APIVERSION` 열이 `pillar-csi.pillar-csi.bhyoo.com/v1alpha1`; `NAMESPACED` 열이 `false` (Cluster 범위) | `TgtCRD`, `VolCRD`, `Kubernetes클러스터` |
| 217j | `TestHelm/API_Resources_shortName_pt_검증` | `kubectl get pt`가 API 서버에서 PillarTarget 리소스를 조회한다 | E26.1 완료; 클러스터에 PillarTarget 오브젝트 없어도 무방 | 1) `kubectl get pt`; 2) 종료 코드 및 출력 확인 | 종료 코드 0; 오류 없이 빈 목록(`No resources found.`) 또는 헤더만 반환; `NotFound` 또는 `Unknown resource type` 오류 없음 | `TgtCRD`, `Kubernetes클러스터` |
| 217k | `TestHelm/API_Resources_shortName_pp_검증` | `kubectl get pp`가 API 서버에서 PillarPool 리소스를 조회한다 | E26.1 완료 | 1) `kubectl get pp`; 2) 종료 코드 확인 | 종료 코드 0; 오류 없이 빈 목록 반환 | `VolCRD`, `Kubernetes클러스터` |
| 217l | `TestHelm/API_Resources_shortName_ppr_검증` | `kubectl get ppr`가 API 서버에서 PillarProtocol 리소스를 조회한다 | E26.1 완료 | 1) `kubectl get ppr`; 2) 종료 코드 확인 | 종료 코드 0; 오류 없이 빈 목록 반환 | `VolCRD`, `Kubernetes클러스터` |
| 217m | `TestHelm/API_Resources_shortName_pb_검증` | `kubectl get pb`가 API 서버에서 PillarBinding 리소스를 조회한다 | E26.1 완료 | 1) `kubectl get pb`; 2) 종료 코드 확인 | 종료 코드 0; 오류 없이 빈 목록 반환 | `VolCRD`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# pillar-csi 그룹 API 리소스 전체 목록
kubectl api-resources --api-group=pillar-csi.pillar-csi.bhyoo.com -o wide
# 기대 출력 (열 순서는 k8s 버전에 따라 다를 수 있음):
# NAME              SHORTNAMES   APIVERSION                                    NAMESPACED   KIND
# pillarbindings    pb           pillar-csi.pillar-csi.bhyoo.com/v1alpha1      false        PillarBinding
# pillarpools       pp           pillar-csi.pillar-csi.bhyoo.com/v1alpha1      false        PillarPool
# pillarprotocols   ppr          pillar-csi.pillar-csi.bhyoo.com/v1alpha1      false        PillarProtocol
# pillartargets     pt           pillar-csi.pillar-csi.bhyoo.com/v1alpha1      false        PillarTarget

# shortName으로 조회 (각각 오류 없이 빈 목록 반환)
kubectl get pt && kubectl get pp && kubectl get ppr && kubectl get pb
# 기대 출력 (각각): "No resources found."
```

---

#### E26.5.4 각 CRD OpenAPI v3 스키마 존재 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 217n | `TestHelm/CRD_Schema_PillarTarget` | PillarTarget CRD `v1alpha1` 버전에 OpenAPI v3 스키마가 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].schema.openAPIV3Schema` 필드 존재 여부 확인; 3) 핵심 spec 필드(`spec.external`, `spec.nodeRef`) 존재 확인 | `.spec.versions[0].schema.openAPIV3Schema` 필드가 null이 아님; `.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties` 에 `external` 및 `nodeRef` 키 존재; `.spec.versions[0].served == true`; `.spec.versions[0].storage == true` | `TgtCRD`, `Kubernetes클러스터` |
| 217o | `TestHelm/CRD_Schema_PillarPool` | PillarPool CRD `v1alpha1` 버전에 OpenAPI v3 스키마가 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillarpools.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties` 에서 `backend` 및 `targetRef` 키 확인 | `openAPIV3Schema` 존재; `spec.properties.backend` 및 `spec.properties.targetRef` 키 존재; `.spec.versions[0].served == true`; `.spec.versions[0].storage == true` | `VolCRD`, `Kubernetes클러스터` |
| 217p | `TestHelm/CRD_Schema_PillarProtocol` | PillarProtocol CRD `v1alpha1` 버전에 OpenAPI v3 스키마가 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillarprotocols.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties` 에서 `type` 키 확인 | `openAPIV3Schema` 존재; `spec.properties.type` 존재 (enum: NVMeoF-TCP 등); `.spec.versions[0].served == true`; `.spec.versions[0].storage == true` | `VolCRD`, `Kubernetes클러스터` |
| 217q | `TestHelm/CRD_Schema_PillarBinding` | PillarBinding CRD `v1alpha1` 버전에 OpenAPI v3 스키마가 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillarbindings.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties` 에서 `poolRef` 및 `protocolRef` 키 확인 | `openAPIV3Schema` 존재; `spec.properties.poolRef` 및 `spec.properties.protocolRef` 존재; `.spec.versions[0].served == true`; `.spec.versions[0].storage == true` | `VolCRD`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# PillarTarget 스키마 존재 확인
kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.type}'
# 기대 출력: object

# PillarTarget spec 내 핵심 필드 확인
kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties}' \
  | jq 'keys'
# 기대 출력: ["external", "nodeRef"] (또는 추가 필드 포함)

# served/storage 플래그 확인
kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com \
  -o jsonpath='{range .spec.versions[*]}{.name}:{.served}/{.storage} {end}'
# 기대 출력: v1alpha1:true/true
```

---

#### E26.5.5 각 CRD 프린터 컬럼(additionalPrinterColumns) 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 217r | `TestHelm/CRD_PrinterColumns_PillarTarget` | PillarTarget CRD에 `Address`, `Agent`, `Ready`, `Age` 프린터 컬럼이 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillartargets.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].additionalPrinterColumns` 배열 검증 | 배열에 `name: "Address"`, `name: "Agent"`, `name: "Ready"`, `name: "Age"` 항목 포함; `Address` jsonPath: `.status.resolvedAddress`; `Ready` jsonPath: `.status.conditions[?(@.type=="Ready")].status` | `TgtCRD`, `Kubernetes클러스터` |
| 217s | `TestHelm/CRD_PrinterColumns_PillarPool` | PillarPool CRD에 `Target`, `Backend`, `Available`, `Ready`, `Age` 프린터 컬럼이 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillarpools.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].additionalPrinterColumns` 배열 검증 | `name: "Target"` (jsonPath: `.spec.targetRef`); `name: "Backend"` (jsonPath: `.spec.backend.type`); `name: "Available"` (jsonPath: `.status.capacity.available`); `name: "Ready"` 항목 포함 | `VolCRD`, `Kubernetes클러스터` |
| 217t | `TestHelm/CRD_PrinterColumns_PillarProtocol` | PillarProtocol CRD에 `Type`, `Bindings`, `Ready`, `Age` 프린터 컬럼이 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillarprotocols.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].additionalPrinterColumns` 배열 검증 | `name: "Type"` (jsonPath: `.spec.type`); `name: "Bindings"` (jsonPath: `.status.bindingCount`); `name: "Ready"` 항목 포함 | `VolCRD`, `Kubernetes클러스터` |
| 217u | `TestHelm/CRD_PrinterColumns_PillarBinding` | PillarBinding CRD에 `Pool`, `Protocol`, `StorageClass`, `Ready`, `Age` 프린터 컬럼이 정의되어 있다 | E26.1 완료 | 1) `kubectl get crd pillarbindings.pillar-csi.pillar-csi.bhyoo.com -o json`; 2) `.spec.versions[0].additionalPrinterColumns` 배열 검증 | `name: "Pool"` (jsonPath: `.spec.poolRef`); `name: "Protocol"` (jsonPath: `.spec.protocolRef`); `name: "StorageClass"` (jsonPath: `.status.storageClassName`); `name: "Ready"` 항목 포함 | `VolCRD`, `Kubernetes클러스터` |

---

#### E26.5.6 Helm resource-policy: keep 어노테이션 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 217v | `TestHelm/CRD_ResourcePolicy_Keep_검증` | 모든 CRD에 `helm.sh/resource-policy: keep` 어노테이션이 설정되어 있다 | E26.1 완료 | 1) 4개 CRD에 대해 각각 `kubectl get crd <name> -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}'`; 2) 각 출력값 확인 | 4개 CRD 모두 `keep` 반환; 이 어노테이션은 `helm uninstall` 시에도 CRD가 삭제되지 않도록 보호하는 역할 | `VolCRD`, `TgtCRD`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# 4종 CRD resource-policy 일괄 확인
for crd in pillartargets pillarpools pillarprotocols pillarbindings; do
  POLICY=$(kubectl get crd ${crd}.pillar-csi.pillar-csi.bhyoo.com \
    -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}')
  echo "${crd}: resource-policy=${POLICY}"
done
# 기대 출력:
# pillartargets: resource-policy=keep
# pillarpools: resource-policy=keep
# pillarprotocols: resource-policy=keep
# pillarbindings: resource-policy=keep
```

---

#### E26.5.7 CRD CRUD 기본 동작 — 샘플 오브젝트 생성/조회/삭제

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 217w | `TestHelm/CRD_CRUD_PillarTarget_생성조회삭제` | Helm 설치 후 PillarTarget 오브젝트를 생성·조회·삭제할 수 있다 | E26.1 완료; `config/samples/pillar-csi_v1alpha1_pillartarget.yaml` 샘플 파일 존재 | 1) `kubectl apply -f config/samples/pillar-csi_v1alpha1_pillartarget.yaml`; 2) `kubectl get pt` 로 조회; 3) `kubectl delete -f config/samples/pillar-csi_v1alpha1_pillartarget.yaml` | apply 종료 코드 0; `kubectl get pt`에 생성된 오브젝트 1개 이상 표시; delete 종료 코드 0; 삭제 후 `kubectl get pt`에서 해당 이름 미존재 | `TgtCRD`, `Kubernetes클러스터` |
| 217x | `TestHelm/CRD_CRUD_PillarPool_생성조회삭제` | Helm 설치 후 PillarPool 오브젝트를 생성·조회·삭제할 수 있다 | E26.1 완료; PillarTarget 오브젝트 사전 존재 (poolRef 의존); `config/samples/pillar-csi_v1alpha1_pillarpool.yaml` 존재 | 1) `kubectl apply -f config/samples/pillar-csi_v1alpha1_pillarpool.yaml`; 2) `kubectl get pp`; 3) `kubectl delete -f config/samples/pillar-csi_v1alpha1_pillarpool.yaml` | apply 종료 코드 0; 조회 성공; 삭제 성공 | `VolCRD`, `Kubernetes클러스터` |
| 217y | `TestHelm/CRD_CRUD_PillarProtocol_생성조회삭제` | Helm 설치 후 PillarProtocol 오브젝트를 생성·조회·삭제할 수 있다 | E26.1 완료; `config/samples/pillar-csi_v1alpha1_pillarprotocol.yaml` 존재 | 1) `kubectl apply -f config/samples/pillar-csi_v1alpha1_pillarprotocol.yaml`; 2) `kubectl get ppr`; 3) `kubectl delete -f config/samples/pillar-csi_v1alpha1_pillarprotocol.yaml` | apply 종료 코드 0; 조회 성공; 삭제 성공 | `VolCRD`, `Kubernetes클러스터` |
| 217z | `TestHelm/CRD_CRUD_PillarBinding_생성조회삭제` | Helm 설치 후 PillarBinding 오브젝트를 생성·조회·삭제할 수 있다 | E26.1 완료; PillarPool 및 PillarProtocol 오브젝트 사전 존재; `config/samples/pillar-csi_v1alpha1_pillarbinding.yaml` 존재 | 1) `kubectl apply -f config/samples/pillar-csi_v1alpha1_pillarbinding.yaml`; 2) `kubectl get pb`; 3) `kubectl delete -f config/samples/pillar-csi_v1alpha1_pillarbinding.yaml` | apply 종료 코드 0; 조회 성공; 삭제 성공 | `VolCRD`, `Kubernetes클러스터` |

> **참고 (CI 실행 가능성):** E26.5.7 테스트는 Kind 클러스터에서 실행 가능하다. 그러나 컨트롤러가 Running 상태여야 웹훅 검증이 통과하므로, E26.1에서 `--wait` 플래그로 설치 완료가 확인된 이후에 실행해야 한다.

---

#### E26.5 종합 커버리지 요약

| 검증 항목 | 테스트 ID | CI 가능 | 비고 |
|---------|---------|:------:|------|
| 4종 CRD 일괄 존재 + Established/NamesAccepted 상태 | 217, 217a–d | ✅ | Kind 클러스터 |
| 각 CRD 그룹·버전·범위·shortName 메타데이터 | 217e–h | ✅ | Kind 클러스터 |
| kubectl api-resources 노출 + shortName 조회 | 217i–m | ✅ | Kind 클러스터 |
| OpenAPI v3 스키마 존재 및 핵심 필드 확인 | 217n–q | ✅ | Kind 클러스터 |
| additionalPrinterColumns 정의 확인 | 217r–u | ✅ | Kind 클러스터 |
| helm.sh/resource-policy: keep 어노테이션 | 217v | ✅ | Kind 클러스터 |
| 샘플 오브젝트 CRUD (생성·조회·삭제) | 217w–z | ✅ | Kind 클러스터; 컨트롤러 Running 필요 |
| PillarVolume CRD 미배포 확인 | 217 내 명시 | ✅ | NotFound 응답 기대 |

---

### E26.6 커스텀 values 오버라이드 설치

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 218 | `TestHelm/커스텀_values_오버라이드_설치` | `--set` 플래그로 값을 오버라이드하면 Deployment 스펙에 반영된다 | Kind 클러스터; E25.1 설치 해제 후 (또는 별도 릴리스 이름 사용); Helm v3.12+ | 1) `helm install pillar-csi-custom ./charts/pillar-csi --namespace pillar-csi-custom --create-namespace --set controller.replicaCount=2 --wait --timeout 5m`; 2) Deployment spec.replicas 확인 | 종료 코드 0; `STATUS: deployed`; controller Deployment `.spec.replicas == 2`; `availableReplicas == 2` | `CSI-C`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
helm install pillar-csi-custom ./charts/pillar-csi \
  --namespace pillar-csi-custom \
  --create-namespace \
  --set controller.replicaCount=2 \
  --wait \
  --timeout 5m

kubectl get deployment -n pillar-csi-custom \
  -l app.kubernetes.io/component=controller \
  -o jsonpath='{.items[0].spec.replicas}'
# 기대 출력: 2
```

---

### E26.7 installCRDs=false 설치 모드 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 219 | `TestHelm/installCRDs_false_설치_검증` | `installCRDs=false`로 설치하면 CRD가 생성되지 않고 다른 리소스는 정상 생성된다 | Kind 클러스터; CRD가 사전 설치되어 있지 않은 클린 상태; Helm v3.12+ | 1) `helm install pillar-csi-nocrd ./charts/pillar-csi --namespace pillar-csi-nocrd --create-namespace --set installCRDs=false --wait --timeout 5m`; 2) CRD 존재 여부 확인; 3) Deployment/DaemonSet 존재 여부 확인 | 종료 코드 0; `STATUS: deployed`; `kubectl get crd | grep pillar-csi.bhyoo.com` 출력이 비어 있음(CRD 없음); controller Deployment 존재 | `VolCRD`, `TgtCRD`, `Kubernetes클러스터` |

> **참고:** `installCRDs=false` 모드는 CRD를 별도의 GitOps 파이프라인 또는 전용 CRD 차트로 관리하는 경우에 사용한다. 이 모드에서 컨트롤러는 시작은 되지만, CRD가 등록되지 않아 CRD 기반 기능(PillarTarget 조회 등)은 동작하지 않는다.

---

### E26.8 중복 설치 시도 오류 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 220 | `TestHelm/중복_설치_시도_오류_검증` | 동일 릴리스 이름으로 `helm install`을 재시도하면 명확한 오류가 반환된다 | E26.1 완료 후 상태 (`pillar-csi` 릴리스가 이미 `deployed` 상태) | 1) 동일한 `helm install pillar-csi ./charts/pillar-csi --namespace pillar-csi-system` 재실행 | 종료 코드 비-0 (오류 반환); stderr에 `"pillar-csi" already exists` 포함; 기존 릴리스 상태 `deployed` 유지 (오염 없음) | `전체시스템`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# 첫 번째 설치 (E26.1에서 완료됨)
helm install pillar-csi ./charts/pillar-csi --namespace pillar-csi-system

# 두 번째 설치 시도 (오류 기대)
helm install pillar-csi ./charts/pillar-csi --namespace pillar-csi-system
# 기대 stderr: Error: INSTALLATION FAILED: "pillar-csi" already exists
# 기대 종료 코드: 1

# 기존 릴리스 상태 유지 확인
helm status pillar-csi --namespace pillar-csi-system
# STATUS: deployed (변경 없음)
```

---

### E26.9 Helm 차트 업그레이드 (helm upgrade)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 221 | `TestHelm/Helm_차트_업그레이드_성공` | `helm upgrade`로 릴리스를 업그레이드하면 REVISION이 증가하고 STATUS가 `deployed`를 유지한다 | E26.1 완료 후 상태 (REVISION: 1) | 1) `helm upgrade pillar-csi ./charts/pillar-csi --namespace pillar-csi-system --wait --timeout 5m`; 2) `helm status` 실행 | 종료 코드 0; `STATUS: deployed`; `REVISION: 2`; 이전 릴리스 히스토리에 REVISION: 1 보존됨 | `전체시스템`, `Kubernetes클러스터` |
| 222 | `TestHelm/Helm_업그레이드_히스토리_검증` | `helm history`가 이전 릴리스 이력을 반환한다 | E26.9.E221 완료 (REVISION: 2 상태) | 1) `helm history pillar-csi --namespace pillar-csi-system`; 2) 이력 항목 수 확인 | 이력 항목 2개; REVISION 1: `superseded`; REVISION 2: `deployed` | `전체시스템`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
helm upgrade pillar-csi ./charts/pillar-csi \
  --namespace pillar-csi-system \
  --wait \
  --timeout 5m
# 기대 출력:
# Release "pillar-csi" has been upgraded. Happy Helming!
# NAME: pillar-csi
# STATUS: deployed
# REVISION: 2

helm history pillar-csi --namespace pillar-csi-system
# REVISION  UPDATED                   STATUS      CHART              ...
# 1         2026-03-25 12:00:00 ...   superseded  pillar-csi-0.1.0
# 2         2026-03-25 12:05:00 ...   deployed    pillar-csi-0.1.0
```

---

### E26.10 Helm 차트 설치 해제 및 리소스 정리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 223 | `TestHelm/Helm_설치_해제_성공` | `helm uninstall`이 성공하고 Deployment·DaemonSet이 삭제된다 | E26.1 완료 후 상태 | 1) `helm uninstall pillar-csi --namespace pillar-csi-system --wait`; 2) 리소스 잔존 여부 확인 | 종료 코드 0; stdout에 `release "pillar-csi" uninstalled`; Deployment, DaemonSet, ServiceAccount, ClusterRole, ClusterRoleBinding 모두 삭제됨; CSIDriver 삭제됨 | `전체시스템`, `Kubernetes클러스터` |
| 224 | `TestHelm/설치_해제_후_CRD_보존_검증` | `helm uninstall` 후에도 CRD는 보존된다(`helm.sh/resource-policy: keep` 어노테이션) | E26.10.E223 완료 | 1) `kubectl get crd | grep pillar-csi.bhyoo.com`; 2) CRD 5종 존재 여부 확인 | CRD 5종 모두 존재 (삭제되지 않음); 이 동작은 `helm.sh/resource-policy: keep` 어노테이션으로 보장됨 | `VolCRD`, `TgtCRD`, `Kubernetes클러스터` |

> **참고:** CRD 보존 정책(`helm.sh/resource-policy: keep`)은 `charts/pillar-csi/templates/crds.yaml`에 어노테이션으로 지정되어 있다. 이 정책은 Helm 언인스톨 시 운영 데이터(CRD에 저장된 CR 인스턴스)를 실수로 삭제하는 것을 방지하기 위한 것이다.

**검증 명령 예시:**
```bash
helm uninstall pillar-csi --namespace pillar-csi-system --wait
# 기대 출력: release "pillar-csi" uninstalled

# Deployment 삭제 확인
kubectl get deployment -n pillar-csi-system
# 기대 출력: No resources found in pillar-csi-system namespace.

# CRD 보존 확인
kubectl get crd | grep pillar-csi.bhyoo.com
# 기대 출력: (CRD 5종 여전히 존재)
```

---

### E26.11 Helm 배포 후 전체 파드 Running 상태 종합 검증

> **목적:** Helm `--wait` 완료 후 pillar-csi의 **모든 기대 파드**가 `Running` 상태이고 컨테이너가 `Ready`임을 종합적으로 검증한다.
> E26.4의 개별 리소스 검증(Deployment availableReplicas, DaemonSet numberReady)과 달리, 이 섹션은 **파드 단위**로 내려가 각 파드의 `status.phase`, 컨테이너별 `ready` 및 재시작 횟수를 개별 검증한다.
>
> **CI 실행 가능 여부:** ✅ Kind 클러스터 환경에서 실행 가능 (빌드 태그: `e2e`)
>
> **기대 파드 인벤토리 (기본 values, 워커 노드 1개 가정):**
>
> | 컴포넌트 | 워크로드 유형 | 기대 파드 수 | 기대 컨테이너 (init 제외) | 비고 |
> |---------|------------|------------|------------------------|------|
> | `controller` | Deployment | 1 | 5개: `controller`, `csi-provisioner`, `csi-attacher`, `csi-resizer`, `liveness-probe` | `replicaCount: 1` 기본값 |
> | `node` | DaemonSet | 노드 수 × 1 | 3개: `node`, `node-driver-registrar`, `liveness-probe` | init: `modprobe` (항상 Succeeded) |
> | `agent` | DaemonSet | 스토리지 레이블 노드 수 | 1개: `agent` | `pillar-csi.bhyoo.com/storage-node=true` 레이블 노드에만 스케줄; 기본 Kind 환경에서 0개가 정상 |

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 225 | `TestHelm/전체_파드_Running_상태_종합_검증` | Helm 배포 후 `pillar-csi-system` 네임스페이스의 모든 파드가 `Running` 상태이고 재시작 없음을 종합 검증한다 | Kind 클러스터 실행 중; E26.1(`helm install --wait`) 완료; `kubectl` CLI 접근 가능 | 1) `kubectl get pods -n pillar-csi-system -o json` 실행; 2) 각 파드 `status.phase` 확인; 3) 각 파드의 모든 컨테이너 `ready` 상태 확인; 4) 각 컨테이너 `restartCount` 확인; 5) `status.conditions[type=Ready].status` 확인 | 모든 파드 `status.phase == "Running"`; 모든 컨테이너 `ready == true`; 모든 컨테이너 `restartCount == 0`; 모든 파드 `conditions[Ready].status == "True"` | `CSI-C`, `CSI-N`, `Kubernetes클러스터` |
| 226 | `TestHelm/컨트롤러_파드_컨테이너_5종_Ready_검증` | controller 파드에 5개 컨테이너(`controller`, `csi-provisioner`, `csi-attacher`, `csi-resizer`, `liveness-probe`)가 모두 `ready`임을 검증한다 | E26.1 완료; controller Deployment `availableReplicas ≥ 1` | 1) `kubectl get pods -n pillar-csi-system -l app.kubernetes.io/component=controller -o json`; 2) 파드 목록에서 첫 번째 파드 선택; 3) `.status.containerStatuses[]` 순회하여 각 `name`, `ready`, `restartCount` 확인 | 컨테이너 수 == 5; 각 컨테이너 이름: `controller`, `csi-provisioner`, `csi-attacher`, `csi-resizer`, `liveness-probe`; 모든 `ready == true`; 모든 `restartCount == 0`; `state.running` 존재 (startedAt 비어 있지 않음) | `CSI-C`, `Kubernetes클러스터` |
| 227 | `TestHelm/노드_파드_컨테이너_3종_Ready_및_initContainer_Succeeded_검증` | node DaemonSet 파드에 메인 컨테이너 3개와 init 컨테이너 1개(`modprobe`)가 올바른 상태임을 검증한다 | E26.1 완료; node DaemonSet `numberReady ≥ 1`; Kind 클러스터 워커 노드 ≥ 1 | 1) `kubectl get pods -n pillar-csi-system -l app.kubernetes.io/component=node -o json`; 2) 각 파드에 대해: `.status.initContainerStatuses[0]` (modprobe) 상태 확인; 3) `.status.containerStatuses[]` 순회하여 메인 컨테이너 3종 확인 | 파드 수 ≥ 1; 각 파드의 init 컨테이너 `modprobe`: `state.terminated.exitCode == 0` (Succeeded); 메인 컨테이너 수 == 3; 컨테이너 이름: `node`, `node-driver-registrar`, `liveness-probe`; 모든 `ready == true`; 모든 `restartCount == 0` | `CSI-N`, `Kubernetes클러스터` |
| 228 | `TestHelm/에이전트_DaemonSet_스토리지_레이블_없는_환경에서_파드_미스케줄_검증` | 스토리지 레이블(`pillar-csi.bhyoo.com/storage-node=true`)이 없는 Kind 기본 환경에서 agent 파드가 0개임을 검증한다 | E26.1 완료; Kind 클러스터 노드에 스토리지 레이블 없음 | 1) `kubectl get pods -n pillar-csi-system -l app.kubernetes.io/component=agent -o json`; 2) `.items` 배열 크기 확인; 3) `kubectl get daemonset -n pillar-csi-system -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].status.desiredNumberScheduled}'` 확인 | `items` 배열 길이 == 0 (파드 없음); DaemonSet `.status.desiredNumberScheduled == 0`; DaemonSet 자체는 존재하고 `.status` 필드 유효함 | `Agent`, `Kubernetes클러스터` |
| 229 | `TestHelm/에이전트_파드_스토리지_레이블_노드에서_Running_검증` | 스토리지 레이블을 부여한 노드에 agent 파드가 스케줄되어 `Running` 상태임을 검증한다 | E26.1 완료; Kind 클러스터 워커 노드 ≥ 1; 해당 노드에 `kubectl label node <node> pillar-csi.bhyoo.com/storage-node=true` 적용됨 | 1) 워커 노드에 스토리지 레이블 적용; 2) `kubectl rollout status daemonset -n pillar-csi-system -l app.kubernetes.io/component=agent --timeout=3m` 대기; 3) `kubectl get pods -n pillar-csi-system -l app.kubernetes.io/component=agent -o json`; 4) 파드 상태 확인 | DaemonSet `desiredNumberScheduled ≥ 1`; agent 파드 수 ≥ 1; 각 파드 `status.phase == "Running"`; init 컨테이너 `modprobe`: `exitCode == 0`; 메인 컨테이너 `agent`: `ready == true`, `restartCount == 0` | `Agent`, `Kubernetes클러스터` |
| 230 | `TestHelm/파드_Ready_Condition_Timeout_검증` | Helm `--wait --timeout 5m` 완료 후 모든 파드의 `Ready` 조건이 `True`이고, `LastTransitionTime`이 5분 이내임을 검증한다 | E26.1 완료 (`--wait --timeout 5m` 사용) | 1) `kubectl get pods -n pillar-csi-system -o json`; 2) 각 파드의 `.status.conditions[]` 순회하여 `type == "Ready"` 조건 검색; 3) `status`, `lastTransitionTime` 확인 | 모든 파드의 `conditions[Ready].status == "True"`; `lastTransitionTime`이 현재 시각 기준 5분 이내; `ContainersReady` 조건도 `True` | `CSI-C`, `CSI-N`, `Kubernetes클러스터` |
| 231 | `TestHelm/파드_재시작_없음_5분_관찰_검증` | 모든 파드가 Running 전환 후 5분 동안 재시작 없이 안정적임을 검증한다 | E26.1 완료; 파드 Running 전환 후 5분 경과 | 1) 초기 재시작 횟수 기록: `kubectl get pods -n pillar-csi-system -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.containerStatuses[*]}{.restartCount}{" "}{end}{"\n"}{end}'`; 2) 5분 대기; 3) 재시작 횟수 재확인; 4) 초기 값과 비교 | 5분 후 재시작 횟수 변화 없음 (모든 컨테이너 `ΔrestartCount == 0`); 파드 수 변화 없음 (파드가 새로 생성되거나 종료되지 않음) | `CSI-C`, `CSI-N`, `Kubernetes클러스터` |

> **⚠️ CI 제약 (agent DaemonSet):** Kind 기본 환경에서는 노드에 스토리지 레이블(`pillar-csi.bhyoo.com/storage-node=true`)이 없으므로 agent 파드가 0개 스케줄된다. E228(레이블 없음 검증)은 CI에서 자동 실행 가능하지만, E229(레이블 부여 후 Running 검증)는 Kind 노드에 레이블을 직접 부여하는 추가 셋업이 필요하다.

**종합 검증 스크립트 예시 (kubectl JSON 기반):**

```bash
#!/bin/bash
# 모든 파드 상태 종합 검증
NAMESPACE="pillar-csi-system"
FAILED=0

echo "=== pillar-csi 파드 Running 상태 종합 검증 ==="

# 1. 모든 파드 목록 및 phase 확인
kubectl get pods -n "$NAMESPACE" -o json | jq -r '
  .items[] |
  {
    name: .metadata.name,
    phase: .status.phase,
    ready: (.status.conditions[] | select(.type=="Ready") | .status),
    containers: [.status.containerStatuses[]? | {name: .name, ready: .ready, restartCount: .restartCount}]
  }
'

# 2. Running이 아닌 파드 검출
NOT_RUNNING=$(kubectl get pods -n "$NAMESPACE" \
  -o jsonpath='{range .items[?(@.status.phase!="Running")]}{.metadata.name}{"\n"}{end}')

if [ -n "$NOT_RUNNING" ]; then
  echo "FAIL: Running이 아닌 파드 발견:"
  echo "$NOT_RUNNING"
  FAILED=1
fi

# 3. ready==false인 컨테이너 검출
NOT_READY=$(kubectl get pods -n "$NAMESPACE" -o json | jq -r '
  .items[] | .metadata.name as $pod |
  .status.containerStatuses[]? |
  select(.ready == false) |
  [$pod, .name, "ready=false"] | join("  ")
')

if [ -n "$NOT_READY" ]; then
  echo "FAIL: ready==false 컨테이너 발견:"
  echo "$NOT_READY"
  FAILED=1
fi

# 4. 재시작 횟수 > 0인 컨테이너 검출
RESTARTED=$(kubectl get pods -n "$NAMESPACE" -o json | jq -r '
  .items[] | .metadata.name as $pod |
  .status.containerStatuses[]? |
  select(.restartCount > 0) |
  [$pod, .name, ("restartCount=" + (.restartCount | tostring))] | join("  ")
')

if [ -n "$RESTARTED" ]; then
  echo "WARN: restartCount > 0인 컨테이너 발견 (0 기대):"
  echo "$RESTARTED"
  FAILED=1
fi

# 5. 컨트롤러 파드 컨테이너 5종 확인
CTRL_CONTAINERS=$(kubectl get pods -n "$NAMESPACE" \
  -l app.kubernetes.io/component=controller \
  -o jsonpath='{range .items[0].status.containerStatuses[*]}{.name}{"\n"}{end}' | sort)
EXPECTED_CTRL="controller\ncsi-attacher\ncsi-provisioner\ncsi-resizer\nliveness-probe"
if [ "$(echo -e "$EXPECTED_CTRL" | sort)" != "$CTRL_CONTAINERS" ]; then
  echo "FAIL: 컨트롤러 컨테이너 목록 불일치"
  echo "기대: $(echo -e "$EXPECTED_CTRL" | sort)"
  echo "실제: $CTRL_CONTAINERS"
  FAILED=1
fi

# 6. 노드 파드 컨테이너 3종 + init 1종 확인
NODE_CONTAINERS=$(kubectl get pods -n "$NAMESPACE" \
  -l app.kubernetes.io/component=node \
  -o jsonpath='{range .items[0].status.containerStatuses[*]}{.name}{"\n"}{end}' | sort)
EXPECTED_NODE="liveness-probe\nnode\nnode-driver-registrar"
if [ "$(echo -e "$EXPECTED_NODE" | sort)" != "$NODE_CONTAINERS" ]; then
  echo "FAIL: 노드 컨테이너 목록 불일치"
  echo "기대: $(echo -e "$EXPECTED_NODE" | sort)"
  echo "실제: $NODE_CONTAINERS"
  FAILED=1
fi

NODE_INIT=$(kubectl get pods -n "$NAMESPACE" \
  -l app.kubernetes.io/component=node \
  -o jsonpath='{.items[0].status.initContainerStatuses[0].name}')
NODE_INIT_EXIT=$(kubectl get pods -n "$NAMESPACE" \
  -l app.kubernetes.io/component=node \
  -o jsonpath='{.items[0].status.initContainerStatuses[0].state.terminated.exitCode}')
if [ "$NODE_INIT" != "modprobe" ] || [ "$NODE_INIT_EXIT" != "0" ]; then
  echo "FAIL: node init container modprobe 상태 이상 (name=$NODE_INIT, exitCode=$NODE_INIT_EXIT)"
  FAILED=1
fi

if [ "$FAILED" -eq 0 ]; then
  echo "PASS: 모든 파드 Running 상태 검증 통과"
  exit 0
else
  echo "FAIL: 일부 검증 항목 실패"
  exit 1
fi
```

**Go 테스트 구현 참고 (Ginkgo/Gomega):**

```go
//go:build e2e
// +build e2e

package e2e

import (
    "encoding/json"
    "os/exec"
    "time"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"

    "github.com/bhyoo/pillar-csi/test/utils"
)

var _ = Describe("E26.11 전체 파드 Running 상태 종합 검증", Ordered, func() {
    const ns = "pillar-csi-system"

    // E225: 모든 파드 Running 종합 검증
    It("E225: 전체_파드_Running_상태_종합_검증", func() {
        verifyAllPodsRunning := func(g Gomega) {
            cmd := exec.Command("kubectl", "get", "pods", "-n", ns, "-o", "json")
            out, err := utils.Run(cmd)
            g.Expect(err).NotTo(HaveOccurred())

            var podList struct {
                Items []struct {
                    Metadata struct{ Name string `json:"name"` } `json:"metadata"`
                    Status struct {
                        Phase      string `json:"phase"`
                        Conditions []struct {
                            Type   string `json:"type"`
                            Status string `json:"status"`
                        } `json:"conditions"`
                        ContainerStatuses []struct {
                            Name         string `json:"name"`
                            Ready        bool   `json:"ready"`
                            RestartCount int    `json:"restartCount"`
                        } `json:"containerStatuses"`
                    } `json:"status"`
                } `json:"items"`
            }
            g.Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
            g.Expect(podList.Items).NotTo(BeEmpty(), "pillar-csi-system에 파드가 없음")

            for _, pod := range podList.Items {
                g.Expect(pod.Status.Phase).To(Equal("Running"),
                    "파드 %s: phase != Running (실제: %s)", pod.Metadata.Name, pod.Status.Phase)

                for _, cs := range pod.Status.ContainerStatuses {
                    g.Expect(cs.Ready).To(BeTrue(),
                        "파드 %s 컨테이너 %s: ready == false", pod.Metadata.Name, cs.Name)
                    g.Expect(cs.RestartCount).To(BeZero(),
                        "파드 %s 컨테이너 %s: restartCount %d (0 기대)",
                        pod.Metadata.Name, cs.Name, cs.RestartCount)
                }
            }
        }
        Eventually(verifyAllPodsRunning, 5*time.Minute, 5*time.Second).Should(Succeed())
    })

    // E226: 컨트롤러 파드 컨테이너 5종 Ready 검증
    It("E226: 컨트롤러_파드_컨테이너_5종_Ready_검증", func() {
        expectedCtrlContainers := []string{
            "controller", "csi-provisioner", "csi-attacher", "csi-resizer", "liveness-probe",
        }
        verifyCtrlContainers := func(g Gomega) {
            cmd := exec.Command("kubectl", "get", "pods", "-n", ns,
                "-l", "app.kubernetes.io/component=controller", "-o", "json")
            out, err := utils.Run(cmd)
            g.Expect(err).NotTo(HaveOccurred())
            // ... (파싱 및 검증 로직)
            _ = out
            _ = expectedCtrlContainers
        }
        Eventually(verifyCtrlContainers, 3*time.Minute, 5*time.Second).Should(Succeed())
    })

    // E227: 노드 파드 컨테이너 3종 + init modprobe 검증
    It("E227: 노드_파드_컨테이너_3종_Ready_및_initContainer_Succeeded_검증", func() {
        // 구현 참고: initContainerStatuses[0].state.terminated.exitCode == 0 확인
    })

    // E228: agent DaemonSet — 레이블 없는 환경에서 파드 미스케줄 검증
    It("E228: 에이전트_DaemonSet_스토리지_레이블_없는_환경에서_파드_미스케줄_검증", func() {
        cmd := exec.Command("kubectl", "get", "pods", "-n", ns,
            "-l", "app.kubernetes.io/component=agent", "-o", "json")
        out, err := utils.Run(cmd)
        Expect(err).NotTo(HaveOccurred())

        var podList struct {
            Items []interface{} `json:"items"`
        }
        Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
        Expect(podList.Items).To(BeEmpty(),
            "스토리지 레이블 없는 환경에서 agent 파드가 스케줄되면 안 됨")
    })
})
```

> **구현 파일 위치:** `test/e2e/helm_pod_running_e2e_test.go` (신규 생성 예정)
> **빌드 태그:** `//go:build e2e`
> **실행:** `go test ./test/e2e/ -tags=e2e -run TestHelm -v`

---

### E26.12 CSIDriver 객체 생성 및 설정 검증

> **목적:** Helm 설치 후 Kubernetes API 서버에 `CSIDriver` 오브젝트(`pillar-csi.bhyoo.com`)가 올바르게 생성되고,
> `spec` 필드 전체(attachRequired, podInfoOnMount, fsGroupPolicy, volumeLifecycleModes)가 `values.yaml` 기본값과 일치함을 검증한다.
>
> **CI 실행 가능 여부:** ✅ Kind 클러스터에서 실행 가능 (빌드 태그: `e2e`)
>
> **관련 소스:**
> - Helm 템플릿: `charts/pillar-csi/templates/csidriver.yaml`
> - 기본값 정의: `charts/pillar-csi/values.yaml` (`.csiDriver` 섹션)
>
> **기본값 스펙 요약:**
>
> | 필드 | 기본값 | 설명 |
> |------|-------|------|
> | `spec.attachRequired` | `true` | ControllerPublishVolume/ControllerUnpublishVolume 호출 필요 여부 |
> | `spec.podInfoOnMount` | `true` | NodePublishVolume 호출 시 Pod 이름·네임스페이스·UID를 volumeAttributes로 주입 |
> | `spec.fsGroupPolicy` | `File` | kubelet이 볼륨 fsGroup 소유권 변경을 처리하는 방식 (`None`, `File`, `ReadWriteOnceWithFSType` 중 선택) |
> | `spec.volumeLifecycleModes` | `["Persistent"]` | 드라이버가 지원하는 볼륨 수명 주기 모드 |
> | `metadata.name` | `pillar-csi.bhyoo.com` | CSIDriver 오브젝트 이름 (클러스터 전역 고유) |

#### E26.12.1 CSIDriver 존재 및 이름 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 232 | `TestHelm/CSIDriver_존재_검증` | Helm 설치 후 `pillar-csi.bhyoo.com` 이름의 CSIDriver 오브젝트가 클러스터에 존재한다 | E26.1 완료 (`helm install --wait` 성공); `kubectl` 클러스터 컨텍스트 설정됨 | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o json` 실행; 2) 종료 코드 확인; 3) `.metadata.name` 필드 확인 | 종료 코드 0; `.metadata.name == "pillar-csi.bhyoo.com"`; `.kind == "CSIDriver"`; `.apiVersion == "storage.k8s.io/v1"` | `전체시스템`, `Kubernetes클러스터` |
| 233 | `TestHelm/CSIDriver_전체_스펙_JSON_파싱_가능` | `kubectl get csidriver -o json` 출력이 유효한 JSON이고 CSIDriver 오브젝트 구조가 올바르다 | E26.1 완료 | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o json`; 2) JSON 파싱; 3) 필수 최상위 필드 존재 확인 | JSON 파싱 성공; `.kind == "CSIDriver"`; `.apiVersion == "storage.k8s.io/v1"`; `.metadata` 오브젝트 존재; `.spec` 오브젝트 존재 | `전체시스템`, `Kubernetes클러스터` |

**검증 명령 예시:**
```bash
# CSIDriver 존재 및 이름 확인
kubectl get csidriver pillar-csi.bhyoo.com
# 기대 출력 (일부):
# NAME                     ATTACHREQUIRED   PODINFOONMOUNT   STORAGECAPACITY   TOKENREQUESTS   REQUIRESREPUBLISH   MODES        AGE
# pillar-csi.bhyoo.com     true             true             false             <unset>         false               Persistent   ...

# JSON 전체 구조 확인
kubectl get csidriver pillar-csi.bhyoo.com -o json | jq '{
  kind: .kind,
  apiVersion: .apiVersion,
  name: .metadata.name,
  spec: .spec
}'
```

---

#### E26.12.2 attachRequired 필드 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 234 | `TestHelm/CSIDriver_attachRequired_true_검증` | CSIDriver `.spec.attachRequired`가 `true`이다 — ControllerPublishVolume/ControllerUnpublishVolume 호출이 필수임을 선언 | E26.1 완료 (기본 values 사용) | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.attachRequired}'` 실행; 2) 출력값 확인 | 출력 `true`; 이 값이 `true`이면 kubelet이 볼륨 연결 전 attach 단계를 수행하고, CSI Controller에 ControllerPublishVolume을 호출함을 보장 | `CSI-C`, `Kubernetes클러스터` |

> **pillar-csi에서 `attachRequired: true`의 의미:**
> - kubelet은 NodeStageVolume/NodePublishVolume 호출 전 `VolumeAttachment` 오브젝트를 생성한다.
> - CSI 컨트롤러 사이드카(csi-attacher)가 `VolumeAttachment`를 감시하고 `ControllerPublishVolume` RPC를 호출한다.
> - pillar-csi의 ControllerPublishVolume은 NVMe-oF 연결 준비 등 attach 로직을 수행하므로, `false`로 변경하면 볼륨이 노드에 접근 불가능해진다.

**검증 명령 예시:**
```bash
kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.attachRequired}'
# 기대 출력: true
```

---

#### E26.12.3 podInfoOnMount 필드 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 235 | `TestHelm/CSIDriver_podInfoOnMount_true_검증` | CSIDriver `.spec.podInfoOnMount`가 `true`이다 — NodePublishVolume 호출 시 Pod 메타데이터가 volumeAttributes로 주입됨 | E26.1 완료 (기본 values 사용) | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.podInfoOnMount}'` 실행; 2) 출력값 확인 | 출력 `true`; kubelet이 NodePublishVolume 호출 시 `csi.storage.k8s.io/pod.name`, `csi.storage.k8s.io/pod.namespace`, `csi.storage.k8s.io/pod.uid` 키를 `volume_context`에 포함시킴 | `CSI-N`, `Kubernetes클러스터` |

> **pillar-csi에서 `podInfoOnMount: true`의 의미:**
> - NodePublishVolume의 `volume_context` 맵에 다음 키가 자동 추가된다:
>   - `csi.storage.k8s.io/pod.name`: Pod 이름
>   - `csi.storage.k8s.io/pod.namespace`: Pod 네임스페이스
>   - `csi.storage.k8s.io/pod.uid`: Pod UID
>   - `csi.storage.k8s.io/serviceAccount.name`: ServiceAccount 이름
> - pillar-csi NodeServer는 이 정보를 로깅 및 접근 제어 목적으로 활용할 수 있다.
> - `false`로 설정하면 이 키들이 volume_context에 포함되지 않아 Pod 추적이 불가능해진다.

**검증 명령 예시:**
```bash
kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.podInfoOnMount}'
# 기대 출력: true
```

---

#### E26.12.4 fsGroupPolicy 필드 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 236 | `TestHelm/CSIDriver_fsGroupPolicy_File_검증` | CSIDriver `.spec.fsGroupPolicy`가 `"File"`이다 — kubelet이 항상 재귀적으로 볼륨 소유권을 fsGroup으로 변경 | E26.1 완료 (기본 values 사용) | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.fsGroupPolicy}'` 실행; 2) 출력값 확인 | 출력 `File`; 허용값: `None`, `File`, `ReadWriteOnceWithFSType` 중 하나; 기본값은 `File` | `CSI-N`, `Kubernetes클러스터` |
| 236a | `TestHelm/CSIDriver_fsGroupPolicy_유효값_범위_검증` | CSIDriver `.spec.fsGroupPolicy` 값이 Kubernetes 허용 범위(`None`, `File`, `ReadWriteOnceWithFSType`) 내에 있다 | E26.1 완료 | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.fsGroupPolicy}'` 실행; 2) 허용값 집합에 포함되는지 확인 | 출력이 `None`, `File`, `ReadWriteOnceWithFSType` 중 하나; 빈 문자열이 아님; API 서버가 유효성 검증 통과 후 저장한 값이므로 항상 유효한 열거형 | `전체시스템`, `Kubernetes클러스터` |

> **fsGroupPolicy 값별 동작 차이:**
> | 값 | kubelet 동작 |
> |---|---|
> | `None` | fsGroup 변경 없음 (드라이버가 직접 처리) |
> | `File` | 마운트 시 항상 재귀적 chown (기본값, pillar-csi 사용) |
> | `ReadWriteOnceWithFSType` | ReadWriteOnce 볼륨이고 fsType이 지정된 경우에만 chown |

**검증 명령 예시:**
```bash
kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.fsGroupPolicy}'
# 기대 출력: File
```

---

#### E26.12.5 volumeLifecycleModes 필드 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 237 | `TestHelm/CSIDriver_volumeLifecycleModes_Persistent_검증` | CSIDriver `.spec.volumeLifecycleModes`에 `"Persistent"` 항목이 존재한다 | E26.1 완료 (기본 values 사용) | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.volumeLifecycleModes}'` 실행; 2) 출력 배열에 `Persistent` 포함 확인 | 출력에 `Persistent` 포함; 배열 길이 ≥ 1; `Ephemeral` 항목 없음(기본 설정에서는 비활성화) | `전체시스템`, `Kubernetes클러스터` |
| 237a | `TestHelm/CSIDriver_volumeLifecycleModes_Ephemeral_미포함_검증` | 기본 values에서 CSIDriver `.spec.volumeLifecycleModes`에 `"Ephemeral"` 항목이 없다 | E26.1 완료 (기본 values 사용; `volumeLifecycleModes: [Persistent]`) | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o json`; 2) `.spec.volumeLifecycleModes` 배열에서 `"Ephemeral"` 검색 | 배열에 `"Ephemeral"` 없음; pillar-csi는 임시 볼륨(ephemeral inline volume)을 현재 지원하지 않으므로 이 모드가 없어야 함 | `전체시스템`, `Kubernetes클러스터` |

> **volumeLifecycleModes 허용값:**
> - `Persistent`: PVC/PV 기반의 영속적 볼륨 (pillar-csi 기본 지원 모드)
> - `Ephemeral`: Pod 수명과 동일한 임시 인라인 볼륨 (pillar-csi 미지원)
>
> 이 필드를 생략하면 API 서버가 기본값으로 `["Persistent"]`를 채운다.

**검증 명령 예시:**
```bash
# volumeLifecycleModes 배열 전체 출력
kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.volumeLifecycleModes}'
# 기대 출력: [Persistent]

# JSON 형식으로 더 명확하게 확인
kubectl get csidriver pillar-csi.bhyoo.com \
  -o json | jq '.spec.volumeLifecycleModes'
# 기대 출력:
# [
#   "Persistent"
# ]
```

---

#### E26.12.6 Helm 레이블 및 어노테이션 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 238 | `TestHelm/CSIDriver_Helm_레이블_검증` | CSIDriver에 Helm 표준 레이블(`app.kubernetes.io/*`)이 올바르게 설정되어 있다 | E26.1 완료 | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o json`; 2) `.metadata.labels` 필드 검증 | `app.kubernetes.io/name` 존재; `app.kubernetes.io/instance` 존재 (릴리스 이름 `pillar-csi`); `app.kubernetes.io/managed-by == "Helm"`; `helm.sh/chart` 레이블 존재 (e.g. `pillar-csi-0.1.0`) | `전체시스템`, `Kubernetes클러스터` |
| 238a | `TestHelm/CSIDriver_Helm_managed_by_레이블_검증` | CSIDriver에 `app.kubernetes.io/managed-by: Helm` 레이블이 있다 | E26.1 완료 | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}'` | 출력 `Helm`; 이 레이블이 없으면 `helm uninstall`이 이 리소스를 관리하지 않음 | `전체시스템`, `Kubernetes클러스터` |
| 239 | `TestHelm/CSIDriver_Helm_어노테이션_검증` | CSIDriver에 Helm 관리 어노테이션(`meta.helm.sh/*`)이 설정되어 있다 | E26.1 완료 | 1) `kubectl get csidriver pillar-csi.bhyoo.com -o json`; 2) `.metadata.annotations` 필드 검증 | `meta.helm.sh/release-name == "pillar-csi"` 어노테이션 존재; `meta.helm.sh/release-namespace == "pillar-csi-system"` 어노테이션 존재 | `전체시스템`, `Kubernetes클러스터` |

> **⚠️ CSIDriver는 클러스터-범위(non-namespaced) 리소스이다:** `CSIDriver`는 네임스페이스가 없는 클러스터 전역 리소스이다. Helm이 `pillar-csi-system` 네임스페이스에 릴리스를 배포하더라도, CSIDriver는 클러스터 수준에서 생성된다. 따라서 `helm uninstall` 시 CSIDriver도 삭제되며(CRD와 달리 `resource-policy: keep`이 없음), 이는 ID 223 테스트에서 확인된다.

**검증 명령 예시:**
```bash
# Helm 레이블 확인
kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.metadata.labels}' | jq .
# 기대 출력 (예시):
# {
#   "app.kubernetes.io/instance": "pillar-csi",
#   "app.kubernetes.io/managed-by": "Helm",
#   "app.kubernetes.io/name": "pillar-csi",
#   "app.kubernetes.io/version": "0.1.0",
#   "helm.sh/chart": "pillar-csi-0.1.0"
# }

# Helm 어노테이션 확인
kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.metadata.annotations}' | jq .
# 기대 출력 (예시):
# {
#   "meta.helm.sh/release-name": "pillar-csi",
#   "meta.helm.sh/release-namespace": "pillar-csi-system"
# }
```

---

#### E26.12.7 csiDriver.create=false 시 CSIDriver 미생성 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 240 | `TestHelm/CSIDriver_create_false_미생성_검증` | `--set csiDriver.create=false`로 설치하면 CSIDriver 오브젝트가 생성되지 않는다 | Kind 클러스터; 이전 pillar-csi 릴리스 해제 완료 (또는 별도 릴리스 이름 사용); Helm v3.12+ | 1) `helm install pillar-csi-nocsidrv ./charts/pillar-csi --namespace pillar-csi-nocsidrv --create-namespace --set csiDriver.create=false --wait --timeout 5m`; 2) `kubectl get csidriver pillar-csi.bhyoo.com` 실행 | helm install 종료 코드 0; `kubectl get csidriver pillar-csi.bhyoo.com` 출력에 `NotFound` 오류 반환 또는 해당 항목 없음; 다른 리소스(Deployment, DaemonSet)는 정상 생성됨 | `전체시스템`, `Kubernetes클러스터` |

> **`csiDriver.create=false` 사용 시나리오:**
> - GitOps 환경에서 CSIDriver를 별도의 클러스터-관리 매니페스트로 관리할 때
> - 멀티-테넌트 클러스터에서 클러스터 관리자가 CSIDriver를 사전 등록하고 사용자는 나머지 리소스만 설치할 때
> - CSIDriver가 이미 다른 릴리스에서 설치된 상태에서 pillar-csi 워크로드만 업그레이드할 때
>
> **주의:** `csiDriver.create=false` 상태에서 pillar-csi 워크로드를 배포하면, 클러스터에 CSIDriver 오브젝트가 없어 kubelet이 해당 드라이버를 인식하지 못할 수 있다. 이 설정은 반드시 외부에서 CSIDriver를 사전 등록한 경우에만 사용해야 한다.

**검증 명령 예시:**
```bash
helm install pillar-csi-nocsidrv ./charts/pillar-csi \
  --namespace pillar-csi-nocsidrv \
  --create-namespace \
  --set csiDriver.create=false \
  --wait \
  --timeout 5m

# CSIDriver가 없음을 확인
kubectl get csidriver pillar-csi.bhyoo.com 2>&1
# 기대 출력: Error from server (NotFound): csidrivers.storage.k8s.io "pillar-csi.bhyoo.com" not found

# 다른 리소스(Deployment)는 정상 생성됨 확인
kubectl get deployment -n pillar-csi-nocsidrv
# 기대 출력: controller Deployment 존재

# 정리
helm uninstall pillar-csi-nocsidrv --namespace pillar-csi-nocsidrv
kubectl delete namespace pillar-csi-nocsidrv
```

---

#### E26.12.8 커스텀 values로 CSIDriver 설정 오버라이드 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 241 | `TestHelm/CSIDriver_podInfoOnMount_false_오버라이드_검증` | `--set csiDriver.podInfoOnMount=false`로 설치 시 CSIDriver `.spec.podInfoOnMount`가 `false`이다 | Kind 클러스터; 별도 릴리스 이름 또는 이전 릴리스 해제 완료 | 1) `helm install pillar-csi-custom ./charts/pillar-csi --namespace pillar-csi-custom --create-namespace --set csiDriver.podInfoOnMount=false --wait --timeout 5m`; 2) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.podInfoOnMount}'` | 출력 `false`; CSIDriver 오브젝트 존재; 다른 spec 필드(attachRequired, fsGroupPolicy, volumeLifecycleModes)는 기본값 유지 | `전체시스템`, `Kubernetes클러스터` |
| 242 | `TestHelm/CSIDriver_fsGroupPolicy_None_오버라이드_검증` | `--set csiDriver.fsGroupPolicy=None`으로 설치 시 CSIDriver `.spec.fsGroupPolicy`가 `"None"`이다 | Kind 클러스터; 별도 릴리스 이름 또는 이전 릴리스 해제 완료 | 1) `helm install pillar-csi-nofsg ./charts/pillar-csi --namespace pillar-csi-nofsg --create-namespace --set csiDriver.fsGroupPolicy=None --wait --timeout 5m`; 2) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.fsGroupPolicy}'` | 출력 `None`; CSIDriver 오브젝트 존재 | `전체시스템`, `Kubernetes클러스터` |
| 243 | `TestHelm/CSIDriver_helm_upgrade_spec_변경_반영_검증` | `helm upgrade --set csiDriver.podInfoOnMount=false`로 업그레이드 시 CSIDriver spec이 갱신된다 | E26.1 완료 (`podInfoOnMount: true` 기본값 설치됨) | 1) `helm upgrade pillar-csi ./charts/pillar-csi --namespace pillar-csi-system --set csiDriver.podInfoOnMount=false --wait --timeout 5m`; 2) `kubectl get csidriver pillar-csi.bhyoo.com -o jsonpath='{.spec.podInfoOnMount}'` | 출력 `false`; `helm status`에서 `REVISION: 2` 확인; CSIDriver는 immutable 필드가 없으므로 패치(PATCH)로 업데이트됨 | `전체시스템`, `Kubernetes클러스터` |

> **⚠️ CI 제약 (네임스페이스 격리):** E241, E242는 각각 별도의 릴리스 이름(`pillar-csi-custom`, `pillar-csi-nofsg`)과 네임스페이스를 사용하므로, 동일 클러스터에서 순차 또는 병렬 실행이 가능하다. 그러나 CSIDriver는 클러스터-범위 리소스이므로, 모든 릴리스가 동일한 CSIDriver 이름(`pillar-csi.bhyoo.com`)을 사용하면 충돌이 발생한다. 따라서 E241, E242 테스트는 **E26.1이 사용 중인 클러스터에서는 동시에 실행할 수 없다** — 별도의 Kind 클러스터 또는 순차 실행(E26.10 완료 후)이 필요하다.

**검증 명령 예시:**
```bash
# E241: podInfoOnMount=false 오버라이드
helm install pillar-csi-custom ./charts/pillar-csi \
  --namespace pillar-csi-custom \
  --create-namespace \
  --set csiDriver.podInfoOnMount=false \
  --wait --timeout 5m

kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.podInfoOnMount}'
# 기대 출력: false

# E243: helm upgrade로 spec 변경
helm upgrade pillar-csi ./charts/pillar-csi \
  --namespace pillar-csi-system \
  --set csiDriver.podInfoOnMount=false \
  --wait --timeout 5m

kubectl get csidriver pillar-csi.bhyoo.com \
  -o jsonpath='{.spec.podInfoOnMount}'
# 기대 출력: false (업그레이드 후)

helm status pillar-csi --namespace pillar-csi-system \
  --output json | jq '.version'
# 기대 출력: 2 (REVISION 증가)
```

---

#### E26.12 종합 커버리지 요약

| 검증 항목 | 테스트 ID | CI 가능 | 비고 |
|---------|---------|:------:|------|
| CSIDriver 존재 및 JSON 구조 | 232, 233 | ✅ | Kind 클러스터; E26.1 선행 필요 |
| `spec.attachRequired == true` | 234 | ✅ | Kind 클러스터 |
| `spec.podInfoOnMount == true` | 235 | ✅ | Kind 클러스터 |
| `spec.fsGroupPolicy == "File"` | 236, 236a | ✅ | Kind 클러스터 |
| `spec.volumeLifecycleModes` 배열에 `"Persistent"` 포함 | 237 | ✅ | Kind 클러스터 |
| `spec.volumeLifecycleModes`에 `"Ephemeral"` 없음 | 237a | ✅ | Kind 클러스터 |
| Helm 표준 레이블(`app.kubernetes.io/*`) | 238, 238a | ✅ | Kind 클러스터 |
| Helm 어노테이션(`meta.helm.sh/*`) | 239 | ✅ | Kind 클러스터 |
| `csiDriver.create=false` 시 미생성 | 240 | ✅ | 별도 릴리스 이름·네임스페이스 필요 |
| `podInfoOnMount=false` 오버라이드 | 241 | ✅ | 별도 클러스터 또는 E26.10 후 실행 |
| `fsGroupPolicy=None` 오버라이드 | 242 | ✅ | 별도 클러스터 또는 E26.10 후 실행 |
| `helm upgrade`로 spec 갱신 | 243 | ✅ | E26.1 + E26.9 선행 필요 |

**Go 테스트 구현 참고 (Ginkgo/Gomega):**

```go
//go:build e2e
// +build e2e

package e2e

import (
    "encoding/json"
    "os/exec"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"

    "github.com/bhyoo/pillar-csi/test/utils"
)

var _ = Describe("E26.12 CSIDriver 객체 생성 및 설정 검증", Ordered, func() {
    const csiDriverName = "pillar-csi.bhyoo.com"

    type CSIDriverSpec struct {
        AttachRequired        bool     `json:"attachRequired"`
        PodInfoOnMount        bool     `json:"podInfoOnMount"`
        FsGroupPolicy         string   `json:"fsGroupPolicy"`
        VolumeLifecycleModes  []string `json:"volumeLifecycleModes"`
    }
    type CSIDriverObj struct {
        Kind       string            `json:"kind"`
        APIVersion string            `json:"apiVersion"`
        Metadata   struct {
            Name        string            `json:"name"`
            Labels      map[string]string `json:"labels"`
            Annotations map[string]string `json:"annotations"`
        } `json:"metadata"`
        Spec CSIDriverSpec `json:"spec"`
    }

    getCSIDriver := func(g Gomega) CSIDriverObj {
        cmd := exec.Command("kubectl", "get", "csidriver", csiDriverName, "-o", "json")
        out, err := utils.Run(cmd)
        g.Expect(err).NotTo(HaveOccurred(), "CSIDriver 조회 실패: %s", csiDriverName)
        var obj CSIDriverObj
        g.Expect(json.Unmarshal([]byte(out), &obj)).To(Succeed())
        return obj
    }

    // E232: CSIDriver 존재 및 이름 검증
    It("E232: CSIDriver_존재_검증", func() {
        Eventually(func(g Gomega) {
            obj := getCSIDriver(g)
            g.Expect(obj.Kind).To(Equal("CSIDriver"))
            g.Expect(obj.APIVersion).To(Equal("storage.k8s.io/v1"))
            g.Expect(obj.Metadata.Name).To(Equal(csiDriverName))
        }, "2m", "5s").Should(Succeed())
    })

    // E234: attachRequired=true 검증
    It("E234: CSIDriver_attachRequired_true_검증", func() {
        Eventually(func(g Gomega) {
            obj := getCSIDriver(g)
            g.Expect(obj.Spec.AttachRequired).To(BeTrue(),
                "attachRequired는 true여야 함 (ControllerPublish/Unpublish 필수)")
        }, "2m", "5s").Should(Succeed())
    })

    // E235: podInfoOnMount=true 검증
    It("E235: CSIDriver_podInfoOnMount_true_검증", func() {
        Eventually(func(g Gomega) {
            obj := getCSIDriver(g)
            g.Expect(obj.Spec.PodInfoOnMount).To(BeTrue(),
                "podInfoOnMount는 true여야 함 (Pod 메타데이터 주입 활성화)")
        }, "2m", "5s").Should(Succeed())
    })

    // E236: fsGroupPolicy=File 검증
    It("E236: CSIDriver_fsGroupPolicy_File_검증", func() {
        Eventually(func(g Gomega) {
            obj := getCSIDriver(g)
            g.Expect(obj.Spec.FsGroupPolicy).To(Equal("File"),
                "fsGroupPolicy는 'File'이어야 함")
            g.Expect([]string{"None", "File", "ReadWriteOnceWithFSType"}).
                To(ContainElement(obj.Spec.FsGroupPolicy),
                "fsGroupPolicy는 허용된 열거형 값이어야 함")
        }, "2m", "5s").Should(Succeed())
    })

    // E237: volumeLifecycleModes=[Persistent] 검증
    It("E237: CSIDriver_volumeLifecycleModes_Persistent_검증", func() {
        Eventually(func(g Gomega) {
            obj := getCSIDriver(g)
            g.Expect(obj.Spec.VolumeLifecycleModes).To(ContainElement("Persistent"),
                "volumeLifecycleModes에 'Persistent'가 포함되어야 함")
            g.Expect(obj.Spec.VolumeLifecycleModes).NotTo(ContainElement("Ephemeral"),
                "기본 설정에서 'Ephemeral' 모드는 포함되면 안 됨")
        }, "2m", "5s").Should(Succeed())
    })

    // E238: Helm 레이블 검증
    It("E238: CSIDriver_Helm_레이블_검증", func() {
        Eventually(func(g Gomega) {
            obj := getCSIDriver(g)
            g.Expect(obj.Metadata.Labels).To(HaveKey("app.kubernetes.io/name"))
            g.Expect(obj.Metadata.Labels).To(HaveKey("app.kubernetes.io/instance"))
            g.Expect(obj.Metadata.Labels["app.kubernetes.io/managed-by"]).To(Equal("Helm"))
            g.Expect(obj.Metadata.Labels).To(HaveKey("helm.sh/chart"))
        }, "2m", "5s").Should(Succeed())
    })

    // E239: Helm 어노테이션 검증
    It("E239: CSIDriver_Helm_어노테이션_검증", func() {
        Eventually(func(g Gomega) {
            obj := getCSIDriver(g)
            g.Expect(obj.Metadata.Annotations["meta.helm.sh/release-name"]).
                To(Equal("pillar-csi"))
            g.Expect(obj.Metadata.Annotations["meta.helm.sh/release-namespace"]).
                To(Equal("pillar-csi-system"))
        }, "2m", "5s").Should(Succeed())
    })
})
```

> **구현 파일 위치:** `test/e2e/helm_csidriver_e2e_test.go` (신규 생성 예정)
> **빌드 태그:** `//go:build e2e`
> **실행:** `go test ./test/e2e/ -tags=e2e -run "TestHelm.*CSIDriver" -v`

---

# 카테고리 3 — 완전 E2E / 수동 스테이징 테스트 (유형 F) ❌

> **빌드 태그:** `//go:build e2e_full` | **실행:** `go test ./test/e2e/ -tags=e2e_full -v`
>
> 실제 ZFS 커널 모듈 필요 · 실제 NVMe-oF 커널 모듈 필요 · 베어메탈 또는 KVM 서버 필요

이 카테고리의 테스트는 **실제 스토리지 하드웨어와 커널 모듈**을 요구하며,
표준 컨테이너 기반 CI에서는 **실행 불가능**하다.
self-hosted 러너 또는 전용 스테이징 서버가 필요하다.

---

## 유형 F: 완전 E2E 테스트 (Full E2E) ❌ 표준 CI 불가

> ⚠️ **참조:** 이 섹션은 `//go:build e2e_full` 태그를 사용하는 F1–F26 체계이다.
> 문서 하단의 [수동/스테이징 테스트 카탈로그](#수동스테이징-테스트-카탈로그-manualstaging-tests)는
> `//go:build hardware` 태그를 사용하는 별도의 F1–F12 체계이며, **F4 이후 번호가 충돌한다.**
> 구현 시 두 섹션 중 하나의 체계로 통일해야 한다.

이 섹션은 **실제 스토리지 백엔드(ZFS, NVMe-oF), 실제 커널 모듈, 실제 Kubernetes
클러스터 + pillar-agent**를 필요로 하는 완전 E2E 테스트의 권위 있는 명세이다.
표준 GitHub Actions / GitLab CI 컨테이너 환경에서는 **실행 불가능**하며,
물리 서버 또는 KVM/베어메탈 self-hosted 러너가 필요하다.

**현재 구현 상태:** 모든 유형 F 테스트는 **미구현(planned)** 상태이다.
이 문서는 구현 전 설계 사양을 정의한다. 구현 시 빌드 태그
`//go:build e2e_full`을 사용해야 한다.

```
실행 명령:
  go test ./test/e2e/ -tags=e2e_full -v -timeout 600s

특정 그룹만:
  go test ./test/e2e/ -tags=e2e_full -v -run TestRealZFS
  go test ./test/e2e/ -tags=e2e_full -v -run TestRealNode
  go test ./test/e2e/ -tags=e2e_full -v -run TestKubernetes
```

**총 유형 F 테스트 케이스: 38개** (F1.1–F26.2)

---

### 유형 F 인프라 요구사항 매트릭스

각 테스트 그룹에서 필요한 인프라 구성 요소를 정리한다.

| 그룹 | 테스트 번호 | ZFS 커널 모듈 | nvmet 커널 모듈 | nvme-tcp 커널 모듈 | 루트 권한 | 실제 K8s 클러스터 | pillar-agent DaemonSet | 최소 노드 수 |
|------|------------|:-------------:|:---------------:|:------------------:|:---------:|:-----------------:|:---------------------:|:----------:|
| ZFS 백엔드 | F1.1–F3.3 | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | 1 |
| K8s 클러스터 통합 | F4.1–F6.2 | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | 2+ |
| mTLS 인증서 갱신 | F7.1–F7.2 | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ | 1 |
| 실제 노드 마운트 | F8.1–F12.2 | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | 1 |
| ZFS 스냅샷/복원 | F13.1–F16.1 | ✅ | ✅ | ✅ | ❌ | ✅ (F16만) | ✅ (F16만) | 1 (F16: 2+) |
| 볼륨 마이그레이션 | F17.1–F19.2 | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | 2 (F19만) |
| 볼륨 클론 | F20.1–F20.2 | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | 1 |
| 확장/용량 한계 | F21.1–F23.1 | ✅ | ✅ | ✅ | ❌ | ✅ (F21만) | ✅ (F21만) | 1 |
| 커널 레이스 및 확장성 | F24.1–F26.2 | ✅ (F25만) | ✅ (F24만) | ✅ (F24만) | ✅ (F24, F26) | ✅ (F25만) | ✅ (F25만) | 2+ (F25만) |

#### 전체 환경 구성 요구사항

| 항목 | 사양 / 버전 | 비고 |
|------|------------|------|
| OS | Ubuntu 22.04+ (베어메탈 또는 KVM) | ZFS/nvme 커널 모듈 지원 필수 |
| Linux 커널 | 5.15+ | `nvme-tcp`, `nvmet`, `nvmet-tcp` 모듈 포함 |
| ZFS 커널 모듈 | `zfsutils-linux` 2.1+ | `zfs`, `zpool`, `zvol` 명령 포함 |
| nvmet 커널 모듈 | `nvmet`, `nvmet-tcp` | `modprobe nvmet nvmet-tcp` |
| nvme-tcp 커널 모듈 | `nvme-tcp` | `modprobe nvme-tcp` |
| `nvme-cli` | 2.x | `nvme connect`, `nvme disconnect`, `nvme list` |
| 전용 디스크 / 블록 디바이스 | 최소 10 GiB 여유 | ZFS pool 생성용 (`/dev/sdb` 등) |
| RAM | 4 GiB+ | ZFS ARC 캐시 + Kubernetes 컴포넌트 |
| CPU | 2코어+ | NVMe-oF TCP 처리량 |
| Kubernetes 클러스터 | v1.29+ (F4–F6, F16, F21, F25) | kubeadm 또는 k3s |
| cert-manager | v1.14+ (F7, F16) | 클러스터 내 설치 |
| external-snapshotter | v7+ (F16) | VolumeSnapshot CRD 및 controller |
| pillar-csi 이미지 | 로컬 빌드 | `make docker-build IMG=...` |
| 루트 권한 | `sudo` 또는 직접 root | F8.1–F12.2, F24.1–F24.2, F26.1–F26.2 |
| Go 빌드 도구체인 | 1.22+ | 테스트 컴파일 |

#### Self-Hosted 러너 구성 예시 (GitHub Actions)

```yaml
# .github/workflows/e2e-full.yml
name: "Full E2E Tests (Self-Hosted)"
on:
  workflow_dispatch:       # 수동 실행만
  schedule:
    - cron: '0 2 * * 1'   # 매주 월요일 02:00 UTC

jobs:
  e2e-full:
    name: "Full E2E (Real ZFS + NVMe-oF)"
    runs-on: [self-hosted, linux, zfs, nvmeof]   # 라벨이 붙은 러너 필요
    timeout-minutes: 60
    env:
      ZFS_POOL: "tank"
      ZFS_POOL_DEVICE: "/dev/sdb"
      NVMEOF_TARGET_ADDR: "127.0.0.1"
      NVMEOF_TARGET_PORT: "4420"
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.22"
      - name: Setup ZFS pool
        run: |
          sudo modprobe zfs nvmet nvmet-tcp nvme-tcp
          sudo zpool create -f "$ZFS_POOL" "$ZFS_POOL_DEVICE"
      - name: Run Full E2E Tests
        run: |
          sudo -E go test ./test/e2e/ -tags=e2e_full -v \
            -timeout 600s -count=1 \
            2>&1 | tee e2e-full.log
      - name: Teardown ZFS pool
        if: always()
        run: sudo zpool destroy "$ZFS_POOL" || true
      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: e2e-full-logs
          path: e2e-full.log
```

---

### F1–F3: 실제 ZFS 백엔드 및 NVMe-oF 내보내기 테스트

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** mock을 사용하는 유형 A/컴포넌트 테스트가 검증할 수 없는
**실제 `zfs(8)` 명령 실행, 실제 zvol 생성/삭제, 실제 configfs 조작, 실제 NVMe-oF
TCP 연결**을 검증한다.

**아키텍처:**
```
테스트 프로세스
    │
    ├──► ZFS 백엔드 (실제 zfs(8) 명령)
    │        └──► /dev/zvol/<pool>/<name>  [F1–F3]
    │
    ├──► NvmetTarget (실제 /sys/kernel/config/nvmet/)  [F2]
    │        └──► nvmet 커널 모듈 조작
    │
    └──► NVMe-oF Connector (실제 nvme connect)  [F3]
             └──► /dev/nvme<n>  블록 디바이스
```

**필수 인프라:**
- ZFS 커널 모듈 및 `zfs-utils` (`zfs`, `zpool` 명령)
- `nvmet`, `nvmet-tcp` 커널 모듈 (F2–F3)
- `nvme-tcp` 커널 모듈, `nvme-cli` (F3)
- 전용 블록 디바이스 (ZFS pool 생성용)
- ZFS pool 이름: 환경변수 `ZFS_POOL` (기본값: `tank`)

---

#### F1: 실제 ZFS zvol 생성/삭제

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F1.1 | `TestRealZFS_CreateVolume` | 실제 ZFS pool에서 zvol을 생성하고 `/dev/zvol/<pool>/<name>` 블록 디바이스가 나타남을 검증 | ZFS pool="tank" 준비됨; `zfsutils-linux` 설치됨; 환경변수 `ZFS_POOL=tank` 설정 | 1) agent.Server 초기화(실제 ZFSBackend); 2) agent.CreateVolume("tank/pvc-test", 1GiB) 호출; 3) `/dev/zvol/tank/pvc-test` 블록 디바이스 폴링; 4) `zfs list` 결과 검증 | `/dev/zvol/tank/<pvc-id>` 블록 디바이스 존재; `zfs list` 결과에 zvol 항목; capacity_bytes=1GiB | `ZFS`, `Agent`, `gRPC` |
| F1.2 | `TestRealZFS_CreateVolume_Idempotent` | 동일 zvol을 두 번 생성해도 동일한 devicePath 반환; 두 번째 호출 시 `zfs create` 미실행 | ZFS pool="tank" 준비됨; F1.1 환경과 동일 | 1) agent.CreateVolume("tank/pvc-idem", 1GiB) 1회 호출; 2) 동일 파라미터로 agent.CreateVolume 재호출; 3) devicePath 비교; 4) `zfs list` 결과 zvol 개수 확인 | 두 번째 호출 성공; devicePath 동일; `zfs list` 결과에 zvol 1개만 존재 | `ZFS`, `Agent`, `gRPC` |
| F1.3 | `TestRealZFS_DeleteVolume` | `agent.DeleteVolume` 호출 후 zvol 및 `/dev/zvol/` 디바이스 노드가 완전히 제거됨 | ZFS pool="tank" 준비됨; F1.1 성공으로 zvol 존재 | 1) agent.CreateVolume("tank/pvc-del", 1GiB) 호출; 2) `/dev/zvol/tank/pvc-del` 존재 확인; 3) agent.DeleteVolume("tank/pvc-del") 호출; 4) `/dev/zvol/` 경로 및 `zfs list` 재확인 | `/dev/zvol/tank/<pvc-id>` 없음; `zfs list` 결과에 해당 zvol 없음 | `ZFS`, `Agent`, `gRPC` |
| F1.4 | `TestRealZFS_CreateVolume_PoolFull` | ZFS pool 용량이 부족할 때 CreateVolume이 적절한 오류를 반환 | ZFS pool="tank" 준비됨; `zfs set quota=100M tank`로 쿼터 설정; pool 90M 이상 사용 중 | 1) `zfs set quota=100M tank` 설정; 2) 200MiB zvol 생성 시도 agent.CreateVolume 호출; 3) 반환된 gRPC 상태 코드 확인; 4) `/dev/zvol/` 디바이스 미생성 검증; 5) pool 상태 확인 | gRPC ResourceExhausted 또는 Internal; `/dev/zvol/` 디바이스 미생성; pool 상태 오염 없음 | `ZFS`, `Agent`, `gRPC` |
| F1.5 | `TestRealZFS_ExpandVolume` | 실제 zvol의 크기를 `zfs set volsize=` 를 통해 확장 후 블록 디바이스 크기 반영 검증 | ZFS pool="tank" 준비됨; F1.1로 1GiB zvol 존재 | 1) CreateVolume(1GiB) 확인; 2) agent.ExpandVolume("tank/pvc-expand", 2GiB) 호출; 3) `zfs get volsize tank/pvc-expand` 결과 검증; 4) `blockdev --getsize64 /dev/zvol/tank/pvc-expand` 확인 | `zfs get volsize` 결과 = 2GiB; 블록 디바이스 크기 2GiB; 데이터 손상 없음 | `ZFS`, `Agent`, `gRPC` |

---

#### F2: 실제 NVMe-oF configfs 내보내기

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F2.1 | `TestRealNVMeoF_Export` | 실제 `/sys/kernel/config/nvmet/` 에 NQN 서브시스템과 포트 설정이 생성됨 | `modprobe nvmet nvmet-tcp` 완료; ZFS zvol `/dev/zvol/tank/pvc-export` 존재; 포트 ID=1, NQN="nqn.2024.pillar.test" | 1) agent.Server 초기화(실제 configfsRoot=/sys/kernel/config); 2) agent.ExportVolume(nqn, portId=1, devicePath) 호출; 3) `/sys/kernel/config/nvmet/subsystems/<nqn>/namespaces/1/device_path` 내용 확인; 4) `enable` 파일 값 확인 | `/sys/kernel/config/nvmet/subsystems/<nqn>/` 디렉토리 존재; `device_path` = zvol 경로; `enable` = "1" | `NVMeF`, `Agent`, `ZFS`, `gRPC` |
| F2.2 | `TestRealNVMeoF_Export_Idempotent` | 동일한 볼륨을 두 번 ExportVolume 해도 configfs 구조 중복 없음 | F2.1 환경과 동일; ExportVolume 1회 성공 완료 | 1) agent.ExportVolume(nqn, portId=1, devicePath) 1회 호출; 2) 동일 파라미터로 agent.ExportVolume 재호출; 3) `/sys/kernel/config/nvmet/subsystems/` 하위 디렉토리 수 확인 | 두 번째 호출 성공; configfs 서브시스템 1개만 존재 | `NVMeF`, `Agent`, `gRPC` |
| F2.3 | `TestRealNVMeoF_UnexportVolume` | `agent.UnexportVolume` 호출 후 configfs 항목 완전 제거 | F2.1 성공 완료; configfs에 서브시스템 항목 존재 | 1) F2.1 ExportVolume 완료 확인; 2) agent.UnexportVolume(nqn) 호출; 3) `/sys/kernel/config/nvmet/subsystems/<nqn>/` 경로 없음 검증; 4) ports/ 디렉토리에서 연결 없음 확인 | `/sys/kernel/config/nvmet/subsystems/<nqn>/` 없음; 포트 연결 없음 | `NVMeF`, `Agent`, `gRPC` |

---

#### F3: 실제 NVMe-oF TCP 연결

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F3.1 | `TestRealNVMeoF_Connect` | `nvme connect --transport tcp` 성공 후 `/dev/nvme<n>` 블록 디바이스 탑재 검증 | F2.1 성공(nvmet 서버 로컬호스트 4420포트 리스닝 중); `modprobe nvme-tcp` 완료; `nvme-cli` 설치됨 | 1) Connector.Connect(nqn, "127.0.0.1", "4420") 호출; 2) 디바이스 경로 폴링(최대 30초); 3) `nvme list`로 디바이스 항목 확인; 4) `blockdev --getsize64 /dev/nvme<n>n<m>` 결과 검증 | `nvme list` 결과에 새 디바이스 항목; `/dev/nvme<n>n<m>` 블록 디바이스 존재; `blockdev --getsize64` = zvol 크기 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| F3.2 | `TestRealNVMeoF_Connect_Disconnect` | NVMe-oF 연결 후 `nvme disconnect` 실행 시 디바이스 노드 제거 검증 | F3.1 완료; 연결된 `/dev/nvme<n>n<m>` 존재 | 1) F3.1 Connect 성공 확인; 2) Connector.Disconnect(nqn) 호출; 3) `nvme list`로 디바이스 제거 확인; 4) `/dev/nvme<n>n<m>` 없음 검증 | `nvme list` 결과에서 해당 디바이스 없음; `/dev/nvme<n>n<m>` 없음 | `Conn`, `NVMeF`, `gRPC` |
| F3.3 | `TestRealNVMeoF_Connect_DeviceAppearDelay` | NVMe-oF connect 후 udev 처리 지연 환경에서 폴링 로직이 올바르게 대기하고 성공 | F2.1 환경 동일; `tc` 명령 사용 가능; iproute2 설치됨 | 1) `tc qdisc add dev lo root netem delay 500ms`로 루프백 지연 설정; 2) Connector.Connect(nqn, "127.0.0.1", "4420") 호출; 3) 디바이스 폴링 대기(30초 타임아웃); 4) 폴링 성공 후 `tc qdisc del` 정리 | 폴링 타임아웃 내(기본 30초) 디바이스 발견; NodeStage 성공 | `Conn`, `State` |

---

### F4–F6: Kubernetes 클러스터 + 실제 스토리지 통합 테스트

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** 실제 Kubernetes 클러스터에서 StorageClass, PVC, Pod를 통한 전체
프로비저닝 흐름을 검증한다. pillar-csi CSI 플러그인이 Kubernetes external-provisioner,
kubelet CSI 호출 체인, etcd, kube-controller-manager와 실제 통합되는지 확인한다.

**아키텍처:**
```
kubectl apply PVC
    │
    ▼
kube-controller-manager
    │  (external-provisioner 호출)
    ▼
pillar-csi ControllerServer (실제 Pod)
    │  (gRPC, mTLS)
    ▼
pillar-agent (실제 DaemonSet, 스토리지 노드)
    │
    ├──► ZFS 백엔드 (실제 zfs(8))
    └──► NVMe-oF configfs (실제 /sys/kernel/config/nvmet/)

kubelet
    │  (CSI NodeStage, NodePublish)
    ▼
pillar-csi NodeServer (실제 DaemonSet, 워커 노드)
    │
    ├──► nvme connect (실제 /dev/nvme*)
    └──► mount(8) (실제 마운트 포인트)
```

**필수 인프라:**
- Kubernetes 클러스터 v1.29+ (kubeadm 또는 k3s)
  - 컨트롤 플레인 노드 1개
  - 스토리지 노드 1개 이상 (ZFS, nvmet 커널 모듈 로드됨)
  - 워커 노드 1개 이상 (nvme-tcp 커널 모듈 로드됨)
- pillar-csi 컨테이너 이미지 (`make docker-build` 후 노드에 배포)
- CRD 설치 (`make install`)
- pillar-agent DaemonSet 배포 (스토리지 노드에 taint/toleration 설정)
- cert-manager v1.14+

---

#### F4: StorageClass → PVC → Pod 전체 프로비저닝 흐름

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F4.1 | `TestKubernetes_StorageClass_PVC` | StorageClass 생성 → PVC 생성 → PV 자동 프로비저닝 → Pod 마운트까지 전체 흐름 | Kubernetes v1.29+ 클러스터; pillar-csi CRD 설치; pillar-csi 컨테이너 이미지 배포; cert-manager v1.14+; ZFS 스토리지 노드 준비 | 1) StorageClass YAML 적용 (`kubectl apply`); 2) PVC(1GiB, RWO) YAML 적용; 3) PVC Bound 상태 대기(최대 120s); 4) Pod(volumeMounts: /data) YAML 적용; 5) Pod Running 대기; 6) `kubectl exec` 로 `df -h /data` 실행 | PVC Phase=Bound; PV 자동 생성; Pod 내 `/data` 마운트 성공; `df -h /data` 결과 1GiB 용량 표시 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `TgtCRD`, `VolCRD`, `gRPC` |
| F4.2 | `TestKubernetes_PVC_Delete_CleansUp` | Pod 삭제 → PVC 삭제 시 PV, ZFS zvol, NVMe-oF configfs 항목이 모두 정리됨 | F4.1 완료; Pod과 PVC 존재; ZFS zvol 및 configfs 항목 존재 | 1) `kubectl delete pod`; 2) Pod 완전 삭제 대기; 3) `kubectl delete pvc`; 4) PV 자동 삭제 대기; 5) 스토리지 노드에서 `zfs list`, configfs 상태 확인 | PV 자동 삭제 (ReclaimPolicy=Delete); ZFS zvol 없음; configfs 서브시스템 없음 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `NVMeF`, `VolCRD`, `gRPC` |
| F4.3 | `TestKubernetes_PVC_ReadWriteOnce_Exclusivity` | ReadWriteOnce PVC는 두 번째 Pod에서 동시 마운트가 불가능함을 검증 | Kubernetes 멀티-노드 클러스터; F4.1로 PVC 생성 및 Pod1 마운트 완료 | 1) PVC(RWO) + Pod1 마운트 성공 확인; 2) 다른 노드에 Pod2 YAML 적용(동일 PVC); 3) Pod2 이벤트 확인(`kubectl get events`); 4) 120초 대기 후 Pod2 상태 확인 | Pod2가 ContainerCreating 상태로 대기; 오류 이벤트에 "volume already attached" 또는 CSI ControllerPublish 거부 | `CSI-C`, `Agent`, `VolCRD`, `gRPC` |

---

#### F5: 볼륨 온라인 확장 (Kubernetes 통합)

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F5.1 | `TestKubernetes_VolumeExpansion` | PVC 용량 증가 요청 → ControllerExpandVolume → NodeExpandVolume → Pod 내 파일시스템 크기 자동 갱신 | F4.1 완료; PVC(1GiB) + Pod 실행 중; StorageClass에 `allowVolumeExpansion: true` | 1) `kubectl patch pvc <name> -p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}'`; 2) PVC capacity=2Gi 대기(120s); 3) `kubectl exec` 로 `df -h /data` 실행; 4) Pod 재시작 여부 확인 | PVC capacity=2GiB; Pod 내 `df -h /data` 결과 2GiB; Pod 재시작 불필요 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Mnt` |
| F5.2 | `TestKubernetes_VolumeExpansion_MultipleRounds` | 볼륨을 여러 번 단계적으로 확장해도 각 단계에서 Pod 내 파일시스템 크기 반영 | F5.1 완료; PVC(1GiB) + Pod 실행 중; 각 단계 후 `df -h` 검증 | 1) PVC → 2GiB 확장; 2) `df -h` 결과 2GiB 확인; 3) PVC → 4GiB 확장; 4) `df -h` 결과 4GiB 확인; 5) 각 단계에서 기존 파일 체크섬 검증 | 각 단계 후 `df` 결과가 새 크기 표시; 데이터 손상 없음 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Mnt` |

---

#### F6: 노드 장애 복구 (pillar-agent ReconcileState)

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F6.1 | `TestKubernetes_NodeFailover` | 스토리지 노드 재시작 후 pillar-agent가 configfs 상태를 자동으로 복원하고 기존 PVC 연결이 재개됨 | F4.1 완료; PVC + Pod 실행 중; 스토리지 노드 SSH 접근 가능 | 1) PVC/Pod 정상 동작 확인; 2) 스토리지 노드 재시작 (`sudo reboot`); 3) 노드 재기동 대기(최대 120s); 4) pillar-agent Pod 재기동 확인; 5) PillarTarget AgentConnected 조건 확인; 6) configfs 항목 재생성 검증; 7) Pod I/O 재개 확인 | 재시작 후 pillar-agent가 `ReconcileState` 호출; configfs 항목 재생성; Pod가 일시적 I/O 오류 후 자동 재연결; 데이터 손상 없음 | `Agent`, `NVMeF`, `TgtCRD`, `gRPC` |
| F6.2 | `TestKubernetes_AgentReconnect_MTLSCert` | pillar-agent 재시작 후 mTLS 인증서로 CSI 컨트롤러에 재연결 성공 | cert-manager 설치; 유효한 mTLS 인증서 발급; pillar-agent 실행 중 | 1) pillar-agent 프로세스 재시작 (`kubectl delete pod`); 2) agent Pod 재기동 대기; 3) PillarTarget 상태 조건 확인; 4) gRPC 호출 가능 여부 검증 | 재시작 후 AgentConnected=True 조건 복원; gRPC 연결 재수립; 기존 볼륨 연산 재개 | `mTLS`, `Agent`, `TgtCRD`, `gRPC` |

---

### F7: mTLS 인증서 실제 갱신 테스트

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** cert-manager가 발급한 TLS 인증서가 만료 임박 시 자동 갱신되고,
인증서 갱신 중에도 pillar-csi CSI 컨트롤러와 pillar-agent 간의 gRPC 연결이
중단 없이 유지되는지 검증한다.

**필수 인프라:**
- cert-manager v1.14+ (클러스터 내 설치)
- 짧은 유효 기간 인증서 발급 가능 설정 (`duration=1m`, `renewBefore=30s`)
- Kubernetes 클러스터

---

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F7.1 | `TestRealMTLS_CertRotation` | cert-manager가 짧은 유효 기간 인증서를 자동 갱신하고 갱신 중 gRPC 연결 유지 | cert-manager v1.14+ 설치; pillar-csi 배포 완료; Certificate 리소스의 duration=1m, renewBefore=30s 설정 | 1) 짧은 TTL Certificate 리소스 적용; 2) 인증서 갱신 대기(~30초); 3) 갱신 전 agent gRPC 호출(CreateVolume 등) 성공 확인; 4) 갱신 중 gRPC 호출 시도; 5) 갱신 후 gRPC 호출 성공 확인; 6) 인증서 시리얼 번호 변경 확인 | 갱신 전후 agent gRPC 호출 모두 성공; TLS 핸드셰이크 오류 없음; 인증서 시리얼 번호 변경 확인 | `mTLS`, `Agent`, `TgtCRD`, `gRPC` |
| F7.2 | `TestRealMTLS_ExpiredCert_Rejected` | 만료된 인증서로 연결 시도 시 gRPC 연결 거부 | cert-manager 설치; pillar-csi 배포 완료; 만료된 인증서 생성 도구(`openssl`) 사용 가능 | 1) 만료된 인증서와 키 생성(openssl); 2) pillar-agent Secret에 만료 인증서 주입; 3) pillar-agent Pod 재시작 트리거; 4) TLS 핸드셰이크 결과 확인; 5) cert-manager 정상 인증서 복원 후 재연결 확인 | TLS 핸드셰이크 실패; gRPC 연결 거부; cert-manager 갱신 후 정상 재연결 | `mTLS`, `Agent`, `TgtCRD`, `gRPC` |

---

### F8–F12: 실제 NVMe-oF 노드 마운트/언마운트 테스트

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** CSI NodeServer의 실제 동작을 검증한다. 유형 A 테스트의
mockConnector/mockMounter 대신 실제 `nvme connect`, `mount(8)`, `mkfs.ext4/xfs`,
`resize2fs/xfs_growfs` 명령이 실행되는 환경에서 테스트한다.

**아키텍처:**
```
테스트 프로세스 (root 권한 필요)
    │
    ├──► CSI NodeServer (실제 코드)
    │         │
    │         ├──► NVMe-oF Connector (실제 nvme-cli)
    │         │        └──► /dev/nvme<n>n<m>  블록 디바이스
    │         │
    │         └──► Mounter (실제 mount(8), mkfs.ext4)
    │                  └──► 실제 파일시스템 마운트
    │
    └──► NVMe-oF 대상 서버 (로컬호스트 nvmet)
             └──► ZFS zvol /dev/zvol/tank/<pvc-id>
```

**필수 인프라:**
- root 권한 (또는 `CAP_SYS_ADMIN`)
- `nvmet`, `nvmet-tcp`, `nvme-tcp` 커널 모듈
- `nvme-cli` (v2.x)
- `e2fsprogs` (`mkfs.ext4`, `resize2fs`)
- `xfsprogs` (`mkfs.xfs`, `xfs_growfs`)
- `mount(8)`, `umount(8)` (기본 설치)
- ZFS pool 및 zvol (F1 참조)

---

#### F8–F10: NodeStage / NodePublish / NodeUnstage 실제 마운트

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F8.1 | `TestRealNode_NodeStageVolume_ActualMount_ext4` | NodeStageVolume이 실제 NVMe-oF connect → `/dev/nvme*` 디바이스 → `mkfs.ext4` → 스테이징 경로 마운트를 순서대로 수행 | root 권한; nvmet/nvme-tcp 커널 모듈 로드; ZFS zvol `/dev/zvol/tank/pvc-node` 준비; e2fsprogs 설치; stagingTargetPath=`/tmp/stage-<vol>` 디렉터리 생성 | 1) NodeStageVolumeRequest(fsType=ext4, stagingPath) 전송; 2) `mount` 출력에서 stagingPath 항목 확인; 3) `df -T /tmp/stage-<vol>` 로 fstype=ext4 확인; 4) `nvme list`로 Connect 1회 확인 | `mount` 결과에 스테이징 경로 존재; `df -T` 결과 fstype=ext4; Connect 1회 호출 | `CSI-N`, `Conn`, `Mnt`, `ZFS`, `NVMeF`, `State` |
| F8.2 | `TestRealNode_NodeStageVolume_ActualMount_xfs` | fsType=xfs로 NodeStageVolume 수행 | F8.1과 동일하되 xfsprogs 설치; fsType=xfs | 1) NodeStageVolumeRequest(fsType=xfs, stagingPath) 전송; 2) `df -T` 확인; 3) `xfs_info stagingPath` 정상 출력 확인 | `df -T` 결과 fstype=xfs; `xfs_info` 정상 출력 | `CSI-N`, `Conn`, `Mnt`, `State` |
| F8.3 | `TestRealNode_NodeStageVolume_Idempotent` | NodeStageVolume 두 번째 호출이 멱등성 보장 (이미 마운트됨 감지 후 성공 반환) | F8.1 성공 완료; 동일 파라미터 준비 | 1) F8.1 NodeStageVolume 완료; 2) 동일 파라미터로 NodeStageVolumeRequest 재전송; 3) `mount` 결과에서 중복 마운트 없음 확인; 4) `nvme list` 디바이스 1개 확인 | 두 번째 호출 성공; `mount` 결과에 중복 마운트 없음; `nvme list` 결과에 디바이스 1개 | `CSI-N`, `Conn`, `Mnt`, `State` |
| F9.1 | `TestRealNode_NodePublishVolume_BindMount` | NodePublish가 스테이징 경로를 타깃 경로로 바인드 마운트 수행 | F8.1 NodeStage 완료; targetPath=`/tmp/target-<vol>` 디렉터리 생성 | 1) NodePublishVolumeRequest(stagingPath, targetPath) 전송; 2) `mount` 출력에서 targetPath에 `bind` 옵션 확인; 3) targetPath에 파일 쓰기/읽기 테스트 | `mount` 결과에 `bind` 옵션과 타깃 경로 존재; 타깃 경로에서 파일 I/O 가능 | `CSI-N`, `Mnt` |
| F9.2 | `TestRealNode_NodePublishVolume_MultiTargets` | 동일 스테이징에서 여러 타깃으로 바인드 마운트 | F8.1 완료; 3개의 서로 다른 targetPath 디렉터리 준비 | 1) NodePublishVolume(targetPath1) 호출; 2) NodePublishVolume(targetPath2) 호출; 3) NodePublishVolume(targetPath3) 호출; 4) 각 타깃에서 독립적 파일 I/O 테스트 | 3개 타깃 경로 모두 마운트됨; 각각 독립적으로 파일 I/O 가능 | `CSI-N`, `Mnt` |
| F10.1 | `TestRealNode_NodeUnstageVolume_ActualDetach` | NodeUnstage가 마운트 해제 → `nvme disconnect` → `/dev/nvme*` 제거까지 수행 | F8.1 + F9.1 완료; 스테이징 경로 마운트됨; NVMe 디바이스 존재 | 1) NodeUnpublishVolumeRequest(targetPath) 전송; 2) targetPath 마운트 해제 확인; 3) NodeUnstageVolumeRequest(stagingPath) 전송; 4) `mount` 결과 stagingPath 없음 확인; 5) `nvme list` 디바이스 없음 확인 | `mount` 결과에 스테이징 경로 없음; `nvme list` 결과에 해당 디바이스 없음; `/dev/nvme*` 없음 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

#### F11: udev 지연 환경에서 디바이스 폴링

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F11.1 | `TestRealNode_NodeStageVolume_DeviceAppearDelay` | NVMe-oF connect 성공 후 `/dev/nvme*` 디바이스가 즉시 나타나지 않고 지연되는 환경에서 폴링 로직이 정상 대기하고 최종 성공 | F2.1 환경(nvmet 서버 실행 중); root 권한; `tc` 명령 사용 가능; iproute2 설치됨 | 1) `tc qdisc add dev lo root netem delay 1s`로 루프백 지연 설정; 2) NodeStageVolumeRequest(fsType=ext4) 전송; 3) 폴링 로그 확인(1초 간격 재시도); 4) 디바이스 발견 및 마운트 완료 확인; 5) `tc qdisc del` 정리 | 폴링 타임아웃 내(30초) 디바이스 발견; NodeStage 성공; 로그에 폴링 재시도 기록 | `CSI-N`, `Conn`, `State` |
| F11.2 | `TestRealNode_NodeStageVolume_DeviceNeverAppears` | 디바이스가 폴링 타임아웃 내에 끝내 나타나지 않을 때 적절한 오류 반환 | nvme-tcp 커널 모듈 로드; 존재하지 않는 NQN 준비; 폴링 타임아웃=5초 설정 | 1) 실제로 연결 불가능한 NQN으로 NodeStageVolumeRequest 전송; 2) 5초 타임아웃 대기; 3) gRPC 상태 코드 확인; 4) `nvme list` 디바이스 없음 확인; 5) 상태 파일 미생성 확인 | gRPC FailedPrecondition 또는 Internal; 자동 nvme disconnect 호출; 스테이징 경로 마운트 미수행 | `CSI-N`, `Conn`, `State` |

---

#### F12: NVMe-oF Multipath

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F12.1 | `TestRealNode_MultiPathAttach` | 동일 NQN에 두 개의 네트워크 인터페이스로 연결 시 NodeStage가 올바른 블록 디바이스를 선택 | 두 개의 네트워크 인터페이스 존재; nvmet 서버가 두 IP(예: 192.168.1.10:4420, 192.168.2.10:4420) 리스닝; VolumeContext에 두 address 포함 | 1) NodeStageVolumeRequest(두 address 포함 VolumeContext) 전송; 2) `nvme list`로 두 경로 확인; 3) 올바른 디바이스 마운트 확인; 4) 파일 I/O 테스트 | 두 경로 모두 `/dev/nvme*`로 나타남; 올바른 디바이스 선택 및 마운트; multipath 또는 단일 경로 중 구현 정의 동작 | `CSI-N`, `Conn`, `Mnt`, `State` |
| F12.2 | `TestRealNode_MultiPath_OnePathDown` | 멀티패스 연결 중 하나의 경로가 끊어져도 파일시스템 I/O 지속 | F12.1 완료; 두 경로 모두 마운트된 상태 | 1) F12.1 NodeStage 완료 확인; 2) `ip link set <iface> down`으로 한 경로 비활성화; 3) 파일 I/O 지속 확인; 4) `dmesg` 로 경로 실패 메시지 확인; 5) `nvme list` 결과 검증 | I/O 지속 (다른 경로 사용); dmesg에 경로 실패 기록; `nvme list` 결과에 나머지 경로만 존재 | `CSI-N`, `Conn`, `Mnt` |

---

### F13–F16: ZFS 스냅샷, 복원 및 클론 테스트

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** CSI `CreateSnapshot`, `DeleteSnapshot`, `ListSnapshots` 기능 (현재 미구현)과
`VolumeContentSource.Snapshot`을 통한 PVC 복원 흐름을 검증한다.
이 테스트는 CSI 스냅샷 기능이 구현된 이후에 실행 가능하다.

**현재 구현 상태:** CSI CreateSnapshot/DeleteSnapshot/ListSnapshots 미구현.
유형 A E12 테스트가 이를 `Unimplemented` gRPC 코드로 반환함을 이미 검증한다.

**필수 인프라 (F13–F15):**
- ZFS 커널 모듈 및 `zfs-utils`
- CSI CreateSnapshot/DeleteSnapshot/ListSnapshots 구현 완료 후 활성화

**필수 인프라 (F16):**
- 위 + 실제 Kubernetes 클러스터, external-snapshotter v7+

---

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F13.1 | `TestRealZFS_CreateSnapshot` | CSI CreateSnapshot 호출 시 실제 `zfs snapshot pool/vol@<snap-id>` 실행 및 ReadyToUse=true 반환 | ZFS zvol `tank/pvc-snap` 존재; CSI CreateSnapshot 구현 완료; snapshot_name="snap-01" | 1) CSI CreateSnapshotRequest(source_volume_id, snapshot_name="snap-01") 전송; 2) `zfs list -t snapshot tank/pvc-snap@snap-01` 확인; 3) 응답 ReadyToUse 값 확인; 4) creation_time 검증 | `zfs list -t snapshot` 결과에 `tank/<pvc-id>@snap-01` 존재; ReadyToUse=true; creation_time 정확 | `CSI-C`, `Agent`, `ZFS`, `gRPC` |
| F13.2 | `TestRealZFS_CreateSnapshot_Idempotent` | 동일 스냅샷 이름으로 두 번 CreateSnapshot 호출 시 동일 응답 반환 | F13.1 환경과 동일; 첫 번째 CreateSnapshot 성공 | 1) CreateSnapshot(snapshot_name="snap-01") 1회 호출; 2) 동일 source_volume_id + snapshot_name으로 CreateSnapshot 재호출; 3) 응답 비교; 4) `zfs list -t snapshot` 스냅샷 개수 확인 | 두 번째 호출 성공; `zfs list` 결과에 스냅샷 1개; ReadyToUse=true | `CSI-C`, `Agent`, `ZFS`, `gRPC` |
| F14.1 | `TestRealZFS_DeleteSnapshot` | CSI DeleteSnapshot 호출 시 `zfs destroy pool/vol@snap` 실행 및 스냅샷 제거 | F13.1 완료; 스냅샷 `tank/pvc-snap@snap-01` 존재; CSI DeleteSnapshot 구현 완료 | 1) CSI DeleteSnapshotRequest(snapshot_id) 전송; 2) `zfs list -t snapshot` 스냅샷 없음 확인 | `zfs list -t snapshot` 결과에 해당 스냅샷 없음 | `CSI-C`, `Agent`, `ZFS`, `gRPC` |
| F14.2 | `TestRealZFS_DeleteSnapshot_Idempotent` | 이미 삭제된 스냅샷 DeleteSnapshot 호출 시 성공 반환 (멱등성) | F14.1 완료; 스냅샷 존재하지 않음 | 1) 동일 snapshot_id로 DeleteSnapshotRequest 재전송; 2) gRPC 상태 코드 확인 | 성공 반환 (gRPC OK); 오류 없음 | `CSI-C`, `Agent`, `ZFS`, `gRPC` |
| F15.1 | `TestRealZFS_ListSnapshots` | CSI ListSnapshots 호출 시 `zfs list -t snapshot` 결과를 올바르게 변환 | ZFS zvol `tank/pvc-list`에 스냅샷 3개 (`@s1`, `@s2`, `@s3`) 존재 | 1) CSI ListSnapshotsRequest 전송; 2) 응답 entries 수 확인; 3) 각 entry의 snapshot_id, source_volume_id, creation_time, ready_to_use 값 검증 | entries 3개; 각 entry에 snapshot_id, source_volume_id, creation_time, ready_to_use 포함 | `CSI-C`, `Agent`, `ZFS`, `gRPC` |
| F16.1 | `TestKubernetes_VolumeSnapshot_CreateRestore` | VolumeSnapshot CRD 생성 → PVC(RestoreFrom) → Pod 마운트 후 데이터 일관성 검증 | Kubernetes 클러스터; external-snapshotter v7+ 설치; VolumeSnapshotClass 적용; PVC + Pod 실행 중 | 1) Pod에서 테스트 파일 쓰기; 2) VolumeSnapshot YAML 적용; 3) VolumeSnapshot ReadyToUse 대기; 4) 복원 PVC(dataSourceRef=스냅샷) 생성; 5) 새 Pod 마운트; 6) 원본 파일 내용 검증; 7) 신규 파일 쓰기 가능 확인 | 복원 PVC의 Pod에서 원본 파일 내용 동일; 신규 파일 쓰기도 가능 | `CSI-C`, `Agent`, `ZFS`, `VolCRD`, `gRPC` |

---

### F17–F19: 볼륨 마이그레이션 (ZFS Send/Receive)

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** `agent.SendVolume` / `agent.ReceiveVolume` 스트리밍 gRPC RPC가
실제 `zfs send` / `zfs receive` 파이프라인과 올바르게 통합되는지 검증한다.
데이터 무결성 (checksum), 스트림 오류 처리, 크로스 노드 마이그레이션 시나리오를 다룬다.

**아키텍처:**
```
테스트 프로세스
    │
    ├──► agent.SendVolume (gRPC streaming, 노드 A)
    │         └──► zfs send pool/vol@snap | stream_to_grpc
    │
    └──► agent.ReceiveVolume (gRPC streaming, 노드 B)
              └──► stream_from_grpc | zfs receive pool/new-vol
```

**필수 인프라:**
- ZFS 커널 모듈, `zfs-utils`
- 단일 노드 (F17–F18) 또는 두 개 스토리지 노드 네트워크 연결 (F19)
- 네트워크 대역폭: 최소 1Gbps (크로스 노드)

---

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F17.1 | `TestRealAgent_SendVolume_ZFSSend` | `agent.SendVolume` 스트리밍 RPC가 `zfs send` 스트림을 gRPC 청크로 수신하고 checksum 일치 | ZFS zvol `tank/src-vol`(1GiB) + 스냅샷 `@snap0` 준비; 수신 파일 저장 경로 준비 | 1) agent.SendVolume(volumeId, snapshotName="snap0") 스트리밍 호출; 2) gRPC 스트림 청크를 파일로 저장; 3) 스트림 완료 대기; 4) 저장 파일 SHA256 계산; 5) `zfs send tank/src-vol@snap0 | sha256sum` 결과와 비교 | 스트림 완료; 수신 파일의 SHA256 = `zfs send <snap> \| sha256sum` 결과와 일치; 오류 없음 | `Agent`, `ZFS`, `gRPC` |
| F17.2 | `TestRealAgent_SendVolume_NetworkInterrupt` | 전송 중 네트워크 중단 시 적절한 스트리밍 오류 반환 | F17.1 환경 동일; `tc` 명령 사용 가능; 네트워크 차단 스크립트 준비 | 1) agent.SendVolume 스트리밍 시작; 2) 스트림 50% 진행 시 `tc` 로 네트워크 차단; 3) gRPC 스트림 오류 수신 대기; 4) ZFS 소스 데이터 무결성 검증 | gRPC 스트림 오류 반환; ZFS 소스 데이터 무손상; 재시도 가능 상태 | `Agent`, `ZFS`, `gRPC` |
| F18.1 | `TestRealAgent_ReceiveVolume_ZFSReceive` | `agent.ReceiveVolume` 스트리밍 RPC가 gRPC 스트림을 수신하여 `zfs receive` 로 볼륨 복원 | F17.1에서 저장한 ZFS 스트림 파일 준비; 대상 ZFS pool `tank2` 준비 | 1) agent.ReceiveVolume("tank2/dst-vol") 스트리밍 시작; 2) 저장된 스트림 파일에서 청크 전송; 3) 스트림 완료 대기; 4) `zfs list tank2/dst-vol` 확인; 5) 데이터 checksum 비교 | `zfs list` 결과에 새 볼륨 존재; 복원된 zvol의 데이터가 원본과 동일 (checksum); 블록 디바이스 크기 일치 | `Agent`, `ZFS`, `gRPC` |
| F18.2 | `TestRealAgent_ReceiveVolume_CorruptedStream` | 손상된 스트림 수신 시 `zfs receive` 오류 처리 및 불완전 볼륨 정리 | ZFS pool `tank2` 준비; 손상된(무작위 바이트 삽입) 스트림 파일 생성 | 1) agent.ReceiveVolume("tank2/bad-vol") 스트리밍 시작; 2) 손상된 스트림 청크 전송; 3) gRPC 오류 수신 대기; 4) `zfs list tank2/bad-vol` 없음 확인; 5) pool 상태 확인 | gRPC Internal 또는 DataLoss 반환; 불완전한 zvol 자동 정리; pool 상태 오염 없음 | `Agent`, `ZFS`, `gRPC` |
| F19.1 | `TestRealAgent_SendReceiveVolume_CrossNode` | 노드 A에서 SendVolume → 노드 B에서 ReceiveVolume 크로스 노드 마이그레이션; 마이그레이션 후 데이터 동일성 검증 | 두 스토리지 노드 네트워크 연결(1Gbps); 노드 A: ZFS zvol(1GiB) + 데이터 기록; 노드 B: 빈 ZFS pool | 1) 노드 A: agent.SendVolume 스트리밍 시작; 2) 스트림을 노드 B로 전달; 3) 노드 B: agent.ReceiveVolume 스트리밍 수신; 4) 복원 완료 후 두 노드 데이터 checksum 비교; 5) 소요 시간 기록 | 노드 B에 볼륨 복원 완료; 노드 B zvol 데이터 = 노드 A 원본 데이터; 마이그레이션 소요 시간 기록 | `Agent`, `ZFS`, `gRPC` |
| F19.2 | `TestRealAgent_SendReceiveVolume_LargeVolume` | 10GiB 볼륨 크로스 노드 마이그레이션; 스트림 청크 수, 총 소요 시간, 처리량 측정 | F19.1 환경 동일; 볼륨 크기 10GiB; 5분 타임아웃 설정 | 1) F19.1과 동일 절차로 10GiB 볼륨 마이그레이션; 2) 청크 수 카운터 기록; 3) 처리량(MiB/s) 계산; 4) 데이터 동일성 검증 | 마이그레이션 완료; 처리량 >= 100 MiB/s; 데이터 동일성; 오류 없음 | `Agent`, `ZFS`, `gRPC` |

---

### F20: 볼륨 클론 (ZFS Clone)

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** CSI `VolumeContentSource.Snapshot`을 통한 볼륨 클론이 `zfs clone`을
사용하여 기존 스냅샷으로부터 새 zvol을 생성하는지 검증한다.
현재 이 기능은 미구현 상태이다.

**필수 인프라:**
- ZFS 커널 모듈, `zfs-utils`
- CSI VolumeContentSource.Snapshot 처리 구현 완료 후 활성화

---

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F20.1 | `TestRealZFS_CloneVolume_FromSnapshot` | `CreateVolume(VolumeContentSource.Snapshot=snap-01)` 호출 시 `zfs clone pool/src@snap-01 pool/new-vol` 실행 후 새 zvol이 독립적으로 동작 | ZFS zvol `tank/src-vol` + 스냅샷 `@snap-01` 존재; CSI VolumeContentSource.Snapshot 처리 구현 완료 | 1) CSI CreateVolumRequest(contentSource.snapshot="snap-01") 전송; 2) `zfs list tank/new-vol` 확인; 3) 새 zvol에서 파일 내용 검증(= 스냅샷 시점 데이터); 4) 원본 수정 후 클론 데이터 변경 없음 확인 | `zfs list` 결과에 새 zvol 존재; 새 zvol의 데이터 = 스냅샷 시점 데이터; 원본 수정이 클론에 영향 없음 | `CSI-C`, `Agent`, `ZFS`, `VolCRD`, `gRPC` |
| F20.2 | `TestRealZFS_CloneVolume_DeleteIndependent` | 클론 볼륨 삭제 시 원본 스냅샷이 유지됨 | F20.1 완료; 클론 zvol 존재; 원본 스냅샷 존재 | 1) CSI DeleteVolumeRequest(클론 VolumeId) 전송; 2) `zfs list tank/new-vol` 없음 확인; 3) `zfs list -t snapshot tank/src-vol@snap-01` 존재 확인; 4) 원본 zvol 데이터 checksum 검증 | `zfs list` 결과에 클론 없음; 원본 스냅샷 유지; 원본 볼륨 데이터 무손상 | `CSI-C`, `Agent`, `ZFS`, `VolCRD`, `gRPC` |

---

### F21–F23: 볼륨 확장 및 스토리지 용량 한계 테스트

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** 실제 ZFS 환경에서 볼륨 확장, pool 용량 고갈 시나리오를 검증한다.
유형 A E15 테스트가 mock 오류 주입으로 오류 코드 전파를 검증하는 반면,
이 테스트는 **실제 ENOSPC 오류, 실제 파일시스템 리사이즈**를 검증한다.

**필수 인프라:**
- ZFS 커널 모듈, `zfs-utils`
- 용량 제한 가능한 ZFS pool (`zfs set quota=` 또는 소형 블록 디바이스)
- F21: 실제 Kubernetes 클러스터, Pod 실행 중, ZFS 노드

---

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F21.1 | `TestKubernetes_VolumeExpansion_OnlinePod` | 실행 중인 Pod의 PVC를 확장하면 Pod 내 파일시스템이 재시작 없이 새 크기로 자동 갱신 | Kubernetes 클러스터; PVC(1GiB) + Pod 실행 중; StorageClass에 `allowVolumeExpansion: true`; ZFS 스토리지 노드 연결 | 1) Pod에서 기준 파일 쓰기; 2) `kubectl patch pvc` 로 2GiB 요청; 3) PVC capacity=2Gi 대기(120s); 4) `kubectl exec` 로 `df -h /data` 결과 확인; 5) Pod 재시작 여부 확인; 6) 기존 파일 내용 검증 | Pod 내 `df -h /data` 결과 2GiB; Pod 재시작 없음; 기존 파일 무손상 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Mnt` |
| F21.2 | `TestKubernetes_VolumeExpansion_FilesystemResize_ext4` | ext4 파일시스템 리사이즈가 `resize2fs`를 통해 성공적으로 수행됨 | F21.1 환경 동일; fsType=ext4; `e2fsprogs` 설치됨 | 1) PVC(1GiB, ext4) + Pod 기동; 2) PVC → 2GiB 확장; 3) 워커 노드 로그에서 `resize2fs` 실행 확인; 4) `df -h /data` 결과 검증; 5) 기존 파일 무손상 확인 | `resize2fs` 명령 실행 로그 기록; Pod 내 파일시스템 크기 증가; 기존 데이터 무손상 | `CSI-N`, `Agent`, `ZFS`, `Mnt` |
| F21.3 | `TestKubernetes_VolumeExpansion_FilesystemResize_xfs` | xfs 파일시스템 리사이즈가 `xfs_growfs`를 통해 수행됨 | F21.1 환경 동일; fsType=xfs; `xfsprogs` 설치됨 | 1) PVC(1GiB, xfs) + Pod 기동; 2) PVC → 2GiB 확장; 3) 워커 노드 로그에서 `xfs_growfs` 실행 확인; 4) `xfs_info /data` 결과 새 크기 반영 확인 | `xfs_growfs` 실행 로그; Pod 내 `xfs_info` 결과 새 크기 반영 | `CSI-N`, `Agent`, `ZFS`, `Mnt` |
| F22.1 | `TestRealZFS_PoolFull_CreateVolume` | ZFS pool을 거의 가득 채운 후 새 zvol 생성 시도 시 실제 ENOSPC 오류 전파 검증 | ZFS pool `tank` 준비; `zfs set quota=500M tank`; 400MiB 데이터로 채움 | 1) `zfs set quota=500M tank` 설정; 2) 기존 더미 zvol(400MiB)로 pool 채움; 3) 200MiB zvol 생성 시도(CreateVolume 호출); 4) gRPC 상태 코드 확인; 5) `zfs list` 결과 변화 없음 확인 | gRPC ResourceExhausted 또는 Internal; zvol 미생성; `zfs list` 결과 변화 없음 | `ZFS`, `Agent`, `CSI-C`, `gRPC` |
| F22.2 | `TestRealZFS_PoolFull_CreateVolume_Cleanup` | 용량 부족으로 실패한 CreateVolume이 불완전한 ZFS 자원을 정리함 | F22.1 환경 동일 | 1) F22.1과 동일 절차로 CreateVolume 실패 유발; 2) `zfs list` 불완전 zvol 없음 확인; 3) configfs 항목 없음 확인; 4) pool 상태 `zpool status` 확인 | 실패 후 불완전 zvol 없음; configfs 항목 없음; pool 상태 오염 없음 | `ZFS`, `NVMeF`, `Agent`, `CSI-C`, `gRPC` |
| F23.1 | `TestRealZFS_PoolFull_ExpandVolume` | 현재 pool 여유 공간 이상의 용량으로 ExpandVolume 시도 시 실제 "out of space" 오류 | ZFS zvol `tank/pvc-expand`(1GiB) 존재; `zfs set quota=1500M tank`; pool 여유 ~500MiB | 1) ExpandVolume(2GiB) 요청 전송; 2) gRPC 상태 코드 확인; 3) `zfs get volsize tank/pvc-expand` 결과 1GiB 유지 확인 | gRPC ResourceExhausted 또는 Internal; zvol 크기 변화 없음 (1GiB 유지) | `ZFS`, `Agent`, `CSI-C`, `gRPC` |

---

### F24–F26: 커널 레이스 컨디션 및 확장성 테스트

**테스트 유형:** F (완전 E2E) ❌ 표준 CI 불가

**목적:** 실제 커널 레벨 동시성, 마운트 포인트 사용 중 오류, 대규모 PVC 확장성을
검증한다. 유형 A E16 테스트가 in-process 고루틴 레벨 동시성을 검증하는 반면,
이 테스트는 **실제 시스템 콜, 실제 커널 잠금, 실제 네임스페이스**를 다룬다.

**필수 인프라:**
- F24: nvmet/nvme-tcp 커널 모듈, root 권한, 실제 NVMe-oF 디바이스
- F25: 실제 Kubernetes 클러스터 (2+ 노드), pillar-agent DaemonSet, ZFS 노드
- F26: root 권한, mount(8), fuser/lsof

---

| # | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|---|------------|------|----------|------|----------|---------|
| F24.1 | `TestRealNode_ConcurrentStageUnstage_SameVolume` | 동일 볼륨에 NodeStage/NodeUnstage를 동시 호출 시 커널 레벨 레이스 컨디션 없음 검증 | root 권한; nvmet/nvme-tcp 커널 모듈; ZFS zvol 존재; 실제 NVMe-oF 디바이스 준비 | 1) 5개 goroutine 동시 시작; 각각 NodeStageVolume/NodeUnstageVolume 반복(5회); 2) 모든 goroutine WaitGroup 완료 대기; 3) `dmesg` 오류 확인; 4) 최종 mount 상태 일관성 검증 | 모든 연산 완료; 커널 패닉 없음; dmesg에 오류 없음; 최종 상태 일관성 | `CSI-N`, `Conn`, `Mnt`, `NVMeF`, `State` |
| F24.2 | `TestRealNode_ConcurrentNodePublish_SameStaging` | 동일 스테이징 경로에서 3개 타깃으로 동시 NodePublish | root 권한; F8.1 NodeStage 1회 완료; 3개 targetPath 디렉터리 준비 | 1) NodeStage 1회 완료; 2) 3개 goroutine 동시 시작(각각 다른 targetPath); 3) WaitGroup 완료 대기; 4) `mount` 결과 3개 바인드 마운트 확인; 5) 각 타깃에서 파일 I/O 검증 | 3개 바인드 마운트 모두 성공; 데이터 무결성; `mount` 결과 정확 | `CSI-N`, `Mnt` |
| F25.1 | `TestKubernetes_ManyPVCsConcurrent` | 100개 PVC 동시 생성 — controller, agent, ZFS 백엔드 확장성 및 레이스 컨디션 검증 | Kubernetes 클러스터(2+ 노드); pillar-agent DaemonSet; ZFS 스토리지 노드; 스토리지 충분(100GiB+) | 1) PVC YAML 100개 동시 `kubectl apply`; 2) 5분 타임아웃으로 모든 PVC Bound 대기; 3) `zfs list` zvol 100개 확인; 4) configfs 서브시스템 100개 확인; 5) 컨트롤러 오류 로그 확인 | 5분 내 100개 PVC 모두 Bound; ZFS zvol 100개; NVMe-oF 서브시스템 100개; controller 오류 없음 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `TgtCRD`, `VolCRD`, `gRPC` |
| F25.2 | `TestKubernetes_ManyPVCsConcurrent_Delete` | F25.1에서 생성한 100개 PVC를 동시 삭제 — 정리 완전성 검증 | F25.1 완료; 100개 PVC Bound 상태 | 1) `kubectl delete pvc --all`; 2) 5분 타임아웃으로 모든 PVC 삭제 대기; 3) `zfs list` zvol 0개 확인; 4) configfs 서브시스템 0개 확인; 5) 리소스 누수 없음 검증 | 5분 내 100개 PVC 모두 삭제; ZFS zvol 0개; configfs 서브시스템 0개; 리소스 누수 없음 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `VolCRD`, `gRPC` |
| F26.1 | `TestRealNode_UnmountWithBusyMount` | 마운트 포인트가 사용 중 (`fuser`로 프로세스 점유)일 때 NodeUnstage 호출 시 EBUSY 오류 처리 검증 | root 권한; F8.1+F9.1 완료; `psutil`/`fuser` 설치됨 | 1) NodeStage+NodePublish 완료; 2) 백그라운드 프로세스로 타깃 경로 내 파일 오픈(`tail -f`); 3) NodeUnpublishVolumeRequest 전송; 4) gRPC 상태 코드 확인; 5) `fuser`로 마운트 포인트 점유 확인 | gRPC Internal 또는 FailedPrecondition 반환; 마운트 포인트 유지; 프로세스 종료 후 재시도 성공 가능 | `CSI-N`, `Mnt` |
| F26.2 | `TestRealNode_UnmountWithBusyMount_ForceUmount` | lazy unmount (`umount -l`) 옵션이 활성화된 경우 EBUSY 상태에서도 NodeUnstage가 성공적으로 완료 | F26.1 환경 동일; pillar-csi 설정에서 `force_umount=true` 활성화 | 1) F26.1과 동일 설정(백그라운드 파일 오픈 상태); 2) NodeUnpublishVolumeRequest 전송; 3) NodeUnstageVolumeRequest 전송; 4) 마운트 포인트 제거 확인; 5) `nvme list` 디바이스 없음 확인 | NodeUnstage 성공; 마운트 포인트 제거; 프로세스는 파일 핸들 유지 (lazy detach); nvme disconnect 성공 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### 유형 F 커버리지 요약

유형 F 테스트가 검증하는 항목과, 유형 A/컴포넌트 테스트와의 차별점을 정리한다.

| 검증 항목 | 유형 A (in-process) | 유형 F (Full E2E) | 관련 테스트 |
|----------|:-------------------:|:-----------------:|------------|
| 실제 `zfs create` 실행 및 zvol 생성 | ❌ (mock) | ✅ | F1.1–F1.5 |
| 실제 `zfs destroy` 및 디바이스 노드 제거 | ❌ (mock) | ✅ | F1.3 |
| 실제 configfs 조작 (`/sys/kernel/config/nvmet/`) | ❌ (tmpdir) | ✅ | F2.1–F2.3 |
| 실제 `nvme connect` 및 `/dev/nvme*` 블록 디바이스 | ❌ (mock) | ✅ | F3.1–F3.3 |
| 실제 `mkfs.ext4` / `mkfs.xfs` | ❌ (mock) | ✅ | F8.1–F8.2 |
| 실제 `mount(8)` / 바인드 마운트 | ❌ (mock) | ✅ | F8.1–F10.1 |
| 실제 `resize2fs` / `xfs_growfs` | ❌ (mock) | ✅ | F21.2–F21.3 |
| 실제 ENOSPC (`zfs out of space`) | ❌ (mock 오류) | ✅ | F22.1–F23.1 |
| Kubernetes external-provisioner 통합 | ❌ | ✅ | F4.1–F4.3 |
| kubelet CSI 호출 체인 (NodeStage/NodePublish) | ❌ | ✅ | F4.1 |
| etcd 일관성 (실제 낙관적 잠금) | ❌ (fake client) | ✅ | F4, F25 |
| CRD 검증 웹훅 실제 실행 | ❌ (fake client) | ✅ | F4 |
| cert-manager 실제 인증서 갱신 | ❌ (in-memory) | ✅ | F7.1–F7.2 |
| ZFS send/receive 스트리밍 (실제 파이프) | ❌ | ✅ | F17–F19 |
| 크로스 노드 볼륨 마이그레이션 데이터 무결성 | ❌ | ✅ | F19.1–F19.2 |
| NVMe-oF multipath 연결 | ❌ (mock) | ✅ | F12.1–F12.2 |
| udev 지연 환경 폴링 | ❌ (mock 즉시 반환) | ✅ | F11.1–F11.2 |
| 실제 EBUSY 마운트 오류 처리 | ❌ | ✅ | F26.1–F26.2 |
| 100개 PVC 확장성 | ❌ (mock 즉시 처리) | ✅ | F25.1–F25.2 |
| ZFS 스냅샷 실제 생성/삭제/복원 | ❌ | ✅ | F13–F16 |
| 노드 재시작 후 agent 자동 복구 | ❌ | ✅ | F6.1 |

---


## 유형 M: 수동/스테이징 테스트 🔬 자동화 불가 — 수동 실행 전용

이 섹션은 **자동화된 CI 파이프라인으로 실행할 수 없거나, 현실적으로 자동화가
불가능한 테스트**의 권위 있는 명세이다. 각 테스트 항목에는 자동화 불가 이유,
필요한 스테이징 환경, 수동 실행 절차 체크리스트를 함께 기술한다.

**유형 M 테스트의 공통 특성:**

| 특성 | 설명 |
|------|------|
| 비결정적(Non-deterministic) 타이밍 | 실제 하드웨어 장애, 네트워크 지연, 인증서 TTL 등 실제 시간에 의존 |
| 인간 판단 필요 | 테스트 결과가 "통과/실패" 이분법으로 표현되지 않고 운영자 판단이 필요 |
| 환경 파괴적(Destructive) | 테스트 실행 중 실제 데이터 손실, 서비스 중단, 하드웨어 조작이 발생 |
| 재현 비용 과다 | 스테이징 클러스터 구축, 물리 서버 확보, 라이선스 비용 등 |
| 프로덕션 의존성 | 실제 워크로드 패턴, 실제 사용자 데이터, 실제 운영 환경 필요 |

**현재 구현 상태:** 모든 유형 M 테스트는 **수동 실행 체크리스트** 형태로
정의되며, Go 테스트 함수로 구현되지 않는다. 일부 항목은 미래에
유형 F(자동화) 테스트로 전환될 수 있으며 해당 항목에는 `→ F 전환 가능` 표시를 한다.

**총 유형 M 테스트 케이스: 42개** (M1–M10 그룹, 각 그룹 3–7개 시나리오)

---

### 유형 M 테스트 요약

| 그룹 | ID | 테스트 이름 | 자동화 불가 이유 | 스테이징 환경 |
|------|-----|-----------|----------------|--------------|
| M1 | M1.1–M1.4 | 롤링 업그레이드 검증 | 서비스 중단 관찰에 인간 판단 필요 | 멀티-노드 Kubernetes 클러스터 |
| M2 | M2.1–M2.5 | 스토리지 네트워크 분리(Network Partition) | 비결정적 타이밍; iptables 규칙 복잡 | 멀티-노드 + 별도 스토리지 네트워크 |
| M3 | M3.1–M3.4 | 물리적 디스크/하드웨어 장애 | 실제 물리 장비 조작 필요 | 베어메탈 서버 + 교체용 디스크 |
| M4 | M4.1–M4.5 | 커널 버전 호환성 매트릭스 | 다수의 커널 버전 환경 준비 비용 | 다중 OS 이미지 + 커널 변형 |
| M5 | M5.1–M5.5 | 프로덕션 유사 부하 및 용량 계획 | 실제 워크로드 해석에 전문가 판단 필요 | 대규모 스테이징 클러스터 |
| M6 | M6.1–M6.4 | 보안 감사 및 침투 테스트 | 결과 해석 및 위험 판단에 인간 개입 필수 | 격리된 보안 테스트 환경 |
| M7 | M7.1–M7.5 | 데이터 무결성 심층 검증 | 실제 데이터 검증 도구 + 긴 실행 시간 | 실제 데이터 워크로드 환경 |
| M8 | M8.1–M8.4 | CSI 드라이버 업그레이드 절차 검증 | 업그레이드 중 실시간 모니터링 필요 | 프로덕션 유사 Kubernetes 클러스터 |
| M9 | M9.1–M9.4 | 다중 테넌트 격리 검증 | 실제 테넌트 자격 증명 및 워크로드 필요 | 멀티-테넌트 클러스터 |
| M10 | M10.1–M10.7 | 인증서 수명 주기 및 실제 PKI 갱신 | 실제 인증서 TTL 대기 (최소 수 시간) | cert-manager + 실제 CA |

---

### 유형 M 스테이징 환경 요구사항

유형 M 테스트를 실행하기 위한 최소 스테이징 환경 사양이다. 이 환경은
**프로덕션 환경을 축소 복제한 것**이어야 하며, 테스트 후 완전한 정리가
가능해야 한다.

#### 스테이징 클러스터 사양

| 구성 요소 | 최소 사양 | 권장 사양 | 비고 |
|-----------|----------|----------|------|
| 컨트롤 플레인 노드 | 1개 (4코어, 8 GiB) | 3개 HA (4코어, 16 GiB) | Kubernetes v1.29+ |
| 스토리지 노드 | 2개 (4코어, 16 GiB, 100 GiB 디스크) | 4개 | pillar-agent DaemonSet 실행 |
| 워크로드 노드 | 2개 (4코어, 8 GiB) | 4개 | 실제 PVC 소비 워크로드 |
| 스토리지 네트워크 | 별도 L2 VLAN 또는 인터페이스 | 10 GbE 전용 NIC | NVMe-oF TCP 트래픽 분리 |
| OS | Ubuntu 22.04 LTS (베어메탈 또는 KVM) | Ubuntu 22.04/24.04 혼합 | ZFS/nvme 커널 모듈 지원 |
| 커널 | 5.15+ | 6.1 LTS | `nvme-tcp`, `nvmet`, `nvmet-tcp` 포함 |
| ZFS 풀 | 각 노드 100 GiB (루프백 가능) | 실제 NVMe/SSD | M3, M7용 실제 디스크 권장 |
| Kubernetes 버전 | v1.29 | v1.29, v1.30, v1.31 혼합 (M4용) | |
| cert-manager | v1.14+ | v1.15+ | |
| 모니터링 | Prometheus + Grafana | Prometheus + Grafana + Loki | M5 실행 시 필수 |
| 로드 생성기 | `fio` 또는 `dd` | `fio` + `pgbench` | M5, M7용 |

#### 스테이징 환경 네트워크 토폴로지

```
+---------------------------------------------------------------------------+
|                           스테이징 환경                                    |
|                                                                           |
|  +---------------+  관리 네트워크 (10.10.0.0/24)   +-------------------+  |
|  | 컨트롤 플레인   +--------------------------------+  워크로드 노드 (4개) |  |
|  |  (3개 노드)    |                                +-------------------+  |
|  +-------+-------+                                                       |
|          |                                                                |
|  +-------v-------+  스토리지 네트워크 (192.168.100.0/24)                  |
|  |  스토리지 노드  +-------------------------------------------------+     |
|  |   (4개 노드)   |    (NVMe-oF TCP 전용, 관리 네트워크와 분리)        |     |
|  |               |                                                   |     |
|  |  ZFS 풀:      |                                                   |     |
|  |  /dev/nvme*   |                                                   |     |
|  +---------------+                                                   |     |
|                                                                           |
|  +---------------------------------------------------------------------+  |
|  |  외부 서비스: cert-manager CA, LDAP/OIDC (M6, M9), 모니터링          |  |
|  +---------------------------------------------------------------------+  |
+---------------------------------------------------------------------------+
```

---

### M1: 롤링 업그레이드 검증

**목적:** pillar-csi 컨트롤러 및 pillar-agent DaemonSet의 롤링 업그레이드 중
실행 중인 PVC/Pod 워크로드의 I/O가 중단 없이 유지되는지, 또는 허용 가능한
수준의 중단만 발생하는지 검증한다.

**자동화 불가 이유:**

- 업그레이드 중 허용 가능한 I/O 중단 지속 시간(예: 30초 vs. 60초)은
  운영 정책에 따라 달라지며, 코드로 표현하기 어렵다.
- 롤링 업그레이드 중 NVMe-oF 세션이 유지되는지는 커널 드라이버 레벨
  동작으로, in-process mock으로는 재현 불가능하다.
- 장애 발생 시 인간이 업그레이드를 일시 정지하거나 롤백해야 하는 판단이 필요하다.
- 서로 다른 버전의 컨트롤러와 에이전트가 공존하는 시간 창(window) 동안의
  프로토콜 호환성은 실제 바이너리 실행 없이는 검증 불가.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M1.1 | **에이전트 롤링 업그레이드 — I/O 유지** | `fio --rw=randwrite` 실행 중 PVC 최소 4개; pillar-agent v_old DaemonSet 배포 완료 | 에이전트 업그레이드(`v_new`) 진행 중 I/O 오류율, 레이턴시 급등, PVC Read-Only 전환 여부 | I/O 오류 0%; 레이턴시 급등 최대 2배 이내; Pod 재시작 없음 | 1) `kubectl rollout restart daemonset/pillar-agent`; 2) `fio` 로그에서 `error` 라인 확인; 3) `kubectl get events --field-selector=reason=Failed` 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |
| M1.2 | **컨트롤러 롤링 업그레이드** | pillar-csi controller-manager deployment; 능동적 PVC 프로비저닝 요청 중(StorageClass로 연속 PVC 생성 스크립트) | 업그레이드 중 신규 PVC 프로비저닝 지연, 기존 PVC 액세스 중단, controller-manager 리더 선출 오류 | PVC 프로비저닝 지연 ≤ 60s; 기존 PVC I/O 무중단 | 1) `kubectl set image deployment/pillar-csi-controller-manager manager=example.com/pillar-csi:v_new`; 2) 신규 PVC 생성 스크립트 실행 유지; 3) `kubectl rollout status` 완료 후 미처리 PVC 수 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |
| M1.3 | **구버전 에이전트 + 신버전 컨트롤러 공존(혼합 버전)** | 스토리지 노드 A: v_old 에이전트, 스토리지 노드 B: v_new 에이전트; 컨트롤러: v_new | 혼합 버전 환경에서 볼륨 생성·삭제 정상 동작; API 프로토콜 하위 호환성 | 모든 볼륨 오퍼레이션 성공; gRPC 직렬화 오류 없음 | 1) 노드 A에 `v_old`, 노드 B에 `v_new` 에이전트 배포; 2) 두 노드에서 번갈아 볼륨 생성/삭제; 3) `kubectl logs` 에서 `Unimplemented`/`Unknown field` 오류 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |
| M1.4 | **롤백 시나리오 — 업그레이드 실패 후 이전 버전 복구** | v_new 에이전트에 의도적인 결함(예: 잘못된 imagePullPolicy); 에이전트 CrashLoopBackOff 상태 | 롤백(`kubectl rollout undo`) 후 기존 PVC 모두 접근 가능 상태 복구; 데이터 손실 없음 | 롤백 완료 후 모든 PVC Bound; I/O 재개 | 1) 결함 있는 버전 배포; 2) CrashLoopBackOff 확인; 3) `kubectl rollout undo daemonset/pillar-agent`; 4) `fio` I/O 재개 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |

---

### M2: 스토리지 네트워크 분리(Network Partition) 시나리오

**목적:** 스토리지 네트워크 장애(네트워크 분리, 패킷 손실, 지연)가 발생했을 때
pillar-csi가 올바른 오류를 반환하고, 네트워크 복구 후 I/O가 자동으로 재개되는지
검증한다.

**자동화 불가 이유:**

- `iptables` 또는 `tc netem` 으로 네트워크 장애를 주입할 수 있으나, 장애 중
  NVMe-oF 세션의 재연결 타이머, 커널 드라이버 동작, 상위 레이어 오류 전파의
  타이밍이 환경(커널 버전, NIC 드라이버)마다 달라 일관된 자동 검증이 어렵다.
- 네트워크 분리 중 Kubernetes Pod이 재시작될지 여부는 kubelet의
  `node-status-update-frequency`, `pod-eviction-timeout` 설정에 따라 달라진다.
- 복구 후 서비스 정상화까지의 허용 시간 정의는 운영 정책 결정 사항이다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M2.1 | **스토리지 네트워크 완전 차단 → 복구** | `fio` I/O 중 PVC 2개; 스토리지 노드 A의 스토리지 NIC에 `iptables -I OUTPUT -d <storage-net> -j DROP` 적용 | I/O 오류 반환 타이밍; Pod Eviction 여부; 네트워크 복구 후 I/O 재개 시간 | 30s 내 I/O 오류 반환; iptables 규칙 제거 후 120s 내 I/O 재개 | 1) `fio` 백그라운드 실행; 2) `iptables` 차단 규칙 적용; 3) `fio` 오류 발생 시간 기록; 4) iptables 규칙 제거; 5) I/O 재개까지 경과 시간 측정 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.2 | **패킷 손실 20% 주입** | `tc qdisc add dev <storage-nic> root netem loss 20%` | NVMe-oF 재전송으로 인한 레이턴시 증가; I/O 오류 발생 여부; ZFS 오류 카운터 증가 여부 | 레이턴시 ≤ 10배 증가 허용; I/O 오류 없음 (재전송으로 보완) | 1) `tc netem loss 20%` 적용; 2) `fio` 레이턴시 분포 수집 (p99, p999); 3) `nvme list`, `dmesg` 에서 재전송 카운터 확인; 4) 규칙 제거 후 레이턴시 정상화 확인 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.3 | **스토리지 노드 재부팅 중 I/O** | `fio` 연속 I/O 중 스토리지 노드 graceful reboot(`sudo reboot`) | 스토리지 노드 부팅 완료 후 PVC Bound 복구; I/O 재개; 데이터 무결성 | 부팅 완료 후 120s 내 PVC 복구; `fio` MD5 checksum 일치 | 1) `fio --verify=md5` 실행; 2) 스토리지 노드 재부팅; 3) 부팅 완료 후 `kubectl get pvc` 상태 확인; 4) `fio verify` 결과 검토 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.4 | **스토리지 네트워크 링크 플랩(Link Flap) — 10초 간격 반복** | 스토리지 NIC `ip link set down` → 5초 후 `ip link set up` × 5회 반복 | PVC 강제 Terminating 전환 없음; I/O 일시 중단 후 자동 재개; dmesg 커널 오류 없음 | PVC 강제 Terminating 전환 0; 링크 복구 후 I/O 재개 ≤ 30s | 1) 링크 플랩 스크립트 실행 (ip link set down 5초 후 up, 5회 반복); 2) `fio` 로그에서 오류 구간 기록; 3) dmesg 에서 `nvme` 관련 경고 확인 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.5 | **컨트롤 플레인 ↔ 스토리지 노드 통신 차단 (NVMe-oF는 유지)** | iptables로 컨트롤 플레인 → 스토리지 노드 API 차단; NVMe-oF 포트(4420)는 유지 | 기존 PVC I/O 유지; 신규 볼륨 프로비저닝 실패 오류 명확성; 컨트롤 플레인 복구 후 프로비저닝 재개 | 기존 I/O 100% 유지; 신규 프로비저닝에 명확한 오류 메시지 반환 | 1) 컨트롤 플레인 → 스토리지 노드 API 차단 (gRPC 포트 50051); 2) `fio` I/O 지속 확인; 3) 신규 PVC 생성 시도 및 오류 메시지 기록; 4) 차단 해제 후 보류 중 PVC 자동 프로비저닝 확인 | `Conn`, `NVMeF`, `Agent`, `gRPC` |

---

### M3: 물리적 디스크/하드웨어 장애 시뮬레이션

**목적:** ZFS 풀을 구성하는 물리 디스크의 장애(불량 블록, 전체 디스크 제거)가
발생했을 때 pillar-csi가 올바른 오류를 반환하고, ZFS RAIDZ/미러 구성에서
자동으로 복구되는지 검증한다.

**자동화 불가 이유:**

- 실제 물리 디스크 제거 또는 SCSI 오류 주입(`scsi_debug`, `dmsetup`)은
  CI 컨테이너 환경에서 지원되지 않으며, 실제 하드웨어가 필요하다.
- 디스크 교체 절차(ZFS `zpool replace`) 중 재실버링(resilvering) 시간은
  디스크 크기와 데이터 양에 따라 수십 분에서 수 시간이 소요된다.
- "허용 가능한 ZFS 오류 상태"(degraded vs. faulted) 판단은 운영 정책에 따라 다르다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M3.1 | **RAIDZ1 구성 중 디스크 1개 오프라인** | ZFS RAIDZ1 풀(디스크 3개); `fio` I/O 활성; pillar-csi PVC 2개 | 디스크 오프라인 후 ZFS `degraded` 상태; I/O 유지; SMART 경고; PVC 상태 `Bound` 유지 | ZFS `degraded` 반환; I/O 오류 없음; PVC `Bound` 유지; `zpool status` 에 경고 | 1) `zpool offline tank sdb`; 2) `fio` I/O 계속 확인; 3) `zpool status` 에서 `DEGRADED` 확인; 4) `kubectl get pvc` 상태 확인; 5) pillar-agent 로그에서 경고 확인 | `ZFS`, `NVMeF`, `Agent` |
| M3.2 | **디스크 교체 및 재실버링(Resilver) 중 I/O 유지** | M3.1 이후 상태; 교체용 디스크(`/dev/sdc`) 준비 | `zpool replace` 후 재실버링 진행 중 I/O 유지; 재실버링 완료 후 `ONLINE` 상태 | 재실버링 중 I/O 오류 없음; 재실버링 완료 후 `zpool status` = `ONLINE` | 1) `zpool replace tank sdb sdc`; 2) 재실버링 진행 중 `zpool status` 주기적 확인; 3) `fio` I/O 유지 확인; 4) 완료 후 `zpool status` = `ONLINE` 검증 | `ZFS`, `NVMeF`, `Agent` |
| M3.3 | **불량 블록(Bad Block) 시뮬레이션 — ZFS 스크럽** | ZFS 풀에 데이터 기록 후 `dd if=/dev/zero of=/dev/sdb bs=4k seek=1000 count=1` 로 데이터 손상 주입 | `zpool scrub` 실행 후 오류 감지; pillar-agent 상태 반영; `zfs status` 의 READ/WRITE/CKSUM 오류 카운터 증가 | `zpool scrub` 후 오류 카운터 > 0; pillar-agent가 해당 볼륨을 오류 상태로 표시 | 1) 정상 데이터 기록; 2) `dd`로 특정 섹터 손상; 3) `zpool scrub tank`; 4) `zpool status` 에서 CKSUM 오류 확인; 5) pillar-agent 로그에서 오류 전파 확인 | `ZFS`, `NVMeF`, `Agent` |
| M3.4 | **스토리지 노드 전원 강제 차단(Power Loss) 시뮬레이션** | `fio --rw=write` 활성 중; KVM 환경에서 `virsh destroy`로 갑작스러운 전원 차단 | 전원 복구 후 ZFS 자동 무결성 검사; 파일시스템 일관성 유지; PVC 재마운트 | ZFS import 후 `ONLINE`; `zfs list` 데이터 무결성; PVC `Bound` 복구 | 1) KVM: `virsh destroy <storage-vm>`; 2) 5초 후 `virsh start <storage-vm>`; 3) 부팅 후 `zpool import -f tank`; 4) `zpool status` 확인; 5) 데이터 검증 (`fio --verify=md5`) | `ZFS`, `NVMeF`, `Agent` |

---

### M4: 커널 버전 호환성 매트릭스

**목적:** pillar-csi 및 pillar-agent가 지원하는 Linux 커널 버전 범위에서 ZFS,
NVMe-oF 커널 모듈이 올바르게 동작하는지 검증한다.

**자동화 불가 이유:**

- 다수의 커널 버전(예: 5.15 LTS, 6.1 LTS, 6.6, 6.8)에 대한 테스트 환경을
  CI에서 매번 구성하는 비용이 매우 크다.
- 특정 커널 버전에서의 회귀(regression)는 자동 감지가 어렵고 수동 확인이 필요하다.
- ZFS와 nvme-tcp 커널 모듈의 상호작용은 커널 마이너 버전마다 다를 수 있다.
- 커널 업데이트 후 새로운 `/sys/kernel/config/nvmet/` 인터페이스 변경은
  수동 검증이 필요하다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M4.1 | **커널 5.15 LTS (Ubuntu 22.04 기본)** | Ubuntu 22.04 LTS 서버; `uname -r` = 5.15.x; ZFS 2.1.x; nvme-cli 2.x | 기본 볼륨 생성/삭제/마운트/언마운트 전체 흐름 | 모든 F1–F10 수준 테스트 통과 | 1) `modprobe zfs nvmet nvme-tcp`; 2) pillar-agent 실행; 3) 기본 볼륨 라이프사이클 수동 실행; 4) 커널 메시지(`dmesg`) 경고 없음 확인 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.2 | **커널 6.1 LTS** | Ubuntu 22.04 + HWE 커널 또는 Debian 12 | 동일 (M4.1 시나리오 반복) | M4.1과 동일 | M4.1과 동일 절차; `dmesg` 경고 비교 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.3 | **커널 6.8 (최신 안정 버전)** | Ubuntu 24.04 LTS | 동일 + nvmet configfs API 변경 여부 확인 | M4.1과 동일; configfs 경로 변경 없음 | M4.1과 동일 절차; `ls /sys/kernel/config/nvmet/` 구조 확인 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.4 | **ZFS 커널 모듈 버전 조합 검증** | Ubuntu 22.04; `apt install zfs-dkms=2.1.x` vs. `2.2.x` | 서로 다른 ZFS 버전에서 zvol 생성/삭제 정상 동작; pillar-agent 경고 없음 | zvol 오퍼레이션 모두 성공; zfs 버전 비호환 경고 없음 | 1) `apt install zfs-dkms=2.1.x`; 2) F1 수준 테스트 수동 실행; 3) `apt upgrade zfs-dkms`; 4) 동일 테스트 재실행; 5) 결과 비교 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.5 | **RHEL 9 / Rocky Linux 9 호환성** | RHEL 9 또는 Rocky Linux 9.x; ZFS on RHEL (DKMS 방식) | Ubuntu 이외 배포판에서 pillar-agent 기본 기능 동작 여부 | zvol 생성/삭제 성공; configfs 디렉터리 구조 동일 | 1) RHEL 9 환경 구성; 2) ZFS DKMS 설치; 3) `modprobe nvmet nvme-tcp`; 4) pillar-agent 컴파일 및 실행; 5) 기본 볼륨 라이프사이클 확인 | `ZFS`, `NVMeF`, `Conn`, `Agent` |

---

### M5: 프로덕션 유사 부하(Production-like Load) 및 용량 계획 테스트

**목적:** 실제 운영 환경과 유사한 워크로드 하에서 pillar-csi의 처리량, 레이턴시,
리소스 소비를 측정하고, 용량 계획(capacity planning)을 위한 기준 지표를 수집한다.

**자동화 불가 이유:**

- 성능 테스트 결과의 "통과/실패" 기준은 배포 환경(하드웨어 사양, 네트워크 구성)에
  따라 다르며, 일률적인 임계값 설정이 불가능하다.
- 측정 결과 해석(레이턴시 분포의 p99 증가가 허용 가능한지)은 전문가 판단이 필요하다.
- 장기 실행(수 시간 이상) 성능 테스트는 CI 파이프라인 제한 시간을 초과한다.
- 메모리 누수, 파일 디스크립터 누수 등의 리소스 소비 패턴은 장기 실행 후에만 관찰 가능하다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M5.1 | **100개 PVC 동시 프로비저닝 성능** | 스테이징 클러스터; 스토리지 노드 4개; ZFS 풀 각 1 TiB | 100개 PVC 생성 완료 시간; 컨트롤러 CPU/메모리; 에이전트 gRPC 레이턴시 p99 | 100개 PVC Bound: ≤ 5분; 컨트롤러 CPU ≤ 1코어; 에이전트 레이턴시 p99 ≤ 500ms | 1) `for i in {1..100}; do kubectl apply -f pvc-$i.yaml; done`; 2) `time kubectl wait --for=condition=Bound pvc --all --timeout=600s`; 3) Prometheus에서 컨트롤러/에이전트 메트릭 수집 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.2 | **지속 I/O 부하 — 4시간 안정성(Soak) 테스트** | 20개 PVC; 각 PVC에 `fio --rw=randrw --bs=4k --iodepth=16 --runtime=14400` | 4시간 동안 I/O 오류 없음; 에이전트 메모리 누수 없음; 파일 디스크립터 증가 없음 | I/O 오류 0; 메모리 증가 ≤ 50 MiB/4h; FD 증가 없음 | 1) 20개 PVC 생성 및 Pod 배포; 2) `fio` 백그라운드 실행 (4시간); 3) 1시간마다 `kubectl top pod`, `lsof -p <agent-pid>` 기록; 4) 완료 후 `fio` 결과 집계 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.3 | **500 GiB 대용량 볼륨 생성/삭제 사이클** | ZFS 풀 1 TiB 이상; PVC 용량 500 GiB | 500 GiB zvol 생성 시간; `zfs create` 출력; 삭제 완료 시간 | 생성 ≤ 30s; 삭제 ≤ 60s; ZFS 용량 즉시 반환 확인 | 1) 500 GiB PVC 생성 및 시간 기록; 2) `zfs list` 로 실제 할당 확인; 3) PVC 삭제 및 시간 기록; 4) `zfs list` 로 용량 반환 확인 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.4 | **볼륨 확장 중 I/O 유지 — 대용량 파일시스템** | `ext4` 포맷된 200 GiB PVC; DB 유사 I/O 패턴 실행 중 | 볼륨 확장(`resize2fs`) 중 I/O 오류 없음; 확장 후 파일시스템 크기 정확성 | I/O 오류 없음; `df -h` 크기 일치 ≤ 1 GiB 오차 | 1) 200 GiB PVC 생성 및 `ext4` 포맷; 2) `fio` I/O 실행; 3) PVC 확장 요청 (`kubectl patch pvc`); 4) `resize2fs` 완료 후 `df -h` 검증 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.5 | **다수 스토리지 노드 동시 에이전트 gRPC 호출 스트레스** | 스토리지 노드 4개; 각 노드에서 동시에 10개 볼륨 생성 요청 | 에이전트 gRPC 큐 처리; 컨트롤러 동시성 처리; 중복 CRD 생성 없음 | 40개 볼륨 모두 성공; 중복 PillarVolume CRD 없음; 데드락 없음 | 1) 4개 노드 × 10개 PVC 동시 생성 스크립트; 2) `kubectl get pillarvolume --all-namespaces` 40개 확인; 3) 에이전트 로그에서 뮤텍스 타임아웃 경고 확인 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |

---

### M6: 보안 감사 및 침투 테스트

**목적:** pillar-csi의 mTLS 통신, RBAC 권한, 컨테이너 보안 설정을
공격자 관점에서 검증한다.

**자동화 불가 이유:**

- 침투 테스트(penetration testing) 결과는 취약점 심각도 분류, 악용 가능성 판단,
  완화 방안 설계 등에 전문 보안 엔지니어의 판단이 필수적이다.
- 자동화 도구(예: `trivy`, `kube-bench`)는 알려진 취약점만 탐지하며, 로직 버그나
  설계 결함은 수동 검토가 필요하다.
- mTLS 우회 시도는 네트워크 캡처와 수동 분석이 필요하다.
- 보안 감사 범위 및 허용 기준은 보안 정책 문서를 기반으로 결정된다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M6.1 | **mTLS 클라이언트 인증서 미제시 → 연결 거부** | 스테이징 클러스터; pillar-csi 배포; cert-manager CA | CA 서명되지 않은 자체 서명 인증서로 에이전트 gRPC 연결 시도; 클라이언트 인증서 없이 연결 시도 | TLS handshake 실패; 연결 거부; gRPC 오류 | 1) `grpcurl -insecure -d @ <agent-endpoint> agent.AgentService/ListVolumes`; 2) 자체 서명 cert으로 연결 시도; 3) 양쪽 모두 `TLS handshake failed` 확인 | `mTLS`, `Agent`, `gRPC` |
| M6.2 | **RBAC 최소 권한 검증** | 스테이징 클러스터; pillar-csi RBAC 설정 배포 | `kube-bench`, `kubectl auth can-i` 로 controller-manager ServiceAccount의 불필요한 권한 없음 확인; ClusterRole 범위 최소화 | `kube-bench` 경고 없음; SA에 `get/list/watch/update` 이외 권한 없음 | 1) `kubectl auth can-i --list --as=system:serviceaccount:pillar-csi-system:pillar-csi-controller-manager`; 2) `create pod`, `delete node` 등 불필요한 권한 부재 확인; 3) 결과 문서화 | `mTLS`, `Agent`, `gRPC` |
| M6.3 | **컨테이너 보안 컨텍스트 검증** | pillar-csi controller-manager/agent Pod 실행 중 | `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation=false` 설정 확인; `trivy` 취약점 스캔 결과 검토 | 모든 설정 올바름; `trivy` HIGH/CRITICAL 취약점 0개 | 1) `kubectl get pod -o jsonpath='{.spec.containers[*].securityContext}'`; 2) `trivy image example.com/pillar-csi:v_current`; 3) HIGH 이상 취약점 리포트 검토 | `mTLS`, `Agent`, `gRPC` |
| M6.4 | **네트워크 정책(NetworkPolicy) 우회 시도** | 스테이징 클러스터; Calico 또는 Cilium NetworkPolicy 배포; pillar-csi 네임스페이스 격리 정책 | 다른 네임스페이스 Pod에서 pillar-csi 에이전트 gRPC 포트(50051)로 직접 접근 차단 확인 | 네임스페이스 외부에서 50051 포트 접근 거부; NetworkPolicy 로그에 차단 기록 | 1) 별도 네임스페이스에서 `nc -z <agent-svc> 50051`; 2) NetworkPolicy 적용 전후 비교; 3) Calico 감사 로그에서 차단 확인 | `mTLS`, `Agent`, `gRPC` |

---

### M7: 데이터 무결성 심층 검증

**목적:** pillar-csi를 통해 기록된 데이터가 에이전트 재시작, 클러스터 재시작,
네트워크 장애, 볼륨 확장 등 다양한 조건 하에서도 손상 없이 보존되는지 검증한다.

**자동화 불가 이유:**

- 데이터 무결성 검증은 실제 블록 I/O(NVMe-oF) + 실제 파일시스템(ext4/xfs)이 필요하다.
- MD5/SHA256 체크섬 계산에 수 GB 데이터 기록 및 검증 시간(수십 분)이 소요된다.
- 파일시스템 수준 무결성(fsck, xfs_repair) 실행은 파일시스템 언마운트가 필요하다.
- 데이터 손상 패턴(비트 플립, 부분 쓰기) 분석은 전문 지식이 필요하다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M7.1 | **10 GiB 랜덤 데이터 기록 후 재마운트 검증** | ext4 PVC 20 GiB; `fio --verify=md5 --size=10g` | 언마운트 → 재마운트 후 MD5 체크섬 일치 | MD5 불일치 0건 | 1) `fio --filename=/mnt/pvc/test --rw=write --bs=4k --size=10g --verify=md5`; 2) Pod 재시작(재마운트); 3) `fio --verify-only` 실행; 4) 오류 수 확인 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.2 | **볼륨 확장 전후 데이터 무결성** | ext4 PVC 10 GiB; 5 GiB 데이터 기록 후 20 GiB 로 확장 | 확장 후 `fsck.ext4`, MD5 체크섬 일치 | `fsck` 오류 0; MD5 일치 | 1) 5 GiB 데이터 기록 및 체크섬 저장; 2) PVC 확장(20 GiB); 3) `resize2fs` 완료 확인; 4) `fsck.ext4 -n /dev/nvme*`; 5) 체크섬 재검증 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.3 | **ZFS 스냅샷 → 복원 후 데이터 일치** | ZFS 기반 PVC; 알려진 데이터셋 기록 | ZFS 스냅샷 생성 후 일부 데이터 수정; 스냅샷 복원 후 원본 데이터 복구 | 복원 후 원본 데이터 100% 일치 | 1) 알려진 데이터 기록 및 체크섬 저장; 2) `zfs snapshot tank/pvc-xxx@snap1`; 3) 데이터 수정; 4) `zfs rollback tank/pvc-xxx@snap1`; 5) 체크섬 재검증 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.4 | **크로스-노드 볼륨 마이그레이션 데이터 무결성** | 2개 스토리지 노드; ZFS send/receive | 노드 A → 노드 B 마이그레이션 후 데이터 체크섬 일치; NQN 변경 없음 | 체크섬 일치; NQN 정상 갱신 | 1) 노드 A에서 PVC 생성 및 10 GiB 데이터 기록; 2) `zfs send/receive` 로 노드 B 전송; 3) pillar-csi PVC 마이그레이션 API 호출; 4) 노드 B에서 재마운트 후 체크섬 검증 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.5 | **XFS 파일시스템 무결성 — I/O 중 에이전트 재시작** | xfs PVC; `fio --rw=randwrite` 진행 중 pillar-agent `kill -9` | 에이전트 재시작 후 XFS 파일시스템 `xfs_repair -n` 경고 없음; I/O 재개 | `xfs_repair -n` 수정 필요 항목 없음; I/O 재개 | 1) `fio --rw=randwrite` 실행; 2) `kill -9 <pillar-agent-pid>`; 3) 에이전트 자동 재시작 대기; 4) Pod 재시작 후 `xfs_repair -n /dev/nvme*` 실행; 5) 결과 기록 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |

---

### M8: CSI 드라이버 업그레이드 절차 검증

**목적:** pillar-csi의 마이너/패치 버전 업그레이드 절차(Helm 차트 업그레이드 또는
`kubectl apply`) 가 올바르게 실행되고, 업그레이드 전후 PVC 접근성이 유지되며,
다운그레이드 절차가 문서화된 대로 동작하는지 검증한다.

**자동화 불가 이유:**

- Helm 업그레이드 중 webhook 서버 재시작 타이밍에 따른 가용성 창(window)은
  환경마다 달라 자동 검증이 어렵다.
- 업그레이드 실패 시 롤백 절차의 정확성은 인간이 확인해야 한다.
- CRD 스키마 변경이 포함된 업그레이드는 데이터 마이그레이션 여부를 수동으로 판단해야 한다.
- 업그레이드 시 발생하는 API 서버 요청 급증(CRD Watch reconnect) 영향 분석은
  수동 모니터링이 필요하다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M8.1 | **Helm 차트 마이너 업그레이드 (v0.x → v0.x+1)** | v_old Helm 릴리스 배포; 기존 PVC 4개 Bound; `fio` I/O 활성 | 업그레이드 중 PVC I/O 중단 시간; webhook 재시작 후 신규 PVC 프로비저닝 정상화 시간; 기존 CRD 데이터 보존 | I/O 중단 ≤ 30s; 신규 PVC 프로비저닝 ≤ 60s; PillarVolume CRD 데이터 손실 없음 | 1) `helm upgrade pillar-csi ./charts/pillar-csi --set image.tag=v_new`; 2) 업그레이드 중 `fio` I/O 로그 기록; 3) 업그레이드 완료 후 `kubectl get pv,pvc` 상태 검증 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |
| M8.2 | **CRD 스키마 변경이 포함된 업그레이드** | v_old CRD에 새 선택적 필드 추가된 v_new | 기존 PillarVolume CRD 인스턴스에 새 필드 기본값 적용 여부; 기존 Controller 코드와 신규 CRD 스키마 호환성 | 기존 CRD 인스턴스 유지; 새 필드 기본값(`nil`/0) 올바름; get/list 오류 없음 | 1) v_new CRD YAML 적용 (`kubectl apply -f crds/`); 2) 기존 PillarVolume 인스턴스 `kubectl get` 확인; 3) 신규 필드 기본값 확인; 4) controller-manager 로그에서 스키마 오류 없음 확인 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |
| M8.3 | **다운그레이드 절차 — 긴급 롤백** | v_new 배포 상태에서 문제 발견 후 v_old 로 롤백 | `helm rollback` 후 PVC I/O 복구; 다운그레이드된 컨트롤러가 v_new CRD 인스턴스 처리 가능 여부 | PVC I/O 복구 ≤ 60s; CRD 호환성 오류 없음 | 1) `helm rollback pillar-csi`; 2) `kubectl rollout status`; 3) 기존 PVC I/O 재개 확인; 4) 컨트롤러 로그에서 `unknown field` 오류 없음 확인 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |
| M8.4 | **업그레이드 후 전체 E2E 회귀 테스트 (Smoke Test)** | M8.1 업그레이드 완료 후 | CreateVolume, DeleteVolume, NodeStage, NodePublish, NodeUnstage, NodeUnpublish 기본 흐름 | 모든 기본 오퍼레이션 성공; 에러 없음 | 1) 임시 PVC 생성/삭제 수동 실행; 2) 노드 스테이징/마운트/언마운트/언스테이징 수동 확인; 3) `kubectl get events` 에서 Warning 없음 확인 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |

---

### M9: 다중 테넌트 격리 검증

**목적:** 서로 다른 Kubernetes 네임스페이스/ServiceAccount를 사용하는 테넌트가
각자의 PVC만 접근 가능하고, 다른 테넌트의 볼륨에 접근할 수 없는지 검증한다.

**자동화 불가 이유:**

- 다중 테넌트 시나리오는 실제 사용자 자격 증명(ServiceAccount 토큰, OIDC)과
  실제 Kubernetes RBAC 정책이 필요하며, fake client로는 RBAC 검증이 불가능하다.
- 테넌트 격리 확인에는 보안 전문가의 수동 리뷰가 필요하다.
- NVMe-oF NQN 수준의 호스트 격리(AllowInitiator 정책)가 올바르게 적용되는지는
  실제 NVMe initiator로만 검증 가능하다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M9.1 | **테넌트 A의 PVC에 테넌트 B 접근 불가 (RBAC)** | 네임스페이스 `tenant-a`, `tenant-b` 각각 생성; RBAC PVC 네임스페이스 격리 정책 | `tenant-b` ServiceAccount 토큰으로 `tenant-a` PVC에 대한 `kubectl get/delete pvc` 시도 거부 | 403 Forbidden 반환; 접근 차단 | 1) `kubectl auth can-i get pvc -n tenant-a --as=system:serviceaccount:tenant-b:default`; 2) `No` 응답 확인 | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |
| M9.2 | **NVMe-oF NQN 수준 호스트 격리 — 다른 NQN으로 볼륨 접근 불가** | 볼륨 A: AllowInitiator NQN = `nqn.node-a`; 볼륨 B: AllowInitiator NQN = `nqn.node-b` | node-b NQN으로 볼륨 A에 `nvme connect` 시도; 연결 거부 확인 | `nvme connect` 실패; `dmesg` 에서 거부 로그 확인 | 1) node-b에서 `nvme connect -t tcp -n nqn.node-a-volume-A ...`; 2) 연결 실패 확인; 3) dmesg에서 거부 로그 확인 | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |
| M9.3 | **StorageClass 테넌트 격리 — 잘못된 StorageClass 사용 거부** | 각 테넌트별 StorageClass (네임스페이스 범위); RBAC로 타 네임스페이스 StorageClass 사용 차단 | 테넌트 A가 테넌트 B의 StorageClass를 사용한 PVC 생성 거부 확인 | PVC Pending 또는 403 오류 반환 | 1) `kubectl apply -f pvc-with-wrong-storageclass.yaml -n tenant-a`; 2) PVC 상태 확인 (`Pending` 및 오류 이벤트) | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |
| M9.4 | **PillarTarget 접근 제어 — 테넌트별 스토리지 노드 격리** | PillarTarget A: 테넌트 A 전용 스토리지 노드; RBAC로 타 테넌트가 해당 Target 참조 불가 | 테넌트 B의 PVC 생성 요청이 테넌트 A의 PillarTarget을 참조할 수 없음 | PVC 생성 실패; `NotFound` 또는 `Forbidden` 오류 | 1) 테넌트 B로 테넌트 A의 `PillarTarget`을 target 파라미터로 PVC 생성 시도; 2) 오류 메시지 확인 | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |

---

### M10: 인증서 수명 주기 및 실제 PKI 갱신 테스트

**목적:** pillar-csi가 사용하는 mTLS 인증서가 실제 만료 주기에 따라 갱신되고,
인증서 갱신 중 gRPC 연결이 자동으로 재연결(reconnect)되는지 검증한다.

**자동화 불가 이유:**

- 인증서 TTL을 인위적으로 단축(예: 1분)하면 cert-manager 동작이 실제 환경과
  달라질 수 있으며, 갱신 타이밍이 비결정적이다.
- cert-manager 갱신 사이클(인증서 만료 30일 전 갱신 시도)을 완전히 시뮬레이션하려면
  실제 시간 경과 또는 클럭 조작이 필요하다.
- 인증서 갱신 중 기존 gRPC 연결의 재연결 동작은 gRPC keepalive 설정, TLS session
  resumption 정책에 따라 달라지며, 실제 환경에서만 신뢰할 수 있게 검증된다.
- 루트 CA 교체(CA Rotation) 시나리오는 잘못 실행하면 클러스터 전체 mTLS 통신이
  차단될 수 있어 수동 제어가 필수적이다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M10.1 | **단기 TTL 인증서(24시간) 갱신 사이클** | cert-manager + Issuer; `spec.duration: 24h`, `spec.renewBefore: 8h` | 인증서 갱신 시 gRPC 세션 유지; 갱신 후 신규 cert로 핸드셰이크 성공 | gRPC 세션 중단 없음; 갱신 후 새 인증서 적용 | 1) 24h TTL Certificate 리소스 배포; 2) `kubectl get certificate -w` 로 갱신 이벤트 관찰; 3) 갱신 시점에 gRPC keepalive 연결 유지 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.2 | **인증서 수동 교체(Manual Rotation) — 다운타임 없음** | 기존 mTLS 인증서 활성 중; 새 인증서로 수동 교체 | `kubectl delete secret pillar-agent-mtls-cert` → cert-manager 재발급 → 에이전트/컨트롤러 재로드 | 인증서 재발급 중 gRPC 연결 유지; 재로드 후 新 인증서로 연결 성공 | 1) `kubectl delete secret -n pillar-csi-system pillar-agent-mtls-cert`; 2) cert-manager 재발급 이벤트 확인; 3) pillar-csi 컨트롤러 재시작 후 에이전트 연결 재설정 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.3 | **루트 CA 교체(CA Rotation) — 완전한 PKI 재발급** | 기존 CA + 인증서 체인; 새 CA 생성 준비 | 새 CA를 번들에 추가 → 모든 리프 인증서 갱신 → 구 CA 제거 단계별 진행 | 각 단계에서 gRPC 연결 유지; CA 제거 후 구 인증서로 연결 거부 | 1) 새 CA ClusterIssuer 추가; 2) 모든 Certificate 리소스 갱신; 3) 각 단계에서 gRPC 연결 테스트; 4) 구 CA 제거 후 구 인증서로 연결 시도 → 거부 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.4 | **인증서 만료(Expiry) — 갱신 실패 시 동작** | cert-manager `ClusterIssuer` 비활성화(Issuer 삭제); TTL이 임박한 인증서(1시간 미만 남음) | 인증서 만료 후 gRPC 연결 거부; 명확한 오류 메시지; cert-manager 복구 후 자동 재연결 | 만료 후 gRPC `UNAVAILABLE` 반환; Issuer 복구 후 120s 내 재연결 | 1) ClusterIssuer 비활성화; 2) 인증서 만료 대기 또는 `notAfter` 조작; 3) gRPC 연결 시도 → 오류 확인; 4) Issuer 복구; 5) 재연결 자동화 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.5 | **SAN(Subject Alternative Name) 불일치 — 연결 거부** | 잘못된 SAN을 가진 인증서(실제 에이전트 주소 불일치) | TLS SAN 검증 실패로 gRPC 연결 거부; 오류 로그에 SAN 불일치 원인 명시 | `x509: certificate is valid for X, not Y` 오류; 연결 거부 | 1) 잘못된 SAN 인증서 수동 생성; 2) pillar-csi 컨트롤러에 적용; 3) 에이전트 연결 시도 → 오류 확인; 4) 오류 메시지에 원인 명시 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.6 | **Webhook TLS 인증서 갱신 중 CRD 어드미션 가용성** | cert-manager가 webhook-server-cert 갱신 중; kube-apiserver의 caBundle 업데이트 지연 | 갱신 중 `kubectl apply` 의 webhook timeout/error 발생 여부; 갱신 완료 후 정상화 시간 | 갱신 중 임시 오류 허용 (≤ 30s); 완료 후 webhook 정상 동작 | 1) webhook-server-cert 수동 삭제(재발급 유도); 2) `kubectl apply -f pillar-target.yaml` 반복 실행; 3) 오류 발생 구간 시간 기록; 4) 정상화 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.7 | **cert-manager 완전 장애 — 기존 인증서 활용 유지** | cert-manager 네임스페이스 전체 삭제; 기존 발급된 인증서 Secret 보존 | cert-manager 없이 기존 인증서로 gRPC 연결 유지 가능 기간 확인; cert-manager 복구 후 갱신 자동 재개 | cert-manager 장애 중 기존 인증서 유효 기간 동안 gRPC 유지; 복구 후 자동 갱신 재개 | 1) `kubectl delete namespace cert-manager`; 2) gRPC 연결 유지 확인; 3) `kubectl apply -f cert-manager.yaml` 재설치; 4) 자동 갱신 재개 확인 | `mTLS`, `gRPC`, `TgtCRD` |

---

### 유형 M 커버리지 매트릭스

유형 A/F와 유형 M이 담당하는 검증 영역을 비교한다.

| 검증 영역 | 유형 A (in-process) | 유형 F (자동화 하드웨어) | 유형 M (수동/스테이징) |
|---------|:-------------------:|:---------------------:|:--------------------:|
| API 계층 오류 처리 | ✅ (mock 주입) | ✅ (실제 명령 오류) | — |
| 실제 커널 모듈 동작 | ❌ | ✅ | — |
| 롤링 업그레이드 가용성 | ❌ | ❌ | ✅ M1 |
| 네트워크 분리 복구 | ❌ | ❌ | ✅ M2 |
| 물리 디스크 장애/교체 | ❌ | ❌ | ✅ M3 |
| 다중 커널 버전 호환성 | ❌ | ❌ (단일 환경) | ✅ M4 |
| 프로덕션 부하 성능 기준선 | ❌ | ❌ (단기 실행) | ✅ M5 |
| 보안 감사/침투 테스트 | ❌ | ❌ | ✅ M6 |
| 장기 데이터 무결성 | ❌ | ❌ (단기 I/O) | ✅ M7 |
| 업그레이드/다운그레이드 절차 | ❌ | ❌ | ✅ M8 |
| 다중 테넌트 격리 | ❌ | ❌ | ✅ M9 |
| 실제 인증서 TTL 갱신 사이클 | ❌ (testcerts) | ❌ (testcerts) | ✅ M10 |
| 자동 실행 가능 | ✅ | ✅ (self-hosted) | ❌ (수동만) |
| CI 통합 | ✅ | 일부 가능 | ❌ |

---

### 유형 M 자동화 전환 가능 항목

아래 항목은 적절한 인프라 확보 및 허용 기준 합의 시 유형 F(자동화)로
전환 가능하다. 단, 현재 상태에서는 수동 실행을 유지한다.

| 항목 | 자동화 전환 조건 | 전환 후 테스트 ID |
|------|---------------|----------------|
| M1.1 (에이전트 롤링 업그레이드 I/O 유지) | 허용 가능한 I/O 중단 기준 합의 + 자동화 KPI 정의 | `TestRollingUpgrade_AgentDaemonSet` |
| M2.1 (네트워크 완전 차단 복구) | Chaos Mesh 또는 `tc netem` 기반 카오스 인젝터 도입 + 타임아웃 기준 합의 | `TestNetworkPartition_Recovery` |
| M4.1–M4.3 (커널 버전 매트릭스) | GitHub Actions 매트릭스 전략 + 커널별 self-hosted 러너 등록 | `TestKernelCompat_5_15`, `TestKernelCompat_6_1` |
| M5.1 (100개 PVC 동시 생성) | 성능 기준선(baseline) 정의 + 자동 임계값 비교 | F25 (`TestScalability_100PVC`) |
| M7.1 (MD5 체크섬 검증) | `fio --verify=md5` 실행 자동화 + 결과 파싱 스크립트 | `TestDataIntegrity_MD5` |
| M10.1 (단기 TTL 인증서 갱신) | cert-manager 단기 TTL 인증서 + 자동 재연결 검증 타임아웃 정의 | F7 확장 (`TestMTLS_CertRenewal`) |

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

# E3: Node 전체 (E3.1–E3.20 포함)
go test ./test/e2e/ -v -run TestCSINode

# E3.11: 파일시스템 타입별 동작
go test ./test/e2e/ -v -run TestCSINode_StageVolume

# E3.12: NVMe-oF Attach 파라미터 검증
go test ./test/e2e/ -v -run TestCSINode_StageVolume_Connect

# E3.13: NodeUnstage/Detach 시나리오
go test ./test/e2e/ -v -run TestCSINode_UnstageVolume

# E3.14–E3.15: NodePublish 다중 타깃 및 접근 모드
go test ./test/e2e/ -v -run TestCSINode_PublishVolume

# E3.16: 디바이스 대기 및 GetDevicePath 유효성 검사
go test ./internal/csi/ -v -run TestNodeStageVolume_Device
go test ./test/component/ -v -run TestCSINode_NodeStageVolume_Device

# E3.17: 기본 파일시스템 타입 및 접근 유형 검증
go test ./internal/csi/ -v -run TestNodeStageVolume_DefaultFsType
go test ./internal/csi/ -v -run TestNodeStageVolume_NoAccessType

# E3.18: 재부팅 후 재스테이징 멱등성
go test ./internal/csi/ -v -run TestNodeStageVolume_IdempotentAfterUnmount
go test ./test/component/ -v -run TestCSINode_NodeStage_Idempotent_StateFileExists

# E3.19: NodeUnstageVolume 오류 경로 심화
go test ./internal/csi/ -v -run TestNodeUnstageVolume

# E3.20: 스테이징 상태 파일 관리
go test ./internal/csi/ -v -run TestStageState
go test ./test/component/ -v -run TestCSINode_NodeUnstage_CorruptStateFile
go test ./test/component/ -v -run TestCSINode_NodeStage_StateDirUnwritable
go test ./test/component/ -v -run TestCSINode_NodeUnstage_StateFileMissingIsOK

# E4+E5: Lifecycle + Ordering
go test ./test/e2e/ -v -run TestCSILifecycle
go test ./test/e2e/ -v -run TestCSIOrdering

# E6: Partial Failure (E6.1/E6.2 - basic CRD persistence)
go test ./test/e2e/ -v -run TestCSIController_PartialFailure
go test ./test/e2e/ -v -run TestCSIController_DeleteVolume_CleansUpCRD

# E6.3: zvol No-Duplication (skipBackend optimisation)
go test ./test/e2e/ -v -run TestCSIZvolNoDup

# E24: Full Lifecycle Integration (failure/recovery scenarios)
go test ./test/e2e/ -v -run "TestCSILifecycle|TestCSIOrdering|TestCSIController_PartialFailure|TestCSIZvolNoDup"

# E7: Publish Idempotency
go test ./test/e2e/ -v -run TestCSIPublishIdempotency

# E8: mTLS
go test ./test/e2e/ -v -run TestMTLS

# E9: Agent gRPC E2E
go test ./test/e2e/ -v -run TestAgent

# E11: 볼륨 확장 통합 (ControllerExpand + NodeExpand)
go test ./test/e2e/ -v -run TestCSIExpand

# E12: CSI 스냅샷 미구현 검증
go test ./test/e2e/ -v -run TestCSISnapshot

# E13: 볼륨 클론 미처리 동작 검증
go test ./test/e2e/ -v -run TestCSIClone

# E14: 잘못된 입력값 및 엣지 케이스
go test ./test/e2e/ -v -run TestCSIEdge

# E15: 리소스 고갈 오류 전파
go test ./test/e2e/ -v -run TestCSIExhaustion

# E16: 동시 작업 안전성
go test ./test/e2e/ -v -run TestCSIConcurrent

# E17: 정리 검증
go test ./test/e2e/ -v -run TestCSICleanup

# E14–E17 전체 (오류/엣지/동시/정리)
go test ./test/e2e/ -v -run "TestCSIEdge|TestCSIExhaustion|TestCSIConcurrent|TestCSICleanup"

# E21.1: 잘못된 CR 런타임 처리 (in-process)
go test ./test/e2e/ -v -run TestCSIInvalidCR
```

### E21 envtest 통합 테스트 실행 (make setup-envtest 필요)

```bash
# 사전 준비: envtest 바이너리 설치
make setup-envtest

# E21.2: PillarTarget 웹훅 — 불변 필드 수정 거부
go test -tags=integration ./internal/webhook/v1alpha1/ -v -run PillarTarget

# E21.3: PillarPool 웹훅 — 불변 필드 수정 거부
go test -tags=integration ./internal/webhook/v1alpha1/ -v -run PillarPool

# E21.2 + E21.3 전체 웹훅 테스트
go test -tags=integration ./internal/webhook/v1alpha1/ -v

# E21.4: CRD OpenAPI 스키마 검증 (controller suite)
go test -tags=integration ./internal/controller/ -v -run TestCRDSchema

# E21 전체 (E21.2–E21.4) — integration 빌드
go test -tags=integration ./internal/... -v -run "PillarTarget|PillarPool|TestCRDSchema"
```

### 클러스터 E2E 테스트 실행 (Kind 필요)

```bash
# Kind 클러스터 사전 준비 필요
make docker-build IMG=example.com/pillar-csi:v0.0.1
kind load docker-image example.com/pillar-csi:v0.0.1

go test ./test/e2e/ -tags=e2e -v -timeout 600s
```

---

## 부록: CI 환경에서 루프백 장치를 이용한 ZFS 스토리지 모킹

### 개요

pillar-csi의 `유형 A` 인프로세스 E2E 테스트는 실제 ZFS 커널 모듈 없이
mock 백엔드를 사용한다. 그러나 F1–F3, F8–F12, F17–F25와 같이 **실제 ZFS
zvol**이 필요한 테스트를 CI에서 실행하려면 루프백(loopback) 장치로
ZFS 풀을 흉내 낼 수 있다.

**루프백 ZFS 접근 방식의 핵심 원리:**

```
일반 파일 (예: /tmp/zfs-pool.img, 1GiB)
        │
  losetup /dev/loop0    ← 루프백 블록 장치로 노출
        │
  zpool create tank /dev/loop0   ← ZFS 풀 생성
        │
  zfs create -V 100M tank/pvc-test   ← zvol 생성
        │
  /dev/zvol/tank/pvc-test   ← 블록 디바이스 사용 가능
```

---

### CI 실행 가능 여부 판단 기준

| 조건 | 설명 | 표준 CI 가능 여부 |
|------|------|:----------------:|
| 루프백 장치만 필요 (`losetup`) | 루트 권한 또는 `CAP_SYS_ADMIN` 필요 | ⚠️ 조건부 가능 |
| ZFS 커널 모듈 (`zfs.ko`) | Ubuntu 22.04+ 기본 제공 (`linux-modules-extra-*`) | ⚠️ 조건부 가능 |
| NVMe-oF target 커널 모듈 (`nvmet`, `nvmet-tcp`) | `modprobe nvmet nvmet-tcp` | ❌ 대부분 CI 불가 |
| NVMe-oF initiator 커널 모듈 (`nvme-tcp`) | `modprobe nvme-tcp` | ❌ 대부분 CI 불가 |
| `/sys/kernel/config/nvmet/` 쓰기 권한 | configfs 마운트 + root | ❌ 대부분 CI 불가 |
| Kind 클러스터 내부 | Docker-in-Docker + 루프백 충돌 위험 | ❌ 표준 CI 불가 |

**권장:** 루프백 ZFS 테스트는 **전용 self-hosted 러너** (베어메탈 Linux 또는
KVM/QEMU 네스티드 가상화 지원 VM)에서만 실행한다.
GitHub Actions 기본 러너(`ubuntu-latest`)는 `CAP_SYS_ADMIN` 없이
루프백 장치를 생성할 수 없다.

---

### 사전 요구사항

```
운영체제: Ubuntu 22.04 LTS 이상 (또는 동등한 Linux 배포판)
커널:     5.15 이상 (ZFS 2.x 지원)
패키지:   zfsutils-linux, zfs-dkms 또는 linux-modules-extra-$(uname -r)
권한:     root 또는 sudo, CAP_SYS_ADMIN
```

**패키지 설치:**

```bash
# Ubuntu 22.04 / 24.04
sudo apt-get update
sudo apt-get install -y zfsutils-linux

# ZFS 커널 모듈 로드 확인
sudo modprobe zfs
lsmod | grep zfs
# 출력 예시:
# zfs                  4100096  0
# spl                   131072  1 zfs
```

---

### 루프백 ZFS 풀 생성 절차

아래 순서대로 실행하면 실제 하드웨어 없이 ZFS 풀을 생성할 수 있다.

#### 1단계: 이미지 파일 생성

```bash
# 1GiB 이미지 파일 생성 (테스트용; 크기는 필요에 따라 조정)
sudo mkdir -p /tmp/pillar-csi-test
sudo dd if=/dev/zero of=/tmp/pillar-csi-test/zfs-pool.img \
    bs=1M count=1024 status=progress
# 또는 sparse 파일로 빠르게 생성
sudo truncate -s 1G /tmp/pillar-csi-test/zfs-pool.img
```

#### 2단계: 루프백 장치 연결

```bash
# 루프백 장치 생성 및 이미지 연결
LOOP_DEV=$(sudo losetup --find --show /tmp/pillar-csi-test/zfs-pool.img)
echo "루프백 장치: ${LOOP_DEV}"
# 출력 예시: /dev/loop0

# 연결된 루프백 장치 확인
sudo losetup -l | grep zfs-pool
```

#### 3단계: ZFS 풀 생성

```bash
# 루프백 장치로 ZFS 풀 생성 (이름: tank)
# ashift=9 는 512B 섹터 크기를 명시 (루프백은 가상 장치이므로 명시 필요)
sudo zpool create -f \
    -o ashift=9 \
    -O compression=off \
    -O atime=off \
    tank "${LOOP_DEV}"

# 풀 상태 확인
sudo zpool status tank
# 예상 출력:
#   pool: tank
#  state: ONLINE
# config:
#         NAME        STATE     READ WRITE CKSUM
#         tank        ONLINE       0     0     0
#           loop0     ONLINE       0     0     0
```

#### 4단계: ZFS zvol 생성 (실제 테스트용)

```bash
# 100MiB zvol 생성 (pillar-csi backend가 생성하는 것과 동일한 타입)
sudo zfs create -V 100M tank/pvc-test

# zvol 블록 장치 경로 확인
ls -la /dev/zvol/tank/pvc-test
# 출력 예시: lrwxrwxrwx ... /dev/zvol/tank/pvc-test -> ../../zd0

# 실제 장치 경로 (pillar-csi agent가 사용하는 경로)
readlink -f /dev/zvol/tank/pvc-test
# 출력 예시: /dev/zd0
```

---

### 루프백 ZFS 환경에서 pillar-agent 단위 테스트 실행

루프백 ZFS 풀이 준비되면, `internal/agent/backend/zfs/` 의 ZFS 백엔드
테스트를 실제 ZFS와 함께 실행할 수 있다.

```bash
# ZFS_TEST_POOL 환경변수로 실제 풀 이름 전달 (테스트 코드가 지원하는 경우)
ZFS_TEST_POOL=tank \
    sudo -E go test ./internal/agent/backend/zfs/ -v -tags=realzfs \
    -timeout 60s
```

**⚠️ 중요:** 현재 `internal/agent/backend/zfs/` 테스트는
`seqExec` 가짜 executor를 사용하여 실제 `zfs(8)` 명령을 실행하지 않는다.
루프백 ZFS와 함께 실제 명령을 실행하려면 별도의 `//go:build realzfs`
빌드 태그를 추가한 통합 테스트가 필요하다 (현재 미구현; F1 참조).

---

### CI 파이프라인 통합 예시 (GitHub Actions — self-hosted 러너)

```yaml
# .github/workflows/realzfs-e2e.yml
name: Real ZFS E2E (self-hosted)

on:
  push:
    branches: [main]
  workflow_dispatch:

jobs:
  realzfs-e2e:
    runs-on: [self-hosted, linux, zfs]   # ZFS 지원 self-hosted 러너 필요
    timeout-minutes: 30

    steps:
      - uses: actions/checkout@v4

      - name: ZFS 커널 모듈 확인
        run: |
          sudo modprobe zfs
          lsmod | grep zfs

      - name: 루프백 ZFS 풀 준비
        run: |
          sudo truncate -s 1G /tmp/pillar-csi-test.img
          LOOP_DEV=$(sudo losetup --find --show /tmp/pillar-csi-test.img)
          echo "LOOP_DEV=${LOOP_DEV}" >> $GITHUB_ENV
          sudo zpool create -f -o ashift=9 tank "${LOOP_DEV}"
          echo "ZFS_TEST_POOL=tank" >> $GITHUB_ENV

      - name: Go 설치
        uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: 실제 ZFS E2E 테스트 실행
        run: |
          sudo -E go test ./internal/agent/backend/zfs/ -v \
            -tags=realzfs \
            -timeout 120s
        env:
          ZFS_TEST_POOL: ${{ env.ZFS_TEST_POOL }}

      - name: 루프백 ZFS 풀 정리
        if: always()
        run: |
          sudo zpool destroy -f tank 2>/dev/null || true
          sudo losetup -d "${LOOP_DEV}" 2>/dev/null || true
          sudo rm -f /tmp/pillar-csi-test.img
```

---

### GitLab CI 통합 예시 (privileged 러너)

```yaml
# .gitlab-ci.yml (realzfs 스테이지)
realzfs-e2e:
  stage: realzfs
  tags:
    - privileged   # CAP_SYS_ADMIN 필요
    - linux
  image: ubuntu:22.04
  before_script:
    - apt-get update -qq && apt-get install -y -qq zfsutils-linux golang-go
    - modprobe zfs || true
    - truncate -s 1G /tmp/zfs-pool.img
    - LOOP_DEV=$(losetup --find --show /tmp/zfs-pool.img)
    - zpool create -f -o ashift=9 tank "${LOOP_DEV}"
    - echo "LOOP_DEV=${LOOP_DEV}" > /tmp/zfs-env.sh
  script:
    - export ZFS_TEST_POOL=tank
    - go test ./internal/agent/backend/zfs/ -v -tags=realzfs -timeout 120s
  after_script:
    - zpool destroy -f tank 2>/dev/null || true
    - source /tmp/zfs-env.sh && losetup -d "${LOOP_DEV}" 2>/dev/null || true
    - rm -f /tmp/zfs-pool.img
  rules:
    - if: $CI_COMMIT_BRANCH == "main"
    - when: manual
```

---

### 루프백 ZFS 풀 정리 (테스트 후)

테스트 환경 오염을 방지하기 위해 반드시 아래 순서로 정리해야 한다.

```bash
# 1. ZFS zvol 언마운트 및 내보내기 해제 (존재하는 경우)
sudo zfs destroy -r tank/pvc-test 2>/dev/null || true

# 2. ZFS 풀 삭제
sudo zpool destroy -f tank

# 3. 루프백 장치 해제
sudo losetup -d "${LOOP_DEV}"

# 4. 이미지 파일 제거
sudo rm -f /tmp/pillar-csi-test/zfs-pool.img
sudo rmdir /tmp/pillar-csi-test 2>/dev/null || true
```

**자동 정리 스크립트:**

```bash
#!/usr/bin/env bash
# hack/cleanup-loopback-zfs.sh
set -euo pipefail

POOL_NAME="${ZFS_TEST_POOL:-tank}"
IMG_PATH="${ZFS_TEST_IMG:-/tmp/pillar-csi-test/zfs-pool.img}"

# 풀이 존재하면 삭제
if sudo zpool list "${POOL_NAME}" &>/dev/null; then
    echo "ZFS 풀 '${POOL_NAME}' 삭제 중..."
    sudo zpool destroy -f "${POOL_NAME}"
fi

# 이미지 파일에 연결된 루프백 장치 해제
LOOP_DEV=$(sudo losetup --associated "${IMG_PATH}" | awk -F: '{print $1}')
if [[ -n "${LOOP_DEV}" ]]; then
    echo "루프백 장치 '${LOOP_DEV}' 해제 중..."
    sudo losetup -d "${LOOP_DEV}"
fi

# 이미지 파일 제거
if [[ -f "${IMG_PATH}" ]]; then
    echo "이미지 파일 '${IMG_PATH}' 제거 중..."
    sudo rm -f "${IMG_PATH}"
fi

echo "루프백 ZFS 환경 정리 완료."
```

---

### 알려진 한계 및 주의 사항

| 한계 | 설명 |
|------|------|
| **루프백 성능** | 루프백 장치는 파일 기반이므로 실제 NVMe SSD 대비 I/O 성능이 현저히 낮다. 성능 벤치마크에는 사용하지 않는다. |
| **Docker/Kind 내부 실행 불가** | Docker 컨테이너 내부에서 `losetup`은 기본적으로 실패한다 (`--privileged` 플래그 + `/dev` 마운트 필요). |
| **NVMe-oF 커널 모듈 분리** | 루프백 ZFS로 zvol을 생성해도 `nvmet`/`nvme-tcp` 커널 모듈 없이는 NVMe-oF 익스포트 불가. F2–F3 테스트는 여전히 별도 환경 필요. |
| **`/dev/zvol` 경로 지연** | `zfs create -V` 후 `/dev/zvol/...` symlink가 생성되는 데 최대 수 초 소요. 폴링 대기 필요 (`udevadm settle` 또는 `udevadm trigger`). |
| **루트 권한** | `zpool`/`zfs` 명령은 root 또는 `CAP_SYS_ADMIN`이 필요하다. CI 환경에서 `sudo` 없이는 실행 불가. |
| **동시 풀 이름 충돌** | 병렬 CI 실행 시 동일한 풀 이름(`tank`)을 사용하면 충돌 발생. 테스트마다 고유한 풀 이름(예: `tank-${CI_JOB_ID}`)을 사용해야 한다. |

---

### `/dev/zvol` 장치 생성 대기 (udev 폴링)

`zfs create -V` 명령 후 udev가 `/dev/zvol/...` symlink를 생성하기까지
약간의 시간이 걸린다. 테스트 코드에서 이를 보장하려면:

```bash
# udev 정착 대기 (권장)
sudo udevadm settle --timeout=10

# 또는 직접 폴링 (udevadm 없는 환경)
ZVOL_PATH="/dev/zvol/tank/pvc-test"
for i in $(seq 1 30); do
    if [[ -e "${ZVOL_PATH}" ]]; then
        echo "zvol 장치 확인됨: ${ZVOL_PATH}"
        break
    fi
    echo "대기 중... (${i}/30)"
    sleep 1
done
if [[ ! -e "${ZVOL_PATH}" ]]; then
    echo "오류: zvol 장치가 30초 내에 나타나지 않음" >&2
    exit 1
fi
```

이 폴링 패턴은 `internal/agent/nvmeof/device_poll.go`의 실제 NVMe-oF
디바이스 대기 로직(F11 참조)과 동일한 방식으로 동작한다.

---

### 관련 향후 테스트 항목

아래 테스트들은 루프백 ZFS 환경에서 실행 가능하지만 현재 미구현이다
(이 부록의 인프라 설정이 선행 조건이다):

| 테스트 ID | 이름 | 루프백 ZFS 필요 항목 |
|-----------|------|---------------------|
| F1 | `TestRealZFS_CreateVolume` | 루프백 풀에서 `zfs create -V` 실행 검증 |
| F13 | `TestRealZFS_CreateSnapshot` | `zfs snapshot tank/vol@snap` |
| F14 | `TestRealZFS_DeleteSnapshot` | `zfs destroy tank/vol@snap` |
| F15 | `TestRealZFS_ListSnapshots` | `zfs list -t snapshot tank` |
| F17 | `TestRealAgent_SendVolume_ZFSSend` | `zfs send tank/vol@snap` 스트리밍 |
| F18 | `TestRealAgent_ReceiveVolume_ZFSReceive` | `zfs receive tank/new-vol` |
| F20 | `TestRealZFS_CloneVolume_FromSnapshot` | `zfs clone tank/vol@snap tank/new-vol` |
| F22 | `TestRealZFS_PoolFull_CreateVolume` | 루프백 풀 크기를 작게 설정하여 ENOSPC 유발 |
| F23 | `TestRealZFS_PoolFull_ExpandVolume` | 풀 여유 공간 이상의 ExpandVolume 시도 |

---

## 부록: Fake Configfs — NVMe-oF 설정 파일시스템 CI 시뮬레이션

### 개요

Linux 커널의 `nvmet` 서브시스템은 configfs(`/sys/kernel/config/nvmet/`)를
통해 NVMe-oF 타깃을 관리한다. configfs는 특수 파일시스템이므로
**커널 모듈(`nvmet`, `nvmet-tcp`)이 로드된 실제 리눅스 커널**에서만
정상 동작한다. 표준 CI 환경(GitHub Actions, GitLab CI, Docker 컨테이너)에서는
이 모듈을 로드할 수 없다.

pillar-csi는 이 문제를 `NvmetTarget.ConfigfsRoot` 필드를 통해 해결한다.
테스트에서 이 필드를 `t.TempDir()`(일반 임시 디렉터리)로 설정하면,
`NvmetTarget.Apply()`/`Remove()` 등의 모든 configfs 조작이
**일반 파일시스템 쓰기**로 실행된다.

```
실제 운영 환경:
  NvmetTarget.ConfigfsRoot = "/sys/kernel/config"
  → mkdir /sys/kernel/config/nvmet/subsystems/nqn.xxx/ 시 커널이 NVMe 서브시스템 객체를 즉시 생성
  → device_path 파일 쓰기 시 커널이 블록 장치 연결
  → enable 파일에 "1" 쓰기 시 NVMe-oF I/O 수락 시작

테스트 환경 (CI 포함):
  NvmetTarget.ConfigfsRoot = t.TempDir()
  → 동일한 코드 경로, 동일한 디렉터리/파일 구조
  → 커널 동작 없음 — 순수 파일시스템 쓰기
  → 커널 모듈 불필요, root 권한 불필요
```

**CI 실행 가능성:** ✅ `t.TempDir()` 기반 fake configfs는 표준 CI에서 실행 가능.
실제 `nvmet`/`nvmet-tcp` 커널 모듈이 없어도 구조적 정확성을 검증할 수 있다.

---

### NVMe-oF configfs 디렉터리 구조

`NvmetTarget.Apply()`는 아래 configfs 트리를 생성한다. fake configfs에서는
동일한 경로가 일반 파일시스템 디렉터리/파일/심볼릭 링크로 생성된다.

```
<ConfigfsRoot>/
└── nvmet/
    ├── subsystems/
    │   └── <SubsystemNQN>/                   ← mkdir: 커널이 NVMe 서브시스템 생성
    │       ├── attr_allow_any_host            ← write: "0" (ACL 활성화) 또는 "1"
    │       ├── namespaces/
    │       │   └── <NamespaceID>/             ← mkdir: 커널이 네임스페이스 생성
    │       │       ├── device_path            ← write: 블록 디바이스 경로
    │       │       └── enable                 ← write: "1" 활성화, "0" 비활성화
    │       └── allowed_hosts/
    │           └── <HostNQN>                  ← symlink → ../../hosts/<HostNQN>
    ├── hosts/
    │   └── <HostNQN>/                         ← mkdir: 커널이 호스트 객체 생성
    └── ports/
        └── <PortID>/                          ← mkdir: 커널이 포트 객체 생성
            ├── addr_trtype                    ← write: "tcp"
            ├── addr_adrfam                    ← write: "ipv4"
            ├── addr_traddr                    ← write: BindAddress
            ├── addr_trsvcid                   ← write: 포트 번호
            └── subsystems/
                └── <SubsystemNQN>             ← symlink → ../../subsystems/<SubsystemNQN>
```

**포트 ID 결정 방식:** `stablePortID(BindAddress, Port)` — FNV-1a 해시 기반
결정적(deterministic) ID. 동일한 (주소, 포트) 쌍은 항상 동일한 포트 ID를 가진다.

```go
// internal/agent/nvmeof/configfs.go 발췌
func stablePortID(addr string, port int32) uint32 {
    var h uint32 = 2166136261 // FNV-1a 오프셋
    for i := range len(addr) {
        h ^= uint32(addr[i])
        h *= 16777619
    }
    h ^= uint32(port)
    h *= 16777619
    return h%65535 + 1 // [1, 65535] — 커널은 0을 거부
}
```

---

### 실제 configfs vs. Fake configfs 비교

| 동작 | 실제 `/sys/kernel/config` | Fake `t.TempDir()` |
|------|--------------------------|-------------------|
| `mkdir nvmet/subsystems/<nqn>/` | 커널이 NVMe 서브시스템 객체를 즉시 생성 | 일반 디렉터리 생성 |
| `mkdir nvmet/namespaces/<id>/` | 커널이 네임스페이스 객체를 즉시 생성 | 일반 디렉터리 생성 |
| `write device_path` | 커널이 블록 디바이스를 네임스페이스에 연결 | 일반 파일 쓰기 |
| `write enable = "1"` | 커널이 NVMe-oF I/O 수락 시작 | 일반 파일 쓰기 |
| `write attr_allow_any_host = "0"` | 커널이 ACL 검사 활성화 | 일반 파일 쓰기 |
| `write addr_trtype = "tcp"` | 커널이 TCP 트랜스포트 설정 | 일반 파일 쓰기 |
| `symlink ports/<id>/subsystems/<nqn>` | 커널이 포트-서브시스템 바인딩 생성 (NVMe-oF TCP 리스닝 시작) | 일반 심볼릭 링크 생성 |
| `symlink allowed_hosts/<host-nqn>` | 커널이 ACL 항목 추가 | 일반 심볼릭 링크 생성 |
| `rmdir nvmet/subsystems/<nqn>/` | 커널이 서브시스템 객체 삭제 | 일반 디렉터리 제거 |
| `write enable = "0"` | 커널이 NVMe-oF I/O 수락 중지 | 일반 파일 쓰기 |
| `rm ports/<id>/subsystems/<nqn>` | 커널이 포트-서브시스템 바인딩 해제 | 일반 파일 제거 |

**핵심 차이점:**

- **실제 configfs**: 각 파일시스템 조작이 즉시 커널 동작을 트리거한다.
  네임스페이스 디렉터리 생성 = NVMe 네임스페이스 객체 생성.
- **Fake configfs**: 동일한 디렉터리/파일/심볼릭 링크 구조가 생성되지만
  커널 트리거가 없다. 코드 경로(디렉터리 생성, 파일 쓰기, 심볼릭 링크)는
  실제와 100% 동일하게 실행된다.
- **읽기 전용 파일 시뮬레이션**: 실제 configfs에서 특정 파일은 읽기 전용이다
  (예: 이미 활성화된 네임스페이스의 일부 속성). 테스트에서는 `chmod`로
  이를 시뮬레이션할 수 있다 (`test/component/nvmeof_errors_test.go` 참조).
- **정리 차이**: 실제 configfs에서 커널은 디렉터리 제거 시 의존 하위 객체를
  자동으로 정리한다. 일반 파일시스템에서는 수동으로 파일을 먼저 제거해야
  디렉터리를 비울 수 있다. `NvmetTarget.Remove()`는 이 차이를 `bestEffort()`
  래퍼로 처리한다.

---

### 테스트에서의 사용 방법

#### 기본 패턴 (컴포넌트 테스트 및 E2E 에이전트 테스트)

```go
// 1. 임시 디렉터리를 configfs 루트로 사용
tmpdir := t.TempDir()  // 테스트 종료 시 자동 정리

// 2. NvmetTarget에 fake configfs 루트 주입
tgt := &nvmeof.NvmetTarget{
    ConfigfsRoot: tmpdir,              // 실제: "/sys/kernel/config"
    SubsystemNQN: "nqn.2026-01.com.bhyoo:pvc-abc123",
    NamespaceID:  1,
    DevicePath:   "/dev/zvol/tank/pvc-abc123",
    BindAddress:  "10.0.0.1",
    Port:         4420,
}

// 3. Apply() 호출 — 일반 파일시스템에 configfs 구조 생성
if err := tgt.Apply(); err != nil {
    t.Fatalf("Apply: %v", err)
}

// 4. 파일 내용으로 구조 검증
nsDir := filepath.Join(tmpdir, "nvmet", "subsystems",
    "nqn.2026-01.com.bhyoo:pvc-abc123", "namespaces", "1")

data, _ := os.ReadFile(filepath.Join(nsDir, "device_path"))
// data == "/dev/zvol/tank/pvc-abc123"

data, _ = os.ReadFile(filepath.Join(nsDir, "enable"))
// data == "1"
```

#### E2E 에이전트 테스트 패턴 (agent_e2e_test.go)

```go
func TestAgent_RoundTrip(t *testing.T) {
    // fake configfs 루트 생성
    configfsRoot := t.TempDir()

    // mock ZFS backend 생성 (실제 zfs(8) 명령 없음)
    mockBackend := &agentE2EMockBackend{
        devicePath: "/dev/zvol/tank/pvc-test",
    }

    // agent.Server 구성 — 실제 코드, fake configfs 루트
    srv := agent.NewServer(
        map[string]backend.VolumeBackend{"tank": mockBackend},
        configfsRoot,  // ← t.TempDir() 주입
    )

    // 실제 gRPC 리스너 (localhost:0)에 등록
    grpcSrv := grpc.NewServer()
    agentv1.RegisterAgentServiceServer(grpcSrv, srv)
    lis, _ := net.Listen("tcp", "127.0.0.1:0")
    go grpcSrv.Serve(lis)
    defer grpcSrv.GracefulStop()

    // 실제 gRPC 클라이언트로 호출
    conn, _ := grpc.NewClient(lis.Addr().String(),
        grpc.WithTransportCredentials(insecure.NewCredentials()))
    client := agentv1.NewAgentServiceClient(conn)

    // ExportVolume 호출 → 내부적으로 NvmetTarget.Apply() 실행
    ctx := context.Background()
    _, err := client.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
        VolumeId:     "tank/pvc-test",
        DevicePath:   "/dev/zvol/tank/pvc-test",
        ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
    })
    if err != nil {
        t.Fatalf("ExportVolume: %v", err)
    }

    // configfs 구조가 tmpdir에 생성됐는지 검증
    subsysBase := filepath.Join(configfsRoot, "nvmet", "subsystems")
    entries, _ := os.ReadDir(subsysBase)
    if len(entries) == 0 {
        t.Error("configfs 서브시스템 디렉터리가 생성되지 않음")
    }
}
```

#### 오류 시뮬레이션 패턴 (읽기 전용 파일)

실제 configfs에서 발생하는 권한 오류(예: 이미 활성화된 네임스페이스의 `enable`
파일 재쓰기 실패)를 fake configfs에서 `chmod`로 재현할 수 있다:

```go
// 파일을 읽기 전용으로 만들어 쓰기 실패 유발
enablePath := filepath.Join(tmpdir, "nvmet", "subsystems", nqn, "namespaces", "1", "enable")
os.WriteFile(enablePath, []byte("1"), 0o600)  // 먼저 파일 생성
os.Chmod(enablePath, 0o444)                    // 읽기 전용으로 변경

// Apply() 재호출 시 enable 파일 쓰기가 실패해야 함
err := tgt.Apply()
// err != nil; "configfs write ... permission denied" 포함
```

---

### E2E 에이전트 테스트에서의 configfs 역할

E9 섹션(Agent gRPC E2E 테스트)의 테스트들은 모두 fake configfs를 사용한다.
각 테스트에서 configfs가 어떤 역할을 하는지 구체적으로 설명한다.

#### E9.2 전체 왕복 (`TestAgent_RoundTrip`)

```
CreateVolume   → mock ZFS backend.Create()                      ← configfs 미사용
ExportVolume   → NvmetTarget.Apply(configfsRoot=tmpdir)         ← configfs 사용
  1. subsystems/<nqn>/ 생성
  2. namespaces/1/ 생성 + device_path/enable 쓰기
  3. ports/<id>/ 생성 + addr_* 쓰기
  4. ports/<id>/subsystems/<nqn> 심볼릭 링크 생성
AllowInitiator → NvmetTarget.AllowHost(hostNQN)                 ← configfs 사용
  5. hosts/<hostNQN>/ 생성
  6. subsystems/<nqn>/allowed_hosts/<hostNQN> 심볼릭 링크 생성
  7. attr_allow_any_host = "0" 쓰기
DenyInitiator  → NvmetTarget.DenyHost(hostNQN)                  ← configfs 사용
  8. allowed_hosts/<hostNQN> 심볼릭 링크 제거
UnexportVolume → NvmetTarget.Remove(configfsRoot=tmpdir)        ← configfs 사용
  9. 포트 서브시스템 링크 제거
  10. enable = "0" 쓰기 + 네임스페이스 디렉터리 제거
  11. 서브시스템 디렉터리 제거
DeleteVolume   → mock ZFS backend.Delete()                      ← configfs 미사용
```

**테스트가 검증하는 것:**
- `ExportVolume` 호출 후 configfs 디렉터리 구조가 기대한 경로에 생성됨
- `device_path`, `enable`, `addr_*` 파일 내용이 요청 파라미터와 일치함
- `AllowInitiator` 후 `allowed_hosts/<nqn>` 심볼릭 링크가 존재함
- `UnexportVolume` 후 서브시스템/네임스페이스 디렉터리가 제거됨

#### E9.3 재조정 복구 (`TestAgent_ReconcileStateRestoresExports`)

```
ReconcileState(volumeList) → 각 볼륨에 대해 NvmetTarget.Apply() 호출
  → tmpdir에 모든 볼륨의 configfs 디렉터리가 생성됨
  → 서버 재시작 후 NVMe-oF 타깃 복원 시뮬레이션
```

재조정(reconciliation) 테스트에서 fake configfs는 특히 중요하다. 실제 시스템에서
서버가 재시작되면 `nvmet` 커널 모듈이 configfs 상태를 초기화할 수 있다. 에이전트는
`ReconcileState` RPC를 통해 configfs를 재구성해야 한다. 이 동작을 `t.TempDir()`로
안전하게 시뮬레이션할 수 있다.

#### E9.1 헬스체크 (`TestAgent_HealthCheck`)

에이전트 헬스체크는 configfs 루트의 존재 여부를 확인한다:

```go
// 헬스체크가 검사하는 경로 (internal/agent/server.go 발췌)
sysModuleZFSPath  = "/sys/module/zfs"          // ZFS 커널 모듈 존재 여부
configfsNvmetPath = configfsRoot + "/nvmet"    // nvmet configfs 마운트 여부
```

테스트에서는 `sysModuleZFSPath`를 tmpdir 내부의 존재하는 파일로,
`configfsNvmetPath`를 tmpdir 내부의 존재하는 디렉터리로 설정하여
헬스체크 로직을 CI에서 검증할 수 있다:

```go
// HealthCheck 테스트 설정 예시
tmpdir := t.TempDir()

// ZFS 모듈 파일 시뮬레이션
zfsModPath := filepath.Join(tmpdir, "zfs-module")
os.WriteFile(zfsModPath, []byte(""), 0o644)

// nvmet configfs 디렉터리 시뮬레이션
nvmetDir := filepath.Join(tmpdir, "nvmet")
os.MkdirAll(nvmetDir, 0o750)

// 서버에 두 경로를 모두 주입 (실제 /sys 경로 대신)
srv := agent.NewServerWithPaths(backends, tmpdir, zfsModPath)
```

---

### configfs 검증 헬퍼 함수

컴포넌트 테스트(`test/component/nvmeof_test.go`)와 에이전트 E2E 테스트에서
공통으로 사용하는 검증 헬퍼 패턴:

```go
// nvmetSubsystemDir은 서브시스템 configfs 경로를 반환한다.
// 실제 경로: /sys/kernel/config/nvmet/subsystems/<nqn>
// 테스트 경로: <tmpdir>/nvmet/subsystems/<nqn>
func nvmetSubsystemDir(configfsRoot, nqn string) string {
    return filepath.Join(configfsRoot, "nvmet", "subsystems", nqn)
}

// nvmetNamespaceDir은 네임스페이스 configfs 경로를 반환한다.
func nvmetNamespaceDir(configfsRoot, nqn string) string {
    return filepath.Join(nvmetSubsystemDir(configfsRoot, nqn), "namespaces", "1")
}

// nvmetPortsDir은 포트 configfs 디렉터리를 반환한다.
func nvmetPortsDir(configfsRoot string) string {
    return filepath.Join(configfsRoot, "nvmet", "ports")
}

// requireFileContent는 configfs 파일의 내용이 기대값과 일치하는지 검증한다.
func requireFileContent(t *testing.T, path, want string) {
    t.Helper()
    data, err := os.ReadFile(path)
    if err != nil {
        t.Fatalf("configfs 파일 %q 읽기 실패: %v", path, err)
    }
    if string(data) != want {
        t.Errorf("configfs 파일 %q: 기대값=%q, 실제값=%q", path, want, string(data))
    }
}

// requireDirExists는 configfs 디렉터리가 존재하는지 검증한다.
func requireDirExists(t *testing.T, path string) {
    t.Helper()
    fi, err := os.Stat(path)
    if err != nil {
        t.Fatalf("configfs 디렉터리 %q 없음: %v", path, err)
    }
    if !fi.IsDir() {
        t.Fatalf("경로 %q 는 디렉터리가 아님 (mode=%s)", path, fi.Mode())
    }
}

// requireSymlinkTarget은 심볼릭 링크가 기대 대상을 가리키는지 검증한다.
func requireSymlinkTarget(t *testing.T, linkPath, wantTarget string) {
    t.Helper()
    target, err := os.Readlink(linkPath)
    if err != nil {
        t.Fatalf("심볼릭 링크 %q 읽기 실패: %v", linkPath, err)
    }
    if target != wantTarget {
        t.Errorf("심볼릭 링크 %q: 기대 대상=%q, 실제 대상=%q", linkPath, wantTarget, target)
    }
}
```

---

### Fake Configfs CI 실행 가능성 요약

| 시나리오 | CI 실행 가능 | 이유 |
|---------|:-----------:|------|
| `NvmetTarget.Apply()` 구조 검증 (디렉터리/파일/심볼릭 링크) | ✅ | 일반 파일시스템 쓰기만 필요 |
| `NvmetTarget.Remove()` 정리 검증 | ✅ | 일반 파일시스템 조작 |
| `NvmetTarget.AllowHost()` / `DenyHost()` ACL 구조 검증 | ✅ | 심볼릭 링크 생성/제거 |
| 에이전트 gRPC ExportVolume → configfs 생성 통합 검증 | ✅ | gRPC 직렬화 레이어 포함 |
| 에이전트 ReconcileState → configfs 복원 검증 | ✅ | 상태 재구성 로직 검증 |
| 에이전트 HealthCheck → configfs 디렉터리 존재 검증 | ✅ | 파일 존재 여부만 확인 |
| 권한 오류 시나리오 (`chmod 0o444` 읽기 전용 파일) | ✅ | 일반 Unix 권한 변경 |
| 멱등성(idempotency) 검증 (Apply() 2회 호출) | ✅ | 파일시스템 재쓰기 허용 |
| 실제 NVMe-oF TCP 리스닝 검증 | ❌ | `nvmet-tcp` 커널 모듈 필요 |
| 실제 NVMe-oF initiator 연결 검증 | ❌ | `nvme-tcp` 커널 모듈 + 블록 장치 필요 |
| 커널 enable 파일 트리거 동작 검증 | ❌ | `/sys/kernel/config` + `nvmet` 모듈 필요 |
| 실제 `/dev/nvme*` 블록 디바이스 생성 검증 | ❌ | 커널 NVMe 드라이버 필요 |
| 실제 NQN 기반 접근 제어 동작 검증 | ❌ | 실제 NVMe-oF initiator/target 환경 필요 |

**결론:** Fake configfs(`t.TempDir()`)는 NVMe-oF 타깃 관리 코드의
**구조적 정확성**(올바른 디렉터리/파일 생성, 올바른 내용 쓰기, 올바른 순서)을
CI에서 완전히 검증할 수 있다. **커널 레벨 동작**(실제 NVMe-oF 리스닝 시작,
실제 initiator 연결)은 별도의 실제 하드웨어 환경(F2, F3 참조)에서만 검증
가능하다.

---

### 실제 configfs가 필요한 테스트 인프라 요구사항

아래는 fake configfs로 검증할 수 없는 테스트(F2–F3 등)를 위한 실제
인프라 요구사항이다:

```
운영체제:         Linux (Ubuntu 22.04+ 권장)
커널:            5.15 이상 (nvmet TCP 지원 안정화)
커널 모듈 (타깃):
  - nvmet          (NVMe-oF target core)
  - nvmet-tcp      (NVMe-oF TCP transport target)
커널 모듈 (이니시에이터):
  - nvme-tcp       (NVMe-oF TCP initiator)
권한:            root 또는 CAP_SYS_ADMIN
configfs 마운트:  /sys/kernel/config (modprobe configfs 또는 자동 마운트)
블록 장치:        실제 zvol 또는 루프백 장치 (F1 참조)
```

**커널 모듈 로드 및 확인:**

```bash
# 타깃 모듈 로드
sudo modprobe nvmet
sudo modprobe nvmet-tcp

# initiator 모듈 로드
sudo modprobe nvme-tcp

# configfs 마운트 확인
mount | grep configfs
# 출력 예시: configfs on /sys/kernel/config type configfs (rw,relatime)

# nvmet configfs 구조 확인 (모듈 로드 후)
ls /sys/kernel/config/nvmet/
# 출력 예시: hosts  ports  subsystems

# nvmet 서브시스템 수동 생성 (테스트용)
NQN="nqn.2026-01.com.test:pvc-manual"
sudo mkdir /sys/kernel/config/nvmet/subsystems/${NQN}
echo 1 | sudo tee /sys/kernel/config/nvmet/subsystems/${NQN}/attr_allow_any_host
# → 커널이 NVMe 서브시스템 객체를 즉시 생성
```

**표준 CI 환경에서의 제약:**

| CI 환경 | nvmet 모듈 사용 가능 여부 | 권장 대안 |
|---------|:------------------------:|---------|
| GitHub Actions (`ubuntu-latest`) | ❌ (커널 모듈 제한) | Fake configfs 사용 (CI 가능) |
| GitLab CI (Docker executor) | ❌ (`--privileged` 없으면 불가) | Fake configfs 사용 (CI 가능) |
| GitLab CI (`privileged: true`) | ⚠️ 조건부 가능 (호스트 커널에 모듈 존재 필요) | Self-hosted 러너 권장 |
| Self-hosted 베어메탈 러너 (Linux) | ✅ | 실제 E2E (F2, F3) 실행 가능 |
| Self-hosted KVM/QEMU VM 러너 | ✅ (네스티드 가상화 지원 시) | 실제 E2E (F2, F3) 실행 가능 |
| Docker 컨테이너 내부 | ❌ (configfs는 호스트 커널 공유 필요) | Fake configfs 사용 (CI 가능) |

---

### 관련 향후 테스트 항목 (실제 configfs 필요)

아래 테스트들은 실제 `nvmet` 커널 모듈 환경에서만 실행 가능하며 현재 미구현이다:

| 테스트 ID | 이름 | 실제 configfs 필요 항목 |
|-----------|------|------------------------|
| F2 | `TestRealNVMeoF_Export` | `/sys/kernel/config/nvmet/` + `nvmet` 모듈 로드 |
| F3 | `TestRealNVMeoF_Connect` | NVMe-oF TCP 대상 서버 + `nvme-tcp` 모듈 + 블록 장치 |
| F8 | `TestRealNode_NodeStageVolume_ActualMount` | 실제 `/dev/nvme*` 블록 장치 + 루트 권한 |
| F9 | `TestRealNode_NodePublishVolume_BindMount` | 실제 NVMe-oF 블록 장치 + 컨테이너 네임스페이스 |
| F10 | `TestRealNode_NodeUnstageVolume_ActualDetach` | 실제 nvme disconnect + udev |
| F11 | `TestRealNode_NodeStageVolume_DeviceAppearDelay` | 실제 udev 지연 환경 |
| F24 | `TestRealNode_ConcurrentStageUnstage_SameVolume` | 실제 NVMe-oF 디바이스 + 커널 레벨 레이스 컨디션 검증 |

---

## 부록: Mock Agent 모드 — CI에서 CSI 에이전트 시뮬레이션

### 개요

pillar-csi의 인프로세스 E2E 테스트(유형 A)는 실제 `pillar-agent` 바이너리 없이
CSI 컨트롤러와 노드 서버의 **에이전트 연동 경로**를 검증한다. 이를 위해
두 종류의 Mock Agent를 사용한다:

| Mock 컴포넌트 | 역할 | 사용 위치 | CI 실행 가능 |
|-------------|------|----------|:-----------:|
| `mockAgentServer` | CSI ControllerServer가 다이얼하는 programmable gRPC 서버 더블 | E1–E8, E11–E17 | ✅ |
| `agentE2EMockBackend` | agent.Server 내부의 실제 gRPC 리스너에 주입된 mock 백엔드 | E9 | ✅ |

이 두 컴포넌트는 서로 다른 레이어를 대체한다:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  유형 A 인프로세스 E2E — 컴포넌트 관계도                                      │
│                                                                             │
│  ┌──────────────────────────┐       실제 gRPC (localhost:0)                  │
│  │  CSI ControllerServer    │ ─────────────────────────────► mockAgentServer │
│  │  (실제 코드)              │       ← E1–E8, E11–E17에서 사용               │
│  └──────────────────────────┘                                               │
│                                                                             │
│  ┌──────────────────────────┐       실제 gRPC (localhost:0)                  │
│  │  agentv1 gRPC 클라이언트  │ ─────────────────────────────► agent.Server   │
│  │  (테스트 코드 직접 호출)   │                                │ (실제 코드)  │
│  └──────────────────────────┘                                │              │
│                                                              ▼              │
│                                                    agentE2EMockBackend      │
│                                                    (mock ZFS backend)       │
│                                                    ← E9에서 사용            │
│                                                    t.TempDir() = configfs   │
└─────────────────────────────────────────────────────────────────────────────┘
```

**핵심 원칙:**
- `mockAgentServer`는 실제 agent.Server **전체를 대체**한다. CSI 컨트롤러가
  에이전트 gRPC 서버에 연결하는 것처럼 보이지만, 실제로는 테스트 내부의
  programmable 스텁이다.
- `agentE2EMockBackend`는 agent.Server 내부의 **ZFS 백엔드만 대체**한다.
  실제 agent.Server 코드(gRPC 직렬화, configfs 조작 로직)가 실행된다.

---

### Mock Agent 유형 1: `mockAgentServer`

#### 목적

CSI ControllerServer가 AgentService gRPC를 호출하는 경로를 검증한다.
실제 pillar-agent 프로세스, ZFS zvol, NVMe-oF 커널 모듈이 **전혀 불필요**하다.

#### 위치

```
test/e2e/csi_helpers_test.go
```

#### 활성화 방법

`mockAgentServer`는 `csiControllerE2EEnv`(및 `csiLifecycleEnv`)를 생성하면
자동으로 시작된다. 별도의 빌드 태그나 환경 변수가 필요 없다:

```go
// csiControllerE2EEnv 생성 시 내부적으로 mockAgentServer가 시작됨
env := newCSIControllerE2EEnv(t, "storage-1")

// env.AgentMock 을 통해 동작을 설정할 수 있음
env.AgentMock.CreateVolumeDevicePath = "/dev/zvol/tank/my-pvc"
env.AgentMock.CreateVolumeCapacityBytes = 10 << 30
```

`t.Cleanup`이 등록되어 테스트 종료 시 gRPC 서버가 자동으로 중지된다.

#### 구조

```go
type mockAgentServer struct {
    agentv1.UnimplementedAgentServiceServer  // 미구현 RPC는 Unimplemented 반환

    mu sync.Mutex  // 동시 호출 보호

    // ── 주입 가능한 오류 ──────────────────────────────────────────────────
    CreateVolumeErr   error  // CreateVolume RPC가 반환할 오류
    DeleteVolumeErr   error  // DeleteVolume RPC가 반환할 오류
    ExpandVolumeErr   error  // ExpandVolume RPC가 반환할 오류
    ExportVolumeErr   error  // ExportVolume RPC가 반환할 오류
    UnexportVolumeErr error  // UnexportVolume RPC가 반환할 오류
    AllowInitiatorErr error  // AllowInitiator RPC가 반환할 오류
    DenyInitiatorErr  error  // DenyInitiator RPC가 반환할 오류

    // ── 주입 가능한 응답 ──────────────────────────────────────────────────
    CreateVolumeDevicePath    string           // CreateVolume 응답의 DevicePath
    CreateVolumeCapacityBytes int64            // 0이면 요청 용량을 그대로 반환
    ExportVolumeInfo          *agentv1.ExportInfo  // nil이면 기본값 사용
    ExpandVolumeCapacityBytes int64            // 0이면 요청 용량을 그대로 반환

    // ── 호출 기록 슬라이스 ────────────────────────────────────────────────
    CreateVolumeCalls   []agentCreateVolumeCall
    DeleteVolumeCalls   []agentDeleteVolumeCall
    ExpandVolumeCalls   []agentExpandVolumeCall
    ExportVolumeCalls   []agentExportVolumeCall
    UnexportVolumeCalls []agentUnexportVolumeCall
    AllowInitiatorCalls []agentAllowInitiatorCall
    DenyInitiatorCalls  []agentDenyInitiatorCall
}
```

#### 기본값

`newMockAgentServer()`가 반환하는 기본 설정:

| 필드 | 기본값 | 의미 |
|------|--------|------|
| `CreateVolumeDevicePath` | `"/dev/test-device"` | 생성된 볼륨의 블록 디바이스 경로 |
| `ExportVolumeInfo.TargetId` | `"nqn.2026-01.com.bhyoo.pillar-csi:test-volume"` | NVMe-oF 서브시스템 NQN |
| `ExportVolumeInfo.Address` | `"127.0.0.1"` | NVMe-oF 타깃 IP 주소 |
| `ExportVolumeInfo.Port` | `4420` | NVMe-oF TCP 포트 |
| `ExportVolumeInfo.VolumeRef` | `"test-volume"` | 볼륨 참조 이름 |
| 모든 `*Err` 필드 | `nil` | 오류 없음 (정상 경로) |
| `CreateVolumeCapacityBytes` | `0` (요청 값 에코) | 요청한 용량을 그대로 반환 |
| `ExpandVolumeCapacityBytes` | `0` (요청 값 에코) | 요청한 용량을 그대로 반환 |

#### 설정 예시

**오류 시나리오 주입:**

```go
env := newCSIControllerE2EEnv(t, "storage-1")

// CreateVolume이 ResourceExhausted 오류를 반환하도록 설정
env.AgentMock.CreateVolumeErr = status.Errorf(
    codes.ResourceExhausted, "out of space on pool tank")

// CSI CreateVolume 호출 → agent.CreateVolume 실패 → CSI 오류 전파
resp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
    Name:               "pvc-test",
    Parameters:         env.defaultCreateVolumeParams(),
    VolumeCapabilities: defaultVolumeCapabilities(),
})
// err != nil; agent 오류가 CSI 상태 코드로 매핑됨
```

**사용자 정의 응답 설정:**

```go
env := newCSIControllerE2EEnv(t, "storage-1")

// 특정 NQN, 주소, 포트로 ExportVolume 응답 설정
env.AgentMock.ExportVolumeInfo = &agentv1.ExportInfo{
    TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-custom",
    Address:   "10.0.1.5",
    Port:      4421,
    VolumeRef: "tank/pvc-custom",
}

// CreateVolume 후 VolumeContext에 위 NQN/Address/Port가 포함됨
resp, err := env.Controller.CreateVolume(ctx, ...)
// resp.Volume.VolumeContext["target_id"] == "nqn.2026-01...pvc-custom"
// resp.Volume.VolumeContext["address"] == "10.0.1.5"
// resp.Volume.VolumeContext["port"] == "4421"
```

**호출 기록 검증:**

```go
env := newCSIControllerE2EEnv(t, "storage-1")

// CreateVolume 호출
_, _ = env.Controller.CreateVolume(ctx, ...)

// agent.CreateVolume 이 정확히 1회 호출됐는지 검증
if len(env.AgentMock.CreateVolumeCalls) != 1 {
    t.Errorf("agent.CreateVolume 호출 횟수 = %d, 기대 = 1",
        len(env.AgentMock.CreateVolumeCalls))
}

// 호출 인수 검증
call := env.AgentMock.CreateVolumeCalls[0]
if call.VolumeID != "tank/pvc-test" {
    t.Errorf("VolumeID = %q, 기대 = %q", call.VolumeID, "tank/pvc-test")
}
```

**라이프사이클 테스트에서 공유 상태 머신과 함께 사용:**

```go
// csiLifecycleEnv는 컨트롤러와 노드가 동일한 VolumeStateMachine을 공유하며,
// 컨트롤러 쪽의 mockAgentServer가 ExportVolumeInfo를 반환하면
// 그 NQN/Address/Port가 NodeStageVolume에 VolumeContext로 전달됨
env := newCSILifecycleEnvWithSM(t, "storage-1", "worker-1")

// env.AgentMock.ExportVolumeInfo는 newCSILifecycleEnvWithSM 내부에서
// 라이프사이클 테스트 상수로 미리 설정됨:
//   TargetId  = lifecycleTestNQN
//   Address   = lifecycleTestAddress
//   Port      = lifecycleTestPort
```

#### `mockAgentServer`가 구현하는 RPC

| RPC | 구현 | 동작 |
|-----|------|------|
| `CreateVolume` | ✅ | `CreateVolumeErr` 반환 또는 `CreateVolumeDevicePath`/`CapacityBytes` 포함 응답 |
| `DeleteVolume` | ✅ | `DeleteVolumeErr` 반환 또는 빈 성공 응답 |
| `ExpandVolume` | ✅ | `ExpandVolumeErr` 반환 또는 `ExpandVolumeCapacityBytes` 포함 응답 |
| `ExportVolume` | ✅ | `ExportVolumeErr` 반환 또는 `ExportVolumeInfo` 포함 응답 |
| `UnexportVolume` | ✅ | `UnexportVolumeErr` 반환 또는 빈 성공 응답 |
| `AllowInitiator` | ✅ | `AllowInitiatorErr` 반환 또는 빈 성공 응답 |
| `DenyInitiator` | ✅ | `DenyInitiatorErr` 반환 또는 빈 성공 응답 |
| `GetCapabilities` | ❌ | `codes.Unimplemented` (UnimplementedAgentServiceServer 기본) |
| `HealthCheck` | ❌ | `codes.Unimplemented` (CSI 컨트롤러 테스트에 불필요) |
| `ReconcileState` | ❌ | `codes.Unimplemented` (CSI 컨트롤러 테스트에 불필요) |
| `ListVolumes` | ❌ | `codes.Unimplemented` |
| `ListExports` | ❌ | `codes.Unimplemented` |
| `GetCapacity` | ❌ | `codes.Unimplemented` |

#### 피델리티 제한

`mockAgentServer`는 실제 pillar-agent 대비 아래 항목을 **시뮬레이션하지 않는다**:

| 항목 | 이유 |
|------|------|
| 실제 ZFS zvol 생성/삭제 | mock은 사전 설정된 응답 필드만 반환 |
| 실제 configfs NVMe-oF 서브시스템 생성 | ExportVolumeInfo는 하드코딩된 테스트 값 |
| 실제 NVMe 접근 제어 (ACL) | AllowInitiator/DenyInitiator는 콜 기록만 |
| 볼륨 상태 유지 | 동일 볼륨 ID에 대한 중복 생성 감지 없음 |
| 풀 용량 계산 | GetCapacity 미구현 |
| 에이전트 재시작 복구 (ReconcileState) | 미구현 |
| 실제 gRPC 재시도/백오프 | 즉시 응답; 연결 오류 시뮬레이션 없음 |

**단, 실제와 동일한 항목:**
- 실제 TCP gRPC 리스너(localhost:0)를 통해 연결하므로 **gRPC 직렬화/역직렬화**가 실제와 동일
- `sync.Mutex`로 보호되므로 **동시 호출**이 안전하게 기록됨
- gRPC 상태 코드가 직접 반환되므로 **오류 코드 매핑**이 검증됨

---

### Mock Agent 유형 2: `agentE2EMockBackend`

#### 목적

실제 `agent.Server` 코드(gRPC 직렬화, configfs 조작 로직, NQN 생성 로직)를
유지하면서 **ZFS 백엔드만 대체**한다. E9 섹션의 Agent gRPC E2E 테스트에서
사용된다.

#### 위치

```
test/e2e/agent_e2e_test.go
```

#### 활성화 방법

```go
// 1. mock 백엔드 생성
mock := &agentE2EMockBackend{
    devicePath:  "/path/to/fake/device",   // os.Stat이 성공할 실제 파일 경로
    totalBytes:  10 << 30,                 // HealthCheck/GetCapacity 응답용
    availBytes:  8 << 30,
}

// 2. agentE2EEnv 생성 (내부적으로 실제 gRPC 서버 시작)
env := newAgentE2EEnv(t, mock)

// 3. 실제 gRPC 클라이언트로 호출
resp, err := env.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
    VolumeId:      "tank/pvc-test",
    CapacityBytes: 1 << 30,
})
```

`t.Cleanup`이 등록되어 테스트 종료 시 gRPC 서버와 클라이언트 연결이
자동으로 종료된다.

#### 구조

```go
type agentE2EMockBackend struct {
    // 응답 값
    devicePath  string  // Create() 및 DevicePath()가 반환할 경로
    totalBytes  int64   // Capacity() 응답의 총 용량
    availBytes  int64   // Capacity() 응답의 가용 용량

    // 주입 가능한 오류 (nil = 성공)
    createErr   error   // Create() 오류
    deleteErr   error   // Delete() 오류
    expandErr   error   // Expand() 오류
    capacityErr error   // Capacity() 오류
}
```

#### 환경 구조 (`agentE2EEnv`)

```go
type agentE2EEnv struct {
    client     agentv1.AgentServiceClient  // 실제 gRPC 클라이언트
    cfgRoot    string                      // t.TempDir() = fake configfs 루트
    grpcServer *grpc.Server               // 실제 gRPC 서버 (localhost:0)
    conn       *grpc.ClientConn           // 클라이언트 연결
}
```

#### 설정 예시

**정상 경로 (볼륨 라이프사이클 검증):**

```go
// 가짜 디바이스 파일 생성 (os.Stat이 통과하도록)
fakeDevPath := createFakeDevice(t, "zvol-test")

mock := &agentE2EMockBackend{
    devicePath: fakeDevPath,   // ExportVolume의 WaitForDevice가 이 경로를 stat함
    totalBytes: 10 << 30,
    availBytes: 8 << 30,
}
env := newAgentE2EEnv(t, mock)

// 전체 라이프사이클: CreateVolume → ExportVolume → ... → DeleteVolume
```

**오류 시뮬레이션:**

```go
mock := &agentE2EMockBackend{
    createErr: errors.New("out of space"),  // ZFS 풀 가득 참
}
env := newAgentE2EEnv(t, mock)

_, err := env.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
    VolumeId:      "tank/pvc-full",
    CapacityBytes: 100 << 30,
})
// err != nil; agent.Server가 백엔드 오류를 gRPC 상태 코드로 변환함
```

**configfs 상태 검증:**

```go
mock := &agentE2EMockBackend{devicePath: createFakeDevice(t, "zvol-cfg")}
env := newAgentE2EEnv(t, mock)

_, _ = env.client.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
    VolumeId:     "tank/pvc-cfg",
    DevicePath:   mock.devicePath,
    ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
    ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
})

// env.cfgRoot 아래 실제 디렉터리/파일/심볼릭 링크가 생성됨
nqn := "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-cfg"
subDir := filepath.Join(env.cfgRoot, "nvmet", "subsystems", nqn)
if _, err := os.Stat(subDir); err != nil {
    t.Fatalf("configfs 서브시스템 디렉터리 없음: %v", err)
}
```

**재조정 복구 시뮬레이션:**

```go
// 에이전트 재시작 후 상태 복원을 시뮬레이션:
// 빈 configfs로 새 서버를 시작하고 ReconcileState 호출
mock := &agentE2EMockBackend{}
env := newAgentE2EEnv(t, mock)

_, err := env.client.ReconcileState(ctx, &agentv1.ReconcileStateRequest{
    Volumes: []*agentv1.VolumeDesiredState{
        {
            VolumeId:   "tank/pvc-restarted",
            DevicePath: "/dev/zvol/tank/pvc-restarted",
            Exports: []*agentv1.ExportDesiredState{{
                ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
                ExportParams:      nvmeofTCPExportParams("10.0.0.1", 4420),
                AllowedInitiators: []string{"nqn.2026-01.io.example:host-1"},
            }},
        },
    },
})
// configfs에 서브시스템/호스트/ACL이 재생성됨
```

#### `agentE2EMockBackend`가 구현하는 인터페이스

```go
type VolumeBackend interface {
    Create(ctx, volumeID string, capacityBytes int64,
        params *agentv1.ZfsVolumeParams) (devicePath string, allocatedBytes int64, error)
    Delete(ctx, volumeID string) error
    Expand(ctx, volumeID string, requestedBytes int64) (allocatedBytes int64, error)
    Capacity(ctx) (totalBytes, availBytes int64, error)
    ListVolumes(ctx) ([]*agentv1.VolumeInfo, error)
    DevicePath(volumeID string) string
}
```

#### 피델리티 제한

`agentE2EMockBackend`는 실제 ZFS 백엔드 대비 아래 항목을 시뮬레이션하지 않는다:

| 항목 | 이유 |
|------|------|
| 실제 `zfs create -V` 명령 실행 | `zfs(8)` 명령 없음; `devicePath` 필드 반환 |
| zvol 크기 반올림 (512B 배수) | 요청 용량을 그대로 반환 |
| ZFS 풀 존재 여부 검증 | 필드 설정으로만 제어 |
| `ConflictError` 자동 감지 | `createErr` 수동 설정 필요 |
| `zpool status` 파싱 | `totalBytes`/`availBytes` 고정 값 |
| ZFS 스냅샷/클론 지원 | 미구현 |
| `ListVolumes` 실제 목록 | 항상 빈 슬라이스 반환 |

**단, 실제와 동일한 항목:**
- `agent.Server` 코드 전체가 실행되므로 **NQN 생성 로직**, **configfs 조작**,
  **gRPC 오류 매핑**, **ReconcileState 재조정 알고리즘**이 실제와 동일하게 검증됨
- 실제 gRPC 리스너(localhost:0)를 통해 호출하므로 **직렬화 레이어** 검증됨
- `t.TempDir()`가 configfs 루트이므로 **configfs 구조 정확성** 검증됨

---

### Mock Agent 선택 가이드

```
테스트 목적에 따른 Mock Agent 선택:

CSI ControllerServer의 에이전트 호출 경로를 검증하고 싶다
    → mockAgentServer 사용 (csiControllerE2EEnv)
    → 이유: 에이전트 동작을 완전 제어하면서 CSI 컨트롤러 로직에 집중

agent.Server의 gRPC 처리 + configfs 조작 로직을 검증하고 싶다
    → agentE2EMockBackend 사용 (agentE2EEnv)
    → 이유: agent.Server 실제 코드 실행하면서 ZFS 의존성만 제거

agent.Server의 ZFS 오류 → gRPC 오류 코드 매핑을 검증하고 싶다
    → agentE2EMockBackend.createErr/deleteErr/... 설정
    → 이유: 실제 gRPC 레이어를 통해 오류 변환 로직 검증

CSI Controller + Node 전체 라이프사이클을 검증하고 싶다
    → newCSILifecycleEnvWithSM 사용 (mockAgentServer + 공유 SM)
    → 이유: 순서 제약(VolumeStateMachine)까지 포함한 통합 검증

agent 재시작 복구(ReconcileState)를 검증하고 싶다
    → agentE2EMockBackend 사용 (새 agentE2EEnv로 "재시작" 시뮬레이션)
    → 이유: ReconcileState가 agent.Server 내부에 구현됨
```

---

### CI에서 Mock Agent 실행 환경 요구사항

두 Mock Agent 모두 **추가 인프라 없이** 표준 CI에서 실행 가능하다.

#### 최소 요구사항

| 항목 | 요구사항 | 설명 |
|------|---------|------|
| Go 버전 | 1.22+ | `go test ./test/e2e/ -v` 실행 |
| 운영체제 | Linux (amd64/arm64) 또는 macOS | tmpfs 디렉터리 지원 필요 |
| root 권한 | 불필요 | mount(8), modprobe 없음 |
| 커널 모듈 | 불필요 | nvmet, nvme-tcp 모듈 없음 |
| 외부 프로세스 | 불필요 | zfs(8), nvme(8) 없음 |
| 네트워크 | loopback(127.0.0.1)만 사용 | 외부 네트워크 불필요 |
| 디스크 | 200 MiB 여유 공간 | `t.TempDir()` 임시 파일용 |

#### GitHub Actions 예시 (Mock Agent 모드)

```yaml
jobs:
  e2e-mock-agent:
    name: "In-Process E2E (Mock Agent)"
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.22"
          cache: true

      # mockAgentServer 사용 (E1-E8, E11-E17)
      - name: Run CSI Controller/Node E2E Tests
        run: |
          go test ./test/e2e/ -v -timeout 120s \
            -run "TestCSI|TestMTLS" \
            -count=1

      # agentE2EMockBackend 사용 (E9)
      - name: Run Agent gRPC E2E Tests
        run: |
          go test ./test/e2e/ -v -timeout 60s \
            -run "TestAgent" \
            -count=1
```

#### GitLab CI 예시 (Mock Agent 모드)

```yaml
e2e-mock-agent:
  stage: test
  image: golang:1.22-bookworm
  script:
    # 빌드 태그 없음 — mock agent 모드가 기본
    - go test ./test/e2e/ -v -timeout 180s -count=1
  artifacts:
    when: always
    reports:
      junit: e2e-mock-report.xml
```

#### 실행 확인

```bash
# 전체 인프로세스 E2E (mockAgentServer + agentE2EMockBackend 모두 포함)
go test ./test/e2e/ -v -timeout 180s

# mockAgentServer 사용 테스트만 실행
go test ./test/e2e/ -v -run "TestCSI|TestMTLS"

# agentE2EMockBackend 사용 테스트만 실행
go test ./test/e2e/ -v -run "TestAgent"

# 병렬 실행 (-parallel 플래그로 동시성 제어)
go test ./test/e2e/ -v -parallel 8 -timeout 180s

# 특정 테스트만 실행 (예: 오류 시나리오만)
go test ./test/e2e/ -v -run "TestCSIController_CreateVolume_Agent"
go test ./test/e2e/ -v -run "TestAgent_ErrorHandling"
```

---

### Mock Agent 모드에서 검증 가능한 항목과 불가능한 항목

#### ✅ CI Mock Agent 모드에서 검증 가능한 항목

| 항목 | 사용하는 Mock | 근거 |
|------|-------------|------|
| CSI CreateVolume이 agent.CreateVolume + ExportVolume을 올바른 인수로 호출 | mockAgentServer | 콜 기록 슬라이스 검증 |
| CSI DeleteVolume이 agent.UnexportVolume + DeleteVolume을 올바른 순서로 호출 | mockAgentServer | 콜 기록 + 호출 순서 검증 |
| agent 오류 코드가 CSI 상태 코드로 정확히 매핑됨 | mockAgentServer | `*Err` 필드 주입 |
| ExportVolumeInfo의 NQN/Address/Port가 VolumeContext로 전달됨 | mockAgentServer | `ExportVolumeInfo` 설정 |
| VolumeContext가 NodeStageVolume에 올바르게 전달됨 | mockAgentServer + mockConnector | 공유 VolumeStateMachine |
| PillarVolume CRD가 정확한 Phase/PartialFailure로 생성됨 | mockAgentServer | fake k8s client |
| agent.Server의 NQN 생성 로직 (`nqn.2026-01.com.bhyoo.pillar-csi:<pool>.<id>`) | agentE2EMockBackend | 실제 agent.Server 코드 실행 |
| ExportVolume 후 configfs 디렉터리/파일/심볼릭 링크 구조 | agentE2EMockBackend | `t.TempDir()` fake configfs |
| ReconcileState가 빈 configfs에서 볼륨 상태를 재구성 | agentE2EMockBackend | 재시작 시뮬레이션 |
| ZFS 백엔드 오류가 올바른 gRPC 상태 코드로 변환됨 | agentE2EMockBackend | `createErr`/`deleteErr` 주입 |
| AllowInitiator/DenyInitiator의 allowed_hosts 심볼릭 링크 | agentE2EMockBackend | 실제 agent.Server 코드 |
| mTLS 연결 (인증서 검증) | mockAgentServer + testcerts | 인메모리 TLS 인증서 |
| ControllerPublishVolume 멱등성 | mockAgentServer | 동일 요청 재호출 |
| 동시 CreateVolume 패닉/데드락 없음 | mockAgentServer | 병렬 고루틴 테스트 |

#### ❌ CI Mock Agent 모드에서 검증 불가능한 항목

| 항목 | 이유 | 대안 |
|------|------|------|
| 실제 ZFS zvol 생성/조회/삭제 | `zfs(8)` 명령 미실행 | F1 (`TestRealZFS_*`) |
| ZFS 풀 용량 실제 계산 | mock은 고정 값 반환 | F1 |
| 실제 NVMe-oF TCP 리스닝 시작 | `nvmet-tcp` 커널 모듈 없음 | F2 (`TestRealNVMeoF_Export`) |
| 실제 NVMe initiator 연결 | `nvme-tcp` 커널 모듈 없음 | F3 (`TestRealNVMeoF_Connect`) |
| 실제 `/dev/nvme*` 블록 장치 생성 | NVMe 드라이버 없음 | F3 |
| 실제 `mount(8)` / `umount(8)` | root 권한 불필요 → 시스템 콜 없음 | F8–F10 |
| 실제 `mkfs.ext4` / `mkfs.xfs` | 포맷 명령 없음 | F8 |
| 에이전트 → 컨트롤러 콜백 (gRPC 서버-to-클라이언트) | 현재 Phase 1은 단방향 | 해당 없음 |
| 실제 Kubernetes PVC 프로비저닝 흐름 | kubectl, external-provisioner 없음 | F4 |
| 에이전트 재시작 후 nvmet 커널 상태 손실 복구 | in-process 환경에서 커널 재시작 불가 | F6 |
| 대규모 볼륨 (100개 이상) 프로비저닝 지연 | mock 응답에 실제 처리 시간 없음 | F25 |
| pillar-agent 바이너리의 신호 처리 (SIGTERM) | in-process 환경 | F5 |

---

## 수동/스테이징 테스트 카탈로그 (Manual/Staging Tests)

> ⚠️ **F-번호 충돌 주의 (Level Coordinator 해결 필요):**
> 이 섹션과 위의 [유형 F: 완전 E2E 테스트](#유형-f-완전-e2e-테스트-full-e2e--표준-ci-불가) 섹션은
> **서로 다른 AC가 독립적으로 작성**되어 F-번호 체계가 충돌한다.
>
> | 섹션 | 빌드 태그 | F-번호 체계 | 특징 |
> |------|----------|------------|------|
> | 유형 F (위, line ~1385) | `//go:build e2e_full` | F1–F26 (F4=K8s PVC, F8=실제 마운트, ...) | agent.Server gRPC 통합 레벨 |
> | 이 섹션 (아래) | `//go:build hardware` | F1–F12 (F4=실제 마운트, F5=K8s PVC, ...) | backend/component 레벨, 6-필드 명세 완비 |
>
> **F1–F3 (ZFS, NVMe-oF configfs, NVMe-oF initiator)은 양쪽에 동일 함수명으로 존재하나 세부 내용이 다르다.**
> F4 이후부터 F-번호가 완전히 달라진다. 구현 시 두 스킴 중 하나로 통일해야 한다.

이 섹션은 **자동화된 CI 파이프라인에서 실행할 수 없는** 테스트 전체 목록을 정의한다.
각 테스트에 대해:

1. **자동화 불가 사유** — 왜 표준 CI/CD에서 실행이 불가능한지 구체적으로 설명
2. **필요 인프라** — 테스트 실행을 위해 갖춰야 할 실제 하드웨어 또는 스테이징 환경
3. **대안** — 자동화 테스트로 부분 대체 가능한 범위와 한계

> **경고:** 아래 테스트들은 **현재 구현(코드)이 존재하지 않는** 미래 테스트 계획이다.
> 각 테스트 함수명은 해당 기능을 구현할 때 사용할 예정인 이름이며,
> `//go:build hardware` 빌드 태그로 표준 `go test` 실행에서 제외될 것이다.

---

### 수동/스테이징 테스트 요약

| 카테고리 | 테스트 그룹 | 테스트 수 | 필요 환경 |
|---------|------------|----------|-----------|
| F1–F3 | 실제 ZFS 백엔드 동작 | 5 | Linux + ZFS 풀 + root |
| F4–F6 | 실제 NVMe-oF 타겟 / 이니시에이터 | 6 | Linux + nvmet-tcp + nvme-tcp 커널 모듈 + root |
| F7–F10 | 실제 마운트 / 파일시스템 | 5 | Linux + root + 블록 디바이스 |
| F11–F12 | Kubernetes PVC 프로비저닝 | 3 | Kind/실제 k8s + pillar-csi DaemonSet |
| F13–F15 | 에이전트 재시작 / 복구 | 4 | Linux + 실제 nvmet + pillar-agent 바이너리 |
| F16–F18 | 볼륨 데이터 마이그레이션 | 4 | 두 개 이상의 스토리지 노드 |
| F19–F21 | 파일시스템 리사이즈 | 3 | Linux + root + 실제 블록 디바이스 |
| F22–F23 | NVMe-oF Multipath | 3 | 다중 NIC + 다중 경로 구성 |
| F24 | 커널 레벨 동시성 / 레이스 컨디션 | 2 | Linux + stress 도구 + root |
| F25 | 대규모 확장성 | 2 | 대용량 스토리지 서버 (수 TB ZFS 풀) |
| F26 | 실제 PKI / cert-manager | 2 | 실제 Kubernetes 클러스터 + cert-manager |
| **합계** | | **39** | |

---

### 수동/스테이징 테스트 공통 인프라 요구사항

#### 스테이징 환경 최소 구성

아래 모든 F 계열 테스트를 실행하기 위한 **최소 스테이징 환경**이다.
이 환경은 표준 CI 서버로는 제공할 수 없다.

| 구성 요소 | 사양 | 비고 |
|-----------|------|------|
| **스토리지 노드** (최소 1대) | 베어메탈 또는 KVM 게스트 (중첩 가상화 금지) | ZFS + NVMe-oF 타겟 실행 |
| CPU | 4코어 이상 | ZFS ARC + NVMe-oF 처리 |
| RAM | 16 GiB 이상 | ZFS ARC 캐시 최소 4 GiB |
| 디스크 | 100 GiB 이상 빈 블록 디바이스 (SSD 권장) | ZFS 풀 생성용 (`/dev/sdb` 등) |
| OS | Ubuntu 22.04 LTS 또는 Debian 12 | ZFS DKMS 패키지 지원 |
| 커널 | 5.15+ | `nvmet`, `nvmet-tcp`, `nvme-tcp` 모듈 |
| ZFS | OpenZFS 2.2+ | `zfsutils-linux` 패키지 |
| 네트워크 | 1 GbE 이상 (NVMe-oF 멀티패스 테스트는 2 NIC) | 이니시에이터 노드와 통신 |
| **이니시에이터 노드** (F4-F12, 선택) | 별도 VM 또는 동일 호스트 다른 네임스페이스 | NVMe-oF 연결 테스트 |
| root 권한 | 필수 | `zfs`, `nvme`, `mount`, `modprobe` 실행 |
| **Kubernetes 클러스터** (F11–F12) | Kind v0.23+ 또는 실제 클러스터 (1 마스터 + 1 워커) | PVC 프로비저닝 테스트 |

#### 커널 모듈 로드 확인

```bash
# F4–F12 테스트 전 반드시 확인
modprobe nvmet
modprobe nvmet-tcp
modprobe nvme-tcp

lsmod | grep nvme
# nvme_tcp     ... nvmet_tcp
# nvme_core    ...
# nvmet        ...
# nvmet_tcp    ...
```

#### ZFS 풀 준비

```bash
# F1–F3 테스트 전 ZFS 풀 생성
# 주의: /dev/sdb는 테스트 전용 디스크여야 함
zpool create tank /dev/sdb
zpool status tank

# 테스트 완료 후 정리
zpool destroy tank
```

---

### F1: 실제 ZFS 백엔드 동작 검증

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `zfs(8)` 바이너리와 실제 ZFS 커널 모듈(`zfs.ko`)이 필요
- ZFS 풀 생성을 위한 빈 블록 디바이스(`/dev/sdb`)가 필요
- `root` 권한 필수 — 표준 CI 컨테이너는 unprivileged
- GitHub Actions의 `ubuntu-22.04` 러너에는 `zfsutils-linux`가 기본 설치되지 않음
  (설치 가능하지만 루프백 디바이스 기반 ZFS는 프로덕션 동작과 다름)

**필요 인프라:**
- ZFS 풀(`tank`)이 준비된 Linux 호스트
- `root` 권한
- `zfsutils-linux` (OpenZFS 2.2+)

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F1.1 | `TestRealZFS_CreateVolume` | 실제 `zfs create -V` 명령으로 zvol이 생성되고 `/dev/zvol/tank/<name>` 블록 디바이스가 나타남 | ZFS 풀 `tank` 마운트됨; root 권한; ZFS 모듈 로드 | 1) `backend.ZFSBackend.Create(ctx, "test-vol", 1GiB, params)` 호출; 2) `stat /dev/zvol/tank/test-vol` 확인 | 블록 디바이스 존재; 크기 1 GiB; `zfs list` 에 `tank/test-vol` 표시 | `ZFS` |
| F1.2 | `TestRealZFS_DeleteVolume` | 실제 `zfs destroy` 명령으로 zvol 삭제 후 블록 디바이스가 사라짐 | F1.1 성공 후 `tank/test-vol` 존재 | 1) `backend.ZFSBackend.Delete(ctx, "test-vol")` 호출; 2) `stat /dev/zvol/tank/test-vol` 확인 | 블록 디바이스 없음; `zfs list` 에 표시되지 않음 | `ZFS` |
| F1.3 | `TestRealZFS_ExpandVolume` | 실제 `zfs set volsize=2G` 명령으로 zvol 크기 확장 후 블록 디바이스 크기 변경 확인 | `tank/test-vol` (1 GiB) 존재 | 1) `backend.ZFSBackend.Expand(ctx, "test-vol", 2GiB)` 호출; 2) `blockdev --getsize64 /dev/zvol/tank/test-vol` 확인 | 블록 디바이스 크기 2 GiB | `ZFS` |
| F1.4 | `TestRealZFS_Capacity` | `zpool get free` / `zfs get available`로 실제 풀 가용 용량 조회 | ZFS 풀 `tank` 마운트됨; 여유 공간 있음 | 1) `backend.ZFSBackend.Capacity(ctx)` 호출 | `TotalBytes > 0`, `AvailableBytes > 0`, `TotalBytes >= AvailableBytes` | `ZFS` |
| F1.5 | `TestRealZFS_CreateVolume_WithParams` | `compression`, `dedup`, `sync` ZFS 파라미터가 실제로 zvol에 적용됨 | ZFS 풀 `tank` 마운트됨; root 권한 | 1) params에 `compression=lz4`, `sync=disabled` 설정 후 `Create` 호출; 2) `zfs get compression,sync tank/test-vol` 확인 | compression=lz4, sync=disabled로 설정됨 | `ZFS` |

**자동화 대체 범위:** F1.1–F1.5의 로직 흐름(파라미터 직렬화, 명령 구성, 오류 매핑)은
`test/component/zfs_test.go`의 `exec.Command` mock으로 검증된다.
그러나 **실제 블록 디바이스 생성/삭제**, **실제 용량 계산**, **ZFS 파라미터 적용**은
mock으로 검증할 수 없다.

---

### F2: 실제 NVMe-oF 타겟 configfs 동작 검증

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `/sys/kernel/config/nvmet/` 커널 configfs가 실제로 마운트되어야 함
- `nvmet` 커널 모듈이 로드되어야 configfs 항목이 생성됨
- `root` 권한 필수 (configfs 쓰기는 root만 가능)
- `t.TempDir()` 기반 fake configfs는 실제 커널의 파일에 쓰기 시 타겟 포트
  리스닝이 시작되는 **사이드이펙트**를 재현할 수 없음

**필요 인프라:**
- `nvmet` 커널 모듈이 로드된 Linux 호스트
- `root` 권한
- 사용 가능한 네트워크 인터페이스 (NVMe-oF TCP 리스닝용)

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F2.1 | `TestRealNVMeoF_Export` | `ExportVolume` 호출 후 `/sys/kernel/config/nvmet/` 에 subsystem/namespace/port 항목이 실제로 생성되고 NQN으로 접근 가능한 타겟 포트가 리스닝 시작 | `nvmet`, `nvmet-tcp` 모듈 로드; `tank/test-vol` zvol 존재; root 권한; configfs 마운트됨 | 1) `agent.ExportVolume(ctx, "tank/test-vol", nqn, port)` 호출; 2) `ls /sys/kernel/config/nvmet/subsystems/` 확인; 3) `nvme discover -t tcp -a 127.0.0.1 -s <port>` 실행 | configfs 항목 존재; `nvme discover` 출력에 NQN 표시 | `NVMeF`, `Agent` |
| F2.2 | `TestRealNVMeoF_Unexport` | `UnexportVolume` 호출 후 configfs 항목이 완전히 제거되고 포트 리스닝이 중단됨 | F2.1 성공 후 타겟 포트 리스닝 중 | 1) `agent.UnexportVolume(ctx, nqn)` 호출; 2) `ls /sys/kernel/config/nvmet/subsystems/` 확인; 3) `nvme discover -t tcp -a 127.0.0.1 -s <port>` 실행 | configfs 항목 없음; `nvme discover` 오류 또는 NQN 미표시 | `NVMeF`, `Agent` |
| F2.3 | `TestRealNVMeoF_AllowInitiator` | `AllowInitiator` 호출로 `allowed_hosts` 심볼릭 링크가 실제 configfs에 생성됨 | F2.1 성공 후 타겟 존재 | 1) `agent.AllowInitiator(ctx, nqn, hostnqn)` 호출; 2) `ls /sys/kernel/config/nvmet/subsystems/<nqn>/allowed_hosts/` 확인 | 심볼릭 링크 존재; `hostnqn` 이름의 항목 표시 | `NVMeF`, `Agent` |
| F2.4 | `TestRealNVMeoF_DenyInitiator` | `DenyInitiator` 호출로 `allowed_hosts` 심볼릭 링크가 제거됨 | F2.3 성공 후 허용된 이니시에이터 존재 | 1) `agent.DenyInitiator(ctx, nqn, hostnqn)` 호출; 2) `ls /sys/kernel/config/nvmet/subsystems/<nqn>/allowed_hosts/` 확인 | 심볼릭 링크 없음 | `NVMeF`, `Agent` |

**자동화 대체 범위:** F2.1–F2.4의 configfs 파일 경로 구성, 심볼릭 링크 생성 로직,
오류 처리는 `test/e2e/agent_e2e_test.go`의 `t.TempDir()` 기반 환경에서
**파일 시스템 구조**만 검증된다. 그러나 **커널이 configfs 항목을 읽어
실제 타겟을 활성화하는 동작**, **NQN으로의 실제 접근 가능 여부**는 검증하지 못한다.

---

### F3: 실제 NVMe-oF 이니시에이터 연결 검증

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `nvme-tcp` 이니시에이터 커널 모듈 필요
- 실제 NVMe-oF 타겟(F2 환경)이 리스닝 중이어야 함
- 실제 `/dev/nvme*` 블록 디바이스가 생성되어야 검증 가능
- `nvme connect` 실행에 `root` 권한 필요

**필요 인프라:**
- F2 환경 (타겟 측)
- `nvme-tcp` 모듈이 로드된 이니시에이터 Linux 호스트
- 두 호스트 간 TCP 네트워크 통신 가능 (또는 동일 호스트 루프백)
- `nvme-cli` 패키지

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F3.1 | `TestRealNVMeoF_Connect` | `connector.Connect` 호출 후 실제 `/dev/nvme*` 블록 디바이스가 이니시에이터 호스트에 나타남 | F2.1 타겟 리스닝 중; `nvme-tcp` 모듈 로드; root 권한 | 1) `connector.Connect(ctx, nqn, addr, port)` 호출; 2) `nvme list` 출력에서 디바이스 확인; 3) `stat /dev/nvme0n1` 또는 유사 경로 확인 | `/dev/nvme*` 디바이스 존재; `nvme list`에 NQN 표시 | `Conn` |
| F3.2 | `TestRealNVMeoF_Disconnect` | `connector.Disconnect` 호출 후 `/dev/nvme*` 블록 디바이스가 사라짐 | F3.1 성공 후 디바이스 존재 | 1) `connector.Disconnect(ctx, nqn)` 호출; 2) `nvme list` 확인 | 디바이스 없음 | `Conn` |
| F3.3 | `TestRealNVMeoF_FullStoragePath` | 실제 타겟(F2.1) + 실제 이니시에이터(F3.1) + 실제 마운트(F7)의 전체 스토리지 경로 동작 확인 | F2 타겟 환경 + F3 이니시에이터 환경 + F7 마운트 환경 | 1) ZFS zvol 생성; 2) NVMe-oF export; 3) NVMe-oF connect; 4) mkfs.ext4; 5) mount; 6) 파일 I/O (쓰기/읽기/fsync); 7) umount; 8) nvme disconnect; 9) unexport; 10) zvol 삭제 | 각 단계 성공; 파일 I/O 데이터 일관성 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |

**자동화 대체 범위:** `mockCSIConnector`가 `Connect/Disconnect` 호출 기록을
검증하지만 실제 **커널 드라이버 수준 동작**, **블록 디바이스 생성**, **데이터 I/O**는
검증하지 못한다.

---

### F4: 실제 마운트 / 파일시스템 포맷 검증

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `mount(8)`, `umount(8)` 시스템 콜에 `root` 권한 필요
- `mkfs.ext4`, `mkfs.xfs` 실행에 실제 블록 디바이스 필요
- GitHub Actions 컨테이너에서 mount 시스템 콜은 unprivileged namespace에서 차단됨
- 마운트 완료 후 파일 I/O 검증을 위한 실제 블록 디바이스 필요

**필요 인프라:**
- 실제 `/dev/nvme*` 블록 디바이스 (또는 루프백 디바이스)
- `root` 권한
- `e2fsprogs`, `xfsprogs` 패키지

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F4.1 | `TestRealMount_FormatAndMount_ext4` | `mounter.FormatAndMount`가 실제 `mkfs.ext4` + `mount -t ext4`를 실행하고 마운트 포인트에서 파일 쓰기/읽기 가능 | `/dev/loop0` 등 루프백 또는 `/dev/nvme0n1` 블록 디바이스; root 권한; 스테이징 디렉터리 | 1) `mounter.FormatAndMount(device, stagingPath, "ext4", nil)` 호출; 2) 마운트 포인트에 파일 쓰기; 3) `umount`; 4) 재마운트 후 파일 존재 확인 | 마운트 성공; 데이터 지속성 확인; `mount` 명령 출력에 ext4 타입 표시 | `Mnt` |
| F4.2 | `TestRealMount_FormatAndMount_xfs` | F4.1과 동일하지만 xfs 파일시스템 | F4.1과 동일 | 1) `mounter.FormatAndMount(device, stagingPath, "xfs", nil)` 호출; 2–4) F4.1과 동일 | 마운트 성공; xfs 타입; 데이터 지속성 | `Mnt` |
| F4.3 | `TestRealMount_BindMount` | `mounter.Mount`가 `--bind` 옵션으로 스테이징 경로를 Pod 볼륨 경로에 바인드 마운트 | F4.1 성공 후 스테이징 경로 마운트됨 | 1) `mounter.Mount(stagingPath, targetPath, "", ["bind"])` 호출; 2) targetPath에서 파일 확인 | 바인드 마운트 성공; 파일 접근 가능 | `Mnt` |
| F4.4 | `TestRealMount_Unmount` | `mounter.Unmount`가 마운트 해제 후 마운트 포인트 정리 | F4.3 성공 후 바인드 마운트됨 | 1) `mounter.Unmount(targetPath)` 호출; 2) targetPath 디렉터리 확인 | 마운트 해제됨; `/proc/mounts`에 항목 없음 | `Mnt` |
| F4.5 | `TestRealMount_NodeStage_NodePublish_Integration` | 실제 블록 디바이스에서 NodeStageVolume(포맷+마운트) → NodePublishVolume(바인드 마운트) → NodeUnpublishVolume → NodeUnstageVolume 전체 흐름 | F3.1 성공 후 `/dev/nvme0n1` 존재; root 권한 | 1) NodeStageVolume(ext4, 스테이징); 2) NodePublishVolume(바인드, Pod 경로); 3) Pod 경로에서 파일 I/O; 4) NodeUnpublishVolume; 5) NodeUnstageVolume | 각 단계 성공; 파일 I/O 가능; 정리 후 디바이스 해제 | `CSI-N`, `Conn`, `Mnt`, `State` |

**자동화 대체 범위:** `mockCSIMounter`가 호출 기록을 추적하지만 실제
**마운트 시스템 콜**, **파일시스템 포맷**, **데이터 I/O 일관성**은 검증하지 못한다.

---

### F5: Kubernetes PVC 프로비저닝 — 실제 external-provisioner 흐름

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `external-provisioner` 사이드카가 CSI Controller에 `CreateVolume`을 실제로 호출하는
  Kubernetes 오케스트레이션 흐름을 재현하려면 실제(또는 Kind) 클러스터 필요
- PVC → PV 바인딩은 `kube-controller-manager`가 담당하며 fake client로 재현 불가
- `external-attacher`, `external-resizer` 사이드카와의 상호작용 검증 불가
- CSI Driver 등록(`CSIDriver` 오브젝트)과 플러그인 소켓 통신은 실제 노드 필요

**필요 인프라:**
- Kind v0.23+ 또는 실제 Kubernetes 1.29+ 클러스터
- pillar-csi DaemonSet 배포 (CSI Node Plugin + CSI Controller Plugin)
- 실제 스토리지 백엔드 (F1 ZFS + F2 NVMe-oF 타겟)
- `external-provisioner`, `external-attacher`, `external-resizer` 사이드카

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F5.1 | `TestK8sPVC_ProvisionAndAttach` | PVC 생성 → PV 동적 프로비저닝 → Pod에 마운트 → 데이터 쓰기/읽기 전체 흐름 | Kind 클러스터; pillar-csi 설치; PillarTarget CRD 등록; ZFS+NVMe-oF 백엔드; StorageClass `pillar-csi.bhyoo.com` | 1) `kubectl apply -f pvc.yaml` (1 GiB); 2) PVC Bound 대기 (Eventually 2분); 3) `kubectl apply -f pod-with-pvc.yaml`; 4) Pod Running 대기; 5) Pod 내에서 `echo test > /data/test.txt`; 6) Pod 재시작 후 파일 존재 확인 | PVC Bound; Pod Running; 파일 지속성; PillarVolume CRD 생성 확인 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `VolCRD`, `TgtCRD` |
| F5.2 | `TestK8sPVC_DeleteAndUnprovision` | PVC 삭제 → PV 삭제 → ZFS zvol 삭제 → NVMe-oF 타겟 해제 전체 정리 흐름 | F5.1 성공 후 PVC/Pod 존재 | 1) `kubectl delete pod`; 2) `kubectl delete pvc`; 3) PV 삭제 대기; 4) `zfs list` 에서 zvol 없음 확인; 5) configfs에서 타겟 없음 확인 | 모든 리소스 삭제; PillarVolume CRD 삭제; 스토리지 누수 없음 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `NVMeF`, `VolCRD` |
| F5.3 | `TestK8sPVC_Resize` | PVC 크기 확장 요청 → `external-resizer`가 `ControllerExpandVolume` + `NodeExpandVolume` 호출 → 마운트된 파일시스템 온라인 확장 | F5.1 성공 후 PVC Bound; StorageClass `allowVolumeExpansion: true` | 1) `kubectl patch pvc` (1 GiB → 2 GiB); 2) PVC `status.capacity.storage=2Gi` 대기; 3) Pod 내에서 `df -h /data` 확인 | 파일시스템 크기 2 GiB; 데이터 손실 없음 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Mnt` |

**자동화 대체 범위:**
- `TestCSIController_ControllerExpandVolume` (E2.3): ControllerExpandVolume RPC 로직
- `TestCSILifecycle_*` (E4): CreateVolume→Delete 전체 CSI 내부 흐름
- 그러나 **실제 PVC/Pod 오케스트레이션**, **external-provisioner 상호작용**,
  **마운트된 파일시스템의 온라인 확장**은 자동화 테스트로 검증 불가

---

### F6: pillar-agent 바이너리 재시작 / 복구

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `ReconcileState`가 실제로 복구할 `nvmet` 커널 상태 손실 시나리오를 재현하려면
  실제 `nvmet` 커널 모듈과 configfs가 필요
- in-process 테스트에서는 프로세스 재시작 시뮬레이션이 불가 (`t.TempDir()`는
  커널 상태와 무관하게 항상 빈 상태)
- SIGTERM/SIGKILL 수신 후 graceful shutdown 동작 검증은 실제 OS 프로세스 필요

**필요 인프라:**
- F2 환경 (실제 nvmet configfs)
- pillar-agent 바이너리 (`make build-agent`)
- root 권한

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F6.1 | `TestRealAgent_ReconcileState_AfterRestart` | pillar-agent 프로세스 재시작 후 `ReconcileState`가 configfs의 기존 nvmet 항목을 감지하고 내부 상태를 복원 | F2.1 타겟 configfs 존재; pillar-agent 프로세스 1회 종료; configfs 항목 유지 | 1) agent 실행 중 ExportVolume 호출; 2) agent 프로세스 SIGTERM으로 종료; 3) configfs 항목 수동 확인 (여전히 존재); 4) agent 프로세스 재시작; 5) ReconcileState 완료 대기; 6) GetVolume으로 상태 조회 | GetVolume이 올바른 상태 반환; 재시작 후 UnexportVolume 정상 동작 | `NVMeF`, `Agent` |
| F6.2 | `TestRealAgent_GracefulShutdown_SIGTERM` | SIGTERM 수신 시 진행 중인 RPC를 완료한 후 종료 (configfs 항목 유지) | pillar-agent 실행 중; 진행 중인 CreateVolume RPC | 1) CreateVolume RPC 시작 (장기 실행); 2) 즉시 SIGTERM 전송; 3) agent 로그 확인 | 진행 중인 RPC 완료 후 종료; configfs 항목 손상 없음; 오류 로그 없음 | `Agent` |
| F6.3 | `TestRealAgent_SIGKILL_DataIntegrity` | SIGKILL 후 재시작 시 데이터 일관성 — 불완전한 configfs 항목 감지 및 정리 | pillar-agent 실행 중; CreateVolume RPC 처리 중간에 SIGKILL | 1) CreateVolume RPC 시작; 2) 처리 중간에 SIGKILL; 3) agent 재시작; 4) ReconcileState 관찰 | 불완전한 항목이 감지되어 정리되거나 오류와 함께 복구 불가 상태 보고; 데이터 손상 없음 | `NVMeF`, `Agent` |
| F6.4 | `TestRealAgent_NodeReboot_StateRecovery` | 노드 재부팅 후 커널 모듈 재로드 + configfs 재생성을 통한 완전 복구 | F2.1 타겟 configfs 존재; 노드 재부팅 가능한 스테이징 환경 | 1) ExportVolume으로 타겟 생성; 2) 노드 재부팅; 3) 커널 모듈 재로드; 4) agent 재시작; 5) configfs 재생성 (ReconcileState + 재export); 6) nvme discover로 타겟 접근 확인 | 노드 재부팅 후 스토리지 경로 복원; PVC 접근 가능 | `NVMeF`, `Agent`, `ZFS` |

**자동화 대체 범위:**
- `TestAgent_E9.4_ReconcileState_RestoredFromConfigfs` (E9): in-process 환경에서
  `t.TempDir()` configfs를 사전 설정하여 ReconcileState 로직 검증 가능
- 그러나 **실제 커널 nvmet 상태 손실/복구**, **프로세스 재시작 동작**, **노드 재부팅**은
  자동화 테스트로 검증 불가

---

### F7: 실제 cert-manager PKI 통합

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- cert-manager의 실제 인증서 발급(CA 서명, ACME 등)은 실제 Kubernetes 클러스터와
  cert-manager 컨트롤러 배포가 필요
- mTLS 연결 테스트(`TestMTLSController_*`)는 `testcerts` 인메모리 인증서를 사용하므로
  cert-manager 발급 인증서의 회전(rotation), 만료(expiry), 갱신(renewal) 검증 불가
- `caBundle` 인젝션이 `ValidatingWebhookConfiguration`에 실제로 반영되는지는
  `cert-manager` + `ca-injector`가 실행 중인 클러스터에서만 검증 가능

**필요 인프라:**
- Kind v0.23+ 또는 실제 Kubernetes 1.29+ 클러스터
- cert-manager v1.14+ 설치
- pillar-csi 컨테이너 이미지 (`make docker-build`)

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F7.1 | `TestRealCertManager_CertificateIssuance` | cert-manager가 `webhook-server-cert` Secret을 발급하고 pillar-csi 웹훅 서버가 해당 인증서로 TLS를 제공 | Kind 클러스터; cert-manager 설치; pillar-csi 배포 | 1) cert-manager 설치 확인; 2) `webhook-server-cert` Secret 생성 대기 (Eventually 5분); 3) `openssl s_client -connect pillar-csi-webhook-service:443` 으로 인증서 체인 확인 | 인증서 발급됨; Common Name 올바름; 인증서 체인 유효 | `mTLS` |
| F7.2 | `TestRealCertManager_CertificateRotation` | cert-manager가 만료 임박 인증서를 자동으로 갱신하고 pillar-csi가 새 인증서로 재로드 | F7.1 성공 후; 인증서 만료 기간 단축 설정 (e.g., 5분) | 1) 만료 기간 5분의 Certificate 생성; 2) 만료 직전 대기 (4분); 3) cert-manager 갱신 트리거; 4) 새 인증서 시리얼 넘버 확인; 5) 웹훅 호출 가능 확인 | 새 인증서로 갱신됨; 웹훅 서비스 중단 없음 | `mTLS` |

**자동화 대체 범위:**
- `TestMTLSController_*` (E8): `testcerts` 인메모리 인증서로 mTLS 연결 로직 검증
- 그러나 **cert-manager 발급 인증서 체인 검증**, **자동 갱신 동작**은
  자동화 테스트로 검증 불가

---

### F8: 파일시스템 온라인 리사이즈

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `resize2fs`, `xfs_growfs` 명령 실행에 실제 마운트된 블록 디바이스 필요
- NodeExpandVolume은 `NodeExpandSecret` 처리와 함께 실제 마운트된 경로를 요구
- 파일시스템 확장 결과 검증(`df -h`)을 위해 실제 마운트 상태 필요

**필요 인프라:**
- F4 환경 (마운트된 블록 디바이스)
- `e2fsprogs`, `xfsprogs` 패키지
- root 권한

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F8.1 | `TestRealResize_NodeExpandVolume_ext4` | NodeExpandVolume 호출 후 `resize2fs`가 실행되어 마운트된 ext4 파일시스템 크기가 확장됨 | ext4 파일시스템이 스테이징 경로에 마운트됨; ZFS zvol이 2 GiB로 확장됨 | 1) `nodeServer.NodeExpandVolume(ctx, req)` 호출 (VolumePath=스테이징경로); 2) `df -h <스테이징경로>` 확인 | 파일시스템 크기 2 GiB; 데이터 손실 없음 | `CSI-N`, `Mnt` |
| F8.2 | `TestRealResize_NodeExpandVolume_xfs` | F8.1과 동일하지만 xfs (`xfs_growfs`) | F8.1과 동일하지만 xfs | 동일 | 파일시스템 크기 확장됨 | `CSI-N`, `Mnt` |
| F8.3 | `TestRealResize_FullPipeline` | ZFS zvol 확장(F1.3) → ControllerExpandVolume → NodeExpandVolume → 파일시스템 온라인 확장 전체 파이프라인 | F1.3 + F4.1 환경; PVC 마운트됨 | 1) `zfs set volsize=2G`; 2) ControllerExpandVolume(2 GiB); 3) NodeExpandVolume; 4) `df -h` 확인 | 전체 파이프라인 성공; 파일시스템 2 GiB | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Mnt` |

**자동화 대체 범위:**
- `TestCSIExpand_*` (E11): ControllerExpandVolume + NodeExpandVolume RPC 로직,
  인수 검증, agent 호출은 mock 환경에서 검증
- 그러나 **실제 `resize2fs`/`xfs_growfs` 실행**, **파일시스템 크기 변경**은
  자동화 테스트로 검증 불가

---

### F9: NVMe-oF Multipath / 고가용성

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- NVMe-oF multipath는 **실제 다중 NIC 또는 다중 경로**를 요구하며,
  커널 NVMe multipath 드라이버 지원이 필요
- 경로 장애(path failure) 시뮬레이션은 실제 네트워크 인터페이스를 down시켜야 하므로
  root 권한 + 실제 NIC 필요
- `mockCSIConnector`는 단일 경로만 시뮬레이션하므로 multipath 시나리오 불가

**필요 인프라:**
- 다중 NIC가 장착된 스토리지 노드 (또는 VLAN 설정)
- F2 + F3 환경
- Linux multipath 도구 (`nvme-cli` multipath 지원 빌드)

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F9.1 | `TestRealNVMeoF_Multipath_Connect` | 두 경로(주 경로 + 보조 경로)로 NVMe-oF 연결 후 multipath 디바이스(`/dev/nvme0c0n1` 등) 확인 | 다중 NIC; nvmet 다중 포트 설정; nvme-tcp 모듈 | 1) 주 경로로 `nvme connect`; 2) 보조 경로로 `nvme connect`; 3) `nvme list` 에서 multipath 확인 | 두 경로 모두 연결됨; multipath 디바이스 노출 | `Conn` |
| F9.2 | `TestRealNVMeoF_Multipath_Failover` | 주 경로 장애 시 I/O가 보조 경로로 자동 전환되고 데이터 손실 없음 | F9.1 성공 후 I/O 진행 중 | 1) 주 NIC `ip link set down`; 2) I/O 지속 확인 (fio); 3) NIC 복원; 4) 두 경로 재확인 | I/O 중단 없음 (또는 단기 지연); 데이터 일관성; 경로 복원 후 두 경로 재활성화 | `Conn` |
| F9.3 | `TestRealNVMeoF_Multipath_AllPaths_Down` | 모든 경로 장애 시 I/O 오류가 적절히 상위로 전파 | F9.1 성공 후 | 1) 모든 NIC `ip link set down`; 2) I/O 시도 | I/O 오류; 타임아웃 이내에 오류 반환 | `Conn` |

**자동화 대체 범위:** mockCSIConnector는 단일 경로만 시뮬레이션한다.
NVMe-oF multipath는 완전히 수동 테스트에 의존한다.

---

### F10: 볼륨 데이터 마이그레이션 (SendVolume / ReceiveVolume)

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `SendVolume`/`ReceiveVolume`은 스트리밍 gRPC RPC로 실제 ZFS 스냅샷 데이터를
  네트워크로 전송 → 두 개 이상의 스토리지 노드 필요
- 실제 ZFS `zfs send | zfs receive` 파이프라인은 실제 ZFS 데이터가 있어야 검증 가능
- 대용량 데이터 전송 성능(예: 100 GiB) 검증은 실제 스토리지 하드웨어 필요

**필요 인프라:**
- 스토리지 노드 2대 (송신 측 + 수신 측)
- 양쪽 노드에 F1 ZFS 환경 구성
- 두 노드 간 TCP 네트워크 연결
- root 권한

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F10.1 | `TestRealMigration_SendReceiveVolume_Small` | 소용량(1 GiB) ZFS 스냅샷을 두 노드 간 `SendVolume`/`ReceiveVolume`으로 전송하고 수신 측에서 데이터 일관성 확인 | 송신 노드에 `tank/source` zvol (1 GiB, 데이터 채움); 수신 노드에 빈 ZFS 풀; 양쪽에 pillar-agent 실행 | 1) 송신 측 `zfs snapshot tank/source@snap1`; 2) 송신 측 `agent.SendVolume(ctx, snap)` → 스트리밍 RPC; 3) 수신 측 `agent.ReceiveVolume(ctx, stream)` → 수신; 4) 수신 측 `zfs list tank/dest` 확인; 5) SHA256 체크섬 비교 | 전송 성공; 수신 측 zvol 존재; 체크섬 일치 | `ZFS`, `Agent`, `gRPC` |
| F10.2 | `TestRealMigration_SendReceiveVolume_Incremental` | 증분 ZFS 스냅샷 전송 (`zfs send -i`) — 델타만 전송 확인 | F10.1 성공 후 수신 측에 베이스 스냅샷 존재; 송신 측에 변경 사항 추가 후 두 번째 스냅샷 | 1) 송신 측 추가 데이터 쓰기; 2) `zfs snapshot tank/source@snap2`; 3) 증분 SendVolume (snap1 → snap2); 4) 수신 측에서 확인 | 증분 전송 성공; 전체 전송 대비 빠름; 수신 측 최신 데이터 포함 | `ZFS`, `Agent`, `gRPC` |
| F10.3 | `TestRealMigration_SendReceiveVolume_NetworkInterruption` | 전송 중 네트워크 단절 시 올바른 오류 반환 및 부분 수신 데이터 정리 | F10.1 설정; 전송 중간에 네트워크 차단 가능한 환경 | 1) SendVolume/ReceiveVolume 시작; 2) 전송 50% 지점에서 네트워크 차단 (`tc qdisc add ... loss 100%`); 3) 양쪽 agent 오류 확인 | 오류 반환; 수신 측 부분 데이터 정리; 재시도 가능한 상태 | `ZFS`, `Agent`, `gRPC` |
| F10.4 | `TestRealMigration_LargeVolume_Performance` | 100 GiB ZFS 볼륨 전송 성능 기준점 측정 (최소 200 MB/s 이상) | 대용량 스토리지 서버 2대; 1 GbE 이상 네트워크 | 1) 100 GiB zvol 생성 및 데이터 채움; 2) 전송 시작; 3) 소요 시간 측정 | 200 MB/s 이상; 전송 완료; 데이터 일관성 | `ZFS`, `Agent`, `gRPC` |

**자동화 대체 범위:** `SendVolume`/`ReceiveVolume` RPC 인터페이스 정의 및
gRPC 스트리밍 직렬화는 unit test로 검증 가능하나, 실제 데이터 전송은 수동 테스트에 의존한다.

---

### F11: 커널 레벨 동시성 / 레이스 컨디션

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- `flock(2)` 기반 파일 잠금의 실제 커널 레벨 동작은 실제 파일시스템 필요
- 커널 드라이버 레벨 레이스 컨디션(configfs 동시 쓰기, nvmet 연결 경합)은
  실제 커널 스택이 없으면 재현 불가
- `go test -race` 는 Go 메모리 모델 레이스만 감지하며, 커널-사용자 공간 경계 레이스는 불가

**필요 인프라:**
- F2 + F3 환경
- `stress-ng`, `fio` 부하 생성 도구
- root 권한

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F11.1 | `TestRealConcurrent_MultipleExportSameNQN` | 두 goroutine이 동일 NQN으로 동시에 ExportVolume 호출 시 configfs 충돌 없음 (정확히 한 번 성공 또는 멱등성) | F2 환경; nvmet 로드; root 권한 | 1) 10개 goroutine이 동일 NQN으로 동시 ExportVolume 호출 | 정확히 1개 성공 또는 모두 성공(멱등); configfs 손상 없음; panic 없음 | `NVMeF`, `Agent` |
| F11.2 | `TestRealConcurrent_ConfigfsWrite_Stress` | configfs에 대한 집중적인 동시 쓰기 시 커널 오류 없음 | nvmet 로드; root 권한 | 1) 100개 goroutine이 서로 다른 NQN으로 동시 ExportVolume/UnexportVolume 교차 실행; 2) `dmesg` 오류 확인 | 모든 RPC 완료; 커널 오류 없음 (`dmesg -T | grep -i error`) | `NVMeF`, `Agent` |

**자동화 대체 범위:**
- `TestCSIConcurrent_*` (E16): Go 애플리케이션 레벨 동시성 (패닉, 데드락)은 검증
- 그러나 **커널 configfs 동시 쓰기 안전성**, **nvmet 드라이버 내 경합**은
  수동 테스트에 의존

---

### F12: 대규모 확장성 (볼륨 100개 이상)

**카테고리:** 수동/스테이징 ❌ CI 불가

**빌드 태그:** `//go:build hardware`

**자동화 불가 사유:**
- 100개 이상의 PVC 동시 생성 시 실제 처리 시간 (ZFS zvol 생성, nvmet 설정 시간)은
  mock으로 재현 불가
- 실제 Kubernetes 클러스터의 `etcd` 처리량, `external-provisioner` 큐 깊이 등
  실제 오케스트레이션 부하가 필요
- CI 환경의 제한된 디스크/메모리 용량으로는 100개 볼륨 프로비저닝 불가

**필요 인프라:**
- 대용량 스토리지 서버 (100 GiB+ 여유 ZFS 풀)
- F5 Kubernetes 클러스터 환경
- 충분한 CPU/RAM (32 GiB+ RAM 권장)

---

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| F12.1 | `TestRealScale_100PVC_ProvisionAll` | 100개 PVC 동시 생성 요청 시 모두 Bound 상태로 완료되고 처리 시간 측정 | K8s 클러스터; 100 GiB+ ZFS 풀; pillar-csi DaemonSet | 1) `kubectl apply -f 100-pvcs.yaml`; 2) 모든 PVC Bound 대기 (Eventually 10분); 3) 총 소요 시간 기록 | 100개 모두 Bound; 10분 이내 완료; 스토리지 누수 없음 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `VolCRD` |
| F12.2 | `TestRealScale_100PVC_DeleteAll` | F12.1 이후 100개 PVC 동시 삭제 시 모두 정리되고 ZFS zvol 없음 확인 | F12.1 성공 후 100개 PVC Bound | 1) `kubectl delete -f 100-pvcs.yaml`; 2) PVC 없음 대기; 3) `zfs list \| wc -l` 확인 | 모든 PVC/PV 삭제; ZFS zvol 없음 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `VolCRD` |

**자동화 대체 범위:**
- `TestCSIConcurrent_*` (E16): 동시 RPC 호출에서 Go 레벨 경합 없음은 검증
- 그러나 **실제 처리 시간**, **Kubernetes etcd 부하**, **스토리지 서버 부하**는 수동 테스트에 의존

---

### 수동/스테이징 테스트 실행 가이드

#### 환경 준비 체크리스트

```bash
# 1. 커널 모듈 확인
lsmod | grep -E "nvmet|zfs"

# 2. ZFS 풀 준비
zpool status tank

# 3. 빌드 태그 확인
go test -list '.' -tags hardware ./...

# 4. root 권한 확인
id  # uid=0 확인

# 5. 네트워크 확인 (NVMe-oF 멀티패스 테스트)
ip link show | grep -E "eth|ens|enp"
```

#### 개별 카테고리 실행

```bash
# F1: ZFS 백엔드 테스트만
go test ./... -tags hardware -run 'TestRealZFS_' -v

# F2: NVMe-oF 타겟 테스트만
go test ./... -tags hardware -run 'TestRealNVMeoF_Export|TestRealNVMeoF_Unexport|TestRealNVMeoF_Allow' -v

# F3: NVMe-oF 이니시에이터 연결 테스트만
go test ./... -tags hardware -run 'TestRealNVMeoF_Connect|TestRealNVMeoF_Disconnect' -v

# F4: 마운트 테스트만 (root 필수)
sudo go test ./... -tags hardware -run 'TestRealMount_' -v

# F5: Kubernetes PVC 테스트 (kubeconfig 설정 필요)
KUBECONFIG=/path/to/kubeconfig go test ./... -tags hardware -run 'TestK8sPVC_' -v

# F6: agent 재시작 복구 테스트
go test ./... -tags hardware -run 'TestRealAgent_' -v

# F3.3: 전체 스토리지 경로 통합 테스트 (F1+F2+F3+F4 환경 필요)
sudo go test ./... -tags hardware -run 'TestRealNVMeoF_FullStoragePath' -v -timeout 600s
```

#### 테스트 후 정리

```bash
# ZFS zvol 정리
zfs list | grep test | awk '{print $1}' | xargs -I{} zfs destroy {}

# configfs 정리
ls /sys/kernel/config/nvmet/subsystems/ | \
  grep pillar | while read nqn; do
    # allowed_hosts 링크 삭제
    ls /sys/kernel/config/nvmet/subsystems/$nqn/allowed_hosts/ | \
      xargs -I{} rm /sys/kernel/config/nvmet/subsystems/$nqn/allowed_hosts/{}
    # namespace 삭제
    ls /sys/kernel/config/nvmet/subsystems/$nqn/namespaces/ | \
      xargs -I{} rmdir /sys/kernel/config/nvmet/subsystems/$nqn/namespaces/{}
    # subsystem 삭제
    rmdir /sys/kernel/config/nvmet/subsystems/$nqn
  done

# NVMe 연결 해제
nvme disconnect-all
```

---

### 수동/스테이징 테스트 자동화 여부 결정 기준

아래 기준으로 테스트를 수동/스테이징 카테고리에 분류했다:

| 결정 기준 | 설명 | 해당 테스트 |
|---------|------|-----------|
| **커널 모듈 필수** | `nvmet`, `nvme-tcp`, `zfs` 등 커널 모듈 로드가 필요한 경우 | F2, F3, F9, F11 |
| **root 권한 필수** | `mount(8)`, `zfs(8)`, `modprobe` 등 root 권한이 필요한 경우 | F1, F2, F3, F4, F8 |
| **실제 블록 디바이스** | `/dev/zvol/*`, `/dev/nvme*` 등 실제 블록 디바이스가 필요한 경우 | F1, F3, F4, F8 |
| **다중 물리 노드** | 송신/수신 스토리지 노드가 분리되어야 하는 경우 | F10 |
| **실제 오케스트레이션** | `external-provisioner`, `kube-controller-manager` 등 실제 K8s 컴포넌트가 필요한 경우 | F5 |
| **프로세스 재시작** | 실제 OS 프로세스를 종료하고 재시작해야 하는 경우 | F6 |
| **노드 재부팅** | 물리/가상 노드 전체 재부팅이 필요한 경우 | F6.4 |
| **대용량 리소스** | 수십 GiB 이상의 실제 스토리지 용량이 필요한 경우 | F10.4, F12 |
| **실제 PKI** | cert-manager 등 실제 인증서 발급 인프라가 필요한 경우 | F7 |

**원칙:** 위 기준 중 하나라도 해당하면 수동/스테이징 카테고리로 분류한다.
자동화 가능한 부분(RPC 로직, 파라미터 직렬화, 오류 매핑)은 E 계열(인프로세스 E2E)
또는 component 테스트로 최대한 커버한다.

---

