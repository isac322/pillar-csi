# Early-Fail Precondition Checks — SSOT Reference

> **목적**: 7개 외부 시스템별 사전조건 검증 규칙을 구현 소스 수준으로 명시한다.
> 각 규칙은 **검증 명령 + 기대 출력 패턴**을 포함하며, Go 코드 없이도 shell 스크립트나
> `prereq.CheckHostPrerequisites()` 구현의 입력으로 직접 사용할 수 있다.
>
> **수준 B**: 규칙 + 검증 명령 + 기대 출력 패턴 (Go 코드 스니펫 미포함)
>
> **CI 환경**: GitHub Actions `ubuntu-latest`
>
> **원칙**: AC 10 "never soft-skip" — 모든 사전조건 미충족 시 즉시 FAIL (Skip 금지)

## 관련 문서

- [테스트 전략 개요](../README.md)
- [통합 테스트](../INTEGRATION-TESTS.md) — envtest 기반 컨트롤러 테스트
- [E2E 테스트](../E2E-TESTS.md) — Kind + 실제 스토리지 E2E
- [인프라 전략 인덱스](./README.md) — 기술별 상세 문서

## 공통 설계 원칙

| 원칙 | 설명 |
|------|------|
| **Hard Fail** | 모든 사전조건 미충족 → 즉시 `os.Exit(1)`. `t.Skip()` / `GinkgoSkip()` 사용 금지 |
| **Non-destructive** | 읽기 전용 검사만 수행 (PATH 조회, `/proc/modules` 읽기, 소켓 연결) |
| **No sudo** | 모든 검사는 비루트 사용자로 실행 가능 |
| **DOCKER_HOST 존중** | Docker 소켓 경로 하드코딩 금지, 환경변수 우선 |
| **Aggregate errors** | 모든 검사를 실행한 후 누적된 오류를 한 번에 출력 |
| **Remediation 포함** | 각 실패 항목에 OS별 설치/로드 명령 포함 |

---

## 1. ZFS 사전조건

### 1.1 커널 모듈: `zfs`

| 차원 | 명세 |
|------|------|
| **규칙** | `/proc/modules`에 `zfs` 모듈이 로드되어 있어야 한다 |
| **검사 방식** | `/proc/modules` 파일을 읽어 첫 번째 필드가 `zfs`인 줄 존재 여부 확인 |
| **실패 시** | Hard FAIL + remediation 출력 |

**검증 명령:**

```bash
grep -q '^zfs ' /proc/modules && echo "PASS: zfs module loaded" || echo "FAIL: zfs module not loaded"
```

**기대 출력 패턴:**

```
PASS: zfs module loaded
```

**실패 시 기대 출력:**

```
FAIL: zfs module not loaded
```

**Remediation:**

```bash
# 모듈 로드
sudo modprobe zfs

# 패키지 설치 (모듈 미존재 시)
# Ubuntu/Debian:
sudo apt install zfsutils-linux
# Fedora/RHEL:
sudo dnf install zfs  # ZFS repo 추가 필요
# Arch Linux:
sudo pacman -S zfs-dkms
```

### 1.2 바이너리: `zfs`, `zpool`

| 차원 | 명세 |
|------|------|
| **규칙** | `zfs`와 `zpool` 바이너리가 PATH에 존재해야 한다 |
| **검사 방식** | `exec.LookPath("zfs")`, `exec.LookPath("zpool")` |
| **실패 시** | Hard FAIL + remediation 출력 |

**검증 명령:**

```bash
which zfs && echo "PASS: zfs binary found at $(which zfs)" || echo "FAIL: zfs binary not in PATH"
which zpool && echo "PASS: zpool binary found at $(which zpool)" || echo "FAIL: zpool binary not in PATH"
```

**기대 출력 패턴:**

```
PASS: zfs binary found at /usr/sbin/zfs
PASS: zpool binary found at /usr/sbin/zpool
```

**Remediation:**

```bash
# Ubuntu/Debian:
sudo apt install zfsutils-linux
# Fedora/RHEL:
sudo dnf install zfs
```

### 1.3 CI 호환성 (GitHub Actions ubuntu-latest)

```yaml
- name: Install ZFS
  run: |
    sudo apt-get update -qq
    sudo apt-get install -y zfsutils-linux
    sudo modprobe zfs
```

**CI 검증 명령:**

```bash
zfs version
```

**기대 출력 패턴:**

```
zfs-.*
zfs-kmod-.*
```

---

## 2. LVM 사전조건

### 2.1 커널 모듈: `dm_mod`, `dm_thin_pool`

| 차원 | 명세 |
|------|------|
| **규칙** | `/proc/modules`에 `dm_mod`와 `dm_thin_pool` 모듈이 모두 로드되어 있어야 한다 |
| **검사 방식** | `/proc/modules` 파일 파싱, 첫 필드 `dm_mod` 및 `dm_thin_pool` 확인 |
| **의존 관계** | `dm_mod` 모듈이 로드되어야 `/dev/mapper/control`이 생성되고, `dm_thin_pool`이 thin provisioning을 지원한다 |
| **실패 시** | Hard FAIL + remediation 출력 |

**검증 명령:**

```bash
for mod in dm_mod dm_thin_pool; do
  grep -q "^${mod} " /proc/modules && echo "PASS: ${mod} loaded" || echo "FAIL: ${mod} not loaded"
done
```

**기대 출력 패턴:**

```
PASS: dm_mod loaded
PASS: dm_thin_pool loaded
```

**부가 검증 — `/dev/mapper/control` 존재:**

```bash
test -e /dev/mapper/control && echo "PASS: /dev/mapper/control present" || echo "FAIL: /dev/mapper/control missing"
```

**기대 출력 패턴:**

```
PASS: /dev/mapper/control present
```

### 2.2 바이너리: `lvcreate`, `vgcreate`

| 차원 | 명세 |
|------|------|
| **규칙** | `lvcreate`와 `vgcreate` 바이너리가 PATH에 존재해야 한다 |
| **검사 방식** | `exec.LookPath("lvcreate")`, `exec.LookPath("vgcreate")` |
| **실패 시** | Hard FAIL + remediation 출력 |

**검증 명령:**

```bash
which lvcreate && echo "PASS: lvcreate found" || echo "FAIL: lvcreate not in PATH"
which vgcreate && echo "PASS: vgcreate found" || echo "FAIL: vgcreate not in PATH"
```

**기대 출력 패턴:**

```
PASS: lvcreate found
PASS: vgcreate found
```

**Remediation:**

```bash
# Ubuntu/Debian:
sudo apt install lvm2 thin-provisioning-tools
# Fedora/RHEL:
sudo dnf install lvm2
```

### 2.3 CI 호환성 (GitHub Actions ubuntu-latest)

```yaml
- name: Install LVM
  run: |
    sudo apt-get install -y lvm2 thin-provisioning-tools
    sudo modprobe dm_mod
    sudo modprobe dm_thin_pool
    test -e /dev/mapper/control && echo "LVM: /dev/mapper/control present" || \
      (echo "ERROR: /dev/mapper/control missing after dm_mod load" && exit 1)
```

---

## 3. NVMe-oF configfs 사전조건

### 3.1 커널 모듈: `nvme_fabrics`, `nvme_tcp`, `nvmet`, `nvmet_tcp`

| 차원 | 명세 |
|------|------|
| **규칙** | 4개 NVMe 모듈 모두 `/proc/modules`에 로드되어야 한다 |
| **순서 의존성** | `nvme_fabrics` (독립) → `nvmet` → `nvmet_tcp` 순서로 로드 (nvmet_tcp는 nvmet에 의존) |
| **nvme_fabrics** | `/dev/nvme-fabrics` 디바이스를 생성하며, Kind 노드에 바인드 마운트됨 |
| **실패 시** | Hard FAIL + 각 모듈별 remediation 출력 |

**검증 명령:**

```bash
for mod in nvme_fabrics nvme_tcp nvmet nvmet_tcp; do
  grep -q "^${mod} " /proc/modules && echo "PASS: ${mod} loaded" || echo "FAIL: ${mod} not loaded"
done
```

**기대 출력 패턴:**

```
PASS: nvme_fabrics loaded
PASS: nvme_tcp loaded
PASS: nvmet loaded
PASS: nvmet_tcp loaded
```

### 3.2 configfs 마운트 및 nvmet 서브시스템 디렉토리

| 차원 | 명세 |
|------|------|
| **규칙** | `/sys/kernel/config/nvmet/` 디렉토리가 존재해야 한다 (Sub-AC 9b) |
| **검사 방식** | 디렉토리 존재 여부 확인 |
| **의미** | nvmet 모듈 로드 시 configfs에 자동 마운트됨 |

**검증 명령:**

```bash
test -d /sys/kernel/config/nvmet && echo "PASS: nvmet configfs present" || echo "FAIL: /sys/kernel/config/nvmet missing"
```

**기대 출력 패턴:**

```
PASS: nvmet configfs present
```

**하위 디렉토리 구조 검증:**

```bash
ls /sys/kernel/config/nvmet/
```

**기대 출력 패턴 (모든 항목 존재):**

```
hosts  ports  subsystems
```

### 3.3 `/dev/nvme-fabrics` 디바이스 존재

| 차원 | 명세 |
|------|------|
| **규칙** | `nvme_fabrics` 모듈 로드 후 `/dev/nvme-fabrics` 디바이스 존재 |
| **이유** | Kind config에서 이 디바이스를 compute-worker 노드에 bind-mount |
| **CI에서** | `sudo modprobe nvme_fabrics` 로 생성 |

**검증 명령:**

```bash
test -e /dev/nvme-fabrics && echo "PASS: /dev/nvme-fabrics exists" || echo "FAIL: /dev/nvme-fabrics missing"
```

**기대 출력 패턴:**

```
PASS: /dev/nvme-fabrics exists
```

### 3.4 커널 버전 요구사항

| 차원 | 명세 |
|------|------|
| **규칙** | 커널 ≥ 5.0 필요 (CONFIG_NVME_TCP=m, CONFIG_NVME_TARGET_TCP=m) |
| **검사 방식** | `uname -r` 파싱 후 메이저.마이너 비교 |

**검증 명령:**

```bash
kernel_version=$(uname -r | cut -d. -f1-2)
if [ "$(echo "$kernel_version >= 5.0" | bc)" -eq 1 ]; then
  echo "PASS: kernel ${kernel_version} >= 5.0"
else
  echo "FAIL: kernel ${kernel_version} < 5.0, NVMe-oF TCP requires >= 5.0"
fi
```

**기대 출력 패턴:**

```
PASS: kernel .* >= 5.0
```

### 3.5 CI 호환성 (GitHub Actions ubuntu-latest)

```yaml
- name: Load NVMe modules
  run: |
    sudo apt-get install -y linux-modules-extra-$(uname -r) || true
    sudo modprobe nvme_fabrics
    sudo modprobe nvmet
    sudo modprobe nvmet_tcp
    sudo modprobe nvme_tcp
```

**주의**: `linux-modules-extra-$(uname -r)` 패키지가 실패할 수 있으므로 fallback 처리 필요.

---

## 4. iSCSI configfs 사전조건

### 4.1 이중 접근 방식 (LIO configfs + tgtd)

| 차원 | 명세 |
|------|------|
| **규칙** | iSCSI target 관리에 두 가지 접근 방식이 존재한다 |
| **LIO configfs** | Integration 테스트용 — PRD 4/2.5에 정의된 LIO configfs 기반. 호스트에 `target_core_mod`, `iscsi_target_mod` 커널 모듈 필요 |
| **tgtd** | Kind E2E 테스트용 — 유저스페이스 tgtd 기반. 호스트 커널 모듈 불필요, Kind 컨테이너 내부에서 동작 |
| **initiator** | `iscsi_tcp` 모듈과 `iscsiadm` 바이너리는 Kind 컨테이너 워커 노드 내부에서만 사용됨 |
| **이유** | `prereq.go` 주석: "iscsi_tcp and iscsiadm are NOT checked here because iSCSI initiator functionality runs inside Kind container worker nodes" |

### 4.2 호스트 커널 모듈 (Integration 테스트용)

| 차원 | 명세 |
|------|------|
| **규칙** | Integration 테스트(LIO configfs 사용)를 위해 호스트에 `target_core_mod`와 `iscsi_target_mod` 커널 모듈이 로드되어야 한다 |
| **검사 방식** | `/proc/modules` 파일 파싱 |
| **실패 시** | Hard FAIL + remediation 출력 |

**검증 명령 (호스트):**

```bash
for mod in target_core_mod iscsi_target_mod; do
  grep -q "^${mod} " /proc/modules && echo "PASS: ${mod} loaded" || echo "FAIL: ${mod} not loaded"
done
```

**기대 출력 패턴:**

```
PASS: target_core_mod loaded
PASS: iscsi_target_mod loaded
```

**Remediation:**

```bash
sudo modprobe target_core_mod
sudo modprobe iscsi_target_mod
# 패키지 설치 (모듈 미존재 시):
# Ubuntu/Debian: sudo apt install linux-modules-extra-$(uname -r)
```

### 4.3 Kind 컨테이너 내부 사전조건 (E2E 테스트용)

| 차원 | 명세 |
|------|------|
| **규칙** | Kind E2E 테스트는 LIO configfs 대신 tgtd를 사용한다 (LIO는 호스트 커널 모듈 로딩이 필요하지만, tgtd는 유저스페이스에서 Kind 컨테이너 내부에서 실행 가능) |
| **구현 위치** | `test/e2e/framework/iscsi/iscsi.go`의 `CreateTarget()` |
| **루프백 디바이스** | loop device 기반 iSCSI 타겟 생성 |

**검증 명령 (Kind 컨테이너 내부):**

```bash
# Kind 워커 노드 컨테이너에서 실행
docker exec <kind-worker-container> which tgtadm
```

**기대 출력 패턴:**

```
/usr/sbin/tgtadm
```

### 4.4 커널 모듈: `iscsi_tcp` (Kind 컨테이너 공유 커널)

| 차원 | 명세 |
|------|------|
| **규칙** | Kind 컨테이너는 호스트 커널을 공유하므로, 호스트에 `iscsi_tcp`가 로드되어야 한다 |
| **현재 상태** | `prereq.go`에서 검사하지 않음 (Kind 내부에서 자동 로드) |
| **추가 검사 권장** | 실패 시 디버깅 난이도 감소를 위해 추가 검증 권장 |

**검증 명령:**

```bash
# 호스트에서 iscsi_tcp 모듈 로드 가능 여부 확인
modinfo iscsi_tcp >/dev/null 2>&1 && echo "PASS: iscsi_tcp module available" || echo "WARN: iscsi_tcp module not available"
```

**기대 출력 패턴:**

```
PASS: iscsi_tcp module available
```

### 4.5 CI 호환성 (GitHub Actions ubuntu-latest)

```yaml
# iSCSI는 별도 호스트 설정 불필요 — Kind 컨테이너 내부에서 처리
# 단, 커널이 iscsi_tcp 모듈을 지원해야 함 (ubuntu-latest 기본 제공)
```

**CI 검증:**

```bash
modinfo iscsi_tcp 2>/dev/null | head -1
```

**기대 출력 패턴:**

```
filename:.*iscsi_tcp.*
```

---

## 5. envtest 사전조건

### 5.1 KUBEBUILDER_ASSETS 또는 로컬 바이너리 디렉토리

| 차원 | 명세 |
|------|------|
| **규칙** | envtest 바이너리(etcd, kube-apiserver, kubectl)가 접근 가능해야 한다 |
| **검사 방식 1** | `KUBEBUILDER_ASSETS` 환경변수가 유효한 디렉토리를 가리킴 |
| **검사 방식 2** | `bin/k8s/<version>/` 디렉토리에 바이너리 존재 (IDE 실행 시) |
| **설정 명령** | `make setup-envtest` |

**검증 명령 (Makefile 경유):**

```bash
make setup-envtest 2>&1 | tail -1
```

**기대 출력 패턴:**

```
Version:.*Path:.*
```

**검증 명령 (직접 확인):**

```bash
# KUBEBUILDER_ASSETS가 설정된 경우
test -d "${KUBEBUILDER_ASSETS}" && \
  test -x "${KUBEBUILDER_ASSETS}/kube-apiserver" && \
  test -x "${KUBEBUILDER_ASSETS}/etcd" && \
  echo "PASS: envtest binaries found" || echo "FAIL: envtest binaries missing"
```

**기대 출력 패턴:**

```
PASS: envtest binaries found
```

**검증 명령 (로컬 bin 디렉토리 — IDE 실행 시):**

```bash
ls bin/k8s/*/kube-apiserver bin/k8s/*/etcd 2>/dev/null && echo "PASS: local envtest binaries found" || echo "FAIL: run 'make setup-envtest' first"
```

**기대 출력 패턴:**

```
bin/k8s/.*/kube-apiserver
bin/k8s/.*/etcd
PASS: local envtest binaries found
```

### 5.2 CRD 매니페스트 파일 존재

| 차원 | 명세 |
|------|------|
| **규칙** | `config/crd/bases/` 디렉토리에 CRD YAML 파일이 존재해야 한다 |
| **이유** | `envtest.Environment{CRDDirectoryPaths: []string{...}, ErrorIfCRDPathMissing: true}` |
| **생성 명령** | `make manifests` |

**검증 명령:**

```bash
ls config/crd/bases/*.yaml 2>/dev/null | wc -l
```

**기대 출력 패턴 (4개 CRD 이상):**

```
[4-9][0-9]*|[4-9]
```

**개별 CRD 확인:**

```bash
for crd in pillartargets pillarvolumes pillarprotocols pillarbindings; do
  ls config/crd/bases/*_${crd}.yaml 2>/dev/null && echo "PASS: ${crd} CRD manifest found" || echo "FAIL: ${crd} CRD manifest missing"
done
```

### 5.3 Go 빌드 태그

| 차원 | 명세 |
|------|------|
| **규칙** | envtest 기반 테스트는 `//go:build integration` 태그 필요 |
| **실행 방식** | `go test -tags=integration ./internal/...` |
| **태그 없이 실행** | envtest 테스트가 아예 컴파일되지 않음 (정상 동작) |

**검증 명령:**

```bash
head -1 internal/controller/suite_test.go
```

**기대 출력 패턴:**

```
//go:build integration
```

### 5.4 CI 호환성 (GitHub Actions ubuntu-latest)

```yaml
- name: Setup envtest
  run: make setup-envtest

- name: Run integration tests
  run: make test
  # Makefile의 test 타겟이 KUBEBUILDER_ASSETS를 자동 설정
```

**CI 검증 명령:**

```bash
KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.32.x --bin-dir bin/k8s -p path)" && \
  echo "PASS: envtest assets at ${KUBEBUILDER_ASSETS}" || \
  echo "FAIL: setup-envtest failed"
```

**기대 출력 패턴:**

```
PASS: envtest assets at .*bin/k8s/.*
```

---

## 6. Kind 사전조건

### 6.1 바이너리: `kind`

| 차원 | 명세 |
|------|------|
| **규칙** | `kind` 바이너리가 PATH에 존재해야 한다 |
| **검사 방식** | `exec.LookPath("kind")` |
| **실패 시** | Hard FAIL + remediation 출력 |

**검증 명령:**

```bash
which kind && echo "PASS: kind binary found at $(which kind)" || echo "FAIL: kind not in PATH"
```

**기대 출력 패턴:**

```
PASS: kind binary found at .*kind
```

**버전 검증:**

```bash
kind version
```

**기대 출력 패턴:**

```
kind v0\.\d+\.\d+ .*
```

**Remediation:**

```bash
go install sigs.k8s.io/kind@latest
# 또는:
curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64 && chmod +x /usr/local/bin/kind
```

### 6.2 Docker daemon 가용성

| 차원 | 명세 |
|------|------|
| **규칙** | Docker daemon이 응답 가능해야 한다 (Kind는 Docker 위에서 동작) |
| **검사 방식** | `docker info --format '{{.ServerVersion}}'` (10초 타임아웃) |
| **DOCKER_HOST** | 환경변수 존중, 하드코딩 금지 |
| **실패 시** | Hard FAIL + DOCKER_HOST 값 포함 에러 메시지 |

**검증 명령:**

```bash
timeout 10 docker info --format '{{.ServerVersion}}' && echo "PASS: Docker daemon reachable" || echo "FAIL: Docker daemon not reachable"
```

**기대 출력 패턴:**

```
[0-9]+\.[0-9]+\.[0-9]+
PASS: Docker daemon reachable
```

**실패 시 기대 출력:**

```
FAIL: Docker daemon not reachable
```

**Remediation:**

```bash
sudo systemctl start docker
# 또는 DOCKER_HOST 설정:
export DOCKER_HOST=unix:///var/run/docker.sock
```

### 6.3 리소스 사이징 — 디스크 공간

| 차원 | 명세 |
|------|------|
| **규칙** | Kind 클러스터 + Docker 이미지에 최소 10GB 여유 공간 필요 |
| **검사 위치** | Docker daemon의 data-root 파티션 |

**검증 명령:**

```bash
docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo "/var/lib/docker")
avail_gb=$(df -BG "${docker_root}" 2>/dev/null | awk 'NR==2{gsub("G","",$4); print $4}')
if [ "${avail_gb}" -ge 10 ] 2>/dev/null; then
  echo "PASS: ${avail_gb}GB available at ${docker_root}"
else
  echo "FAIL: only ${avail_gb}GB available at ${docker_root} (need >= 10GB)"
fi
```

**기대 출력 패턴:**

```
PASS: [0-9]+GB available at .*
```

### 6.4 CI 호환성 (GitHub Actions ubuntu-latest)

```yaml
- name: Install Kind
  uses: helm/kind-action@v1
  with:
    install_only: true
    version: "v0.27.0"

# 환경변수:
env:
  KIND_CLUSTER: "pillar-csi-e2e"
  DOCKER_HOST: "unix:///var/run/docker.sock"
```

---

## 7. Helm 사전조건

### 7.1 바이너리: `helm`

| 차원 | 명세 |
|------|------|
| **규칙** | `helm` 바이너리가 PATH에 존재해야 한다 |
| **검사 방식** | `exec.LookPath("helm")` |
| **실패 시** | Hard FAIL + remediation 출력 |

**검증 명령:**

```bash
which helm && echo "PASS: helm binary found at $(which helm)" || echo "FAIL: helm not in PATH"
```

**기대 출력 패턴:**

```
PASS: helm binary found at .*helm
```

**버전 검증:**

```bash
helm version --short
```

**기대 출력 패턴:**

```
v3\.\d+\.\d+\+.*
```

**Remediation:**

```bash
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

### 7.2 Helm 차트 존재

| 차원 | 명세 |
|------|------|
| **규칙** | `charts/pillar-csi/Chart.yaml`가 존재해야 한다 |
| **이유** | E2E 테스트가 로컬 차트 경로로 `helm install` 실행 |

**검증 명령:**

```bash
test -f charts/pillar-csi/Chart.yaml && echo "PASS: Helm chart found" || echo "FAIL: charts/pillar-csi/Chart.yaml missing"
```

**기대 출력 패턴:**

```
PASS: Helm chart found
```

**차트 유효성 검증:**

```bash
helm lint charts/pillar-csi/ 2>&1 | tail -1
```

**기대 출력 패턴:**

```
.*0 chart\(s\) failed.*
```

### 7.3 CI 호환성 (GitHub Actions ubuntu-latest)

```yaml
- name: Install Helm
  uses: azure/setup-helm@v5
```

---

## 8. 추가 권장 차원

아래는 기본 6개 차원 외에 추가로 검증이 필요한 사항이다.

### 8.1 네트워크 격리 — 포트 충돌 방지

| 차원 | 적용 대상 | 명세 |
|------|-----------|------|
| **NVMe-oF TCP 포트** | NVMe-oF | `ports.Registry`가 `net.Listen(:0)`으로 OS 임시 포트를 동적 할당 — 고정 포트 예약 불필요 |
| **iSCSI 포트** | iSCSI | `ports.Registry`가 동적 할당 — Kind 컨테이너 내부에서 사용 |

Port allocation: dynamic via `net.Listen(:0)` -- no fixed port reservation needed. `ports.Registry` handles allocation and deconfliction at runtime. 사전 포트 가용성 검사는 불필요하다.

### 8.2 루프백 디바이스 가용성

| 차원 | 적용 대상 | 명세 |
|------|-----------|------|
| **loop device** | ZFS, LVM, iSCSI | 루프백 디바이스로 스토리지 풀 생성 |

**검증 명령:**

```bash
losetup -f >/dev/null 2>&1 && echo "PASS: free loop device available" || echo "FAIL: no free loop device"
```

**기대 출력 패턴:**

```
PASS: free loop device available
```

### 8.3 디스크 공간 (Disk Space)

| 차원 | 적용 대상 | 명세 |
|------|-----------|------|
| **디스크 공간** | 공통 | Docker 이미지, Kind 노드, ZFS/LVM loopback 파일을 위해 최소 10GB 여유 공간 필요 |

**검증 명령:**

```bash
avail_kb=$(df --output=avail / 2>/dev/null | tail -1 | tr -d ' ')
avail_gb=$(( avail_kb / 1048576 ))
if [ "${avail_gb}" -ge 10 ] 2>/dev/null; then
  echo "PASS: ${avail_gb}GB available at /"
else
  echo "FAIL: ${avail_gb}GB available at / (need >= 10GB)"
fi
```

**기대 출력 패턴:**

```
PASS: [0-9]+GB available at /
```

### 8.4 configfs 마운트 확인

| 차원 | 적용 대상 | 명세 |
|------|-----------|------|
| **configfs** | NVMe-oF | `/sys/kernel/config`가 configfs로 마운트되어야 함 |

**검증 명령:**

```bash
mount | grep -q 'configfs on /sys/kernel/config' && echo "PASS: configfs mounted" || echo "FAIL: configfs not mounted at /sys/kernel/config"
```

**기대 출력 패턴:**

```
PASS: configfs mounted
```

---

## 통합 검증 스크립트

모든 사전조건을 한 번에 검증하는 명령 모음:

```bash
#!/bin/bash
# pillar-csi E2E precondition check (shell equivalent of prereq.CheckHostPrerequisites)
set -euo pipefail
FAIL=0

echo "=== Docker daemon ==="
timeout 10 docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 && echo "PASS: Docker" || { echo "FAIL: Docker"; FAIL=1; }

echo "=== Kernel modules ==="
for mod in zfs dm_mod dm_thin_pool nvme_fabrics nvmet nvmet_tcp nvme_tcp target_core_mod iscsi_target_mod; do
  grep -q "^${mod} " /proc/modules && echo "PASS: ${mod}" || { echo "FAIL: ${mod}"; FAIL=1; }
done

echo "=== Binaries ==="
for bin in kind helm zfs zpool lvcreate vgcreate; do
  which "${bin}" >/dev/null 2>&1 && echo "PASS: ${bin}" || { echo "FAIL: ${bin}"; FAIL=1; }
done

echo "=== NVMe-oF configfs ==="
test -d /sys/kernel/config/nvmet && echo "PASS: nvmet configfs" || { echo "FAIL: nvmet configfs"; FAIL=1; }
test -e /dev/nvme-fabrics && echo "PASS: /dev/nvme-fabrics" || { echo "FAIL: /dev/nvme-fabrics"; FAIL=1; }

echo "=== LVM device-mapper ==="
test -e /dev/mapper/control && echo "PASS: /dev/mapper/control" || { echo "FAIL: /dev/mapper/control"; FAIL=1; }

echo "=== envtest (integration only) ==="
ls bin/k8s/*/kube-apiserver >/dev/null 2>&1 && echo "PASS: envtest binaries" || { echo "FAIL: envtest binaries missing (run 'make setup-envtest')"; FAIL=1; }

echo "=== Disk space ==="
avail_kb=$(df --output=avail / 2>/dev/null | tail -1 | tr -d ' ')
avail_gb=$(( avail_kb / 1048576 ))
if [ "${avail_gb}" -ge 10 ] 2>/dev/null; then
  echo "PASS: ${avail_gb}GB available at /"
else
  echo "FAIL: ${avail_gb}GB available at / (need >= 10GB)"; FAIL=1
fi

echo "=== Helm chart ==="
test -f charts/pillar-csi/Chart.yaml && echo "PASS: Helm chart" || { echo "FAIL: Helm chart"; FAIL=1; }

echo ""
if [ "${FAIL}" -eq 0 ]; then
  echo "All preconditions PASSED"
else
  echo "Some preconditions FAILED — fix issues above before running E2E tests"
  exit 1
fi
```

**기대 출력 패턴 (전체 통과):**

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
PASS: target_core_mod
PASS: iscsi_target_mod
=== Binaries ===
PASS: kind
PASS: helm
PASS: zfs
PASS: zpool
PASS: lvcreate
PASS: vgcreate
=== NVMe-oF configfs ===
PASS: nvmet configfs
PASS: /dev/nvme-fabrics
=== LVM device-mapper ===
PASS: /dev/mapper/control
=== envtest (integration only) ===
PASS: envtest binaries
=== Disk space ===
PASS: .*GB available at .*
=== Helm chart ===
PASS: Helm chart

All preconditions PASSED
```

---

## 사전조건 매트릭스 요약

| 기술 | 커널 모듈 | 바이너리 | configfs/장치 | 파일시스템 | 네트워크 |
|------|-----------|----------|---------------|------------|----------|
| **ZFS** | `zfs` | `zfs`, `zpool` | — | loop device | — |
| **LVM** | `dm_mod`, `dm_thin_pool` | `lvcreate`, `vgcreate` | `/dev/mapper/control` | loop device | — |
| **NVMe-oF** | `nvme_fabrics`, `nvme_tcp`, `nvmet`, `nvmet_tcp` | — | `/sys/kernel/config/nvmet`, `/dev/nvme-fabrics` | — | TCP (동적 포트, `ports.Registry`) |
| **iSCSI** | `target_core_mod`, `iscsi_target_mod` (호스트, integration) | LIO configfs (integration) / tgtd (Kind E2E) | `/sys/kernel/config/target/iscsi/` (integration) | loop device (Kind 내부) | TCP (동적 포트, Kind 내부) |
| **envtest** | — | `kube-apiserver`, `etcd` (via setup-envtest) | — | CRD YAML | — |
| **Kind** | — | `kind` | — | 10GB+ 디스크 | Docker socket |
| **Helm** | — | `helm` | — | Chart.yaml | — |

> **참조 구현**: `test/e2e/framework/prereq/prereq.go` — AC 10 계약에 따른 hard-fail 사전조건 검사의 Go 구현체
