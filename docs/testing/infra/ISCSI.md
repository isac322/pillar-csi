# iSCSI configfs 테스트 인프라 전략 (SSOT)

> **수준 B** — 규칙 + 검증 명령 + 기대 출력 패턴
>
> **적용 범위:** pillar-csi E2E/Integration 테스트에서 iSCSI 프로토콜을 사용하는 모든 TC
> (E34, E35, iSCSI configfs integration 등)
>
> **크로스레퍼런스:**
> - [E2E 테스트 케이스](../E2E-TESTS.md) — E34: LVM + iSCSI, E35: ZFS + iSCSI
> - [Integration 테스트](../INTEGRATION-TESTS.md) — iSCSI configfs integration
> - [테스트 전략 README](../README.md) — 테스트 피라미드 및 분류 기준
> - [인프라 전략 인덱스](README.md) — 공통 원칙 및 기술 간 의존 관계
> - [LVM 인프라](LVM.md) — iSCSI는 LVM/ZFS backend와 함께 사용됨
> - [ZFS 인프라](ZFS.md) — E35에서 ZFS + iSCSI 조합
> - 프레임워크 코드: `test/e2e/framework/iscsi/iscsi.go`
> - 포트 할당: `test/e2e/framework/ports/registry.go`

---

## 1. 호스트 사전조건 (Host Prerequisites)

iSCSI 테스트는 두 가지 접근 방식을 사용한다:

1. **LIO configfs (PRD 기준, integration 테스트)**: PRD 4/2.5에 정의된 대로 `/sys/kernel/config/target/iscsi/`를 통한 LIO configfs 기반 iSCSI target 관리. 호스트 커널에 `target_core_mod`, `iscsi_target_mod` 모듈 로딩이 필요하다.

2. **tgtd (Kind E2E 테스트)**: Kind 컨테이너 기반 E2E 테스트에서는 LIO configfs 대신 tgtd를 사용한다. LIO configfs는 호스트 커널 모듈 로딩이 필요하지만, tgtd는 유저스페이스에서 동작하여 Kind 컨테이너 내부에서 독립적으로 실행 가능하기 때문이다.

### 규칙

- **Integration 테스트 (LIO configfs)**:
  - `target_core_mod` 커널 모듈이 호스트에 로드되어 있어야 한다 — LIO target core subsystem.
  - `iscsi_target_mod` 커널 모듈이 호스트에 로드되어 있어야 한다 — iSCSI target fabric module.
  - `/sys/kernel/config/target/iscsi/` configfs 경로가 Kind 노드 컨테이너에 바인드 마운트되어 있어야 한다.
- **Kind E2E 테스트 (tgtd)**:
  - `tgtadm` 바이너리가 Kind 워커 노드 컨테이너 내부에서 사용 가능해야 한다.
  - 호스트 커널 모듈 로딩 불필요 — tgtd는 유저스페이스에서 동작.
- **공통**:
  - `iscsi_tcp` 커널 모듈은 Kind 컨테이너 내부에서 로드 — 호스트에서는 불필요 (prereq_ac10 참조).
  - `iscsiadm` CLI 도구가 initiator 측 Kind 노드에서 사용 가능해야 한다.
  - GHA ubuntu-latest에서는 `linux-modules-extra-$(uname -r)` 패키지 설치 필요.

### 검증 명령 및 기대 출력

**커널 모듈 확인 (호스트):**

```bash
lsmod | grep target_core_mod
lsmod | grep iscsi_target_mod
```

**기대 출력 패턴:**
```
target_core_mod\s+\d+\s+\d+
iscsi_target_mod\s+\d+\s+\d+
```

**iSCSI configfs 경로 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/target/iscsi/
```

**기대 출력:**
```
/sys/kernel/config/target/iscsi/
```

**iscsiadm 도구 확인:**

```bash
docker exec <NODE_CONTAINER> which iscsiadm
```

**기대 출력 패턴:**
```
/usr/bin/iscsiadm
```

**iscsiadm 버전 확인:**

```bash
docker exec <NODE_CONTAINER> iscsiadm --version
```

**기대 출력 패턴:**
```
iscsiadm version \d+\.\d+\.\d+.*
```

**GHA CI 사전 설치:**

```yaml
- name: Install iSCSI kernel modules
  run: |
    sudo apt-get install -y linux-modules-extra-$(uname -r)
    sudo modprobe target_core_mod
    sudo modprobe iscsi_target_mod
    ls -d /sys/kernel/config/target/iscsi/ && echo "iSCSI configfs present"
```

**기대 출력:**
```
iSCSI configfs present
```

---

## 2. 리소스 생성 (iSCSI Target Creation)

### 규칙

- iSCSI target은 configfs(LIO)에 디렉토리/파일을 생성하여 구성한다.
- IQN 형식: `iqn.2026-01.com.bhyoo.pillar-csi:<UNIQUE_SUFFIX>`.
- target 경로: `/sys/kernel/config/target/iscsi/<IQN>/`.
- TPG(Target Portal Group) 생성: `<IQN>/tpgt_1/`.
- LUN 생성: TPG 아래 `lun/lun_0/` 디렉토리.
- backstore 생성: `/sys/kernel/config/target/core/iblock_<N>/<NAME>/` 경로.
- portal 생성: TPG 아래 `np/<IP>:<PORT>/` 디렉토리.
- 포트 번호는 `ports.Registry`에서 동적 할당 (기본 3260이 아닌 OS 할당 포트 사용).
- TPG 활성화: `echo 1 > .../tpgt_1/enable`.
- `demo_mode_write_protect` 설정: `echo 0 > .../tpgt_1/attrib/demo_mode_write_protect`.

### 검증 명령 및 기대 출력

**target 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/target/iscsi/<IQN>
```

**기대 출력:**
```
/sys/kernel/config/target/iscsi/<IQN>
```

**TPG 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/target/iscsi/<IQN>/tpgt_1
```

**기대 출력:**
```
/sys/kernel/config/target/iscsi/<IQN>/tpgt_1
```

**TPG 활성화 확인:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/target/iscsi/<IQN>/tpgt_1/enable
```

**기대 출력:**
```
1
```

**LUN 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/target/iscsi/<IQN>/tpgt_1/lun/lun_0
```

**기대 출력:**
```
/sys/kernel/config/target/iscsi/<IQN>/tpgt_1/lun/lun_0
```

**backstore 확인:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/target/core/iblock_*/ 2>/dev/null
```

**기대 출력 패턴:**
```
.*iblock_\d+/.*
```

**portal 확인:**

```bash
docker exec <NODE_CONTAINER> ls -d /sys/kernel/config/target/iscsi/<IQN>/tpgt_1/np/<IP>:<PORT>
```

**기대 출력:**
```
/sys/kernel/config/target/iscsi/<IQN>/tpgt_1/np/<IP>:<PORT>
```

---

## 3. 리소스 정리 (iSCSI Target Destruction)

### 규칙

- 정리 순서 (역순): initiator logout → LUN symlink 삭제 → portal 삭제 → TPG 삭제 → target 삭제 → backstore 삭제.
- initiator side: `iscsiadm -m node -T <IQN> -u` 먼저 실행 (logout).
- session cleanup: `iscsiadm -m node -T <IQN> -o delete` (node record 삭제).
- configfs 디렉토리는 `rmdir`로만 삭제 가능 — `rm -rf` 사용 불가.
- "No such file or directory" 에러는 멱등성을 위해 무시한다.
- 정리 에러는 수집하여 한꺼번에 반환 — 앞 단계 실패가 뒷 단계를 차단하지 않음.
- backstore 삭제 전 LUN 연결이 먼저 해제되어야 한다.

### 검증 명령 및 기대 출력

**정리 후 target 부재 확인:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/target/iscsi/ | grep <IQN_SUFFIX>
```

**기대 출력:**
```
(빈 출력)
```

**정리 후 backstore 부재 확인:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/target/core/ | grep 'e2e'
```

**기대 출력:**
```
(빈 출력)
```

**initiator session 정리 확인:**

```bash
docker exec <NODE_CONTAINER> iscsiadm -m session 2>&1
```

**기대 출력 패턴:**
```
(빈 출력 또는 "No active sessions")
```

---

## 4. TC간 격리 (Test Case Isolation)

### 규칙

- 각 TC는 고유한 IQN을 사용한다 — `iqn.2026-01.com.bhyoo.pillar-csi:<TC_UNIQUE_SUFFIX>`.
- 각 TC는 고유한 TCP 포트를 사용한다 — `ports.Registry`에서 `KindISCSITarget`으로 동적 할당.
- 포트 할당 방식: probe-and-release (OS가 포트 할당 후 즉시 해제하여 컨테이너가 바인드).
- backstore 이름도 TC별로 고유해야 한다 — `iblock_<IDX>/<TC_NAME>`.
- 각 TC는 자체 iSCSI target을 생성하고 자체적으로 정리한다 — 공유 target 금지.
- initiator IQN(InitiatorName)은 Kind 노드별로 고유 — `/etc/iscsi/initiatorname.iscsi`.

### 검증 명령 및 기대 출력

**현재 존재하는 모든 e2e target 목록:**

```bash
docker exec <NODE_CONTAINER> ls /sys/kernel/config/target/iscsi/ | grep 'pillar-csi'
```

**기대 출력 (TC 완료 후):**
```
(빈 출력 — 모든 e2e target이 정리됨)
```

**활성 iSCSI session 확인:**

```bash
docker exec <NODE_CONTAINER> iscsiadm -m session -P 0 2>&1
```

**기대 출력 (TC 완료 후):**
```
(빈 출력 또는 "No active sessions")
```

**Suite 시작 전 잔여 configfs 항목 확인 (scavenger):**

```bash
docker exec <NODE_CONTAINER> find /sys/kernel/config/target/iscsi/ -maxdepth 1 -name 'iqn.*pillar*' 2>/dev/null | wc -l
```

**기대 출력:**
```
0
```

---

## 5. 사이징 (Sizing)

### 규칙

- iSCSI configfs 자체는 메모리 사용량이 미미 — target당 수 KB.
- 병렬 TC 8개(E2E_PROCS=8) 기준, 최대 8개 동시 target + 8개 portal.
- 실제 용량 제약은 backend(LVM/ZFS)의 사이징에 의해 결정된다.
- TCP 포트 범위: OS 임시 포트(32768-60999)에서 자동 할당.
- backstore 수 제한: `iblock_<N>` 인덱스는 0-255 범위 — E2E 규모에서는 문제 없음.
- initiator 측 session 수 제한: GHA ubuntu-latest에서 기본 session 제한은 충분.

### 검증 명령 및 기대 출력

**사용 가능한 임시 포트 범위 확인:**

```bash
cat /proc/sys/net/ipv4/ip_local_port_range
```

**기대 출력 패턴:**
```
32768\s+60999
```

**현재 활성 iSCSI session 수:**

```bash
docker exec <NODE_CONTAINER> iscsiadm -m session 2>/dev/null | wc -l
```

**기대 출력 패턴 (테스트 중):**
```
[0-8]
```

**configfs 메모리 사용량 (참고용):**

```bash
docker exec <NODE_CONTAINER> du -sh /sys/kernel/config/target/ 2>/dev/null
```

**기대 출력 패턴:**
```
\d+K\s+/sys/kernel/config/target/
```

---

## 6. 실패 시 정리 (Failure Cleanup)

### 규칙

- TC 실패/panic 시에도 `defer` 패턴으로 iSCSI target 정리가 보장되어야 한다.
- initiator logout이 가장 먼저 실행 — 연결 상태에서 target 삭제 시 "Device busy" 에러.
- configfs 삭제 실패 시 "Device or resource busy" 에러는 활성 session이 원인 — logout 재시도.
- suite 종료 시 scavenger가 잔여 configfs 항목(`iqn.*pillar*`)을 검색하여 강제 정리.
- 포트 할당은 `ports.Registry`가 추적하므로, 실패 시에도 포트 해제가 보장됨.
- `iscsiadm -m node -o delete` 명령으로 stale node record 정리.
- backstore가 LUN에 연결된 상태에서는 backstore 삭제 불가 — LUN 연결 먼저 해제.

### 검증 명령 및 기대 출력

**Suite 종료 후 잔여 iSCSI 리소스 감사:**

```bash
# target 잔여 확인
docker exec <NODE_CONTAINER> ls /sys/kernel/config/target/iscsi/ 2>/dev/null | grep 'pillar'

# backstore 잔여 확인
docker exec <NODE_CONTAINER> ls /sys/kernel/config/target/core/ 2>/dev/null | grep 'e2e'

# initiator session 잔여 확인
docker exec <NODE_CONTAINER> iscsiadm -m session 2>/dev/null | grep 'pillar'

# node record 잔여 확인
docker exec <NODE_CONTAINER> iscsiadm -m node 2>/dev/null | grep 'pillar'
```

**기대 출력 (모두 정리됨):**
```
(각 명령 모두 빈 출력)
```

---

## 7. CI 호환성 (CI Compatibility)

### 규칙

- CI 환경: GitHub Actions `ubuntu-latest`.
- `linux-modules-extra-$(uname -r)` 패키지에 `target_core_mod`, `iscsi_target_mod` 모듈 포함.
- `iscsi_tcp` 모듈은 Kind 컨테이너 내부에서 로드 — 호스트 레벨에서는 불필요 (prereq_ac10 검증).
- kind-config.yaml에서 `/sys/kernel/config` 바인드 마운트 설정 필요.
- `open-iscsi` 패키지가 Kind 노드 이미지에 포함되어야 한다 (`iscsiadm` 제공).
- DOCKER_HOST는 환경변수에서만 읽음 — 하드코딩 금지.
- 병렬 Ginkgo 워커는 각자 독립 포트와 IQN으로 동시 실행 가능.

### 검증 명령 및 기대 출력

**GHA runner에서 iSCSI target 모듈 확인:**

```bash
for mod in target_core_mod iscsi_target_mod; do
  lsmod | grep "^${mod}" && echo "${mod}: OK" || echo "${mod}: MISSING"
done
```

**기대 출력:**
```
target_core_mod: OK
iscsi_target_mod: OK
```

**Kind 컨테이너 configfs 바인드 마운트 확인:**

```bash
docker exec <NODE_CONTAINER> mountpoint -q /sys/kernel/config && echo "configfs mounted" || echo "NOT mounted"
```

**기대 출력:**
```
configfs mounted
```

**iSCSI initiator 서비스 확인 (Kind 내부):**

```bash
docker exec <NODE_CONTAINER> cat /etc/iscsi/initiatorname.iscsi
```

**기대 출력 패턴:**
```
InitiatorName=iqn\.\d{4}-\d{2}\..*
```

---

## 8. iSCSI 인증 및 접근 제어 (추가 차원)

### 규칙

- E2E 테스트에서는 `demo_mode_write_protect=0` (인증 없이 접근 허용).
- `generate_node_acls=1` 설정으로 모든 initiator IQN 자동 허용.
- CHAP 인증 테스트는 별도 TC에서 수행 — 기본 E2E는 no-auth.
- ACL 미설정 시 LIO가 `demo mode`로 동작 — 모든 initiator 접속 허용.
- 프로덕션 환경에서는 반드시 ACL + CHAP 설정 필요 (E2E 스코프 밖).

### 검증 명령 및 기대 출력

**demo mode 설정 확인:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/target/iscsi/<IQN>/tpgt_1/attrib/demo_mode_write_protect
```

**기대 출력:**
```
0
```

**generate_node_acls 설정 확인:**

```bash
docker exec <NODE_CONTAINER> cat /sys/kernel/config/target/iscsi/<IQN>/tpgt_1/attrib/generate_node_acls
```

**기대 출력:**
```
1
```

**로그인 후 LUN 접근 확인:**

```bash
docker exec <NODE_CONTAINER> iscsiadm -m session -P 3 2>/dev/null | grep 'Attached scsi disk'
```

**기대 출력 패턴:**
```
.*Attached scsi disk sd[a-z]+.*
```

---

## 차원 요약 매트릭스

| 차원 | 규칙 수 | 검증 명령 수 | 핵심 도구 |
|------|--------|------------|----------|
| 호스트 사전조건 | 6 | 6 | `modprobe`, `iscsiadm`, `ls configfs` |
| 리소스 생성 | 10 | 7 | configfs `mkdir/echo`, `cat` |
| 리소스 정리 | 7 | 3 | `iscsiadm logout`, configfs `rmdir` |
| TC간 격리 | 6 | 3 | `ports.Registry`, IQN uniqueness |
| 사이징 | 6 | 3 | `/proc/sys`, `iscsiadm -m session`, `du` |
| 실패 시 정리 | 7 | 4 | configfs scan, `iscsiadm -m session/node` |
| CI 호환성 | 7 | 3 | `lsmod`, `mountpoint`, `initiatorname.iscsi` |
| 인증/접근 제어 | 5 | 3 | configfs `attrib`, `iscsiadm -P 3` |
