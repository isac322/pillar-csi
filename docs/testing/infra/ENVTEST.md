# envtest 테스트 인프라 전략 (SSOT)

> **수준 B** — 규칙 + 검증 명령 + 기대 출력 패턴
>
> **적용 범위:** pillar-csi Integration 테스트에서 envtest(실제 kube-apiserver + etcd)를
> 사용하는 모든 TC (E19–E21, webhook 검증, controller reconcile 등)
>
> **크로스레퍼런스:**
> - [Integration 테스트](../INTEGRATION-TESTS.md) — envtest 기반 TC 목록 (E19, E20, E21 등)
> - [테스트 전략 README](../README.md) — 테스트 피라미드 및 분류 기준
> - [인프라 전략 인덱스](README.md) — 공통 원칙 및 기술 간 의존 관계
> - Suite 설정: `internal/controller/suite_test.go`
> - Webhook 테스트: `internal/webhook/v1alpha1/`
> - CSI 통합 테스트: `internal/csi/`
> - Makefile 타겟: `make test`, `make setup-envtest`

---

## 1. 호스트 사전조건 (Host Prerequisites)

envtest는 실제 kube-apiserver와 etcd 바이너리를 다운로드하여 로컬에서 실행한다.

### 규칙

- `setup-envtest` CLI 도구가 설치되어 있어야 한다 — `make setup-envtest`으로 자동 설치.
- kube-apiserver, etcd 바이너리가 `KUBEBUILDER_ASSETS` 경로에 존재해야 한다.
- K8s 버전은 `go.mod`의 `k8s.io/api` 모듈 버전에서 추출 — 수동 지정 불필요.
- Docker/Kind 불필요 — 순수 로컬 프로세스로 실행.
- 빌드 태그 `//go:build integration` 필수 — `go test -tags=integration` 으로만 실행.
- GHA ubuntu-latest에서 추가 패키지 설치 불필요 (Go + setup-envtest으로 충분).

### 검증 명령 및 기대 출력

**setup-envtest 설치 확인:**

```bash
go run sigs.k8s.io/controller-runtime/tools/setup-envtest list 2>&1 | head -3
```

**기대 출력 패턴:**
```
Version.*Path
\d+\.\d+\.\d+\s+.*
```

**KUBEBUILDER_ASSETS 경로 확인:**

```bash
eval $(go run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p env) && \
  ls $KUBEBUILDER_ASSETS/kube-apiserver $KUBEBUILDER_ASSETS/etcd
```

**기대 출력 패턴:**
```
.*/kube-apiserver
.*/etcd
```

**kube-apiserver 바이너리 실행 가능 확인:**

```bash
eval $(go run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p env) && \
  $KUBEBUILDER_ASSETS/kube-apiserver --version
```

**기대 출력 패턴:**
```
Kubernetes v\d+\.\d+\.\d+
```

**etcd 바이너리 실행 가능 확인:**

```bash
eval $(go run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p env) && \
  $KUBEBUILDER_ASSETS/etcd --version | head -1
```

**기대 출력 패턴:**
```
etcd Version: \d+\.\d+\.\d+
```

**GHA CI 사전 설치 (Makefile 기준):**

```yaml
- name: Setup envtest
  run: make setup-envtest
```

---

## 2. 리소스 생성 (envtest Environment Setup)

### 규칙

- envtest Environment는 `envtest.Environment{CRDDirectoryPaths: [...]}` 구조체로 초기화한다.
- CRD 매니페스트 경로: `config/crd/bases/` 디렉토리 — `make manifests`로 생성된 YAML.
- Environment.Start()가 실제 kube-apiserver + etcd 프로세스를 시작한다.
- 반환된 `*rest.Config`로 controller-runtime Client를 생성한다.
- webhook 테스트 시 `WebhookInstallOptions` 설정 필요 — 로컬 webhook 서버 자동 시작.
- CRD 설치는 `envtest.InstallCRDs()`가 자동 수행 — 수동 `kubectl apply` 불필요.
- 한 Suite(패키지)당 하나의 envtest Environment만 생성 — `TestMain` 또는 `BeforeSuite`에서 초기화.

### 검증 명령 및 기대 출력

**CRD 매니페스트 존재 확인:**

```bash
ls config/crd/bases/*.yaml
```

**기대 출력 패턴:**
```
config/crd/bases/pillar-csi\.bhyoo\.com_pillar.*\.yaml
```

**CRD YAML 유효성 확인 (apiVersion 필드):**

```bash
head -3 config/crd/bases/pillar-csi.bhyoo.com_pillartargets.yaml
```

**기대 출력 패턴:**
```
apiVersion: apiextensions\.k8s\.io/v1
kind: CustomResourceDefinition
metadata:
```

**envtest 실행 확인 (make test):**

```bash
make test 2>&1 | tail -5
```

**기대 출력 패턴:**
```
ok\s+github\.com/bhyoo/pillar-csi/internal/controller\s+\d+\.\d+s
```

---

## 3. 리소스 정리 (envtest Environment Teardown)

### 규칙

- `Environment.Stop()`이 kube-apiserver와 etcd 프로세스를 종료한다.
- `AfterSuite`에서 반드시 `testEnv.Stop()` 호출 — 프로세스 누수 방지.
- etcd 데이터 디렉토리는 임시 디렉토리에 생성되어 자동 정리.
- Stop() 호출 전 모든 controller가 StopFunc를 통해 정지되어야 한다.
- Stop() 실패 시 `os.Exit(1)` 전에 에러 로그를 남긴다.
- webhook 서버도 Environment.Stop()에서 자동 종료.

### 검증 명령 및 기대 출력

**envtest 프로세스 잔여 확인 (Suite 종료 후):**

```bash
pgrep -f kube-apiserver | wc -l
pgrep -f etcd | wc -l
```

**기대 출력:**
```
0
0
```

**임시 디렉토리 정리 확인:**

```bash
ls /tmp/envtest-* 2>/dev/null | wc -l
```

**기대 출력:**
```
0
```

---

## 4. TC간 격리 (Test Case Isolation)

### 규칙

- 같은 패키지 내 모든 TC는 동일한 envtest Environment(apiserver + etcd)를 공유한다.
- TC 간 격리는 **K8s namespace** 단위로 수행 — 각 TC가 고유 namespace를 생성.
- cluster-scoped 리소스(PillarTarget, PillarPool 등)는 이름에 TC 고유 접미사를 포함.
- `BeforeEach`에서 테스트용 namespace 생성, `AfterEach`에서 삭제.
- controller 재등록은 불필요 — Environment 레벨에서 한 번 등록, TC 레벨에서는 리소스만 격리.
- 동시 TC 실행 시 Ginkgo의 직렬 모드(`Ordered` 컨테이너) 또는 namespace 격리로 충돌 방지.

### 검증 명령 및 기대 출력

**TC 전용 namespace 생성 확인 (TC 실행 중):**

```bash
# envtest 내부에서 kubectl 사용 불가 — Go 테스트 코드에서 확인
go test -tags=integration -v ./internal/controller/... -run 'TestControllers' 2>&1 | grep 'namespace'
```

**기대 출력 패턴:**
```
.*Creating namespace.*test-.*
```

**TC 완료 후 namespace 정리 확인:**

```bash
go test -tags=integration -v ./internal/controller/... -run 'TestControllers' 2>&1 | grep -i 'delet.*namespace'
```

**기대 출력 패턴:**
```
.*Delet.*namespace.*test-.*
```

**cluster-scoped 리소스 이름 충돌 검사 (테스트 실행 중):**

```bash
go test -tags=integration -v ./internal/controller/... -count=1 2>&1 | grep -c 'already exists'
```

**기대 출력:**
```
0
```

---

## 5. 사이징 (Sizing)

### 규칙

- envtest 프로세스 메모리: kube-apiserver ~100 MB + etcd ~50 MB (GHA 기준 충분).
- GHA ubuntu-latest 러너: 7 GB RAM, 2 CPU — envtest 실행에 충분.
- etcd 데이터: TC당 수 MB (CRD 오브젝트 수십 개 수준).
- envtest 시작 시간: 첫 번째 실행 ~5초, 이후 바이너리 캐시로 ~2초.
- TC 수행 시간: 개별 TC ~100ms–500ms (I/O 없는 API 서버 호출만).
- 전체 integration suite: `make test` 기준 10초 내외 목표.

### 검증 명령 및 기대 출력

**envtest 바이너리 캐시 크기 확인:**

```bash
du -sh $(go run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p path)
```

**기대 출력 패턴:**
```
\d+M\s+.*
```

**GHA runner 리소스 확인:**

```bash
free -m | grep Mem | awk '{print $2}'
nproc
```

**기대 출력 패턴:**
```
\d{4,}
\d+
```

**테스트 실행 시간 확인:**

```bash
time make test 2>&1 | grep '^real'
```

**기대 출력 패턴:**
```
real\s+\d+m\d+\.\d+s
```
> 10초 이내 완료 목표.

---

## 6. 실패 시 정리 (Failure Cleanup)

### 규칙

- TC 실패 시에도 `AfterEach`/`AfterSuite`에서 namespace 삭제와 Environment.Stop() 실행.
- panic 발생 시 Ginkgo의 `DeferCleanup`이 정리를 보장한다.
- kube-apiserver/etcd 프로세스가 orphan이 되면 OS가 timeout 후 SIGTERM 전송.
- envtest의 `UseExistingCluster` 옵션은 false(기본값) — 항상 새 프로세스 시작/종료.
- 테스트 timeout은 `go test -timeout` 플래그로 제한 (기본 10분).
- timeout 도달 시 Go test runner가 모든 goroutine의 스택 트레이스를 출력 후 종료.

### 검증 명령 및 기대 출력

**orphan 프로세스 확인:**

```bash
pgrep -f 'kube-apiserver.*envtest' 2>/dev/null | wc -l
```

**기대 출력:**
```
0
```

**Go test timeout 설정 확인 (Makefile):**

```bash
grep -o 'timeout[= ]*[0-9]*[ms]' Makefile | head -3
```

**기대 출력 패턴:**
```
timeout.*\d+[ms]
```

---

## 7. CI 호환성 (CI Compatibility)

### 규칙

- CI 환경: GitHub Actions `ubuntu-latest`.
- Go 버전: `go.mod`의 `go` 디렉티브에서 자동 추출 — `actions/setup-go`의 `go-version-file: go.mod`.
- envtest 바이너리: `make setup-envtest`이 `KUBEBUILDER_ASSETS` 경로에 다운로드.
- 추가 시스템 패키지 불필요 — Go toolchain만으로 충분.
- Docker 불필요 — envtest 테스트는 Kind 클러스터 없이 실행.
- `make test` 타겟이 `setup-envtest` + `go test -tags=integration` 을 자동 수행.
- 캐시: `actions/setup-go`의 `cache: true`로 Go 모듈 캐시 + envtest 바이너리 캐시.
- 네트워크: envtest 바이너리 다운로드 시 인터넷 접근 필요 (첫 실행 시만).

### 검증 명령 및 기대 출력

**Go 버전 호환성 확인:**

```bash
go version
```

**기대 출력 패턴:**
```
go version go\d+\.\d+(\.\d+)? .*
```

**envtest 바이너리 아키텍처 확인:**

```bash
eval $(go run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p env) && \
  file $KUBEBUILDER_ASSETS/kube-apiserver
```

**기대 출력 패턴:**
```
.*ELF 64-bit.*x86-64.*
```

**make test 실행 확인:**

```bash
make test 2>&1 | tail -1
```

**기대 출력 패턴:**
```
(ok|PASS).*
```

**빌드 태그 확인 (integration tests만 envtest 사용):**

```bash
grep -r '//go:build integration' internal/ | head -5
```

**기대 출력 패턴:**
```
internal/controller/suite_test\.go://go:build integration
internal/csi/.*_test\.go://go:build integration
```

---

## 8. CRD 및 Webhook 등록 (추가 차원)

### 규칙

- CRD 매니페스트는 `make manifests`로 생성 — 수동 편집 금지.
- CRD 변경 후 반드시 `make manifests` → `make generate` 실행.
- envtest에 CRD 등록: `CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}`.
- webhook 등록: `WebhookInstallOptions{Paths: []string{filepath.Join("..", "..", "config", "webhook")}}`.
- 스키마 등록: `runtime.SchemeBuilder`로 API 타입을 scheme에 추가.
- Admission webhook 테스트: envtest가 로컬 webhook 서버를 자동으로 시작.
- RBAC은 envtest에서 기본 무제한 — RBAC 테스트는 별도 설정 필요.

### 검증 명령 및 기대 출력

**CRD 매니페스트 최신 여부 확인:**

```bash
make manifests 2>&1 && git diff --stat config/crd/bases/
```

**기대 출력:**
```
(빈 출력 — 변경 없음 = 최신 상태)
```

**등록된 CRD 목록 확인 (make test 로그):**

```bash
go test -tags=integration -v ./internal/controller/... -run 'TestControllers' 2>&1 | grep -i 'CRD\|install'
```

**기대 출력 패턴:**
```
.*install.*CRD.*
```

**webhook 설정 경로 확인:**

```bash
ls config/webhook/manifests.yaml
```

**기대 출력:**
```
config/webhook/manifests.yaml
```

---

## 9. 테스트 프레임워크 패턴 (추가 차원)

### 규칙

- Ginkgo BDD 스타일: `Describe`/`Context`/`It`/`BeforeEach`/`AfterEach`.
- Gomega 매처: `Expect().To()`, `Eventually().Should()` (비동기 reconcile 대기).
- `Eventually` 타임아웃: 기본 5초, 폴링 간격 200ms.
- controller 테스트: `Reconcile(ctx, req)` 직접 호출 또는 controller-runtime Manager 경유.
- 상태 검증: `k8sClient.Get()` → `Expect(obj.Status.Conditions).To(ContainElement(...))`.
- 에러 검증: `Expect(err).To(HaveOccurred())`, `Expect(apierrors.IsNotFound(err)).To(BeTrue())`.

### 검증 명령 및 기대 출력

**Ginkgo 테스트 실행 확인:**

```bash
go test -tags=integration -v ./internal/controller/... -count=1 2>&1 | grep -E '(PASS|FAIL|It\[)'
```

**기대 출력 패턴:**
```
.*(PASS|ok).*
```

**Eventually 타임아웃 내 reconcile 완료 확인:**

```bash
go test -tags=integration -v ./internal/controller/... -count=1 2>&1 | grep -c 'timed out'
```

**기대 출력:**
```
0
```

---

## 차원 요약 매트릭스

| 차원 | 규칙 수 | 검증 명령 수 | 핵심 도구 |
|------|--------|------------|----------|
| 호스트 사전조건 | 6 | 4 | `setup-envtest`, `kube-apiserver --version` |
| 리소스 생성 | 7 | 3 | `envtest.Environment`, CRD YAML, `make manifests` |
| 리소스 정리 | 6 | 2 | `Environment.Stop()`, `pgrep` |
| TC간 격리 | 6 | 3 | namespace, cluster-scoped name suffix |
| 사이징 | 6 | 3 | `du`, `free`, `time make test` |
| 실패 시 정리 | 6 | 2 | `pgrep`, `go test -timeout` |
| CI 호환성 | 8 | 4 | `go version`, `file`, `make test`, build tags |
| CRD/Webhook 등록 | 7 | 3 | `make manifests`, CRD path, webhook path |
| 테스트 프레임워크 | 6 | 2 | Ginkgo/Gomega, `Eventually` |
