# pillar-csi Agent Guide

## 프로젝트 정보

- API group / CSI provisioner name: `pillar-csi.bhyoo.com`
- CRD (전부 cluster-scoped): PillarAgent, PillarStore, PillarProtocol, PillarStorageClass, PillarVolumeState
- 요구사항 기준 문서: `docs/PRD.md`

## 생성 파일은 직접 수정하지 말 것

아래 파일은 소스에서 재생성한다. 손으로 고치면 CI의 `git diff --exit-code` 검사에서 실패한다.

|생성물|원본|재생성|
|---|---|---|
|`config/crd/`, `config/rbac/role.yaml`, `config/webhook/`, `charts/pillar-csi/templates/crds.yaml`|`api/v1alpha1/*_types.go` 마커, controller의 `//+kubebuilder:rbac` 마커|`make manifests`|
|`api/v1alpha1/zz_generated.deepcopy.go`|`api/v1alpha1/*_types.go` 필드|`make generate`|
|`gen/go/`|`proto/`|`make proto-gen`|

- 새 CRD, controller, webhook 스캐폴딩은 `kubebuilder` CLI로 만든다. `PROJECT` 파일이 스캐폴딩 상태를 추적한다.
- 직접 작성하는 코드: `api/v1alpha1/*_types.go`, `internal/controller/`, `internal/webhook/v1alpha1/`, `internal/agent/`, `internal/csi/`, `proto/`.
- proto 변경 시 `make proto-lint`, `make proto-breaking`으로 wire 호환성을 확인한다.

## No Silent Failures

- `configfs`, `sysfs` 등 커널 인터페이스 write 실패를 `continue`, 무시, debug 로그로 삼키지 말 것.
- write 후 실제 상태가 중요하면 즉시 read-back으로 검증하고, 기대값과 다르면 에러를 반환할 것.
- cleanup/rollback 경로라도 스토리지·연결 상태에 영향을 주는 실패는 호출자에게 반환하거나 최소한 `log.Error`로 남길 것.
- 에러 메시지에는 작업 종류(`write`, `disconnect`, `expand`), 대상(`path`, `NQN`, `device`, `volumeID`), 원인(`%w`)을 포함할 것.

## E2E 테스트 케이스

- `docs/E2E-TESTCASES.md`의 TC ID와 `test/e2e/`의 Ginkgo 노드 이름은 1:1로 대응한다. TC를 추가·삭제하면 둘 다 고치고 `make verify-tc-ids`로 확인한다.
- TC 구현 파일에서 조건부 `Skip()`을 쓰지 말고 `Fail()`/`Expect()`로 게이트한다.

## 커밋 전 검증

- `make lint` 결과가 0 issues가 아니면 커밋하지 말 것.
- `_types.go`, RBAC 마커, proto를 바꿨다면 해당 재생성 타겟을 실행하고 결과물을 함께 커밋한다.
- 빠른 단위 테스트는 `make test-fast`, envtest를 포함한 전체 테스트는 `make test`로 실행한다.
- 커밋 제목은 `fix:`, `test:`, `build(deps):` 같은 conventional prefix를 붙이고, 무엇을 했는지보다 왜 바꿨는지를 적는다.
