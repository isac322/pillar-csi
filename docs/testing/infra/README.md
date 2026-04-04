# 테스트 인프라 전략 — 인덱스 및 공통 원칙

> **SSOT (Single Source of Truth)** — pillar-csi E2E/Integration 테스트에서 사용하는
> 7개 외부 시스템의 인프라 전략을 수준 B(규칙 + 검증 명령 + 기대 출력 패턴)로 정의한다.
>
> **CI 환경:** GitHub Actions `ubuntu-latest`
>
> **대상 독자:** TC 구현 개발자, CI 파이프라인 관리자, 코드 리뷰어

---

## 크로스레퍼런스

| 문서 | 설명 |
|------|------|
| [테스트 전략 README](../README.md) | 테스트 피라미드, 분류 기준, TC ID 추적 |
| [Integration 테스트](../INTEGRATION-TESTS.md) | envtest, 실제 backend, Helm 배포 TC 목록 |
| [E2E 테스트](../E2E-TESTS.md) | Kind + 실제 스토리지 + 실제 프로토콜 TC 목록 |
| [Component 테스트](../COMPONENT-TESTS.md) | mock/fake 기반 오케스트레이션 TC 목록 |
| [CSI Sanity](../CSI-SANITY.md) | CSI gRPC 스펙 준수 계약 테스트 |
| [사전조건 SSOT](./preconditions.md) | 7개 기술의 early-fail 사전조건 통합 명세 |
| 프레임워크 구현 | `test/e2e/framework/prereq/prereq.go` |

---

## 기술별 상세 문서

| # | 기술 | 문서 | 주요 적용 TC |
|---|------|------|-------------|
| 1 | **ZFS** | [ZFS.md](./ZFS.md) | E35 (Kind + ZFS + iSCSI), ZFS backend integration |
| 2 | **LVM** | [LVM.md](./LVM.md) | E28 (실제 LVM backend), E33 (LVM + NVMe-oF), E34 (LVM + iSCSI) |
| 3 | **NVMe-oF configfs** | [NVMEOF.md](./NVMEOF.md) | E33 (LVM + NVMe-oF TCP), NVMe-oF target integration |
| 4 | **iSCSI configfs** | [ISCSI.md](./ISCSI.md) | E34 (LVM + iSCSI), E35 (ZFS + iSCSI) |
| 5 | **envtest** | [ENVTEST.md](./ENVTEST.md) | E19–E26 (CRD 컨트롤러), E21 (Webhook), E32 (LVM CRD) |
| 6 | **Kind** | [KIND.md](./KIND.md) | E10, E27, E33–E35, E-FAULT (장애 복구) |
| 7 | **Helm** | [HELM.md](./HELM.md) | E27 (Helm 차트 배포 29 TC) |

---

## 차원 매트릭스

각 기술별 문서는 최소 아래 6개 **필수 차원**과 기술별 **추가 차원**을 빠짐없이 기술한다.

### 필수 차원 (6개)

| # | 차원 | 설명 | 규칙 예시 |
|---|------|------|----------|
| 1 | **호스트 사전조건** | TC 실행 전 호스트에 필요한 커널 모듈, 바이너리, 디바이스 | `modprobe zfs`, `which kind` |
| 2 | **리소스 생성** | 테스트 인프라(풀, 클러스터, 릴리스 등) 생성 규칙 | loopback zpool, Kind cluster |
| 3 | **리소스 정리** | 테스트 후 인프라 해체 규칙 (정상 종료 경로) | `zpool destroy`, `kind delete cluster` |
| 4 | **TC간 격리** | 병렬/순차 실행 시 TC 간 간섭 방지 규칙 | 고유 풀 이름, 고유 NQN, 별도 namespace |
| 5 | **사이징** | 리소스 크기 및 용량 제한 규칙 | loop file 512 MiB (기본), 디스크 여유 10GB |
| 6 | **실패 시 정리** | TC 실패, panic, 시그널 인터럽트 시 정리 규칙 | `t.Cleanup()` 등록, scavenger pass |

### 공통 추가 차원 (모든 기술에 적용)

| # | 차원 | 설명 |
|---|------|------|
| 7 | **CI 호환성** | GitHub Actions `ubuntu-latest`에서의 설치/설정 방법 |

### 기술별 추가 차원

| 기술 | 추가 차원 |
|------|-----------|
| ZFS | 스냅샷 및 클론, 상태 조회 |
| LVM | thin provisioning, 상태 조회 |
| NVMe-oF | 포트 레지스트리, 서브시스템 격리, 커널 버전 |
| iSCSI | Kind 컨테이너 내부 vs 호스트, 타겟 관리 |
| envtest | CRD 매니페스트, 빌드 태그, 바이너리 관리 |
| Kind | 멀티-패스 실행 전략, kubeconfig 관리 |
| Helm | 차트 유효성, values 오버라이드 |

---

## 공통 원칙

### 원칙 1: Hard Fail — Never Soft-Skip

모든 사전조건 미충족 시 **즉시 실패** (`os.Exit(1)` 또는 `t.Fatal()`).
`t.Skip()`, `GinkgoSkip()` 등의 soft-skip은 **금지**한다.

> **근거:** soft-skip은 CI 그린바를 유지하면서 실제 검증을 건너뛰는 false positive를 만든다.
> 사전조건 불충분 시 실패해야 즉시 인지하고 수정할 수 있다.

**검증 명령:**

```bash
# 테스트 코드에 t.Skip 또는 GinkgoSkip이 사전조건 관련으로 사용되지 않음을 확인
grep -rn 't\.Skip\|GinkgoSkip' test/e2e/framework/prereq/ | wc -l
```

**기대 출력 패턴:**

```
0
```

---

### 원칙 2: 비파괴적 사전조건 검사 (Non-destructive)

사전조건 검사는 **읽기 전용** 연산만 수행한다.

- `/proc/modules` 읽기 (커널 모듈 확인)
- `exec.LookPath()` (바이너리 존재 확인)
- 디렉토리/파일 존재 여부 (`os.Stat()`)
- 소켓 연결 테스트 (Docker daemon)

시스템 상태를 변경하는 연산 (`modprobe`, `apt install`, `mount`)은 검사 단계에서 **절대 실행하지 않는다**.

---

### 원칙 3: DOCKER_HOST 환경변수 존중

Docker 소켓 경로를 하드코딩하지 않는다.
`DOCKER_HOST` 환경변수가 설정되어 있으면 이를 우선 사용한다.

**검증 명령:**

```bash
# Docker 소켓 경로 하드코딩이 없음을 확인
grep -rn '/var/run/docker.sock' test/e2e/framework/ | grep -v '_test.go' | grep -v '// ' | wc -l
```

**기대 출력 패턴:**

```
0
```

---

### 원칙 4: 집계된 에러 보고 (Aggregate Errors)

사전조건 검사는 **모든 항목을 검사한 후** 누적된 에러를 한 번에 출력한다.
첫 번째 실패에서 멈추지 않고, 모든 실패를 보고하여 한 번에 수정할 수 있게 한다.

---

### 원칙 5: Remediation 포함

각 사전조건 실패 항목에 **OS별 설치/설정 명령**을 포함한다.

```
FAIL: zfs kernel module not loaded
  → Ubuntu: sudo apt install zfsutils-linux && sudo modprobe zfs
  → Fedora: sudo dnf install zfs && sudo modprobe zfs
```

---

### 원칙 6: sudo 금지 (검사 단계)

사전조건 **검사**는 비루트 사용자로 실행 가능해야 한다.
`sudo`가 필요한 작업은 remediation 안내에만 포함한다.

---

### 원칙 7: 루프백 기반 격리 (Loopback Isolation)

스토리지 테스트(ZFS, LVM, iSCSI)는 **루프백 디바이스** 기반으로 동작한다.
실제 블록 디바이스를 사용하지 않아 호스트 스토리지에 영향을 주지 않는다.

| 기술 | 루프백 사용 | 기본 크기 |
|------|------------|----------|
| ZFS | `truncate` + `losetup` → zpool | 512 MiB (기본), 64 MiB (fault/exhaustion 테스트) |
| LVM | `truncate` + `losetup` → PV → VG | 512 MiB (기본), 64 MiB (fault/exhaustion 테스트) |
| iSCSI | Kind 컨테이너 내부 loop device | TC별 상이 |

**검증 명령 (루프백 디바이스 가용성):**

```bash
losetup -f >/dev/null 2>&1 && echo "PASS: free loop device available" || echo "FAIL: no free loop device"
```

**기대 출력 패턴:**

```
PASS: free loop device available
```

> **루프백 디바이스 고갈 주의**: 최대 동시 루프백 디바이스 수는 기본 256개 (커널 기본값).
> `E2E_PROCS=8`과 TC당 복수 스토리지 백엔드를 사용할 경우 루프백 사용량을 모니터링해야 한다.
> 프레임워크는 `losetup -a | wc -l`로 현재 사용량을 확인하고, 한계에 근접하면 경고를 출력해야 한다.

---

### 원칙 8: TC별 고유 이름 (Unique Naming)

모든 테스트 리소스는 **TC별 고유 이름**을 사용하여 격리한다.

| 기술 | 이름 패턴 | 예시 |
|------|----------|------|
| ZFS | `e2e-tank-<RANDOM_SUFFIX>` | `e2e-tank-a1b2c3d4` |
| LVM | `e2e-vg-<RANDOM_SUFFIX>` | `e2e-vg-e5f6g7h8` |
| NVMe-oF | `nqn.2024-01.com.pillar-csi:test-<uuid>` | 고유 NQN per TC |
| iSCSI | `iqn.2024-01.com.pillar-csi:test-<uuid>` | 고유 IQN per TC |
| Kind | `pillar-e2e-<suite>` | Suite 레벨 단일 클러스터 |
| Helm | `pillar-csi-test-<ns>` | 고유 namespace per TC |
| envtest | (프로세스 격리) | 별도 etcd/apiserver per suite |

---

### 원칙 9: 정리 등록 우선 (Cleanup-First)

리소스 생성 **직후** 정리 함수를 등록한다.
생성과 정리 등록 사이에 어떤 코드도 실행하지 않는다.

```
생성 → 즉시 t.Cleanup() 등록 → 이후 로직
```

이 원칙을 통해 panic, `t.Fatal()`, 시그널 인터럽트 시에도 정리가 실행된다.

---

### 원칙 10: CI 환경 동일성 (CI Parity)

로컬 개발 환경과 CI 환경은 **동일한 사전조건 검사**를 실행한다.
CI 전용 예외나 CI 전용 skip은 존재하지 않는다.

**CI 환경 (GitHub Actions ubuntu-latest) 기본 제공:**

| 제공됨 | 별도 설치 필요 |
|--------|--------------|
| Docker daemon | ZFS (`zfsutils-linux`) |
| Go toolchain | LVM (`lvm2`, `thin-provisioning-tools`) |
| git | Kind (`kind-action`) |
| — | Helm (`setup-helm`) |
| — | NVMe 커널 모듈 (`linux-modules-extra`) |
| — | envtest (`make setup-envtest`) |

---

## 기술별 사전조건 요약 매트릭스

| 기술 | 커널 모듈 | 바이너리 | configfs/디바이스 | 루프백 | 네트워크 |
|------|-----------|---------|------------------|--------|---------|
| **ZFS** | `zfs` | `zfs`, `zpool` | — | ✅ 512 MiB | — |
| **LVM** | `dm_mod`, `dm_thin_pool` | `lvcreate`, `vgcreate` 등 | `/dev/mapper/control` | ✅ 512 MiB | — |
| **NVMe-oF** | `nvme_fabrics`, `nvme_tcp`, `nvmet`, `nvmet_tcp` | — | `/sys/kernel/config/nvmet`, `/dev/nvme-fabrics` | — | TCP (동적 포트, `ports.Registry`) |
| **iSCSI** | `target_core_mod`, `iscsi_target_mod` (호스트, integration) / (Kind 내부, E2E) | LIO configfs (integration) / tgtd (Kind E2E) | `/sys/kernel/config/target/iscsi/` (integration) | Kind 내부 loop device | TCP (동적 포트, Kind 내부) |
| **envtest** | — | `kube-apiserver`, `etcd` | — | — | — |
| **Kind** | — | `kind`, `docker` | — | — | Docker socket |
| **Helm** | — | `helm` | — | — | — |

---

## CI 파이프라인 호스트 세팅 체크리스트

GitHub Actions `ubuntu-latest`에서 E2E 테스트를 실행하기 위한 **최소 세팅 순서:**

```yaml
# 1. 커널 모듈 설치 및 로드
- name: Install kernel modules
  run: |
    sudo apt-get update -qq
    sudo apt-get install -y linux-modules-extra-$(uname -r) || \
      sudo apt-get install -y linux-modules-extra-azure
    sudo modprobe zfs || true  # ZFS TC 실행 시
    sudo modprobe dm_mod dm_thin_pool
    sudo modprobe nvme_fabrics nvmet nvmet_tcp nvme_tcp
    sudo modprobe target_core_mod iscsi_target_mod  # iscsi_tcp는 Kind 컨테이너 내부에서만 필요
    sudo mount -t configfs none /sys/kernel/config 2>/dev/null || true

# 2. 스토리지 도구 설치
- name: Install storage tools
  run: |
    sudo apt-get install -y \
      zfsutils-linux \
      lvm2 thin-provisioning-tools

# 3. Kind 설치
- name: Install Kind
  uses: helm/kind-action@v1
  with:
    install_only: true
    version: "v0.27.0"

# 4. Helm 설치
- name: Install Helm
  uses: azure/setup-helm@v5

# 5. envtest 바이너리 설치
- name: Setup envtest
  run: make setup-envtest

# 6. 사전조건 검증 (통합)
- name: Verify preconditions
  run: |
    echo "=== Kernel modules ==="
    for mod in zfs dm_mod dm_thin_pool nvme_fabrics nvmet nvmet_tcp nvme_tcp target_core_mod iscsi_target_mod; do
      grep -q "^${mod} " /proc/modules && echo "PASS: ${mod}" || echo "FAIL: ${mod}"
    done
    echo "=== Binaries ==="
    for bin in kind helm zfs zpool lvcreate vgcreate; do
      which "${bin}" >/dev/null 2>&1 && echo "PASS: ${bin}" || echo "FAIL: ${bin}"
    done
    echo "=== Docker ==="
    docker info --format '{{.ServerVersion}}'
    echo "=== configfs ==="
    test -d /sys/kernel/config/nvmet && echo "PASS: nvmet" || echo "FAIL: nvmet"
```

**기대 출력 패턴 (전체 통과):**

```
=== Kernel modules ===
PASS: zfs
PASS: dm_mod
PASS: dm_thin_pool
PASS: nvme_fabrics
PASS: nvmet
PASS: nvmet_tcp
PASS: nvme_tcp
PASS: target_core_mod
PASS: iscsi_target_mod
=== Binaries ===
PASS: kind
PASS: helm
PASS: zfs
PASS: zpool
PASS: lvcreate
PASS: vgcreate
=== Docker ===
[0-9]+\.[0-9]+\.[0-9]+
=== configfs ===
PASS: nvmet
```

---

## 문서 관리 규칙

1. **SSOT**: 각 기술의 인프라 규칙은 해당 기술 문서에서만 정의한다. 다른 문서에서 중복 정의하지 않고 크로스레퍼런스로 연결한다.
2. **수준 B 준수**: 모든 규칙에는 검증 명령과 기대 출력 패턴이 동반되어야 한다. Go 코드 스니펫은 포함하지 않는다.
3. **차원 완전성**: 새 기술 문서 추가 시 필수 6개 차원 + CI 호환성을 반드시 포함한다.
4. **변경 시 검증**: 기술 문서 변경 시 해당 검증 명령을 실제 실행하여 패턴 일치를 확인한다.
