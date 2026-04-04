# ZFS 테스트 인프라 전략 (SSOT)

> **수준 B** — 규칙 + 검증 명령 + 기대 출력 패턴
>
> **적용 범위:** pillar-csi E2E/Integration 테스트에서 ZFS zpool을 사용하는 모든 TC
> (E35, ZFS backend integration 등)
>
> **크로스레퍼런스:**
> - [E2E 테스트 케이스](../E2E-TESTS.md) — E35: ZFS Kind 클러스터 E2E
> - [Integration 테스트](../INTEGRATION-TESTS.md) — ZFS backend integration (계획)
> - [테스트 전략 README](../README.md) — 테스트 피라미드 및 분류 기준
> - 프레임워크 코드: `test/e2e/framework/zfs/zfs.go`, `zfs_kubectl.go`

---

## 1. 호스트 사전조건 (Host Prerequisites)

ZFS E2E 테스트는 커널 모듈과 사용자 공간 도구 모두가 필요하다.

### 규칙

- `zfs` 커널 모듈이 로드되어 있어야 한다.
- `zpool`, `zfs` CLI 도구가 Kind 노드 컨테이너 내부에서 사용 가능해야 한다.
- `losetup`, `truncate`/`dd` 도구가 Kind 노드 컨테이너 내부에서 사용 가능해야 한다.
- GHA ubuntu-latest에서는 `zfsutils-linux` 패키지 설치 필요.
- Kind 노드 이미지에 ZFS 도구가 포함되어 있어야 하거나, 특권 Pod를 통해 호스트 도구에 접근해야 한다.

### 검증 명령 및 기대 출력

**커널 모듈 확인:**

```bash
# Kind 노드 컨테이너에서 실행 (docker exec 경유)
docker exec <NODE_CONTAINER> modinfo zfs
```

**기대 출력 패턴:**
```
filename:       /lib/modules/.*/zfs\.ko.*
description:    ZFS
license:        .*
```

**ZFS CLI 도구 확인:**

```bash
docker exec <NODE_CONTAINER> which zpool
docker exec <NODE_CONTAINER> which zfs
```

**기대 출력 패턴:**
```
/usr/sbin/zpool
/usr/sbin/zfs
```

**루프 디바이스 도구 확인:**

```bash
docker exec <NODE_CONTAINER> which losetup
docker exec <NODE_CONTAINER> which truncate
```

**기대 출력 패턴:**
```
/usr/sbin/losetup
/usr/bin/truncate
```

**GHA CI 사전 설치:**

```yaml
- name: Install ZFS
  run: |
    sudo apt-get update
    sudo apt-get install -y zfsutils-linux
    sudo modprobe zfs
    lsmod | grep zfs
```

**기대 출력 패턴 (lsmod):**
```
zfs\s+\d+\s+\d+
```

---

## 2. 리소스 생성 (Pool Creation)

### 규칙

- 테스트용 zpool은 루프백 디바이스 위에 생성한다 — 호스트 디스크를 직접 사용하지 않는다.
- 이미지 파일은 `/tmp/zfs-pool-<PoolName>.img` 경로에 sparse 파일로 생성한다 (truncate).
- 기본 크기는 512 MiB이며, TC에서 별도 지정하지 않으면 이 기본값을 사용한다.
- 풀 이름은 테스트별로 고유해야 하며, 접두사 `e2e-` + 랜덤 suffix 패턴을 따른다.
- `CreatePool` 또는 `CreatePoolViaKubectl` 함수를 통해서만 생성한다 — 직접 쉘 명령 호출 금지.

### 검증 명령 및 기대 출력

**이미지 파일 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls -la /tmp/zfs-pool-<POOL_NAME>.img
```

**기대 출력 패턴:**
```
-rw-r--r--.*\d+.*\/tmp\/zfs-pool-<POOL_NAME>\.img
```

**루프 디바이스 연결 확인:**

```bash
docker exec <NODE_CONTAINER> losetup -a | grep <POOL_NAME>
```

**기대 출력 패턴:**
```
/dev/loop\d+:.*\/tmp\/zfs-pool-<POOL_NAME>\.img
```

**풀 생성 확인:**

```bash
docker exec <NODE_CONTAINER> zpool list <POOL_NAME>
```

**기대 출력 패턴:**
```
NAME\s+SIZE\s+ALLOC\s+FREE\s+.*HEALTH.*
<POOL_NAME>\s+\d+(\.\d+)?[MGTP]\s+.*ONLINE
```

**풀 상태 확인 (단순):**

```bash
docker exec <NODE_CONTAINER> zpool list -H -o health <POOL_NAME>
```

**기대 출력:**
```
ONLINE
```

---

## 3. 리소스 정리 (Pool Destruction)

### 규칙

- 정리는 반드시 3단계로 수행: `zpool destroy -f` → `losetup -d` → `rm -f`.
- 모든 단계는 항상 실행 — 앞 단계 실패가 뒷 단계를 차단하지 않는다.
- "no such pool" 에러는 멱등성을 위해 무시한다.
- "no such device" 에러(루프 디바이스)는 멱등성을 위해 무시한다.
- nil Pool에 대한 Destroy 호출은 안전한 no-op이다.
- `Pool.Destroy()` 또는 `DestroyPoolViaKubectl()` 함수를 통해서만 정리 — 직접 명령 금지.

### 검증 명령 및 기대 출력

**정리 후 풀 부재 확인:**

```bash
docker exec <NODE_CONTAINER> zpool list <POOL_NAME> 2>&1
```

**기대 출력 패턴 (에러):**
```
cannot open '<POOL_NAME>': no such pool
```

**정리 후 루프 디바이스 부재 확인:**

```bash
docker exec <NODE_CONTAINER> losetup -a | grep <POOL_NAME>
```

**기대 출력:**
```
(빈 출력 — 해당 풀 이름이 포함된 루프 디바이스가 없음)
```

**정리 후 이미지 파일 부재 확인:**

```bash
docker exec <NODE_CONTAINER> ls /tmp/zfs-pool-<POOL_NAME>.img 2>&1
```

**기대 출력 패턴:**
```
.*No such file or directory
```

---

## 4. TC간 격리 (Test Case Isolation)

### 규칙

- 각 TC는 고유한 풀 이름을 사용한다 — `e2e-tank-<RANDOM_SUFFIX>` 형식.
- TC 간 ZFS 풀 공유 금지 — 각 TC는 자체 풀을 생성하고 자체적으로 정리한다.
- 병렬 실행 시 풀 이름 충돌 방지를 위해 `names.Registry`에서 고유 이름을 발급받는다.
- zvol, 스냅샷, 클론은 해당 풀 내부에 생성되므로, 풀 단위 격리가 곧 완전 격리이다.
- 루프 디바이스 번호는 커널의 `LOOP_CTL_GET_FREE` ioctl로 원자적 할당 — 병렬 TC 간 충돌 없음.

### 검증 명령 및 기대 출력

**현재 존재하는 모든 e2e 풀 목록:**

```bash
docker exec <NODE_CONTAINER> zpool list -H -o name | grep '^e2e-'
```

**기대 출력 (TC 완료 후):**
```
(빈 출력 — 모든 e2e 풀이 정리됨)
```

**Suite 시작 전 잔여 풀 확인 (scavenger):**

```bash
docker exec <NODE_CONTAINER> zpool list -H -o name 2>/dev/null | grep '^e2e-' | wc -l
```

**기대 출력:**
```
0
```

---

## 5. 사이징 (Sizing)

### 규칙

- 기본 풀 크기: 512 MiB (sparse 파일 — 실제 디스크 사용량은 데이터 기록 시까지 0에 가까움).
- GHA ubuntu-latest 러너의 `/tmp` 파티션: 최소 14 GB 여유 공간.
- 병렬 TC 5개 기준, 최대 5 × 512 MiB = 2.5 GiB sparse 파일 (실제 사용량은 이보다 훨씬 적음).
- zvol 크기는 풀 크기의 50%를 초과하지 않도록 설정 (여유 공간 유지).
- 풀 용량 고갈 테스트(E-FAULT-3)는 별도의 소형 풀(64 MiB)을 사용한다.

### 검증 명령 및 기대 출력

**호스트 /tmp 여유 공간 확인:**

```bash
df -BM /tmp | tail -1 | awk '{print $4}'
```

**기대 출력 패턴 (최소 4 GiB 여유):**
```
\d{4,}M
```

**풀 크기 확인:**

```bash
docker exec <NODE_CONTAINER> zpool list -H -o size <POOL_NAME>
```

**기대 출력 패턴:**
```
(480M|496M|512M|528M)
```
> ZFS 메타데이터 오버헤드로 인해 정확히 512M이 아닐 수 있음.

**풀 여유 공간 확인:**

```bash
docker exec <NODE_CONTAINER> zpool list -H -o free <POOL_NAME>
```

**기대 출력 패턴:**
```
\d+(\.\d+)?[MG]
```

---

## 6. 실패 시 정리 (Failure Cleanup)

### 규칙

- `CreatePool` 실패 시 이미 생성된 리소스(이미지 파일, 루프 디바이스)를 best-effort로 정리한다.
- 테스트 실패/panic 시에도 `defer pool.Destroy(ctx)` 패턴으로 정리가 보장되어야 한다.
- `registry.Resource` 인터페이스를 통해 프레임워크가 미정리 리소스를 추적하고 suite 종료 시 강제 정리.
- 루프 디바이스 이미지는 `/tmp` 아래에 위치하므로, 컨테이너 재시작 시 자동 정리.
- 정리 에러는 수집하여 한꺼번에 반환 — 앞 단계 실패가 뒷 단계를 차단하지 않음.

### 검증 명령 및 기대 출력

**Suite 종료 후 잔여 리소스 감사:**

```bash
# 풀 잔여 확인
docker exec <NODE_CONTAINER> zpool list -H -o name 2>/dev/null | grep '^e2e-'

# 루프 디바이스 잔여 확인
docker exec <NODE_CONTAINER> losetup -a | grep 'zfs-pool-e2e'

# 이미지 파일 잔여 확인
docker exec <NODE_CONTAINER> ls /tmp/zfs-pool-e2e-*.img 2>/dev/null
```

**기대 출력 (모두 정리됨):**
```
(각 명령 모두 빈 출력)
```

**멱등 정리 확인 (이중 Destroy):**

```bash
# 이미 정리된 풀에 대해 다시 destroy — 에러 없이 성공해야 함
docker exec <NODE_CONTAINER> zpool destroy -f <POOL_NAME> 2>&1 || true
```

**기대 출력 패턴:**
```
(빈 출력 또는 "cannot open.*no such pool")
```

---

## 7. CI 호환성 (CI Compatibility)

### 규칙

- CI 환경: GitHub Actions `ubuntu-latest`.
- ZFS 커널 모듈은 기본 제공되지 않으므로 `zfsutils-linux` 패키지 설치 후 `modprobe zfs` 필요.
- Kind 노드 컨테이너는 `--privileged`로 실행되어 루프 디바이스 접근 가능.
- Docker socket 접근 불가 시 `kubectl exec` 경유 모드 사용 (KubectlExecOptions).
- DOCKER_HOST는 환경변수에서만 읽음 — 하드코딩 금지.
- ZFS 풀 생성/파괴 작업은 직렬화 없이 병렬 실행 가능 (커널 레벨에서 풀 이름 기반 격리).

### 검증 명령 및 기대 출력

**GHA runner에서 ZFS 모듈 로드 확인:**

```bash
lsmod | grep '^zfs'
```

**기대 출력 패턴:**
```
zfs\s+\d+\s+\d+
```

**Kind 컨테이너 특권 모드 확인:**

```bash
docker inspect <NODE_CONTAINER> --format '{{.HostConfig.Privileged}}'
```

**기대 출력:**
```
true
```

**Docker socket 접근 확인 (docker exec 모드):**

```bash
docker info --format '{{.ServerVersion}}' 2>/dev/null
```

**기대 출력 패턴:**
```
\d+\.\d+\.\d+
```

**kubectl exec 모드 대체 확인 (docker socket 부재 시):**

```bash
kubectl exec -n kube-system <POD_NAME> -- zpool version
```

**기대 출력 패턴:**
```
zfs-\d+\.\d+\.\d+
```

---

## 8. 스냅샷 및 클론 (Snapshot & Clone)

### 규칙

- ZFS 스냅샷은 `<pool>/<dataset>@<snap-name>` 형식으로 생성한다.
- 스냅샷 이름은 TC별로 고유해야 한다 — `snap-<TC_ID>-<RANDOM>` 형식.
- 클론은 스냅샷으로부터 생성 — 스냅샷 없이 직접 클론 생성 불가.
- 클론 삭제 전 스냅샷 삭제 시도 시 "has dependent clones" 에러 발생.
- 정리 순서: 클론 삭제 → 스냅샷 삭제 → (필요 시) 원본 zvol 삭제.

### 검증 명령 및 기대 출력

**스냅샷 생성 확인:**

```bash
docker exec <NODE_CONTAINER> zfs list -t snapshot -H -o name <POOL>/<DATASET>@<SNAP>
```

**기대 출력:**
```
<POOL>/<DATASET>@<SNAP>
```

**스냅샷 목록 확인:**

```bash
docker exec <NODE_CONTAINER> zfs list -t snapshot -H -o name -r <POOL>
```

**기대 출력 패턴:**
```
<POOL>/.*@.*
```

**클론 생성 확인:**

```bash
docker exec <NODE_CONTAINER> zfs list -H -o origin <POOL>/<CLONE_NAME>
```

**기대 출력:**
```
<POOL>/<DATASET>@<SNAP>
```

**클론 정리 후 스냅샷 삭제 가능 확인:**

```bash
docker exec <NODE_CONTAINER> zfs destroy <POOL>/<DATASET>@<SNAP>
echo $?
```

**기대 출력:**
```
0
```

---

## 9. 상태 조회 (State Inspection)

### 규칙

- 풀 상태 조회는 `zpool list -H -o health` 명령으로 수행한다.
- 정상 상태: `ONLINE`. 다른 상태(`DEGRADED`, `FAULTED`, `OFFLINE`, `UNAVAIL`, `REMOVED`)는 테스트 실패로 처리.
- `VerifyOnline()` 함수가 풀 생성 후 즉시 호출되어 풀 건전성을 확인한다.
- 풀 속성 조회: `zpool get all <POOL_NAME>` 또는 특정 속성 `zpool get <prop> <POOL_NAME>`.

### 검증 명령 및 기대 출력

**풀 건전성 확인:**

```bash
docker exec <NODE_CONTAINER> zpool list -H -o health <POOL_NAME>
```

**기대 출력:**
```
ONLINE
```

**풀 상세 상태:**

```bash
docker exec <NODE_CONTAINER> zpool status <POOL_NAME>
```

**기대 출력 패턴:**
```
\s*pool:\s*<POOL_NAME>
\s*state:\s*ONLINE
\s*config:
.*
\s*errors:\s*No known data errors
```

**zvol 존재 확인:**

```bash
docker exec <NODE_CONTAINER> zfs list -H -o name <POOL>/<ZVOL>
```

**기대 출력:**
```
<POOL>/<ZVOL>
```

**zvol 크기 확인:**

```bash
docker exec <NODE_CONTAINER> zfs get -H -o value volsize <POOL>/<ZVOL>
```

**기대 출력 패턴:**
```
\d+(\.\d+)?[MGTP]
```

**풀 전체 속성 조회:**

```bash
docker exec <NODE_CONTAINER> zpool get -H -o property,value health,size,free <POOL_NAME>
```

**기대 출력 패턴:**
```
health\s+ONLINE
size\s+\d+(\.\d+)?[MGTP]
free\s+\d+(\.\d+)?[MGTP]
```

---

## 차원 요약 매트릭스

| 차원 | 규칙 수 | 검증 명령 수 | 핵심 도구 |
|------|--------|------------|----------|
| 호스트 사전조건 | 5 | 4 | `modinfo`, `which`, `apt-get` |
| 리소스 생성 | 5 | 4 | `zpool create`, `losetup`, `truncate` |
| 리소스 정리 | 6 | 4 | `zpool destroy`, `losetup -d`, `rm -f` |
| TC간 격리 | 5 | 2 | `zpool list`, `names.Registry` |
| 사이징 | 5 | 3 | `df`, `zpool list` |
| 실패 시 정리 | 5 | 3 | `zpool list`, `losetup -a`, `ls /tmp` |
| CI 호환성 | 6 | 4 | `lsmod`, `docker inspect`, `kubectl` |
| 스냅샷/클론 | 5 | 4 | `zfs snapshot`, `zfs clone`, `zfs list` |
| 상태 조회 | 4 | 5 | `zpool list`, `zpool status`, `zfs get` |
