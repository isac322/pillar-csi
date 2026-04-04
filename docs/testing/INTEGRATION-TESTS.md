# Integration Tests — 실제 외부 시스템 경계 검증

실제 외부 시스템(K8s API, LVM/ZFS backend, Helm)과의 상호작용을 검증한다.
mock이 아닌 실제 시스템을 사용하므로, 기술 고유의 동작과 에러를 잡을 수 있다.

**하위 카테고리:**

| 카테고리 | 경계 | 빌드 태그 | CI |
|---------|------|----------|-----|
| envtest | 실제 kube-apiserver (envtest) | `integration` | ✅ |
| backend | 실제 LVM VG (loopback device) | `e2e` | ⚠️ Kind 필요 |
| helm | Helm → Kind 클러스터 | `e2e` | ⚠️ Kind 필요 |

---

## 카테고리: Envtest (실제 K8s API 서버)

> **경계:** envtest가 구동하는 실제 kube-apiserver + etcd 바이너리.
> fake client와 달리 CRD OpenAPI 스키마 검증, admission webhook 호출,
> RBAC 평가가 실제로 수행된다. Docker/Kind 불필요.

---

### E19: PillarTarget CRD 라이프사이클 (19 TCs)

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarTarget'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarTarget'
```

**목적:**
PillarTarget CRD의 전체 라이프사이클을 검증한다. 이 CRD는 스토리지 에이전트(pillar-agent)가
실행 중인 노드 또는 외부 주소를 식별하는 클러스터-스코프 리소스이다. 다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.nodeRef` / `spec.external` 판별 유니온(discriminated union) 검증
2. **상태 조건 전이** — `NodeExists`, `AgentConnected`, `Ready` 조건의 정확한 설정
3. **삭제 보호 동작** — PillarPool이 참조하는 동안 파이널라이저가 삭제를 차단

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `TgtCRD` | `api/v1alpha1.PillarTarget` CRD 및 상태 |
| `TgtCtrl` | `internal/controller.PillarTargetReconciler` |
| `TgtWH` | `internal/webhook/v1alpha1.PillarTargetCustomValidator` |
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD |
| `MockDialer` | `internal/controller.mockDialer` (테스트 더블) |

---

#### E19.1 유효한 스펙으로 생성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.1.1 | `TestPillarTargetWebhook_ValidCreate_External` | `spec.external.address` + `spec.external.port`가 모두 설정된 external 스펙으로 ValidateCreate 통과 | envtest API 서버; PillarTarget CRD 설치; `PillarTargetCustomValidator` 인스턴스 생성 | 1) `spec.external.address="10.0.0.1"`, `spec.external.port=9500`으로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `TgtWH` |
| E19.1.2 | `TestPillarTargetWebhook_ValidCreate_NodeRef` | `spec.nodeRef.name`만 설정된 nodeRef 스펙으로 ValidateCreate 통과 | envtest API 서버; PillarTarget CRD 설치; `PillarTargetCustomValidator` 인스턴스 생성 | 1) `spec.nodeRef.name="worker-1"`으로 `validator.ValidateCreate(ctx, obj)` 호출 | `warnings=nil`; `err=nil`; 허용 | `TgtWH` |
| E19.1.3 | `TestPillarTargetController_FinalizerAddedOnFirstReconcile` | PillarTarget 생성 후 첫 번째 `Reconcile` 호출에서 `pillar-target-protection` 파이널라이저 자동 추가 | envtest API 서버; `PillarTargetReconciler` 초기화 (`Dialer=nil`); `spec.external` PillarTarget 생성 | 1) `k8sClient.Create(ctx, target)` 실행; 2) `reconciler.Reconcile(ctx, req)` 1회 호출 | PillarTarget에 `pillar-csi.bhyoo.com/pillar-target-protection` 파이널라이저 존재; `result.RequeueAfter==0` | `TgtCRD`, `TgtCtrl` |

---

#### E19.2 잘못된 스펙으로 생성 거부 — CRD 스키마 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.2.1 | `TestPillarTargetCRD_InvalidCreate_EmptyNodeRefName` | `spec.nodeRef.name`이 빈 문자열인 경우 API 서버가 HTTP 422로 거부 | envtest API 서버; PillarTarget CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.nodeRef.name=""`으로 `k8sClient.Create(ctx, target)` 호출 | `k8sClient.Create` 오류 반환; HTTP 422 UnprocessableEntity; `spec.nodeRef.name` 필드 검증 실패 메시지 포함 | `TgtCRD` |
| E19.2.2 | `TestPillarTargetCRD_InvalidCreate_ExternalPortTooLow` | `spec.external.port=0` (최솟값 미달) 시 API 서버가 거부 | envtest API 서버; PillarTarget CRD 설치 (`Minimum=1` 마커 포함) | 1) `spec.external.address="10.0.0.1"`, `spec.external.port=0`으로 Create 호출 | 오류 반환; `spec.external.port` 값 범위 검증 실패 | `TgtCRD` |
| E19.2.3 | `TestPillarTargetCRD_InvalidCreate_ExternalPortTooHigh` | `spec.external.port=65536` (최댓값 초과) 시 API 서버가 거부 | envtest API 서버; PillarTarget CRD 설치 (`Maximum=65535` 마커 포함) | 1) `spec.external.port=65536`으로 Create 호출 | 오류 반환; `spec.external.port` 값 범위 검증 실패 | `TgtCRD` |
| E19.2.4 | `TestPillarTargetCRD_InvalidCreate_EmptyExternalAddress` | `spec.external.address`가 빈 문자열인 경우 거부 | envtest API 서버; PillarTarget CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.external.address=""`으로 Create 호출 | 오류 반환; `spec.external.address` 필드 검증 실패 | `TgtCRD` |

---

#### E19.3 불변 필드 업데이트 거부 — 웹훅 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.3.1 | `TestPillarTargetWebhook_ImmutableUpdate_NodeRefToExternal` | `spec.nodeRef` → `spec.external`로 판별자 전환 시 거부 | `oldObj.spec.nodeRef.name="node-1"`; `newObj.spec.external.address="10.0.0.1"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; 오류 메시지에 `"cannot switch between nodeRef and external"` 포함; `field.Forbidden` 타입; `spec` 경로 | `TgtWH` |
| E19.3.2 | `TestPillarTargetWebhook_ImmutableUpdate_ExternalToNodeRef` | `spec.external` → `spec.nodeRef`로 역전환 시 거부 | `oldObj.spec.external.address="10.0.0.1"`; `newObj.spec.nodeRef.name="node-1"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec` 경로 | `TgtWH` |
| E19.3.3 | `TestPillarTargetWebhook_ImmutableUpdate_NodeRefNameChange` | `spec.nodeRef.name` 변경 시 거부 | `oldObj.spec.nodeRef.name="node-1"`; `newObj.spec.nodeRef.name="node-2"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.nodeRef.name` 경로; 오류 메시지에 이전값 `"node-1"`과 신규값 `"node-2"` 모두 포함 | `TgtWH` |
| E19.3.4 | `TestPillarTargetWebhook_ImmutableUpdate_ExternalAddressChange` | `spec.external.address` 변경 시 거부 | `oldObj.spec.external.address="10.0.0.1"`; `newObj.spec.external.address="10.0.0.2"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.external.address` 경로 | `TgtWH` |
| E19.3.5 | `TestPillarTargetWebhook_ImmutableUpdate_ExternalPortChange` | `spec.external.port` 변경 시 거부 | `oldObj.spec.external.port=9500`; `newObj.spec.external.port=9501` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.external.port` 경로 | `TgtWH` |
| E19.3.6 | `TestPillarTargetWebhook_MutableUpdate_AddressTypeChange` | `spec.nodeRef.addressType` 변경은 허용 (식별 필드 아님) | `oldObj.spec.nodeRef.name="node-1"`, `addressType="InternalIP"`; `newObj.spec.nodeRef.name="node-1"`, `addressType="ExternalIP"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err=nil`; `warnings=nil`; 허용 | `TgtWH` |

---

#### E19.4 상태 조건 전이 — NodeExists

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.4.1 | `TestPillarTargetController_NodeExists_Unknown_ExternalMode` | external 모드 PillarTarget에서 `NodeExists=Unknown/ExternalMode` 설정 | envtest; external PillarTarget 생성; 파이널라이저 추가 조정 1회 완료 | 1) 두 번째 `reconciler.Reconcile(ctx, req)` 호출 | `NodeExists.Status=Unknown`; `Reason="ExternalMode"` | `TgtCRD`, `TgtCtrl` |
| E19.4.2 | `TestPillarTargetController_NodeExists_True_NodePresent` | nodeRef 모드에서 참조된 K8s Node가 존재하면 `NodeExists=True/NodeFound` | envtest; `spec.nodeRef.name="worker-1"` PillarTarget; `worker-1` Node 오브젝트 사전 생성 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `NodeExists.Status=True`; `Reason="NodeFound"` | `TgtCRD`, `TgtCtrl` |
| E19.4.3 | `TestPillarTargetController_NodeExists_False_NodeMissing` | nodeRef 모드에서 참조된 K8s Node가 없으면 `NodeExists=False/NodeNotFound` | envtest; `spec.nodeRef.name="missing-node"` PillarTarget; `missing-node` Node 오브젝트 없음 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `NodeExists.Status=False`; `Reason="NodeNotFound"`; `Message`에 `"missing-node"` 포함 | `TgtCRD`, `TgtCtrl` |

---

#### E19.5 상태 조건 전이 — AgentConnected

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.5.1 | `TestPillarTargetController_AgentConnected_False_DialerNil` | `reconciler.Dialer=nil`이면 `AgentConnected=False/DialerNotConfigured` | envtest; external PillarTarget; `reconciler.Dialer=nil` | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `AgentConnected.Status=False`; `Reason="DialerNotConfigured"` | `TgtCRD`, `TgtCtrl` |
| E19.5.2 | `TestPillarTargetController_AgentConnected_True_PlainTCP` | `mockDialer{healthy:true, mtls:false}` → `AgentConnected=True/Dialed` | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{healthy:true, mtls:false}` | 1) 파이널라이저 조정; 2) mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=True`; `Reason="Dialed"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.5.3 | `TestPillarTargetController_AgentConnected_True_MTLS` | `mockDialer{healthy:true, mtls:true}` → `AgentConnected=True/Authenticated` | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{healthy:true, mtls:true}` | 1) 파이널라이저 조정; 2) mTLS mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=True`; `Reason="Authenticated"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.5.4 | `TestPillarTargetController_AgentConnected_False_HealthCheckError` | `mockDialer.err != nil` → `AgentConnected=False/HealthCheckFailed` | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{err:errors.New("connection refused")}` | 1) 파이널라이저 조정; 2) 오류 반환 mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=False`; `Reason="HealthCheckFailed"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.5.5 | `TestPillarTargetController_AgentConnected_False_AgentUnhealthy` | `mockDialer{healthy:false}` → `AgentConnected=False/AgentUnhealthy` | envtest; external PillarTarget; `reconciler.Dialer = &mockDialer{healthy:false}` | 1) 파이널라이저 조정; 2) 비정상 응답 mockDialer 설정 후 일반 조정 실행 | `AgentConnected.Status=False`; `Reason="AgentUnhealthy"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |

---

#### E19.6 상태 조건 전이 — Ready 종합

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.6.1 | `TestPillarTargetController_Ready_True_AllConditionsMet` | `NodeExists=True` + `AgentConnected=True` → `Ready=True/AllConditionsMet` | envtest; nodeRef PillarTarget; 해당 Node 오브젝트 존재; `mockDialer{healthy:true}` 설정 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `Ready.Status=True`; `Reason="AllConditionsMet"` | `TgtCRD`, `TgtCtrl`, `MockDialer` |
| E19.6.2 | `TestPillarTargetController_Ready_False_NodeMissing` | `NodeExists=False` → `Ready=False` | envtest; nodeRef PillarTarget; 해당 Node 없음 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `Ready.Status=False`; `NodeExists.Status=False` | `TgtCRD`, `TgtCtrl` |
| E19.6.3 | `TestPillarTargetController_Ready_False_AgentUnreachable` | `AgentConnected=False` → `Ready=False` | envtest; external PillarTarget; `mockDialer{err:errors.New("timeout")}` | 1) 파이널라이저 조정; 2) 오류 반환 mockDialer 설정 후 일반 조정 실행 | `Ready.Status=False`; `AgentConnected.Status=False`도 설정됨 | `TgtCRD`, `TgtCtrl`, `MockDialer` |

---

#### E19.7 삭제 보호 동작

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E19.7.1 | `TestPillarTargetController_DeletionBlocked_ReferencingPoolExists` | 참조 PillarPool이 존재하는 동안 삭제 차단; `result.RequeueAfter=10s` | envtest; PillarTarget + 파이널라이저; `spec.targetRef=<target>` PillarPool 존재 | 1) `k8sClient.Delete(ctx, target)` 호출; 2) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=10s`; 파이널라이저 여전히 존재; `k8sClient.Get` 성공 (오브젝트 미삭제) | `TgtCRD`, `TgtCtrl`, `PoolCRD` |
| E19.7.2 | `TestPillarTargetController_DeletionAllowed_NoReferencingPools` | 참조 PillarPool 없을 때 즉시 파이널라이저 제거 및 삭제 진행 | envtest; PillarTarget + 파이널라이저; 참조 PillarPool 없음; 삭제 요청 완료 | 1) `reconciler.Reconcile(ctx, req)` 호출 | `result.RequeueAfter=0`; 파이널라이저 제거; `k8sClient.Get` → NotFound | `TgtCRD`, `TgtCtrl` |
| E19.7.3 | `TestPillarTargetController_DeletionAllowed_AfterPoolRemoval` | 참조 PillarPool 제거 후 다음 조정에서 파이널라이저 제거 및 삭제 완료 | envtest; PillarTarget + 파이널라이저; 삭제 요청 후 첫 조정에서 차단 확인; 이후 참조 PillarPool 삭제 | 1) 첫 조정: `RequeueAfter=10s` 확인; 2) 참조 PillarPool 삭제; 3) 두 번째 조정 실행 | 두 번째 조정 후 파이널라이저 제거; `k8sClient.Get` → NotFound | `TgtCRD`, `TgtCtrl`, `PoolCRD` |
| E19.7.4 | `TestPillarTargetController_DeletionBlocked_MultiplePoolsRequireAllRemoved` | 여러 PillarPool이 동일 PillarTarget을 참조할 때, 하나라도 남아 있으면 차단 지속 | envtest; PillarTarget; PillarPool A, B 모두 `targetRef=<target>`; 삭제 요청 | 1) 첫 조정: 차단; 2) PillarPool A 삭제; 3) 두 번째 조정: 여전히 차단; 4) PillarPool B 삭제; 5) 세 번째 조정 | 첫 두 조정에서 `RequeueAfter=10s`; 세 번째 조정 후 파이널라이저 제거; 오브젝트 삭제 | `TgtCRD`, `TgtCtrl`, `PoolCRD` |

---

### E20: PillarPool CRD 라이프사이클 (20 TCs)

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarPool'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarPool'
```

**목적:**
PillarPool CRD의 전체 라이프사이클을 검증한다. 다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.targetRef`, `spec.backend.type` 필드 검증
2. **상태 조건 전이** — `TargetReady`, `PoolDiscovered`, `BackendSupported`, `Ready` 조건의 정확한 전이
3. **용량 동기화** — PillarTarget의 `DiscoveredPools`에서 `status.capacity`로의 자동 동기화
4. **삭제 보호 동작** — PillarBinding이 참조하는 동안 파이널라이저가 삭제를 차단

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD 및 상태 |
| `PoolCtrl` | `internal/controller.PillarPoolReconciler` |
| `PoolWH` | `internal/webhook/v1alpha1.PillarPoolCustomValidator` |
| `TgtCRD` | `api/v1alpha1.PillarTarget` CRD 및 상태 |
| `BindCRD` | `api/v1alpha1.PillarBinding` CRD |

---

#### E20.1 유효한 스펙으로 생성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.1.1 | `TestPillarPoolWebhook_ValidCreate_ZFSZvol` | `backend.type="zfs-zvol"` + ZFS 설정으로 ValidateCreate 통과 | envtest; PillarPool CRD 설치; `PillarPoolCustomValidator` 인스턴스 생성 | 1) `spec.targetRef="target-a"`, `spec.backend.type="zfs-zvol"`, `spec.backend.zfs.pool="hot-data"`로 `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `PoolWH` |
| E20.1.2 | `TestPillarPoolWebhook_ValidCreate_Dir` | `backend.type="dir"`로 ValidateCreate 통과 | envtest; PillarPool CRD 설치; `PillarPoolCustomValidator` 인스턴스 생성 | 1) `spec.targetRef="target-a"`, `spec.backend.type="dir"`로 `validator.ValidateCreate(ctx, obj)` 호출 | `err=nil`; 허용 | `PoolWH` |
| E20.1.3 | `TestPillarPoolController_FinalizerAddedOnFirstReconcile` | PillarPool 생성 후 첫 번째 `Reconcile` 호출에서 `pool-protection` 파이널라이저 자동 추가 | envtest; `PillarPoolReconciler` 초기화; zfs-zvol 스펙으로 PillarPool 생성 | 1) `k8sClient.Create(ctx, pool)` 실행; 2) `reconciler.Reconcile(ctx, req)` 1회 호출 | PillarPool에 `pillar-csi.bhyoo.com/pool-protection` 파이널라이저 존재; `result.RequeueAfter==0` | `PoolCRD`, `PoolCtrl` |
| E20.1.4 | `TestPillarPoolController_FinalizerNotDuplicated` | 동일 PillarPool을 두 번 조정해도 파이널라이저 중복 없음 | envtest; PillarPool 생성; 첫 조정으로 파이널라이저 추가 완료 | 1) 두 번째 `reconciler.Reconcile(ctx, req)` 호출 | 파이널라이저 개수 정확히 1개; 중복 없음 | `PoolCRD`, `PoolCtrl` |

---

#### E20.2 잘못된 스펙으로 생성 거부 — CRD 스키마 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.2.1 | `TestPillarPoolCRD_InvalidCreate_EmptyTargetRef` | `spec.targetRef`가 빈 문자열인 경우 API 서버가 거부 | envtest; PillarPool CRD 설치 (`MinLength=1` 마커 포함) | 1) `spec.targetRef=""`로 `k8sClient.Create(ctx, pool)` 호출 | 오류 반환; HTTP 422; `spec.targetRef` 필드 검증 실패 | `PoolCRD` |
| E20.2.2 | `TestPillarPoolCRD_InvalidCreate_InvalidBackendType` | `spec.backend.type`에 열거형 외 값 설정 시 거부 | envtest; PillarPool CRD 설치 (`Enum=zfs-zvol;zfs-dataset;lvm-lv;dir` 마커 포함) | 1) `spec.backend.type="unknown-backend"`로 Create 호출 | 오류 반환; HTTP 422; `spec.backend.type` 열거형 검증 실패 | `PoolCRD` |
| E20.2.3 | `TestPillarPoolCRD_InvalidCreate_EmptyBackendType` | `spec.backend.type`이 빈 문자열인 경우 거부 | envtest; PillarPool CRD 설치 | 1) `spec.backend.type=""`로 Create 호출 | 오류 반환; `spec.backend.type` 필수 필드 오류 | `PoolCRD` |

---

#### E20.3 불변 필드 업데이트 거부 — 웹훅 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.3.1 | `TestPillarPoolWebhook_ImmutableUpdate_TargetRefChange` | `spec.targetRef` 변경 시 거부 | `oldObj.spec.targetRef="target-a"`; `newObj.spec.targetRef="target-b"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.targetRef` 경로 | `PoolWH` |
| E20.3.2 | `TestPillarPoolWebhook_ImmutableUpdate_BackendTypeChange` | `spec.backend.type` 변경 시 거부 | `oldObj.spec.backend.type="zfs-zvol"`; `newObj.spec.backend.type="lvm-lv"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.backend.type` 경로 | `PoolWH` |
| E20.3.3 | `TestPillarPoolWebhook_ImmutableUpdate_BothFieldsChange` | `spec.targetRef`와 `spec.backend.type` 동시 변경 시 두 오류 모두 반환 | `oldObj.spec.targetRef="target-a"`, `backend.type="zfs-zvol"`; `newObj.spec.targetRef="target-b"`, `backend.type="lvm-lv"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; 오류 집계에 2개 오류; `spec.targetRef`와 `spec.backend.type` 모두 Forbidden | `PoolWH` |
| E20.3.4 | `TestPillarPoolWebhook_MutableUpdate_ZFSPropertiesChange` | `spec.backend.zfs.properties` 변경은 허용 (불변 필드 아님) | `oldObj.spec.backend.type="zfs-zvol"`, `properties={"compression":"off"}`; `newObj.properties={"compression":"lz4"}` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err=nil`; 허용 | `PoolWH` |

---

#### E20.4 상태 조건 전이 — TargetReady

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E20.4.1 | `TestPillarPoolController_TargetReady_False_TargetAbsent` | 참조 PillarTarget이 없으면 `TargetReady=False/TargetNotFound` | envtest; PillarPool 생성 (`targetRef="nonexistent"`); 해당 PillarTarget 없음 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `TargetReady.Status=False`; `Reason="TargetNotFound"`; 하위 조건 모두 False; `Ready.Status=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.4.2 | `TestPillarPoolController_TargetReady_False_TargetNotReady` | 참조 PillarTarget이 존재하지만 `Ready=False`이면 `TargetReady=False/TargetNotReady` | envtest; PillarPool 생성; PillarTarget 존재하되 `Ready.Status=False`로 상태 패치 | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `TargetReady.Status=False`; `Reason="TargetNotReady"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E20.4.3 | `TestPillarPoolController_TargetReady_True_TargetReady` | 참조 PillarTarget이 `Ready=True`이면 `TargetReady=True` | envtest; PillarPool 생성; PillarTarget `Ready.Status=True` | 1) 파이널라이저 조정; 2) 일반 조정 실행 | `TargetReady.Status=True`; `Reason="TargetReady"` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |

---

#### E20.5-E20.10 (나머지 소섹션)

**E20.5 PoolDiscovered** (4 TCs): E20.5.1-E20.5.4 -- discoveredPools 이름 매칭 검증
**E20.6 BackendSupported** (3 TCs): E20.6.1-E20.6.3 -- capabilities.backends 목록 검증
**E20.7 Ready 종합** (3 TCs): E20.7.1-E20.7.3 -- 하위 조건 논리적 AND 종합
**E20.8 용량 동기화** (3 TCs): E20.8.1-E20.8.3 -- DiscoveredPool → status.capacity 동기화, Used 클램핑
**E20.9 삭제 보호** (4 TCs): E20.9.1-E20.9.4 -- PillarBinding 참조 시 삭제 차단
**E20.10 PillarTarget 상태 변경 시 재조정** (2 TCs): E20.10.1-E20.10.2 -- Ready 전이 전파

> 상세 테이블은 원본 E2E-TESTCASES.md E20.5-E20.10 참조.

---

### E23: PillarProtocol CRD 라이프사이클 (24 TCs)

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarProtocol'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarProtocol'
```

**목적:**
PillarProtocol CRD의 전체 라이프사이클을 검증한다. 다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.type` 열거형 검증 및 프로토콜별 포트 범위 검증
2. **불변 필드 업데이트 거부** — `spec.type`은 생성 후 변경 불가 (웹훅 검증)
3. **상태 조건 전이** — `Ready` 조건, `BindingCount`, `ActiveTargets` 상태 필드의 정확한 전이
4. **삭제 보호 동작** — PillarBinding이 참조하는 동안 파이널라이저가 삭제를 차단

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `PProtCRD` | `api/v1alpha1.PillarProtocol` CRD 및 상태 |
| `PProtCtrl` | `internal/controller.PillarProtocolReconciler` |
| `PProtWH` | `internal/webhook/v1alpha1.PillarProtocolCustomValidator` |
| `BindCRD` | `api/v1alpha1.PillarBinding` CRD |
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD |

---

#### E23.1 유효한 스펙으로 생성

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.1.1 | `TestPillarProtocolWebhook_ValidCreate_NVMeOFTCP` | `spec.type="nvmeof-tcp"` 스펙으로 ValidateCreate 통과 | envtest API 서버; PillarProtocol CRD 설치 | 1) `spec.type="nvmeof-tcp"`, `spec.nvmeofTcp.port=4420`으로 호출 | `err=nil`; 허용 | `PProtWH` |
| E23.1.2 | `TestPillarProtocolWebhook_ValidCreate_ISCSI` | `spec.type="iscsi"` 스펙으로 ValidateCreate 통과 | envtest API 서버 | 1) `spec.type="iscsi"`, `spec.iscsi.port=3260`으로 호출 | `err=nil`; 허용 | `PProtWH` |
| E23.1.3 | `TestPillarProtocolWebhook_ValidCreate_NFS` | `spec.type="nfs"` 스펙으로 ValidateCreate 통과 | envtest API 서버 | 1) `spec.type="nfs"`, `spec.nfs.version="4.2"`으로 호출 | `err=nil`; 허용 | `PProtWH` |
| E23.1.4 | `TestPillarProtocolController_FinalizerAddedOnFirstReconcile` | 생성 후 첫 Reconcile에서 `protocol-protection` 파이널라이저 추가 | envtest; `spec.type="nvmeof-tcp"` PillarProtocol 생성 | 1) Create; 2) Reconcile 1회 | 파이널라이저 존재; `result.RequeueAfter==0` | `PProtCRD`, `PProtCtrl` |
| E23.1.5 | `TestPillarProtocolController_FinalizerNotDuplicated` | 두 번 조정해도 파이널라이저 중복 없음 | envtest; 첫 조정 완료 | 1) 두 번째 Reconcile | 파이널라이저 1개 | `PProtCRD`, `PProtCtrl` |

---

#### E23.2 잘못된 스펙으로 생성 거부 — CRD 스키마 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.2.1 | `TestPillarProtocolCRD_InvalidCreate_UnknownType` | `spec.type`에 열거형 외 값 설정 시 API 서버가 HTTP 422로 거부 | envtest; PillarProtocol CRD 설치 | 1) `spec.type="unknown-protocol"`으로 Create | 오류 반환; HTTP 422; `spec.type` 열거형 검증 실패 | `PProtCRD` |
| E23.2.2 | `TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooLow` | `spec.nvmeofTcp.port=0` 시 거부 | envtest | 1) `spec.nvmeofTcp.port=0`으로 Create | 오류 반환; 포트 범위 검증 실패 | `PProtCRD` |
| E23.2.3 | `TestPillarProtocolCRD_InvalidCreate_NVMeOFTCPPortTooHigh` | `spec.nvmeofTcp.port=65536` 시 거부 | envtest | 1) `spec.nvmeofTcp.port=65536`으로 Create | 오류 반환; 포트 범위 검증 실패 | `PProtCRD` |
| E23.2.4 | `TestPillarProtocolCRD_InvalidCreate_InvalidFSType` | `spec.fsType`에 허용 외 값 설정 시 거부 | envtest | 1) `spec.fsType="btrfs"`으로 Create | 오류 반환; HTTP 422; `spec.fsType` 열거형 검증 실패 | `PProtCRD` |

---

#### E23.3 불변 필드 업데이트 거부 — 웹훅 검증

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.3.1 | `TestPillarProtocolWebhook_ImmutableUpdate_TypeChange_NVMeToISCSI` | `spec.type="nvmeof-tcp"` → `"iscsi"` 변경 시 거부 | `oldObj.spec.type="nvmeof-tcp"`; `newObj.spec.type="iscsi"` | 1) `validator.ValidateUpdate(ctx, oldObj, newObj)` 호출 | `err != nil`; `field.Forbidden`; `spec.type` 경로 | `PProtWH` |
| E23.3.2 | `TestPillarProtocolWebhook_ImmutableUpdate_TypeChange_ISCSIToNFS` | `spec.type="iscsi"` → `"nfs"` 변경 시 거부 | `oldObj.spec.type="iscsi"`; `newObj.spec.type="nfs"` | 1) ValidateUpdate 호출 | `err != nil`; `field.Forbidden` | `PProtWH` |
| E23.3.3 | `TestPillarProtocolWebhook_MutableUpdate_PortChange` | `spec.nvmeofTcp.port` 변경은 허용 | `oldObj.port=4420`; `newObj.port=4421` | 1) ValidateUpdate 호출 | `err=nil`; 허용 | `PProtWH` |

---

#### E23.4 상태 조건 전이 — Ready 조건

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.4.1 | `TestPillarProtocolController_Ready_True_NoBindings` | 참조 PillarBinding 없는 정상 조정에서 `Ready=True/ProtocolConfigured` | envtest; PillarProtocol; 파이널라이저 추가 완료 | 1) Reconcile | `Ready.Status=True`; `Ready.Reason="ProtocolConfigured"` | `PProtCRD`, `PProtCtrl` |
| E23.4.2 | `TestPillarProtocolController_Ready_True_WithBindings` | 참조 PillarBinding이 존재하는 정상 조정에서도 `Ready=True` | envtest; PillarProtocol + PillarBinding | 1) Reconcile | `Ready.Status=True` | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.4.3 | `TestPillarProtocolController_Ready_Message_ContainsType` | `Ready` 조건 메시지에 `spec.type` 값이 포함됨 | envtest; `spec.type="nvmeof-tcp"` | 1) Reconcile | `Ready.Message`에 `"nvmeof-tcp"` 포함 | `PProtCRD`, `PProtCtrl` |
| E23.4.4 | `TestPillarProtocolController_Ready_False_DeletionBlocked` | 삭제 요청 중 참조 PillarBinding 존재 시 `Ready=False/DeletionBlocked` | envtest; 삭제 요청; 참조 PillarBinding 존재 | 1) Delete; 2) Reconcile | `Ready.Status=False`; `Ready.Reason="DeletionBlocked"` | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.4.5 | `TestPillarProtocolController_NoRequeue_WhenReady` | 정상 상태에서 `result.RequeueAfter==0` | envtest | 1) Reconcile | `result.RequeueAfter==0` | `PProtCRD`, `PProtCtrl` |

---

#### E23.5 상태 필드 — BindingCount 및 ActiveTargets

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.5.1 | `TestPillarProtocolController_BindingCount_Zero_NoBindings` | 참조 PillarBinding 없을 때 `BindingCount=0`, `ActiveTargets=[]` | envtest | 1) Reconcile | `status.bindingCount=0`; `status.activeTargets=[]` | `PProtCRD`, `PProtCtrl` |
| E23.5.2 | `TestPillarProtocolController_BindingCount_One_SingleBinding` | 참조 PillarBinding 1개 시 `BindingCount=1` | envtest; PillarProtocol + PillarPool + PillarBinding | 1) Reconcile | `status.bindingCount=1` | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.5.3 | `TestPillarProtocolController_ActiveTargets_PopulatedFromPool` | PillarBinding → PillarPool → targetRef 체인으로 `ActiveTargets` 자동 계산 | envtest; PillarPool(`targetRef="node-1"`) | 1) Reconcile | `status.activeTargets=["node-1"]` | `PProtCRD`, `PProtCtrl`, `BindCRD`, `PoolCRD` |
| E23.5.4 | `TestPillarProtocolController_ActiveTargets_DeduplicatedSorted` | 동일 풀 참조 시 `ActiveTargets`에 중복 없이 정렬 | envtest; 동일 풀 참조하는 바인딩 2개 | 1) Reconcile | `status.activeTargets=["node-1"]` (중복 없음) | `PProtCRD`, `PProtCtrl`, `BindCRD`, `PoolCRD` |
| E23.5.5 | `TestPillarProtocolController_ActiveTargets_EmptyWhenPoolNotFound` | Pool 미존재 시 `ActiveTargets=[]` (우아한 저하) | envtest; PillarBinding 존재; PillarPool 없음 | 1) Reconcile | `status.bindingCount=1`; `status.activeTargets=[]` | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.5.6 | `TestPillarProtocolController_BindingCount_Decremented_AfterBindingRemoval` | PillarBinding 삭제 후 `BindingCount` 감소 | envtest; 초기 BindingCount=1; PillarBinding 삭제 | 1) Delete binding; 2) Reconcile | `status.bindingCount=0` | `PProtCRD`, `PProtCtrl`, `BindCRD` |

---

#### E23.6 삭제 보호 동작

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E23.6.1 | `TestPillarProtocolController_DeletionBlocked_ReferencingBindingExists` | 참조 PillarBinding 존재 시 삭제 차단 | envtest; 파이널라이저; 참조 PillarBinding; 삭제 요청 | 1) Delete; 2) Reconcile | `result.RequeueAfter=10s`; 파이널라이저 유지 | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.6.2 | `TestPillarProtocolController_DeletionBlocked_StatusUpdated` | 삭제 차단 시 `Ready=False/DeletionBlocked` | envtest | 1) Reconcile | `Ready.Status=False`; 메시지에 바인딩 이름 포함 | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.6.3 | `TestPillarProtocolController_DeletionBlocked_FinalizerKept` | 삭제 차단 중 파이널라이저 유지 | envtest | 1) Reconcile | `ContainsFinalizer=true` | `PProtCRD`, `PProtCtrl` |
| E23.6.4 | `TestPillarProtocolController_DeletionAllowed_NoReferencingBindings` | 참조 없을 때 즉시 삭제 | envtest; 참조 PillarBinding 없음 | 1) Reconcile | NotFound; 파이널라이저 제거 | `PProtCRD`, `PProtCtrl` |
| E23.6.5 | `TestPillarProtocolController_DeletionAllowed_AfterBindingRemoval` | 참조 제거 후 삭제 완료 | envtest; 첫 조정 차단 후 바인딩 삭제 | 1) 차단; 2) 바인딩 삭제; 3) 재조정 | NotFound | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E23.6.6 | `TestPillarProtocolController_DeletionBlocked_MultipleBindingsAllNamed` | 여러 바인딩 참조 시 메시지에 모든 이름 나열 | envtest; binding-a, binding-b 참조 | 1) Reconcile | 메시지에 두 이름 모두 포함 | `PProtCRD`, `PProtCtrl`, `BindCRD` |

---

### E25: PillarBinding CRD 라이프사이클 (41 TCs)

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/PillarBinding'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/PillarBinding'
```

**목적:**
PillarBinding CRD의 전체 라이프사이클을 검증한다. 다음 동작을 검증한다:

1. **유효/무효 스펙 생성** — `spec.poolRef`, `spec.protocolRef` 필수 필드 검증
2. **불변 필드 업데이트 거부** — `spec.poolRef`, `spec.protocolRef`는 생성 후 변경 불가
3. **Defaulting 웹훅** — `allowVolumeExpansion` 자동 설정 (백엔드 타입 기반)
4. **백엔드-프로토콜 호환성 웹훅** — 비호환 조합 거부
5. **상태 조건 전이** — `PoolReady`, `ProtocolValid`, `Compatible`, `StorageClassCreated`, `Ready`
6. **StorageClass 라이프사이클** — 소유권, 파라미터, 커스텀 이름, ReclaimPolicy
7. **삭제 보호 동작** — PVC 참조 시 차단

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `BindCRD` | `api/v1alpha1.PillarBinding` CRD 및 상태 |
| `BindCtrl` | `internal/controller.PillarBindingReconciler` |
| `BindWH` | `internal/webhook/v1alpha1.PillarBindingCustomValidator` |
| `BindDef` | `internal/webhook/v1alpha1.PillarBindingCustomDefaulter` |
| `PoolCRD` | `api/v1alpha1.PillarPool` CRD |
| `PProtCRD` | `api/v1alpha1.PillarProtocol` CRD |
| `SC` | `storage.k8s.io/v1.StorageClass` |

---

#### E25.1 유효한 스펙으로 생성 (3 TCs): E25.1.1-E25.1.3
#### E25.2 잘못된 스펙 생성 거부 (3 TCs): E25.2.1-E25.2.3
#### E25.3 불변 필드 업데이트 거부 (3 TCs): E25.3.1-E25.3.3

---

#### E25.4 Defaulting 웹훅 — allowVolumeExpansion 자동 설정

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.4.1 | `TestPillarBindingDefaulter_AllowVolumeExpansion_True_ZFSZvol` | `backend.type="zfs-zvol"` 풀 참조 시 `allowVolumeExpansion=true` 자동 설정 | envtest; zfs-zvol PillarPool | 1) `defaulter.Default(ctx, obj)` | `allowVolumeExpansion=true` | `BindDef`, `PoolCRD` |
| E25.4.2 | `TestPillarBindingDefaulter_AllowVolumeExpansion_True_LVMLV` | `backend.type="lvm-lv"` 풀 참조 시 `allowVolumeExpansion=true` | envtest; lvm-lv PillarPool | 1) Default | `allowVolumeExpansion=true` | `BindDef`, `PoolCRD` |
| E25.4.3 | `TestPillarBindingDefaulter_AllowVolumeExpansion_False_ZFSDataset` | `backend.type="zfs-dataset"` 시 `allowVolumeExpansion=false` | envtest; zfs-dataset PillarPool | 1) Default | `allowVolumeExpansion=false` | `BindDef`, `PoolCRD` |
| E25.4.4 | `TestPillarBindingDefaulter_AllowVolumeExpansion_False_Dir` | `backend.type="dir"` 시 `allowVolumeExpansion=false` | envtest; dir PillarPool | 1) Default | `allowVolumeExpansion=false` | `BindDef`, `PoolCRD` |
| E25.4.5 | `TestPillarBindingDefaulter_AllowVolumeExpansion_NotOverridden_Explicit` | 명시적 설정 시 Defaulter가 덮어쓰지 않음 | envtest; zfs-zvol PillarPool; 명시 `false` | 1) Default | `allowVolumeExpansion=false` (유지) | `BindDef`, `PoolCRD` |
| E25.4.6 | `TestPillarBindingDefaulter_AllowVolumeExpansion_NilWhenPoolNotFound` | Pool 미존재 시 건너뜀 | envtest; pool 미존재 | 1) Default | `nil`; 오류 없음 | `BindDef` |

---

#### E25.5 백엔드-프로토콜 호환성 웹훅 검증

**호환성 매트릭스:**

| 백엔드 타입 | nvmeof-tcp | iscsi | nfs |
|------------|:---------:|:-----:|:---:|
| zfs-zvol   | ✅ | ✅ | ❌ |
| lvm-lv     | ✅ | ✅ | ❌ |
| zfs-dataset| ❌ | ❌ | ✅ |
| dir        | ❌ | ❌ | ✅ |

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| E25.5.1 | `TestPillarBindingWebhook_Compatible_ZFSZvol_NVMeOFTCP` | zfs-zvol + nvmeof-tcp → 허용 | envtest; 해당 Pool/Protocol | 1) ValidateCreate | `err=nil` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.2 | `TestPillarBindingWebhook_Compatible_LVMLV_ISCSI` | lvm-lv + iscsi → 허용 | envtest | 1) ValidateCreate | `err=nil` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.3 | `TestPillarBindingWebhook_Compatible_ZFSDataset_NFS` | zfs-dataset + nfs → 허용 | envtest | 1) ValidateCreate | `err=nil` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.4 | `TestPillarBindingWebhook_Compatible_Dir_NFS` | dir + nfs → 허용 | envtest | 1) ValidateCreate | `err=nil` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.5 | `TestPillarBindingWebhook_Incompatible_ZFSZvol_NFS` | zfs-zvol + nfs → 거부 | envtest | 1) ValidateCreate | `err != nil`; `"incompatible"` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.6 | `TestPillarBindingWebhook_Incompatible_LVMLV_NFS` | lvm-lv + nfs → 거부 | envtest | 1) ValidateCreate | `err != nil` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.7 | `TestPillarBindingWebhook_Incompatible_ZFSDataset_NVMeOFTCP` | zfs-dataset + nvmeof-tcp → 거부 | envtest | 1) ValidateCreate | `err != nil` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.8 | `TestPillarBindingWebhook_Incompatible_Dir_ISCSI` | dir + iscsi → 거부 | envtest | 1) ValidateCreate | `err != nil` | `BindWH`, `PoolCRD`, `PProtCRD` |
| E25.5.9 | `TestPillarBindingWebhook_CompatibilitySkipped_PoolNotFound` | pool 미존재 시 검사 건너뜀 | envtest; pool 없음 | 1) ValidateCreate | `err=nil` (graceful skip) | `BindWH` |
| E25.5.10 | `TestPillarBindingWebhook_CompatibilitySkipped_ProtocolNotFound` | protocol 미존재 시 검사 건너뜀 | envtest; protocol 없음 | 1) ValidateCreate | `err=nil` (graceful skip) | `BindWH` |

---

#### E25.6-E25.11 (나머지 소섹션)

**E25.6 PoolReady 상태 조건** (3 TCs): E25.6.1-E25.6.3
**E25.7 ProtocolValid 상태 조건** (3 TCs): E25.7.1-E25.7.3
**E25.8 Compatible 및 Ready 종합** (3 TCs): E25.8.1-E25.8.3
**E25.9 StorageClass 생성 및 소유권** (7 TCs): E25.9.1-E25.9.7
**E25.10 StorageClass 커스텀 설정** (2 TCs): E25.10.1-E25.10.2
**E25.11 삭제 보호 동작** (4 TCs): E25.11.1-E25.11.4

> 상세 테이블은 원본 E2E-TESTCASES.md E25.6-E25.11 참조.

---

### E26: 교차-CRD 라이프사이클 상호작용 (23 TCs)

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

**실행 방법:**
```bash
make setup-envtest
go test -tags=integration ./internal/controller/... -v -run 'TestControllers/CrossCRD'
go test -tags=integration ./internal/webhook/... -v -run 'TestWebhooks/CrossCRD'
```

**목적:**
여러 CRD(PillarTarget → PillarPool → PillarBinding ← PillarProtocol)의
**교차-CRD 라이프사이클 상호작용**을 검증한다:

1. **의존 순서** — 참조 CRD가 없거나 Not-Ready일 때 하위 CRD 상태 조건이 False
2. **연쇄 상태 업데이트** — 상위 CRD 상태 변화가 하위 CRD에 전파
3. **삭제 보호** — 의존 리소스가 존재할 때 삭제가 파이널라이저로 차단

**컴포넌트 의존성 그래프:**

```
PillarTarget (pt)
  └─(targetRef)──► PillarPool (pp)
                     └─(poolRef)────► PillarBinding (pb) ──► StorageClass ──► PVC
PillarProtocol (ppr)
  └─(protocolRef)──► PillarBinding (pb)
```

---

#### E26.1 의존 순서 — 참조 CRD 없음/Not-Ready 시 하위 조건 차단 (8 TCs)

| ID | 테스트 함수 | 설명 | 기대 결과 | 커버리지 |
|----|------------|------|----------|---------|
| E26.1.1 | `TestCrossLifecycle_Pool_TargetMissing_TargetReadyFalse` | Target 없으면 Pool `TargetReady=False` | `TargetReady=False`; `Ready=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.1.2 | `TestCrossLifecycle_Pool_TargetNotReady_TargetReadyFalse` | Target Not-Ready이면 Pool `TargetReady=False` | `TargetReady=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.1.3 | `TestCrossLifecycle_Pool_TargetReady_TargetReadyTrue` | Target Ready이면 Pool `TargetReady=True` | `TargetReady=True` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.1.4 | `TestCrossLifecycle_Binding_PoolMissing_PoolReadyFalse` | Pool 없으면 Binding `PoolReady=False` | `PoolReady=False`; StorageClass 미생성 | `BindCRD`, `BindCtrl`, `PoolCRD` |
| E26.1.5 | `TestCrossLifecycle_Binding_PoolNotReady_PoolReadyFalse` | Pool Not-Ready이면 Binding `PoolReady=False` | `PoolReady=False` | `BindCRD`, `BindCtrl`, `PoolCRD` |
| E26.1.6 | `TestCrossLifecycle_Binding_ProtocolMissing_ProtocolValidFalse` | Protocol 없으면 Binding `ProtocolValid=False` | `ProtocolValid=False` | `BindCRD`, `BindCtrl`, `PProtCRD` |
| E26.1.7 | `TestCrossLifecycle_Binding_BothMissing_BothConditionsFalse` | Pool과 Protocol 둘 다 없으면 두 조건 모두 False | 두 조건 모두 False | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD` |
| E26.1.8 | `TestCrossLifecycle_Binding_PoolReadyProtocolReady_BecomeReady` | 둘 다 Ready이면 Binding Ready, StorageClass 생성 | `Ready=True`; StorageClass 존재 | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD`, `SC` |

---

#### E26.2 연쇄 상태 업데이트 (7 TCs)

| ID | 테스트 함수 | 설명 | 기대 결과 | 커버리지 |
|----|------------|------|----------|---------|
| E26.2.1 | `TestCrossLifecycle_Cascade_TargetLosesReady_PoolConditionUpdates` | Target Ready→False 시 Pool TargetReady=False | Pool `Ready=False` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.2.2 | `TestCrossLifecycle_Cascade_TargetRecovery_PoolConditionRestores` | Target False→Ready 시 Pool TargetReady=True 복원 | Pool `Ready=True` | `PoolCRD`, `PoolCtrl`, `TgtCRD` |
| E26.2.3 | `TestCrossLifecycle_Cascade_PoolLosesReady_BindingConditionUpdates` | Pool Ready→False 시 Binding PoolReady=False | Binding `Ready=False`; StorageClass 유지 | `BindCRD`, `BindCtrl`, `PoolCRD`, `SC` |
| E26.2.4 | `TestCrossLifecycle_Cascade_ProtocolBecomesInvalid_BindingNotReady` | Protocol Ready→False 시 Binding ProtocolValid=False | Binding `Ready=False` | `BindCRD`, `BindCtrl`, `PProtCRD` |
| E26.2.5 | `TestCrossLifecycle_Cascade_FullChainRecovery` | Target 회복 시 전체 체인 Ready 복원 | Pool+Binding 모두 `Ready=True` | `PoolCRD`, `PoolCtrl`, `BindCRD`, `BindCtrl`, `TgtCRD` |
| E26.2.6 | `TestCrossLifecycle_Cascade_BindingBecomesReady_StorageClassCreated` | 모든 의존성 Ready 후 StorageClass 생성 | StorageClass 생성; Binding `Ready=True` | `BindCRD`, `BindCtrl`, `PoolCRD`, `PProtCRD`, `SC` |
| E26.2.7 | `TestCrossLifecycle_Cascade_ProtocolBindingCount_IncrementOnCreate` | Binding 조정 완료 시 Protocol bindingCount 증가 | `bindingCount=1` | `PProtCRD`, `PProtCtrl`, `BindCRD` |

---

#### E26.3 삭제 보호 — 의존 리소스 존재 시 삭제 차단 (8 TCs)

| ID | 테스트 함수 | 설명 | 기대 결과 | 커버리지 |
|----|------------|------|----------|---------|
| E26.3.1 | `TestCrossLifecycle_DeleteProtection_Target_BlockedByPool` | Pool이 참조하는 Target 삭제 차단 | 파이널라이저 유지; Target 존재 | `TgtCRD`, `TgtCtrl`, `PoolCRD` |
| E26.3.2 | `TestCrossLifecycle_DeleteProtection_Target_AllowedAfterPoolRemoved` | Pool 삭제 후 Target 삭제 완료 | Target NotFound | `TgtCRD`, `TgtCtrl`, `PoolCRD` |
| E26.3.3 | `TestCrossLifecycle_DeleteProtection_Pool_BlockedByBinding` | Binding이 참조하는 Pool 삭제 차단 | 파이널라이저 유지; Pool 존재 | `PoolCRD`, `PoolCtrl`, `BindCRD` |
| E26.3.4 | `TestCrossLifecycle_DeleteProtection_Pool_AllowedAfterBindingRemoved` | Binding 삭제 후 Pool 삭제 완료 | Pool NotFound | `PoolCRD`, `PoolCtrl`, `BindCRD` |
| E26.3.5 | `TestCrossLifecycle_DeleteProtection_Protocol_BlockedByBinding` | Binding이 참조하는 Protocol 삭제 차단 | 파이널라이저 유지 | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E26.3.6 | `TestCrossLifecycle_DeleteProtection_Protocol_AllowedAfterBindingRemoved` | Binding 삭제 후 Protocol 삭제 완료 | Protocol NotFound | `PProtCRD`, `PProtCtrl`, `BindCRD` |
| E26.3.7 | `TestCrossLifecycle_DeleteProtection_FullChain_ReverseOrderDeletion` | 역순(Binding→Pool→Target) 삭제 시 전체 정상 삭제 | 세 CRD 모두 NotFound | `TgtCRD`, `TgtCtrl`, `PoolCRD`, `PoolCtrl`, `BindCRD`, `BindCtrl` |
| E26.3.8 | `TestCrossLifecycle_DeleteProtection_NoDependent_ImmediateDeletion` | 의존 없는 CRD는 즉시 삭제 | Target NotFound | `TgtCRD`, `TgtCtrl` |

---

### E21.2-E21.4: Webhook/Schema 검증 (20 TCs)

> **경계:** envtest의 실제 kube-apiserver를 통한 CRD OpenAPI 스키마 검증 및
> admission webhook 직접 호출. fake client에서는 수행되지 않는 검증이다.

**빌드 태그:** `//go:build integration`

---

#### E21.2 PillarTarget 웹훅 — 불변 필드 수정 거부 (7 TCs)

**위치:** `internal/webhook/v1alpha1/pillartarget_webhook_test.go`

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 151 | `TestPillarTargetWebhook_Update_DiscriminantSwitch_NodeToExternal` | `spec.nodeRef` → `spec.external` 전환 거부 | validator; oldObj nodeRef; newObj external | 1) ValidateUpdate | `field.Forbidden`; `"cannot switch"` | `Webhook`, `TgtCRD` |
| 152 | `TestPillarTargetWebhook_Update_DiscriminantSwitch_ExternalToNode` | `spec.external` → `spec.nodeRef` 전환 거부 | oldObj external; newObj nodeRef | 1) ValidateUpdate | `field.Forbidden` | `Webhook`, `TgtCRD` |
| 153 | `TestPillarTargetWebhook_Update_NodeRefNameImmutable` | `spec.nodeRef.name` 변경 거부 | oldObj name=node-a; newObj name=node-b | 1) ValidateUpdate | `field.Forbidden(spec.nodeRef.name)` | `Webhook`, `TgtCRD` |
| 154 | `TestPillarTargetWebhook_Update_ExternalAddressImmutable` | `spec.external.address` 변경 거부 | oldObj addr=1.2.3.4; newObj addr=5.6.7.8 | 1) ValidateUpdate | `field.Forbidden(spec.external.address)` | `Webhook`, `TgtCRD` |
| 155 | `TestPillarTargetWebhook_Update_ExternalPortImmutable` | `spec.external.port` 변경 거부 | oldObj port=9500; newObj port=9600 | 1) ValidateUpdate | `field.Forbidden(spec.external.port)` | `Webhook`, `TgtCRD` |
| 156 | `TestPillarTargetWebhook_Update_NodeRefNonIdentityFieldChange_OK` | 비식별 필드(`addressType`) 변경 허용 | name 동일; addressType 변경 | 1) ValidateUpdate | `err=nil` | `Webhook`, `TgtCRD` |
| 157 | `TestPillarTargetWebhook_Create_Valid` | 유효한 생성 시 허용 | 유효한 PillarTarget | 1) ValidateCreate | `err=nil` | `Webhook`, `TgtCRD` |

---

#### E21.3 PillarPool 웹훅 — 불변 필드 수정 거부 (5 TCs)

**위치:** `internal/webhook/v1alpha1/pillarpool_webhook_test.go`

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 158 | `TestPillarPoolWebhook_Update_TargetRefImmutable` | `spec.targetRef` 변경 거부 | oldObj targetRef=target-a; newObj target-b | 1) ValidateUpdate | `field.Forbidden(spec.targetRef)` | `Webhook`, `TgtCRD`, `VolCRD` |
| 159 | `TestPillarPoolWebhook_Update_BackendTypeImmutable` | `spec.backend.type` 변경 거부 | oldObj zfs-zvol; newObj lvm-lv | 1) ValidateUpdate | `field.Forbidden(spec.backend.type)` | `Webhook`, `TgtCRD`, `VolCRD` |
| 160 | `TestPillarPoolWebhook_Update_ZFSPoolChange_OK` | ZFS 풀 이름 변경 허용 | type 동일; zfs.pool 변경 | 1) ValidateUpdate | `err=nil` | `Webhook`, `TgtCRD`, `VolCRD` |
| 161 | `TestPillarPoolWebhook_Update_BothFieldsChanged_MultipleErrors` | targetRef + backend.type 동시 변경 시 두 오류 | 두 필드 모두 변경 | 1) ValidateUpdate | ErrorList 길이=2; 두 필드 모두 Forbidden | `Webhook`, `TgtCRD`, `VolCRD` |
| 162 | `TestPillarPoolWebhook_Create_Valid` | 유효한 생성 시 허용 | 유효한 PillarPool | 1) ValidateCreate | `err=nil` | `Webhook`, `TgtCRD`, `VolCRD` |

---

#### E21.4 CRD OpenAPI 스키마 검증 — 필드 범위/형식 위반 (8 TCs)

**위치:** `internal/controller/pillartarget_controller_test.go` 또는 `internal/controller/pillarpool_controller_test.go`

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 163 | `TestCRDSchema_PillarTarget_NodeRefName_Empty` | `spec.nodeRef.name=""` → MinLength=1 위반 | envtest API 서버 | 1) Create | 422; `spec.nodeRef.name` 오류 | `TgtCRD`, `API서버스키마` |
| 164 | `TestCRDSchema_PillarTarget_ExternalPort_Zero` | `spec.external.port=0` → Minimum=1 위반 | envtest API 서버 | 1) Create | 422; `spec.external.port` 오류 | `TgtCRD`, `API서버스키마` |
| 165 | `TestCRDSchema_PillarTarget_ExternalAddress_Empty` | `spec.external.address=""` → MinLength=1 위반 | envtest API 서버 | 1) Create | 422; `spec.external.address` 오류 | `TgtCRD`, `API서버스키마` |
| 166 | `TestCRDSchema_PillarTarget_NodeRefAddressType_Invalid` | `spec.nodeRef.addressType="FooType"` → Enum 위반 | envtest API 서버 | 1) Create | 422; Enum 위반 오류 | `TgtCRD`, `API서버스키마` |
| 167 | `TestCRDSchema_PillarPool_TargetRef_Empty` | `spec.targetRef=""` → MinLength=1 위반 | envtest API 서버 | 1) Create | 422; `spec.targetRef` 오류 | `TgtCRD`, `VolCRD`, `API서버스키마` |
| 168 | `TestCRDSchema_PillarPool_BackendType_Invalid` | `spec.backend.type="not-supported"` → Enum 위반 | envtest API 서버 | 1) Create | 422; Enum 위반 | `TgtCRD`, `VolCRD`, `API서버스키마` |
| 169 | `TestCRDSchema_PillarVolume_Phase_Invalid` | `status.phase="GarbagePhase"` → Enum 위반 | envtest; 유효한 PillarVolume 존재 | 1) Status Patch | 422; Enum 위반 | `VolCRD`, `API서버스키마` |
| 170 | `TestCRDSchema_PillarVolume_CapacityBytes_Negative` | `spec.capacityBytes=-1` → Minimum=0 위반 | envtest API 서버 | 1) Create | 422; Minimum 위반 | `VolCRD`, `API서버스키마` |

---

### E32: PillarPool/PillarBinding LVM CRD 라이프사이클 (9 TCs)

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

PillarPool과 PillarBinding CRD에서 LVM 고유 필드의 OpenAPI 스키마 검증,
웹훅 유효성 검사, 그리고 Backend-Protocol 호환성 매트릭스 적용을 검증한다.

---

#### E32.1 PillarPool LVM 설정 검증 (5 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 276 | `TestPillarPool_LVM_ValidLinearConfig` | type=lvm-lv, volumeGroup + provisioningMode=linear 유효 | envtest; PillarTarget Ready | 1) PillarPool 생성 | 생성 성공; OpenAPI 통과 | `PoolCRD` |
| 277 | `TestPillarPool_LVM_ValidThinConfig` | type=lvm-lv, volumeGroup + thinPool + thin 유효 | envtest | 1) PillarPool 생성 | 생성 성공 | `PoolCRD` |
| 278 | `TestPillarPool_LVM_MissingVolumeGroup_Rejected` | type=lvm-lv이나 volumeGroup 미지정 시 거부 | envtest | 1) 생성 시도 | minLength=1 위반 | `PoolCRD` |
| 279 | `TestPillarPool_LVM_InvalidProvisioningMode_Rejected` | provisioningMode에 잘못된 값 시 거부 | envtest | 1) 생성 시도 | Enum 검증 오류 | `PoolCRD` |
| 280 | `TestPillarPool_LVM_MissingLVMConfig_Rejected` | type=lvm-lv이나 lvm 섹션 누락 시 웹훅 거부 | envtest | 1) 생성 시도 | ValidationFailed | `PoolCRD` |

---

#### E32.2 PillarBinding LVM 오버라이드 및 호환성 검증 (4 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 281 | `TestPillarBinding_LVM_ValidOverride` | LVM provisioningMode 오버라이드 유효 | envtest; PillarPool(lvm-lv) Ready; PillarProtocol(nvmeof-tcp) | 1) 생성 | 성공; StorageClass 생성 | `BindCRD`, `SC` |
| 282 | `TestPillarBinding_LVM_InvalidOverride_Rejected` | 잘못된 Enum 값 시 거부 | envtest | 1) 생성 시도 | Enum 검증 오류 | `BindCRD` |
| 283 | `TestPillarBinding_LVM_NVMeOFTCP_Compatible` | lvm-lv + nvmeof-tcp 호환 | envtest | 1) 생성; 2) Reconcile | Compatible=True; Ready=True | `BindCRD`, `BindCtrl` |
| 284 | `TestPillarBinding_LVM_NFS_Incompatible` | lvm-lv + nfs 비호환 거부 | envtest | 1) 생성 시도 | 거부 또는 Compatible=False | `BindCRD`, `BindCtrl` |

---

### I-NEW: PRD 갭 — 추가 TC (envtest) (15 TCs)

**테스트 유형:** C (Envtest 통합) ⚠️ envtest 필요

**빌드 태그:** `//go:build integration`

PRD에서 요구하지만 기존 TC에서 누락된 동작을 보완한다.
gRPC 주소 결정, CRD 기본값, 노드 label 관리, StorageClass drift correction,
leader election 등 envtest 경계에서 검증 가능한 항목을 다룬다.

**컴포넌트 약어 참조:**

| 약어 | 의미 |
|------|------|
| `TgtCRD` | `api/v1alpha1.PillarTarget` CRD 및 상태 |
| `TgtCtrl` | `internal/controller.PillarTargetReconciler` |
| `BindCtrl` | `internal/controller.PillarBindingReconciler` |
| `ProtoCRD` | `api/v1alpha1.PillarProtocol` CRD |
| `SC` | StorageClass 자동 생성/관리 |
| `Mgr` | controller-runtime Manager |

---

#### I-NEW-1 gRPC 주소 결정 로직

> **Integration test 근거:** 실제 K8s Node 객체의 status.addresses에서 IP를 선택하는 로직은 envtest에서 실제 Node 객체를 만들어야 검증 가능.

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-1-1 | `TestPillarTargetReconciler_ResolvesInternalIP` | addressType=InternalIP 시 Node의 InternalIP를 resolvedAddress에 기록 | envtest; K8s Node "node-1" (status.addresses: [{InternalIP, "10.0.0.5"}, {ExternalIP, "203.0.113.10"}]); PillarTarget(nodeRef.name="node-1", addressType=InternalIP) 생성 | 1) PillarTarget 생성; 2) reconcile 대기; 3) status.resolvedAddress 확인 | resolvedAddress = "10.0.0.5" | `TgtCtrl`, `TgtCRD` |
| I-NEW-1-2 | `TestPillarTargetReconciler_ResolvesExternalIP` | addressType=ExternalIP 시 Node의 ExternalIP를 선택 | Node에 [{InternalIP, "10.0.0.5"}, {ExternalIP, "203.0.113.10"}]; PillarTarget(addressType=ExternalIP) | 1) 생성; 2) reconcile; 3) 확인 | resolvedAddress = "203.0.113.10" | `TgtCtrl`, `TgtCRD` |
| I-NEW-1-3 | `TestPillarTargetReconciler_CIDRFilterSelectsCorrectIP` | 동일 타입 IP 여러 개 + addressSelector CIDR로 필터 | Node에 [{InternalIP, "10.0.0.5"}, {InternalIP, "192.168.219.6"}]; PillarTarget(addressType=InternalIP, addressSelector="192.168.219.0/24") | 1) 생성; 2) reconcile; 3) 확인 | resolvedAddress = "192.168.219.6" | `TgtCtrl`, `TgtCRD` |
| I-NEW-1-4 | `TestPillarTargetReconciler_DefaultAddressType_InternalIP` | addressType 미지정 시 기본값 InternalIP로 동작 | Node에 [{InternalIP, "10.0.0.5"}, {ExternalIP, "203.0.113.10"}]; PillarTarget(addressType 미지정) | 1) 생성; 2) reconcile; 3) 확인 | resolvedAddress = "10.0.0.5" (InternalIP 선택) | `TgtCtrl`, `TgtCRD` |

---

#### I-NEW-2 addressType 기본값

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-2-1 | `TestPillarTarget_DefaultAddressType` | addressType 필드 미지정 시 API 서버가 기본값 InternalIP 적용 | envtest; PillarTarget(nodeRef.name="n1") 생성 시 addressType 생략 | 1) k8sClient.Create(target); 2) k8sClient.Get → spec.nodeRef.addressType 확인 | addressType = "InternalIP" | `TgtCRD` |

---

#### I-NEW-3 nodeRef.port 오버라이드

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-3-1 | `TestPillarTarget_PortOverride_StoredInSpec` | nodeRef.port=9600 설정 시 CRD에 저장 | envtest; PillarTarget(nodeRef.port=9600) | 1) 생성; 2) Get; 3) spec.nodeRef.port 확인 | port = 9600 | `TgtCRD` |
| I-NEW-3-2 | `TestPillarTarget_PortDefault_9500` | port 미지정 시 기본값 9500 적용 (defaulting webhook 또는 reconciler) | envtest; PillarTarget(port 생략) | 1) 생성; 2) Get 또는 reconciler가 연결 시 사용하는 포트 확인 | 기본 포트 9500 사용 | `TgtCRD`, `TgtCtrl` |

---

#### I-NEW-4 노드 label 자동 관리

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-4-1 | `TestPillarTargetReconciler_AddsStorageNodeLabel` | PillarTarget 생성 시 참조 Node에 storage-node label 자동 추가 | envtest; K8s Node "node-1" (label 없음); PillarTarget(nodeRef.name="node-1") 생성 | 1) PillarTarget 생성; 2) reconcile 대기; 3) Node "node-1"의 labels 확인 | labels["pillar-csi.bhyoo.com/storage-node"] = "true" | `TgtCtrl` |
| I-NEW-4-2 | `TestPillarTargetReconciler_RemovesStorageNodeLabel` | PillarTarget 삭제 시 Node에서 label 제거 | Node에 label 있음; PillarTarget 존재 | 1) PillarTarget 삭제; 2) reconcile 대기; 3) Node labels 확인 | labels["pillar-csi.bhyoo.com/storage-node"] 없음 | `TgtCtrl` |
| I-NEW-4-3 | `TestPillarTargetReconciler_LabelIdempotent` | 이미 label이 있는 Node에 PillarTarget 생성해도 에러 없음 | Node에 이미 label 존재 | 1) PillarTarget 생성; 2) reconcile 대기 | 정상 완료; label 유지; 에러 없음 | `TgtCtrl` |

---

#### I-NEW-5 StorageClass drift correction

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-5-1 | `TestPillarBindingReconciler_SCDriftCorrection` | StorageClass를 직접 수정하면 reconciler가 PillarBinding spec으로 되돌림 | envtest; PillarBinding → SC 자동 생성 완료(reclaimPolicy=Delete) | 1) SC의 reclaimPolicy를 Retain으로 직접 변경; 2) reconcile 대기; 3) SC 재조회 | reclaimPolicy = Delete (원래 값으로 복원) | `BindCtrl`, `SC` |

---

#### I-NEW-6 Binding spec 변경 → SC 업데이트

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-6-1 | `TestPillarBindingReconciler_SpecChange_UpdatesSC` | PillarBinding spec 변경 시 SC parameters 업데이트 | envtest; PillarBinding → SC 존재; overrides.fsType="ext4" | 1) PillarBinding overrides.fsType="xfs"로 업데이트; 2) reconcile 대기; 3) SC parameters 확인 | SC.parameters["fs-type"] = "xfs" | `BindCtrl`, `SC` |

---

#### I-NEW-7 Leader election

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-7-1 | `TestControllerManager_LeaderElection` | --leader-election 활성화 시 Lease 객체 생성 및 단일 리더 동작 | envtest; manager 2개 인스턴스 시작(leaderElection=true) | 1) 두 manager 시작; 2) Lease 객체 조회; 3) 하나만 reconcile 실행 확인 | Lease 객체 존재; holderIdentity가 하나의 manager; 나머지는 대기 | `Mgr` |

---

#### I-NEW-8 PillarProtocol 기본값

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| I-NEW-8-1 | `TestPillarProtocol_NVMeoFDefaults` | NVMe-oF 프로토콜 필드 미지정 시 기본값 적용 | envtest; PillarProtocol(type=nvmeof-tcp, nvmeofTcp.port 미지정, acl 미지정) 생성 | 1) 생성; 2) Get; 3) 필드 값 확인 | nvmeofTcp.port=4420; acl=true | `ProtoCRD` |
| I-NEW-8-2 | `TestPillarProtocol_ISCSIDefaults` | iSCSI 프로토콜 필드 미지정 시 기본값 적용 | envtest; PillarProtocol(type=iscsi, iscsi 필드 미지정) 생성 | 1) 생성; 2) Get | iscsi.port=3260; iscsi.loginTimeout=15; iscsi.replacementTimeout=120; iscsi.nodeSessionTimeout=120 | `ProtoCRD` |

---

## 카테고리: 실제 Backend (loopback device)

> **경계:** 실제 LVM 명령어(`lvcreate`, `lvremove` 등)를 loopback device 위의
> 실제 VG에서 실행한다. mock이 아닌 실제 LVM 바이너리를 호출하므로,
> 명령 출력 파싱, 용량 계산, 이름 검증 등 LVM 고유 동작을 검증할 수 있다.

---

### E28: LVM Agent gRPC E2E 테스트 (30 TCs)

**테스트 유형:** A (인프로세스 E2E) ✅ CI 실행 가능

실제 gRPC 리스너(localhost:0)와 mock LVM backend로 agent.Server의
네트워크 직렬화/역직렬화 레이어를 검증한다. LVM 고유 동작(프로비저닝 모드 전파,
VG 용량 vs thin pool 용량, thin pool LV 필터링, 디바이스 경로 형식)을 중점 검증한다.

**아키텍처:**
```
테스트 프로세스
    │
    ├──► agent.Server (실제 gRPC 리스너, localhost:0)
    │        │
    │        ├──► mockLVMBackend (linear VG 모드)
    │        ├──► mockLVMBackend (thin pool 모드)
    │        └──► NvmetTarget (tmpdir configfs)
    │
    └──► agentv1.AgentServiceClient (gRPC stub)
```

---

#### E28.1 LVM 역량 및 헬스체크 (2 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 244 | `TestAgent_LVM_GetCapabilities` | GetCapabilities 응답에 BACKEND_TYPE_LVM 포함 | mockLVMBackend; gRPC 리스너 | 1) GetCapabilitiesRequest | backends에 LVM 포함 | `Agent`, `LVM`, `gRPC` |
| 245 | `TestAgent_LVM_HealthCheck` | LVM backend 등록 상태에서 HealthCheck 정상 응답 | mockLVMBackend; tmpdir configfs | 1) HealthCheckRequest | 응답 성공 | `Agent`, `LVM`, `gRPC` |

---

#### E28.2 LVM 전체 왕복 (2 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 246 | `TestAgent_LVM_RoundTrip_Linear` | Linear LV 전체 왕복: Create→Export→Allow→Deny→Unexport→Delete | mockLVMBackend(linear); tmpdir configfs | 6단계 | 모든 단계 성공; device_path="/dev/\<vg\>/\<lv\>" | `Agent`, `LVM`, `NVMeF`, `gRPC` |
| 247 | `TestAgent_LVM_RoundTrip_Thin` | Thin LV 전체 왕복 | mockLVMBackend(thin) | 동일 6단계 | 동일 | `Agent`, `LVM`, `NVMeF`, `gRPC` |

---

#### E28.3 LVM GetCapacity (4 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 248 | `TestAgent_LVM_GetCapacity_LinearVG` | Linear VG: vg_size/vg_free 반환 | linear: total=100GiB, avail=60GiB | 1) GetCapacity | Total=100GiB; Available=60GiB | `Agent`, `LVM`, `gRPC` |
| 249 | `TestAgent_LVM_GetCapacity_ThinPool` | Thin pool: data_percent 기반 available | thin: total=200GiB, data_percent=30% | 1) GetCapacity | Available=140GiB | `Agent`, `LVM`, `gRPC` |
| 250 | `TestAgent_LVM_GetCapacity_ThinPoolOverProvisioned` | Over-provisioned thin pool: Available=0 | thin: data_percent=120% | 1) GetCapacity | Available=0 (음수 아님) | `Agent`, `LVM`, `gRPC` |
| 251 | `TestAgent_LVM_GetCapacity_FullVG` | VG 가득 참: Available=0 | linear: avail=0 | 1) GetCapacity | Available=0 | `Agent`, `LVM`, `gRPC` |

---

#### E28.4 LVM ListVolumes (2 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 252 | `TestAgent_LVM_ListVolumes_SkipsThinPoolLV` | thin pool 인프라 LV 제외, 데이터 LV만 반환 | thin: 인프라 1 + 데이터 2 | 1) ListVolumes | 데이터 LV 2개만 | `Agent`, `LVM`, `gRPC` |
| 253 | `TestAgent_LVM_ListVolumes_Linear_AllReturned` | Linear 모드에서 모든 LV 반환 | linear: LV 3개 | 1) ListVolumes | 3개 모두 | `Agent`, `LVM`, `gRPC` |

---

#### E28.5 LVM 프로비저닝 모드 gRPC 전파 (3 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 254 | `TestAgent_LVM_CreateVolume_LinearModeParam` | ProvisionMode="linear" gRPC 전달 | mockLVMBackend(thin 기본) | 1) CreateVolume(linear) | linear 모드 생성 | `Agent`, `LVM`, `gRPC` |
| 255 | `TestAgent_LVM_CreateVolume_ThinModeParam` | ProvisionMode="thin" 전달 | mockLVMBackend(thin) | 1) CreateVolume(thin) | thin 모드 생성 | `Agent`, `LVM`, `gRPC` |
| 256 | `TestAgent_LVM_CreateVolume_EmptyMode_DefaultsToBackend` | 빈 문자열 시 backend 기본 모드 사용 | mockLVMBackend(기본 thin) | 1) CreateVolume("") | 기본 모드 적용 | `Agent`, `LVM`, `gRPC` |

---

#### E28.6 LVM 오류 처리 (5 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 257 | `TestAgent_LVM_CreateVolume_VGNotFound` | 존재하지 않는 VG로 CreateVolume 시 오류 | 미등록 poolName | 1) CreateVolume | NotFound | `Agent`, `LVM`, `gRPC` |
| 258 | `TestAgent_LVM_ExpandVolume_ShrinkRejected` | 축소 요청 거부 | 현재 2GiB, 요청 1GiB | 1) ExpandVolume | FailedPrecondition | `Agent`, `LVM`, `gRPC` |
| 259 | `TestAgent_LVM_CreateVolume_ThinWithoutPool` | thin 모드이나 thinpool 미설정 시 오류 | linear 모드, thinpool 없음 | 1) CreateVolume(thin) | InvalidArgument | `Agent`, `LVM`, `gRPC` |
| 260 | `TestAgent_LVM_CreateVolume_Idempotent` | 동일 파라미터 2회 호출 시 멱등 | 첫 호출 성공 | 1) Create; 2) 동일 재호출 | 두 번째 성공 | `Agent`, `LVM`, `gRPC` |
| 261 | `TestAgent_LVM_DeleteVolume_NonExistent_Idempotent` | 미존재 LV 삭제 성공 (멱등) | LV 미존재 | 1) DeleteVolume | 성공 | `Agent`, `LVM`, `gRPC` |

---

#### E28.7 LVM ReconcileState (1 TC)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 262 | `TestAgent_LVM_ReconcileState_RestoresExports` | LVM 볼륨의 NVMe-oF export를 configfs에 복원 | 빈 tmpdir; LVM 볼륨 2개 | 1) ReconcileState | configfs 서브시스템 존재; ACL 복원 | `Agent`, `LVM`, `NVMeF`, `gRPC` |

---

#### E28.8 LVM 이름 검증 엣지 케이스 (5 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 263a | `TestAgent_LVM_CreateVolume_ReservedPrefix_Snapshot` | "snapshot" 접두사 거부 | mockLVMBackend | 1) CreateVolume | InvalidArgument | `Agent`, `LVM`, `gRPC` |
| 263b | `TestAgent_LVM_CreateVolume_ReservedPrefix_Pvmove` | "pvmove" 접두사 거부 | 동일 | 1) CreateVolume | InvalidArgument | `Agent`, `LVM`, `gRPC` |
| 263c | `TestAgent_LVM_CreateVolume_InvalidFirstChar_Hyphen` | 첫 문자 하이픈 거부 | 동일 | 1) CreateVolume | InvalidArgument | `Agent`, `LVM`, `gRPC` |
| 263d | `TestAgent_LVM_CreateVolume_MaxLength_128` | 128자 LV 이름 허용 | 동일 | 1) CreateVolume | 성공 | `Agent`, `LVM`, `gRPC` |
| 263e | `TestAgent_LVM_CreateVolume_OverMaxLength_129` | 129자 LV 이름 거부 | 동일 | 1) CreateVolume | InvalidArgument | `Agent`, `LVM`, `gRPC` |

---

#### E28.9 LVM ExtraFlags 전달 및 VG Override 검증 (3 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 263f | `TestAgent_LVM_CreateVolume_ExtraFlags_Forwarded` | ExtraFlags가 backend까지 전달 | mockLVMBackend | 1) CreateVolume(ExtraFlags) | 전달 확인 | `Agent`, `LVM`, `gRPC` |
| 263g | `TestAgent_LVM_CreateVolume_ExtraFlags_Empty_NoEffect` | 빈 배열이면 추가 인자 없음 | 동일 | 1) CreateVolume([]) | 기본 인자만 | `Agent`, `LVM`, `gRPC` |
| 263h | `TestAgent_LVM_CreateVolume_VGOverride_Mismatch_Rejected` | VG 불일치 시 거부 | VG="data-vg"; 요청 VG="other-vg" | 1) CreateVolume | InvalidArgument | `Agent`, `LVM`, `gRPC` |

---

#### E28.10 Thin Pool 고갈 시나리오 (2 TCs)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 263i | `TestAgent_LVM_CreateVolume_ThinPool_NearFull` | data_percent=95%, 잔여 초과 요청 시 오류 | thin: pool=10GiB, 95%, 요청 1GiB | 1) CreateVolume | ResourceExhausted | `Agent`, `LVM`, `gRPC` |
| 263j | `TestAgent_LVM_CreateVolume_ThinPool_Full` | data_percent=100% 시 거부 | thin: 100% | 1) CreateVolume | 오류; LV 미생성 | `Agent`, `LVM`, `gRPC` |

---

#### E28.11 멀티 백엔드 Agent (1 TC)

| ID | 테스트 함수 | 설명 | 사전 조건 | 단계 | 기대 결과 | 커버리지 |
|----|------------|------|----------|------|----------|---------|
| 263k | `TestAgent_MultiBackend_ZFS_LVM_GetCapabilities` | ZFS + LVM 동시 등록 시 둘 다 보고 | mock ZFS + mock LVM | 1) GetCapabilities | ZFS_ZVOL과 LVM 모두 포함 | `Agent`, `ZFS`, `LVM`, `gRPC` |

---

#### E28 커버리지 요약

| 소섹션 | 검증 내용 | 테스트 수 | CI 실행 |
|--------|---------|----------|--------|
| E28.1 | LVM 역량 및 헬스체크 | 2개 | ✅ |
| E28.2 | LVM 전체 왕복 (linear + thin) | 2개 | ✅ |
| E28.3 | GetCapacity — linear VG, thin pool, over-provisioned, full VG | 4개 | ✅ |
| E28.4 | ListVolumes — thin pool LV 필터링 | 2개 | ✅ |
| E28.5 | 프로비저닝 모드 gRPC 전파 | 3개 | ✅ |
| E28.6 | 오류 처리 — VG 미존재, 축소 거부, thin pool 미설정, 멱등성 | 5개 | ✅ |
| E28.7 | ReconcileState — configfs 복원 | 1개 | ✅ |
| E28.8 | 이름 검증 — 예약 접두사, 길이 한계, 잘못된 첫 문자 | 5개 | ✅ |
| E28.9 | ExtraFlags 전달 + VG override 불일치 거부 | 3개 | ✅ |
| E28.10 | Thin pool 고갈 — near-full, full CreateVolume 거부 | 2개 | ✅ |
| E28.11 | 멀티 백엔드 agent — ZFS + LVM 동시 | 1개 | ✅ |
| **합계** | | **30개** | ✅ |

---

## 카테고리: Helm 배포 (Kind 클러스터)

> **경계:** 실제 Kind 클러스터에 Helm v3으로 pillar-csi 차트를 설치/업그레이드/제거하고,
> 배포된 Kubernetes 리소스(Deployment, DaemonSet, CRD, CSIDriver 등)의
> 존재 여부, 상태, 스펙을 검증한다. Docker + Kind 클러스터 + 컨테이너 이미지 필요.

---

### E27: Helm 차트 설치 및 릴리스 검증 (29 TCs)

**테스트 유형:** B (클러스터 레벨) ⚠️ Kind 클러스터 필요

**빌드 태그:** `//go:build e2e`

**현재 구현 상태:** 미구현(planned). 이 문서는 구현 전 설계 사양이다.

**실행 방법:**
```bash
# Kind 클러스터 준비 및 Helm v3.12+ 설치 후
go test ./test/e2e/ -tags=e2e -v -run TestHelm
```

**필수 인프라:**
- `helm` CLI v3.12 이상
- Kind 클러스터 또는 kubeconfig 설정된 K8s 클러스터
- pillar-csi 컨테이너 이미지 (Kind 로컬 로드 또는 레지스트리 접근 가능)

---

#### E27.1 Helm 차트 기본값 설치 성공 (1 TC)

| ID | 테스트 함수 | 설명 | 기대 결과 |
|----|------------|------|----------|
| 207 | `TestHelm/Helm_차트_기본값_설치_성공` | `helm install`을 기본값으로 실행하면 `deployed` 상태로 완료 | 종료 코드 0; `STATUS: deployed`; `REVISION: 1` |

---

#### E27.2 Helm 릴리스 상태 검증 (2 TCs)

| ID | 테스트 함수 | 설명 | 기대 결과 |
|----|------------|------|----------|
| 208 | `TestHelm/Helm_릴리스_상태_검증` | `helm status` 명령이 릴리스 메타데이터 반환 | `NAME: pillar-csi`; `STATUS: deployed` |
| 209 | `TestHelm/Helm_릴리스_상태_JSON_검증` | JSON 출력 파싱 가능; 필수 필드 포함 | `.info.status == "deployed"` |

---

#### E27.3 Helm 릴리스 목록 검증 (2 TCs)

| ID | 테스트 함수 | 설명 | 기대 결과 |
|----|------------|------|----------|
| 210 | `TestHelm/Helm_릴리스_목록_검증` | `helm list` 출력에 릴리스 존재 | `pillar-csi` 행; `deployed` |
| 211 | `TestHelm/Helm_릴리스_목록_JSON_검증` | JSON 배열 파싱; 릴리스 포함 | `.name == "pillar-csi"` |

---

#### E27.4 배포된 Kubernetes 리소스 정상 동작 검증 (5 TCs)

| ID | 테스트 함수 | 설명 | 기대 결과 |
|----|------------|------|----------|
| 212 | `TestHelm/컨트롤러_Deployment_Running_검증` | controller Deployment Available; 파드 Running | `availableReplicas==1`; 재시작 0 |
| 213 | `TestHelm/노드_DaemonSet_배포_검증` | node DaemonSet 파드 수 일치 | `numberReady==numberDesired` |
| 214 | `TestHelm/에이전트_DaemonSet_배포_검증` | agent DaemonSet 스토리지 레이블 노드에만 배포 | 레이블 없으면 `desiredNumberScheduled==0` |
| 215 | `TestHelm/ServiceAccount_3종_존재_검증` | controller/node/agent SA 3개 존재 | SA 3개 확인 |
| 216 | `TestHelm/CSIDriver_등록_검증` | CSIDriver `pillar-csi.bhyoo.com` 올바른 스펙 | `attachRequired==true`; `podInfoOnMount==true` |

---

#### E27.5 CRD 등록 및 가용성 검증 (E27.5.1-E27.5.7, 총 TCs)

- **E27.5.1** CRD 4종 일괄 존재 및 Established (5 TCs): 217, 217a-d
- **E27.5.2** 메타데이터 상세 검증 (4 TCs): 217e-h
- **E27.5.3** kubectl api-resources 검증 (5 TCs): 217i-m
- **E27.5.4** OpenAPI v3 스키마 존재 (4 TCs): 217n-q
- **E27.5.5** 프린터 컬럼 검증 (4 TCs): 217r-u
- **E27.5.6** resource-policy: keep 어노테이션 (1 TC): 217v
- **E27.5.7** 샘플 오브젝트 CRUD (4 TCs): 217w-z

> 상세 테이블은 원본 E2E-TESTCASES.md E27.5 참조.

---

#### E27.6-E27.12 (나머지 소섹션)

| 소섹션 | 설명 | TCs |
|--------|------|-----|
| E27.6 | 커스텀 values 오버라이드 설치 | 1 (218) |
| E27.7 | installCRDs=false 설치 모드 | 1 (219) |
| E27.8 | 중복 설치 시도 오류 | 1 (220) |
| E27.9 | Helm 업그레이드 + 히스토리 | 2 (221-222) |
| E27.10 | 설치 해제 및 CRD 보존 | 2 (223-224) |
| E27.11 | 전체 파드 Running 종합 | 7 (225-231) |
| E27.12 | CSIDriver 객체 설정 검증 | 12 (232-243) |

> 상세 테이블은 원본 E2E-TESTCASES.md E27.6-E27.12 참조.

---

## 카테고리: 실제 ZFS Backend (계획) 🔲

> **경계:** Agent ↔ 실제 ZFS zpool (loopback device).
> LVM의 E28과 동등한 수준으로, ZFS 고유 동작과 에러를 검증한다.

**현재 상태:** ZFS backend는 E2E(E35)에서만 실제 zpool을 사용. Integration 레벨에서
ZFS 고유 에러(property 충돌, quota/reservation, zvol 리사이즈 제한, dataset busy 등)를
잡는 전용 테스트가 없다.

**필요 TC (E28 패턴 적용):**

| 계획 ID | 검증 대상 | E28 대응 |
|---------|----------|---------|
| ZFS-I-1 | zvol create/delete 정상 경로 + 멱등성 | E28.2 |
| ZFS-I-2 | zvol expand (volsize 증가) | E28.2 |
| ZFS-I-3 | GetCapacity: pool available/used/total | E28.3 |
| ZFS-I-4 | ListVolumes: zvol 필터링 | E28.4 |
| ZFS-I-5 | ZFS property 전파 (compression, volblocksize) | E29 대응 |
| ZFS-I-6 | 에러: pool not found, shrink 거부, quota 초과 | E28.6 |
| ZFS-I-7 | ReconcileState configfs 복원 | E28.7 |
| ZFS-I-8 | parentDataset 경로 검증 | 신규 |
| ZFS-I-9 | snapshot create/delete (Phase 4 준비) | 신규 |

**인프라:** loopback device → zpool create → 테스트 → zpool destroy
**CI:** GHA ubuntu runner + `apt install zfsutils-linux` (또는 OpenZFS PPA)

---

## 카테고리: Protocol Target — 실제 configfs (계획) 🔲

> **경계:** Agent Protocol 플러그인 ↔ 실제 커널 configfs (/sys/kernel/config/nvmet/, /sys/kernel/config/target/iscsi/).
> mock configfs(t.TempDir())에서는 잡을 수 없는 커널 수준 에러를 검증한다.

**현재 상태:** 모든 configfs 테스트가 t.TempDir()를 configfs root로 사용.
파일 쓰기는 성공하지만 실제 커널이 거부하는 설정(잘못된 NQN 형식, 존재하지 않는
블록 디바이스 바인딩, 포트 충돌 등)을 잡을 수 없다.

**필요 TC:**

### NVMe-oF Target (nvmet configfs)

| 계획 ID | 검증 대상 |
|---------|----------|
| NVMEOF-I-1 | subsystem 생성/삭제 정상 경로 |
| NVMEOF-I-2 | namespace 활성화 (실제 블록 디바이스 바인딩) |
| NVMEOF-I-3 | port 생성 + TCP 리스너 바인딩 |
| NVMEOF-I-4 | subsystem-port 링크 (symlink) |
| NVMEOF-I-5 | ACL: allowed_hosts 추가/제거 |
| NVMEOF-I-6 | 에러: 존재하지 않는 디바이스 바인딩 → 커널 에러 |
| NVMEOF-I-7 | 에러: 포트 충돌 (이미 사용 중인 포트) |
| NVMEOF-I-8 | 에러: NQN 길이 초과 (커널 223자 제한) |
| NVMEOF-I-9 | ReconcileState: 전체 configfs 재구성 |

### iSCSI Target (LIO configfs)

| 계획 ID | 검증 대상 |
|---------|----------|
| ISCSI-I-1 | target IQN 생성/삭제 정상 경로 |
| ISCSI-I-2 | TPG + LUN 생성 (실제 블록 디바이스) |
| ISCSI-I-3 | Network Portal 바인딩 (IP:port) |
| ISCSI-I-4 | ACL: initiator IQN 추가/제거 |
| ISCSI-I-5 | 에러: 잘못된 IQN 형식 |
| ISCSI-I-6 | 에러: 포트 충돌 |

**인프라:** linux-modules-extra (nvmet, target_core_mod) + loopback block device
**CI:** GHA ubuntu runner에서 실행 가능 (커널 모듈 설치 후)

**격리 전략:**
- 테스트별 고유 NQN/IQN + 고유 포트 → 병렬 안전 (커널 su_mutex로 직렬화되나 레이스 없음)
- suite 시작 시 scavenger pass: pillar-csi 프리픽스의 stale configfs 엔트리 정리
- t.Cleanup()으로 정상 teardown, SIGKILL 대비 scavenger가 보완

---

## 전체 Integration Test 요약

| 그룹 | 테스트 수 | 빌드 태그 | CI |
|------|----------|----------|-----|
| E19: PillarTarget CRD | 19 | `integration` | ✅ |
| E20: PillarPool CRD | 20 | `integration` | ✅ |
| E23: PillarProtocol CRD | 24 | `integration` | ✅ |
| E25: PillarBinding CRD | 41 | `integration` | ✅ |
| E26: Cross-CRD | 23 | `integration` | ✅ |
| E21.2-E21.4: Webhook/Schema | 20 | `integration` | ✅ |
| E32: LVM CRD | 9 | `integration` | ✅ |
| I-NEW: PRD 갭 추가 TC | 15 | `integration` | ✅ |
| **envtest 소계** | **171** | | |
| E28: LVM Agent gRPC | 30 | (없음) | ✅ |
| **backend 소계** | **30** | | |
| E27: Helm 차트 | 29 | `e2e` | ⚠️ Kind |
| **helm 소계** | **29** | | |
| **총 합계** | **230** | | |
