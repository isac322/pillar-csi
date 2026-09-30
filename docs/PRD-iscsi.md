# pillar-csi iSCSI Support PRD

상태: **구현됨**. 이 문서는 `pillar-csi`의 iSCSI 프로토콜 지원을 구현된 그대로 기술한다. 공통 CRD 모델과 프로토콜 union, override 문서 규칙은 [`PRD.md`](./PRD.md)를 따른다.

## 1. 문서 목적

이 문서는 `pillar-csi`가 제공하는 `iSCSI` 프로토콜 기능의 제품 계약을 정리한다.

- 사용자는 iSCSI를 어떤 리소스 조합으로 사용하는가
- 어떤 파라미터를 어느 계층에 둘 수 있는가
- 스토리지 노드(agent)와 워커 노드(node)는 iSCSI를 어떤 방식으로 구현하는가
- 노드에 무엇을 설치해야 하는가 (커널 모듈만)
- 무엇을 지원하고 무엇을 지원하지 않는가
- 왜 이 구조를 선택했는가

### 1.1 기반 RFC

iSCSI는 [`docs/RFC-multi-protocol-driver-foundation.md`](./RFC-multi-protocol-driver-foundation.md)가 정한 multi-protocol driver 계약 위에 구현되었다.

- `NodeGetInfo.node_id`는 모든 프로토콜에서 stable node handle이다.
- 프로토콜별 initiator identity는 `CSINode` annotation에서 조회한다.
- agent와 node는 프로토콜별 handler를 통해 export / attach를 수행한다.

## 2. 현재 상태

iSCSI는 NVMe-oF/TCP와 같은 수준의 block protocol로 구현되어 있다.

- CRD: `PillarProtocol.spec.protocol.iscsi` union 멤버가 있다. CEL 규칙은 `nvmeofTcp`와 `iscsi` 중 정확히 하나만 허용한다. 위반 시 메시지는 `exactly one protocol member must be set (supported: nvmeofTcp, iscsi)`이다.
- override: `PillarStorageClass.spec.overrides.protocol.iscsi`와 PVC annotation `pillar-csi.bhyoo.com/protocol`의 `iscsi` 문서가 있다.
- controller: `CreateVolume`이 iSCSI export를 만들고 VolumeContext를 채운다. `ControllerPublish/Unpublish`는 `CSINode` annotation의 initiator IQN으로 ACL을 관리한다.
- agent: LIO iSCSI target을 configfs로 직접 구성한다. 외부 CLI는 쓰지 않는다.
- node: `pillar-node` 안의 pure-Go initiator가 로그인하고 커널 `iscsi_tcp`에 연결을 넘긴다. 외부 CLI나 daemon은 쓰지 않는다.
- agent RPC: 기존 proto를 그대로 쓴다. `PROTOCOL_TYPE_ISCSI`와 `ExportParams.iscsi = IscsiExportParams{bind_address, port}`.

## 3. 왜 iSCSI가 필요한가

### 3.1 배경

NVMe-oF는 성능과 현대성 측면에서 유리하지만, self-hosted 환경에는 iSCSI 수요가 여전히 있다.

- 더 넓은 OS/배포판 호환성
- 더 익숙한 운영 경험
- 기존 NAS/SAN/가상화 환경과의 연결 용이성
- NVMe-oF를 쓰기 어려운 커널이나 네트워크 환경의 대안

### 3.2 제품 목표

`pillar-csi`의 iSCSI는 "기존 iSCSI target을 Kubernetes에서 마운트하는 static driver"가 아니다. `pillar-csi`의 선언형 CRD 모델 안에서 동적 프로비저닝과 attach/mount lifecycle을 제공하는 block protocol 옵션이다.

### 3.3 제품 포지셔닝

최종 목표는 "iSCSI를 쓰는 또 다른 CSI driver"를 추가하는 것이 아니라, **`pillar-csi`가 iSCSI 경로까지 포함하는 하나의 CSI driver가 되는 것**이다.

- 드라이버 이름은 계속 `pillar-csi.bhyoo.com`이다.
- 배포 토폴로지는 기존과 같이 `pillar-controller` + `pillar-node` + `pillar-agent`이다.
- 외부 프로비저너/어태처/리사이저와 node-driver-registrar/livenessprobe를 포함한 Kubernetes CSI 표준 통합은 그대로 유지된다.
- iSCSI는 그 위에서 선택 가능한 `PillarProtocol`(`spec.protocol.iscsi`)이다.
- 노드에는 커널 모듈 외에 아무것도 설치하지 않는다 (§9).

## 4. 제품 목표와 비목표

### 4.1 목표

- `zfs-zvol`, `lvm-lv` block backend를 `iscsi`로 export할 수 있다.
- `PillarProtocol` / `PillarStorageClass` / PVC override라는 기존 계층 모델을 그대로 쓴다.
- 사용자가 `targetPortal`, `iqn`, `lun` 같은 저수준 iSCSI 세부 값을 직접 다루지 않는다.
- `Filesystem`과 `Block` volumeMode를 모두 지원한다.
- core CSI block lifecycle을 NVMe-oF와 같은 수준으로 제공한다.
  - Create/Delete
  - Identity (`GetPluginInfo`, `GetPluginCapabilities`, `Probe`)
  - `ValidateVolumeCapabilities`
  - `ControllerGetCapabilities`
  - ControllerPublish/Unpublish
  - `NodeGetCapabilities`
  - `NodeGetInfo`
  - NodeStage/Unstage
  - NodePublish/Unpublish
  - ControllerExpand + NodeExpand (온라인)
  - `NodeGetVolumeStats`
- local attach와 재시작 복구를 NVMe-oF와 같은 수준으로 제공한다.
- 노드 사용자 공간에 iSCSI 도구를 설치하지 않는다. 필요한 것은 커널 모듈뿐이다.

### 4.2 비목표

- iSCSI RWX 제공
- iSCSI 자체만으로 CSI snapshot/clone 제공 (snapshot/clone은 제품 전체에서 미지원)
- 앱 팀이 `targetPortal`/`IQN`/`LUN`을 직접 입력하는 static PV UX
- `PillarAgent.spec.external` 또는 외부 SAN/NAS의 pre-existing iSCSI target을 소비하는 static-import UX
- Windows initiator 지원
- CHAP 인증
- multipath / multi-portal

## 5. 외부 드라이버 조사와 시사점

### 5.1 kubernetes-csi/csi-driver-iscsi

관찰:

- README 기준 이 드라이버는 "existing and already configured iscsi server"를 전제로 한다.
- examples는 PV에서 `targetPortal`, `iqn`, `lun`, `fsType`을 직접 다룬다.
- 즉 "이미 존재하는 iSCSI target에 Kubernetes에서 연결"하는 static/low-level 모델에 가깝다.

시사점:

- `pillar-csi`는 이 모델을 그대로 따르지 않는다.
- app/operator가 volume마다 portal/IQN/LUN을 다루면 `pillar-csi`의 CRD 기반 동적 프로비저닝 가치가 사라진다.
- 따라서 `pillar-csi`는 controller가 target IQN과 LUN을 생성하고, PV `VolumeContext`에만 runtime 정보로 기록한다.
- node identity 역시 raw IQN을 그대로 노출하는 static 드라이버가 아니라, stable node handle과 protocol-specific identity를 분리한다.

### 5.2 democratic-csi

관찰:

- `freenas-iscsi`, `freenas-api-iscsi`, `zfs-generic-iscsi`, `synology-iscsi` 등 driver 종류가 backend/protocol 조합별로 나뉜다.
- 문서상 resize, snapshots, clones 등 CSI 기능 폭이 넓다.
- 노드에 iSCSI initiator 사용자 공간 패키지 설치와 multipath 설정을 별도 운영 작업으로 요구한다.

시사점:

- `pillar-csi`는 "driver per protocol/back-end combination" 대신 `Backend`와 `Protocol`을 분리한 CRD 모델을 유지한다.
- iSCSI는 새 CSI driver 추가가 아니라 `PillarProtocol`(`spec.protocol.iscsi`) 추가다.
- 노드 사전 설치 요구는 `pillar-csi`의 운영 모델과 맞지 않는다. `pillar-csi`는 커널 모듈만 요구한다.

### 5.3 HPE CSI Driver

관찰:

- `StorageClass`에서 `accessProtocol: iscsi`를 선택한다.
- CHAP을 Secret 기반으로 다룬다.
- raw block, expansion, snapshot 등 Kubernetes 표준 사용 방식을 따른다.

시사점:

- expansion은 controller와 node가 결합된 online expansion이며, attach된 상태에서 완결된다.
- CHAP을 도입한다면 plain-text CRD 필드가 아니라 Secret 참조 기반이어야 한다. 현재 `pillar-csi`는 CHAP을 지원하지 않는다.

### 5.4 조사 결론

- 저수준 iSCSI 연결 정보는 사용자 입력이 아니라 controller가 생성/관리한다.
- 프로토콜 선택은 `PillarProtocol`, class-level 세부 정책은 `PillarStorageClass`, volume-level 튜닝은 PVC override로 제한한다.
- snapshots/clones는 iSCSI 전용 기능이 아니라 block backend 공통 기능으로 다룬다.
- 다른 드라이버는 대체로 "단일 protocol 전용 driver" 또는 "backend/protocol 조합별 driver"이며 노드 사전 설치를 전제한다. `pillar-csi`는 하나의 driver 안에서 NVMe-oF와 iSCSI를 함께 제공하고 노드 사전 설치를 요구하지 않는다.

## 6. 사용자 경험(UX)과 사용 흐름

### 6.1 클러스터 관리자 흐름

클러스터 관리자는 기존과 동일하게 4개 리소스 조합으로 iSCSI를 사용한다.

1. `PillarAgent`
   - `nodeRef` 기반 storage node를 정의한다.
2. `PillarStore`
   - `zfs-zvol` 또는 `lvm-lv` pool을 정의한다.
3. `PillarProtocol`
   - `spec.protocol.iscsi`에 port, ACL, 세션 타이머 기본값을 정의한다.
4. `PillarStorageClass`
   - store + protocol을 결합해 StorageClass를 생성한다.

노드에는 커널 모듈만 있으면 된다. chart의 init container가 모듈을 best-effort로 로드한다 (§8.8).

### 6.2 앱 팀 흐름

앱 팀이 직접 알아야 하는 것은 다음뿐이다.

- 어떤 StorageClass를 쓸지
- `Filesystem`으로 쓸지 `Block`으로 쓸지
- 필요한 경우 PVC annotation으로 허용된 튜닝 값을 덮어쓸지

앱 팀이 알 필요가 없는 값:

- target IQN
- portal IP/port
- LUN
- ACL 생성/삭제 순서
- node initiator IQN

### 6.3 사용 예시

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: iscsi-default
spec:
  protocol:
    iscsi:
      port: 3260
      acl: true
      replacementTimeout: 120
```

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: pillar-iscsi
spec:
  storeRef: storage-1-hot
  protocolRef: iscsi-default
  storageClass:
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true
  filesystem:
    fsType: xfs
  overrides:
    protocol:
      iscsi: {loginTimeout: 30}
```

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: app-data
  annotations:
    pillar-csi.bhyoo.com/protocol: |
      iscsi:
        replacementTimeout: 180
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: pillar-iscsi
  resources:
    requests:
      storage: 20Gi
```

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-disk
spec:
  accessModes: ["ReadWriteOnce"]
  volumeMode: Block
  storageClassName: pillar-iscsi
  resources:
    requests:
      storage: 40Gi
```

## 7. 파라미터 계층 설계

핵심 원칙은 "구조적 파라미터는 상위 계층에서만, 튜닝 파라미터만 하위 계층에서 override"다. 프로토콜 파라미터는 flat key가 아니라 `iscsi` 멤버를 가진 문서로만 표현한다.

### 7.1 계층별 책임

| 계층 | 위치 | 책임 | iSCSI에서 다루는 값 |
|------|------|------|---------------------|
| 설치/배포 | Helm values / node DaemonSet | 노드 커널 모듈과 런타임 준비 | `node.initModprobe.modules`(`iscsi_tcp`), `agent.initModprobe.modules`(LIO 모듈), `node.hostNetwork`, `node.iscsi.netlinkNetnsPath` |
| 프로토콜 기본값 | `PillarProtocol.spec.protocol.iscsi` | 클러스터 공통 transport/security/timer 정책 | `port`, `acl`, `loginTimeout`, `replacementTimeout`, `noopOutInterval`, `noopOutTimeout` |
| 클래스별 조정 | `PillarStorageClass.spec.overrides.protocol.iscsi` | 특정 StorageClass에만 적용할 타이머 정책 | 네 타이머 필드 |
| 파일시스템 | `PillarStorageClass.spec.filesystem` / PVC annotation `pillar-csi.bhyoo.com/filesystem` | 포맷과 마운트 | `fsType`, `mkfsOptions`, `mountOptions` (프로토콜 공통) |
| 볼륨 단위 튜닝 | PVC annotation `pillar-csi.bhyoo.com/protocol` | 안전한 미세 조정 | 네 타이머 필드 |
| 런타임 연결 정보 | PV `volumeAttributes` / CSI `VolumeContext` | attach/mount에 필요한 실제 export 정보 | `target_id`(IQN), `address`, `port`, `protocol-type=iscsi`, 해석된 타이머 |

### 7.2 `PillarProtocol.spec.protocol.iscsi`

`PillarProtocol`에는 "모든 binding이 공유해도 이상하지 않은 프로토콜 정책"만 둔다. 모든 시간 단위는 초다.

| 필드 | 타입 | 기본값 / 범위 | 의미 |
|------|------|---------------|------|
| `port` | int32 | 기본 3260, 1-65535 | agent가 여는 target portal 포트 |
| `acl` | bool | 기본 `false` | `true`면 publish된 노드의 initiator IQN만 허용 |
| `loginTimeout` | int32 | 최소 1, 미설정 시 노드 기본 15 | 로그인 한 번의 제한 시간 |
| `replacementTimeout` | int32 | 최소 0, 미설정 시 노드 기본 120 | 세션 장애 중 I/O를 큐에 보관하는 시간 (session recovery timeout) |
| `noopOutInterval` | int32 | 최소 0, 미설정 시 노드 기본 5 | 유휴 연결 NOP-Out ping 간격, 0이면 끔 |
| `noopOutTimeout` | int32 | 최소 0, 미설정 시 노드 기본 5 | NOP-In 응답 대기 시간 |

타이머 변경은 변경 이후 로그인하는 세션에 적용된다. 이미 stage된 볼륨은 자신의 `CreateVolume` 시점에 해석된 값을 유지한다.

`PillarProtocol`에 두지 않는 값:

- 구체적인 target portal 주소 (agent의 `PillarAgent.status.resolvedAddress`에서 온다)
- volume별 target 식별자 (controller/agent가 파생한다)

### 7.3 `PillarStorageClass`에 둘 값

`PillarStorageClass`는 generated `StorageClass`의 제품 표면이다.

- `spec.overrides.protocol.iscsi`: `loginTimeout`, `replacementTimeout`, `noopOutInterval`, `noopOutTimeout`
- `spec.filesystem`: `fsType`(`ext4`/`xfs`), `mkfsOptions`, `mountOptions`
- `spec.storageClass`: `reclaimPolicy`, `volumeBindingMode`, `allowVolumeExpansion`

override 문서의 멤버는 참조한 `PillarProtocol`의 멤버와 같아야 한다. `nvmeofTcp` 프로토콜을 참조하는 binding에 `iscsi` override를 넣으면 거부된다.

### 7.4 PVC annotation에 둘 값

PVC annotation `pillar-csi.bhyoo.com/protocol`은 YAML 문서다. 마지막 레이어의 튜닝만 허용한다.

```yaml
pillar-csi.bhyoo.com/protocol: |
  iscsi:
    loginTimeout: 30
```

허용:

- `loginTimeout`
- `replacementTimeout`
- `noopOutInterval`
- `noopOutTimeout`

금지 (구조적 필드):

- `port`
- `acl`

구조적 필드를 넣으면 `pillar-csi.bhyoo.com/protocol: iscsi.acl is structural and cannot be set per volume` 같은 메시지로 거부된다. 문서의 멤버가 프로토콜 멤버와 다를 때도 거부된다.

### 7.5 값이 VolumeContext로 전달되는 방식

`CreateVolume`은 `PillarProtocol` → `PillarStorageClass.spec.overrides` → PVC annotation 순으로 값을 해석하고, 결과를 VolumeContext에 기록한다. node는 VolumeContext만 보고 로그인한다.

| VolumeContext 키 | 값 |
|------------------|----|
| `target_id` | target IQN |
| `address` | portal IP |
| `port` | portal 포트 |
| `pillar-csi.bhyoo.com/protocol-type` | `iscsi` |
| `pillar-csi.bhyoo.com/iscsi-login-timeout` | 10진수 초, 해석된 값이 있을 때만 |
| `pillar-csi.bhyoo.com/iscsi-replacement-timeout` | 10진수 초, 해석된 값이 있을 때만 |
| `pillar-csi.bhyoo.com/iscsi-noop-out-interval` | 10진수 초, 해석된 값이 있을 때만 |
| `pillar-csi.bhyoo.com/iscsi-noop-out-timeout` | 10진수 초, 해석된 값이 있을 때만 |

키가 없으면 node 기본값(15/120/5/5초)을 쓴다.

## 8. 구현 설계

### 8.1 export 모델 (agent)

agent는 LIO를 configfs(`/sys/kernel/config/target/iscsi/`)로 직접 구성한다. nvmet과 같은 방식이며 외부 CLI는 쓰지 않는다.

- volume마다 iSCSI target 하나를 만든다.
- target IQN은 `iqn.2026-01.com.bhyoo.pillar-csi:` + agent volume ID이며, `/`는 `.`로 바꾼다.
  - 예: `iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc`
- target마다 TPG 1(`tpgt_1`) 하나를 둔다.
- TPG에 LUN 0 하나를 두고, zvol 또는 LV를 iblock backstore로 연결한다.
- backstore를 활성화한 뒤 `attrib/emulate_tpu=1`을 써서 thin provisioning(UNMAP)을 광고한다. LIO 기본값은 0이라 광고하지 않으면 node의 `/dev/sdX`가 `discard_max_bytes=0`이 되어 discard가 zvol/LV에 닿지 않는다. 이미 활성화된 backstore(agent 재시작)도 0이면 1로 올린다. 쓴 뒤 다시 읽어 확인한다.
  - backing 디바이스가 discard를 지원하지 않으면 커널이 `ENOSYS`로 거부한다(`target_try_configure_unmap`). 이때는 0으로 두고 UNMAP 없이 export하며 agent가 Info 로그를 남긴다. 다른 오류는 export 실패다.
  - 공간은 node가 discard를 보내야 반환된다. pillar-node가 스테이지한 filesystem 볼륨을 주기적으로 trim하므로(기본 매주, `--trim-interval`; PVC·클래스 filesystem 문서의 `periodicTrim: false`로 제외, PRD §5.5) 별도 작업이 필요 없다. 더 빨리 돌려받으려면 `fstrim`을 직접 돌리거나 `discard` 옵션으로 마운트한다. raw Block 볼륨은 trim하지 않는다.
- network portal은 `<bind 주소>:<port>` 하나다. bind 주소는 `PillarAgent.status.resolvedAddress`의 IP다.
- `zfs-zvol`과 `lvm-lv`에 동일하게 적용된다.

ACL:

- `acl: false`: TPG demo mode(`generate_node_acls=1`). portal에 도달하는 모든 initiator가 접속할 수 있다.
- `acl: true`: `ControllerPublishVolume`에서 해당 노드 initiator IQN의 node ACL을 만들고, `ControllerUnpublishVolume`에서 지운다. ACL이 없는 initiator의 로그인은 거부된다.

agent는 LIO iSCSI target을 쓸 수 있을 때만(`target_core_mod` 로드, `target/iscsi` 생성 가능) `GetCapabilities`와 `PillarAgent` status의 protocols에 iSCSI를 보고한다.

이 모델의 장점:

- 식별자 충돌이 적다.
- ACL과 disconnect semantics가 단순하다.
- volume 하나가 target 하나라서 volume별 세션과 장애 격리가 명확하다.

### 8.2 node initiator identity

iSCSI에서 `ControllerPublishVolume`은 node의 initiator IQN을 알아야 한다.

- `NodeGetInfo.node_id`는 모든 프로토콜에서 stable node handle이다. raw transport identity는 `node_id`에 싣지 않는다.
- `pillar-node`는 호스트의 `/etc/iscsi/initiatorname.iscsi`(hostPath `/etc/iscsi`, `DirectoryOrCreate`)에서 `InitiatorName=`을 읽는다.
- 파일이나 값이 없으면 `iqn.2026-01.com.bhyoo.pillar-csi:node.<32자리 소문자 hex>`를 생성해 `InitiatorName=<iqn>` 형식(0644)으로 저장한다.
- `pillar-node`는 이 값을 `CSINode` annotation `pillar-csi.bhyoo.com/iscsi-initiator-iqn`으로 게시한다.
- controller는 `ControllerPublishVolume`에서 이 annotation을 읽는다. 없으면 `FailedPrecondition`을 반환하고 재시도를 기다린다.
- 사용자가 annotation을 직접 편집할 필요는 없다.
- uninstall은 `pillar-node`가 만든 `/etc/iscsi/initiatorname.iscsi`를 지우지 않는다.
- iSCSI 전용 topology key는 없다.

### 8.3 node attach/stage 모델

node는 `pillar-node` 프로세스 안의 pure-Go iSCSI initiator(`internal/iscsi`)를 쓴다.

- TCP 연결과 iSCSI 로그인(RFC 7143 login PDU, AuthMethod=None)을 Go에서 수행한다.
- 로그인이 끝나면 NETLINK_ISCSI로 연결을 커널 `iscsi_tcp`에 넘긴다. 세션/연결 생성, 파라미터 설정, full-feature phase 시작이 모두 이 netlink 프로토콜로 이뤄진다.
- 커널이 LUN 0에 대한 SCSI 디바이스(`/dev/sdX`)를 만든다. `pillar-node`는 sysfs에서 디바이스를 찾고, 컨테이너 `/dev`에 노드가 없으면 sysfs의 major/minor로 만든다.
- 로그인은 (target IQN, portal) 단위로 멱등이다.

NodeStage / NodeUnstage:

- `NodeStageVolume`: VolumeContext의 target/portal/타이머로 로그인 → LUN 0 디바이스 확인 → `Filesystem`이면 포맷(필요 시)과 staging mount.
- stage 상태는 `ISCSIStageState{TargetIQN, Address, Port, LUN}`으로 JSON 키 `iscsi` 아래에 저장된다.
- `NodeUnstageVolume`: unmount 후 logout. logout은 멱등이다. logout은 세션이 `LOGGED_IN`인 동안 LUN 블록 디바이스를 fsync(page cache write-back + SYNCHRONIZE CACHE)하고 sysfs `delete`로 SCSI 디바이스를 제거한 뒤에 Logout PDU / 세션 파기를 수행한다. 연결이 끊겨 세션이 `FAILED`(커널이 `replacementTimeout` 동안 I/O를 큐에 보관 중)이면 SCSI 디바이스가 있는 세션은 파기하지 않고 복구를 계속하며 재시도 가능한 오류로 실패해 kubelet이 재시도하게 하고, 이미 timeout이 지나 `FREE`가 된 세션은 flush할 수 없으므로 건너뛰고 로그로 남긴 뒤 파기한다.

세션 복구:

- 연결 오류가 나면 커널이 이벤트를 보낸다. `pillar-node`가 프로세스 안에서 연결을 정리하고 재로그인한다.
- 복구 중 I/O는 `replacementTimeout` 동안 커널 큐에 머문다. 이 시간이 지나면 I/O는 실패로 올라간다.
- `noopOutInterval`/`noopOutTimeout`으로 끊긴 연결을 감지한다.

재시작 복구:

- `pillar-node`는 시작할 때 sysfs에서 기존 세션을 찾아 입양(adopt)한다. 대상은 target IQN이 pillar prefix(`iqn.2026-01.com.bhyoo.pillar-csi:`)로 시작하고 initiator 이름이 노드 IQN인 세션뿐이다.
- 입양한 세션도 이후 연결 오류 시 같은 방식으로 복구한다.

protocol handler 책임:

- NVMe-oF: connect / disconnect / rescan / device resolve
- iSCSI: login / logout / rescan / device resolve
- 그 위의 block device lifecycle은 공통이다.
  - `volumeMode: Filesystem`의 mkfs/mount
  - `volumeMode: Block`의 raw device publish
  - `NodeGetVolumeStats`
  - `NodeExpandVolume`의 filesystem grow

#### 8.3.1 NETLINK_ISCSI와 네트워크 네임스페이스

커널은 NETLINK_ISCSI 소켓을 호스트 init network namespace에만 만든다. 그래서 `pillar-node`는 `hostNetwork: true`로 동작해야 하며, chart 기본값이 이미 그렇다.

- 운영 노드: `hostNetwork: true`면 추가 설정이 필요 없다.
- Kind 같은 중첩 컨테이너 노드: 노드 자체가 별도 netns에 있으므로 chart 값 `node.iscsi.netlinkNetnsPath`에 호스트 init netns 파일(예: `/host/proc/1/ns/net`)을 지정한다. chart는 `--iscsi-netlink-netns=<path>` 플래그를 넘기고 그 디렉터리를 읽기 전용으로 마운트한다.
- 플래그가 있으면 `pillar-node`는 OS 스레드를 해당 netns로 옮겨 NETLINK_ISCSI 소켓만 만들고 원래 netns로 돌아온다. TCP 소켓은 pod 자신의 netns에 남는다.

#### 8.3.2 호스트 iscsid와의 공존

호스트에 open-iscsi의 `iscsid`가 따로 떠 있어도 `pillar-node`는 동작한다. `pillar-node`는 target IQN이 pillar prefix로 시작하고 initiator 이름이 노드 IQN인 세션만 관리한다. 다른 세션은 건드리지 않는다.

#### 8.3.3 커널 모듈이 없을 때

`pillar-node` 시작 시 `iscsi_tcp`가 로드되어 있지 않으면(`/sys/class/iscsi_transport/tcp` 없음) iSCSI handler가 비활성화된다.

- 로그: `iSCSI initiator disabled: kernel module iscsi_tcp is not loaded`
- initiator IQN을 게시하지 않는다.
- iscsi 볼륨의 `NodeStageVolume`은 실패한다.
- 조치: 모듈을 로드하고 `pillar-node`를 재시작한다.

### 8.4 filesystem / raw block

두 volumeMode를 모두 지원한다.

- `volumeMode: Filesystem`
  - stage 시 `mkfs` + mount
  - `fsType`은 `ext4`/`xfs`
  - 이미 파일시스템이 있는 볼륨은 다시 포맷하지 않는다
- `volumeMode: Block`
  - login 후 raw block device를 pod에 노출
  - KubeVirt/DB/WAL 용도에 중요

### 8.5 확장

online expansion을 지원한다.

- `ControllerExpandVolume`
  - agent가 zvol/LV 크기를 늘린다. LIO iblock backstore는 새 크기를 읽는다.
- `NodeExpandVolume`
  - 세션의 SCSI 디바이스를 rescan해 새 크기를 반영한다 (온라인).
  - `Filesystem`이면 `resize2fs`/`xfs_growfs`로 파일시스템을 늘린다.

### 8.6 보안 모델

- `acl: true`면 node initiator IQN 기반 ACL을 쓴다.
- `acl: false`(기본값)면 portal에 도달하는 모든 initiator가 접속할 수 있다.
- CHAP은 지원하지 않는다.
- 데이터는 암호화되지 않는다 (NVMe/TCP와 같음). 스토리지 네트워크를 분리하고 `acl: true`를 쓰는 것을 권장한다.

### 8.7 multipath

multipath / multi-portal은 지원하지 않는다.

- target마다 portal 하나, 노드마다 세션 하나다.
- single portal/single path만으로도 대부분의 homelab/on-prem 환경을 커버할 수 있다.
- 세션 장애는 `replacementTimeout` 범위 안의 in-process 재로그인으로 처리한다.

### 8.8 CSI driver packaging과 배포 요구사항

`pillar-csi`가 iSCSI 경로까지 포함하는 CSI driver로 동작하도록 배포 구조를 유지한다.

- `pillar-controller`는 단일 driver 이름(`pillar-csi.bhyoo.com`)으로 CSI Controller + Identity 서비스를 제공한다.
- `pillar-node`는 같은 driver 이름으로 CSI Node + Identity 서비스를 제공한다.
- controller 측 sidecar는 `external-provisioner`, `external-attacher`, `external-resizer`, `livenessprobe`를 유지한다.
- node 측 sidecar는 `node-driver-registrar`, `livenessprobe`를 유지한다.
- `pillar-agent`는 기존과 같이 `hostPort`로 노드 IP에서 접근 가능하다. iSCSI 때문에 별도 agent 배포 토폴로지를 만들지 않는다.
- `pillar-node`와 `pillar-agent`는 `hostNetwork: true`(chart 기본값)로 동작한다.
- `pillar-node`는 호스트 `/etc/iscsi`를 hostPath(`DirectoryOrCreate`)로 마운트한다.
- 노드 이미지에 iSCSI 사용자 공간 도구를 넣지 않는다. 노드 호스트에도 설치하지 않는다.

커널 모듈:

| 역할 | 모듈 | chart 값 |
|------|------|----------|
| agent (target) | `target_core_mod`, `target_core_iblock`, `iscsi_target_mod` (configfs는 `/sys/kernel/config`에 마운트) | `agent.initModprobe.modules` |
| node (initiator) | `iscsi_tcp` (`libiscsi`, `libiscsi_tcp`, `scsi_transport_iscsi`를 함께 로드) | `node.initModprobe.modules` |

### 8.9 재시작 복구 (agent)

- agent는 재시작 후 controller의 export 목록으로 LIO target을 다시 만든다. NVMe-oF(nvmet)와 같은 ExportsReady 게이트와 `PillarVolumeState`의 `spec.resolved`/`exportSpec`을 쓴다.
- NVMe-oF와 같이 목록에 있는 볼륨의 target만 수정한다. 목록에 없는 pillar 소유 target은 지우지 않으며, fencing이 적용된 UnexportVolume/DeleteVolume으로만 제거된다.
- 같은 주소/포트의 portal을 여러 target이 공유하므로, 모든 export를 구성한 뒤에 target을 활성화한다.

### 8.10 local attach

`PillarStorageClass.spec.localAttach`가 켜진 볼륨이 스토리지 노드 자신에서 쓰일 때:

- `local=true`: agent가 TPG를 비활성화(세션 종료)하고 LUN 0과 backstore를 지워 디바이스를 비운다. node는 backend 디바이스를 직접 마운트한다.
- `local=false`: backstore와 LUN을 다시 만들고 TPG를 다시 활성화한다.

## 9. 설계 결정: 노드 무설치 initiator

### 9.1 요구사항

사용자는 노드에 커널 모듈 외에 아무것도 설치하지 않는다. 외부 CLI나 daemon을 전제하지 않는다. 이 요구는 target과 initiator 양쪽에 적용된다.

- target: `targetcli` 대신 agent가 LIO configfs를 직접 쓴다. nvmet을 configfs로 다루는 기존 방식과 같다.
- initiator: 아래 대안을 검토한 뒤 자체 pure-Go initiator를 만들었다.

### 9.2 검토한 대안

| 대안 | 결론 | 이유 |
|------|------|------|
| 호스트의 `iscsiadm` 사용 | 기각 | 노드에 open-iscsi 설치를 요구한다. 무설치 요구와 충돌한다. |
| node 이미지에 open-iscsi + `iscsid` 번들 | 기각 | 결국 외부 CLI/daemon에 의존한다. `iscsid`의 abstract socket과 NETLINK_ISCSI가 호스트에 떠 있는 `iscsid`와 충돌한다. |
| cgo로 libiscsi 사용 | 기각 | 사용자 공간 initiator라 커널 block device를 만들지 않는다. CSI stage/publish에 쓸 `/dev/sdX`가 없다. |
| u-root `iscsinl` | 기각 | 2021년 이후 관리되지 않는다. 세션 복구와 동시성 처리가 없다. |
| 자체 pure-Go initiator | 채택 | 로그인은 Go로, 데이터 경로는 커널 `iscsi_tcp`로. `iscsid`가 쓰는 것과 같은 NETLINK_ISCSI 프로토콜을 쓴다. 세션 복구와 재시작 후 입양을 `pillar-node`가 직접 한다. |

### 9.3 결과

- 노드 요구사항은 `iscsi_tcp` 모듈 하나다.
- 데이터 경로는 커널이 처리하므로 성능은 커널 initiator와 같다.
- 대가: NETLINK_ISCSI가 init netns에만 있어서 `hostNetwork: true`가 필요하다. 중첩 컨테이너 노드에서는 `--iscsi-netlink-netns`가 필요하다 (§8.3.1).
- 호스트에 `iscsid`가 있어도 pillar 소유 세션만 관리하므로 공존할 수 있다 (§8.3.2).

## 10. CSI 기능 범위와 지원 정책

### 10.1 지원 기능

| 기능 | 지원 | 비고 |
|------|:----:|------|
| Dynamic provisioning | O | `PillarStorageClass`가 생성한 StorageClass 사용 |
| Delete / reclaim policy | O | 기존 block backend와 동일 |
| ControllerPublish / Unpublish | O | `acl: true`면 node initiator IQN 기반 ACL |
| NodeStage / Unstage | O | in-process login/logout, 디바이스 확인 |
| NodePublish / Unpublish | O | Filesystem/Block 모두 |
| `volumeMode: Filesystem` | O | `ext4`/`xfs` |
| `volumeMode: Block` | O | raw block 제공 |
| Expansion | O | backend expand + SCSI rescan + filesystem grow (온라인) |
| NodeGetVolumeStats | O | filesystem/block 둘 다 |
| Access mode `RWO`, `RWOP` | O | block protocol 정책 동일 |
| local attach | O | TPG 비활성화 후 backend 디바이스 직접 사용 |
| 재시작 복구 | O | agent: target 재구성, node: 세션 입양 |
| CSI driver registration / Probe / sidecar 연동 | O | 기존 `pillar-csi` driver 배포 모델 유지 |

### 10.2 지원하지 않는 기능

| 기능 | 지원 | 이유 |
|------|:----:|------|
| `RWX` | X | iSCSI block protocol 특성과 맞지 않음 |
| CHAP | X | 인증 정보 관리(Secret, rotation, target+initiator 양쪽)가 필요 |
| multipath / multi-portal | X | 운영 복잡도와 session 관리 난도 |
| snapshot / clone | X | 제품 전체에서 미지원 (iSCSI 고유 제약 아님) |
| topology-aware scheduling | X | iSCSI 전용 topology key 없음 |
| inline ephemeral volume | X | 현재 pillar-csi 제품 범위 밖 |

### 10.3 "CSI의 모든 기능"에 대한 제품 입장

iSCSI를 추가했다고 해서 CSI ecosystem의 모든 optional feature를 한 번에 제공하지는 않는다.

- **core CSI block lifecycle은 NVMe-oF와 같은 수준으로 제공한다.**
- **snapshot/clone은 iSCSI 전용 과제가 아니라 pillar-csi 공통 block data-management 과제다.** 이 기능이 제품에 추가되면 iSCSI는 별도 UX 변경 없이 그 기능을 상속받아야 한다.

## 11. 후속 단계

### Phase A: CHAP

- generated `StorageClass`에 `csi.storage.k8s.io/node-stage-secret-name/namespace` 매핑
- cluster-wide 기본 CHAP 또는 per-binding CHAP 선택
- Secret rotation 정책 문서화

### Phase B: multipath / multi-portal

- `PillarAgent` 또는 protocol status에서 복수 portal advertise
- 노드 측 multipath 구성
- disconnect reference counting

### Phase C: snapshot / clone 연동

- backend snapshot/clone 기능이 pillar-csi 공통 제품 기능으로 들어오면, iSCSI는 export 경로만 재사용한다.

## 12. 구현 위치

| 영역 | 위치 |
|------|------|
| CRD | `api/v1alpha1/pillarprotocol_types.go`(`ISCSIConfig`), `api/v1alpha1/pillarstorageclass_types.go`(`ISCSIOverrides`), `api/v1alpha1/annotations.go` |
| controller | `internal/csi/controller.go` (export 파라미터, VolumeContext, `CSINode` IQN 조회) |
| agent | `internal/agent/protocol_handler_iscsi.go`, `internal/agent/lio/` (LIO configfs) |
| node initiator | `internal/iscsi/` (login PDU, NETLINK_ISCSI, 세션 복구/입양, sysfs, rescan) |
| node 진입점 | `cmd/node/main.go` (`--iscsi-netlink-netns`) |
| chart | `charts/pillar-csi/values.yaml` (`node.iscsi.netlinkNetnsPath`, modprobe 모듈, `hostNetwork`) |

## 13. 검증

### 13.1 단위 테스트

- `internal/iscsi`: NETLINK_ISCSI 메시지 바이트 레이아웃, login PDU 인코딩, 세션 복구와 입양을 가짜 커널(NETLINK_ISCSI + sysfs 모사)로 검증한다.
- `internal/agent/lio`: configfs target/TPG/LUN/ACL/portal 구성을 가짜 파일시스템으로 검증한다.
- `internal/csi`: iscsi publish 시 `CSINode` annotation 조회와 annotation이 없을 때의 `FailedPrecondition`을 검증한다.

### 13.2 in-process E2E (`test/e2e`, E35)

- iscsi 프로토콜 `CreateVolume`이 iSCSI export와 iSCSI VolumeContext를 만든다 (ZFS, LVM).
- `acl: true` publish가 `CSINode`의 initiator IQN을 허용하고 unpublish가 회수한다.
- `CSINode`에 IQN annotation이 없으면 `acl: true` publish가 `FailedPrecondition`이다.
- `PillarProtocol` webhook은 `nvmeofTcp`와 `iscsi`를 동시에 지정하면 거부하고 `iscsi` 단독은 허용한다.
- `PillarStorageClass` webhook은 `nvmeofTcp` 프로토콜에 대한 `iscsi` override를 거부한다.

### 13.3 Kind 클러스터 E2E (`test/docker-e2e`)

실제 커널 데이터 경로(LIO target, `iscsi_tcp` 세션, `/dev/sd*` 디바이스)를 검증한다. Kind 노드는 `node.iscsi.netlinkNetnsPath`로 호스트 init netns를 쓴다.

- ext4/xfs 볼륨 쓰기 → pod 재시작 → 같은 노드에서 데이터 확인
- Filesystem 볼륨의 노드 간 재attach
- raw block 볼륨의 노드 간 handoff
- 온라인 filesystem 확장
- `acl: true`에서 publish되지 않은 노드 IQN의 로그인 거부
- 프로토콜 admission (iscsi override 허용/거부)

### 13.4 성공 기준

- 클러스터 관리자가 `PillarProtocol`(`spec.protocol.iscsi`)만 추가해 기존 pool/binding 모델로 iSCSI StorageClass를 만들 수 있다.
- 앱 팀이 `targetPortal`/`IQN`/`LUN`을 몰라도 PVC를 만들어 사용할 수 있다.
- iSCSI 경로에서도 `Filesystem`과 `Block` 볼륨이 모두 정상 동작한다.
- expansion, stats, local attach, 재시작 복구가 NVMe-oF와 같은 수준으로 동작한다.
- 노드에는 커널 모듈 외에 아무것도 설치하지 않는다.
- 같은 node identity 계약 아래에서 기존 NVMe-oF 경로도 계속 동작한다.

## 14. 참고 자료

- 현재 저장소
  - `api/v1alpha1/pillarprotocol_types.go`
  - `api/v1alpha1/pillarstorageclass_types.go`
  - `api/v1alpha1/annotations.go`
  - `internal/csi/controller.go`
  - `internal/agent/protocol_handler_iscsi.go`
  - `internal/iscsi/`
- RFC 7143 (iSCSI Protocol)
  - https://www.rfc-editor.org/rfc/rfc7143
- Kubernetes CSI external-provisioner docs
  - https://kubernetes-csi.github.io/docs/external-provisioner.html
- kubernetes-csi/csi-driver-iscsi
  - https://github.com/kubernetes-csi/csi-driver-iscsi
- democratic-csi
  - https://github.com/democratic-csi/democratic-csi
- HPE CSI Driver docs
  - https://scod.hpedev.io/csi_driver/using.html
- CSI spec
  - https://github.com/container-storage-interface/spec
