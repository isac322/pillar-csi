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
| **멀티 pool** | pool마다 Helm release. SSH 설정, RBAC, 사이드카 모두 중복 | PillarStore CR 하나 추가 |
| **스토리지 노드 통신** | SSH (셸 명령 파싱, 키 관리, 인젝션 위험) | gRPC agent (타입 안전, 자동 재연결) |
| **Target 설정** | targetcli/nvmetcli CLI (Python 의존) | configfs 직접 조작 (의존성 제로) |
| **노드 사전 설치** | 워커 노드에 open-iscsi, nvme-cli 등 필요 | 커널 모듈만 (init container modprobe). NVMe-oF는 `/dev/nvme-fabrics` 직접 쓰기, iSCSI는 pillar-node 내장 initiator — nvme-cli·open-iscsi 불필요 |
| **파라미터 커스터마이징** | StorageClass parameters + PVC annotation | Store/Protocol → Binding → PVC annotation 문서. 모든 계층에서 같은 키·같은 YAML 구조 |
| **프로토콜/백엔드 확장** | 드라이버 타입 하드코딩 (zfs-generic-iscsi 등) | Backend/Protocol 플러그인 아키텍처 |

## 2. 아키텍처

### 2.1 Custom Resource Definitions

API group: `pillar-csi.bhyoo.com`
CSI provisioner name: `pillar-csi.bhyoo.com`

4개의 CRD를 사용한다. **모두 cluster-scoped**이다 (StorageClass와 동일한 인프라 레벨).

#### PillarAgent

스토리지 agent 인스턴스를 나타낸다. **사용자가 생성한다.** Agent의 위치를 정의하고, controller가 agent에 gRPC로 조회한 상태 정보를 status에 반영한다.

`nodeRef`와 `external`은 discriminated union으로, 둘 중 하나만 지정한다.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
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
    backends: [zfs-zvol, zfs-dataset, lvm-lv]
    protocols: [nvmeof-tcp, iscsi, nfs]
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

**PillarAgent conditions:**
| Condition | 의미 |
|-----------|------|
| `NodeExists` | nodeRef의 K8s Node가 존재하는지 |
| `AgentConnected` | agent gRPC 연결 상태 |
| `ExportsReady` | 에이전트의 export 복원이 완료되어 export를 서빙하는지 (`export_restore_pending` 게이트 반영) |
| `Ready` | 전체 준비 상태 (모든 condition True) |

gRPC 주소 결정 로직 (nodeRef):
1. K8s Node `status.addresses`에서 `addressType` 매칭
2. 동일 타입이 여러 개면 `addressSelector` CIDR로 필터
3. 미지정 시 첫 번째 InternalIP 사용

#### PillarStore

특정 target의 특정 스토리지 풀. **storage 축만** 담는다: `spec.backend`는 정확히 하나의 멤버(`zfs` 또는 `lvm`)를 갖는 union이며 `type` 필드는 없다 (CRD CEL이 exactly-one을 강제). **사용자가 생성한다.**

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: rock5bp-hot-data
spec:
  agentRef: rock5bp                   # PillarAgent 참조
  backend:                             # 정확히 하나의 멤버: zfs | lvm
    zfs:
      volumeType: zvol                 # 선택 (기본값·유일한 구현: zvol)
      pool: hot-data
      parentDataset: k8s               # 선택: 생략 시 pool 루트
      properties:                      # 선택: 모든 볼륨에 적용할 ZFS property
        compression: lz4
        volblocksize: 8K
    # 또는
    # lvm:
    #   volumeGroup: data-vg
    #   thinPool: thin0                # 선택
    #   provisioningMode: linear       # linear | thin (기본값: linear)
status:
  capacity:
    total: 712G
    available: 412G
    used: 300G
  conditions:
    - type: TargetReady
      status: "True"
      reason: TargetReady
      message: "PillarAgent rock5bp is Ready"
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

`backend` 멤버 이름은 agent 설정 파일(`--config`)의 `backends` 항목과 같은 키·같은 구조를 쓴다. `zfs.pool`, `zfs.parentDataset`, `lvm.volumeGroup`, `lvm.thinPool`은 구조적 필드이며 PillarStorageClass 오버라이드·PVC annotation에서 설정할 수 없다.

> **현재 branch의 파일시스템 채택 기능:** `directory` backend는 기존 디렉토리 채택 전용으로 구현되었다. `files.pillar-csi.bhyoo.com` CSI identity와 `PillarStorageClass.spec.csiDriver`를 명시해야 하며, chart의 `fileDriver.enabled`도 켜야 한다. `zfs-dataset`은 기존 ZFS filesystem dataset을 같은 파일 identity로 채택할 수 있다. 두 backend 모두 동적 생성, 포맷, 속성·소유권 변경, quota 확장 및 원본 삭제를 수행하지 않는다. Directory는 ext4/XFS의 사전 구성된 project quota를, ZFS dataset은 유한한 유효 quota를 PVC 요청량과 정확히 일치시켜야 한다. 이 기능은 아직 release되지 않았고 기본값은 opt-in이다.

**PillarStore conditions:**
| Condition | 의미 |
|-----------|------|
| `TargetReady` | 참조 PillarAgent이 Ready인지 |
| `PoolDiscovered` | agent에서 해당 pool(ZFS pool / LVM VG)이 발견되었고, agent 설정 파일 `backends` 항목의 `zfs.parentDataset`/`lvm.thinPool`이 store의 `zfs.parentDataset`/`lvm.thinPool`과 일치하는지. 불일치 시 `False`/`BackendLayoutMismatch`이며 agent는 CreateVolume을 `FailedPrecondition`으로 거부한다 (다른 위치에 볼륨을 만들지 않음) |
| `BackendSupported` | backend 타입이 agent capabilities에 있는지 |
| `Ready` | 전체 준비 상태 |

#### PillarProtocol

네트워크 공유 프로토콜과 그 기본 설정. **transport 축만** 담는다: `spec.protocol`은 정확히 하나의 멤버를 갖는 union이며 `type` 필드는 없다. 현재 구현된 멤버는 `nvmeofTcp`, `iscsi`, `nfs`다. NFS는 버전 4.2와 포트 2049만 허용하며 ACL은 기본값 false, squash는 기본값 `root`다. 파일시스템 설정은 여전히 PillarStorageClass `spec.filesystem`과 PVC `filesystem` 문서에서 설정한다. **노드와 무관하게 재사용 가능하다.**

status에는 이 프로토콜을 참조하는 바인딩의 역참조 메타 정보를 포함한다 (`storageClassCount`, `activeAgents`). Reconciler가 자동으로 계산한다.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nvmeof-tcp
spec:
  protocol:                            # 정확히 하나의 멤버: nvmeofTcp
    nvmeofTcp:
      port: 4420                       # 기본값: 4420
      acl: true                        # true: host NQN 기반 ACL / false: allow_any_host (기본값: false)
      # initiator 큐 깊이: pillar-node가 fabrics connect의 queue_size로 적용 (허용 범위 16-1024, 생략 시 커널 기본값 128).
      # PillarStorageClass overrides.protocol.nvmeofTcp.maxQueueSize, PVC pillar-csi.bhyoo.com/protocol 문서로 덮어쓸 수 있다.
      maxQueueSize: 128
      # target port의 in-capsule data size (nvmet ports/<id>/param_inline_data_size, 바이트).
      # 같은 storage node 주소·포트로 export되는 모든 볼륨이 공유하는 포트 속성이며, 커널은 포트에
      # subsystem이 하나라도 링크된 동안 변경을 거부한다(EACCES). 따라서 agent는:
      #   - 링크된 subsystem이 없는 포트: 요청값(없으면 transport 기본값 -1)을 쓰고 read-back 검증한다.
      #   - 이미 사용 중인 포트: 요청값이 포트의 현재 값과 다르면 ExportVolume이 FAILED_PRECONDITION으로
      #     실패한다(조용히 포트 값을 쓰지 않는다). 값을 요청하지 않은 볼륨은 포트의 현재 값을 그대로 쓴다.
      # 같은 포트를 쓰는 PillarProtocol/PillarStorageClass/PVC는 같은 값을 쓰거나 다른 포트를 사용해야 한다.
      # 생략 시 TCP transport 기본값(4 * PAGE_SIZE, 4KiB 페이지에서 16384). 최소값은 1024: NVMe/TCP host는
      # 1024바이트 fabrics Connect 데이터를 항상 in-capsule로 보내므로 더 작은 값이면 모든 connect가 실패한다.
      # agent 재시작 복구는 PillarVolumeState.status.exportSpec.inCapsuleDataSize를 사용하며, 포트에 먼저
      # 링크되는 export가 값을 정하므로 CreateVolume이 완료된 볼륨을 CreatePartial 볼륨보다, 같은 그룹에서는
      # 값을 요구하는 export를 먼저 링크한다. CreatePartial 재시도는 재시도에 쓴 값으로 exportSpec을 갱신한다.
      inCapsuleDataSize: 16384
      # 명령 하나의 최대 데이터 전송 크기(MDTS, 바이트). 0 = 제한 없음, 그 외 8192..2^30의 2의 거듭제곱.
      # 생략 시 CreateVolume에서 4194304(4 MiB)로 resolve된다. 제한이 없으면 최신 host 커널이 32 MiB 명령을 보내고,
      # nvmet_tcp가 명령의 scatterlist를 한 번에 연속 할당(32 MiB → 256 KiB)하다 메모리 단편화로 실패한다.
      #   - target(Linux >= 7.1): agent가 포트의 param_mdts에 log2(size/4096)을 쓰고 read-back 검증한다.
      #     값은 포트 공유 속성이지만 상한이다: 포트가 더 작은 제한을 광고하면 명령이 작아질 뿐이므로 안전하다.
      #     활성 포트에서는 포트가 제한 없음(0, 노드가 직접 제한)이거나 포트 제한 <= 요청(요청 0 = 무제한)이면
      #     받아들이고, 포트 제한이 0이 아닌 요청보다 클 때만 충돌로 실패한다. agent 재시작 복구는 링크 전에
      #     빈 포트를 같은 포트 export들의 0이 아닌 최소값으로 설정하므로 순서와 무관하게 복구된다.
      #     param_mdts가 없는 커널(< 7.1)은 export를 실패시키지 않고 한 번 경고한다.
      #   - node: Identify Controller로 MDTS를 읽어 0이면 namespace 블록 장치(multipath head와 path 장치)의
      #     queue/max_sectors_kb를 size/1024로 쓰고 read-back 검증한다. MDTS가 0이 아니면 아무것도 하지 않는다.
      #     VolumeContext 키가 없는 기존 볼륨은 4 MiB로 취급한다.
      maxDataTransferSize: 4194304
      # initiator 타임아웃/재연결 파라미터 (pillar-node가 nvme connect 시 적용). 모든 계층에서 같은 범위(>= 0)를 쓴다.
      # 생략 시 connect 문자열에서 빠지고 커널 기본값(600/10)이 적용된다.
      # 커널 의미(Linux fabrics.c): ctrlLossTmo=0 → 재연결 시도 없음.
      # reconnectDelay=0은 CRD상 허용되지만 커널이 EINVAL로 거부하므로 NodeStage connect가 명시적으로 실패한다.
      # 값은 CreateVolume 시점에 live CR에서 resolve되어 PillarVolumeState.spec.resolved에 고정된다.
      # 따라서 값을 바꿔도 StorageClass 재생성은 필요 없고, 이후 생성되는 볼륨부터 적용된다 (기존 볼륨에는 소급 적용되지 않는다).
      ctrlLossTmo: 600                 # 초. target 유실 시 최대 대기 시간
      reconnectDelay: 10               # 초. 재연결 시도 간격
status:
  storageClassCount: 2                      # 이 Protocol을 참조하는 PillarStorageClass 수
  activeAgents: [rock5bp]             # 이 Protocol이 사용 중인 Target 목록
```

iSCSI는 구현되어 있다. 같은 union 규칙(`spec.protocol.<member>`, `type` 필드 없음)을 따르며 CEL이 `nvmeofTcp`·`iscsi` 중 정확히 하나를 요구한다. 상세 설계는 [`PRD-iscsi.md`](./PRD-iscsi.md) 참조.

```yaml
# iSCSI 예시
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: iscsi
spec:
  protocol:
    iscsi:
      port: 3260                       # 기본값 3260 (1-65535)
      acl: true                        # 기본값 false (generate_node_acls=1, 모든 initiator 허용)
      # initiator 타임아웃 (초). 생략 시 pillar-node 기본값 적용.
      # 값은 CreateVolume 시점에 PillarVolumeState.spec.resolved에 고정되며 이후 로그인하는 세션부터 적용된다.
      loginTimeout: 15                 # 선택: min 1 (기본값: 15)
      replacementTimeout: 120          # 선택: min 0 (기본값: 120). 세션 복구 중 I/O를 붙잡는 최대 시간
      noopOutInterval: 5               # 선택: min 0 (기본값: 5). 0이면 NOP-Out ping 비활성
      noopOutTimeout: 5                # 선택: min 0 (기본값: 5)
      auth:                            # 선택: 생략 시 method None
        method: CHAP                   # None | CHAP | MutualCHAP. CHAP 계열은 acl: true 필요
        secretRef:
          name: iscsi-chap             # 설치 네임스페이스의 Secret (username/password, MutualCHAP은 mutualUsername/mutualPassword 추가)
```

`port`·`acl`·`auth`는 구조적 필드라 `PillarStorageClass.spec.overrides.protocol.iscsi`와 PVC annotation `pillar-csi.bhyoo.com/protocol`(예: `iscsi: {loginTimeout: 30}`)에서는 네 타임아웃만 허용된다. `auth.method`가 `None`이 아니면 생성된 StorageClass에 `csi.storage.k8s.io/node-stage-secret-name`/`-namespace`가 붙어 kubelet이 같은 Secret을 NodeStage에 넘긴다. CHAP Secret 규칙, 교체 의미와 보안 주의는 [`PRD-iscsi.md`](./PRD-iscsi.md) §8.6.1을 따른다.

NFS는 구현되었고 SMB는 아직 구현되지 않았다. NFS는 ZFS dataset backend와만 호환되며 NFSv4.2/2049를 사용한다. 아래는 동작하는 NFS protocol 예시다.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: nfs42
spec:
  protocol:
    nfs:
      version: "4.2"
      port: 2049
      acl: true
      squash: root
```

#### PillarStorageClass

PillarStore과 PillarProtocol을 조합하여 Kubernetes StorageClass를 자동 생성한다. **filesystem 축**(`spec.filesystem`)과 바인딩별 backend·protocol 오버라이드(`spec.overrides`)를 담는다. **사용자가 생성한다.**

호환되지 않는 조합(Block backend + File protocol, 또는 Filesystem backend + Block protocol)은 validation webhook이 거부한다. 기본 CSI identity에서 구현된 조합은 `zfs` zvol/`lvm` × `nvmeofTcp`/`iscsi`, 그리고 `zfs` dataset × `nfs`다. 이 branch의 opt-in file CSI identity는 기존 `directory` × `nfs`와 기존 `zfs` filesystem dataset 채택도 지원한다.

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: fast-nvmeof
spec:
  storeRef: rock5bp-hot-data
  protocolRef: nvmeof-tcp
  storageClass:
    name: fast-nvmeof
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true          # 선택: 미지정 시 backend capability에서 자동 결정
  filesystem:                           # 선택: 블록 프로토콜 + volumeMode: Filesystem일 때만 적용
    fsType: ext4                        # ext4(기본값) | xfs
    mkfsOptions: ["-E", "lazy_itable_init=1"]  # 선택: mkfs 추가 옵션
    mountOptions: [noatime]             # 선택: 생성되는 StorageClass의 mountOptions
    periodicTrim: true                  # 선택: false면 노드의 주기적 filesystem trim에서 제외 (생략 = 활성)
  localAttach: false                    # 선택: true면 스토리지 노드의 파드가 백엔드 디바이스를 직접 attach (아래 "로컬 attach" 참조)
  overrides:                            # 선택: 튜닝 가능한 부분집합만 허용
    backend:                            # 정확히 하나의 멤버, store의 backend와 같은 멤버여야 한다
      zfs:
        properties:
          volblocksize: 16K             # store 값(8K) 오버라이드
    protocol:                           # 정확히 하나의 멤버, protocol의 멤버와 같아야 한다
      nvmeofTcp:
        maxQueueSize: 256               # protocol 값(128) 오버라이드
status:
  storageClassName: fast-nvmeof
  conditions:
    - type: PoolReady
      status: "True"
      reason: PoolReady
      message: "PillarStore rock5bp-hot-data is Ready"
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

**PillarStorageClass conditions:**
| Condition | 의미 |
|-----------|------|
| `PoolReady` | 참조 PillarStore이 Ready인지 |
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

NFS는 구현된 파일 프로토콜이며 SMB는 설계 노트로만 남아 있다.

| 프로토콜 | 클라이언트 마운트 | AccessMode | volumeMode |
|----------|---------------|------------|------------|
| NFS | NFSv4.2 mounted dataset or adopted filesystem proxy | RWX, RWO, RWOP, ROX | Filesystem만 |
| SMB | 마운트된 디렉토리 (미구현) | 설계상 RWX, RWO, ROX | Filesystem만 |

NFS는 `filesystem.mountOptions`만 허용한다. `fsType`, `mkfsOptions`, periodicTrim, Block volume mode와 support defaults를 뒤집는 `nfsvers`/`proto`/`soft` 옵션은 거부한다. 기본 CSI identity에서는 `localAttach`도 거부하고 NFS network mount를 사용한다. file CSI identity의 existing-filesystem adoption은 `localAttach: true` direct single-node mount와 `localAttach: false` owned-host NFS/RWX를 지원한다. `squash` 기본값은 `root`; root/fsGroup 초기화가 필요한 workload는 `squash: none`을 명시해야 한다. RPC TLS는 제공하지 않는다.

#### Backend-Protocol 호환성 매트릭스

|  | NVMe-oF TCP | iSCSI | NFS | SMB |
|--|:---:|:---:|:---:|:---:|
| **zfs-zvol** (Block) | O | O | - | - |
| **zfs-dataset** (FS) | - | - | O | - |
| **lvm** (Block) | O | O | - | - |
| **block-device** (Block, 미구현) | - | - | - | - |
| **directory** (FS, file CSI existing adoption) | - | - | O* | - |

기본 CSI identity에서 served schema의 구현 조합은 **zfs-zvol·lvm × NVMe-oF TCP·iSCSI**와 **zfs-dataset × NFS**다. 이 branch의 opt-in file CSI identity는 기존 `directory`와 기존 `zfs-dataset`을 채택하며 NFS를 사용한다. SMB와 block-device는 미구현이다. `O*`는 기존 filesystem 채택 전용이며 동적 생성은 하지 않는다.

### 2.3 파라미터 오버라이드 계층

storage·protocol·filesystem 세 축의 튜닝 파라미터를 **PVC 단위까지 세밀하게 커스터마이징**할 수 있다. 하나의 설정은 설정할 수 있는 모든 위치에서 **같은 키 이름과 같은 YAML 구조**를 쓰며, 한 축의 설정은 다른 축의 리소스나 키에 두지 않는다. 뒤의 계층이 앞의 계층을 덮어쓴다.

```
PillarStore.spec.backend / PillarProtocol.spec.protocol   (기본값)
  ↓ 오버라이드
PillarStorageClass spec.overrides.{backend,protocol} + spec.filesystem   (바인딩별)
  ↓ 오버라이드
수동 StorageClass의 backend/protocol/filesystem 문서 파라미터   (PillarStorageClass 없이 쓰는 경우만)
  ↓ 오버라이드
PVC annotation 문서 pillar-csi.bhyoo.com/{backend,protocol,filesystem}   (볼륨별)
```

| 축 | 기본값 | 바인딩 (PillarStorageClass) | 볼륨 (PVC annotation = 수동 SC 파라미터) |
|----|--------|---------------------------|----------------------------------------|
| storage | `PillarStore.spec.backend.{zfs,lvm}` | `spec.overrides.backend.{zfs,lvm}` | `pillar-csi.bhyoo.com/backend` |
| protocol | `PillarProtocol.spec.protocol.{nvmeofTcp,iscsi}` | `spec.overrides.protocol.{nvmeofTcp,iscsi}` | `pillar-csi.bhyoo.com/protocol` |
| filesystem | — (fsType 기본값 ext4) | `spec.filesystem` | `pillar-csi.bhyoo.com/filesystem` |

오버라이드 가능 항목 (튜닝 부분집합):

| 문서 | 허용 키 | 병합 규칙 |
|------|---------|-----------|
| backend | `zfs.properties` (compression, volblocksize 등) | 키 단위 병합. 우선순위: PVC > 바인딩 > store |
| backend | `lvm.provisioningMode` (`linear` \| `thin`, 기본값 linear) | 마지막 계층의 값 |
| protocol | `nvmeofTcp.maxQueueSize` (16-1024), `inCapsuleDataSize` (>= 1024), `maxDataTransferSize` (0 또는 8192-2^30의 2의 거듭제곱, 기본값 4 MiB), `ctrlLossTmo` (>= 0), `reconnectDelay` (>= 0) | 필드 단위, 마지막 계층의 값 |
| protocol | `iscsi.loginTimeout` (>= 1), `replacementTimeout` (>= 0), `noopOutInterval` (>= 0), `noopOutTimeout` (>= 0) | 필드 단위, 마지막 계층의 값 |
| filesystem | `fsType` (`ext4` \| `xfs`) | 마지막 계층의 값 |
| filesystem | `periodicTrim` (bool, 생략 = 활성) | 마지막 계층의 값 |
| filesystem | `mkfsOptions`, `mountOptions` | 생략 = 상속, 명시적 `[]` = 비움, 값 = 교체 (모든 계층 동일) |

backend·protocol 문서는 exactly-one union이다: 정확히 하나의 멤버만 쓸 수 있고, 그 멤버는 store의 backend(`zfs`/`lvm`, file CSI에서는 `directory` 포함)·protocol(`nvmeofTcp`/`iscsi`/`nfs`)과 같아야 한다. 기본 CSI identity의 NFS는 `zfs.volumeType: dataset` backend에서만 허용되며, file CSI identity에서는 기존 directory adoption에도 사용할 수 있다. version/port/ACL/squash는 structural fields다. 같은 수치 범위와 기본값(ACL 기본값 false, NFS version 4.2/port 2049/squash root, LVM provisioningMode 기본값 linear)이 모든 계층에 적용된다.

**구조적 필드·알 수 없는 키 거부:** PVC annotation·수동 SC 문서에서는 튜닝 부분집합만 허용한다. 구조적 필드(`zfs.pool`, `zfs.parentDataset`, `zfs.volumeType`, `lvm.volumeGroup`, `lvm.thinPool`, `nvmeofTcp.port`, `nvmeofTcp.acl`, `iscsi.port`, `iscsi.acl`, `iscsi.auth`, `nfs.version`, `nfs.port`, `nfs.acl`, `nfs.squash`)와 알 수 없는 키는 하나의 공유 decoder가 전체 경로와 함께 거부한다.

fsType/mkfsOptions 전달 규칙:
- CreateVolume은 resolve된 fsType을 PV VolumeContext `pillar-csi.bhyoo.com/fs-type`에, mkfsOptions를 `pillar-csi.bhyoo.com/mkfs-options`(JSON 문자열 배열)에 기록한다. PVC `filesystem` 문서가 클래스의 mountOptions를 바꾼 경우에만 `pillar-csi.bhyoo.com/mount-options`(JSON 문자열 배열)를 기록한다.
- NodeStageVolume은 디바이스에 파일시스템이 없을 때만(blkid 기준) mkfs를 실행하며, 이미 포맷된 볼륨은 절대 재포맷하지 않는다. mkfs 인자는 셸 없이 argv 요소 그대로 전달된다. 기본 인자(ext4: `-F -m0`) 뒤에 붙으므로 같은 옵션을 지정하면 사용자 값이 우선한다. mkfs 종료 후 blkid로 요청한 파일시스템이 생성되었는지 확인하고, 아니면 (옵션 없이 다시 포맷하지 않고) 실패한다.
- 포맷 타입 우선순위: PVC `filesystem` 문서 fsType > 수동 SC `filesystem` 문서 fsType > PillarStorageClass `spec.filesystem.fsType` > ext4. 생성된 StorageClass는 바인딩의 fsType(기본값 ext4)을 `csi.storage.k8s.io/fstype`으로 싣는다. 수동 SC가 `csi.storage.k8s.io/fstype`과 fsType이 있는 `filesystem` 문서를 함께 쓰면 두 값이 같아야 한다 (다르면 `InvalidArgument`). external-provisioner는 PV fsType을 StorageClass에서만 채우므로 PVC fsType을 쓰면 PV의 `spec.csi.fsType`은 클래스 값으로 남는다. 노드는 포맷한 타입을 스테이지 상태 파일에 기록하고, VolumeContext를 받지 않는 NodeExpandVolume은 이 값으로 resize 도구를 고른다.
- mkfsOptions는 파일시스템별 허용 목록(allowlist)만 받는다. ext4: `-b -C -D -e -E(허용 서브옵션) -F -g -G -i -I -j -J(size,fast_commit_size,location) -L -m -M -N -o -O(journal_dev 제외) -q -r -T -U -v`, xfs: `-b -d -i -l -m -n -s`(각각 허용 서브옵션) `-f -K -L -q`. 다른 파일/디바이스를 여는 옵션(`-J device=`(LABEL=/UUID= 포함), `-l logdev=`, `-r rtdev=`, `-d name=/file=`, ext4 `-d`/`-l`/`-z`, xfs `-p`/`-c`), 파일시스템을 만들지 않거나 다른 결과를 내는 옵션(ext4 `-n`/`-S`/`-V`/`-t`/`-E offset=`, xfs `-N`), 위치 인자·긴 옵션·묶인 플래그(`-Fq`)는 거부된다.
적용될 수 없는 설정은 CreateVolume이 `InvalidArgument`로 거부한다: 잘못된 YAML 문서, 알 수 없는 키·구조적 필드, 허용 목록 밖 mkfs 옵션, ext4/xfs 이외의 fsType, 기본 CSI identity NFS의 fsType/mkfsOptions/periodicTrim/localAttach, NFS와 Block mode, 또는 support defaults를 뒤집는 NFS mountOptions. file CSI의 existing-filesystem adoption은 source를 포맷하지 않으며 별도 localAttach/NFS 경로를 사용한다. 클래스 수준 mkfsOptions는 Block 볼륨에서 `csi.storage.k8s.io/fstype`처럼 무시된다.
`periodicTrim`은 block filesystem에서만 의미가 있다. NFS volume은 periodic trim을 명시하면 거부한다.

**해석 방식 (단일 resolve 지점):** 유효 설정은 CreateVolume에서 한 번만, live CR로부터 resolve한다.

1. identity: StorageClass 파라미터 `pillar-csi.bhyoo.com/storage-class`가 있으면 그 PillarStorageClass에서 store·protocol 이름, `spec.overrides`, `spec.filesystem`을 얻는다. 없으면(수동 StorageClass) `pillar-csi.bhyoo.com/store-ref`·`pillar-csi.bhyoo.com/protocol-ref`로 CR을 찾고 `backend`/`protocol`/`filesystem` 문서 파라미터를 바인딩 대신 쓴다.
2. backend: `PillarStore.spec.backend`를 복사한 뒤 바인딩 오버라이드 → 수동 SC 문서 → PVC 문서 순으로 적용한다. protocol과 filesystem도 같은 순서다.
3. PVC는 csi-provisioner `--extra-create-metadata`(차트 기본값)가 전달하는 `csi.storage.k8s.io/pvc/name`·`pvc/namespace`로 조회한다.
4. 결과(`ResolvedVolumeConfig{backend, protocol, filesystem}`)를 `PillarVolumeState.spec.resolved`에 저장한다. 재시도와 복구는 저장된 값을 다시 쓰므로, 도중에 CR이 바뀌어도 한 볼륨의 설정은 바뀌지 않는다.

StorageClass가 가리키는 PillarStorageClass·PillarStore·PillarProtocol·PVC가 없으면 FailedPrecondition, 조회가 실패하면 Internal로 CreateVolume이 실패하며(provisioner가 재시도) 설정을 버린 채 볼륨을 만들지 않는다.

생성되는 StorageClass에는 identity 참조와 Kubernetes가 직접 쓰는 값만 들어간다: `pillar-csi.bhyoo.com/storage-class`(PillarStorageClass 이름), `csi.storage.k8s.io/fstype`, 그리고 `mountOptions` 필드. 튜닝 값은 CreateVolume에서 live로 resolve하므로 튜닝 값을 바꿔도 StorageClass를 다시 만들 필요가 없다.

StorageClass 파라미터 (`pillar-csi.bhyoo.com/` 접두사):

| 키 | 작성 주체 | 의미 |
|----|----------|------|
| `pillar-csi.bhyoo.com/storage-class` | 생성된 SC | PillarStorageClass 이름 (identity 참조) |
| `pillar-csi.bhyoo.com/store-ref` | 수동 SC | PillarStore 이름 (identity 참조) |
| `pillar-csi.bhyoo.com/protocol-ref` | 수동 SC | PillarProtocol 이름 (identity 참조) |
| `pillar-csi.bhyoo.com/backend` | 수동 SC | backend 오버라이드 YAML 문서 |
| `pillar-csi.bhyoo.com/protocol` | 수동 SC | protocol 오버라이드 YAML 문서 |
| `pillar-csi.bhyoo.com/filesystem` | 수동 SC | filesystem YAML 문서 |
| `pillar-csi.bhyoo.com/local-attach` | 수동 SC | `"true"` \| `"false"` (기본 false). 바인딩의 `spec.localAttach`와 같은 의미. 다른 값은 `InvalidArgument` |
| `csi.storage.k8s.io/fstype` | 생성된 SC, 수동 SC | PV fsType (생성된 SC: 바인딩 `spec.filesystem.fsType`, 기본값 ext4) |
| `csi.storage.k8s.io/node-stage-secret-name` / `-namespace` | 생성된 SC(iSCSI `auth.method`가 CHAP·MutualCHAP일 때), 수동 SC | kubelet이 NodeStage에 넘길 CHAP Secret (프로토콜의 `secretRef.name`, 설치 네임스페이스) |

그 밖의 `pillar-csi.bhyoo.com/` 파라미터 키는 `unsupported StorageClass parameter "<key>"`로 `InvalidArgument` 거부된다.

수동 StorageClass 예시:
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: manual-nvmeof
provisioner: pillar-csi.bhyoo.com
parameters:
  pillar-csi.bhyoo.com/store-ref: rock5bp-hot-data
  pillar-csi.bhyoo.com/protocol-ref: nvmeof-tcp
  pillar-csi.bhyoo.com/backend: |
    zfs:
      properties:
        compression: lz4
  pillar-csi.bhyoo.com/filesystem: |
    fsType: xfs
  csi.storage.k8s.io/fstype: xfs          # filesystem 문서의 fsType과 같아야 한다
```

PVC annotation 예시:
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
  annotations:
    pillar-csi.bhyoo.com/backend: |
      zfs:
        properties:
          volblocksize: "8K"
          compression: zstd
    pillar-csi.bhyoo.com/protocol: |
      nvmeofTcp:
        maxQueueSize: 64
    pillar-csi.bhyoo.com/filesystem: |
      fsType: xfs
      mkfsOptions: ["-K"]
      mountOptions: []                   # 명시적 [] = 클래스 mountOptions 비움
spec:
  storageClassName: fast-nvmeof
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 50Gi
```

#### 로컬 attach (`localAttach`)

`localAttach`는 CreateVolume에서 resolve되어 `PillarVolumeState.spec.resolved.localAttach`에 고정된다. 기본 CSI identity의 NFS에서는 항상 거부된다. 생성된 StorageClass에는 들어가지 않고 컨트롤러가 바인딩의 `spec.localAttach`를 읽는다. 기본 CSI identity의 Block volume `ControllerPublishVolume`은 다음을 모두 만족할 때만 로컬 attach를 고른다. file CSI의 existing-filesystem adoption은 `localAttach: true`에서 direct single-node mount를 별도로 사용한다.

- 대상 노드가 볼륨 PillarAgent의 `spec.nodeRef.name`이다 (`spec.external` 에이전트는 해당 없음).
- access mode가 `SINGLE_NODE_*`이다 (multi-node 모드는 항상 프로토콜).

기본 CSI identity의 NFS는 storage node에서도 network NFS mount를 사용한다. file CSI의 `localAttach: true` adoption은 direct host mount를 사용하고, `localAttach: false`와 multi-node access는 owned-host NFS mount를 사용한다.
기본 CSI identity의 Block 로컬 publish는 PublishContext에 `pillar-csi.bhyoo.com/attach-mode: local`, `pillar-csi.bhyoo.com/local-node`, `pillar-csi.bhyoo.com/local-device-path`를 싣는다. NodeStageVolume은 프로토콜 connector를 호출하지 않고 백엔드 디바이스 위에 device-mapper linear 디바이스 `pillar-local-<sha256(volumeID) 앞 16 hex>`를 만들어 그 위에 마운트한다 (block 볼륨은 dm 디바이스를 bind). 파일 adoption의 direct mount는 이 block device-mapper 경로를 사용하지 않는다.

기본 CSI identity의 Block volume은 Kubernetes force-detach ... 포함해 어떤 경우에도 두 노드에서 동시에 쓰이지 않는다. file CSI의 `ReadWriteMany` adoption은 owned-host NFS를 통해 여러 노드의 동시 접근을 지원하며, source identity와 export fencing은 별도로 유지한다.

1. 로컬 publish는 먼저 에이전트의 `SetLocalAttach(local=true)`로 모든 원격 initiator에 대해 export를 끈다 (NVMe-oF namespace `enable=0`, read-back 확인). 남아 있던 원격 세션은 I/O를 할 수 없다.
2. 컨트롤러는 로컬 publication을 예약하는 같은 CAS에서 `status.localAttachNode`를 기록한다. export resync는 `ExportDesiredState.local_attach`로 이 값을 보내 에이전트 재시작·재부팅 후에도 namespace를 꺼진 상태로 복원한다.
3. 로컬 stage는 백엔드 디바이스에 커널 exclusive claim(dm 디바이스)을 남긴다. 이 claim은 node plugin이나 kubelet이 죽어도 남고, 실제 unstage에서만 사라진다.
4. 노드는 claim을 먼저 잡는다: 로컬 stage에서 dm claim을 잡은 뒤 해당 볼륨 subsystem의 모든 nvmet namespace가 `enable=0`인지 읽고, 켜진 namespace가 하나라도 있으면 물러나 stage를 `FailedPrecondition`으로 거부한다. claim 이후 어느 단계에서든 stage가 실패하면 staged surface를 먼저 unmount한 뒤 claim을 푼다 (unmount가 실패하면 claim을 유지한다). stage 기록 없이 들어온 unstage도 남은 `pillar-local-*` claim을 제거한다.
5. 에이전트는 자기 claim 안에서만 enable한다: `SetLocalAttach(local=false)`·Reconcile·Prepare가 namespace를 켤 때 백엔드 디바이스를 `O_EXCL`로 열어 `enable=1` 쓰기와 read-back이 끝날 때까지 유지한 뒤 닫는다. 디바이스가 이미 잡혀 있으면(스토리지 노드의 dm claim) 거부하고(`SetLocalAttach`는 `FailedPrecondition`, Reconcile은 해당 볼륨 항목의 오류) namespace는 꺼진 채로 둔다. 두 claim은 커널에서 상호 배타이므로, 에이전트가 enable하는 동안 노드는 claim을 잡을 수 없고, 그 뒤에 claim을 잡은 노드는 켜진 namespace를 보고 물러난다. 틈이 없다.
6. localAttach 볼륨의 프로토콜 publish는 `status.localAttachNode`가 비어 있어도 매번 grant 전에 예약 generation으로 `SetLocalAttach(local=false)`를 호출한다 (이미 켜져 있으면 no-op). `status.localAttachNode`가 설정돼 있었다면 성공 후 새 fencing generation을 커밋하는 CAS로 비우고, 그 generation으로 한 번 더 `local=false`를 보낸 뒤 initiator를 grant한다.

namespace가 꺼진 동안에도 ControllerExpandVolume은 백엔드를 키운다 (에이전트는 `revalidate_size`만 건너뛴다). NodeExpandVolume은 dm 테이블을 다시 로드한 뒤 파일시스템을 키운다.

### 2.4 컴포넌트

```
┌─ Kubernetes Cluster ──────────────────────────────────────┐
│                                                           │
│  ┌─ pillar-controller (Deployment, 1 replica) ──────────┐ │
│  │  • CRD reconciler (PillarAgent/Pool/Protocol/        │ │
│  │    Binding)                                           │ │
│  │  • CSI Controller service                             │ │
│  │  • gRPC client → agent 통신                            │ │
│  │  • PillarAgent status 관리                             │ │
│  │  • Target bind IP resolve (PillarAgent → Node IP)    │ │
│  │  • StorageClass 자동 생성 (PillarStorageClass → SC)         │ │
│  │  • 노드 label 관리 (PillarAgent ↔ storage-node)      │ │
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
│  │    - iSCSI: in-process Go initiator (login PDU +      │ │
│  │      NETLINK_ISCSI로 커널 iscsi_tcp에 연결 인계)          │ │
│  │    - NFS: bundled mount helper / umount                      │ │
│  │    - SMB: mount.cifs / umount (미구현)                      │ │
│  │  • 유저스페이스 도구 컨테이너 번들                         │ │
│  │  • Init container: 커널 모듈 modprobe (best-effort)     │ │
│  │  • periodic filesystem trim (FITRIM)                  │ │
│  │  • CSI sidecars: node-driver-registrar, liveness-probe│ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
│  ┌─ pillar-agent (DaemonSet, 스토리지 노드만) ───────────┐  │
│  │  • nodeSelector: pillar-csi.bhyoo.com/agent-node    │ │
│  │  • gRPC server (Phase 1: 평문, TLS 옵션 준비)          │ │
│  │  • controller-pushed desired state plus durable fencing/NFS recovery records │ │
│  │  • Backend 플러그인: ZFS zvol/dataset, LVM; directory 기존 filesystem 채택(file CSI opt-in) │ │
│  │  • Protocol target 플러그인:                           │ │
│  │    - NVMe-oF: nvmet configfs 직접 조작                 │ │
│  │    - iSCSI: LIO configfs 직접 조작                     │ │
│  │    - NFS: kernel nfsd + owned export state             │ │
│  │  • K8s API 의존성 없음 — 순수 gRPC 서버                  │ │
│  │  • hostNetwork: true (nvmet/LIO listener를 호스트 netns에)│ │
│  │  • Init container: target 커널 모듈 modprobe            │ │
│  └───────────────────────────────────────────────────────┘ │
│                                                           │
└───────────────────────────────────────────────────────────┘
```

위 그림의 SMB 경로는 미구현 설계 노트다. `directory` backend는 이 branch에서 file CSI identity를 통한 기존 filesystem 채택 전용으로 구현되었고, 동적 생성은 하지 않는다. 현재 기본 CSI 구현은 ZFS zvol·ZFS dataset·LVM backend와 NVMe-oF TCP·iSCSI·NFS다.

**democratic-csi와의 배포 차이:**
- democratic-csi: backend마다 controller StatefulSet + node DaemonSet = N개 배포
- pillar-csi: controller 1개 + node DaemonSet 1개 + agent DaemonSet 1개 = 항상 3개. Backend/Protocol 추가는 CR만 생성.

### 2.5 스토리지 노드 통신: gRPC Agent

#### Agent 역할

스토리지 노드에서 실행되는 경량 Go 바이너리. **K8s API에 의존하지 않는 순수 gRPC 서버**로, K8s DaemonSet과 외부 standalone 배포에서 **동일한 바이너리**를 사용한다.

Agent는 controller가 원하는 export 상태를 reconcile하는 서버다. configfs와 kernel NFS export state는 재부팅 시 사라지지만, agent는 fencing marks와 NFS recovery state를 기존 hostPath에 보존한다. controller가 authoritative volume/export snapshot을 다시 전달하면 agent가 이를 적용하고, foreign NFS export는 건드리지 않는다.

CLI 도구 없이 **configfs 직접 조작**으로 target을 설정한다:

| Protocol | configfs 경로 | Go 참조 구현 |
|----------|-------------|------------|
| NVMe-oF TCP | `/sys/kernel/config/nvmet/` | `github.com/0xfd4d/nvmet-config` (~150줄) |
| iSCSI LIO | `/sys/kernel/config/target/iscsi/` | 직접 작성 (`internal/agent/lio`: 볼륨당 target 1개, TPG 1, iblock backstore의 LUN 0, network portal 1개) |
| NFS | kernel nfsd + supervised rpc.mountd/exportfs | agent가 소유한 export만 직접 작성·복구 |

#### Agent 설정 파일

Agent가 볼륨을 만들 위치(backend 배치)는 `--config <path>` YAML 파일에서 읽는다. 차트는 `agent.backends` 값을 ConfigMap으로 렌더링해 마운트한다. 각 항목은 `PillarStore.spec.backend`와 같은 키·같은 구조의 union 멤버 하나다 (항목마다 공유 decoder로 검증, 알 수 없는 키·미구현 backend 거부). 기본 CSI identity의 NFS는 `zfs.volumeType: dataset`인 backend에서 사용하고, file CSI identity의 directory adoption도 NFS를 사용할 수 있다.

```yaml
# pillar-agent --config 파일 (차트: agent.backends)
backends:
  - zfs:
      volumeType: zvol                 # 선택 (기본값: zvol)
      pool: hot-data
      parentDataset: k8s               # 선택
  - zfs:
      volumeType: dataset
      pool: hot-data
      parentDataset: k8s
  - lvm:
      volumeGroup: data-vg
      thinPool: thin0                  # 선택
  - directory:
      logicalPool: existing-files
      hostRoot: /srv/pillar
```

- 라우팅 키는 zfs → `pool`과 `volumeType`, lvm → `volumeGroup`, directory → `logicalPool`이다. 같은 `(pool, volumeType)` 또는 pool/VG 이름 충돌은 agent가 시작을 거부하지만 같은 ZFS pool에 zvol·dataset 항목을 함께 둘 수 있다.
- 같은 pool/VG/directory를 쓰는 PillarStore는 `zfs.parentDataset`/`lvm.thinPool`/`directory.hostRoot`를 agent 항목과 같게 선언해야 한다 (불일치 시 `PoolDiscovered=False`/`BackendLayoutMismatch`).
- gRPC listen 주소 기본값은 `:9500`이며 PillarAgent `nodeRef.port`/`external.port`와 차트 `agent.grpcPort` 기본값과 같다.

#### Agent 디스커버리

- **K8s 내부** (Phase 1): PillarAgent의 `nodeRef` → K8s Node `status.addresses`에서 IP 조회 → `<nodeIP>:<port>`로 직접 연결 (DaemonSet `hostPort` 사용, pod IP 조회 불필요)
- **K8s 외부** (Phase N): PillarAgent의 `external` → 명시된 address로 직접 연결

#### Agent DaemonSet 노드 선택

PillarAgent CR 생성 시 controller가 해당 노드에 `pillar-csi.bhyoo.com/agent-node=true` label을 자동 부여한다. Agent DaemonSet은 이 label이 있는 노드에만 스케줄링된다. PillarAgent 삭제 시 label도 자동 제거된다. 사용자는 PillarAgent CR만 만들면 agent가 자동으로 배포된다.

#### Agent 크래시/리부트 복구

Agent 재시작이나 노드 리부트 후 configfs와 NFS export state는 비어 있으므로 controller가 해당 target의 모든 볼륨 + export 상태를 gRPC로 push한다. Agent는 받은 상태를 configfs(nvmet/LIO)와 owned NFS export manager에 다시 적용(reconcile)한다. reconcile은 목록에 있는 볼륨의 export만 수정하며, 목록에 없는 export는 fencing이 적용된 UnexportVolume/DeleteVolume으로만 제거된다. NFS ownership을 확립할 수 없으면 availability를 보고하지 않는다.

#### Agent가 필요한 호스트 권한

| 권한 | 용도 |
|------|------|
| `CAP_SYS_ADMIN` | configfs 조작, ZFS 명령 실행 |
| `CAP_SYS_MODULE` (init container) | 커널 모듈 로드 |
| `/sys/kernel/config` 마운트 | nvmet/LIO configfs 접근 |
| `/dev` 마운트 | 블록 디바이스(zvol 등) 접근 |
| `/lib/modules` 읽기 마운트 (init) | modprobe용 |
| `hostPort: 9500` (DaemonSet) | agent gRPC 서버를 노드 IP로 노출 |
| `hostNetwork: true` (agent + node DaemonSet) | NVMe-oF/iSCSI target listener와 initiator를 호스트 netns에 바인딩 |

**hostNetwork 필수.** 커널의 `nvmet_tcp`는 listening socket을, `nvme-fabrics`는 outbound TCP 연결을 **configfs/`/dev/nvme-fabrics`에 쓴 프로세스의 network namespace에 바인딩한다.** LIO `iscsi_target_mod`의 network portal도 configfs를 쓴 agent의 netns에 listener를 만든다. Agent/node DaemonSet을 `hostNetwork: false`로 두면 listener는 agent pod netns에, initiator의 SYN은 node pod netns에서 출발하기 때문에 두 netns 간 격리로 인해 데이터 플레인이 동작하지 않는다 (`NodeStageVolume`에서 `connection refused`). 이는 Kind뿐 아니라 bare-metal에서도 동일하게 재현되며, democratic-csi, OpenEBS Mayastor, Lightbits, NetApp Trident 등 모든 메이저 NVMe-oF/iSCSI CSI 드라이버가 agent + node DaemonSet 둘 다 `hostNetwork: true`로 운용한다. 트레이드오프는 호스트 포트 점유 및 NetworkPolicy 미적용이며, 그 외에 데이터 플레인을 동작시킬 방법이 없으므로 업계 전체가 수용하는 표준 구성이다.

iSCSI initiator는 추가로 `NETLINK_ISCSI` 소켓이 필요한데, 커널은 이 소켓을 init network namespace에만 만든다(`scsi_transport_iscsi.c`의 `netlink_kernel_create(&init_net, NETLINK_ISCSI, ...)`). 따라서 pillar-node도 `hostNetwork: true`여야 한다. Kind처럼 노드 자체가 컨테이너인 환경에서는 차트 값 `node.iscsi.netlinkNetnsPath`(예: `/host/proc/1/ns/net`)가 pillar-node에 `--iscsi-netlink-netns`를 넘기고, pillar-node는 그 netns에서만 netlink 소켓을 연다 (TCP 소켓은 pod netns에 남는다).

Target bind IP는 controller가 PillarAgent nodeRef에서 resolve하여 gRPC로 agent에 전달한다.

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
| nvme-cli | 불필요 | pillar-node가 `/dev/nvme-fabrics`에 직접 connect 문자열을 쓴다 |
| open-iscsi (iscsiadm, iscsid) | 불필요 | pillar-node 안의 pure-Go initiator가 login PDU를 직접 주고받고 연결을 `NETLINK_ISCSI`로 커널 `iscsi_tcp`에 넘긴다. 세션 복구(재로그인)도 pillar-node가 한다. 유저스페이스 도구·데몬 번들 없음 |
| nfs-common (mount.nfs) | O | pillar-node 컨테이너에 포함 |
| cifs-utils (mount.cifs) | O | 미구현 |
| mkfs/리사이즈 도구 (e2fsprogs, xfsprogs, xfsprogs-extra) | O | pillar-node 컨테이너에 포함 |
| **커널 모듈 (initiator)** | | |
| nvme_tcp, nvme_fabrics | X | init container modprobe |
| iscsi_tcp (libiscsi, libiscsi_tcp, scsi_transport_iscsi를 끌어옴) | X | 동일 |
| nfs, nfsv4 | X | init container modprobe 또는 kernel built-in |
| cifs | X | 미구현 |
| **커널 모듈 (target)** | | |
| nvmet, nvmet_tcp | X | agent init container modprobe |
| target_core_mod, target_core_iblock, iscsi_target_mod | X | 동일 |
| **Target CLI 도구** | | |
| targetcli, nvmetcli | 불필요 | configfs 직접 조작으로 대체 |

iSCSI initiator IQN은 호스트 `/etc/iscsi/initiatorname.iscsi`의 `InitiatorName=`에서 읽는다 (hostPath `/etc/iscsi`, `DirectoryOrCreate`). 파일이 없으면 `iqn.2026-01.com.bhyoo.pillar-csi:node.<32 hex>`를 생성해 저장하고, CSINode annotation `pillar-csi.bhyoo.com/iscsi-initiator-iqn`으로 게시한다. 호스트에서 open-iscsi의 `iscsid`가 함께 돌아도 pillar-node는 target IQN이 pillar prefix이고 initiator 이름이 노드 IQN인 세션만 관리하므로 공존한다.

**modprobe 실패 정책:** Init container는 best-effort로 modprobe를 실행한다. 실패해도 pod 시작을 차단하지 않는다.
- **pillar-agent:** 모듈 로딩 실패 시 해당 프로토콜을 capabilities에서 제외하고 계속 동작. PillarAgent status에 반영. iSCSI는 LIO iSCSI fabric(`/sys/kernel/config/target/iscsi`)을 쓸 수 있을 때만 보고한다.
- **pillar-node:** 모듈 로딩 실패 시 pod은 정상 시작. 해당 프로토콜의 볼륨 마운트 요청이 오면 NodeStageVolume에서 명확한 에러 메시지 반환 (예: "nvme_tcp module not available on this node"). iSCSI는 시작 시 `iscsi_tcp`가 없으면 initiator를 끄고 IQN을 게시하지 않으며, iscsi 볼륨의 NodeStage는 명시적 에러로 실패한다 (모듈 로드 후 pillar-node 재시작 필요).

**한계:** 커널 모듈이 커널에 빌드되지 않은 경우 (예: RPi의 nvme_tcp) modprobe가 실패한다. 이 경우 DKMS 패키지 사전 설치가 필요하다.

### 2.7 ControllerPublishVolume — 접근 제어

CSI `ControllerPublishVolume`/`ControllerUnpublishVolume` RPC를 구현하여 볼륨 접근 제어를 설정한다. ACL 사용 여부는 PillarProtocol에서 설정한다.

| Protocol | ACL 메커니즘 | acl: true | acl: false |
|----------|------------|-----------|------------|
| NVMe-oF TCP | `allowed_hosts` symlink | host NQN 추가/제거 | `attr_allow_any_host=1` |
| iSCSI | LIO node ACL (`tpgt_1/acls/<IQN>`, LUN 0 매핑; CHAP이면 ACL `auth/`에 자격 증명) | initiator IQN 추가/제거 | `generate_node_acls=1` (demo mode) |
| NFS | export client list | client IP 추가/제거 | reachable clients 허용 |

`acl: false`이면 ControllerPublish/Unpublish는 block protocols에서 no-op이며 NFS에서는 export client restriction을 생략한다. ACL은 암호화가 아니며 NFS RPC TLS는 제공하지 않는다.

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
| **zfs-dataset** | Filesystem | `zfs create` + quota | ZFS dataset mountpoint | X (snapshot/clone not yet) | O (quota) | X (snapshot/clone not yet) |
| **lvm** | Block | `lvcreate` | `/dev/vg/lv` | O (thin) | O | O (thin) |
| **block-device** (미구현) | Block | 기존 디바이스 사용 | `/dev/sdX` | X | X | X |
| **directory** (file CSI opt-in) | Filesystem | 기존 디렉토리 채택 | `/path/to/dir` | X | X | X |

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

| | NVMe-oF TCP | iSCSI | NFS | SMB (미구현) |
|--|--|--|--|--|
| **Target 구현** | nvmet configfs | LIO configfs (`/sys/kernel/config/target/iscsi`, targetcli 없음) | kernel nfsd + supervised rpc.mountd/exportfs; agent-owned exports only | Samba |
| **Initiator 구현** | `/dev/nvme-fabrics` 직접 쓰기 (nvme-cli 없음) | pillar-node 내장 pure-Go initiator (login PDU + `NETLINK_ISCSI` 인계, iscsiadm/iscsid 없음) | bundled mount helper | mount.cifs |
| **Initiator ID** | NQN (`/etc/nvme/hostnqn`) | IQN (`/etc/iscsi/initiatorname.iscsi`, 없으면 생성) | client InternalIP | Client IP |
| **Target ID** | NQN | IQN | server export path (public fsid 없음) | - |
| **기본 포트** | 4420 | 3260 | 2049 (fixed) | 445 |
| **커널 모듈 (target)** | nvmet, nvmet_tcp | target_core_mod, target_core_iblock, iscsi_target_mod | nfsd | (user-space) |
| **커널 모듈 (initiator)** | nvme_tcp, nvme_fabrics | iscsi_tcp | nfs client | cifs |
| **인증/접근제어** | host NQN ACL | IQN ACL + CHAP | node IP ACL, squash root/none/all | - |
| **미지원** | - | multipath(다중 portal) | RPC TLS, localAttach, Block, mkfs, dir backend | Entire protocol |

## 5. 볼륨 생명주기

### Volume ID 형식

구조화된 ID: `<target>/<pool>/<volume-name>` (슬래시 구분). Controller가 ID만 보고 어떤 agent에 요청할지 라우팅할 수 있다.

```
Volume ID: rock5bp/hot-data/pvc-abc123

파싱:
  target: rock5bp       → PillarAgent 참조 → agent gRPC 주소
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
   a. StorageClass의 identity 참조 확인 (`storage-class` → PillarStorageClass, 수동 SC는 `store-ref`/`protocol-ref`)
   b. 유효 설정 resolve (store/protocol → 바인딩 → 수동 SC 문서 → PVC 문서, §2.3)
      - PVC 문서는 튜닝 부분집합만 허용, 구조적 필드·알 수 없는 키 거부
      - 결과를 PillarVolumeState.spec.resolved에 저장 (재시도는 저장된 값 재사용)
   c. Backend-Protocol 호환성 검증 (NFS는 zfs dataset만. file CSI adoption은 directory도 허용)
   d. PillarStore → PillarAgent → Node IP resolve
   e. gRPC로 agent에 CreateVolume + ExportVolume 요청
      (NFS는 owned root/child export state와 fixed port/version 포함)
   f. 중간 실패 시 롤백 (예: export 실패 → 생성된 볼륨 삭제)
4. pillar-agent:
   a. Backend: 볼륨 생성 (NFS는 `zfs create` dataset + quota)
   b. Protocol: 볼륨 export (block configfs 또는 NFS owned export state)
   c. ExportInfo 반환 (block target 또는 NFS server export path)
5. pillar-controller:
   a. Volume ID 생성: {target}/{pool}/{volume-name}
   b. PV 생성, volumeContext에 ExportInfo 저장
```

### 5.2 ControllerPublishVolume (Pod 스케줄링)

```
1. external-attacher → CSI ControllerPublishVolume
2. pillar-controller:
   a. Volume ID에서 target/pool 파싱하여 라우팅 대상 결정
   b. 대상 노드의 protocol identity 조회 (block NQN/IQN, NFS InternalIP)
   c. PillarProtocol의 acl 설정 확인
   d. acl=true: gRPC로 agent에 AllowInitiator 요청 (NFS는 numeric client IP)
   e. acl=false: block no-op; NFS unrestricted export policy
3. publish_context 반환
```

### 5.3 NodeStageVolume

```
1. kubelet → CSI NodeStageVolume
2. pillar-node:
   a. volumeContext + publish_context에서 ExportInfo 추출
   b. Protocol initiator 연결:
      NVMe-oF: kernel fabrics connect
      iSCSI: in-process initiator가 login PDU 교환 후 NETLINK_ISCSI로 커널 iscsi_tcp에 연결 인계
      NFS: bundled mount helper로 NFSv4.2/TCP/hard 기본값을 staging에 mount
   c. Block protocol + volumeMode=Filesystem:
      mkfs (디바이스에 파일시스템이 없을 때만, fsType/mkfsOptions 적용) + mount
   d. NFS: dataset은 이미 filesystem이므로 mkfs하지 않고 `filesystem.mountOptions`만 적용
   e. Block protocol + volumeMode=Block: 디바이스 경로 기록
   f. 커널 모듈 미로드 시 명확한 에러 반환
```

이 절차는 기본 CSI identity `pillar-csi.bhyoo.com`의 pillar-node다. file CSI identity `files.pillar-csi.bhyoo.com`의 file node는 `NodeGetCapabilities`에서 `GET_VOLUME_STATS`만 advertise하고 `STAGE_UNSTAGE_VOLUME`·`EXPAND_VOLUME`은 advertise하지 않는다. NodeStageVolume·NodeUnstageVolume·NodeExpandVolume은 `Unimplemented`를 반환하므로 kubelet은 staging path 없이 NodePublishVolume만 호출한다.

### 5.4 NodePublishVolume

```
1. kubelet → CSI NodePublishVolume
2. pillar-node:
   a. volumeMode=Filesystem: staging → pod mount point bind mount
   b. volumeMode=Block: 블록 디바이스를 pod에 device file로 제공
```

file CSI identity의 file node는 stage 단계가 없으므로 NodePublishVolume이 pod target마다 마운트를 직접 만든다. local publish는 agent·controller 소유 proxy를 bind하고, multi-node publish는 owned-host NFS export를 bundled NFSv4.2 helper로 마운트한다. 어느 경로를 쓸지는 controller가 만든 publish context만이 결정하고, 무엇을 마운트할지는 NFS transport hint가 아니라 기록된 native identity가 결정한다. `readonly` publish는 자기 target에만 적용된다. NodeUnpublishVolume은 mount table이 해당 마운트가 이 볼륨의 기록된 publish임을 증명한 뒤에만 요청받은 target을 제거한다. 같은 볼륨의 다른 pod 마운트와, agent·controller 소유인 source·proxy·NFS export는 그대로 유지된다. file node는 publish된 볼륨마다 node state dir(`.../node/files/`) 아래에 immutable 기록을 남기고 마지막 target이 unpublish되면 지운다. 기록의 target 항목은 mount보다 먼저 쓰이므로 mount와 완료 사이에서 죽어도 retry가 수렴할 durable intent가 남는다.

file node는 시작 시 마운트를 스캔하지 않는다. NodePublishVolume·NodeUnpublishVolume·NodeGetVolumeStats가 호출될 때마다 durable 기록을 커널 mount table과 대조한다. 이미 있는 target 마운트는 이 볼륨의 publish임이 증명될 때만 받아들이고, 남의 마운트는 거부한다. 마운트는 publish에서만 새로 만든다. 기록된 target이 마운트돼 있지 않으면 stats는 fail closed로 거부하고, unpublish는 증명된 기존 마운트와 기록된 intent를 제거할 뿐 마운트를 다시 만들지 않는다. remote target은 mount-table identity(기록된 export source, `nfs`/`nfs4` type, readonly flag)만으로 검증하고 마운트 자체의 성공이 usable의 근거다. local bind는 추가로 owned proxy의 채택된 root를 bind했음을 증명하고 read probe에 답해야 한다. NodeGetVolumeStats는 agent `InspectImport` RPC로 기록된 identity·backend layout·정확한 quota를 재검증한 뒤 그 quota를 total capacity로 보고하고, 공유 filesystem의 `statfs`를 per-volume 사용량으로 읽지 않는다.

`staging_path` 필드가 남아 있는 file record는 이 lifecycle 밖이다. publish·unpublish·stats는 그 기록에 대해 fail closed로 거부하고, 기록과 그 마운트를 그대로 둔다.

### 5.5 주기적 filesystem trim (pillar-node)

thin zvol·LV는 노드가 discard를 보내야만 해제된 블록을 돌려받는다. Kubernetes와 호스트 `fstrim.timer`는 kubelet이
마운트한 PVC를 trim하지 않으므로 pillar-node가 스테이지한 filesystem 볼륨을 직접 trim한다 (ceph-csi ReclaimSpace,
Longhorn filesystem-trim, Portworx auto-fstrim과 같은 역할).

```
1. pillar-node 안의 백그라운드 루프 하나. CRD·sidecar·fstrim 바이너리 없음.
   --trim-interval (기본 168h, 0 = 비활성; Helm node.trim.enabled/node.trim.interval)
2. 대상: 이 노드에 스테이지된 volumeMode=Filesystem 볼륨의 staging 경로(globalmount)만.
   publish 경로와 raw Block 볼륨(파일시스템은 소비자 소유)은 trim하지 않는다.
   PVC/클래스 filesystem 문서의 periodicTrim: false 볼륨도 제외.
3. staging 디렉터리 fd에 FITRIM ioctl. 매 trim·청크 전에 /proc/self/mountinfo로 staging 경로가
   여전히 그 볼륨 디바이스의 마운트 지점인지 확인하고, 아니면 건너뛴다 (루트 파일시스템 trim 방지).
4. 노드당 동시에 하나, 볼륨은 순차. 16 GiB 청크 단위로 NodeStage/NodeUnstage/NodeExpand와 같은
   볼륨 잠금을 잡고 청크 사이에 놓는다. 잠금이 바쁘면 이번 회차는 건너뛰고 다음 tick에 재시도.
5. 일정: 스테이지 상태의 last_trim 기준 last + interval. 기록이 없으면 스테이지 시각 + [0, interval)
   무작위 지연. 루프는 min(interval, 1m)마다 깨어나 기한이 된 볼륨을 trim한다.
6. EOPNOTSUPP/ENOTTY·EROFS는 건너뜀(Info 로그), 그 밖의 오류는 볼륨·경로와 함께 Error 로그.
   메트릭: pillar_csi_node_trim_operations_total{result}, pillar_csi_node_trim_bytes_total,
   pillar_csi_node_trim_duration_seconds.
```

### 5.6 기존 LVM LV 채택 (`import-lv`, issue #163)

PVC 어노테이션으로 스토리지 노드에 이미 있는 LV를 새 볼륨 대신 채택한다. 미릴리스 기능이다. 운영 절차는
[`import-lv` how-to](../site/src/content/docs/docs/how-to/import-lv.md) 참조.

```
1. 요청: PVC 어노테이션
   pillar-csi.bhyoo.com/import-lv: "<vg>/<lv>:<vg_uuid>:<lv_uuid>"   (네 값 모두 필수, `lvs -o vg_uuid,lv_uuid`)
   pillar-csi.bhyoo.com/import-lv-policy: PreserveOriginal | Managed (생략 = PreserveOriginal, 그 밖의 값 거부)
   import-zvol·import-directory·import-zfs-dataset과 같은 claim에 함께 쓸 수 없고 StorageClass 파라미터로는 받지 않는다.
2. controller (CreateVolume):
   a. lvm-lv PillarStore + 같은 VG만 허용
   b. 첫 시도에서 이름·UUID·정책을 PillarVolumeState.spec.lvmSource에 기록 (불변, 추가·삭제 불가).
      재시도는 기록을 따른다. 다른 LV·UUID·정책을 가리키는 어노테이션은 거부 (재지정·강등 없음)
   c. 같은 agent의 다른 PillarVolumeState가 같은 <vg>/<lv> 또는 LV UUID를 쓰면 거부.
      LV UUID 키 PillarVolumeReservation으로 동시 생성 경합 차단
   d. agent ImportVolume(backend_type=LVM, expected_lvm_source, preserve_original)
3. agent (ImportVolume, backend 읽기 전용):
   a. lvs 한 번으로 이름·UUID 일치 확인 → linear 또는 설정된 thinPool의 thin LV만 허용
      (thinPool 없는 backend = linear만, 있는 backend = 그 pool의 thin만).
      snapshot·origin·pool·mirror·raid·pvmove·virtual·type 불명 LV 거부
   b. 비활성 LV 거부 (자동 활성화 없음), lv_size < 요청 용량 거부 (resize 없음)
   c. O_RDONLY|O_EXCL claim을 잡은 채 mount·holder·LIO/nvmet export 검사 후 lvs 재확인. 사용 중이면 거부
   d. 성공 시 identity와 정책을 fence mark에 고정. 고정된 source는 이후 lifecycle에서도 바뀌지 않고
      PreserveOriginal은 Managed로 강등되지 않는다
   e. lvchange·lvcreate·lvextend·lvremove·mkfs·fsck·mount를 실행하지 않는다. VG/LV 메타데이터 불변
   f. proto의 preserve_original은 optional: 미설정 = PreserveOriginal, 명시적 false만 Managed
```

**정책별 동작:**

| | PreserveOriginal (기본) | Managed |
|---|---|---|
| NodeStage | MountExisting: 기존 filesystem을 그대로 mount. mkfs·fsck·자동 repair·resize 없음. 빈 디바이스나 다른 fsType은 거부 | 일반 볼륨 경로 |
| 확장 | controller·agent·node 모두 거부 | 허용 |
| PVC 삭제 + reclaim Delete | ReleaseVolume: export 제거, pinned identity 재확인, 로컬 consumer 없음 확인 후 lifecycle retire. LV·데이터·thin pool 유지 | DeleteVolume으로 LV 삭제 |
| PVC 삭제 + reclaim Retain | PV·PillarVolumeState·export 유지. LV는 lifecycle이 계속 소유 (반환 아님) | 같음 |

커널의 일반 동작(journal replay)과 workload의 rw 쓰기는 허용된다. PreserveOriginal은 드라이버가 데이터를 바꾸지 않는다는
약속이며, rw mount 후 LV가 채택 전과 바이트 단위로 같다는 보장이 아니다.

**InspectVolume (인벤토리, LVM 전용):**
- 읽기 전용·fencing 없음: lvs, `blkid -p`, 즉시 닫는 O_EXCL open 1회, devidle 보고. 활성화·mount·쓰기·영속화 없음.
  LVM 외 backend는 UNIMPLEMENTED, 손상된 fence mark는 INTERNAL
- 응답: LV identity·lv_attr·segtype·pool_lv·크기·활성 상태·exclusive_claim(free/busy/unknown), filesystem signature와
  probe 상태(detected/unknown), consumers, agent 자신의 export 설정, fence mark(uid·generation·ended·ended_uids·정책·source)
- 관측일 뿐 소유권 주장이 아니다. probe unknown은 빈 디바이스가 아니고, consumers가 비어도 exclusive_claim busy면 사용 중
  (다른 mount namespace의 mount 등), fence mark 부재는 미관리 증거가 아니다
- 소유자 대응은 운영자가 한다: mark uid·agentRef·agentVolumeID·LV UUID가 모두 일치하는 PillarVolumeState가 정확히 하나일 때만
  claimed. 그 밖은 unknown으로 두고 소유자를 지정하지 않는다
- 전용 CLI 없음. grpcurl + 이미지와 같은 revision의 agent.proto + 승인된 client 인증서. CA에 서명된 client 인증서는 모든
  agent RPC를 호출할 수 있다 (읽기 전용 role 없음)

**Retain된 PV 재바인딩 (수동 절차):**

```
같은 PV·volumeHandle·PillarVolumeState UID를 유지한다. CreateVolume·새 lifecycle 없음.
1. Kubernetes 사전 확인: PV Released + Retain, 이전 claim(같은 UID, terminating 포함) 없음,
   VolumeAttachment 없음, publishedNodes 비어 있음, deleting=false,
   이전 노드 Ready + volumesInUse에 볼륨 없음 (오프라인·미확인 소유자는 차단)
2. controller를 replicas 0으로 일시 정지. 모든 종료 경로에서 원래 replicas로 복구 (trap)
3. UnexportVolume을 현재 lifecycle 토큰(PillarVolumeState UID, status.publicationGeneration)으로 호출.
   ReleaseVolume은 쓰지 않는다 (lifecycle retire)
4. InspectVolume: fence 존재·같은 UID·ended=false, fence와 관측 identity 모두 spec.lvmSource 네 값과 일치,
   exports·consumers 비어 있음, exclusive_claim=free. 빈 ACL·빈 consumers만으로 판단하지 않는다
5. PV를 다시 읽어 사전 확인 반복 후 metadata.uid·resourceVersion을 유지한 kubectl replace로 claimRef 교체 (충돌 시 처음부터)
6. volumeName을 지정한 static PVC 생성 → Bound
7. controller 복구 → ReconcileState가 같은 lifecycle로 export 재생성 → 새 Pod가 데이터 읽기
```

- PillarVolumeState.spec.claimRef는 원래 claim을 계속 가리킨다 (불변 기록, 수정하지 않음)
- 기존 publish 충돌 검사는 그대로 동작한다. 같은 노드에서 Kubernetes 밖으로 살아남은 이전 workload를 자동으로 막는 장치는
  없으며 사전 확인이 그 역할을 한다
- fence mark 삭제, PillarVolumeState 임의 patch는 지원하지 않는다
- Release된 lifecycle 토큰의 DeleteVolume 등 변경 요청은 FAILED_PRECONDITION. 예외는 이미 성공한 Managed DeleteVolume의
  같은 요청 재시도로, backend를 다시 건드리지 않고 성공한다

**메타데이터 유실 후 복구 (TransferVolumeOwnership, 수동 절차):**

권한은 두 서명뿐이다 (A+B). mTLS 호출자 신원, CA 개인키, 새 PKI 서비스는 권한이 아니다.

```
A. agent RecoverySnapshot: InspectVolume이 verified mTLS 호출자에게만, mark가 이 LV에 고정된 live lifecycle을
   기록할 때만 반환. volume_id·backend·lvm_source·old uid·정확한 old generation·preserve 정책·exclusive_claim·
   consumers·exports·fence·agent_identity(서버 인증서 CN 또는 첫 DNS SAN)·issued_at을 agent 서버 TLS 키로 서명
B. operator RecoveryAuthorization: snapshot digest·같은 volume/backend/source/old uid/old generation·
   new uid(복구 PillarVolumeState metadata.uid)·new generation·preserve(명시 필수)·issued_at/expires_at을
   --recovery-trust-anchor PEM(PUBLIC KEY/CERTIFICATE, 여러 키 허용)에 있는 키로 서명
서명: proto.MarshalOptions{Deterministic:true}로 signature를 비운 메시지 → SHA-256 → RSA PKCS#1 v1.5 또는 ECDSA ASN.1.
그 밖의 키 종류는 거부. grant 창 ≤ 24h(+2m skew), snapshot은 transfer 시점 15분 이내.

1. old lifecycle 정지: workload·unstage 확인, controller replicas 0, export가 남으면 old 토큰으로 UnexportVolume
2. import 어노테이션 없는 일반 PVC 생성 → 이름이 pvc-<claim UID>인 PillarVolumeState를 spec.recovery
   (oldVolumeUID, oldGeneration, source, newGeneration)로 생성. lvmSource와 함께 쓸 수 없다.
   이 레코드는 처음부터 RecoveryPending: publish·expand·delete 거부, reaper 제외(status가 비어도)
3. mTLS InspectVolume으로 새 snapshot → 오프라인에서 grant 서명 → 한 번의 patch로
   spec.recovery.newVolumeUID·authorization·authorizationDigest(write-once)와
   pillar-csi.bhyoo.com/recovery-snapshot 어노테이션(base64 deterministic proto)을 설정
4. controller 복구 → CreateVolume: grant·snapshot을 intent와 live 관찰에 대조 → TransferVolumeOwnership
5. agent: verified mTLS가 아니면 UNAUTHENTICATED, trust anchor 없으면 UNAVAILABLE. per-volume lock 아래
   두 서명 검증, mark의 uid·정확한 generation·pinned source·preserve 일치, LV 재관찰(export·ACL 없음, mount·
   holder 없음, O_EXCL free)을 확인한 뒤 mark 한 번 원자 쓰기: new uid/generation, old uid retired, source·정책,
   transfer 기록 + grant digest. rename 후 fsync 불확실은 TRANSFER_OUTCOME_UNKNOWN(rollback 없음)
6. 같은 요청 재시도는 ALREADY_COMMITTED. 같은 목적지의 다른 grant, 다른 목적지, stale snapshot, 옛 토큰은 거부
7. PillarVolumeState는 transfer 기록과 status가 일치한 뒤에만 Ready, claim은 유실 전과 같은 volumeHandle로 Bound
```

- trust anchor 개인키는 클러스터 밖에서 운영자가 보관한다. 파일은 시작 시 한 번 읽고, 읽기 실패·사용할 키 없음·
  서명할 수 없는 서버 인증서는 agent 기동 실패(fail closed). 회전: 새 키 추가 → 재시작 → 새 키로 서명 → 옛 키 제거 → 재시작
- agent 서버 인증서를 회전하면 미커밋 snapshot은 무효다. 다시 inspect한다
- import-lv로 채택된 LVM LV만 대상. 자동 takeover 없음. 오프라인·미확인 노드, 증명되지 않은 old initiator 정지,
  손상·부재·불일치 history는 모두 거부. 기존 managed LVM/zvol/NFS 경로는 바뀌지 않는다
- 검증: `test/e2e/tc_e39_metadata_recovery_e2e_test.go`(E39, 전용 Serial lane). 실제 Kind runner에서 아직 실행하지 않았다

**미구현·차단:**
- 두 서명 없는 메타데이터 유실 복구. fence mark 삭제·강제·오프라인 우회는 제공하지 않는다
- 지원 레이아웃 밖의 LV, 고정된 source·정책 변경, PreserveOriginal 확장, 읽기 전용 agent 인증서
- 지원 범위는 위 레이아웃과 초기 상태(활성·유휴 LV)에 한정되며 LVM API 전체와의 동등성을 뜻하지 않는다

## 6. 로드맵

### Phase 1: ZFS zvol + NVMe-oF TCP (MVP)

**범위:**
- CRD: PillarAgent, PillarStore, PillarProtocol, PillarStorageClass (모두 cluster-scoped)
- CRD controller + validation webhook (immutable 필드 검증)
- CRD status conditions (K8s 표준 패턴)
- Finalizer 기반 의존성 삭제 보호
- pillar-agent: ZFS zvol backend + NVMe-oF TCP target (configfs)
- pillar-agent: controller-pushed desired state + durable fencing/NFS recovery records
- pillar-node: NVMe-oF TCP initiator + init container modprobe (best-effort) + 도구 번들
- pillar-controller: CSI Controller (CreateVolume, DeleteVolume, ExpandVolume, ControllerPublishVolume/UnpublishVolume, ValidateVolumeCapabilities, GetCapacity)
- pillar-controller: CSI 작업 재시도/롤백 (exponential backoff)
- pillar-controller: PillarAgent 노드 label 자동 관리
- CSI Node (기본 identity: Stage/Unstage/Publish/Unpublish, NodeGetVolumeStats, NodeExpandVolume. file CSI node는 Stage 없이 Publish/Unpublish와 NodeGetVolumeStats만)
- NVMe-oF ACL on/off (PillarProtocol acl 필드)
- NVMe-oF 타임아웃 파라미터 (PillarProtocol 필드)
- StorageClass 자동 생성 (PillarStorageClass reconcile, ownerReference 관리)
- 파라미터 오버라이드 계층 (store/protocol → 바인딩 → 수동 SC 문서 → PVC 문서)
- filesystem 축: fsType/mkfsOptions/mountOptions (fsType 기본값: ext4)
- volumeMode: Filesystem 지원
- 볼륨 확장 (allowVolumeExpansion: backend capability 자동 결정 + 사용자 오버라이드)
- AccessMode: RWO, RWOP, ROX
- 구조화된 Volume ID: `{target}/{pool}/{volume-name}`
- gRPC 평문 통신 (TLS 옵션은 아키텍처에 포함, 비활성)
- Helm chart
- K8s 내부 노드만

**미포함:** 스냅샷/클론, volumeMode: Block, CSI Topology, RWX, 다른 backend/protocol, 외부 노드, 볼륨 와이핑, 별도 CLI/대시보드

### Phase 2: iSCSI Protocol — 구현됨
- LIO configfs 직접 조작 (target, `internal/agent/lio`) + pillar-node 내장 pure-Go initiator (`internal/iscsi`: login PDU, `NETLINK_ISCSI`로 커널 `iscsi_tcp`에 연결 인계, 세션 복구). targetcli·iscsiadm·iscsid 불필요
- 노드 사전 설치는 커널 모듈뿐이라는 zero-install 요구 때문에 호스트 iscsiadm, 이미지 번들 open-iscsi+iscsid, cgo libiscsi, u-root iscsinl을 검토 후 기각했다. 근거는 [`PRD-iscsi.md`](./PRD-iscsi.md) 참조

### Phase 3: ZFS Dataset + NFS — 구현됨
- 기본 CSI identity의 ZFS dataset backend + NFSv4.2 export + RWX 지원
- fixed port 2049, root squash default, explicit squash none/all, node-IP ACL, server-side quota expansion
- 기본 CSI identity는 Filesystem only이며 `filesystem.mountOptions` is the only mount flag axis; localAttach, mkfs, Block and RPC TLS are not offered. 이 branch의 file CSI identity는 별도 filesystem adoption 경로로 local direct mount와 owned-host NFS/RWX를 추가한다.

### Phase 4: 스냅샷/클론
- CSI Snapshot + ZFS snapshot/clone 통합

### Phase 5: LVM Backend

### Phase 6: SMB Protocol

### Phase 7: 외부 노드 지원
- PillarAgent `spec.external` + agent standalone 바이너리

### Phase 8: 추가 Backend
- block-device, directory 동적 생성, Btrfs subvolume (기존 directory filesystem 채택은 branch의 file CSI opt-in 기능)

## 7. 운영 정책

### 7.1 CRD 필드 Immutability

| 필드 구분 | 예시 | 수정 가능 |
|----------|------|:---:|
| **참조/구조** | agentRef, storeRef, protocolRef, storageClass.name, backend 멤버(zfs ↔ lvm), zfs.pool, lvm.volumeGroup, protocol 멤버 | X (validation webhook 거부) |
| **튜닝 파라미터** | zfs.properties, maxQueueSize, acl, ctrlLossTmo, filesystem.fsType | O |

### 7.2 의존성 삭제 보호

Finalizer를 사용하여 하위 참조가 있으면 삭제를 거부한다:

```
PillarAgent ← PillarStore ← PillarStorageClass ← StorageClass ← PVC
```

- PillarStore이 참조하는 PillarAgent 삭제 시도 → **거부**
- PillarStorageClass이 참조하는 PillarStore 삭제 시도 → **거부**
- 활성 PVC가 있는 StorageClass의 PillarStorageClass 삭제 시도 → **거부**
- 볼륨(PersistentVolume, PillarVolumeState)이 남아 있으면 그 볼륨이 프로비저닝된 PillarStorageClass·PillarStore와 볼륨이 놓인 PillarAgent 삭제 시도 → **거부**

볼륨은 StorageClass로 귀속한다: PV는 `spec.storageClassName`, PillarVolumeState는 같은 이름의 PV 또는 `spec.claimRef`가 가리키는 PVC의 StorageClass를 따른다. StorageClass가 정말 그 PillarStorageClass의 생성물인지는 확인된 증거만 인정한다: 존재하는 StorageClass는 PillarStorageClass를 가리키는 ownerReference가 있어야 하고, 없어진 StorageClass 이름은 binding의 `status.storageClassName` 기록이 증거가 된다. spec에 요청한 이름만으로는 귀속되지 않는다 — 준비되지 않은 binding이 다른 class와 이름이 충돌할 수 있으므로. 확인된 binding의 `spec.storeRef`가 볼륨의 PillarStore다. 한 풀(ZFS pool, LVM VG)에 PillarStore를 여러 개 두는 구성에서 다른 PillarStore의 볼륨은 삭제를 막지 않는다. 귀속할 수 없는 볼륨(어느 PillarStorageClass의 생성물로도 확인되지 않는 StorageClass, PV·claim 기록이 모두 없는 PillarVolumeState)은 안전하게 볼륨이 놓인 풀의 모든 PillarStore 삭제를 막는다.

사용자는 역순(PVC → PillarStorageClass → PillarStore → PillarAgent)으로 삭제해야 한다.

### 7.3 CRD Status Conditions

모든 CRD status conditions는 K8s 표준 패턴을 따른다. 각 condition은 다음 필드를 가진다:
- `type` — condition 타입 (예: Ready, AgentConnected)
- `status` — "True", "False", "Unknown"
- `reason` — 기계 판독 가능한 이유 코드
- `message` — 사람이 읽을 수 있는 상세 메시지
- `lastTransitionTime` — status가 마지막으로 변경된 시각

이 정보가 트러블슈팅의 주요 수단이다 (7.7 에러/트러블슈팅 DX 참조).

### 7.4 StorageClass 라이프사이클

PillarStorageClass이 ownerReference로 StorageClass를 완전 관리한다:

- PillarStorageClass 생성 → StorageClass 자동 생성
- PillarStorageClass spec 수정 → StorageClass 업데이트
- PillarStorageClass 삭제 → StorageClass 삭제
- StorageClass 직접 수정(`kubectl edit sc`) → reconciler가 PillarStorageClass spec으로 되돌림

StorageClass 이름은 PillarStorageClass의 `spec.storageClass.name`에서 사용자가 명시적으로 지정한다. `allowVolumeExpansion`은 backend capability에서 자동 결정되되, 사용자가 오버라이드할 수 있다.

### 7.5 CSI 작업 실패 시 롤백/재시도

- **CreateVolume 중간 실패**: Agent에서 zvol 생성 성공 후 export 실패 시, controller가 생성된 리소스를 **자동 롤백(정리)**.
- **재시도 정책**: Controller 자체 exponential backoff 재시도. 최대 횟수/타임아웃 설정 가능.
- **Agent 미연결 시 DeleteVolume**: controller가 agent 재연결까지 재시도. 중간 상태는 CRD status conditions에 반영.
- **볼륨 삭제**: 단순 `zfs destroy`. 데이터 와이핑 없음 (Phase 1).

### 7.6 용량 관리

Controller는 사전 용량 검증을 하지 않는다. Agent에 요청을 보내고 `zfs create` 실패 시 에러를 반환한다. ZFS의 quota/reservation 기능은 PillarStore의 backend properties에서 설정 가능하다. Controller 레벨 quota (Pool별 최대 프로비저닝 제한)는 Phase 1 스코프 아님.

### 7.7 에러/트러블슈팅 DX

**kubectl 네이티브만** 사용한다. 별도 CLI나 웹 대시보드를 제공하지 않는다.

- `kubectl describe pillaragent <name>` — conditions로 agent 연결, 노드 존재 여부 진단
- `kubectl describe pillarstore <name>` — conditions로 pool 발견, backend 지원 여부 진단
- `kubectl describe pillarstorageclass <name>` — conditions로 호환성, SC 생성 상태 진단
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
- Agent 크래시/리부트 복구: controller가 전체 상태를 push하고 agent가 durable fencing/NFS recovery state를 사용
- Controller 자체 재시도 로직: exponential backoff, 설정 가능한 최대 횟수/타임아웃
- Leader election 구현 완료 (`--leader-election` 플래그); Phase 1 Helm chart 기본값 replicas=1, 필요 시 스케일아웃 가능

### 8.3 보안
- Phase 1: 평문 gRPC (클러스터 내부 신뢰). TLS 옵션 아키텍처에 포함, 비활성
- Phase N: Agent ↔ controller 간 mTLS (외부 노드 지원 시)
- NVMe-oF/iSCSI ACL on/off (PillarProtocol acl 필드)
- iSCSI CHAP·MutualCHAP (PillarProtocol `iscsi.auth`, 설치 네임스페이스 Secret). controller는 그 네임스페이스의 Secret만 읽고, 자격 증명은 controller → agent gRPC로 전달되므로 CHAP을 쓰면 mTLS를 켠다
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
| **PillarAgent** | 사용자가 생성하는 스토리지 agent 인스턴스 정의. 노드 위치 + agent 상태 |
| **PillarStore** | PillarAgent의 특정 Backend 인스턴스 (예: rock5bp의 hot-data ZFS pool) |
| **PillarProtocol** | 프로토콜 타입과 기본 설정의 재사용 가능한 정의. 노드 무관 |
| **PillarStorageClass** | PillarStore + PillarProtocol 조합. StorageClass를 자동 생성 |
| **Target** | 스토리지를 네트워크로 내보내는 측 (스토리지 노드, agent가 configfs로 관리) |
| **Initiator** | 네트워크 스토리지에 연결하는 측 (워커 노드, CSI node plugin이 관리) |
| **Agent** | 스토리지 노드의 gRPC 서버. Backend/Protocol target 플러그인. K8s 의존성 없음. fencing/NFS recovery state를 hostPath에 보존 |
| **configfs** | 리눅스 커널 설정 파일시스템. NVMe-oF/iSCSI target을 CLI 없이 직접 제어 |
