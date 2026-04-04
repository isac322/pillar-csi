# Kind 테스트 인프라 전략 (SSOT)

> **수준 B** — 규칙 + 검증 명령 + 기대 출력 패턴
>
> **적용 범위:** pillar-csi E2E 테스트에서 Kind(Kubernetes in Docker) 클러스터를 사용하는 모든 TC
> (E10, E33–E35, E-FAULT-*, Helm 배포 등)
>
> **크로스레퍼런스:**
> - [E2E 테스트 케이스](../E2E-TESTS.md) — Kind 기반 전체 E2E TC 목록
> - [Integration 테스트](../INTEGRATION-TESTS.md) — backend/Helm integration
> - [테스트 전략 README](../README.md) — 테스트 피라미드 및 분류 기준
> - [인프라 전략 인덱스](README.md) — 공통 원칙 및 기술 간 의존 관계
> - [LVM 인프라](LVM.md), [ZFS 인프라](ZFS.md) — Kind 컨테이너 내부 스토리지 설정
> - [NVMe-oF 인프라](NVMEOF.md), [iSCSI 인프라](ISCSI.md) — Kind 바인드 마운트 요구사항
> - [Helm 인프라](HELM.md) — Kind 클러스터 위에 Helm 배포
> - 프레임워크 코드: `test/e2e/framework/kind/kind.go`, `backend_daemonset.go`, `fabric_daemonset.go`
> - 부트스트랩: `test/e2e/kind_bootstrap.go`, `kind_reaper.go`

---

## 1. 호스트 사전조건 (Host Prerequisites)

Kind는 Docker 위에서 K8s 클러스터를 실행하므로 Docker 데몬과 kind CLI가 필요하다.

### 규칙

- Docker 데몬이 실행 중이어야 한다 — `docker info`가 성공해야 한다.
- `kind` CLI 도구가 설치되어 있어야 한다 — CI에서는 `helm/kind-action@v1`의 `install_only: true`.
- Kind 버전: `v0.27.0` (CI 환경 변수 `KIND_VERSION`에서 관리).
- `DOCKER_HOST` 환경변수로 Docker 데몬 엔드포인트 지정 — 하드코딩 금지.
- `kubectl` CLI 도구 필요 — Kind가 kubeconfig를 자동 설정.
- `helm` CLI 도구 필요 — Helm 배포 TC에서 사용 ([Helm 인프라](HELM.md) 참조).
- 커널 모듈 사전 로드 필요 — `zfs`, `dm_mod`, `dm_thin_pool`, `nvme_fabrics`, `nvmet`, `nvmet_tcp`, `nvme_tcp`.

### 검증 명령 및 기대 출력

**Docker 데몬 실행 확인:**

```bash
docker info --format '{{.ServerVersion}}'
```

**기대 출력 패턴:**
```
\d+\.\d+\.\d+
```

**kind CLI 버전 확인:**

```bash
kind version
```

**기대 출력 패턴:**
```
kind v0\.\d+\.\d+.*
```

**kubectl 설치 확인:**

```bash
kubectl version --client --output=yaml 2>/dev/null | grep gitVersion
```

**기대 출력 패턴:**
```
gitVersion: v\d+\.\d+\.\d+
```

**DOCKER_HOST 환경변수 확인:**

```bash
echo "${DOCKER_HOST:-unix:///var/run/docker.sock}"
```

**기대 출력 패턴:**
```
unix:///var/run/docker\.sock
```

**GHA CI 사전 설치:**

```yaml
- name: Install Kind
  uses: helm/kind-action@v1
  with:
    install_only: true
    version: v0.27.0

- name: Install Helm
  uses: azure/setup-helm@v5
```

---

## 2. 리소스 생성 (Cluster Creation)

### 규칙

- 클러스터 이름: `KIND_CLUSTER` 환경변수 (기본값: `pillar-csi-e2e`).
- 노드 구성: 1 control-plane + 2 worker 노드 (kind-config.yaml에 정의).
- kind-config.yaml의 `extraMounts` 설정으로 호스트 리소스를 컨테이너에 바인드 마운트:
  - `/dev/mapper` → Bidirectional (LVM device-mapper).
  - `/dev/nvme-fabrics` → HostToContainer (NVMe-oF).
  - `/sys/kernel/config` → Bidirectional (configfs: NVMe-oF, iSCSI).
- 클러스터 생성 후 이미지 빌드 → `kind load docker-image`로 Kind 노드에 로드.
- `E2E_USE_EXISTING_CLUSTER=true` 시 기존 클러스터 재사용 — 빠른 반복 개발용.
- `E2E_SKIP_IMAGE_BUILD=true` 시 이미지 빌드 스킵 — 이전 빌드 재사용.
- 이미지 태그: `E2E_IMAGE_TAG` (기본값: `e2e`).
- 부트스트랩 순서: prereq 확인 → 클러스터 생성 → 이미지 빌드/로드 → backend 설정 → 테스트 실행 → teardown.

### 검증 명령 및 기대 출력

**클러스터 생성 확인:**

```bash
kind get clusters | grep pillar-csi-e2e
```

**기대 출력:**
```
pillar-csi-e2e
```

**노드 상태 확인:**

```bash
kubectl get nodes --context kind-pillar-csi-e2e -o wide --no-headers
```

**기대 출력 패턴:**
```
pillar-csi-e2e-control-plane\s+Ready\s+control-plane.*
pillar-csi-e2e-worker\s+Ready\s+<none>.*
pillar-csi-e2e-worker2\s+Ready\s+<none>.*
```

**Kind 컨테이너 실행 확인:**

```bash
docker ps --filter "label=io.x-k8s.kind.cluster=pillar-csi-e2e" --format '{{.Names}}'
```

**기대 출력 패턴:**
```
pillar-csi-e2e-control-plane
pillar-csi-e2e-worker
pillar-csi-e2e-worker2
```

**extraMounts 바인드 마운트 확인 (/dev/mapper):**

```bash
docker exec pillar-csi-e2e-control-plane ls /dev/mapper/control
```

**기대 출력:**
```
/dev/mapper/control
```

**extraMounts 바인드 마운트 확인 (configfs):**

```bash
docker exec pillar-csi-e2e-control-plane ls -d /sys/kernel/config/nvmet/
```

**기대 출력:**
```
/sys/kernel/config/nvmet/
```

**이미지 로드 확인:**

```bash
docker exec pillar-csi-e2e-control-plane crictl images | grep pillar-csi
```

**기대 출력 패턴:**
```
.*pillar-csi.*e2e.*
```

---

## 3. 리소스 정리 (Cluster Deletion)

### 규칙

- `kind delete cluster --name <CLUSTER_NAME>`으로 전체 클러스터 삭제.
- `make test-e2e` 타겟이 테스트 종료 후 자동으로 클러스터 삭제.
- `E2E_USE_EXISTING_CLUSTER=true` 시에는 삭제하지 않음 (개발자 워크플로).
- `kind_reaper.go`가 orphan 클러스터를 검출하고 정리하는 로직 제공.
- Kind 노드 컨테이너가 삭제되면 내부 루프 디바이스/VG/ZFS 풀도 자동 정리.
- Docker volume/network도 Kind가 자동 정리 — 수동 정리 불필요.

### 검증 명령 및 기대 출력

**클러스터 삭제 후 확인:**

```bash
kind get clusters 2>/dev/null | grep pillar-csi-e2e
```

**기대 출력:**
```
(빈 출력)
```

**Docker 컨테이너 잔여 확인:**

```bash
docker ps -a --filter "label=io.x-k8s.kind.cluster=pillar-csi-e2e" --format '{{.Names}}' | wc -l
```

**기대 출력:**
```
0
```

**Docker network 잔여 확인:**

```bash
docker network ls --filter "label=io.x-k8s.kind.cluster=pillar-csi-e2e" --format '{{.Name}}' | wc -l
```

**기대 출력:**
```
0
```

---

## 4. TC간 격리 (Test Case Isolation)

### 규칙

- 모든 E2E TC는 동일한 Kind 클러스터를 공유한다 — TC마다 클러스터 생성은 비용이 너무 높음.
- TC 간 격리는 **K8s namespace** 단위로 수행 — `namespace.Manager`가 TC별 고유 namespace 생성.
- cluster-scoped 리소스(PillarTarget, PillarPool, PillarBinding)는 이름에 TC 고유 접미사 포함.
- backend 리소스(LVM VG, ZFS pool)는 TC별로 독립 생성 — [LVM 격리](LVM.md#4-tc간-격리-test-case-isolation), [ZFS 격리](ZFS.md#4-tc간-격리-test-case-isolation) 참조.
- protocol 리소스(NVMe-oF target, iSCSI target)는 TC별로 독립 포트 + NQN/IQN 사용.
- Ginkgo `Ordered` 컨테이너 내 TC는 순서 보장 — 같은 워커에서 직렬 실행.
- Ginkgo 병렬 워커 간: namespace + 리소스 이름으로 완전 격리.

### 검증 명령 및 기대 출력

**TC 전용 namespace 존재 확인 (테스트 실행 중):**

```bash
kubectl get namespaces --context kind-pillar-csi-e2e -o name | grep 'test-'
```

**기대 출력 패턴 (실행 중):**
```
namespace/test-.*
```

**TC 완료 후 namespace 정리 확인:**

```bash
kubectl get namespaces --context kind-pillar-csi-e2e -o name | grep 'test-' | wc -l
```

**기대 출력 (Suite 완료 후):**
```
0
```

**cluster-scoped 리소스 정리 확인:**

```bash
kubectl get pillartargets --context kind-pillar-csi-e2e -o name 2>/dev/null | grep 'e2e-' | wc -l
```

**기대 출력:**
```
0
```

---

## 5. 사이징 (Sizing)

### 규칙

- Kind 클러스터 생성 시간: ~30초 (노드 3개 기준).
- 이미지 빌드 + 로드 시간: ~20초 (Docker 레이어 캐시 활용 시).
- Docker 메모리: 노드당 ~200 MB × 3 = ~600 MB 기본 + 워크로드.
- GHA ubuntu-latest: 7 GB RAM, 2 CPU, 14 GB 디스크 — Kind 실행에 충분.
- E2E 전체 Suite 시간 예산: 120초 (CI timing gate).
- Ginkgo 병렬 워커: `E2E_PROCS=8` (CI), 기본값 `4` (로컬).
- Docker 이미지 캐시: pre-pull로 Docker Hub rate limit 회피.
- third-party sidecar 이미지: `docker save | docker cp` 로 Kind 노드에 직접 로드.

### 검증 명령 및 기대 출력

**Docker 리소스 사용량 확인:**

```bash
docker stats --no-stream --format "table {{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}" | grep pillar-csi-e2e
```

**기대 출력 패턴:**
```
pillar-csi-e2e-control-plane\s+\d+(\.\d+)?MiB.*
pillar-csi-e2e-worker\s+\d+(\.\d+)?MiB.*
pillar-csi-e2e-worker2\s+\d+(\.\d+)?MiB.*
```

**호스트 디스크 여유 확인:**

```bash
df -BG / | tail -1 | awk '{print $4}'
```

**기대 출력 패턴 (최소 5 GB 여유):**
```
\d+G
```

**E2E timing gate 확인 (CI 로그):**

```bash
# CI 실행 후 step output에서 확인
echo "elapsed=${E2E_ELAPSED}" # 120초 이내여야 함
```

**기대 출력 패턴:**
```
elapsed=\d{1,3}
```
> 값이 120 이하여야 CI 통과.

---

## 6. 실패 시 정리 (Failure Cleanup)

### 규칙

- `make test-e2e` 타겟이 `trap` 또는 종료 시 `kind delete cluster` 실행.
- `kind_reaper.go`가 stale 클러스터를 검출: 일정 시간 이상 존재하는 `pillar-csi-e2e-*` 클러스터 삭제.
- TC 실패 시에도 Ginkgo의 `AfterEach`/`DeferCleanup`이 namespace 및 리소스 정리.
- Kind 노드 컨테이너 crash 시 Docker가 자동 재시작하지 않음 (`restart: no`).
- CI에서는 Job 종료 시 runner의 모든 Docker 컨테이너가 정리됨.
- 로컬 개발 시 수동 정리: `kind delete cluster --name pillar-csi-e2e`.

### 검증 명령 및 기대 출력

**orphan Kind 클러스터 검색:**

```bash
kind get clusters 2>/dev/null | grep 'pillar-csi'
```

**기대 출력 (CI 종료 후):**
```
(빈 출력)
```

**stale Docker 컨테이너 검색:**

```bash
docker ps -a --filter "label=io.x-k8s.kind.cluster" --filter "status=exited" --format '{{.Names}}'
```

**기대 출력:**
```
(빈 출력)
```

**로컬 수동 정리:**

```bash
kind delete cluster --name pillar-csi-e2e 2>&1
```

**기대 출력 패턴:**
```
Deleting cluster "pillar-csi-e2e" \.\.\.
Deleted nodes:.*
```

---

## 7. CI 호환성 (CI Compatibility)

### 규칙

- CI 환경: GitHub Actions `ubuntu-latest`.
- Kind 설치: `helm/kind-action@v1` (install_only: true) — 클러스터 생성은 TestMain이 담당.
- Helm 설치: `azure/setup-helm@v5`.
- Docker: GHA runner에 기본 설치 — 추가 설치 불필요.
- DOCKER_HOST: CI에서 `unix:///var/run/docker.sock`으로 명시적 설정.
- Docker Hub rate limit 회피: base image + sidecar image를 step에서 사전 pull.
- `E2E_PROCS=8`: CI에서 8개 Ginkgo 병렬 워커.
- `E2E_FAIL_FAST=true`: CI에서 첫 실패 시 나머지 TC 스킵 (빠른 피드백).
- Skip 조건: 커밋 메시지에 `[skip e2e]` 또는 `[e2e skip]` 포함 시 E2E Job 스킵.
- 2-pass 실행: internal-agent 모드 → external-agent 모드 (동일 클러스터 재사용).

### 검증 명령 및 기대 출력

**GHA runner Docker 버전 확인:**

```bash
docker version --format '{{.Server.Version}}'
```

**기대 출력 패턴:**
```
\d+\.\d+\.\d+
```

**Kind 노드 이미지 존재 확인:**

```bash
docker images --format '{{.Repository}}:{{.Tag}}' | grep kindest/node
```

**기대 출력 패턴:**
```
kindest/node:v\d+\.\d+\.\d+
```

**pre-pull 이미지 존재 확인:**

```bash
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '(csi-provisioner|csi-attacher|csi-resizer|livenessprobe|csi-node-driver-registrar)'
```

**기대 출력 패턴:**
```
registry\.k8s\.io/sig-storage/csi-provisioner:v\d+.*
registry\.k8s\.io/sig-storage/csi-attacher:v\d+.*
registry\.k8s\.io/sig-storage/csi-resizer:v\d+.*
registry\.k8s\.io/sig-storage/livenessprobe:v\d+.*
registry\.k8s\.io/sig-storage/csi-node-driver-registrar:v\d+.*
```

**E2E 환경변수 확인:**

```bash
echo "KIND_CLUSTER=${KIND_CLUSTER:-pillar-csi-e2e}"
echo "E2E_PROCS=${E2E_PROCS:-4}"
echo "E2E_FAIL_FAST=${E2E_FAIL_FAST:-false}"
```

**기대 출력:**
```
KIND_CLUSTER=pillar-csi-e2e
E2E_PROCS=8
E2E_FAIL_FAST=true
```

---

## 8. 멀티-패스 실행 전략 (추가 차원)

### 규칙

- E2E Suite는 2-pass로 실행: internal-agent 모드 → external-agent 모드.
- internal-agent: pillar-agent가 DaemonSet으로 Kind 노드 내부에서 실행.
- external-agent: pillar-agent가 Docker 컨테이너로 Kind 외부에서 실행.
- 두 모드 모두 동일 Kind 클러스터를 재사용 — 클러스터 재생성 없음.
- 각 pass에서 Helm 릴리스를 재설치/업그레이드.
- `E2E_STAGE_TIMING=1` 설정 시 각 단계의 wall-clock 시간 출력.
- `make test-e2e-internal`, `make test-e2e-external`로 개별 패스만 실행 가능.

### 검증 명령 및 기대 출력

**internal-agent DaemonSet 확인:**

```bash
kubectl get daemonset -n pillar-csi-system --context kind-pillar-csi-e2e -o name
```

**기대 출력 패턴:**
```
daemonset\.apps/pillar-agent
```

**external-agent Docker 컨테이너 확인:**

```bash
docker ps --filter "name=pillar-agent" --format '{{.Names}}'
```

**기대 출력 패턴:**
```
pillar-agent.*
```

**stage timing 출력 확인:**

```bash
E2E_STAGE_TIMING=1 make test-e2e 2>&1 | grep 'STAGE'
```

**기대 출력 패턴:**
```
STAGE.*\d+s
```

---

## 9. kubeconfig 관리 (추가 차원)

### 규칙

- Kind가 생성한 kubeconfig는 `kind get kubeconfig --name <CLUSTER>`로 추출.
- `KUBECONFIG` 환경변수로 테스트 프로세스에 전달.
- 병렬 Ginkgo 워커는 동일한 kubeconfig를 공유 (API 서버 접근은 thread-safe).
- kubeconfig의 server 주소: `https://127.0.0.1:<RANDOM_PORT>` (Kind가 Docker port-forward 설정).
- Kind 클러스터 삭제 시 kubeconfig도 자동으로 무효화.
- `E2E_USE_EXISTING_CLUSTER=true` 시 기존 `KUBECONFIG`를 그대로 사용.

### 검증 명령 및 기대 출력

**kubeconfig 유효성 확인:**

```bash
kubectl cluster-info --context kind-pillar-csi-e2e 2>&1
```

**기대 출력 패턴:**
```
Kubernetes control plane is running at https://127\.0\.0\.1:\d+
CoreDNS is running at.*
```

**API 서버 접근 확인:**

```bash
kubectl get --raw /healthz --context kind-pillar-csi-e2e
```

**기대 출력:**
```
ok
```

---

## 차원 요약 매트릭스

| 차원 | 규칙 수 | 검증 명령 수 | 핵심 도구 |
|------|--------|------------|----------|
| 호스트 사전조건 | 7 | 4 | `docker info`, `kind version`, `kubectl` |
| 리소스 생성 | 8 | 6 | `kind create cluster`, `kind load`, `docker exec` |
| 리소스 정리 | 6 | 3 | `kind delete cluster`, `docker ps` |
| TC간 격리 | 7 | 3 | namespace, cluster-scoped names, Ginkgo workers |
| 사이징 | 8 | 3 | `docker stats`, `df`, timing gate |
| 실패 시 정리 | 6 | 3 | `kind get clusters`, `docker ps`, reaper |
| CI 호환성 | 10 | 4 | Docker version, Kind image, pre-pull, env vars |
| 멀티-패스 실행 | 7 | 3 | DaemonSet, Docker container, stage timing |
| kubeconfig 관리 | 6 | 2 | `kubectl cluster-info`, `/healthz` |
