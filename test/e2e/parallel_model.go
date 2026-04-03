package e2e

// parallel_model.go — 병렬성 모델 결정 (Sub-AC 2a)
//
// # 선택: 카테고리별 병렬 (Category-parallel)
//
// 각 TestXxx 함수에 t.Parallel()을 추가하는 방식을 채택한다.
// suite-level t.Parallel (TestE2E에 t.Parallel() 추가)은 채택하지 않는다.
//
// ## 테스트 레이어 구조
//
// 이 패키지에는 두 가지 독립적인 테스트 레이어가 존재한다:
//
// ### Layer 1 — Ginkgo 스펙 (404개 문서화된 TC)
//
// TestE2E (suite_test.go) 가 단일 Ginkgo 진입점이다. 내부 병렬성은 Ginkgo
// 자체의 프로세스 레벨 병렬화(--procs N)로 관리된다:
//   - TestMain → reexecViaGinkgoCLI → ginkgo --procs=N 으로 재실행
//   - N개의 독립 OS 프로세스가 각자 It() 노드 서브셋을 실행
//   - SynchronizedBeforeSuite: 클러스터 부트스트랩 (프로세스 1개만 실행)
//   - SynchronizedAfterSuite: 클러스터 정리 (프로세스 1개만 실행)
//
// TestE2E에 t.Parallel()을 추가하면 안 되는 이유:
//   - Ginkgo의 SynchronizedBeforeSuite barrier와 충돌한다 — Go 스케줄러가 TestE2E를
//     다른 TestXxx와 동시에 실행하면 SynchronizedBeforeSuite의 GinkgoWriter 상태
//     및 클러스터 부트스트랩 페이즈가 오염된다.
//   - reexecViaGinkgoCLI 내에서 이미 최적의 worker count로 프로세스 재실행을 관리한다.
//   - minParallelProcs=8 / maxParallelProcs=8 클램핑이 Kind API server 포화를 방지한다.
//
// ### Layer 2 — 프레임워크 유닛 테스트 (TestXxx 함수 ~1001개)
//
// 타이밍, 라우팅, 격리, 포트, 이미지 빌드 등 프레임워크 기계장치를 검증하는
// 순수 Go 테스트 함수들이다. 이들은 Ginkgo와 무관하게 go test로 실행된다.
//
// ## 결정: 카테고리별 병렬
//
// 모든 Layer 2 TestXxx 함수에 t.Parallel()을 추가한다. 단, 다음 예외를 적용한다:
//
// ### 예외 1 — TestE2E (절대 금지)
//
//   func TestE2E(t *testing.T) {
//       // t.Parallel() 금지 — Ginkgo 진입점이며 프로세스 재실행 방식으로 병렬화됨
//       RegisterFailHandler(Fail)
//       ...
//   }
//
// ### 예외 2 — t.Setenv()를 사용하는 함수 (금지)
//
// Go 런타임은 t.Parallel() 호출 이후에 t.Setenv()를 호출하면 panic을 발생시킨다:
//   "testing: t.Setenv called after t.Parallel; use t.Setenv before calling t.Parallel"
//
// 따라서 t.Setenv()를 사용하는 TestXxx 함수에는 t.Parallel()을 추가하지 않는다.
// 영향 받는 파일:
//   - failfast_ac10_test.go
//   - image_bootstrap_test.go
//   - invocation_environment_test.go
//   - image_pipeline_phase_test.go
//   - stage_timer_test.go
//   - debug_pipeline_test.go
//   - bottleneck_summary_test.go
//   - kind_bootstrap_test.go
//   - concurrent_invocation_isolation_test.go
//   - timing_flags_ac7_test.go
//   - suite_result_summary_test.go
//   - parallel_image_build_test.go
//   - kind_existing_cluster_test.go
//   - debug_tc_duration_test.go
//   - debug_tc_steps_test.go
//   - image_bootstrap.go (non-test, t.Setenv usage in helpers)
//
// ### 예외 3 — TestMain (해당 없음)
//
// TestMain은 testing.T를 받지 않으므로 t.Parallel() 대상이 아니다.
//
// ## 효과
//
// - Layer 2 TestXxx 함수들이 go test -run 'TestAC...' 형태로 실행될 때
//   Go 스케줄러가 GOMAXPROCS 범위 안에서 동시 실행 → 총 wall-clock 시간 단축
// - 2분 예산(suiteLevelTimeout) 내에 make test-e2e 완료를 달성하는 데 기여
// - 각 함수가 독립적인 격리 스코프(TestCaseScope)를 생성하므로 공유 뮤터블 상태 없음
//
// ## 구현 참조
//
// 병렬 실행 관련 검증 테스트:
//   - parallel_ac51_test.go: DefaultParallelNodes, worker count 클램핑, 격리 스코프 uniqueness
//   - tc_independence_test.go: TestAC34ExecutionIndependenceAcrossSchedules (이미 t.Parallel() 있음)
