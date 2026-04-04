# CI Pipeline Host Setup Conditions — SSOT Reference

> **목적**: GitHub Actions `ubuntu-latest` 러너에서 pillar-csi E2E/Integration 테스트를
> 실행하기 위한 호스트 세팅 조건을 7개 외부 시스템별로 정리한다.
> 이 문서는 **CI 파이프라인 정의의 직접 입력**으로 사용할 수 있다.
>
> **수준 B**: 규칙 + 검증 명령 + 기대 출력 패턴 (Go 코드 미포함)
>
> **참조 구현**: `.github/workflows/ci.yml` — 실제 GHA 워크플로우

## 관련 문서

- [인프라 전략 인덱스](./README.md) — 기술별 상세 문서 목록
- [사전조건 체크 상세](./preconditions.md) — 개발 agent용 early-fail 사전조건 규칙
- [ZFS 인프라](./ZFS.md) | [LVM 인프라](./LVM.md) | [NVMe-oF 인프라](./NVMEOF.md) | [iSCSI 인프라](./ISCSI.md) | [envtest 인프라](./ENVTEST.md)
- [E2E 테스트](../E2E-TESTS.md) — Kind + 실제 스토리지 E2E TC 목록
- [Integration 테스트](../INTEGRATION-TESTS.md) — envtest 기반 컨트롤러 테스트

---

## 파이프라인 단계 개요

```
┌─────────────────────────────────────────────────────────────┐
│  Step 1: Checkout + Go Setup                                │
│  Step 2: Tool Installation (Kind, Helm, Ginkgo)             │
│  Step 3: System Package Installation (ZFS, LVM, NVMe)       │
│  Step 4: Kernel Module Loading                              │
│  Step 5: Host Condition Verification (gate)                 │
│  Step 6: Docker Image Pre-pull                              │
│  Step 7: Test Execution (make test-e2e)                     │
│  Step 8: Timing Gate & Summary                              │
└─────────────────────────────────────────────────────────────┘
```

---

## Step 1: 기본 환경 (Checkout + Go)

### 규칙

- `actions/checkout@v6` 으로 소스 체크아웃
- `actions/setup-go@v6` 으로 Go 설치 (`go-version-file: go.mod`)
- Go 모듈 캐시 활성화 (`cache: true`)

### 검증 명령

```bash
go version
```

### 기대 출력 패턴

```
go version go1\.\d+(\.\d+)? linux/amd64
```

### GHA YAML

```yaml
- name: Checkout
  uses: actions/checkout@v6

- name: Setup Go
  uses: actions/setup-go@v6
  with:
    go-version-file: go.mod
    cache: true
```

---

## Step 2: 도구 설치 (Kind, Helm, Ginkgo)

### 2.1 Kind

| 차원 | 명세 |
|------|------|
| **규칙** | Kind v0.27.0을 `install_only: true`로 설치 (클러스터 생성은 TestMain이 관리) |
| **이유** | TestMain이 클러스터 라이프사이클을 소유하므로 GHA에서 클러스터를 미리 만들면 안 됨 |

**GHA YAML:**

```yaml
- name: Install Kind
  uses: helm/kind-action@v1
  with:
    install_only: true
    version: "v0.27.0"
```

**검증 명령:**

```bash
kind version
```

**기대 출력 패턴:**

```
kind v0\.27\.0 .*
```

### 2.2 Helm

| 차원 | 명세 |
|------|------|
| **규칙** | Helm v3.x 설치 |
| **이유** | E2E 테스트가 `helm install --wait` 으로 pillar-csi 차트 배포 |

**GHA YAML:**

```yaml
- name: Install Helm
  uses: azure/setup-helm@v5
```

**검증 명령:**

```bash
helm version --short
```

**기대 출력 패턴:**

```
v3\.\d+\.\d+\+.*
```

### 2.3 Ginkgo CLI (TC 커버리지 검증 전용)

| 차원 | 명세 |
|------|------|
| **규칙** | `verify-tc-coverage` job에서만 필요. E2E job은 Makefile이 자동 설치 |
| **설치** | `go install github.com/onsi/ginkgo/v2/ginkgo@latest` |

**검증 명령:**

```bash
ginkgo version
```

**기대 출력 패턴:**

```
Ginkgo Version \d+\.\d+\.\d+
```

---

## Step 3: 시스템 패키지 설치

### 규칙

7개 외부 시스템 중 호스트 패키지가 필요한 것:

| 기술 | 패키지 | 필수 여부 |
|------|--------|-----------|
| **ZFS** | `zfsutils-linux` | E2E 필수 |
| **LVM** | `lvm2`, `thin-provisioning-tools` | E2E 필수 |
| **NVMe-oF** | `linux-modules-extra-$(uname -r)` | E2E 필수 |
| **iSCSI** | (없음 — Kind 컨테이너 내부) | 호스트 설치 불필요 |
| **envtest** | (없음 — Go 바이너리) | `make setup-envtest` |
| **Kind** | (GHA action으로 설치) | 별도 패키지 불필요 |
| **Helm** | (GHA action으로 설치) | 별도 패키지 불필요 |

### GHA YAML (통합 설치 스텝)

```yaml
- name: Install ZFS, LVM, and NVMe kernel modules
  run: |
    sudo apt-get update -qq
    sudo apt-get install -y zfsutils-linux lvm2 thin-provisioning-tools \
      linux-modules-extra-$(uname -r) || \
      sudo apt-get install -y zfsutils-linux lvm2 thin-provisioning-tools
```

> **Fallback 전략**: `linux-modules-extra-$(uname -r)` 패키지가 없는 커널 버전일 수 있으므로
> `||` 로 해당 패키지 없이 재시도한다.

### 검증 명령

```bash
# ZFS 도구
zfs version 2>&1 | head -1

# LVM 도구  
lvcreate --version 2>&1 | head -1

# thin-provisioning-tools
thin_check -V 2>&1 | head -1
```

### 기대 출력 패턴

```
zfs-.*
  LVM version:.*
thin_check .*
```

---

## Step 4: 커널 모듈 로딩

### 규칙

| 모듈 | 의존 기술 | 생성하는 리소스 | 로딩 순서 |
|------|-----------|-----------------|-----------|
| `zfs` | ZFS | — | 독립 |
| `dm_mod` | LVM | `/dev/mapper/control` | `dm_thin_pool` 전에 |
| `dm_thin_pool` | LVM | thin provisioning target | `dm_mod` 후에 |
| `nvme_fabrics` | NVMe-oF | `/dev/nvme-fabrics` | 독립 |
| `nvmet` | NVMe-oF | `/sys/kernel/config/nvmet/` | `nvmet_tcp` 전에 |
| `nvmet_tcp` | NVMe-oF | NVMe-oF TCP target 지원 | `nvmet` 후에 |
| `nvme_tcp` | NVMe-oF | NVMe-oF TCP initiator 지원 | 독립 |

> **iSCSI 참고**: `iscsi_tcp` 모듈은 호스트에서 명시적 로딩 불필요. Kind 컨테이너가 호스트
> 커널을 공유하며, ubuntu-latest 커널에 기본 포함되어 있다.

### GHA YAML (모듈 로딩 + 검증)

```yaml
- name: Load kernel modules
  run: |
    sudo modprobe zfs
    sudo modprobe dm_mod
    sudo modprobe dm_thin_pool
    sudo modprobe nvme_fabrics
    sudo modprobe nvmet
    sudo modprobe nvmet_tcp
    sudo modprobe nvme_tcp
    # Critical verification: /dev/mapper/control must exist for LVM
    test -e /dev/mapper/control && echo "LVM: /dev/mapper/control present" || \
      (echo "ERROR: /dev/mapper/control missing after dm_mod load" && exit 1)
```

### 검증 명령 (모든 모듈 일괄 확인)

```bash
for mod in zfs dm_mod dm_thin_pool nvme_fabrics nvmet nvmet_tcp nvme_tcp; do
  grep -q "^${mod} " /proc/modules && echo "PASS: ${mod}" || echo "FAIL: ${mod}"
done
```

### 기대 출력 패턴

```
PASS: zfs
PASS: dm_mod
PASS: dm_thin_pool
PASS: nvme_fabrics
PASS: nvmet
PASS: nvmet_tcp
PASS: nvme_tcp
```

### 부가 디바이스/디렉토리 검증

```bash
# LVM device-mapper control
test -e /dev/mapper/control && echo "PASS: /dev/mapper/control" || echo "FAIL: /dev/mapper/control"

# NVMe-oF fabrics device
test -e /dev/nvme-fabrics && echo "PASS: /dev/nvme-fabrics" || echo "FAIL: /dev/nvme-fabrics"

# NVMe-oF configfs directory
test -d /sys/kernel/config/nvmet && echo "PASS: nvmet configfs" || echo "FAIL: nvmet configfs"

# configfs mount
mount | grep -q 'configfs on /sys/kernel/config' && echo "PASS: configfs mounted" || echo "FAIL: configfs"
```

### 기대 출력 패턴

```
PASS: /dev/mapper/control
PASS: /dev/nvme-fabrics
PASS: nvmet configfs
PASS: configfs mounted
```

---

## Step 5: 호스트 조건 검증 게이트

> 이 단계는 CI 파이프라인에서 **early fail** 역할을 한다. 모든 사전조건을 확인한 후
> 불충족 시 나머지 단계를 실행하지 않고 즉시 실패한다.

### 규칙

| 카테고리 | 검증 항목 | 실패 시 |
|----------|-----------|---------|
| **Docker** | Docker daemon 응답 (10초 타임아웃) | 즉시 FAIL |
| **커널 모듈** | 7개 모듈 전체 로딩 확인 | 즉시 FAIL |
| **바이너리** | `kind`, `helm`, `zfs`, `zpool`, `lvcreate`, `vgcreate` | 즉시 FAIL |
| **디바이스** | `/dev/mapper/control`, `/dev/nvme-fabrics` | 즉시 FAIL |
| **configfs** | `/sys/kernel/config/nvmet` 디렉토리 | 즉시 FAIL |
| **차트 파일** | `charts/pillar-csi/Chart.yaml` | 즉시 FAIL |
| **루프백** | `losetup -f` 가용성 | 즉시 FAIL |
| **디스크 공간** | Docker data-root 파티션 ≥ 10GB | 즉시 FAIL |
| **NVMe-oF 포트** | 동적 할당 (`ports.Registry`, `net.Listen(:0)`) — 사전 검사 불필요 | — |

### GHA YAML (검증 게이트 스텝)

```yaml
- name: Verify host preconditions
  run: |
    FAIL=0
    
    echo "=== Docker daemon ==="
    timeout 10 docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 \
      && echo "PASS: Docker" || { echo "FAIL: Docker"; FAIL=1; }
    
    echo "=== Kernel modules ==="
    for mod in zfs dm_mod dm_thin_pool nvme_fabrics nvmet nvmet_tcp nvme_tcp; do
      grep -q "^${mod} " /proc/modules \
        && echo "PASS: ${mod}" || { echo "FAIL: ${mod}"; FAIL=1; }
    done
    
    echo "=== Binaries ==="
    for bin in kind helm zfs zpool lvcreate vgcreate; do
      which "${bin}" >/dev/null 2>&1 \
        && echo "PASS: ${bin}" || { echo "FAIL: ${bin}"; FAIL=1; }
    done
    
    echo "=== Devices & configfs ==="
    test -e /dev/mapper/control \
      && echo "PASS: /dev/mapper/control" || { echo "FAIL: /dev/mapper/control"; FAIL=1; }
    test -e /dev/nvme-fabrics \
      && echo "PASS: /dev/nvme-fabrics" || { echo "FAIL: /dev/nvme-fabrics"; FAIL=1; }
    test -d /sys/kernel/config/nvmet \
      && echo "PASS: nvmet configfs" || { echo "FAIL: nvmet configfs"; FAIL=1; }
    
    echo "=== Filesystem ==="
    test -f charts/pillar-csi/Chart.yaml \
      && echo "PASS: Helm chart" || { echo "FAIL: Helm chart"; FAIL=1; }
    losetup -f >/dev/null 2>&1 \
      && echo "PASS: loop device" || { echo "FAIL: loop device"; FAIL=1; }
    
    echo "=== Disk space ==="
    docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo "/var/lib/docker")
    avail_gb=$(df -BG "${docker_root}" 2>/dev/null | awk 'NR==2{gsub("G","",$4); print $4}')
    if [ "${avail_gb:-0}" -ge 10 ] 2>/dev/null; then
      echo "PASS: ${avail_gb}GB available"
    else
      echo "FAIL: ${avail_gb}GB available (need >= 10GB)"; FAIL=1
    fi
    
    echo "=== NVMe-oF TCP ports ==="
    echo "PASS: port allocation is dynamic via net.Listen(:0) — no fixed port reservation needed"
    echo "      ports.Registry handles allocation and deconfliction at runtime"
    
    echo ""
    if [ "${FAIL}" -eq 0 ]; then
      echo "All preconditions PASSED"
    else
      echo "PRECONDITION FAILURE — aborting pipeline"
      exit 1
    fi
```

### 기대 출력 패턴 (전체 통과)

```
=== Docker daemon ===
PASS: Docker
=== Kernel modules ===
PASS: zfs
PASS: dm_mod
PASS: dm_thin_pool
PASS: nvme_fabrics
PASS: nvmet
PASS: nvmet_tcp
PASS: nvme_tcp
=== Binaries ===
PASS: kind
PASS: helm
PASS: zfs
PASS: zpool
PASS: lvcreate
PASS: vgcreate
=== Devices & configfs ===
PASS: /dev/mapper/control
PASS: /dev/nvme-fabrics
PASS: nvmet configfs
=== Filesystem ===
PASS: Helm chart
PASS: loop device
=== Disk space ===
PASS: .*GB available
=== NVMe-oF TCP ports ===
PASS: port allocation is dynamic via net.Listen(:0) — no fixed port reservation needed

All preconditions PASSED
```

---

## Step 6: Docker 이미지 Pre-pull

### 규칙

| 카테고리 | 이미지 | 이유 |
|----------|--------|------|
| **Dockerfile 빌드 스테이지** | `alpine:3.21` | 런타임 베이스 (agent, node) |
| **Dockerfile 빌드 스테이지** | `golang:1.26-alpine3.23` | 빌더 스테이지 (공유) |
| **E2E 테스트 워크로드** | `busybox:1.36` | 테스트 Pod 이미지 |
| **Helm 차트 사이드카** | `registry.k8s.io/sig-storage/csi-provisioner:v5.2.0` | CSI provisioner |
| **Helm 차트 사이드카** | `registry.k8s.io/sig-storage/csi-attacher:v4.8.1` | CSI attacher |
| **Helm 차트 사이드카** | `registry.k8s.io/sig-storage/csi-resizer:v1.13.2` | CSI resizer |
| **Helm 차트 사이드카** | `registry.k8s.io/sig-storage/livenessprobe:v2.15.0` | Liveness probe |
| **Helm 차트 사이드카** | `registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0` | Node registrar |

> **목적**: Docker Hub rate-limit (HTTP 429) 및 인증 에러 (HTTP 401) 방지.
> Pre-pull로 로컬 캐시에 보관하면 `docker build`와 Kind `docker save/cp`가 네트워크를 사용하지 않는다.

### GHA YAML

```yaml
- name: Pre-pull Docker Hub base images
  run: |
    docker pull alpine:3.21@sha256:56fa17d2a7e7f168a043a2712e63aed1f8543aeafdcee47c58dcffe38ed51099
    docker tag \
      alpine:3.21@sha256:56fa17d2a7e7f168a043a2712e63aed1f8543aeafdcee47c58dcffe38ed51099 \
      alpine:3.21
    docker pull golang:1.26-alpine3.23

- name: Pre-pull third-party images
  run: |
    docker pull busybox:1.36
    docker pull registry.k8s.io/sig-storage/csi-provisioner:v5.2.0
    docker pull registry.k8s.io/sig-storage/csi-attacher:v4.8.1
    docker pull registry.k8s.io/sig-storage/csi-resizer:v1.13.2
    docker pull registry.k8s.io/sig-storage/livenessprobe:v2.15.0
    docker pull registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0
```

### 검증 명령

```bash
for img in alpine:3.21 golang:1.26-alpine3.23 busybox:1.36; do
  docker image inspect "${img}" >/dev/null 2>&1 \
    && echo "PASS: ${img} cached" || echo "FAIL: ${img} not cached"
done
```

### 기대 출력 패턴

```
PASS: alpine:3.21 cached
PASS: golang:1.26-alpine3.23 cached
PASS: busybox:1.36 cached
```

---

## Step 7: 테스트 실행

### 7.1 E2E 테스트 (Kind 클러스터)

| 차원 | 명세 |
|------|------|
| **진입점** | `make test-e2e` |
| **병렬 워커** | `E2E_PROCS=8` (기본) |
| **실패 시 중단** | `E2E_FAIL_FAST=true` |
| **타임아웃** | 120초 wall-clock budget |
| **Docker endpoint** | `DOCKER_HOST=unix:///var/run/docker.sock` |

**GHA YAML:**

```yaml
env:
  KIND_CLUSTER: "pillar-csi-e2e"
  E2E_IMAGE_TAG: "e2e"
  E2E_HELM_NAMESPACE: "pillar-csi-system"
  E2E_PROCS: "8"
  E2E_FAIL_FAST: "true"
  DOCKER_HOST: "unix:///var/run/docker.sock"

# ...
- name: Run full e2e suite with 120s timing gate
  run: |
    go mod tidy
    E2E_START=$SECONDS
    make test-e2e && MAKE_RC=0 || MAKE_RC=$?
    E2E_ELAPSED=$(( SECONDS - E2E_START ))
    echo "elapsed=${E2E_ELAPSED}" >> "$GITHUB_OUTPUT"
    if [ "${MAKE_RC}" -ne 0 ]; then exit "${MAKE_RC}"; fi
    if [ "${E2E_ELAPSED}" -gt 120 ]; then exit 1; fi
```

### 7.2 Integration 테스트 (envtest)

| 차원 | 명세 |
|------|------|
| **진입점** | `make test` |
| **사전조건** | envtest 바이너리 (`make setup-envtest` — Makefile 내부에서 자동 실행) |
| **CRD 매니페스트** | `make manifests` (Makefile dependency로 자동) |

**GHA YAML:**

```yaml
- name: Run tests
  run: |
    go mod tidy
    make test
```

### 7.3 E2E Baseline Benchmark (envtest만, Kind 없음)

| 차원 | 명세 |
|------|------|
| **진입점** | `make test-e2e-bench` |
| **목적** | 새 spec 추가로 인한 regression canary |
| **타임아웃** | 120초 wall-clock budget |
| **호스트 요구사항** | Go + go.mod 의존성만 (커널 모듈/Docker 불필요) |

---

## Step 8: Timing Gate & Summary

### 규칙

- E2E 전체 실행 시간이 120초를 초과하면 job FAIL
- Step Summary에 timing table을 항상 출력 (pass/fail 무관)

### GHA YAML

```yaml
- name: Emit e2e timing summary
  if: always()
  run: |
    ELAPSED="${{ steps.e2e_run.outputs.elapsed }}"
    if [ -z "$ELAPSED" ]; then ELAPSED="unknown"; fi
    {
      echo "## E2E Timing Gate"
      echo ""
      echo "| Metric | Value |"
      echo "|--------|-------|"
      echo "| Wall-clock | ${ELAPSED}s |"
      echo "| Budget | 120s |"
      if [ "$ELAPSED" != "unknown" ] && [ "$ELAPSED" -gt 120 ] 2>/dev/null; then
        echo "| Status | EXCEEDED |"
      else
        echo "| Status | PASS |"
      fi
    } >> "$GITHUB_STEP_SUMMARY"
```

---

## 기술별 CI 호스트 세팅 매트릭스

아래 표는 7개 기술 × CI 호스트 세팅 차원을 한 눈에 보여준다.

| 기술 | apt 패키지 | 커널 모듈 (modprobe) | GHA Action | 생성 디바이스/경로 | 환경변수 |
|------|-----------|---------------------|------------|-------------------|----------|
| **ZFS** | `zfsutils-linux` | `zfs` | — | — | — |
| **LVM** | `lvm2`, `thin-provisioning-tools` | `dm_mod`, `dm_thin_pool` | — | `/dev/mapper/control` | — |
| **NVMe-oF** | `linux-modules-extra-$(uname -r)` | `nvme_fabrics`, `nvmet`, `nvmet_tcp`, `nvme_tcp` | — | `/dev/nvme-fabrics`, `/sys/kernel/config/nvmet/` | — |
| **iSCSI** | (없음) | (없음 — 호스트 설치 불필요) | — | — | — |
| **envtest** | (없음) | (없음) | — | — | `KUBEBUILDER_ASSETS` (Makefile 자동) |
| **Kind** | (없음) | (없음) | `helm/kind-action@v1` | — | `KIND_CLUSTER`, `DOCKER_HOST` |
| **Helm** | (없음) | (없음) | `azure/setup-helm@v5` | — | `E2E_HELM_NAMESPACE`, `E2E_HELM_RELEASE` |

---

## Job별 호스트 요구사항 매트릭스

단일 CI 파이프라인에 여러 job이 있으며, 각 job마다 필요한 호스트 설정이 다르다.

| Job | ZFS | LVM | NVMe-oF | iSCSI | envtest | Kind | Helm | Docker |
|-----|-----|-----|---------|-------|---------|------|------|--------|
| **lint** | — | — | — | — | — | — | — | — |
| **verify-tc-coverage** | — | — | — | — | — | — | — | — |
| **test** (unit + integration) | — | — | — | — | **필수** | — | — | — |
| **e2e** (Kind multi-node) | **필수** | **필수** | **필수** | (Kind 내부) | — | **필수** | **필수** | **필수** |
| **e2e-bench** (in-process) | — | — | — | — | — | — | — | — |
| **build** (multi-platform) | — | — | — | — | — | — | — | **필수** (BuildKit) |

---

## 환경변수 전체 목록

CI 파이프라인에서 설정해야 하는 환경변수의 SSOT:

| 변수 | 기본값 | 사용처 | 설명 |
|------|--------|--------|------|
| `KIND_VERSION` | `v0.27.0` | e2e job | Kind 바이너리 버전 |
| `KIND_CLUSTER` | `pillar-csi-e2e` | e2e job | Kind 클러스터 이름 |
| `E2E_IMAGE_TAG` | `e2e` | e2e job | Docker 이미지 태그 |
| `E2E_HELM_NAMESPACE` | `pillar-csi-system` | e2e job | Helm 배포 네임스페이스 |
| `E2E_PROCS` | `8` | e2e job | Ginkgo 병렬 워커 수 |
| `E2E_FAIL_FAST` | `true` | e2e job | 첫 실패 시 중단 |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | e2e, build job | Docker daemon 소켓 |

> **주의**: `DOCKER_HOST`를 하드코딩하지 않고 환경변수로 주입한다.
> 참조: [MEMORY — DOCKER_HOST 하드코딩 금지](../../../.claude/projects/-home-bhyoo-projects-go-pillar-csi/memory/feedback_docker_host.md)

---

## 추가 권장 차원

기본 6개 차원 외에 CI 파이프라인에서 추가로 고려해야 할 차원:

### A. Runner 아키텍처 제약

| 차원 | 명세 |
|------|------|
| **규칙** | E2E job은 `linux/amd64` 러너에서만 실행 |
| **이유** | 커널 모듈 (`zfs`, `nvmet` 등)이 `x86_64` 전용으로 빌드됨 |
| **ARM 러너** | Kind + NVMe-oF 조합이 ARM에서 미검증 |

**검증 명령:**

```bash
uname -m
```

**기대 출력 패턴:**

```
x86_64
```

### B. GitHub Actions 러너 디스크 레이아웃

| 차원 | 명세 |
|------|------|
| **규칙** | ubuntu-latest 러너는 ~14GB 여유 공간 (`/` 파티션) |
| **위험** | Docker 이미지 빌드 + Kind 노드 이미지 + ZFS/LVM loopback 파일로 공간 부족 가능 |
| **대응** | loopback 이미지는 sparse 파일 사용 (ZFS: 512 MiB, LVM: 512 MiB per VG, fault 테스트: 64 MiB) |

**검증 명령:**

```bash
df -h / | awk 'NR==2{print "Available: "$4}'
```

**기대 출력 패턴:**

```
Available: [1-9][0-9]*G
```

### C. 커널 버전 호환성

| 차원 | 명세 |
|------|------|
| **규칙** | NVMe-oF TCP 지원에는 커널 ≥ 5.0 필요 |
| **현재** | ubuntu-latest (22.04/24.04) 커널은 5.15+ 또는 6.x (충분) |
| **향후 위험** | ubuntu-latest 이미지 업데이트 시 커널 모듈 패키지 이름 변경 가능 |

**검증 명령:**

```bash
uname -r | cut -d. -f1-2
```

**기대 출력 패턴:**

```
[5-9]\.[0-9]+|[1-9][0-9]+\..*
```

### D. 동시 실행 안전성

| 차원 | 명세 |
|------|------|
| **규칙** | 같은 러너에서 여러 E2E job이 동시 실행되면 리소스 충돌 |
| **대응** | GHA `concurrency` 그룹으로 동일 PR의 이전 실행 자동 취소 |
| **Kind 클러스터 이름** | PID/엔트로피 기반 랜덤 이름으로 충돌 방지 |

---

## 통합 사전조건 검증 스크립트

CI 파이프라인의 검증 게이트로 직접 사용할 수 있는 완전한 스크립트:

> 참고: 상세 항목별 규칙은 [preconditions.md](./preconditions.md) 참조

```bash
#!/bin/bash
# pillar-csi CI host precondition gate
# Usage: bash docs/testing/infra/ci-precondition-gate.sh
set -euo pipefail
FAIL=0

check() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "PASS: ${name}"
  else
    echo "FAIL: ${name}"
    FAIL=1
  fi
}

echo "=== Docker ==="
check "Docker daemon" timeout 10 docker info --format '{{.ServerVersion}}'

echo "=== Kernel modules ==="
for mod in zfs dm_mod dm_thin_pool nvme_fabrics nvmet nvmet_tcp nvme_tcp; do
  check "${mod}" grep -q "^${mod} " /proc/modules
done

echo "=== Binaries ==="
for bin in kind helm zfs zpool lvcreate vgcreate; do
  check "${bin}" which "${bin}"
done

echo "=== Devices ==="
check "/dev/mapper/control" test -e /dev/mapper/control
check "/dev/nvme-fabrics" test -e /dev/nvme-fabrics
check "nvmet configfs" test -d /sys/kernel/config/nvmet

echo "=== Filesystem ==="
check "Helm chart" test -f charts/pillar-csi/Chart.yaml
check "loop device" losetup -f

echo "=== Architecture ==="
check "x86_64" test "$(uname -m)" = "x86_64"

echo "=== NVMe-oF ports ==="
echo "PASS: port allocation is dynamic via net.Listen(:0) — no fixed port reservation needed"

echo ""
if [ "${FAIL}" -eq 0 ]; then
  echo "All CI host preconditions PASSED"
else
  echo "CI HOST PRECONDITION FAILURE"
  echo "See remediation steps in docs/testing/infra/preconditions.md"
  exit 1
fi
```

### 기대 출력 패턴 (전체 통과)

```
=== Docker ===
PASS: Docker daemon
=== Kernel modules ===
PASS: zfs
PASS: dm_mod
PASS: dm_thin_pool
PASS: nvme_fabrics
PASS: nvmet
PASS: nvmet_tcp
PASS: nvme_tcp
=== Binaries ===
PASS: kind
PASS: helm
PASS: zfs
PASS: zpool
PASS: lvcreate
PASS: vgcreate
=== Devices ===
PASS: /dev/mapper/control
PASS: /dev/nvme-fabrics
PASS: nvmet configfs
=== Filesystem ===
PASS: Helm chart
PASS: loop device
=== Architecture ===
PASS: x86_64
=== NVMe-oF ports ===
PASS: port allocation is dynamic via net.Listen(:0) — no fixed port reservation needed

All CI host preconditions PASSED
```

---

## 트러블슈팅: 자주 발생하는 CI 실패

| 증상 | 원인 | 해결 |
|------|------|------|
| `modprobe: FATAL: Module zfs not found` | `zfsutils-linux` 미설치 | Step 3 패키지 설치 확인 |
| `modprobe: FATAL: Module nvmet not found` | `linux-modules-extra` 미설치 or 커널 버전 불일치 | `sudo apt-get install -y linux-modules-extra-$(uname -r)` |
| `/dev/mapper/control missing` | `dm_mod` 미로딩 | `sudo modprobe dm_mod` 후 device 확인 |
| `Cannot connect to Docker daemon` | Docker 서비스 미시작 or DOCKER_HOST 오설정 | `DOCKER_HOST=unix:///var/run/docker.sock` 확인 |
| NVMe-oF 포트 바인드 실패 | 이전 E2E 실행 잔류 프로세스 | `ports.Registry`가 동적 할당하므로 일반적으로 발생하지 않음. 발생 시 `kind delete cluster`로 정리 |
| `no space left on device` | Docker 이미지/loopback 파일로 디스크 부족 | `docker system prune -f` 또는 loopback 크기 축소 |
| `Error: INSTALLATION FAILED: timed out` | Helm install 7분 타임아웃 초과 | 이미지 pre-pull 확인, Kind 노드 리소스 확인 |
| `thin_check: command not found` | `thin-provisioning-tools` 미설치 | Step 3 패키지 설치에 포함 확인 |
