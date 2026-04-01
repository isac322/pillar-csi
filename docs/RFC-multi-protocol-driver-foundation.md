# pillar-csi Multi-Protocol Driver Foundation RFC

## 1. 문서 목적

이 RFC는 `pillar-csi`를 "NVMe-oF 전용에 가까운 현재 구조"에서
"하나의 CSI driver 안에서 여러 protocol을 수용할 수 있는 구조"로 교정하기 위한
선행 설계 문서다.

이 RFC의 직접 목표는 iSCSI를 구현하는 것이 아니다.
대신 아래 기반 계약을 먼저 확정하는 것이다.

- `NodeGetInfo.node_id`의 의미
- protocol-specific initiator identity를 다루는 방식
- controller publish/unpublish의 protocol 해석 경로
- node runtime의 multi-protocol 구조
- agent runtime의 multi-protocol export dispatch 구조
- block protocol과 file protocol을 수용할 수 있는 공통 토대
- driver-wide CSI capability의 프로토콜별 의미 해석

이 RFC가 완료되어야 [`docs/PRD-iscsi.md`](./PRD-iscsi.md)를 구현 착수 대상으로 넘길 수 있다.

---

## 2. 문제 정의

현재 `pillar-csi`는 제품 방향상 "하나의 CSI driver가 backend와 protocol을 조합해 제공"하려고 하지만,
실제 구현과 테스트 설명 일부는 여전히 단일 protocol 가정을 품고 있다.

### 2.1 이미 multi-protocol을 수용하는 계층

현재 구조에서 아래 계층은 이미 multi-protocol을 올바르게 수용한다:

- **Agent proto 정의** (`agent.proto`): `ProtocolType` enum, `ExportParams` oneof,
  `AllowInitiator`/`DenyInitiator` RPC가 protocol-agnostic하게 설계됨.
- **CRD 정의** (`PillarProtocol`, `PillarBinding`): discriminated union 패턴
  (`nvmeofTcp`/`iscsi`/`nfs`)으로 protocol별 config를 분리.
- **Volume ID 라우팅**: `<target>/<protocol>/<backend>/<vol-id>` 포맷이
  모든 CSI 호출에서 protocol type과 backend type을 모두 포함.
- **Controller CreateVolume/DeleteVolume**: protocol type을 파라미터로 전달하며
  agent에 위임.

이 계층들은 이 RFC의 수정 대상이 아니다.

### 2.2 NVMe-oF에 결합된 계층

결합 지점은 크게 6곳에 분포한다:

1. **controller publish/unpublish의 identity resolution**
   (`controller.go:1472-1477`)
   - `node_id`를 `initiator_id`로 직접 전달:
     `AllowInitiator(…, InitiatorId: nodeID)`
   - 한 노드가 NQN과 IQN을 동시에 갖는 경우 `node_id` 하나로 표현 불가.

2. **node runtime의 `Connector` 인터페이스와 구현**
   (`node.go:94-114`, `nvmeof_connector.go` 전체)
   - `Connect(subsysNQN, trAddr, trSvcID)`, `Disconnect(subsysNQN)`,
     `GetDevicePath(subsysNQN)` — 모두 NVMe-oF TCP 전용.
   - iSCSI는 discovery→login→SCSI device resolve가 필요하고,
     NFS/SMB는 block device가 아닌 mount source를 반환해야 한다.

3. **`NodeStageVolume`의 하드코딩된 NVMe-oF 경로**
   (`node.go:356-525`)
   - protocol type 확인 없이 `connector.Connect()` 직접 호출.
   - device poll이 `/sys/class/nvme-subsystem/` 전용.
   - VolumeContext 키 이름(`target_id`, `address`, `port`)은 generic하지만
     주석과 사용처가 NVMe-oF 전용.

4. **node stage state 구조**
   (`node.go:89-92`)
   - `nodeStageState{SubsysNQN string}` — NVMe-oF 전용 단일 필드.
   - iSCSI는 target IQN + session ID, NFS는 server + export path가 필요.

5. **`cmd/node/main.go`의 production connector** (~400 lines)
   - `fabricsConnector` 구조체가 nvme-cli fallback scanning,
     containerized `mknod` 자동생성, `getDevicePathViaController()` 등을 포함.
   - RFC의 `Connector` → `ProtocolHandler` 리팩토링 시 이 구현체도 반드시 포함해야 함.
   - `NewNodeServer` 생성자가 단일 `Connector`를 받는 구조를 handler map으로 교체해야 함.

6. **agent-side protocol gating**
   (`internal/agent/server.go`, `server_export.go`, `server_reconcile.go`)
   - `errOnlyNvmeofTCP` sentinel로 모든 export/ACL/reconcile handler에서 non-NVMe-oF 거부.
   - `volumeNQN()` 함수가 NVMe NQN을 하드코딩 파생 — iSCSI는 IQN, NFS는 export path.
   - `nqnMu sync.Map` lock이 NQN 기반 — protocol별 target ID 기반으로 일반화 필요.
   - `nvmeof/` 패키지 전체가 NVMe-oF configfs 전용 — agent 측 protocol handler 추상화 없음.

부수적으로:
- 에러 메시지가 NVMe-oF를 직접 참조 (`node.go:24,48,337,448,481`).
- state machine 주석이 NVMe-oF 가정 (`statemachine.go:42,75-93,175`).
- CRD의 `ProtocolType` enum에 SMB가 누락 (proto에는 있음).
- `NodeExpandVolume`이 block device resize만 수행 (file protocol은 고려 안 함).
- `ControllerExpandVolume`이 무조건 `NodeExpansionRequired: true` 반환 (file protocol은 `false`여야 함).
- `supportedAccessModes`(`controller.go:238-258`)가 `MULTI_NODE_MULTI_WRITER`를 제외 — NFS/SMB RWX 차단.

이 상태에서 iSCSI를 바로 구현하면, iSCSI는 들어가더라도
앞으로 NFS/SMB 같은 protocol을 추가할 때 다시 구조를 뒤집게 된다.

지금은 아직 public release도 아니고 production compatibility burden도 없으므로,
이 타이밍에 foundation을 바로잡는 것이 맞다.

---

## 3. 목표

- `pillar-csi`는 계속 **하나의 CSI driver**여야 한다.
- `NodeGetInfo.node_id`는 transport-specific identity가 아니라 **stable node handle**이어야 한다.
- protocol-specific identity는 `node_id`와 분리되어야 한다.
- controller는 `node_id`를 통해 Kubernetes의 CSI-scoped node record를 찾고,
  거기서 protocol-specific identity를 해석해야 한다.
- node runtime과 agent runtime 모두 protocol handler와 common lifecycle로
  분리돼야 한다.
- future protocol(NFS/SMB) 추가 시 `node_id` 의미를 다시 바꾸지 않게 해야 한다.
- future file protocol은 block protocol과 동일한 publish/stage semantics를 자동 상속한다고
  가정하지 않아야 한다.
- driver-wide CSI capability(`STAGE_UNSTAGE_VOLUME`, `EXPAND_VOLUME`)가
  보고되더라도, 각 protocol은 해당 capability의 의미를 자체 정의할 수 있어야 한다.
- file protocol의 ReadWriteMany (RWX) access mode를 수용할 수 있는 구조여야 한다.
- 기존 NVMe-oF 경로도 이 새 계약 아래에서 계속 동작해야 한다.

---

## 4. 비목표

- iSCSI target/LIO 구현 자체
- CHAP/multipath 구체 설계 (단, ProtocolHandler가 향후 이를 수용할 수 있어야 함)
- snapshot/clone 제품 요구사항 (단, 이들은 backend-only 연산이며 protocol 코드와 무관함을 확인)
- NFS/SMB 제품 PRD 작성
- 외부 SAN/NAS import UX 정의
- NVMe/RDMA transport variant 구현

이 RFC는 foundation만 다룬다.

---

## 5. 핵심 결정

### 5.1 `NodeGetInfo.node_id`

- `NodeGetInfo.node_id`는 CSI driver가 사용하는 **stable node handle**이다.
- Kubernetes 내부에서는 MVP 기본값으로 Kubernetes node name을 사용한다.
- `node_id`는 NVMe host NQN도 아니고 iSCSI initiator IQN도 아니다.

근거 (CSI spec):

> "The identifier of the node as understood by the SP."
> — CSI spec, NodeGetInfoResponse

`node_id`는 plugin이 정하는 node identifier이지, transport-level identity가 아니다.
NetApp Trident(가장 성숙한 multi-protocol CSI driver)도 Kubernetes node name을
`node_id`로 사용한다.

결과:

- 기존 NVMe-oF path도 `node_id == NQN` 가정을 버려야 한다.
  - `controller.go:1472`의 `InitiatorId: nodeID` → CSINode lookup으로 교체.
- iSCSI 추가 시에도 별도 driver를 만들지 않고 같은 계약을 유지할 수 있다.

### 5.2 protocol-specific identity publication

- transport-specific identity는 Kubernetes `Node` 본문이 아니라
  `CSINode.metadata.annotations`에 publish한다.
- 예시:
  - `pillar-csi.bhyoo.com/nvmeof-host-nqn` — NVMe host NQN (`/etc/nvme/hostnqn`)
  - `pillar-csi.bhyoo.com/iscsi-initiator-iqn` — iSCSI IQN (`/etc/iscsi/initiatorname.iscsi`)
  - `pillar-csi.bhyoo.com/node-ip` — NFS/SMB client IP
- `CSINode`는 kubelet이 CSI driver 등록 시 생성/갱신하는 CSI-scoped node object이며,
  이름은 Kubernetes node name과 같다.
- 이 annotation은 node-side publisher가 자동으로 채운다.
- publisher는 `pillar-node` 내부 기능일 수도 있고, 함께 배포되는 helper일 수도 있다.

Identity source별 특성:

| Protocol | Identity | Source | 특성 |
|----------|----------|--------|------|
| NVMe-oF TCP | Host NQN | `/etc/nvme/hostnqn` | 파일 기반, 노드 고정 |
| NVMe-oF RDMA | Host NQN | 동일 | NVMe/TCP와 identity 공유 |
| iSCSI | Initiator IQN | `/etc/iscsi/initiatorname.iscsi` | 파일 기반, 노드 고정 |
| NFS | Client IP | `Node.status.addresses` | IP 변경 가능 — 아래 참고 |
| SMB | Credentials | K8s Secret (`nodeStageSecretRef`) | annotation 불필요 |

NFS identity 특수성:

- NFS ACL은 client IP 기반이다.
- IP는 노드 재시작이나 네트워크 재구성 시 변경될 수 있다.
- `CSINode` annotation에 IP를 정적 저장하는 대신, controller가 `ControllerPublishVolume`
  시점에 `Node.status.addresses`에서 직접 읽는 것이 더 정확할 수 있다.
- 또는 `PillarTarget.status.resolvedAddress`와 동일한 패턴으로 노드 IP를 resolve.
- 이 결정은 NFS PRD 시점에 확정하되, CSINode annotation 스키마에 NFS IP key를
  **예약만** 해둔다.

SMB identity 특수성:

- SMB는 username/password 기반 인증이다.
- initiator identity가 CSINode annotation이 아니라 **K8s Secret**으로 전달된다.
- CSI spec의 `NodeStageVolume.secrets` 맵이 이 용도로 설계됨.
- `ControllerPublishVolume`에서 ACL 관리가 필요한 경우,
  controller가 Secret에서 username을 읽어 agent에 전달하는 flow.

보안 모델 참고 (비목표지만 인터페이스 호환 필요):

- **iSCSI CHAP**: mutual CHAP은 initiator/target 양쪽에 credentials 필요.
  CSI `NodeStageVolume.secrets`로 전달. ProtocolHandler.Attach()의 `Extra` 맵으로 수용 가능.
- **NFS Kerberos**: `sec=krb5/krb5i/krb5p` mount option. Kerberos 인프라 필요.
  mount options로 전달 가능.
- **SMB AD**: Active Directory 기반 인증. `mount.cifs`의 `-o sec=krb5` 옵션.

RBAC 요구사항:

- node plugin DaemonSet가 `storage.k8s.io/csinodes`에 대해
  `get`, `update`, `patch` 권한 필요.
- controller가 `CSINode` 읽기 위해 `get`, `list` 권한 필요.
- Helm chart (`charts/pillar-csi/templates/clusterrole.yaml`)에 위 권한 추가 필요.

Annotation write race:

- node plugin이 CSINode annotation을 아직 쓰지 않은 상태에서
  controller가 ControllerPublishVolume을 호출할 수 있다.
- 이 경우 `FailedPrecondition`을 반환하고 CO가 exponential backoff으로 재시도.
- Fresh node bootstrap에서 첫 publish까지 지연 발생 가능 (예상: 15-30초).
- 이 동작은 정상이며 문서화한다.

제품 계약:

- 사용자가 수동으로 `CSINode` annotation을 편집하는 것이 기본 운영 모델이면 안 된다.
- node restart 후에도 annotation은 self-healing 되어야 한다.
- `Node.status`나 임의의 built-in status field를 확장 포인트로 사용하지 않는다.

### 5.3 controller publish/unpublish resolution

- controller는 protocol type을 `VolumeId` 포맷에만 의존해 해석하지 않는다.
- protocol type의 authoritative source는 volume control-plane state
  (`PillarVolume` 또는 `volume_context`)여야 한다.
- `node_id`로 `CSINode`를 찾는다.
- protocol type에 맞는 identity key를 `CSINode` annotation에서 읽는다.
- 읽은 값을 `AllowInitiator` / `DenyInitiator`에 넘긴다.

즉, publish/unpublish 계약은 아래로 바뀐다.

`stable node handle` → `CSINode annotation lookup` → `protocol-specific identity`

protocol별 resolution:

```
NVMe-oF TCP/RDMA:
  CSINode["pillar-csi.bhyoo.com/nvmeof-host-nqn"] → AllowInitiator(initiator_id=NQN)

iSCSI:
  CSINode["pillar-csi.bhyoo.com/iscsi-initiator-iqn"] → AllowInitiator(initiator_id=IQN)

NFS:
  Node.status.addresses[InternalIP] 또는 CSINode annotation → AllowInitiator(initiator_id=IP)

SMB:
  Secret에서 username 추출 → AllowInitiator(initiator_id=username)
  또는 no-op (Samba 자체 인증에 위임)
```

annotation이 존재하지 않으면 `FailedPrecondition`을 반환하고,
CO (external-attacher)가 exponential backoff으로 재시도한다.

### 5.4 node runtime 구조

#### 5.4.1 3계층 분리

node runtime은 아래 3계층으로 나뉘어야 한다.

1. **protocol handler (Layer 1)**
   - 목적: transport/session setup과 teardown.
   - NVMe-oF: connect / disconnect / rescan / device resolve
   - iSCSI: discovery / login / logout / rescan / device resolve
   - NFS/SMB: (no-op — Layer 2가 직접 처리)
2. **common volume presentation (Layer 2)**
   - 목적: Layer 1의 출력을 workload가 사용할 수 있는 형태로 변환.
   - block protocol 경로: mkfs + filesystem mount (또는 raw block bind-mount)
   - file protocol 경로: protocol-specific mount (`mount -t nfs`, `mount -t cifs`)
   - 공통: stats, filesystem grow, unmount
   - **Layer 2는 struct/함수 집합으로 구현**하며, 단순 inline code가 아니라
     `VolumePresenter` 같은 명시적 컴포넌트로 캡슐화한다.
3. **CSI node orchestration (Layer 3)**
   - NodeStage / Unstage / Publish / Unpublish / Expand / Stats
   - Layer 1 → Layer 2 순서 호출 + state machine 관리

핵심은 "transport/session setup"과 "volume presentation to workload"를 분리하는 것이다.
block protocol에서는 device lifecycle이, file protocol에서는 share mount lifecycle이
여기에 대응한다.

file protocol 경로의 Layer 1↔2 관계:

- NFS/SMB에서 Layer 1 (ProtocolHandler.Attach)은 mount source string만 반환한다.
- 실제 `mount -t nfs` 호출은 **Layer 2 (VolumePresenter)**가 담당한다.
- Layer 3은 Layer 1의 결과를 보고 Layer 2의 올바른 경로(block 또는 file)를 선택한다.
- 이로써 3-layer가 file protocol에서도 유지된다.

#### 5.4.2 ProtocolHandler 인터페이스

현재 `Connector` 인터페이스(`node.go:94-114`)를 `ProtocolHandler`로 교체한다.

```go
// ProtocolHandler abstracts transport-level operations for different
// storage protocols. Each protocol (NVMe-oF, iSCSI, NFS, SMB) provides
// its own implementation.
type ProtocolHandler interface {
    // Attach establishes the transport connection and returns either:
    //   - Block protocols: the local block device path (e.g. /dev/nvme0n1)
    //     For iSCSI, use stable path: /dev/disk/by-path/ip-<ip>:3260-iscsi-<iqn>-lun-<lun>
    //   - File protocols: the mount source (e.g. "192.168.1.10:/export/vol1")
    // The returned AttachResult indicates which presentation path to follow.
    Attach(ctx context.Context, params AttachParams) (*AttachResult, error)

    // Detach tears down the transport connection.
    Detach(ctx context.Context, state ProtocolState) error

    // Rescan triggers a device rescan after online volume expansion.
    // For NVMe-oF: echo 1 > /sys/class/nvme-ns/<ns>/rescan_controller
    // For iSCSI: iscsiadm -m session --rescan
    // For NFS/SMB: no-op (server-side resize is transparent).
    Rescan(ctx context.Context, state ProtocolState) error
}

// AttachParams is the protocol-agnostic input to Attach.
type AttachParams struct {
    ProtocolType string            // "nvmeof-tcp", "iscsi", "nfs", "smb"
    ConnectionID string            // Protocol-specific target identifier:
                                   //   NVMe-oF: subsystem NQN
                                   //   iSCSI:   target IQN
                                   //   NFS:     server IP (mount source is derived)
                                   //   SMB:     server IP (UNC path is derived)
    Address      string            // target IP address
    Port         string            // target port
    VolumeRef    string            // namespace ID / LUN / export path / share name
    Extra        map[string]string // protocol-specific (e.g. NFS version, CHAP credentials)
}

// AttachResult is the protocol-agnostic output of Attach.
type AttachResult struct {
    // DevicePath is set for block protocols (NVMe-oF, iSCSI).
    // For iSCSI: /dev/disk/by-path/ip-<ip>:3260-iscsi-<iqn>-lun-<lun>
    DevicePath string
    // MountSource is set for file protocols (NFS, SMB).
    // NFS: "192.168.1.10:/export/vol1"
    // SMB: "//192.168.1.10/share"
    MountSource string
    // State is the opaque state needed for Detach/Rescan recovery.
    State ProtocolState
}
```

`AttachParams.TargetID`를 `ConnectionID`로 변경한 이유:
- `TargetID`는 NVMe-oF/iSCSI에서는 target identifier이지만,
  NFS/SMB에서는 server IP이며 "target ID"라는 이름이 의미를 혼란시킴.
- `ConnectionID`는 "이 transport connection을 식별하는 값"이라는 의미로
  모든 protocol에서 일관된 의미를 가짐.

`ProtocolState`는 Section 5.5에서 정의한다.

기존 코드 리팩토링 대상:

- `nvmeof_connector.go`의 `NVMeoFConnector` → `NVMeoFTCPHandler` (ProtocolHandler 구현).
- `cmd/node/main.go`의 `fabricsConnector` (~400 lines) → `NVMeoFTCPHandler`의 production 구현.
  이 파일의 `newFabricsConnector()` → handler map 생성자로 교체.
- `NewNodeServer` 생성자: `connector Connector` → `handlers map[string]ProtocolHandler`.

multipath 호환성:

- `ProtocolHandler.Attach()`는 단일 path를 반환한다. multipath 지원 시
  `Attach()`가 여러 path를 반환하거나, 별도 `AddPath()` 메서드가 필요할 수 있다.
- 이는 이 RFC의 비목표이지만, interface가 향후 multipath 확장을 허용하도록
  `AttachResult`를 struct로 두고 field 추가가 가능한 형태를 유지한다.

#### 5.4.3 NodeStageVolume protocol dispatch

`NodeStageVolume`은 `volume_context["protocol-type"]` 또는
`volumeID`에서 추출한 protocol type을 기반으로 올바른 handler를 선택한다:

```go
func (n *NodeServer) NodeStageVolume(ctx, req) {
    protocolType := extractProtocolType(req)
    handler := n.handlers[protocolType]  // map[string]ProtocolHandler
    if handler == nil {
        return FailedPrecondition("unsupported protocol: %s", protocolType)
    }

    // Layer 1: transport attach
    result, err := handler.Attach(ctx, attachParamsFromContext(req))
    // ...

    // Layer 2: volume presentation
    if result.DevicePath != "" {
        // Block protocol path → VolumePresenter.PresentBlock()
        n.presenter.PresentBlock(result.DevicePath, stagingPath, fsType, flags)
    } else if result.MountSource != "" {
        // File protocol path → VolumePresenter.PresentFile()
        n.presenter.PresentFile(result.MountSource, stagingPath, protocolMountType, mountOpts)
    }

    // Layer 3: state persistence
    persistState(volumeID, result.State)
}
```

### 5.5 runtime state 일반화

#### 5.5.1 구조 변경

persisted stage state는 NVMe 전용 필드(`SubsysNQN`)에 고정되면 안 된다.

현재:
```go
type nodeStageState struct {
    SubsysNQN string `json:"subsys_nqn"`  // NVMe-oF 전용
}
```

변경 — discriminated union 패턴 (CRD의 protocol config와 동일한 접근):
```go
type nodeStageState struct {
    ProtocolType string               `json:"protocol_type"`
    NVMeoF       *NVMeoFStageState    `json:"nvmeof,omitempty"`
    ISCSI        *ISCSIStageState     `json:"iscsi,omitempty"`
    NFS          *NFSStageState       `json:"nfs,omitempty"`
    SMB          *SMBStageState       `json:"smb,omitempty"`
}

type NVMeoFStageState struct {
    SubsysNQN string `json:"subsys_nqn"`
    Address   string `json:"address"`
    Port      string `json:"port"`
}

type ISCSIStageState struct {
    TargetIQN string `json:"target_iqn"`
    Portal    string `json:"portal"`     // ip:port
    LUN       int    `json:"lun"`
}

type NFSStageState struct {
    Server     string `json:"server"`
    ExportPath string `json:"export_path"`
}

type SMBStageState struct {
    Server string `json:"server"`
    Share  string `json:"share"`
}
```

`Extra map[string]string` 대신 typed struct를 사용하는 이유:

- `Detach()`는 노드 재부팅 후 persisted state에서 복구하여 동작해야 한다.
- iSCSI의 portal/LUN 등 필수 필드가 generic map에 있으면,
  필드 누락 시 silent failure로 세션이 leak된다.
- Typed struct는 compile-time에 필수 필드를 보장한다.

`ProtocolHandler.Detach()`와의 연결:

- `nodeStageState`에서 `ProtocolState`를 파생하여 `Detach()`에 전달:
  ```go
  func (s *nodeStageState) ToProtocolState() ProtocolState { ... }
  ```
- `ProtocolState`는 `ProtocolHandler` 인터페이스의 internal type으로,
  각 handler가 자신의 state를 해석한다.

#### 5.5.2 기존 state 파일 마이그레이션

기존 NVMe-oF 볼륨의 state 파일(`/var/lib/pillar-csi/node/<volumeID>`)은
`{"subsys_nqn": "nqn.…"}` 형식이다. 새 코드는 `{"protocol_type": "nvmeof-tcp", ...}`
형식을 기대한다.

마이그레이션 전략:

- `readStageState()` 함수에서 `protocol_type` 필드가 없으면 legacy 형식으로 판단.
- Legacy 형식 감지 시 `subsys_nqn` 필드를 읽어 `NVMeoFStageState`로 변환.
- 변환된 state를 새 형식으로 재저장 (in-place migration).
- 이로써 rolling upgrade 시 기존 볼륨의 disconnect가 정상 동작한다.

### 5.6 `VolumeContext` 계약

`VolumeContext`는 "모든 protocol에 동일한 고정 키셋"이라고 가정하지 않는다.

기본 원칙:

- 공통 코어 키는 최소화한다.
  - 예: `protocol-type`
- protocol별 런타임 정보는 protocol-scoped key로 분리한다.

concrete key schema:

```
공통 (모든 protocol):
  protocol-type     = "nvmeof-tcp" | "iscsi" | "nfs" | "smb"

Block transport (NVMe-oF TCP/RDMA, iSCSI):
  target_id         = NQN 또는 IQN
  address           = target IP
  port              = target port (string)
  volume_ref        = namespace ID 또는 LUN number

NFS:
  nfs.server        = NFS server IP
  nfs.export_path   = export path (e.g. "/mnt/tank/pvc-abc123")
  nfs.version       = "4.2" (optional)

SMB:
  smb.server        = SMB server IP
  smb.share         = share name
  smb.subdir        = subdirectory within share (optional)
```

현재 `node.go:49-61`의 `VolumeContextKeyTargetNQN` 등 상수는:
- 변수명: `VolumeContextKeyTargetNQN` → `VolumeContextKeyTargetID`로 rename.
- 주석에서 "NVMe-oF" 참조 제거.
- block protocol 공통 키(`target_id`, `address`, `port`)는 NVMe-oF와 iSCSI가 공유.

### 5.7 driver-wide capability 계약

#### 5.7.1 `attachRequired: true`

- `CSIDriver.spec.attachRequired`는 driver-wide contract다.
- 현재 `pillar-csi`는 `ControllerPublish/Unpublish`를 controller-side publish 단계로
  사용하므로 `attachRequired: true`를 유지한다.
- 이는 external-attacher가 호출하는 controller-side publish 계약이지,
  iSCSI/NVMe-oF node-side login/connect 자체를 의미하지 않는다.
- future file protocol을 같은 driver에 넣으려면 아래 둘 중 하나여야 한다.
  - controller-side publish/unpublish를 authorization, export policy, credential setup,
    또는 no-op publish contract로 수용한다.
  - 이 계약이 맞지 않으면 별도 follow-up RFC에서 separate-driver 또는 다른 packaging
    결정을 내린다.

#### 5.7.2 `STAGE_UNSTAGE_VOLUME`

`STAGE_UNSTAGE_VOLUME` capability는 driver-wide다.
`pillar-csi`는 block protocol의 device lifecycle을 위해 이를 보고하므로,
**모든 protocol의 volume이 NodeStage/Unstage를 거친다**.

protocol별 Stage/Unstage 의미:

| Protocol | NodeStage | NodeUnstage |
|----------|-----------|-------------|
| NVMe-oF TCP | transport connect + device resolve + format/mount | unmount + disconnect |
| iSCSI | discovery + login + device resolve + format/mount | unmount + logout |
| NFS | `mount -t nfs server:path staging_path` | `umount staging_path` |
| SMB | `mount -t cifs //server/share staging_path -o credentials` | `umount staging_path` |

NFS/SMB에서 Stage가 "불필요"하다고 느낄 수 있지만:
- SMB는 **credentials를 Stage에서 적용**해야 한다 (CSI spec의 `node_stage_secrets`).
  공식 `csi-driver-smb`도 동일한 패턴.
- NFS는 Stage에서 mount하고 Publish에서 bind-mount하면
  **multi-pod sharing** (ReadWriteMany) 시 single NFS session을 공유할 수 있다.

따라서 `STAGE_UNSTAGE_VOLUME`을 유지하되,
각 protocol handler가 Stage/Unstage의 concrete 행위를 자체 정의한다.

참고: 공식 `csi-driver-nfs`는 `STAGE_UNSTAGE_VOLUME`을 사용하지 않는다.
pillar-csi가 이를 유지하는 것은 설계 선택이며 spec 위반은 아니다.

#### 5.7.3 `EXPAND_VOLUME`

`NodeExpandVolume`은 현재 block device의 filesystem resize만 수행한다
(`node_expand.go:82`).

protocol별 expand 의미:

| Protocol | ControllerExpandVolume | NodeExpandVolume |
|----------|----------------------|-----------------|
| NVMe-oF/iSCSI | agent.ExpandVolume + **NodeExpansionRequired: true** | handler.Rescan() + resize2fs / xfs_growfs |
| NFS | agent.ExpandVolume + **NodeExpansionRequired: false** | **호출되지 않음** |
| SMB | agent.ExpandVolume + **NodeExpansionRequired: false** | **호출되지 않음** |

`ControllerExpandVolume` (`controller.go:1709`)이 현재 무조건 `NodeExpansionRequired: true`를
반환하는 것을 수정해야 한다. file protocol 볼륨인 경우 `false`를 반환하여
불필요한 `NodeExpandVolume` roundtrip을 방지한다.

`NodeExpandVolume`도 방어적으로 file protocol 검사를 유지하되,
정상 경로에서는 `ControllerExpandVolume`이 `false`를 반환하여 아예 호출되지 않는다.

block protocol에서 `NodeExpandVolume`은:
1. `ProtocolHandler.Rescan()` 호출 → block device가 새 크기를 인식.
2. `resize2fs` 또는 `xfs_growfs` 실행 → filesystem 확장.

#### 5.7.4 `MULTI_NODE_MULTI_WRITER` (RWX) access mode

현재 `supportedAccessModes` (`controller.go:238-258`)가
`MULTI_NODE_MULTI_WRITER`를 제외한다. NFS/SMB의 핵심 가치 중 하나가 RWX이므로,
이를 protocol-aware하게 수용해야 한다.

| Access Mode | Block (NVMe-oF, iSCSI) | File (NFS, SMB) |
|-------------|----------------------|----------------|
| SINGLE_NODE_WRITER | ✓ | ✓ |
| SINGLE_NODE_READER_ONLY | ✓ | ✓ |
| MULTI_NODE_READER_ONLY | ✓ | ✓ |
| MULTI_NODE_SINGLE_WRITER | ✗ | ✓ |
| MULTI_NODE_MULTI_WRITER | ✗ | ✓ |

수정 방향:

- `ValidateVolumeCapabilities`와 `CreateVolume`에서 protocol type에 따라
  지원되는 access mode를 동적으로 판단.
- `MULTI_NODE_MULTI_WRITER`는 file protocol에서만 수락.
- `ControllerPublishVolume`이 동일 볼륨에 대해 여러 node grant를 허용해야 함
  (현재는 단일 node 가정).
- `NodeUnstageVolume`이 shared NFS mount를 다른 pod가 사용 중일 때
  premature disconnect하지 않도록 reference counting 필요.

이 기능의 Phase 3 포함 범위:
- `supportedAccessModes`를 protocol-aware하게 분기하는 것은 Phase 3에 포함.
- 실제 multi-node grant와 reference counting은 NFS PRD 시점에 구현.

### 5.8 topology를 통한 프로토콜 호환성 표현

현재 `NodeGetInfo`는 `AccessibleTopology: nil`을 반환한다.

multi-protocol 환경에서 노드가 지원하는 프로토콜을 topology key로 표현하면,
CO가 volume을 올바른 노드에 스케줄링할 수 있다:

```go
AccessibleTopology: &csi.Topology{
    Segments: map[string]string{
        "pillar-csi.bhyoo.com/nvmeof": "true",
        "pillar-csi.bhyoo.com/iscsi":  "true",
        "pillar-csi.bhyoo.com/nfs":    "true",
    },
}
```

이 topology key는:
- node plugin 시작 시 등록된 protocol handler를 기반으로 결정.
- NVMe-oF: `/sys/module/nvme_tcp` 존재 여부.
- iSCSI: `iscsid` 프로세스 존재 또는 `/etc/iscsi/initiatorname.iscsi` 존재.
- NFS: `mount.nfs` 바이너리 존재.
- SMB: `mount.cifs` 바이너리 존재.

StorageClass의 `allowedTopologies`에서 protocol 호환성을 제약할 수 있다:

```yaml
allowedTopologies:
  - matchLabelExpressions:
      - key: pillar-csi.bhyoo.com/iscsi
        values: ["true"]
```

**topology는 Phase 1b에서 구현한다** (optional이 아님).
topology 없이 multi-protocol 볼륨을 스케줄링하면, protocol handler가 없는 노드에
배치되어 `NodeStageVolume`이 실패한다. 이는 예방 가능한 failure이므로
topology reporting을 필수로 둔다.

### 5.9 CRD 정합성 (SMB)

proto에 `PROTOCOL_TYPE_SMB`과 `SmbExportParams`가 정의되어 있으나,
CRD의 `ProtocolType` enum(`pillarprotocol_types.go:25`)에 `smb`가 누락되어 있다.

Phase 3c에서 다음을 추가한다:
- `ProtocolType`에 `smb` 추가.
- `SMBConfig` struct 추가 (share naming convention, authentication mode 등).
- `PillarBinding.Overrides.Protocol`에 `SMB *SMBOverrides` 추가.

### 5.10 backend↔protocol 경계와 VolumeMode

#### 5.10.1 원칙: backend이 access type의 source of truth

현재 `controller.go:1261-1268`의 `accessTypeForProtocol()` 함수는
**protocol type → access type** 방향으로 매핑한다:

```go
func accessTypeForProtocol(pt agentv1.ProtocolType) agentv1.VolumeAccessType {
    case NFS, SMB: return MOUNT
    default:       return BLOCK
}
```

이 방향은 올바르지 않다. Access type은 **backend type**이 결정해야 한다:

- Block backend (zvol, LVM LV, block-device) → `VOLUME_ACCESS_TYPE_BLOCK`
- Filesystem backend (ZFS dataset, directory) → `VOLUME_ACCESS_TYPE_MOUNT`

Protocol은 backend이 생산한 것을 네트워크로 노출하는 방법이지,
backend의 출력 유형을 결정하는 것이 아니다.

실용적 참고: 호환성 매트릭스가 잘못된 조합을 방지하므로, 유효한 조합에서
`accessTypeForProtocol()`과 `accessTypeForBackend()`는 동일한 결과를 반환한다.
이 변경은 naming과 의존 방향의 정확성 개선이며, volume ID에서 backend type과
protocol type이 모두 추출 가능하므로 구현상 차이는 minimal하다.

수정 방향:

```go
func accessTypeForBackend(bt agentv1.BackendType) agentv1.VolumeAccessType {
    switch bt {
    case BACKEND_TYPE_ZFS_DATASET, BACKEND_TYPE_DIRECTORY:
        return VOLUME_ACCESS_TYPE_MOUNT
    default:
        return VOLUME_ACCESS_TYPE_BLOCK
    }
}
```

이 변경으로 `CreateVolume`은 protocol과 무관하게 backend type에 따라
agent에 올바른 access type을 전달한다.

#### 5.10.2 호환성 매트릭스 단일 정의

현재 backend↔protocol 호환성 검증이 두 곳에 중복된다:

1. `pillarbinding_webhook.go:265-268` — admission webhook
2. `pillarbinding_controller.go:440-473` — reconciler

호환성 매트릭스를 한 곳에 정의하고 양쪽이 참조해야 한다:

```go
// api/v1alpha1/compatibility.go
//
// BackendCategory classifies backends by their output type.
type BackendCategory int
const (
    BackendCategoryBlock      BackendCategory = iota // zvol, LVM, block-device
    BackendCategoryFilesystem                        // dataset, directory
)

func CategoryOf(bt BackendType) BackendCategory { ... }

type ProtocolCategory int
const (
    ProtocolCategoryBlock ProtocolCategory = iota // NVMe-oF, iSCSI
    ProtocolCategoryFile                          // NFS, SMB
)

func ProtocolCategoryOf(pt ProtocolType) ProtocolCategory { ... }

func Compatible(bt BackendType, pt ProtocolType) bool {
    return CategoryOf(bt) == BackendCategoryBlock &&
           ProtocolCategoryOf(pt) == ProtocolCategoryBlock ||
           CategoryOf(bt) == BackendCategoryFilesystem &&
           ProtocolCategoryOf(pt) == ProtocolCategoryFile
}
```

Webhook과 controller reconciler 양쪽이 이 함수를 호출한다.

Note: loopback mount (block backend → filesystem → NFS export) 같은 cross-category
조합은 이 RFC의 범위 밖이다. `Compatible()`은 strict category matching만 수행한다.

#### 5.10.3 VolumeMode 제약 선언

CSI의 `VolumeCapability`는 `Block` (raw block) 또는 `Mount` (filesystem)를 요청한다.
현재 `ValidateVolumeCapabilities` (`controller.go:307-346`)는 모든 access mode를
무조건 수락한다.

protocol type에 따라 지원되는 VolumeMode가 다르다:

| Protocol Category | Raw Block (VolumeMode: Block) | Filesystem (VolumeMode: Filesystem) |
|-------------------|------------------------------|-------------------------------------|
| Block (NVMe-oF, iSCSI) | ✓ | ✓ |
| File (NFS, SMB) | ✗ | ✓ |

`ValidateVolumeCapabilities`와 `CreateVolume`에서 이를 검증해야 한다:

```go
if volCap.GetBlock() != nil && protocolCategory == ProtocolCategoryFile {
    return nil, status.Error(codes.InvalidArgument,
        "raw block volume mode is not supported with file protocols (NFS/SMB)")
}
```

#### 5.10.4 backend 추가 시 protocol 코드 무수정 보장

이 RFC의 목표 중 하나는 **backend과 protocol의 독립적 확장**이다.

- 새 block backend 추가 (예: LVM thin): protocol 코드 수정 불필요.
  - `CategoryOf()`에 매핑 추가 + agent에 backend 구현 추가.
- 새 filesystem backend 추가 (예: Btrfs subvolume): protocol 코드 수정 불필요.
  - `CategoryOf()`에 매핑 추가 + agent에 backend 구현 추가.
- 새 protocol 추가 (예: iSCSI): backend 코드 수정 불필요.
  - `ProtocolCategoryOf()`에 매핑 추가 + node-side ProtocolHandler 구현 추가
    + agent에 export handler 추가.

교차점은 호환성 매트릭스뿐이며, 이는 단순 category 매칭이다.

참고: snapshot/clone은 backend-only 연산이며 protocol 코드와 무관하다.
이는 backend↔protocol 분리가 CSI feature set 전체에 걸쳐 유지됨을 확인한다.

### 5.11 agent-side protocol dispatch

현재 agent (`internal/agent/`)는 node-side와 동일하게 NVMe-oF에 결합되어 있다:

- `server_export.go:51-53`: `errOnlyNvmeofTCP`로 non-NVMe-oF 거부.
- `server.go:107`: `volumeNQN()` 함수가 NQN을 하드코딩 파생.
- `server.go:76`: `nqnMu sync.Map` lock이 NQN 기반.
- `nvmeof/` 패키지: configfs 전용 NVMe target 관리.
- `server_reconcile.go`: reconcile도 NVMe-oF만 지원.

agent에도 node-side와 유사한 protocol handler 추상화가 필요하다:

```go
// AgentProtocolHandler abstracts protocol-specific export operations.
type AgentProtocolHandler interface {
    // Export creates a network protocol target entry for a volume.
    Export(ctx context.Context, params ExportParams) (*ExportResult, error)
    // Unexport removes the protocol target entry.
    Unexport(ctx context.Context, volumeID string) error
    // AllowInitiator grants access to a specific initiator.
    AllowInitiator(ctx context.Context, volumeID, initiatorID string) error
    // DenyInitiator revokes access.
    DenyInitiator(ctx context.Context, volumeID, initiatorID string) error
    // Reconcile re-creates protocol state after reboot.
    Reconcile(ctx context.Context, desired []ExportDesiredState) error
}
```

- `server_export.go`의 switch 문이 protocol type에 따라 올바른 handler를 dispatch.
- 기존 `nvmeof/` 패키지는 `NVMeoFTCPAgentHandler`로 wrap.
- `volumeNQN()` → `volumeTargetID(protocolType, volumeID)` — protocol별 target ID 파생.
- `nqnMu` → `targetMu` — protocol별 target ID 기반 locking.

이 리팩토링은 Phase 3a에 포함한다.

### 5.12 NVMe transport variant 고려

현재 proto의 `PROTOCOL_TYPE_NVMEOF_TCP`은 TCP transport만 포함한다.
NVMe/RDMA (RoCE v2)는 동일한 NQN identity model을 공유하므로:

- **설계 방향**: `nvmeof-tcp`와 `nvmeof-rdma`를 별도 `ProtocolType`으로 둔다.
- **근거**: transport에 따라 kernel module (`nvme_tcp` vs `nvme_rdma`),
  connect 파라미터, 인프라 요구사항이 다르다.
- **공유 로직**: NQN 기반 identity resolution, configfs subsystem/namespace 관리는
  agent에서 공유. node-side Attach의 transport 파라미터만 분기.

이 결정은 foundation에서 확정하되, `nvmeof-rdma` 구현 자체는 이 RFC의 비목표다.
Proto에 `PROTOCOL_TYPE_NVMEOF_RDMA = 5`를 예약만 해둔다.

---

## 6. 구현 순서

### Phase 1. node identity 계약 교정 + metadata publication

Phase 1과 2를 통합한다.
controller가 CSINode annotation을 lookup하려면 node가 먼저 annotation을 써야 하므로,
identity 계약 교정과 publication은 **동시에** 이루어져야 한다.

- `node_id`를 stable node handle (K8s node name)로 확정한다.
- node-side publisher를 도입한다.
  - `pillar-node` 시작 시 `/etc/nvme/hostnqn` 읽기 → CSINode annotation 기록.
  - CSINode update를 위한 RBAC 설정 (DaemonSet ServiceAccount).
  - Helm chart `clusterrole.yaml` 업데이트.
- controller의 `node_id == NQN` 가정을 제거한다.
  - `controller.go:1472-1477`: `InitiatorId: nodeID` → CSINode annotation lookup 경로로 교체.
  - controller RBAC에 CSINode `get`, `list` 추가.
- annotation 미존재 시 명확한 `FailedPrecondition` 경로를 둔다.
- 관련 문서와 테스트 명세를 새 계약으로 맞춘다.

### Phase 1b. topology reporting

- `NodeGetInfo`에서 `AccessibleTopology`를 반환한다.
- 등록된 protocol handler를 기반으로 topology key를 결정한다.
- StorageClass의 `allowedTopologies` 문서화.

### Phase 2. node runtime generalization

- `Connector` 인터페이스 → `ProtocolHandler` 인터페이스로 교체.
  - `NVMeoFConnector` + `fabricsConnector` → `NVMeoFTCPHandler` (ProtocolHandler 구현).
  - `NodeServer.connector` → `NodeServer.handlers map[string]ProtocolHandler`.
  - `cmd/node/main.go`의 `newFabricsConnector()` → handler map 생성자.
- Layer 2 `VolumePresenter` 컴포넌트 도입.
- `nodeStageState` 일반화 (Section 5.5.1) + legacy migration (Section 5.5.2).
- `NodeStageVolume`에 protocol dispatch 로직 추가 (Section 5.4.3).
- `NodeUnstageVolume`에서 typed state로 disconnect/unmount 수행.
- `NodeExpandVolume`에 protocol type 확인 + file protocol no-op 추가.
- `ControllerExpandVolume`에 protocol-aware `NodeExpansionRequired` 추가.
- `ProtocolHandler.Rescan()` 호출을 `NodeExpandVolume`에 통합.
- VolumeContext 상수 rename + 주석 일반화.
- 에러 메시지 + state machine 주석에서 NVMe-oF 직접 참조 제거.
- NVMe-oF regression 없이 구조 변경이 완료되어야 한다.

### Phase 3a. agent-side protocol dispatch

- `server_export.go`에 `AgentProtocolHandler` 인터페이스 도입.
- 기존 `nvmeof/` 패키지를 `NVMeoFTCPAgentHandler`로 wrap.
- `volumeNQN()` → `volumeTargetID()` 일반화.
- `nqnMu` → `targetMu` 일반화.
- `server_reconcile.go`의 NVMe-oF 전용 gating 제거.

### Phase 3b. backend↔protocol 경계

- `accessTypeForProtocol()` → `accessTypeForBackend()`로 교체 (Section 5.10.1).
- 호환성 매트릭스를 `api/v1alpha1/compatibility.go`에 단일 정의 (Section 5.10.2).
- `ValidateVolumeCapabilities`에 protocol-aware VolumeMode 검증 추가 (Section 5.10.3).
- `supportedAccessModes`를 protocol-aware하게 분기 (Section 5.7.4).
- `CreateVolume`에서 raw block + file protocol 조합 거부 검증 추가.

### Phase 3c. CRD 정합성

- CRD에 SMB `ProtocolType` 및 config 추가 (Section 5.9).
- `make manifests` + `make generate` 실행.

### Phase 4. iSCSI 구현 착수

- 이 시점부터 [`docs/PRD-iscsi.md`](./PRD-iscsi.md)를 구현 대상으로 삼는다.

---

## 7. 수용 기준

다음이 충족되면 RFC가 완료된 것으로 본다.

Identity:
- `NodeGetInfo.node_id`가 K8s node name을 반환한다.
- node plugin이 CSINode annotation에 NVMe host NQN을 publish한다.
- controller publish/unpublish가 CSINode annotation lookup으로 initiator identity를 resolve한다.
- missing annotation 시 `FailedPrecondition`이 반환된다.
- Helm chart에 CSINode RBAC이 추가된다.

Topology:
- `NodeGetInfo`가 `AccessibleTopology`에 지원 protocol key를 반환한다.

Node runtime:
- `ProtocolHandler` 인터페이스가 정의되고 (`Attach`/`Detach`/`Rescan`) NVMe-oF TCP handler가 구현된다.
- `cmd/node/main.go`의 `fabricsConnector`가 `NVMeoFTCPHandler`로 리팩토링된다.
- `VolumePresenter`가 block/file 경로를 캡슐화한다.
- `nodeStageState`가 discriminated union으로 일반화되고, legacy state migration이 구현된다.
- `NodeStageVolume`이 protocol dispatch를 수행한다.
- `NodeExpandVolume`이 `Rescan()` 호출 후 resize를 수행하며, file protocol에서 no-op을 반환한다.
- `ControllerExpandVolume`이 file protocol에서 `NodeExpansionRequired: false`를 반환한다.

Agent:
- `AgentProtocolHandler` 인터페이스가 정의되고 NVMe-oF handler가 구현된다.
- `volumeNQN()` → `volumeTargetID()`, `nqnMu` → `targetMu`로 일반화된다.

Backend↔Protocol:
- `accessTypeForProtocol()`이 `accessTypeForBackend()`로 교체된다.
- 호환성 매트릭스가 단일 소스에서 정의되고 webhook/controller가 공유한다.
- `ValidateVolumeCapabilities`가 file protocol에서 raw block 요청을 거부한다.
- `supportedAccessModes`가 protocol-aware하게 동작한다.

CRD:
- `ProtocolType`에 SMB가 추가된다.

검증:
- **기존 NVMe-oF E2E 테스트 스위트가 모든 Phase 완료 후 통과한다.**
- 이후 iSCSI PRD를 별도 product workstream으로 실행할 수 있다.

---

## 8. 테스트 게이트

이 RFC 구현이 끝날 때 필요한 테스트 게이트:

- identity + topology 테스트
  - stable node handle
  - CSINode annotation resolution
  - missing identity annotation → FailedPrecondition
  - protocol별 identity key 선택
  - topology key reporting
- concurrency / idempotency 테스트
  - 서로 다른 NodeId가 서로 다른 resolved identity로 분기됨
- NVMe-oF regression
  - 기존 publish/unpublish와 node attach lifecycle이 계속 성립
  - `ProtocolHandler` 인터페이스 경유 시 기존 동작과 동일
  - **기존 E2E 테스트 스위트 전체 통과**
- node runtime 테스트
  - `ProtocolHandler.Attach/Detach/Rescan` 단위 테스트
  - `nodeStageState` 직렬화/역직렬화 (각 protocol type)
  - **legacy `nodeStageState` migration 테스트** (old `subsys_nqn` → new format)
  - `NodeStageVolume` protocol dispatch 테스트
  - `NodeExpandVolume` file protocol no-op 테스트 + Rescan 호출 테스트
  - `ControllerExpandVolume` protocol-aware NodeExpansionRequired 테스트
  - `VolumePresenter` block/file 경로 단위 테스트
- agent 테스트
  - `AgentProtocolHandler` 인터페이스 단위 테스트
  - `volumeTargetID()` protocol별 파생 테스트
  - NVMe-oF agent handler regression
- backend↔protocol 경계 테스트
  - `accessTypeForBackend()` 단위 테스트 (block backend → BLOCK, fs backend → MOUNT)
  - 호환성 매트릭스 단위 테스트 (block backend + NFS → 거부, fs backend + NVMe-oF → 거부)
  - `ValidateVolumeCapabilities`: file protocol + raw block → InvalidArgument
  - `supportedAccessModes`: file protocol + RWX → 수락, block protocol + RWX → 거부
  - `CreateVolume`: fs backend가 MOUNT access type으로 agent에 전달되는지 확인
- packaging contract 테스트
  - `attachRequired: true`가 controller-side publish contract로 해석됨
  - `STAGE_UNSTAGE_VOLUME`이 보고되고 모든 protocol이 Stage를 거침

iSCSI protocol 자체의 E2E는 이 RFC가 아니라 `PRD-iscsi`의 게이트다.

---

## 9. 오픈 이슈

- `CSINode` annotation이 future protocol facts까지 계속 수용 가능한지
  아니면 후속 RFC에서 dedicated pillar CRD(`PillarNode`)로 승격할지.
  - 판단 기준: annotation key가 5개 이상이거나 schema validation이 필요해지면 CRD 승격.
  - NetApp Trident은 `TridentNode` CRD를 사용한다 (참고).
- metadata publisher를 `pillar-node` 내부로 둘지 helper로 둘지.
- future file protocol에서 publish/unpublish를 no-op로 둘지
  authorization/policy hook로 둘지.
  - NFS: export client list 관리 (agent의 AllowInitiator) → no-op이 아님.
  - SMB: Samba 자체 인증 위임 가능 → no-op 후보.
- NFS identity resolution 시 `CSINode` annotation vs `Node.status.addresses` 직접 조회.
- NVMe/RDMA를 별도 `ProtocolType`으로 둘지 NVMe-oF의 transport 파라미터로 둘지
  최종 확정 (이 RFC에서는 별도 type 방향을 권장).
- RWX multi-node grant의 구체 구현 시점과 reference counting 설계.
- `ProtocolHandler` interface의 multipath 확장 방향.

이 이슈들은 RFC 구현 중 결정하되, 위 핵심 계약은 유지한다.

---

## 10. 산업 참고

| CSI Driver | 전략 | node_id | identity 저장 |
|-----------|------|---------|-------------|
| **NetApp Trident** | 단일 driver, multi-protocol | K8s node name | TridentNode CRD |
| **democratic-csi** | 프로토콜별 별도 driver | driver별 상이 | driver에 내장 |
| **HPE CSI** | 단일 driver + CSP sidecar | 미문서화 | CSP가 관리 |
| **csi-driver-nfs** | NFS 전용 | K8s node name | 불필요 (NFS는 identity 없음) |
| **csi-driver-smb** | SMB 전용 | K8s node name | Secret 기반 |

`pillar-csi`는 Trident 모델과 가장 유사하며, 단일 driver에서
block + file protocol을 모두 수용하는 방향이다.

---

## 11. 프로토콜 범위

### 지원 대상

| 우선순위 | Protocol | 유형 | Identity Model |
|---------|----------|------|---------------|
| P0 | NVMe-oF TCP | Block | Host NQN |
| P1 | iSCSI | Block | Initiator IQN |
| P2 | NFS v4 | File | Client IP |
| P3 | SMB/CIFS | File | Username/Secret |
| Future | NVMe/RDMA | Block | Host NQN (TCP와 공유) |

### 검토 후 제외

| Protocol | 유형 | 제외 이유 |
|---------|------|----------|
| Fibre Channel | Block | 전용 HBA + FC switch 필요. 로컬 스토리지와 무관 |
| FCoE | Block | FC의 Ethernet 변형. 쇠퇴 기술 |
| InfiniBand (SRP/iSER) | Block | 전용 InfiniBand 인프라 필요. HPC 전용 |
| AoE | Block | L2 only, ACL 없음, IP routing 불가. 사실상 폐기 |
| DRBD/NBD | Block | LINSTOR CSI driver가 이미 성숙. 복제 중심 |
| Ceph RBD/CephFS | Block/File | RADOS 프로토콜. ceph-csi driver가 표준 |
| GlusterFS | File | CSI driver deprecated (2018) |
| Lustre | File | HPC 전용 parallel filesystem |
| FUSE (S3/Swift) | File | 클라우드/오브젝트 스토리지. 로컬 스토리지와 무관 |
| virtio-blk/scsi | Block | hypervisor 레벨 device. 네트워크 프로토콜이 아님 |

---

## 12. 핸드오프

이 RFC가 구현되면 다음 문서를 실행 대상으로 넘긴다.

- [`docs/PRD-iscsi.md`](./PRD-iscsi.md)

순서:

1. 이 RFC 구현
2. RFC acceptance criteria 통과 (E2E 포함)
3. iSCSI PRD 구현 시작
