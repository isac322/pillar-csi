# pillar-csi: Product Requirements Document

## 1. Overview

pillar-csi는 self-hosted Kubernetes 클러스터를 위한 Go 기반 CSI(Container Storage Interface) 드라이버이다. 스토리지 노드에 존재하는 다양한 종류의 로컬 스토리지를 네트워크 프로토콜을 통해 클러스터의 다른 노드에서 사용할 수 있도록 한다.

### 핵심 컨셉

pillar-csi는 **분산 파일시스템(DFS)이 아니다.** 여러 backend를 하나의 통합 파일시스템으로 합치지 않는다. 스토리지 노드에 이미 구성된 스토리지(ZFS, LVM, Ceph 볼륨, GlusterFS 마운트, 일반 디렉토리, raw block device 등)를 **있는 그대로** 네트워크로 공유하는 역할만 한다.

```
┌─────────────────────────────────────────────────────┐
│                  스토리지 노드                         │
│                                                     │
│  ┌─────────┐  ┌──────┐  ┌────────┐  ┌───────────┐  │
│  │ ZFS Pool│  │ LVM  │  │ Ceph   │  │ Local Dir │  │
│  │ (zvol)  │  │ (LV) │  │ (RBD)  │  │ (/data)   │  │
│  └────┬────┘  └──┬───┘  └───┬────┘  └─────┬─────┘  │
│       │          │          │              │        │
│       └──────────┴──────────┴──────────────┘        │
│                         │                           │
│                  pillar-csi agent                    │
│                    (gRPC server)                     │
│                         │                           │
│              ┌──────────┼──────────┐                │
│              │          │          │                │
│           NVMe-oF    iSCSI      NFS                │
│            TCP                                      │
└──────────────┼──────────┼──────────┼────────────────┘
               │          │          │
    ┌──────────┼──────────┼──────────┼────────────┐
    │          ▼          ▼          ▼            │
    │     /dev/nvmeXnY  /dev/sdX   mount point   │
    │                                             │
    │              워커 노드 (Pod)                   │
    └─────────────────────────────────────────────┘
```

### democratic-csi 대비 개선점

| 항목 | democratic-csi | pillar-csi |
|------|---------------|------------|
| **언어** | Node.js | Go (경량, 단일 바이너리) |
| **배포 모델** | backend마다 별도 Helm release (controller StatefulSet + node DaemonSet) | 클러스터당 단일 배포. CRD로 backend/protocol을 선언적으로 관리 |
| **멀티 pool** | pool마다 Helm release 추가. SSH 설정, RBAC, 사이드카 모두 중복 | StoragePool CR 하나 추가하면 끝 |
| **스토리지 노드 통신** | SSH (셸 명령 실행, 취약) | gRPC agent (타입 안전, 안정적) |
| **파라미터 커스터마이징** | StorageClass parameters + PVC annotation으로 제한적 오버라이드 | StorageProtocol → StorageClass → PVC annotation 3단계 계층적 오버라이드 |
| **프로토콜/백엔드 확장** | 드라이버 타입으로 하드코딩 (zfs-generic-iscsi 등) | Backend/Protocol 플러그인 아키텍처 |

## 2. 아키텍처

### 2.1 Custom Resource Definitions

4개의 CRD를 사용한다:

#### StorageNode (agent가 자동 생성/관리)

스토리지를 보유한 노드의 상태를 나타낸다. **사용자가 직접 생성하지 않는다.** pillar-agent가 시작될 때 자동으로 생성하고, 주기적으로 상태를 업데이트한다.

Agent는 시작 시 자신이 실행 중인 Kubernetes Node 이름을 기반으로 StorageNode CR을 생성한다. Controller는 StoragePool의 `nodeRef`를 통해 해당 노드의 agent pod를 자동으로 찾아 gRPC로 통신한다. 별도의 주소/포트 설정이 필요 없다.

```yaml
apiVersion: pillar.storage/v1alpha1
kind: StorageNode
metadata:
  name: rock5bp                 # Kubernetes Node 이름과 동일
  labels:
    pillar.storage/node: "true"
# spec 없음 — agent가 관리하는 읽기 전용 리소스
status:
  connected: true
  agentVersion: "0.1.0"
  # agent가 자동으로 감지한 capabilities
  capabilities:
    backends: [zfs-zvol, zfs-dataset]
    protocols: [nvmeof-tcp]
  # agent가 보고하는 풀 정보
  discoveredPools:
    - name: hot-data
      type: zfs
      total: 712G
      available: 412G
    - name: nas
      type: zfs
      total: 32.7T
      available: 15.0T
    - name: temporal
      type: zfs
      total: 928G
      available: 650G
```

`kubectl get storagenodes`로 어떤 노드가 스토리지를 제공하고 있는지, 어떤 backend/protocol을 지원하는지, 풀 용량이 얼마인지 한눈에 볼 수 있다.

#### StoragePool

특정 노드의 특정 스토리지 풀을 나타낸다. Backend 타입과 설정을 포함한다. **사용자가 생성한다.**

`nodeRef`는 Kubernetes Node 이름(= StorageNode CR 이름)을 참조한다. Controller는 이 이름으로 해당 노드에서 실행 중인 agent pod를 자동으로 찾는다.

```yaml
apiVersion: pillar.storage/v1alpha1
kind: StoragePool
metadata:
  name: rock5bp-hot-data
spec:
  nodeRef: rock5bp              # Kubernetes Node 이름
  backend:
    type: zfs-zvol
    zfs:
      pool: hot-data
      parentDataset: k8s
      # ZFS 속성 기본값
      properties:
        compression: lz4
        volblocksize: 8K
status:
  ready: true
  capacity:
    total: 712G
    available: 412G
    used: 300G
```

#### StorageProtocol

네트워크 공유 프로토콜의 타입과 기본 설정을 정의한다. 노드와 무관하게 재사용 가능하다.

```yaml
apiVersion: pillar.storage/v1alpha1
kind: StorageProtocol
metadata:
  name: nvmeof-tcp
spec:
  type: nvmeof-tcp
  nvmeofTcp:
    port: 4420
    # 전송 최적화 파라미터
    maxQueueSize: 128
    inCapsuleDataSize: 16384
```

#### StorageBinding (StorageClass 연결)

StoragePool과 StorageProtocol을 조합하여 Kubernetes StorageClass와 연결한다. 파라미터 오버라이드 레이어를 제공한다.

```yaml
apiVersion: pillar.storage/v1alpha1
kind: StorageBinding
metadata:
  name: fast-nvmeof
spec:
  poolRef: rock5bp-hot-data
  protocolRef: nvmeof-tcp
  # StorageClass 자동 생성
  storageClass:
    name: fast-nvmeof
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true
  # 이 바인딩에 특화된 파라미터 오버라이드
  parameters:
    zfs:
      properties:
        volblocksize: 16K   # pool 기본값(8K)을 16K로 오버라이드
    nvmeofTcp:
      maxQueueSize: 256     # protocol 기본값(128)을 256으로 오버라이드
```

### 2.2 파라미터 오버라이드 계층

가장 구체적인 레벨이 우선한다:

```
StoragePool (backend 기본값)
  ↓ 오버라이드
StorageProtocol (protocol 기본값)
  ↓ 오버라이드
StorageBinding (바인딩별 오버라이드)
  ↓ 오버라이드
PVC annotation (볼륨별 오버라이드)
```

PVC annotation 예시:
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
  annotations:
    pillar.storage/zfs-properties: '{"volblocksize": "8K", "compression": "zstd"}'
    pillar.storage/nvmeof-queue-size: "64"
spec:
  storageClassName: fast-nvmeof
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 50Gi
```

### 2.3 컴포넌트

```
┌─ Kubernetes Cluster ──────────────────────────────────────┐
│                                                           │
│  ┌─ pillar-controller (Deployment, 1 replica) ──────────┐ │
│  │  • CRD controller (reconcile StorageNode/Pool/        │ │
│  │    Protocol/Binding)                                  │ │
│  │  • CSI Controller (CreateVolume, DeleteVolume,        │ │
│  │    ExpandVolume, Snapshot)                             │ │
│  │  • gRPC client → agent 통신                            │ │
│  │  • CSI sidecars: provisioner, resizer, snapshotter    │ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
│  ┌─ pillar-node (DaemonSet, 모든 노드) ─────────────────┐  │
│  │  • CSI Node (NodeStageVolume, NodePublishVolume)      │ │
│  │  • 프로토콜별 initiator 실행                            │ │
│  │    (nvme connect, iscsiadm, mount.nfs)                │ │
│  │  • CSI sidecar: node-driver-registrar                 │ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
│  ┌─ pillar-agent (DaemonSet, 스토리지 노드만) ───────────┐  │
│  │  • gRPC server (agent API)                            │ │
│  │  • Backend 플러그인: ZFS, LVM, directory 등            │ │
│  │  • Protocol 플러그인: NVMe-oF target, iSCSI target,   │ │
│  │    NFS export 등                                      │ │
│  │  • 볼륨 생성/삭제/리사이즈/스냅샷 실행                    │ │
│  │  • 프로토콜 target 설정 (nvmet configfs, targetcli 등)  │ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
└───────────────────────────────────────────────────────────┘
```

**democratic-csi와의 차이:**
- democratic-csi: backend마다 controller StatefulSet + node DaemonSet = N개 배포
- pillar-csi: controller 1개 + node DaemonSet 1개 + agent DaemonSet 1개 = 항상 3개 배포. Backend/Protocol 추가는 CR만 생성하면 됨.

### 2.4 스토리지 노드 통신: gRPC Agent

스토리지 노드에 경량 Go 바이너리(pillar-agent)가 DaemonSet으로 배포된다. controller와 agent 간 통신은 gRPC(protobuf)로 이루어진다.

**Agent 디스커버리:**
- Agent DaemonSet은 `pillar.storage/storage-node=true` 라벨이 있는 노드에만 스케줄링
- Agent가 시작되면 자신의 Kubernetes Node 이름으로 StorageNode CR을 자동 생성
- Controller는 StoragePool의 `nodeRef`로 노드 이름을 알고, 해당 노드에서 실행 중인 agent pod의 IP를 Kubernetes API로 조회
- 별도의 주소/포트 설정 불필요 — 클러스터 내부 네트워킹으로 자동 해결

**Agent가 하는 일:**
- 볼륨 생명주기: 생성, 삭제, 리사이즈, 스냅샷, 클론
- 프로토콜 target 설정: NVMe-oF subsystem/namespace, iSCSI target/LUN, NFS export
- 상태 보고: 풀 용량, 볼륨 목록, 연결 상태
- StorageNode CR 자동 생성 및 상태 업데이트

**Agent가 필요한 호스트 권한:**
- ZFS 명령 실행: `SYS_ADMIN` capability
- nvmet configfs 조작: `/sys/kernel/config/nvmet/` 마운트
- 블록 디바이스 접근: `/dev` 마운트
- 호스트 네트워크: NVMe-oF target 포트가 외부에서 접근 가능하도록

**democratic-csi의 SSH 방식 대비 장점:**

| SSH (democratic-csi) | gRPC Agent (pillar-csi) |
|---------------------|------------------------|
| SSH 키 관리 필요, YAML 형식 문제 빈발 | 클러스터 내부 통신, 인증 불필요 또는 mTLS |
| 셸 출력 파싱 (취약, 로케일/OS 의존) | 타입 안전한 protobuf 응답 |
| 명령당 SSH 채널 오버헤드 (~1-5ms) | 단일 gRPC 호출로 복합 작업 (~0.3ms) |
| 셸 인젝션 위험 | 구조화된 API, 인젝션 불가 |
| root SSH 접근 필요 (보안 우려) | 최소 권한 컨테이너 |
| 연결 끊김 시 수동 복구 | gRPC 자동 재연결 + health check |
| 테스트: SSH 서버 목업 필요 | 테스트: gRPC 인터페이스 목업 (간편) |

## 3. 구현 방식: Go 직접 호출 vs CLI exec

각 컴포넌트가 시스템과 상호작용하는 방식을 정의한다. 원칙: **커널이 파일 인터페이스(configfs, sysfs, devfs)를 제공하면 Go에서 직접 파일 I/O로 호출한다. 데몬 기반 아키텍처면 CLI exec를 사용한다.**

### 3.1 Agent 측 (스토리지 노드)

| 컴포넌트 | 방식 | 이유 |
|----------|------|------|
| **nvmet (NVMe-oF target)** | Go 파일 I/O | `/sys/kernel/config/nvmet/` configfs를 mkdir, WriteFile, Symlink로 직접 조작. nvmetcli 불필요 |
| **LIO (iSCSI target)** | Go 파일 I/O | `/sys/kernel/config/target/` configfs 직접 조작. targetcli 불필요 |
| **ZFS** | CLI exec (`zfs`, `zpool`) | libzfs Go 바인딩(cgo)이 불안정. CLI가 안정적이고 `-H -p -o` 옵션으로 파싱 용이 |
| **LVM** | CLI exec (`lvcreate` 등) | CLI가 표준 인터페이스 |
| **NFS export** | Go 파일 I/O + exec | `/etc/exports` 직접 쓰기 + `exportfs -ra` exec |

### 3.2 Node 측 (워커 노드)

| 컴포넌트 | 방식 | 이유 |
|----------|------|------|
| **NVMe-oF initiator** | Go 파일 I/O | `/dev/nvme-fabrics`에 write로 connect, sysfs로 disconnect. 데몬/CLI 불필요 |
| **iSCSI initiator** | CLI exec (`iscsiadm`) | iscsid 데몬 기반 아키텍처. iscsiadm → Unix socket → iscsid → netlink → 커널. Go로 직접 하려면 iscsid 재구현 수준 |
| **NFS mount** | Go 라이브러리 | `k8s.io/mount-utils` 사용 |
| **mkfs/mount/resize** | Go 라이브러리 | `k8s.io/mount-utils` — Kubernetes 공식 마운트 유틸리티 |

### 3.3 호스트 의존성

pillar-csi 설치 시 각 노드에 추가 패키지 설치 없이 동작해야 한다. 프로토콜별 호스트 의존성:

| Protocol | 워커 노드 호스트 의존성 | 스토리지 노드 호스트 의존성 | 비고 |
|----------|---------------------|----------------------|------|
| **NVMe-oF TCP** | 커널 모듈만 (`nvme_tcp`, `nvme_fabrics`) | 커널 모듈만 (`nvmet`, `nvmet_tcp`) + ZFS 등 backend | **추가 패키지 불필요.** Go에서 파일 I/O로 모든 것 처리. nvme-extras-dkms로 커널 모듈 제공 |
| **iSCSI** | `open-iscsi` 패키지 + `iscsid` 데몬 | 커널 모듈 (`target_core_mod` 등) | **호스트 패키지 필수.** iSCSI initiator가 데몬 기반 아키텍처이므로 회피 불가. pillar-node 시작 시 iscsid 미감지 시 명확한 에러 메시지 출력 |
| **NFS** | `nfs-common` 패키지 | `nfs-kernel-server` 패키지 | **호스트 패키지 필수.** mount.nfs가 호스트에 필요 |

**NVMe-oF TCP가 유일하게 호스트 추가 설치 없이 동작하는 프로토콜이다.** 이것이 Phase 1으로 선택한 이유 중 하나이다.

iSCSI/NFS 사용 시 pillar-node는 시작 시 필요 도구의 존재를 감지하고, 없으면 구체적인 설치 안내와 함께 에러를 보고한다. 호스트에 패키지를 자동 설치하는 side effect는 발생시키지 않는다.

### 3.4 커널 모듈 및 최소 커널 버전 요구사항

각 Protocol과 Backend는 특정 커널 모듈과 최소 커널 버전을 요구한다. pillar-agent와 pillar-node는 시작 시 **모든 요구사항을 검사**하고, 결과를 StorageNode CR의 `status.capabilities`에 반영한다.

#### Protocol별 커널 요구사항

**NVMe-oF TCP Target (스토리지 노드, pillar-agent):**

| 커널 모듈 | Kconfig | 최소 커널 | 용도 |
|----------|---------|----------|------|
| `nvmet` | `CONFIG_NVME_TARGET` | 4.8 | NVMe target 코어 |
| `nvmet_tcp` | `CONFIG_NVME_TARGET_TCP` | 5.0 | TCP transport for target |
| `configfs` | `CONFIG_CONFIGFS_FS` | 2.6.16 | nvmet 설정 인터페이스 |

**NVMe-oF TCP Initiator (워커 노드, pillar-node):**

| 커널 모듈 | Kconfig | 최소 커널 | 용도 |
|----------|---------|----------|------|
| `nvme_core` | `CONFIG_NVME_CORE` | 3.3 | NVMe 코어 (built-in일 수 있음) |
| `nvme_fabrics` | `CONFIG_NVME_FABRICS` | 4.8 | NVMe-oF 프레임워크 |
| `nvme_tcp` | `CONFIG_NVME_TCP` | 5.0 | TCP transport for host |

**iSCSI Target (스토리지 노드, pillar-agent):**

| 커널 모듈 | Kconfig | 최소 커널 | 용도 |
|----------|---------|----------|------|
| `target_core_mod` | `CONFIG_TARGET_CORE` | 2.6.38 | LIO target 코어 |
| `iscsi_target_mod` | `CONFIG_ISCSI_TARGET` | 3.1 | iSCSI target fabric |
| `target_core_iblock` | `CONFIG_TCM_IBLOCK` | 2.6.38 | 블록 디바이스 backstore |
| `configfs` | `CONFIG_CONFIGFS_FS` | 2.6.16 | LIO 설정 인터페이스 |

**iSCSI Initiator (워커 노드, pillar-node):**

| 커널 모듈 | Kconfig | 최소 커널 | 용도 |
|----------|---------|----------|------|
| `scsi_transport_iscsi` | `CONFIG_SCSI_ISCSI_ATTRS` | 2.6.14 | iSCSI 전송 레이어 (built-in일 수 있음) |
| `libiscsi` | `CONFIG_ISCSI_TCP` | 2.6.14 | iSCSI 라이브러리 |
| `iscsi_tcp` | `CONFIG_ISCSI_TCP` | 2.6.14 | TCP 전송 드라이버 |

**NFS Server (스토리지 노드, pillar-agent):**

| 커널 모듈 | Kconfig | 최소 커널 | 용도 |
|----------|---------|----------|------|
| `nfsd` | `CONFIG_NFSD` | 2.2 | NFS 서버 |
| `nfsv4` | `CONFIG_NFSD_V4` | 2.6 | NFSv4 지원 (권장) |

**NFS Client (워커 노드, pillar-node):**

| 커널 모듈 | Kconfig | 최소 커널 | 용도 |
|----------|---------|----------|------|
| `nfs` | `CONFIG_NFS_FS` | 2.2 | NFS 클라이언트 (대부분 built-in) |
| `nfsv4` | `CONFIG_NFS_V4` | 2.6 | NFSv4 클라이언트 (권장) |

#### Backend별 커널/호스트 요구사항

| Backend | 호스트 바이너리 | 커널 모듈 | 최소 커널 |
|---------|-------------|----------|----------|
| **zfs-zvol** | `zfs`, `zpool` | `zfs` (out-of-tree, DKMS) | 3.10+ (ZFS on Linux 요구) |
| **zfs-dataset** | `zfs`, `zpool` | `zfs` (out-of-tree, DKMS) | 3.10+ |
| **lvm** | `lvcreate`, `lvremove`, `lvresize`, `lvs` | `dm_mod` | 2.6 |
| **block-device** | 없음 | 없음 | - |
| **directory** | 없음 | 없음 | - |

#### Capability 검사 흐름

pillar-agent와 pillar-node는 시작 시 다음을 자동 검사한다:

```
1. 커널 버전 확인 (uname)
2. 각 커널 모듈 존재 여부 확인:
   a. /sys/module/<name> 존재 → 이미 로드됨
   b. modprobe --dry-run <name> 성공 → 로드 가능 (모듈 파일 존재)
   c. 둘 다 실패 → 사용 불가
3. 호스트 바이너리 존재 여부 확인 (which/PATH 검색)
4. 호스트 데몬 상태 확인 (iSCSI: iscsid 프로세스 확인)
5. 결과를 StorageNode CR status.capabilities에 반영
```

**StorageNode status 예시:**

```yaml
status:
  capabilities:
    backends:
      - type: zfs-zvol
        available: true
        details:
          zfsVersion: "2.4.1"
          kernelModule: "loaded"
      - type: lvm
        available: false
        reason: "lvcreate not found in PATH"
    protocols:
      - type: nvmeof-tcp
        target:
          available: true
          kernelModules:
            nvmet: loaded
            nvmet_tcp: loaded
        initiator:
          available: true
          kernelModules:
            nvme_fabrics: loadable
            nvme_tcp: loadable
      - type: iscsi
        target:
          available: true
          kernelModules:
            target_core_mod: loaded
            iscsi_target_mod: loaded
        initiator:
          available: false
          reason: "iscsid daemon not running. Install: apt install open-iscsi && systemctl enable --now iscsid"
      - type: nfs
        target:
          available: false
          reason: "nfsd kernel module not available. Install: apt install nfs-kernel-server"
        initiator:
          available: true
          kernelModules:
            nfs: built-in
```

**에러 처리:**

사용자가 사용 불가능한 기능을 요청하면 (예: NVMe-oF 모듈이 없는 노드에서 NVMe-oF StorageBinding 생성), controller가 즉시 구체적인 에러를 반환한다:

```
StorageBinding "fast-nvmeof" cannot be created:
  Protocol "nvmeof-tcp" is not available on node "rpi4":
    - Kernel module "nvme_tcp" not found.
    - Required minimum kernel version: 5.0 (current: 6.12.62)
    - Fix: Install nvme-extras-dkms package or enable CONFIG_NVME_TCP in kernel config.
```

이 에러는:
1. StorageBinding CR의 `status.conditions`에 기록
2. Kubernetes Event로 발행
3. controller 로그에 출력

## 4. Backend 플러그인

각 Backend는 다음 인터페이스를 구현한다:

```go
type Backend interface {
    // 볼륨 생명주기
    CreateVolume(ctx context.Context, req *CreateVolumeRequest) (*Volume, error)
    DeleteVolume(ctx context.Context, volumeID string) error
    ExpandVolume(ctx context.Context, volumeID string, newSize int64) error

    // 스냅샷
    CreateSnapshot(ctx context.Context, volumeID string, snapshotID string) (*Snapshot, error)
    DeleteSnapshot(ctx context.Context, snapshotID string) error

    // 정보
    GetVolume(ctx context.Context, volumeID string) (*Volume, error)
    ListVolumes(ctx context.Context) ([]*Volume, error)
    GetCapacity(ctx context.Context) (*Capacity, error)

    // 볼륨이 블록 디바이스인지 디렉토리인지
    VolumeType() VolumeType  // Block or Filesystem
}
```

### 4.1 Backend 타입 매트릭스

| Backend | VolumeType | 생성 방식 | 볼륨 경로 | 스냅샷 | 리사이즈 | 클론 |
|---------|-----------|----------|----------|:---:|:---:|:---:|
| **zfs-zvol** | Block | `zfs create -V` | `/dev/zvol/pool/name` | O | O | O |
| **zfs-dataset** | Filesystem | `zfs create` | ZFS 마운트포인트 | O | O (quota) | O |
| **lvm** | Block | `lvcreate` | `/dev/vg/lv` | O (thin) | O | O (thin) |
| **block-device** | Block | 기존 디바이스 사용 | `/dev/sdX` | X | X | X |
| **directory** | Filesystem | `mkdir` | `/path/to/dir` | X | X | X |

## 5. Protocol 플러그인

각 Protocol은 다음 인터페이스를 구현한다:

```go
// Target 측 (스토리지 노드, agent에서 실행)
type ProtocolTarget interface {
    // 볼륨을 네트워크로 export
    ExportVolume(ctx context.Context, volume *Volume, opts ExportOpts) (*ExportInfo, error)
    // export 해제
    UnexportVolume(ctx context.Context, volume *Volume) error
}

// Initiator 측 (워커 노드, CSI node에서 실행)
type ProtocolInitiator interface {
    // 원격 볼륨에 연결하여 로컬 디바이스/마운트포인트 생성
    Connect(ctx context.Context, exportInfo *ExportInfo, opts ConnectOpts) (*LocalDevice, error)
    // 연결 해제
    Disconnect(ctx context.Context, localDevice *LocalDevice) error
}
```

### 5.1 Protocol 호환성 매트릭스

|  | NVMe-oF TCP | iSCSI | NFS |
|--|:---:|:---:|:---:|
| **zfs-zvol** | O | O | - |
| **zfs-dataset** | - | - | O |
| **lvm** | O | O | - |
| **block-device** | O | O | - |
| **directory** | - | - | O |

규칙: **Block 타입 backend는 블록 프로토콜(NVMe-oF, iSCSI)과 호환. Filesystem 타입 backend는 파일 프로토콜(NFS)과 호환.**

StorageBinding 생성 시 호환되지 않는 조합이면 validation webhook이 거부한다.

## 6. 볼륨 생명주기

### 6.1 CreateVolume (PVC 생성 시)

```
1. PVC 생성
2. external-provisioner가 CSI CreateVolume 호출
3. pillar-controller:
   a. StorageBinding에서 poolRef, protocolRef 확인
   b. 파라미터 머지 (Pool → Protocol → Binding → PVC annotation)
   c. StoragePool의 nodeRef로 agent pod IP 자동 조회
   d. gRPC로 agent에 CreateVolume 요청
4. pillar-agent:
   a. Backend 플러그인으로 볼륨 생성 (예: zfs create -V 50G hot-data/k8s/pvc-xxx)
   b. Protocol 플러그인으로 볼륨 export (예: nvmet subsystem/namespace 생성)
   c. ExportInfo 반환 (target IP, port, NQN, namespace ID 등)
5. pillar-controller:
   a. PV 생성, volumeContext에 ExportInfo 저장
```

### 6.2 NodeStageVolume (Pod 스케줄링 시)

```
1. kubelet이 CSI NodeStageVolume 호출
2. pillar-node:
   a. volumeContext에서 ExportInfo 추출
   b. Protocol initiator로 연결 (예: /dev/nvme-fabrics에 write로 NVMe-oF TCP connect)
   c. 로컬 블록 디바이스 생성됨 (예: /dev/nvme0n1)
   d. 파일시스템 포맷 (첫 사용 시) 및 staging 디렉토리에 마운트
```

### 6.3 NodePublishVolume

```
1. kubelet이 CSI NodePublishVolume 호출
2. pillar-node:
   a. staging 디렉토리에서 pod 마운트 포인트로 bind mount
```

## 7. 프로토콜 상태 관리 및 장애 전파

**핵심 원칙: PVC에 마운트된 스토리지 경로의 어떤 지점에서든 장애가 발생하면, 해당 PVC를 사용하는 Pod이 정상 실행되어서는 안 된다.** pillar-csi는 전체 스토리지 경로(backend → target export → 네트워크 → initiator connect → mount)의 건강 상태를 책임진다.

### 7.1 프로토콜별 중간 상태 분석

각 프로토콜이 갖는 중간 상태(daemon, 설정, 커널 상태)와 그 상태가 유실/파손되는 시나리오:

#### NVMe-oF TCP

**Target 측 (스토리지 노드, pillar-agent):**

| 상태 | 저장 위치 | 휘발성 | 유실 시나리오 |
|------|----------|:---:|------------|
| nvmet subsystem | `/sys/kernel/config/nvmet/subsystems/<NQN>` | **휘발** (reboot 시 소멸) | 노드 재부팅, 모듈 unload |
| nvmet namespace | configfs 내 하위 디렉토리 | **휘발** | 상동 |
| nvmet port (TCP listener) | configfs 내 하위 디렉토리 | **휘발** | 상동 |
| subsystem↔port 연결 | configfs symlink | **휘발** | 상동 |
| 데몬 | **없음** | - | - |

→ **모든 상태가 configfs에 있고 reboot 시 소멸.** pillar-agent가 시작될 때 자신이 관리하는 볼륨 목록(PV 메타데이터)을 기반으로 configfs 상태를 reconcile해야 한다.

**Initiator 측 (워커 노드, pillar-node):**

| 상태 | 저장 위치 | 휘발성 | 유실 시나리오 |
|------|----------|:---:|------------|
| NVMe-oF TCP 연결 | 커널 (`/dev/nvme-fabrics`) | **휘발** (reboot 시 소멸) | 노드 재부팅, 네트워크 단절, target 다운 |
| `/dev/nvmeXnY` 블록 디바이스 | 커널 | **휘발** | 연결 끊기면 디바이스 사라짐 |
| 파일시스템 마운트 | 커널 VFS | **휘발** | 블록 디바이스 사라지면 마운트 깨짐 |
| 데몬 | **없음** | - | - |

→ **데몬 없음, 모든 상태가 커널 레벨.** 네트워크 단절 시 NVMe-oF 커널 드라이버가 자체 에러 복구를 시도하고, 실패하면 I/O 에러 발생 → 파일시스템 에러 → Pod에 전파.

#### iSCSI

**Target 측 (스토리지 노드, pillar-agent):**

| 상태 | 저장 위치 | 휘발성 | 유실 시나리오 |
|------|----------|:---:|------------|
| LIO target core 설정 | `/sys/kernel/config/target/` configfs | **휘발** | 노드 재부팅, 모듈 unload |
| iSCSI portal/TPG | configfs | **휘발** | 상동 |
| ACL (접근 제어) | configfs | **휘발** | 상동 |
| LUN 매핑 | configfs | **휘발** | 상동 |
| 데몬 | **없음** (LIO는 커널 내장) | - | - |

→ NVMe-oF target과 동일. configfs 기반이라 reboot 시 소멸. agent가 reconcile 필요.

**Initiator 측 (워커 노드, pillar-node):**

| 상태 | 저장 위치 | 휘발성 | 유실 시나리오 |
|------|----------|:---:|------------|
| **iscsid 데몬** | 프로세스 | **휘발** | 데몬 crash, OOM kill, 노드 재부팅 |
| iSCSI 세션 | iscsid 메모리 + `/etc/iscsi/nodes/` | **부분 영속** | 데몬 crash 시 메모리 상태 유실, 디스크의 node DB에서 복구 가능 |
| `/dev/sdX` 블록 디바이스 | 커널 | **휘발** | 세션 끊기면 디바이스 사라짐 |
| 파일시스템 마운트 | 커널 VFS | **휘발** | 블록 디바이스 사라지면 마운트 깨짐 |

→ **iscsid 데몬이 핵심 상태.** 데몬이 죽으면 세션 관리가 불가능. 다만 커널 레벨 iSCSI 세션은 데몬 독립적으로 유지될 수 있음 — iscsid가 재시작되면 기존 세션을 recovery할 수 있다. 하지만 iscsid가 장시간 다운되면 에러 복구가 불가능해지고 세션이 타임아웃됨.

#### NFS

**Server 측 (스토리지 노드, pillar-agent):**

| 상태 | 저장 위치 | 휘발성 | 유실 시나리오 |
|------|----------|:---:|------------|
| `/etc/exports` 설정 | 파일시스템 | **영속** | 파일 삭제/손상 |
| `nfsd` 커널 데몬 | 커널 스레드 | **휘발** | 재부팅, 서비스 중지 |
| export 테이블 (메모리) | 커널 | **휘발** | nfsd 재시작 시 exportfs -ra로 복구 |

→ `/etc/exports`는 영속적이지만, nfsd와 export 테이블은 휘발. nfsd 재시작 시 `exportfs -ra`로 복구 필요.

**Client 측 (워커 노드, pillar-node):**

| 상태 | 저장 위치 | 휘발성 | 유실 시나리오 |
|------|----------|:---:|------------|
| NFS 마운트 | 커널 VFS | **휘발** | 재부팅, umount |
| 데몬 | **없음** (커널이 NFS 클라이언트) | - | - |

→ **NFS 마운트 옵션이 장애 전파에 결정적.** `hard` 마운트(기본값)는 서버 다운 시 I/O가 무한 대기 — Pod이 hang. `soft` 마운트는 타임아웃 후 I/O 에러 반환. pillar-csi는 **`soft,timeo=300,retrans=3`** 같은 적절한 기본값을 사용하고, StorageProtocol에서 오버라이드 가능하게 한다.

### 7.2 Reconcile 전략

#### Target 측 Reconcile (pillar-agent)

pillar-agent는 시작 시 + 주기적으로 target 상태를 reconcile한다:

```
1. 기대 상태 조회: controller에서 이 노드에 할당된 볼륨 목록 가져옴
2. 실제 상태 조회: configfs 스캔 (nvmet/LIO) 또는 exports 확인 (NFS)
3. 비교:
   a. 기대에 있으나 실제에 없음 → target 재생성 (재부팅 후 복구)
   b. 실제에 있으나 기대에 없음 → orphan target 정리
   c. 양쪽 다 있지만 설정 불일치 → 설정 업데이트
4. 결과를 controller에 보고
```

**NVMe-oF / iSCSI target:**
- configfs는 reboot 시 소멸하므로, agent가 매 시작 시 전체 재생성
- agent crash 후 재시작: configfs 상태는 커널에 남아있으므로 delta만 적용
- 볼륨의 backing 블록 디바이스(zvol)가 사라진 경우: target에서 해당 namespace/LUN 제거 + 에러 보고

**NFS server:**
- `/etc/exports`는 영속적이므로 agent 재시작 시에도 유지
- nfsd 재시작이 필요하면 `exportfs -ra`로 export 테이블 갱신
- export된 디렉토리가 사라진 경우: export 제거 + 에러 보고

#### Initiator 측 Reconcile (pillar-node)

pillar-node는 CSI `NodeGetVolumeStats` 호출 시 + 주기적으로 initiator 상태를 점검한다:

```
1. 마운트된 볼륨 목록 순회
2. 각 볼륨에 대해:
   a. 프로토콜 연결 상태 확인
   b. 블록 디바이스 존재 확인
   c. 마운트 포인트 상태 확인
   d. 간단한 I/O 테스트 (선택적)
3. 이상 발견 시:
   a. 복구 가능 → 자동 복구 시도 (재연결)
   b. 복구 불가 → CSI Volume Condition ABNORMAL 보고
```

### 7.3 프로토콜별 장애 감지 및 복구

#### NVMe-oF TCP

| 장애 | 감지 방법 | 자동 복구 | Pod 영향 |
|------|----------|:---:|---------|
| 네트워크 일시 단절 | NVMe 커널 드라이버 자체 에러 복구 (ctrl_loss_tmo) | **커널이 자동 재연결** (기본 600초 타임아웃) | I/O 일시 정지, 복구되면 재개 |
| target 다운 (agent crash) | NVMe 커널 드라이버 재연결 실패 | agent 재시작 → target reconcile → 커널 자동 재연결 | I/O 일시 정지 → 복구 |
| target 영구 장애 | `ctrl_loss_tmo` 만료 | **복구 불가** | `/dev/nvmeXnY` 사라짐 → 마운트 I/O 에러 → **Pod 장애** |
| 스토리지 노드 재부팅 | target 소멸 → 커널 재연결 실패 | agent 재시작 → configfs reconcile → 커널 자동 재연결 | I/O 일시 정지 → 복구 |

**NVMe-oF 커널 파라미터 (StorageProtocol에서 설정 가능):**
- `ctrl_loss_tmo`: controller loss timeout (초). 이 시간 내 재연결 실패 시 디바이스 제거. 기본값 600초.
- `reconnect_delay`: 재연결 시도 간격 (초). 기본값 10초.
- `nr_io_queues`: I/O 큐 수. 성능 튜닝용.

#### iSCSI

| 장애 | 감지 방법 | 자동 복구 | Pod 영향 |
|------|----------|:---:|---------|
| 네트워크 일시 단절 | iscsid 세션 타임아웃 | **iscsid가 자동 재연결** | I/O 일시 정지, 복구되면 재개 |
| iscsid 데몬 crash | pillar-node 주기적 프로세스 확인 | **복구 불가** (pillar-csi가 iscsid를 시작하지 않음) | 에러 보고, 관리자가 iscsid 재시작 필요 |
| target 다운 | iscsid 재연결 실패 | agent 재시작 → target reconcile → iscsid 재연결 | I/O 일시 정지 → 복구 |
| target 영구 장애 | `replacement_timeout` 만료 | **복구 불가** | 세션 종료 → `/dev/sdX` 사라짐 → **Pod 장애** |

**iSCSI iscsid 파라미터 (StorageProtocol에서 설정 가능):**
- `node.session.timeo.replacement_timeout`: 세션 교체 타임아웃. 기본값 120초.
- `node.conn[0].timeo.noop_out_interval`: keep-alive 간격.
- `node.conn[0].timeo.noop_out_timeout`: keep-alive 타임아웃.

#### NFS

| 장애 | 감지 방법 | 자동 복구 | Pod 영향 |
|------|----------|:---:|---------|
| 네트워크 일시 단절 | NFS 커널 클라이언트 재시도 | **커널이 자동 재시도** | I/O 일시 정지 (`soft`: 타임아웃 후 에러, `hard`: 무한 대기) |
| nfsd 다운 | NFS 클라이언트 타임아웃 | agent 재시작 → nfsd + exportfs 복구 | `soft`: I/O 에러 → **Pod 장애**. `hard`: hang |
| export 제거됨 | NFS ESTALE 에러 | agent reconcile → export 재생성 | ESTALE → I/O 에러 → **Pod 장애** (자동 복구 후 Pod 재시작 필요) |
| `/etc/exports` 손상 | agent 주기적 검사 | agent가 재생성 | 일시적 I/O 에러 |

**NFS 마운트 옵션 (StorageProtocol에서 설정 가능):**
- `soft` vs `hard`: 서버 무응답 시 에러 반환(`soft`) vs 무한 대기(`hard`). **pillar-csi 기본값: `soft`** — Pod이 hang되는 것보다 에러로 실패하는 것이 낫다.
- `timeo`: 타임아웃 (0.1초 단위). 기본값 300 (30초).
- `retrans`: 재시도 횟수. 기본값 3.
- `nfsvers`: NFS 버전. 기본값 4.1.

### 7.4 Volume Health Monitoring

pillar-node는 CSI `NodeGetVolumeStats` (CSI 1.2+) 및 `VOLUME_CONDITION` capability (CSI 1.3+)를 구현하여 볼륨 건강 상태를 Kubernetes에 보고한다.

```go
type VolumeHealthChecker interface {
    // 볼륨의 전체 스토리지 경로 건강 상태 확인
    CheckHealth(ctx context.Context, volumeID string) (*VolumeCondition, error)
}

type VolumeCondition struct {
    Abnormal bool
    Message  string  // 구체적인 장애 원인과 해결 방법
}
```

**점검 항목 (프로토콜별):**

| 점검 | NVMe-oF | iSCSI | NFS |
|------|:---:|:---:|:---:|
| 프로토콜 연결 활성 | `/sys/class/nvme/<ctrl>/state` == `live` | iscsid 세션 상태 확인 | mountpoint 접근 가능 |
| 블록 디바이스 존재 | `/dev/nvmeXnY` 존재 | `/dev/sdX` 존재 | N/A |
| 마운트 포인트 유효 | `/proc/mounts`에서 확인 | 동일 | 동일 |
| I/O 가능 | `statfs()` 호출 | 동일 | 동일 + ESTALE 체크 |
| iscsid 데몬 | N/A | 프로세스 존재 확인 | N/A |

**Kubelet 연동:**
- kubelet은 주기적으로 `NodeGetVolumeStats`를 호출
- `VolumeCondition.Abnormal == true`이면 Kubernetes는 PVC에 `VolumeHealth` Event를 발생시킴
- Pod의 `.status.conditions`에 반영되어 모니터링/알림 가능

### 7.5 장애 전파 보장

**원칙: 스토리지 경로가 끊어졌는데 Pod이 정상 실행되는 상태는 절대 허용하지 않는다.**

스토리지 경로의 모든 레이어에서 장애가 Pod까지 전파되는 흐름:

```
Backend 장애 (zvol 사라짐)
  → Agent가 감지, target에서 해당 namespace/LUN 제거
    → Initiator 연결 끊김 (I/O 에러)
      → 파일시스템 에러 (EIO)
        → Pod 내 프로세스가 I/O 에러 수신
          → 컨테이너 crash 또는 liveness probe 실패
            → Pod 재스케줄링

Target export 사라짐 (configfs 삭제)
  → Initiator 연결 끊김
    → (이하 동일)

네트워크 단절 (장기)
  → Initiator 타임아웃 (ctrl_loss_tmo / replacement_timeout / soft mount timeo)
    → 블록 디바이스 제거 또는 I/O 에러
      → (이하 동일)

Initiator 상태 손상 (iscsid crash)
  → pillar-node가 감지
    → Volume Condition ABNORMAL 보고
      → Kubernetes Event 발생 → 관리자 알림
      → iscsid 복구 전까지 새 Pod 스케줄링 차단 (NodePublishVolume 에러 반환)
```

**NodeStageVolume / NodePublishVolume 시 사전 검증:**

```go
func (n *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
    // 1. 프로토콜 연결 상태 확인
    if !n.protocolInitiator.IsConnected(volumeID) {
        return nil, status.Errorf(codes.FailedPrecondition,
            "volume %s: protocol connection lost, cannot mount to pod", volumeID)
    }

    // 2. 블록 디바이스 / 마운트 소스 존재 확인
    if !n.deviceExists(stagingPath) {
        return nil, status.Errorf(codes.FailedPrecondition,
            "volume %s: staging device missing at %s", volumeID, stagingPath)
    }

    // 3. 정상일 때만 bind mount 수행
    // ...
}
```

Pod에 마운트되기까지의 **모든 경로에서 이상이 있으면 CSI 에러를 반환**하고, kubelet은 Pod을 ContainerCreating 상태로 유지하거나 실패시킨다.

## 8. 로드맵

### Phase 1: ZFS zvol + NVMe-oF TCP (MVP)

**범위:**
- CRD 정의 및 controller: StorageNode, StoragePool, StorageProtocol, StorageBinding
- pillar-agent: ZFS zvol backend + NVMe-oF TCP target (nvmet configfs)
- pillar-node: NVMe-oF TCP initiator (nvme connect/disconnect)
- pillar-controller: CSI Controller (CreateVolume, DeleteVolume, ExpandVolume)
- 기본 CSI Node (NodeStageVolume, NodePublishVolume, NodeUnstageVolume, NodeUnpublishVolume)
- Helm chart
- 파라미터 오버라이드 계층 (Pool → Protocol → Binding → PVC annotation)

**미포함:**
- 스냅샷/클론
- 다른 backend/protocol

### Phase 2: iSCSI Protocol

**추가:**
- pillar-agent: iSCSI target (LIO configfs 직접 조작) protocol 플러그인
- pillar-node: iSCSI initiator (`iscsiadm` CLI exec) protocol 플러그인
- StorageProtocol type: iscsi

**호스트 의존성:** 워커 노드에 `open-iscsi` 패키지 + `iscsid` 데몬 필요. pillar-node가 시작 시 감지하고, 없으면 설치 안내 에러 메시지 출력. 호스트 패키지 자동 설치는 하지 않음.

### Phase 3: ZFS Dataset + NFS

**추가:**
- pillar-agent: ZFS dataset backend 플러그인
- pillar-agent: NFS export (/etc/exports 직접 쓰기 + exportfs exec) protocol 플러그인
- pillar-node: NFS mount initiator (`k8s.io/mount-utils`) 플러그인
- StorageProtocol type: nfs
- ReadWriteMany(RWX) access mode 지원

**호스트 의존성:** 워커 노드에 `nfs-common`, 스토리지 노드에 `nfs-kernel-server` 필요. 호스트 패키지 자동 설치는 하지 않음.

### Phase 4: 스냅샷/클론

**추가:**
- CSI CreateSnapshot, DeleteSnapshot, CreateVolume (from snapshot)
- VolumeSnapshotClass 지원
- ZFS snapshot/clone 통합

### Phase 5: LVM Backend

**추가:**
- pillar-agent: LVM backend 플러그인 (lvcreate/lvremove/lvresize)
- LVM thin provisioning 스냅샷 지원

### Phase 6: 추가 Backend

**후보:**
- block-device (raw partition/device 직접 사용)
- directory (로컬 디렉토리 공유)
- Btrfs subvolume

## 9. 비기능 요구사항

### 9.1 성능

- gRPC agent 통신 오버헤드: < 1ms (LAN)
- 볼륨 프로비저닝 시간: < 5초 (ZFS zvol 기준)
- NVMe-oF 데이터 패스에 pillar-csi 오버헤드 없음 (커널 레벨 프로토콜)

### 9.2 안정성

- agent 연결 끊김 시 자동 재연결
- 볼륨 생성 중 실패 시 정리 (orphan 방지)
- 멱등성: 모든 CSI 오퍼레이션은 멱등적으로 구현
- leader election: controller 고가용성

### 9.3 보안

- agent ↔ controller 간 mTLS 지원
- PVC annotation 파라미터 validation (injection 방지)
- RBAC: CRD별 세분화된 권한 설정

### 9.4 관측성

- Prometheus 메트릭: 볼륨 수, 용량, 오퍼레이션 지연시간, 에러율
- 구조화된 로깅 (JSON)
- CRD status 필드에 상태 반영

## 10. 기술 스택

| 구성요소 | 기술 |
|---------|------|
| 언어 | Go 1.23+ |
| CSI spec | v1.11.0 |
| gRPC | google.golang.org/grpc + protobuf |
| CRD 프레임워크 | controller-runtime (kubebuilder) |
| CLI | cobra |
| 로깅 | slog (stdlib) |
| 메트릭 | prometheus/client_golang |
| 빌드 | goreleaser + ko (컨테이너 이미지) |
| Helm | Helm 3 chart |
| CI | GitHub Actions |

## 11. 용어 정의

| 용어 | 설명 |
|------|------|
| **Backend** | 스토리지 노드에서 볼륨을 생성/관리하는 방법 (ZFS, LVM 등) |
| **Protocol** | 볼륨을 네트워크로 공유하는 방법 (NVMe-oF TCP, iSCSI, NFS 등) |
| **StorageNode** | pillar-agent가 자동 생성하는 노드 상태 CR. 노드 capabilities, 풀 용량 등 조회용 |
| **StoragePool** | 특정 노드의 특정 Backend 인스턴스 (예: rock5bp의 hot-data ZFS pool) |
| **StorageProtocol** | 프로토콜 타입과 기본 설정의 재사용 가능한 정의 |
| **StorageBinding** | StoragePool + StorageProtocol 조합. StorageClass를 자동 생성 |
| **Target** | 스토리지를 네트워크로 내보내는 측 (스토리지 노드) |
| **Initiator** | 네트워크로 내보내진 스토리지에 연결하는 측 (워커 노드) |
| **Agent** | 스토리지 노드에서 실행되는 gRPC 서버. Backend/Protocol 플러그인 실행 |
