# CRD 이름 계약 변경에 따른 업그레이드 (issue #58)

## 무엇이 바뀌었나

Helm 차트의 CRD 템플릿(`charts/pillar-csi/templates/crds.yaml`)은 이제 `make manifests`가
controller-gen 산출물(`config/crd/bases/`)에서 생성한다. 이전 차트는 손으로 관리되어 kubebuilder
마커(`api/v1alpha1/*_types.go`)와 다른 이름을 배포했다.

| Kind | 이전 차트 | 현재 (생성 계약) |
|------|-----------|------------------|
| PillarVolumeState | CRD `pillarvolumestatestates.pillar-csi.bhyoo.com`, plural `pillarvolumestatestates`, shortName `pv` | CRD `pillarvolumestates.pillar-csi.bhyoo.com`, plural `pillarvolumestates`, shortName `pvst` |
| PillarAgent | shortName `pt` | `pa` |
| PillarStore | shortName `pp` | `pst` |
| PillarProtocol | shortName `ppr` | `pstr` |
| PillarStorageClass | shortName `pb` | `psc` |

- API group, version(`v1alpha1`), kind, 스키마는 바뀌지 않았다. 따라서 conversion webhook이나 저장 버전
  마이그레이션은 필요 없다.
- PillarVolumeState의 REST 경로(`/apis/pillar-csi.bhyoo.com/v1alpha1/pillarvolumestates`)와 CRD 이름이
  바뀌었다. 이전 이름을 쓰는 스크립트나 별도 RBAC는 수정해야 한다.
- 이제 모든 차트 CRD에 `helm.sh/resource-policy: keep`이 붙는다. 이후 업그레이드나 uninstall에서
  Helm이 CRD(와 그에 속한 커스텀 리소스)를 삭제하지 않는다.

## 영향을 받는 설치

- **`installCRDs=true`(기본값)로 이전 차트를 설치한 클러스터**: 이전 CRD
  `pillarvolumestatestates.pillar-csi.bhyoo.com`에는 keep 어노테이션이 없다. 따라서 `helm upgrade`는 새
  매니페스트에 없는 이 CRD를 삭제하고, API 서버는 그 CRD의 모든 PillarVolumeState 오브젝트를 함께
  삭제한다. 두 CRD는 같은 kind를 쓰므로 동시에 존재할 수 없다(새 CRD는 이전 CRD가 사라질 때까지
  `NamesAccepted=False`, `ListKindConflict`). 자동 마이그레이션은 제공하지 않는다. PillarVolumeState를
  보존하려면 아래 절차를 따른다. PillarVolumeState가 없거나 폐기해도 되는 클러스터(예: QA용)는
  `helm uninstall` 후 새로 설치하면 된다.
- **`installCRDs=false`로 CRD를 따로 관리하는 클러스터**: `config/crd/bases/`의 CRD를 적용해 왔다면,
  이전 차트 RBAC가 `pillarvolumestatestates`에만 권한을 주어 컨트롤러의 PillarVolumeState 접근이
  Forbidden으로 실패했다. 이번 차트에서 해소된다. 이전 차트에서 추출한 CRD를 적용해 왔다면 아래 절차의
  2단계와 4단계를 직접 수행하고, 3단계에서는 Helm 대신 새 CRD를 직접 적용한 뒤 이전 CRD를 삭제한다.

## 데이터를 보존하는 수동 마이그레이션

`RELEASE`와 `HELM_NAMESPACE`(릴리스가 설치된 네임스페이스)는 설치 값에 맞게 바꾼다. 워크로드
네임스페이스 `WORKLOAD_NAMESPACE`는 `namespaceOverride`가 있으면 그 값, 없으면 `HELM_NAMESPACE`이다
(`pillar-csi.namespace` 헬퍼). 작업 중에는 CreateVolume, DeleteVolume, ControllerPublish 호출이 멈춘다.

0. 컨트롤러 Deployment와 replica 수를 실제 설치에서 확인한다. Deployment 이름은 릴리스 이름과
   다를 수 있다(`fullnameOverride`, `nameOverride`). 차트는 컨트롤러에
   `app.kubernetes.io/component=controller` 레이블을 붙인다. 정확히 하나만 나와야 한다.
   0개 또는 2개 이상이면 멈추고 설치 값을 확인한다.

   ```bash
   WORKLOAD_NAMESPACE=$(helm get values "$RELEASE" -n "$HELM_NAMESPACE" -o json \
     | jq -r '.namespaceOverride // empty')
   WORKLOAD_NAMESPACE=${WORKLOAD_NAMESPACE:-$HELM_NAMESPACE}
   DEPLOY=$(kubectl -n "$WORKLOAD_NAMESPACE" get deploy \
     -l app.kubernetes.io/instance="$RELEASE",app.kubernetes.io/component=controller \
     -o jsonpath='{.items[*].metadata.name}')
   [ "$(wc -w <<<"$DEPLOY")" -eq 1 ] || { echo "controller Deployment가 정확히 하나가 아님: $DEPLOY"; exit 1; }
   REPLICAS=$(kubectl -n "$WORKLOAD_NAMESPACE" get deploy "$DEPLOY" -o jsonpath='{.spec.replicas}')
   echo "controller=$DEPLOY namespace=$WORKLOAD_NAMESPACE replicas=$REPLICAS"
   ```

1. 컨트롤러를 멈춰서 PillarVolumeState 쓰기를 막는다.

   ```bash
   kubectl -n "$WORKLOAD_NAMESPACE" scale deployment/"$DEPLOY" --replicas=0
   kubectl -n "$WORKLOAD_NAMESPACE" rollout status deployment/"$DEPLOY"
   ```

2. 이전 리소스를 spec과 status 그대로 백업한다.

   ```bash
   kubectl get pillarvolumestatestates.pillar-csi.bhyoo.com -o json > pvs-backup.json
   jq '.items | length' pvs-backup.json
   ```

3. 컨트롤러 replica를 0으로 둔 채 업그레이드하고, 새 CRD가 Established 되기를 기다린다.
   이 단계에서 이전 CRD와 그 오브젝트가 삭제된다.

   ```bash
   helm upgrade "$RELEASE" <chart> -n "$HELM_NAMESPACE" --reuse-values --set controller.replicaCount=0
   kubectl wait --for=condition=Established --timeout=120s crd/pillarvolumestates.pillar-csi.bhyoo.com
   kubectl get crd pillarvolumestatestates.pillar-csi.bhyoo.com   # NotFound여야 한다
   ```

4. 오브젝트를 다시 만든 뒤 status를 복원한다. status는 subresource라 create 요청에서 무시되므로
   따로 patch 한다.

   ```bash
   jq '.items[] | del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
         .metadata.generation, .metadata.managedFields, .status)' pvs-backup.json \
     | kubectl create -f -
   jq -c '.items[] | select(.status != null) | {name: .metadata.name, status: .status}' pvs-backup.json \
     | while read -r obj; do
         kubectl patch pillarvolumestates "$(jq -r .name <<<"$obj")" --subresource=status --type=merge \
           -p "$(jq -c '{status: .status}' <<<"$obj")"
       done
   kubectl get pvst
   ```

5. 복원 개수와 phase를 백업과 비교한 뒤 컨트롤러를 원래 replica 수로 되돌린다.

   ```bash
   helm upgrade "$RELEASE" <chart> -n "$HELM_NAMESPACE" --reuse-values --set controller.replicaCount="$REPLICAS"
   ```
