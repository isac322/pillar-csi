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

4개의 CRD를 사용한다:

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
  # controller가 agent gRPC 조회 결과로 관리
  connected: true
  agentVersion: "0.1.0"
  resolvedAddress: 192.168.219.6       # resolve된 agent 주소
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
```

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
status:
  ready: true
  capacity:
    total: 712G
    available: 412G
    used: 300G
```

#### PillarProtocol

네트워크 공유 프로토콜의 타입과 기본 설정. **노드와 무관하게 재사용 가능하다.** Target bind IP는 포함하지 않는다 — controller가 런타임에 PillarTarget에서 resolve하여 agent에 전달한다.

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
    allowVolumeExpansion: true
  overrides:
    backend:
      zfs:
        properties:
          volblocksize: 16K            # pool 기본값(8K) 오버라이드
    protocol:
      nvmeofTcp:
        maxQueueSize: 256              # protocol 기본값(128) 오버라이드
```

### 2.2 프로토콜 카테고리와 VolumeMode/AccessMode

프로토콜은 **블록**과 **파일시스템** 두 카테고리로 나뉜다. Kubernetes의 `volumeMode`와 `accessModes`에 직접 매핑된다.

#### 블록 프로토콜

| 프로토콜 | 클라이언트 디바이스 | 기본 AccessMode | volumeMode |
|----------|-----------------|----------------|------------|
| NVMe-oF TCP | `/dev/nvmeXnY` | ReadWriteOnce (RWO) | Block 또는 Filesystem |
| iSCSI | `/dev/sdX` | ReadWriteOnce (RWO) | Block 또는 Filesystem |

- `volumeMode: Filesystem` → 블록 디바이스에 mkfs + mount
- `volumeMode: Block` → raw 블록 디바이스를 Pod에 직접 제공

#### 파일시스템 프로토콜

| 프로토콜 | 클라이언트 마운트 | 기본 AccessMode | volumeMode |
|----------|---------------|----------------|------------|
| NFS | 마운트된 디렉토리 | ReadWriteMany (RWX) | Filesystem만 |
| SMB | 마운트된 디렉토리 | ReadWriteMany (RWX) | Filesystem만 |

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
│  │  • CSI sidecars: provisioner, attacher, resizer,      │ │
│  │    snapshotter                                        │ │
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
│  │  • Init container: 커널 모듈 modprobe                   │ │
│  │  • CSI sidecar: node-driver-registrar                 │ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
│  ┌─ pillar-agent (DaemonSet, 스토리지 노드만) ───────────┐  │
│  │  • gRPC server                                        │ │
│  │  • Backend 플러그인: ZFS, LVM, directory 등            │ │
│  │  • Protocol target 플러그인:                           │ │
│  │    - NVMe-oF: nvmet configfs 직접 조작                 │ │
│  │    - iSCSI: LIO configfs 직접 조작                     │ │
│  │    - NFS: kernel nfsd 설정                             │ │
│  │    - SMB: Samba 설정                                   │ │
│  │  • K8s API 의존성 없음 — 순수 gRPC 서버                  │ │
│  │  • Host network 불필요                                 │ │
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

CLI 도구 없이 **configfs 직접 조작**으로 target을 설정한다:

| Protocol | configfs 경로 | Go 참조 구현 |
|----------|-------------|------------|
| NVMe-oF TCP | `/sys/kernel/config/nvmet/` | `github.com/0xfd4d/nvmet-config` (~150줄) |
| iSCSI LIO | `/sys/kernel/config/target/iscsi/` | `github.com/sapslaj/shortrack` (~1400줄) |
| NFS | `/etc/exports` + `exportfs` | 직접 작성 |

#### Agent 디스커버리

- **K8s 내부** (Phase 1): PillarTarget의 `nodeRef` → K8s Node IP → agent pod IP를 K8s API로 조회
- **K8s 외부** (Phase N): PillarTarget의 `external` → 명시된 address로 직접 연결

#### Agent가 필요한 호스트 권한

| 권한 | 용도 |
|------|------|
| `CAP_SYS_ADMIN` | configfs 조작, ZFS 명령 실행 |
| `CAP_SYS_MODULE` (init container) | 커널 모듈 로드 |
| `/sys/kernel/config` 마운트 | nvmet/LIO configfs 접근 |
| `/dev` 마운트 | 블록 디바이스(zvol 등) 접근 |
| `/lib/modules` 읽기 마운트 (init) | modprobe용 |

**Host network 불필요.** NVMe-oF/iSCSI target은 호스트 커널 레벨 서비스이므로, agent pod 네트워킹 모드와 무관하게 호스트 네트워크에서 listen한다. Target bind IP는 controller가 PillarTarget nodeRef에서 resolve하여 gRPC로 agent에 전달한다.

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

**한계:** 커널 모듈이 커널에 없으면 (예: RPi의 nvme_tcp) modprobe가 실패한다. 이 경우 DKMS 패키지 사전 설치가 필요하다. Init container는 모듈 로딩 실패 시 해당 프로토콜을 capabilities에서 제외하고 계속 동작한다.

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

### 5.1 CreateVolume (PVC 생성)

```
1. PVC 생성
2. external-provisioner → CSI CreateVolume
3. pillar-controller:
   a. PillarBinding에서 poolRef, protocolRef 확인
   b. 파라미터 머지 (Pool → Protocol → Binding → PVC annotation)
   c. Backend-Protocol 호환성 검증
   d. PillarPool → PillarTarget → Node IP resolve
   e. gRPC로 agent에 CreateVolume + ExportVolume 요청
      (target bind IP도 함께 전달)
4. pillar-agent:
   a. Backend: 볼륨 생성 (예: zfs create -V 50G hot-data/k8s/pvc-xxx)
   b. Protocol: 볼륨 export (예: nvmet configfs에 subsystem/namespace/port 생성)
   c. ExportInfo 반환 (NQN, namespace ID 등)
5. pillar-controller:
   a. PV 생성, volumeContext에 ExportInfo 저장
```

### 5.2 ControllerPublishVolume (Pod 스케줄링)

```
1. external-attacher → CSI ControllerPublishVolume
2. pillar-controller:
   a. 대상 노드의 initiator ID 조회 (NodeGetInfo에서 등록된 NQN/IQN)
   b. PillarProtocol의 acl 설정 확인
   c. acl=true: gRPC로 agent에 AllowInitiator 요청
      (NVMe-oF: allowed_hosts에 NQN symlink / iSCSI: ACL에 IQN)
   d. acl=false: no-op
3. publish_context 반환
```

### 5.3 NodeStageVolume

```
1. kubelet → CSI NodeStageVolume
2. pillar-node:
   a. volumeContext + publish_context에서 ExportInfo 추출
   b. Protocol initiator 연결:
      NVMe-oF: nvme connect -t tcp -a <ip> -s <port> -n <nqn>
      iSCSI: iscsiadm -m discovery + login
      NFS: mount.nfs <ip>:<path> <staging>
   c. Block protocol + volumeMode=Filesystem: mkfs (첫 사용) + mount
   d. Block protocol + volumeMode=Block: 디바이스 경로 기록
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
- CRD: PillarTarget, PillarPool, PillarProtocol, PillarBinding
- CRD controller + validation webhook
- pillar-agent: ZFS zvol backend + NVMe-oF TCP target (configfs)
- pillar-node: NVMe-oF TCP initiator + init container modprobe + 도구 번들
- pillar-controller: CSI Controller (CreateVolume, DeleteVolume, ExpandVolume, ControllerPublishVolume/UnpublishVolume)
- CSI Node (Stage/Unstage/Publish/Unpublish)
- NVMe-oF ACL on/off (PillarProtocol acl 필드)
- StorageClass 자동 생성 (PillarBinding reconcile)
- 파라미터 오버라이드 계층 (Pool → Protocol → Binding → PVC annotation)
- volumeMode: Filesystem 지원
- Helm chart
- K8s 내부 노드만

**미포함:** 스냅샷/클론, volumeMode: Block, 다른 backend/protocol, 외부 노드

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

## 7. 비기능 요구사항

### 7.1 성능
- gRPC agent 통신 오버헤드: < 1ms (LAN)
- 볼륨 프로비저닝 시간: < 5초 (ZFS zvol 기준)
- NVMe-oF/iSCSI 데이터 패스에 pillar-csi 오버헤드 없음 (커널 레벨 프로토콜)

### 7.2 안정성
- Agent 연결 끊김 시 gRPC 자동 재연결 (keepalive)
- 볼륨 생성 중 실패 시 정리 (orphan 방지)
- 멱등성: 모든 CSI 오퍼레이션은 멱등적으로 구현
- Agent 크래시 복구: 시작 시 configfs 상태를 기대 상태와 reconcile
- Leader election: controller 고가용성

### 7.3 보안
- Agent ↔ controller 간 mTLS 지원 (선택)
- NVMe-oF/iSCSI ACL (PillarProtocol acl 필드)
- PVC annotation 파라미터 validation (injection 방지)
- RBAC: CRD별 세분화된 권한

### 7.4 관측성
- Prometheus 메트릭: 볼륨 수, 용량, 오퍼레이션 지연시간, 에러율
- 구조화된 로깅 (JSON, slog)
- CRD status 필드에 상태 반영

## 8. 기술 스택

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

## 9. 용어 정의

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
| **Agent** | 스토리지 노드의 gRPC 서버. Backend/Protocol target 플러그인. K8s 의존성 없음 |
| **configfs** | 리눅스 커널 설정 파일시스템. NVMe-oF/iSCSI target을 CLI 없이 직접 제어 |
