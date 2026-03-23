# pillar-csi: Product Requirements Document

## 1. Overview

pillar-csi는 self-hosted Kubernetes 클러스터를 위한 Go 기반 CSI(Container Storage Interface) 드라이버이다. 스토리지 노드에 존재하는 다양한 종류의 로컬 스토리지를 네트워크 프로토콜을 통해 클러스터의 다른 노드에서 사용할 수 있도록 한다.

### 핵심 컨셉

pillar-csi는 **분산 파일시스템(DFS)이 아니다.** 여러 backend를 하나의 통합 파일시스템으로 합치지 않는다. 스토리지 노드에 이미 구성된 스토리지(ZFS, LVM, Ceph 볼륨, GlusterFS 마운트, 일반 디렉토리, raw block device 등 무엇이든)를 **있는 그대로** 네트워크로 공유하는 역할만 한다.

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
│                  pillar-agent                        │
│           (gRPC server + configfs 직접 조작)          │
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
| **배포 모델** | backend마다 별도 Helm release (controller + node DaemonSet 중복) | 클러스터당 단일 배포. CRD로 선언적 관리 |
| **멀티 pool** | pool마다 Helm release. SSH 설정, RBAC, 사이드카 모두 중복 | PillarPool CR 하나 추가 |
| **스토리지 노드 통신** | SSH (셸 명령 파싱, 키 관리, 인젝션 위험) | gRPC agent (타입 안전, 자동 재연결) |
| **Target 설정** | targetcli/nvmetcli CLI (Python 의존) | configfs 직접 조작 (의존성 제로) |
| **노드 사전 설치** | 워커 노드에 open-iscsi, nvme-cli 등 필요 | 컨테이너에 번들 + init container modprobe |
| **파라미터 커스터마이징** | StorageClass parameters + PVC annotation | Pool → Protocol → Binding → PVC annotation 4단계 |
| **프로토콜/백엔드 확장** | 드라이버 타입 하드코딩 (zfs-generic-iscsi 등) | Backend/Protocol 플러그인 아키텍처 |

## 2. 아키텍처

### 2.1 Custom Resource Definitions

API group: `pillar-csi.bhyoo.com`
CSI provisioner name: `pillar-csi.bhyoo.com`

4개의 CRD를 사용한다. **모두 cluster-scoped**이다 (StorageClass와 동일한 인프라 레벨).

#### PillarTarget

스토리지 agent 인스턴스를 나타낸다. **사용자가 생성한다.** Agent의 위치를 정의하고, controller가 agent에 gRPC로 조회한 상태 정보를 status에 반영한다.

`nodeRef`와 `external`은 discriminated union으로, 둘 중 하나만 지정한다.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarTarget
metadata:
  name: rock5bp
spec:
  # K8s 클러스터 내부 노드
  nodeRef:
    name: rock5bp                      # K8s Node 이름
    addressType: InternalIP            # 선택 (기본값: InternalIP) | ExternalIP
    addressSelector: 192.168.219.0/24  # 선택: 동일 타입 IP가 여러 개일 때 CIDR 필터
    port: 9500                         # 선택: agent gRPC 포트 오버라이드

  # 또는 K8s 외부 서버 (Phase N)
  # external:
  #   address: 192.168.1.100
  #   port: 9500

status:
  resolvedAddress: 192.168.219.6
  agentVersion: "0.1.0"
  capabilities:
    backends: [zfs-zvol, zfs-dataset]
    protocols: [nvmeof-tcp]
  discoveredPools:
    - name: hot-data
      type: zfs
      total: 712G
      available: 412G
    - name: nas
      type: zfs
      total: 32.7T
      available: 15.0T
  conditions:
    - type: NodeExists
      status: "True"
      reason: NodeFound
      message: "Node rock5bp exists"
      lastTransitionTime: "2025-01-15T09:55:00Z"
    - type: AgentConnected
      status: "True"
      reason: Connected
      message: "gRPC connection established"
      lastTransitionTime: "2025-01-15T10:00:00Z"
    - type: Ready
      status: "True"
      reason: AllChecksPass
      message: "Target is ready"
      lastTransitionTime: "2025-01-15T10:00:00Z"
```

**PillarTarget conditions:**
| Condition | 의미 |
|-----------|------|
| `NodeExists` | nodeRef의 K8s Node가 존재하는지 |
| `AgentConnected` | agent gRPC 연결 상태 |
| `Ready` | 전체 준비 상태 (모든 condition True) |

gRPC 주소 결정 로직 (nodeRef):
1. K8s Node `status.addresses`에서 `addressType` 매칭
2. 동일 타입이 여러 개면 `addressSelector` CIDR로 필터
3. 미지정 시 첫 번째 InternalIP 사용

#### PillarPool

특정 target의 특정 스토리지 풀. Backend 타입과 설정을 포함한다. **사용자가 생성한다.**

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarPool
metadata:
  name: rock5bp-hot-data
spec:
  targetRef: rock5bp                   # PillarTarget 참조
  backend:
    type: zfs-zvol
    zfs:
      pool: hot-data
      parentDataset: k8s
      properties:
        compression: lz4
        volblocksize: 8K
        quota: 500G                    # 선택: ZFS quota
        reservation: 100G             # 선택: ZFS reservation
status:
  capacity:
    total: 712G
    available: 412G
    used: 300G
  conditions:
    - type: TargetReady
      status: "True"
      reason: TargetReady
      message: "PillarTarget rock5bp is Ready"
      lastTransitionTime: "2025-01-15T10:00:00Z"
    - type: PoolDiscovered
      status: "True"
      reason: PoolFound
      message: "Pool hot-data discovered on agent"
      lastTransitionTime: "2025-01-15T10:00:00Z"
    - type: BackendSupported
      status: "True"
      reason: Supported
      message: "Backend zfs-zvol is supported by agent"
      lastTransitionTime: "2025-01-15T10:00:00Z"
    - type: Ready
      status: "True"
      reason: AllChecksPass
      message: "Pool is ready"
      lastTransitionTime: "2025-01-15T10:00:00Z"
```

**PillarPool conditions:**
| Condition | 의미 |
|-----------|------|
| `TargetReady` | 참조 PillarTarget이 Ready인지 |
| `PoolDiscovered` | agent에서 해당 pool이 발견되었는지 |
| `BackendSupported` | backend 타입이 agent capabilities에 있는지 |
| `Ready` | 전체 준비 상태 |

#### PillarProtocol

네트워크 공유 프로토콜의 타입과 기본 설정. **노드와 무관하게 재사용 가능하다.** Target bind IP는 포함하지 않는다 — controller가 런타임에 PillarTarget에서 resolve하여 agent에 전달한다.

status에는 이 프로토콜을 참조하는 바인딩의 역참조 메타 정보를 포함한다 (`bindingCount`, `activeTargets`). Reconciler가 자동으로 계산한다.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nvmeof-tcp
spec:
  type: nvmeof-tcp
  nvmeofTcp:
    port: 4420
    acl: true                          # true: host NQN 기반 ACL / false: allow_any_host
    maxQueueSize: 128
    inCapsuleDataSize: 16384
    # initiator 타임아웃/재연결 파라미터 (pillar-node가 nvme connect 시 적용)
    ctrlLossTmo: 600                   # 초. target 유실 시 최대 대기 시간
    reconnectDelay: 10                 # 초. 재연결 시도 간격
  # 블록 프로토콜에서 volumeMode: Filesystem일 때 적용
  fsType: ext4                         # ext4 | xfs (기본값: ext4)
  mkfsOptions: []                      # 예: ["-E", "lazy_itable_init=0"]
status:
  bindingCount: 2                      # 이 Protocol을 참조하는 PillarBinding 수
  activeTargets: [rock5bp]             # 이 Protocol이 사용 중인 Target 목록
```

```yaml
# iSCSI 예시
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: iscsi
spec:
  type: iscsi
  iscsi:
    port: 3260
    acl: true
    # iSCSI 타임아웃 파라미터 (pillar-csi가 합리적 기본값 제공)
    loginTimeout: 15                   # 선택: 초 단위 (기본값: 15)
    replacementTimeout: 120            # 선택: 초 단위 (기본값: 120)
    nodeSessionTimeout: 120            # 선택: 초 단위 (기본값: 120)
```

```yaml
# NFS 예시
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nfs
spec:
  type: nfs
  nfs:
    version: "4.2"
```

#### PillarBinding

PillarPool과 PillarProtocol을 조합하여 Kubernetes StorageClass를 자동 생성한다. 파라미터 오버라이드 레이어를 제공한다. **사용자가 생성한다.**

호환되지 않는 조합(Block backend + File protocol)은 validation webhook이 거부한다.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarBinding
metadata:
  name: fast-nvmeof
spec:
  poolRef: rock5bp-hot-data
  protocolRef: nvmeof-tcp
  storageClass:
    name: fast-nvmeof
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true          # 선택: 미지정 시 backend capability에서 자동 결정
  overrides:
    backend:
      zfs:
        properties:
          volblocksize: 16K            # pool 기본값(8K) 오버라이드
    protocol:
      nvmeofTcp:
        maxQueueSize: 256              # protocol 기본값(128) 오버라이드
    fsType: ext4                       # 선택: ext4(기본값) 또는 xfs. 블록 프로토콜 + volumeMode: Filesystem일 때만
    mkfsOptions: ["-E", "lazy_itable_init=1"]  # 선택: mkfs 추가 옵션
status:
  storageClassName: fast-nvmeof
  conditions:
    - type: PoolReady
      status: "True"
      reason: PoolReady
      message: "PillarPool rock5bp-hot-data is Ready"
      lastTransitionTime: "2025-01-15T10:00:00Z"
    - type: ProtocolValid
      status: "True"
      reason: ProtocolExists
      message: "PillarProtocol nvmeof-tcp exists"
      lastTransitionTime: "2025-01-15T10:00:00Z"
    - type: Compatible
      status: "True"
      reason: Compatible
      message: "zfs-zvol is compatible with nvmeof-tcp"
      lastTransitionTime: "2025-01-15T10:00:00Z"
    - type: StorageClassCreated
      status: "True"
      reason: Created
      message: "StorageClass fast-nvmeof created"
      lastTransitionTime: "2025-01-15T10:01:00Z"
    - type: Ready
      status: "True"
      reason: AllChecksPass
      message: "Binding is ready"
      lastTransitionTime: "2025-01-15T10:01:00Z"
```

**PillarBinding conditions:**
| Condition | 의미 |
|-----------|------|
| `PoolReady` | 참조 PillarPool이 Ready인지 |
| `ProtocolValid` | 참조 PillarProtocol이 존재하는지 |
| `Compatible` | Backend-Protocol 호환성 검증 통과 |
| `StorageClassCreated` | SC 자동 생성 완료 |
| `Ready` | 전체 준비 상태 |

### 2.2 프로토콜 카테고리와 VolumeMode/AccessMode

프로토콜은 **블록**과 **파일시스템** 두 카테고리로 나뉜다. Kubernetes의 `volumeMode`와 `accessModes`에 직접 매핑된다.

#### 블록 프로토콜

| 프로토콜 | 클라이언트 디바이스 | AccessMode | volumeMode |
|----------|-----------------|------------|------------|
| NVMe-oF TCP | `/dev/nvmeXnY` | RWO, RWOP, ROX | Block 또는 Filesystem |
| iSCSI | `/dev/sdX` | RWO, RWOP, ROX | Block 또는 Filesystem |

- `volumeMode: Filesystem` → 블록 디바이스에 mkfs + mount
- `volumeMode: Block` → raw 블록 디바이스를 Pod에 직접 제공

#### 파일시스템 프로토콜

| 프로토콜 | 클라이언트 마운트 | AccessMode | volumeMode |
|----------|---------------|------------|------------|
| NFS | 마운트된 디렉토리 | RWX, RWO, ROX | Filesystem만 |
| SMB | 마운트된 디렉토리 | RWX, RWO, ROX | Filesystem만 |

RWX는 Phase 3 (NFS)에서 지원한다.

#### Backend-Protocol 호환성 매트릭스

|  | NVMe-oF TCP | iSCSI | NFS | SMB |
|--|:---:|:---:|:---:|:---:|
| **zfs-zvol** (Block) | O | O | - | - |
| **zfs-dataset** (FS) | - | - | O | O |
| **lvm** (Block) | O | O | - | - |
| **block-device** (Block) | O | O | - | - |
| **directory** (FS) | - | - | O | O |

규칙: **Block backend ↔ Block protocol, Filesystem backend ↔ Filesystem protocol.**

### 2.3 파라미터 오버라이드 계층

모든 스택(backend, protocol)의 파라미터를 **PVC 단위까지 세밀하게 커스터마이징**할 수 있다. 가장 구체적인 레벨이 우선한다.

```
PillarPool (backend 기본값)
  ↓ 오버라이드
PillarProtocol (protocol 기본값)
  ↓ 오버라이드
PillarBinding (바인딩별 오버라이드 — CRD typed schema)
  ↓ 오버라이드
PVC annotation (볼륨별 오버라이드)
```

PillarBinding의 오버라이드는 CRD typed schema를 사용한다 (JSON string이 아님). Phase 1에서는 ZFS + NVMe-oF 필드를 정적으로 정의하고, 타입 추가 시 kubebuilder marker로 확장한다.

오버라이드 가능 항목:
- Backend 파라미터: ZFS properties(compression, volblocksize 등)
- Protocol 파라미터: NVMe-oF/iSCSI 타임아웃, 큐 사이즈 등
- fsType: ext4(기본값) 또는 xfs. 블록 프로토콜 + `volumeMode: Filesystem`일 때만
- mkfsOptions: mkfs 추가 옵션

**PVC annotation 오버라이드 범위 제한:** 튜닝 파라미터만 허용한다 (properties, maxQueueSize, fsType 등). 구조적 참조 변경(pool, parentDataset, type, port 등)은 controller가 CreateVolume 시점에 거부한다. CRD 필드 immutability 규칙과 동일 기준.

PVC annotation 예시:
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
  annotations:
    pillar-csi.bhyoo.com/backend-override: |
      zfs:
        properties:
          volblocksize: "8K"
          compression: zstd
    pillar-csi.bhyoo.com/protocol-override: |
      nvmeofTcp:
        maxQueueSize: 64
    pillar-csi.bhyoo.com/fs-override: |
      fsType: xfs
      mkfsOptions: ["-K"]
spec:
  storageClassName: fast-nvmeof
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 50Gi
```

### 2.4 컴포넌트

```
┌─ Kubernetes Cluster ──────────────────────────────────────┐
│                                                           │
│  ┌─ pillar-controller (Deployment, 1 replica) ──────────┐ │
│  │  • CRD reconciler (PillarTarget/Pool/Protocol/        │ │
│  │    Binding)                                           │ │
│  │  • CSI Controller service                             │ │
│  │  • gRPC client → agent 통신                            │ │
│  │  • PillarTarget status 관리                             │ │
│  │  • Target bind IP resolve (PillarTarget → Node IP)    │ │
│  │  • StorageClass 자동 생성 (PillarBinding → SC)         │ │
│  │  • 노드 label 관리 (PillarTarget ↔ storage-node)      │ │
│  │  • CSI 작업 재시도 + 롤백 (exponential backoff)         │ │
│  │  • Agent 복구: 연결 복구 시 전체 상태 push              │ │
│  │  • CSI sidecars: provisioner, attacher, resizer,      │ │
│  │    liveness-probe                                     │ │
│  │    (snapshotter는 Phase 4에서 추가)                    │ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
│  ┌─ pillar-node (DaemonSet, 모든 워커 노드) ──────────────┐ │
│  │  • CSI Node service                                   │ │
│  │  • 프로토콜 initiator 실행                              │ │
│  │    - NVMe-oF: nvme connect/disconnect                 │ │
│  │    - iSCSI: iscsiadm login/logout                     │ │
│  │    - NFS: mount.nfs / umount                          │ │
│  │    - SMB: mount.cifs / umount                         │ │
│  │  • 유저스페이스 도구 컨테이너 번들                         │ │
│  │  • Init container: 커널 모듈 modprobe (best-effort)     │ │
│  │  • CSI sidecars: node-driver-registrar, liveness-probe│ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
│  ┌─ pillar-agent (DaemonSet, 스토리지 노드만) ───────────┐  │
│  │  • nodeSelector: pillar-csi.bhyoo.com/storage-node    │ │
│  │  • gRPC server (Phase 1: 평문, TLS 옵션 준비)          │ │
│  │  • 완전 stateless — 복구 시 controller에서 상태 수신     │ │
│  │  • Backend 플러그인: ZFS, LVM, directory 등            │ │
│  │  • Protocol target 플러그인:                           │ │
│  │    - NVMe-oF: nvmet configfs 직접 조작                 │ │
│  │    - iSCSI: LIO configfs 직접 조작                     │ │
│  │    - NFS: kernel nfsd 설정                             │ │
│  │    - SMB: Samba 설정                                   │ │
│  │  • K8s API 의존성 없음 — 순수 gRPC 서버                  │ │
│  │  • hostNetwork 불필요 (hostPort: 9500으로 노드 IP 노출) │ │
│  │  • Init container: target 커널 모듈 modprobe            │ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
└───────────────────────────────────────────────────────────┘
```

**democratic-csi와의 배포 차이:**
- democratic-csi: backend마다 controller StatefulSet + node DaemonSet = N개 배포
- pillar-csi: controller 1개 + node DaemonSet 1개 + agent DaemonSet 1개 = 항상 3개. Backend/Protocol 추가는 CR만 생성.

### 2.5 스토리지 노드 통신: gRPC Agent

#### Agent 역할

스토리지 노드에서 실행되는 경량 Go 바이너리. **K8s API에 의존하지 않는 순수 gRPC 서버**로, K8s DaemonSet과 외부 standalone 배포에서 **동일한 바이너리**를 사용한다.

Agent는 **완전 stateless**이다. 로컬에 상태를 저장하지 않으며, controller가 gRPC로 전달하는 지시에 따라 동작한다.

CLI 도구 없이 **configfs 직접 조작**으로 target을 설정한다:

| Protocol | configfs 경로 | Go 참조 구현 |
|----------|-------------|------------|
| NVMe-oF TCP | `/sys/kernel/config/nvmet/` | `github.com/0xfd4d/nvmet-config` (~150줄) |
| iSCSI LIO | `/sys/kernel/config/target/iscsi/` | `github.com/sapslaj/shortrack` (~1400줄) |
| NFS | `/etc/exports` + `exportfs` | 직접 작성 |

#### Agent 디스커버리

- **K8s 내부** (Phase 1): PillarTarget의 `nodeRef` → K8s Node `status.addresses`에서 IP 조회 → `<nodeIP>:<port>`로 직접 연결 (DaemonSet `hostPort` 사용, pod IP 조회 불필요)
- **K8s 외부** (Phase N): PillarTarget의 `external` → 명시된 address로 직접 연결

#### Agent DaemonSet 노드 선택

PillarTarget CR 생성 시 controller가 해당 노드에 `pillar-csi.bhyoo.com/storage-node=true` label을 자동 부여한다. Agent DaemonSet은 이 label이 있는 노드에만 스케줄링된다. PillarTarget 삭제 시 label도 자동 제거된다. 사용자는 PillarTarget CR만 만들면 agent가 자동으로 배포된다.

#### Agent 크래시/리부트 복구

Agent는 **완전히 stateless**하다. 로컬 상태를 저장하지 않는다. configfs는 리부트 시 소멸되므로, agent 재시작이나 노드 리부트 후 controller가 해당 target의 모든 볼륨 + export 상태를 gRPC로 push한다. Agent는 받은 상태를 configfs에 다시 적용(reconcile)한다.

#### Agent가 필요한 호스트 권한

| 권한 | 용도 |
|------|------|
| `CAP_SYS_ADMIN` | configfs 조작, ZFS 명령 실행 |
| `CAP_SYS_MODULE` (init container) | 커널 모듈 로드 |
| `/sys/kernel/config` 마운트 | nvmet/LIO configfs 접근 |
| `/dev` 마운트 | 블록 디바이스(zvol 등) 접근 |
| `/lib/modules` 읽기 마운트 (init) | modprobe용 |
| `hostPort: 9500` (DaemonSet) | agent gRPC 서버를 노드 IP로 노출 (hostNetwork 없이) |

**hostNetwork 불필요.** NVMe-oF/iSCSI target은 호스트 커널 레벨 서비스이므로, agent pod 네트워킹 모드와 무관하게 호스트 네트워크에서 listen한다. Target bind IP는 controller가 PillarTarget nodeRef에서 resolve하여 gRPC로 agent에 전달한다.

#### gRPC 보안

Phase 1에서는 평문 gRPC를 사용한다. TLS 지원은 아키텍처에 포함하되 Phase 1에서는 비활성 상태이다:
- Agent와 controller 모두 TLS 인증서 경로 설정 옵션을 가진다
- 설정 미지정 시 평문으로 동작 (Phase 1 기본값)
- 향후 mTLS 활성화 시 코드 변경 없이 설정만으로 전환

#### SSH 대비 gRPC의 장점

| SSH (democratic-csi) | gRPC Agent (pillar-csi) |
|---------------------|------------------------|
| SSH 키 관리, YAML 형식 오류 빈발 | 클러스터 내부 통신, 추가 인증 불필요 |
| 셸 출력 파싱 (취약, 로케일/OS 의존) | 타입 안전한 protobuf 응답 |
| 명령당 SSH 채널 오버헤드 (~1-5ms) | 단일 gRPC 호출 (~0.3ms), HTTP/2 멀티플렉싱 |
| 셸 인젝션 위험 | 구조화된 API, 인젝션 불가 |
| root SSH 접근 필요 | 최소 권한 컨테이너 |
| targetcli/nvmetcli Python 의존 | configfs 직접 조작, 의존성 제로 |
| 연결 끊김 시 수동 복구 | gRPC 자동 재연결 + health check |
| Teleport 사례: gRPC 전환 시 레이턴시 40% 감소 | |

### 2.6 Zero-Install 전략

**목표: 사용자가 CSI를 위해 K8s 노드에 직접 뭔가를 설치/설정할 필요 없음.**

| 구성요소 | 번들 가능 | 전략 |
|---------|:---:|------|
| **유저스페이스 도구** | | |
| nvme-cli | O | pillar-node 컨테이너에 포함 |
| open-iscsi (iscsiadm, iscsid) | O | 동일 |
| nfs-common (mount.nfs) | O | 동일 |
| cifs-utils (mount.cifs) | O | 동일 |
| mkfs 도구 (e2fsprogs, xfsprogs) | O | 동일 |
| **커널 모듈 (initiator)** | | |
| nvme_tcp, nvme_fabrics | X | init container modprobe |
| iscsi_tcp, libiscsi | X | 동일 |
| nfs (거의 항상 built-in) | - | 대부분 이미 있음 |
| cifs | X | init container modprobe |
| **커널 모듈 (target)** | | |
| nvmet, nvmet_tcp | X | agent init container modprobe |
| target_core_mod, iscsi_target_mod | X | 동일 |
| **Target CLI 도구** | | |
| targetcli, nvmetcli | 불필요 | configfs 직접 조작으로 대체 |

**modprobe 실패 정책:** Init container는 best-effort로 modprobe를 실행한다. 실패해도 pod 시작을 차단하지 않는다.
- **pillar-agent:** 모듈 로딩 실패 시 해당 프로토콜을 capabilities에서 제외하고 계속 동작. PillarTarget status에 반영.
- **pillar-node:** 모듈 로딩 실패 시 pod은 정상 시작. 해당 프로토콜의 볼륨 마운트 요청이 오면 NodeStageVolume에서 명확한 에러 메시지 반환 (예: "nvme_tcp module not available on this node").

**한계:** 커널 모듈이 커널에 빌드되지 않은 경우 (예: RPi의 nvme_tcp) modprobe가 실패한다. 이 경우 DKMS 패키지 사전 설치가 필요하다.

### 2.7 ControllerPublishVolume — 접근 제어

CSI `ControllerPublishVolume`/`ControllerUnpublishVolume` RPC를 구현하여 볼륨 접근 제어를 설정한다. ACL 사용 여부는 PillarProtocol에서 설정한다.

| Protocol | ACL 메커니즘 | acl: true | acl: false |
|----------|------------|-----------|------------|
| NVMe-oF TCP | `allowed_hosts` symlink | host NQN 추가/제거 | `attr_allow_any_host=1` |
| iSCSI | LIO ACL | initiator IQN 추가/제거 | `generate_node_acls=1` |
| NFS | export client list | 클라이언트 IP 추가/제거 | 전체 허용 |

`acl: false`이면 ControllerPublish/Unpublish는 no-op이다.

## 3. Backend 플러그인

각 Backend는 다음 인터페이스를 구현한다:

```go
type Backend interface {
    CreateVolume(ctx context.Context, req *CreateVolumeRequest) (*Volume, error)
    DeleteVolume(ctx context.Context, volumeID string) error
    ExpandVolume(ctx context.Context, volumeID string, newSize int64) error

    CreateSnapshot(ctx context.Context, volumeID string, snapshotID string) (*Snapshot, error)
    DeleteSnapshot(ctx context.Context, snapshotID string) error

    GetVolume(ctx context.Context, volumeID string) (*Volume, error)
    ListVolumes(ctx context.Context) ([]*Volume, error)
    GetCapacity(ctx context.Context) (*Capacity, error)

    VolumeType() VolumeType  // Block or Filesystem
}
```

### Backend 타입 매트릭스

| Backend | VolumeType | 생성 방식 | 볼륨 경로 | 스냅샷 | 리사이즈 | 클론 |
|---------|-----------|----------|----------|:---:|:---:|:---:|
| **zfs-zvol** | Block | `zfs create -V` | `/dev/zvol/pool/name` | O | O | O |
| **zfs-dataset** | Filesystem | `zfs create` | ZFS 마운트포인트 | O | O (quota) | O |
| **lvm** | Block | `lvcreate` | `/dev/vg/lv` | O (thin) | O | O (thin) |
| **block-device** | Block | 기존 디바이스 사용 | `/dev/sdX` | X | X | X |
| **directory** | Filesystem | `mkdir` | `/path/to/dir` | X | X | X |

## 4. Protocol 플러그인

각 Protocol은 Target과 Initiator 양측 인터페이스를 구현한다:

```go
// Target 측 (agent에서 실행)
type ProtocolTarget interface {
    ExportVolume(ctx context.Context, req *ExportRequest) (*ExportInfo, error)
    UnexportVolume(ctx context.Context, exportInfo *ExportInfo) error
    AllowInitiator(ctx context.Context, exportInfo *ExportInfo, initiatorID string) error
    DenyInitiator(ctx context.Context, exportInfo *ExportInfo, initiatorID string) error
}

// Initiator 측 (pillar-node에서 실행)
type ProtocolInitiator interface {
    Connect(ctx context.Context, exportInfo *ExportInfo, opts ConnectOpts) (*LocalDevice, error)
    Disconnect(ctx context.Context, localDevice *LocalDevice) error
    GetInitiatorID(ctx context.Context) (string, error)
}
```

### Protocol 구현 세부사항

| | NVMe-oF TCP | iSCSI | NFS | SMB |
|--|--|--|--|--|
| **Target 구현** | nvmet configfs | LIO configfs | /etc/exports + exportfs | Samba |
| **Initiator 구현** | nvme-cli | open-iscsi | mount.nfs | mount.cifs |
| **Initiator ID** | NQN | IQN | Client IP | Client IP |
| **기본 포트** | 4420 | 3260 | 2049 | 445 |
| **커널 모듈 (target)** | nvmet, nvmet_tcp | target_core_mod, iscsi_target_mod | nfsd | (user-space) |
| **커널 모듈 (initiator)** | nvme_tcp, nvme_fabrics | iscsi_tcp, libiscsi | nfs (built-in) | cifs |

## 5. 볼륨 생명주기

### Volume ID 형식

구조화된 ID: `<target>/<pool>/<volume-name>` (슬래시 구분). Controller가 ID만 보고 어떤 agent에 요청할지 라우팅할 수 있다.

```
Volume ID: rock5bp/hot-data/pvc-abc123

파싱:
  target: rock5bp       → PillarTarget 참조 → agent gRPC 주소
  pool: hot-data         → ZFS pool 이름
  name: pvc-abc123       → 볼륨 이름

ZFS 경로: hot-data/k8s/pvc-abc123
NVMe NQN: nqn.2024-01.com.bhyoo.pillar-csi:rock5bp:pvc-abc123
```

### 5.1 CreateVolume (PVC 생성)

```
1. PVC 생성
2. external-provisioner → CSI CreateVolume
3. pillar-controller:
   a. PillarBinding에서 poolRef, protocolRef 확인
   b. 파라미터 머지 (Pool → Protocol → Binding → PVC annotation)
      - PVC annotation은 튜닝 파라미터만 허용, 구조적 참조 거부
   c. Backend-Protocol 호환성 검증
   d. PillarPool → PillarTarget → Node IP resolve
   e. gRPC로 agent에 CreateVolume + ExportVolume 요청
      (target bind IP도 함께 전달)
   f. 중간 실패 시 롤백 (예: export 실패 → 생성된 볼륨 삭제)
4. pillar-agent:
   a. Backend: 볼륨 생성 (예: zfs create -V 50G hot-data/k8s/pvc-xxx)
   b. Protocol: 볼륨 export (예: nvmet configfs에 subsystem/namespace/port 생성)
   c. ExportInfo 반환 (NQN, namespace ID 등)
5. pillar-controller:
   a. Volume ID 생성: {target}/{pool}/{volume-name}
   b. PV 생성, volumeContext에 ExportInfo 저장
```

### 5.2 ControllerPublishVolume (Pod 스케줄링)

```
1. external-attacher → CSI ControllerPublishVolume
2. pillar-controller:
   a. Volume ID에서 target/pool 파싱하여 라우팅 대상 결정
   b. 대상 노드의 initiator ID 조회 (NodeGetInfo에서 등록된 NQN/IQN)
   c. PillarProtocol의 acl 설정 확인
   d. acl=true: gRPC로 agent에 AllowInitiator 요청
      (NVMe-oF: allowed_hosts에 NQN symlink / iSCSI: ACL에 IQN)
   e. acl=false: no-op
3. publish_context 반환
```

### 5.3 NodeStageVolume

```
1. kubelet → CSI NodeStageVolume
2. pillar-node:
   a. volumeContext + publish_context에서 ExportInfo 추출
   b. Protocol initiator 연결:
      NVMe-oF: nvme connect -t tcp -a <ip> -s <port> -n <nqn>
              (PillarProtocol 타임아웃 파라미터 적용:
               --ctrl-loss-tmo, --reconnect-delay, --keep-alive-tmo)
      iSCSI: iscsiadm -m discovery + login
             (타임아웃 파라미터 적용)
      NFS: mount.nfs <ip>:<path> <staging>
   c. Block protocol + volumeMode=Filesystem:
      mkfs (첫 사용, fsType/mkfsOptions 적용) + mount
   d. Block protocol + volumeMode=Block: 디바이스 경로 기록
   e. 커널 모듈 미로드 시 명확한 에러 반환
```

### 5.4 NodePublishVolume

```
1. kubelet → CSI NodePublishVolume
2. pillar-node:
   a. volumeMode=Filesystem: staging → pod mount point bind mount
   b. volumeMode=Block: 블록 디바이스를 pod에 device file로 제공
```

## 6. 로드맵

### Phase 1: ZFS zvol + NVMe-oF TCP (MVP)

**범위:**
- CRD: PillarTarget, PillarPool, PillarProtocol, PillarBinding (모두 cluster-scoped)
- CRD controller + validation webhook (immutable 필드 검증)
- CRD status conditions (K8s 표준 패턴)
- Finalizer 기반 의존성 삭제 보호
- pillar-agent: ZFS zvol backend + NVMe-oF TCP target (configfs)
- pillar-agent: stateless 설계, controller push 복구
- pillar-node: NVMe-oF TCP initiator + init container modprobe (best-effort) + 도구 번들
- pillar-controller: CSI Controller (CreateVolume, DeleteVolume, ExpandVolume, ControllerPublishVolume/UnpublishVolume, ValidateVolumeCapabilities, GetCapacity)
- pillar-controller: CSI 작업 재시도/롤백 (exponential backoff)
- pillar-controller: PillarTarget 노드 label 자동 관리
- CSI Node (Stage/Unstage/Publish/Unpublish, NodeGetVolumeStats, NodeExpandVolume)
- NVMe-oF ACL on/off (PillarProtocol acl 필드)
- NVMe-oF 타임아웃 파라미터 (PillarProtocol 필드)
- StorageClass 자동 생성 (PillarBinding reconcile, ownerReference 관리)
- 파라미터 오버라이드 계층 (Pool → Protocol → Binding → PVC annotation)
- fsType/mkfsOptions 오버라이드 (기본값: ext4)
- volumeMode: Filesystem 지원
- 볼륨 확장 (allowVolumeExpansion: backend capability 자동 결정 + 사용자 오버라이드)
- AccessMode: RWO, RWOP, ROX
- 구조화된 Volume ID: `{target}/{pool}/{volume-name}`
- gRPC 평문 통신 (TLS 옵션은 아키텍처에 포함, 비활성)
- Helm chart
- K8s 내부 노드만

**미포함:** 스냅샷/클론, volumeMode: Block, CSI Topology, RWX, 다른 backend/protocol, 외부 노드, 볼륨 와이핑, 별도 CLI/대시보드

### Phase 2: iSCSI Protocol
- LIO configfs 직접 조작 (target) + open-iscsi (initiator)

### Phase 3: ZFS Dataset + NFS
- ZFS dataset backend + NFS export + RWX 지원

### Phase 4: 스냅샷/클론
- CSI Snapshot + ZFS snapshot/clone 통합

### Phase 5: LVM Backend

### Phase 6: SMB Protocol

### Phase 7: 외부 노드 지원
- PillarTarget `spec.external` + agent standalone 바이너리

### Phase 8: 추가 Backend
- block-device, directory, Btrfs subvolume

## 7. 운영 정책

### 7.1 CRD 필드 Immutability

| 필드 구분 | 예시 | 수정 가능 |
|----------|------|:---:|
| **참조/타입** | targetRef, poolRef, protocolRef, backend.type, protocol.type | X (validation webhook 거부) |
| **튜닝 파라미터** | properties, maxQueueSize, acl, fsType, ctrlLossTmo | O |

### 7.2 의존성 삭제 보호

Finalizer를 사용하여 하위 참조가 있으면 삭제를 거부한다:

```
PillarTarget ← PillarPool ← PillarBinding ← StorageClass ← PVC
```

- PillarPool이 참조하는 PillarTarget 삭제 시도 → **거부**
- PillarBinding이 참조하는 PillarPool 삭제 시도 → **거부**
- 활성 PVC가 있는 StorageClass의 PillarBinding 삭제 시도 → **거부**

사용자는 역순(PVC → PillarBinding → PillarPool → PillarTarget)으로 삭제해야 한다.

### 7.3 CRD Status Conditions

모든 CRD status conditions는 K8s 표준 패턴을 따른다. 각 condition은 다음 필드를 가진다:
- `type` — condition 타입 (예: Ready, AgentConnected)
- `status` — "True", "False", "Unknown"
- `reason` — 기계 판독 가능한 이유 코드
- `message` — 사람이 읽을 수 있는 상세 메시지
- `lastTransitionTime` — status가 마지막으로 변경된 시각

이 정보가 트러블슈팅의 주요 수단이다 (7.7 에러/트러블슈팅 DX 참조).

### 7.4 StorageClass 라이프사이클

PillarBinding이 ownerReference로 StorageClass를 완전 관리한다:

- PillarBinding 생성 → StorageClass 자동 생성
- PillarBinding spec 수정 → StorageClass 업데이트
- PillarBinding 삭제 → StorageClass 삭제
- StorageClass 직접 수정(`kubectl edit sc`) → reconciler가 PillarBinding spec으로 되돌림

StorageClass 이름은 PillarBinding의 `spec.storageClass.name`에서 사용자가 명시적으로 지정한다. `allowVolumeExpansion`은 backend capability에서 자동 결정되되, 사용자가 오버라이드할 수 있다.

### 7.5 CSI 작업 실패 시 롤백/재시도

- **CreateVolume 중간 실패**: Agent에서 zvol 생성 성공 후 export 실패 시, controller가 생성된 리소스를 **자동 롤백(정리)**.
- **재시도 정책**: Controller 자체 exponential backoff 재시도. 최대 횟수/타임아웃 설정 가능.
- **Agent 미연결 시 DeleteVolume**: controller가 agent 재연결까지 재시도. 중간 상태는 CRD status conditions에 반영.
- **볼륨 삭제**: 단순 `zfs destroy`. 데이터 와이핑 없음 (Phase 1).

### 7.6 용량 관리

Controller는 사전 용량 검증을 하지 않는다. Agent에 요청을 보내고 `zfs create` 실패 시 에러를 반환한다. ZFS의 quota/reservation 기능은 PillarPool의 backend properties에서 설정 가능하다. Controller 레벨 quota (Pool별 최대 프로비저닝 제한)는 Phase 1 스코프 아님.

### 7.7 에러/트러블슈팅 DX

**kubectl 네이티브만** 사용한다. 별도 CLI나 웹 대시보드를 제공하지 않는다.

- `kubectl describe pillartarget <name>` — conditions로 agent 연결, 노드 존재 여부 진단
- `kubectl describe pillarpool <name>` — conditions로 pool 발견, backend 지원 여부 진단
- `kubectl describe pillarbinding <name>` — conditions로 호환성, SC 생성 상태 진단
- `kubectl describe pvc <name>` — Events에서 프로비저닝 실패 원인 확인
- `kubectl get events --field-selector reason=ProvisioningFailed` — 볼륨 생성 실패 이벤트 조회

## 8. 비기능 요구사항

### 8.1 성능
- gRPC agent 통신 오버헤드: < 1ms (LAN)
- 볼륨 프로비저닝 시간: < 5초 (ZFS zvol 기준)
- NVMe-oF/iSCSI 데이터 패스에 pillar-csi 오버헤드 없음 (커널 레벨 프로토콜)

### 8.2 안정성
- Agent 연결 끊김 시 gRPC 자동 재연결 (keepalive)
- 볼륨 생성 중간 실패 시 자동 롤백 (orphan 방지)
- 멱등성: 모든 CSI 오퍼레이션은 멱등적으로 구현
- Agent 크래시/리부트 복구: controller가 전체 상태를 push (agent stateless)
- Controller 자체 재시도 로직: exponential backoff, 설정 가능한 최대 횟수/타임아웃
- Leader election 구현 완료 (`--leader-election` 플래그); Phase 1 Helm chart 기본값 replicas=1, 필요 시 스케일아웃 가능

### 8.3 보안
- Phase 1: 평문 gRPC (클러스터 내부 신뢰). TLS 옵션 아키텍처에 포함, 비활성
- Phase N: Agent ↔ controller 간 mTLS (외부 노드 지원 시)
- NVMe-oF/iSCSI ACL on/off (PillarProtocol acl 필드)
- PVC annotation 파라미터 validation — 튜닝 파라미터만 허용, 구조적 참조 거부
- RBAC: CRD별 세분화된 권한

### 8.4 관측성
- Prometheus 메트릭: 볼륨 수, 용량, 오퍼레이션 지연시간, 에러율
- NodeGetVolumeStats RPC: kubelet이 PVC 사용량(bytes/inodes 사용/가용/용량)을 주기적으로 조회; kubectl top pvc 및 PVC 용량 알림에 필요
- 구조화된 로깅 (JSON, slog)
- CRD status conditions에 상태 반영 (K8s 표준 패턴: type, status, reason, message, lastTransitionTime)
- 트러블슈팅: kubectl describe, events, status conditions로 진단 (별도 도구 없음)

## 9. 기술 스택

| 구성요소 | 기술 |
|---------|------|
| 언어 | Go 1.26+ |
| CSI spec | v1.12.0 |
| gRPC | google.golang.org/grpc + protobuf |
| CRD 프레임워크 | controller-runtime (kubebuilder) |
| CLI | cobra |
| 로깅 | slog (stdlib) |
| 메트릭 | prometheus/client_golang |
| 빌드 | goreleaser + ko (컨테이너 이미지) |
| Helm | Helm 3 chart |
| CI | GitHub Actions |

## 10. 용어 정의

| 용어 | 설명 |
|------|------|
| **Backend** | 스토리지 노드에서 볼륨을 생성/관리하는 방법 (ZFS, LVM 등) |
| **Protocol** | 볼륨을 네트워크로 공유하는 방법. 블록(NVMe-oF, iSCSI)과 파일(NFS, SMB) 두 카테고리 |
| **PillarTarget** | 사용자가 생성하는 스토리지 agent 인스턴스 정의. 노드 위치 + agent 상태 |
| **PillarPool** | PillarTarget의 특정 Backend 인스턴스 (예: rock5bp의 hot-data ZFS pool) |
| **PillarProtocol** | 프로토콜 타입과 기본 설정의 재사용 가능한 정의. 노드 무관 |
| **PillarBinding** | PillarPool + PillarProtocol 조합. StorageClass를 자동 생성 |
| **Target** | 스토리지를 네트워크로 내보내는 측 (스토리지 노드, agent가 configfs로 관리) |
| **Initiator** | 네트워크 스토리지에 연결하는 측 (워커 노드, CSI node plugin이 관리) |
| **Agent** | 스토리지 노드의 gRPC 서버. Backend/Protocol target 플러그인. K8s 의존성 없음. Stateless |
| **configfs** | 리눅스 커널 설정 파일시스템. NVMe-oF/iSCSI target을 CLI 없이 직접 제어 |
