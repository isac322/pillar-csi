# Performance Tests — 성능 벤치마크

PRD §8.1의 비기능 성능 요구사항을 검증한다.
별도의 성능 테스트 인프라에서 실행하며, 기능 테스트와는 독립적으로 운영한다.

**빌드 태그:** `//go:build performance`
**실행:** `go test -tags=performance ./test/performance/ -v`
**인프라:** Kind + 실제 스토리지 backend (E2E 환경과 동일)

---

## P1: gRPC Agent 통신 오버헤드

> PRD §8.1: "gRPC agent 통신 오버헤드: < 1ms (LAN)"

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| P1-1 | `BenchmarkGRPC_AgentRoundTrip` | Agent gRPC round-trip 지연시간 측정 | Kind 클러스터; pillar-agent 배포; 같은 노드에서 gRPC 호출 | 1) GetCapabilities RPC 1000회 호출; 2) p50/p95/p99 지연시간 계산 | p99 < 1ms (LAN 환경) | `Agent`, `gRPC` |

---

## P2: 볼륨 프로비저닝 시간

> PRD §8.1: "볼륨 프로비저닝 시간: < 5초 (ZFS zvol 기준)"

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| P2-1 | `BenchmarkProvisioning_ZFSZvol` | ZFS zvol 프로비저닝 end-to-end 시간 측정 | Kind + 실제 ZFS zpool (loopback); StorageClass 설정 완료 | 1) PVC 생성; 2) PV Bound 대기; 3) 소요 시간 측정 | 소요 시간 < 5초 | `CSI-C`, `Agent`, `ZFS` |
| P2-2 | `BenchmarkProvisioning_LVM` | LVM LV 프로비저닝 시간 측정 | Kind + 실제 LVM VG (loopback) | 1) PVC 생성; 2) PV Bound 대기; 3) 시간 측정 | 소요 시간 기록 (기준값 TBD) | `CSI-C`, `Agent`, `LVM` |

---

## P3: 오퍼레이션 지연시간 메트릭

> PRD §8.4: "Prometheus 메트릭: 오퍼레이션 지연시간"

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| P3-1 | `BenchmarkMetrics_OperationLatency` | 프로비저닝 후 Prometheus 메트릭에 실제 지연시간 기록 확인 | Kind + 실제 backend; Prometheus 메트릭 활성 | 1) PVC 10개 생성; 2) /metrics 스크래핑; 3) histogram 값 확인 | pillar_operation_duration_seconds histogram에 10개 관측값; 값이 0이 아닌 실제 측정값 | `CSI-C`, `Agent` |

---

## P4: 동시 PVC 프로비저닝 스케일

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| P4-1 | `BenchmarkScale_ConcurrentPVCCreation` | 100개 PVC 동시 생성 시 모두 성공하고 총 소요 시간 기록 | Kind + 실제 backend; 충분한 pool 용량 | 1) 100개 PVC 동시 생성; 2) 모두 Bound 대기; 3) 총 소요 시간 및 실패율 기록 | 100% 성공; 총 시간 기록 (기준값 TBD) | `CSI-C`, `Agent` |
