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

**총 테스트 케이스: 137** (인프로세스 134개 + 클러스터 레벨 3개)

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
| E2 | CSI Controller — Publish / Unpublish / Expand / Validate | 8 | `TestCSIController_ControllerPublish*`, `TestCSIController_ControllerExpand*`, `TestCSIController_Validate*` |
| E3 | CSI Node — Stage / Publish / Unstage / Unpublish (E3.1–E3.15) | 34 | `TestCSINode_*` |
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
| **합계** | | **134** | |

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

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 88 | `TestCSIExpand_ControllerExpandVolume_ForwardsToAgent` | ControllerExpandVolume이 agent.ExpandVolume을 올바른 VolumeId·BackendType·RequestedBytes로 호출하고 node_expansion_required=true를 반환 | VolumeId="storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-expand"; CapacityRange.RequiredBytes=2GiB; mockAgentServer.ExpandVolumeResp.CapacityBytes=2GiB | 성공; CapacityBytes=2GiB; NodeExpansionRequired=true; agent.ExpandVolume 1회 호출 |
| 89 | `TestCSIExpand_ControllerExpandVolume_AgentReturnsZeroCapacity` | agent.ExpandVolume이 CapacityBytes=0을 반환하면 ControllerExpandVolume은 RequiredBytes를 폴백으로 사용 | CapacityRange.RequiredBytes=3GiB; mockAgentServer.ExpandVolumeResp.CapacityBytes=0 | 성공; CapacityBytes=3GiB (RequiredBytes 폴백); NodeExpansionRequired=true |

---

### E11.2 NodeExpandVolume — 파일시스템 타입별 리사이즈

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 90 | `TestCSIExpand_NodeExpandVolume_Ext4` | NodeExpandVolume이 ext4 파일시스템에서 mockResizer.ResizeFS("ext4")를 호출 | VolumeCapability.MountVolume.FsType="ext4"; VolumePath="/mnt/staging/pvc-expand"; mockResizer 주입 | 성공; ResizeFS 1회 호출; FsType="ext4"; CapacityBytes=RequiredBytes |
| 91 | `TestCSIExpand_NodeExpandVolume_XFS` | NodeExpandVolume이 xfs 파일시스템에서 mockResizer.ResizeFS("xfs")를 호출 | VolumeCapability.MountVolume.FsType="xfs" | 성공; ResizeFS 1회 호출; FsType="xfs" |

---

### E11.3 전체 확장 왕복(Full Expand Round Trip)

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 92 | `TestCSIExpand_FullExpandRoundTrip` | CreateVolume → ControllerExpandVolume → NodeExpandVolume 전체 확장 흐름: 볼륨 생성 후 컨트롤러가 백엔드를 확장하고 노드가 파일시스템을 리사이즈 | 단일 mockAgentServer; mockResizer; VolumeId 동일 사용 | ControllerExpandVolume: CapacityBytes=newSize, NodeExpansionRequired=true; NodeExpandVolume: ResizeFS 1회 호출; 오류 없음 |
| 93 | `TestCSIExpand_ControllerExpandVolume_Idempotent` | 이미 확장된 볼륨에 동일한 크기로 ControllerExpandVolume 재호출 — 멱등성 | mockAgentServer: ExpandVolume 항상 현재 크기 반환; 동일 RequiredBytes로 2회 호출 | 두 호출 모두 성공; agent.ExpandVolume 2회 호출; 오류 없음 |

---

### E11.4 오류 경로

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 94 | `TestCSIExpand_ControllerExpandVolume_AgentFails` | agent.ExpandVolume 실패 시 ControllerExpandVolume이 오류 코드를 전파 | mockAgentServer.ExpandVolumeErr=gRPC ResourceExhausted | ControllerExpandVolume이 ResourceExhausted 반환; NodeExpansionRequired 없음 |
| 95 | `TestCSIExpand_NodeExpandVolume_ResizerFails` | mockResizer.ResizeFS 실패 시 NodeExpandVolume이 Internal 반환 | mockResizer.ResizeFSErr="resize2fs: device busy" | gRPC Internal; 오류 메시지에 "resize" 포함 |

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

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 96 | `TestCSISnapshot_CreateSnapshot_ReturnsUnimplemented` | CSI CreateSnapshot이 현재 미구현으로 gRPC Unimplemented를 반환 | ControllerServer(실제 구현체) 직접 호출; CreateSnapshotRequest 유효 파라미터 제공 | gRPC Unimplemented; 에이전트 호출 없음 |
| 97 | `TestCSISnapshot_DeleteSnapshot_ReturnsUnimplemented` | CSI DeleteSnapshot이 현재 미구현으로 gRPC Unimplemented를 반환 | DeleteSnapshotRequest; SnapshotId="storage-1/snap-test" | gRPC Unimplemented |
| 98 | `TestCSISnapshot_ListSnapshots_ReturnsUnimplemented` | CSI ListSnapshots이 현재 미구현으로 gRPC Unimplemented를 반환 | ListSnapshotsRequest 빈 요청 | gRPC Unimplemented |

---

### E12.2 GetPluginCapabilities — 스냅샷 역량 미선언 검증

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 99 | `TestCSISnapshot_PluginCapabilities_NoSnapshotCapability` | GetPluginCapabilities 응답에 VolumeSnapshot 역량이 포함되지 않음 | IdentityServer.GetPluginCapabilities() 직접 호출 | 반환된 역량 목록에 `PluginCapability_VolumeExpansion_ONLINE`은 있으나 스냅샷 관련 역량은 없음 |

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

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 100 | `TestCSIClone_CreateVolume_SnapshotSourceIgnored` | VolumeContentSource.Snapshot이 포함된 CreateVolume 호출 시 스냅샷 소스를 무시하고 빈 볼륨을 생성 (현재 동작 고정 테스트) | CreateVolumeRequest에 VolumeContentSource.Snapshot="snap-A" 추가; mockAgentServer 정상 동작 | CreateVolume 성공; agent.CreateVolume 1회 호출(VolumeContentSource 없이); 생성된 볼륨은 스냅샷과 무관한 빈 볼륨 |
| 101 | `TestCSIClone_CreateVolume_VolumeSourceIgnored` | VolumeContentSource.Volume이 포함된 CreateVolume 호출 시 소스 볼륨을 무시하고 빈 볼륨을 생성 (현재 동작 고정 테스트) | CreateVolumeRequest에 VolumeContentSource.Volume="src-pvc-id" 추가 | CreateVolume 성공; 소스 볼륨 데이터 복사 없이 빈 볼륨 생성; agent.CreateVolume 1회 |

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

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 102 | `TestCSIEdge_CreateVolume_ExtremelyLongVolumeName` | 극도로 긴 볼륨 이름(2048자)으로 CreateVolume 호출 | name="pvc-" + 2000자 문자열; 유효한 StorageClass 파라미터 | gRPC InvalidArgument 또는 성공; 패닉 없음; agent 호출 시 VolumeId 길이 제한 초과 오류 |
| 103 | `TestCSIEdge_CreateVolume_SpecialCharactersInName` | 볼륨 이름에 슬래시("/") 포함 — VolumeId 파싱 혼동 유발 시도 | name="pvc/with/slashes"; 유효한 StorageClass 파라미터 | gRPC InvalidArgument; agent 호출 없음; VolumeId 파싱이 혼동되지 않음 |
| 104 | `TestCSIEdge_DeleteVolume_EmptyVolumeId` | 빈 VolumeId로 DeleteVolume 호출 | VolumeId="" | gRPC InvalidArgument; agent 호출 없음 |
| 105 | `TestCSIEdge_ControllerPublish_EmptyNodeId` | NodeId가 빈 문자열인 ControllerPublishVolume | NodeId=""; 유효한 VolumeId 및 VolumeContext | gRPC InvalidArgument; agent.AllowInitiator 호출 없음 |

---

### E14.2 CapacityRange 경계값

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 106 | `TestCSIEdge_CreateVolume_LimitLessThanRequired` | LimitBytes < RequiredBytes로 CreateVolume | CapacityRange: RequiredBytes=2GiB, LimitBytes=1GiB | gRPC InvalidArgument; CSI 명세상 불가능한 범위; agent 호출 없음 |
| 107 | `TestCSIEdge_ControllerExpand_ZeroRequiredBytes` | ControllerExpandVolume에서 RequiredBytes=0 | CapacityRange.RequiredBytes=0; LimitBytes=0 | gRPC InvalidArgument; agent.ExpandVolume 호출 없음 |
| 108 | `TestCSIEdge_ControllerExpand_ShrinkRequest` | 현재 크기보다 작은 RequiredBytes로 ControllerExpandVolume | mockAgentServer.ExpandVolumeErr에 "volsize cannot be decreased" 설정 | 비-OK gRPC 상태 (Internal); CSI 명세상 축소는 미지원 |
| 109 | `TestCSIEdge_CreateVolume_ExactLimitEqualsRequired` | RequiredBytes == LimitBytes (경계값) 로 CreateVolume | CapacityRange: RequiredBytes=LimitBytes=1GiB | 성공; agent.CreateVolume이 1GiB로 호출됨; 오류 없음 |

---

### E14.3 VolumeContext 값 검증

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 110 | `TestCSIEdge_NodeStage_InvalidPort` | VolumeContext.port가 숫자가 아닌 문자열 | VolumeContext: port="not-a-port" | gRPC InvalidArgument; Connector.Connect 미호출 |
| 111 | `TestCSIEdge_NodeStage_EmptyNQN` | VolumeContext.target_id(NQN)가 빈 문자열 | VolumeContext: target_id="" | gRPC InvalidArgument; Connector.Connect 미호출 |
| 112 | `TestCSIEdge_NodeStage_MissingVolumeContext` | VolumeContext 자체가 nil인 NodeStageVolume | VolumeContext=nil | gRPC InvalidArgument; Connector.Connect 미호출 |

---

### E14.4 StorageClass 파라미터 조합 오류

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 113 | `TestCSIEdge_CreateVolume_UnsupportedBackendType` | 알 수 없는 backend-type 파라미터로 CreateVolume | parameters["backend-type"]="lvm" (미지원) | gRPC InvalidArgument; agent 호출 없음 |
| 114 | `TestCSIEdge_CreateVolume_EmptyProtocolType` | protocol-type 파라미터 값이 빈 문자열 | parameters["protocol-type"]="" | gRPC InvalidArgument; agent 호출 없음 |

---

### E14.5 접근 모드(Access Mode) 조합 오류

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 115 | `TestCSIEdge_NodeStage_BlockAccessWithFsType` | 블록 접근 모드 VolumeCapability에 FsType 지정 (잘못된 조합) | VolumeCapability: AccessType=Block, FsType="ext4" | gRPC InvalidArgument 또는 FsType이 무시되고 블록 접근 성공 (구현 정의 동작); FormatAndMount 미호출 |
| 116 | `TestCSIEdge_CreateVolume_MultiNodeMultiWriter` | MULTI_NODE_MULTI_WRITER 접근 모드로 ValidateVolumeCapabilities | VolumeCapabilities: AccessMode=MULTI_NODE_MULTI_WRITER | ValidateVolumeCapabilities의 Message 필드에 미지원 이유 기록; CreateVolume은 이 모드로 성공할 수 없음 |

---

## E15: 리소스 고갈 (Resource Exhaustion)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

스토리지 풀 용량 부족, 연결 타임아웃 등 리소스 고갈 시나리오에서
올바른 gRPC 오류 코드가 반환되고 상태가 오염되지 않음을 검증한다.

**주의:** 실제 ZFS 풀 용량 고갈 테스트(F22–F23)는 실제 하드웨어가 필요하다.
이 섹션은 mock을 통한 오류 코드 매핑 및 상태 일관성에 집중한다.

---

### E15.1 풀 용량 고갈 오류 전파

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 117 | `TestCSIExhaustion_CreateVolume_PoolFull` | 스토리지 풀 가득 참 시 CreateVolume 실패 — gRPC 오류 코드 전파 검증 | mockAgentServer.CreateVolumeErr=gRPC ResourceExhausted("out of space") | gRPC ResourceExhausted 또는 Internal 반환; PillarVolume CRD 미생성; agent.ExportVolume 미호출 |
| 118 | `TestCSIExhaustion_ExpandVolume_ExceedsPoolCapacity` | ControllerExpandVolume이 풀 용량 초과 시도 — 오류 전파 검증 | mockAgentServer.ExpandVolumeErr=gRPC ResourceExhausted("pool at capacity") | gRPC ResourceExhausted 반환; NodeExpansionRequired 없음 |
| 119 | `TestCSIExhaustion_CreateVolume_InsufficientStorage` | 요청 용량이 사용 가능 용량보다 큰 경우 | mockAgentServer.CreateVolumeErr=gRPC OutOfRange("insufficient free space") | 비-OK gRPC 상태; 볼륨 미생성; 패닉 없음 |

---

### E15.2 연속 실패 시나리오 — 상태 일관성

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 120 | `TestCSIExhaustion_CreateVolume_ConsecutiveFailures` | agent.CreateVolume이 연속 5회 실패해도 상태 오염 없음 | 루프 5회; mockAgentServer.CreateVolumeErr 매 호출 설정 | 5회 모두 비-OK gRPC 상태; PillarVolume CRD 0개; fake k8s 클라이언트 상태 오염 없음; 패닉 없음 |
| 121 | `TestCSIExhaustion_NodeStage_ConnectTimeout` | NVMe-oF 연결 타임아웃 시 NodeStage 실패 — 상태 파일 미생성 검증 | mockConnector.ConnectErr=errors.New("connection timed out") | 비-OK gRPC 상태; StateDir에 상태 파일 0개; Mounter.FormatAndMount 미호출 |
| 122 | `TestCSIExhaustion_NodeStage_DeviceNeverAppears` | NVMe-oF 연결 성공 후 디바이스가 폴링 타임아웃 내에 나타나지 않음 | mockConnector.Connect 성공; DevicePath="" (빈 경로, 디바이스 미출현); 폴링 타임아웃=50ms | 비-OK gRPC 상태 (FailedPrecondition 또는 Internal); 상태 파일 미생성 |

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

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 123 | `TestCSIConcurrent_CreateVolume_SameNameNoPanic` | 동일 이름으로 5개 고루틴이 동시에 CreateVolume 호출해도 패닉/데드락 없음 | 5개 고루틴; 동일 볼륨 이름; mockAgentServer 정상 동작; 5초 타임아웃 | 5개 고루틴 모두 5초 내 완료; 패닉 없음; 일부는 성공, 나머지는 AlreadyExists 반환 가능 |
| 124 | `TestCSIConcurrent_CreateVolume_DifferentNames` | 5개 고루틴이 각각 다른 이름의 볼륨을 동시에 생성 | 5개 고루틴; 각각 고유한 볼륨 이름; mockAgentServer 정상 동작 | 5개 볼륨 모두 성공적으로 생성; PillarVolume CRD 5개; 데이터 손상 없음 |
| 125 | `TestCSIConcurrent_CreateDelete_Interleaved` | 볼륨 생성과 삭제를 동시에 수행 — 최종 상태 일관성 검증 | 고루틴 A: CreateVolume; 고루틴 B: 동시에 동일 볼륨 DeleteVolume; 양측 기다림 | 두 연산 모두 완료; 최종 상태는 생성됨 또는 삭제됨 중 하나; CRD 상태 일관성; 패닉 없음 |

---

### E16.2 동시 노드 마운트/언마운트

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 126 | `TestCSIConcurrent_NodeStage_SameVolumeDifferentPaths` | 동일 VolumeId를 서로 다른 스테이징 경로로 동시에 NodeStage 호출 | 2개 고루틴; 동일 VolumeId; 서로 다른 StagingTargetPath; mockConnector 정상 | 두 호출 모두 완료; 데드락 없음; 각각 독립적인 Connector.Connect 호출; 각각 별도 상태 파일 |
| 127 | `TestCSIConcurrent_NodePublish_MultipleTargets` | 스테이지 완료 후 3개 고루틴이 서로 다른 targetPath로 동시 NodePublish | NodeStage 1회 완료 후 3개 고루틴이 서로 다른 targetPath로 NodePublish 동시 호출 | 3개 NodePublish 모두 성공; 3개 독립 마운트 생성; 데드락 없음; 마운트 테이블 오염 없음 |

---

### E16.3 동시 ACL 관리

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 128 | `TestCSIConcurrent_AllowInitiator_MultipleNodes` | 다른 NodeId에 대해 동시에 ControllerPublishVolume 3회 호출 | 3개 고루틴; 동일 VolumeId; 서로 다른 NodeId; mockAgentServer.AllowInitiator 정상 | 3개 AllowInitiator 호출 모두 완료; 데드락 없음; 각각 다른 호스트 NQN으로 호출 |
| 129 | `TestCSIConcurrent_UnpublishVolume_Race` | 3개 노드에서 동시에 ControllerUnpublishVolume 호출 | 3개 고루틴; 각기 다른 NodeId; mockAgentServer.DenyInitiator 정상 | 3개 모두 완료; 패닉 없음; DenyInitiator 3회 호출 (각 고루틴별 1회) |

---

## E17: 정리 검증 (Cleanup Validation)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

CSI 연산 실패 또는 성공 후 부가 상태(상태 파일, PillarVolume CRD,
마운트 테이블, NVMe-oF 연결)가 올바르게 정리되는지 검증한다.
리소스 누수가 없음을 확인하는 것이 이 섹션의 핵심 목표이다.

---

### E17.1 실패 후 상태 파일 정리

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 130 | `TestCSICleanup_NodeStage_ConnectFailureNoStateFile` | NodeStageVolume에서 Connect 실패 시 상태 파일이 생성되지 않음 | mockConnector.ConnectErr 설정; NodeStageVolume 호출 | 비-OK gRPC 상태; StateDir에 상태 파일 0개; Mounter.FormatAndMount 미호출 |
| 131 | `TestCSICleanup_NodeStage_MountFailureDisconnects` | FormatAndMount 실패 시 이미 완료된 NVMe-oF 연결이 정리(롤백)됨 | mockConnector 정상(Connect 성공); mockMounter.FormatAndMountErr 설정 | 비-OK gRPC 상태; Connector.Disconnect 1회 호출 (롤백); StateDir에 상태 파일 0개 |
| 132 | `TestCSICleanup_NodeUnstage_FailurePreservesStateFile` | Connector.Disconnect 실패 시 상태 파일이 보존됨 — 재시도 가능 상태 유지 | NodeStage 성공; mockConnector.DisconnectErr 설정; NodeUnstage 호출 | 비-OK gRPC 상태; StateDir에 상태 파일 유지 (재시도를 위해 보존); 언마운트 동작은 구현 정의 |

---

### E17.2 PillarVolume CRD 정리 검증

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 133 | `TestCSICleanup_DeleteVolume_RemovesAllCRD` | 성공적 DeleteVolume 후 PillarVolume CRD가 fake k8s 클라이언트에서 완전히 삭제됨 | CreateVolume 성공 후 CRD 존재 확인; DeleteVolume 호출; CRD 재조회 | DeleteVolume 성공; fake 클라이언트에서 PillarVolume CRD 조회 시 NotFound |
| 134 | `TestCSICleanup_CreatePartial_DeleteVolumeCleansCRD` | 부분 생성 상태(Phase=CreatePartial) CRD도 DeleteVolume으로 정리됨 | ExportVolume 실패로 Phase=CreatePartial CRD 생성 후 DeleteVolume 호출 | DeleteVolume 성공; CRD 제거; agent.DeleteVolume 호출 (BackendCreated=true이면 호출) |
| 135 | `TestCSICleanup_FullLifecycle_NoResourceLeak` | 전체 라이프사이클 완료 후 모든 상태가 완전히 정리됨 | 전체 Create→ControllerPublish→NodeStage→NodePublish→NodeUnpublish→NodeUnstage→ControllerUnpublish→Delete 사이클 완료 | StateDir 상태 파일 0개; PillarVolume CRD 0개; mockMounter 마운트 테이블 빈 상태; Connector.DisconnectCalls=1 |

---

### E17.3 반복 생성/삭제 — 누적 상태 오염 없음

| # | 테스트 함수 | 설명 | 설정 | 기대 결과 |
|---|------------|------|------|----------|
| 136 | `TestCSICleanup_RepeatedCreateDelete` | 동일 이름의 볼륨을 10회 반복 생성/삭제해도 상태 오염 없음 | 루프 10회: CreateVolume(성공) → DeleteVolume(성공); 동일 볼륨 이름 재사용 | 모든 반복 성공; 매 반복 후 PillarVolume CRD 0개; 누적 오류 없음; 패닉 없음 |
| 137 | `TestCSICleanup_RepeatedStageUnstage` | 동일 볼륨을 5회 반복 NodeStage/NodeUnstage해도 상태 파일 누적 없음 | 루프 5회: NodeStage(성공) → NodeUnstage(성공); 동일 VolumeId 및 StagingTargetPath | 모든 반복 성공; 매 반복 후 StateDir 빈 상태; Connect/Disconnect 각 5회씩 호출 |

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
| F13 | `TestRealZFS_CreateSnapshot` | ZFS 커널 모듈, `zfs-utils`, CSI CreateSnapshot 구현 (미구현) | `zfs snapshot pool/vol@snap` 실행 후 CSI CreateSnapshot 응답 검증; ReadyToUse=true |
| F14 | `TestRealZFS_DeleteSnapshot` | ZFS 커널 모듈, CSI DeleteSnapshot 구현 (미구현) | `zfs destroy pool/vol@snap` 실행 후 스냅샷 목록에서 제거 확인 |
| F15 | `TestRealZFS_ListSnapshots` | ZFS 커널 모듈, CSI ListSnapshots 구현 (미구현) | `zfs list -t snapshot` 결과를 CSI ListSnapshots 응답으로 변환 |
| F16 | `TestKubernetes_VolumeSnapshot_CreateRestore` | 실제 Kubernetes 클러스터, external-snapshotter, ZFS 노드 | VolumeSnapshot CRD 생성 → PVC RestoreFrom → Pod 마운트; 데이터 일관성 검증 |
| F17 | `TestRealAgent_SendVolume_ZFSSend` | ZFS 커널 모듈, 실제 zvol, `zfs send` | `agent.SendVolume` 스트리밍 RPC: zfs send 스트림 청크 수신 및 checksum 검증 |
| F18 | `TestRealAgent_ReceiveVolume_ZFSReceive` | ZFS 커널 모듈, 실제 zvol, `zfs receive` | `agent.ReceiveVolume` 스트리밍 RPC: zfs receive 스트림 클라이언트 전송 및 볼륨 복원 |
| F19 | `TestRealAgent_SendReceiveVolume_CrossNode` | 두 스토리지 노드, ZFS 커널 모듈 | 노드 A → SendVolume → 노드 B ReceiveVolume 크로스 노드 마이그레이션; 마이그레이션 후 데이터 동일성 검증 |
| F20 | `TestRealZFS_CloneVolume_FromSnapshot` | ZFS 커널 모듈, CSI VolumeContentSource 구현 (미구현) | `zfs clone pool/vol@snap pool/new-vol` 후 CSI CreateVolume(VolumeContentSource.Snapshot) 응답 검증 |
| F21 | `TestKubernetes_VolumeExpansion_OnlinePod` | 실제 Kubernetes 클러스터, Pod 실행 중, ZFS 노드 | ControllerExpandVolume → NodeExpandVolume → Pod 내 파일시스템이 새 크기 반영 확인 |
| F22 | `TestRealZFS_PoolFull_CreateVolume` | ZFS 커널 모듈, 실제 ZFS pool (용량 제한 설정) | ZFS pool을 거의 가득 채운 후 새 zvol 생성 시도; ENOSPC 오류 전파 검증 |
| F23 | `TestRealZFS_PoolFull_ExpandVolume` | ZFS 커널 모듈, 용량 제한 ZFS pool | 현재 pool 여유 공간 이상의 용량으로 ExpandVolume 시도; 실제 "out of space" 오류 검증 |
| F24 | `TestRealNode_ConcurrentStageUnstage_SameVolume` | 실제 NVMe-oF 디바이스, 루트 권한, nvme-tcp 커널 모듈 | 동일 볼륨에 NodeStage/NodeUnstage를 동시 호출; 커널 레벨 레이스 컨디션 검증 |
| F25 | `TestKubernetes_ManyPVCsConcurrent` | 실제 Kubernetes 클러스터, pillar-agent, ZFS 노드 | 100개 PVC 동시 생성 — controller/agent 확장성 및 레이스 컨디션 검증 |
| F26 | `TestRealNode_UnmountWithBusyMount` | 실제 마운트 환경, 루트 권한 | 마운트 포인트가 사용 중(fuser)일 때 NodeUnstage 호출 — EBUSY 오류 처리 검증 |

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
