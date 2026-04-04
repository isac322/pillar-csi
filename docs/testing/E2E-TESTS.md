# E2E Tests — 사용자 시나리오 전체 재현

실제 인프라에서 PVC 생성 → Pod 마운트 → 데이터 I/O → 정리까지 전체 사용자 시나리오를
재현한다. 실제 스토리지 backend(LVM VG, ZFS zpool)와 실제 네트워크 프로토콜(NVMe-oF TCP,
iSCSI)을 사용한다.

**빌드 태그:**
- Kind + loopback: `//go:build e2e` 또는 `//go:build e2e_helm`

**인프라 요구사항:**
| 구성 요소 | 버전 | 비고 |
|-----------|------|------|
| Kind | v0.23+ | 로컬 클러스터 |
| Docker | 24+ | Kind 노드 컨테이너 |
| 커널 모듈 | nvmet, nvmet_tcp, target_core_mod, iscsi_target_mod | `linux-modules-extra` 설치 필요 |
| pillar-csi 이미지 | `example.com/pillar-csi:v0.0.1` | `make docker-build` |

**GHA CI 설정:**
```yaml
- name: Install storage kernel modules
  run: |
    sudo apt-get update
    sudo apt-get install -y linux-modules-extra-$(uname -r) || \
      sudo apt-get install -y linux-modules-extra-azure
    sudo modprobe nvmet nvmet_tcp target_core_mod iscsi_target_mod
```

**컴포넌트 약어표:**

| 약어 | 컴포넌트 경로 |
|------|-------------|
| `CSI-C` | `internal/csi.ControllerServer` |
| `CSI-N` | `internal/csi.NodeServer` |
| `Agent` | `internal/agent.Server` (또는 mockAgentServer gRPC stub) |
| `ZFS` | `internal/backend.ZFSBackend` |
| `LVM` | `internal/agent/backend/lvm.Backend` |
| `NVMeF` | NVMe-oF TCP protocol path |
| `Conn` | `internal/csi.Connector` |
| `Mnt` | `internal/csi.Mounter` |
| `State` | staging state file management |
| `gRPC` | agent gRPC interface |
| `TgtCRD` | PillarTarget CRD |
| `PoolCRD` | PillarPool CRD |
| `BindCRD` | PillarBinding CRD |
| `VolCRD` | PillarVolume CRD |
| `SC` | Kubernetes StorageClass |
| `mTLS` | mutual TLS authentication |

---

## 목차

### Kind + 실제 스토리지 (loopback + 커널 모듈)
- [E10: 클러스터 레벨 E2E 테스트](#e10-클러스터-레벨-e2e-테스트)
- [E33: LVM Kind 클러스터 E2E — 실제 LVM VG + NVMe-oF TCP](#e33-lvm-kind-클러스터-e2e--실제-lvm-vg--nvme-of-tcp)
  - [E33.1 LVM 백엔드 Core RPC](#e331-lvm-백엔드-core-rpc)
  - [E33.2 LVM PVC 프로비저닝 및 Pod 마운트](#e332-lvm-pvc-프로비저닝-및-pod-마운트)
  - [E33.3 LVM 볼륨 확장](#e333-lvm-볼륨-확장)
  - [E33.4 LVM 백엔드 독립 E2E (Standalone)](#e334-lvm-백엔드-독립-e2e-standalone)
- [E34: LVM Kind 클러스터 E2E — 실제 LVM VG + iSCSI](#e34-lvm-kind-클러스터-e2e--실제-lvm-vg--iscsi)
  - [E34.1 iSCSI 제어면 및 export 계약](#e341-iscsi-제어면-및-export-계약)
  - [E34.2 iSCSI PVC 프로비저닝 및 Pod 마운트](#e342-iscsi-pvc-프로비저닝-및-pod-마운트)
  - [E34.3 Raw Block, 확장, 통계 및 재스테이징](#e343-raw-block-확장-통계-및-재스테이징)
- [E35: ZFS Kind 클러스터 E2E — 실제 ZFS zvol + iSCSI](#e35-zfs-kind-클러스터-e2e--실제-zfs-zvol--iscsi)
  - [E35.1 zvol 백엔드 제어면 및 export 계약](#e351-zvol-백엔드-제어면-및-export-계약)
  - [E35.2 zvol-backed Filesystem PVC 및 Pod 마운트](#e352-zvol-backed-filesystem-pvc-및-pod-마운트)
  - [E35.3 Raw Block, 확장, 통계 및 재스테이징](#e353-raw-block-확장-통계-및-재스테이징)

### 장애 복구 및 이상 상황 E2E (Kind 시뮬레이션)
- [E-FAULT-1: 노드 리부트 후 Agent ReconcileState 복구](#e-fault-1-노드-리부트-후-agent-reconcilestate-복구)
- [E-FAULT-2: Agent 네트워크 단절 및 복구](#e-fault-2-agent-네트워크-단절-및-복구)
- [E-FAULT-3: 스토리지 풀 용량 고갈 (loopback)](#e-fault-3-스토리지-풀-용량-고갈-loopback)
- [E-FAULT-4: 블록 디바이스 제거 (loopback detach)](#e-fault-4-블록-디바이스-제거-loopback-detach)
- [E-FAULT-5: 다중 노드 시나리오](#e-fault-5-다중-노드-시나리오)

### 수동/스테이징 테스트
- [AD 시나리오: Agent Down 수동 검증](#ad-시나리오-agent-down-수동-검증)
- [BP 시나리오: 비호환 백엔드-프로토콜 수동 검증](#bp-시나리오-비호환-백엔드-프로토콜-수동-검증)
- [유형 M: 수동/스테이징 테스트](#유형-m-수동스테이징-테스트)
  - [M1: 롤링 업그레이드 검증](#m1-롤링-업그레이드-검증)
  - [M2: 스토리지 네트워크 분리](#m2-스토리지-네트워크-분리)
  - [M3: 물리적 디스크/하드웨어 장애](#m3-물리적-디스크하드웨어-장애)
  - [M4: 커널 버전 호환성 매트릭스](#m4-커널-버전-호환성-매트릭스)
  - [M5: 프로덕션 유사 부하 및 용량 계획](#m5-프로덕션-유사-부하-및-용량-계획)
  - [M6: 보안 감사 및 침투 테스트](#m6-보안-감사-및-침투-테스트)
  - [M7: 데이터 무결성 심층 검증](#m7-데이터-무결성-심층-검증)
  - [M8: CSI 드라이버 업그레이드 절차 검증](#m8-csi-드라이버-업그레이드-절차-검증)
  - [M9: 다중 테넌트 격리 검증](#m9-다중-테넌트-격리-검증)
  - [M10: 인증서 수명 주기 및 실제 PKI 갱신](#m10-인증서-수명-주기-및-실제-pki-갱신)

---

# Kind + 실제 스토리지 (loopback + 커널 모듈)

---

## E10: 클러스터 레벨 E2E 테스트

**테스트 유형:** B (클러스터 레벨)

**빌드 태그:** `//go:build e2e`

**실행 방법:**
```bash
# Kind 클러스터 준비 후
go test ./test/e2e/ -tags=e2e -v -run TestE2E
```

**필수 인프라:** Kind 클러스터; `make docker-build` 후 이미지 로드; CRD 설치; 매니저 배포 완료

---

### E10.1 매니저 배포 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 68 | `TestE2E/Manager_컨트롤러_파드_실행_확인` | pillar-csi-controller-manager 파드가 `pillar-csi-system` 네임스페이스에서 정상 실행됨 | Kind 클러스터; `make docker-build` 후 이미지 로드; CRD 설치; 매니저 배포 완료 | 1) pillar-csi-system 네임스페이스에서 파드 목록 조회; 2) 파드 상태 확인 | 컨트롤러 파드가 Running 상태; 재시작 없음 | `전체시스템`, `Kubernetes클러스터` |
| 69 | `TestE2E/매니저_메트릭스_서비스_접근_가능` | RBAC RoleBinding 생성 후 `/metrics` 엔드포인트에서 메트릭 수집 가능 | Kind 클러스터; 컨트롤러 파드 Running; 메트릭 RoleBinding 생성 | 1) kubectl port-forward 또는 직접 curl로 /metrics 접근 | HTTP 200 응답; Go 런타임 메트릭 포함 | `전체시스템`, `Kubernetes클러스터` |
| 70 | `TestE2E/cert-manager_통합` | cert-manager가 설치된 환경에서 TLS 인증서 발급 동작 | Kind 클러스터; cert-manager v1.14+ 설치 완료; 클러스터 배포 | 1) cert-manager Certificate 리소스 상태 확인 | 인증서 발급 성공; Secret에 tls.crt/tls.key 존재 | `전체시스템`, `cert-manager`, `TgtCRD` |

---

## E33: LVM Kind 클러스터 E2E — 실제 LVM VG + NVMe-oF TCP

**테스트 유형:** D (Kind 클러스터 + 실제 LVM VG)

> **유형 A와의 차이:** mock backend가 아닌 **실제 LVM VG**(루프백 디바이스 기반)와
> **실제 NVMe-oF TCP 커널 모듈**을 사용한다.
> **유형 F와의 차이:** Kind 클러스터 내부 Docker 컨테이너의 루프백 LVM VG를
> 사용하므로 베어메탈 서버 불필요.

**인프라 요구사항:**

| 항목 | 버전/사양 | 비고 |
|------|----------|------|
| Kind | v0.23+ | 2노드 클러스터 (storage-worker + compute-worker) |
| Docker | 24+ | 컨테이너 이미지 빌드 및 Kind 로드 |
| 호스트 커널 | 5.15+ | nvmet, nvmet-tcp, nvme-tcp, dm_thin_pool 모듈 |
| LVM2 도구 | `lvcreate`, `vgs`, `lvs` | Kind 워커 컨테이너 내 설치 |
| pillar-csi 이미지 | 로컬 빌드 | `make docker-build` |
| `PILLAR_E2E_LVM_VG` | 환경변수 | LVM VG 이름 |
| `PILLAR_E2E_LVM_THIN_POOL` | 환경변수 (선택) | thin pool LV 이름 |

**빌드 태그:** `//go:build e2e`

```
실행 명령:
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="lvm"

특정 그룹만:
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="lvm && rpc"
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="lvm && mount"
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="lvm && expansion"
```

---

> **분류 참고:** E33의 하위 섹션 중 E33.1 (Core RPC)과 E33.4 (Standalone backend)는
> K8s PVC 흐름 없이 Agent + 실제 LVM만 테스트하므로 엄밀히는 **Integration** 레벨이다.
> E33.2 (PVC 프로비저닝 + Pod 마운트)와 E33.3 (볼륨 확장)만 PVC→Pod 전체 흐름을 재현하는 **E2E**이다.
> 다만 모두 동일한 Kind + LVM 인프라를 공유하므로 실행 편의상 E2E 문서에 함께 배치한다.
> Integration 관점의 E33.1/E33.4 커버리지는 INTEGRATION-TESTS.md의 E28과 중복/보완 관계이다.

### E33.1 LVM 백엔드 Core RPC

**위치:** `test/e2e/lvm_backend_core_rpcs_e2e_test.go`

6개 핵심 LVM 백엔드 RPC를 Kind 클러스터 내 실제 pillar-agent에 대해 검증.
kubectl port-forward로 agent gRPC 포트(9500)에 접근하여 `LvmVolumeParams`가
실제 `lvcreate`/`lvremove`/`lvextend`/`vgs`/`lvs` 명령으로 변환되는 과정을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 285 | `It("GetCapacity returns positive total and available bytes for the LVM VG")` | 실제 LVM VG의 GetCapacity가 양수 total/available 반환 | Kind; pillar-agent Running; LVM VG 존재; port-forward 활성 | 1) GetCapacityRequest(poolName=VG) | TotalBytes > 0; AvailableBytes > 0; AvailableBytes <= TotalBytes | `Agent`, `LVM`, `gRPC` |
| 286 | `It("CreateVolume (thin) returns device_path=/dev/<vg>/<lv>")` | Thin LV 생성 시 `/dev/<vg>/<lv>` 형식 device_path | thin pool 설정; port-forward 활성 | 1) CreateVolumeRequest(thin) | device_path="/dev/<vg>/<lv>"; capacity_bytes >= 요청 크기 | `Agent`, `LVM`, `gRPC` |
| 287 | `It("CreateVolume (linear) creates a linear LV using ProvisionMode override")` | ProvisionMode="linear" 오버라이드로 linear LV 생성 | thin 기본 모드 VG; port-forward | 1) CreateVolumeRequest(ProvisionMode="linear") | 성공; device_path 형식 정확 | `Agent`, `LVM`, `gRPC` |
| 288 | `It("DeleteVolume destroys an LV and is idempotent")` | LV 삭제 + 동일 ID 재삭제 멱등 | CreateVolume 성공 | 1) DeleteVolume; 2) 동일 ID 재삭제 | 두 호출 모두 성공 | `Agent`, `LVM`, `gRPC` |
| 289 | `It("ExpandVolume grows an LVM LV to at least the requested size")` | LV 확장 후 capacity_bytes >= 요청 크기 — PE 반올림 허용 | CreateVolume(1GiB) 성공 | 1) ExpandVolumeRequest(2GiB) | capacity_bytes >= 2GiB | `Agent`, `LVM`, `gRPC` |
| 290 | `It("ListVolumes returns created LVs with correct device_path")` | 생성된 LV가 올바른 device_path와 함께 ListVolumes에 포함 | CreateVolume 성공 | 1) ListVolumesRequest | 생성한 LV volumeId/device_path 포함 | `Agent`, `LVM`, `gRPC` |
| 291 | `It("CreateVolume is idempotent: re-creating with same volume ID succeeds (linear)")` | 동일 volumeId linear LV 재생성 멱등 | linear CreateVolume 성공 | 1) 동일 파라미터 재호출 | 성공; 동일 device_path | `Agent`, `LVM`, `gRPC` |
| 292 | `It("CreateVolume is idempotent: re-creating with same volume ID succeeds (thin)")` | 동일 volumeId thin LV 재생성 멱등 | thin CreateVolume 성공 | 1) 동일 파라미터 재호출 | 성공; 동일 device_path | `Agent`, `LVM`, `gRPC` |
| 293 | `It("returns an error for a non-existent LVM VG pool name")` | 존재하지 않는 VG로 GetCapacity 오류 | port-forward 활성 | 1) GetCapacityRequest("nonexistent-vg") | gRPC 오류 | `Agent`, `LVM`, `gRPC` |

---

### E33.2 LVM PVC 프로비저닝 및 Pod 마운트

**위치:** `test/e2e/lvm_pvc_pod_mount_e2e_test.go`

LVM 백엔드로 PillarTarget -> PillarPool(lvm-lv) -> PillarProtocol(nvmeof-tcp) ->
PillarBinding CR 스택을 생성하고, 실제 PVC 프로비저닝 및 Pod 마운트/언마운트
전체 라이프사이클을 검증한다. 이 테스트는 CSI Controller -> Agent -> LVM 백엔드 ->
NVMe-oF TCP -> 워커 노드 마운트의 **전체 데이터 경로**를 관통한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 294 | `It("PillarPool BackendSupported condition becomes True (agent advertises lvm-lv)")` | agent가 lvm-lv 백엔드 지원 보고 | PillarTarget Ready; PillarPool(type=lvm-lv) 생성 | 1) PillarPool 조건 폴링 | BackendSupported=True | `Agent`, `LVM`, `PoolCRD` |
| 295 | `It("PillarPool PoolDiscovered condition becomes True (VG is visible to agent)")` | agent가 LVM VG 발견 | PillarTarget Ready; PillarPool 생성 | 1) PillarPool 조건 폴링 | PoolDiscovered=True | `Agent`, `LVM`, `PoolCRD` |
| 296 | `It("PillarPool reaches Ready=True and reports capacity")` | PillarPool Ready + VG 용량 보고 | 위 조건 True | 1) PillarPool 상태 확인 | Ready=True; capacity.total > 0; capacity.available > 0 | `Agent`, `LVM`, `PoolCRD` |
| 297 | `It("PillarBinding generates a Kubernetes StorageClass with the pillar-csi provisioner")` | StorageClass 자동 생성 | PillarPool Ready; PillarProtocol/PillarBinding 생성 | 1) StorageClass 확인 | provisioner=pillar-csi.bhyoo.com; parameters에 backend-type=lvm-lv | `BindCRD`, `SC` |
| 298 | `It("first PVC (1Gi) becomes Bound via LVM CreateVolume")` | 1GiB PVC -> LVM LV 프로비저닝 -> Bound | StorageClass 존재; VG 여유 | 1) PVC(1Gi) 생성; 2) Bound 대기 | PVC Phase=Bound | `CSI-C`, `Agent`, `LVM`, `VolCRD` |
| 299 | `It("bound PV (first PVC) has capacity >= 1Gi")` | PV capacity >= 1Gi — LVM PE 반올림 가능 | PVC Bound | 1) PV capacity 확인 | storage >= 1Gi | `CSI-C`, `LVM` |
| 300 | `It("bound PV (first PVC) references the correct StorageClass")` | PV StorageClass 참조 정확 | PVC Bound | 1) PV.spec.storageClassName 확인 | 이름 일치 | `CSI-C` |
| 301 | `It("bound PV (first PVC) uses the Delete reclaim policy")` | PV reclaimPolicy=Delete | PVC Bound | 1) PV.spec.persistentVolumeReclaimPolicy | Delete | `CSI-C` |
| 302 | `It("second PVC (2Gi) is independently provisioned and Bound")` | 두 번째 PVC 독립 Bound | 첫 PVC Bound | 1) PVC(2Gi) 생성; 2) Bound 대기 | PVC Bound; PV capacity >= 2Gi | `CSI-C`, `Agent`, `LVM` |
| 303 | `It("a Pod mounting the LVM PVC starts Running on the compute-worker node")` | NVMe-oF TCP 경유 Pod 마운트 성공 — NodeStage(connect) + NodePublish(mount) | PVC Bound; NVMe-oF TCP 모듈 | 1) Pod 생성; 2) Running 대기 | Pod Running on compute-worker | `CSI-C`, `CSI-N`, `Agent`, `LVM`, `NVMeF`, `Conn`, `Mnt` |
| 304 | `It("Pod deletion triggers NodeUnpublish + NodeUnstage + ControllerUnpublish")` | Pod 삭제 시 NVMe-oF disconnect + umount 정리 | Pod Running | 1) Pod 삭제; 2) 완료 대기 | Pod 삭제; NVMe-oF 연결 해제 | `CSI-C`, `CSI-N`, `Conn`, `Mnt` |
| 305 | `It("PVC deletion after Pod removal triggers DeleteVolume (LV destroyed on agent)")` | PVC 삭제 -> agent에서 LV 제거 | Pod 삭제 완료; PVC 존재 | 1) PVC 삭제; 2) PV 삭제 대기 | PV 삭제; agent에서 LV 제거 | `CSI-C`, `Agent`, `LVM`, `VolCRD` |

---

### E33.3 LVM 볼륨 확장

**위치:** `test/e2e/lvm_volume_expansion_e2e_test.go`

LVM PVC의 온라인 볼륨 확장을 실행 중 Pod 내부에서 파일시스템 크기 변화까지 검증.
CSI resizer -> ControllerExpandVolume(`lvextend`) -> NodeExpandVolume(`resize2fs`) 경로.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 306 | `It("Pod mounts 1Gi LVM PVC and reaches Running")` | 1Gi PVC 마운트 Pod Running | AllowVolumeExpansion=true; PVC Bound; NVMe-oF 설정 | 1) Pod 생성; 2) Running 대기 | Pod Running | `CSI-C`, `CSI-N`, `Agent`, `LVM`, `NVMeF` |
| 307 | `It("filesystem inside Pod reports approximately 1Gi capacity before expansion")` | 확장 전 `df` 출력 ~= 1Gi | Pod Running | 1) `kubectl exec: df --output=avail /data` | avail ~= 1Gi (+-10%) | `CSI-N`, `Mnt` |
| 308 | `It("PVC resize to 2Gi is reflected in PVC status capacity")` | PVC spec 2Gi 패치 후 status.capacity 갱신 — `lvextend` 실행 확인 | Pod Running; PVC Bound | 1) PVC 크기 2Gi 패치; 2) status.capacity 폴링 | status.capacity >= 2Gi | `CSI-C`, `Agent`, `LVM` |
| 309 | `It("filesystem inside running Pod is resized to >= 2Gi after PVC expansion")` | `resize2fs` 후 Pod 내 `df` >= 2Gi | 확장 완료 | 1) `kubectl exec: df --output=avail /data` | avail >= 기대값 90% | `CSI-N`, `Mnt`, `LVM` |
| 310 | `It("Pod deletion and PVC deletion complete cleanly after expansion")` | 확장된 볼륨 Pod/PVC 정리 | 확장 완료; Pod Running | 1) Pod 삭제; 2) PVC 삭제; 3) PV 삭제 대기 | 모든 리소스 정리 | `CSI-C`, `CSI-N`, `Agent`, `LVM` |

---

### E33.4 LVM 백엔드 독립 E2E (Standalone)

**위치:** `test/e2e/lvmbackend/lvm_backend_test.go`

Kind 클러스터 없이 Docker 컨테이너에서 직접 pillar-agent를 기동하여
LVM 백엔드 RPC를 검증. 루프백 디바이스 기반 LVM VG + thin pool 사용.
Docker host에서 블록 디바이스 존재를 직접 확인하므로 실제 `lvcreate`/`lvremove` 실행을 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 311 | `TestLVMBackend_CreateVolume_Thin` | Thin LV 생성 + 호스트에서 `/dev/<vg>/<lv>` 존재 확인 | Docker 내 LVM VG + thin pool; pillar-agent 실행 중 | 1) CreateVolume(thin); 2) 호스트 device 폴링 | device_path="/dev/<vg>/<lv>"; 호스트 블록 디바이스 존재 | `Agent`, `LVM`, `gRPC` |
| 312 | `TestLVMBackend_CreateVolume_Linear` | Linear LV 생성 + ProvisionMode 오버라이드 검증 | 동일 | 1) CreateVolume(linear); 2) 디바이스 확인 | 성공; device_path 형식 정확 | `Agent`, `LVM`, `gRPC` |
| 313 | `TestLVMBackend_DeleteVolume` | LV 삭제 + 디바이스 소멸 + 재삭제 멱등 | CreateVolume 성공 | 1) DeleteVolume; 2) 소멸 확인; 3) 재삭제 | 디바이스 없음; 재삭제 성공 | `Agent`, `LVM`, `gRPC` |
| 314 | `TestLVMBackend_ExpandVolume` | LV 확장 후 capacity_bytes >= 요청 | CreateVolume 성공 | 1) ExpandVolume(2x) | capacity_bytes >= 요청 | `Agent`, `LVM`, `gRPC` |
| 315 | `TestLVMBackend_GetCapacity` | VG 용량: total > 0, available <= total | LVM VG 존재 | 1) GetCapacity | total > 0; available <= total | `Agent`, `LVM`, `gRPC` |
| 316 | `TestLVMBackend_ListVolumes` | 생성된 LV가 ListVolumes에 포함 | CreateVolume 성공 | 1) ListVolumes | volumeId/device_path 포함 | `Agent`, `LVM`, `gRPC` |
| 317 | `TestLVMBackend_DevicePath` | CreateVolume과 ListVolumes 모두 `/dev/<vg>/<lv>` 형식 일관 | CreateVolume 성공 | 1) device_path 비교 | `/dev/<vg>/<lv>` 형식 일관 | `Agent`, `LVM`, `gRPC` |

---

### E33 커버리지 요약

| 소섹션 | 검증 내용 | 테스트 수 | 인프라 |
|--------|---------|----------|--------|
| E33.1 | Core RPC — linear/thin create, delete, expand, capacity, list, idempotency, error | 9개 | Kind + LVM |
| E33.2 | PVC 프로비저닝 -> Pod 마운트 -> 정리 전체 라이프사이클 | 12개 | Kind + LVM + NVMe-oF |
| E33.3 | 온라인 볼륨 확장 — ControllerExpand(lvextend) + NodeExpand(resize2fs) | 5개 | Kind + LVM + NVMe-oF |
| E33.4 | 백엔드 독립 E2E — Docker 직접 agent, 호스트 디바이스 검증 | 7개 | Docker + LVM |
| **합계** | | **33개** | |

**CI 실행 가능 여부:**
E33 테스트는 표준 GitHub Actions에서 LVM 루프백 VG를 생성할 수 있으므로
**조건부 CI 실행 가능**하다. 단, NVMe-oF 커널 모듈(`nvmet`, `nvmet-tcp`,
`nvme-tcp`)이 필요한 E33.2/E33.3은 커널 모듈 지원 러너가 필요하다.

---

## E34: LVM Kind 클러스터 E2E — 실제 LVM VG + iSCSI

**테스트 유형:** D (Kind 클러스터 + 실제 LVM VG + 실제 iSCSI)

> **목적:** E33이 `NVMe-oF TCP` 경로를 검증한다면, E34는 동일한 `LVM LV`
> 백엔드를 `iSCSI`로 export 했을 때의 전체 제품 경로를 검증한다.
> 즉, `PillarProtocol(type=iscsi)` -> generated `StorageClass` ->
> `ControllerPublish/Unpublish` ACL -> `NodeStage` discovery/login/logout ->
> filesystem/raw block 사용까지를 실제 Kind 환경에서 검증한다.

**인프라 요구사항:**

| 항목 | 버전/사양 | 비고 |
|------|----------|------|
| Kind | v0.23+ | 2노드 클러스터 (storage-worker + compute-worker) |
| Docker | 24+ | Kind 및 테스트 이미지 로드 |
| 호스트 커널 | 5.15+ | `target_core_mod`, `iscsi_target_mod`, `iscsi_tcp`, `libiscsi` 모듈 필요 |
| configfs | `/sys/kernel/config` | storage-worker에서 LIO target export용 |
| LVM2 도구 | `lvcreate`, `vgs`, `lvs` | storage-worker 컨테이너 내 설치 |
| open-iscsi | `iscsiadm`, `iscsid` | compute-worker 측 node image에 번들 |
| pillar-csi 이미지 | 로컬 빌드 | `make docker-build` |
| `PILLAR_E2E_LVM_VG` | 환경변수 | 테스트용 VG 이름 |
| `PILLAR_E2E_ISCSI_PORT` | 환경변수 (선택) | 기본값 `3260` |

**빌드 태그:** `//go:build e2e`

```
실행 명령:
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi"

특정 그룹만:
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi && controlplane"
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi && mount"
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi && expansion"
```

**위치(신규 작성 필요):**

- `test/e2e/lvm_iscsi_core_rpcs_e2e_test.go`
- `test/e2e/lvm_iscsi_pvc_pod_mount_e2e_test.go`
- `test/e2e/lvm_iscsi_volume_expansion_e2e_test.go`

---

> **분류 참고:** E34.1 (iSCSI 제어면 및 export 계약)은 generated StorageClass,
> CreateVolume VolumeContext, Publish/Unpublish ACL 등 컨트롤러 계약을 검증하며
> PVC→Pod 전체 마운트 흐름을 재현하지 않으므로 엄밀히는 **Integration** 레벨이다.
> E34.2 (PVC 프로비저닝 + Pod 마운트)와 E34.3 (Raw Block, 확장, 통계, 재스테이징)만
> iSCSI discovery/login/mount까지 포함하는 **E2E**이다.
> 다만 모두 동일한 Kind + LVM + LIO 인프라를 공유하므로 실행 편의상 E2E 문서에 함께 배치한다.

### E34.1 iSCSI 제어면 및 export 계약

`PillarProtocol(type=iscsi)`가 만들어내는 generated `StorageClass`와
CSI Controller의 `CreateVolume` / `ControllerPublish` / `ControllerUnpublish`
계약을 검증한다. 핵심 포인트는 app 사용자가 `portal`, `target IQN`, `LUN`을
직접 다루지 않아도 되고, 이 값들이 런타임 `VolumeContext`로만 노출되며,
publish/unpublish는 `CSINode` annotation에 publish된 initiator IQN을 기준으로 동작한다는 점이다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 318 | `It("PillarBinding generates an iSCSI StorageClass with protocol-type=iscsi and timer parameters")` | generated StorageClass가 `protocol-type=iscsi`, `iscsi-port`, iSCSI timeout 파라미터를 포함 | Kind; PillarTarget Ready; PillarPool(type=lvm-lv); PillarProtocol(type=iscsi) 생성 | 1) PillarBinding 생성; 2) StorageClass 조회 | StorageClass 존재; `parameters["pillar-csi.bhyoo.com/protocol-type"]=="iscsi"`; `iscsi-port` 및 iSCSI timer 파라미터 존재 | `CSI-C`, `TgtCRD`, `gRPC` |
| 319 | `It("CreateVolume returns target IQN, portal, port and LUN in VolumeContext")` | iSCSI CreateVolume이 PV `VolumeContext`에 target IQN, portal IP, port, LUN을 기록 | StorageClass Ready; agent가 iSCSI export 가능 | 1) PVC 생성; 2) PV 생성 대기; 3) PV `spec.csi.volumeAttributes` 조회 | `target_id`는 IQN 형식; `address`는 storage-worker IP; `port==3260`(또는 override 값); `volume_ref`는 LUN 문자열 | `CSI-C`, `Agent`, `LVM`, `VolCRD`, `gRPC` |
| 320 | `It("pillar-node publishes the initiator IQN to CSINode annotations and ControllerPublishVolume uses it for ACLs")` | node-side publisher가 compute-worker의 initiator IQN을 `CSINode` annotation에 반영하고, `ControllerPublishVolume`이 그 값을 사용해 ACL을 추가 | PVC Bound; Pod 미생성; compute-worker node image에 initiator IQN 설정 | 1) compute-worker `CSINode` annotation `pillar-csi.bhyoo.com/iscsi-initiator-iqn` 대기; 2) Pod 생성; 3) ControllerPublish 발생; 4) storage-worker의 LIO ACL 조회 | `CSINode` annotation 존재; Pod Running; 해당 target ACL에 annotation과 같은 compute-worker IQN 존재 | `CSI-C`, `CSI-N`, `Agent`, `LVM`, `gRPC` |
| 321 | `It("ControllerUnpublishVolume revokes the same CSINode-derived initiator IQN ACL")` | Pod 삭제 시 `CSINode` annotation에서 해석된 동일 IQN ACL이 제거 | 320 성공 후 Pod Running | 1) Pod 삭제; 2) ControllerUnpublish 완료 대기; 3) LIO ACL 조회 | target ACL에서 해당 IQN 제거; 다른 volume/session 영향 없음 | `CSI-C`, `Agent`, `LVM`, `gRPC` |

---

### E34.2 iSCSI PVC 프로비저닝 및 Pod 마운트

`CreateVolume` 이후 실제 compute-worker에서 discovery/login/mount가 수행되고,
filesystem PVC가 앱 관점에서 정상 사용 가능한지 검증한다. 이 섹션은
`pillar-csi`가 iSCSI를 "동적 프로비저닝되는 CSI block protocol"로 제공함을 보여준다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 322 | `It("filesystem PVC becomes Bound via LVM + iSCSI")` | `PillarProtocol(type=iscsi)`를 참조하는 StorageClass로 PVC가 정상 Bound | StorageClass Ready; VG 여유 공간 존재 | 1) Filesystem PVC(1Gi) 생성; 2) Bound 대기 | PVC Phase=Bound; PV provisioner=`pillar-csi.bhyoo.com` | `CSI-C`, `Agent`, `LVM`, `VolCRD` |
| 323 | `It("a Pod mounting the iSCSI PVC reaches Running on the compute-worker node")` | Pod 생성 시 compute-worker에서 iSCSI discovery/login 후 마운트 성공 | 322 성공; compute-worker에 open-iscsi 실행 가능 | 1) Pod 생성; 2) Running 대기; 3) `mount` / `lsblk` 확인 | Pod Running; pod 내부 mount 성공; node에서 active iSCSI session 1개 | `CSI-C`, `CSI-N`, `Agent`, `LVM`, `Conn`, `Mnt` |
| 324 | `It("PVC protocol override changes the iSCSI replacement timeout for one volume only")` | PVC annotation이 단일 volume의 iSCSI timeout만 오버라이드 | Binding에 기본 `replacementTimeout=120`; PVC annotation에 `replacementTimeout=180` 지정 | 1) PVC 생성; 2) Pod 생성; 3) node session 파라미터 조회 | 해당 session만 replacement timeout 180 반영; 다른 PVC/session은 기본값 유지 | `CSI-C`, `CSI-N`, `Agent`, `Conn` |
| 325 | `It("deleting the Pod triggers NodeUnpublish, NodeUnstage and iSCSI logout")` | Pod 삭제 시 bind mount 해제, staging 해제, session logout 수행 | 323 성공 후 Pod Running | 1) Pod 삭제; 2) node mount/session 상태 확인 | target path 정리; staging path 정리; `iscsiadm -m session`에서 세션 제거 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 326 | `It("deleting the PVC removes the exported target and destroys the LV")` | PVC 삭제 시 target export와 backend LV가 모두 정리 | 325 완료; PVC/PV 잔존 | 1) PVC 삭제; 2) PV 삭제 대기; 3) storage-worker에서 LIO target/LV 확인 | PV 제거; target export 없음; LV 삭제됨 | `CSI-C`, `Agent`, `LVM`, `VolCRD`, `gRPC` |

---

### E34.3 Raw Block, 확장, 통계 및 재스테이징

iSCSI를 단순 mount 경로가 아니라 block protocol로서 완성도 있게 제공하려면,
raw block, online expansion, node stats, restage idempotency까지 검증해야 한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 327 | `It("raw block PVC is published as an unformatted block device to the Pod")` | `volumeMode: Block` PVC가 raw block device로 publish | StorageClass Ready; block-mode PVC 생성 | 1) Block PVC 생성; 2) raw block consumer Pod 생성; 3) pod 내부 장치 확인 | pod에 block device 노출; filesystem 생성 흔적 없음; device path 접근 가능 | `CSI-C`, `CSI-N`, `Agent`, `LVM`, `Conn` |
| 328 | `It("online expansion rescans the iSCSI session and grows the filesystem inside the running Pod")` | PVC 확장 시 backend LV와 node filesystem이 모두 커짐 | Filesystem Pod Running; `allowVolumeExpansion=true` | 1) PVC 1Gi->2Gi patch; 2) Expand 완료 대기; 3) pod 내부 `df` 확인 | PVC/PV capacity 증가; iSCSI session rescan 완료; pod 내부 filesystem 용량 증가 | `CSI-C`, `CSI-N`, `Agent`, `LVM`, `Conn`, `Mnt` |
| 329 | `It("NodeGetVolumeStats reports bytes and inodes for filesystem volumes and bytes for raw block volumes")` | iSCSI 경로에서도 `NodeGetVolumeStats`가 filesystem/block 모드별로 올바른 usage를 반환 | 323, 327 성공 | 1) filesystem PVC에 stats 조회; 2) raw block PVC에 stats 조회 | filesystem은 bytes+inodes; raw block은 total bytes만 보고 | `CSI-N`, `Conn`, `Mnt` |
| 330 | `It("after node plugin restart, restaging is idempotent and does not create duplicate iSCSI sessions")` | node plugin 재시작 후 재스테이징이 session 중복 없이 복구 | Filesystem Pod Running; node plugin restart 가능 | 1) node plugin 재시작; 2) workload 유지/복구 대기; 3) session 수와 mount 상태 확인 | volume 재사용 성공; 동일 volume에 중복 session 없음; mount 상태 일관 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E34 커버리지 요약

| 소섹션 | 검증 내용 | 테스트 수 | 인프라 |
|--------|---------|----------|--------|
| E34.1 | iSCSI generated StorageClass, CreateVolume `VolumeContext`, Publish/Unpublish ACL | 4개 | Kind + LVM + LIO |
| E34.2 | Filesystem PVC 프로비저닝, Pod mount, per-volume timeout override, logout/cleanup | 5개 | Kind + LVM + LIO + open-iscsi |
| E34.3 | Raw block, online expansion, NodeGetVolumeStats, 재스테이징 멱등성 | 4개 | Kind + LVM + LIO + open-iscsi |
| **합계** | | **13개** | |

**CI 실행 가능 여부:**
E34는 E33보다 호스트 요구사항이 높다. LVM 루프백 VG 외에도
`target_core_mod`, `iscsi_target_mod`, `iscsi_tcp`, `libiscsi` 커널 모듈과
compute-worker 측 `open-iscsi` 런타임이 필요하므로, 표준 GitHub Actions에서는
기본적으로 비활성화하고 커널 모듈이 보장되는 self-hosted/전용 러너에서 실행한다.

**MVP 범위에서 제외되는 시나리오:**

- CHAP Secret 기반 인증
- multipath / multi-portal
- RWX

---

## E35: ZFS Kind 클러스터 E2E — 실제 ZFS zvol + iSCSI

**테스트 유형:** D (Kind 클러스터 + 실제 ZFS zvol + 실제 iSCSI)

> **목적:** E35는 `zfs-zvol` backend를 `iSCSI`로 export했을 때의 전체 제품 경로를
> 검증한다. `PillarProtocol(type=iscsi)`와 ZFS 고유 파라미터가 함께 적용되는지,
> `ControllerPublish/Unpublish` ACL과 `NodeStage` discovery/login/logout,
> filesystem/raw block/확장/통계/재스테이징까지 실제 Kind 환경에서 확인한다.

**인프라 요구사항:**

| 항목 | 버전/사양 | 비고 |
|------|----------|------|
| Kind | v0.23+ | 2노드 클러스터 (storage-worker + compute-worker) |
| Docker | 24+ | Kind 및 테스트 이미지 로드 |
| 호스트 커널 | 5.15+ | `zfs`, `target_core_mod`, `iscsi_target_mod`, `iscsi_tcp`, `libiscsi` 모듈 필요 |
| ZFS 도구 | `zpool`, `zfs` | storage-worker 측에서 실제 zvol 생성/삭제 |
| `/dev/zvol` | udev 활성화 | zvol 블록 장치 노출 필요 |
| configfs | `/sys/kernel/config` | storage-worker에서 LIO target export용 |
| open-iscsi | `iscsiadm`, `iscsid` | compute-worker 측 node image에 번들 |
| pillar-csi 이미지 | 로컬 빌드 | `make docker-build` |
| `PILLAR_E2E_ZFS_POOL` | 환경변수 | 테스트용 zpool 이름 |
| `PILLAR_E2E_ZFS_PARENT_DATASET` | 환경변수 (선택) | zvol parent dataset 검증용 |
| `PILLAR_E2E_ISCSI_PORT` | 환경변수 (선택) | 기본값 `3260` |

**빌드 태그:** `//go:build e2e`

```
실행 명령:
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi && zfs"

특정 그룹만:
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi && zfs && controlplane"
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi && zfs && mount"
  go test ./test/e2e/ -tags=e2e -v --ginkgo.label-filter="iscsi && zfs && expansion"
```

**위치(신규 작성 필요):**

- `test/e2e/zfs_iscsi_core_rpcs_e2e_test.go`
- `test/e2e/zfs_iscsi_pvc_pod_mount_e2e_test.go`
- `test/e2e/zfs_iscsi_volume_expansion_e2e_test.go`

---

> **분류 참고:** E35.1 (zvol 백엔드 제어면 및 export 계약)은 generated StorageClass,
> CreateVolume VolumeContext, Publish/Unpublish ACL 등 컨트롤러 계약을 검증하며
> PVC→Pod 전체 마운트 흐름을 재현하지 않으므로 엄밀히는 **Integration** 레벨이다.
> E35.2 (Filesystem PVC + Pod 마운트)와 E35.3 (Raw Block, 확장, 통계, 재스테이징)만
> iSCSI discovery/login/mount까지 포함하는 **E2E**이다.
> 다만 모두 동일한 Kind + ZFS + LIO 인프라를 공유하므로 실행 편의상 E2E 문서에 함께 배치한다.

### E35.1 zvol 백엔드 제어면 및 export 계약

`PillarPool(type=zfs-zvol)`과 `PillarProtocol(type=iscsi)` 조합이 만들어내는
generated `StorageClass`, `CreateVolume`, `ControllerPublish`, `ControllerUnpublish`
계약을 검증한다. 핵심은 ZFS 고유 파라미터가 유지된 채 iSCSI 연결 정보가
런타임 `VolumeContext`로 노출되고, publish/unpublish ACL이 `CSINode` annotation에서
해석한 initiator IQN에 맞춰 동작하는지 확인하는 것이다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 331 | `It("PillarBinding generates an iSCSI StorageClass for zfs-zvol pools without losing zvol parameters")` | generated StorageClass가 `backend-type=zfs-zvol`, pool/zvol 파라미터, `protocol-type=iscsi`, iSCSI timer 파라미터를 함께 포함 | Kind; PillarTarget Ready; PillarPool(type=zfs-zvol); PillarProtocol(type=iscsi) 생성 | 1) PillarBinding 생성; 2) StorageClass 조회 | StorageClass 존재; `parameters["pillar-csi.bhyoo.com/backend-type"]=="zfs-zvol"`; `protocol-type=iscsi`; pool/zvol 파라미터와 iSCSI 파라미터가 모두 유지 | `CSI-C`, `TgtCRD`, `gRPC`, `ZFS` |
| 332 | `It("CreateVolume provisions a zvol-backed volume and returns target IQN, portal, port and LUN in VolumeContext")` | zvol-backed iSCSI CreateVolume이 PV `VolumeContext`에 target IQN, portal IP, port, LUN을 기록하고 backend zvol을 준비 | StorageClass Ready; agent가 ZFS zvol 생성 및 iSCSI export 가능 | 1) PVC 생성; 2) PV/PillarVolume 생성 대기; 3) PV `spec.csi.volumeAttributes`와 PillarVolume 상태 조회 | `target_id`는 IQN 형식; `address`는 storage-worker IP; `port==3260`(또는 override 값); `volume_ref`는 LUN 문자열; backend path가 `/dev/zvol/` 아래에 존재 | `CSI-C`, `Agent`, `ZFS`, `VolCRD`, `gRPC` |
| 333 | `It("ControllerPublishVolume resolves the compute-worker initiator IQN from CSINode annotations for a zvol-backed target")` | `ControllerPublishVolume`이 `CSINode` annotation의 compute-worker initiator IQN을 사용해 zvol-backed target ACL을 추가 | PVC Bound; Pod 미생성; compute-worker node image에 initiator IQN 설정; `CSINode` annotation publish 완료 | 1) Pod 생성; 2) ControllerPublish 발생; 3) storage-worker의 LIO ACL 조회 | Pod Running; 해당 target ACL에 `CSINode` annotation과 같은 compute-worker IQN 존재 | `CSI-C`, `Agent`, `ZFS`, `gRPC` |
| 334 | `It("ControllerUnpublishVolume revokes the CSINode-derived initiator IQN ACL without deleting the zvol-backed target before PVC cleanup")` | Pod 삭제 시 동일 IQN ACL만 제거되고 export 삭제는 PVC 삭제 단계까지 보류 | 333 성공 후 Pod Running | 1) Pod 삭제; 2) ControllerUnpublish 완료 대기; 3) LIO ACL/target 조회 | target ACL에서 해당 IQN 제거; target export는 PVC 삭제 전까지 유지; 다른 volume/session 영향 없음 | `CSI-C`, `Agent`, `ZFS`, `gRPC` |

---

### E35.2 zvol-backed Filesystem PVC 및 Pod 마운트

이 섹션은 ZFS backend 고유 파라미터가 유지된 상태에서 iSCSI filesystem PVC가
실제로 Bound 되고, compute-worker에서 discovery/login/mount 후 앱이 사용할 수
있는지 검증한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 335 | `It("filesystem PVC becomes Bound via ZFS zvol + iSCSI")` | `PillarProtocol(type=iscsi)`를 참조하는 zfs-zvol StorageClass로 PVC가 정상 Bound | StorageClass Ready; zpool 여유 공간 존재 | 1) Filesystem PVC(1Gi) 생성; 2) Bound 대기 | PVC Phase=Bound; PV provisioner=`pillar-csi.bhyoo.com`; backend는 zvol | `CSI-C`, `Agent`, `ZFS`, `VolCRD` |
| 336 | `It("a Pod mounting the zvol-backed iSCSI PVC reaches Running on the compute-worker node")` | Pod 생성 시 compute-worker에서 iSCSI discovery/login 후 마운트 성공 | 335 성공; compute-worker에 open-iscsi 실행 가능 | 1) Pod 생성; 2) Running 대기; 3) `mount` / `lsblk` 확인 | Pod Running; pod 내부 mount 성공; node에서 active iSCSI session 1개 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Conn`, `Mnt` |
| 337 | `It("zfs-specific volume parameters remain effective when the protocol is iSCSI")` | `zfs-parent-dataset`, `compression` 같은 ZFS 파라미터가 iSCSI 경로에서도 유지 | Binding 또는 pool에 ZFS 파라미터 설정; `PILLAR_E2E_ZFS_PARENT_DATASET` 준비 | 1) PVC 생성; 2) Pod 생성; 3) storage-worker에서 `zfs list` / `zfs get` 확인 | zvol이 기대 parent dataset 아래 생성; 설정한 ZFS 속성 유지; Pod Running | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `VolCRD` |
| 338 | `It("deleting the Pod triggers NodeUnpublish, NodeUnstage and iSCSI logout for the zvol-backed volume")` | Pod 삭제 시 bind mount 해제, staging 해제, session logout 수행 | 336 성공 후 Pod Running | 1) Pod 삭제; 2) node mount/session 상태 확인 | target path 정리; staging path 정리; `iscsiadm -m session`에서 세션 제거 | `CSI-N`, `Conn`, `Mnt`, `State` |
| 339 | `It("deleting the PVC removes the exported target and destroys the zvol")` | PVC 삭제 시 target export와 backend zvol이 모두 정리 | 338 완료; PVC/PV 잔존 | 1) PVC 삭제; 2) PV 삭제 대기; 3) storage-worker에서 LIO target/zvol 확인 | PV 제거; target export 없음; `zfs list`에 zvol 없음 | `CSI-C`, `Agent`, `ZFS`, `VolCRD`, `gRPC` |

---

### E35.3 Raw Block, 확장, 통계 및 재스테이징

ZFS zvol을 iSCSI로 제공하는 경로에서도 raw block, online expansion,
node stats, restage idempotency까지 동일 수준으로 제공해야 한다.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 340 | `It("raw block PVC is published as an unformatted block device from a zvol-backed iSCSI LUN")` | `volumeMode: Block` PVC가 zvol-backed raw block device로 publish | StorageClass Ready; block-mode PVC 생성 | 1) Block PVC 생성; 2) raw block consumer Pod 생성; 3) pod 내부 장치 확인 | pod에 block device 노출; filesystem 생성 흔적 없음; device path 접근 가능 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Conn` |
| 341 | `It("online expansion grows the zvol, rescans the iSCSI session and expands the filesystem inside the running Pod")` | PVC 확장 시 backend zvol과 node filesystem이 모두 커짐 | Filesystem Pod Running; `allowVolumeExpansion=true` | 1) PVC 1Gi->2Gi patch; 2) Expand 완료 대기; 3) storage-worker `zfs list`; 4) pod 내부 `df` 확인 | PVC/PV capacity 증가; zvol size 증가; iSCSI session rescan 완료; pod 내부 filesystem 용량 증가 | `CSI-C`, `CSI-N`, `Agent`, `ZFS`, `Conn`, `Mnt` |
| 342 | `It("NodeGetVolumeStats reports bytes and inodes for filesystem volumes and bytes for raw block volumes on zvol-backed iSCSI volumes")` | iSCSI + zvol 경로에서도 `NodeGetVolumeStats`가 filesystem/block 모드별로 올바른 usage를 반환 | 336, 340 성공 | 1) filesystem PVC에 stats 조회; 2) raw block PVC에 stats 조회 | filesystem은 bytes+inodes; raw block은 total bytes만 보고 | `CSI-N`, `Conn`, `Mnt` |
| 343 | `It("after node plugin restart, restaging is idempotent and does not create duplicate iSCSI sessions for the same zvol-backed volume")` | node plugin 재시작 후 재스테이징이 session 중복 없이 복구 | Filesystem Pod Running; node plugin restart 가능 | 1) node plugin 재시작; 2) workload 유지/복구 대기; 3) session 수와 mount 상태 확인 | volume 재사용 성공; 동일 volume에 중복 session 없음; mount 상태 일관 | `CSI-N`, `Conn`, `Mnt`, `State` |

---

### E35 커버리지 요약

| 소섹션 | 검증 내용 | 테스트 수 | 인프라 |
|--------|---------|----------|--------|
| E35.1 | zvol+iSCSI generated StorageClass, CreateVolume `VolumeContext`, Publish/Unpublish ACL | 4개 | Kind + ZFS + LIO |
| E35.2 | Filesystem PVC 프로비저닝, Pod mount, ZFS 파라미터 유지, logout/cleanup | 5개 | Kind + ZFS + LIO + open-iscsi |
| E35.3 | Raw block, online expansion, NodeGetVolumeStats, 재스테이징 멱등성 | 4개 | Kind + ZFS + LIO + open-iscsi |
| **합계** | | **13개** | |

**CI 실행 가능 여부:**
E35는 E34보다도 호스트 요구사항이 높다. `zfs` 커널 모듈과 실제 zpool,
`/dev/zvol` 장치 생성, `target_core_mod`, `iscsi_target_mod`, `iscsi_tcp`,
`libiscsi` 커널 모듈, compute-worker 측 `open-iscsi` 런타임이 모두 필요하므로
표준 GitHub Actions에서는 실행하지 않고 ZFS/iSCSI가 보장되는 self-hosted/전용
러너에서만 실행한다.

**MVP 범위에서 제외되는 시나리오:**

- CHAP Secret 기반 인증
- multipath / multi-portal
- RWX
- snapshot/clone 전용 iSCSI UX

---

## E-NEW: PRD 갭 — 추가 TC

### E-NEW-1: init container modprobe best-effort

> **E2E test 근거:** init container의 modprobe 실패가 pod 시작을 차단하지 않는지는 실제 pod lifecycle이 필요.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E-NEW-1-1 | `TestHelm_InitContainer_ModprobeFailure_PodStarts` | init container의 modprobe가 실패해도(존재하지 않는 모듈) pod이 정상 시작 | Kind 클러스터; Helm 배포; init container에 존재하지 않는 모듈(fake_module_xyz) modprobe 설정 | 1) Helm 배포(init modprobe에 fake 모듈 포함); 2) pod 상태 확인; 3) init container 로그 확인 | Pod가 Running 상태; init container는 에러 로그 남기지만 종료 코드 0 (best-effort); main container 정상 시작 | `Agent`, `Helm` |

---

# 장애 복구 및 이상 상황 E2E (Kind 시뮬레이션)

> **빌드 태그:** `//go:build e2e` | **실행:** `go test ./test/e2e/ -tags=e2e -v -run TestE2E_Fault`
>
> Kind 클러스터 + loopback device로 장애 상황을 시뮬레이션한다.
> 베어메탈/KVM 없이도 노드 리부트, 네트워크 단절, 용량 고갈, 디바이스 장애를 재현할 수 있다.

---

### E-FAULT-1: 노드 리부트 후 Agent ReconcileState 복구

> **E2E test 근거:** Kind 노드 컨테이너를 재시작하면 configfs가 소멸한다. Controller가 ReconcileState로 상태를 재구성하는 전체 경로를 검증.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E-FAULT-1-1 | `TestE2E_NodeReboot_AgentRecovery` | 스토리지 노드 리부트 후 기존 볼륨의 NVMe-oF export가 자동 복구 | Kind 클러스터; PVC 생성 → Pod 마운트 완료; 데이터 기록 확인 | 1) `docker restart <kind-storage-node>`; 2) agent pod Ready 대기; 3) 기존 PVC의 Pod에서 데이터 읽기 | Pod가 재마운트 성공; 이전 데이터 보존; PillarTarget AgentConnected=True 복구 | `Agent`, `CSI-C`, `CSI-N`, `NVMeF` |

---

### E-FAULT-2: Agent 네트워크 단절 및 복구

> **E2E test 근거:** iptables로 agent gRPC 포트를 차단하여 네트워크 장애를 시뮬레이션.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E-FAULT-2-1 | `TestE2E_AgentNetworkPartition_CreateVolumeFails` | agent 네트워크 차단 중 CreateVolume 시도 → 적절한 에러 반환 | Kind 클러스터; PillarTarget Ready | 1) `iptables -A INPUT -p tcp --dport 9500 -j DROP` (스토리지 노드에서); 2) PVC 생성; 3) 일정 시간 대기 | PVC Pending 상태; Event에 연결 실패 메시지; PillarTarget AgentConnected=False | `CSI-C`, `Agent`, `TgtCRD` |
| E-FAULT-2-2 | `TestE2E_AgentNetworkPartition_Recovery` | 네트워크 복구 후 대기 중이던 PVC가 자동 프로비저닝 | 위 테스트 이어서 | 1) `iptables -D INPUT -p tcp --dport 9500 -j DROP`; 2) PVC Bound 대기; 3) Pod 마운트 확인 | PVC Bound; Pod Running; PillarTarget AgentConnected=True 복구 | `CSI-C`, `Agent`, `TgtCRD` |

---

### E-FAULT-3: 스토리지 풀 용량 고갈 (loopback)

> **E2E test 근거:** 작은 loopback device로 실제 용량 고갈 상황 재현.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E-FAULT-3-1 | `TestE2E_PoolExhaustion_CreateVolumeFails` | 풀 용량 부족 시 PVC가 적절한 에러와 함께 Pending | Kind; 50MB loopback VG/zpool; 이미 40MB 볼륨 존재 | 1) 20MB PVC 생성; 2) Event 확인 | PVC Pending; Event에 "insufficient capacity" 또는 "no space" 에러 | `CSI-C`, `Agent`, `LVM`/`ZFS` |

---

### E-FAULT-4: 블록 디바이스 제거 (loopback detach)

> **E2E test 근거:** loopback device 해제로 디스크 장애를 시뮬레이션.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E-FAULT-4-1 | `TestE2E_BackingDeviceRemoved_GracefulError` | 백킹 디바이스 제거 후 새 볼륨 생성 시 명확한 에러 | Kind; loopback device 기반 VG/zpool | 1) `losetup -d /dev/loopN` (백킹 디바이스 해제); 2) 새 PVC 생성 시도 | PVC Pending; Event에 backend 에러 메시지; 패닉/크래시 없음 | `CSI-C`, `Agent`, `LVM`/`ZFS` |

---

### E-FAULT-5: 다중 노드 시나리오

> **E2E test 근거:** Kind multi-node로 다중 워커 노드 환경 검증.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E-FAULT-5-1 | `TestE2E_MultiNode_VolumeAccessFromDifferentWorker` | 스토리지 노드가 아닌 다른 워커에서 볼륨 접근 | Kind 3-node (1 control-plane, 1 storage, 1 worker); PVC 생성; Pod을 worker 노드에 스케줄링 | 1) PVC 생성 (storage 노드에서 zvol/LV 생성); 2) Pod을 worker 노드에 nodeSelector로 배치; 3) Pod에서 데이터 쓰기/읽기 | Pod Running on worker; NVMe-oF/iSCSI로 스토리지 노드에 연결; 데이터 I/O 성공 | `CSI-C`, `CSI-N`, `Agent`, `NVMeF` |

---

# 수동/스테이징 테스트

> **참고:** 기존 수동 시나리오(AD, BP)는 위 Kind 시뮬레이션 TC로 자동화되었다. 수동 테스트는 자동화가 불가능한 운영 환경 검증(멀티-AZ, 물리 NIC 이중화 등)에만 유지한다.

---

## AD 시나리오: Agent Down 수동 검증

> E18 섹션에서 분리된 수동 검증 시나리오 (3개). 실제 프로세스 종료/재시작,
> 실제 커널 NVMe-oF 상태, Kubernetes DaemonSet 복구 동작을 검증한다.

**자동화 불가 이유:**
- 실제 프로세스 종료/재시작은 `os.Exit()` 또는 시그널 전송이 필요하며, 인프로세스 테스트에서는 테스트 프로세스 자체가 종료된다.
- 실제 커널 NVMe-oF 상태(`/sys/kernel/config/nvmet`)는 루트 권한 + `nvmet` 커널 모듈이 필요하다.
- Kubernetes DaemonSet 재시작 동작은 실제 API 서버와 kubelet이 필요하다.

| ID | 시나리오 | 사전 조건 | 수동 실행 절차 | 허용 기준 | 커버리지 |
|----|---------|----------|--------------|---------|---------|
| AD-1 | **에이전트 프로세스 강제 종료 후 재시작 — NVMe-oF 수출 지속성 검증** | 실제 NVMe-oF 수출 설정이 완료된 스토리지 노드; 실제 `/sys/kernel/config` 마운트; 진행 중인 클라이언트 NVMe 연결 | 1) `kill -9 <agent-pid>` 실행; 2) `/sys/kernel/config/nvmet/subsystems/` 아래 엔트리 유지 확인(`ls`); 3) 클라이언트 노드에서 NVMe 연결 지속성 확인(`nvme list`); 4) 에이전트 재시작(`systemctl start pillar-agent`); 5) CSI 컨트롤러의 `ReconcileState` 호출 확인(에이전트 로그); 6) 볼륨 I/O 정상 확인 | 에이전트 종료 중에도 커널 NVMe-oF 상태 유지; 재시작 후 `ReconcileState`로 상태 동기화 완료; 클라이언트 I/O 무중단(또는 짧은 중단 후 자동 재연결) | `Agent`, `NVMeF`, `실제 커널` |
| AD-2 | **CSI 컨트롤러가 에이전트 다운 감지 후 PillarTarget 상태 갱신** | 실제 Kubernetes 클러스터; cert-manager; mTLS 설정 완료; `PillarTarget` CRD 존재; 에이전트 정상 실행 중 | 1) `kubectl get pillartarget -o yaml`로 초기 `AgentConnected=True` 확인; 2) 에이전트 중지(`systemctl stop` 또는 `kill -STOP`); 3) CSI 컨트롤러 `HealthCheck` 폴링 주기(~30초) 대기; 4) `kubectl get pillartarget -o yaml` 재확인; 5) 에이전트 재시작 후 상태 복원 확인 | 에이전트 중지 후: `PillarTarget.Status.Conditions[AgentConnected].Status=False`, `Reason=HealthCheckFailed` 또는 `ConnectionLost`; 에이전트 재시작 후: `AgentConnected=True` 복원 | `mTLS`, `TgtCRD`, `Agent` |
| AD-3 | **에이전트 OOM Kill 후 Kubernetes 자동 복구** | Kubernetes 스토리지 노드에 배포된 에이전트 DaemonSet; `restartPolicy: Always`; 진행 중인 PVC 사용 파드 | 1) `kubectl exec -n pillar-csi <agent-pod> -- kill -9 1`(PID 1 강제 종료); 2) `kubectl get pod -n pillar-csi -w`로 재시작 관찰; 3) 재시작 후 `kubectl describe pod`에서 Restart Count 확인; 4) 기존 PVC를 사용하는 파드의 I/O 정상 확인 | Kubernetes가 에이전트 파드를 자동 재시작; 재시작 후 `ReconcileState`로 상태 복원; 기존 PVC 마운트는 커널 NVMe 레이어에서 지속됨(I/O 무중단 또는 짧은 중단 후 자동 복구) | `Agent`, `NVMeF`, `실제 커널`, `Kubernetes클러스터` |

---

## BP 시나리오: 비호환 백엔드-프로토콜 수동 검증

> E22.4 섹션에서 분리된 수동 검증 시나리오 (4개). 실제 버전 불일치, 커널 모듈
> 미로드 상태에서의 동작을 검증한다.

**자동화 불가 이유:**
- `GetCapabilitiesResponse.agent_version` 필드는 실제 에이전트 바이너리에서만 의미 있는 값을 반환한다.
- 커널 모듈(`nvmet`, `nvme-fabrics`) 로드/언로드는 root 권한과 실제 커널이 필요하다.
- 컨트롤러-에이전트 간 실제 gRPC 연결과 프로토콜 협상은 실제 네트워크와 mTLS 인증이 필요하다.
- `GetCapabilitiesResponse.supported_protocols` 필드를 기반으로 한 프로토콜 사전 검증 로직이
  현재 CSI 컨트롤러에 **구현되어 있지 않다** — 버전 협상은 향후 구현 예정이다.

| ID | 시나리오 | 사전 조건 | 수동 실행 절차 | 허용 기준 | 커버리지 |
|----|---------|----------|--------------|---------|---------|
| BP-1 | **Controller-Agent 에이전트 버전 확인 — `GetCapabilitiesResponse.agent_version` 필드 기록 여부** | 실제 Kubernetes 클러스터; pillar-csi-controller 배포; pillar-agent 배포 (`agent_version="0.1.0"` 내장, `internal/agent/server.go:36` 상수) | 1) PillarTarget CRD 등록 후 컨트롤러 재조정 대기; 2) `kubectl get pillartarget <name> -o yaml`로 `status.agentVersion` 또는 관련 조건 메시지 확인; 3) 에이전트 바이너리를 이전 버전으로 교체 후 컨트롤러 반응 확인 | `PillarTarget.status` 또는 이벤트에 에이전트 버전 정보 기록됨; 버전 불일치 경고는 현재 미구현(향후 구현 예정); 버전 불일치 시에도 볼륨 생성 시도 가능 — 미지원 RPC 호출 시 `Unimplemented` 반환으로 오류 감지 | `Agent`, `TgtCRD`, `gRPC` |
| BP-2 | **스토리지 노드에서 nvmet 커널 모듈 미로드 — HealthCheck 경고 및 ExportVolume 실패** | 실제 스토리지 노드; ZFS 커널 모듈 로드됨; nvmet/nvme-fabrics 모듈 **미로드** (`modprobe -r nvmet nvme-fabrics`) | 1) pillar-agent 프로세스 시작; 2) `agent.HealthCheck()` 응답의 `subsystems` 배열 확인 — `nvmet-configfs` 서브시스템 `healthy` 필드 값 확인; 3) PVC 생성 시도(CSI CreateVolume -> `agent.CreateVolume` 성공 -> `agent.ExportVolume` 실패 예상); 4) `kubectl describe pvc`에서 오류 이벤트 확인 | `HealthCheck` 응답에 `nvmet-configfs.healthy=false` 표시; `ExportVolume` 호출 시 configfs 디렉터리 생성 실패로 `codes.Internal` 또는 `codes.FailedPrecondition` 반환; PVC가 `Pending` 상태 유지; 오류 메시지에 configfs 관련 진단 정보 포함 | `Agent`, `NVMeF`, `TgtCRD` |
| BP-3 | **프로토콜 협상 실패 엔드투엔드 — StorageClass `protocol-type: iscsi`로 PVC 생성 시 오류 전파** | 실제 Kubernetes 클러스터; StorageClass `protocol-type: iscsi`로 구성; 실제 pillar-agent 배포 (NVMe-oF TCP 전용) | 1) `kubectl apply -f storageclass-iscsi.yaml`; 2) `kubectl apply -f pvc-iscsi.yaml`; 3) PVC 이벤트 확인 (`kubectl describe pvc <name>`); 4) CSI 컨트롤러 로그에서 `Unimplemented` 오류 확인 | PVC가 `Pending` 상태 유지; CSI CreateVolume 오류 이벤트에 `Unimplemented: only NVMe-oF TCP is supported` 메시지; 지속적인 재시도 없이 명확한 오류 보고; PillarVolume CRD 미생성 | `CSI-C`, `Agent`, `gRPC`, `실제 Kubernetes클러스터` |
| BP-4 | **향후 iSCSI 지원 추가 시 회귀 검증 체크리스트** | iSCSI 지원 버전의 pillar-agent 배포 후; LIO 커널 모듈 로드됨 (`iscsi_target_mod`, `target_core_mod`, `configfs`) | 1) StorageClass에 `protocol-type: "iscsi"` 설정; 2) PVC 생성; 3) `kubectl describe pvc`로 `Bound` 확인; 4) 스토리지 노드에서 `targetcli ls` 실행하여 iSCSI 타깃 생성 확인 | PVC `Bound` 상태; iSCSI LIO 타깃 생성 확인; **현재 E22.1 테스트(171-172)가 `Unimplemented` 예상에서 `OK` 예상으로 갱신 필요**; `TestAgentErrors_*_InvalidProtocol` 시리즈 삭제 또는 프로토콜 목록 업데이트 필요 | `Agent`, `CSI-C`, `실제 커널`, `실제 Kubernetes클러스터` |

---

## 유형 M: 수동/스테이징 테스트

> **총 유형 M 테스트 케이스: 42개** (M1-M10 그룹, 각 그룹 3-7개 시나리오)
>
> 자동화된 CI 파이프라인으로 실행할 수 없거나, 현실적으로 자동화가
> 불가능한 테스트의 권위 있는 명세이다.

**유형 M 테스트의 공통 특성:**

| 특성 | 설명 |
|------|------|
| 비결정적(Non-deterministic) 타이밍 | 실제 하드웨어 장애, 네트워크 지연, 인증서 TTL 등 실제 시간에 의존 |
| 인간 판단 필요 | 테스트 결과가 "통과/실패" 이분법으로 표현되지 않고 운영자 판단이 필요 |
| 환경 파괴적(Destructive) | 테스트 실행 중 실제 데이터 손실, 서비스 중단, 하드웨어 조작이 발생 |
| 재현 비용 과다 | 스테이징 클러스터 구축, 물리 서버 확보, 라이선스 비용 등 |
| 프로덕션 의존성 | 실제 워크로드 패턴, 실제 사용자 데이터, 실제 운영 환경 필요 |

### 유형 M 테스트 요약

| 그룹 | ID | 테스트 이름 | 자동화 불가 이유 | 스테이징 환경 |
|------|-----|-----------|----------------|--------------|
| M1 | M1.1-M1.4 | 롤링 업그레이드 검증 | 서비스 중단 관찰에 인간 판단 필요 | 멀티-노드 Kubernetes 클러스터 |
| M2 | M2.1-M2.5 | 스토리지 네트워크 분리(Network Partition) | 비결정적 타이밍; iptables 규칙 복잡 | 멀티-노드 + 별도 스토리지 네트워크 |
| M3 | M3.1-M3.4 | 물리적 디스크/하드웨어 장애 | 실제 물리 장비 조작 필요 | 베어메탈 서버 + 교체용 디스크 |
| M4 | M4.1-M4.5 | 커널 버전 호환성 매트릭스 | 다수의 커널 버전 환경 준비 비용 | 다중 OS 이미지 + 커널 변형 |
| M5 | M5.1-M5.5 | 프로덕션 유사 부하 및 용량 계획 | 실제 워크로드 해석에 전문가 판단 필요 | 대규모 스테이징 클러스터 |
| M6 | M6.1-M6.4 | 보안 감사 및 침투 테스트 | 결과 해석 및 위험 판단에 인간 개입 필수 | 격리된 보안 테스트 환경 |
| M7 | M7.1-M7.5 | 데이터 무결성 심층 검증 | 실제 데이터 검증 도구 + 긴 실행 시간 | 실제 데이터 워크로드 환경 |
| M8 | M8.1-M8.4 | CSI 드라이버 업그레이드 절차 검증 | 업그레이드 중 실시간 모니터링 필요 | 프로덕션 유사 Kubernetes 클러스터 |
| M9 | M9.1-M9.4 | 다중 테넌트 격리 검증 | 실제 테넌트 자격 증명 및 워크로드 필요 | 멀티-테넌트 클러스터 |
| M10 | M10.1-M10.7 | 인증서 수명 주기 및 실제 PKI 갱신 | 실제 인증서 TTL 대기 (최소 수 시간) | cert-manager + 실제 CA |

### 스테이징 클러스터 최소 사양

| 구성 요소 | 최소 사양 | 권장 사양 | 비고 |
|-----------|----------|----------|------|
| 컨트롤 플레인 노드 | 1개 (4코어, 8 GiB) | 3개 HA (4코어, 16 GiB) | Kubernetes v1.29+ |
| 스토리지 노드 | 2개 (4코어, 16 GiB, 100 GiB 디스크) | 4개 | pillar-agent DaemonSet 실행 |
| 워크로드 노드 | 2개 (4코어, 8 GiB) | 4개 | 실제 PVC 소비 워크로드 |
| 스토리지 네트워크 | 별도 L2 VLAN 또는 인터페이스 | 10 GbE 전용 NIC | NVMe-oF TCP 트래픽 분리 |
| OS | Ubuntu 22.04 LTS (베어메탈 또는 KVM) | Ubuntu 22.04/24.04 혼합 | ZFS/nvme 커널 모듈 지원 |
| 커널 | 5.15+ | 6.1 LTS | `nvme-tcp`, `nvmet`, `nvmet-tcp` 포함 |
| ZFS 풀 | 각 노드 100 GiB (루프백 가능) | 실제 NVMe/SSD | M3, M7용 실제 디스크 권장 |

---

### M1: 롤링 업그레이드 검증

**목적:** pillar-csi 컨트롤러 및 pillar-agent DaemonSet의 롤링 업그레이드 중
실행 중인 PVC/Pod 워크로드의 I/O가 중단 없이 유지되는지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M1.1 | **에이전트 롤링 업그레이드 — I/O 유지** | `fio --rw=randwrite` 실행 중 PVC 최소 4개; pillar-agent v_old DaemonSet 배포 완료 | 에이전트 업그레이드(`v_new`) 진행 중 I/O 오류율, 레이턴시 급등, PVC Read-Only 전환 여부 | I/O 오류 0%; 레이턴시 급등 최대 2배 이내; Pod 재시작 없음 | 1) `kubectl rollout restart daemonset/pillar-agent`; 2) `fio` 로그에서 `error` 라인 확인; 3) `kubectl get events --field-selector=reason=Failed` 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |
| M1.2 | **컨트롤러 롤링 업그레이드** | pillar-csi controller-manager deployment; 능동적 PVC 프로비저닝 요청 중 | 업그레이드 중 신규 PVC 프로비저닝 지연, 기존 PVC 액세스 중단, controller-manager 리더 선출 오류 | PVC 프로비저닝 지연 <= 60s; 기존 PVC I/O 무중단 | 1) `kubectl set image deployment/pillar-csi-controller-manager manager=example.com/pillar-csi:v_new`; 2) 신규 PVC 생성 스크립트 실행 유지; 3) `kubectl rollout status` 완료 후 미처리 PVC 수 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |
| M1.3 | **구버전 에이전트 + 신버전 컨트롤러 공존(혼합 버전)** | 스토리지 노드 A: v_old 에이전트, 스토리지 노드 B: v_new 에이전트; 컨트롤러: v_new | 혼합 버전 환경에서 볼륨 생성/삭제 정상 동작; API 프로토콜 하위 호환성 | 모든 볼륨 오퍼레이션 성공; gRPC 직렬화 오류 없음 | 1) 노드 A에 `v_old`, 노드 B에 `v_new` 에이전트 배포; 2) 두 노드에서 번갈아 볼륨 생성/삭제; 3) `kubectl logs`에서 `Unimplemented`/`Unknown field` 오류 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |
| M1.4 | **롤백 시나리오 — 업그레이드 실패 후 이전 버전 복구** | v_new 에이전트에 의도적인 결함; 에이전트 CrashLoopBackOff 상태 | 롤백(`kubectl rollout undo`) 후 기존 PVC 모두 접근 가능 상태 복구; 데이터 손실 없음 | 롤백 완료 후 모든 PVC Bound; I/O 재개 | 1) 결함 있는 버전 배포; 2) CrashLoopBackOff 확인; 3) `kubectl rollout undo daemonset/pillar-agent`; 4) `fio` I/O 재개 확인 | `Agent`, `CSI-C`, `ZFS`, `NVMeF`, `gRPC` |

---

### M2: 스토리지 네트워크 분리

**목적:** 스토리지 네트워크 장애 시 pillar-csi가 올바른 오류를 반환하고,
네트워크 복구 후 I/O가 자동으로 재개되는지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M2.1 | **스토리지 네트워크 완전 차단 -> 복구** | `fio` I/O 중 PVC 2개; 스토리지 노드 A의 스토리지 NIC에 `iptables` 차단 | I/O 오류 반환 타이밍; Pod Eviction 여부; 복구 후 I/O 재개 시간 | 30s 내 I/O 오류 반환; iptables 규칙 제거 후 120s 내 I/O 재개 | 1) `fio` 백그라운드 실행; 2) `iptables` 차단 규칙 적용; 3) 오류 발생 시간 기록; 4) 규칙 제거; 5) I/O 재개까지 경과 시간 측정 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.2 | **패킷 손실 20% 주입** | `tc qdisc add dev <storage-nic> root netem loss 20%` | NVMe-oF 재전송 레이턴시 증가; I/O 오류 여부 | 레이턴시 <= 10배 증가; I/O 오류 없음 | 1) `tc netem loss 20%` 적용; 2) `fio` 레이턴시 수집; 3) 규칙 제거 후 정상화 확인 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.3 | **스토리지 노드 재부팅 중 I/O** | `fio` 연속 I/O 중 스토리지 노드 graceful reboot | 부팅 완료 후 PVC Bound 복구; I/O 재개; 데이터 무결성 | 부팅 완료 후 120s 내 PVC 복구; `fio` MD5 checksum 일치 | 1) `fio --verify=md5` 실행; 2) 스토리지 노드 재부팅; 3) 부팅 완료 후 상태 확인; 4) `fio verify` 결과 검토 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.4 | **스토리지 네트워크 링크 플랩(Link Flap) — 10초 간격 반복** | 스토리지 NIC `ip link set down` -> 5초 후 `ip link set up` x 5회 | PVC 강제 Terminating 전환 없음; I/O 자동 재개 | PVC Terminating 전환 0; 링크 복구 후 I/O 재개 <= 30s | 1) 링크 플랩 스크립트 실행; 2) `fio` 로그에서 오류 구간 기록; 3) dmesg에서 `nvme` 관련 경고 확인 | `Conn`, `NVMeF`, `Agent`, `gRPC` |
| M2.5 | **컨트롤 플레인 <-> 스토리지 노드 통신 차단 (NVMe-oF는 유지)** | iptables로 컨트롤 플레인 -> 스토리지 노드 API 차단; NVMe-oF 포트 유지 | 기존 PVC I/O 유지; 신규 프로비저닝 실패 | 기존 I/O 100% 유지; 신규 프로비저닝에 명확한 오류 반환 | 1) gRPC 포트 차단; 2) `fio` I/O 지속 확인; 3) 신규 PVC 생성 시도; 4) 차단 해제 후 보류 PVC 자동 프로비저닝 확인 | `Conn`, `NVMeF`, `Agent`, `gRPC` |

---

### M3: 물리적 디스크/하드웨어 장애

**목적:** ZFS 풀을 구성하는 물리 디스크의 장애 시 자동 복구되는지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M3.1 | **RAIDZ1 구성 중 디스크 1개 오프라인** | ZFS RAIDZ1 풀(디스크 3개); `fio` I/O 활성; PVC 2개 | 디스크 오프라인 후 ZFS `degraded` 상태; I/O 유지 | ZFS `degraded` 반환; I/O 오류 없음; PVC `Bound` 유지 | 1) `zpool offline tank sdb`; 2) `fio` I/O 확인; 3) `zpool status` 확인; 4) PVC 상태 확인 | `ZFS`, `NVMeF`, `Agent` |
| M3.2 | **디스크 교체 및 재실버링(Resilver) 중 I/O 유지** | M3.1 이후 상태; 교체용 디스크 준비 | `zpool replace` 후 재실버링 중 I/O 유지 | 재실버링 중 I/O 오류 없음; 완료 후 `ONLINE` 상태 | 1) `zpool replace tank sdb sdc`; 2) 진행 중 `zpool status` 확인; 3) `fio` I/O 유지 확인 | `ZFS`, `NVMeF`, `Agent` |
| M3.3 | **불량 블록(Bad Block) 시뮬레이션 — ZFS 스크럽** | ZFS 풀에 데이터 기록 후 `dd`로 데이터 손상 주입 | `zpool scrub` 후 오류 감지; 오류 카운터 증가 | `zpool scrub` 후 오류 카운터 > 0; pillar-agent가 오류 상태 표시 | 1) 데이터 기록; 2) `dd`로 섹터 손상; 3) `zpool scrub`; 4) CKSUM 오류 확인 | `ZFS`, `NVMeF`, `Agent` |
| M3.4 | **스토리지 노드 전원 강제 차단(Power Loss) 시뮬레이션** | `fio --rw=write` 활성 중; KVM 환경에서 `virsh destroy` | 전원 복구 후 ZFS 자동 무결성 검사; PVC 재마운트 | ZFS import 후 `ONLINE`; 데이터 무결성; PVC `Bound` 복구 | 1) `virsh destroy`; 2) 5초 후 `virsh start`; 3) `zpool import -f`; 4) 데이터 검증 | `ZFS`, `NVMeF`, `Agent` |

---

### M4: 커널 버전 호환성 매트릭스

**목적:** 지원하는 Linux 커널 버전 범위에서 ZFS, NVMe-oF 커널 모듈이 올바르게 동작하는지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M4.1 | **커널 5.15 LTS (Ubuntu 22.04 기본)** | Ubuntu 22.04 LTS; `uname -r` = 5.15.x | 기본 볼륨 생성/삭제/마운트/언마운트 전체 흐름 | 모든 F1-F10 수준 테스트 통과 | 1) `modprobe zfs nvmet nvme-tcp`; 2) pillar-agent 실행; 3) 기본 볼륨 라이프사이클 수동 실행; 4) `dmesg` 경고 없음 확인 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.2 | **커널 6.1 LTS** | Ubuntu 22.04 + HWE 커널 또는 Debian 12 | 동일 (M4.1 반복) | M4.1과 동일 | M4.1과 동일 절차; `dmesg` 경고 비교 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.3 | **커널 6.8 (최신 안정 버전)** | Ubuntu 24.04 LTS | 동일 + nvmet configfs API 변경 여부 | M4.1과 동일; configfs 경로 변경 없음 | M4.1과 동일; `ls /sys/kernel/config/nvmet/` 구조 확인 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.4 | **ZFS 커널 모듈 버전 조합 검증** | Ubuntu 22.04; `zfs-dkms` 2.1.x vs. 2.2.x | 서로 다른 ZFS 버전에서 zvol 생성/삭제 정상 동작 | zvol 오퍼레이션 모두 성공; 비호환 경고 없음 | 1) `apt install zfs-dkms=2.1.x`; 2) F1 수준 테스트; 3) `apt upgrade zfs-dkms`; 4) 동일 테스트 재실행 | `ZFS`, `NVMeF`, `Conn`, `Agent` |
| M4.5 | **RHEL 9 / Rocky Linux 9 호환성** | RHEL 9 또는 Rocky Linux 9.x; ZFS on RHEL (DKMS) | Ubuntu 이외 배포판에서 pillar-agent 기본 기능 동작 여부 | zvol 생성/삭제 성공; configfs 구조 동일 | 1) RHEL 9 환경 구성; 2) ZFS DKMS 설치; 3) 기본 볼륨 라이프사이클 확인 | `ZFS`, `NVMeF`, `Conn`, `Agent` |

---

### M5: 프로덕션 유사 부하 및 용량 계획

**목적:** 실제 운영 환경과 유사한 워크로드 하에서 처리량, 레이턴시, 리소스 소비를 측정한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M5.1 | **100개 PVC 동시 프로비저닝 성능** | 스테이징 클러스터; 스토리지 노드 4개; ZFS 풀 각 1 TiB | 100개 PVC 생성 완료 시간; 컨트롤러 CPU/메모리 | 100개 PVC Bound <= 5분; 컨트롤러 CPU <= 1코어 | 1) 100개 PVC 동시 생성; 2) Bound 대기 시간 측정; 3) Prometheus 메트릭 수집 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.2 | **지속 I/O 부하 — 4시간 Soak 테스트** | 20개 PVC; 각 PVC에 `fio --rw=randrw --runtime=14400` | 4시간 동안 I/O 오류 없음; 메모리 누수 없음 | I/O 오류 0; 메모리 증가 <= 50 MiB/4h; FD 증가 없음 | 1) 20개 PVC 생성; 2) `fio` 4시간 실행; 3) 1시간마다 리소스 기록 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.3 | **500 GiB 대용량 볼륨 생성/삭제 사이클** | ZFS 풀 1 TiB 이상; PVC 용량 500 GiB | 생성/삭제 시간; 용량 즉시 반환 | 생성 <= 30s; 삭제 <= 60s | 1) 500 GiB PVC 생성 시간 기록; 2) 삭제 시간 기록; 3) 용량 반환 확인 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.4 | **볼륨 확장 중 I/O 유지 — 대용량 파일시스템** | `ext4` 200 GiB PVC; DB 유사 I/O 패턴 실행 중 | `resize2fs` 중 I/O 오류 없음 | I/O 오류 없음; `df -h` 크기 일치 <= 1 GiB 오차 | 1) 200 GiB PVC 생성; 2) `fio` I/O 실행; 3) PVC 확장; 4) `df -h` 검증 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |
| M5.5 | **다수 스토리지 노드 동시 에이전트 gRPC 호출 스트레스** | 스토리지 노드 4개; 각 노드 10개 볼륨 동시 생성 | 에이전트 gRPC 큐 처리; 중복 CRD 없음 | 40개 볼륨 모두 성공; 중복 PillarVolume CRD 없음; 데드락 없음 | 1) 4x10 PVC 동시 생성; 2) 40개 PillarVolume 확인; 3) 에이전트 로그에서 타임아웃 경고 확인 | `CSI-C`, `Agent`, `ZFS`, `NVMeF`, `Conn`, `Mnt`, `gRPC` |

---

### M6: 보안 감사 및 침투 테스트

**목적:** mTLS 통신, RBAC 권한, 컨테이너 보안 설정을 공격자 관점에서 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M6.1 | **mTLS 클라이언트 인증서 미제시 -> 연결 거부** | 스테이징 클러스터; cert-manager CA | CA 서명되지 않은 인증서로 gRPC 연결 시도 | TLS handshake 실패; 연결 거부 | 1) `grpcurl -insecure`; 2) 자체 서명 cert으로 연결; 3) 양쪽 `TLS handshake failed` 확인 | `mTLS`, `Agent`, `gRPC` |
| M6.2 | **RBAC 최소 권한 검증** | 스테이징 클러스터; RBAC 설정 배포 | `kubectl auth can-i`로 불필요한 권한 없음 확인 | `kube-bench` 경고 없음; SA에 불필요한 권한 없음 | 1) `kubectl auth can-i --list --as=system:serviceaccount:pillar-csi-system:...`; 2) 불필요 권한 부재 확인 | `mTLS`, `Agent`, `gRPC` |
| M6.3 | **컨테이너 보안 컨텍스트 검증** | controller-manager/agent Pod 실행 중 | `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation=false` 확인 | 모든 설정 올바름; `trivy` HIGH/CRITICAL 0개 | 1) securityContext 조회; 2) `trivy image` 스캔; 3) 리포트 검토 | `mTLS`, `Agent`, `gRPC` |
| M6.4 | **NetworkPolicy 우회 시도** | Calico/Cilium NetworkPolicy 배포; 네임스페이스 격리 정책 | 다른 네임스페이스에서 gRPC 포트 접근 차단 확인 | 외부에서 50051 포트 접근 거부 | 1) 별도 네임스페이스에서 `nc -z`; 2) NetworkPolicy 전후 비교; 3) 감사 로그 확인 | `mTLS`, `Agent`, `gRPC` |

---

### M7: 데이터 무결성 심층 검증

**목적:** 다양한 조건 하에서 데이터가 손상 없이 보존되는지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M7.1 | **10 GiB 랜덤 데이터 기록 후 재마운트 검증** | ext4 PVC 20 GiB; `fio --verify=md5 --size=10g` | 언마운트 -> 재마운트 후 MD5 일치 | MD5 불일치 0건 | 1) `fio` write+verify; 2) Pod 재시작; 3) `fio --verify-only` | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.2 | **볼륨 확장 전후 데이터 무결성** | ext4 PVC 10 GiB; 5 GiB 데이터 기록 후 20 GiB로 확장 | `fsck.ext4`, MD5 체크섬 일치 | `fsck` 오류 0; MD5 일치 | 1) 데이터 기록+체크섬; 2) PVC 확장; 3) `fsck.ext4 -n`; 4) 체크섬 재검증 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.3 | **ZFS 스냅샷 -> 복원 후 데이터 일치** | ZFS 기반 PVC; 알려진 데이터셋 기록 | 스냅샷 복원 후 원본 데이터 복구 | 복원 후 원본 데이터 100% 일치 | 1) 데이터 기록+체크섬; 2) `zfs snapshot`; 3) 데이터 수정; 4) `zfs rollback`; 5) 체크섬 재검증 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.4 | **크로스-노드 볼륨 마이그레이션 데이터 무결성** | 2개 스토리지 노드; ZFS send/receive | 노드 A -> B 마이그레이션 후 체크섬 일치 | 체크섬 일치; NQN 정상 갱신 | 1) 노드 A에서 PVC+데이터; 2) `zfs send/receive`; 3) 마이그레이션 API; 4) 노드 B에서 체크섬 검증 | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |
| M7.5 | **XFS 파일시스템 무결성 — I/O 중 에이전트 재시작** | xfs PVC; `fio --rw=randwrite` 중 `kill -9` | 에이전트 재시작 후 `xfs_repair -n` 경고 없음 | `xfs_repair -n` 수정 필요 항목 없음; I/O 재개 | 1) `fio` 실행; 2) `kill -9`; 3) 재시작 대기; 4) `xfs_repair -n` | `ZFS`, `NVMeF`, `Conn`, `Mnt`, `Agent` |

---

### M8: CSI 드라이버 업그레이드 절차 검증

**목적:** 마이너/패치 버전 업그레이드 전후 PVC 접근성이 유지되는지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M8.1 | **Helm 차트 마이너 업그레이드 (v0.x -> v0.x+1)** | v_old Helm 릴리스; 기존 PVC 4개; `fio` I/O 활성 | 업그레이드 중 PVC I/O 중단 시간; CRD 데이터 보존 | I/O 중단 <= 30s; 신규 PVC 프로비저닝 <= 60s; CRD 데이터 손실 없음 | 1) `helm upgrade`; 2) `fio` I/O 로그 기록; 3) PV/PVC 상태 검증 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |
| M8.2 | **CRD 스키마 변경이 포함된 업그레이드** | v_old CRD에 새 선택적 필드 추가된 v_new | 기존 CRD 인스턴스에 새 필드 기본값 적용 여부 | 기존 인스턴스 유지; 새 필드 기본값 올바름 | 1) v_new CRD 적용; 2) 기존 인스턴스 확인; 3) 새 필드 기본값 확인 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |
| M8.3 | **다운그레이드 절차 — 긴급 롤백** | v_new 배포 상태에서 문제 발견 후 v_old로 롤백 | `helm rollback` 후 PVC I/O 복구 | PVC I/O 복구 <= 60s; CRD 호환성 오류 없음 | 1) `helm rollback`; 2) `kubectl rollout status`; 3) I/O 재개 확인 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |
| M8.4 | **업그레이드 후 전체 E2E 회귀 테스트 (Smoke Test)** | M8.1 완료 후 | 기본 CSI 오퍼레이션 모두 성공 | 모든 기본 오퍼레이션 성공; 에러 없음 | 1) 임시 PVC 생성/삭제 수동 실행; 2) 전체 라이프사이클 확인; 3) 이벤트에 Warning 없음 확인 | `CSI-C`, `CSI-N`, `Agent`, `gRPC` |

---

### M9: 다중 테넌트 격리 검증

**목적:** 서로 다른 네임스페이스/ServiceAccount의 테넌트가 각자의 PVC만 접근 가능한지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M9.1 | **테넌트 A의 PVC에 테넌트 B 접근 불가 (RBAC)** | 네임스페이스 `tenant-a`, `tenant-b`; RBAC 격리 정책 | `tenant-b` SA 토큰으로 `tenant-a` PVC 접근 시도 거부 | 403 Forbidden 반환 | 1) `kubectl auth can-i get pvc -n tenant-a --as=system:serviceaccount:tenant-b:default`; 2) `No` 확인 | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |
| M9.2 | **NVMe-oF NQN 수준 호스트 격리** | 볼륨 A: AllowInitiator NQN = `nqn.node-a`; 볼륨 B: NQN = `nqn.node-b` | node-b NQN으로 볼륨 A에 `nvme connect` 시도 거부 | `nvme connect` 실패; `dmesg` 거부 로그 | 1) node-b에서 `nvme connect`; 2) 실패 확인; 3) dmesg 거부 로그 확인 | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |
| M9.3 | **StorageClass 테넌트 격리** | 각 테넌트별 StorageClass; RBAC로 타 네임스페이스 StorageClass 차단 | 테넌트 A가 테넌트 B의 StorageClass 사용 거부 | PVC Pending 또는 403 오류 | 1) 잘못된 StorageClass로 PVC 생성 시도; 2) PVC 상태 확인 | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |
| M9.4 | **PillarTarget 접근 제어** | PillarTarget A: 테넌트 A 전용; RBAC로 타 테넌트 참조 불가 | 테넌트 B가 테넌트 A의 PillarTarget 참조 불가 | PVC 생성 실패; `NotFound` 또는 `Forbidden` | 1) 테넌트 B로 테넌트 A의 Target 참조 PVC 생성 시도; 2) 오류 메시지 확인 | `CSI-C`, `TgtCRD`, `VolCRD`, `gRPC` |

---

### M10: 인증서 수명 주기 및 실제 PKI 갱신

**목적:** mTLS 인증서가 실제 만료 주기에 따라 갱신되고, 갱신 중 gRPC 연결이
자동으로 재연결되는지 검증한다.

| ID | 시나리오 | 사전 조건 | 검증 항목 | 허용 기준 | 수동 실행 절차 | 커버리지 |
|----|--------|---------|---------|---------|-------------|---------|
| M10.1 | **단기 TTL 인증서(24시간) 갱신 사이클** | cert-manager; `spec.duration: 24h`, `spec.renewBefore: 8h` | 갱신 시 gRPC 세션 유지; 새 cert으로 핸드셰이크 성공 | gRPC 세션 중단 없음; 갱신 후 새 인증서 적용 | 1) 24h TTL Certificate 배포; 2) 갱신 이벤트 관찰; 3) gRPC keepalive 유지 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.2 | **인증서 수동 교체(Manual Rotation) — 다운타임 없음** | 기존 mTLS 인증서 활성 중 | 수동 교체 중 gRPC 연결 유지; 재로드 후 새 인증서로 성공 | 교체 중 gRPC 유지; 재로드 후 새 cert 연결 성공 | 1) `kubectl delete secret`; 2) cert-manager 재발급 확인; 3) 재시작 후 연결 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.3 | **루트 CA 교체(CA Rotation) — 완전한 PKI 재발급** | 기존 CA + 인증서 체인; 새 CA 준비 | 각 단계에서 gRPC 연결 유지; CA 제거 후 구 cert 거부 | 각 단계 gRPC 유지; 구 CA 제거 후 구 cert 거부 | 1) 새 CA 추가; 2) 모든 Certificate 갱신; 3) 각 단계 gRPC 테스트; 4) 구 CA 제거 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.4 | **인증서 만료(Expiry) — 갱신 실패 시 동작** | cert-manager `ClusterIssuer` 비활성화; TTL 임박 인증서 | 만료 후 gRPC 거부; Issuer 복구 후 자동 재연결 | 만료 후 gRPC `UNAVAILABLE`; 복구 후 120s 내 재연결 | 1) Issuer 비활성화; 2) 만료 대기; 3) gRPC 오류 확인; 4) Issuer 복구; 5) 재연결 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.5 | **SAN(Subject Alternative Name) 불일치 — 연결 거부** | 잘못된 SAN 인증서(에이전트 주소 불일치) | TLS SAN 검증 실패로 gRPC 거부 | `x509: certificate is valid for X, not Y` 오류 | 1) 잘못된 SAN cert 생성; 2) 적용; 3) 연결 시도 -> 오류 확인 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.6 | **Webhook TLS 인증서 갱신 중 CRD 어드미션 가용성** | cert-manager가 webhook-server-cert 갱신 중 | 갱신 중 `kubectl apply` timeout/error 여부 | 갱신 중 임시 오류 허용 (<= 30s); 완료 후 정상 | 1) webhook-server-cert 삭제(재발급 유도); 2) `kubectl apply` 반복; 3) 오류 구간 시간 기록 | `mTLS`, `gRPC`, `TgtCRD` |
| M10.7 | **cert-manager 완전 장애 — 기존 인증서 활용 유지** | cert-manager 네임스페이스 전체 삭제; 기존 Secret 보존 | cert-manager 없이 기존 cert으로 gRPC 유지 가능 기간 | cert-manager 장애 중 기존 cert 유효 기간 동안 gRPC 유지; 복구 후 갱신 재개 | 1) `kubectl delete namespace cert-manager`; 2) gRPC 유지 확인; 3) cert-manager 재설치; 4) 자동 갱신 재개 확인 | `mTLS`, `gRPC`, `TgtCRD` |

---
