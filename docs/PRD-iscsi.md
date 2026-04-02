# pillar-csi iSCSI Support PRD

## 1. 문서 목적

이 문서는 `pillar-csi`에 `iSCSI` 프로토콜을 제품 기능으로 추가하기 위한 PRD다.
목적은 "코드를 어떻게 짤지"를 먼저 정하는 것이 아니라, 다음을 제품 관점에서 확정하는 데 있다.

- 사용자는 iSCSI를 어떤 리소스 조합으로 사용하게 되는가
- 어떤 파라미터를 어느 계층에 배치해야 운영이 단순하고 안전한가
- iSCSI 추가 시 `pillar-csi`가 제공해야 할 CSI 기능 범위는 어디까지인가
- 무엇을 1차 출시(MVP)로 넣고, 무엇을 후속 단계로 미루는가

이 문서는 구현 방향을 일부 포함하지만, 상세 코드 구조나 함수 시그니처를 설계 문서 수준으로 다루지 않는다.

### 1.1 선행 RFC

이번 iSCSI 추가는 단순 protocol 구현만으로 끝나지 않고,
기존 NVMe-oF 경로의 단일 protocol 가정도 함께 바로잡아야 한다.

따라서 이 PRD는 아래 RFC의 구현 완료를 전제로 한다.

- [`docs/RFC-multi-protocol-driver-foundation.md`](./RFC-multi-protocol-driver-foundation.md)

순서:

1. foundation RFC 구현
2. RFC acceptance criteria 통과
3. 이 PRD 실행

---

## 2. 현재 상태 요약

현재 저장소에는 iSCSI를 위한 "스캐폴딩"은 일부 존재하지만, 제품으로서의 iSCSI는 아직 완성되지 않았다.

- CRD 타입에는 `iscsi`가 이미 포함되어 있다.
  - `api/v1alpha1/pillarprotocol_types.go`
  - `api/v1alpha1/pillarbinding_types.go`
  - `api/v1alpha1/annotations.go`
- `PillarBinding`는 generated `StorageClass`에 `iscsi-port`, `acl-enabled`를 넣을 준비가 되어 있다.
  - `internal/controller/pillarbinding_controller.go`
- CSI controller는 protocol type `iscsi`를 인식하고 agent `ExportVolume`용 iSCSI export params를 만들 수 있다.
  - `internal/csi/controller.go`
- CSI Identity / Controller / Node 서비스라는 "driver 골격"은 이미 이 저장소의 기본 방향에 포함되어 있다.
  - 따라서 iSCSI는 별도 CSI driver를 새로 만드는 일이 아니라, 기존 `pillar-csi` driver의 protocol surface를 확장하는 일이다.
- 하지만 실제 agent export path와 node attach path는 아직 NVMe-oF 중심이다.
  - agent 쪽 테스트는 현재 iSCSI를 `Unimplemented`로 기대한다.
  - node 쪽 connector는 NVMe-oF 전용 인터페이스다.
- 또한 현재 publish/state 설명 일부는 `node_id == NQN/IQN` 같은 단일 프로토콜 가정을
  전제로 하고 있어, iSCSI를 넣기 전에 multi-protocol driver 계약부터 바로잡아야 한다.

즉, 현재 상태는 "API 표면 일부 준비 + 실제 제품 기능 미구현"이다. 따라서 iSCSI는 단순 구현 작업이 아니라, 제품 범위와 운영 모델을 먼저 확정해야 하는 기능이다.

---

## 3. 왜 iSCSI가 필요한가

### 3.1 배경

NVMe-oF는 성능과 현대성 측면에서 유리하지만, 실제 self-hosted 환경에서는 다음 이유로 iSCSI 수요가 여전히 크다.

- 더 넓은 OS/배포판 호환성
- 더 익숙한 운영 경험 (`iscsiadm`, `open-iscsi`, LIO)
- 기존 NAS/SAN/가상화 환경과의 연결 용이성
- NVMe-oF 미지원 커널/장비/네트워크 환경에서의 대안

### 3.2 제품 목표

`pillar-csi`의 iSCSI는 "기존 iSCSI target을 Kubernetes에서 마운트하는 얇은 static driver"가 아니라,
`pillar-csi`의 선언형 CRD 모델 안에서 동적 프로비저닝과 attach/mount lifecycle을 제공하는 block protocol 옵션이어야 한다.

### 3.3 이 기능의 정확한 포지셔닝

최종 목표는 "iSCSI를 쓰는 또 다른 CSI driver"를 추가로 내는 것이 아니라,
**`pillar-csi`가 iSCSI 경로까지 포함하는 하나의 CSI driver가 되는 것**이다.

즉, 이 PRD의 산출물은 아래와 같아야 한다.

- 드라이버 이름은 계속 `pillar-csi.bhyoo.com` 하나다.
- 배포 토폴로지도 기존과 같이 `pillar-controller` + `pillar-node` + `pillar-agent`를 유지한다.
- 외부 프로비저너/어태처/리사이저와 node-driver-registrar/livenessprobe를 포함한
  Kubernetes CSI 표준 통합은 그대로 유지된다.
- iSCSI는 그 위에서 선택 가능한 `PillarProtocol(type=iscsi)`가 된다.

---

## 4. 제품 목표와 비목표

### 4.1 목표

- `zfs-zvol`, `lvm-lv` 같은 block backend를 `iscsi`로 export할 수 있어야 한다.
- `PillarProtocol` / `PillarBinding` / PVC override라는 기존 계층 모델을 그대로 유지해야 한다.
- 사용자는 `targetPortal`, `iqn`, `lun` 같은 저수준 iSCSI 세부값을 직접 만들지 않아도 되어야 한다.
- `Filesystem`과 `Block` volumeMode를 모두 지원해야 한다.
- core CSI block lifecycle은 NVMe-oF와 동등한 수준으로 제공해야 한다.
  - Create/Delete
  - Identity (`GetPluginInfo`, `GetPluginCapabilities`, `Probe`)
  - `ValidateVolumeCapabilities`
  - `ControllerGetCapabilities`
  - ControllerPublish/Unpublish
  - `NodeGetCapabilities`
  - `NodeGetInfo`
  - NodeStage/Unstage
  - NodePublish/Unpublish
  - ControllerExpand + NodeExpand
  - NodeGetVolumeStats
- 보안/운영 리스크가 큰 항목(CHAP, multipath)은 MVP와 후속 단계를 분리해 다뤄야 한다.

### 4.2 비목표

- iSCSI로 RWX 제공
- iSCSI 자체만으로 CSI snapshot/clone을 새로 정의
- 앱 팀이 `targetPortal`/`IQN`/`LUN`을 직접 입력하는 static PV 중심 UX
- `PillarTarget.spec.external` 또는 기존 외부 SAN/NAS의 pre-existing iSCSI target을
  바로 소비하는 static-import UX
- Windows initiator 지원
- 첫 출시에서 multipath와 CHAP을 동시에 넣는 것

---

## 5. 외부 드라이버 조사와 시사점

### 5.1 kubernetes-csi/csi-driver-iscsi

관찰:

- README 기준으로 이 드라이버는 `existing and already configured iscsi server`를 전제로 한다.
- examples 기준으로 PV에서 `targetPortal`, `iqn`, `lun`, `fsType`을 직접 다룬다.
- 즉, "이미 존재하는 iSCSI target을 Kubernetes에 연결"하는 static/low-level 모델에 가깝다.
- pkg 문서 예시에서는 `csc node get-id` 결과가 generic한 `CSINode`이며,
  node ID 자체를 IQN으로 고정하지 않는다.

시사점:

- `pillar-csi`는 이 모델을 그대로 따라가면 안 된다.
- app/operator가 volume마다 `portal/IQN/LUN`을 다루게 되면 `pillar-csi`의 CRD 기반 동적 프로비저닝 가치가 사라진다.
- 따라서 `pillar-csi`는 controller가 target IQN과 LUN을 생성하고, PV `VolumeContext`에만 runtime 정보로 기록해야 한다.
- node identity 역시 raw IQN을 그대로 노출하는 static 드라이버 모델이 아니라,
  `pillar-csi` 내부의 stable node handle과 protocol-specific identity를 분리하는 편이 맞다.

### 5.2 democratic-csi

관찰:

- `freenas-iscsi`, `freenas-api-iscsi`, `zfs-generic-iscsi`, `synology-iscsi` 등 driver 종류가 backend/protocol 조합별로 나뉜다.
- 문서상 resize, snapshots, clones 등 CSI 기능 폭이 넓다.
- node prep 문서에서 `open-iscsi` 설치와 multipath 설정을 별도 운영 작업으로 강하게 요구한다.
- 반면 driver 타입과 Helm values가 많아 운영 표면이 커진다.

시사점:

- `pillar-csi`는 `driver per protocol/back-end combination` 대신,
  `Backend`와 `Protocol`을 분리한 현재 CRD 모델을 유지하는 것이 맞다.
- iSCSI 추가는 새 CSI driver 제품군을 늘리는 작업이 아니라,
  `PillarProtocol(type=iscsi)`를 추가하는 작업이어야 한다.

### 5.3 HPE CSI Driver

관찰:

- `StorageClass`에서 `accessProtocol: iscsi`를 선택한다.
- CHAP은 Secret 기반으로 다루며, cluster-wide 또는 per-StorageClass 둘 다 지원한다.
- `allowOverrides`와 PVC override 정책이 문서화되어 있다.
- raw block, expansion, snapshot 등 Kubernetes 표준 사용 방식을 따른다.
- 문서상 expansion은 controller+node가 결합된 online expansion이며,
  볼륨이 node에 attach된 상태여야 완료된다.

시사점:

- `pillar-csi`도 CHAP은 plain-text CRD 필드가 아니라 Secret 참조 기반으로 가야 한다.
- per-volume override는 allowlist 방식으로 제한해야 한다.
- `csi.storage.k8s.io/node-stage-secret-*` 같은 표준 CSI secret mechanism을 우선 사용해야 한다.
- expansion도 "attach된 block volume의 online grow"로 제품 계약을 명확히 두는 편이 맞다.

### 5.4 조사 결론

`pillar-csi`의 iSCSI는 아래 원칙을 따라야 한다.

- 저수준 iSCSI 연결 정보는 사용자 입력이 아니라 controller가 생성/관리한다.
- 프로토콜 선택은 `PillarProtocol`, class-level 세부정책은 `PillarBinding`, volume-level 튜닝은 PVC override로 제한한다.
- CHAP/secret은 Kubernetes 표준 CSI secret path를 사용한다.
- snapshots/clones는 "iSCSI 전용 기능"이 아니라 "block backend용 pillar-csi 공통 기능"으로 다룬다.
- 추론:
  외부 드라이버들은 대체로 "단일 protocol 전용 driver" 또는 "backend/protocol 조합별 driver"이므로
  `node_id`와 initiator identity 충돌 문제가 표면화되지 않는다.
  `pillar-csi`처럼 하나의 driver 안에서 NVMe-oF와 iSCSI를 함께 제공하려면,
  `stable node handle → protocol-specific identity` 해석 계층을 제품 계약으로 명시해야 한다.

---

## 6. 사용자 경험(UX)과 사용 흐름

### 6.1 관리자 흐름

클러스터 관리자는 기존과 동일하게 4개 리소스 조합으로 iSCSI를 사용한다.

1. `PillarTarget`
   - MVP에서는 `nodeRef` 기반 storage node를 정의
   - `spec.external` 기반 외부 agent는 제품 전체 로드맵의 후속 단계로 둔다
2. `PillarPool`
   - `zfs-zvol` 또는 `lvm-lv` pool을 정의
3. `PillarProtocol`
   - `type: iscsi`와 transport/security/timer 기본값을 정의
4. `PillarBinding`
   - pool + protocol을 결합하고 StorageClass를 생성

앱 팀은 생성된 StorageClass만 사용해 PVC를 만든다.

### 6.2 앱 팀 흐름

앱 팀이 직접 알아야 하는 것은 다음뿐이다.

- 어떤 StorageClass를 쓸지
- `Filesystem`로 쓸지 `Block`으로 쓸지
- 정말 필요한 경우에만 PVC annotation으로 제한된 튜닝 값을 덮어쓸지

앱 팀이 직접 알 필요가 없는 값:

- target IQN
- portal IP/port
- LUN
- ACL 생성/삭제 순서
- node initiator IQN 조회 방법

### 6.3 사용 예시

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: iscsi-default
spec:
  type: iscsi
  iscsi:
    port: 3260
    acl: true
    loginTimeout: 15
    replacementTimeout: 120
    initialLoginRetryMax: 8
    noopOutInterval: 5
    noopOutTimeout: 5
  fsType: xfs
  mkfsOptions: ["-K"]
```

```yaml
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarBinding
metadata:
  name: fast-iscsi
spec:
  poolRef: hot-lvm
  protocolRef: iscsi-default
  storageClass:
    name: fast-iscsi
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true
  overrides:
    protocol:
      iscsi:
        replacementTimeout: 180
    fsType: xfs
```

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: app-data
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: fast-iscsi
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
  storageClassName: fast-iscsi
  resources:
    requests:
      storage: 40Gi
```

---

## 7. 파라미터 계층 설계

핵심 원칙은 "구조적 파라미터는 상위 계층에서만, 튜닝 파라미터만 하위 계층에서"다.

### 7.1 계층별 책임

| 계층 | 위치 | 책임 | iSCSI에서 다루는 값 |
|------|------|------|---------------------|
| 설치/배포 | Helm values / node DaemonSet | 노드 패키징과 런타임 준비 | `open-iscsi` 번들, `iscsid` 실행 방식, initiator IQN 소스, multipath enable 여부 |
| 프로토콜 기본값 | `PillarProtocol.spec.iscsi` | 클러스터 공통 transport/security/timer 정책 | `port`, `acl`, `loginTimeout`, `replacementTimeout`, `initialLoginRetryMax`, `noopOutInterval`, `noopOutTimeout` |
| 클래스별 조정 | `PillarBinding.spec.overrides.protocol.iscsi` | 특정 StorageClass에만 적용할 차등 정책 | 상위 타이머 필드 override, 후속 단계의 CHAP secret ref |
| 표준 CSI 클래스 파라미터 | generated `StorageClass.parameters` | sidecar/kubelet이 해석하는 표준 키 | `csi.storage.k8s.io/fstype`, 향후 `csi.storage.k8s.io/node-stage-secret-*` |
| 볼륨 단위 튜닝 | PVC annotation | 안전한 미세 조정 | timeout override, `fsType`, `mkfsOptions` |
| 런타임 연결 정보 | PV `volumeAttributes` / CSI `VolumeContext` | attach/mount에 필요한 실제 export 정보 | `protocol-type=iscsi`, `target_id=<IQN>`, `address`, `port`, `volume_ref=<LUN>` |

### 7.2 `PillarProtocol`에 둘 값

`PillarProtocol`에는 "모든 binding이 공유해도 이상하지 않은 프로토콜 정책"만 둔다.

MVP 필드:

- `port`
- `acl`
- `loginTimeout`
- `replacementTimeout`
- `initialLoginRetryMax`
- `noopOutInterval`
- `noopOutTimeout`
- 공통 block protocol 필드: `fsType`, `mkfsOptions`

MVP에서 제외할 값:

- `targetPortal` / `portals`
- target IQN prefix를 제외한 volume별 target 식별자
- CHAP credential plain text
- multipath 세션 수/iface별 세부값

### 7.3 `PillarBinding`에 둘 값

`PillarBinding`은 generated `StorageClass`의 제품 표면이다.

MVP에서는 다음을 둘 수 있다.

- protocol timer override
- `fsType`
- `mkfsOptions`
- `allowVolumeExpansion`

후속 단계에서는 여기에 CHAP secret reference를 추가한다.
이때 secret 값 자체는 binding spec에 넣지 않고, generated `StorageClass`의 표준 CSI secret keys로 매핑한다.

### 7.4 PVC annotation에 둘 값

PVC annotation은 "마지막 레이어의 튜닝"만 허용한다.

허용:

- `loginTimeout`
- `replacementTimeout`
- `initialLoginRetryMax`
- `noopOutInterval`
- `noopOutTimeout`
- `fsType`
- `mkfsOptions`

금지:

- `port`
- `acl`
- target portal / portal list
- target IQN
- CHAP secret ref
- multipath on/off

즉, PVC annotation으로 보안/토폴로지/라우팅을 바꾸면 안 된다.

### 7.5 제품 파라미터의 구체 매핑

MVP에서 제품 문서와 테스트는 아래 이름을 기준으로 맞춘다.

| 제품 필드 | 위치 | generated `StorageClass` key | node/agent 적용 지점 |
|----------|------|-------------------------------|----------------------|
| `port` | `PillarProtocol.spec.iscsi.port` | `pillar-csi.bhyoo.com/iscsi-port` | agent export bind port |
| `acl` | `PillarProtocol.spec.iscsi.acl` | `pillar-csi.bhyoo.com/acl-enabled` | ControllerPublish/Unpublish에서 ACL on/off |
| `loginTimeout` | `PillarProtocol` / `PillarBinding` / PVC override | `pillar-csi.bhyoo.com/iscsi-login-timeout` | `iscsiadm` login timeout |
| `replacementTimeout` | `PillarProtocol` / `PillarBinding` / PVC override | `pillar-csi.bhyoo.com/iscsi-replacement-timeout` | session recovery timeout |
| `initialLoginRetryMax` | `PillarProtocol` / `PillarBinding` / PVC override | `pillar-csi.bhyoo.com/iscsi-initial-login-retry-max` | 초기 login 재시도 상한 |
| `noopOutInterval` | `PillarProtocol` / `PillarBinding` / PVC override | `pillar-csi.bhyoo.com/iscsi-noop-out-interval` | connection keepalive ping 간격 |
| `noopOutTimeout` | `PillarProtocol` / `PillarBinding` / PVC override | `pillar-csi.bhyoo.com/iscsi-noop-out-timeout` | ping 응답 대기 시간 |
| `fsType` | `PillarProtocol` / `PillarBinding` / PVC override | `csi.storage.k8s.io/fstype` | `mkfs` / mount |
| `mkfsOptions` | `PillarProtocol` / `PillarBinding` / PVC override | `pillar-csi.bhyoo.com/mkfs-options` | `mkfs` 인자 |

주의:

- 현재 스캐폴딩에 있는 `nodeSessionTimeout`은 제품 필드로 채택하지 않는다.
- 표준 open-iscsi 의미와 직접 매핑되는 이름만 남긴다.
- CHAP이 추가되면 Secret 자체는 CRD에 넣지 않고, generated `StorageClass`의
  `csi.storage.k8s.io/node-stage-secret-name/namespace`로 연결한다.
- 현재 저장소 스키마/annotation helper에는 `nodeSessionTimeout`이 남아 있고
  `initialLoginRetryMax`, `noopOutInterval`, `noopOutTimeout`는 아직 없다.
- 이 PRD는 제품 표면을 `loginTimeout`, `replacementTimeout`,
  `initialLoginRetryMax`, `noopOutInterval`, `noopOutTimeout`로 정규화한다.
- 따라서 구현 착수 전 schema, generated `StorageClass` key, PVC override allowlist를
  이 제품 표면에 맞춰 먼저 정렬해야 한다.

---

## 8. MVP 제품 설계

### 8.1 export 모델

MVP는 다음 모델로 고정한다.

- volume마다 iSCSI target 하나 생성
- 각 target은 LUN 0 하나만 export
- portal은 `PillarTarget.status.resolvedAddress` 하나만 사용
- target IQN은 controller/agent가 자동 생성
- 이 export 모델은 `zfs-zvol`과 `lvm-lv` 두 supported block backend에 동일하게 적용한다.

이 모델의 장점:

- 식별자 충돌이 적다
- ACL과 disconnect semantics가 단순하다
- multipath를 미지원으로 둘 때 가장 예측 가능하다

### 8.2 node initiator 식별

iSCSI에서 `ControllerPublishVolume`은 node의 initiator IQN을 알아야 한다.

아래 내용은 iSCSI PRD가 의존하는 foundation 요약이다.
구조 변경의 canonical 설계와 구현 순서는 RFC를 따른다.

제품 결정:

- `NodeGetInfo.node_id`는 모든 프로토콜에서 공통으로 쓰는 stable node handle이어야 한다.
- 이 교정은 새 iSCSI 경로뿐 아니라 기존 NVMe-oF 경로에도 함께 적용된다.
- Kubernetes 내부 노드에서 MVP 기본값은 Kubernetes node name이다.
- raw transport identity는 `node_id`에 실리지 않는다.
  - NVMe-oF는 host NQN
  - iSCSI는 initiator IQN
  - 향후 NFS/SMB는 client IP/hostname 또는 별도 client identity
- node-side 구성요소는 `/etc/iscsi/initiatorname.iscsi` 같은 호스트 정보를 읽어
  iSCSI initiator IQN을 자동 감지해야 한다.
- 같은 방식으로 NVMe-oF host NQN도 자동 감지하거나 명시 override할 수 있어야 한다.
- 이 감지 결과는 Kubernetes `Node` 본문이 아니라
  `CSINode.metadata.annotations`에 반영된다.
  - 예: `pillar-csi.bhyoo.com/iscsi-initiator-iqn`
  - 예: `pillar-csi.bhyoo.com/nvmeof-host-nqn`
- 이 annotation publisher는 `pillar-node` 내부 기능일 수도 있고, node 배포에 동봉된
  helper일 수도 있다. 중요한 것은 **사용자에게 수동 annotation 편집을 요구하지 않는 것**이다.
- controller는 `ControllerPublish/Unpublish` 시 `node_id`로 `CSINode`를 찾고,
  `protocol-type`에 맞는 identity를 조회해 agent ACL 호출에 사용한다.
- 따라서 `pillar-csi`의 제품 계약은
  **"CSI node identity"와 "protocol initiator identity"를 분리한다**로 정리된다.

이 결정은 ACL 모델과 직결되므로 MVP 범위에 포함한다.

### 8.3 node attach/stage 모델

MVP node path는 `open-iscsi`를 사용한다.

다만 제품 구조는 iSCSI 추가를 계기로 "NVMe-oF 전용 node path"에서
"multi-protocol node path"로 바뀌어야 한다.

이 구조 변경의 구현 ownership은 RFC에 있고, 여기서는 iSCSI가 요구하는 제품 조건만 요약한다.

- discovery: sendtargets 기반
- login: target IQN + portal + port
- rescan: 확장 또는 재연결 시 명시 수행
- logout: 마지막 stage 해제 시 수행

제품 아키텍처 원칙:

- transport attach/detach는 protocol handler 책임이다.
  - NVMe-oF: connect / disconnect / rescan / device resolve
  - iSCSI: discovery / login / logout / rescan / device resolve
- 그 위의 block device lifecycle은 공통화한다.
  - `volumeMode: Filesystem`의 mkfs/mount
  - `volumeMode: Block`의 raw device publish
  - `NodeGetVolumeStats`
  - `NodeExpandVolume`의 filesystem grow
- 향후 NFS/SMB 같은 file protocol은 같은 controller surface를 쓰되,
  node 쪽에서는 "transport login + block device" 대신
  "network export mount" 계층으로 구현한다.
- 따라서 node의 persisted stage state도 `SubsysNQN` 같은 NVMe 전용 형식이 아니라,
  `protocol-type`과 protocol별 transport 상태를 담는 일반화된 모델이어야 한다.

제품 관점에서 중요한 점:

- `pillar-csi`는 iSCSI 사용자에게 `iscsiadm` CLI를 직접 요구하지 않아야 한다.
- node image가 이를 번들하고, kubelet/CSI 흐름 안에서 자동 수행해야 한다.

### 8.4 filesystem / raw block

MVP에서 둘 다 지원한다.

- `volumeMode: Filesystem`
  - attach 후 `mkfs` + mount
  - `fsType` 기본값 제공
- `volumeMode: Block`
  - login 후 raw block device를 pod에 노출
  - KubeVirt/DB/WAL 용도에 중요

이 결정은 HPE CSI 등 성숙한 iSCSI 드라이버의 일반적인 사용 방식과도 맞다.

### 8.5 확장

iSCSI MVP는 expansion을 지원한다.

- `ControllerExpandVolume`
  - backend(zvol/LV) 크기 증가
- `NodeExpandVolume`
  - iSCSI session/LUN rescan
  - filesystem grow
- MVP의 expansion 계약은 attached volume에 대한 online expansion이다.
  attach되지 않은 상태의 별도 offline controller-only expansion은 MVP 목표에 넣지 않는다.

즉, iSCSI는 NVMe-oF와 마찬가지로 "block protocol용 online expansion"의 일부로 제공한다.

### 8.6 보안

MVP 보안 모델:

- 제품 기본 정책은 `acl: true`
- node initiator IQN 기반 ACL 사용
- CHAP은 MVP에서 미지원
- 스토리지 네트워크 분리를 전제

명확히 할 점:

- 현재 저장소의 CRD 스키마 기본값은 개발/테스트 편의상 `acl=false` 방향으로 남아 있다.
- iSCSI MVP를 제품 기본 정책 `acl: true`로 출시하려면, 구현 작업에 CRD default와
  generated 예제/문서를 함께 맞추는 변경이 포함되어야 한다.
- 그 전까지는 공식 예제 YAML에서 `acl: true`를 반드시 명시해야 한다.

후속 단계:

- CHAP support
- 필요시 mutual CHAP

### 8.7 multipath

multipath는 MVP에서 미지원으로 둔다.

이유:

- 단일 session disconnect가 다른 volume session에 영향을 주지 않도록 정교한 node-side 세션 참조 관리가 필요하다.
- `dm-multipath`와 iSCSI timeout 조합은 운영 난도가 높다.
- single portal/single path만으로도 많은 homelab/on-prem 환경을 커버할 수 있다.

MVP에서는 관련 파라미터를 제품 표면에 올리지 않는다.

### 8.8 CSI driver packaging과 배포 요구사항

iSCSI 지원은 protocol 구현만으로 완료되지 않는다. `pillar-csi`가 실제 CSI driver로
동작하려면 배포와 sidecar 통합까지 기존 제품 원칙과 맞아야 한다.

MVP 요구사항:

- `pillar-controller`는 기존과 같은 단일 driver 이름(`pillar-csi.bhyoo.com`)으로
  CSI Controller + Identity 서비스를 제공한다.
- `pillar-node`는 기존과 같은 driver 이름으로 CSI Node + Identity 서비스를 제공한다.
- controller 측 sidecar는 최소 `external-provisioner`, `external-attacher`,
  `external-resizer`, `livenessprobe`를 유지한다.
- node 측 sidecar는 최소 `node-driver-registrar`, `livenessprobe`를 유지한다.
- `pillar-agent`는 기존 제품 방향과 동일하게 `hostPort` 기반으로 node IP에서
  접근 가능해야 하며, iSCSI 추가 때문에 별도 agent 배포 토폴로지를 만들지 않는다.
- `pillar-node` 이미지는 `open-iscsi` 사용자 공간 도구와 필요한 런타임(`iscsid`)을
  번들해, 사용자가 Kubernetes 노드에 수동 설치하지 않아도 되게 해야 한다.
- node 배포에는 protocol-specific initiator identity를
  `CSINode.metadata.annotations`로 publish하는 구성요소가 포함되어야 하며,
  이 경로는 재시작 후에도 self-healing 해야 한다.

---

## 9. CSI 기능 범위와 지원 정책

### 9.1 MVP에서 지원할 기능

| 기능 | MVP | 메모 |
|------|:---:|------|
| Dynamic provisioning | O | `PillarBinding`가 생성한 StorageClass 사용 |
| Delete / reclaim policy | O | 기존 block backend와 동일 |
| ControllerPublish / Unpublish | O | node initiator IQN 기반 ACL |
| NodeStage / Unstage | O | discovery/login/logout |
| NodePublish / Unpublish | O | Filesystem/Block 모두 |
| `volumeMode: Filesystem` | O | ext4/xfs |
| `volumeMode: Block` | O | raw block 제공 |
| Expansion | O | backend expand + rescan + filesystem grow |
| NodeGetVolumeStats | O | filesystem/block 둘 다 |
| Access mode `RWO`, `RWOP`, `ROX` | O | block protocol 정책 동일 |
| CSI driver registration / Probe / sidecar 연동 | O | 기존 `pillar-csi` driver 배포 모델 유지 |

### 9.2 MVP에서 지원하지 않을 기능

| 기능 | 지원 여부 | 이유 |
|------|-----------|------|
| `RWX` | X | iSCSI block protocol 특성과 맞지 않음 |
| CHAP | X | Secret/rotation/target+initiator 양측 적용 설계가 필요 |
| multipath | X | 운영 복잡도와 session 관리 난도 높음 |
| topology-aware scheduling | X | iSCSI 추가 자체와 직접적 관련이 적음 |
| inline ephemeral volume | X | 현 시점 pillar-csi 제품 우선순위 아님 |

### 9.3 "CSI의 모든 기능"에 대한 제품 입장

iSCSI를 추가한다고 해서 첫 출시에서 CSI ecosystem의 모든 optional feature를 한 번에 제공하는 것은 목표가 아니다.

정확한 제품 입장은 아래와 같다.

- **core CSI block lifecycle은 MVP에서 제공한다.**
- **snapshot/clone은 iSCSI 전용 과제가 아니라 pillar-csi 공통 block data-management 과제다.**
- 따라서 snapshot/clone 기능이 pillar-csi 전체 제품에 추가되면, iSCSI도 별도 UX 변경 없이 그 기능을 상속받아야 한다.

즉, iSCSI는 "CSI의 핵심 block 기능은 MVP에서", "data management 기능은 product-wide 후속 phase에서" 제공한다.

---

## 10. 후속 단계

### Phase A: iSCSI MVP

- single portal
- ACL 기반 publish/unpublish
- Filesystem + Block
- expansion + stats
- ZFS zvol / LVM LV 지원

### Phase B: CHAP

- generated `StorageClass`에 `csi.storage.k8s.io/node-stage-secret-name/namespace` 매핑
- cluster-wide 기본 CHAP 또는 per-binding CHAP 선택
- Secret rotation 정책 문서화

### Phase C: multipath / multi-portal

- `PillarTarget` 또는 protocol status에서 복수 portal advertise
- `dm-multipath` 운영 가이드
- disconnect reference counting

### Phase D: snapshot / clone 연동

- backend snapshot/clone 기능이 pillar-csi 공통 제품 기능으로 들어오면, iSCSI는 export 경로만 재사용

---

## 11. 구현 방향과 실행 계획(최소한만)

이 PRD는 코드 설계 문서는 아니지만, 제품 결정을 구현으로 연결하기 위한 최소 방향은 필요하다.

### 11.1 선행 조건 — foundation RFC 구현 완료

- `NodeGetInfo.node_id`를 stable node handle로 재정의하는 작업은 이 PRD의 직접 구현 범위가 아니다.
- controller의 protocol identity resolver, `CSINode` annotation publication,
  node runtime 일반화는
  선행 RFC 구현 항목이다.
- 이 PRD는 그 foundation 위에서 iSCSI 제품 기능을 완성하는 문서다.

### 11.2 iSCSI control plane 제품 표면

- generated `StorageClass`와 PVC override 정책을 최종 제품 파라미터 표면에 맞춰 정렬한다.
- `CreateVolume`은 target IQN, portal, port, LUN을 런타임 `VolumeContext`로 제공한다.
- `ControllerPublish/Unpublish`는 `CSINode` annotation에서 해석한 initiator IQN을 이용해
  LIO ACL을 관리한다.
- `acl: true`를 제품 기본 정책으로 출시하려면 CRD default, generated 예제, 운영 문서를 함께 맞춘다.

### 11.3 iSCSI node data path

- `open-iscsi`/`iscsid` 번들, discovery/login/logout/rescan 경로를 완성한다.
- Filesystem / Block 두 `volumeMode`를 같은 제품 release 안에서 제공한다.
- expansion, stats, restart recovery를 block protocol 공통 품질 기준으로 맞춘다.

### 11.4 배포와 운영성

- node/controller sidecar 구성은 기존 `pillar-csi` 배포 모델을 유지한다.
- node-side annotation publisher를 포함해 "노드에 수동 설치/수동 annotation 편집이
  필요 없는" 운영 모델을 제공한다.
- 호스트 요구사항과 self-hosted runner 조건(LIO, open-iscsi, ZFS/LVM)을 명확히 문서화한다.

### 11.5 검증과 출시 게이트

- generic in-process CSI 테스트는 `node_id == initiator ID` 가정을 버리고,
  stable node handle + `CSINode` annotation resolution 계약으로 재작성한다.
- iSCSI cluster E2E는 지원 backend 조합인 `lvm-lv + iscsi`, `zfs-zvol + iscsi`
  둘 다에서 control plane, mount, raw block, expansion, stats, recovery를 검증한다.
- 동시에 기존 NVMe-oF 테스트도 같은 node identity 계약 아래에서 계속 통과해야 한다.
- 즉, iSCSI MVP의 출시 게이트에는 "iSCSI 추가"뿐 아니라
  "NVMe-oF regression 없이 multi-protocol driver contract 정착"이 포함된다.

---

## 12. 성공 기준

다음이 충족되면 제품적으로 iSCSI MVP가 성공했다고 본다.

- cluster admin이 `PillarProtocol(type=iscsi)`만 추가해 기존 pool/binding 모델로 iSCSI StorageClass를 만들 수 있다.
- app team이 `targetPortal/IQN/LUN`을 몰라도 PVC를 만들고 사용할 수 있다.
- iSCSI 경로에서도 `Filesystem`과 `Block` 볼륨이 모두 정상 동작한다.
- expansion과 stats가 NVMe-oF와 같은 수준으로 동작한다.
- 지원 backend 조합인 `zfs-zvol + iscsi`, `lvm-lv + iscsi` 각각에 대해 control plane, mount, raw block, expansion, stats, cleanup, restart recovery가 E2E로 검증된다.
- `pillar-controller`와 `pillar-node`가 기존과 동일한 CSI driver 이름으로 등록되고,
  Identity/Probe/sidecar 연동까지 포함한 Kubernetes CSI 경로가 end-to-end로 동작한다.
- controller가 `CSINode` annotation에서 iSCSI initiator IQN을 안정적으로 해석해 ACL publish/unpublish에 사용한다.
- 같은 node identity 계약 아래에서 기존 NVMe-oF 경로도 계속 동작한다.
- 이 구조가 future protocol(NFS/SMB) 추가 시에도 `node_id` 의미를 다시 바꾸지 않게 만든다.
- 운영 문서가 "CHAP/multipath는 후속 단계"임을 명확히 말하고, MVP 범위 안에서는 예측 가능한 동작을 제공한다.

---

## 13. 참고 자료

- 현재 저장소
  - `api/v1alpha1/pillarprotocol_types.go`
  - `api/v1alpha1/pillarbinding_types.go`
  - `api/v1alpha1/annotations.go`
  - `internal/controller/pillarbinding_controller.go`
  - `internal/csi/controller.go`
  - `internal/csi/node.go`
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
- open-iscsi project
  - https://github.com/open-iscsi/open-iscsi
