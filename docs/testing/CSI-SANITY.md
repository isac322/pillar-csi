# CSI Sanity Tests — CSI 스펙 준수 계약 테스트

`kubernetes-csi/csi-test` 스위트를 사용하여 pillar-csi의 CSI gRPC 스펙 준수를
자동으로 검증한다. **실제 backend와 함께 실행**하여 기술 고유 에러도 함께 잡는다.

**분류:** Contract test (계약 테스트) — 실제 backend 사용 시 Integration 레벨
**패키지:** `github.com/kubernetes-csi/csi-test/v5/pkg/sanity`
**실행:** `go test ./test/sanity/ -v`

---

## 1. CSI Sanity란 무엇인가

[kubernetes-csi/csi-test](https://github.com/kubernetes-csi/csi-test)는 CSI
커뮤니티가 관리하는 공식 스펙 준수 테스트 스위트이다. CSI gRPC 스펙(v1.x)에 정의된
모든 RPC의 **요청/응답 계약**을 자동으로 검증한다.

동작 방식:
1. 테스트 대상 CSI 드라이버가 Unix domain socket에서 gRPC 서비스를 노출한다.
2. `sanity.Test(t, config)` 가 약 70개의 Ginkgo 테스트를 실행한다.
3. 각 테스트는 CSI gRPC 클라이언트로 RPC를 호출하고, 스펙이 요구하는 응답
   코드/필드/멱등성을 검증한다.

이 스위트는 **드라이버 구현의 정확성이 아닌 스펙 준수 여부**를 검증한다.
내부 비즈니스 로직(PillarTarget 조회, agent 위임 등)은 검증 대상이 아니다.

---

## 2. 분류 — 계약 테스트, Integration 레벨

| 속성 | 값 |
|------|-----|
| 테스트 유형 | Contract test (스펙 준수 계약 검증) |
| 실행 레벨 | Integration — 실제 backend(ZFS zpool, LVM VG)와 함께 실행 |
| 외부 의존성 | 실제 스토리지 backend (loopback device), 실제 gRPC |
| K8s 의존성 | 없음 — K8s API/envtest/Kind 불필요 |
| 실행 시간 | ~1-2분 (backend별) |
| CI 실행 | 가능 (loopback device 사용 가능한 환경) |

**왜 mock이 아닌 실제 backend인가:**

CSI Sanity의 기본 사용 패턴은 mock backend로 스펙 준수만 확인하는 것이다.
그러나 pillar-csi의 목표는 **새 backend/protocol 추가 시 기술 고유 에러를
조기에 발견**하는 것이다.

- Mock으로 실행하면 `CreateVolume` → `OK` 가 항상 성공하여, ZFS `zfs create`
  실패나 LVM `lvcreate` 권한 에러를 잡을 수 없다.
- 실제 backend로 실행하면 ~70개 테스트가 실제 create/delete/expand 경로를
  거치면서, 스펙 준수와 backend 호환성을 **동시에** 검증한다.

비용: loopback device 생성이 필요하지만, E2E 테스트의 Kind 클러스터 비용보다
훨씬 가볍다.

---

## 3. 커버리지 — ~70개 테스트

### 3.1 Identity 서비스 (3개)

| 테스트 | 검증 내용 |
|--------|----------|
| GetPluginInfo | 드라이버 이름과 버전이 비어 있지 않은 문자열 |
| GetPluginCapabilities | CONTROLLER_SERVICE 역량 포함, 응답 형식 유효 |
| Probe | Ready=true 반환, 응답 형식 유효 |

> pillar-csi는 현재 Identity 서비스에 대한 별도 테스트가
> `test/component/csi_identity_test.go`에만 있으며, CSI Sanity가 추가하는
> 스펙 준수 검증은 새로운 커버리지이다.

### 3.2 Controller 서비스 (~45개)

| 카테고리 | 테스트 수 | 검증 내용 |
|----------|----------|----------|
| CreateVolume 정상 | ~5 | 이름/용량/capabilities 지정 시 성공, VolumeId 비어 있지 않음 |
| CreateVolume 에러 | ~8 | 이름 없음 → InvalidArgument, capabilities 없음 → InvalidArgument, 동일 이름 다른 용량 → AlreadyExists |
| CreateVolume 멱등성 | ~3 | 동일 파라미터 재호출 → 동일 VolumeId 반환 |
| DeleteVolume 정상 | ~3 | 성공, 존재하지 않는 ID → 성공 (멱등성) |
| DeleteVolume 에러 | ~3 | VolumeId 없음 → InvalidArgument |
| ControllerPublishVolume | ~8 | 정상 게시, VolumeId/NodeId/Capability 없음 → InvalidArgument |
| ControllerUnpublishVolume | ~5 | 정상 해제, VolumeId 없음 → InvalidArgument, 미게시 볼륨 → 성공 (멱등성) |
| ControllerExpandVolume | ~5 | 정상 확장, VolumeId/Capability 없음 → InvalidArgument |
| ListVolumes / GetCapacity | ~5 | 역량 선언에 따라 구현 여부 검증 |

### 3.3 Node 서비스 (~20개)

| 카테고리 | 테스트 수 | 검증 내용 |
|----------|----------|----------|
| NodeStageVolume | ~5 | 정상 스테이징, VolumeId/StagingPath/Capability 없음 → InvalidArgument |
| NodeUnstageVolume | ~3 | 정상 언스테이징, VolumeId/StagingPath 없음 → InvalidArgument |
| NodePublishVolume | ~5 | 정상 게시, VolumeId/TargetPath/Capability 없음 → InvalidArgument |
| NodeUnpublishVolume | ~3 | 정상 해제, VolumeId/TargetPath 없음 → InvalidArgument |
| NodeGetVolumeStats | ~2 | 게시된 볼륨에 대한 통계 반환 |
| NodeExpandVolume | ~2 | 파일시스템 리사이즈 성공, VolumeId 없음 → InvalidArgument |

### 3.4 전체 라이프사이클 (~5개)

| 테스트 | 검증 내용 |
|--------|----------|
| Full lifecycle chain | CreateVolume → ControllerPublish → NodeStage → NodePublish → VolumeStats → NodeUnpublish → NodeUnstage → ControllerUnpublish → DeleteVolume |
| Lifecycle idempotency | 각 단계를 두 번 호출해도 오류 없음 |
| Lifecycle with expand | 라이프사이클 중간에 ControllerExpand + NodeExpand |

이 라이프사이클 테스트는 Component 레벨의 E4/E24와 유사하지만,
**실제 backend**에서 실행된다는 점이 다르다.

---

## 4. 커버리지 제외 영역

CSI Sanity는 다음을 검증하지 **않는다**:

| 제외 영역 | 이유 | 대안 |
|-----------|------|------|
| K8s 통합 (PVC/PV/StorageClass) | CSI Sanity는 순수 gRPC 레벨 | E2E 테스트 (E33/E34/E35) |
| 데이터 I/O (읽기/쓰기 검증) | gRPC 응답만 검증 | E2E 테스트 (E33 data-persistence) |
| 장애 주입 (agent 다운, backend 에러) | Sanity는 happy path + 입력 검증만 | Component 테스트 (E18, E24) |
| 동시 작업 안전성 | 순차 실행만 | Component 테스트 (E16) |
| 토폴로지/노드 친화성 | 단일 노드 가정 | E2E 테스트 (E2.5) |
| Webhook/CRD 스키마 검증 | K8s API 불필요 | Integration 테스트 (E21.2-E21.4) |
| mTLS 핸드셰이크 | 테스트 환경에서 인증서 불필요 | Component 테스트 (E8) |
| Snapshot/Clone | pillar-csi가 현재 미구현 | Unit 테스트 (E12/E13) |

---

## 5. 통합 방식 — `sanity.Test(t, config)` 패턴

```go
// test/sanity/sanity_test.go
package sanity_test

import (
    "testing"

    "github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
)

func TestCSISanity_ZFS(t *testing.T) {
    // 사전 조건: ZFS zpool "sanity-tank" on loopback device
    config := sanity.NewTestConfig()
    config.Address = csiEndpoint       // unix:///tmp/csi-sanity-zfs.sock
    config.TargetPath = t.TempDir()    // NodePublish 대상 경로
    config.StagingPath = t.TempDir()   // NodeStage 대상 경로

    // pillar-csi 고유: StorageClass 파라미터
    config.TestVolumeParameters = map[string]string{
        "target":        "sanity-target",
        "backend-type":  "zfs-zvol",
        "protocol-type": "nvmeof-tcp",
        "pool":          "sanity-tank",
    }

    // pillar-csi 고유: ControllerPublish에 필요한 NodeID
    config.TestNodeID = "sanity-node-01"

    sanity.Test(t, config)
}
```

핵심 포인트:
- **`sanity.Test(t, config)`** 한 줄이 ~70개 Ginkgo 테스트를 실행한다.
- `config.TestVolumeParameters`로 pillar-csi의 StorageClass 파라미터를 주입한다.
- **테스트 대상 프로세스 구성:** CSI Controller + Node 서비스를 하나의 gRPC 서버에
  등록하고 Unix domain socket으로 노출한다. Agent는 실제 backend에 연결된
  상태로 같은 프로세스에서 실행한다.

---

## 6. Backend 구성 — backend별 1회 실행

새 backend/protocol을 추가할 때마다 sanity 테스트를 추가하여, 해당 기술의
CSI 스펙 준수를 검증한다.

### 6.1 실행 매트릭스

| Backend | Protocol | Loopback 장치 | zpool/VG | 테스트 함수 |
|---------|----------|-------------|----------|------------|
| ZFS zvol | NVMe-oF TCP | `/tmp/sanity-zfs.img` (256MB) | `sanity-tank` | `TestCSISanity_ZFS_NVMeoF` |
| ZFS zvol | iSCSI | `/tmp/sanity-zfs.img` (256MB) | `sanity-tank` | `TestCSISanity_ZFS_iSCSI` |
| LVM | NVMe-oF TCP | `/tmp/sanity-lvm.img` (256MB) | `sanity-vg` | `TestCSISanity_LVM_NVMeoF` |
| LVM | iSCSI | `/tmp/sanity-lvm.img` (256MB) | `sanity-vg` | `TestCSISanity_LVM_iSCSI` |

### 6.2 TestMain 환경 셋업

```go
func TestMain(m *testing.M) {
    // 1. loopback device 생성 (256MB sparse file)
    // 2. zpool create / vgcreate on loopback
    // 3. Agent 프로세스 시작 (실제 backend plugin 등록)
    // 4. CSI gRPC 서버 시작 (IdentityServer + ControllerServer + NodeServer)
    // 5. 테스트 실행
    // 6. 정리: CSI 서버 종료 → Agent 종료 → zpool destroy / vgremove → loopback 해제
    code := m.Run()
    cleanup()
    os.Exit(code)
}
```

### 6.3 새 backend 추가 시

1. `test/sanity/` 에 `TestCSISanity_<Backend>_<Protocol>` 함수 추가
2. `TestMain`에 해당 backend의 loopback 셋업 추가
3. CI workflow에 필요한 커널 모듈 추가 (있는 경우)

---

## 7. 기존 TC와의 중복 — CSI Sanity가 대체할 수 있는 TC

CSI Sanity는 **CSI gRPC 스펙이 정의한 에러 코드/멱등성 패턴**을 자동으로
검증한다. 기존 TC 중 이 패턴과 정확히 겹치는 것들은 CSI Sanity에 위임할 수 있다.

### 7.1 대체 가능 TC 목록

| 기존 TC | 기존 검증 내용 | CSI Sanity 대응 테스트 | 조치 |
|---------|-------------|---------------------|------|
| E1.2 ID3 (`MissingParams`) | StorageClass 파라미터 누락 → InvalidArgument | "CreateVolume fails with no name" + "CreateVolume fails with no capabilities" | **대체 가능** — Sanity가 이름/capabilities 누락을 검증. 단, SC 파라미터 누락은 pillar-csi 고유이므로 Component TC 유지 |
| E1.6-7 (`VolumeCapabilities_Empty`) | 빈 VolumeCapabilities → InvalidArgument | "CreateVolume fails with no capabilities" | **대체 가능** |
| E1.7-5~6 (`ExistingTooSmall/TooLarge`) | 동일 이름 다른 용량 → AlreadyExists | "CreateVolume same name different capacity returns AlreadyExists" | **대체 가능** |
| E1.4 ID10 (`MalformedID`) | 빈/잘못된 VolumeId → InvalidArgument | "DeleteVolume fails with no volume id" | **대체 가능** — 빈 ID. Malformed ID(`"noslash"`)는 pillar-csi 고유 형식이므로 Component TC 유지 |
| E1.3 ID9 (`NotFoundIsIdempotent`) | 존재하지 않는 ID → DeleteVolume 성공 | "DeleteVolume succeeds with invalid id" (멱등성) | **대체 가능** |
| E2.6-3 (`EmptyVolumeID`) | VolumeId="" → InvalidArgument | "ControllerPublish fails with no volume id" | **대체 가능** |
| E2.6-4 (`EmptyNodeID`) | NodeId="" → InvalidArgument | "ControllerPublish fails with no node id" | **대체 가능** |
| E2.6-5 (`NilVolumeCapability`) | VolumeCapability=nil → InvalidArgument | "ControllerPublish fails with no volume capability" | **대체 가능** |
| E3 에러 경로 부분집합 | NodeStage/NodePublish: VolumeId/Path/Capability 없음 → InvalidArgument | "NodeStage fails with no volume id/staging path/capability", "NodePublish fails with no volume id/target path/capability" | **대체 가능** |
| E11 에러 경로 부분집합 | ControllerExpand: VolumeId/Capability 없음 → InvalidArgument | "ControllerExpand fails with no volume id/capability" | **대체 가능** |

### 7.2 대체 불가 TC (유지해야 하는 것)

| 기존 TC | 유지 이유 |
|---------|----------|
| E1.2 ID3 (`MissingParams`) SC 파라미터 부분 | pillar-csi 고유 파라미터(`target`, `backend-type`, `pool`) — CSI 스펙에 없음 |
| E1.4 ID10 (`MalformedID`) `"noslash"` 부분 | pillar-csi 고유 VolumeId 형식 검증 (`target/protocol/backend/pool/name`) |
| E2.6-6 (`MalformedVolumeID`) | 위와 동일 — 고유 VolumeId 파싱 로직 |
| E3 전체 오케스트레이션 | Connector/Mounter/StateFile 위임 순서 — CSI 스펙 범위 밖 |
| E11 agent 위임 에러 전파 | agent.ExpandVolume 실패 → 에러 코드 전파 — CSI 스펙 범위 밖 |
| E16 동시 작업 | CSI Sanity는 순차 실행만 |
| E24 장애/복구 체인 | 단계별 실패 주입 — CSI Sanity에 없음 |

### 7.3 새로운 커버리지 (기존에 없던 것)

| CSI Sanity 테스트 | 설명 |
|-------------------|------|
| Identity 서비스 전체 (3개) | 현재 `csi_identity_test.go`에 기본 테스트만 존재. 스펙 준수 수준의 체계적 검증은 없음 |
| Full lifecycle chain | 실제 backend에서 Create→Publish→Stage→Publish→Stats→Unpublish→Unstage→Unpublish→Delete 전체 체인 |
| ControllerUnpublish 멱등성 | 미게시 볼륨에 대한 Unpublish → 성공 (현재 미검증) |
| NodeUnstage 멱등성 | 미스테이징 볼륨에 대한 Unstage → 성공 (현재 미검증) |

---

## 8. 구현 계획

### 8.1 파일 구조

```
test/
  sanity/
    sanity_test.go        # sanity.Test(t, config) 진입점 (backend별 테스트 함수)
    setup_test.go         # TestMain: loopback/zpool/VG 생성, CSI 서버 시작
    config_test.go        # backend별 sanity.TestConfig 생성 헬퍼
```

### 8.2 빌드 태그

```go
//go:build sanity

// CSI Sanity 테스트는 실제 backend(loopback ZFS/LVM)를 요구하므로
// 기본 `go test`에서 실행되지 않는다.
```

실행: `go test -tags=sanity ./test/sanity/ -v`

### 8.3 사전 의존성

| 의존성 | 용도 | CI 설치 |
|--------|------|---------|
| `github.com/kubernetes-csi/csi-test/v5` | Sanity 테스트 프레임워크 | `go mod tidy` |
| ZFS userspace (`zfsutils-linux`) | `zpool create/destroy` | `apt-get install -y zfsutils-linux` |
| LVM userspace (`lvm2`) | `vgcreate/vgremove` | `apt-get install -y lvm2` |
| 커널 모듈 (NVMe-oF/iSCSI) | protocol 테스트 시 | 기존 E2E CI 설정 재사용 |

### 8.4 CI 통합

```yaml
# .github/workflows/ci.yml (발췌)
sanity:
  name: CSI Sanity
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version-file: go.mod

    - name: Install storage prerequisites
      run: |
        sudo apt-get update
        sudo apt-get install -y zfsutils-linux lvm2
        sudo modprobe zfs || true

    - name: Run CSI Sanity tests
      run: go test -tags=sanity ./test/sanity/ -v -timeout 10m
```

### 8.5 구현 단계

| 단계 | 작업 | 산출물 |
|------|------|--------|
| S1 | `go get github.com/kubernetes-csi/csi-test/v5` 의존성 추가 | `go.mod`, `go.sum` |
| S2 | `test/sanity/setup_test.go` — TestMain 작성 (loopback + CSI 서버 시작) | ZFS zvol backend 1종 |
| S3 | `test/sanity/sanity_test.go` — `TestCSISanity_ZFS_NVMeoF` 작성 | 첫 번째 sanity 통과 |
| S4 | LVM backend 추가 (`TestCSISanity_LVM_NVMeoF`) | 2종 backend |
| S5 | iSCSI protocol 추가 | 4종 매트릭스 완성 |
| S6 | CI workflow 추가 | GHA에서 자동 실행 |
| S7 | 중복 TC 정리 — 7.1의 대체 가능 TC를 `// Covered by CSI Sanity` 주석 처리 또는 제거 | TC 수 감소 |

---

## 부록: CSI Sanity가 발견할 수 있는 실제 backend 에러 예시

| 시나리오 | 에러 | Sanity에서 발견? |
|----------|------|-----------------|
| ZFS zpool이 꽉 찬 상태에서 CreateVolume | `zfs create` 실패 → gRPC ResourceExhausted (또는 Internal) | 라이프사이클 테스트에서 발견 |
| LVM VG에 thin pool 없이 thin provisioning 시도 | `lvcreate` 실패 → gRPC Internal | CreateVolume 정상 경로에서 발견 |
| NVMe-oF subsystem 생성 시 NQN 중복 | `nvmet` configfs 충돌 → gRPC AlreadyExists 또는 Internal | ControllerPublish에서 발견 |
| iSCSI LUN export 시 target portal 미설정 | LIO configfs 에러 → gRPC Internal | ControllerPublish에서 발견 |

이런 에러들은 mock backend에서는 절대 발견할 수 없다.
CSI Sanity + 실제 backend 조합의 가치가 여기에 있다.
