# Helm 테스트 인프라 전략 (SSOT)

> **수준 B** — 규칙 + 검증 명령 + 기대 출력 패턴
>
> **적용 범위:** pillar-csi E2E/Integration 테스트에서 Helm 차트 배포를 사용하는 모든 TC
> (E27 Helm integration, E33–E35 E2E 배포 등)
>
> **크로스레퍼런스:**
> - [E2E 테스트 케이스](../E2E-TESTS.md) — Helm 기반 E2E 배포
> - [Integration 테스트](../INTEGRATION-TESTS.md) — E27: Helm 차트 검증
> - [테스트 전략 README](../README.md) — 테스트 피라미드 및 분류 기준
> - [인프라 전략 인덱스](README.md) — 공통 원칙 및 기술 간 의존 관계
> - [Kind 인프라](KIND.md) — Helm은 Kind 클러스터 위에서 실행
> - 프레임워크 코드: `test/e2e/helm_bootstrap_e2e.go`, `helm_bootstrap_test.go`
> - Helm 차트: `charts/pillar-csi/` 디렉토리 (Chart.yaml v0.1.0)
> - TC: `test/e2e/tc_e27_helm_e2e_test.go`
> - Makefile 변수: `E2E_HELM_RELEASE`, `E2E_HELM_NAMESPACE`, `E2E_HELM_BOOTSTRAP`

---

## 1. 호스트 사전조건 (Host Prerequisites)

Helm E2E 테스트는 Helm CLI와 Kind 클러스터가 필요하다.

### 규칙

- `helm` CLI 도구가 설치되어 있어야 한다 — CI에서는 `azure/setup-helm@v5`.
- Kind 클러스터가 실행 중이어야 한다 — [Kind 인프라](KIND.md) 참조.
- `kubectl`이 Kind 클러스터에 접근 가능해야 한다 — `KUBECONFIG` 설정.
- Helm 차트 디렉토리 `charts/pillar-csi/`가 존재하고, `Chart.yaml`이 유효해야 한다.
- pillar-csi Docker 이미지가 Kind 노드에 로드되어 있어야 한다.
- sidecar 이미지(csi-provisioner, csi-attacher, csi-resizer, livenessprobe, csi-node-driver-registrar)가 Kind 노드에 로드되어 있어야 한다.

### 검증 명령 및 기대 출력

**helm CLI 설치 확인:**

```bash
helm version --short
```

**기대 출력 패턴:**
```
v3\.\d+\.\d+\+.*
```

**Helm 차트 디렉토리 존재 확인:**

```bash
ls charts/pillar-csi/Chart.yaml
```

**기대 출력:**
```
charts/pillar-csi/Chart.yaml
```

**Chart.yaml 유효성 확인:**

```bash
helm lint charts/pillar-csi/
```

**기대 출력 패턴:**
```
==> Linting charts/pillar-csi/
\[INFO\].*
1 chart\(s\) linted, 0 chart\(s\) failed
```

**Kind 클러스터 접근 확인:**

```bash
kubectl cluster-info --context kind-pillar-csi-e2e 2>&1 | head -1
```

**기대 출력 패턴:**
```
Kubernetes control plane is running at.*
```

**pillar-csi 이미지 존재 확인 (Kind 노드):**

```bash
docker exec pillar-csi-e2e-control-plane crictl images | grep pillar-csi
```

**기대 출력 패턴:**
```
.*pillar-csi.*(controller|agent|node).*e2e.*
```

---

## 2. 리소스 생성 (Helm Release Installation)

### 규칙

- Helm 릴리스 이름: `E2E_HELM_RELEASE` 환경변수 (기본값: `pillar-csi`).
- Helm 네임스페이스: `E2E_HELM_NAMESPACE` 환경변수 (기본값: `pillar-csi-system`).
- namespace가 없으면 `--create-namespace` 플래그로 자동 생성.
- 이미지 태그: `--set image.tag=${E2E_IMAGE_TAG}` (기본값: `e2e`).
- `E2E_HELM_BOOTSTRAP=true` 설정 시 Suite 레벨에서 자동 Helm install.
- 병렬 워커 조율: worker-1(node-1)이 install 수행, 다른 워커는 대기 후 readiness 확인.
- `helm install`이 아닌 `helm upgrade --install`로 멱등성 보장.
- values override는 `--set` 또는 `--values` 플래그로 전달.
- CRD 설치: Helm 차트의 `crds/` 디렉토리 또는 `installCRDs: true`.
- Pod readiness 대기: `--wait --timeout=120s` 플래그.

### 검증 명령 및 기대 출력

**Helm 릴리스 설치 확인:**

```bash
helm list -n pillar-csi-system --kube-context kind-pillar-csi-e2e --output json | \
  python3 -c "import sys,json; d=json.load(sys.stdin); print(d[0]['status'] if d else 'NOT_FOUND')"
```

**기대 출력:**
```
deployed
```

**간단한 릴리스 상태 확인:**

```bash
helm status pillar-csi -n pillar-csi-system --kube-context kind-pillar-csi-e2e 2>&1 | grep STATUS
```

**기대 출력:**
```
STATUS: deployed
```

**배포된 Pod 상태 확인:**

```bash
kubectl get pods -n pillar-csi-system --context kind-pillar-csi-e2e -o wide --no-headers
```

**기대 출력 패턴:**
```
pillar-csi-controller-.*\s+\d+/\d+\s+Running\s+0\s+.*
pillar-csi-node-.*\s+\d+/\d+\s+Running\s+0\s+.*
```

**CRD 설치 확인:**

```bash
kubectl get crd --context kind-pillar-csi-e2e -o name | grep pillar-csi
```

**기대 출력 패턴:**
```
customresourcedefinition\.apiextensions\.k8s\.io/pillar.*\.pillar-csi\.bhyoo\.com
```

**namespace 생성 확인:**

```bash
kubectl get namespace pillar-csi-system --context kind-pillar-csi-e2e -o jsonpath='{.status.phase}'
```

**기대 출력:**
```
Active
```

---

## 3. 리소스 정리 (Helm Release Uninstallation)

### 규칙

- Suite 종료 시 `helm uninstall <RELEASE> -n <NAMESPACE>` 실행.
- CRD는 Helm uninstall로 삭제되지 않음 — 수동 `kubectl delete crd` 필요시에만.
- E2E에서는 Kind 클러스터 삭제가 최종 정리 — Helm uninstall은 클러스터 재사용 시에만 필요.
- `--wait` 플래그로 모든 리소스가 삭제될 때까지 대기.
- PVC/PV는 Helm에 의해 관리되지 않으므로, TC 레벨에서 별도 정리.
- 2-pass 실행 시: 첫 번째 pass 후 `helm upgrade --install`로 두 번째 mode 설정 적용.

### 검증 명령 및 기대 출력

**Helm 릴리스 삭제 후 확인:**

```bash
helm list -n pillar-csi-system --kube-context kind-pillar-csi-e2e --short
```

**기대 출력:**
```
(빈 출력)
```

**Pod 정리 확인:**

```bash
kubectl get pods -n pillar-csi-system --context kind-pillar-csi-e2e --no-headers 2>&1
```

**기대 출력 패턴:**
```
(빈 출력 또는 "No resources found")
```

**namespace 잔여 확인 (Kind 클러스터 삭제 전):**

```bash
kubectl get namespace pillar-csi-system --context kind-pillar-csi-e2e -o jsonpath='{.status.phase}' 2>&1
```

**기대 출력:**
```
(Active 또는 Terminating — 클러스터 삭제 시 자동 정리)
```

---

## 4. TC간 격리 (Test Case Isolation)

### 규칙

- Helm 배포는 Suite 레벨에서 한 번 수행 — TC마다 재배포하지 않는다.
- TC 간 격리는 **K8s namespace** 단위 — 각 TC가 자체 namespace에서 PVC/Pod를 생성.
- controller, node DaemonSet은 `pillar-csi-system` namespace에서 실행 — 모든 TC에 서비스 제공.
- TC에서 Helm values를 변경해야 하는 경우 `helm upgrade`로 in-place 업데이트.
- 빌드 태그: `//go:build e2e && e2e_helm` — Helm-specific TC만 별도 태그.
- `E2E_HELM_BOOTSTRAP=true` 환경변수로 Helm 자동 배포 활성화.
- 병렬 워커 조율: Ginkgo의 `SynchronizedBeforeSuite`로 node-1만 Helm install 실행.
- 릴리스 이름 충돌 방지: 각 테스트 실행은 고유한 릴리스 이름(예: `pillar-csi-<RANDOM_SUFFIX>`)을 사용하여 동시 실행 간 충돌을 방지한다. 기본 `E2E_HELM_RELEASE` 값은 병렬 CI에서 사용하지 않는다.

### 검증 명령 및 기대 출력

**TC 전용 namespace에서 PVC 확인 (실행 중):**

```bash
kubectl get pvc -n test-<TC_ID> --context kind-pillar-csi-e2e --no-headers
```

**기대 출력 패턴 (실행 중):**
```
pvc-.*\s+(Bound|Pending)\s+.*
```

**TC 완료 후 PVC 정리 확인:**

```bash
kubectl get pvc --all-namespaces --context kind-pillar-csi-e2e -o name | grep 'test-' | wc -l
```

**기대 출력:**
```
0
```

**Helm 릴리스가 TC 간 공유됨을 확인:**

```bash
helm list -n pillar-csi-system --kube-context kind-pillar-csi-e2e --short | wc -l
```

**기대 출력:**
```
1
```

---

## 5. 사이징 (Sizing)

### 규칙

- Helm install 시간: ~10초 (이미지 pre-loaded, `--wait` 포함).
- controller Pod 리소스: requests 64Mi memory, 100m CPU (Helm values에서 설정).
- node DaemonSet Pod: 노드당 1개, 각 128Mi memory requests.
- Helm chart 크기: 수십 KB (template + values).
- `--wait --timeout=120s`: Pod readiness 대기 최대 120초.
- Helm 릴리스 히스토리: 기본 10개 revision 유지.
- GHA runner에서 Helm + Kind + 워크로드: 총 메모리 ~2 GB 이내.

### 검증 명령 및 기대 출력

**controller Pod 리소스 사용량:**

```bash
kubectl top pod -n pillar-csi-system --context kind-pillar-csi-e2e --no-headers 2>/dev/null
```

**기대 출력 패턴:**
```
pillar-csi-controller-.*\s+\d+m\s+\d+Mi
```

**Helm 차트 크기:**

```bash
du -sh charts/pillar-csi/
```

**기대 출력 패턴:**
```
\d+K\s+charts/pillar-csi/
```

**Helm 릴리스 히스토리 크기:**

```bash
helm history pillar-csi -n pillar-csi-system --kube-context kind-pillar-csi-e2e --max 5 2>/dev/null | wc -l
```

**기대 출력 패턴:**
```
[2-6]
```
> 헤더 1줄 + revision 1줄 이상.

---

## 6. 실패 시 정리 (Failure Cleanup)

### 규칙

- Helm install 실패 시 `helm rollback` 또는 `helm uninstall --no-hooks`로 정리.
- TC 실패 시 Helm 배포 자체는 유지 — 다른 TC에 영향을 주지 않기 위해.
- Pod CrashLoopBackOff 발생 시 로그를 수집한 후 정리: `kubectl logs -n pillar-csi-system <POD> --previous`.
- Kind 클러스터 삭제가 최종 안전망 — 모든 K8s 리소스가 함께 삭제.
- `helm install` timeout 시 릴리스가 `failed` 상태로 남음 — `helm upgrade --install`로 복구 가능.
- Pending Pod는 event로 원인 확인: `kubectl describe pod -n pillar-csi-system <POD>`.

### 검증 명령 및 기대 출력

**Helm 릴리스 상태 확인 (실패 감지):**

```bash
helm status pillar-csi -n pillar-csi-system --kube-context kind-pillar-csi-e2e -o json 2>/dev/null | \
  python3 -c "import sys,json; print(json.load(sys.stdin)['info']['status'])"
```

**기대 출력 (정상):**
```
deployed
```

**비정상 출력 예:**
```
failed
pending-install
```

**Pod 장애 로그 수집:**

```bash
kubectl logs -n pillar-csi-system --context kind-pillar-csi-e2e -l app.kubernetes.io/name=pillar-csi --tail=20
```

**기대 출력 패턴 (정상 시):**
```
.*starting.*
.*ready.*
```

**CrashLoopBackOff Pod 감지:**

```bash
kubectl get pods -n pillar-csi-system --context kind-pillar-csi-e2e --field-selector=status.phase!=Running --no-headers 2>/dev/null | wc -l
```

**기대 출력 (정상):**
```
0
```

**Event로 원인 확인:**

```bash
kubectl get events -n pillar-csi-system --context kind-pillar-csi-e2e --sort-by='.lastTimestamp' --field-selector type=Warning 2>/dev/null | tail -5
```

**기대 출력 (정상 — 경고 없음):**
```
(빈 출력)
```

---

## 7. CI 호환성 (CI Compatibility)

### 규칙

- CI 환경: GitHub Actions `ubuntu-latest`.
- Helm 설치: `azure/setup-helm@v5` (버전 자동).
- Kind 클러스터가 먼저 생성되어야 Helm install 가능 — [Kind CI 호환성](KIND.md#7-ci-호환성-ci-compatibility) 참조.
- Docker 이미지 pre-pull: Docker Hub rate limit 회피를 위해 CI step에서 사전 pull.
- 이미지 Kind 로드: `docker save | docker cp` 또는 `kind load docker-image`.
- `E2E_HELM_NAMESPACE=pillar-csi-system`: CI 환경변수에서 설정.
- `E2E_PROCS=8`: 병렬 워커 중 node-1만 Helm install 수행.
- Helm chart 경로는 상대 경로 `charts/pillar-csi/` 사용 — CI에서도 동일.
- DOCKER_HOST: 환경변수에서만 읽음 — 하드코딩 금지.

### 검증 명령 및 기대 출력

**Helm CLI 존재 확인:**

```bash
which helm
```

**기대 출력 패턴:**
```
/usr/(local/)?bin/helm
```

**Helm chart 렌더링 테스트 (CI dry-run):**

```bash
helm template pillar-csi charts/pillar-csi/ --namespace pillar-csi-system 2>&1 | head -5
```

**기대 출력 패턴:**
```
# Source: pillar-csi/templates/.*
apiVersion:.*
kind:.*
```

**CI 환경변수 확인:**

```bash
echo "E2E_HELM_NAMESPACE=${E2E_HELM_NAMESPACE:-pillar-csi-system}"
echo "E2E_HELM_RELEASE=${E2E_HELM_RELEASE:-pillar-csi}"
echo "E2E_IMAGE_TAG=${E2E_IMAGE_TAG:-e2e}"
```

**기대 출력:**
```
E2E_HELM_NAMESPACE=pillar-csi-system
E2E_HELM_RELEASE=pillar-csi
E2E_IMAGE_TAG=e2e
```

---

## 8. Helm Values 관리 (추가 차원)

### 규칙

- 기본 values: `charts/pillar-csi/values.yaml` (프로덕션 기본값).
- E2E override values: `--set` 플래그로 테스트 전용 값 주입.
- 이미지 설정: `image.repository`, `image.tag`, `image.pullPolicy=Never` (로컬 이미지).
- replicas 설정: controller는 1, node DaemonSet은 노드 수에 맞게 자동.
- 로그 레벨: E2E에서는 `-v=4` 이상으로 설정하여 디버깅 용이.
- feature gate: `--set featureGates.<FEATURE>=true`로 특정 기능 활성화/비활성화 테스트.
- values 변경 시 `helm upgrade`로 rolling update — 재설치 불필요.

### 검증 명령 및 기대 출력

**현재 적용된 values 확인:**

```bash
helm get values pillar-csi -n pillar-csi-system --kube-context kind-pillar-csi-e2e -o yaml 2>/dev/null
```

**기대 출력 패턴:**
```
image:
  tag: e2e
  pullPolicy: Never
```

**values 기본값 확인:**

```bash
helm show values charts/pillar-csi/ | head -20
```

**기대 출력 패턴:**
```
# Default values for pillar-csi.*
image:
  repository:.*
  tag:.*
```

**computed manifest 확인 (디버깅):**

```bash
helm get manifest pillar-csi -n pillar-csi-system --kube-context kind-pillar-csi-e2e 2>/dev/null | grep 'kind:' | sort -u
```

**기대 출력 패턴:**
```
kind: ClusterRole
kind: ClusterRoleBinding
kind: DaemonSet
kind: Deployment
kind: Namespace
kind: ServiceAccount
```

---

## 9. 버전 호환성 (Version Compatibility)

### 규칙

- Helm CLI v3.12+ 필요 — Chart apiVersion v2 전용 (Helm 2/Tiller 미지원).
- Chart apiVersion: v2 (`charts/pillar-csi/Chart.yaml`).
- Kind가 제공하는 K8s 버전과 Helm의 K8s 호환성 매트릭스 일치 필요.
- sigs.k8s.io/kind v0.31.0 → K8s 1.32 기본 노드 이미지.
- sigs.k8s.io/controller-runtime v0.23.3 → K8s 1.32 호환.
- CSI sidecar 이미지 버전은 `values.yaml`에 고정 — K8s 버전과 호환 확인.
- Helm 3.12+ → K8s 1.26–1.32 지원.

### 검증 명령 및 기대 출력

**Helm CLI 최소 버전 확인:**

```bash
helm version --template '{{.Version}}' | sed 's/v//'
```

**기대 출력 패턴:**
```
3\.(1[2-9]|[2-9]\d)\.\d+
```

*v3.12.0 이상이어야 한다.*

**Chart apiVersion 확인:**

```bash
grep 'apiVersion:' charts/pillar-csi/Chart.yaml
```

**기대 출력:**
```
apiVersion: v2
```

**Kind K8s 버전과 Helm 호환성 확인:**

```bash
kubectl version --kubeconfig "${KUBECONFIG}" -o json 2>/dev/null | \
  grep -o '"gitVersion":"v[^"]*"'
```

**기대 출력 패턴:**
```
"gitVersion":"v1\.(2[6-9]|3[0-9])\.\d+"
```

**CSI sidecar 이미지 버전 확인:**

```bash
helm template pillar-csi charts/pillar-csi/ 2>/dev/null | \
  grep 'image:' | grep -oP 'registry\.k8s\.io/sig-storage/\S+'
```

**기대 출력 패턴:**
```
registry\.k8s\.io/sig-storage/csi-provisioner:v\d+\.\d+\.\d+
registry\.k8s\.io/sig-storage/csi-attacher:v\d+\.\d+\.\d+
registry\.k8s\.io/sig-storage/csi-resizer:v\d+\.\d+\.\d+
registry\.k8s\.io/sig-storage/livenessprobe:v\d+\.\d+\.\d+
registry\.k8s\.io/sig-storage/csi-node-driver-registrar:v\d+\.\d+\.\d+
```

---

## 10. Helm 차트 검증 (추가 차원)

### 규칙

- `helm lint charts/pillar-csi/`: 차트 구조 및 template 문법 검증.
- `helm template charts/pillar-csi/`: 렌더링 결과 확인 (클러스터 접근 없이).
- `helm install --dry-run --debug`: 클러스터 접근 포함 시뮬레이션.
- E27 TC에서 Helm 차트의 기능별 검증 수행.
- chart version은 `Chart.yaml`의 `version` 필드에서 관리.
- appVersion은 Go 모듈 버전과 일치시킨다.

### 검증 명령 및 기대 출력

**차트 lint:**

```bash
helm lint charts/pillar-csi/ --strict
```

**기대 출력 패턴:**
```
==> Linting charts/pillar-csi/
\[INFO\].*
1 chart\(s\) linted, 0 chart\(s\) failed
```

**차트 template 렌더링:**

```bash
helm template test charts/pillar-csi/ --namespace test-ns 2>&1 | grep 'kind:' | wc -l
```

**기대 출력 패턴:**
```
\d+
```
> 최소 5개 이상의 K8s 리소스 종류.

**Chart.yaml 유효성:**

```bash
grep -E '^(apiVersion|name|version|appVersion):' charts/pillar-csi/Chart.yaml
```

**기대 출력 패턴:**
```
apiVersion: v2
name: pillar-csi
version: \d+\.\d+\.\d+.*
appVersion: .*
```

---

## 차원 요약 매트릭스

| 차원 | 규칙 수 | 검증 명령 수 | 핵심 도구 |
|------|--------|------------|----------|
| 호스트 사전조건 | 6 | 5 | `helm version`, `helm lint`, `crictl images` |
| 리소스 생성 | 10 | 5 | `helm upgrade --install`, `kubectl get` |
| 리소스 정리 | 6 | 3 | `helm uninstall`, `kubectl get pods` |
| TC간 격리 | 7 | 3 | namespace, `SynchronizedBeforeSuite` |
| 사이징 | 7 | 3 | `kubectl top`, `du`, `helm history` |
| 실패 시 정리 | 6 | 4 | `helm status`, `kubectl logs`, `kubectl get events` |
| CI 호환성 | 9 | 3 | `which helm`, `helm template`, env vars |
| Values 관리 | 7 | 3 | `helm get values`, `helm show values` |
| 버전 호환성 | 7 | 4 | Helm version, apiVersion, K8s compat, sidecar images |
| 차트 검증 | 6 | 3 | `helm lint`, `helm template`, `Chart.yaml` |
