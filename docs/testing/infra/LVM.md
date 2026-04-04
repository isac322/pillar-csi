# LVM 테스트 인프라 전략 (SSOT)

> **수준 B** — 규칙 + 검증 명령 + 기대 출력 패턴
>
> **적용 범위:** pillar-csi E2E/Integration 테스트에서 LVM VG를 사용하는 모든 TC
> (E28, E33, E34, LVM backend integration 등)
>
> **크로스레퍼런스:**
> - [E2E 테스트 케이스](../E2E-TESTS.md) — E33: LVM + NVMe-oF, E34: LVM + iSCSI
> - [Integration 테스트](../INTEGRATION-TESTS.md) — E28: 실제 LVM backend (loopback VG)
> - [테스트 전략 README](../README.md) — 테스트 피라미드 및 분류 기준
> - 프레임워크 코드: `test/e2e/framework/lvm/lvm.go`, `lvm_kubectl.go`

---

## 1. 호스트 사전조건 (Host Prerequisites)

### 규칙

- `dm_mod` 커널 모듈이 로드되어 있어야 한다 — `/dev/mapper/control` 디바이스 존재 필수.
- `dm_thin_pool` 커널 모듈이 로드되어 있어야 한다 — thin provisioning TC에 필요.
- `pvcreate`, `vgcreate`, `lvcreate`, `lvs`, `vgs`, `lvremove`, `lvextend`, `vgremove`, `pvremove` CLI 도구가 Kind 노드 컨테이너 내부에서 사용 가능해야 한다.
- `losetup`, `truncate`/`dd` 도구가 Kind 노드 컨테이너 내부에서 사용 가능해야 한다.
- GHA ubuntu-latest에서는 `lvm2`, `thin-provisioning-tools` 패키지 설치 필요.
- `thin_check` 바이너리 존재 필요 (thin pool 메타데이터 검증).

### 검증 명령 및 기대 출력

**커널 모듈 확인:**

```bash
lsmod | grep dm_mod
lsmod | grep dm_thin_pool
```

**기대 출력 패턴:**
```
dm_mod\s+\d+\s+\d+
dm_thin_pool\s+\d+\s+\d+
```

**device-mapper 제어 디바이스 확인:**

```bash
test -e /dev/mapper/control && echo "OK" || echo "MISSING"
```

**기대 출력:**
```
OK
```

**LVM CLI 도구 확인:**

```bash
docker exec <NODE_CONTAINER> which pvcreate vgcreate lvcreate lvs vgs
```

**기대 출력 패턴:**
```
/usr/sbin/pvcreate
/usr/sbin/vgcreate
/usr/sbin/lvcreate
/usr/sbin/lvs
/usr/sbin/vgs
```

**thin_check 도구 확인:**

```bash
docker exec <NODE_CONTAINER> which thin_check
```

**기대 출력 패턴:**
```
/usr/sbin/thin_check
```

**GHA CI 사전 설치:**

```yaml
- name: Install LVM and device-mapper modules
  run: |
    sudo apt-get update -qq
    sudo apt-get install -y lvm2 thin-provisioning-tools
    sudo modprobe dm_mod
    sudo modprobe dm_thin_pool
    test -e /dev/mapper/control && echo "LVM: /dev/mapper/control present"
```

**기대 출력 패턴:**
```
LVM: /dev/mapper/control present
```

---

## 2. 리소스 생성 (VG Creation)

### 규칙

- 테스트용 VG는 루프백 디바이스 위에 생성한다 — 호스트 디스크를 직접 사용하지 않는다.
- 이미지 파일은 `/tmp/lvm-vg-<VGName>.img` 경로에 sparse 파일(truncate)로 생성한다.
- 기본 크기는 512 MiB이며, TC에서 별도 지정하지 않으면 이 기본값을 사용한다.
- VG 이름은 테스트별로 고유해야 하며, `e2e-vg-<RANDOM_SUFFIX>` 패턴을 따른다.
- 생성 순서: `truncate` → `losetup --find --show` → `pvcreate --yes --force` → `vgcreate`.
- `CreateVG` 또는 `CreateVGViaKubectl` 함수를 통해서만 생성한다 — 직접 쉘 명령 호출 금지.
- `/dev/loop*` 노드 부재 시 자동으로 `mknod`로 생성하는 스크립트가 내장되어 있다.

### 검증 명령 및 기대 출력

**이미지 파일 생성 확인:**

```bash
docker exec <NODE_CONTAINER> ls -la /tmp/lvm-vg-<VG_NAME>.img
```

**기대 출력 패턴:**
```
-rw-r--r--.*\d+.*\/tmp\/lvm-vg-<VG_NAME>\.img
```

**루프 디바이스 연결 확인:**

```bash
docker exec <NODE_CONTAINER> losetup -a | grep <VG_NAME>
```

**기대 출력 패턴:**
```
/dev/loop\d+:.*\/tmp\/lvm-vg-<VG_NAME>\.img
```

**PV 생성 확인:**

```bash
docker exec <NODE_CONTAINER> pvs --noheadings -o pv_name,vg_name | grep <VG_NAME>
```

**기대 출력 패턴:**
```
\s*/dev/loop\d+\s+<VG_NAME>
```

**VG 생성 확인:**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings -o vg_name,vg_size <VG_NAME>
```

**기대 출력 패턴:**
```
\s*<VG_NAME>\s+\d+(\.\d+)?[mgtp]
```

**VG 속성 확인 (정상 상태):**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings -o vg_attr <VG_NAME>
```

**기대 출력 패턴:**
```
\s*wz--n-
```
> `w`=writable, `z`=resizeable, `--`=not exported/partial, `n`=normal alloc, `-`=not clustered

---

## 3. 리소스 정리 (VG Destruction)

### 규칙

- 정리는 반드시 4단계로 수행: `vgremove -f` → `pvremove -f -f` → `losetup -d` → `rm -f`.
- 모든 단계는 항상 실행 — 앞 단계 실패가 뒷 단계를 차단하지 않는다.
- "Volume group not found" 에러는 멱등성을 위해 무시한다.
- "not a PV" / "no physical volume label" 에러는 멱등성을 위해 무시한다.
- "no such device" 에러(루프 디바이스)는 멱등성을 위해 무시한다.
- nil VG에 대한 Destroy 호출은 안전한 no-op이다.
- `VG.Destroy()` 또는 `DestroyVGViaKubectl()` 함수를 통해서만 정리 — 직접 명령 금지.
- LV가 남아 있더라도 `vgremove -f`가 강제로 LV를 삭제한 후 VG를 삭제한다.

### 검증 명령 및 기대 출력

**정리 후 VG 부재 확인:**

```bash
docker exec <NODE_CONTAINER> vgs <VG_NAME> 2>&1
```

**기대 출력 패턴:**
```
Volume group "<VG_NAME>" not found
```

**정리 후 PV 부재 확인:**

```bash
docker exec <NODE_CONTAINER> pvs --noheadings -o pv_name,vg_name 2>/dev/null | grep <VG_NAME>
```

**기대 출력:**
```
(빈 출력)
```

**정리 후 루프 디바이스 부재 확인:**

```bash
docker exec <NODE_CONTAINER> losetup -a | grep <VG_NAME>
```

**기대 출력:**
```
(빈 출력 — 해당 VG 이름이 포함된 루프 디바이스가 없음)
```

**정리 후 이미지 파일 부재 확인:**

```bash
docker exec <NODE_CONTAINER> ls /tmp/lvm-vg-<VG_NAME>.img 2>&1
```

**기대 출력 패턴:**
```
.*No such file or directory
```

---

## 4. TC간 격리 (Test Case Isolation)

### 규칙

- 각 TC는 고유한 VG 이름을 사용한다 — `e2e-vg-<RANDOM_SUFFIX>` 형식.
- TC 간 LVM VG 공유 금지 — 각 TC는 자체 VG를 생성하고 자체적으로 정리한다.
- 병렬 실행 시 VG 이름 충돌 방지를 위해 `names.Registry`에서 고유 이름을 발급받는다.
- LV(Logical Volume)는 해당 VG 내부에 생성되므로, VG 단위 격리가 곧 완전 격리이다.
- 루프 디바이스 번호는 커널의 `LOOP_CTL_GET_FREE` ioctl로 원자적 할당 — 병렬 TC 간 충돌 없음.
- thin pool도 VG 내부에 생성되므로 VG 격리에 포함된다.

### 검증 명령 및 기대 출력

**현재 존재하는 모든 e2e VG 목록:**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings -o vg_name | grep 'e2e-vg-'
```

**기대 출력 (TC 완료 후):**
```
(빈 출력 — 모든 e2e VG가 정리됨)
```

**Suite 시작 전 잔여 VG 확인 (scavenger):**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings -o vg_name 2>/dev/null | grep 'e2e-vg-' | wc -l
```

**기대 출력:**
```
0
```

**특정 TC의 LV 격리 확인 (실행 중):**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings -o lv_name,vg_name | grep <VG_NAME>
```

**기대 출력 패턴 (해당 VG의 LV만 표시):**
```
\s*lv-.*\s+<VG_NAME>
```

---

## 5. 사이징 (Sizing)

### 규칙

- 기본 VG 크기: 512 MiB (sparse 파일 — 실제 디스크 사용량은 데이터 기록 시까지 최소).
- GHA ubuntu-latest 러너의 `/tmp` 파티션: 최소 14 GB 여유 공간.
- 병렬 TC 8개(E2E_PROCS=8) 기준, 최대 8 × 512 MiB = 4 GiB sparse 파일.
- thin pool 사용 시 풀 오버프로비저닝 가능 — 실제 사용량만큼만 공간 소비.
- LV 크기는 VG 크기의 50%를 초과하지 않도록 설정 (여유 공간 유지).
- 용량 고갈 테스트(E-FAULT-3)는 별도의 소형 VG(64 MiB)를 사용한다.

### 검증 명령 및 기대 출력

**호스트 /tmp 여유 공간 확인:**

```bash
df -BM /tmp | tail -1 | awk '{print $4}'
```

**기대 출력 패턴 (최소 4 GiB 여유):**
```
\d{4,}M
```

**VG 크기 및 여유 공간 확인:**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings --units m -o vg_name,vg_size,vg_free <VG_NAME>
```

**기대 출력 패턴:**
```
\s*<VG_NAME>\s+5\d{2}\.\d+m\s+\d+\.\d+m
```

**thin pool 사용률 확인 (thin provisioning TC):**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings -o lv_name,data_percent <VG_NAME>
```

**기대 출력 패턴:**
```
\s*thinpool.*\d+\.\d+
```

---

## 6. 실패 시 정리 (Failure Cleanup)

### 규칙

- `CreateVG` 실패 시 이미 생성된 리소스(이미지 파일, 루프 디바이스, PV)를 best-effort로 정리한다.
- 정리 순서: PV label 제거 → 루프 디바이스 해제 → 이미지 파일 삭제.
- 테스트 실패/panic 시에도 `defer vg.Destroy(ctx)` 패턴으로 정리가 보장되어야 한다.
- `registry.Resource` 인터페이스를 통해 프레임워크가 미정리 리소스를 추적하고 suite 종료 시 강제 정리.
- 루프 디바이스 이미지는 `/tmp` 아래에 위치하므로, 컨테이너 재시작 시 자동 정리.
- 정리 에러는 `errors.Join`으로 수집하여 한꺼번에 반환 — 앞 단계 실패가 뒷 단계를 차단하지 않음.
- 활성 LV가 있는 VG의 `vgremove -f`는 먼저 LV를 강제 비활성화/삭제한 후 VG를 삭제한다.

### 검증 명령 및 기대 출력

**Suite 종료 후 잔여 리소스 감사:**

```bash
# VG 잔여 확인
docker exec <NODE_CONTAINER> vgs --noheadings -o vg_name 2>/dev/null | grep 'e2e-vg-'

# PV 잔여 확인
docker exec <NODE_CONTAINER> pvs --noheadings -o pv_name,vg_name 2>/dev/null | grep 'e2e-'

# 루프 디바이스 잔여 확인
docker exec <NODE_CONTAINER> losetup -a | grep 'lvm-vg-e2e'

# 이미지 파일 잔여 확인
docker exec <NODE_CONTAINER> ls /tmp/lvm-vg-e2e-*.img 2>/dev/null
```

**기대 출력 (모두 정리됨):**
```
(각 명령 모두 빈 출력)
```

**멱등 정리 확인 (이중 Destroy):**

```bash
docker exec <NODE_CONTAINER> vgremove -f <VG_NAME> 2>&1 || true
```

**기대 출력 패턴:**
```
(빈 출력 또는 "Volume group.*not found")
```

---

## 7. CI 호환성 (CI Compatibility)

### 규칙

- CI 환경: GitHub Actions `ubuntu-latest`.
- `dm_mod`, `dm_thin_pool` 커널 모듈은 `modprobe`로 로드 필요.
- `lvm2`, `thin-provisioning-tools` 패키지 설치 필요.
- Kind 노드 컨테이너의 `/dev/mapper` 바인드 마운트 필요 (kind-config.yaml의 `extraMounts`).
- DOCKER_HOST는 환경변수에서만 읽음 — 하드코딩 금지.
- LVM VG 생성/파괴 작업은 VG 이름 기반 격리로 병렬 실행 가능.
- CI에서 `E2E_PROCS=8`로 설정하여 8개 병렬 워커가 각자 독립 VG를 운영.

### 검증 명령 및 기대 출력

**GHA runner에서 dm_mod 확인:**

```bash
lsmod | grep '^dm_mod'
```

**기대 출력 패턴:**
```
dm_mod\s+\d+\s+\d+
```

**Kind 컨테이너 /dev/mapper 마운트 확인:**

```bash
docker exec <NODE_CONTAINER> ls /dev/mapper/control
```

**기대 출력:**
```
/dev/mapper/control
```

**Kind 컨테이너 특권 모드 확인:**

```bash
docker inspect <NODE_CONTAINER> --format '{{.HostConfig.Privileged}}'
```

**기대 출력:**
```
true
```

**LVM 도구 버전 확인 (CI 디버깅용):**

```bash
docker exec <NODE_CONTAINER> lvm version 2>&1 | head -1
```

**기대 출력 패턴:**
```
LVM version:\s+\d+\.\d+\.\d+.*
```

**kubectl exec 모드 대체 확인 (docker socket 부재 시):**

```bash
kubectl exec -n kube-system <POD_NAME> -- vgs --version
```

**기대 출력 패턴:**
```
\s*LVM version:\s+\d+\.\d+\.\d+.*
```

---

## 8. Thin Provisioning (추가 차원)

### 규칙

- thin pool은 VG 내부에 `lvcreate --type thin-pool`으로 생성한다.
- thin pool 기본 크기는 128 MiB (`CreateThinPool` 함수의 기본값).
- thin LV는 thin pool 위에 `lvcreate -V <size> --thin-pool <pool> -n <name> <vg>`으로 생성 — 오버프로비저닝 가능.
- thin pool의 `lv_attr[0]`은 `t`(thin pool)이어야 한다.
- thin LV의 `lv_attr[0]`은 `V`(virtual/thin)이어야 한다.
- thin pool 메타데이터 크기는 자동 산정되지만, 소형 테스트 풀에서는 최소 4 MiB.
- `thin_check`가 실패하면 thin pool이 read-only로 전환 — 데이터 무결성 보호.

### 검증 명령 및 기대 출력

**thin pool 생성 확인:**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings -o lv_attr <VG_NAME>/<THIN_POOL>
```

**기대 출력 패턴:**
```
\s*t.*
```
> 첫 문자 `t`가 thin pool 타입을 나타냄.

**thin LV 생성 확인:**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings -o lv_name,pool_lv <VG_NAME> | grep <THIN_POOL>
```

**기대 출력 패턴:**
```
\s*<THIN_LV>\s+<THIN_POOL>
```

**thin pool 사용률:**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings -o data_percent,metadata_percent <VG_NAME>/<THIN_POOL>
```

**기대 출력 패턴:**
```
\s*\d+\.\d+\s+\d+\.\d+
```

**thin pool 메타데이터 무결성:**

```bash
docker exec <NODE_CONTAINER> thin_check /dev/mapper/<VG_NAME>-<THIN_POOL>_tmeta 2>&1
echo $?
```

**기대 출력:**
```
0
```

---

## 9. 볼륨 확장 (Volume Expansion)

### 규칙

- LV 확장은 `lvextend -L +<SIZE>` 또는 `lvextend -L <NEW_SIZE>`로 수행.
- 온라인 확장 지원: 파일시스템이 마운트된 상태에서도 LV 확장 가능.
- 확장 후 파일시스템 리사이즈 필요: `resize2fs`(ext4) 또는 `xfs_growfs`(xfs).
- 확장 크기는 VG 여유 공간을 초과할 수 없다.
- thin LV 확장은 thin pool 여유 용량과 무관하게 virtual size만 증가.

### 검증 명령 및 기대 출력

**LV 확장 후 크기 확인:**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings --units m -o lv_name,lv_size <VG_NAME>/<LV_NAME>
```

**기대 출력 패턴:**
```
\s*<LV_NAME>\s+<EXPECTED_SIZE_MIB>\.00m
```

**VG 여유 공간 확인 (확장 전 사전조건):**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings --units m -o vg_free <VG_NAME>
```

**기대 출력 패턴 (충분한 여유):**
```
\s+\d+\.\d+m
```

**확장 이력 확인:**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings -o lv_name,lv_size,seg_count <VG_NAME>/<LV_NAME>
```

**기대 출력 패턴:**
```
\s*<LV_NAME>\s+\d+(\.\d+)?m\s+\d+
```

---

## 10. 상태 조회 (State Inspection)

### 규칙

- VG 상태 조회는 `vgs --noheadings -o vg_attr` 명령으로 수행한다.
- VG 속성 문자열은 6자리: `[rw][z-][x-][p-][cnlai][c-]`.
  - `[0]` permissions: `w`=writable, `r`=read-only
  - `[1]` resizeable: `z`=resizeable, `-`=not
  - `[2]` exported: `x`=exported, `-`=not
  - `[3]` partial: `p`=one or more PVs missing, `-`=all present
  - `[4]` allocation: `c`,`l`,`n`,`a`,`i`
  - `[5]` cluster: `c`=clustered, `-`=not
- 정상 상태: `wz--n-`.
- `VerifyActive()` 함수가 VG 생성 후 즉시 호출되어 VG 건전성을 확인한다.
- attr[0] ≠ 'w' → read-only, 사용 불가.
- attr[2] = 'x' → exported, 이 호스트에서 사용 불가.
- attr[3] = 'p' → partial (PV 누락), LV 생성 신뢰 불가.

### 검증 명령 및 기대 출력

**VG 건전성 확인:**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings -o vg_attr <VG_NAME>
```

**기대 출력:**
```
  wz--n-
```

**VG 상세 정보:**

```bash
docker exec <NODE_CONTAINER> vgdisplay <VG_NAME>
```

**기대 출력 패턴:**
```
\s*--- Volume group ---
\s*VG Name\s+<VG_NAME>
\s*.*
\s*VG Access\s+read/write
\s*VG Status\s+resizable
```

**PV 상태 확인:**

```bash
docker exec <NODE_CONTAINER> pvs --noheadings -o pv_name,pv_attr /dev/loop<N>
```

**기대 출력 패턴:**
```
\s*/dev/loop\d+\s+a--
```
> `a`=allocatable, `--`=no additional flags

**VG 용량 사용률:**

```bash
docker exec <NODE_CONTAINER> vgs --noheadings --units m -o vg_size,vg_free <VG_NAME>
```

**기대 출력 패턴:**
```
\s*\d+(\.\d+)?m\s+\d+(\.\d+)?m
```

**모든 LV 목록:**

```bash
docker exec <NODE_CONTAINER> lvs --noheadings -o lv_name,lv_size,lv_attr <VG_NAME>
```

**기대 출력 패턴:**
```
(\s*\S+\s+\d+(\.\d+)?[mgtp]\s+\S+\n)*
```

---

## 차원 요약 매트릭스

| 차원 | 규칙 수 | 검증 명령 수 | 핵심 도구 |
|------|--------|------------|----------|
| 호스트 사전조건 | 6 | 6 | `modprobe`, `which`, `apt-get`, `test -e` |
| 리소스 생성 | 7 | 5 | `pvcreate`, `vgcreate`, `losetup`, `truncate` |
| 리소스 정리 | 8 | 4 | `vgremove`, `pvremove`, `losetup -d`, `rm -f` |
| TC간 격리 | 6 | 3 | `vgs`, `lvs`, `names.Registry` |
| 사이징 | 6 | 3 | `df`, `vgs`, `lvs` |
| 실패 시 정리 | 7 | 4 | `vgs`, `pvs`, `losetup -a`, `ls /tmp` |
| CI 호환성 | 7 | 5 | `lsmod`, `docker inspect`, `lvm version`, `kubectl` |
| Thin Provisioning | 7 | 4 | `lvcreate --thin`, `thin_check`, `lvs` |
| 볼륨 확장 | 5 | 3 | `lvextend`, `lvs`, `vgs` |
| 상태 조회 | 6 | 5 | `vgs`, `vgdisplay`, `pvs`, `lvs` |
