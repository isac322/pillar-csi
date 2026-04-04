# pillar-csi 테스트 전략

이 디렉토리는 pillar-csi의 테스트 케이스를 **테스트 분류별**로 정리한다.
기존 `E2E-TESTCASES.md`의 모놀리식 구조를 개선하여, 각 TC가 검증하는 경계(boundary)에 따라 올바른 카테고리에 배치한다.

## 테스트 피라미드

```
                ┌─────────────┐
                │ Performance  │  5 TC
                │ (성능 벤치)   │  gRPC 지연, 프로비저닝 시간, 스케일
                └──────┬──────┘
                    ┌──┴──────┐
                    │  E2E    │  ~130 TC
                    │ (실제   │  Kind + 실제 LVM/ZFS + 실제 NVMe-oF/iSCSI
                    │  인프라) │  PVC → Pod 마운트 → 데이터 I/O
                ┌───┴─────────┴───┐
                │  Integration    │  ~215 TC
                │ (실제 외부      │  envtest(실제 K8s API), 실제 LVM backend,
                │  시스템 경계)   │  Helm 배포, CSI Sanity + 실제 backend
            ┌───┴─────────────────┴───┐
            │  Component              │  ~178 TC
            │ (프레임워크 오케스트레이션)│  Controller/Node 로직, mock agent,
            │                         │  mock connector, fake k8s client
        ┌───┴─────────────────────────┴───┐
        │  Unit                           │  64 TC
        │ (순수 로직)                      │  입력 검증, 파싱, 호환성 매트릭스,
        │                                 │  상태 머신 전이, 에러 코드 상수
        └─────────────────────────────────┘
```

추가로 CSI 커뮤니티 표준인 **CSI Sanity** (계약 테스트)를 별도로 운영한다.

## 분류 기준

| 레벨 | 경계 | 외부 의존성 | 실행 속도 | CI 가능 |
|------|------|-----------|----------|---------|
| **Unit** | 함수/모듈 내부 | 없음 | 초 | ✅ |
| **Component** | 여러 내부 모듈 조합 | 전부 mock/fake | 초 | ✅ |
| **Integration** | 실제 외부 시스템 경계를 넘음 | envtest, 실제 LVM/ZFS, 실제 gRPC, Kind | 분 | ⚠️ 환경 필요 |
| **E2E** | 사용자 시나리오 전체 재현 | Kind + 실제 스토리지 + 실제 프로토콜 | 분~시간 | ⚠️ 커널 모듈 필요 |
| **CSI Sanity** | CSI gRPC 스펙 준수 계약 | 실제 backend 권장 | 분 | ✅/⚠️ |
| **Performance** | 비기능 성능 요구사항 검증 | Kind + 실제 스토리지 backend | 분~시간 | ⚠️ 환경 필요 |

### 분류 결정 트리

```
이 TC가 검증하는 것은?
    │
    ├── 순수 로직 (입력 검증, 파싱, 상태 전이, 에러 코드)?
    │       └── Unit
    │
    ├── 내부 모듈 간 오케스트레이션 (호출 순서, 에러 전파, 멱등성)?
    │   └── 외부 의존성이 전부 mock/fake?
    │           └── Component
    │
    ├── 실제 외부 시스템과의 상호작용?
    │   ├── 실제 K8s API (envtest/Kind) → Integration
    │   ├── 실제 backend (LVM VG, ZFS zpool on loopback) → Integration
    │   ├── 실제 configfs (커널 nvmet/LIO) → Integration
    │   └── Helm 차트 배포 → Integration
    │
    └── PVC 생성 → Pod 마운트 → 데이터 I/O → 정리 전체 흐름?
            └── E2E
```

## 문서 구조

| 문서 | TC 수 | 설명 |
|------|-------|------|
| [UNIT-TESTS.md](UNIT-TESTS.md) | 64 | 입력 검증, 파싱, 호환성 매트릭스, 상태 머신 |
| [COMPONENT-TESTS.md](COMPONENT-TESTS.md) | ~178 | 프레임워크 오케스트레이션, mock 기반 |
| [INTEGRATION-TESTS.md](INTEGRATION-TESTS.md) | ~215 | envtest, 실제 backend, Helm, CSI Sanity |
| [E2E-TESTS.md](E2E-TESTS.md) | ~130 | Kind + 실제 스토리지 + 실제 프로토콜 |
| [CSI-SANITY.md](CSI-SANITY.md) | ~70 | kubernetes-csi/csi-test 스펙 준수 |
| [PERFORMANCE-TESTS.md](PERFORMANCE-TESTS.md) | 5 | gRPC 지연, 프로비저닝 시간, 스케일 |

## 원본 TC ID 추적

모든 TC는 원본 `E2E-TESTCASES.md`의 ID(E1.1, E28.3 등)를 보존한다.
새 분류에서의 위치가 달라졌을 뿐, TC 자체의 정의(ID, 테스트 함수, 설명, 사전 조건, 단계, 기대 결과, 커버리지)는 변경되지 않았다.

## TC 분류 요약표

### Unit (순수 로직)

| 원본 섹션 | TC 수 | 검증 대상 |
|-----------|-------|----------|
| E1.6 | 8 | Access mode 검증 (`isSupportedAccessMode`) |
| E1.7 | 5 | Capacity range 산술 검증 |
| E1.11 | 3 | VolumeId 형식 파싱/생성 |
| E5 | 6 | 상태 머신 순서 제약 |
| E12 | 4 | Snapshot Unimplemented 에러 코드 |
| E13 | 2 | Clone 미처리 동작 에러 코드 |
| E14 | 15 | 잘못된 입력값/엣지 케이스 입력 검증 |
| E22 | 12 | Backend-Protocol 호환성 매트릭스 |
| E2.6 중 입력 검증 | 4 | Empty VolumeID/NodeID/Capability → InvalidArgument |

### Component (프레임워크 오케스트레이션)

| 원본 섹션 | TC 수 | 검증 대상 |
|-----------|-------|----------|
| E1.1-E1.5 | 13 | CreateVolume/DeleteVolume 오케스트레이션 |
| E1.8-E1.10 | ~6 | PillarTarget 상태, 부분 실패 복구, PVC 오버라이드 |
| E2 (E2.5, E2.6 중 오케스트레이션) | ~8 | Publish/Unpublish ACL 위임 |
| E3 | 70 | NodeStage/Publish 전체 |
| E4 | 4 | 교차-컴포넌트 라이프사이클 체인 |
| E6 | 5 | 부분 실패 영속성 (CRD 상태 기록) |
| E7 | 5 | 게시 멱등성 |
| E8 | 3 | mTLS 핸드셰이크 (testcerts) |
| E9 | 6 | Agent gRPC 디스패치 (mock backend) |
| E11 | 8 | 볼륨 확장 오케스트레이션 |
| E15 | 6 | 리소스 고갈 에러 전파 |
| E16 | 7 | 동시 작업 안전성 |
| E17 | 8 | 정리 검증 |
| E18 | 6 | Agent 다운 에러 핸들링 |
| E21.1 | 6 | 잘못된 CR 런타임 처리 (fake client) |
| E24 | 10 | 8단계 전체 라이프사이클 실패/복구 |
| E29 | 12 | LVM 파라미터 전파 |
| E30 | 3 | LVM 중복 방지 최적화 |

### Integration (실제 외부 시스템)

| 원본 섹션 | TC 수 | 실제로 넘는 경계 |
|-----------|-------|----------------|
| E19 | 19 | envtest — PillarTarget CRD |
| E20 | 20 | envtest — PillarPool CRD |
| E23 | 24 | envtest — PillarProtocol CRD |
| E25 | 41 | envtest — PillarBinding CRD |
| E26 | 23 | envtest — 교차-CRD 상호작용 |
| E21.2-E21.4 | 20 | envtest — Webhook/스키마 검증 |
| E27 | 29 | Kind — Helm 차트 배포 |
| E28 | 30 | 실제 LVM backend (loopback VG) |
| E32 | 9 | envtest — LVM CRD |
| ZFS backend (계획) | TBD | 실제 ZFS backend (loopback zpool) |
| Protocol configfs (계획) | TBD | 실제 커널 configfs (nvmet/LIO) |

### E2E (사용자 시나리오)

| 원본 섹션 | TC 수 | 인프라 |
|-----------|-------|-------|
| E10 | 3 | Kind 기본 클러스터 |
| E33 | 33 | Kind + 실제 LVM VG + NVMe-oF TCP |
| E34 | 13 | Kind + 실제 LVM VG + iSCSI |
| E35 | 13 | Kind + 실제 ZFS zpool + iSCSI |
| E-NEW-1 | 1 | Kind — init container modprobe best-effort |
| E-FAULT-1~5 | 6 | Kind — 장애 복구 시뮬레이션 (리부트, 네트워크, 풀 고갈) |
| 수동 | 7 | 운영 환경 전용 (멀티-AZ, 물리 NIC 등) |

### 현재 부재 — 계획 필요

아래 영역은 현재 TC가 없거나 불충분하며, 향후 추가가 필요하다.

1. **ZFS backend integration test**
   LVM은 E28에서 30개 TC로 실제 VG를 테스트하지만, ZFS에는 동등한 integration 레벨 TC가 없다.
   E28 패턴으로 loopback zpool에서 zvol create/delete/expand/snapshot을 테스트해야 한다.

2. **Protocol target integration test (실제 configfs)**
   현재 NVMe-oF/iSCSI target 설정을 실제 커널 configfs로 테스트하는 TC가 없다.
   mock configfs(`t.TempDir()`)에서 성공하지만 실제 커널에서 실패하는 케이스를 잡으려면
   실제 configfs 조작 integration test가 필요하다.
   GHA ubuntu runner에서 `linux-modules-extra` 설치 후 실행 가능.

## 실행 방법

```bash
# Unit + Component (빌드 태그 없음, 표준 CI)
go test ./test/unit/ -v
go test ./test/component/ -v

# Integration — envtest
make setup-envtest
go test -tags=integration ./internal/... -v

# Integration — 실제 LVM backend
go test -tags=e2e ./test/e2e/ -run TestLVMAgent -v

# Integration — Helm (Kind 필요)
go test -tags=e2e ./test/e2e/ -run TestHelm -v

# E2E — Kind + 실제 스토리지 (커널 모듈 필요)
sudo apt-get install -y linux-modules-extra-$(uname -r)
sudo modprobe nvmet nvmet_tcp target_core_mod iscsi_target_mod
go test -tags=e2e ./test/e2e/ -run TestLVM_Kind -v

# CSI Sanity (실제 backend 권장)
go test ./test/sanity/ -v

# 전체 E2E (베어메탈)
go test -tags=e2e_full ./test/e2e/ -v
```

## 새 backend/protocol 추가 시 필수 테스트 체크리스트

```
새 backend (예: Btrfs) 추가 시:
  ☐ Unit: 파라미터 검증, 호환성 매트릭스 엔트리
  ☐ Component: Controller가 새 backend params를 agent에 올바르게 전달
  ☐ Integration: 실제 backend에서 create/delete/expand (E28 패턴)
  ☐ CSI Sanity: 실제 backend로 CSI 스펙 준수 검증
  ☐ E2E: PVC → Pod 마운트 검증 (E33 패턴)

새 protocol (예: NFS) 추가 시:
  ☐ Unit: 호환성 매트릭스, 프로토콜 파라미터 검증
  ☐ Component: Controller/Node가 새 protocol을 올바르게 위임
  ☐ Integration: 실제 export/mount (configfs 또는 exportfs)
  ☐ CSI Sanity: 실제 protocol로 CSI 스펙 준수 검증
  ☐ E2E: PVC → protocol mount → Pod I/O (E33 패턴)
```

## CI 환경 — 커널 모듈 설정 (GHA)

GHA ubuntu runner에서 NVMe-oF/iSCSI 커널 모듈 사용이 가능하다:

```yaml
- name: Install storage kernel modules
  run: |
    sudo apt-get update
    sudo apt-get install -y linux-modules-extra-$(uname -r) || \
      sudo apt-get install -y linux-modules-extra-azure
    sudo modprobe nvmet nvmet_tcp nvme_tcp nvme_fabrics
    sudo modprobe target_core_mod iscsi_target_mod iscsi_tcp
    sudo mount -t configfs none /sys/kernel/config 2>/dev/null || true
```

**주의:** `linux-modules-extra` 패키지 버전이 커널과 불일치할 수 있음 (간헐적).
Meta-package fallback으로 완화. Canonical이 커널 6.15부터 base에 통합 예정.

## configfs 테스트 격리 (병렬 실행)

- **이름 격리:** 테스트별 고유 NQN/IQN 사용 → 별도 configfs subtree
- **포트 격리:** 테스트별 고유 포트 할당 (`ports.Registry`)
- **병렬 안전성:** 커널 `su_mutex`로 직렬화되지만 데이터 레이스/패닉 없음
- **크래시 정리:** suite 시작 시 scavenger pass로 잔여 configfs 엔트리 제거
