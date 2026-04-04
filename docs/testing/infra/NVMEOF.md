# NVMe-oF configfs 테스트 인프라 전략 (SSOT)

> **수준 B** — 규칙 + 검증 명령 + 기대 출력 패턴
>
> **적용 범위:** pillar-csi E2E/Integration 테스트에서 NVMe-oF TCP 프로토콜을 사용하는 모든 TC
> (E33, NVMe-oF configfs integration 등)
>
> **크로스레퍼런스:**
> - [E2E 테스트 케이스](../E2E-TESTS.md) — E33: LVM + NVMe-oF TCP E2E
> - [Integration 테스트](../INTEGRATION-TESTS.md) — NVMe-oF configfs integration
> - [테스트 전략 README](../README.md) — 테스트 피라미드 및 분류 기준
> - [인프라 전략 인덱스](README.md) — 공통 원칙 및 기술 간 의존 관계
> - [LVM 인프라](LVM.md) — NVMe-oF는 LVM backend와 함께 사용됨
> - 프레임워크 코드: `test/e2e/framework/ports/registry.go`
> - Agent 코드: `internal/agent/nvmeof/`

---

## 1. 호스트 사전조건 (Host Prerequisites)

NVMe-oF E2E 테스트는 4개의 커널 모듈과 configfs 접근이 필요하다.

### 규칙

- `nvmet` 커널 모듈이 로드되어 있어야 한다 — NVMe-oF target subsystem.
- `nvmet_tcp` 커널 모듈이 로드되어 있어야 한다 — TCP transport binding.
- `nvme_tcp` 커널 모듈이 로드되어 있어야 한다 — initiator(client) side TCP transport.
- `nvme_fabrics` 커널 모듈이 로드되어 있어야 한다 — `/dev/nvme-fabrics` 디바이스를 생성.
- `/sys/kernel/config/nvmet/` configfs 경로가 Kind 노드 컨테이너에 바인드 마운트되어 있어야 한다.
- `/dev/nvme-fabrics` 디바이스가 Kind 노드 컨테이너에 바인드 마운트되어 있어야 한다.
- GHA ubuntu-latest에서는 `linux-modules-extra-$(uname -r)` 패키지 설치 필요.
- `nvme-cli` 도구가 initiator 측 Kind 노드에서 사용 가능해야 한다.

### 검증 명령 및 기대 출력

**커널 모듈 확인 (호스트):**

```bash
lsmod | grep nvmet
lsmod | grep nvme_tcp
lsmod | grep nvme_fabrics
```

**기대 출력 패턴:**
```
nvmet_tcp\s+\d+\s+\d+
nvmet\s+\d+\s+\d+.*
nvme_tcp\s+\d+\s+\d+
nvme_fabrics\s+\d+\s+\d+
```

**configfs 마운트 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/nvmet/
```

**기대 출력:**
```
/sys/kernel/config/nvmet/
```

**/dev/nvme-fabrics 디바이스 확인:**

```bash
docker exec <NODE_CONTAINER> test -c /dev/nvme-fabrics && echo "OK" || echo "MISSING"
```

**기대 출력:**
```
OK
```

**nvme-cli 확인:**

```bash
docker exec <NODE_CONTAINER> nvme version
```

**기대 출력 패턴:**
```
nvme version \d+\.\d+.*
```

**GHA CI 사전 설치:**

```yaml
- name: Install NVMe kernel modules
  run: |
    sudo apt-get install -y linux-modules-extra-$(uname -r)
    sudo modprobe nvme_fabrics
    sudo modprobe nvmet
    sudo modprobe nvmet_tcp
    sudo modprobe nvme_tcp
    test -c /dev/nvme-fabrics && echo "nvme-fabrics device present"
```

**기대 출력:**
```
nvme-fabrics device present
```

---

## 2. 리소스 생성 (NVMe-oF Target Creation)

### 규칙

- NVMe-oF target은 configfs에 직접 파일/디렉토리를 생성하여 구성한다.
- subsystem은 `/sys/kernel/config/nvmet/subsystems/<NQN>/` 경로에 생성.
- NQN 형식: `nqn.2026-01.com.bhyoo.pillar-csi:<UNIQUE_SUFFIX>`.
- namespace는 subsystem 아래 `/namespaces/<NSID>/` 디렉토리로 생성.
- port는 `/sys/kernel/config/nvmet/ports/<PORT_ID>/` 경로에 생성.
- port의 transport type은 `tcp`로 설정, address family는 `ipv4`.
- 포트 번호는 `ports.Registry`에서 동적 할당 — 하드코딩 금지.
- subsystem과 port를 연결하기 위해 port 디렉토리 아래 subsystem으로의 symlink 생성.

### 검증 명령 및 기대 출력

**subsystem 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/nvmet/subsystems/<NQN>
```

**기대 출력:**
```
/sys/kernel/config/nvmet/subsystems/<NQN>
```

**subsystem 설정 확인:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/nvmet/subsystems/<NQN>/attr_allow_any_host
```

**기대 출력:**
```
1
```

**namespace 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/nvmet/subsystems/<NQN>/namespaces/1
```

**기대 출력:**
```
/sys/kernel/config/nvmet/subsystems/<NQN>/namespaces/1
```

**namespace 디바이스 경로 확인:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/nvmet/subsystems/<NQN>/namespaces/1/device_path
```

**기대 출력 패턴:**
```
/dev/<VG_NAME>/lv-.*
```

**namespace 활성화 확인:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/nvmet/subsystems/<NQN>/namespaces/1/enable
```

**기대 출력:**
```
1
```

**port 생성 및 transport 설정 확인:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/nvmet/ports/<PORT_ID>/addr_trtype
docker exec <NODE_CONTAINER> cat /sys/kernel/config/nvmet/ports/<PORT_ID>/addr_trsvcid
docker exec <NODE_CONTAINER> cat /sys/kernel/config/nvmet/ports/<PORT_ID>/addr_adrfam
```

**기대 출력:**
```
tcp
<PORT_NUMBER>
ipv4
```

**subsystem-port 연결 확인:**

```bash
docker exec <NODE_CONTAINER> ls -l /sys/kernel/config/nvmet/ports/<PORT_ID>/subsystems/ | grep <NQN>
```

**기대 출력 패턴:**
```
.*<NQN> -> .*
```

---

## 3. 리소스 정리 (NVMe-oF Target Destruction)

### 규칙

- 정리 순서 (역순): initiator disconnect → symlink 삭제 → namespace disable → namespace 삭제 → port 삭제 → subsystem 삭제.
- initiator side: `nvme disconnect -n <NQN>` 먼저 실행 — 연결된 상태에서 target 삭제 시 커널 panic 위험.
- namespace disable: `echo 0 > .../namespaces/<NSID>/enable` 후 `rmdir`.
- configfs 디렉토리는 `rmdir`로만 삭제 가능 — `rm -rf` 사용 불가.
- "No such file or directory" 에러는 멱등성을 위해 무시한다.
- 정리 에러는 수집하여 한꺼번에 반환 — 앞 단계 실패가 뒷 단계를 차단하지 않음.

### 검증 명령 및 기대 출력

**정리 후 subsystem 부재 확인:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/nvmet/subsystems/ | grep <NQN_SUFFIX>
```

**기대 출력:**
```
(빈 출력)
```

**정리 후 port 부재 확인:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/nvmet/ports/<PORT_ID> 2>&1
```

**기대 출력 패턴:**
```
.*No such file or directory
```

**initiator disconnect 확인:**

```bash
docker exec <NODE_CONTAINER> nvme list-subsys 2>/dev/null | grep <NQN>
```

**기대 출력:**
```
(빈 출력 — 해당 NQN에 연결된 subsystem 없음)
```

---

## 4. TC간 격리 (Test Case Isolation)

### 규칙

- 각 TC는 고유한 NQN을 사용한다 — `nqn.2026-01.com.bhyoo.pillar-csi:<TC_UNIQUE_SUFFIX>`.
- 각 TC는 고유한 TCP 포트를 사용한다 — `ports.Registry`에서 동적 할당.
- Ginkgo 병렬 워커 간 포트 충돌 방지: OS 수준 포트 0 할당(`net.Listen(:0)`) 후 포트 번호 전달.
- configfs port ID도 TC별로 고유해야 한다 — 전역 카운터 또는 랜덤 할당.
- subsystem 이름(NQN) 충돌은 커널 레벨에서 `mkdir` 실패로 감지됨 — early fail.
- 각 TC는 자체 NVMe-oF target을 생성하고 자체적으로 정리한다 — 공유 target 금지.

### 검증 명령 및 기대 출력

**현재 존재하는 모든 e2e subsystem 목록:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/nvmet/subsystems/ | grep 'pillar-csi'
```

**기대 출력 (TC 완료 후):**
```
(빈 출력 — 모든 e2e subsystem이 정리됨)
```

**현재 할당된 포트 확인:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/nvmet/ports/
```

**기대 출력 (TC 완료 후):**
```
(빈 출력 — 모든 e2e port가 정리됨)
```

**Suite 시작 전 잔여 configfs 항목 확인 (scavenger):**

```bash
docker exec <NODE_CONTAINER> find /sys/kernel/config/nvmet/subsystems/ -maxdepth 1 -name 'nqn.*pillar*' 2>/dev/null | wc -l
```

**기대 출력:**
```
0
```

---

## 5. 사이징 (Sizing)

### 규칙

- NVMe-oF configfs 자체는 메모리 사용량이 미미 — subsystem당 수 KB.
- 병렬 TC 8개(E2E_PROCS=8) 기준, 최대 8개 동시 subsystem + 8개 port.
- 실제 용량 제약은 backend(LVM/ZFS)의 사이징에 의해 결정된다 — [LVM 사이징](LVM.md#5-사이징-sizing) 참조.
- TCP 포트 범위: OS 임시 포트(32768-60999)에서 자동 할당.
- NVMe namespace ID(NSID)는 1부터 시작, TC당 보통 1개.

### 검증 명령 및 기대 출력

**사용 가능한 임시 포트 범위 확인:**

```bash
cat /proc/sys/net/ipv4/ip_local_port_range
```

**기대 출력 패턴:**
```
32768\s+60999
```

**configfs 메모리 사용량 (참고용):**

```bash
docker exec <NODE_CONTAINER> du -sh /sys/kernel/config/nvmet/ 2>/dev/null
```

**기대 출력 패턴:**
```
\d+K\s+/sys/kernel/config/nvmet/
```

---

## 6. 실패 시 정리 (Failure Cleanup)

### 규칙

- TC 실패/panic 시에도 `defer` 패턴으로 NVMe-oF target 정리가 보장되어야 한다.
- initiator disconnect가 가장 먼저 실행 — 연결 상태에서 target 삭제는 커널 panic 유발 가능.
- configfs 삭제 실패 시 "Device or resource busy" 에러는 활성 연결이 원인 — disconnect 재시도.
- suite 종료 시 scavenger가 잔여 configfs 항목(`nqn.*pillar*`)을 검색하여 강제 정리.
- 포트 할당은 `ports.Registry`가 추적하므로, 실패 시에도 포트 해제가 보장됨.
- 부분 실패 상태(subsystem은 있지만 port가 없는 경우 등)도 안전하게 정리 가능해야 한다.

### 검증 명령 및 기대 출력

**Suite 종료 후 잔여 NVMe-oF 리소스 감사:**

```bash
# subsystem 잔여 확인
docker exec <NODE_CONTAINER> ls /sys/kernel/config/nvmet/subsystems/ 2>/dev/null | grep 'pillar'

# port 잔여 확인
docker exec <NODE_CONTAINER> ls /sys/kernel/config/nvmet/ports/ 2>/dev/null

# initiator 연결 잔여 확인
docker exec <NODE_CONTAINER> nvme list-subsys 2>/dev/null | grep 'pillar'
```

**기대 출력 (모두 정리됨):**
```
(각 명령 모두 빈 출력)
```

**busy 상태 진단:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/nvmet/subsystems/<NQN>/namespaces/1/enable
```

**기대 출력 (정리 전 busy 원인 확인):**
```
1
```
> enable=1이면 먼저 `echo 0 > enable` 후 rmdir 가능.

---

## 7. CI 호환성 (CI Compatibility)

### 규칙

- CI 환경: GitHub Actions `ubuntu-latest`.
- `linux-modules-extra-$(uname -r)` 패키지에 4개 NVMe 커널 모듈 모두 포함.
- 모듈 로드 순서: `nvme_fabrics` → `nvmet` → `nvmet_tcp` → `nvme_tcp`.
- `/dev/nvme-fabrics`는 `nvme_fabrics` 모듈 로드 시 자동 생성.
- kind-config.yaml에서 `/sys/kernel/config`와 `/dev/nvme-fabrics`를 바인드 마운트 설정 필요.
- DOCKER_HOST는 환경변수에서만 읽음 — 하드코딩 금지.
- 병렬 Ginkgo 워커는 각자 독립 포트와 NQN으로 동시 실행 가능.

### 검증 명령 및 기대 출력

**GHA runner에서 NVMe 모듈 로드 확인:**

```bash
for mod in nvmet nvmet_tcp nvme_tcp nvme_fabrics; do
  lsmod | grep "^${mod}" && echo "${mod}: OK" || echo "${mod}: MISSING"
done
```

**기대 출력:**
```
nvmet: OK
nvmet_tcp: OK
nvme_tcp: OK
nvme_fabrics: OK
```

**Kind 컨테이너 configfs 바인드 마운트 확인:**

```bash
docker exec <NODE_CONTAINER> mountpoint -q /sys/kernel/config && echo "configfs mounted" || echo "NOT mounted"
```

**기대 출력:**
```
configfs mounted
```

**Kind 컨테이너 nvme-fabrics 디바이스 확인:**

```bash
docker exec <NODE_CONTAINER> stat -c '%F' /dev/nvme-fabrics
```

**기대 출력:**
```
character special file
```

---

## 8. NVMe-oF 연결 라이프사이클 (추가 차원)

### 규칙

- initiator는 `nvme connect` 명령으로 target에 연결한다.
- 연결 파라미터: `-t tcp -a <TARGET_IP> -s <PORT> -n <NQN>`.
- 연결 성공 시 `/dev/nvme<N>n<NSID>` 블록 디바이스가 생성된다.
- disconnect는 `nvme disconnect -n <NQN>` 또는 `nvme disconnect -d /dev/nvme<N>`.
- 블록 디바이스가 마운트된 상태에서 disconnect 시 I/O 에러 발생 — unmount 먼저.
- discovery는 `nvme discover -t tcp -a <TARGET_IP> -s <PORT>`로 수행.

### 검증 명령 및 기대 출력

**NVMe-oF 연결 확인:**

```bash
docker exec <NODE_CONTAINER> nvme list-subsys 2>/dev/null | grep <NQN>
```

**기대 출력 패턴 (연결됨):**
```
.*<NQN>.*tcp.*
```

**블록 디바이스 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls /dev/nvme*n* 2>/dev/null
```

**기대 출력 패턴:**
```
/dev/nvme\d+n\d+
```

**discovery 결과 확인:**

```bash
docker exec <NODE_CONTAINER> nvme discover -t tcp -a <TARGET_IP> -s <PORT> 2>/dev/null
```

**기대 출력 패턴:**
```
Discovery Log Number of Records \d+.*
.*subnqn:\s+<NQN>
.*trtype:\s+tcp
.*trsvcid:\s+<PORT>
```

**disconnect 후 블록 디바이스 부재 확인:**

```bash
docker exec <NODE_CONTAINER> ls /dev/nvme*n* 2>/dev/null | wc -l
```

**기대 출력:**
```
0
```

---

## 차원 요약 매트릭스

| 차원 | 규칙 수 | 검증 명령 수 | 핵심 도구 |
|------|--------|------------|----------|
| 호스트 사전조건 | 8 | 6 | `modprobe`, `nvme version`, `ls configfs` |
| 리소스 생성 | 8 | 7 | configfs `mkdir/echo`, `cat` |
| 리소스 정리 | 6 | 3 | `nvme disconnect`, configfs `rmdir` |
| TC간 격리 | 6 | 3 | `ports.Registry`, NQN uniqueness |
| 사이징 | 5 | 2 | `/proc/sys`, `du` |
| 실패 시 정리 | 6 | 3 | configfs scan, `nvme list-subsys` |
| CI 호환성 | 7 | 3 | `lsmod`, `mountpoint`, `stat` |
| 연결 라이프사이클 | 6 | 4 | `nvme connect/disconnect/discover` |
