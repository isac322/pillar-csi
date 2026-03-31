# pillar-csi Development Rules

## kubebuilder CLI vs 수동 코드: 역할 분담

### 반드시 kubebuilder CLI를 사용할 것 (직접 코드 수정 금지)

| 작업 | 명령어 |
|------|--------|
| 새 CRD + Controller 추가 | `kubebuilder create api --group pillar-csi.bhyoo.com --version v1alpha1 --kind <Kind> --namespaced=false` |
| Validation webhook 추가 | `kubebuilder create webhook --group pillar-csi.bhyoo.com --version v1alpha1 --kind <Kind> --programmatic-validation` |
| Defaulting webhook 추가 | `kubebuilder create webhook --group pillar-csi.bhyoo.com --version v1alpha1 --kind <Kind> --defaulting` |
| CRD YAML / RBAC 재생성 | `make manifests` (`_types.go` 마커 변경 후 반드시 실행) |
| DeepCopy 메서드 재생성 | `make generate` (`_types.go` 필드 변경 후 반드시 실행) |
| CRD 클러스터 설치 | `make install` |
| 멀티그룹 전환 | `kubebuilder edit --multigroup` |

**절대 하지 말 것:**
- `config/crd/` 아래 YAML 직접 수정 → `make manifests`로 재생성
- `zz_generated.deepcopy.go` 직접 수정 → `make generate`로 재생성
- `config/rbac/role.yaml` 직접 수정 → controller 코드의 `//+kubebuilder:rbac` 마커 수정 후 `make manifests`

### 수동으로 작성하는 코드

| 파일 | 내용 |
|------|------|
| `api/v1alpha1/*_types.go` | CRD Spec/Status 필드 정의 (kubebuilder 마커 포함) |
| `internal/controller/*_controller.go` | Reconcile 비즈니스 로직 |
| `api/v1alpha1/*_webhook.go` | Validation/Defaulting 로직 (kubebuilder가 스캐폴딩 후 로직 작성) |
| `proto/*.proto` | gRPC Agent API 정의 |
| `internal/agent/` | Agent gRPC 서버, Backend/Protocol 플러그인 |
| `internal/csi/` | CSI Controller/Node 서비스 구현 |

## No Silent Failures 에러 보고 원칙

- `configfs`, `sysfs` 등 커널 인터페이스에 대한 write 실패를 `continue`, 무시, debug-only 로그로 삼키지 말 것
- write 후 실제 상태가 중요하면 즉시 read-back 검증을 수행하고, 기대값과 다르면 명시적 에러로 반환할 것
- cleanup/rollback 경로라도 스토리지 상태나 연결 상태에 영향을 주는 실패는 호출자에게 반환하거나 최소한 `log.Error`로 남길 것
- 에러 메시지에는 작업 종류(`write`, `disconnect`, `expand`), 대상(`path`, `NQN`, `device`, `volumeID`)과 원인(`%w`)을 포함할 것

## Git 커밋 규칙

- **커밋 전 반드시 `make lint` 실행하여 0 issues 확인** — lint 에러가 있으면 커밋하지 말 것
- 매 작업 단계마다 커밋
- 커밋 메시지는 "why not what" — 왜 그런 변경인지 (코드에 없는 정보)
- `Co-Authored-By: Claude Opus 4.6 (1M context) <noreply@anthropic.com>` 포함

## 프로젝트 정보

- API group: `pillar-csi.bhyoo.com`
- CSI provisioner name: `pillar-csi.bhyoo.com`
- CRDs (all cluster-scoped): PillarTarget, PillarPool, PillarProtocol, PillarBinding
- PRD: `docs/PRD.md`

<!-- ooo:START -->
<!-- ooo:VERSION:0.26.6 -->
# Ouroboros — Specification-First AI Development

> Before telling AI what to build, define what should be built.
> As Socrates asked 2,500 years ago — "What do you truly know?"
> Ouroboros turns that question into an evolutionary AI workflow engine.

Most AI coding fails at the input, not the output. Ouroboros fixes this by
**exposing hidden assumptions before any code is written**.

1. **Socratic Clarity** — Question until ambiguity ≤ 0.2
2. **Ontological Precision** — Solve the root problem, not symptoms
3. **Evolutionary Loops** — Each evaluation cycle feeds back into better specs

```
Interview → Seed → Execute → Evaluate
    ↑                           ↓
    └─── Evolutionary Loop ─────┘
```

## ooo Commands

Each command loads its agent/MCP on-demand. Details in each skill file.

| Command | Loads |
|---------|-------|
| `ooo` | — |
| `ooo interview` | `ouroboros:socratic-interviewer` |
| `ooo seed` | `ouroboros:seed-architect` |
| `ooo run` | MCP required |
| `ooo evolve` | MCP: `evolve_step` |
| `ooo evaluate` | `ouroboros:evaluator` |
| `ooo unstuck` | `ouroboros:{persona}` |
| `ooo status` | MCP: `session_status` |
| `ooo setup` | — |
| `ooo help` | — |

## Agents

Loaded on-demand — not preloaded.

**Core**: socratic-interviewer, ontologist, seed-architect, evaluator,
wonder, reflect, advocate, contrarian, judge
**Support**: hacker, simplifier, researcher, architect
<!-- ooo:END -->
